package emailprefs

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/platform"
)

// Preview is who the next email of a category would go to.
type Preview struct {
	Recipients []string `json:"recipients"`
	// Enabled: the email is switched on at all (e.g. the night report and
	// the daily agenda are off until an admin turns them on).
	Enabled bool   `json:"enabled"`
	Note    string `json:"note,omitempty"`
}

// PreviewFunc resolves a category's recipients with the sender's own code.
type PreviewFunc func(org string) Preview

// Handler serves
//
//	GET  /email-recipients                 mode, categories, people (admins)
//	PUT  /email-recipients/policy          {mode}
//	PUT  /email-recipients/users/:id       {all_off?, overrides?{category: bool|null}}
//	GET  /email-recipients/preview?category=
//	GET  /me/email-preferences             the signed-in user's own switches
//	PUT  /me/email-preferences             {all_off?, categories?{category: bool}}
type Handler struct {
	DB *gorm.DB
	// Previews per category, wired to the senders (cmd/production). A
	// category without one previews the members who would get it.
	Previews map[string]PreviewFunc
}

// RegisterRoutes mounts the API on the authenticated group.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/email-recipients")
	g.GET("", h.list)
	g.GET("/preview", h.preview)
	g.PUT("/policy", h.setPolicy)
	g.PUT("/users/:id", h.setUser)
	auth.GET("/me/email-preferences", h.mine)
	auth.PUT("/me/email-preferences", h.saveMine)
}

func ok(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func orgOf(c *gin.Context) string {
	if v := c.GetString("organization_id"); v != "" {
		return v
	}
	return c.GetString("org_id")
}

// IsAdminRole: who may decide who gets emails (owners and admins; a super
// admin acting in an organisation).
func IsAdminRole(role string) bool { return ownerRole(strings.ToLower(role)) }

func (h *Handler) admin(c *gin.Context) (string, bool) {
	org := orgOf(c)
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "No organisation in the session")
		return "", false
	}
	if !IsAdminRole(c.GetString("user_role")) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only owners and admins can choose who gets emails")
		return "", false
	}
	return org, true
}

func (h *Handler) payload(org string) gin.H {
	people, pol := People(h.DB, org)
	off := 0
	for _, p := range people {
		if p.EmailsOff {
			off++
		}
	}
	return gin.H{"mode": pol.Mode, "mode_source": pol.Source, "business_type": BusinessType(h.DB, org), "modes": Modes, "categories": Categories,
		"people": people, "counts": gin.H{"people": len(people), "emails_off": off}, "can_edit": true}
}

func (h *Handler) list(c *gin.Context) {
	org, okk := h.admin(c)
	if !okk {
		return
	}
	ok(c, h.payload(org))
}

func (h *Handler) setPolicy(c *gin.Context) {
	org, okk := h.admin(c)
	if !okk {
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || !ValidMode(in.Mode) {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose everyone, owners and managers, or only owners")
		return
	}
	before := PolicyFor(h.DB, org)
	if _, err := SetMode(h.DB, org, in.Mode, c.GetString("user_id")); err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not save the email policy")
		return
	}
	if before.Mode != in.Mode || before.Source != "admin" {
		platform.Audit(h.DB, c, org, "email_recipients.mode", "organization", org, gin.H{"mode": before.Mode, "source": before.Source}, gin.H{"mode": in.Mode})
	}
	ok(c, h.payload(org))
}

func (h *Handler) setUser(c *gin.Context) {
	org, okk := h.admin(c)
	if !okk {
		return
	}
	uid := c.Param("id")
	before, member := PersonOf(h.DB, org, uid)
	if !member {
		fail(c, http.StatusNotFound, "NOT_FOUND", "That person is not a member of this organisation")
		return
	}
	var in struct {
		AllOff    *bool            `json:"all_off"`
		Overrides map[string]*bool `json:"overrides"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	for k := range in.Overrides {
		if _, known := CategoryByKey(k); !known {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Unknown email type: "+k)
			return
		}
	}
	if in.AllOff == nil && len(in.Overrides) == 0 {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Nothing to change")
		return
	}
	if err := SetAdmin(h.DB, org, uid, in.AllOff, in.Overrides, c.GetString("user_id")); err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not save")
		return
	}
	after, _ := PersonOf(h.DB, org, uid)
	platform.Audit(h.DB, c, org, "email_recipients.person", "user", uid,
		gin.H{"name": before.Name, "all_off": before.AllOff, "overrides": before.Overrides, "effective": before.Effective},
		gin.H{"name": after.Name, "all_off": after.AllOff, "overrides": after.Overrides, "effective": after.Effective})
	ok(c, gin.H{"person": after, "mode": PolicyFor(h.DB, org).Mode})
}

func (h *Handler) preview(c *gin.Context) {
	org, okk := h.admin(c)
	if !okk {
		return
	}
	key := c.Query("category")
	cat, known := CategoryByKey(key)
	if !known {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose an email type")
		return
	}
	var p Preview
	if f := h.Previews[key]; f != nil {
		p = f(org)
	} else {
		p = Preview{Enabled: true, Recipients: MembersFor(h.DB, org, key)}
	}
	if p.Recipients == nil {
		p.Recipients = []string{}
	}
	ok(c, gin.H{"category": cat.Key, "label": cat.Label, "mode": PolicyFor(h.DB, org).Mode, "recipients": p.Recipients, "enabled": p.Enabled, "note": p.Note})
}

// MembersFor is the addresses of the members who get a category when one of
// its emails concerns them (the preview of booking emails and the agenda).
func MembersFor(db *gorm.DB, org, cat string) []string {
	people, _ := People(db, org)
	out := []string{}
	for _, p := range people {
		if p.Effective[cat] && p.Email != "" {
			out = append(out, p.Email)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- my own

// MyCategory is one switch of My profile → Email notifications.
type MyCategory struct {
	Category
	On         bool   `json:"on"`      // they get it now
	SelfOn     bool   `json:"self_on"` // their own choice
	Locked     bool   `json:"locked"`  // an administrator (or the mode) keeps it off
	Reason     string `json:"reason"`
	ReasonText string `json:"reason_text"`
}

// Mine is a person's own email preferences.
func Mine(db *gorm.DB, org, userID string) gin.H {
	u, member := usersByID(db, []string{userID})[userID]
	if !member || org == "" || u.OrganizationID != org {
		return gin.H{"member": false, "categories": []MyCategory{}, "all_off": false, "all_locked": false}
	}
	pol := PolicyFor(db, org)
	p := toPerson(org, u)
	prefs := prefsOf(db, org, []string{userID})[userID]
	// What they would get with their own switches on: anything still off
	// is an administrator's decision.
	adminOnly := map[string]Pref{}
	for k, v := range prefs {
		v.SelfOff = false
		adminOnly[k] = v
	}
	out := []MyCategory{}
	for _, c := range Categories {
		d := decide(org, pol.Mode, p, c, prefs)
		if d.Reason == ReasonNotEligible {
			continue
		}
		a := decide(org, pol.Mode, p, c, adminOnly)
		mc := MyCategory{Category: c, On: d.On, SelfOn: !prefs[AllKey].SelfOff && !prefs[c.Key].SelfOff, Locked: !a.On, Reason: d.Reason}
		mc.ReasonText = myReason(d.Reason, a, pol.Mode)
		out = append(out, mc)
	}
	allLocked := prefs[AllKey].Admin != nil && !*prefs[AllKey].Admin
	return gin.H{"member": true, "mode": pol.Mode, "level": p.Level, "role_label": p.RoleLabel, "categories": out, "all_off": prefs[AllKey].SelfOff, "all_locked": allLocked}
}

func myReason(r string, admin Decision, mode string) string {
	if !admin.On {
		switch admin.Reason {
		case ReasonMode:
			if mode == ModeOwnersManagers {
				return "Turned off by your administrator — only owners and managers get emails"
			}
			return "Turned off by your administrator — only owners get emails"
		default:
			return "Turned off by your administrator"
		}
	}
	switch r {
	case ReasonSelfOff, ReasonSelfAllOff:
		return "You turned this off"
	case ReasonAdminOn:
		return "Switched on for you by your administrator"
	}
	return ""
}

func (h *Handler) mine(c *gin.Context) {
	uid := c.GetString("user_id")
	if uid == "" {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Not signed in")
		return
	}
	ok(c, Mine(h.DB, orgOf(c), uid))
}

func (h *Handler) saveMine(c *gin.Context) {
	uid, org := c.GetString("user_id"), orgOf(c)
	if uid == "" {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Not signed in")
		return
	}
	var in struct {
		AllOff     *bool           `json:"all_off"`
		Categories map[string]bool `json:"categories"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || (in.AllOff == nil && len(in.Categories) == 0) {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose which emails to turn on or off")
		return
	}
	cur := Mine(h.DB, org, uid)
	if cur["member"] != true {
		fail(c, http.StatusForbidden, "NOT_A_MEMBER", "You are not a member of this organisation, so it does not email you")
		return
	}
	mine := map[string]MyCategory{}
	for _, mc := range cur["categories"].([]MyCategory) {
		mine[mc.Key] = mc
	}
	for k, on := range in.Categories {
		mc, applies := mine[k]
		if !applies {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "This email is not sent to you: "+k)
			return
		}
		if on && mc.Locked {
			fail(c, http.StatusForbidden, "LOCKED_BY_ADMIN", "Turned off by your administrator — ask them to switch it on")
			return
		}
	}
	if err := SetSelf(h.DB, org, uid, in.AllOff, in.Categories); err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not save")
		return
	}
	after := Mine(h.DB, org, uid)
	platform.Audit(h.DB, c, org, "email_preferences.self", "user", uid, gin.H{"all_off": cur["all_off"]}, gin.H{"all_off": after["all_off"], "changed": in.Categories})
	ok(c, after)
}
