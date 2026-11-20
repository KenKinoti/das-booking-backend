package model

import (
	"math"
	"sort"
	"strconv"
)

// Scoreline is one exact score and its probability.
type Scoreline struct {
	H, A int
	P    float64
}

// AHOutcome is how an Asian-handicap bet on the home side settles. Quarter
// lines split the stake over the two neighbouring lines, so half-wins and
// half-losses appear; integer lines can push (stake back).
type AHOutcome struct {
	Win, HalfWin, Push, HalfLoss, Loss float64
	// P is the push-aware "effective" probability: 1/FairOdds, i.e. the win
	// probability at which a bet at decimal odds o has EV = 0. For half-goal
	// lines it equals Win. EV of a bet at odds o is
	// (Win+HalfWin/2)(o−1) − (Loss+HalfLoss/2).
	P        float64
	FairOdds float64
}

// Prediction is the full forecast from a goals model.
type Prediction struct {
	LambdaHome, LambdaAway float64
	Matrix                 [][]float64 // [home goals][away goals], 0..MaxGoals, sums to 1
	PHome, PDraw, PAway    float64
	Over                   map[string]float64 // "0.5".."5.5" → P(total goals > line)
	BTTS                   float64
	TopScores              []Scoreline        // most likely 6
	AHHome                 map[string]float64 // "-1.5".."+1.5" (quarter steps) → push-aware P(home covers); away at the mirrored line = 1 − value

	Rho            float64
	AH             map[string]AHOutcome // full settlement breakdown per line (home side)
	HomeCleanSheet float64              // P(away scores 0)
	AwayCleanSheet float64              // P(home scores 0)
	ExpGoalsHome   float64              // mean of the (ρ-adjusted) score distribution
	ExpGoalsAway   float64
	MatrixTail     float64  // probability mass beyond MaxGoals that was folded in by renormalising Matrix
	Unknown        []string // teams not in the fitted model (rated at the newcomer prior)
}

// Probs returns the 1X2 part.
func (p Prediction) Probs() Probs { return Probs{p.PHome, p.PDraw, p.PAway} }

// OverLines are the total-goals lines reported in Prediction.Over.
var OverLines = []float64{0.5, 1.5, 2.5, 3.5, 4.5, 5.5}

// AHLines are the Asian-handicap lines reported in Prediction.AHHome.
var AHLines = []float64{-1.5, -1.25, -1, -0.75, -0.5, -0.25, 0, 0.25, 0.5, 0.75, 1, 1.25, 1.5}

// LineKey formats a total-goals line ("2.5").
func LineKey(line float64) string { return strconv.FormatFloat(line, 'f', -1, 64) }

// AHKey formats a handicap line with an explicit sign: "-0.75", "0", "+1".
func AHKey(line float64) string {
	if line == 0 {
		return "0"
	}
	s := strconv.FormatFloat(line, 'f', -1, 64)
	if line > 0 {
		return "+" + s
	}
	return s
}

// DCTau is the Dixon–Coles low-score correction factor τ(x, y).
func DCTau(x, y int, lh, la, rho float64) float64 {
	switch {
	case x == 0 && y == 0:
		return 1 - lh*la*rho
	case x == 0 && y == 1:
		return 1 + lh*rho
	case x == 1 && y == 0:
		return 1 + la*rho
	case x == 1 && y == 1:
		return 1 - rho
	}
	return 1
}

// ScoreGrid returns the exact Dixon–Coles score probabilities for 0..n-1
// goals per side (not renormalised; the mass beyond is 1 − sum). τ factors
// below zero (only possible for extreme ρ·λ·μ) are clamped to 0.
func ScoreGrid(lh, la, rho float64, n int) [][]float64 {
	px := poissonPMF(lh, n)
	py := poissonPMF(la, n)
	m := make([][]float64, n)
	for x := 0; x < n; x++ {
		m[x] = make([]float64, n)
		for y := 0; y < n; y++ {
			v := px[x] * py[y]
			if x <= 1 && y <= 1 {
				t := DCTau(x, y, lh, la, rho)
				if t < 0 {
					t = 0
				}
				v *= t
			}
			m[x][y] = v
		}
	}
	return m
}

// PredictFromRates builds a Prediction from Poisson means and ρ. Markets are
// computed on a grid large enough that the truncated mass is < 1e-12; the
// returned Matrix is cut at maxGoals and renormalised to sum to 1.
func PredictFromRates(lh, la, rho float64, maxGoals int) Prediction {
	if maxGoals <= 0 {
		maxGoals = 10
	}
	if !(lh > 0) || math.IsInf(lh, 0) {
		lh = 1e-9
	}
	if !(la > 0) || math.IsInf(la, 0) {
		la = 1e-9
	}
	if math.IsNaN(rho) {
		rho = 0
	}
	lmax := math.Max(lh, la)
	n := int(lmax+12*math.Sqrt(lmax)) + 12
	if n < maxGoals+1 {
		n = maxGoals + 1
	}
	if n > 200 {
		n = 200
	}
	full := ScoreGrid(lh, la, rho, n)
	total := 0.0
	for x := range full {
		for y := range full[x] {
			total += full[x][y]
		}
	}
	// τ clamping (extreme parameters only) can make the total differ from 1.
	if total > 0 && math.Abs(total-1) > 1e-12 {
		for x := range full {
			for y := range full[x] {
				full[x][y] /= total
			}
		}
	}
	pr := Prediction{LambdaHome: lh, LambdaAway: la, Rho: rho,
		Over: map[string]float64{}, AHHome: map[string]float64{}, AH: map[string]AHOutcome{}}
	totals := make([]float64, 2*n)
	margins := make(map[int]float64)
	for x := 0; x < n; x++ {
		for y := 0; y < n; y++ {
			v := full[x][y]
			switch {
			case x > y:
				pr.PHome += v
			case x == y:
				pr.PDraw += v
			default:
				pr.PAway += v
			}
			if x > 0 && y > 0 {
				pr.BTTS += v
			}
			if y == 0 {
				pr.HomeCleanSheet += v
			}
			if x == 0 {
				pr.AwayCleanSheet += v
			}
			pr.ExpGoalsHome += float64(x) * v
			pr.ExpGoalsAway += float64(y) * v
			totals[x+y] += v
			margins[x-y] += v
		}
	}
	for _, line := range OverLines {
		under := 0.0
		for t := 0; float64(t) < line; t++ {
			under += totals[t]
		}
		pr.Over[LineKey(line)] = clamp(1-under, 0, 1)
	}
	for _, line := range AHLines {
		o := AsianHandicap(margins, line)
		pr.AH[AHKey(line)] = o
		pr.AHHome[AHKey(line)] = o.P
	}
	// Display matrix.
	m := make([][]float64, maxGoals+1)
	in := 0.0
	for x := 0; x <= maxGoals; x++ {
		m[x] = make([]float64, maxGoals+1)
		for y := 0; y <= maxGoals; y++ {
			m[x][y] = full[x][y]
			in += full[x][y]
		}
	}
	pr.MatrixTail = clamp(1-in, 0, 1)
	if in > 0 {
		for x := range m {
			for y := range m[x] {
				m[x][y] /= in
			}
		}
	}
	pr.Matrix = m
	pr.TopScores = TopScorelines(m, 6)
	return pr
}

// TopScorelines returns the k most likely scores in a matrix (ties broken by
// fewer total goals, then home goals) — deterministic.
func TopScorelines(m [][]float64, k int) []Scoreline {
	all := make([]Scoreline, 0, len(m)*len(m))
	for x := range m {
		for y := range m[x] {
			all = append(all, Scoreline{H: x, A: y, P: m[x][y]})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].P != all[j].P {
			return all[i].P > all[j].P
		}
		if all[i].H+all[i].A != all[j].H+all[j].A {
			return all[i].H+all[i].A < all[j].H+all[j].A
		}
		return all[i].H < all[j].H
	})
	if k > len(all) {
		k = len(all)
	}
	return all[:k]
}

// AsianHandicap settles a home-side handicap bet over the distribution of
// goal margins (home − away → probability). Quarter lines are split into the
// two neighbouring half/whole lines with half the stake each.
func AsianHandicap(margins map[int]float64, line float64) AHOutcome {
	q := math.Round(line * 4)
	parts := []float64{line, line}
	if int(q)%2 != 0 { // quarter line
		parts = []float64{line - 0.25, line + 0.25}
	}
	var o AHOutcome
	for _, mg := range sortedIntKeys(margins) {
		p := margins[mg]
		score := 0
		for _, l := range parts {
			v := float64(mg) + l
			switch {
			case v > 1e-9:
				score++
			case v < -1e-9:
				score--
			}
		}
		switch score {
		case 2:
			o.Win += p
		case 1:
			o.HalfWin += p
		case 0:
			o.Push += p
		case -1:
			o.HalfLoss += p
		default:
			o.Loss += p
		}
	}
	good := o.Win + o.HalfWin/2
	bad := o.Loss + o.HalfLoss/2
	if good+bad > 0 {
		o.P = good / (good + bad)
	}
	if good > 0 {
		o.FairOdds = 1 + bad/good
	}
	return o
}

func sortedIntKeys(m map[int]float64) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}
