package expenses

// Synergy accounting: how Synergy Wholesale costs reach the P&L.
//
// Synergy is prepaid: you top up a balance from your bank (statement
// "Refill" lines) and Synergy takes monthly hosting, renewals and support
// fees out of it. Either can be "the expense" — never both:
//
//   - "charges" (default, accrual-friendly): the service charges minus credits,
//     one expense per month × category (see synergy_statement.go), dated by
//     charge date or by service month (basis).
//   - "topups" (cash): each top-up from your bank is one expense on the top-up
//     date, category "Synergy prepaid hosting"; charges then only drive the
//     analytics.
//
// ApplySynergyAccounting makes the expenses match the chosen mode. It runs
// automatically after every statement import (upload or Gmail) and once on the
// first load after the upgrade (backfill); with "Auto-approve Synergy
// expenses" on (default) they are approved straight away, so the P&L shows
// them immediately. Everything is keyed, so re-running updates and never
// duplicates.

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Synergy accounting modes.
const (
	ModeCharges = "charges"
	ModeTopups  = "topups"
)

// synTopupSource: an expense for one Synergy top-up (topups mode).
const synTopupSource = "synergy_topup"

// AccountingPrefs are the organisation's expense accounting settings.
type AccountingPrefs struct {
	OrganizationID  string     `json:"-" gorm:"type:varchar(255);primaryKey"`
	SynergyMode     string     `json:"synergy_mode" gorm:"type:varchar(20)"`  // charges | topups
	SynergyBasis    string     `json:"synergy_basis" gorm:"type:varchar(10)"` // charge | service (charges mode)
	AutoApprove     *bool      `json:"-"`                                     // nil: on
	AUGST           *bool      `json:"-" gorm:"column:au_gst"`                // nil: on only when the org is in Australia
	BackfilledAt    *time.Time `json:"backfilled_at"`
	Notice          string     `json:"notice" gorm:"type:text"`
	NoticeDismissed bool       `json:"notice_dismissed"`
	LastAppliedAt   *time.Time `json:"last_applied_at"`
	LastApply       string     `json:"-" gorm:"type:text"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (AccountingPrefs) TableName() string { return "expense_prefs" }

func (s *Service) prefs(org string) AccountingPrefs {
	var p AccountingPrefs
	if s.DB.Where("organization_id = ?", org).First(&p).Error != nil {
		p = AccountingPrefs{OrganizationID: org}
	}
	if p.SynergyMode != ModeTopups {
		p.SynergyMode = ModeCharges
	}
	if p.SynergyBasis != "service" {
		p.SynergyBasis = "charge"
	}
	return p
}

func (s *Service) savePrefs(p AccountingPrefs) {
	p.UpdatedAt = s.now()
	s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&p)
}

func (p AccountingPrefs) autoApprove() bool { return p.AutoApprove == nil || *p.AutoApprove }

// auGST: is the organisation registered for Australian GST (can it claim the
// 1/11 GST in Synergy's AUD prices)? Default: only when it is in Australia.
func (s *Service) auGST(org string, p AccountingPrefs) bool {
	if p.AUGST != nil {
		return *p.AUGST
	}
	return orgprefs.Country(s.DB, org) == "AU"
}

// synergyTax is the claimable tax of a Synergy amount (AUD incl. GST).
func (s *Service) synergyTax(org string, total float64) (float64, string) {
	if s.auGST(org, s.prefs(org)) {
		return r2(total / 11), "Australian GST 1/11 of the total (claimable: registered for Australian GST)"
	}
	return 0, fmt.Sprintf("Incl. Australian GST of %s — not claimable (not registered for Australian GST), so the full amount is the cost", audFmt(r2(total/11)))
}

// AccountingView is GET /expenses/synergy/accounting.
type AccountingView struct {
	Mode            string              `json:"mode"`
	Basis           string              `json:"basis"`
	AutoApprove     bool                `json:"auto_approve"`
	AUGSTRegistered bool                `json:"au_gst_registered"`
	AUGSTDefault    bool                `json:"au_gst_default"`
	Country         string              `json:"country"`
	Home            string              `json:"home"`
	Notice          string              `json:"notice,omitempty"`
	LastAppliedAt   *time.Time          `json:"last_applied_at"`
	LastApply       *SynergyApplyResult `json:"last_apply,omitempty"`
	Counted         []SynCountedMonth   `json:"counted"`
	CountedAUD      float64             `json:"counted_aud"`
	CountedHome     float64             `json:"counted_home"`
	Pending         int                 `json:"pending"`
	Reconciliation  []SynRecon          `json:"reconciliation"`
	ReconTotal      *SynRecon           `json:"reconciliation_total"`
	FX              RateInfo            `json:"fx"`
	MonthRates      []RateInfo          `json:"month_rates"`
}

// SynCountedMonth is what the P&L counts for Synergy in one month.
type SynCountedMonth struct {
	Month     string   `json:"month"`
	AUD       float64  `json:"aud"`
	Home      float64  `json:"home"`
	Tax       float64  `json:"tax"` // claimable GST (AUD)
	Rate      float64  `json:"rate"`
	RateLabel string   `json:"rate_label"`
	Items     int      `json:"items"`
	Approved  int      `json:"approved"`
	ItemIDs   []string `json:"item_ids"`
}

// SynRecon reconciles a statement month: top-ups − charges = balance change.
type SynRecon struct {
	Month      string  `json:"month"`
	Opening    float64 `json:"opening"`
	Topups     float64 `json:"topups"`
	Charges    float64 `json:"charges"` // debits − credits, top-ups excluded
	Closing    float64 `json:"closing"`
	Change     float64 `json:"change"`
	Difference float64 `json:"difference"` // change − (top-ups − charges); 0 when it reconciles
	OK         bool    `json:"ok"`
}

// SynergyApplyResult reports what ApplySynergyAccounting changed.
type SynergyApplyResult struct {
	Mode        string   `json:"mode"`
	Basis       string   `json:"basis"`
	AutoApprove bool     `json:"auto_approve"`
	Created     int      `json:"created"`
	Updated     int      `json:"updated"`
	Unchanged   int      `json:"unchanged"`
	Approved    int      `json:"approved"`
	Removed     int      `json:"removed"`
	Ignored     int      `json:"ignored"`
	Errors      []string `json:"errors"`
	Months      []string `json:"months"`
	TotalAUD    float64  `json:"total_aud"`
	TotalHome   float64  `json:"total_home"`
	Home        string   `json:"home"`
	Message     string   `json:"message"`
}

// AccountingInput changes the settings (nil: keep).
type AccountingInput struct {
	Mode            *string `json:"mode"`
	Basis           *string `json:"basis"`
	AutoApprove     *bool   `json:"auto_approve"`
	AUGSTRegistered *bool   `json:"au_gst_registered"`
}

// SaveAccounting stores the settings and re-applies them.
func (s *Service) SaveAccounting(org, user string, in AccountingInput) (*SynergyApplyResult, error) {
	p := s.prefs(org)
	if in.Mode != nil {
		if *in.Mode != ModeCharges && *in.Mode != ModeTopups {
			return nil, invalid("choose how Synergy costs count: service charges or top-ups")
		}
		p.SynergyMode = *in.Mode
	}
	if in.Basis != nil {
		if *in.Basis != "charge" && *in.Basis != "service" {
			return nil, invalid("basis must be charge or service")
		}
		p.SynergyBasis = *in.Basis
	}
	if in.AutoApprove != nil {
		v := *in.AutoApprove
		p.AutoApprove = &v
	}
	if in.AUGSTRegistered != nil {
		v := *in.AUGSTRegistered
		p.AUGST = &v
	}
	if p.BackfilledAt == nil {
		t := s.now()
		p.BackfilledAt = &t
	}
	s.savePrefs(p)
	return s.ApplySynergyAccounting(org, user)
}

// DismissAccountingNotice hides the one-time backfill notice.
func (s *Service) DismissAccountingNotice(org string) {
	p := s.prefs(org)
	p.NoticeDismissed = true
	s.savePrefs(p)
}

// EnsureSynergyBackfill applies the accounting once for statements uploaded
// before automatic Synergy expenses existed, and leaves a one-time notice.
func (s *Service) EnsureSynergyBackfill(org string) {
	p := s.prefs(org)
	if p.BackfilledAt != nil {
		return
	}
	var n int64
	s.DB.Model(&SynergyTxn{}).Where("organization_id = ?", org).Count(&n)
	t := s.now()
	p.BackfilledAt = &t
	// Keep the basis existing statement expenses were made with.
	var cur Item
	if s.DB.Where("organization_id = ? AND source = ?", org, synStatementSource).First(&cur).Error == nil && cur.Basis == "service" {
		p.SynergyBasis = "service"
	}
	s.savePrefs(p)
	if n == 0 {
		return
	}
	res, err := s.ApplySynergyAccounting(org, "")
	if err != nil || res == nil || len(res.Months) == 0 {
		return
	}
	p = s.prefs(org)
	p.Notice = fmt.Sprintf("Synergy expenses for %s added: %s (%s)%s.", monthSpan(res.Months), "A"+audFmt(res.TotalAUD), fmtMoney(res.TotalHome, res.Home),
		map[bool]string{true: " and approved — they are in your profit & loss now", false: " — waiting in the inbox for approval"}[res.AutoApprove])
	p.NoticeDismissed = false
	s.savePrefs(p)
}

// monthSpan: ["2026-08","2026-09"] → "Aug–Sep 2026".
func monthSpan(ms []string) string {
	if len(ms) == 0 {
		return ""
	}
	sort.Strings(ms)
	a, _ := time.Parse("2006-01", ms[0])
	b, _ := time.Parse("2006-01", ms[len(ms)-1])
	switch {
	case ms[0] == ms[len(ms)-1]:
		return a.Format("Jan 2006")
	case a.Year() == b.Year():
		return a.Format("Jan") + "–" + b.Format("Jan 2006")
	}
	return a.Format("Jan 2006") + "–" + b.Format("Jan 2006")
}

// removeItems un-approves (removing the bills) and deletes items.
func (s *Service) removeItems(org, user string, items []Item, res *SynergyApplyResult) {
	for _, it := range items {
		if it.Status == StatusApproved {
			if _, err := s.Unapprove(org, user, it.ID); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", nonEmpty(it.InvoiceNumber, it.ID), err))
				continue
			}
		}
		s.DB.Where("item_id = ?", it.ID).Delete(&Allocation{})
		s.DB.Delete(&Item{}, "id = ?", it.ID)
		res.Removed++
	}
}

// ApplySynergyAccounting makes the Synergy expenses match the settings.
func (s *Service) ApplySynergyAccounting(org, user string) (*SynergyApplyResult, error) {
	p := s.prefs(org)
	res := &SynergyApplyResult{Mode: p.SynergyMode, Basis: p.SynergyBasis, AutoApprove: p.autoApprove(), Errors: []string{}, Months: []string{}, Home: s.Home(org)}
	v := s.synergyVendor(org)
	if v == nil {
		return nil, invalid("the Synergy Wholesale vendor is missing")
	}
	// Statements are the source of truth in both modes: Synergy receipts /
	// invoices from Gmail are top-ups of the prepaid balance.
	if v.CountFrom != "statement" {
		s.DB.Model(&Vendor{}).Where("id = ?", v.ID).Updates(map[string]any{"count_from": "statement", "updated_at": s.now()})
		v.CountFrom = "statement"
	}
	var txns []SynergyTxn
	s.DB.Where("organization_id = ?", org).Order("date, seq").Find(&txns)
	if p.SynergyMode == ModeCharges {
		var tops []Item
		s.DB.Where("organization_id = ? AND source = ?", org, synTopupSource).Find(&tops)
		s.removeItems(org, user, tops, res)
		if len(txns) > 0 {
			pv, err := s.CreateStatementExpenses(org, user, StatementExpenseInput{Basis: p.SynergyBasis, UseStatement: true, Approve: p.autoApprove()})
			if err != nil {
				return nil, err
			}
			res.Created, res.Updated, res.Unchanged, res.Approved, res.Ignored = pv.Created, pv.Updated, pv.Unchanged, pv.Approved, pv.Ignored
			res.Removed += pv.Removed
			res.Errors = append(res.Errors, pv.Errors...)
			keep := map[string]bool{}
			for _, g := range pv.Groups {
				keep[statementExtID(g)] = true
			}
			// Groups of another basis / months that no longer have charges.
			var cur []Item
			s.DB.Where("organization_id = ? AND source = ?", org, synStatementSource).Find(&cur)
			stale := []Item{}
			for _, it := range cur {
				if !keep[it.ExternalID] {
					stale = append(stale, it)
				}
			}
			s.removeItems(org, user, stale, res)
		}
	} else {
		var stmts []Item
		s.DB.Where("organization_id = ? AND source = ?", org, synStatementSource).Find(&stmts)
		s.removeItems(org, user, stmts, res)
		s.applyTopups(org, user, v, txns, p, res)
	}
	counted, _, _ := s.synergyCounted(org, p.SynergyMode)
	for _, c := range counted {
		res.Months = append(res.Months, c.Month)
		res.TotalAUD = r2(res.TotalAUD + c.AUD)
		res.TotalHome = fxrates.Round(res.TotalHome+c.Home, res.Home)
	}
	res.Message = fmt.Sprintf("%d created, %d updated, %d approved, %d removed", res.Created, res.Updated, res.Approved, res.Removed)
	now := s.now()
	p = s.prefs(org)
	p.LastAppliedAt = &now
	if b, err := json.Marshal(res); err == nil {
		p.LastApply = string(b)
	}
	if p.BackfilledAt == nil {
		p.BackfilledAt = &now
	}
	s.savePrefs(p)
	return res, nil
}

// applyTopups: one expense per statement top-up (cash basis).
func (s *Service) applyTopups(org, user string, v *Vendor, txns []SynergyTxn, p AccountingPrefs, res *SynergyApplyResult) {
	covered := map[string]bool{}
	want := map[string]SynergyTxn{}
	for _, t := range txns {
		covered[t.Month] = true
		if t.Kind == SynTopup && t.Credit-t.Debit > 0.004 {
			want["topup|"+t.DedupeKey] = t
		}
	}
	// Synergy receipts / invoices in statement months are the same top-ups:
	// not counted again.
	var others []Item
	s.DB.Where("organization_id = ? AND vendor_id = ? AND source NOT IN ? AND status IN ? AND kind <> ?", org, v.ID, []string{synStatementSource, synTopupSource},
		[]string{StatusPending, StatusApproved}, KindDeclined).Find(&others)
	for _, o := range others {
		if o.InvoiceDate == nil || !covered[monthOf(*o.InvoiceDate)] {
			continue
		}
		if o.Status == StatusApproved {
			if _, err := s.Unapprove(org, user, o.ID); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", nonEmpty(o.InvoiceNumber, o.ID), err))
				continue
			}
		}
		if _, err := s.SetStatus(org, o.ID, StatusIgnored, "Synergy is counted from the statement top-ups — this receipt is the same money, so it is not counted again"); err == nil {
			res.Ignored++
		}
	}
	var cur []Item
	s.DB.Where("organization_id = ? AND source = ?", org, synTopupSource).Find(&cur)
	have := map[string]Item{}
	stale := []Item{}
	for _, it := range cur {
		if _, ok := want[it.ExternalID]; ok {
			have[it.ExternalID] = it
		} else {
			stale = append(stale, it)
		}
	}
	s.removeItems(org, user, stale, res)
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return want[keys[i]].Date.Before(want[keys[j]].Date) })
	for _, k := range keys {
		t := want[k]
		total := r2(t.Credit - t.Debit)
		tax, note := s.synergyTax(org, total)
		d := Day(t.Date)
		ref := "SYN-TOPUP-" + t.Date.Format("20060102-1504")
		service := "Synergy prepaid hosting — top-up (" + nonEmpty(strings.TrimSpace(t.Description), "Refill") + ")"
		if ex, ok := have[k]; ok {
			rate := s.RateFor(org, "AUD", s.Home(org), d)
			if math.Abs(ex.Total-total) < 0.005 && math.Abs(ex.Tax-tax) < 0.005 && ex.HomeCurrency == s.Home(org) && sameRate(ex.FxRate, rate.Rate) && ex.Category == CatPrepaid {
				res.Unchanged++
				if p.autoApprove() && ex.Status == StatusPending {
					if _, err := s.Approve(org, user, ex.ID, ApproveInput{}); err == nil {
						res.Approved++
					} else {
						res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", ref, err))
					}
				}
				continue
			}
			wasApproved := ex.Status == StatusApproved
			if wasApproved {
				if _, err := s.Unapprove(org, user, ex.ID); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", ref, err))
					continue
				}
			}
			ex.Total, ex.Tax, ex.TaxNote, ex.Currency, ex.Category, ex.FxRate = total, tax, note, "AUD", CatPrepaid, 0
			s.convert(org, &ex, true)
			s.DB.Model(&Item{}).Where("id = ?", ex.ID).Updates(map[string]any{"total": ex.Total, "tax": ex.Tax, "net": ex.Net, "tax_note": ex.TaxNote, "category": ex.Category,
				"home_currency": ex.HomeCurrency, "fx_rate": ex.FxRate, "fx_source": ex.FxSource, "fx_date": ex.FxDate, "fx_basis": ex.FxBasis, "fx_note": ex.FxNote,
				"home_total": ex.HomeTotal, "home_tax": ex.HomeTax, "home_net": ex.HomeNet, "status": StatusPending, "updated_at": s.now()})
			res.Updated++
			if wasApproved || p.autoApprove() {
				if _, err := s.Approve(org, user, ex.ID, ApproveInput{}); err == nil {
					res.Approved++
				} else {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", ref, err))
				}
			}
			continue
		}
		it := &Item{ID: uuid.NewString(), OrganizationID: org, Source: synTopupSource, ExternalID: k, Status: StatusPending, Kind: KindReceipt, VendorID: v.ID, VendorName: v.Name,
			InvoiceNumber: ref, InvoiceDate: &d, Service: service, Description: fmt.Sprintf("Top-up of your Synergy Wholesale prepaid balance on %s (statement line: %s)", t.Date.Format("2 Jan 2006 15:04"), t.Description),
			Category: CatPrepaid, Paid: true, PaidDate: &d, Currency: "AUD", Total: total, Tax: tax, TaxNote: note, Confidence: 1, Method: "statement",
			Warnings: []string{}, Subject: "Synergy Wholesale statement", CreatedAt: s.now(), UpdatedAt: s.now()}
		s.convert(org, it, true)
		it.DedupeKey = dedupeKey(v.ID+v.Name, it)
		if err := s.DB.Create(it).Error; err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", ref, err))
			continue
		}
		res.Created++
		if p.autoApprove() {
			if _, err := s.Approve(org, user, it.ID, ApproveInput{}); err == nil {
				res.Approved++
			} else {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", ref, err))
			}
		}
	}
}

func sameRate(a, b float64) bool {
	if a <= 0 || b <= 0 {
		return a == b
	}
	return math.Abs(a-b)/b < 0.0005
}

// synergyCounted groups the mode's Synergy expenses by month.
func (s *Service) synergyCounted(org, mode string) ([]SynCountedMonth, float64, float64) {
	src := synStatementSource
	if mode == ModeTopups {
		src = synTopupSource
	}
	var items []Item
	s.DB.Where("organization_id = ? AND source = ? AND status IN ?", org, src, []string{StatusPending, StatusApproved}).Order("invoice_date").Find(&items)
	by := map[string]*SynCountedMonth{}
	order := []string{}
	home := s.Home(org)
	var ta, th float64
	for _, it := range items {
		if it.InvoiceDate == nil {
			continue
		}
		m := monthOf(*it.InvoiceDate)
		c := by[m]
		if c == nil {
			c = &SynCountedMonth{Month: m, ItemIDs: []string{}}
			by[m] = c
			order = append(order, m)
		}
		c.AUD = r2(c.AUD + it.Total)
		c.Tax = r2(c.Tax + it.Tax)
		c.Home = fxrates.Round(c.Home+it.HomeTotal, home)
		c.Rate = it.FxRate
		c.RateLabel = rateLabel(RateInfo{From: it.Currency, To: it.HomeCurrency, Rate: it.FxRate, Source: it.FxSource, AsOf: it.FxDate, Basis: it.FxBasis, Month: m})
		c.Items++
		if it.Status == StatusApproved {
			c.Approved++
		}
		c.ItemIDs = append(c.ItemIDs, it.ID)
		ta = r2(ta + it.Total)
		th = fxrates.Round(th+it.HomeTotal, home)
	}
	sort.Strings(order)
	out := make([]SynCountedMonth, 0, len(order))
	for _, m := range order {
		out = append(out, *by[m])
	}
	return out, ta, th
}

// SynergyReconciliation: per statement month, top-ups vs charges vs the
// balance change (opening → closing).
func SynergyReconciliation(txns []SynergyTxn) ([]SynRecon, *SynRecon) {
	out := []SynRecon{}
	if len(txns) == 0 {
		return out, nil
	}
	rows := append([]SynergyTxn(nil), txns...)
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Date.Equal(rows[j].Date) {
			return rows[i].Date.Before(rows[j].Date)
		}
		return rows[i].Seq < rows[j].Seq // oldest first within the same second
	})
	by := map[string]*SynRecon{}
	order := []string{}
	for _, t := range rows {
		r := by[t.Month]
		if r == nil {
			r = &SynRecon{Month: t.Month}
			if t.HasBalance {
				r.Opening = r2(t.Balance - t.Credit + t.Debit)
			}
			by[t.Month] = r
			order = append(order, t.Month)
		}
		if t.Kind == SynTopup {
			r.Topups = r2(r.Topups + t.Credit - t.Debit)
		} else {
			r.Charges = r2(r.Charges + t.Amount)
		}
		if t.HasBalance {
			r.Closing = t.Balance
		}
	}
	tot := &SynRecon{Month: "total"}
	for i, m := range order {
		r := by[m]
		r.Change = r2(r.Closing - r.Opening)
		r.Difference = r2(r.Change - (r.Topups - r.Charges))
		r.OK = math.Abs(r.Difference) < 0.01
		if i == 0 {
			tot.Opening = r.Opening
		}
		tot.Closing = r.Closing
		tot.Topups = r2(tot.Topups + r.Topups)
		tot.Charges = r2(tot.Charges + r.Charges)
		out = append(out, *r)
	}
	tot.Change = r2(tot.Closing - tot.Opening)
	tot.Difference = r2(tot.Change - (tot.Topups - tot.Charges))
	tot.OK = math.Abs(tot.Difference) < 0.01
	return out, tot
}

// Accounting returns the settings with what is counted and the reconciliation.
func (s *Service) Accounting(org string) *AccountingView {
	p := s.prefs(org)
	home := s.Home(org)
	v := &AccountingView{Mode: p.SynergyMode, Basis: p.SynergyBasis, AutoApprove: p.autoApprove(), AUGSTRegistered: s.auGST(org, p),
		AUGSTDefault: orgprefs.Country(s.DB, org) == "AU", Country: orgprefs.Country(s.DB, org), Home: home, LastAppliedAt: p.LastAppliedAt,
		Counted: []SynCountedMonth{}, Reconciliation: []SynRecon{}, MonthRates: []RateInfo{}}
	if !p.NoticeDismissed {
		v.Notice = p.Notice
	}
	if p.LastApply != "" {
		var r SynergyApplyResult
		if json.Unmarshal([]byte(p.LastApply), &r) == nil {
			v.LastApply = &r
		}
	}
	v.Counted, v.CountedAUD, v.CountedHome = s.synergyCounted(org, p.SynergyMode)
	for _, c := range v.Counted {
		if c.Approved < c.Items {
			v.Pending += c.Items - c.Approved
		}
	}
	var txns []SynergyTxn
	s.DB.Where("organization_id = ?", org).Order("date, seq").Find(&txns)
	v.Reconciliation, v.ReconTotal = SynergyReconciliation(txns)
	v.FX = s.RateFor(org, "AUD", home, s.now())
	v.MonthRates = s.MonthRates(org, "AUD", home)
	return v
}
