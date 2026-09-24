package platform

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

// secretFields are never returned to the browser. Each entry is
// "<section json key>.<field json key>".
var secretFields = []string{
	"email_settings.smtp_password",
	"stripe_settings.secret_key",
	"stripe_settings.webhook_secret",
	"twilio_settings.auth_token",
	"whatsapp_settings.access_token",
	"whatsapp_settings.verify_token",
}

// Sections the settings page may edit.
var editableSections = []string{"general_settings", "email_settings", "security_settings", "stripe_settings", "twilio_settings", "whatsapp_settings"}

// Fields the browser may not set directly.
var serverOwned = []string{"connection_verified", "last_tested_at", "test_email_sent"}

func (h *Handler) loadIntegrationSettings(orgID string) (*models.IntegrationSettings, error) {
	var s models.IntegrationSettings
	err := h.DB.Where("organization_id = ?", orgID).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		s = models.IntegrationSettings{
			OrganizationID:  orgID,
			GeneralSettings: models.GeneralSettings{PlatformName: "DASYIN ERP", DefaultTimezone: "Australia/Adelaide", DefaultCurrency: "AUD", DefaultLanguage: "en"},
			EmailSettings:   models.EmailSettings{Provider: "smtp", SMTPPort: 587, SMTPSecurity: "tls", FromName: "DASYIN ERP"},
			SecuritySettings: models.SecuritySettings{SessionTimeout: 30, PasswordMinLength: 8, RequireSpecialChars: true,
				RequireNumbers: true, RateLimitRequests: 100, RateLimitWindow: 15},
			StripeSettings:   models.StripeSettings{TestMode: true, Currency: "AUD", CaptureMethod: "automatic", PaymentMethods: "card"},
			TwilioSettings:   models.TwilioSettings{EnableSMS: true},
			WhatsAppSettings: models.WhatsAppSettings{APIVersion: "v18.0"},
		}
		if err := h.DB.Omit("Organization").Create(&s).Error; err != nil {
			return nil, err
		}
		return &s, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func toMap(v interface{}) map[string]interface{} {
	b, _ := json.Marshal(v)
	m := map[string]interface{}{}
	_ = json.Unmarshal(b, &m)
	return m
}

func hint(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}

// publicSettings strips secrets and reports which ones are set.
func publicSettings(s *models.IntegrationSettings) gin.H {
	all := toMap(s)
	out := gin.H{"updated_at": s.UpdatedAt}
	for _, sec := range editableSections {
		out[sec] = all[sec]
	}
	secrets := gin.H{}
	for _, f := range secretFields {
		parts := strings.SplitN(f, ".", 2)
		sec, _ := out[parts[0]].(map[string]interface{})
		if sec == nil {
			continue
		}
		v, _ := sec[parts[1]].(string)
		secrets[f] = gin.H{"set": v != "", "hint": hint(v)}
		sec[parts[1]] = ""
	}
	out["secrets"] = secrets
	return out
}

// GetIntegrations returns the platform organisation's integration settings
// plus the server's runtime configuration (no secrets).
func (h *Handler) GetIntegrations(c *gin.Context) {
	s, err := h.loadIntegrationSettings(actorOrg(c))
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load settings")
		return
	}
	out := publicSettings(s)
	out["runtime"] = h.runtimeInfo()
	ok(c, http.StatusOK, out)
}

func (h *Handler) runtimeInfo() gin.H {
	m := invoicing.SMTPMailerFromEnv()
	env := ""
	var jwtHours float64
	if h.Cfg != nil {
		env = h.Cfg.Environment
		jwtHours = h.Cfg.JWTExpiry.Hours()
	}
	return gin.H{
		"environment":   env,
		"go_version":    runtime.Version(),
		"session_hours": jwtHours,
		"smtp": gin.H{
			"configured": m.Configured(),
			"host":       m.Host,
			"port":       m.Port,
			"from":       m.From,
			"auth":       m.Username != "",
		},
	}
}

// SendTestEmail sends a real message through the server's outgoing mail
// configuration (SMTP_* environment variables), which invoices and meeting
// invitations use.
func (h *Handler) SendTestEmail(c *gin.Context) {
	var req struct {
		To string `json:"to"`
	}
	_ = c.ShouldBindJSON(&req)
	to := strings.TrimSpace(req.To)
	if to == "" {
		to = c.GetString("user_email")
	}
	if _, err := mail.ParseAddress(to); err != nil || to == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Enter a valid email address")
		return
	}
	m := invoicing.SMTPMailerFromEnv()
	if !m.Configured() {
		fail(c, http.StatusBadRequest, "SMTP_NOT_CONFIGURED", "Outgoing email is not configured on the server (set SMTP_HOST and SMTP_FROM)")
		return
	}
	start := time.Now()
	err := m.Send(to, "DASYIN test email", "This is a test message from the DASYIN platform admin.\n\nIf you received it, outgoing email is working.")
	status, msg := "success", "Test email sent to "+to
	if err != nil {
		status, msg = "error", "Sending failed: "+err.Error()
	}
	h.DB.Omit("Organization").Create(&models.IntegrationLog{OrganizationID: actorOrg(c), Integration: "email", Event: "test_email",
		Status: status, Message: msg, Duration: int(time.Since(start).Milliseconds()), UserID: actorID(c), IPAddress: c.ClientIP()})
	ok(c, http.StatusOK, gin.H{"success": err == nil, "message": msg})
}

// UpdateIntegrations merges the posted sections into the stored settings.
// Blank secret fields keep their stored value unless listed in clear_secrets.
func (h *Handler) UpdateIntegrations(c *gin.Context) {
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	s, err := h.loadIntegrationSettings(actorOrg(c))
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load settings")
		return
	}
	clear := map[string]bool{}
	if raw, ok := body["clear_secrets"]; ok {
		var list []string
		_ = json.Unmarshal(raw, &list)
		for _, k := range list {
			clear[k] = true
		}
	}
	current := toMap(s)
	changed := []string{}
	for _, sec := range editableSections {
		raw, present := body[sec]
		if !present {
			continue
		}
		incoming := map[string]interface{}{}
		if err := json.Unmarshal(raw, &incoming); err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid "+sec)
			return
		}
		existing, _ := current[sec].(map[string]interface{})
		if existing == nil {
			existing = map[string]interface{}{}
		}
		for k, v := range incoming {
			skip := false
			for _, so := range serverOwned {
				if k == so {
					skip = true
				}
			}
			if skip {
				continue
			}
			key := sec + "." + k
			if isSecret(key) {
				if str, _ := v.(string); str == "" && !clear[key] {
					continue
				}
			}
			existing[k] = v
		}
		for _, f := range secretFields {
			if clear[f] && strings.HasPrefix(f, sec+".") {
				existing[strings.TrimPrefix(f, sec+".")] = ""
			}
		}
		current[sec] = existing
		changed = append(changed, sec)
	}
	b, _ := json.Marshal(current)
	merged := *s
	if err := json.Unmarshal(b, &merged); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Some values have the wrong type: "+err.Error())
		return
	}
	merged.ID, merged.OrganizationID, merged.CreatedAt = s.ID, s.OrganizationID, s.CreatedAt
	if msg := validateSettings(&merged); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	merged.UpdatedAt = time.Now()
	if err := h.DB.Omit("Organization").Save(&merged).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to save settings")
		return
	}
	Audit(h.DB, c, "", "settings.integrations.update", "integration_settings", merged.ID, nil, gin.H{"sections": changed})
	ok(c, http.StatusOK, publicSettings(&merged))
}

func isSecret(k string) bool {
	for _, f := range secretFields {
		if f == k {
			return true
		}
	}
	return false
}

func validateSettings(s *models.IntegrationSettings) string {
	e := s.EmailSettings
	if e.SMTPPort < 0 || e.SMTPPort > 65535 {
		return "SMTP port must be between 1 and 65535"
	}
	if e.SMTPSecurity != "" && e.SMTPSecurity != "tls" && e.SMTPSecurity != "ssl" && e.SMTPSecurity != "none" {
		return "SMTP security must be TLS, SSL or none"
	}
	sec := s.SecuritySettings
	if sec.PasswordMinLength < 6 || sec.PasswordMinLength > 64 {
		return "Minimum password length must be between 6 and 64"
	}
	if sec.SessionTimeout < 5 || sec.SessionTimeout > 1440 {
		return "Session timeout must be between 5 and 1440 minutes"
	}
	if len(s.GeneralSettings.DefaultCurrency) != 3 && s.GeneralSettings.DefaultCurrency != "" {
		return "Currency must be a 3-letter code"
	}
	if len(s.StripeSettings.Currency) != 3 && s.StripeSettings.Currency != "" {
		return "Stripe currency must be a 3-letter code"
	}
	if k := s.StripeSettings.SecretKey; k != "" && !strings.HasPrefix(k, "sk_") && !strings.HasPrefix(k, "rk_") {
		return "The Stripe secret key should start with sk_ or rk_"
	}
	if k := s.StripeSettings.PublishableKey; k != "" && !strings.HasPrefix(k, "pk_") {
		return "The Stripe publishable key should start with pk_"
	}
	return ""
}

var httpClient = &http.Client{Timeout: 12 * time.Second}

// TestIntegration performs a real connection check against the provider.
func (h *Handler) TestIntegration(c *gin.Context) {
	kind := c.Param("kind")
	s, err := h.loadIntegrationSettings(actorOrg(c))
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load settings")
		return
	}
	start := time.Now()
	var detail string
	var column string
	switch kind {
	case "email":
		detail, err = testSMTP(s.EmailSettings)
		column = "email_last_tested_at"
	case "stripe":
		detail, err = testStripe(s.StripeSettings)
		column = "stripe_"
	case "twilio":
		detail, err = testTwilio(s.TwilioSettings)
		column = "twilio_"
	case "whatsapp":
		detail, err = testWhatsApp(s.WhatsAppSettings)
		column = "whatsapp_"
	default:
		fail(c, http.StatusBadRequest, "UNKNOWN_INTEGRATION", "Unknown integration")
		return
	}
	now := time.Now()
	success := err == nil
	upd := map[string]interface{}{}
	if kind == "email" {
		upd["email_last_tested_at"] = now
		upd["email_test_email_sent"] = false
	} else {
		upd[column+"last_tested_at"] = now
		upd[column+"connection_verified"] = success
	}
	h.DB.Model(&models.IntegrationSettings{}).Where("id = ?", s.ID).Updates(upd)

	status, msg := "success", detail
	if !success {
		status, msg = "error", err.Error()
	}
	logRow := models.IntegrationLog{OrganizationID: s.OrganizationID, Integration: kind, Event: "connection_test", Status: status,
		Message: msg, Duration: int(time.Since(start).Milliseconds()), UserID: actorID(c), IPAddress: c.ClientIP()}
	h.DB.Omit("Organization").Create(&logRow)

	ok(c, http.StatusOK, gin.H{"success": success, "message": msg, "tested_at": now, "duration_ms": time.Since(start).Milliseconds()})
}

// IntegrationLogs lists the latest integration events for the platform org.
func (h *Handler) IntegrationLogs(c *gin.Context) {
	rows := []models.IntegrationLog{}
	h.DB.Omit("Organization").Where("organization_id = ?", actorOrg(c)).Order("created_at DESC").Limit(50).Find(&rows)
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{"id": r.ID, "integration": r.Integration, "event": r.Event, "status": r.Status,
			"message": r.Message, "duration_ms": r.Duration, "created_at": r.CreatedAt})
	}
	ok(c, http.StatusOK, gin.H{"logs": out})
}

func testSMTP(e models.EmailSettings) (string, error) {
	if strings.TrimSpace(e.SMTPHost) == "" {
		return "", errors.New("Enter an SMTP host first")
	}
	port := e.SMTPPort
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(e.SMTPHost, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: e.SMTPHost, MinVersion: tls.VersionTLS12}
	if e.SMTPSecurity == "ssl" || port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return "", fmt.Errorf("Could not connect to %s: %v", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	cl, err := smtp.NewClient(conn, e.SMTPHost)
	if err != nil {
		conn.Close()
		return "", fmt.Errorf("The server did not respond like an SMTP server: %v", err)
	}
	defer cl.Close()
	if e.SMTPSecurity == "tls" && port != 465 {
		if okExt, _ := cl.Extension("STARTTLS"); !okExt {
			return "", errors.New("The server does not support STARTTLS; choose SSL or none")
		}
		if err := cl.StartTLS(tlsCfg); err != nil {
			return "", fmt.Errorf("STARTTLS failed: %v", err)
		}
	}
	if e.SMTPUsername != "" {
		if err := cl.Auth(smtp.PlainAuth("", e.SMTPUsername, e.SMTPPassword, e.SMTPHost)); err != nil {
			return "", fmt.Errorf("Authentication failed: %v", err)
		}
	}
	_ = cl.Quit()
	return fmt.Sprintf("Connected to %s and signed in successfully", addr), nil
}

func getJSON(req *http.Request) (int, map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	resp, err := httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	m := map[string]interface{}{}
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m, nil
}

func providerError(m map[string]interface{}, status int) error {
	if e, ok := m["error"].(map[string]interface{}); ok {
		if msg, ok := e["message"].(string); ok {
			return errors.New(msg)
		}
	}
	if msg, ok := m["message"].(string); ok {
		return errors.New(msg)
	}
	return fmt.Errorf("The provider answered with HTTP %d", status)
}

func testStripe(s models.StripeSettings) (string, error) {
	if s.SecretKey == "" {
		return "", errors.New("Enter a Stripe secret key first")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://api.stripe.com/v1/balance", nil)
	req.SetBasicAuth(s.SecretKey, "")
	status, m, err := getJSON(req)
	if err != nil {
		return "", fmt.Errorf("Could not reach Stripe: %v", err)
	}
	if status != http.StatusOK {
		return "", providerError(m, status)
	}
	mode := "live"
	if live, ok := m["livemode"].(bool); ok && !live {
		mode = "test"
	}
	return "Stripe accepted the key (" + mode + " mode)", nil
}

func testTwilio(s models.TwilioSettings) (string, error) {
	if s.AccountSID == "" || s.AuthToken == "" {
		return "", errors.New("Enter the Twilio account SID and auth token first")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://api.twilio.com/2010-04-01/Accounts/"+url.PathEscape(s.AccountSID)+".json", nil)
	req.SetBasicAuth(s.AccountSID, s.AuthToken)
	status, m, err := getJSON(req)
	if err != nil {
		return "", fmt.Errorf("Could not reach Twilio: %v", err)
	}
	if status != http.StatusOK {
		return "", providerError(m, status)
	}
	name, _ := m["friendly_name"].(string)
	st, _ := m["status"].(string)
	return fmt.Sprintf("Twilio account %q is %s", name, st), nil
}

func testWhatsApp(s models.WhatsAppSettings) (string, error) {
	if s.AccessToken == "" || s.PhoneNumberID == "" {
		return "", errors.New("Enter the WhatsApp access token and phone number ID first")
	}
	ver := s.APIVersion
	if ver == "" {
		ver = "v18.0"
	}
	req, _ := http.NewRequest(http.MethodGet, "https://graph.facebook.com/"+url.PathEscape(ver)+"/"+url.PathEscape(s.PhoneNumberID)+"?fields=display_phone_number,verified_name", nil)
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	status, m, err := getJSON(req)
	if err != nil {
		return "", fmt.Errorf("Could not reach the WhatsApp Cloud API: %v", err)
	}
	if status != http.StatusOK {
		return "", providerError(m, status)
	}
	num, _ := m["display_phone_number"].(string)
	name, _ := m["verified_name"].(string)
	return strings.TrimSpace(fmt.Sprintf("Connected to %s %s", name, num)), nil
}
