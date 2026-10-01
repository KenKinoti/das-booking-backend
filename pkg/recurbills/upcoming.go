package recurbills

import (
	"sort"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
)

// UpcomingItem is one row of the upcoming-bills view (and one reminder
// target): an open bill, or a period of a schedule that has no bill yet.
type UpcomingItem struct {
	Key              string  `json:"key"`  // bill:<id> | occ:<recurring id>:<period>
	Kind             string  `json:"kind"` // bill | projected
	BillID           string  `json:"bill_id,omitempty"`
	BillNumber       string  `json:"bill_number,omitempty"`
	BillStatus       string  `json:"bill_status,omitempty"`
	RecurringID      string  `json:"recurring_id,omitempty"`
	RecurringName    string  `json:"recurring_name,omitempty"`
	Period           string  `json:"period,omitempty"`
	VendorID         string  `json:"vendor_id"`
	Vendor           string  `json:"vendor"`
	Name             string  `json:"name"`
	Service          string  `json:"service,omitempty"`
	SynergyDomain    string  `json:"synergy_domain,omitempty"`
	BillDate         string  `json:"bill_date"`
	DueDate          string  `json:"due_date"`
	Amount           float64 `json:"amount"` // home currency, still to pay
	Total            float64 `json:"total"`  // home currency
	OriginalAmount   float64 `json:"original_amount"`
	OriginalCurrency string  `json:"original_currency"`
	Status           string  `json:"status"` // projected | awaiting | draft | unpaid | partial | overdue | skipped | paid
	Estimate         bool    `json:"estimate"`
	AutoPay          bool    `json:"auto_pay"`
	Mode             string  `json:"mode,omitempty"`
	DaysUntilDue     int     `json:"days_until_due"`
	SnoozedUntil     string  `json:"snoozed_until,omitempty"`
	LastPaid         string  `json:"last_paid,omitempty"`
	LastReminder     string  `json:"last_reminder,omitempty"`
	LastReminderAt   string  `json:"last_reminder_at,omitempty"`
	RemindersOff     bool    `json:"reminders_off,omitempty"`
	Reference        string  `json:"reference,omitempty"`

	sched *RecurringBill
	due   time.Time
}

// MonthTotal is the amount due in one month.
type MonthTotal struct {
	Month string  `json:"month"` // YYYY-MM
	Total float64 `json:"total"`
	Count int     `json:"count"`
}

// Upcoming is the upcoming-bills view.
type Upcoming struct {
	Today    string         `json:"today"`
	Currency string         `json:"currency"`
	Days     int            `json:"days"`
	Items    []UpcomingItem `json:"items"`
	Months   []MonthTotal   `json:"months"`
	Totals   map[string]any `json:"totals"`
}

func dayStr(t time.Time) string { return t.Format(dayFmt) }

func daysBetween(a, b time.Time) int { return int(Day(b).Sub(Day(a)).Hours() / 24) }

// UpcomingFor builds the view for the next `days` days (plus everything
// overdue). Each bill appears once: a period with a bill is shown as that
// bill, periods without one as projected.
func (s *Service) UpcomingFor(org string, days int) *Upcoming {
	if days <= 0 || days > 366 {
		days = 90
	}
	today := s.Today(org)
	horizon := today.AddDate(0, 0, days)
	home := s.home(org)
	names := s.vendorNames(org)
	var scheds []RecurringBill
	s.DB.Where("organization_id = ?", org).Find(&scheds)
	sm := map[string]*RecurringBill{}
	for i := range scheds {
		sm[scheds[i].ID] = &scheds[i]
	}
	var occs []Occurrence
	s.DB.Where("organization_id = ?", org).Find(&occs)
	occByBill := map[string]*Occurrence{}
	occByPeriod := map[string]*Occurrence{}
	for i := range occs {
		o := &occs[i]
		if o.BillID != "" {
			occByBill[o.BillID] = o
		}
		occByPeriod[o.RecurringID+"|"+dayStr(Day(o.Period))] = o
	}
	var snoozes []BillSnooze
	s.DB.Where("organization_id = ? AND until > ?", org, today).Find(&snoozes)
	snz := map[string]string{}
	for _, z := range snoozes {
		snz[z.BillID] = dayStr(Day(z.Until))
	}

	items := []UpcomingItem{}
	// 1. Bills: open ones due by the horizon (any age), plus paid bills of
	// schedule periods inside the window (so a paid period shows as paid).
	var bills []finance.Bill
	s.DB.Where("organization_id = ? AND due_date <= ? AND (status IN ? OR id IN (SELECT bill_id FROM finance_recurring_occurrences WHERE organization_id = ? AND bill_id <> ''))",
		org, horizon.Add(24*time.Hour-time.Second), []string{"draft", "unpaid", "partial"}, org).Order("due_date").Find(&bills)
	for _, b := range bills {
		o := occByBill[b.ID]
		due := Day(b.DueDate)
		if b.Status == "paid" || b.Status == "void" {
			if o == nil || due.Before(today) {
				continue
			}
		}
		if b.Status == "draft" && o == nil {
			continue // a one-off draft isn't a commitment yet
		}
		it := UpcomingItem{Key: "bill:" + b.ID, Kind: "bill", BillID: b.ID, BillNumber: b.BillNumber, BillStatus: b.Status, VendorID: b.VendorID, Vendor: names[b.VendorID],
			BillDate: dayStr(Day(b.BillDate)), DueDate: dayStr(due), Total: b.Total, Amount: r2(b.Total - b.PaidAmount), OriginalAmount: b.Total, OriginalCurrency: home,
			DaysUntilDue: daysBetween(today, due), SnoozedUntil: snz[b.ID], Reference: b.Reference, due: due}
		it.Name = b.Reference
		var line finance.BillLineItem
		if s.DB.Where("bill_id = ?", b.ID).Order("sort_order").First(&line).Error == nil {
			it.Name = line.Description
		}
		switch {
		case b.Status == "paid":
			it.Status, it.Amount = "paid", 0
		case b.Status == "draft":
			it.Status = "draft"
		case due.Before(today):
			it.Status = "overdue"
		default:
			it.Status = b.Status
		}
		if o != nil {
			if r := sm[o.RecurringID]; r != nil {
				it.RecurringID, it.RecurringName, it.Period = r.ID, r.Name, dayStr(Day(o.Period))
				it.Estimate = r.AmountMode == AmountEstimate && b.Status == "draft"
				it.AutoPay, it.Mode, it.Service, it.SynergyDomain, it.RemindersOff = r.AutoPay, r.Mode, r.Service, r.SynergyDomain, r.RemindersOff
				it.OriginalAmount, it.OriginalCurrency = o.Amount, o.Currency
				if o.Currency == home || o.Amount == 0 {
					it.OriginalAmount, it.OriginalCurrency = b.Total, home
				}
				it.sched = r
			}
			if o.SnoozedUntil != nil && Day(*o.SnoozedUntil).After(today) {
				it.SnoozedUntil = dayStr(Day(*o.SnoozedUntil))
			}
		}
		items = append(items, it)
	}

	// 2. Schedule periods without a bill.
	for i := range scheds {
		r := &scheds[i]
		if r.Status != StatusActive {
			continue
		}
		rule := RuleOf(r)
		from := today.AddDate(0, 0, -60)
		if r.ResumedAt != nil && Day(*r.ResumedAt).After(from) {
			from = Day(*r.ResumedAt)
		}
		// Periods whose due date falls in the window.
		for _, oc := range rule.Between(from.AddDate(0, 0, -r.DueDays), horizon.AddDate(0, 0, -r.DueDays), 400) {
			period := Day(oc.Date)
			due := period.AddDate(0, 0, r.DueDays)
			ex := occByPeriod[r.ID+"|"+dayStr(period)]
			if ex != nil && ex.BillID != "" {
				continue // shown as its bill
			}
			if ex == nil && r.Mode == ModeCreate && due.Before(today) {
				continue // generated on the next run; never show a stale projection
			}
			it := UpcomingItem{Key: "occ:" + r.ID + ":" + dayStr(period), Kind: "projected", RecurringID: r.ID, RecurringName: r.Name, Period: dayStr(period),
				VendorID: r.VendorID, Vendor: names[r.VendorID], Name: nz(r.Description, r.Name), Service: r.Service, SynergyDomain: r.SynergyDomain,
				BillDate: dayStr(period), DueDate: dayStr(due), OriginalAmount: r.Amount, OriginalCurrency: r.Currency, Estimate: r.AmountMode == AmountEstimate,
				AutoPay: r.AutoPay, Mode: r.Mode, DaysUntilDue: daysBetween(today, due), RemindersOff: r.RemindersOff, Reference: r.Reference, sched: r, due: due}
			amt := r.Amount
			if r.Currency != home {
				if q := s.FX.Quote(org, r.Currency, home); q.OK() {
					amt = fxrates.Convert(r.Amount, q.Rate, home)
				}
			}
			it.Amount, it.Total = r2(amt), r2(amt)
			switch {
			case ex != nil && ex.Status == OccSkipped:
				it.Status, it.Amount = "skipped", 0
			case ex != nil && ex.Status == OccPaid:
				it.Status, it.Amount = "paid", 0
			case r.Mode == ModeTrack && due.Before(today):
				it.Status = "overdue"
			case r.Mode == ModeTrack:
				it.Status = "awaiting"
			default:
				it.Status = "projected"
			}
			if ex != nil && ex.SnoozedUntil != nil && Day(*ex.SnoozedUntil).After(today) {
				it.SnoozedUntil = dayStr(Day(*ex.SnoozedUntil))
			}
			if ex == nil || ex.Status == "snoozed" || ex.Status == OccPaid || ex.Status == OccSkipped {
				items = append(items, it)
			}
		}
	}

	// Last paid per schedule and the latest reminder per item.
	lastPaid := map[string]string{}
	for _, r := range s.List(org) {
		lastPaid[r.ID] = r.LastPaid
	}
	var logs []ReminderLog
	s.DB.Where("organization_id = ? AND kind = ? AND status = ?", org, "reminder", "sent").Order("created_at").Find(&logs)
	lastRem := map[string]ReminderLog{}
	for _, l := range logs {
		k := l.Key
		if i := strings.LastIndex(k, "|"); i > 0 {
			k = k[:i]
		}
		lastRem[k] = l
	}
	for i := range items {
		it := &items[i]
		if it.RecurringID != "" {
			it.LastPaid = lastPaid[it.RecurringID]
		}
		if l, ok := lastRem[it.Key]; ok {
			it.LastReminder, it.LastReminderAt = l.Stage, l.CreatedAt.Format(time.RFC3339)
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].DueDate != items[j].DueDate {
			return items[i].DueDate < items[j].DueDate
		}
		return items[i].Vendor < items[j].Vendor
	})
	months := map[string]*MonthTotal{}
	var overdue, next30, next90 float64
	nOver, nOpen := 0, 0
	for _, it := range items {
		if it.Status == "skipped" || it.Status == "paid" {
			continue
		}
		nOpen++
		if it.Status == "overdue" {
			overdue += it.Amount
			nOver++
		}
		if it.DaysUntilDue <= 30 {
			next30 += it.Amount
		}
		next90 += it.Amount
		m := it.DueDate[:7]
		if it.due.Before(today) {
			m = today.Format("2006-01") // overdue counts in the current month
		}
		if months[m] == nil {
			months[m] = &MonthTotal{Month: m}
		}
		months[m].Total = r2(months[m].Total + it.Amount)
		months[m].Count++
	}
	ml := []MonthTotal{}
	for _, m := range months {
		ml = append(ml, *m)
	}
	sort.Slice(ml, func(i, j int) bool { return ml[i].Month < ml[j].Month })
	return &Upcoming{Today: dayStr(today), Currency: home, Days: days, Items: items, Months: ml,
		Totals: map[string]any{"overdue": r2(overdue), "overdue_count": nOver, "next_30": r2(next30), "window": r2(next90), "open_count": nOpen}}
}
