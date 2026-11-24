package football

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Pure tests of v2.18: verdict tiers and edges, ✓/✗ grading, the
// Poisson-binomial range, the consensus blend and learned weights (incl.
// the holdout fallback), the budget planner, the jackpot review of 20 Nov,
// the new providers' parsers on captured fake payloads and the planners.

func wnear(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func TestVerdictTiersAndEdges(t *testing.T) {
	cases := map[float64]string{0.60: "strong", 0.5999: "good", 0.50: "good", 0.4999: "close", 0.42: "close", 0.4199: "coinflip", 0.34: "coinflip", 0.9: "strong"}
	for p, want := range cases {
		if got := tierOf(p); got != want {
			t.Errorf("tierOf(%v) = %s, want %s", p, got, want)
		}
	}
	// Strong home favourite: safer = 1X, history numbers from the open table.
	v := BuildVerdict(VerdictInput{Home: "Arsenal", Away: "Chelsea", Probs: P3{0.64, 0.22, 0.14}, MarketUsed: true})
	if v.Pick != "1" || v.PickLabel != "Arsenal win" || v.Tier.Key != "strong" || v.Tier.Label != "Strong" || v.Tier.History.HitRate != 0.7183 ||
		v.Tier.History.N != 12719 || v.Safer.Selection != "1X" || v.Safer.Label != "Arsenal or draw" || !wnear(v.Safer.P, 0.86, 1e-9) ||
		v.Safer.HistoryHitRate != 0.8959 || v.DrawAlert.On || v.FairOdds != 1.56 || v.Tier.Note != "" || v.ScoreNote != ScoreNote {
		t.Fatalf("strong verdict %+v", v)
	}
	if len(v.TopScores) != 3 || v.Goals == nil || v.BTTS == nil || v.Goals.Source != "estimate" {
		t.Fatalf("goal lines %+v %+v %+v", v.TopScores, v.Goals, v.BTTS)
	}
	// Away favourite with home second: safer = 12 ("no draw").
	v = BuildVerdict(VerdictInput{Home: "Fulham", Away: "Chelsea", Probs: P3{0.33, 0.25, 0.42}, MarketUsed: true})
	if v.Pick != "2" || v.Tier.Key != "close" || v.Safer.Selection != "12" || v.Safer.Label != "Fulham or Chelsea (no draw)" {
		t.Fatalf("away verdict %+v", v.Safer)
	}
	// Draw alert: P(draw) ≥ 0.30 and within 0.06 of the top (edges).
	on := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{0.33, 0.31, 0.36}, MarketUsed: true})
	if !on.DrawAlert.On || !strings.Contains(on.DrawAlert.Why, "Teams level on chances") || !strings.HasSuffix(on.DrawAlert.Why, "31% draw") {
		t.Fatalf("draw alert %+v", on.DrawAlert)
	}
	if v := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{0.3501, 0.2999, 0.35}, MarketUsed: true}); v.DrawAlert.On {
		t.Fatal("draw alert below 30%")
	}
	if v := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{0.39, 0.30, 0.31}, MarketUsed: true}); v.DrawAlert.On {
		t.Fatal("draw alert with a gap above 6 points")
	}
	if v := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{0.36, 0.30, 0.34}, MarketUsed: true}); !v.DrawAlert.On {
		t.Fatal("draw alert at exactly 6 points")
	}
	over := 0.40
	if v := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{0.33, 0.32, 0.35}, Over: &over, OverSource: "market", MarketUsed: true}); !strings.Contains(v.DrawAlert.Why, "few goals expected") ||
		v.Goals.Pick != "under" || v.Goals.P != 0.6 || v.Goals.Source != "market" {
		t.Fatalf("few goals %+v %+v", v.DrawAlert, v.Goals)
	}
	// Draw as the top outcome: pick X, safer draw + the next outcome.
	d := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{0.30, 0.38, 0.32}, MarketUsed: true})
	if d.Pick != "X" || d.PickLabel != "Draw" || d.Safer.Selection != "X2" || !d.DrawAlert.On {
		t.Fatalf("draw pick %+v", d)
	}
	// No market: the tier says so; unnormalised input is normalised.
	nm := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{2, 1, 1}})
	if nm.Tier.Note != "No market price — less reliable" || nm.P != 0.5 || nm.Tier.Key != "good" {
		t.Fatalf("no market %+v", nm)
	}
	// Jackpot flavour: safer = pick or draw, no goal lines.
	j := BuildVerdict(VerdictInput{Home: "Piast", Away: "Korona", Probs: P3{0.3724, 0.2712, 0.3563}, Jackpot: true, MarketUsed: true})
	if j.Safer.Selection != "1X" || j.Safer.HistoryHitRate != History.Tier("coinflip").PickOrDrawWon || j.Goals != nil || j.BTTS != nil || len(j.TopScores) != 0 || j.Tier.Note != "" {
		t.Fatalf("jackpot verdict %+v", j)
	}
	j2 := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{0.30, 0.38, 0.32}, Jackpot: true})
	if j2.Safer.Selection != "X2" {
		t.Fatalf("jackpot draw pick safer %s", j2.Safer.Selection)
	}
	// Honest wording: no forbidden words anywhere in a verdict.
	b, _ := json.Marshal([]Verdict{v, on, d, nm, j})
	for _, w := range []string{"sure", "banker", "guarantee", "fixed"} {
		if strings.Contains(strings.ToLower(string(b)), w) {
			t.Fatalf("verdict text contains %q", w)
		}
	}
}

func TestScoreGridFit(t *testing.T) {
	for _, p := range []P3{{0.64, 0.22, 0.14}, {0.37, 0.27, 0.36}, {0.2, 0.25, 0.55}} {
		lh, la := FitLambdas(p, nil)
		q := grid1X2(ScoreGrid(lh, la, 10))
		for k := 0; k < 3; k++ {
			if !wnear(q[k], p[k], 0.025) {
				t.Fatalf("fit %v → %v (λ %.2f %.2f)", p, q, lh, la)
			}
		}
	}
	over := 0.62
	lh, la := FitLambdas(P3{0.45, 0.27, 0.28}, &over)
	if g := gridOver(ScoreGrid(lh, la, 10), 2.5); !wnear(g, over, 0.04) {
		t.Fatalf("over fit %.3f", g)
	}
	ts := topScores(ScoreGrid(1.3, 1.1, 10), 3)
	if len(ts) != 3 || ts[0].P < ts[1].P || ts[1].P < ts[2].P || ts[0].Score != "1-1" {
		t.Fatalf("top scores %+v", ts)
	}
	s := 0.0
	for _, row := range ScoreGrid(1.6, 0.9, 10) {
		for _, x := range row {
			s += x
		}
	}
	if !wnear(s, 1, 1e-9) {
		t.Fatalf("grid sums to %v", s)
	}
}

func TestGradeVerdict(t *testing.T) {
	now := time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)
	v := BuildVerdict(VerdictInput{Home: "Arsenal", Away: "Chelsea", Probs: P3{0.64, 0.22, 0.14}, MarketUsed: true})
	rc := GradeVerdict(v, 1, 1, "Arsenal", "Chelsea", now, ProviderAPIFootball)
	if rc.Status != "wrong" || rc.Label != "Wrong" || *rc.Score != "1-1" || *rc.Outcome != "X" || *rc.Pick.Correct || !*rc.Safer.Correct ||
		rc.Summary != "Wrong — it was a draw, 1-1 (the pick was Arsenal win, 64%). The safer pick (Arsenal or draw) was correct." || *rc.Tier != "strong" {
		t.Fatalf("draw check %+v %s", rc, rc.Summary)
	}
	rc = GradeVerdict(v, 0, 2, "Arsenal", "Chelsea", now, ProviderOpenFootball)
	if rc.Status != "wrong" || *rc.Safer.Correct || !strings.HasSuffix(rc.Summary, "was wrong too.") || *rc.ResultSource != ProviderOpenFootball || !strings.Contains(rc.Summary, "Chelsea won 0-2") {
		t.Fatalf("away win %+v", rc.Summary)
	}
	top := v.TopScores[0].Score
	var hg, ag int
	fmt.Sscanf(top, "%d-%d", &hg, &ag)
	rc = GradeVerdict(v, hg, ag, "Arsenal", "Chelsea", now, "")
	if hg > ag && (rc.Status != "correct" || !*rc.ExactScore.Correct || !strings.Contains(rc.Summary, "top score too")) {
		t.Fatalf("exact score %+v", rc)
	}
	// Goals and both teams to score marks.
	over := 0.6
	btts := 0.3
	v2 := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{0.5, 0.25, 0.25}, Over: &over, BTTS: &btts, MarketUsed: true})
	rc = GradeVerdict(v2, 3, 0, "A", "B", now, "")
	if !*rc.Goals.Correct || rc.Goals.Pick != "over" || !*rc.BTTS.Correct || rc.BTTS.Pick != "no" || rc.Status != "correct" {
		t.Fatalf("goal marks %+v %+v", rc.Goals, rc.BTTS)
	}
	np := NoPickCheck("0-1")
	if np.Status != "no_pick" || np.Label != "No pick" || *np.Outcome != "2" || np.Pick != nil || np.Summary != "No pick — not locked before kick-off." { // v2.19.1 wording
		t.Fatalf("no pick %+v", np)
	}
	vd := VoidCheck(&v, voidWhy("ABD"), now, ProviderAPIFootball)
	if vd.Status != "void" || vd.Label != "Void" || vd.Pick == nil || vd.Pick.Correct != nil || vd.Summary != "Void — the match was abandoned, so the pick isn't counted." {
		t.Fatalf("void %+v", vd)
	}
}

func TestPBRangeAndVerdict(t *testing.T) {
	e, lo, hi := PBRange([]float64{0.5, 0.5, 0.5, 0.5})
	if e != 2 || lo != 1 || hi != 3 {
		t.Fatalf("binomial(4, .5): %v %d %d", e, lo, hi)
	}
	// The 20 Nov favourites (SportPesa odds, proportional de-margin).
	ps := []float64{}
	for _, g := range winJP20Nov {
		p, _ := DeMarginProportional(g.Odds)
		ps = append(ps, math.Max(p[0], math.Max(p[1], p[2])))
	}
	e, lo, hi = PBRange(ps)
	if fmt.Sprintf("%.2f", e) != "5.16" || lo != 3 || hi != 7 {
		t.Fatalf("jackpot range %v %d %d", e, lo, hi)
	}
	d := PoissonBinomial(ps)
	if fmt.Sprintf("%.2f", d[0]+d[1]+d[2]+d[3]+d[4]) != "0.36" {
		t.Fatal("P(≤4)")
	}
	cases := []struct {
		c, n int
		e    float64
		lo   int
		hi   int
		want string
	}{{4, 13, 5.16, 3, 7, "bad_luck"}, {2, 13, 5.16, 3, 7, "below_range"}, {9, 13, 5.16, 3, 7, "above_range"}, {5, 13, 5.16, 3, 7, "as_expected"},
		{6, 13, 5.16, 3, 7, "good_luck"}, {2, 3, 1.5, 0, 3, "too_few"}, {0, 0, 0, 0, 0, "none"}}
	for _, c := range cases {
		if k, _ := RangeVerdict(c.c, c.n, c.e, c.lo, c.hi, "picks"); k != c.want {
			t.Errorf("RangeVerdict(%d/%d) = %s, want %s", c.c, c.n, k, c.want)
		}
	}
	_, txt := RangeVerdict(4, 13, 5.16, 3, 7, "picks")
	if txt != "4 of 13 right. The picks' own chances said about 5 (normal range 3–7), so this was bad luck rather than a broken model." {
		t.Fatalf("text %q", txt)
	}
	_, txt = RangeVerdict(2, 13, 5.16, 3, 7, "picks")
	if !strings.HasSuffix(txt, "below the normal range — worth a look.") {
		t.Fatalf("below %q", txt)
	}
}

func TestBlendConsensus(t *testing.T) {
	sharp, market, model := P3{0.6, 0.25, 0.15}, P3{0.58, 0.26, 0.16}, P3{0.5, 0.3, 0.2}
	p, eff := BlendConsensus(map[string]*P3{"sharp": &sharp, "market": &market, "model": &model}, DefaultConsWeights)
	if !wnear(eff["sharp"], 0.55/0.83, 1e-9) || !wnear(eff["market"], 0.20/0.83, 1e-9) || !wnear(eff["model"], 0.08/0.83, 1e-9) {
		t.Fatalf("eff %v", eff)
	}
	if !wnear(p[0]+p[1]+p[2], 1, 1e-12) {
		t.Fatal("sum")
	}
	// No sharp price: the market takes its weight.
	_, eff = BlendConsensus(map[string]*P3{"market": &market, "model": &model}, DefaultConsWeights)
	if !wnear(eff["market"], 0.75/0.83, 1e-9) || !wnear(eff["model"], 0.08/0.83, 1e-9) {
		t.Fatalf("market takes the sharp weight %v", eff)
	}
	// No market at all: the model alone carries it.
	p, eff = BlendConsensus(map[string]*P3{"model": &model}, DefaultConsWeights)
	if eff["model"] != 1 || p != model {
		t.Fatalf("model only %v %v", p, eff)
	}
	if _, eff := BlendConsensus(map[string]*P3{}, DefaultConsWeights); len(eff) != 0 {
		t.Fatal("nothing known")
	}
}

func TestMarketSources(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	pr := func(book, src string, x float64) PriceOut {
		return PriceOut{Bookmaker: book, Title: book, Source: src, Price: x, FetchedAt: now}
	}
	line := 2.5
	mv := MarketView{Selections: []SelectionOut{
		{Market: Mkt1X2, Selection: SelHome, Prices: []PriceOut{pr("pinnacle", ProviderOddsAPI, 1.50), pr("bet365", ProviderOddsAPI, 1.45), pr("sportpesa", "manual: SportPesa", 1.9)}},
		{Market: Mkt1X2, Selection: SelDraw, Prices: []PriceOut{pr("pinnacle", ProviderOddsAPI, 4.40), pr("bet365", ProviderOddsAPI, 4.2), pr("sportpesa", "manual: SportPesa", 3)}},
		{Market: Mkt1X2, Selection: SelAway, Prices: []PriceOut{pr("pinnacle", ProviderOddsAPI, 7.30), pr("bet365", ProviderOddsAPI, 6.5), pr("sportpesa", "manual: SportPesa", 4)}},
		{Market: MktOU, Line: &line, Selection: SelOver, FairP: ptr(0.55)},
	}}
	in := newConsInputs()
	marketSources(mv, &in)
	if in.P["sharp"] == nil || in.P["market"] == nil || !strings.Contains(in.Label["sharp"], "Pinnacle") || !strings.Contains(in.Label["market"], "median of 2") {
		t.Fatalf("sources %v %v", in.Label, in.Why)
	}
	s := in.P["sharp"]
	if !wnear(s[0]+s[1]+s[2], 1, 1e-9) || s[0] < 0.6 || s[0] > 0.68 {
		t.Fatalf("shin %v", *s)
	}
	if in.Over == nil || *in.Over != 0.55 || in.OverSrc != "market" || in.Fav != in.P["sharp"] {
		t.Fatal("over / favourite source")
	}
	in2 := newConsInputs()
	marketSources(MarketView{}, &in2)
	if in2.P["sharp"] != nil || in2.P["market"] != nil || in2.Why["market"] == "" || in2.Why["sharp"] == "" {
		t.Fatal("no prices")
	}
}

// synthetic graded samples: "sharp" knows the truth, "model" is noise.
func consSamples(n int, flip int) []ConsSample {
	out := []ConsSample{}
	r := uint64(7)
	rnd := func() float64 {
		r = r*6364136223846793005 + 1442695040888963407
		return float64(r>>11) / float64(1<<53)
	}
	for i := 0; i < n; i++ {
		ph := 0.2 + 0.6*rnd()
		truth := P3{ph, (1 - ph) * 0.45, (1 - ph) * 0.55}
		u := rnd()
		o := 0
		if u > truth[0] {
			o = 1
			if u > truth[0]+truth[1] {
				o = 2
			}
		}
		noise := P3{0.6 - 0.3*rnd(), 0.2, 0}
		noise[2] = 1 - noise[0] - noise[1]
		smp := ConsSample{Probs: map[string]P3{"sharp": truth, "model": noise}, Outcome: o, Kickoff: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)}
		if flip > 0 && i >= flip {
			smp.Probs = map[string]P3{"sharp": noise, "model": truth}
		}
		out = append(out, smp)
	}
	return out
}

func TestFitConsensus(t *testing.T) {
	fit := FitConsensus(consSamples(150, 0), DefaultConsWeights)
	if fit.InUse != "default" || !strings.Contains(fit.Note, "only 150 graded matches") || fit.Weights["sharp"] != 0.55 {
		t.Fatalf("too few %+v", fit)
	}
	fit = FitConsensus(consSamples(1200, 0), DefaultConsWeights)
	if fit.InUse != "learned" || fit.HoldoutN != 240 || fit.N != 1200 || *fit.LoglossLearned >= *fit.LoglossDefault {
		t.Fatalf("learned %+v", fit)
	}
	if fit.Weights["sharp"] <= 0.55/(0.55+0.08)*(0.55+0.08) || fit.Weights["model"] >= 0.08 {
		t.Fatalf("weights should favour the truthful source: %v", fit.Weights)
	}
	sum := 0.0
	for _, k := range ConsKeys {
		if fit.Weights[k] < 0 {
			t.Fatal("negative weight")
		}
		sum += fit.Weights[k]
	}
	if !wnear(sum, 1, 0.002) {
		t.Fatalf("weights sum %v", sum)
	}
	// Holdout fallback: the newest 20 % behaves differently → defaults kept.
	fit = FitConsensus(consSamples(1000, 800), DefaultConsWeights)
	if fit.InUse != "default" || !strings.Contains(fit.Note, "did not beat") || fit.LoglossLearned == nil {
		t.Fatalf("holdout fallback %+v", fit)
	}
	// One source only → defaults.
	one := consSamples(400, 0)
	for i := range one {
		delete(one[i].Probs, "model")
	}
	if fit := FitConsensus(one, DefaultConsWeights); fit.InUse != "default" || !strings.Contains(fit.Note, "fewer than two sources") {
		t.Fatalf("one source %+v", fit)
	}
}

func TestPlanCover(t *testing.T) {
	ps := []P3{}
	for _, g := range winJP20Nov {
		p, _ := DeMarginProportional(g.Odds)
		ps = append(ps, p)
	}
	prev := math.Inf(-1)
	for c := 1; c <= 400; c++ {
		ms, _ := PlanCover(ps, c)
		lp, n := planValue(ps, ms)
		if n > c {
			t.Fatalf("cap %d: %d combinations", c, n)
		}
		if lp < prev-1e-12 {
			t.Fatalf("not monotone at %d: %.6f < %.6f", c, lp, prev)
		}
		prev = lp
		if c == 1 && strings.Join(func() []string {
			out := []string{}
			for _, m := range ms {
				out = append(out, maskPick(m))
			}
			return out
		}(), " ") != strings.Join(JP20NovTicket, " ") {
			t.Fatal("one combination = the most likely ticket")
		}
	}
	// At least as good as the greedy cover order within the same cap.
	steps, _ := CoverPriority(ps, 26)
	for _, st := range steps {
		ms, _ := PlanCover(ps, st.Combinations)
		lp, _ := planValue(ps, ms)
		if lp < math.Log(st.PAll)-1e-9 {
			t.Fatalf("worse than greedy at %d combinations", st.Combinations)
		}
	}
	if lv := combosLevels(3, 30); fmt.Sprint(lv) != "[1 2 3 4 6 8 9 12 18 27]" {
		t.Fatalf("levels %v", lv)
	}
}

func TestJackpotReview20Nov(t *testing.T) {
	gs := []JackpotGame{}
	fr := &JackpotFrozen{At: time.Date(2026, 11, 20, 16, 40, 0, 0, time.UTC), Picks: JP20NovTicket}
	for i, g := range winJP20Nov {
		p, _ := DeMarginProportional(g.Odds)
		fr.Probs = append(fr.Probs, probsMap(p))
		res := outcomeKey(g.HG, g.AG)
		gs = append(gs, JackpotGame{N: i + 1, Home: "H", Away: "A", Odds1: g.Odds[0], OddsX: g.Odds[1], Odds2: g.Odds[2], Result: res, Score: fmt.Sprintf("%d-%d", g.HG, g.AG)})
	}
	fr.Favourites = jackpotFavourites(gs)
	h := make([]string, 13)
	r, checks := jackpotReview(Jackpot{Bookmaker: "SportPesa"}, gs, fr, h, h)
	if r == nil || r.Correct != 4 || r.Of != 13 || r.SaferHits != 8 || r.DrawsActual != 4 || r.FavouritesCorrect != 4 || r.FavouritesSameAsPick != 13 ||
		fmt.Sprintf("%.2f", r.Expected) != "5.16" || r.Low != 3 || r.High != 7 || fmt.Sprintf("%.2f", r.PAtMostCorrect) != "0.36" || r.Verdict != "bad_luck" {
		t.Fatalf("review %+v", r)
	}
	want := []string{"Our most likely ticket got 4/13. It was identical to SportPesa's own favourites, which also got 4/13.",
		"Each pick was only a 37–42% chance; the best single ticket expected 5.2 right, and 4 or fewer happens 36% of the time.",
		"Four games were draws (3.4 expected); a single most-likely ticket never picks a draw.",
		"\"Pick or draw\" on every game would have hit 8/13."}
	if strings.Join(r.Text, "|") != strings.Join(want, "|") {
		t.Fatalf("text\n%q\nwant\n%q", r.Text, want)
	}
	if checks[2].Status != "wrong" || !*checks[2].Safer.Correct || checks[0].Status != "correct" {
		t.Fatalf("checks %+v %+v", checks[0], checks[2])
	}
	// Partial: only one result so far.
	for i := 1; i < len(gs); i++ {
		gs[i].Result = ""
	}
	gs[5].Void = true
	r, checks = jackpotReview(Jackpot{}, gs, fr, h, h)
	if r == nil || r.Complete || r.Of != 1 || checks[5].Status != "void" || checks[3].Status != "pending" || !strings.HasSuffix(r.Headline, "(so far)") {
		t.Fatalf("partial %+v", r)
	}
	// Nothing graded → no review.
	gs[0].Result, gs[5].Void = "", false
	if r, _ := jackpotReview(Jackpot{}, gs, fr, h, h); r != nil {
		t.Fatal("review without results")
	}
}

func readWin(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/win/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParsePolymarket(t *testing.T) {
	var evs []pmEvent
	if err := json.Unmarshal(readWin(t, "pm_events.json"), &evs); err != nil {
		t.Fatal(err)
	}
	got := map[string]PMParsed{}
	for _, e := range evs {
		got[string(e.ID)] = ParsePMEvent(e, 5000, 0.04)
	}
	ok := got["800141"] // 1200141: id % 3 == 0 → Polymarket spellings
	if !ok.Usable || ok.Home != "Eastmoor Albion FC" || ok.Away != "Oakvale Utd FC" || !ok.HasKickoff || ok.Kickoff.Format(time.RFC3339) != "2026-11-21T15:00:00Z" ||
		len(ok.Markets) != 3 || !wnear(ok.Probs[0]+ok.Probs[1]+ok.Probs[2], 1, 1e-9) {
		t.Fatalf("usable event %+v", ok)
	}
	for _, m := range ok.Markets {
		if !m.Used || m.Mid == nil || *m.Spread > 0.04 || *m.Liquidity < 5000 {
			t.Fatalf("market %+v", m)
		}
	}
	thin := got["800145"] // 1200145 % 5 == 0: thin draw market
	if thin.Usable || !strings.HasPrefix(thin.Why, "thin market ignored: liquidity under $5,000") {
		t.Fatalf("thin %+v", thin.Why)
	}
	wide := got["800143"] // 1200143 % 7 == 0: wide home spread
	if wide.Usable || !strings.Contains(wide.Why, "spread 0.080 above 0.040") {
		t.Fatalf("wide %+v", wide.Why)
	}
	if ht := got["900141"]; ht.Usable || ht.Home != "" || ht.Why != "not a match-result event" {
		t.Fatalf("halftime companion %+v", ht)
	}
	// Edge shapes.
	var edge []pmEvent
	if err := json.Unmarshal(readWin(t, "pm_events_edge.json"), &edge); err != nil {
		t.Fatal(err)
	}
	three := ParsePMEvent(edge[0], 5000, 0.04)
	if !three.Usable || string(edge[0].ID) != "700001" || !wnear(three.Probs[0], 0.52/1.02, 1e-9) {
		t.Fatalf("three-way market %+v", three)
	}
	nobook := ParsePMEvent(edge[1], 5000, 0.04)
	if nobook.Usable || !strings.Contains(nobook.Why, "no bid/ask spread") || !nobook.HasKickoff || len(nobook.Markets) != 3 || nobook.Markets[0].Mid == nil {
		t.Fatalf("no order book %+v", nobook)
	}
	if sp := ParsePMEvent(edge[2], 5000, 0.04); sp.Usable || len(sp.Markets) != 0 {
		t.Fatalf("spreads-only market used %+v", sp)
	}
	if cl := ParsePMEvent(edge[3], 5000, 0.04); cl.Usable {
		t.Fatal("closed market used")
	}
	// Looser limits make the thin event usable.
	if loose := ParsePMEvent(evs[4], 1000, 0.04); !loose.Usable {
		t.Fatalf("loose limits %+v", loose.Why)
	}
	var sports []PMSport
	json.Unmarshal(readWin(t, "pm_sports.json"), &sports)
	if tag := pmTagFor(sports, "epl"); tag != "306" {
		t.Fatalf("epl tag %q", tag)
	}
	if tag := pmTagFor(sports, "nba"); tag != "745" {
		t.Fatalf("nba tag %q", tag)
	}
	if pmTagFor(sports, "xyz") != "" {
		t.Fatal("unknown sport")
	}
	if m := pmLeagues("epl:39, LAL:140,bad,x:0"); len(m) != 2 || m["lal"] != 140 {
		t.Fatalf("leagues %v", m)
	}
}

func TestParseOpenFootball(t *testing.T) {
	name, rows, err := ParseOFFile(readWin(t, "of_edge.json"), "Europe/London")
	if err != nil || name != "Test League 2026/27" || len(rows) != 5 {
		t.Fatalf("%s %d %v", name, len(rows), err)
	}
	// 15:00 London in August (BST, UTC+1) = 14:00 UTC.
	if rows[0].Kickoff == nil || rows[0].Kickoff.Format(time.RFC3339) != "2026-08-15T14:00:00Z" || *rows[0].HG != 2 || *rows[0].AG != 1 {
		t.Fatalf("row 0 %+v", rows[0])
	}
	if rows[1].Kickoff.Format(time.RFC3339) != "2026-08-16T19:00:00Z" || *rows[1].HG != 0 || !rows[1].HasTimeZone {
		t.Fatalf("explicit UTC+0 + bare score %+v", rows[1])
	}
	if rows[2].Kickoff.Format(time.RFC3339) != "2026-08-16T07:30:00Z" || rows[2].HG != nil {
		t.Fatalf("UTC+2 without score %+v", rows[2])
	}
	if rows[3].Kickoff.Format(time.RFC3339) != "2026-11-21T15:00:00Z" {
		t.Fatalf("November = GMT %+v", rows[3].Kickoff)
	}
	if rows[4].Kickoff != nil || *rows[4].HG != 3 || *rows[4].AG != 3 {
		t.Fatalf("no time, ft from the object %+v", rows[4])
	}
	_, rows, err = ParseOFFile(readWin(t, "of_en1.json"), "Europe/London")
	if err != nil || len(rows) != 380 {
		t.Fatalf("en.1 %d %v", len(rows), err)
	}
	if _, _, err := ParseOFFile([]byte("404: Not Found"), "Europe/London"); err == nil {
		t.Fatal("not json accepted")
	}
	if OFSeason(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)) != "2026-27" || OFSeason(time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)) != "2026-27" ||
		OFSeason(time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC)) != "2027-28" {
		t.Fatal("season folder")
	}
	if c := ofCodes("en.1, de.2,xx.9"); len(c) != 2 {
		t.Fatalf("codes %v", c)
	}
}

func TestParseAFPrediction(t *testing.T) {
	for _, f := range []string{"testdata/af_predictions.json", "testdata/win/af_predictions_1200141.json"} {
		b, _ := os.ReadFile(f)
		var env struct {
			Response []afPredEntry `json:"response"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatal(err)
		}
		p, d, ok := ParseAFPrediction(env.Response)
		if !ok || !wnear(p[0]+p[1]+p[2], 1, 1e-9) || d["advice"] == "" || d["winner"] == "" || d["percent"] == nil {
			t.Fatalf("%s: %v %v", f, p, d)
		}
	}
	b, _ := os.ReadFile("testdata/af_predictions.json")
	var env struct {
		Response []afPredEntry `json:"response"`
	}
	json.Unmarshal(b, &env)
	p, d, _ := ParseAFPrediction(env.Response)
	if p != (P3{0.4, 0.3, 0.3}) || d["advice"] != "Double chance : Eastmoor Albion or draw" || d["win_or_draw"] != true {
		t.Fatalf("doc sample %v %v", p, d)
	}
	if _, _, ok := ParseAFPrediction(nil); ok {
		t.Fatal("empty")
	}
	var zero afPredEntry
	if _, _, ok := ParseAFPrediction([]afPredEntry{zero}); ok {
		t.Fatal("no percentages")
	}
}

func TestPlanWin(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	fetched := now.Add(-20 * time.Hour)
	recent := now.Add(-time.Hour)
	in := WinPlanInput{Now: now, On: map[string]bool{ProviderAFPred: true, ProviderPolymarket: true, ProviderOpenFootball: true}, State: map[string]TaskState{},
		AP: []apTarget{
			{FixtureID: 1, Kickoff: now.Add(5 * time.Hour)},                        // new → ask
			{FixtureID: 2, Kickoff: now.Add(60 * time.Hour)},                       // beyond 48 h
			{FixtureID: 3, Kickoff: now.Add(10 * time.Minute)},                     // too close
			{FixtureID: 4, Kickoff: now.Add(2 * time.Hour), FetchedAt: &fetched},   // refresh at 3 h
			{FixtureID: 5, Kickoff: now.Add(2 * time.Hour), FetchedAt: &recent},    // fetched inside the 3 h window: done
			{FixtureID: 6, Kickoff: now.Add(30 * time.Hour), HasRef: true},         // reuse the stored reference
			{FixtureID: 7, Kickoff: now.Add(30 * time.Hour), FetchedAt: &fetched}}, // once is enough until 3 h before
		PMLeague: map[string]int64{"epl": 39}, OFFiles: []string{"en.1", "de.1"}, Season: "2026-27"}
	ts := PlanWin(in)
	keys := []string{}
	for _, t := range ts {
		keys = append(keys, t.Key)
	}
	want := "pm:sports of:de.1:2026-27 of:en.1:2026-27 pm:events:epl ap:pred3:4 ap:pred:1 ap:import:6"
	if strings.Join(keys, " ") != want {
		t.Fatalf("plan\n%s\nwant\n%s", strings.Join(keys, " "), want)
	}
	if ts[0].Prio != PrioNormal || ts[len(ts)-1].Cost != 0 || ts[len(ts)-2].Prio != PrioBackfill {
		t.Fatal("priorities / free import")
	}
	// Ran recently: nothing due; the sports cache fresh: only the events.
	st := map[string]TaskState{}
	for _, k := range keys {
		st[k] = TaskState{Key: k, LastRunAt: now.Add(-10 * time.Minute), LastOKAt: ptr(now.Add(-10 * time.Minute)), Status: "ok"}
	}
	in.State, in.PMSports = st, true
	if ts := PlanWin(in); len(ts) != 1 || ts[0].Kind != TAPImport {
		t.Fatalf("all fresh: %+v", ts)
	}
	in.Now = now.Add(61 * time.Minute)
	if ts := PlanWin(in); len(ts) != 2 || ts[0].Key != "pm:events:epl" {
		t.Fatalf("hourly events: %+v", ts)
	}
	// Only some providers; switched off; no fixtures soon.
	in.Only = winOnly(map[string]bool{"openfootball": true})
	in.Now, in.State = now, map[string]TaskState{}
	if ts := PlanWin(in); len(ts) != 2 || ts[0].Provider != ProviderOpenFootball {
		t.Fatalf("only openfootball %+v", ts)
	}
	if winOnly(map[string]bool{"clubelo": true})["__none__"] != true {
		t.Fatal("only other kinds")
	}
	in.Only, in.On = nil, map[string]bool{}
	if ts := PlanWin(in); len(ts) != 0 {
		t.Fatal("switched off")
	}
	q := url.Values{}
	_ = q
}

func TestGroupsAndHistory(t *testing.T) {
	if groupOf(Competition{ID: 39}) != "top5" || groupOf(Competition{ID: 2}) != "europe" || groupOf(Competition{ID: 106, Country: "Poland"}) != "europe" ||
		groupOf(Competition{ID: 276, Country: "Kenya"}) != "world" || groupOf(Competition{ID: 999, Country: "Brazil"}) != "world" {
		t.Fatal("groups")
	}
	// The embedded table is the spec's reference table.
	n, share := 0, 0.0
	for _, tr := range History.Tiers {
		n += tr.N
		share += tr.Share
	}
	if n != History.N || !wnear(share, 1, 0.001) {
		t.Fatalf("tiers %d %.4f", n, share)
	}
	ref := map[string][3]float64{"strong": {0.166, 0.718, 0.896}, "good": {0.220, 0.561, 0.807}, "close": {0.299, 0.461, 0.752}, "coinflip": {0.314, 0.385, 0.710}}
	for k, r := range ref {
		tr := History.Tier(k)
		if !wnear(tr.Share, r[0], 0.0006) || !wnear(tr.FavouriteWon, r[1], 0.0006) || !wnear(tr.DoubleChanceWon, r[2], 0.0006) {
			t.Errorf("tier %s: %+v vs spec %v", k, tr, r)
		}
	}
	if History.Overall.DrawFavouriteMatches != 115 || !wnear(History.Overall.FavouriteWon, 0.502, 0.0006) || History.Scores[0].Score != "1-1" ||
		!wnear(History.Scores[0].Share, 0.123, 0.0006) || !wnear(History.Tier("coinflip").DrawRate, 0.30, 0.0015) {
		t.Fatal("overall / scores / coin-flip draws")
	}
	for _, g := range History.Groups {
		gn := 0
		for _, tr := range g.Tiers {
			gn += tr.N
		}
		if gn != g.N {
			t.Fatalf("group %s %d vs %d", g.Group, gn, g.N)
		}
	}
}
