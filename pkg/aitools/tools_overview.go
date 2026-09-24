package aitools

import (
	"net/url"
	"strings"
	"sync"
)

func init() {
	Register(Tool{
		Name:  "whoami",
		Title: "Who am I",
		Description: "Returns the signed-in user (name, email, role), their organisation, plan tier, enabled modules " +
			"and default currency. Call this first when you need to know which modules exist, which currency to use " +
			"or what the user is allowed to do.",
		InputSchema: tObj(map[string]any{}),
		ReadOnly:    true,
		Module:      "core",
		Handler:     whoami,
	})
	Register(Tool{
		Name:  "business_summary",
		Title: "Business summary",
		Description: "Headline numbers for the business over a period: revenue collected, invoiced, outstanding and overdue " +
			"receivables, bookings today/this week, top customers/products/services, low stock and recent activity. " +
			"Use for questions like 'how is the business doing', 'what's overdue', 'how much did we make this month'.",
		InputSchema: tObj(map[string]any{
			"range": tEnum("Period to summarise (default 30d)", "7d", "30d", "90d", "mtd", "qtd", "ytd", "12m"),
			"from":  tDate("Custom period start (overrides range)"),
			"to":    tDate("Custom period end"),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  businessSummary,
	})
	Register(Tool{
		Name:  "search_everything",
		Title: "Search everything",
		Description: "Searches customers, invoices & quotes, bookings (by customer), products, suppliers and CRM leads at once " +
			"and returns the top matches of each kind with ids and links. Use when you don't know what kind of record the " +
			"user means (e.g. 'find Acme', 'anything for john@example.com'). Modules not in the plan are skipped silently.",
		InputSchema: tObj(map[string]any{
			"query": tStr("Text to look for: a name, email, phone, invoice number, SKU …"),
			"limit": tInt("Max matches per kind (default 5)", 1, 20),
		}, "query"),
		ReadOnly: true,
		Module:   "core",
		Handler:  searchEverything,
	})
}

func whoami(c *Ctx, a map[string]any) (any, error) {
	me, err := c.get("/users/me", nil)
	if err != nil {
		return nil, err
	}
	u := tObjOf(me, "user")
	out := map[string]any{
		"user": map[string]any{"id": tS(u, "id"), "name": tJoinName(u), "email": tS(u, "email"), "role": tS(u, "role")},
	}
	if org, err := c.get("/organization", nil); err == nil {
		o := tObjOf(org)
		out["organization"] = tPick(o, "id", "name", "business_type", "email", "phone", "address.country:country", "business_hours.timezone:timezone")
	}
	if p, err := c.get("/plans/me", nil); err == nil {
		pm := tObjOf(p)
		out["plan"] = tPick(pm, "quote.tier:tier", "quote.tier_name:tier_name", "industry.name:industry", "status")
		out["modules"] = pm["entitlements"]
		out["default_currency"] = pm["default_currency"]
	}
	if s, err := c.get("/invoicing/settings", nil); err == nil {
		sm := tObjOf(s, "settings")
		if e, ok := tObjOf(s)["email_enabled"].(bool); ok {
			out["email_sending_configured"] = e
		}
		if cur := tS(sm, "currency"); cur != "" {
			out["invoicing_currency"] = cur
		}
		out["tax"] = tPick(sm, "tax_label:name", "default_tax_rate:default_rate", "prices_include_tax")
	}
	out["today"] = c.today()
	if c.Who.TZ != "" {
		out["timezone"] = c.Who.TZ
	}
	out["app_url"] = AppURL()
	return out, nil
}

func businessSummary(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{"limit": {"5"}}
	if r := argStr(a, "range"); r != "" {
		q.Set("range", r)
	}
	if f := argStr(a, "from"); f != "" {
		q.Set("from", f)
		if t := argStr(a, "to"); t != "" {
			q.Set("to", t)
		} else {
			q.Set("to", c.today())
		}
	}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	ov, err := c.get("/analytics/overview", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(ov)
	out := map[string]any{
		"currency": d["currency"],
		"period":   d["range"],
	}
	if k, ok := d["kpis"].(map[string]any); ok {
		kp := map[string]any{}
		for name, v := range k {
			if m, ok := v.(map[string]any); ok {
				kp[name] = tPick(m, "value", "previous", "delta_pct")
			}
		}
		out["kpis"] = kp
	}
	out["revenue_by_source"] = d["revenue_by_source"]
	aging := []map[string]any{}
	for _, b := range tList(d["aging"]) {
		if tF(b, "count") > 0 {
			aging = append(aging, tPick(b, "label", "amount", "count"))
		}
	}
	out["receivables_aging"] = aging
	if b, ok := d["bookings"].(map[string]any); ok {
		bk := tPick(b, "today", "week")
		if up := tList(b["upcoming"]); len(up) > 0 {
			up, _ = tLimitList(up, 5)
			bk["upcoming"] = up
		}
		out["bookings"] = bk
	}
	if inv, ok := d["inventory"].(map[string]any); ok {
		out["inventory"] = tPick(inv, "products", "units", "low_stock", "out_of_stock", "cost_value", "retail_value")
	}
	for _, k := range []string{"top_customers", "top_products", "top_services"} {
		if l := tList(d[k]); len(l) > 0 {
			out[k] = l
		}
	}
	if act := tList(d["activity"]); len(act) > 0 {
		act, _ = tLimitList(act, 8)
		trim := make([]map[string]any, 0, len(act))
		for _, x := range act {
			m := tPick(x, "kind", "title", "detail", "at")
			if l := tS(x, "link"); l != "" {
				m["url"] = tURL(l)
			}
			trim = append(trim, m)
		}
		out["recent_activity"] = trim
	}
	if st, err := c.get("/invoicing/stats", nil); err == nil {
		s := tObjOf(st)
		inv := tPick(s, "currency", "outstanding", "outstanding_count", "overdue", "overdue_count", "drafts_count",
			"open_quotes", "invoiced_this_month", "paid_this_month", "other_currencies")
		if top := tList(s["top_outstanding"]); len(top) > 0 {
			top, _ = tLimitList(top, 5)
			rows := []map[string]any{}
			for _, t := range top {
				r := tPick(t, "id", "number", "client_name", "balance_due", "currency", "due_date", "days_overdue")
				if id := tS(t, "id"); id != "" {
					r["url"] = tURL("/invoices/" + id)
				}
				rows = append(rows, r)
			}
			inv["top_outstanding"] = rows
		}
		out["invoicing"] = inv
	}
	return out, nil
}

func searchEverything(c *Ctx, a map[string]any) (any, error) {
	query := argStr(a, "query")
	if query == "" {
		return nil, tErr("query is required")
	}
	n := argInt(a, "limit", 5)
	if n < 1 || n > 20 {
		n = 5
	}
	type job struct {
		key string
		fn  func() ([]map[string]any, error)
	}
	jobs := []job{
		{"customers", func() ([]map[string]any, error) {
			v, err := c.get("/customers", url.Values{"search": {query}, "sort": {"recent"}})
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, cu := range tList(v, "customers") {
				out = append(out, customerBrief(cu))
			}
			return out, nil
		}},
		{"invoices_and_quotes", func() ([]map[string]any, error) {
			v, err := c.get("/invoicing/invoices", url.Values{"type": {"all"}, "q": {query}, "per_page": {"20"}})
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, inv := range tList(v, "invoices") {
				out = append(out, invoiceBrief(inv))
			}
			return out, nil
		}},
		{"products", func() ([]map[string]any, error) {
			v, err := c.get("/inventory/products", url.Values{"search": {query}})
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, p := range tList(v, "products") {
				out = append(out, productBrief(p))
			}
			return out, nil
		}},
		{"suppliers", func() ([]map[string]any, error) {
			v, err := c.get("/suppliers", url.Values{"search": {query}})
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, s := range tList(v, "suppliers") {
				m := tPick(s, "id", "name", "contact_person", "email", "phone")
				m["url"] = tURL("/suppliers")
				out = append(out, m)
			}
			return out, nil
		}},
		{"leads", func() ([]map[string]any, error) {
			v, err := c.get("/crm/leads", url.Values{"search": {query}})
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, l := range tList(v, "leads") {
				out = append(out, leadBrief(l))
			}
			return out, nil
		}},
	}
	results := make([][]map[string]any, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			if r, err := j.fn(); err == nil {
				results[i] = r
			}
		}(i, j)
	}
	wg.Wait()
	out := map[string]any{"query": query}
	total := 0
	var customerIDs []string
	for i, j := range jobs {
		r := results[i]
		if len(r) == 0 {
			continue
		}
		if j.key == "customers" {
			for _, cu := range r {
				customerIDs = append(customerIDs, tS(cu, "id"))
			}
		}
		r, _ = tLimitList(r, n)
		out[j.key] = r
		total += len(r)
	}
	// Bookings for the best-matching customers.
	if len(customerIDs) > 0 && len(customerIDs) <= 3 {
		bookings := []map[string]any{}
		for _, id := range customerIDs {
			v, err := c.get("/bookings", url.Values{"customer_id": {id}})
			if err != nil {
				break
			}
			for _, b := range tList(v, "bookings") {
				bookings = append(bookings, c.bookingBrief(b))
			}
		}
		if len(bookings) > 0 {
			bookings, _ = tLimitList(bookings, n)
			out["bookings"] = bookings
			total += len(bookings)
		}
	}
	out["total_matches"] = total
	if total == 0 {
		out["hint"] = "Nothing matched. Try a shorter or different term (e.g. just the surname or the email domain)."
	}
	return out, nil
}

// lowerContains is a case-insensitive substring test.
func lowerContains(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
