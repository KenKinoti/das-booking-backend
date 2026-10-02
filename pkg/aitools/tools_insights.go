package aitools

import (
	"net/url"
	"strconv"
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

// Insights 2.10: industry watch, trade & exports and the strategy brief
// (/insights/watch, /insights/trade, /insights/briefs — pkg/insights).
func init() {
	Register(Tool{
		Name:  "industry_watch_latest",
		Title: "Industry watch",
		Description: "Latest industry news and developments the business follows (stored headlines from its RSS / Atom sources: local tech and business press, " +
			"regulators, platform vendors, trade bodies), most relevant first. Each item has a title, link, source, date, a short summary taken from the feed, " +
			"topic tags (hosting & domains, cloud, AI, payments, e-commerce, cybersecurity, regulation & tax, trade & export, FX & economy, competitors, clients' " +
			"sectors), the countries it is about and a relevance score with the reasons. Use for 'what is new in my industry?', 'any regulation or tax news?', " +
			"'what are competitors doing?', 'news about Kenya / Australia'. Only headlines and summaries are stored — open the link for the article.",
		InputSchema: tObj(map[string]any{
			"topic":   tStr("Topic key: hosting, cloud, ai, payments, ecommerce, cybersecurity, regulation, trade, fx, platforms, competitors, client_sectors (or a custom topic key)"),
			"country": tStr("ISO country code the item is about, e.g. KE, AU"),
			"q":       tStr("Search in titles, summaries and source names"),
			"days":    tInt("Only items from the last N days (default 30; 0 = all stored, up to 180 days)", 0, 180),
			"saved":   tBool("Only items the team saved"),
			"unread":  tBool("Only unread items"),
			"sort":    tEnum("Order (default relevance)", "relevance", "newest"),
			"limit":   tLimit(),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  industryWatchLatest,
	})
	Register(Tool{
		Name:  "trade_exports_summary",
		Title: "Trade & exports",
		Description: "The business's foreign-trade position for a date range (default the last 12 months), in the home currency: revenue by client country with the " +
			"domestic vs export split and the same period a year earlier, revenue by invoice currency, top export clients, average invoice and days to pay by " +
			"country, open receivables in foreign currencies valued at the rate locked on each invoice and at today's rate (unrealised FX gain or loss), " +
			"realised FX on paid foreign invoices, costs paid in foreign currencies, and the natural hedge per currency (earned vs spent, net exposure, what a 5% " +
			"rate move changes). With markets=true also the target-market table: World Bank indicators, FX volatility, revenue there and an opportunity score " +
			"with a plain-English note per country. Use for 'how much do I export?', 'what is my AUD exposure?', 'which market should I grow?'.",
		InputSchema: tObj(map[string]any{
			"from":    tDate("Start date (YYYY-MM-DD); default 12 months ago; 'all' for all history"),
			"to":      tDate("End date (YYYY-MM-DD); default today"),
			"markets": tBool("Also return the target-market table with opportunity scores"),
			"limit":   tInt("Maximum countries / clients listed (default 10)", 1, 50),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  tradeExportsSummary,
	})
	Register(Tool{
		Name:  "insights_strategy_brief",
		Title: "Strategy brief",
		Description: "The monthly strategy brief: performance snapshot (revenue, costs, net profit, cash, receivables), what changed against the month before, the " +
			"export & FX position, market signals from public indicators, the top industry headlines, risks and recommended actions ranked with their rationale " +
			"and estimated value in the home currency. Returns the stored brief of the month (rule-based; version=ai for the AI-written one when it exists) or, " +
			"when none is stored, the rule-based brief computed now from the same data. Without a period it is the last complete month; period=current is the " +
			"month to date. Use for 'how did last month go?', 'what should I focus on?', 'give me my strategy brief'.",
		InputSchema: tObj(map[string]any{
			"period":  tStr("Month as YYYY-MM, or 'current' for the month to date (default: the last complete month)"),
			"version": tEnum("Which stored version to prefer (default rule)", "rule", "ai"),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  insightsStrategyBrief,
	})
}

func industryWatchLatest(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	for _, k := range []string{"topic", "country", "q"} {
		if s := strings.TrimSpace(argStr(a, k)); s != "" {
			q.Set(k, s)
		}
	}
	days := 30
	if argHas(a, "days") {
		days = argInt(a, "days", 30)
	}
	if days > 0 {
		q.Set("days", strconv.Itoa(days))
	}
	if v, ok := argBool(a, "saved"); ok && v {
		q.Set("saved", "1")
	}
	if v, ok := argBool(a, "unread"); ok && v {
		q.Set("unread", "1")
	}
	if argStr(a, "sort") == "newest" {
		q.Set("sort", "newest")
	}
	lim := argLimit(a)
	q.Set("limit", strconv.Itoa(lim))
	v, err := c.get("/insights/watch", q)
	if err != nil {
		return nil, err
	}
	l := tObjOf(tObjOf(v)["list"])
	items := []map[string]any{}
	for _, it := range tList(l["items"]) {
		m := tPick(it, "title", "link", "source", "published_at", "first_seen_at", "summary", "score:relevance", "topics", "countries", "saved")
		why := []string{}
		for _, r := range tList(it["reasons"]) {
			why = append(why, strings.TrimSpace(tS(r, "label")+": "+tS(r, "detail")))
		}
		m["why_relevant"] = why
		m["read"] = it["read_at"] != nil
		items = append(items, m)
	}
	res := map[string]any{"items": items, "count": len(items), "matching": l["total"], "stored": l["all"], "unread": l["unread"], "sources": l["feeds_enabled"],
		"sources_failing": l["feeds_failing"], "last_refresh": l["last_run"], "url": tURL("/insights?tab=watch"),
		"note": "Headlines and short summaries come from the sources' own feeds; relevance is a keyword score (topics × business type × service lines × countries)."}
	if len(items) == 0 {
		if tF(l, "feeds") == 0 {
			res["note"] = "No sources are followed yet. Add sources in Insights → Industry watch → Manage sources."
		} else {
			res["note"] = "Nothing stored matches. Try more days, another topic, or refresh the sources in Insights → Industry watch."
		}
	}
	return res, nil
}

func tradeExportsSummary(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	for _, k := range []string{"from", "to"} {
		if s := strings.TrimSpace(argStr(a, k)); s != "" {
			q.Set(k, s)
		}
	}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	v, err := c.get("/insights/trade", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	lim := argInt(a, "limit", 10)
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
	rc := tObjOf(d["receivables"])
	re := tObjOf(d["realised"])
	res := map[string]any{
		"currency": d["currency"], "home_country": d["home_country_name"], "from": d["from"], "to": d["to"], "as_of": d["today"],
		"totals": tPick(tObjOf(d["totals"]), "revenue", "domestic", "export", "export_pct", "prev_export_pct", "export_yoy_pct", "export_clients", "export_markets",
			"inferred_docs:docs_country_from_currency", "assumed_docs:docs_assumed_domestic"),
		"by_country":         clip(tList(d["countries"]), "country", "name", "domestic", "revenue", "share_pct", "yoy_pct", "invoices", "clients", "avg_invoice", "avg_days_to_pay"),
		"by_currency":        clip(tList(d["currencies"]), "currency", "amount", "home:amount_home", "share_pct", "docs:documents"),
		"top_export_clients": clip(tList(d["top_export_clients"]), "name", "customer_id", "country_name:country", "currencies", "revenue", "share_pct:share_of_export_pct", "invoices"),
		"open_foreign_receivables": map[string]any{
			"by_currency": clip(tList(rc["rows"]), "currency", "invoices", "balance", "locked_home:value_at_invoice_rate", "today_rate", "today_home:value_at_today_rate",
				"unrealised:unrealised_fx", "no_locked_rate:invoices_without_locked_rate", "overdue"),
			"value_at_invoice_rate": rc["locked_home"], "value_at_today_rate": rc["today_home"], "unrealised_fx": rc["unrealised"],
		},
		"realised_fx": map[string]any{"gain": re["gain"], "payments_counted": re["counted"], "payments_without_rate": re["skipped"],
			"by_currency": clip(tList(re["rows"]), "currency", "payments", "amount", "at_invoice_rate", "at_payment_rate", "gain"), "note": re["note"]},
		"foreign_costs": clip(tList(d["costs"]), "currency", "amount", "home:amount_home", "items:expenses", "vendors"),
		"natural_hedge": clip(tList(d["hedge"]), "currency", "in:earned", "out:spent", "net", "cover_pct", "net_home", "move_5pct_home", "summary"),
		"formulas":      d["formulas"], "notes": d["notes"], "url": tURL("/insights?tab=trade"),
	}
	if want, ok := argBool(a, "markets"); ok && want {
		mv, err := c.get("/insights/trade/markets", nil)
		if err != nil {
			return nil, err
		}
		m := tObjOf(tObjOf(mv)["markets"])
		rows := []map[string]any{}
		for _, r := range tList(m["rows"]) {
			x := tPick(r, "rank", "country", "name", "currency", "home", "score", "factors:factors_with_data", "fx_volatility_pct", "fx_basis", "revenue:revenue_12m", "share_pct", "clients", "note", "indicators")
			rows = append(rows, x)
		}
		res["target_markets"] = map[string]any{"rows": rows, "weights": m["weights"], "formula": m["formula"], "data_problems": m["errors"], "source": m["source"]}
	}
	return res, nil
}

func insightsStrategyBrief(c *Ctx, a map[string]any) (any, error) {
	period := strings.TrimSpace(argStr(a, "period"))
	want := argStr(a, "version")
	if want == "" {
		want = "rule"
	}
	q := url.Values{}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	lv, err := c.get("/insights/briefs", q)
	if err != nil {
		return nil, err
	}
	ld := tObjOf(lv)
	target := period
	switch strings.ToLower(period) {
	case "":
		target = tS(ld, "last_month")
	case "current":
		target = tS(ld, "this_month")
	}
	id, got := "", ""
	for _, b := range tList(ld["briefs"]) {
		if tS(b, "period") != target {
			continue
		}
		if by := tS(b, "generated_by"); id == "" || by == want {
			id, got = tS(b, "id"), by
		}
	}
	var b map[string]any
	if id != "" {
		v, err := c.get("/insights/briefs/"+url.PathEscape(id), nil)
		if err != nil {
			return nil, err
		}
		b = tObjOf(v)
		b["stored"] = true
	} else {
		if period != "" {
			q.Set("period", period)
		}
		v, err := c.get("/insights/briefs/preview", q)
		if err != nil {
			return nil, err
		}
		b = tObjOf(v)
	}
	ct := tObjOf(b["content"])
	res := map[string]any{
		"period": b["period"], "title": b["title"], "headline": b["headline"], "generated_by": nonEmptyStr(got, "rule"), "generated_at": b["generated_at"], "stored": b["stored"],
		"currency": ct["currency"], "from": ct["from"], "to": ct["to"], "as_of": ct["as_of"], "month_in_progress": ct["partial"],
		"snapshot": ct["snapshot"], "what_changed": ct["changes"], "export_and_fx": ct["trade"], "market_signals": ct["signals"], "industry_headlines": ct["headlines"],
		"risks": ct["risks"], "recommended_actions": ct["actions"], "actions_estimated_total": ct["action_total"], "notes": ct["notes"],
		"url": tURL("/insights?tab=brief"),
	}
	if got == "ai" {
		res["ai_text"] = b["ai_text"]
		res["ai_note"] = "AI-written from the figures in this brief — check them before acting."
	}
	if id == "" {
		res["note"] = "No brief is stored for this month yet: this one was computed now and not saved. Generate it in Insights → Strategy brief to keep it in the history."
	}
	return res, nil
}

func nonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
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
