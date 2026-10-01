package insights

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Analyst-grade analytics over invoices, credit notes and payments. Revenue
// is recognised on the issue date, excluding tax, credit notes negative,
// converted to the reporting currency (see Conv). Line-level figures split
// each document's net total across its lines in proportion to line totals
// (a window over the document), so line, client and month views always add
// up to the same total.

// Opts are the common query options.
type Opts struct {
	Range    Range
	Currency string // optional filter
	Client   string // client key (customer id or "n:<name>")
	Line     string // service line
	Month    string // YYYY-MM drill-down
	Grain    string // cohorts: month | quarter | year
	Today    time.Time
}

func (o Opts) args(org string) map[string]interface{} {
	today := o.Today
	if today.IsZero() {
		today = o.Range.To
	}
	return map[string]interface{}{
		"today": today.Format(dayFmt),
		"org":   org, "from": o.Range.From.Format(dayFmt), "to": o.Range.To.Format(dayFmt),
		"pfrom": o.Range.From.AddDate(-1, 0, 0).Format(dayFmt), "pto": o.Range.To.AddDate(-1, 0, 0).Format(dayFmt),
		"client": o.Client, "line": o.Line, "month": o.Month,
	}
}

// linesCTE adds `lines` (one row per document line with its share of the
// document's net total) and `fl` (lines with a rate, filtered).
func linesCTE(c *Conv, rules []ServiceRule, o Opts) string {
	desc := "COALESCE(NULLIF(TRIM(it.description),''), NULLIF(d.title,''), '')"
	share := `CASE WHEN COALESCE(SUM(it.line_total) OVER (PARTITION BY d.id), 0) <> 0
			THEN COALESCE(it.line_total,0) / SUM(it.line_total) OVER (PARTITION BY d.id)
			ELSE 1.0 / COUNT(*) OVER (PARTITION BY d.id) END`
	q := c.docsCTE() + `,
lines AS (
	SELECT d.id, d.number, d.doc_type, d.d, d.ck, d.customer_id, d.cur, d.f, d.source, ` + desc + ` AS descr,
		` + LineSQL(rules, desc) + ` AS line,
		d.sgn * (d.total - d.tax_total) * (` + share + `) AS amt_doc
	FROM docs d LEFT JOIN billing_invoice_items it ON it.invoice_id = d.id
),
fl AS (
	SELECT l.*, l.amt_doc * l.f AS amt FROM lines l WHERE l.f IS NOT NULL`
	if o.Client != "" {
		q += ` AND l.ck = @client`
	}
	if o.Line != "" {
		q += ` AND l.line = @line`
	}
	return q + `
)`
}

// ---------- revenue ----------

// MonthPoint is one month of revenue with its value a year earlier.
type MonthPoint struct {
	Month      string   `json:"month"`
	Revenue    float64  `json:"revenue"`
	PrevYear   *float64 `json:"prev_year"`
	YoYPct     *float64 `json:"yoy_pct"`
	Docs       int      `json:"docs"`
	Cumulative float64  `json:"cumulative"`
}

// Share is one row of a breakdown.
type Share struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	CustomerID string   `json:"customer_id,omitempty"`
	Revenue    float64  `json:"revenue"`
	Prev       float64  `json:"prev"`
	YoYPct     *float64 `json:"yoy_pct"`
	SharePct   float64  `json:"share_pct"`
	CumPct     float64  `json:"cum_pct"`
	Rank       int      `json:"rank"`
	Docs       int      `json:"docs"`
}

// DrillLine is one invoice line behind a figure.
type DrillLine struct {
	InvoiceID   string  `json:"invoice_id"`
	Number      string  `json:"number"`
	DocType     string  `json:"doc_type"`
	Date        string  `json:"date"`
	Client      string  `json:"client"`
	CustomerID  string  `json:"customer_id,omitempty"`
	Description string  `json:"description"`
	Line        string  `json:"line"`
	Currency    string  `json:"currency"`
	Amount      float64 `json:"amount"`      // document currency, excl. tax
	AmountHome  float64 `json:"amount_home"` // reporting currency
	Source      string  `json:"source"`
}

// Revenue is the revenue breakdown view.
type Revenue struct {
	Conv     *Conv        `json:"conversion"`
	From     string       `json:"from"`
	To       string       `json:"to"`
	Filters  Opts         `json:"-"`
	Total    float64      `json:"total"`
	Prev     float64      `json:"prev"`
	YoYPct   *float64     `json:"yoy_pct"`
	Docs     int          `json:"docs"`
	Clients  int          `json:"clients"`
	AvgDoc   float64      `json:"avg_invoice"`
	Months   []MonthPoint `json:"months"`
	ByClient []Share      `json:"by_client"`
	ByLine   []Share      `json:"by_line"`
	Lines    []DrillLine  `json:"lines"`
	Note     string       `json:"note"`
}

// RevenueBreakdown builds the revenue view.
func (s *Service) RevenueBreakdown(org string, o Opts) *Revenue {
	c := s.conv(org, o.Currency)
	rules := s.Rules(org)
	a := o.args(org)
	base := "WITH " + linesCTE(c, rules, o)
	out := &Revenue{Conv: c, From: o.Range.From.Format(dayFmt), To: o.Range.To.Format(dayFmt), Months: []MonthPoint{}, ByClient: []Share{}, ByLine: []Share{}, Lines: []DrillLine{},
		Note: "Revenue = invoices issued (not draft or void) minus credit notes, excluding tax, by issue date. A document's net total is split across its lines in proportion to the line totals."}

	var months []struct {
		Month       string
		Revenue     float64
		Docs        int
		Prev        *float64
		Cum         float64
		InRangeFlag bool
	}
	s.scan(&months, base+`,
m AS (
	SELECT gs::date AS m FROM generate_series(date_trunc('month', (@from)::date) - interval '12 months', date_trunc('month', (@to)::date), interval '1 month') gs
),
agg AS (
	SELECT m.m, COALESCE(SUM(fl.amt), 0) AS revenue, COUNT(DISTINCT fl.id) AS docs
	FROM m LEFT JOIN fl ON date_trunc('month', fl.d) = m.m AND fl.d <= (@to)::date
	GROUP BY m.m
)
SELECT to_char(m, 'YYYY-MM') AS month, revenue, docs,
	LAG(revenue, 12) OVER (ORDER BY m) AS prev,
	SUM(CASE WHEN m >= date_trunc('month', (@from)::date) THEN revenue ELSE 0 END) OVER (ORDER BY m) AS cum,
	m >= date_trunc('month', (@from)::date) AS in_range_flag
FROM agg ORDER BY m`, a)
	for _, m := range months {
		if !m.InRangeFlag {
			continue
		}
		p := MonthPoint{Month: m.Month, Revenue: r2(m.Revenue), Docs: m.Docs, Cumulative: r2(m.Cum)}
		if m.Prev != nil {
			p.PrevYear = fptr(r2(*m.Prev))
			p.YoYPct = pctChange(m.Revenue, *m.Prev)
		}
		out.Months = append(out.Months, p)
	}

	names := s.clientNames(org)
	breakdown := func(key string) []Share {
		var rows []struct {
			K       string
			Rev     float64
			Prev    float64
			Docs    int
			SharePc float64
			CumPc   float64
			Rk      int
		}
		s.scan(&rows, base+`,
g AS (
	SELECT `+key+` AS k,
		COALESCE(SUM(amt) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date), 0) AS rev,
		COALESCE(SUM(amt) FILTER (WHERE d BETWEEN (@pfrom)::date AND (@pto)::date), 0) AS prev,
		COUNT(DISTINCT id) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date) AS docs
	FROM fl GROUP BY 1
)
SELECT k, rev, prev, docs,
	CASE WHEN SUM(rev) OVER () <> 0 THEN rev / SUM(rev) OVER () * 100 ELSE 0 END AS share_pc,
	CASE WHEN SUM(rev) OVER () <> 0 THEN SUM(rev) OVER (ORDER BY rev DESC, k ROWS UNBOUNDED PRECEDING) / SUM(rev) OVER () * 100 ELSE 0 END AS cum_pc,
	RANK() OVER (ORDER BY rev DESC) AS rk
FROM g WHERE rev <> 0 OR prev <> 0 ORDER BY rev DESC, k`, a)
		res := []Share{}
		for _, r := range rows {
			sh := Share{Key: r.K, Revenue: r2(r.Rev), Prev: r2(r.Prev), YoYPct: pctChange(r.Rev, r.Prev), SharePct: r1(r.SharePc), CumPct: r1(r.CumPc), Rank: r.Rk, Docs: r.Docs}
			if key == "ck" {
				sh.Name, sh.CustomerID = nameOf(names, r.K), customerID(r.K)
			} else {
				sh.Name = LineLabels[r.K]
			}
			res = append(res, sh)
		}
		return res
	}
	out.ByClient = breakdown("ck")
	out.ByLine = breakdown("line")

	var tot struct {
		Rev, Prev     float64
		Docs, Clients int
		Inv           int
		InvTotal      float64
	}
	s.scan(&tot, base+`
SELECT COALESCE(SUM(amt) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date), 0) AS rev,
	COALESCE(SUM(amt) FILTER (WHERE d BETWEEN (@pfrom)::date AND (@pto)::date), 0) AS prev,
	COUNT(DISTINCT id) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date) AS docs,
	COUNT(DISTINCT ck) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date) AS clients,
	COUNT(DISTINCT id) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date AND doc_type = 'invoice') AS inv,
	COALESCE(SUM(amt) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date AND doc_type = 'invoice'), 0) AS inv_total
FROM fl`, a)
	out.Total, out.Prev, out.YoYPct, out.Docs, out.Clients = r2(tot.Rev), r2(tot.Prev), pctChange(tot.Rev, tot.Prev), tot.Docs, tot.Clients
	if tot.Inv > 0 {
		out.AvgDoc = r2(tot.InvTotal / float64(tot.Inv))
	}

	if o.Client != "" || o.Line != "" || o.Month != "" {
		w := "d BETWEEN (@from)::date AND (@to)::date"
		if o.Month != "" {
			w = "to_char(d, 'YYYY-MM') = @month"
		}
		var ls []struct {
			ID, Number, DocType, D, CK, CustomerID, Descr, Line, Cur, Source string
			AmtDoc, Amt                                                      float64
		}
		s.scan(&ls, base+`
SELECT id, number, doc_type, to_char(d, 'YYYY-MM-DD') AS d, ck, customer_id, descr, line, cur, source, amt_doc, amt
FROM fl WHERE `+w+` ORDER BY d DESC, number LIMIT 1000`, a)
		for _, l := range ls {
			out.Lines = append(out.Lines, DrillLine{InvoiceID: l.ID, Number: l.Number, DocType: l.DocType, Date: l.D, Client: nameOf(names, l.CK), CustomerID: l.CustomerID,
				Description: l.Descr, Line: l.Line, Currency: l.Cur, Amount: r2(l.AmtDoc), AmountHome: r2(l.Amt), Source: l.Source})
		}
	}
	return out
}

// ---------- clients: cohorts, concentration, churn, lifetime value ----------

// CohortRow is one cohort (clients whose first invoice fell in a period).
type CohortRow struct {
	Cohort    string    `json:"cohort"`
	Size      int       `json:"size"`
	Revenue0  float64   `json:"revenue_0"`
	Active    []int     `json:"active"`    // clients invoiced in period offset k
	Retention []float64 `json:"retention"` // % of the cohort
	Revenue   []float64 `json:"revenue"`   // revenue in period offset k
	NetRevRet []float64 `json:"net_revenue_retention"`
}

// Concentration measures dependence on a few clients.
type Concentration struct {
	Clients     int       `json:"clients"`
	Top1Pct     float64   `json:"top1_pct"`
	Top5Pct     float64   `json:"top5_pct"`
	Top10Pct    float64   `json:"top10_pct"`
	HHI         float64   `json:"hhi"` // 0–10,000 (shares in %)
	Effective   float64   `json:"effective_clients"`
	Level       string    `json:"level"` // low | moderate | high
	Lorenz      []float64 `json:"lorenz"`
	ByYear      []YearHHI `json:"by_year"`
	Top         []Share   `json:"top"`
	Explanation string    `json:"explanation"`
}

// YearHHI is the HHI of one calendar year.
type YearHHI struct {
	Year    int     `json:"year"`
	HHI     float64 `json:"hhi"`
	Clients int     `json:"clients"`
	Top1Pct float64 `json:"top1_pct"`
	Revenue float64 `json:"revenue"`
}

// ChurnClient is a client that stopped buying.
type ChurnClient struct {
	Key          string  `json:"key"`
	Name         string  `json:"name"`
	CustomerID   string  `json:"customer_id,omitempty"`
	First        string  `json:"first_invoice"`
	Last         string  `json:"last_invoice"`
	MonthsSince  int     `json:"months_since"`
	Invoices     int     `json:"invoices"`
	Lifetime     float64 `json:"lifetime_revenue"`
	AnnualValue  float64 `json:"annual_value"`
	Status       string  `json:"status"` // at_risk | lapsed
	Lines        string  `json:"lines"`
	HostingSites int     `json:"hosting_sites"`
}

// Churn summarises lapsed clients.
type Churn struct {
	AsOf           string        `json:"as_of"`
	Active         int           `json:"active"`  // invoiced in the last 6 months
	AtRisk         int           `json:"at_risk"` // last invoice 6–12 months ago
	Lapsed         int           `json:"lapsed"`  // last invoice over 12 months ago
	AtRiskValue    float64       `json:"at_risk_value"`
	LapsedValue    float64       `json:"lapsed_value"`
	RatePct        *float64      `json:"churn_rate_pct"` // clients active 12–24 months ago not active in the last 12
	ByYear         []YearChurn   `json:"by_year"`
	Clients        []ChurnClient `json:"clients"`
	CancelledSites []SiteRow     `json:"cancelled_sites"`
}

// YearChurn is the logo churn of one year.
type YearChurn struct {
	Year     int      `json:"year"`
	Start    int      `json:"start"`    // clients active the previous year
	Retained int      `json:"retained"` // of those, active this year
	New      int      `json:"new"`
	RatePct  *float64 `json:"churn_pct"`
}

// CLVRow is one client's lifetime value.
type CLVRow struct {
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	CustomerID  string  `json:"customer_id,omitempty"`
	First       string  `json:"first_invoice"`
	Last        string  `json:"last_invoice"`
	TenureMo    int     `json:"tenure_months"`
	Invoices    int     `json:"invoices"`
	Lifetime    float64 `json:"lifetime_revenue"`
	AnnualValue float64 `json:"annual_value"`
	CLV         float64 `json:"clv"`
	Status      string  `json:"status"`
}

// CLV summarises lifetime value.
type CLV struct {
	MarginPct      float64  `json:"margin_pct"`
	MarginSource   string   `json:"margin_source"`
	ExpectedYears  float64  `json:"expected_years"`
	Formula        string   `json:"formula"`
	Mean           float64  `json:"mean"`
	Median         float64  `json:"median"`
	Top            []CLVRow `json:"top"`
	TotalLifetime  float64  `json:"total_lifetime_revenue"`
	ActiveClients  int      `json:"active_clients"`
	AvgAnnualValue float64  `json:"avg_annual_value"`
}

// Clients is the clients view.
type Clients struct {
	Conv          *Conv          `json:"conversion"`
	From          string         `json:"from"`
	To            string         `json:"to"`
	Grain         string         `json:"grain"`
	Cohorts       []CohortRow    `json:"cohorts"`
	Periods       int            `json:"periods"`
	Concentration *Concentration `json:"concentration"`
	Churn         *Churn         `json:"churn"`
	CLV           *CLV           `json:"clv"`
}

func grainMonths(g string) int {
	switch g {
	case "month":
		return 1
	case "year":
		return 12
	}
	return 3
}

// ClientAnalytics builds cohorts, concentration, churn and CLV.
func (s *Service) ClientAnalytics(org string, o Opts) *Clients {
	c := s.conv(org, o.Currency)
	if o.Grain != "month" && o.Grain != "year" {
		o.Grain = "quarter"
	}
	a := o.args(org)
	out := &Clients{Conv: c, From: o.Range.From.Format(dayFmt), To: o.Range.To.Format(dayFmt), Grain: o.Grain, Cohorts: []CohortRow{}}
	gm := grainMonths(o.Grain)
	maxP := map[string]int{"month": 24, "quarter": 12, "year": 10}[o.Grain]
	out.Periods = maxP

	// Cohorts: first invoice period × periods since (window MIN over each client).
	var cr []struct {
		Cohort  time.Time
		Off     int
		Clients int
		Rev     float64
	}
	s.scan(&cr, "WITH "+c.docsCTE()+`,
act AS (
	SELECT ck, date_trunc('`+o.Grain+`', d)::date AS p, SUM(sgn * f * (total - tax_total)) AS rev
	FROM docs WHERE f IS NOT NULL AND d <= (@to)::date GROUP BY 1, 2
),
c AS (
	SELECT ck, p, rev, MIN(p) OVER (PARTITION BY ck) AS cohort FROM act
)
SELECT cohort, ((EXTRACT(YEAR FROM p) - EXTRACT(YEAR FROM cohort)) * 12 + EXTRACT(MONTH FROM p) - EXTRACT(MONTH FROM cohort))::int / `+fmt.Sprint(gm)+` AS off,
	COUNT(DISTINCT ck) AS clients, SUM(rev) AS rev
FROM c WHERE cohort >= date_trunc('`+o.Grain+`', (@from)::date) GROUP BY 1, 2 ORDER BY 1, 2`, a)
	byC := map[string]*CohortRow{}
	order := []string{}
	for _, r := range cr {
		key := cohortLabel(r.Cohort, o.Grain)
		row := byC[key]
		if row == nil {
			row = &CohortRow{Cohort: key, Active: make([]int, maxP), Retention: make([]float64, maxP), Revenue: make([]float64, maxP), NetRevRet: make([]float64, maxP)}
			byC[key] = row
			order = append(order, key)
		}
		if r.Off == 0 {
			row.Size, row.Revenue0 = r.Clients, r2(r.Rev)
		}
		if r.Off >= 0 && r.Off < maxP {
			row.Active[r.Off] = r.Clients
			row.Revenue[r.Off] = r2(r.Rev)
		}
	}
	// Periods that have not happened yet are marked -1.
	cur := periodIndex(o.Range.To, o.Grain)
	for _, k := range order {
		row := byC[k]
		start := periodIndexLabel(k, o.Grain)
		for i := 0; i < maxP; i++ {
			if start+i > cur {
				row.Active[i], row.Retention[i], row.Revenue[i], row.NetRevRet[i] = -1, -1, -1, -1
				continue
			}
			if row.Size > 0 {
				row.Retention[i] = r1(float64(row.Active[i]) / float64(row.Size) * 100)
			}
			if row.Revenue0 != 0 {
				row.NetRevRet[i] = r1(row.Revenue[i] / row.Revenue0 * 100)
			}
		}
		out.Cohorts = append(out.Cohorts, *row)
	}

	out.Concentration = s.concentration(org, c, o)
	out.Churn = s.churn(org, c, o)
	out.CLV = s.clv(org, c, o, out.Churn)
	return out
}

func cohortLabel(t time.Time, grain string) string {
	switch grain {
	case "month":
		return t.Format("2006-01")
	case "year":
		return t.Format("2006")
	}
	return fmt.Sprintf("%d-Q%d", t.Year(), (int(t.Month())-1)/3+1)
}

func periodIndex(t time.Time, grain string) int {
	m := t.Year()*12 + int(t.Month()) - 1
	return m / grainMonths(grain)
}

func periodIndexLabel(label, grain string) int {
	var y, n int
	switch grain {
	case "month":
		fmt.Sscanf(label, "%d-%d", &y, &n)
		return periodIndex(time.Date(y, time.Month(n), 1, 0, 0, 0, 0, time.UTC), grain)
	case "year":
		fmt.Sscanf(label, "%d", &y)
		return periodIndex(time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC), grain)
	}
	fmt.Sscanf(label, "%d-Q%d", &y, &n)
	return periodIndex(time.Date(y, time.Month((n-1)*3+1), 1, 0, 0, 0, 0, time.UTC), grain)
}

// HHI returns the Herfindahl–Hirschman index (shares in %, 0–10,000) of positive values.
func HHI(vals []float64) float64 {
	var tot float64
	for _, v := range vals {
		if v > 0 {
			tot += v
		}
	}
	if tot == 0 {
		return 0
	}
	var h float64
	for _, v := range vals {
		if v > 0 {
			sh := v / tot * 100
			h += sh * sh
		}
	}
	return math.Round(h)
}

func concentrationLevel(h float64) string {
	switch {
	case h >= 2500:
		return "high"
	case h >= 1500:
		return "moderate"
	}
	return "low"
}

func (s *Service) concentration(org string, c *Conv, o Opts) *Concentration {
	a := o.args(org)
	out := &Concentration{Lorenz: []float64{}, ByYear: []YearHHI{}, Top: []Share{}}
	var rows []struct {
		CK    string
		Rev   float64
		Share float64
		Cum   float64
		Rk    int
	}
	s.scan(&rows, "WITH "+c.docsCTE()+`,
g AS (
	SELECT ck, SUM(sgn * f * (total - tax_total)) AS rev FROM docs
	WHERE f IS NOT NULL AND d BETWEEN (@from)::date AND (@to)::date GROUP BY ck
	HAVING SUM(sgn * f * (total - tax_total)) > 0
)
SELECT ck, rev, rev / SUM(rev) OVER () * 100 AS share,
	SUM(rev) OVER (ORDER BY rev DESC, ck ROWS UNBOUNDED PRECEDING) / SUM(rev) OVER () * 100 AS cum,
	ROW_NUMBER() OVER (ORDER BY rev DESC, ck) AS rk
FROM g ORDER BY rev DESC, ck`, a)
	names := s.clientNames(org)
	vals := []float64{}
	for _, r := range rows {
		vals = append(vals, r.Rev)
		out.Lorenz = append(out.Lorenz, r1(r.Cum))
		switch r.Rk {
		case 1:
			out.Top1Pct = r1(r.Cum)
		case 5:
			out.Top5Pct = r1(r.Cum)
		case 10:
			out.Top10Pct = r1(r.Cum)
		}
		if r.Rk <= 10 {
			out.Top = append(out.Top, Share{Key: r.CK, Name: nameOf(names, r.CK), CustomerID: customerID(r.CK), Revenue: r2(r.Rev), SharePct: r1(r.Share), CumPct: r1(r.Cum), Rank: r.Rk})
		}
	}
	n := len(rows)
	out.Clients = n
	if n > 0 && n < 5 {
		out.Top5Pct = 100
	}
	if n > 0 && n < 10 {
		out.Top10Pct = 100
	}
	out.HHI = HHI(vals)
	if out.HHI > 0 {
		out.Effective = r1(10000 / out.HHI)
	}
	out.Level = concentrationLevel(out.HHI)
	out.Explanation = fmt.Sprintf("HHI %s (sum of squared revenue shares in %%; under 1,500 is unconcentrated, 1,500–2,500 moderate, above 2,500 high). Your revenue behaves like %s equal-sized clients.",
		trimFloat(out.HHI), trimFloat(out.Effective))

	var ys []struct {
		Y       int
		HHI     float64
		Clients int
		Top1    float64
		Rev     float64
	}
	s.scan(&ys, "WITH "+c.docsCTE()+`,
g AS (
	SELECT EXTRACT(YEAR FROM d)::int AS y, ck, SUM(sgn * f * (total - tax_total)) AS rev FROM docs
	WHERE f IS NOT NULL AND d BETWEEN (@from)::date AND (@to)::date GROUP BY 1, 2 HAVING SUM(sgn * f * (total - tax_total)) > 0
),
sh AS (SELECT y, ck, rev, rev / SUM(rev) OVER (PARTITION BY y) * 100 AS share FROM g)
SELECT y, ROUND(SUM(share * share)::numeric)::float8 AS hhi, COUNT(*) AS clients, MAX(share) AS top1, SUM(rev) AS rev
FROM sh GROUP BY y ORDER BY y`, a)
	for _, y := range ys {
		out.ByYear = append(out.ByYear, YearHHI{Year: y.Y, HHI: y.HHI, Clients: y.Clients, Top1Pct: r1(y.Top1), Revenue: r2(y.Rev)})
	}
	return out
}

func (s *Service) churn(org string, c *Conv, o Opts) *Churn {
	a := o.args(org)
	out := &Churn{AsOf: o.Range.To.Format(dayFmt), ByYear: []YearChurn{}, Clients: []ChurnClient{}, CancelledSites: []SiteRow{}}
	rules := s.Rules(org)
	var rows []struct {
		CK, First, Last, Lines string
		Months, N              int
		Life                   float64
	}
	s.scan(&rows, "WITH "+linesCTE(c, rules, Opts{})+`,
cl AS (
	SELECT ck, MIN(d) AS first_d, MAX(d) FILTER (WHERE doc_type = 'invoice') AS last_d,
		COUNT(DISTINCT id) FILTER (WHERE doc_type = 'invoice') AS n, SUM(amt) AS life,
		STRING_AGG(DISTINCT line, ',') AS lines
	FROM fl WHERE d <= (@to)::date GROUP BY ck
)
SELECT ck, to_char(first_d, 'YYYY-MM-DD') AS first, to_char(last_d, 'YYYY-MM-DD') AS last, lines, n, life,
	((EXTRACT(YEAR FROM (@to)::date) - EXTRACT(YEAR FROM last_d)) * 12 + EXTRACT(MONTH FROM (@to)::date) - EXTRACT(MONTH FROM last_d))::int AS months
FROM cl WHERE last_d IS NOT NULL ORDER BY life DESC`, a)
	names := s.clientNames(org)
	sites := map[string]int{}
	if rep := s.synergy(org); rep != nil {
		for _, st := range rep.Sites {
			if st.Status != "cancelled" && st.CustomerID != "" {
				sites[st.CustomerID]++
			}
			if st.Status == "cancelled" {
				out.CancelledSites = append(out.CancelledSites, siteRow(st))
			}
		}
	}
	to := o.Range.To
	for _, r := range rows {
		last, _ := time.Parse(dayFmt, r.Last)
		first, _ := time.Parse(dayFmt, r.First)
		ago := to.Sub(last).Hours() / 24
		years := math.Max(1, to.Sub(first).Hours()/24/365.25)
		cc := ChurnClient{Key: r.CK, Name: nameOf(names, r.CK), CustomerID: customerID(r.CK), First: r.First, Last: r.Last, MonthsSince: r.Months, Invoices: r.N,
			Lifetime: r2(r.Life), Lines: r.Lines, HostingSites: sites[customerID(r.CK)]}
		span := math.Max(1, last.Sub(first).Hours()/24/365.25)
		cc.AnnualValue = r2(r.Life / span)
		_ = years
		switch {
		case ago <= 182:
			out.Active++
			continue
		case ago <= 365:
			cc.Status = "at_risk"
			out.AtRisk++
			out.AtRiskValue += cc.AnnualValue
		default:
			cc.Status = "lapsed"
			out.Lapsed++
			out.LapsedValue += cc.AnnualValue
		}
		if r.Life <= 0 {
			continue
		}
		out.Clients = append(out.Clients, cc)
	}
	out.AtRiskValue, out.LapsedValue = r2(out.AtRiskValue), r2(out.LapsedValue)
	sort.SliceStable(out.Clients, func(i, j int) bool { return out.Clients[i].AnnualValue > out.Clients[j].AnnualValue })

	// Churn rate: active 12–24 months before `to`, not active in the last 12.
	var cr struct{ Base, Lost int }
	s.scan(&cr, "WITH "+c.docsCTE()+`,
prev AS (SELECT DISTINCT ck FROM docs WHERE doc_type = 'invoice' AND d > (@to)::date - interval '24 months' AND d <= (@to)::date - interval '12 months'),
now AS (SELECT DISTINCT ck FROM docs WHERE doc_type = 'invoice' AND d > (@to)::date - interval '12 months' AND d <= (@to)::date)
SELECT COUNT(*) AS base, COUNT(*) FILTER (WHERE ck NOT IN (SELECT ck FROM now)) AS lost FROM prev`, a)
	if cr.Base > 0 {
		out.RatePct = fptr(r1(float64(cr.Lost) / float64(cr.Base) * 100))
	}
	// Yearly logo churn with LAG over each client's active years.
	var yr []struct {
		Y, Start, Retained, New int
	}
	s.scan(&yr, "WITH "+c.docsCTE()+`,
ay AS (SELECT DISTINCT ck, EXTRACT(YEAR FROM d)::int AS y FROM docs WHERE doc_type = 'invoice' AND d <= (@to)::date),
w AS (SELECT ck, y, LAG(y) OVER (PARTITION BY ck ORDER BY y) AS prev_y, LEAD(y) OVER (PARTITION BY ck ORDER BY y) AS next_y FROM ay),
years AS (SELECT DISTINCT y FROM ay)
SELECT years.y,
	(SELECT COUNT(*) FROM w WHERE w.y = years.y - 1) AS start,
	(SELECT COUNT(*) FROM w WHERE w.y = years.y - 1 AND w.next_y = years.y) AS retained,
	(SELECT COUNT(*) FROM w WHERE w.y = years.y AND w.prev_y IS NULL) AS new
FROM years ORDER BY years.y`, a)
	for _, y := range yr {
		yc := YearChurn{Year: y.Y, Start: y.Start, Retained: y.Retained, New: y.New}
		if y.Start > 0 {
			yc.RatePct = fptr(r1(float64(y.Start-y.Retained) / float64(y.Start) * 100))
		}
		out.ByYear = append(out.ByYear, yc)
	}
	return out
}

func (s *Service) clv(org string, c *Conv, o Opts, ch *Churn) *CLV {
	out := &CLV{Top: []CLVRow{}}
	// Gross margin: revenue less direct (service-line) costs over the range.
	sl := s.ServiceLines(org, o)
	out.MarginPct, out.MarginSource = 100, "no direct costs recorded — 100% assumed"
	if sl.Revenue > 0 && sl.DirectCost > 0 {
		out.MarginPct = r1(math.Max(0, (sl.Revenue-sl.DirectCost)/sl.Revenue*100))
		out.MarginSource = "revenue less direct costs (hosting, domains, Workspace, software) over the selected range"
	}
	rate := 0.0
	if ch != nil && ch.RatePct != nil {
		rate = *ch.RatePct / 100
	}
	switch {
	case rate > 0:
		out.ExpectedYears = r1(math.Min(10, 1/rate))
	default:
		out.ExpectedYears = 5
	}
	out.Formula = fmt.Sprintf("CLV = average annual revenue × gross margin (%s%%) × expected lifetime (%s years = 1 ÷ annual churn rate, capped at 10; 5 when churn is unknown).",
		trimFloat(out.MarginPct), trimFloat(out.ExpectedYears))
	a := o.args(org)
	var rows []struct {
		CK, First, Last string
		N               int
		Life            float64
	}
	s.scan(&rows, "WITH "+c.docsCTE()+`
SELECT ck, to_char(MIN(d), 'YYYY-MM-DD') AS first, to_char(MAX(d), 'YYYY-MM-DD') AS last,
	COUNT(*) FILTER (WHERE doc_type = 'invoice') AS n, SUM(sgn * f * (total - tax_total)) AS life
FROM docs WHERE f IS NOT NULL AND d <= (@to)::date GROUP BY ck HAVING SUM(sgn * f * (total - tax_total)) > 0`, a)
	names := s.clientNames(org)
	vals := []float64{}
	var annSum float64
	for _, r := range rows {
		f, _ := time.Parse(dayFmt, r.First)
		l, _ := time.Parse(dayFmt, r.Last)
		ten := (l.Year()-f.Year())*12 + int(l.Month()) - int(f.Month()) + 1
		ann := r.Life / math.Max(1, float64(ten)/12)
		if ten < 12 {
			ann = r.Life // under a year of history: what they spent so far is the year's value
		}
		row := CLVRow{Key: r.CK, Name: nameOf(names, r.CK), CustomerID: customerID(r.CK), First: r.First, Last: r.Last, TenureMo: ten, Invoices: r.N,
			Lifetime: r2(r.Life), AnnualValue: r2(ann), CLV: r2(ann * out.MarginPct / 100 * out.ExpectedYears)}
		ago := o.Range.To.Sub(l).Hours() / 24
		switch {
		case ago <= 182:
			row.Status = "active"
			out.ActiveClients++
			annSum += ann
		case ago <= 365:
			row.Status = "at_risk"
		default:
			row.Status = "lapsed"
			row.CLV = 0
		}
		out.TotalLifetime += r.Life
		vals = append(vals, row.CLV)
		out.Top = append(out.Top, row)
	}
	sort.SliceStable(out.Top, func(i, j int) bool {
		if out.Top[i].CLV != out.Top[j].CLV {
			return out.Top[i].CLV > out.Top[j].CLV
		}
		return out.Top[i].Lifetime > out.Top[j].Lifetime
	})
	if len(out.Top) > 100 {
		out.Top = out.Top[:100]
	}
	out.TotalLifetime = r2(out.TotalLifetime)
	if out.ActiveClients > 0 {
		out.AvgAnnualValue = r2(annSum / float64(out.ActiveClients))
	}
	act := []float64{}
	for _, v := range vals {
		if v > 0 {
			act = append(act, v)
		}
	}
	out.Mean, out.Median = r2(mean(act)), r2(median(act))
	return out
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var t float64
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// ---------- payments: DSO and behaviour per client ----------

// PayClient is one client's payment behaviour.
type PayClient struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	CustomerID  string   `json:"customer_id,omitempty"`
	Paid        int      `json:"paid_invoices"`
	AvgDays     float64  `json:"avg_days_to_pay"`      // amount-weighted
	MedianDays  float64  `json:"median_days_to_pay"`   // per invoice
	AvgLate     float64  `json:"avg_days_late"`        // vs due date, amount-weighted
	OnTimePct   float64  `json:"on_time_pct"`          // paid by the due date
	Open        float64  `json:"open_balance"`         // today
	Overdue     float64  `json:"overdue_balance"`      // today
	OverdueDays int      `json:"oldest_overdue_days"`  // today
	Behaviour   string   `json:"behaviour"`            // prompt | ok | slow | very_slow | new
	Trend       *float64 `json:"trend_days,omitempty"` // last 3 paid vs earlier (days, + = slower)
}

// DSOPoint is days sales outstanding at a month end.
type DSOPoint struct {
	Month       string  `json:"month"`
	Receivables float64 `json:"receivables"`
	Sales90     float64 `json:"sales_90d"`
	DSO         float64 `json:"dso"`
	AvgDays     float64 `json:"avg_days_to_pay"` // invoices issued that month and paid
	Paid        int     `json:"paid_invoices"`
}

// Payments is the payments view.
type Payments struct {
	Conv      *Conv       `json:"conversion"`
	From      string      `json:"from"`
	To        string      `json:"to"`
	DSO       float64     `json:"dso"`
	AvgDays   float64     `json:"avg_days_to_pay"`
	MedianDay float64     `json:"median_days_to_pay"`
	OnTimePct float64     `json:"on_time_pct"`
	Paid      int         `json:"paid_invoices"`
	Months    []DSOPoint  `json:"months"`
	Clients   []PayClient `json:"clients"`
	Formula   string      `json:"formula"`
}

func payCTE(c *Conv) string {
	return c.docsCTE() + `,
pay AS (
	SELECT p.invoice_id, SUM(p.amount) AS paid, MAX(p.date)::date AS last_pay
	FROM billing_payments p WHERE p.organization_id = @org GROUP BY p.invoice_id
),
inv AS (
	SELECT d.*, COALESCE(pay.paid, 0) AS paid, pay.last_pay,
		GREATEST(0, (d.total - d.credits - d.writeoff) - COALESCE(pay.paid, 0)) AS unexplained,
		COALESCE(pay.last_pay, d.paid_at::date, d.due) AS settle
	FROM docs d LEFT JOIN pay ON pay.invoice_id = d.id
	WHERE d.doc_type = 'invoice' AND d.f IS NOT NULL
),
settled AS (
	SELECT inv.*, (settle - d) AS dtp, (settle - due) AS late, total * f AS amt_home
	FROM inv WHERE status = 'paid' AND total > 0 AND settle IS NOT NULL
)`
}

// PaymentAnalytics builds DSO and per-client payment behaviour.
func (s *Service) PaymentAnalytics(org string, o Opts) *Payments {
	c := s.conv(org, o.Currency)
	a := o.args(org)
	out := &Payments{Conv: c, From: o.Range.From.Format(dayFmt), To: o.Range.To.Format(dayFmt), Months: []DSOPoint{}, Clients: []PayClient{},
		Formula: "DSO at a month end = receivables then ÷ invoiced in the 90 days before × 90. Days to pay = last payment date − issue date of fully paid invoices (amount-weighted)."}
	var ms []struct {
		M       time.Time
		AR      float64
		Sales   float64
		AvgDays *float64
		Paid    int
	}
	s.scan(&ms, "WITH "+payCTE(c)+`,
me AS (
	SELECT LEAST((gs + interval '1 month' - interval '1 day')::date, (@to)::date) AS me
	FROM generate_series(date_trunc('month', (@from)::date), date_trunc('month', (@to)::date), interval '1 month') gs
),
grid AS (
	SELECT me.me, i.id, i.f, (i.total - i.credits - i.writeoff) AS net, i.balance_due, i.settle, i.unexplained
	FROM me JOIN inv i ON i.d <= me.me
),
pd AS (
	SELECT p.invoice_id, p.date::date AS pd, SUM(p.amount) AS amt FROM billing_payments p WHERE p.organization_id = @org GROUP BY 1, 2
),
paid AS (
	SELECT g.me, g.id, SUM(pd.amt) AS paid FROM grid g JOIN pd ON pd.invoice_id = g.id AND pd.pd <= g.me GROUP BY 1, 2
),
ar AS (
	SELECT g.me, SUM(g.f * GREATEST(0, g.net - COALESCE(paid.paid, 0)
		- CASE WHEN g.balance_due <= 0.005 AND g.settle <= g.me THEN g.unexplained ELSE 0 END)) AS ar
	FROM grid g LEFT JOIN paid ON paid.me = g.me AND paid.id = g.id GROUP BY g.me
),
sales AS (
	SELECT me.me, SUM(i.total * i.f) AS sales FROM me JOIN inv i ON i.d > me.me - 90 AND i.d <= me.me GROUP BY me.me
),
dtp AS (
	SELECT date_trunc('month', d)::date AS m, SUM(dtp * amt_home) / NULLIF(SUM(amt_home), 0) AS avg_days, COUNT(*) AS paid FROM settled GROUP BY 1
)
SELECT me.me AS m, COALESCE(ar.ar, 0) AS ar, COALESCE(sales.sales, 0) AS sales, dtp.avg_days, COALESCE(dtp.paid, 0) AS paid
FROM me LEFT JOIN ar ON ar.me = me.me LEFT JOIN sales ON sales.me = me.me LEFT JOIN dtp ON dtp.m = date_trunc('month', me.me)::date
ORDER BY me.me`, a)
	for _, m := range ms {
		p := DSOPoint{Month: m.M.Format("2006-01"), Receivables: r2(m.AR), Sales90: r2(m.Sales), Paid: m.Paid}
		if m.Sales > 0 {
			p.DSO = r1(m.AR / m.Sales * 90)
		}
		if m.AvgDays != nil {
			p.AvgDays = r1(*m.AvgDays)
		}
		out.Months = append(out.Months, p)
	}
	if n := len(out.Months); n > 0 {
		out.DSO = out.Months[n-1].DSO
	}
	var tot struct {
		Avg, Med, OnTime *float64
		N                int
	}
	s.scan(&tot, "WITH "+payCTE(c)+`
SELECT SUM(dtp * amt_home) / NULLIF(SUM(amt_home), 0) AS avg,
	PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY dtp) AS med,
	AVG(CASE WHEN late <= 0 THEN 100.0 ELSE 0 END) AS on_time, COUNT(*) AS n
FROM settled WHERE d BETWEEN (@from)::date AND (@to)::date`, a)
	if tot.Avg != nil {
		out.AvgDays = r1(*tot.Avg)
	}
	if tot.Med != nil {
		out.MedianDay = r1(*tot.Med)
	}
	if tot.OnTime != nil {
		out.OnTimePct = r1(*tot.OnTime)
	}
	out.Paid = tot.N

	var cs []struct {
		CK                  string
		N                   int
		Avg, Med, Late, OnT *float64
		Recent, Earlier     *float64
		Open, Overdue       float64
		Oldest              *int
	}
	s.scan(&cs, "WITH "+payCTE(c)+`,
ranked AS (
	SELECT st.*, ROW_NUMBER() OVER (PARTITION BY ck ORDER BY d DESC) AS rn FROM settled st WHERE d BETWEEN (@from)::date AND (@to)::date
),
per AS (
	SELECT ck, COUNT(*) AS n,
		SUM(dtp * amt_home) / NULLIF(SUM(amt_home), 0) AS avg,
		PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY dtp) AS med,
		SUM(late * amt_home) / NULLIF(SUM(amt_home), 0) AS late,
		AVG(CASE WHEN late <= 0 THEN 100.0 ELSE 0 END) AS on_t,
		AVG(dtp) FILTER (WHERE rn <= 3) AS recent,
		AVG(dtp) FILTER (WHERE rn > 3) AS earlier
	FROM ranked GROUP BY ck
),
open AS (
	SELECT ck, SUM(balance_due * f) AS open,
		SUM(balance_due * f) FILTER (WHERE due < (@today)::date) AS overdue,
		MAX((@today)::date - due) FILTER (WHERE due < (@today)::date) AS oldest
	FROM inv WHERE status IN ('sent','partial') AND balance_due > 0 GROUP BY ck
)
SELECT COALESCE(per.ck, open.ck) AS ck, COALESCE(per.n, 0) AS n, per.avg, per.med, per.late, per.on_t, per.recent, per.earlier,
	COALESCE(open.open, 0) AS open, COALESCE(open.overdue, 0) AS overdue, open.oldest
FROM per FULL OUTER JOIN open ON open.ck = per.ck
ORDER BY COALESCE(open.overdue, 0) DESC, per.avg DESC NULLS LAST`, a)
	names := s.clientNames(org)
	for _, r := range cs {
		pc := PayClient{Key: r.CK, Name: nameOf(names, r.CK), CustomerID: customerID(r.CK), Paid: r.N, Open: r2(r.Open), Overdue: r2(r.Overdue)}
		if r.Avg != nil {
			pc.AvgDays = r1(*r.Avg)
		}
		if r.Med != nil {
			pc.MedianDays = r1(*r.Med)
		}
		if r.Late != nil {
			pc.AvgLate = r1(*r.Late)
		}
		if r.OnT != nil {
			pc.OnTimePct = r1(*r.OnT)
		}
		if r.Oldest != nil {
			pc.OverdueDays = *r.Oldest
		}
		if r.Recent != nil && r.Earlier != nil {
			pc.Trend = fptr(r1(*r.Recent - *r.Earlier))
		}
		pc.Behaviour = Behaviour(pc.Paid, pc.AvgLate)
		out.Clients = append(out.Clients, pc)
	}
	return out
}

// Behaviour labels a client's payment habit from the average days late.
func Behaviour(paid int, avgLate float64) string {
	switch {
	case paid == 0:
		return "new"
	case avgLate <= 0:
		return "prompt"
	case avgLate <= 14:
		return "ok"
	case avgLate <= 45:
		return "slow"
	}
	return "very_slow"
}

// ---------- seasonality ----------

// Seasonality is revenue by calendar month across years.
type Seasonality struct {
	Conv    *Conv        `json:"conversion"`
	Years   []int        `json:"years"`
	Matrix  [][]*float64 `json:"matrix"` // [year][month 0–11]
	Index   []float64    `json:"index"`  // 100 = an average month
	Average []float64    `json:"average"`
	Peak    string       `json:"peak"`
	Low     string       `json:"low"`
	Note    string       `json:"note"`
}

// SeasonalityAnalytics builds the seasonality view (full calendar years only count towards the index).
func (s *Service) SeasonalityAnalytics(org string, o Opts) *Seasonality {
	c := s.conv(org, o.Currency)
	a := o.args(org)
	out := &Seasonality{Conv: c, Years: []int{}, Matrix: [][]*float64{}, Index: make([]float64, 12), Average: make([]float64, 12)}
	var rows []struct {
		Y, M int
		Rev  float64
	}
	s.scan(&rows, "WITH "+c.docsCTE()+`
SELECT EXTRACT(YEAR FROM d)::int AS y, EXTRACT(MONTH FROM d)::int AS m, SUM(sgn * f * (total - tax_total)) AS rev
FROM docs WHERE f IS NOT NULL AND d BETWEEN (@from)::date AND (@to)::date GROUP BY 1, 2 ORDER BY 1, 2`, a)
	yi := map[int]int{}
	for y := o.Range.From.Year(); y <= o.Range.To.Year(); y++ {
		yi[y] = len(out.Years)
		out.Years = append(out.Years, y)
		row := make([]*float64, 12)
		for m := 0; m < 12; m++ {
			d := time.Date(y, time.Month(m+1), 1, 0, 0, 0, 0, time.UTC)
			if !d.Before(monthStart(o.Range.From)) && !d.After(o.Range.To) {
				row[m] = fptr(0)
			}
		}
		out.Matrix = append(out.Matrix, row)
	}
	for _, r := range rows {
		if i, ok := yi[r.Y]; ok && r.M >= 1 && r.M <= 12 {
			out.Matrix[i][r.M-1] = fptr(r2(r.Rev))
		}
	}
	var sum [12]float64
	var cnt [12]int
	for _, row := range out.Matrix {
		for m, v := range row {
			if v != nil {
				sum[m] += *v
				cnt[m]++
			}
		}
	}
	var all float64
	n := 0
	for m := 0; m < 12; m++ {
		if cnt[m] > 0 {
			out.Average[m] = r2(sum[m] / float64(cnt[m]))
			all += out.Average[m]
			n++
		}
	}
	if n > 0 && all > 0 {
		avg := all / float64(n)
		hi, lo := -1, -1
		for m := 0; m < 12; m++ {
			if cnt[m] == 0 {
				continue
			}
			out.Index[m] = r1(out.Average[m] / avg * 100)
			if hi < 0 || out.Index[m] > out.Index[hi] {
				hi = m
			}
			if lo < 0 || out.Index[m] < out.Index[lo] {
				lo = m
			}
		}
		out.Peak, out.Low = time.Month(hi+1).String(), time.Month(lo+1).String()
	}
	out.Note = "Index = the month's average revenue across the years in range ÷ the average month × 100."
	return out
}

// ---------- service lines ----------

// LineMargin is revenue, direct cost and margin of one service line.
type LineMargin struct {
	Line      string    `json:"line"`
	Label     string    `json:"label"`
	Revenue   float64   `json:"revenue"`
	Prev      float64   `json:"prev"`
	YoYPct    *float64  `json:"yoy_pct"`
	Cost      float64   `json:"cost"`
	Margin    float64   `json:"margin"`
	MarginPct *float64  `json:"margin_pct"`
	Clients   int       `json:"clients"`
	Lines     int       `json:"lines"`
	Months    []float64 `json:"months"`
}

// ServiceLineView is margin by service line.
type ServiceLineView struct {
	Conv        *Conv         `json:"conversion"`
	From        string        `json:"from"`
	To          string        `json:"to"`
	Months      []string      `json:"months"`
	Lines       []LineMargin  `json:"lines"`
	Revenue     float64       `json:"revenue"`
	DirectCost  float64       `json:"direct_cost"`
	Overhead    float64       `json:"overhead"`
	Rules       []ServiceRule `json:"rules"`
	Defaults    bool          `json:"defaults"`
	CostMapping string        `json:"cost_mapping"`
}

// ServiceLines builds revenue, direct costs and margins per service line.
func (s *Service) ServiceLines(org string, o Opts) *ServiceLineView {
	c := s.conv(org, o.Currency)
	rules := s.Rules(org)
	var n int64
	s.DB.Model(&ServiceRule{}).Where("organization_id = ?", org).Count(&n)
	a := o.args(org)
	o2 := o
	o2.Line, o2.Client = "", ""
	out := &ServiceLineView{Conv: c, From: o.Range.From.Format(dayFmt), To: o.Range.To.Format(dayFmt), Months: []string{}, Lines: []LineMargin{}, Rules: rules, Defaults: n == 0,
		CostMapping: "Approved expenses (Expenses module, excl. tax, home currency) by category: Web Hosting, Support Fees and Synergy prepaid → Hosting; Domain Names → Domains; Email & Workspace → Google Workspace & email; Software & Subscriptions → Software & ERP. Other expenses are overhead."}
	for m := monthStart(o.Range.From); !m.After(o.Range.To); m = m.AddDate(0, 1, 0) {
		out.Months = append(out.Months, m.Format("2006-01"))
	}
	mi := map[string]int{}
	for i, m := range out.Months {
		mi[m] = i
	}
	by := map[string]*LineMargin{}
	get := func(l string) *LineMargin {
		if by[l] == nil {
			by[l] = &LineMargin{Line: l, Label: LineLabels[l], Months: make([]float64, len(out.Months))}
		}
		return by[l]
	}
	var rows []struct {
		Line, M      string
		Rev, Prev    float64
		Clients, Cnt int
	}
	s.scan(&rows, "WITH "+linesCTE(c, rules, o2)+`
SELECT line, CASE WHEN d BETWEEN (@from)::date AND (@to)::date THEN to_char(d, 'YYYY-MM') ELSE '' END AS m,
	COALESCE(SUM(amt) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date), 0) AS rev,
	COALESCE(SUM(amt) FILTER (WHERE d BETWEEN (@pfrom)::date AND (@pto)::date), 0) AS prev,
	COUNT(DISTINCT ck) AS clients, COUNT(*) AS cnt
FROM fl WHERE d BETWEEN (@pfrom)::date AND (@to)::date GROUP BY 1, 2`, a)
	clients := map[string]int{}
	var cl []struct {
		Line string
		N    int
	}
	s.scan(&cl, "WITH "+linesCTE(c, rules, o2)+`
SELECT line, COUNT(DISTINCT ck) AS n FROM fl WHERE d BETWEEN (@from)::date AND (@to)::date GROUP BY line`, a)
	for _, x := range cl {
		clients[x.Line] = x.N
	}
	for _, r := range rows {
		lm := get(r.Line)
		lm.Revenue += r.Rev
		lm.Prev += r.Prev
		if r.M != "" {
			if i, ok := mi[r.M]; ok {
				lm.Months[i] += r.Rev
			}
			lm.Lines += r.Cnt
		}
	}
	// Direct costs from approved expenses (home currency, excl. tax), unless a
	// currency filter is set (expenses are kept in the home currency).
	if c.Filter == "" {
		var cs []struct {
			Category string
			Cost     float64
		}
		s.scan(&cs, `SELECT category, SUM(home_net) AS cost FROM expense_items
			WHERE organization_id = @org AND status = 'approved' AND invoice_date BETWEEN (@from)::date AND (@to)::date GROUP BY category`, a)
		for _, x := range cs {
			if l := costLine(x.Category); l != "" {
				get(l).Cost += x.Cost
				out.DirectCost += x.Cost
			} else {
				out.Overhead += x.Cost
			}
		}
	}
	for _, l := range LineOrder {
		lm := by[l]
		if lm == nil {
			continue
		}
		lm.Revenue, lm.Prev, lm.Cost = r2(lm.Revenue), r2(lm.Prev), r2(lm.Cost)
		lm.YoYPct = pctChange(lm.Revenue, lm.Prev)
		lm.Margin = r2(lm.Revenue - lm.Cost)
		if lm.Revenue > 0 {
			lm.MarginPct = fptr(r1(lm.Margin / lm.Revenue * 100))
		}
		lm.Clients = clients[l]
		for i := range lm.Months {
			lm.Months[i] = r2(lm.Months[i])
		}
		out.Revenue += lm.Revenue
		if lm.Revenue != 0 || lm.Cost != 0 || lm.Prev != 0 {
			out.Lines = append(out.Lines, *lm)
		}
	}
	out.Revenue, out.DirectCost, out.Overhead = r2(out.Revenue), r2(out.DirectCost), r2(out.Overhead)
	return out
}

// labelList joins service line labels from a comma list of keys.
func labelList(keys string) string {
	out := []string{}
	for _, k := range strings.Split(keys, ",") {
		if l := LineLabels[strings.TrimSpace(k)]; l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, ", ")
}
