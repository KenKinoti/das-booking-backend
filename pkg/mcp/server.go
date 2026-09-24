package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

// Protocol versions.
const (
	LatestProtocol = "2025-06-18"
	ServerName     = "dasyin-erp"
	MaxBodyBytes   = 1 << 20 // 1 MiB
	ToolTimeout    = 60 * time.Second
)

var supportedProtocols = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

func protocolSupported(v string) bool {
	for _, p := range supportedProtocols {
		if p == v {
			return true
		}
	}
	return false
}

// Version is the server version reported to clients (env APP_VERSION).
func Version() string {
	if v := os.Getenv("APP_VERSION"); v != "" {
		return v
	}
	return handlers.AppVersion()
}

// Instructions is sent to clients on initialize.
const Instructions = `DASYIN ERP is a business management system for small and medium businesses: customers & contacts, invoices and quotes, bookings/appointments, services & staff, inventory, suppliers & purchase orders, point of sale, a small-business cash book with reminders and P&L, CRM leads & deals, events, HR (employees & leave), meetings and projects.

How to use these tools well:
- Call whoami first to learn the organisation, enabled modules, default currency, tax rate, time zone and today's date. Only modules in the organisation's plan work; others return "Not in your plan".
- Find records before acting: use search_everything or the *_search / *_list tools to get ids; never guess ids. When several records match, ask the user which one.
- Dates are YYYY-MM-DD; times are local business time (YYYY-MM-DDTHH:MM) unless an RFC3339 offset is given. Money is in the record's currency; don't convert currencies yourself.
- Creating invoices/quotes makes drafts; only email them (invoices_send) when the user asks. Confirm before destructive actions (void, cancel) and summarise what will change before bulk updates.
- Every result includes a "url" deep link to the record in the web app — share it so the user can check the change.
- Your access follows the user's role and the connection's scope; a read-only connection cannot change data.`

// Server serves the MCP endpoint, OAuth and the settings API.
type Server struct {
	DB      *gorm.DB
	Now     func() time.Time
	limiter *limiter
	// RatePerMinute limits JSON-RPC requests per token (default 120).
	RatePerMinute int
	regLimiter    *limiter
}

// New creates a server.
func New(db *gorm.DB) *Server {
	s := &Server{DB: db, RatePerMinute: 120}
	s.limiter = newLimiter(s.RatePerMinute)
	s.regLimiter = newLimiter(20)
	return s
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// ---------------------------------------------------------------------------
// JSON-RPC types

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

func errResp(id json.RawMessage, code int, msg string) rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (r rpcRequest) isRequest() bool {
	id := strings.TrimSpace(string(r.ID))
	return id != "" && id != "null"
}

// ---------------------------------------------------------------------------
// HTTP transport

func (s *Server) setCORS(c *gin.Context) {
	if o := c.GetHeader("Origin"); o != "" && originAllowed(o) {
		c.Header("Access-Control-Allow-Origin", o)
		c.Header("Vary", "Origin")
		c.Header("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id, MCP-Protocol-Version, Last-Event-ID")
		c.Header("Access-Control-Expose-Headers", "Mcp-Session-Id, MCP-Protocol-Version, WWW-Authenticate")
	}
}

// unauthorized answers 401 with the RFC 9728 resource metadata pointer.
func (s *Server) unauthorized(c *gin.Context, hadToken bool) {
	v := `Bearer resource_metadata="` + ResourceMetadataURL(c.Request) + `"`
	if hadToken {
		v += `, error="invalid_token", error_description="The access token is invalid, expired or revoked"`
	}
	c.Header("WWW-Authenticate", v)
	c.AbortWithStatusJSON(http.StatusUnauthorized, errResp(nil, -32001, "Unauthorized: connect with OAuth or send Authorization: Bearer dsy_… (create a token in Settings → AI & MCP)"))
}

// HandleMCP serves POST /mcp (Streamable HTTP, JSON responses).
func (s *Server) HandleMCP(c *gin.Context) {
	s.setCORS(c)
	if !originAllowed(c.GetHeader("Origin")) {
		c.AbortWithStatusJSON(http.StatusForbidden, errResp(nil, codeInvalidRequest, "Origin not allowed"))
		return
	}
	if v := c.GetHeader("MCP-Protocol-Version"); v != "" && !protocolSupported(v) {
		c.AbortWithStatusJSON(http.StatusBadRequest, errResp(nil, codeInvalidRequest, "Unsupported MCP-Protocol-Version "+v+"; supported: "+strings.Join(supportedProtocols, ", ")))
		return
	}
	tok := bearer(c)
	if tok == "" {
		s.unauthorized(c, false)
		return
	}
	p, err := s.authenticate(tok)
	if err != nil {
		s.unauthorized(c, true)
		return
	}
	if sid := c.GetHeader("Mcp-Session-Id"); sid != "" {
		c.Header("Mcp-Session-Id", sid)
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, errResp(nil, codeInvalidRequest, "Request body too large (max 1 MiB)"))
		return
	}
	s.serveRPC(c, p, body)
}

func (s *Server) serveRPC(c *gin.Context, p Principal, body []byte) {
	body = bytes.TrimSpace(body)
	var msgs []rpcRequest
	batch := len(body) > 0 && body[0] == '['
	if batch {
		if err := json.Unmarshal(body, &msgs); err != nil {
			c.JSON(http.StatusBadRequest, errResp(nil, codeParse, "Parse error: "+err.Error()))
			return
		}
		if len(msgs) == 0 {
			c.JSON(http.StatusBadRequest, errResp(nil, codeInvalidRequest, "Empty batch"))
			return
		}
		if len(msgs) > 50 {
			c.JSON(http.StatusBadRequest, errResp(nil, codeInvalidRequest, "Batch too large (max 50)"))
			return
		}
	} else {
		var m rpcRequest
		if err := json.Unmarshal(body, &m); err != nil {
			c.JSON(http.StatusBadRequest, errResp(nil, codeParse, "Parse error: invalid JSON"))
			return
		}
		msgs = []rpcRequest{m}
	}
	n := 0
	for _, m := range msgs {
		if m.isRequest() {
			n++
		}
	}
	if n > 0 {
		if ok, wait := s.limiter.take(p.key(), n, s.now()); !ok {
			c.Header("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
			c.JSON(http.StatusTooManyRequests, errResp(nil, -32029, fmt.Sprintf("Rate limit exceeded (%d requests per minute). Retry in %ds.", s.RatePerMinute, int(wait.Seconds())+1)))
			return
		}
	}
	var out []rpcResponse
	for _, m := range msgs {
		resp, isReq := s.dispatch(c, p, m)
		if isReq {
			out = append(out, resp)
		}
	}
	if len(out) == 0 {
		c.Status(http.StatusAccepted)
		return
	}
	c.Header("Cache-Control", "no-store")
	if batch {
		c.JSON(http.StatusOK, out)
		return
	}
	c.JSON(http.StatusOK, out[0])
}

// dispatch handles one message. isReq is false for notifications/responses.
func (s *Server) dispatch(c *gin.Context, p Principal, m rpcRequest) (rpcResponse, bool) {
	if !m.isRequest() {
		// Notifications (notifications/initialized, cancelled …) and client
		// responses need no answer.
		return rpcResponse{}, false
	}
	if m.JSONRPC != "2.0" {
		return errResp(m.ID, codeInvalidRequest, `jsonrpc must be "2.0"`), true
	}
	if m.Method == "" {
		return errResp(m.ID, codeInvalidRequest, "method is required"), true
	}
	ok := func(v any) (rpcResponse, bool) { return rpcResponse{JSONRPC: "2.0", ID: m.ID, Result: v}, true }
	switch m.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string         `json:"protocolVersion"`
			ClientInfo      map[string]any `json:"clientInfo"`
		}
		if len(m.Params) > 0 {
			if err := json.Unmarshal(m.Params, &params); err != nil {
				return errResp(m.ID, codeInvalidParams, "Invalid initialize params"), true
			}
		}
		pv := params.ProtocolVersion
		if !protocolSupported(pv) {
			pv = LatestProtocol
		}
		if c != nil {
			c.Header("Mcp-Session-Id", "mcp-"+randomString(32))
			c.Header("MCP-Protocol-Version", pv)
		}
		return ok(map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": ServerName, "title": "DASYIN ERP", "version": Version()},
			"instructions":    Instructions,
		})
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		return ok(map[string]any{"tools": ToolDescriptors(p.CanWrite())})
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil || params.Name == "" {
			return errResp(m.ID, codeInvalidParams, "tools/call needs {name, arguments}"), true
		}
		t, found := aitools.Get(params.Name)
		if !found {
			return errResp(m.ID, codeInvalidParams, "Unknown tool: "+params.Name), true
		}
		var ctx context.Context = context.Background()
		if c != nil {
			ctx = c.Request.Context()
		}
		return ok(s.callTool(ctx, p, t, params.Arguments))
	case "resources/list":
		return ok(map[string]any{"resources": []any{}})
	case "resources/templates/list":
		return ok(map[string]any{"resourceTemplates": []any{}})
	case "prompts/list":
		return ok(map[string]any{"prompts": []any{}})
	}
	return errResp(m.ID, codeMethodNotFound, "Method not found: "+m.Method), true
}

// ToolDescriptors is the tools/list payload (write tools only when allowed).
func ToolDescriptors(includeWrite bool) []map[string]any {
	out := []map[string]any{}
	for _, t := range aitools.All() {
		if !t.ReadOnly && !includeWrite {
			continue
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": t.Description,
			"inputSchema": t.InputSchema,
			"annotations": map[string]any{
				"title":           t.Title,
				"readOnlyHint":    t.ReadOnly,
				"destructiveHint": t.Destructive,
				"idempotentHint":  aitools.Idempotent(t),
				"openWorldHint":   false,
			},
		})
	}
	return out
}

func textResult(v any, isErr bool) map[string]any {
	var text string
	if s, ok := v.(string); ok {
		text = s
	} else {
		b, err := json.Marshal(v)
		if err != nil {
			text = fmt.Sprint(v)
		} else {
			text = string(b)
		}
	}
	res := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	if isErr {
		res["isError"] = true
	}
	return res
}

// callTool runs a tool for the principal and audits it.
func (s *Server) callTool(ctx context.Context, p Principal, t aitools.Tool, args map[string]any) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	started := time.Now()
	var result any
	var errMsg string
	switch {
	case !t.ReadOnly && !p.CanWrite():
		errMsg = "This connection has read-only access, so " + t.Name + " can't run. Reconnect with read & write access (or use a read & write token) to make changes."
	default:
		if msg := validateArgs(t.InputSchema, args); msg != "" {
			errMsg = "Invalid arguments for " + t.Name + ": " + msg
			break
		}
		cctx, cancel := context.WithTimeout(ctx, ToolTimeout)
		func() {
			defer cancel()
			defer func() {
				if r := recover(); r != nil {
					errMsg = fmt.Sprintf("Internal error in %s", t.Name)
				}
			}()
			v, err := t.Handler(&aitools.Ctx{Context: cctx, Who: p.Identity}, args)
			if err != nil {
				errMsg = aitools.ErrorText(err)
				return
			}
			result = v
		}()
	}
	LogActivity(s.DB, Activity{
		OrgID: p.OrgID, UserID: p.UserID, Source: p.Kind, TokenID: p.TokenID, ClientName: p.ClientName,
		Tool: t.Name, ReadOnly: t.ReadOnly, OK: errMsg == "", Error: errMsg,
		DurationMS: time.Since(started).Milliseconds(), Args: SummarizeArgs(args),
	})
	if errMsg != "" {
		return textResult(errMsg, true)
	}
	res := textResult(result, false)
	switch v := result.(type) {
	case map[string]any:
		res["structuredContent"] = v
	case nil:
	default:
		// structuredContent must be an object.
		b, _ := json.Marshal(v)
		var m map[string]any
		if json.Unmarshal(b, &m) == nil && m != nil {
			res["structuredContent"] = m
		} else {
			var raw any
			_ = json.Unmarshal(b, &raw)
			res["structuredContent"] = map[string]any{"result": raw}
		}
	}
	return res
}

// validateArgs does a light JSON-Schema check of the top-level arguments:
// required keys, unknown keys, primitive types and enums.
func validateArgs(schema map[string]any, args map[string]any) string {
	props, _ := schema["properties"].(map[string]any)
	var problems []string
	if req, ok := schema["required"].([]string); ok {
		for _, k := range req {
			v, present := args[k]
			if !present || v == nil || v == "" {
				problems = append(problems, k+" is required")
			}
		}
	}
	if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
		var unknown []string
		for k := range args {
			if _, ok := props[k]; !ok {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			allowed := make([]string, 0, len(props))
			for k := range props {
				allowed = append(allowed, k)
			}
			sort.Strings(allowed)
			problems = append(problems, "unknown argument(s) "+strings.Join(unknown, ", ")+" (allowed: "+strings.Join(allowed, ", ")+")")
		}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ps, ok := props[k].(map[string]any)
		v := args[k]
		if !ok || v == nil {
			continue
		}
		typ, _ := ps["type"].(string)
		bad := false
		switch typ {
		case "string":
			_, isStr := v.(string)
			bad = !isStr
		case "number":
			_, isNum := v.(float64)
			bad = !isNum
		case "integer":
			f, isNum := v.(float64)
			bad = !isNum || f != float64(int64(f))
		case "boolean":
			_, isB := v.(bool)
			bad = !isB
		case "array":
			_, isA := v.([]any)
			bad = !isA
		case "object":
			_, isO := v.(map[string]any)
			bad = !isO
		}
		if bad {
			art := "a "
			if typ == "integer" || typ == "array" || typ == "object" {
				art = "an "
			}
			problems = append(problems, k+" must be "+art+typ)
			continue
		}
		if enum, ok := ps["enum"].([]string); ok {
			sv, _ := v.(string)
			found := false
			for _, e := range enum {
				if e == sv {
					found = true
				}
			}
			if !found {
				problems = append(problems, k+" must be one of "+strings.Join(enum, ", "))
			}
		}
	}
	return strings.Join(problems, "; ")
}
