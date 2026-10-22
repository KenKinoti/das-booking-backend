package aitools

import (
	"fmt"
	"net/url"
	"strings"
)

// Subscription tools read and act on /subscriptions (pkg/subscriptions):
// clients billed monthly / quarterly / yearly for catalogue services
// (hosting, domains, email), with reminders, admin alerts and suspension
// when unpaid. Creating or cancelling subscriptions stays in the app.
func init() {
	Register(Tool{
		Name:  "subscriptions_overview",
		Title: "Subscriptions overview",
		Description: "Client subscriptions at a glance, in the home currency: MRR and ARR (active + past-due subscriptions, before tax, at today's " +
			"exchange rates), active, past due (count and amount), suspended, churned this month, renewals in the next 30 days, and the upcoming " +
			"suspensions — who will be suspended in the next 7 days unless they pay, with the unpaid invoice, amount, suspension date, days left, " +
			"phone and whether it is suspended automatically at Synergy Wholesale — plus domains expiring in 30 days and open provider tasks. MRR is " +
			"a forecast, not revenue. Use for 'what's my MRR?', 'who do I need to call today?', 'whose hosting gets suspended this week?'.",
		InputSchema: tObj(map[string]any{}),
		ReadOnly:    true,
		Module:      "invoicing",
		Handler:     subscriptionsOverview,
	})
	Register(Tool{
		Name:  "subscriptions_list",
		Title: "Client subscriptions",
		Description: "Clients' subscriptions: number, client, services, hosted site / domain, billing period, amount per period (incl. tax) in the " +
			"subscription currency and the monthly amount in the home currency, status (active, past_due, suspended, cancelled), next billing and " +
			"due dates, the oldest unpaid invoice with days overdue and the suspension date. Filter by status or search a client, domain or service.",
		InputSchema: tObj(map[string]any{
			"status": tEnum("Only this status (open = everything not cancelled, at_risk = past due or suspended)", "open", "at_risk", "active", "past_due", "suspended", "cancelled"),
			"search": tStr("Client name, domain, service or subscription number (or part of it)"),
			"limit":  tLimit(),
		}),
		ReadOnly: true,
		Module:   "invoicing",
		Handler:  subscriptionsList,
	})
	Register(Tool{
		Name:  "subscriptions_extend",
		Title: "Give a client more time",
		Description: "Gives a client more time to pay a subscription's unpaid invoice: the suspension (and the final notice and admin alerts before it) " +
			"moves `days` later; a suspended subscription is unsuspended now (at Synergy Wholesale through the API, or with a task for other " +
			"providers) and suspended again `days` from today unless paid. Identify the subscription by subscription_id, its number (SUB-0003) or " +
			"a search (client name or domain) that matches exactly one. Returns the new suspension date. The user confirms before it runs.",
		InputSchema: tObj(map[string]any{
			"subscription_id": tStr("Subscription id"),
			"number":          tStr("Subscription number, e.g. SUB-0003"),
			"search":          tStr("Client name or domain, when the id and number are unknown (must match one subscription)"),
			"days":            tInt("More days to pay (default 7)", 1, 60),
			"note":            tStr("Why (kept on the subscription's timeline), e.g. 'Promised to pay by M-Pesa on Friday'"),
		}),
		Module:  "invoicing",
		Handler: subscriptionsExtend,
	})
	idempotentTools["subscriptions_extend"] = false
}

func subscriptionsOverview(c *Ctx, a map[string]any) (any, error) {
	v, err := c.get("/subscriptions/dashboard", nil)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	out := tPick(d, "home:currency", "today", "mrr", "arr", "mrr_by_currency", "active", "past_due", "suspended", "churned_this_month", "renewals_30d", "open_tasks",
		"unbilled_sites", "notes")
	up := []map[string]any{}
	for _, r := range tList(d["upcoming_suspensions"]) {
		m := tPick(r, "number", "customer:client", "phone", "service", "invoice", "balance", "currency", "due_date", "days_overdue", "suspend_on", "days_to_suspend", "automatic")
		m["url"] = tURL("/subscriptions/" + tS(r, "subscription_id"))
		up = append(up, m)
	}
	out["upcoming_suspensions"] = up
	ren := []map[string]any{}
	for _, r := range tList(d["renewals"]) {
		ren = append(ren, tPick(r, "number", "customer:client", "service", "start", "due_date", "amount", "currency", "invoiced", "paid"))
	}
	out["renewals"] = ren
	doms := []map[string]any{}
	for _, r := range tList(d["domains"]) {
		doms = append(doms, tPick(r, "domain", "customer:client", "expires", "days_left", "auto_renew", "number"))
	}
	out["domains_expiring"] = doms
	out["note"] = "MRR counts active and past-due subscriptions before tax at today's exchange rates; it is a forecast, not revenue. " +
		"The admins are always alerted at least 3 days before a suspension."
	out["url"] = tURL("/subscriptions")
	return out, nil
}

func subscriptionsList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if s := argStr(a, "status"); s != "" {
		q.Set("status", s)
	}
	if s := strings.TrimSpace(argStr(a, "search")); s != "" {
		q.Set("q", s)
	}
	v, err := c.get("/subscriptions", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "subscriptions")
	rows := make([]map[string]any, 0, len(list))
	for _, r := range list {
		m := tPick(r, "id", "number", "customer_name:client", "name:service", "site_domain", "domain", "supplier", "status", "period", "amount_tax:amount_per_period", "currency",
			"monthly_home", "home_currency", "next_billing_date", "next_due_date", "next_invoice_date", "paid_through", "open.invoice_number:unpaid_invoice",
			"open.balance:unpaid_balance", "open.days_overdue:days_overdue", "open.suspend_on:suspends_on", "open.no_suspend:not_suspended_this_time")
		lines := []string{}
		for _, l := range tList(r["lines"]) {
			lines = append(lines, tS(l, "description"))
		}
		m["services"] = lines
		m["url"] = tURL("/subscriptions/" + tS(r, "id"))
		rows = append(rows, m)
	}
	lim := argLimit(a)
	page, more := tLimitList(rows, lim)
	out := tResult("subscriptions", page, len(rows), more)
	out["counts"] = tDig(tData(v), "counts")
	out["url"] = tURL("/subscriptions")
	return out, nil
}

func subscriptionsExtend(c *Ctx, a map[string]any) (any, error) {
	id := strings.TrimSpace(argStr(a, "subscription_id"))
	if id == "" {
		q := strings.TrimSpace(argStr(a, "number"))
		if q == "" {
			q = strings.TrimSpace(argStr(a, "search"))
		}
		if q == "" {
			return nil, &ToolError{Msg: "Say which subscription: its number (SUB-0003), the client's name or the domain."}
		}
		v, err := c.get("/subscriptions", url.Values{"q": {q}, "status": {"open"}})
		if err != nil {
			return nil, err
		}
		list := tList(v, "subscriptions")
		exact := []map[string]any{}
		for _, r := range list {
			if strings.EqualFold(tS(r, "number"), q) {
				exact = append(exact, r)
			}
		}
		if len(exact) == 1 {
			list = exact
		}
		switch len(list) {
		case 0:
			return nil, &ToolError{Msg: fmt.Sprintf("No open subscription matches %q.", q)}
		case 1:
			id = tS(list[0], "id")
		default:
			names := []string{}
			for i, r := range list {
				if i == 6 {
					break
				}
				names = append(names, tS(r, "number")+" "+tS(r, "customer_name")+" ("+firstOf(tS(r, "site_domain"), tS(r, "domain"), tS(r, "name"))+")")
			}
			return nil, &ToolError{Msg: fmt.Sprintf("%d subscriptions match %q — which one? %s", len(list), q, strings.Join(names, "; "))}
		}
	}
	days := argInt(a, "days", 7)
	v, err := c.api("POST", "/subscriptions/"+url.PathEscape(id)+"/extend", nil, map[string]any{"days": days, "note": argStr(a, "note")})
	if err != nil {
		return nil, err
	}
	r := tObjOf(v)
	out := tPick(r, "number", "customer_name:client", "name:service", "status", "days", "suspend_on:suspends_on", "unsuspended", "open.invoice_number:unpaid_invoice",
		"open.balance:unpaid_balance", "currency")
	out["url"] = tURL("/subscriptions/" + id)
	if s := tS(r, "suspend_on"); s != "" {
		out["summary"] = fmt.Sprintf("Gave %s %d more day(s): suspended on %s unless paid.", tS(r, "customer_name"), days, s)
	} else {
		out["summary"] = fmt.Sprintf("Gave %s %d more day(s).", tS(r, "customer_name"), days)
	}
	return out, nil
}

func firstOf(v ...string) string {
	for _, x := range v {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}
