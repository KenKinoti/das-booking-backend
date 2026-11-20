package football

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// ---------------------------------------------------------------- service

// Service is the football module.
type Service struct {
	DB   *gorm.DB
	HTTP *http.Client
	// AFBase / OABase are the provider base URLs (FOOTBALL_APIFOOTBALL_BASE,
	// FOOTBALL_ODDSAPI_BASE; tests point them at the fakes).
	AFBase, OABase string
	// TestClock enables the test clock endpoints (FOOTBALL_TEST_CLOCK=1).
	TestClock bool
	// Can reports whether a person may perform act on the football module
	// (business type + role, pkg/industry). nil = legacy roles only.
	Can func(org, user, role, act string) bool
	// OrgCurrency / OrgZone give an organisation's currency and time zone.
	OrgCurrency func(org string) string
	OrgZone     func(org string) *time.Location
	// Sleep is used between pages and on back-off (tests replace it).
	Sleep func(time.Duration)

	runMu       sync.Mutex
	offset      atomic.Int64
	minuteBlock atomic.Int64 // wall-clock unix ns until which API-Football's per-minute limit is used up
	settMu      sync.Mutex
	settC       *Settings
	settAt      time.Time
}

// DefaultAFBase / DefaultOABase are the production endpoints.
const (
	DefaultAFBase = "https://v3.football.api-sports.io"
	DefaultOABase = "https://api.the-odds-api.com"
)

// New builds the service from the environment.
func New(db *gorm.DB) *Service {
	s := &Service{DB: db, HTTP: &http.Client{Timeout: 25 * time.Second},
		AFBase:    strings.TrimRight(envOr("FOOTBALL_APIFOOTBALL_BASE", DefaultAFBase), "/"),
		OABase:    strings.TrimRight(envOr("FOOTBALL_ODDSAPI_BASE", DefaultOABase), "/"),
		TestClock: strings.TrimSpace(os.Getenv("FOOTBALL_TEST_CLOCK")) == "1",
		Sleep:     time.Sleep}
	return s
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// Now is the module clock: the wall clock plus the test offset (only ever
// non-zero with FOOTBALL_TEST_CLOCK=1).
func (s *Service) Now() time.Time { return time.Now().UTC().Add(time.Duration(s.offset.Load())) }

// SetClock moves the test clock to t (zero time resets it).
func (s *Service) SetClock(t time.Time) error {
	if !s.TestClock {
		return errors.New("the test clock is off (FOOTBALL_TEST_CLOCK)")
	}
	if t.IsZero() {
		s.offset.Store(0)
		return nil
	}
	s.offset.Store(int64(time.Until(t)))
	return nil
}

// ---------------------------------------------------------------- errors

// InputError is a validation problem shown to the person as is.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func inputErr(f string, a ...any) error { return &InputError{Msg: fmt.Sprintf(f, a...)} }

// LimitError is a bet refused by the person's own limits or break.
type LimitError struct {
	Msg   string
	Until *time.Time
	Limit string
}

func (e *LimitError) Error() string { return e.Msg }

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrBusy      = errors.New("a refresh is already running")
)

// ---------------------------------------------------------------- plans

// PlanQuota is API-Football's daily request quota per plan
// (https://www.api-football.com/pricing; free = 100/day).
var PlanQuota = map[string]int{"free": 100, "pro": 7500, "ultra": 75000, "mega": 150000}

// ---------------------------------------------------------------- competitions

// DefaultComp is a competition enabled on a fresh install. Ids are
// API-Football league ids (GET /leagues); they are checked against the
// provider's list on every leagues sync and corrected by name and country if
// the provider numbers them differently. Odds API sport keys are from
// https://the-odds-api.com/sports-odds-data/sports-apis.html ("" = not
// covered there: odds come from API-Football or are typed in by hand).
type DefaultComp struct {
	ID       int64
	Name     string
	Country  string
	Type     string
	SportKey string
	Group    string
	Priority int
}

// DefaultCompetitions: the top European leagues, UEFA club competitions,
// Kenyan and African football, AFCON and World Cup qualifiers.
var DefaultCompetitions = []DefaultComp{
	{39, "Premier League", "England", "League", "soccer_epl", "europe_top", 1},
	{140, "La Liga", "Spain", "League", "soccer_spain_la_liga", "europe_top", 2},
	{135, "Serie A", "Italy", "League", "soccer_italy_serie_a", "europe_top", 3},
	{78, "Bundesliga", "Germany", "League", "soccer_germany_bundesliga", "europe_top", 4},
	{61, "Ligue 1", "France", "League", "soccer_france_ligue_one", "europe_top", 5},
	{40, "Championship", "England", "League", "soccer_efl_champ", "europe_top", 8},
	{88, "Eredivisie", "Netherlands", "League", "soccer_netherlands_eredivisie", "europe_top", 9},
	{94, "Primeira Liga", "Portugal", "League", "soccer_portugal_primeira_liga", "europe_top", 10},
	{2, "UEFA Champions League", "World", "Cup", "soccer_uefa_champs_league", "uefa", 2},
	{3, "UEFA Europa League", "World", "Cup", "soccer_uefa_europa_league", "uefa", 6},
	{848, "UEFA Europa Conference League", "World", "Cup", "soccer_uefa_europa_conference_league", "uefa", 9},
	{276, "Premier League", "Kenya", "League", "", "africa", 3},
	{12, "CAF Champions League", "World", "Cup", "", "africa", 5},
	{20, "CAF Confederation Cup", "World", "Cup", "", "africa", 7},
	{6, "Africa Cup of Nations", "World", "Cup", "soccer_africa_cup_of_nations", "international", 4},
	{36, "Africa Cup of Nations - Qualification", "World", "Cup", "", "international", 6},
	{29, "World Cup - Qualification Africa", "World", "Cup", "", "international", 5},
	{32, "World Cup - Qualification Europe", "World", "Cup", "soccer_fifa_world_cup_qualifiers_europe", "international", 7},
}

// KnownSportKeys maps more API-Football leagues to The Odds API sport keys
// for competitions a super admin enables later.
var KnownSportKeys = map[int64]string{
	1: "soccer_fifa_world_cup", 4: "soccer_uefa_european_championship", 5: "soccer_uefa_nations_league", 9: "soccer_conmebol_copa_america",
	34: "soccer_fifa_world_cup_qualifiers_south_america", 41: "soccer_england_league1", 42: "soccer_england_league2", 45: "soccer_fa_cup",
	48: "soccer_england_efl_cup", 62: "soccer_france_ligue_two", 79: "soccer_germany_bundesliga2", 81: "soccer_germany_dfb_pokal",
	136: "soccer_italy_serie_b", 137: "soccer_italy_coppa_italia", 141: "soccer_spain_segunda_division", 143: "soccer_spain_copa_del_rey",
	144: "soccer_belgium_first_div", 179: "soccer_spl", 203: "soccer_turkey_super_league", 207: "soccer_switzerland_superleague",
	218: "soccer_austria_bundesliga", 253: "soccer_usa_mls", 307: "soccer_saudi_arabia_pro_league",
}

func seed(db *gorm.DB) error {
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&Settings{ID: 1, APIFootballPlan: "free", APIFootballDaily: 90,
		OddsAPIMonthly: 450, OddsRegions: "uk,eu", OddsMarkets: "h2h,totals,spreads", OddsSnapshots: "1440,360,60,10", LivePolling: true,
		LiveIntervalSec: 60, LookaheadDays: 3, SchedulerOn: true, MinEdge: 0.03, KellyFraction: 0.25, KellyCap: 0.02, LastRun: RunSummary{}}).Error; err != nil {
		return err
	}
	for _, d := range DefaultCompetitions {
		c := Competition{ID: d.ID, Name: d.Name, Country: d.Country, Type: d.Type, Enabled: true, Priority: d.Priority,
			OddsSportKey: d.SportKey, Group: d.Group, Coverage: Coverage{}, UpdatedAt: time.Now()}
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&c).Error; err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- settings

// Settings returns the platform settings (cached for a few seconds).
func (s *Service) Settings() (Settings, error) {
	s.settMu.Lock()
	if s.settC != nil && time.Since(s.settAt) < 5*time.Second {
		st := *s.settC
		s.settMu.Unlock()
		return st, nil
	}
	s.settMu.Unlock()
	var st Settings
	err := s.DB.Where("id = 1").First(&st).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if err := seed(s.DB); err != nil {
			return st, err
		}
		err = s.DB.Where("id = 1").First(&st).Error
	}
	if err != nil {
		return st, err
	}
	normalizeSettings(&st)
	s.settMu.Lock()
	c := st
	s.settC, s.settAt = &c, time.Now()
	s.settMu.Unlock()
	return st, nil
}

func (s *Service) invalidateSettings() {
	s.settMu.Lock()
	s.settC = nil
	s.settMu.Unlock()
}

func normalizeSettings(st *Settings) {
	if st.APIFootballPlan == "" {
		st.APIFootballPlan = "free"
	}
	if st.APIFootballDaily <= 0 {
		st.APIFootballDaily = 90
	}
	if st.OddsAPIMonthly <= 0 {
		st.OddsAPIMonthly = 450
	}
	if st.OddsRegions == "" {
		st.OddsRegions = "uk,eu"
	}
	if st.OddsMarkets == "" {
		st.OddsMarkets = "h2h,totals,spreads"
	}
	if st.OddsSnapshots == "" {
		st.OddsSnapshots = "1440,360,60,10"
	}
	if st.LiveIntervalSec < 30 {
		st.LiveIntervalSec = 60
	}
	if st.LookaheadDays <= 0 || st.LookaheadDays > 7 {
		st.LookaheadDays = 3
	}
	if st.MinEdge <= 0 {
		st.MinEdge = 0.03
	}
	if st.KellyFraction <= 0 || st.KellyFraction > 1 {
		st.KellyFraction = 0.25
	}
	if st.KellyCap <= 0 || st.KellyCap > 0.05 {
		st.KellyCap = 0.02
	}
}

// Keys returns the decrypted provider keys ("" when not set).
func (s *Service) Keys() (af, oa string) {
	st, err := s.Settings()
	if err != nil {
		return "", ""
	}
	af, _ = mail.DecryptSecret(st.APIFootballKey)
	oa, _ = mail.DecryptSecret(st.OddsAPIKey)
	return strings.TrimSpace(af), strings.TrimSpace(oa)
}

// Snapshots returns the odds snapshot offsets (minutes before kickoff),
// largest first.
func (st Settings) Snapshots() []int {
	out := []int{}
	for _, p := range strings.Split(st.OddsSnapshots, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 && n <= 7*1440 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		out = []int{1440, 360, 60, 10}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// Regions / Markets of The Odds API requests.
func (st Settings) Regions() []string { return splitList(st.OddsRegions, []string{"uk", "eu"}) }
func (st Settings) Markets() []string {
	return splitList(st.OddsMarkets, []string{"h2h", "totals", "spreads"})
}

func splitList(s string, def []string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// OddsCallCost is the credit cost of one The Odds API odds call:
// markets × regions (https://the-odds-api.com/liveapi/guides/v4/#usage-quota-costs).
func (st Settings) OddsCallCost() int { return len(st.Markets()) * len(st.Regions()) }

// mask shows the last 4 characters of a key.
func mask(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) <= 6 {
		return "••••"
	}
	return "••••" + k[len(k)-4:]
}

// SettingsPatch is a super admin's change (nil = unchanged; an empty key
// string removes it).
type SettingsPatch struct {
	APIFootballKey   *string  `json:"apifootball_key"`
	OddsAPIKey       *string  `json:"oddsapi_key"`
	APIFootballPlan  *string  `json:"apifootball_plan"`
	APIFootballDaily *int     `json:"apifootball_daily_budget"`
	OddsAPIMonthly   *int     `json:"oddsapi_monthly_budget"`
	OddsRegions      *string  `json:"odds_regions"`
	OddsMarkets      *string  `json:"odds_markets"`
	OddsSnapshots    *string  `json:"odds_snapshots"`
	LivePolling      *bool    `json:"live_polling"`
	LiveIntervalSec  *int     `json:"live_interval_sec"`
	LookaheadDays    *int     `json:"lookahead_days"`
	SchedulerOn      *bool    `json:"scheduler_on"`
	MinEdge          *float64 `json:"min_edge"`
	KellyFraction    *float64 `json:"kelly_fraction"`
	KellyCap         *float64 `json:"kelly_cap"`
	// Competitions: ids to enable (all others are disabled) when set.
	Enabled []int64 `json:"enabled_competitions"`
	// SportKeys sets The Odds API sport key of competitions ("" = none).
	SportKeys map[string]string `json:"sport_keys"`
}

// SaveSettings applies a patch.
func (s *Service) SaveSettings(p SettingsPatch, by string) error {
	st, err := s.Settings()
	if err != nil {
		return err
	}
	up := map[string]any{"updated_by": by, "updated_at": s.Now()}
	setKey := func(col string, v *string) error {
		if v == nil {
			return nil
		}
		k := strings.TrimSpace(*v)
		if strings.HasPrefix(k, "••••") {
			return nil // the masked value came back unchanged
		}
		if k != "" && (len(k) < 10 || len(k) > 200 || strings.ContainsAny(k, " \t\r\n")) {
			return inputErr("That doesn't look like an API key. Paste the key exactly as the provider's dashboard shows it.")
		}
		enc, err := mail.EncryptSecret(k)
		if err != nil {
			return err
		}
		up[col] = enc
		return nil
	}
	if err := setKey("apifootball_key", p.APIFootballKey); err != nil {
		return err
	}
	if err := setKey("oddsapi_key", p.OddsAPIKey); err != nil {
		return err
	}
	if p.APIFootballPlan != nil {
		plan := strings.ToLower(strings.TrimSpace(*p.APIFootballPlan))
		if _, ok := PlanQuota[plan]; !ok && plan != "custom" {
			return inputErr("Plan must be free, pro, ultra, mega or custom.")
		}
		up["apifootball_plan"] = plan
		if p.APIFootballDaily == nil {
			if q, ok := PlanQuota[plan]; ok && st.APIFootballDaily > q {
				up["apifootball_daily_budget"] = q * 9 / 10
			} else if ok && plan != st.APIFootballPlan {
				up["apifootball_daily_budget"] = q * 9 / 10
			}
		}
	}
	if p.APIFootballDaily != nil {
		if *p.APIFootballDaily < 10 || *p.APIFootballDaily > 1000000 {
			return inputErr("The API-Football daily budget must be between 10 and 1,000,000 requests.")
		}
		up["apifootball_daily_budget"] = *p.APIFootballDaily
	}
	if p.OddsAPIMonthly != nil {
		if *p.OddsAPIMonthly < 0 || *p.OddsAPIMonthly > 10000000 {
			return inputErr("The Odds API monthly budget must be between 0 and 10,000,000 credits.")
		}
		up["oddsapi_monthly_budget"] = *p.OddsAPIMonthly
	}
	if p.OddsRegions != nil {
		for _, r := range splitList(*p.OddsRegions, nil) {
			if r != "uk" && r != "eu" && r != "us" && r != "us2" && r != "au" {
				return inputErr("Unknown odds region %q (use uk, eu, us, us2 or au).", r)
			}
		}
		up["odds_regions"] = strings.Join(splitList(*p.OddsRegions, []string{"uk", "eu"}), ",")
	}
	if p.OddsMarkets != nil {
		for _, m := range splitList(*p.OddsMarkets, nil) {
			if m != "h2h" && m != "totals" && m != "spreads" {
				return inputErr("Unknown odds market %q (use h2h, totals or spreads).", m)
			}
		}
		up["odds_markets"] = strings.Join(splitList(*p.OddsMarkets, []string{"h2h"}), ",")
	}
	if p.OddsSnapshots != nil {
		tmp := Settings{OddsSnapshots: *p.OddsSnapshots}
		parts := []string{}
		for _, n := range tmp.Snapshots() {
			parts = append(parts, strconv.Itoa(n))
		}
		up["odds_snapshots"] = strings.Join(parts, ",")
	}
	if p.LivePolling != nil {
		up["live_polling"] = *p.LivePolling
	}
	if p.LiveIntervalSec != nil {
		if *p.LiveIntervalSec < 30 || *p.LiveIntervalSec > 900 {
			return inputErr("Live polling must be every 30 to 900 seconds.")
		}
		up["live_interval_sec"] = *p.LiveIntervalSec
	}
	if p.LookaheadDays != nil {
		if *p.LookaheadDays < 1 || *p.LookaheadDays > 7 {
			return inputErr("Look ahead 1 to 7 days.")
		}
		up["lookahead_days"] = *p.LookaheadDays
	}
	if p.SchedulerOn != nil {
		up["scheduler_on"] = *p.SchedulerOn
	}
	if p.MinEdge != nil {
		if *p.MinEdge < 0.005 || *p.MinEdge > 0.5 {
			return inputErr("Minimum edge must be between 0.5%% and 50%%.")
		}
		up["min_edge"] = *p.MinEdge
	}
	if p.KellyFraction != nil {
		if *p.KellyFraction <= 0 || *p.KellyFraction > 0.5 {
			return inputErr("Kelly fraction must be above 0 and at most 0.5 (half Kelly).")
		}
		up["kelly_fraction"] = *p.KellyFraction
	}
	if p.KellyCap != nil {
		if *p.KellyCap <= 0 || *p.KellyCap > 0.05 {
			return inputErr("The stake cap must be above 0 and at most 5%% of the bankroll.")
		}
		up["kelly_cap"] = *p.KellyCap
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Settings{}).Where("id = 1").Updates(up).Error; err != nil {
			return err
		}
		if p.Enabled != nil {
			if len(p.Enabled) > 60 {
				return inputErr("Enable at most 60 competitions.")
			}
			if err := tx.Model(&Competition{}).Where("enabled AND id NOT IN ?", append(p.Enabled, -1)).Update("enabled", false).Error; err != nil {
				return err
			}
			for _, id := range p.Enabled {
				var c Competition
				if tx.Where("id = ?", id).Limit(1).Find(&c).RowsAffected == 0 {
					c = Competition{ID: id, Name: fmt.Sprintf("League %d", id), Priority: 50, OddsSportKey: KnownSportKeys[id], Group: "other", UpdatedAt: s.Now()}
					if err := tx.Create(&c).Error; err != nil {
						return err
					}
				}
				if err := tx.Model(&Competition{}).Where("id = ?", id).Update("enabled", true).Error; err != nil {
					return err
				}
			}
		}
		for k, v := range p.SportKeys {
			id, err := strconv.ParseInt(k, 10, 64)
			if err != nil {
				continue
			}
			v = strings.TrimSpace(v)
			if v != "" && !strings.HasPrefix(v, "soccer_") {
				return inputErr("The Odds API soccer keys start with soccer_ (e.g. soccer_epl).")
			}
			tx.Model(&Competition{}).Where("id = ?", id).Update("odds_sport_key", v)
		}
		return nil
	})
	s.invalidateSettings()
	return err
}

// Competitions lists competitions (enabled first, by priority).
func (s *Service) Competitions(onlyEnabled bool) []Competition {
	out := []Competition{}
	q := s.DB.Order("enabled DESC, priority, name")
	if onlyEnabled {
		q = q.Where("enabled")
	}
	q.Find(&out)
	return out
}

// season guesses the current season (start year) for a competition when
// the provider hasn't told us yet: European-style leagues start in July/August.
func guessSeason(c Competition, now time.Time) int {
	if c.Season > 0 {
		return c.Season
	}
	if c.Type == "Cup" && c.Group == "international" {
		return now.Year()
	}
	if now.Month() >= time.July {
		return now.Year()
	}
	return now.Year() - 1
}
