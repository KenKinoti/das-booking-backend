package football

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FreeBases are the free sources' base URLs (production defaults, env
// overrides for tests).
type FreeBases struct {
	OddsPapi, ClubElo, FDCOUK, FDOrg, GNews, GDELT string

	mu       sync.Mutex
	lastCall map[string]time.Time // per provider, for minimum spacing
	// NoSpacing turns the polite spacing between calls off
	// (FOOTBALL_FREE_NOSPACING=1, test environments only).
	NoSpacing bool
}

// Production endpoints of the free sources.
const (
	DefaultOddsPapiBase = "https://api.oddspapi.io"         // https://oddspapi.io/en/docs ("All requests use the host https://api.oddspapi.io")
	DefaultClubEloBase  = "http://api.clubelo.com"          // http://clubelo.com/API
	DefaultFDCOUKBase   = "https://www.football-data.co.uk" // https://www.football-data.co.uk/data.php
	DefaultFDOrgBase    = "https://api.football-data.org"   // https://docs.football-data.org/general/v4/index.html
	DefaultGNewsBase    = "https://news.google.com"         // RSS search: /rss/search?q=…&hl=…&gl=…&ceid=…
	DefaultGDELTBase    = "https://api.gdeltproject.org"    // DOC 2.0: /api/v2/doc/doc?query=…&mode=ArtList&format=json
)

// Docs shown in sources and settings.
const (
	docOddsPapi = "https://oddspapi.io/en/docs"
	docClubElo  = "http://clubelo.com/API"
	docFDCOUK   = "https://www.football-data.co.uk/notes.txt"
	docFDOrg    = "https://docs.football-data.org/general/v4/index.html"
	docGNews    = "https://news.google.com/rss"
	docGDELT    = "https://blog.gdeltproject.org/gdelt-doc-2-0-api-debuts/"
)

func freeBasesFromEnv() *FreeBases {
	tr := func(k, d string) string { return strings.TrimRight(envOr(k, d), "/") }
	return &FreeBases{OddsPapi: tr("FOOTBALL_ODDSPAPI_BASE", DefaultOddsPapiBase), ClubElo: tr("FOOTBALL_CLUBELO_BASE", DefaultClubEloBase),
		FDCOUK: tr("FOOTBALL_FDCOUK_BASE", DefaultFDCOUKBase), FDOrg: tr("FOOTBALL_FDORG_BASE", DefaultFDOrgBase),
		GNews: tr("FOOTBALL_GNEWS_BASE", DefaultGNewsBase), GDELT: tr("FOOTBALL_GDELT_BASE", DefaultGDELTBase), lastCall: map[string]time.Time{},
		NoSpacing: strings.TrimSpace(envOr("FOOTBALL_FREE_NOSPACING", "")) == "1"}
}

func (s *Service) bases() *FreeBases {
	if s.Free == nil {
		s.Free = freeBasesFromEnv()
	}
	return s.Free
}

var freeProviderNames = map[string]string{ProviderOddsPapi: "OddsPapi", ProviderClubElo: "ClubElo", ProviderFDCOUK: "football-data.co.uk",
	ProviderFDOrg: "football-data.org", ProviderGNews: "Google News", ProviderGDELT: "GDELT", ProviderFeeds: "RSS / Atom feeds"}

func isFreeProvider(p string) bool { _, ok := freeProviderNames[p]; return ok }

// monthlyFree: OddsPapi's free tier is counted per month (250 requests).
func monthlyFree(p string) bool { return p == ProviderOddsPapi }

// minSpacing between two calls of a provider: football-data.org allows 10
// calls a minute on the free tier; GDELT asks for one request every few
// seconds; ClubElo / football-data.co.uk are small sites.
var minSpacing = map[string]time.Duration{ProviderFDOrg: 6500 * time.Millisecond, ProviderGDELT: 5 * time.Second,
	ProviderClubElo: 2 * time.Second, ProviderFDCOUK: 2 * time.Second, ProviderOddsPapi: 2 * time.Second, ProviderGNews: 2 * time.Second}

// allowFree applies a free provider's budget: per day, except OddsPapi
// (per month, paced over the rest of the month for low priorities).
func (s *Service) allowFree(provider string, cost, prio int, day Quota) (bool, string) {
	st := s.FreeSettingsGet()
	budget := st.Budgets[provider]
	if budget <= 0 {
		budget = defaultFreeBudgets[provider]
	}
	limit := int(float64(budget) * prioShare[prio])
	if monthlyFree(provider) {
		mo := s.quotaRow(provider, monthPeriod(s.Now()))
		if mo.BackoffUntil != nil && mo.BackoffUntil.After(s.Now()) {
			return false, "backing off until " + mo.BackoffUntil.Format(time.RFC3339)
		}
		if mo.Used+cost > limit {
			return false, fmt.Sprintf("monthly budget: %d of %d requests used (priority %d may use %d)", mo.Used, budget, prio, limit)
		}
		if prio >= PrioNormal {
			if allow := oddsDailyAllowance(budget, mo.Used, s.Now()); day.Used+cost > allow {
				return false, fmt.Sprintf("daily pace: %d of %d requests used today", day.Used, allow)
			}
		}
		return true, ""
	}
	if day.Used+cost > limit {
		return false, fmt.Sprintf("daily budget: %d of %d requests used (priority %d may use %d)", day.Used, budget, prio, limit)
	}
	return true, ""
}

// freeSpec is one request to a free source.
type freeSpec struct {
	Provider string
	Path     string // appended to the provider's base (feeds: a full URL)
	Params   url.Values
	Prio     int
	Task     string
	Free     bool // not counted (connection tests that cost nothing)
	Accept   string
}

func (s *Service) baseOf(p string) string {
	b := s.bases()
	switch p {
	case ProviderOddsPapi:
		return b.OddsPapi
	case ProviderClubElo:
		return b.ClubElo
	case ProviderFDCOUK:
		return b.FDCOUK
	case ProviderFDOrg:
		return b.FDOrg
	case ProviderGNews:
		return b.GNews
	case ProviderGDELT:
		return b.GDELT
	}
	return ""
}

// userAgent is generic (no personal details are sent to third parties).
const userAgent = "Mozilla/5.0 (compatible; football-data-fetcher/1.0)"

// callFree performs one request to a free source: budget, spacing, logging
// (fb_fetch_log with cost 1, or 0 when free), usage rows and back-off — the
// same bookkeeping as the paid providers (quota.go).
func (s *Service) callFree(ctx context.Context, sp freeSpec) (*callOut, error) {
	op, fo := s.FreeKeys()
	key := ""
	switch sp.Provider {
	case ProviderOddsPapi:
		key = op
	case ProviderFDOrg:
		key = fo
	}
	if (sp.Provider == ProviderOddsPapi || sp.Provider == ProviderFDOrg) && key == "" {
		return nil, &ProviderError{Provider: sp.Provider, Kind: "no_key", Msg: "no API key saved"}
	}
	cost := 1
	if sp.Free {
		cost = 0
	}
	if !sp.Free {
		if ok, why := s.Allow(sp.Provider, cost, sp.Prio); !ok {
			kind := "budget"
			if strings.HasPrefix(why, "backing off") {
				kind = "backoff"
			}
			return nil, &ProviderError{Provider: sp.Provider, Kind: kind, Msg: why}
		}
	}
	// Spacing between calls.
	if d, ok := minSpacing[sp.Provider]; ok && !s.bases().NoSpacing {
		b := s.bases()
		b.mu.Lock()
		last := b.lastCall[sp.Provider]
		wait := time.Until(last.Add(d))
		b.lastCall[sp.Provider] = time.Now().Add(maxDur(wait, 0))
		b.mu.Unlock()
		if wait > 0 {
			s.Sleep(wait)
		}
	}
	params := url.Values{}
	for k, v := range sp.Params {
		params[k] = v
	}
	if sp.Provider == ProviderOddsPapi {
		params.Set("apiKey", key) // OddsPapi: "All endpoints require an API key to be passed as a request parameter."
	}
	u := sp.Path
	if sp.Provider != ProviderFeeds {
		u = s.baseOf(sp.Provider) + "/" + strings.TrimLeft(sp.Path, "/")
	}
	if len(params) > 0 {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + params.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now := s.Now()
	lg := FetchLog{Provider: sp.Provider, Endpoint: clip(endpointOf(sp), 60), Params: clip(redact(sp.Params), 2000), Task: clip(sp.Task, 120), At: now}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &ProviderError{Provider: sp.Provider, Kind: "bad_request", Msg: "bad address"}
	}
	req.Header.Set("User-Agent", userAgent)
	if sp.Accept != "" {
		req.Header.Set("Accept", sp.Accept)
	}
	if sp.Provider == ProviderFDOrg {
		req.Header.Set("X-Auth-Token", key) // football-data.org v4 authentication header
	}
	spec := callSpec{Provider: sp.Provider, Cost: cost, Prio: sp.Prio, Task: sp.Task, Free: sp.Free}
	started := time.Now()
	resp, err := s.HTTP.Do(req)
	if err != nil {
		msg := err.Error()
		if key != "" {
			msg = strings.ReplaceAll(msg, key, "***")
		}
		lg.DurationMS, lg.Error = time.Since(started).Milliseconds(), "network: "+clip(msg, 300)
		s.record(lg, nil, spec, 0)
		perr := &ProviderError{Provider: sp.Provider, Kind: "network", Msg: "could not reach the provider"}
		s.backoff(sp.Provider, perr)
		return nil, perr
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	lg.Status, lg.DurationMS = resp.StatusCode, time.Since(started).Milliseconds()
	out := &callOut{Status: resp.StatusCode, Body: body, Header: resp.Header, Cost: cost}
	var perr *ProviderError
	low := strings.ToLower(string(body[:min(len(body), 600)]))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		kind := "rate_limited"
		if sp.Provider == ProviderOddsPapi && (strings.Contains(low, "quota") || strings.Contains(low, "limit exceeded") || strings.Contains(low, "monthly")) {
			kind = "quota"
		}
		perr = &ProviderError{Provider: sp.Provider, Kind: kind, Status: resp.StatusCode, Msg: "too many requests"}
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		perr = &ProviderError{Provider: sp.Provider, Kind: "bad_key", Status: resp.StatusCode, Msg: "the request was refused (key or access)"}
	case resp.StatusCode == 404:
		perr = &ProviderError{Provider: sp.Provider, Kind: "not_found", Status: resp.StatusCode, Msg: "not found"}
	case resp.StatusCode >= 500:
		perr = &ProviderError{Provider: sp.Provider, Kind: "server", Status: resp.StatusCode, Msg: "provider error"}
	case resp.StatusCode >= 400:
		perr = &ProviderError{Provider: sp.Provider, Kind: "bad_request", Status: resp.StatusCode, Msg: clip(http.StatusText(resp.StatusCode)+": "+string(body[:min(len(body), 160)]), 200)}
	}
	if sp.Provider == ProviderFDOrg {
		out.Remaining = headerInt(resp.Header, "X-Requests-Available-Minute")
	}
	lg.Cost, lg.Remaining = cost, out.Remaining
	if perr != nil {
		lg.Error = perr.Kind + ": " + clip(perr.Msg, 300)
		s.record(lg, resp.Header, spec, cost)
		if perr.Kind != "not_found" {
			s.backoff(sp.Provider, perr)
		}
		return nil, perr
	}
	lg.OK = true
	s.record(lg, resp.Header, spec, cost)
	return out, nil
}

func endpointOf(sp freeSpec) string {
	if sp.Provider == ProviderFeeds {
		if u, err := url.Parse(sp.Path); err == nil {
			return u.Host
		}
	}
	p := sp.Path
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return strings.TrimPrefix(p, "/")
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// setResults stores the number of results on the latest log row of a task
// (parsers know it only after decoding).
func (s *Service) setResults(provider, task string, n int) {
	s.DB.Exec(`UPDATE fb_fetch_log SET results = ? WHERE id = (SELECT MAX(id) FROM fb_fetch_log WHERE provider = ? AND task = ?)`, n, provider, clip(task, 120))
}

// markLogError flags the latest log row of a task as failed when the
// answer arrived but couldn't be read (e.g. a feed that isn't a feed).
func (s *Service) markLogError(provider, task, msg string) {
	s.DB.Exec(`UPDATE fb_fetch_log SET ok = false, error = ? WHERE id = (SELECT MAX(id) FROM fb_fetch_log WHERE provider = ? AND task = ?)`, clip(msg, 300), provider, clip(task, 120))
}

// FreeUsage is a free provider's usage (the v2.16 Usage shape).
func (s *Service) FreeUsage(provider string) Usage {
	st := s.FreeSettingsGet()
	now := s.Now()
	op, fo := s.FreeKeys()
	u := Usage{Provider: provider, Name: providerName(provider), History: []DayUse{}, Period: "day", Budget: st.Budgets[provider]}
	switch provider {
	case ProviderOddsPapi:
		u.HasKey, u.KeyHint = op != "", mask(op)
	case ProviderFDOrg:
		u.HasKey, u.KeyHint = fo != "", mask(fo)
	}
	day := s.quotaRow(provider, dayPeriod(now))
	u.Today, u.Used, u.Requests, u.BackoffUntil, u.LastError = day.Used, day.Used, day.Requests, day.BackoffUntil, day.LastError
	u.DailyAllowance = u.Budget
	if monthlyFree(provider) {
		mo := s.quotaRow(provider, monthPeriod(now))
		u.Period, u.Used, u.Requests = "month", mo.Used, mo.Requests
		if mo.BackoffUntil != nil && (u.BackoffUntil == nil || mo.BackoffUntil.After(*u.BackoffUntil)) {
			u.BackoffUntil = mo.BackoffUntil
		}
		if mo.LastError != "" {
			u.LastError = mo.LastError
		}
		u.DailyAllowance = oddsDailyAllowance(u.Budget, mo.Used, now)
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

// gormExpr is gorm.Expr (SQL expression in updates).
func gormExpr(expr string, args ...any) clause.Expr { return gorm.Expr(expr, args...) }
