package football

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- helpers

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func envOf(t *testing.T, name string, v any) afEnvelope {
	t.Helper()
	var env afEnvelope
	if err := json.Unmarshal(load(t, name), &env); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if v != nil {
		if err := json.Unmarshal(env.Response, v); err != nil {
			t.Fatalf("%s response: %v", name, err)
		}
	}
	return env
}

func fp(v float64) *float64 { return &v }

func approx(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

// ---------------------------------------------------------------- provider parsing

// API-Football samples (testdata, recorded from the documented shapes):
// envelope, errors as [] or {key: message}, statistics values as numbers,
// "55%", "1.84" (expected_goals) or null, embedded events / line-ups /
// statistics / players with fixtures?ids=.
func TestParseAPIFootballFixtures(t *testing.T) {
	var fs []afFixtureEntry
	env := envOf(t, "af_fixtures_ids_ft.json", &fs)
	if env.Get != "fixtures" || env.Results != 2 || len(afErrors(env.Errors)) != 0 || len(fs) != 2 {
		t.Fatalf("envelope %+v", env)
	}
	now := time.Date(2026, 11, 21, 17, 0, 0, 0, time.UTC)
	f := fixtureRow(fs[0], now)
	if f.ID != 1200141 || f.CompetitionID != 39 || f.Season != 2026 || f.Status != "FT" || !IsFinal(f.Status) || f.FTHome == nil || f.HomeGoals == nil ||
		*f.FTHome != *f.HomeGoals || f.Kickoff != time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC) || f.Round != "Regular Season - 15" || f.Venue == "" {
		t.Fatalf("fixture %+v", f)
	}
	if len(fs[0].Events) == 0 || len(fs[0].Lineups) != 2 || len(fs[0].Statistics) != 2 || len(fs[0].Players) != 2 {
		t.Fatalf("embedded: %d events %d lineups %d stats %d players", len(fs[0].Events), len(fs[0].Lineups), len(fs[0].Statistics), len(fs[0].Players))
	}
	st := statRow(f.ID, fs[0].Statistics[0], now)
	if st.XG == nil || *st.XG <= 0 || st.Possession == nil || *st.Possession < 20 || st.ShotsTotal == nil || st.PassPct == nil || *st.PassPct != 82 {
		t.Fatalf("stats %+v", st)
	}
	if len(fs[0].Lineups[0].StartXI) != 11 || fs[0].Lineups[0].Formation == nil {
		t.Fatalf("lineup %+v", fs[0].Lineups[0])
	}
	// Live sample: in play, elapsed, no full-time score yet.
	var live []afFixtureEntry
	envOf(t, "af_fixtures_ids_live.json", &live)
	l := fixtureRow(live[0], now)
	if !IsLive(l.Status) || l.Elapsed == nil || *l.Elapsed != 32 || l.FTHome != nil || l.HomeGoals == nil {
		t.Fatalf("live %+v", l)
	}
	// Documented edge cases: nulls, AET, own goal without a player, null xG.
	var doc []afFixtureEntry
	envOf(t, "af_doc_fixture.json", &doc)
	d := fixtureRow(doc[0], now)
	if d.Status != "AET" || *d.FTHome != 1 || *d.FTAway != 1 || *d.HomeGoals != 2 || *d.ETHome != 1 || d.Referee != "" || d.City != "" {
		t.Fatalf("doc fixture %+v", d)
	}
	ds := statRow(d.ID, doc[0].Statistics[1], now)
	if ds.XG != nil || ds.Possession == nil || *ds.Possession != 42 {
		t.Fatalf("null xG must stay nil: %+v", ds)
	}
	if doc[0].Events[0].Player.ID != nil || doc[0].Events[1].Time.Extra == nil || *doc[0].Events[1].Time.Extra != 3 {
		t.Fatalf("events %+v", doc[0].Events)
	}
	if ratingOf(doc[0].Players[0].Players[1].Statistics[0].Games.Rating) != nil || *ratingOf(doc[0].Players[0].Players[0].Statistics[0].Games.Rating) != 7.8 {
		t.Fatal("ratings")
	}
}

func TestParseAPIFootballOthers(t *testing.T) {
	var ls []afLeagueEntry
	envOf(t, "af_leagues.json", &ls)
	var epl afLeagueEntry
	for _, l := range ls {
		if l.League.ID == 39 {
			epl = l
		}
	}
	if epl.League.Name != "Premier League" || epl.Country.Name != "England" || len(epl.Seasons) != 2 || !epl.Seasons[1].Current || !epl.Seasons[1].Coverage.Odds ||
		!epl.Seasons[1].Coverage.Fixtures.StatisticsFixture {
		t.Fatalf("league %+v", epl)
	}
	var stds []afStandingsEntry
	envOf(t, "af_standings.json", &stds)
	rows := stds[0].League.Standings[0]
	if len(rows) != 20 || rows[0].Rank != 1 || rows[0].Points < rows[19].Points || rows[0].All.Played != 14 || rows[0].Form == nil || len(*rows[0].Form) != 5 {
		t.Fatalf("standings %+v", rows[0])
	}
	var inj []afInjury
	envOf(t, "af_injuries.json", &inj)
	statuses := map[string]int{}
	for _, i := range inj {
		statuses[injuryStatus(i.Player.Type, strp(i.Player.Reason))]++
	}
	if statuses["injured"] == 0 || statuses["suspended"] == 0 || statuses["doubtful"] == 0 {
		t.Fatalf("injury statuses %v", statuses)
	}
	var ps []afPlayerEntry
	env := envOf(t, "af_players_p1.json", &ps)
	if env.Paging.Total != 2 || env.Paging.Current != 1 || len(ps) != 20 || ps[0].Statistics[0].League.ID != 39 {
		t.Fatalf("players paging %+v (%d)", env.Paging, len(ps))
	}
	var odds []afOddsEntry
	envOf(t, "af_odds.json", &odds)
	rowsO := oddsRowsFromAF(odds[0], time.Now())
	markets := map[string]int{}
	for _, r := range rowsO {
		markets[r.Market]++
		if r.Price <= 1 || r.Source != ProviderAPIFootball {
			t.Fatalf("odd %+v", r)
		}
	}
	// 4 bookmakers × (3 + 2 + 2 + 6 + 2 + 3); "Exact Score" is ignored.
	if markets[Mkt1X2] != 12 || markets[MktDNB] != 8 || markets[MktAH] != 8 || markets[MktOU] != 24 || markets[MktBTTS] != 8 || markets[MktDC] != 12 {
		t.Fatalf("markets %v", markets)
	}
	var lo []afLiveOdds
	envOf(t, "af_odds_live.json", &lo)
	if len(lo) == 0 || len(lo[0].Odds) != 2 || lo[0].Odds[1].Values[0].Handicap == nil {
		t.Fatalf("live odds %+v", lo)
	}
	r := Odd{}
	if !afSelection("goals over/under", "Over", lo[0].Odds[1].Values[0].Handicap, &r) || r.Market != MktOU || r.Line == nil {
		t.Fatal("live over/under with the line in handicap")
	}
	var pr []afPrediction
	envOf(t, "af_predictions.json", &pr)
	if p := pr[0].Predictions; parsePct(p.Percent.Home)+parsePct(p.Percent.Draw)+parsePct(p.Percent.Away) < 0.99 || p.Advice == nil {
		t.Fatalf("prediction %+v", p)
	}
	var st afStatus
	envOf(t, "af_status.json", &st)
	if st.Subscription.Plan != "Pro" || st.Requests.LimitDay != 7500 || !st.Subscription.Active {
		t.Fatalf("status %+v", st)
	}
	// Error envelopes.
	for name, key := range map[string]string{"af_error_token.json": "token", "af_error_requests.json": "requests", "af_error_plan.json": "plan"} {
		env := envOf(t, name, nil)
		if afErrors(env.Errors)[key] == "" {
			t.Fatalf("%s: %v", name, afErrors(env.Errors))
		}
	}
}

func TestParseOddsAPI(t *testing.T) {
	var sports []OASport
	if err := json.Unmarshal(load(t, "oa_sports.json"), &sports); err != nil || len(sports) < 5 || sports[0].Key != "soccer_epl" {
		t.Fatalf("sports %v %v", err, sports)
	}
	var evs []OAEvent
	if err := json.Unmarshal(load(t, "oa_odds_epl.json"), &evs); err != nil || len(evs) < 5 {
		t.Fatal(err)
	}
	rows := oddsRowsFromOA(1200141, evs[0], time.Now())
	books := map[string]bool{}
	per := map[string]int{}
	for _, r := range rows {
		books[r.Bookmaker] = true
		per[r.Market+":"+r.Selection]++
		if r.ProviderUpdatedAt == nil {
			t.Fatal("bookmaker update time")
		}
	}
	// uk + eu: William Hill, Betway, Paddy Power, Pinnacle (eu only), 1xBet (onexbet), Betsson.
	if len(books) != 6 || !books["pinnacle"] || !books["1xbet"] || !books["betway"] {
		t.Fatalf("books %v", books)
	}
	if per["1x2:home"] != 6 || per["1x2:draw"] != 6 || per["ou:over"] != 6 || per["ah:home"] != 6 || per["ah:away"] != 6 {
		t.Fatalf("selections %v", per)
	}
	for _, r := range rows {
		if r.Market == MktAH && r.Selection == SelAway && r.Line != nil {
			// The away point is the opposite of the home point.
			for _, h := range rows {
				if h.Market == MktAH && h.Selection == SelHome && h.Bookmaker == r.Bookmaker && *h.Line != -*r.Line {
					t.Fatalf("spreads %v %v", *h.Line, *r.Line)
				}
			}
		}
	}
	var e struct {
		ErrorCode string `json:"error_code"`
	}
	if json.Unmarshal(load(t, "oa_error_credits.json"), &e); e.ErrorCode != "OUT_OF_USAGE_CREDITS" {
		t.Fatal(e)
	}
}

// ---------------------------------------------------------------- settlement

func TestSettleEveryMarket(t *testing.T) {
	type c struct {
		m, sel string
		line   *float64
		hg, ag int
		want   string
	}
	cases := []c{
		{Mkt1X2, SelHome, nil, 2, 1, BetWon}, {Mkt1X2, SelHome, nil, 1, 1, BetLost}, {Mkt1X2, SelDraw, nil, 0, 0, BetWon}, {Mkt1X2, SelAway, nil, 0, 3, BetWon},
		{MktDC, Sel1X, nil, 1, 1, BetWon}, {MktDC, Sel1X, nil, 0, 1, BetLost}, {MktDC, Sel12, nil, 1, 1, BetLost}, {MktDC, Sel12, nil, 0, 2, BetWon}, {MktDC, SelX2, nil, 2, 2, BetWon},
		{MktDNB, SelHome, nil, 1, 1, BetVoid}, {MktDNB, SelHome, nil, 2, 0, BetWon}, {MktDNB, SelAway, nil, 2, 0, BetLost},
		{MktBTTS, SelYes, nil, 1, 1, BetWon}, {MktBTTS, SelYes, nil, 2, 0, BetLost}, {MktBTTS, SelNo, nil, 0, 0, BetWon}, {MktBTTS, SelNo, nil, 3, 1, BetLost},
		// Totals: half, whole and quarter lines.
		{MktOU, SelOver, fp(2.5), 2, 1, BetWon}, {MktOU, SelOver, fp(2.5), 1, 1, BetLost}, {MktOU, SelUnder, fp(2.5), 1, 1, BetWon},
		{MktOU, SelOver, fp(3), 2, 1, BetVoid}, {MktOU, SelUnder, fp(3), 1, 1, BetWon}, {MktOU, SelUnder, fp(3), 2, 2, BetLost},
		{MktOU, SelOver, fp(2.25), 1, 1, BetHalfLost}, {MktOU, SelOver, fp(2.25), 2, 1, BetWon}, {MktOU, SelUnder, fp(2.25), 1, 1, BetHalfWon},
		{MktOU, SelOver, fp(2.75), 2, 1, BetHalfWon}, {MktOU, SelUnder, fp(2.75), 2, 1, BetHalfLost}, {MktOU, SelUnder, fp(2.75), 1, 1, BetWon},
		// Asian handicap: the line is the selected team's.
		{MktAH, SelHome, fp(-0.5), 1, 0, BetWon}, {MktAH, SelHome, fp(-0.5), 1, 1, BetLost}, {MktAH, SelAway, fp(0.5), 1, 1, BetWon},
		{MktAH, SelHome, fp(0), 1, 1, BetVoid}, {MktAH, SelHome, fp(-1), 2, 1, BetVoid}, {MktAH, SelHome, fp(-1), 3, 1, BetWon},
		{MktAH, SelHome, fp(-0.25), 1, 1, BetHalfLost}, {MktAH, SelHome, fp(-0.25), 2, 1, BetWon}, {MktAH, SelHome, fp(0.25), 1, 1, BetHalfWon},
		{MktAH, SelHome, fp(-0.75), 1, 0, BetHalfWon}, {MktAH, SelHome, fp(-0.75), 2, 0, BetWon}, {MktAH, SelHome, fp(-0.75), 1, 1, BetLost},
		{MktAH, SelAway, fp(0.75), 1, 0, BetHalfLost}, {MktAH, SelAway, fp(-1.25), 0, 1, BetHalfLost}, {MktAH, SelAway, fp(-1.25), 0, 2, BetWon},
		{MktAH, SelAway, fp(1.25), 2, 1, BetHalfWon}, {MktAH, SelHome, fp(1.5), 0, 1, BetWon}, {MktAH, SelHome, fp(1.5), 0, 2, BetLost},
	}
	for _, x := range cases {
		if got := Settle(x.m, x.sel, x.line, x.hg, x.ag); got != x.want {
			l := ""
			if x.line != nil {
				l = lineKey(x.line)
			}
			t.Errorf("%s %s %s at %d-%d: %s, want %s", x.m, x.sel, l, x.hg, x.ag, got, x.want)
		}
	}
	for _, x := range []struct {
		st       string
		ret, pnl float64
	}{{BetWon, 210, 110}, {BetLost, 0, -100}, {BetVoid, 100, 0}, {BetHalfWon, 155, 55}, {BetHalfLost, 50, -50}} {
		ret, pnl := Payout(x.st, 2.1, 100)
		if ret != x.ret || pnl != x.pnl {
			t.Errorf("%s: %v %v", x.st, ret, pnl)
		}
	}
}

// Probabilities integrated over a score matrix agree with direct sums.
func TestSelectionProb(t *testing.T) {
	m := [][]float64{{0.1, 0.1, 0.05}, {0.15, 0.1, 0.05}, {0.2, 0.15, 0.1}}
	W, L, V := SelectionProb(m, Mkt1X2, SelHome, nil)
	if !approx(W, 0.5, 1e-9) || !approx(L, 0.5, 1e-9) || V != 0 {
		t.Fatal(W, L, V)
	}
	W, L, V = SelectionProb(m, MktAH, SelHome, fp(0))
	if !approx(W, 0.5, 1e-9) || !approx(V, 0.3, 1e-9) || !approx(L, 0.2, 1e-9) {
		t.Fatal(W, L, V)
	}
	W, L, V = SelectionProb(m, MktAH, SelHome, fp(-0.25)) // draws lose half
	if !approx(W, 0.5, 1e-9) || !approx(L, 0.35, 1e-9) || !approx(V, 0.15, 1e-9) || !approx(EffP(W, L), 0.5/0.85, 1e-9) {
		t.Fatal(W, L, V)
	}
	if !approx(EVOf(0.5, 0.5, 2.2), 0.1, 1e-9) {
		t.Fatal("ev")
	}
}

// ---------------------------------------------------------------- planner

func TestPlannerPriorities(t *testing.T) {
	now := time.Date(2026, 11, 21, 14, 50, 0, 0, time.UTC)
	st := Settings{APIFootballDaily: 400, OddsAPIMonthly: 300, OddsRegions: "uk,eu", OddsMarkets: "h2h,totals,spreads", OddsSnapshots: "1440,360,60,10",
		LivePolling: true, LiveIntervalSec: 60, LookaheadDays: 3, APIFootballPlan: "pro"}
	cov := Coverage{Events: true, Lineups: true, StatisticsFixture: true, Standings: true, Players: true, TopScorers: true, Injuries: true, Predictions: true, Odds: true}
	in := PlanInput{Now: now, Settings: st, HasAF: true, HasOA: true, State: map[string]TaskState{}, StandingsAt: map[int64]time.Time{}, LastFinished: map[int64]time.Time{},
		PlayersAt: map[int64]time.Time{},
		Comps:     []PlanComp{{ID: 39, Season: 2026, SportKey: "soccer_epl", Type: "League", Coverage: cov, Known: true}},
		Fixtures: []PlanFixture{
			{ID: 1, Comp: 39, Season: 2026, Kickoff: now.Add(10 * time.Minute), Status: "NS", HomeID: 1, AwayID: 2},
			{ID: 2, Comp: 39, Season: 2026, Kickoff: now.Add(-30 * time.Minute), Status: "1H", HomeID: 3, AwayID: 4},
			{ID: 3, Comp: 39, Season: 2026, Kickoff: now.Add(-6 * time.Hour), Status: "2H", HomeID: 5, AwayID: 6},
			{ID: 4, Comp: 39, Season: 2026, Kickoff: now.Add(30 * time.Hour), Status: "NS", HomeID: 7, AwayID: 8},
		}}
	ok := func(st TaskState) TaskState {
		t := now.Add(-30 * time.Second)
		st.LastRunAt, st.LastOKAt, st.Status = t, &t, "ok"
		return st
	}
	tasks := Plan(in)
	byKind := map[string]Task{}
	for _, tk := range tasks {
		if _, seen := byKind[tk.Kind]; !seen {
			byKind[tk.Kind] = tk
		}
	}
	if tasks[0].Prio != PrioCritical {
		t.Fatalf("first task %+v", tasks[0])
	}
	if l := byKind[TLive]; l.Prio != PrioCritical || l.Params.Get("ids") != "2" {
		t.Fatalf("live %+v", l)
	}
	if r := byKind[TResults]; r.Prio != PrioCritical || r.Params.Get("ids") != "3" {
		t.Fatalf("overdue result %+v", r)
	}
	if o := byKind[TOddsOA]; o.Prio != PrioCritical || o.Cost != 6 || !strings.Contains(o.Reason, "T-10min") || o.Params.Get("regions") != "uk,eu" {
		t.Fatalf("closing odds snapshot %+v", o)
	}
	if l := byKind[TPrematch]; l.Prio != PrioHigh || l.Params.Get("ids") != "1" {
		t.Fatalf("line-ups %+v", l)
	}
	if _, ok := byKind[TOddsLive]; !ok {
		t.Fatal("in-play odds on a paid plan")
	}
	for i := 1; i < len(tasks); i++ {
		if tasks[i].Prio < tasks[i-1].Prio {
			t.Fatal("not sorted by priority")
		}
	}
	// Intervals: a task that just ran is not due; a failing one retries sooner.
	in.State = map[string]TaskState{"af:live:0": ok(TaskState{}), "af:leagues": ok(TaskState{})}
	for _, tk := range Plan(in) {
		if tk.Key == "af:live:0" || tk.Key == "af:leagues" {
			t.Fatalf("%s should not be due", tk.Key)
		}
	}
	in.State = map[string]TaskState{"af:leagues": {Key: "af:leagues", LastRunAt: now.Add(-11 * time.Minute), Status: "failed", Fails: 1}}
	found := false
	for _, tk := range Plan(in) {
		found = found || tk.Key == "af:leagues"
	}
	if !found {
		t.Fatal("failed task retried after 10 minutes")
	}
	// The free plan doesn't poll in-play odds; Only limits kinds.
	in.Settings.APIFootballPlan = "free"
	in.Only = map[string]bool{TLive: true, TOddsLive: true}
	ts := Plan(in)
	if len(ts) != 1 || ts[0].Kind != TLive {
		t.Fatalf("only live: %+v", ts)
	}
	// No snapshot before T-24 h; T-6 h is normal priority; a snapshot inside the window isn't repeated.
	in.Only = map[string]bool{TOddsOA: true}
	in.Fixtures = []PlanFixture{{ID: 9, Comp: 39, Kickoff: now.Add(30 * time.Hour), Status: "NS"}}
	if len(Plan(in)) != 0 {
		t.Fatal("snapshot before T-24h")
	}
	in.Fixtures = []PlanFixture{{ID: 9, Comp: 39, Kickoff: now.Add(5 * time.Hour), Status: "NS"}}
	ts = Plan(in)
	if len(ts) != 1 || ts[0].Prio != PrioNormal || !strings.Contains(ts[0].Reason, "T-6h") {
		t.Fatalf("T-6h %+v", ts)
	}
	got := now.Add(-30 * time.Minute)
	in.Fixtures[0].OddsOAAt = &got
	if len(Plan(in)) != 0 {
		t.Fatal("snapshot repeated inside its window")
	}
}

func TestLiveIntervalStretchesWithBudget(t *testing.T) {
	now := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	in := PlanInput{Now: now, Settings: Settings{APIFootballDaily: 7000, LiveIntervalSec: 60}}
	for i := 0; i < 3; i++ {
		in.Fixtures = append(in.Fixtures, PlanFixture{ID: int64(i), Kickoff: now.Add(time.Duration(i) * 30 * time.Minute), Status: "NS"})
	}
	if LiveInterval(in) != time.Minute {
		t.Fatalf("plenty of budget: %v", LiveInterval(in))
	}
	in.Settings.APIFootballDaily = 100 // free plan: 50 live requests for ~175 live minutes
	if iv := LiveInterval(in); iv < 3*time.Minute || iv > 4*time.Minute {
		t.Fatalf("stretched interval %v", iv)
	}
	in.LiveUsedToday = 50
	if LiveInterval(in) < 12*time.Hour {
		t.Fatal("live budget used up")
	}
}

// ---------------------------------------------------------------- matching

func TestNameSimilarityAndMatching(t *testing.T) {
	for _, p := range [][2]string{{"Manchester Utd", "Manchester United"}, {"St Albury", "Saint Albury"}, {"Northbridge Ath", "Northbridge Athletic"},
		{"Ashford Rovers FC", "Ashford Rovers"}, {"Atlético Madrid", "Atletico Madrid"}, {"Oakvale Utd", "Oakvale United"}} {
		if s := NameSimilarity(p[0], p[1]); s < 0.8 {
			t.Errorf("%q ~ %q = %.2f", p[0], p[1], s)
		}
	}
	if s := NameSimilarity("Moonstone FC", "Northbridge Athletic"); s > 0.5 {
		t.Errorf("unrelated names %.2f", s)
	}
	ko := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	cands := []Candidate{
		{FixtureID: 1, Kickoff: ko, HomeID: 10, AwayID: 11, Home: "Northbridge Athletic", Away: "Kingsport United"},
		{FixtureID: 2, Kickoff: ko, HomeID: 12, AwayID: 13, Home: "Kingsport City", Away: "Northbridge Rovers"},
		{FixtureID: 3, Kickoff: ko.Add(48 * time.Hour), HomeID: 10, AwayID: 13, Home: "Northbridge Athletic", Away: "Northbridge Rovers"},
	}
	if m := MatchEvent("Northbridge Ath", "Kingsport Utd", ko.Add(5*time.Minute), cands, nil); m.FixtureID != 1 {
		t.Fatalf("fuzzy %+v", m)
	}
	if m := MatchEvent("Northbridge Ath", "Kingsport Utd", ko.Add(20*time.Minute), cands, nil); m.FixtureID != 0 {
		t.Fatalf("outside ±15 min %+v", m)
	}
	if m := MatchEvent("Moonstone FC", "Sunbury Athletic", ko, cands, nil); m.FixtureID != 0 {
		t.Fatalf("decoy %+v", m)
	}
	// An alias decides when the names are ambiguous, and vetoes a wrong team.
	aliases := map[string]int64{NormTeam("NB Town"): 10, NormTeam("KU"): 11}
	if m := MatchEvent("NB Town", "KU", ko, cands, aliases); m.FixtureID != 1 || !m.ByAlias {
		t.Fatalf("aliases %+v", m)
	}
	aliases = map[string]int64{NormTeam("Northbridge Ath"): 99}
	if m := MatchEvent("Northbridge Ath", "Kingsport Utd", ko, cands, aliases); m.FixtureID != 0 {
		t.Fatalf("alias veto %+v", m)
	}
}

// ---------------------------------------------------------------- markets

func TestFairGroupAndBookmakers(t *testing.T) {
	p, margin, ok := fairGroup(Mkt1X2, map[string]float64{SelHome: 2.0, SelDraw: 3.4, SelAway: 3.8})
	if !ok || margin < 0.02 || margin > 0.07 || !approx(p[SelHome]+p[SelDraw]+p[SelAway], 1, 1e-9) || p[SelHome] < p[SelAway] {
		t.Fatalf("%v %v", p, margin)
	}
	if _, _, ok := fairGroup(Mkt1X2, map[string]float64{SelHome: 2.0, SelAway: 3.8}); ok {
		t.Fatal("incomplete group")
	}
	if _, _, ok := fairGroup(MktDC, map[string]float64{Sel1X: 1.3, Sel12: 1.3, SelX2: 1.9}); ok {
		t.Fatal("double chance is derived, not de-vigged")
	}
	for in, want := range map[string]string{"Pinnacle": "pinnacle", "1xBet": "1xbet", "onexbet": "1xbet", "Betway (UK)": "betway", "William Hill": "williamhill", "Betfair Exchange": "betfair_exchange"} {
		if got := bookmakerSlug(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
	if !ValidLine(MktOU, 2.25) || ValidLine(MktOU, 2.3) || ValidLine(MktOU, -1) || !ValidLine(MktAH, -1.75) || ValidLine(MktAH, 6) {
		t.Fatal("lines")
	}
	if SelectionLabel(MktAH, SelAway, fp(0.25), "A", "B") != "B +0.25" || SelectionLabel(MktOU, SelOver, fp(2.5), "", "") != "Over 2.5 goals" {
		t.Fatal("labels")
	}
}

func TestKenyanIn(t *testing.T) {
	got := KenyanIn([]string{"Pinnacle", "Bet365", "1xBet", "Betway", "SportPesa", "Unibet"})
	if strings.Join(got, ",") != "1xBet,Betway,SportPesa" {
		t.Fatal(got)
	}
}

func TestPeriodStart(t *testing.T) {
	loc, _ := time.LoadLocation("Africa/Nairobi")
	ts := time.Date(2026, 11, 20, 22, 30, 0, 0, time.UTC) // Saturday 01:30 in Nairobi
	d0, d1 := periodStart("daily", ts, loc)
	if d0.Format("2006-01-02 15:04") != "2026-11-21 00:00" || d1.Sub(d0) != 24*time.Hour {
		t.Fatal(d0, d1)
	}
	w0, _ := periodStart("weekly", ts, loc)
	if w0.Weekday() != time.Monday || w0.Format("2006-01-02") != "2026-11-16" {
		t.Fatal(w0)
	}
	m0, m1 := periodStart("monthly", ts, loc)
	if m0.Format("2006-01-02") != "2026-11-01" || m1.Format("2006-01-02") != "2026-12-01" {
		t.Fatal(m0, m1)
	}
	if money("KES", 1234567.5) != "KES 1,234,567.50" || money("KES", 5000) != "KES 5,000" {
		t.Fatal(money("KES", 1234567.5))
	}
}

func TestOddsDailyAllowance(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) // 10 days left incl. today
	if a := oddsDailyAllowance(500, 100, now); a != 40 {
		t.Fatal(a)
	}
	if oddsDailyAllowance(500, 500, now) != 0 {
		t.Fatal("used up")
	}
	st := Settings{OddsRegions: "uk,eu,au", OddsMarkets: "h2h,totals"}
	if st.OddsCallCost() != 6 {
		t.Fatal(st.OddsCallCost())
	}
	if s := (Settings{OddsSnapshots: "60, 1440,10,x"}).Snapshots(); len(s) != 3 || s[0] != 1440 || s[2] != 10 {
		t.Fatal(s)
	}
}
