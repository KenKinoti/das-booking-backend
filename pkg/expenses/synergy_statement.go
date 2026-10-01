package expenses

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
)

// Expenses from Synergy statements.
//
// One expense (→ one supplier bill) per month per category — Web Hosting,
// Domain Names, Support Fees — with a line per domain (credits for that
// domain netted in). Why not one expense per statement line: a month has
// ~30 lines incl. credits that bills cannot hold (no negative lines), and
// three bills a month keep Bills and the P&L readable while the per-domain
// detail stays on the bill lines and in Expenses → Synergy.
//
// Count once: the expense is keyed (source "synergy_statement", "YYYY-MM|category"),
// so re-uploading a statement updates it (an approved one is re-approved with
// the new amounts) instead of adding another. Synergy invoices / receipts
// from Gmail or uploads are top-ups of the prepaid balance: when the user
// picks the statement as the source of truth they are ignored, and approving
// one for a month the statement already counts (or the other way round) is
// refused.
//
// Basis: "charge" (default) dates each expense by its charge date (cash);
// "service" puts monthly hosting in the month it pays for (charged 25 Aug
// for 1 Sep – 1 Oct → September bill, paid 25 Aug), so an accrual P&L
// allocates it to the service month while a cash P&L still follows the payment.

// KindStatement is an expense built from Synergy statement lines.
const KindStatement = "statement"

// StatementExpenseInput chooses what to create.
type StatementExpenseInput struct {
	Months       []string `json:"months"`        // empty: every month with statement lines
	Basis        string   `json:"basis"`         // charge (default) | service
	UseStatement bool     `json:"use_statement"` // count Synergy from statements: ignore overlapping invoices / receipts
	Approve      bool     `json:"approve"`       // approve the expenses straight away
	DryRun       bool     `json:"-"`
	refresh      bool     // re-upload: update existing statement expenses only
}

// StatementGroup is one month × category expense.
type StatementGroup struct {
	Key        string          `json:"key"` // YYYY-MM|category
	Month      string          `json:"month"`
	Category   string          `json:"category"`
	Label      string          `json:"label"`
	Total      float64         `json:"total"` // AUD incl. GST
	Credits    float64         `json:"credits"`
	Lines      []StatementLine `json:"lines"`
	BillDate   string          `json:"bill_date"`
	PaidDate   string          `json:"paid_date"`
	ChargeFrom string          `json:"charge_from"`
	ChargeTo   string          `json:"charge_to"`
	Warnings   []string        `json:"warnings"`
	ItemID     string          `json:"item_id,omitempty"`
	ItemStatus string          `json:"item_status,omitempty"`
	Change     string          `json:"change"` // new | update | unchanged
}

// StatementOverlap is a Synergy invoice / receipt in the same months.
type StatementOverlap struct {
	ItemID        string  `json:"item_id"`
	Source        string  `json:"source"`
	Status        string  `json:"status"`
	InvoiceNumber string  `json:"invoice_number"`
	Date          string  `json:"date"`
	Total         float64 `json:"total"`
	Currency      string  `json:"currency"`
	Match         string  `json:"match"` // what it matches in the statement (usually a top-up)
}

// StatementPreview is the plan (or the result) of creating statement expenses.
type StatementPreview struct {
	Basis           string             `json:"basis"`
	Months          []string           `json:"months"`
	AvailableMonths []string           `json:"available_months"`
	Groups          []StatementGroup   `json:"groups"`
	Total           float64            `json:"total"`
	Overlaps        []StatementOverlap `json:"overlaps"`
	CountFrom       string             `json:"count_from"`
	Created         int                `json:"created"`
	Updated         int                `json:"updated"`
	Unchanged       int                `json:"unchanged"`
	Removed         int                `json:"removed"`
	Approved        int                `json:"approved"`
	Ignored         int                `json:"ignored"`
	Errors          []string           `json:"errors"`
	Note            string             `json:"note"`
}

func monthLabelOf(m string) string {
	t, err := time.Parse("2006-01", m)
	if err != nil {
		return m
	}
	return t.Format("Jan 2006")
}

// groupMonth is the month a line is counted in for the basis.
func groupMonth(t SynergyTxn, basis string) string {
	if basis == "service" && t.ServiceMonth != "" {
		return t.ServiceMonth
	}
	return t.Month
}

// BuildStatementGroups turns statement lines into month × category expenses.
// Top-ups and zero lines are left out; months filters by group month.
func BuildStatementGroups(txns []SynergyTxn, basis string, months map[string]bool) []StatementGroup {
	type lineAcc struct {
		StatementLine
		kinds []string
		plans []string
		last  time.Time
	}
	type acc struct {
		g     *StatementGroup
		lines map[string]*lineAcc
		order []string
		first time.Time
		last  time.Time
	}
	groups := map[string]*acc{}
	keys := []string{}
	for _, t := range txns {
		if t.Kind == SynTopup || math.Abs(t.Amount) < 0.005 {
			continue
		}
		m := groupMonth(t, basis)
		if len(months) > 0 && !months[m] {
			continue
		}
		cat := nonEmpty(t.Category, CatOther)
		k := m + "|" + cat
		a := groups[k]
		if a == nil {
			a = &acc{g: &StatementGroup{Key: k, Month: m, Category: cat, Label: fmt.Sprintf("Synergy Wholesale — %s, %s", CategoryLabels[cat], monthLabelOf(m)), Warnings: []string{}},
				lines: map[string]*lineAcc{}}
			groups[k] = a
			keys = append(keys, k)
		}
		if a.first.IsZero() || t.Date.Before(a.first) {
			a.first = t.Date
		}
		if t.Date.After(a.last) {
			a.last = t.Date
		}
		name := t.Domain
		if name == "" {
			name = t.Description
			if t.Kind == SynSupportFee {
				name = "Priority ticket — " + t.Description
			}
		}
		lk := strings.ToLower(name)
		l := a.lines[lk]
		if l == nil {
			l = &lineAcc{StatementLine: StatementLine{Domain: t.Domain, ByMonth: map[string]float64{}, LastByMon: map[string]string{}}}
			l.Label = name
			a.lines[lk] = l
			a.order = append(a.order, lk)
		}
		l.ByMonth[t.Month] = r2(l.ByMonth[t.Month] + t.Amount)
		if d := t.Date.Format(dayFmt); d > l.LastByMon[t.Month] {
			l.LastByMon[t.Month] = d
		}
		l.Amount = r2(l.Amount + t.Amount)
		l.Count++
		if t.Date.After(l.last) {
			l.last = t.Date
		}
		if !containsStr(l.kinds, t.Kind) {
			l.kinds = append(l.kinds, t.Kind)
		}
		switch t.Kind {
		case SynPlanUpgrade, SynPlanDowngrade:
			l.plans = append(l.plans, t.PlanFrom+" → "+t.PlanTo)
		case SynHostingMonthly:
			if t.PeriodFrom != nil {
				l.plans = append(l.plans, "hosting "+t.PeriodFrom.Format("Jan 2006"))
			}
		}
	}
	out := []StatementGroup{}
	sort.Strings(keys)
	for _, k := range keys {
		a := groups[k]
		g := a.g
		lines := []*lineAcc{}
		for _, lk := range a.order {
			l := a.lines[lk]
			l.ChargeDate, l.ChargeMon = l.last.Format(dayFmt), monthOf(l.last)
			parts := []string{}
			for _, p := range l.plans {
				if !containsStr(parts, p) {
					parts = append(parts, p)
				}
			}
			for _, kd := range l.kinds {
				switch kd {
				case SynHostingProrata:
					parts = append(parts, "pro-rata (new service)")
				case SynCancelCredit:
					parts = append(parts, "cancellation credit")
				case SynDomainRenewal:
					parts = append(parts, "domain renewal")
				}
			}
			if l.Domain != "" && len(parts) > 0 {
				l.Label = l.Domain + " — " + strings.Join(parts, ", ")
			}
			lines = append(lines, l)
		}
		// Net negative lines (credits without a charge for that domain in the
		// group) into the largest other lines; they stay listed at 0.
		for _, l := range lines {
			if l.Amount >= 0 {
				continue
			}
			credit := -l.Amount
			g.Credits = r2(g.Credits + credit)
			into := []string{}
			for credit > 0.004 {
				var best *lineAcc
				for _, c := range lines {
					if c.Amount > 0.004 && (best == nil || c.Amount > best.Amount) {
						best = c
					}
				}
				if best == nil {
					break
				}
				take := math.Min(credit, best.Amount)
				best.Amount, best.Credits, credit = r2(best.Amount-take), r2(best.Credits+take), r2(credit-take)
				best.Label += fmt.Sprintf(" (after %s credit for %s)", audFmt(take), nonEmpty(l.Domain, l.Label))
				into = append(into, nonEmpty(best.Domain, best.Label))
			}
			if credit > 0.004 {
				g.Warnings = append(g.Warnings, fmt.Sprintf("%s of credits could not be netted (no charges left in this month)", audFmt(credit)))
			}
			l.Credits = r2(-l.Amount - credit)
			l.Label += fmt.Sprintf(" — %s credit netted into %s", audFmt(l.Credits), strings.Join(into, ", "))
			l.Amount = 0
		}
		for _, l := range lines {
			if l.Amount <= 0.004 {
				g.Lines = append(g.Lines, l.StatementLine)
			}
		}
		pos := []StatementLine{}
		for _, l := range lines {
			if l.Amount > 0.004 {
				pos = append(pos, l.StatementLine)
				g.Total = r2(g.Total + l.Amount)
			}
		}
		g.Lines = append(pos, g.Lines...)
		if g.Total <= 0 {
			continue
		}
		g.ChargeFrom, g.ChargeTo = a.first.Format(dayFmt), a.last.Format(dayFmt)
		g.BillDate, g.PaidDate = g.ChargeTo, g.ChargeTo
		if basis == "service" && g.ChargeFrom[:7] < g.Month {
			g.BillDate = g.Month + "-01"
		}
		out = append(out, *g)
	}
	return out
}

func statementExtID(g StatementGroup) string { return g.Month + "|" + g.Category }

func linesEqual(a, b []StatementLine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Label != b[i].Label || math.Abs(a[i].Amount-b[i].Amount) > 0.004 || a[i].ChargeDate != b[i].ChargeDate {
			return false
		}
	}
	return true
}

func (s *Service) synergyVendor(org string) *Vendor { return s.vendorByKey(org, "synergy") }

// statementOverlaps lists Synergy invoices / receipts (not statement expenses)
// dated in the given months, and what each matches in the statement.
func (s *Service) statementOverlaps(org string, months map[string]bool, txns []SynergyTxn) []StatementOverlap {
	out := []StatementOverlap{}
	v := s.synergyVendor(org)
	if v == nil || len(months) == 0 {
		return out
	}
	var items []Item
	s.DB.Where("organization_id = ? AND vendor_id = ? AND source <> ? AND status IN ? AND kind <> ?", org, v.ID, synStatementSource,
		[]string{StatusPending, StatusApproved}, KindDeclined).Order("invoice_date").Find(&items)
	for _, it := range items {
		if it.InvoiceDate == nil || !months[monthOf(*it.InvoiceDate)] {
			continue
		}
		o := StatementOverlap{ItemID: it.ID, Source: it.Source, Status: it.Status, InvoiceNumber: it.InvoiceNumber, Date: it.InvoiceDate.Format(dayFmt), Total: it.Total, Currency: it.Currency}
		for _, t := range txns {
			if t.Kind == SynTopup && math.Abs(t.Credit-it.Total) < 0.005 && math.Abs(Day(t.Date).Sub(Day(*it.InvoiceDate)).Hours()) <= 72 {
				o.Match = fmt.Sprintf("the %s top-up on %s (money added to your prepaid balance)", audFmt(t.Credit), t.Date.Format("2 Jan 2006"))
				break
			}
		}
		if o.Match == "" {
			o.Match = "no statement line with this amount — check it is not a cost the statement already shows"
		}
		out = append(out, o)
	}
	return out
}

// StatementExpenses previews the expenses statements would create.
func (s *Service) StatementExpenses(org string, in StatementExpenseInput) (*StatementPreview, error) {
	in.DryRun = true
	return s.CreateStatementExpenses(org, "", in)
}

// CreateStatementExpenses creates / updates the statement expenses.
func (s *Service) CreateStatementExpenses(org, user string, in StatementExpenseInput) (*StatementPreview, error) {
	if in.Basis != "service" {
		in.Basis = "charge"
	}
	var txns []SynergyTxn
	s.DB.Where("organization_id = ?", org).Order("date, seq").Find(&txns)
	avail := map[string]bool{}
	for _, t := range txns {
		if t.Kind != SynTopup && math.Abs(t.Amount) >= 0.005 {
			avail[groupMonth(t, in.Basis)] = true
		}
	}
	p := &StatementPreview{Basis: in.Basis, Months: []string{}, AvailableMonths: []string{}, Groups: []StatementGroup{}, Overlaps: []StatementOverlap{}, Errors: []string{}}
	for m := range avail {
		p.AvailableMonths = append(p.AvailableMonths, m)
	}
	sort.Strings(p.AvailableMonths)
	months := map[string]bool{}
	for _, m := range in.Months {
		m = strings.TrimSpace(m)
		if _, err := time.Parse("2006-01", m); err != nil {
			return nil, invalid("months must look like 2026-09")
		}
		months[m] = true
	}
	if len(months) == 0 {
		for m := range avail {
			months[m] = true
		}
	}
	for m := range months {
		p.Months = append(p.Months, m)
	}
	sort.Strings(p.Months)
	v := s.synergyVendor(org)
	if v == nil {
		return nil, invalid("the Synergy Wholesale vendor is missing")
	}
	p.CountFrom = nonEmpty(v.CountFrom, "invoices")
	if len(txns) == 0 {
		p.Note = "Upload a Synergy statement first."
		return p, nil
	}
	p.Groups = BuildStatementGroups(txns, in.Basis, months)
	// Months the expenses touch: their own month and the charge months.
	touched := map[string]bool{}
	for _, g := range p.Groups {
		touched[g.Month] = true
		for _, l := range g.Lines {
			for m := range l.ByMonth {
				touched[m] = true
			}
		}
		p.Total = r2(p.Total + g.Total)
	}
	p.Overlaps = s.statementOverlaps(org, touched, txns)
	existing := map[string]Item{}
	var cur []Item
	s.DB.Where("organization_id = ? AND source = ?", org, synStatementSource).Find(&cur)
	for _, it := range cur {
		existing[it.ExternalID] = it
	}
	for i := range p.Groups {
		g := &p.Groups[i]
		g.Change = "new"
		if it, ok := existing[statementExtID(*g)]; ok {
			g.ItemID, g.ItemStatus = it.ID, it.Status
			g.Change = "update"
			tax, _ := s.synergyTax(org, g.Total)
			bill, _ := time.Parse(dayFmt, g.BillDate)
			rate := s.RateFor(org, "AUD", s.Home(org), bill)
			if math.Abs(it.Total-g.Total) < 0.005 && linesEqual(it.Lines, g.Lines) && it.Basis == in.Basis && it.InvoiceDate != nil && it.InvoiceDate.Format(dayFmt) == g.BillDate &&
				math.Abs(it.Tax-tax) < 0.005 && it.HomeCurrency == s.Home(org) && sameRate(it.FxRate, rate.Rate) {
				g.Change = "unchanged"
			}
		}
	}
	if in.refresh {
		// Only months that already have statement expenses.
		keep := []StatementGroup{}
		for _, g := range p.Groups {
			if g.ItemID != "" {
				keep = append(keep, g)
			}
		}
		p.Groups = keep
	}
	approvedOverlap := 0
	for _, o := range p.Overlaps {
		if o.Status == StatusApproved {
			approvedOverlap++
		}
	}
	switch {
	case approvedOverlap > 0 && p.CountFrom == "statement":
		p.Note = fmt.Sprintf("%d Synergy invoice(s)/receipt(s) in these months are approved although Synergy is counted from statements — they will be un-approved and ignored.", approvedOverlap)
	case approvedOverlap > 0:
		p.Note = fmt.Sprintf("%d Synergy invoice(s)/receipt(s) in these months are already approved — counting the statement as well would count Synergy twice. Use the statement as the source of truth to ignore them.", approvedOverlap)
	case len(p.Overlaps) > 0:
		p.Note = fmt.Sprintf("%d Synergy invoice(s)/receipt(s) in these months are waiting in the inbox — with the statement as the source of truth they are ignored (they are prepaid top-ups).", len(p.Overlaps))
	}
	if in.DryRun {
		return p, nil
	}
	useStatement := in.UseStatement || p.CountFrom == "statement"
	if approvedOverlap > 0 && !useStatement && !in.refresh {
		return nil, invalid("%d Synergy invoice(s)/receipt(s) in these months are already approved — counting the statement too would count Synergy twice. Tick “Use the statement as the source of truth”.", approvedOverlap)
	}
	if !in.refresh && useStatement {
		if v.CountFrom != "statement" {
			s.DB.Model(&Vendor{}).Where("id = ?", v.ID).Updates(map[string]any{"count_from": "statement", "updated_at": s.now()})
			p.CountFrom = "statement"
		}
		for _, o := range p.Overlaps {
			if o.Status == StatusApproved {
				if _, err := s.Unapprove(org, user, o.ItemID); err != nil {
					p.Errors = append(p.Errors, fmt.Sprintf("%s: %v", nonEmpty(o.InvoiceNumber, o.Date), err))
					continue
				}
			}
			if _, err := s.SetStatus(org, o.ItemID, StatusIgnored, "Synergy is counted from the monthly statement — this is "+o.Match); err == nil {
				p.Ignored++
			}
		}
	}
	inGroups := map[string]bool{}
	for i := range p.Groups {
		g := &p.Groups[i]
		inGroups[statementExtID(*g)] = true
		it, err := s.upsertStatementItem(org, user, v, g, in.Basis)
		if err != nil {
			p.Errors = append(p.Errors, fmt.Sprintf("%s: %v", g.Label, err))
			continue
		}
		g.ItemID, g.ItemStatus = it.ID, it.Status
		switch g.Change {
		case "new":
			p.Created++
		case "update":
			p.Updated++
		default:
			p.Unchanged++
		}
	}
	// Statement expenses of these months that no longer have lines.
	if !in.refresh {
		for _, it := range cur {
			m := strings.SplitN(it.ExternalID, "|", 2)[0]
			if !months[m] || inGroups[it.ExternalID] {
				continue
			}
			if it.Status == StatusApproved {
				if _, err := s.Unapprove(org, user, it.ID); err != nil {
					p.Errors = append(p.Errors, err.Error())
					continue
				}
			}
			s.DB.Where("item_id = ?", it.ID).Delete(&Allocation{})
			s.DB.Delete(&Item{}, "id = ?", it.ID)
			p.Removed++
		}
	}
	if in.Approve {
		for i := range p.Groups {
			g := &p.Groups[i]
			if g.ItemID == "" || g.ItemStatus == StatusApproved {
				continue
			}
			it, err := s.Approve(org, user, g.ItemID, ApproveInput{})
			if err != nil {
				p.Errors = append(p.Errors, fmt.Sprintf("%s: %v", g.Label, err))
				continue
			}
			g.ItemStatus = it.Status
			p.Approved++
		}
	}
	return p, nil
}

// RefreshStatementItems updates existing statement expenses after a
// re-upload (same basis as they were created with). Returns how many changed.
func (s *Service) RefreshStatementItems(org, user string, months []string) (int, error) {
	var cur []Item
	s.DB.Where("organization_id = ? AND source = ?", org, synStatementSource).Find(&cur)
	if len(cur) == 0 {
		return 0, nil
	}
	basis := cur[0].Basis
	// The months to refresh: the statement's months, and (service basis) the next month too.
	ms := map[string]bool{}
	for _, m := range months {
		ms[m] = true
		if t, err := time.Parse("2006-01", m); err == nil {
			ms[t.AddDate(0, 1, 0).Format("2006-01")] = true
		}
	}
	list := []string{}
	for m := range ms {
		list = append(list, m)
	}
	p, err := s.CreateStatementExpenses(org, user, StatementExpenseInput{Months: list, Basis: basis, refresh: true})
	if err != nil {
		return 0, err
	}
	return p.Updated, nil
}

// upsertStatementItem creates or updates the expense of one group.
func (s *Service) upsertStatementItem(org, user string, v *Vendor, g *StatementGroup, basis string) (*Item, error) {
	ext := statementExtID(*g)
	bill, _ := time.Parse(dayFmt, g.BillDate)
	paid, _ := time.Parse(dayFmt, g.PaidDate)
	mStart, _ := time.Parse("2006-01", g.Month)
	mEnd := mStart.AddDate(0, 1, -1)
	tax, taxNote := s.synergyTax(org, g.Total)
	desc := []string{}
	for _, l := range g.Lines {
		desc = append(desc, fmt.Sprintf("%s: %s", l.Label, audFmt(l.Amount)))
	}
	ref := fmt.Sprintf("SYN-%s-%s", g.Month, strings.ToUpper(g.Category))
	service := fmt.Sprintf("%s — %s (statement)", CategoryLabels[g.Category], monthLabelOf(g.Month))
	ex := s.exists(org, synStatementSource, ext)
	if ex == nil {
		it := &Item{ID: uuid.NewString(), OrganizationID: org, Source: synStatementSource, ExternalID: ext, Status: StatusPending, Kind: KindStatement, VendorID: v.ID, VendorName: v.Name,
			InvoiceNumber: ref, InvoiceDate: &bill, PeriodStart: &mStart, PeriodEnd: &mEnd, Service: service, Description: strings.Join(desc, "\n"), Category: g.Category,
			Paid: true, PaidDate: &paid, Currency: "AUD", Total: g.Total, Tax: tax, TaxNote: taxNote, Confidence: 1, Method: "statement", Warnings: g.Warnings,
			Subject: "Synergy Wholesale statement", Lines: g.Lines, Basis: basis, CreatedAt: s.now(), UpdatedAt: s.now()}
		s.convert(org, it, true)
		it.DedupeKey = dedupeKey(v.ID+v.Name, it)
		if err := s.DB.Create(it).Error; err != nil {
			return nil, err
		}
		return it, nil
	}
	if g.Change == "unchanged" {
		return ex, nil
	}
	wasApproved := ex.Status == StatusApproved
	if wasApproved {
		if _, err := s.Unapprove(org, nonEmpty(user, ex.ApprovedBy), ex.ID); err != nil {
			return nil, err
		}
	}
	ex.InvoiceDate, ex.PeriodStart, ex.PeriodEnd, ex.PaidDate = &bill, &mStart, &mEnd, &paid
	ex.Total, ex.Tax, ex.TaxNote, ex.Currency, ex.Lines, ex.Basis = g.Total, tax, taxNote, "AUD", g.Lines, basis
	ex.Service, ex.Description, ex.InvoiceNumber, ex.Warnings = service, strings.Join(desc, "\n"), ref, g.Warnings
	ex.Category = g.Category
	s.convert(org, ex, true)
	lj, _ := json.Marshal(ex.Lines)
	if ex.Warnings == nil {
		ex.Warnings = []string{}
	}
	wj, _ := json.Marshal(ex.Warnings)
	if err := s.DB.Model(&Item{}).Where("id = ?", ex.ID).Updates(map[string]any{"invoice_date": ex.InvoiceDate, "period_start": ex.PeriodStart, "period_end": ex.PeriodEnd,
		"paid_date": ex.PaidDate, "total": ex.Total, "tax": ex.Tax, "net": ex.Net, "tax_note": ex.TaxNote, "currency": ex.Currency, "lines": string(lj), "basis": basis,
		"service": ex.Service, "description": ex.Description, "invoice_number": ex.InvoiceNumber, "warnings": string(wj), "home_currency": ex.HomeCurrency,
		"fx_rate": ex.FxRate, "fx_source": ex.FxSource, "fx_date": ex.FxDate, "fx_basis": ex.FxBasis, "fx_note": ex.FxNote, "home_total": ex.HomeTotal, "home_tax": ex.HomeTax, "home_net": ex.HomeNet,
		"status": map[bool]string{true: StatusPending, false: ex.Status}[wasApproved || ex.Status == StatusIgnored], "updated_at": s.now()}).Error; err != nil {
		return nil, err
	}
	if wasApproved {
		return s.Approve(org, nonEmpty(user, ex.ApprovedBy), ex.ID, ApproveInput{})
	}
	return s.Get(org, ex.ID)
}

// createStatementBill makes the supplier bill of a statement expense: a line
// per domain / fee and a payment per charge month (dated on the charge), so a
// cash P&L follows the charges and an accrual P&L the bill date.
func (s *Service) createStatementBill(org, user string, it *Item, finVendor, accID string, in ApproveInput) (*finance.Bill, error) {
	home := it.HomeCurrency
	gst := it.Tax > 0
	lines := []finance.BillLineInput{}
	byMonth := map[string]float64{}
	lastDate := map[string]string{}
	for _, l := range it.Lines {
		for m, v := range l.ByMonth {
			byMonth[m] += v
			if l.LastByMon[m] > lastDate[m] {
				lastDate[m] = l.LastByMon[m]
			}
		}
		if len(l.ByMonth) == 0 && l.Amount > 0 { // lines stored before ByMonth existed
			byMonth[l.ChargeMon] += l.Amount
			if l.ChargeDate > lastDate[l.ChargeMon] {
				lastDate[l.ChargeMon] = l.ChargeDate
			}
		}
		if l.Amount <= 0 {
			continue
		}
		h := fxrates.Convert(l.Amount, it.FxRate, home)
		unit, rate := h, 0.0
		if gst {
			// GST included (1/11): the rate is fine-tuned so the line total is exactly what was charged.
			unit = r2(h / 1.1)
			rate = math.Round((h-unit)/unit*100*10000) / 10000
		}
		lines = append(lines, finance.BillLineInput{Description: clip(l.Label, 250), AccountID: accID, Quantity: 1, UnitPrice: unit, TaxRate: rate})
	}
	// Payments: one per charge month with a net charge, sharing the bill total.
	var gross float64
	for m, v := range byMonth {
		if v <= 0.004 {
			delete(byMonth, m)
			continue
		}
		gross += v
	}
	if len(lines) == 0 {
		return nil, invalid("this statement expense has no lines")
	}
	billDate := it.InvoiceDate.Format(dayFmt)
	due := billDate
	if it.PaidDate != nil && it.PaidDate.After(*it.InvoiceDate) {
		due = it.PaidDate.Format(dayFmt)
	}
	notes := fmt.Sprintf("Created from Synergy Wholesale statements (%s, %s basis). Amounts charged in AUD incl. GST.", monthLabelOf(it.ExternalID[:7]), nonEmpty(it.Basis, "charge"))
	if it.Currency != home {
		notes += fmt.Sprintf(" Converted at %s %s per AUD (%s rate).", trimFloat(it.FxRate), home, it.FxSource)
	}
	if it.TaxNote != "" {
		notes += " " + it.TaxNote + "."
	}
	bill, err := s.Fin.CreateBill(org, user, finance.BillInput{VendorID: finVendor, Reference: clip(it.InvoiceNumber, 120), BillDate: billDate, DueDate: due, Notes: notes, LineItems: lines})
	if err != nil {
		return nil, err
	}
	ms := []string{}
	for m := range byMonth {
		ms = append(ms, m)
	}
	sort.Strings(ms)
	var paidSoFar float64
	for i, m := range ms {
		amt := r2(bill.Total * byMonth[m] / gross)
		if i == len(ms)-1 {
			amt = r2(bill.Total - paidSoFar)
		}
		if amt <= 0 {
			continue
		}
		if _, err := s.Fin.RecordBillPayment(org, user, bill.ID, finance.PaymentInput{Amount: amt, PaymentDate: lastDate[m], PaymentMethod: nonEmpty(in.PaymentMethod, "card"),
			BankAccountID: in.BankAccountID, Reference: it.InvoiceNumber, Notes: "Synergy Wholesale prepaid balance (" + monthLabelOf(m) + " charges)"}); err != nil {
			if b, gerr := s.Fin.GetBill(org, bill.ID); gerr == nil {
				for _, p := range b.Payments {
					_, _ = s.Fin.DeleteBillPayment(org, user, b.ID, p.ID)
				}
			}
			_ = s.Fin.DeleteBill(org, user, bill.ID)
			return nil, err
		}
		paidSoFar = r2(paidSoFar + amt)
	}
	return bill, nil
}

// countOnceGuard refuses approving a Synergy invoice / receipt for a month the
// statement already counts, and a statement expense for a month whose Synergy
// invoices / receipts are approved.
func (s *Service) countOnceGuard(org string, it *Item) error {
	v := s.synergyVendor(org)
	if v == nil || it.VendorID != v.ID || it.InvoiceDate == nil {
		return nil
	}
	if it.Source == synStatementSource {
		months := map[string]bool{monthOf(*it.InvoiceDate): true}
		for _, l := range it.Lines {
			for m := range l.ByMonth {
				months[m] = true
			}
		}
		var others []Item
		s.DB.Where("organization_id = ? AND vendor_id = ? AND source <> ? AND status = ?", org, v.ID, synStatementSource, StatusApproved).Find(&others)
		n, total := 0, 0.0
		for _, o := range others {
			if o.InvoiceDate != nil && months[monthOf(*o.InvoiceDate)] {
				n++
				total += o.Total
			}
		}
		if n > 0 {
			return invalid("%d Synergy invoice(s)/receipt(s) for %s are already approved (%s) — Synergy would be counted twice. Use “Create expenses from statement” with the statement as the source of truth, or undo those first.",
				n, monthLabelOf(monthOf(*it.InvoiceDate)), fmtMoney(total, "AUD"))
		}
		return nil
	}
	m := monthOf(*it.InvoiceDate)
	var stmts []Item
	s.DB.Where("organization_id = ? AND source = ? AND status = ?", org, synStatementSource, StatusApproved).Find(&stmts)
	for _, st := range stmts {
		months := map[string]bool{}
		if st.InvoiceDate != nil {
			months[monthOf(*st.InvoiceDate)] = true
		}
		for _, l := range st.Lines {
			for m := range l.ByMonth {
				months[m] = true
			}
		}
		if months[m] {
			return invalid("Synergy costs for %s are already counted from the statement (%s) — this invoice/receipt is a top-up of the prepaid balance; ignore it instead", monthLabelOf(m), st.InvoiceNumber)
		}
	}
	return nil
}
