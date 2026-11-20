package model

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestEloUpdateHandChecked(t *testing.T) {
	e := NewElo(EloOptions{K: 20, HomeAdv: 100, Init: 1500, GoalDiffScale: true})
	e.Update(Match{Date: t0, Home: "A", Away: "B", HG: 3, AG: 0})
	// dr = 100 → E = 1/(1+10^-0.25) = 0.640065; G(3) = 14/8 = 1.75.
	exp := 1 / (1 + math.Pow(10, -0.25))
	delta := 20 * 1.75 * (1 - exp)
	near(t, "A rating", e.Rating("A"), 1500+delta, 1e-9)
	near(t, "B rating", e.Rating("B"), 1500-delta, 1e-9)
	near(t, "zero-sum", e.Rating("A")+e.Rating("B"), 3000, 1e-9)
	near(t, "GD mult 2", GoalDiffMultiplier(-2), 1.5, 0)
	near(t, "GD mult 5", GoalDiffMultiplier(5), 2, 0)
	near(t, "unknown team", e.Rating("Z"), 1500, 0)
	// A draw at home moves the home side down (it was expected to win).
	e.Update(Match{Date: t0.AddDate(0, 0, 7), Home: "C", Away: "D", HG: 1, AG: 1})
	if e.Rating("C") >= 1500 {
		t.Errorf("home draw should cost the home side rating")
	}
	ph, pd, pa := e.Probabilities("A", "B", false)
	near(t, "probs sum", ph+pd+pa, 1, 1e-12)
	if ph <= pa {
		t.Errorf("stronger home side should be favourite")
	}
	nh, _, na := e.Probabilities("P", "Q", true)
	near(t, "neutral equal teams", nh, na, 1e-15)
	if e.Options().DrawModel != "davidson" {
		t.Errorf("default draw model")
	}
	if NewElo(EloOptions{}).Options().K != DefaultEloOptions().K {
		t.Errorf("zero options should mean defaults")
	}
}

// On a simulated league the fitted draw model must reproduce the league's
// draw rate, and Elo must rank teams like the true strengths.
func TestEloCalibratedDrawModel(t *testing.T) {
	l := newSimLeague(20, 31)
	ms := l.simulate(8, t0, 32)
	for _, model := range []string{"davidson", "ordlogit"} {
		e := NewElo(EloOptions{K: 20, HomeAdv: 65, Init: 1500, GoalDiffScale: true, DrawModel: model})
		half := len(ms) / 2
		for _, m := range ms[:half] {
			e.Update(m)
		}
		draws, pred, n := 0, 0.0, 0
		var ll float64
		for _, m := range ms[half:] {
			ph, pd, pa := e.Probabilities(m.Home, m.Away, false)
			near(t, "sum", ph+pd+pa, 1, 1e-9)
			if m.HG == m.AG {
				draws++
			}
			pred += pd
			n++
			ll += LogLoss(Probs{ph, pd, pa}, Outcome(m.HG, m.AG))
			e.Update(m)
		}
		obs := float64(draws) / float64(n)
		// In sample (the fit window) maximum likelihood matches the draw
		// rate exactly up to the weak prior: Σ P(draw) = #draws.
		e.fit()
		e.dirty, e.fitAt = true, -1000
		e.fit()
		insObs, insPred := 0.0, 0.0
		for _, o := range e.window() {
			if o.outcome == OutcomeDraw {
				insObs++
			}
			insPred += e.probsFromDiff(o.dr, e.params).D
		}
		nw := float64(len(e.window()))
		t.Logf("%s: in-sample draw rate %.4f vs fitted %.4f (n=%.0f); out-of-sample observed %.3f vs predicted %.3f, log loss %.4f",
			model, insObs/nw, insPred/nw, nw, obs, pred/float64(n), ll/float64(n))
		if math.Abs(insObs-insPred)/nw > 0.002 {
			t.Errorf("%s draw model not fitted to the league draw rate: %.4f vs %.4f", model, insObs/nw, insPred/nw)
		}
		if math.Abs(obs-pred/float64(n)) > 0.035 {
			t.Errorf("%s draw model badly off out of sample: %.3f vs %.3f", model, obs, pred/float64(n))
		}
		var tr, er []float64
		for _, tm := range l.teams {
			tr = append(tr, l.att[tm]+l.def[tm])
			er = append(er, e.Rating(tm))
		}
		if c := corr(tr, er); c < 0.85 {
			t.Errorf("%s: Elo ratings correlate %.3f with true strength", model, c)
		}
		if tbl := e.Table(); len(tbl) != 20 || tbl[0].Rating < tbl[19].Rating {
			t.Errorf("table not sorted")
		}
	}
}

func TestEloNewTeamOffset(t *testing.T) {
	e := NewElo(EloOptions{K: 20, HomeAdv: 65, Init: 1500, NewTeamOffset: -80})
	teams := []string{"A", "B", "C", "D", "E", "F"}
	for i := 0; i < 30; i++ {
		e.Update(Match{Date: t0.AddDate(0, 0, i), Home: teams[i%6], Away: teams[(i+1)%6], HG: i % 3, AG: 1})
	}
	mean := 0.0
	for _, tm := range teams {
		mean += e.Rating(tm)
	}
	mean /= 6
	e.Update(Match{Date: t0.AddDate(0, 0, 40), Home: "NEW", Away: "A", HG: 0, AG: 0})
	if r := e.Rating("NEW"); r > mean-60 {
		t.Errorf("newcomer should start below the active mean: %.1f vs %.1f", r, mean)
	}
}

func f64(v float64) *float64 { return &v }

func TestFormHandChecked(t *testing.T) {
	d := func(i int) time.Time { return t0.AddDate(0, 0, 7*i) }
	rs := []TeamResult{
		{Date: d(0), Home: true, GF: 0, GA: 2, Opp: "X", OppRating: 1600},                                // L
		{Date: d(1), Home: false, GF: 1, GA: 1, Opp: "Y", OppRating: 1500},                               // D
		{Date: d(2), Home: true, GF: 2, GA: 0, Opp: "Z", OppRating: 1450, XGF: f64(1.8), XGA: f64(0.4)},  // W
		{Date: d(3), Home: false, GF: 3, GA: 1, Opp: "W", OppRating: 1700, XGF: f64(1.2), XGA: f64(1.5)}, // W
		{Date: d(4), Home: true, GF: 1, GA: 0, Opp: "V", OppRating: 1550, XGF: f64(0.9), XGA: f64(0.7)},  // W
	}
	f := Form(rs, 0)
	if f.N != 5 || f.W != 3 || f.D != 1 || f.L != 1 || f.Sequence != "LDWWW" {
		t.Fatalf("counts: %+v", f)
	}
	near(t, "points", f.Points, 10, 0)
	near(t, "ppg", f.PPG, 2, 1e-12)
	if f.GF != 7 || f.GA != 4 || f.GD != 3 || f.CleanSheets != 2 || f.FailedToScore != 1 {
		t.Errorf("goals: %+v", f)
	}
	if !f.HasXG || f.XGN != 3 {
		t.Errorf("xg flags")
	}
	near(t, "xgf", f.XGF, 3.9, 1e-12)
	// Weighted PPG: weights 0.5^(age/4), newest age 0.
	ws := []float64{math.Pow(0.5, 1), math.Pow(0.5, 0.75), math.Pow(0.5, 0.5), math.Pow(0.5, 0.25), 1}
	pts := []float64{0, 1, 3, 3, 3}
	sw, sp := 0.0, 0.0
	for i := range ws {
		sw += ws[i]
		sp += ws[i] * pts[i]
	}
	near(t, "weighted ppg", f.WeightedPPG, sp/sw, 1e-12)
	// 5 matches → rolling window 2: [0.5, 2, 3, 3] → OLS slope 0.85.
	near(t, "trend", f.Trend, 0.85, 1e-12)
	if f.AdjN != 5 {
		t.Errorf("AdjN")
	}
	// Last 3 only.
	l3 := Form(rs, 3)
	if l3.Sequence != "WWW" || l3.N != 3 {
		t.Errorf("last-n: %q", l3.Sequence)
	}
	// Opponent adjustment: the same results against strong opposition
	// adjust upward relative to weak opposition.
	strong := append([]TeamResult(nil), rs...)
	weak := append([]TeamResult(nil), rs...)
	for i := range strong {
		strong[i].OppRating = 1750
		weak[i].OppRating = 1300
	}
	if !(Form(strong, 0).AdjPPG > Form(weak, 0).AdjPPG) {
		t.Errorf("AdjPPG should reward results against stronger opponents")
	}
	// Average opponents at alternating venues ≈ raw PPG shifted slightly.
	if e := Form(nil, 5); e.N != 0 || e.Sequence != "" {
		t.Errorf("empty form")
	}
	// Reference: neutral average-vs-average expected points ≈ 1.37.
	near(t, "xp ref", davidsonXP(0, 0.7), (3+0.7)/2.7, 1e-12)
}

func TestAvailabilityAdjust(t *testing.T) {
	squad := []PlayerContribution{
		{ID: "1", Name: "Keeper One", Position: "GK", Minutes: 2700},
		{ID: "2", Name: "Back Two", Position: "Defender", Minutes: 2600, Goals: 1, Assists: 2, XG: f64(1.1), XA: f64(1.5)},
		{ID: "3", Name: "Mid Three", Position: "M", Minutes: 2400, Goals: 4, Assists: 6, XG: f64(3.5), XA: f64(5.0)},
		{ID: "9", Name: "Striker Nine", Position: "FW", Minutes: 2500, Goals: 18, Assists: 4, XG: f64(16.0), XA: f64(3.0)},
		{ID: "10", Name: "Wing Ten", Position: "Attacker", Minutes: 1800, Goals: 7, Assists: 8, XG: f64(6.4), XA: f64(6.0)},
		{ID: "21", Name: "Keeper Two", Position: "Goalkeeper", Minutes: 90},
	}
	a, d, why := AvailabilityAdjust(squad, nil)
	if a != 1 || d != 1 || why == "" {
		t.Errorf("no absences: %v %v %q", a, d, why)
	}
	// Total involvement = Σ xG + ½ xA = 27 + 7.75 = 34.75; winger = 9.4 → 27%.
	a, d, why = AvailabilityAdjust(squad, []string{"10"})
	near(t, "winger attack", a, 1-0.4*9.4/34.75, 1e-12)
	near(t, "winger defence", d, 1, 0)
	if !strings.Contains(why, "Wing Ten") || !strings.Contains(why, "xG") {
		t.Errorf("why: %q", why)
	}
	// The striker carries 50% of the output: 1 − 0.4·0.504 < 0.8 → floored.
	a, _, _ = AvailabilityAdjust(squad, []string{"9"})
	near(t, "striker attack floored", a, AvailMinAttack, 1e-12)
	// Regular keeper: +6%; backup keeper barely matters.
	_, d, _ = AvailabilityAdjust(squad, []string{"1"})
	near(t, "gk", d, 1.06, 1e-12)
	_, d2, _ := AvailabilityAdjust(squad, []string{"21"})
	if d2 >= 1.003 {
		t.Errorf("backup keeper impact too large: %v", d2)
	}
	// Caps: everyone missing.
	a, d, _ = AvailabilityAdjust(squad, []string{"1", "2", "3", "9", "10", "21", "1"})
	near(t, "attack floor", a, AvailMinAttack, 1e-12)
	if d > AvailMaxDefence || d < 1 {
		t.Errorf("defence cap: %v", d)
	}
	_, _, why = AvailabilityAdjust(squad, []string{"9", "nope"})
	if !strings.Contains(why, "1 missing player(s) not in the squad list") {
		t.Errorf("unknown ids not mentioned: %q", why)
	}
	a, d, _ = AvailabilityAdjust(squad, []string{"nope"})
	if a != 1 || d != 1 {
		t.Errorf("only unknown ids should not adjust")
	}
	// Without xG the goals+½assists measure is used.
	noxg := []PlayerContribution{{ID: "a", Goals: 10, Assists: 0, Minutes: 900, Position: "F"}, {ID: "b", Goals: 5, Assists: 10, Minutes: 900}}
	a, _, why = AvailabilityAdjust(noxg, []string{"a"})
	near(t, "goals measure", a, 1-0.4*10.0/20.0, 1e-12)
	if !strings.Contains(why, "goals+½assists") {
		t.Errorf("measure not explained: %q", why)
	}
	for in, want := range map[string]string{"G": "GK", "Goalkeeper": "GK", "CB": "DEF", "Defender": "DEF", "M": "MID", "Midfielder": "MID", "FW": "FWD", "Attacker": "FWD", "": ""} {
		if PositionGroup(in) != want {
			t.Errorf("PositionGroup(%q) = %q", in, PositionGroup(in))
		}
	}
}

func TestResultsFor(t *testing.T) {
	x := 1.4
	ms := []Match{
		{Date: t0.AddDate(0, 0, 14), Home: "B", Away: "A", HG: 2, AG: 2},
		{Date: t0, Home: "A", Away: "B", HG: 1, AG: 0, HxG: &x, AxG: &x},
		{Date: t0.AddDate(0, 0, 7), Home: "C", Away: "D", HG: 0, AG: 0},
		{Date: t0.AddDate(0, 0, 21), Home: "A", Away: "C", HG: 0, AG: 3},
	}
	rs := ResultsFor("A", ms, t0.AddDate(0, 0, 21), func(opp string, _ time.Time) float64 {
		if opp == "B" {
			return 1600
		}
		return 0
	})
	if len(rs) != 2 || !rs[0].Home || rs[1].Home || rs[0].GF != 1 || rs[1].GF != 2 || rs[1].Opp != "B" || rs[0].OppRating != 1600 || rs[0].XGF == nil {
		t.Fatalf("ResultsFor: %+v", rs)
	}
	if f := Form(rs, 10); f.Sequence != "WD" {
		t.Errorf("sequence %q", f.Sequence)
	}
}
