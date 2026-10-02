package insights

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Target markets: public indicators (World Bank), exchange-rate volatility
// and the revenue already earned in each country, combined into a transparent
// opportunity score whose weights the organisation can change.

// TradeIndicators are the World Bank series of the target-market table.
//
// Every code was requested from https://api.worldbank.org/v2 on 2026-10-02
// (…/country/{codes}/indicator/{CODE}?format=json) and answered with data,
// "lastupdated" 2026-07-13:
//
//	NY.GDP.PCAP.CD  GDP per capita (current US$)                      — KEN AUS UGA TZA RWA GBR USA ARE ZAF
//	IT.NET.USER.ZS  Individuals using the Internet (% of population)  — the same nine
//	BX.GSR.CCIS.CD  ICT service exports (BoP, current US$)            — the same nine
//	BX.GSR.CCIS.ZS  ICT service exports (% of service exports, BoP)   — the same nine
//	PA.NUS.FCRF     Official exchange rate (LCU per US$)              — the same nine
//	BX.GSR.NFSV.CD  Service exports (BoP, current US$)                — KEN AUS
//	NE.TRD.GNFS.ZS  Trade (% of GDP)                                  — KEN AUS (the nine-country request timed out)
//	IC.BUS.NREG     New businesses registered (number)                — KEN AUS (the nine-country request timed out)
//
// Not every country reports every series. A value the API does not return
// shows "n/a" in the table and that factor is left out of the country's score
// (the remaining weights are rescaled), so a gap produces a missing number,
// never a wrong one.
var TradeIndicators = []Indicator{
	{Code: "NY.GDP.PCAP.CD", Title: "GDP per capita", Unit: "usd", Short: "US$", Why: "Purchasing power: what clients there can pay for web and IT services."},
	{Code: "IT.NET.USER.ZS", Title: "Internet users", Unit: "pct", Short: "% of population", Why: "How digital the market is — demand for websites, e-commerce and online services."},
	{Code: "IC.BUS.NREG", Title: "New businesses", Unit: "number", Short: "registered a year", Why: "Each new business needs a domain, email and a website."},
	{Code: "NE.TRD.GNFS.ZS", Title: "Trade", Unit: "pct", Short: "% of GDP", Why: "How used the economy is to buying and selling across borders."},
	{Code: "BX.GSR.NFSV.CD", Title: "Service exports", Unit: "usd", Short: "US$ a year", Why: "The size of the country's own services export sector."},
	{Code: "BX.GSR.CCIS.CD", Title: "ICT service exports", Unit: "usd", Short: "US$ a year", Why: "How much IT work the country itself sells abroad — competitors, and partners."},
	{Code: "BX.GSR.CCIS.ZS", Title: "ICT share of service exports", Unit: "pct", Short: "% of service exports", Why: "How established IT services are in the country's exports."},
}

const fxIndicator = "PA.NUS.FCRF"

// DefaultTargetMarkets is the starting country list.
var DefaultTargetMarkets = []string{"KE", "AU", "UG", "TZ", "RW", "GB", "US", "AE", "ZA"}

// ScoreFactor describes one part of the opportunity score.
type ScoreFactor struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Default float64 `json:"default"`
	Why     string  `json:"why"`
}

// ScoreFactors are the parts of the opportunity score, with default weights.
var ScoreFactors = []ScoreFactor{
	{"purchasing_power", "Purchasing power", 25, "GDP per capita (log scale): what clients can pay."},
	{"digital", "Digital readiness", 15, "Share of the population online."},
	{"new_business", "New business formation", 15, "New businesses registered a year (log scale): first-website demand."},
	{"openness", "Openness to trade", 10, "Trade as a share of GDP: used to buying across borders."},
	{"fx_stability", "Currency stability", 15, "Low month-to-month movement of the currency against yours."},
	{"foothold", "Your foothold", 20, "Revenue you already earn there (log scale): references and know-how to build on."},
}

// MarketsFormula explains the score.
const MarketsFormula = "Opportunity score (0–100) = Σ weight × factor ÷ Σ weights, over the factors that have data for the country. Each factor is scaled 0–1 across the countries in the table (lowest = 0, highest = 1; GDP per capita, new businesses and your revenue on a log scale; currency stability is reversed: the calmest currency scores 1). Factors without data are left out and the remaining weights rescaled. FX volatility = standard deviation of the month-to-month % change of the exchange rate over the last 12 recorded months; where fewer than 4 monthly rates exist it is estimated from World Bank yearly averages (std dev of yearly % changes ÷ √12)."

// DefaultWeights returns the default weights.
func DefaultWeights() map[string]float64 {
	m := map[string]float64{}
	for _, f := range ScoreFactors {
		m[f.Key] = f.Default
	}
	return m
}

// MarketValue is one indicator value of a country.
type MarketValue struct {
	Value float64 `json:"value"`
	Year  int     `json:"year"`
}

// ScorePart is one factor's contribution to a country's score.
type ScorePart struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Raw    string   `json:"raw"`  // the underlying figure, formatted
	Norm   *float64 `json:"norm"` // 0–1 across the table (nil = no data)
	Weight float64  `json:"weight"`
	Points float64  `json:"points"` // contribution to the score
}

// MarketRow is one target market.
type MarketRow struct {
	Country    string                  `json:"country"`
	ISO3       string                  `json:"iso3"`
	Name       string                  `json:"name"`
	Currency   string                  `json:"currency"`
	Home       bool                    `json:"home"`
	Indicators map[string]*MarketValue `json:"indicators"`
	FXVol      *float64                `json:"fx_volatility_pct"`
	FXBasis    string                  `json:"fx_basis"` // monthly | annual | home | same | ""
	FXPoints   int                     `json:"fx_points"`
	Revenue    float64                 `json:"revenue"` // last 12 months, home currency
	SharePct   float64                 `json:"share_pct"`
	Clients    int                     `json:"clients"`
	Score      float64                 `json:"score"`
	Factors    int                     `json:"factors"` // factors with data
	Parts      []ScorePart             `json:"parts"`
	Note       string                  `json:"note"`
	Rank       int                     `json:"rank"`
}

// Markets is GET /insights/trade/markets.
type Markets struct {
	Currency     string             `json:"currency"`
	HomeCountry  string             `json:"home_country"`
	Countries    []string           `json:"countries"`
	Rows         []MarketRow        `json:"rows"`
	Indicators   []Indicator        `json:"indicators"`
	Factors      []ScoreFactor      `json:"factors"`
	Weights      map[string]float64 `json:"weights"`
	Custom       bool               `json:"custom"` // list or weights were changed
	Formula      string             `json:"formula"`
	Errors       []string           `json:"errors"`
	Source       string             `json:"source"`
	SourceURL    string             `json:"source_url"`
	FetchedAt    string             `json:"fetched_at"`
	Stale        bool               `json:"stale"`
	RevenueFrom  string             `json:"revenue_from"`
	RevenueTo    string             `json:"revenue_to"`
	AllCountries []CountryInfo      `json:"all_countries"`
}

// marketConfig returns the organisation's country list and weights.
func (i *Intel) marketConfig(org string) ([]string, map[string]float64, bool) {
	st := i.Settings(org)
	custom := false
	countries := []string{}
	for _, c := range st.TargetMarkets {
		if ci, ok := CountryOf(c); ok {
			countries = append(countries, ci.ISO2)
		}
	}
	if len(countries) == 0 {
		countries = append(countries, DefaultTargetMarkets...)
	} else {
		custom = true
	}
	w := DefaultWeights()
	if len(st.Weights) > 0 {
		custom = true
		for k := range w {
			if v, ok := st.Weights[k]; ok && v >= 0 && !math.IsNaN(v) {
				w[k] = v
			}
		}
	}
	return countries, w, custom
}

// SaveMarketConfig stores the country list and the weights.
func (i *Intel) SaveMarketConfig(org string, countries []string, weights map[string]float64) error {
	seen := map[string]bool{}
	list := []string{}
	for _, c := range countries {
		ci, ok := CountryOf(c)
		if !ok {
			return fmt.Errorf("%q is not a country in the list", strings.ToUpper(strings.TrimSpace(c)))
		}
		if !seen[ci.ISO2] {
			seen[ci.ISO2] = true
			list = append(list, ci.ISO2)
		}
	}
	if len(list) < 2 {
		return fmt.Errorf("choose at least two countries to compare")
	}
	if len(list) > 15 {
		return fmt.Errorf("compare up to 15 countries at a time")
	}
	w := map[string]float64{}
	sum := 0.0
	for _, f := range ScoreFactors {
		v, ok := weights[f.Key]
		if !ok {
			v = f.Default
		}
		if math.IsNaN(v) || v < 0 || v > 100 {
			return fmt.Errorf("%s: the weight must be between 0 and 100", f.Label)
		}
		w[f.Key] = math.Round(v*10) / 10
		sum += w[f.Key]
	}
	if sum <= 0 {
		return fmt.Errorf("give at least one factor a weight above zero")
	}
	i.Settings(org)
	return i.DB.Model(&IntelSettings{}).Where("organization_id = ?", org).Updates(map[string]any{"target_markets": jsonText(list), "weights": jsonText(w), "updated_at": i.now()}).Error
}

// ResetMarketConfig restores the default countries and weights.
func (i *Intel) ResetMarketConfig(org string) error {
	i.Settings(org)
	return i.DB.Model(&IntelSettings{}).Where("organization_id = ?", org).Updates(map[string]any{"target_markets": "null", "weights": "null", "updated_at": i.now()}).Error
}

func latestPoint(s *WBSeries) *MarketValue {
	if s == nil {
		return nil
	}
	for k := len(s.Points) - 1; k >= 0; k-- {
		if p := s.Points[k]; p.Value != nil {
			return &MarketValue{Value: roundSig(*p.Value), Year: p.Year}
		}
	}
	return nil
}

// stdDev is the sample standard deviation (0 for fewer than two values).
func stdDev(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	m := mean(v)
	ss := 0.0
	for _, x := range v {
		ss += (x - m) * (x - m)
	}
	return math.Sqrt(ss / float64(len(v)-1))
}

// pctChanges returns the % change between consecutive values.
func pctChanges(v []float64) []float64 {
	out := []float64{}
	for k := 1; k < len(v); k++ {
		if v[k-1] > 0 {
			out = append(out, (v[k]-v[k-1])/v[k-1]*100)
		}
	}
	return out
}

// MonthlyVolatility is the std dev of month-to-month % changes of a rate
// series (needs at least 4 monthly rates = 3 changes).
func MonthlyVolatility(rates []float64) (float64, bool) {
	ch := pctChanges(rates)
	if len(ch) < 3 {
		return 0, false
	}
	return stdDev(ch), true
}

// AnnualVolatility estimates monthly volatility from yearly average rates:
// std dev of yearly % changes ÷ √12 (needs at least 4 years = 3 changes).
func AnnualVolatility(yearly []float64) (float64, bool) {
	ch := pctChanges(yearly)
	if len(ch) < 3 {
		return 0, false
	}
	return stdDev(ch) / math.Sqrt(12), true
}

// normalise scales values 0–1 across the rows that have one (nil = no data).
func normalise(vals []*float64, reverse bool) []*float64 {
	out := make([]*float64, len(vals))
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range vals {
		if v != nil {
			lo, hi = math.Min(lo, *v), math.Max(hi, *v)
		}
	}
	for k, v := range vals {
		if v == nil {
			continue
		}
		n := 0.0
		switch {
		case hi > lo:
			n = (*v - lo) / (hi - lo)
			if reverse {
				n = 1 - n
			}
		case reverse:
			n = 1 // every currency equally calm
		case hi > 0:
			n = 1
		}
		out[k] = fptr(math.Round(n*1000) / 1000)
	}
	return out
}

func logp(v *MarketValue) *float64 {
	if v == nil || v.Value <= 0 {
		return nil
	}
	return fptr(math.Log(v.Value))
}

func rawp(v *MarketValue) *float64 {
	if v == nil {
		return nil
	}
	return fptr(v.Value)
}

func usdShort(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return fmt.Sprintf("US$%sbn", trimFloat(r1(v/1e9)))
	case a >= 1e6:
		return fmt.Sprintf("US$%sm", trimFloat(r1(v/1e6)))
	}
	return "US$" + fmtThousands(v)
}

// ScoreMarkets fills Score, Parts and Rank of the rows from their figures.
func ScoreMarkets(rows []MarketRow, weights map[string]float64, home string) {
	n := len(rows)
	col := func(get func(r *MarketRow) *float64) []*float64 {
		out := make([]*float64, n)
		for k := range rows {
			out[k] = get(&rows[k])
		}
		return out
	}
	norms := map[string][]*float64{
		"purchasing_power": normalise(col(func(r *MarketRow) *float64 { return logp(r.Indicators["NY.GDP.PCAP.CD"]) }), false),
		"digital":          normalise(col(func(r *MarketRow) *float64 { return rawp(r.Indicators["IT.NET.USER.ZS"]) }), false),
		"new_business":     normalise(col(func(r *MarketRow) *float64 { return logp(r.Indicators["IC.BUS.NREG"]) }), false),
		"openness":         normalise(col(func(r *MarketRow) *float64 { return rawp(r.Indicators["NE.TRD.GNFS.ZS"]) }), false),
		"fx_stability":     normalise(col(func(r *MarketRow) *float64 { return r.FXVol }), true),
		"foothold":         normalise(col(func(r *MarketRow) *float64 { return fptr(math.Log1p(math.Max(r.Revenue, 0))) }), false),
	}
	raw := func(r *MarketRow, key string) string {
		iv := func(code string) *MarketValue { return r.Indicators[code] }
		switch key {
		case "purchasing_power":
			if v := iv("NY.GDP.PCAP.CD"); v != nil {
				return fmt.Sprintf("US$%s per person (%d)", fmtThousands(v.Value), v.Year)
			}
		case "digital":
			if v := iv("IT.NET.USER.ZS"); v != nil {
				return fmt.Sprintf("%s%% online (%d)", trimFloat(r1(v.Value)), v.Year)
			}
		case "new_business":
			if v := iv("IC.BUS.NREG"); v != nil {
				return fmt.Sprintf("%s new businesses (%d)", fmtThousands(v.Value), v.Year)
			}
		case "openness":
			if v := iv("NE.TRD.GNFS.ZS"); v != nil {
				return fmt.Sprintf("trade = %s%% of GDP (%d)", trimFloat(r1(v.Value)), v.Year)
			}
		case "fx_stability":
			if r.FXVol != nil {
				if r.FXBasis == "home" || r.FXBasis == "same" {
					return "your own currency"
				}
				return fmt.Sprintf("±%s%% a month", trimFloat(r2(*r.FXVol)))
			}
		case "foothold":
			if r.Revenue > 0 {
				return fmt.Sprintf("%s billed in 12 months", fmtThousands(r.Revenue))
			}
			return "no revenue yet"
		}
		return "no data"
	}
	for k := range rows {
		r := &rows[k]
		r.Parts = []ScorePart{}
		wsum := 0.0
		for _, f := range ScoreFactors {
			if norms[f.Key][k] != nil {
				wsum += weights[f.Key]
			}
		}
		score := 0.0
		r.Factors = 0
		for _, f := range ScoreFactors {
			p := ScorePart{Key: f.Key, Label: f.Label, Raw: raw(r, f.Key), Norm: norms[f.Key][k], Weight: weights[f.Key]}
			if p.Norm != nil {
				r.Factors++
				if wsum > 0 {
					p.Points = r1(100 * weights[f.Key] * *p.Norm / wsum)
					score += 100 * weights[f.Key] * *p.Norm / wsum
				}
			}
			r.Parts = append(r.Parts, p)
		}
		r.Score = r1(score)
		r.Home = r.Country == home
	}
	// Rank: best score first; ties by revenue, then name.
	idx := make([]int, n)
	for k := range idx {
		idx[k] = k
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, y := rows[idx[a]], rows[idx[b]]
		if x.Score != y.Score {
			return x.Score > y.Score
		}
		if x.Revenue != y.Revenue {
			return x.Revenue > y.Revenue
		}
		return x.Name < y.Name
	})
	for rank, k := range idx {
		rows[k].Rank = rank + 1
	}
	for k := range rows {
		rows[k].Note = marketNote(&rows[k], n)
	}
}

// marketNote writes the plain-English line of a market.
func marketNote(r *MarketRow, n int) string {
	strong, weak := []string{}, []string{}
	for _, p := range r.Parts {
		if p.Norm == nil || p.Weight <= 0 || p.Key == "foothold" {
			continue
		}
		switch {
		case *p.Norm >= 0.67:
			strong = append(strong, strings.ToLower(p.Label)+" ("+p.Raw+")")
		case *p.Norm <= 0.33:
			weak = append(weak, strings.ToLower(p.Label)+" ("+p.Raw+")")
		}
	}
	b := &strings.Builder{}
	if r.Home {
		b.WriteString("Your home market")
		if r.SharePct > 0 {
			fmt.Fprintf(b, " — %s%% of your revenue", trimFloat(r.SharePct))
		}
		b.WriteString(". ")
	} else if r.Revenue > 0 {
		fmt.Fprintf(b, "You already bill %s there (%s%% of revenue, %d client%s). ", fmtThousands(r.Revenue), trimFloat(r.SharePct), r.Clients, plural(r.Clients))
	} else {
		b.WriteString("No clients there yet. ")
	}
	if len(strong) > 0 {
		b.WriteString("Strong on " + joinAnd(first(strong, 3)) + ". ")
	}
	if len(weak) > 0 {
		b.WriteString("Weaker on " + joinAnd(first(weak, 2)) + ". ")
	}
	if !r.Home {
		switch {
		case r.Revenue > 0 && r.Rank <= (n+1)/2:
			b.WriteString("Worth growing: ask existing clients for referrals and quote in " + r.Currency + ".")
		case r.Revenue > 0:
			b.WriteString("Keep serving existing clients; the indicators do not argue for extra marketing spend.")
		case r.Rank <= 3:
			b.WriteString("A top-ranked market you are not in — test it with a small, targeted offer.")
		}
	}
	if r.Factors < len(ScoreFactors) {
		fmt.Fprintf(b, " Score uses %d of %d factors (no data for the rest).", r.Factors, len(ScoreFactors))
	}
	return strings.TrimSpace(b.String())
}

func first(l []string, n int) []string {
	if len(l) > n {
		return l[:n]
	}
	return l
}

func joinAnd(l []string) string {
	switch len(l) {
	case 0:
		return ""
	case 1:
		return l[0]
	}
	return strings.Join(l[:len(l)-1], ", ") + " and " + l[len(l)-1]
}

// MarketsFor builds the target-market table.
func (i *Intel) MarketsFor(ctx context.Context, org string, force bool) *Markets {
	home := i.Home(org)
	hc := i.homeCountry(org, home)
	countries, weights, custom := i.marketConfig(org)
	now := i.now()
	today := day(now)
	m := &Markets{Currency: home, HomeCountry: hc, Countries: countries, Rows: []MarketRow{}, Indicators: TradeIndicators, Factors: ScoreFactors, Weights: weights, Custom: custom,
		Formula: MarketsFormula, Errors: []string{}, Source: "World Bank, World Development Indicators (CC BY 4.0)", SourceURL: "https://data.worldbank.org",
		FetchedAt: now.UTC().Format(time.RFC3339), AllCountries: CountriesSorted()}

	// Revenue by country over the last 12 months (same rules as the trade view).
	rng := Range{From: today.AddDate(-1, 0, 1), To: today}
	m.RevenueFrom, m.RevenueTo = rng.From.Format(dayFmt), rng.To.Format(dayFmt)
	tr := i.TradeFor(org, Opts{Range: rng, Today: today})
	rev := map[string]TradeCountry{}
	for _, c := range tr.Countries {
		rev[c.Country] = c
	}

	// World Bank: one request per indicator for all countries (cached 24 h).
	iso3 := []string{}
	for _, c := range countries {
		ci, _ := CountryOf(c)
		iso3 = append(iso3, ci.ISO3)
	}
	fxCountries := append([]string{}, iso3...)
	if ci, ok := CountryOf(hc); ok && !containsStr(fxCountries, ci.ISO3) {
		fxCountries = append(fxCountries, ci.ISO3)
	}
	codes := []string{}
	for _, in := range TradeIndicators {
		codes = append(codes, in.Code)
	}
	codes = append(codes, fxIndicator)
	results := make([]*WBResult, len(codes))
	if i.WB != nil {
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for k, code := range codes {
			wg.Add(1)
			go func(k int, code string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				cs := iso3
				if code == fxIndicator {
					cs = fxCountries
				}
				results[k] = i.WB.Fetch(cctx, code, cs, now.Year()-11, now.Year(), force)
			}(k, code)
		}
		wg.Wait()
	}
	series := map[string]map[string]*WBSeries{}
	for k, code := range codes {
		r := results[k]
		if r == nil {
			continue
		}
		series[code] = r.Series
		if r.Error != "" {
			title := code
			for _, in := range TradeIndicators {
				if in.Code == code {
					title = in.Title
				}
			}
			m.Errors = append(m.Errors, title+": "+r.Error)
			if r.Stale {
				m.Stale = true
			}
		}
	}
	if i.WB == nil {
		m.Errors = append(m.Errors, "World Bank data is not available on this server.")
	}

	months := i.monthRates(org, home)
	from := monthStart(today).AddDate(0, -11, 0).Format("2006-01")
	homeFX := []WBPoint{}
	if ci, ok := CountryOf(hc); ok && series[fxIndicator] != nil {
		if s := series[fxIndicator][ci.ISO3]; s != nil {
			homeFX = s.Points
		}
	}
	for _, c := range countries {
		ci, _ := CountryOf(c)
		row := MarketRow{Country: ci.ISO2, ISO3: ci.ISO3, Name: ci.Name, Currency: ci.Currency, Home: ci.ISO2 == hc, Indicators: map[string]*MarketValue{}}
		for _, in := range TradeIndicators {
			if s := series[in.Code]; s != nil {
				row.Indicators[in.Code] = latestPoint(s[ci.ISO3])
			} else {
				row.Indicators[in.Code] = nil
			}
		}
		if rc, ok := rev[ci.ISO2]; ok {
			row.Revenue, row.SharePct, row.Clients = rc.Revenue, rc.SharePct, rc.Clients
		}
		// FX volatility against the home currency.
		switch {
		case ci.ISO2 == hc:
			row.FXVol, row.FXBasis = fptr(0), "home"
		case ci.Currency == home:
			row.FXVol, row.FXBasis = fptr(0), "same"
		default:
			ms := []string{}
			for k := range months[ci.Currency] {
				if k >= from {
					ms = append(ms, k)
				}
			}
			sort.Strings(ms)
			rates := []float64{}
			for _, k := range ms {
				rates = append(rates, months[ci.Currency][k])
			}
			if v, ok := MonthlyVolatility(rates); ok {
				row.FXVol, row.FXBasis, row.FXPoints = fptr(r2(v)), "monthly", len(rates)
			} else if s := series[fxIndicator]; s != nil && s[ci.ISO3] != nil && len(homeFX) > 0 {
				// Cross rate from yearly averages (local currency per USD): home per 1 unit.
				cross := []float64{}
				hm := map[int]float64{}
				for _, p := range homeFX {
					if p.Value != nil && *p.Value > 0 {
						hm[p.Year] = *p.Value
					}
				}
				for _, p := range s[ci.ISO3].Points {
					if p.Value != nil && *p.Value > 0 && hm[p.Year] > 0 && p.Year >= now.Year()-6 {
						cross = append(cross, hm[p.Year] / *p.Value)
					}
				}
				if v, ok := AnnualVolatility(cross); ok {
					row.FXVol, row.FXBasis, row.FXPoints = fptr(r2(v)), "annual", len(cross)
				}
			}
		}
		m.Rows = append(m.Rows, row)
	}
	ScoreMarkets(m.Rows, weights, hc)
	sort.SliceStable(m.Rows, func(a, b int) bool { return m.Rows[a].Rank < m.Rows[b].Rank })
	return m
}

func containsStr(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}
