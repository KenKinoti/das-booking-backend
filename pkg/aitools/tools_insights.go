package aitools

import (
	"net/url"
	"strings"
)

// Insights tools read /insights/opportunities and /analytics/revenue — the
// same calculations as the Insights and Analytics pages (home currency,
// excluding tax, foreign documents converted at their locked rate).
func init() {
	Register(Tool{
		Name:  "insights_opportunities",
		Title: "Business opportunities",
		Description: "Actionable internal business opportunities with an estimated value in the home currency and how it was estimated: hosting plan " +
			"upgrades for sites near their disk limit, hosting clients without Google Workspace, websites without a maintenance plan, lapsed and " +
			"quiet clients to win back, cancelled hosting, sites billed below cost or below the usual rate for their plan, hosting that is not billed, " +
			"overdue invoices by client (collection risk), open quotes with the 12-month win rate (pipeline), domain / hosting renewals to bill, and " +
			"client concentration risk. Each has the clients / sites behind it. Use for 'where can I make more money?', 'who should I upsell?', " +
			"'which clients stopped buying?', 'what is in my pipeline?'. Filter with type.",
		InputSchema: tObj(map[string]any{
			"type":  tEnum("Only one kind of opportunity", "upsell", "winback", "pricing", "unbilled", "collection", "pipeline", "renewal", "risk"),
			"limit": tInt("Maximum items listed per opportunity (default 10)", 1, 25),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  insightsOpportunities,
	})
	Register(Tool{
		Name:  "analytics_revenue_breakdown",
		Title: "Revenue breakdown",
		Description: "Revenue (invoices minus credit notes, excluding tax, by issue date, in the home currency) for a date range: total with the same " +
			"period a year earlier (YoY), by month with YoY, by client (share of revenue, rank) and by service line (web development, hosting, domains, " +
			"Google Workspace & email, maintenance, software & ERP, consulting — classified from invoice line descriptions with the editable keyword " +
			"mapping). Narrow to one client (customer id) or service line, or one month, to get the invoice lines behind the figure. Use for 'how much " +
			"did hosting bring in this year?', 'top clients last 12 months', 'revenue by month vs last year', 'what did client X buy?'.",
		InputSchema: tObj(map[string]any{
			"from":     tDate("Start date (YYYY-MM-DD); default 24 months ago; 'all' for all history"),
			"to":       tDate("End date (YYYY-MM-DD); default today"),
			"client":   tStr("Customer id to focus on (from customers_search)"),
			"line":     tEnum("Service line to focus on", "web_dev", "hosting", "domains", "workspace", "maintenance", "software", "consulting", "other"),
			"month":    tStr("One month YYYY-MM for the invoice lines behind it"),
			"currency": tStr("Only documents in this currency (ISO code), reported in it"),
			"limit":    tLimit(),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  analyticsRevenueBreakdown,
	})
}

func insightsOpportunities(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	v, err := c.get("/insights/opportunities", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	typ := strings.TrimSpace(argStr(a, "type"))
	lim := argInt(a, "limit", 10)
	out := []map[string]any{}
	for _, o := range tList(d["opportunities"]) {
		if typ != "" && tS(o, "type") != typ {
			continue
		}
		items := tList(o["items"])
		if len(items) > lim {
			items = items[:lim]
		}
		short := []map[string]any{}
		for _, it := range items {
			short = append(short, tPick(it, "name", "customer_id", "domain", "detail", "value"))
		}
		m := tPick(o, "key", "type", "title", "summary", "severity", "value", "value_kind", "value_basis", "count", "action_text")
		m["items"] = short
		if l := tS(o, "link"); l != "" {
			m["url"] = tURL(l)
		}
		out = append(out, m)
	}
	res := map[string]any{"currency": d["currency"], "total_estimated_value": d["total_value"], "opportunities": out, "count": len(out),
		"pipeline": d["pipeline"], "url": tURL("/insights"),
		"note": "Values are estimates (per year unless value_kind is one_off or at_risk); value_basis explains each."}
	if cal := tList(d["renewal_calendar"]); len(cal) > 0 {
		if len(cal) > 15 {
			cal = cal[:15]
		}
		res["renewals_next_90_days"] = cal
	}
	return res, nil
}

func analyticsRevenueBreakdown(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	copyQ := func(k string) {
		if s := strings.TrimSpace(argStr(a, k)); s != "" {
			q.Set(k, s)
		}
	}
	for _, k := range []string{"from", "to", "client", "line", "month", "currency"} {
		copyQ(k)
	}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	v, err := c.get("/analytics/revenue", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	lim := argLimit(a)
	clip := func(l []map[string]any, keys ...string) []map[string]any {
		out := []map[string]any{}
		for i, x := range l {
			if i >= lim {
				break
			}
			out = append(out, tPick(x, keys...))
		}
		return out
	}
	conv, _ := d["conversion"].(map[string]any)
	res := map[string]any{
		"from": d["from"], "to": d["to"], "currency": tDig(conv, "currency"), "conversion_note": tDig(conv, "note"),
		"total": d["total"], "previous_year": d["prev"], "yoy_pct": d["yoy_pct"], "documents": d["docs"], "clients": d["clients"], "average_invoice": d["avg_invoice"],
		"by_month":  clip(tList(d["months"]), "month", "revenue", "prev_year", "yoy_pct"),
		"by_client": clip(tList(d["by_client"]), "name", "customer_id", "revenue", "share_pct", "prev", "yoy_pct", "rank"),
		"by_line":   clip(tList(d["by_line"]), "key:line", "name", "revenue", "share_pct", "prev", "yoy_pct"),
		"note":      d["note"],
		"url":       tURL("/analytics?view=revenue"),
	}
	// by_month: keep the most recent months when clipped.
	if ms := tList(d["months"]); len(ms) > lim {
		res["by_month"] = clipTail(ms, lim, "month", "revenue", "prev_year", "yoy_pct")
	}
	if ls := tList(d["lines"]); len(ls) > 0 {
		res["invoice_lines"] = clip(ls, "number", "date", "client", "description", "line", "currency", "amount", "amount_home", "invoice_id")
		res["invoice_lines_total"] = len(ls)
	}
	return res, nil
}

func clipTail(l []map[string]any, n int, keys ...string) []map[string]any {
	if len(l) > n {
		l = l[len(l)-n:]
	}
	out := []map[string]any{}
	for _, x := range l {
		out = append(out, tPick(x, keys...))
	}
	return out
}
