package aitools

import (
	"net/url"
	"strings"
)

// Expenses tools read /expenses/summary and /expenses/renewals — the same
// figures as the Expenses page (approved expenses in the home currency,
// excluding GST/VAT, which is also what the P&L counts).
func init() {
	Register(Tool{
		Name:  "expenses_summary",
		Title: "Expenses summary",
		Description: "What the business spends on suppliers and subscriptions (Synergy Wholesale, Google Workspace, other vendors): this month, " +
			"last month and year to date with change, spend by vendor and by category (hosting, domains, email & workspace, software, other), " +
			"detected recurring subscriptions with interval, last and next charge and annualised cost, price-increase / duplicate / failed-payment / " +
			"low-balance alerts, a 3-month forecast and the Synergy Wholesale account balance. Use for 'what am I spending on Google Workspace?', " +
			"'what are my subscriptions?', 'did anything go up in price?'. Pass vendor to focus on one vendor. Amounts are in the organisation's " +
			"currency excluding tax; only approved expenses count (items still in the Expenses inbox are reported separately).",
		InputSchema: tObj(map[string]any{
			"vendor": tStr("Vendor or service name to focus on, e.g. \"Google Workspace\" or \"Synergy\""),
			"from":   tDate("Start of the period for the vendor/category breakdown (default: 12 months ago)"),
			"to":     tDate("End of the period for the breakdown (default: today)"),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  expensesSummary,
	})
	Register(Tool{
		Name:  "expenses_upcoming_renewals",
		Title: "Upcoming renewals",
		Description: "Domains and hosting services (from Synergy Wholesale) and other tracked services that renew or expire soon, with expiry date, " +
			"days left, auto-renew, expected renewal cost and the client it belongs to. Use for 'which domains expire this month?', " +
			"'what renewals are coming up?'.",
		InputSchema: tObj(map[string]any{
			"within_days": tInt("Only renewals due within this many days (default 60)", 1, 730),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  expensesRenewals,
	})
}

func expensesSummary(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	for _, k := range []string{"vendor", "from", "to"} {
		if s := argStr(a, k); s != "" {
			q.Set(k, s)
		}
	}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	v, err := c.get("/expenses/summary", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	out := tPick(d, "currency", "today", "kpis", "by_vendor", "by_category", "subscriptions", "alerts", "forecast", "months", "balance", "range", "note")
	if vendor := strings.TrimSpace(argStr(a, "vendor")); vendor != "" {
		out["vendor_filter"] = vendor
	}
	out["url"] = tURL("/expenses")
	return out, nil
}

func expensesRenewals(c *Ctx, a map[string]any) (any, error) {
	within := argInt(a, "within_days", 60)
	v, err := c.get("/expenses/renewals", url.Values{"within": {itoa(within)}})
	if err != nil {
		return nil, err
	}
	list := tList(v, "renewals")
	rows := []map[string]any{}
	for _, r := range list {
		m := tPick(r, "name", "kind", "plan", "expires_on", "days_left", "auto_renew", "cost", "currency", "cost_source", "customer_name", "status")
		if dl, ok := r["days_left"].(float64); ok && dl < -45 {
			continue
		}
		rows = append(rows, m)
	}
	return map[string]any{"within_days": within, "renewals": rows, "count": len(rows), "url": tURL("/expenses?tab=renewals")}, nil
}
