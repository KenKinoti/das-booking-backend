package market

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var currencyNames = map[string]string{
	"AUD": "Australian dollar", "USD": "US dollar", "EUR": "Euro", "GBP": "British pound",
	"KES": "Kenyan shilling", "NZD": "New Zealand dollar", "JPY": "Japanese yen", "CNY": "Chinese yuan",
	"CAD": "Canadian dollar", "CHF": "Swiss franc", "SGD": "Singapore dollar", "HKD": "Hong Kong dollar",
	"INR": "Indian rupee", "ZAR": "South African rand", "UGX": "Ugandan shilling", "TZS": "Tanzanian shilling",
	"NGN": "Nigerian naira", "AED": "UAE dirham", "SEK": "Swedish krona", "NOK": "Norwegian krone",
}

// ecbCurrencies are the currencies the ECB publishes reference rates for
// (and therefore Frankfurter serves).
var ecbCurrencies = map[string]bool{
	"AUD": true, "BGN": true, "BRL": true, "CAD": true, "CHF": true, "CNY": true, "CZK": true, "DKK": true,
	"EUR": true, "GBP": true, "HKD": true, "HUF": true, "IDR": true, "ILS": true, "INR": true, "ISK": true,
	"JPY": true, "KRW": true, "MXN": true, "MYR": true, "NOK": true, "NZD": true, "PHP": true, "PLN": true,
	"RON": true, "SEK": true, "SGD": true, "THB": true, "TRY": true, "USD": true, "ZAR": true,
}

var (
	srcECB      = Source{Name: "European Central Bank reference rates (via Frankfurter)", URL: "https://www.frankfurter.app"}
	srcERAPI    = Source{Name: "ExchangeRate-API (open access)", URL: "https://www.exchangerate-api.com"}
	srcGecko    = Source{Name: "CoinGecko", URL: "https://www.coingecko.com"}
	srcGoldAPI  = Source{Name: "Gold API (spot)", URL: "https://gold-api.com"}
	srcAlphaVan = Source{Name: "Alpha Vantage", URL: "https://www.alphavantage.co"}
)

// ---------------------------------------------------------------- FX

// FXProvider serves FX rates vs the base currency: ECB reference rates via
// Frankfurter (with a 30-day history for sparklines), and ExchangeRate-API's
// open endpoint for currencies the ECB does not cover (e.g. KES) or when
// Frankfurter is unreachable.
type FXProvider struct {
	FrankfurterURL string
	FallbackURL    string
	Symbols        []string
	HistoryDays    int
	CacheTTL       time.Duration
	Now            func() time.Time
}

func (p *FXProvider) Key() string        { return "fx" }
func (p *FXProvider) Title() string      { return "Exchange rates" }
func (p *FXProvider) TTL() time.Duration { return p.CacheTTL }
func (p *FXProvider) Enabled() bool      { return len(p.Symbols) > 0 }
func (p *FXProvider) Sources() []Source  { return []Source{srcECB, srcERAPI} }

func (p *FXProvider) symbolsFor(base string) []string {
	out := []string{}
	seen := map[string]bool{base: true}
	list := append([]string{}, p.Symbols...)
	if base != "AUD" {
		list = append([]string{"AUD"}, list...)
	}
	for _, s := range list {
		s = NormalizeCurrency(s, "")
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

type frankLatest struct {
	Base  string             `json:"base"`
	Date  string             `json:"date"`
	Rates map[string]float64 `json:"rates"`
}

type frankSeries struct {
	Rates map[string]map[string]float64 `json:"rates"`
}

type erLatest struct {
	Result   string             `json:"result"`
	Base     string             `json:"base_code"`
	Updated  int64              `json:"time_last_update_unix"`
	Rates    map[string]float64 `json:"rates"`
	ErrorTyp string             `json:"error-type"`
}

func (p *FXProvider) Fetch(ctx context.Context, req Request) (Section, error) {
	base := req.Base
	syms := p.symbolsFor(base)
	quotes := map[string]*Quote{}
	for _, s := range syms {
		name := currencyNames[s]
		if name == "" {
			name = s
		}
		quotes[s] = &Quote{Symbol: s, Name: name, Kind: "fx", Currency: s, Unit: fmt.Sprintf("%s per 1 %s", s, base)}
	}
	var ecbSyms, otherSyms []string
	for _, s := range syms {
		if ecbCurrencies[base] && ecbCurrencies[s] {
			ecbSyms = append(ecbSyms, s)
		} else {
			otherSyms = append(otherSyms, s)
		}
	}
	errs := []string{}
	used := []Source{}
	ecbDate := ""
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}

	if len(ecbSyms) > 0 {
		var latest frankLatest
		u := fmt.Sprintf("%s/latest?from=%s&to=%s", strings.TrimRight(p.FrankfurterURL, "/"), base, strings.Join(ecbSyms, ","))
		if err := getJSON(ctx, req.Client, u, nil, &latest); err != nil || len(latest.Rates) == 0 {
			if err == nil {
				err = fmt.Errorf("frankfurter: empty response")
			}
			errs = append(errs, err.Error())
			otherSyms = append(otherSyms, ecbSyms...)
		} else {
			used = append(used, srcECB)
			ecbDate = latest.Date
			for _, s := range ecbSyms {
				r, ok := latest.Rates[s]
				if !ok || r <= 0 {
					otherSyms = append(otherSyms, s)
					continue
				}
				q := quotes[s]
				q.Price, q.Available, q.AsOf = sigRound(r), true, latest.Date
				q.Source, q.SourceURL = srcECB.Name, srcECB.URL
			}
			// 30-day history for sparklines and changes (best effort)
			days := p.HistoryDays
			if days <= 0 {
				days = 30
			}
			end := now().UTC()
			start := end.AddDate(0, 0, -days)
			var hist frankSeries
			hu := fmt.Sprintf("%s/%s..%s?from=%s&to=%s", strings.TrimRight(p.FrankfurterURL, "/"),
				start.Format("2006-01-02"), end.Format("2006-01-02"), base, strings.Join(ecbSyms, ","))
			if err := getJSON(ctx, req.Client, hu, nil, &hist); err == nil && len(hist.Rates) > 0 {
				dates := sortedKeys(hist.Rates)
				for _, s := range ecbSyms {
					q := quotes[s]
					if !q.Available {
						continue
					}
					series := []float64{}
					lastDate := ""
					for _, d := range dates {
						if v, ok := hist.Rates[d][s]; ok && v > 0 {
							series = append(series, v)
							lastDate = d
						}
					}
					if latest.Date > lastDate && q.Price > 0 {
						series = append(series, q.Price)
					}
					if len(series) >= 2 {
						q.Spark = series
						q.SparkSpan = fmt.Sprintf("%dd", days)
						q.Change30D = pct(series[0], series[len(series)-1])
						q.Change1D = pct(series[len(series)-2], series[len(series)-1])
					}
				}
			}
		}
	}

	if len(otherSyms) > 0 && p.FallbackURL != "" {
		var er erLatest
		u := fmt.Sprintf("%s/%s", strings.TrimRight(p.FallbackURL, "/"), url.PathEscape(base))
		err := getJSON(ctx, req.Client, u, nil, &er)
		if err == nil && er.Result != "success" {
			err = fmt.Errorf("exchangerate-api: %s", firstNonEmpty(er.ErrorTyp, er.Result, "error"))
		}
		if err != nil {
			errs = append(errs, err.Error())
		} else {
			asOf := ""
			if er.Updated > 0 {
				asOf = time.Unix(er.Updated, 0).UTC().Format(time.RFC3339)
			}
			for _, s := range otherSyms {
				if r, ok := er.Rates[s]; ok && r > 0 {
					q := quotes[s]
					q.Price, q.Available, q.AsOf = sigRound(r), true, asOf
					q.Source, q.SourceURL = srcERAPI.Name, srcERAPI.URL
					if !containsSource(used, srcERAPI) {
						used = append(used, srcERAPI)
					}
				}
			}
		}
	}

	sec := Section{Items: []Quote{}, Sources: used, AsOf: ecbDate}
	for _, s := range syms {
		q := quotes[s]
		if q.Available && q.Price > 0 {
			q.Inverse = sigRound(1 / q.Price)
		} else {
			q.Note = "Rate not available"
		}
		sec.Items = append(sec.Items, *q)
	}
	sec.Note = "Rates show units of each currency per 1 " + base + ". ECB reference rates are published once per business day around 16:00 CET."
	if !anyAvailable(sec.Items) {
		return sec, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	if len(errs) > 0 {
		sec.Error = strings.Join(errs, "; ")
	}
	return sec, nil
}

func containsSource(list []Source, s Source) bool {
	for _, x := range list {
		if x.Name == s.Name {
			return true
		}
	}
	return false
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// ---------------------------------------------------------------- Crypto

// CryptoProvider serves crypto prices from CoinGecko's public API
// (/coins/markets, which includes 24h/7d/30d change and a 7-day sparkline).
type CryptoProvider struct {
	BaseURL  string
	IDs      []string
	APIKey   string // optional CoinGecko demo key
	CacheTTL time.Duration
}

func (p *CryptoProvider) Key() string        { return "crypto" }
func (p *CryptoProvider) Title() string      { return "Crypto" }
func (p *CryptoProvider) TTL() time.Duration { return p.CacheTTL }
func (p *CryptoProvider) Enabled() bool      { return len(p.IDs) > 0 }
func (p *CryptoProvider) Sources() []Source  { return []Source{srcGecko} }

type geckoCoin struct {
	ID          string   `json:"id"`
	Symbol      string   `json:"symbol"`
	Name        string   `json:"name"`
	Price       float64  `json:"current_price"`
	Change24    *float64 `json:"price_change_percentage_24h_in_currency"`
	Change24Alt *float64 `json:"price_change_percentage_24h"`
	Change7     *float64 `json:"price_change_percentage_7d_in_currency"`
	Change30    *float64 `json:"price_change_percentage_30d_in_currency"`
	LastUpdated string   `json:"last_updated"`
	Sparkline   struct {
		Price []float64 `json:"price"`
	} `json:"sparkline_in_7d"`
}

func (p *CryptoProvider) fetch(ctx context.Context, req Request, vs string) ([]geckoCoin, error) {
	u := fmt.Sprintf("%s/coins/markets?vs_currency=%s&ids=%s&sparkline=true&price_change_percentage=24h,7d,30d",
		strings.TrimRight(p.BaseURL, "/"), strings.ToLower(vs), url.QueryEscape(strings.Join(p.IDs, ",")))
	var h map[string]string
	if p.APIKey != "" {
		h = map[string]string{"x-cg-demo-api-key": p.APIKey}
	}
	var coins []geckoCoin
	err := getJSON(ctx, req.Client, u, h, &coins)
	return coins, err
}

func (p *CryptoProvider) Fetch(ctx context.Context, req Request) (Section, error) {
	cur := req.Base
	factor := 1.0
	note := ""
	coins, err := p.fetch(ctx, req, cur)
	if (err != nil || len(coins) == 0) && cur != "USD" {
		// Base currency not supported by CoinGecko (or request failed): try USD and convert.
		if usdRate := req.FX["USD"]; usdRate > 0 {
			if c2, err2 := p.fetch(ctx, req, "USD"); err2 == nil && len(c2) > 0 {
				coins, err = c2, nil
				factor = 1 / usdRate
				note = "Converted from USD at the current " + cur + "/USD rate"
			}
		}
	}
	if err != nil {
		return Section{}, err
	}
	byID := map[string]geckoCoin{}
	for _, c := range coins {
		byID[c.ID] = c
	}
	sec := Section{Items: []Quote{}, Sources: []Source{srcGecko}, Note: note}
	for _, id := range p.IDs {
		c, ok := byID[id]
		if !ok {
			sec.Items = append(sec.Items, Quote{Symbol: strings.ToUpper(id), Name: id, Kind: "crypto", Available: false, Source: srcGecko.Name, Note: "Not found"})
			continue
		}
		ch := c.Change24
		if ch == nil {
			ch = c.Change24Alt
		}
		q := Quote{
			Symbol: strings.ToUpper(c.Symbol), Name: c.Name, Kind: "crypto",
			Price: sigRound(c.Price * factor), Currency: cur, Unit: cur + " per 1 " + strings.ToUpper(c.Symbol),
			Change1D: roundPtr(ch), Change7D: roundPtr(c.Change7), Change30D: roundPtr(c.Change30),
			Available: c.Price > 0, Source: srcGecko.Name, SourceURL: "https://www.coingecko.com/en/coins/" + c.ID,
			AsOf: c.LastUpdated, Note: note,
		}
		if len(c.Sparkline.Price) > 1 {
			sp := downsample(c.Sparkline.Price, 56)
			for i := range sp {
				sp[i] = sigRound(sp[i] * factor)
			}
			q.Spark, q.SparkSpan = sp, "7d"
		}
		sec.Items = append(sec.Items, q)
	}
	return sec, nil
}

func roundPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := round(*v, 2)
	return &r
}

// ---------------------------------------------------------------- Metals

// MetalsProvider serves gold and silver spot prices (USD per troy ounce)
// from gold-api.com's free endpoint, converted to the base currency.
type MetalsProvider struct {
	BaseURL  string
	Symbols  []string
	CacheTTL time.Duration
}

func (p *MetalsProvider) Key() string        { return "metals" }
func (p *MetalsProvider) Title() string      { return "Precious metals" }
func (p *MetalsProvider) TTL() time.Duration { return p.CacheTTL }
func (p *MetalsProvider) Enabled() bool      { return len(p.Symbols) > 0 && p.BaseURL != "" }
func (p *MetalsProvider) Sources() []Source  { return []Source{srcGoldAPI} }

type goldAPIPrice struct {
	Name      string  `json:"name"`
	Price     float64 `json:"price"`
	Symbol    string  `json:"symbol"`
	UpdatedAt string  `json:"updatedAt"`
}

var metalNames = map[string]string{"XAU": "Gold", "XAG": "Silver", "XPT": "Platinum", "XPD": "Palladium"}

func (p *MetalsProvider) Fetch(ctx context.Context, req Request) (Section, error) {
	cur, factor, note := "USD", 1.0, ""
	if req.Base != "USD" {
		if r := req.FX["USD"]; r > 0 {
			cur, factor = req.Base, 1/r
			note = "USD spot converted to " + req.Base
		} else {
			note = "Shown in USD (no " + req.Base + " rate available)"
		}
	}
	sec := Section{Items: []Quote{}, Sources: []Source{srcGoldAPI}, Note: note}
	errs := []string{}
	for _, s := range p.Symbols {
		var g goldAPIPrice
		u := fmt.Sprintf("%s/price/%s", strings.TrimRight(p.BaseURL, "/"), s)
		name := metalNames[s]
		if name == "" {
			name = s
		}
		q := Quote{Symbol: s, Name: name, Kind: "metal", Currency: cur, Unit: cur + " per troy oz", Source: srcGoldAPI.Name, SourceURL: srcGoldAPI.URL}
		if err := getJSON(ctx, req.Client, u, nil, &g); err != nil || g.Price <= 0 {
			if err != nil {
				errs = append(errs, err.Error())
			}
			q.Note = "Price not available"
		} else {
			q.Price, q.Available, q.AsOf, q.Note = sigRound(g.Price*factor), true, g.UpdatedAt, note
		}
		sec.Items = append(sec.Items, q)
	}
	if !anyAvailable(sec.Items) {
		return sec, fmt.Errorf("%s", firstNonEmpty(strings.Join(errs, "; "), "no prices"))
	}
	return sec, nil
}

// ---------------------------------------------------------------- Indices (optional)

// SymbolDef is an instrument to quote.
type SymbolDef struct {
	Symbol string
	Name   string
}

func parseSymbols(s string) []SymbolDef {
	out := []SymbolDef{}
	for _, part := range splitList(s) {
		sym, name := part, part
		if i := strings.Index(part, "="); i > 0 {
			sym, name = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		out = append(out, SymbolDef{Symbol: sym, Name: name})
	}
	return out
}

// AlphaVantageProvider quotes equities/ETFs tracking major indices. It is
// only enabled when ALPHAVANTAGE_API_KEY is set (free keys allow ~25
// requests/day, hence the long default TTL).
type AlphaVantageProvider struct {
	BaseURL  string
	APIKey   string
	Symbols  []SymbolDef
	CacheTTL time.Duration
}

func (p *AlphaVantageProvider) Key() string        { return "indices" }
func (p *AlphaVantageProvider) Title() string      { return "Markets & indices" }
func (p *AlphaVantageProvider) TTL() time.Duration { return p.CacheTTL }
func (p *AlphaVantageProvider) Enabled() bool      { return p.APIKey != "" && len(p.Symbols) > 0 }
func (p *AlphaVantageProvider) Sources() []Source  { return []Source{srcAlphaVan} }

func exchangeCurrency(sym string) string {
	switch {
	case strings.HasSuffix(sym, ".AX"), strings.HasSuffix(sym, ".AUS"):
		return "AUD"
	case strings.HasSuffix(sym, ".LON"), strings.HasSuffix(sym, ".L"):
		return "GBP"
	case strings.HasSuffix(sym, ".TRT"), strings.HasSuffix(sym, ".TO"):
		return "CAD"
	case strings.HasSuffix(sym, ".DEX"), strings.HasSuffix(sym, ".DE"):
		return "EUR"
	}
	return "USD"
}

func (p *AlphaVantageProvider) Fetch(ctx context.Context, req Request) (Section, error) {
	sec := Section{Items: []Quote{}, Sources: []Source{srcAlphaVan}, Note: "Index levels are tracked via ETFs; prices are delayed."}
	errs := []string{}
	for _, s := range p.Symbols {
		var res map[string]interface{}
		u := fmt.Sprintf("%s?function=GLOBAL_QUOTE&symbol=%s&apikey=%s", p.BaseURL, url.QueryEscape(s.Symbol), url.QueryEscape(p.APIKey))
		q := Quote{Symbol: s.Symbol, Name: s.Name, Kind: "index", Currency: exchangeCurrency(s.Symbol), Source: srcAlphaVan.Name, SourceURL: srcAlphaVan.URL}
		err := getJSON(ctx, req.Client, u, nil, &res)
		if err == nil {
			for _, k := range []string{"Note", "Information", "Error Message"} {
				if m, ok := res[k].(string); ok && m != "" {
					err = fmt.Errorf("alphavantage: %s", m)
				}
			}
		}
		gq, _ := res["Global Quote"].(map[string]interface{})
		if err == nil && len(gq) == 0 {
			err = fmt.Errorf("alphavantage: no quote for %s", s.Symbol)
		}
		if err != nil {
			errs = append(errs, err.Error())
			q.Note = "Quote not available"
			sec.Items = append(sec.Items, q)
			continue
		}
		str := func(k string) string { v, _ := gq[k].(string); return v }
		price, _ := strconv.ParseFloat(str("05. price"), 64)
		cp, perr := strconv.ParseFloat(strings.TrimSuffix(str("10. change percent"), "%"), 64)
		q.Price, q.Available, q.AsOf = sigRound(price), price > 0, str("07. latest trading day")
		if perr == nil {
			q.Change1D = roundPtr(&cp)
		}
		sec.Items = append(sec.Items, q)
	}
	if !anyAvailable(sec.Items) {
		return sec, fmt.Errorf("%s", firstNonEmpty(strings.Join(errs, "; "), "no quotes"))
	}
	sort.SliceStable(sec.Items, func(i, j int) bool { return sec.Items[i].Available && !sec.Items[j].Available })
	return sec, nil
}
