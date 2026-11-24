package outbox

// AI / MCP tools (pkg/aitools): every call goes through this package's API
// in-process as the calling user, so permissions and the audit trail are the
// web app's.

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

func init() {
	aitools.Register(aitools.Tool{
		Name:  "outbox_list",
		Title: "Messages waiting for approval",
		Description: "Lists client messages in Pending review (Communication → Pending review): invoice, quote, reminder and receipt emails, " +
			"scheduled customer messages and statements (email / SMS / WhatsApp), subscription notices and meeting invitations that wait for an " +
			"owner or admin to approve them before they go to the client. Each has who it goes to, the subject, the linked record and amount, " +
			"why it was queued, how long it has waited and flags (stale, blocking a suspension). tab: waiting (default), approved (approved for " +
			"later), sent, closed (rejected or no longer needed). Use for 'what's waiting for my approval?'.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"tab":     map[string]any{"type": "string", "enum": []string{"waiting", "approved", "sent", "closed"}, "description": "Which list (default waiting)"},
			"kind":    map[string]any{"type": "string", "description": "Only this kind, e.g. invoice_email, reminder_email, customer_message"},
			"channel": map[string]any{"type": "string", "enum": []string{"email", "sms", "whatsapp"}, "description": "Only this channel"},
			"q":       map[string]any{"type": "string", "description": "Search: recipient, subject, invoice number or customer"},
			"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum results (default 20)"},
		}},
		ReadOnly: true,
		Module:   "core",
		Handler:  toolList,
	})
	aitools.Register(aitools.Tool{
		Name:  "outbox_approve",
		Title: "Approve waiting messages",
		Description: "Approves client messages waiting in Pending review so they go out (owners and admins only). Confirm with the user first, " +
			"naming each message (kind, recipient, subject). when: now (default — sends at once through the normal send path) or scheduled (for a " +
			"message with a later send time). A message that is no longer needed (e.g. its invoice was paid) is marked so instead of being sent. " +
			"Get the ids from outbox_list.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"id":   map[string]any{"type": "string", "description": "One message id"},
			"ids":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Several message ids (≤ 100)"},
			"when": map[string]any{"type": "string", "enum": []string{"now", "scheduled"}, "description": "Send now (default) or at its scheduled time"},
		}},
		Module:  "core",
		Handler: toolApprove,
	})
	aitools.Register(aitools.Tool{
		Name:  "outbox_reject",
		Title: "Reject waiting messages",
		Description: "Rejects client messages waiting in Pending review so they are never sent (owners and admins only); the record shows that it " +
			"was not sent. Confirm with the user first. note: why (optional, shown in the history).",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"id":   map[string]any{"type": "string", "description": "One message id"},
			"ids":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Several message ids (≤ 100)"},
			"note": map[string]any{"type": "string", "description": "Why it is rejected"},
		}},
		Destructive: true,
		Module:      "core",
		Handler:     toolReject,
	})
}

func aStr(a map[string]any, k string) string {
	v, _ := a[k].(string)
	return strings.TrimSpace(v)
}

func aIDs(a map[string]any) []string {
	out := []string{}
	if id := aStr(a, "id"); id != "" {
		out = append(out, id)
	}
	if l, ok := a["ids"].([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
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

func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil && v != "" && v != false {
			out[k] = v
		}
	}
	return out
}

func brief(r map[string]any) map[string]any {
	out := pick(r, "id", "kind_label", "channel", "status_label", "to", "to_name", "subject", "amount", "currency", "trigger", "created_at", "send_at",
		"stale", "flag", "error", "superseded_reason", "result")
	if rec, ok := r["record"].(map[string]any); ok {
		out["record"] = rec["label"]
	}
	if id, _ := r["id"].(string); id != "" {
		out["url"] = aitools.AppURL() + "/outbox?open=" + id
	}
	return out
}

func toolList(c *aitools.Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	q.Set("tab", firstNonEmpty(aStr(a, "tab"), "waiting"))
	for _, k := range []string{"kind", "channel", "q"} {
		if v := aStr(a, k); v != "" {
			q.Set(k, v)
		}
	}
	limit := 20
	if n, ok := a["limit"].(float64); ok && n >= 1 && n <= 100 {
		limit = int(n)
	}
	q.Set("per_page", fmt.Sprint(limit))
	v, err := c.Call("GET", "/outbox/items", q, nil)
	if err != nil {
		return nil, toolErr(err)
	}
	d := dataOf(v)
	items := []map[string]any{}
	if l, ok := d["items"].([]any); ok {
		for _, x := range l {
			if m, ok := x.(map[string]any); ok {
				items = append(items, brief(m))
			}
		}
	}
	out := map[string]any{"items": items, "total": d["total"], "url": aitools.AppURL() + "/outbox"}
	if cnt, ok := d["counts"].(map[string]any); ok {
		out["counts"] = pick(cnt, "waiting", "stale", "time_sensitive", "approved", "can_approve", "enabled")
	}
	return out, nil
}

func toolApprove(c *aitools.Ctx, a map[string]any) (any, error) {
	ids := aIDs(a)
	if len(ids) == 0 {
		return nil, &aitools.ToolError{Msg: "Which message? Pass id or ids (from outbox_list)."}
	}
	when := firstNonEmpty(aStr(a, "when"), "now")
	v, err := c.Call("POST", "/outbox/bulk", nil, map[string]any{"action": "approve", "ids": ids, "when": when})
	if err != nil {
		return nil, toolErr(err)
	}
	d := dataOf(v)
	return pick(d, "results", "done", "failed"), nil
}

func toolReject(c *aitools.Ctx, a map[string]any) (any, error) {
	ids := aIDs(a)
	if len(ids) == 0 {
		return nil, &aitools.ToolError{Msg: "Which message? Pass id or ids (from outbox_list)."}
	}
	v, err := c.Call("POST", "/outbox/bulk", nil, map[string]any{"action": "reject", "ids": ids, "note": aStr(a, "note")})
	if err != nil {
		return nil, toolErr(err)
	}
	d := dataOf(v)
	return pick(d, "results", "done", "failed"), nil
}
