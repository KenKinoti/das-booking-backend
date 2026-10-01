// Package sso implements "Sign in with Google / Microsoft": OpenID Connect,
// authorization code flow with PKCE, state and nonce, run on the server.
//
//	GET  /api/v1/public/sso/providers              enabled providers (login page)
//	GET  /api/v1/public/sso/:provider/start        → provider (?redirect=/path, or ?ticket= for linking)
//	GET  /api/v1/public/sso/:provider/callback     provider → app /auth/sso-complete#code=… | #error=…
//	POST /api/v1/public/sso/exchange               {code} → the normal DASYIN session (single use, 2 min)
//	GET  /api/v1/sso/identities                    my linked accounts
//	POST /api/v1/sso/:provider/link                → {url} to start linking (signed in)
//	DELETE /api/v1/sso/identities/:id              unlink
//	GET/POST /api/v1/sso/domains, DELETE /sso/domains/:id   "anyone at this domain may join" (org admins)
//	GET  /api/v1/sso/admin/providers, PUT /sso/admin/providers/:provider   (super admin)
//
// Nobody is signed up silently: an unknown verified email gets a "no
// account" page unless the organisation allows its email domain.
package sso

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Handler serves the SSO endpoints.
type Handler struct {
	DB   *gorm.DB
	OIDC *OIDC
	// Issue creates the normal DASYIN session (access + refresh token, user)
	// — the same as password sign-in (handlers.IssueSession).
	Issue func(c *gin.Context, u *models.User) (interface{}, error)
	// UserLimit returns a message when the organisation can't take another
	// user (plan limit); "" when it can.
	UserLimit func(org string) string
	// PublicAPI / PublicApp: base URLs (PUBLIC_API_URL / PUBLIC_APP_URL);
	// empty → derived from the request.
	PublicAPI string
	PublicApp string
	Now       func() time.Time
}

// NewHandler builds a handler reading PUBLIC_API_URL / PUBLIC_APP_URL.
func NewHandler(db *gorm.DB) *Handler {
	return &Handler{DB: db, OIDC: NewOIDC(), PublicAPI: os.Getenv("PUBLIC_API_URL"), PublicApp: os.Getenv("PUBLIC_APP_URL")}
}

const (
	flowTTL = 10 * time.Minute
	codeTTL = 2 * time.Minute
	// CompletePath is the app page that finishes a sign-in.
	CompletePath = "/auth/sso-complete"
)

// Roles a domain rule may grant (never admin / super_admin).
var JoinRoles = []string{"staff", "manager", "care_worker", "support_coordinator"}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// RegisterPublic mounts the unauthenticated routes on /api/v1/public.
func (h *Handler) RegisterPublic(public *gin.RouterGroup) {
	g := public.Group("/sso")
	g.GET("/providers", h.PublicProviders)
	g.GET("/:provider/start", h.Start)
	g.GET("/:provider/callback", h.Callback)
	g.POST("/exchange", h.Exchange)
}

// RegisterAuth mounts the signed-in routes on /api/v1.
func (h *Handler) RegisterAuth(auth *gin.RouterGroup) {
	g := auth.Group("/sso")
	g.GET("/identities", h.ListIdentities)
	g.DELETE("/identities/:id", h.Unlink)
	g.POST("/:provider/link", h.StartLink)
	g.GET("/domains", h.ListDomains)
	g.POST("/domains", h.orgAdmin, h.AddDomain)
	g.DELETE("/domains/:id", h.orgAdmin, h.DeleteDomain)
	g.GET("/admin/providers", h.superAdmin, h.AdminProviders)
	g.PUT("/admin/providers/:provider", h.superAdmin, h.SaveProvider)
}

// ---------------------------------------------------------------- helpers

func fail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func ok(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashCode(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func knownProvider(p string) bool { return p == Google || p == Microsoft }

func isHTTPS(c *gin.Context) bool {
	return c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
}

func requestBase(c *gin.Context) string {
	scheme := "http"
	if isHTTPS(c) {
		scheme = "https"
	}
	host := c.Request.Host
	if fh := c.GetHeader("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	return scheme + "://" + host
}

func (h *Handler) apiBase(c *gin.Context) string {
	if v := strings.TrimRight(strings.TrimSpace(h.PublicAPI), "/"); v != "" {
		return v
	}
	return requestBase(c)
}

func (h *Handler) appBase(c *gin.Context) string {
	if v := strings.TrimRight(strings.TrimSpace(h.PublicApp), "/"); v != "" {
		return v
	}
	return requestBase(c)
}

// RedirectURI is the callback URL to register at the provider.
func (h *Handler) RedirectURI(c *gin.Context, provider string) string {
	return h.apiBase(c) + "/api/v1/public/sso/" + provider + "/callback"
}

// safeRedirect keeps only app-relative paths ("/invoices?x=1"), never
// another site ("//evil", "https://…", "/\evil").
func safeRedirect(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.ContainsAny(p, "\r\n") || len(p) > 1000 {
		return ""
	}
	if strings.HasPrefix(p, "/login") || strings.HasPrefix(p, CompletePath) {
		return ""
	}
	return p
}

// toApp redirects the browser to the app's completion page with values in
// the URL fragment (never sent to any server, not logged, not in Referer).
func (h *Handler) toApp(c *gin.Context, vals url.Values) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Redirect(http.StatusFound, h.appBase(c)+CompletePath+"#"+vals.Encode())
}

func (h *Handler) toAppError(c *gin.Context, code, provider, email string) {
	v := url.Values{"error": {code}}
	if provider != "" {
		v.Set("provider", provider)
	}
	if email != "" {
		v.Set("email", email)
	}
	h.toApp(c, v)
}

func cookieName(state string) string {
	if len(state) > 12 {
		state = state[:12]
	}
	return "das_sso_" + state
}

func cookiePath(c *gin.Context, provider string) string {
	p := c.Request.URL.Path
	for _, suf := range []string{"/" + provider + "/start", "/" + provider + "/callback"} {
		if strings.HasSuffix(p, suf) {
			return strings.TrimSuffix(p, suf)
		}
	}
	return "/"
}

func (h *Handler) audit(c *gin.Context, org, userID, action, entityID string, data gin.H) {
	if org == "" {
		return
	}
	b, _ := json.Marshal(data)
	ua := c.Request.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}
	var uid *string
	if userID != "" {
		u := userID
		uid = &u
	}
	if entityID == "" {
		entityID = userID
	}
	h.DB.Create(&models.AuditLog{ID: uuid.NewString(), OrganizationID: org, UserID: uid, Action: action, EntityType: "user",
		EntityID: entityID, OldValues: "{}", NewValues: string(b), IPAddress: c.ClientIP(), UserAgent: ua, CreatedAt: h.now()})
}

// orgBlocked: the organisation is suspended / cancelled / merged away.
func (h *Handler) orgBlocked(org string) bool {
	var st struct{ Status string }
	if h.DB.Migrator().HasTable("platform_org_status") {
		h.DB.Table("platform_org_status").Select("status").Where("organization_id = ?", org).Limit(1).Scan(&st)
		if st.Status != "" && st.Status != "active" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- public

// PublicProviders lists the providers the login page should offer.
func (h *Handler) PublicProviders(c *gin.Context) {
	out := []gin.H{}
	for _, p := range Providers {
		if s := Load(h.DB, p); s.Usable() {
			out = append(out, gin.H{"id": p, "name": s.Name})
		}
	}
	ok(c, gin.H{"providers": out})
}

// Start sends the browser to the provider.
func (h *Handler) Start(c *gin.Context) {
	p := c.Param("provider")
	if !knownProvider(p) {
		h.toAppError(c, "unknown_provider", "", "")
		return
	}
	s := Load(h.DB, p)
	if !s.Usable() {
		h.toAppError(c, "provider_disabled", p, "")
		return
	}
	now := h.now()
	var f Flow
	if t := c.Query("ticket"); t != "" {
		// Linking: the signed-in app asked for this ticket (POST /sso/:p/link).
		res := h.DB.Model(&Flow{}).Where("ticket = ? AND provider = ? AND mode = 'link' AND started = false AND expires_at > ?", hashCode(t), p, now).Update("started", true)
		if res.Error != nil || res.RowsAffected != 1 || h.DB.Where("ticket = ?", hashCode(t)).Take(&f).Error != nil {
			h.toAppError(c, "expired", p, "")
			return
		}
	} else {
		f = Flow{State: randToken(32), Provider: p, Nonce: randToken(24), Verifier: randToken(48), Redirect: safeRedirect(c.Query("redirect")),
			Mode: "login", ExpiresAt: now.Add(flowTTL), CreatedAt: now}
		if err := h.DB.Create(&f).Error; err != nil {
			h.toAppError(c, "server_error", p, "")
			return
		}
	}
	h.DB.Where("expires_at < ?", now.Add(-time.Hour)).Delete(&Flow{})
	disc, err := h.OIDC.Discover(c.Request.Context(), s.DiscoveryURL)
	if err != nil {
		h.toAppError(c, "provider_unavailable", p, "")
		return
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {s.ClientID},
		"redirect_uri":          {h.RedirectURI(c, p)},
		"scope":                 {"openid email profile"},
		"state":                 {f.State},
		"nonce":                 {f.Nonce},
		"code_challenge":        {PKCEChallenge(f.Verifier)},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	if p == Microsoft {
		q.Set("response_mode", "query")
	}
	// Binds the callback to this browser (login CSRF).
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName(f.State), Value: f.State, Path: cookiePath(c, p), MaxAge: int(flowTTL.Seconds()),
		HttpOnly: true, Secure: isHTTPS(c), SameSite: http.SameSiteLaxMode})
	sep := "?"
	if strings.Contains(disc.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, disc.AuthorizationEndpoint+sep+q.Encode())
}

// takeFlow loads and deletes the flow (single use).
func (h *Handler) takeFlow(state string) (*Flow, bool) {
	var rows []Flow
	res := h.DB.Clauses(clause.Returning{}).Where("state = ?", state).Delete(&rows)
	if res.Error != nil || res.RowsAffected != 1 || len(rows) != 1 {
		return nil, false
	}
	return &rows[0], true
}

// Callback finishes the provider round trip.
func (h *Handler) Callback(c *gin.Context) {
	p := c.Param("provider")
	if !knownProvider(p) {
		h.toAppError(c, "unknown_provider", "", "")
		return
	}
	state := c.Query("state")
	if state == "" {
		h.toAppError(c, "expired", p, "")
		return
	}
	ck, _ := c.Cookie(cookieName(state))
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName(state), Value: "", Path: cookiePath(c, p), MaxAge: -1, HttpOnly: true, Secure: isHTTPS(c), SameSite: http.SameSiteLaxMode})
	f, found := h.takeFlow(state)
	if !found || f.Provider != p || h.now().After(f.ExpiresAt) {
		h.toAppError(c, "expired", p, "")
		return
	}
	if subtle.ConstantTimeCompare([]byte(ck), []byte(state)) != 1 {
		h.toAppError(c, "browser_mismatch", p, "")
		return
	}
	if e := c.Query("error"); e != "" {
		code := "provider_error"
		if e == "access_denied" {
			code = "cancelled"
		}
		h.toAppError(c, code, p, "")
		return
	}
	s := Load(h.DB, p)
	if !s.Usable() {
		h.toAppError(c, "provider_disabled", p, "")
		return
	}
	claims, err := h.verify(c, s, f, c.Query("code"))
	if err != nil {
		h.toAppError(c, "invalid_token", p, "")
		return
	}
	if f.Mode == "link" {
		h.finishLink(c, f, claims)
		return
	}
	h.finishLogin(c, f, claims)
}

func (h *Handler) verify(c *gin.Context, s Settings, f *Flow, code string) (*Claims, error) {
	if code == "" {
		return nil, errors.New("no code")
	}
	ctx := c.Request.Context()
	disc, err := h.OIDC.Discover(ctx, s.DiscoveryURL)
	if err != nil {
		return nil, err
	}
	tr, err := h.OIDC.Exchange(ctx, disc.TokenEndpoint, s.ClientID, s.ClientSecret, code, h.RedirectURI(c, s.Provider), f.Verifier)
	if err != nil {
		return nil, err
	}
	return h.OIDC.Verify(ctx, tr.IDToken, VerifyOptions{JWKSURI: disc.JWKSURI, ClientID: s.ClientID, Nonce: f.Nonce, IssuerOK: issuerRule(s.Provider, disc.Issuer)})
}

func (h *Handler) identityBySub(provider, sub string) *Identity {
	var id Identity
	if h.DB.Where("provider = ? AND subject = ?", provider, sub).Take(&id).Error != nil {
		return nil
	}
	return &id
}

func (h *Handler) finishLink(c *gin.Context, f *Flow, cl *Claims) {
	p := f.Provider
	var u models.User
	if h.DB.Where("id = ? AND is_active = ?", f.UserID, true).Take(&u).Error != nil {
		h.toAppError(c, "account_disabled", p, "")
		return
	}
	email := VerifiedEmail(p, cl)
	if email == "" {
		email = strings.ToLower(strings.TrimSpace(firstNonEmpty(cl.Email, cl.PreferredUsername)))
	}
	if ex := h.identityBySub(p, cl.Subject); ex != nil {
		if ex.UserID != u.ID {
			h.toAppError(c, "identity_in_use", p, email)
			return
		}
	} else {
		now := h.now()
		id := Identity{ID: uuid.NewString(), UserID: u.ID, Provider: p, Subject: cl.Subject, Email: email, TenantID: cl.TenantID, LinkedAt: now, CreatedAt: now}
		if err := h.DB.Create(&id).Error; err != nil {
			h.toAppError(c, "server_error", p, "")
			return
		}
		h.audit(c, u.OrganizationID, u.ID, "auth.sso_link", u.ID, gin.H{"provider": p, "email": email})
	}
	v := url.Values{"linked": {p}}
	if email != "" {
		v.Set("email", email)
	}
	v.Set("redirect", firstNonEmpty(f.Redirect, "/profile"))
	h.toApp(c, v)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// Outcome of resolving a sign-in to a user.
var (
	errNoAccount = errors.New("no_account")
	errUnverifed = errors.New("email_unverified")
	errDisabled  = errors.New("account_disabled")
	errSuspended = errors.New("org_suspended")
	errLimit     = errors.New("user_limit")
)

// Resolve finds (or, under a domain rule, creates) the user for verified
// claims. joined reports a new account.
func (h *Handler) Resolve(provider string, cl *Claims) (u *models.User, email string, joined bool, err error) {
	email = VerifiedEmail(provider, cl)
	var user models.User
	found := false
	if id := h.identityBySub(provider, cl.Subject); id != nil {
		found = h.DB.Where("id = ?", id.UserID).Take(&user).Error == nil
	}
	if !found {
		if email == "" {
			return nil, "", false, errUnverifed
		}
		found = h.DB.Where("LOWER(email) = ?", email).Order("created_at").Take(&user).Error == nil
	}
	if !found {
		at := strings.LastIndex(email, "@")
		var rule DomainRule
		if at < 0 || h.DB.Where("domain = ?", email[at+1:]).Take(&rule).Error != nil {
			return nil, email, false, errNoAccount
		}
		if h.orgBlocked(rule.OrganizationID) {
			return nil, email, false, errSuspended
		}
		if h.UserLimit != nil {
			if msg := h.UserLimit(rule.OrganizationID); msg != "" {
				return nil, email, false, errLimit
			}
		}
		pw, _ := bcrypt.GenerateFromPassword([]byte(randToken(32)), bcrypt.DefaultCost)
		first, last := strings.TrimSpace(cl.GivenName), strings.TrimSpace(cl.FamilyName)
		if first == "" && last == "" {
			parts := strings.Fields(cl.Name)
			if len(parts) > 0 {
				first, last = parts[0], strings.Join(parts[1:], " ")
			}
		}
		if first == "" {
			first = email[:at]
		}
		user = models.User{ID: uuid.NewString(), Email: email, PasswordHash: string(pw), FirstName: cut(first, 100), LastName: cut(last, 100),
			Role: rule.Role, OrganizationID: rule.OrganizationID, IsActive: true}
		if err := h.DB.Omit("Organization", "Shifts", "UploadedDocs", "UserPermissions", "RefreshTokens").Create(&user).Error; err != nil {
			return nil, email, false, err
		}
		joined = true
	}
	if !user.IsActive {
		return nil, email, false, errDisabled
	}
	if h.orgBlocked(user.OrganizationID) {
		return nil, email, false, errSuspended
	}
	return &user, email, joined, nil
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func (h *Handler) finishLogin(c *gin.Context, f *Flow, cl *Claims) {
	p := f.Provider
	u, email, joined, err := h.Resolve(p, cl)
	if err != nil {
		code := err.Error()
		switch err {
		case errNoAccount, errUnverifed, errDisabled, errSuspended, errLimit:
		default:
			code = "server_error"
		}
		h.toAppError(c, code, p, email)
		return
	}
	now := h.now()
	// Link by subject so later sign-ins don't depend on the email.
	if id := h.identityBySub(p, cl.Subject); id == nil {
		h.DB.Create(&Identity{ID: uuid.NewString(), UserID: u.ID, Provider: p, Subject: cl.Subject, Email: email, TenantID: cl.TenantID, LinkedAt: now, LastUsed: &now, CreatedAt: now})
	} else {
		upd := map[string]interface{}{"last_used": now, "user_id": u.ID}
		if email != "" {
			upd["email"] = email
		}
		h.DB.Model(&Identity{}).Where("id = ?", id.ID).Updates(upd)
	}
	code := randToken(32)
	if err := h.DB.Create(&Code{Hash: hashCode(code), UserID: u.ID, Provider: p, Redirect: f.Redirect, ExpiresAt: now.Add(codeTTL), CreatedAt: now}).Error; err != nil {
		h.toAppError(c, "server_error", p, "")
		return
	}
	h.DB.Where("expires_at < ?", now.Add(-time.Hour)).Delete(&Code{})
	if joined {
		h.audit(c, u.OrganizationID, u.ID, "auth.sso_join", u.ID, gin.H{"provider": p, "email": email, "role": u.Role})
	}
	h.audit(c, u.OrganizationID, u.ID, "auth.sso_login", u.ID, gin.H{"provider": p, "email": email})
	v := url.Values{"code": {code}}
	if joined {
		v.Set("joined", "1")
	}
	h.toApp(c, v)
}

// Exchange swaps a one-time code for a session.
func (h *Handler) Exchange(c *gin.Context) {
	var in struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Code) == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Missing sign-in code")
		return
	}
	now := h.now()
	var rows []Code
	res := h.DB.Model(&rows).Clauses(clause.Returning{}).Where("hash = ? AND used_at IS NULL AND expires_at > ?", hashCode(strings.TrimSpace(in.Code)), now).Update("used_at", now)
	if res.Error != nil || res.RowsAffected != 1 || len(rows) != 1 {
		fail(c, http.StatusUnauthorized, "INVALID_CODE", "This sign-in link has expired or was already used. Please sign in again.")
		return
	}
	var u models.User
	if h.DB.Where("id = ? AND is_active = ?", rows[0].UserID, true).Take(&u).Error != nil || h.orgBlocked(u.OrganizationID) {
		fail(c, http.StatusUnauthorized, "ACCOUNT_DISABLED", "This account is disabled.")
		return
	}
	if h.Issue == nil {
		fail(c, http.StatusInternalServerError, "NOT_CONFIGURED", "Sign-in is not available")
		return
	}
	sess, err := h.Issue(c, &u)
	if err != nil {
		fail(c, http.StatusInternalServerError, "TOKEN_GENERATION_ERROR", "Failed to sign in")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": sess, "redirect": rows[0].Redirect, "provider": rows[0].Provider, "message": "Login successful"})
}

// ---------------------------------------------------------------- signed in

func (h *Handler) superAdmin(c *gin.Context) {
	if c.GetString("user_role") != "super_admin" {
		fail(c, http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Super administrator access required")
	}
}

func (h *Handler) orgAdmin(c *gin.Context) {
	if r := c.GetString("user_role"); r != "admin" && r != "super_admin" {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only administrators can change sign-in settings")
	}
}

// ListIdentities returns the signed-in user's linked accounts.
func (h *Handler) ListIdentities(c *gin.Context) {
	uid := c.GetString("user_id")
	var ids []Identity
	h.DB.Where("user_id = ?", uid).Order("linked_at").Find(&ids)
	if ids == nil {
		ids = []Identity{}
	}
	provs := []gin.H{}
	for _, p := range Providers {
		s := Load(h.DB, p)
		provs = append(provs, gin.H{"id": p, "name": s.Name, "enabled": s.Usable()})
	}
	ok(c, gin.H{"identities": ids, "providers": provs})
}

// StartLink prepares a linking flow; the app navigates to the returned url.
func (h *Handler) StartLink(c *gin.Context) {
	p := c.Param("provider")
	if !knownProvider(p) {
		fail(c, http.StatusNotFound, "UNKNOWN_PROVIDER", "Unknown sign-in provider")
		return
	}
	if !Load(h.DB, p).Usable() {
		fail(c, http.StatusConflict, "PROVIDER_DISABLED", providerNames[p]+" sign-in is not enabled")
		return
	}
	var in struct {
		Redirect string `json:"redirect"`
	}
	_ = c.ShouldBindJSON(&in)
	now := h.now()
	ticket := randToken(32)
	f := Flow{State: randToken(32), Ticket: hashCode(ticket), Provider: p, Nonce: randToken(24), Verifier: randToken(48),
		Redirect: firstNonEmpty(safeRedirect(in.Redirect), "/profile"), Mode: "link", UserID: c.GetString("user_id"), ExpiresAt: now.Add(flowTTL), CreatedAt: now}
	if err := h.DB.Create(&f).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not start linking")
		return
	}
	ok(c, gin.H{"url": h.apiBase(c) + "/api/v1/public/sso/" + p + "/start?ticket=" + url.QueryEscape(ticket)})
}

// Unlink removes one of the signed-in user's linked accounts.
func (h *Handler) Unlink(c *gin.Context) {
	uid := c.GetString("user_id")
	var id Identity
	if h.DB.Where("id = ? AND user_id = ?", c.Param("id"), uid).Take(&id).Error != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Linked account not found")
		return
	}
	if err := h.DB.Delete(&id).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not unlink")
		return
	}
	h.audit(c, c.GetString("org_id"), uid, "auth.sso_unlink", uid, gin.H{"provider": id.Provider, "email": id.Email})
	ok(c, gin.H{"id": id.ID})
}

// ListDomains returns the organisation's auto-join domains.
func (h *Handler) ListDomains(c *gin.Context) {
	org := c.GetString("org_id")
	var rules []DomainRule
	h.DB.Where("organization_id = ?", org).Order("domain").Find(&rules)
	if rules == nil {
		rules = []DomainRule{}
	}
	anyOn := false
	for _, p := range Providers {
		anyOn = anyOn || Load(h.DB, p).Usable()
	}
	var me models.User
	h.DB.Select("email").Where("id = ?", c.GetString("user_id")).Take(&me)
	own := ""
	if at := strings.LastIndex(me.Email, "@"); at >= 0 {
		own = strings.ToLower(me.Email[at+1:])
	}
	ok(c, gin.H{"rules": rules, "roles": JoinRoles, "sso_enabled": anyOn, "own_domain": own, "can_edit": c.GetString("user_role") == "admin" || c.GetString("user_role") == "super_admin"})
}

// AddDomain lets anyone with a verified address at a domain join as a role.
// Organisation admins can only add their own email domain (so nobody can
// claim someone else's company); super admins can add any.
func (h *Handler) AddDomain(c *gin.Context) {
	org := c.GetString("org_id")
	var in struct {
		Domain string `json:"domain"`
		Role   string `json:"role"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	d := NormDomain(in.Domain)
	if msg := DomainProblem(d); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	roleOK := false
	for _, r := range JoinRoles {
		roleOK = roleOK || r == in.Role
	}
	if !roleOK {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose the role new people get")
		return
	}
	if c.GetString("user_role") != "super_admin" {
		var me models.User
		h.DB.Select("email").Where("id = ?", c.GetString("user_id")).Take(&me)
		if !strings.HasSuffix(strings.ToLower(me.Email), "@"+d) {
			fail(c, http.StatusForbidden, "DOMAIN_NOT_YOURS", "You can only add your own email domain ("+d+" isn't). Ask DASYIN support to add another domain.")
			return
		}
	}
	var ex DomainRule
	if h.DB.Where("domain = ?", d).Take(&ex).Error == nil {
		if ex.OrganizationID == org {
			fail(c, http.StatusConflict, "DUPLICATE", d+" is already on the list")
		} else {
			fail(c, http.StatusConflict, "DOMAIN_TAKEN", d+" is already used by another organisation")
		}
		return
	}
	r := DomainRule{ID: uuid.NewString(), OrganizationID: org, Domain: d, Role: in.Role, CreatedBy: c.GetString("user_id"), CreatedAt: h.now()}
	if err := h.DB.Create(&r).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not save the domain")
		return
	}
	h.audit(c, org, c.GetString("user_id"), "auth.sso_domain_add", r.ID, gin.H{"domain": d, "role": in.Role})
	ok(c, r)
}

// DeleteDomain removes an auto-join domain.
func (h *Handler) DeleteDomain(c *gin.Context) {
	org := c.GetString("org_id")
	var r DomainRule
	if h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), org).Take(&r).Error != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Domain not found")
		return
	}
	h.DB.Delete(&r)
	h.audit(c, org, c.GetString("user_id"), "auth.sso_domain_remove", r.ID, gin.H{"domain": r.Domain})
	ok(c, gin.H{"id": r.ID})
}

// ---------------------------------------------------------------- super admin

func (h *Handler) providerView(c *gin.Context, p string) gin.H {
	s := Load(h.DB, p)
	var row ProviderConfig
	saved := h.DB.Where("provider = ?", p).Take(&row).Error == nil
	v := gin.H{
		"provider": p, "name": s.Name, "enabled": s.Enabled, "usable": s.Usable(), "configured": s.Configured(), "saved": saved,
		"client_id": s.ClientID, "client_id_from": s.ClientIDFrom, "secret_set": s.ClientSecret != "", "secret_from": s.ClientSecretFrom,
		"redirect_uri": h.RedirectURI(c, p), "test_issuer": s.TestIssuer,
	}
	if p == Microsoft {
		v["tenant"] = s.Tenant
	}
	return v
}

// AdminProviders shows the sign-in provider settings and redirect URIs.
func (h *Handler) AdminProviders(c *gin.Context) {
	out := []gin.H{}
	for _, p := range Providers {
		out = append(out, h.providerView(c, p))
	}
	ok(c, gin.H{"providers": out, "public_api_url": strings.TrimSpace(h.PublicAPI), "public_app_url": strings.TrimSpace(h.PublicApp)})
}

// SaveProvider stores a provider's settings (blank secret keeps the saved one).
func (h *Handler) SaveProvider(c *gin.Context) {
	p := c.Param("provider")
	if !knownProvider(p) {
		fail(c, http.StatusNotFound, "UNKNOWN_PROVIDER", "Unknown sign-in provider")
		return
	}
	var in struct {
		Enabled      bool    `json:"enabled"`
		ClientID     *string `json:"client_id"`
		ClientSecret string  `json:"client_secret"`
		ClearSecret  bool    `json:"clear_secret"`
		Tenant       *string `json:"tenant"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	var row ProviderConfig
	if h.DB.Where("provider = ?", p).Take(&row).Error != nil {
		row = ProviderConfig{Provider: p}
	}
	if in.ClientID != nil {
		row.ClientID = strings.TrimSpace(*in.ClientID)
	}
	if in.ClearSecret {
		row.ClientSecret = ""
	}
	if sec := strings.TrimSpace(in.ClientSecret); sec != "" {
		enc, err := mail.EncryptSecret(sec)
		if err != nil {
			fail(c, http.StatusInternalServerError, "ENCRYPTION_ERROR", "Could not store the secret")
			return
		}
		row.ClientSecret = enc
	}
	if in.Tenant != nil && p == Microsoft {
		t := strings.TrimSpace(*in.Tenant)
		if t != "" && !ValidTenant(t) {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Tenant must be common, organizations, consumers, a tenant ID or a domain")
			return
		}
		row.Tenant = t
	}
	row.Enabled = in.Enabled
	row.UpdatedBy = c.GetString("user_id")
	row.UpdatedAt = h.now()
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		if s := Load(tx, p); s.Enabled && !s.Configured() {
			return errors.New("not configured")
		}
		return nil
	})
	if err != nil {
		if err.Error() == "not configured" {
			fail(c, http.StatusBadRequest, "NOT_CONFIGURED", "Enter the client ID and client secret before enabling "+providerNames[p]+" sign-in")
			return
		}
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not save")
		return
	}
	h.audit(c, c.GetString("org_id"), c.GetString("user_id"), "platform.sso_provider_update", p, gin.H{"provider": p, "enabled": in.Enabled, "client_id": row.ClientID, "secret_changed": in.ClientSecret != "" || in.ClearSecret})
	ok(c, h.providerView(c, p))
}
