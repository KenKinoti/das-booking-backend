package insights

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// World Bank Indicators API v2 (https://datahelpdesk.worldbank.org/knowledgebase/articles/889392),
// no key needed:
//
//	GET {base}/v2/country/KEN;AUS/indicator/IT.NET.USER.ZS?format=json&date=2015:2026&per_page=1000&page=1
//
// The response is a two-element array [meta, rows] (rows null when there is
// no data) or [{"message":[{"id","key","value"}]}] on error. Responses are
// cached for 24 hours in memory and in insights_wb_cache, and a stale copy is
// served (flagged) when the API cannot be reached.

// DefaultWorldBankURL is the public API.
const DefaultWorldBankURL = "https://api.worldbank.org"

// WBCache is a cached API answer (public data, not per organisation).
type WBCache struct {
	Key       string    `gorm:"type:varchar(255);primaryKey"`
	Body      string    `gorm:"type:text"`
	FetchedAt time.Time `gorm:"index"`
}

// TableName is fixed.
func (WBCache) TableName() string { return "insights_wb_cache" }

// WBPoint is one yearly observation (Value nil when the World Bank has none).
type WBPoint struct {
	Year  int      `json:"year"`
	Value *float64 `json:"value"`
}

// WBSeries is one indicator for one country.
type WBSeries struct {
	Indicator     string    `json:"indicator"`
	IndicatorName string    `json:"indicator_name"`
	Country       string    `json:"country"` // ISO3
	CountryName   string    `json:"country_name"`
	Unit          string    `json:"unit,omitempty"`
	Points        []WBPoint `json:"points"` // ascending years
}

// WBResult is one fetch (one indicator, several countries).
type WBResult struct {
	Series      map[string]*WBSeries `json:"series"`
	LastUpdated string               `json:"last_updated"` // the World Bank's own date for the indicator
	FetchedAt   time.Time            `json:"fetched_at"`
	Cached      bool                 `json:"cached"`
	Stale       bool                 `json:"stale"`
	Error       string               `json:"error,omitempty"`
	URL         string               `json:"url"`
}

// WorldBank is the API client.
type WorldBank struct {
	BaseURL string
	Client  *http.Client
	TTL     time.Duration
	DB      *gorm.DB // optional persistent cache
	Now     func() time.Time
	PerPage int

	mu    sync.Mutex
	mem   map[string]memEntry
	calls int
}

type memEntry struct {
	res *WBResult
	at  time.Time
}

// NewWorldBankFromEnv uses WORLDBANK_BASE_URL (default the public API).
func NewWorldBankFromEnv(db *gorm.DB) *WorldBank {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("WORLDBANK_BASE_URL")), "/")
	if base == "" {
		base = DefaultWorldBankURL
	}
	return &WorldBank{BaseURL: base, Client: &http.Client{Timeout: 12 * time.Second}, TTL: 24 * time.Hour, DB: db}
}

func (w *WorldBank) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Calls is the number of HTTP requests made (for tests).
func (w *WorldBank) Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

var (
	reIndicator = regexp.MustCompile(`^[A-Za-z0-9]{1,12}(\.[A-Za-z0-9_]{1,20}){1,6}$`)
	reCountry   = regexp.MustCompile(`^[A-Za-z]{2,3}$`)
)

// ValidIndicator reports whether code looks like a World Bank indicator code.
func ValidIndicator(code string) bool { return reIndicator.MatchString(code) }

// ValidCountry reports whether code looks like an ISO2/ISO3 country (or WB aggregate) code.
func ValidCountry(code string) bool { return reCountry.MatchString(code) }

type wbMeta struct {
	Page        int    `json:"page"`
	Pages       int    `json:"pages"`
	PerPage     any    `json:"per_page"`
	Total       int    `json:"total"`
	LastUpdated string `json:"lastupdated"`
	Message     []struct {
		ID    string `json:"id"`
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"message"`
}

type wbRow struct {
	Indicator struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	} `json:"indicator"`
	Country struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	} `json:"country"`
	ISO3  string   `json:"countryiso3code"`
	Date  string   `json:"date"`
	Value *float64 `json:"value"`
	Unit  string   `json:"unit"`
}

// URLFor is the human-facing API URL of a request (for attribution).
func (w *WorldBank) URLFor(code string, countries []string, from, to int) string {
	return fmt.Sprintf("%s/v2/country/%s/indicator/%s?format=json&date=%d:%d", w.BaseURL, strings.Join(countries, ";"), url.PathEscape(code), from, to)
}

func cacheKey(code string, countries []string, from, to int) string {
	cs := append([]string(nil), countries...)
	sort.Strings(cs)
	return fmt.Sprintf("%s|%s|%d:%d", strings.ToUpper(code), strings.ToUpper(strings.Join(cs, ";")), from, to)
}

// Fetch returns an indicator for several countries and years, from cache
// when fresh (force skips the cache).
func (w *WorldBank) Fetch(ctx context.Context, code string, countries []string, from, to int, force bool) *WBResult {
	key := cacheKey(code, countries, from, to)
	now := w.now()
	var stale *WBResult
	w.mu.Lock()
	if e, ok := w.mem[key]; ok {
		if !force && now.Sub(e.at) < w.TTL {
			w.mu.Unlock()
			r := *e.res
			r.Cached = true
			return &r
		}
		stale = e.res
	}
	w.mu.Unlock()
	if stale == nil && w.DB != nil {
		var row WBCache
		if w.DB.Where("key = ?", key).First(&row).Error == nil {
			var r WBResult
			if json.Unmarshal([]byte(row.Body), &r) == nil {
				r.FetchedAt = row.FetchedAt
				if !force && now.Sub(row.FetchedAt) < w.TTL {
					w.store(key, &r, row.FetchedAt)
					c := r
					c.Cached = true
					return &c
				}
				stale = &r
			}
		}
	}
	res, err := w.fetchAll(ctx, code, countries, from, to)
	if err != nil {
		if stale != nil {
			r := *stale
			r.Cached, r.Stale, r.Error = true, true, "Could not refresh from the World Bank ("+err.Error()+"); showing data fetched "+stale.FetchedAt.Format("2 Jan 2006 15:04")+"."
			return &r
		}
		return &WBResult{Series: map[string]*WBSeries{}, Error: err.Error(), URL: w.URLFor(code, countries, from, to), FetchedAt: now}
	}
	res.FetchedAt = now
	w.store(key, res, now)
	if w.DB != nil {
		if b, err := json.Marshal(res); err == nil {
			w.DB.Save(&WBCache{Key: key, Body: string(b), FetchedAt: now})
		}
	}
	return res
}

func (w *WorldBank) store(key string, r *WBResult, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.mem == nil {
		w.mem = map[string]memEntry{}
	}
	w.mem[key] = memEntry{res: r, at: at}
}

func (w *WorldBank) fetchAll(ctx context.Context, code string, countries []string, from, to int) (*WBResult, error) {
	if !ValidIndicator(code) {
		return nil, fmt.Errorf("invalid indicator code %q", code)
	}
	for _, c := range countries {
		if !ValidCountry(c) {
			return nil, fmt.Errorf("invalid country code %q", c)
		}
	}
	per := w.PerPage
	if per <= 0 {
		per = 1000
	}
	res := &WBResult{Series: map[string]*WBSeries{}, URL: w.URLFor(code, countries, from, to)}
	for page, pages := 1, 1; page <= pages && page <= 50; page++ {
		u := fmt.Sprintf("%s/v2/country/%s/indicator/%s?format=json&date=%d:%d&per_page=%d&page=%d",
			w.BaseURL, strings.Join(countries, ";"), url.PathEscape(code), from, to, per, page)
		meta, rows, err := w.get(ctx, u)
		if err != nil {
			return nil, err
		}
		if meta.LastUpdated != "" {
			res.LastUpdated = meta.LastUpdated
		}
		pages = meta.Pages
		for _, r := range rows {
			y, err := strconv.Atoi(strings.TrimSpace(r.Date))
			if err != nil {
				continue
			}
			cc := strings.ToUpper(r.ISO3)
			if cc == "" {
				cc = strings.ToUpper(r.Country.ID)
			}
			s := res.Series[cc]
			if s == nil {
				s = &WBSeries{Indicator: r.Indicator.ID, IndicatorName: r.Indicator.Value, Country: cc, CountryName: r.Country.Value, Unit: r.Unit, Points: []WBPoint{}}
				res.Series[cc] = s
			}
			s.Points = append(s.Points, WBPoint{Year: y, Value: r.Value})
		}
	}
	for _, s := range res.Series {
		sort.Slice(s.Points, func(i, j int) bool { return s.Points[i].Year < s.Points[j].Year })
	}
	return res, nil
}

func (w *WorldBank) get(ctx context.Context, u string) (*wbMeta, []wbRow, error) {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "DASYIN-ERP/insights")
	cl := w.Client
	if cl == nil {
		cl = &http.Client{Timeout: 12 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("World Bank API unreachable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("World Bank API read failed")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("World Bank API returned HTTP %d", resp.StatusCode)
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(body, &parts); err != nil || len(parts) == 0 {
		return nil, nil, fmt.Errorf("unexpected World Bank API response")
	}
	var meta wbMeta
	if err := json.Unmarshal(parts[0], &meta); err != nil {
		return nil, nil, fmt.Errorf("unexpected World Bank API response")
	}
	if len(meta.Message) > 0 {
		m := meta.Message[0]
		return nil, nil, fmt.Errorf("World Bank API: %s", strings.TrimSpace(m.Key+" — "+m.Value))
	}
	var rows []wbRow
	if len(parts) > 1 && string(parts[1]) != "null" {
		if err := json.Unmarshal(parts[1], &rows); err != nil {
			return nil, nil, fmt.Errorf("unexpected World Bank API data")
		}
	}
	return &meta, rows, nil
}
