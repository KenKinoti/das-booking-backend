package assistant

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

// toolTitle is the human title of a tool ("Search customers").
func toolTitle(t aitools.Tool, fallback string) string {
	if strings.TrimSpace(t.Title) != "" {
		return t.Title
	}
	n := t.Name
	if n == "" {
		n = fallback
	}
	parts := strings.Split(n, "_")
	if len(parts) >= 2 {
		// customers_search -> Search customers
		verb := parts[len(parts)-1]
		rest := strings.Join(parts[:len(parts)-1], " ")
		return upperFirst(verb) + " " + rest
	}
	return upperFirst(strings.ReplaceAll(n, "_", " "))
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if len(r) > 1 && strings.ToUpper(string(r[1])) == string(r[1]) {
		return s // acronym
	}
	return strings.ToLower(string(r[0])) + string(r[1:])
}

var pastTense = map[string]string{
	"create": "Created", "add": "Added", "update": "Updated", "edit": "Updated", "delete": "Deleted", "remove": "Removed",
	"send": "Sent", "void": "Voided", "cancel": "Cancelled", "record": "Recorded", "mark": "Marked", "approve": "Approved",
	"search": "Searched", "list": "Listed", "get": "Looked up", "find": "Found", "show": "Looked up", "set": "Set",
	"convert": "Converted", "issue": "Issued", "complete": "Completed", "assign": "Assigned", "log": "Logged",
	"schedule": "Scheduled", "book": "Booked", "reschedule": "Rescheduled", "archive": "Archived", "restore": "Restored",
	"adjust": "Adjusted", "receive": "Received", "pay": "Paid", "refund": "Refunded", "close": "Closed", "reopen": "Reopened",
	"duplicate": "Duplicated", "move": "Moved", "check": "Checked", "summarize": "Summarised", "summarise": "Summarised",
}

func splitTitle(title string) (verb, rest string) {
	i := strings.IndexByte(title, ' ')
	if i < 0 {
		return title, ""
	}
	return title[:i], title[i+1:]
}

func runningLabel(t aitools.Tool) string {
	title := toolTitle(t, t.Name)
	verb, rest := splitTitle(title)
	v := strings.ToLower(verb)
	ing := ""
	switch {
	case strings.HasSuffix(v, "e") && !strings.HasSuffix(v, "ee"):
		ing = v[:len(v)-1] + "ing"
	case v == "get":
		ing = "getting"
	case v == "set":
		ing = "setting"
	default:
		ing = v + "ing"
	}
	if _, known := pastTense[v]; !known {
		return title + "…"
	}
	return strings.TrimSpace(upperFirst(ing)+" "+rest) + "…"
}

// entityOf guesses the entity type a tool acts on ("customers_create" -> "customers").
func entityOf(t aitools.Tool) string {
	parts := strings.Split(t.Name, "_")
	if len(parts) > 1 {
		return parts[0]
	}
	if t.Module != "" {
		return t.Module
	}
	return t.Name
}

var labelKeys = []string{"number", "invoice_number", "quote_number", "bill_number", "reference", "display_name", "name", "full_name", "customer_name", "first_name", "title", "subject", "email"}
var linkKeys = []string{"link", "url", "app_url", "web_url", "href"}

// unwrap returns the main object of an API-style result ({data: {...}}).
func unwrap(v any) any {
	for i := 0; i < 3; i++ {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		inner, has := m["data"]
		if !has {
			return v
		}
		if _, isMap := inner.(map[string]any); isMap {
			v = inner
			continue
		}
		if _, isArr := inner.([]any); isArr {
			return v
		}
		return v
	}
	return v
}

func findString(v any, keys []string) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			if k == "first_name" {
				if l, ok := m["last_name"].(string); ok && strings.TrimSpace(l) != "" {
					s = s + " " + l
				}
			}
			return strings.TrimSpace(s)
		}
	}
	// One level down for common wrappers.
	for _, w := range []string{"data", "item", "record", "result", "invoice", "customer"} {
		if inner, ok := m[w].(map[string]any); ok {
			if s := findString(inner, keys); s != "" {
				return s
			}
		}
	}
	return ""
}

func safeLink(s string) string {
	s = strings.TrimSpace(s)
	// Links into this web app become in-app paths.
	if base := aitools.AppURL(); base != "" && strings.HasPrefix(s, base+"/") {
		s = strings.TrimPrefix(s, base)
	}
	if strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//") {
		return s
	}
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		return s
	}
	return ""
}

// countOf finds the size of the first list in a result.
func countOf(v any) (int, bool) {
	switch x := v.(type) {
	case []any:
		return len(x), true
	case map[string]any:
		for _, k := range []string{"items", "results", "data", "records", "rows"} {
			if arr, ok := x[k].([]any); ok {
				return len(arr), true
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if arr, ok := x[k].([]any); ok {
				return len(arr), true
			}
		}
	}
	return 0, false
}

// resultMeta builds the chip shown for a finished tool call.
func resultMeta(t aitools.Tool, args map[string]any, out any) ToolMeta {
	title := toolTitle(t, t.Name)
	verb, rest := splitTitle(title)
	past := pastTense[strings.ToLower(verb)]
	if past == "" {
		past = verb
	}
	m := ToolMeta{Name: t.Name, Title: title, Status: "done", Write: !t.ReadOnly}
	obj := unwrap(out)
	m.Link = safeLink(findString(obj, linkKeys))
	if t.ReadOnly {
		summary := strings.TrimSpace(past + " " + rest)
		if n, ok := countOf(out); ok {
			summary += fmt.Sprintf(" · %d result", n)
			if n != 1 {
				summary += "s"
			}
		} else if l := findString(obj, labelKeys); l != "" {
			summary += " · " + truncate(l, 60)
		}
		m.Summary = summary
		return m
	}
	label := findString(obj, labelKeys)
	if label == "" {
		label = findString(args, labelKeys)
	}
	m.Summary = strings.TrimSpace(past + " " + rest)
	if label != "" {
		m.Summary += " " + truncate(label, 60)
	}
	m.Changed = entityOf(t)
	return m
}

// Field is one argument shown on a pending action card.
type Field struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Value string `json:"value"`
}

// describeAction builds the confirmation card for a pending tool call.
func describeAction(convID string, t aitools.Tool, u Block) map[string]any {
	var args map[string]any
	json.Unmarshal(u.Input, &args)
	if args == nil {
		args = map[string]any{}
	}
	title := toolTitle(t, u.Name)
	props, _ := t.InputSchema["properties"].(map[string]any)
	var order []string
	for k := range args {
		order = append(order, k)
	}
	// Schema order is not preserved in a Go map, so put identifying keys first.
	rank := map[string]int{}
	for i, k := range labelKeys {
		rank[k] = i + 1
	}
	sort.SliceStable(order, func(i, j int) bool {
		ri, rj := rank[order[i]], rank[order[j]]
		if ri == 0 {
			ri = 1000
		}
		if rj == 0 {
			rj = 1000
		}
		if ri != rj {
			return ri < rj
		}
		return order[i] < order[j]
	})
	var fields []Field
	for _, k := range order {
		label := humanKey(k)
		if p, ok := props[k].(map[string]any); ok {
			if tt, ok := p["title"].(string); ok && tt != "" {
				label = tt
			}
		}
		fields = append(fields, Field{Key: k, Label: label, Value: displayValue(args[k])})
	}
	summary := title
	if l := findString(args, labelKeys); l != "" {
		summary += ": " + truncate(l, 80)
	} else if len(fields) > 0 {
		var bits []string
		for i, f := range fields {
			if i >= 3 {
				break
			}
			bits = append(bits, f.Label+" "+truncate(f.Value, 40))
		}
		summary += " (" + strings.Join(bits, ", ") + ")"
	}
	return map[string]any{
		"conversation_id": convID, "id": u.ID, "name": u.Name, "title": title, "summary": summary,
		"description": t.Description, "destructive": t.Destructive, "fields": fields, "args": args,
	}
}

func humanKey(k string) string {
	k = strings.ReplaceAll(k, "_", " ")
	k = strings.TrimSuffix(k, " id")
	return upperFirst(k)
}

func displayValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "—"
	case string:
		return truncate(x, 400)
	case bool:
		if x {
			return "Yes"
		}
		return "No"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case []any:
		if len(x) == 0 {
			return "—"
		}
		var parts []string
		for i, it := range x {
			if i >= 10 {
				parts = append(parts, fmt.Sprintf("… %d more", len(x)-10))
				break
			}
			parts = append(parts, compactValue(it))
		}
		return strings.Join(parts, "\n")
	default:
		return compactValue(v)
	}
}

func compactValue(v any) string {
	if m, ok := v.(map[string]any); ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, humanKey(k)+": "+strings.ReplaceAll(displayValue(m[k]), "\n", "; "))
		}
		return truncate(strings.Join(parts, ", "), 300)
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return truncate(string(b), 300)
}
