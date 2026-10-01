package recurbills

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
)

// Handler serves /api/v1/finance/recurring… and /finance/bill-reminders….
type Handler struct{ S *Service }

// RegisterRoutes mounts the routes on the authenticated group.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/finance/recurring")
	g.GET("", h.list)
	g.POST("", h.create)
	g.POST("/run", h.run)
	g.GET("/upcoming", h.upcoming)
	g.GET("/subscriptions", h.subscriptions)
	g.POST("/from-subscription", h.fromSubscription)
	g.GET("/:id", h.get)
	g.PUT("/:id", h.update)
	g.DELETE("/:id", h.del)
	g.POST("/:id/pause", h.pause)
	g.POST("/:id/resume", h.resume)
	g.POST("/:id/occurrences/:period/generate", h.generate)
	g.POST("/:id/occurrences/:period/skip", h.skip)
	g.DELETE("/:id/occurrences/:period/skip", h.unskip)
	g.POST("/:id/occurrences/:period/paid", h.paid)

	r := auth.Group("/finance/bill-reminders")
	r.GET("/settings", h.getSettings)
	r.PUT("/settings", h.saveSettings)
	r.GET("/log", h.log)
	r.POST("/snooze", h.snooze)
	r.GET("/digest", h.digestPreview)
	r.POST("/digest", h.digestSend)
}

func orgOf(c *gin.Context) string {
	if v := c.GetString("organization_id"); v != "" {
		return v
	}
	if v, ok := c.Get("org_id"); ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func (h *Handler) org(c *gin.Context) (string, bool) {
	o := orgOf(c)
	if o == "" {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "No organisation is associated with this account"})
		return "", false
	}
	return o, true
}

// IsAdmin reports whether a role manages the organisation.
func IsAdmin(role string) bool {
	switch role {
	case "owner", "admin", "org_admin", "super_admin", "manager":
		return true
	}
	return false
}

func (h *Handler) admin(c *gin.Context) (string, bool) {
	o, ok := h.org(c)
	if !ok {
		return "", false
	}
	if !IsAdmin(c.GetString("user_role")) {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "Only admins and managers can do this"})
		return "", false
	}
	return o, true
}

func ok(c *gin.Context, data any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": data}) }

func fail(c *gin.Context, err error) {
	var ve *ValidationError
	var fe *finance.ValidationError
	switch {
	case errors.As(err, &ve):
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": ve.Msg})
	case errors.As(err, &fe):
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": fe.Msg})
	case errors.Is(err, ErrNotFound), errors.Is(err, finance.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "Not found"})
	default:
		log.Printf("recurbills: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Something went wrong — please try again"})
	}
}

func bind(c *gin.Context, v any) bool {
	if err := c.ShouldBindJSON(v); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Invalid request: " + err.Error()})
		return false
	}
	return true
}

func user(c *gin.Context) string { return nz(c.GetString("user_id"), "system") }

func (h *Handler) list(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	ok(c, h.S.List(o))
}

func (h *Handler) get(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	r, err := h.S.Get(o, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	var occs []Occurrence
	h.S.DB.Where("recurring_id = ?", r.ID).Order("period DESC").Limit(24).Find(&occs)
	ok(c, gin.H{"schedule": r, "occurrences": occs})
}

func (h *Handler) create(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in Input
	if !bind(c, &in) {
		return
	}
	r, err := h.S.Create(o, user(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": r})
}

func (h *Handler) update(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in Input
	if !bind(c, &in) {
		return
	}
	r, err := h.S.Update(o, c.Param("id"), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, r)
}

func (h *Handler) del(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	if err := h.S.Delete(o, c.Param("id")); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) pause(c *gin.Context) { h.setPaused(c, true) }

func (h *Handler) resume(c *gin.Context) { h.setPaused(c, false) }

func (h *Handler) setPaused(c *gin.Context, p bool) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	r, err := h.S.SetPaused(o, c.Param("id"), p)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, r)
}

func (h *Handler) generate(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	b, err := h.S.GenerateNow(o, user(c), c.Param("id"), c.Param("period"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, b)
}

type noteIn struct {
	Note string `json:"note"`
}

func (h *Handler) skip(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in noteIn
	_ = c.ShouldBindJSON(&in)
	if err := h.S.Skip(o, user(c), c.Param("id"), c.Param("period"), in.Note); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"skipped": true})
}

func (h *Handler) unskip(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	if err := h.S.Unskip(o, c.Param("id"), c.Param("period")); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"skipped": false})
}

func (h *Handler) paid(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in noteIn
	_ = c.ShouldBindJSON(&in)
	if err := h.S.MarkPaid(o, user(c), c.Param("id"), c.Param("period"), in.Note); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"paid": true})
}

func (h *Handler) upcoming(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	days, _ := strconv.Atoi(c.DefaultQuery("days", "90"))
	ok(c, h.S.UpcomingFor(o, days))
}

func (h *Handler) subscriptions(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	ok(c, h.S.Subscriptions(o))
}

func (h *Handler) fromSubscription(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in struct {
		Key string `json:"key"`
		Input
	}
	if !bind(c, &in) {
		return
	}
	r, err := h.S.FromSubscription(o, user(c), in.Key, &in.Input)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": r})
}

// run is "Run now": generate due bills and send due reminders immediately
// (ignoring the send hour). Admins only.
func (h *Handler) run(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	r, got := h.S.RunOrgLocked(c.Request.Context(), o, RunOptions{Force: true})
	if !got {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "A run is already in progress — try again in a moment"})
		return
	}
	ok(c, r)
}

func (h *Handler) getSettings(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	st := h.S.SettingsFor(o)
	cfg, _ := h.S.Mail.Resolve(o)
	ok(c, gin.H{"settings": st, "recipients_preview": h.S.Recipients(o, st, nil), "email_ready": cfg.Ready(),
		"notification_recipients": cfg.Notify, "timezone": h.S.Loc(o).String()})
}

func (h *Handler) saveSettings(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in SettingsInput
	if !bind(c, &in) {
		return
	}
	st, err := h.S.SaveSettings(o, in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"settings": st, "recipients_preview": h.S.Recipients(o, st, nil)})
}

func (h *Handler) log(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	n, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	ok(c, h.S.Log(o, n))
}

func (h *Handler) snooze(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in struct {
		BillID      string `json:"bill_id"`
		RecurringID string `json:"recurring_id"`
		Period      string `json:"period"`
		Days        int    `json:"days"`
		Until       string `json:"until"`
	}
	if !bind(c, &in) {
		return
	}
	if in.BillID == "" && (in.RecurringID == "" || in.Period == "") {
		fail(c, invalid("choose the bill to snooze"))
		return
	}
	u, err := h.S.Snooze(o, in.BillID, in.RecurringID, in.Period, in.Days, in.Until)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"snoozed_until": u.Format(dayFmt)})
}

func (h *Handler) digestPreview(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	ok(c, h.S.Digest(o))
}

func (h *Handler) digestSend(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	status, err := h.S.SendDigest(c.Request.Context(), o, "", "manual")
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "error": err.Error(), "status": status})
		return
	}
	ok(c, gin.H{"status": status})
}
