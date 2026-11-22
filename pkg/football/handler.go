package football

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Handler serves the football API.
type Handler struct{ S *Service }

// RegisterRoutes mounts the routes on the authenticated API group.
//
// Everyone with the football module (plan → business type → role; owners
// and managers by default):
//
//	GET    /football/meta                    markets, bookmaker suggestions, settings people need, provider status
//	GET    /football/competitions            enabled competitions with coverage
//	GET    /football/fixtures                ?date&from&to&competition&status&q&limit — model P(H/D/A), best odds, value flags, freshness
//	GET    /football/fixtures/:id/analysis   the match page (see Analysis)
//	GET    /football/fixtures/:id/kelly      ?market&selection&line&odds — model p, EV and ¼-Kelly stake at the odds someone was offered
//	GET    /football/value                   ?date&min_edge — value selections
//	GET    /football/model/performance       ?competition&from&to — Brier, RPS, log loss vs the market, calibration
//	GET    /football/teams/:id               season profile
//	GET    /football/standings/:competition  ?season — table snapshots over time
//	GET    /football/live                    fixtures in play
//	GET    /football/bets                    ?status&market&bookmaker&competition&from&to&limit&offset (own bets)
//	POST   /football/bets                    record a bet (limits and breaks apply)
//	GET    /football/bets/summary            ?from&to — P&L, ROI, yield, strike rate, CLV, breakdowns, bankroll series, drawdown
//	GET    /football/bets/:id
//	PUT    /football/bets/:id
//	DELETE /football/bets/:id
//	POST   /football/bets/:id/settle         {status, cash_out?} manual settlement ("auto" = from the result)
//	GET    /football/bankroll                entries + balance
//	POST   /football/bankroll                {kind: start|deposit|withdrawal, amount, at?, note?}
//	DELETE /football/bankroll/:id
//	GET    /football/limits                  limits, usage, pending raises, break
//	PUT    /football/limits                  {daily_stake, weekly_stake, monthly_stake, daily_loss, weekly_loss, monthly_loss} (null = none)
//	POST   /football/limits/break            {days}
//	GET    /football/watchlist  POST {kind, ref_id, note}  DELETE /football/watchlist/:id
//	GET    /football/odds/manual ?fixture_id  POST {fixture_id, bookmaker, market, selection, line?, price, seen_at?}  DELETE /football/odds/manual/:id
//
// Super admins (the providers' keys are the platform's):
//
//	GET    /super-admin/football/settings           masked keys, plan, budgets, usage per provider/day, competitions, last run
//	PUT    /super-admin/football/settings           SettingsPatch
//	POST   /super-admin/football/settings/test      {provider: apifootball|oddsapi} free connection test
//	GET    /super-admin/football/competitions       ?q&enabled — API-Football's list for the picker
//	POST   /super-admin/football/refresh            {kinds?: [...], force?: bool, refit?: bool} run the scheduler now
//	POST   /super-admin/football/fixtures/:id/refresh  one fixture with everything (1 request; ?parts=true: 4 per-endpoint requests)
//	GET    /super-admin/football/fetch-log          ?provider&limit
//	GET    /super-admin/football/aliases            team aliases;  PUT {name, team_id};  DELETE /aliases/:alias
//	GET    /super-admin/football/unmatched          odds events not matched to a fixture
//	POST   /super-admin/football/test/clock         {now: RFC3339 | null} (FOOTBALL_TEST_CLOCK=1 only)
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/football")
	g.GET("/meta", h.meta)
	g.GET("/competitions", h.competitions)
	g.GET("/fixtures", h.fixtures)
	g.GET("/fixtures/:id/analysis", h.analysis)
	g.GET("/fixtures/:id/kelly", h.kelly)
	g.GET("/value", h.value)
	g.GET("/model/performance", h.performance)
	g.GET("/teams/:id", h.team)
	g.GET("/standings/:competition", h.standings)
	g.GET("/live", h.live)
	g.GET("/bets", h.bets)
	g.POST("/bets", h.createBet)
	g.GET("/bets/summary", h.summary)
	g.GET("/bets/:id", h.bet)
	g.PUT("/bets/:id", h.updateBet)
	g.DELETE("/bets/:id", h.deleteBet)
	g.POST("/bets/:id/settle", h.settleBet)
	g.GET("/bankroll", h.bankroll)
	g.POST("/bankroll", h.addBankroll)
	g.DELETE("/bankroll/:id", h.deleteBankroll)
	g.GET("/limits", h.limits)
	g.PUT("/limits", h.setLimits)
	g.POST("/limits/break", h.takeBreak)
	g.GET("/watchlist", h.watchlist)
	g.POST("/watchlist", h.watch)
	g.DELETE("/watchlist/:id", h.unwatch)
	g.GET("/odds/manual", h.manualOdds)
	g.POST("/odds/manual", h.addManualOdd)
	g.DELETE("/odds/manual/:id", h.deleteManualOdd)

	sa := auth.Group("/super-admin/football", requireSuper)
	sa.GET("/settings", h.settings)
	sa.PUT("/settings", h.saveSettings)
	sa.POST("/settings/test", h.testConn)
	sa.GET("/competitions", h.allCompetitions)
	sa.POST("/refresh", h.refresh)
	sa.POST("/fixtures/:id/refresh", h.refreshFixture)
	sa.GET("/fetch-log", h.fetchLog)
	sa.GET("/aliases", h.aliases)
	sa.PUT("/aliases", h.setAlias)
	sa.DELETE("/aliases/:alias", h.deleteAlias)
	sa.GET("/unmatched", h.unmatched)
	if h.S.TestClock {
		sa.POST("/test/clock", h.testClock)
	}
	// v2.17: free sources, news, team news, source check, jackpots (freehandler.go).
	h.registerFree(g, sa)
	// v2.18: scorecard, picks, consensus weights, history, jackpot plan (win_handler.go).
	h.registerWin(g, sa)
}

func ok(c *gin.Context, v any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": v}) }

func fail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func requireSuper(c *gin.Context) {
	if strings.ToLower(c.GetString("user_role")) != "super_admin" {
		fail(c, http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Only a super admin can change the football data settings: the provider keys and their costs are the platform's.")
		return
	}
	c.Next()
}

func who(c *gin.Context) (org, user string, okk bool) {
	for _, k := range []string{"organization_id", "org_id"} {
		if v := strings.TrimSpace(c.GetString(k)); v != "" {
			org = v
			break
		}
	}
	user = strings.TrimSpace(c.GetString("user_id"))
	if org == "" || user == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
		return "", "", false
	}
	return org, user, true
}

func (h *Handler) err(c *gin.Context, err error) {
	var ie *InputError
	var le *LimitError
	var pe *ProviderError
	switch {
	case errors.As(err, &ie):
		fail(c, http.StatusBadRequest, "VALIDATION", ie.Msg)
	case errors.As(err, &le):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"success": false, "error": gin.H{"code": "LIMIT_REACHED", "message": le.Msg, "limit": le.Limit, "until": le.Until}, "message": le.Msg})
	case errors.Is(err, ErrNotFound):
		fail(c, http.StatusNotFound, "NOT_FOUND", "Not found")
	case errors.Is(err, ErrBusy):
		fail(c, http.StatusConflict, "BUSY", "A football data refresh is already running. Try again in a minute.")
	case errors.As(err, &pe):
		fail(c, http.StatusBadGateway, "PROVIDER_"+strings.ToUpper(pe.Kind), testMessage(pe.Provider, err))
	default:
		fail(c, http.StatusInternalServerError, "ERROR", "Something went wrong")
	}
}

func qInt(c *gin.Context, k string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(c.Query(k)), 10, 64)
	return n
}

func qTime(c *gin.Context, k string, loc *time.Location) (*time.Time, error) {
	v := strings.TrimSpace(c.Query(k))
	if v == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", v, loc); err == nil {
		u := t.UTC()
		return &u, nil
	}
	return nil, inputErr("Use a date like 2026-11-21 for %s.", k)
}

func pathID(c *gin.Context, k string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(k), 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid id")
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------- reads

func (h *Handler) meta(c *gin.Context) {
	org, _, okk := who(c)
	if !okk {
		return
	}
	st, _ := h.S.Settings()
	af, oa := h.S.Keys()
	markets := []gin.H{}
	for _, m := range []string{Mkt1X2, MktDC, MktDNB, MktOU, MktBTTS, MktAH} {
		markets = append(markets, gin.H{"key": m, "label": MarketLabels[m], "selections": MarketSelections[m], "needs_line": NeedsLine(m)})
	}
	ok(c, gin.H{"markets": markets, "bookmakers": KenyanBookmakers, "currency": h.S.currencyOf(org), "disclaimer": Disclaimer, "limits_help": LimitsHelp,
		"min_edge": st.MinEdge, "kelly_fraction": st.KellyFraction, "kelly_cap": st.KellyCap, "lookahead_days": st.LookaheadDays,
		"providers": gin.H{"apifootball": af != "", "oddsapi": oa != ""}, "last_run_at": st.LastRunAt, "now": h.S.Now(), "test_clock": h.S.TestClock,
		"model_version": ModelVersion,
		"statuses":      []string{BetOpen, BetWon, BetLost, BetVoid, BetHalfWon, BetHalfLost, BetCashedOut}})
}

func (h *Handler) competitions(c *gin.Context) {
	ok(c, gin.H{"competitions": h.S.Competitions(true)})
}

func (h *Handler) fixtures(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	loc := h.S.zoneOf(org)
	from, err := qTime(c, "from", loc)
	if err != nil {
		h.err(c, err)
		return
	}
	to, err := qTime(c, "to", loc)
	if err != nil {
		h.err(c, err)
		return
	}
	lim, _ := strconv.Atoi(c.Query("limit"))
	minTier := strings.ToLower(strings.TrimSpace(c.Query("min_tier")))
	if minTier != "" && tierRank(minTier) < 0 {
		fail(c, http.StatusBadRequest, "VALIDATION", "min_tier must be strong, good, close or coinflip.")
		return
	}
	rows, err := h.S.Fixtures(FixtureQuery{Date: c.Query("date"), From: from, To: to, Competition: qInt(c, "competition"), Status: c.Query("status"),
		Q: c.Query("q"), Limit: lim, Org: org, User: user, TZ: loc, MinTier: minTier})
	if err != nil {
		h.err(c, err)
		return
	}
	h.S.addTeamNewsCounts(rows, org)
	st, _ := h.S.Settings()
	ok(c, gin.H{"fixtures": rows, "count": len(rows), "now": h.S.Now(), "last_run_at": st.LastRunAt, "timezone": loc.String(), "disclaimer": Disclaimer,
		"last_weekend": h.S.LastWeekendBanner(loc)})
}

func (h *Handler) analysis(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	id, okk := pathID(c, "id")
	if !okk {
		return
	}
	a, err := h.S.Analysis(id, org, user)
	if err != nil {
		h.err(c, err)
		return
	}
	if a.TeamNews != nil {
		a.TeamNews.CanReview = canReviewRole(c.GetString("user_role"))
	}
	ok(c, a)
}

func (h *Handler) kelly(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	id, okk := pathID(c, "id")
	if !okk {
		return
	}
	odds, _ := strconv.ParseFloat(c.Query("odds"), 64)
	var line *float64
	if l := strings.TrimSpace(c.Query("line")); l != "" {
		v, err := strconv.ParseFloat(l, 64)
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", "Invalid line")
			return
		}
		line = &v
	}
	k, err := h.S.KellyFor(org, user, id, c.Query("market"), c.Query("selection"), line, odds)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, k)
}

func (h *Handler) value(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	me, _ := strconv.ParseFloat(c.Query("min_edge"), 64)
	rows, err := h.S.ValueList(c.Query("date"), me, org, user, h.S.zoneOf(org))
	if err != nil {
		h.err(c, err)
		return
	}
	st, _ := h.S.Settings()
	if me <= 0 {
		me = st.MinEdge
	}
	ok(c, gin.H{"value": rows, "count": len(rows), "min_edge": me, "disclaimer": Disclaimer,
		"note": "Value = the model's probability is at least the minimum edge above the de-margined market and the best price has positive expected value. Estimates, not certainties: check the model's track record."})
}

func (h *Handler) performance(c *gin.Context) {
	org, _, okk := who(c)
	if !okk {
		return
	}
	loc := h.S.zoneOf(org)
	from, err := qTime(c, "from", loc)
	if err != nil {
		h.err(c, err)
		return
	}
	to, err := qTime(c, "to", loc)
	if err != nil {
		h.err(c, err)
		return
	}
	var comp *int64
	if id := qInt(c, "competition"); id != 0 {
		comp = &id
	}
	ok(c, h.S.ModelPerformance(comp, from, to))
}

func (h *Handler) team(c *gin.Context) {
	id, okk := pathID(c, "id")
	if !okk {
		return
	}
	tp, err := h.S.TeamProfile(id)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, tp)
}

func (h *Handler) standings(c *gin.Context) {
	id, okk := pathID(c, "competition")
	if !okk {
		return
	}
	season, _ := strconv.Atoi(c.Query("season"))
	if season == 0 {
		var comp Competition
		h.S.DB.Where("id = ?", id).Limit(1).Find(&comp)
		season = guessSeason(comp, h.S.Now())
	}
	ok(c, gin.H{"competition_id": id, "season": season, "snapshots": h.S.StandingsHistory(id, season), "computed": h.S.computedTable(id, season, h.S.Now())})
}

func (h *Handler) live(c *gin.Context) { ok(c, gin.H{"live": h.S.Live(), "now": h.S.Now()}) }

// ---------------------------------------------------------------- bets

func (h *Handler) bets(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	loc := h.S.zoneOf(org)
	from, err := qTime(c, "from", loc)
	if err != nil {
		h.err(c, err)
		return
	}
	to, err := qTime(c, "to", loc)
	if err != nil {
		h.err(c, err)
		return
	}
	lim, _ := strconv.Atoi(c.Query("limit"))
	off, _ := strconv.Atoi(c.Query("offset"))
	rows, n := h.S.Bets(org, user, BetFilter{Status: c.Query("status"), Market: c.Query("market"), Bookmaker: c.Query("bookmaker"), Competition: qInt(c, "competition"),
		From: from, To: to, Limit: lim, Offset: off})
	ok(c, gin.H{"bets": rows, "total": n})
}

func (h *Handler) createBet(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in BetInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	b, err := h.S.CreateBet(org, user, in)
	if err != nil {
		h.err(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": b})
}

func (h *Handler) bet(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	b, err := h.S.BetView(org, user, c.Param("id"))
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, b)
}

func (h *Handler) updateBet(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in BetInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	b, err := h.S.UpdateBet(org, user, c.Param("id"), in)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, b)
}

func (h *Handler) deleteBet(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	if err := h.S.DeleteBet(org, user, c.Param("id")); err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) settleBet(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in SettleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	b, err := h.S.SettleBet(org, user, c.Param("id"), in)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, b)
}

func (h *Handler) summary(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	loc := h.S.zoneOf(org)
	from, err := qTime(c, "from", loc)
	if err != nil {
		h.err(c, err)
		return
	}
	to, err := qTime(c, "to", loc)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, h.S.BetSummary(org, user, from, to))
}

func (h *Handler) bankroll(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	ok(c, gin.H{"entries": h.S.Bankroll(org, user), "balance": h.S.bankrollBalance(org, user), "currency": h.S.currencyOf(org)})
}

func (h *Handler) addBankroll(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in BankrollInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	e, err := h.S.AddBankroll(org, user, in)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"entry": e, "balance": h.S.bankrollBalance(org, user)})
}

func (h *Handler) deleteBankroll(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	if err := h.S.DeleteBankroll(org, user, c.Param("id")); err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"deleted": true, "balance": h.S.bankrollBalance(org, user)})
}

func (h *Handler) limits(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	ok(c, h.S.LimitsView(org, user))
}

func (h *Handler) setLimits(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in LimitSet
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	v, err := h.S.SetLimits(org, user, in)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) takeBreak(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in struct {
		Days int `json:"days"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	v, err := h.S.StartBreak(org, user, in.Days)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) watchlist(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	ok(c, gin.H{"items": h.S.Watchlist(org, user)})
}

func (h *Handler) watch(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in struct {
		Kind  string `json:"kind"`
		RefID int64  `json:"ref_id"`
		Note  string `json:"note"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	w, err := h.S.Watch(org, user, in.Kind, in.RefID, in.Note)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, w)
}

func (h *Handler) unwatch(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	if err := h.S.Unwatch(org, user, c.Param("id")); err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) manualOdds(c *gin.Context) {
	org, _, okk := who(c)
	if !okk {
		return
	}
	ok(c, gin.H{"odds": h.S.ManualOdds(org, qInt(c, "fixture_id")), "bookmakers": KenyanBookmakers})
}

func (h *Handler) addManualOdd(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in ManualOddInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	m, err := h.S.AddManualOdd(org, user, in)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"odd": m, "source": "manual: " + m.Bookmaker})
}

func (h *Handler) deleteManualOdd(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	if err := h.S.DeleteManualOdd(org, user, c.Param("id")); err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"deleted": true})
}

// ---------------------------------------------------------------- super admin

// SettingsView is the platform settings page.
type SettingsView struct {
	Settings     Settings       `json:"settings"`
	Usage        []Usage        `json:"usage"`
	Competitions []Competition  `json:"competitions"` // enabled + defaults
	Defaults     []int64        `json:"default_competitions"`
	PlanQuota    map[string]int `json:"plan_quota"`
	OddsCallCost int            `json:"odds_call_cost"`
	Unmatched    int64          `json:"unmatched_events"`
	TestClock    bool           `json:"test_clock"`
	Now          time.Time      `json:"now"`
	Bases        gin.H          `json:"endpoints"`
	// Which bookmakers each provider carries, and which of them operate in Kenya.
	Bookmakers gin.H `json:"bookmakers"`
}

func (h *Handler) settingsView() (SettingsView, error) {
	st, err := h.S.Settings()
	if err != nil {
		return SettingsView{}, err
	}
	v := SettingsView{Settings: st, Usage: []Usage{h.S.UsageFor(ProviderAPIFootball), h.S.UsageFor(ProviderOddsAPI)}, PlanQuota: PlanQuota,
		OddsCallCost: st.OddsCallCost(), TestClock: h.S.TestClock, Now: h.S.Now(), Defaults: []int64{}}
	for _, d := range DefaultCompetitions {
		v.Defaults = append(v.Defaults, d.ID)
	}
	h.S.DB.Where("enabled OR id IN ?", v.Defaults).Order("enabled DESC, priority, name").Find(&v.Competitions)
	h.S.DB.Model(&UnmatchedEvent{}).Count(&v.Unmatched)
	v.Bases = gin.H{"apifootball": h.S.AFBase, "oddsapi": h.S.OABase, "apifootball_docs": docAF, "oddsapi_docs": docOA}
	var oa []string
	h.S.DB.Model(&Odd{}).Where("source = ?", ProviderOddsAPI).Distinct("bookmaker_title").Order("bookmaker_title").Pluck("bookmaker_title", &oa)
	af := st.AFBookmakers
	if af == nil {
		af = []string{}
	}
	v.Bookmakers = gin.H{"apifootball": af, "oddsapi_seen": oa, "kenyan_in_apifootball": KenyanIn(af), "kenyan_in_oddsapi": KenyanIn(oa),
		"note": "SportPesa, Betika, Odibets, Mozzartbet and Shabiki are not carried by either provider: people type their prices in (\"manual: <bookmaker>\"). 1xBet and Betway are international brands whose prices can differ from their Kenyan sites."}
	return v, nil
}

func (h *Handler) settings(c *gin.Context) {
	v, err := h.settingsView()
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) saveSettings(c *gin.Context) {
	var p SettingsPatch
	if err := c.ShouldBindJSON(&p); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	if err := h.S.SaveSettings(p, c.GetString("user_id")); err != nil {
		h.err(c, err)
		return
	}
	if p.APIFootballKey != nil {
		h.S.ClearBackoff(ProviderAPIFootball)
	}
	if p.OddsAPIKey != nil {
		h.S.ClearBackoff(ProviderOddsAPI)
	}
	h.settings(c)
}

func (h *Handler) testConn(c *gin.Context) {
	var in struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&in)
	if in.Provider != ProviderAPIFootball && in.Provider != ProviderOddsAPI {
		fail(c, http.StatusBadRequest, "VALIDATION", "Provider must be apifootball or oddsapi.")
		return
	}
	ok(c, h.S.TestConnection(c.Request.Context(), in.Provider))
}

func (h *Handler) allCompetitions(c *gin.Context) {
	q := h.S.DB.Model(&Competition{})
	if t := strings.TrimSpace(c.Query("q")); t != "" {
		like := "%" + strings.ToLower(t) + "%"
		q = q.Where("(LOWER(name) LIKE ? OR LOWER(country) LIKE ?)", like, like)
	}
	if c.Query("enabled") == "true" {
		q = q.Where("enabled")
	}
	out := []Competition{}
	q.Order("enabled DESC, priority, country, name").Limit(400).Find(&out)
	ok(c, gin.H{"competitions": out})
}

func (h *Handler) refresh(c *gin.Context) {
	var in struct {
		Kinds []string `json:"kinds"`
		Force bool     `json:"force"`
		Refit bool     `json:"refit"`
		Only  bool     `json:"no_fetch"`
	}
	_ = c.ShouldBindJSON(&in)
	o := RunOpts{Trigger: "manual", Force: in.Force, Refit: in.Refit, NoFetch: in.Only}
	if len(in.Kinds) > 0 {
		o.Only = map[string]bool{}
		for _, k := range in.Kinds {
			o.Only[strings.TrimSpace(k)] = true
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 4*time.Minute)
	defer cancel()
	sum, err := h.S.RunOnce(ctx, o)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, sum)
}

func (h *Handler) refreshFixture(c *gin.Context) {
	id, okk := pathID(c, "id")
	if !okk {
		return
	}
	var err error
	if c.Query("parts") == "true" {
		err = h.S.FetchFixtureParts(c.Request.Context(), id)
	} else {
		_, err = h.S.RefreshFixture(c.Request.Context(), id)
	}
	if err != nil {
		h.err(c, err)
		return
	}
	var f Fixture
	h.S.DB.Where("id = ?", id).Limit(1).Find(&f)
	ok(c, f)
}

func (h *Handler) fetchLog(c *gin.Context) {
	lim, _ := strconv.Atoi(c.Query("limit"))
	if lim <= 0 || lim > 1000 {
		lim = 200
	}
	q := h.S.DB.Model(&FetchLog{})
	if p := c.Query("provider"); p != "" {
		q = q.Where("provider = ?", p)
	}
	out := []FetchLog{}
	q.Order("id DESC").Limit(lim).Find(&out)
	ok(c, gin.H{"log": out})
}

func (h *Handler) aliases(c *gin.Context) {
	out := []map[string]any{}
	var rows []TeamAlias
	h.S.DB.Order("source DESC, alias").Limit(2000).Find(&rows)
	for _, r := range rows {
		out = append(out, map[string]any{"alias": r.Alias, "name": r.Name, "team_id": r.TeamID, "team": h.S.teamName(r.TeamID), "source": r.Source, "provider": r.Provider, "created_at": r.CreatedAt})
	}
	ok(c, gin.H{"aliases": out})
}

func (h *Handler) setAlias(c *gin.Context) {
	var in struct {
		Name     string `json:"name"`
		TeamID   int64  `json:"team_id"`
		Provider string `json:"provider"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	a, err := h.S.SetAliasFor(in.Name, in.TeamID, in.Provider)
	if err != nil {
		h.err(c, err)
		return
	}
	// Retry matching of the unmatched events next time.
	h.S.DB.Where("1 = 1").Delete(&UnmatchedEvent{})
	ok(c, a)
}

func (h *Handler) deleteAlias(c *gin.Context) {
	res := h.S.DB.Where("alias = ?", c.Param("alias")).Delete(&TeamAlias{})
	if res.RowsAffected == 0 {
		h.err(c, ErrNotFound)
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) unmatched(c *gin.Context) {
	out := []UnmatchedEvent{}
	h.S.DB.Order("last_seen DESC").Limit(300).Find(&out)
	ok(c, gin.H{"events": out})
}

func (h *Handler) testClock(c *gin.Context) {
	var in struct {
		Now *time.Time `json:"now"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	t := time.Time{}
	if in.Now != nil {
		t = *in.Now
	}
	if err := h.S.SetClock(t); err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	ok(c, gin.H{"now": h.S.Now()})
}
