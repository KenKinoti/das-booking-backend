package scout

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/assistant"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Service is the market scout.
type Service struct {
	DB *gorm.DB
	// FX converts estimated values into the home currency (optional).
	FX *fxrates.Service
	// HTTP calls the Anthropic API (nil: a client with a 6-minute timeout).
	HTTP *http.Client
	// AI resolves the Anthropic configuration (nil: assistant.Resolve(DB)).
	AI func() assistant.Config
	// BusinessType names the organisation's business type (optional).
	BusinessType func(org string) string
	// CRMAllowed says whether the user may create CRM leads / deals (plan and
	// role); nil = allowed. The string is the reason when not.
	CRMAllowed func(org, userID, role string) (bool, string)
	// AppURL is the web app's address (links in notifications and CRM notes).
	AppURL string
	// TestClock enables the test clock endpoints (SCOUT_TEST_CLOCK=1).
	TestClock bool
	// Now is the clock (tests).
	Now func() time.Time

	wg sync.WaitGroup
}

// testOffset moves every clock of the package (SCOUT_TEST_CLOCK only): the
// service and BriefSection see the same "now".
var testOffset atomic.Int64

// clockNow is the package clock.
func clockNow() time.Time { return time.Now().Add(time.Duration(testOffset.Load())) }

// New builds the service from the environment.
func New(db *gorm.DB, fx *fxrates.Service) *Service {
	tc := strings.TrimSpace(os.Getenv("SCOUT_TEST_CLOCK"))
	return &Service{DB: db, FX: fx, AppURL: strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_APP_URL")), "/"), TestClock: tc != "" && tc != "0" && tc != "false"}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return clockNow()
}

// SetClock moves the test clock (TestClock only): to a time, or back to now.
func (s *Service) SetClock(at *time.Time) time.Time {
	if at == nil {
		testOffset.Store(0)
	} else {
		testOffset.Store(int64(time.Until(*at)))
	}
	return s.now()
}

func (s *Service) ai() assistant.Config {
	if s.AI != nil {
		return s.AI()
	}
	return assistant.Resolve(s.DB)
}

// Wait blocks until background scans finish (tests).
func (s *Service) Wait() { s.wg.Wait() }

// Loc is the organisation's time zone.
func (s *Service) Loc(org string) *time.Location {
	if l, err := time.LoadLocation(orgprefs.Get(s.DB, org).Timezone); err == nil {
		return l
	}
	return time.UTC
}

func localDay(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}

// link is a deep link into the web app.
func (s *Service) link(path string) string { return s.AppURL + path }

// CardPath is the in-app path of a card.
func CardPath(id string) string { return "/insights?tab=scout&opp=" + id }

// TabPath is the in-app path of the tab.
const TabPath = "/insights?tab=scout"

// ---------------------------------------------------------------- settings

// ErrForbidden / ErrNotFound / ErrInvalid are mapped to HTTP statuses.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
)

// InputError is a validation problem with a message for the user.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func bad(format string, a ...any) error { return &InputError{Msg: fmt.Sprintf(format, a...)} }

// Settings returns the organisation's settings, creating them (profile from
// the data, default focus, switched off) on first use.
func (s *Service) Settings(org string) (Settings, error) {
	var st Settings
	err := s.DB.Where("organization_id = ?", org).Limit(1).Find(&st).Error
	if err != nil {
		return st, err
	}
	if st.OrganizationID != "" {
		return st, nil
	}
	now := s.now()
	st = defaultSettings(org)
	st.Profile = s.BuildProfile(org)
	st.ProfileBuiltAt = &now
	st.Focus = DefaultFocus(st.Profile)
	if err := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&st).Error; err != nil {
		return st, err
	}
	err = s.DB.Where("organization_id = ?", org).First(&st).Error
	return st, err
}

func defaultSettings(org string) Settings {
	return Settings{OrganizationID: org, DailyEnabled: true, DailyAt: "06:30", DailySearches: 5, WeeklyEnabled: true, WeeklyDay: int(time.Monday), WeeklySearches: 20, MonthlyBudgetUSD: 10}
}

// RefreshProfile rebuilds the profile from the data, keeping the owner's
// description.
func (s *Service) RefreshProfile(org, by string) (Settings, error) {
	st, err := s.Settings(org)
	if err != nil {
		return st, err
	}
	p := s.BuildProfile(org)
	p.Description = st.Profile.Description
	now := s.now()
	st.Profile, st.ProfileBuiltAt, st.UpdatedBy = p, &now, by
	return st, s.DB.Model(&Settings{}).Where("organization_id = ?", org).Updates(map[string]any{"profile": asJSON(p), "profile_built_at": now, "updated_by": by}).Error
}

// SettingsPatch is what an editor may change (nil = unchanged).
type SettingsPatch struct {
	Profile        *Profile `json:"profile"`
	Focus          *Focus   `json:"focus"`
	DailyEnabled   *bool    `json:"daily_enabled"`
	DailyAt        *string  `json:"daily_at"`
	DailySearches  *int     `json:"daily_searches"`
	WeeklyEnabled  *bool    `json:"weekly_enabled"`
	WeeklyDay      *int     `json:"weekly_day"`
	WeeklySearches *int     `json:"weekly_searches"`
	// MonthlyBudgetUSD needs a super admin (the platform key pays).
	MonthlyBudgetUSD *float64 `json:"monthly_budget_usd"`
}

// MaxBudgetUSD bounds the monthly budget.
const MaxBudgetUSD = 500

func validTime(s string) bool {
	_, err := time.Parse("15:04", s)
	return err == nil && len(s) == 5
}

// SaveSettings applies a patch. superAdmin is needed for the budget.
func (s *Service) SaveSettings(org, by string, superAdmin bool, p SettingsPatch) (Settings, error) {
	st, err := s.Settings(org)
	if err != nil {
		return st, err
	}
	up := map[string]any{"updated_by": by}
	if p.Profile != nil {
		pr := cleanProfile(*p.Profile)
		// Facts from the data are not editable here (Refresh rebuilds them).
		pr.Currency, pr.Timezone = st.Profile.Currency, st.Profile.Timezone
		pr.Countries, pr.Sectors, pr.TargetMarkets = st.Profile.Countries, st.Profile.Sectors, st.Profile.TargetMarkets
		up["profile"] = asJSON(pr)
	}
	if p.Focus != nil {
		up["focus"] = asJSON(cleanFocus(*p.Focus, st.Profile.Currency))
	}
	if p.DailyEnabled != nil {
		up["daily_enabled"] = *p.DailyEnabled
	}
	if p.DailyAt != nil {
		if !validTime(*p.DailyAt) {
			return st, bad("The daily scan time must be HH:MM, e.g. 06:30.")
		}
		up["daily_at"] = *p.DailyAt
	}
	if p.DailySearches != nil {
		if *p.DailySearches < 1 || *p.DailySearches > MaxSearchesLight {
			return st, bad("Searches per daily scan must be between 1 and %d.", MaxSearchesLight)
		}
		up["daily_searches"] = *p.DailySearches
	}
	if p.WeeklyEnabled != nil {
		up["weekly_enabled"] = *p.WeeklyEnabled
	}
	if p.WeeklyDay != nil {
		if *p.WeeklyDay < 0 || *p.WeeklyDay > 6 {
			return st, bad("Choose a day of the week for the weekly scan.")
		}
		up["weekly_day"] = *p.WeeklyDay
	}
	if p.WeeklySearches != nil {
		if *p.WeeklySearches < 1 || *p.WeeklySearches > MaxSearchesDeep {
			return st, bad("Searches per weekly scan must be between 1 and %d.", MaxSearchesDeep)
		}
		up["weekly_searches"] = *p.WeeklySearches
	}
	if p.MonthlyBudgetUSD != nil {
		if !superAdmin {
			return st, ErrForbidden
		}
		if *p.MonthlyBudgetUSD < 1 || *p.MonthlyBudgetUSD > MaxBudgetUSD {
			return st, bad("The monthly budget must be between US$1 and US$%d.", MaxBudgetUSD)
		}
		up["monthly_budget_usd"] = round2(*p.MonthlyBudgetUSD)
	}
	if err := s.DB.Model(&Settings{}).Where("organization_id = ?", org).Updates(up).Error; err != nil {
		return st, err
	}
	return s.Settings(org)
}

// Enable turns the scout on after the cost was confirmed (super admins).
func (s *Service) Enable(org, by string, budget float64, confirmedEstimate float64) (Settings, error) {
	st, err := s.Settings(org)
	if err != nil {
		return st, err
	}
	cfg := s.ai()
	if !cfg.Enabled() {
		if cfg.Problem != "" {
			return st, bad("%s", cfg.Problem)
		}
		return st, bad("No Anthropic API key is saved. Add it in System settings → AI assistant first.")
	}
	if budget <= 0 {
		budget = st.MonthlyBudgetUSD
	}
	if budget < 1 || budget > MaxBudgetUSD {
		return st, bad("The monthly budget must be between US$1 and US$%d.", MaxBudgetUSD)
	}
	now := s.now()
	err = s.DB.Model(&Settings{}).Where("organization_id = ?", org).Updates(map[string]any{
		"enabled": true, "monthly_budget_usd": round2(budget), "cost_confirmed_at": now, "cost_confirmed_by": by,
		"confirmed_estimate_usd": round2(confirmedEstimate), "enabled_by": by, "enabled_at": now, "updated_by": by,
		// The first scheduled scan is tomorrow's: today's slot counts as done.
		"last_slot": localDay(now, s.Loc(org)).Format("2006-01-02"),
	}).Error
	if err != nil {
		return st, err
	}
	return s.Settings(org)
}

// Disable switches scheduled and manual scans off (the feed stays).
func (s *Service) Disable(org, by string) (Settings, error) {
	if _, err := s.Settings(org); err != nil {
		return Settings{}, err
	}
	if err := s.DB.Model(&Settings{}).Where("organization_id = ?", org).Updates(map[string]any{"enabled": false, "updated_by": by}).Error; err != nil {
		return Settings{}, err
	}
	return s.Settings(org)
}

// ---------------------------------------------------------------- spend

// MonthSpend is what scans cost this calendar month (organisation time).
func (s *Service) MonthSpend(org string, loc *time.Location) (spent float64, month string) {
	start, end := monthBounds(s.now(), loc)
	s.DB.Model(&Run{}).Where("organization_id = ? AND started_at >= ? AND started_at < ?", org, start, end).Select("COALESCE(SUM(cost_usd), 0)").Row().Scan(&spent)
	return round4(spent), start.Format("2006-01")
}

// monthBounds is the calendar month of t in loc: [start, end).
func monthBounds(t time.Time, loc *time.Location) (time.Time, time.Time) {
	n := t.In(loc)
	start := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 1, 0)
}

// ---------------------------------------------------------------- status

// NextRun is when the next scheduled scan is due.
type NextRun struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
}

// Status is the overview the tab opens with.
type Status struct {
	Settings   Settings       `json:"settings"`
	CanEdit    bool           `json:"can_edit"`
	CanEnable  bool           `json:"can_enable"` // super admin
	KeyOK      bool           `json:"key_ok"`
	KeySource  string         `json:"key_source"`
	KeyProblem string         `json:"key_problem,omitempty"`
	Model      string         `json:"model"`
	Estimate   Estimate       `json:"estimate"`
	Month      string         `json:"month"`
	Spent      float64        `json:"spent_usd"`
	Left       float64        `json:"left_usd"`
	BudgetHit  bool           `json:"budget_reached"`
	Next       []NextRun      `json:"next"`
	LastRun    *Run           `json:"last_run"`
	LastScan   *Run           `json:"last_scan"` // the last run that called the API (not refused by the budget)
	Running    *Run           `json:"running"`
	Counts     map[string]int `json:"counts"`
	Timezone   string         `json:"timezone"`
	Today      string         `json:"today"`
	Types      []Choice       `json:"types"`
	Reasons    []Choice       `json:"dismiss_reasons"`
	Suggested  []Source       `json:"suggested_sources"`
	TestClock  bool           `json:"test_clock"`
	Now        time.Time      `json:"now"`
}

// StatusFor builds the overview.
func (s *Service) StatusFor(org string) (*Status, error) {
	st, err := s.Settings(org)
	if err != nil {
		return nil, err
	}
	s.expire(org)
	loc := s.Loc(org)
	cfg := s.ai()
	out := &Status{Settings: st, KeyOK: cfg.Enabled(), KeySource: cfg.Source, KeyProblem: cfg.Problem, Model: cfg.Model,
		Estimate: EstimateFor(cfg.Model, st), Timezone: loc.String(), Today: localDay(s.now(), loc).Format("2006-01-02"),
		Types: Types, Reasons: DismissReasons, Suggested: SuggestedSources, TestClock: s.TestClock, Now: s.now(), Counts: map[string]int{}}
	out.Spent, out.Month = s.MonthSpend(org, loc)
	out.Left = round4(st.MonthlyBudgetUSD - out.Spent)
	if out.Left < 0 {
		out.Left = 0
	}
	// Reached: spent it all, or what is left cannot pay for even a one-search scan.
	out.BudgetHit = st.MonthlyBudgetUSD > 0 && (out.Spent >= st.MonthlyBudgetUSD || FitSearches(cfg.Model, 1, LightMaxTokens, out.Left) == 0)
	if st.Enabled {
		out.Next = NextRuns(st, s.now(), loc)
	} else {
		out.Next = []NextRun{}
	}
	var last Run
	if s.DB.Omit("prompt", "answer_text").Where("organization_id = ? AND status <> ?", org, RunRunning).Order("started_at DESC").Limit(1).Find(&last).Error == nil && last.ID != "" {
		out.LastRun = &last
	}
	var scan Run
	if s.DB.Omit("prompt", "answer_text").Where("organization_id = ? AND status IN ?", org, []string{RunOK, RunError}).Order("started_at DESC").Limit(1).Find(&scan).Error == nil && scan.ID != "" {
		out.LastScan = &scan
	}
	var running Run
	if s.DB.Omit("prompt", "answer_text").Where("organization_id = ? AND status = ? AND started_at > ?", org, RunRunning, s.now().Add(-staleRun)).Order("started_at DESC").Limit(1).Find(&running).Error == nil && running.ID != "" {
		out.Running = &running
	}
	var counts []struct {
		Status string
		N      int
	}
	s.DB.Model(&Opportunity{}).Select("status, COUNT(*) AS n").Where("organization_id = ?", org).Group("status").Scan(&counts)
	for _, c := range counts {
		out.Counts[c.Status] = c.N
	}
	return out, nil
}

// ---------------------------------------------------------------- scans

// PerSearchWorst is the input tokens one search may add in the worst case:
// the built-in assumption, or — when this organisation's recent scans read
// more — 1.25 × the most they read per search. The budget check learns from
// real usage, so a cap is not overshot twice by the same kind of scan.
func (s *Service) PerSearchWorst(org string) int {
	var obs float64
	s.DB.Raw(`SELECT COALESCE(MAX(GREATEST(x.input_tokens + x.cache_write + x.cache_read - ?, 0)::float / x.searches), 0)
		FROM (SELECT input_tokens, cache_write, cache_read, searches FROM scout_runs
		      WHERE organization_id = ? AND searches > 0 AND status IN ? ORDER BY started_at DESC LIMIT 20) x`,
		PromptTokens, org, []string{RunOK, RunError}).Row().Scan(&obs)
	if p := int(obs * 1.25); p > TokensPerSearchWorst {
		return p
	}
	return TokensPerSearchWorst
}

// staleRun is when a run still marked running is considered dead (a
// restart in the middle of a scan).
const staleRun = 15 * time.Minute

// ErrBusy means a scan is already running for the organisation.
var ErrBusy = errors.New("a scan is already running")

// StartOpts describes a scan to start.
type StartOpts struct {
	Kind    string
	Trigger string // schedule | manual | ai
	By      string
	ByName  string
}

// Start checks that a scan may run, records it as running and runs it in
// the background. The returned run is already finished (status budget or
// skipped) when it was refused before calling the API.
func (s *Service) Start(org string, o StartOpts) (*Run, error) {
	if o.Kind != KindDeep {
		o.Kind = KindLight
	}
	st, err := s.Settings(org)
	if err != nil {
		return nil, err
	}
	if !st.Enabled {
		return nil, bad("Market scout is switched off. Turn it on (and confirm the cost) in Set up.")
	}
	cfg := s.ai()
	if !cfg.Enabled() {
		msg := "No Anthropic API key is saved. Add it in System settings → AI assistant."
		if cfg.Problem != "" {
			msg = cfg.Problem
		}
		return nil, bad("%s", msg)
	}
	run := &Run{ID: uuid.NewString(), OrganizationID: org, Kind: o.Kind, Trigger: o.Trigger, By: o.By, ByName: clip(o.ByName, 200), Status: RunRunning,
		StartedAt: s.now(), Model: cfg.Model, MaxTokens: MaxTokensFor(o.Kind), Queries: []string{}, SearchErrors: []string{}}
	// Claim: one scan per organisation at a time (row lock on the settings).
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var lock Settings
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("organization_id = ?", org).First(&lock).Error; err != nil {
			return err
		}
		tx.Model(&Run{}).Where("organization_id = ? AND status = ? AND started_at <= ?", org, RunRunning, s.now().Add(-staleRun)).
			Updates(map[string]any{"status": RunError, "message": "The scan was interrupted (the server restarted).", "finished_at": s.now()})
		var n int64
		tx.Model(&Run{}).Where("organization_id = ? AND status = ?", org, RunRunning).Count(&n)
		if n > 0 {
			return ErrBusy
		}
		return tx.Create(run).Error
	})
	if err != nil {
		return nil, err
	}
	// Budget before anything is spent.
	loc := s.Loc(org)
	spent, _ := s.MonthSpend(org, loc)
	left := st.MonthlyBudgetUSD - spent
	want := st.DailySearches
	if o.Kind == KindDeep {
		want = st.WeeklySearches
	}
	per := s.PerSearchWorst(org)
	n := FitSearchesPer(cfg.Model, want, run.MaxTokens, per, left)
	if n == 0 {
		msg := fmt.Sprintf("Monthly budget reached: US$%.2f of US$%.2f spent this month, and even a one-search scan could cost more than what is left. Scans stop until %s, or raise the budget in Set up.",
			spent, st.MonthlyBudgetUSD, nextMonth(s.now(), loc))
		s.finish(run, RunBudget, msg)
		s.notifyBudget(org, st, spent)
		return run, nil
	}
	run.MaxSearches = n
	run.EstimateUSD = round4(WorstCasePer(cfg.Model, n, run.MaxTokens, per))
	if n < want {
		run.Message = fmt.Sprintf("Limited to %d of %d searches to stay within the monthly budget (US$%.2f left).", n, want, left)
	}
	s.DB.Model(&Run{}).Where("id = ?", run.ID).Updates(map[string]any{"max_searches": n, "estimate_usd": run.EstimateUSD, "message": run.Message})
	// The background scan works on its own copy; the caller gets the
	// snapshot of the run as it started.
	bg := *run
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("scout: scan %s panicked: %v", bg.ID, r)
				s.finish(&bg, RunError, "The scan failed unexpectedly.")
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cancel()
		s.execute(ctx, &bg, st, cfg, left)
	}()
	return run, nil
}

func nextMonth(now time.Time, loc *time.Location) string {
	n := now.In(loc)
	return time.Date(n.Year(), n.Month()+1, 1, 0, 0, 0, 0, loc).Format("2 January")
}

// finish records a run's outcome.
func (s *Service) finish(run *Run, status, msg string) {
	now := s.now()
	run.Status, run.FinishedAt = status, &now
	if msg != "" {
		if run.Message != "" && status == RunOK {
			msg = msg + " " + run.Message
		}
		run.Message = msg
	}
	if err := s.DB.Model(&Run{}).Where("id = ?", run.ID).Select("*").Omit("id", "organization_id", "created_at").Updates(run).Error; err != nil {
		log.Printf("scout: save run %s: %v", run.ID, err)
	}
	s.DB.Model(&Settings{}).Where("organization_id = ?", run.OrganizationID).Update("last_run_at", now)
}

// GetRun returns one run of the organisation.
func (s *Service) GetRun(org, id string) (*Run, error) {
	var r Run
	if err := s.DB.Where("organization_id = ? AND id = ?", org, id).First(&r).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

// WaitRun polls a run until it is no longer running or the time is up.
func (s *Service) WaitRun(ctx context.Context, org, id string, max time.Duration) (*Run, error) {
	deadline := time.Now().Add(max)
	for {
		r, err := s.GetRun(org, id)
		if err != nil || r.Status != RunRunning || time.Now().After(deadline) {
			return r, err
		}
		select {
		case <-ctx.Done():
			return r, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// execute calls the API, validates the answer and stores what it found.
func (s *Service) execute(ctx context.Context, run *Run, st Settings, cfg assistant.Config, left float64) {
	org := run.OrganizationID
	loc := s.Loc(org)
	today := localDay(s.now(), loc)
	s.expire(org)
	known, dismissed := s.memory(org)
	prompt := BuildPrompt(promptInput{Settings: st, Kind: run.Kind, MaxSearches: run.MaxSearches, Today: s.now(), Loc: loc, Known: known, Dismissed: dismissed,
		MaxOpps: map[bool]int{true: 15, false: 6}[run.Kind == KindDeep], MaxSignals: map[bool]int{true: 6, false: 3}[run.Kind == KindDeep], OnlySources: st.Focus.OnlySources && len(st.Focus.Sources) > 0})
	run.Prompt = prompt
	tool := webSearchTool{Type: WebSearchToolType, Name: "web_search", MaxUses: run.MaxSearches}
	if st.Focus.OnlySources && len(st.Focus.Sources) > 0 {
		for _, src := range st.Focus.Sources {
			tool.AllowedDomains = append(tool.AllowedDomains, src.Domain)
		}
	} else if len(st.Focus.BlockedDomains) > 0 {
		tool.BlockedDomains = append(tool.BlockedDomains, st.Focus.BlockedDomains...)
	}
	if cc := st.Profile.HomeCountry; cc != "" {
		tool.UserLocation = &userLocation{Type: "approximate", Country: cc, Timezone: loc.String()}
	}
	req := apiRequest{Model: cfg.Model, MaxTokens: run.MaxTokens, System: SystemPrompt, Tools: []webSearchTool{tool},
		Messages: []apiMessage{{Role: "user", Content: jsonString(prompt)}}}
	cl := &client{cfg: cfg, http: s.HTTP}
	ex, err := cl.converse(ctx, req, cfg.Model, left)
	if ex != nil {
		run.Requests, run.StopReason = ex.Requests, ex.StopReason
		run.Searches = ex.Usage.Searches
		run.InputTokens, run.OutputTokens, run.CacheWrite, run.CacheRead = ex.Usage.Input, ex.Usage.Output, ex.Usage.CacheWrite, ex.Usage.CacheRead
		run.SearchCostUSD, run.TokenCostUSD, run.CostUSD = Cost(cfg.Model, ex.Usage)
		if r := RateFor(cfg.Model); !r.Known {
			run.PriceNote = "Priced at the highest published rate: " + cfg.Model + " is not in the price table."
		}
		if ex.Queries != nil {
			run.Queries = ex.Queries
		}
		if ex.SearchErrs != nil {
			run.SearchErrors = ex.SearchErrs
		}
		run.ResultURLs = len(ex.Results)
		run.AnswerText = clip(ex.AllText, 30000)
	}
	defer s.afterRun(org, st)
	if err != nil {
		s.finish(run, RunError, Friendly(err))
		return
	}
	ans, perr := ExtractAnswer(ex.Text)
	if perr != nil {
		ans, perr = ExtractAnswer(ex.AllText)
	}
	if perr != nil {
		msg := "The AI answer could not be read (no JSON), so nothing was added. The searches it ran are still billed."
		if ex.Interrupted != "" {
			msg = ex.Interrupted + " " + msg
		} else if ex.StopReason == "max_tokens" {
			msg = "The answer was cut off at the output limit, so nothing could be read. The searches it ran are still billed."
		}
		s.finish(run, RunError, msg)
		return
	}
	home := st.Profile.Currency
	if home == "" {
		home = orgprefs.Get(s.DB, org).Currency
	}
	cands, sigs, stats := Validate(ans, ex, today, home)
	run.Found, run.Unverified, run.Expired, run.Duplicates, run.Invalid = stats.Found, stats.Unverified, stats.Expired, stats.Duplicates, stats.Invalid
	run.SignalsFound = stats.SignalsFound
	kept, updated, filtered := s.ingest(org, run, st, cands, dismissed, today, home)
	run.Kept, run.Updated, run.Filtered = kept, updated, filtered
	run.SignalsKept = s.ingestSignals(org, run, sigs)
	parts := []string{fmt.Sprintf("%d new", kept)}
	if updated > 0 {
		parts = append(parts, fmt.Sprintf("%d seen before", updated))
	}
	dropped := run.Unverified + run.Expired + run.Duplicates + run.Invalid + run.Filtered
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d dropped", dropped))
	}
	msg := fmt.Sprintf("%d search%s · %s · %d market signal%s.", run.Searches, map[bool]string{true: "", false: "es"}[run.Searches == 1], strings.Join(parts, ", "), run.SignalsKept, plural(run.SignalsKept))
	if ex.Interrupted != "" {
		msg += " " + ex.Interrupted
	}
	s.finish(run, RunOK, msg)
	if kept > 0 && run.Trigger == "schedule" && st.EnabledBy != "" {
		notify.Go(s.DB, org, notify.Users(st.EnabledBy), notify.Message{Type: "scout.found", Severity: notify.Info,
			Title: fmt.Sprintf("Market scout: %d new opportunit%s", kept, map[bool]string{true: "y", false: "ies"}[kept == 1]),
			Body:  "Ranked by fit in Insights → Market scout.", Link: TabPath, EntityType: "scout_run", EntityID: run.ID})
	}
}

// asJSON is a serializer:json column's value for map updates (gorm applies
// serializers to struct fields only).
func asJSON(v any) string {
	b, _ := jsonMarshal(v)
	return string(b)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func jsonString(s string) []byte {
	b, _ := jsonMarshal(s)
	return b
}

// afterRun notifies when the month's spend reached the budget.
func (s *Service) afterRun(org string, st Settings) {
	loc := s.Loc(org)
	spent, _ := s.MonthSpend(org, loc)
	if st.MonthlyBudgetUSD > 0 && spent >= st.MonthlyBudgetUSD {
		s.notifyBudget(org, st, spent)
	}
}

// notifyBudget tells the admins (once a month) that scans stopped.
func (s *Service) notifyBudget(org string, st Settings, spent float64) {
	month := s.now().In(s.Loc(org)).Format("2006-01")
	res := s.DB.Model(&Settings{}).Where("organization_id = ? AND COALESCE(budget_notified,'') <> ?", org, month).Update("budget_notified", month)
	if res.RowsAffected == 0 {
		return
	}
	to := notify.Admins()
	if st.EnabledBy != "" {
		to = to.Plus(st.EnabledBy)
	}
	notify.Go(s.DB, org, to, notify.Message{Type: "scout.budget", Severity: notify.Warning,
		Title: "Market scout stopped: monthly budget reached",
		Body:  fmt.Sprintf("US$%.2f of US$%.2f spent this month. Scans resume next month, or raise the budget in Insights → Market scout → Set up.", spent, st.MonthlyBudgetUSD),
		Link:  TabPath + "&view=setup", EntityType: "scout_budget", EntityID: org + ":" + month, Dedupe: true})
}

// memory is what the scan should not repeat: cards already in the feed and
// what was dismissed (newest first).
func (s *Service) memory(org string) (known, dismissed []Opportunity) {
	s.DB.Where("organization_id = ? AND status IN ?", org, []string{StatusNew, StatusSaved, StatusPursued}).Order("fit DESC, first_seen_at DESC").Limit(40).Find(&known)
	since := s.now().AddDate(0, 0, -180)
	s.DB.Where("organization_id = ? AND status = ? AND dismissed_at >= ?", org, StatusDismissed, since).Order("dismissed_at DESC").Limit(60).Find(&dismissed)
	return
}

// toHome converts a value into the home currency (nil when no rate).
func (s *Service) toHome(org string, v float64, cur, home string) *float64 {
	if cur == "" || home == "" {
		return nil
	}
	if strings.EqualFold(cur, home) {
		return &v
	}
	if s.FX == nil {
		return nil
	}
	q := s.FX.Quote(org, cur, home)
	if !q.OK() {
		return nil
	}
	h := round2(v * q.Rate)
	return &h
}

// ingest stores new cards and refreshes the ones seen before.
func (s *Service) ingest(org string, run *Run, st Settings, cands []Candidate, dismissed []Opportunity, today time.Time, home string) (kept, updated, filtered int) {
	var existing []Opportunity
	s.DB.Where("organization_id = ? AND first_seen_at >= ?", org, s.now().AddDate(-1, 0, 0)).Find(&existing)
	minHome := 0.0
	if st.Focus.MinValue > 0 {
		if v := s.toHome(org, st.Focus.MinValue, st.Focus.MinValueCurrency, home); v != nil {
			minHome = *v
		}
	}
	sc := newScorer(st, dismissed, today, minHome)
	excl := []string{}
	for _, e := range st.Focus.Exclusions {
		if e = strings.ToLower(strings.TrimSpace(e)); len(e) >= 3 {
			excl = append(excl, e)
		}
	}
	now := s.now()
	for _, c := range cands {
		o := c.Opportunity
		if o.EstValue != nil {
			o.ValueHome = s.toHome(org, *o.EstValue, o.Currency, home)
		}
		text := " " + strings.ToLower(o.Title+" "+o.Summary+" "+o.Buyer) + " "
		if len(hits(text, excl)) > 0 {
			filtered++
			continue
		}
		if minHome > 0 && o.ValueHome != nil && *o.ValueHome < minHome {
			filtered++
			continue
		}
		var same *Opportunity
		for i := range existing {
			if SameOpportunity(&existing[i], &o) {
				same = &existing[i]
				break
			}
		}
		if same != nil {
			up := map[string]any{"last_seen_at": now, "seen_count": gorm.Expr("seen_count + 1"), "last_run_id": run.ID, "sources": asJSON(mergeSources(same.Sources, o.Sources))}
			if same.Deadline == nil && o.Deadline != nil {
				up["deadline"], up["deadline_text"] = o.Deadline, ""
			}
			if same.EstValue == nil && o.EstValue != nil {
				up["est_value"], up["currency"], up["value_home"] = o.EstValue, o.Currency, o.ValueHome
			}
			s.DB.Model(&Opportunity{}).Where("id = ?", same.ID).Updates(up)
			updated++
			continue
		}
		sc.score(&o)
		o.ID, o.OrganizationID, o.Status, o.RunID, o.LastRunID = uuid.NewString(), org, StatusNew, run.ID, run.ID
		o.FirstSeenAt, o.LastSeenAt, o.SeenCount = now, now, 1
		if err := s.DB.Create(&o).Error; err != nil {
			log.Printf("scout: store opportunity: %v", err)
			continue
		}
		existing = append(existing, o)
		kept++
	}
	return
}

// ingestSignals stores market signals (refreshing ones seen in the last 60 days).
func (s *Service) ingestSignals(org string, run *Run, sigs []SignalCand) int {
	var existing []Signal
	s.DB.Where("organization_id = ? AND last_seen_at >= ?", org, s.now().AddDate(0, 0, -60)).Find(&existing)
	now := s.now()
	kept := 0
	for _, c := range sigs {
		sg := c.Signal
		dup := false
		for _, e := range existing {
			if e.Country == sg.Country && similar(e.SignalKey, sg.SignalKey) >= 0.75 {
				s.DB.Model(&Signal{}).Where("id = ?", e.ID).Updates(map[string]any{"last_seen_at": now, "seen_count": gorm.Expr("seen_count + 1"), "sources": asJSON(mergeSources(e.Sources, sg.Sources))})
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		sg.ID, sg.OrganizationID, sg.RunID, sg.FirstSeenAt, sg.LastSeenAt, sg.SeenCount = uuid.NewString(), org, run.ID, now, now, 1
		if err := s.DB.Create(&sg).Error; err == nil {
			existing = append(existing, sg)
			kept++
		}
	}
	return kept
}

// expire marks cards whose deadline passed (organisation's calendar).
func (s *Service) expire(org string) {
	today := localDay(s.now(), s.Loc(org))
	s.DB.Model(&Opportunity{}).Where("organization_id = ? AND status IN ? AND deadline IS NOT NULL AND deadline < ?", org, []string{StatusNew, StatusSaved}, today.Format("2006-01-02")).
		Update("status", StatusExpired)
}

// ---------------------------------------------------------------- feed

// Filter narrows the feed.
type Filter struct {
	Status   string // active (new + saved, default) | new | saved | pursued | dismissed | expired | all
	Type     string
	Country  string
	Deadline string // 7 | 30 | 90 | none
	Q        string
	Sort     string // fit (default) | deadline | newest | value
	Limit    int
	Offset   int
	Since    *time.Time
}

// Facet is a filter value with a count.
type Facet struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Feed is the list response.
type Feed struct {
	Items     []Opportunity  `json:"items"`
	Total     int64          `json:"total"`
	Types     []Facet        `json:"types"`
	Countries []Facet        `json:"countries"`
	Statuses  map[string]int `json:"statuses"`
	Today     string         `json:"today"`
	Currency  string         `json:"currency"`
}

// List returns the feed.
func (s *Service) List(org string, f Filter) (*Feed, error) {
	s.expire(org)
	loc := s.Loc(org)
	today := localDay(s.now(), loc)
	switch f.Status {
	case "", "active", "all", StatusNew, StatusSaved, StatusPursued, StatusDismissed, StatusExpired:
	default:
		return nil, bad("Unknown status")
	}
	// base is the organisation's cards in the chosen status (a fresh
	// statement each time: the facets must not inherit the filters).
	base := func() *gorm.DB {
		q := s.DB.Model(&Opportunity{}).Where("organization_id = ?", org)
		switch f.Status {
		case "", "active":
			q = q.Where("status IN ?", []string{StatusNew, StatusSaved})
		case "all":
		default:
			q = q.Where("status = ?", f.Status)
		}
		return q
	}
	q := base()
	if f.Type != "" {
		q = q.Where("type = ?", f.Type)
	}
	if f.Country != "" {
		if f.Country == "-" {
			q = q.Where("COALESCE(country,'') = ''")
		} else {
			q = q.Where("country = ?", strings.ToUpper(f.Country))
		}
	}
	switch f.Deadline {
	case "7", "30", "90":
		days := map[string]int{"7": 7, "30": 30, "90": 90}[f.Deadline]
		q = q.Where("deadline IS NOT NULL AND deadline >= ? AND deadline <= ?", today.Format("2006-01-02"), today.AddDate(0, 0, days).Format("2006-01-02"))
	case "none":
		q = q.Where("deadline IS NULL")
	}
	if t := strings.TrimSpace(f.Q); t != "" {
		like := "%" + strings.NewReplacer("%", `\%`, "_", `\_`).Replace(t) + "%"
		q = q.Where("(title ILIKE ? OR summary ILIKE ? OR buyer ILIKE ? OR location ILIKE ? OR notes ILIKE ?)", like, like, like, like, like)
	}
	if f.Since != nil {
		q = q.Where("first_seen_at >= ?", *f.Since)
	}
	out := &Feed{Items: []Opportunity{}, Types: []Facet{}, Countries: []Facet{}, Statuses: map[string]int{}, Today: today.Format("2006-01-02")}
	q.Session(&gorm.Session{}).Count(&out.Total)
	switch f.Sort {
	case "deadline":
		q = q.Order("deadline ASC NULLS LAST").Order("fit DESC")
	case "newest":
		q = q.Order("first_seen_at DESC").Order("fit DESC")
	case "value":
		q = q.Order("value_home DESC NULLS LAST").Order("fit DESC")
	default:
		q = q.Order("fit DESC").Order("deadline ASC NULLS LAST").Order("first_seen_at DESC")
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if err := q.Limit(f.Limit).Offset(f.Offset).Find(&out.Items).Error; err != nil {
		return nil, err
	}
	var types []struct {
		Type string
		N    int
	}
	base().Select("type, COUNT(*) AS n").Group("type").Scan(&types)
	for _, c := range Types {
		for _, t := range types {
			if t.Type == c.Key {
				out.Types = append(out.Types, Facet{Key: c.Key, Label: TypeLabel(c.Key), Count: t.N})
			}
		}
	}
	var cs []struct {
		Country string
		N       int
	}
	base().Select("COALESCE(country,'') AS country, COUNT(*) AS n").Group("COALESCE(country,'')").Scan(&cs)
	for _, c := range cs {
		key, label := c.Country, countryLabel(c.Country)
		if key == "" {
			key, label = "-", "No country"
		}
		out.Countries = append(out.Countries, Facet{Key: key, Label: label, Count: c.N})
	}
	sort.Slice(out.Countries, func(i, j int) bool { return out.Countries[i].Count > out.Countries[j].Count })
	var ss []struct {
		Status string
		N      int
	}
	s.DB.Model(&Opportunity{}).Select("status, COUNT(*) AS n").Where("organization_id = ?", org).Group("status").Scan(&ss)
	for _, x := range ss {
		out.Statuses[x.Status] = x.N
	}
	out.Currency = orgprefs.Get(s.DB, org).Currency
	return out, nil
}

// Get returns one card.
func (s *Service) Get(org, id string) (*Opportunity, error) {
	var o Opportunity
	if err := s.DB.Where("organization_id = ? AND id = ?", org, id).First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &o, nil
}

// ItemPatch changes a card.
type ItemPatch struct {
	Saved   *bool   `json:"saved"`
	Notes   *string `json:"notes"`
	Restore bool    `json:"restore"` // back to the feed (from dismissed / expired)
}

// Update saves / unsaves, edits notes or restores a card.
func (s *Service) Update(org, id string, p ItemPatch) (*Opportunity, error) {
	o, err := s.Get(org, id)
	if err != nil {
		return nil, err
	}
	up := map[string]any{}
	if p.Notes != nil {
		up["notes"] = clip(strings.TrimSpace(*p.Notes), 4000)
	}
	if p.Saved != nil && (o.Status == StatusNew || o.Status == StatusSaved) {
		up["status"] = map[bool]string{true: StatusSaved, false: StatusNew}[*p.Saved]
	}
	if p.Restore && (o.Status == StatusDismissed || o.Status == StatusExpired) {
		up["status"], up["dismiss_reason"], up["dismiss_note"], up["dismissed_at"] = StatusNew, "", "", nil
	}
	if len(up) > 0 {
		if err := s.DB.Model(&Opportunity{}).Where("id = ?", id).Updates(up).Error; err != nil {
			return nil, err
		}
	}
	s.expire(org)
	return s.Get(org, id)
}

// Dismiss hides a card with a reason (fed back to later scans).
func (s *Service) Dismiss(org, id, by, reason, note string) (*Opportunity, error) {
	if _, err := s.Get(org, id); err != nil {
		return nil, err
	}
	ok := false
	for _, r := range DismissReasons {
		if r.Key == reason {
			ok = true
		}
	}
	if !ok {
		return nil, bad("Choose why it is not a fit.")
	}
	now := s.now()
	if err := s.DB.Model(&Opportunity{}).Where("id = ?", id).Updates(map[string]any{"status": StatusDismissed, "dismiss_reason": reason,
		"dismiss_note": clip(cleanText(note), 300), "dismissed_at": now, "dismissed_by": by}).Error; err != nil {
		return nil, err
	}
	return s.Get(org, id)
}

// Remind sets (or clears with nil) a reminder for the user on a date.
func (s *Service) Remind(org, id, user string, on *time.Time) (*Opportunity, error) {
	o, err := s.Get(org, id)
	if err != nil {
		return nil, err
	}
	up := map[string]any{"reminded_at": nil}
	if on == nil {
		up["remind_on"], up["remind_user_id"] = nil, ""
	} else {
		d := time.Date(on.Year(), on.Month(), on.Day(), 0, 0, 0, 0, time.UTC)
		today := localDay(s.now(), s.Loc(org))
		if d.Before(today) {
			return nil, bad("Choose today or a later date.")
		}
		if d.After(today.AddDate(1, 0, 0)) {
			return nil, bad("Choose a date within the next year.")
		}
		up["remind_on"], up["remind_user_id"] = d, user
		if o.Status == StatusNew {
			up["status"] = StatusSaved // a card with a reminder is kept
		}
	}
	if err := s.DB.Model(&Opportunity{}).Where("id = ?", id).Updates(up).Error; err != nil {
		return nil, err
	}
	return s.Get(org, id)
}

// ---------------------------------------------------------------- markets

// MarketGroup is the signals of one country.
type MarketGroup struct {
	Country string   `json:"country"`
	Name    string   `json:"name"`
	Sectors []string `json:"sectors"`
	Signals []Signal `json:"signals"`
}

// Markets returns the market signals by country (and sector), newest first.
func (s *Service) Markets(org string, days int) ([]MarketGroup, error) {
	if days <= 0 || days > 365 {
		days = 90
	}
	var sigs []Signal
	if err := s.DB.Where("organization_id = ? AND NOT hidden AND last_seen_at >= ?", org, s.now().AddDate(0, 0, -days)).Order("last_seen_at DESC").Limit(300).Find(&sigs).Error; err != nil {
		return nil, err
	}
	by := map[string]*MarketGroup{}
	order := []string{}
	for _, sg := range sigs {
		k := sg.Country
		g := by[k]
		if g == nil {
			name := countryLabel(k)
			if k == "" {
				name = "Several countries / global"
			}
			g = &MarketGroup{Country: k, Name: name, Sectors: []string{}, Signals: []Signal{}}
			by[k] = g
			order = append(order, k)
		}
		g.Signals = append(g.Signals, sg)
		if sg.Sector != "" {
			found := false
			for _, x := range g.Sectors {
				if strings.EqualFold(x, sg.Sector) {
					found = true
				}
			}
			if !found {
				g.Sectors = append(g.Sectors, sg.Sector)
			}
		}
	}
	out := []MarketGroup{}
	for _, k := range order {
		out = append(out, *by[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Country == "") != (out[j].Country == "") {
			return out[j].Country == ""
		}
		return len(out[i].Signals) > len(out[j].Signals)
	})
	return out, nil
}

// HideSignal hides a market signal.
func (s *Service) HideSignal(org, id string) error {
	res := s.DB.Model(&Signal{}).Where("organization_id = ? AND id = ?", org, id).Update("hidden", true)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// RunList is the run history with this month's spend.
type RunList struct {
	Runs   []Run   `json:"runs"`
	Month  string  `json:"month"`
	Spent  float64 `json:"spent_usd"`
	Budget float64 `json:"budget_usd"`
	Totals struct {
		Runs     int     `json:"runs"`
		Searches int     `json:"searches"`
		Input    int     `json:"input_tokens"`
		Output   int     `json:"output_tokens"`
		Cost     float64 `json:"cost_usd"`
	} `json:"month_totals"`
	Rates  []Rate `json:"rates"`
	Source string `json:"pricing_source"`
}

// Runs lists recent runs (without prompts).
func (s *Service) Runs(org string, limit int) (*RunList, error) {
	st, err := s.Settings(org)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := &RunList{Runs: []Run{}, Budget: st.MonthlyBudgetUSD, Rates: Rates, Source: PricingSource}
	if err := s.DB.Omit("prompt", "answer_text").Where("organization_id = ?", org).Order("started_at DESC").Limit(limit).Find(&out.Runs).Error; err != nil {
		return nil, err
	}
	loc := s.Loc(org)
	out.Spent, out.Month = s.MonthSpend(org, loc)
	start, end := monthBounds(s.now(), loc)
	s.DB.Model(&Run{}).Where("organization_id = ? AND started_at >= ? AND started_at < ?", org, start, end).
		Select("COUNT(*) AS runs, COALESCE(SUM(searches),0) AS searches, COALESCE(SUM(input_tokens),0) AS input, COALESCE(SUM(output_tokens),0) AS output, COALESCE(SUM(cost_usd),0) AS cost").
		Row().Scan(&out.Totals.Runs, &out.Totals.Searches, &out.Totals.Input, &out.Totals.Output, &out.Totals.Cost)
	out.Totals.Cost = round4(out.Totals.Cost)
	return out, nil
}

func countryLabel(code string) string {
	if code == "" {
		return ""
	}
	if n := countryName(code); n != "" {
		return n
	}
	return code
}
