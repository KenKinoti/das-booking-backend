// Package model is the statistics core of the football analysis module: pure,
// deterministic Go (standard library only, no database, no network).
//
// What is in it
//
//   - Dixon–Coles (1997) bivariate Poisson goals model with exponential time
//     decay ξ, the low-score dependence correction ρ, home advantage,
//     sum-to-zero identifiability, a ridge prior (shrinkage toward the league
//     mean) that is topped up for teams with few matches, and optional
//     blending of expected goals (xG) into the likelihood. Fitted by a damped
//     Newton method with analytic gradient and Hessian (FitDixonColes).
//     A fitted model turns into a full score matrix and from it 1X2,
//     over/under, both-teams-to-score, the most likely scorelines and
//     push-aware Asian handicaps (DCParams.Predict).
//   - Elo ratings with home advantage, goal-difference scaling (World Football
//     Elo) and a calibrated three-way draw model (Davidson 1970, or an ordered
//     logit), fitted by maximum likelihood to the league's own results (Elo).
//   - Market tools: removing the bookmaker margin (proportional, Shin 1993,
//     power), expected value, fractional Kelly (1956) with a cap, and linear /
//     log-linear pooling of several forecasts (DeVig, EV, Kelly, Blend).
//   - Form summaries and the impact of missing players (Form,
//     AvailabilityAdjust).
//   - Evaluation: Brier, ranked probability score (Epstein 1969; Constantinou &
//     Fenton 2012), log loss, calibration bins, and a walk-forward backtest
//     with no look-ahead that also scores the market's own de-margined closing
//     odds and simulates value betting at those odds (WalkForward).
//
// Conventions: probabilities are 0–1, odds are decimal, times are UTC, team
// identifiers are opaque strings. A fitted *DCParams is read-only (safe for
// concurrent Predict calls); *Elo holds running state and is not. Every
// function is deterministic: maps are always iterated in sorted key order and
// there is no randomness outside the tests' fixed-seed simulators.
//
// References
//
//   - M. J. Dixon & S. G. Coles (1997), Modelling association football scores
//     and inefficiencies in the football betting market. JRSS C 46(2), 265–280.
//   - M. J. Maher (1982), Modelling association football scores. Statistica
//     Neerlandica 36(3), 109–118.
//   - A. E. Elo (1978), The Rating of Chessplayers, Past and Present; World
//     Football Elo Ratings (eloratings.net) for the goal-difference multiplier.
//   - R. R. Davidson (1970), On extending the Bradley–Terry model to
//     accommodate ties in paired comparison experiments. JASA 65, 317–328.
//   - H. S. Shin (1993), Measuring the incidence of insider trading in a market
//     for state-contingent claims. Economic Journal 103, 1141–1153; closed form
//     as in E. Štrumbelj (2014), On determining probability forecasts from
//     betting odds. Int. J. Forecasting 30(4), 934–943.
//   - A. C. Constantinou & N. E. Fenton (2012), Solving the problem of
//     inadequate scoring rules for assessing probabilistic football forecast
//     models. J. Quantitative Analysis in Sports 8(1).
//   - J. L. Kelly (1956), A new interpretation of information rate. Bell System
//     Technical Journal 35(4), 917–926.
//   - G. W. Brier (1950), Verification of forecasts expressed in terms of
//     probability. Monthly Weather Review 78(1), 1–3.
//
// See README.md in this directory for formulas, defaults and the backtest.
package model
