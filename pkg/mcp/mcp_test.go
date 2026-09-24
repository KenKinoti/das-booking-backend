package mcp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

func testPrincipal(scope string) Principal {
	return Principal{Identity: aitools.Identity{UserID: "u1", OrgID: "o1", Role: "admin"}, Scope: scope, Kind: "token", TokenID: "t1"}
}

func call(t *testing.T, s *Server, p Principal, method, params string) rpcResponse {
	t.Helper()
	req := rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("7"), Method: method}
	if params != "" {
		req.Params = json.RawMessage(params)
	}
	resp, isReq := s.dispatch(nil, p, req)
	if !isReq {
		t.Fatalf("%s: expected a response", method)
	}
	return resp
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	s := New(nil)
	for req, want := range map[string]string{"2025-06-18": "2025-06-18", "2025-03-26": "2025-03-26", "1999-01-01": LatestProtocol, "": LatestProtocol} {
		r := call(t, s, testPrincipal(ScopeRead), "initialize", `{"protocolVersion":"`+req+`","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`)
		res := r.Result.(map[string]any)
		if res["protocolVersion"] != want {
			t.Fatalf("%q negotiated %v", req, res["protocolVersion"])
		}
		info := res["serverInfo"].(map[string]any)
		if info["name"] != ServerName || info["version"] == "" {
			t.Fatalf("serverInfo %v", info)
		}
		caps := res["capabilities"].(map[string]any)["tools"].(map[string]any)
		if caps["listChanged"] != false {
			t.Fatal("listChanged should be false")
		}
		if !strings.Contains(res["instructions"].(string), "whoami") {
			t.Fatal("instructions missing")
		}
	}
}

func TestToolsListAnnotationsAndScope(t *testing.T) {
	s := New(nil)
	rw := call(t, s, testPrincipal(ScopeReadWrite), "tools/list", "").Result.(map[string]any)["tools"].([]map[string]any)
	ro := call(t, s, testPrincipal(ScopeRead), "tools/list", "").Result.(map[string]any)["tools"].([]map[string]any)
	if len(rw) < 35 || len(ro) >= len(rw) || len(ro) == 0 {
		t.Fatalf("rw %d ro %d", len(rw), len(ro))
	}
	names := map[string]bool{}
	for _, tl := range rw {
		names[tl["name"].(string)] = true
		a := tl["annotations"].(map[string]any)
		for _, k := range []string{"title", "readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := a[k]; !ok {
				t.Fatalf("%s lacks %s", tl["name"], k)
			}
		}
		if a["openWorldHint"] != false {
			t.Fatal("openWorldHint must be false")
		}
		sch := tl["inputSchema"].(map[string]any)
		if sch["type"] != "object" {
			t.Fatalf("%s schema type %v", tl["name"], sch["type"])
		}
		if d, _ := tl["description"].(string); len(d) < 40 {
			t.Fatalf("%s description too short", tl["name"])
		}
	}
	for _, n := range []string{"whoami", "business_summary", "search_everything", "customers_create", "invoices_create", "quotes_create", "invoices_void", "bookings_create", "bookings_cancel", "inventory_adjust_stock", "purchase_orders_create", "leave_decide", "meetings_schedule"} {
		if !names[n] {
			t.Fatalf("missing tool %s", n)
		}
	}
	for _, tl := range ro {
		if tl["annotations"].(map[string]any)["readOnlyHint"] != true {
			t.Fatalf("read-only list contains %s", tl["name"])
		}
	}
	void, _ := aitools.Get("invoices_void")
	if !void.Destructive {
		t.Fatal("invoices_void must be destructive")
	}
}

func TestJSONRPCErrors(t *testing.T) {
	s := New(nil)
	p := testPrincipal(ScopeRead)
	if r := call(t, s, p, "no/such", ""); r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Fatalf("method not found: %+v", r)
	}
	if r := call(t, s, p, "tools/call", `{"name":"nope"}`); r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Fatalf("unknown tool: %+v", r)
	}
	if r := call(t, s, p, "tools/call", `[1]`); r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Fatalf("bad params: %+v", r)
	}
	r, _ := s.dispatch(nil, p, rpcRequest{JSONRPC: "1.0", ID: json.RawMessage("1"), Method: "ping"})
	if r.Error == nil || r.Error.Code != codeInvalidRequest {
		t.Fatalf("bad version: %+v", r)
	}
	if _, isReq := s.dispatch(nil, p, rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}); isReq {
		t.Fatal("notification must not get a response")
	}
	if r := call(t, s, p, "ping", ""); r.Error != nil {
		t.Fatal("ping failed")
	}
}

func TestReadOnlyScopeRefusesWriteTool(t *testing.T) {
	s := New(nil)
	r := call(t, s, testPrincipal(ScopeRead), "tools/call", `{"name":"customers_create","arguments":{"first_name":"A","email":"a@b.c"}}`)
	res := r.Result.(map[string]any)
	if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "read-only") {
		t.Fatalf("expected read-only refusal: %v", res)
	}
}

func TestArgumentValidation(t *testing.T) {
	tl, _ := aitools.Get("invoices_list")
	cases := map[string]string{
		`{"limit":"x"}`:        "limit must be an integer",
		`{"status":"bogus"}`:   "status must be one of",
		`{"unknown":1}`:        "unknown argument",
		`{"limit":2.5}`:        "limit must be an integer",
		`{"doc_type":"quote"}`: "",
	}
	for in, want := range cases {
		var args map[string]any
		_ = json.Unmarshal([]byte(in), &args)
		got := validateArgs(tl.InputSchema, args)
		if (want == "" && got != "") || (want != "" && !strings.Contains(got, want)) {
			t.Fatalf("%s: got %q want %q", in, got, want)
		}
	}
	cc, _ := aitools.Get("customers_create")
	if got := validateArgs(cc.InputSchema, map[string]any{}); !strings.Contains(got, "first_name is required") {
		t.Fatalf("required: %q", got)
	}
}

func TestPKCEAndRedirects(t *testing.T) {
	verifier := strings.Repeat("a", 43)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if !pkceMatches(verifier, challenge) || pkceMatches(verifier+"b", challenge) || pkceMatches("short", challenge) {
		t.Fatal("pkce")
	}
	for u, ok := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback": true,
		"http://localhost:6274/callback":          true,
		"http://127.0.0.1:33418/cb":               true,
		"http://example.com/cb":                   false,
		"https://x.test/cb#frag":                  false,
		"myapp://cb":                              false,
		"javascript:alert(1)":                     false,
	} {
		if validRedirect(u) != ok {
			t.Fatalf("%s: expected %v", u, ok)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	t.Setenv("PUBLIC_APP_URL", "https://app.example.com")
	t.Setenv("MCP_ALLOWED_ORIGINS", "https://tools.example.org")
	for o, ok := range map[string]bool{"": true, "https://claude.ai": true, "https://app.example.com": true, "http://localhost:6274": true,
		"https://tools.example.org": true, "https://evil.example": false, "null": false, "http://localhost.evil.com": false} {
		if originAllowed(o) != ok {
			t.Fatalf("%q: expected %v", o, ok)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(3)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if ok, _ := l.take("k", 1, now); !ok {
			t.Fatal("should allow")
		}
	}
	if ok, wait := l.take("k", 1, now); ok || wait <= 0 {
		t.Fatal("should limit")
	}
	if ok, _ := l.take("other", 1, now); !ok {
		t.Fatal("per key")
	}
	if ok, _ := l.take("k", 1, now.Add(30*time.Second)); !ok {
		t.Fatal("should refill")
	}
}

func TestSummarizeArgsRedacts(t *testing.T) {
	s := SummarizeArgs(map[string]any{"name": "Bob", "password": "hunter2", "nested": map[string]any{"api_key": "x"}, "long": strings.Repeat("z", 500)})
	if strings.Contains(s, "hunter2") || strings.Contains(s, `"x"`) || !strings.Contains(s, "[redacted]") || strings.Contains(s, strings.Repeat("z", 300)) {
		t.Fatal(s)
	}
}

func TestHTTPWithoutDB(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := New(nil)
	r := gin.New()
	s.RegisterRoot(r)
	do := func(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w := do("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, nil)
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), `resource_metadata="http://example.com/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("401: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	if w := do("POST", "/mcp", `{}`, map[string]string{"Origin": "https://evil.example"}); w.Code != 403 {
		t.Fatalf("origin: %d", w.Code)
	}
	if w := do("POST", "/mcp", `{}`, map[string]string{"MCP-Protocol-Version": "1999-01-01"}); w.Code != 400 {
		t.Fatalf("version: %d", w.Code)
	}
	if w := do("GET", "/mcp", "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
	w = do("GET", "/.well-known/oauth-authorization-server", "", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "api.example.com"})
	var md map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &md)
	if md["issuer"] != "https://api.example.com" || md["token_endpoint"] != "https://api.example.com/oauth/token" || md["code_challenge_methods_supported"].([]any)[0] != "S256" {
		t.Fatalf("as metadata %v", md)
	}
	w = do("GET", "/.well-known/oauth-protected-resource/mcp", "", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "api.example.com"})
	_ = json.Unmarshal(w.Body.Bytes(), &md)
	if md["resource"] != "https://api.example.com/mcp" {
		t.Fatalf("prm %v", md)
	}
}

func TestErrorTextPlan(t *testing.T) {
	err := &aitools.APIError{Status: 402, Message: "x", Body: map[string]any{"error": map[string]any{"code": "MODULE_NOT_IN_PLAN", "module": "invoicing"}}}
	if got := aitools.ErrorText(err); !strings.HasPrefix(got, "Not in your plan: Invoicing") {
		t.Fatal(got)
	}
}
