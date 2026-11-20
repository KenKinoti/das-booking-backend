package football

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB tests: each test gets its own schema (TEST_DATABASE_URL; skipped
// without it) with the football tables migrated.

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	schema := "fb_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	admin.Exec("CREATE SCHEMA " + schema)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := db.DB(); err == nil {
			d.Close()
		}
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if d, err := admin.DB(); err == nil {
			d.Close()
		}
	})
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	ResetModelCache()
	return db
}

func newSvc(t *testing.T, db *gorm.DB, now time.Time) *Service {
	t.Helper()
	s := New(db)
	s.TestClock = true
	if err := s.SetClock(now); err != nil {
		t.Fatal(err)
	}
	s.OrgCurrency = func(string) string { return "KES" }
	s.OrgZone = func(string) *time.Location { l, _ := time.LoadLocation("Africa/Nairobi"); return l }
	return s
}

func count(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var n int64
	db.Table(table).Count(&n)
	return n
}

// storeSample stores a testdata fixtures response.
func storeSample(t *testing.T, s *Service, name string, details bool) StoreResult {
	t.Helper()
	var fs []afFixtureEntry
	envOf(t, name, &fs)
	r, err := s.storeFixtures(fs, details)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// ---------------------------------------------------------------- ingestion

func TestIngestionIdempotent(t *testing.T) {
	db := testDB(t)
	s := newSvc(t, db, time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	var ls []afLeagueEntry
	envOf(t, "af_leagues.json", &ls)
	for i := 0; i < 2; i++ {
		if _, err := s.storeLeagues(ls); err != nil {
			t.Fatal(err)
		}
		storeSample(t, s, "af_fixtures_season.json", false)
		storeSample(t, s, "af_fixtures_date.json", false)
		storeSample(t, s, "af_fixtures_ids_ft.json", true)
		var st []afStandingsEntry
		envOf(t, "af_standings.json", &st)
		if _, err := s.storeStandings(st); err != nil {
			t.Fatal(err)
		}
		var inj []afInjury
		envOf(t, "af_injuries.json", &inj)
		if _, err := s.storeInjuries(inj, nil); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"af_players_p1.json", "af_players_p2.json", "af_topscorers.json"} {
			var ps []afPlayerEntry
			envOf(t, p, &ps)
			if _, err := s.storePlayers(ps); err != nil {
				t.Fatal(err)
			}
		}
	}
	uniq := map[int64]bool{}
	for _, name := range []string{"af_fixtures_season.json", "af_fixtures_date.json", "af_fixtures_ids_ft.json"} {
		var fs []afFixtureEntry
		envOf(t, name, &fs)
		for _, f := range fs {
			uniq[f.Fixture.ID] = true
		}
	}
	want := map[string]int64{"fb_fixtures": int64(len(uniq)), "fb_fixture_stats": 4, "fb_lineups": 4, "fb_standings": 20}
	for tbl, n := range want {
		if got := count(t, db, tbl); got != n {
			t.Errorf("%s: %d rows, want %d", tbl, got, n)
		}
	}
	var evs int64
	db.Model(&FixtureEvent{}).Where("fixture_id = 1200141").Count(&evs)
	var fs []afFixtureEntry
	envOf(t, "af_fixtures_ids_ft.json", &fs)
	if int(evs) != len(fs[0].Events) {
		t.Fatalf("events replaced, not duplicated: %d vs %d", evs, len(fs[0].Events))
	}
	if n := count(t, db, "fb_player_seasons"); n < 22 {
		t.Fatalf("player seasons %d", n)
	}
	// The default competitions keep their ids; known leagues get coverage.
	var c Competition
	db.Where("id = 39").First(&c)
	if !c.Known || c.Season != 2026 || !c.Coverage.Odds || !c.Enabled || c.Note != "" {
		t.Fatalf("competition %+v", c)
	}
	var kenya Competition
	db.Where("id = 276").First(&kenya)
	if !kenya.Known || kenya.Country != "Kenya" || !kenya.Enabled {
		t.Fatalf("kenya %+v", kenya)
	}
	// A finished fixture got its markers once.
	var f Fixture
	db.Where("id = 1200141").First(&f)
	if f.FinishedAt == nil || f.DetailsAt == nil || f.LineupsAt == nil || f.FTHome == nil {
		t.Fatalf("fixture markers %+v", f)
	}
	// Odds: the same prices stored twice add no rows; a moved price adds one.
	var odds []afOddsEntry
	envOf(t, "af_odds.json", &odds)
	rows := oddsRowsFromAF(odds[0], s.Now())
	for i := range rows {
		rows[i].FixtureID = 1200141
	}
	ins, unch, err := s.storeOdds(rows)
	if err != nil || ins != len(rows) || unch != 0 {
		t.Fatal(ins, unch, err)
	}
	s.SetClock(s.Now().Add(time.Hour))
	again := oddsRowsFromAF(odds[0], s.Now())
	for i := range again {
		again[i].FixtureID = 1200141
	}
	again[0].Price += 0.05
	ins, unch, err = s.storeOdds(again)
	if err != nil || ins != 1 || unch != len(rows)-1 {
		t.Fatalf("second store: %d new, %d unchanged, %v", ins, unch, err)
	}
	var checked int64
	db.Model(&Odd{}).Where("checked_at > fetched_at").Count(&checked)
	if checked != int64(len(rows)-1) {
		t.Fatalf("checked_at updated on %d rows", checked)
	}
}

// A provider that lists a default competition under another id: the one it
// lists by name and country is used instead, with a note.
func TestLeagueIDCorrection(t *testing.T) {
	db := testDB(t)
	s := newSvc(t, db, time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	var ls []afLeagueEntry
	envOf(t, "af_leagues.json", &ls)
	for i := range ls {
		if ls[i].League.ID == 276 {
			ls[i].League.ID = 2760
		}
	}
	if _, err := s.storeLeagues(ls); err != nil {
		t.Fatal(err)
	}
	var old, moved Competition
	db.Where("id = 276").First(&old)
	db.Where("id = 2760").First(&moved)
	if old.Enabled || !strings.Contains(old.Note, "league 2760") || !moved.Enabled || moved.Group != "africa" {
		t.Fatalf("old %+v new %+v", old, moved)
	}
}

func TestStandingsHistory(t *testing.T) {
	db := testDB(t)
	day1 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	s := newSvc(t, db, day1)
	storeSample(t, s, "af_fixtures_season.json", false)
	var st []afStandingsEntry
	envOf(t, "af_standings.json", &st)
	for d := 0; d < 3; d++ {
		s.SetClock(day1.AddDate(0, 0, d))
		// Move the leader down a place each day.
		rows := st[0].League.Standings[0]
		rows[0].Rank, rows[1].Rank = rows[1].Rank, rows[0].Rank
		if _, err := s.storeStandings(st); err != nil {
			t.Fatal(err)
		}
		if _, err := s.storeStandings(st); err != nil { // same day twice → one snapshot
			t.Fatal(err)
		}
	}
	h := s.StandingsHistory(39, 2026)
	if len(h) != 3 || len(h["2026-11-21"]) != 20 {
		t.Fatalf("snapshots %d", len(h))
	}
	// With ≥ 3 snapshots the match page charts them.
	f := Fixture{ID: 77, CompetitionID: 39, Season: 2026, HomeID: st[0].League.Standings[0][0].Team.ID, AwayID: st[0].League.Standings[0][5].Team.ID, Kickoff: day1.AddDate(0, 0, 5), Status: "NS"}
	db.Create(&f)
	out := s.standingsSection(f, Competition{ID: 39})
	if out == nil || len(out.History.Home) != 3 || out.History.Source != "API-Football standings snapshots" || out.TableSize != 20 || out.Home == nil {
		t.Fatalf("section %+v", out)
	}
	// Without snapshots the history is replayed from stored results.
	db.Where("1=1").Delete(&Standing{})
	out = s.standingsSection(f, Competition{ID: 39})
	if out == nil || !strings.HasPrefix(out.Source, "computed") || len(out.History.Home) == 0 || !strings.HasPrefix(out.History.Source, "replayed") {
		t.Fatalf("computed %+v", out)
	}
	pts := 0
	for _, r := range out.Table {
		pts += r.Points
	}
	if pts == 0 || len(out.Table) != 20 {
		t.Fatalf("computed table %d rows", len(out.Table))
	}
}

// ---------------------------------------------------------------- predictions

// seedMatch adds an upcoming fixture between two teams of the sample season.
func seedMatch(t *testing.T, db *gorm.DB, id int64, ko time.Time) Fixture {
	f := Fixture{ID: id, CompetitionID: 39, Season: 2026, Round: "Regular Season - 15", Kickoff: ko, Status: "NS", HomeID: 9001, AwayID: 9002, FetchedAt: ko.Add(-24 * time.Hour)}
	if err := db.Create(&f).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func odd(fid int64, src, book, m, sel string, line *float64, price float64, at time.Time) Odd {
	return Odd{FixtureID: fid, Source: src, Bookmaker: book, BookmakerTitle: book, Market: m, Selection: sel, Line: line, Price: price, FetchedAt: at, CheckedAt: at}
}

func TestPredictionFreezeAndGrade(t *testing.T) {
	db := testDB(t)
	ko := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	s := newSvc(t, db, ko.Add(-26*time.Hour))
	storeSample(t, s, "af_fixtures_season.json", false) // 60 finished matches, rounds 1–6
	f := seedMatch(t, db, 5000001, ko)
	if n := s.FitDue(false); n != 1 {
		t.Fatalf("fitted %d", n)
	}
	var r Rating
	if db.Where("competition_id = 39 AND model = 'dc'").First(&r).Error != nil || len(r.Attack) != 20 || r.Matches != 60 {
		t.Fatalf("rating %+v", r)
	}
	if s.PredictUpcoming() != 1 {
		t.Fatal("prediction")
	}
	p, ok := s.CurrentPrediction(f)
	if !ok || p.Kind != "current" || p.Detail.DC == nil || p.Detail.Elo == nil || !approx(p.PHome+p.PDraw+p.PAway, 1, 1e-6) || len(p.Detail.Matrix) != 7 {
		t.Fatalf("current %+v", p)
	}
	// Market before kickoff: Pinnacle and a soft book.
	before := ko.Add(-2 * time.Hour)
	if _, _, err := s.storeOdds([]Odd{odd(f.ID, ProviderOddsAPI, "pinnacle", Mkt1X2, SelHome, nil, 2.1, before), odd(f.ID, ProviderOddsAPI, "pinnacle", Mkt1X2, SelDraw, nil, 3.5, before),
		odd(f.ID, ProviderOddsAPI, "pinnacle", Mkt1X2, SelAway, nil, 3.6, before), odd(f.ID, ProviderAPIFootball, "bet365", Mkt1X2, SelHome, nil, 2.0, before),
		odd(f.ID, ProviderAPIFootball, "bet365", Mkt1X2, SelDraw, nil, 3.3, before), odd(f.ID, ProviderAPIFootball, "bet365", Mkt1X2, SelAway, nil, 3.5, before)}); err != nil {
		t.Fatal(err)
	}
	// Not yet: freezing happens in the last 30 minutes.
	s.SetClock(ko.Add(-45 * time.Minute))
	if s.FreezeDue() != 0 {
		t.Fatal("frozen too early")
	}
	s.SetClock(ko.Add(-20 * time.Minute))
	if s.FreezeDue() != 1 || s.FreezeDue() != 0 {
		t.Fatal("freeze once")
	}
	var fr Prediction
	db.Where("fixture_id = ? AND kind = 'frozen'", f.ID).First(&fr)
	if fr.FrozenAt == nil || fr.MarketHome == nil || !strings.Contains(fr.MarketSource, "Pinnacle") {
		t.Fatalf("frozen %+v", fr)
	}
	// A late price (after kickoff) is not part of the closing line.
	if _, _, err := s.storeOdds([]Odd{odd(f.ID, ProviderOddsAPI, "pinnacle", Mkt1X2, SelHome, nil, 1.5, ko.Add(10*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	s.SetClock(ko.Add(15 * time.Minute))
	if err := s.Freeze(f); err == nil {
		t.Fatal("frozen after kickoff")
	}
	if s.CaptureClosing() < 1 {
		t.Fatal("closing")
	}
	db.Where("fixture_id = ? AND kind = 'frozen'", f.ID).First(&fr)
	if !strings.HasPrefix(fr.MarketSource, "closing: Pinnacle") || *fr.MarketHome < 0.4 || *fr.MarketHome > 0.5 {
		t.Fatalf("closing market %+v → %v", fr.MarketSource, *fr.MarketHome)
	}
	// The frozen copy is what the page shows after kickoff.
	f.Status = "1H"
	cur, _ := s.CurrentPrediction(f)
	if cur.Kind != "frozen" {
		t.Fatal("after kickoff the frozen prediction is shown")
	}
	// Full time 2-1 → graded.
	db.Model(&Fixture{}).Where("id = ?", f.ID).Updates(map[string]any{"status": "FT", "ft_home": 2, "ft_away": 1, "home_goals": 2, "away_goals": 1})
	if s.GradeFinished() != 1 || s.GradeFinished() != 0 {
		t.Fatal("graded once")
	}
	db.Where("fixture_id = ? AND kind = 'frozen'", f.ID).First(&fr)
	if fr.Outcome == nil || *fr.Outcome != 0 || fr.Brier == nil || fr.RPS == nil || fr.LogLoss == nil || fr.MarketBrier == nil ||
		!approx(*fr.Brier, (1-fr.PHome)*(1-fr.PHome)+fr.PDraw*fr.PDraw+fr.PAway*fr.PAway, 1e-9) {
		t.Fatalf("grade %+v", fr)
	}
	perf := s.ModelPerformance(nil, nil, nil)
	if perf.N != 1 || perf.NMarket != 1 || *perf.RPS != *fr.RPS || len(perf.ByCompetition) != 1 || len(perf.Calibration) == 0 {
		t.Fatalf("performance %+v", perf)
	}
	from := ko.Add(24 * time.Hour)
	if s.ModelPerformance(nil, &from, nil).N != 0 {
		t.Fatal("from filter")
	}
}

// ---------------------------------------------------------------- bets

func TestBetsSettlementCLVAndSummary(t *testing.T) {
	db := testDB(t)
	ko := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	s := newSvc(t, db, ko.Add(-3*time.Hour))
	storeSample(t, s, "af_fixtures_season.json", false)
	f := seedMatch(t, db, 5000002, ko)
	at := ko.Add(-4 * time.Hour)
	l25, lm25, lp25 := 2.5, -0.25, 0.25
	var rows []Odd
	for _, b := range []struct {
		book string
		k    float64
	}{{"pinnacle", 1}, {"betway", 0.95}, {"bet365", 0.97}} {
		rows = append(rows, odd(f.ID, ProviderOddsAPI, b.book, Mkt1X2, SelHome, nil, 2.2*b.k, at), odd(f.ID, ProviderOddsAPI, b.book, Mkt1X2, SelDraw, nil, 3.4*b.k, at),
			odd(f.ID, ProviderOddsAPI, b.book, Mkt1X2, SelAway, nil, 3.3*b.k, at), odd(f.ID, ProviderOddsAPI, b.book, MktOU, SelOver, &l25, 1.95*b.k, at),
			odd(f.ID, ProviderOddsAPI, b.book, MktOU, SelUnder, &l25, 1.9*b.k, at), odd(f.ID, ProviderOddsAPI, b.book, MktAH, SelHome, &lm25, 1.9*b.k, at),
			odd(f.ID, ProviderOddsAPI, b.book, MktAH, SelAway, &lp25, 1.95*b.k, at))
	}
	if _, _, err := s.storeOdds(rows); err != nil {
		t.Fatal(err)
	}
	org, user := "org-a", "u1"
	if _, err := s.AddBankroll(org, user, BankrollInput{Kind: "start", Amount: 10000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddBankroll(org, user, BankrollInput{Kind: "start", Amount: 5}); err == nil {
		t.Fatal("second start")
	}
	in := []BetInput{
		{Market: Mkt1X2, Selection: SelHome, Odds: 2.4, Stake: 100},
		{Market: MktOU, Selection: SelOver, Line: &l25, Odds: 2.0, Stake: 200},
		{Market: MktAH, Selection: SelHome, Line: &lm25, Odds: 2.0, Stake: 100},
		{Market: MktDNB, Selection: SelAway, Odds: 2.5, Stake: 100},
		{Market: MktBTTS, Selection: SelNo, Odds: 1.8, Stake: 50},
		{Market: MktDC, Selection: SelX2, Odds: 1.7, Stake: 50},
	}
	ids := []string{}
	for _, b := range in {
		b.FixtureID, b.Bookmaker = f.ID, "SportPesa"
		v, err := s.CreateBet(org, user, b)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != BetOpen || v.Currency != "KES" || v.ModelP == nil {
			t.Fatalf("bet %+v", v)
		}
		ids = append(ids, v.ID)
	}
	// Kickoff: closing odds (Pinnacle) and CLV.
	s.SetClock(ko.Add(time.Minute))
	if n := s.captureBetClosing(); n != 3 { // 1x2, ou 2.5, ah −0.25 were priced; dnb/btts/dc were not
		t.Fatalf("closing for %d bets", n)
	}
	var b0 Bet
	db.Where("id = ?", ids[0]).First(&b0)
	if b0.ClosingOdds == nil || *b0.ClosingOdds != 2.2 || b0.ClosingSource != "pinnacle" || !approx(*b0.CLV, 2.4/2.2-1, 1e-4) || b0.CLVFair == nil {
		t.Fatalf("clv %+v", b0)
	}
	// Full time 1-1.
	db.Model(&Fixture{}).Where("id = ?", f.ID).Updates(map[string]any{"status": "FT", "ft_home": 1, "ft_away": 1, "home_goals": 1, "away_goals": 1})
	if n := s.AutoSettle(); n != 6 {
		t.Fatalf("settled %d", n)
	}
	want := []struct {
		st  string
		pnl float64
	}{{BetLost, -100}, {BetLost, -200}, {BetHalfLost, -50}, {BetVoid, 0}, {BetLost, -50}, {BetWon, 35}}
	for i, id := range ids {
		var b Bet
		db.Where("id = ?", id).First(&b)
		if b.Status != want[i].st || !approx(b.PnL, want[i].pnl, 1e-9) || b.SettledBy != "auto" {
			t.Errorf("bet %d: %s %v, want %s %v", i, b.Status, b.PnL, want[i].st, want[i].pnl)
		}
	}
	sm := s.BetSummary(org, user, nil, nil)
	if sm.Settled != 6 || sm.Staked != 600 || sm.PnL != -365 || sm.Void != 1 || sm.HalfLost != 1 || !approx(sm.Yield, -365.0/600, 1e-4) ||
		!approx(sm.StrikeRate, 1.0/5, 1e-4) || sm.CLVBets != 3 || *sm.Bankroll.Balance != 9635 || *sm.Bankroll.Available != 9635 || !approx(*sm.ROI, -0.0365, 1e-4) {
		t.Fatalf("summary %+v", sm)
	}
	if sm.Bankroll.MaxDrawdown != 400 || len(sm.Bankroll.Series) != 7 || sm.Bankroll.Peak != 10000 || sm.Bankroll.Drawdown != 365 {
		t.Fatalf("bankroll %+v", sm.Bankroll)
	}
	tot := 0.0
	for _, b := range sm.ByMarket {
		tot += b.PnL
	}
	if !approx(tot, sm.PnL, 1e-9) || len(sm.ByBookmaker) != 1 || sm.ByBookmaker[0].Bets != 6 {
		t.Fatalf("breakdowns %+v", sm.ByMarket)
	}
	// Manual settlement and re-settling.
	v, err := s.SettleBet(org, user, ids[1], SettleInput{Status: BetCashedOut, CashOut: fp(150)})
	if err != nil || v.PnL != -50 || v.SettledBy != "manual" {
		t.Fatalf("cash out %+v %v", v, err)
	}
	if _, err := s.SettleBet(org, user, ids[1], SettleInput{Status: BetCashedOut, CashOut: fp(9999)}); err == nil {
		t.Fatal("cash-out above the potential return")
	}
	v, _ = s.SettleBet(org, user, ids[1], SettleInput{Status: "auto"})
	if v.Status != BetLost || v.SettledBy != "auto" {
		t.Fatal(v)
	}
	// Editing a settled bet re-settles it.
	v, err = s.UpdateBet(org, user, ids[0], BetInput{Selection: SelDraw})
	if err != nil || v.Status != BetWon || !approx(v.PnL, 140, 1e-9) {
		t.Fatalf("edit %+v %v", v, err)
	}
	// Void when the match is abandoned.
	g := seedMatch(t, db, 5000003, ko.Add(48*time.Hour))
	vb, _ := s.CreateBet(org, user, BetInput{FixtureID: g.ID, Market: Mkt1X2, Selection: SelHome, Odds: 2, Stake: 10, Bookmaker: "Betika"})
	db.Model(&Fixture{}).Where("id = ?", g.ID).Update("status", "ABD")
	s.AutoSettle()
	var vv Bet
	db.Where("id = ?", vb.ID).First(&vv)
	if vv.Status != BetVoid || vv.PnL != 0 || vv.SettledBy != "auto" {
		t.Fatal(vv.Status)
	}
	// Validation.
	for _, bad := range []BetInput{{FixtureID: f.ID, Market: "corners", Selection: SelHome, Odds: 2, Stake: 1, Bookmaker: "x"},
		{FixtureID: f.ID, Market: MktOU, Selection: SelOver, Odds: 2, Stake: 1, Bookmaker: "x"},
		{FixtureID: f.ID, Market: Mkt1X2, Selection: SelHome, Odds: 1, Stake: 1, Bookmaker: "x"},
		{FixtureID: f.ID, Market: Mkt1X2, Selection: SelHome, Odds: 2, Stake: 0, Bookmaker: "x"},
		{FixtureID: f.ID, Market: Mkt1X2, Selection: SelHome, Odds: 2, Stake: 1},
		{FixtureID: 999, Market: Mkt1X2, Selection: SelHome, Odds: 2, Stake: 1, Bookmaker: "x"}} {
		var ie *InputError
		if _, err := s.CreateBet(org, user, bad); !errors.As(err, &ie) {
			t.Errorf("accepted %+v (%v)", bad, err)
		}
	}
}

func TestLimitsCoolOffAndBreak(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC) // 12:00 in Nairobi
	s := newSvc(t, db, now)
	f := seedMatch(t, db, 5000004, now.Add(48*time.Hour))
	org, user := "org-a", "u1"
	bet := func(stake float64) error {
		_, err := s.CreateBet(org, user, BetInput{FixtureID: f.ID, Market: Mkt1X2, Selection: SelHome, Odds: 2, Stake: stake, Bookmaker: "Betika"})
		return err
	}
	if _, err := s.SetLimits(org, user, LimitSet{DailyStake: fp(1000), WeeklyLoss: fp(1500)}); err != nil {
		t.Fatal(err)
	}
	if err := bet(600); err != nil {
		t.Fatal(err)
	}
	var le *LimitError
	if err := bet(500); !errors.As(err, &le) || le.Limit != "daily_stake" || !strings.Contains(le.Msg, "KES 1,100") || le.Until == nil {
		t.Fatalf("daily stake limit: %v", err)
	}
	if err := bet(400); err != nil {
		t.Fatal("exactly the limit is fine:", err)
	}
	// Raising waits 24 h; lowering applies at once.
	v, _ := s.SetLimits(org, user, LimitSet{DailyStake: fp(5000), WeeklyLoss: fp(1200)})
	var ds, wl LimitUse
	for _, l := range v.Limits {
		if l.Key == "daily_stake" {
			ds = l
		}
		if l.Key == "weekly_loss" {
			wl = l
		}
	}
	if *ds.Limit != 1000 || ds.Pending == nil || *ds.Pending != 5000 || *wl.Limit != 1200 || v.PendingFrom == nil {
		t.Fatalf("limits %+v", v)
	}
	// Next day: the stake limit has not been raised yet (cool-off), but the
	// weekly loss limit counts the 1,000 still at risk.
	s.SetClock(now.Add(22 * time.Hour))
	if err := bet(300); !errors.As(err, &le) || le.Limit != "weekly_loss" {
		t.Fatalf("weekly loss limit: %v", err)
	}
	if err := bet(200); err != nil {
		t.Fatal(err)
	}
	s.SetClock(now.Add(25 * time.Hour))
	v = s.LimitsView(org, user)
	if v.PendingFrom != nil || *v.Limits[0].Limit != 5000 {
		t.Fatalf("pending applied: %+v", v.Limits[0])
	}
	// A break blocks bets and can't be shortened.
	if _, err := s.StartBreak(org, user, 7); err != nil {
		t.Fatal(err)
	}
	if err := bet(1); !errors.As(err, &le) || le.Limit != "break" {
		t.Fatalf("break: %v", err)
	}
	if _, err := s.StartBreak(org, user, 1); err == nil {
		t.Fatal("break shortened")
	}
	if _, err := s.StartBreak(org, user, 10); err != nil {
		t.Fatal("break extended:", err)
	}
	// Another person in the organisation is not affected.
	if _, err := s.CreateBet(org, "u2", BetInput{FixtureID: f.ID, Market: Mkt1X2, Selection: SelHome, Odds: 2, Stake: 5000, Bookmaker: "Betika"}); err != nil {
		t.Fatal(err)
	}
}

func TestOrgIsolation(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	s := newSvc(t, db, now)
	f := seedMatch(t, db, 5000005, now.Add(30*time.Hour))
	a, err := s.CreateBet("org-a", "u1", BetInput{FixtureID: f.ID, Market: Mkt1X2, Selection: SelHome, Odds: 2, Stake: 100, Bookmaker: "SportPesa"})
	if err != nil {
		t.Fatal(err)
	}
	s.CreateBet("org-b", "u9", BetInput{FixtureID: f.ID, Market: Mkt1X2, Selection: SelAway, Odds: 3, Stake: 50, Bookmaker: "Betika"})
	if _, err := s.BetView("org-b", "u9", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("another organisation reads a bet")
	}
	if _, err := s.BetView("org-a", "u2", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("a colleague reads a personal bet")
	}
	if err := s.DeleteBet("org-b", "u9", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted across organisations")
	}
	if _, err := s.UpdateBet("org-b", "u9", a.ID, BetInput{Stake: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatal("updated across organisations")
	}
	if l, n := s.Bets("org-a", "u1", BetFilter{}); n != 1 || len(l) != 1 {
		t.Fatal("own list")
	}
	if l, _ := s.Bets("org-b", "u9", BetFilter{}); len(l) != 1 || l[0].ID == a.ID {
		t.Fatal("other list")
	}
	if s.BetSummary("org-a", "u2", nil, nil).Bets != 0 {
		t.Fatal("summary per person")
	}
	// Hand-typed prices are seen only by their organisation.
	if _, err := s.AddManualOdd("org-a", "u1", ManualOddInput{FixtureID: f.ID, Bookmaker: "Odibets", Market: Mkt1X2, Selection: SelHome, Price: 2.3}); err != nil {
		t.Fatal(err)
	}
	mvA, mvB := s.marketView(f, nil, "org-a"), s.marketView(f, nil, "org-b")
	if len(mvA.Selections) != 1 || mvA.Selections[0].Prices[0].Source != "manual: Odibets" || len(mvB.Selections) != 0 || len(mvA.Manual) != 1 {
		t.Fatalf("manual odds A %+v B %+v", mvA.Selections, mvB.Selections)
	}
	if err := s.DeleteManualOdd("org-b", "u9", mvA.Manual[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted another organisation's price")
	}
	// Watchlists and bankrolls are per person.
	s.Watch("org-a", "u1", "fixture", f.ID, "")
	if len(s.Watchlist("org-a", "u2")) != 0 || len(s.Watchlist("org-a", "u1")) != 1 {
		t.Fatal("watchlist")
	}
	s.AddBankroll("org-a", "u1", BankrollInput{Kind: "start", Amount: 100})
	if s.bankrollBalance("org-b", "u1") != nil {
		t.Fatal("bankroll")
	}
}

// ---------------------------------------------------------------- provider calls, quota, back-off

type fakeAF struct {
	mu        sync.Mutex
	remaining int
	mode      string // "" | "429" | "500"
	calls     atomic.Int64
	seenKey   string
}

func sample(name string) []byte {
	b, _ := os.ReadFile("testdata/" + name)
	return b
}

func (f *fakeAF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seenKey = r.Header.Get("x-apisports-key") + r.URL.Query().Get("apiKey")
	if strings.HasPrefix(r.URL.Path, "/v4/") {
		w.Header().Set("x-requests-remaining", "4")
		w.Header().Set("x-requests-used", "496")
		if f.mode == "429" {
			w.Header().Set("x-requests-last", "0")
			w.WriteHeader(429)
			w.Write(sample("oa_error_credits.json"))
			return
		}
		w.Header().Set("x-requests-last", "6")
		w.Write(sample("oa_odds_epl.json"))
		return
	}
	w.Header().Set("x-ratelimit-requests-limit", "100")
	w.Header().Set("x-ratelimit-requests-remaining", itoa(f.remaining))
	switch f.mode {
	case "429":
		w.WriteHeader(429)
		w.Write(sample("af_error_requests.json"))
		return
	case "500":
		w.WriteHeader(500)
		return
	}
	f.remaining--
	if r.URL.Path == "/status" {
		w.Write(sample("af_status.json"))
		return
	}
	w.Write(sample("af_fixtures_date.json"))
}

func TestProviderCallsQuotaBackoff(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	s := newSvc(t, db, now)
	fk := &fakeAF{remaining: 50}
	srv := httptest.NewServer(fk)
	defer srv.Close()
	s.AFBase, s.OABase = srv.URL, srv.URL
	ctx := context.Background()
	if _, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Cost: 1}); !IsKind(err, "no_key") {
		t.Fatalf("no key: %v", err)
	}
	if err := s.SaveSettings(SettingsPatch{APIFootballKey: ptr("af-secret-key-123"), OddsAPIKey: ptr("oa-secret-key-456"), APIFootballDaily: ptr(10)}, "test"); err != nil {
		t.Fatal(err)
	}
	var stored Settings
	db.First(&stored)
	if strings.Contains(stored.APIFootballKey, "af-secret") || stored.APIFootballKey == "" {
		t.Fatal("key not encrypted")
	}
	tr := s.TestConnection(ctx, ProviderAPIFootball)
	if !tr.OK || tr.Plan != "Pro" || fk.seenKey != "af-secret-key-123" {
		t.Fatalf("test connection %+v", tr)
	}
	out, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Params: map[string][]string{"date": {"2026-11-21"}}, Cost: 1, Task: "t1"})
	if err != nil || out.Results == 0 || *out.Remaining != 49 {
		t.Fatal(out, err)
	}
	q := s.quotaRow(ProviderAPIFootball, dayPeriod(now))
	if q.Used != 1 || q.Requests != 2 || *q.ReportedRemain != 49 || *q.ReportedLimit != 100 {
		t.Fatalf("quota %+v", q)
	}
	// Budget (10/day) by priority: backfill may use 70%.
	for i := 0; i < 6; i++ {
		if _, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Cost: 1, Prio: PrioBackfill}); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Cost: 1, Prio: PrioBackfill}); !IsKind(err, "budget") {
		t.Fatalf("backfill over 70%%: %v", err)
	}
	if _, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Cost: 1, Prio: PrioCritical}); err != nil {
		t.Fatal("critical still allowed:", err)
	}
	// The provider says none left → no call.
	fk.remaining = 0
	s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Cost: 1, Prio: PrioCritical})
	n := fk.calls.Load()
	if _, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Cost: 1, Prio: PrioCritical}); !IsKind(err, "budget") || fk.calls.Load() != n {
		t.Fatalf("reported remaining respected: %v", err)
	}
	// 429 with errors.requests → quota → back off until the next UTC day.
	s.SetClock(now.Add(24 * time.Hour))
	fk.mode, fk.remaining = "429", 0
	if _, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Cost: 1}); !IsKind(err, "quota") {
		t.Fatalf("429: %v", err)
	}
	q = s.quotaRow(ProviderAPIFootball, dayPeriod(s.Now()))
	if q.BackoffUntil == nil || !q.BackoffUntil.Equal(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)) || !strings.Contains(q.LastError, "quota") {
		t.Fatalf("backoff %+v", q)
	}
	n = fk.calls.Load()
	if _, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Cost: 1}); !IsKind(err, "backoff") || fk.calls.Load() != n {
		t.Fatal("called while backing off")
	}
	// Server errors back off exponentially (a new day: the provider's
	// "0 left" of the 429 no longer applies).
	s.SetClock(now.Add(48 * time.Hour))
	fk.mode = "500"
	s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Cost: 1})
	q = s.quotaRow(ProviderAPIFootball, dayPeriod(s.Now()))
	if q.BackoffUntil == nil || q.BackoffUntil.Sub(s.Now()) > 3*time.Minute {
		t.Fatalf("server error backoff %+v", q)
	}
	// The Odds API: cost from x-requests-last, out of credits → back off to next month.
	out, err = s.call(ctx, callSpec{Provider: ProviderOddsAPI, Endpoint: "/v4/sports/soccer_epl/odds", Cost: 6, Prio: PrioCritical})
	if err != nil || out.Cost != 6 || *out.Remaining != 4 {
		t.Fatal(out, err)
	}
	fk.mode = "429"
	if _, err := s.call(ctx, callSpec{Provider: ProviderOddsAPI, Endpoint: "/v4/sports/soccer_epl/odds", Cost: 6, Prio: PrioCritical}); !IsKind(err, "quota") && !IsKind(err, "budget") {
		t.Fatalf("out of credits: %v", err)
	}
	// Every call logged; the key is never in the log.
	var logs []FetchLog
	db.Find(&logs)
	if len(logs) < 12 {
		t.Fatalf("logged %d", len(logs))
	}
	b, _ := json.Marshal(logs)
	if strings.Contains(string(b), "secret-key") {
		t.Fatal("key in the fetch log")
	}
}

// ---------------------------------------------------------------- scheduler against a stub provider

// A stub serving the recorded samples by endpoint.
func stubProviders(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var n atomic.Int64
	files := map[string]string{"/leagues": "af_leagues.json", "/standings": "af_standings.json", "/injuries": "af_injuries.json", "/players": "af_players_p1.json",
		"/players/topscorers": "af_topscorers.json", "/odds": "af_odds.json", "/predictions": "af_predictions.json", "/fixtures/headtohead": "af_h2h.json",
		"/teams": "af_teams.json", "/status": "af_status.json", "/odds/live": "af_odds_live.json", "/odds/bookmakers": "af_bookmakers.json", "/v4/sports": "oa_sports.json", "/v4/sports/soccer_epl/odds": "oa_odds_epl.json"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		q := r.URL.Query()
		name := files[r.URL.Path]
		switch {
		case r.URL.Path == "/fixtures" && q.Get("ids") != "":
			name = "af_fixtures_ids_ft.json"
		case r.URL.Path == "/fixtures" && q.Get("date") != "":
			name = "af_fixtures_date.json"
		case r.URL.Path == "/fixtures" && q.Get("league") == "39" && q.Get("season") == "2026":
			name = "af_fixtures_season.json"
		case r.URL.Path == "/fixtures":
			w.Write([]byte(`{"get":"fixtures","errors":[],"results":0,"paging":{"current":1,"total":1},"response":[]}`))
			return
		case strings.HasPrefix(r.URL.Path, "/v4/"):
			w.Header().Set("x-requests-remaining", "400")
			if strings.HasSuffix(r.URL.Path, "/odds") {
				w.Header().Set("x-requests-last", "6")
			} else {
				w.Header().Set("x-requests-last", "0")
			}
		}
		if name == "" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("x-ratelimit-requests-remaining", "7000")
		w.Write(load(t, name))
	}))
	return srv, &n
}

func TestRunOnceWithStubProviders(t *testing.T) {
	db := testDB(t)
	s := newSvc(t, db, time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC))
	srv, calls := stubProviders(t)
	defer srv.Close()
	s.AFBase, s.OABase = srv.URL, srv.URL
	if err := s.SaveSettings(SettingsPatch{APIFootballKey: ptr("af-secret-key-123"), OddsAPIKey: ptr("oa-secret-key-456"), APIFootballPlan: ptr("pro"),
		Enabled: []int64{39}}, "test"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sum, err := s.RunOnce(ctx, RunOpts{Trigger: "test"})
	if err != nil || sum.Failed != 0 || sum.Done < 5 {
		t.Fatalf("first pass %+v %v", sum, err)
	}
	sum2, _ := s.RunOnce(ctx, RunOpts{Trigger: "test"})
	if sum2.Failed != 0 || sum2.Done == 0 {
		t.Fatalf("second pass %+v", sum2)
	}
	var logged int64
	db.Model(&FetchLog{}).Count(&logged)
	if logged != calls.Load() {
		t.Fatalf("logged %d of %d calls", logged, calls.Load())
	}
	if count(t, db, "fb_standings") != 20 || count(t, db, "fb_odds") == 0 || count(t, db, "fb_player_seasons") == 0 {
		t.Fatal("stored")
	}
	st, _ := s.Settings()
	if len(st.AFBookmakers) < 3 || strings.Join(KenyanIn(st.AFBookmakers), ",") != "1xBet,Betway" {
		t.Fatalf("bookmakers %v → Kenyan %v", st.AFBookmakers, KenyanIn(st.AFBookmakers))
	}
	// The Odds API snapshot was matched to the stored fixtures.
	var matched int64
	db.Model(&Fixture{}).Where("odds_event_id <> ''").Count(&matched)
	if matched == 0 {
		t.Fatal("no odds events matched")
	}
	// A third pass with nothing due makes no calls.
	before := calls.Load()
	sum3, _ := s.RunOnce(ctx, RunOpts{Trigger: "test"})
	if calls.Load() != before || sum3.Planned != 0 {
		t.Fatalf("nothing due but %d calls (%d planned)", calls.Load()-before, sum3.Planned)
	}
	// One run at a time.
	s.runMu.Lock()
	if _, err := s.RunOnce(ctx, RunOpts{}); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent run")
	}
	s.runMu.Unlock()
	// Another instance holding the advisory lock → busy.
	conn, _ := db.DB()
	c, _ := conn.Conn(ctx)
	c.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext('football:scheduler'))")
	if _, err := s.RunOnce(ctx, RunOpts{}); !errors.Is(err, ErrBusy) {
		t.Fatal("advisory lock ignored")
	}
	c.ExecContext(ctx, "SELECT pg_advisory_unlock(hashtext('football:scheduler'))")
	c.Close()
	// A tight budget stops low-priority work.
	s.SaveSettings(SettingsPatch{APIFootballDaily: ptr(10)}, "test")
	used := s.quotaRow(ProviderAPIFootball, dayPeriod(s.Now())).Used
	before = calls.Load()
	s.RunOnce(ctx, RunOpts{Force: true})
	if s.quotaRow(ProviderAPIFootball, dayPeriod(s.Now())).Used > max(used, 10) {
		t.Fatalf("over budget: %d", s.quotaRow(ProviderAPIFootball, dayPeriod(s.Now())).Used)
	}
	_ = before
}
