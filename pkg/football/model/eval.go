package model

import (
	"math"
)

// Brier is the multi-class Brier score Σ_k (p_k − o_k)² over H/D/A
// (0 = perfect, 2 = worst; a uniform forecast scores 0.667). outcome 0=H 1=D 2=A.
func Brier(p Probs, outcome int) float64 {
	o := [3]float64{}
	if outcome >= 0 && outcome <= 2 {
		o[outcome] = 1
	}
	return (p.H-o[0])*(p.H-o[0]) + (p.D-o[1])*(p.D-o[1]) + (p.A-o[2])*(p.A-o[2])
}

// RPS is the ranked probability score (Epstein 1969), the scoring rule
// Constantinou & Fenton (2012) recommend for football because H/D/A are
// ordered: ½·[(P_H − O_H)² + (P_H+P_D − O_H−O_D)²]. 0 = perfect, 1 = worst.
func RPS(p Probs, outcome int) float64 {
	o := [3]float64{}
	if outcome >= 0 && outcome <= 2 {
		o[outcome] = 1
	}
	c1 := p.H - o[0]
	c2 := p.H + p.D - o[0] - o[1]
	return (c1*c1 + c2*c2) / 2
}

// LogLoss is −ln p(outcome), with p floored at 1e-15.
func LogLoss(p Probs, outcome int) float64 {
	return -math.Log(math.Max(p.At(outcome), 1e-15))
}

// CalBin is one reliability-diagram bin: forecasts with Lo ≤ p < Hi (the
// last bin includes 1), their mean forecast, the observed frequency, the
// count and a 95% Wilson interval for the frequency.
type CalBin struct {
	Lo, Hi, MeanP, Freq float64
	N                   int
	CILo, CIHi          float64
}

// Calibration bins forecasts ps (of binary events hits) into `bins`
// equal-width bins (default 10). Empty bins are left out; NaN forecasts are
// skipped.
func Calibration(ps []float64, hits []bool, bins int) []CalBin {
	if bins <= 0 {
		bins = 10
	}
	sumP := make([]float64, bins)
	cnt := make([]int, bins)
	hit := make([]int, bins)
	for i, p := range ps {
		if i >= len(hits) || math.IsNaN(p) {
			continue
		}
		p = clamp(p, 0, 1)
		b := int(p * float64(bins))
		if b >= bins {
			b = bins - 1
		}
		sumP[b] += p
		cnt[b]++
		if hits[i] {
			hit[b]++
		}
	}
	var out []CalBin
	for b := 0; b < bins; b++ {
		if cnt[b] == 0 {
			continue
		}
		n := float64(cnt[b])
		fq := float64(hit[b]) / n
		lo, hi := wilson(hit[b], cnt[b])
		out = append(out, CalBin{Lo: float64(b) / float64(bins), Hi: float64(b+1) / float64(bins),
			MeanP: sumP[b] / n, Freq: fq, N: cnt[b], CILo: lo, CIHi: hi})
	}
	return out
}

// ECE is the expected calibration error: Σ N_b·|MeanP_b − Freq_b| / Σ N_b.
func ECE(bins []CalBin) float64 {
	s, n := 0.0, 0
	for _, b := range bins {
		s += float64(b.N) * math.Abs(b.MeanP-b.Freq)
		n += b.N
	}
	if n == 0 {
		return 0
	}
	return s / float64(n)
}

func wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959963984540054
	p := float64(k) / float64(n)
	nn := float64(n)
	den := 1 + z*z/nn
	c := (p + z*z/(2*nn)) / den
	h := z * math.Sqrt(p*(1-p)/nn+z*z/(4*nn*nn)) / den
	return math.Max(0, c-h), math.Min(1, c+h)
}

// Scores aggregates the three scoring rules over a set of forecasts.
type Scores struct {
	Name    string
	N       int
	Brier   float64
	RPS     float64
	LogLoss float64
}

// Score computes mean Brier, RPS and log loss of forecasts against outcomes.
func Score(name string, ps []Probs, outcomes []int) Scores {
	s := Scores{Name: name}
	for i, p := range ps {
		if i >= len(outcomes) {
			break
		}
		s.Brier += Brier(p, outcomes[i])
		s.RPS += RPS(p, outcomes[i])
		s.LogLoss += LogLoss(p, outcomes[i])
		s.N++
	}
	if s.N > 0 {
		s.Brier /= float64(s.N)
		s.RPS /= float64(s.N)
		s.LogLoss /= float64(s.N)
	}
	return s
}

// meanSE returns the mean and its standard error.
func meanSE(x []float64) (mean, se float64) {
	n := float64(len(x))
	if n == 0 {
		return 0, 0
	}
	for _, v := range x {
		mean += v
	}
	mean /= n
	if n < 2 {
		return mean, 0
	}
	ss := 0.0
	for _, v := range x {
		ss += (v - mean) * (v - mean)
	}
	return mean, math.Sqrt(ss / (n - 1) / n)
}
