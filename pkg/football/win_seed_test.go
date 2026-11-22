package football

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// Seed generator for the UI agent (skipped by default):
//
//	FBWIN_SEED_OUT=testdata/win/seed_fbwin.sql go test ./pkg/football -run TestGenerateSeedFbwin
//
// It builds the v2.18 world with the real code under a test clock fixed at
// T0 = 2026-11-22 12:00 UTC — upcoming fixtures with verdicts in every tier
// (a draw alert, consensus from several sources, one without a market),
// the graded weekend of 20–22 Nov (the 20 Nov SportPesa jackpot's 13 games
// with their real results, correct and wrong picks, a void, a no-pick) and
// two earlier graded weekends — then dumps the rows as idempotent SQL. Rows
// of upcoming fixtures are written relative to now() so the world is always
// "upcoming" when loaded; the past stays at its real dates. Organisation /
// user ids are placeholders that seed-fbwin.sh replaces with the test
// accounts (kennedy@dasyin.com.au, Michael.Thomas75@test.com).

var seedT0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// The 20 Nov 2026 SportPesa jackpot: real games, odds and results.
var winJP20Nov = []struct {
	Fix        int64
	Odds       [3]float64
	HG, AG     int
	Pinnacle   [3]float64
	Betika     [3]float64
	TeamsLabel string
}{
	{9100101, [3]float64{2.44, 3.35, 2.55}, 2, 0, [3]float64{2.52, 3.56, 2.84}, [3]float64{2.44, 3.35, 2.55}, "Piast Gliwice v Korona Kielce"},
	{9100102, [3]float64{2.19, 3.20, 2.90}, 0, 2, [3]float64{2.27, 3.38, 3.15}, [3]float64{2.19, 3.20, 2.90}, "Chrobry Głogów v Puszcza Niepołomice"},
	{9100103, [3]float64{2.40, 3.55, 2.36}, 1, 1, [3]float64{2.47, 3.52, 2.63}, [3]float64{2.40, 3.55, 2.36}, "Lechia Zielona Góra v Chojniczanka"},
	{9100104, [3]float64{2.60, 3.50, 2.25}, 1, 0, [3]float64{2.70, 3.62, 2.41}, [3]float64{2.60, 3.50, 2.25}, "Kapfenberg v Hertha Wels"},
	{9100105, [3]float64{2.20, 3.55, 3.00}, 1, 2, [3]float64{2.31, 3.62, 3.20}, [3]float64{2.20, 3.55, 3.00}, "Arminia Bielefeld v Magdeburg"},
	{9100106, [3]float64{2.55, 3.30, 2.36}, 1, 1, [3]float64{2.66, 3.45, 2.58}, [3]float64{2.55, 3.30, 2.36}, "Kolding v AaB"},
	{9100107, [3]float64{2.60, 3.30, 2.37}, 2, 3, [3]float64{2.71, 3.36, 2.66}, [3]float64{2.60, 3.30, 2.37}, "Farul v U Cluj"},
	{9100108, [3]float64{2.23, 3.40, 2.65}, 2, 2, [3]float64{2.35, 3.50, 2.88}, [3]float64{2.23, 3.40, 2.65}, "Club Brugge U21 v Virton"},
	{9100109, [3]float64{2.20, 3.50, 2.80}, 5, 1, [3]float64{2.30, 3.66, 2.97}, [3]float64{2.20, 3.50, 2.80}, "Helmond Sport v Dordrecht"},
	{9100110, [3]float64{2.65, 3.75, 2.20}, 1, 4, [3]float64{2.80, 3.80, 2.36}, [3]float64{2.65, 3.75, 2.20}, "Almere City v NAC Breda"},
	{9100111, [3]float64{2.30, 3.40, 2.60}, 1, 2, [3]float64{2.42, 3.47, 2.83}, [3]float64{2.30, 3.40, 2.60}, "Austria Salzburg v Admira"},
	{9100112, [3]float64{2.22, 3.40, 2.70}, 1, 1, [3]float64{2.30, 3.49, 2.96}, [3]float64{2.22, 3.40, 2.70}, "Miedź Legnica v ŁKS Łódź"},
	{9100113, [3]float64{2.65, 3.40, 2.28}, 3, 2, [3]float64{2.74, 3.48, 2.50}, [3]float64{2.65, 3.40, 2.28}, "Cliftonville v Glentoran"},
}

// JP20NovTicket is the ticket that was played (SportPesa's favourites).
var JP20NovTicket = []string{"1", "1", "2", "2", "1", "2", "2", "1", "1", "2", "1", "1", "2"}

func loadSQLFile(t *testing.T, db *gorm.DB, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "--") {
			continue
		}
		if err := db.Exec(l).Error; err != nil {
			t.Fatalf("%s: %v", l[:min(len(l), 80)], err)
		}
	}
}

type seedW struct {
	t  *testing.T
	db *gorm.DB
	s  *Service
}

func (w seedW) team(id int64, name, country string) {
	w.db.Exec(`INSERT INTO fb_teams (id, name, code, country, logo, national, founded, venue, city, fetched_at) VALUES (?,?,?,?,'',false,0,'','',?) ON CONFLICT (id) DO NOTHING`,
		id, name, strings.ToUpper(name[:min(3, len(name))]), country, w.s.Now())
}

func (w seedW) fixture(id, comp int64, ko time.Time, home, away int64, round string) {
	if err := w.db.Create(&Fixture{ID: id, CompetitionID: comp, Season: 2026, Round: round, Kickoff: ko, Status: "NS", StatusLong: "Not Started", HomeID: home, AwayID: away,
		FetchedAt: w.s.Now(), UpdatedAt: w.s.Now()}).Error; err != nil {
		w.t.Fatal(err)
	}
}

func (w seedW) odds(fid int64, source, book, title string, o [3]float64, at time.Time) {
	for i, sel := range []string{SelHome, SelDraw, SelAway} {
		w.db.Create(&Odd{FixtureID: fid, Source: source, Bookmaker: book, BookmakerTitle: title, Market: Mkt1X2, Selection: sel, Price: o[i], FetchedAt: at, CheckedAt: at})
	}
}

func (w seedW) ou(fid int64, book, title string, over, under float64, at time.Time) {
	l := 2.5
	w.db.Create(&Odd{FixtureID: fid, Source: ProviderOddsAPI, Bookmaker: book, BookmakerTitle: title, Market: MktOU, Selection: SelOver, Line: &l, Price: over, FetchedAt: at, CheckedAt: at})
	w.db.Create(&Odd{FixtureID: fid, Source: ProviderOddsAPI, Bookmaker: book, BookmakerTitle: title, Market: MktOU, Selection: SelUnder, Line: &l, Price: under, FetchedAt: at, CheckedAt: at})
}

// market adds Pinnacle plus three soft books around a fair 1X2.
func (w seedW) market(fid int64, pin [3]float64, at time.Time, withPin bool) {
	if withPin {
		w.odds(fid, ProviderOddsAPI, "pinnacle", "Pinnacle", pin, at)
	}
	books := []struct {
		k, t string
		m    float64
		skew float64
	}{{"bet365", "Bet365", 1.035, 0.02}, {"williamhill", "William Hill", 1.04, -0.02}, {"betway", "Betway", 1.045, 0.0}}
	for _, b := range books {
		var o [3]float64
		for i := range pin {
			adj := 1.0
			if i == 0 {
				adj += b.skew
			} else if i == 2 {
				adj -= b.skew
			}
			o[i] = float64(int(pin[i]/b.m*adj*100+0.5)) / 100
		}
		w.odds(fid, ProviderOddsAPI, b.k, b.t, o, at)
	}
}

func (w seedW) srcProb(fid int64, source string, p [3]float64, detail map[string]any, url string, at time.Time) {
	w.db.Create(&SourceProb{FixtureID: fid, Source: source, PHome: p[0], PDraw: p[1], PAway: p[2], Detail: detail, URL: url, FetchedAt: at})
}

func (w seedW) result(fid int64, status string, hg, ag *int, at time.Time) {
	up := map[string]any{"status": status, "status_long": map[string]string{"FT": "Match Finished", "ABD": "Match Abandoned"}[status], "fetched_at": at, "updated_at": at}
	if hg != nil {
		up["home_goals"], up["away_goals"], up["ft_home"], up["ft_away"], up["finished_at"], up["elapsed"] = *hg, *ag, *hg, *ag, at, 90
	}
	w.db.Model(&Fixture{}).Where("id = ?", fid).Updates(up)
}

func ip(n int) *int { return &n }

// freezeAt moves the clock to 20 minutes before a kickoff and freezes.
func (w seedW) freezeAt(ko time.Time) {
	w.s.SetClock(ko.Add(-20 * time.Minute))
	w.s.FreezeDue()
	w.s.FreezeVerdicts()
}

func TestGenerateSeedFbwin(t *testing.T) {
	out := os.Getenv("FBWIN_SEED_OUT")
	if out == "" {
		t.Skip("FBWIN_SEED_OUT not set (seed generator)")
	}
	db := testDB(t)
	s := newSvc(t, db, time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))
	w := seedW{t, db, s}
	loadSQLFile(t, db, "testdata/free/seed_jackpot.sql")
	db.Exec(`UPDATE fb_free_settings SET enabled = ? WHERE id = 1`, mustJSON(map[string]bool{"oddspapi": false, "clubelo": true, "fdcouk": false, "fdorg": false,
		"google_news": false, "gdelt": false, "feeds": false, "apifootball_pred": true, "polymarket": true, "openfootball": false}))
	// Teams (invented names, as in the fakes) for the Premier League (39) and the Kenyan league (276).
	names := []string{"Northbridge Athletic", "Kingsport United", "Ashford Rovers", "Riverside City", "Hollowmere Town", "Saint Albury", "Westholm Wanderers",
		"Eastmoor Albion", "Brackenfield", "Coldwater Rangers", "Fairhaven Town", "Greywick County"}
	for i, n := range names {
		w.team(9300101+int64(i), n, "England")
	}
	w.team(9300201, "Tana River Stars", "Kenya")
	w.team(9300202, "Lakeside FC", "Kenya")
	w.team(9300203, "Highland United", "Kenya")
	w.team(9300204, "Coast Mariners", "Kenya")
	tm := func(i int) int64 { return 9300101 + int64(i) }

	// ---------------------------------------------------------------- earlier weekends (7–8 and 14–15 Nov)
	type past struct {
		id         int64
		ko         time.Time
		h, a       int64
		pin        [3]float64
		hg, ag     int
		comp       int64
		status     string
		noFreeze   bool
		withPinOff bool
	}
	pasts := []past{
		{9300301, time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC), tm(0), tm(3), [3]float64{1.60, 4.00, 5.80}, 2, 1, 39, "FT", false, false},
		{9300302, time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC), tm(4), tm(1), [3]float64{2.35, 3.30, 3.20}, 0, 0, 39, "FT", false, false},
		{9300303, time.Date(2026, 11, 8, 14, 0, 0, 0, time.UTC), tm(2), tm(5), [3]float64{1.95, 3.50, 4.10}, 1, 2, 39, "FT", false, false},
		{9300304, time.Date(2026, 11, 8, 16, 30, 0, 0, time.UTC), tm(6), tm(9), [3]float64{2.70, 3.20, 2.70}, 1, 0, 39, "FT", false, false},
		{9300305, time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC), tm(1), tm(0), [3]float64{3.10, 3.40, 2.30}, 1, 3, 39, "FT", false, false},
		{9300306, time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC), tm(7), tm(8), [3]float64{1.50, 4.30, 6.80}, 3, 0, 39, "FT", false, false},
		{9300307, time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC), tm(10), tm(11), [3]float64{2.05, 3.40, 3.70}, 2, 2, 39, "FT", false, false},
		{9300308, time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC), 9300201, 9300202, [3]float64{2.10, 3.10, 3.60}, 1, 0, 276, "FT", false, false},
		// The graded weekend 20–22 Nov (with the jackpot's 13 games below).
		{9300311, time.Date(2026, 11, 21, 12, 30, 0, 0, time.UTC), tm(0), tm(2), [3]float64{1.48, 4.50, 7.00}, 2, 0, 39, "FT", false, false},
		{9300312, time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC), tm(3), tm(1), [3]float64{1.90, 3.60, 4.20}, 1, 1, 39, "FT", false, false},
		{9300313, time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC), tm(4), tm(6), [3]float64{2.20, 3.40, 3.30}, 0, 1, 39, "FT", false, false},
		{9300314, time.Date(2026, 11, 21, 17, 30, 0, 0, time.UTC), tm(7), tm(5), [3]float64{2.55, 3.20, 2.85}, 3, 1, 39, "FT", false, false},
		{9300315, time.Date(2026, 11, 21, 17, 30, 0, 0, time.UTC), tm(8), tm(10), [3]float64{2.40, 3.30, 2.95}, 0, 0, 39, "ABD", false, false},
		{9300316, time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC), tm(9), tm(11), [3]float64{2.30, 3.30, 3.10}, 0, 2, 39, "FT", true, false},
		{9300317, time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC), 9300203, 9300204, [3]float64{2.60, 3.00, 2.90}, 2, 2, 276, "FT", false, false},
	}
	for _, p := range pasts {
		w.fixture(p.id, p.comp, p.ko, p.h, p.a, "Regular Season - 13")
		w.market(p.id, p.pin, p.ko.Add(-3*time.Hour), true)
		w.ou(p.id, "pinnacle", "Pinnacle", 1.95, 1.93, p.ko.Add(-3*time.Hour))
	}
	// The jackpot games: Betika (OddsPapi) and Pinnacle prices before kickoff.
	var jkos []time.Time
	for _, g := range winJP20Nov {
		var f Fixture
		db.Where("id = ?", g.Fix).First(&f)
		jkos = append(jkos, f.Kickoff)
		w.odds(g.Fix, ProviderOddsPapi, "betika", "Betika (KE)", g.Betika, f.Kickoff.Add(-4*time.Hour))
		w.odds(g.Fix, ProviderOddsPapi, "pinnacle", "Pinnacle", g.Pinnacle, f.Kickoff.Add(-4*time.Hour))
	}
	// The jackpot (one for each test login), created on the morning of 20 Nov.
	s.SetClock(time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	closes := time.Date(2026, 11, 20, 16, 45, 0, 0, time.UTC)
	jpIDs := map[string]string{}
	for _, who := range []string{"K", "M"} {
		games := []GameInput{}
		for _, g := range winJP20Nov {
			fid := g.Fix
			games = append(games, GameInput{Home: strings.Split(g.TeamsLabel, " v ")[0], Away: strings.Split(g.TeamsLabel, " v ")[1], Odds: g.Odds[:], FixtureID: &fid})
		}
		name, book, prize, price := "SportPesa Midweek 20 Nov", "SportPesa", 22500447.0, 99.0
		st := "open"
		v, err := s.CreateJackpot("@@ORG_"+who+"@@", "@@USER_"+who+"@@", JackpotInput{Name: &name, Bookmaker: &book, Prize: &prize, TicketPrice: &price, ClosesAt: &closes,
			Status: &st, Games: games, Bonuses: []BonusTier{{12, 250000}, {11, 25000}, {10, 2500}},
			Weights: map[string]float64{"jackpot": 1, "market": 0, "clubelo": 0, "model": 0}})
		if err != nil {
			t.Fatal(err)
		}
		jpIDs[who] = v.Jackpot.ID
		got := strings.Join(v.Analysis.MostLikely.Picks, " ")
		if got != strings.Join(JP20NovTicket, " ") {
			t.Fatalf("most likely %s", got)
		}
	}
	// Freeze everything in time order: the earlier weekends, the jackpot at close, the 20–22 Nov games.
	events := []time.Time{}
	for _, p := range pasts {
		if !p.noFreeze {
			events = append(events, p.ko)
		}
	}
	events = append(events, jkos...)
	sort.Slice(events, func(i, j int) bool { return events[i].Before(events[j]) })
	frozeJP := false
	for _, ko := range events {
		if !frozeJP && !ko.Before(closes) {
			s.SetClock(closes.Add(-2 * time.Minute))
			if n := s.FreezeJackpots(); n != 2 {
				t.Fatalf("jackpots frozen: %d", n)
			}
			frozeJP = true
		}
		w.freezeAt(ko)
	}
	// Results and grading.
	for _, p := range pasts {
		if p.status == "ABD" {
			w.result(p.id, "ABD", nil, nil, p.ko.Add(2*time.Hour))
		} else {
			w.result(p.id, "FT", ip(p.hg), ip(p.ag), p.ko.Add(2*time.Hour))
		}
	}
	for i, g := range winJP20Nov {
		w.result(g.Fix, "FT", ip(g.HG), ip(g.AG), jkos[i].Add(2*time.Hour))
	}
	s.SetClock(time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC))
	s.GradeFinished()
	if n := s.GradeVerdicts(); n == 0 {
		t.Fatal("nothing graded")
	}
	if n := s.GradeJackpots(); n != 2 {
		t.Fatalf("jackpots graded %d", n)
	}
	// Checks: the jackpot review matches the facts of 20 Nov.
	for who, id := range jpIDs {
		v, err := s.JackpotViewOf("@@ORG_"+who+"@@", "@@USER_"+who+"@@", id, 0)
		if err != nil || v.Review == nil {
			t.Fatal(err)
		}
		r := v.Review
		if r.Correct != 4 || r.Of != 13 || r.SaferHits != 8 || r.DrawsActual != 4 || r.FavouritesCorrect != 4 || fmt.Sprintf("%.2f", r.Expected) != "5.16" ||
			fmt.Sprintf("%.1f", r.DrawsExpected) != "3.4" || fmt.Sprintf("%.2f", r.PAtMostCorrect) != "0.36" || !r.Complete {
			t.Fatalf("review %+v", r)
		}
	}
	// Upcoming world (relative to T0).
	s.SetClock(seedT0)
	type up struct {
		id     int64
		in     time.Duration
		h, a   int64
		pin    [3]float64
		comp   int64
		market bool
		pm     *[3]float64
		ap     *[3]float64
		ce     bool
	}
	pm1 := [3]float64{0.66, 0.21, 0.13}
	ap1 := [3]float64{0.55, 0.25, 0.20}
	pm4 := [3]float64{0.33, 0.32, 0.35}
	ap3 := [3]float64{0.45, 0.30, 0.25}
	ups := []up{
		{9300001, 3 * time.Hour, tm(0), tm(1), [3]float64{1.45, 4.60, 7.50}, 39, true, &pm1, &ap1, true},
		{9300002, 5 * time.Hour, tm(2), tm(3), [3]float64{1.85, 3.60, 4.50}, 39, true, nil, nil, false},
		{9300003, 5 * time.Hour, tm(4), tm(5), [3]float64{2.15, 3.40, 3.50}, 39, true, nil, &ap3, false},
		{9300004, 26 * time.Hour, tm(6), tm(7), [3]float64{2.90, 3.00, 2.75}, 39, true, &pm4, nil, false},
		{9300005, 27 * time.Hour, tm(8), tm(9), [3]float64{}, 39, false, nil, nil, false},
		{9300006, 28 * time.Hour, tm(10), tm(11), [3]float64{6.00, 4.20, 1.55}, 39, true, nil, nil, false},
		{9300007, 30 * time.Hour, 9300201, 9300204, [3]float64{2.25, 3.10, 3.30}, 276, true, nil, nil, false},
		{9300008, 50 * time.Hour, 9300203, 9300202, [3]float64{1.95, 3.20, 4.00}, 276, true, nil, nil, false},
	}
	for _, u := range ups {
		ko := seedT0.Add(u.in)
		w.fixture(u.id, u.comp, ko, u.h, u.a, "Regular Season - 14")
		at := seedT0.Add(-20 * time.Minute)
		if u.market {
			w.market(u.id, u.pin, at, u.comp == 39)
			if u.comp == 39 {
				w.ou(u.id, "pinnacle", "Pinnacle", 1.90+float64(u.id%3)/10, 1.95, at)
			}
		}
		if u.pm != nil {
			w.srcProb(u.id, ProviderPolymarket, *u.pm, map[string]any{"event_id": fmt.Sprint(90000 + u.id%100), "slug": "epl-seed-" + strconv.FormatInt(u.id, 10), "title": s.teamName(u.h) + " vs. " + s.teamName(u.a),
				"n_markets": 3, "markets": []PMMarketOut{}}, "https://polymarket.com/event/epl-seed-"+strconv.FormatInt(u.id, 10), seedT0.Add(-time.Hour))
		}
		if u.ap != nil {
			w.srcProb(u.id, ProviderAFPred, *u.ap, map[string]any{"advice": "Double chance : " + s.teamName(u.h) + " or draw", "winner": s.teamName(u.h),
				"percent": map[string]string{"home": fmt.Sprintf("%.0f%%", u.ap[0]*100), "draw": fmt.Sprintf("%.0f%%", u.ap[1]*100), "away": fmt.Sprintf("%.0f%%", u.ap[2]*100)}},
				docAFPred, seedT0.Add(-3*time.Hour))
		}
		if u.ce {
			d := seedT0.Truncate(24 * time.Hour)
			db.Exec(`INSERT INTO fb_clubelo_ratings (club, date, country, level, elo, team_id, fetched_at) VALUES ('Northbridge', ?, 'ENG', 1, 1902.5, ?, ?), ('Kingsport', ?, 'ENG', 1, 1688.0, ?, ?)`,
				d, u.h, seedT0.Add(-6*time.Hour), d, u.a, seedT0.Add(-6*time.Hour))
			db.Create(&ClubEloFixture{Date: ko.Truncate(24 * time.Hour), Home: "Northbridge", Away: "Kingsport", Country: "ENG", PHome: 0.61, PDraw: 0.23, PAway: 0.16, FixtureID: u.id, FetchedAt: seedT0.Add(-6 * time.Hour)})
		}
	}
	s.PredictUpcoming()
	// Every tier and a draw alert among the upcoming verdicts.
	rows, err := s.Fixtures(FixtureQuery{From: ptr(seedT0), To: ptr(seedT0.Add(72 * time.Hour)), Status: "upcoming", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	tiers, alert, multi, nomarket := map[string]bool{}, false, false, false
	for _, r := range rows {
		if r.Verdict == nil {
			continue
		}
		tiers[r.Verdict.Tier.Key] = true
		alert = alert || r.Verdict.DrawAlert.On
		multi = multi || len(r.Verdict.Consensus.Sources) >= 4
		nomarket = nomarket || !r.Verdict.Consensus.MarketUsed
	}
	if len(tiers) != 4 || !alert || !multi || !nomarket {
		t.Fatalf("upcoming verdicts: tiers %v alert %v multi %v nomarket %v", tiers, alert, multi, nomarket)
	}
	// Fixed jackpot ids (idempotent seed).
	for who, id := range jpIDs {
		nid := map[string]string{"K": "6b1f0c1e-0000-4000-8000-00000000000a", "M": "6b1f0c1e-0000-4000-8000-00000000000b"}[who]
		db.Exec(`UPDATE fb_jackpots SET id = ? WHERE id = ?`, nid, id)
		db.Exec(`UPDATE fb_jackpot_games SET jackpot_id = ? WHERE jackpot_id = ?`, nid, id)
		db.Exec(`UPDATE fb_jackpot_tickets SET jackpot_id = ?, id = ? WHERE jackpot_id = ?`, nid, strings.Replace(nid, "-00000000000", "-10000000000", 1), id)
	}
	// Dump.
	upIDs := []string{}
	for _, u := range ups {
		upIDs = append(upIDs, strconv.FormatInt(u.id, 10))
	}
	pastIDs := []string{}
	for _, p := range pasts {
		pastIDs = append(pastIDs, strconv.FormatInt(p.id, 10))
	}
	for _, g := range winJP20Nov {
		pastIDs = append(pastIDs, strconv.FormatInt(g.Fix, 10))
	}
	ups2 := strings.Join(upIDs, ",")
	pasts2 := strings.Join(pastIDs, ",")
	var b strings.Builder
	b.WriteString("-- DASYIN football v2.18 seed (agent fbwin): upcoming verdicts in every tier, the graded weekend of 20–22 Nov 2026 (incl. the\n")
	b.WriteString("-- 20 Nov SportPesa jackpot with its real results), two earlier graded weekends. Generated by TestGenerateSeedFbwin — do not edit.\n")
	b.WriteString("-- Load after seed_jackpot.sql (fbfree); placeholders @@ORG_K@@/@@USER_K@@/@@ORG_M@@/@@USER_M@@ are replaced by seed-fbwin.sh.\n")
	b.WriteString("BEGIN;\n")
	b.WriteString(fmt.Sprintf("DELETE FROM fb_odds WHERE fixture_id IN (%s,%s);\n", ups2, pasts2))
	b.WriteString(fmt.Sprintf("DELETE FROM fb_source_probs WHERE fixture_id IN (%s);\n", ups2))
	b.WriteString(fmt.Sprintf("DELETE FROM fb_predictions WHERE fixture_id IN (%s,%s);\n", ups2, pasts2))
	b.WriteString(fmt.Sprintf("DELETE FROM fb_verdicts WHERE fixture_id IN (%s,%s);\n", ups2, pasts2))
	b.WriteString("DELETE FROM fb_jackpot_tickets WHERE jackpot_id LIKE '6b1f0c1e-0000-4000-8000-%';\nDELETE FROM fb_jackpot_games WHERE jackpot_id LIKE '6b1f0c1e-0000-4000-8000-%';\nDELETE FROM fb_jackpots WHERE id LIKE '6b1f0c1e-0000-4000-8000-%';\n")
	b.WriteString("DELETE FROM fb_clubelo_ratings WHERE club IN ('Northbridge','Kingsport');\nDELETE FROM fb_clubelo_fixtures WHERE home = 'Northbridge';\n")
	b.WriteString(`UPDATE fb_free_settings SET enabled = (COALESCE(NULLIF(enabled,''),'{}')::jsonb || '{"clubelo":true,"polymarket":true,"apifootball_pred":true}'::jsonb)::text WHERE id = 1;` + "\n")
	d := seedDumper{t: t, db: db, b: &b}
	d.dump("fb_teams", "id BETWEEN 9300101 AND 9300299", false, "id")
	d.dump("fb_fixtures", "id IN ("+pasts2+")", false, "id")
	d.dump("fb_fixtures", "id IN ("+ups2+")", true, "id")
	d.dump("fb_odds", "fixture_id IN ("+pasts2+")", false, "")
	d.dump("fb_odds", "fixture_id IN ("+ups2+")", true, "")
	d.dump("fb_predictions", "fixture_id IN ("+pasts2+")", false, "fixture_id,kind")
	d.dump("fb_predictions", "fixture_id IN ("+ups2+")", true, "fixture_id,kind")
	d.dump("fb_source_probs", "fixture_id IN ("+ups2+")", true, "fixture_id,source")
	d.dump("fb_clubelo_ratings", "club IN ('Northbridge','Kingsport')", true, "club,date")
	d.dump("fb_clubelo_fixtures", "home = 'Northbridge'", true, "date,home,away")
	d.dump("fb_verdicts", "fixture_id IN ("+pasts2+")", false, "fixture_id")
	d.dump("fb_jackpots", "id LIKE '6b1f0c1e-0000-4000-8000-%'", false, "id")
	d.dump("fb_jackpot_games", "jackpot_id LIKE '6b1f0c1e-0000-4000-8000-%'", false, "jackpot_id,n")
	d.dump("fb_jackpot_tickets", "jackpot_id LIKE '6b1f0c1e-0000-4000-8000-%'", false, "id")
	b.WriteString("COMMIT;\n")
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("seed written: %s (%d bytes)", out, b.Len())
}

type seedDumper struct {
	t  *testing.T
	db *gorm.DB
	b  *strings.Builder
}

// dump writes rows as INSERT … ON CONFLICT (pk) DO UPDATE (pk "" = no
// conflict target, the id column is left to the sequence). shift writes
// timestamps relative to now() (T0 → now()).
func (d seedDumper) dump(table, where string, shift bool, pk string) {
	var cols []struct {
		Name string `gorm:"column:column_name"`
		Type string `gorm:"column:data_type"`
	}
	d.db.Raw(`SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? ORDER BY ordinal_position`, table).Scan(&cols)
	var rows []map[string]any
	d.db.Table(table).Where(where).Order("1").Find(&rows)
	t0 := seedT0.Format("2006-01-02 15:04:05+00")
	for _, r := range rows {
		names, vals := []string{}, []string{}
		for _, c := range cols {
			if pk == "" && c.Name == "id" {
				continue
			}
			names = append(names, c.Name)
			vals = append(vals, sqlLit(r[c.Name], c.Type, shift, t0))
		}
		stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(names, ","), strings.Join(vals, ","))
		if pk != "" {
			set := []string{}
			pks := map[string]bool{}
			for _, k := range strings.Split(pk, ",") {
				pks[k] = true
			}
			for _, n := range names {
				if !pks[n] {
					set = append(set, n+"=EXCLUDED."+n)
				}
			}
			stmt += " ON CONFLICT (" + pk + ") DO UPDATE SET " + strings.Join(set, ",")
		}
		d.b.WriteString(stmt + ";\n")
	}
}

func sqlLit(v any, typ string, shift bool, t0 string) string {
	if v == nil {
		return "NULL"
	}
	switch x := v.(type) {
	case time.Time:
		if strings.HasPrefix(typ, "date") {
			lit := x.Format("2006-01-02")
			if shift {
				return fmt.Sprintf("(DATE '%s' + (now()::date - DATE '%s'))", lit, t0[:10])
			}
			return "DATE '" + lit + "'"
		}
		lit := x.UTC().Format("2006-01-02 15:04:05.999999+00")
		if shift {
			return fmt.Sprintf("(TIMESTAMPTZ '%s' + (now() - TIMESTAMPTZ '%s'))", lit, t0)
		}
		return "TIMESTAMPTZ '" + lit + "'"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64, int32, int, int16:
		return fmt.Sprint(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 64)
	case []byte:
		return quoteSQL(string(x))
	case string:
		return quoteSQL(x)
	}
	return quoteSQL(fmt.Sprint(v))
}

func quoteSQL(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
