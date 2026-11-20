package model

import (
	"errors"
	"math"
	"sort"
	"time"
)

// Match is one played (or, for prediction, scheduled) match. Home and Away
// are opaque team ids. HG/AG are full-time goals (90 minutes plus stoppage
// time; extra time and penalties are not part of the model). HxG/AxG are
// expected goals when the provider has them.
type Match struct {
	ID       string
	Date     time.Time
	Home     string
	Away     string
	HG, AG   int
	Neutral  bool
	HxG, AxG *float64
	League   string
}

// Probs is a three-way (home / draw / away) forecast.
type Probs struct{ H, D, A float64 }

// Outcome codes used by the scoring rules.
const (
	OutcomeHome = 0
	OutcomeDraw = 1
	OutcomeAway = 2
)

// Outcome returns 0 (home win), 1 (draw) or 2 (away win) for a score.
func Outcome(hg, ag int) int {
	switch {
	case hg > ag:
		return OutcomeHome
	case hg == ag:
		return OutcomeDraw
	default:
		return OutcomeAway
	}
}

// Slice returns the probabilities as [H, D, A].
func (p Probs) Slice() []float64 { return []float64{p.H, p.D, p.A} }

// At returns the probability of outcome k (0=H, 1=D, 2=A).
func (p Probs) At(k int) float64 {
	switch k {
	case OutcomeHome:
		return p.H
	case OutcomeDraw:
		return p.D
	default:
		return p.A
	}
}

// Normalize rescales the probabilities to sum to 1 (negative or NaN values
// count as 0). An all-zero input becomes the uniform forecast.
func (p Probs) Normalize() Probs {
	h, d, a := clean(p.H), clean(p.D), clean(p.A)
	s := h + d + a
	if s <= 0 {
		return Probs{1.0 / 3, 1.0 / 3, 1.0 / 3}
	}
	return Probs{h / s, d / s, a / s}
}

// Valid reports whether the probabilities are finite, non-negative and sum to
// 1 within 1e-6.
func (p Probs) Valid() bool {
	for _, v := range []float64{p.H, p.D, p.A} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return false
		}
	}
	return math.Abs(p.H+p.D+p.A-1) < 1e-6
}

// ErrNoData is returned when there is nothing to fit.
var ErrNoData = errors.New("model: no matches to fit")

func clean(v float64) float64 {
	if math.IsNaN(v) || v < 0 || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// poissonPMF returns P(X=k) for k = 0..n-1 for a Poisson(lambda) variable,
// computed by recurrence (stable for the small means met in football).
func poissonPMF(lambda float64, n int) []float64 {
	out := make([]float64, n)
	if n == 0 {
		return out
	}
	if lambda <= 0 {
		out[0] = 1
		return out
	}
	out[0] = math.Exp(-lambda)
	for k := 1; k < n; k++ {
		out[k] = out[k-1] * lambda / float64(k)
	}
	return out
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func dayFloor(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
