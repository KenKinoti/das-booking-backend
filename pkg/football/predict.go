package football

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/football/model"
)

// ModelVersion names the prediction recipe stored with every prediction.
const ModelVersion = "dc+elo linear blend 0.6/0.4 (pkg/football/model defaults: Dixon–Coles xi 0.0019 ridge 4; Elo K15 HA80 Davidson)"

// Model options (pkg/football/model; references there: Dixon & Coles 1997,
// World Football Elo, Davidson 1970, Shin 1993, Constantinou & Fenton 2012,
// Kelly 1956). The package defaults come from its walk-forward tuning.
var (
	dcOptions  = model.DefaultDCOptions()
	eloOptions = model.DefaultEloOptions()
	// Blend weights (Dixon–Coles, Elo); Elo alone when there's no DC fit.
	blendDC, blendElo = model.DefaultBlendW[0], model.DefaultBlendW[1]
)

type dcEntry struct {
	p     *model.DCParams
	asOf  string
	n     int
	err   error
	teams map[string]int // matches per team in the fit
}

type eloEntry struct {
	e    *model.Elo
	key  string
	asOf time.Time
}

var (
	cacheMu  sync.Mutex
	dcCache  = map[int64]dcEntry{}
	eloCache eloEntry
	lastFit  atomic.Int64 // when a competition was last refitted (unix ns)
)

// ResetModelCache drops fitted models (tests).
func ResetModelCache() {
	cacheMu.Lock()
	dcCache = map[int64]dcEntry{}
	eloCache = eloEntry{}
	cacheMu.Unlock()
	lastFit.Store(0)
	resetReliabilityCache()
}

func tid(id int64) string { return strconv.FormatInt(id, 10) }

// modelMatches loads finished matches of a competition (this and last season).
func (s *Service) modelMatches(comp int64, season int, before time.Time) []model.Match {
	var rows []struct {
		ID      int64
		Kickoff time.Time
		HomeID  int64
		AwayID  int64
		FTHome  int
		FTAway  int
		HXG     *float64
		AXG     *float64
	}
	s.DB.Raw(`SELECT f.id, f.kickoff, f.home_id, f.away_id, f.ft_home, f.ft_away, hs.xg AS hxg, as2.xg AS axg
		FROM fb_fixtures f
		LEFT JOIN fb_fixture_stats hs ON hs.fixture_id = f.id AND hs.team_id = f.home_id
		LEFT JOIN fb_fixture_stats as2 ON as2.fixture_id = f.id AND as2.team_id = f.away_id
		WHERE f.competition_id = ? AND f.season IN (?, ?) AND f.status IN ('FT','AET','PEN') AND f.ft_home IS NOT NULL AND f.kickoff < ?
		ORDER BY f.kickoff`, comp, season, season-1, before).Scan(&rows)
	out := make([]model.Match, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.Match{ID: tid(r.ID), Date: r.Kickoff, Home: tid(r.HomeID), Away: tid(r.AwayID), HG: r.FTHome, AG: r.FTAway,
			HxG: r.HXG, AxG: r.AXG, League: tid(comp)})
	}
	return out
}

// dcFor returns the competition's Dixon–Coles fit as of today (fitted on
// demand, cached for the day).
func (s *Service) dcFor(comp int64, force bool) dcEntry {
	now := s.Now()
	day := now.Format("2006-01-02")
	cacheMu.Lock()
	if e, ok := dcCache[comp]; ok && e.asOf == day && !force {
		cacheMu.Unlock()
		return e
	}
	cacheMu.Unlock()
	var c Competition
	s.DB.Where("id = ?", comp).Limit(1).Find(&c)
	season := guessSeason(c, now)
	ms := s.modelMatches(comp, season, now)
	e := dcEntry{asOf: day, n: len(ms), teams: map[string]int{}}
	for _, m := range ms {
		e.teams[m.Home]++
		e.teams[m.Away]++
	}
	if len(ms) < 30 {
		e.err = fmt.Errorf("only %d finished matches in this competition's last two seasons", len(ms))
	} else {
		opts := dcOptions
		withXG := 0
		for _, m := range ms {
			if m.HxG != nil && m.AxG != nil {
				withXG++
			}
		}
		if withXG*2 >= len(ms) {
			opts.UseXG = 0.3 // blend expected goals in when most matches have them
		}
		e.p, e.err = model.FitDixonColes(ms, now, opts)
	}
	cacheMu.Lock()
	dcCache[comp] = e
	cacheMu.Unlock()
	if e.p != nil {
		r := Rating{CompetitionID: comp, AsOf: now.Truncate(24 * time.Hour), Model: "dc", Version: ModelVersion, Attack: e.p.Attack, Defence: e.p.Defence,
			HomeAdv: e.p.HomeAdv, Rho: e.p.Rho, Matches: e.p.Matches, LogLik: e.p.LogLik, Converged: e.p.Converged, FittedAt: now}
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&r)
	}
	return e
}

// eloNow replays every finished match in the database (all competitions,
// oldest first) — cached until a new result arrives.
func (s *Service) eloNow() *model.Elo {
	now := s.Now()
	var stamp struct {
		N  int64
		At *time.Time
	}
	s.DB.Raw(`SELECT COUNT(*) AS n, MAX(kickoff) AS at FROM fb_fixtures WHERE status IN ('FT','AET','PEN') AND ft_home IS NOT NULL AND kickoff < ?`, now).Scan(&stamp)
	key := fmt.Sprintf("%d|%v|%s", stamp.N, stamp.At, now.Format("2006-01-02"))
	cacheMu.Lock()
	if eloCache.e != nil && eloCache.key == key {
		e := eloCache.e
		cacheMu.Unlock()
		return e
	}
	cacheMu.Unlock()
	var rows []struct {
		ID      int64
		Kickoff time.Time
		HomeID  int64
		AwayID  int64
		FTHome  int
		FTAway  int
	}
	s.DB.Raw(`SELECT id, kickoff, home_id, away_id, ft_home, ft_away FROM fb_fixtures WHERE status IN ('FT','AET','PEN') AND ft_home IS NOT NULL AND kickoff < ? ORDER BY kickoff, id`, now).Scan(&rows)
	e := model.NewElo(eloOptions)
	for _, r := range rows {
		e.Update(model.Match{ID: tid(r.ID), Date: r.Kickoff, Home: tid(r.HomeID), Away: tid(r.AwayID), HG: r.FTHome, AG: r.FTAway})
	}
	cacheMu.Lock()
	eloCache = eloEntry{e: e, key: key, asOf: now}
	cacheMu.Unlock()
	return e
}

// FitDue fits each enabled competition once a day and again when new
// results arrived since the fit (or all now with force).
func (s *Service) FitDue(force bool) int {
	n := 0
	today := s.Now().Format("2006-01-02")
	for _, c := range s.Competitions(true) {
		cacheMu.Lock()
		e, cached := dcCache[c.ID]
		cacheMu.Unlock()
		if cached && e.asOf == today && !force {
			var finished int64
			s.DB.Model(&Fixture{}).Where("competition_id = ? AND season IN (?, ?) AND status IN ('FT','AET','PEN') AND ft_home IS NOT NULL AND kickoff < ?",
				c.ID, guessSeason(c, s.Now()), guessSeason(c, s.Now())-1, s.Now()).Count(&finished)
			if int(finished) == e.n {
				continue
			}
		}
		if e := s.dcFor(c.ID, true); e.p != nil {
			n++
		}
	}
	if n == 0 && !force {
		var elo int64
		s.DB.Model(&Rating{}).Where("competition_id = 0 AND model = 'elo' AND as_of = ?", s.Now().Truncate(24*time.Hour)).Count(&elo)
		if elo > 0 {
			return 0
		}
	}
	if n > 0 {
		lastFit.Store(s.Now().UnixNano())
	}
	e := s.eloNow()
	if len(s.Competitions(true)) > 0 {
		ratings := map[string]float64{}
		var teams []int64
		s.DB.Raw(`SELECT DISTINCT home_id FROM fb_fixtures f JOIN fb_competitions c ON c.id = f.competition_id WHERE c.enabled
			UNION SELECT DISTINCT away_id FROM fb_fixtures f JOIN fb_competitions c ON c.id = f.competition_id WHERE c.enabled`).Scan(&teams)
		for _, t := range teams {
			ratings[tid(t)] = math.Round(e.Rating(tid(t))*10) / 10
		}
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&Rating{CompetitionID: 0, AsOf: s.Now().Truncate(24 * time.Hour), Model: "elo", Version: ModelVersion, Ratings: ratings, FittedAt: s.Now()})
	}
	return n
}

// ---------------------------------------------------------------- predictions

// DCOut is the Dixon–Coles part of a prediction (attack / defence on the
// model's log scale, as fitted).
type DCOut struct {
	Home        float64 `json:"p_home"`
	Draw        float64 `json:"p_draw"`
	Away        float64 `json:"p_away"`
	LambdaHome  float64 `json:"lambda_home"`
	LambdaAway  float64 `json:"lambda_away"`
	AttackHome  float64 `json:"attack_home"`
	DefenceHome float64 `json:"defence_home"`
	AttackAway  float64 `json:"attack_away"`
	DefenceAway float64 `json:"defence_away"`
	HomeAdv     float64 `json:"home_adv"`
	Rho         float64 `json:"rho"`
	Matches     int     `json:"matches"`
	FittedAsOf  string  `json:"fitted_as_of"`
}

// EloOut is the Elo part.
type EloOut struct {
	Home       float64 `json:"p_home"`
	Draw       float64 `json:"p_draw"`
	Away       float64 `json:"p_away"`
	RatingHome float64 `json:"rating_home"`
	RatingAway float64 `json:"rating_away"`
}

// AdjustOut is the availability adjustment applied.
type AdjustOut struct {
	HomeAttack  float64 `json:"home_attack"`
	HomeDefence float64 `json:"home_defence"`
	AwayAttack  float64 `json:"away_attack"`
	AwayDefence float64 `json:"away_defence"`
	HomeWhy     string  `json:"home_why"`
	AwayWhy     string  `json:"away_why"`
	HomeMissing []int64 `json:"home_missing"`
	AwayMissing []int64 `json:"away_missing"`
}

// ScoreP is a scoreline probability.
type ScoreP struct {
	H int     `json:"h"`
	A int     `json:"a"`
	P float64 `json:"p"`
}

// PredDetail is stored with a prediction (the inputs and every output).
type PredDetail struct {
	DC        *DCOut             `json:"dc"`
	Elo       *EloOut            `json:"elo"`
	Weights   map[string]float64 `json:"weights"`
	Adjust    AdjustOut          `json:"adjust"`
	Matrix    [][]float64        `json:"matrix,omitempty"` // [home goals][away goals], 0..6 shown, tail folded into 6
	Full      [][]float64        `json:"-"`
	TopScores []ScoreP           `json:"top_scores"`
	Over      map[string]float64 `json:"over"`
	BTTS      *float64           `json:"btts"`
	AH        map[string]float64 `json:"ah_home"`
	Inputs    []string           `json:"inputs"`
	DataAsOf  time.Time          `json:"data_as_of"`
	Note      string             `json:"note,omitempty"`
}

// compute builds the current prediction of a fixture.
func (s *Service) compute(f Fixture) (Prediction, error) { return s.computeOrg(f, "") }

// computeOrg is compute with an organisation's own team-news reviews
// (v2.17: confirmed / dismissed facts change the missing players for that
// organisation's view only; org "" = the global prediction).
func (s *Service) computeOrg(f Fixture, org string) (Prediction, error) {
	now := s.Now()
	d := PredDetail{Weights: map[string]float64{}, Over: map[string]float64{}, AH: map[string]float64{}, Inputs: []string{}, DataAsOf: now, TopScores: []ScoreP{}}
	h, a := tid(f.HomeID), tid(f.AwayID)
	adj, adjOut := s.availabilityOrg(f, org)
	d.Adjust = adjOut
	e := s.eloNow()
	eh, ed, ea := e.Probabilities(h, a, false)
	d.Elo = &EloOut{Home: eh, Draw: ed, Away: ea, RatingHome: math.Round(e.Rating(h)*10) / 10, RatingAway: math.Round(e.Rating(a)*10) / 10}
	d.Inputs = append(d.Inputs, fmt.Sprintf("Elo ratings from every stored result: %s %.0f, %s %.0f.", s.teamName(f.HomeID), d.Elo.RatingHome, s.teamName(f.AwayID), d.Elo.RatingAway))
	probs := []model.Probs{{H: eh, D: ed, A: ea}}
	weights := []float64{1}
	dc := s.dcFor(f.CompetitionID, false)
	if dc.p != nil {
		if _, ok := dc.p.Attack[h]; ok {
			if _, ok := dc.p.Attack[a]; ok {
				pr, err := dc.p.Predict(h, a, adj)
				if err == nil {
					d.DC = &DCOut{Home: pr.PHome, Draw: pr.PDraw, Away: pr.PAway, LambdaHome: pr.LambdaHome, LambdaAway: pr.LambdaAway,
						AttackHome: dc.p.Attack[h], DefenceHome: dc.p.Defence[h], AttackAway: dc.p.Attack[a], DefenceAway: dc.p.Defence[a],
						HomeAdv: dc.p.HomeAdv, Rho: dc.p.Rho, Matches: dc.p.Matches, FittedAsOf: dc.asOf}
					wdc, welo := blendDC, blendElo
					if dc.teams[h] < 6 || dc.teams[a] < 6 {
						wdc, welo = 0.4, 0.6
						d.Inputs = append(d.Inputs, "Few matches for one of the teams in this competition: the Dixon–Coles weight is lowered.")
					}
					probs = []model.Probs{{H: pr.PHome, D: pr.PDraw, A: pr.PAway}, {H: eh, D: ed, A: ea}}
					weights = []float64{wdc, welo}
					d.Full = pr.Matrix
					d.Matrix = foldMatrix(pr.Matrix, 6)
					for _, sc := range pr.TopScores {
						d.TopScores = append(d.TopScores, ScoreP{sc.H, sc.A, sc.P})
					}
					for k, v := range pr.Over {
						d.Over[k] = v
					}
					b := pr.BTTS
					d.BTTS = &b
					for k, v := range pr.AHHome {
						d.AH[k] = v
					}
					d.Inputs = append(d.Inputs, fmt.Sprintf("Dixon–Coles fitted on %d matches of this competition (this and last season, older matches weighted less) — expected goals %.2f v %.2f.",
						dc.p.Matches, pr.LambdaHome, pr.LambdaAway))
				}
			}
		}
	} else if dc.err != nil {
		d.Note = "Dixon–Coles not available: " + dc.err.Error() + ". Only Elo is used, so goal markets have no model estimate."
		d.Inputs = append(d.Inputs, d.Note)
	}
	if len(weights) == 2 {
		d.Weights["dc"], d.Weights["elo"] = weights[0], weights[1]
	} else {
		d.Weights["elo"] = 1
	}
	if adjOut.HomeWhy != "" || adjOut.AwayWhy != "" {
		d.Inputs = append(d.Inputs, "Availability: "+s.teamName(f.HomeID)+" — "+adjOut.HomeWhy+" "+s.teamName(f.AwayID)+" — "+adjOut.AwayWhy)
	}
	b := model.Blend(probs, weights)
	p := Prediction{FixtureID: f.ID, Kind: "current", CompetitionID: f.CompetitionID, Kickoff: f.Kickoff, ModelVersion: ModelVersion,
		PHome: b.H, PDraw: b.D, PAway: b.A, Detail: d, ComputedAt: now}
	return p, nil
}

// foldMatrix keeps 0..n goals, folding larger scores into the last row/column.
func foldMatrix(m [][]float64, n int) [][]float64 {
	out := make([][]float64, n+1)
	for i := range out {
		out[i] = make([]float64, n+1)
	}
	for i := range m {
		for j := range m[i] {
			ii, jj := i, j
			if ii > n {
				ii = n
			}
			if jj > n {
				jj = n
			}
			out[ii][jj] += m[i][j]
		}
	}
	for i := range out {
		for j := range out[i] {
			out[i][j] = math.Round(out[i][j]*1e5) / 1e5
		}
	}
	return out
}

// availability builds the AvailabilityAdjust inputs from injuries,
// suspensions and published line-ups.
func (s *Service) availability(f Fixture) (model.Adjust, AdjustOut) { return s.availabilityOrg(f, "") }

func (s *Service) availabilityOrg(f Fixture, org string) (model.Adjust, AdjustOut) {
	adj := model.Adjust{HomeAttack: 1, HomeDefence: 1, AwayAttack: 1, AwayDefence: 1}
	out := AdjustOut{HomeAttack: 1, HomeDefence: 1, AwayAttack: 1, AwayDefence: 1, HomeMissing: []int64{}, AwayMissing: []int64{}}
	for _, side := range []string{"home", "away"} {
		team := f.HomeID
		if side == "away" {
			team = f.AwayID
		}
		contrib := s.contributions(team, f.Season)
		missing := s.missingForOrg(f, team, org)
		ids := []string{}
		for _, id := range missing {
			ids = append(ids, tid(id))
		}
		att, def, why := model.AvailabilityAdjust(contrib, ids)
		if side == "home" {
			adj.HomeAttack, adj.HomeDefence, out.HomeAttack, out.HomeDefence, out.HomeWhy, out.HomeMissing = att, def, att, def, why, missing
		} else {
			adj.AwayAttack, adj.AwayDefence, out.AwayAttack, out.AwayDefence, out.AwayWhy, out.AwayMissing = att, def, att, def, why, missing
		}
	}
	return adj, out
}

// contributions are a team's players' season numbers (all competitions of
// the season added up).
func (s *Service) contributions(team int64, season int) []model.PlayerContribution {
	var rows []struct {
		PlayerID int64
		Name     string
		Minutes  int
		Goals    int
		Assists  int
		Rating   *float64
		Position string
	}
	s.DB.Raw(`SELECT ps.player_id, COALESCE(p.name,'') AS name, SUM(ps.minutes) AS minutes, SUM(ps.goals) AS goals, SUM(ps.assists) AS assists,
		AVG(ps.rating) AS rating, MAX(ps.position) AS position
		FROM fb_player_seasons ps LEFT JOIN fb_players p ON p.id = ps.player_id
		WHERE ps.team_id = ? AND ps.season = ? GROUP BY ps.player_id, p.name`, team, season).Scan(&rows)
	out := []model.PlayerContribution{}
	for _, r := range rows {
		out = append(out, model.PlayerContribution{ID: tid(r.PlayerID), Name: r.Name, Minutes: r.Minutes, Goals: r.Goals, Assists: r.Assists, Rating: r.Rating, Position: r.Position})
	}
	return out
}

// missingFor: injured or suspended for this fixture, plus — once line-ups
// are out — regulars who are neither starting nor on the bench.
func (s *Service) missingFor(f Fixture, team int64) []int64 { return s.missingForOrg(f, team, "") }

func (s *Service) missingForOrg(f Fixture, team int64, org string) []int64 {
	set := map[int64]bool{}
	// v2.17: confirmed team news (injured / suspended players) — newsrules.go.
	for _, id := range s.newsMissing(f, team, org) {
		set[id] = true
	}
	var inj []Injury
	s.DB.Where("fixture_id = ? AND team_id = ? AND status IN ?", f.ID, team, []string{"injured", "suspended"}).Find(&inj)
	for _, i := range inj {
		set[i.PlayerID] = true
	}
	var l Lineup
	if s.DB.Where("fixture_id = ? AND team_id = ?", f.ID, team).Limit(1).Find(&l).RowsAffected > 0 && len(l.StartXI) > 0 {
		in := map[int64]bool{}
		for _, p := range append(append([]LineupPlayer{}, l.StartXI...), l.Substitutes...) {
			in[p.ID] = true
		}
		var regulars []int64
		s.DB.Model(&PlayerSeason{}).Where("team_id = ? AND season = ? AND minutes >= 450", team, f.Season).Order("minutes DESC").Limit(14).Pluck("player_id", &regulars)
		for _, id := range regulars {
			if !in[id] {
				set[id] = true
			}
		}
	}
	out := keys(set)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *Service) teamName(id int64) string {
	var n string
	s.DB.Model(&Team{}).Where("id = ?", id).Pluck("name", &n)
	if n == "" {
		return "Team " + tid(id)
	}
	return n
}

// PredictUpcoming refreshes the current prediction of fixtures in the next
// days (not yet kicked off).
func (s *Service) PredictUpcoming() int {
	st, _ := s.Settings()
	now := s.Now()
	var fx []Fixture
	s.DB.Joins("JOIN fb_competitions c ON c.id = fb_fixtures.competition_id AND c.enabled").
		Where("fb_fixtures.kickoff > ? AND fb_fixtures.kickoff < ? AND fb_fixtures.status IN ?", now, now.Add(time.Duration(st.LookaheadDays+1)*24*time.Hour), []string{"NS", "TBD"}).
		Find(&fx)
	n := 0
	fit := time.Unix(0, lastFit.Load())
	for _, f := range fx {
		// Recompute only what may have changed: older than 15 minutes, or
		// injuries / line-ups / a refit came in since.
		var cur Prediction
		if s.DB.Where("fixture_id = ? AND kind = 'current'", f.ID).Limit(1).Find(&cur).RowsAffected > 0 && now.Sub(cur.ComputedAt) < 15*time.Minute &&
			cur.ComputedAt.After(fit) && (f.InjuriesAt == nil || cur.ComputedAt.After(*f.InjuriesAt)) && (f.LineupsAt == nil || cur.ComputedAt.After(*f.LineupsAt)) {
			continue
		}
		p, err := s.compute(f)
		if err != nil {
			continue
		}
		if s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&p).Error == nil {
			n++
		}
	}
	return n
}

// CurrentPrediction returns (computing when needed) a fixture's prediction:
// the frozen one once kicked off, else the current one.
func (s *Service) CurrentPrediction(f Fixture) (Prediction, bool) {
	var p Prediction
	if !f.Kickoff.After(s.Now()) || !notStartedStats[f.Status] {
		if s.DB.Where("fixture_id = ? AND kind = 'frozen'", f.ID).Limit(1).Find(&p).RowsAffected > 0 {
			return p, true
		}
	}
	if s.DB.Where("fixture_id = ? AND kind = 'current'", f.ID).Limit(1).Find(&p).RowsAffected > 0 && s.Now().Sub(p.ComputedAt) < 6*time.Hour {
		return p, true
	}
	p, err := s.compute(f)
	if err != nil {
		return p, false
	}
	if f.Kickoff.After(s.Now()) {
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&p)
	}
	return p, true
}

// FreezeWindow: predictions are frozen during the last 30 minutes before
// kickoff (never after).
const FreezeWindow = 30 * time.Minute

// FreezeDue freezes the prediction of fixtures kicking off within 30
// minutes, with the market's de-margined view at that moment.
func (s *Service) FreezeDue() int {
	now := s.Now()
	var fx []Fixture
	s.DB.Joins("JOIN fb_competitions c ON c.id = fb_fixtures.competition_id AND c.enabled").
		Where("fb_fixtures.kickoff > ? AND fb_fixtures.kickoff <= ? AND fb_fixtures.status IN ?", now, now.Add(FreezeWindow), []string{"NS", "TBD"}).
		Where("NOT EXISTS (SELECT 1 FROM fb_predictions p WHERE p.fixture_id = fb_fixtures.id AND p.kind = 'frozen')").Find(&fx)
	n := 0
	for _, f := range fx {
		if s.Freeze(f) == nil {
			n++
		}
	}
	return n
}

// Freeze stores the frozen copy of a fixture's prediction.
func (s *Service) Freeze(f Fixture) error {
	now := s.Now()
	if !f.Kickoff.After(now) {
		return inputErr("too late: the match has kicked off")
	}
	p, err := s.compute(f)
	if err != nil {
		return err
	}
	p.Kind, p.FrozenAt = "frozen", &now
	if mv := s.marketView(f, nil, ""); mv.Fair1X2 != nil {
		p.MarketHome, p.MarketDraw, p.MarketAway = ptr(mv.Fair1X2[SelHome]), ptr(mv.Fair1X2[SelDraw]), ptr(mv.Fair1X2[SelAway])
		p.MarketSource = "at freeze: " + mv.FairSource
	}
	return s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&p).Error
}

// CaptureClosing records the closing market (last prices before kickoff)
// on frozen predictions and the closing odds / CLV of bets, for fixtures
// that have kicked off.
func (s *Service) CaptureClosing() int {
	now := s.Now()
	var ps []Prediction
	s.DB.Where("kind = 'frozen' AND kickoff <= ? AND kickoff > ? AND (market_source IS NULL OR market_source NOT LIKE 'closing%')", now, now.Add(-72*time.Hour)).Find(&ps)
	n := 0
	for _, p := range ps {
		var f Fixture
		if s.DB.Where("id = ?", p.FixtureID).Limit(1).Find(&f).RowsAffected == 0 {
			continue
		}
		ko := f.Kickoff
		mv := s.marketView(f, &ko, "")
		up := map[string]any{"market_source": "closing: none (no prices stored before kickoff)"}
		if mv.Fair1X2 != nil {
			up = map[string]any{"market_home": mv.Fair1X2[SelHome], "market_draw": mv.Fair1X2[SelDraw], "market_away": mv.Fair1X2[SelAway],
				"market_source": "closing: " + mv.FairSource}
		}
		if s.DB.Model(&Prediction{}).Where("fixture_id = ? AND kind = 'frozen'", p.FixtureID).Updates(up).Error == nil {
			n++
		}
	}
	n += s.captureBetClosing()
	return n
}

// outcomeOf: 0 home, 1 draw, 2 away from the 90-minute score.
func outcomeOf(hg, ag int) int {
	switch {
	case hg > ag:
		return 0
	case hg == ag:
		return 1
	}
	return 2
}

// GradeFinished scores frozen predictions of finished fixtures (Brier, RPS,
// log loss for the model and the closing market).
func (s *Service) GradeFinished() int {
	var rows []struct {
		FixtureID int64
		FTHome    int
		FTAway    int
	}
	s.DB.Raw(`SELECT p.fixture_id, f.ft_home, f.ft_away FROM fb_predictions p JOIN fb_fixtures f ON f.id = p.fixture_id
		WHERE p.kind = 'frozen' AND p.graded_at IS NULL AND f.status IN ('FT','AET','PEN') AND f.ft_home IS NOT NULL`).Scan(&rows)
	n := 0
	now := s.Now()
	for _, r := range rows {
		var p Prediction
		if s.DB.Where("fixture_id = ? AND kind = 'frozen'", r.FixtureID).Limit(1).Find(&p).RowsAffected == 0 {
			continue
		}
		o := outcomeOf(r.FTHome, r.FTAway)
		mp := model.Probs{H: p.PHome, D: p.PDraw, A: p.PAway}
		up := map[string]any{"outcome": o, "brier": model.Brier(mp, o), "rps": model.RPS(mp, o), "log_loss": model.LogLoss(mp, o), "graded_at": now}
		if p.MarketHome != nil && p.MarketDraw != nil && p.MarketAway != nil {
			mk := model.Probs{H: *p.MarketHome, D: *p.MarketDraw, A: *p.MarketAway}
			up["market_brier"], up["market_rps"], up["market_log_loss"] = model.Brier(mk, o), model.RPS(mk, o), model.LogLoss(mk, o)
		}
		if s.DB.Model(&Prediction{}).Where("fixture_id = ? AND kind = 'frozen'", r.FixtureID).Updates(up).Error == nil {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- track record

// Performance is the model's measured track record.
type Performance struct {
	CompetitionID *int64     `json:"competition_id"`
	Competition   string     `json:"competition,omitempty"`
	From          *time.Time `json:"from"`
	To            *time.Time `json:"to"`
	N             int        `json:"n"`
	NMarket       int        `json:"n_with_market"`
	Brier         *float64   `json:"brier"`
	RPS           *float64   `json:"rps"`
	LogLoss       *float64   `json:"log_loss"`
	MarketBrier   *float64   `json:"market_brier"`
	MarketRPS     *float64   `json:"market_rps"`
	MarketLogLoss *float64   `json:"market_log_loss"`
	// Same matches only (model vs market where both exist).
	PairedBrier   *float64    `json:"paired_brier"`
	PairedRPS     *float64    `json:"paired_rps"`
	PairedLogLoss *float64    `json:"paired_log_loss"`
	Accuracy      *float64    `json:"accuracy"` // most likely outcome happened
	Calibration   []CalBinOut `json:"calibration"`
	MarketCalib   []CalBinOut `json:"market_calibration"`
	ByCompetition []PerfRow   `json:"by_competition,omitempty"`
	Note          string      `json:"note"`
	// Uncertainty and history (for the track-record page): standard errors
	// (sample sd / √n) of the mean scores, the paired model − market
	// differences on the same matches (negative = the model did better) and
	// graded matches per week.
	SE       *PerfSE     `json:"se"`
	Diff     *PerfScores `json:"diff"`
	DiffSE   *PerfScores `json:"diff_se"`
	Timeline []PerfWeek  `json:"timeline"`
}

// PerfScores is one value per score.
type PerfScores struct {
	Brier   float64 `json:"brier"`
	RPS     float64 `json:"rps"`
	LogLoss float64 `json:"log_loss"`
}

// PerfSE holds the standard errors of the model's and the market's mean scores.
type PerfSE struct {
	Model  PerfScores  `json:"model"`
	Market *PerfScores `json:"market"`
}

// PerfWeek is one week (Monday, UTC) of graded predictions.
type PerfWeek struct {
	Week      string   `json:"week"`
	N         int      `json:"n"`
	Cum       int      `json:"cum"`
	RPS       float64  `json:"rps"`
	MarketRPS *float64 `json:"market_rps"`
	NMarket   int      `json:"n_market"`
}

// meanSE returns the mean and its standard error (sample sd / √n; 0 below 2 values).
func meanSE(xs []float64) (float64, float64) {
	n := float64(len(xs))
	if n == 0 {
		return 0, 0
	}
	var m float64
	for _, x := range xs {
		m += x
	}
	m /= n
	if n < 2 {
		return m, 0
	}
	var v float64
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	return m, math.Sqrt(v/(n-1)) / math.Sqrt(n)
}

// CalBinOut is a calibration bin.
type CalBinOut struct {
	Lo    float64 `json:"lo"`
	Hi    float64 `json:"hi"`
	MeanP float64 `json:"mean_p"`
	Freq  float64 `json:"freq"`
	N     int     `json:"n"`
	CILo  float64 `json:"ci_lo"` // Wilson 95% interval of the observed frequency
	CIHi  float64 `json:"ci_hi"`
}

// PerfRow is one competition's summary.
type PerfRow struct {
	CompetitionID int64    `json:"competition_id"`
	Name          string   `json:"name"`
	N             int      `json:"n"`
	RPS           float64  `json:"rps"`
	MarketRPS     *float64 `json:"market_rps"`
	Brier         float64  `json:"brier"`
	MarketBrier   *float64 `json:"market_brier"`
	NMarket       int      `json:"n_market"`
	RPSDiff       *float64 `json:"rps_diff"`    // model − market, same matches
	RPSDiffSE     *float64 `json:"rps_diff_se"` // its standard error
}

func calOut(bs []model.CalBin) []CalBinOut {
	out := []CalBinOut{}
	for _, b := range bs {
		out = append(out, CalBinOut{Lo: b.Lo, Hi: b.Hi, MeanP: b.MeanP, Freq: b.Freq, N: b.N, CILo: b.CILo, CIHi: b.CIHi})
	}
	return out
}

// ModelPerformance aggregates graded frozen predictions.
func (s *Service) ModelPerformance(comp *int64, from, to *time.Time) Performance {
	q := s.DB.Where("kind = 'frozen' AND graded_at IS NOT NULL")
	if comp != nil {
		q = q.Where("competition_id = ?", *comp)
	}
	if from != nil {
		q = q.Where("kickoff >= ?", *from)
	}
	if to != nil {
		q = q.Where("kickoff < ?", *to)
	}
	var ps []Prediction
	q.Order("kickoff").Find(&ps)
	perf := Performance{CompetitionID: comp, From: from, To: to, N: len(ps), Calibration: []CalBinOut{}, MarketCalib: []CalBinOut{}, Timeline: []PerfWeek{},
		Note: "Predictions are frozen up to 30 minutes before kickoff and scored after full time against the 90-minute result. Lower Brier, RPS and log loss are better; the market is the de-margined closing price (Pinnacle when available, otherwise the consensus of bookmakers)."}
	if comp != nil {
		var c Competition
		if s.DB.Where("id = ?", *comp).Limit(1).Find(&c).RowsAffected > 0 {
			perf.Competition = c.Name
		}
	}
	if len(ps) == 0 {
		perf.Note = "No graded predictions yet: the track record starts with the first match that kicks off after the model's prediction is frozen. " + perf.Note
		return perf
	}
	var b, r, l, mb, mr, ml, pb, pr, pl, acc float64
	var xb, xr, xl, xmb, xmr, xml, db, dr, dl []float64
	compDiff := map[int64][]float64{}
	type wk struct {
		n, nm   int
		rps, mr float64
	}
	weeks := map[string]*wk{}
	mps, mhits := []float64{}, []bool{}
	kps, khits := []float64{}, []bool{}
	byComp := map[int64]*struct {
		n          int
		rps, brier float64
		mn         int
		mrps, mbr  float64
	}{}
	for _, p := range ps {
		o := *p.Outcome
		b += *p.Brier
		r += *p.RPS
		l += *p.LogLoss
		xb, xr, xl = append(xb, *p.Brier), append(xr, *p.RPS), append(xl, *p.LogLoss)
		ku := p.Kickoff.UTC()
		wd := (int(ku.Weekday()) + 6) % 7
		wkey := time.Date(ku.Year(), ku.Month(), ku.Day()-wd, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		w := weeks[wkey]
		if w == nil {
			w = &wk{}
			weeks[wkey] = w
		}
		w.n++
		w.rps += *p.RPS
		probs := []float64{p.PHome, p.PDraw, p.PAway}
		best := 0
		for i := range probs {
			if probs[i] > probs[best] {
				best = i
			}
			mps = append(mps, probs[i])
			mhits = append(mhits, i == o)
		}
		if best == o {
			acc++
		}
		bc := byComp[p.CompetitionID]
		if bc == nil {
			bc = &struct {
				n          int
				rps, brier float64
				mn         int
				mrps, mbr  float64
			}{}
			byComp[p.CompetitionID] = bc
		}
		bc.n++
		bc.rps += *p.RPS
		bc.brier += *p.Brier
		if p.MarketBrier != nil {
			perf.NMarket++
			mb += *p.MarketBrier
			mr += *p.MarketRPS
			ml += *p.MarketLogLoss
			pb += *p.Brier
			pr += *p.RPS
			pl += *p.LogLoss
			bc.mn++
			bc.mrps += *p.MarketRPS
			bc.mbr += *p.MarketBrier
			xmb, xmr, xml = append(xmb, *p.MarketBrier), append(xmr, *p.MarketRPS), append(xml, *p.MarketLogLoss)
			db, dr, dl = append(db, *p.Brier-*p.MarketBrier), append(dr, *p.RPS-*p.MarketRPS), append(dl, *p.LogLoss-*p.MarketLogLoss)
			compDiff[p.CompetitionID] = append(compDiff[p.CompetitionID], *p.RPS-*p.MarketRPS)
			w.nm++
			w.mr += *p.MarketRPS
			for i, x := range []float64{*p.MarketHome, *p.MarketDraw, *p.MarketAway} {
				kps = append(kps, x)
				khits = append(khits, i == o)
			}
		}
	}
	n := float64(len(ps))
	perf.Brier, perf.RPS, perf.LogLoss, perf.Accuracy = ptr(b/n), ptr(r/n), ptr(l/n), ptr(acc/n)
	if perf.NMarket > 0 {
		m := float64(perf.NMarket)
		perf.MarketBrier, perf.MarketRPS, perf.MarketLogLoss = ptr(mb/m), ptr(mr/m), ptr(ml/m)
		perf.PairedBrier, perf.PairedRPS, perf.PairedLogLoss = ptr(pb/m), ptr(pr/m), ptr(pl/m)
		perf.MarketCalib = calOut(model.Calibration(kps, khits, 10))
	}
	perf.Calibration = calOut(model.Calibration(mps, mhits, 10))
	se := &PerfSE{}
	_, se.Model.Brier = meanSE(xb)
	_, se.Model.RPS = meanSE(xr)
	_, se.Model.LogLoss = meanSE(xl)
	if perf.NMarket > 0 {
		ms := PerfScores{}
		_, ms.Brier = meanSE(xmb)
		_, ms.RPS = meanSE(xmr)
		_, ms.LogLoss = meanSE(xml)
		se.Market = &ms
		d, dse := PerfScores{}, PerfScores{}
		d.Brier, dse.Brier = meanSE(db)
		d.RPS, dse.RPS = meanSE(dr)
		d.LogLoss, dse.LogLoss = meanSE(dl)
		perf.Diff, perf.DiffSE = &d, &dse
	}
	perf.SE = se
	wkeys := make([]string, 0, len(weeks))
	for k := range weeks {
		wkeys = append(wkeys, k)
	}
	sort.Strings(wkeys)
	cum := 0
	for _, k := range wkeys {
		w := weeks[k]
		cum += w.n
		pw := PerfWeek{Week: k, N: w.n, Cum: cum, RPS: w.rps / float64(w.n), NMarket: w.nm}
		if w.nm > 0 {
			pw.MarketRPS = ptr(w.mr / float64(w.nm))
		}
		perf.Timeline = append(perf.Timeline, pw)
	}
	if comp == nil {
		for id, bc := range byComp {
			row := PerfRow{CompetitionID: id, Name: s.compName(id), N: bc.n, RPS: bc.rps / float64(bc.n), Brier: bc.brier / float64(bc.n), NMarket: bc.mn}
			if bc.mn > 0 {
				row.MarketRPS, row.MarketBrier = ptr(bc.mrps/float64(bc.mn)), ptr(bc.mbr/float64(bc.mn))
				m, e := meanSE(compDiff[id])
				row.RPSDiff, row.RPSDiffSE = ptr(m), ptr(e)
			}
			perf.ByCompetition = append(perf.ByCompetition, row)
		}
		sort.Slice(perf.ByCompetition, func(i, j int) bool { return perf.ByCompetition[i].N > perf.ByCompetition[j].N })
	}
	return perf
}

func (s *Service) compName(id int64) string {
	var n string
	s.DB.Model(&Competition{}).Where("id = ?", id).Pluck("name", &n)
	return n
}
