package football

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Priorities of planned fetches: the quota manager keeps a reserve so that
// low-priority work (backfills, weekly player stats) can never use up the
// budget needed for results, live scores and closing odds.
const (
	PrioCritical = 0 // results to grade, live scores, closing odds
	PrioHigh     = 1 // today's fixtures, odds near kickoff, line-ups
	PrioNormal   = 2 // next days' fixtures, standings, injuries, earlier odds
	PrioLow      = 3 // head-to-head, player stats, match details for history
	PrioBackfill = 4 // previous seasons, reference predictions
)

// prioShare is the share of the budget a priority may use.
var prioShare = []float64{1.0, 0.97, 0.9, 0.8, 0.7}

// ProviderError is a failed provider call.
type ProviderError struct {
	Provider string
	Kind     string // no_key | bad_key | quota | rate_limited | plan | network | server | bad_request | backoff | budget
	Status   int
	Msg      string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s: %s (%s)", providerName(e.Provider), e.Msg, e.Kind)
}

// IsKind reports whether err is a ProviderError of kind k.
func IsKind(err error, k string) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.Kind == k
}

func providerName(p string) string {
	switch p {
	case ProviderAPIFootball:
		return "API-Football"
	case ProviderOddsAPI:
		return "The Odds API"
	}
	if n, ok := freeProviderNames[p]; ok {
		return n
	}
	return p
}

// callSpec is one provider request.
type callSpec struct {
	Provider string
	Endpoint string // API-Football: "fixtures", "odds/live"…; The Odds API: "/v4/sports/soccer_epl/odds"
	Params   url.Values
	Cost     int // expected cost (API-Football 1, The Odds API markets × regions, 0 = free)
	Prio     int
	Task     string
	Free     bool // does not count (API-Football /status, The Odds API /sports)
}

// callOut is a successful call.
type callOut struct {
	Status    int
	Body      []byte
	Cost      int
	Results   int
	Remaining *int
	Envelope  *afEnvelope // API-Football
	Header    http.Header
}

func dayPeriod(t time.Time) string   { return "day:" + t.UTC().Format("2006-01-02") }
func monthPeriod(t time.Time) string { return "month:" + t.UTC().Format("2006-01") }

// Usage is a provider's usage for the settings page.
type Usage struct {
	Provider       string     `json:"provider"`
	Name           string     `json:"name"`
	HasKey         bool       `json:"has_key"`
	KeyHint        string     `json:"key_hint"`
	Period         string     `json:"period"` // day or month
	Budget         int        `json:"budget"` // what DASYIN allows itself
	Used           int        `json:"used"`   // counted by DASYIN this period
	Requests       int        `json:"requests"`
	ReportedLimit  *int       `json:"reported_limit"` // what the provider said
	ReportedRemain *int       `json:"reported_remaining"`
	ReportedUsed   *int       `json:"reported_used"`
	Today          int        `json:"today"`           // used today (both providers)
	DailyAllowance int        `json:"daily_allowance"` // The Odds API: pace for the rest of the month
	BackoffUntil   *time.Time `json:"backoff_until"`
	LastError      string     `json:"last_error"`
	History        []DayUse   `json:"history"`
}

// DayUse is one day of usage.
type DayUse struct {
	Day      string `json:"day"`
	Cost     int    `json:"cost"`
	Requests int    `json:"requests"`
	Errors   int    `json:"errors"`
}

func (s *Service) quotaRow(provider, period string) Quota {
	var q Quota
	s.DB.Where("provider = ? AND period = ?", provider, period).Limit(1).Find(&q)
	q.Provider, q.Period = provider, period
	return q
}

// UsageFor returns a provider's usage now.
func (s *Service) UsageFor(provider string) Usage {
	st, _ := s.Settings()
	af, oa := s.Keys()
	now := s.Now()
	u := Usage{Provider: provider, Name: providerName(provider), History: []DayUse{}}
	day := s.quotaRow(provider, dayPeriod(now))
	u.Today = day.Used
	switch provider {
	case ProviderAPIFootball:
		u.HasKey, u.KeyHint, u.Period, u.Budget = af != "", mask(af), "day", st.APIFootballDaily
		u.Used, u.Requests = day.Used, day.Requests
		u.ReportedLimit, u.ReportedRemain, u.ReportedUsed = day.ReportedLimit, day.ReportedRemain, day.ReportedUsed
		u.BackoffUntil, u.LastError = day.BackoffUntil, day.LastError
		u.DailyAllowance = st.APIFootballDaily
	case ProviderOddsAPI:
		mo := s.quotaRow(provider, monthPeriod(now))
		u.HasKey, u.KeyHint, u.Period, u.Budget = oa != "", mask(oa), "month", st.OddsAPIMonthly
		u.Used, u.Requests = mo.Used, mo.Requests
		u.ReportedLimit, u.ReportedRemain, u.ReportedUsed = mo.ReportedLimit, mo.ReportedRemain, mo.ReportedUsed
		u.BackoffUntil, u.LastError = mo.BackoffUntil, mo.LastError
		if day.BackoffUntil != nil && (u.BackoffUntil == nil || day.BackoffUntil.After(*u.BackoffUntil)) {
			u.BackoffUntil = day.BackoffUntil
		}
		u.DailyAllowance = oddsDailyAllowance(st.OddsAPIMonthly, mo.Used, now)
	}
	if u.BackoffUntil != nil && !u.BackoffUntil.After(now) {
		u.BackoffUntil = nil
	}
	var rows []struct {
		Day      string
		Cost     int
		Requests int
		Errors   int
	}
	s.DB.Raw(`SELECT to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, COALESCE(SUM(cost),0) AS cost, COUNT(*) AS requests,
		SUM(CASE WHEN ok THEN 0 ELSE 1 END) AS errors FROM fb_fetch_log WHERE provider = ? AND at >= ? GROUP BY 1 ORDER BY 1`,
		provider, now.AddDate(0, 0, -13).Truncate(24*time.Hour)).Scan(&rows)
	for _, r := range rows {
		u.History = append(u.History, DayUse{Day: r.Day, Cost: r.Cost, Requests: r.Requests, Errors: r.Errors})
	}
	return u
}

// oddsDailyAllowance paces The Odds API credits over the rest of the month.
func oddsDailyAllowance(budget, usedMonth int, now time.Time) int {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	daysLeft := int(first.AddDate(0, 1, 0).Sub(now.Truncate(24*time.Hour)).Hours()/24 + 0.5)
	if daysLeft < 1 {
		daysLeft = 1
	}
	left := budget - usedMonth
	if left <= 0 {
		return 0
	}
	return int(math.Ceil(float64(left) / float64(daysLeft)))
}

// Allow decides whether a call of cost at priority prio fits the budget.
func (s *Service) Allow(provider string, cost, prio int) (bool, string) {
	st, _ := s.Settings()
	now := s.Now()
	if prio < 0 {
		prio = 0
	}
	if prio >= len(prioShare) {
		prio = len(prioShare) - 1
	}
	day := s.quotaRow(provider, dayPeriod(now))
	if day.BackoffUntil != nil && day.BackoffUntil.After(now) {
		return false, "backing off until " + day.BackoffUntil.Format(time.RFC3339)
	}
	if isFreeProvider(provider) {
		return s.allowFree(provider, cost, prio, day)
	}
	switch provider {
	case ProviderAPIFootball:
		limit := int(float64(st.APIFootballDaily) * prioShare[prio])
		if day.Used+cost > limit {
			return false, fmt.Sprintf("daily budget: %d of %d used (priority %d may use %d)", day.Used, st.APIFootballDaily, prio, limit)
		}
		if day.ReportedRemain != nil && *day.ReportedRemain < cost {
			return false, "API-Football says no requests are left today"
		}
	case ProviderOddsAPI:
		mo := s.quotaRow(provider, monthPeriod(now))
		if mo.BackoffUntil != nil && mo.BackoffUntil.After(now) {
			return false, "backing off until " + mo.BackoffUntil.Format(time.RFC3339)
		}
		limit := int(float64(st.OddsAPIMonthly) * prioShare[prio])
		if mo.Used+cost > limit {
			return false, fmt.Sprintf("monthly budget: %d of %d credits used", mo.Used, st.OddsAPIMonthly)
		}
		if mo.ReportedRemain != nil && *mo.ReportedRemain < cost {
			return false, "The Odds API says not enough credits are left"
		}
		// Earlier snapshots are paced over the month; closing odds may use the reserve.
		if prio >= PrioNormal {
			if allow := oddsDailyAllowance(st.OddsAPIMonthly, mo.Used, now); day.Used+cost > allow {
				return false, fmt.Sprintf("daily pace: %d of %d credits used today", day.Used, allow)
			}
		}
	}
	return true, ""
}

func headerInt(h http.Header, keys ...string) *int {
	for _, k := range keys {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				n := int(f)
				return &n
			}
		}
	}
	return nil
}

func redact(v url.Values) string {
	c := url.Values{}
	for k, vs := range v {
		if strings.EqualFold(k, "apiKey") {
			continue
		}
		c[k] = vs
	}
	return c.Encode()
}

// call performs one provider request, logs it with its cost and keeps the
// quota rows and back-off up to date.
func (s *Service) call(ctx context.Context, sp callSpec) (*callOut, error) {
	af, oa := s.Keys()
	now := s.Now()
	key := af
	if sp.Provider == ProviderOddsAPI {
		key = oa
	}
	if key == "" {
		return nil, &ProviderError{Provider: sp.Provider, Kind: "no_key", Msg: "no API key saved"}
	}
	if !sp.Free {
		if ok, why := s.Allow(sp.Provider, sp.Cost, sp.Prio); !ok {
			kind := "budget"
			if strings.HasPrefix(why, "backing off") {
				kind = "backoff"
			}
			return nil, &ProviderError{Provider: sp.Provider, Kind: kind, Msg: why}
		}
	}
	// Per-minute limit (X-RateLimit-Remaining): wait for the next minute
	// rather than be blocked by the provider's firewall.
	if sp.Provider == ProviderAPIFootball {
		if until := s.minuteBlock.Load(); until > 0 {
			if d := time.Until(time.Unix(0, until)); d > 0 && d <= 61*time.Second {
				s.Sleep(d)
			}
		}
	}
	var u string
	params := url.Values{}
	for k, v := range sp.Params {
		params[k] = v
	}
	if sp.Provider == ProviderOddsAPI {
		params.Set("apiKey", key)
		u = s.OABase + sp.Endpoint
	} else {
		u = s.AFBase + "/" + strings.TrimLeft(sp.Endpoint, "/")
	}
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &ProviderError{Provider: sp.Provider, Kind: "bad_request", Msg: "bad address"}
	}
	if sp.Provider == ProviderAPIFootball {
		req.Header.Set("x-apisports-key", key)
	}
	started := time.Now()
	resp, err := s.HTTP.Do(req)
	lg := FetchLog{Provider: sp.Provider, Endpoint: strings.TrimPrefix(sp.Endpoint, "/"), Params: redact(sp.Params), Task: clip(sp.Task, 120), At: now}
	if err != nil {
		lg.DurationMS, lg.Error = time.Since(started).Milliseconds(), "network: "+clip(strings.ReplaceAll(err.Error(), key, "***"), 300)
		s.record(lg, nil, sp, 0)
		return nil, &ProviderError{Provider: sp.Provider, Kind: "network", Msg: "could not reach the provider"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	lg.Status, lg.DurationMS = resp.StatusCode, time.Since(started).Milliseconds()
	out := &callOut{Status: resp.StatusCode, Body: body, Header: resp.Header, Cost: sp.Cost}
	if sp.Free {
		out.Cost = 0
	}
	var perr *ProviderError
	switch sp.Provider {
	case ProviderAPIFootball:
		out.Remaining = headerInt(resp.Header, "x-ratelimit-requests-remaining")
		if pm := headerInt(resp.Header, "X-RateLimit-Remaining"); pm != nil && *pm <= 1 {
			s.minuteBlock.Store(time.Now().Truncate(time.Minute).Add(time.Minute + time.Second).UnixNano())
		} else {
			s.minuteBlock.Store(0)
		}
		var env afEnvelope
		jerr := json.Unmarshal(body, &env)
		errs := afErrors(env.Errors)
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			kind, msg := "rate_limited", "too many requests"
			if m := firstOf(errs, "requests"); m != "" || (out.Remaining != nil && *out.Remaining <= 0) {
				kind, msg = "quota", "daily request limit reached"
			} else if m := firstOf(errs, "rateLimit", "ratelimit"); m != "" {
				msg = m
			}
			perr = &ProviderError{Provider: sp.Provider, Kind: kind, Status: resp.StatusCode, Msg: msg}
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			perr = &ProviderError{Provider: sp.Provider, Kind: "bad_key", Status: resp.StatusCode, Msg: "the API key was refused"}
		case resp.StatusCode >= 500:
			perr = &ProviderError{Provider: sp.Provider, Kind: "server", Status: resp.StatusCode, Msg: "provider error"}
		case resp.StatusCode >= 400:
			perr = &ProviderError{Provider: sp.Provider, Kind: "bad_request", Status: resp.StatusCode, Msg: http.StatusText(resp.StatusCode)}
		case jerr != nil:
			perr = &ProviderError{Provider: sp.Provider, Kind: "server", Status: resp.StatusCode, Msg: "unreadable response"}
		case len(errs) > 0:
			switch {
			case firstOf(errs, "token", "access") != "":
				perr = &ProviderError{Provider: sp.Provider, Kind: "bad_key", Status: resp.StatusCode, Msg: firstOf(errs, "token", "access")}
			case firstOf(errs, "requests") != "":
				perr = &ProviderError{Provider: sp.Provider, Kind: "quota", Status: resp.StatusCode, Msg: firstOf(errs, "requests")}
			case firstOf(errs, "rateLimit", "ratelimit") != "":
				perr = &ProviderError{Provider: sp.Provider, Kind: "rate_limited", Status: resp.StatusCode, Msg: firstOf(errs, "rateLimit", "ratelimit")}
			case firstOf(errs, "plan") != "":
				perr = &ProviderError{Provider: sp.Provider, Kind: "plan", Status: resp.StatusCode, Msg: firstOf(errs, "plan")}
			default:
				for k, v := range errs {
					perr = &ProviderError{Provider: sp.Provider, Kind: "bad_request", Status: resp.StatusCode, Msg: k + ": " + v}
					break
				}
			}
		default:
			out.Envelope, out.Results = &env, env.Results
		}
	case ProviderOddsAPI:
		out.Remaining = headerInt(resp.Header, "x-requests-remaining")
		if last := headerInt(resp.Header, "x-requests-last"); last != nil && !sp.Free {
			out.Cost = *last
		}
		var e struct {
			Message   string `json:"message"`
			ErrorCode string `json:"error_code"`
		}
		if resp.StatusCode >= 400 {
			_ = json.Unmarshal(body, &e)
		}
		switch {
		case e.ErrorCode == "OUT_OF_USAGE_CREDITS" || (resp.StatusCode == 429 && out.Remaining != nil && *out.Remaining <= 0):
			perr = &ProviderError{Provider: sp.Provider, Kind: "quota", Status: resp.StatusCode, Msg: "usage credits used up"}
		case resp.StatusCode == http.StatusTooManyRequests:
			perr = &ProviderError{Provider: sp.Provider, Kind: "rate_limited", Status: resp.StatusCode, Msg: "too many requests"}
		case resp.StatusCode == 401 || resp.StatusCode == 403 || e.ErrorCode == "INVALID_KEY" || e.ErrorCode == "MISSING_KEY" || e.ErrorCode == "DEACTIVATED_KEY":
			perr = &ProviderError{Provider: sp.Provider, Kind: "bad_key", Status: resp.StatusCode, Msg: "the API key was refused"}
		case resp.StatusCode >= 500:
			perr = &ProviderError{Provider: sp.Provider, Kind: "server", Status: resp.StatusCode, Msg: "provider error"}
		case resp.StatusCode >= 400:
			msg := e.Message
			if msg == "" {
				msg = http.StatusText(resp.StatusCode)
			}
			perr = &ProviderError{Provider: sp.Provider, Kind: "bad_request", Status: resp.StatusCode, Msg: clip(msg, 200)}
		default:
			var arr []json.RawMessage
			if json.Unmarshal(body, &arr) == nil {
				out.Results = len(arr)
			}
		}
	}
	lg.Remaining, lg.Results, lg.Cost = out.Remaining, out.Results, out.Cost
	if perr != nil {
		lg.Error = perr.Kind + ": " + clip(perr.Msg, 300)
		// The provider still counts failed API-Football calls against the
		// day, so they cost what they would have.
		if sp.Provider == ProviderOddsAPI && headerInt(resp.Header, "x-requests-last") == nil {
			lg.Cost = 0
		}
	} else {
		lg.OK = true
	}
	s.record(lg, resp.Header, sp, out.Cost)
	if perr != nil {
		s.backoff(sp.Provider, perr)
		return nil, perr
	}
	return out, nil
}

func firstOf(m map[string]string, keys ...string) string {
	for k, v := range m {
		for _, want := range keys {
			if strings.EqualFold(k, want) && v != "" {
				return v
			}
		}
	}
	return ""
}

// record writes the fetch log and the period usage.
func (s *Service) record(lg FetchLog, h http.Header, sp callSpec, cost int) {
	if err := s.DB.Create(&lg).Error; err != nil {
		return
	}
	now := s.Now()
	periods := []string{dayPeriod(now)}
	if sp.Provider == ProviderOddsAPI || monthlyFree(sp.Provider) {
		periods = append(periods, monthPeriod(now))
	}
	for _, p := range periods {
		row := Quota{Provider: sp.Provider, Period: p, UpdatedAt: now}
		s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		up := map[string]any{"used": gorm.Expr("used + ?", lg.Cost), "requests": gorm.Expr("requests + 1"), "updated_at": now}
		if h != nil {
			switch sp.Provider {
			case ProviderAPIFootball:
				if v := headerInt(h, "x-ratelimit-requests-remaining"); v != nil {
					up["reported_remain"] = *v
				}
				if v := headerInt(h, "x-ratelimit-requests-limit"); v != nil {
					up["reported_limit"] = *v
				}
			case ProviderOddsAPI:
				if v := headerInt(h, "x-requests-remaining"); v != nil {
					up["reported_remain"] = *v
				}
				if v := headerInt(h, "x-requests-used"); v != nil {
					up["reported_used"] = *v
				}
			}
		}
		if lg.OK {
			up["failures"], up["last_error"], up["backoff_until"] = 0, "", nil
		}
		s.DB.Model(&Quota{}).Where("provider = ? AND period = ?", sp.Provider, p).Updates(up)
	}
}

// backoff pauses a provider after a failure: exponentially for errors and
// rate limits (1, 2, 4 … 60 minutes), until the quota resets when it is used up.
func (s *Service) backoff(provider string, e *ProviderError) {
	now := s.Now()
	var until time.Time
	period := dayPeriod(now)
	switch e.Kind {
	case "quota":
		if provider == ProviderOddsAPI || monthlyFree(provider) {
			first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			until, period = first.AddDate(0, 1, 0), monthPeriod(now)
		} else {
			until = now.Truncate(24 * time.Hour).Add(24 * time.Hour)
		}
	case "network", "server", "rate_limited":
		q := s.quotaRow(provider, period)
		mins := math.Min(60, math.Pow(2, float64(q.Failures)))
		until = now.Add(time.Duration(mins) * time.Minute)
	case "bad_key":
		until = now.Add(30 * time.Minute)
	default:
		s.DB.Model(&Quota{}).Where("provider = ? AND period = ?", provider, period).Updates(map[string]any{"last_error": clip(e.Error(), 300)})
		return
	}
	row := Quota{Provider: provider, Period: period, UpdatedAt: now}
	s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	s.DB.Model(&Quota{}).Where("provider = ? AND period = ?", provider, period).Updates(map[string]any{
		"failures": gorm.Expr("failures + 1"), "backoff_until": until, "last_error": clip(e.Error(), 300), "updated_at": now})
}

// ClearBackoff lifts a provider's back-off (after a key change or a
// successful connection test).
func (s *Service) ClearBackoff(provider string) {
	s.DB.Model(&Quota{}).Where("provider = ?", provider).Updates(map[string]any{"backoff_until": nil, "failures": 0})
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// ---------------------------------------------------------------- tests of keys

// TestResult is a connection test.
type TestResult struct {
	Provider   string `json:"provider"`
	OK         bool   `json:"ok"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Plan       string `json:"plan,omitempty"`
	LimitDay   int    `json:"limit_day,omitempty"`
	UsedToday  int    `json:"used_today,omitempty"`
	Remaining  *int   `json:"remaining,omitempty"`
	Used       *int   `json:"used,omitempty"`
	Sports     int    `json:"soccer_competitions,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// TestConnection checks a provider key with a free call (API-Football
// /status, The Odds API /v4/sports — neither counts towards the quota).
func (s *Service) TestConnection(ctx context.Context, provider string) TestResult {
	started := time.Now()
	res := TestResult{Provider: provider}
	done := func(code, msg string) TestResult {
		res.Code, res.Message, res.DurationMS = code, msg, time.Since(started).Milliseconds()
		res.OK = code == "ok"
		return res
	}
	s.ClearBackoff(provider)
	switch provider {
	case ProviderAPIFootball:
		out, err := s.call(ctx, callSpec{Provider: provider, Endpoint: "status", Free: true, Task: "test connection"})
		if err != nil {
			return done(testCode(err), testMessage(provider, err))
		}
		var st afStatus
		// /status returns an object (some accounts return [] when the key is unknown).
		if json.Unmarshal(out.Envelope.Response, &st) != nil || st.Subscription.Plan == "" {
			return done("bad_key", "API-Football answered but did not recognise the key.")
		}
		res.Plan, res.LimitDay, res.UsedToday = st.Subscription.Plan, st.Requests.LimitDay, st.Requests.Current
		if !st.Subscription.Active {
			return done("inactive", "The API-Football subscription ("+st.Subscription.Plan+") is not active.")
		}
		return done("ok", fmt.Sprintf("Connected — plan %s, %d of %d requests used today.", st.Subscription.Plan, st.Requests.Current, st.Requests.LimitDay))
	case ProviderOddsAPI:
		out, err := s.call(ctx, callSpec{Provider: provider, Endpoint: "/v4/sports", Free: true, Task: "test connection"})
		if err != nil {
			return done(testCode(err), testMessage(provider, err))
		}
		var sports []OASport
		_ = json.Unmarshal(out.Body, &sports)
		for _, sp := range sports {
			if strings.HasPrefix(sp.Key, "soccer_") {
				res.Sports++
			}
		}
		res.Remaining, res.Used = out.Remaining, headerInt(out.Header, "x-requests-used")
		msg := fmt.Sprintf("Connected — %d soccer competitions in season.", res.Sports)
		if res.Remaining != nil {
			msg += fmt.Sprintf(" %d credits left this month.", *res.Remaining)
		}
		return done("ok", msg)
	}
	return done("error", "Unknown provider.")
}

func testCode(err error) string {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return "error"
}

func testMessage(provider string, err error) string {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return "The test failed."
	}
	n := providerName(provider)
	switch pe.Kind {
	case "no_key":
		return "No " + n + " key saved yet. Paste the key and save first."
	case "bad_key":
		return n + " refused the key. Check it in the provider's dashboard and paste it again."
	case "quota":
		return n + " says the quota is used up. It resets at the start of the next day (API-Football) or month (The Odds API)."
	case "rate_limited":
		return n + " asked us to slow down. Try again in a minute."
	case "network":
		return "Could not reach " + n + ". Check the server's internet connection."
	case "server":
		return n + " had a problem answering. Try again later."
	}
	return n + ": " + pe.Msg
}
