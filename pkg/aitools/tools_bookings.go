package aitools

import (
	"net/url"
	"strings"
	"time"
)

var bookingStatuses = []string{"scheduled", "confirmed", "in_progress", "completed", "cancelled", "no_show"}

func init() {
	Register(Tool{
		Name:  "bookings_list",
		Title: "List bookings",
		Description: "Lists appointments/bookings for a day or date range (default: today), optionally filtered by status, " +
			"customer or staff member. Returns time (in the business time zone), customer, services, staff, status, price and link.",
		InputSchema: tObj(map[string]any{
			"date":        tDate("A single day (default today unless from/to given)"),
			"from":        tDate("Range start (inclusive)"),
			"to":          tDate("Range end (inclusive)"),
			"status":      tEnum("Only this status", bookingStatuses...),
			"customer_id": tStr("Only this customer's bookings (any date when no dates are given)"),
			"staff_id":    tStr("Only this staff member's bookings"),
			"limit":       tInt("Maximum results (default 50)", 1, 200),
		}),
		ReadOnly: true,
		Module:   "bookings",
		Handler:  bookingsList,
	})
	Register(Tool{
		Name:  "bookings_availability",
		Title: "Free booking slots",
		Description: "Free start times on a date within business hours, taking existing bookings and the buffer into account. " +
			"Pass service_ids to use their total duration, or duration_minutes. Optionally for one staff member. " +
			"Use before bookings_create to offer times.",
		InputSchema: tObj(map[string]any{
			"date":             tDate("Day to check"),
			"duration_minutes": tInt("Appointment length in minutes (default 60, or the services' total)", 5, 720),
			"service_ids":      tArr("Services to book (their durations are added up)", tStr("service id")),
			"staff_id":         tStr("Only check this staff member's calendar"),
		}, "date"),
		ReadOnly: true,
		Module:   "bookings",
		Handler:  bookingsAvailability,
	})
	Register(Tool{
		Name:  "bookings_create",
		Title: "Create booking",
		Description: "Books an appointment for a customer. start_time is local business time (YYYY-MM-DDTHH:MM) or RFC3339. " +
			"End time defaults to start + the services' total duration (or 60 min); price defaults to the services' total. " +
			"Fails with a clear message if it overlaps another booking for the same staff member. Returns the booking and link.",
		InputSchema: tObj(map[string]any{
			"customer_id":    tStr("Customer id"),
			"customer":       tStr("Customer name or email to look up when you don't have the id"),
			"start_time":     tStr("Start, e.g. 2026-10-02T14:30 (business time zone) or RFC3339"),
			"end_time":       tStr("Optional end time (same formats)"),
			"service_ids":    tArr("Services being booked (see services_list)", tStr("service id")),
			"staff_id":       tStr("Staff member (see staff_list)"),
			"status":         tEnum("Initial status (default scheduled)", "scheduled", "confirmed"),
			"total_price":    tNum("Override the price"),
			"notes":          tStr("Notes visible to the customer"),
			"internal_notes": tStr("Internal notes"),
		}, "start_time"),
		Module:  "bookings",
		Handler: bookingsCreate,
	})
	Register(Tool{
		Name:  "bookings_update",
		Title: "Reschedule / update booking",
		Description: "Reschedules a booking (new start/end time), changes staff, services, notes or price, or sets its status " +
			"(confirmed, in_progress, completed, no_show). Only fields you pass change; moving start_time keeps the " +
			"same duration unless end_time is given. To cancel use bookings_cancel.",
		InputSchema: tObj(map[string]any{
			"booking_id":     tStr("Booking id"),
			"start_time":     tStr("New start (business time zone YYYY-MM-DDTHH:MM or RFC3339)"),
			"end_time":       tStr("New end"),
			"status":         tEnum("New status", "scheduled", "confirmed", "in_progress", "completed", "no_show"),
			"staff_id":       tStr("New staff member id (empty string to unassign)"),
			"service_ids":    tArr("Replace the services", tStr("service id")),
			"total_price":    tNum("New price"),
			"notes":          tStr("Replace the notes"),
			"internal_notes": tStr("Replace the internal notes"),
		}, "booking_id"),
		Module:  "bookings",
		Handler: bookingsUpdate,
	})
	Register(Tool{
		Name:        "bookings_cancel",
		Title:       "Cancel booking",
		Description: "Cancels a booking (status → cancelled; the record is kept). Confirm with the user first.",
		InputSchema: tObj(map[string]any{
			"booking_id": tStr("Booking id"),
			"reason":     tStr("Reason, added to the internal notes"),
		}, "booking_id"),
		Destructive: true,
		Module:      "bookings",
		Handler:     bookingsCancel,
	})
	Register(Tool{
		Name:        "services_list",
		Title:       "List services",
		Description: "Lists the services customers can book (name, category, duration in minutes, price, active). Use to find service_ids for bookings or invoice lines.",
		InputSchema: tObj(map[string]any{
			"query":            tStr("Filter by name"),
			"include_inactive": tBool("Also return inactive services"),
		}),
		ReadOnly: true,
		Module:   "bookings",
		Handler:  servicesList,
	})
	Register(Tool{
		Name:        "staff_list",
		Title:       "List staff",
		Description: "Lists the organisation's staff/users (id, name, email, role, active) — use for staff_id on bookings.",
		InputSchema: tObj(map[string]any{
			"query": tStr("Filter by name or email"),
			"role":  tStr("Only this role, e.g. admin, manager, staff"),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  staffList,
	})
	idempotentTools["bookings_update"] = true
	idempotentTools["bookings_cancel"] = true
}

func (c *Ctx) bookingBrief(b map[string]any) map[string]any {
	m := tPick(b, "id", "status", "total_price", "notes", "customer_id", "staff_id")
	m["start_time"] = c.localTime(tS(b, "start_time"))
	m["end_time"] = c.localTime(tS(b, "end_time"))
	if cu, ok := b["customer"].(map[string]any); ok && tS(cu, "id") != "" {
		m["customer"] = tJoinName(cu)
		if e := tS(cu, "email"); e != "" {
			m["customer_email"] = e
		}
	}
	if st, ok := b["staff"].(map[string]any); ok && tS(st, "id") != "" {
		m["staff"] = tJoinName(st)
	}
	svcs := []string{}
	for _, s := range tList(b["services"]) {
		svcs = append(svcs, tS(s, "name"))
	}
	if l, ok := b["services"].([]any); ok && len(svcs) == 0 {
		for _, x := range l {
			if s, ok := x.(string); ok {
				svcs = append(svcs, s)
			}
		}
	}
	if len(svcs) > 0 {
		m["services"] = svcs
	}
	if tS(b, "id") != "" {
		m["url"] = tURL("/bookings?booking=" + tS(b, "id"))
	}
	return m
}

func (c *Ctx) dayStart(d string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", d, c.loc())
	if err != nil {
		return t, tErr("Invalid date %q (use YYYY-MM-DD)", d)
	}
	return t, nil
}

func bookingsList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	from, to := argStr(a, "from"), argStr(a, "to")
	if d := argStr(a, "date"); d != "" {
		from, to = d, d
	}
	if from == "" && to == "" && argStr(a, "customer_id") == "" && argStr(a, "staff_id") == "" {
		from, to = c.today(), c.today()
	}
	if from != "" {
		t, err := c.dayStart(from)
		if err != nil {
			return nil, err
		}
		q.Set("date_from", t.Format(time.RFC3339))
	}
	if to != "" {
		t, err := c.dayStart(to)
		if err != nil {
			return nil, err
		}
		q.Set("date_to", t.AddDate(0, 0, 1).Format(time.RFC3339))
	}
	for _, k := range []string{"status", "customer_id", "staff_id"} {
		if s := argStr(a, k); s != "" {
			q.Set(k, s)
		}
	}
	v, err := c.get("/bookings", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "bookings")
	total := len(list)
	n := argInt(a, "limit", 50)
	if n < 1 || n > 200 {
		n = 50
	}
	list, cut := tLimitList(list, n)
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		out = append(out, c.bookingBrief(b))
	}
	res := tResult("bookings", out, total, cut)
	if from != "" || to != "" {
		res["range"] = map[string]any{"from": from, "to": to}
	}
	return res, nil
}

func (c *Ctx) servicesDuration(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	v, err := c.get("/services", url.Values{"limit": {"500"}})
	if err != nil {
		return 0, err
	}
	byID := map[string]map[string]any{}
	for _, s := range tList(v, "services") {
		byID[tS(s, "id")] = s
	}
	total := 0
	for _, id := range ids {
		s, ok := byID[id]
		if !ok {
			return 0, tErr("Unknown service_id %s (see services_list)", id)
		}
		total += int(tF(s, "duration"))
	}
	return total, nil
}

func bookingsAvailability(c *Ctx, a map[string]any) (any, error) {
	date := argStr(a, "date")
	if !validDate(date) {
		return nil, tErr("date must be YYYY-MM-DD")
	}
	dur := argInt(a, "duration_minutes", 0)
	if ids := argStrList(a, "service_ids"); len(ids) > 0 && dur == 0 {
		d, err := c.servicesDuration(ids)
		if err != nil {
			return nil, err
		}
		dur = d
	}
	if dur <= 0 {
		dur = 60
	}
	q := url.Values{"date": {date}, "duration": {itoa(dur)}}
	if s := argStr(a, "staff_id"); s != "" {
		q.Set("staff_id", s)
	}
	v, err := c.get("/bookings/available-slots", q)
	if err != nil {
		return nil, err
	}
	slots := tDig(tData(v), "available_slots")
	if slots == nil {
		slots = tDig(v, "available_slots")
	}
	out := map[string]any{"date": date, "duration_minutes": dur, "free_start_times": slots}
	if l, ok := slots.([]any); !ok || len(l) == 0 {
		out["free_start_times"] = []any{}
		out["hint"] = "No free slots: the business may be closed that day (set business hours in Settings) or fully booked."
	}
	return out, nil
}

func bookingsCreate(c *Ctx, a map[string]any) (any, error) {
	cid, err := c.resolveCustomerID(a, "customer_id", "customer")
	if err != nil {
		return nil, err
	}
	start, err := c.parseWhen(argStr(a, "start_time"))
	if err != nil {
		return nil, err
	}
	body := map[string]any{"customer_id": cid, "start_time": start.Format(time.RFC3339)}
	if e := argStr(a, "end_time"); e != "" {
		end, err := c.parseWhen(e)
		if err != nil {
			return nil, err
		}
		body["end_time"] = end.Format(time.RFC3339)
	}
	if ids := argStrList(a, "service_ids"); len(ids) > 0 {
		body["service_ids"] = ids
	}
	copyArgs(body, a, "staff_id", "status", "total_price", "notes", "internal_notes")
	v, err := c.api("POST", "/bookings", nil, body)
	if err != nil {
		return nil, err
	}
	out := c.bookingBrief(tObjOf(v, "booking"))
	out["created"] = true
	return out, nil
}

func bookingsUpdate(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "booking_id")
	if id == "" {
		return nil, tErr("booking_id is required")
	}
	body := map[string]any{}
	if s := argStr(a, "start_time"); s != "" {
		start, err := c.parseWhen(s)
		if err != nil {
			return nil, err
		}
		body["start_time"] = start.Format(time.RFC3339)
		if argStr(a, "end_time") == "" {
			cur, err := c.get("/bookings/"+url.PathEscape(id), nil)
			if err != nil {
				return nil, err
			}
			b := tObjOf(cur, "booking")
			st, e1 := time.Parse(time.RFC3339Nano, tS(b, "start_time"))
			en, e2 := time.Parse(time.RFC3339Nano, tS(b, "end_time"))
			if e1 == nil && e2 == nil && en.After(st) {
				body["end_time"] = start.Add(en.Sub(st)).Format(time.RFC3339)
			}
		}
	}
	if s := argStr(a, "end_time"); s != "" {
		end, err := c.parseWhen(s)
		if err != nil {
			return nil, err
		}
		body["end_time"] = end.Format(time.RFC3339)
	}
	if argHas(a, "service_ids") {
		body["service_ids"] = argStrList(a, "service_ids")
	}
	copyArgs(body, a, "staff_id", "status", "total_price", "notes", "internal_notes")
	if len(body) == 0 {
		return nil, tErr("Nothing to update: pass at least one field")
	}
	var v any
	var err error
	if len(body) == 1 && body["status"] != nil {
		v, err = c.api("PATCH", "/bookings/"+url.PathEscape(id)+"/status", nil, body)
	} else {
		v, err = c.api("PUT", "/bookings/"+url.PathEscape(id), nil, body)
	}
	if err != nil {
		return nil, err
	}
	out := c.bookingBrief(tObjOf(v, "booking"))
	out["updated_fields"] = keysOf(body)
	return out, nil
}

func bookingsCancel(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "booking_id")
	if id == "" {
		return nil, tErr("booking_id is required")
	}
	body := map[string]any{"status": "cancelled"}
	if r := argStr(a, "reason"); r != "" {
		cur, err := c.get("/bookings/"+url.PathEscape(id), nil)
		if err != nil {
			return nil, err
		}
		notes := strings.TrimSpace(tS(tObjOf(cur, "booking"), "internal_notes"))
		if notes != "" {
			notes += "\n"
		}
		body["internal_notes"] = notes + "Cancelled: " + r
	}
	v, err := c.api("PUT", "/bookings/"+url.PathEscape(id), nil, body)
	if err != nil {
		return nil, err
	}
	out := c.bookingBrief(tObjOf(v, "booking"))
	out["cancelled"] = true
	return out, nil
}

func servicesList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{"limit": {"500"}}
	if s := argStr(a, "query"); s != "" {
		q.Set("search", s)
	}
	v, err := c.get("/services", q)
	if err != nil {
		return nil, err
	}
	inactive, _ := argBool(a, "include_inactive")
	out := []map[string]any{}
	for _, s := range tList(v, "services") {
		if b, ok := s["is_active"].(bool); ok && !b && !inactive {
			continue
		}
		m := tPick(s, "id", "name", "category", "price", "description")
		m["duration_minutes"] = s["duration"]
		m["is_active"] = s["is_active"]
		out = append(out, m)
	}
	return tResult("services", out, 0, false), nil
}

func staffList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{"limit": {"200"}}
	if s := argStr(a, "query"); s != "" {
		q.Set("search", s)
	}
	if s := argStr(a, "role"); s != "" {
		q.Set("role", s)
	}
	v, err := c.get("/users", q)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	needle := strings.ToLower(argStr(a, "query"))
	for _, u := range tList(v, "users") {
		name := tJoinName(u)
		if needle != "" && !strings.Contains(strings.ToLower(name+" "+tS(u, "email")), needle) {
			continue
		}
		m := map[string]any{"id": tS(u, "id"), "name": name}
		for k, val := range tPick(u, "email", "role", "phone", "is_active") {
			m[k] = val
		}
		out = append(out, m)
	}
	return tResult("staff", out, 0, false), nil
}
