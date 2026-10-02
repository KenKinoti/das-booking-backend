package aitools

import (
	"net/url"
	"strings"
)

// Supplier price tools read /finance/supplier-prices — the price lists of
// wholesale suppliers, the self-hosted server and competitors (Finance →
// Supplier prices) — and run the domain + hosting combo comparison.
func init() {
	Register(Tool{
		Name:  "supplier_prices_list",
		Title: "Supplier price lists",
		Description: "Prices on the supplier price lists (Finance → Supplier prices): hosting plans, domain registration / renewal / transfer per TLD, SSL and " +
			"mailboxes from wholesale suppliers (e.g. Synergy Wholesale in AUD, hosting.com in USD), the self-hosted server and competitors (market retail " +
			"benchmarks). Each price has its currency, billing period (month, year, 2y, 3y, one_off), introductory vs regular (renewal) price, setup fee, " +
			"disk / sites / mailboxes, the date it was checked (as_of) and whether it is stale (older than 90 days: check it before quoting). Use for " +
			"'what does Synergy charge for .co.ke?', 'which prices need checking?', 'list hosting.com's plans'. Filter with supplier, kind, tld, search, stale_only.",
		InputSchema: tObj(map[string]any{
			"supplier":   tStr("Supplier name (or part of it)"),
			"kind":       tEnum("Only this kind of price", "hosting_plan", "domain_register", "domain_renew", "domain_transfer", "ssl", "email_mailbox", "other"),
			"tld":        tStr("Domain ending, e.g. .co.ke"),
			"search":     tStr("Text to find in the plan / product name or notes"),
			"stale_only": tBool("Only prices older than 90 days (or with no date)"),
			"limit":      tLimit(),
		}),
		ReadOnly: true,
		Module:   "accounting",
		Handler:  supplierPricesList,
	})
	Register(Tool{
		Name:  "supplier_prices_compare",
		Title: "Compare domain + hosting costs",
		Description: "Compares what a domain + hosting combo costs from each supplier on the price lists and finds the cheapest mix (domain from one " +
			"supplier, hosting from another). For the TLD, the disk space needed and a 1- or 2-year term it returns per supplier: domain year 1 and " +
			"renewal, the hosting plan and its monthly price, setup fees, the first-year total, the ongoing yearly cost and the term total — in the " +
			"supplier's currency and converted to the home currency with the rate, its source and date — the cheapest option, the option and mix that are cheapest to renew, 'year 1 cheap, renews at …' " +
			"promo traps, stale prices, and competitors' retail prices for the same combo as a market benchmark (never options to buy). With " +
			"margin_percent it also suggests a yearly and monthly retail price (payment fees and tax included, rounded to a friendly price) with the " +
			"margin and the margin if exchange rates move ±10%. Use for 'what's the cheapest way to supply a .co.ke domain with 2 GB hosting?', " +
			"'what should I charge for starter hosting + a .com domain?'. Omit tld for hosting only.",
		InputSchema: tObj(map[string]any{
			"tld":            tStr("Domain ending, e.g. .co.ke (omit for hosting only)"),
			"disk_gb":        tNum("Disk space the customer needs in GB (default: the starter tier)"),
			"term":           tInt("Years to compare over (1 or 2; default 1)", 1, 2),
			"tier":           tEnum("Hosting tier instead of disk_gb", "starter", "standard", "business"),
			"sites":          tInt("Websites the customer's plan must allow (default 1)", 1, 1000),
			"include_ssl":    tBool("Add an SSL certificate"),
			"mailboxes":      tInt("Mailboxes needed (default 0)", 0, 1000),
			"margin_percent": tNum("Target margin % — adds suggested retail prices for the cheapest option"),
		}),
		ReadOnly: true,
		Module:   "accounting",
		Handler:  supplierPricesCompare,
	})
}

func supplierPricesList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{"active": {"1"}}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	if k := argStr(a, "kind"); k != "" {
		q.Set("kind", k)
	}
	if s := argStr(a, "search"); s != "" {
		q.Set("q", s)
	}
	if b, _ := argBool(a, "stale_only"); b {
		q.Set("stale", "1")
	}
	v, err := c.get("/finance/supplier-prices/items", q)
	if err != nil {
		return nil, err
	}
	supplier := strings.ToLower(argStr(a, "supplier"))
	tld := strings.ToLower(strings.TrimPrefix(argStr(a, "tld"), "."))
	out := []map[string]any{}
	stale := 0
	for _, it := range tList(v, "items") {
		if supplier != "" && !strings.Contains(strings.ToLower(tS(it, "supplier_name")), supplier) {
			continue
		}
		if tld != "" && strings.TrimPrefix(tS(it, "tld"), ".") != tld {
			continue
		}
		m := tPick(it, "supplier_name:supplier", "supplier_type", "kind", "name", "tld", "period", "price", "currency", "regular_price", "setup_fee",
			"disk_gb", "sites", "email_accounts", "bandwidth", "source_url", "notes", "prev_price")
		m["price"] = tF(it, "price")
		m["as_of"] = tDay(tS(it, "as_of"))
		if d, ok := it["age_days"].(float64); ok {
			m["age_days"] = d
		}
		if b, _ := it["stale"].(bool); b {
			m["stale"] = true
			stale++
		}
		out = append(out, m)
	}
	total := len(out)
	list, trunc := tLimitList(out, argLimit(a))
	res := tResult("prices", list, total, trunc)
	res["stale"] = stale
	res["note"] = "price is per billing period in the price's own currency; with regular_price set, price is an introductory offer for the first period and regular_price what it renews at; -1 for disk_gb, sites or email_accounts means unlimited; competitor suppliers are market retail prices, not suppliers to buy from; stale = checked more than 90 days ago."
	res["url"] = tURL("/supplier-prices")
	return res, nil
}

func pbPart(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	m := tPick(p, "supplier_name:supplier", "name", "period", "currency", "price", "regular", "monthly", "setup", "first_year", "ongoing:renewal_per_year", "term_total",
		"first_year_home", "ongoing_home:renewal_per_year_home", "note", "as_of")
	for _, k := range []string{"promo", "included", "assumed", "stale"} {
		if b, _ := p[k].(bool); b {
			m[k] = true
		}
	}
	if n := tF(p, "shared_by"); n > 1 {
		m["shared_by_sites"] = n
	}
	return m
}

func pbOption(o map[string]any) map[string]any {
	m := tPick(o, "key", "label", "supplier_name:supplier", "supplier_type", "currency", "missing", "reasons")
	m["complete"], _ = o["complete"].(bool)
	for _, k := range []string{"domain", "hosting", "ssl", "email"} {
		if p, ok := o[k].(map[string]any); ok {
			m[k] = pbPart(p)
		}
	}
	if tS(o, "currency") != "" {
		m["first_year"], m["ongoing_yearly"], m["term_total"], m["setup"] = tF(o, "first_year"), tF(o, "ongoing"), tF(o, "term_total"), tF(o, "setup")
	}
	m["first_year_home"], m["ongoing_yearly_home"], m["term_total_home"], m["setup_home"] = tF(o, "first_year_home"), tF(o, "ongoing_home"), tF(o, "term_total_home"), tF(o, "setup_home")
	flags := []string{}
	for _, f := range tList(o["flags"]) {
		flags = append(flags, tS(f, "message"))
	}
	if len(flags) > 0 {
		m["flags"] = flags
	}
	for _, k := range []string{"promo_trap", "cheapest", "lowest_ongoing", "same_supplier"} {
		if b, _ := o[k].(bool); b {
			m[k] = true
		}
	}
	if v := tF(o, "saving_home"); v > 0 {
		m["saving_vs_best_single_supplier_home"] = v
	}
	if v := tF(o, "saving_ongoing_home"); v > 0 {
		m["saving_per_year_vs_best_single_supplier_home"] = v
	}
	return m
}

func supplierPricesCompare(c *Ctx, a map[string]any) (any, error) {
	in := map[string]any{"term_years": argInt(a, "term", 1), "tlds": []string{}}
	tld := argStr(a, "tld")
	if tld != "" {
		in["tlds"] = []string{tld}
	}
	if d, ok := argNum(a, "disk_gb"); ok && d > 0 {
		in["min_disk_gb"] = d
	} else if t := argStr(a, "tier"); t != "" {
		in["tier"] = t
	} else {
		in["tier"] = "starter"
	}
	if n := argInt(a, "sites", 0); n > 0 {
		in["min_sites"] = n
	}
	if b, _ := argBool(a, "include_ssl"); b {
		in["include_ssl"] = true
	}
	if n := argInt(a, "mailboxes", 0); n > 0 {
		in["mailboxes"] = n
	}
	q := url.Values{}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	v, err := c.api("POST", "/finance/supplier-prices/compare", q, in)
	if err != nil {
		return nil, err
	}
	r := tObjOf(v)
	combos := tList(r["combos"])
	if len(combos) == 0 {
		return nil, tErr("Nothing to compare — give a tld or a hosting need")
	}
	combo := combos[0]
	res := map[string]any{"home_currency": r["home"], "combo": combo["label"], "need": r["need"], "term_years": argInt(a, "term", 1), "today": r["today"], "url": tURL("/supplier-prices")}
	rates := []map[string]any{}
	for _, ri := range tList(r["rates"]) {
		m := tPick(ri, "currency", "rate", "source", "provider", "warning")
		m["as_of"] = tDay(tS(ri, "as_of"))
		rates = append(rates, m)
	}
	res["rates_to_home"] = rates
	opts := []map[string]any{}
	for _, o := range tList(combo["options"]) {
		opts = append(opts, pbOption(o))
	}
	res["options"] = opts
	if m, ok := combo["mix"].(map[string]any); ok {
		res["cheapest_mix"] = pbOption(m)
	}
	// The renewal-optimised mix: the domain that is cheapest to renew + the
	// hosting that costs least per year once introductory prices have ended.
	if m, ok := combo["mix_ongoing"].(map[string]any); ok {
		res["cheapest_to_renew_mix"] = pbOption(m)
	}
	best := tS(combo, "best")
	res["cheapest"] = best
	if best == "" {
		res["cheapest"] = nil
	}
	if bo := tS(combo, "best_ongoing"); bo != "" {
		res["cheapest_to_renew"] = bo
	}
	if n := tS(combo, "mix_note"); n != "" {
		res["mix_note"] = n
	}
	// When the cheapest option is only cheap for an introductory year, the
	// option that is cheapest to renew is the one to price on.
	priceKey := best
	for _, o := range append(tList(combo["options"]), tObjOf(combo, "mix")) {
		if tS(o, "key") == best {
			if trap, _ := o["promo_trap"].(bool); trap && tS(combo, "best_ongoing") != "" {
				priceKey = tS(combo, "best_ongoing")
			}
		}
	}
	bench := []map[string]any{}
	for _, o := range tList(combo["benchmarks"]) {
		b := tPick(o, "supplier_name:competitor", "missing")
		b["complete"], _ = o["complete"].(bool)
		b["first_year_home"], b["renewal_per_year_home"] = tF(o, "first_year_home"), tF(o, "ongoing_home")
		bench = append(bench, b)
	}
	res["competitors"] = bench
	if m, ok := combo["market"].(map[string]any); ok {
		res["market"] = m
	}
	if w := r["warnings"]; !tEmpty(w) {
		res["warnings"] = w
	}
	res["note"] = "Amounts ending in _home are in the home currency at the listed rates; the others are in the supplier's currency. first_year includes setup fees; " +
		"ongoing_yearly is a year at the regular (renewal) prices; cheapest is by term_total_home, cheapest_to_renew by ongoing_yearly_home (cheapest_to_renew_mix, when present, pairs the cheapest-renewal domain with the cheapest-ongoing hosting from two suppliers); retail prices are for cheapest, or for cheapest_to_renew when the cheapest is an introductory-price trap. Competitors are market retail prices for the same combo (a benchmark), not suppliers to buy from. " +
		"Suppliers with something missing are listed but never the cheapest."

	if pct, ok := argNum(a, "margin_percent"); ok && best != "" {
		ctxv, err := c.get("/finance/supplier-prices/context", nil)
		if err != nil {
			return nil, err
		}
		cx := tObjOf(ctxv)
		home := tS(cx, "home")
		market := func(cur string, tax float64, incl bool) map[string]any {
			m := map[string]any{"currency": cur, "tax_rate": tax, "prices_include_tax": incl, "rounding": "friendly"}
			if f, ok := tDig(cx, "fees", cur).(map[string]any); ok {
				m["fee_percent"], m["fee_fixed"], m["fee_label"] = tF(f, "percent"), tF(f, "fixed"), tS(f, "label")
			}
			return m
		}
		incl, _ := cx["prices_include_tax"].(bool)
		markets := []map[string]any{market(home, tF(cx, "tax_rate"), incl)}
		if sc := tS(cx, "second_currency"); sc != "" && sc != home {
			markets = append(markets, market(sc, 0, false))
		}
		rv, err := c.api("POST", "/finance/supplier-prices/retail", q, map[string]any{"compare": in, "tld": tS(combo, "tld"), "key": priceKey,
			"retail": map[string]any{"mode": "margin", "percent": pct, "basis": "higher", "markets": markets}})
		if err != nil {
			return nil, err
		}
		rr := tObjOf(rv)
		retail := map[string]any{"for": rr["label"], "target_margin_percent": pct, "cost_basis": rr["basis_used"], "cost_per_year_home": rr["cost_home"], "cost_note": rr["cost_note"]}
		prices := []map[string]any{}
		for _, m := range tList(rr["markets"]) {
			p := map[string]any{"currency": tDig(m, "market", "currency"), "tax_rate": tDig(m, "market", "tax_rate"), "prices_include_tax": tDig(m, "market", "prices_include_tax"),
				"payment_fee_percent": tDig(m, "market", "fee_percent"), "cost_per_year": m["cost"]}
			if e := tS(m, "error"); e != "" {
				p["error"] = e
			}
			for _, k := range []string{"yearly", "monthly"} {
				if pp, ok := m[k].(map[string]any); ok {
					p[k] = tPick(pp, "price", "net", "tax", "gross", "fee", "cost", "profit", "margin_pct", "markup_pct", "profit_home")
				}
			}
			sens := []map[string]any{}
			for _, s := range tList(m["sensitivity"]) {
				sens = append(sens, map[string]any{"fx_move_percent": s["shock_pct"], "margin_pct": s["margin_pct"], "profit": s["profit"]})
			}
			p["if_exchange_rates_move"] = sens
			pos := []string{}
			for _, x := range tList(m["positions"]) {
				pos = append(pos, tS(x, "text"))
			}
			if len(pos) > 0 {
				p["vs_competitors"] = pos
			}
			prices = append(prices, p)
		}
		retail["prices"] = prices
		if w := rr["warnings"]; !tEmpty(w) {
			retail["warnings"] = w
		}
		retail["note"] = "price is the figure to quote (tax included when prices_include_tax), rounded up to a friendly price; profit = net − payment fee − cost; margin_pct = profit ÷ net."
		res["retail"] = retail
	}
	return res, nil
}
