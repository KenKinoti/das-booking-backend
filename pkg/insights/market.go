package insights

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Market indicators for a web / IT services business in Kenya and Australia,
// with East African Community peers for regional opportunity.

// Indicator describes a World Bank series shown on the page.
type Indicator struct {
	Code     string `json:"code"`
	Title    string `json:"title"`
	Unit     string `json:"unit"`   // pct | per100 | number | lcu_usd
	Short    string `json:"short"`  // unit suffix for display
	Why      string `json:"why"`    // why it matters for a web / IT business
	Custom   bool   `json:"custom"` // added by the organisation
	CustomID string `json:"custom_id,omitempty"`
}

// DefaultIndicators are verified codes of the World Bank WDI database.
var DefaultIndicators = []Indicator{
	{Code: "IT.NET.USER.ZS", Title: "Internet users", Unit: "pct", Short: "% of population", Why: "The size of the online audience your clients' websites can reach."},
	{Code: "IT.CEL.SETS.P2", Title: "Mobile subscriptions", Unit: "per100", Short: "per 100 people", Why: "Mobile-first design, mobile money and SMS integrations matter where this is high."},
	{Code: "IC.BUS.NREG", Title: "New businesses registered", Unit: "number", Short: "per year", Why: "Every new business needs a domain, email and a website — your top-of-funnel."},
	{Code: "NY.GDP.MKTP.KD.ZG", Title: "GDP growth", Unit: "pct", Short: "% a year", Why: "Growing economies grow IT budgets."},
	{Code: "BX.GSR.CCIS.ZS", Title: "ICT service exports", Unit: "pct", Short: "% of service exports", Why: "How established the country is at selling IT services abroad."},
	{Code: "FP.CPI.TOTL.ZG", Title: "Inflation", Unit: "pct", Short: "% a year", Why: "Your prices need to rise at least this much to keep their real value."},
	{Code: "PA.NUS.FCRF", Title: "Exchange rate", Unit: "lcu_usd", Short: "local currency per USD", Why: "Drives what AUD/USD revenue is worth at home and what AUD-priced supplier costs (Synergy) cost you."},
}

// DefaultCountries: the user's markets and EAC peers.
var DefaultCountries = []string{"KEN", "AUS", "UGA", "TZA", "RWA"}

// CountryNames for display when the API has none.
var CountryNames = map[string]string{"KEN": "Kenya", "AUS": "Australia", "UGA": "Uganda", "TZA": "Tanzania", "RWA": "Rwanda"}

var currencyCountry = map[string]string{"KES": "KEN", "AUD": "AUS", "UGX": "UGA", "TZS": "TZA", "RWF": "RWA", "NZD": "NZL", "GBP": "GBR", "ZAR": "ZAF", "NGN": "NGA", "GHS": "GHA", "INR": "IND", "CAD": "CAN"}

// CustomIndicator is an extra World Bank series an organisation follows.
type CustomIndicator struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;index"`
	Code           string    `json:"code" gorm:"type:varchar(80);not null"`
	Country        string    `json:"country" gorm:"type:varchar(3);not null"`
	Label          string    `json:"label" gorm:"type:varchar(160)"`
	CreatedAt      time.Time `json:"created_at"`
}

// TableName is fixed.
func (CustomIndicator) TableName() string { return "insights_custom_indicators" }

// DataPoint is one value of an uploaded series.
type DataPoint struct {
	Period string  `json:"period"` // YYYY, YYYY-MM, YYYY-Qn or YYYY-MM-DD
	Value  float64 `json:"value"`
}

// Dataset is a series the organisation uploaded (e.g. KENIC .ke domain
// registrations, Communications Authority of Kenya sector statistics).
type Dataset struct {
	ID             string      `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string      `json:"-" gorm:"type:varchar(255);not null;index"`
	Name           string      `json:"name" gorm:"type:varchar(160);not null"`
	Unit           string      `json:"unit" gorm:"type:varchar(60)"`
	Country        string      `json:"country" gorm:"type:varchar(3)"`
	Source         string      `json:"source" gorm:"type:varchar(255);not null"`
	SourceURL      string      `json:"source_url" gorm:"type:varchar(500)"`
	Notes          string      `json:"notes" gorm:"type:text"`
	Points         []DataPoint `json:"points" gorm:"type:text;serializer:json"`
	CreatedBy      string      `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// TableName is fixed.
func (Dataset) TableName() string { return "insights_datasets" }

// CountryStat is one country's figures for an indicator.
type CountryStat struct {
	Country     string    `json:"country"`
	Name        string    `json:"name"`
	Latest      *float64  `json:"latest"`
	LatestYear  int       `json:"latest_year"`
	Previous    *float64  `json:"previous"`
	PrevYear    int       `json:"previous_year"`
	Change      *float64  `json:"change"`      // pct units: points; others: %
	ChangeUnit  string    `json:"change_unit"` // pts | %
	FiveYear    *float64  `json:"five_year_change"`
	FiveYearPct *float64  `json:"five_year_pct"`
	CAGR        *float64  `json:"cagr_pct"`
	Trend       []WBPoint `json:"trend"` // last 6 years
}

// IndicatorCard is one small multiple.
type IndicatorCard struct {
	Indicator
	Name        string        `json:"name"` // World Bank's own name
	Countries   []CountryStat `json:"countries"`
	Note        string        `json:"note"`
	Source      string        `json:"source"`
	SourceURL   string        `json:"source_url"`
	LastUpdated string        `json:"last_updated"`
	FetchedAt   string        `json:"fetched_at"`
	Cached      bool          `json:"cached"`
	Stale       bool          `json:"stale"`
	Error       string        `json:"error,omitempty"`
}

// FXPair is a currency pair trend.
type FXPair struct {
	Pair       string    `json:"pair"` // e.g. AUD/KES = KES per 1 AUD
	Base       string    `json:"base"`
	Quote      string    `json:"quote"`
	Live       *float64  `json:"live"`
	LiveSource string    `json:"live_source"`
	LiveAsOf   string    `json:"live_as_of,omitempty"`
	Annual     []WBPoint `json:"annual"` // yearly averages from PA.NUS.FCRF
	FiveYear   *float64  `json:"five_year_pct"`
	YoY        *float64  `json:"yoy_pct"`
	Note       string    `json:"note"`
}

// Market is GET /insights/market.
type Market struct {
	Countries  []string        `json:"countries"`
	Home       string          `json:"home"`
	Indicators []IndicatorCard `json:"indicators"`
	FX         []FXPair        `json:"fx"`
	Datasets   []Dataset       `json:"datasets"`
	Highlights []string        `json:"highlights"`
	Source     string          `json:"source"`
	SourceURL  string          `json:"source_url"`
	APIBase    string          `json:"api_base"`
	AIEnabled  bool            `json:"ai_enabled"`
	FetchedAt  string          `json:"fetched_at"`
}

func valAt(pts []WBPoint, year int) *float64 {
	for _, p := range pts {
		if p.Year == year && p.Value != nil {
			return p.Value
		}
	}
	return nil
}

// Stat computes latest / previous / 5-year change of a series.
func Stat(s *WBSeries, unit string) CountryStat {
	cs := CountryStat{Country: s.Country, Name: nonEmpty(s.CountryName, CountryNames[s.Country]), Trend: []WBPoint{}}
	var latest *WBPoint
	for i := len(s.Points) - 1; i >= 0; i-- {
		if s.Points[i].Value != nil {
			latest = &s.Points[i]
			break
		}
	}
	if latest == nil {
		return cs
	}
	cs.Latest, cs.LatestYear = fptr(roundSig(*latest.Value)), latest.Year
	for i := len(s.Points) - 1; i >= 0; i-- {
		p := s.Points[i]
		if p.Year < latest.Year && p.Value != nil {
			cs.Previous, cs.PrevYear = fptr(roundSig(*p.Value)), p.Year
			break
		}
	}
	for _, p := range s.Points {
		if p.Year > latest.Year-6 && p.Year <= latest.Year {
			cs.Trend = append(cs.Trend, p)
		}
	}
	pctUnit := unit == "pct"
	if pctUnit {
		cs.ChangeUnit = "pts"
	} else {
		cs.ChangeUnit = "%"
	}
	if cs.Previous != nil {
		if pctUnit {
			cs.Change = fptr(r2(*latest.Value - *cs.Previous))
		} else {
			cs.Change = pctChange(*latest.Value, *cs.Previous)
		}
	}
	// Five years back (nearest available year at or after latest−5).
	for y := latest.Year - 5; y < latest.Year; y++ {
		if v := valAt(s.Points, y); v != nil {
			if pctUnit {
				cs.FiveYear = fptr(r2(*latest.Value - *v))
			} else {
				cs.FiveYearPct = pctChange(*latest.Value, *v)
				if *v > 0 && *latest.Value > 0 {
					n := float64(latest.Year - y)
					cs.CAGR = fptr(r1((math.Pow(*latest.Value / *v, 1/n) - 1) * 100))
				}
			}
			break
		}
	}
	return cs
}

func roundSig(v float64) float64 {
	a := math.Abs(v)
	switch {
	case a >= 1000:
		return math.Round(v)
	case a >= 1:
		return math.Round(v*100) / 100
	}
	return math.Round(v*10000) / 10000
}

func statOf(card *IndicatorCard, c string) *CountryStat {
	for i := range card.Countries {
		if card.Countries[i].Country == c {
			return &card.Countries[i]
		}
	}
	return nil
}

func fnum(v float64, unit string) string {
	switch unit {
	case "number":
		return fmtThousands(v)
	case "pct":
		return trimFloat(r1(v)) + "%"
	}
	return trimFloat(r1(v))
}

// Note writes the rule-based "what this means" text for a card.
func Note(card *IndicatorCard) string {
	ke, au := statOf(card, "KEN"), statOf(card, "AUS")
	has := func(s *CountryStat) bool { return s != nil && s.Latest != nil }
	peers := []string{}
	fastest, fv := "", math.Inf(-1)
	for _, c := range []string{"UGA", "TZA", "RWA"} {
		if s := statOf(card, c); has(s) {
			peers = append(peers, fmt.Sprintf("%s %s", s.Name, fnum(*s.Latest, card.Unit)))
			if *s.Latest > fv {
				fastest, fv = s.Name, *s.Latest
			}
		}
	}
	switch card.Code {
	case "IT.NET.USER.ZS":
		if has(ke) {
			t := fmt.Sprintf("Only %s of Kenyans were online in %d", fnum(*ke.Latest, "pct"), ke.LatestYear)
			if ke.FiveYear != nil {
				t += fmt.Sprintf(", up %s points in five years", trimFloat(r1(*ke.FiveYear)))
			}
			t += " — the online market is still being built: first websites, e-commerce and digital marketing have a long runway."
			if has(au) {
				t += fmt.Sprintf(" Australia (%s) is saturated: growth there comes from redesigns, performance and ongoing services, not first sites.", fnum(*au.Latest, "pct"))
			}
			return t
		}
	case "IT.CEL.SETS.P2":
		if has(ke) {
			t := fmt.Sprintf("Kenya has %s mobile subscriptions per 100 people", trimFloat(r1(*ke.Latest)))
			if *ke.Latest > 100 {
				t += " (more than one per person)"
			}
			return t + ". Sell mobile-first sites, M-Pesa / mobile-money checkout and SMS or WhatsApp integrations as standard, not extras."
		}
	case "IC.BUS.NREG":
		if has(ke) {
			t := fmt.Sprintf("%s new businesses were registered in Kenya in %d", fmtThousands(*ke.Latest), ke.LatestYear)
			if ke.Change != nil {
				t += fmt.Sprintf(" (%+.1f%% on %d)", *ke.Change, ke.PrevYear)
			}
			t += ". Each needs a domain, email and a website: a starter bundle (domain + Workspace + one-page site) targets this flow directly."
			if has(au) && au.Change != nil {
				t += fmt.Sprintf(" Australia: %s (%+.1f%%).", fmtThousands(*au.Latest), *au.Change)
			}
			return t
		}
	case "NY.GDP.MKTP.KD.ZG":
		if has(ke) {
			t := fmt.Sprintf("Kenya grew %s in %d", fnum(*ke.Latest, "pct"), ke.LatestYear)
			if has(au) {
				t += fmt.Sprintf(" vs Australia %s", fnum(*au.Latest, "pct"))
			}
			t += "."
			if fastest != "" && fv > *ke.Latest {
				t += fmt.Sprintf(" %s is growing faster (%s) — East African clients are a natural next market, served from Nairobi.", fastest, fnum(fv, "pct"))
			}
			return t
		}
	case "BX.GSR.CCIS.ZS":
		if has(ke) {
			t := fmt.Sprintf("ICT services are %s of Kenya's service exports", fnum(*ke.Latest, "pct"))
			if has(au) {
				t += fmt.Sprintf(" (Australia %s)", fnum(*au.Latest, "pct"))
			}
			return t + ". Selling IT work from Kenya to Australian clients is an established model: lead with AU time-zone overlap and AUD pricing."
		}
	case "FP.CPI.TOTL.ZG":
		if has(ke) {
			t := fmt.Sprintf("Kenyan inflation was %s in %d", fnum(*ke.Latest, "pct"), ke.LatestYear)
			if has(au) {
				t += fmt.Sprintf(", Australia %s", fnum(*au.Latest, "pct"))
			}
			return t + ". Review KES prices for retainers, hosting and Workspace at least yearly by this much to keep their real value; AUD-priced contracts are steadier."
		}
	case "PA.NUS.FCRF":
		if has(ke) && has(au) && *au.Latest > 0 {
			cross := *ke.Latest / *au.Latest
			return fmt.Sprintf("In %d one AUD averaged about KES %s (USD 1 = KES %s). A weaker shilling makes AUD revenue worth more at home but raises AUD-priced costs such as Synergy Wholesale — price KE hosting with an FX buffer.",
				ke.LatestYear, trimFloat(r1(cross)), trimFloat(r1(*ke.Latest)))
		}
	}
	if len(peers) > 0 {
		return "EAC peers: " + strings.Join(peers, ", ") + "."
	}
	if card.Error != "" {
		return ""
	}
	return "Latest values and five-year trend from the World Bank."
}

func fxAnnual(ser map[string]*WBSeries, base, quote string) []WBPoint {
	// PA.NUS.FCRF is local currency per USD; cross = quote per base.
	per := func(cur string) map[int]float64 {
		m := map[int]float64{}
		if cur == "USD" {
			for y := 1990; y <= 2100; y++ {
				m[y] = 1
			}
			return m
		}
		if s := ser[currencyCountry[cur]]; s != nil {
			for _, p := range s.Points {
				if p.Value != nil && *p.Value > 0 {
					m[p.Year] = *p.Value
				}
			}
		}
		return m
	}
	b, q := per(base), per(quote)
	out := []WBPoint{}
	years := []int{}
	for y := range q {
		if _, ok := b[y]; ok && y > 1990 && y < 2100 {
			years = append(years, y)
		}
	}
	sort.Ints(years)
	for _, y := range years {
		v := roundSig(q[y] / b[y])
		out = append(out, WBPoint{Year: y, Value: fptr(v)})
	}
	return out
}

// MarketFor builds the market view (fetches in parallel, cached 24 h).
func (s *Service) MarketFor(ctx context.Context, org string, force bool) *Market {
	home := s.Home(org)
	now := s.now()
	m := &Market{Countries: DefaultCountries, Home: home, Indicators: []IndicatorCard{}, FX: []FXPair{}, Datasets: []Dataset{}, Highlights: []string{},
		Source: "World Bank, World Development Indicators (CC BY 4.0)", SourceURL: "https://data.worldbank.org", FetchedAt: now.UTC().Format(time.RFC3339), AIEnabled: AIEnabled()}
	if s.WB == nil {
		return m
	}
	m.APIBase = s.WB.BaseURL
	inds := append([]Indicator(nil), DefaultIndicators...)
	type job struct {
		ind       Indicator
		countries []string
	}
	jobs := []job{}
	for _, in := range inds {
		jobs = append(jobs, job{in, DefaultCountries})
	}
	var customs []CustomIndicator
	s.DB.Where("organization_id = ?", org).Order("created_at").Find(&customs)
	for _, ci := range customs {
		jobs = append(jobs, job{Indicator{Code: ci.Code, Title: nonEmpty(ci.Label, ci.Code), Unit: "number", Custom: true, CustomID: ci.ID}, []string{strings.ToUpper(ci.Country)}})
	}
	from, to := now.Year()-11, now.Year()
	cards := make([]IndicatorCard, len(jobs))
	results := make([]*WBResult, len(jobs))
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.WB.Fetch(cctx, j.ind.Code, j.countries, from, to, force)
		}(i, j)
	}
	wg.Wait()
	for i, j := range jobs {
		r := results[i]
		card := IndicatorCard{Indicator: j.ind, Countries: []CountryStat{}, Source: "World Bank WDI", SourceURL: fmt.Sprintf("https://data.worldbank.org/indicator/%s", j.ind.Code),
			LastUpdated: r.LastUpdated, Cached: r.Cached, Stale: r.Stale, Error: r.Error}
		if !r.FetchedAt.IsZero() {
			card.FetchedAt = r.FetchedAt.UTC().Format(time.RFC3339)
		}
		for _, c := range j.countries {
			ser := r.Series[strings.ToUpper(c)]
			if ser == nil {
				continue
			}
			if card.Name == "" {
				card.Name = ser.IndicatorName
			}
			if j.ind.Custom && card.Title == j.ind.Code && ser.IndicatorName != "" {
				card.Title = ser.IndicatorName
			}
			card.Countries = append(card.Countries, Stat(ser, j.ind.Unit))
		}
		if card.Error == "" && len(card.Countries) == 0 {
			card.Error = "No data for this indicator and country."
		}
		card.Note = Note(&card)
		cards[i] = card
		if j.ind.Code == "PA.NUS.FCRF" && !j.ind.Custom {
			m.FX = s.fxPairs(org, home, r.Series)
		}
	}
	m.Indicators = cards
	s.DB.Where("organization_id = ?", org).Order("name").Find(&m.Datasets)
	m.Highlights = highlights(cards)
	return m
}

func (s *Service) fxPairs(org, home string, ser map[string]*WBSeries) []FXPair {
	pairs := [][2]string{}
	for _, b := range []string{"AUD", "USD"} {
		if b != home {
			pairs = append(pairs, [2]string{b, home})
		}
	}
	if home == "AUD" {
		pairs = append(pairs, [2]string{"AUD", "KES"})
	}
	out := []FXPair{}
	for _, p := range pairs {
		fp := FXPair{Pair: p[0] + "/" + p[1], Base: p[0], Quote: p[1], Annual: fxAnnual(ser, p[0], p[1])}
		if q := s.FX.Quote(org, p[0], p[1]); q.OK() {
			fp.Live, fp.LiveSource = fptr(q.Rate), q.Provider
			if q.AsOf != nil {
				fp.LiveAsOf = q.AsOf.UTC().Format(time.RFC3339)
			}
		}
		if n := len(fp.Annual); n > 0 {
			last := fp.Annual[n-1]
			if n > 1 {
				fp.YoY = pctChange(*last.Value, *fp.Annual[n-2].Value)
			}
			for _, a := range fp.Annual {
				if a.Year >= last.Year-5 {
					fp.FiveYear = pctChange(*last.Value, *a.Value)
					break
				}
			}
			if fp.FiveYear != nil {
				dir := "stronger"
				if *fp.FiveYear < 0 {
					dir = "weaker"
				}
				fp.Note = fmt.Sprintf("1 %s bought %s%% more %s in %d than five years earlier (%s is %s against %s).", p[0], trimFloat(r1(*fp.FiveYear)), p[1], last.Year, p[0], dir, p[1])
				if *fp.FiveYear < 0 {
					fp.Note = fmt.Sprintf("1 %s bought %s%% less %s in %d than five years earlier.", p[0], trimFloat(r1(-*fp.FiveYear)), p[1], last.Year)
				}
			}
		} else if fp.Live == nil {
			continue
		}
		out = append(out, fp)
	}
	return out
}

func highlights(cards []IndicatorCard) []string {
	out := []string{}
	for _, c := range cards {
		if c.Custom || c.Error != "" {
			continue
		}
		ke := statOf(&c, "KEN")
		if ke == nil || ke.Latest == nil {
			continue
		}
		switch c.Code {
		case "IC.BUS.NREG":
			out = append(out, fmt.Sprintf("%s new Kenyan businesses a year (%d) — the market for starter web bundles.", fmtThousands(*ke.Latest), ke.LatestYear))
		case "IT.NET.USER.ZS":
			out = append(out, fmt.Sprintf("%s of Kenyans online (%d) and rising — long runway for first websites and e-commerce.", fnum(*ke.Latest, "pct"), ke.LatestYear))
		case "FP.CPI.TOTL.ZG":
			out = append(out, fmt.Sprintf("Kenyan inflation %s (%d): the minimum yearly increase for KES retainers.", fnum(*ke.Latest, "pct"), ke.LatestYear))
		}
	}
	return out
}

// ---------- custom indicators & datasets ----------

// AddCustom validates and stores a custom indicator (checked against the API).
func (s *Service) AddCustom(ctx context.Context, org, code, country, label string) (*CustomIndicator, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	country = strings.ToUpper(strings.TrimSpace(country))
	if !ValidIndicator(code) {
		return nil, fmt.Errorf("enter a World Bank indicator code such as IT.NET.BBND.P2")
	}
	if !ValidCountry(country) {
		return nil, fmt.Errorf("enter a country code such as KEN or AUS")
	}
	var n int64
	s.DB.Model(&CustomIndicator{}).Where("organization_id = ?", org).Count(&n)
	if n >= 20 {
		return nil, fmt.Errorf("you can follow up to 20 custom indicators")
	}
	s.DB.Model(&CustomIndicator{}).Where("organization_id = ? AND code = ? AND country = ?", org, code, country).Count(&n)
	if n > 0 {
		return nil, fmt.Errorf("you already follow %s for %s", code, country)
	}
	if s.WB != nil {
		now := s.now()
		r := s.WB.Fetch(ctx, code, []string{country}, now.Year()-11, now.Year(), false)
		if r.Error != "" && !r.Stale && strings.Contains(r.Error, "World Bank API:") {
			return nil, fmt.Errorf("the World Bank does not recognise this indicator / country (%s)", r.Error)
		}
	}
	ci := &CustomIndicator{ID: uuid.NewString(), OrganizationID: org, Code: code, Country: country, Label: strings.TrimSpace(label), CreatedAt: s.now()}
	if len(ci.Label) > 160 {
		ci.Label = ci.Label[:160]
	}
	return ci, s.DB.Create(ci).Error
}

var (
	rePeriodY  = regexp.MustCompile(`^(\d{4})$`)
	rePeriodM  = regexp.MustCompile(`^(\d{4})[-/](\d{1,2})$`)
	rePeriodQ  = regexp.MustCompile(`^(\d{4})[- ]?[Qq]([1-4])$`)
	rePeriodQ2 = regexp.MustCompile(`^[Qq]([1-4])[- ](\d{4})$`)
	rePeriodD  = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	reMonName  = regexp.MustCompile(`^([A-Za-z]{3,9})[- ](\d{4})$`)
)

// NormPeriod parses a period label into a canonical, sortable form.
func NormPeriod(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if m := rePeriodY.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	if m := rePeriodD.FindStringSubmatch(s); m != nil {
		if _, err := time.Parse(dayFmt, s); err == nil {
			return s, true
		}
	}
	if m := rePeriodM.FindStringSubmatch(s); m != nil {
		mo, _ := strconv.Atoi(m[2])
		if mo >= 1 && mo <= 12 {
			return fmt.Sprintf("%s-%02d", m[1], mo), true
		}
	}
	if m := rePeriodQ.FindStringSubmatch(s); m != nil {
		return m[1] + "-Q" + m[2], true
	}
	if m := rePeriodQ2.FindStringSubmatch(s); m != nil {
		return m[2] + "-Q" + m[1], true
	}
	if m := reMonName.FindStringSubmatch(s); m != nil {
		for i := 1; i <= 12; i++ {
			mn := time.Month(i).String()
			if strings.EqualFold(m[1], mn) || strings.EqualFold(m[1], mn[:3]) {
				return fmt.Sprintf("%s-%02d", m[2], i), true
			}
		}
	}
	return "", false
}

// ParseSeriesCSV reads a two-column CSV (period, value); a header row and
// thousands separators are allowed; extra columns are ignored.
func ParseSeriesCSV(r io.Reader) ([]DataPoint, []string, error) {
	data, err := io.ReadAll(io.LimitReader(r, 2<<20))
	if err != nil {
		return nil, nil, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	sep := ','
	if first := strings.SplitN(string(data), "\n", 2)[0]; strings.Count(first, ";") > strings.Count(first, ",") {
		sep = ';'
	} else if strings.Count(first, "\t") > strings.Count(first, ",") {
		sep = '\t'
	}
	cr := csv.NewReader(bytes.NewReader(data))
	cr.Comma, cr.FieldsPerRecord, cr.TrimLeadingSpace = sep, -1, true
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("could not read the CSV: %v", err)
	}
	pts := []DataPoint{}
	warn := []string{}
	seen := map[string]bool{}
	for i, row := range rows {
		if len(row) < 2 {
			continue
		}
		p, ok := NormPeriod(row[0])
		if !ok {
			if i == 0 {
				continue // header
			}
			warn = append(warn, fmt.Sprintf("row %d: period %q not understood (use 2024, 2024-03, 2024-Q1 or 2024-03-31)", i+1, row[0]))
			continue
		}
		raw := strings.NewReplacer(",", "", " ", "", "%", "", " ", "").Replace(strings.TrimSpace(row[1]))
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			warn = append(warn, fmt.Sprintf("row %d: value %q is not a number", i+1, row[1]))
			continue
		}
		if seen[p] {
			warn = append(warn, fmt.Sprintf("row %d: %s appears twice (kept the first)", i+1, p))
			continue
		}
		seen[p] = true
		pts = append(pts, DataPoint{Period: p, Value: v})
	}
	if len(pts) == 0 {
		return nil, warn, fmt.Errorf("no rows with a period and a numeric value were found")
	}
	if len(pts) > 5000 {
		return nil, warn, fmt.Errorf("too many rows (max 5,000)")
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].Period < pts[j].Period })
	if len(warn) > 20 {
		warn = append(warn[:20], fmt.Sprintf("… and %d more", len(warn)-20))
	}
	return pts, warn, nil
}

// ---------- optional AI explanation ----------

// AIEnabled reports whether ANTHROPIC_API_KEY is configured.
func AIEnabled() bool { return strings.TrimSpace(getenv("ANTHROPIC_API_KEY")) != "" }

// Explain asks the configured Anthropic model for a short explanation of the
// indicators for a web / IT services business.
func (s *Service) Explain(ctx context.Context, m *Market, focus string) (string, error) {
	if !AIEnabled() {
		return "", fmt.Errorf("AI is not configured (ANTHROPIC_API_KEY is not set on the backend)")
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "I run a small web & IT services business (websites, hosting resold from Synergy Wholesale, Google Workspace, ERP software, consulting) based in Kenya with clients in Kenya and Australia. Home currency %s.\n", m.Home)
	b.WriteString("Here are World Bank indicators (latest value, year, change):\n")
	for _, c := range m.Indicators {
		if focus != "" && c.Code != focus {
			continue
		}
		fmt.Fprintf(b, "- %s (%s, %s):", c.Title, c.Code, c.Short)
		for _, s := range c.Countries {
			if s.Latest != nil {
				fmt.Fprintf(b, " %s %s (%d)", s.Country, trimFloat(*s.Latest), s.LatestYear)
				if s.Change != nil {
					fmt.Fprintf(b, " %+.1f%s", *s.Change, s.ChangeUnit)
				}
				b.WriteString(";")
			}
		}
		b.WriteString("\n")
	}
	for _, f := range m.FX {
		if f.Live != nil {
			fmt.Fprintf(b, "- FX %s live %s\n", f.Pair, trimFloat(*f.Live))
		}
	}
	b.WriteString("In at most 6 short bullet points, explain what these mean for my business and which concrete opportunities or risks to act on. Be specific and numeric; no preamble.")
	cfg := aiConfig()
	body, _ := json.Marshal(map[string]any{"model": cfg.model, "max_tokens": 700, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": []map[string]string{{"type": "text", "text": b.String()}}}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", cfg.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("the AI service could not be reached")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the AI service returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("unexpected AI response")
	}
	parts := []string{}
	for _, c := range out.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), nil
}
