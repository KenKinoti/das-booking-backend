package football

// Football v2 (v2.17): near-free sources, news → team news, the
// cross-source stats check and the jackpot analyser.
//
// Sources (each optional, on/off and budgeted in System settings → Football
// data; every call is logged in fb_fetch_log with its cost):
//   - OddsPapi v4 (https://oddspapi.io/en/docs, free 250 requests/month,
//     Betika KE + Pinnacle): oddspapi.go
//   - ClubElo (http://clubelo.com/API, free CSV): clubelo.go
//   - football-data.co.uk (https://www.football-data.co.uk/notes.txt, free CSV
//     results + closing odds): fdcouk.go
//   - football-data.org v4 (https://docs.football-data.org/general/v4/index.html,
//     free key, 10 calls/minute): fdorg.go
//   - news: Google News RSS search, GDELT DOC 2.0
//     (https://blog.gdeltproject.org/gdelt-doc-2-0-api-debuts/), RSS/Atom
//     feeds: news.go; keyword rules → team-news facts: newsrules.go
// Files: free_models.go (tables, settings), freequota.go (budgets + calls),
// freeplan.go (what to fetch when), sourcecheck.go, freeanalysis.go (match
// page additions), jackpot_math.go / jackpot_parse.go / jackpot.go
// (analyser), freehandler.go (HTTP API).

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Free providers (fb_fetch_log.provider / fb_quota.provider).
const (
	ProviderOddsPapi = "oddspapi"
	ProviderClubElo  = "clubelo"
	ProviderFDCOUK   = "fdcouk"
	ProviderFDOrg    = "fdorg"
	ProviderGNews    = "google_news"
	ProviderGDELT    = "gdelt"
	ProviderFeeds    = "feeds"
)

// FreeProviders in settings order.
var FreeProviders = []string{ProviderOddsPapi, ProviderClubElo, ProviderFDCOUK, ProviderFDOrg, ProviderGNews, ProviderGDELT, ProviderFeeds}

// FreeSettings is the platform's settings row for the free sources (id 1).
type FreeSettings struct {
	ID                 int                `json:"-" gorm:"primaryKey"`
	OddsPapiKey        string             `json:"-" gorm:"column:oddspapi_key;type:text"`
	FDOrgKey           string             `json:"-" gorm:"column:fdorg_key;type:text"`
	Enabled            map[string]bool    `json:"enabled" gorm:"type:text;serializer:json"`
	Budgets            map[string]int     `json:"budgets" gorm:"type:text;serializer:json"`
	OddsPapiBookmakers string             `json:"oddspapi_bookmakers" gorm:"column:oddspapi_bookmakers;type:varchar(200)"`
	FDCOUKNew          string             `json:"fdcouk_new_countries" gorm:"column:fdcouk_new;type:varchar(200)"`
	FDCOUKSeasons      int                `json:"fdcouk_seasons" gorm:"column:fdcouk_seasons"`
	NewsExpiryDays     int                `json:"news_expiry_days"`
	NewsConfirmDomains int                `json:"news_confirm_domains"`
	NewsGoogle         map[string]string  `json:"news_google" gorm:"type:text;serializer:json"`
	JackpotWeights     map[string]float64 `json:"jackpot_weights" gorm:"type:text;serializer:json"`
	JackpotSharing     SharingOpts        `json:"jackpot_sharing" gorm:"type:text;serializer:json"`
	RulesChanged       bool               `json:"rules_changed"`
	LastSeedAt         *time.Time         `json:"last_seed_at"`
	UpdatedBy          string             `json:"updated_by" gorm:"type:varchar(255)"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

func (FreeSettings) TableName() string { return "fb_free_settings" }

// SharingOpts is the "share with other winners" assumption of the jackpot EV.
type SharingOpts struct {
	Mode        string   `json:"mode"`         // model | fixed
	TicketsSold float64  `json:"tickets_sold"` // model: tickets in the pool
	Popularity  float64  `json:"popularity"`   // model: public pick share ∝ p^popularity
	CoWinners   *float64 `json:"co_winners"`   // fixed: expected other winners
}

// Default budgets: requests per period (OddsPapi per month, the rest per day).
var defaultFreeBudgets = map[string]int{ProviderOddsPapi: 230, ProviderClubElo: 40, ProviderFDCOUK: 60, ProviderFDOrg: 300,
	ProviderGNews: 150, ProviderGDELT: 150, ProviderFeeds: 300}

// DefaultJackpotWeights blend the per-game probabilities.
var DefaultJackpotWeights = map[string]float64{"jackpot": 0.25, "market": 0.45, "clubelo": 0.15, "model": 0.15}

func defaultFreeSettings() FreeSettings {
	// Every free source starts switched off: a super admin turns each one on
	// in System settings → Football data (after reading its terms).
	en := map[string]bool{}
	for _, p := range FreeProviders {
		en[p] = false
	}
	b := map[string]int{}
	for k, v := range defaultFreeBudgets {
		b[k] = v
	}
	w := map[string]float64{}
	for k, v := range DefaultJackpotWeights {
		w[k] = v
	}
	return FreeSettings{ID: 1, Enabled: en, Budgets: b, OddsPapiBookmakers: "betika,pinnacle", FDCOUKNew: "POL,AUT,DNK,ROU,IRL,SWZ,NOR,SWE",
		FDCOUKSeasons: 3, NewsExpiryDays: 14, NewsConfirmDomains: 2, NewsGoogle: map[string]string{"hl": "en-KE", "gl": "KE", "ceid": "KE:en"},
		JackpotWeights: w, JackpotSharing: SharingOpts{Mode: "model", TicketsSold: 300000, Popularity: 1.5}}
}

func normalizeFree(st *FreeSettings) {
	d := defaultFreeSettings()
	if st.Enabled == nil {
		st.Enabled = d.Enabled
	}
	if st.Budgets == nil {
		st.Budgets = map[string]int{}
	}
	for k, v := range d.Budgets {
		if st.Budgets[k] <= 0 {
			st.Budgets[k] = v
		}
	}
	if strings.TrimSpace(st.OddsPapiBookmakers) == "" {
		st.OddsPapiBookmakers = d.OddsPapiBookmakers
	}
	if st.FDCOUKSeasons <= 0 || st.FDCOUKSeasons > 10 {
		st.FDCOUKSeasons = d.FDCOUKSeasons
	}
	if st.NewsExpiryDays <= 0 || st.NewsExpiryDays > 90 {
		st.NewsExpiryDays = d.NewsExpiryDays
	}
	if st.NewsConfirmDomains < 2 || st.NewsConfirmDomains > 5 {
		st.NewsConfirmDomains = d.NewsConfirmDomains
	}
	if st.NewsGoogle == nil || st.NewsGoogle["hl"] == "" {
		st.NewsGoogle = d.NewsGoogle
	}
	if len(st.JackpotWeights) == 0 {
		st.JackpotWeights = d.JackpotWeights
	}
	if st.JackpotSharing.Mode == "" {
		st.JackpotSharing = d.JackpotSharing
	}
	if st.JackpotSharing.TicketsSold <= 0 {
		st.JackpotSharing.TicketsSold = d.JackpotSharing.TicketsSold
	}
	if st.JackpotSharing.Popularity <= 0 {
		st.JackpotSharing.Popularity = d.JackpotSharing.Popularity
	}
}

// FreeSettingsGet returns the free-source settings.
func (s *Service) FreeSettingsGet() FreeSettings {
	var st FreeSettings
	err := s.DB.Where("id = 1").First(&st).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		d := defaultFreeSettings()
		s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&d)
		s.DB.Where("id = 1").First(&st)
	}
	normalizeFree(&st)
	return st
}

// FreeKeys returns the decrypted OddsPapi and football-data.org keys.
func (s *Service) FreeKeys() (oddspapi, fdorg string) {
	st := s.FreeSettingsGet()
	a, _ := mail.DecryptSecret(st.OddsPapiKey)
	b, _ := mail.DecryptSecret(st.FDOrgKey)
	return strings.TrimSpace(a), strings.TrimSpace(b)
}

// FreeOn reports whether a free provider is switched on (and has its key).
func (s *Service) FreeOn(p string) bool {
	st := s.FreeSettingsGet()
	if !st.Enabled[p] {
		return false
	}
	op, fo := s.FreeKeys()
	switch p {
	case ProviderOddsPapi:
		return op != ""
	case ProviderFDOrg:
		return fo != ""
	}
	return true
}

// ---------------------------------------------------------------- source data (global)

// ClubEloRating is a club's ClubElo rating on a day (GET /YYYY-MM-DD).
type ClubEloRating struct {
	Club      string    `json:"club" gorm:"primaryKey;type:varchar(80)"`
	Date      time.Time `json:"date" gorm:"primaryKey;type:date"`
	Country   string    `json:"country" gorm:"type:varchar(8)"`
	Level     int       `json:"level"`
	Elo       float64   `json:"elo"`
	Rank      *int      `json:"rank"`
	TeamID    int64     `json:"team_id" gorm:"index"`
	FetchedAt time.Time `json:"fetched_at"`
}

func (ClubEloRating) TableName() string { return "fb_clubelo_ratings" }

// ClubEloFixture is ClubElo's own probability for an upcoming match (GET /Fixtures).
type ClubEloFixture struct {
	Date      time.Time `json:"date" gorm:"primaryKey;type:date"`
	Home      string    `json:"home" gorm:"primaryKey;type:varchar(80)"`
	Away      string    `json:"away" gorm:"primaryKey;type:varchar(80)"`
	Country   string    `json:"country" gorm:"type:varchar(8)"`
	PHome     float64   `json:"p_home"`
	PDraw     float64   `json:"p_draw"`
	PAway     float64   `json:"p_away"`
	FixtureID int64     `json:"fixture_id" gorm:"index"`
	FetchedAt time.Time `json:"fetched_at"`
}

func (ClubEloFixture) TableName() string { return "fb_clubelo_fixtures" }

// HistMatch is a historical result with closing odds from football-data.co.uk.
type HistMatch struct {
	ID            int64     `json:"id" gorm:"primaryKey"`
	Source        string    `json:"source" gorm:"type:varchar(12);uniqueIndex:idx_fb_hist_key,priority:1"`
	Div           string    `json:"div" gorm:"type:varchar(40);uniqueIndex:idx_fb_hist_key,priority:2"` // E0, D2 … or "POL Ekstraklasa"
	Date          time.Time `json:"date" gorm:"uniqueIndex:idx_fb_hist_key,priority:3"`                 // kickoff UTC (UK time converted)
	Home          string    `json:"home" gorm:"type:varchar(80);uniqueIndex:idx_fb_hist_key,priority:4"`
	Away          string    `json:"away" gorm:"type:varchar(80);uniqueIndex:idx_fb_hist_key,priority:5"`
	Country       string    `json:"country" gorm:"type:varchar(40)"`
	League        string    `json:"league" gorm:"type:varchar(80)"`
	Season        int       `json:"season" gorm:"index"`
	HG            int       `json:"hg"`
	AG            int       `json:"ag"`
	HTHG          *int      `json:"hthg" gorm:"column:hthg"`
	HTAG          *int      `json:"htag" gorm:"column:htag"`
	PSCH          *float64  `json:"psch" gorm:"column:psch"` // Pinnacle closing
	PSCD          *float64  `json:"pscd" gorm:"column:pscd"`
	PSCA          *float64  `json:"psca" gorm:"column:psca"`
	AvgCH         *float64  `json:"avgch" gorm:"column:avgch"` // market average closing
	AvgCD         *float64  `json:"avgcd" gorm:"column:avgcd"`
	AvgCA         *float64  `json:"avgca" gorm:"column:avgca"`
	MaxCH         *float64  `json:"maxch" gorm:"column:maxch"`
	MaxCD         *float64  `json:"maxcd" gorm:"column:maxcd"`
	MaxCA         *float64  `json:"maxca" gorm:"column:maxca"`
	CompetitionID int64     `json:"competition_id" gorm:"index"`
	HomeID        int64     `json:"home_id" gorm:"index"`
	AwayID        int64     `json:"away_id" gorm:"index"`
	FixtureID     int64     `json:"fixture_id" gorm:"index"` // API-Football fixture, or a seeded one (negative id)
	FetchedAt     time.Time `json:"fetched_at"`
}

func (HistMatch) TableName() string { return "fb_hist_matches" }

// FDOrgMatch is a football-data.org match (cross-check).
type FDOrgMatch struct {
	ID          int64     `json:"id" gorm:"primaryKey;autoIncrement:false"`
	Competition string    `json:"competition" gorm:"type:varchar(10);index"`
	UTCDate     time.Time `json:"utc_date" gorm:"index"`
	Status      string    `json:"status" gorm:"type:varchar(16)"`
	Matchday    int       `json:"matchday"`
	Home        string    `json:"home" gorm:"type:varchar(120)"`
	Away        string    `json:"away" gorm:"type:varchar(120)"`
	HomeFDID    int64     `json:"home_fd_id"`
	AwayFDID    int64     `json:"away_fd_id"`
	HG          *int      `json:"hg"`
	AG          *int      `json:"ag"`
	HomeID      int64     `json:"home_id" gorm:"index"`
	AwayID      int64     `json:"away_id" gorm:"index"`
	FixtureID   int64     `json:"fixture_id" gorm:"index"`
	FetchedAt   time.Time `json:"fetched_at"`
}

func (FDOrgMatch) TableName() string { return "fb_fdorg_matches" }

// FDOrgStanding is a football-data.org table row (latest fetch).
type FDOrgStanding struct {
	Competition string    `json:"competition" gorm:"primaryKey;type:varchar(10)"`
	FDTeamID    int64     `json:"fd_team_id" gorm:"primaryKey;autoIncrement:false"`
	Team        string    `json:"team" gorm:"type:varchar(120)"`
	Position    int       `json:"position"`
	Played      int       `json:"played"`
	Won         int       `json:"won"`
	Draw        int       `json:"draw"`
	Lost        int       `json:"lost"`
	Points      int       `json:"points"`
	GF          int       `json:"gf" gorm:"column:gf"`
	GA          int       `json:"ga" gorm:"column:ga"`
	Form        string    `json:"form" gorm:"type:varchar(20)"`
	TeamID      int64     `json:"team_id" gorm:"index"`
	FetchedAt   time.Time `json:"fetched_at"`
}

func (FDOrgStanding) TableName() string { return "fb_fdorg_standings" }

// OPTournament maps an OddsPapi tournament to a competition.
type OPTournament struct {
	ID            int64     `json:"id" gorm:"primaryKey;autoIncrement:false"`
	Slug          string    `json:"slug" gorm:"type:varchar(120)"`
	Name          string    `json:"name" gorm:"type:varchar(160)"`
	Category      string    `json:"category" gorm:"type:varchar(120)"`
	CompetitionID int64     `json:"competition_id" gorm:"index"`
	Upcoming      int       `json:"upcoming"`
	FetchedAt     time.Time `json:"fetched_at"`
}

func (OPTournament) TableName() string { return "fb_oddspapi_tournaments" }

// OPFixture is an OddsPapi fixture and the API-Football fixture it matched.
type OPFixture struct {
	ID           string     `json:"id" gorm:"primaryKey;type:varchar(64)"`
	TournamentID int64      `json:"tournament_id" gorm:"index"`
	Start        time.Time  `json:"start" gorm:"index"`
	Home         string     `json:"home" gorm:"type:varchar(160)"`
	Away         string     `json:"away" gorm:"type:varchar(160)"`
	HomePID      int64      `json:"home_pid"`
	AwayPID      int64      `json:"away_pid"`
	FixtureID    int64      `json:"fixture_id" gorm:"index"`
	OddsAt       *time.Time `json:"odds_at"`
	FetchedAt    time.Time  `json:"fetched_at"`
}

func (OPFixture) TableName() string { return "fb_oddspapi_fixtures" }

// UnmatchedName is a provider's team name that no alias or name rule mapped.
type UnmatchedName struct {
	Provider   string    `json:"provider" gorm:"primaryKey;type:varchar(20)"`
	Name       string    `json:"name" gorm:"primaryKey;type:varchar(160)"`
	Country    string    `json:"country" gorm:"type:varchar(40)"`
	Seen       int       `json:"seen"`
	LastSeen   time.Time `json:"last_seen"`
	BestGuess  string    `json:"best_guess" gorm:"type:varchar(200)"`
	BestTeamID int64     `json:"best_team_id"`
}

func (UnmatchedName) TableName() string { return "fb_unmatched_names" }

// ---------------------------------------------------------------- news (global)

// NewsItem is a headline (title, snippet, link, date — never the article).
type NewsItem struct {
	ID          string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	URL         string    `json:"url" gorm:"type:text"`
	URLKey      string    `json:"-" gorm:"type:varchar(64);uniqueIndex"`
	TitleKey    string    `json:"-" gorm:"type:varchar(300);index"`
	Title       string    `json:"title" gorm:"type:text"`
	Snippet     string    `json:"snippet" gorm:"type:text"`
	Source      string    `json:"source" gorm:"type:varchar(160)"`
	Domain      string    `json:"domain" gorm:"type:varchar(160);index"`
	PublishedAt time.Time `json:"published_at" gorm:"index"`
	Language    string    `json:"language" gorm:"type:varchar(8)"`
	Provider    string    `json:"provider" gorm:"type:varchar(20)"`
	Keywords    []string  `json:"matched_keywords" gorm:"type:text;serializer:json"`
	Statuses    []string  `json:"statuses" gorm:"type:text;serializer:json"`
	TeamIDs     []int64   `json:"team_ids" gorm:"type:text;serializer:json"`
	Players     []string  `json:"players" gorm:"type:text;serializer:json"`
	FactIDs     []string  `json:"fact_ids" gorm:"type:text;serializer:json"`
	Postponed   bool      `json:"postponed"`
	FetchedAt   time.Time `json:"fetched_at"`
}

func (NewsItem) TableName() string { return "fb_news" }

// NewsTeam links a news item to a team (for fast per-team lists).
type NewsTeam struct {
	NewsID string `gorm:"primaryKey;type:varchar(36)"`
	TeamID int64  `gorm:"primaryKey;autoIncrement:false;index"`
}

func (NewsTeam) TableName() string { return "fb_news_teams" }

// Team-news statuses.
const (
	TNInjured   = "injured"
	TNSuspended = "suspended"
	TNDoubtful  = "doubtful"
	TNReturning = "returning"
	TNCoach     = "coach_change"
)

// TeamNewsStatuses in display order.
var TeamNewsStatuses = []string{TNInjured, TNSuspended, TNDoubtful, TNReturning, TNCoach}

// TeamNewsFact is a candidate fact extracted from news (global). Platform-wide
// reviews (super admins) live on the fact; organisations' reviews in
// fb_team_news_reviews.
type TeamNewsFact struct {
	ID         string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	Key        string    `json:"-" gorm:"type:varchar(200);uniqueIndex"` // team|player-or-coach|status
	TeamID     int64     `json:"team_id" gorm:"index"`
	PlayerID   int64     `json:"player_id"`
	PlayerName string    `json:"player" gorm:"type:varchar(120)"`
	Role       string    `json:"role" gorm:"type:varchar(10)"`
	Status     string    `json:"status" gorm:"type:varchar(16)"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen" gorm:"index"`
	ExpiresAt  time.Time `json:"expires_at" gorm:"index"`
	NewsIDs    []string  `json:"news_ids" gorm:"type:text;serializer:json"`
	Domains    []string  `json:"domains" gorm:"type:text;serializer:json"`
	Keywords   []string  `json:"keywords" gorm:"type:text;serializer:json"`
	Languages  []string  `json:"languages" gorm:"type:text;serializer:json"`
	InAF       bool      `json:"in_apifootball"`
	// Platform review by a super admin: confirm | dismiss | "".
	AdminAction string     `json:"admin_action" gorm:"type:varchar(10)"`
	AdminBy     string     `json:"admin_by" gorm:"type:varchar(160)"`
	AdminAt     *time.Time `json:"admin_at"`
	AdminNote   string     `json:"admin_note" gorm:"type:text"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (TeamNewsFact) TableName() string { return "fb_team_news" }

// TeamNewsReview is an organisation manager's confirm / dismiss of a fact.
type TeamNewsReview struct {
	OrganizationID string    `json:"-" gorm:"type:varchar(255);primaryKey"`
	FactID         string    `json:"fact_id" gorm:"type:varchar(36);primaryKey"`
	Action         string    `json:"action" gorm:"type:varchar(10)"`
	UserID         string    `json:"user_id" gorm:"type:varchar(255)"`
	UserName       string    `json:"by" gorm:"type:varchar(160)"`
	Note           string    `json:"note" gorm:"type:text"`
	At             time.Time `json:"at"`
}

func (TeamNewsReview) TableName() string { return "fb_team_news_reviews" }

// TeamNewsAudit records every review (kept on reset).
type TeamNewsAudit struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);index"`
	FactID         string    `json:"fact_id" gorm:"type:varchar(36);index"`
	Action         string    `json:"action" gorm:"type:varchar(10)"`
	Scope          string    `json:"scope" gorm:"type:varchar(14)"`
	UserID         string    `json:"by_user_id" gorm:"type:varchar(255)"`
	UserName       string    `json:"by" gorm:"type:varchar(160)"`
	Note           string    `json:"note" gorm:"type:text"`
	At             time.Time `json:"at"`
}

func (TeamNewsAudit) TableName() string { return "fb_team_news_audit" }

// NewsRule is one editable keyword set (language × status).
type NewsRule struct {
	ID       int64    `json:"-" gorm:"primaryKey"`
	Language string   `json:"language" gorm:"type:varchar(8);uniqueIndex:idx_fb_news_rule,priority:1"`
	Status   string   `json:"status" gorm:"type:varchar(16);uniqueIndex:idx_fb_news_rule,priority:2"`
	Keywords []string `json:"keywords" gorm:"type:text;serializer:json"`
	Negative []string `json:"negative,omitempty" gorm:"type:text;serializer:json"`
}

func (NewsRule) TableName() string { return "fb_news_rules" }

// NewsFeed is an RSS/Atom feed configured by a super admin.
type NewsFeed struct {
	ID            string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	URL           string     `json:"url" gorm:"type:text"`
	Label         string     `json:"label" gorm:"type:varchar(160)"`
	Language      string     `json:"language" gorm:"type:varchar(8)"`
	TeamID        int64      `json:"team_id"`
	CompetitionID int64      `json:"competition_id"`
	Enabled       bool       `json:"enabled"`
	LastFetchedAt *time.Time `json:"last_fetched_at"`
	LastError     string     `json:"last_error" gorm:"type:text"`
	Items         int        `json:"items"`
	CreatedAt     time.Time  `json:"created_at"`
}

func (NewsFeed) TableName() string { return "fb_news_feeds" }

// ---------------------------------------------------------------- jackpots (per organisation + person)

// Jackpot is a jackpot a person analyses (SportPesa / Betika style 1X2 pool).
type Jackpot struct {
	ID             string             `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string             `json:"-" gorm:"type:varchar(255);not null;index:idx_fb_jp_owner,priority:1"`
	UserID         string             `json:"user_id" gorm:"type:varchar(255);not null;index:idx_fb_jp_owner,priority:2"`
	Name           string             `json:"name" gorm:"type:varchar(160)"`
	Bookmaker      string             `json:"bookmaker" gorm:"type:varchar(80)"`
	Prize          float64            `json:"prize"`
	Currency       string             `json:"currency" gorm:"type:varchar(3)"`
	TicketPrice    float64            `json:"ticket_price"`
	Bonuses        []BonusTier        `json:"bonuses" gorm:"type:text;serializer:json"`
	ClosesAt       *time.Time         `json:"closes_at" gorm:"index"`
	Status         string             `json:"status" gorm:"type:varchar(10);index"`
	Weights        map[string]float64 `json:"weights" gorm:"type:text;serializer:json"`
	Sharing        SharingOpts        `json:"sharing" gorm:"type:text;serializer:json"`
	TopN           int                `json:"top_n"`
	Notes          string             `json:"notes" gorm:"type:text"`
	Grading        *Grading           `json:"-" gorm:"type:text;serializer:json"`
	// The last analysis' most likely ticket (the list shows it as on the page).
	LastML         []string   `json:"-" gorm:"column:last_ml;type:text;serializer:json"`
	LastMLP        float64    `json:"-" gorm:"column:last_ml_p"`
	LastAnalysedAt *time.Time `json:"last_analysed_at"`
	GradedAt       *time.Time `json:"graded_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (Jackpot) TableName() string { return "fb_jackpots" }

// BonusTier pays amount for exactly `correct` right picks.
type BonusTier struct {
	Correct int     `json:"correct"`
	Amount  float64 `json:"amount"`
}

// JackpotGame is one game of a jackpot.
type JackpotGame struct {
	JackpotID      string     `json:"-" gorm:"type:varchar(36);primaryKey"`
	N              int        `json:"n" gorm:"primaryKey;autoIncrement:false"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);not null;index"`
	Home           string     `json:"home" gorm:"type:varchar(160)"`
	Away           string     `json:"away" gorm:"type:varchar(160)"`
	Raw            string     `json:"raw" gorm:"type:text"`
	Odds1          float64    `json:"-" gorm:"column:odds1"`
	OddsX          float64    `json:"-" gorm:"column:oddsx"`
	Odds2          float64    `json:"-" gorm:"column:odds2"`
	Kickoff        *time.Time `json:"kickoff"`
	FixtureID      *int64     `json:"fixture_id" gorm:"index"`
	Match          GameMatch  `json:"match" gorm:"type:text;serializer:json"`
	Result         string     `json:"result" gorm:"type:varchar(2)"` // 1 | X | 2 | ""
	ResultSource   string     `json:"result_source" gorm:"type:varchar(12)"`
	Void           bool       `json:"void"`
	Score          string     `json:"score" gorm:"type:varchar(12)"`
}

func (JackpotGame) TableName() string { return "fb_jackpot_games" }

// JackpotTicket is a ticket the person saved (single picks or doubles/triples).
type JackpotTicket struct {
	ID             string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	JackpotID      string     `json:"-" gorm:"type:varchar(36);index"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);not null;index"`
	Name           string     `json:"name" gorm:"type:varchar(120)"`
	Source         string     `json:"source" gorm:"type:varchar(16)"`
	Picks          []string   `json:"picks" gorm:"type:text;serializer:json"`
	Correct        *int       `json:"correct"`
	GradedAt       *time.Time `json:"graded_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

func (JackpotTicket) TableName() string { return "fb_jackpot_tickets" }

// FreeModels lists the v2.17 tables.
func FreeModels() []any {
	return []any{&FreeSettings{}, &ClubEloRating{}, &ClubEloFixture{}, &HistMatch{}, &FDOrgMatch{}, &FDOrgStanding{}, &OPTournament{}, &OPFixture{},
		&UnmatchedName{}, &NewsItem{}, &NewsTeam{}, &TeamNewsFact{}, &TeamNewsReview{}, &TeamNewsAudit{}, &NewsRule{}, &NewsFeed{},
		&Jackpot{}, &JackpotGame{}, &JackpotTicket{}}
}

// seedFree creates the settings row and the default keyword rules (never
// overwriting edits).
func seedFree(db *gorm.DB) error {
	d := defaultFreeSettings()
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&d).Error; err != nil {
		return err
	}
	var n int64
	db.Model(&NewsRule{}).Count(&n)
	if n == 0 {
		for _, r := range DefaultNewsRules() {
			r := r
			if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&r).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
