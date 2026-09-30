package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Handler serves /email-settings: the organisation's (or, for super admins,
// the platform default) outgoing email account.
type Handler struct {
	Svc *Service
}

// NewHandler builds a handler on the shared service.
func NewHandler(db *gorm.DB) *Handler { return &Handler{Svc: Shared(db)} }

// RegisterRoutes mounts the API on an authenticated group.
func (h *Handler) RegisterRoutes(g *gin.RouterGroup) {
	g.GET("/email-settings", h.Get)
	g.PUT("/email-settings", h.Update)
	g.GET("/email-settings/status", h.GetStatus)
	g.POST("/email-settings/test", h.Test)
	g.POST("/email-settings/verify", h.Verify)
}

func ok(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func canEdit(role string) bool {
	switch role {
	case "super_admin", "admin", "owner", "org_admin":
		return true
	}
	return false
}

// target returns the organisation whose settings the request addresses.
func (h *Handler) target(c *gin.Context, write bool) (string, bool) {
	org := c.GetString("org_id")
	role := c.GetString("user_role")
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "No organisation in the session")
		return "", false
	}
	if c.Query("scope") == "platform" && role != "super_admin" {
		fail(c, http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Only a platform administrator can change the platform default")
		return "", false
	}
	if write && !canEdit(role) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only administrators can change the email settings")
		return "", false
	}
	return org, true
}

func (h *Handler) row(org string, create bool) (*models.IntegrationSettings, error) {
	var s models.IntegrationSettings
	err := h.Svc.DB.Omit("Organization").Where("organization_id = ?", org).Order("created_at").First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		s = models.IntegrationSettings{
			ID: uuid.NewString(), OrganizationID: org,
			GeneralSettings:  models.GeneralSettings{PlatformName: "DASYIN ERP", DefaultTimezone: "UTC", DefaultCurrency: "AUD", DefaultLanguage: "en"},
			EmailSettings:    models.EmailSettings{Provider: "smtp", SMTPPort: 587, SMTPSecurity: "tls"},
			SecuritySettings: models.SecuritySettings{SessionTimeout: 30, PasswordMinLength: 8, RequireSpecialChars: true, RequireNumbers: true, RateLimitRequests: 100, RateLimitWindow: 15},
			StripeSettings:   models.StripeSettings{TestMode: true, Currency: "AUD", CaptureMethod: "automatic", PaymentMethods: "card"},
			WhatsAppSettings: models.WhatsAppSettings{APIVersion: "v18.0"},
		}
		if !create {
			return &s, nil
		}
		if err := h.Svc.DB.Omit("Organization").Create(&s).Error; err != nil {
			return nil, err
		}
		return &s, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// secretHint describes a stored password without revealing it.
func secretHint(stored string) gin.H {
	if stored == "" {
		return gin.H{"set": false, "hint": ""}
	}
	pw, err := DecryptSecret(stored)
	if err != nil || len(pw) <= 8 {
		return gin.H{"set": true, "hint": "••••"}
	}
	return gin.H{"set": true, "hint": "••••" + pw[len(pw)-2:]}
}

func publicEmail(e models.EmailSettings) gin.H {
	cc, _ := ParseList(e.AlwaysCC)
	bcc, _ := ParseList(e.AlwaysBCC)
	notify, _ := ParseList(e.NotificationRecipients)
	sec := "starttls"
	switch NormalizeSecurity(e.SMTPSecurity, e.SMTPPort) {
	case SecuritySSL:
		sec = "ssl"
	case SecurityNone:
		sec = "none"
	}
	return gin.H{
		"enabled": e.Enabled, "provider": e.Provider, "smtp_host": e.SMTPHost, "smtp_port": e.SMTPPort,
		"smtp_security": sec, "smtp_username": e.SMTPUsername, "from_name": e.FromName, "from_email": e.FromEmail,
		"reply_to_email": e.ReplyToEmail, "always_cc": cc, "always_bcc": bcc, "notification_recipients": notify,
		"last_tested_at": e.LastTestedAt,
	}
}

func (h *Handler) view(c *gin.Context, org string, s *models.IntegrationSettings, saved bool) gin.H {
	isPlatform := h.Svc.IsPlatformOrg(org)
	out := gin.H{
		"scope":       map[bool]string{true: "platform", false: "organisation"}[isPlatform],
		"is_platform": isPlatform,
		"can_edit":    canEdit(c.GetString("user_role")),
		"saved":       saved,
		"settings":    publicEmail(s.EmailSettings),
		"secrets":     gin.H{"smtp_password": secretHint(s.EmailSettings.SMTPPassword)},
		"status":      h.Svc.Status(org),
		"updated_at":  s.UpdatedAt,
	}
	// What this organisation falls back to without its own settings.
	if !isPlatform {
		for _, id := range h.Svc.PlatformOrgs() {
			if row, ok := h.Svc.loadSettings(id); ok {
				if cfg, usable := FromSettings(row.EmailSettings); usable {
					out["fallback"] = gin.H{"source": SourcePlatform, "from": cfg.Display(), "ready": cfg.Ready()}
					break
				}
			}
		}
		if _, ok := out["fallback"]; !ok {
			if env := FromEnv(); env.Host != "" {
				out["fallback"] = gin.H{"source": SourceEnvironment, "from": env.Display(), "ready": env.Ready()}
			}
		}
	} else if env := FromEnv(); env.Host != "" {
		out["fallback"] = gin.H{"source": SourceEnvironment, "from": env.Display(), "ready": env.Ready()}
	}
	return out
}

// Get GET /email-settings
func (h *Handler) Get(c *gin.Context) {
	org, okk := h.target(c, false)
	if !okk {
		return
	}
	s, err := h.row(org, false)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load the email settings")
		return
	}
	ok(c, h.view(c, org, s, s.CreatedAt.IsZero() == false))
}

// GetStatus GET /email-settings/status — the effective sender for the
// caller's organisation (any signed-in user).
func (h *Handler) GetStatus(c *gin.Context) {
	org := c.GetString("org_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "No organisation in the session")
		return
	}
	ok(c, h.Svc.Status(org))
}

// settingsInput is the editable part of the email settings.
type settingsInput struct {
	Enabled                *bool     `json:"enabled"`
	Provider               *string   `json:"provider"`
	SMTPHost               *string   `json:"smtp_host"`
	SMTPPort               *int      `json:"smtp_port"`
	SMTPSecurity           *string   `json:"smtp_security"`
	SMTPUsername           *string   `json:"smtp_username"`
	SMTPPassword           *string   `json:"smtp_password"` // blank keeps the saved one
	ClearPassword          bool      `json:"clear_password"`
	FromName               *string   `json:"from_name"`
	FromEmail              *string   `json:"from_email"`
	ReplyToEmail           *string   `json:"reply_to_email"`
	AlwaysCC               *[]string `json:"always_cc"`
	AlwaysBCC              *[]string `json:"always_bcc"`
	NotificationRecipients *[]string `json:"notification_recipients"`
}

// apply merges in into e and validates. It returns field errors.
func (in settingsInput) apply(e *models.EmailSettings) map[string]string {
	errs := map[string]string{}
	str := func(p *string) string { return strings.TrimSpace(*p) }
	if in.Enabled != nil {
		e.Enabled = *in.Enabled
	}
	if in.Provider != nil {
		e.Provider = str(in.Provider)
	}
	if in.SMTPHost != nil {
		h := strings.ToLower(str(in.SMTPHost))
		h = strings.TrimPrefix(strings.TrimPrefix(h, "smtp://"), "smtps://")
		if strings.ContainsAny(h, " /:@") {
			errs["smtp_host"] = "Enter just the host name, e.g. smtp.gmail.com"
		}
		e.SMTPHost = h
	}
	if in.SMTPPort != nil {
		if *in.SMTPPort < 1 || *in.SMTPPort > 65535 {
			errs["smtp_port"] = "Port must be between 1 and 65535"
		}
		e.SMTPPort = *in.SMTPPort
	}
	if in.SMTPSecurity != nil {
		switch strings.ToLower(str(in.SMTPSecurity)) {
		case "starttls", "tls":
			e.SMTPSecurity = "tls" // stored name for STARTTLS
		case "ssl":
			e.SMTPSecurity = "ssl"
		case "none":
			e.SMTPSecurity = "none"
		default:
			errs["smtp_security"] = "Security must be STARTTLS, SSL/TLS or none"
		}
	}
	if in.SMTPUsername != nil {
		e.SMTPUsername = str(in.SMTPUsername)
	}
	if in.ClearPassword {
		e.SMTPPassword = ""
	} else if in.SMTPPassword != nil && strings.TrimSpace(*in.SMTPPassword) != "" {
		pw := CleanPassword(e.SMTPHost, *in.SMTPPassword)
		enc, err := EncryptSecret(pw)
		if err != nil {
			errs["smtp_password"] = "The password could not be stored securely"
		}
		e.SMTPPassword = enc
	}
	if in.FromName != nil {
		e.FromName = headerClean(*in.FromName)
		if len(e.FromName) > 120 {
			errs["from_name"] = "Keep the From name under 120 characters"
		}
	}
	if in.FromEmail != nil {
		e.FromEmail = strings.ToLower(str(in.FromEmail))
		if e.FromEmail != "" && !ValidAddress(e.FromEmail) {
			errs["from_email"] = "Enter a valid From email address"
		}
	}
	if in.ReplyToEmail != nil {
		e.ReplyToEmail = strings.ToLower(str(in.ReplyToEmail))
		if e.ReplyToEmail != "" && !ValidAddress(e.ReplyToEmail) {
			errs["reply_to_email"] = "Enter a valid reply-to address"
		}
	}
	list := func(p *[]string, key string, dst *string) {
		if p == nil {
			return
		}
		valid, invalid := ParseList(*p...)
		if len(invalid) > 0 {
			errs[key] = "Not a valid email address: " + strings.Join(invalid, ", ")
		}
		if len(valid) > 20 {
			errs[key] = "At most 20 addresses"
		}
		*dst = JoinList(valid)
	}
	list(in.AlwaysCC, "always_cc", &e.AlwaysCC)
	list(in.AlwaysBCC, "always_bcc", &e.AlwaysBCC)
	list(in.NotificationRecipients, "notification_recipients", &e.NotificationRecipients)
	if e.Enabled {
		if e.SMTPHost == "" {
			errs["smtp_host"] = "Enter the SMTP host (for Gmail: smtp.gmail.com)"
		}
		if e.FromEmail == "" && !strings.Contains(e.SMTPUsername, "@") {
			errs["from_email"] = "Enter the From email address"
		}
		if e.SMTPUsername != "" && e.SMTPPassword == "" {
			errs["smtp_password"] = "Enter the password (for Gmail, a 16-character App Password)"
		}
		if e.SMTPUsername != "" && NormalizeSecurity(e.SMTPSecurity, e.SMTPPort) == SecurityNone {
			errs["smtp_security"] = "Sign-in needs an encrypted connection — choose STARTTLS or SSL/TLS"
		}
	}
	return errs
}

func firstError(errs map[string]string) string {
	for _, k := range []string{"smtp_host", "smtp_port", "smtp_security", "smtp_username", "smtp_password", "from_name", "from_email", "reply_to_email", "always_cc", "always_bcc", "notification_recipients"} {
		if m, ok := errs[k]; ok {
			return m
		}
	}
	return "Check the highlighted fields"
}

// Update PUT /email-settings
func (h *Handler) Update(c *gin.Context) {
	org, okk := h.target(c, true)
	if !okk {
		return
	}
	var in settingsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body: "+err.Error())
		return
	}
	s, err := h.row(org, true)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load the email settings")
		return
	}
	e := s.EmailSettings
	if errs := in.apply(&e); len(errs) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"code": "VALIDATION_ERROR", "message": firstError(errs), "fields": errs}})
		return
	}
	now := time.Now()
	s.EmailSettings = e
	s.UpdatedAt = now
	if err := h.Svc.DB.Omit("Organization").Save(s).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to save the email settings")
		return
	}
	InvalidateAll()
	h.log(c, org, "settings_saved", "success", "Outgoing email settings saved", 0)
	ok(c, h.view(c, org, s, true))
}

func (h *Handler) log(c *gin.Context, org, event, status, msg string, ms int) {
	if h.Svc.DB == nil || !h.Svc.DB.Migrator().HasTable(&models.IntegrationLog{}) {
		return
	}
	h.Svc.DB.Omit("Organization").Create(&models.IntegrationLog{OrganizationID: org, Integration: "email", Event: event,
		Status: status, Message: truncate(msg, 900), Duration: ms, UserID: c.GetString("user_id"), IPAddress: c.ClientIP()})
}

type testInput struct {
	To    []string       `json:"to"`
	Draft *settingsInput `json:"draft"` // test unsaved changes
}

// configFor returns the saved configuration of org merged with an optional
// draft (unsaved changes). Without a draft it is the effective (resolved)
// configuration.
func (h *Handler) configFor(org string, draft *settingsInput) (Config, map[string]string, error) {
	if draft == nil {
		cfg, _ := h.Svc.Resolve(org)
		return cfg, nil, nil
	}
	s, err := h.row(org, false)
	if err != nil {
		return Config{}, nil, err
	}
	e := s.EmailSettings
	d := *draft
	t := true
	d.Enabled = &t
	if errs := d.apply(&e); len(errs) > 0 {
		return Config{}, errs, nil
	}
	cfg, _ := FromSettings(e)
	cfg.Source = SourceOrganisation
	if h.Svc.IsPlatformOrg(org) {
		cfg.Source = SourcePlatform
	}
	return cfg, nil, nil
}

// Test POST /email-settings/test — sends a real test email to the given
// addresses, or the notification recipients, or the signed-in user.
func (h *Handler) Test(c *gin.Context) {
	org, okk := h.target(c, true)
	if !okk {
		return
	}
	var in testInput
	_ = c.ShouldBindJSON(&in)
	cfg, errs, err := h.configFor(org, in.Draft)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load the email settings")
		return
	}
	if len(errs) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"code": "VALIDATION_ERROR", "message": firstError(errs), "fields": errs}})
		return
	}
	to, invalid := ParseList(in.To...)
	if len(invalid) > 0 {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Not a valid email address: "+strings.Join(invalid, ", "))
		return
	}
	if len(to) == 0 {
		to = cfg.Notify
	}
	if len(to) == 0 {
		if e := strings.ToLower(c.GetString("user_email")); ValidAddress(e) {
			to = []string{e}
		}
	}
	if len(to) == 0 {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Enter an address to send the test email to")
		return
	}
	if !cfg.Ready() {
		msg := ErrNotConfigured.Msg
		if cfg.Problem != "" {
			msg = cfg.Problem
		}
		ok(c, gin.H{"success": false, "code": "NOT_CONFIGURED", "message": msg, "to": to, "status": h.Svc.Status(org)})
		return
	}
	start := time.Now()
	body := fmt.Sprintf("This is a test message from DASYIN ERP.\n\nIf you received it, outgoing email is working.\n\nSent from: %s\nServer: %s:%d (%s)\nSettings: %s\nTime: %s\n",
		cfg.Display(), cfg.Host, cfg.Port, strings.ToUpper(cfg.Security), SourceLabel(cfg.Source), start.UTC().Format(time.RFC1123))
	ctx, cancel := context.WithTimeout(c.Request.Context(), Timeout+5*time.Second)
	defer cancel()
	sendErr := h.Svc.SendWith(ctx, org, cfg, Message{To: to, Subject: "DASYIN test email ✓", Text: body})
	ms := int(time.Since(start).Milliseconds())
	if sendErr != nil {
		h.log(c, org, "test_email", "error", "Test email failed: "+sendErr.Error(), ms)
		ok(c, gin.H{"success": false, "code": Code(sendErr), "message": sendErr.Error(), "to": to, "status": h.Svc.Status(org)})
		return
	}
	if in.Draft == nil {
		now := time.Now()
		h.Svc.DB.Model(&models.IntegrationSettings{}).Where("organization_id = ?", org).Updates(map[string]interface{}{"email_last_tested_at": now, "email_test_email_sent": true})
	}
	msg := "Test email sent to " + strings.Join(to, ", ") + " from " + cfg.Display()
	h.log(c, org, "test_email", "success", msg, ms)
	ok(c, gin.H{"success": true, "message": msg, "to": to, "status": h.Svc.Status(org)})
}

// Verify POST /email-settings/verify — connects and signs in without sending.
func (h *Handler) Verify(c *gin.Context) {
	org, okk := h.target(c, true)
	if !okk {
		return
	}
	var in testInput
	_ = c.ShouldBindJSON(&in)
	cfg, errs, err := h.configFor(org, in.Draft)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load the email settings")
		return
	}
	if len(errs) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"code": "VALIDATION_ERROR", "message": firstError(errs), "fields": errs}})
		return
	}
	start := time.Now()
	if cfg.Problem != "" {
		ok(c, gin.H{"success": false, "message": cfg.Problem})
		return
	}
	perr := Probe(c.Request.Context(), cfg)
	ms := int(time.Since(start).Milliseconds())
	if perr != nil {
		h.log(c, org, "connection_test", "error", perr.Error(), ms)
		ok(c, gin.H{"success": false, "code": Code(perr), "message": perr.Error()})
		return
	}
	msg := fmt.Sprintf("Connected to %s:%d", cfg.Host, cfg.Port)
	if cfg.Username != "" {
		msg += " and signed in as " + cfg.Username
	}
	h.log(c, org, "connection_test", "success", msg, ms)
	ok(c, gin.H{"success": true, "message": msg})
}
