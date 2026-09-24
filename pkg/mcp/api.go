package mcp

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

// RegisterRoot mounts /mcp, the OAuth endpoints and the well-known metadata
// on the engine root.
func (s *Server) RegisterRoot(r gin.IRoutes) {
	r.POST("/mcp", s.HandleMCP)
	notAllowed := func(c *gin.Context) {
		s.setCORS(c)
		c.Header("Allow", "POST")
		c.JSON(http.StatusMethodNotAllowed, errResp(nil, codeInvalidRequest, "Use POST (this server does not offer an SSE stream)"))
	}
	r.GET("/mcp", notAllowed)
	r.DELETE("/mcp", notAllowed)
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		r.GET(p, s.ProtectedResourceMetadata)
	}
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/mcp", "/.well-known/openid-configuration"} {
		r.GET(p, s.AuthorizationServerMetadata)
	}
	r.POST("/oauth/register", s.Register)
	r.GET("/oauth/authorize", s.Authorize)
	r.POST("/oauth/authorize", s.Authorize)
	r.POST("/oauth/token", s.Token)
	r.POST("/oauth/revoke", s.Revoke)
}

// RegisterAPI mounts the signed-in user's settings API under /api/v1.
func (s *Server) RegisterAPI(auth *gin.RouterGroup) {
	g := auth.Group("/mcp", noAITokens)
	g.GET("/info", s.apiInfo)
	g.GET("/tools", s.apiTools)
	g.POST("/test", s.apiTest)
	g.GET("/tokens", s.apiListTokens)
	g.POST("/tokens", s.apiCreateToken)
	g.DELETE("/tokens/:id", s.apiRevokeToken)
	g.GET("/connections", s.apiListConnections)
	g.DELETE("/connections/:id", s.apiRevokeConnection)
	g.GET("/activity", s.apiActivity)
	o := auth.Group("/oauth", noAITokens)
	o.GET("/requests/:id", s.apiGetRequest)
	o.POST("/approve", s.apiApprove)
}

// StartCleanup runs Cleanup periodically until ctx is done.
func (s *Server) StartCleanup(ctx context.Context) {
	go func() {
		t := time.NewTicker(15 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Cleanup()
			}
		}
	}()
}

// noAITokens stops AI tools (which run with short-lived "src":"ai" JWTs)
// from managing MCP credentials.
func noAITokens(c *gin.Context) {
	h := c.GetHeader("Authorization")
	if len(h) > 7 {
		if tok, _, err := jwt.NewParser().ParseUnverified(h[7:], jwt.MapClaims{}); err == nil {
			if cl, ok := tok.Claims.(jwt.MapClaims); ok && cl["src"] == "ai" {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"success": false, "error": gin.H{"code": "FORBIDDEN", "message": "Not available to AI tools"}})
				return
			}
		}
	}
	c.Next()
}

func ctxStr(c *gin.Context, k string) string {
	if v, ok := c.Get(k); ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

func apiFail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func apiOK(c *gin.Context, status int, data any) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func isAdmin(role string) bool {
	switch role {
	case "admin", "super_admin", "owner":
		return true
	}
	return false
}

// principalFromSession builds a principal for the signed-in web user.
func (s *Server) principalFromSession(c *gin.Context) (Principal, bool) {
	id, err := s.userIdentity(ctxStr(c, "user_id"))
	if err != nil || id.OrgID != ctxStr(c, "org_id") {
		apiFail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Sign in again")
		return Principal{}, false
	}
	return Principal{Identity: id, Scope: ScopeReadWrite, Kind: "test", TokenID: "session:" + id.UserID, ClientName: "Connection test"}, true
}

func (s *Server) apiInfo(c *gin.Context) {
	all := aitools.All()
	read := 0
	for _, t := range all {
		if t.ReadOnly {
			read++
		}
	}
	apiOK(c, http.StatusOK, gin.H{
		"server_url":            ResourceURL(c.Request),
		"resource_metadata_url": ResourceMetadataURL(c.Request),
		"authorization_server":  BaseURL(c.Request),
		"protocol_version":      LatestProtocol,
		"server_name":           ServerName,
		"version":               Version(),
		"tool_count":            len(all),
		"read_tool_count":       read,
		"write_tool_count":      len(all) - read,
		"app_url":               aitools.AppURL(),
		"oauth_ready":           aitools.AppURL() != "",
		"rate_limit_per_minute": s.RatePerMinute,
	})
}

func (s *Server) apiTools(c *gin.Context) {
	out := []gin.H{}
	for _, t := range aitools.All() {
		out = append(out, gin.H{"name": t.Name, "title": t.Title, "description": t.Description, "read_only": t.ReadOnly,
			"destructive": t.Destructive, "module": t.Module, "area": aitools.Area(t)})
	}
	apiOK(c, http.StatusOK, gin.H{"tools": out})
}

// apiTest runs initialize + tools/list through the JSON-RPC dispatcher as the
// signed-in user and reports what a client would see.
func (s *Server) apiTest(c *gin.Context) {
	p, ok := s.principalFromSession(c)
	if !ok {
		return
	}
	started := time.Now()
	ini, _ := s.dispatch(nil, p, rpcRequest{JSONRPC: "2.0", ID: []byte("1"), Method: "initialize", Params: []byte(`{"protocolVersion":"` + LatestProtocol + `","capabilities":{},"clientInfo":{"name":"settings-test","version":"1"}}`)})
	list, _ := s.dispatch(nil, p, rpcRequest{JSONRPC: "2.0", ID: []byte("2"), Method: "tools/list"})
	if ini.Error != nil || list.Error != nil {
		apiFail(c, http.StatusInternalServerError, "MCP_TEST_FAILED", "The MCP server did not answer correctly")
		return
	}
	tools, _ := list.Result.(map[string]any)["tools"].([]map[string]any)
	read := 0
	for _, t := range tools {
		if a, ok := t["annotations"].(map[string]any); ok && a["readOnlyHint"] == true {
			read++
		}
	}
	res := ini.Result.(map[string]any)
	apiOK(c, http.StatusOK, gin.H{
		"ok": true, "protocol_version": res["protocolVersion"], "server": res["serverInfo"],
		"tool_count": len(tools), "read_tools": read, "write_tools": len(tools) - read,
		"duration_ms": time.Since(started).Milliseconds(), "server_url": ResourceURL(c.Request),
	})
}

type tokenView struct {
	Token
	Status string `json:"status"`
}

func (s *Server) tokenView(t Token) tokenView {
	st := "active"
	if t.RevokedAt != nil {
		st = "revoked"
	} else if t.ExpiresAt != nil && s.now().After(*t.ExpiresAt) {
		st = "expired"
	}
	return tokenView{Token: t, Status: st}
}

func (s *Server) apiListTokens(c *gin.Context) {
	var list []Token
	s.DB.Where("user_id = ? AND org_id = ?", ctxStr(c, "user_id"), ctxStr(c, "org_id")).Order("created_at DESC").Limit(200).Find(&list)
	out := make([]tokenView, 0, len(list))
	for _, t := range list {
		out = append(out, s.tokenView(t))
	}
	apiOK(c, http.StatusOK, gin.H{"tokens": out})
}

func (s *Server) apiCreateToken(c *gin.Context) {
	var in struct {
		Name          string `json:"name"`
		Scope         string `json:"scope"`
		ExpiresInDays *int   `json:"expires_in_days"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		apiFail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		apiFail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Give the token a name, e.g. “Claude Code on my laptop”")
		return
	}
	if len(name) > 120 {
		apiFail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Name is too long")
		return
	}
	scope := in.Scope
	if scope == "" {
		scope = ScopeRead
	}
	if scope != ScopeRead && scope != ScopeReadWrite {
		apiFail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Scope must be read or read_write")
		return
	}
	var exp *time.Time
	if in.ExpiresInDays != nil && *in.ExpiresInDays > 0 {
		if *in.ExpiresInDays > 3650 {
			apiFail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Expiry must be within 10 years")
			return
		}
		t := s.now().AddDate(0, 0, *in.ExpiresInDays)
		exp = &t
	}
	var n int64
	s.DB.Model(&Token{}).Where("user_id = ? AND revoked_at IS NULL", ctxStr(c, "user_id")).Count(&n)
	if n >= 50 {
		apiFail(c, http.StatusConflict, "TOO_MANY_TOKENS", "You have 50 active tokens; revoke some first")
		return
	}
	plain := NewPAT()
	t := Token{ID: uuid.NewString(), OrgID: ctxStr(c, "org_id"), UserID: ctxStr(c, "user_id"), Name: name, Prefix: plain[:10],
		Hash: HashToken(plain), Scope: scope, CreatedAt: s.now(), ExpiresAt: exp}
	if err := s.DB.Create(&t).Error; err != nil {
		apiFail(c, http.StatusInternalServerError, "SERVER_ERROR", "Could not create the token")
		return
	}
	apiOK(c, http.StatusCreated, gin.H{"token": s.tokenView(t), "secret": plain})
}

func (s *Server) apiRevokeToken(c *gin.Context) {
	res := s.DB.Model(&Token{}).Where("id = ? AND user_id = ? AND revoked_at IS NULL", c.Param("id"), ctxStr(c, "user_id")).Update("revoked_at", s.now())
	if res.RowsAffected == 0 {
		apiFail(c, http.StatusNotFound, "NOT_FOUND", "Token not found or already revoked")
		return
	}
	apiOK(c, http.StatusOK, gin.H{"revoked": true})
}

func (s *Server) apiListConnections(c *gin.Context) {
	var grants []OAuthGrant
	s.DB.Where("user_id = ? AND org_id = ? AND revoked_at IS NULL", ctxStr(c, "user_id"), ctxStr(c, "org_id")).Order("created_at DESC").Find(&grants)
	out := []gin.H{}
	for _, g := range grants {
		var cl OAuthClient
		s.DB.Where("id = ?", g.ClientID).First(&cl)
		host := ""
		if len(cl.RedirectURIs) > 0 {
			host = hostOf(cl.RedirectURIs[0])
		}
		out = append(out, gin.H{"id": g.ID, "client_id": g.ClientID, "client_name": cl.Name, "redirect_host": host, "scope": g.Scope,
			"created_at": g.CreatedAt, "last_used_at": g.LastUsedAt})
	}
	apiOK(c, http.StatusOK, gin.H{"connections": out})
}

func (s *Server) apiRevokeConnection(c *gin.Context) {
	var g OAuthGrant
	if err := s.DB.Where("id = ? AND user_id = ? AND revoked_at IS NULL", c.Param("id"), ctxStr(c, "user_id")).First(&g).Error; err != nil {
		apiFail(c, http.StatusNotFound, "NOT_FOUND", "Connection not found")
		return
	}
	s.revokeGrant(s.DB, g.ID)
	apiOK(c, http.StatusOK, gin.H{"revoked": true})
}

func (s *Server) apiActivity(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	q := s.DB.Model(&Activity{}).Where("org_id = ?", ctxStr(c, "org_id"))
	if !(c.Query("scope") == "org" && isAdmin(ctxStr(c, "user_role"))) {
		q = q.Where("user_id = ?", ctxStr(c, "user_id"))
	}
	if c.Query("writes_only") == "true" {
		q = q.Where("read_only = ?", false)
	}
	if t := c.Query("tool"); t != "" {
		q = q.Where("tool = ?", t)
	}
	var rows []Activity
	q.Order("created_at DESC").Limit(limit).Find(&rows)
	if rows == nil {
		rows = []Activity{}
	}
	apiOK(c, http.StatusOK, gin.H{"activity": rows, "can_view_org": isAdmin(ctxStr(c, "user_role"))})
}

func hostOf(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	return raw
}

func (s *Server) apiGetRequest(c *gin.Context) {
	req, cl, err := s.pendingRequest(c.Param("id"))
	if err != nil {
		apiFail(c, http.StatusNotFound, "REQUEST_EXPIRED", "This connection request has expired or was already used. Start again from the app you are connecting.")
		return
	}
	var org struct{ Name string }
	s.DB.Raw("SELECT name FROM organizations WHERE id = ?", ctxStr(c, "org_id")).Scan(&org)
	var user struct {
		Email     string
		FirstName string
		LastName  string
	}
	s.DB.Raw("SELECT email, first_name, last_name FROM users WHERE id = ?", ctxStr(c, "user_id")).Scan(&user)
	var existing int64
	s.DB.Model(&OAuthGrant{}).Where("client_id = ? AND user_id = ? AND revoked_at IS NULL", cl.ID, ctxStr(c, "user_id")).Count(&existing)
	apiOK(c, http.StatusOK, gin.H{
		"id":                req.ID,
		"client":            gin.H{"id": cl.ID, "name": cl.Name, "uri": cl.URI, "redirect_host": hostOf(req.RedirectURI)},
		"requested_scopes":  strings.Fields(req.Scope),
		"write_requested":   strings.Contains(req.Scope, OAuthScopeWrite),
		"organization":      gin.H{"id": ctxStr(c, "org_id"), "name": org.Name},
		"user":              gin.H{"email": user.Email, "name": strings.TrimSpace(user.FirstName + " " + user.LastName), "role": ctxStr(c, "user_role")},
		"expires_at":        req.ExpiresAt,
		"already_connected": existing > 0,
		"read_tool_count":   len(ToolDescriptors(false)),
		"total_tool_count":  len(ToolDescriptors(true)),
	})
}

func (s *Server) apiApprove(c *gin.Context) {
	var in struct {
		Request string `json:"request"`
		Allow   bool   `json:"allow"`
		Write   bool   `json:"write"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Request == "" {
		apiFail(c, http.StatusBadRequest, "VALIDATION_ERROR", "request is required")
		return
	}
	if _, err := s.userIdentity(ctxStr(c, "user_id")); err != nil {
		apiFail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Sign in again")
		return
	}
	redirect, err := s.approve(c.Request, in.Request, ctxStr(c, "user_id"), ctxStr(c, "org_id"), in.Allow, in.Write)
	if err != nil {
		apiFail(c, http.StatusNotFound, "REQUEST_EXPIRED", "This connection request has expired or was already used. Start again from the app you are connecting.")
		return
	}
	apiOK(c, http.StatusOK, gin.H{"redirect_url": redirect, "approved": in.Allow})
}
