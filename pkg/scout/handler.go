package scout

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Handler serves /api/v1/insights/scout/….
type Handler struct{ S *Service }

// RegisterRoutes mounts the routes on the authenticated API group.
//
//	GET    /insights/scout                         overview: settings, key, model, estimate, spend, next scans, counts
//	PUT    /insights/scout/settings                {profile?, focus?, schedule fields?, monthly_budget_usd? (super admin)}
//	POST   /insights/scout/profile/refresh         rebuild the profile from the data (keeps the description)
//	POST   /insights/scout/estimate                {daily_*, weekly_*, monthly_budget_usd} → monthly cost estimate (not saved)
//	POST   /insights/scout/enable                  {confirm_cost: true, monthly_budget_usd, estimate_usd} (super admin)
//	POST   /insights/scout/disable
//	POST   /insights/scout/scan                    {kind: light|deep, wait_seconds?} → the run (status running unless refused)
//	GET    /insights/scout/opportunities           ?status&type&country&deadline&q&sort&limit&offset
//	GET    /insights/scout/opportunities/:id
//	PUT    /insights/scout/opportunities/:id       {saved?, notes?, restore?}
//	POST   /insights/scout/opportunities/:id/dismiss {reason, note}
//	POST   /insights/scout/opportunities/:id/remind  {date: YYYY-MM-DD | null}
//	POST   /insights/scout/opportunities/:id/pursue  {as: deal|lead}
//	GET    /insights/scout/markets                 ?days — market signals by country
//	DELETE /insights/scout/markets/:id             hide a signal
//	GET    /insights/scout/runs                    history with searches, tokens and cost; ?limit
//	GET    /insights/scout/runs/:id                one run with its prompt and queries
//	POST   /insights/scout/test/clock              {now: RFC3339 | null} (SCOUT_TEST_CLOCK only)
//	POST   /insights/scout/test/run-due            run the scheduler once and wait (SCOUT_TEST_CLOCK only)
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/insights/scout")
	g.GET("", h.status)
	g.PUT("/settings", h.saveSettings)
	g.POST("/profile/refresh", h.refreshProfile)
	g.POST("/estimate", h.estimate)
	g.POST("/enable", h.enable)
	g.POST("/disable", h.disable)
	g.POST("/scan", h.scan)
	g.GET("/opportunities", h.list)
	g.GET("/opportunities/:id", h.get)
	g.PUT("/opportunities/:id", h.update)
	g.POST("/opportunities/:id/dismiss", h.dismiss)
	g.POST("/opportunities/:id/remind", h.remind)
	g.POST("/opportunities/:id/pursue", h.pursue)
	g.GET("/markets", h.markets)
	g.DELETE("/markets/:id", h.hideSignal)
	g.GET("/runs", h.runs)
	g.GET("/runs/:id", h.run)
	if h.S.TestClock {
		g.POST("/test/clock", h.testClock)
		g.POST("/test/run-due", h.testRunDue)
	}
}

func ok(c *gin.Context, v any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": v}) }

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func orgOf(c *gin.Context) (string, bool) {
	for _, k := range []string{"organization_id", "org_id"} {
		if v := strings.TrimSpace(c.GetString(k)); v != "" {
			return v, true
		}
	}
	fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
	return "", false
}

func role(c *gin.Context) string { return strings.ToLower(c.GetString("user_role")) }

func canEdit(c *gin.Context) bool {
	switch role(c) {
	case "super_admin", "admin", "owner", "manager", "org_admin":
		return true
	}
	return false
}

func isSuper(c *gin.Context) bool { return role(c) == "super_admin" }

func (h *Handler) editor(c *gin.Context, what string) bool {
	if canEdit(c) {
		return true
	}
	fail(c, http.StatusForbidden, "FORBIDDEN", "Only admins and managers can "+what+".")
	return false
}

func (h *Handler) userName(c *gin.Context) string {
	id := c.GetString("user_id")
	if id == "" {
		return ""
	}
	var n struct{ FirstName, LastName, Email string }
	h.S.DB.Raw(`SELECT COALESCE(first_name,'') AS first_name, COALESCE(last_name,'') AS last_name, COALESCE(email,'') AS email FROM users WHERE id = ?`, id).Scan(&n)
	if s := strings.TrimSpace(n.FirstName + " " + n.LastName); s != "" {
		return s
	}
	return n.Email
}

func (h *Handler) err(c *gin.Context, err error) {
	var ie *InputError
	switch {
	case errors.As(err, &ie):
		fail(c, http.StatusBadRequest, "VALIDATION", ie.Msg)
	case errors.Is(err, ErrNotFound):
		fail(c, http.StatusNotFound, "NOT_FOUND", "Not found")
	case errors.Is(err, ErrForbidden):
		fail(c, http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Only a super admin can change the budget: scans are paid with the platform's Anthropic key.")
	case errors.Is(err, ErrBusy):
		fail(c, http.StatusConflict, "SCAN_RUNNING", "A scan is already running. It usually takes a minute or two.")
	default:
		fail(c, http.StatusInternalServerError, "ERROR", "Something went wrong")
	}
}

func (h *Handler) status(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	st, err := h.S.StatusFor(org)
	if err != nil {
		h.err(c, err)
		return
	}
	st.CanEdit, st.CanEnable = canEdit(c), isSuper(c)
	ok(c, st)
}

func (h *Handler) saveSettings(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !h.editor(c, "set up the market scout") {
		return
	}
	var p SettingsPatch
	if err := c.ShouldBindJSON(&p); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	if _, err := h.S.SaveSettings(org, c.GetString("user_id"), isSuper(c), p); err != nil {
		h.err(c, err)
		return
	}
	h.status(c)
}

func (h *Handler) refreshProfile(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !h.editor(c, "refresh the business profile") {
		return
	}
	st, err := h.S.RefreshProfile(org, c.GetString("user_id"))
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"profile": st.Profile, "profile_built_at": st.ProfileBuiltAt})
}

func (h *Handler) estimate(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	st, err := h.S.Settings(org)
	if err != nil {
		h.err(c, err)
		return
	}
	var p SettingsPatch
	_ = c.ShouldBindJSON(&p)
	if p.DailyEnabled != nil {
		st.DailyEnabled = *p.DailyEnabled
	}
	if p.DailySearches != nil && *p.DailySearches >= 1 && *p.DailySearches <= MaxSearchesLight {
		st.DailySearches = *p.DailySearches
	}
	if p.WeeklyEnabled != nil {
		st.WeeklyEnabled = *p.WeeklyEnabled
	}
	if p.WeeklySearches != nil && *p.WeeklySearches >= 1 && *p.WeeklySearches <= MaxSearchesDeep {
		st.WeeklySearches = *p.WeeklySearches
	}
	if p.MonthlyBudgetUSD != nil && *p.MonthlyBudgetUSD > 0 {
		st.MonthlyBudgetUSD = *p.MonthlyBudgetUSD
	}
	ok(c, EstimateFor(h.S.ai().Model, st))
}

func (h *Handler) enable(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !isSuper(c) {
		fail(c, http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Only a super admin can switch the market scout on: scans are paid with the platform's Anthropic key.")
		return
	}
	var in struct {
		ConfirmCost bool    `json:"confirm_cost"`
		Budget      float64 `json:"monthly_budget_usd"`
		Estimate    float64 `json:"estimate_usd"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	if !in.ConfirmCost {
		fail(c, http.StatusBadRequest, "CONFIRM_COST", "Confirm the estimated monthly cost to switch the market scout on.")
		return
	}
	if _, err := h.S.Enable(org, c.GetString("user_id"), in.Budget, in.Estimate); err != nil {
		h.err(c, err)
		return
	}
	h.status(c)
}

func (h *Handler) disable(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !h.editor(c, "switch the market scout off") {
		return
	}
	if _, err := h.S.Disable(org, c.GetString("user_id")); err != nil {
		h.err(c, err)
		return
	}
	h.status(c)
}

func (h *Handler) scan(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !h.editor(c, "run a scan") {
		return
	}
	var in struct {
		Kind        string `json:"kind"`
		WaitSeconds int    `json:"wait_seconds"`
	}
	_ = c.ShouldBindJSON(&in)
	trigger := "manual"
	if c.GetHeader("X-Requested-By") == "dasyin-ai" {
		trigger = "ai"
	}
	run, err := h.S.Start(org, StartOpts{Kind: in.Kind, Trigger: trigger, By: c.GetString("user_id"), ByName: h.userName(c)})
	if err != nil {
		h.err(c, err)
		return
	}
	if in.WaitSeconds > 0 && run.Status == RunRunning {
		if in.WaitSeconds > 55 {
			in.WaitSeconds = 55
		}
		if r, err := h.S.WaitRun(c.Request.Context(), org, run.ID, time.Duration(in.WaitSeconds)*time.Second); err == nil {
			run = r
		}
	}
	run.Prompt, run.AnswerText = "", ""
	ok(c, run)
}

func (h *Handler) list(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	n := func(k string) int { v, _ := strconv.Atoi(c.Query(k)); return v }
	f := Filter{Status: c.Query("status"), Type: c.Query("type"), Country: c.Query("country"), Deadline: c.Query("deadline"), Q: c.Query("q"),
		Sort: c.Query("sort"), Limit: n("limit"), Offset: n("offset")}
	feed, err := h.S.List(org, f)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"feed": feed, "can_edit": canEdit(c)})
}

func (h *Handler) get(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	o, err := h.S.Get(org, c.Param("id"))
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, o)
}

func (h *Handler) update(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !h.editor(c, "change opportunities") {
		return
	}
	var p ItemPatch
	if err := c.ShouldBindJSON(&p); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	o, err := h.S.Update(org, c.Param("id"), p)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, o)
}

func (h *Handler) dismiss(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !h.editor(c, "dismiss opportunities") {
		return
	}
	var in struct {
		Reason string `json:"reason"`
		Note   string `json:"note"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	o, err := h.S.Dismiss(org, c.Param("id"), c.GetString("user_id"), in.Reason, in.Note)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, o)
}

func (h *Handler) remind(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !h.editor(c, "set reminders") {
		return
	}
	var in struct {
		Date *string `json:"date"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	var on *time.Time
	if in.Date != nil && strings.TrimSpace(*in.Date) != "" {
		d, err := time.Parse("2006-01-02", strings.TrimSpace(*in.Date))
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", "The date must be YYYY-MM-DD.")
			return
		}
		on = &d
	}
	o, err := h.S.Remind(org, c.Param("id"), c.GetString("user_id"), on)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, o)
}

func (h *Handler) pursue(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !h.editor(c, "pursue opportunities") {
		return
	}
	var in struct {
		As string `json:"as"`
	}
	_ = c.ShouldBindJSON(&in)
	r, err := h.S.Pursue(org, c.Param("id"), c.GetString("user_id"), role(c), in.As)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, r)
}

func (h *Handler) markets(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	days, _ := strconv.Atoi(c.Query("days"))
	g, err := h.S.Markets(org, days)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"markets": g, "can_edit": canEdit(c)})
}

func (h *Handler) hideSignal(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !h.editor(c, "hide market signals") {
		return
	}
	if err := h.S.HideSignal(org, c.Param("id")); err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"hidden": true})
}

func (h *Handler) runs(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	n, _ := strconv.Atoi(c.Query("limit"))
	r, err := h.S.Runs(org, n)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, r)
}

func (h *Handler) run(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	r, err := h.S.GetRun(org, c.Param("id"))
	if err != nil {
		h.err(c, err)
		return
	}
	r.AnswerText = ""
	ok(c, r)
}

func (h *Handler) testClock(c *gin.Context) {
	if !canEdit(c) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Admins only")
		return
	}
	var in struct {
		Now *string `json:"now"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	var at *time.Time
	if in.Now != nil && *in.Now != "" {
		t, err := time.Parse(time.RFC3339, *in.Now)
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", "now is RFC3339, e.g. 2026-11-23T03:35:00Z")
			return
		}
		at = &t
	}
	ok(c, gin.H{"now": h.S.SetClock(at)})
}

func (h *Handler) testRunDue(c *gin.Context) {
	if !canEdit(c) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Admins only")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ok(c, h.S.RunScheduled(ctx, true))
}
