package football

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// v2.17 routes (contract: /home/claude/tools/football-free-contract.md).
//
// Everyone with the football module:
//
//	GET    /football/fixtures/:id/news            team news + news list
//	GET    /football/news                         ?team&fixture&q&limit
//	POST   /football/team-news/:id/review         {action: confirm|dismiss|clear, note} (owners, admins, managers; super admins platform-wide)
//	GET    /football/team-news/:id/audit
//	GET    /football/fixtures/:id/source-check
//	GET    /football/jackpots                     ?status&limit&offset (own jackpots)
//	GET    /football/jackpots/meta
//	GET    /football/jackpots/history
//	GET    /football/jackpots/candidates          ?home&away&date&q
//	POST   /football/jackpots/parse               {text, date?}
//	POST   /football/jackpots/analyse             JackpotInput (not saved)
//	POST   /football/jackpots                     JackpotInput
//	GET    /football/jackpots/:id                 ?top_n
//	PUT    /football/jackpots/:id                 JackpotInput
//	DELETE /football/jackpots/:id
//	POST   /football/jackpots/:id/duplicate       {name?}
//	PUT    /football/jackpots/:id/games/:n/match  {fixture_id|null}
//	PUT    /football/jackpots/:id/games/:n/result {result: 1|X|2|null, void}
//	POST   /football/jackpots/:id/tickets         {name, picks[], source}
//	DELETE /football/jackpots/:id/tickets/:ticket
//	POST   /football/jackpots/:id/grade
//
// Super admins:
//
//	GET/PUT /super-admin/football/sources, POST /sources/test {provider}, POST /sources/run {providers?, force?},
//	POST /sources/seed-history {competition_id?, seasons?}
//	GET/PUT /super-admin/football/news-rules, POST /news-rules/test
//	GET/POST /super-admin/football/news-feeds, PUT/DELETE /news-feeds/:id
//	GET /super-admin/football/news-sources, GET /super-admin/football/unmatched-names ?provider
func (h *Handler) registerFree(g, sa *gin.RouterGroup) {
	g.GET("/fixtures/:id/news", h.fixtureNews)
	g.GET("/fixtures/:id/source-check", h.sourceCheck)
	g.GET("/news", h.newsList)
	g.POST("/team-news/:id/review", h.reviewFact)
	g.GET("/team-news/:id/audit", h.factAudit)
	g.GET("/jackpots", h.jackpots)
	g.GET("/jackpots/meta", h.jackpotMeta)
	g.GET("/jackpots/history", h.jackpotHistory)
	g.GET("/jackpots/candidates", h.jackpotCandidates)
	g.POST("/jackpots/parse", h.jackpotParse)
	g.POST("/jackpots/analyse", h.jackpotAnalyse)
	g.POST("/jackpots", h.jackpotCreate)
	g.GET("/jackpots/:id", h.jackpot)
	g.PUT("/jackpots/:id", h.jackpotUpdate)
	g.DELETE("/jackpots/:id", h.jackpotDelete)
	g.POST("/jackpots/:id/duplicate", h.jackpotDuplicate)
	g.PUT("/jackpots/:id/games/:n/match", h.jackpotMatch)
	g.PUT("/jackpots/:id/games/:n/result", h.jackpotResult)
	g.POST("/jackpots/:id/tickets", h.jackpotTicket)
	g.DELETE("/jackpots/:id/tickets/:ticket", h.jackpotTicketDelete)
	g.POST("/jackpots/:id/grade", h.jackpotGrade)

	sa.GET("/sources", h.sources)
	sa.PUT("/sources", h.saveSources)
	sa.POST("/sources/test", h.testSource)
	sa.POST("/sources/run", h.runSources)
	sa.POST("/sources/seed-history", h.seedHistory)
	sa.GET("/news-rules", h.newsRulesGet)
	sa.PUT("/news-rules", h.newsRulesPut)
	sa.POST("/news-rules/test", h.newsRulesTest)
	sa.GET("/news-feeds", h.feeds)
	sa.POST("/news-feeds", h.feedCreate)
	sa.PUT("/news-feeds/:id", h.feedUpdate)
	sa.DELETE("/news-feeds/:id", h.feedDelete)
	sa.GET("/news-sources", h.newsSources)
	sa.GET("/unmatched-names", h.unmatchedNames)
}

func (h *Handler) fixtureOf(c *gin.Context) (Fixture, bool) {
	id, okk := pathID(c, "id")
	if !okk {
		return Fixture{}, false
	}
	var f Fixture
	if h.S.DB.Where("id = ?", id).Limit(1).Find(&f).RowsAffected == 0 {
		h.err(c, ErrNotFound)
		return f, false
	}
	return f, true
}

func (h *Handler) fixtureNews(c *gin.Context) {
	org, _, okk := who(c)
	if !okk {
		return
	}
	f, okk := h.fixtureOf(c)
	if !okk {
		return
	}
	tn := h.S.TeamNewsFor(f, org)
	tn.CanReview = canReviewRole(c.GetString("user_role"))
	ok(c, gin.H{"team_news": tn, "news": h.S.NewsSectionFor(f)})
}

func (h *Handler) sourceCheck(c *gin.Context) {
	if _, _, okk := who(c); !okk {
		return
	}
	f, okk := h.fixtureOf(c)
	if !okk {
		return
	}
	ok(c, h.S.SourceCheckFor(f))
}

func (h *Handler) newsList(c *gin.Context) {
	if _, _, okk := who(c); !okk {
		return
	}
	teams := []int64{}
	if t := qInt(c, "team"); t != 0 {
		teams = append(teams, t)
	}
	if fid := qInt(c, "fixture"); fid != 0 {
		var f Fixture
		if h.S.DB.Where("id = ?", fid).Limit(1).Find(&f).RowsAffected > 0 {
			teams = append(teams, f.HomeID, f.AwayID)
		}
	}
	lim, _ := strconv.Atoi(c.Query("limit"))
	items, n := h.S.NewsFor(teams, c.Query("q"), lim)
	ok(c, gin.H{"items": items, "total": n})
}

// userName looks up a person's display name for audits.
func (h *Handler) userName(user string) string {
	var r struct {
		FirstName string
		LastName  string
		Email     string
	}
	h.S.DB.Raw(`SELECT COALESCE(first_name,'') AS first_name, COALESCE(last_name,'') AS last_name, COALESCE(email,'') AS email FROM users WHERE id = ?`, user).Scan(&r)
	n := strings.TrimSpace(r.FirstName + " " + r.LastName)
	if n == "" {
		n = r.Email
	}
	if n == "" {
		n = "user " + clip(user, 12)
	}
	return n
}

func (h *Handler) reviewFact(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	role := c.GetString("user_role")
	if !canReviewRole(role) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only owners, admins and managers can confirm or dismiss team news.")
		return
	}
	var in struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	f, a, err := h.S.ReviewFact(org, user, h.userName(user), role, c.Param("id"), strings.ToLower(strings.TrimSpace(in.Action)), in.Note)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"fact": f, "audit": a})
}

func (h *Handler) factAudit(c *gin.Context) {
	org, _, okk := who(c)
	if !okk {
		return
	}
	ok(c, gin.H{"audit": h.S.FactAudit(org, c.GetString("user_role"), c.Param("id"))})
}

// ---------------------------------------------------------------- jackpots

func (h *Handler) jackpots(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	lim, _ := strconv.Atoi(c.Query("limit"))
	off, _ := strconv.Atoi(c.Query("offset"))
	rows, n := h.S.ListJackpots(org, user, c.Query("status"), lim, off)
	ok(c, gin.H{"jackpots": rows, "total": n})
}

func (h *Handler) jackpotMeta(c *gin.Context) {
	org, _, okk := who(c)
	if !okk {
		return
	}
	st := h.S.FreeSettingsGet()
	ok(c, gin.H{"bookmakers": JackpotBookmakers, "currency": h.S.currencyOf(org), "disclaimer": JackpotDisclaimer,
		"defaults": gin.H{"weights": st.JackpotWeights, "sharing": st.JackpotSharing, "top_n": 10, "ticket_price": 99},
		"help": gin.H{
			"paste":    "Paste one game per line, e.g. \"Piast Gliwice v Korona Kielce 2.44 3.35 2.55\" — numbering, dates, countries and capitals are fine.",
			"weights":  "Each game's chance blends the jackpot's own odds (margin removed) with the sharp market, ClubElo and DASYIN's model where they exist.",
			"sharing":  "One prize is shared by every winner. Favourites are popular, so a winning favourites ticket is usually shared with more people.",
			"cover":    "Doubles and triples cover two or three results in a game: each double doubles the combinations (and the cost).",
			"honesty":  "Even the most likely ticket almost always loses. These are estimates, never certainties.",
			"at_least": "P(at least k correct) uses the exact Poisson-binomial distribution of the ticket's per-game chances."}})
}

func (h *Handler) jackpotHistory(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	lim, _ := strconv.Atoi(c.Query("limit"))
	ok(c, h.S.JackpotHistory(org, user, lim))
}

func (h *Handler) jackpotCandidates(c *gin.Context) {
	org, _, okk := who(c)
	if !okk {
		return
	}
	ok(c, gin.H{"candidates": h.S.JackpotCandidates(c.Query("home"), c.Query("away"), c.Query("q"), c.Query("date"), h.S.zoneOf(org))})
}

func (h *Handler) jackpotParse(c *gin.Context) {
	org, _, okk := who(c)
	if !okk {
		return
	}
	var in struct {
		Text string `json:"text"`
		Date string `json:"date"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Text) == "" {
		fail(c, http.StatusBadRequest, "VALIDATION", "Paste the jackpot's games (one per line).")
		return
	}
	if len(in.Text) > 20000 {
		fail(c, http.StatusBadRequest, "VALIDATION", "That's too much text for one jackpot.")
		return
	}
	ok(c, h.S.ParseAndMatch(in.Text, in.Date, h.S.zoneOf(org)))
}

func bindJackpot(c *gin.Context) (JackpotInput, bool) {
	var in JackpotInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return in, false
	}
	if in.Text != nil && len(*in.Text) > 20000 {
		fail(c, http.StatusBadRequest, "VALIDATION", "That's too much text for one jackpot.")
		return in, false
	}
	return in, true
}

func (h *Handler) jackpotAnalyse(c *gin.Context) {
	org, _, okk := who(c)
	if !okk {
		return
	}
	in, okk := bindJackpot(c)
	if !okk {
		return
	}
	v, err := h.S.AnalyseJackpot(org, in)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) jackpotCreate(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	in, okk := bindJackpot(c)
	if !okk {
		return
	}
	v, err := h.S.CreateJackpot(org, user, in)
	if err != nil {
		h.err(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": v})
}

func (h *Handler) jackpot(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	n, _ := strconv.Atoi(c.Query("top_n"))
	v, err := h.S.JackpotViewOf(org, user, c.Param("id"), n)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) jackpotUpdate(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	in, okk := bindJackpot(c)
	if !okk {
		return
	}
	v, err := h.S.UpdateJackpot(org, user, c.Param("id"), in)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) jackpotDelete(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	if err := h.S.DeleteJackpot(org, user, c.Param("id")); err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) jackpotDuplicate(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&in)
	v, err := h.S.DuplicateJackpot(org, user, c.Param("id"), in.Name)
	if err != nil {
		h.err(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": v})
}

func gameN(c *gin.Context) (int, bool) {
	n, err := strconv.Atoi(c.Param("n"))
	if err != nil || n <= 0 || n > 30 {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid game number")
		return 0, false
	}
	return n, true
}

func (h *Handler) jackpotMatch(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	n, okk := gameN(c)
	if !okk {
		return
	}
	var in struct {
		FixtureID *int64 `json:"fixture_id"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	v, err := h.S.SetGameMatch(org, user, c.Param("id"), n, in.FixtureID)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) jackpotResult(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	n, okk := gameN(c)
	if !okk {
		return
	}
	var in struct {
		Result *string `json:"result"`
		Void   bool    `json:"void"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	v, err := h.S.SetGameResult(org, user, c.Param("id"), n, in.Result, in.Void)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) jackpotTicket(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in struct {
		Name   string   `json:"name"`
		Picks  []string `json:"picks"`
		Source string   `json:"source"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	v, err := h.S.AddTicket(org, user, c.Param("id"), in.Name, in.Source, in.Picks)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) jackpotTicketDelete(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	v, err := h.S.DeleteTicket(org, user, c.Param("id"), c.Param("ticket"))
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) jackpotGrade(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	v, err := h.S.GradeNow(org, user, c.Param("id"))
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, v)
}

// ---------------------------------------------------------------- super admin: sources

// SourceView is one free source on the settings page.
type SourceView struct {
	Key       string     `json:"key"`
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	NeedsKey  bool       `json:"needs_key"`
	HasKey    bool       `json:"has_key"`
	KeyHint   string     `json:"key_hint"`
	Base      string     `json:"base"`
	Docs      string     `json:"docs"`
	FreeTier  string     `json:"free_tier"`
	What      string     `json:"what"`
	Budget    int        `json:"budget"`
	Period    string     `json:"period"`
	Usage     Usage      `json:"usage"`
	LastOKAt  *time.Time `json:"last_ok_at"`
	LastError string     `json:"last_error"`
}

var sourceInfo = map[string][3]string{
	ProviderOddsPapi: {docOddsPapi, "250 requests a month, every bookmaker incl. Betika (KE) and Pinnacle", "Kenyan bookmakers' and Pinnacle prices for watched fixtures, jackpot games and the next 24 h of top competitions"},
	ProviderClubElo:  {docClubElo, "free CSV, no key", "daily club Elo ratings and its own fixture probabilities"},
	ProviderFDCOUK:   {docFDCOUK, "free CSV files, no key (attribute the source)", "results history with closing odds (Pinnacle where present) for model fitting, grading and backtests; weekly"},
	ProviderFDOrg:    {docFDOrg, "free key, 10 calls a minute, 12 competitions", "cross-check of fixtures, results and tables for the big leagues"},
	ProviderGNews:    {docGNews, "free RSS, no key (be gentle)", "headlines per team for team news"},
	ProviderGDELT:    {docGDELT, "free JSON API, no key", "multilingual news search per team"},
	ProviderFeeds:    {"", "the sites' own feeds", "club, league and local sports-press feeds you add below"},
}

func (h *Handler) sourcesView() gin.H {
	st := h.S.FreeSettingsGet()
	op, fo := h.S.FreeKeys()
	out := []SourceView{}
	for _, p := range FreeProviders {
		inf := sourceInfo[p]
		v := SourceView{Key: p, Name: providerName(p), Enabled: st.Enabled[p], Base: h.S.baseOf(p), Docs: inf[0], FreeTier: inf[1], What: inf[2],
			Budget: st.Budgets[p], Period: "day", Usage: h.S.FreeUsage(p)}
		if p == ProviderOddsPapi || p == ProviderFDOrg {
			v.NeedsKey = true
			k := op
			if p == ProviderFDOrg {
				k = fo
			}
			v.HasKey, v.KeyHint = k != "", mask(k)
		}
		if monthlyFree(p) {
			v.Period = "month"
		}
		var lg FetchLog
		if h.S.DB.Where("provider = ? AND ok", p).Order("id DESC").Limit(1).Find(&lg).RowsAffected > 0 {
			t := lg.At
			v.LastOKAt = &t
		}
		v.LastError = v.Usage.LastError
		out = append(out, v)
	}
	var tours int64
	h.S.DB.Model(&OPTournament{}).Count(&tours)
	main := []gin.H{}
	for _, c := range h.S.Competitions(false) {
		for code, id := range FDMainLeagues {
			if id == c.ID && c.Enabled {
				main = append(main, gin.H{"code": code, "competition_id": id, "name": c.Name})
			}
		}
	}
	var hm int64
	h.S.DB.Model(&HistMatch{}).Count(&hm)
	fdo := []gin.H{}
	for code, id := range FDOrgCompetitions {
		fdo = append(fdo, gin.H{"code": code, "competition_id": id})
	}
	kn := []string{}
	for k := range KenyanOP {
		kn = append(kn, k)
	}
	return gin.H{"sources": out,
		"oddspapi": gin.H{"bookmakers": splitList(st.OddsPapiBookmakers, []string{"betika"}), "kenyan_known": kn, "tournaments_mapped": tours},
		"fdcouk":   gin.H{"main": main, "new_countries": upperList(st.FDCOUKNew), "seasons": st.FDCOUKSeasons, "last_seed_at": st.LastSeedAt, "matches": hm},
		"fdorg":    gin.H{"competitions": fdo},
		"news": gin.H{"expiry_days": st.NewsExpiryDays, "confirm_domains": st.NewsConfirmDomains, "confirm_window_days": 7, "max_per_team_day": 2,
			"google": st.NewsGoogle, "languages": NewsLanguages},
		"jackpot":     gin.H{"weights": st.JackpotWeights, "sharing": st.JackpotSharing, "top_n": 10},
		"attribution": "Results and closing odds: football-data.co.uk. Club ratings and probabilities: clubelo.com. Cross-check: football-data.org. Prices: OddsPapi. News: Google News, GDELT and the publishers' feeds (links only)."}
}

func (h *Handler) sources(c *gin.Context) { ok(c, h.sourcesView()) }

func upperList(s string) []string {
	out := []string{}
	for _, x := range splitList(s, nil) {
		out = append(out, strings.ToUpper(x))
	}
	return out
}

// SourcesPatch changes the free sources' settings.
type SourcesPatch struct {
	OddsPapiKey        *string            `json:"oddspapi_key"`
	FDOrgKey           *string            `json:"fdorg_key"`
	Enabled            map[string]bool    `json:"enabled"`
	Budgets            map[string]int     `json:"budgets"`
	OddsPapiBookmakers *string            `json:"oddspapi_bookmakers"`
	FDCOUKNew          *string            `json:"fdcouk_new_countries"`
	FDCOUKSeasons      *int               `json:"fdcouk_seasons"`
	NewsExpiryDays     *int               `json:"news_expiry_days"`
	NewsConfirmDomains *int               `json:"news_confirm_domains"`
	NewsGoogle         map[string]string  `json:"news_google"`
	JackpotWeights     map[string]float64 `json:"jackpot_weights"`
	JackpotSharing     *SharingOpts       `json:"jackpot_sharing"`
}

// SaveSources applies a patch.
func (s *Service) SaveSources(p SourcesPatch, by string) error {
	st := s.FreeSettingsGet()
	up := map[string]any{"updated_by": by, "updated_at": s.Now()}
	setKey := func(col string, v *string) error {
		if v == nil {
			return nil
		}
		k := strings.TrimSpace(*v)
		if strings.HasPrefix(k, "••••") {
			return nil
		}
		if k != "" && (len(k) < 8 || len(k) > 200 || strings.ContainsAny(k, " \t\r\n")) {
			return inputErr("That doesn't look like an API key. Paste the key exactly as the provider's dashboard shows it.")
		}
		enc, err := mail.EncryptSecret(k)
		if err != nil {
			return err
		}
		up[col] = enc
		return nil
	}
	if err := setKey("oddspapi_key", p.OddsPapiKey); err != nil {
		return err
	}
	if err := setKey("fdorg_key", p.FDOrgKey); err != nil {
		return err
	}
	if p.Enabled != nil {
		en := st.Enabled
		for k, v := range p.Enabled {
			if !isFreeProvider(k) {
				return inputErr("Unknown source %q.", k)
			}
			en[k] = v
		}
		up["enabled"] = mustJSON(en)
	}
	if p.Budgets != nil {
		b := st.Budgets
		for k, v := range p.Budgets {
			if !isFreeProvider(k) {
				return inputErr("Unknown source %q.", k)
			}
			max := 100000
			if k == ProviderOddsPapi {
				max = 1000000
			}
			if v < 0 || v > max {
				return inputErr("The %s budget must be between 0 and %d requests.", providerName(k), max)
			}
			b[k] = v
		}
		up["budgets"] = mustJSON(b)
	}
	if p.OddsPapiBookmakers != nil {
		bs := []string{}
		for _, x := range splitList(*p.OddsPapiBookmakers, nil) {
			x = strings.TrimSpace(strings.ToLower(x))
			if x == "" || len(x) > 40 || strings.ContainsAny(x, " /?&=") {
				return inputErr("Bookmakers are OddsPapi slugs separated by commas, e.g. betika,pinnacle.")
			}
			bs = append(bs, x)
		}
		if len(bs) > 5 {
			return inputErr("At most 5 bookmakers (each one costs a request per snapshot).")
		}
		up["oddspapi_bookmakers"] = strings.Join(bs, ",")
	}
	if p.FDCOUKNew != nil {
		cs := []string{}
		for _, x := range splitList(strings.ToUpper(*p.FDCOUKNew), nil) {
			x = strings.ToUpper(x)
			if _, ok := FDNewLeagues[x]; !ok {
				return inputErr("Unknown football-data.co.uk country %q (use codes like POL, AUT, DNK, ROU).", x)
			}
			cs = append(cs, x)
		}
		up["fdcouk_new"] = strings.Join(cs, ",")
	}
	if p.FDCOUKSeasons != nil {
		if *p.FDCOUKSeasons < 1 || *p.FDCOUKSeasons > 10 {
			return inputErr("Seasons to seed: 1 to 10.")
		}
		up["fdcouk_seasons"] = *p.FDCOUKSeasons
	}
	if p.NewsExpiryDays != nil {
		if *p.NewsExpiryDays < 1 || *p.NewsExpiryDays > 90 {
			return inputErr("Team news expires after 1 to 90 days.")
		}
		up["news_expiry_days"] = *p.NewsExpiryDays
	}
	if p.NewsConfirmDomains != nil {
		if *p.NewsConfirmDomains < 2 || *p.NewsConfirmDomains > 5 {
			return inputErr("Confirmation needs 2 to 5 independent sites.")
		}
		up["news_confirm_domains"] = *p.NewsConfirmDomains
	}
	if p.NewsGoogle != nil {
		g := map[string]string{"hl": strings.TrimSpace(p.NewsGoogle["hl"]), "gl": strings.ToUpper(strings.TrimSpace(p.NewsGoogle["gl"])), "ceid": strings.TrimSpace(p.NewsGoogle["ceid"])}
		if g["hl"] == "" || g["gl"] == "" || g["ceid"] == "" {
			return inputErr("Google News needs hl, gl and ceid (e.g. en-KE, KE, KE:en).")
		}
		up["news_google"] = mustJSON(g)
	}
	if p.JackpotWeights != nil {
		w := map[string]float64{}
		tot := 0.0
		for _, k := range []string{"jackpot", "market", "clubelo", "model"} {
			v := p.JackpotWeights[k]
			if v < 0 || v > 10 {
				return inputErr("Weights must be between 0 and 10.")
			}
			w[k] = v
			tot += v
		}
		if tot <= 0 {
			return inputErr("At least one weight must be above 0.")
		}
		up["jackpot_weights"] = mustJSON(w)
	}
	if p.JackpotSharing != nil {
		sh := *p.JackpotSharing
		if sh.TicketsSold <= 0 || sh.TicketsSold > 1e9 || sh.Popularity <= 0 || sh.Popularity > 4 {
			return inputErr("Sharing: tickets sold above 0 and popularity between 0 and 4.")
		}
		if sh.Mode != "fixed" {
			sh.Mode = "model"
		}
		up["jackpot_sharing"] = mustJSON(sh)
	}
	return s.DB.Model(&FreeSettings{}).Where("id = 1").Updates(up).Error
}

func (h *Handler) saveSources(c *gin.Context) {
	var p SourcesPatch
	if err := c.ShouldBindJSON(&p); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	h.S.FreeSettingsGet()
	if err := h.S.SaveSources(p, c.GetString("user_id")); err != nil {
		h.err(c, err)
		return
	}
	if p.OddsPapiKey != nil {
		h.S.ClearBackoff(ProviderOddsPapi)
	}
	if p.FDOrgKey != nil {
		h.S.ClearBackoff(ProviderFDOrg)
	}
	h.sources(c)
}

// TestFreeSource checks a free source with one small request (it counts
// towards the budget, except where noted).
func (s *Service) TestFreeSource(ctx context.Context, provider string) TestResult {
	started := time.Now()
	res := TestResult{Provider: provider}
	done := func(code, msg string) TestResult {
		res.Code, res.Message, res.DurationMS = code, msg, time.Since(started).Milliseconds()
		res.OK = code == "ok"
		return res
	}
	s.ClearBackoff(provider)
	var err error
	var out *callOut
	switch provider {
	case ProviderOddsPapi:
		out, err = s.callFree(ctx, freeSpec{Provider: provider, Path: "/v4/sports", Prio: PrioCritical, Task: "test connection"})
		if err == nil {
			return done("ok", "Connected to OddsPapi (1 request of the monthly allowance used).")
		}
	case ProviderClubElo:
		out, err = s.callFree(ctx, freeSpec{Provider: provider, Path: "/Fixtures", Prio: PrioCritical, Task: "test connection"})
		if err == nil {
			fs, perr := ParseClubEloFixtures(out.Body)
			if perr != nil {
				return done("server", "ClubElo answered with something unexpected.")
			}
			return done("ok", "Connected to ClubElo — "+strconv.Itoa(len(fs))+" upcoming fixtures listed.")
		}
	case ProviderFDCOUK:
		out, err = s.callFree(ctx, freeSpec{Provider: provider, Path: "/mmz4281/" + seasonCode(guessSeason(Competition{}, s.Now())) + "/E0.csv", Prio: PrioCritical, Task: "test connection"})
		if err == nil {
			rows, perr := ParseFDCSV(out.Body, "E0", 0)
			if perr != nil {
				return done("server", "football-data.co.uk answered with something unexpected.")
			}
			return done("ok", "Connected to football-data.co.uk — "+strconv.Itoa(len(rows))+" Premier League matches this season.")
		}
	case ProviderFDOrg:
		out, err = s.callFree(ctx, freeSpec{Provider: provider, Path: "/v4/competitions/PL/standings", Prio: PrioCritical, Task: "test connection"})
		if err == nil {
			_, rows, perr := ParseFDOrgStandings(out.Body)
			if perr != nil {
				return done("server", "football-data.org answered with something unexpected.")
			}
			res.Remaining = out.Remaining
			return done("ok", "Connected to football-data.org — Premier League table with "+strconv.Itoa(len(rows))+" teams.")
		}
	case ProviderGNews:
		out, err = s.callFree(ctx, freeSpec{Provider: provider, Path: "/rss/search", Prio: PrioCritical, Task: "test connection",
			Params: url.Values{"q": {"football"}, "hl": {"en-KE"}, "gl": {"KE"}, "ceid": {"KE:en"}}})
		if err == nil {
			items, perr := ParseRSS(out.Body, provider, "en")
			if perr != nil {
				return done("server", "Google News answered with something unexpected.")
			}
			return done("ok", "Connected to Google News — "+strconv.Itoa(len(items))+" headlines.")
		}
	case ProviderGDELT:
		out, err = s.callFree(ctx, freeSpec{Provider: provider, Path: "/api/v2/doc/doc", Prio: PrioCritical, Task: "test connection",
			Params: url.Values{"query": {"football"}, "mode": {"artlist"}, "format": {"json"}, "maxrecords": {"5"}, "timespan": {"1d"}}})
		if err == nil {
			items, perr := ParseGDELT(out.Body)
			if perr != nil {
				return done("server", "GDELT answered with something unexpected.")
			}
			return done("ok", "Connected to GDELT — "+strconv.Itoa(len(items))+" articles.")
		}
	default:
		return done("error", "Unknown source.")
	}
	return done(testCode(err), testMessage(provider, err))
}

func (h *Handler) testSource(c *gin.Context) {
	var in struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&in)
	if !isFreeProvider(in.Provider) || in.Provider == ProviderFeeds {
		fail(c, http.StatusBadRequest, "VALIDATION", "Provider must be oddspapi, clubelo, fdcouk, fdorg, google_news or gdelt.")
		return
	}
	ok(c, h.S.TestFreeSource(c.Request.Context(), in.Provider))
}

func (h *Handler) runSources(c *gin.Context) {
	var in struct {
		Providers []string `json:"providers"`
		Force     bool     `json:"force"`
	}
	_ = c.ShouldBindJSON(&in)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 4*time.Minute)
	defer cancel()
	sum, err := h.S.RunFreeNow(ctx, in.Providers, in.Force)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, gin.H{"tasks": sum.Tasks, "done": sum.Done, "skipped": sum.Skipped, "failed": sum.Failed, "cost": sum.Cost, "planned": sum.Planned, "notes": sum.Notes, "duration_ms": sum.DurationMS})
}

func (h *Handler) seedHistory(c *gin.Context) {
	var in struct {
		CompetitionID int64 `json:"competition_id"`
		Seasons       int   `json:"seasons"`
	}
	_ = c.ShouldBindJSON(&in)
	if !h.S.FreeOn(ProviderFDCOUK) {
		fail(c, http.StatusBadRequest, "VALIDATION", "Switch football-data.co.uk on first.")
		return
	}
	if !h.S.runMu.TryLock() {
		h.err(c, ErrBusy)
		return
	}
	defer h.S.runMu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Minute)
	defer cancel()
	r, err := h.S.SeedHistory(ctx, in.CompetitionID, in.Seasons)
	if err != nil && r.Files == 0 {
		h.err(c, err)
		return
	}
	ok(c, r)
}

// ---------------------------------------------------------------- news rules & feeds

func (h *Handler) rulesView() gin.H {
	st := h.S.FreeSettingsGet()
	return gin.H{"rules": h.S.newsRules(), "statuses": TeamNewsStatuses, "languages": NewsLanguages, "defaults_changed": st.RulesChanged}
}

func (h *Handler) newsRulesGet(c *gin.Context) { ok(c, h.rulesView()) }

func (h *Handler) newsRulesPut(c *gin.Context) {
	var in struct {
		Rules []NewsRule `json:"rules"`
		Reset bool       `json:"reset"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	rules := in.Rules
	if in.Reset {
		rules = DefaultNewsRules()
	}
	if len(rules) == 0 || len(rules) > 200 {
		fail(c, http.StatusBadRequest, "VALIDATION", "Give between 1 and 200 rules.")
		return
	}
	valid := map[string]bool{}
	for _, s := range TeamNewsStatuses {
		valid[s] = true
	}
	clean := []NewsRule{}
	seen := map[string]bool{}
	for _, r := range rules {
		r.Language = strings.ToLower(strings.TrimSpace(r.Language))
		if len(r.Language) < 2 || len(r.Language) > 8 || !valid[r.Status] {
			fail(c, http.StatusBadRequest, "VALIDATION", "Each rule needs a language code (e.g. pl) and a status ("+strings.Join(TeamNewsStatuses, ", ")+").")
			return
		}
		k := r.Language + "|" + r.Status
		if seen[k] {
			fail(c, http.StatusBadRequest, "VALIDATION", "One rule per language and status ("+k+" twice).")
			return
		}
		seen[k] = true
		kw := []string{}
		for _, x := range r.Keywords {
			x = strings.TrimSpace(x)
			if x != "" && len(x) <= 60 {
				kw = append(kw, x)
			}
		}
		neg := []string{}
		for _, x := range r.Negative {
			x = strings.TrimSpace(x)
			if x != "" && len(x) <= 60 {
				neg = append(neg, x)
			}
		}
		if len(kw) == 0 {
			continue
		}
		clean = append(clean, NewsRule{Language: r.Language, Status: r.Status, Keywords: kw, Negative: neg})
	}
	err := h.S.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("1 = 1").Delete(&NewsRule{}).Error; err != nil {
			return err
		}
		if len(clean) > 0 {
			if err := tx.Create(&clean).Error; err != nil {
				return err
			}
		}
		return tx.Model(&FreeSettings{}).Where("id = 1").Update("rules_changed", !in.Reset).Error
	})
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, h.rulesView())
}

func (h *Handler) newsRulesTest(c *gin.Context) {
	var in struct {
		Text     string `json:"text"`
		Language string `json:"language"`
		TeamID   int64  `json:"team_id"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Text) == "" {
		fail(c, http.StatusBadRequest, "VALIDATION", "Type a headline to test.")
		return
	}
	ok(c, h.S.TestRules(clip(in.Text, 1000), in.Language, in.TeamID))
}

func (h *Handler) feeds(c *gin.Context) {
	out := []NewsFeed{}
	h.S.DB.Order("created_at").Find(&out)
	ok(c, gin.H{"feeds": out})
}

type feedInput struct {
	URL           string `json:"url"`
	Label         string `json:"label"`
	Language      string `json:"language"`
	TeamID        int64  `json:"team_id"`
	CompetitionID int64  `json:"competition_id"`
	Enabled       *bool  `json:"enabled"`
}

func (in feedInput) validate() error {
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return inputErr("The feed address must start with https:// (or http://).")
	}
	if in.TeamID == 0 && in.CompetitionID == 0 {
		return inputErr("Link the feed to a team or a competition so its news can be matched.")
	}
	return nil
}

func (h *Handler) feedCreate(c *gin.Context) {
	var in feedInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	if err := in.validate(); err != nil {
		h.err(c, err)
		return
	}
	var n int64
	h.S.DB.Model(&NewsFeed{}).Count(&n)
	if n >= 200 {
		fail(c, http.StatusBadRequest, "VALIDATION", "At most 200 feeds.")
		return
	}
	f := NewsFeed{ID: uuid.NewString(), URL: strings.TrimSpace(in.URL), Label: clip(firstNonEmpty(in.Label, hostOf(in.URL)), 160), Language: strings.ToLower(clip(in.Language, 8)),
		TeamID: in.TeamID, CompetitionID: in.CompetitionID, Enabled: in.Enabled == nil || *in.Enabled, CreatedAt: h.S.Now()}
	if err := h.S.DB.Create(&f).Error; err != nil {
		h.err(c, err)
		return
	}
	ok(c, f)
}

func (h *Handler) feedUpdate(c *gin.Context) {
	var f NewsFeed
	if h.S.DB.Where("id = ?", c.Param("id")).Limit(1).Find(&f).RowsAffected == 0 {
		h.err(c, ErrNotFound)
		return
	}
	var in feedInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	if in.URL == "" {
		in.URL = f.URL
	}
	if in.TeamID == 0 && in.CompetitionID == 0 {
		in.TeamID, in.CompetitionID = f.TeamID, f.CompetitionID
	}
	if err := in.validate(); err != nil {
		h.err(c, err)
		return
	}
	f.URL, f.TeamID, f.CompetitionID = strings.TrimSpace(in.URL), in.TeamID, in.CompetitionID
	if in.Label != "" {
		f.Label = clip(in.Label, 160)
	}
	if in.Language != "" {
		f.Language = strings.ToLower(clip(in.Language, 8))
	}
	if in.Enabled != nil {
		f.Enabled = *in.Enabled
	}
	if err := h.S.DB.Save(&f).Error; err != nil {
		h.err(c, err)
		return
	}
	ok(c, f)
}

func (h *Handler) feedDelete(c *gin.Context) {
	if h.S.DB.Where("id = ?", c.Param("id")).Delete(&NewsFeed{}).RowsAffected == 0 {
		h.err(c, ErrNotFound)
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) newsSources(c *gin.Context) { ok(c, gin.H{"sources": h.S.SourceReliability()}) }

func (h *Handler) unmatchedNames(c *gin.Context) {
	out := []UnmatchedName{}
	q := h.S.DB.Model(&UnmatchedName{})
	if p := c.Query("provider"); p != "" {
		q = q.Where("provider = ?", p)
	}
	q.Order("last_seen DESC").Limit(500).Find(&out)
	ok(c, gin.H{"names": out})
}

// SetAliasFor adds or changes an alias by hand for a provider's spelling
// (both the plain and the diacritics-folded key).
func (s *Service) SetAliasFor(name string, team int64, provider string) (TeamAlias, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "", ProviderOddsAPI, ProviderClubElo, ProviderOddsPapi, ProviderFDCOUK, ProviderFDOrg, ProviderJackpotAlias, "news":
	default:
		return TeamAlias{}, inputErr("Unknown provider %q.", provider)
	}
	a, err := s.SetAlias(name, team)
	if err != nil {
		return a, err
	}
	a.Provider = provider
	s.DB.Model(&TeamAlias{}).Where("alias = ?", a.Alias).Update("provider", provider)
	if f := normFree(name); f != "" && f != a.Alias {
		row := TeamAlias{Alias: clip(f, 160), TeamID: team, Source: "manual", Name: clip(name, 160), Provider: provider, CreatedAt: s.Now()}
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row)
	}
	// Retry mapping of this name.
	s.DB.Where("name = ?", name).Delete(&UnmatchedName{})
	if provider == ProviderClubElo {
		s.DB.Model(&ClubEloRating{}).Where("club = ?", name).Update("team_id", team)
		s.linkClubEloFixtures()
	}
	return a, nil
}
