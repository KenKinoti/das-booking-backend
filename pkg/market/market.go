// Package market serves live market reference data (FX, crypto, precious
// metals and optionally equity indices) for the dashboard.
//
// Data is fetched server-side from free, reputable sources and cached in
// memory. When a source is unreachable the last good value is returned with
// stale=true; if there has never been a good value the section is returned
// with available=false. The endpoint never fails because a source is down.
//
// Adding a source: implement Provider and append it in NewServiceFromEnv.
package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Quote is one instrument.
type Quote struct {
	Symbol    string    `json:"symbol"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`  // fx | crypto | metal | index
	Price     float64   `json:"price"` // in Currency (for fx: units of Symbol per 1 base)
	Currency  string    `json:"currency"`
	Inverse   float64   `json:"inverse,omitempty"` // fx: base per 1 unit of Symbol
	Unit      string    `json:"unit,omitempty"`
	Change1D  *float64  `json:"change_1d,omitempty"` // percent
	Change7D  *float64  `json:"change_7d,omitempty"`
	Change30D *float64  `json:"change_30d,omitempty"`
	Spark     []float64 `json:"spark,omitempty"`
	SparkSpan string    `json:"spark_span,omitempty"` // e.g. "30d"
	Available bool      `json:"available"`
	Source    string    `json:"source"`
	SourceURL string    `json:"source_url,omitempty"`
	AsOf      string    `json:"as_of,omitempty"` // RFC3339 or YYYY-MM-DD as published by the source
	Note      string    `json:"note,omitempty"`
}

// Section groups quotes from one provider.
type Section struct {
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	Items     []Quote   `json:"items"`
	Available bool      `json:"available"`
	Stale     bool      `json:"stale"`
	Error     string    `json:"error,omitempty"`
	Sources   []Source  `json:"sources"`
	AsOf      string    `json:"as_of,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
	Note      string    `json:"note,omitempty"`
}

// Source names a data provider for attribution.
type Source struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Request is passed to providers.
type Request struct {
	Base   string
	Client *http.Client
	// FX holds units of currency X per 1 Base, from the FX section (may be empty).
	FX map[string]float64
}

// Provider fetches one section.
type Provider interface {
	Key() string
	Title() string
	TTL() time.Duration
	Enabled() bool
	Sources() []Source
	Fetch(ctx context.Context, req Request) (Section, error)
}

type cacheEntry struct {
	good      *Section
	goodAt    time.Time
	lastErr   string
	lastErrAt time.Time
}

// Service caches provider results.
type Service struct {
	Providers []Provider
	FX        Provider // fetched first; its rates feed the others
	Client    *http.Client
	// RetryAfter is the minimum wait before retrying a failed source.
	RetryAfter time.Duration
	Now        func() time.Time

	mu    sync.Mutex
	cache map[string]*cacheEntry
	locks map[string]*sync.Mutex
}

// NewService builds a service with the given providers.
func NewService(fx Provider, others ...Provider) *Service {
	return &Service{
		FX:         fx,
		Providers:  others,
		Client:     &http.Client{Timeout: 8 * time.Second},
		RetryAfter: 60 * time.Second,
		Now:        time.Now,
		cache:      map[string]*cacheEntry{},
		locks:      map[string]*sync.Mutex{},
	}
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// NewServiceFromEnv wires the default providers. Environment:
//
//	MARKET_TTL (default 15m), MARKET_FX_SYMBOLS (USD,EUR,GBP,KES,NZD,JPY,CNY),
//	MARKET_CRYPTO_IDS (bitcoin,ethereum), COINGECKO_API_KEY (optional demo key),
//	ALPHAVANTAGE_API_KEY (enables indices), MARKET_EQUITY_SYMBOLS
//	("SPY=S&P 500 · SPY ETF,STW.AX=ASX 200 · STW ETF"), MARKET_EQUITY_TTL (2h),
//	MARKET_DISABLE=1 (no outbound calls at all).
func NewServiceFromEnv() *Service {
	ttl := envDuration("MARKET_TTL", 15*time.Minute)
	fx := &FXProvider{
		FrankfurterURL: envOr("FRANKFURTER_URL", "https://api.frankfurter.app"),
		FallbackURL:    envOr("EXCHANGERATE_API_URL", "https://open.er-api.com/v6/latest"),
		Symbols:        splitList(envOr("MARKET_FX_SYMBOLS", "USD,EUR,GBP,KES,NZD,JPY,CNY")),
		HistoryDays:    30,
		CacheTTL:       ttl,
	}
	crypto := &CryptoProvider{
		BaseURL:  envOr("COINGECKO_URL", "https://api.coingecko.com/api/v3"),
		IDs:      splitList(strings.ToLower(envOr("MARKET_CRYPTO_IDS", "bitcoin,ethereum"))),
		APIKey:   os.Getenv("COINGECKO_API_KEY"),
		CacheTTL: ttl,
	}
	metals := &MetalsProvider{
		BaseURL:  envOr("GOLDAPI_URL", "https://api.gold-api.com"),
		Symbols:  []string{"XAU", "XAG"},
		CacheTTL: ttl,
	}
	indices := &AlphaVantageProvider{
		BaseURL:  envOr("ALPHAVANTAGE_URL", "https://www.alphavantage.co/query"),
		APIKey:   os.Getenv("ALPHAVANTAGE_API_KEY"),
		Symbols:  parseSymbols(envOr("MARKET_EQUITY_SYMBOLS", "SPY=S&P 500 · SPY ETF,STW.AX=ASX 200 · STW ETF")),
		CacheTTL: envDuration("MARKET_EQUITY_TTL", 2*time.Hour),
	}
	s := NewService(fx, crypto, metals, indices)
	if os.Getenv("MARKET_DISABLE") == "1" {
		s.Client = &http.Client{Transport: disabledTransport{}}
	}
	return s
}

type disabledTransport struct{}

func (disabledTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("outbound market data disabled (MARKET_DISABLE=1)")
}

func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Overview is the payload of GET /market/overview.
type Overview struct {
	Base        string    `json:"base"`
	GeneratedAt time.Time `json:"generated_at"`
	RefreshSecs int       `json:"refresh_seconds"`
	Sections    []Section `json:"sections"`
	Sources     []Source  `json:"sources"`
	Disabled    []string  `json:"disabled,omitempty"` // providers skipped (e.g. no API key)
}

// Overview returns all sections for a base currency. It never errors.
func (s *Service) Overview(ctx context.Context, base string) Overview {
	base = NormalizeCurrency(base, "AUD")
	out := Overview{Base: base, GeneratedAt: s.Now().UTC(), Sections: []Section{}, Sources: []Source{}}
	minTTL := time.Duration(0)
	fxRates := map[string]float64{}
	all := []Provider{}
	if s.FX != nil {
		all = append(all, s.FX)
	}
	all = append(all, s.Providers...)
	for _, p := range all {
		if p.Enabled() && (minTTL == 0 || p.TTL() < minTTL) {
			minTTL = p.TTL()
		}
	}
	if s.FX != nil && s.FX.Enabled() {
		sec := s.get(ctx, s.FX, Request{Base: base, Client: s.Client})
		for _, q := range sec.Items {
			if q.Available && q.Price > 0 {
				fxRates[q.Symbol] = q.Price
			}
		}
		fxRates[base] = 1
		out.Sections = append(out.Sections, sec)
	}
	results := make([]*Section, len(s.Providers))
	var wg sync.WaitGroup
	for i, p := range s.Providers {
		if !p.Enabled() {
			out.Disabled = append(out.Disabled, p.Key())
			continue
		}
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			sec := s.get(ctx, p, Request{Base: base, Client: s.Client, FX: fxRates})
			results[i] = &sec
		}(i, p)
	}
	wg.Wait()
	for _, r := range results {
		if r != nil {
			out.Sections = append(out.Sections, *r)
		}
	}
	seen := map[string]bool{}
	for _, sec := range out.Sections {
		for _, src := range sec.Sources {
			if !seen[src.Name] {
				seen[src.Name] = true
				out.Sources = append(out.Sources, src)
			}
		}
	}
	if minTTL == 0 {
		minTTL = 15 * time.Minute
	}
	out.RefreshSecs = int(minTTL.Seconds())
	return out
}

func (s *Service) lockFor(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	if s.cache == nil {
		s.cache = map[string]*cacheEntry{}
	}
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	return l
}

// get returns a cached section, refreshing it when expired. Concurrent
// callers for the same key share one fetch.
func (s *Service) get(ctx context.Context, p Provider, req Request) Section {
	key := p.Key() + ":" + req.Base
	l := s.lockFor(key)
	l.Lock()
	defer l.Unlock()

	s.mu.Lock()
	e := s.cache[key]
	if e == nil {
		e = &cacheEntry{}
		s.cache[key] = e
	}
	s.mu.Unlock()

	now := s.Now()
	if e.good != nil && now.Sub(e.goodAt) < p.TTL() {
		return *e.good
	}
	// Recently failed: don't hammer the source (or slow the dashboard).
	if e.lastErr != "" && now.Sub(e.lastErrAt) < s.RetryAfter && e.lastErrAt.After(e.goodAt) {
		return s.fallback(p, e)
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	sec, err := p.Fetch(fctx, req)
	if err == nil && !anyAvailable(sec.Items) {
		err = fmt.Errorf("no data returned")
	}
	if err != nil {
		e.lastErr = err.Error()
		e.lastErrAt = now
		return s.fallback(p, e)
	}
	sec.Key, sec.Title = p.Key(), p.Title()
	sec.Available = true
	sec.Stale = false
	sec.Error = ""
	sec.FetchedAt = now.UTC()
	if len(sec.Sources) == 0 {
		sec.Sources = p.Sources()
	}
	if sec.AsOf == "" {
		sec.AsOf = latestAsOf(sec.Items)
	}
	cp := sec
	e.good = &cp
	e.goodAt = now
	e.lastErr = ""
	return sec
}

func (s *Service) fallback(p Provider, e *cacheEntry) Section {
	if e.good != nil {
		sec := *e.good
		sec.Stale = true
		sec.Error = "Showing last known values: " + e.lastErr
		return sec
	}
	return Section{Key: p.Key(), Title: p.Title(), Items: []Quote{}, Available: false, Stale: false,
		Error: "Source unavailable: " + e.lastErr, Sources: p.Sources(), FetchedAt: e.lastErrAt.UTC()}
}

func anyAvailable(items []Quote) bool {
	for _, q := range items {
		if q.Available {
			return true
		}
	}
	return false
}

func latestAsOf(items []Quote) string {
	best := ""
	for _, q := range items {
		if q.AsOf > best {
			best = q.AsOf
		}
	}
	return best
}

// NormalizeCurrency returns an upper-case 3-letter code or def.
func NormalizeCurrency(c, def string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if len(c) != 3 {
		return def
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return def
		}
	}
	return c
}

// getJSON fetches url into v.
func getJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, v interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "DASYIN-ERP/1.0 (+market dashboard)")
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 160 {
			msg = msg[:160]
		}
		return fmt.Errorf("%s returned HTTP %d %s", hostOf(url), res.StatusCode, msg)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: invalid JSON: %v", hostOf(url), err)
	}
	return nil
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(u, "/?"); i >= 0 {
		u = u[:i]
	}
	return u
}

func pct(from, to float64) *float64 {
	if from == 0 {
		return nil
	}
	v := round((to-from)/from*100, 2)
	return &v
}

func round(v float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	if v < 0 {
		return -float64(int64(-v*p+0.5)) / p
	}
	return float64(int64(v*p+0.5)) / p
}

// sigRound rounds to a sensible number of significant digits for display.
func sigRound(v float64) float64 {
	a := v
	if a < 0 {
		a = -a
	}
	switch {
	case a == 0:
		return 0
	case a >= 1000:
		return round(v, 2)
	case a >= 1:
		return round(v, 4)
	case a >= 0.01:
		return round(v, 6)
	default:
		return round(v, 8)
	}
}

// downsample reduces a series to at most n points (keeping the last one).
func downsample(xs []float64, n int) []float64 {
	if len(xs) <= n || n < 2 {
		return xs
	}
	out := make([]float64, 0, n)
	step := float64(len(xs)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		out = append(out, xs[int(float64(i)*step+0.5)])
	}
	return out
}

func sortedKeys(m map[string]map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
