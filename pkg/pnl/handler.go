package pnl

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
)

// Handler serves GET /api/v1/reports/profit-loss.
type Handler struct {
	DB  *gorm.DB
	Now func() time.Time // overridable in tests
}

// NewHandler creates the handler.
func NewHandler(db *gorm.DB) *Handler { return &Handler{DB: db, Now: time.Now} }

// RegisterRoutes mounts the endpoint on the authenticated /api/v1 group.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	auth.GET("/reports/profit-loss", h.ProfitLoss)
}

func orgOf(c *gin.Context) string {
	for _, k := range []string{"org_id", "organization_id"} {
		if v, ok := c.Get(k); ok && v != nil {
			if s := fmt.Sprint(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

// ResolvePeriod works out the reporting period from ?from=&to= or ?range=.
// Ranges: 7d, 30d, 90d, 12m, mtd (= this_month), qtd (= this_quarter),
// ytd (= this_year), last_month, last_quarter, last_year, this_fy, last_fy.
func ResolvePeriod(today time.Time, from, to, rng string, fyStart int) (Period, error) {
	today = Day(today)
	f, errF := time.Parse(dayFmt, strings.TrimSpace(from))
	t, errT := time.Parse(dayFmt, strings.TrimSpace(to))
	var p Period
	switch {
	case errF == nil && errT == nil:
		p = Period{f, t}
	case errF == nil:
		p = Period{f, today}
	default:
		mStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
		qStart := time.Date(today.Year(), time.Month((int(today.Month())-1)/3*3+1), 1, 0, 0, 0, 0, time.UTC)
		yStart := time.Date(today.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		fy := time.Date(today.Year(), time.Month(fyStart), 1, 0, 0, 0, 0, time.UTC)
		if fy.After(today) {
			fy = fy.AddDate(-1, 0, 0)
		}
		switch strings.ToLower(strings.TrimSpace(rng)) {
		case "7d":
			p = Period{today.AddDate(0, 0, -6), today}
		case "30d":
			p = Period{today.AddDate(0, 0, -29), today}
		case "90d":
			p = Period{today.AddDate(0, 0, -89), today}
		case "12m", "1y":
			p = Period{mStart.AddDate(0, -11, 0), today}
		case "qtd", "this_quarter":
			p = Period{qStart, today}
		case "ytd", "this_year":
			p = Period{yStart, today}
		case "last_month":
			p = Period{mStart.AddDate(0, -1, 0), mStart.AddDate(0, 0, -1)}
		case "last_quarter":
			p = Period{qStart.AddDate(0, -3, 0), qStart.AddDate(0, 0, -1)}
		case "last_year":
			p = Period{yStart.AddDate(-1, 0, 0), yStart.AddDate(0, 0, -1)}
		case "this_fy":
			p = Period{fy, today}
		case "last_fy":
			p = Period{fy.AddDate(-1, 0, 0), fy.AddDate(0, 0, -1)}
		default: // mtd, this_month
			p = Period{mStart, today}
		}
	}
	if p.To.Before(p.From) {
		return p, fmt.Errorf("'from' must be on or before 'to'")
	}
	if p.Days() > 5*366 {
		return p, fmt.Errorf("date range too large (max 5 years)")
	}
	return p, nil
}

// ComparePeriod returns the comparison period (nil for none).
func ComparePeriod(p Period, mode string) *Period {
	switch strings.ToLower(mode) {
	case "none", "off", "false", "0":
		return nil
	case "year", "yoy":
		return &Period{p.From.AddDate(-1, 0, 0), p.To.AddDate(-1, 0, 0)}
	}
	to := p.From.AddDate(0, 0, -1)
	return &Period{to.AddDate(0, 0, -(p.Days() - 1)), to}
}

// ---------- response ----------

type part struct {
	Label  string  `json:"label"`
	Amount float64 `json:"amount"`
	Link   string  `json:"link,omitempty"`
}

type lineOut struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	Amount    float64  `json:"amount"`
	Previous  *float64 `json:"previous"`
	ChangePct *float64 `json:"change_pct"`
	Count     int      `json:"count"`
	Source    string   `json:"source"`
	Link      string   `json:"link,omitempty"`
	Note      string   `json:"note,omitempty"`
	Sources   []part   `json:"sources,omitempty"`
	Children  []part   `json:"children,omitempty"`
}

type sectionOut struct {
	Key       string    `json:"key"`
	Label     string    `json:"label"`
	Lines     []lineOut `json:"lines"`
	Total     float64   `json:"total"`
	Previous  *float64  `json:"previous"`
	ChangePct *float64  `json:"change_pct"`
}

type totalsOut struct {
	Revenue           float64  `json:"revenue"`
	CostOfSales       float64  `json:"cost_of_sales"`
	GrossProfit       float64  `json:"gross_profit"`
	GrossMarginPct    *float64 `json:"gross_margin_pct"`
	OperatingExpenses float64  `json:"operating_expenses"`
	TotalExpenses     float64  `json:"total_expenses"`
	NetProfit         float64  `json:"net_profit"`
	NetMarginPct      *float64 `json:"net_margin_pct"`
}

func pct(v float64) *float64 { v = math.Round(v*10) / 10; return &v }

func change(cur, prev int64) *float64 {
	if prev == 0 {
		if cur == 0 {
			return pct(0)
		}
		return nil
	}
	return pct(float64(cur-prev) / math.Abs(float64(prev)) * 100)
}

type ctx struct {
	in             *Input
	cur            Period
	basis          string
	tz             string
	prevByLine     map[string]int64
	incomeCatLinks map[string]string
}

func (x *ctx) major(v int64) float64 {
	return float64(v) / math.Pow10(x.in.Decimals)
}

func (x *ctx) fp(v int64) *float64 { f := x.major(v); return &f }

func (x *ctx) totalsOut(t Totals) totalsOut {
	o := totalsOut{Revenue: x.major(t.Revenue), CostOfSales: x.major(t.CostOfSales), GrossProfit: x.major(t.GrossProfit),
		OperatingExpenses: x.major(t.Expenses), TotalExpenses: x.major(t.TotalExpenses), NetProfit: x.major(t.NetProfit)}
	if t.Revenue != 0 {
		o.GrossMarginPct = pct(float64(t.GrossProfit) / float64(t.Revenue) * 100)
		o.NetMarginPct = pct(float64(t.NetProfit) / float64(t.Revenue) * 100)
	}
	return o
}

func q(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	return v.Encode()
}

// links to the pages listing the records behind a line, with the same dates.
func (x *ctx) link(source string, extra ...string) string {
	f, t := x.cur.From.Format(dayFmt), x.cur.To.Format(dayFmt)
	switch source {
	case "invoices":
		if x.basis == Cash {
			return "/invoices?" + q("paid_from", f, "paid_to", t)
		}
		return "/invoices?" + q("from", f, "to", t)
	case "pos":
		return "/pos-transactions?" + q("range", "custom", "from", f, "to", t, "status", "completed")
	case "online":
		return "/ecommerce"
	case "events":
		return "/events?scope=all"
	case "cashbook_income":
		return "/business?" + q("tab", "cashbook", "type", "income", "from", f, "to", t)
	case "cashbook_expense":
		cat := ""
		if len(extra) > 0 {
			cat = extra[0]
		}
		return "/business?" + q("tab", "cashbook", "type", "expense", "from", f, "to", t, "category", cat)
	case "bills":
		if x.basis == Cash {
			return "/bills?" + q("paid_from", f, "paid_to", t)
		}
		return "/bills?" + q("from", f, "to", t)
	case "stock_received":
		return "/suppliers?tab=orders&status=received"
	case "payroll":
		return "/hcm?tab=payruns"
	}
	return ""
}

var sourceOf = map[string]string{
	"invoices": "invoices", "pos": "pos", "online": "online", "events": "events", "cashbook_income": "cashbook_income",
	"stock_received": "stock_received", "bills_cos": "bills", "cashbook_cos": "cashbook_expense", "payroll": "payroll",
}

func (x *ctx) lineOut(l *Line, prev *Statement) lineOut {
	o := lineOut{Key: l.Key, Label: l.Label, Amount: x.major(l.Amount), Count: l.Count}
	if prev != nil {
		var pv int64
		if pl := prev.Lines[l.Key]; pl != nil {
			pv = pl.Amount
		}
		o.Previous, o.ChangePct = x.fp(pv), change(l.Amount, pv)
	}
	src := sourceOf[l.Key]
	switch {
	case l.Key == "pos":
		o.Note = fmt.Sprintf("Sales %s, refunds %s (excl. tax)", x.fmt(l.Parts["sales"]), x.fmt(-l.Parts["refunds"]))
	case l.Key == "payroll":
		o.Note = fmt.Sprintf("Gross wages. Net paid %s + %s withheld (tax, super, other deductions)", x.fmt(l.Parts["net"]), x.fmt(l.Parts["deductions"]))
	case l.Key == "invoices" && x.basis == Cash:
		o.Note = "Payments received, excluding the tax part of each payment"
	case l.Key == "invoices":
		o.Note = "Issued invoices (not draft or void), excluding tax"
	case l.Key == "stock_received":
		o.Note = "Received quantity × unit cost, for orders without a linked supplier bill"
	}
	if strings.HasPrefix(l.Key, "exp:") {
		src = ""
		for _, s := range []string{"bills", "cashbook"} {
			if v, ok := l.Parts[s]; ok {
				label, lk := "Supplier bills", x.link("bills")
				if s == "cashbook" {
					label, lk = "Cash book", x.link("cashbook_expense", x.catID(l.Label))
				}
				o.Sources = append(o.Sources, part{Label: label, Amount: x.major(v), Link: lk})
			}
		}
		if len(o.Sources) > 0 {
			o.Link = o.Sources[0].Link
			o.Source = map[bool]string{true: "bills", false: "cashbook"}[o.Sources[0].Label == "Supplier bills"]
			if len(o.Sources) > 1 {
				o.Source = "mixed"
			}
		}
	} else {
		o.Source = src
		o.Link = x.link(src)
		if l.Key == "cashbook_income" || l.Key == "cashbook_cos" || l.Key == "bills_cos" {
			names := make([]string, 0, len(l.Parts))
			for k := range l.Parts {
				names = append(names, k)
			}
			sort.Slice(names, func(i, j int) bool {
				if l.Parts[names[i]] != l.Parts[names[j]] {
					return l.Parts[names[i]] > l.Parts[names[j]]
				}
				return names[i] < names[j]
			})
			for _, n := range names {
				lk := ""
				switch l.Key {
				case "cashbook_income":
					lk = x.link("cashbook_income")
				case "cashbook_cos":
					lk = x.link("cashbook_expense", x.catID(n))
				}
				o.Children = append(o.Children, part{Label: n, Amount: x.major(l.Parts[n]), Link: lk})
			}
		}
	}
	return o
}

func (x *ctx) catID(name string) string { return x.incomeCatLinks[strings.ToLower(name)] }

func (x *ctx) fmt(v int64) string {
	return fmt.Sprintf("%s %.*f", x.in.Home, x.in.Decimals, x.major(v))
}

var sectionLabels = map[string]string{SecIncome: "Income", SecCOS: "Cost of sales", SecExpenses: "Operating expenses"}

// ProfitLoss handles GET /reports/profit-loss.
//
// Query: from, to (YYYY-MM-DD) or range (see ResolvePeriod; default
// this_month); basis=accrual|cash (default accrual); compare=previous|year|none
// (default previous); series=0 to skip the 12-month series; tz (IANA).
func (h *Handler) ProfitLoss(c *gin.Context) {
	org := orgOf(c)
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
		return
	}
	loc := reqtz.Location(c)
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	today := Day(now().In(loc))
	home, taxLabel, fyStart, orgName := OrgSettings(h.DB, org)
	cur, err := ResolvePeriod(today, c.Query("from"), c.Query("to"), c.DefaultQuery("range", "this_month"), fyStart)
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_RANGE", err.Error())
		return
	}
	basis := strings.ToLower(c.DefaultQuery("basis", Accrual))
	if basis != Cash {
		basis = Accrual
	}
	prevP := ComparePeriod(cur, c.DefaultQuery("compare", "previous"))
	withSeries := c.DefaultQuery("series", "1") != "0"

	// 12 whole months ending with the period's last month.
	endMonth := time.Date(cur.To.Year(), cur.To.Month(), 1, 0, 0, 0, 0, time.UTC)
	seriesFrom := endMonth.AddDate(0, -11, 0)
	wFrom, wTo := cur.From, cur.To
	if prevP != nil && prevP.From.Before(wFrom) {
		wFrom = prevP.From
	}
	if withSeries {
		if seriesFrom.Before(wFrom) {
			wFrom = seriesFrom
		}
		if last := endMonth.AddDate(0, 1, -1); last.After(wTo) {
			wTo = last
		}
	}

	in := Load(h.DB, org, wFrom, wTo, loc)
	in.Home, in.OrgHome, in.Decimals = home, home, currency.Decimals(home)
	var cats []struct{ ID, Name string }
	h.DB.Raw("SELECT id, name FROM smallbiz_categories WHERE organization_id = ?", org).Scan(&cats)
	x := &ctx{in: in, cur: cur, basis: basis, tz: loc.String(), incomeCatLinks: map[string]string{}}
	for _, ct := range cats {
		x.incomeCatLinks[strings.ToLower(ct.Name)] = ct.ID
	}

	st := Compute(in, cur, basis)
	var prev *Statement
	if prevP != nil {
		prev = Compute(in, *prevP, basis)
	}
	tot := st.Totals()

	sections := []sectionOut{}
	topRows := []gin.H{}
	for _, key := range []string{SecIncome, SecCOS, SecExpenses} {
		sec := sectionOut{Key: key, Label: sectionLabels[key], Lines: []lineOut{}}
		var total, ptotal int64
		seen := map[string]bool{}
		for _, l := range st.SortedLines(key) {
			seen[l.Key] = true
			total += l.Amount
			sec.Lines = append(sec.Lines, x.lineOut(l, prev))
		}
		if prev != nil {
			for _, l := range prev.SortedLines(key) {
				ptotal += l.Amount
				if !seen[l.Key] && l.Amount != 0 {
					zero := &Line{Key: l.Key, Section: l.Section, Label: l.Label, Parts: map[string]int64{}}
					for k := range l.Parts {
						zero.Parts[k] = 0
					}
					sec.Lines = append(sec.Lines, x.lineOut(zero, prev))
				}
			}
			sec.Previous, sec.ChangePct = x.fp(ptotal), change(total, ptotal)
		}
		sec.Total = x.major(total)
		sections = append(sections, sec)
		if key != SecIncome {
			for _, lo := range sec.Lines {
				if lo.Amount > 0 {
					share := 0.0
					if tot.TotalExpenses > 0 {
						share = math.Round(lo.Amount/x.major(tot.TotalExpenses)*1000) / 10
					}
					topRows = append(topRows, gin.H{"key": lo.Key, "label": lo.Label, "amount": lo.Amount, "share_pct": share, "link": lo.Link, "section": key})
				}
			}
		}
	}
	sort.SliceStable(topRows, func(i, j int) bool { return topRows[i]["amount"].(float64) > topRows[j]["amount"].(float64) })
	if len(topRows) > 5 {
		topRows = topRows[:5]
	}

	resp := gin.H{
		"organization": gin.H{"name": orgName},
		"currency":     home, "decimals": in.Decimals, "basis": basis, "tz": loc.String(),
		"period":   gin.H{"from": cur.From.Format(dayFmt), "to": cur.To.Format(dayFmt), "days": cur.Days()},
		"compare":  nil,
		"sections": sections,
		"totals":   x.totalsOut(tot),
		"previous": nil, "change_pct": nil,
		"top_expenses": topRows,
		"has_data":     len(st.Lines) > 0,
	}
	if prev != nil {
		pt := prev.Totals()
		resp["compare"] = gin.H{"from": prevP.From.Format(dayFmt), "to": prevP.To.Format(dayFmt), "days": prevP.Days()}
		resp["previous"] = x.totalsOut(pt)
		resp["change_pct"] = gin.H{"revenue": change(tot.Revenue, pt.Revenue), "cost_of_sales": change(tot.CostOfSales, pt.CostOfSales),
			"gross_profit": change(tot.GrossProfit, pt.GrossProfit), "operating_expenses": change(tot.Expenses, pt.Expenses),
			"total_expenses": change(tot.TotalExpenses, pt.TotalExpenses), "net_profit": change(tot.NetProfit, pt.NetProfit)}
		if len(prev.Lines) > 0 {
			resp["has_data"] = true
		}
	}

	// Tax memo (not part of profit)
	tax := gin.H{"label": taxLabel, "collected": x.major(st.TaxCollected), "paid": x.major(st.TaxPaid), "net": x.major(st.TaxCollected - st.TaxPaid)}
	if ex := st.Excluded[ExCashTax]; ex != nil {
		tax["payments_to_tax_office"] = x.major(ex.Amount)
	}
	if prev != nil {
		tax["previous"] = gin.H{"collected": x.major(prev.TaxCollected), "paid": x.major(prev.TaxPaid), "net": x.major(prev.TaxCollected - prev.TaxPaid)}
	}
	resp["tax"] = tax

	excluded := []gin.H{}
	for _, k := range []string{ExCashLinked, ExCashMatched, ExCashTransfer, ExCashTax, ExPOBilled, ExBillCapital, ExPOSVoided, ExInvoiceVoid} {
		if e := st.Excluded[k]; e != nil && e.Count > 0 {
			excluded = append(excluded, gin.H{"key": k, "label": excludedLabels[k], "count": e.Count, "amount": x.major(e.Amount)})
		}
	}
	resp["excluded"] = excluded

	if withSeries {
		series := []gin.H{}
		for m := seriesFrom; !m.After(endMonth); m = m.AddDate(0, 1, 0) {
			mp := Period{m, m.AddDate(0, 1, -1)}
			mt := Compute(in, mp, basis).Totals()
			series = append(series, gin.H{"month": m.Format("2006-01"), "revenue": x.major(mt.Revenue), "cost_of_sales": x.major(mt.CostOfSales),
				"operating_expenses": x.major(mt.Expenses), "expenses": x.major(mt.TotalExpenses), "gross_profit": x.major(mt.GrossProfit),
				"net_profit": x.major(mt.NetProfit), "partial": mp.To.After(today)})
		}
		resp["series"] = series
	}

	others := []gin.H{}
	for _, oc := range OtherCurrencies(in, home) {
		oin := *in
		oin.Home, oin.Decimals = oc, currency.Decimals(oc)
		os := Compute(&oin, cur, basis)
		ot := os.Totals()
		if ot.Revenue == 0 && ot.TotalExpenses == 0 {
			continue
		}
		d := math.Pow10(oin.Decimals)
		others = append(others, gin.H{"currency": oc, "revenue": float64(ot.Revenue) / d, "expenses": float64(ot.TotalExpenses) / d, "net_profit": float64(ot.NetProfit) / d})
	}
	resp["other_currencies"] = others
	resp["rules"] = Rules(basis, taxLabel)
	resp["links"] = gin.H{"report": "/reports/profit-loss?" + q("from", cur.From.Format(dayFmt), "to", cur.To.Format(dayFmt), "basis", basis)}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

var excludedLabels = map[string]string{
	ExCashLinked:   "Cash-book entries marked as already recorded elsewhere (invoice payment, bill, POS sale, pay run …)",
	ExCashMatched:  "Cash-book entries whose reference matches an invoice, bill, purchase order, pay run or POS sale",
	ExCashTransfer: "Cash-book transfers, loans and owner money",
	ExCashTax:      "Cash-book tax payments to the tax office",
	ExPOBilled:     "Stock received on purchase orders that have a supplier bill (the bill is counted)",
	ExBillCapital:  "Bill lines coded to asset, liability or equity accounts (not an expense)",
	ExPOSVoided:    "Voided POS sales",
	ExInvoiceVoid:  "Void invoices",
}

// Rules returns the plain-English rules behind the numbers.
func Rules(basis, taxLabel string) []string {
	b := "Accrual basis: income is counted when an invoice is issued and costs when a supplier bill is dated, whether or not they have been paid."
	if basis == Cash {
		b = "Cash basis: income is counted when an invoice payment is received and costs when a supplier bill is paid; unpaid invoices and bills are left out."
	}
	return []string{
		b,
		"All amounts exclude " + taxLabel + ". " + taxLabel + " collected and paid is shown separately and is not profit.",
		"Income: invoices, point-of-sale sales net of refunds (voided sales excluded), paid online store orders, paid event tickets and other money in from the cash book. Quotes, drafts and void invoices never count.",
		"Cost of sales: stock received on purchase orders (quantity × unit cost), supplier bills for stock or coded to cost-of-sales accounts, and cash-book stock / COGS categories. Production (MRP) is not counted again — its materials were counted when bought.",
		"Operating expenses: supplier bills by account, cash-book money out by category, and paid pay runs (gross wages; deductions withheld are part of wages). Employer on-costs are included only when entered as a bill or cash-book expense.",
		"Counted once: a supplier bill linked to a purchase order replaces that order's received stock. Cash-book entries marked as already recorded elsewhere, or whose reference matches an invoice, bill, purchase order, pay run or POS sale number, are left out. Transfers, loans, owner money and tax payments are not profit.",
		"Bank transactions are not a source: they are the money moving for the records above and are used for reconciliation. The Accounting ledger (journals, categorised bank lines) has its own \"Ledger P&L\", which can differ.",
		"Only the organisation's own currency is totalled; other currencies are listed separately. Point of sale, supplier bills and purchase orders are always in the organisation's currency.",
		"Dates follow your time zone. " + map[bool]string{true: "In cash basis, pay runs count on the day they were marked paid.", false: "Pay runs count on their pay date."}[basis == Cash],
	}
}
