package mcp

// End-to-end tests against Postgres. Set TEST_DATABASE_URL to a migrated ERP
// database, e.g.
//   TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/das_mcp?sslmode=disable&search_path=erp'

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/middleware"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

const testSecret = "mcp-test-secret-0123456789abcdef"

type env struct {
	t      *testing.T
	db     *gorm.DB
	s      *Server
	r      *gin.Engine
	userID string
	orgID  string
}

func setup(t *testing.T) *env {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUBLIC_APP_URL", "https://app.test")
	t.Setenv("PUBLIC_API_URL", "")
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{JWTSecret: testSecret, Environment: "test"}
	r := gin.New()
	handlers.NewHandler(db, cfg).SetupRoutes(r)
	s := New(db)
	s.RegisterRoot(r)
	auth := r.Group("/api/v1", middleware.AuthRequired(cfg))
	handlers.NewHandler(db, cfg).RegisterCustomerContactRoutes(auth)
	invoicing.NewHandler(db).RegisterRoutes(auth.Group("/invoicing"))
	s.RegisterAPI(auth)
	aitools.Engine = r
	aitools.JWTSecret = testSecret

	e := &env{t: t, db: db, s: s, r: r}
	org := models.Organization{ID: uuid.NewString(), Name: "MCP Test Org", BusinessType: "general", ABN: fmt.Sprint(time.Now().UnixNano() % 100000000000)}
	if err := db.Create(&org).Error; err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("x"), bcrypt.MinCost)
	u := models.User{ID: uuid.NewString(), Email: "mcp-" + uuid.NewString()[:8] + "@test.example", PasswordHash: string(hash), FirstName: "Mia", LastName: "Test", Role: "admin", OrganizationID: org.ID, IsActive: true}
	if err := db.Omit("Organization").Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	e.userID, e.orgID = u.ID, org.ID
	return e
}

func (e *env) jwt() string {
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"user_id": e.userID, "org_id": e.orgID, "role": "admin", "email": "x", "exp": time.Now().Add(time.Hour).Unix()}).SignedString([]byte(testSecret))
	return tok
}

func (e *env) do(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if strings.HasPrefix(body, "{") || strings.HasPrefix(body, "[") {
		req.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

// pat creates a personal access token through the settings API.
func (e *env) pat(scope string, days int) (string, string) {
	body := fmt.Sprintf(`{"name":"test %s","scope":%q,"expires_in_days":%d}`, scope, scope, days)
	w := e.do("POST", "/api/v1/mcp/tokens", body, map[string]string{"Authorization": "Bearer " + e.jwt()})
	if w.Code != 201 {
		e.t.Fatalf("create token: %d %s", w.Code, w.Body.String())
	}
	var res struct {
		Data struct {
			Secret string `json:"secret"`
			Token  Token  `json:"token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return res.Data.Secret, res.Data.Token.ID
}

func (e *env) rpc(token, method string, params any) (int, map[string]any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	w := e.do("POST", "/mcp", string(b), map[string]string{"Authorization": "Bearer " + token})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// tool calls a tool and returns its structured result (fails on tool errors).
func (e *env) tool(token, name string, args map[string]any) map[string]any {
	e.t.Helper()
	code, out := e.rpc(token, "tools/call", map[string]any{"name": name, "arguments": args})
	if code != 200 || out["result"] == nil {
		e.t.Fatalf("%s: %d %v", name, code, out)
	}
	res := out["result"].(map[string]any)
	if res["isError"] == true {
		e.t.Fatalf("%s failed: %v", name, res["content"])
	}
	sc, _ := res["structuredContent"].(map[string]any)
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	var fromText map[string]any
	if err := json.Unmarshal([]byte(txt), &fromText); err != nil || fromText == nil {
		e.t.Fatalf("%s: text content is not JSON: %s", name, txt)
	}
	return sc
}

func TestPATAuth(t *testing.T) {
	e := setup(t)
	rw, _ := e.pat(ScopeReadWrite, 0)
	ro, _ := e.pat(ScopeRead, 30)
	revoked, revokedID := e.pat(ScopeReadWrite, 0)
	if !strings.HasPrefix(rw, "dsy_") || len(rw) != 44 {
		t.Fatalf("token format %q", rw)
	}
	if w := e.do("DELETE", "/api/v1/mcp/tokens/"+revokedID, "", map[string]string{"Authorization": "Bearer " + e.jwt()}); w.Code != 200 {
		t.Fatalf("revoke %d", w.Code)
	}
	expired, expiredID := e.pat(ScopeRead, 1)
	e.db.Model(&Token{}).Where("id = ?", expiredID).Update("expires_at", time.Now().Add(-time.Hour))

	// Only the hash is stored.
	var n int64
	e.db.Model(&Token{}).Where("hash = ?", rw).Count(&n)
	if n != 0 {
		t.Fatal("plain token stored")
	}

	for name, tok := range map[string]string{"revoked": revoked, "expired": expired, "garbage": "dsy_nope", "jwt": e.jwt()} {
		code, _ := e.rpc(tok, "ping", nil)
		if code != 401 {
			t.Fatalf("%s token: %d", name, code)
		}
	}
	_, rwList := e.rpc(rw, "tools/list", nil)
	_, roList := e.rpc(ro, "tools/list", nil)
	nrw := len(rwList["result"].(map[string]any)["tools"].([]any))
	nro := len(roList["result"].(map[string]any)["tools"].([]any))
	if nro == 0 || nro >= nrw {
		t.Fatalf("rw %d ro %d", nrw, nro)
	}
	// Read-only token refuses writes.
	_, out := e.rpc(ro, "tools/call", map[string]any{"name": "customers_create", "arguments": map[string]any{"first_name": "No", "email": "no@x.test"}})
	if out["result"].(map[string]any)["isError"] != true {
		t.Fatal("read-only token wrote")
	}
	// last_used_at is recorded.
	var tk Token
	e.db.Where("hash = ?", HashToken(rw)).First(&tk)
	if tk.LastUsedAt == nil {
		t.Fatal("last_used_at not set")
	}

	// Session id is issued on initialize and echoed.
	w := e.do("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, map[string]string{"Authorization": "Bearer " + rw})
	if w.Header().Get("Mcp-Session-Id") == "" {
		t.Fatal("no session id")
	}
	w = e.do("POST", "/mcp", `{"jsonrpc":"2.0","id":2,"method":"ping"}`, map[string]string{"Authorization": "Bearer " + rw, "Mcp-Session-Id": "abc", "MCP-Protocol-Version": "2025-06-18"})
	if w.Code != 200 || w.Header().Get("Mcp-Session-Id") != "abc" {
		t.Fatalf("echo: %d %q", w.Code, w.Header().Get("Mcp-Session-Id"))
	}
	// Parse error, notifications (202) and batches.
	if w := e.do("POST", "/mcp", `{nope`, map[string]string{"Authorization": "Bearer " + rw, "Content-Type": "application/json"}); w.Code != 400 || !strings.Contains(w.Body.String(), "-32700") {
		t.Fatalf("parse: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("POST", "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, map[string]string{"Authorization": "Bearer " + rw}); w.Code != 202 {
		t.Fatalf("notification: %d", w.Code)
	}
	w = e.do("POST", "/mcp", `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":"b","method":"tools/list"}]`, map[string]string{"Authorization": "Bearer " + rw})
	var batch []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &batch); err != nil || len(batch) != 2 {
		t.Fatalf("batch: %s", w.Body.String())
	}
	// Body size limit.
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", MaxBodyBytes) + `"}}`
	if w := e.do("POST", "/mcp", big, map[string]string{"Authorization": "Bearer " + rw}); w.Code != 413 {
		t.Fatalf("size limit: %d", w.Code)
	}
	// Rate limit per token.
	e.s.RatePerMinute = 3
	e.s.limiter = newLimiter(3)
	codes := []int{}
	for i := 0; i < 4; i++ {
		c, _ := e.rpc(ro, "ping", nil)
		codes = append(codes, c)
	}
	if codes[3] != 429 || codes[0] != 200 {
		t.Fatalf("rate limit codes %v", codes)
	}
	if c, _ := e.rpc(rw, "ping", nil); c != 200 {
		t.Fatal("rate limit should be per token")
	}
	// AI tools cannot manage tokens.
	aiJWT, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"user_id": e.userID, "org_id": e.orgID, "role": "admin", "src": "ai", "exp": time.Now().Add(time.Hour).Unix()}).SignedString([]byte(testSecret))
	if w := e.do("GET", "/api/v1/mcp/tokens", "", map[string]string{"Authorization": "Bearer " + aiJWT}); w.Code != 403 {
		t.Fatalf("ai jwt: %d", w.Code)
	}
}

func pkce() (string, string) {
	v := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	s := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(s[:])
}

func TestOAuthFlow(t *testing.T) {
	e := setup(t)
	const cb = "https://claude.ai/api/mcp/auth_callback"
	if w := e.do("POST", "/oauth/register", `{"client_name":"Bad","redirect_uris":["http://evil.example/cb"]}`, nil); w.Code != 400 {
		t.Fatalf("insecure redirect accepted: %d", w.Code)
	}
	w := e.do("POST", "/oauth/register", `{"client_name":"Claude","redirect_uris":["`+cb+`"],"token_endpoint_auth_method":"none"}`, nil)
	if w.Code != 201 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var client map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &client)
	cid := client["client_id"].(string)

	authorize := func(challenge, method string) *httptest.ResponseRecorder {
		q := url.Values{"response_type": {"code"}, "client_id": {cid}, "redirect_uri": {cb}, "code_challenge": {challenge},
			"code_challenge_method": {method}, "state": {"xyz"}, "scope": {"erp.read erp.write"}, "resource": {"http://example.com/mcp"}}
		return e.do("GET", "/oauth/authorize?"+q.Encode(), "", nil)
	}
	verifier, challenge := pkce()
	if w := authorize(challenge, "plain"); w.Code != 302 || !strings.Contains(w.Header().Get("Location"), "error=invalid_request") {
		t.Fatalf("plain PKCE: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = authorize(challenge, "S256")
	loc := w.Header().Get("Location")
	if w.Code != 302 || !strings.HasPrefix(loc, "https://app.test/connect/authorize?request=") {
		t.Fatalf("authorize: %d %s", w.Code, loc)
	}
	reqID := strings.TrimPrefix(loc, "https://app.test/connect/authorize?request=")
	auth := map[string]string{"Authorization": "Bearer " + e.jwt()}
	if w := e.do("GET", "/api/v1/oauth/requests/"+reqID, "", auth); w.Code != 200 || !strings.Contains(w.Body.String(), `"write_requested":true`) {
		t.Fatalf("request view: %d %s", w.Code, w.Body.String())
	}
	w = e.do("POST", "/api/v1/oauth/approve", `{"request":"`+reqID+`","allow":true,"write":true}`, auth)
	var ap struct {
		Data struct {
			RedirectURL string `json:"redirect_url"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &ap)
	ru, _ := url.Parse(ap.Data.RedirectURL)
	code := ru.Query().Get("code")
	if w.Code != 200 || code == "" || ru.Query().Get("state") != "xyz" || !strings.HasPrefix(ap.Data.RedirectURL, cb) {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	// The consent request can't be reused.
	if w := e.do("POST", "/api/v1/oauth/approve", `{"request":"`+reqID+`","allow":true}`, auth); w.Code != 404 {
		t.Fatalf("request reuse: %d", w.Code)
	}
	tokenReq := func(form url.Values) (int, map[string]any) {
		w := e.do("POST", "/oauth/token", form.Encode(), nil)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	exch := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {cb}, "client_id": {cid}}
	bad := url.Values{}
	for k, v := range exch {
		bad[k] = v
	}
	bad.Set("code_verifier", verifier+"x")
	if c, out := tokenReq(bad); c != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %v", c, out)
	}
	exch.Set("code_verifier", verifier)
	c, tok := tokenReq(exch)
	if c != 200 || tok["token_type"] != "Bearer" || tok["scope"] != "erp.read erp.write" || tok["expires_in"].(float64) != 3600 {
		t.Fatalf("exchange: %d %v", c, tok)
	}
	access, refresh := tok["access_token"].(string), tok["refresh_token"].(string)
	if !strings.HasPrefix(access, AccessPrefix) || !strings.HasPrefix(refresh, RefreshPrefix) {
		t.Fatal("token prefixes")
	}
	if c, _ := e.rpc(access, "ping", nil); c != 200 {
		t.Fatalf("access token rejected: %d", c)
	}
	// Code replay fails.
	if c, _ := tokenReq(exch); c != 400 {
		t.Fatalf("code replay: %d", c)
	}
	// Code replay revokes the grant (defence against stolen codes); redo the flow.
	if c, _ := e.rpc(access, "ping", nil); c != 401 {
		t.Fatalf("replay should revoke: %d", c)
	}
	verifier, challenge = pkce()
	loc = authorize(challenge, "S256").Header().Get("Location")
	reqID = strings.TrimPrefix(loc, "https://app.test/connect/authorize?request=")
	w = e.do("POST", "/api/v1/oauth/approve", `{"request":"`+reqID+`","allow":true,"write":true}`, auth)
	_ = json.Unmarshal(w.Body.Bytes(), &ap)
	ru, _ = url.Parse(ap.Data.RedirectURL)
	exch.Set("code", ru.Query().Get("code"))
	exch.Set("code_verifier", verifier)
	_, tok = tokenReq(exch)
	access, refresh = tok["access_token"].(string), tok["refresh_token"].(string)

	// Refresh rotation.
	c, tok2 := tokenReq(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {cid}})
	if c != 200 || tok2["refresh_token"] == refresh || tok2["access_token"] == access {
		t.Fatalf("refresh: %d %v", c, tok2)
	}
	if c, _ := e.rpc(tok2["access_token"].(string), "ping", nil); c != 200 {
		t.Fatal("new access token rejected")
	}
	// Reusing the old refresh token is detected and revokes the connection.
	if c, out := tokenReq(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}); c != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("refresh reuse: %d %v", c, out)
	}
	if c, _ := e.rpc(tok2["access_token"].(string), "ping", nil); c != 401 {
		t.Fatal("grant should be revoked after refresh reuse")
	}

	// Read-only approval + deny + expired access token.
	verifier, challenge = pkce()
	loc = authorize(challenge, "S256").Header().Get("Location")
	reqID = strings.TrimPrefix(loc, "https://app.test/connect/authorize?request=")
	w = e.do("POST", "/api/v1/oauth/approve", `{"request":"`+reqID+`","allow":true,"write":false}`, auth)
	_ = json.Unmarshal(w.Body.Bytes(), &ap)
	ru, _ = url.Parse(ap.Data.RedirectURL)
	exch.Set("code", ru.Query().Get("code"))
	exch.Set("code_verifier", verifier)
	_, tok = tokenReq(exch)
	if tok["scope"] != "erp.read" {
		t.Fatalf("read-only scope: %v", tok)
	}
	_, list := e.rpc(tok["access_token"].(string), "tools/list", nil)
	for _, tl := range list["result"].(map[string]any)["tools"].([]any) {
		if tl.(map[string]any)["annotations"].(map[string]any)["readOnlyHint"] != true {
			t.Fatal("read-only connection lists write tools")
		}
	}
	e.db.Model(&OAuthToken{}).Where("hash = ?", HashToken(tok["access_token"].(string))).Update("expires_at", time.Now().Add(-time.Minute))
	if c, _ := e.rpc(tok["access_token"].(string), "ping", nil); c != 401 {
		t.Fatal("expired access token accepted")
	}
	// Connected apps list + disconnect.
	w = e.do("GET", "/api/v1/mcp/connections", "", auth)
	if !strings.Contains(w.Body.String(), `"client_name":"Claude"`) {
		t.Fatalf("connections: %s", w.Body.String())
	}
	_, _ = challenge, verifier
	loc = authorize(challenge, "S256").Header().Get("Location")
	reqID = strings.TrimPrefix(loc, "https://app.test/connect/authorize?request=")
	w = e.do("POST", "/api/v1/oauth/approve", `{"request":"`+reqID+`","allow":false}`, auth)
	if !strings.Contains(w.Body.String(), "error=access_denied") {
		t.Fatalf("deny: %s", w.Body.String())
	}
	// Expired codes are cleaned up.
	e.db.Create(&OAuthCode{Hash: "old-" + uuid.NewString(), ClientID: cid, GrantID: "g", RedirectURI: cb, CodeChallenge: "c", ExpiresAt: time.Now().Add(-2 * time.Hour)})
	e.s.Cleanup()
	var old int64
	e.db.Model(&OAuthCode{}).Where("expires_at < ?", time.Now().Add(-time.Hour)).Count(&old)
	if old != 0 {
		t.Fatal("cleanup left expired codes")
	}
	if w := e.do("GET", "/oauth/authorize?client_id=nope&response_type=code", "", nil); w.Code != 400 || !strings.Contains(w.Body.String(), "Unknown application") {
		t.Fatalf("unknown client: %d", w.Code)
	}
}

func TestToolsEndToEnd(t *testing.T) {
	e := setup(t)
	tok, _ := e.pat(ScopeReadWrite, 0)

	me := e.tool(tok, "whoami", map[string]any{})
	if me["user"].(map[string]any)["email"] == "" || me["organization"].(map[string]any)["name"] != "MCP Test Org" {
		t.Fatalf("whoami %v", me)
	}

	email := "zed-" + uuid.NewString()[:6] + "@acme.test"
	cu := e.tool(tok, "customers_create", map[string]any{"first_name": "Zed", "last_name": "Zulu", "email": email, "country_code": "KE"})
	cid, _ := cu["id"].(string)
	if cid == "" || cu["billing_currency"] != "KES" || !strings.HasPrefix(cu["url"].(string), "https://app.test/customers?customer=") {
		t.Fatalf("create customer %v", cu)
	}
	found := e.tool(tok, "customers_search", map[string]any{"query": "Zulu"})
	if found["count"].(float64) != 1 {
		t.Fatalf("search %v", found)
	}
	up := e.tool(tok, "customers_update", map[string]any{"customer_id": cid, "phone": "0700000000", "address": map[string]any{"suburb": "Nairobi"}})
	if up["phone"] != "0700000000" {
		t.Fatalf("update %v", up)
	}
	got := e.tool(tok, "customers_get", map[string]any{"customer": email})
	if got["address"].(map[string]any)["suburb"] != "Nairobi" {
		t.Fatalf("get %v", got)
	}

	inv := e.tool(tok, "invoices_create", map[string]any{"customer_id": cid, "items": []any{map[string]any{"description": "Work", "quantity": 2, "unit_price": 100, "tax_rate": 10}}})
	if inv["total"].(float64) != 220 || inv["currency"] != "KES" || inv["status"] != "draft" {
		t.Fatalf("invoice %v", inv)
	}
	byNum := e.tool(tok, "invoices_get", map[string]any{"number": inv["number"]})
	if byNum["id"] != inv["id"] || len(byNum["items"].([]any)) != 1 {
		t.Fatalf("invoice get %v", byNum)
	}
	paid := e.tool(tok, "invoices_add_payment", map[string]any{"invoice_id": inv["id"], "amount": 20})
	if paid["balance_due"].(float64) != 200 {
		t.Fatalf("payment %v", paid)
	}

	bk := e.tool(tok, "bookings_create", map[string]any{"customer_id": cid, "start_time": "2030-01-15T10:00", "notes": "via mcp"})
	if bk["status"] != "scheduled" || !strings.Contains(bk["url"].(string), "/bookings?booking=") {
		t.Fatalf("booking %v", bk)
	}

	// Tool errors come back as isError with a friendly message.
	_, out := e.rpc(tok, "tools/call", map[string]any{"name": "customers_create", "arguments": map[string]any{"first_name": "Zed", "email": email}})
	res := out["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "already exists") {
		t.Fatalf("duplicate: %v", res)
	}

	// Every call is audited, with args but no secrets.
	var acts []Activity
	e.db.Where("user_id = ?", e.userID).Order("created_at").Find(&acts)
	if len(acts) < 9 {
		t.Fatalf("activity rows %d", len(acts))
	}
	last := acts[len(acts)-1]
	if last.OK || last.Tool != "customers_create" || last.Source != "token" || !strings.Contains(last.Args, email) {
		t.Fatalf("audit %+v", last)
	}
	w := e.do("GET", "/api/v1/mcp/activity?writes_only=true", "", map[string]string{"Authorization": "Bearer " + e.jwt()})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"tool":"whoami"`) || !strings.Contains(w.Body.String(), "invoices_create") {
		t.Fatalf("activity api: %s", w.Body.String())
	}
	// Connection test endpoint.
	w = e.do("POST", "/api/v1/mcp/test", "", map[string]string{"Authorization": "Bearer " + e.jwt()})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("test: %s", w.Body.String())
	}
}
