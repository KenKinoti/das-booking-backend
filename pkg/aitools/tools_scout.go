package aitools

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Market scout (pkg/scout, /insights/scout/…): external opportunities found
// on the web — tenders, grants, partnerships, leads, new markets, products to
// add — ranked by fit, with their sources. Reading is free; a scan searches
// the web with the platform's Anthropic key and costs money, so it is marked
// destructive: the assistant always asks before it runs (even with
// auto-approve) and MCP clients see destructiveHint.
func init() {
	Register(Tool{
		Name:  "market_scout_opportunities",
		Title: "Market scout opportunities",
		Description: "External business opportunities the market scout found on the web for this business — public tenders, grants and calls for " +
			"proposals, partnership / reseller programmes, specific organisations to approach (leads), new markets and products or services to add — " +
			"ranked by a 0–100 fit score against the business profile (what it sells, where its customers are, the owner's focus). Each has the buyer, " +
			"estimated value and currency, deadline and days left, country, effort, confidence, why it fits, a next step and the source links it was " +
			"found on (always cite them). Also returns this month's scan spend against the budget and the latest market signals. Use for 'any tenders or " +
			"grants for us?', 'what opportunities did the scout find?', 'what closes this week?'. These are leads to check, not verified facts. " +
			"Different from insights_opportunities, which reads the business's own data.",
		InputSchema: tObj(map[string]any{
			"status":   tEnum("Which cards (default active = new + saved)", "active", "new", "saved", "pursued", "dismissed", "expired", "all"),
			"type":     tEnum("Only one kind", "tender", "grant", "partnership", "lead", "new_market", "product"),
			"country":  tStr("ISO country code, e.g. KE, AU"),
			"deadline": tEnum("Only deadlines within the next 7 / 30 / 90 days, or none = no fixed deadline", "7", "30", "90", "none"),
			"q":        tStr("Search in titles, summaries, buyers and notes"),
			"sort":     tEnum("Order (default fit)", "fit", "deadline", "newest", "value"),
			"signals":  tBool("Also return the latest market signals by country (default true)"),
			"limit":    tLimit(),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  marketScoutOpportunities,
	})
	Register(Tool{
		Name:  "market_scout_scan",
		Title: "Run a market scout scan",
		Description: "Runs a market scout scan now: Claude searches the web (Anthropic web search, paid per search plus tokens with the platform's " +
			"API key) for new opportunities matching the business profile and adds them to Insights → Market scout. COSTS MONEY — about US$0.10–0.50 " +
			"for a light scan (up to the daily search limit) and more for a deep scan (up to the weekly limit); it is refused when the monthly budget " +
			"is reached. Always tell the user it costs money and get their confirmation first. Only works when the owner switched the scout on. Waits " +
			"up to ~50 seconds and returns what was found; if the scan is still running, check back with market_scout_opportunities.",
		InputSchema: tObj(map[string]any{
			"kind": tEnum("light = the daily search limit (default), deep = the weekly search limit", "light", "deep"),
		}),
		Destructive: true,
		Module:      "core",
		Handler:     marketScoutScan,
	})
}

func scoutCard(it map[string]any) map[string]any {
	m := tPick(it, "id", "title", "type", "buyer", "summary", "why_it_fits", "est_value", "currency", "value_home", "deadline", "deadline_text", "country",
		"location", "effort", "confidence", "next_step", "fit", "status", "notes", "remind_on", "crm_kind", "first_seen_at")
	if d := tS(it, "deadline"); len(d) >= 10 {
		m["deadline"] = d[:10]
	}
	srcs := []map[string]any{}
	for _, s := range tList(it["sources"]) {
		srcs = append(srcs, tPick(s, "title", "url"))
	}
	m["sources"] = srcs
	why := []string{}
	for _, r := range tList(it["fit_reasons"]) {
		why = append(why, fmt.Sprintf("%s (%+d)", tS(r, "label"), int(tF(r, "points"))))
	}
	m["fit_reasons"] = why
	m["url"] = tURL("/insights?tab=scout&opp=" + tS(it, "id"))
	return m
}

func marketScoutOpportunities(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	for _, k := range []string{"status", "type", "country", "deadline", "q", "sort"} {
		if s := strings.TrimSpace(argStr(a, k)); s != "" {
			q.Set(k, s)
		}
	}
	q.Set("limit", strconv.Itoa(argLimit(a)))
	v, err := c.get("/insights/scout/opportunities", q)
	if err != nil {
		return nil, err
	}
	feed := tObjOf(tObjOf(v)["feed"])
	items := []map[string]any{}
	for _, it := range tList(feed["items"]) {
		items = append(items, scoutCard(it))
	}
	res := map[string]any{"items": items, "count": len(items), "matching": feed["total"], "by_status": feed["statuses"], "home_currency": feed["currency"],
		"today": feed["today"], "url": tURL("/insights?tab=scout"),
		"note": "Found on the web by the market scout; every item cites the pages it came from. Fit is a rule-based 0–100 score against the business profile. Check the source before acting."}
	st, err := c.get("/insights/scout", nil)
	if err == nil {
		s := tObjOf(st)
		set := tObjOf(s["settings"])
		res["scout"] = map[string]any{"enabled": set["enabled"], "spent_this_month_usd": s["spent_usd"], "monthly_budget_usd": set["monthly_budget_usd"],
			"budget_reached": s["budget_reached"], "model": s["model"], "next_scans": s["next"]}
		if lr := tObjOf(s["last_run"]); lr != nil {
			res["last_scan"] = tPick(lr, "kind", "status", "message", "started_at", "searches", "cost_usd")
		}
		if set["enabled"] != true && len(items) == 0 {
			res["note"] = "The market scout is switched off. A super admin can switch it on in Insights → Market scout → Set up (it shows the monthly cost first)."
		}
	}
	if b, ok := argBool(a, "signals"); !ok || b {
		if mv, err := c.get("/insights/scout/markets", url.Values{"days": {"60"}}); err == nil {
			groups := []map[string]any{}
			for i, g := range tList(tObjOf(mv)["markets"]) {
				if i == 6 {
					break
				}
				sigs := []map[string]any{}
				for j, sg := range tList(g["signals"]) {
					if j == 3 {
						break
					}
					x := tPick(sg, "sector", "signal", "so_what")
					src := []string{}
					for _, s := range tList(sg["sources"]) {
						src = append(src, tS(s, "url"))
					}
					x["sources"] = src
					sigs = append(sigs, x)
				}
				groups = append(groups, map[string]any{"market": g["name"], "country": g["country"], "signals": sigs})
			}
			res["market_signals"] = groups
		}
	}
	return res, nil
}

func marketScoutScan(c *Ctx, a map[string]any) (any, error) {
	kind := argStr(a, "kind")
	if kind != "deep" {
		kind = "light"
	}
	v, err := c.api("POST", "/insights/scout/scan", nil, map[string]any{"kind": kind, "wait_seconds": 50})
	if err != nil {
		return nil, err
	}
	run := tPick(tObjOf(v), "id", "kind", "status", "message", "searches", "max_searches", "queries", "input_tokens", "output_tokens", "cost_usd",
		"kept", "updated", "unverified", "expired", "duplicates", "filtered", "signals_kept")
	res := map[string]any{"scan": run, "url": tURL("/insights?tab=scout")}
	switch tS(run, "status") {
	case "running":
		res["note"] = "Still searching. It usually finishes within two minutes — then list the results with market_scout_opportunities."
	case "ok":
		if n := tF(run, "kept"); n > 0 {
			if l, err := c.get("/insights/scout/opportunities", url.Values{"sort": {"newest"}, "limit": {strconv.Itoa(int(n))}}); err == nil {
				items := []map[string]any{}
				for _, it := range tList(tObjOf(tObjOf(l)["feed"])["items"]) {
					items = append(items, scoutCard(it))
				}
				res["new_items"] = items
			}
		}
	case "budget":
		res["note"] = "Not run: the monthly market scout budget is reached. A super admin can raise it in Insights → Market scout → Set up."
	}
	return res, nil
}
