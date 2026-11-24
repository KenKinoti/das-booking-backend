package outbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Handler serves /api/v1/outbox.
type Handler struct {
	S *Service
}

// RegisterRoutes mounts the API on the signed-in group.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/outbox")
	g.GET("/items", h.list)
	g.GET("/counts", h.counts)
	g.GET("/items/:id", h.get)
	g.PUT("/items/:id", h.update)
	g.POST("/items/:id/preview", h.preview)
	g.POST("/items/:id/approve", h.approve)
	g.POST("/items/:id/reject", h.reject)
	g.POST("/items/:id/retry", h.retry)
	g.POST("/bulk", h.bulk)
	g.GET("/settings", h.getSettings)
	g.PUT("/settings", h.putSettings)
	if h.S.TestClock {
		g.POST("/test/clock", h.testClock)
		g.POST("/test/run", h.testRun)
	}
}

func str(c *gin.Context, key string) string {
	v, ok := c.Get(key)
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func (h *Handler) actor(c *gin.Context) (Actor, bool) {
	a := Actor{Org: str(c, "org_id"), User: str(c, "user_id"), Role: str(c, "user_role")}
	if a.Org == "" || a.User == "" {
		fail(c, &Error{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Msg: "Please sign in again."})
		return a, false
	}
	return a, true
}

func ok(c *gin.Context, data any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": data}) }

func fail(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		body := gin.H{"code": e.Code, "message": e.Msg}
		if e.Field != "" {
			body["field"] = e.Field
		}
		c.JSON(e.Status, gin.H{"success": false, "error": body, "message": e.Msg})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"code": "SERVER_ERROR", "message": "Something went wrong: " + err.Error()},
		"message": "Something went wrong: " + err.Error()})
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (h *Handler) list(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	q := Query{Tab: c.Query("tab"), Status: c.Query("status"), Kind: c.Query("kind"), Channel: c.Query("channel"), CustomerID: c.Query("customer_id"),
		CreatedBy: c.Query("created_by"), TriggerKind: c.Query("trigger_kind"), Flag: c.Query("flag"), Q: c.Query("q"), Sort: c.Query("sort"),
		Page: atoi(c.Query("page")), PerPage: atoi(c.Query("per_page"))}
	if q.Status != "" {
		switch q.Status {
		case StatusPending, StatusApproved, StatusSending, StatusSent, StatusRejected, StatusSuperseded, StatusFailed:
		default:
			fail(c, field("VALIDATION_ERROR", "status", "Unknown status"))
			return
		}
	}
	rows, total, filters := h.S.List(a, q)
	page, per := q.Page, q.PerPage
	if page <= 0 {
		page = 1
	}
	if per <= 0 {
		per = 50
	}
	if per > 200 {
		per = 200
	}
	ok(c, gin.H{"items": rows, "total": total, "page": page, "per_page": per, "counts": h.S.CountsFor(a), "filters": filters})
}

func (h *Handler) counts(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	ok(c, h.S.CountsFor(a))
}

func (h *Handler) one(c *gin.Context, a Actor) (*Item, bool, bool) {
	it, err := h.S.load(a.Org, c.Param("id"))
	if err != nil {
		fail(c, err)
		return nil, false, false
	}
	canA := h.S.CanApprove(a.Org, a.User, a.Role)
	if !canA && it.CreatedBy != a.User {
		fail(c, &Error{Status: 404, Code: "NOT_FOUND", Msg: "This message wasn't found."})
		return nil, false, false
	}
	return it, canA, true
}

func (h *Handler) detail(c *gin.Context, a Actor, id string) Detail {
	it, _ := h.S.load(a.Org, id)
	if it == nil {
		return Detail{}
	}
	return h.S.DetailOf(c.Request.Context(), it, h.S.CanApprove(a.Org, a.User, a.Role))
}

func (h *Handler) get(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	it, canA, found := h.one(c, a)
	if !found {
		return
	}
	ok(c, h.S.DetailOf(c.Request.Context(), it, canA))
}

func bindEdits(c *gin.Context) (Edits, bool) {
	var e Edits
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&e); err != nil {
			fail(c, field("VALIDATION_ERROR", "", "Invalid request: "+err.Error()))
			return e, false
		}
	}
	return e, true
}

func (h *Handler) update(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	e, okk := bindEdits(c)
	if !okk {
		return
	}
	it, err := h.S.Update(a, c.Param("id"), e)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, h.S.DetailOf(c.Request.Context(), it, true))
}

// preview builds the preview of unsaved edits (nothing is saved, no event).
func (h *Handler) preview(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	it, _, found := h.one(c, a)
	if !found {
		return
	}
	e, okk := bindEdits(c)
	if !okk {
		return
	}
	if !e.empty() {
		if _, err := applyEdits(it, e); err != nil {
			fail(c, err)
			return
		}
	}
	d := h.S.DetailOf(c.Request.Context(), it, false)
	ok(c, gin.H{"preview": d.Preview})
}

func outcomeMessage(it *Item) string {
	switch it.Status {
	case StatusSent:
		return firstNonEmpty(it.Result, "Sent")
	case StatusApproved:
		if it.SendAt != nil {
			return "Approved — it goes " + it.SendAt.Format(time.RFC3339)
		}
		return "Approved"
	case StatusSuperseded:
		return "Not sent — it's no longer needed: " + it.SupersededReason
	case StatusFailed:
		return "Not sent: " + it.Error
	}
	return ""
}

func (h *Handler) approve(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	var in struct {
		When  string `json:"when"`
		Edits *Edits `json:"edits"`
		Note  string `json:"note"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&in); err != nil {
			fail(c, field("VALIDATION_ERROR", "", "Invalid request: "+err.Error()))
			return
		}
	}
	if in.When == "" {
		in.When = "now"
	}
	if in.When != "now" && in.When != "scheduled" {
		fail(c, field("VALIDATION_ERROR", "when", "Choose now or scheduled"))
		return
	}
	it, outcome, err := h.S.Approve(c.Request.Context(), a, c.Param("id"), in.When, in.Edits, in.Note)
	if err != nil && it == nil {
		fail(c, err)
		return
	}
	if err != nil && outcome == OutFailed {
		// The item is kept (failed, with the error) — say what happened.
		var e *Error
		if errors.As(err, &e) {
			c.JSON(e.Status, gin.H{"success": false, "error": gin.H{"code": e.Code, "message": e.Msg}, "message": e.Msg,
				"data": gin.H{"item": h.S.DetailOf(c.Request.Context(), it, true), "outcome": outcome, "message": outcomeMessage(it)}})
			return
		}
	}
	ok(c, gin.H{"item": h.S.DetailOf(c.Request.Context(), it, true), "outcome": outcome, "message": outcomeMessage(it)})
}

func (h *Handler) retry(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	it, outcome, err := h.S.Retry(c.Request.Context(), a, c.Param("id"))
	if err != nil && it == nil {
		fail(c, err)
		return
	}
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			c.JSON(e.Status, gin.H{"success": false, "error": gin.H{"code": e.Code, "message": e.Msg}, "message": e.Msg,
				"data": gin.H{"item": h.S.DetailOf(c.Request.Context(), it, true), "outcome": outcome, "message": outcomeMessage(it)}})
			return
		}
	}
	ok(c, gin.H{"item": h.S.DetailOf(c.Request.Context(), it, true), "outcome": outcome, "message": outcomeMessage(it)})
}

func (h *Handler) reject(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if c.Request.ContentLength != 0 {
		_ = c.ShouldBindJSON(&in)
	}
	it, err := h.S.Reject(c.Request.Context(), a, c.Param("id"), in.Note)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"item": h.S.DetailOf(c.Request.Context(), it, true)})
}

func (h *Handler) bulk(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	var in struct {
		Action string   `json:"action"`
		IDs    []string `json:"ids"`
		When   string   `json:"when"`
		Note   string   `json:"note"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, field("VALIDATION_ERROR", "", "Invalid request: "+err.Error()))
		return
	}
	res, err := h.S.Bulk(c.Request.Context(), a, in.Action, in.IDs, in.When, in.Note)
	if err != nil {
		fail(c, err)
		return
	}
	done, failed := 0, 0
	for _, r := range res {
		if r.OK {
			done++
		} else {
			failed++
		}
	}
	ok(c, gin.H{"results": res, "done": done, "failed": failed, "counts": h.S.CountsFor(a)})
}

// ---------------------------------------------------------------- settings

// SettingsView is the settings card.
type SettingsView struct {
	Enabled           bool              `json:"enabled"`
	AdminsCanSendNow  bool              `json:"admins_can_send_now"`
	CanSendNow        bool              `json:"can_send_now"`
	StaleDays         int               `json:"stale_days"`
	SoonAfter         bool              `json:"soon_after"`
	SoonAfterMinutes  int               `json:"soon_after_minutes"`
	Daily             bool              `json:"daily"`
	DailyTime         string            `json:"daily_time"`
	Timezone          string            `json:"timezone"`
	PrepareAheadHours int               `json:"prepare_ahead_hours"`
	Source            string            `json:"source"`
	UpdatedAt         *time.Time        `json:"updated_at"`
	UpdatedByName     string            `json:"updated_by_name"`
	Recipients        []RecipientView   `json:"recipients"`
	WhoGetsEmailsLink string            `json:"who_gets_emails_link"`
	Waiting           int64             `json:"waiting"`
	CanEdit           bool              `json:"can_edit"`
	Notes             map[string]string `json:"notes,omitempty"`
}

// RecipientView is one person who gets the update emails.
type RecipientView struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (h *Handler) settingsView(a Actor) SettingsView {
	s := h.S
	st := s.SettingsFor(a.Org)
	v := SettingsView{Enabled: st.Enabled, AdminsCanSendNow: st.AdminsCanSendNow, StaleDays: st.StaleDays, SoonAfter: st.SoonAfter,
		SoonAfterMinutes: int(SoonAfterDelay / time.Minute), Daily: st.Daily, DailyTime: st.DailyTime, Timezone: s.Loc(a.Org).String(),
		PrepareAheadHours: int(PrepareAhead / time.Hour), Source: "default", Recipients: []RecipientView{},
		WhoGetsEmailsLink: "/settings#who-gets-emails", CanEdit: s.CanApprove(a.Org, a.User, a.Role)}
	v.CanSendNow = v.CanEdit && st.AdminsCanSendNow
	if st.saved {
		v.Source = "saved"
		u := st.UpdatedAt
		v.UpdatedAt, v.UpdatedByName = &u, st.UpdatedByName
	}
	emails := map[string]bool{}
	for _, e := range s.DigestRecipients(a.Org) {
		emails[strings.ToLower(e)] = true
	}
	if len(emails) > 0 {
		var rows []struct{ FirstName, LastName, Email string }
		s.db().Raw(`SELECT COALESCE(first_name,'') AS first_name, COALESCE(last_name,'') AS last_name, LOWER(COALESCE(email,'')) AS email FROM users
			WHERE organization_id = ? AND deleted_at IS NULL ORDER BY created_at`, a.Org).Scan(&rows)
		for _, r := range rows {
			if emails[r.Email] {
				v.Recipients = append(v.Recipients, RecipientView{Name: strings.TrimSpace(r.FirstName + " " + r.LastName), Email: r.Email})
				delete(emails, r.Email)
			}
		}
		for e := range emails {
			v.Recipients = append(v.Recipients, RecipientView{Email: e})
		}
	}
	s.db().Model(&Item{}).Where("organization_id = ? AND status IN ?", a.Org, []string{StatusPending, StatusFailed}).Count(&v.Waiting)
	return v
}

func (h *Handler) getSettings(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	ok(c, h.settingsView(a))
}

func (h *Handler) putSettings(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	if !h.S.CanApprove(a.Org, a.User, a.Role) {
		fail(c, &Error{Status: 403, Code: "FORBIDDEN", Msg: "Only owners and admins can change Pending review."})
		return
	}
	var in struct {
		Enabled          *bool   `json:"enabled"`
		AdminsCanSendNow *bool   `json:"admins_can_send_now"`
		StaleDays        *int    `json:"stale_days"`
		SoonAfter        *bool   `json:"soon_after"`
		Daily            *bool   `json:"daily"`
		DailyTime        *string `json:"daily_time"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, field("VALIDATION_ERROR", "", "Invalid request: "+err.Error()))
		return
	}
	st, err := h.S.SaveSettings(a, func(st *Settings) error {
		if in.Enabled != nil {
			st.Enabled = *in.Enabled
		}
		if in.AdminsCanSendNow != nil {
			st.AdminsCanSendNow = *in.AdminsCanSendNow
		}
		if in.StaleDays != nil {
			if *in.StaleDays < 1 || *in.StaleDays > 60 {
				return field("VALIDATION_ERROR", "stale_days", "Choose between 1 and 60 days")
			}
			st.StaleDays = *in.StaleDays
		}
		if in.SoonAfter != nil {
			st.SoonAfter = *in.SoonAfter
		}
		if in.Daily != nil {
			st.Daily = *in.Daily
		}
		if in.DailyTime != nil {
			v := strings.TrimSpace(*in.DailyTime)
			if _, okk := parseHHMM(v); !okk {
				return field("VALIDATION_ERROR", "daily_time", "Use a time like 07:30")
			}
			st.DailyTime = v
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	_ = st
	ok(c, h.settingsView(a))
}

// SaveSettings changes the organisation's settings (audited as an event-less log line).
func (s *Service) SaveSettings(a Actor, change func(*Settings) error) (Settings, error) {
	st := s.SettingsFor(a.Org)
	before := st
	if err := change(&st); err != nil {
		return st, err
	}
	now := s.now()
	st.OrganizationID, st.UpdatedBy, st.UpdatedByName, st.UpdatedAt = a.Org, a.User, s.actorName(a), now
	if !st.saved {
		st.CreatedAt = now
	}
	if err := s.db().Save(&st).Error; err != nil {
		return st, err
	}
	s.audit(a, "outbox.settings", "outbox_settings", a.Org, before, st)
	st.saved = true
	return st, nil
}

// audit writes an audit_logs row (best effort).
func (s *Service) audit(a Actor, action, entityType, entityID string, old, values any) {
	Audit(s.db(), a.Org, a.User, action, entityType, entityID, old, values, s.now())
}

// ---------------------------------------------------------------- test clock

func (h *Handler) testAdmin(c *gin.Context) bool {
	if str(c, "user_role") != "super_admin" {
		fail(c, &Error{Status: 403, Code: "FORBIDDEN", Msg: "Test clock: super admins only."})
		return false
	}
	return true
}

func (h *Handler) testClock(c *gin.Context) {
	if !h.testAdmin(c) {
		return
	}
	var in struct {
		Now string `json:"now"`
	}
	_ = c.ShouldBindJSON(&in)
	if strings.TrimSpace(in.Now) == "" {
		h.S.SetClock(nil)
	} else {
		t, err := time.Parse(time.RFC3339, in.Now)
		if err != nil {
			fail(c, field("VALIDATION_ERROR", "now", "Use an RFC3339 time"))
			return
		}
		h.S.SetClock(&t)
	}
	ok(c, gin.H{"now": h.S.now()})
}

func (h *Handler) testRun(c *gin.Context) {
	if !h.testAdmin(c) {
		return
	}
	r, err := h.S.RunOnce(context.Background(), 10*time.Second)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, r)
}
