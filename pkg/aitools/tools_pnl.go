package aitools

import (
	"net/url"
	"strings"
)

// profit_and_loss reads GET /reports/profit-loss — the same calculation as the
// dashboard, the Profit & loss report, Finance and the Business hub — so the
// assistant and MCP clients give exactly the numbers the app shows.
func init() {
	Register(Tool{
		Name:  "profit_and_loss",
		Title: "Profit & loss",
		Description: "The business's profit and loss for a period — the same figures as the dashboard and Reports → Profit & loss. " +
			"Revenue (invoices, point of sale net of refunds, online orders, event tickets, other cash-book income), cost of sales " +
			"(stock received, stock bills), operating expenses by category (bills, cash book, payroll), gross and net profit with " +
			"margins, comparison with the previous period, tax collected vs paid (memo) and the rules used to avoid double counting. " +
			"Use for 'how much profit did we make', 'what are our biggest expenses', 'revenue vs expenses this quarter'. " +
			"basis=accrual (default: invoices/bills by date) or cash (when paid). Amounts exclude tax, in the organisation's currency.",
		InputSchema: tObj(map[string]any{
			"range": tEnum("Period (default this_month). Ignored when from/to are given.", "this_month", "last_month", "this_quarter", "last_quarter",
				"ytd", "last_year", "this_fy", "last_fy", "7d", "30d", "90d", "12m"),
			"from":    tDate("Custom period start"),
			"to":      tDate("Custom period end"),
			"basis":   tEnum("accrual (default) or cash", "accrual", "cash"),
			"compare": tEnum("Compare with the previous period (default), the same period last year, or none", "previous", "year", "none"),
			"monthly": tBool("Include the last 12 months of revenue, expenses and net profit"),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  businessProfitAndLoss,
	})
}

func businessProfitAndLoss(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	for _, k := range []string{"range", "from", "to", "basis", "compare"} {
		if s := argStr(a, k); s != "" {
			q.Set(k, strings.ToLower(s))
		}
	}
	monthly, _ := argBool(a, "monthly")
	if !monthly {
		q.Set("series", "0")
	}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	v, err := c.get("/reports/profit-loss", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	out := tPick(d, "currency", "basis", "period", "compare", "totals", "previous", "change_pct", "tax", "top_expenses", "other_currencies", "excluded", "rules")
	sections := []map[string]any{}
	for _, s := range tList(d, "sections") {
		lines := []map[string]any{}
		for _, l := range tList(s, "lines") {
			m := tPick(l, "label", "amount", "previous", "change_pct", "count", "note")
			if lk := tS(l, "link"); lk != "" {
				m["url"] = tURL(lk)
			}
			lines = append(lines, m)
		}
		sections = append(sections, map[string]any{"section": tS(s, "label"), "total": tF(s, "total"), "lines": lines})
	}
	out["sections"] = sections
	if monthly {
		out["monthly"] = tList(d, "series")
	}
	if l := tObjOf(d, "links"); l != nil {
		out["url"] = tURL(tS(l, "report"))
	}
	return out, nil
}
