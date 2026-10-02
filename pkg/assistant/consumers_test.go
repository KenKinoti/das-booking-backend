package assistant_test

// Every AI feature outside the chat (meeting summaries, expense extraction,
// market "Explain with AI") must use the key saved in the app — with no
// ANTHROPIC_API_KEY in the environment and no restart. This lives in an
// external test package because those packages import pkg/assistant.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/assistant"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/insights"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mcp"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/meetings"
)

const savedKey = "sk-ant-api03-SAVEDINAPPSAVEDINAPP-q7Rt"

// anthropicStub answers non-streaming Messages calls with a canned text per
// feature and records the key and model each caller used.
type anthropicStub struct {
	mu    sync.Mutex
	calls []stubCall
	srv   *httptest.Server
}

type stubCall struct{ Key, Model, Prompt string }

func newStub(t *testing.T) *anthropicStub {
	s := &anthropicStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		json.Unmarshal(raw, &body)
		prompt := string(raw)
		s.mu.Lock()
		s.calls = append(s.calls, stubCall{Key: r.Header.Get("x-api-key"), Model: body.Model, Prompt: prompt})
		s.mu.Unlock()
		text := "ok"
		switch {
		case strings.Contains(prompt, "meeting transcript"):
			text = `{"summary":"AI summary of the meeting.","key_decisions":["Ship on Friday"],"action_items":[{"text":"Send the quote","owner":"Ann","due":"Friday"}]}`
		case strings.Contains(prompt, "Extract the expense"):
			text = `{"vendor":"Acme Hosting","invoice_number":"INV-77","invoice_date":"2026-09-01","currency":"usd","total":120.5,"tax":0,"service":"Hosting","category":"hosting","paid":true,"is_expense":true,"confidence":0.9}`
		case strings.Contains(prompt, "World Bank indicators"):
			text = "- Inflation is easing, so price rises can be smaller."
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"type": "message", "role": "assistant", "model": body.Model,
			"content": []map[string]string{{"type": "text", "text": text}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 10, "output_tokens": 10}})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *anthropicStub) last(t *testing.T) stubCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("no call reached Anthropic")
	}
	return s.calls[len(s.calls)-1]
}

func (s *anthropicStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type world struct {
	db   *gorm.DB
	stub *anthropicStub
	r    *gin.Engine
}

// newWorld is a "running server": database given to assistant.Use, no
// Anthropic key in the environment, ANTHROPIC_BASE_URL pointing at the stub.
func newWorld(t *testing.T) *world {
	t.Helper()
	for _, k := range []string{"ANTHROPIC_API_KEY", "ASSISTANT_MODEL", "ANTHROPIC_MODEL", "ENCRYPTION_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("JWT_SECRET", "unit-test-secret")
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "c.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := assistant.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := mcp.Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE TABLE audit_logs (id text PRIMARY KEY, organization_id text, user_id text, action text, entity_type text, entity_id text, old_values text, new_values text, ip_address text, user_agent text, created_at datetime)`,
		`CREATE TABLE users (id text PRIMARY KEY, first_name text, last_name text, email text)`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	stub := newStub(t)
	t.Setenv("ANTHROPIC_BASE_URL", stub.srv.URL)
	assistant.Invalidate()
	assistant.Use(db)
	t.Cleanup(func() { assistant.Use(nil); assistant.Invalidate() })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	mw := func(c *gin.Context) {
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_role", c.GetHeader("X-Role"))
		c.Next()
	}
	h := &assistant.Handler{Svc: assistant.NewService(db)}
	h.RegisterRoutes(r.Group("/api/v1/assistant", mw))
	h.RegisterAdminRoutes(r.Group("/api/v1/super-admin/ai", mw))
	return &world{db: db, stub: stub, r: r}
}

func (w *world) call(method, path, role, user, org, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Role", role)
	req.Header.Set("X-User", user)
	req.Header.Set("X-Org", org)
	rec := httptest.NewRecorder()
	w.r.ServeHTTP(rec, req)
	return rec
}

func (w *world) saveKey(t *testing.T, key, model string) {
	t.Helper()
	rec := w.call("PUT", "/api/v1/super-admin/ai/settings", "super_admin", "u-sa", "org-1", fmt.Sprintf(`{"api_key":%q,"model":%q}`, key, model))
	if rec.Code != 200 {
		t.Fatalf("saving the key failed: %d %s", rec.Code, rec.Body.String())
	}
}

const transcript = "Ann: We agreed to ship the new website on Friday.\nBen: I will send the quote to the client by Thursday.\nAnn: Great, the budget is approved and we decided to go with the hosting upgrade."

func TestConsumersUseTheSavedKeyWithoutEnv(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// --- nothing configured: every feature is off / falls back, nothing is sent.
	if expenses.AIEnabled() || insights.AIEnabled() {
		t.Fatal("AI reported as enabled without any key")
	}
	if sum := meetings.Summarize(ctx, "Weekly", transcript); sum.Source != "local" {
		t.Fatalf("summary without a key should be local, got %q", sum.Source)
	}
	if _, _, err := expenses.AIExtract(ctx, "Invoice INV-77 total USD 120.50", nil, ""); err == nil {
		t.Fatal("AIExtract should refuse without a key")
	}
	if _, err := (&insights.Service{}).Explain(ctx, &insights.Market{Home: "KES"}, ""); err == nil {
		t.Fatal("Explain should refuse without a key")
	}
	if n := w.stub.count(); n != 0 {
		t.Fatalf("%d calls reached Anthropic without a key", n)
	}

	// --- a super admin saves the key in the app (verified with one call).
	w.saveKey(t, savedKey, "claude-haiku-4-5")
	if c := w.stub.last(t); c.Key != savedKey || c.Model != "claude-haiku-4-5" {
		t.Fatalf("verification call = %+v", c)
	}

	// meetings
	sum := meetings.Summarize(ctx, "Weekly", transcript)
	if sum.Source != "anthropic" || sum.Summary != "AI summary of the meeting." || len(sum.ActionItems) != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if c := w.stub.last(t); c.Key != savedKey || c.Model != "claude-haiku-4-5" || !strings.Contains(c.Prompt, "meeting transcript") {
		t.Fatalf("meetings call = key %q model %q", c.Key, c.Model)
	}

	// expenses
	if !expenses.AIEnabled() {
		t.Fatal("expenses.AIEnabled() is false with a saved key")
	}
	ex, meta, err := expenses.AIExtract(ctx, "Invoice INV-77 total USD 120.50", nil, "")
	if err != nil || ex == nil || ex.Method != "ai" || ex.InvoiceNumber != "INV-77" || ex.Total != 120.5 || !strings.HasPrefix(meta, "hosting|") {
		t.Fatalf("AIExtract = %+v, %q, %v", ex, meta, err)
	}
	if c := w.stub.last(t); c.Key != savedKey || c.Model != "claude-haiku-4-5" || !strings.Contains(c.Prompt, "Extract the expense") {
		t.Fatalf("expenses call = key %q model %q", c.Key, c.Model)
	}

	// insights
	if !insights.AIEnabled() {
		t.Fatal("insights.AIEnabled() is false with a saved key")
	}
	txt, err := (&insights.Service{}).Explain(ctx, &insights.Market{Home: "KES"}, "")
	if err != nil || !strings.Contains(txt, "Inflation is easing") {
		t.Fatalf("Explain = %q, %v", txt, err)
	}
	if c := w.stub.last(t); c.Key != savedKey || c.Model != "claude-haiku-4-5" || !strings.Contains(c.Prompt, "World Bank indicators") {
		t.Fatalf("insights call = key %q model %q", c.Key, c.Model)
	}

	// --- the saved key beats an environment key …
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env-key-0123456789")
	meetings.Summarize(ctx, "Weekly", transcript)
	if c := w.stub.last(t); c.Key != savedKey {
		t.Fatalf("the saved key should win over the environment, got %q", c.Key)
	}
	// … and removing it hands over to the environment at once.
	if rec := w.call("DELETE", "/api/v1/super-admin/ai/settings", "super_admin", "u-sa", "org-1", ""); rec.Code != 200 {
		t.Fatalf("delete = %d", rec.Code)
	}
	meetings.Summarize(ctx, "Weekly", transcript)
	if c := w.stub.last(t); c.Key != "sk-ant-env-key-0123456789" {
		t.Fatalf("after removing the saved key the environment key should be used, got %q", c.Key)
	}

	// --- no key anywhere again: off, immediately.
	t.Setenv("ANTHROPIC_API_KEY", "")
	before := w.stub.count()
	if expenses.AIEnabled() || insights.AIEnabled() {
		t.Fatal("AI still enabled after the key was removed")
	}
	if sum := meetings.Summarize(ctx, "Weekly", transcript); sum.Source != "local" {
		t.Fatalf("summary after removal = %q", sum.Source)
	}
	if w.stub.count() != before {
		t.Fatal("a call was made after the key was removed")
	}
}

// The assistant status tells people that Claude ↔ DASYIN (MCP) is a separate,
// already-working connection.
func TestStatusReportsMCPConnection(t *testing.T) {
	w := newWorld(t)
	status := func(user, org, role string) map[string]any {
		rec := w.call("GET", "/api/v1/assistant/status", role, user, org, "")
		var out struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
		return out.Data
	}
	mcpOf := func(d map[string]any) map[string]any { return d["mcp"].(map[string]any) }

	d := status("u-1", "org-1", "admin")
	if d["enabled"] != false || mcpOf(d)["connected"] != false || mcpOf(d)["mine"] != false {
		t.Fatalf("fresh status = %v", d)
	}

	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(24*time.Hour)
	used := now.Add(-5 * time.Minute)
	rows := []mcp.Token{
		{ID: "t-revoked", OrgID: "org-1", UserID: "u-1", Name: "old", Prefix: "dsy_aaaaaa", Hash: "h1", Scope: "read", CreatedAt: now, RevokedAt: &past},
		{ID: "t-expired", OrgID: "org-1", UserID: "u-1", Name: "expired", Prefix: "dsy_bbbbbb", Hash: "h2", Scope: "read", CreatedAt: now, ExpiresAt: &past},
		{ID: "t-other-org", OrgID: "org-2", UserID: "u-9", Name: "other org", Prefix: "dsy_cccccc", Hash: "h3", Scope: "read", CreatedAt: now},
	}
	for i := range rows {
		if err := w.db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Revoked, expired and other organisations' tokens do not count.
	if m := mcpOf(status("u-1", "org-1", "admin")); m["connected"] != false {
		t.Fatalf("inactive tokens counted: %v", m)
	}

	// An active token of a colleague: the workspace is connected, but not "mine".
	if err := w.db.Create(&mcp.Token{ID: "t-live", OrgID: "org-1", UserID: "u-2", Name: "Claude Code", Prefix: "dsy_dddddd", Hash: "h4", Scope: "read", CreatedAt: now, ExpiresAt: &future, LastUsedAt: &used}).Error; err != nil {
		t.Fatal(err)
	}
	m := mcpOf(status("u-1", "org-1", "admin"))
	if m["connected"] != true || m["mine"] != false || m["tokens"] != float64(1) || m["apps"] != float64(0) || m["last_used_at"] == nil {
		t.Fatalf("colleague's token: %v", m)
	}
	if m := mcpOf(status("u-2", "org-1", "admin")); m["mine"] != true {
		t.Fatalf("own token: %v", m)
	}

	// A one-click connector (OAuth grant) counts as well; a revoked one does not.
	if err := w.db.Create(&mcp.OAuthGrant{ID: "g-1", ClientID: "c-1", OrgID: "org-1", UserID: "u-1", Scope: "read_write", CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := w.db.Create(&mcp.OAuthGrant{ID: "g-2", ClientID: "c-1", OrgID: "org-1", UserID: "u-1", Scope: "read", CreatedAt: now, RevokedAt: &past}).Error; err != nil {
		t.Fatal(err)
	}
	m = mcpOf(status("u-1", "org-1", "admin"))
	if m["connected"] != true || m["mine"] != true || m["apps"] != float64(1) || m["tokens"] != float64(1) {
		t.Fatalf("with a connector: %v", m)
	}
	// Another organisation still sees nothing of this.
	if m := mcpOf(status("u-7", "org-3", "admin")); m["connected"] != false || m["apps"] != float64(0) {
		t.Fatalf("other organisation: %v", m)
	}
	// MCP being connected does not switch the in-app assistant on.
	if d := status("u-1", "org-1", "super_admin"); d["enabled"] != false || d["setup_path"] != "/system-settings?tab=ai" {
		t.Fatalf("status with MCP but no key = %v", d)
	}
}
