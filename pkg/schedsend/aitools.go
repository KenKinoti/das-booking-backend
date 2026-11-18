package schedsend

// AI / MCP tools (pkg/aitools): every call goes through this package's own
// API in-process as the calling user, so permissions, validation and the
// audit log are exactly the web app's.

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

func init() {
	aitools.Register(aitools.Tool{
		Name:  "scheduled_list",
		Title: "Scheduled messages",
		Description: "Lists scheduled sends (Communication → Scheduled): invoice / quote / receipt emails, payment reminders, customer messages " +
			"(email, SMS, WhatsApp), statements of account and team messages, with the next send time in its time zone, how it repeats, who it goes " +
			"to and the last result (sent / partly failed / failed / skipped / missed with the reason). tab: upcoming (one-off, not yet sent), " +
			"recurring, sent (finished or cancelled) or failed. Use for 'what's scheduled for tomorrow?', 'did the monthly statements go out?'.",
		InputSchema: obj(map[string]any{
			"tab":    enum("Which list (default upcoming)", "upcoming", "recurring", "sent", "failed"),
			"kind":   enum("Only this kind", KindInvoice, KindReceipt, KindReminder, KindCustomer, KindStatement, KindTeam),
			"search": strP("Title, subject, invoice number or customer (part of it)"),
			"limit":  map[string]any{"type": "integer", "description": "Maximum results (default 20)", "minimum": 1, "maximum": 100},
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  toolList,
	})
	aitools.Register(aitools.Tool{
		Name:  "schedule_message",
		Title: "Schedule a message",
		Description: "Schedules a send for later (the user confirms before it is saved). what: invoice (an invoice or quote email, by its number — the " +
			"same email, PDF and tracking as Send), receipt (the latest payment's receipt), reminder (payment reminder for an unpaid invoice; " +
			"remind_every_days repeats it until paid), customer (a message to one customer, chosen customers or a group), statement (statement of " +
			"account with each customer's open invoices and balance) or team (staff: in-app notification, optional email / WhatsApp). send_at is " +
			"the local wall-clock time YYYY-MM-DDTHH:MM in the user's time zone (e.g. 'tomorrow at 8am' → the date of tomorrow + T08:00). Text " +
			"may use merge fields like {first_name}, {company}, {balance}, {invoice_number}, {amount_due}, {due_date}, {pay_link}. Set preview=true " +
			"first to show who gets what without saving. Example: 'send invoice INV-1043 tomorrow at 8am' → what=invoice, invoice_number=INV-1043, " +
			"send_at=<tomorrow>T08:00.",
		InputSchema: obj(map[string]any{
			"what":              enum("What to send", KindInvoice, KindReceipt, KindReminder, KindCustomer, KindStatement, KindTeam),
			"invoice_number":    strP("Invoice or quote number, e.g. INV-1043 (invoice, receipt, reminder)"),
			"send_at":           strP("Local date and time YYYY-MM-DDTHH:MM"),
			"timezone":          strP("IANA time zone (default: the user's, else the organisation's)"),
			"channels":          map[string]any{"type": "array", "items": enum("", ChanEmail, ChanSMS, ChanWhatsApp), "description": "Channels (customer, statement, reminder: email / sms / whatsapp; team: email / whatsapp besides in-app)"},
			"customer":          strP("One customer by name or email (customer / statement messages)"),
			"customer_ids":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Chosen customers' ids"},
			"group":             enum("A group of customers", GroupAllEmail, GroupAllPhone, GroupOverdue, GroupOpenBalance, GroupSubscriptions, GroupType, GroupCountry),
			"group_value":       strP("For customer_type: business or individual; for country: ISO code like KE"),
			"subject":           strP("Email subject / team message title"),
			"message":           strP("Email / in-app text (merge fields allowed)"),
			"sms_message":       strP("Short text for SMS and WhatsApp (default: message)"),
			"marketing":         map[string]any{"type": "boolean", "description": "Offers / promotions: adds an unsubscribe link and skips customers who opted out"},
			"repeat":            enum("Repeat (default none)", FreqNone, FreqDaily, FreqWeekly, FreqMonthly, FreqYearly),
			"every":             map[string]any{"type": "integer", "description": "Every N days / weeks / months / years", "minimum": 1, "maximum": 365},
			"weekdays":          map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 0, "maximum": 6}, "description": "Weekly: 0=Sun … 6=Sat"},
			"month_day":         map[string]any{"type": "integer", "description": "Monthly: day 1–31, or -1 for the last day", "minimum": -1, "maximum": 31},
			"until":             strP("Last date YYYY-MM-DD"),
			"count":             map[string]any{"type": "integer", "description": "At most this many sends", "minimum": 1, "maximum": 5000},
			"remind_every_days": map[string]any{"type": "integer", "description": "Reminders: repeat every N days until paid", "minimum": 1, "maximum": 90},
			"whatsapp_template": strP("Approved WhatsApp template for people without an open 24-hour conversation"),
			"whatsapp_params":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Template {{1}}, {{2}} … as text or merge fields"},
			"preview":           map[string]any{"type": "boolean", "description": "Only show who gets what (nothing is saved)"},
		}, "what", "send_at"),
		Module:  "core",
		Handler: toolSchedule,
	})
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strP(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func enum(desc string, vals ...string) map[string]any {
	m := map[string]any{"type": "string", "enum": vals}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

func aStr(a map[string]any, k string) string {
	switch v := a[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return fmt.Sprint(v)
	}
	return ""
}

func aInt(a map[string]any, k string) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func aBool(a map[string]any, k string) bool { b, _ := a[k].(bool); return b }

func aList(a map[string]any, k string) []string {
	out := []string{}
	if l, ok := a[k].([]any); ok {
		for _, v := range l {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

func aInts(a map[string]any, k string) []int {
	out := []int{}
	if l, ok := a[k].([]any); ok {
		for _, v := range l {
			if f, ok := v.(float64); ok {
				out = append(out, int(f))
			}
		}
	}
	return out
}

func dataOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	if d, ok := m["data"].(map[string]any); ok {
		return d
	}
	return m
}

func toolErr(err error) error {
	if ae, ok := err.(*aitools.APIError); ok {
		return &aitools.ToolError{Msg: ae.Message}
	}
	return err
}

func link(path string) string { return aitools.AppURL() + path }

func summary(m map[string]any, extra map[string]any) map[string]any {
	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		msg = m
	}
	out := map[string]any{}
	for _, k := range []string{"id", "title", "kind", "channels", "status", "next_local", "timezone", "last_result", "last_note", "occurrences", "doc_number"} {
		if v, ok := msg[k]; ok && v != nil && v != "" {
			out[k] = v
		}
	}
	for k, v := range extra {
		if v != nil && v != "" {
			out[k] = v
		}
	}
	if id, _ := msg["id"].(string); id != "" {
		out["url"] = link("/communication/scheduled?open=" + id)
	}
	return out
}

func toolList(c *aitools.Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	q.Set("tab", firstNonEmpty(aStr(a, "tab"), "upcoming"))
	if k := aStr(a, "kind"); k != "" {
		q.Set("kind", k)
	}
	if s := aStr(a, "search"); s != "" {
		q.Set("q", s)
	}
	v, err := c.Call("GET", "/schedsend/messages", q, nil)
	if err != nil {
		return nil, toolErr(err)
	}
	d := dataOf(v)
	items, _ := d["items"].([]any)
	limit := aInt(a, "limit")
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	out := []map[string]any{}
	for i, it := range items {
		if i >= limit {
			break
		}
		m, _ := it.(map[string]any)
		out = append(out, summary(m, map[string]any{"repeat": m["repeat_text"], "to": m["recipients_text"]}))
	}
	return map[string]any{"tab": q.Get("tab"), "counts": d["counts"], "items": out, "total": len(items), "truncated": len(items) > limit,
		"url": link("/communication/scheduled?tab=" + q.Get("tab"))}, nil
}

func (c ctxFinder) findCustomer(query string) (string, error) {
	v, err := c.Call("GET", "/customers", url.Values{"search": {query}}, nil)
	if err != nil {
		return "", toolErr(err)
	}
	var list []any
	switch d := dataOf(v); {
	case d != nil && d["customers"] != nil:
		list, _ = d["customers"].([]any)
	default:
		if m, ok := v.(map[string]any); ok {
			list, _ = m["data"].([]any)
		}
	}
	type hit struct{ id, name string }
	hits := []hit{}
	for _, x := range list {
		m, _ := x.(map[string]any)
		id, _ := m["id"].(string)
		name := strings.TrimSpace(fmt.Sprint(m["first_name"], " ", m["last_name"]))
		if id != "" {
			hits = append(hits, hit{id, name})
		}
	}
	switch len(hits) {
	case 0:
		return "", &aitools.ToolError{Msg: "No customer matches “" + query + "”"}
	case 1:
		return hits[0].id, nil
	}
	names := []string{}
	for i, h := range hits {
		if i >= 6 {
			break
		}
		names = append(names, h.name+" ("+h.id+")")
	}
	return "", &aitools.ToolError{Msg: "Several customers match “" + query + "”: " + strings.Join(names, ", ") + " — ask which one, then pass customer_ids"}
}

type ctxFinder struct{ *aitools.Ctx }

func toolSchedule(c *aitools.Ctx, a map[string]any) (any, error) {
	what := aStr(a, "what")
	tz := firstNonEmpty(aStr(a, "timezone"), c.Who.TZ)
	in := map[string]any{"kind": what, "local_send_at": aStr(a, "send_at"), "timezone": tz, "channels": aList(a, "channels"),
		"subject": aStr(a, "subject"), "body": aStr(a, "message"), "sms_body": aStr(a, "sms_message"), "marketing": aBool(a, "marketing")}
	rep := map[string]any{"freq": firstNonEmpty(aStr(a, "repeat"), FreqNone), "interval": aInt(a, "every"), "weekdays": aInts(a, "weekdays"),
		"month_day": aInt(a, "month_day"), "until": aStr(a, "until"), "count": aInt(a, "count")}
	switch what {
	case KindInvoice, KindReceipt, KindReminder:
		num := aStr(a, "invoice_number")
		if num == "" {
			return nil, &aitools.ToolError{Msg: "invoice_number is required"}
		}
		in["invoice_number"] = num
		if what == KindReceipt {
			pid, err := latestPayment(c, num)
			if err != nil {
				return nil, err
			}
			in["payment_id"] = pid
		}
		if what == KindReminder {
			if n := aInt(a, "remind_every_days"); n > 0 {
				rep["freq"], rep["interval"] = FreqDaily, n
			}
			if len(aList(a, "channels")) == 0 {
				in["channels"] = []string{ChanEmail}
			}
			if s := aStr(a, "subject"); s != "" || aStr(a, "message") != "" {
				in["invoice_send"] = map[string]any{"subject": s, "message": aStr(a, "message"), "accept_warnings": true}
			}
		} else {
			in["invoice_send"] = map[string]any{"subject": aStr(a, "subject"), "message": aStr(a, "message"), "accept_warnings": true}
		}
	case KindCustomer, KindStatement:
		aud := map[string]any{}
		ids := aList(a, "customer_ids")
		switch {
		case aStr(a, "group") != "":
			aud["mode"], aud["group"], aud["group_value"] = AudGroup, aStr(a, "group"), aStr(a, "group_value")
		case len(ids) > 0:
			aud["mode"], aud["customer_ids"] = AudChosen, ids
			if len(ids) == 1 {
				aud["mode"] = AudOne
			}
		case aStr(a, "customer") != "":
			id, err := ctxFinder{c}.findCustomer(aStr(a, "customer"))
			if err != nil {
				return nil, err
			}
			aud["mode"], aud["customer_ids"] = AudOne, []string{id}
		default:
			return nil, &aitools.ToolError{Msg: "Say who it goes to: customer, customer_ids or group"}
		}
		if what == KindStatement {
			aud["skip_zero_balance"] = true
		}
		in["audience"] = aud
		if len(aList(a, "channels")) == 0 {
			in["channels"] = []string{ChanEmail}
		}
	case KindTeam:
		in["audience"] = map[string]any{"mode": AudStaff}
	default:
		return nil, &aitools.ToolError{Msg: "what must be invoice, receipt, reminder, customer, statement or team"}
	}
	in["repeat"] = rep
	if t := aStr(a, "whatsapp_template"); t != "" {
		in["whatsapp"] = map[string]any{"template": t, "params": aList(a, "whatsapp_params"), "language": "en"}
	}
	if aBool(a, "preview") {
		v, err := c.Call("POST", "/schedsend/preview", nil, in)
		if err != nil {
			return nil, toolErr(err)
		}
		d := dataOf(v)
		out := map[string]any{"preview": true, "saved": false, "recipients": d["recipients"], "will_send": d["per_channel"], "skipped": d["skipped"],
			"send_times": d["upcoming"], "repeat": d["repeat_text"], "warnings": d["warnings"]}
		if smp, ok := d["samples"].([]any); ok && len(smp) > 0 {
			out["example"] = smp[0]
		}
		if what == KindInvoice || what == KindReceipt {
			out["note"] = "The email is the invoicing email (same recipients, subject, message and PDF as Send)."
		}
		return out, nil
	}
	v, err := c.Call("POST", "/schedsend/messages", nil, in)
	if err != nil {
		return nil, toolErr(err)
	}
	d := dataOf(v)
	return summary(d, map[string]any{"repeat": d["repeat_text"], "saved_at": time.Now().UTC().Format(time.RFC3339)}), nil
}

// latestPayment is the id of the latest payment on an invoice (by number).
func latestPayment(c *aitools.Ctx, number string) (string, error) {
	v, err := c.Call("GET", "/invoicing/invoices", url.Values{"search": {number}}, nil)
	if err != nil {
		return "", toolErr(err)
	}
	d := dataOf(v)
	var list []any
	if l, ok := d["invoices"].([]any); ok {
		list = l
	} else if m, ok := v.(map[string]any); ok {
		list, _ = m["data"].([]any)
	}
	for _, x := range list {
		m, _ := x.(map[string]any)
		if n, _ := m["number"].(string); strings.EqualFold(n, number) {
			id, _ := m["id"].(string)
			full, err := c.Call("GET", "/invoicing/invoices/"+id, nil, nil)
			if err != nil {
				return "", toolErr(err)
			}
			inv := dataOf(full)
			pays, _ := inv["payments"].([]any)
			if len(pays) == 0 {
				return "", &aitools.ToolError{Msg: number + " has no payments yet, so there is no receipt to send"}
			}
			p, _ := pays[len(pays)-1].(map[string]any)
			pid, _ := p["id"].(string)
			return pid, nil
		}
	}
	return "", &aitools.ToolError{Msg: "No invoice numbered " + number}
}
