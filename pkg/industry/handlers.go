package industry

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/plans"
)

// Handler serves /api/v1/industry/*.
type Handler struct {
	Svc *Service
}

func fail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func who(c *gin.Context) (org, user, role string) {
	return c.GetString("org_id"), fmt.Sprint(c.MustGet("user_id")), c.GetString("user_role")
}

func isAdmin(role string) bool { return role == "admin" || role == "super_admin" || role == "owner" }

func requireAdmin(c *gin.Context) bool {
	if !isAdmin(c.GetString("user_role")) {
		fail(c, http.StatusForbidden, "ADMIN_REQUIRED", "Only organisation admins can change the business type and roles")
		return false
	}
	return true
}

// RegisterRoutes mounts the routes on the authenticated /api/v1 group.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/industry")
	g.GET("/me", h.me)
	g.GET("/types", h.listTypes)
	g.GET("/types/:key", h.getType)
	g.PUT("/types/:key", h.updateType)
	g.POST("/types", h.createType)
	g.POST("/types/:key/reset", h.resetType)
	g.PUT("/org/type", h.setType)
	g.PUT("/org/extras", h.setExtras)
	g.PUT("/org/setup", h.setSetup)
	g.GET("/roles", h.listRoles)
	g.POST("/roles", h.createRole)
	g.PUT("/roles/:id", h.updateRole)
	g.DELETE("/roles/:id", h.deleteRole)
	g.GET("/assignments", h.listAssignments)
	g.PUT("/assignments/:user_id", h.assign)
}

// ModuleKeys are the sellable module keys (no core).
func ModuleKeys() []string {
	out := []string{}
	for _, m := range plans.DefaultModules() {
		if !m.Core {
			out = append(out, m.Key)
		}
	}
	return out
}

func roleView(r *OrgRole) gin.H {
	if r == nil {
		return nil
	}
	return gin.H{"id": r.ID, "key": r.Key, "name": r.Name, "description": r.Description, "base": r.Base, "scope": r.Scope, "home": r.Home, "custom": r.Custom}
}

func typeView(t BusinessType) gin.H {
	return gin.H{
		"key": t.Key, "name": t.Name, "icon": t.Icon, "description": t.Description, "plan_industry": t.PlanIndustry,
		"modules": t.Modules, "hidden_paths": t.HiddenPaths, "terms": Terms(t), "features": t.Features,
		"booking_defaults": t.BookingDefaults, "dashboard_widgets": t.DashboardWidgets, "checklist": t.Checklist,
		"sample_services": t.SampleServices, "roles": t.Roles, "active": t.Active, "sort_order": t.SortOrder,
	}
}

// me: everything the frontend needs to tailor itself to the signed-in user.
func (h *Handler) me(c *gin.Context) {
	org, user, role := who(c)
	if org == "" {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Organisation not found in token")
		return
	}
	a, err := h.Svc.AccessFor(org, user, role)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not load your business type")
		return
	}
	st, _ := h.Svc.Settings(org)
	t := h.Svc.Type(st.TypeKey)
	mods := ModuleKeys()
	planMods := []string{}
	for _, m := range mods {
		if a.InPlan(m) {
			planMods = append(planMods, m)
		}
	}
	typeMods := []string{}
	for _, m := range mods {
		if a.TypeShows(m) {
			typeMods = append(typeMods, m)
		}
	}
	admin := isAdmin(role)
	terms := DefaultTerms()
	if st.Confirmed {
		terms = Terms(t)
	}
	hidden := a.HiddenPath
	if hidden == nil {
		hidden = []string{}
	}
	extras := st.ExtraModules
	if extras == nil {
		extras = []string{}
	}
	ok(c, http.StatusOK, gin.H{
		"type": gin.H{"key": t.Key, "name": t.Name, "icon": t.Icon, "description": t.Description, "features": t.Features,
			"booking_defaults": t.BookingDefaults, "dashboard_widgets": t.DashboardWidgets, "checklist": t.Checklist,
			"sample_services": t.SampleServices, "modules": t.Modules},
		"terms":             terms,
		"confirmed":         st.Confirmed,
		"restricted":        a.Restricted,
		"setup_status":      st.SetupStatus,
		"setup_step":        st.SetupStep,
		"needs_setup":       st.SetupStatus == SetupPending && admin,
		"show_banner":       !st.Confirmed && admin,
		"hidden_paths":      hidden,
		"extra_modules":     extras,
		"plan_modules":      planMods,
		"type_modules":      typeMods,
		"effective_modules": a.EffectiveModules(mods),
		"role":              roleView(a.Role),
		"permissions":       a.Permissions(mods),
		"scope":             a.Scope(),
		"super_admin":       a.SuperAdmin,
		"is_admin":          admin,
	})
}

func (h *Handler) listTypes(c *gin.Context) {
	types, err := h.Svc.Types()
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not load business types")
		return
	}
	all := c.GetString("user_role") == "super_admin" && c.Query("all") == "true"
	out := []gin.H{}
	for _, t := range types {
		if t.Active || all {
			out = append(out, typeView(t))
		}
	}
	ok(c, http.StatusOK, gin.H{"types": out, "modules": plans.DefaultModules(), "actions": AllActions})
}

func (h *Handler) getType(c *gin.Context) {
	key := normKey(c.Param("key"))
	if !h.Svc.typeExists(key) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Business type not found")
		return
	}
	ok(c, http.StatusOK, typeView(h.Svc.Type(key)))
}

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,39}$`)

func validActions(acts []string) bool {
	for _, a := range acts {
		if !contains(AllActions, a) {
			return false
		}
	}
	return true
}

func validBase(b string) bool {
	return b == BaseAdmin || b == BaseManager || b == BaseStaff || b == BaseReadOnly
}

func validPerms(p Perms) string {
	known := map[string]bool{AllModules: true, PermCore: true, PermTeam: true}
	for _, m := range ModuleKeys() {
		known[m] = true
	}
	for m, acts := range p {
		if !known[m] {
			return "Unknown module " + m
		}
		if !validActions(acts) {
			return "Unknown action for " + m
		}
	}
	return ""
}

// validateType checks a registry entry edited by a super admin.
func validateType(t *BusinessType) string {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return "Name is required"
	}
	known := map[string]bool{AllModules: true}
	for _, m := range ModuleKeys() {
		known[m] = true
	}
	for _, m := range t.Modules {
		if !known[m] {
			return "Unknown module " + m
		}
	}
	if len(t.Modules) == 0 {
		return "Choose at least one module"
	}
	hasOwner := false
	seen := map[string]bool{}
	for i := range t.Roles {
		r := &t.Roles[i]
		r.Key, r.Name = normKey(r.Key), strings.TrimSpace(r.Name)
		if !keyRe.MatchString(r.Key) || r.Name == "" {
			return "Every role needs a key (lower case) and a name"
		}
		if seen[r.Key] {
			return "Duplicate role key " + r.Key
		}
		seen[r.Key] = true
		if !validBase(r.Base) {
			return "Role " + r.Name + ": base must be admin, manager, staff or read_only"
		}
		if r.Scope == "" {
			r.Scope = ScopeAll
		}
		if r.Scope != ScopeAll && r.Scope != ScopeOwn {
			return "Role " + r.Name + ": scope must be all or own"
		}
		if msg := validPerms(r.Perms); msg != "" {
			return "Role " + r.Name + ": " + msg
		}
		if r.Key == "owner" && r.Base == BaseAdmin {
			hasOwner = true
		}
	}
	if !hasOwner {
		return "Every business type needs an \"owner\" role with admin base"
	}
	for i := range t.SampleServices {
		s := &t.SampleServices[i]
		if strings.TrimSpace(s.Name) == "" || s.Duration <= 0 || s.Duration > 1440 || s.Price < 0 {
			return "Sample services need a name, a duration (1–1440 min) and a price of zero or more"
		}
	}
	if t.Terms == nil {
		t.Terms = map[string]string{}
	}
	for _, list := range []*[]string{&t.HiddenPaths, &t.Features, &t.DashboardWidgets} {
		if *list == nil {
			*list = []string{}
		}
	}
	return ""
}

func (h *Handler) requireSuper(c *gin.Context) bool {
	if c.GetString("user_role") != "super_admin" {
		fail(c, http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Only platform super admins can edit the business type registry")
		return false
	}
	return true
}

func (h *Handler) updateType(c *gin.Context) {
	if !h.requireSuper(c) {
		return
	}
	key := normKey(c.Param("key"))
	if !h.Svc.typeExists(key) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Business type not found")
		return
	}
	var in BusinessType
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	in.Key = key
	if msg := validateType(&in); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	if key == "general" {
		in.Active = true
	}
	in.UpdatedAt = time.Now()
	if err := h.Svc.DB.Save(&in).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not save the business type")
		return
	}
	h.Svc.Invalidate("")
	ok(c, http.StatusOK, typeView(h.Svc.Type(key)))
}

func (h *Handler) createType(c *gin.Context) {
	if !h.requireSuper(c) {
		return
	}
	var in BusinessType
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	in.Key = normKey(in.Key)
	if !keyRe.MatchString(in.Key) {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Key must be 2–40 lower-case letters, digits, - or _")
		return
	}
	if h.Svc.typeExists(in.Key) {
		fail(c, http.StatusConflict, "TYPE_EXISTS", "A business type with this key already exists")
		return
	}
	if len(in.Roles) == 0 {
		in.Roles = h.Svc.Type("general").Roles
	}
	if msg := validateType(&in); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	in.Active = true
	if in.SortOrder == 0 {
		types, _ := h.Svc.Types()
		in.SortOrder = len(types) + 1
	}
	if err := h.Svc.DB.Create(&in).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not create the business type")
		return
	}
	h.Svc.Invalidate("")
	ok(c, http.StatusCreated, typeView(h.Svc.Type(in.Key)))
}

func (h *Handler) resetType(c *gin.Context) {
	if !h.requireSuper(c) {
		return
	}
	key := normKey(c.Param("key"))
	for _, t := range DefaultTypes() {
		if t.Key == key {
			t.UpdatedAt = time.Now()
			if err := h.Svc.DB.Save(&t).Error; err != nil {
				fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not reset the business type")
				return
			}
			h.Svc.Invalidate("")
			ok(c, http.StatusOK, typeView(h.Svc.Type(key)))
			return
		}
	}
	fail(c, http.StatusNotFound, "NOT_FOUND", "Only built-in business types can be reset")
}

func (h *Handler) setType(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	org, user, _ := who(c)
	var in struct {
		Type string `json:"type"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Type) == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose a business type")
		return
	}
	key := normKey(in.Type)
	if t := h.Svc.Type(key); t.Key != key || !t.Active {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose a business type from the list")
		return
	}
	st, err := h.Svc.SetType(org, key, user)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not change the business type: "+err.Error())
		return
	}
	ok(c, http.StatusOK, st)
}

func (h *Handler) setExtras(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	org, user, _ := who(c)
	var in struct {
		Modules []string `json:"modules"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	known := map[string]bool{}
	for _, m := range ModuleKeys() {
		known[m] = true
	}
	for _, m := range in.Modules {
		if !known[normKey(m)] {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Unknown module "+m)
			return
		}
	}
	st, err := h.Svc.SetExtras(org, in.Modules, user)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not save the extra modules")
		return
	}
	ok(c, http.StatusOK, st)
}

func (h *Handler) setSetup(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	org, user, _ := who(c)
	var in struct {
		Status string `json:"status"`
		Step   string `json:"step"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	st, err := h.Svc.SetSetup(org, in.Status, strings.TrimSpace(in.Step), user)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	ok(c, http.StatusOK, st)
}

// ---------------------------------------------------------------- roles

func (h *Handler) listRoles(c *gin.Context) {
	org, _, role := who(c)
	if !isAdmin(role) && role != "manager" {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only admins and managers can see roles")
		return
	}
	roles, err := h.Svc.Roles(org)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not load roles")
		return
	}
	// Member counts by resolved role (assignments + legacy mapping).
	var users []struct {
		ID   string
		Role string
	}
	h.Svc.DB.Table("users").Select("id, role").Where("organization_id = ? AND deleted_at IS NULL AND role <> ?", org, "super_admin").Scan(&users)
	counts := map[string]int{}
	for _, u := range users {
		if r, _ := h.Svc.RoleFor(org, u.ID, u.Role); r != nil {
			counts[r.ID]++
		}
	}
	out := []gin.H{}
	for _, r := range roles {
		perms := r.Perms
		if perms == nil {
			perms = Perms{}
		}
		out = append(out, gin.H{"id": r.ID, "key": r.Key, "type": r.TypeKey, "name": r.Name, "description": r.Description, "base": r.Base,
			"scope": r.Scope, "permissions": perms, "legacy": r.Legacy, "home": r.Home, "custom": r.Custom, "members": counts[r.ID],
			"locked": r.Key == "owner"})
	}
	ok(c, http.StatusOK, gin.H{"roles": out, "modules": ModuleKeys(), "actions": AllActions})
}

type roleInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Base        string `json:"base"`
	Scope       string `json:"scope"`
	Permissions Perms  `json:"permissions"`
	Home        string `json:"home"`
}

func (in *roleInput) validate() string {
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	if in.Name == "" || len(in.Name) > 80 {
		return "Role name is required (80 characters max)"
	}
	if !validBase(in.Base) {
		return "Choose a permission level"
	}
	if in.Scope == "" {
		in.Scope = ScopeAll
	}
	if in.Scope != ScopeAll && in.Scope != ScopeOwn {
		return "Choose what records the role can see"
	}
	if in.Permissions == nil {
		in.Permissions = Perms{}
	}
	if msg := validPerms(in.Permissions); msg != "" {
		return msg
	}
	if in.Home != "" && !strings.HasPrefix(in.Home, "/") {
		in.Home = ""
	}
	return ""
}

func (h *Handler) createRole(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	org, _, _ := who(c)
	var in roleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	if msg := in.validate(); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	roles, _ := h.Svc.Roles(org)
	for _, r := range roles {
		if strings.EqualFold(r.Name, in.Name) {
			fail(c, http.StatusConflict, "ROLE_EXISTS", "A role with this name already exists")
			return
		}
	}
	r := OrgRole{ID: uuid.NewString(), OrganizationID: org, Key: "custom-" + uuid.NewString()[:8], Name: in.Name, Description: in.Description,
		Base: in.Base, Scope: in.Scope, Perms: in.Permissions, Home: in.Home, Custom: true, SortOrder: 100 + len(roles)}
	if err := h.Svc.DB.Create(&r).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not create the role")
		return
	}
	h.Svc.Invalidate(org)
	ok(c, http.StatusCreated, r)
}

func (h *Handler) findRole(c *gin.Context) (*OrgRole, bool) {
	org, _, _ := who(c)
	var r OrgRole
	if err := h.Svc.DB.Where("id = ? AND organization_id = ?", c.Param("id"), org).First(&r).Error; err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Role not found")
		return nil, false
	}
	return &r, true
}

func (h *Handler) updateRole(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	r, found := h.findRole(c)
	if !found {
		return
	}
	var in roleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	if msg := in.validate(); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	if r.Key == "owner" && in.Base != BaseAdmin {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The owner role always has full access")
		return
	}
	if err := h.Svc.DB.Model(&OrgRole{}).Where("id = ?", r.ID).Updates(map[string]interface{}{
		"name": in.Name, "description": in.Description, "base": in.Base, "scope": in.Scope, "perms": mustJSON(in.Permissions),
		"home": in.Home, "updated_at": time.Now(),
	}).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not save the role")
		return
	}
	// Keep users.role in step with the (possibly new) base level.
	if in.Base != r.Base {
		var ids []string
		h.Svc.DB.Model(&UserRole{}).Where("role_id = ?", r.ID).Pluck("user_id", &ids)
		for _, id := range ids {
			var cur string
			h.Svc.DB.Table("users").Select("role").Where("id = ?", id).Scan(&cur)
			if cur != "super_admin" {
				h.Svc.DB.Table("users").Where("id = ?", id).Update("role", LegacyRoleForBase(in.Base, cur))
			}
		}
	}
	h.Svc.Invalidate(r.OrganizationID)
	var out OrgRole
	h.Svc.DB.Where("id = ?", r.ID).First(&out)
	ok(c, http.StatusOK, out)
}

func (h *Handler) deleteRole(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	r, found := h.findRole(c)
	if !found {
		return
	}
	if r.Key == "owner" {
		fail(c, http.StatusBadRequest, "ROLE_LOCKED", "The owner role cannot be deleted")
		return
	}
	var n int64
	h.Svc.DB.Model(&UserRole{}).Where("role_id = ?", r.ID).Count(&n)
	if n > 0 {
		fail(c, http.StatusConflict, "ROLE_IN_USE", fmt.Sprintf("%d team member(s) have this role. Give them another role first.", n))
		return
	}
	if err := h.Svc.DB.Where("id = ?", r.ID).Delete(&OrgRole{}).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not delete the role")
		return
	}
	h.Svc.Invalidate(r.OrganizationID)
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) listAssignments(c *gin.Context) {
	org, _, role := who(c)
	if !isAdmin(role) && role != "manager" {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only admins and managers can see role assignments")
		return
	}
	var users []struct {
		ID   string
		Role string
	}
	h.Svc.DB.Table("users").Select("id, role").Where("organization_id = ? AND deleted_at IS NULL", org).Scan(&users)
	stored, _ := h.Svc.Assignments(org)
	out := map[string]gin.H{}
	for _, u := range users {
		if u.Role == "super_admin" {
			continue
		}
		r, _ := h.Svc.RoleFor(org, u.ID, u.Role)
		if r == nil {
			continue
		}
		_, explicit := stored[u.ID]
		out[u.ID] = gin.H{"role_id": r.ID, "key": r.Key, "name": r.Name, "base": r.Base, "scope": r.Scope, "explicit": explicit && stored[u.ID] == r.ID}
	}
	ok(c, http.StatusOK, gin.H{"assignments": out})
}

func (h *Handler) assign(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	org, me, _ := who(c)
	target := c.Param("user_id")
	if target == me {
		fail(c, http.StatusBadRequest, "INVALID_OPERATION", "You cannot change your own role")
		return
	}
	var in struct {
		RoleID string `json:"role_id"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.RoleID == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose a role")
		return
	}
	r, legacy, err := h.Svc.Assign(org, target, in.RoleID, me)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"role": roleView(r), "user_role": legacy})
}
