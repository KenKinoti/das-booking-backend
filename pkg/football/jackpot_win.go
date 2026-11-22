package football

import (
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strings"
	"time"
)

// Jackpot upgrades (v2.18): a verdict per game (tier, draw alert, safer =
// pick or draw), the per-game chances frozen when the jackpot closes, ✓/✗
// per game against the frozen "Most likely" ticket, a plain-English review
// after grading, and a budget planner for doubles / triples.

// JackpotFrozen is the per-game snapshot stored at close.
type JackpotFrozen struct {
	At              time.Time            `json:"at"`
	Picks           []string             `json:"picks"`
	Probs           []map[string]float64 `json:"probs"`
	Favourites      []string             `json:"favourites"` // the jackpot odds' favourites ("" = no odds)
	ExpectedCorrect float64              `json:"expected_correct"`
	Source          string               `json:"source"` // "frozen at close" | "frozen after the first kickoff"
}

// JackpotCheck is a game's ✓/✗.
type JackpotCheck struct {
	Status  string  `json:"status"` // correct | wrong | void | pending
	Label   string  `json:"label"`
	Result  *string `json:"result"`
	Score   *string `json:"score"`
	Pick    *Mark   `json:"pick"`
	Safer   *Mark   `json:"safer"`
	Summary string  `json:"summary"`
}

// JackpotReview explains a graded jackpot.
type JackpotReview struct {
	Complete             bool      `json:"complete"`
	Correct              int       `json:"correct"`
	Of                   int       `json:"of"`
	Expected             float64   `json:"expected"`
	Low                  int       `json:"low"`
	High                 int       `json:"high"`
	PAtMostCorrect       float64   `json:"p_at_most_correct"`
	FavouritesCorrect    int       `json:"favourites_correct"`
	FavouritesOf         int       `json:"favourites_of"`
	FavouritesSameAsPick int       `json:"favourites_same_as_pick"`
	DrawsActual          int       `json:"draws_actual"`
	DrawsExpected        float64   `json:"draws_expected"`
	SaferHits            int       `json:"safer_hits"`
	SaferOf              int       `json:"safer_of"`
	SaferExpected        float64   `json:"safer_expected"`
	Picks                []string  `json:"picks"`
	Results              []*string `json:"results"`
	Marks                []*bool   `json:"marks"`
	Verdict              string    `json:"verdict"`
	Headline             string    `json:"headline"`
	Text                 []string  `json:"text"`
	FrozenAt             time.Time `json:"frozen_at"`
}

// jackpotVerdicts adds the per-game verdicts to an analysis.
func (s *Service) jackpotVerdicts(a *JackpotAnalysis) {
	if a == nil {
		return
	}
	for i := range a.Games {
		ga := &a.Games[i]
		srcs := []ConsSource{}
		for _, sp := range ga.Sources {
			if sp.Used && sp.Probs != nil {
				srcs = append(srcs, ConsSource{Key: sp.Key, Label: sp.Label, Probs: sp.Probs, Weight: sp.Weight, Detail: sp.Detail})
			}
		}
		missing := []ConsMissing{}
		for _, sp := range ga.Sources {
			if !sp.Used {
				missing = append(missing, ConsMissing{Key: sp.Key, Label: sp.Label, Why: sp.Why})
			}
		}
		v := BuildVerdict(VerdictInput{Home: ga.Home, Away: ga.Away, Probs: ga.p, Sources: srcs, Missing: missing, WeightsFrom: "jackpot",
			MarketUsed: true, AsOf: a.AsOf, Jackpot: true})
		ga.Verdict = &v
	}
}

// jackpotFreezeAt: a few minutes before the jackpot closes or its first
// game kicks off, whichever is first (zero when neither is known).
func jackpotFreezeAt(j Jackpot, gs []JackpotGame) time.Time {
	var t time.Time
	if j.ClosesAt != nil {
		t = *j.ClosesAt
	}
	for _, g := range gs {
		if g.Kickoff != nil && (t.IsZero() || g.Kickoff.Before(t)) {
			t = *g.Kickoff
		}
	}
	if t.IsZero() {
		return t
	}
	return t.Add(-5 * time.Minute)
}

func jackpotFavourites(gs []JackpotGame) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		if hasOdds(g) {
			p, _ := DeMarginProportional([3]float64{g.Odds1, g.OddsX, g.Odds2})
			out[i] = selKeys[MostLikely([]P3{p})[0]]
		}
	}
	return out
}

// freezeJackpot stores the snapshot (analysis chances, the "Most likely"
// ticket's picks).
func (s *Service) freezeJackpot(j *Jackpot, gs []JackpotGame, a *JackpotAnalysis) *JackpotFrozen {
	if a == nil || len(a.Games) != len(gs) || len(gs) == 0 {
		return nil
	}
	now := s.Now()
	fr := &JackpotFrozen{At: now, Picks: append([]string(nil), a.MostLikely.Picks...), Favourites: jackpotFavourites(gs), Source: "frozen at close"}
	if ft := jackpotFreezeAt(*j, gs); !ft.IsZero() && now.After(ft.Add(5*time.Minute)) {
		fr.Source = "frozen after the first kickoff"
	}
	var t JackpotTicket
	if s.DB.Where("jackpot_id = ? AND source = 'most_likely' AND name = 'Most likely'", j.ID).Limit(1).Find(&t).RowsAffected > 0 && len(t.Picks) == len(gs) {
		fr.Picks = append([]string(nil), t.Picks...)
	}
	for i, ga := range a.Games {
		fr.Probs = append(fr.Probs, probsMap(ga.p))
		if i < len(fr.Picks) {
			for _, k := range strings.Split(fr.Picks[i], "") {
				fr.ExpectedCorrect += ga.p[map[string]int{"1": 0, "X": 1, "2": 2}[k]]
			}
		}
	}
	fr.ExpectedCorrect = round2(fr.ExpectedCorrect)
	j.Frozen = fr
	if j.ID != "" {
		s.DB.Model(&Jackpot{}).Where("id = ?", j.ID).Update("frozen", mustJSON(fr))
	}
	return fr
}

// FreezeJackpots snapshots jackpots about to close (scheduler pass).
func (s *Service) FreezeJackpots() int {
	now := s.Now()
	var js []Jackpot
	s.DB.Where("status NOT IN ('graded','draft') AND frozen IS NULL AND (closes_at IS NULL OR closes_at < ?)", now.Add(10*time.Minute)).Limit(200).Find(&js)
	n := 0
	for i := range js {
		j := js[i]
		var gs []JackpotGame
		s.DB.Where("jackpot_id = ?", j.ID).Order("n").Find(&gs)
		ft := jackpotFreezeAt(j, gs)
		if ft.IsZero() || now.Before(ft) {
			continue
		}
		a := s.analyseJackpot(j, gs, j.OrganizationID)
		if s.freezeJackpot(&j, gs, a) != nil {
			n++
		}
	}
	return n
}

func probsP3(m map[string]float64) P3 { return P3{m["1"], m["X"], m["2"]} }

func pickProb(p P3, pick string) float64 {
	s := 0.0
	for _, k := range strings.Split(pick, "") {
		switch k {
		case "1":
			s += p[0]
		case "X":
			s += p[1]
		case "2":
			s += p[2]
		}
	}
	return s
}

// pickOrDraw is a pick's safer double chance (the draw added; for a draw
// pick, the next most likely outcome).
func pickOrDraw(pick string, p P3) string {
	if len(pick) != 1 {
		return pick
	}
	k := map[string]int{"1": 0, "X": 1, "2": 2}[pick]
	other := 1
	if k == 1 {
		other = 0
		if p[2] > p[0] {
			other = 2
		}
	}
	return dcSel(k, other)
}

var numberWords = []string{"No", "One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight", "Nine", "Ten", "Eleven", "Twelve", "Thirteen", "Fourteen", "Fifteen"}

func numWord(n int) string {
	if n >= 0 && n < len(numberWords) {
		return numberWords[n]
	}
	return fmt.Sprint(n)
}

// jackpotReview builds the review and the per-game checks.
func jackpotReview(j Jackpot, gs []JackpotGame, fr *JackpotFrozen, home, away []string) (*JackpotReview, []*JackpotCheck) {
	checks := make([]*JackpotCheck, len(gs))
	if fr == nil || len(fr.Picks) != len(gs) || len(fr.Probs) != len(gs) {
		return nil, checks
	}
	r := &JackpotReview{Complete: true, Picks: fr.Picks, Results: make([]*string, len(gs)), Marks: make([]*bool, len(gs)), FrozenAt: fr.At, Text: []string{}}
	any := false
	ps, minP, maxP := []float64{}, 1.0, 0.0
	noDrawPick := true
	for i, g := range gs {
		p := probsP3(fr.Probs[i])
		pick := fr.Picks[i]
		if strings.Contains(pick, "X") {
			noDrawPick = false
		}
		safer := pickOrDraw(pick, p)
		chk := &JackpotCheck{Status: "pending", Label: "Not played yet", Pick: &Mark{Selection: pick, Label: jpPickLabel(pick, home[i], away[i]), P: round4(pickProb(p, pick))},
			Safer: &Mark{Selection: safer, Label: safeLabel(safer, home[i], away[i]), P: round4(pickProb(p, safer))}, Summary: "No result yet."}
		checks[i] = chk
		if g.Void {
			any = true
			chk.Status, chk.Label, chk.Summary = "void", "Void", "Void — the game wasn't played, so it isn't counted."
			continue
		}
		pp := pickProb(p, pick)
		if pp < minP {
			minP = pp
		}
		if pp > maxP {
			maxP = pp
		}
		if g.Result == "" {
			r.Complete = false
			continue
		}
		any = true
		res := g.Result
		r.Results[i] = &res
		r.Of++
		ps = append(ps, pp)
		r.Expected += pp
		r.DrawsExpected += p[1]
		r.SaferExpected += pickProb(p, safer)
		r.SaferOf++
		ok := strings.Contains(pick, res)
		r.Marks[i] = bp(ok)
		chk.Result = &res
		if g.Score != "" {
			sc := g.Score
			chk.Score = &sc
		}
		chk.Pick.Correct, chk.Safer.Correct = bp(ok), bp(strings.Contains(safer, res))
		if ok {
			r.Correct++
		}
		if res == "X" {
			r.DrawsActual++
		}
		if strings.Contains(safer, res) {
			r.SaferHits++
		}
		if i < len(fr.Favourites) && fr.Favourites[i] != "" {
			r.FavouritesOf++
			if fr.Favourites[i] == res {
				r.FavouritesCorrect++
			}
			if fr.Favourites[i] == pick {
				r.FavouritesSameAsPick++
			}
		}
		ended := "ended " + firstNonEmpty(g.Score, res)
		if res == "X" {
			ended += ", a draw"
		}
		if ok {
			chk.Status, chk.Label = "correct", "Correct"
			chk.Summary = fmt.Sprintf("Correct — %s (the pick was %s, %.0f%%).", ended, chk.Pick.Label, pp*100)
		} else {
			chk.Status, chk.Label = "wrong", "Wrong"
			chk.Summary = fmt.Sprintf("Wrong — %s (the pick was %s, %.0f%%).", ended, chk.Pick.Label, pp*100)
			if *chk.Safer.Correct {
				chk.Summary += " Pick or draw (" + chk.Safer.Label + ") was correct."
			}
		}
	}
	if !any {
		return nil, checks
	}
	e, lo, hi := PBRange(ps)
	r.Expected, r.Low, r.High = round2(e), lo, hi
	r.DrawsExpected, r.SaferExpected = round2(r.DrawsExpected), round2(r.SaferExpected)
	d := PoissonBinomial(ps)
	cum := 0.0
	for k := 0; k <= r.Correct && k < len(d); k++ {
		cum += d[k]
	}
	r.PAtMostCorrect = round4(cum)
	r.Verdict, _ = RangeVerdict(r.Correct, r.Of, r.Expected, r.Low, r.High, "picks")
	switch r.Verdict {
	case "below_range":
		r.Headline = fmt.Sprintf("%d of %d right — below the normal range (%d–%d)", r.Correct, r.Of, r.Low, r.High)
	case "above_range":
		r.Headline = fmt.Sprintf("%d of %d right — above the normal range (%d–%d), a lucky slate", r.Correct, r.Of, r.Low, r.High)
	default:
		r.Headline = fmt.Sprintf("%d of %d right — within the normal range (%d–%d) for these chances", r.Correct, r.Of, r.Low, r.High)
	}
	if !r.Complete {
		r.Headline += " (so far)"
	}
	book := firstNonEmpty(j.Bookmaker, "the bookmaker")
	t1 := fmt.Sprintf("Our most likely ticket got %d/%d.", r.Correct, r.Of)
	if r.FavouritesOf == r.Of && r.FavouritesSameAsPick == r.Of && r.Of > 0 {
		t1 += fmt.Sprintf(" It was identical to %s's own favourites, which also got %d/%d.", book, r.FavouritesCorrect, r.FavouritesOf)
	} else if r.FavouritesOf > 0 {
		t1 += fmt.Sprintf(" %s's own favourites got %d/%d.", book, r.FavouritesCorrect, r.FavouritesOf)
	}
	r.Text = append(r.Text, t1)
	if len(ps) > 0 {
		lead := "Each pick was only a"
		if maxP >= 0.5 {
			lead = "The picks were a"
		}
		r.Text = append(r.Text, fmt.Sprintf("%s %.0f–%.0f%% chance; the best single ticket expected %.1f right, and %d or fewer happens %.0f%% of the time.",
			lead, minP*100, maxP*100, r.Expected, r.Correct, r.PAtMostCorrect*100))
	}
	dl := fmt.Sprintf("%s game%s %s draws (%.1f expected)", numWord(r.DrawsActual), plural(r.DrawsActual), map[bool]string{true: "was a", false: "were"}[r.DrawsActual == 1], r.DrawsExpected)
	if r.DrawsActual == 1 {
		dl = fmt.Sprintf("One game was a draw (%.1f expected)", r.DrawsExpected)
	}
	if noDrawPick {
		dl += "; a single most-likely ticket never picks a draw."
	} else {
		dl += "."
	}
	r.Text = append(r.Text, dl)
	r.Text = append(r.Text, fmt.Sprintf("\"Pick or draw\" on every game would have hit %d/%d.", r.SaferHits, r.SaferOf))
	return r, checks
}

func jpPickLabel(pick, home, away string) string {
	switch pick {
	case "1":
		return home + " win"
	case "X":
		return "Draw"
	case "2":
		return away + " win"
	}
	return safeLabel(pick, home, away)
}

// jackpotWin decorates a jackpot view: snapshot, review and per-game ✓/✗.
func (s *Service) jackpotWin(v *JackpotView, j *Jackpot, gs []JackpotGame) {
	if v == nil {
		return
	}
	fr := j.Frozen
	if fr != nil && len(fr.Picks) != len(gs) {
		fr = nil
	}
	anyResult := false
	for _, g := range gs {
		if g.Result != "" || g.Void {
			anyResult = true
		}
	}
	if fr == nil && j.ID != "" && j.Status != "draft" {
		if ft := jackpotFreezeAt(*j, gs); (!ft.IsZero() && !s.Now().Before(ft)) || anyResult {
			fr = s.freezeJackpot(j, gs, v.Analysis)
		}
	}
	v.Frozen = fr
	v.Jackpot.Frozen = fr
	home, away := make([]string, len(gs)), make([]string, len(gs))
	for i, g := range gs {
		home[i], away[i] = g.Home, g.Away
		if g.Match.Home != "" && (g.Match.Status == "matched" || g.Match.Status == "manual") {
			home[i], away[i] = g.Match.Home, g.Match.Away
		}
	}
	rv, checks := jackpotReview(*j, gs, fr, home, away)
	v.Review = rv
	for i := range v.Games {
		if i < len(checks) && checks[i] != nil && (checks[i].Status != "pending" || anyResult) {
			v.Games[i].ResultCheck = checks[i]
		}
	}
}

// reviewSummary adds the review's numbers to a list row.
func (s *Service) reviewSummary(sm *JackpotSummary, j Jackpot, gs []JackpotGame) {
	if sm.Graded == nil || j.Frozen == nil {
		return
	}
	home, away := make([]string, len(gs)), make([]string, len(gs))
	rv, _ := jackpotReview(j, gs, j.Frozen, home, away)
	if rv == nil {
		return
	}
	sm.Graded.ReviewCorrect, sm.Graded.ReviewExpected, sm.Graded.SaferHits = &rv.Correct, &rv.Expected, &rv.SaferHits
}

// jackpotLines are a person's graded jackpots in a period (scorecard).
func (s *Service) jackpotLines(org, user string, from, to time.Time) []JackpotLine {
	var js []Jackpot
	s.DB.Where("organization_id = ? AND user_id = ? AND grading IS NOT NULL AND COALESCE(closes_at, graded_at, created_at) >= ? AND COALESCE(closes_at, graded_at, created_at) < ?",
		org, user, from, to).Order("COALESCE(closes_at, graded_at, created_at)").Find(&js)
	out := []JackpotLine{}
	for _, j := range js {
		var gs []JackpotGame
		s.DB.Where("jackpot_id = ?", j.ID).Order("n").Find(&gs)
		fr := j.Frozen
		if fr == nil {
			a := s.analyseJackpot(j, gs, org)
			fr = s.freezeJackpot(&j, gs, a)
		}
		rv, _ := jackpotReview(j, gs, fr, make([]string, len(gs)), make([]string, len(gs)))
		if rv == nil {
			continue
		}
		out = append(out, JackpotLine{ID: j.ID, Name: j.Name, Bookmaker: j.Bookmaker, Correct: rv.Correct, Of: rv.Of, Expected: rv.Expected, Low: rv.Low, High: rv.High, GradedAt: j.GradedAt})
	}
	return out
}

// ---------------------------------------------------------------- budget planner

// PlanGame is one game of a plan.
type PlanGame struct {
	N        int     `json:"n"`
	Home     string  `json:"home"`
	Away     string  `json:"away"`
	Covered  string  `json:"covered"`
	Kind     string  `json:"kind"` // single | double | triple
	PCovered float64 `json:"p_covered"`
	Pick     string  `json:"pick"`
}

// PlanTicket is a covered ticket.
type PlanTicket struct {
	Picks           []string   `json:"picks"`
	Combinations    int        `json:"combinations"`
	Cost            float64    `json:"cost"`
	PAll            float64    `json:"p_all"`
	OneIn           float64    `json:"one_in"`
	ExpectedCorrect float64    `json:"expected_correct"`
	Doubles         int        `json:"doubles"`
	Triples         int        `json:"triples"`
	Singles         int        `json:"singles"`
	Games           []PlanGame `json:"games"`
	AtLeast         []KP       `json:"at_least"`
	Graded          *struct {
		Correct    int        `json:"correct"`
		Of         int        `json:"of"`
		AllCorrect bool       `json:"all_correct"`
		Bonus      *BonusTier `json:"bonus"`
	} `json:"graded"`
}

// JackpotPlan is POST /jackpots/:id/plan.
type JackpotPlan struct {
	Budget          float64          `json:"budget"`
	TicketPrice     float64          `json:"ticket_price"`
	Currency        string           `json:"currency"`
	MaxCombinations int              `json:"max_combinations"`
	Plan            PlanTicket       `json:"plan"`
	Single          map[string]any   `json:"single"`
	Ladder          []map[string]any `json:"ladder"`
	Basis           string           `json:"basis"`
	Explain         string           `json:"explain"`
	Disclaimer      string           `json:"disclaimer"`
}

// maskP is the covered probability of a game (bit 0 = 1, 1 = X, 2 = 2).
func maskP(p P3, m uint8) float64 {
	s := 0.0
	for k := 0; k < 3; k++ {
		if m&(1<<k) != 0 {
			s += p[k]
		}
	}
	return s
}

func planValue(ps []P3, ms []uint8) (float64, int) {
	lp, c := 0.0, 1
	for i, m := range ms {
		lp += math.Log(math.Max(maskP(ps[i], m), 1e-300))
		c *= bits.OnesCount8(m)
	}
	return lp, c
}

// localImprove improves a plan under a combination cap: add one outcome, or
// move one outcome from a game to another, while it raises P(all right).
func localImprove(ps []P3, ms []uint8, capC int) []uint8 {
	cur := append([]uint8(nil), ms...)
	curLP, _ := planValue(ps, cur)
	for iter := 0; iter < 60; iter++ {
		bestLP, var_ := curLP, []uint8(nil)
		try := func(c []uint8) {
			lp, n := planValue(ps, c)
			if n <= capC && lp > bestLP+1e-12 {
				bestLP, var_ = lp, append([]uint8(nil), c...)
			}
		}
		for i := range cur {
			for k := 0; k < 3; k++ {
				bit := uint8(1 << k)
				if cur[i]&bit == 0 {
					c := append([]uint8(nil), cur...)
					c[i] |= bit
					try(c)
					// swap: remove another outcome elsewhere
					for j := range cur {
						if bits.OnesCount8(cur[j]) < 2 {
							continue
						}
						for k2 := 0; k2 < 3; k2++ {
							b2 := uint8(1 << k2)
							if cur[j]&b2 != 0 && !(j == i && k2 == k) {
								c2 := append([]uint8(nil), c...)
								c2[j] &^= b2
								if c2[j] != 0 {
									try(c2)
								}
							}
						}
					}
				}
			}
		}
		if var_ == nil {
			break
		}
		cur, curLP = var_, bestLP
	}
	return cur
}

// combosLevels are the achievable combination counts 2^a·3^b ≤ max.
func combosLevels(n, max int) []int {
	out := []int{}
	for a, p2 := 0, 1; a <= n && p2 <= max; a, p2 = a+1, p2*2 {
		for b, p3 := 0, 1; a+b <= n && p2*p3 <= max; b, p3 = b+1, p3*3 {
			out = append(out, p2*p3)
		}
	}
	sort.Ints(out)
	return out
}

// PlanCover chooses doubles / triples maximising P(every game covered)
// within maxC combinations: per achievable combination level, the greedy
// cover order (Δlog P per Δlog combinations) then a local swap pass; the
// answer is the best plan of every level ≤ maxC, so a bigger budget is
// never worse. It returns the masks and the per-level ladder.
func PlanCover(ps []P3, maxC int) ([]uint8, []struct {
	C  int
	LP float64
}) {
	n := len(ps)
	base := make([]uint8, n)
	ml := MostLikely(ps)
	for i, k := range ml {
		base[i] = 1 << k
	}
	if maxC < 1 {
		maxC = 1
	}
	// Greedy prefixes (CoverPriority's order).
	steps, _ := CoverPriority(ps, 2*n)
	prefixes := [][]uint8{append([]uint8(nil), base...)}
	cur := append([]uint8(nil), base...)
	for _, st := range steps {
		ks, _ := ParsePick(st.Picks)
		var m uint8
		for _, k := range ks {
			m |= 1 << k
		}
		cur[st.Game-1] = m
		prefixes = append(prefixes, append([]uint8(nil), cur...))
	}
	best := append([]uint8(nil), base...)
	bestLP, _ := planValue(ps, best)
	ladder := []struct {
		C  int
		LP float64
	}{}
	for _, lv := range combosLevels(n, maxC) {
		start := best
		for _, pf := range prefixes {
			if lp, c := planValue(ps, pf); c <= lv && lp > bestLP {
				start = pf
			}
		}
		cand := localImprove(ps, start, lv)
		if lp, _ := planValue(ps, cand); lp > bestLP+1e-12 {
			best, bestLP = cand, lp
			_, c := planValue(ps, best)
			ladder = append(ladder, struct {
				C  int
				LP float64
			}{c, lp})
		}
	}
	return best, ladder
}

func maskPick(m uint8) string {
	ks := []int{}
	for k := 0; k < 3; k++ {
		if m&(1<<k) != 0 {
			ks = append(ks, k)
		}
	}
	return pickString(ks)
}

// PlanJackpot is the budget planner of a jackpot (frozen chances when the
// jackpot has closed, else the current analysis).
func (s *Service) PlanJackpot(org, user, id string, budget float64) (*JackpotPlan, error) {
	j, gs, err := s.loadJackpot(org, user, id)
	if err != nil {
		return nil, err
	}
	if j.TicketPrice <= 0 {
		return nil, inputErr("Set the ticket price first.")
	}
	if budget > 10000000 {
		return nil, inputErr("The budget must be at most 10,000,000.")
	}
	if budget < j.TicketPrice {
		return nil, inputErr("The budget doesn't cover one ticket (%s %s).", j.Currency, fmtThousands(j.TicketPrice))
	}
	if len(gs) == 0 {
		return nil, inputErr("The jackpot has no games.")
	}
	ps := make([]P3, len(gs))
	basis := "current chances (the jackpot hasn't closed)"
	if j.Frozen != nil && len(j.Frozen.Probs) == len(gs) {
		for i := range gs {
			ps[i] = probsP3(j.Frozen.Probs[i])
		}
		basis = "chances frozen when the jackpot closed"
	} else {
		a := s.analyseJackpot(j, gs, org)
		for i := range a.Games {
			ps[i] = a.Games[i].p
		}
	}
	maxC := int(math.Floor(budget/j.TicketPrice + 1e-9))
	if maxC > 1000000 {
		maxC = 1000000
	}
	masks, ladder := PlanCover(ps, maxC)
	out := &JackpotPlan{Budget: budget, TicketPrice: j.TicketPrice, Currency: j.Currency, MaxCombinations: maxC, Basis: basis, Disclaimer: JackpotDisclaimer, Ladder: []map[string]any{}}
	out.Plan = planTicket(ps, masks, gs, j)
	ml := MostLikely(ps)
	pml := TicketProb(ps, ml)
	out.Single = map[string]any{"p_all": pml, "one_in": math.Round(1 / math.Max(pml, 1e-300)), "cost": j.TicketPrice}
	for _, l := range ladder {
		p := math.Exp(l.LP)
		out.Ladder = append(out.Ladder, map[string]any{"budget": round2(float64(l.C) * j.TicketPrice), "combinations": l.C, "cost": round2(float64(l.C) * j.TicketPrice),
			"p_all": p, "one_in": math.Round(1 / p)})
	}
	out.Explain = fmt.Sprintf("On a coin-flip slate no formula picks the winners; covering games with doubles and triples is the only lever. "+
		"With %s %s you can afford %d combination%s: this plan has %d, so the chance that one of them gets all %d right is about 1 in %s (one ticket: 1 in %s). "+
		"Every double doubles the cost; the jackpot is still very unlikely.", j.Currency, fmtThousands(budget), maxC, plural(maxC), out.Plan.Combinations, len(gs),
		fmtThousands(out.Plan.OneIn), fmtThousands(math.Round(1/math.Max(pml, 1e-300))))
	return out, nil
}

func planTicket(ps []P3, masks []uint8, gs []JackpotGame, j Jackpot) PlanTicket {
	lp, c := planValue(ps, masks)
	t := PlanTicket{Combinations: c, Cost: round2(float64(c) * j.TicketPrice), PAll: math.Exp(lp), Games: []PlanGame{}, AtLeast: []KP{}}
	if t.PAll > 0 {
		t.OneIn = math.Round(1 / t.PAll)
	}
	qs := make([]float64, len(ps))
	ml := MostLikely(ps)
	for i, m := range masks {
		pk := maskPick(m)
		t.Picks = append(t.Picks, pk)
		qs[i] = maskP(ps[i], m)
		t.ExpectedCorrect += qs[i]
		kind := "single"
		switch bits.OnesCount8(m) {
		case 2:
			kind = "double"
			t.Doubles++
		case 3:
			kind = "triple"
			t.Triples++
		default:
			t.Singles++
		}
		h, a := "", ""
		if i < len(gs) {
			h, a = gs[i].Home, gs[i].Away
			if gs[i].Match.Home != "" && (gs[i].Match.Status == "matched" || gs[i].Match.Status == "manual") {
				h, a = gs[i].Match.Home, gs[i].Match.Away
			}
		}
		t.Games = append(t.Games, PlanGame{N: i + 1, Home: h, Away: a, Covered: pk, Kind: kind, PCovered: round4(qs[i]), Pick: selKeys[ml[i]]})
	}
	t.ExpectedCorrect = round2(t.ExpectedCorrect)
	dist := PoissonBinomial(qs)
	ks := []int{len(ps)}
	for _, b := range j.Bonuses {
		if b.Correct > 0 && b.Correct < len(ps) {
			ks = append(ks, b.Correct)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ks)))
	seen := map[int]bool{}
	for _, k := range ks {
		if !seen[k] {
			seen[k] = true
			t.AtLeast = append(t.AtLeast, KP{K: k, P: AtLeast(dist, k)})
		}
	}
	// After grading: how many games the plan covered.
	anyRes, complete := false, true
	cor, of := 0, 0
	for i, g := range gs {
		if g.Void {
			continue
		}
		if g.Result == "" {
			complete = false
			continue
		}
		anyRes = true
		of++
		if i < len(t.Picks) && strings.Contains(t.Picks[i], g.Result) {
			cor++
		}
	}
	if anyRes {
		t.Graded = &struct {
			Correct    int        `json:"correct"`
			Of         int        `json:"of"`
			AllCorrect bool       `json:"all_correct"`
			Bonus      *BonusTier `json:"bonus"`
		}{Correct: cor, Of: of, AllCorrect: complete && cor == of && of > 0}
		if complete {
			for _, b := range j.Bonuses {
				if b.Correct == cor {
					bb := b
					t.Graded.Bonus = &bb
				}
			}
		}
	}
	return t
}
