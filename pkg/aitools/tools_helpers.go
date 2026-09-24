package aitools

// Shared helpers for the tool definitions in tools_*.go (owned by the MCP
// work). Unexported helpers are prefixed with "t" (schema/response helpers) or
// "arg" (argument readers) to avoid clashing with other files in the package.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Errors

// ToolError is a user-facing tool failure (bad input, not found, not allowed).
// Its message is safe to show to the model and the user.
type ToolError struct{ Msg string }

func (e *ToolError) Error() string { return e.Msg }

func tErr(format string, a ...any) error { return &ToolError{Msg: fmt.Sprintf(format, a...)} }

var moduleLabels = map[string]string{
	"core": "Core", "smallbiz": "Business hub", "invoicing": "Invoicing & quotes", "pos": "Point of sale",
	"ecommerce": "E-commerce", "crm": "CRM", "bookings": "Bookings", "events": "Events", "inventory": "Inventory & suppliers",
	"manufacturing": "Manufacturing", "accounting": "Accounting & finance", "hr": "HR & payroll", "projects": "Projects",
	"documents": "Documents", "communication": "Meetings & messaging",
}

// ModuleLabel is a readable name for a plan module key.
func ModuleLabel(k string) string {
	if l, ok := moduleLabels[k]; ok {
		return l
	}
	return k
}

// ErrorText turns any tool error into a friendly, single-paragraph message.
// 402 plan errors become "Not in your plan: <module>".
func ErrorText(err error) string {
	if err == nil {
		return ""
	}
	var te *ToolError
	if errors.As(err, &te) {
		return te.Msg
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return apiErrorText(ae)
	}
	return err.Error()
}

func apiErrorText(ae *APIError) string {
	body, _ := ae.Body.(map[string]any)
	var e map[string]any
	if body != nil {
		e, _ = body["error"].(map[string]any)
	}
	switch ae.Status {
	case 402:
		mod := ""
		if e != nil {
			mod, _ = e["module"].(string)
		}
		if mod != "" {
			return "Not in your plan: " + ModuleLabel(mod) + ". An administrator can add it under Plan & billing."
		}
		return "Not in your plan: " + ae.Message
	case 401:
		return "Not signed in or the session expired."
	case 403:
		return "You don't have permission to do that (" + ae.Message + ")."
	}
	msg := ae.Message
	// Field-level validation details, if any.
	if e != nil {
		if f, ok := e["fields"].(map[string]any); ok && len(f) > 0 {
			keys := make([]string, 0, len(f))
			for k := range f {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := []string{}
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s: %v", k, f[k]))
			}
			msg += " (" + strings.Join(parts, "; ") + ")"
		}
	}
	if ae.Status == 404 && !strings.Contains(strings.ToLower(msg), "not found") {
		msg = "Not found: " + msg
	}
	return msg
}

// api wraps Call and converts API failures into friendly ToolErrors.
func (c *Ctx) api(method, path string, q url.Values, body any) (any, error) {
	v, err := c.Call(method, path, q, body)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			return nil, &ToolError{Msg: apiErrorText(ae)}
		}
		return nil, err
	}
	return v, nil
}

func (c *Ctx) get(path string, q url.Values) (any, error) { return c.api("GET", path, q, nil) }

// ---------------------------------------------------------------------------
// JSON Schema builders

func tObj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func tStr(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func tEnum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}

func tDate(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc + " (YYYY-MM-DD)", "pattern": `^\d{4}-\d{2}-\d{2}$`}
}

func tNum(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }

func tInt(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}

func tBool(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }

func tArr(desc string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": items}
}

func tLimit() map[string]any { return tInt("Maximum results to return (default 20)", 1, 100) }

// ---------------------------------------------------------------------------
// Argument readers

func argStr(a map[string]any, k string) string {
	switch v := a[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func argHas(a map[string]any, k string) bool {
	v, ok := a[k]
	return ok && v != nil
}

func argNum(a map[string]any, k string) (float64, bool) {
	switch v := a[k].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

func argInt(a map[string]any, k string, def int) int {
	if f, ok := argNum(a, k); ok {
		return int(f)
	}
	return def
}

func argBool(a map[string]any, k string) (bool, bool) {
	switch v := a[k].(type) {
	case bool:
		return v, true
	case string:
		b, err := strconv.ParseBool(v)
		return b, err == nil
	}
	return false, false
}

func argList(a map[string]any, k string) []any {
	if l, ok := a[k].([]any); ok {
		return l
	}
	return nil
}

func argStrList(a map[string]any, k string) []string {
	out := []string{}
	for _, v := range argList(a, k) {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func argLimit(a map[string]any) int {
	n := argInt(a, "limit", 20)
	if n < 1 {
		n = 1
	}
	if n > 100 {
		n = 100
	}
	return n
}

func argRequire(a map[string]any, keys ...string) error {
	for _, k := range keys {
		if argStr(a, k) == "" {
			return tErr("%s is required", k)
		}
	}
	return nil
}

// copyArgs copies present arguments into a request body (optionally renamed).
func copyArgs(dst, a map[string]any, keys ...string) {
	for _, k := range keys {
		from, to := k, k
		if i := strings.Index(k, ":"); i > 0 {
			from, to = k[:i], k[i+1:]
		}
		if argHas(a, from) {
			v := a[from]
			if s, ok := v.(string); ok {
				v = strings.TrimSpace(s)
			}
			dst[to] = v
		}
	}
}

// ---------------------------------------------------------------------------
// Response helpers

// tDig walks nested maps.
func tDig(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// tData unwraps {"success":true,"data":X} envelopes.
func tData(v any) any {
	if m, ok := v.(map[string]any); ok {
		if d, ok := m["data"]; ok && d != nil {
			return d
		}
	}
	return v
}

// tList extracts an array of objects from a response: data.<key>, <key>, or
// data / the response itself when it is an array.
func tList(v any, keys ...string) []map[string]any {
	d := tData(v)
	cands := []any{}
	for _, k := range keys {
		cands = append(cands, tDig(d, k), tDig(v, k))
	}
	cands = append(cands, d, v)
	for _, c := range cands {
		if arr, ok := c.([]any); ok {
			out := make([]map[string]any, 0, len(arr))
			for _, x := range arr {
				if m, ok := x.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out
		}
	}
	return []map[string]any{}
}

// tObjOf extracts an object from data.<key>, <key>, data or the response.
func tObjOf(v any, keys ...string) map[string]any {
	d := tData(v)
	for _, k := range keys {
		if m, ok := tDig(d, k).(map[string]any); ok {
			return m
		}
		if m, ok := tDig(v, k).(map[string]any); ok {
			return m
		}
	}
	if m, ok := d.(map[string]any); ok {
		return m
	}
	m, _ := v.(map[string]any)
	return m
}

// tPick copies the listed keys (dotted paths allowed, "a.b:alias" renames),
// dropping empty values.
func tPick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	if m == nil {
		return out
	}
	for _, k := range keys {
		src, dst := k, k
		if i := strings.Index(k, ":"); i > 0 {
			src, dst = k[:i], k[i+1:]
		}
		var v any = m
		for _, p := range strings.Split(src, ".") {
			v = tDig(v, p)
		}
		if tEmpty(v) {
			continue
		}
		if !strings.Contains(k, ":") && strings.Contains(src, ".") {
			dst = strings.ReplaceAll(src, ".", "_")
		}
		out[dst] = v
	}
	return out
}

func tEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func tS(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func tF(m map[string]any, k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}

func tRound(f float64) float64 { return math.Round(f*100) / 100 }

func tJoinName(m map[string]any) string {
	n := strings.TrimSpace(tS(m, "first_name") + " " + tS(m, "last_name"))
	if n == "" {
		n = tS(m, "name")
	}
	return n
}

// tDay trims an ISO timestamp to its date.
func tDay(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// tLimitList truncates a list and reports whether it was cut.
func tLimitList[T any](l []T, n int) ([]T, bool) {
	if len(l) > n {
		return l[:n], true
	}
	return l, false
}

// tResult builds a list result {count, total?, truncated?, <key>: items}.
func tResult(key string, items any, total int, truncated bool) map[string]any {
	out := map[string]any{key: items, "count": lenOf(items)}
	if total > 0 {
		out["total"] = total
	}
	if truncated {
		out["truncated"] = true
		out["hint"] = "More results exist; narrow the search or raise limit."
	}
	return out
}

func lenOf(v any) int {
	switch x := v.(type) {
	case []map[string]any:
		return len(x)
	case []any:
		return len(x)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Links

// AppURL is the web app base URL (env PUBLIC_APP_URL, no trailing slash).
func AppURL() string { return strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_APP_URL")), "/") }

// tURL builds a deep link into the web app for a path like "/invoices/123".
func tURL(path string) string { return AppURL() + path }

// ---------------------------------------------------------------------------
// Time

func (c *Ctx) loc() *time.Location {
	if c.Who.TZ != "" {
		if l, err := time.LoadLocation(c.Who.TZ); err == nil {
			return l
		}
	}
	return time.UTC
}

func (c *Ctx) today() string { return time.Now().In(c.loc()).Format("2006-01-02") }

// parseWhen accepts RFC3339 or a local "YYYY-MM-DD HH:MM" / "YYYY-MM-DDTHH:MM"
// in the caller's time zone.
func (c *Ctx) parseWhen(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, c.loc()); err == nil {
			return t, nil
		}
	}
	return time.Time{}, tErr("Invalid date/time %q: use YYYY-MM-DDTHH:MM (local time) or RFC3339", s)
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// localTime formats an RFC3339 timestamp in the caller's time zone.
func (c *Ctx) localTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.In(c.loc()).Format("2006-01-02T15:04:05-07:00")
}

// ---------------------------------------------------------------------------
// Tool metadata

// Idempotent reports whether repeating a write tool with the same arguments
// has no additional effect (used for MCP idempotentHint).
func Idempotent(t Tool) bool {
	if t.ReadOnly {
		return true
	}
	return idempotentTools[t.Name]
}

var idempotentTools = map[string]bool{}

// Area groups tools for display (derived from the module).
func Area(t Tool) string {
	switch t.Module {
	case "", "core":
		if strings.HasPrefix(t.Name, "customers") {
			return "Customers"
		}
		return "Overview"
	}
	return ModuleLabel(t.Module)
}

func quote(s string) string { return strconv.Quote(s) }

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func itoa(i int) string { return strconv.Itoa(i) }
