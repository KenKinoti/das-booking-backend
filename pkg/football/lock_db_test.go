package football

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// DB tests of v2.19.1: picks locked early and refreshed until kickoff, the
// production situation (no market, no ClubElo, almost no history → no pick;
// no pass in the last 30 minutes → the earlier lock is graded), lock on view
// and its throttle, the tick endpoint (token, rate limit), the start-up
// catch-up and background self-heal passes, the data gate, the keyless
// defaults, the Results view and My bets.

// history adds n finished matches of team against filler teams in comp.
func history(t *testing.T, w seedW, base int64, team, comp int64, n int, start time.Time, hg, ag int) {
	t.Helper()
	for i := 0; i < n; i++ {
		opp := 98000 + base%1000*10 + int64(i)
		w.team(opp, "Filler "+tid(opp), "Nowhere")
		id := base + int64(i)
		w.fixture(id, comp, start.Add(time.Duration(i)*72*time.Hour), team, opp, "history")
		w.result(id, "FT", ip(hg), ip(ag), start.Add(time.Duration(i)*72*time.Hour+2*time.Hour))
	}
}

// near: equal within two seconds (the test clock keeps running).
func near2(a, b time.Time) bool { d := a.Sub(b); return d < 2*time.Second && d > -2*time.Second }

func row(t *testing.T, db *gorm.DB, fid int64) VerdictRow {
	t.Helper()
	r, ok := verdictRow(t, db, fid)
	if !ok {
		t.Fatalf("no lock for %d", fid)
	}
	return r
}

func rowsByID(rs []FixtureRow) map[int64]FixtureRow {
	m := map[int64]FixtureRow{}
	for _, r := range rs {
		m[r.ID] = r
	}
	return m
}

// The production situation of 24 Nov 2026, re-created.
func TestLockProductionSituation(t *testing.T) {
	ko := time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)
	s, db, w := winSvc(t, ko.Add(-50*time.Hour))
	w.team(9101, "Caspian Harbour FA", "Azerbaijan")
	w.team(9102, "Costa Brava CF", "Spain")
	w.team(9103, "Lisboa Verde", "Portugal")
	w.team(9104, "Mersey Reds", "England")
	past := ko.AddDate(0, -4, 0)
	history(t, w, 8100000, 9102, 140, 6, past, 2, 0) // a handful of stored results
	history(t, w, 8200000, 9103, 94, 16, past, 1, 1) // enough history
	history(t, w, 8300000, 9104, 39, 16, past, 2, 1)
	w.fixture(7100001, 2, ko, 9101, 9102, "League Stage - 5") // no market, ClubElo off, almost no history
	w.fixture(7100002, 2, ko, 9103, 9104, "League Stage - 5")
	w.market(7100002, [3]float64{3.1, 3.5, 2.3}, ko.Add(-51*time.Hour), true)
	ctx := context.Background()
	if _, err := s.RunOnce(ctx, RunOpts{Trigger: "tick", NoFetch: true}); err != nil {
		t.Fatal(err)
	}
	// No pick without real data — said plainly, not an unfitted default.
	nd := row(t, db, 7100001)
	if nd.Status != "no_data" || nd.Pick != "" || nd.Reason != "No prices and too little match history for these teams" || nd.GradedAt != nil || nd.Gate == nil || nd.Gate.OK {
		t.Fatalf("no-data lock %+v", nd)
	}
	pk := row(t, db, 7100002)
	if pk.Status != "" || pk.Pick != "2" || !near2(pk.FrozenAt, ko.Add(-50*time.Hour)) || pk.FirstLockedAt == nil || pk.Gate == nil || !pk.Gate.OK {
		t.Fatalf("pick lock %+v", pk)
	}
	var fr Prediction
	if db.Where("fixture_id = 7100002 AND kind = 'frozen'").Limit(1).Find(&fr).RowsAffected == 0 || fr.FrozenAt == nil {
		t.Fatal("the model's prediction was not stored early")
	}
	if db.Where("fixture_id = 7100001 AND kind = 'frozen'").Limit(1).Find(&Prediction{}).RowsAffected != 0 {
		t.Fatal("an unfitted default was stored as a prediction")
	}
	rows, _ := s.Fixtures(FixtureQuery{From: ptr(ko.Add(-time.Hour)), To: ptr(ko.Add(time.Hour))})
	m := rowsByID(rows)
	if r := m[7100001]; r.Verdict != nil || r.NoPick == nil || r.NoPick.Reason != nd.Reason || r.Model != nil || r.ModelNote == "" || r.Lock == nil || r.Lock.Status != "not_enough_data" {
		t.Fatalf("no-data row %+v %+v %+v", r.Verdict, r.NoPick, r.Model)
	}
	r2 := m[7100002]
	if r2.Verdict == nil || r2.Verdict.Kind != "frozen" || r2.Verdict.LockedLabel != "Locked 2 days 2 h before kick-off" || r2.Lock == nil || r2.Lock.Final ||
		*r2.Lock.BeforeKickoffMin != 50*60 || r2.Model == nil || r2.Verdict.Data == nil || !r2.Verdict.Data.OK {
		t.Fatalf("pick row %+v %+v", r2.Verdict, r2.Lock)
	}
	// A day later a new Pinnacle price → the lock is refreshed (first lock kept).
	s.SetClock(ko.Add(-26 * time.Hour))
	w.odds(7100002, ProviderOddsAPI, "pinnacle", "Pinnacle", [3]float64{2.9, 3.5, 2.45}, ko.Add(-26*time.Hour-time.Minute))
	if _, err := s.RunOnce(ctx, RunOpts{Trigger: "tick", NoFetch: true}); err != nil {
		t.Fatal(err)
	}
	pk2 := row(t, db, 7100002)
	if !near2(pk2.FrozenAt, ko.Add(-26*time.Hour)) || !pk2.FirstLockedAt.Equal(*pk.FirstLockedAt) || pk2.Refreshes != 1 || pk2.P == pk.P {
		t.Fatalf("refresh %+v", pk2)
	}
	// The instance sleeps: no pass in the 30 minutes before kickoff. After
	// full time the earlier lock is still there and gets graded.
	s.SetClock(ko.Add(3 * time.Hour))
	w.result(7100001, "FT", ip(0), ip(3), ko.Add(2*time.Hour))
	w.result(7100002, "FT", ip(1), ip(2), ko.Add(2*time.Hour))
	sum, err := s.RunOnce(ctx, RunOpts{Trigger: "tick", NoFetch: true})
	if err != nil || sum.VGraded != 1 {
		t.Fatalf("graded %v %+v", err, sum)
	}
	g := row(t, db, 7100002)
	if g.Status != "correct" || g.Score != "1-2" || g.GradedAt == nil || !g.FrozenAt.Equal(pk2.FrozenAt) || g.Check == nil {
		t.Fatalf("graded %+v", g)
	}
	if n := row(t, db, 7100001); n.Status != "no_data" || n.GradedAt != nil {
		t.Fatal("a no-data lock was graded")
	}
	rows, _ = s.Fixtures(FixtureQuery{From: ptr(ko.Add(-time.Hour)), To: ptr(ko.Add(time.Hour))})
	m = rowsByID(rows)
	if r := m[7100002]; r.ResultCheck == nil || r.ResultCheck.Status != "correct" || r.ResultCheck.LockedLabel != "Locked 1 day 2 h before kick-off" || !r.Lock.Final {
		t.Fatalf("graded row %+v %+v", r.ResultCheck, r.Lock)
	}
	if r := m[7100001]; r.ResultCheck == nil || r.ResultCheck.Status != "not_enough_data" || !strings.Contains(r.ResultCheck.Summary, "not enough data") || r.NoPick != nil {
		t.Fatalf("no-data finished row %+v", r.ResultCheck)
	}
	sc, err := s.ScorecardFor(ScoreQuery{Period: "custom", From: ptr(ko.Add(-24 * time.Hour)), Loc: time.UTC})
	if err != nil || sc.Totals.Graded != 1 || sc.Totals.Correct != 1 || sc.Totals.NoData != 1 || sc.Totals.NoPick != 0 {
		t.Fatalf("scorecard %v %+v", err, sc.Totals)
	}
	// Graded once: a later correction changes nothing.
	w.result(7100002, "FT", ip(2), ip(2), ko.Add(4*time.Hour))
	s.SetClock(ko.Add(6 * time.Hour))
	s.RunOnce(ctx, RunOpts{Trigger: "tick", NoFetch: true})
	if g2 := row(t, db, 7100002); g2.Status != "correct" || !g2.GradedAt.Equal(*g.GradedAt) {
		t.Fatal("regraded")
	}
	// The Results view and My bets show it.
	res, err := s.ResultsFor(ko.Format("2006-01-02"), 0, 0, time.UTC)
	if err != nil || res.Tally.Correct != 1 || res.Tally.NotEnoughData != 1 || !strings.HasPrefix(res.Tally.Line, "Yesterday: 1 ✓ · 0 ✗ · 1 no pick") {
		t.Fatalf("results %v %+v", err, res.Tally)
	}
	if len(res.Days) != 1 || res.Days[0].Rows[0].Pick == nil && res.Days[0].Rows[1].Pick == nil {
		t.Fatalf("results rows %+v", res.Days)
	}
	b := Bet{ID: "b-lock-1", OrganizationID: "o1", UserID: "u1", FixtureID: 7100002, CompetitionID: 2, Kickoff: ko, Market: Mkt1X2, Selection: SelHome, Odds: 3.1,
		Stake: 100, Bookmaker: "Betika", Status: BetLost, PlacedAt: ko.Add(-3 * time.Hour)}
	db.Create(&b)
	bv := s.view(b)
	if bv.Locked == nil || bv.Locked.Status != "correct" || bv.Locked.PickLabel != "Mersey Reds win" || bv.Locked.Score != "1-2" || bv.Locked.LockedLabel == "" {
		t.Fatalf("bet's locked pick %+v", bv.Locked)
	}
}

// The data gate with real rows: a model that knows the teams but no market
// → capped at Good; one bookmaker → no pick; a price typed in → a pick; the
// model's numbers hidden when it knows nothing.
func TestLockDataGateDB(t *testing.T) {
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	s, db, w := winSvc(t, now)
	for id, n := range map[int64]string{9201: "Highgate Wanderers", 9202: "Lowmoor Town", 9203: "Riverbend", 9204: "Stonebridge", 9205: "Eastfield", 9206: "Westvale"} {
		w.team(id, n, "England")
	}
	past := now.AddDate(0, -5, 0)
	history(t, w, 8400000, 9201, 40, 20, past, 3, 0) // wins everything
	history(t, w, 8500000, 9202, 40, 20, past, 0, 3) // loses everything
	ko := now.Add(30 * time.Hour)
	w.fixture(7200001, 39, ko, 9201, 9202, "R14")
	w.fixture(7200002, 39, ko, 9203, 9204, "R14")
	w.odds(7200002, ProviderAPIFootball, "bet365", "Bet365", [3]float64{2.1, 3.3, 3.4}, now.Add(-time.Hour))
	w.fixture(7200003, 39, ko, 9205, 9206, "R14")
	for i, sel := range []string{SelHome, SelDraw, SelAway} {
		db.Create(&ManualOdd{ID: "m-lock-" + sel, OrganizationID: "o1", UserID: "u1", FixtureID: 7200003, Bookmaker: "Betika", Market: Mkt1X2, Selection: sel,
			Price: []float64{1.95, 3.4, 3.9}[i], SeenAt: now.Add(-30 * time.Minute)})
	}
	s.PredictUpcoming()
	picks, noData := s.LockVerdicts()
	if picks != 2 || noData != 1 {
		t.Fatalf("locked %d picks, %d no data", picks, noData)
	}
	mo := row(t, db, 7200001).Verdict
	if mo.Pick != "1" || mo.Tier.Key != "good" || !mo.Tier.Capped || mo.Tier.Note != ModelOnlyNote || mo.P < 0.6 || mo.Consensus.MarketUsed || !mo.Data.ModelOnly {
		t.Fatalf("model-only %+v %+v", mo.Tier, mo.Data)
	}
	one := row(t, db, 7200002)
	if one.Status != "no_data" || !strings.HasPrefix(one.Reason, "Only one bookmaker's price") {
		t.Fatalf("one bookmaker %+v", one)
	}
	ty := row(t, db, 7200003).Verdict
	if ty.Pick != "1" || !ty.Consensus.MarketUsed || len(ty.Consensus.Sources) != 1 || !strings.HasPrefix(ty.Consensus.Sources[0].Label, "Typed-in price (Betika") ||
		ty.Tier.Capped || ty.Data.ModelOnly {
		t.Fatalf("typed %+v", ty.Consensus)
	}
	// The thin teams' model numbers are hidden (no more identical unfitted
	// 0.4573 / 0.2542 / 0.2885 on every row); the known teams' are shown.
	rows, _ := s.Fixtures(FixtureQuery{From: ptr(ko.Add(-time.Hour)), To: ptr(ko.Add(time.Hour))})
	m := rowsByID(rows)
	if m[7200002].Model != nil || m[7200003].Model != nil || m[7200001].Model == nil || m[7200001].ModelNote != "" || len(m[7200002].Edges) != 0 {
		t.Fatalf("model numbers %+v %+v %+v", m[7200001].Model, m[7200002].Model, m[7200003].Model)
	}
	a, err := s.Analysis(7200002, "o1", "u1")
	if err != nil || a.Verdict != nil || a.NoPick == nil || a.Model != nil || a.ModelData == nil || a.ModelData.HasData || len(a.Value) != 0 {
		t.Fatalf("analysis %v %+v %+v", err, a.NoPick, a.ModelData)
	}
	// Elo uses every stored result of a team across competitions.
	w.team(9207, "Northport", "England")
	history(t, w, 8600000, 9207, 39, 8, past, 1, 0)
	history(t, w, 8700000, 9207, 2, 8, past.Add(time.Hour), 1, 1)
	ResetModelCache()
	f := Fixture{ID: 1, CompetitionID: 39, HomeID: 9207, AwayID: 9201}
	if md := s.modelDataNow(f); md.EloHome != 16 || !md.EloOK {
		t.Fatalf("cross-competition Elo %+v", md)
	}
}

// Lock on view: any API path showing an upcoming match stores / refreshes
// its lock, at most once per fixture every 10 minutes; never after kickoff.
func TestLockOnView(t *testing.T) {
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	s, db, w := winSvc(t, now)
	w.team(9301, "Ashby", "England")
	w.team(9302, "Brook", "England")
	ko := now.Add(5 * time.Hour)
	for i, id := range []int64{7300001, 7300002, 7300003, 7300004} {
		w.fixture(id, 39, ko.Add(time.Duration(i)*time.Hour), 9301, 9302, "R14")
		w.market(id, [3]float64{1.9, 3.5, 4.2}, now.Add(-time.Hour), true)
	}
	r := router(s)
	day := "/api/v1/football/fixtures?from=" + ko.Add(-time.Minute).Format(time.RFC3339) + "&to=" + ko.Add(time.Minute).Format(time.RFC3339)
	if code, _ := do(r, "GET", day, "o1", "u1", "admin", nil); code != 200 {
		t.Fatal(code)
	}
	r1 := row(t, db, 7300001)
	if !near2(r1.FrozenAt, now) || r1.CheckedAt == nil || !near2(*r1.CheckedAt, now) {
		t.Fatalf("lock on view %+v", r1)
	}
	// A new price 1 minute later; a view at +5 min is throttled.
	w.odds(7300001, ProviderOddsAPI, "pinnacle", "Pinnacle", [3]float64{1.7, 3.8, 5.0}, now.Add(time.Minute))
	s.SetClock(now.Add(5 * time.Minute))
	do(r, "GET", day, "o1", "u1", "admin", nil)
	if r2 := row(t, db, 7300001); !r2.FrozenAt.Equal(r1.FrozenAt) || !r2.CheckedAt.Equal(*r1.CheckedAt) || r2.Refreshes != 0 {
		t.Fatalf("not throttled %+v", r2)
	}
	s.SetClock(now.Add(11 * time.Minute))
	_, out := do(r, "GET", day, "o1", "u1", "admin", nil)
	r3 := row(t, db, 7300001)
	if !near2(r3.FrozenAt, now.Add(11*time.Minute)) || r3.Refreshes != 1 || r3.P <= r1.P {
		t.Fatalf("not refreshed after the throttle %+v", r3)
	}
	fx := out["data"].(map[string]any)["fixtures"].([]any)[0].(map[string]any)
	if fx["lock"].(map[string]any)["label"] != "Locked 4 h 49 min before kick-off" || fx["verdict"].(map[string]any)["locked_at"] == nil {
		t.Fatalf("row lock %v", fx["lock"])
	}
	// The match page, the picks list and jackpot games lock too.
	do(r, "GET", "/api/v1/football/fixtures/7300002/analysis", "o1", "u1", "admin", nil)
	row(t, db, 7300002)
	do(r, "GET", "/api/v1/football/picks?to="+ko.Format("2006-01-02"), "o1", "u1", "admin", nil)
	row(t, db, 7300003)
	if n := s.LockFixtures([]int64{7300004}); n != 1 {
		t.Fatal("jackpot games not locked")
	}
	row(t, db, 7300004)
	// After kickoff nothing changes, whatever is viewed.
	s.SetClock(ko.Add(10 * time.Minute))
	db.Model(&Fixture{}).Where("id = 7300001").Update("status", "1H")
	w.odds(7300001, ProviderOddsAPI, "pinnacle", "Pinnacle", [3]float64{4.0, 3.0, 2.0}, ko.Add(5*time.Minute))
	do(r, "GET", day, "o1", "u1", "admin", nil)
	s.LockVerdicts()
	if r4 := row(t, db, 7300001); !r4.FrozenAt.Equal(r3.FrozenAt) || r4.P != r3.P || !r4.CheckedAt.Equal(*r3.CheckedAt) {
		t.Fatalf("changed after kickoff %+v", r4)
	}
}

func tickRouter(s *Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	(&Handler{S: s}).RegisterPublic(r.Group("/api/v1/public"))
	return r
}

func TestTickEndpoint(t *testing.T) {
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	s, db, w := winSvc(t, now)
	t.Setenv("FOOTBALL_TICK_TOKEN", "")
	w.team(9401, "Ashby", "England")
	w.team(9402, "Brook", "England")
	w.fixture(7400001, 39, now.Add(6*time.Hour), 9401, 9402, "R14")
	w.market(7400001, [3]float64{1.9, 3.5, 4.2}, now.Add(-time.Hour), true)
	enc, _ := mail.EncryptSecret("af-secret-key-12345678")
	db.Model(&Settings{}).Where("id = 1").Update("apifootball_key", enc)
	s.invalidateSettings()
	s.AFBase = "http://127.0.0.1:1" // nothing listens: the pass logs failures, no secret leaves
	r := tickRouter(s)
	call := func(method, path string, hdr map[string]string) (int, map[string]any, string) {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		jsonUnmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}
	code, out, body := call("GET", "/api/v1/public/football/tick", nil)
	d, _ := out["data"].(map[string]any)
	if code != 200 || d["ran"] != true || d["status"] != "ran" || d["pass"].(map[string]any)["locked"].(float64) != 1 || strings.Contains(body, "af-secret") {
		t.Fatalf("tick %d %s", code, body)
	}
	if v := row(t, db, 7400001); v.Pick == "" {
		t.Fatal("tick did not lock")
	}
	// Rate limit: at most one pass every 5 minutes (any trigger).
	s.SetClock(now.Add(4 * time.Minute))
	code, out, _ = call("POST", "/api/v1/public/football/tick", nil)
	d = out["data"].(map[string]any)
	if code != 200 || d["ran"] != false || d["status"] != "ran_recently" || d["pass"] == nil || d["next_allowed_at"] == nil {
		t.Fatalf("rate limit %d %v", code, d)
	}
	s.SetClock(now.Add(6 * time.Minute))
	if _, out, _ = call("POST", "/api/v1/public/football/tick", nil); out["data"].(map[string]any)["status"] != "ran" {
		t.Fatalf("second pass %v", out)
	}
	ls := s.lockState()
	if ls.LastTrigger != "tick" || ls.Ticks != 3 || ls.LastTickAt == nil {
		t.Fatalf("lock state %+v", ls)
	}
	// FOOTBALL_TICK_TOKEN: the header (or ?token=) must match.
	t.Setenv("FOOTBALL_TICK_TOKEN", "tick-s3cret")
	if code, _, _ := call("GET", "/api/v1/public/football/tick", nil); code != http.StatusUnauthorized {
		t.Fatalf("no token %d", code)
	}
	if code, _, _ := call("GET", "/api/v1/public/football/tick", map[string]string{"X-Tick-Token": "wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("wrong token %d", code)
	}
	s.SetClock(now.Add(20 * time.Minute))
	if code, out, _ := call("GET", "/api/v1/public/football/tick", map[string]string{"X-Tick-Token": "tick-s3cret"}); code != 200 || out["data"].(map[string]any)["status"] != "ran" {
		t.Fatalf("header token %d %v", code, out)
	}
	if code, out, _ := call("GET", "/api/v1/public/football/tick?token=tick-s3cret", nil); code != 200 || out["data"].(map[string]any)["status"] != "ran_recently" {
		t.Fatalf("query token %d %v", code, out)
	}
	// The scheduler switched off in the settings: the tick says so.
	db.Model(&Settings{}).Where("id = 1").Update("scheduler_on", false)
	s.invalidateSettings()
	s.SetClock(now.Add(40 * time.Minute))
	if _, out, _ := call("GET", "/api/v1/public/football/tick", map[string]string{"X-Tick-Token": "tick-s3cret"}); out["data"].(map[string]any)["status"] != "off" {
		t.Fatalf("off %v", out)
	}
}

func waitHeal(t *testing.T, s *Service) {
	t.Helper()
	for i := 0; i < 400 && s.rt().healing.Load(); i++ {
		time.Sleep(25 * time.Millisecond)
	}
	if s.rt().healing.Load() {
		t.Fatal("self-heal pass did not finish")
	}
}

func TestSelfHealAndCatchUp(t *testing.T) {
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	s, db, w := winSvc(t, now)
	t.Setenv("FOOTBALL_SCHEDULER", "")
	w.team(9501, "Ashby", "England")
	w.team(9502, "Brook", "England")
	w.fixture(7500001, 39, now.Add(6*time.Hour), 9501, 9502, "R14")
	w.market(7500001, [3]float64{1.9, 3.5, 4.2}, now.Add(-time.Hour), true)
	r := router(s)
	// Not armed (no StartScheduler: tests, tools) → never.
	do(r, "GET", "/api/v1/football/meta", "o1", "u1", "admin", nil)
	if s.rt().heals.Load() != 0 {
		t.Fatal("healed without the scheduler")
	}
	s.rt().armed.Store(true)
	old := now.Add(-2 * time.Hour)
	db.Model(&LockState{}).Where("id = 1").Updates(map[string]any{"last_pass_at": old, "last_trigger": "schedule"})
	s.rt().readAt.Store(0)
	// The last pass is 2 h old: a request starts one in the background.
	if code, _ := do(r, "GET", "/api/v1/football/meta", "o1", "u1", "admin", nil); code != 200 {
		t.Fatal(code)
	}
	waitHeal(t, s)
	if ls := s.lockState(); ls.LastTrigger != "selfheal" || ls.LastPassAt == nil || !ls.LastPassAt.After(old) || s.rt().heals.Load() != 1 {
		t.Fatalf("self-heal %+v", ls)
	}
	if v := row(t, db, 7500001); v.Pick == "" {
		t.Fatal("self-heal pass did not lock")
	}
	// Fresh pass → nothing more; single flight; FOOTBALL_SCHEDULER=off → never.
	do(r, "GET", "/api/v1/football/meta", "o1", "u1", "admin", nil)
	if s.rt().heals.Load() != 1 {
		t.Fatal("healed again after a fresh pass")
	}
	s.SetClock(now.Add(45 * time.Minute))
	s.rt().healing.Store(true)
	if s.maybeSelfHeal() {
		t.Fatal("second flight")
	}
	s.rt().healing.Store(false)
	t.Setenv("FOOTBALL_SCHEDULER", "off")
	if s.maybeSelfHeal() {
		t.Fatal("healed with the scheduler off")
	}
	t.Setenv("FOOTBALL_SCHEDULER", "")
	// Catch-up after start: skipped right after a pass, runs when the last is old.
	s.SetClock(now.Add(2 * time.Minute))
	if s.CatchUp(context.Background()) {
		t.Fatal("catch-up right after a pass")
	}
	s.SetClock(now.Add(3 * time.Hour))
	if !s.CatchUp(context.Background()) || s.lockState().LastTrigger != "catchup" {
		t.Fatalf("catch-up %+v", s.lockState())
	}
}

func TestKeylessDefaultsMigration(t *testing.T) {
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	s, db, _ := winSvc(t, now) // the test harness switched everything off (an existing install)
	reset := func() {
		db.Model(&LockState{}).Where("id = 1").Updates(map[string]any{"keyless_at": nil, "keyless_on": "[]", "chosen": "{}"})
		freeAllOff(db)
	}
	reset()
	// ClubElo was used before and is off now: that was a choice → stays off.
	db.Create(&FetchLog{Provider: ProviderClubElo, Endpoint: "/2026-10-01", Task: "ce:ratings", OK: true, At: now.AddDate(0, -1, 0)})
	db.Create(&FetchLog{Provider: ProviderFDCOUK, Endpoint: "/mmz4281/2627/E0.csv", Task: "test connection", OK: true, At: now.AddDate(0, -1, 0)})
	if err := applyKeylessDefaults(db, now); err != nil {
		t.Fatal(err)
	}
	en := s.FreeSettingsGet().Enabled
	ls := s.lockState()
	if en[ProviderClubElo] || !en[ProviderFDCOUK] || !en[ProviderOpenFootball] || en[ProviderOddsPapi] || en[ProviderPolymarket] || ls.KeylessAt == nil ||
		strings.Join(ls.KeylessOn, ",") != "fdcouk,openfootball" {
		t.Fatalf("defaults %v %+v", en, ls)
	}
	// Once only: switching openfootball off afterwards is respected.
	if err := s.SaveSources(SourcesPatch{Enabled: map[string]bool{ProviderOpenFootball: false, ProviderFDCOUK: true}}, "su"); err != nil {
		t.Fatal(err)
	}
	applyKeylessDefaults(db, now.Add(time.Hour))
	if s.FreeSettingsGet().Enabled[ProviderOpenFootball] {
		t.Fatal("an explicit choice was overridden")
	}
	if ch := s.lockState().Chosen; ch[ProviderOpenFootball] || len(ch) != 1 {
		t.Fatalf("choices %v (only changes count)", ch)
	}
	// Even if the defaults ran again, a recorded choice wins.
	db.Model(&LockState{}).Where("id = 1").Update("keyless_at", nil)
	applyKeylessDefaults(db, now.Add(2*time.Hour))
	if en := s.FreeSettingsGet().Enabled; en[ProviderOpenFootball] || !en[ProviderFDCOUK] {
		t.Fatalf("after a rerun %v", en)
	}
	// A brand-new install: the keyless sources start on.
	if d := defaultFreeSettings(); !d.Enabled[ProviderClubElo] || !d.Enabled[ProviderFDCOUK] || !d.Enabled[ProviderOpenFootball] || d.Enabled[ProviderOddsPapi] {
		t.Fatalf("new install %v", d.Enabled)
	}
	r := router(s)
	_, out := do(r, "GET", "/api/v1/super-admin/football/sources", "o1", "su", "super_admin", nil)
	if k := out["data"].(map[string]any)["keyless"].(map[string]any); len(k["defaults"].([]any)) != 3 || !strings.Contains(k["note"].(string), "no key") {
		t.Fatalf("keyless view %v", k)
	}
}

func TestResultsView(t *testing.T) {
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	s, db, w := winSvc(t, now.Add(-30*time.Hour))
	w.team(9601, "Ashby", "England")
	w.team(9602, "Brook", "England")
	ko := now.Add(-20 * time.Hour) // yesterday 13:00 UTC
	for i, o := range [][3]float64{{1.6, 4.0, 5.5}, {1.7, 3.8, 5.0}, {2.0, 3.4, 3.9}} {
		id := int64(7600001 + i)
		w.fixture(id, 39, ko, 9601, 9602, "R13")
		w.market(id, o, now.Add(-31*time.Hour), true)
	}
	w.fixture(7600004, 39, ko, 9602, 9601, "R13") // no data → "not enough data"
	s.LockVerdicts()
	w.fixture(7600005, 140, ko, 9602, 9601, "R13") // created after the pass → never locked
	s.SetClock(now)
	w.result(7600001, "FT", ip(2), ip(0), ko.Add(2*time.Hour))
	w.result(7600002, "FT", ip(0), ip(1), ko.Add(2*time.Hour))
	w.result(7600004, "FT", ip(1), ip(1), ko.Add(2*time.Hour))
	w.result(7600005, "FT", ip(3), ip(0), ko.Add(2*time.Hour))
	// 7600003: still "NS" on 24 Nov (the result was never fetched) → waiting.
	s.GradeVerdicts()
	res, err := s.ResultsFor("", 7, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	ta := res.Tally
	if ta.Correct != 1 || ta.Wrong != 1 || ta.NotEnoughData != 1 || ta.NoPick != 1 || ta.Pending != 1 ||
		ta.Line != "Last 7 days: 1 ✓ · 1 ✗ · 2 no pick · 1 waiting for the result" || res.Scorecard != "/football/scorecard?period=7d" {
		t.Fatalf("tally %+v", ta)
	}
	if len(res.Days) != 1 || res.Days[0].Label != "Yesterday" || res.Days[0].Tally.Line != "Yesterday: 1 ✓ · 1 ✗ · 2 no pick · 1 waiting for the result" {
		t.Fatalf("days %+v", res.Days)
	}
	marks := map[int64]string{}
	for _, rr := range res.Days[0].Rows {
		marks[rr.FixtureID] = rr.Mark
	}
	if marks[7600001] != "correct" || marks[7600002] != "wrong" || marks[7600003] != "pending" || marks[7600004] != "not_enough_data" || marks[7600005] != "no_pick" {
		t.Fatalf("marks %v", marks)
	}
	r := router(s)
	if code, out := do(r, "GET", "/api/v1/football/results?date="+ko.Format("2006-01-02"), "o1", "u1", "admin", nil); code != 200 ||
		!strings.HasPrefix(out["data"].(map[string]any)["tally"].(map[string]any)["line"].(string), "Yesterday: 1 ✓") {
		t.Fatalf("results API %d %v", code, out)
	}
	if code, _ := do(r, "GET", "/api/v1/football/results?days=30", "o1", "u1", "admin", nil); code != 400 {
		t.Fatal("days=30 accepted")
	}
	_ = db
}
