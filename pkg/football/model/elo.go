package model

import (
	"math"
	"sort"
	"time"
)

// EloOptions configures an Elo rating system.
//
// The zero value EloOptions{} means DefaultEloOptions(). Otherwise K ≤ 0 and
// Init ≤ 0 fall back to the defaults and DrawModel "" means "davidson";
// HomeAdv and the booleans are taken literally.
type EloOptions struct {
	K             float64 // update speed (default 15)
	HomeAdv       float64 // home advantage in rating points (default 80)
	Init          float64 // rating of a team the first time it is seen (default 1500)
	GoalDiffScale bool    // World Football Elo multiplier: 1, 1.5, (11+gd)/8 for gd = ≤1, 2, ≥3
	// DrawModel turns a rating difference into P(H/D/A):
	//   "davidson" (default): Davidson (1970) ties model with a fitted scale,
	//              home shift and tie parameter ν;
	//   "ordlogit": ordered logit with a fitted slope and two cut points.
	// Both are fitted by maximum likelihood on the matches this Elo has seen
	// (once both teams have ≥ MinGames), so the draw rate is the league's own.
	DrawModel string

	// NewTeamOffset is added to the mean rating of the currently active teams
	// to initialise a team first seen after the league is established
	// (promoted teams; default -50). 0 = start at Init. Ignored until at
	// least four teams have played in the last 400 days.
	NewTeamOffset float64
	// Window is the number of most recent matches used to fit the draw model
	// (default 3000). MinGames is how many games both teams need before a
	// match is used for that fit (default 10).
	Window   int
	MinGames int
}

// DefaultEloOptions returns the defaults chosen by the EPL tuning (README.md).
func DefaultEloOptions() EloOptions {
	return EloOptions{K: 15, HomeAdv: 80, Init: 1500, GoalDiffScale: true, DrawModel: "davidson", NewTeamOffset: -50}
}

func (o EloOptions) normalized() EloOptions {
	if o == (EloOptions{}) {
		o = DefaultEloOptions()
	}
	d := DefaultEloOptions()
	if o.K <= 0 || math.IsNaN(o.K) {
		o.K = d.K
	}
	if o.Init <= 0 || math.IsNaN(o.Init) {
		o.Init = d.Init
	}
	if math.IsNaN(o.HomeAdv) {
		o.HomeAdv = 0
	}
	if o.DrawModel != "ordlogit" {
		o.DrawModel = "davidson"
	}
	if o.Window <= 0 {
		o.Window = 3000
	}
	if o.MinGames <= 0 {
		o.MinGames = 10
	}
	return o
}

type eloObs struct {
	dr      float64 // pre-match rating difference incl. home advantage
	outcome int
}

// Elo is a running Elo rating system. Feed it matches in chronological order
// with Update; ask for forecasts with Probabilities. Not safe for concurrent
// use.
type Elo struct {
	opt      EloOptions
	ratings  map[string]float64
	games    map[string]int
	lastSeen map[string]time.Time
	obs      []eloObs
	n, draws int
	// fitted draw model
	dirty    bool
	params   [3]float64 // davidson: log ν, scale, shift ; ordlogit: slope, c1, log(c2−c1)
	fitted   bool
	totalObs int // observations ever recorded
	fitAt    int // totalObs at the last fit
	last     time.Time
}

// NewElo creates an empty rating system.
func NewElo(o EloOptions) *Elo {
	return &Elo{opt: o.normalized(), ratings: map[string]float64{}, games: map[string]int{},
		lastSeen: map[string]time.Time{}, dirty: true}
}

// Options returns the effective options.
func (e *Elo) Options() EloOptions { return e.opt }

func (e *Elo) ensure(team string, at time.Time) {
	if _, ok := e.ratings[team]; ok {
		return
	}
	r := e.opt.Init
	if e.opt.NewTeamOffset != 0 && len(e.ratings) >= 4 {
		// Mean of teams active in the last 400 days.
		s, k := 0.0, 0
		for _, t := range sortedKeys(e.ratings) {
			if at.Sub(e.lastSeen[t]) < 400*24*time.Hour {
				s += e.ratings[t]
				k++
			}
		}
		if k >= 4 {
			r = s/float64(k) + e.opt.NewTeamOffset
		}
	}
	e.ratings[team] = r
}

// Rating returns a team's current rating (Init for an unknown team).
func (e *Elo) Rating(team string) float64 {
	if r, ok := e.ratings[team]; ok {
		return r
	}
	return e.opt.Init
}

// Ratings returns a copy of all ratings.
func (e *Elo) Ratings() map[string]float64 {
	out := make(map[string]float64, len(e.ratings))
	for k, v := range e.ratings {
		out[k] = v
	}
	return out
}

// Games returns how many matches a team has been rated on.
func (e *Elo) Games(team string) int { return e.games[team] }

// DrawRate is the share of draws among the matches seen.
func (e *Elo) DrawRate() float64 {
	if e.n == 0 {
		return 0
	}
	return float64(e.draws) / float64(e.n)
}

// Diff returns the rating difference used for a fixture (home advantage in).
func (e *Elo) Diff(home, away string, neutral bool) float64 {
	dr := e.Rating(home) - e.Rating(away)
	if !neutral {
		dr += e.opt.HomeAdv
	}
	return dr
}

// Expected is the classical Elo expected score of the home side.
func Expected(dr float64) float64 { return 1 / (1 + math.Pow(10, -dr/400)) }

// GoalDiffMultiplier is the World Football Elo K multiplier.
func GoalDiffMultiplier(gd int) float64 {
	if gd < 0 {
		gd = -gd
	}
	switch {
	case gd <= 1:
		return 1
	case gd == 2:
		return 1.5
	default:
		return (11 + float64(gd)) / 8
	}
}

// Update applies one result. Call in chronological order.
func (e *Elo) Update(m Match) {
	if m.Home == "" || m.Away == "" || m.Home == m.Away || m.HG < 0 || m.AG < 0 {
		return
	}
	e.ensure(m.Home, m.Date)
	e.ensure(m.Away, m.Date)
	dr := e.Diff(m.Home, m.Away, m.Neutral)
	out := Outcome(m.HG, m.AG)
	if e.games[m.Home] >= e.opt.MinGames && e.games[m.Away] >= e.opt.MinGames {
		e.obs = append(e.obs, eloObs{dr, out})
		e.totalObs++
		if len(e.obs) > 2*e.opt.Window {
			e.obs = append([]eloObs(nil), e.obs[len(e.obs)-e.opt.Window:]...)
		}
		e.dirty = true
	}
	w := 1.0
	switch out {
	case OutcomeDraw:
		w = 0.5
		e.draws++
	case OutcomeAway:
		w = 0
	}
	e.n++
	k := e.opt.K
	if e.opt.GoalDiffScale {
		k *= GoalDiffMultiplier(m.HG - m.AG)
	}
	delta := k * (w - Expected(dr))
	e.ratings[m.Home] += delta
	e.ratings[m.Away] -= delta
	e.games[m.Home]++
	e.games[m.Away]++
	e.lastSeen[m.Home] = m.Date
	e.lastSeen[m.Away] = m.Date
	if m.Date.After(e.last) {
		e.last = m.Date
	}
}

// Probabilities returns P(home win), P(draw), P(away win) from the current
// ratings and the fitted draw model.
func (e *Elo) Probabilities(home, away string, neutral bool) (pH, pD, pA float64) {
	e.fit()
	p := e.probsFromDiff(e.Diff(home, away, neutral), e.params)
	return p.H, p.D, p.A
}

// DrawModelParams returns the fitted draw-model parameters (davidson: ν,
// scale, shift; ordlogit: slope per 400 points, c1, c2) and the number of
// matches they were fitted on.
func (e *Elo) DrawModelParams() (a, b, c float64, n int) {
	e.fit()
	w := e.window()
	if e.opt.DrawModel == "ordlogit" {
		return e.params[0], e.params[1], e.params[1] + math.Exp(e.params[2]), len(w)
	}
	return math.Exp(e.params[0]), e.params[1], e.params[2], len(w)
}

func (e *Elo) defaultParams() [3]float64 {
	if e.opt.DrawModel == "ordlogit" {
		// slope ≈ Elo's logistic, cut points giving ~26% draws at dr = 0.
		return [3]float64{math.Ln10, -0.55, math.Log(1.1)}
	}
	return [3]float64{math.Log(0.7), 1, 0}
}

func (e *Elo) probsFromDiff(dr float64, th [3]float64) Probs {
	if e.opt.DrawModel == "ordlogit" {
		s := dr / 400
		c1 := th[1]
		c2 := c1 + math.Exp(th[2])
		pA := logistic(c1 - th[0]*s)
		pAD := logistic(c2 - th[0]*s)
		return Probs{H: 1 - pAD, D: pAD - pA, A: pA}.Normalize()
	}
	nu := math.Exp(th[0])
	z := th[1] * (dr + th[2]) / 800 * math.Ln10 // log r
	// Stable: divide by max term.
	a, b, c := z, -z, math.Log(nu)
	mx := math.Max(a, math.Max(b, c))
	ea, eb, ec := math.Exp(a-mx), math.Exp(b-mx), math.Exp(c-mx)
	s := ea + eb + ec
	return Probs{H: ea / s, D: ec / s, A: eb / s}
}

func logistic(x float64) float64 {
	if x >= 0 {
		return 1 / (1 + math.Exp(-x))
	}
	ex := math.Exp(x)
	return ex / (1 + ex)
}

func (e *Elo) window() []eloObs {
	w := e.obs
	if len(w) > e.opt.Window {
		w = w[len(w)-e.opt.Window:]
	}
	return w
}

// refitObs: the draw model is refitted after this many new observations
// (it moves slowly; refitting on every call would dominate run time).
const refitObs = 50

func (e *Elo) fit() {
	if !e.dirty {
		return
	}
	w := e.window()
	th := e.defaultParams()
	if len(w) < 50 {
		e.params = th
		e.dirty = false
		return
	}
	if e.fitted && e.totalObs-e.fitAt < refitObs {
		return
	}
	e.dirty = false
	e.fitAt = e.totalObs
	if e.fitted {
		th = e.params
	}
	ll := func(t [3]float64) float64 {
		s := 0.0
		for _, o := range w {
			p := e.probsFromDiff(o.dr, t)
			s += math.Log(math.Max(p.At(o.outcome), 1e-300))
		}
		// weak priors keep the fit sane on small samples
		d := e.defaultParams()
		for i := range t {
			s -= 0.5 * 1e-2 * (t[i] - d[i]) * (t[i] - d[i])
		}
		return s
	}
	e.params = maximize3(ll, th)
	e.fitted = true
}

// maximize3 is a small damped Newton maximiser with central-difference
// derivatives for the 3-parameter draw models (deterministic).
func maximize3(f func([3]float64) float64, x [3]float64) [3]float64 {
	const h = 1e-4
	fx := f(x)
	lm := 1e-3
	for iter := 0; iter < 60; iter++ {
		var g [3]float64
		var H [3][3]float64
		for i := 0; i < 3; i++ {
			xp, xm := x, x
			xp[i] += h
			xm[i] -= h
			fp, fm := f(xp), f(xm)
			g[i] = (fp - fm) / (2 * h)
			H[i][i] = (fp - 2*fx + fm) / (h * h)
			for j := i + 1; j < 3; j++ {
				a, b, c, d := x, x, x, x
				a[i] += h
				a[j] += h
				b[i] += h
				b[j] -= h
				c[i] -= h
				c[j] += h
				d[i] -= h
				d[j] -= h
				H[i][j] = (f(a) - f(b) - f(c) + f(d)) / (4 * h * h)
				H[j][i] = H[i][j]
			}
		}
		if maxAbs(g[:]) < 1e-6 {
			break
		}
		improved := false
		for tries := 0; tries < 30; tries++ {
			A := make([][]float64, 3)
			for i := range A {
				A[i] = make([]float64, 3)
				for j := range A[i] {
					A[i][j] = -H[i][j]
				}
				A[i][i] += lm * (1 + math.Abs(H[i][i]))
			}
			L, ok := cholesky(A)
			if !ok {
				lm *= 10
				continue
			}
			d := make([]float64, 3)
			cholSolve(L, g[:], d)
			xt := x
			for i := range xt {
				xt[i] += d[i]
			}
			ft := f(xt)
			if ft > fx {
				small := ft-fx < 1e-10
				x, fx = xt, ft
				lm = math.Max(lm/10, 1e-10)
				improved = !small
				break
			}
			lm *= 10
		}
		if !improved {
			break
		}
	}
	return x
}

// EloRating is one row of Elo.Table.
type EloRating struct {
	Team   string
	Rating float64
	Games  int
}

// Table returns teams sorted by rating (highest first, ties by name).
func (e *Elo) Table() []EloRating {
	out := make([]EloRating, 0, len(e.ratings))
	for _, t := range sortedKeys(e.ratings) {
		out = append(out, EloRating{t, e.ratings[t], e.games[t]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rating > out[j].Rating })
	return out
}
