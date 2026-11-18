package brief

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// Handler serves the brief API.
//
//	GET  /brief/settings              my settings, sections, WhatsApp readiness, next send
//	PUT  /brief/settings              change them (phone → users.phone)
//	POST /brief/settings/quick        "Get a morning brief at 8:00" (one click)
//	GET  /brief/today                 today's brief, live (?refresh=1 syncs Jira first)
//	GET  /brief/day/:date             a stored brief (history), today live when not sent yet
//	GET  /brief/history               the last 30 days with delivery status per channel
//	GET  /brief/preview               the brief + the email and WhatsApp message it would send
//	POST /brief/test                  "Send me a test now"
//	GET/PUT /super-admin/brief/config the WhatsApp template (super admins)
//	GET/POST /brief/test-clock, POST /brief/test-clock/run   only with BRIEF_TEST_CLOCK=1
type Handler struct {
	S *Service
}

// RegisterRoutes mounts the routes on the signed-in group.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/brief")
	g.GET("/settings", h.getSettings)
	g.PUT("/settings", h.putSettings)
	g.POST("/settings/quick", h.quick)
	g.GET("/today", h.today)
	g.GET("/day/:date", h.day)
	g.GET("/history", h.history)
	g.GET("/preview", h.preview)
	g.POST("/test", h.test)
	sa := auth.Group("/super-admin/brief")
	sa.GET("/config", h.superAdmin(h.getConfig))
	sa.PUT("/config", h.superAdmin(h.putConfig))
	if h.S.TestClock {
		g.GET("/test-clock", h.managing(h.getClock))
		g.POST("/test-clock", h.managing(h.setClock))
		g.POST("/test-clock/run", h.managing(h.runClock))
	}
}

func ok(c *gin.Context, data any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": data}) }

func fail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func errOut(c *gin.Context, err error) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		fail(c, http.StatusBadRequest, "VALIDATION", ve.Msg)
		return
	}
	fail(c, http.StatusInternalServerError, "SERVER_ERROR", "Something went wrong — please try again")
}

func (h *Handler) viewer(c *gin.Context) (*Viewer, bool) {
	uid := c.GetString("user_id")
	if uid == "" {
		if v, ok := c.Get("user_id"); ok && v != nil {
			uid = fmt.Sprint(v)
		}
	}
	v, err := h.S.Viewer(uid)
	if err != nil {
		fail(c, http.StatusNotFound, "NO_USER", "Your account was not found — sign in again")
		return nil, false
	}
	return v, true
}

func (h *Handler) superAdmin(fn gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetString("user_role") != "super_admin" {
			fail(c, http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Only super admins can change this")
			return
		}
		fn(c)
	}
}

func (h *Handler) managing(fn gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !managingRoles[strings.ToLower(c.GetString("user_role"))] {
			fail(c, http.StatusForbidden, "FORBIDDEN", "Only owners and admins can use the test clock")
			return
		}
		fn(c)
	}
}

// ---------------------------------------------------------------- settings

func (h *Handler) settingsPayload(v *Viewer, st Settings) gin.H {
	s := h.S
	tz, _ := s.zoneOf(st)
	wst := wa.StatusOf(s.DB, v.OrgID, true)
	cfg := s.Config()
	waInfo := gin.H{"ready": wst.Ready && cfg.WATemplate != "", "connected": wst.Ready, "platform": wst.Platform, "template_ready": cfg.WATemplate != "", "note": wst.Note}
	if !wst.Ready {
		waInfo["note"] = "WhatsApp isn't set up yet — a super admin adds DASYIN's WhatsApp Business number in System settings → WhatsApp."
	} else if cfg.WATemplate == "" {
		waInfo["note"] = "WhatsApp is connected; the brief's message template still needs to be chosen by a super admin (System settings → Notifications)."
	}
	out := gin.H{
		"settings": st, "timezone": tz, "org_timezone": OrgZone(s.DB, v.OrgID), "phone": v.Phone, "email": v.Email,
		"first_name": v.FirstName, "managing": v.Managing, "country": orgprefs.Country(s.DB, v.OrgID),
		"sections": s.Sections(v, st), "whatsapp": waInfo, "next_send_at": nil, "last": nil,
	}
	if ok, why := s.emailAllowed(v); !ok {
		out["email_note"] = why
	}
	if at := s.NextSend(st); !at.IsZero() {
		out["next_send_at"] = at
	}
	var last Snapshot
	s.DB.Select("id, date, kind, status, headline, email_status, wa_status, email_error, wa_error, created_at, late, wa_kind, wa_platform").
		Where("user_id = ? AND kind = ?", v.UserID, KindScheduled).Order("date DESC").Limit(1).Find(&last)
	if last.ID != "" {
		out["last"] = last
	}
	return out
}

func (h *Handler) getSettings(c *gin.Context) {
	v, okk := h.viewer(c)
	if !okk {
		return
	}
	ok(c, h.settingsPayload(v, h.S.SettingsFor(v.UserID, v.OrgID)))
}

func (h *Handler) putSettings(c *gin.Context) {
	v, okk := h.viewer(c)
	if !okk {
		return
	}
	var in SettingsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_INPUT", "Invalid request body")
		return
	}
	st, err := h.S.SaveSettings(v, in)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, h.settingsPayload(v, st))
}

// quick is the one-click "Get a morning brief at 8:00": every day at 08:00
// in the organisation's time zone, by email, and by WhatsApp too when the
// person has a number and WhatsApp is ready.
func (h *Handler) quick(c *gin.Context) {
	v, okk := h.viewer(c)
	if !okk {
		return
	}
	t, days, tz, on := DefaultTime, DaysEveryDay, "", true
	email := v.Email != ""
	cfg := h.S.Config()
	whatsapp := strings.TrimSpace(v.Phone) != "" && wa.StatusOf(h.S.DB, v.OrgID, true).Ready && cfg.WATemplate != ""
	if _, err := wa.NormalizeNumber(v.Phone, orgprefs.Country(h.S.DB, v.OrgID)); err != nil {
		whatsapp = false
	}
	st, err := h.S.SaveSettings(v, SettingsInput{Enabled: &on, Time: &t, Days: &days, Timezone: &tz, Email: &email, WhatsApp: &whatsapp})
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, h.settingsPayload(v, st))
}

// ---------------------------------------------------------------- briefs

func strip(s Snapshot) Snapshot {
	s.Data = nil
	return s
}

func (h *Handler) today(c *gin.Context) {
	v, okk := h.viewer(c)
	if !okk {
		return
	}
	st := h.S.SettingsFor(v.UserID, v.OrgID)
	b := h.S.Build(c.Request.Context(), v, st, BuildOptions{SyncJira: true, JiraTimeout: 15 * time.Second})
	var snaps []Snapshot
	h.S.DB.Where("user_id = ? AND date = ?", v.UserID, b.Date).Order("kind").Find(&snaps)
	var delivery any
	for _, sn := range snaps {
		if sn.Kind == KindScheduled {
			delivery = strip(sn)
		}
	}
	ok(c, gin.H{"brief": b, "delivery": delivery, "live": true, "enabled": st.Enabled, "url": h.S.BriefURL(b.Date)})
}

func (h *Handler) day(c *gin.Context) {
	v, okk := h.viewer(c)
	if !okk {
		return
	}
	date := c.Param("date")
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_DATE", "Use a date like 2026-11-18")
		return
	}
	st := h.S.SettingsFor(v.UserID, v.OrgID)
	_, loc := h.S.zoneOf(st)
	today := localDay(h.S.now(), loc)
	if d.After(today) {
		fail(c, http.StatusNotFound, "NOT_YET", "That day's brief hasn't been written yet")
		return
	}
	var snaps []Snapshot
	h.S.DB.Where("user_id = ? AND date = ?", v.UserID, date).Find(&snaps)
	var pick *Snapshot
	for i := range snaps {
		if snaps[i].Kind == KindScheduled {
			pick = &snaps[i]
		}
	}
	if pick == nil {
		for i := range snaps {
			if snaps[i].Kind == KindTest && snaps[i].Data != nil {
				pick = &snaps[i]
			}
		}
	}
	if pick != nil && pick.Data != nil {
		b := pick.Data
		ok(c, gin.H{"brief": b, "delivery": strip(*pick), "live": false, "url": h.S.BriefURL(date)})
		return
	}
	if d.Equal(today) {
		b := h.S.Build(c.Request.Context(), v, st, BuildOptions{SyncJira: true, JiraTimeout: 15 * time.Second})
		var delivery any
		if pick != nil {
			delivery = strip(*pick)
		}
		ok(c, gin.H{"brief": b, "delivery": delivery, "live": true, "url": h.S.BriefURL(date)})
		return
	}
	if pick != nil { // missed or failed before it was written
		ok(c, gin.H{"brief": nil, "delivery": strip(*pick), "live": false, "url": h.S.BriefURL(date)})
		return
	}
	fail(c, http.StatusNotFound, "NO_BRIEF", "There is no brief for "+d.Format("Mon 2 Jan 2006"))
}

func (h *Handler) history(c *gin.Context) {
	v, okk := h.viewer(c)
	if !okk {
		return
	}
	days := 30
	if n, err := strconv.Atoi(c.Query("days")); err == nil && n > 0 && n <= 90 {
		days = n
	}
	st := h.S.SettingsFor(v.UserID, v.OrgID)
	_, loc := h.S.zoneOf(st)
	from := localDay(h.S.now(), loc).AddDate(0, 0, -days+1)
	list := []Snapshot{}
	h.S.DB.Omit("data").Where("user_id = ? AND date >= ?", v.UserID, dayStr(from)).Order("date DESC, kind").Find(&list)
	ok(c, gin.H{"items": list, "days": days})
}

func (h *Handler) preview(c *gin.Context) {
	v, okk := h.viewer(c)
	if !okk {
		return
	}
	st := h.S.SettingsFor(v.UserID, v.OrgID)
	b := h.S.Build(c.Request.Context(), v, st, BuildOptions{SyncJira: true, JiraTimeout: 15 * time.Second})
	msg := h.S.Email(v, b, false)
	plan := h.S.planWhatsApp(v, b)
	ok(c, gin.H{"brief": b, "email": gin.H{"to": v.Email, "subject": msg.Subject, "html": msg.HTML, "text": msg.Text},
		"whatsapp": plan, "channels": gin.H{"email": st.Email, "whatsapp": st.WhatsApp}})
}

func (h *Handler) test(c *gin.Context) {
	v, okk := h.viewer(c)
	if !okk {
		return
	}
	st := h.S.SettingsFor(v.UserID, v.OrgID)
	var in struct {
		Email    *bool `json:"email"`
		WhatsApp *bool `json:"whatsapp"`
	}
	_ = c.ShouldBindJSON(&in)
	if in.Email != nil {
		st.Email = *in.Email
	}
	if in.WhatsApp != nil {
		st.WhatsApp = *in.WhatsApp
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	snap, err := h.S.SendNow(ctx, v, st, KindTest)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, gin.H{"delivery": strip(*snap), "headline": snap.Headline})
}

// ---------------------------------------------------------------- platform config

func (h *Handler) configPayload(c *gin.Context) gin.H {
	s := h.S
	cfg := s.Config()
	rec := RecommendedTemplate
	button := strings.TrimRight(s.AppURL, "/") + rec.ButtonURL
	out := gin.H{"config": cfg, "recommended": gin.H{"name": rec.Name, "language": rec.Language, "category": rec.Category, "body": rec.Body,
		"button_text": rec.ButtonText, "button_url": button, "example": "Good morning Ken, your DASYIN brief for Wed 18 Nov: 3 bills due this week (KES 41,200), 2 Jira issues overdue, 1 new opportunity worth ~KES 60,000"},
		"templates": []wa.TemplateInfo{}, "whatsapp": wa.StatusOf(s.DB, c.GetString("org_id"), true), "chosen": nil, "note": ""}
	acc, err := wa.Resolve(s.DB, c.GetString("org_id"), true)
	if err != nil {
		out["note"] = err.Error()
		return out
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	list, err := acc.Templates(ctx)
	if err != nil {
		out["note"] = wa.Friendly(err)
		return out
	}
	out["templates"] = list
	for _, t := range list {
		if t.Name == cfg.WATemplate && t.Language == cfg.WALanguage {
			problems := []string{}
			if t.Status != "APPROVED" {
				problems = append(problems, "it is "+strings.ToLower(t.Status)+" at Meta, not approved yet")
			}
			if t.BodyParams != 3 {
				problems = append(problems, fmt.Sprintf("its body has %d parameters; the brief sends 3 (name, date, headline)", t.BodyParams))
			}
			if !t.URLButton {
				problems = append(problems, "it has no \"Open brief\" URL button ending in {{1}}")
			}
			out["chosen"] = gin.H{"template": t, "ok": len(problems) == 0, "problems": problems}
		}
	}
	if cfg.WATemplate != "" && out["chosen"] == nil {
		out["chosen"] = gin.H{"ok": false, "problems": []string{"no template named " + cfg.WATemplate + " in language " + cfg.WALanguage + " in the WhatsApp Business account"}}
	}
	return out
}

func (h *Handler) getConfig(c *gin.Context) { ok(c, h.configPayload(c)) }

func (h *Handler) putConfig(c *gin.Context) {
	var in struct {
		Template string `json:"whatsapp_template"`
		Language string `json:"whatsapp_language"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_INPUT", "Invalid request body")
		return
	}
	if _, err := h.S.SaveConfig(in.Template, in.Language, c.GetString("user_email")); err != nil {
		errOut(c, err)
		return
	}
	ok(c, h.configPayload(c))
}

// ---------------------------------------------------------------- test clock

func (h *Handler) clockPayload() gin.H {
	n := h.S.now()
	real := h.S.realNow()
	return gin.H{"enabled": true, "now": n, "offset_seconds": int(n.Sub(real).Seconds())}
}

func (h *Handler) getClock(c *gin.Context) { ok(c, h.clockPayload()) }

func (h *Handler) setClock(c *gin.Context) {
	var in struct {
		Now   string `json:"now"`
		Reset bool   `json:"reset"`
	}
	_ = c.ShouldBindJSON(&in)
	if in.Reset || strings.TrimSpace(in.Now) == "" {
		h.S.SetClock(time.Time{})
		ok(c, h.clockPayload())
		return
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(in.Now))
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_TIME", "Use an RFC 3339 time like 2026-11-19T08:00:00+03:00")
		return
	}
	h.S.SetClock(t)
	ok(c, h.clockPayload())
}

func (h *Handler) runClock(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()
	ok(c, h.S.RunDueWait(ctx, 90*time.Second))
}
