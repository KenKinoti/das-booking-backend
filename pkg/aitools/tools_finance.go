package aitools

import "net/url"

func init() {
	Register(Tool{
		Name:  "finance_summary",
		Title: "Finance summary",
		Description: "Accounting overview: bank balances, receivables and payables (bills) due/overdue, income and expenses " +
			"for the period from the general ledger.",
		InputSchema: tObj(map[string]any{}),
		ReadOnly:    true,
		Module:      "accounting",
		Handler:     financeSummary,
	})
	Register(Tool{
		Name:        "bills_list",
		Title:       "List bills",
		Description: "Lists supplier bills (accounts payable) with vendor, number, dates, total, amount due and status. Filter by status (e.g. overdue, awaiting_payment, paid, draft) or text.",
		InputSchema: tObj(map[string]any{
			"status": tStr("Status filter, e.g. draft, awaiting_approval, awaiting_payment, overdue, paid"),
			"query":  tStr("Search bill number, vendor or reference"),
			"limit":  tLimit(),
		}),
		ReadOnly: true,
		Module:   "accounting",
		Handler:  billsList,
	})
}

func financeSummary(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	v, err := c.get("/finance/summary", q)
	if err != nil {
		return nil, err
	}
	out := tObjOf(v)
	if out == nil {
		out = map[string]any{}
	}
	res := map[string]any{}
	for k, val := range out {
		// Drop long series; keep the headline numbers and short lists.
		if l, ok := val.([]any); ok && len(l) > 12 {
			res[k] = l[:12]
			continue
		}
		res[k] = val
	}
	res["url"] = tURL("/finance")
	return res, nil
}

func billsList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if s := argStr(a, "status"); s != "" {
		q.Set("status", s)
	}
	if s := argStr(a, "query"); s != "" {
		q.Set("q", s)
	}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	v, err := c.get("/finance/bills", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "bills")
	total := len(list)
	list, cut := tLimitList(list, argLimit(a))
	out := []map[string]any{}
	for _, b := range list {
		m := tPick(b, "id", "bill_number", "vendor_name", "vendor.name:vendor", "reference", "status", "currency", "total_amount", "amount_paid", "amount_due", "balance_due", "is_overdue")
		for _, k := range []string{"bill_date", "due_date"} {
			if d := tDay(tS(b, k)); d != "" {
				m[k] = d
			}
		}
		m["url"] = tURL("/bills")
		out = append(out, m)
	}
	return tResult("bills", out, total, cut), nil
}
