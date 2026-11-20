package model

import (
	"math"
	"reflect"
	"testing"
	"time"
)

func TestScoringRulesHandChecked(t *testing.T) {
	p := Probs{0.5, 0.3, 0.2}
	near(t, "brier H", Brier(p, 0), 0.25+0.09+0.04, 1e-15)
	near(t, "brier D", Brier(p, 1), 0.25+0.49+0.04, 1e-15)
	near(t, "rps H", RPS(p, 0), 0.5*(0.25+0.04), 1e-15)
	near(t, "rps D", RPS(p, 1), 0.5*(0.25+0.04), 1e-15)
	near(t, "rps A", RPS(p, 2), 0.5*(0.25+0.64), 1e-15)
	near(t, "logloss H", LogLoss(p, 0), math.Ln2, 1e-15)
	near(t, "logloss A", LogLoss(p, 2), -math.Log(0.2), 1e-15)
	u := Probs{1.0 / 3, 1.0 / 3, 1.0 / 3}
	near(t, "uniform brier", Brier(u, 1), 2.0/3, 1e-15)
	near(t, "uniform logloss", LogLoss(u, 2), math.Log(3), 1e-15)
	near(t, "perfect rps", RPS(Probs{0, 0, 1}, 2), 0, 0)
	near(t, "worst rps", RPS(Probs{1, 0, 0}, 2), 1, 0)
	if v := LogLoss(Probs{1, 0, 0}, 2); math.IsInf(v, 0) || v < 30 {
		t.Errorf("log loss floor: %v", v)
	}
	// RPS respects order: predicting H when D happens is less bad than when A happens.
	if !(RPS(Probs{1, 0, 0}, 1) < RPS(Probs{1, 0, 0}, 2)) {
		t.Errorf("RPS ordering")
	}
	s := Score("x", []Probs{p, u}, []int{0, 1})
	near(t, "score mean brier", s.Brier, (0.38+2.0/3)/2, 1e-12)
}

func TestCalibrationBins(t *testing.T) {
	ps := []float64{0.05, 0.12, 0.15, 0.95, 1.0, 0.5, math.NaN()}
	hits := []bool{false, true, false, true, true, false, true}
	bins := Calibration(ps, hits, 10)
	if len(bins) != 4 {
		t.Fatalf("want 4 non-empty bins, got %d: %+v", len(bins), bins)
	}
	b := bins[1] // 0.1–0.2
	if b.N != 2 || b.Freq != 0.5 || math.Abs(b.MeanP-0.135) > 1e-12 || b.Lo != 0.1 {
		t.Errorf("bin: %+v", b)
	}
	if last := bins[3]; last.N != 2 || last.Hi != 1 || last.Freq != 1 {
		t.Errorf("p = 1 must land in the last bin: %+v", last)
	}
	for _, b := range bins {
		if b.CILo > b.Freq || b.CIHi < b.Freq || b.CILo < 0 || b.CIHi > 1 {
			t.Errorf("wilson interval: %+v", b)
		}
	}
	// Wilson 95% for 5/10: 0.2366–0.7634.
	lo, hi := wilson(5, 10)
	near(t, "wilson lo", lo, 0.2365931, 1e-6)
	near(t, "wilson hi", hi, 0.7634069, 1e-6)
	near(t, "ece", ECE([]CalBin{{MeanP: 0.2, Freq: 0.3, N: 10}, {MeanP: 0.6, Freq: 0.6, N: 30}}), 0.025, 1e-12)
}

// simWithOdds simulates a league and odds = true probabilities with a 5%
// proportional margin (a "perfect" bookmaker).
func simWithOdds(seasons int, seed int64) ([]Match, map[string][3]float64, simLeague) {
	l := newSimLeague(20, seed)
	ms := l.simulate(seasons, t0, seed+1)
	odds := map[string][3]float64{}
	for _, m := range ms {
		odds[m.ID] = marketOdds(l.probs(m.Home, m.Away), 0.05)
	}
	return ms, odds, l
}

func TestWalkForwardNoLookAhead(t *testing.T) {
	ms, odds, _ := simWithOdds(3, 41)
	start := ms[380].Date // start of season 2
	opt := BacktestOptions{Start: start, RefitEvery: 7, BlendW: []float64{0.7, 0.3}, EdgeMin: 0.02, KeepRows: true}
	base := WalkForward(ms, odds, opt)
	// Pick a mid-backtest day and scramble that day's and all later results.
	cut := dayFloor(ms[900].Date)
	scr := append([]Match(nil), ms...)
	for i := range scr {
		if !dayFloor(scr[i].Date).Before(cut) {
			scr[i].HG, scr[i].AG = scr[i].AG+3, scr[i].HG
		}
	}
	alt := WalkForward(scr, odds, opt)
	checked, sameDay := 0, 0
	for i := range base.Rows {
		r := base.Rows[i]
		if dayFloor(r.Date).After(cut) {
			break
		}
		a := alt.Rows[i]
		if r.MatchID != a.MatchID || r.Model != a.Model || r.DC != a.DC || r.Elo != a.Elo {
			t.Fatalf("forecast for %s (%s) changed when results on/after %s changed", r.MatchID, r.Date.Format("2006-01-02"), cut.Format("2006-01-02"))
		}
		checked++
		if dayFloor(r.Date).Equal(cut) {
			sameDay++
		}
	}
	if checked < 400 || sameDay == 0 {
		t.Fatalf("checked %d forecasts (%d on the cut day)", checked, sameDay)
	}
	// And the scramble does change later forecasts (the test has teeth).
	changed := false
	for i := range base.Rows {
		if dayFloor(base.Rows[i].Date).After(cut.AddDate(0, 0, 8)) && base.Rows[i].Model != alt.Rows[i].Model {
			changed = true
			break
		}
	}
	if !changed {
		t.Fatalf("later forecasts did not react to changed results")
	}
}

func TestWalkForwardDeterministic(t *testing.T) {
	ms, odds, _ := simWithOdds(2, 51)
	opt := BacktestOptions{Start: ms[380].Date, EdgeMin: 0.03, KeepRows: true}
	a := WalkForward(ms, odds, opt)
	// Shuffled input order must not matter.
	rev := make([]Match, len(ms))
	for i := range ms {
		rev[len(ms)-1-i] = ms[i]
	}
	b := WalkForward(rev, odds, opt)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("WalkForward is not deterministic / order independent")
	}
}

// With a bookmaker that knows the true probabilities (plus 5% margin) the
// model cannot beat the market and value betting must not show a profit
// beyond noise — a guard against leakage and accounting bugs.
func TestWalkForwardAgainstPerfectMarket(t *testing.T) {
	ms, odds, l := simWithOdds(5, 61)
	start := ms[760].Date
	res := WalkForward(ms, odds, BacktestOptions{Start: start, EdgeMin: 0.0, KeepRows: true})
	t.Logf("N=%d model RPS %.4f vs market %.4f (diff %+.4f ± %.4f); log loss %.4f vs %.4f; ECE %.4f; bets %d, flat yield %+.3f ± %.3f, ROI %+.3f, maxDD %.3f",
		res.N, res.RPS, res.MarketRPS, res.RPSDiff, res.RPSDiffSE, res.LogLoss, res.MarketLogLoss, res.ECE, len(res.Bets), res.FlatYield, res.FlatYieldSE, res.ROI, res.MaxDrawdown)
	if res.N != len(ms)-760 {
		t.Errorf("N = %d", res.N)
	}
	if res.RPSDiff < 0 {
		t.Errorf("model beat a perfect market: %v", res.RPSDiff)
	}
	if res.FlatYield > 2*res.FlatYieldSE {
		t.Errorf("profit against a perfect market: %.3f ± %.3f", res.FlatYield, res.FlatYieldSE)
	}
	// Expected flat yield of a bet chosen by the model = Σ (p_true·odds − 1).
	evSum := 0.0
	for _, b := range res.Bets {
		var r BacktestRow
		for _, row := range res.Rows {
			if row.MatchID == b.MatchID {
				r = row
			}
		}
		k := map[string]int{"H": 0, "D": 1, "A": 2}[b.Selection]
		evSum += l.probs(r.Home, r.Away).At(k)*b.Odds - 1
	}
	if n := float64(len(res.Bets)); n > 0 {
		t.Logf("true expected flat yield of the model's bets: %+.4f (≈ −margin)", evSum/n)
		if evSum/n > -0.02 {
			t.Errorf("true EV of bets should be about −margin: %v", evSum/n)
		}
	}
	// Accounting identities.
	staked, pnl := 0.0, 0.0
	for _, b := range res.Bets {
		staked += b.Stake
		pnl += b.PnL
		if b.Stake > DefaultKellyCap*b.Bankroll*1.2 && b.Stake > DefaultKellyCap*2 {
			t.Errorf("stake above cap: %+v", b)
		}
		if b.EV <= 0 {
			t.Errorf("bet without edge: %+v", b)
		}
	}
	near(t, "staked", res.Staked, staked, 1e-9)
	near(t, "pnl", res.PnL, pnl, 1e-9)
	near(t, "roi", res.ROI, res.FinalBankroll-1, 1e-12)
	near(t, "yield", res.Yield, pnl/staked, 1e-12)
	if res.MaxDrawdown < 0 || res.MaxDrawdown > 1 {
		t.Errorf("drawdown %v", res.MaxDrawdown)
	}
	if len(res.Calib) == 0 || len(res.MarketCalib) == 0 || len(res.Components) != 4 {
		t.Errorf("missing calibration/components")
	}
}

func TestWalkForwardWithoutOdds(t *testing.T) {
	ms, _, _ := simWithOdds(2, 71)
	res := WalkForward(ms, nil, BacktestOptions{Start: ms[380].Date})
	if res.N != 380 || res.MarketRPS != 0 || len(res.Bets) != 0 {
		t.Errorf("no-odds run: N=%d market %v bets %d", res.N, res.MarketRPS, len(res.Bets))
	}
	if res.RPS <= 0 || res.RPS > 0.25 {
		t.Errorf("RPS %v", res.RPS)
	}
	_ = time.Second
}
