package football

// Football v3 (v2.18): simple picks, honest win rate, ✓/✗ results and more
// open sources (contract: /home/claude/tools/football-winrate-contract.md).
//
//   - verdict.go: the one-line verdict (pick, tier with its history hit rate,
//     safer double chance, draw alert, goals, both teams to score, top scores)
//     and the ✓/✗ result check — pure functions.
//   - win_consensus.go: the consensus probability per fixture (sharp, market,
//     Polymarket, ClubElo, DASYIN's model, API-Football's prediction) with
//     default or learned weights (fb_consensus_weights).
//   - win_freeze.go: freezing verdicts before kickoff (fb_verdicts) and
//     grading them after full time (API-Football, fallback openfootball /
//     football-data.org); the verdict and result check on lists and pages.
//   - scorecard.go: GET /scorecard and GET /picks.
//   - jackpot_win.go: per-game verdicts and ✓/✗, the frozen snapshot, the
//     review and the budget planner of jackpots.
//   - apfpred.go (API-Football predictions), polymarket.go (Gamma API),
//     openfootball.go (football.json): the new sources, all off by default.
//   - history_tables.go: hit-rate tables from Club Football Match Data.
//   - win_handler.go: HTTP routes.

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// New sources (fb_fetch_log.provider / fb_quota.provider / settings keys).
const (
	ProviderAFPred       = "apifootball_pred" // calls are logged as apifootball (endpoint predictions); usage counted here too
	ProviderPolymarket   = "polymarket"
	ProviderOpenFootball = "openfootball"
)

// Production endpoints and docs of the new sources.
const (
	DefaultPolymarketBase   = "https://gamma-api.polymarket.com"                                    // https://docs.polymarket.com/api-spec/gamma-openapi.yaml (servers)
	DefaultOpenFootballBase = "https://raw.githubusercontent.com/openfootball/football.json/master" // README: "Use the raw links served by GitHub"
	docPolymarket           = "https://docs.polymarket.com"
	docOpenFootball         = "https://github.com/openfootball/football.json"
	docAFPred               = "https://www.api-football.com/documentation-v3#tag/Predictions"
)

// SourceProb is an external source's 1X2 probability for a fixture
// (Polymarket mid prices, API-Football's prediction). Global.
type SourceProb struct {
	FixtureID int64          `json:"fixture_id" gorm:"primaryKey;autoIncrement:false"`
	Source    string         `json:"source" gorm:"primaryKey;type:varchar(20)"`
	PHome     float64        `json:"p_home"`
	PDraw     float64        `json:"p_draw"`
	PAway     float64        `json:"p_away"`
	Detail    map[string]any `json:"detail" gorm:"type:text;serializer:json"`
	URL       string         `json:"url" gorm:"type:text"`
	FetchedAt time.Time      `json:"fetched_at" gorm:"index"`
}

func (SourceProb) TableName() string { return "fb_source_probs" }

// VerdictRow is a fixture's frozen verdict (made with the frozen model
// prediction, refreshed at every pre-kickoff odds refresh, final at kickoff)
// and its marks after full time. Global: everybody sees DASYIN's picks.
type VerdictRow struct {
	FixtureID     int64        `json:"fixture_id" gorm:"primaryKey;autoIncrement:false"`
	CompetitionID int64        `json:"competition_id" gorm:"index"`
	Kickoff       time.Time    `json:"kickoff" gorm:"index"`
	Grp           string       `json:"group" gorm:"column:grp;type:varchar(10)"`
	FrozenAt      time.Time    `json:"frozen_at"`
	InputsAt      *time.Time   `json:"inputs_at"`
	Verdict       Verdict      `json:"verdict" gorm:"type:text;serializer:json"`
	Pick          string       `json:"pick" gorm:"type:varchar(2)"`
	P             float64      `json:"p"`
	Tier          string       `json:"tier" gorm:"type:varchar(10);index"`
	Safer         string       `json:"safer" gorm:"type:varchar(3)"`
	SaferP        float64      `json:"safer_p"`
	GoalsPick     string       `json:"goals_pick" gorm:"type:varchar(6)"`
	GoalsP        *float64     `json:"goals_p"`
	BTTSPick      string       `json:"btts_pick" gorm:"column:btts_pick;type:varchar(4)"`
	BTTSP         *float64     `json:"btts_p" gorm:"column:btts_p"`
	TopScore      string       `json:"top_score" gorm:"type:varchar(8)"`
	TopScoreP     float64      `json:"top_score_p"`
	FavPick       string       `json:"fav_pick" gorm:"type:varchar(2)"` // the bookmakers' favourite at freeze ("" = no market)
	FavP          *float64     `json:"fav_p"`
	NoMarket      bool         `json:"no_market"`
	GradedAt      *time.Time   `json:"graded_at" gorm:"index"`
	Status        string       `json:"status" gorm:"type:varchar(10);index"` // "" | correct | wrong | void
	Score         string       `json:"score" gorm:"type:varchar(8)"`
	Outcome       string       `json:"outcome" gorm:"type:varchar(2)"`
	PickOK        *bool        `json:"pick_ok"`
	SaferOK       *bool        `json:"safer_ok"`
	GoalsOK       *bool        `json:"goals_ok"`
	BTTSOK        *bool        `json:"btts_ok" gorm:"column:btts_ok"`
	ScoreOK       *bool        `json:"score_ok"`
	FavOK         *bool        `json:"fav_ok"`
	ResultSource  string       `json:"result_source" gorm:"type:varchar(14)"`
	Check         *ResultCheck `json:"check" gorm:"column:check_json;type:text;serializer:json"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

func (VerdictRow) TableName() string { return "fb_verdicts" }

// ConsensusWeightsRow is a competition group's learned consensus weights.
type ConsensusWeightsRow struct {
	Grp            string             `json:"group" gorm:"column:grp;primaryKey;type:varchar(10)"`
	Weights        map[string]float64 `json:"weights" gorm:"type:text;serializer:json"`
	InUse          string             `json:"in_use" gorm:"type:varchar(10)"` // learned | default
	N              int                `json:"n"`
	HoldoutN       int                `json:"holdout_n"`
	LoglossDefault *float64           `json:"logloss_default"`
	LoglossLearned *float64           `json:"logloss_learned"`
	Sources        []string           `json:"sources" gorm:"type:text;serializer:json"`
	Note           string             `json:"note" gorm:"type:text"`
	FittedAt       time.Time          `json:"fitted_at"`
}

func (ConsensusWeightsRow) TableName() string { return "fb_consensus_weights" }

// WinSettings holds the v2.18 sources' extra settings (platform, id 1).
type WinSettings struct {
	ID               int        `json:"-" gorm:"primaryKey"`
	PolyMinLiquidity float64    `json:"polymarket_min_liquidity"`
	PolyMaxSpread    float64    `json:"polymarket_max_spread"`
	PolyLeagues      string     `json:"polymarket_leagues" gorm:"type:varchar(400)"`
	OFFiles          string     `json:"openfootball_files" gorm:"column:of_files;type:varchar(200)"`
	PolySports       []PMSport  `json:"-" gorm:"type:text;serializer:json"` // GET /sports, cached a day
	PolySportsAt     *time.Time `json:"-"`
	LastPolyAt       *time.Time `json:"last_polymarket_at"`
	LastOFAt         *time.Time `json:"last_openfootball_at"`
	LastFitAt        *time.Time `json:"last_fit_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func (WinSettings) TableName() string { return "fb_win_settings" }

// PMSport is one GET /sports entry (Gamma API SportsMetadata).
type PMSport struct {
	Sport  string `json:"sport"`
	Tags   string `json:"tags"`
	Series string `json:"series"`
}

// PMEvent is a Polymarket match event and the fixture it matched.
type PMEvent struct {
	ID        string    `json:"id" gorm:"primaryKey;type:varchar(40)"`
	Slug      string    `json:"slug" gorm:"type:varchar(200)"`
	Title     string    `json:"title" gorm:"type:varchar(300)"`
	Sport     string    `json:"sport" gorm:"type:varchar(20)"`
	Home      string    `json:"home" gorm:"type:varchar(160)"`
	Away      string    `json:"away" gorm:"type:varchar(160)"`
	Start     time.Time `json:"start" gorm:"index"`
	FixtureID int64     `json:"fixture_id" gorm:"index"`
	Usable    bool      `json:"usable"`
	Why       string    `json:"why" gorm:"type:varchar(200)"`
	FetchedAt time.Time `json:"fetched_at"`
}

func (PMEvent) TableName() string { return "fb_polymarket_events" }

// OFMatch is an openfootball match (football.json) and the fixture it matched.
type OFMatch struct {
	ID            int64      `json:"id" gorm:"primaryKey"`
	Code          string     `json:"code" gorm:"type:varchar(8);uniqueIndex:idx_fb_of_key,priority:1"`
	Season        string     `json:"season" gorm:"type:varchar(10);uniqueIndex:idx_fb_of_key,priority:2"`
	Date          time.Time  `json:"date" gorm:"type:date;uniqueIndex:idx_fb_of_key,priority:3"`
	Team1         string     `json:"team1" gorm:"type:varchar(120);uniqueIndex:idx_fb_of_key,priority:4"`
	Team2         string     `json:"team2" gorm:"type:varchar(120);uniqueIndex:idx_fb_of_key,priority:5"`
	Round         string     `json:"round" gorm:"type:varchar(60)"`
	Kickoff       *time.Time `json:"kickoff"`
	HG            *int       `json:"hg"`
	AG            *int       `json:"ag"`
	CompetitionID int64      `json:"competition_id" gorm:"index"`
	HomeID        int64      `json:"home_id"`
	AwayID        int64      `json:"away_id"`
	FixtureID     int64      `json:"fixture_id" gorm:"index"`
	FetchedAt     time.Time  `json:"fetched_at"`
}

func (OFMatch) TableName() string { return "fb_openfootball_matches" }

// WinModels lists the v2.18 tables (all global: no organisation data).
func WinModels() []any {
	return []any{&SourceProb{}, &VerdictRow{}, &ConsensusWeightsRow{}, &WinSettings{}, &PMEvent{}, &OFMatch{}}
}

// DefaultPolyLeagues maps Polymarket sport codes (GET /sports "sport") to
// competitions. Editable in the settings ("epl:39,lal:140,…").
const DefaultPolyLeagues = "epl:39,lal:140,bun:78,sea:135,fl1:61,ucl:2,uel:3"

// DefaultOFFiles are the football.json files fetched (README league codes:
// English, German, Spanish, Italian and French top two divisions).
const DefaultOFFiles = "en.1,en.2,de.1,de.2,es.1,es.2,it.1,it.2,fr.1,fr.2"

func defaultWinSettings() WinSettings {
	return WinSettings{ID: 1, PolyMinLiquidity: 5000, PolyMaxSpread: 0.04, PolyLeagues: DefaultPolyLeagues, OFFiles: DefaultOFFiles}
}

func seedWin(db *gorm.DB) error {
	d := defaultWinSettings()
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&d).Error
}

// WinSettingsGet returns the v2.18 settings (defaults filled in).
func (s *Service) WinSettingsGet() WinSettings {
	var st WinSettings
	if s.DB.Where("id = 1").Limit(1).Find(&st).RowsAffected == 0 {
		st = defaultWinSettings()
		s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&st)
	}
	d := defaultWinSettings()
	if st.PolyMinLiquidity <= 0 {
		st.PolyMinLiquidity = d.PolyMinLiquidity
	}
	if st.PolyMaxSpread <= 0 {
		st.PolyMaxSpread = d.PolyMaxSpread
	}
	if st.PolyLeagues == "" {
		st.PolyLeagues = d.PolyLeagues
	}
	if st.OFFiles == "" {
		st.OFFiles = d.OFFiles
	}
	return st
}
