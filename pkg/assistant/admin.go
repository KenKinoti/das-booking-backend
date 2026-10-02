package assistant

// Super-admin API for the platform's Anthropic configuration
// (System settings → AI assistant):
//
//	GET    /api/v1/super-admin/ai/settings         masked settings + status
//	PUT    /api/v1/super-admin/ai/settings         save a key and/or the model
//	DELETE /api/v1/super-admin/ai/settings         remove the saved key
//	POST   /api/v1/super-admin/ai/settings/test    one-token Messages API call
//	GET    /api/v1/super-admin/ai/settings/models  models the key can use
//
// The API key is write-only: it is encrypted before it is stored and is never
// returned, logged or written to the audit log (only a "sk-ant-…1a2B" hint).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// ConsoleKeysURL is where an Anthropic API key is created.
const ConsoleKeysURL = "https://console.anthropic.com/settings/keys"

// ModelOption is a suggestion for the model picker.
type ModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Note  string `json:"note,omitempty"`
}

// ModelSuggestions are offered in the picker; any model id can be typed.
func ModelSuggestions() []ModelOption {
	return []ModelOption{
		{ID: "claude-sonnet-4-5", Label: "Claude Sonnet 4.5", Note: "Default — balanced speed, quality and cost"},
		{ID: "claude-haiku-4-5", Label: "Claude Haiku 4.5", Note: "Fastest and cheapest"},
		{ID: "claude-opus-4-5", Label: "Claude Opus 4.5", Note: "Most capable, highest cost"},
	}
}

// Test result codes.
const (
	TestOK            = "ok"
	TestNoKey         = "no_key"
	TestInvalidKey    = "invalid_key"
	TestNoCredit      = "no_credit"
	TestModelNotFound = "model_not_found"
	TestPermission    = "permission"
	TestRateLimited   = "rate_limited"
	TestUnavailable   = "unavailable"
	TestNetwork       = "network"
	TestError         = "error"
)

// TestResult is the outcome of CheckKey.
type TestResult struct {
	OK         bool   `json:"ok"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Model      string `json:"model"`
	HTTPStatus int    `json:"http_status,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// CheckKey makes the smallest possible Messages API call (max_tokens 1) with
// cfg and maps the outcome to a clear result. hc may be nil.
func CheckKey(ctx context.Context, cfg Config, hc *http.Client) TestResult {
	res := TestResult{Model: cfg.Model}
	if strings.TrimSpace(cfg.APIKey) == "" {
		res.Code, res.Message = TestNoKey, "No Anthropic API key to test. Paste a key first."
		return res
	}
	if hc == nil {
		hc = &http.Client{Timeout: 25 * time.Second}
	}
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	body, _ := json.Marshal(map[string]any{
		"model": cfg.Model, "max_tokens": 1,
		"messages": []map[string]string{{"role": "user", "content": "Hi"}},
	})
	started := time.Now()
	fail := func(code, msg string) TestResult {
		res.Code, res.Message, res.DurationMS = code, msg, time.Since(started).Milliseconds()
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return fail(TestError, "The test could not be started: the Anthropic address is not valid.")
	}
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fail(TestNetwork, networkMessage(err, base))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	res.HTTPStatus = resp.StatusCode
	if resp.StatusCode == http.StatusOK {
		res.OK = true
		return fail(TestOK, fmt.Sprintf("The key works — Anthropic answered with model %s.", cfg.Model))
	}
	ae := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		ae.Type, ae.Message = e.Error.Type, e.Error.Message
	} else if len(ae.Message) > 200 {
		ae.Message = ae.Message[:200]
	}
	code, msg := classifyTestError(ae, cfg.Model)
	return fail(code, msg)
}

// classifyTestError maps an Anthropic error to a result code and a message
// that says what to do about it.
func classifyTestError(e *APIError, model string) (string, string) {
	switch {
	case e.Status == 401 || e.Type == "authentication_error":
		return TestInvalidKey, "Anthropic rejected this API key. Copy the whole key again from console.anthropic.com → API keys (it starts with sk-ant-), or create a new one."
	case e.noCredit():
		return TestNoCredit, "The key is valid, but the Anthropic account has no credit. Add credit or a payment method at console.anthropic.com → Billing. API usage is billed separately from a Claude subscription."
	case e.Status == 404 || e.Type == "not_found_error":
		return TestModelNotFound, fmt.Sprintf("The key is valid, but the model “%s” was not found. Pick one of the suggested models or check the spelling.", model)
	case e.Status == 403 || e.Type == "permission_error":
		return TestPermission, "Anthropic accepted the key but it is not allowed to use this model or workspace. Check the key's permissions in console.anthropic.com."
	case e.Status == 429 || e.Type == "rate_limit_error":
		return TestRateLimited, "The key was accepted, but Anthropic is rate limiting this account right now. Try again in a minute."
	case e.Status == 529 || e.Type == "overloaded_error" || e.Status >= 500:
		return TestUnavailable, fmt.Sprintf("Anthropic is temporarily unavailable (HTTP %d). The key was not rejected — try again in a minute.", e.Status)
	default:
		m := e.Message
		if m == "" {
			m = "no details"
		}
		return TestError, fmt.Sprintf("Anthropic returned an unexpected answer (HTTP %d): %s", e.Status, truncate(m, 200))
	}
}

// networkMessage explains a transport failure (DNS, firewall, timeout, TLS).
func networkMessage(err error, base string) string {
	host := base
	if u, perr := url.Parse(base); perr == nil && u.Host != "" {
		host = u.Host
	}
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.As(err, &dns):
		return fmt.Sprintf("The server could not find %s (DNS). Check the server's internet connection.", host)
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return fmt.Sprintf("%s did not answer in time. Check the server's internet connection or firewall and try again.", host)
	case strings.Contains(err.Error(), "connection refused"):
		return fmt.Sprintf("The connection to %s was refused. Check the server's firewall or proxy.", host)
	case strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "tls:"):
		return fmt.Sprintf("A secure connection to %s could not be made (TLS). Check the server's clock and certificates.", host)
	}
	return fmt.Sprintf("The server could not reach %s. Check the server's internet connection or firewall and try again.", host)
}

// ---------------------------------------------------------------- validation

var (
	modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/\-]{0,119}$`)
	spaceRe = regexp.MustCompile(`\s`)
)

// cleanKey validates a pasted key. strict applies the sk-ant- check (skipped
// when ANTHROPIC_BASE_URL points at a gateway whose keys look different).
func cleanKey(raw string, strict bool) (string, error) {
	k := strings.TrimSpace(raw)
	k = strings.Trim(k, `"'`)
	if k == "" {
		return "", errors.New("Paste the API key first.")
	}
	if spaceRe.MatchString(k) {
		return "", errors.New("The API key contains spaces or line breaks. Copy it again as one line.")
	}
	if len(k) > 512 {
		return "", errors.New("That is too long to be an API key.")
	}
	if strings.HasPrefix(k, "dsy_") {
		return "", errors.New("That is a DASYIN access token (it lets Claude connect to DASYIN over MCP), not an Anthropic API key. Anthropic keys start with sk-ant- and come from console.anthropic.com → API keys.")
	}
	if strict {
		if !strings.HasPrefix(k, "sk-ant-") {
			return "", errors.New("That does not look like an Anthropic API key — they start with sk-ant-. Copy it from console.anthropic.com → API keys.")
		}
		if len(k) < 20 {
			return "", errors.New("That key looks incomplete. Copy the whole key from console.anthropic.com → API keys.")
		}
	}
	return k, nil
}

func cleanModel(raw string) (string, error) {
	m := strings.TrimSpace(raw)
	if m == "" {
		return "", nil
	}
	if !modelRe.MatchString(m) {
		return "", errors.New("The model name may only contain letters, digits, dots and dashes (for example claude-sonnet-4-5).")
	}
	return m, nil
}

// ---------------------------------------------------------------- handlers

func requireSuperAdmin(c *gin.Context) {
	if c.GetString("user_role") != "super_admin" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"success": false, "error": gin.H{"code": "SUPER_ADMIN_REQUIRED", "message": "Super administrator access required"}})
		return
	}
	c.Next()
}

// RegisterAdminRoutes mounts the super-admin settings API on g
// (/api/v1/super-admin/ai). Every route requires the super_admin role.
func (h *Handler) RegisterAdminRoutes(g *gin.RouterGroup) {
	g.Use(requireSuperAdmin)
	g.GET("/settings", h.adminGet)
	g.PUT("/settings", h.adminPut)
	g.DELETE("/settings", h.adminDelete)
	g.POST("/settings/test", h.adminTest)
	g.GET("/settings/models", h.adminModels)
}

func sourceLabel(src string) string {
	switch src {
	case SourceSettings:
		return "Saved in app"
	case SourceEnvironment:
		return "Environment variable"
	}
	return "Not set"
}

func (h *Handler) userName(id string) string {
	if id == "" {
		return ""
	}
	var u struct {
		FirstName, LastName, Email string
	}
	h.Svc.DB.Table("users").Select("first_name, last_name, email").Where("id = ?", id).Limit(1).Scan(&u)
	if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
		return n
	}
	return u.Email
}

// adminView is the masked settings document. It never contains the key.
func (h *Handler) adminView(c *gin.Context) gin.H {
	db := h.Svc.DB
	cfg := h.Svc.Config()
	env := ConfigFromEnv()
	row, _ := loadPlatformSettings(db)
	envModel := ""
	if env.ModelSource == SourceEnvironment {
		envModel = env.Model
	}
	out := gin.H{
		"enabled":      cfg.APIKey != "",
		"source":       cfg.Source,
		"source_label": sourceLabel(cfg.Source),
		"problem":      cfg.Problem,

		"key_set":        row.APIKey != "",
		"key_hint":       row.KeyHint,
		"key_updated_at": row.KeyUpdatedAt,
		"key_updated_by": h.userName(row.KeyUpdatedBy),
		"env_key_set":    env.APIKey != "",
		"env_key_hint":   MaskKey(env.APIKey),

		"model":             cfg.Model,
		"model_source":      cfg.ModelSource,
		"saved_model":       row.Model,
		"env_model":         envModel,
		"default_model":     DefaultModel,
		"model_suggestions": ModelSuggestions(),

		"custom_endpoint": cfg.BaseURL != DefaultBaseURL,
		"console_url":     ConsoleKeysURL,
		"tools_count":     len(h.Svc.tools()),
		"last_test":       nil,
	}
	if cfg.BaseURL != DefaultBaseURL {
		out["endpoint"] = cfg.BaseURL
	}
	if row.LastTestAt != nil {
		out["last_test"] = gin.H{"at": row.LastTestAt, "ok": row.LastTestOK, "code": row.LastTestCode, "message": row.LastTestMessage, "model": row.LastTestModel}
	}
	y, m, d := time.Now().UTC().Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	out["usage_today"] = gin.H{"platform": h.usageSince(day, ""), "organization": h.usageSince(day, c.GetString("org_id")), "since": day}
	return out
}

func (h *Handler) adminGet(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.adminView(c)})
}

// audit writes one audit-log row. Values never include the key.
func (h *Handler) audit(c *gin.Context, action string, values gin.H) {
	b, err := json.Marshal(values)
	if err != nil {
		b = []byte("{}")
	}
	var uid *string
	if u := c.GetString("user_id"); u != "" {
		uid = &u
	}
	ua := c.Request.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}
	row := models.AuditLog{
		ID: uuid.NewString(), OrganizationID: c.GetString("org_id"), UserID: uid,
		Action: action, EntityType: "ai_settings", EntityID: PlatformSettingsID,
		OldValues: "{}", NewValues: string(b), IPAddress: c.ClientIP(), UserAgent: ua, CreatedAt: time.Now(),
	}
	if err := h.Svc.DB.Omit("Organization", "User").Create(&row).Error; err != nil {
		log.Printf("assistant: audit write failed: %v", err)
	}
}

// savePlatformSettings loads the single settings row (or starts a new one),
// lets change edit it and writes it back.
func savePlatformSettings(db *gorm.DB, change func(*PlatformSettings)) error {
	row, _ := loadPlatformSettings(db)
	row.ID = PlatformSettingsID
	change(&row)
	return db.Save(&row).Error
}

func (r *PlatformSettings) setTest(t *TestResult, at time.Time) {
	if t == nil {
		r.LastTestAt, r.LastTestOK, r.LastTestCode, r.LastTestMessage, r.LastTestModel = nil, false, "", "", ""
		return
	}
	r.LastTestAt, r.LastTestOK, r.LastTestCode, r.LastTestMessage, r.LastTestModel = &at, t.OK, t.Code, t.Message, t.Model
}

func (h *Handler) adminPut(c *gin.Context) {
	var body struct {
		APIKey *string `json:"api_key"`
		Model  *string `json:"model"`
		// Verify (default true) checks a new key with Anthropic before saving;
		// a key Anthropic rejects is not saved.
		Verify *bool `json:"verify"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	cfg := h.Svc.Config()
	newKey := ""
	if body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "" {
		k, err := cleanKey(*body.APIKey, cfg.BaseURL == DefaultBaseURL)
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
			return
		}
		newKey = k
	}
	model := ""
	if body.Model != nil {
		m, err := cleanModel(*body.Model)
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
			return
		}
		model = m
	}
	if newKey == "" && body.Model == nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Nothing to save: paste an API key or choose a model.")
		return
	}
	changed := gin.H{}
	var test *TestResult
	if newKey != "" && (body.Verify == nil || *body.Verify) {
		tc := cfg
		tc.APIKey = newKey
		if body.Model != nil {
			// The model being saved with the key ("" = back to env/default).
			tc.Model = model
			if model == "" {
				tc.Model = ConfigFromEnv().Model
			}
		}
		r := CheckKey(c.Request.Context(), tc, nil)
		test = &r
		if r.Code == TestInvalidKey {
			h.audit(c, "ai_settings.key_rejected", gin.H{"key_hint": MaskKey(newKey), "code": r.Code})
			c.JSON(http.StatusUnprocessableEntity, gin.H{"success": false, "error": gin.H{"code": "INVALID_KEY", "message": r.Message}, "test": r})
			return
		}
	}
	enc := ""
	if newKey != "" {
		var err error
		enc, err = mail.EncryptSecret(newKey)
		if err != nil || !mail.IsEncrypted(enc) {
			fail(c, http.StatusInternalServerError, "ENCRYPTION", "The key could not be encrypted, so it was not saved.")
			return
		}
	}
	now := time.Now()
	err := savePlatformSettings(h.Svc.DB, func(row *PlatformSettings) {
		if body.Model != nil {
			row.Model = model
			changed["model"] = model
			if model == "" {
				changed["model"] = "(default)"
			}
		}
		if newKey != "" {
			row.APIKey, row.KeyHint, row.KeyUpdatedAt, row.KeyUpdatedBy = enc, MaskKey(newKey), &now, c.GetString("user_id")
			row.setTest(test, now)
			changed["key"], changed["key_hint"] = "replaced", row.KeyHint
			if test != nil {
				changed["test"] = test.Code
			}
		}
	})
	if err != nil {
		log.Printf("assistant: saving AI settings failed: %v", err)
		fail(c, http.StatusInternalServerError, "DB", "Could not save the AI settings")
		return
	}
	Invalidate()
	h.audit(c, "ai_settings.update", changed)
	out := h.adminView(c)
	if test != nil {
		out["test"] = test
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
}

// adminDelete removes the saved key (the saved model is kept). The
// environment key, when there is one, takes over.
func (h *Handler) adminDelete(c *gin.Context) {
	row, ok := loadPlatformSettings(h.Svc.DB)
	if ok && row.APIKey != "" {
		now := time.Now()
		err := savePlatformSettings(h.Svc.DB, func(r *PlatformSettings) {
			r.APIKey, r.KeyHint, r.KeyUpdatedAt, r.KeyUpdatedBy = "", "", &now, c.GetString("user_id")
			r.setTest(nil, now)
		})
		if err != nil {
			log.Printf("assistant: removing the AI key failed: %v", err)
			fail(c, http.StatusInternalServerError, "DB", "Could not remove the API key")
			return
		}
		h.audit(c, "ai_settings.remove_key", gin.H{"key": "removed", "key_hint": row.KeyHint})
	}
	Invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.adminView(c)})
}

// adminTest checks a key with Anthropic: the pasted one (api_key) before it
// is saved, or the key in use when none is given.
func (h *Handler) adminTest(c *gin.Context) {
	var body struct {
		APIKey string `json:"api_key"`
		Model  string `json:"model"`
	}
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	cfg := h.Svc.Config()
	candidate := strings.TrimSpace(body.APIKey) != ""
	if candidate {
		k, err := cleanKey(body.APIKey, cfg.BaseURL == DefaultBaseURL)
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
			return
		}
		cfg.APIKey = k
	}
	m, err := cleanModel(body.Model)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	customModel := m != "" && m != cfg.Model
	if m != "" {
		cfg.Model = m
	}
	res := CheckKey(c.Request.Context(), cfg, nil)
	// Remember the result when it describes the configuration in use.
	if !candidate && !customModel && res.Code != TestNoKey {
		if err := savePlatformSettings(h.Svc.DB, func(r *PlatformSettings) { r.setTest(&res, time.Now()) }); err != nil {
			log.Printf("assistant: storing the AI key test result failed: %v", err)
		}
	}
	tested := cfg.Source
	if candidate {
		tested = "pasted"
	}
	h.audit(c, "ai_settings.test", gin.H{"ok": res.OK, "code": res.Code, "model": res.Model, "key": tested})
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"ok": res.OK, "code": res.Code, "message": res.Message, "model": res.Model, "duration_ms": res.DurationMS,
		"source": tested, "tested_at": time.Now(),
	}})
}

// adminModels lists the models the key in use can call (GET /v1/models), so
// the picker can offer real, current names. Falls back to the built-in
// suggestions when there is no key or the list cannot be fetched.
func (h *Handler) adminModels(c *gin.Context) {
	cfg := h.Svc.Config()
	out := gin.H{"models": ModelSuggestions(), "live": false}
	if cfg.APIKey == "" {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/models?limit=100", nil)
	if err == nil {
		req.Header.Set("x-api-key", cfg.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		if resp, derr := (&http.Client{Timeout: 12 * time.Second}).Do(req); derr == nil {
			defer resp.Body.Close()
			var r struct {
				Data []struct {
					ID          string `json:"id"`
					DisplayName string `json:"display_name"`
				} `json:"data"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r) == nil && len(r.Data) > 0 {
				list := make([]ModelOption, 0, len(r.Data))
				for _, m := range r.Data {
					if !modelRe.MatchString(m.ID) {
						continue
					}
					label := m.DisplayName
					if label == "" {
						label = m.ID
					}
					list = append(list, ModelOption{ID: m.ID, Label: label})
				}
				if len(list) > 0 {
					out["models"], out["live"] = list, true
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
}
