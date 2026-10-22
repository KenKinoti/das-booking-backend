package broadcast

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/occasions"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/platform"
)

// Option is a key/label pair for the composer's pickers.
type Option struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Handler serves the API:
//
// Super admins (audit-logged):
//
//	GET    /super-admin/announcements                 list (status, q, page) with stats
//	POST   /super-admin/announcements                 create a draft
//	GET    /super-admin/announcements/meta            pickers, AI and email status, settings
//	GET    /super-admin/announcements/organizations   ?q= organisation search
//	POST   /super-admin/announcements/audience        {audience, kind} → recipient count
//	POST   /super-admin/announcements/email-preview   composer input → the email (and the clean body)
//	GET    /super-admin/announcements/occasions       "Coming up" (?days=&all=1)
//	POST   /super-admin/announcements/occasions/plan  {key, date, schedule?} → a draft (or scheduled)
//	POST   /super-admin/announcements/occasions/skip  {key, date, skip}
//	POST   /super-admin/announcements/ai-draft        Write with AI
//	GET|PUT /super-admin/announcements/settings       nudge lead days, email, send hour
//	POST   /super-admin/announcements/run-due         run the scheduler now
//	GET    /super-admin/announcements/:id             detail + stats + next sends
//	PUT    /super-admin/announcements/:id             edit (draft / scheduled)
//	DELETE /super-admin/announcements/:id             delete a draft
//	POST   /super-admin/announcements/:id/schedule | unschedule | send-now | cancel | duplicate | test | retry-emails
//	GET    /super-admin/announcements/:id/recipients  ?filter=&q=&page=
//	GET    /super-admin/announcements/:id/export      CSV
//
// Everyone signed in:
//
//	GET  /announcements/me                    active announcements (+ the urgent banner)
//	GET  /announcements/me/:id                one (delivery id)
//	POST /announcements/me/:id/read | dismiss | click
//	GET|PUT /announcements/me/preferences     {promotions: bool}
//
// Public: GET /public/announcements/open?t= (tracked email buttons),
// GET|POST /public/announcements/unsubscribe, POST …/resubscribe.
type Handler struct {
	S *Service
	// BusinessTypes lists the business types (nil = pkg/industry defaults).
	BusinessTypes func() []Option
	// TestHooks enables the test clock and the fake occasions endpoints
	// (BROADCAST_TEST_HOOKS=1; never in production).
	TestHooks bool

	fakeMu   sync.Mutex
	fakeOccs []occasions.Occasion
	faking   bool
}

func ok(c *gin.Context, data any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": data}) }

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func failErr(c *gin.Context, err error, fallback string) {
	var fe *FieldError
	switch {
	case errors.As(err, &fe):
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"code": "VALIDATION_ERROR", "message": fe.Msg, "field": fe.Field}})
	case errors.Is(err, ErrNotEditable):
		fail(c, http.StatusConflict, "NOT_EDITABLE", err.Error())
	case errors.Is(err, gorm.ErrRecordNotFound):
		fail(c, http.StatusNotFound, "NOT_FOUND", "Announcement not found")
	case errors.Is(err, ErrAIOff):
		fail(c, http.StatusServiceUnavailable, "AI_NOT_CONFIGURED", err.Error())
	default:
		msg := fallback
		if err != nil && fallback == "" {
			msg = err.Error()
		}
		fail(c, http.StatusInternalServerError, "ERROR", msg)
	}
}

// RegisterRoutes mounts the routes.
func (h *Handler) RegisterRoutes(auth, public *gin.RouterGroup) {
	if h.TestHooks {
		base := h.S.Occasions
		h.S.Occasions = func(from, to time.Time, country string) []occasions.Occasion {
			h.fakeMu.Lock()
			faking, list := h.faking, append([]occasions.Occasion{}, h.fakeOccs...)
			h.fakeMu.Unlock()
			if !faking {
				if base != nil {
					return base(from, to, country)
				}
				return occasions.Between(from, to, country)
			}
			out := []occasions.Occasion{}
			f, t := dateOnly(from), dateOnly(to)
			for _, o := range list {
				d := dateOnly(o.Date)
				if d.Before(f) || d.After(t) {
					continue
				}
				if country != "" && len(o.Countries) > 0 && !containsFold(o.Countries, country) {
					continue
				}
				out = append(out, o)
			}
			sort.SliceStable(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
			return out
		}
	}

	g := auth.Group("/super-admin/announcements", platform.RequireSuperAdmin())
	g.GET("", h.list)
	g.POST("", h.create)
	g.GET("/meta", h.meta)
	g.GET("/organizations", h.orgs)
	g.POST("/audience", h.audience)
	g.POST("/email-preview", h.emailPreview)
	g.GET("/occasions", h.occasions)
	g.POST("/occasions/plan", h.planOccasion)
	g.POST("/occasions/skip", h.skipOccasion)
	g.POST("/ai-draft", h.aiDraft)
	g.GET("/settings", h.getSettings)
	g.PUT("/settings", h.putSettings)
	g.POST("/run-due", h.runDue)
	if h.TestHooks {
		g.POST("/test-clock", h.testClock)
		g.POST("/test-occasions", h.testOccasions)
	}
	g.GET("/:id", h.get)
	g.PUT("/:id", h.update)
	g.DELETE("/:id", h.remove)
	g.POST("/:id/schedule", h.schedule)
	g.POST("/:id/unschedule", h.unschedule)
	g.POST("/:id/send-now", h.sendNow)
	g.POST("/:id/cancel", h.cancel)
	g.POST("/:id/duplicate", h.duplicate)
	g.POST("/:id/test", h.test)
	g.POST("/:id/retry-emails", h.retryEmails)
	g.GET("/:id/recipients", h.recipients)
	g.GET("/:id/export", h.export)

	me := auth.Group("/announcements/me")
	me.GET("", h.mine)
	me.GET("/preferences", h.myPrefs)
	me.PUT("/preferences", h.saveMyPrefs)
	me.GET("/:id", h.mineOne)
	me.POST("/:id/read", func(c *gin.Context) { h.mark(c, "read") })
	me.POST("/:id/dismiss", func(c *gin.Context) { h.mark(c, "dismiss") })
	me.POST("/:id/click", func(c *gin.Context) { h.mark(c, "click") })

	pg := public.Group("/announcements")
	pg.GET("/open", h.open)
	pg.GET("/unsubscribe", h.unsubInfo)
	pg.POST("/unsubscribe", h.unsubscribe)
	pg.POST("/resubscribe", h.resubscribe)
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

func (h *Handler) audit(c *gin.Context, action, id string, oldV, newV any) {
	platform.Audit(h.S.DB, c, "", "announcement."+action, "announcement", id, oldV, newV)
}

func (h *Handler) load(c *gin.Context) (*Announcement, bool) {
	var a Announcement
	if err := fresh(h.S.DB).First(&a, "id = ?", c.Param("id")).Error; err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Announcement not found")
		return nil, false
	}
	return &a, true
}

func summary(a *Announcement) gin.H {
	return gin.H{"title": a.Title, "kind": a.Kind, "priority": a.Priority, "status": a.Status, "audience": a.Audience, "local_send_at": a.LocalSendAt,
		"timezone": a.Timezone, "recurrence": a.Recurrence, "repeat_until": a.RepeatUntil, "email_delay_minutes": a.EmailDelayMinutes}
}

// view is an announcement with its stats and schedule.
func (h *Handler) view(a *Announcement, st *Stats) gin.H {
	if st == nil {
		m := h.S.StatsFor([]string{a.ID})
		v := m[a.ID]
		st = &v
	}
	next := []time.Time{}
	if a.Status == StatusScheduled && a.NextSendAt != nil {
		if a.Recurrence == RepeatNone {
			next = []time.Time{*a.NextSendAt}
		} else {
			for k := a.Occurrences; len(next) < 4 && k < a.Occurrences+60; k++ {
				t, err := Occurrence(a.LocalSendAt, a.Timezone, a.Recurrence, k)
				if err != nil || !withinUntil(t, a.Timezone, a.RepeatUntil) {
					break
				}
				if !t.Before(*a.NextSendAt) {
					next = append(next, t)
				}
			}
		}
	}
	return gin.H{"announcement": a, "stats": st, "next_sends": next, "service_notice": ServiceNotice(a)}
}

// ---------------------------------------------------------------- list / detail

func (h *Handler) list(c *gin.Context) {
	q := fresh(h.S.DB).Model(&Announcement{})
	if st := c.Query("status"); st != "" && st != "all" {
		q = q.Where("status = ?", st)
	}
	if k := c.Query("kind"); k != "" {
		q = q.Where("kind = ?", k)
	}
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(s)) + "%"
		q = q.Where("LOWER(title) LIKE ? OR LOWER(body_text) LIKE ?", like, like)
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "25"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 25
	}
	var total int64
	q.Count(&total)
	var rows []Announcement
	q.Order("CASE status WHEN 'sending' THEN 0 WHEN 'scheduled' THEN 1 WHEN 'draft' THEN 2 ELSE 3 END, COALESCE(next_send_at, last_sent_at, updated_at) DESC").
		Offset((page - 1) * limit).Limit(limit).Find(&rows)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	stats := h.S.StatsFor(ids)
	items := make([]gin.H, 0, len(rows))
	for i := range rows {
		st := stats[rows[i].ID]
		items = append(items, gin.H{"announcement": rows[i], "stats": st})
	}
	var counts []struct {
		Status string
		N      int64
	}
	fresh(h.S.DB).Model(&Announcement{}).Select("status, COUNT(*) AS n").Group("status").Scan(&counts)
	byStatus := gin.H{"all": int64(0), StatusDraft: int64(0), StatusScheduled: int64(0), StatusSending: int64(0), StatusSent: int64(0), StatusCancelled: int64(0)}
	var all int64
	for _, r := range counts {
		byStatus[r.Status] = r.N
		all += r.N
	}
	byStatus["all"] = all
	// Totals over everything sent in the last 30 days.
	var t struct{ Delivered, Read, Emailed, Clicked int64 }
	fresh(h.S.DB).Raw(`SELECT COUNT(*) FILTER (WHERE delivered_at IS NOT NULL) AS delivered, COUNT(*) FILTER (WHERE read_at IS NOT NULL) AS read,
		COUNT(*) FILTER (WHERE email_status = 'sent') AS emailed, COUNT(*) FILTER (WHERE clicked_at IS NOT NULL) AS clicked
		FROM broadcast_deliveries WHERE NOT is_test AND delivered_at >= ?`, Now().AddDate(0, 0, -30)).Scan(&t)
	ok(c, gin.H{"items": items, "total": total, "page": page, "limit": limit, "counts": byStatus,
		"last_30_days": gin.H{"delivered": t.Delivered, "read": t.Read, "read_pct": pct(t.Read, t.Delivered), "emailed": t.Emailed, "clicked": t.Clicked}})
}

func (h *Handler) get(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	ok(c, h.view(a, nil))
}

func bind(c *gin.Context) (Input, bool) {
	var in Input
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return in, false
	}
	return in, true
}

func (h *Handler) create(c *gin.Context) {
	in, okk := bind(c)
	if !okk {
		return
	}
	now := Now()
	a := Announcement{ID: uuid.NewString(), Status: StatusDraft, CreatedBy: c.GetString("user_id"), UpdatedBy: c.GetString("user_id"), CreatedAt: now, UpdatedAt: now}
	if p, found := h.S.Person(a.CreatedBy); found {
		a.CreatedByName = p.Name
	}
	if err := Apply(&a, in); err != nil {
		failErr(c, err, "")
		return
	}
	if err := fresh(h.S.DB).Create(&a).Error; err != nil {
		failErr(c, err, "Could not save the announcement")
		return
	}
	h.audit(c, "create", a.ID, nil, summary(&a))
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": h.view(&a, &Stats{})})
}

func (h *Handler) update(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	if a.Status != StatusDraft && a.Status != StatusScheduled {
		failErr(c, ErrNotEditable, "")
		return
	}
	if a.Status == StatusScheduled && a.Occurrences > 0 {
		failErr(c, errors.New("this repeating announcement has started — cancel it and duplicate it to change it"), "")
		return
	}
	in, okk := bind(c)
	if !okk {
		return
	}
	before := summary(a)
	if err := Apply(a, in); err != nil {
		failErr(c, err, "")
		return
	}
	a.UpdatedBy, a.UpdatedAt = c.GetString("user_id"), Now()
	if a.Status == StatusScheduled {
		// A scheduled one stays scheduled at its (new) time.
		if err := h.S.Schedule(a, a.UpdatedBy); err != nil {
			failErr(c, err, "")
			return
		}
	} else if err := fresh(h.S.DB).Save(a).Error; err != nil {
		failErr(c, err, "Could not save the announcement")
		return
	}
	h.audit(c, "update", a.ID, before, summary(a))
	ok(c, h.view(a, nil))
}

func (h *Handler) remove(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	var n int64
	fresh(h.S.DB).Model(&Delivery{}).Where("announcement_id = ? AND NOT is_test", a.ID).Count(&n)
	if a.Status != StatusDraft || n > 0 {
		fail(c, http.StatusConflict, "NOT_DELETABLE", "Only drafts that were never sent can be deleted — cancel this one instead")
		return
	}
	err := fresh(h.S.DB).Transaction(func(tx *gorm.DB) error {
		if hasTable(tx, "notifications") {
			tx.Exec(`DELETE FROM notifications WHERE entity_type = 'announcement' AND entity_id IN (SELECT id FROM broadcast_deliveries WHERE announcement_id = ?)`, a.ID)
		}
		if err := tx.Where("announcement_id = ?", a.ID).Delete(&Delivery{}).Error; err != nil {
			return err
		}
		return tx.Delete(&Announcement{}, "id = ?", a.ID).Error
	})
	if err != nil {
		failErr(c, err, "Could not delete the announcement")
		return
	}
	h.audit(c, "delete", a.ID, summary(a), nil)
	ok(c, gin.H{"deleted": a.ID})
}

func (h *Handler) schedule(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	if err := h.S.Schedule(a, c.GetString("user_id")); err != nil {
		failErr(c, err, "")
		return
	}
	h.audit(c, "schedule", a.ID, nil, summary(a))
	ok(c, h.view(a, nil))
}

func (h *Handler) unschedule(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	if err := h.S.Unschedule(a, c.GetString("user_id")); err != nil {
		failErr(c, err, "")
		return
	}
	h.audit(c, "unschedule", a.ID, nil, summary(a))
	ok(c, h.view(a, nil))
}

func (h *Handler) sendNow(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	n, err := h.S.SendNow(c.Request.Context(), a, c.GetString("user_id"))
	if err != nil {
		failErr(c, err, "")
		return
	}
	h.audit(c, "send_now", a.ID, nil, gin.H{"title": a.Title, "recipients": n})
	v := h.view(a, nil)
	v["sent_to"] = n
	ok(c, v)
}

func (h *Handler) cancel(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	before := a.Status
	if err := h.S.Cancel(a, c.GetString("user_id")); err != nil {
		failErr(c, err, "Could not cancel the announcement")
		return
	}
	h.audit(c, "cancel", a.ID, gin.H{"status": before}, gin.H{"status": a.Status})
	ok(c, h.view(a, nil))
}

func (h *Handler) duplicate(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	now := Now()
	b := *a
	b.ID, b.Status, b.Occurrences, b.LastSentAt, b.CancelledAt, b.NextSendAt, b.LastError = uuid.NewString(), StatusDraft, 0, nil, nil, nil, ""
	b.Title = clip("Copy of "+a.Title, 140)
	b.CreatedBy, b.UpdatedBy, b.CreatedAt, b.UpdatedAt = c.GetString("user_id"), c.GetString("user_id"), now, now
	if p, found := h.S.Person(b.CreatedBy); found {
		b.CreatedByName = p.Name
	}
	if b.SendAt != nil && b.SendAt.Before(now) {
		b.SendAt, b.LocalSendAt = nil, ""
	}
	if b.ExpiresAt != nil && b.ExpiresAt.Before(now) {
		b.ExpiresAt, b.LocalExpiresAt = nil, ""
	}
	if err := fresh(h.S.DB).Create(&b).Error; err != nil {
		failErr(c, err, "Could not duplicate the announcement")
		return
	}
	h.audit(c, "duplicate", b.ID, gin.H{"from": a.ID}, summary(&b))
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": h.view(&b, &Stats{})})
}

func (h *Handler) test(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	me, found := h.S.Person(c.GetString("user_id"))
	if !found {
		fail(c, http.StatusBadRequest, "NO_USER", "Your account could not be found")
		return
	}
	d, err := h.S.TestSend(c.Request.Context(), a, me)
	if err != nil {
		failErr(c, err, "Could not send the test")
		return
	}
	h.audit(c, "test_send", a.ID, nil, gin.H{"to": me.Email, "email_status": d.EmailStatus})
	ok(c, gin.H{"delivery": d, "email": me.Email, "email_status": d.EmailStatus, "email_note": d.EmailNote})
}

func (h *Handler) retryEmails(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	n := h.S.RetryFailed(a)
	h.audit(c, "retry_emails", a.ID, nil, gin.H{"queued": n})
	ok(c, gin.H{"queued": n})
}

func (h *Handler) recipients(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	h.S.syncReads(readScope{announcements: []string{a.ID}})
	q := fresh(h.S.DB).Model(&Delivery{}).Where("announcement_id = ? AND NOT is_test", a.ID)
	switch c.Query("filter") {
	case "read":
		q = q.Where("read_at IS NOT NULL")
	case "unread":
		q = q.Where("read_at IS NULL")
	case "clicked":
		q = q.Where("clicked_at IS NOT NULL")
	case "emailed":
		q = q.Where("email_status = ?", EmailSent)
	case "failed":
		q = q.Where("email_status = ?", EmailFailed)
	case "skipped":
		q = q.Where("email_status = ?", EmailSkipped)
	case "unsubscribed":
		q = q.Where("unsubscribed")
	}
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(s)) + "%"
		q = q.Where("LOWER(name) LIKE ? OR LOWER(email) LIKE ? OR LOWER(org_name) LIKE ?", like, like, like)
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	var total int64
	q.Count(&total)
	var rows []Delivery
	q.Order("occurrence_at DESC, org_name, name").Offset((page - 1) * limit).Limit(limit).Find(&rows)
	ok(c, gin.H{"recipients": rows, "total": total, "page": page, "limit": limit})
}

func (h *Handler) export(c *gin.Context) {
	a, okk := h.load(c)
	if !okk {
		return
	}
	b, err := h.S.ExportCSV(a)
	if err != nil {
		failErr(c, err, "Could not export")
		return
	}
	h.audit(c, "export", a.ID, nil, gin.H{"bytes": len(b)})
	name := "announcement-" + slug(a.Title) + ".csv"
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", b)
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "export"
	}
	return out
}

// ---------------------------------------------------------------- composer helpers

var commonZones = []string{"Africa/Nairobi", "Africa/Kampala", "Africa/Dar_es_Salaam", "Africa/Kigali", "Africa/Lagos", "Africa/Johannesburg",
	"Australia/Sydney", "Australia/Melbourne", "Australia/Brisbane", "Australia/Adelaide", "Australia/Perth", "Australia/Darwin", "Australia/Hobart",
	"Pacific/Auckland", "Asia/Dubai", "Asia/Singapore", "Europe/London", "America/New_York", "UTC"}

func (h *Handler) businessTypes() []Option {
	if h.BusinessTypes != nil {
		return h.BusinessTypes()
	}
	out := []Option{}
	for _, t := range industry.DefaultTypes() {
		out = append(out, Option{Key: t.Key, Label: t.Name})
	}
	return out
}

func (h *Handler) meta(c *gin.Context) {
	// Countries that have organisations, Kenya and Australia first.
	perCountry := map[string]int{}
	tiersSeen := map[string]bool{}
	for _, o := range h.S.Orgs() {
		if o.Country != "" {
			perCountry[o.Country]++
		}
		if o.Tier != "" {
			tiersSeen[o.Tier] = true
		}
	}
	for _, cc := range []string{"KE", "AU"} {
		if _, ok := perCountry[cc]; !ok {
			perCountry[cc] = 0
		}
	}
	countries := []gin.H{}
	keys := make([]string, 0, len(perCountry))
	for k := range perCountry {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		switch k {
		case "KE":
			return 0
		case "AU":
			return 1
		}
		return 2
	}
	sort.Slice(keys, func(i, j int) bool {
		if rank(keys[i]) != rank(keys[j]) {
			return rank(keys[i]) < rank(keys[j])
		}
		return currency.CountryName(keys[i]) < currency.CountryName(keys[j])
	})
	for _, k := range keys {
		name := currency.CountryName(k)
		if name == "" {
			name = k
		}
		countries = append(countries, gin.H{"key": k, "label": name, "organizations": perCountry[k]})
	}
	tiers := []Option{}
	if hasTable(h.S.DB, "plan_tiers") {
		var ts []struct{ Key, Name string }
		fresh(h.S.DB).Raw(`SELECT key, name FROM plan_tiers ORDER BY sort_order, key`).Scan(&ts)
		for _, t := range ts {
			tiers = append(tiers, Option{Key: t.Key, Label: t.Name})
			delete(tiersSeen, t.Key)
		}
	}
	for k := range tiersSeen {
		tiers = append(tiers, Option{Key: k, Label: strings.ToUpper(k[:1]) + k[1:]})
	}
	if len(tiers) == 0 {
		tiers = []Option{{"small", "Small"}, {"sme", "SME"}, {"large", "Large"}}
	}
	zones := append([]string{}, commonZones...)
	st := h.S.GetSettings()
	cfg, _ := mail.Shared(h.S.DB).Resolve(firstOr(mail.Shared(h.S.DB).PlatformOrgs()))
	ok(c, gin.H{
		"kinds": []gin.H{
			{"key": KindUpdate, "label": "Update", "icon": "fa-solid fa-bullhorn", "description": "New features, changes and tips."},
			{"key": KindReminder, "label": "Reminder", "icon": "fa-solid fa-bell", "description": "Something to check or do."},
			{"key": KindPromotion, "label": "Offer", "icon": "fa-solid fa-gift", "description": "Plans, modules, discounts — with an unsubscribe link."},
			{"key": KindUrgent, "label": "Urgent", "icon": "fa-solid fa-triangle-exclamation", "description": "Outages, maintenance, security. Always emailed."},
		},
		"delays": []gin.H{
			{"minutes": 0, "label": "Straight away"}, {"minutes": 60, "label": "After 1 hour"}, {"minutes": 180, "label": "After 3 hours"},
			{"minutes": 360, "label": "After 6 hours"}, {"minutes": 1440, "label": "After 24 hours"}, {"minutes": NeverEmail, "label": "Never — in the app only"},
		},
		"role_levels":    []Option{{LevelEveryone, "Everyone"}, {LevelManagers, "Managers and above"}, {LevelOwners, "Owners and admins only"}},
		"business_types": h.businessTypes(), "plan_tiers": tiers, "countries": countries, "timezones": zones,
		"default_timezone": DefaultTimezone, "ai_enabled": h.S.AIEnabled(),
		"email":      gin.H{"ready": cfg.Ready(), "from": cfg.Display(), "source": cfg.Source, "problem": cfg.Problem},
		"settings":   st,
		"legal_note": "Offers always carry a one-click unsubscribe link: Kenya's Data Protection Act 2019 and Australia's Spam Act 2003 require a working opt-out in commercial messages, honoured straight away. Unsubscribing stops offer emails and in-app offers for that person.",
		"test_hooks": h.TestHooks, "clock_offset_minutes": int(ClockOffset() / time.Minute), "now": Now(),
	})
}

func firstOr(list []string) string {
	if len(list) > 0 {
		return list[0]
	}
	return ""
}

func (h *Handler) orgs(c *gin.Context) {
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	ids := map[string]bool{}
	for _, id := range strings.Split(c.Query("ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids[id] = true
		}
	}
	out := []OrgInfo{}
	for _, o := range h.S.Orgs() {
		if len(ids) > 0 {
			if ids[o.ID] {
				out = append(out, o)
			}
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(o.Name), q) {
			continue
		}
		out = append(out, o)
		if len(out) >= 20 {
			break
		}
	}
	ok(c, gin.H{"organizations": out})
}

func (h *Handler) audience(c *gin.Context) {
	var in struct {
		Audience Audience `json:"audience"`
		Kind     string   `json:"kind"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	ok(c, h.S.PreviewAudience(in.Audience, in.Kind))
}

func (h *Handler) emailPreview(c *gin.Context) {
	in, okk := bind(c)
	if !okk {
		return
	}
	if strings.TrimSpace(in.Title) == "" {
		in.Title = "Your title"
	}
	if strings.TrimSpace(in.Body) == "" {
		in.Body = "Your message"
	}
	in.LocalSendAt, in.LocalExpiresAt, in.RepeatUntil = "", "", ""
	a := Announcement{ID: "preview"}
	if err := Apply(&a, in); err != nil {
		failErr(c, err, "")
		return
	}
	me, _ := h.S.Person(c.GetString("user_id"))
	d := Delivery{ID: "preview", Email: me.Email, Name: me.Name}
	if d.Email == "" {
		d.Email = "you@example.com"
	}
	msg := h.S.EmailFor(&a, &d, false)
	ok(c, gin.H{"subject": msg.Subject, "html": msg.HTML, "text": msg.Text, "body": a.Body, "body_text": a.BodyText, "service_notice": ServiceNotice(&a)})
}

// ---------------------------------------------------------------- occasions

func (h *Handler) occasions(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "0"))
	if days > 120 {
		days = 120
	}
	all := c.Query("all") == "1" || c.Query("all") == "true"
	ok(c, gin.H{"occasions": h.S.ComingUp(days, all), "settings": h.S.GetSettings(), "ai_enabled": h.S.AIEnabled()})
}

func (h *Handler) planOccasion(c *gin.Context) {
	var in struct {
		Key      string `json:"key"`
		Date     string `json:"date"`
		Schedule bool   `json:"schedule"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Key == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose an occasion")
		return
	}
	o, found := h.S.FindOccasion(in.Key, in.Date)
	if !found {
		fail(c, http.StatusNotFound, "NOT_FOUND", "That occasion is not in the calendar")
		return
	}
	if p := h.S.plannedFor(o.Key, in.Date); p != nil && !in.Schedule {
		ok(c, gin.H{"announcement": p, "existing": true})
		return
	}
	d := h.S.DraftFor(o, h.S.GetSettings())
	delay := DefaultDelay(d.Priority)
	inp := Input{Title: d.Title, Body: d.Body, Kind: d.Kind, Priority: d.Priority, CTALabel: d.CTALabel, CTAURL: d.CTAURL, Audience: d.Audience,
		Timezone: d.Timezone, LocalSendAt: d.LocalSendAt, EmailDelayMinutes: &delay, OccasionKey: o.Key, OccasionDate: in.Date, OccasionName: o.Name}
	now := Now()
	a := Announcement{ID: uuid.NewString(), Status: StatusDraft, CreatedBy: c.GetString("user_id"), UpdatedBy: c.GetString("user_id"), CreatedAt: now, UpdatedAt: now}
	if p, found := h.S.Person(a.CreatedBy); found {
		a.CreatedByName = p.Name
	}
	if err := Apply(&a, inp); err != nil {
		failErr(c, err, "")
		return
	}
	if err := fresh(h.S.DB).Create(&a).Error; err != nil {
		failErr(c, err, "Could not create the draft")
		return
	}
	if in.Schedule {
		if err := h.S.Schedule(&a, a.CreatedBy); err != nil {
			failErr(c, err, "")
			return
		}
	}
	h.audit(c, "occasion_plan", a.ID, nil, gin.H{"occasion": o.Key, "date": in.Date, "scheduled": in.Schedule, "send_at": a.LocalSendAt})
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": gin.H{"announcement": a, "existing": false}})
}

func (h *Handler) skipOccasion(c *gin.Context) {
	var in struct {
		Key  string `json:"key"`
		Date string `json:"date"`
		Skip bool   `json:"skip"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Key == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose an occasion")
		return
	}
	d, err := time.Parse(DateLayout, in.Date)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose an occasion date")
		return
	}
	who := c.GetString("user_id")
	name := ""
	if p, found := h.S.Person(who); found {
		name = p.Name
	}
	if err := h.S.SetSkip(in.Key, d.Year(), in.Skip, who, name); err != nil {
		failErr(c, err, "Could not save")
		return
	}
	h.audit(c, "occasion_skip", in.Key, nil, gin.H{"occasion": in.Key, "year": d.Year(), "skip": in.Skip})
	ok(c, gin.H{"key": in.Key, "year": d.Year(), "skipped": in.Skip})
}

func (h *Handler) aiDraft(c *gin.Context) {
	var in AIRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	if in.OccasionKey != "" && in.OccasionName == "" {
		if o, found := h.S.FindOccasion(in.OccasionKey, in.OccasionDate); found {
			in.OccasionName = o.Name
		}
	}
	res, err := h.S.WriteWithAI(c.Request.Context(), in)
	if err != nil {
		if errors.Is(err, ErrAIOff) {
			failErr(c, err, "")
			return
		}
		fail(c, http.StatusBadGateway, "AI_FAILED", err.Error())
		return
	}
	h.audit(c, "ai_draft", in.OccasionKey, nil, gin.H{"occasion": in.OccasionKey, "kind": in.Kind, "swahili": in.Swahili, "model": res.Model})
	ok(c, res)
}

func (h *Handler) getSettings(c *gin.Context) { ok(c, h.S.GetSettings()) }

func (h *Handler) putSettings(c *gin.Context) {
	var in Settings
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	before := h.S.GetSettings()
	st, err := h.S.SaveSettings(in, c.GetString("user_id"))
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	h.audit(c, "settings", SettingsID, before, st)
	ok(c, st)
}

func (h *Handler) runDue(c *gin.Context) {
	r, err := h.S.RunOnce(c.Request.Context(), 20*time.Second)
	if err != nil {
		fail(c, http.StatusConflict, "BUSY", err.Error())
		return
	}
	h.audit(c, "run_due", "", nil, r)
	ok(c, r)
}

// testClock moves the package clock (test hooks only).
func (h *Handler) testClock(c *gin.Context) {
	var in struct {
		OffsetMinutes *int   `json:"offset_minutes"`
		Offset        string `json:"offset"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	switch {
	case in.OffsetMinutes != nil:
		SetClockOffset(time.Duration(*in.OffsetMinutes) * time.Minute)
	case in.Offset != "":
		d, err := time.ParseDuration(in.Offset)
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "offset is a duration like 61m")
			return
		}
		SetClockOffset(d)
	}
	ok(c, gin.H{"offset_minutes": int(ClockOffset() / time.Minute), "now": Now()})
}

// testOccasions replaces the calendar with the given occasions (test hooks
// only); {"occasions": null} goes back to the real one.
func (h *Handler) testOccasions(c *gin.Context) {
	var in struct {
		Occasions []struct {
			occasions.Occasion
			Date string `json:"date"`
		} `json:"occasions"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	list := []occasions.Occasion{}
	for _, o := range in.Occasions {
		d, err := time.Parse(DateLayout, o.Date)
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "date is YYYY-MM-DD")
			return
		}
		occ := o.Occasion
		occ.Date = d
		list = append(list, occ)
	}
	h.fakeMu.Lock()
	h.faking = in.Occasions != nil
	h.fakeOccs = list
	h.fakeMu.Unlock()
	ok(c, gin.H{"faking": in.Occasions != nil, "count": len(list)})
}

// ---------------------------------------------------------------- my announcements

// MyItem is one announcement in a person's app.
type MyItem struct {
	ID             string     `json:"id"` // delivery id
	AnnouncementID string     `json:"announcement_id"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	BodyText       string     `json:"body_text"`
	Kind           string     `json:"kind"`
	KindLabel      string     `json:"kind_label"`
	Priority       string     `json:"priority"`
	CTALabel       string     `json:"cta_label"`
	CTAURL         string     `json:"cta_url"`
	ImageURL       string     `json:"image_url"`
	DeliveredAt    *time.Time `json:"delivered_at"`
	ReadAt         *time.Time `json:"read_at"`
	DismissedAt    *time.Time `json:"dismissed_at"`
	ClickedAt      *time.Time `json:"clicked_at"`
	ExpiresAt      *time.Time `json:"expires_at"`
	IsTest         bool       `json:"is_test"`
	Banner         bool       `json:"banner"`
	Active         bool       `json:"active"`
}

func itemOf(a *Announcement, d *Delivery, now time.Time) MyItem {
	it := MyItem{ID: d.ID, AnnouncementID: a.ID, Title: a.Title, Body: a.Body, BodyText: a.BodyText, Kind: a.Kind, KindLabel: KindLabel(a.Kind),
		Priority: a.Priority, CTALabel: a.CTALabel, CTAURL: a.CTAURL, ImageURL: a.ImageURL, DeliveredAt: d.DeliveredAt, ReadAt: d.ReadAt,
		DismissedAt: d.DismissedAt, ClickedAt: d.ClickedAt, ExpiresAt: a.ExpiresAt, IsTest: d.IsTest}
	it.Active = a.Status != StatusCancelled && (a.ExpiresAt == nil || a.ExpiresAt.After(now))
	it.Banner = it.Active && (a.Priority == PriorityUrgent || a.Kind == KindUrgent) && d.ReadAt == nil && d.DismissedAt == nil
	return it
}

func (h *Handler) who(c *gin.Context) (string, bool) {
	u := c.GetString("user_id")
	if u == "" {
		fail(c, http.StatusUnauthorized, "NO_SESSION", "Sign in to see announcements")
		return "", false
	}
	return u, true
}

// Mine lists a person's active announcements: the newest occurrence of
// each, not cancelled or expired, offers only while they are subscribed.
func (s *Service) Mine(userID string, limit int) []MyItem {
	s.syncReads(readScope{userID: userID})
	now := Now()
	var ds []Delivery
	fresh(s.DB).Raw(`SELECT DISTINCT ON (d.announcement_id) d.* FROM broadcast_deliveries d
		JOIN broadcast_announcements a ON a.id = d.announcement_id
		WHERE d.user_id = ? AND a.status <> 'cancelled' AND (a.expires_at IS NULL OR a.expires_at > ?)
		ORDER BY d.announcement_id, d.occurrence_at DESC`, userID, now).Scan(&ds)
	unsub := s.Unsubscribed(userID)
	ids := []string{}
	for _, d := range ds {
		ids = append(ids, d.AnnouncementID)
	}
	anns := map[string]*Announcement{}
	if len(ids) > 0 {
		var as []Announcement
		fresh(s.DB).Where("id IN ?", ids).Find(&as)
		for i := range as {
			anns[as[i].ID] = &as[i]
		}
	}
	out := []MyItem{}
	for i := range ds {
		a := anns[ds[i].AnnouncementID]
		if a == nil || (a.Kind == KindPromotion && unsub) {
			continue
		}
		out = append(out, itemOf(a, &ds[i], now))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Banner != out[j].Banner {
			return out[i].Banner
		}
		ti, tj := out[i].DeliveredAt, out[j].DeliveredAt
		return ti != nil && (tj == nil || ti.After(*tj))
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (h *Handler) mine(c *gin.Context) {
	u, okk := h.who(c)
	if !okk {
		return
	}
	items := h.S.Mine(u, 50)
	banner := []MyItem{}
	unread := 0
	for _, it := range items {
		if it.Banner {
			banner = append(banner, it)
		}
		if it.ReadAt == nil {
			unread++
		}
	}
	ok(c, gin.H{"items": items, "banner": banner, "unread": unread, "promotions": !h.S.Unsubscribed(u)})
}

func (h *Handler) mineOne(c *gin.Context) {
	u, okk := h.who(c)
	if !okk {
		return
	}
	var d Delivery
	if err := fresh(h.S.DB).First(&d, "id = ? AND user_id = ?", c.Param("id"), u).Error; err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "This announcement is no longer available")
		return
	}
	var a Announcement
	if err := fresh(h.S.DB).First(&a, "id = ?", d.AnnouncementID).Error; err != nil || a.Status == StatusCancelled {
		fail(c, http.StatusNotFound, "NOT_FOUND", "This announcement was withdrawn")
		return
	}
	if a.Kind == KindPromotion && h.S.Unsubscribed(u) {
		fail(c, http.StatusNotFound, "UNSUBSCRIBED", "You unsubscribed from DASYIN offers")
		return
	}
	ok(c, itemOf(&a, &d, Now()))
}

func (h *Handler) mark(c *gin.Context, what string) {
	u, okk := h.who(c)
	if !okk {
		return
	}
	d, err := h.S.Mark(u, c.Param("id"), what)
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "This announcement is no longer available")
		return
	}
	ok(c, gin.H{"id": d.ID, "read_at": d.ReadAt, "dismissed_at": d.DismissedAt, "clicked_at": d.ClickedAt})
}

func (h *Handler) myPrefs(c *gin.Context) {
	u, okk := h.who(c)
	if !okk {
		return
	}
	ok(c, gin.H{"promotions": !h.S.Unsubscribed(u)})
}

func (h *Handler) saveMyPrefs(c *gin.Context) {
	u, okk := h.who(c)
	if !okk {
		return
	}
	var in struct {
		Promotions *bool `json:"promotions"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Promotions == nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Say whether you want DASYIN offers")
		return
	}
	var err error
	if *in.Promotions {
		err = h.S.Resubscribe(u)
	} else {
		err = h.S.Unsubscribe(u, "", "app")
	}
	if err != nil {
		failErr(c, err, "Could not save")
		return
	}
	platform.Audit(h.S.DB, c, "", "announcement.promotions_"+map[bool]string{true: "on", false: "off"}[*in.Promotions], "user", u, nil, gin.H{"promotions": *in.Promotions})
	ok(c, gin.H{"promotions": *in.Promotions})
}

// ---------------------------------------------------------------- public

// open is the email's tracked button: read + clicked, then on to the target.
func (h *Handler) open(c *gin.Context) {
	id, target, err := h.S.ParseToken(c.Query("t"), PurposeOpen)
	if err != nil {
		c.Redirect(http.StatusFound, h.S.appURL()+"/dashboard")
		return
	}
	var d Delivery
	if fresh(h.S.DB).First(&d, "id = ?", id).Error != nil {
		c.Redirect(http.StatusFound, h.S.appURL()+"/dashboard")
		return
	}
	var a Announcement
	if fresh(h.S.DB).First(&a, "id = ?", d.AnnouncementID).Error != nil {
		c.Redirect(http.StatusFound, h.S.appURL()+"/dashboard")
		return
	}
	if _, err := h.S.Mark(d.UserID, d.ID, "click"); err != nil {
		c.Redirect(http.StatusFound, h.S.appURL()+"/dashboard")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, h.S.ResolveTarget(&a, &d, target))
}

func (h *Handler) tokenDelivery(c *gin.Context, tok string) (*Delivery, bool) {
	id, _, err := h.S.ParseToken(tok, PurposeUnsub)
	if err != nil {
		fail(c, http.StatusBadRequest, "BAD_LINK", err.Error())
		return nil, false
	}
	var d Delivery
	if fresh(h.S.DB).First(&d, "id = ?", id).Error != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "This unsubscribe link is no longer valid")
		return nil, false
	}
	return &d, true
}

func (h *Handler) unsubInfo(c *gin.Context) {
	d, okk := h.tokenDelivery(c, c.Query("t"))
	if !okk {
		return
	}
	ok(c, gin.H{"email": MaskEmail(d.Email), "unsubscribed": h.S.Unsubscribed(d.UserID)})
}

func (h *Handler) unsubscribe(c *gin.Context) {
	var in struct {
		T string `json:"t"`
	}
	_ = c.ShouldBindJSON(&in)
	if in.T == "" {
		in.T = c.Query("t")
	}
	d, okk := h.tokenDelivery(c, in.T)
	if !okk {
		return
	}
	if err := h.S.Unsubscribe(d.UserID, d.ID, "email"); err != nil {
		failErr(c, err, "Could not unsubscribe — please try again")
		return
	}
	platform.Audit(h.S.DB, c, d.OrganizationID, "announcement.unsubscribe", "user", d.UserID, nil, gin.H{"announcement_id": d.AnnouncementID, "via": "email link"})
	ok(c, gin.H{"email": MaskEmail(d.Email), "unsubscribed": true})
}

func (h *Handler) resubscribe(c *gin.Context) {
	var in struct {
		T string `json:"t"`
	}
	_ = c.ShouldBindJSON(&in)
	d, okk := h.tokenDelivery(c, in.T)
	if !okk {
		return
	}
	if err := h.S.Resubscribe(d.UserID); err != nil {
		failErr(c, err, "Could not resubscribe — please try again")
		return
	}
	platform.Audit(h.S.DB, c, d.OrganizationID, "announcement.resubscribe", "user", d.UserID, nil, gin.H{"via": "email link"})
	ok(c, gin.H{"email": MaskEmail(d.Email), "unsubscribed": false})
}
