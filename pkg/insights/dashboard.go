package insights

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/pnl"
)

// Snapshot is the dashboard's top row (home currency).
type Snapshot struct {
	MonthStart      string    `json:"month_start"`
	YearStart       string    `json:"year_start"` // financial year start
	RevenueMTD      float64   `json:"revenue_mtd"`
	RevenuePrevMTD  float64   `json:"revenue_prev_mtd"` // same days of last month
	RevenueMTDDelta *float64  `json:"revenue_mtd_delta_pct"`
	RevenueYTD      float64   `json:"revenue_ytd"`
	RevenuePrevYTD  float64   `json:"revenue_prev_ytd"` // same days a year earlier
	RevenueYTDDelta *float64  `json:"revenue_ytd_delta_pct"`
	InvoicesMTD     int       `json:"invoices_mtd"`
	InvoicesYTD     int       `json:"invoices_ytd"`
	CashMTD         float64   `json:"cash_mtd"`
	CashYTD         float64   `json:"cash_ytd"`
	PaymentsMTD     int       `json:"payments_mtd"`
	Outstanding     float64   `json:"outstanding"`
	OutstandingN    int       `json:"outstanding_count"`
	Overdue         float64   `json:"overdue"`
	OverdueN        int       `json:"overdue_count"`
	NetProfitMTD    float64   `json:"net_profit_mtd"`
	NetProfitYTD    float64   `json:"net_profit_ytd"`
	PnLRevenueMTD   float64   `json:"pnl_revenue_mtd"`
	ExpensesMTD     float64   `json:"expenses_mtd"`
	ExpensesYTD     float64   `json:"expenses_ytd"`
	MRR             float64   `json:"mrr"`
	MRRInvoices     float64   `json:"mrr_recurring_invoices"`
	MRRHosting      float64   `json:"mrr_hosting"`
	MRRTemplates    int       `json:"mrr_templates"`
	MRRSites        int       `json:"mrr_sites"`
	MRRCost         float64   `json:"mrr_cost"` // Synergy hosting run-rate (home)
	MRRMargin       float64   `json:"mrr_margin"`
	MRRMarginPct    *float64  `json:"mrr_margin_pct"`
	ARR             float64   `json:"arr"`
	Months          []string  `json:"months"`      // last 12 months (YYYY-MM)
	Revenue12m      []float64 `json:"revenue_12m"` // revenue per month
	Cash12m         []float64 `json:"cash_12m"`    // payments received per month
}

// YearRow is one year of history.
type YearRow struct {
	Year      int      `json:"year"`
	Revenue   float64  `json:"revenue"`
	Invoices  int      `json:"invoices"`
	Clients   int      `json:"clients"`
	AvgDays   *float64 `json:"avg_days_to_pay"`
	Imported  int      `json:"imported"`
	AvgInvoic float64  `json:"avg_invoice"`
}

// History summarises the whole invoice history (shown once Zoho data exists).
type History struct {
	Imported      map[string]int `json:"imported"` // invoices, credit_notes, quotes, payments, customers from Zoho
	LastImport    string         `json:"last_import,omitempty"`
	Lifetime      float64        `json:"lifetime_revenue"`
	First         string         `json:"first_invoice"`
	Last          string         `json:"last_invoice"`
	Years         int            `json:"years_covered"`
	Invoices      int            `json:"invoices"`
	Clients       int            `json:"clients"`
	AvgInvoice    float64        `json:"avg_invoice"`
	AvgDaysToPay  *float64       `json:"avg_days_to_pay"`
	ByYear        []YearRow      `json:"by_year"`
	TopClients    []Share        `json:"top_clients"`
	DaysToPayNote string         `json:"days_to_pay_note"`
}

// HostingSummary is the dashboard hosting card.
type HostingSummary struct {
	Sites       int      `json:"sites"`
	Billed      int      `json:"billed"`
	Unbilled    int      `json:"unbilled"`
	NearFull    int      `json:"near_full"`
	Revenue     float64  `json:"revenue_month"`
	Cost        float64  `json:"cost_month"`
	Margin      float64  `json:"margin_month"`
	MarginPct   *float64 `json:"margin_pct"`
	RunRateAUD  float64  `json:"run_rate_aud"`
	BalanceAUD  *float64 `json:"balance_aud"`
	RunwayDays  *int     `json:"runway_days"`
	RunwayDate  string   `json:"runway_date,omitempty"`
	Alerts      int      `json:"alerts"`
	BelowCost   int      `json:"below_cost"`
	UnbilledAUD float64  `json:"unbilled_cost_aud"`
}

// CashWeek is one week of the 90-day forecast.
type CashWeek struct {
	Start       string  `json:"start"`
	End         string  `json:"end"`
	Receivables float64 `json:"receivables"`
	Recurring   float64 `json:"recurring_invoices"`
	Bills       float64 `json:"bills"`
	Synergy     float64 `json:"synergy"`
	In          float64 `json:"in"`
	Out         float64 `json:"out"`
	Net         float64 `json:"net"`
	Cumulative  float64 `json:"cumulative"`
}

// CashForecast is the next-90-days view.
type CashForecast struct {
	Days        int        `json:"days"`
	Weeks       []CashWeek `json:"weeks"`
	Receivables float64    `json:"receivables"`
	Recurring   float64    `json:"recurring_invoices"`
	Bills       float64    `json:"bills"`
	Synergy     float64    `json:"synergy"`
	In          float64    `json:"in"`
	Out         float64    `json:"out"`
	Net         float64    `json:"net"`
	Overdue     float64    `json:"overdue_receivables"` // not in the forecast
	OverdueBill float64    `json:"overdue_bills"`       // not in the forecast
	Notes       []string   `json:"notes"`
}

// Dashboard is GET /insights/dashboard.
type Dashboard struct {
	Currency string          `json:"currency"`
	Today    string          `json:"today"`
	Conv     *Conv           `json:"conversion"`
	Snapshot Snapshot        `json:"snapshot"`
	History  *History        `json:"history"`
	Hosting  *HostingSummary `json:"hosting"`
	Cash     *CashForecast   `json:"cashflow"`
}

func intervalMonths(iv string) float64 {
	switch iv {
	case "weekly":
		return 12.0 / 52
	case "fortnightly":
		return 12.0 / 26
	case "quarterly":
		return 3
	case "yearly":
		return 12
	}
	return 1
}

// DashboardFor builds the dashboard summary. today is the user's local date.
func (s *Service) DashboardFor(org string, today time.Time, loc *time.Location) *Dashboard {
	today = day(today)
	c := s.conv(org, "")
	home := c.Home
	_, _, fyStart, _ := pnl.OrgSettings(s.DB, org)
	ytd, _ := pnl.ResolvePeriod(today, "", "", "this_fy", fyStart)
	ms := monthStart(today)
	d := &Dashboard{Currency: home, Today: today.Format(dayFmt), Conv: c}
	sn := &d.Snapshot
	sn.MonthStart, sn.YearStart = ms.Format(dayFmt), ytd.From.Format(dayFmt)

	// Same days of last month / last year for the deltas.
	pms := ms.AddDate(0, -1, 0)
	pme := pms.AddDate(0, 0, today.Day()-1)
	if pme.Month() != pms.Month() {
		pme = ms.AddDate(0, 0, -1)
	}
	a := map[string]interface{}{"org": org, "today": today.Format(dayFmt), "ms": ms.Format(dayFmt), "ys": ytd.From.Format(dayFmt),
		"pms": pms.Format(dayFmt), "pme": pme.Format(dayFmt), "pys": ytd.From.AddDate(-1, 0, 0).Format(dayFmt), "pye": today.AddDate(-1, 0, 0).Format(dayFmt)}
	var rv struct {
		MTD, PMTD, YTD, PYTD float64
		NMTD, NYTD           int
	}
	s.scan(&rv, "WITH "+c.docsCTE()+`
SELECT COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@ms)::date AND (@today)::date), 0) AS mtd,
	COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@pms)::date AND (@pme)::date), 0) AS pmtd,
	COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@ys)::date AND (@today)::date), 0) AS ytd,
	COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@pys)::date AND (@pye)::date), 0) AS pytd,
	COUNT(*) FILTER (WHERE doc_type = 'invoice' AND d BETWEEN (@ms)::date AND (@today)::date) AS nmtd,
	COUNT(*) FILTER (WHERE doc_type = 'invoice' AND d BETWEEN (@ys)::date AND (@today)::date) AS nytd
FROM docs WHERE f IS NOT NULL`, a)
	sn.RevenueMTD, sn.RevenuePrevMTD, sn.RevenueYTD, sn.RevenuePrevYTD = r2(rv.MTD), r2(rv.PMTD), r2(rv.YTD), r2(rv.PYTD)
	sn.RevenueMTDDelta, sn.RevenueYTDDelta = pctChange(rv.MTD, rv.PMTD), pctChange(rv.YTD, rv.PYTD)
	sn.InvoicesMTD, sn.InvoicesYTD = rv.NMTD, rv.NYTD

	var cash struct {
		MTD, YTD float64
		N        int
	}
	s.scan(&cash, `SELECT COALESCE(SUM(p.amount * `+c.Factor("i")+`) FILTER (WHERE p.date BETWEEN (@ms)::date AND (@today)::date), 0) AS mtd,
		COALESCE(SUM(p.amount * `+c.Factor("i")+`) FILTER (WHERE p.date BETWEEN (@ys)::date AND (@today)::date), 0) AS ytd,
		COUNT(*) FILTER (WHERE p.date BETWEEN (@ms)::date AND (@today)::date) AS n
	FROM billing_payments p JOIN billing_invoices i ON i.id = p.invoice_id AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status <> 'void'
	WHERE p.organization_id = @org AND p.date BETWEEN LEAST((@ms)::date, (@ys)::date) AND (@today)::date`, a)
	sn.CashMTD, sn.CashYTD, sn.PaymentsMTD = r2(cash.MTD), r2(cash.YTD), cash.N

	var ar struct {
		Out, Over float64
		N, NO     int
	}
	s.scan(&ar, `SELECT COALESCE(SUM(i.balance_due * `+c.Factor("i")+`), 0) AS out, COUNT(*) AS n,
		COALESCE(SUM(i.balance_due * `+c.Factor("i")+`) FILTER (WHERE i.due_date::date < (@today)::date), 0) AS over,
		COUNT(*) FILTER (WHERE i.due_date::date < (@today)::date) AS no
	FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status IN ('sent','partial')
		AND `+c.Factor("i")+` IS NOT NULL`, a)
	sn.Outstanding, sn.OutstandingN, sn.Overdue, sn.OverdueN = r2(ar.Out), ar.N, r2(ar.Over), ar.NO

	// Profit & loss (the shared pkg/pnl calculation, accrual basis).
	in := pnl.Load(s.DB, org, ytd.From, today, loc)
	in.Home, in.OrgHome, in.Decimals = home, home, currency.Decimals(home)
	m := pnl.Compute(in, pnl.Period{From: ms, To: today}, pnl.Accrual).Totals()
	y := pnl.Compute(in, ytd, pnl.Accrual).Totals()
	mj := func(v int64) float64 { return r2(float64(v) / math.Pow10(in.Decimals)) }
	sn.NetProfitMTD, sn.NetProfitYTD, sn.PnLRevenueMTD = mj(m.NetProfit), mj(y.NetProfit), mj(m.Revenue)
	sn.ExpensesMTD, sn.ExpensesYTD = mj(m.TotalExpenses), mj(y.TotalExpenses)

	// MRR: active recurring invoice schedules + hosting billed outside them.
	var recs []struct {
		Interval string
		Net      float64
		Number   string
	}
	s.scan(&recs, `SELECT COALESCE(i.recurrence_interval,'monthly') AS interval, (i.total - i.tax_total) * `+c.Factor("i")+` AS net, i.number
	FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.is_recurring AND i.status <> 'void'
		AND (i.recurrence_end_date IS NULL OR i.recurrence_end_date >= (@today)::date) AND `+c.Factor("i")+` IS NOT NULL`, a)
	for _, r := range recs {
		sn.MRRInvoices += r.Net / intervalMonths(r.Interval)
		sn.MRRTemplates++
	}
	var recNums []string
	s.DB.Raw(`SELECT number FROM billing_invoices WHERE organization_id = ? AND deleted_at IS NULL AND (is_recurring OR recurrence_parent_id IS NOT NULL)`, org).Scan(&recNums)
	inRec := map[string]bool{}
	for _, n := range recNums {
		inRec[n] = true
	}
	rep := s.synergy(org)
	if rep != nil {
		for _, st := range rep.Sites {
			if st.Status == "cancelled" || st.Billed == nil || *st.Billed <= 0 {
				continue
			}
			if st.BilledSource == "invoice" && inRec[st.BilledInvoice] {
				continue // already counted with its recurring invoice
			}
			sn.MRRHosting += *st.Billed
			sn.MRRSites++
		}
		sn.MRRCost = r2(rep.KPIs.RunRateHome)
	}
	sn.MRRInvoices, sn.MRRHosting = r2(sn.MRRInvoices), r2(sn.MRRHosting)
	sn.MRR = r2(sn.MRRInvoices + sn.MRRHosting)
	sn.ARR = r2(sn.MRR * 12)
	sn.MRRMargin = r2(sn.MRR - sn.MRRCost)
	if sn.MRR > 0 {
		sn.MRRMarginPct = fptr(r1(sn.MRRMargin / sn.MRR * 100))
	}

	var ser []struct {
		M        string
		Rev, Cas float64
	}
	s.scan(&ser, "WITH "+c.docsCTE()+`,
m AS (SELECT gs::date AS m FROM generate_series(date_trunc('month', (@today)::date) - interval '11 months', date_trunc('month', (@today)::date), interval '1 month') gs),
r AS (SELECT date_trunc('month', d)::date AS m, SUM(sgn * f * (total - tax_total)) AS rev FROM docs WHERE f IS NOT NULL AND d <= (@today)::date GROUP BY 1),
p AS (SELECT date_trunc('month', p.date)::date AS m, SUM(p.amount * `+c.Factor("i")+`) AS cas FROM billing_payments p
	JOIN billing_invoices i ON i.id = p.invoice_id AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status <> 'void'
	WHERE p.organization_id = @org AND p.date <= (@today)::date GROUP BY 1)
SELECT to_char(m.m, 'YYYY-MM') AS m, COALESCE(r.rev, 0) AS rev, COALESCE(p.cas, 0) AS cas FROM m LEFT JOIN r ON r.m = m.m LEFT JOIN p ON p.m = m.m ORDER BY m.m`, a)
	sn.Months, sn.Revenue12m, sn.Cash12m = []string{}, []float64{}, []float64{}
	for _, x := range ser {
		sn.Months = append(sn.Months, x.M)
		sn.Revenue12m = append(sn.Revenue12m, r2(x.Rev))
		sn.Cash12m = append(sn.Cash12m, r2(x.Cas))
	}

	d.History = s.history(org, c)
	if rep != nil {
		h := &HostingSummary{RunRateAUD: r2(rep.KPIs.RunRate), BalanceAUD: rep.KPIs.Balance, RunwayDays: rep.KPIs.RunwayDays, RunwayDate: rep.KPIs.RunwayDate,
			Alerts: len(rep.Alerts), UnbilledAUD: r2(rep.KPIs.UnbilledCost)}
		for _, st := range rep.Sites {
			if st.Status == "cancelled" {
				continue
			}
			h.Sites++
			h.Cost += st.CostHome
			if hasFlag(st.Flags, "near_full") || hasFlag(st.Flags, "over_quota") {
				h.NearFull++
			}
			if hasFlag(st.Flags, "below_cost") {
				h.BelowCost++
			}
			if st.Billed != nil && *st.Billed > 0 {
				h.Billed++
				h.Revenue += *st.Billed
			} else if !st.NoClient {
				h.Unbilled++
			}
		}
		h.Revenue, h.Cost = r2(h.Revenue), r2(h.Cost)
		h.Margin = r2(h.Revenue - h.Cost)
		if h.Revenue > 0 {
			h.MarginPct = fptr(r1(h.Margin / h.Revenue * 100))
		}
		d.Hosting = h
	}
	d.Cash = s.cashForecast(org, c, today, 90, rep != nil, func() (float64, string) {
		if rep == nil {
			return 0, ""
		}
		return rep.KPIs.RunRateHome, rep.KPIs.NextChargeDate
	})
	return d
}

func (s *Service) history(org string, c *Conv) *History {
	var imp []struct {
		DocType string
		N       int
	}
	s.scan(&imp, `SELECT doc_type, COUNT(*) AS n FROM billing_invoices WHERE organization_id = ? AND deleted_at IS NULL AND source = 'zoho' GROUP BY doc_type`, org)
	if len(imp) == 0 {
		return nil
	}
	h := &History{Imported: map[string]int{}, ByYear: []YearRow{}, TopClients: []Share{}}
	for _, r := range imp {
		key := map[string]string{"invoice": "invoices", "quote": "quotes", "credit_note": "credit_notes"}[r.DocType]
		if key == "" {
			key = r.DocType
		}
		h.Imported[key] = r.N
	}
	var n int
	s.DB.Raw(`SELECT COUNT(*) FROM billing_payments WHERE organization_id = ? AND source = 'zoho'`, org).Scan(&n)
	h.Imported["payments"] = n
	n = 0
	s.DB.Raw(`SELECT COUNT(*) FROM customers WHERE organization_id = ? AND deleted_at IS NULL AND source = 'zoho'`, org).Scan(&n)
	h.Imported["customers"] = n
	var last *time.Time
	s.DB.Raw(`SELECT MAX(updated_at) FROM billing_invoices WHERE organization_id = ? AND source = 'zoho'`, org).Row().Scan(&last)
	if last != nil {
		h.LastImport = last.Format(dayFmt)
	}
	a := map[string]interface{}{"org": org}
	var ys []struct {
		Y, Inv, Clients, Imported int
		Rev, InvTotal             float64
	}
	s.scan(&ys, "WITH "+c.docsCTE()+`
SELECT EXTRACT(YEAR FROM d)::int AS y, SUM(sgn * f * (total - tax_total)) AS rev,
	COUNT(*) FILTER (WHERE doc_type = 'invoice') AS inv, COUNT(DISTINCT ck) AS clients,
	COUNT(*) FILTER (WHERE source = 'zoho') AS imported,
	COALESCE(SUM(f * (total - tax_total)) FILTER (WHERE doc_type = 'invoice'), 0) AS inv_total
FROM docs WHERE f IS NOT NULL GROUP BY 1 ORDER BY 1`, a)
	var dtp []struct {
		Y   int
		Avg float64
	}
	s.scan(&dtp, "WITH "+payCTE(c)+`
SELECT EXTRACT(YEAR FROM d)::int AS y, SUM(dtp * amt_home) / NULLIF(SUM(amt_home), 0) AS avg FROM settled GROUP BY 1`, a)
	dm := map[int]float64{}
	for _, x := range dtp {
		dm[x.Y] = x.Avg
	}
	var invTotal float64
	for _, r := range ys {
		yr := YearRow{Year: r.Y, Revenue: r2(r.Rev), Invoices: r.Inv, Clients: r.Clients, Imported: r.Imported}
		if r.Inv > 0 {
			yr.AvgInvoic = r2(r.InvTotal / float64(r.Inv))
		}
		if v, ok := dm[r.Y]; ok {
			yr.AvgDays = fptr(r1(v))
		}
		h.ByYear = append(h.ByYear, yr)
		h.Lifetime += r.Rev
		h.Invoices += r.Inv
		invTotal += r.InvTotal
	}
	h.Lifetime = r2(h.Lifetime)
	if h.Invoices > 0 {
		h.AvgInvoice = r2(invTotal / float64(h.Invoices))
	}
	var span struct {
		First, Last string
		Clients     int
	}
	s.scan(&span, "WITH "+c.docsCTE()+`
SELECT to_char(MIN(d), 'YYYY-MM-DD') AS first, to_char(MAX(d), 'YYYY-MM-DD') AS last, COUNT(DISTINCT ck) AS clients FROM docs WHERE doc_type = 'invoice'`, a)
	h.First, h.Last, h.Clients = span.First, span.Last, span.Clients
	if f, err := time.Parse(dayFmt, span.First); err == nil {
		if l, err := time.Parse(dayFmt, span.Last); err == nil {
			h.Years = l.Year() - f.Year() + 1
		}
	}
	var avg *float64
	s.DB.Raw("WITH "+payCTE(c)+` SELECT SUM(dtp * amt_home) / NULLIF(SUM(amt_home), 0) FROM settled`, a).Row().Scan(&avg)
	if avg != nil {
		h.AvgDaysToPay = fptr(r1(*avg))
	}
	h.DaysToPayNote = "Days to pay = last payment date − issue date of fully paid invoices, weighted by amount."
	var top []struct {
		CK  string
		Rev float64
		N   int
	}
	s.scan(&top, "WITH "+c.docsCTE()+`
SELECT ck, SUM(sgn * f * (total - tax_total)) AS rev, COUNT(*) FILTER (WHERE doc_type = 'invoice') AS n
FROM docs WHERE f IS NOT NULL GROUP BY ck ORDER BY rev DESC, ck LIMIT 5`, a)
	names := s.clientNames(org)
	for i, t := range top {
		sh := Share{Key: t.CK, Name: nameOf(names, t.CK), CustomerID: customerID(t.CK), Revenue: r2(t.Rev), Docs: t.N, Rank: i + 1}
		if h.Lifetime != 0 {
			sh.SharePct = r1(t.Rev / h.Lifetime * 100)
		}
		h.TopClients = append(h.TopClients, sh)
	}
	return h
}

// cashForecast projects money in and out over the next `days` days.
func (s *Service) cashForecast(org string, c *Conv, today time.Time, days int, hasSynergy bool, syn func() (float64, string)) *CashForecast {
	end := today.AddDate(0, 0, days-1)
	f := &CashForecast{Days: days, Weeks: []CashWeek{}, Notes: []string{}}
	nw := (days + 6) / 7
	for i := 0; i < nw; i++ {
		ws := today.AddDate(0, 0, i*7)
		we := ws.AddDate(0, 0, 6)
		if we.After(end) {
			we = end
		}
		f.Weeks = append(f.Weeks, CashWeek{Start: ws.Format(dayFmt), End: we.Format(dayFmt)})
	}
	idx := func(t time.Time) int {
		t = day(t)
		if t.Before(today) || t.After(end) {
			return -1
		}
		return int(t.Sub(today).Hours()/24) / 7
	}
	a := map[string]interface{}{"org": org, "today": today.Format(dayFmt), "end": end.Format(dayFmt)}

	// 1. Open invoices by due date (tax included: it is cash).
	var open []struct {
		Due time.Time
		Amt float64
	}
	s.scan(&open, `SELECT i.due_date::date AS due, i.balance_due * `+c.Factor("i")+` AS amt
	FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status IN ('sent','partial')
		AND i.balance_due > 0 AND i.due_date::date <= (@end)::date AND `+c.Factor("i")+` IS NOT NULL`, a)
	for _, o := range open {
		if day(o.Due).Before(today) {
			f.Overdue += o.Amt
			continue
		}
		if k := idx(o.Due); k >= 0 {
			f.Weeks[k].Receivables += o.Amt
		}
	}
	// 2. Recurring invoices: the copies the schedules will create.
	var recs []struct {
		Interval    string
		Next        *time.Time
		EndDate     *time.Time
		Issue, Due  time.Time
		Total, Fact float64
	}
	s.scan(&recs, `SELECT COALESCE(i.recurrence_interval,'monthly') AS interval, i.next_recurrence_date AS next, i.recurrence_end_date AS end_date,
		i.issue_date AS issue, i.due_date AS due, i.total, `+c.Factor("i")+` AS fact
	FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.is_recurring AND i.status <> 'void'
		AND i.next_recurrence_date IS NOT NULL AND `+c.Factor("i")+` IS NOT NULL`, a)
	for _, r := range recs {
		terms := int(day(r.Due).Sub(day(r.Issue)).Hours() / 24)
		if terms < 0 {
			terms = 0
		}
		occ := day(*r.Next)
		for guard := 0; guard < 400 && !occ.After(end); guard++ {
			if r.EndDate != nil && occ.After(day(*r.EndDate)) {
				break
			}
			if k := idx(occ.AddDate(0, 0, terms)); k >= 0 {
				f.Weeks[k].Recurring += r.Total * r.Fact
			}
			occ = invoicing.NextDate(occ, r.Interval)
		}
	}
	// 3. Supplier bills: open bills and projected recurring bills (pkg/recurbills).
	synInBills := false
	if s.Bills != nil {
		up := s.Bills.UpcomingFor(org, days)
		for _, it := range up.Items {
			if it.Status == "paid" || it.Status == "skipped" || it.Amount <= 0 {
				continue
			}
			if it.SynergyDomain != "" || strings.Contains(strings.ToLower(it.Vendor+" "+it.Name), "synergy") {
				synInBills = true
			}
			due, err := time.Parse(dayFmt, it.DueDate)
			if err != nil {
				continue
			}
			if day(due).Before(today) {
				f.OverdueBill += it.Amount
				continue
			}
			if k := idx(due); k >= 0 {
				f.Weeks[k].Bills += it.Amount
			}
		}
	}
	// 4. Synergy Wholesale run-rate (monthly), unless recurring bills already cover it.
	if hasSynergy && !synInBills {
		rate, next := syn()
		if rate > 0 {
			at := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
			if t, err := time.Parse(dayFmt, next); err == nil {
				at = day(t)
			}
			for at.Before(today) {
				at = at.AddDate(0, 1, 0)
			}
			for ; !at.After(end); at = at.AddDate(0, 1, 0) {
				if k := idx(at); k >= 0 {
					f.Weeks[k].Synergy += rate
				}
			}
			f.Notes = append(f.Notes, fmt.Sprintf("Synergy Wholesale hosting at the current run-rate (%s %s a month), on each monthly charge date.", c.Home, amt(rate)))
		}
	} else if synInBills {
		f.Notes = append(f.Notes, "Synergy Wholesale is included through your recurring bills (not counted twice).")
	}
	var cum float64
	for i := range f.Weeks {
		w := &f.Weeks[i]
		w.Receivables, w.Recurring, w.Bills, w.Synergy = r2(w.Receivables), r2(w.Recurring), r2(w.Bills), r2(w.Synergy)
		w.In = r2(w.Receivables + w.Recurring)
		w.Out = r2(w.Bills + w.Synergy)
		w.Net = r2(w.In - w.Out)
		cum += w.Net
		w.Cumulative = r2(cum)
		f.Receivables += w.Receivables
		f.Recurring += w.Recurring
		f.Bills += w.Bills
		f.Synergy += w.Synergy
	}
	f.Receivables, f.Recurring, f.Bills, f.Synergy = r2(f.Receivables), r2(f.Recurring), r2(f.Bills), r2(f.Synergy)
	f.In, f.Out = r2(f.Receivables+f.Recurring), r2(f.Bills+f.Synergy)
	f.Net = r2(f.In - f.Out)
	f.Overdue, f.OverdueBill = r2(f.Overdue), r2(f.OverdueBill)
	f.Notes = append(f.Notes, "Money in: open invoices by due date and the invoices your recurring schedules will create (incl. tax). Money out: open and projected supplier bills. Overdue amounts are shown separately and not assumed.")
	return f
}
