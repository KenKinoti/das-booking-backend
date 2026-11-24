// Package football is the football (soccer) match analysis and bet tracker
// (v2.16, /api/v1/football/…): fixtures, results, standings, players,
// injuries, line-ups and odds fetched from API-Football v3
// (https://www.api-football.com/documentation-v3) and The Odds API v4
// (https://the-odds-api.com/liveapi/guides/v4/), models fitted per
// competition (pkg/football/model: Dixon–Coles, Elo), predictions frozen
// before kickoff and graded after full time, and a personal bet tracker.
//
// DASYIN never takes bets or money: people bet at their own bookmaker and
// record the bet here. Football data is global (fetched once, shared by every
// organisation); bets, bankroll, limits, watchlists and odds typed in by hand
// belong to a person in an organisation.
//
// Files: models.go (tables), settings.go (platform settings, competitions),
// apifootball.go / oddsapi.go (provider clients), quota.go (budgets, fetch
// log), ingest.go (upserts), matching.go (The Odds API events → fixtures),
// planner.go / scheduler.go (what to fetch when), markets.go (odds, de-vig,
// consensus), predict.go (models, freeze, grading, track record),
// analysis.go (the match page payload), bets.go / settle.go / limits.go
// (bet tracker), handler.go (HTTP API).
package football

import (
	"time"

	"gorm.io/gorm"
)

// Providers.
const (
	ProviderAPIFootball = "apifootball"
	ProviderOddsAPI     = "oddsapi"
	ProviderManual      = "manual"
)

// ---------------------------------------------------------------- platform

// Settings is the platform's single settings row (id 1). Keys are stored
// encrypted (mail.EncryptSecret) and are never returned.
type Settings struct {
	ID               int        `json:"-" gorm:"primaryKey"`
	APIFootballKey   string     `json:"-" gorm:"column:apifootball_key;type:text"`
	OddsAPIKey       string     `json:"-" gorm:"column:oddsapi_key;type:text"`
	APIFootballPlan  string     `json:"apifootball_plan" gorm:"column:apifootball_plan;type:varchar(20);default:'free'"`
	APIFootballDaily int        `json:"apifootball_daily_budget" gorm:"column:apifootball_daily_budget;default:90"`
	OddsAPIMonthly   int        `json:"oddsapi_monthly_budget" gorm:"column:oddsapi_monthly_budget;default:450"`
	OddsRegions      string     `json:"odds_regions" gorm:"type:varchar(40);default:'uk,eu'"`
	OddsMarkets      string     `json:"odds_markets" gorm:"type:varchar(60);default:'h2h,totals,spreads'"`
	OddsSnapshots    string     `json:"odds_snapshots" gorm:"type:varchar(60);default:'1440,360,60,10'"` // minutes before kickoff
	LivePolling      bool       `json:"live_polling" gorm:"default:true"`
	LiveIntervalSec  int        `json:"live_interval_sec" gorm:"default:60"`
	LookaheadDays    int        `json:"lookahead_days" gorm:"default:3"`
	SchedulerOn      bool       `json:"scheduler_on" gorm:"default:true"`
	MinEdge          float64    `json:"min_edge" gorm:"default:0.03"`
	KellyFraction    float64    `json:"kelly_fraction" gorm:"default:0.25"`
	KellyCap         float64    `json:"kelly_cap" gorm:"default:0.02"`
	AFBookmakers     []string   `json:"apifootball_bookmakers" gorm:"column:af_bookmakers;type:text;serializer:json"` // GET /odds/bookmakers (monthly)
	LastRunAt        *time.Time `json:"last_run_at"`
	LastRun          RunSummary `json:"last_run" gorm:"type:text;serializer:json"`
	UpdatedBy        string     `json:"updated_by" gorm:"type:varchar(255)"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func (Settings) TableName() string { return "fb_settings" }

// Coverage flags of a competition season (API-Football /leagues
// seasons[].coverage).
type Coverage struct {
	Events            bool `json:"events"`
	Lineups           bool `json:"lineups"`
	StatisticsFixture bool `json:"statistics_fixtures"`
	StatisticsPlayers bool `json:"statistics_players"`
	Standings         bool `json:"standings"`
	Players           bool `json:"players"`
	TopScorers        bool `json:"top_scorers"`
	Injuries          bool `json:"injuries"`
	Predictions       bool `json:"predictions"`
	Odds              bool `json:"odds"`
}

// Competition is an API-Football league or cup (id = API-Football league id).
type Competition struct {
	ID           int64      `json:"id" gorm:"primaryKey;autoIncrement:false"`
	Name         string     `json:"name" gorm:"type:varchar(160)"`
	Country      string     `json:"country" gorm:"type:varchar(80)"`
	CountryCode  string     `json:"country_code" gorm:"type:varchar(10)"`
	Type         string     `json:"type" gorm:"type:varchar(20)"` // League | Cup
	Logo         string     `json:"logo" gorm:"type:text"`
	Flag         string     `json:"flag" gorm:"type:text"`
	Season       int        `json:"season"`
	SeasonStart  *time.Time `json:"season_start"`
	SeasonEnd    *time.Time `json:"season_end"`
	Coverage     Coverage   `json:"coverage" gorm:"type:text;serializer:json"`
	Enabled      bool       `json:"enabled" gorm:"index"`
	Priority     int        `json:"priority" gorm:"default:50"`               // 1 = most important
	OddsSportKey string     `json:"odds_sport_key" gorm:"type:varchar(80)"`   // The Odds API sport key ("" = not covered)
	Group        string     `json:"group" gorm:"column:grp;type:varchar(40)"` // europe_top | uefa | africa | international | other
	Known        bool       `json:"known"`                                    // seen in API-Football's /leagues
	Note         string     `json:"note,omitempty" gorm:"type:text"`
	FetchedAt    *time.Time `json:"fetched_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func (Competition) TableName() string { return "fb_competitions" }

// Team (id = API-Football team id).
type Team struct {
	ID        int64     `json:"id" gorm:"primaryKey;autoIncrement:false"`
	Name      string    `json:"name" gorm:"type:varchar(160);index"`
	Code      string    `json:"code" gorm:"type:varchar(10)"`
	Country   string    `json:"country" gorm:"type:varchar(80)"`
	Logo      string    `json:"logo" gorm:"type:text"`
	National  bool      `json:"national"`
	Founded   int       `json:"founded"`
	Venue     string    `json:"venue" gorm:"type:varchar(160)"`
	City      string    `json:"city" gorm:"type:varchar(120)"`
	FetchedAt time.Time `json:"fetched_at"`
}

func (Team) TableName() string { return "fb_teams" }

// Fixture (id = API-Football fixture id). Scores are null until known;
// FTHome/FTAway is the 90-minute score bets are settled on.
type Fixture struct {
	ID            int64          `json:"id" gorm:"primaryKey;autoIncrement:false"`
	CompetitionID int64          `json:"competition_id" gorm:"index:idx_fb_fix_comp"`
	Season        int            `json:"season" gorm:"index:idx_fb_fix_comp"`
	Round         string         `json:"round" gorm:"type:varchar(80)"`
	Kickoff       time.Time      `json:"kickoff" gorm:"index"`
	Status        string         `json:"status" gorm:"type:varchar(8);index"`
	StatusLong    string         `json:"status_long" gorm:"type:varchar(60)"`
	Elapsed       *int           `json:"elapsed"`
	ElapsedExtra  *int           `json:"elapsed_extra"`
	HomeID        int64          `json:"home_id" gorm:"index"`
	AwayID        int64          `json:"away_id" gorm:"index"`
	HomeGoals     *int           `json:"home_goals"`
	AwayGoals     *int           `json:"away_goals"`
	HTHome        *int           `json:"ht_home" gorm:"column:ht_home"`
	HTAway        *int           `json:"ht_away" gorm:"column:ht_away"`
	FTHome        *int           `json:"ft_home" gorm:"column:ft_home"`
	FTAway        *int           `json:"ft_away" gorm:"column:ft_away"`
	ETHome        *int           `json:"et_home" gorm:"column:et_home"`
	ETAway        *int           `json:"et_away" gorm:"column:et_away"`
	PenHome       *int           `json:"pen_home"`
	PenAway       *int           `json:"pen_away"`
	Venue         string         `json:"venue" gorm:"type:varchar(160)"`
	City          string         `json:"city" gorm:"type:varchar(120)"`
	Referee       string         `json:"referee" gorm:"type:varchar(120)"`
	OddsEventID   string         `json:"odds_event_id" gorm:"type:varchar(64);index"` // The Odds API event id
	PredictionRef *RefPrediction `json:"prediction_ref" gorm:"type:text;serializer:json"`
	FetchedAt     time.Time      `json:"fetched_at"`
	DetailsAt     *time.Time     `json:"details_at"` // events/statistics/players fetched (fixtures?ids=)
	LineupsAt     *time.Time     `json:"lineups_at"` // line-ups published and stored
	InjuriesAt    *time.Time     `json:"injuries_at"`
	OddsAFAt      *time.Time     `json:"odds_af_at" gorm:"column:odds_af_at"`
	OddsOAAt      *time.Time     `json:"odds_oa_at" gorm:"column:odds_oa_at"`
	PredRefAt     *time.Time     `json:"pred_ref_at"`
	H2HAt         *time.Time     `json:"h2h_at" gorm:"column:h2h_at"`
	FinishedAt    *time.Time     `json:"finished_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

func (Fixture) TableName() string { return "fb_fixtures" }

// RefPrediction is API-Football's own prediction, stored as a reference only
// (it is not part of DASYIN's model).
type RefPrediction struct {
	Winner    string    `json:"winner,omitempty"`
	Advice    string    `json:"advice,omitempty"`
	Home      float64   `json:"home"`
	Draw      float64   `json:"draw"`
	Away      float64   `json:"away"`
	UnderOver string    `json:"under_over,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
}

// FixtureStat is one team's match statistics (expected goals when provided).
type FixtureStat struct {
	FixtureID      int64     `json:"fixture_id" gorm:"primaryKey;autoIncrement:false"`
	TeamID         int64     `json:"team_id" gorm:"primaryKey;autoIncrement:false"`
	ShotsOn        *int      `json:"shots_on"`
	ShotsOff       *int      `json:"shots_off"`
	ShotsTotal     *int      `json:"shots_total"`
	ShotsBlocked   *int      `json:"shots_blocked"`
	ShotsInside    *int      `json:"shots_inside"`
	ShotsOutside   *int      `json:"shots_outside"`
	Fouls          *int      `json:"fouls"`
	Corners        *int      `json:"corners"`
	Offsides       *int      `json:"offsides"`
	Possession     *float64  `json:"possession"`
	Yellow         *int      `json:"yellow"`
	Red            *int      `json:"red"`
	Saves          *int      `json:"saves"`
	Passes         *int      `json:"passes"`
	PassesAccurate *int      `json:"passes_accurate"`
	PassPct        *float64  `json:"pass_pct"`
	XG             *float64  `json:"xg" gorm:"column:xg"`
	FetchedAt      time.Time `json:"fetched_at"`
}

func (FixtureStat) TableName() string { return "fb_fixture_stats" }

// FixtureEvent is a goal, card, substitution or VAR decision.
type FixtureEvent struct {
	ID         int64     `json:"-" gorm:"primaryKey"`
	FixtureID  int64     `json:"fixture_id" gorm:"index"`
	Seq        int       `json:"seq"`
	Minute     int       `json:"minute"`
	Extra      *int      `json:"extra"`
	TeamID     int64     `json:"team_id"`
	PlayerID   int64     `json:"player_id"`
	PlayerName string    `json:"player" gorm:"type:varchar(120)"`
	AssistID   int64     `json:"assist_id"`
	AssistName string    `json:"assist" gorm:"type:varchar(120)"`
	Type       string    `json:"type" gorm:"type:varchar(20)"`
	Detail     string    `json:"detail" gorm:"type:varchar(60)"`
	Comments   string    `json:"comments" gorm:"type:varchar(160)"`
	FetchedAt  time.Time `json:"fetched_at"`
}

func (FixtureEvent) TableName() string { return "fb_fixture_events" }

// LineupPlayer is one player of a published line-up.
type LineupPlayer struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Number int    `json:"number"`
	Pos    string `json:"pos"`
	Grid   string `json:"grid,omitempty"`
}

// Lineup is a team's published line-up.
type Lineup struct {
	FixtureID   int64          `json:"fixture_id" gorm:"primaryKey;autoIncrement:false"`
	TeamID      int64          `json:"team_id" gorm:"primaryKey;autoIncrement:false"`
	Formation   string         `json:"formation" gorm:"type:varchar(20)"`
	Coach       string         `json:"coach" gorm:"type:varchar(120)"`
	StartXI     []LineupPlayer `json:"start_xi" gorm:"type:text;serializer:json"`
	Substitutes []LineupPlayer `json:"substitutes" gorm:"type:text;serializer:json"`
	FetchedAt   time.Time      `json:"fetched_at"`
}

func (Lineup) TableName() string { return "fb_lineups" }

// FixturePlayer is a player's in-game statistics and rating.
type FixturePlayer struct {
	FixtureID  int64     `json:"fixture_id" gorm:"primaryKey;autoIncrement:false"`
	PlayerID   int64     `json:"player_id" gorm:"primaryKey;autoIncrement:false;index"`
	TeamID     int64     `json:"team_id"`
	Name       string    `json:"name" gorm:"type:varchar(120)"`
	Minutes    int       `json:"minutes"`
	Rating     *float64  `json:"rating"`
	Position   string    `json:"position" gorm:"type:varchar(4)"`
	Substitute bool      `json:"substitute"`
	Captain    bool      `json:"captain"`
	Goals      int       `json:"goals"`
	Assists    int       `json:"assists"`
	Shots      int       `json:"shots"`
	ShotsOn    int       `json:"shots_on"`
	KeyPasses  int       `json:"key_passes"`
	Yellow     int       `json:"yellow"`
	Red        int       `json:"red"`
	FetchedAt  time.Time `json:"fetched_at"`
}

func (FixturePlayer) TableName() string { return "fb_fixture_players" }

// Player (id = API-Football player id).
type Player struct {
	ID          int64     `json:"id" gorm:"primaryKey;autoIncrement:false"`
	Name        string    `json:"name" gorm:"type:varchar(120)"`
	Firstname   string    `json:"firstname" gorm:"type:varchar(80)"`
	Lastname    string    `json:"lastname" gorm:"type:varchar(80)"`
	Age         int       `json:"age"`
	Nationality string    `json:"nationality" gorm:"type:varchar(80)"`
	Photo       string    `json:"photo" gorm:"type:text"`
	Injured     bool      `json:"injured"`
	FetchedAt   time.Time `json:"fetched_at"`
}

func (Player) TableName() string { return "fb_players" }

// PlayerSeason is a player's season statistics for one team in one competition.
type PlayerSeason struct {
	PlayerID      int64     `json:"player_id" gorm:"primaryKey;autoIncrement:false"`
	TeamID        int64     `json:"team_id" gorm:"primaryKey;autoIncrement:false;index"`
	CompetitionID int64     `json:"competition_id" gorm:"primaryKey;autoIncrement:false"`
	Season        int       `json:"season" gorm:"primaryKey;autoIncrement:false"`
	Position      string    `json:"position" gorm:"type:varchar(20)"`
	Appearances   int       `json:"appearances"`
	Lineups       int       `json:"lineups"`
	Minutes       int       `json:"minutes"`
	Rating        *float64  `json:"rating"`
	Goals         int       `json:"goals"`
	Assists       int       `json:"assists"`
	Shots         int       `json:"shots"`
	ShotsOn       int       `json:"shots_on"`
	KeyPasses     int       `json:"key_passes"`
	Yellow        int       `json:"yellow"`
	Red           int       `json:"red"`
	PenScored     int       `json:"pen_scored"`
	FetchedAt     time.Time `json:"fetched_at"`
}

func (PlayerSeason) TableName() string { return "fb_player_seasons" }

// Injury is a player missing (or doubtful for) a fixture.
type Injury struct {
	FixtureID     int64     `json:"fixture_id" gorm:"primaryKey;autoIncrement:false"`
	PlayerID      int64     `json:"player_id" gorm:"primaryKey;autoIncrement:false"`
	TeamID        int64     `json:"team_id" gorm:"index"`
	CompetitionID int64     `json:"competition_id"`
	Season        int       `json:"season"`
	PlayerName    string    `json:"player_name" gorm:"type:varchar(120)"`
	Type          string    `json:"type" gorm:"type:varchar(40)"`    // "Missing Fixture" | "Questionable"
	Reason        string    `json:"reason" gorm:"type:varchar(120)"` // "Knee Injury", "Suspended", …
	Status        string    `json:"status" gorm:"type:varchar(12)"`  // injured | suspended | doubtful
	FetchedAt     time.Time `json:"fetched_at"`
}

func (Injury) TableName() string { return "fb_injuries" }

// Standing is one row of a table snapshot. Snapshots are kept per date so
// the position of a team can be charted over the season.
type Standing struct {
	CompetitionID int64     `json:"competition_id" gorm:"primaryKey;autoIncrement:false"`
	Season        int       `json:"season" gorm:"primaryKey;autoIncrement:false"`
	AsOf          time.Time `json:"as_of" gorm:"primaryKey;type:date"`
	GroupName     string    `json:"group" gorm:"primaryKey;type:varchar(80)"`
	TeamID        int64     `json:"team_id" gorm:"primaryKey;autoIncrement:false"`
	Rank          int       `json:"rank"`
	Points        int       `json:"points"`
	Played        int       `json:"played"`
	Won           int       `json:"won"`
	Drawn         int       `json:"drawn"`
	Lost          int       `json:"lost"`
	GF            int       `json:"gf" gorm:"column:gf"`
	GA            int       `json:"ga" gorm:"column:ga"`
	GD            int       `json:"gd" gorm:"column:gd"`
	Form          string    `json:"form" gorm:"type:varchar(10)"`
	Description   string    `json:"description" gorm:"type:varchar(160)"`
	Home          Record    `json:"home" gorm:"type:text;serializer:json"`
	Away          Record    `json:"away" gorm:"type:text;serializer:json"`
	Source        string    `json:"source" gorm:"type:varchar(20)"` // apifootball | computed
	FetchedAt     time.Time `json:"fetched_at"`
}

func (Standing) TableName() string { return "fb_standings" }

// Record is a home or away split of a standings row.
type Record struct {
	Played int `json:"played"`
	Won    int `json:"won"`
	Drawn  int `json:"drawn"`
	Lost   int `json:"lost"`
	GF     int `json:"gf"`
	GA     int `json:"ga"`
}

// H2H caches the head-to-head fetch for a pair of teams (low id first).
type H2H struct {
	TeamA      int64     `json:"team_a" gorm:"primaryKey;autoIncrement:false"`
	TeamB      int64     `json:"team_b" gorm:"primaryKey;autoIncrement:false"`
	FixtureIDs []int64   `json:"fixture_ids" gorm:"type:text;serializer:json"`
	FetchedAt  time.Time `json:"fetched_at"`
}

func (H2H) TableName() string { return "fb_h2h" }

// Odd is a price seen at a bookmaker. A new row is written only when the
// price changes; CheckedAt says when it was last seen unchanged, so the rows
// are the line movement and the last row before kickoff is the closing price.
type Odd struct {
	ID                int64      `json:"-" gorm:"primaryKey"`
	FixtureID         int64      `json:"fixture_id" gorm:"index:idx_fb_odds_key,priority:1"`
	Source            string     `json:"source" gorm:"type:varchar(20);index:idx_fb_odds_key,priority:2"`    // apifootball | oddsapi
	Bookmaker         string     `json:"bookmaker" gorm:"type:varchar(60);index:idx_fb_odds_key,priority:3"` // slug, e.g. pinnacle
	BookmakerTitle    string     `json:"bookmaker_title" gorm:"type:varchar(80)"`
	Market            string     `json:"market" gorm:"type:varchar(8);index:idx_fb_odds_key,priority:4"` // 1x2 | ou | btts | dc | dnb | ah
	Selection         string     `json:"selection" gorm:"type:varchar(8);index:idx_fb_odds_key,priority:5"`
	Line              *float64   `json:"line"`
	Price             float64    `json:"price"`
	Live              bool       `json:"live"`
	ProviderUpdatedAt *time.Time `json:"provider_updated_at"`
	FetchedAt         time.Time  `json:"fetched_at" gorm:"index"`
	CheckedAt         time.Time  `json:"checked_at"`
}

func (Odd) TableName() string { return "fb_odds" }

// Rating is a fitted model per competition per day (Dixon–Coles parameters
// or Elo ratings).
type Rating struct {
	CompetitionID int64              `json:"competition_id" gorm:"primaryKey;autoIncrement:false"`
	AsOf          time.Time          `json:"as_of" gorm:"primaryKey;type:date"`
	Model         string             `json:"model" gorm:"primaryKey;type:varchar(10)"` // dc | elo
	Version       string             `json:"version" gorm:"type:varchar(160)"`
	Attack        map[string]float64 `json:"attack,omitempty" gorm:"type:text;serializer:json"`
	Defence       map[string]float64 `json:"defence,omitempty" gorm:"type:text;serializer:json"`
	Ratings       map[string]float64 `json:"ratings,omitempty" gorm:"type:text;serializer:json"`
	HomeAdv       float64            `json:"home_adv"`
	Rho           float64            `json:"rho"`
	Matches       int                `json:"matches"`
	LogLik        float64            `json:"log_lik"`
	Converged     bool               `json:"converged"`
	FittedAt      time.Time          `json:"fitted_at"`
}

func (Rating) TableName() string { return "fb_ratings" }

// Prediction is the model's estimate for a fixture. Kind "current" is
// recomputed as data arrives; "frozen" is the copy made before kickoff that
// is graded after full time (it never changes afterwards except the grade).
type Prediction struct {
	FixtureID     int64      `json:"fixture_id" gorm:"primaryKey;autoIncrement:false"`
	Kind          string     `json:"kind" gorm:"primaryKey;type:varchar(8)"`
	CompetitionID int64      `json:"competition_id" gorm:"index"`
	Kickoff       time.Time  `json:"kickoff" gorm:"index"`
	ModelVersion  string     `json:"model_version" gorm:"type:varchar(160)"`
	PHome         float64    `json:"p_home"`
	PDraw         float64    `json:"p_draw"`
	PAway         float64    `json:"p_away"`
	Detail        PredDetail `json:"detail" gorm:"type:text;serializer:json"`
	MarketHome    *float64   `json:"market_home"` // de-margined market at freeze / close
	MarketDraw    *float64   `json:"market_draw"`
	MarketAway    *float64   `json:"market_away"`
	MarketSource  string     `json:"market_source" gorm:"type:varchar(60)"`
	ComputedAt    time.Time  `json:"computed_at"`
	FrozenAt      *time.Time `json:"frozen_at"`
	Outcome       *int       `json:"outcome"` // 0 home 1 draw 2 away
	Brier         *float64   `json:"brier"`
	RPS           *float64   `json:"rps" gorm:"column:rps"`
	LogLoss       *float64   `json:"log_loss"`
	MarketBrier   *float64   `json:"market_brier"`
	MarketRPS     *float64   `json:"market_rps" gorm:"column:market_rps"`
	MarketLogLoss *float64   `json:"market_log_loss"`
	GradedAt      *time.Time `json:"graded_at"`
}

func (Prediction) TableName() string { return "fb_predictions" }

// FetchLog records every provider request with its cost.
type FetchLog struct {
	ID         int64     `json:"id" gorm:"primaryKey"`
	Provider   string    `json:"provider" gorm:"type:varchar(20);index:idx_fb_fetch_prov_at,priority:1"`
	Endpoint   string    `json:"endpoint" gorm:"type:varchar(60)"`
	Params     string    `json:"params" gorm:"type:text"`
	Task       string    `json:"task" gorm:"type:varchar(120)"`
	Status     int       `json:"status"`
	OK         bool      `json:"ok"`
	Cost       int       `json:"cost"`
	Results    int       `json:"results"`
	Remaining  *int      `json:"remaining"`
	DurationMS int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty" gorm:"type:text"`
	At         time.Time `json:"at" gorm:"index:idx_fb_fetch_prov_at,priority:2"`
}

func (FetchLog) TableName() string { return "fb_fetch_log" }

// Quota is a provider's usage in one period ("day:2026-11-20",
// "month:2026-11") with what the provider last reported.
type Quota struct {
	Provider       string     `json:"provider" gorm:"primaryKey;type:varchar(20)"`
	Period         string     `json:"period" gorm:"primaryKey;type:varchar(20)"`
	Used           int        `json:"used"`
	Requests       int        `json:"requests"`
	ReportedLimit  *int       `json:"reported_limit"`
	ReportedRemain *int       `json:"reported_remaining"`
	ReportedUsed   *int       `json:"reported_used"`
	BackoffUntil   *time.Time `json:"backoff_until"`
	Failures       int        `json:"failures"`
	LastError      string     `json:"last_error" gorm:"type:text"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (Quota) TableName() string { return "fb_quota" }

// TaskState remembers when a planned fetch last ran.
type TaskState struct {
	Key       string     `json:"key" gorm:"primaryKey;type:varchar(160)"`
	LastRunAt time.Time  `json:"last_run_at"`
	LastOKAt  *time.Time `json:"last_ok_at"`
	Status    string     `json:"status" gorm:"type:varchar(20)"`
	Fails     int        `json:"fails"`
	Note      string     `json:"note" gorm:"type:text"`
}

func (TaskState) TableName() string { return "fb_task_state" }

// TeamAlias maps a provider's team name (normalised) to a team. Aliases are
// added automatically when an Odds API event is matched and can be edited by
// super admins.
type TeamAlias struct {
	Alias     string    `json:"alias" gorm:"primaryKey;type:varchar(160)"`
	TeamID    int64     `json:"team_id" gorm:"index"`
	Source    string    `json:"source" gorm:"type:varchar(20)"`   // auto | manual
	Name      string    `json:"name" gorm:"type:varchar(160)"`    // as the provider wrote it
	Provider  string    `json:"provider" gorm:"type:varchar(20)"` // whose spelling: oddsapi, clubelo, oddspapi, fdcouk, fdorg, jackpot, news ("" = any)
	CreatedAt time.Time `json:"created_at"`
}

func (TeamAlias) TableName() string { return "fb_team_aliases" }

// UnmatchedEvent is an Odds API event that could not be matched to a fixture.
type UnmatchedEvent struct {
	EventID   string    `json:"event_id" gorm:"primaryKey;type:varchar(64)"`
	SportKey  string    `json:"sport_key" gorm:"type:varchar(80)"`
	Home      string    `json:"home" gorm:"type:varchar(160)"`
	Away      string    `json:"away" gorm:"type:varchar(160)"`
	Commence  time.Time `json:"commence"`
	BestGuess string    `json:"best_guess" gorm:"type:text"`
	Seen      int       `json:"seen"`
	LastSeen  time.Time `json:"last_seen"`
}

func (UnmatchedEvent) TableName() string { return "fb_unmatched_events" }

// ---------------------------------------------------------------- per person

// Bet statuses.
const (
	BetOpen      = "open"
	BetWon       = "won"
	BetLost      = "lost"
	BetVoid      = "void"
	BetHalfWon   = "half_won"
	BetHalfLost  = "half_lost"
	BetCashedOut = "cashed_out"
)

// Bet is a bet a person placed at their bookmaker and recorded here.
type Bet struct {
	ID             string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);not null;index:idx_fb_bets_owner,priority:1"`
	UserID         string     `json:"user_id" gorm:"type:varchar(255);not null;index:idx_fb_bets_owner,priority:2"`
	FixtureID      int64      `json:"fixture_id" gorm:"index"`
	CompetitionID  int64      `json:"competition_id"`
	Kickoff        time.Time  `json:"kickoff"`
	Bookmaker      string     `json:"bookmaker" gorm:"type:varchar(80)"`
	Market         string     `json:"market" gorm:"type:varchar(8)"`
	Selection      string     `json:"selection" gorm:"type:varchar(8)"`
	Line           *float64   `json:"line"`
	Odds           float64    `json:"odds"`
	Stake          float64    `json:"stake"`
	Currency       string     `json:"currency" gorm:"type:varchar(3)"`
	PlacedAt       time.Time  `json:"placed_at" gorm:"index"`
	ModelP         *float64   `json:"model_p"`
	MarketP        *float64   `json:"market_p"`
	Status         string     `json:"status" gorm:"type:varchar(12);index"`
	SettledAt      *time.Time `json:"settled_at"`
	SettledBy      string     `json:"settled_by" gorm:"type:varchar(20)"` // auto | manual
	Return         float64    `json:"return"`                             // amount paid back (0 when lost)
	PnL            float64    `json:"pnl" gorm:"column:pnl"`
	CashOut        *float64   `json:"cash_out"`
	ClosingOdds    *float64   `json:"closing_odds"`
	ClosingSource  string     `json:"closing_source" gorm:"type:varchar(60)"`
	ClosingFairP   *float64   `json:"closing_fair_p"`
	CLV            *float64   `json:"clv" gorm:"column:clv"` // odds / closing odds − 1
	CLVFair        *float64   `json:"clv_fair"`              // odds × closing fair p − 1
	Notes          string     `json:"notes" gorm:"type:text"`
	Tags           []string   `json:"tags" gorm:"type:text;serializer:json"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (Bet) TableName() string { return "fb_bets" }

// BankrollEntry is the starting bankroll or a deposit / withdrawal.
type BankrollEntry struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;index:idx_fb_bankroll_owner,priority:1"`
	UserID         string    `json:"user_id" gorm:"type:varchar(255);not null;index:idx_fb_bankroll_owner,priority:2"`
	Kind           string    `json:"kind" gorm:"type:varchar(12)"` // start | deposit | withdrawal
	Amount         float64   `json:"amount"`
	Currency       string    `json:"currency" gorm:"type:varchar(3)"`
	At             time.Time `json:"at"`
	Note           string    `json:"note" gorm:"type:text"`
	CreatedAt      time.Time `json:"created_at"`
}

func (BankrollEntry) TableName() string { return "fb_bankroll" }

// Limits are a person's own stake and loss limits. Lowering a limit applies
// at once; raising one (or removing it) waits CoolOffHours (Pending*). A
// break (CoolOffUntil) blocks recording new bets until it ends.
type Limits struct {
	OrganizationID string     `json:"-" gorm:"type:varchar(255);primaryKey"`
	UserID         string     `json:"user_id" gorm:"type:varchar(255);primaryKey"`
	DailyStake     *float64   `json:"daily_stake"`
	WeeklyStake    *float64   `json:"weekly_stake"`
	MonthlyStake   *float64   `json:"monthly_stake"`
	DailyLoss      *float64   `json:"daily_loss"`
	WeeklyLoss     *float64   `json:"weekly_loss"`
	MonthlyLoss    *float64   `json:"monthly_loss"`
	Pending        *LimitSet  `json:"pending" gorm:"type:text;serializer:json"`
	PendingFrom    *time.Time `json:"pending_from"`
	CoolOffUntil   *time.Time `json:"cool_off_until"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (Limits) TableName() string { return "fb_limits" }

// LimitSet is a full set of limits (nil = no limit).
type LimitSet struct {
	DailyStake   *float64 `json:"daily_stake"`
	WeeklyStake  *float64 `json:"weekly_stake"`
	MonthlyStake *float64 `json:"monthly_stake"`
	DailyLoss    *float64 `json:"daily_loss"`
	WeeklyLoss   *float64 `json:"weekly_loss"`
	MonthlyLoss  *float64 `json:"monthly_loss"`
}

// WatchItem is a fixture, team or competition a person follows.
type WatchItem struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;index:idx_fb_watch_owner,priority:1"`
	UserID         string    `json:"user_id" gorm:"type:varchar(255);not null;index:idx_fb_watch_owner,priority:2"`
	Kind           string    `json:"kind" gorm:"type:varchar(12)"` // fixture | team | competition
	RefID          int64     `json:"ref_id"`
	Note           string    `json:"note" gorm:"type:text"`
	CreatedAt      time.Time `json:"created_at"`
}

func (WatchItem) TableName() string { return "fb_watchlist" }

// ManualOdd is a price a person saw on a bookmaker's app (e.g. a Kenyan
// bookmaker the providers don't cover). It is shown only to their
// organisation, labelled "manual: <bookmaker>".
type ManualOdd struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;index"`
	UserID         string    `json:"user_id" gorm:"type:varchar(255)"`
	FixtureID      int64     `json:"fixture_id" gorm:"index"`
	Bookmaker      string    `json:"bookmaker" gorm:"type:varchar(80)"`
	Market         string    `json:"market" gorm:"type:varchar(8)"`
	Selection      string    `json:"selection" gorm:"type:varchar(8)"`
	Line           *float64  `json:"line"`
	Price          float64   `json:"price"`
	SeenAt         time.Time `json:"seen_at"`
	CreatedAt      time.Time `json:"created_at"`
}

func (ManualOdd) TableName() string { return "fb_manual_odds" }

// AllModels lists every table.
func AllModels() []any {
	out := []any{&Settings{}, &Competition{}, &Team{}, &Fixture{}, &FixtureStat{}, &FixtureEvent{}, &Lineup{}, &FixturePlayer{},
		&Player{}, &PlayerSeason{}, &Injury{}, &Standing{}, &H2H{}, &Odd{}, &Rating{}, &Prediction{}, &FetchLog{}, &Quota{},
		&TaskState{}, &TeamAlias{}, &UnmatchedEvent{}, &Bet{}, &BankrollEntry{}, &Limits{}, &WatchItem{}, &ManualOdd{}}
	// v2.17 (free sources, news, jackpots): free_models.go; v2.18: win_models.go.
	out = append(out, FreeModels()...)
	out = append(out, WinModels()...)
	return append(out, LockModels()...) // v2.19.1: lock_models.go
}

// Migrate creates the tables and seeds the settings row and the default
// competitions (never overwriting edits).
func Migrate(db *gorm.DB) error {
	for _, m := range AllModels() {
		if err := db.AutoMigrate(m); err != nil {
			return err
		}
	}
	if err := seed(db); err != nil {
		return err
	}
	if err := seedFree(db); err != nil {
		return err
	}
	if err := seedWin(db); err != nil {
		return err
	}
	return seedLock(db) // v2.19.1: keyless free sources on by default, once (lock_tick.go)
}
