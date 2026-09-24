package plans

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Catalog is an in-memory snapshot of modules, industries and tiers.
type Catalog struct {
	Modules    []Module   `json:"modules"`
	Industries []Industry `json:"industries"`
	Tiers      []Tier     `json:"tiers"`
}

// Module returns the module with key k.
func (c *Catalog) Module(k string) (Module, bool) {
	for _, m := range c.Modules {
		if m.Key == k {
			return m, true
		}
	}
	return Module{}, false
}

// Tier returns the tier with key k.
func (c *Catalog) Tier(k string) (Tier, bool) {
	for _, t := range c.Tiers {
		if t.Key == k {
			return t, true
		}
	}
	return Tier{}, false
}

// Industry returns the industry with key k.
func (c *Catalog) Industry(k string) (Industry, bool) {
	for _, i := range c.Industries {
		if i.Key == k {
			return i, true
		}
	}
	return Industry{}, false
}

// sellable reports whether k is an active, non-core module.
func (c *Catalog) sellable(k string) bool {
	m, ok := c.Module(k)
	return ok && m.Active && !m.Core
}

// AllSellable lists active non-core modules in display order.
func (c *Catalog) AllSellable() []string {
	ms := append([]Module{}, c.Modules...)
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].SortOrder < ms[j].SortOrder })
	out := []string{}
	for _, m := range ms {
		if m.Active && !m.Core {
			out = append(out, m.Key)
		}
	}
	return out
}

// Selection is what a customer asks for.
type Selection struct {
	Tier           string   `json:"tier"`
	Industry       string   `json:"industry"`
	Modules        []string `json:"modules"`
	Cycle          string   `json:"cycle"`
	Currency       string   `json:"currency"`
	CustomPriceUSD *float64 `json:"-"`
}

// Line is one priced row of a quote.
type Line struct {
	Kind      string  `json:"kind"` // base | module | included | addon | discount | custom
	Module    string  `json:"module,omitempty"`
	Label     string  `json:"label"`
	AmountUSD float64 `json:"amount_usd"` // monthly
	Amount    float64 `json:"amount"`     // monthly, in Currency
}

// Quote is a fully priced selection.
type Quote struct {
	Tier             string   `json:"tier"`
	TierName         string   `json:"tier_name"`
	Industry         string   `json:"industry"`
	Cycle            string   `json:"cycle"`
	Currency         string   `json:"currency"`
	Decimals         int      `json:"decimals"`
	Rate             float64  `json:"rate"` // Currency per 1 USD
	RateSource       string   `json:"rate_source"`
	Included         []string `json:"included"`     // modules covered by the tier/bundle
	Addons           []string `json:"addons"`       // extra priced modules
	Entitlements     []string `json:"entitlements"` // everything the org can use (incl. core)
	MaxModules       int      `json:"max_modules"`
	MaxUsers         int      `json:"max_users"`
	Lines            []Line   `json:"lines"`
	MonthlyUSD       float64  `json:"monthly_usd"`
	Monthly          float64  `json:"monthly"`         // monthly list price in Currency
	AnnualDiscount   float64  `json:"annual_discount"` // saved per year in Currency (annual cycle)
	AnnualDiscountPc float64  `json:"annual_discount_pct"`
	Total            float64  `json:"total"`        // per billing cycle, in Currency
	PerMonth         float64  `json:"per_month"`    // effective monthly in Currency
	TotalUSD         float64  `json:"total_usd"`    // per billing cycle, USD
	Enterprise       bool     `json:"enterprise"`   // custom pricing available
	CustomPrice      bool     `json:"custom_price"` // an override was applied
	Warnings         []string `json:"warnings,omitempty"`
}

// Validation errors.
var (
	ErrUnknownTier      = errors.New("unknown plan tier")
	ErrUnknownIndustry  = errors.New("unknown industry")
	ErrNoModules        = errors.New("select at least one system")
	ErrTooManyModules   = errors.New("too many systems for this plan")
	ErrUnknownModule    = errors.New("unknown module")
	ErrBadCycle         = errors.New("billing cycle must be monthly or annual")
	ErrUnsupportedCurr  = errors.New("unsupported currency")
	ErrRateNotAvailable = errors.New("exchange rate not available")
)

// QuoteError carries a machine code with a human message.
type QuoteError struct {
	Code string
	Err  error
	Msg  string
}

func (e *QuoteError) Error() string { return e.Msg }
func (e *QuoteError) Unwrap() error { return e.Err }

func qerr(code string, err error, format string, a ...interface{}) error {
	return &QuoteError{Code: code, Err: err, Msg: fmt.Sprintf(format, a...)}
}

// Normalize cleans a selection: lower-cases keys, dedupes modules, drops core
// and applies defaults.
func Normalize(s Selection) Selection {
	s.Tier = strings.ToLower(strings.TrimSpace(s.Tier))
	s.Industry = strings.ToLower(strings.TrimSpace(s.Industry))
	if s.Industry == "" {
		s.Industry = "general"
	}
	s.Cycle = strings.ToLower(strings.TrimSpace(s.Cycle))
	switch s.Cycle {
	case "", "month", "monthly":
		s.Cycle = CycleMonthly
	case "year", "yearly", "annual", "annually":
		s.Cycle = CycleAnnual
	}
	s.Currency = strings.ToUpper(strings.TrimSpace(s.Currency))
	if s.Currency == "" {
		s.Currency = "USD"
	}
	seen := map[string]bool{}
	mods := []string{}
	for _, m := range s.Modules {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" || m == CoreModule || seen[m] {
			continue
		}
		seen[m] = true
		mods = append(mods, m)
	}
	s.Modules = mods
	return s
}

// Rates converts USD into other currencies.
type Rates interface {
	Rate(code string) (rate float64, source string, ok bool)
}

// StaticRates is a simple map-backed Rates (tests, fallbacks).
type StaticRates map[string]float64

// Rate implements Rates.
func (s StaticRates) Rate(code string) (float64, string, bool) {
	if code == "USD" {
		return 1, "fixed", true
	}
	r, ok := s[code]
	return r, "fallback", ok && r > 0
}

// Resolve works out included/add-on modules and validates tier rules without
// pricing. It is used for entitlements and for quotes.
func Resolve(cat *Catalog, s Selection) (tier Tier, included, addons []string, err error) {
	s = Normalize(s)
	tier, ok := cat.Tier(s.Tier)
	if !ok {
		return tier, nil, nil, qerr("UNKNOWN_TIER", ErrUnknownTier, "Unknown plan %q", s.Tier)
	}
	if _, ok := cat.Industry(s.Industry); !ok {
		return tier, nil, nil, qerr("UNKNOWN_INDUSTRY", ErrUnknownIndustry, "Unknown industry %q", s.Industry)
	}
	for _, m := range s.Modules {
		if !cat.sellable(m) {
			return tier, nil, nil, qerr("UNKNOWN_MODULE", ErrUnknownModule, "Module %q is not available", m)
		}
	}
	included, addons = []string{}, []string{}
	switch {
	case tier.IncludesAllModules:
		included = cat.AllSellable()
	case tier.IncludesIndustryBundle:
		ind, _ := cat.Industry(s.Industry)
		inBundle := map[string]bool{}
		for _, m := range ind.Modules {
			if cat.sellable(m) && !inBundle[m] {
				inBundle[m] = true
				included = append(included, m)
			}
		}
		for _, m := range s.Modules {
			if !inBundle[m] {
				addons = append(addons, m)
			}
		}
	default:
		if len(s.Modules) == 0 {
			return tier, nil, nil, qerr("NO_MODULES", ErrNoModules, "Select at least one system for the %s plan", tier.Name)
		}
		addons = append(addons, s.Modules...)
	}
	if tier.MaxModules > 0 && !tier.IncludesAllModules {
		if n := len(included) + len(addons); n > tier.MaxModules {
			return tier, nil, nil, qerr("TOO_MANY_MODULES", ErrTooManyModules,
				"The %s plan includes up to %d systems — you selected %d. Remove %d or choose a bigger plan.",
				tier.Name, tier.MaxModules, n, n-tier.MaxModules)
		}
	}
	return tier, included, addons, nil
}

// Price quotes a selection in the requested currency.
func Price(cat *Catalog, s Selection, rates Rates) (*Quote, error) {
	s = Normalize(s)
	if s.Cycle != CycleMonthly && s.Cycle != CycleAnnual {
		return nil, qerr("BAD_CYCLE", ErrBadCycle, "Billing cycle must be monthly or annual")
	}
	if !currency.IsSupported(s.Currency) {
		return nil, qerr("UNSUPPORTED_CURRENCY", ErrUnsupportedCurr, "Currency %s is not supported", s.Currency)
	}
	tier, included, addons, err := Resolve(cat, s)
	if err != nil {
		return nil, err
	}
	rate, source, ok := rates.Rate(s.Currency)
	if !ok || rate <= 0 {
		return nil, qerr("RATE_UNAVAILABLE", ErrRateNotAvailable, "No exchange rate for %s", s.Currency)
	}
	dec := currency.Decimals(s.Currency)
	conv := func(usd float64) float64 { return NiceRound(usd*rate, dec) }

	q := &Quote{
		Tier: tier.Key, TierName: tier.Name, Industry: s.Industry, Cycle: s.Cycle, Currency: s.Currency,
		Decimals: dec, Rate: rate, RateSource: source, Included: included, Addons: addons,
		MaxModules: tier.MaxModules, MaxUsers: tier.MaxUsers, Enterprise: tier.Enterprise,
		AnnualDiscountPc: tier.AnnualDiscountPct, Lines: []Line{},
	}
	add := func(l Line) {
		l.AmountUSD = round(l.AmountUSD, 2)
		l.Amount = conv(l.AmountUSD)
		q.Lines = append(q.Lines, l)
		q.MonthlyUSD += l.AmountUSD
		q.Monthly += l.Amount
	}

	if s.CustomPriceUSD != nil && *s.CustomPriceUSD >= 0 {
		q.CustomPrice = true
		add(Line{Kind: "custom", Label: tier.Name + " plan — custom agreement", AmountUSD: *s.CustomPriceUSD})
	} else {
		add(Line{Kind: "base", Label: tier.Name + " plan", AmountUSD: tier.BasePriceUSD})
		for _, k := range included {
			m, _ := cat.Module(k)
			l := Line{Kind: "included", Module: k, Label: m.Name + " (included)"}
			q.Lines = append(q.Lines, l)
		}
		disc := clampPct(tier.AddonDiscountPct)
		for _, k := range addons {
			m, _ := cat.Module(k)
			price := m.MonthlyPriceUSD
			label := m.Name
			if tier.IncludesIndustryBundle || tier.IncludesAllModules {
				label += " (add-on)"
				if disc > 0 {
					price = price * (1 - disc/100)
					label = fmt.Sprintf("%s (add-on, %g%% off)", m.Name, disc)
				}
			}
			add(Line{Kind: map[bool]string{true: "addon", false: "module"}[tier.IncludesIndustryBundle || tier.IncludesAllModules], Module: k, Label: label, AmountUSD: price})
		}
	}
	q.MonthlyUSD = round(q.MonthlyUSD, 2)
	q.Monthly = round(q.Monthly, dec)

	if s.Cycle == CycleAnnual {
		d := clampPct(tier.AnnualDiscountPct)
		full := q.Monthly * 12
		q.Total = NiceRound(full*(1-d/100), dec)
		q.AnnualDiscount = round(full-q.Total, dec)
		q.PerMonth = round(q.Total/12, dec)
		q.TotalUSD = round(q.MonthlyUSD*12*(1-d/100), 2)
	} else {
		q.Total = q.Monthly
		q.PerMonth = q.Monthly
		q.TotalUSD = q.MonthlyUSD
	}

	q.Entitlements = Entitlements(included, addons)
	if tier.Key == TierSmall || (!tier.IncludesAllModules && tier.MaxModules > 0) {
		if left := tier.MaxModules - len(included) - len(addons); left > 0 {
			q.Warnings = append(q.Warnings, fmt.Sprintf("You can add %d more system(s) on this plan.", left))
		}
	}
	return q, nil
}

// Entitlements merges core + included + addons into a sorted unique list.
func Entitlements(included, addons []string) []string {
	seen := map[string]bool{CoreModule: true}
	out := []string{CoreModule}
	for _, list := range [][]string{included, addons} {
		for _, m := range list {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

func clampPct(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

func round(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}

// NiceRound rounds a converted price so it reads well in its currency:
// below 10 it keeps the currency's minor units; above that it keeps three
// significant figures and never shows minor units (e.g. 3,741.12 KES → 3,740,
// 44,950 NGN → 45,000, 26.68 EUR → 27).
func NiceRound(v float64, decimals int) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	neg := v < 0
	a := math.Abs(v)
	var r float64
	if a < 10 {
		r = round(a, decimals)
	} else {
		exp := int(math.Floor(math.Log10(a))) - 2
		if exp < 0 {
			exp = 0
		}
		step := math.Pow10(exp)
		r = math.Round(a/step) * step
	}
	if neg {
		return -r
	}
	return r
}
