package football

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// v2.18 routes (contract: /home/claude/tools/football-winrate-contract.md).
//
// Everyone with the football module:
//
//	GET  /football/scorecard                 ?period=weekend|7d|30d|season|custom&from&to&competition&tier
//	GET  /football/picks                     ?from&to&min_tier&competition&limit — upcoming verdicts, strongest first
//	GET  /football/history                   the open-history hit-rate tables
//	GET  /football/model/consensus-weights   learned / default consensus weights per group
//	POST /football/jackpots/:id/plan         {budget} — doubles / triples within a budget
//
// Super admins:
//
//	POST /super-admin/football/consensus-weights/fit
func (h *Handler) registerWin(g, sa *gin.RouterGroup) {
	g.GET("/scorecard", h.scorecard)
	g.GET("/picks", h.picks)
	g.GET("/history", h.history)
	g.GET("/model/consensus-weights", h.consWeights)
	g.POST("/jackpots/:id/plan", h.jackpotPlan)
	sa.POST("/consensus-weights/fit", h.consFit)
}

func (h *Handler) scorecard(c *gin.Context) {
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
	sc, err := h.S.ScorecardFor(ScoreQuery{Period: strings.ToLower(strings.TrimSpace(c.Query("period"))), From: from, To: to, Competition: qInt(c, "competition"),
		Tier: strings.ToLower(strings.TrimSpace(c.Query("tier"))), Loc: loc, Org: org, User: user})
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, sc)
}

func (h *Handler) picks(c *gin.Context) {
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
	if to != nil && c.Query("to") != "" && !strings.Contains(c.Query("to"), "T") {
		t := to.AddDate(0, 0, 1)
		to = &t
	}
	lim, _ := strconv.Atoi(c.Query("limit"))
	out, err := h.S.Picks(from, to, strings.ToLower(strings.TrimSpace(c.Query("min_tier"))), qInt(c, "competition"), lim, org, user, loc)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, out)
}

func (h *Handler) history(c *gin.Context) { ok(c, History) }

func (h *Handler) consWeights(c *gin.Context) { ok(c, h.S.ConsensusWeightsView()) }

func (h *Handler) consFit(c *gin.Context) {
	h.S.FitConsensusWeights()
	ok(c, h.S.ConsensusWeightsView())
}

func (h *Handler) jackpotPlan(c *gin.Context) {
	org, user, okk := who(c)
	if !okk {
		return
	}
	var in struct {
		Budget float64 `json:"budget"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	p, err := h.S.PlanJackpot(org, user, c.Param("id"), in.Budget)
	if err != nil {
		h.err(c, err)
		return
	}
	ok(c, p)
}

// ---------------------------------------------------------------- match page

// SourceProbOut is an external probability on the match page.
type SourceProbOut struct {
	Source    string             `json:"source"`
	Label     string             `json:"label"`
	Probs     map[string]float64 `json:"probs"`
	FetchedAt time.Time          `json:"fetched_at"`
	URL       string             `json:"url"`
	Detail    map[string]any     `json:"detail"`
}

// winSections adds the verdict, the ✓/✗ and the new sources to the match page.
func (s *Service) winSections(a *Analysis, f Fixture, pred *Prediction, addSrc func(SourceOut)) {
	a.SourceProbs = []SourceProbOut{}
	// v2.19.1: the lock (lock on view), the no-pick reason and what the model
	// knows; value flags only from a model that knows the teams.
	md := s.modelDataFor(f, pred)
	a.ModelData = &md
	if !md.Solid {
		a.Value = []ValueOut{}
	}
	if !md.HasData {
		a.Model = nil
	}
	lv := s.lockForFixture(f, a.Market, pred)
	a.Verdict, a.ResultCheck, a.Lock, a.NoPick = lv.Verdict, lv.Check, lv.Lock, lv.NoPick
	var sps []SourceProb
	s.DB.Where("fixture_id = ?", f.ID).Order("source").Find(&sps)
	for _, sp := range sps {
		if !s.FreeOn(sp.Source) {
			continue
		}
		a.SourceProbs = append(a.SourceProbs, SourceProbOut{Source: sp.Source, Label: consLabels[sp.Source], Probs: probsMap(P3{sp.PHome, sp.PDraw, sp.PAway}),
			FetchedAt: sp.FetchedAt, URL: sp.URL, Detail: sp.Detail})
		t := sp.FetchedAt
		switch sp.Source {
		case ProviderPolymarket:
			addSrc(SourceOut{Provider: ProviderPolymarket, Object: "prediction market prices", ID: strFromAny(sp.Detail["event_id"]), Endpoint: "GET /events (Gamma API)",
				FetchedAt: &t, URL: sp.URL, Note: "Polymarket prediction market — crowd prices; thin markets ignored"})
		case ProviderAFPred:
			addSrc(SourceOut{Provider: ProviderAFPred, Object: "API-Football prediction", ID: tid(f.ID), Endpoint: "GET /predictions?fixture=" + tid(f.ID), FetchedAt: &t, URL: docAFPred})
		}
	}
	var of OFMatch
	if s.DB.Where("fixture_id = ?", f.ID).Limit(1).Find(&of).RowsAffected > 0 {
		t := of.FetchedAt
		addSrc(SourceOut{Provider: ProviderOpenFootball, Object: "fixtures & results (public domain)", ID: of.Code + " " + of.Season, Endpoint: "GET /" + of.Season + "/" + of.Code + ".json",
			FetchedAt: &t, URL: docOpenFootball})
	}
}

func strFromAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return strings.TrimSpace(strings.Trim(mustJSON(v), `"`))
}

// ---------------------------------------------------------------- settings (free sources)

var winSourceInfo = map[string][3]string{
	ProviderAFPred: {docAFPred, "included in every API-Football plan, 1 request per fixture (counted in API-Football's daily budget too)",
		"API-Football's own win/draw/win percentages for upcoming fixtures of enabled competitions (next 48 h, once, plus a refresh 3 h before kickoff) — one of the consensus sources"},
	ProviderPolymarket: {docPolymarket, "public Gamma API, no key", "Polymarket prediction market — crowd prices; thin markets ignored (liquidity and spread limits below); one discovery pass an hour"},
	ProviderOpenFootball: {docOpenFootball, "public-domain files on GitHub, no key",
		"fixtures and results of the English, German, Spanish, Italian and French top two divisions: kickoff / result cross-check and grading fallback; one fetch per file a day"},
}

// winSourcesView adds the v2.18 blocks to the sources view.
func (h *Handler) winSourcesView(v gin.H) {
	st := h.S.WinSettingsGet()
	leagues := []gin.H{}
	for _, sp := range sortedKeys(pmLeagues(st.PolyLeagues)) {
		leagues = append(leagues, gin.H{"sport": sp, "competition_id": pmLeagues(st.PolyLeagues)[sp]})
	}
	var matched, stored, ofN int64
	h.S.DB.Model(&SourceProb{}).Where("source = ?", ProviderPolymarket).Count(&matched)
	h.S.DB.Model(&SourceProb{}).Where("source = ?", ProviderAFPred).Count(&stored)
	h.S.DB.Model(&OFMatch{}).Count(&ofN)
	files := []gin.H{}
	for _, code := range ofCodes(st.OFFiles) {
		l := OFLeagues[code]
		files = append(files, gin.H{"code": code, "competition_id": l.Comp, "name": l.Name})
	}
	v["polymarket"] = gin.H{"min_liquidity": st.PolyMinLiquidity, "max_spread": st.PolyMaxSpread, "leagues": leagues, "matched": matched,
		"last_discovery_at": st.LastPolyAt, "label": "Polymarket prediction market — crowd prices; thin markets ignored"}
	v["openfootball"] = gin.H{"files": files, "season": OFSeason(h.S.Now()), "last_fetch_at": st.LastOFAt, "matches": ofN, "licence": "Public domain (CC0)"}
	v["apifootball_pred"] = gin.H{"window_hours": 48, "refresh_before_min": 180, "stored": stored}
	learned := 0
	var rows []ConsensusWeightsRow
	h.S.DB.Where("in_use = 'learned'").Find(&rows)
	learned = len(rows)
	v["consensus"] = gin.H{"defaults": DefaultConsWeights, "learned_groups": learned}
	v["not_used"] = []gin.H{
		{"name": "Transfermarkt open dataset", "why": "updates paused since July 2026"},
		{"name": "SofaScore / FotMob / FBref", "why": "scraping is against their terms"},
		{"name": "Understat", "why": "no official API"},
	}
	v["history"] = History.Summary()
}

// winSourceInfoFor fills a SourceView's docs / free tier / what text.
func winSourceInfoFor(p string) ([3]string, bool) {
	x, ok := winSourceInfo[p]
	return x, ok
}

// WinPatch are the v2.18 fields of the sources patch.
type WinPatch struct {
	PolyMinLiquidity *float64 `json:"polymarket_min_liquidity"`
	PolyMaxSpread    *float64 `json:"polymarket_max_spread"`
	PolyLeagues      *string  `json:"polymarket_leagues"`
	OFFiles          *string  `json:"openfootball_files"`
}

// SaveWinSources applies the v2.18 fields (validated before anything is saved).
func (s *Service) SaveWinSources(p WinPatch) error {
	up := map[string]any{}
	if p.PolyMinLiquidity != nil {
		if *p.PolyMinLiquidity < 0 || *p.PolyMinLiquidity > 10000000 {
			return inputErr("Polymarket minimum liquidity: 0 to 10,000,000 US dollars.")
		}
		up["poly_min_liquidity"] = *p.PolyMinLiquidity
	}
	if p.PolyMaxSpread != nil {
		if *p.PolyMaxSpread < 0.005 || *p.PolyMaxSpread > 0.5 {
			return inputErr("Polymarket maximum spread: 0.005 to 0.5.")
		}
		up["poly_max_spread"] = *p.PolyMaxSpread
	}
	if p.PolyLeagues != nil {
		parts := splitList(*p.PolyLeagues, nil)
		m := pmLeagues(*p.PolyLeagues)
		if len(m) != len(parts) || len(m) > 30 {
			return inputErr("Polymarket leagues are sport:competition pairs separated by commas, e.g. epl:39,lal:140.")
		}
		keys := sortedKeys(m)
		out := []string{}
		for _, k := range keys {
			out = append(out, k+":"+strconv.FormatInt(m[k], 10))
		}
		up["poly_leagues"] = strings.Join(out, ",")
	}
	if p.OFFiles != nil {
		codes := []string{}
		for _, c := range splitList(*p.OFFiles, nil) {
			c = strings.ToLower(strings.TrimSpace(c))
			if _, ok := OFLeagues[c]; !ok {
				return inputErr("Unknown openfootball file %q (use en.1, en.2, de.1, de.2, es.1, es.2, it.1, it.2, fr.1, fr.2).", c)
			}
			codes = append(codes, c)
		}
		up["of_files"] = strings.Join(codes, ",")
	}
	if len(up) == 0 {
		return nil
	}
	s.WinSettingsGet()
	up["updated_at"] = s.Now()
	return s.DB.Model(&WinSettings{}).Where("id = 1").Updates(up).Error
}

// testWinSource checks a v2.18 source with one small request.
func (s *Service) testWinSource(ctx context.Context, provider string, done func(code, msg string) TestResult) (TestResult, bool) {
	switch provider {
	case ProviderPolymarket:
		out, err := s.callFree(ctx, freeSpec{Provider: provider, Path: "/sports", Prio: PrioCritical, Task: "test connection", Accept: "application/json"})
		if err != nil {
			return done(testCode(err), testMessage(provider, err)), true
		}
		var sp []PMSport
		if jerr := jsonUnmarshal(out.Body, &sp); jerr != nil {
			return done("server", "Polymarket answered with something unexpected."), true
		}
		soccer := 0
		for _, x := range sp {
			if _, ok := pmLeagues(s.WinSettingsGet().PolyLeagues)[strings.ToLower(x.Sport)]; ok {
				soccer++
			}
		}
		return done("ok", "Connected to Polymarket — "+strconv.Itoa(len(sp))+" sports listed, "+strconv.Itoa(soccer)+" of your football leagues among them."), true
	case ProviderOpenFootball:
		season := OFSeason(s.Now())
		out, err := s.callFree(ctx, freeSpec{Provider: provider, Path: "/" + season + "/en.1.json", Prio: PrioCritical, Task: "test connection", Accept: "application/json"})
		if err != nil {
			return done(testCode(err), testMessage(provider, err)), true
		}
		name, rows, perr := ParseOFFile(out.Body, "Europe/London")
		if perr != nil {
			return done("server", "openfootball answered with something unexpected."), true
		}
		return done("ok", "Connected to openfootball — "+name+": "+strconv.Itoa(len(rows))+" matches."), true
	case ProviderAFPred:
		af, _ := s.Keys()
		if af == "" {
			return done("no_key", "Save the API-Football key first (System settings → Football data)."), true
		}
		r := s.TestConnection(ctx, ProviderAPIFootball)
		r.Provider = ProviderAFPred
		if r.OK {
			r.Message = "API-Football answers; predictions use the same key (1 request per fixture)."
		}
		return r, true
	}
	return TestResult{}, false
}

var _ = url.Values{}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
