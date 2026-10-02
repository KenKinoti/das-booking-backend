package insights

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
)

// IntelHandler serves the industry watch, trade & exports and strategy brief.
type IntelHandler struct{ I *Intel }

// RegisterRoutes mounts the routes on the authenticated API group.
//
//	GET    /insights/watch                    stored items (?topic&country&source&saved=1&unread=1&dismissed=1&q&days&sort&limit&offset)
//	GET    /insights/watch/latest             dashboard card: top items by relevance (?n=3)
//	POST   /insights/watch/refresh            fetch the enabled sources now ({feed_id} for one)
//	PUT    /insights/watch/items/:id          {saved, dismissed, read}
//	POST   /insights/watch/read-all
//	GET    /insights/watch/feeds              sources, suggested sources, topics
//	POST   /insights/watch/feeds/preview      {url} → validates and returns the latest 3 items
//	POST   /insights/watch/feeds              {url | catalog_key, title, country, topics}
//	POST   /insights/watch/feeds/suggested    {keys} → check and add catalogue sources
//	PUT    /insights/watch/feeds/:id          {title, country, topics, enabled}
//	DELETE /insights/watch/feeds/:id
//	GET/PUT/DELETE /insights/watch/rules      keyword rules (PUT {rules}; DELETE resets)
//	GET    /insights/trade                    trade & exports (?from&to)
//	GET    /insights/trade/documents          drill-down (?country|currency, open=1)
//	GET    /insights/trade/markets            target markets (?refresh=1)
//	PUT/DELETE /insights/trade/markets        {countries, weights}; DELETE resets
//	GET    /insights/trade/export/:dataset    CSV
//	GET    /insights/briefs                   history + settings
//	GET    /insights/briefs/:id
//	GET    /insights/briefs/preview           the rule-based brief of ?period, computed now and not stored
//	POST   /insights/briefs/generate          {period: "YYYY-MM" | "current" | ""}
//	POST   /insights/briefs/:id/ai            write the AI version from the same data
//	POST   /insights/briefs/:id/email         send to the administrators
//	DELETE /insights/briefs/:id
//	PUT    /insights/briefs/settings          {brief_auto, brief_email}
func (h *IntelHandler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/insights")
	g.GET("/watch", h.watchList)
	g.GET("/watch/latest", h.watchLatest)
	g.POST("/watch/refresh", h.watchRefresh)
	g.PUT("/watch/items/:id", h.watchItem)
	g.POST("/watch/read-all", h.watchReadAll)
	g.GET("/watch/feeds", h.watchFeeds)
	g.POST("/watch/feeds/preview", h.watchPreview)
	g.POST("/watch/feeds/suggested", h.watchAddSuggested)
	g.POST("/watch/feeds", h.watchAddFeed)
	g.PUT("/watch/feeds/:id", h.watchUpdateFeed)
	g.DELETE("/watch/feeds/:id", h.watchDeleteFeed)
	g.GET("/watch/rules", h.watchRules)
	g.PUT("/watch/rules", h.watchSaveRules)
	g.DELETE("/watch/rules", h.watchResetRules)

	g.GET("/trade", h.trade)
	g.GET("/trade/documents", h.tradeDocs)
	g.GET("/trade/markets", h.markets)
	g.PUT("/trade/markets", h.saveMarkets)
	g.DELETE("/trade/markets", h.resetMarkets)
	g.GET("/trade/export/:dataset", h.tradeExport)

	g.GET("/briefs", h.briefs)
	g.PUT("/briefs/settings", h.briefSettings)
	g.POST("/briefs/generate", h.briefGenerate)
	g.GET("/briefs/preview", h.briefPreview)
	g.GET("/briefs/:id", h.brief)
	g.POST("/briefs/:id/ai", h.briefAI)
	g.POST("/briefs/:id/email", h.briefEmail)
	g.DELETE("/briefs/:id", h.briefDelete)
}

func (h *IntelHandler) editor(c *gin.Context, what string) bool {
	if canEdit(c) {
		return true
	}
	fail(c, http.StatusForbidden, "FORBIDDEN", "Only admins and managers can "+what)
	return false
}

func feedErr(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Source not found")
		return
	}
	var fe *FeedError
	if errors.As(err, &fe) {
		fail(c, http.StatusBadRequest, "FEED_UNREADABLE", fe.Msg)
		return
	}
	fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
}

// ---------- industry watch ----------

func (h *IntelHandler) watchList(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	n := func(k string) int { v, _ := strconv.Atoi(c.Query(k)); return v }
	f := WatchFilter{Topic: c.Query("topic"), Country: c.Query("country"), FeedID: c.Query("source"), Q: c.Query("q"), Saved: c.Query("saved") == "1",
		Unread: c.Query("unread") == "1", Dismissed: c.Query("dismissed") == "1", Days: n("days"), Sort: c.Query("sort"), Limit: n("limit"), Offset: n("offset")}
	if f.Days < 0 || f.Days > 3650 {
		f.Days = 0
	}
	l := h.I.List(org, f)
	ok(c, gin.H{"list": l, "can_edit": canEdit(c)})
}

func (h *IntelHandler) watchLatest(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	n, _ := strconv.Atoi(c.Query("n"))
	ok(c, h.I.Latest(org, n))
}

func (h *IntelHandler) watchRefresh(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "refresh the sources") {
		return
	}
	var in struct {
		FeedID string `json:"feed_id"`
	}
	_ = c.ShouldBindJSON(&in)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 100*time.Second)
	defer cancel()
	sum := h.I.Refresh(ctx, org, strings.TrimSpace(in.FeedID))
	if sum.Busy {
		fail(c, http.StatusConflict, "BUSY", "A refresh is already running — try again in a minute")
		return
	}
	ok(c, sum)
}

func (h *IntelHandler) watchItem(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var p ItemPatch
	if err := c.ShouldBindJSON(&p); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	it, err := h.I.SetItem(org, c.Param("id"), p)
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Item not found")
		return
	}
	ok(c, it)
}

func (h *IntelHandler) watchReadAll(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	ok(c, gin.H{"marked": h.I.MarkAllRead(org)})
}

func (h *IntelHandler) feedsPayload(org string, c *gin.Context) gin.H {
	st := h.I.Settings(org)
	return gin.H{"feeds": h.I.Feeds(org), "suggested": h.I.Suggestions(org), "groups": WatchGroups, "topics": h.I.WatchRules(org), "max": MaxWatchFeeds,
		"last_run": st.WatchLastRun, "last_status": st.WatchLastStatus, "can_edit": canEdit(c), "countries": CountriesSorted(),
		"note": "Suggested sources are checked when you add them: one that does not answer with a readable RSS or Atom feed is not added."}
}

func (h *IntelHandler) watchFeeds(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	h.I.EnsureSeeded(org)
	ok(c, h.feedsPayload(org, c))
}

func (h *IntelHandler) watchPreview(c *gin.Context) {
	if _, okk := orgOf(c); !okk {
		return
	}
	if !h.editor(c, "add sources") {
		return
	}
	var in struct {
		URL string `json:"url"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	p, _, err := h.I.PreviewFeed(ctx, in.URL)
	if err != nil {
		feedErr(c, err)
		return
	}
	ok(c, p)
}

func (h *IntelHandler) watchAddFeed(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "add sources") {
		return
	}
	var in FeedInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	f, run, err := h.I.AddFeed(ctx, org, c.GetString("user_id"), in)
	if err != nil {
		feedErr(c, err)
		return
	}
	ok(c, gin.H{"feed": f, "run": run})
}

func (h *IntelHandler) watchAddSuggested(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "add sources") {
		return
	}
	var in struct {
		Keys []string `json:"keys"`
	}
	_ = c.ShouldBindJSON(&in)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 100*time.Second)
	defer cancel()
	ok(c, h.I.AddSuggested(ctx, org, c.GetString("user_id"), in.Keys))
}

func (h *IntelHandler) watchUpdateFeed(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "change sources") {
		return
	}
	var in FeedPatch
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	f, err := h.I.UpdateFeed(ctx, org, c.Param("id"), in)
	if err != nil {
		feedErr(c, err)
		return
	}
	ok(c, f)
}

func (h *IntelHandler) watchDeleteFeed(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "remove sources") {
		return
	}
	if err := h.I.DeleteFeed(org, c.Param("id")); err != nil {
		feedErr(c, err)
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *IntelHandler) rulesPayload(org string, c *gin.Context) gin.H {
	var n int64
	h.I.DB.Model(&WatchRule{}).Where("organization_id = ?", org).Count(&n)
	return gin.H{"rules": h.I.WatchRules(org), "defaults": n == 0, "formula": WatchFormulaDoc, "context": h.I.Context(org), "can_edit": canEdit(c)}
}

func (h *IntelHandler) watchRules(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	ok(c, h.rulesPayload(org, c))
}

func (h *IntelHandler) watchSaveRules(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "change the keyword rules") {
		return
	}
	var in struct {
		Rules []WatchRule `json:"rules"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	if _, err := h.I.SaveWatchRules(org, in.Rules); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	p := h.rulesPayload(org, c)
	p["rescored"] = h.I.Rescore(org)
	ok(c, p)
}

func (h *IntelHandler) watchResetRules(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "change the keyword rules") {
		return
	}
	h.I.ResetWatchRules(org)
	p := h.rulesPayload(org, c)
	p["rescored"] = h.I.Rescore(org)
	ok(c, p)
}

// ---------- trade & exports ----------

func (h *IntelHandler) tradeOpts(c *gin.Context) (Opts, bool) {
	r, err := ParseRange(reqtz.Now(c), c.Query("from"), c.Query("to"), 12)
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_RANGE", err.Error())
		return Opts{}, false
	}
	return Opts{Range: r, Today: day(reqtz.Now(c))}, true
}

func (h *IntelHandler) trade(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	o, good := h.tradeOpts(c)
	if !good {
		return
	}
	o.Range = h.I.clampAll(org, o.Range)
	ok(c, h.I.TradeFor(org, o))
}

func (h *IntelHandler) tradeDocs(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	o, good := h.tradeOpts(c)
	if !good {
		return
	}
	o.Range = h.I.clampAll(org, o.Range)
	docs := h.I.TradeDocs(org, o, TradeDocsFilter{Country: c.Query("country"), Currency: c.Query("currency"), Open: c.Query("open") == "1"})
	ok(c, gin.H{"documents": docs, "count": len(docs), "currency": h.I.Home(org), "from": o.Range.From.Format(dayFmt), "to": o.Range.To.Format(dayFmt), "capped": len(docs) >= 500})
}

func (h *IntelHandler) markets(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	m := h.I.MarketsFor(c.Request.Context(), org, c.Query("refresh") == "1")
	ok(c, gin.H{"markets": m, "can_edit": canEdit(c)})
}

func (h *IntelHandler) saveMarkets(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "change the target markets") {
		return
	}
	var in struct {
		Countries []string           `json:"countries"`
		Weights   map[string]float64 `json:"weights"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	if err := h.I.SaveMarketConfig(org, in.Countries, in.Weights); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", capFirst(err.Error()))
		return
	}
	ok(c, gin.H{"markets": h.I.MarketsFor(c.Request.Context(), org, false), "can_edit": true})
}

func (h *IntelHandler) resetMarkets(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "change the target markets") {
		return
	}
	if err := h.I.ResetMarketConfig(org); err != nil {
		fail(c, http.StatusInternalServerError, "DB", "Could not reset the target markets")
		return
	}
	ok(c, gin.H{"markets": h.I.MarketsFor(c.Request.Context(), org, false), "can_edit": true})
}

func (h *IntelHandler) tradeExport(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	key := strings.TrimSuffix(c.Param("dataset"), ".csv")
	if !TradeDataset(key) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Unknown dataset")
		return
	}
	o, good := h.tradeOpts(c)
	if !good {
		return
	}
	o.Range = h.I.clampAll(org, o.Range)
	var (
		t    *Trade
		docs []TradeDoc
		m    *Markets
	)
	name := fmt.Sprintf("trade_%s_%s_%s.csv", key, o.Range.From.Format(dayFmt), o.Range.To.Format(dayFmt))
	switch key {
	case "markets":
		m = h.I.MarketsFor(c.Request.Context(), org, false)
		name = fmt.Sprintf("target_markets_%s.csv", o.Today.Format(dayFmt))
	case "documents":
		t = &Trade{Currency: h.I.Home(org)}
		docs = h.I.TradeDocs(org, o, TradeDocsFilter{Country: c.Query("country"), Currency: c.Query("currency"), Open: c.Query("open") == "1"})
	default:
		t = h.I.TradeFor(org, o)
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	c.Status(http.StatusOK)
	if err := WriteTradeCSV(c.Writer, key, t, docs, m); err != nil {
		log.Printf("insights trade export %s: %v", key, err)
	}
}

// ---------- strategy brief ----------

func (h *IntelHandler) briefsPayload(org string, c *gin.Context) gin.H {
	st := h.I.Settings(org)
	today := day(reqtz.Now(c))
	mailReady := false
	if h.I.Mail != nil {
		cfg, _ := h.I.Mail.Resolve(org)
		mailReady = cfg.Ready()
	}
	return gin.H{"briefs": h.I.Briefs(org), "settings": gin.H{"brief_auto": st.BriefAuto, "brief_email": st.BriefEmail}, "ai_enabled": AIEnabled(),
		"recipients": h.I.BriefRecipients(org), "mail_ready": mailReady, "can_edit": canEdit(c),
		"last_month": monthStart(today).AddDate(0, -1, 0).Format("2006-01"), "this_month": today.Format("2006-01")}
}

func (h *IntelHandler) briefs(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	ok(c, h.briefsPayload(org, c))
}

func (h *IntelHandler) brief(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	b := h.I.BriefByID(org, c.Param("id"))
	if b == nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Brief not found")
		return
	}
	ok(c, b)
}

func (h *IntelHandler) briefPreview(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	now := reqtz.Now(c)
	period := strings.TrimSpace(c.Query("period"))
	if strings.EqualFold(period, "current") {
		period = now.Format("2006-01")
	}
	content, err := h.I.BuildBrief(c.Request.Context(), org, period, now, reqtz.Location(c))
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", capFirst(err.Error()))
		return
	}
	ok(c, gin.H{"period": content.Period, "generated_by": "rule", "stored": false, "title": "Strategy brief — " + content.PeriodLabel, "headline": content.Headline(), "content": content})
}

func (h *IntelHandler) briefGenerate(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "generate a brief") {
		return
	}
	var in struct {
		Period string `json:"period"`
	}
	_ = c.ShouldBindJSON(&in)
	now := reqtz.Now(c)
	period := strings.TrimSpace(in.Period)
	if strings.EqualFold(period, "current") {
		period = now.Format("2006-01")
	}
	b, err := h.I.GenerateBrief(c.Request.Context(), org, c.GetString("user_id"), period, "manual", now, reqtz.Location(c))
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", capFirst(err.Error()))
		return
	}
	ok(c, b)
}

func (h *IntelHandler) briefAI(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "write a brief with AI") {
		return
	}
	if !AIEnabled() {
		fail(c, http.StatusServiceUnavailable, "AI_DISABLED", "AI is not set up — add an Anthropic API key in System settings → AI assistant.")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 100*time.Second)
	defer cancel()
	b, err := h.I.WriteBriefAI(ctx, org, c.GetString("user_id"), c.Param("id"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Brief not found")
		return
	}
	if err != nil {
		fail(c, http.StatusBadGateway, "AI_FAILED", capFirst(err.Error()))
		return
	}
	ok(c, b)
}

func (h *IntelHandler) briefEmail(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "email a brief") {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 40*time.Second)
	defer cancel()
	to, err := h.I.EmailBrief(ctx, org, c.Param("id"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Brief not found")
		return
	}
	if err != nil {
		fail(c, http.StatusBadRequest, "EMAIL_FAILED", capFirst(err.Error()))
		return
	}
	ok(c, gin.H{"sent_to": to})
}

func (h *IntelHandler) briefDelete(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "delete a brief") {
		return
	}
	if !h.I.DeleteBrief(org, c.Param("id")) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Brief not found")
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *IntelHandler) briefSettings(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !h.editor(c, "change the brief settings") {
		return
	}
	var in struct {
		BriefAuto  *bool `json:"brief_auto"`
		BriefEmail *bool `json:"brief_email"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	h.I.Settings(org)
	up := map[string]any{"updated_at": h.I.now()}
	if in.BriefAuto != nil {
		up["brief_auto"] = *in.BriefAuto
	}
	if in.BriefEmail != nil {
		up["brief_email"] = *in.BriefEmail
	}
	if err := h.I.DB.Model(&IntelSettings{}).Where("organization_id = ?", org).Updates(up).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB", "Could not save the settings")
		return
	}
	ok(c, h.briefsPayload(org, c))
}
