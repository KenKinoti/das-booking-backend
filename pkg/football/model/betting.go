package model

import (
	"math"
)

// Default staking constants (the product's responsible-gambling defaults).
const (
	DefaultKellyFraction = 0.25 // quarter Kelly
	DefaultKellyCap      = 0.02 // never more than 2% of bankroll on one bet
)

// DeVig method names.
const (
	DeVigProportional = "proportional"
	DeVigShin         = "shin"
	DeVigPower        = "power"
)

// DeVig removes the bookmaker margin from one market's decimal odds (all
// outcomes of the market, e.g. H/D/A) and returns the implied probabilities
// (summing to 1) and the margin (booksum − 1, e.g. 0.05 = 5%).
//
//   - "proportional" (multiplicative / basic normalisation): p_i = π_i / Σπ.
//   - "shin": Shin's (1993) insider-trading model, which assigns more of the
//     margin to long shots (favourite–longshot bias). With π_i = 1/o_i and
//     Π = Σπ_i, p_i(z) = (√(z² + 4(1−z)π_i²/Π) − z) / (2(1−z)), with z (the
//     share of insider money) solved so Σp_i = 1 (Štrumbelj 2014).
//   - "power": p_i = π_i^k with k solved so Σp_i = 1 (also long-shot heavy).
//
// Unknown methods fall back to proportional. Invalid input (fewer than two
// prices, a price ≤ 1, NaN/Inf) returns nil and margin 0. With a negative
// margin (an arbitrage across books) Shin falls back to proportional.
func DeVig(odds []float64, method string) (probs []float64, margin float64) {
	if len(odds) < 2 {
		return nil, 0
	}
	pi := make([]float64, len(odds))
	sum := 0.0
	for i, o := range odds {
		if !(o > 1) || math.IsInf(o, 0) || math.IsNaN(o) {
			return nil, 0
		}
		pi[i] = 1 / o
		sum += pi[i]
	}
	margin = sum - 1
	probs = make([]float64, len(odds))
	switch method {
	case DeVigShin:
		if sum <= 1 {
			break
		}
		z := shinZ(pi, sum)
		s := 0.0
		for i, p := range pi {
			probs[i] = (math.Sqrt(z*z+4*(1-z)*p*p/sum) - z) / (2 * (1 - z))
			s += probs[i]
		}
		for i := range probs {
			probs[i] /= s // exact to ~1e-15 already; guards rounding
		}
		return probs, margin
	case DeVigPower:
		k := powerK(pi)
		s := 0.0
		for i, p := range pi {
			probs[i] = math.Pow(p, k)
			s += probs[i]
		}
		for i := range probs {
			probs[i] /= s
		}
		return probs, margin
	}
	for i, p := range pi {
		probs[i] = p / sum
	}
	return probs, margin
}

// ShinZ returns Shin's z (estimated share of insider trading) for a market,
// or 0 when the margin is not positive or the odds are invalid.
func ShinZ(odds []float64) float64 {
	if len(odds) < 2 {
		return 0
	}
	pi := make([]float64, len(odds))
	sum := 0.0
	for i, o := range odds {
		if !(o > 1) {
			return 0
		}
		pi[i] = 1 / o
		sum += pi[i]
	}
	if sum <= 1 {
		return 0
	}
	return shinZ(pi, sum)
}

func shinZ(pi []float64, sum float64) float64 {
	f := func(z float64) float64 {
		s := 0.0
		for _, p := range pi {
			s += (math.Sqrt(z*z+4*(1-z)*p*p/sum) - z) / (2 * (1 - z))
		}
		return s - 1
	}
	lo, hi := 0.0, 0.999
	if f(hi) > 0 { // pathological margin; take the boundary
		return hi
	}
	for i := 0; i < 200 && hi-lo > 1e-15; i++ {
		mid := (lo + hi) / 2
		if f(mid) > 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

func powerK(pi []float64) float64 {
	f := func(k float64) float64 {
		s := 0.0
		for _, p := range pi {
			s += math.Pow(p, k)
		}
		return s - 1
	}
	// Σπ^k is decreasing in k (π < 1). Bracket the root.
	lo, hi := 0.01, 1.0
	for f(hi) > 0 && hi < 1e3 {
		lo = hi
		hi *= 2
	}
	for f(lo) < 0 && lo > 1e-6 {
		hi = lo
		lo /= 2
	}
	for i := 0; i < 200 && hi-lo > 1e-15*hi; i++ {
		mid := (lo + hi) / 2
		if f(mid) > 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// DeVig3 is DeVig for a 1X2 market. ok is false for invalid odds.
func DeVig3(odds [3]float64, method string) (p Probs, margin float64, ok bool) {
	ps, m := DeVig(odds[:], method)
	if ps == nil {
		return Probs{}, 0, false
	}
	return Probs{ps[0], ps[1], ps[2]}, m, true
}

// EV is the expected profit per unit staked: p·odds − 1. 0.05 = +5%.
func EV(p, decimalOdds float64) float64 {
	if math.IsNaN(p) || math.IsNaN(decimalOdds) {
		return 0
	}
	return p*decimalOdds - 1
}

// Kelly returns the stake as a fraction of bankroll: fraction × the Kelly
// (1956) optimum f* = (p·o − 1)/(o − 1), never negative, and at most
// capFraction when capFraction > 0. fraction is clamped to 0..1 (1 = full
// Kelly, which is far too aggressive when p is itself an estimate).
// Returns 0 for no edge, invalid odds (≤ 1) or invalid p.
func Kelly(p, decimalOdds, fraction, capFraction float64) float64 {
	if math.IsNaN(p) || math.IsNaN(decimalOdds) || math.IsNaN(fraction) || math.IsInf(decimalOdds, 0) {
		return 0
	}
	if !(decimalOdds > 1) || p <= 0 || fraction <= 0 {
		return 0
	}
	if p > 1 {
		p = 1
	}
	fraction = math.Min(fraction, 1)
	f := (p*decimalOdds - 1) / (decimalOdds - 1)
	if f <= 0 {
		return 0
	}
	s := fraction * f
	if capFraction > 0 && s > capFraction {
		s = capFraction
	}
	return s
}

// Blend is linear opinion pooling: the weighted average of the forecasts,
// renormalised. Each forecast is normalised first; invalid ones (NaN) are
// skipped. w nil/empty = equal weights; missing, negative or NaN weights
// count as 0; if every weight is 0 the forecasts are averaged equally.
func Blend(ps []Probs, w []float64) Probs {
	ws := blendWeights(ps, w)
	var out Probs
	tot := 0.0
	for i, p := range ps {
		if ws[i] == 0 {
			continue
		}
		q := p.Normalize()
		out.H += ws[i] * q.H
		out.D += ws[i] * q.D
		out.A += ws[i] * q.A
		tot += ws[i]
	}
	if tot == 0 {
		return Probs{1.0 / 3, 1.0 / 3, 1.0 / 3}
	}
	return out.Normalize()
}

// BlendLogLinear is logarithmic opinion pooling: p ∝ Π p_i^{w_i} with the
// weights normalised to sum to 1 (a geometric mean; it respects agreement
// between sources more than the linear pool). Probabilities are floored at
// 1e-9 before taking logs.
func BlendLogLinear(ps []Probs, w []float64) Probs {
	ws := blendWeights(ps, w)
	tot := 0.0
	for _, x := range ws {
		tot += x
	}
	if tot == 0 {
		return Probs{1.0 / 3, 1.0 / 3, 1.0 / 3}
	}
	var lh, ld, la float64
	for i, p := range ps {
		if ws[i] == 0 {
			continue
		}
		q := p.Normalize()
		a := ws[i] / tot
		lh += a * math.Log(math.Max(q.H, 1e-9))
		ld += a * math.Log(math.Max(q.D, 1e-9))
		la += a * math.Log(math.Max(q.A, 1e-9))
	}
	mx := math.Max(lh, math.Max(ld, la))
	return Probs{math.Exp(lh - mx), math.Exp(ld - mx), math.Exp(la - mx)}.Normalize()
}

func blendWeights(ps []Probs, w []float64) []float64 {
	ws := make([]float64, len(ps))
	any := false
	for i := range ps {
		v := 1.0
		if len(w) > 0 {
			v = 0
			if i < len(w) {
				v = w[i]
			}
		}
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			v = 0
		}
		p := ps[i]
		if math.IsNaN(p.H) || math.IsNaN(p.D) || math.IsNaN(p.A) {
			v = 0
		}
		ws[i] = v
		if v > 0 {
			any = true
		}
	}
	if !any {
		for i, p := range ps {
			if !(math.IsNaN(p.H) || math.IsNaN(p.D) || math.IsNaN(p.A)) {
				ws[i] = 1
			}
		}
	}
	return ws
}
