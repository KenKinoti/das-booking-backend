package aitools

import (
	"net/url"
)

var leadStatuses = []string{"new", "contacted", "qualified", "unqualified", "converted"}
var dealStages = []string{"prospecting", "qualification", "proposal", "negotiation", "closed_won", "closed_lost"}

func init() {
	leadProps := func() map[string]any {
		return map[string]any{
			"first_name":      tStr("First name"),
			"last_name":       tStr("Last name"),
			"email":           tStr("Email"),
			"phone":           tStr("Phone"),
			"company":         tStr("Company"),
			"job_title":       tStr("Job title"),
			"source":          tStr("Where the lead came from, e.g. website, referral, social_media, event"),
			"rating":          tEnum("How promising", "hot", "warm", "cold"),
			"estimated_value": tNum("Estimated deal value"),
			"notes":           tStr("Notes"),
		}
	}
	Register(Tool{
		Name:  "leads_list",
		Title: "List leads",
		Description: "Lists CRM leads (prospects) filtered by status, rating, source or search text. Returns name, company, " +
			"contact details, status, rating, estimated value and link.",
		InputSchema: tObj(map[string]any{
			"status": tEnum("Lead status", leadStatuses...),
			"rating": tEnum("Rating", "hot", "warm", "cold"),
			"source": tStr("Source"),
			"query":  tStr("Search name, email or company"),
			"limit":  tLimit(),
		}),
		ReadOnly: true,
		Module:   "crm",
		Handler:  leadsList,
	})
	cp := leadProps()
	cp["status"] = tEnum("Initial status (default new)", "new", "contacted", "qualified")
	Register(Tool{
		Name:        "leads_create",
		Title:       "Create lead",
		Description: "Adds a new CRM lead (prospect). first_name is required. Returns the lead with id and link.",
		InputSchema: tObj(cp, "first_name"),
		Module:      "crm",
		Handler:     leadsCreate,
	})
	up := leadProps()
	up["lead_id"] = tStr("Lead id")
	up["status"] = tEnum("New status", "new", "contacted", "qualified", "unqualified")
	Register(Tool{
		Name:  "leads_update",
		Title: "Update lead",
		Description: "Updates a lead's status (e.g. contacted, qualified, unqualified), rating, value, notes or details. " +
			"Only fields you pass change.",
		InputSchema: tObj(up, "lead_id"),
		Module:      "crm",
		Handler:     leadsUpdate,
	})
	Register(Tool{
		Name:  "deals_list",
		Title: "List deals",
		Description: "Lists sales pipeline deals (opportunities), optionally for one stage, with value, probability, " +
			"expected close date and totals per stage.",
		InputSchema: tObj(map[string]any{
			"stage": tEnum("Only this stage", dealStages...),
			"limit": tLimit(),
		}),
		ReadOnly: true,
		Module:   "crm",
		Handler:  dealsList,
	})
	Register(Tool{
		Name:  "deals_create",
		Title: "Create deal",
		Description: "Adds a deal to the sales pipeline. name and value are required; link it to a customer_id or lead_id " +
			"when known.",
		InputSchema: tObj(map[string]any{
			"name":                tStr("Deal name"),
			"value":               tNum("Deal value"),
			"stage":               tEnum("Stage (default prospecting)", dealStages...),
			"probability":         tInt("Win probability 0-100", 0, 100),
			"expected_close_date": tDate("Expected close date"),
			"customer_id":         tStr("Customer id"),
			"lead_id":             tStr("Lead id"),
			"contact_name":        tStr("Contact person"),
			"company":             tStr("Company"),
			"notes":               tStr("Notes"),
		}, "name", "value"),
		Module:  "crm",
		Handler: dealsCreate,
	})
	Register(Tool{
		Name:  "deals_move_stage",
		Title: "Move deal stage",
		Description: "Moves a deal to another pipeline stage (e.g. proposal, negotiation, closed_won, closed_lost). " +
			"Optionally update probability, value or lost_reason at the same time.",
		InputSchema: tObj(map[string]any{
			"deal_id":     tStr("Deal id"),
			"stage":       tEnum("New stage", dealStages...),
			"probability": tInt("New probability 0-100", 0, 100),
			"value":       tNum("New value"),
			"lost_reason": tStr("Why it was lost (for closed_lost)"),
		}, "deal_id", "stage"),
		Module:  "crm",
		Handler: dealsMove,
	})
	idempotentTools["leads_update"] = true
	idempotentTools["deals_move_stage"] = true
}

func leadBrief(l map[string]any) map[string]any {
	m := map[string]any{"id": tS(l, "id"), "name": tJoinName(l)}
	for k, v := range tPick(l, "company", "email", "phone", "status", "rating", "source", "estimated_value", "customer_id") {
		m[k] = v
	}
	m["url"] = tURL("/crm?tab=leads")
	return m
}

func dealBrief(o map[string]any) map[string]any {
	m := tPick(o, "id", "name", "value", "stage", "probability", "contact_name", "company", "customer_id", "lead_id", "lost_reason")
	if d := tDay(tS(o, "expected_close_date")); d != "" {
		m["expected_close_date"] = d
	}
	m["url"] = tURL("/crm?tab=pipeline")
	return m
}

func leadsList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	for _, k := range []string{"status", "rating", "source"} {
		if s := argStr(a, k); s != "" {
			q.Set(k, s)
		}
	}
	if s := argStr(a, "query"); s != "" {
		q.Set("search", s)
	}
	v, err := c.get("/crm/leads", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "leads")
	total := len(list)
	list, cut := tLimitList(list, argLimit(a))
	out := []map[string]any{}
	for _, l := range list {
		out = append(out, leadBrief(l))
	}
	return tResult("leads", out, total, cut), nil
}

func leadBody(a map[string]any) map[string]any {
	b := map[string]any{}
	copyArgs(b, a, "first_name", "last_name", "email", "phone", "company", "job_title", "source", "status", "rating", "estimated_value", "notes")
	return b
}

func leadsCreate(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "first_name"); err != nil {
		return nil, err
	}
	v, err := c.api("POST", "/crm/leads", nil, leadBody(a))
	if err != nil {
		return nil, err
	}
	out := leadBrief(tObjOf(v, "lead"))
	out["created"] = true
	return out, nil
}

func leadsUpdate(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "lead_id")
	if id == "" {
		return nil, tErr("lead_id is required")
	}
	b := leadBody(a)
	if len(b) == 0 {
		return nil, tErr("Nothing to update: pass at least one field")
	}
	v, err := c.api("PATCH", "/crm/leads/"+url.PathEscape(id), nil, b)
	if err != nil {
		return nil, err
	}
	out := leadBrief(tObjOf(v, "lead"))
	out["updated_fields"] = keysOf(b)
	return out, nil
}

func dealsList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if s := argStr(a, "stage"); s != "" {
		q.Set("stage", s)
	}
	v, err := c.get("/crm/opportunities", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "opportunities")
	stages := map[string]map[string]float64{}
	for _, o := range list {
		st := tS(o, "stage")
		if stages[st] == nil {
			stages[st] = map[string]float64{}
		}
		stages[st]["count"]++
		stages[st]["value"] = tRound(stages[st]["value"] + tF(o, "value"))
	}
	total := len(list)
	list, cut := tLimitList(list, argLimit(a))
	out := []map[string]any{}
	for _, o := range list {
		out = append(out, dealBrief(o))
	}
	res := tResult("deals", out, total, cut)
	res["by_stage"] = stages
	return res, nil
}

func dealsCreate(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "name"); err != nil {
		return nil, err
	}
	b := map[string]any{"stage": "prospecting"}
	copyArgs(b, a, "name", "value", "stage", "probability", "expected_close_date", "customer_id", "lead_id", "contact_name", "company", "notes")
	v, err := c.api("POST", "/crm/opportunities", nil, b)
	if err != nil {
		return nil, err
	}
	out := dealBrief(tObjOf(v, "opportunity"))
	out["created"] = true
	return out, nil
}

func dealsMove(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "deal_id", "stage"); err != nil {
		return nil, err
	}
	b := map[string]any{}
	copyArgs(b, a, "stage", "probability", "value", "lost_reason")
	v, err := c.api("PATCH", "/crm/opportunities/"+url.PathEscape(argStr(a, "deal_id")), nil, b)
	if err != nil {
		return nil, err
	}
	return dealBrief(tObjOf(v, "opportunity")), nil
}
