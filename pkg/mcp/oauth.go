package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

// ---------------------------------------------------------------------------
// Metadata

// ProtectedResourceMetadata (RFC 9728).
func (s *Server) ProtectedResourceMetadata(c *gin.Context) {
	base := BaseURL(c.Request)
	c.Header("Access-Control-Allow-Origin", "*")
	c.JSON(http.StatusOK, gin.H{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"scopes_supported":         []string{OAuthScopeRead, OAuthScopeWrite},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "DASYIN ERP",
		"resource_documentation":   aitools.AppURL() + "/settings/ai",
	})
}

// AuthorizationServerMetadata (RFC 8414).
func (s *Server) AuthorizationServerMetadata(c *gin.Context) {
	base := BaseURL(c.Request)
	c.Header("Access-Control-Allow-Origin", "*")
	c.JSON(http.StatusOK, gin.H{
		"issuer":                                     base,
		"authorization_endpoint":                     base + "/oauth/authorize",
		"token_endpoint":                             base + "/oauth/token",
		"registration_endpoint":                      base + "/oauth/register",
		"revocation_endpoint":                        base + "/oauth/revoke",
		"response_types_supported":                   []string{"code"},
		"response_modes_supported":                   []string{"query"},
		"grant_types_supported":                      []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":           []string{"S256"},
		"token_endpoint_auth_methods_supported":      []string{"none"},
		"revocation_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                           []string{OAuthScopeRead, OAuthScopeWrite},
		"service_documentation":                      aitools.AppURL() + "/settings/ai",
	})
}

// ---------------------------------------------------------------------------
// Dynamic client registration (RFC 7591)

func oauthError(c *gin.Context, status int, code, desc string) {
	c.Header("Cache-Control", "no-store")
	c.AbortWithStatusJSON(status, gin.H{"error": code, "error_description": desc})
}

// validRedirect: https, or http on localhost/127.0.0.1/[::1]; no fragment.
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		h := strings.ToLower(u.Hostname())
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

// Register handles POST /oauth/register.
func (s *Server) Register(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	if ok, _ := s.regLimiter.take("reg:"+c.ClientIP(), 1, s.now()); !ok {
		oauthError(c, http.StatusTooManyRequests, "slow_down", "Too many client registrations from this address; try again later")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var in struct {
		ClientName              string   `json:"client_name"`
		ClientURI               string   `json:"client_uri"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		Scope                   string   `json:"scope"`
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&in); err != nil {
		oauthError(c, http.StatusBadRequest, "invalid_client_metadata", "Body must be JSON client metadata")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		oauthError(c, http.StatusBadRequest, "invalid_redirect_uri", "Provide 1-10 redirect_uris")
		return
	}
	for _, r := range in.RedirectURIs {
		if len(r) > 500 || !validRedirect(r) {
			oauthError(c, http.StatusBadRequest, "invalid_redirect_uri", "Redirect URIs must use https (or http://localhost / 127.0.0.1): "+r)
			return
		}
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(c, http.StatusBadRequest, "invalid_client_metadata", "Unsupported grant type "+g)
			return
		}
	}
	name := strings.TrimSpace(in.ClientName)
	if name == "" {
		name = "MCP client"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	uri := strings.TrimSpace(in.ClientURI)
	if len(uri) > 500 {
		uri = ""
	}
	cl := OAuthClient{ID: "mcpc_" + randomString(24), Name: name, URI: uri, RedirectURIs: in.RedirectURIs, RegisteredIP: c.ClientIP(), CreatedAt: s.now()}
	if err := s.DB.Create(&cl).Error; err != nil {
		oauthError(c, http.StatusInternalServerError, "server_error", "Could not register the client")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{
		"client_id":                  cl.ID,
		"client_id_issued_at":        cl.CreatedAt.Unix(),
		"client_name":                cl.Name,
		"client_uri":                 cl.URI,
		"redirect_uris":              cl.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      OAuthScopeRead + " " + OAuthScopeWrite,
	})
}

// ---------------------------------------------------------------------------
// Authorization endpoint

func errorPage(c *gin.Context, status int, title, msg string) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(status, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<style>body{font-family:system-ui,sans-serif;max-width:520px;margin:15vh auto;padding:0 16px;color:#1f2937}@media(prefers-color-scheme:dark){body{background:#0f172a;color:#e5e7eb}}h1{font-size:20px}</style></head>
<body><h1>%s</h1><p>%s</p></body></html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}

func redirectWith(raw string, params url.Values) string {
	u, _ := url.Parse(raw)
	q := u.Query()
	for k, v := range params {
		for _, x := range v {
			q.Add(k, x)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// normalizeScope keeps known scopes; empty means read + write requested.
func normalizeScope(raw string) string {
	var out []string
	for _, s := range strings.Fields(raw) {
		if s == OAuthScopeRead || s == OAuthScopeWrite {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return OAuthScopeRead + " " + OAuthScopeWrite
	}
	if !strings.Contains(strings.Join(out, " "), OAuthScopeRead) {
		out = append([]string{OAuthScopeRead}, out...)
	}
	return strings.Join(out, " ")
}

// Authorize handles GET /oauth/authorize: validates the request and sends the
// browser to the web app's consent page.
func (s *Server) Authorize(c *gin.Context) {
	q := c.Request.URL.Query()
	if c.Request.Method == http.MethodPost {
		_ = c.Request.ParseForm()
		q = c.Request.Form
	}
	var cl OAuthClient
	if err := s.DB.Where("id = ?", q.Get("client_id")).First(&cl).Error; err != nil {
		errorPage(c, http.StatusBadRequest, "Unknown application", "This connection request comes from an unknown application (client_id). Remove the connector and add it again.")
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(cl.RedirectURIs) == 1 {
		redirect = cl.RedirectURIs[0]
	}
	allowed := false
	for _, r := range cl.RedirectURIs {
		if r == redirect {
			allowed = true
		}
	}
	if !allowed {
		errorPage(c, http.StatusBadRequest, "Invalid redirect", "The redirect address does not match the one this application registered.")
		return
	}
	state := q.Get("state")
	fail := func(code, desc string) {
		p := url.Values{"error": {code}, "error_description": {desc}, "iss": {BaseURL(c.Request)}}
		if state != "" {
			p.Set("state", state)
		}
		c.Redirect(http.StatusFound, redirectWith(redirect, p))
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "response_type must be code")
		return
	}
	challenge := q.Get("code_challenge")
	if q.Get("code_challenge_method") != "S256" || len(challenge) < 43 || len(challenge) > 128 {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	resource := q.Get("resource")
	if resource != "" && !sameResource(resource, ResourceURL(c.Request)) {
		fail("invalid_target", "Unknown resource "+resource)
		return
	}
	if len(state) > 2000 {
		fail("invalid_request", "state is too long")
		return
	}
	app := aitools.AppURL()
	if app == "" {
		errorPage(c, http.StatusInternalServerError, "Not configured", "The server's PUBLIC_APP_URL is not set, so the sign-in page can't be shown. Ask your administrator to set it.")
		return
	}
	now := s.now()
	req := OAuthRequest{ID: randomString(32), ClientID: cl.ID, RedirectURI: redirect, State: state, CodeChallenge: challenge,
		Scope: normalizeScope(q.Get("scope")), Resource: resource, CreatedAt: now, ExpiresAt: now.Add(RequestTTL)}
	if err := s.DB.Create(&req).Error; err != nil {
		errorPage(c, http.StatusInternalServerError, "Something went wrong", "Could not start the connection. Please try again.")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, app+"/connect/authorize?request="+url.QueryEscape(req.ID))
}

// pendingRequest loads an unexpired, unconsumed authorization request.
func (s *Server) pendingRequest(id string) (*OAuthRequest, *OAuthClient, error) {
	var r OAuthRequest
	if err := s.DB.Where("id = ?", id).First(&r).Error; err != nil {
		return nil, nil, err
	}
	if r.ConsumedAt != nil || s.now().After(r.ExpiresAt) {
		return nil, nil, gorm.ErrRecordNotFound
	}
	var cl OAuthClient
	if err := s.DB.Where("id = ?", r.ClientID).First(&cl).Error; err != nil {
		return nil, nil, err
	}
	return &r, &cl, nil
}

// approve records the user's decision and returns where to send the browser.
func (s *Server) approve(r *http.Request, reqID string, userID, orgID string, allow, write bool) (string, error) {
	var redirect string
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var req OAuthRequest
		if err := tx.Where("id = ? AND consumed_at IS NULL", reqID).First(&req).Error; err != nil {
			return err
		}
		if s.now().After(req.ExpiresAt) {
			return gorm.ErrRecordNotFound
		}
		now := s.now()
		res := tx.Model(&OAuthRequest{}).Where("id = ? AND consumed_at IS NULL", req.ID).Update("consumed_at", now)
		if res.Error != nil || res.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		params := url.Values{"iss": {BaseURL(r)}}
		if req.State != "" {
			params.Set("state", req.State)
		}
		if !allow {
			params.Set("error", "access_denied")
			params.Set("error_description", "The user declined the connection")
			redirect = redirectWith(req.RedirectURI, params)
			return nil
		}
		scope := ScopeRead
		if write && strings.Contains(req.Scope, OAuthScopeWrite) {
			scope = ScopeReadWrite
		}
		var g OAuthGrant
		err := tx.Where("client_id = ? AND user_id = ? AND revoked_at IS NULL", req.ClientID, userID).First(&g).Error
		if err == nil {
			tx.Model(&OAuthGrant{}).Where("id = ?", g.ID).Updates(map[string]any{"scope": scope, "org_id": orgID, "resource": req.Resource})
		} else {
			g = OAuthGrant{ID: uuid.NewString(), ClientID: req.ClientID, OrgID: orgID, UserID: userID, Scope: scope, Resource: req.Resource, CreatedAt: now}
			if err := tx.Create(&g).Error; err != nil {
				return err
			}
		}
		code := "dsyc_" + randomString(40)
		if err := tx.Create(&OAuthCode{Hash: HashToken(code), ClientID: req.ClientID, GrantID: g.ID, RedirectURI: req.RedirectURI,
			CodeChallenge: req.CodeChallenge, Resource: req.Resource, ExpiresAt: now.Add(CodeTTL)}).Error; err != nil {
			return err
		}
		params.Set("code", code)
		redirect = redirectWith(req.RedirectURI, params)
		return nil
	})
	return redirect, err
}

// ---------------------------------------------------------------------------
// Token endpoint

func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func scopeString(scope string) string {
	if scope == ScopeReadWrite {
		return OAuthScopeRead + " " + OAuthScopeWrite
	}
	return OAuthScopeRead
}

// issue creates a new access + refresh token pair for a grant.
func (s *Server) issue(tx *gorm.DB, g OAuthGrant) (gin.H, error) {
	now := s.now()
	access := AccessPrefix + randomString(40)
	refresh := RefreshPrefix + randomString(48)
	if err := tx.Create(&OAuthToken{Hash: HashToken(access), GrantID: g.ID, Kind: "access", CreatedAt: now, ExpiresAt: now.Add(AccessTTL)}).Error; err != nil {
		return nil, err
	}
	if err := tx.Create(&OAuthToken{Hash: HashToken(refresh), GrantID: g.ID, Kind: "refresh", CreatedAt: now, ExpiresAt: now.Add(RefreshTTL)}).Error; err != nil {
		return nil, err
	}
	return gin.H{"access_token": access, "token_type": "Bearer", "expires_in": int(AccessTTL.Seconds()), "refresh_token": refresh, "scope": scopeString(g.Scope)}, nil
}

// revokeGrant revokes a grant and all its tokens.
func (s *Server) revokeGrant(tx *gorm.DB, grantID string) {
	now := s.now()
	tx.Model(&OAuthGrant{}).Where("id = ? AND revoked_at IS NULL", grantID).Update("revoked_at", now)
	tx.Model(&OAuthToken{}).Where("grant_id = ? AND revoked_at IS NULL", grantID).Update("revoked_at", now)
}

func formValue(c *gin.Context, body map[string]string, k string) string {
	if v, ok := body[k]; ok {
		return v
	}
	return c.Request.PostFormValue(k)
}

// Token handles POST /oauth/token.
func (s *Server) Token(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	body := map[string]string{}
	if strings.HasPrefix(c.ContentType(), "application/json") {
		var raw map[string]any
		if err := json.NewDecoder(c.Request.Body).Decode(&raw); err == nil {
			for k, v := range raw {
				if s, ok := v.(string); ok {
					body[k] = s
				}
			}
		}
	} else {
		_ = c.Request.ParseForm()
	}
	clientID := formValue(c, body, "client_id")
	if u, _, ok := c.Request.BasicAuth(); ok && clientID == "" {
		clientID = u
	}
	now := s.now()
	switch formValue(c, body, "grant_type") {
	case "authorization_code":
		code := formValue(c, body, "code")
		var out gin.H
		status, errCode, errDesc := 0, "", ""
		_ = s.DB.Transaction(func(tx *gorm.DB) error {
			var ac OAuthCode
			if err := tx.Where("hash = ?", HashToken(code)).First(&ac).Error; err != nil {
				status, errCode, errDesc = 400, "invalid_grant", "Unknown authorization code"
				return err
			}
			if ac.UsedAt != nil {
				// Code replay: revoke everything issued from it.
				s.revokeGrant(tx, ac.GrantID)
				status, errCode, errDesc = 400, "invalid_grant", "Authorization code already used"
				return nil
			}
			if now.After(ac.ExpiresAt) {
				status, errCode, errDesc = 400, "invalid_grant", "Authorization code expired"
				return nil
			}
			if clientID != "" && clientID != ac.ClientID {
				status, errCode, errDesc = 400, "invalid_grant", "Code was issued to another client"
				return nil
			}
			if ru := formValue(c, body, "redirect_uri"); ru != "" && ru != ac.RedirectURI {
				status, errCode, errDesc = 400, "invalid_grant", "redirect_uri does not match"
				return nil
			}
			if !pkceMatches(formValue(c, body, "code_verifier"), ac.CodeChallenge) {
				status, errCode, errDesc = 400, "invalid_grant", "PKCE verification failed"
				return nil
			}
			if res := formValue(c, body, "resource"); res != "" && !sameResource(res, ResourceURL(c.Request)) {
				status, errCode, errDesc = 400, "invalid_target", "Unknown resource"
				return nil
			}
			r := tx.Model(&OAuthCode{}).Where("hash = ? AND used_at IS NULL", ac.Hash).Update("used_at", now)
			if r.Error != nil || r.RowsAffected != 1 {
				status, errCode, errDesc = 400, "invalid_grant", "Authorization code already used"
				return nil
			}
			var g OAuthGrant
			if err := tx.Where("id = ? AND revoked_at IS NULL", ac.GrantID).First(&g).Error; err != nil {
				status, errCode, errDesc = 400, "invalid_grant", "The connection was revoked"
				return nil
			}
			var err error
			out, err = s.issue(tx, g)
			if err != nil {
				status, errCode, errDesc = 500, "server_error", "Could not issue tokens"
				return err
			}
			tx.Model(&OAuthClient{}).Where("id = ?", g.ClientID).Update("last_used_at", now)
			return nil
		})
		if errCode != "" {
			oauthError(c, status, errCode, errDesc)
			return
		}
		c.JSON(http.StatusOK, out)
	case "refresh_token":
		rt := formValue(c, body, "refresh_token")
		var out gin.H
		errCode, errDesc := "", ""
		_ = s.DB.Transaction(func(tx *gorm.DB) error {
			var t OAuthToken
			if err := tx.Where("hash = ? AND kind = ?", HashToken(rt), "refresh").First(&t).Error; err != nil {
				errCode, errDesc = "invalid_grant", "Unknown refresh token"
				return nil
			}
			if t.UsedAt != nil {
				// Refresh token reuse → assume theft, revoke the connection.
				s.revokeGrant(tx, t.GrantID)
				errCode, errDesc = "invalid_grant", "Refresh token already used; the connection was revoked for safety"
				return nil
			}
			if t.RevokedAt != nil || now.After(t.ExpiresAt) {
				errCode, errDesc = "invalid_grant", "Refresh token expired or revoked"
				return nil
			}
			var g OAuthGrant
			if err := tx.Where("id = ? AND revoked_at IS NULL", t.GrantID).First(&g).Error; err != nil {
				errCode, errDesc = "invalid_grant", "The connection was revoked"
				return nil
			}
			if clientID != "" && clientID != g.ClientID {
				errCode, errDesc = "invalid_grant", "Refresh token was issued to another client"
				return nil
			}
			r := tx.Model(&OAuthToken{}).Where("hash = ? AND used_at IS NULL", t.Hash).Update("used_at", now)
			if r.Error != nil || r.RowsAffected != 1 {
				errCode, errDesc = "invalid_grant", "Refresh token already used"
				return nil
			}
			// Optional down-scoping to read only.
			if sc := formValue(c, body, "scope"); sc != "" && !strings.Contains(sc, OAuthScopeWrite) && g.Scope == ScopeReadWrite {
				g.Scope = ScopeRead
				tx.Model(&OAuthGrant{}).Where("id = ?", g.ID).Update("scope", ScopeRead)
			}
			var err error
			out, err = s.issue(tx, g)
			if err != nil {
				errCode, errDesc = "server_error", "Could not issue tokens"
			}
			return err
		})
		if errCode != "" {
			status := http.StatusBadRequest
			if errCode == "server_error" {
				status = 500
			}
			oauthError(c, status, errCode, errDesc)
			return
		}
		c.JSON(http.StatusOK, out)
	default:
		oauthError(c, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

// Revoke handles POST /oauth/revoke (RFC 7009): always 200.
func (s *Server) Revoke(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	_ = c.Request.ParseForm()
	tok := c.Request.PostFormValue("token")
	if tok != "" {
		var t OAuthToken
		if err := s.DB.Where("hash = ?", HashToken(tok)).First(&t).Error; err == nil {
			if t.Kind == "refresh" {
				s.revokeGrant(s.DB, t.GrantID)
			} else {
				s.DB.Model(&OAuthToken{}).Where("hash = ?", t.Hash).Update("revoked_at", s.now())
			}
		}
	}
	c.Status(http.StatusOK)
}

// Cleanup deletes expired requests, codes and tokens.
func (s *Server) Cleanup() {
	now := s.now()
	s.DB.Where("expires_at < ?", now.Add(-time.Hour)).Delete(&OAuthRequest{})
	s.DB.Where("expires_at < ?", now.Add(-time.Hour)).Delete(&OAuthCode{})
	s.DB.Where("expires_at < ?", now.Add(-24*time.Hour)).Delete(&OAuthToken{})
	s.DB.Where("created_at < ?", now.AddDate(0, 0, -180)).Delete(&Activity{})
}
