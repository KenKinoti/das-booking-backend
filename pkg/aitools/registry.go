// Package aitools is the single registry of AI-callable actions ("tools")
// shared by the MCP server (pkg/mcp) and the in-app assistant (pkg/assistant).
//
// Every tool executes through the ERP's own HTTP API in-process (see Ctx.Call),
// authenticated as the calling user. That way org scoping, role checks, plan
// entitlements (402 MODULE_NOT_IN_PLAN) and validation are exactly the same as
// in the web app — tools never touch the database directly.
package aitools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Engine is the root HTTP handler (the gin engine). main.go sets it after all
// routes are registered.
var Engine http.Handler

// JWTSecret signs the short-lived internal tokens used by Ctx.Call.
var JWTSecret string

// Identity is who a tool runs as.
type Identity struct {
	UserID string
	OrgID  string
	Role   string
	Email  string
	TZ     string // IANA time zone of the caller, optional
}

// Ctx is passed to every tool handler.
type Ctx struct {
	context.Context
	Who Identity
}

// Tool describes one action.
type Tool struct {
	Name        string         // snake_case, unique, e.g. "customers_search"
	Title       string         // human title
	Description string         // what it does, when to use it, what it returns
	InputSchema map[string]any // JSON Schema (type: object)
	ReadOnly    bool           // true = no side effects
	Destructive bool           // deletes/voids/irreversible — assistant must confirm; MCP marks destructiveHint
	Module      string         // plan module key (informational; the API enforces entitlements)
	Handler     func(c *Ctx, args map[string]any) (any, error)
}

var (
	mu    sync.RWMutex
	tools = map[string]Tool{}
)

// Register adds a tool (call from init()).
func Register(t Tool) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := tools[t.Name]; dup {
		panic("aitools: duplicate tool " + t.Name)
	}
	if t.InputSchema == nil {
		t.InputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	tools[t.Name] = t
}

// All returns every tool sorted by name.
func All() []Tool {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get finds a tool by name.
func Get(name string) (Tool, bool) {
	mu.RLock()
	defer mu.RUnlock()
	t, ok := tools[name]
	return t, ok
}

// APIError is returned by Call for non-2xx responses.
type APIError struct {
	Status  int
	Message string
	Body    any
}

func (e *APIError) Error() string { return fmt.Sprintf("API %d: %s", e.Status, e.Message) }

// Call performs an in-process request to /api/v1<path> as the calling user and
// decodes the JSON response. query may be nil. body may be nil.
func (c *Ctx) Call(method, path string, query url.Values, body any) (any, error) {
	if Engine == nil {
		return nil, fmt.Errorf("aitools: engine not initialised")
	}
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	u := "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, u, rdr)
	if c.Context != nil {
		req = req.WithContext(c.Context)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-By", "dasyin-ai")
	if c.Who.TZ != "" {
		req.Header.Set("X-Timezone", c.Who.TZ)
	}
	rec := httptest.NewRecorder()
	Engine.ServeHTTP(rec, req)
	var out any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			out = strings.TrimSpace(rec.Body.String())
		}
	}
	if rec.Code >= 300 {
		return nil, &APIError{Status: rec.Code, Message: errorMessage(out, rec.Code), Body: out}
	}
	return out, nil
}

func errorMessage(v any, code int) string {
	if m, ok := v.(map[string]any); ok {
		for _, k := range []string{"error", "message"} {
			switch e := m[k].(type) {
			case string:
				return e
			case map[string]any:
				if s, ok := e["message"].(string); ok {
					return s
				}
			}
		}
	}
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return http.StatusText(code)
}

// token mints a 5-minute access token for the identity, in the same shape the
// login handler issues (user_id, org_id, role, email, type=access).
func (c *Ctx) token() (string, error) {
	if JWTSecret == "" {
		return "", fmt.Errorf("aitools: JWT secret not set")
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"user_id": c.Who.UserID,
		"org_id":  c.Who.OrgID,
		"role":    c.Who.Role,
		"email":   c.Who.Email,
		"iat":     now.Unix(),
		"exp":     now.Add(5 * time.Minute).Unix(),
		"src":     "ai",
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(JWTSecret))
}
