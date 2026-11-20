# pkg/football/model — football match probabilities

Pure, deterministic Go (standard library only; no database, no network). It turns past results
(and, when available, expected goals and bookmaker odds) into match probabilities, and measures
how good those probabilities are. The data layer (`pkg/football`) and the UI call into it.

**Bottom line from the backtests below:** the model is well calibrated (when it says 30%, it
happens about 30% of the time), but it is consistently **less accurate than Pinnacle's closing
odds**: about +0.005 to +0.006 RPS worse in each of five leagues, each difference about 5
standard errors. Betting every "value" edge it finds at closing prices **lost money in all five
leagues** (flat yield −3% to −12%). Use the model to explain matches and as a second opinion.
The de-margined sharp market is the better probability. A model–market disagreement is
information, not a bet signal.

## API at a glance

| Area | Functions / types |
|---|---|
| Goals model | `FitDixonColes(ms, asOf, DCOptions) (*DCParams, error)`, `(*DCParams).Predict(home, away, Adjust) (Prediction, error)`, `Rates`, `Table`, `PredictFromRates(λh, λa, ρ, maxGoals)`, `ScoreGrid`, `DCTau`, `AsianHandicap`, `TopScorelines`, `LineKey`, `AHKey`, `DefaultDCOptions()` |
| Elo | `NewElo(EloOptions)`, `Update(Match)`, `Rating`, `Ratings`, `Probabilities(home, away, neutral)`, `Diff`, `DrawModelParams`, `Table`, `DrawRate`, `Expected`, `GoalDiffMultiplier`, `DefaultEloOptions()` |
| Market | `DeVig(odds, "proportional"|"shin"|"power") (probs, margin)`, `DeVig3`, `ShinZ`, `EV`, `Kelly(p, odds, fraction, cap)`, `Blend` (linear pool), `BlendLogLinear`, `DefaultKellyFraction` (0.25), `DefaultKellyCap` (0.02) |
| Form | `Form(rs, n)`, `FormWith(rs, n, FormOptions)`, `ResultsFor(team, ms, before, rating)`, `AvailabilityAdjust(squad, missingIDs)`, `PositionGroup` |
| Evaluation | `Brier`, `RPS`, `LogLoss`, `Score`, `Calibration` (with Wilson 95% CIs), `ECE`, `WalkForward(ms, closingOdds, BacktestOptions) BacktestResult`, `DefaultBlendW` |

Conventions: probabilities 0–1, decimal odds, UTC times, opaque team ids, outcomes 0 = home,
1 = draw, 2 = away. A fitted `*DCParams` is read-only, so concurrent `Predict` calls are safe.
`*Elo` keeps running state and is not safe for concurrent use. Option structs left at their zero value mean "use the defaults"
(`DCOptions{}` → `DefaultDCOptions()`, `EloOptions{}` → `DefaultEloOptions()`). No result struct
contains NaN or ±Inf, so everything is JSON-safe. Empty calibration bins are left out.

## Models

### Dixon–Coles (1997)

Goals of home side *i* and away side *j* are Poisson with means

    log λ = c + h + a_i − d_j          log μ = c + a_j − d_i

where `a` is attack, `d` is defence (**higher d = better defence**), `c` is the log goal rate
between average teams at a neutral venue (`Intercept`) and `h` is home advantage (`HomeAdv`;
exp(h) ≈ 1.29 in the EPL). The score probability is

    P(x, y) = τ(x, y) · Pois(x; λ) · Pois(y; μ)
    τ(0,0) = 1 − λμρ, τ(0,1) = 1 + λρ, τ(1,0) = 1 + μρ, τ(1,1) = 1 − ρ, else 1

The ρ term keeps total probability exactly 1. Typical fitted ρ is −0.04 to −0.15, meaning
slightly more 0-0 and 1-1 than independent Poissons give.

Fitting maximises the time-weighted, penalised log-likelihood

    Σ_k exp(−ξ·days_k) · log P(x_k, y_k)  −  Σ_i ½·P_i·[(a_i − m_a)² + (d_i − m_d)²]

over the matches strictly before `asOf`:

- **Identifiability:** Σa = Σd = 0 holds exactly. The optimiser works in the reduced space, where
  the last team's parameter is minus the sum of the others.
- **Ridge prior:** P_i = `Prior` (default 4). One unit is worth about 0.7 matches of information
  on the log scale, so a team with two seasons of data is shrunk by about 5%.
- **Few matches:** a team with n < `MinMatches` (default 6) matches gets (MinMatches − n) ×
  (mean goals) of extra precision. That is equivalent to topping it up with phantom
  average-team matches. The prior centre m is the league mean (0) unless `NewTeamAttack` /
  `NewTeamDefence` are set. A newcomer centre of −0.15 helped by only 0.00003 RPS in the EPL, so
  the default stays at the league mean.
- **Time decay:** default ξ = 0.0019/day (half-life ≈ 1 year). Matches whose weight is below
  `MinWeight` (0.02, ≈ 5.6 years) are dropped.
- **xG blending (`UseXG` 0–1):** when both HxG and AxG are present, the Poisson part uses the
  count (1 − u)·goals + u·xG (a quasi-likelihood). The ρ term always uses the real score.
- **Optimiser:** Levenberg–Marquardt damped Newton with the analytic gradient and Hessian,
  including the τ terms. It stops when the Newton decrement is below 1e-10. Typical fits take
  5 iterations. A fit can warm-start (`Warm`) without changing the optimum. The approximate
  standard errors (`AttackSE`, `HomeAdvSE`, `RhoSE`…) come from the inverse penalised Hessian.
  With time decay they are indicative only.

**Predictions.** Markets are computed on a score grid large enough that the truncated mass is
below 1e-12. `Matrix` is that grid cut at `MaxGoals` (10) and renormalised to sum to 1; the mass
that was cut is reported in `MatrixTail`.

- 1X2, `Over["0.5".."5.5"]` = P(total > line), `BTTS`, clean sheets, the expected goals of the
  ρ-adjusted distribution, and the six most likely scores.
- **Asian handicap, push-aware.** For each line from −1.5 to +1.5 in quarter steps,
  `AH[line]` gives the settlement split (win / half-win / push / half-loss / loss). Quarter lines
  split the stake over the two neighbouring lines.
- `AHHome[line]` = (W + HW/2) / (W + HW/2 + L + HL/2) = 1 / fair odds. For example, AH 0 is
  P(H) / (P(H) + P(A)) (draw no bet), and −0.25 is P(H) / (P(H) + P(D)/2 + P(A)). The away side
  at the mirrored line is 1 − AHHome. The true EV of a bet at odds o is
  (W + HW/2)(o − 1) − (L + HL/2).
- `Adjust` multiplies expected goals: `HomeAttack` scales what the home side scores and
  `HomeDefence` scales what it **concedes** (> 1 = weaker), and the same for the away side.
- Teams the fit has not seen are rated at the newcomer centre and listed in `Unknown`. They do
  not cause an error.

### Elo with a calibrated draw model

- **Update:** World Football Elo: R ← R ± K·G·(W − E), with E = 1/(1 + 10^(−dr/400)) and
  dr = R_home + HomeAdv − R_away.
- **Goal-difference multiplier G:** 1 for |gd| ≤ 1, 1.5 for 2, (11 + |gd|)/8 for 3 or more.
  Turning it off cost 0.0005 RPS in tuning.
- **Davidson (1970) draw model** (default): with r = 10^(β·(dr + s)/800),
  P(H) = r / (r + 1/r + ν), P(D) = ν / (·), P(A) = (1/r) / (·).
  - ν (draws), β (scale) and s (shift) are fitted by maximum likelihood on the last 3,000 matches
    in which both teams already had 10 or more games. They are refitted after every 50 new
    matches.
  - At the optimum, Σ P(draw) equals the number of draws: the model reproduces the league's own
    draw rate. Tests confirm this to 4 decimals.
  - `"ordlogit"` (ordered logit with two cut points) is an alternative. It scored the same.
- **New teams:** a team seen for the first time once the league is established starts at the
  active mean + `NewTeamOffset` (−50; for promoted sides).

### Blending

`Blend` is a linear opinion pool (renormalised weighted average). `BlendLogLinear` is a
geometric pool. The default `DefaultBlendW` = [0.6 Dixon–Coles, 0.4 Elo], linear. Log pooling
scored 0.00003 RPS worse.

### Market tools

- **`DeVig`** returns probabilities and margin = Σ(1/o) − 1:
  - proportional: p_i = π_i/Σπ.
  - power: p_i = π_i^k, with k solved so Σp = 1.
  - Shin (1993): p_i = (√(z² + 4(1 − z)·π_i²/Π) − z) / (2(1 − z)), with z solved by bisection
    (Štrumbelj 2014). For two outcomes Shin equals the additive method, which the tests check.
  - On EPL closing prices the three methods are within 0.00005 RPS of each other. Shin is the
    default.
- **`EV`** = p·o − 1.
- **`Kelly`** = fraction × (p·o − 1)/(o − 1), never negative and at most `cap`. The product
  defaults are ¼ Kelly with a 2%-of-bankroll cap. Invalid input (odds ≤ 1, NaN, p ≤ 0, no edge)
  returns 0.

### Form and missing players

`Form(rs, n)` summarises the last n results (oldest → newest; filter on `Home` for home-only or
away-only form):

- W/D/L, points, PPG, goals for and against, xG sums, the "WWDLW" sequence, clean sheets.
- `WeightedPPG`: weight 0.5^(age/4), so the half-life is 4 matches.
- `Trend`: OLS slope of the 5-match rolling PPG (n/2 when there are fewer than 8 matches), in PPG
  per match.
- `AdjPPG` = mean(points − xPts_avg) + 1.37. Here xPts_avg is what an average team would expect
  against that opponent (Elo-scale `OppRating`, 0 = unknown) at that venue, under a Davidson
  model (home advantage 65, ν = 0.7), and 1.37 is an average team's expected points at a neutral
  venue. Results against stronger opponents therefore count for more.

`AvailabilityAdjust(squad, missing)` is a documented, deliberately conservative mapping:

- A player's attacking share = (xG + ½xA) / team total. If less than half of the squad's minutes
  have xG, it uses (goals + ½assists) instead.
- A replacement is assumed to recover 60% of the missing output, so
  attackMult = 1 − 0.4·Σshares, floored at 0.80.
- A missing regular goalkeeper adds +6% to goals conceded and each regular defender +3%, scaled
  by minutes / the squad maximum. defenceMult is capped at 1.15. Midfielders' defensive work is
  ignored.
- It returns a plain-English explanation.
- These constants are judgement, not fitted. No public data was available here to calibrate
  them. Treat the multipliers as a sensitivity check.

### Evaluation and the walk-forward backtest

- **Scores:**
  - Brier = Σ(p − o)² over H/D/A (0–2; uniform = 0.667).
  - RPS = ½[(P_H − O_H)² + (P_H + P_D − O_H − O_D)²] (Epstein 1969; recommended for football by
    Constantinou & Fenton 2012; 0–1).
  - Log loss = −ln p(outcome).
  - `Calibration` uses equal-width bins with Wilson 95% intervals. `ECE` = the N-weighted
    mean |forecast − observed|.
- **`WalkForward` protocol.** It replays matches day by day. For each match day on or after
  `Start`, in this order:
  1. refit Dixon–Coles every `RefitEvery` days (default 7) on matches from earlier days only;
  2. forecast every match of the day;
  3. score the forecasts against the de-margined closing odds, on the same matches;
  4. place at most one value bet per match: the selection with the highest EV ≥ `EdgeMin`,
     staked at ¼ Kelly capped at 2% of the bankroll at the start of the day;
  5. only then feed the day's results to Elo and settle the bets.

  Tests change all results from a day onward and check that no earlier or same-day forecast
  moves.
- **Betting metrics:**
  - ROI = profit / starting bankroll, with compounding Kelly stakes.
  - Yield = profit / amount staked.
  - Flat yield = mean profit per 1-unit bet, ± its standard error.
  - Max drawdown = the largest peak-to-trough fall as a share of the peak.
- `BetOdds` (optional) places the bets at other prices, such as a local bookmaker's or the best
  available price. Scoring stays against the benchmark odds.

## Defaults and why

| Setting | Default | Evidence (EPL tuning seasons 2014/15–2018/19, N = 1,900) |
|---|---|---|
| ξ (decay/day) | 0.0019 | Best RPS was ξ = 0.0015 (0.19609) against 0.19616 at 0.0019, which is within noise. 0.0019 is the literature and contract value. ξ = 0.0025 was worse (0.19626) |
| Prior | 4 | Prior 0: 0.19635, prior 2–4: 0.19616, prior 8: worse |
| MinMatches | 6 | (as specified) |
| Newcomer centre | 0 (league mean) | −0.15: 0.19613 against 0.19616. Not worth departing from the contract |
| UseXG | 0 | Only takes effect when xG is present. **Recommended: 0.5 when a league has good xG** (see the xG test below) |
| MaxGoals | 10 | Tail mass < 1e-4 for λ ≤ 3.5 |
| Elo K / home / newcomer | 15 / 80 / −50 | Best of 144 combinations (0.19641–0.19646 for K 10–15). K 20 / home 80 / no offset scored 0.19666 |
| Draw model | Davidson | Ordered logit was within 0.0003 |
| Blend DC/Elo | 0.6 / 0.4 linear | With the best DC fit: DC alone 0.19609, 0.7/0.3 0.19571, 0.6/0.4 0.19570, 0.5/0.5 0.19571. The final defaults score 0.19572 (market 0.19131) |
| Kelly | ¼, capped at 2% | Product policy |
| DeVig for scoring | Shin | Proportional 0.19135, power 0.19130, Shin 0.19131 |

All tuning differences between sensible settings are ≤ 0.0005 RPS, which is about one standard
error of a paired RPS difference over 1,900 matches. The defaults are reasonable rather than
optimal. Out of sample, the original untuned guesses scored about the same (blend RPS 0.2029 vs
0.2030 on the EPL test seasons).

## Validation

### Unit tests

`go test ./pkg/football/model/` runs in about 3 s:

- Hand-checked score grid and τ values; the τ correction keeps mass at 1 to 1e-12.
- Every market checked against an independent large grid: 1X2, over/under, BTTS, the Asian
  handicap identities (AH 0 = draw no bet, −0.25, ±0.5, quarter-line half results), and
  monotonicity of the AH and over lines.
- Symmetry: equal teams at a neutral venue give P(H) = P(A) and a symmetric matrix.
- The analytic gradient and Hessian match finite differences.
- The fit is order-independent and deterministic, and a warm start reaches the same optimum.
- The fit ignores matches on or after `asOf`.
- Shrinkage and newcomer behaviour, xG blending, and the 20-team / 2-season speed check.
- `DeVig` against reference values computed independently in Python/SciPy, plus edge cases
  (invalid odds, arbitrage).
- Kelly edge cases: no edge, odds ≤ 1, p = 0, NaN, cap, fraction clamp.
- Blend: linear and log-linear.
- Elo: a hand-checked update, zero-sum, and the draw model fitted to the league draw rate for
  both draw models.
- Form hand-checked (counts, weighted PPG, trend = 0.85 on a worked example, the opponent
  adjustment) and every `AvailabilityAdjust` branch.
- Brier/RPS/log-loss hand values, calibration bins and the Wilson interval.
- WalkForward:
  - **no look-ahead**, by scrambling results from a cut day and checking that every forecast up
    to and including that day is identical;
  - determinism;
  - a "perfect bookmaker" simulation (odds = true probability + 5% margin), where the model must
    not beat the market and the true EV of its bets is −4.8%;
  - accounting identities.
- A golden regression test on `testdata/synthetic_league.csv`: 180 matches, 10 invented clubs,
  simulated, no real data.

### Parameter recovery on simulated leagues (known parameters)

| Check | Result |
|---|---|
| One league, 20 teams × 6 seasons (2,280 matches), ξ = 0, no prior | corr(attack) 0.978, corr(defence) 0.938, RMSE 0.068. Home 0.248 (true 0.25) ± 0.025; ρ −0.152 (true −0.10) ± 0.028; intercept 0.118 (true 0.12) |
| 50 leagues × 2 seasons (760 matches each) | Home advantage: bias −0.002, empirical SD 0.039, mean reported SE 0.044, 95% coverage 100%. ρ: bias +0.003, SD 0.060, reported SE 0.049, **coverage 84%** (the ρ SE is about 20% optimistic). Attack: RMSE 0.099, coverage 96% |
| xG blend with perfect xG (true rates), 10 teams × 2 seasons | Parameter RMSE 0.192 → 0.058 at UseXG = 0.7 |
| Elo draw model, 20 teams × 8 seasons | Fitted draw rate equals observed in sample (0.2721 vs 0.2721) |

### Independent cross-check

One real EPL fit window (1,621 matches, 29 teams, ξ = 0.0019, prior 4) was re-fitted from scratch
with SciPy L-BFGS-B, numerical gradients and the same penalised likelihood. All 61 parameters
agreed within 1.3e-5, and so did the log-likelihood (−1464.8706). The Go fit took 4 ms
(5 Newton steps); SciPy needed 97 iterations.

### Speed

20 teams × 2 seasons fits in about 1.5 ms. The cost grows with the number of teams cubed
(dense Newton): 40 teams take 3 ms, 100 teams 50 ms and 200 teams 0.22 s. A full EPL walk-forward with weekly refits over 7½
seasons (245 refits) takes about 2.5 s on the 2-CPU sandbox.

## Real-data backtest

**Data.** These are football-data.co.uk CSVs as mirrored on GitHub, cloned locally and not stored
in this repo: `vijinho/epl_mysql_db` (EPL 2012/13–2016/17), `nickpadd/EuropeanFootballLeaguePredictor`
(5 leagues 2017/18–2023/24) and `Lonjedzeke/football-model` (5 leagues 2024/25–2026/27, the
current season to 8 Nov 2026).

- **Odds:** Pinnacle closing 1X2 (`PSCH/PSCD/PSCA`), falling back to the market-average closing
  price (`AvgC*`) where Pinnacle is missing. That applies to every 2026/27 match (the 2026/27
  files have no Pinnacle) and 5–9% of matches overall.
- **xG experiment:** Understat per-match xG from `vaastav/Fantasy-Premier-League` (MIT,
  2019/20–2024/25).

**Protocol.**

- Tuning: the EPL 2014/15–2018/19 only, with 2012/13–2013/14 as burn-in.
- Out of sample: 2019/20 to 8 Nov 2026 with the frozen defaults, for the EPL and, untuned, for
  the Bundesliga, La Liga, Serie A and Ligue 1 (their burn-in is 2017/18–2018/19).
- Forecasts are pre-match from data up to the previous day. Market = Shin-de-margined closing
  odds. ± is one standard error of the paired per-match difference.

### Accuracy, out of sample (2019/20 – Nov 2026)

| League | N | Model RPS | Market RPS | Model − market | Model log loss | Market log loss | Model Brier | Market Brier | ECE model / market |
|---|---|---|---|---|---|---|---|---|---|
| Premier League | 2,760 | 0.2030 | 0.1971 | +0.0059 ± 0.0011 | 0.9836 | 0.9658 | 0.5853 | 0.5730 | 0.009 / 0.004 |
| Bundesliga | 2,223 | 0.2043 | 0.1980 | +0.0063 ± 0.0012 | 0.9938 | 0.9738 | 0.5929 | 0.5786 | 0.011 / 0.011 |
| La Liga | 2,779 | 0.1964 | 0.1914 | +0.0050 ± 0.0010 | 0.9826 | 0.9657 | 0.5851 | 0.5737 | 0.011 / 0.014 |
| Serie A | 2,770 | 0.1959 | 0.1904 | +0.0055 ± 0.0009 | 0.9770 | 0.9590 | 0.5815 | 0.5694 | 0.007 / 0.011 |
| Ligue 1 | 2,427 | 0.2074 | 0.2017 | +0.0057 ± 0.0011 | 1.0026 | 0.9839 | 0.5988 | 0.5866 | 0.006 / 0.006 |

Components, by RPS:

| League | Dixon–Coles | Elo | Blend |
|---|---|---|---|
| EPL | 0.2033 | 0.2045 | 0.2030 |
| Bundesliga | 0.2045 | 0.2051 | 0.2043 |
| La Liga | 0.1973 | 0.1968 | 0.1964 |
| Serie A | 0.1963 | 0.1969 | 0.1959 |
| Ligue 1 | 0.2077 | 0.2080 | 0.2074 |

The blend beats both components in every league.

- **Draw rates, observed vs mean predicted:** EPL 0.238 / 0.235, Bundesliga 0.246 / 0.245, La Liga
  0.267 / 0.262, Serie A 0.259 / 0.253, Ligue 1 0.246 / 0.251.
- **Pooling with the market:** 20% model + 80% market scores +0.0003 to +0.0005 RPS against the
  market alone. The model adds nothing to the closing line. On the tuning seasons, 10% model +
  90% market was also slightly worse than the market alone.

EPL by season (RPS; model / market):

| Season | 19/20 | 20/21 | 21/22 | 22/23 | 23/24 | 24/25 | 25/26 | 26/27* |
|---|---|---|---|---|---|---|---|---|
| Model | 0.1985 | 0.2183 | 0.1936 | 0.2068 | 0.1897 | 0.2046 | 0.2085 | 0.2078 |
| Market | 0.1985 | 0.2112 | 0.1889 | 0.1974 | 0.1802 | 0.1962 | 0.2054 | 0.2048 |

\* 100 matches; the benchmark is the market-average closing price because there is no Pinnacle
data. 2020/21, played without crowds, shows the slow adaptation of a decayed home-advantage
estimate.

EPL calibration (H, D and A forecasts pooled, out of sample):

| Forecast bin | N | Mean forecast | Observed (95% CI) |
|---|---|---|---|
| 0.0–0.1 | 264 | 0.072 | 0.087 (0.059–0.127) |
| 0.1–0.2 | 1,174 | 0.160 | 0.150 (0.131–0.171) |
| 0.2–0.3 | 3,316 | 0.249 | 0.258 (0.243–0.273) |
| 0.3–0.4 | 1,142 | 0.350 | 0.352 (0.325–0.380) |
| 0.4–0.5 | 945 | 0.450 | 0.425 (0.394–0.457) |
| 0.5–0.6 | 665 | 0.547 | 0.546 (0.508–0.583) |
| 0.6–0.7 | 465 | 0.647 | 0.654 (0.609–0.696) |
| 0.7–0.8 | 237 | 0.746 | 0.738 (0.679–0.790) |
| 0.8–0.9 | 69 | 0.837 | 0.841 (0.737–0.909) |

### Value bets at closing odds (honest ROI)

These are the model's best-EV selections, staked at ¼ Kelly capped at 2% of the bankroll, from
2019/20 onward. Bets placed at the Pinnacle closing price:

| League, edge threshold | Bets | Avg odds | Yield (Kelly) | Flat yield ± SE | Bankroll ROI | Max drawdown |
|---|---|---|---|---|---|---|
| EPL ≥ 2% | 2,401 | 4.63 | −3.9% | −3.5% ± 3.7% | −93.8% | 96.7% |
| EPL ≥ 5% | 2,089 | 4.85 | −4.9% | −5.4% ± 4.0% | −94.3% | 96.4% |
| EPL ≥ 10% | 1,664 | 5.21 | −7.7% | −11.2% ± 4.6% | −96.5% | 96.8% |
| Bundesliga ≥ 5% | 1,581 | 4.76 | −7.0% | −7.7% ± 5.0% | −86.6% | 93.6% |
| La Liga ≥ 5% | 2,055 | 4.30 | −6.5% | −12.3% ± 3.6% | −95.6% | 96.2% |
| Serie A ≥ 5% | 1,976 | 4.38 | −9.0% | −11.2% ± 3.9% | −96.3% | 97.9% |
| Ligue 1 ≥ 5% | 1,797 | 4.47 | −10.8% | −11.5% ± 4.1% | −95.0% | 96.0% |

A higher edge threshold does not help: the biggest "edges" are mostly model error.

| Other prices, edge ≥ 2% | Bets | Flat yield ± SE | Bankroll ROI |
|---|---|---|---|
| EPL, model edges at the **market-average** closing price (≈ a typical bookmaker) | 2,231 | −5.7% ± 3.9% | −95.2% |
| EPL, model edges at the **best** closing price across bookmakers | 2,673 | +0.7% ± 3.9% | −62.6% |
| EPL, **no model**: Pinnacle fair probability vs the best price | 1,612 | +3.4% ± 5.2% | +14.6% |
| Five leagues, Pinnacle fair probability vs the best price | 1,257–1,612 each | −0.8% to +3.4% | −16% to +84% |

The only non-negative results come from price shopping against a sharp, de-margined price. Even
those are within about one standard error of zero. Real accounts at the best-priced books get
limited, and the "best price" column is a hindsight maximum over many books.

### Expected goals (UseXG), EPL with Understat xG

UseXG was chosen on 2020/21–2021/22 (best 0.5) and tested on 2022/23–2024/25 (N = 1,140). Dixon–Coles RPS:

| UseXG | 0 | 0.25 | 0.5 | 0.75 | 1 |
|---|---|---|---|---|---|
| Tuning RPS | 0.20471 | 0.20412 | **0.20385** | 0.20389 | 0.20426 |
| Test RPS | 0.20125 | 0.20057 | **0.20021** | 0.20018 | 0.20048 |
| Test, vs goals only | – | −0.00068 ± 0.00030 | −0.00104 ± 0.00061 | −0.00107 ± 0.00091 | −0.00077 ± 0.00121 |

Half goals, half xG is a small but consistent gain: about a sixth of the gap to the market. The
market's RPS on the test seasons was 0.19126.

## Limitations

- **The market is better.** No setting tried here closes the gap to Pinnacle's closing line
  (≈ 0.005 RPS). The model uses results (and optionally xG) only: no line-ups, injuries, news,
  motivation or schedule congestion.
- **Home advantage** has one decayed estimate per fit. It adapts slowly to shocks such as the
  2020/21 empty stadiums.
- **One competition per fit.** Cups and European ties, which mix leagues, need a joint fit with
  league-strength terms. That is not implemented. Fits on short cup histories are weak and lean
  on the prior.
- **Promoted teams** start near the league mean in DC (by contract) and at mean − 50 in Elo.
  Feeding the previous division's matches into the fit is not supported, because there is a
  single intercept.
- **Uncertainty:** the ρ standard error is about 20% optimistic. With time decay, all SEs are
  indicative.
- **Missing players:** `AvailabilityAdjust` constants are expert judgement, not fitted, so it is
  deliberately conservative.
- **Data coverage:** the backtest covers the top-5 European leagues only. Kenyan and other
  African leagues have no public closing-odds history here. Their accuracy is unmeasured until
  the data layer has collected and graded its own predictions.
- **Market price of the current season:** 2026/27 is benchmarked against market-average closing
  prices (no Pinnacle in the source).

## Reproduce

These tests are skipped unless the environment variables are set:

    FBMODEL_DATA=<dir with E0/1415.csv …> go test -run 'TestRealData$' -v ./pkg/football/model/
    FBMODEL_DATA=… FBMODEL_TUNE=1 go test -run TestRealDataTune -v -timeout 60m ./pkg/football/model/
    FBMODEL_DATA=… FBMODEL_XG=<Fantasy-Premier-League/data> go test -run TestRealDataXG -v ./pkg/football/model/
    FBMODEL_DATA=… FBMODEL_XCHECK=<out dir> go test -run TestRealDataExportFit ./pkg/football/model/   # rows for the SciPy cross-check

The expected layout is `<league>/<yyyy>.csv` (for example `E0/1920.csv`), in football-data.co.uk
column format. To regenerate the synthetic fixture:
`FBMODEL_WRITE_FIXTURE=1 go test -run TestWriteFixture`.

## References

Dixon & Coles (1997) JRSS C 46:265–280 · Maher (1982) Statistica Neerlandica 36:109–118 ·
Elo (1978); eloratings.net (World Football Elo) · Davidson (1970) JASA 65:317–328 · Shin (1993)
Economic Journal 103:1141–1153 · Štrumbelj (2014) IJF 30:934–943 · Epstein (1969) J. Appl.
Meteorology 8:985–987 · Constantinou & Fenton (2012) JQAS 8(1) · Brier (1950) Monthly Weather
Review 78:1–3 · Kelly (1956) Bell System Technical Journal 35:917–926 · Wilson (1927) JASA 22:209–212.
