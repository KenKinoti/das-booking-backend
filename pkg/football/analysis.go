package football

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/football/model"
)

// GET /fixtures/:id/analysis — one call gives the match page everything:
// see Analysis. Every figure carries its provenance in Sources and the
// payload's AsOf; stale inputs are flagged.

// Docs used as "url" in sources (the providers' data itself is not public).
const (
	docAF = "https://www.api-football.com/documentation-v3"
	docOA = "https://the-odds-api.com/liveapi/guides/v4/"
)

// TeamRef is a team in a payload.
type TeamRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Logo string `json:"logo"`
	Code string `json:"code,omitempty"`
}

// CompRef is a competition in a payload.
type CompRef struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Country string `json:"country"`
	Logo    string `json:"logo"`
	Type    string `json:"type"`
	Season  int    `json:"season"`
	Round   string `json:"round,omitempty"`
}

// ScoreOut is a score.
type ScoreOut struct {
	Home    *int `json:"home"`
	Away    *int `json:"away"`
	HTHome  *int `json:"ht_home"`
	HTAway  *int `json:"ht_away"`
	FTHome  *int `json:"ft_home"`
	FTAway  *int `json:"ft_away"`
	ETHome  *int `json:"et_home"`
	ETAway  *int `json:"et_away"`
	PenHome *int `json:"pen_home"`
	PenAway *int `json:"pen_away"`
}

// FixtureOut is the match header.
type FixtureOut struct {
	ID          int64     `json:"id"`
	Competition CompRef   `json:"competition"`
	Kickoff     time.Time `json:"kickoff"`
	Status      string    `json:"status"`
	StatusLong  string    `json:"status_long"`
	Elapsed     *int      `json:"elapsed"`
	Venue       string    `json:"venue"`
	City        string    `json:"city"`
	Referee     string    `json:"referee"`
	Home        TeamRef   `json:"home"`
	Away        TeamRef   `json:"away"`
	Score       ScoreOut  `json:"score"`
	IsLive      bool      `json:"is_live"`
	IsFinished  bool      `json:"is_finished"`
	FetchedAt   time.Time `json:"fetched_at"`
}

// StandingOut is a table row.
type StandingOut struct {
	TeamID      int64  `json:"team_id"`
	Name        string `json:"name"`
	Logo        string `json:"logo"`
	Rank        int    `json:"rank"`
	Points      int    `json:"points"`
	Played      int    `json:"played"`
	Won         int    `json:"won"`
	Drawn       int    `json:"drawn"`
	Lost        int    `json:"lost"`
	GF          int    `json:"gf"`
	GA          int    `json:"ga"`
	GD          int    `json:"gd"`
	Form        string `json:"form"`
	Description string `json:"description"`
	Home        Record `json:"home"`
	Away        Record `json:"away"`
	Group       string `json:"group"`
}

// PosPoint is a team's position after a matchday.
type PosPoint struct {
	Date   time.Time `json:"date"`
	Played int       `json:"played"`
	Rank   int       `json:"rank"`
	Points int       `json:"points"`
}

// StandingsOut is the standings section.
type StandingsOut struct {
	CompetitionID int64         `json:"competition_id"`
	Season        int           `json:"season"`
	AsOf          *time.Time    `json:"as_of"`
	TableSize     int           `json:"table_size"`
	Home          *StandingOut  `json:"home"`
	Away          *StandingOut  `json:"away"`
	Table         []StandingOut `json:"table"`
	History       struct {
		Home   []PosPoint `json:"home"`
		Away   []PosPoint `json:"away"`
		Source string     `json:"source"`
	} `json:"position_history"`
	Source string `json:"source"`
}

// ResultOut is one past match of a team.
type ResultOut struct {
	FixtureID     int64     `json:"fixture_id"`
	Date          time.Time `json:"date"`
	Competition   string    `json:"competition"`
	CompetitionID int64     `json:"competition_id"`
	Venue         string    `json:"venue"` // H | A
	Opponent      TeamRef   `json:"opponent"`
	OppRank       *int      `json:"opponent_rank"`
	GF            int       `json:"gf"`
	GA            int       `json:"ga"`
	Result        string    `json:"result"` // W | D | L
	XGF           *float64  `json:"xg_for"`
	XGA           *float64  `json:"xg_against"`
	Score         string    `json:"score"`
}

// FormOut is a form summary (model.Form).
type FormOut struct {
	N           int      `json:"n"`
	W           int      `json:"w"`
	D           int      `json:"d"`
	L           int      `json:"l"`
	Points      float64  `json:"points"`
	PPG         float64  `json:"ppg"`
	GF          int      `json:"gf"`
	GA          int      `json:"ga"`
	XGF         *float64 `json:"xgf"`
	XGA         *float64 `json:"xga"`
	HasXG       bool     `json:"has_xg"`
	Sequence    string   `json:"sequence"`
	WeightedPPG float64  `json:"weighted_ppg"`
	AdjPPG      float64  `json:"adj_ppg"`
	Trend       float64  `json:"trend"`
	WinRate     float64  `json:"win_rate"`
	CleanSheets int      `json:"clean_sheets"`
	Scored      int      `json:"failed_to_score"`
	BTTS        int      `json:"btts"`
	Over25      int      `json:"over_2_5"`
}

// SeriesPoint is a goals / xG rolling series point.
type SeriesPoint struct {
	FixtureID int64     `json:"fixture_id"`
	Date      time.Time `json:"date"`
	GF        int       `json:"gf"`
	GA        int       `json:"ga"`
	XGF       *float64  `json:"xgf"`
	XGA       *float64  `json:"xga"`
	RollGF    float64   `json:"roll_gf"`
	RollGA    float64   `json:"roll_ga"`
	RollXGF   *float64  `json:"roll_xgf"`
	RollXGA   *float64  `json:"roll_xga"`
}

// H2HOut is the head-to-head section.
type H2HOut struct {
	Matches []struct {
		FixtureID   int64     `json:"fixture_id"`
		Date        time.Time `json:"date"`
		Competition string    `json:"competition"`
		Home        TeamRef   `json:"home"`
		Away        TeamRef   `json:"away"`
		HG          int       `json:"hg"`
		AG          int       `json:"ag"`
		WinnerID    *int64    `json:"winner_id"`
	} `json:"matches"`
	Summary struct {
		N         int     `json:"n"`
		HomeWins  int     `json:"home_team_wins"` // wins of this match's home team
		Draws     int     `json:"draws"`
		AwayWins  int     `json:"away_team_wins"`
		HomeGoals int     `json:"home_team_goals"`
		AwayGoals int     `json:"away_team_goals"`
		AvgGoals  float64 `json:"avg_goals"`
		BTTSRate  float64 `json:"btts_rate"`
		Over25    float64 `json:"over_2_5_rate"`
	} `json:"summary"`
	FetchedAt *time.Time `json:"fetched_at"`
}

// PlayerOut is a key player.
type PlayerOut struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Photo    string `json:"photo"`
	Position string `json:"position"`
	Age      int    `json:"age"`
	Season   struct {
		Appearances int      `json:"appearances"`
		Lineups     int      `json:"lineups"`
		Minutes     int      `json:"minutes"`
		Goals       int      `json:"goals"`
		Assists     int      `json:"assists"`
		Shots       int      `json:"shots"`
		KeyPasses   int      `json:"key_passes"`
		Rating      *float64 `json:"rating"`
		GoalsPer90  float64  `json:"goals_per90"`
		Share       float64  `json:"goal_share"` // share of the team's goal involvements
	} `json:"season"`
	Recent struct {
		Matches int      `json:"matches"`
		Minutes int      `json:"minutes"`
		Goals   int      `json:"goals"`
		Assists int      `json:"assists"`
		Rating  *float64 `json:"rating"`
	} `json:"recent"` // last 5 matches
	Availability string `json:"availability"` // available | injured | suspended | doubtful
	Reason       string `json:"reason,omitempty"`
	ExpectedXI   *bool  `json:"expected_in_lineup"`
	InLineup     *bool  `json:"in_lineup"` // once line-ups are published
}

// TeamPlayers is a team's key players and the missing-player impact.
type TeamPlayers struct {
	Key     []PlayerOut `json:"key"`
	Missing []PlayerOut `json:"missing"`
	Impact  struct {
		AttackMult  float64 `json:"attack_mult"`
		DefenceMult float64 `json:"defence_mult"`
		Why         string  `json:"why"`
	} `json:"impact"`
	FetchedAt *time.Time `json:"fetched_at"`
}

// LineupOut is a published line-up.
type LineupOut struct {
	Formation   string         `json:"formation"`
	Coach       string         `json:"coach"`
	StartXI     []LineupPlayer `json:"start_xi"`
	Substitutes []LineupPlayer `json:"substitutes"`
}

// ModelOut is the model section.
type ModelOut struct {
	Version       string                        `json:"version"`
	Kind          string                        `json:"kind"` // current | frozen
	ComputedAt    time.Time                     `json:"computed_at"`
	FrozenAt      *time.Time                    `json:"frozen_at"`
	DC            *DCOut                        `json:"dc"`
	Elo           *EloOut                       `json:"elo"`
	Blend         map[string]float64            `json:"blend"`
	Weights       map[string]float64            `json:"weights"`
	Adjustments   AdjustOut                     `json:"adjustments"`
	ScoreMatrix   [][]float64                   `json:"score_matrix"`
	TopScores     []ScoreP                      `json:"top_scores"`
	OverUnder     map[string]map[string]float64 `json:"over_under"`
	BTTS          map[string]float64            `json:"btts"`
	AsianHandicap map[string]float64            `json:"asian_handicap"` // line → P(home covers), push-aware
	DoubleChance  map[string]float64            `json:"double_chance"`
	DrawNoBet     map[string]float64            `json:"draw_no_bet"`
	Inputs        []string                      `json:"inputs"`
	Note          string                        `json:"note,omitempty"`
	OrgAdjusted   bool                          `json:"org_adjusted"` // v2.17: recomputed with this organisation's team-news reviews
	OrgNote       string                        `json:"org_note,omitempty"`
	Graded        *struct {
		Outcome       int      `json:"outcome"`
		Brier         float64  `json:"brier"`
		RPS           float64  `json:"rps"`
		LogLoss       float64  `json:"log_loss"`
		MarketBrier   *float64 `json:"market_brier"`
		MarketRPS     *float64 `json:"market_rps"`
		MarketLogLoss *float64 `json:"market_log_loss"`
		MarketSource  string   `json:"market_source"`
	} `json:"graded"`
}

// LiveOut is the in-play section.
type LiveOut struct {
	Status    string                  `json:"status"`
	Minute    *int                    `json:"minute"`
	Extra     *int                    `json:"extra"`
	Score     ScoreOut                `json:"score"`
	Events    []EventOut              `json:"events"`
	Stats     map[string]*FixtureStat `json:"stats"` // home / away
	UpdatedAt time.Time               `json:"updated_at"`
	Prices    []PriceOut              `json:"live_prices"`
}

// EventOut is a match event.
type EventOut struct {
	Minute int    `json:"minute"`
	Extra  *int   `json:"extra"`
	Side   string `json:"side"` // home | away
	TeamID int64  `json:"team_id"`
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Player string `json:"player"`
	Assist string `json:"assist"`
}

// SourceOut is the provenance of a part of the page.
type SourceOut struct {
	Provider  string     `json:"provider"`
	Object    string     `json:"object"`
	ID        string     `json:"id,omitempty"`
	Endpoint  string     `json:"endpoint,omitempty"`
	FetchedAt *time.Time `json:"fetched_at"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"` // bookmaker update time for odds
	URL       string     `json:"url,omitempty"`
	Stale     bool       `json:"stale"`
	Note      string     `json:"note,omitempty"`
}

// Analysis is the whole match page.
type Analysis struct {
	Fixture   FixtureOut    `json:"fixture"`
	Standings *StandingsOut `json:"standings"`
	Recent    struct {
		Home []ResultOut `json:"home"`
		Away []ResultOut `json:"away"`
	} `json:"recent"`
	Form struct {
		Home map[string]FormOut `json:"home"` // last5, last10, home (at home), season
		Away map[string]FormOut `json:"away"` // last5, last10, away (away from home), season
	} `json:"form"`
	Series struct {
		Window int           `json:"window"`
		Home   []SeriesPoint `json:"home"`
		Away   []SeriesPoint `json:"away"`
	} `json:"series"`
	H2H     H2HOut `json:"h2h"`
	Players struct {
		Home TeamPlayers `json:"home"`
		Away TeamPlayers `json:"away"`
	} `json:"players"`
	Lineups struct {
		Published bool       `json:"published"`
		Home      *LineupOut `json:"home"`
		Away      *LineupOut `json:"away"`
		FetchedAt *time.Time `json:"fetched_at"`
	} `json:"lineups"`
	Model       *ModelOut   `json:"model"`
	Market      MarketView  `json:"market"`
	Value       []ValueOut  `json:"value"`
	Live        *LiveOut    `json:"live"`
	TrackRecord Performance `json:"track_record"`
	Reference   struct {
		APIFootball *RefPrediction `json:"apifootball_prediction"`
		Note        string         `json:"note"`
	} `json:"reference"`
	Bankroll struct {
		Currency      string   `json:"currency"`
		Balance       *float64 `json:"balance"`
		KellyFraction float64  `json:"kelly_fraction"`
		KellyCap      float64  `json:"kelly_cap"`
		MinEdge       float64  `json:"min_edge"`
	} `json:"bankroll"`
	// v2.17 (freeanalysis.go): team news from keyword rules, the news list,
	// the cross-source stats check, ClubElo and Kenyan bookmakers (OddsPapi).
	TeamNews    *TeamNewsSection `json:"team_news"`
	News        *NewsSection     `json:"news"`
	SourceCheck *SourceCheck     `json:"source_check"`
	ClubElo     *ClubEloOut      `json:"clubelo"`
	KenyanOdds  *KenyanOddsOut   `json:"kenyan_odds"`
	// v2.18: the one-line verdict, the ✓/✗ after full time and the external
	// probabilities (Polymarket, API-Football's prediction) — win_handler.go.
	Verdict     *Verdict     `json:"verdict"`
	ResultCheck *ResultCheck `json:"result_check"`
	// v2.19.1 (lock.go): the lock, why there's no pick, what the model knows.
	Lock        *LockOut        `json:"lock"`
	NoPick      *NoPickOut      `json:"no_pick"`
	ModelData   *ModelData      `json:"model_data"`
	SourceProbs []SourceProbOut `json:"source_probs"`
	Sources     []SourceOut     `json:"sources"`
	Stale       bool            `json:"stale"`
	StaleItems  []string        `json:"stale_items"`
	AsOf        *time.Time      `json:"as_of"`
	GeneratedAt time.Time       `json:"generated_at"`
	Disclaimer  string          `json:"disclaimer"`
}

// Disclaimer is shown with every analysis.
const Disclaimer = "18+ only. Probabilities are model estimates with their inputs shown, never certainties — no bet is a sure win. " +
	"DASYIN does not take bets or money: bets are placed with your own bookmaker and only recorded here. Set limits and stop when it isn't fun."

// ---------------------------------------------------------------- helpers

func (s *Service) teamRef(id int64) TeamRef {
	var t Team
	s.DB.Where("id = ?", id).Limit(1).Find(&t)
	if t.Name == "" {
		t.Name = "Team " + tid(id)
	}
	return TeamRef{ID: id, Name: t.Name, Logo: t.Logo, Code: t.Code}
}

func scoreOf(f Fixture) ScoreOut {
	return ScoreOut{Home: f.HomeGoals, Away: f.AwayGoals, HTHome: f.HTHome, HTAway: f.HTAway, FTHome: f.FTHome, FTAway: f.FTAway,
		ETHome: f.ETHome, ETAway: f.ETAway, PenHome: f.PenHome, PenAway: f.PenAway}
}

// resultRow is a finished match from one team's side.
type resultRow struct {
	ID            int64
	Kickoff       time.Time
	CompetitionID int64
	Season        int
	HomeID        int64
	AwayID        int64
	FTHome        int
	FTAway        int
	HXG           *float64
	AXG           *float64
}

// teamHistory: a team's finished matches before t, newest first.
func (s *Service) teamHistory(team int64, before time.Time, limit int) []resultRow {
	var rows []resultRow
	s.DB.Raw(`SELECT f.id, f.kickoff, f.competition_id, f.season, f.home_id, f.away_id, f.ft_home, f.ft_away, hs.xg AS hxg, as2.xg AS axg
		FROM fb_fixtures f
		LEFT JOIN fb_fixture_stats hs ON hs.fixture_id = f.id AND hs.team_id = f.home_id
		LEFT JOIN fb_fixture_stats as2 ON as2.fixture_id = f.id AND as2.team_id = f.away_id
		WHERE (f.home_id = ? OR f.away_id = ?) AND f.status IN ('FT','AET','PEN') AND f.ft_home IS NOT NULL AND f.kickoff < ?
		ORDER BY f.kickoff DESC LIMIT ?`, team, team, before, limit).Scan(&rows)
	return rows
}

func side(r resultRow, team int64) (home bool, gf, ga int, xgf, xga *float64, opp int64) {
	if r.HomeID == team {
		return true, r.FTHome, r.FTAway, r.HXG, r.AXG, r.AwayID
	}
	return false, r.FTAway, r.FTHome, r.AXG, r.HXG, r.HomeID
}

func formOut(rs []resultRow, team int64, n int, elo *model.Elo) FormOut {
	// model.Form wants oldest → newest.
	trs := []model.TeamResult{}
	for i := len(rs) - 1; i >= 0; i-- {
		home, gf, ga, xgf, xga, opp := side(rs[i], team)
		trs = append(trs, model.TeamResult{Date: rs[i].Kickoff, Home: home, GF: gf, GA: ga, XGF: xgf, XGA: xga, Opp: tid(opp), OppRating: elo.Rating(tid(opp))})
	}
	if n > 0 && len(trs) > n {
		trs = trs[len(trs)-n:]
	}
	f := model.Form(trs, n)
	o := FormOut{N: f.N, W: f.W, D: f.D, L: f.L, Points: f.Points, PPG: round4(f.PPG), GF: f.GF, GA: f.GA, HasXG: f.HasXG, Sequence: f.Sequence,
		WeightedPPG: round4(f.WeightedPPG), AdjPPG: round4(f.AdjPPG), Trend: round4(f.Trend)}
	if f.HasXG {
		o.XGF, o.XGA = ptr(round4(f.XGF)), ptr(round4(f.XGA))
	}
	if f.N > 0 {
		o.WinRate = round4(float64(f.W) / float64(f.N))
	}
	for _, t := range trs {
		if t.GA == 0 {
			o.CleanSheets++
		}
		if t.GF == 0 {
			o.Scored++
		}
		if t.GF > 0 && t.GA > 0 {
			o.BTTS++
		}
		if t.GF+t.GA > 2 {
			o.Over25++
		}
	}
	return o
}

// ---------------------------------------------------------------- assembly

// Analysis builds the match page for a person (org/user: their bankroll and
// hand-typed prices).
func (s *Service) Analysis(id int64, org, user string) (*Analysis, error) {
	var f Fixture
	if s.DB.Where("id = ?", id).Limit(1).Find(&f).RowsAffected == 0 {
		return nil, ErrNotFound
	}
	now := s.Now()
	st, _ := s.Settings()
	var comp Competition
	s.DB.Where("id = ?", f.CompetitionID).Limit(1).Find(&comp)
	a := &Analysis{GeneratedAt: now, Disclaimer: Disclaimer, Sources: []SourceOut{}, StaleItems: []string{}, Value: []ValueOut{}}
	upcoming := f.Kickoff.After(now) && notStartedStats[f.Status]
	addSrc := func(src SourceOut) {
		// "Data as of" is the latest provider fetch (not the model's own run).
		if src.Provider != "dasyin" && src.FetchedAt != nil && (a.AsOf == nil || src.FetchedAt.After(*a.AsOf)) {
			t := *src.FetchedAt
			a.AsOf = &t
		}
		if src.Stale {
			a.Stale = true
			a.StaleItems = append(a.StaleItems, src.Object)
		}
		a.Sources = append(a.Sources, src)
	}
	stale := func(t *time.Time, max time.Duration) bool {
		return upcoming && (t == nil || now.Sub(*t) > max)
	}
	// Header.
	a.Fixture = FixtureOut{ID: f.ID, Competition: CompRef{ID: comp.ID, Name: comp.Name, Country: comp.Country, Logo: comp.Logo, Type: comp.Type, Season: f.Season, Round: f.Round},
		Kickoff: f.Kickoff, Status: f.Status, StatusLong: f.StatusLong, Elapsed: f.Elapsed, Venue: f.Venue, City: f.City, Referee: f.Referee,
		Home: s.teamRef(f.HomeID), Away: s.teamRef(f.AwayID), Score: scoreOf(f), IsLive: IsLive(f.Status), IsFinished: IsFinal(f.Status), FetchedAt: f.FetchedAt}
	fa := f.FetchedAt
	maxAge := 12 * time.Hour
	if f.Kickoff.Sub(now) < 6*time.Hour {
		maxAge = 3 * time.Hour
	}
	addSrc(SourceOut{Provider: ProviderAPIFootball, Object: "fixture", ID: tid(f.ID), Endpoint: "GET /fixtures?id=" + tid(f.ID), FetchedAt: &fa, URL: docAF + "#tag/Fixtures",
		Stale: stale(&fa, maxAge) || (IsLive(f.Status) && now.Sub(fa) > 5*time.Minute)})

	// Standings.
	a.Standings = s.standingsSection(f, comp)
	if a.Standings != nil {
		addSrc(SourceOut{Provider: ProviderAPIFootball, Object: "standings", ID: fmt.Sprintf("%d/%d", f.CompetitionID, f.Season),
			Endpoint: fmt.Sprintf("GET /standings?league=%d&season=%d", f.CompetitionID, f.Season), FetchedAt: a.Standings.AsOf, URL: docAF + "#tag/Standings",
			Stale: upcoming && a.Standings.AsOf != nil && now.Sub(*a.Standings.AsOf) > 8*24*time.Hour, Note: a.Standings.Source})
	}

	// Recent results, form, series.
	elo := s.eloNow()
	before := f.Kickoff
	if before.After(now) {
		before = now
	}
	hh := s.teamHistory(f.HomeID, minTime(f.Kickoff, now.Add(time.Minute)), 60)
	ah := s.teamHistory(f.AwayID, minTime(f.Kickoff, now.Add(time.Minute)), 60)
	a.Recent.Home = s.recentRows(hh, f.HomeID, 10)
	a.Recent.Away = s.recentRows(ah, f.AwayID, 10)
	filter := func(rs []resultRow, team int64, homeOnly, awayOnly bool, comp int64, season int) []resultRow {
		out := []resultRow{}
		for _, r := range rs {
			if homeOnly && r.HomeID != team || awayOnly && r.AwayID != team {
				continue
			}
			if comp != 0 && (r.CompetitionID != comp || r.Season != season) {
				continue
			}
			out = append(out, r)
		}
		return out
	}
	a.Form.Home = map[string]FormOut{"last5": formOut(hh, f.HomeID, 5, elo), "last10": formOut(hh, f.HomeID, 10, elo),
		"home": formOut(filter(hh, f.HomeID, true, false, 0, 0), f.HomeID, 6, elo), "season": formOut(filter(hh, f.HomeID, false, false, f.CompetitionID, f.Season), f.HomeID, 0, elo)}
	a.Form.Away = map[string]FormOut{"last5": formOut(ah, f.AwayID, 5, elo), "last10": formOut(ah, f.AwayID, 10, elo),
		"away": formOut(filter(ah, f.AwayID, false, true, 0, 0), f.AwayID, 6, elo), "season": formOut(filter(ah, f.AwayID, false, false, f.CompetitionID, f.Season), f.AwayID, 0, elo)}
	a.Series.Window = 5
	a.Series.Home = seriesOf(hh, f.HomeID, 15, 5)
	a.Series.Away = seriesOf(ah, f.AwayID, 15, 5)
	if len(hh)+len(ah) > 0 {
		var latest time.Time
		s.DB.Model(&Fixture{}).Where("id IN ?", append(idsOf(hh), idsOf(ah)...)).Select("MAX(fetched_at)").Scan(&latest)
		var det *time.Time
		s.DB.Model(&Fixture{}).Where("id IN ? AND details_at IS NOT NULL", append(idsOf(hh), idsOf(ah)...)).Select("MAX(details_at)").Scan(&det)
		addSrc(SourceOut{Provider: ProviderAPIFootball, Object: "results", Endpoint: "GET /fixtures?league&season, /fixtures?date", FetchedAt: &latest, URL: docAF + "#tag/Fixtures",
			Note: fmt.Sprintf("%d + %d past matches", len(hh), len(ah))})
		if det != nil {
			addSrc(SourceOut{Provider: ProviderAPIFootball, Object: "match statistics (xG)", Endpoint: "GET /fixtures?ids= (statistics, events, players)", FetchedAt: det, URL: docAF + "#tag/Fixtures/operation/get-fixtures-statistics"})
		}
	}

	// Head-to-head.
	a.H2H = s.h2hSection(f)
	if a.H2H.FetchedAt != nil {
		addSrc(SourceOut{Provider: ProviderAPIFootball, Object: "head-to-head", ID: fmt.Sprintf("%d-%d", f.HomeID, f.AwayID),
			Endpoint: fmt.Sprintf("GET /fixtures/headtohead?h2h=%d-%d&last=10", f.HomeID, f.AwayID), FetchedAt: a.H2H.FetchedAt, URL: docAF + "#tag/Fixtures/operation/get-fixtures-headtohead",
			Stale: stale(a.H2H.FetchedAt, 8*24*time.Hour)})
	}

	// Prediction (model) first: players' impact comes from it.
	pred, havePred := s.CurrentPrediction(f)
	// Players and availability.
	a.Players.Home = s.playersSection(f, f.HomeID, pred.Detail.Adjust.HomeAttack, pred.Detail.Adjust.HomeDefence, pred.Detail.Adjust.HomeWhy)
	a.Players.Away = s.playersSection(f, f.AwayID, pred.Detail.Adjust.AwayAttack, pred.Detail.Adjust.AwayDefence, pred.Detail.Adjust.AwayWhy)
	pAt := latestTime(a.Players.Home.FetchedAt, a.Players.Away.FetchedAt)
	if pAt != nil {
		addSrc(SourceOut{Provider: ProviderAPIFootball, Object: "player season statistics", Endpoint: fmt.Sprintf("GET /players?team&season=%d", f.Season),
			FetchedAt: pAt, URL: docAF + "#tag/Players", Stale: stale(pAt, 10*24*time.Hour)})
	}
	if f.InjuriesAt != nil || upcoming {
		addSrc(SourceOut{Provider: ProviderAPIFootball, Object: "injuries & suspensions", Endpoint: fmt.Sprintf("GET /injuries?league=%d&season=%d", f.CompetitionID, f.Season),
			FetchedAt: f.InjuriesAt, URL: docAF + "#tag/Injuries", Stale: upcoming && f.Kickoff.Sub(now) < 36*time.Hour && stale(f.InjuriesAt, 24*time.Hour)})
	}
	// Line-ups.
	var lus []Lineup
	s.DB.Where("fixture_id = ?", f.ID).Find(&lus)
	for _, l := range lus {
		lo := &LineupOut{Formation: l.Formation, Coach: l.Coach, StartXI: l.StartXI, Substitutes: l.Substitutes}
		if l.TeamID == f.HomeID {
			a.Lineups.Home = lo
		} else if l.TeamID == f.AwayID {
			a.Lineups.Away = lo
		}
		t := l.FetchedAt
		a.Lineups.FetchedAt = latestTime(a.Lineups.FetchedAt, &t)
	}
	a.Lineups.Published = a.Lineups.Home != nil && a.Lineups.Away != nil && len(a.Lineups.Home.StartXI) > 0
	if a.Lineups.FetchedAt != nil {
		addSrc(SourceOut{Provider: ProviderAPIFootball, Object: "line-ups", ID: tid(f.ID), Endpoint: "GET /fixtures?ids= (lineups)", FetchedAt: a.Lineups.FetchedAt, URL: docAF + "#tag/Fixtures/operation/get-fixtures-lineups"})
	}

	// Model.
	if havePred {
		a.Model = modelOut(pred)
		fit := pred.ComputedAt
		note := pred.ModelVersion
		if pred.Kind == "frozen" {
			note = "frozen before kickoff — " + note
		}
		addSrc(SourceOut{Provider: "dasyin", Object: "model prediction", ID: tid(f.ID), FetchedAt: &fit, Note: note})
	}

	// Market and value.
	var ko *time.Time
	if !upcoming {
		k := f.Kickoff
		ko = &k
	}
	a.Market = s.marketView(f, ko, org, true)
	for _, b := range a.Market.Bookmakers {
		b := b
		src := SourceOut{Provider: b.Source, Object: "odds: " + b.Title, ID: b.Key, FetchedAt: &b.FetchedAt, UpdatedAt: b.UpdatedAt, Stale: b.Stale}
		switch b.Source {
		case ProviderOddsAPI:
			src.Endpoint, src.URL = "GET /v4/sports/{sport}/odds", docOA
		case ProviderAPIFootball:
			src.Endpoint, src.URL = "GET /odds?league&season", docAF+"#tag/Odds-(Pre-Match)"
		case ProviderOddsPapi:
			src.Endpoint, src.URL = "GET /v4/odds-by-tournaments", "https://oddspapi.io/en/docs/get-odds-by-tournament"
		case ProviderFDCOUK:
			src.Endpoint, src.URL, src.Note = "closing odds CSV", "https://www.football-data.co.uk/data.php", "closing prices from football-data.co.uk"
		default:
			src.Note = "typed in by someone in your organisation"
		}
		addSrc(src)
	}
	cur := s.currencyOf(org)
	bal := s.bankrollBalance(org, user)
	a.Bankroll.Currency, a.Bankroll.KellyFraction, a.Bankroll.KellyCap, a.Bankroll.MinEdge = cur, st.KellyFraction, st.KellyCap, st.MinEdge
	if bal != nil {
		a.Bankroll.Balance = bal
	}
	if havePred {
		b := 0.0
		if bal != nil {
			b = *bal
		}
		a.Value = valueRows(pred, a.Market, st.MinEdge, st.KellyFraction, st.KellyCap, b, cur)
	}

	// Live.
	if IsLive(f.Status) || IsFinal(f.Status) && f.FinishedAt != nil && now.Sub(*f.FinishedAt) < 3*time.Hour {
		a.Live = s.liveSection(f)
		ev := a.Live.UpdatedAt
		addSrc(SourceOut{Provider: ProviderAPIFootball, Object: "live: score, events, statistics", ID: tid(f.ID), Endpoint: "GET /fixtures?ids=", FetchedAt: &ev,
			URL: docAF + "#tag/Fixtures", Stale: IsLive(f.Status) && now.Sub(ev) > 5*time.Minute})
	}

	// Track record of this competition.
	cid := f.CompetitionID
	a.TrackRecord = s.ModelPerformance(&cid, nil, nil)
	a.Reference.APIFootball = f.PredictionRef
	a.Reference.Note = "API-Football's own prediction, shown for reference only — it is not part of DASYIN's model."
	if f.PredictionRef != nil {
		t := f.PredictionRef.FetchedAt
		addSrc(SourceOut{Provider: ProviderAPIFootball, Object: "reference prediction", ID: tid(f.ID), Endpoint: "GET /predictions?fixture=" + tid(f.ID), FetchedAt: &t, URL: docAF + "#tag/Predictions"})
	}
	// v2.17: team news, news, source check, ClubElo, Kenyan bookmakers.
	s.freeSections(a, f, org, user, upcoming, addSrc)
	// v2.18: the verdict, the ✓/✗ and the new sources' probabilities (win_handler.go).
	var pp *Prediction
	if havePred {
		pp = &pred
	}
	s.winSections(a, f, pp, addSrc)
	return a, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func latestTime(ts ...*time.Time) *time.Time {
	var out *time.Time
	for _, t := range ts {
		if t != nil && (out == nil || t.After(*out)) {
			x := *t
			out = &x
		}
	}
	return out
}

func idsOf(rs []resultRow) []int64 {
	out := make([]int64, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func modelOut(p Prediction) *ModelOut {
	d := p.Detail
	m := &ModelOut{Version: p.ModelVersion, Kind: p.Kind, ComputedAt: p.ComputedAt, FrozenAt: p.FrozenAt, DC: d.DC, Elo: d.Elo,
		Blend: map[string]float64{"home": round4(p.PHome), "draw": round4(p.PDraw), "away": round4(p.PAway)}, Weights: d.Weights,
		Adjustments: d.Adjust, ScoreMatrix: d.Matrix, TopScores: d.TopScores, OverUnder: map[string]map[string]float64{}, AsianHandicap: d.AH,
		DoubleChance: map[string]float64{Sel1X: round4(p.PHome + p.PDraw), Sel12: round4(p.PHome + p.PAway), SelX2: round4(p.PDraw + p.PAway)},
		DrawNoBet:    map[string]float64{}, Inputs: d.Inputs, Note: d.Note}
	if p.PHome+p.PAway > 0 {
		m.DrawNoBet[SelHome], m.DrawNoBet[SelAway] = round4(p.PHome/(p.PHome+p.PAway)), round4(p.PAway/(p.PHome+p.PAway))
	}
	for k, v := range d.Over {
		m.OverUnder[k] = map[string]float64{"over": round4(v), "under": round4(1 - v)}
	}
	if d.BTTS != nil {
		m.BTTS = map[string]float64{"yes": round4(*d.BTTS), "no": round4(1 - *d.BTTS)}
	}
	if m.ScoreMatrix == nil {
		m.ScoreMatrix = [][]float64{}
	}
	if m.AsianHandicap == nil {
		m.AsianHandicap = map[string]float64{}
	}
	if p.GradedAt != nil && p.Outcome != nil {
		g := &struct {
			Outcome       int      `json:"outcome"`
			Brier         float64  `json:"brier"`
			RPS           float64  `json:"rps"`
			LogLoss       float64  `json:"log_loss"`
			MarketBrier   *float64 `json:"market_brier"`
			MarketRPS     *float64 `json:"market_rps"`
			MarketLogLoss *float64 `json:"market_log_loss"`
			MarketSource  string   `json:"market_source"`
		}{Outcome: *p.Outcome, Brier: *p.Brier, RPS: *p.RPS, LogLoss: *p.LogLoss, MarketBrier: p.MarketBrier, MarketRPS: p.MarketRPS, MarketLogLoss: p.MarketLogLoss, MarketSource: p.MarketSource}
		m.Graded = g
	}
	return m
}

func (s *Service) recentRows(rs []resultRow, team int64, n int) []ResultOut {
	out := []ResultOut{}
	ranks := map[int64]map[int64]int{} // comp → team → rank (latest snapshot)
	for i, r := range rs {
		if i >= n {
			break
		}
		home, gf, ga, xgf, xga, opp := side(r, team)
		ro := ResultOut{FixtureID: r.ID, Date: r.Kickoff, CompetitionID: r.CompetitionID, Competition: s.compName(r.CompetitionID), Venue: "A",
			Opponent: s.teamRef(opp), GF: gf, GA: ga, XGF: xgf, XGA: xga, Score: fmt.Sprintf("%d-%d", r.FTHome, r.FTAway)}
		if home {
			ro.Venue = "H"
		}
		switch {
		case gf > ga:
			ro.Result = "W"
		case gf == ga:
			ro.Result = "D"
		default:
			ro.Result = "L"
		}
		if ranks[r.CompetitionID] == nil {
			ranks[r.CompetitionID] = map[int64]int{}
			var rows []Standing
			s.DB.Raw(`SELECT * FROM fb_standings WHERE competition_id = ? AND season = ? AND as_of = (SELECT MAX(as_of) FROM fb_standings WHERE competition_id = ? AND season = ?)`,
				r.CompetitionID, r.Season, r.CompetitionID, r.Season).Scan(&rows)
			for _, x := range rows {
				ranks[r.CompetitionID][x.TeamID] = x.Rank
			}
		}
		if rk, ok := ranks[r.CompetitionID][opp]; ok {
			ro.OppRank = ptr(rk)
		}
		out = append(out, ro)
	}
	return out
}

func seriesOf(rs []resultRow, team int64, n, w int) []SeriesPoint {
	if len(rs) > n {
		rs = rs[:n]
	}
	out := []SeriesPoint{}
	for i := len(rs) - 1; i >= 0; i-- {
		_, gf, ga, xgf, xga, _ := side(rs[i], team)
		out = append(out, SeriesPoint{FixtureID: rs[i].ID, Date: rs[i].Kickoff, GF: gf, GA: ga, XGF: xgf, XGA: xga})
	}
	for i := range out {
		lo := i - w + 1
		if lo < 0 {
			lo = 0
		}
		var gf, ga, xf, xa float64
		nx := 0
		for j := lo; j <= i; j++ {
			gf += float64(out[j].GF)
			ga += float64(out[j].GA)
			if out[j].XGF != nil && out[j].XGA != nil {
				xf += *out[j].XGF
				xa += *out[j].XGA
				nx++
			}
		}
		k := float64(i - lo + 1)
		out[i].RollGF, out[i].RollGA = round4(gf/k), round4(ga/k)
		if nx == i-lo+1 {
			out[i].RollXGF, out[i].RollXGA = ptr(round4(xf/k)), ptr(round4(xa/k))
		}
	}
	return out
}

// standingsSection: the latest provider snapshot (both teams + the table)
// and each team's position over the season.
func (s *Service) standingsSection(f Fixture, comp Competition) *StandingsOut {
	var rows []Standing
	s.DB.Raw(`SELECT * FROM fb_standings WHERE competition_id = ? AND season = ? AND as_of = (SELECT MAX(as_of) FROM fb_standings WHERE competition_id = ? AND season = ?) ORDER BY group_name, rank`,
		f.CompetitionID, f.Season, f.CompetitionID, f.Season).Scan(&rows)
	out := &StandingsOut{CompetitionID: f.CompetitionID, Season: f.Season, Table: []StandingOut{}, Source: "API-Football standings snapshot"}
	out.History.Home, out.History.Away = []PosPoint{}, []PosPoint{}
	if len(rows) == 0 {
		// No snapshot: build the table from stored results.
		rows = s.computedTable(f.CompetitionID, f.Season, s.Now())
		out.Source = "computed from stored results (no provider table yet)"
		if len(rows) == 0 {
			return nil
		}
	}
	group := ""
	for _, r := range rows {
		if r.TeamID == f.HomeID || r.TeamID == f.AwayID {
			group = r.GroupName
			break
		}
	}
	for _, r := range rows {
		if group != "" && r.GroupName != group {
			continue
		}
		t := s.teamRef(r.TeamID)
		so := StandingOut{TeamID: r.TeamID, Name: t.Name, Logo: t.Logo, Rank: r.Rank, Points: r.Points, Played: r.Played, Won: r.Won, Drawn: r.Drawn, Lost: r.Lost,
			GF: r.GF, GA: r.GA, GD: r.GD, Form: r.Form, Description: r.Description, Home: r.Home, Away: r.Away, Group: r.GroupName}
		out.Table = append(out.Table, so)
		if r.TeamID == f.HomeID {
			x := so
			out.Home = &x
		}
		if r.TeamID == f.AwayID {
			x := so
			out.Away = &x
		}
		if !r.FetchedAt.IsZero() && (out.AsOf == nil || r.FetchedAt.After(*out.AsOf)) {
			t := r.FetchedAt
			out.AsOf = &t
		}
	}
	out.TableSize = len(out.Table)
	// Position history: provider snapshots when there are several, else
	// replayed from results.
	var dates []time.Time
	s.DB.Model(&Standing{}).Where("competition_id = ? AND season = ? AND source = ?", f.CompetitionID, f.Season, ProviderAPIFootball).Distinct("as_of").Order("as_of").Pluck("as_of", &dates)
	if len(dates) >= 3 {
		var hist []Standing
		s.DB.Where("competition_id = ? AND season = ? AND team_id IN ? AND source = ?", f.CompetitionID, f.Season, []int64{f.HomeID, f.AwayID}, ProviderAPIFootball).Order("as_of").Find(&hist)
		for _, h := range hist {
			p := PosPoint{Date: h.AsOf, Played: h.Played, Rank: h.Rank, Points: h.Points}
			if h.TeamID == f.HomeID {
				out.History.Home = append(out.History.Home, p)
			} else {
				out.History.Away = append(out.History.Away, p)
			}
		}
		out.History.Source = "API-Football standings snapshots"
	} else {
		out.History.Home, out.History.Away = s.replayPositions(f.CompetitionID, f.Season, f.HomeID, f.AwayID, minTime(f.Kickoff, s.Now()))
		out.History.Source = "replayed from stored results (points, goal difference, goals scored)"
	}
	return out
}

// tableRow accumulates a computed table.
type tableRow struct {
	team               int64
	p, w, d, l, gf, ga int
	home, away         Record
}

func rankRows(m map[int64]*tableRow) []*tableRow {
	rs := make([]*tableRow, 0, len(m))
	for _, r := range m {
		rs = append(rs, r)
	}
	pts := func(r *tableRow) int { return 3*r.w + r.d }
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if pts(a) != pts(b) {
			return pts(a) > pts(b)
		}
		if a.gf-a.ga != b.gf-b.ga {
			return a.gf-a.ga > b.gf-b.ga
		}
		if a.gf != b.gf {
			return a.gf > b.gf
		}
		return a.team < b.team
	})
	return rs
}

func (s *Service) seasonResults(comp int64, season int, before time.Time) []resultRow {
	var rows []resultRow
	s.DB.Raw(`SELECT id, kickoff, competition_id, season, home_id, away_id, ft_home, ft_away FROM fb_fixtures
		WHERE competition_id = ? AND season = ? AND status IN ('FT','AET','PEN') AND ft_home IS NOT NULL AND kickoff < ? ORDER BY kickoff, id`, comp, season, before).Scan(&rows)
	return rows
}

func addResult(m map[int64]*tableRow, r resultRow) {
	for _, t := range []int64{r.HomeID, r.AwayID} {
		if m[t] == nil {
			m[t] = &tableRow{team: t}
		}
	}
	h, a := m[r.HomeID], m[r.AwayID]
	h.p++
	a.p++
	h.gf += r.FTHome
	h.ga += r.FTAway
	a.gf += r.FTAway
	a.ga += r.FTHome
	h.home.Played++
	a.away.Played++
	h.home.GF += r.FTHome
	h.home.GA += r.FTAway
	a.away.GF += r.FTAway
	a.away.GA += r.FTHome
	switch {
	case r.FTHome > r.FTAway:
		h.w++
		a.l++
		h.home.Won++
		a.away.Lost++
	case r.FTHome == r.FTAway:
		h.d++
		a.d++
		h.home.Drawn++
		a.away.Drawn++
	default:
		a.w++
		h.l++
		a.away.Won++
		h.home.Lost++
	}
}

// computedTable builds a table from stored results (league competitions).
func (s *Service) computedTable(comp int64, season int, before time.Time) []Standing {
	m := map[int64]*tableRow{}
	for _, r := range s.seasonResults(comp, season, before) {
		addResult(m, r)
	}
	out := []Standing{}
	for i, r := range rankRows(m) {
		out = append(out, Standing{CompetitionID: comp, Season: season, TeamID: r.team, Rank: i + 1, Points: 3*r.w + r.d, Played: r.p, Won: r.w, Drawn: r.d,
			Lost: r.l, GF: r.gf, GA: r.ga, GD: r.gf - r.ga, Home: r.home, Away: r.away, Source: "computed", GroupName: ""})
	}
	return out
}

// replayPositions: each team's rank after each of its matches this season.
func (s *Service) replayPositions(comp int64, season int, home, away int64, before time.Time) ([]PosPoint, []PosPoint) {
	m := map[int64]*tableRow{}
	hs, as := []PosPoint{}, []PosPoint{}
	for _, r := range s.seasonResults(comp, season, before) {
		addResult(m, r)
		if r.HomeID != home && r.AwayID != home && r.HomeID != away && r.AwayID != away {
			continue
		}
		rk := rankRows(m)
		for i, x := range rk {
			p := PosPoint{Date: r.Kickoff, Played: x.p, Rank: i + 1, Points: 3*x.w + x.d}
			if x.team == home && (r.HomeID == home || r.AwayID == home) {
				hs = append(hs, p)
			}
			if x.team == away && (r.HomeID == away || r.AwayID == away) {
				as = append(as, p)
			}
		}
	}
	return hs, as
}

// StandingsHistory returns a competition's tables over time (provider
// snapshots, one per day they were fetched).
func (s *Service) StandingsHistory(comp int64, season int) map[string][]Standing {
	var rows []Standing
	s.DB.Where("competition_id = ? AND season = ?", comp, season).Order("as_of, group_name, rank").Find(&rows)
	out := map[string][]Standing{}
	for _, r := range rows {
		k := r.AsOf.Format("2006-01-02")
		out[k] = append(out[k], r)
	}
	return out
}

func (s *Service) h2hSection(f Fixture) H2HOut {
	out := H2HOut{}
	out.Matches = out.Matches[:0]
	a, b := f.HomeID, f.AwayID
	if a > b {
		a, b = b, a
	}
	var h H2H
	if s.DB.Where("team_a = ? AND team_b = ?", a, b).Limit(1).Find(&h).RowsAffected > 0 {
		t := h.FetchedAt
		out.FetchedAt = &t
	}
	var fx []Fixture
	s.DB.Where("((home_id = ? AND away_id = ?) OR (home_id = ? AND away_id = ?)) AND status IN ('FT','AET','PEN') AND ft_home IS NOT NULL AND id <> ? AND kickoff < ?",
		f.HomeID, f.AwayID, f.AwayID, f.HomeID, f.ID, minTime(f.Kickoff, s.Now().Add(time.Minute))).Order("kickoff DESC").Limit(10).Find(&fx)
	for _, x := range fx {
		m := struct {
			FixtureID   int64     `json:"fixture_id"`
			Date        time.Time `json:"date"`
			Competition string    `json:"competition"`
			Home        TeamRef   `json:"home"`
			Away        TeamRef   `json:"away"`
			HG          int       `json:"hg"`
			AG          int       `json:"ag"`
			WinnerID    *int64    `json:"winner_id"`
		}{FixtureID: x.ID, Date: x.Kickoff, Competition: s.compName(x.CompetitionID), Home: s.teamRef(x.HomeID), Away: s.teamRef(x.AwayID), HG: *x.FTHome, AG: *x.FTAway}
		hg, ag := *x.FTHome, *x.FTAway
		// From this match's home team's side.
		gfHome, gfAway := hg, ag
		if x.HomeID != f.HomeID {
			gfHome, gfAway = ag, hg
		}
		switch {
		case hg > ag:
			m.WinnerID = ptr(x.HomeID)
		case ag > hg:
			m.WinnerID = ptr(x.AwayID)
		}
		switch {
		case gfHome > gfAway:
			out.Summary.HomeWins++
		case gfHome == gfAway:
			out.Summary.Draws++
		default:
			out.Summary.AwayWins++
		}
		out.Summary.HomeGoals += gfHome
		out.Summary.AwayGoals += gfAway
		if hg > 0 && ag > 0 {
			out.Summary.BTTSRate++
		}
		if hg+ag > 2 {
			out.Summary.Over25++
		}
		out.Matches = append(out.Matches, m)
	}
	out.Summary.N = len(out.Matches)
	if n := float64(out.Summary.N); n > 0 {
		out.Summary.AvgGoals = round4(float64(out.Summary.HomeGoals+out.Summary.AwayGoals) / n)
		out.Summary.BTTSRate = round4(out.Summary.BTTSRate / n)
		out.Summary.Over25 = round4(out.Summary.Over25 / n)
	}
	return out
}

// playersSection: a team's key players (most goal involvements and
// minutes this season) with recent form and availability.
func (s *Service) playersSection(f Fixture, team int64, att, def float64, why string) TeamPlayers {
	tp := TeamPlayers{Key: []PlayerOut{}, Missing: []PlayerOut{}}
	if att == 0 {
		att = 1
	}
	if def == 0 {
		def = 1
	}
	tp.Impact.AttackMult, tp.Impact.DefenceMult, tp.Impact.Why = round4(att), round4(def), why
	type agg struct {
		PlayerID  int64
		Minutes   int
		Apps      int
		Lineups   int
		Goals     int
		Assists   int
		Shots     int
		KeyPasses int
		Rating    *float64
		Position  string
		FetchedAt time.Time
	}
	var rows []agg
	s.DB.Raw(`SELECT player_id, SUM(minutes) AS minutes, SUM(appearances) AS apps, SUM(lineups) AS lineups, SUM(goals) AS goals, SUM(assists) AS assists,
		SUM(shots) AS shots, SUM(key_passes) AS key_passes, AVG(rating) AS rating, MAX(position) AS position, MAX(fetched_at) AS fetched_at
		FROM fb_player_seasons WHERE team_id = ? AND season = ? GROUP BY player_id`, team, f.Season).Scan(&rows)
	teamInv := 0
	for _, r := range rows {
		teamInv += r.Goals + r.Assists
		t := r.FetchedAt
		tp.FetchedAt = latestTime(tp.FetchedAt, &t)
	}
	sort.Slice(rows, func(i, j int) bool {
		si := float64(rows[i].Goals+rows[i].Assists)*100 + float64(rows[i].Minutes)/30
		sj := float64(rows[j].Goals+rows[j].Assists)*100 + float64(rows[j].Minutes)/30
		return si > sj
	})
	var inj []Injury
	s.DB.Where("fixture_id = ? AND team_id = ?", f.ID, team).Find(&inj)
	injBy := map[int64]Injury{}
	for _, i := range inj {
		injBy[i.PlayerID] = i
	}
	var lu Lineup
	haveLU := s.DB.Where("fixture_id = ? AND team_id = ?", f.ID, team).Limit(1).Find(&lu).RowsAffected > 0 && len(lu.StartXI) > 0
	inXI, onBench := map[int64]bool{}, map[int64]bool{}
	for _, p := range lu.StartXI {
		inXI[p.ID] = true
	}
	for _, p := range lu.Substitutes {
		onBench[p.ID] = true
	}
	build := func(r agg) PlayerOut {
		var pl Player
		s.DB.Where("id = ?", r.PlayerID).Limit(1).Find(&pl)
		po := PlayerOut{ID: r.PlayerID, Name: pl.Name, Photo: pl.Photo, Position: r.Position, Age: pl.Age, Availability: "available"}
		po.Season.Appearances, po.Season.Lineups, po.Season.Minutes, po.Season.Goals, po.Season.Assists = r.Apps, r.Lineups, r.Minutes, r.Goals, r.Assists
		po.Season.Shots, po.Season.KeyPasses = r.Shots, r.KeyPasses
		if r.Rating != nil {
			po.Season.Rating = ptr(math.Round(*r.Rating*100) / 100)
		}
		if r.Minutes > 0 {
			po.Season.GoalsPer90 = round4(float64(r.Goals) / float64(r.Minutes) * 90)
		}
		if teamInv > 0 {
			po.Season.Share = round4(float64(r.Goals+r.Assists) / float64(teamInv))
		}
		var recent []FixturePlayer
		s.DB.Raw(`SELECT fp.* FROM fb_fixture_players fp JOIN fb_fixtures fx ON fx.id = fp.fixture_id WHERE fp.player_id = ? AND fx.kickoff < ? ORDER BY fx.kickoff DESC LIMIT 5`,
			r.PlayerID, minTime(f.Kickoff, s.Now())).Scan(&recent)
		var rs float64
		nr := 0
		for _, x := range recent {
			po.Recent.Matches++
			po.Recent.Minutes += x.Minutes
			po.Recent.Goals += x.Goals
			po.Recent.Assists += x.Assists
			if x.Rating != nil {
				rs += *x.Rating
				nr++
			}
		}
		if nr > 0 {
			po.Recent.Rating = ptr(math.Round(rs/float64(nr)*100) / 100)
		}
		if i, ok := injBy[r.PlayerID]; ok {
			po.Availability, po.Reason = i.Status, i.Reason
		}
		if haveLU {
			in := inXI[r.PlayerID]
			po.InLineup = &in
			po.ExpectedXI = &in
			if !in && !onBench[r.PlayerID] && po.Availability == "available" {
				po.Reason = "not in the squad for this match"
			}
		} else if po.Availability == "available" && r.Apps > 0 {
			exp := float64(r.Lineups)/float64(maxInt(r.Apps, 1)) >= 0.6
			po.ExpectedXI = &exp
		} else if po.Availability != "available" {
			no := false
			po.ExpectedXI = &no
		}
		return po
	}
	for i, r := range rows {
		if i >= 8 {
			break
		}
		tp.Key = append(tp.Key, build(r))
	}
	for _, i := range inj {
		found := false
		for _, r := range rows {
			if r.PlayerID == i.PlayerID {
				tp.Missing = append(tp.Missing, build(r))
				found = true
				break
			}
		}
		if !found {
			tp.Missing = append(tp.Missing, PlayerOut{ID: i.PlayerID, Name: i.PlayerName, Availability: i.Status, Reason: i.Reason, ExpectedXI: ptr(false)})
		}
	}
	return tp
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Service) liveSection(f Fixture) *LiveOut {
	l := &LiveOut{Status: f.Status, Minute: f.Elapsed, Extra: f.ElapsedExtra, Score: scoreOf(f), Events: []EventOut{}, Stats: map[string]*FixtureStat{}, UpdatedAt: f.FetchedAt, Prices: []PriceOut{}}
	var evs []FixtureEvent
	s.DB.Where("fixture_id = ?", f.ID).Order("minute, seq").Find(&evs)
	for _, e := range evs {
		sd := "away"
		if e.TeamID == f.HomeID {
			sd = "home"
		}
		l.Events = append(l.Events, EventOut{Minute: e.Minute, Extra: e.Extra, Side: sd, TeamID: e.TeamID, Type: e.Type, Detail: e.Detail, Player: e.PlayerName, Assist: e.AssistName})
	}
	var sts []FixtureStat
	s.DB.Where("fixture_id = ?", f.ID).Find(&sts)
	for i := range sts {
		x := sts[i]
		if x.TeamID == f.HomeID {
			l.Stats["home"] = &x
		} else {
			l.Stats["away"] = &x
		}
	}
	var lo []Odd
	s.DB.Where("fixture_id = ? AND live", f.ID).Order("fetched_at DESC").Limit(40).Find(&lo)
	for _, o := range lo {
		l.Prices = append(l.Prices, PriceOut{Bookmaker: o.Bookmaker, Title: o.BookmakerTitle + " — " + SelectionLabel(o.Market, o.Selection, o.Line, "", ""), Source: o.Source, Price: o.Price, UpdatedAt: o.ProviderUpdatedAt, FetchedAt: o.FetchedAt})
	}
	return l
}

// ---------------------------------------------------------------- lists

// FixtureRow is a fixture in a list.
type FixtureRow struct {
	ID          int64     `json:"id"`
	Kickoff     time.Time `json:"kickoff"`
	Status      string    `json:"status"`
	Elapsed     *int      `json:"elapsed"`
	Competition CompRef   `json:"competition"`
	Home        TeamRef   `json:"home"`
	Away        TeamRef   `json:"away"`
	Score       ScoreOut  `json:"score"`
	Model       *struct {
		Home float64 `json:"home"`
		Draw float64 `json:"draw"`
		Away float64 `json:"away"`
		Kind string  `json:"kind"`
	} `json:"model"`
	Best         map[string]*PriceOut `json:"best_odds"` // home / draw / away
	Market       map[string]float64   `json:"market_1x2"`
	MarketSource string               `json:"market_source"`
	Edges        []ValueOut           `json:"edges"` // selections with value
	ValueCount   int                  `json:"value_count"`
	FetchedAt    time.Time            `json:"fetched_at"`
	OddsAt       *time.Time           `json:"odds_at"`
	Stale        bool                 `json:"stale"`
	Watched      bool                 `json:"watched"`
	TeamNews     *TeamNewsCount       `json:"team_news,omitempty"` // v2.17: confirmed injured/suspended facts per side (freeanalysis.go)
	// v2.18 (win_freeze.go): the one-line verdict and the ✓/✗ after the game.
	Verdict     *Verdict     `json:"verdict"`
	ResultCheck *ResultCheck `json:"result_check"`
	// v2.19.1 (lock.go): the lock ("Locked 2 h 10 min before kick-off"), why
	// an upcoming match has no pick, and why the model's numbers are hidden
	// (model is null when it knows nothing about the teams).
	Lock      *LockOut   `json:"lock"`
	NoPick    *NoPickOut `json:"no_pick"`
	ModelNote string     `json:"model_note,omitempty"`
}

// FixtureQuery filters the list.
type FixtureQuery struct {
	Date        string
	From, To    *time.Time
	Competition int64
	Status      string // upcoming | live | finished | all | an API-Football code
	Q           string
	Limit       int
	Org, User   string
	TZ          *time.Location
	MinTier     string // v2.18: keep rows whose verdict tier is at least this
}

// Fixtures lists fixtures with the model's probabilities, best odds and
// value flags.
func (s *Service) Fixtures(q FixtureQuery) ([]FixtureRow, error) {
	now := s.Now()
	loc := q.TZ
	if loc == nil {
		loc = time.UTC
	}
	db := s.DB.Model(&Fixture{}).Joins("JOIN fb_competitions c ON c.id = fb_fixtures.competition_id")
	switch {
	case q.Date != "":
		d, err := time.ParseInLocation("2006-01-02", q.Date, loc)
		if err != nil {
			return nil, inputErr("Use a date like 2026-11-21.")
		}
		db = db.Where("fb_fixtures.kickoff >= ? AND fb_fixtures.kickoff < ?", d.UTC(), d.AddDate(0, 0, 1).UTC())
	case q.From != nil || q.To != nil:
		if q.From != nil {
			db = db.Where("fb_fixtures.kickoff >= ?", *q.From)
		}
		if q.To != nil {
			db = db.Where("fb_fixtures.kickoff < ?", *q.To)
		}
	default:
		st, _ := s.Settings()
		db = db.Where("fb_fixtures.kickoff >= ? AND fb_fixtures.kickoff < ?", now.Add(-3*time.Hour), now.Add(time.Duration(st.LookaheadDays+1)*24*time.Hour))
	}
	if q.Competition != 0 {
		db = db.Where("fb_fixtures.competition_id = ?", q.Competition)
	} else {
		db = db.Where("c.enabled")
	}
	switch q.Status {
	case "", "all":
	case "upcoming":
		db = db.Where("fb_fixtures.status IN ?", []string{"NS", "TBD"})
	case "live":
		db = db.Where("fb_fixtures.status IN ?", keysOfS(liveStatuses))
	case "finished":
		db = db.Where("fb_fixtures.status IN ?", []string{"FT", "AET", "PEN"})
	default:
		db = db.Where("fb_fixtures.status = ?", strings.ToUpper(q.Status))
	}
	if t := strings.TrimSpace(q.Q); t != "" {
		like := "%" + strings.ToLower(t) + "%"
		db = db.Where(`(EXISTS (SELECT 1 FROM fb_teams t WHERE t.id IN (fb_fixtures.home_id, fb_fixtures.away_id) AND LOWER(t.name) LIKE ?) OR LOWER(c.name) LIKE ?)`, like, like)
	}
	if q.Limit <= 0 || q.Limit > 300 {
		q.Limit = 150
	}
	var fx []Fixture
	if err := db.Select("fb_fixtures.*").Order("fb_fixtures.kickoff, c.priority, fb_fixtures.id").Limit(q.Limit).Find(&fx).Error; err != nil {
		return nil, err
	}
	st, _ := s.Settings()
	watched := map[int64]bool{}
	if q.Org != "" {
		var ids []int64
		s.DB.Model(&WatchItem{}).Where("organization_id = ? AND user_id = ? AND kind = 'fixture'", q.Org, q.User).Pluck("ref_id", &ids)
		for _, id := range ids {
			watched[id] = true
		}
	}
	ids := make([]int64, len(fx))
	for i, f := range fx {
		ids[i] = f.ID
	}
	vrows := s.verdictRows(ids) // v2.18 frozen verdicts
	out := []FixtureRow{}
	for _, f := range fx {
		var c Competition
		s.DB.Where("id = ?", f.CompetitionID).Limit(1).Find(&c)
		r := FixtureRow{ID: f.ID, Kickoff: f.Kickoff, Status: f.Status, Elapsed: f.Elapsed, Competition: CompRef{ID: c.ID, Name: c.Name, Country: c.Country, Logo: c.Logo, Type: c.Type, Season: f.Season, Round: f.Round},
			Home: s.teamRef(f.HomeID), Away: s.teamRef(f.AwayID), Score: scoreOf(f), Best: map[string]*PriceOut{}, Edges: []ValueOut{}, FetchedAt: f.FetchedAt,
			OddsAt: latestTime(f.OddsAFAt, f.OddsOAAt), Watched: watched[f.ID]}
		r.Stale = f.Kickoff.After(now) && f.Kickoff.Sub(now) < 24*time.Hour && now.Sub(f.FetchedAt) > 12*time.Hour
		var p Prediction
		var pp *Prediction
		kind := "current"
		if !f.Kickoff.After(now) {
			kind = "frozen"
		}
		var md ModelData
		if s.DB.Where("fixture_id = ? AND kind = ?", f.ID, kind).Limit(1).Find(&p).RowsAffected > 0 {
			md = s.modelDataFor(f, &p) // v2.19.1: no numbers without data (lock_gate.go)
			if !md.Solid {
				r.ModelNote = md.Why
			}
		}
		if md.HasData {
			pp = &p
			r.Model = &struct {
				Home float64 `json:"home"`
				Draw float64 `json:"draw"`
				Away float64 `json:"away"`
				Kind string  `json:"kind"`
			}{round4(p.PHome), round4(p.PDraw), round4(p.PAway), p.Kind}
		}
		var ko *time.Time
		if !f.Kickoff.After(now) {
			k := f.Kickoff
			ko = &k
		}
		mv := s.marketView(f, ko, q.Org)
		for _, so := range mv.Selections {
			if so.Market == Mkt1X2 && so.Best != nil {
				b := *so.Best
				r.Best[so.Selection] = &b
			}
		}
		r.Market, r.MarketSource = mv.Fair1X2, mv.FairSource
		if r.Model != nil && md.Solid && f.Kickoff.After(now) {
			for _, v := range valueRows(p, mv, st.MinEdge, st.KellyFraction, st.KellyCap, 0, "") {
				if v.Value {
					r.Edges = append(r.Edges, v)
				}
			}
			r.ValueCount = len(r.Edges)
			if len(r.Edges) > 3 {
				r.Edges = r.Edges[:3]
			}
		}
		var vr *VerdictRow
		if x, ok := vrows[f.ID]; ok {
			vr = &x
		}
		lv := s.fixtureLock(f, c, mv, pp, vr) // v2.19.1: lock on view, lock, no-pick reason (lock.go)
		r.Verdict, r.ResultCheck, r.Lock, r.NoPick = lv.Verdict, lv.Check, lv.Lock, lv.NoPick
		if q.MinTier != "" && (r.Verdict == nil || tierRank(r.Verdict.Tier.Key) > tierRank(q.MinTier)) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func keysOfS(m map[string]bool) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ValueBet is one value selection in the value list.
type ValueBet struct {
	ValueOut
	FixtureID   int64     `json:"fixture_id"`
	Kickoff     time.Time `json:"kickoff"`
	Competition string    `json:"competition"`
	Home        string    `json:"home"`
	Away        string    `json:"away"`
}

// ValueList lists selections where the model's probability is clearly
// above the de-margined market (min edge) and the best price has positive
// expected value.
func (s *Service) ValueList(date string, minEdge float64, org, user string, loc *time.Location) ([]ValueBet, error) {
	st, _ := s.Settings()
	if minEdge <= 0 {
		minEdge = st.MinEdge
	}
	q := FixtureQuery{Status: "upcoming", Org: org, User: user, TZ: loc, Limit: 300}
	if date != "" {
		q.Date = date
	}
	fx, err := s.Fixtures(q)
	if err != nil {
		return nil, err
	}
	bal := s.bankrollBalance(org, user)
	b := 0.0
	if bal != nil {
		b = *bal
	}
	cur := s.currencyOf(org)
	out := []ValueBet{}
	for _, r := range fx {
		var f Fixture
		if s.DB.Where("id = ?", r.ID).Limit(1).Find(&f).RowsAffected == 0 {
			continue
		}
		p, ok := s.CurrentPrediction(f)
		if !ok {
			continue
		}
		mv := s.marketView(f, nil, org)
		for _, v := range valueRows(p, mv, minEdge, st.KellyFraction, st.KellyCap, b, cur) {
			if v.Value {
				out = append(out, ValueBet{ValueOut: v, FixtureID: f.ID, Kickoff: f.Kickoff, Competition: r.Competition.Name, Home: r.Home.Name, Away: r.Away.Name})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return *out[i].EV > *out[j].EV })
	return out, nil
}

// TeamProfile is GET /teams/:id.
type TeamProfile struct {
	Team      Team               `json:"team"`
	Elo       float64            `json:"elo"`
	Standings []StandingOut      `json:"standings"`
	Form      map[string]FormOut `json:"form"`
	Recent    []ResultOut        `json:"recent"`
	Upcoming  []FixtureRow       `json:"upcoming"`
	Players   []PlayerSeason     `json:"players"`
	Series    []SeriesPoint      `json:"series"`
}

// Team builds a team's season profile.
func (s *Service) TeamProfile(id int64) (*TeamProfile, error) {
	var t Team
	if s.DB.Where("id = ?", id).Limit(1).Find(&t).RowsAffected == 0 {
		return nil, ErrNotFound
	}
	now := s.Now()
	elo := s.eloNow()
	hist := s.teamHistory(id, now, 60)
	tp := &TeamProfile{Team: t, Elo: math.Round(elo.Rating(tid(id))*10) / 10, Standings: []StandingOut{}, Players: []PlayerSeason{}, Upcoming: []FixtureRow{}}
	tp.Form = map[string]FormOut{"last5": formOut(hist, id, 5, elo), "last10": formOut(hist, id, 10, elo)}
	tp.Recent = s.recentRows(hist, id, 10)
	tp.Series = seriesOf(hist, id, 20, 5)
	var rows []Standing
	s.DB.Raw(`SELECT DISTINCT ON (competition_id) * FROM fb_standings WHERE team_id = ? ORDER BY competition_id, as_of DESC`, id).Scan(&rows)
	for _, r := range rows {
		tp.Standings = append(tp.Standings, StandingOut{TeamID: r.TeamID, Name: s.compName(r.CompetitionID), Rank: r.Rank, Points: r.Points, Played: r.Played,
			Won: r.Won, Drawn: r.Drawn, Lost: r.Lost, GF: r.GF, GA: r.GA, GD: r.GD, Form: r.Form, Description: r.Description, Home: r.Home, Away: r.Away, Group: r.GroupName})
	}
	s.DB.Where("team_id = ?", id).Order("season DESC, goals DESC, minutes DESC").Limit(40).Find(&tp.Players)
	var up []Fixture
	s.DB.Where("(home_id = ? OR away_id = ?) AND kickoff > ?", id, id, now).Order("kickoff").Limit(5).Find(&up)
	for _, f := range up {
		tp.Upcoming = append(tp.Upcoming, FixtureRow{ID: f.ID, Kickoff: f.Kickoff, Status: f.Status, Home: s.teamRef(f.HomeID), Away: s.teamRef(f.AwayID),
			Competition: CompRef{ID: f.CompetitionID, Name: s.compName(f.CompetitionID), Season: f.Season, Round: f.Round}, Best: map[string]*PriceOut{}, Edges: []ValueOut{}})
	}
	return tp, nil
}

// Live lists fixtures in play now (enabled competitions).
func (s *Service) Live() []LiveRow {
	var fx []Fixture
	s.DB.Joins("JOIN fb_competitions c ON c.id = fb_fixtures.competition_id AND c.enabled").Where("fb_fixtures.status IN ?", keysOfS(liveStatuses)).
		Order("fb_fixtures.kickoff").Find(&fx)
	out := []LiveRow{}
	for _, f := range fx {
		out = append(out, LiveRow{FixtureRow: FixtureRow{ID: f.ID, Kickoff: f.Kickoff, Status: f.Status, Elapsed: f.Elapsed, Home: s.teamRef(f.HomeID), Away: s.teamRef(f.AwayID),
			Score: scoreOf(f), Competition: CompRef{ID: f.CompetitionID, Name: s.compName(f.CompetitionID), Season: f.Season, Round: f.Round}, Best: map[string]*PriceOut{},
			Edges: []ValueOut{}, FetchedAt: f.FetchedAt}, Live: s.liveSection(f)})
	}
	return out
}

// LiveRow is a fixture in play with its live section.
type LiveRow struct {
	FixtureRow
	Live *LiveOut `json:"live"`
}
