package football

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/football/model"
)

// Unit tests of v2.19.1 (no database): the lock wording, the data gate, the
// model-only cap, the Elo replay across competitions, the planner's history
// backfill and the football-data.co.uk seeding.

func TestLockLabels(t *testing.T) {
	ko := time.Date(2026, 11, 24, 17, 45, 0, 0, time.UTC)
	cases := map[time.Duration]string{
		2*time.Hour + 10*time.Minute: "Locked 2 h 10 min before kick-off",
		45 * time.Minute:             "Locked 45 min before kick-off",
		3 * time.Hour:                "Locked 3 h before kick-off",
		27*time.Hour + 5*time.Minute: "Locked 1 day 3 h before kick-off",
		50 * time.Hour:               "Locked 2 days 2 h before kick-off",
		20 * time.Second:             "Locked less than a minute before kick-off",
	}
	for d, want := range cases {
		if got := LockedLabel(ko.Add(-d), ko); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
	rc := NoDataCheck("0-3", "No prices and too little match history for these teams")
	if rc.Status != "not_enough_data" || rc.Label != "No pick" || *rc.Outcome != "2" || rc.Pick != nil ||
		rc.Summary != "No pick — not enough data (no prices and too little match history for these teams)." {
		t.Fatalf("no-data check %+v", rc)
	}
	if np := NoPickCheck("1-1"); np.Summary != "No pick — not locked before kick-off." || np.Status != "no_pick" {
		t.Fatalf("no-pick check %+v", np)
	}
}

func p3(h, d, a float64) *P3 { p := P3{h, d, a}; return &p }

// The AI tools unwrap any object with a "data" key as an API envelope: the
// v2.19.1 additions must never be serialised under that name.
func TestLockJSONNoDataKey(t *testing.T) {
	v := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{0.5, 0.3, 0.2}, AsOf: time.Now()})
	v.Data = &GateOut{OK: true, Solid: []string{"x"}}
	var m map[string]any
	if err := jsonUnmarshal([]byte(mustJSON(v)), &m); err != nil || m["data"] != nil || m["evidence"] == nil {
		t.Fatalf("verdict json %v %v", err, m["data"])
	}
	d := PredDetail{Data: &ModelData{Solid: true}}
	var pm map[string]any
	if jsonUnmarshal([]byte(mustJSON(d)), &pm); pm["data"] != nil || pm["model_data"] == nil {
		t.Fatalf("prediction detail json %v", pm)
	}
}

func TestDataGate(t *testing.T) {
	thin := &ModelData{EloHome: 0, EloAway: 6, Why: "Too little match history"}
	solidElo := &ModelData{EloHome: 40, EloAway: 31, EloOK: true, Solid: true, HasData: true}
	solidDC := &ModelData{DCFitted: true, DCHome: 9, DCAway: 12, DCOK: true, Solid: true, HasData: true}
	mk := func() consInputs { in := newConsInputs(); in.Why["clubelo"] = "switched off"; return in }

	// The production case (UCL, no market, ClubElo off, almost no history): no pick, plain reason.
	in := mk()
	in.Model = thin
	g := gateFor(in)
	if g.OK || g.Reason != "No prices and too little match history for these teams" || len(g.Checks) != 5 || len(g.Solid) != 0 {
		t.Fatalf("thin gate %+v", g)
	}
	// One bookmaker and thin history: still no pick.
	in = mk()
	in.Model, in.P["market"], in.Books = thin, p3(0.5, 0.3, 0.2), 1
	if g := gateFor(in); g.OK || !strings.HasPrefix(g.Reason, "Only one bookmaker") {
		t.Fatalf("one book %+v", g)
	}
	// Two bookmakers / a sharp price / a Kenyan price / a typed-in price / ClubElo / API-Football / Polymarket each suffice.
	for name, set := range map[string]func(*consInputs){
		"two books": func(in *consInputs) { in.P["market"], in.Books = p3(0.5, 0.3, 0.2), 2 },
		"sharp":     func(in *consInputs) { in.P["sharp"] = p3(0.5, 0.3, 0.2) },
		"kenyan":    func(in *consInputs) { in.P["market"], in.Books, in.KEorOP = p3(0.5, 0.3, 0.2), 1, true },
		"typed":     func(in *consInputs) { in.P["market"], in.Books, in.Typed = p3(0.5, 0.3, 0.2), 0, true },
		"clubelo":   func(in *consInputs) { in.P["clubelo"] = p3(0.2, 0.25, 0.55) },
		"apfpred":   func(in *consInputs) { in.P["apifootball_pred"] = p3(0.45, 0.3, 0.25) },
		"poly":      func(in *consInputs) { in.P["polymarket"] = p3(0.45, 0.3, 0.25) },
	} {
		in := mk()
		in.Model = thin
		set(&in)
		g := gateFor(in)
		if !g.OK || len(g.Solid) != 1 {
			t.Fatalf("%s: %+v", name, g)
		}
		wantModelOnly := name == "apfpred"
		if g.ModelOnly != wantModelOnly {
			t.Fatalf("%s: model only %v", name, g.ModelOnly)
		}
	}
	// A model that knows the teams is enough — but it's "model only".
	for _, md := range []*ModelData{solidElo, solidDC} {
		in := mk()
		in.Model = md
		if g := gateFor(in); !g.OK || !g.ModelOnly {
			t.Fatalf("solid model %+v", g)
		}
	}
	// Model-only picks are never Strong (capped at Good) and say so.
	v := BuildVerdict(VerdictInput{Home: "Caspian Harbour FA", Away: "Costa Brava CF", Probs: P3{0.79, 0.13, 0.08}, AsOf: time.Now()})
	if v.Tier.Key != "strong" {
		t.Fatalf("tier %s", v.Tier.Key)
	}
	capModelOnly(&v, GateOut{OK: true, ModelOnly: true})
	if v.Tier.Key != "good" || v.Tier.Label != "Good" || !v.Tier.Capped || v.Tier.Note != ModelOnlyNote || v.P != 0.79 {
		t.Fatalf("capped %+v", v.Tier)
	}
	w := BuildVerdict(VerdictInput{Home: "A", Away: "B", Probs: P3{0.44, 0.3, 0.26}, AsOf: time.Now()})
	capModelOnly(&w, GateOut{OK: true, ModelOnly: true})
	if w.Tier.Key != "close" || w.Tier.Capped || w.Tier.Note != ModelOnlyNote {
		t.Fatalf("close stays close %+v", w.Tier)
	}
}

func TestModelDataThresholds(t *testing.T) {
	e := model.NewElo(crossCompElo())
	d := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 15; i++ { // team 1: 15 matches, team 2: 14
		e.Update(model.Match{Date: d, Home: "1", Away: fmt.Sprint(100 + i), HG: 1, AG: 0})
		if i < 14 {
			e.Update(model.Match{Date: d, Home: "2", Away: fmt.Sprint(200 + i), HG: 0, AG: 0})
		}
		d = d.Add(24 * time.Hour)
	}
	md := modelDataFrom(e, dcEntry{}, 1, 2, "Home FC", "Away FC")
	if md.EloOK || md.Solid || !md.HasData || md.EloHome != 15 || md.EloAway != 14 || !strings.Contains(md.Why, "Away FC 14 stored results (15 each needed)") {
		t.Fatalf("14 matches %+v", md)
	}
	e.Update(model.Match{Date: d, Home: "2", Away: "300", HG: 2, AG: 0})
	if md := modelDataFrom(e, dcEntry{}, 1, 2, "Home FC", "Away FC"); !md.EloOK || !md.Solid {
		t.Fatalf("15 each %+v", md)
	}
	// Both teams unknown (or almost): the model's numbers are hidden.
	if md := modelDataFrom(e, dcEntry{}, 900, 901, "X", "Y"); md.HasData || md.Solid {
		t.Fatalf("unknown teams %+v", md)
	}
	for i := 0; i < 4; i++ {
		e.Update(model.Match{Date: d, Home: "900", Away: "901", HG: 1, AG: 1})
	}
	if md := modelDataFrom(e, dcEntry{}, 900, 901, "X", "Y"); md.HasData || md.EloHome != 4 {
		t.Fatalf("4 matches each %+v", md)
	}
	e.Update(model.Match{Date: d, Home: "901", Away: "900", HG: 0, AG: 2})
	if md := modelDataFrom(e, dcEntry{}, 900, 901, "X", "Y"); !md.HasData || md.Solid {
		t.Fatalf("5 matches each %+v", md)
	}
	// Dixon–Coles: both teams ≥ 8 matches in the competition's fit.
	dc := dcEntry{p: &model.DCParams{Attack: map[string]float64{"900": 0.1, "901": -0.1}}, n: 60, teams: map[string]int{"900": 8, "901": 7}}
	if md := modelDataFrom(e, dc, 900, 901, "X", "Y"); md.DCOK || !md.HasData {
		t.Fatalf("dc 8/7 %+v", md)
	}
	dc.teams["901"] = 8
	if md := modelDataFrom(e, dc, 900, 901, "X", "Y"); !md.DCOK || !md.Solid {
		t.Fatalf("dc 8/8 %+v", md)
	}
}

// The production bug: Barcelona at 1280 next to an unrated 1500. The tuned
// new-team offset (−50 vs the mean of active teams) drags the mean down for
// every team first seen later, when hundreds of teams of many competitions
// are replayed together. v2.19.1 starts every team at 1500.
func TestEloReplayAcrossCompetitions(t *testing.T) {
	replay := func(o model.EloOptions) (*model.Elo, float64) {
		e := model.NewElo(o)
		r := rand.New(rand.NewSource(7))
		d := time.Date(2025, 8, 1, 0, 0, 0, 0, time.UTC)
		for l := 0; l < 30; l++ { // 30 competitions × 12 teams, first seen one after another
			for round := 0; round < 3; round++ {
				for i := 0; i < 6; i++ {
					e.Update(model.Match{Date: d, Home: fmt.Sprintf("L%dT%d", l, (i+round)%12), Away: fmt.Sprintf("L%dT%d", l, (i+6+round)%12), HG: r.Intn(3), AG: r.Intn(3)})
					d = d.Add(time.Hour)
				}
			}
		}
		sum := 0.0
		for _, v := range e.Ratings() {
			sum += v
		}
		return e, sum / float64(len(e.Ratings()))
	}
	old, oldMean := replay(model.DefaultEloOptions())
	if oldMean > 1350 || old.Rating("L29T0") > 1300 {
		t.Fatalf("expected the old drift (mean %.0f, late team %.0f)", oldMean, old.Rating("L29T0"))
	}
	e, mean := replay(crossCompElo())
	if mean < 1499 || mean > 1501 || e.Rating("L29T0") < 1400 || e.Rating("nobody") != 1500 {
		t.Fatalf("cross-competition replay drifted: mean %.1f, late team %.0f", mean, e.Rating("L29T0"))
	}
	if eloOptions.NewTeamOffset != 0 || eloOptions.K != model.DefaultEloOptions().K {
		t.Fatalf("service Elo options %+v", eloOptions)
	}
}

// API-Football history: the current season weekly and the previous one at
// the lowest priority (within the daily budget) — the last two seasons.
func TestPlanHistoryBackfill(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	st := Settings{APIFootballDaily: 400, LookaheadDays: 3, APIFootballPlan: "pro", LivePolling: true, LiveIntervalSec: 60}
	in := PlanInput{Now: now, Settings: st, HasAF: true, State: map[string]TaskState{}, StandingsAt: map[int64]time.Time{}, LastFinished: map[int64]time.Time{},
		PlayersAt: map[int64]time.Time{}, Comps: []PlanComp{{ID: 2, Season: 2026, Type: "Cup", Known: true}, {ID: 140, Season: 2026, Type: "League", Known: true}}}
	found := map[string]int{}
	for _, tk := range Plan(in) {
		if tk.Kind == TSeason {
			found[tk.Key] = tk.Prio
		}
	}
	for _, c := range []int64{2, 140} {
		cur, prev := fmt.Sprintf("af:season:%d:2026", c), fmt.Sprintf("af:season:%d:2025", c)
		if _, ok := found[cur]; !ok {
			t.Fatalf("no current season for %d: %v", c, found)
		}
		if p, ok := found[prev]; !ok || p != PrioBackfill {
			t.Fatalf("previous season of %d not at the lowest priority: %v", c, found)
		}
	}
}

// football-data.co.uk: once switched on (v2.19.1: by default), past seasons
// are seeded once and the current season weekly.
func TestPlanFDCOUKSeeding(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	fs := defaultFreeSettings()
	if !fs.Enabled[ProviderFDCOUK] || !fs.Enabled[ProviderClubElo] || !fs.Enabled[ProviderOpenFootball] || fs.Enabled[ProviderOddsPapi] || fs.Enabled[ProviderPolymarket] ||
		fs.Enabled[ProviderAFPred] || fs.Enabled[ProviderFDOrg] || fs.Enabled[ProviderGNews] {
		t.Fatalf("defaults %v", fs.Enabled)
	}
	in := FreePlanInput{Now: now, Settings: fs, On: fs.Enabled, State: map[string]TaskState{},
		FD: []fdTarget{{Path: "/mmz4281/2627/SP1.csv", Div: "SP1", Season: 2026, Comp: 140, Key: "fd:main:SP1:2026", Weekly: true},
			{Path: "/mmz4281/2526/SP1.csv", Div: "SP1", Season: 2025, Comp: 140, Key: "fd:main:SP1:2025"}}}
	keys := func() map[string]bool {
		m := map[string]bool{}
		for _, tk := range PlanFree(in) {
			if tk.Kind == TFDFile {
				m[tk.Key] = true
			}
		}
		return m
	}
	if k := keys(); !k["fd:main:SP1:2026"] || !k["fd:main:SP1:2025"] {
		t.Fatalf("first seed %v", k)
	}
	day := now.Add(-24 * time.Hour)
	in.State["fd:main:SP1:2026"] = TaskState{Key: "fd:main:SP1:2026", LastRunAt: day, LastOKAt: &day, Status: "ok"}
	in.State["fd:main:SP1:2025"] = TaskState{Key: "fd:main:SP1:2025", LastRunAt: day, LastOKAt: &day, Status: "ok"}
	if k := keys(); len(k) != 0 {
		t.Fatalf("seeded again within a week %v", k)
	}
	in.Now = now.Add(7 * 24 * time.Hour)
	if k := keys(); !k["fd:main:SP1:2026"] || k["fd:main:SP1:2025"] {
		t.Fatalf("weekly refresh %v", k)
	}
}
