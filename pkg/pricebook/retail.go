package pricebook

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Retail pricing: from what a combo costs to what to charge for it.
//
//	C  yearly cost in the market's currency
//	t  tax rate (VAT / GST), f payment fee as a share of what the customer
//	   pays (tax included), F fixed fee per payment
//	N  price before tax, G = N × (1 + t) what the customer pays
//	fee    = f × G + F
//	profit = N − fee − C
//
//	target margin m (profit ÷ N):  N = (C + F) ÷ (1 − m − f × (1 + t))
//	target markup k (profit ÷ C):  N = (C × (1 + k) + F) ÷ (1 − f × (1 + t))
//
// The quoted figure (G when prices include tax, else N) is then rounded UP
// to a friendly price, so the real margin is never below the target, and
// the margin is worked out again from the rounded price.

// RetailMarket is where the price is quoted: a currency with its tax, fees
// and rounding.
type RetailMarket struct {
	Currency         string  `json:"currency"`
	TaxRate          float64 `json:"tax_rate"` // %
	PricesIncludeTax bool    `json:"prices_include_tax"`
	FeePercent       float64 `json:"fee_percent"` // % of what the customer pays
	FeeFixed         float64 `json:"fee_fixed"`   // per payment
	Rounding         string  `json:"rounding"`    // friendly (default) | step | none
	Step             float64 `json:"step"`        // for rounding "step": round up to a multiple of it
	FeeLabel         string  `json:"fee_label,omitempty"`
}

// RetailInput is the pricing policy.
type RetailInput struct {
	Basis   string         `json:"basis"`   // higher (default) | first_year | ongoing
	Mode    string         `json:"mode"`    // margin (default) | markup
	Percent float64        `json:"percent"` // target margin or markup %
	Markets []RetailMarket `json:"markets"`
	Shock   float64        `json:"shock"` // FX sensitivity in % (default 10)
}

// PricePoint is one price with what is left of it.
type PricePoint struct {
	Price      float64 `json:"price"`     // the figure to quote (incl. tax when prices include tax)
	Unrounded  float64 `json:"unrounded"` // before friendly rounding
	Net        float64 `json:"net"`       // before tax
	Tax        float64 `json:"tax"`
	Gross      float64 `json:"gross"` // what the customer pays
	Fee        float64 `json:"fee"`   // payment fees
	Cost       float64 `json:"cost"`
	Profit     float64 `json:"profit"`     // net − fee − cost
	MarginPct  float64 `json:"margin_pct"` // profit ÷ net
	MarkupPct  float64 `json:"markup_pct"` // profit ÷ cost
	ProfitHome float64 `json:"profit_home"`
}

// Sensitivity is the margin if exchange rates move.
type Sensitivity struct {
	ShockPct   float64 `json:"shock_pct"` // +10 = foreign costs 10% dearer
	Cost       float64 `json:"cost"`
	Profit     float64 `json:"profit"`
	MarginPct  float64 `json:"margin_pct"`
	ProfitHome float64 `json:"profit_home"`
}

// Position places the price among the competitors' prices.
type Position struct {
	Basis       string  `json:"basis"` // first_year | ongoing
	Stats       Stats   `json:"stats"` // in the market's currency
	Where       string  `json:"where"` // cheapest | below_median | above_median | most_expensive
	Rank        int     `json:"rank"`  // 1 = cheapest
	Of          int     `json:"of"`    // competitors + you
	VsMedianPct float64 `json:"vs_median_pct"`
	Text        string  `json:"text"`
}

// RetailPrice is the suggested price in one market.
type RetailPrice struct {
	Market      RetailMarket  `json:"market"`
	Rate        float64       `json:"rate"`        // home units per 1 unit of the market's currency
	Cost        float64       `json:"cost"`        // per year
	FixedCost   float64       `json:"fixed_cost"`  // the part already in this currency
	MovingCost  float64       `json:"moving_cost"` // the part that moves with exchange rates
	Yearly      *PricePoint   `json:"yearly"`
	Monthly     *PricePoint   `json:"monthly"`
	Sensitivity []Sensitivity `json:"sensitivity"`
	Positions   []Position    `json:"positions"`
	Error       string        `json:"error,omitempty"`
}

// RetailResult is the retail pricing of one option.
type RetailResult struct {
	Home       string        `json:"home"`
	TLD        string        `json:"tld"`
	Key        string        `json:"key"`
	Label      string        `json:"label"`
	Basis      string        `json:"basis"`      // as asked
	BasisUsed  string        `json:"basis_used"` // first_year | ongoing
	FirstYearH float64       `json:"first_year_home"`
	OngoingH   float64       `json:"ongoing_home"`
	CostHome   float64       `json:"cost_home"`
	Mode       string        `json:"mode"`
	Percent    float64       `json:"percent"`
	Shock      float64       `json:"shock"`
	Markets    []RetailPrice `json:"markets"`
	CostNote   string        `json:"cost_note"`
	Warnings   []string      `json:"warnings"`
}

// Friendly rounds a price UP to one that reads well:
//
//	1,000 and over   ends in 90 on the hundreds   4,321 → 4,390 · 4,990 → 4,990
//	100 – 999.99     the next multiple of 10      487.30 → 490
//	1 – 99.99        ends in .95                  49.20 → 49.95 (whole units for currencies without cents)
//	under 1          the next 0.05
func Friendly(v float64, cur string) float64 {
	if v <= 0 {
		return 0
	}
	const eps = 1e-9
	dec := currency.Decimals(cur)
	switch {
	case v >= 1000:
		c := math.Floor(v/100)*100 + 90
		if c < v-eps {
			c += 100
		}
		return c
	case v >= 100:
		return math.Ceil(v/10-eps) * 10
	case v >= 1:
		if dec == 0 {
			return math.Ceil(v - eps)
		}
		c := math.Floor(v) + 0.95
		if c < v-eps {
			c++
		}
		return math.Round(c*100) / 100
	default:
		if dec == 0 {
			return 1
		}
		return math.Round(math.Ceil(v/0.05-eps)*0.05*100) / 100
	}
}

// RoundUp applies a market's rounding to a price (never down, except
// "none", which rounds to the currency's minor units).
func RoundUp(v float64, m RetailMarket) float64 {
	switch m.Rounding {
	case "none":
		return RoundCur(v, m.Currency)
	case "step":
		if m.Step > 0 {
			return RoundCur(math.Ceil(v/m.Step-1e-9)*m.Step, m.Currency)
		}
		return RoundCur(v, m.Currency)
	}
	return Friendly(v, m.Currency)
}

// ErrNoMargin is returned when the target margin and the fees leave nothing.
var ErrNoMargin = errors.New("the margin and the payment fees add up to 100% or more — lower one of them")

// PriceFor works out the price for a cost in a market.
func PriceFor(cost float64, mode string, pct float64, m RetailMarket) (*PricePoint, error) {
	t, f := m.TaxRate/100, m.FeePercent/100
	var net float64
	switch mode {
	case "markup":
		den := 1 - f*(1+t)
		if den <= 0.0001 {
			return nil, ErrNoMargin
		}
		net = (cost*(1+pct/100) + m.FeeFixed) / den
	default:
		den := 1 - pct/100 - f*(1+t)
		if den <= 0.0001 {
			return nil, ErrNoMargin
		}
		net = (cost + m.FeeFixed) / den
	}
	p := &PricePoint{Cost: RoundCur(cost, m.Currency)}
	if m.PricesIncludeTax {
		p.Unrounded = RoundCur(net*(1+t), m.Currency)
		p.Gross = RoundUp(net*(1+t), m)
		p.Net = RoundCur(p.Gross/(1+t), m.Currency)
		p.Price = p.Gross
	} else {
		p.Unrounded = RoundCur(net, m.Currency)
		p.Net = RoundUp(net, m)
		p.Gross = RoundCur(p.Net*(1+t), m.Currency)
		p.Price = p.Net
	}
	p.Tax = RoundCur(p.Gross-p.Net, m.Currency)
	settle(p, cost, m)
	return p, nil
}

// settle works out fee, profit and margin for a price and a cost.
func settle(p *PricePoint, cost float64, m RetailMarket) {
	p.Fee = 0
	if p.Gross > 0 {
		p.Fee = RoundCur(p.Gross*m.FeePercent/100+m.FeeFixed, m.Currency)
	}
	p.Cost = RoundCur(cost, m.Currency)
	p.Profit = RoundCur(p.Net-p.Fee-p.Cost, m.Currency)
	p.MarginPct, p.MarkupPct = 0, 0
	if p.Net > 0 {
		p.MarginPct = math.Round(p.Profit/p.Net*1000) / 10
	}
	if p.Cost > 0 {
		p.MarkupPct = math.Round(p.Profit/p.Cost*1000) / 10
	}
}

// Place ranks a price among competitor prices (same currency).
func Place(price float64, others []float64, basis, cur string) Position {
	pos := Position{Basis: basis, Stats: stats(others, cur), Of: len(others) + 1, Rank: 1}
	if len(others) == 0 {
		return pos
	}
	for _, v := range others {
		if v < price-0.005 {
			pos.Rank++
		}
	}
	s := pos.Stats
	if s.Median > 0 {
		pos.VsMedianPct = math.Round((price/s.Median-1)*1000) / 10
	}
	what := map[string]string{"first_year": "first-year", "ongoing": "renewal"}[basis]
	vs := fmt.Sprintf("%.0f%% %s the median", math.Abs(pos.VsMedianPct), map[bool]string{true: "above", false: "below"}[pos.VsMedianPct > 0])
	if pos.VsMedianPct == 0 {
		vs = "at the median"
	}
	switch {
	case price < s.Min-0.005:
		pos.Where = "cheapest"
		pos.Text = fmt.Sprintf("Cheaper than every competitor's %s price (cheapest %s) — %s.", what, money(s.Min, cur), vs)
	case price <= s.Median+0.005:
		pos.Where = "below_median"
		pos.Text = fmt.Sprintf("Between the cheapest (%s) and the median (%s) %s price — %s.", money(s.Min, cur), money(s.Median, cur), what, vs)
	case price <= s.Max+0.005:
		pos.Where = "above_median"
		pos.Text = fmt.Sprintf("Between the median (%s) and the most expensive (%s) %s price — %s.", money(s.Median, cur), money(s.Max, cur), what, vs)
	default:
		pos.Where = "most_expensive"
		pos.Text = fmt.Sprintf("Dearer than every competitor's %s price (most expensive %s) — %s.", what, money(s.Max, cur), vs)
	}
	if len(others) == 1 {
		pos.Text = strings.Replace(pos.Text, "every competitor's", "the one competitor's", 1)
	}
	return pos
}

// Normalize fills the defaults.
func (in RetailInput) Normalize(home string) RetailInput {
	out := in
	if out.Basis != "first_year" && out.Basis != "ongoing" {
		out.Basis = "higher"
	}
	if out.Mode != "markup" {
		out.Mode = "margin"
	}
	if out.Percent < 0 || math.IsNaN(out.Percent) {
		out.Percent = 0
	}
	if out.Shock <= 0 || out.Shock > 90 {
		out.Shock = 10
	}
	ms := []RetailMarket{}
	seen := map[string]bool{}
	for _, m := range in.Markets {
		m.Currency = strings.ToUpper(strings.TrimSpace(m.Currency))
		if !currency.IsSupported(m.Currency) || seen[m.Currency] {
			continue
		}
		seen[m.Currency] = true
		if m.TaxRate < 0 {
			m.TaxRate = 0
		}
		if m.FeePercent < 0 {
			m.FeePercent = 0
		}
		if m.FeeFixed < 0 {
			m.FeeFixed = 0
		}
		if m.Rounding != "none" && m.Rounding != "step" {
			m.Rounding = "friendly"
		}
		ms = append(ms, m)
	}
	if len(ms) == 0 {
		ms = append(ms, RetailMarket{Currency: home, Rounding: "friendly"})
	}
	out.Markets = ms
	return out
}

// Retail prices an option. market holds the competitors' prices for the same
// combo (nil when there are none); in must be normalised.
func Retail(o *Option, benchmarks []Option, in RetailInput, r Rates) *RetailResult {
	res := &RetailResult{Home: r.Home, Key: o.Key, Label: o.Label, Basis: in.Basis, Mode: in.Mode, Percent: in.Percent, Shock: in.Shock,
		FirstYearH: o.FirstYearH, OngoingH: o.OngoingH, Markets: []RetailPrice{}, Warnings: []string{}}
	res.BasisUsed = in.Basis
	if in.Basis == "higher" {
		res.BasisUsed = "ongoing"
		if o.FirstYearH > o.OngoingH {
			res.BasisUsed = "first_year"
		}
	}
	first := res.BasisUsed == "first_year"
	res.CostHome = o.OngoingH
	if first {
		res.CostHome = o.FirstYearH
	}
	if o.PromoTrap && first {
		res.Warnings = append(res.Warnings, "Priced on the first year, which is cheaper than the renewal — the margin drops from year 2 unless you raise the price.")
	}
	if !o.Complete {
		res.Warnings = append(res.Warnings, "This option is incomplete ("+strings.Join(o.Missing, ", ")+" missing) — the price only covers what is listed.")
	}
	notes := []string{}
	for _, p := range o.Parts() {
		if p.Included {
			continue
		}
		a := p.Ongoing
		if first {
			a = p.FirstYear + p.Setup
		}
		notes = append(notes, fmt.Sprintf("%s %s (%s)", strings.ToLower(p.Role), money(a, p.Currency), p.SupplierName))
	}
	res.CostNote = fmt.Sprintf("Cost %s a year (%s)", money(res.CostHome, r.Home), strings.Join(notes, ", "))

	for _, m := range in.Markets {
		rp := RetailPrice{Market: m, Rate: r.Rate(m.Currency), Sensitivity: []Sensitivity{}, Positions: []Position{}}
		if rp.Rate <= 0 {
			rp.Error = "No exchange rate for " + m.Currency
			res.Markets = append(res.Markets, rp)
			continue
		}
		// The cost in this market's currency, split into what is already in
		// it and what moves with exchange rates.
		fixed, moving := 0.0, 0.0
		for _, p := range o.Parts() {
			if p.Included {
				continue
			}
			a, h := p.Ongoing, p.OngoingH
			if first {
				a, h = p.FirstYear+p.Setup, p.FirstYearH+p.SetupH
			}
			var x float64
			switch {
			case p.Currency == m.Currency:
				x = a
			case m.Currency == r.Home:
				x = h
			default:
				x = r.Convert(a, p.Currency, m.Currency)
			}
			for _, sh := range p.Split {
				if sh.Currency == m.Currency {
					fixed += x * sh.Share
				} else {
					moving += x * sh.Share
				}
			}
		}
		rp.FixedCost, rp.MovingCost = RoundCur(fixed, m.Currency), RoundCur(moving, m.Currency)
		rp.Cost = RoundCur(fixed+moving, m.Currency)
		y, err := PriceFor(fixed+moving, in.Mode, in.Percent, m)
		if err != nil {
			rp.Error = err.Error()
			res.Markets = append(res.Markets, rp)
			continue
		}
		y.ProfitHome = r.ToHome(y.Profit, m.Currency)
		rp.Yearly = y
		if mo, err := PriceFor((fixed+moving)/12, in.Mode, in.Percent, m); err == nil {
			mo.ProfitHome = r.ToHome(mo.Profit, m.Currency)
			rp.Monthly = mo
		}
		for _, s := range []float64{in.Shock, -in.Shock} {
			c := fixed + moving*(1+s/100)
			pp := *y
			settle(&pp, c, m)
			rp.Sensitivity = append(rp.Sensitivity, Sensitivity{ShockPct: s, Cost: pp.Cost, Profit: pp.Profit, MarginPct: pp.MarginPct, ProfitHome: r.ToHome(pp.Profit, m.Currency)})
		}
		// Against the competitors.
		fy, og := []float64{}, []float64{}
		for _, b := range benchmarks {
			if !b.Complete {
				continue
			}
			fy = append(fy, RoundCur(b.FirstYearH/rp.Rate, m.Currency))
			og = append(og, RoundCur(b.OngoingH/rp.Rate, m.Currency))
		}
		if len(fy) > 0 {
			sort.Float64s(fy)
			sort.Float64s(og)
			rp.Positions = append(rp.Positions, Place(y.Price, fy, "first_year", m.Currency), Place(y.Price, og, "ongoing", m.Currency))
		}
		res.Markets = append(res.Markets, rp)
	}
	return res
}
