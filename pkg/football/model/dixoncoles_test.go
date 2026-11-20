package model

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

var t0 = time.Date(2020, 8, 1, 15, 0, 0, 0, time.UTC)

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %.10g, want %.10g (±%g)", what, got, want, tol)
	}
}

func TestScoreGridHandChecked(t *testing.T) {
	lh, la := 1.5, 1.1
	g := ScoreGrid(lh, la, 0, 3)
	e := math.Exp(-2.6)
	near(t, "P(0,0)", g[0][0], e, 1e-15)
	near(t, "P(1,0)", g[1][0], 1.5*e, 1e-15)
	near(t, "P(0,1)", g[0][1], 1.1*e, 1e-15)
	near(t, "P(2,1)", g[2][1], 1.5*1.5/2*1.1*e, 1e-15)
	// τ with ρ = −0.1
	g = ScoreGrid(lh, la, -0.1, 3)
	near(t, "τ P(0,0)", g[0][0], e*(1+1.5*1.1*0.1), 1e-15)
	near(t, "τ P(0,1)", g[0][1], 1.1*e*(1-1.5*0.1), 1e-15)
	near(t, "τ P(1,0)", g[1][0], 1.5*e*(1-1.1*0.1), 1e-15)
	near(t, "τ P(1,1)", g[1][1], 1.5*1.1*e*(1.1), 1e-15)
	near(t, "τ P(2,2) untouched", g[2][2], 1.5*1.5/2*1.1*1.1/2*e, 1e-15)
	// τ preserves total mass exactly.
	big := ScoreGrid(lh, la, -0.13, 40)
	s := 0.0
	for x := range big {
		for y := range big[x] {
			s += big[x][y]
		}
	}
	near(t, "grid mass", s, 1, 1e-12)
}

func TestPredictionMarkets(t *testing.T) {
	for _, c := range []struct{ lh, la, rho float64 }{{1.6, 1.1, -0.1}, {0.4, 3.2, 0.05}, {2.9, 0.3, -0.2}, {1e-6, 1e-6, 0}} {
		pr := PredictFromRates(c.lh, c.la, c.rho, 10)
		near(t, "1X2 sum", pr.PHome+pr.PDraw+pr.PAway, 1, 1e-12)
		s := 0.0
		for x := range pr.Matrix {
			for y := range pr.Matrix[x] {
				if pr.Matrix[x][y] < 0 {
					t.Fatalf("negative cell")
				}
				s += pr.Matrix[x][y]
			}
		}
		near(t, "matrix sum", s, 1, 1e-12)
		if len(pr.Matrix) != 11 || len(pr.Matrix[0]) != 11 {
			t.Fatalf("matrix size %d", len(pr.Matrix))
		}
		// Over/under from an independent big grid.
		g := ScoreGrid(c.lh, c.la, c.rho, 60)
		over25, btts := 0.0, 0.0
		for x := range g {
			for y := range g[x] {
				if x+y > 2 {
					over25 += g[x][y]
				}
				if x > 0 && y > 0 {
					btts += g[x][y]
				}
			}
		}
		near(t, "over 2.5", pr.Over["2.5"], over25, 1e-10)
		near(t, "btts", pr.BTTS, btts, 1e-10)
		for i := 1; i < len(OverLines); i++ {
			if pr.Over[LineKey(OverLines[i])] > pr.Over[LineKey(OverLines[i-1])]+1e-15 {
				t.Errorf("over lines not monotone")
			}
		}
		// Asian handicap identities.
		near(t, "AH 0 (draw no bet)", pr.AHHome["0"], pr.PHome/(pr.PHome+pr.PAway), 1e-12)
		near(t, "AH -0.5", pr.AHHome["-0.5"], pr.PHome, 1e-12)
		near(t, "AH +0.5", pr.AHHome["+0.5"], pr.PHome+pr.PDraw, 1e-12)
		near(t, "AH -0.25", pr.AHHome["-0.25"], pr.PHome/(pr.PHome+pr.PDraw/2+pr.PAway), 1e-12)
		near(t, "AH +0.25", pr.AHHome["+0.25"], (pr.PHome+pr.PDraw/2)/(pr.PHome+pr.PDraw/2+pr.PAway), 1e-12)
		o := pr.AH["-1"]
		near(t, "AH -1 parts", o.Win+o.HalfWin+o.Push+o.HalfLoss+o.Loss, 1, 1e-12)
		if o.HalfWin != 0 || o.HalfLoss != 0 {
			t.Errorf("integer line has half results")
		}
		q := pr.AH["-0.75"]
		if q.Push != 0 {
			t.Errorf("quarter line has a full push")
		}
		// -0.75: win by 1 is a half win.
		near(t, "AH -0.75 half win = P(margin 1)", q.HalfWin, marginProb(g, 1), 1e-10)
		near(t, "fair odds", 1/pr.AH["-1"].FairOdds, pr.AHHome["-1"], 1e-12)
		for i := 1; i < len(AHLines); i++ {
			if pr.AHHome[AHKey(AHLines[i])] < pr.AHHome[AHKey(AHLines[i-1])]-1e-12 {
				t.Errorf("AH not monotone at %v", AHLines[i])
			}
		}
		for i := 1; i < len(pr.TopScores); i++ {
			if pr.TopScores[i].P > pr.TopScores[i-1].P {
				t.Errorf("top scores not sorted")
			}
		}
		if len(pr.TopScores) != 6 {
			t.Errorf("want 6 top scores")
		}
	}
}

func marginProb(g [][]float64, m int) float64 {
	s := 0.0
	for x := range g {
		for y := range g[x] {
			if x-y == m {
				s += g[x][y]
			}
		}
	}
	return s
}

func TestKeys(t *testing.T) {
	for line, want := range map[float64]string{-1.5: "-1.5", -0.25: "-0.25", 0: "0", 0.75: "+0.75", 1: "+1"} {
		if got := AHKey(line); got != want {
			t.Errorf("AHKey(%v) = %q", line, got)
		}
	}
	if LineKey(2.5) != "2.5" {
		t.Errorf("LineKey")
	}
}

func TestSymmetricNeutral(t *testing.T) {
	p := &DCParams{Attack: map[string]float64{"A": 0.1, "B": 0.1}, Defence: map[string]float64{"A": -0.05, "B": -0.05},
		HomeAdv: 0.3, Rho: -0.08, Intercept: 0.2, MaxGoals: 10}
	pr, err := p.Predict("A", "B", Adjust{Neutral: true})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "neutral H=A", pr.PHome, pr.PAway, 1e-14)
	for x := range pr.Matrix {
		for y := range pr.Matrix {
			near(t, "symmetric matrix", pr.Matrix[x][y], pr.Matrix[y][x], 1e-15)
		}
	}
	near(t, "AH 0 = 0.5", pr.AHHome["0"], 0.5, 1e-14)
	home, _ := p.Predict("A", "B", Adjust{})
	if home.PHome <= home.PAway {
		t.Errorf("home advantage should favour the home side")
	}
	// Adjust multipliers: halving the home attack lowers λ_home by half.
	adj, _ := p.Predict("A", "B", Adjust{HomeAttack: 0.5, HomeDefence: 2})
	near(t, "adjust λh", adj.LambdaHome, home.LambdaHome*0.5, 1e-12)
	near(t, "adjust λa", adj.LambdaAway, home.LambdaAway*2, 1e-12)
	// Unknown team: newcomer centre, listed.
	u, err := p.Predict("A", "Z", Adjust{})
	if err != nil || len(u.Unknown) != 1 || u.Unknown[0] != "Z" {
		t.Errorf("unknown team handling: %v %v", u.Unknown, err)
	}
	if _, err := (*DCParams)(nil).Predict("A", "B", Adjust{}); err == nil {
		t.Errorf("nil params should error")
	}
}

func TestFitRecoversParameters(t *testing.T) {
	l := newSimLeague(20, 7)
	ms := l.simulate(6, t0, 11) // 2280 matches
	p, err := FitDixonColes(ms, time.Time{}, DCOptions{Xi: 0, MaxGoals: 10, Prior: 0, MinMatches: 0})
	if err != nil {
		t.Fatal(err)
	}
	var ta, fa, td, fd []float64
	sa, sd, rmse := 0.0, 0.0, 0.0
	for _, tm := range l.teams {
		ta = append(ta, l.att[tm])
		fa = append(fa, p.Attack[tm])
		td = append(td, l.def[tm])
		fd = append(fd, p.Defence[tm])
		sa += p.Attack[tm]
		sd += p.Defence[tm]
		rmse += (l.att[tm]-p.Attack[tm])*(l.att[tm]-p.Attack[tm]) + (l.def[tm]-p.Defence[tm])*(l.def[tm]-p.Defence[tm])
	}
	rmse = math.Sqrt(rmse / 40)
	near(t, "Σ attack", sa, 0, 1e-9)
	near(t, "Σ defence", sd, 0, 1e-9)
	ca, cd := corr(ta, fa), corr(td, fd)
	t.Logf("recovery (20 teams, 2280 matches): corr attack %.3f defence %.3f, RMSE %.3f, home %.3f (true %.2f) ±%.3f, rho %.3f (true %.2f) ±%.3f, intercept %.3f (true %.2f), iter %d",
		ca, cd, rmse, p.HomeAdv, l.home, p.HomeAdvSE, p.Rho, l.rho, p.RhoSE, p.Intercept, l.intercept, p.Iter)
	if ca < 0.95 || cd < 0.9 || rmse > 0.08 {
		t.Errorf("poor recovery: corr %.3f/%.3f rmse %.3f", ca, cd, rmse)
	}
	near(t, "home advantage", p.HomeAdv, l.home, 3*p.HomeAdvSE+0.01)
	near(t, "rho", p.Rho, l.rho, 3*p.RhoSE+0.01)
	near(t, "intercept", p.Intercept, l.intercept, 0.05)
	if !p.Converged || p.GradNorm > 1e-5 {
		t.Errorf("not converged: %v grad %g", p.Converged, p.GradNorm)
	}
}

// The analytic gradient must match finite differences of the objective.
func TestGradientMatchesFiniteDifferences(t *testing.T) {
	l := newSimLeague(6, 3)
	ms := l.simulate(1, t0, 5)
	// Build the internal data via a fit, then perturb parameters.
	r := rand.New(rand.NewSource(1))
	d := &dcData{n: 6}
	idx := map[string]int{}
	for i, tm := range l.teams {
		idx[tm] = i
	}
	for i, m := range ms {
		d.hi = append(d.hi, idx[m.Home])
		d.ai = append(d.ai, idx[m.Away])
		d.x = append(d.x, float64(m.HG))
		d.y = append(d.y, float64(m.AG))
		d.gx = append(d.gx, m.HG)
		d.gy = append(d.gy, m.AG)
		d.w = append(d.w, 0.5+float64(i%3)/4)
		hf := 1.0
		if i%7 == 0 {
			hf = 0
		}
		d.hf = append(d.hf, hf)
	}
	d.prec = []float64{1, 2, 3, 1, 2, 3}
	d.mAtt = []float64{0, -0.1, 0, 0, 0, 0.1}
	d.mDef = []float64{0.05, 0, 0, 0, -0.2, 0}
	np := 2*6 + 3
	theta := make([]float64, np)
	for i := range theta {
		theta[i] = 0.2 * r.NormFloat64()
	}
	theta[np-1] = -0.07
	g := make([]float64, np)
	H := make([][]float64, np)
	for i := range H {
		H[i] = make([]float64, np)
	}
	d.eval(theta, g, H)
	const h = 1e-6
	for i := 0; i < np; i++ {
		tp := append([]float64(nil), theta...)
		tm := append([]float64(nil), theta...)
		tp[i] += h
		tm[i] -= h
		fd := (d.eval(tp, nil, nil) - d.eval(tm, nil, nil)) / (2 * h)
		near(t, "gradient", g[i], fd, 1e-5*math.Max(1, math.Abs(fd)))
		gp := make([]float64, np)
		gm := make([]float64, np)
		Hp := make([][]float64, np)
		Hm := make([][]float64, np)
		for k := range Hp {
			Hp[k] = make([]float64, np)
			Hm[k] = make([]float64, np)
		}
		d.eval(tp, gp, Hp)
		d.eval(tm, gm, Hm)
		for j := 0; j < np; j++ {
			fdh := (gp[j] - gm[j]) / (2 * h)
			near(t, "hessian", H[i][j], fdh, 1e-4*math.Max(1, math.Abs(fdh)))
		}
	}
}

func TestFitDeterministicAndOrderFree(t *testing.T) {
	l := newSimLeague(12, 21)
	ms := l.simulate(2, t0, 22)
	asOf := ms[len(ms)-1].Date.AddDate(0, 0, 1)
	o := DefaultDCOptions()
	a, err := FitDixonColes(ms, asOf, o)
	if err != nil {
		t.Fatal(err)
	}
	rev := make([]Match, len(ms))
	for i := range ms {
		rev[len(ms)-1-i] = ms[i]
	}
	b, err := FitDixonColes(rev, asOf, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, tm := range l.teams {
		if a.Attack[tm] != b.Attack[tm] || a.Defence[tm] != b.Defence[tm] {
			t.Fatalf("fit depends on input order for %s", tm)
		}
	}
	if a.Rho != b.Rho || a.HomeAdv != b.HomeAdv || a.LogLik != b.LogLik {
		t.Fatalf("not deterministic")
	}
	// Warm start reaches the same optimum.
	o.Warm = a
	c, err := FitDixonColes(ms, asOf, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, tm := range l.teams {
		near(t, "warm attack", c.Attack[tm], a.Attack[tm], 1e-6)
	}
}

func TestFitUsesOnlyMatchesBeforeAsOf(t *testing.T) {
	l := newSimLeague(10, 1)
	ms := l.simulate(2, t0, 2)
	cut := ms[100].Date
	a, err := FitDixonColes(ms, cut, DefaultDCOptions())
	if err != nil {
		t.Fatal(err)
	}
	// Change every result on/after the cut: the fit must not move.
	changed := append([]Match(nil), ms...)
	for i := range changed {
		if !changed[i].Date.Before(cut) {
			changed[i].HG, changed[i].AG = 9, 0
		}
	}
	b, _ := FitDixonColes(changed, cut, DefaultDCOptions())
	if a.LogLik != b.LogLik || a.Attack["T01"] != b.Attack["T01"] {
		t.Fatalf("fit used matches on/after asOf")
	}
	n := 0
	for _, m := range ms {
		if m.Date.Before(cut) {
			n++
		}
	}
	if a.Matches != n {
		t.Errorf("Matches = %d, want %d", a.Matches, n)
	}
	if _, err := FitDixonColes(ms, t0, DefaultDCOptions()); err != ErrNoData {
		t.Errorf("want ErrNoData, got %v", err)
	}
}

func TestShrinkageAndNewcomers(t *testing.T) {
	l := newSimLeague(10, 4)
	ms := l.simulate(1, t0, 4)
	// A newcomer with two big wins.
	last := ms[len(ms)-1].Date
	ms = append(ms,
		Match{ID: "n1", Date: last.AddDate(0, 0, 1), Home: "NEW", Away: "T01", HG: 4, AG: 0},
		Match{ID: "n2", Date: last.AddDate(0, 0, 8), Home: "T02", Away: "NEW", HG: 0, AG: 3})
	asOf := last.AddDate(0, 0, 10)
	loose, _ := FitDixonColes(ms, asOf, DCOptions{Xi: 0.0019, MaxGoals: 10, Prior: 0.5, MinMatches: 0})
	tight, _ := FitDixonColes(ms, asOf, DCOptions{Xi: 0.0019, MaxGoals: 10, Prior: 0.5, MinMatches: 6})
	if !(tight.Attack["NEW"] < loose.Attack["NEW"]) || tight.Attack["NEW"] <= 0 {
		t.Errorf("MinMatches should shrink a 2-match team toward the mean: %.3f vs %.3f", tight.Attack["NEW"], loose.Attack["NEW"])
	}
	weak, _ := FitDixonColes(ms, asOf, DCOptions{Xi: 0.0019, MaxGoals: 10, Prior: 0.5, MinMatches: 6, NewTeamAttack: -0.3, NewTeamDefence: -0.3})
	if !(weak.Attack["NEW"] < tight.Attack["NEW"]) {
		t.Errorf("newcomer prior centre should pull the newcomer down")
	}
	sum := 0.0
	for _, v := range weak.Attack {
		sum += v
	}
	near(t, "Σ attack with off-centre prior", sum, 0, 1e-9)
	pr, _ := weak.Predict("NEVERSEEN", "T01", Adjust{})
	pr0, _ := weak.Predict("T01", "T03", Adjust{})
	_ = pr0
	if len(pr.Unknown) != 1 {
		t.Errorf("unknown not reported")
	}
	if FitDixonColesMust(t, ms, asOf).Matches == 0 {
		t.Errorf("no matches")
	}
}

func FitDixonColesMust(t *testing.T, ms []Match, asOf time.Time) *DCParams {
	t.Helper()
	p, err := FitDixonColes(ms, asOf, DCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestXGBlend(t *testing.T) {
	l := newSimLeague(10, 9)
	ms := l.simulate(2, t0, 9)
	// xG = true rates (a perfect xG model): blending should move estimates
	// toward the truth on average.
	for i := range ms {
		lh, la := l.rates(ms[i].Home, ms[i].Away)
		ms[i].HxG, ms[i].AxG = &lh, &la
	}
	errOf := func(p *DCParams) float64 {
		s := 0.0
		for _, tm := range l.teams {
			s += math.Pow(p.Attack[tm]-l.att[tm], 2) + math.Pow(p.Defence[tm]-l.def[tm], 2)
		}
		return math.Sqrt(s / float64(2*len(l.teams)))
	}
	g, _ := FitDixonColes(ms, time.Time{}, DCOptions{MaxGoals: 10, Prior: 1})
	x, _ := FitDixonColes(ms, time.Time{}, DCOptions{MaxGoals: 10, Prior: 1, UseXG: 0.7})
	t.Logf("RMSE goals-only %.4f, xG-blend 0.7 %.4f", errOf(g), errOf(x))
	if errOf(x) >= errOf(g) {
		t.Errorf("xG blending with perfect xG should reduce error")
	}
}

func TestFitPerformance20Teams2Seasons(t *testing.T) {
	l := newSimLeague(20, 99)
	ms := l.simulate(2, t0, 98)
	start := time.Now()
	p, err := FitDixonColes(ms, time.Time{}, DefaultDCOptions())
	el := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fit 20 teams × %d matches: %v (%d iterations)", len(ms), el, p.Iter)
	if el > 2*time.Second {
		t.Errorf("too slow: %v", el)
	}
}

// Monte Carlo check of the estimator and its standard errors: 50 simulated
// 20-team leagues × 2 seasons from known parameters (fixed seeds).
func TestRecoveryCoverage(t *testing.T) {
	const reps = 50
	var hb, rb, hse, rse, hsd, rsd, hcov, rcov, acov, rmse float64
	var hs, rs []float64
	ateams := 0
	for rep := 0; rep < reps; rep++ {
		l := newSimLeague(20, int64(1000+rep))
		ms := l.simulate(2, t0, int64(5000+rep))
		p, err := FitDixonColes(ms, time.Time{}, DCOptions{MaxGoals: 10, Prior: 1e-9})
		if err != nil {
			t.Fatal(err)
		}
		hs = append(hs, p.HomeAdv)
		rs = append(rs, p.Rho)
		hb += p.HomeAdv - l.home
		rb += p.Rho - l.rho
		hse += p.HomeAdvSE
		rse += p.RhoSE
		if math.Abs(p.HomeAdv-l.home) < 1.96*p.HomeAdvSE {
			hcov++
		}
		if math.Abs(p.Rho-l.rho) < 1.96*p.RhoSE {
			rcov++
		}
		for _, tm := range l.teams {
			ateams++
			if math.Abs(p.Attack[tm]-l.att[tm]) < 1.96*p.AttackSE[tm] {
				acov++
			}
			rmse += math.Pow(p.Attack[tm]-l.att[tm], 2)
		}
	}
	_, hsd = meanSE(hs)
	_, rsd = meanSE(rs)
	hsd *= math.Sqrt(reps) // meanSE returns SE of the mean
	rsd *= math.Sqrt(reps)
	t.Logf("home adv: bias %+.4f, empirical SD %.4f, mean reported SE %.4f, 95%% coverage %.0f%%", hb/reps, hsd, hse/reps, 100*hcov/reps)
	t.Logf("rho:      bias %+.4f, empirical SD %.4f, mean reported SE %.4f, 95%% coverage %.0f%%", rb/reps, rsd, rse/reps, 100*rcov/reps)
	t.Logf("attack:   RMSE %.4f, 95%% coverage %.0f%% (%d team-fits)", math.Sqrt(rmse/float64(ateams)), 100*acov/float64(ateams), ateams)
	if math.Abs(hb/reps) > 0.02 || math.Abs(rb/reps) > 0.03 {
		t.Errorf("biased estimates")
	}
	if hcov/reps < 0.85 || rcov/reps < 0.75 || acov/float64(ateams) < 0.88 {
		t.Errorf("standard errors under-cover")
	}
}
