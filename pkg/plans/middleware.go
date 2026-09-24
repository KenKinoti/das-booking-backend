package plans

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Rule maps an API path prefix (below /api/v1) to the modules that grant it.
// Any listed module is enough. ReadAlso lists extra modules that grant GET
// access (shared look-ups such as the product list used by POS and
// invoicing); ReadAny lets every organisation read it.
type Rule struct {
	Prefix   string
	Modules  []string
	ReadAlso []string
	ReadAny  bool
}

// Rules is the path → module map used by the enforcement middleware.
var Rules = []Rule{
	{Prefix: "/invoicing", Modules: []string{"invoicing"}},
	{Prefix: "/pos", Modules: []string{"pos"}},
	{Prefix: "/bookings", Modules: []string{"bookings"}},
	{Prefix: "/booking", Modules: []string{"bookings"}},
	{Prefix: "/services", Modules: []string{"bookings"}, ReadAlso: []string{"invoicing", "pos", "smallbiz"}},
	{Prefix: "/events", Modules: []string{"events"}},
	{Prefix: "/inventory", Modules: []string{"inventory"}, ReadAlso: []string{"pos", "invoicing", "ecommerce", "manufacturing", "smallbiz"}},
	{Prefix: "/suppliers", Modules: []string{"inventory"}, ReadAlso: []string{"accounting", "manufacturing"}},
	{Prefix: "/finance", Modules: []string{"accounting"}},
	{Prefix: "/crm", Modules: []string{"crm"}},
	{Prefix: "/customers", Modules: []string{"crm", "invoicing", "bookings", "pos", "events", "smallbiz", "ecommerce"}, ReadAny: true},
	{Prefix: "/meetings", Modules: []string{"communication"}},
	{Prefix: "/messaging", Modules: []string{"communication"}},
	{Prefix: "/video", Modules: []string{"communication"}},
	{Prefix: "/pm", Modules: []string{"projects"}},
	{Prefix: "/projects", Modules: []string{"projects"}},
	{Prefix: "/production", Modules: []string{"manufacturing"}},
	{Prefix: "/hcm", Modules: []string{"hr"}},
	{Prefix: "/ecommerce", Modules: []string{"ecommerce"}},
	{Prefix: "/smallbiz", Modules: []string{"smallbiz"}},
	{Prefix: "/documents", Modules: []string{"documents"}},
}

// MatchRule finds the rule for an API path (with or without /api/v1).
// Prefixes match whole path segments only (/pos matches /pos/x, not /posts).
func MatchRule(path string) (Rule, bool) {
	p := strings.TrimPrefix(path, "/api/v1")
	if p == "" {
		p = "/"
	}
	best := Rule{}
	found := false
	for _, r := range Rules {
		if p == r.Prefix || strings.HasPrefix(p, r.Prefix+"/") {
			if !found || len(r.Prefix) > len(best.Prefix) {
				best, found = r, true
			}
		}
	}
	return best, found
}

// Allowed decides whether an organisation with access a may call method+path.
// It returns the module to report when access is denied.
func Allowed(a Access, method, path string) (bool, string) {
	r, ok := MatchRule(path)
	if !ok {
		return true, ""
	}
	for _, m := range r.Modules {
		if a.Has(m) {
			return true, ""
		}
	}
	if method == http.MethodGet || method == http.MethodHead {
		if r.ReadAny {
			return true, ""
		}
		for _, m := range r.ReadAlso {
			if a.Has(m) {
				return true, ""
			}
		}
	}
	return false, r.Modules[0]
}

// Identity is who is calling.
type Identity struct {
	Org  string
	Role string
}

// TokenIdentity parses the bearer token with the JWT secret. It never
// rejects: the real auth middleware does that later.
func TokenIdentity(secret string) func(c *gin.Context) (Identity, bool) {
	return func(c *gin.Context) (Identity, bool) {
		if v, ok := c.Get("org_id"); ok && v != nil {
			role, _ := c.Get("user_role")
			return Identity{Org: fmt.Sprint(v), Role: fmt.Sprint(role)}, true
		}
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			return Identity{}, false
		}
		tok, err := jwt.Parse(strings.TrimPrefix(h, "Bearer "), func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(secret), nil
		})
		if err != nil || tok == nil || !tok.Valid {
			return Identity{}, false
		}
		claims, ok := tok.Claims.(jwt.MapClaims)
		if !ok {
			return Identity{}, false
		}
		org, _ := claims["org_id"].(string)
		role, _ := claims["role"].(string)
		return Identity{Org: org, Role: role}, org != ""
	}
}

// Enforcer blocks API calls to modules outside the organisation's plan.
type Enforcer struct {
	Lookup   func(org string) (Access, error)
	Identify func(c *gin.Context) (Identity, bool)
	// Users counts users for the max-users limit (optional).
	Users func(org string) int64
}

// Middleware returns the gin handler. It is safe to install at engine level:
// only /api/v1 paths covered by Rules (and POST /users) are checked.
func (e *Enforcer) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if !strings.HasPrefix(path, "/api/v1/") || strings.HasPrefix(path, "/api/v1/public/") || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		_, matched := MatchRule(path)
		userCreate := c.Request.Method == http.MethodPost && strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1"), "/") == "/users"
		if !matched && !userCreate {
			c.Next()
			return
		}
		id, ok := e.Identify(c)
		if !ok || id.Role == "super_admin" {
			c.Next()
			return
		}
		a, err := e.Lookup(id.Org)
		if err != nil {
			// Fail open: never lock customers out because of a plans outage.
			log.Printf("plans: entitlement lookup for %s: %v", id.Org, err)
			c.Next()
			return
		}
		if matched {
			if allowed, mod := Allowed(a, c.Request.Method, path); !allowed {
				c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"success": false, "error": gin.H{
					"code": "MODULE_NOT_IN_PLAN", "module": mod,
					"message": fmt.Sprintf("Your plan does not include the %s module. Upgrade your plan to use it.", ModuleLabel(mod)),
				}})
				return
			}
		}
		if userCreate && a.MaxUsers > 0 && e.Users != nil {
			if n := e.Users(id.Org); n >= int64(a.MaxUsers) {
				c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"success": false, "error": gin.H{
					"code": "USER_LIMIT_REACHED", "limit": a.MaxUsers,
					"message": fmt.Sprintf("Your plan allows up to %d users. Upgrade your plan to add more.", a.MaxUsers),
				}})
				return
			}
		}
		c.Next()
	}
}

// ModuleLabel is a readable module name for messages.
func ModuleLabel(k string) string {
	for _, m := range DefaultModules() {
		if m.Key == k {
			return m.Name
		}
	}
	return k
}
