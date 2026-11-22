package football

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// DB tests of v2.18: freezing and refreshing verdicts until kickoff, never
// regrading, the grading fallback, the scorecard maths, jackpot freeze /
// review / planner, the three new providers against captured fake payloads
// (budgets, fetch log, matching, aliases), learned weights and the HTTP API.

func winSvc(t *testing.T, now time.Time) (*Service, *gorm.DB, seedW) {
	t.Helper()
	db := testDB(t)
	s := newSvc(t, db, now)
	s.Sleep = func(time.Duration) {}
	return s, db, seedW{t, db, s}
}

func enableWin(t *testing.T, s *Service, keys ...string) {
	t.Helper()
	en := map[string]bool{}
	for _, k := range keys {
		en[k] = true
	}
	if err := s.SaveSources(SourcesPatch{Enabled: en}, "test"); err != nil {
		t.Fatal(err)
	}
}

func verdictRow(t *testing.T, db *gorm.DB, fid int64) (VerdictRow, bool) {
	t.Helper()
	var r VerdictRow
	ok := db.Where("fixture_id = ?", fid).Limit(1).Find(&r).RowsAffected > 0
	return r, ok
}

func TestWinFreezeRefreshGrade(t *testing.T) {
	ko := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	s, db, w := winSvc(t, ko.Add(-3*time.Hour))
	w.team(9001, "Northbridge Athletic", "England")
	w.team(9002, "Kingsport United", "England")
	db.Model(&Competition{}).Where("id = 39").Update("enabled", true)
	w.fixture(7000001, 39, ko, 9001, 9002, "R15")
	w.fixture(7000002, 39, ko, 9002, 9001, "R15") // finishes without a frozen pick
	w.market(7000001, [3]float64{1.50, 4.40, 7.30}, ko.Add(-3*time.Hour), true)
	// Before the window: nothing frozen; the list shows a live verdict.
	if n := s.FreezeVerdicts(); n != 0 {
		t.Fatalf("frozen too early: %d", n)
	}
	rows, _ := s.Fixtures(FixtureQuery{From: ptr(ko.Add(-time.Hour)), To: ptr(ko.Add(time.Hour))})
	var live *Verdict
	for _, r := range rows {
		if r.ID == 7000001 {
			live = r.Verdict
		}
	}
	if live == nil || live.Kind != "live" || live.Pick != "1" || live.Tier.Key != "strong" || !live.Consensus.MarketUsed {
		t.Fatalf("live verdict %+v", live)
	}
	// T-25: frozen with the market of the moment.
	s.SetClock(ko.Add(-25 * time.Minute))
	if n := s.FreezeVerdicts(); n != 1 {
		t.Fatalf("freeze: %d", n)
	}
	r1, ok := verdictRow(t, db, 7000001)
	if !ok || r1.Verdict.Kind != "frozen" || r1.Verdict.FrozenAt == nil || r1.Pick != "1" || r1.FavPick != "1" || r1.Grp != "top5" || r1.GradedAt != nil {
		t.Fatalf("frozen row %+v", r1)
	}
	// Nothing changed: no refresh.
	s.SetClock(ko.Add(-20 * time.Minute))
	if n := s.FreezeVerdicts(); n != 0 {
		t.Fatalf("refreshed without new inputs: %d", n)
	}
	// T-10: a new Pinnacle price → refreshed (the last pre-kickoff refresh).
	s.SetClock(ko.Add(-10 * time.Minute))
	w.odds(7000001, ProviderOddsAPI, "pinnacle", "Pinnacle", [3]float64{1.70, 3.90, 5.20}, ko.Add(-11*time.Minute))
	if n := s.FreezeVerdicts(); n != 1 {
		t.Fatalf("refresh: %d", n)
	}
	r2, _ := verdictRow(t, db, 7000001)
	if !r2.FrozenAt.After(r1.FrozenAt) || r2.P >= r1.P {
		t.Fatalf("refresh kept the old verdict: %v %v", r1.P, r2.P)
	}
	// After kickoff a new price changes nothing.
	s.SetClock(ko.Add(5 * time.Minute))
	w.odds(7000001, ProviderOddsAPI, "pinnacle", "Pinnacle", [3]float64{3.0, 3.0, 2.5}, ko.Add(4*time.Minute))
	s.FreezeVerdicts()
	r3, _ := verdictRow(t, db, 7000001)
	if r3.P != r2.P || !r3.FrozenAt.Equal(r2.FrozenAt) {
		t.Fatal("changed after kickoff")
	}
	// Full time 1-1: graded once (pick wrong, safer right) …
	w.result(7000001, "FT", ip(1), ip(1), ko.Add(2*time.Hour))
	w.result(7000002, "FT", ip(0), ip(1), ko.Add(2*time.Hour))
	s.SetClock(ko.Add(3 * time.Hour))
	if n := s.GradeVerdicts(); n != 1 {
		t.Fatalf("graded %d", n)
	}
	g, _ := verdictRow(t, db, 7000001)
	if g.Status != "wrong" || g.Score != "1-1" || g.Outcome != "X" || *g.PickOK || !*g.SaferOK || g.FavOK == nil || *g.FavOK || g.Check == nil || g.ResultSource != ProviderAPIFootball {
		t.Fatalf("marks %+v", g)
	}
	// … and never again, even when the score is corrected later.
	w.result(7000001, "FT", ip(2), ip(1), ko.Add(5*time.Hour))
	s.SetClock(ko.Add(6 * time.Hour))
	s.GradeVerdicts()
	g2, _ := verdictRow(t, db, 7000001)
	if g2.Status != "wrong" || g2.Score != "1-1" || !g2.GradedAt.Equal(*g.GradedAt) {
		t.Fatal("regraded after the fact")
	}
	// The list: frozen verdict + ✓/✗; the match without a frozen pick says so.
	rows, _ = s.Fixtures(FixtureQuery{From: ptr(ko.Add(-time.Hour)), To: ptr(ko.Add(time.Hour))})
	for _, r := range rows {
		switch r.ID {
		case 7000001:
			if r.Verdict == nil || r.Verdict.Kind != "frozen" || r.ResultCheck == nil || r.ResultCheck.Status != "wrong" || *r.ResultCheck.Score != "1-1" {
				t.Fatalf("graded row %+v %+v", r.Verdict, r.ResultCheck)
			}
		case 7000002:
			if r.Verdict != nil || r.ResultCheck == nil || r.ResultCheck.Status != "no_pick" {
				t.Fatalf("no-pick row %+v", r.ResultCheck)
			}
		}
	}
	// The match page.
	a, err := s.Analysis(7000001, "o1", "u1")
	if err != nil || a.Verdict == nil || a.ResultCheck == nil || a.ResultCheck.Status != "wrong" || a.SourceProbs == nil {
		t.Fatalf("analysis %v %+v", err, a.ResultCheck)
	}
	// Rescheduled before kickoff: the old pick is dropped and refrozen later.
	w.fixture(7000003, 39, ko.Add(24*time.Hour), 9001, 9002, "R16")
	w.market(7000003, [3]float64{2.1, 3.3, 3.6}, ko.Add(20*time.Hour), true)
	s.SetClock(ko.Add(24*time.Hour - 20*time.Minute))
	s.FreezeVerdicts()
	db.Model(&Fixture{}).Where("id = 7000003").Update("kickoff", ko.Add(72*time.Hour))
	s.FreezeVerdicts()
	if _, ok := verdictRow(t, db, 7000003); ok {
		t.Fatal("pick of a rescheduled match kept")
	}
	// Abandoned after the freeze: void.
	w.fixture(7000004, 39, ko.Add(30*time.Hour), 9002, 9001, "R16")
	w.market(7000004, [3]float64{2.4, 3.3, 3.0}, ko.Add(28*time.Hour), true)
	s.SetClock(ko.Add(30*time.Hour - 15*time.Minute))
	s.FreezeVerdicts()
	w.result(7000004, "ABD", nil, nil, ko.Add(31*time.Hour))
	s.SetClock(ko.Add(33 * time.Hour))
	s.GradeVerdicts()
	if v, _ := verdictRow(t, db, 7000004); v.Status != "void" || v.Check == nil || v.Check.Label != "Void" {
		t.Fatalf("void %+v", v)
	}
}

func TestWinGradeFallback(t *testing.T) {
	ko := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	s, db, w := winSvc(t, ko.Add(-20*time.Minute))
	w.team(9001, "Northbridge Athletic", "England")
	w.team(9002, "Kingsport United", "England")
	db.Model(&Competition{}).Where("id = 39").Update("enabled", true)
	w.fixture(7000011, 39, ko, 9001, 9002, "R15")
	w.market(7000011, [3]float64{2.0, 3.4, 3.9}, ko.Add(-time.Hour), true)
	s.FreezeVerdicts()
	db.Create(&OFMatch{Code: "en.1", Season: "2026-27", Date: ko.Truncate(24 * time.Hour), Team1: "Northbridge Athletic FC", Team2: "Kingsport United FC",
		HG: ip(0), AG: ip(2), CompetitionID: 39, HomeID: 9001, AwayID: 9002, FixtureID: 7000011, FetchedAt: ko.Add(3 * time.Hour)})
	// API-Football still shows the match as not finished: wait 6 h after the expected end.
	s.SetClock(ko.Add(7 * time.Hour))
	if n := s.GradeVerdicts(); n != 0 {
		t.Fatal("fallback used too early")
	}
	s.SetClock(ko.Add(8*time.Hour + time.Minute))
	if n := s.GradeVerdicts(); n != 1 {
		t.Fatal("fallback not used")
	}
	r, _ := verdictRow(t, db, 7000011)
	if r.Status != "wrong" || r.Score != "0-2" || r.ResultSource != ProviderOpenFootball || *r.Check.ResultSource != ProviderOpenFootball {
		t.Fatalf("fallback grade %+v", r)
	}
	// The source check shows openfootball's kickoff and (once API-Football has it) the score comparison.
	w.result(7000011, "FT", ip(0), ip(2), ko.Add(9*time.Hour))
	var f Fixture
	db.Where("id = 7000011").First(&f)
	sc := s.SourceCheckFor(f)
	found := false
	for _, it := range sc.Items {
		if it.Key == "result" && it.Status == "agree" && len(it.Values) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("result item %+v", sc.Items)
	}
}

func TestWinScorecard(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	s, db, w := winSvc(t, now)
	db.Model(&Competition{}).Where("id IN (39, 276)").Update("enabled", true)
	w.team(9001, "A", "England")
	w.team(9002, "B", "England")
	nbo, _ := time.LoadLocation("Africa/Nairobi")
	type g struct {
		ko     time.Time
		p      float64
		tier   string
		ok     bool
		status string
		comp   int64
		fav    string
	}
	sat := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	gs := []g{
		{sat, 0.66, "strong", true, "correct", 39, "1"}, {sat, 0.55, "good", false, "wrong", 39, "1"}, {sat, 0.45, "close", false, "wrong", 39, "2"},
		{sat, 0.38, "coinflip", true, "correct", 276, "1"}, {sat, 0.40, "coinflip", false, "wrong", 276, "1"}, {sat.Add(24 * time.Hour), 0.41, "coinflip", false, "wrong", 39, "1"},
		{sat, 0.5, "good", false, "void", 39, "1"},
		{sat.AddDate(0, 0, -7), 0.7, "strong", true, "correct", 39, "1"}, // previous weekend
		{sat.AddDate(0, 0, -3), 0.6, "strong", true, "correct", 39, "1"}, // a Wednesday
	}
	for i, x := range gs {
		fid := int64(7100001 + i)
		w.fixture(fid, x.comp, x.ko, 9001, 9002, "R")
		row := VerdictRow{FixtureID: fid, CompetitionID: x.comp, Kickoff: x.ko, Grp: "top5", FrozenAt: x.ko.Add(-20 * time.Minute), Pick: "1", P: x.p, Tier: x.tier,
			Safer: "1X", SaferP: x.p + 0.25, FavPick: x.fav, Status: x.status, GradedAt: ptr(x.ko.Add(2 * time.Hour)), Outcome: "1", Score: "1-0",
			Verdict: Verdict{PickLabel: "A win", Safer: SaferOut{Label: "A or draw"}}, GoalsPick: "under", GoalsP: ptr(0.55), TopScore: "1-0", TopScoreP: 0.11}
		if x.status != "void" {
			row.PickOK, row.SaferOK, row.GoalsOK, row.ScoreOK = ptr(x.ok), ptr(true), ptr(i%2 == 0), ptr(x.ok)
			row.FavOK = ptr(x.fav == "1" && x.ok || x.fav == "2" && !x.ok)
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	// A finished match without a pick.
	w.fixture(7100099, 39, sat, 9002, 9001, "R")
	w.result(7100099, "FT", ip(1), ip(0), sat.Add(2*time.Hour))
	sc, err := s.ScorecardFor(ScoreQuery{Loc: nbo})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Period.Key != "weekend" || sc.Period.From.Format(time.RFC3339) != "2026-11-19T21:00:00Z" || sc.Period.To.Format(time.RFC3339) != "2026-11-23T21:00:00Z" {
		t.Fatalf("weekend window %+v", sc.Period)
	}
	ps := []float64{0.66, 0.55, 0.45, 0.38, 0.40, 0.41}
	e, lo, hi := PBRange(ps)
	tt := sc.Totals
	if tt.Graded != 6 || tt.Correct != 2 || tt.Void != 1 || tt.NoPick != 1 || tt.Expected != round2(e) || tt.ExpectedLow != lo || tt.ExpectedHigh != hi || tt.Verdict != "bad_luck" {
		t.Fatalf("totals %+v (want e %.2f %d–%d)", tt, e, lo, hi)
	}
	if sc.Safer.Graded != 6 || sc.Safer.Correct != 6 || sc.Goals.Graded != 6 || sc.ExactScores.Correct != 2 || sc.Favourite.Graded != 6 || sc.Favourite.Agreement != 5 {
		t.Fatalf("lines %+v %+v %+v %+v", sc.Safer, sc.Goals, sc.ExactScores, sc.Favourite)
	}
	if len(sc.ByTier) != 4 || sc.ByTier[3].Graded != 3 || sc.ByTier[3].Correct != 1 || sc.ByTier[0].HistoryHitRate != 0.7183 || len(sc.ByCompetition) != 2 || sc.ByCompetition[0].CompetitionID != 39 {
		t.Fatalf("by tier / competition %+v %+v", sc.ByTier, sc.ByCompetition)
	}
	if len(sc.Rows) != 7 || sc.Rows[0].Kickoff.Before(sc.Rows[len(sc.Rows)-1].Kickoff) {
		t.Fatal("rows newest first")
	}
	if len(sc.Weekly) != 2 || sc.Weekly[1].Graded != 7 {
		t.Fatalf("weekly %+v", sc.Weekly)
	}
	// Tier and competition filters; 30 days; custom; bad input.
	if sc, _ := s.ScorecardFor(ScoreQuery{Loc: nbo, Tier: "coinflip"}); sc.Totals.Graded != 3 {
		t.Fatal("tier filter")
	}
	if sc, _ := s.ScorecardFor(ScoreQuery{Loc: nbo, Period: "30d", Competition: 39}); sc.Totals.Graded != 6 {
		t.Fatalf("30d comp 39: %d", sc.Totals.Graded)
	}
	from := time.Date(2026, 11, 18, 0, 0, 0, 0, nbo)
	to := time.Date(2026, 11, 18, 0, 0, 0, 0, nbo)
	if sc, err := s.ScorecardFor(ScoreQuery{Loc: nbo, Period: "custom", From: &from, To: &to}); err != nil || sc.Totals.Graded != 1 {
		t.Fatalf("custom one day %v", err)
	}
	if _, err := s.ScorecardFor(ScoreQuery{Loc: nbo, Period: "custom"}); err == nil {
		t.Fatal("custom without from")
	}
	if _, err := s.ScorecardFor(ScoreQuery{Loc: nbo, Period: "year"}); err == nil {
		t.Fatal("bad period")
	}
	if b := s.LastWeekendBanner(nbo); b == nil || b["graded"] != 6 || b["correct"] != 2 {
		t.Fatalf("banner %v", b)
	}
	// Own record per tier shows from 30 graded.
	if own := s.ownRecords(); len(own) != 0 {
		t.Fatal("own record below 30")
	}
}

func TestWinJackpotFreezeReviewPlan(t *testing.T) {
	s, db, w := winSvc(t, time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	loadSQLFile(t, db, "testdata/free/seed_jackpot.sql")
	closes := time.Date(2026, 11, 20, 16, 45, 0, 0, time.UTC)
	games := []GameInput{}
	for _, g := range winJP20Nov {
		fid := g.Fix
		games = append(games, GameInput{Home: strings.Split(g.TeamsLabel, " v ")[0], Away: strings.Split(g.TeamsLabel, " v ")[1], Odds: g.Odds[:], FixtureID: &fid})
	}
	name, price, st := "Midweek", 99.0, "open"
	v, err := s.CreateJackpot("o1", "u1", JackpotInput{Name: &name, TicketPrice: &price, ClosesAt: &closes, Status: &st, Games: games, Bonuses: []BonusTier{{12, 250000}, {10, 2500}},
		Weights: map[string]float64{"jackpot": 1}})
	if err != nil {
		t.Fatal(err)
	}
	id := v.Jackpot.ID
	if v.Frozen != nil || v.Review != nil || v.Analysis.Games[0].Verdict == nil || v.Analysis.Games[0].Verdict.Tier.Key != "coinflip" || v.Analysis.Games[0].Verdict.Safer.Selection != "1X" {
		t.Fatalf("before close %+v %+v", v.Frozen, v.Analysis.Games[0].Verdict)
	}
	// Planner before close: current chances, within budget.
	if _, err := s.PlanJackpot("o1", "u1", id, 50); err == nil {
		t.Fatal("budget below one ticket accepted")
	}
	if _, err := s.PlanJackpot("o2", "u1", id, 5000); err != ErrNotFound {
		t.Fatal("another organisation's jackpot")
	}
	p, err := s.PlanJackpot("o1", "u1", id, 5000)
	if err != nil || p.Plan.Combinations > 50 || p.Plan.Cost > 5000 || p.MaxCombinations != 50 || !strings.HasPrefix(p.Basis, "current") || len(p.Plan.AtLeast) != 3 || p.Plan.Graded != nil {
		t.Fatalf("plan %v %+v", err, p)
	}
	if p.Plan.PAll <= p.Single["p_all"].(float64) || len(p.Ladder) == 0 {
		t.Fatal("covering should raise the chance")
	}
	prev := 0.0
	for _, b := range []float64{99, 198, 500, 1000, 5000, 20000, 100000} {
		q, _ := s.PlanJackpot("o1", "u1", id, b)
		if q.Plan.PAll < prev-1e-15 || q.Plan.Cost > b {
			t.Fatalf("budget %v: p %v < %v or cost %v", b, q.Plan.PAll, prev, q.Plan.Cost)
		}
		prev = q.Plan.PAll
	}
	// Freeze at close; later prices don't change the snapshot.
	s.SetClock(closes.Add(-3 * time.Minute))
	if n := s.FreezeJackpots(); n != 1 {
		t.Fatalf("frozen %d", n)
	}
	w.odds(9100101, ProviderOddsPapi, "pinnacle", "Pinnacle", [3]float64{1.5, 4.0, 6.0}, closes.Add(-time.Minute))
	db.Model(&Jackpot{}).Where("id = ?", id).Update("weights", mustJSON(map[string]float64{"jackpot": 0.2, "market": 0.8}))
	var j Jackpot
	db.Where("id = ?", id).First(&j)
	if j.Frozen == nil || strings.Join(j.Frozen.Picks, " ") != strings.Join(JP20NovTicket, " ") || j.Frozen.Source != "frozen at close" || j.Frozen.ExpectedCorrect != 5.16 {
		t.Fatalf("snapshot %+v", j.Frozen)
	}
	// Results and grading → review and per-game ✓/✗ from the snapshot.
	for _, g := range winJP20Nov {
		var f Fixture
		db.Where("id = ?", g.Fix).First(&f)
		w.result(g.Fix, "FT", ip(g.HG), ip(g.AG), f.Kickoff.Add(2*time.Hour))
	}
	s.SetClock(time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC))
	if n := s.GradeJackpots(); n != 1 {
		t.Fatalf("graded %d", n)
	}
	v, err = s.JackpotViewOf("o1", "u1", id, 0)
	if err != nil || v.Review == nil || v.Review.Correct != 4 || v.Review.SaferHits != 8 || v.Review.Expected != 5.16 || !v.Review.Complete || v.Frozen == nil {
		t.Fatalf("review %v %+v", err, v.Review)
	}
	if v.Games[2].ResultCheck == nil || v.Games[2].ResultCheck.Status != "wrong" || !*v.Games[2].ResultCheck.Safer.Correct || v.Games[0].ResultCheck.Status != "correct" {
		t.Fatalf("game checks %+v", v.Games[2].ResultCheck)
	}
	// List row and history carry the review numbers; the scorecard lists the jackpot.
	list, _ := s.ListJackpots("o1", "u1", "", 10, 0)
	if len(list) != 1 || list[0].Graded == nil || *list[0].Graded.ReviewCorrect != 4 || *list[0].Graded.SaferHits != 8 {
		t.Fatalf("list %+v", list[0].Graded)
	}
	nbo, _ := time.LoadLocation("Africa/Nairobi")
	sc, _ := s.ScorecardFor(ScoreQuery{Loc: nbo, Period: "30d", Org: "o1", User: "u1"})
	if len(sc.Jackpots) != 1 || sc.Jackpots[0].Correct != 4 || sc.Jackpots[0].Low != 3 || sc.Jackpots[0].High != 7 {
		t.Fatalf("scorecard jackpots %+v", sc.Jackpots)
	}
	// After grading the plan says how it would have done, on the frozen chances.
	p, _ = s.PlanJackpot("o1", "u1", id, 5000)
	if p.Plan.Graded == nil || p.Plan.Graded.Of != 13 || !strings.HasPrefix(p.Basis, "chances frozen") {
		t.Fatalf("graded plan %+v", p.Plan.Graded)
	}
	// A copy starts without the snapshot.
	cp, err := s.DuplicateJackpot("o1", "u1", id, "")
	if err != nil || cp.Frozen != nil || cp.Review != nil {
		t.Fatalf("duplicate %v", err)
	}
}

// fakeWin serves captured payloads for the three new sources.
type fakeWin struct {
	mu     sync.Mutex
	srv    *httptest.Server
	calls  map[string]int
	afKeys []string
}

func newFakeWin(t *testing.T) *fakeWin {
	f := &fakeWin{calls: map[string]int{}}
	files := map[string]string{"/sports": "pm_sports.json", "/events": "pm_events.json", "/2026-27/en.1.json": "of_en1.json", "/predictions": "af_predictions_1200141.json"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.URL.Path]++
		if r.URL.Path == "/predictions" {
			f.afKeys = append(f.afKeys, r.Header.Get("x-apisports-key"))
		}
		f.mu.Unlock()
		name, ok := files[r.URL.Path]
		if !ok || (r.URL.Path == "/events" && r.URL.Query().Get("tag_id") != "306") {
			http.NotFound(w, r)
			return
		}
		b, _ := os.ReadFile("testdata/win/" + name)
		if r.URL.Path == "/predictions" && r.URL.Query().Get("fixture") != "1200141" {
			b = []byte(`{"get":"predictions","parameters":{},"errors":[],"results":0,"paging":{"current":1,"total":1},"response":[]}`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWin) n(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

func winWorld(t *testing.T, s *Service, db *gorm.DB, f *fakeWin) {
	t.Helper()
	var fs []afFixtureEntry
	b, _ := os.ReadFile("testdata/win/af_rounds_14_15.json")
	var env struct {
		Response json.RawMessage `json:"response"`
	}
	json.Unmarshal(b, &env)
	json.Unmarshal(env.Response, &fs)
	if _, err := s.storeFixtures(fs, false); err != nil {
		t.Fatal(err)
	}
	db.Model(&Competition{}).Where("id = 39").Updates(map[string]any{"enabled": true, "name": "Premier League", "country": "England"})
	s.Free = freeBasesFromEnv()
	s.Free.Polymarket, s.Free.OpenFootball, s.Free.NoSpacing = f.srv.URL, f.srv.URL, true
	s.AFBase = f.srv.URL
}

func TestWinPolymarketProvider(t *testing.T) {
	s, db, _ := winSvc(t, time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC))
	f := newFakeWin(t)
	winWorld(t, s, db, f)
	// Off by default: nothing fetched.
	sum, err := s.RunFreeNow(context.Background(), []string{"polymarket"}, false)
	if err != nil || f.n("/sports")+f.n("/events") != 0 {
		t.Fatalf("ran while off: %v %+v", err, sum)
	}
	enableWin(t, s, ProviderPolymarket)
	sum, err = s.RunFreeNow(context.Background(), []string{"polymarket"}, false)
	if err != nil || f.n("/sports") != 1 || f.n("/events") != 1 {
		t.Fatalf("discovery %v %+v calls %v", err, sum, f.calls)
	}
	var sps []SourceProb
	db.Where("source = ?", ProviderPolymarket).Order("fixture_id").Find(&sps)
	got := map[int64]bool{}
	for _, sp := range sps {
		got[sp.FixtureID] = true
		if !wnear(sp.PHome+sp.PDraw+sp.PAway, 1, 0.0005) || !strings.HasPrefix(sp.URL, "https://polymarket.com/event/") || sp.Detail["slug"] == nil {
			t.Fatalf("source prob %+v", sp)
		}
	}
	// 1200141 (Polymarket spellings) matched; 1200145 thin and 1200143 wide spread are ignored.
	if !got[1200141] || got[1200145] || got[1200143] || !got[1200142] {
		t.Fatalf("stored for %v", got)
	}
	var ev PMEvent
	db.Where("id = '800145'").First(&ev)
	if ev.FixtureID != 1200145 || ev.Usable || !strings.HasPrefix(ev.Why, "thin market") {
		t.Fatalf("thin event row %+v", ev)
	}
	var unk PMEvent
	if db.Where("id = '999001'").First(&unk); unk.FixtureID != 0 {
		t.Fatal("unknown teams matched")
	}
	var al int64
	db.Model(&TeamAlias{}).Where("provider = ?", ProviderPolymarket).Count(&al)
	if al == 0 {
		t.Fatal("no aliases learned")
	}
	var lg int64
	db.Model(&FetchLog{}).Where("provider = ? AND ok", ProviderPolymarket).Count(&lg)
	if lg != 2 {
		t.Fatalf("fetch log %d", lg)
	}
	// The verdict of a matched fixture uses it.
	var fx Fixture
	db.Where("id = 1200141").First(&fx)
	v, _ := s.winForFixture(fx, s.marketView(fx, nil, ""), nil)
	hasPM := false
	for _, src := range v.Consensus.Sources {
		hasPM = hasPM || src.Key == "polymarket"
	}
	if v == nil || !hasPM || v.Tier.Note == "" {
		t.Fatalf("verdict sources %+v", v.Consensus)
	}
	// Hourly: a second run is not due; with force, the cached sports list is reused.
	s.RunFreeNow(context.Background(), []string{"polymarket"}, false)
	if f.n("/events") != 1 {
		t.Fatal("events fetched again within the hour")
	}
	s.RunFreeNow(context.Background(), []string{"polymarket"}, true)
	if f.n("/sports") != 1 || f.n("/events") != 2 {
		t.Fatalf("forced run %v", f.calls)
	}
	// Budget: 3 requests a day → the next one is skipped.
	s.SaveSources(SourcesPatch{Budgets: map[string]int{ProviderPolymarket: 3}}, "test")
	sum, _ = s.RunFreeNow(context.Background(), []string{"polymarket"}, true)
	if f.n("/events") != 2 || sum.Skipped == 0 {
		t.Fatalf("budget not applied %+v %v", sum, f.calls)
	}
	// Stricter liquidity makes every market thin: stored prices are removed.
	s.SaveSources(SourcesPatch{Budgets: map[string]int{ProviderPolymarket: 30}, WinPatch: WinPatch{PolyMinLiquidity: ptr(1e6)}}, "test")
	s.RunFreeNow(context.Background(), []string{"polymarket"}, true)
	var left int64
	db.Model(&SourceProb{}).Where("source = ?", ProviderPolymarket).Count(&left)
	if left != 0 {
		t.Fatalf("thin markets kept: %d", left)
	}
}

func TestWinOpenFootballProvider(t *testing.T) {
	s, db, _ := winSvc(t, time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC))
	f := newFakeWin(t)
	winWorld(t, s, db, f)
	enableWin(t, s, ProviderOpenFootball)
	s.SaveSources(SourcesPatch{WinPatch: WinPatch{OFFiles: ptr("en.1,de.1")}}, "test")
	sum, err := s.RunFreeNow(context.Background(), []string{"openfootball"}, false)
	if err != nil || f.n("/2026-27/en.1.json") != 1 || f.n("/2026-27/de.1.json") != 1 || sum.Failed != 0 {
		t.Fatalf("fetch %v %+v %v", err, sum, f.calls)
	}
	var n, linked int64
	db.Model(&OFMatch{}).Count(&n)
	db.Model(&OFMatch{}).Where("fixture_id <> 0").Count(&linked)
	if n != 380 || linked < 18 {
		t.Fatalf("matches %d linked %d", n, linked)
	}
	var m OFMatch
	db.Where("fixture_id = 1200141").First(&m)
	if m.Kickoff == nil || m.Kickoff.UTC().Format(time.RFC3339) != "2026-11-21T15:00:00Z" || m.HG != nil {
		t.Fatalf("1200141 %+v", m)
	}
	// Source check: openfootball's kickoff agrees (1200151 is 30 min off at the fake: minor).
	var fx Fixture
	db.Where("id = 1200141").First(&fx)
	sc := s.SourceCheckFor(fx)
	koOK := false
	for _, it := range sc.Items {
		if it.Key == "kickoff" {
			for _, v := range it.Values {
				koOK = koOK || v.Source == ProviderOpenFootball
			}
			if it.Status != "agree" {
				t.Fatalf("kickoff %+v", it)
			}
		}
	}
	if !koOK {
		t.Fatal("openfootball kickoff missing from the source check")
	}
	// Daily: not again today; the missing file was fine ("no file for this season yet").
	s.RunFreeNow(context.Background(), []string{"openfootball"}, false)
	if f.n("/2026-27/en.1.json") != 1 {
		t.Fatal("fetched twice a day")
	}
	var ts TaskState
	db.Where("key = 'of:de.1:2026-27'").First(&ts)
	if ts.Status != "ok" || !strings.Contains(ts.Note, "no file") {
		t.Fatalf("404 state %+v", ts)
	}
	// Bad settings are refused.
	if err := s.SaveSources(SourcesPatch{WinPatch: WinPatch{OFFiles: ptr("xx.1")}}, "t"); err == nil {
		t.Fatal("unknown file accepted")
	}
}

func TestWinAFPredProvider(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	s, db, _ := winSvc(t, now)
	f := newFakeWin(t)
	winWorld(t, s, db, f)
	enableWin(t, s, ProviderAFPred)
	// No API-Football key: stored predictions could be used, but nothing is fetched.
	s.RunFreeNow(context.Background(), []string{"apifootball_pred"}, false)
	if f.n("/predictions") != 0 {
		t.Fatal("fetched without a key")
	}
	if err := s.SaveSettings(SettingsPatch{APIFootballKey: ptr("af-secret-key-123"), APIFootballDaily: ptr(500)}, "test"); err != nil {
		t.Fatal(err)
	}
	s.SaveSources(SourcesPatch{Budgets: map[string]int{ProviderAFPred: 4}}, "test")
	// One fixture already has v2.16's reference prediction: reused, no request.
	db.Model(&Fixture{}).Where("id = 1200142").Updates(map[string]any{"prediction_ref": mustJSON(RefPrediction{Home: 0.5, Draw: 0.3, Away: 0.2, Advice: "x", FetchedAt: now.Add(-time.Hour)}), "pred_ref_at": now.Add(-time.Hour)})
	sum, err := s.RunFreeNow(context.Background(), []string{"apifootball_pred"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.n("/predictions") != 4 || f.afKeys[0] != "af-secret-key-123" || sum.Failed != 0 {
		t.Fatalf("requests %d (budget 4) %+v", f.n("/predictions"), sum)
	}
	// Fixtures API-Football has no prediction for (200, empty response) are done, not failed.
	var empty int64
	db.Model(&TaskState{}).Where("key LIKE 'ap:pred:%' AND status = 'ok' AND note = 'no prediction for this fixture'").Count(&empty)
	if empty != 3 {
		t.Fatalf("empty predictions %d", empty)
	}
	var sp SourceProb
	if db.Where("fixture_id = 1200141 AND source = ?", ProviderAFPred).First(&sp).Error != nil || !wnear(sp.PHome+sp.PDraw+sp.PAway, 1, 0.001) || sp.Detail["advice"] == "" {
		t.Fatalf("stored %+v", sp)
	}
	var reused SourceProb
	if db.Where("fixture_id = 1200142 AND source = ?", ProviderAFPred).First(&reused).Error != nil || reused.PHome != 0.5 {
		t.Fatal("reference not reused")
	}
	var fx Fixture
	db.Where("id = 1200141").First(&fx)
	if fx.PredictionRef == nil || fx.PredRefAt == nil {
		t.Fatal("reference copy not kept")
	}
	// Usage: counted for the source and inside API-Football's budget.
	if u := s.FreeUsage(ProviderAFPred); u.Used != 4 || u.Budget != 4 {
		t.Fatalf("usage %+v", u)
	}
	if q := s.quotaRow(ProviderAPIFootball, dayPeriod(s.Now())); q.Used != 4 {
		t.Fatalf("API-Football usage %d", q.Used)
	}
	// Refresh 3 h before kickoff (and not again).
	s.SaveSources(SourcesPatch{Budgets: map[string]int{ProviderAFPred: 100}}, "test")
	s.SetClock(fx.Kickoff.Add(-2 * time.Hour))
	before := f.n("/predictions")
	s.RunFreeNow(context.Background(), []string{"apifootball_pred"}, false)
	var sp2 SourceProb
	db.Where("fixture_id = 1200141 AND source = ?", ProviderAFPred).First(&sp2)
	if !sp2.FetchedAt.After(sp.FetchedAt) || f.n("/predictions") <= before {
		t.Fatal("no refresh at 3 h")
	}
	after := f.n("/predictions")
	s.RunFreeNow(context.Background(), []string{"apifootball_pred"}, false)
	if f.n("/predictions") != after {
		t.Fatalf("asked again: %d → %d", after, f.n("/predictions"))
	}
	// Switched off: the stored prediction is no longer used in the verdict.
	enableWin(t, s)
	s.SaveSources(SourcesPatch{Enabled: map[string]bool{ProviderAFPred: false}}, "test")
	v, in := s.liveVerdict(fx, s.compOf(39), s.marketView(fx, nil, ""), nil)
	if v != nil {
		for _, src := range v.Consensus.Sources {
			if src.Key == ProviderAFPred {
				t.Fatal("used while off")
			}
		}
	}
	if in.Why[ProviderAFPred] != "switched off" {
		t.Fatalf("why %q", in.Why[ProviderAFPred])
	}
}

func TestWinConsensusWeightsDB(t *testing.T) {
	s, db, _ := winSvc(t, time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC))
	rows := s.FitConsensusWeights()
	if len(rows) != 3 || rows[0].InUse != "default" {
		t.Fatalf("empty fit %+v", rows)
	}
	if w, from := s.consWeightsFor("top5"); from != "default" || w["sharp"] != 0.55 {
		t.Fatal("defaults")
	}
	// 1,200 graded verdicts where the sharp book knows the truth.
	for i, smp := range consSamples(1200, 0) {
		v := Verdict{Consensus: Consensus{Sources: []ConsSource{{Key: "sharp", Probs: probsMap(smp.Probs["sharp"])}, {Key: "model", Probs: probsMap(smp.Probs["model"])}}}}
		st := "wrong"
		if smp.Outcome == 0 {
			st = "correct"
		}
		db.Create(&VerdictRow{FixtureID: int64(8000000 + i), Kickoff: smp.Kickoff, Grp: "top5", Verdict: v, Status: st, Outcome: selKeys[smp.Outcome], GradedAt: &smp.Kickoff, Tier: "good"})
	}
	rows = s.FitConsensusWeights()
	if rows[0].Grp != "top5" || rows[0].InUse != "learned" || rows[0].N != 1200 || rows[0].HoldoutN != 240 || rows[1].InUse != "default" {
		t.Fatalf("fit %+v", rows)
	}
	if _, from := s.consWeightsFor("top5"); from != "learned" {
		t.Fatal("learned weights not used")
	}
	if s.FitConsensusDue() {
		t.Fatal("refit within a week")
	}
	view := s.ConsensusWeightsView()
	if len(view["groups"].([]map[string]any)) != 3 {
		t.Fatal("view")
	}
	// Own tier record (≥ 30 graded) appears on verdicts.
	resetOwnCache()
	if own := s.ownRecords(); own["good"] == nil || own["good"].N != 1200 {
		t.Fatalf("own %+v", own)
	}
}

func TestWinHandlers(t *testing.T) {
	s, db, w := winSvc(t, time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC))
	f := newFakeWin(t)
	winWorld(t, s, db, f)
	w.market(1200141, [3]float64{1.6, 4.0, 5.5}, s.Now().Add(-time.Hour), true)
	w.market(1200142, [3]float64{2.6, 3.2, 2.8}, s.Now().Add(-time.Hour), true)
	gin.SetMode(gin.TestMode)
	r := router(s)
	// Sources view: the three new sources, off by default, with the new blocks.
	code, out := do(r, "GET", "/api/v1/super-admin/football/sources", "o1", "su", "super_admin", nil)
	d := out["data"].(map[string]any)
	keys := map[string]bool{}
	for _, x := range d["sources"].([]any) {
		m := x.(map[string]any)
		keys[m["key"].(string)] = m["enabled"].(bool)
		if m["key"] == "polymarket" && (m["docs"] != docPolymarket || m["period"] != "day" || m["budget"].(float64) != 30) {
			t.Fatalf("polymarket view %v", m)
		}
	}
	if code != 200 || keys["polymarket"] || keys["openfootball"] || keys["apifootball_pred"] || d["polymarket"] == nil || d["openfootball"] == nil || d["not_used"] == nil || d["history"] == nil {
		t.Fatalf("sources %d %v", code, keys)
	}
	if code, _ := do(r, "PUT", "/api/v1/super-admin/football/sources", "o1", "su", "super_admin", map[string]any{"polymarket_max_spread": 0.9}); code != 400 {
		t.Fatal("bad spread accepted")
	}
	if code, _ := do(r, "PUT", "/api/v1/super-admin/football/sources", "o1", "su", "super_admin", map[string]any{"polymarket_leagues": "epl"}); code != 400 {
		t.Fatal("bad leagues accepted")
	}
	code, out = do(r, "PUT", "/api/v1/super-admin/football/sources", "o1", "su", "super_admin", map[string]any{"enabled": map[string]bool{"polymarket": true, "openfootball": true},
		"polymarket_min_liquidity": 8000, "polymarket_leagues": "epl:39,lal:140", "budgets": map[string]int{"polymarket": 20}})
	d = out["data"].(map[string]any)
	if code != 200 || d["polymarket"].(map[string]any)["min_liquidity"].(float64) != 8000 || len(d["polymarket"].(map[string]any)["leagues"].([]any)) != 2 {
		t.Fatalf("save %d %v", code, d["polymarket"])
	}
	if code, out := do(r, "POST", "/api/v1/super-admin/football/sources/test", "o1", "su", "super_admin", map[string]any{"provider": "openfootball"}); code != 200 || out["data"].(map[string]any)["ok"] != true {
		t.Fatalf("test openfootball %v", out)
	}
	if code, out := do(r, "POST", "/api/v1/super-admin/football/sources/test", "o1", "su", "super_admin", map[string]any{"provider": "polymarket"}); code != 200 || out["data"].(map[string]any)["ok"] != true {
		t.Fatalf("test polymarket %v", out)
	}
	if _, out := do(r, "POST", "/api/v1/super-admin/football/sources/test", "o1", "su", "super_admin", map[string]any{"provider": "apifootball_pred"}); out["data"].(map[string]any)["code"] != "no_key" {
		t.Fatalf("test predictions without key %v", out)
	}
	if code, _ := do(r, "GET", "/api/v1/super-admin/football/sources", "o1", "a1", "admin", nil); code != 403 {
		t.Fatal("admin read sources")
	}
	// Reads for everyone with the module.
	for _, p := range []string{"/api/v1/football/scorecard", "/api/v1/football/scorecard?period=7d", "/api/v1/football/picks?min_tier=coinflip", "/api/v1/football/history",
		"/api/v1/football/model/consensus-weights", "/api/v1/football/fixtures?min_tier=strong"} {
		if code, out := do(r, "GET", p, "o1", "u1", "staff", nil); code != 200 || out["success"] != true {
			t.Fatalf("%s %d %v", p, code, out)
		}
	}
	for _, p := range []string{"/api/v1/football/scorecard?period=year", "/api/v1/football/scorecard?tier=x", "/api/v1/football/picks?min_tier=x", "/api/v1/football/fixtures?min_tier=x"} {
		if code, _ := do(r, "GET", p, "o1", "u1", "staff", nil); code != 400 {
			t.Fatalf("%s accepted", p)
		}
	}
	_, out = do(r, "GET", "/api/v1/football/picks", "o1", "u1", "staff", nil)
	picks := out["data"].(map[string]any)["picks"].([]any)
	if len(picks) == 0 {
		t.Fatal("no picks")
	}
	last := 2.0
	for _, p := range picks {
		pv := p.(map[string]any)["verdict"].(map[string]any)["p"].(float64)
		if pv > last+1e-9 {
			t.Fatal("picks not strongest first")
		}
		last = pv
	}
	// Fitting weights is for super admins.
	if code, _ := do(r, "POST", "/api/v1/super-admin/football/consensus-weights/fit", "o1", "a1", "admin", nil); code != 403 {
		t.Fatal("admin fit")
	}
	if code, _ := do(r, "POST", "/api/v1/super-admin/football/consensus-weights/fit", "o1", "su", "super_admin", nil); code != 200 {
		t.Fatal("super admin fit")
	}
	// Plan: validation and not found.
	if code, _ := do(r, "POST", "/api/v1/football/jackpots/nope/plan", "o1", "u1", "staff", map[string]any{"budget": 1000}); code != 404 {
		t.Fatal("plan of a missing jackpot")
	}
	_ = fmt.Sprint
}
