package insights

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Trade & exports: where revenue comes from (client country, currency), what
// foreign-currency money is owed and what it is worth, what was won or lost on
// exchange rates, and how far foreign costs offset foreign income.
//
// Conventions
//
//   - Revenue is invoices minus credit notes, excluding tax, by issue date, in
//     the home currency at the rate locked on each document (the same docs CTE
//     as every other Insights figure).
//   - A document's country is its client's country (customers.country_code).
//     Without one: a document in a foreign currency is filed under that
//     currency's country ("from the currency"), one in the home currency is
//     assumed domestic. Both are counted and flagged.
//   - Export = any country other than the organisation's own.
//   - exchange_rate on a document is units of its currency per 1 home unit, so
//     its home value is amount ÷ exchange_rate.
//
// Formulas (also returned by the API and shown on the page)
//
//	unrealised FX = open balance × today's rate − open balance ÷ locked rate
//	realised FX   = payment × rate of the payment month − payment ÷ locked rate
//	natural hedge = costs in a currency ÷ revenue in that currency
const (
	FormulaUnrealised = "Unrealised FX gain / loss = open balance × today's rate − open balance ÷ the rate locked on the invoice (both in the home currency). Positive: the invoice currency has strengthened since you invoiced, so the money is worth more today."
	FormulaRealised   = "Realised FX gain / loss = payment × the rate of the month it was received − payment ÷ the rate locked on the invoice. Only payments where both rates are known are counted; month rates are the ones recorded under Expenses → exchange rates."
	FormulaHedge      = "Natural hedge = what you spend in a currency ÷ what you earn in it. The uncovered part (earned − spent) is your net exposure: its home value moves 1:1 with the exchange rate."
	FormulaCountry    = "Country = the client's country. Without one, a foreign-currency document is filed under that currency's country and a home-currency document is assumed domestic (both are flagged)."
)

// UnknownForeign is the pseudo country of foreign-currency documents whose
// country cannot be told.
const UnknownForeign = "ZZ"

// TradeTotals is the headline split.
type TradeTotals struct {
	Revenue        float64  `json:"revenue"`
	Domestic       float64  `json:"domestic"`
	Export         float64  `json:"export"`
	ExportPct      *float64 `json:"export_pct"`
	Docs           int      `json:"docs"`
	ExportDocs     int      `json:"export_docs"`
	ExportClients  int      `json:"export_clients"`
	ExportMarkets  int      `json:"export_markets"`
	PrevRevenue    float64  `json:"prev_revenue"` // same period a year earlier
	PrevExport     float64  `json:"prev_export"`
	PrevExportPct  *float64 `json:"prev_export_pct"`
	ExportYoY      *float64 `json:"export_yoy_pct"`
	InferredDocs   int      `json:"inferred_docs"`   // country taken from the currency
	AssumedDocs    int      `json:"assumed_docs"`    // no country: assumed domestic
	AssumedClients int      `json:"assumed_clients"` // clients without a country
	Excluded       int      `json:"excluded_docs"`   // no exchange rate: left out
}

// TradeMonth is one month of the domestic / export trend.
type TradeMonth struct {
	Month     string   `json:"month"`
	Domestic  float64  `json:"domestic"`
	Export    float64  `json:"export"`
	ExportPct *float64 `json:"export_pct"`
}

// TradeCountry is revenue from one client country.
type TradeCountry struct {
	Country      string   `json:"country"`
	Name         string   `json:"name"`
	Domestic     bool     `json:"domestic"`
	Revenue      float64  `json:"revenue"`
	SharePct     float64  `json:"share_pct"`
	Prev         float64  `json:"prev"`
	YoY          *float64 `json:"yoy_pct"`
	Invoices     int      `json:"invoices"`
	Clients      int      `json:"clients"`
	AvgInvoice   float64  `json:"avg_invoice"`
	AvgDaysToPay *float64 `json:"avg_days_to_pay"`
	PaidInvoices int      `json:"paid_invoices"`
	Inferred     int      `json:"inferred_docs"`
}

// TradeCurrency is revenue invoiced in one currency.
type TradeCurrency struct {
	Currency string  `json:"currency"`
	Amount   float64 `json:"amount"` // in that currency
	Home     float64 `json:"home"`
	SharePct float64 `json:"share_pct"`
	Docs     int     `json:"docs"`
	Foreign  bool    `json:"foreign"`
}

// TradeClient is one export client.
type TradeClient struct {
	Name       string  `json:"name"`
	CustomerID string  `json:"customer_id,omitempty"`
	Country    string  `json:"country"`
	CountryNm  string  `json:"country_name"`
	Currencies string  `json:"currencies"`
	Revenue    float64 `json:"revenue"`
	SharePct   float64 `json:"share_pct"` // of export revenue
	Invoices   int     `json:"invoices"`
}

// FXRecvRow is the open receivables in one foreign currency.
type FXRecvRow struct {
	Currency      string   `json:"currency"`
	Invoices      int      `json:"invoices"`
	Balance       float64  `json:"balance"`        // in that currency
	LockedBalance float64  `json:"locked_balance"` // part with a locked rate
	LockedHome    float64  `json:"locked_home"`    // at the rates locked on the invoices
	TodayRate     float64  `json:"today_rate"`     // home per 1 unit (0 = none)
	TodayHome     *float64 `json:"today_home"`     // locked part at today's rate
	Unrealised    *float64 `json:"unrealised"`
	UnrealisedPct *float64 `json:"unrealised_pct"`
	AvgLockedRate *float64 `json:"avg_locked_rate"` // home per 1 unit, balance-weighted
	NoLocked      int      `json:"no_locked_rate"`  // invoices without a locked rate (not in the gain / loss)
	Overdue       float64  `json:"overdue"`         // in that currency
	RateSource    string   `json:"rate_source"`
	RateProvider  string   `json:"rate_provider"`
}

// FXReceivables is the open foreign-currency position.
type FXReceivables struct {
	Rows        []FXRecvRow `json:"rows"`
	LockedHome  float64     `json:"locked_home"`
	TodayHome   float64     `json:"today_home"`
	Unrealised  float64     `json:"unrealised"`
	Invoices    int         `json:"invoices"`
	HomeBalance float64     `json:"home_balance"` // open receivables in the home currency, for scale
	Formula     string      `json:"formula"`
}

// FXRealRow is the realised gain / loss in one currency.
type FXRealRow struct {
	Currency  string  `json:"currency"`
	Payments  int     `json:"payments"`
	Amount    float64 `json:"amount"`
	AtInvoice float64 `json:"at_invoice_rate"`
	AtPayment float64 `json:"at_payment_rate"`
	Gain      float64 `json:"gain"`
	Skipped   int     `json:"skipped"` // payments without a month rate
}

// FXRealised is the realised FX result on paid foreign invoices.
type FXRealised struct {
	Rows    []FXRealRow `json:"rows"`
	Gain    float64     `json:"gain"`
	Counted int         `json:"counted"`
	Skipped int         `json:"skipped"`
	Formula string      `json:"formula"`
	Note    string      `json:"note"`
}

// FXVendor is one supplier paid in a foreign currency.
type FXVendor struct {
	Name   string  `json:"name"`
	Amount float64 `json:"amount"`
	Home   float64 `json:"home"`
	Items  int     `json:"items"`
}

// FXCost is the approved costs in one foreign currency.
type FXCost struct {
	Currency string     `json:"currency"`
	Amount   float64    `json:"amount"`
	Home     float64    `json:"home"`
	Items    int        `json:"items"`
	Vendors  []FXVendor `json:"vendors"`
}

// HedgeRow compares money in and out in one foreign currency.
type HedgeRow struct {
	Currency  string   `json:"currency"`
	In        float64  `json:"in"`
	Out       float64  `json:"out"`
	Net       float64  `json:"net"`
	CoverPct  *float64 `json:"cover_pct"` // out ÷ in
	Rate      float64  `json:"rate"`      // today's rate, home per 1 unit
	NetHome   *float64 `json:"net_home"`
	Move5Home *float64 `json:"move_5pct_home"` // what a 5% rate move changes
	Summary   string   `json:"summary"`
}

// Trade is GET /insights/trade.
type Trade struct {
	Currency    string            `json:"currency"`
	HomeCountry string            `json:"home_country"`
	HomeName    string            `json:"home_country_name"`
	From        string            `json:"from"`
	To          string            `json:"to"`
	Today       string            `json:"today"`
	Conv        *Conv             `json:"conversion"`
	Totals      TradeTotals       `json:"totals"`
	Months      []TradeMonth      `json:"months"`
	Countries   []TradeCountry    `json:"countries"`
	Currencies  []TradeCurrency   `json:"currencies"`
	TopExport   []TradeClient     `json:"top_export_clients"`
	Receivables *FXReceivables    `json:"receivables"`
	Realised    *FXRealised       `json:"realised"`
	Costs       []FXCost          `json:"costs"`
	Hedge       []HedgeRow        `json:"hedge"`
	Notes       []string          `json:"notes"`
	Formulas    map[string]string `json:"formulas"`
}

// homeCountry is the organisation's country (falls back to its currency's).
func (s *Service) homeCountry(org, homeCur string) string {
	cc := strings.ToUpper(strings.TrimSpace(orgprefs.Country(s.DB, org)))
	if len(cc) == 2 {
		return cc
	}
	if cc := currencyHome[homeCur]; cc != "" {
		return cc
	}
	return strings.ToUpper(orgprefs.CountryForCurrency(homeCur))
}

// tradeCTE adds the country of every counted document to the docs CTE.
func tradeCTE(c *Conv, homeCountry string) string {
	curs := make([]string, 0, len(currencyHome))
	for k := range currencyHome {
		curs = append(curs, k)
	}
	sort.Strings(curs)
	b := &strings.Builder{}
	b.WriteString("CASE d.cur")
	for _, k := range curs {
		fmt.Fprintf(b, " WHEN '%s' THEN '%s'", k, currencyHome[k])
	}
	fmt.Fprintf(b, " ELSE '%s' END", UnknownForeign)
	hc := homeCountry
	if !reCountry.MatchString(hc) {
		hc = ""
	}
	return c.docsCTE() + `,
td AS (
	SELECT d.*,
		CASE WHEN COALESCE(TRIM(cu.country_code),'') <> '' THEN UPPER(TRIM(cu.country_code))
			WHEN d.cur <> '` + c.Home + `' THEN ` + b.String() + `
			ELSE '` + hc + `' END AS country,
		CASE WHEN COALESCE(TRIM(cu.country_code),'') <> '' THEN 'client' WHEN d.cur <> '` + c.Home + `' THEN 'currency' ELSE 'assumed' END AS basis
	FROM docs d LEFT JOIN customers cu ON cu.id = d.customer_id AND cu.organization_id = @org
)`
}

const tradeNet = "sgn * f * (total - tax_total)"

// TradeFor builds the trade & exports view for a date range.
func (s *Service) TradeFor(org string, o Opts) *Trade {
	c := s.conv(org, "")
	home := c.Home
	hc := s.homeCountry(org, home)
	today := o.Today
	if today.IsZero() {
		today = day(s.now())
	}
	a := o.args(org)
	a["home"], a["hc"], a["today"] = home, hc, today.Format(dayFmt)
	t := &Trade{Currency: home, HomeCountry: hc, HomeName: CountryName(hc), From: o.Range.From.Format(dayFmt), To: o.Range.To.Format(dayFmt), Today: today.Format(dayFmt), Conv: c,
		Months: []TradeMonth{}, Countries: []TradeCountry{}, Currencies: []TradeCurrency{}, TopExport: []TradeClient{}, Costs: []FXCost{}, Hedge: []HedgeRow{}, Notes: []string{},
		Formulas: map[string]string{"unrealised": FormulaUnrealised, "realised": FormulaRealised, "hedge": FormulaHedge, "country": FormulaCountry}}
	cte := "WITH " + tradeCTE(c, hc)

	// ---- by country (with the same period a year earlier) ----
	var cr []struct {
		Country                    string
		Rev, Prev, InvTotal        float64
		Inv, Clients, Inferred, As int
	}
	s.scan(&cr, cte+`
SELECT country,
	COALESCE(SUM(`+tradeNet+`) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date), 0) AS rev,
	COALESCE(SUM(`+tradeNet+`) FILTER (WHERE d BETWEEN (@pfrom)::date AND (@pto)::date), 0) AS prev,
	COALESCE(SUM(f * (total - tax_total)) FILTER (WHERE doc_type = 'invoice' AND d BETWEEN (@from)::date AND (@to)::date), 0) AS inv_total,
	COUNT(*) FILTER (WHERE doc_type = 'invoice' AND d BETWEEN (@from)::date AND (@to)::date) AS inv,
	COUNT(DISTINCT ck) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date) AS clients,
	COUNT(*) FILTER (WHERE basis = 'currency' AND d BETWEEN (@from)::date AND (@to)::date) AS inferred,
	COUNT(*) FILTER (WHERE basis = 'assumed' AND d BETWEEN (@from)::date AND (@to)::date) AS "as"
FROM td WHERE f IS NOT NULL AND (d BETWEEN (@from)::date AND (@to)::date OR d BETWEEN (@pfrom)::date AND (@pto)::date)
GROUP BY country`, a)
	// Days to pay: fully paid invoices issued in the range, amount-weighted.
	var dp []struct {
		Country string
		Avg     *float64
		N       int
	}
	s.scan(&dp, cte+`,
pay AS (SELECT p.invoice_id, MAX(p.date)::date AS last_pay FROM billing_payments p WHERE p.organization_id = @org GROUP BY p.invoice_id)
SELECT td.country, SUM((COALESCE(pay.last_pay, td.paid_at::date, td.due) - td.d) * td.total * td.f) / NULLIF(SUM(td.total * td.f), 0) AS avg, COUNT(*) AS n
FROM td LEFT JOIN pay ON pay.invoice_id = td.id
WHERE td.doc_type = 'invoice' AND td.status = 'paid' AND td.total > 0 AND td.f IS NOT NULL AND td.d BETWEEN (@from)::date AND (@to)::date
GROUP BY td.country`, a)
	dtp := map[string]struct {
		avg *float64
		n   int
	}{}
	for _, r := range dp {
		dtp[r.Country] = struct {
			avg *float64
			n   int
		}{r.Avg, r.N}
	}
	tt := &t.Totals
	for _, r := range cr {
		dom := r.Country == hc
		tt.Revenue += r.Rev
		tt.PrevRevenue += r.Prev
		tt.Docs += r.Inv
		tt.InferredDocs += r.Inferred
		tt.AssumedDocs += r.As
		if dom {
			tt.Domestic += r.Rev
		} else {
			tt.Export += r.Rev
			tt.PrevExport += r.Prev
			tt.ExportDocs += r.Inv
			if r.Inv > 0 || r.Rev != 0 {
				tt.ExportMarkets++
			}
		}
		if r.Inv == 0 && r.Rev == 0 {
			continue // only present a year earlier
		}
		row := TradeCountry{Country: r.Country, Name: countryLabel(r.Country), Domestic: dom, Revenue: r2(r.Rev), Prev: r2(r.Prev), YoY: pctChange(r.Rev, r.Prev),
			Invoices: r.Inv, Clients: r.Clients, Inferred: r.Inferred + r.As}
		if r.Inv > 0 {
			row.AvgInvoice = r2(r.InvTotal / float64(r.Inv))
		}
		if d, ok := dtp[r.Country]; ok && d.avg != nil {
			row.AvgDaysToPay, row.PaidInvoices = fptr(r1(*d.avg)), d.n
		}
		t.Countries = append(t.Countries, row)
	}
	for k := range t.Countries {
		if tt.Revenue != 0 {
			t.Countries[k].SharePct = r1(t.Countries[k].Revenue / tt.Revenue * 100)
		}
	}
	sort.SliceStable(t.Countries, func(x, y int) bool {
		if t.Countries[x].Revenue != t.Countries[y].Revenue {
			return t.Countries[x].Revenue > t.Countries[y].Revenue
		}
		return t.Countries[x].Country < t.Countries[y].Country
	})
	if tt.Revenue != 0 {
		tt.ExportPct = fptr(r1(tt.Export / tt.Revenue * 100))
	}
	if tt.PrevRevenue != 0 {
		tt.PrevExportPct = fptr(r1(tt.PrevExport / tt.PrevRevenue * 100))
	}
	tt.ExportYoY = pctChange(tt.Export, tt.PrevExport)
	tt.Revenue, tt.Domestic, tt.Export, tt.PrevRevenue, tt.PrevExport = r2(tt.Revenue), r2(tt.Domestic), r2(tt.Export), r2(tt.PrevRevenue), r2(tt.PrevExport)
	s.DB.Raw(cte+` SELECT COUNT(DISTINCT ck) FROM td WHERE f IS NOT NULL AND country <> @hc AND d BETWEEN (@from)::date AND (@to)::date`, a).Row().Scan(&tt.ExportClients)
	s.DB.Raw(cte+` SELECT COUNT(DISTINCT ck) FROM td WHERE basis <> 'client' AND d BETWEEN (@from)::date AND (@to)::date`, a).Row().Scan(&tt.AssumedClients)
	s.DB.Raw(cte+` SELECT COUNT(*) FROM td WHERE f IS NULL AND d BETWEEN (@from)::date AND (@to)::date`, a).Row().Scan(&tt.Excluded)

	// ---- monthly trend (at most 60 months back from the end) ----
	var ms []struct {
		M        string
		Dom, Exp float64
	}
	s.scan(&ms, cte+`,
m AS (SELECT gs::date AS m FROM generate_series(GREATEST(date_trunc('month', (@from)::date), date_trunc('month', (@to)::date) - interval '59 months'), date_trunc('month', (@to)::date), interval '1 month') gs),
r AS (SELECT date_trunc('month', d)::date AS m,
		COALESCE(SUM(`+tradeNet+`) FILTER (WHERE country = @hc), 0) AS dom,
		COALESCE(SUM(`+tradeNet+`) FILTER (WHERE country <> @hc), 0) AS exp
	FROM td WHERE f IS NOT NULL AND d BETWEEN (@from)::date AND (@to)::date GROUP BY 1)
SELECT to_char(m.m, 'YYYY-MM') AS m, COALESCE(r.dom, 0) AS dom, COALESCE(r.exp, 0) AS exp FROM m LEFT JOIN r ON r.m = m.m ORDER BY m.m`, a)
	for _, r := range ms {
		tm := TradeMonth{Month: r.M, Domestic: r2(r.Dom), Export: r2(r.Exp)}
		if tot := r.Dom + r.Exp; tot != 0 {
			tm.ExportPct = fptr(r1(r.Exp / tot * 100))
		}
		t.Months = append(t.Months, tm)
	}

	// ---- by currency ----
	var cu []struct {
		Cur       string
		Amt, Home float64
		N         int
	}
	s.scan(&cu, cte+`
SELECT cur, COALESCE(SUM(sgn * (total - tax_total)), 0) AS amt, COALESCE(SUM(`+tradeNet+`), 0) AS home, COUNT(*) AS n
FROM td WHERE f IS NOT NULL AND d BETWEEN (@from)::date AND (@to)::date GROUP BY cur ORDER BY 3 DESC, 1`, a)
	revIn := map[string]float64{}
	for _, r := range cu {
		row := TradeCurrency{Currency: r.Cur, Amount: r2(r.Amt), Home: r2(r.Home), Docs: r.N, Foreign: r.Cur != home}
		if tt.Revenue != 0 {
			row.SharePct = r1(r.Home / tt.Revenue * 100)
		}
		t.Currencies = append(t.Currencies, row)
		if r.Cur != home {
			revIn[r.Cur] = r.Amt
		}
	}

	// ---- top export clients ----
	var tc []struct {
		CK, Country, Curs string
		Rev               float64
		N                 int
	}
	s.scan(&tc, cte+`
SELECT ck, MIN(country) AS country, string_agg(DISTINCT cur, ', ' ORDER BY cur) AS curs, COALESCE(SUM(`+tradeNet+`), 0) AS rev, COUNT(*) FILTER (WHERE doc_type = 'invoice') AS n
FROM td WHERE f IS NOT NULL AND country <> @hc AND d BETWEEN (@from)::date AND (@to)::date
GROUP BY ck ORDER BY rev DESC, ck LIMIT 10`, a)
	names := s.clientNames(org)
	for _, r := range tc {
		row := TradeClient{Name: nameOf(names, r.CK), CustomerID: customerID(r.CK), Country: r.Country, CountryNm: countryLabel(r.Country), Currencies: r.Curs, Revenue: r2(r.Rev), Invoices: r.N}
		if tt.Export != 0 {
			row.SharePct = r1(r.Rev / tt.Export * 100)
		}
		t.TopExport = append(t.TopExport, row)
	}

	t.Receivables = s.fxReceivables(org, home, today)
	t.Realised = s.fxRealised(org, home, o.Range, today)
	var costOut map[string]float64
	t.Costs, costOut = s.fxCosts(org, home, o.Range)
	t.Hedge = s.hedge(org, home, revIn, costOut)

	// ---- notes ----
	if hc == "" {
		t.Notes = append(t.Notes, "Your organisation has no country set (Settings → Organisation): every client with a country is counted as export.")
	}
	if tt.AssumedDocs > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d document%s in %s for clients without a country %s assumed domestic — set the country on those clients to be sure.", tt.AssumedDocs, plural(tt.AssumedDocs), home, wasWere(tt.AssumedDocs)))
	}
	if tt.InferredDocs > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d foreign-currency document%s for clients without a country %s filed under the currency's country.", tt.InferredDocs, plural(tt.InferredDocs), wasWere(tt.InferredDocs)))
	}
	if tt.Excluded > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d document%s left out: no exchange rate (set a manual rate in Invoice settings → Exchange rates).", tt.Excluded, plural(tt.Excluded)))
	}
	t.Notes = append(t.Notes, c.Note)
	return t
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

func countryLabel(cc string) string {
	switch cc {
	case UnknownForeign:
		return "Other (foreign currency, country not set)"
	case "":
		return "Country not set"
	}
	return CountryName(cc)
}

// fxReceivables values the open foreign-currency invoices at the rate locked
// on each invoice and at today's rate.
func (s *Service) fxReceivables(org, home string, today time.Time) *FXReceivables {
	out := &FXReceivables{Rows: []FXRecvRow{}, Formula: FormulaUnrealised}
	a := map[string]interface{}{"org": org, "home": home, "today": today.Format(dayFmt)}
	const open = `FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status IN ('sent','partial') AND i.balance_due > 0`
	const cur = `UPPER(COALESCE(NULLIF(i.currency,''), @home))`
	const locked = `(UPPER(COALESCE(i.base_currency,'')) = @home AND i.exchange_rate > 0)`
	var rows []struct {
		Cur                        string
		N, NoLocked                int
		Bal, LockedBal, LockedHome float64
		Overdue                    float64
	}
	s.scan(&rows, `SELECT `+cur+` AS cur, COUNT(*) AS n, COUNT(*) FILTER (WHERE NOT `+locked+`) AS no_locked,
	COALESCE(SUM(i.balance_due), 0) AS bal,
	COALESCE(SUM(i.balance_due) FILTER (WHERE `+locked+`), 0) AS locked_bal,
	COALESCE(SUM(i.balance_due / NULLIF(i.exchange_rate, 0)) FILTER (WHERE `+locked+`), 0) AS locked_home,
	COALESCE(SUM(i.balance_due) FILTER (WHERE i.due_date::date < (@today)::date), 0) AS overdue
`+open+` AND `+cur+` <> @home GROUP BY 1 ORDER BY 1`, a)
	for _, r := range rows {
		row := FXRecvRow{Currency: r.Cur, Invoices: r.N, Balance: r2(r.Bal), LockedBalance: r2(r.LockedBal), LockedHome: r2(r.LockedHome), NoLocked: r.NoLocked, Overdue: r2(r.Overdue)}
		if r.LockedBal > 0 && r.LockedHome > 0 {
			row.AvgLockedRate = fptr(roundRate(r.LockedHome / r.LockedBal))
		}
		if q := s.FX.Quote(org, r.Cur, home); q.OK() {
			row.TodayRate, row.RateSource, row.RateProvider = q.Rate, q.Source, q.Provider
			if r.LockedBal > 0 {
				th := r.LockedBal * q.Rate
				un := th - r.LockedHome
				row.TodayHome, row.Unrealised = fptr(r2(th)), fptr(r2(un))
				if r.LockedHome != 0 {
					row.UnrealisedPct = fptr(r1(un / r.LockedHome * 100))
				}
				out.TodayHome += th
				out.Unrealised += un
				out.LockedHome += r.LockedHome
			}
		}
		out.Invoices += r.N
		out.Rows = append(out.Rows, row)
	}
	sort.SliceStable(out.Rows, func(x, y int) bool { return out.Rows[x].LockedHome > out.Rows[y].LockedHome })
	out.LockedHome, out.TodayHome, out.Unrealised = r2(out.LockedHome), r2(out.TodayHome), r2(out.Unrealised)
	s.DB.Raw(`SELECT COALESCE(SUM(i.balance_due), 0) `+open+` AND `+cur+` = @home`, a).Row().Scan(&out.HomeBalance)
	out.HomeBalance = r2(out.HomeBalance)
	return out
}

func roundRate(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}

// monthRates reads the month rates recorded by Expenses (expense_fx_months,
// read only): currency → month (YYYY-MM) → home units per 1 unit.
func (s *Service) monthRates(org, home string) map[string]map[string]float64 {
	out := map[string]map[string]float64{}
	if !s.DB.Migrator().HasTable("expense_fx_months") {
		return out
	}
	var rows []struct {
		FromCurrency, Month string
		Rate                float64
	}
	s.DB.Raw(`SELECT from_currency, month, rate FROM expense_fx_months WHERE organization_id = ? AND to_currency = ? AND rate > 0`, org, home).Scan(&rows)
	for _, r := range rows {
		c := strings.ToUpper(r.FromCurrency)
		if out[c] == nil {
			out[c] = map[string]float64{}
		}
		out[c][r.Month] = r.Rate
	}
	return out
}

// fxRealised compares, for every payment received on a foreign-currency
// invoice in the range, its home value at the rate of the payment month with
// its value at the rate locked on the invoice.
func (s *Service) fxRealised(org, home string, r Range, today time.Time) *FXRealised {
	out := &FXRealised{Rows: []FXRealRow{}, Formula: FormulaRealised}
	var rows []struct {
		Cur, M     string
		Amt, AtInv float64
		N          int
	}
	s.scan(&rows, `SELECT UPPER(i.currency) AS cur, to_char(p.date, 'YYYY-MM') AS m, COALESCE(SUM(p.amount), 0) AS amt,
	COALESCE(SUM(p.amount / i.exchange_rate), 0) AS at_inv, COUNT(*) AS n
FROM billing_payments p JOIN billing_invoices i ON i.id = p.invoice_id AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status <> 'void'
WHERE p.organization_id = @org AND p.date BETWEEN (@from)::date AND (@to)::date
	AND UPPER(COALESCE(NULLIF(i.currency,''), @home)) <> @home AND UPPER(COALESCE(i.base_currency,'')) = @home AND i.exchange_rate > 0
GROUP BY 1, 2 ORDER BY 1, 2`, map[string]interface{}{"org": org, "home": home, "from": r.From.Format(dayFmt), "to": r.To.Format(dayFmt)})
	rates := s.monthRates(org, home)
	thisMonth := today.Format("2006-01")
	by := map[string]*FXRealRow{}
	order := []string{}
	todayRate := map[string]float64{}
	for _, x := range rows {
		row := by[x.Cur]
		if row == nil {
			row = &FXRealRow{Currency: x.Cur}
			by[x.Cur] = row
			order = append(order, x.Cur)
		}
		rate := rates[x.Cur][x.M]
		if rate <= 0 && x.M == thisMonth {
			// The current month's rate is today's rate until the month ends.
			if _, ok := todayRate[x.Cur]; !ok {
				if q := s.FX.Quote(org, x.Cur, home); q.OK() {
					todayRate[x.Cur] = q.Rate
				} else {
					todayRate[x.Cur] = 0
				}
			}
			rate = todayRate[x.Cur]
		}
		if rate <= 0 {
			row.Skipped += x.N
			out.Skipped += x.N
			continue
		}
		row.Payments += x.N
		row.Amount += x.Amt
		row.AtInvoice += x.AtInv
		row.AtPayment += x.Amt * rate
		out.Counted += x.N
	}
	for _, k := range order {
		row := by[k]
		row.Gain = r2(row.AtPayment - row.AtInvoice)
		row.Amount, row.AtInvoice, row.AtPayment = r2(row.Amount), r2(row.AtInvoice), r2(row.AtPayment)
		out.Gain += row.Gain
		out.Rows = append(out.Rows, *row)
	}
	out.Gain = r2(out.Gain)
	switch {
	case out.Counted == 0 && out.Skipped == 0:
		out.Note = "No payments on foreign-currency invoices in this period."
	case out.Skipped > 0:
		out.Note = fmt.Sprintf("%d payment%s not counted: no exchange rate was recorded for the month they were received (Expenses → exchange rates records one per month from now on; you can also set a month's rate by hand).", out.Skipped, plural(out.Skipped))
	}
	return out
}

// fxCosts sums the approved expenses dated in the range that were charged in
// a foreign currency (pkg/expenses, read only), by currency and supplier.
func (s *Service) fxCosts(org, home string, r Range) ([]FXCost, map[string]float64) {
	out := []FXCost{}
	sums := map[string]float64{}
	if !s.DB.Migrator().HasColumn("expense_items", "currency") {
		return out, sums
	}
	var rows []struct {
		Cur, Vendor string
		Amt, Home   float64
		N           int
	}
	s.scan(&rows, `SELECT UPPER(e.currency) AS cur, COALESCE(NULLIF(TRIM(e.vendor_name),''), 'Other suppliers') AS vendor,
	COALESCE(SUM(e.net), 0) AS amt, COALESCE(SUM(e.home_net), 0) AS home, COUNT(*) AS n
FROM expense_items e
WHERE e.organization_id = @org AND e.status = 'approved' AND e.invoice_date BETWEEN (@from)::date AND (@to)::date
	AND COALESCE(e.currency,'') <> '' AND UPPER(e.currency) <> @home
GROUP BY 1, 2 ORDER BY 1, 4 DESC, 2`, map[string]interface{}{"org": org, "home": home, "from": r.From.Format(dayFmt), "to": r.To.Format(dayFmt)})
	idx := map[string]int{}
	for _, x := range rows {
		k, ok := idx[x.Cur]
		if !ok {
			out = append(out, FXCost{Currency: x.Cur, Vendors: []FXVendor{}})
			k = len(out) - 1
			idx[x.Cur] = k
		}
		out[k].Amount += x.Amt
		out[k].Home += x.Home
		out[k].Items += x.N
		out[k].Vendors = append(out[k].Vendors, FXVendor{Name: x.Vendor, Amount: r2(x.Amt), Home: r2(x.Home), Items: x.N})
		sums[x.Cur] += x.Amt
	}
	for k := range out {
		out[k].Amount, out[k].Home = r2(out[k].Amount), r2(out[k].Home)
	}
	sort.SliceStable(out, func(x, y int) bool { return out[x].Home > out[y].Home })
	return out, sums
}

// hedge compares what was earned and spent in each foreign currency.
func (s *Service) hedge(org, home string, in, out map[string]float64) []HedgeRow {
	set := map[string]bool{}
	for k, v := range in {
		if v != 0 {
			set[k] = true
		}
	}
	for k, v := range out {
		if v != 0 {
			set[k] = true
		}
	}
	curs := make([]string, 0, len(set))
	for k := range set {
		curs = append(curs, k)
	}
	sort.Strings(curs)
	rows := []HedgeRow{}
	for _, cur := range curs {
		h := HedgeRow{Currency: cur, In: r2(in[cur]), Out: r2(out[cur])}
		h.Net = r2(h.In - h.Out)
		if h.In > 0 {
			h.CoverPct = fptr(r1(math.Min(h.Out/h.In, 9.99) * 100))
		}
		if q := s.FX.Quote(org, cur, home); q.OK() {
			h.Rate = q.Rate
			h.NetHome = fptr(r2(h.Net * q.Rate))
			h.Move5Home = fptr(r2(math.Abs(h.Net) * q.Rate * 0.05))
		}
		move := ""
		if h.Move5Home != nil {
			move = fmt.Sprintf(" A 5%% move in %s/%s changes that by %s %s.", cur, home, home, amt(*h.Move5Home))
		}
		switch {
		case h.In > 0 && h.Out > 0 && h.Net >= 0:
			h.Summary = fmt.Sprintf("%s %s in vs %s %s out: your %s costs cover %s%% of your %s income, leaving %s %s exposed to the exchange rate.%s",
				cur, amt(h.In), cur, amt(h.Out), cur, trimFloat(*h.CoverPct), cur, cur, amt(h.Net), move)
		case h.In > 0 && h.Out > 0:
			h.Summary = fmt.Sprintf("%s %s in vs %s %s out: you spend more %s than you earn, so %s %s has to be bought with %s.%s",
				cur, amt(h.In), cur, amt(h.Out), cur, cur, amt(-h.Net), home, move)
		case h.In > 0:
			h.Summary = fmt.Sprintf("%s %s in, nothing spent in %s: all of it is exposed to the exchange rate.%s", cur, amt(h.In), cur, move)
		default:
			h.Summary = fmt.Sprintf("%s %s out, no %s income: a weaker %s makes these costs dearer.%s", cur, amt(h.Out), cur, home, move)
		}
		rows = append(rows, h)
	}
	sort.SliceStable(rows, func(x, y int) bool {
		ax, ay := 0.0, 0.0
		if rows[x].Move5Home != nil {
			ax = *rows[x].Move5Home
		}
		if rows[y].Move5Home != nil {
			ay = *rows[y].Move5Home
		}
		if ax != ay {
			return ax > ay
		}
		return rows[x].Currency < rows[y].Currency
	})
	return rows
}

// ---------- drill-down ----------

// TradeDoc is one document behind a trade figure.
type TradeDoc struct {
	ID         string   `json:"id"`
	Number     string   `json:"number"`
	DocType    string   `json:"doc_type"`
	Status     string   `json:"status"`
	Date       string   `json:"date"`
	Due        string   `json:"due"`
	Client     string   `json:"client"`
	CustomerID string   `json:"customer_id,omitempty"`
	Country    string   `json:"country"`
	CountryNm  string   `json:"country_name"`
	Basis      string   `json:"country_basis"` // client | currency | assumed
	Currency   string   `json:"currency"`
	Net        float64  `json:"net"`      // document currency, signed
	NetHome    float64  `json:"net_home"` // at the locked rate
	Balance    float64  `json:"balance"`
	LockedRate *float64 `json:"locked_rate"` // home per 1 unit
	TodayHome  *float64 `json:"today_home"`  // open balance at today's rate
	Unrealised *float64 `json:"unrealised"`
}

// TradeDocsFilter picks the documents to list.
type TradeDocsFilter struct {
	Country  string // ISO2, "export" or "domestic"
	Currency string
	Open     bool // open invoices today instead of the revenue of the range
}

// TradeDocs lists the documents behind a country / currency figure (max 500).
func (s *Service) TradeDocs(org string, o Opts, f TradeDocsFilter) []TradeDoc {
	c := s.conv(org, "")
	home := c.Home
	hc := s.homeCountry(org, home)
	a := o.args(org)
	a["hc"], a["home"] = hc, home
	where := "td.f IS NOT NULL AND td.d BETWEEN (@from)::date AND (@to)::date"
	if f.Open {
		where = "td.doc_type = 'invoice' AND td.status IN ('sent','partial') AND td.balance_due > 0"
	}
	switch cc := strings.ToUpper(strings.TrimSpace(f.Country)); {
	case cc == "EXPORT":
		where += " AND td.country <> @hc"
	case cc == "DOMESTIC":
		where += " AND td.country = @hc"
	case cc == "NONE":
		where += " AND td.country = ''"
	case reCountry.MatchString(cc) && len(cc) == 2:
		a["cc"] = cc
		where += " AND td.country = @cc"
	}
	if cur := strings.ToUpper(strings.TrimSpace(f.Currency)); reCur.MatchString(cur) {
		a["cur"] = cur
		where += " AND td.cur = @cur"
	}
	var rows []struct {
		ID, Number, DocType, Status, D, Due, CK, CustomerID, Country, Basis, Cur string
		Net, NetHome, Balance, Rate                                              float64
	}
	s.scan(&rows, "WITH "+tradeCTE(c, hc)+`
SELECT td.id, td.number, td.doc_type, td.status, to_char(td.d, 'YYYY-MM-DD') AS d, to_char(td.due, 'YYYY-MM-DD') AS due, td.ck, td.customer_id, td.country, td.basis, td.cur,
	td.sgn * (td.total - td.tax_total) AS net, COALESCE(td.sgn * td.f * (td.total - td.tax_total), 0) AS net_home, td.balance_due AS balance,
	CASE WHEN UPPER(COALESCE(i.base_currency,'')) = @home AND i.exchange_rate > 0 THEN 1.0 / i.exchange_rate ELSE 0 END AS rate
FROM td JOIN billing_invoices i ON i.id = td.id WHERE `+where+` ORDER BY td.d DESC, td.number DESC LIMIT 500`, a)
	names := s.clientNames(org)
	quotes := map[string]float64{}
	out := []TradeDoc{}
	for _, r := range rows {
		d := TradeDoc{ID: r.ID, Number: r.Number, DocType: r.DocType, Status: r.Status, Date: r.D, Due: r.Due, Client: nameOf(names, r.CK), CustomerID: r.CustomerID,
			Country: r.Country, CountryNm: countryLabel(r.Country), Basis: r.Basis, Currency: r.Cur, Net: r2(r.Net), NetHome: r2(r.NetHome), Balance: r2(r.Balance)}
		if r.Cur != home && r.Rate > 0 {
			d.LockedRate = fptr(roundRate(r.Rate))
			if f.Open && r.Balance > 0 {
				q, ok := quotes[r.Cur]
				if !ok {
					if qq := s.FX.Quote(org, r.Cur, home); qq.OK() {
						q = qq.Rate
					}
					quotes[r.Cur] = q
				}
				if q > 0 {
					d.TodayHome = fptr(r2(r.Balance * q))
					d.Unrealised = fptr(r2(r.Balance*q - r.Balance*r.Rate))
				}
			}
		}
		out = append(out, d)
	}
	return out
}
