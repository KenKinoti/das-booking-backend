package subscriptions

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
)

// Dashboard figures. MRR is the monthly equivalent of every active and
// past-due subscription's recurring amount before tax (suspended and
// cancelled subscriptions are not counted; proration is not recurring),
// converted to the home currency at today's rate (as the rest of the app
// does: manual rate → live → reference table). ARR is MRR × 12. These
// figures are a forecast: they are never added to revenue, the P&L or the
// invoicing totals — the invoices the subscriptions create are counted
// there like any other invoice.

// MRRLine is MRR in one currency.
type MRRLine struct {
	Currency string  `json:"currency"`
	MRR      float64 `json:"mrr"`
	MRRHome  float64 `json:"mrr_home"`
	Count    int     `json:"count"`
}

// RiskItem is a subscription to be suspended soon unless paid.
type RiskItem struct {
	SubscriptionID string  `json:"subscription_id"`
	Number         string  `json:"number"`
	Customer       string  `json:"customer"`
	CustomerID     string  `json:"customer_id"`
	Phone          string  `json:"phone,omitempty"`
	Service        string  `json:"service"`
	Invoice        string  `json:"invoice"`
	InvoiceID      string  `json:"invoice_id"`
	Balance        float64 `json:"balance"`
	Currency       string  `json:"currency"`
	DueDate        string  `json:"due_date"`
	DaysOverdue    int     `json:"days_overdue"`
	SuspendOn      string  `json:"suspend_on"`
	DaysToSuspend  int     `json:"days_to_suspend"`
	Automatic      bool    `json:"automatic"` // suspended through the Synergy API
	Status         string  `json:"status"`
}

// Renewal is a period starting in the next 30 days.
type Renewal struct {
	SubscriptionID string  `json:"subscription_id"`
	Number         string  `json:"number"`
	Customer       string  `json:"customer"`
	Service        string  `json:"service"`
	Start          string  `json:"start"`
	DueDate        string  `json:"due_date"`
	InvoiceDate    string  `json:"invoice_date,omitempty"`
	Amount         float64 `json:"amount"`
	Currency       string  `json:"currency"`
	AmountHome     float64 `json:"amount_home"`
	Period         string  `json:"period"`
	Invoiced       bool    `json:"invoiced"`
	Paid           bool    `json:"paid"`
}

// Dashboard is the Subscriptions overview.
type Dashboard struct {
	Home          string           `json:"home"`
	Today         string           `json:"today"`
	MRR           float64          `json:"mrr"`
	ARR           float64          `json:"arr"`
	MRRBy         []MRRLine        `json:"mrr_by_currency"`
	Active        int              `json:"active"`
	PastDue       map[string]any   `json:"past_due"` // subscriptions, invoices, amount (home)
	Suspended     int              `json:"suspended"`
	Churned       map[string]any   `json:"churned_this_month"`
	RenewalsKPI   map[string]any   `json:"renewals_30d"` // count, amount (home)
	Counts        map[string]int   `json:"counts"`
	Subscribers   int              `json:"subscribers"`
	Upcoming      []RiskItem       `json:"upcoming_suspensions"` // within 7 days unless paid
	Renewals      []Renewal        `json:"renewals"`
	Domains       []DomainDue      `json:"domains"`
	OpenTasks     int              `json:"open_tasks"`
	Unbilled      int              `json:"unbilled_sites"`
	ReadySites    int              `json:"ready_sites"`
	HasSynergy    bool             `json:"has_synergy"`
	SynergyAPI    bool             `json:"synergy_api"`
	Margin        map[string]any   `json:"margin"`
	Notes         []string         `json:"notes"`
	LastRunAt     *time.Time       `json:"last_run_at"`
	TestClock     bool             `json:"test_clock"`
	TestOffset    int              `json:"test_offset_days"`
	BillingActive bool             `json:"billing_enabled"`
	Settings      map[string]any   `json:"settings"`
	Catalog       int              `json:"catalog_services"`
	Recent        []map[string]any `json:"recent_suspensions"`
}

var siteCache = struct {
	sync.Mutex
	m map[string]siteEntry
}{m: map[string]siteEntry{}}

type siteEntry struct {
	at    time.Time
	sites []expenses.SynSite
}

// synergySites are the hosted sites from Expenses → Synergy (cached 60s).
func (s *Service) synergySites(org string) []expenses.SynSite {
	if s.SitesFn != nil {
		return s.SitesFn(org)
	}
	if s.Exp == nil || !hasTable(s.DB, "synergy_transactions") {
		return nil
	}
	siteCache.Lock()
	e, ok := siteCache.m[org]
	siteCache.Unlock()
	if ok && time.Since(e.at) < time.Minute {
		return e.sites
	}
	rep := s.Exp.SynergyReport(org, s.now(), "")
	var sites []expenses.SynSite
	if rep != nil {
		sites = rep.Sites
	}
	siteCache.Lock()
	siteCache.m[org] = siteEntry{at: time.Now(), sites: sites}
	siteCache.Unlock()
	return sites
}

func forgetSites(org string) {
	siteCache.Lock()
	delete(siteCache.m, org)
	siteCache.Unlock()
}

// MRRCounts reports whether a status counts towards MRR.
func MRRCounts(status string) bool { return status == StatusActive || status == StatusPastDue }

// Dashboard works out the overview.
func (s *Service) Dashboard(org string) Dashboard {
	home := s.home(org)
	today := s.Today(org)
	st := s.SettingsFor(org)
	d := Dashboard{Home: home, Today: DayStr(today), Counts: map[string]int{}, MRRBy: []MRRLine{}, Upcoming: []RiskItem{}, Renewals: []Renewal{},
		Notes: []string{}, LastRunAt: st.LastRunAt, TestClock: s.TestClock, TestOffset: st.TestOffsetDays, BillingActive: st.Enabled, Recent: []map[string]any{},
		Settings: map[string]any{"grace_days": st.GraceDays, "lead_monthly": st.LeadMonthly, "lead_yearly": st.LeadYearly, "admin_days": st.AdminDays,
			"client_days": st.ClientDays, "final_notice_days": st.FinalNoticeDays, "auto_suspend": st.AutoSuspend, "use_synergy_api": st.UseSynergyAPI}}
	if !s.TestClock {
		d.TestOffset = 0
	}
	subs := s.List(org, Filter{})
	by := map[string]*MRRLine{}
	customers := map[string]bool{}
	missing := 0
	revenue, cost, withCost := 0.0, 0.0, 0
	horizon := today.AddDate(0, 0, 30)
	renewHome := 0.0
	for i := range subs {
		x := &subs[i]
		d.Counts[x.Status]++
		if MRRCounts(x.Status) {
			customers[x.CustomerID] = true
			if x.MonthlyHome == nil {
				missing++
			} else {
				d.MRR += *x.MonthlyHome
				l := by[x.Currency]
				if l == nil {
					l = &MRRLine{Currency: x.Currency}
					by[x.Currency] = l
				}
				l.MRR += Monthly(x.Amount, x.Period)
				l.MRRHome += *x.MonthlyHome
				l.Count++
				if x.Cost != nil && x.Cost.Source != "none" {
					revenue += *x.MonthlyHome
					cost += x.Cost.Monthly
					withCost++
				}
			}
		}
		if x.Open != nil && x.Status != StatusCancelled && x.Status != StatusSuspended && x.Open.Sent && x.Open.DaysToSuspend != nil &&
			*x.Open.DaysToSuspend >= 0 && *x.Open.DaysToSuspend <= 7 {
			d.Upcoming = append(d.Upcoming, RiskItem{SubscriptionID: x.ID, Number: x.Number, Customer: x.CustomerName, CustomerID: x.CustomerID, Phone: x.CustomerPhone,
				Service: x.Name, Invoice: x.Open.InvoiceNumber, InvoiceID: x.Open.InvoiceID, Balance: x.Open.Balance, Currency: x.Open.Currency, DueDate: x.Open.DueDate,
				DaysOverdue: x.Open.DaysOverdue, SuspendOn: x.Open.SuspendOn, DaysToSuspend: *x.Open.DaysToSuspend, Status: x.Status})
		}
		// The next period to invoice, when it starts in the next 30 days.
		if x.Status != StatusCancelled && x.Status != StatusSuspended && x.NextDueDate != "" {
			ns := Day(x.NextPeriodStart)
			if !ns.Before(today) && !ns.After(horizon) {
				r := Renewal{SubscriptionID: x.ID, Number: x.Number, Customer: x.CustomerName, Service: x.Name, Start: DayStr(ns), DueDate: x.NextDueDate,
					InvoiceDate: x.NextInvoiceOn, Amount: x.AmountTax, Currency: x.Currency, Period: x.Period}
				if v, ok := s.convert(org, x.AmountTax, x.Currency, home); ok {
					r.AmountHome = RoundCur(v, home)
				}
				d.Renewals = append(d.Renewals, r)
			}
		}
		d.OpenTasks += x.OpenTasks
	}
	// Invoiced periods that start in the next 30 days.
	var upcoming []struct {
		SubscriptionID string
		PeriodStart    time.Time
		DueDate        time.Time
		Amount         float64
		Currency       string
		Status         string
	}
	s.DB.Raw(`SELECT subscription_id, period_start, due_date, amount, currency, status FROM client_subscription_periods WHERE organization_id = ? AND status IN ?
		AND period_start >= ? AND period_start <= ?`, org, []string{PerInvoiced, PerPaid}, today, horizon).Scan(&upcoming)
	bySub := map[string]*Subscription{}
	for i := range subs {
		bySub[subs[i].ID] = &subs[i]
	}
	for _, u := range upcoming {
		x := bySub[u.SubscriptionID]
		if x == nil || x.Status == StatusCancelled {
			continue
		}
		r := Renewal{SubscriptionID: x.ID, Number: x.Number, Customer: x.CustomerName, Service: x.Name, Start: DayStr(u.PeriodStart), DueDate: DayStr(u.DueDate),
			Amount: u.Amount, Currency: u.Currency, Period: x.Period, Invoiced: true, Paid: u.Status == PerPaid}
		if v, ok := s.convert(org, u.Amount, u.Currency, home); ok {
			r.AmountHome = RoundCur(v, home)
		}
		d.Renewals = append(d.Renewals, r)
	}
	for _, r := range d.Renewals {
		renewHome += r.AmountHome
	}
	sort.Slice(d.Renewals, func(i, j int) bool { return d.Renewals[i].Start < d.Renewals[j].Start })
	sort.Slice(d.Upcoming, func(i, j int) bool { return d.Upcoming[i].SuspendOn < d.Upcoming[j].SuspendOn })
	for i := range d.Upcoming {
		x := bySub[d.Upcoming[i].SubscriptionID]
		d.Upcoming[i].Automatic = x != nil && s.automatable(org, x)
	}
	d.RenewalsKPI = map[string]any{"count": len(d.Renewals), "amount": RoundCur(renewHome, home)}
	for _, l := range by {
		l.MRR, l.MRRHome = RoundCur(l.MRR, l.Currency), RoundCur(l.MRRHome, home)
		d.MRRBy = append(d.MRRBy, *l)
	}
	sort.Slice(d.MRRBy, func(i, j int) bool { return d.MRRBy[i].MRRHome > d.MRRBy[j].MRRHome })
	d.MRR = RoundCur(d.MRR, home)
	d.ARR = RoundCur(d.MRR*12, home)
	d.Subscribers = len(customers)
	d.Active = d.Counts[StatusActive]
	d.Suspended = d.Counts[StatusSuspended]
	if missing > 0 {
		d.Notes = append(d.Notes, "Some subscriptions are in a currency with no exchange rate to "+home+" and are left out of MRR — set a rate in Invoice settings → Exchange rates.")
	}
	// Past due: unpaid sent invoices past their due date (home currency at
	// each invoice's locked rate, as the invoicing totals do).
	// amount: on past-due subscriptions; suspended_amount: on suspended ones.
	var pds []struct {
		Status string
		N      int
		Amount float64
	}
	s.DB.Raw(`SELECT c.status, COUNT(*) AS n, COALESCE(SUM(i.balance_due / COALESCE(NULLIF(i.exchange_rate, 0), 1)), 0) AS amount
		FROM client_subscription_periods p JOIN billing_invoices i ON i.id = p.invoice_id JOIN client_subscriptions c ON c.id = p.subscription_id
		WHERE p.organization_id = ? AND p.status = ? AND p.sent AND p.due_date < ? AND i.deleted_at IS NULL GROUP BY c.status`, org, PerInvoiced, today).Scan(&pds)
	var pdN int
	var pdAmt, suspAmt float64
	for _, r := range pds {
		pdN += r.N
		if r.Status == StatusSuspended {
			suspAmt += r.Amount
		} else {
			pdAmt += r.Amount
		}
	}
	d.PastDue = map[string]any{"subscriptions": d.Counts[StatusPastDue], "invoices": pdN, "amount": RoundCur(pdAmt, home), "suspended_amount": RoundCur(suspAmt, home)}
	// Churned this month: cancelled since the 1st (the organisation's month).
	from := StartOfMonth(today)
	var churned, startN int
	s.DB.Raw(`SELECT COUNT(*) FROM client_subscriptions WHERE organization_id = ? AND status = ? AND cancelled_at >= ? AND cancelled_at <= ?`, org, StatusCancelled, from, today).Scan(&churned)
	s.DB.Raw(`SELECT COUNT(*) FROM client_subscriptions WHERE organization_id = ? AND start_date < ? AND (cancelled_at IS NULL OR cancelled_at >= ?)`, org, from, from).Scan(&startN)
	ch := map[string]any{"count": churned, "since": DayStr(from), "at_start": startN, "rate_pct": nil}
	if startN > 0 {
		ch["rate_pct"] = math.Round(float64(churned)/float64(startN)*1000) / 10
	}
	d.Churned = ch
	m := map[string]any{"revenue": RoundCur(revenue, home), "cost": RoundCur(cost, home), "margin": RoundCur(revenue-cost, home), "margin_pct": nil, "subscriptions": withCost}
	if revenue > 0 {
		m["margin_pct"] = math.Round((revenue-cost)/revenue*1000) / 10
	}
	d.Margin = m
	d.Domains = s.DomainsDue(org, subs, today, 30)
	rows := s.SynergySites(org)
	d.HasSynergy = len(rows) > 0
	for _, r := range rows {
		if r.Subscription == nil && r.Status != "cancelled" && !r.NoClient {
			d.Unbilled++
			if r.Ready {
				d.ReadySites++
			}
		}
	}
	if s.Exp != nil {
		cl, _ := s.Exp.SynergyAPI(org)
		d.SynergyAPI = cl != nil && st.UseSynergyAPI
	}
	d.Catalog = len(s.catalog(org, nil))
	// Suspensions of the last 30 days.
	var recent []struct {
		ID, Number, CustomerID, Name, SuspendedVia string
		SuspendedAt                                time.Time
	}
	s.DB.Raw(`SELECT id, number, customer_id, name, COALESCE(suspended_via,'') AS suspended_via, suspended_at FROM client_subscriptions WHERE organization_id = ? AND status = ?
		ORDER BY suspended_at DESC LIMIT 10`, org, StatusSuspended).Scan(&recent)
	for _, r := range recent {
		name := ""
		if x := bySub[r.ID]; x != nil {
			name = x.CustomerName
		}
		d.Recent = append(d.Recent, map[string]any{"subscription_id": r.ID, "number": r.Number, "customer": name, "service": r.Name, "suspended_at": DayStr(r.SuspendedAt), "via": r.SuspendedVia})
	}
	return d
}
