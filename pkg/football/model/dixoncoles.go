package model

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// DCOptions configures FitDixonColes.
//
// The zero value DCOptions{} means "use DefaultDCOptions()". Otherwise every
// field is taken literally (Xi 0 = no time decay, Prior 0 = no ridge,
// MinMatches 0 = no top-up), except MaxGoals ≤ 0 which becomes 10.
type DCOptions struct {
	// Xi is the exponential time decay per day: a match d days before asOf
	// has weight exp(-Xi·d). 0.0019 ≈ half-life of one year (Dixon & Coles
	// used half-weeks; 0.0065/half-week ≈ 0.0019/day).
	Xi float64
	// MaxGoals is the score grid size used by Predict (0..MaxGoals goals per
	// side are returned in Prediction.Matrix; markets use an exact larger grid).
	MaxGoals int
	// UseXG in 0..1 blends expected goals into the Poisson part of the
	// likelihood when both HxG and AxG are present: the "count" used is
	// (1-UseXG)·goals + UseXG·xG (a quasi-likelihood; the low-score ρ term
	// always uses the real score).
	UseXG float64
	// MinMatches: a team with n < MinMatches matches in the fit window gets
	// extra prior precision equal to (MinMatches-n) average matches of
	// information, i.e. it is topped up with phantom matches at its prior
	// centre (the league mean unless NewTeamAttack/NewTeamDefence are set).
	MinMatches int
	// Prior is the ridge precision on every attack and defence parameter
	// (penalty Prior/2·(a²+d²) on the log scale). One unit ≈ the information
	// in 0.7 matches. Larger = more shrinkage toward the league mean.
	Prior float64

	// NewTeamAttack / NewTeamDefence are the prior centres (log scale,
	// relative to the league mean; 0 = league mean) used for teams with fewer
	// than MinMatches matches in the window and for teams Predict has never
	// seen. Negative values encode "newcomers (e.g. promoted teams) are
	// usually weaker than average". Defence > 0 means a better defence.
	NewTeamAttack, NewTeamDefence float64
	// MinWeight drops matches whose decay weight is below it (default 0.02,
	// i.e. older than ln(50)/Xi days ≈ 5.6 years at Xi = 0.0019).
	MinWeight float64
	// MaxIter caps Newton iterations (default 100).
	MaxIter int
	// Warm optionally starts the optimiser from an earlier fit (faster
	// walk-forward refits; the optimum does not depend on it).
	Warm *DCParams
}

// DefaultDCOptions returns the defaults chosen by the EPL walk-forward
// tuning described in README.md.
func DefaultDCOptions() DCOptions {
	return DCOptions{
		Xi:             0.0019,
		MaxGoals:       10,
		UseXG:          0,
		MinMatches:     6,
		Prior:          4,
		NewTeamAttack:  0,
		NewTeamDefence: 0,
	}
}

func (o DCOptions) isZero() bool {
	return o.Xi == 0 && o.MaxGoals == 0 && o.UseXG == 0 && o.MinMatches == 0 && o.Prior == 0 &&
		o.NewTeamAttack == 0 && o.NewTeamDefence == 0 && o.MinWeight == 0 && o.MaxIter == 0 && o.Warm == nil
}

func (o DCOptions) normalized() DCOptions {
	if o.isZero() {
		o = DefaultDCOptions()
	}
	if o.MaxGoals <= 0 {
		o.MaxGoals = 10
	}
	if o.Xi < 0 || math.IsNaN(o.Xi) {
		o.Xi = 0
	}
	o.UseXG = clamp(o.UseXG, 0, 1)
	if math.IsNaN(o.UseXG) {
		o.UseXG = 0
	}
	if o.Prior < 0 || math.IsNaN(o.Prior) {
		o.Prior = 0
	}
	if o.MinMatches < 0 {
		o.MinMatches = 0
	}
	if o.MinWeight <= 0 {
		o.MinWeight = 0.02
	}
	if o.MaxIter <= 0 {
		o.MaxIter = 100
	}
	return o
}

// DCParams is a fitted Dixon–Coles model.
//
//	log λ_home = Intercept + HomeAdv + Attack[home] − Defence[away]
//	log λ_away = Intercept           + Attack[away] − Defence[home]
//
// Attack and Defence are on the log scale and each sums to zero over the
// teams in the fit, so 0 is the league average; Defence > 0 means the team
// concedes fewer goals than average. exp(HomeAdv) is the home goals
// multiplier. Rho is the Dixon–Coles low-score dependence (negative = more
// 0-0 and 1-1 than independent Poissons give).
type DCParams struct {
	Attack, Defence  map[string]float64
	HomeAdv, Rho, Xi float64
	AsOf             time.Time
	Matches          int
	LogLik           float64 // weighted log-likelihood at the optimum (prior excluded)
	Converged        bool
	Iter             int

	Intercept float64 // log goals per team per match at a neutral venue between average teams
	MaxGoals  int
	// Approximate standard errors from the inverse penalised (weighted)
	// Hessian. With time decay they are indicative, not exact.
	AttackSE, DefenceSE           map[string]float64
	HomeAdvSE, RhoSE              float64
	TeamMatches                   map[string]int     // matches per team in the fit window
	TeamWeight                    map[string]float64 // sum of decay weights per team
	NewTeamAttack, NewTeamDefence float64
	Penalty                       float64 // ridge penalty at the optimum
	GradNorm                      float64 // max |gradient| at the end (reduced space)
}

type dcData struct {
	hi, ai []int
	x, y   []float64 // counts in the Poisson part (goals, or goals blended with xG)
	gx, gy []int     // real goals (for τ and the log x! constants)
	w      []float64
	hf     []float64 // 1 = home advantage applies, 0 = neutral
	n      int       // teams
	prec   []float64 // prior precision per team
	mAtt   []float64 // prior centre per team (attack)
	mDef   []float64 // prior centre per team (defence)
	lfact  float64   // Σ w·(log x! + log y!) for the reported log-likelihood
	sumW   float64
}

// FitDixonColes fits the model on the matches strictly before asOf (a zero
// asOf means "after the last match"), weighting each by exp(-ξ·days before
// asOf). Matches are not modified; their order does not matter.
func FitDixonColes(ms []Match, asOf time.Time, o DCOptions) (*DCParams, error) {
	o = o.normalized()
	if asOf.IsZero() {
		for _, m := range ms {
			if m.Date.After(asOf) {
				asOf = m.Date
			}
		}
		asOf = asOf.Add(time.Nanosecond)
	}
	// Collect usable matches in a deterministic order.
	type row struct {
		m Match
		w float64
	}
	rows := make([]row, 0, len(ms))
	for _, m := range ms {
		if !m.Date.Before(asOf) || m.Home == "" || m.Away == "" || m.Home == m.Away || m.HG < 0 || m.AG < 0 {
			continue
		}
		days := asOf.Sub(m.Date).Hours() / 24
		w := 1.0
		if o.Xi > 0 {
			w = math.Exp(-o.Xi * days)
		}
		if w < o.MinWeight {
			continue
		}
		rows = append(rows, row{m, w})
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].m, rows[j].m
		if !a.Date.Equal(b.Date) {
			return a.Date.Before(b.Date)
		}
		if a.Home != b.Home {
			return a.Home < b.Home
		}
		if a.Away != b.Away {
			return a.Away < b.Away
		}
		return a.ID < b.ID
	})
	teamSet := map[string]bool{}
	for _, r := range rows {
		teamSet[r.m.Home] = true
		teamSet[r.m.Away] = true
	}
	teams := sortedKeys(teamSet)
	n := len(teams)
	if n < 2 {
		return nil, ErrNoData
	}
	idx := make(map[string]int, n)
	for i, t := range teams {
		idx[t] = i
	}
	d := &dcData{n: n}
	cnt := make([]int, n)
	tw := make([]float64, n)
	var sumHome, sumAway, sumWH, sumAll float64
	for _, r := range rows {
		m := r.m
		h, a := idx[m.Home], idx[m.Away]
		x, y := float64(m.HG), float64(m.AG)
		if o.UseXG > 0 && m.HxG != nil && m.AxG != nil && *m.HxG >= 0 && *m.AxG >= 0 {
			x = (1-o.UseXG)*x + o.UseXG**m.HxG
			y = (1-o.UseXG)*y + o.UseXG**m.AxG
		}
		hf := 1.0
		if m.Neutral {
			hf = 0
		}
		d.hi = append(d.hi, h)
		d.ai = append(d.ai, a)
		d.x = append(d.x, x)
		d.y = append(d.y, y)
		d.gx = append(d.gx, m.HG)
		d.gy = append(d.gy, m.AG)
		d.w = append(d.w, r.w)
		d.hf = append(d.hf, hf)
		lx, _ := math.Lgamma(x + 1)
		ly, _ := math.Lgamma(y + 1)
		d.lfact += r.w * (lx + ly)
		d.sumW += r.w
		cnt[h]++
		cnt[a]++
		tw[h] += r.w
		tw[a] += r.w
		if hf == 1 {
			sumHome += r.w * x
			sumAway += r.w * y
			sumWH += r.w
		}
		sumAll += r.w * (x + y)
	}
	meanGoals := sumAll / (2 * d.sumW)
	if meanGoals <= 0 {
		meanGoals = 0.05
	}
	// Prior precision per team: base ridge + phantom matches for sparse teams.
	d.prec = make([]float64, n)
	d.mAtt = make([]float64, n)
	d.mDef = make([]float64, n)
	for i := 0; i < n; i++ {
		p := o.Prior
		if cnt[i] < o.MinMatches {
			p += float64(o.MinMatches-cnt[i]) * meanGoals
			d.mAtt[i] = o.NewTeamAttack
			d.mDef[i] = o.NewTeamDefence
		}
		if p < 1e-6 {
			p = 1e-6 // numerical floor only
		}
		d.prec[i] = p
	}

	// Starting point.
	np := 2*n + 3
	theta := make([]float64, np)
	ic, ih, ir := 2*n, 2*n+1, 2*n+2
	theta[ic] = math.Log(meanGoals)
	if sumWH > 0 && sumHome > 0 && sumAway > 0 {
		theta[ih] = clamp(math.Log(sumHome/sumAway), -1, 1)
	}
	if w := o.Warm; w != nil {
		for i, t := range teams {
			theta[i] = w.Attack[t]
			theta[n+i] = w.Defence[t]
		}
		center(theta[:n])
		center(theta[n : 2*n])
		if w.Intercept != 0 {
			theta[ic] = w.Intercept
		}
		theta[ih] = w.HomeAdv
		if math.Abs(w.Rho) < 0.5 {
			theta[ir] = w.Rho
		}
		if math.IsInf(d.eval(theta, nil, nil), -1) {
			theta[ir] = 0
		}
	}

	res := newtonMax(d, theta, o.MaxIter)

	p := &DCParams{
		Attack: make(map[string]float64, n), Defence: make(map[string]float64, n),
		AttackSE: make(map[string]float64, n), DefenceSE: make(map[string]float64, n),
		TeamMatches: make(map[string]int, n), TeamWeight: make(map[string]float64, n),
		HomeAdv: res.theta[ih], Rho: res.theta[ir], Xi: o.Xi, AsOf: asOf, Matches: len(rows),
		Converged: res.converged, Iter: res.iter, Intercept: res.theta[ic], MaxGoals: o.MaxGoals,
		NewTeamAttack: o.NewTeamAttack, NewTeamDefence: o.NewTeamDefence, GradNorm: res.gradNorm,
	}
	for i, t := range teams {
		p.Attack[t] = res.theta[i]
		p.Defence[t] = res.theta[n+i]
		p.TeamMatches[t] = cnt[i]
		p.TeamWeight[t] = tw[i]
		if res.se != nil {
			p.AttackSE[t] = res.se[i]
			p.DefenceSE[t] = res.se[n+i]
		}
	}
	if res.se != nil {
		p.HomeAdvSE = res.se[ih]
		p.RhoSE = res.se[ir]
	}
	p.Penalty = d.penalty(res.theta)
	p.LogLik = res.f + p.Penalty - d.lfact
	if !p.Converged {
		return p, fmt.Errorf("model: Dixon–Coles fit did not converge in %d iterations (max |grad| %.3g)", res.iter, res.gradNorm)
	}
	return p, nil
}

func center(v []float64) {
	if len(v) == 0 {
		return
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	s /= float64(len(v))
	for i := range v {
		v[i] -= s
	}
}

func (d *dcData) penalty(theta []float64) float64 {
	n := d.n
	pen := 0.0
	for i := 0; i < n; i++ {
		da := theta[i] - d.mAtt[i]
		dd := theta[n+i] - d.mDef[i]
		pen += 0.5 * d.prec[i] * (da*da + dd*dd)
	}
	return pen
}

// eval returns the penalised objective (weighted log-likelihood without the
// log x! constants, minus the ridge penalty). When g/H are non-nil it also
// accumulates the full-space gradient and Hessian. -Inf means infeasible ρ.
func (d *dcData) eval(theta []float64, g []float64, H [][]float64) float64 {
	n := d.n
	ic, ih, ir := 2*n, 2*n+1, 2*n+2
	c, h, rho := theta[ic], theta[ih], theta[ir]
	if rho >= 1 || rho <= -1 || math.IsNaN(rho) {
		return math.Inf(-1)
	}
	want := g != nil
	np := 2*n + 3
	if want {
		for i := range g {
			g[i] = 0
		}
		for i := range H {
			for j := range H[i] {
				H[i][j] = 0
			}
		}
	}
	f := 0.0
	var j1i, j2i [4]int
	var j1v, j2v [4]float64
	for k := range d.w {
		w := d.w[k]
		hi, ai := d.hi[k], d.ai[k]
		hf := d.hf[k]
		e1 := c + hf*h + theta[hi] - theta[n+ai]
		e2 := c + theta[ai] - theta[n+hi]
		lam, mu := math.Exp(e1), math.Exp(e2)
		x, y := d.x[k], d.y[k]
		f += w * (x*e1 - lam + y*e2 - mu)
		g1, g2, gr := w*(x-lam), w*(y-mu), 0.0
		h11, h22, h12, hrr, h1r, h2r := -w*lam, -w*mu, 0.0, 0.0, 0.0, 0.0
		switch {
		case d.gx[k] == 0 && d.gy[k] == 0:
			u := rho * lam * mu
			t := 1 - u
			if t <= 0 {
				return math.Inf(-1)
			}
			f += w * math.Log(t)
			if want {
				lm := lam * mu
				g1 += -w * u / t
				g2 += -w * u / t
				gr += -w * lm / t
				q := -w * u / (t * t)
				h11 += q
				h22 += q
				h12 += q
				hrr += -w * lm * lm / (t * t)
				h1r += -w * lm / (t * t)
				h2r += -w * lm / (t * t)
			}
		case d.gx[k] == 0 && d.gy[k] == 1:
			v := rho * lam
			t := 1 + v
			if t <= 0 {
				return math.Inf(-1)
			}
			f += w * math.Log(t)
			if want {
				g1 += w * v / t
				gr += w * lam / t
				h11 += w * v / (t * t)
				hrr += -w * lam * lam / (t * t)
				h1r += w * lam / (t * t)
			}
		case d.gx[k] == 1 && d.gy[k] == 0:
			v := rho * mu
			t := 1 + v
			if t <= 0 {
				return math.Inf(-1)
			}
			f += w * math.Log(t)
			if want {
				g2 += w * v / t
				gr += w * mu / t
				h22 += w * v / (t * t)
				hrr += -w * mu * mu / (t * t)
				h2r += w * mu / (t * t)
			}
		case d.gx[k] == 1 && d.gy[k] == 1:
			t := 1 - rho
			f += w * math.Log(t)
			if want {
				gr += -w / t
				hrr += -w / (t * t)
			}
		}
		if !want {
			continue
		}
		// η1 = c + hf·h + a_hi − d_ai ; η2 = c + a_ai − d_hi
		j1i = [4]int{ic, ih, hi, n + ai}
		j1v = [4]float64{1, hf, 1, -1}
		j2i = [4]int{ic, -1, ai, n + hi}
		j2v = [4]float64{1, 0, 1, -1}
		for a := 0; a < 4; a++ {
			if j1v[a] != 0 {
				g[j1i[a]] += g1 * j1v[a]
			}
			if j2v[a] != 0 {
				g[j2i[a]] += g2 * j2v[a]
			}
		}
		g[ir] += gr
		for a := 0; a < 4; a++ {
			for b := 0; b < 4; b++ {
				if j1v[a] != 0 && j1v[b] != 0 {
					H[j1i[a]][j1i[b]] += h11 * j1v[a] * j1v[b]
				}
				if j2v[a] != 0 && j2v[b] != 0 {
					H[j2i[a]][j2i[b]] += h22 * j2v[a] * j2v[b]
				}
				if h12 != 0 && j1v[a] != 0 && j2v[b] != 0 {
					v := h12 * j1v[a] * j2v[b]
					H[j1i[a]][j2i[b]] += v
					H[j2i[b]][j1i[a]] += v
				}
			}
			if j1v[a] != 0 && h1r != 0 {
				H[j1i[a]][ir] += h1r * j1v[a]
				H[ir][j1i[a]] += h1r * j1v[a]
			}
			if j2v[a] != 0 && h2r != 0 {
				H[j2i[a]][ir] += h2r * j2v[a]
				H[ir][j2i[a]] += h2r * j2v[a]
			}
		}
		H[ir][ir] += hrr
	}
	// Ridge prior.
	for i := 0; i < n; i++ {
		da := theta[i] - d.mAtt[i]
		dd := theta[n+i] - d.mDef[i]
		f -= 0.5 * d.prec[i] * (da*da + dd*dd)
		if want {
			g[i] -= d.prec[i] * da
			g[n+i] -= d.prec[i] * dd
			H[i][i] -= d.prec[i]
			H[n+i][n+i] -= d.prec[i]
		}
	}
	_ = np
	return f
}

type newtonResult struct {
	theta     []float64
	f         float64
	converged bool
	iter      int
	gradNorm  float64
	se        []float64 // full-space standard errors
}

// newtonMax maximises d.eval with a Levenberg–Marquardt damped Newton method
// in the reduced (sum-to-zero) parameter space: the last team's attack and
// defence are minus the sum of the others'. Reduced index i maps to full
// index fi[i] with coefficient +1 and, for team parameters, to the block's
// last team li[i] with coefficient −1.
func newtonMax(d *dcData, theta0 []float64, maxIter int) newtonResult {
	n := d.n
	np := 2*n + 3
	nr := 2*n + 1
	fi := make([]int, nr)
	li := make([]int, nr)
	for i := 0; i < n-1; i++ {
		fi[i], li[i] = i, n-1
		fi[n-1+i], li[n-1+i] = n+i, 2*n-1
	}
	fi[2*n-2], li[2*n-2] = 2*n, -1
	fi[2*n-1], li[2*n-1] = 2*n+1, -1
	fi[2*n], li[2*n] = 2*n+2, -1

	theta := append([]float64(nil), theta0...)
	center(theta[:n])
	center(theta[n : 2*n])
	g := make([]float64, np)
	H := make([][]float64, np)
	for i := range H {
		H[i] = make([]float64, np)
	}
	gr := make([]float64, nr)
	Hr := make([][]float64, nr)
	A := make([][]float64, nr)
	for i := range Hr {
		Hr[i] = make([]float64, nr)
		A[i] = make([]float64, nr)
	}
	HZ := make([][]float64, np)
	for i := range HZ {
		HZ[i] = make([]float64, nr)
	}
	reduce := func() {
		for i := 0; i < nr; i++ {
			v := g[fi[i]]
			if li[i] >= 0 {
				v -= g[li[i]]
			}
			gr[i] = v
		}
		for r := 0; r < np; r++ {
			for j := 0; j < nr; j++ {
				v := H[r][fi[j]]
				if li[j] >= 0 {
					v -= H[r][li[j]]
				}
				HZ[r][j] = v
			}
		}
		for i := 0; i < nr; i++ {
			for j := 0; j < nr; j++ {
				v := HZ[fi[i]][j]
				if li[i] >= 0 {
					v -= HZ[li[i]][j]
				}
				Hr[i][j] = v
			}
		}
	}
	step := func(dst, src, delta []float64) {
		copy(dst, src)
		for i := 0; i < nr; i++ {
			dst[fi[i]] += delta[i]
			if li[i] >= 0 {
				dst[li[i]] -= delta[i]
			}
		}
	}

	f := d.eval(theta, g, H)
	if math.IsInf(f, -1) || math.IsNaN(f) {
		theta[2*n+2] = 0
		f = d.eval(theta, g, H)
	}
	reduce()
	lm := 1e-6
	res := newtonResult{}
	trial := make([]float64, np)
	delta := make([]float64, nr)
	iter := 0
	converged := false
	for iter = 0; iter < maxIter; iter++ {
		accepted := false
		for tries := 0; tries < 40; tries++ {
			for i := 0; i < nr; i++ {
				for j := 0; j < nr; j++ {
					A[i][j] = -Hr[i][j]
				}
				A[i][i] += lm * (1 + math.Abs(Hr[i][i]))
			}
			L, ok := cholesky(A)
			if !ok {
				lm = math.Max(lm*10, 1e-8)
				continue
			}
			cholSolve(L, gr, delta)
			dec := 0.0 // Newton decrement² (≈ 2 × predicted increase)
			for i := range delta {
				dec += delta[i] * gr[i]
			}
			step(trial, theta, delta)
			ft := d.eval(trial, nil, nil)
			if !math.IsNaN(ft) && !math.IsInf(ft, -1) && ft >= f-1e-12*math.Max(1, math.Abs(f)) {
				copy(theta, trial)
				improvement := ft - f
				f = d.eval(theta, g, H)
				reduce()
				lm = math.Max(lm/10, 1e-12)
				accepted = true
				if dec < 1e-10 && improvement < 1e-9 {
					converged = true
				}
				break
			}
			lm = math.Max(lm*10, 1e-8)
		}
		if !accepted {
			// No ascent step exists: at the optimum to numerical precision
			// if the gradient is tiny.
			converged = maxAbs(gr) < 1e-6*math.Max(1, d.sumW)
			break
		}
		if converged {
			iter++
			break
		}
	}
	res.theta = theta
	res.f = f
	res.converged = converged
	res.iter = iter
	res.gradNorm = maxAbs(gr)
	// Standard errors: diag(Z (−Hr)⁻¹ Zᵀ).
	for i := 0; i < nr; i++ {
		for j := 0; j < nr; j++ {
			A[i][j] = -Hr[i][j]
		}
	}
	if L, ok := cholesky(A); ok {
		inv := cholInverse(L)
		se := make([]float64, np)
		for i := 0; i < nr; i++ {
			if v := inv[i][i]; v > 0 {
				se[fi[i]] = math.Sqrt(v)
			}
		}
		// Last team in each block: variance of −Σ (others).
		for _, blk := range [][2]int{{0, n - 1}, {n - 1, 2*n - 2}} {
			v := 0.0
			for i := blk[0]; i < blk[1]; i++ {
				for j := blk[0]; j < blk[1]; j++ {
					v += inv[i][j]
				}
			}
			if v > 0 {
				se[li[blk[0]]] = math.Sqrt(v)
			}
		}
		res.se = se
	}
	return res
}

func maxAbs(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if a := math.Abs(x); a > m || math.IsNaN(a) {
			m = a
		}
	}
	return m
}

// cholesky returns the lower-triangular L with A = L·Lᵀ, or ok=false when A
// is not (numerically) positive definite.
func cholesky(A [][]float64) ([][]float64, bool) {
	n := len(A)
	L := make([][]float64, n)
	for i := range L {
		L[i] = make([]float64, n)
	}
	for j := 0; j < n; j++ {
		s := A[j][j]
		for k := 0; k < j; k++ {
			s -= L[j][k] * L[j][k]
		}
		if s <= 0 || math.IsNaN(s) {
			return nil, false
		}
		L[j][j] = math.Sqrt(s)
		for i := j + 1; i < n; i++ {
			t := A[i][j]
			for k := 0; k < j; k++ {
				t -= L[i][k] * L[j][k]
			}
			L[i][j] = t / L[j][j]
		}
	}
	return L, true
}

func cholSolve(L [][]float64, b, x []float64) {
	n := len(L)
	y := make([]float64, n)
	for i := 0; i < n; i++ {
		s := b[i]
		for k := 0; k < i; k++ {
			s -= L[i][k] * y[k]
		}
		y[i] = s / L[i][i]
	}
	for i := n - 1; i >= 0; i-- {
		s := y[i]
		for k := i + 1; k < n; k++ {
			s -= L[k][i] * x[k]
		}
		x[i] = s / L[i][i]
	}
}

func cholInverse(L [][]float64) [][]float64 {
	n := len(L)
	inv := make([][]float64, n)
	e := make([]float64, n)
	col := make([]float64, n)
	for j := 0; j < n; j++ {
		for i := range e {
			e[i] = 0
		}
		e[j] = 1
		cholSolve(L, e, col)
		for i := 0; i < n; i++ {
			if inv[i] == nil {
				inv[i] = make([]float64, n)
			}
			inv[i][j] = col[i]
		}
	}
	return inv
}

// ErrNilParams is returned by Predict on a nil receiver.
var ErrNilParams = errors.New("model: no fitted parameters")

// Adjust holds goal-expectation multipliers applied on top of the fitted
// rates (1 = none; 0, negative or NaN also mean none so Adjust{} is "no
// adjustment"). HomeAttack scales the goals the home team is expected to
// score; HomeDefence scales the goals the home team is expected to CONCEDE
// (> 1 = weaker defence), and likewise for the away team. So
//
//	λ_home = base_home · HomeAttack · AwayDefence
//	λ_away = base_away · AwayAttack · HomeDefence
//
// which is exactly how AvailabilityAdjust's attackMult / defenceMult plug in.
// Neutral removes home advantage.
type Adjust struct {
	HomeAttack, HomeDefence, AwayAttack, AwayDefence float64
	Neutral                                          bool
}

func mult(v float64) float64 {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 1
	}
	return v
}

// Rates returns the expected goals (Poisson means before the ρ correction)
// for a fixture, and the list of teams that were not in the fit (they get the
// newcomer prior centre).
func (p *DCParams) Rates(home, away string, adj Adjust) (lh, la float64, unknown []string) {
	ah, okh := p.Attack[home]
	dh := p.Defence[home]
	if !okh {
		ah, dh = p.NewTeamAttack, p.NewTeamDefence
		unknown = append(unknown, home)
	}
	aa, oka := p.Attack[away]
	da := p.Defence[away]
	if !oka {
		aa, da = p.NewTeamAttack, p.NewTeamDefence
		unknown = append(unknown, away)
	}
	hadv := p.HomeAdv
	if adj.Neutral {
		hadv = 0
	}
	lh = math.Exp(p.Intercept+hadv+ah-da) * mult(adj.HomeAttack) * mult(adj.AwayDefence)
	la = math.Exp(p.Intercept+aa-dh) * mult(adj.AwayAttack) * mult(adj.HomeDefence)
	return lh, la, unknown
}

// Predict returns the full forecast for home vs away. Teams the model has not
// seen are not an error: they are rated at the newcomer prior centre and
// listed in Prediction.Unknown so the caller can label the forecast.
func (p *DCParams) Predict(home, away string, adj Adjust) (Prediction, error) {
	if p == nil {
		return Prediction{}, ErrNilParams
	}
	if home == "" || away == "" || home == away {
		return Prediction{}, fmt.Errorf("model: invalid fixture %q vs %q", home, away)
	}
	lh, la, unknown := p.Rates(home, away, adj)
	mg := p.MaxGoals
	if mg <= 0 {
		mg = 10
	}
	pr := PredictFromRates(lh, la, p.Rho, mg)
	pr.Unknown = unknown
	return pr, nil
}

// Strength is a team's fitted ratings in goals: expected goals scored and
// conceded per match against an average team at a neutral venue.
type Strength struct {
	Team            string
	Attack, Defence float64 // log scale (as in DCParams)
	GoalsFor        float64 // exp(Intercept + Attack)
	GoalsAgainst    float64 // exp(Intercept − Defence)
	Matches         int
}

// Table returns every team's strength, strongest (by goal difference vs an
// average team) first.
func (p *DCParams) Table() []Strength {
	out := make([]Strength, 0, len(p.Attack))
	for _, t := range sortedKeys(p.Attack) {
		a, d := p.Attack[t], p.Defence[t]
		out = append(out, Strength{Team: t, Attack: a, Defence: d,
			GoalsFor: math.Exp(p.Intercept + a), GoalsAgainst: math.Exp(p.Intercept - d), Matches: p.TeamMatches[t]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		gi := out[i].GoalsFor - out[i].GoalsAgainst
		gj := out[j].GoalsFor - out[j].GoalsAgainst
		if gi != gj {
			return gi > gj
		}
		return out[i].Team < out[j].Team
	})
	return out
}
