package model

import (
	"math"
	"sort"
	"time"
)

// DefaultBlendW are the default pooling weights for [Dixon–Coles, Elo]
// (chosen on the EPL tuning seasons, README.md).
var DefaultBlendW = []float64{0.6, 0.4}

// BacktestOptions configures WalkForward.
type BacktestOptions struct {
	Start      time.Time  // first match day evaluated (earlier matches are history only)
	RefitEvery int        // days between Dixon–Coles refits (default 7)
	DC         DCOptions  // zero value = DefaultDCOptions()
	Elo        EloOptions // zero value = DefaultEloOptions()
	// BlendW weights [Dixon–Coles, Elo] or [Dixon–Coles, Elo, Market]
	// (market = de-margined closing odds). nil = DefaultBlendW. A zero
	// Dixon–Coles weight skips the (costly) refits entirely.
	BlendW  []float64
	EdgeMin float64 // a value bet needs EV = p·odds − 1 ≥ EdgeMin (and > 0)
	Kelly   float64 // Kelly fraction (≤ 0 = DefaultKellyFraction)

	End      time.Time // evaluate matches before End (zero = to the end)
	KellyCap float64   // stake cap as a share of bankroll (≤ 0 = DefaultKellyCap)
	DeVig    string    // method for the market's probabilities (default "shin")
	LogPool  bool      // log-linear instead of linear pooling
	CalBins  int       // calibration bins (default 10)
	MaxOdds  float64   // only bet at odds ≤ MaxOdds (0 = no limit)
	KeepRows bool      // fill BacktestResult.Rows with every forecast
	// BetOdds, when set, are the prices bets are placed at (e.g. a local
	// bookmaker's or the best available price) while `odds` stays the
	// market benchmark that is scored and blended. Matches missing from
	// BetOdds get no bet.
	BetOdds map[string][3]float64
}

// BacktestBet is one simulated value bet at the given (closing) odds.
type BacktestBet struct {
	MatchID, Selection  string // Selection "H", "D" or "A"
	P, Odds, Stake, PnL float64

	Date     time.Time
	MarketP  float64 // de-margined market probability of the selection
	EV       float64 // P·Odds − 1
	Won      bool
	Bankroll float64 // bankroll after the day's bets settled
}

// BacktestRow is one evaluated forecast (when KeepRows is set).
type BacktestRow struct {
	MatchID    string
	Date       time.Time
	Home, Away string
	Outcome    int
	DC, Elo    Probs
	Model      Probs // the blended forecast that is scored and bet on
	Market     Probs
	HasDC      bool
	HasMarket  bool
	Odds       [3]float64
	LambdaHome float64
	LambdaAway float64
}

// BacktestResult summarises a walk-forward run. Model and market metrics are
// computed on the same matches: when odds are given, only matches with valid
// odds count (N), otherwise every evaluated match.
type BacktestResult struct {
	N                                     int
	Brier, RPS, LogLoss                   float64 // the blended model
	MarketBrier, MarketRPS, MarketLogLoss float64 // de-margined closing odds
	Calib                                 []CalBin
	Bets                                  []BacktestBet
	ROI, Yield, MaxDrawdown               float64

	// ROI = profit / starting bankroll (fractional-Kelly stakes, compounding);
	// Yield = profit / total staked; MaxDrawdown = largest peak-to-trough
	// fall of the bankroll as a share of the peak.
	Components  []Scores // "dixon-coles", "elo", "model" (blend), "market" on the same matches
	MarketCalib []CalBin
	ECE         float64
	MarketECE   float64
	// Paired differences model − market per match (negative = model better)
	// with standard errors.
	RPSDiff, RPSDiffSE         float64
	BrierDiff, BrierDiffSE     float64
	LogLossDiff, LogLossDiffSE float64

	Staked, PnL, FinalBankroll float64
	FlatYield, FlatYieldSE     float64 // 1 unit per bet: mean profit per bet and its SE
	Wins                       int
	AvgOdds                    float64
	Refits, RefitFailures      int
	DrawRate, PredDrawRate     float64
	Rows                       []BacktestRow
}

func (o BacktestOptions) normalized() BacktestOptions {
	if o.RefitEvery <= 0 {
		o.RefitEvery = 7
	}
	if o.Kelly <= 0 || math.IsNaN(o.Kelly) {
		o.Kelly = DefaultKellyFraction
	}
	if o.KellyCap <= 0 || math.IsNaN(o.KellyCap) {
		o.KellyCap = DefaultKellyCap
	}
	if o.DeVig == "" {
		o.DeVig = DeVigShin
	}
	if o.CalBins <= 0 {
		o.CalBins = 10
	}
	if len(o.BlendW) == 0 {
		o.BlendW = DefaultBlendW
	}
	o.DC = o.DC.normalized()
	return o
}

// WalkForward replays the matches in date order. For every match day on or
// after Start it (1) refits Dixon–Coles if RefitEvery days have passed, using
// only matches from earlier days, (2) forecasts every match of the day with
// that fit and the current Elo ratings, (3) scores the forecasts, (4) places
// value bets at the given closing odds sized by fractional Kelly on the
// bankroll at the start of the day, and only then (5) feeds the day's results
// to Elo and settles the bets. Nothing from a match day is used before all of
// its matches are forecast, so there is no look-ahead.
//
// odds maps Match.ID → closing decimal odds [H, D, A]. Deterministic.
func WalkForward(ms []Match, odds map[string][3]float64, o BacktestOptions) BacktestResult {
	o = o.normalized()
	sorted := append([]Match(nil), ms...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.Before(b.Date)
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Home < b.Home
	})
	useOdds := len(odds) > 0
	elo := NewElo(o.Elo)
	var res BacktestResult
	var dc *DCParams
	var lastFit time.Time
	bankroll, peak := 1.0, 1.0
	var modelPs, mktPs, dcPs, eloPs []Probs
	var outcomes []int
	var calP, mcalP []float64
	var calH, mcalH []bool
	var dRPS, dBrier, dLL, flat []float64
	oddsSum := 0.0
	draws, predDraw := 0, 0.0
	sel := [3]string{"H", "D", "A"}

	for i := 0; i < len(sorted); {
		day := dayFloor(sorted[i].Date)
		j := i
		for j < len(sorted) && dayFloor(sorted[j].Date).Equal(day) {
			j++
		}
		batch := sorted[i:j]
		inRange := !day.Before(dayFloor(o.Start)) && (o.End.IsZero() || day.Before(o.End))
		if inRange {
			if weightAt(o.BlendW, 0) > 0 && (dc == nil || day.Sub(lastFit) >= time.Duration(o.RefitEvery)*24*time.Hour) {
				opt := o.DC
				opt.Warm = dc
				p, err := FitDixonColes(sorted[:i], day, opt)
				res.Refits++
				if err != nil {
					res.RefitFailures++
				}
				if p != nil {
					dc, lastFit = p, day
				}
			}
			dayStart := bankroll
			dayPnL := 0.0
			var dayBets []int
			for _, m := range batch {
				var od [3]float64
				var mkt Probs
				hasMkt := false
				if useOdds {
					var ok bool
					od, ok = odds[m.ID]
					if !ok {
						continue
					}
					mkt, _, hasMkt = DeVig3(od, o.DeVig)
					if !hasMkt {
						continue
					}
				}
				eh, ed, ea := elo.Probabilities(m.Home, m.Away, m.Neutral)
				ep := Probs{eh, ed, ea}
				var dp Probs
				var lh, la float64
				hasDC := false
				if dc != nil {
					if pr, err := dc.Predict(m.Home, m.Away, Adjust{Neutral: m.Neutral}); err == nil {
						dp = pr.Probs()
						lh, la = pr.LambdaHome, pr.LambdaAway
						hasDC = true
					}
				}
				var ps []Probs
				var ws []float64
				if hasDC {
					ps = append(ps, dp)
					ws = append(ws, weightAt(o.BlendW, 0))
				}
				ps = append(ps, ep)
				ws = append(ws, weightAt(o.BlendW, 1))
				if hasMkt && len(o.BlendW) > 2 {
					ps = append(ps, mkt)
					ws = append(ws, weightAt(o.BlendW, 2))
				}
				var mp Probs
				if o.LogPool {
					mp = BlendLogLinear(ps, ws)
				} else {
					mp = Blend(ps, ws)
				}
				out := Outcome(m.HG, m.AG)
				res.N++
				modelPs = append(modelPs, mp)
				eloPs = append(eloPs, ep)
				if hasDC {
					dcPs = append(dcPs, dp)
				} else {
					dcPs = append(dcPs, ep) // scored as Elo when DC is not yet available
				}
				outcomes = append(outcomes, out)
				if out == OutcomeDraw {
					draws++
				}
				predDraw += mp.D
				for k := 0; k < 3; k++ {
					calP = append(calP, mp.At(k))
					calH = append(calH, k == out)
				}
				if hasMkt {
					mktPs = append(mktPs, mkt)
					for k := 0; k < 3; k++ {
						mcalP = append(mcalP, mkt.At(k))
						mcalH = append(mcalH, k == out)
					}
					dRPS = append(dRPS, RPS(mp, out)-RPS(mkt, out))
					dBrier = append(dBrier, Brier(mp, out)-Brier(mkt, out))
					dLL = append(dLL, LogLoss(mp, out)-LogLoss(mkt, out))
					// Value bet: best EV selection.
					best, bestEV := -1, 0.0
					if o.BetOdds != nil {
						bo, ok := o.BetOdds[m.ID]
						if !ok || !(bo[0] > 1 && bo[1] > 1 && bo[2] > 1) {
							bo = [3]float64{}
						}
						od = bo
					}
					for k := 0; k < 3; k++ {
						if !(od[k] > 1) {
							continue
						}
						if o.MaxOdds > 0 && od[k] > o.MaxOdds {
							continue
						}
						ev := EV(mp.At(k), od[k])
						if ev > 0 && ev >= o.EdgeMin && (best < 0 || ev > bestEV) {
							best, bestEV = k, ev
						}
					}
					if best >= 0 {
						f := Kelly(mp.At(best), od[best], o.Kelly, o.KellyCap)
						if f > 0 {
							stake := f * dayStart
							won := best == out
							pnl := -stake
							fl := -1.0
							if won {
								pnl = stake * (od[best] - 1)
								fl = od[best] - 1
								res.Wins++
							}
							flat = append(flat, fl)
							oddsSum += od[best]
							dayPnL += pnl
							res.Staked += stake
							res.Bets = append(res.Bets, BacktestBet{MatchID: m.ID, Selection: sel[best], P: mp.At(best),
								Odds: od[best], Stake: stake, PnL: pnl, Date: m.Date, MarketP: mkt.At(best), EV: bestEV, Won: won})
							dayBets = append(dayBets, len(res.Bets)-1)
						}
					}
				}
				if o.KeepRows {
					res.Rows = append(res.Rows, BacktestRow{MatchID: m.ID, Date: m.Date, Home: m.Home, Away: m.Away,
						Outcome: out, DC: dp, Elo: ep, Model: mp, Market: mkt, HasDC: hasDC, HasMarket: hasMkt, Odds: od,
						LambdaHome: lh, LambdaAway: la})
				}
			}
			bankroll += dayPnL
			res.PnL += dayPnL
			for _, b := range dayBets {
				res.Bets[b].Bankroll = bankroll
			}
			if bankroll > peak {
				peak = bankroll
			}
			if peak > 0 {
				if dd := (peak - bankroll) / peak; dd > res.MaxDrawdown {
					res.MaxDrawdown = dd
				}
			}
		}
		for _, m := range batch {
			elo.Update(m)
		}
		i = j
	}

	ms3 := Score("model", modelPs, outcomes)
	res.Brier, res.RPS, res.LogLoss = ms3.Brier, ms3.RPS, ms3.LogLoss
	res.Components = []Scores{Score("dixon-coles", dcPs, outcomes), Score("elo", eloPs, outcomes), ms3}
	if len(mktPs) > 0 {
		mk := Score("market", mktPs, outcomes)
		res.MarketBrier, res.MarketRPS, res.MarketLogLoss = mk.Brier, mk.RPS, mk.LogLoss
		res.Components = append(res.Components, mk)
		res.MarketCalib = Calibration(mcalP, mcalH, o.CalBins)
		res.MarketECE = ECE(res.MarketCalib)
	}
	res.Calib = Calibration(calP, calH, o.CalBins)
	res.ECE = ECE(res.Calib)
	res.RPSDiff, res.RPSDiffSE = meanSE(dRPS)
	res.BrierDiff, res.BrierDiffSE = meanSE(dBrier)
	res.LogLossDiff, res.LogLossDiffSE = meanSE(dLL)
	res.FinalBankroll = bankroll
	res.ROI = bankroll - 1
	if res.Staked > 0 {
		res.Yield = res.PnL / res.Staked
	}
	res.FlatYield, res.FlatYieldSE = meanSE(flat)
	if len(res.Bets) > 0 {
		res.AvgOdds = oddsSum / float64(len(res.Bets))
	}
	if res.N > 0 {
		res.DrawRate = float64(draws) / float64(res.N)
		res.PredDrawRate = predDraw / float64(res.N)
	}
	return res
}

func weightAt(w []float64, i int) float64 {
	if i < len(w) {
		return w[i]
	}
	return 0
}
