package football

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// The verdict: one plain line per match ("Arsenal win · 64% · Strong") with
// the data-science detail one tap away. Pure functions (no DB): the inputs
// are the consensus probabilities and whatever goal markets are known.

// VerdictHistory is a tier's record in open history (history_tables.go).
type VerdictHistory struct {
	HitRate    float64 `json:"hit_rate"`
	N          int     `json:"n"`
	DCHitRate  float64 `json:"double_chance_hit_rate"`
	PODHitRate float64 `json:"pick_or_draw_hit_rate"`
	DrawRate   float64 `json:"draw_rate"`
	Share      float64 `json:"share"`
	Source     string  `json:"source"`
}

// OwnRecord is DASYIN's own graded record for a tier (shown from 30 graded).
type OwnRecord struct {
	HitRate float64 `json:"hit_rate"`
	N       int     `json:"n"`
}

// TierOut is a verdict's tier.
type TierOut struct {
	Key     string         `json:"key"`
	Label   string         `json:"label"`
	Range   string         `json:"range"`
	History VerdictHistory `json:"history"`
	Own     *OwnRecord     `json:"own"`
	Note    string         `json:"note"`
}

// SaferOut is the safer double-chance pick.
type SaferOut struct {
	Selection      string  `json:"selection"`
	Label          string  `json:"label"`
	P              float64 `json:"p"`
	FairOdds       float64 `json:"fair_odds"`
	HistoryHitRate float64 `json:"history_hit_rate"`
}

// DrawAlert flags games where the draw is close to the top outcome.
type DrawAlert struct {
	On    bool    `json:"on"`
	PDraw float64 `json:"p_draw"`
	Why   string  `json:"why"`
}

// GoalsOut is the over/under 2.5 lean.
type GoalsOut struct {
	Line   float64 `json:"line"`
	Pick   string  `json:"pick"`
	P      float64 `json:"p"`
	Source string  `json:"source"`
}

// BTTSOut is the both-teams-to-score lean.
type BTTSOut struct {
	Pick   string  `json:"pick"`
	P      float64 `json:"p"`
	Source string  `json:"source"`
}

// ScorePick is a likely exact score.
type ScorePick struct {
	Score string  `json:"score"`
	P     float64 `json:"p"`
}

// ConsSource is one consensus source.
type ConsSource struct {
	Key    string             `json:"key"`
	Label  string             `json:"label"`
	Probs  map[string]float64 `json:"probs"`
	Weight float64            `json:"weight"`
	AsOf   *time.Time         `json:"as_of"`
	URL    string             `json:"url"`
	Detail string             `json:"detail"`
}

// ConsMissing is a source that wasn't used and why.
type ConsMissing struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Why   string `json:"why"`
}

// Consensus is how the verdict's chances were made.
type Consensus struct {
	Probs       map[string]float64 `json:"probs"`
	Sources     []ConsSource       `json:"sources"`
	Missing     []ConsMissing      `json:"missing"`
	WeightsFrom string             `json:"weights_from"` // learned | default | jackpot
	Group       string             `json:"group"`
	MarketUsed  bool               `json:"market_used"`
}

// Verdict is the one-line pick of a match.
type Verdict struct {
	Pick      string      `json:"pick"`
	PickLabel string      `json:"pick_label"`
	P         float64     `json:"p"`
	FairOdds  float64     `json:"fair_odds"`
	Tier      TierOut     `json:"tier"`
	Safer     SaferOut    `json:"safer"`
	DrawAlert DrawAlert   `json:"draw_alert"`
	Goals     *GoalsOut   `json:"goals"`
	BTTS      *BTTSOut    `json:"btts"`
	TopScores []ScorePick `json:"top_scores"`
	ScoreNote string      `json:"score_note"`
	Consensus Consensus   `json:"consensus"`
	Kind      string      `json:"kind"` // live | frozen
	FrozenAt  *time.Time  `json:"frozen_at"`
	AsOf      time.Time   `json:"as_of"`
}

// VerdictInput feeds BuildVerdict.
type VerdictInput struct {
	Home, Away  string
	Probs       P3 // consensus 1X2
	Sources     []ConsSource
	Missing     []ConsMissing
	WeightsFrom string
	Group       string
	MarketUsed  bool
	Over        *float64 // P(over 2.5) from a market or the model
	OverSource  string
	BTTS        *float64 // P(both teams score)
	BTTSSource  string
	Own         map[string]*OwnRecord // tier → DASYIN's own record (≥ 30 graded)
	AsOf        time.Time
	Jackpot     bool // jackpot flavour: safer = pick or draw, no goal markets
}

var selKeys = [3]string{"1", "X", "2"}

func probsMap(p P3) map[string]float64 {
	return map[string]float64{"1": round4(p[0]), "X": round4(p[1]), "2": round4(p[2])}
}

func pickLabel(k int, home, away string) string {
	switch k {
	case 0:
		return home + " win"
	case 1:
		return "Draw"
	}
	return away + " win"
}

// safeLabel names a double chance.
func safeLabel(sel, home, away string) string {
	switch sel {
	case "1X":
		return home + " or draw"
	case "X2":
		return "Draw or " + away
	case "12":
		return home + " or " + away + " (no draw)"
	}
	return sel
}

// dcSel returns the double chance of two outcome indexes ("1X", "X2", "12").
func dcSel(a, b int) string {
	if a > b {
		a, b = b, a
	}
	return selKeys[a] + selKeys[b]
}

func fairOdds(p float64) float64 {
	if p <= 0 {
		return 0
	}
	return math.Round(100/p) / 100
}

// BuildVerdict turns consensus probabilities into the verdict.
func BuildVerdict(in VerdictInput) Verdict {
	p := in.Probs
	s := p[0] + p[1] + p[2]
	if s > 0 {
		for k := range p {
			p[k] /= s
		}
	}
	order := []int{0, 1, 2}
	sort.SliceStable(order, func(a, b int) bool { return p[order[a]] > p[order[b]] })
	k, k2 := order[0], order[1]
	tk := tierOf(p[k])
	ht := History.Tier(tk)
	v := Verdict{Pick: selKeys[k], PickLabel: pickLabel(k, in.Home, in.Away), P: round4(p[k]), FairOdds: fairOdds(p[k]),
		Tier: TierOut{Key: tk, Label: ht.Label, Range: ht.Range, History: VerdictHistory{HitRate: ht.FavouriteWon, N: ht.N, DCHitRate: ht.DoubleChanceWon,
			PODHitRate: ht.PickOrDrawWon, DrawRate: ht.DrawRate, Share: ht.Share, Source: historySourceLine}},
		TopScores: []ScorePick{}, ScoreNote: ScoreNote, Kind: "live", AsOf: in.AsOf,
		Consensus: Consensus{Probs: probsMap(p), Sources: in.Sources, Missing: in.Missing, WeightsFrom: in.WeightsFrom, Group: in.Group, MarketUsed: in.MarketUsed}}
	if v.Consensus.Sources == nil {
		v.Consensus.Sources = []ConsSource{}
	}
	if v.Consensus.Missing == nil {
		v.Consensus.Missing = []ConsMissing{}
	}
	if in.Own != nil {
		v.Tier.Own = in.Own[tk]
	}
	if !in.MarketUsed && !in.Jackpot {
		v.Tier.Note = "No market price — less reliable"
	}
	// Safer pick: the top two outcomes (fixtures) or pick-or-draw (jackpots).
	if in.Jackpot {
		other := 1
		if k == 1 {
			other = k2
		}
		sel := dcSel(k, other)
		v.Safer = SaferOut{Selection: sel, Label: safeLabel(sel, in.Home, in.Away), P: round4(p[k] + p[other]), FairOdds: fairOdds(p[k] + p[other]), HistoryHitRate: ht.PickOrDrawWon}
	} else {
		sel := dcSel(k, k2)
		v.Safer = SaferOut{Selection: sel, Label: safeLabel(sel, in.Home, in.Away), P: round4(p[k] + p[k2]), FairOdds: fairOdds(p[k] + p[k2]), HistoryHitRate: ht.DoubleChanceWon}
	}
	// Draw alert: P(draw) ≥ 30% and within 6 points of the top outcome.
	v.DrawAlert = DrawAlert{PDraw: round4(p[1])}
	if p[1] >= 0.30 && p[k]-p[1] <= 0.06+1e-9 {
		v.DrawAlert.On = true
		why := "The draw is close to the favourite"
		if math.Abs(p[0]-p[2]) <= 0.08 {
			why = "Teams level on chances"
		}
		over := in.Over
		if over == nil && !in.Jackpot {
			lh, la := FitLambdas(p, nil)
			o := poissonOver(lh, la, 2.5)
			over = &o
		}
		if over != nil && *over < 0.5 {
			why += " and few goals expected"
		}
		v.DrawAlert.Why = fmt.Sprintf("%s — %.0f%% draw", why, p[1]*100)
	}
	if in.Jackpot {
		return v
	}
	// Goals, both teams to score and top scores: a Poisson score grid fitted
	// to the consensus (and the over/under price when known).
	lh, la := FitLambdas(p, in.Over)
	grid := ScoreGrid(lh, la, 10)
	if in.Over != nil {
		v.Goals = goalsOut(*in.Over, in.OverSource)
	} else {
		v.Goals = goalsOut(gridOver(grid, 2.5), "estimate")
	}
	if in.BTTS != nil {
		v.BTTS = bttsOut(*in.BTTS, in.BTTSSource)
	} else {
		v.BTTS = bttsOut(gridBTTS(grid), "estimate")
	}
	for _, sc := range topScores(grid, 3) {
		v.TopScores = append(v.TopScores, sc)
	}
	return v
}

func goalsOut(over float64, src string) *GoalsOut {
	if over >= 0.5 {
		return &GoalsOut{Line: 2.5, Pick: "over", P: round4(over), Source: src}
	}
	return &GoalsOut{Line: 2.5, Pick: "under", P: round4(1 - over), Source: src}
}

func bttsOut(yes float64, src string) *BTTSOut {
	if yes >= 0.5 {
		return &BTTSOut{Pick: "yes", P: round4(yes), Source: src}
	}
	return &BTTSOut{Pick: "no", P: round4(1 - yes), Source: src}
}

// ---------------------------------------------------------------- score grid

func poissonPMF(l float64, n int) []float64 {
	out := make([]float64, n+1)
	out[0] = math.Exp(-l)
	for i := 1; i <= n; i++ {
		out[i] = out[i-1] * l / float64(i)
	}
	return out
}

// ScoreGrid is the independent-Poisson score matrix [home][away] with a
// Dixon–Coles low-score correction (rho −0.08: slightly more 0-0 and 1-1),
// renormalised.
func ScoreGrid(lh, la float64, n int) [][]float64 {
	ph, pa := poissonPMF(lh, n), poissonPMF(la, n)
	g := make([][]float64, n+1)
	tot := 0.0
	const rho = -0.08
	for i := 0; i <= n; i++ {
		g[i] = make([]float64, n+1)
		for j := 0; j <= n; j++ {
			v := ph[i] * pa[j]
			switch {
			case i == 0 && j == 0:
				v *= 1 - lh*la*rho
			case i == 0 && j == 1:
				v *= 1 + lh*rho
			case i == 1 && j == 0:
				v *= 1 + la*rho
			case i == 1 && j == 1:
				v *= 1 - rho
			}
			g[i][j] = v
			tot += v
		}
	}
	for i := range g {
		for j := range g[i] {
			g[i][j] /= tot
		}
	}
	return g
}

func grid1X2(g [][]float64) P3 {
	var p P3
	for i := range g {
		for j := range g[i] {
			switch {
			case i > j:
				p[0] += g[i][j]
			case i == j:
				p[1] += g[i][j]
			default:
				p[2] += g[i][j]
			}
		}
	}
	return p
}

func gridOver(g [][]float64, line float64) float64 {
	s := 0.0
	for i := range g {
		for j := range g[i] {
			if float64(i+j) > line {
				s += g[i][j]
			}
		}
	}
	return s
}

func gridBTTS(g [][]float64) float64 {
	s := 0.0
	for i := 1; i < len(g); i++ {
		for j := 1; j < len(g[i]); j++ {
			s += g[i][j]
		}
	}
	return s
}

func poissonOver(lh, la, line float64) float64 { return gridOver(ScoreGrid(lh, la, 10), line) }

func topScores(g [][]float64, n int) []ScorePick {
	type c struct {
		i, j int
		p    float64
	}
	all := []c{}
	for i := 0; i <= 6 && i < len(g); i++ {
		for j := 0; j <= 6 && j < len(g[i]); j++ {
			all = append(all, c{i, j, g[i][j]})
		}
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].p > all[b].p })
	out := []ScorePick{}
	for _, x := range all[:minInt(n, len(all))] {
		out = append(out, ScorePick{Score: fmt.Sprintf("%d-%d", x.i, x.j), P: round4(x.p)})
	}
	return out
}

var lambdaCache sync.Map // key → [2]float64

// FitLambdas finds expected goals (home, away) whose score grid matches
// the 1X2 chances (and P(over 2.5) when given) in least squares: a coarse
// grid search then a fine one around the best point.
func FitLambdas(p P3, over *float64) (float64, float64) {
	key := fmt.Sprintf("%.4f|%.4f|%.4f|", p[0], p[1], p[2])
	if over != nil {
		key += fmt.Sprintf("%.4f", *over)
	}
	if v, ok := lambdaCache.Load(key); ok {
		x := v.([2]float64)
		return x[0], x[1]
	}
	loss := func(lh, la float64) float64 {
		g := ScoreGrid(lh, la, 8)
		q := grid1X2(g)
		e := (q[0]-p[0])*(q[0]-p[0]) + (q[1]-p[1])*(q[1]-p[1]) + (q[2]-p[2])*(q[2]-p[2])
		if over != nil {
			d := gridOver(g, 2.5) - *over
			e += d * d
		}
		return e
	}
	bl, bh, ba := math.Inf(1), 1.4, 1.1
	for lh := 0.2; lh <= 4.01; lh += 0.1 {
		for la := 0.2; la <= 4.01; la += 0.1 {
			if e := loss(lh, la); e < bl {
				bl, bh, ba = e, lh, la
			}
		}
	}
	ch, ca := bh, ba
	for lh := ch - 0.1; lh <= ch+0.1001; lh += 0.01 {
		for la := ca - 0.1; la <= ca+0.1001; la += 0.01 {
			if lh <= 0.05 || la <= 0.05 {
				continue
			}
			if e := loss(lh, la); e < bl {
				bl, bh, ba = e, lh, la
			}
		}
	}
	bh, ba = math.Round(bh*100)/100, math.Round(ba*100)/100
	lambdaCache.Store(key, [2]float64{bh, ba})
	return bh, ba
}

// ---------------------------------------------------------------- ✓ / ✗

// Mark is one graded line of a result check.
type Mark struct {
	Selection string   `json:"selection,omitempty"`
	Label     string   `json:"label,omitempty"`
	Line      *float64 `json:"line,omitempty"`
	Pick      string   `json:"pick,omitempty"`
	TopScore  string   `json:"top_score,omitempty"`
	P         float64  `json:"p"`
	Correct   *bool    `json:"correct"`
}

// ResultCheck is the ✓/✗ after the game.
type ResultCheck struct {
	Status       string     `json:"status"` // correct | wrong | no_pick | void
	Label        string     `json:"label"`
	Score        *string    `json:"score"`
	Outcome      *string    `json:"outcome"`
	Pick         *Mark      `json:"pick"`
	Safer        *Mark      `json:"safer"`
	Goals        *Mark      `json:"goals"`
	BTTS         *Mark      `json:"btts"`
	ExactScore   *Mark      `json:"exact_score"`
	Summary      string     `json:"summary"`
	GradedAt     *time.Time `json:"graded_at"`
	ResultSource *string    `json:"result_source"`
	Tier         *string    `json:"tier"`
}

func bp(b bool) *bool { return &b }

func outcomeKey(hg, ag int) string {
	switch {
	case hg > ag:
		return "1"
	case hg < ag:
		return "2"
	}
	return "X"
}

// NoPickCheck is a finished match without a frozen pick.
func NoPickCheck(score string) *ResultCheck {
	rc := &ResultCheck{Status: "no_pick", Label: "No pick", Summary: "No pick was frozen before kickoff."}
	if score != "" {
		s := score
		rc.Score = &s
		var h, a int
		if _, err := fmt.Sscanf(score, "%d-%d", &h, &a); err == nil {
			o := outcomeKey(h, a)
			rc.Outcome = &o
		}
	}
	return rc
}

// VoidCheck is an abandoned / cancelled / postponed match.
func VoidCheck(v *Verdict, why string, now time.Time, source string) *ResultCheck {
	rc := &ResultCheck{Status: "void", Label: "Void", Summary: "Void — " + why + ", so the pick isn't counted."}
	if v != nil {
		rc.Pick = &Mark{Selection: v.Pick, Label: v.PickLabel, P: v.P}
		t := v.Tier.Key
		rc.Tier = &t
		rc.GradedAt = &now
		if source != "" {
			rc.ResultSource = &source
		}
	}
	return rc
}

// GradeVerdict grades a frozen verdict against the 90-minute score.
func GradeVerdict(v Verdict, hg, ag int, home, away string, now time.Time, source string) *ResultCheck {
	o := outcomeKey(hg, ag)
	score := fmt.Sprintf("%d-%d", hg, ag)
	tk := v.Tier.Key
	rc := &ResultCheck{Score: &score, Outcome: &o, GradedAt: &now, Tier: &tk}
	if source != "" {
		rc.ResultSource = &source
	}
	ok := v.Pick == o
	rc.Pick = &Mark{Selection: v.Pick, Label: v.PickLabel, P: v.P, Correct: bp(ok)}
	if v.Safer.Selection != "" {
		rc.Safer = &Mark{Selection: v.Safer.Selection, Label: v.Safer.Label, P: v.Safer.P, Correct: bp(strings.Contains(v.Safer.Selection, o))}
	}
	if v.Goals != nil {
		over := float64(hg+ag) > v.Goals.Line
		l := v.Goals.Line
		rc.Goals = &Mark{Line: &l, Pick: v.Goals.Pick, P: v.Goals.P, Correct: bp((v.Goals.Pick == "over") == over)}
	}
	if v.BTTS != nil {
		yes := hg > 0 && ag > 0
		rc.BTTS = &Mark{Pick: v.BTTS.Pick, P: v.BTTS.P, Correct: bp((v.BTTS.Pick == "yes") == yes)}
	}
	if len(v.TopScores) > 0 {
		rc.ExactScore = &Mark{TopScore: v.TopScores[0].Score, P: v.TopScores[0].P, Correct: bp(v.TopScores[0].Score == score)}
	}
	what := ""
	switch o {
	case "1":
		what = fmt.Sprintf("%s won %s", home, score)
	case "2":
		what = fmt.Sprintf("%s won %s", away, score)
	default:
		what = fmt.Sprintf("it was a draw, %s", score)
	}
	pct := fmt.Sprintf("%.0f%%", v.P*100)
	if ok {
		rc.Status, rc.Label = "correct", "Correct"
		rc.Summary = fmt.Sprintf("Correct — %s (the pick was %s, %s).", what, v.PickLabel, pct)
		if rc.ExactScore != nil && *rc.ExactScore.Correct {
			rc.Summary += fmt.Sprintf(" The exact score %s was the top score too.", score)
		}
	} else {
		rc.Status, rc.Label = "wrong", "Wrong"
		rc.Summary = fmt.Sprintf("Wrong — %s (the pick was %s, %s).", what, v.PickLabel, pct)
		if rc.Safer != nil {
			if *rc.Safer.Correct {
				rc.Summary += fmt.Sprintf(" The safer pick (%s) was correct.", v.Safer.Label)
			} else {
				rc.Summary += fmt.Sprintf(" The safer pick (%s) was wrong too.", v.Safer.Label)
			}
		}
	}
	return rc
}

// ---------------------------------------------------------------- Poisson-binomial range

// PBRange returns the expected count and the 10th / 90th percentiles of
// the number of successes of independent trials (Poisson-binomial).
func PBRange(ps []float64) (exp float64, lo, hi int) {
	if len(ps) == 0 {
		return 0, 0, 0
	}
	d := PoissonBinomial(ps)
	for _, p := range ps {
		exp += p
	}
	cum := 0.0
	lo, hi = -1, -1
	for k, v := range d {
		cum += v
		if lo < 0 && cum >= 0.10-1e-12 {
			lo = k
		}
		if hi < 0 && cum >= 0.90-1e-12 {
			hi = k
		}
	}
	if hi < 0 {
		hi = len(ps)
	}
	return exp, lo, hi
}

// RangeVerdict classifies an observed count against its Poisson-binomial
// range and explains it in plain English.
func RangeVerdict(correct, graded int, exp float64, lo, hi int, what string) (string, string) {
	if graded == 0 {
		return "none", "No graded " + what + " yet."
	}
	head := fmt.Sprintf("%d of %d right.", correct, graded)
	if graded < 5 {
		return "too_few", head + fmt.Sprintf(" Only %d graded — too few to judge.", graded)
	}
	said := fmt.Sprintf(" The %s' own chances said about %.0f (normal range %d–%d)", what, exp, lo, hi)
	switch {
	case correct < lo:
		return "below_range", head + said + ", so this was below the normal range — worth a look."
	case correct > hi:
		return "above_range", head + said + ", so this was above the normal range — a lucky run; don't expect it to last."
	case float64(correct) < exp-0.5:
		return "bad_luck", head + said + ", so this was bad luck rather than a broken model."
	case float64(correct) > exp+0.5:
		return "good_luck", head + said + ", so this was a little lucky."
	}
	return "as_expected", head + said + " — about what the chances said."
}
