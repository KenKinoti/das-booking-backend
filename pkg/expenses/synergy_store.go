package expenses

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"
)

// SynergyUploadSummary reports what one uploaded Synergy file added.
type SynergyUploadSummary struct {
	UploadID     string              `json:"upload_id"`
	FileType     string              `json:"file_type"` // statement | usage
	Filename     string              `json:"filename"`
	RowsRead     int                 `json:"rows_read"`
	New          int                 `json:"new"`
	Duplicates   int                 `json:"duplicates"`
	Updated      int                 `json:"updated"`
	Warnings     []string            `json:"warnings"`
	PeriodFrom   string              `json:"period_from,omitempty"`
	PeriodTo     string              `json:"period_to,omitempty"`
	Months       []string            `json:"months,omitempty"`
	SnapshotDate string              `json:"snapshot_date,omitempty"`
	DateSource   string              `json:"date_source,omitempty"` // file name | chosen
	Opening      *float64            `json:"opening_balance"`
	Closing      *float64            `json:"closing_balance"`
	Reconciled   bool                `json:"reconciled"`         // the file's own running balance adds up
	Continuity   string              `json:"continuity"`         // ok | gap | first — against the statements already stored
	Gaps         []BalanceGap        `json:"gaps"`               // inside the file and against neighbouring statements
	Kinds        map[string]int      `json:"kinds,omitempty"`    // statement: lines per kind
	Expenses     float64             `json:"expenses,omitempty"` // statement: debits − service credits (AUD)
	Topups       float64             `json:"topups,omitempty"`   // statement: money added to the account (AUD)
	Appeared     []string            `json:"appeared,omitempty"` // usage: sites not in the previous snapshot
	Gone         []string            `json:"gone,omitempty"`     // usage: sites in the previous snapshot but not this one
	Previous     string              `json:"previous_snapshot,omitempty"`
	ItemsUpdated int                 `json:"expense_items_updated,omitempty"` // statement expenses refreshed after this upload
	Accounting   *SynergyApplyResult `json:"accounting,omitempty"`            // what the automatic Synergy expenses did
}

func monthOf(t time.Time) string { return t.Format("2006-01") }

// serviceMonth is the month a charge pays for: the start of its service
// period (monthly hosting charged on 25 Aug for 1 Sep – 1 Oct → 2026-09).
func serviceMonth(r SynRow) string {
	if r.Kind == SynHostingMonthly && r.PeriodFrom != nil {
		return monthOf(*r.PeriodFrom)
	}
	return monthOf(r.Date)
}

// ImportSynergyStatement stores the lines of a statement CSV.
func (s *Service) ImportSynergyStatement(org, user, name string, data []byte, docID, sha string) (*SynergyUploadSummary, error) {
	rows, warns, err := ParseSynergyStatement(data)
	if err != nil {
		return nil, invalid("%s: %v", name, err)
	}
	if len(rows) == 0 {
		return nil, invalid("%s: no transactions found", name)
	}
	sum := &SynergyUploadSummary{UploadID: uuid.NewString(), FileType: SynFileStatement, Filename: name, RowsRead: len(rows), Warnings: warns, Kinds: map[string]int{}, Gaps: []BalanceGap{}}
	ordered := append([]SynRow(nil), rows...)
	sortSynRows(ordered)
	sum.PeriodFrom, sum.PeriodTo = ordered[0].Date.Format(dayFmt), ordered[len(ordered)-1].Date.Format(dayFmt)
	sum.Opening, sum.Closing = OpeningBalance(ordered), ClosingBalance(ordered)
	inner := ReconcileSynergy(ordered)
	sum.Reconciled = len(inner) == 0
	sum.Gaps = append(sum.Gaps, inner...)
	months := map[string]bool{}
	now := s.now()
	for _, r := range ordered {
		sum.Kinds[r.Kind]++
		months[monthOf(r.Date)] = true
		if r.Kind == SynTopup {
			sum.Topups += r.Credit - r.Debit
		} else {
			sum.Expenses += r.Amount()
		}
		t := SynergyTxn{ID: uuid.NewString(), OrganizationID: org, DedupeKey: synTxnKey(r), Date: r.Date, Seq: r.Seq, ProductType: clip(r.ProductType, 78),
			Description: r.Description, Domain: clip(r.Domain, 250), Kind: r.Kind, Category: r.Category, PeriodFrom: r.PeriodFrom, PeriodTo: r.PeriodTo,
			PlanFrom: clip(r.PlanFrom, 118), PlanTo: clip(r.PlanTo, 118), Credit: r.Credit, Debit: r.Debit, Amount: r.Amount(), Balance: r.Balance, HasBalance: r.HasBalance,
			Month: monthOf(r.Date), ServiceMonth: serviceMonth(r), UploadID: sum.UploadID, CreatedAt: now}
		res := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&t)
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			sum.Duplicates++
		} else {
			sum.New++
		}
	}
	sum.Expenses, sum.Topups = r2(sum.Expenses), r2(sum.Topups)
	for m := range months {
		sum.Months = append(sum.Months, m)
	}
	sort.Strings(sum.Months)

	// Continuity with the statements already stored: the line just before this
	// file must end on its opening balance, the line just after must follow on.
	sum.Continuity = "first"
	first, last := ordered[0], ordered[len(ordered)-1]
	var before, after []SynergyTxn
	s.DB.Where("organization_id = ? AND date < ?", org, first.Date).
		Order("date DESC, seq DESC").Limit(1).Find(&before)
	s.DB.Where("organization_id = ? AND date > ?", org, last.Date).Order("date, seq").Limit(1).Find(&after)
	keys := map[string]bool{}
	for _, r := range ordered {
		keys[synTxnKey(r)] = true
	}
	if len(before) == 1 && !keys[before[0].DedupeKey] && sum.Opening != nil {
		sum.Continuity = "ok"
		if b := before[0]; b.HasBalance && math.Abs(b.Balance-*sum.Opening) > 0.005 {
			sum.Continuity = "gap"
			d := r2(*sum.Opening - b.Balance)
			sum.Gaps = append(sum.Gaps, BalanceGap{After: b.Date.Format("2006-01-02 15:04:05"), Before: first.Date.Format("2006-01-02 15:04:05"), Expected: b.Balance, Actual: *sum.Opening, Difference: d,
				Message: fmt.Sprintf("Gap before this statement: the previous line (%s) ends on %s but this statement opens on %s (%s) — upload the statement for the months in between",
					b.Date.Format("2 Jan 2006"), audFmt(b.Balance), audFmt(*sum.Opening), audSigned(d))})
		}
	}
	if len(after) == 1 && !keys[after[0].DedupeKey] && sum.Closing != nil {
		if sum.Continuity == "first" {
			sum.Continuity = "ok"
		}
		a := after[0]
		if exp := r2(*sum.Closing + a.Credit - a.Debit); a.HasBalance && math.Abs(exp-a.Balance) > 0.005 {
			sum.Continuity = "gap"
			sum.Gaps = append(sum.Gaps, BalanceGap{After: last.Date.Format("2006-01-02 15:04:05"), Before: a.Date.Format("2006-01-02 15:04:05"), Expected: exp, Actual: a.Balance, Difference: r2(a.Balance - exp),
				Message: fmt.Sprintf("Gap after this statement: it closes on %s but the next stored line (%s) does not follow on — a statement may be missing",
					audFmt(*sum.Closing), a.Date.Format("2 Jan 2006"))})
		}
	}
	if !sum.Reconciled {
		sum.Warnings = append(sum.Warnings, fmt.Sprintf("The running balance does not add up in %d place(s) — see the gaps below", len(inner)))
	}
	up := SynergyUpload{ID: sum.UploadID, OrganizationID: org, Filename: clip(name, 250), SHA256: sha, FileType: SynFileStatement, DocumentID: docID, PeriodFrom: sum.PeriodFrom,
		PeriodTo: sum.PeriodTo, RowsRead: sum.RowsRead, New: sum.New, Duplicates: sum.Duplicates, Opening: sum.Opening, Closing: sum.Closing, Reconciled: sum.Reconciled && sum.Continuity != "gap",
		Warnings: sum.Warnings, CreatedBy: user, CreatedAt: now}
	s.DB.Create(&up)
	s.syncStatementBalance(org)
	// Create / update (and, with auto-approve, approve) the Synergy expenses
	// for the chosen accounting mode — idempotent, so a re-upload updates.
	if res, err := s.ApplySynergyAccounting(org, user); err == nil {
		sum.ItemsUpdated = res.Created + res.Updated
		sum.Accounting = res
		for _, e := range res.Errors {
			sum.Warnings = append(sum.Warnings, "Synergy expense: "+e)
		}
	} else {
		sum.Warnings = append(sum.Warnings, "Synergy expenses could not be created: "+err.Error())
	}
	return sum, nil
}

// syncStatementBalance records the newest statement balance as the Synergy
// account balance when it is newer than the API / email balance.
func (s *Service) syncStatementBalance(org string) {
	var t SynergyTxn
	if s.DB.Where("organization_id = ? AND has_balance", org).Order("date DESC, seq DESC").First(&t).Error != nil {
		return
	}
	when := t.Date
	bal := t.Balance
	c := s.connection(org, "synergy")
	if c == nil {
		s.DB.Create(&Connection{ID: uuid.NewString(), OrganizationID: org, Kind: "synergy", Balance: &bal, BalanceAt: &when, BalanceSource: "statement", CreatedAt: s.now(), UpdatedAt: s.now()})
		return
	}
	if c.BalanceAt == nil || c.BalanceAt.Before(when) || (c.BalanceSource == "statement" && !c.BalanceAt.Equal(when)) {
		s.DB.Model(&Connection{}).Where("id = ?", c.ID).Updates(map[string]any{"balance": bal, "balance_at": when, "balance_source": "statement"})
	}
}

// ImportSynergyUsage stores a usage report as the snapshot of date.
func (s *Service) ImportSynergyUsage(org, user, name string, data []byte, date *time.Time, docID, sha string) (*SynergyUploadSummary, error) {
	rows, warns, err := ParseSynergyUsage(data)
	if err != nil {
		return nil, invalid("%s: %v", name, err)
	}
	sum := &SynergyUploadSummary{UploadID: uuid.NewString(), FileType: SynFileUsage, Filename: name, RowsRead: len(rows), Warnings: warns, Gaps: []BalanceGap{}, Reconciled: true}
	if date != nil {
		sum.DateSource = "chosen"
	} else if date = SnapshotDateFromName(name); date != nil {
		sum.DateSource = "file name"
	} else {
		return nil, invalid("%s: choose the date of this usage report (it is not in the file name)", name)
	}
	d := Day(*date)
	if d.After(Day(s.now()).AddDate(0, 0, 1)) {
		return nil, invalid("%s: the snapshot date %s is in the future", name, d.Format(dayFmt))
	}
	sum.SnapshotDate = d.Format(dayFmt)
	var prevDate struct{ D *time.Time }
	s.DB.Raw("SELECT MAX(snapshot_date) AS d FROM synergy_usage_snapshots WHERE organization_id = ? AND snapshot_date < ?", org, d).Scan(&prevDate)
	prev := map[string]bool{}
	if prevDate.D != nil {
		sum.Previous = prevDate.D.Format(dayFmt)
		var doms []string
		s.DB.Model(&SynergyUsage{}).Where("organization_id = ? AND snapshot_date = ?", org, *prevDate.D).Pluck("domain", &doms)
		for _, x := range doms {
			prev[x] = true
		}
	}
	var existing []SynergyUsage
	s.DB.Where("organization_id = ? AND snapshot_date = ?", org, d).Find(&existing)
	ex := map[string]SynergyUsage{}
	for _, e := range existing {
		ex[e.Domain] = e
	}
	now := s.now()
	cur := map[string]bool{}
	for _, r := range rows {
		cur[r.Domain] = true
		if e, ok := ex[r.Domain]; ok {
			if e.Plan == r.Plan && e.UsageMB == r.UsageMB && e.LimitMB == r.LimitMB && e.Status == r.Status && e.RemainingMB == r.RemainingMB {
				sum.Duplicates++
				continue
			}
			s.DB.Model(&SynergyUsage{}).Where("id = ?", e.ID).Updates(map[string]any{"status": r.Status, "plan": r.Plan, "usage_mb": r.UsageMB, "limit_mb": r.LimitMB,
				"pct_used": r.PctUsed, "at_75": r.At75, "at_100mb": r.At100MB, "remaining_mb": r.RemainingMB, "upload_id": sum.UploadID, "updated_at": now})
			sum.Updated++
			continue
		}
		u := SynergyUsage{ID: uuid.NewString(), OrganizationID: org, SnapshotDate: d, Domain: clip(r.Domain, 250), Status: clip(r.Status, 38), Plan: clip(r.Plan, 118),
			UsageMB: r.UsageMB, LimitMB: r.LimitMB, PctUsed: r.PctUsed, At75: r.At75, At100MB: r.At100MB, RemainingMB: r.RemainingMB, UploadID: sum.UploadID, CreatedAt: now, UpdatedAt: now}
		if err := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&u).Error; err != nil {
			return nil, err
		}
		sum.New++
		if r.RemainingMB < 0 || r.PctUsed >= 100 {
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("%s is over quota (%.0f%% of %s on %s)", r.Domain, r.PctUsed, mbText(r.LimitMB), r.Plan))
		}
	}
	if prevDate.D != nil {
		for _, r := range rows {
			if !prev[r.Domain] {
				sum.Appeared = append(sum.Appeared, r.Domain)
			}
		}
		for x := range prev {
			if !cur[x] {
				sum.Gone = append(sum.Gone, x)
			}
		}
		sort.Strings(sum.Gone)
	}
	s.DB.Create(&SynergyUpload{ID: sum.UploadID, OrganizationID: org, Filename: clip(name, 250), SHA256: sha, FileType: SynFileUsage, DocumentID: docID, SnapshotDate: sum.SnapshotDate,
		RowsRead: sum.RowsRead, New: sum.New, Duplicates: sum.Duplicates, Updated: sum.Updated, Reconciled: true, Warnings: sum.Warnings, CreatedBy: user, CreatedAt: now})
	return sum, nil
}

// mbText shows a size in MB or GB.
func mbText(mb float64) string {
	if math.Abs(mb) >= 1024 {
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", mb/1024), "0"), ".") + " GB"
	}
	return fmt.Sprintf("%.0f MB", mb)
}

// importSynergyCSV routes an uploaded / emailed Synergy CSV; kind from DetectSynergyFile.
func (s *Service) importSynergyCSV(org, user, name, source, kind string, data []byte, date *time.Time) (*SynergyUploadSummary, error) {
	doc, _, err := s.StoreDocument(org, name, "text/csv", source, data, "")
	if err != nil {
		return nil, err
	}
	var sum *SynergyUploadSummary
	if kind == SynFileStatement {
		sum, err = s.ImportSynergyStatement(org, user, name, data, doc.ID, doc.SHA256)
	} else {
		sum, err = s.ImportSynergyUsage(org, user, name, data, date, doc.ID, doc.SHA256)
	}
	if err != nil {
		return nil, err
	}
	go s.notifySafe(org)
	return sum, nil
}

// notifySafe sends alert notifications without failing the caller.
func (s *Service) notifySafe(org string) {
	defer func() { _ = recover() }()
	if NotifyAfterUpload {
		s.NotifyAlerts(org)
	}
}

// NotifyAfterUpload: send notifications for new alerts after a Synergy upload
// (tests switch it off to keep them deterministic).
var NotifyAfterUpload = true
