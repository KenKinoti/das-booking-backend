package aitools

// Events, HR, meetings and projects tools.

import (
	"net/url"
	"strings"
	"time"
)

func init() {
	// ---- Events
	Register(Tool{
		Name:  "events_list",
		Title: "List events",
		Description: "Lists the organisation's events (upcoming, past, draft or all) with dates, venue, status, capacity, " +
			"registrations sold and link.",
		InputSchema: tObj(map[string]any{
			"scope": tEnum("Which events (default upcoming)", "upcoming", "past", "draft", "cancelled", "all"),
			"query": tStr("Search title, description, venue"),
			"limit": tLimit(),
		}),
		ReadOnly: true,
		Module:   "events",
		Handler:  eventsList,
	})
	Register(Tool{
		Name:  "events_registrations_summary",
		Title: "Event registrations",
		Description: "Registration summary for one event: totals by status, tickets sold, check-ins and revenue, plus the " +
			"latest registrations (name, email, ticket, status). Pass event_id (see events_list).",
		InputSchema: tObj(map[string]any{
			"event_id": tStr("Event id"),
			"status":   tStr("Only registrations with this status, e.g. confirmed, pending, cancelled"),
			"limit":    tInt("How many registrations to list (default 20)", 0, 200),
		}, "event_id"),
		ReadOnly: true,
		Module:   "events",
		Handler:  eventRegistrations,
	})

	// ---- HR
	Register(Tool{
		Name:        "employees_list",
		Title:       "List employees",
		Description: "Lists employees (HR) with position, department, employment type, status, email and phone. Use to get employee_id for leave requests.",
		InputSchema: tObj(map[string]any{
			"query":  tStr("Search name, email or position"),
			"status": tEnum("Employment status (default active)", "active", "on_leave", "terminated", "all"),
			"limit":  tLimit(),
		}),
		ReadOnly: true,
		Module:   "hr",
		Handler:  employeesList,
	})
	Register(Tool{
		Name:        "leave_list",
		Title:       "List leave requests",
		Description: "Lists leave requests (employee, type, dates, working days, status). Filter by status or employee.",
		InputSchema: tObj(map[string]any{
			"status":      tEnum("Status (default all)", "pending", "approved", "rejected", "all"),
			"employee_id": tStr("Only this employee"),
			"limit":       tLimit(),
		}),
		ReadOnly: true,
		Module:   "hr",
		Handler:  leaveList,
	})
	Register(Tool{
		Name:        "leave_create",
		Title:       "Request leave",
		Description: "Creates a pending leave request for an employee. Weekends are excluded from the day count; overlapping requests are refused.",
		InputSchema: tObj(map[string]any{
			"employee_id": tStr("Employee id (see employees_list)"),
			"type":        tEnum("Leave type", "annual", "sick", "personal", "parental", "unpaid", "other"),
			"start_date":  tDate("First day of leave"),
			"end_date":    tDate("Last day of leave"),
			"reason":      tStr("Reason"),
		}, "employee_id", "type", "start_date", "end_date"),
		Module:  "hr",
		Handler: leaveCreate,
	})
	Register(Tool{
		Name:        "leave_decide",
		Title:       "Approve / reject leave",
		Description: "Approves or rejects a leave request. Only managers/admins may do this (the API enforces it).",
		InputSchema: tObj(map[string]any{
			"leave_id": tStr("Leave request id"),
			"decision": tEnum("approve or reject", "approve", "reject"),
		}, "leave_id", "decision"),
		Module:  "hr",
		Handler: leaveDecide,
	})

	// ---- Meetings
	Register(Tool{
		Name:        "meetings_list",
		Title:       "List meetings",
		Description: "Lists meetings (upcoming by default) with time, participants, location / join link and status.",
		InputSchema: tObj(map[string]any{
			"scope": tEnum("Which meetings (default upcoming)", "upcoming", "past", "cancelled", "all"),
			"query": tStr("Search title, description or participant"),
			"limit": tLimit(),
		}),
		ReadOnly: true,
		Module:   "communication",
		Handler:  meetingsList,
	})
	Register(Tool{
		Name:  "meetings_schedule",
		Title: "Schedule meeting",
		Description: "Schedules a meeting and emails calendar invites to the participants (unless send_invites=false). " +
			"Participants can be emails, customers (customer_id) or staff (user_id). Default is an in-app video meeting. " +
			"Returns the meeting, its link and the invite result.",
		InputSchema: tObj(map[string]any{
			"title":            tStr("Meeting title"),
			"start_time":       tStr("Start, e.g. 2026-10-02T14:30 (business time zone) or RFC3339"),
			"duration_minutes": tInt("Length in minutes (default 30)", 5, 1440),
			"description":      tStr("Agenda / description"),
			"participants": tArr("People to invite", tObj(map[string]any{
				"name":        tStr("Name"),
				"email":       tStr("Email"),
				"customer_id": tStr("Customer id"),
				"user_id":     tStr("Staff user id"),
			})),
			"location_type": tEnum("in_app (video, default), external (join_url) or physical (location)", "in_app", "external", "physical"),
			"location":      tStr("Address for physical meetings"),
			"join_url":      tStr("Link for external meetings (Zoom, Teams …)"),
			"send_invites":  tBool("Email invites (default true)"),
		}, "title", "start_time"),
		Module:  "communication",
		Handler: meetingsSchedule,
	})

	// ---- Projects
	Register(Tool{
		Name:        "projects_list",
		Title:       "List projects",
		Description: "Lists projects (local and synced from Jira) with key, status, progress/task counts and link.",
		InputSchema: tObj(map[string]any{
			"query":  tStr("Search name or key"),
			"status": tStr("Only this status, e.g. active"),
		}),
		ReadOnly: true,
		Module:   "projects",
		Handler:  projectsList,
	})
	Register(Tool{
		Name:  "projects_search_issues",
		Title: "Search project issues",
		Description: "Searches tasks/issues in one project or across all projects by text, status category " +
			"(todo, in_progress, done) or assignee. Returns key, title, status, priority, assignee, due date and links.",
		InputSchema: tObj(map[string]any{
			"project_id":      tStr("Project id (leave out to search all projects)"),
			"query":           tStr("Text in title, key or assignee"),
			"status_category": tEnum("Status category", "todo", "in_progress", "done"),
			"assignee":        tStr("Assignee name, or 'unassigned'"),
			"limit":           tLimit(),
		}),
		ReadOnly: true,
		Module:   "projects",
		Handler:  projectsSearchIssues,
	})
}

func eventsList(c *Ctx, a map[string]any) (any, error) {
	scope := argStr(a, "scope")
	if scope == "" {
		scope = "upcoming"
	}
	q := url.Values{"scope": {scope}, "per_page": {itoa(argLimit(a))}}
	if s := argStr(a, "query"); s != "" {
		q.Set("search", s)
	}
	v, err := c.get("/events", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	out := []map[string]any{}
	for _, e := range tList(d, "events") {
		m := tPick(e, "id", "title", "display_status:status", "type", "category", "venue_name", "address.suburb:suburb", "online_url", "max_capacity:capacity", "is_free", "base_price:price", "currency")
		if st, ok := e["stats"].(map[string]any); ok {
			m["stats"] = tPick(st, "registrations", "tickets", "checked_in", "waitlisted", "revenue", "remaining", "fill_rate")
		}
		m["start"] = c.localTime(tS(e, "start_date"))
		m["end"] = c.localTime(tS(e, "end_date"))
		m["url"] = tURL("/events/" + tS(e, "id"))
		out = append(out, m)
	}
	total := int(tF(d, "total"))
	return tResult("events", out, total, total > len(out)), nil
}

func eventRegistrations(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "event_id")
	if id == "" {
		return nil, tErr("event_id is required")
	}
	q := url.Values{}
	if s := argStr(a, "status"); s != "" {
		q.Set("status", s)
	}
	v, err := c.get("/events/"+url.PathEscape(id)+"/registrations", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	regs := tList(d, "registrations")
	n := argInt(a, "limit", 20)
	total := len(regs)
	regs, cut := tLimitList(regs, n)
	rows := []map[string]any{}
	for _, r := range regs {
		m := tPick(r, "id", "registrant_name:name", "registrant_email:email", "registrant_phone:phone", "company", "ticket_type_name:ticket", "quantity", "total_amount", "status", "payment_status", "checked_in_at", "created_at")
		rows = append(rows, m)
	}
	out := tResult("registrations", rows, total, cut)
	out["summary"] = d["summary"]
	out["url"] = tURL("/events/" + id + "/registrations")
	return out, nil
}

func employeesList(c *Ctx, a map[string]any) (any, error) {
	st := argStr(a, "status")
	if st == "" {
		st = "active"
	}
	q := url.Values{"status": {st}}
	if s := argStr(a, "query"); s != "" {
		q.Set("q", s)
	}
	v, err := c.get("/hr/employees", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "employees")
	total := len(list)
	list, cut := tLimitList(list, argLimit(a))
	out := []map[string]any{}
	for _, e := range list {
		m := map[string]any{"id": tS(e, "id"), "name": strings.TrimSpace(tS(e, "full_name"))}
		if m["name"] == "" {
			m["name"] = tJoinName(e)
		}
		for k, val := range tPick(e, "position", "department_name:department", "employment_type", "status", "email", "phone", "start_date", "user_id") {
			m[k] = val
		}
		m["url"] = tURL("/hcm?tab=employees")
		out = append(out, m)
	}
	return tResult("employees", out, total, cut), nil
}

func leaveBrief(l map[string]any) map[string]any {
	m := tPick(l, "id", "employee_id", "employee_name", "type", "start_date", "end_date", "days", "reason", "status", "decided_at")
	m["url"] = tURL("/hcm?tab=leave")
	return m
}

func leaveList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if s := argStr(a, "status"); s != "" {
		q.Set("status", s)
	}
	if s := argStr(a, "employee_id"); s != "" {
		q.Set("employee_id", s)
	}
	v, err := c.get("/hr/leave", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "leave")
	total := len(list)
	list, cut := tLimitList(list, argLimit(a))
	out := []map[string]any{}
	for _, l := range list {
		out = append(out, leaveBrief(l))
	}
	return tResult("leave_requests", out, total, cut), nil
}

func leaveCreate(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "employee_id", "type", "start_date", "end_date"); err != nil {
		return nil, err
	}
	b := map[string]any{}
	copyArgs(b, a, "employee_id", "type", "start_date", "end_date", "reason")
	v, err := c.api("POST", "/hr/leave", nil, b)
	if err != nil {
		return nil, err
	}
	out := leaveBrief(tObjOf(v))
	out["created"] = true
	return out, nil
}

func leaveDecide(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "leave_id", "decision"); err != nil {
		return nil, err
	}
	action := "approve"
	if argStr(a, "decision") == "reject" {
		action = "reject"
	}
	v, err := c.api("POST", "/hr/leave/"+url.PathEscape(argStr(a, "leave_id"))+"/"+action, nil, nil)
	if err != nil {
		return nil, err
	}
	return leaveBrief(tObjOf(v)), nil
}

func meetingBrief(c *Ctx, m map[string]any) map[string]any {
	out := tPick(m, "id", "title", "status", "location_type", "location", "join_url", "timezone", "organizer_name")
	out["start"] = c.localTime(tS(m, "start_at"))
	out["end"] = c.localTime(tS(m, "end_at"))
	ps := []map[string]any{}
	for _, p := range tList(m["participants"]) {
		ps = append(ps, tPick(p, "name", "email", "response"))
	}
	if len(ps) > 0 {
		out["participants"] = ps
	}
	out["url"] = tURL("/meetings/" + tS(m, "id"))
	return out
}

func meetingsList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	scope := argStr(a, "scope")
	if scope == "" {
		scope = "upcoming"
	}
	if scope != "all" {
		q.Set("scope", scope)
	}
	if s := argStr(a, "query"); s != "" {
		q.Set("q", s)
	}
	v, err := c.get("/meetings", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "meetings")
	if scope == "past" { // API returns oldest first; show the most recent
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	total := len(list)
	list, cut := tLimitList(list, argLimit(a))
	out := []map[string]any{}
	for _, m := range list {
		out = append(out, meetingBrief(c, m))
	}
	return tResult("meetings", out, total, cut), nil
}

func meetingsSchedule(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "title", "start_time"); err != nil {
		return nil, err
	}
	start, err := c.parseWhen(argStr(a, "start_time"))
	if err != nil {
		return nil, err
	}
	dur := argInt(a, "duration_minutes", 30)
	if dur <= 0 {
		dur = 30
	}
	b := map[string]any{
		"title":    argStr(a, "title"),
		"start_at": start.Format(time.RFC3339),
		"end_at":   start.Add(time.Duration(dur) * time.Minute).Format(time.RFC3339),
	}
	if c.Who.TZ != "" {
		b["timezone"] = c.Who.TZ
	}
	copyArgs(b, a, "description", "location_type", "location", "join_url", "send_invites")
	if ps := argList(a, "participants"); ps != nil {
		resolved, err := c.resolveParticipants(ps)
		if err != nil {
			return nil, err
		}
		b["participants"] = resolved
	}
	v, err := c.api("POST", "/meetings", nil, b)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	out := meetingBrief(c, tObjOf(d, "meeting"))
	if inv, ok := d["invite"]; ok {
		out["invites"] = inv
	}
	out["created"] = true
	return out, nil
}

func projectsList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if s := argStr(a, "query"); s != "" {
		q.Set("q", s)
	}
	if s := argStr(a, "status"); s != "" {
		q.Set("status", s)
	}
	v, err := c.get("/pm/projects", q)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, p := range tList(v, "projects") {
		m := tPick(p, "id", "key", "name", "description", "status", "source", "owner_name", "budget", "external_url:jira_url", "stats")
		if d := tDay(tS(p, "due_date")); d != "" {
			m["due_date"] = d
		}
		m["url"] = tURL("/projects?project=" + tS(p, "id"))
		out = append(out, m)
	}
	return tResult("projects", out, 0, false), nil
}

func projectsSearchIssues(c *Ctx, a map[string]any) (any, error) {
	ids := []string{}
	names := map[string]string{}
	if id := argStr(a, "project_id"); id != "" {
		ids = append(ids, id)
	} else {
		v, err := c.get("/pm/projects", nil)
		if err != nil {
			return nil, err
		}
		for _, p := range tList(v, "projects") {
			ids = append(ids, tS(p, "id"))
			names[tS(p, "id")] = tS(p, "name")
		}
		if len(ids) > 25 {
			ids = ids[:25]
		}
	}
	q := url.Values{}
	if s := argStr(a, "query"); s != "" {
		q.Set("q", s)
	}
	if s := argStr(a, "status_category"); s != "" {
		q.Set("status_category", s)
	}
	if s := argStr(a, "assignee"); s != "" {
		q.Set("assignee", s)
	}
	limit := argLimit(a)
	out := []map[string]any{}
	total := 0
	for _, pid := range ids {
		v, err := c.get("/pm/projects/"+url.PathEscape(pid)+"/tasks", q)
		if err != nil {
			if len(ids) == 1 {
				return nil, err
			}
			continue
		}
		for _, t := range tList(v, "tasks") {
			total++
			if len(out) >= limit {
				continue
			}
			m := tPick(t, "id", "key", "title", "status", "status_category", "type", "priority", "assignee_name:assignee", "story_points", "url:jira_url")
			if d := tDay(tS(t, "due_date")); d != "" {
				m["due_date"] = d
			}
			m["project_id"] = pid
			if n := names[pid]; n != "" {
				m["project"] = n
			}
			m["url"] = tURL("/projects?project=" + pid)
			out = append(out, m)
		}
	}
	return tResult("issues", out, total, total > len(out)), nil
}

// resolveParticipants fills in name/email for customer and staff participants.
func (c *Ctx) resolveParticipants(ps []any) ([]map[string]any, error) {
	out := []map[string]any{}
	var users []map[string]any
	for i, x := range ps {
		p, ok := x.(map[string]any)
		if !ok {
			return nil, tErr("participants[%d] must be an object", i)
		}
		m := map[string]any{}
		copyArgs(m, p, "name", "email", "customer_id", "user_id")
		if argStr(m, "email") == "" {
			switch {
			case argStr(m, "customer_id") != "":
				v, err := c.get("/customers/"+url.PathEscape(argStr(m, "customer_id")), nil)
				if err != nil {
					return nil, err
				}
				cu := tObjOf(v, "customer")
				m["email"] = tS(cu, "email")
				if argStr(m, "name") == "" {
					m["name"] = tJoinName(cu)
				}
			case argStr(m, "user_id") != "":
				if users == nil {
					v, err := c.get("/users", url.Values{"limit": {"200"}})
					if err != nil {
						return nil, err
					}
					users = tList(v, "users")
				}
				for _, u := range users {
					if tS(u, "id") == argStr(m, "user_id") {
						m["email"] = tS(u, "email")
						if argStr(m, "name") == "" {
							m["name"] = tJoinName(u)
						}
					}
				}
			}
			if argStr(m, "email") == "" {
				return nil, tErr("participants[%d] needs an email (or a customer/staff member with an email)", i)
			}
		}
		out = append(out, m)
	}
	return out, nil
}
