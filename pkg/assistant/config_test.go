package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// ---- helpers ---------------------------------------------------------------

var (
	superAdmin = aitools.Identity{UserID: "u-sa", OrgID: "org-1", Role: "super_admin"}
	orgAdmin   = aitools.Identity{UserID: "u-admin", OrgID: "org-1", Role: "admin"}
)

// cleanEnv removes every Anthropic variable and pins the encryption key.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ANTHROPIC_API_KEY", "ASSISTANT_MODEL", "ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL", "ENCRYPTION_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("JWT_SECRET", "unit-test-secret")
	Invalidate()
	t.Cleanup(Invalidate)
}

// liveEnv is a service that resolves its configuration on every call (the
// production shape: no pinned client), with the audit and users tables.
func liveEnv(t *testing.T) *env {
	t.Helper()
	cleanEnv(t)
	e := setup(t)
	e.svc.Client = nil
	for _, sql := range []string{
		`CREATE TABLE IF NOT EXISTS audit_logs (id text PRIMARY KEY, organization_id text, user_id text, action text, entity_type text, entity_id text, old_values text, new_values text, ip_address text, user_agent text, created_at datetime)`,
		`CREATE TABLE IF NOT EXISTS users (id text PRIMARY KEY, first_name text, last_name text, email text)`,
		`INSERT INTO users (id, first_name, last_name, email) VALUES ('u-sa', 'Kennedy', 'Admin', 'k@example.com')`,
	} {
		if err := e.db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ANTHROPIC_BASE_URL", e.api.srv.URL)
	return e
}

// adminRouter mounts both the assistant and the super-admin settings API.
func adminRouter(e *env) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	mw := func(c *gin.Context) {
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_role", c.GetHeader("X-Role"))
		c.Next()
	}
	h := &Handler{Svc: e.svc}
	h.RegisterRoutes(r.Group("/api/v1/assistant", mw))
	h.RegisterAdminRoutes(r.Group("/api/v1/super-admin/ai", mw))
	return r
}

func data(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON (%d): %s", w.Code, w.Body.String())
	}
	return out.Data
}

const (
	goodKey  = "sk-ant-api03-GOODKEYGOODKEYGOODKEY-aB3d"
	otherKey = "sk-ant-api03-OTHERKEYOTHERKEYOTHER-Zz99"
	settings = "/api/v1/super-admin/ai/settings"
)

// scripted is a fake Anthropic that answers by API key / model.
type scripted struct {
	mu   sync.Mutex
	reqs []map[string]any
	keys []string
	srv  *httptest.Server
}

func newScripted(t *testing.T) *scripted {
	s := &scripted{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		key := r.Header.Get("x-api-key")
		s.mu.Lock()
		s.reqs = append(s.reqs, body)
		s.keys = append(s.keys, key)
		s.mu.Unlock()
		fail := func(status int, typ, msg string) {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg)
		}
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			fail(404, "not_found_error", "not found")
			return
		}
		if r.Header.Get("anthropic-version") == "" {
			fail(400, "invalid_request_error", "anthropic-version header is required")
			return
		}
		model, _ := body["model"].(string)
		switch {
		case strings.Contains(key, "BAD"):
			fail(401, "authentication_error", "invalid x-api-key")
		case strings.Contains(key, "NOCREDIT"):
			fail(400, "invalid_request_error", "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits.")
		case strings.Contains(key, "FORBIDDEN"):
			fail(403, "permission_error", "Your API key does not have permission to use the specified resource.")
		case strings.Contains(key, "LIMITED"):
			fail(429, "rate_limit_error", "rate limited")
		case strings.Contains(key, "OVERLOADED"):
			fail(529, "overloaded_error", "Overloaded")
		case strings.Contains(key, "WEIRD"):
			w.WriteHeader(418)
			io.WriteString(w, "<html>teapot</html>")
		case model == "no-such-model":
			fail(404, "not_found_error", "model: no-such-model")
		default:
			w.Header().Set("content-type", "application/json")
			fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"Hi"}],"stop_reason":"max_tokens","usage":{"input_tokens":8,"output_tokens":1}}`, model)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *scripted) last() (map[string]any, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		return nil, ""
	}
	return s.reqs[len(s.reqs)-1], s.keys[len(s.keys)-1]
}

// ---- resolver ----------------------------------------------------------------

func TestResolvePrecedence(t *testing.T) {
	e := liveEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "")

	// 3. nothing anywhere.
	cfg := Resolve(e.db)
	if cfg.Enabled() || cfg.Source != SourceNone || cfg.Model != DefaultModel || cfg.ModelSource != SourceDefault || cfg.BaseURL != DefaultBaseURL {
		t.Fatalf("empty = %+v", cfg)
	}
	if nilCfg := Resolve(nil); nilCfg.Source != SourceNone || nilCfg.Model != DefaultModel {
		t.Fatalf("nil db = %+v", nilCfg)
	}

	// 2. environment (read on every call, never cached).
	t.Setenv("ANTHROPIC_API_KEY", "  sk-ant-env-key  ")
	t.Setenv("ANTHROPIC_MODEL", "env-anthropic-model")
	cfg = Resolve(e.db)
	if cfg.APIKey != "sk-ant-env-key" || cfg.Source != SourceEnvironment || cfg.Model != "env-anthropic-model" || cfg.ModelSource != SourceEnvironment {
		t.Fatalf("env = %+v", cfg)
	}
	t.Setenv("ASSISTANT_MODEL", "env-assistant-model") // wins over ANTHROPIC_MODEL
	if cfg = Resolve(e.db); cfg.Model != "env-assistant-model" {
		t.Fatalf("ASSISTANT_MODEL should win: %+v", cfg)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://gateway.test/")
	if cfg = Resolve(e.db); cfg.BaseURL != "http://gateway.test" {
		t.Fatalf("base url = %q", cfg.BaseURL)
	}

	// 1. saved in the app beats the environment — key and model independently.
	enc, err := mail.EncryptSecret(goodKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := savePlatformSettings(e.db, func(r *PlatformSettings) { r.APIKey = enc }); err != nil {
		t.Fatal(err)
	}
	// Cached: the old answer is served until the cache is invalidated.
	if cfg = Resolve(e.db); cfg.Source != SourceEnvironment {
		t.Fatalf("expected the cached environment config, got %+v", cfg)
	}
	Invalidate()
	cfg = Resolve(e.db)
	if cfg.APIKey != goodKey || cfg.Source != SourceSettings || cfg.Model != "env-assistant-model" || cfg.ModelSource != SourceEnvironment {
		t.Fatalf("saved key = %+v", cfg)
	}
	if cfg.BaseURL != "http://gateway.test" {
		t.Fatalf("the base URL only comes from the environment: %+v", cfg)
	}
	if err := savePlatformSettings(e.db, func(r *PlatformSettings) { r.Model = "claude-haiku-4-5" }); err != nil {
		t.Fatal(err)
	}
	Invalidate()
	if cfg = Resolve(e.db); cfg.Model != "claude-haiku-4-5" || cfg.ModelSource != SourceSettings || cfg.APIKey != goodKey {
		t.Fatalf("saved model = %+v", cfg)
	}

	// A saved model alone leaves the key with the environment.
	if err := savePlatformSettings(e.db, func(r *PlatformSettings) { r.APIKey = "" }); err != nil {
		t.Fatal(err)
	}
	Invalidate()
	if cfg = Resolve(e.db); cfg.APIKey != "sk-ant-env-key" || cfg.Source != SourceEnvironment || cfg.Model != "claude-haiku-4-5" {
		t.Fatalf("after removing the saved key = %+v", cfg)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if cfg = Resolve(e.db); cfg.Enabled() || cfg.Source != SourceNone {
		t.Fatalf("nothing left = %+v", cfg)
	}

	// Current() follows the database given to Use.
	Use(e.db)
	t.Cleanup(func() { Use(nil) })
	if err := savePlatformSettings(e.db, func(r *PlatformSettings) { r.APIKey = enc }); err != nil {
		t.Fatal(err)
	}
	Invalidate()
	if cur := Current(); cur.APIKey != goodKey || cur.Source != SourceSettings {
		t.Fatalf("Current = %+v", cur)
	}
	Use(nil)
	if cur := Current(); cur.Enabled() {
		t.Fatalf("Current without a database must only see the environment: %+v", cur)
	}
}

func TestResolveCacheExpires(t *testing.T) {
	e := liveEnv(t)
	old := CacheTTL
	CacheTTL = 0 // every call re-reads
	t.Cleanup(func() { CacheTTL = old })
	if Resolve(e.db).Enabled() {
		t.Fatal("should start empty")
	}
	enc, _ := mail.EncryptSecret(goodKey)
	if err := savePlatformSettings(e.db, func(r *PlatformSettings) { r.APIKey = enc }); err != nil {
		t.Fatal(err)
	}
	// No Invalidate: another instance saved it; the TTL alone picks it up.
	if cfg := Resolve(e.db); cfg.APIKey != goodKey {
		t.Fatalf("expired cache not refreshed: %+v", cfg)
	}
}

// ---- storage -------------------------------------------------------------------

func TestKeyIsEncryptedAtRest(t *testing.T) {
	e := liveEnv(t)
	r := adminRouter(e)
	if w := do(r, superAdmin, "PUT", settings, fmt.Sprintf(`{"api_key":%q}`, goodKey)); w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	var stored, hint string
	e.db.Raw("SELECT api_key, key_hint FROM assistant_platform_settings WHERE id = 'platform'").Row().Scan(&stored, &hint)
	if stored == "" || strings.Contains(stored, goodKey) || strings.Contains(stored, "GOODKEY") {
		t.Fatalf("the key is stored in plain text: %q", stored)
	}
	if !mail.IsEncrypted(stored) {
		t.Fatalf("not encrypted with the mail helper: %q", stored)
	}
	if plain, err := mail.DecryptSecret(stored); err != nil || plain != goodKey {
		t.Fatalf("decrypt = %q, %v", plain, err)
	}
	if hint != "sk-ant-…aB3d" {
		t.Fatalf("hint = %q", hint)
	}
	var rows int64
	e.db.Raw("SELECT COUNT(*) FROM assistant_platform_settings").Scan(&rows)
	if rows != 1 {
		t.Fatalf("expected a single platform row, got %d", rows)
	}
	// Saving again replaces the same row.
	if w := do(r, superAdmin, "PUT", settings, fmt.Sprintf(`{"api_key":%q}`, otherKey)); w.Code != 200 {
		t.Fatalf("replace = %d %s", w.Code, w.Body.String())
	}
	e.db.Raw("SELECT COUNT(*) FROM assistant_platform_settings").Scan(&rows)
	if rows != 1 || Resolve(e.db).APIKey != otherKey {
		t.Fatalf("rows=%d cfg=%+v", rows, Resolve(e.db))
	}

	// A different server encryption key: the saved key is unusable, the
	// environment takes over and the problem is reported (no panic, no garbage key).
	t.Setenv("JWT_SECRET", "a-different-secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env-key")
	Invalidate()
	cfg := Resolve(e.db)
	if cfg.APIKey != "sk-ant-env-key" || cfg.Source != SourceEnvironment || !strings.Contains(cfg.Problem, "can't be read") {
		t.Fatalf("undecryptable key = %+v", cfg)
	}
}

func TestMaskKey(t *testing.T) {
	for in, want := range map[string]string{
		"":                        "",
		"short":                   "…rt",
		goodKey:                   "sk-ant-…aB3d",
		"gw_0123456789abcdefWXYZ": "gw_…WXYZ",
	} {
		if got := MaskKey(in); got != want {
			t.Errorf("MaskKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- settings API ----------------------------------------------------------------

func TestAdminSettingsSuperAdminOnly(t *testing.T) {
	e := liveEnv(t)
	r := adminRouter(e)
	for _, who := range []aitools.Identity{orgAdmin, alice} {
		for _, rq := range [][2]string{{"GET", settings}, {"PUT", settings}, {"DELETE", settings}, {"POST", settings + "/test"}, {"GET", settings + "/models"}} {
			w := do(r, who, rq[0], rq[1], fmt.Sprintf(`{"api_key":%q}`, goodKey))
			if w.Code != 403 || !strings.Contains(w.Body.String(), "SUPER_ADMIN_REQUIRED") {
				t.Fatalf("%s %s as %s = %d %s", rq[0], rq[1], who.Role, w.Code, w.Body.String())
			}
		}
	}
	if Resolve(e.db).Enabled() {
		t.Fatal("a non-super-admin saved a key")
	}
	if len(e.api.requests()) != 0 {
		t.Fatal("a non-super-admin triggered a call to Anthropic")
	}
}

func TestAdminSettingsAreMaskedAndAudited(t *testing.T) {
	e := liveEnv(t)
	r := adminRouter(e)

	d := data(t, do(r, superAdmin, "GET", settings, ""))
	if d["enabled"] != false || d["source"] != "none" || d["source_label"] != "Not set" || d["key_set"] != false || d["model"] != DefaultModel || d["default_model"] != DefaultModel {
		t.Fatalf("empty settings = %v", d)
	}
	if len(d["model_suggestions"].([]any)) < 3 || d["console_url"] != ConsoleKeysURL {
		t.Fatalf("suggestions/console = %v", d)
	}

	w := do(r, superAdmin, "PUT", settings, fmt.Sprintf(`{"api_key":"  %s \n","model":"claude-haiku-4-5"}`, goodKey))
	if w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), goodKey) || strings.Contains(w.Body.String(), "GOODKEY") || strings.Contains(w.Body.String(), "enc:v1:") {
		t.Fatalf("PUT response leaks the key: %s", w.Body.String())
	}
	d = data(t, w)
	if d["enabled"] != true || d["source"] != "settings" || d["source_label"] != "Saved in app" || d["key_set"] != true || d["key_hint"] != "sk-ant-…aB3d" ||
		d["model"] != "claude-haiku-4-5" || d["model_source"] != "settings" || d["key_updated_by"] != "Kennedy Admin" {
		t.Fatalf("saved settings = %v", d)
	}
	// Saving verified the key with a one-token call, using the new model.
	if test, _ := d["test"].(map[string]any); test == nil || test["ok"] != true || test["code"] != "ok" {
		t.Fatalf("save should report the verification: %v", d["test"])
	}
	if lt, _ := d["last_test"].(map[string]any); lt == nil || lt["ok"] != true || lt["model"] != "claude-haiku-4-5" {
		t.Fatalf("last_test = %v", d["last_test"])
	}
	if e.api.lastKey() != goodKey {
		t.Fatalf("verification used key %q", e.api.lastKey())
	}

	w = do(r, superAdmin, "GET", settings, "")
	if strings.Contains(w.Body.String(), goodKey) || strings.Contains(w.Body.String(), "GOODKEY") || strings.Contains(w.Body.String(), "enc:v1:") {
		t.Fatalf("GET leaks the key: %s", w.Body.String())
	}
	for _, f := range []string{"api_key", "APIKey"} {
		if strings.Contains(w.Body.String(), `"`+f+`"`) {
			t.Fatalf("GET exposes field %s: %s", f, w.Body.String())
		}
	}
	// The assistant status never carries it either.
	if s := do(r, superAdmin, "GET", "/api/v1/assistant/status", "").Body.String(); strings.Contains(s, "GOODKEY") || !strings.Contains(s, `"source":"settings"`) {
		t.Fatalf("status = %s", s)
	}

	// Model only: the key stays.
	d = data(t, do(r, superAdmin, "PUT", settings, `{"model":"claude-opus-4-5"}`))
	if d["key_set"] != true || d["model"] != "claude-opus-4-5" || Resolve(e.db).APIKey != goodKey {
		t.Fatalf("model-only save = %v", d)
	}
	// Empty key = keep; empty model = back to the default.
	d = data(t, do(r, superAdmin, "PUT", settings, `{"api_key":"","model":""}`))
	if d["key_set"] != true || d["model"] != DefaultModel || d["model_source"] != "default" || d["saved_model"] != "" {
		t.Fatalf("reset model = %v", d)
	}

	// Validation.
	for body, want := range map[string]string{
		`{}`: "Nothing to save",
		`{"api_key":"sk-ant-has space-0123456789"}`:    "spaces",
		`{"api_key":"dsy_0123456789abcdef0123456789"}`: "DASYIN access token",
		`{"model":"bad model!"}`:                       "model name",
	} {
		w := do(r, superAdmin, "PUT", settings, body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("PUT %s = %d %s", body, w.Code, w.Body.String())
		}
	}

	// Remove: back to "not set" (the saved model would be kept).
	w = do(r, superAdmin, "DELETE", settings, "")
	d = data(t, w)
	if w.Code != 200 || d["enabled"] != false || d["key_set"] != false || d["source"] != "none" || d["key_hint"] != "" || d["last_test"] != nil {
		t.Fatalf("after delete = %d %v", w.Code, d)
	}
	var stored string
	e.db.Raw("SELECT api_key FROM assistant_platform_settings WHERE id = 'platform'").Row().Scan(&stored)
	if stored != "" {
		t.Fatalf("key still stored after remove: %q", stored)
	}

	// Audit: every change is logged, the key never is.
	type auditRow struct{ Action, EntityType, UserID, NewValues string }
	var logs []auditRow
	e.db.Raw("SELECT action, entity_type, user_id, new_values FROM audit_logs ORDER BY created_at").Scan(&logs)
	seen := map[string]int{}
	for _, l := range logs {
		seen[l.Action]++
		if strings.Contains(l.NewValues, "GOODKEY") || strings.Contains(l.NewValues, "enc:v1:") || l.EntityType != "ai_settings" || l.UserID != "u-sa" {
			t.Fatalf("bad audit row %+v", l)
		}
	}
	if seen["ai_settings.update"] != 3 || seen["ai_settings.remove_key"] != 1 {
		t.Fatalf("audit actions = %v", seen)
	}
	if !strings.Contains(logs[0].NewValues, "sk-ant-…aB3d") || !strings.Contains(logs[0].NewValues, "replaced") {
		t.Fatalf("first audit row = %+v", logs[0])
	}
}

func TestStrictKeyFormatAgainstRealAnthropic(t *testing.T) {
	// With the real endpoint (no ANTHROPIC_BASE_URL) obviously wrong keys are
	// refused before anything is sent anywhere.
	for raw, want := range map[string]string{
		"my-password-123456789012345":  "start with sk-ant-",
		"sk-ant-short":                 "incomplete",
		"dsy_abcdefabcdefabcdefabcdef": "DASYIN access token",
		"":                             "Paste the API key",
	} {
		if _, err := cleanKey(raw, true); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("cleanKey(%q) = %v, want %q", raw, err, want)
		}
	}
	if k, err := cleanKey(` "`+goodKey+`" `, true); err != nil || k != goodKey {
		t.Fatalf("quoted key = %q, %v", k, err)
	}
	// A gateway (custom base URL) may use other key formats.
	if k, err := cleanKey("gateway-key-1", false); err != nil || k != "gateway-key-1" {
		t.Fatalf("gateway key = %q, %v", k, err)
	}
}

// ---- test key ----------------------------------------------------------------------

func TestKeyErrorMapping(t *testing.T) {
	fake := newScripted(t)
	cases := []struct {
		name, key, model, code string
		ok                     bool
		msg                    string
	}{
		{"works", goodKey, "claude-sonnet-4-5", TestOK, true, "The key works"},
		{"invalid key", "sk-ant-BAD", "claude-sonnet-4-5", TestInvalidKey, false, "rejected this API key"},
		{"no credit", "sk-ant-NOCREDIT", "claude-sonnet-4-5", TestNoCredit, false, "no credit"},
		{"model not found", goodKey, "no-such-model", TestModelNotFound, false, "“no-such-model” was not found"},
		{"permission", "sk-ant-FORBIDDEN", "claude-sonnet-4-5", TestPermission, false, "not allowed"},
		{"rate limited", "sk-ant-LIMITED", "claude-sonnet-4-5", TestRateLimited, false, "rate limiting"},
		{"overloaded", "sk-ant-OVERLOADED", "claude-sonnet-4-5", TestUnavailable, false, "temporarily unavailable"},
		{"unexpected", "sk-ant-WEIRD", "claude-sonnet-4-5", TestError, false, "HTTP 418"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := CheckKey(context.Background(), Config{APIKey: tc.key, Model: tc.model, BaseURL: fake.srv.URL}, nil)
			if res.OK != tc.ok || res.Code != tc.code || !strings.Contains(res.Message, tc.msg) || res.Model != tc.model {
				t.Fatalf("result = %+v", res)
			}
			if strings.Contains(res.Message, tc.key) {
				t.Fatalf("the message repeats the key: %s", res.Message)
			}
			body, key := fake.last()
			if key != tc.key || body["max_tokens"] != float64(1) || body["model"] != tc.model || body["stream"] != nil {
				t.Fatalf("request = %v (key %q)", body, key)
			}
		})
	}
	// "No credit" must point at billing and explain the subscription mix-up.
	nc := CheckKey(context.Background(), Config{APIKey: "sk-ant-NOCREDIT", Model: "m", BaseURL: fake.srv.URL}, nil)
	if !strings.Contains(nc.Message, "Billing") || !strings.Contains(nc.Message, "billed separately from a Claude subscription") {
		t.Fatalf("no-credit message = %s", nc.Message)
	}

	t.Run("no key", func(t *testing.T) {
		before := len(fake.reqs)
		if res := CheckKey(context.Background(), Config{Model: "m", BaseURL: fake.srv.URL}, nil); res.OK || res.Code != TestNoKey {
			t.Fatalf("result = %+v", res)
		}
		if len(fake.reqs) != before {
			t.Fatal("a request was sent without a key")
		}
	})
	t.Run("network", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		// No proxy: the sandbox's HTTP proxy would answer for unknown hosts.
		direct := &http.Client{Transport: &http.Transport{Proxy: nil}}
		res := CheckKey(context.Background(), Config{APIKey: goodKey, Model: "m", BaseURL: url}, direct)
		if res.OK || res.Code != TestNetwork || !strings.Contains(res.Message, "refused") {
			t.Fatalf("result = %+v", res)
		}
		res = CheckKey(context.Background(), Config{APIKey: goodKey, Model: "m", BaseURL: "http://no-such-host.invalid"}, direct)
		if res.OK || res.Code != TestNetwork || !strings.Contains(res.Message, "no-such-host.invalid") {
			t.Fatalf("dns result = %+v", res)
		}
	})
}

func TestAdminTestEndpointAndRejectedKeys(t *testing.T) {
	e := liveEnv(t)
	fake := newScripted(t)
	t.Setenv("ANTHROPIC_BASE_URL", fake.srv.URL)
	r := adminRouter(e)

	// Nothing configured: a clear "no key" answer, nothing sent.
	d := data(t, do(r, superAdmin, "POST", settings+"/test", ""))
	if d["ok"] != false || d["code"] != "no_key" {
		t.Fatalf("test without a key = %v", d)
	}

	// Test a pasted key before saving it: nothing is stored.
	d = data(t, do(r, superAdmin, "POST", settings+"/test", fmt.Sprintf(`{"api_key":%q}`, goodKey)))
	if d["ok"] != true || d["code"] != "ok" || d["source"] != "pasted" {
		t.Fatalf("candidate test = %v", d)
	}
	if Resolve(e.db).Enabled() {
		t.Fatal("testing a pasted key must not save it")
	}
	d = data(t, do(r, superAdmin, "POST", settings+"/test", `{"api_key":"sk-ant-BAD-0123456789abcdef"}`))
	if d["ok"] != false || d["code"] != "invalid_key" {
		t.Fatalf("bad candidate = %v", d)
	}

	// Saving a key Anthropic rejects: refused, nothing stored.
	w := do(r, superAdmin, "PUT", settings, `{"api_key":"sk-ant-BAD-0123456789abcdef"}`)
	if w.Code != 422 || !strings.Contains(w.Body.String(), "INVALID_KEY") || !strings.Contains(w.Body.String(), "rejected this API key") {
		t.Fatalf("rejected save = %d %s", w.Code, w.Body.String())
	}
	if Resolve(e.db).Enabled() {
		t.Fatal("a rejected key was saved")
	}

	// A valid key on an account without credit is saved, with the warning.
	w = do(r, superAdmin, "PUT", settings, `{"api_key":"sk-ant-NOCREDIT-0123456789abcdef"}`)
	d = data(t, w)
	if w.Code != 200 || d["key_set"] != true {
		t.Fatalf("no-credit save = %d %v", w.Code, d)
	}
	if test := d["test"].(map[string]any); test["ok"] != false || test["code"] != "no_credit" {
		t.Fatalf("no-credit warning = %v", test)
	}

	// verify:false skips the call (e.g. the server is offline right now).
	before := len(fake.reqs)
	w = do(r, superAdmin, "PUT", settings, `{"api_key":"sk-ant-BAD-0123456789abcdef","verify":false}`)
	if w.Code != 200 || len(fake.reqs) != before || data(t, w)["last_test"] != nil {
		t.Fatalf("unverified save = %d %s", w.Code, w.Body.String())
	}

	// Testing the key in use remembers the result; a wrong model is its own error.
	if w := do(r, superAdmin, "PUT", settings, fmt.Sprintf(`{"api_key":%q}`, goodKey)); w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	d = data(t, do(r, superAdmin, "POST", settings+"/test", `{"model":"no-such-model"}`))
	if d["ok"] != false || d["code"] != "model_not_found" || d["source"] != "settings" {
		t.Fatalf("model test = %v", d)
	}
	d = data(t, do(r, superAdmin, "POST", settings+"/test", ""))
	if d["ok"] != true || d["source"] != "settings" {
		t.Fatalf("saved-key test = %v", d)
	}
	if _, key := fake.last(); key != goodKey {
		t.Fatalf("tested with %q", key)
	}
	lt := data(t, do(r, superAdmin, "GET", settings, ""))["last_test"].(map[string]any)
	if lt["ok"] != true || lt["code"] != "ok" {
		t.Fatalf("last_test = %v", lt)
	}

	// The environment key is testable too, and reported as such.
	do(r, superAdmin, "DELETE", settings, "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env-OVERLOADED")
	d = data(t, do(r, superAdmin, "POST", settings+"/test", ""))
	if d["ok"] != false || d["code"] != "unavailable" || d["source"] != "environment" {
		t.Fatalf("env-key test = %v", d)
	}
	d = data(t, do(r, superAdmin, "GET", settings, ""))
	if d["source"] != "environment" || d["source_label"] != "Environment variable" || d["env_key_set"] != true || d["key_set"] != false || d["env_key_hint"] != "sk-ant-…ADED" {
		t.Fatalf("env settings = %v", d)
	}
}

// ---- no restart ----------------------------------------------------------------------

func TestSavedKeyWorksImmediatelyWithoutRestart(t *testing.T) {
	e := liveEnv(t) // ANTHROPIC_BASE_URL → the streaming fake; no key anywhere
	r := adminRouter(e)

	st := data(t, do(r, alice, "GET", "/api/v1/assistant/status", ""))
	if st["enabled"] != false || st["source"] != "none" {
		t.Fatalf("status before = %v", st)
	}
	if w := do(r, alice, "POST", "/api/v1/assistant/chat", `{"message":"hi"}`); w.Code != 503 || !strings.Contains(w.Body.String(), "ASSISTANT_DISABLED") {
		t.Fatalf("chat before = %d %s", w.Code, w.Body.String())
	}

	// A super admin saves a key in the app. Same process, same Service.
	if w := do(r, superAdmin, "PUT", settings, fmt.Sprintf(`{"api_key":%q,"model":"claude-haiku-4-5"}`, goodKey)); w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	st = data(t, do(r, alice, "GET", "/api/v1/assistant/status", ""))
	if st["enabled"] != true || st["source"] != "settings" || st["model"] != "claude-haiku-4-5" {
		t.Fatalf("status after save = %v", st)
	}
	e.api.push(fakeReply{text: "Hello from the saved key"})
	w := do(r, alice, "POST", "/api/v1/assistant/chat", `{"message":"hi"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "saved") {
		t.Fatalf("chat after save = %d %s", w.Code, w.Body.String())
	}
	reqs := e.api.requests()
	if e.api.lastKey() != goodKey || reqs[len(reqs)-1].Model != "claude-haiku-4-5" || !reqs[len(reqs)-1].Stream {
		t.Fatalf("chat used key %q model %q", e.api.lastKey(), reqs[len(reqs)-1].Model)
	}
	var model string
	e.db.Raw("SELECT model FROM assistant_messages WHERE role = 'assistant' ORDER BY created_at DESC LIMIT 1").Scan(&model)
	if model != "claude-haiku-4-5" {
		t.Fatalf("stored message model = %q", model)
	}

	// Replace the key: the very next message uses the new one.
	if w := do(r, superAdmin, "PUT", settings, fmt.Sprintf(`{"api_key":%q}`, otherKey)); w.Code != 200 {
		t.Fatalf("replace = %d %s", w.Code, w.Body.String())
	}
	e.api.push(fakeReply{text: "again"})
	if w := do(r, alice, "POST", "/api/v1/assistant/chat", `{"message":"hi again"}`); w.Code != 200 || e.api.lastKey() != otherKey {
		t.Fatalf("chat after replace = %d, key %q", w.Code, e.api.lastKey())
	}

	// Remove it: off again, at once.
	if w := do(r, superAdmin, "DELETE", settings, ""); w.Code != 200 {
		t.Fatalf("delete = %d", w.Code)
	}
	if st = data(t, do(r, alice, "GET", "/api/v1/assistant/status", "")); st["enabled"] != false {
		t.Fatalf("status after remove = %v", st)
	}
	if w := do(r, alice, "POST", "/api/v1/assistant/chat", `{"message":"hi"}`); w.Code != 503 {
		t.Fatalf("chat after remove = %d", w.Code)
	}
}

func TestChatErrorsAreFriendly(t *testing.T) {
	for _, tc := range []struct {
		err  APIError
		want string
	}{
		{APIError{Status: 401, Type: "authentication_error"}, "System settings → AI assistant"},
		{APIError{Status: 400, Type: "invalid_request_error", Message: "Your credit balance is too low to access the Anthropic API."}, "no credit left"},
		{APIError{Status: 404, Type: "not_found_error", Message: "model: x"}, "model"},
	} {
		if got := tc.err.Friendly(); !strings.Contains(got, tc.want) || strings.Contains(got, "ANTHROPIC_API_KEY") {
			t.Errorf("Friendly(%d) = %q", tc.err.Status, got)
		}
	}
}
