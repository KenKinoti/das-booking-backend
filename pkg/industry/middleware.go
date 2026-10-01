package industry

import (
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/plans"
)

// Guard enforces the business type and the user's role on module APIs. It
// is plugged into plans.Enforcer.After so it runs right after the plan check:
//
//	402 MODULE_NOT_IN_PLAN      (plans)    the plan does not include the module
//	403 MODULE_HIDDEN_FOR_TYPE  (industry) the business type hides the module
//	403 ROLE_FORBIDDEN          (industry) the user's role lacks the action
//
// Roles with the "own" data scope only see and change bookings (job cards)
// assigned to them.
type Guard struct {
	Svc *Service
}

var bookingPath = regexp.MustCompile(`^/(?:booking/)?bookings(?:/([^/]+))?(?:/.*)?$`)

// Check implements plans.Enforcer.After.
func (g *Guard) Check(c *gin.Context, id plans.Identity) bool {
	if id.Role == "super_admin" || id.Org == "" {
		return true
	}
	a, err := g.Svc.AccessFor(id.Org, id.User, id.Role)
	if err != nil {
		log.Printf("industry: access for %s: %v", id.Org, err)
		return true // fail open, like the plan check
	}
	d := Decide(a, c.Request.Method, c.Request.URL.Path, plans.ModuleLabel)
	if !d.Allowed {
		c.AbortWithStatusJSON(d.Status, gin.H{"success": false, "error": gin.H{
			"code": d.Code, "module": d.Module, "action": d.Action, "message": d.Message,
		}, "message": d.Message})
		return false
	}
	if d.OwnScope {
		return g.ownScope(c, id)
	}
	return true
}

// ownScope limits booking requests to the caller's own records.
func (g *Guard) ownScope(c *gin.Context, id plans.Identity) bool {
	p := strings.TrimPrefix(c.Request.URL.Path, "/api/v1")
	m := bookingPath.FindStringSubmatch(strings.TrimSuffix(p, "/"))
	if m == nil {
		return true
	}
	rec := m[1]
	switch {
	case rec == "" && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead):
		// List: only bookings assigned to the caller.
		q := c.Request.URL.Query()
		q.Set("staff_id", id.User)
		c.Request.URL.RawQuery = q.Encode()
		return true
	case rec == "" || rec == "available-slots" || rec == "stats" || rec == "calendar":
		return true
	}
	var staff *string
	row := struct{ StaffID *string }{}
	if err := g.Svc.DB.Table("bookings").Select("staff_id").Where("id = ? AND organization_id = ?", rec, id.Org).Scan(&row).Error; err != nil {
		return true
	}
	staff = row.StaffID
	if staff == nil || *staff != id.User {
		msg := "You can only open the records assigned to you."
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"success": false, "error": gin.H{
			"code": "ROLE_FORBIDDEN", "module": "bookings", "scope": ScopeOwn, "message": msg,
		}, "message": msg})
		return false
	}
	return true
}
