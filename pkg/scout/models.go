// Package scout is the AI market scout (Insights → Market scout): it searches
// the public web with the Anthropic Messages API and its server-side web
// search tool, using the platform's Anthropic key (System settings → AI
// assistant, pkg/assistant), and keeps a ranked feed of external
// opportunities — tenders, grants, partnerships, leads, new markets and
// products / services to add — plus market signals by country and sector.
//
// It sits on top of pkg/insights: the own-data opportunities, industry watch
// and market indicators stay there; the scout reads the organisation's
// catalogue, customers and saved target markets to build its business
// profile, and only looks outward.
//
// Money is spent on every scan, so:
//
//   - the scout is off until a super admin (whose key pays) turns it on and
//     confirms the estimated monthly cost; it never runs without an API key;
//   - each run has a search limit (max_uses) and an output-token limit; the
//     worst case of a run is checked against what is left of the monthly
//     budget (US$, default 10) before it starts, and the search limit is
//     lowered to fit or the run is refused;
//   - when the month's spend reaches the budget, scans stop and the admins are
//     notified until the next month or a higher budget.
//
// Web content is untrusted: the system prompt tells the model to ignore any
// instructions found in pages, only the web search tool is enabled, the
// answer is parsed as data (never executed), every item must cite a URL that
// appeared in that response's search results, and links are shown as plain
// external links.
package scout

import (
	"time"

	"gorm.io/gorm"
)

// Opportunity types.
const (
	TypeTender      = "tender"
	TypeGrant       = "grant"
	TypePartnership = "partnership"
	TypeLead        = "lead"
	TypeNewMarket   = "new_market"
	TypeProduct     = "product"
)

// Types in display order with their labels.
var Types = []Choice{
	{TypeTender, "Tenders"},
	{TypeGrant, "Grants & funding"},
	{TypePartnership, "Partnerships"},
	{TypeLead, "Leads (businesses to approach)"},
	{TypeNewMarket, "New markets"},
	{TypeProduct, "Products / services to add"},
}

// Choice is a key with a label.
type Choice struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// TypeLabel is the singular label of a type.
func TypeLabel(t string) string {
	switch t {
	case TypeTender:
		return "Tender"
	case TypeGrant:
		return "Grant"
	case TypePartnership:
		return "Partnership"
	case TypeLead:
		return "Lead"
	case TypeNewMarket:
		return "New market"
	case TypeProduct:
		return "Product / service idea"
	}
	return "Opportunity"
}

// Statuses of an opportunity.
const (
	StatusNew       = "new"
	StatusSaved     = "saved"
	StatusPursued   = "pursued"
	StatusDismissed = "dismissed"
	StatusExpired   = "expired"
)

// Dismiss reasons (fed back to later scans as "not interested").
var DismissReasons = []Choice{
	{"not_relevant", "Not relevant to what we sell"},
	{"too_small", "Too small"},
	{"wrong_country", "Wrong country or region"},
	{"wrong_sector", "Wrong sector"},
	{"too_much_effort", "Too much effort for us"},
	{"already_known", "Already knew about it"},
	{"not_eligible", "We are not eligible"},
	{"other", "Other"},
}

// DismissLabel is the label of a dismiss reason.
func DismissLabel(k string) string {
	for _, c := range DismissReasons {
		if c.Key == k {
			return c.Label
		}
	}
	return "Other"
}

// Run kinds.
const (
	KindLight = "light" // daily: a few searches
	KindDeep  = "deep"  // weekly: more searches
)

// Run statuses.
const (
	RunRunning = "running"
	RunOK      = "ok"
	RunError   = "error"
	RunBudget  = "budget" // refused or stopped: monthly budget
	RunSkipped = "skipped"
)

// ---------------------------------------------------------------- settings

// Offering is one thing the organisation sells.
type Offering struct {
	Name     string  `json:"name"`
	Line     string  `json:"line,omitempty"` // service line label (pkg/insights classification)
	Price    float64 `json:"price,omitempty"`
	Currency string  `json:"currency,omitempty"`
	Source   string  `json:"source,omitempty"` // invoices | services | products | manual
	Sold     int     `json:"sold,omitempty"`   // invoice lines in the last 24 months
}

// Share is a country or sector with how many customers are there.
type Share struct {
	Key       string  `json:"key"`
	Name      string  `json:"name"`
	Customers int     `json:"customers"`
	SharePct  float64 `json:"share_pct"`
}

// Profile is what the scout knows about the business: built from its data,
// editable by the owner.
type Profile struct {
	OrgName      string     `json:"org_name"`
	BusinessType string     `json:"business_type"`
	HomeCountry  string     `json:"home_country"`
	Currency     string     `json:"currency"`
	Timezone     string     `json:"timezone"`
	Description  string     `json:"description"`
	Offerings    []Offering `json:"offerings"`
	Countries    []Share    `json:"customer_countries"`
	Sectors      []Share    `json:"customer_sectors"`
	// TargetMarkets are the countries saved in Insights → Trade & exports.
	TargetMarkets []string `json:"target_markets"`
}

// Source is a website to favour.
type Source struct {
	Domain  string `json:"domain"`
	Label   string `json:"label"`
	Country string `json:"country,omitempty"`
}

// Focus is the owner's direction for the scout.
type Focus struct {
	Countries        []string `json:"countries"` // ISO 3166-1 alpha-2
	Regions          []string `json:"regions"`   // free text, e.g. "East Africa"
	Sectors          []string `json:"sectors"`
	Keywords         []string `json:"keywords"`
	Exclusions       []string `json:"exclusions"`
	Types            []string `json:"types"`
	MinValue         float64  `json:"min_value"`
	MinValueCurrency string   `json:"min_value_currency"`
	Sources          []Source `json:"sources"`
	OnlySources      bool     `json:"only_sources"` // search only the favoured sources (allowed_domains)
	BlockedDomains   []string `json:"blocked_domains"`
	Notes            string   `json:"notes"`
}

// Settings is one organisation's scout configuration (one row).
type Settings struct {
	OrganizationID string `json:"-" gorm:"type:varchar(255);primaryKey"`

	Enabled bool `json:"enabled" gorm:"not null;default:false"`
	// The cost confirmation of the super admin who turned it on.
	CostConfirmedAt      *time.Time `json:"cost_confirmed_at"`
	CostConfirmedBy      string     `json:"cost_confirmed_by" gorm:"type:varchar(255)"`
	ConfirmedEstimateUSD float64    `json:"confirmed_estimate_usd"`
	EnabledBy            string     `json:"-" gorm:"type:varchar(255)"` // user id
	EnabledAt            *time.Time `json:"enabled_at"`

	Profile        Profile    `json:"profile" gorm:"type:text;serializer:json"`
	ProfileBuiltAt *time.Time `json:"profile_built_at"`
	Focus          Focus      `json:"focus" gorm:"type:text;serializer:json"`

	DailyEnabled   bool   `json:"daily_enabled" gorm:"not null;default:true"`
	DailyAt        string `json:"daily_at" gorm:"type:varchar(5);not null;default:'06:30'"`
	DailySearches  int    `json:"daily_searches" gorm:"not null;default:5"`
	WeeklyEnabled  bool   `json:"weekly_enabled" gorm:"not null;default:true"`
	WeeklyDay      int    `json:"weekly_day" gorm:"not null;default:1"` // 0 = Sunday … 1 = Monday
	WeeklySearches int    `json:"weekly_searches" gorm:"not null;default:20"`

	MonthlyBudgetUSD float64 `json:"monthly_budget_usd" gorm:"not null;default:10"`

	// LastSlot is the local date (YYYY-MM-DD) of the last scheduled run.
	LastSlot       string     `json:"last_slot" gorm:"type:varchar(10)"`
	LastRunAt      *time.Time `json:"last_run_at"`
	BudgetNotified string     `json:"-" gorm:"type:varchar(7)"` // YYYY-MM

	UpdatedBy string    `json:"-" gorm:"type:varchar(255)"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Settings) TableName() string { return "scout_settings" }

// ---------------------------------------------------------------- feed

// SourceRef is a page an item cites (it appeared in the search results).
type SourceRef struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Domain  string `json:"domain"`
	PageAge string `json:"page_age,omitempty"`
}

// Reason is one part of the fit score.
type Reason struct {
	Label  string `json:"label"`
	Points int    `json:"points"`
}

// Opportunity is one external opportunity in the feed.
type Opportunity struct {
	ID             string `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string `json:"-" gorm:"type:varchar(255);not null;index:idx_scout_opp_org,priority:1"`

	Type         string      `json:"type" gorm:"type:varchar(20);not null"`
	Title        string      `json:"title" gorm:"type:varchar(300);not null"`
	TitleKey     string      `json:"-" gorm:"type:varchar(300);index"`
	Buyer        string      `json:"buyer" gorm:"type:varchar(200)"`
	Summary      string      `json:"summary" gorm:"type:text"`
	WhyItFits    string      `json:"why_it_fits" gorm:"type:text"`
	EstValue     *float64    `json:"est_value"`
	Currency     string      `json:"currency" gorm:"type:varchar(3)"`
	ValueHome    *float64    `json:"value_home"` // in the organisation's currency (today's rate when it was found)
	Deadline     *time.Time  `json:"deadline" gorm:"type:date;index"`
	DeadlineText string      `json:"deadline_text" gorm:"type:varchar(80)"`
	Country      string      `json:"country" gorm:"type:varchar(2);index"`
	Location     string      `json:"location" gorm:"type:varchar(160)"`
	Effort       string      `json:"effort" gorm:"type:varchar(10)"`
	Confidence   string      `json:"confidence" gorm:"type:varchar(10)"`
	NextStep     string      `json:"next_step" gorm:"type:text"`
	Sources      []SourceRef `json:"sources" gorm:"type:text;serializer:json"`
	Fit          int         `json:"fit" gorm:"index"`
	FitReasons   []Reason    `json:"fit_reasons" gorm:"type:text;serializer:json"`

	Status        string     `json:"status" gorm:"type:varchar(12);not null;default:'new';index:idx_scout_opp_org,priority:2"`
	DismissReason string     `json:"dismiss_reason" gorm:"type:varchar(20)"`
	DismissNote   string     `json:"dismiss_note" gorm:"type:varchar(300)"`
	DismissedAt   *time.Time `json:"dismissed_at"`
	DismissedBy   string     `json:"-" gorm:"type:varchar(255)"`
	Notes         string     `json:"notes" gorm:"type:text"`

	RemindOn     *time.Time `json:"remind_on" gorm:"type:date;index"`
	RemindUserID string     `json:"-" gorm:"type:varchar(255)"`
	RemindedAt   *time.Time `json:"reminded_at"`

	CRMKind   string     `json:"crm_kind" gorm:"type:varchar(8)"` // deal | lead
	CRMID     string     `json:"crm_id" gorm:"type:varchar(64)"`
	PursuedAt *time.Time `json:"pursued_at"`
	PursuedBy string     `json:"-" gorm:"type:varchar(255)"`

	RunID       string    `json:"run_id" gorm:"type:varchar(36)"`
	LastRunID   string    `json:"-" gorm:"type:varchar(36)"`
	FirstSeenAt time.Time `json:"first_seen_at" gorm:"index"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	SeenCount   int       `json:"seen_count" gorm:"not null;default:1"`
	CreatedAt   time.Time `json:"-"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Opportunity) TableName() string { return "scout_opportunities" }

// Signal is a market signal: something happening in a market that matters to
// the business.
type Signal struct {
	ID             string      `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string      `json:"-" gorm:"type:varchar(255);not null;index"`
	Market         string      `json:"market" gorm:"type:varchar(160)"`
	Country        string      `json:"country" gorm:"type:varchar(2);index"`
	Sector         string      `json:"sector" gorm:"type:varchar(120)"`
	Signal         string      `json:"signal" gorm:"type:text"`
	SignalKey      string      `json:"-" gorm:"type:varchar(300)"`
	SoWhat         string      `json:"so_what" gorm:"type:text"`
	Sources        []SourceRef `json:"sources" gorm:"type:text;serializer:json"`
	RunID          string      `json:"run_id" gorm:"type:varchar(36)"`
	Hidden         bool        `json:"hidden" gorm:"not null;default:false"`
	FirstSeenAt    time.Time   `json:"first_seen_at" gorm:"index"`
	LastSeenAt     time.Time   `json:"last_seen_at"`
	SeenCount      int         `json:"seen_count" gorm:"not null;default:1"`
	CreatedAt      time.Time   `json:"-"`
	UpdatedAt      time.Time   `json:"-"`
}

// TableName is fixed.
func (Signal) TableName() string { return "scout_signals" }

// Run is one scan with what it cost. Runs are the spend ledger of the monthly
// budget (an organisation reset keeps them).
type Run struct {
	ID             string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);not null;index:idx_scout_run_org,priority:1"`
	Kind           string     `json:"kind" gorm:"type:varchar(8);not null"`
	Trigger        string     `json:"trigger" gorm:"type:varchar(10)"` // schedule | manual | ai
	By             string     `json:"-" gorm:"type:varchar(255)"`
	ByName         string     `json:"by_name" gorm:"type:varchar(200)"`
	Status         string     `json:"status" gorm:"type:varchar(10);not null;index"`
	Message        string     `json:"message" gorm:"type:text"`
	StartedAt      time.Time  `json:"started_at" gorm:"index:idx_scout_run_org,priority:2"`
	FinishedAt     *time.Time `json:"finished_at"`
	Model          string     `json:"model" gorm:"type:varchar(120)"`
	MaxSearches    int        `json:"max_searches"`
	MaxTokens      int        `json:"max_tokens"`
	Searches       int        `json:"searches"`
	Queries        []string   `json:"queries" gorm:"type:text;serializer:json"`
	SearchErrors   []string   `json:"search_errors" gorm:"type:text;serializer:json"`
	Requests       int        `json:"requests"`
	StopReason     string     `json:"stop_reason" gorm:"type:varchar(30)"`
	InputTokens    int        `json:"input_tokens"`
	OutputTokens   int        `json:"output_tokens"`
	CacheWrite     int        `json:"cache_write_tokens"`
	CacheRead      int        `json:"cache_read_tokens"`
	SearchCostUSD  float64    `json:"search_cost_usd"`
	TokenCostUSD   float64    `json:"token_cost_usd"`
	CostUSD        float64    `json:"cost_usd"`
	EstimateUSD    float64    `json:"estimate_usd"` // worst case checked before it ran
	PriceNote      string     `json:"price_note" gorm:"type:varchar(200)"`
	ResultURLs     int        `json:"result_urls"`
	Found          int        `json:"found"`      // opportunities in the answer
	Kept           int        `json:"kept"`       // new cards
	Updated        int        `json:"updated"`    // seen before: card refreshed
	Unverified     int        `json:"unverified"` // dropped: no source URL from the search results
	Expired        int        `json:"expired"`    // dropped: deadline already passed
	Duplicates     int        `json:"duplicates"` // dropped: same item twice in the answer
	Invalid        int        `json:"invalid"`    // dropped: missing title / unusable
	Filtered       int        `json:"filtered"`   // dropped: below the minimum value or an exclusion
	SignalsFound   int        `json:"signals_found"`
	SignalsKept    int        `json:"signals_kept"`
	Prompt         string     `json:"prompt,omitempty" gorm:"type:text"`
	AnswerText     string     `json:"-" gorm:"type:text"`
	CreatedAt      time.Time  `json:"-"`
	UpdatedAt      time.Time  `json:"-"`
}

// TableName is fixed.
func (Run) TableName() string { return "scout_runs" }

// Migrate creates the tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&Settings{}, &Opportunity{}, &Signal{}, &Run{})
}
