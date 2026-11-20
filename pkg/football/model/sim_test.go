package model

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// simLeague holds the true parameters of a simulated league.
type simLeague struct {
	teams     []string
	att, def  map[string]float64 // sum-to-zero
	intercept float64
	home      float64
	rho       float64
}

// newSimLeague draws team strengths with a fixed seed. Team names are
// invented.
func newSimLeague(nTeams int, seed int64) simLeague {
	r := rand.New(rand.NewSource(seed))
	l := simLeague{att: map[string]float64{}, def: map[string]float64{}, intercept: 0.12, home: 0.25, rho: -0.10}
	var as, ds []float64
	for i := 0; i < nTeams; i++ {
		as = append(as, 0.28*r.NormFloat64())
		ds = append(ds, 0.24*r.NormFloat64())
	}
	center(as)
	center(ds)
	for i := 0; i < nTeams; i++ {
		t := fmt.Sprintf("T%02d", i+1)
		l.teams = append(l.teams, t)
		l.att[t] = as[i]
		l.def[t] = ds[i]
	}
	return l
}

func (l simLeague) rates(h, a string) (float64, float64) {
	return math.Exp(l.intercept + l.home + l.att[h] - l.def[a]), math.Exp(l.intercept + l.att[a] - l.def[h])
}

func (l simLeague) probs(h, a string) Probs {
	lh, la := l.rates(h, a)
	return PredictFromRates(lh, la, l.rho, 10).Probs()
}

// sampleScore draws a score from the exact DC distribution by inverse CDF.
func sampleScore(r *rand.Rand, lh, la, rho float64) (int, int) {
	g := ScoreGrid(lh, la, rho, 25)
	u := r.Float64()
	acc := 0.0
	for x := range g {
		for y := range g[x] {
			acc += g[x][y]
			if u < acc {
				return x, y
			}
		}
	}
	return 0, 0
}

// simulate plays `seasons` double round robins starting at start, one round
// per week (all matches of a round on the same day).
func (l simLeague) simulate(seasons int, start time.Time, seed int64) []Match {
	r := rand.New(rand.NewSource(seed))
	n := len(l.teams)
	var ms []Match
	id := 0
	day := start
	for s := 0; s < seasons; s++ {
		rounds := roundRobin(n)
		// second half: reversed venues
		all := append([][][2]int{}, rounds...)
		for _, rd := range rounds {
			var rev [][2]int
			for _, p := range rd {
				rev = append(rev, [2]int{p[1], p[0]})
			}
			all = append(all, rev)
		}
		for _, rd := range all {
			for _, p := range rd {
				h, a := l.teams[p[0]], l.teams[p[1]]
				lh, la := l.rates(h, a)
				x, y := sampleScore(r, lh, la, l.rho)
				id++
				ms = append(ms, Match{ID: fmt.Sprintf("m%05d", id), Date: day, Home: h, Away: a, HG: x, AG: y, League: "SIM"})
			}
			day = day.AddDate(0, 0, 7)
		}
		day = day.AddDate(0, 0, 70) // summer break
	}
	return ms
}

// roundRobin returns n-1 rounds of n/2 pairings (circle method), n even.
func roundRobin(n int) [][][2]int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	var rounds [][][2]int
	for r := 0; r < n-1; r++ {
		var rd [][2]int
		for i := 0; i < n/2; i++ {
			a, b := idx[i], idx[n-1-i]
			if (r+i)%2 == 0 {
				rd = append(rd, [2]int{a, b})
			} else {
				rd = append(rd, [2]int{b, a})
			}
		}
		rounds = append(rounds, rd)
		// rotate all but the first
		last := idx[n-1]
		copy(idx[2:], idx[1:n-1])
		idx[1] = last
	}
	return rounds
}

// marketOdds turns true probabilities into bookmaker odds with a
// proportional margin.
func marketOdds(p Probs, margin float64) [3]float64 {
	f := 1 + margin
	return [3]float64{1 / (p.H * f), 1 / (p.D * f), 1 / (p.A * f)}
}

func corr(a, b []float64) float64 {
	n := float64(len(a))
	ma, mb := 0.0, 0.0
	for i := range a {
		ma += a[i]
		mb += b[i]
	}
	ma /= n
	mb /= n
	sab, saa, sbb := 0.0, 0.0, 0.0
	for i := range a {
		sab += (a[i] - ma) * (b[i] - mb)
		saa += (a[i] - ma) * (a[i] - ma)
		sbb += (b[i] - mb) * (b[i] - mb)
	}
	return sab / math.Sqrt(saa*sbb)
}
