package aitools

import (
	"net/url"
	"sort"
	"strings"
)

// Synergy Wholesale tools read /expenses/synergy — the analytics built from the
// uploaded statements and hosting usage reports (Expenses → Synergy).
func init() {
	Register(Tool{
		Name:  "synergy_sites",
		Title: "Synergy hosted sites",
		Description: "Every website hosted at Synergy Wholesale (from the uploaded statements and hosting usage reports): status (active / new / " +
			"cancelled), plan, monthly cost in AUD, disk used / limit and % used, growth per month, days until the disk is full, the client it " +
			"belongs to, what the client is billed per month (home currency), margin and margin %, and flags (over_quota, near_full, fast_growth, " +
			"unbilled, no_client, below_cost). Use for 'which sites are close to full?', 'what's my margin per hosting client?', 'which sites " +
			"aren't billed?', 'how much does hosting cost me a month?'. Filter with: near_full (75%+ or over quota), unbilled, client, search.",
		InputSchema: tObj(map[string]any{
			"filter": tEnum("Only some sites", "all", "near_full", "over_quota", "unbilled", "below_cost", "cancelled", "new"),
			"client": tStr("Client name (or part of it) to focus on"),
			"search": tStr("Part of a domain name"),
			"sort":   tEnum("Sort order (default: most urgent first)", "urgency", "margin", "cost", "usage", "domain"),
			"limit":  tLimit(),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  synergySites,
	})
	Register(Tool{
		Name:  "synergy_recommendations",
		Title: "Synergy recommendations",
		Description: "Decisions for the Synergy Wholesale hosting reseller account, each with the numbers behind it: sites over quota or close to " +
			"full (with the cheapest plan that fits + 25% headroom and its price difference), fast-growing sites, plans that could be downsized, " +
			"plan-change costs, unbilled sites and what they cost, sites billed below cost, the prepaid balance and runway (top up how much by when), " +
			"priority support fees, cancellations and their savings, price changes, plus the KPIs (spend this / last month, run-rate, balance, " +
			"top-ups) and the month-on-month comparison in plain English. Use for 'what should I do about my hosting?', 'do I need to top up " +
			"Synergy?', 'what changed in September?'.",
		InputSchema: tObj(map[string]any{
			"severity": tEnum("Only this severity or worse", "danger", "warning", "info", "success"),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  synergyRecommendations,
	})
}

func synergyReport(c *Ctx) (map[string]any, error) {
	q := url.Values{}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	v, err := c.get("/expenses/synergy", q)
	if err != nil {
		return nil, err
	}
	return tObjOf(v), nil
}

func hasFlag(s map[string]any, f string) bool {
	l, _ := s["flags"].([]any)
	for _, x := range l {
		if x == f {
			return true
		}
	}
	return false
}

func synergySites(c *Ctx, a map[string]any) (any, error) {
	r, err := synergyReport(c)
	if err != nil {
		return nil, err
	}
	if b, _ := r["has_data"].(bool); !b {
		return map[string]any{"sites": []any{}, "count": 0, "note": "No Synergy statements or hosting usage reports uploaded yet — upload them in Expenses → Synergy.", "url": tURL("/expenses?tab=synergy")}, nil
	}
	filter := argStr(a, "filter")
	client := strings.ToLower(strings.TrimSpace(argStr(a, "client")))
	search := strings.ToLower(strings.TrimSpace(argStr(a, "search")))
	sites := tList(r, "sites")
	out := []map[string]any{}
	for _, s := range sites {
		st := tS(s, "status")
		switch filter {
		case "near_full":
			if !hasFlag(s, "near_full") && !hasFlag(s, "over_quota") {
				continue
			}
		case "over_quota", "unbilled", "below_cost":
			if !hasFlag(s, filter) {
				continue
			}
		case "cancelled", "new":
			if st != filter {
				continue
			}
		}
		if client != "" && !strings.Contains(strings.ToLower(tS(s, "customer_name")), client) {
			continue
		}
		if search != "" && !strings.Contains(tS(s, "domain"), search) {
			continue
		}
		out = append(out, tPick(s, "domain", "status", "plan", "monthly_cost", "domain_monthly", "total_monthly", "cost_home", "usage_mb", "limit_mb", "pct_used",
			"remaining_mb", "growth_mb_month", "trend", "days_to_full", "suggested_plan", "suggested_delta", "customer_name", "client_match", "billed", "billed_source",
			"margin", "margin_pct", "flags", "snapshot_date", "last_charge"))
	}
	urgency := func(m map[string]any) float64 {
		u := 0.0
		if hasFlag(m, "over_quota") {
			u += 1000
		}
		if hasFlag(m, "near_full") {
			u += 500
		}
		if hasFlag(m, "below_cost") {
			u += 200
		}
		if hasFlag(m, "unbilled") {
			u += 100
		}
		return u + tF(m, "pct_used")
	}
	switch argStr(a, "sort") {
	case "margin":
		sort.SliceStable(out, func(i, j int) bool { return tF(out[i], "margin") < tF(out[j], "margin") })
	case "cost":
		sort.SliceStable(out, func(i, j int) bool { return tF(out[i], "total_monthly") > tF(out[j], "total_monthly") })
	case "usage":
		sort.SliceStable(out, func(i, j int) bool { return tF(out[i], "pct_used") > tF(out[j], "pct_used") })
	case "domain":
		sort.SliceStable(out, func(i, j int) bool { return tS(out[i], "domain") < tS(out[j], "domain") })
	default:
		sort.SliceStable(out, func(i, j int) bool { return urgency(out[i]) > urgency(out[j]) })
	}
	total := len(out)
	list, trunc := tLimitList(out, argLimit(a))
	res := tResult("sites", list, total, trunc)
	res["cost_currency"] = "AUD"
	res["home_currency"] = r["home"]
	res["kpis"] = tPick(tObjOf(r, "kpis"), "run_rate", "run_rate_home", "active_sites", "billed_monthly", "margin_monthly", "unbilled", "unbilled_cost", "over_quota", "near_full")
	res["note"] = "monthly_cost / total_monthly are what Synergy charges per month in AUD (total adds domain renewals ÷ 12); billed, margin and cost_home are per month in the home currency; days_to_full is a linear forecast from the last two usage reports."
	res["url"] = tURL("/expenses?tab=synergy")
	return res, nil
}

func synergyRecommendations(c *Ctx, a map[string]any) (any, error) {
	r, err := synergyReport(c)
	if err != nil {
		return nil, err
	}
	if b, _ := r["has_data"].(bool); !b {
		return map[string]any{"decisions": []any{}, "note": "No Synergy statements or hosting usage reports uploaded yet — upload them in Expenses → Synergy.", "url": tURL("/expenses?tab=synergy")}, nil
	}
	rank := map[string]int{"danger": 0, "warning": 1, "info": 2, "success": 3}
	maxRank := 3
	if sv := argStr(a, "severity"); sv != "" {
		if v, ok := rank[sv]; ok {
			maxRank = v
		}
	}
	out := []map[string]any{}
	for _, d := range tList(r, "decisions") {
		if rank[tS(d, "severity")] > maxRank {
			continue
		}
		out = append(out, tPick(d, "type", "severity", "title", "body", "domain", "domains", "impact", "action", "numbers"))
	}
	res := map[string]any{"decisions": out, "count": len(out), "currency": "AUD", "home_currency": r["home"],
		"kpis": tPick(tObjOf(r, "kpis"), "this_month", "last_month", "ytd", "ytd_home", "run_rate", "run_rate_home", "active_sites", "new_sites", "cancelled_sites", "balance",
			"balance_at", "runway_days", "runway_date", "topup_needed", "next_charge_date", "topups_total", "topups_monthly_avg", "plan_changes_month", "billed_monthly", "margin_monthly",
			"unbilled", "unbilled_cost", "latest_month"),
		"note": "Amounts in AUD as Synergy charges them (incl. GST) unless marked _home. Expenses = debits − service credits; top-ups (Refill) are money added to the prepaid balance, never an expense.",
		"url":  tURL("/expenses?tab=synergy")}
	if cmp := tObjOf(r, "comparison"); cmp != nil && r["comparison"] != nil {
		res["comparison"] = tPick(cmp, "from", "to", "cost_from", "cost_to", "delta", "lines")
	}
	return res, nil
}
