package football

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

var jpNow = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

const jpText = `1. GKS PIAST GLIWICE - KORONA KELCE 2.44 3.35 2.55
2. CHROBRY GLOGOW - PUSZCZA NIEPOLOMICE 2.19 3.20 2.90
3. LECHIA ZIELONA GORA - CHOJNICE 2.40 3.55 2.36
4. KAPFENBERG - WSC HERTHA WELS 2.60 3.50 2.25
5. ARMINIA BIELEFELD - MAGDEBURG 2.20 3.55 3.00
6. KOLDING IF - AAB 2.55 3.30 2.36
7. FARUL - UNIVERSITATEA CLUJ 2.60 3.30 2.37
8. CLUB BRUGGE U21 - VIRTON 2.23 3.40 2.65
9. HELMOND SPORT - DORDRECHT 2.20 3.50 2.80
10. ALMERE CITY - NAC BREDA 2.65 3.75 2.20
11. AUSTRIA SALZBURG - ADMIRA WACKER 2.30 3.40 2.60
12. MIEDZ LEGNICA - LKS LODZ 2.22 3.40 2.70
13. CLIFTONVILLE BELFAST - GLENTORAN 2.65 3.40 2.28`

// seedJackpotWorld loads testdata/free/seed_jackpot.sql (also used by
// /home/claude/tools/seed-fbfree.sh for the API flow).
func seedJackpotWorld(t *testing.T, db *gorm.DB) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "free", "seed_jackpot.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range strings.Split(string(b), ";\n") {
		st = strings.TrimSpace(st)
		if st == "" || strings.HasPrefix(st, "--") && !strings.Contains(st, "INSERT") {
			continue
		}
		if err := db.Exec(st).Error; err != nil {
			t.Fatalf("%v: %s", err, st)
		}
	}
}

func fixtureByID(t *testing.T, db *gorm.DB, id int64) Fixture {
	t.Helper()
	var f Fixture
	if err := db.Where("id = ?", id).First(&f).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func freeSvc(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db := testDB(t)
	s := newSvc(t, db, jpNow)
	s.Sleep = func(time.Duration) {}
	return s, db
}

// fakeFree is a test server for the free sources (records requests).
type fakeFree struct {
	mu    sync.Mutex
	srv   *httptest.Server
	calls []string
	hdr   []http.Header
	h     map[string]func(w http.ResponseWriter, r *http.Request)
}

func newFakeFree(t *testing.T) *fakeFree {
	f := &fakeFree{h: map[string]func(w http.ResponseWriter, r *http.Request){}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.URL.RequestURI())
		f.hdr = append(f.hdr, r.Header.Clone())
		h, ok := f.h[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFree) file(t *testing.T, path, name, ctype string) {
	b := freeSample(t, name)
	f.h[path] = func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", ctype); w.Write(b) }
}

func (f *fakeFree) point(s *Service) {
	b := s.bases()
	b.OddsPapi, b.ClubElo, b.FDCOUK, b.FDOrg, b.GNews, b.GDELT = f.srv.URL, f.srv.URL, f.srv.URL, f.srv.URL, f.srv.URL, f.srv.URL
}

func enableFree(t *testing.T, s *Service, budgets map[string]int) {
	t.Helper()
	op, fo := "op-test-key-0001", "fdorg-test-key-01"
	en := map[string]bool{}
	for _, p := range FreeProviders {
		en[p] = true
	}
	if err := s.SaveSources(SourcesPatch{OddsPapiKey: &op, FDOrgKey: &fo, Enabled: en, Budgets: budgets}, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestFreeCallBudgetsLogAndKeys(t *testing.T) {
	s, db := freeSvc(t)
	f := newFakeFree(t)
	f.point(s)
	f.file(t, "/Fixtures", "ce_fixtures.csv", "text/csv")
	f.h["/v4/sports"] = func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[{"sportId":10,"sportName":"Soccer"}]`)) }
	f.h["/v4/competitions/PL/standings"] = func(w http.ResponseWriter, r *http.Request) { w.Write(freeSample(t, "fdo_standings.json")) }
	ctx := context.Background()
	// Keyed sources refuse without a key.
	if _, err := s.callFree(ctx, freeSpec{Provider: ProviderOddsPapi, Path: "/v4/sports", Task: "t"}); !IsKind(err, "no_key") {
		t.Fatalf("no key: %v", err)
	}
	enableFree(t, s, map[string]int{ProviderClubElo: 2, ProviderOddsPapi: 3})
	// The keys are stored encrypted and shown masked.
	var st FreeSettings
	db.First(&st)
	if strings.Contains(st.OddsPapiKey, "op-test-key") || st.OddsPapiKey == "" {
		t.Fatal("key not encrypted")
	}
	if k, _ := mail.DecryptSecret(st.OddsPapiKey); k != "op-test-key-0001" {
		t.Fatal("decrypt")
	}
	// ClubElo: 2 a day, then refused (budget).
	for i := 0; i < 2; i++ {
		if _, err := s.callFree(ctx, freeSpec{Provider: ProviderClubElo, Path: "/Fixtures", Prio: PrioCritical, Task: "ce test"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.callFree(ctx, freeSpec{Provider: ProviderClubElo, Path: "/Fixtures", Prio: PrioCritical, Task: "ce test"}); !IsKind(err, "budget") {
		t.Fatalf("budget: %v", err)
	}
	var logs []FetchLog
	db.Where("provider = ?", ProviderClubElo).Find(&logs)
	if len(logs) != 2 || logs[0].Cost != 1 || !logs[0].OK || logs[0].Endpoint != "Fixtures" {
		t.Fatalf("log %+v", logs)
	}
	// OddsPapi: monthly budget (3); the key goes as apiKey and is redacted from the log.
	for i := 0; i < 3; i++ {
		if _, err := s.callFree(ctx, freeSpec{Provider: ProviderOddsPapi, Path: "/v4/sports", Prio: PrioCritical, Task: "op test"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.callFree(ctx, freeSpec{Provider: ProviderOddsPapi, Path: "/v4/sports", Prio: PrioCritical, Task: "op test"}); !IsKind(err, "budget") {
		t.Fatalf("monthly budget: %v", err)
	}
	if !strings.Contains(f.calls[2], "apiKey=op-test-key-0001") {
		t.Fatalf("apiKey not sent: %v", f.calls)
	}
	db.Where("provider = ?", ProviderOddsPapi).Find(&logs)
	for _, l := range logs {
		if strings.Contains(l.Params, "op-test-key") {
			t.Fatal("key in the log")
		}
	}
	if q := s.quotaRow(ProviderOddsPapi, monthPeriod(jpNow)); q.Used != 3 {
		t.Fatalf("month usage %+v", q)
	}
	u := s.FreeUsage(ProviderOddsPapi)
	if u.Period != "month" || u.Used != 3 || u.Budget != 3 || !u.HasKey || u.KeyHint != "••••0001" {
		t.Fatalf("usage %+v", u)
	}
	// Low priorities are paced over the rest of the month.
	s.SaveSources(SourcesPatch{Budgets: map[string]int{ProviderOddsPapi: 230}}, "test")
	if ok, why := s.Allow(ProviderOddsPapi, 1, PrioNormal); ok || !strings.Contains(why, "daily pace") {
		// 227 left over 11 days ≈ 21/day; 3 used today → allowed. Use many to exceed.
		_ = why
	}
	db.Model(&Quota{}).Where("provider = ? AND period = ?", ProviderOddsPapi, dayPeriod(jpNow)).Update("used", 40)
	if ok, why := s.Allow(ProviderOddsPapi, 1, PrioNormal); ok || !strings.Contains(why, "daily pace") {
		t.Fatalf("pace: %v %s", ok, why)
	}
	if ok, _ := s.Allow(ProviderOddsPapi, 1, PrioHigh); !ok {
		t.Fatal("high priority may use the reserve")
	}
	// football-data.org: X-Auth-Token header.
	if _, err := s.callFree(ctx, freeSpec{Provider: ProviderFDOrg, Path: "/v4/competitions/PL/standings", Prio: PrioCritical, Task: "fo"}); err != nil {
		t.Fatal(err)
	}
	if f.hdr[len(f.hdr)-1].Get("X-Auth-Token") != "fdorg-test-key-01" {
		t.Fatal("X-Auth-Token")
	}
	// A quota error from OddsPapi backs off until the next month.
	f.h["/v4/odds"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"Monthly request limit exceeded"}`))
	}
	db.Model(&Quota{}).Where("provider = ?", ProviderOddsPapi).Update("used", 0)
	if _, err := s.callFree(ctx, freeSpec{Provider: ProviderOddsPapi, Path: "/v4/odds", Prio: PrioCritical, Task: "q"}); !IsKind(err, "quota") {
		t.Fatalf("quota: %v", err)
	}
	if ok, why := s.Allow(ProviderOddsPapi, 1, PrioCritical); ok || !strings.Contains(why, "backing off until 2026-12-01") {
		t.Fatalf("backoff: %s", why)
	}
	// Free connection tests.
	s.ClearBackoff(ProviderOddsPapi)
	if r := s.TestFreeSource(ctx, ProviderFDOrg); !r.OK {
		t.Fatalf("test fdorg %+v", r)
	}
	if r := s.TestFreeSource(ctx, ProviderGDELT); r.OK || r.Code != "not_found" {
		t.Fatalf("test gdelt %+v", r)
	}
}

func TestClubEloFetchMapAndSection(t *testing.T) {
	s, db := freeSvc(t)
	seedJackpotWorld(t, db)
	f := newFakeFree(t)
	f.point(s)
	f.file(t, "/2026-11-20", "ce_ratings.csv", "text/csv")
	f.file(t, "/Fixtures", "ce_fixtures.csv", "text/csv")
	enableFree(t, s, nil)
	ctx := context.Background()
	n, err := s.fetchClubEloRatings(ctx, jpNow, PrioNormal, "ce:ratings")
	if err != nil || n != 12 {
		t.Fatal(n, err)
	}
	m := s.clubEloTeamMap()
	if m["Piast"] != 9100201 || m["Korona"] != 9100202 || m["Arminia"] != 9100209 || m["Magdeburg"] != 9100210 || m["Cliftonville"] != 9100225 {
		t.Fatalf("mapping %v", m)
	}
	if _, ok := m["Brugge"]; ok {
		// Belgian clubs: "Club Brugge KV" (senior) exists → Brugge maps there, never to the U21 side.
		if m["Brugge"] == 9100215 {
			t.Fatal("Brugge mapped to the U21 team")
		}
	}
	if n, err := s.fetchClubEloFixtures(ctx, PrioNormal, "ce:fixtures"); err != nil || n != 3 {
		t.Fatal(n, err)
	}
	fx := fixtureByID(t, db, 9100101)
	ce := s.clubEloFor(fx)
	if !ce.Available || ce.ProbsSource != "clubelo_fixtures" || !near(ce.Probs["draw"], 0.274, 0.001) || ce.Home.Club != "Piast" || ce.EloDiff == nil {
		t.Fatalf("section %+v", ce)
	}
	// A match ClubElo doesn't list: computed from the ratings.
	fx = fixtureByID(t, db, 9100105)
	db.Exec("DELETE FROM fb_clubelo_fixtures WHERE home = 'Arminia'")
	ce = s.clubEloFor(fx)
	if !ce.Available || ce.ProbsSource != "elo_formula" || ce.Probs["home"] <= ce.Probs["away"] {
		t.Fatalf("formula %+v", ce)
	}
	// A team ClubElo doesn't cover.
	fx = fixtureByID(t, db, 9100104)
	if ce = s.clubEloFor(fx); ce.Available || !strings.Contains(ce.Note, "Kapfenberg") {
		t.Fatalf("uncovered %+v", ce)
	}
	// A manual alias maps a club.
	if _, err := s.SetAliasFor("Almere", 9100219, ProviderClubElo); err != nil {
		t.Fatal(err)
	}
	if s.clubEloTeamMap()["Almere"] != 9100219 {
		t.Fatal("alias")
	}
}

func TestFDCOUKSeedMergeAndDedupe(t *testing.T) {
	s, db := freeSvc(t)
	seedJackpotWorld(t, db)
	rows, err := ParseFDCSV(freeSample(t, "fd_new_POL.csv"), "POL", 0)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.storeFDRows(rows, 106)
	if err != nil {
		t.Fatal(err)
	}
	// 2024 Piast–Korona: no API-Football fixture → seeded; 2025 Korona–Piast
	// exists (9100161) → merged; Lech–Legia: both teams known in the Ekstraklasa → seeded.
	if r.Matches != 3 || r.Merged != 1 || r.Inserted != 2 || r.Odds != 18 {
		t.Fatalf("result %+v", r)
	}
	var seeded []Fixture
	db.Where("id < 0").Find(&seeded)
	if len(seeded) != 2 || seeded[0].Status != "FT" || seeded[0].Round != "football-data.co.uk" {
		t.Fatalf("seeded %+v", seeded)
	}
	var h HistMatch
	db.Where("home = 'Korona Kielce' AND season = 2025").First(&h)
	if h.FixtureID != 9100161 || h.HomeID != 9100202 || h.AwayID != 9100201 || *h.PSCH != 2.61 {
		t.Fatalf("hist %+v", h)
	}
	// Closing prices stamped a minute before kickoff (the closing line).
	var o Odd
	db.Where("fixture_id = 9100161 AND source = ? AND bookmaker = 'pinnacle' AND selection = 'home'", ProviderFDCOUK).First(&o)
	if o.Price != 2.61 || !o.FetchedAt.Equal(fixtureByID(t, db, 9100161).Kickoff.Add(-time.Minute)) {
		t.Fatalf("closing %+v", o)
	}
	var f Fixture
	db.Where("id = 9100161").First(&f)
	mv := s.marketView(f, &f.Kickoff, "")
	if mv.FairSource != "Pinnacle (de-margined)" || len(mv.Fair1X2) != 3 {
		t.Fatalf("closing market %+v", mv)
	}
	// Idempotent.
	r, _ = s.storeFDRows(rows, 106)
	if c := count(t, db, "fb_hist_matches"); c != 3 || r.Inserted != 0 {
		t.Fatalf("again %d %+v", c, r)
	}
	// API-Football later has the 2024 match: the seeded one is merged away.
	var sd Fixture
	db.Where("id < 0 AND home_id = 9100201").First(&sd)
	db.Exec(`INSERT INTO fb_fixtures (id,competition_id,season,kickoff,status,home_id,away_id,ft_home,ft_away,fetched_at,updated_at) VALUES (9100170,106,2024,?, 'FT',9100201,9100202,1,1,now(),now())`, sd.Kickoff.Add(30*time.Minute))
	if n := s.dedupeSeeded(); n != 1 {
		t.Fatalf("dedupe %d", n)
	}
	if count(t, db, "fb_fixtures WHERE id = "+tid(sd.ID)) != 0 {
		t.Fatal("seeded fixture kept")
	}
	var h2 HistMatch
	db.Where("home = 'Piast Gliwice' AND season = 2024").First(&h2)
	if h2.FixtureID != 9100170 {
		t.Fatalf("hist relinked %+v", h2)
	}
	var n int64
	db.Model(&Odd{}).Where("fixture_id = 9100170 AND source = ?", ProviderFDCOUK).Count(&n)
	if n != 6 {
		t.Fatalf("odds moved %d", n)
	}
	// The seeded results feed the model's history and the source check's table.
	if ms := s.modelMatches(106, 2026, jpNow); len(ms) < 11 {
		t.Fatalf("model matches %d", len(ms))
	}
	// SeedHistory: files via the planner targets (E0 for the enabled Premier League).
	ff := newFakeFree(t)
	ff.point(s)
	ff.file(t, "/mmz4281/2627/E0.csv", "fd_E0.csv", "text/csv")
	ff.file(t, "/new/POL.csv", "fd_new_POL.csv", "text/csv")
	enableFree(t, s, nil)
	res, err := s.SeedHistory(context.Background(), 0, 2)
	if err != nil || res.Files != 2 {
		t.Fatalf("seed %+v %v (%v)", res, err, ff.calls)
	}
	var ts TaskState
	db.Where("key = 'fd:main:E0:2025'").First(&ts)
	if ts.Status != "ok" || ts.Note != "no file" {
		t.Fatalf("missing season file %+v", ts)
	}
}

func seedSquadNews(t *testing.T, s *Service) {
	t.Helper()
	gn, _ := ParseRSS(freeSample(t, "gnews.rss"), ProviderGNews, "pl")
	s.storeNews(gn, []newsTeam{{ID: 9100201, Name: "Piast Gliwice", Country: "Poland"}}, true)
}

func TestNewsFactsConfidenceReviewsExpiry(t *testing.T) {
	s, db := freeSvc(t)
	seedJackpotWorld(t, db)
	var fx Fixture
	db.Where("id = 9100101").First(&fx)
	// 1. Google News: one site says Félix is injured → reported.
	seedSquadNews(t, s)
	if c := count(t, db, "fb_news"); c != 2 {
		t.Fatalf("news %d", c)
	}
	facts := s.activeFacts(9100201, "")
	if len(facts) != 1 || facts[0].PlayerID != 9105003 || facts[0].Status != TNInjured || facts[0].confirmed || facts[0].usedInModel() {
		t.Fatalf("reported %+v", facts)
	}
	if ids := s.newsMissing(fx, 9100201, ""); len(ids) != 0 {
		t.Fatal("reported fact used by the model")
	}
	// The same items again: no duplicates; a near-identical headline on the same site is not a new item.
	seedSquadNews(t, s)
	s.storeNews([]RawNews{{Title: "Piast bez Félixa! Kontuzja wyklucza go z meczu z Koroną", URL: "https://www.sport.pl/pilka/other-url", Domain: "sport.pl", PublishedAt: jpNow.Add(-3 * time.Hour), Provider: ProviderGNews, Language: "pl"}},
		[]newsTeam{{ID: 9100201, Name: "Piast Gliwice"}}, true)
	if c := count(t, db, "fb_news"); c != 2 {
		t.Fatalf("dedupe %d", c)
	}
	// 2. GDELT: a second, independent site → confirmed (2 sites within 7 days) → in the model.
	gd, _ := ParseGDELT(freeSample(t, "gdelt.json"))
	s.storeNews(gd, []newsTeam{{ID: 9100201, Name: "Piast Gliwice", Country: "Poland"}}, true)
	facts = s.activeFacts(9100201, "")
	if len(facts) != 1 || !facts[0].confirmed || facts[0].confirmedBy != "sources" || facts[0].domains7 != 2 || !facts[0].usedInModel() {
		t.Fatalf("confirmed %+v", facts)
	}
	ResetModelCache()
	if ids := s.newsMissing(fx, 9100201, ""); len(ids) != 1 || ids[0] != 9105003 {
		t.Fatalf("missing %v", ids)
	}
	adj, out := s.availability(fx)
	if !containsInt(out.HomeMissing, 9105003) || adj.HomeAttack >= 1 {
		t.Fatalf("availability %+v %+v", adj, out)
	}
	// 3. A club feed (Korona): Błanik injured (reported) and a coach rumour.
	at, _ := ParseRSS(freeSample(t, "feed_atom.xml"), ProviderFeeds, "pl")
	s.storeNews(at, []newsTeam{{ID: 9100202, Name: "Korona Kielce"}}, false)
	kf := s.activeFacts(9100202, "")
	if len(kf) != 2 {
		t.Fatalf("korona facts %+v", kf)
	}
	var blanik, coach factState
	for _, f := range kf {
		if f.Role == "coach" {
			coach = f
		} else {
			blanik = f
		}
	}
	if blanik.PlayerID != 9105104 || blanik.confirmed || coach.Status != TNCoach {
		t.Fatalf("facts %+v %+v", blanik, coach)
	}
	// 4. A manager of org A confirms Błanik: confirmed for org A only (audited).
	f1, a1, err := s.ReviewFact("orgA", "uA", "Michael Thomas", "admin", blanik.ID, "confirm", "club website")
	if err != nil || f1.Confidence != "confirmed" || *f1.ConfirmedBy != "manager" || !f1.UsedInModel || a1.Scope != "organisation" {
		t.Fatalf("review %+v %+v %v", f1, a1, err)
	}
	if fs := s.activeFacts(9100202, "orgB"); fs[0].confirmed && fs[0].ID == blanik.ID || fs[1].confirmed && fs[1].ID == blanik.ID {
		t.Fatal("org B sees org A's confirmation")
	}
	if ids := s.newsMissing(fx, 9100202, "orgA"); len(ids) != 1 {
		t.Fatalf("org A missing %v", ids)
	}
	if ids := s.newsMissing(fx, 9100202, ""); len(ids) != 0 {
		t.Fatal("global prediction changed by an organisation")
	}
	if !s.orgChangesMissing(fx, "orgA") || s.orgChangesMissing(fx, "orgB") {
		t.Fatal("org adjustment detection")
	}
	// The org's analysis is recomputed; the shared one isn't.
	aA, err := s.Analysis(fx.ID, "orgA", "uA")
	if err != nil || aA.Model == nil || !aA.Model.OrgAdjusted || aA.TeamNews.Counts.UsedInModel != 2 {
		t.Fatalf("org analysis %v %+v", err, aA.TeamNews)
	}
	aB, _ := s.Analysis(fx.ID, "orgB", "uB")
	if aB.Model.OrgAdjusted || aB.TeamNews.Counts.UsedInModel != 1 || len(aB.News.Items) == 0 || aB.SourceCheck == nil || aB.ClubElo == nil || aB.KenyanOdds == nil {
		t.Fatalf("org B analysis %+v", aB.TeamNews)
	}
	// Dismiss the coach rumour (org A), then clear.
	if f2, _, _ := s.ReviewFact("orgA", "uA", "Michael Thomas", "manager", coach.ID, "dismiss", "speculation"); !f2.Dismissed || f2.Review == nil {
		t.Fatalf("dismiss %+v", f2)
	}
	if f2, _, _ := s.ReviewFact("orgA", "uA", "Michael Thomas", "manager", coach.ID, "clear", ""); f2.Dismissed {
		t.Fatal("clear")
	}
	if aud := s.FactAudit("orgA", "admin", coach.ID); len(aud) != 2 || aud[0].Action != "clear" {
		t.Fatalf("audit %+v", aud)
	}
	if aud := s.FactAudit("orgB", "admin", coach.ID); len(aud) != 0 {
		t.Fatal("audit visible to another org")
	}
	if _, _, err := s.ReviewFact("orgA", "uA", "x", "admin", blanik.ID, "maybe", ""); err == nil {
		t.Fatal("bad action")
	}
	// 5. A super admin dismisses Félix platform-wide: not used anywhere.
	if f3, a3, _ := s.ReviewFact("orgP", "uS", "Kennedy", "super_admin", facts[0].ID, "dismiss", "wrong player"); !f3.Dismissed || a3.Scope != "platform" {
		t.Fatalf("platform dismiss %+v", f3)
	}
	if ids := s.newsMissing(fx, 9100201, "orgB"); len(ids) != 0 {
		t.Fatal("dismissed fact used")
	}
	// 6. API-Football's injury list confirms Błanik for everyone.
	db.Exec(`INSERT INTO fb_injuries (fixture_id, player_id, team_id, competition_id, season, player_name, type, reason, status, fetched_at) VALUES (9100101, 9105104, 9100202, 106, 2026, 'D. Błanik', 'Missing Fixture', 'Knee Injury', 'injured', now())`)
	for _, f := range s.activeFacts(9100202, "orgB") {
		if f.ID == blanik.ID && (!f.confirmed || f.confirmedBy != ProviderAPIFootball) {
			t.Fatalf("AF confirmation %+v", f)
		}
	}
	// Reliability: sport.pl's fact was later dismissed; echodnia's confirmed.
	rel := map[string]NewsSourceStat{}
	for _, r := range s.SourceReliability() {
		rel[r.Domain] = r
	}
	if rel["echodnia.eu"].Confirmed != 1 || rel["sport.pl"].Dismissed != 1 || rel["sport.pl"].Reliability >= rel["echodnia.eu"].Reliability {
		t.Fatalf("reliability %+v", rel)
	}
	// 7. Expiry: 15 days later nothing is active (and a new report starts afresh).
	s.SetClock(jpNow.Add(15 * 24 * time.Hour))
	if fs := s.activeFacts(9100202, ""); len(fs) != 0 {
		t.Fatalf("expired facts %+v", fs)
	}
	s.storeNews([]RawNews{{Title: "Korona: Błanik znów kontuzjowany", URL: "https://echodnia.eu/blanik-znow", Domain: "echodnia.eu", PublishedAt: jpNow.Add(15 * 24 * time.Hour), Provider: ProviderFeeds}},
		[]newsTeam{{ID: 9100202, Name: "Korona Kielce"}}, false)
	fs := s.activeFacts(9100202, "")
	if len(fs) != 1 || len(fs[0].NewsIDs) != 1 || fs[0].AdminAction != "" {
		t.Fatalf("renewed %+v", fs)
	}
}

func TestSourceCheckDB(t *testing.T) {
	s, db := freeSvc(t)
	seedJackpotWorld(t, db)
	var fx Fixture
	db.Where("id = 9100101").First(&fx)
	// football-data.org has Piast one place lower (7th) with the same points; Korona 10th with 2 more points.
	db.Create(&FDOrgStanding{Competition: "XX", FDTeamID: 1, Team: "Piast Gliwice", Position: 7, Points: 24, Played: 16, GF: 21, GA: 18, TeamID: 9100201, FetchedAt: jpNow})
	db.Create(&FDOrgStanding{Competition: "XX", FDTeamID: 2, Team: "Korona Kielce", Position: 10, Points: 21, Played: 16, GF: 17, GA: 22, TeamID: 9100202, FetchedAt: jpNow})
	FDOrgCompetitions["XX"] = 106
	defer delete(FDOrgCompetitions, "XX")
	db.Create(&OPFixture{ID: "id1", Start: fx.Kickoff.Add(10 * time.Minute), Home: "Piast Gliwice", Away: "Korona Kielce", FixtureID: fx.ID, FetchedAt: jpNow})
	sc := s.SourceCheckFor(fx)
	by := map[string]SCItem{}
	for _, it := range sc.Items {
		by[it.Key] = it
	}
	if by["home.position"].Status != "minor" || by["home.points"].Status != "agree" || by["away.points"].Status != "conflict" {
		t.Fatalf("standings %+v %+v %+v", by["home.position"], by["home.points"], by["away.points"])
	}
	if by["kickoff"].Status != "minor" || by["home.last5"].Value != "WDLWW" || by["h2h.results"].Status != "single" {
		t.Fatalf("kickoff/form %+v %+v %+v", by["kickoff"], by["home.last5"], by["h2h.results"])
	}
	if by["home.identity"].Status != "agree" || sc.Summary.Status != "conflict" || sc.Sections["standings"] != "conflict" || sc.Sections["kickoff"] != "minor" {
		t.Fatalf("summary %+v %+v", sc.Summary, sc.Sections)
	}
	// News about a postponement is flagged.
	s.storeNews([]RawNews{{Title: "Mecz Piast Gliwice – Korona Kielce przełożony", URL: "https://x.pl/p", Domain: "x.pl", PublishedAt: jpNow.Add(-time.Hour), Provider: ProviderGNews, Language: "pl"}},
		[]newsTeam{{ID: 9100201, Name: "Piast Gliwice"}}, true)
	sc = s.SourceCheckFor(fx)
	found := false
	for _, it := range sc.Items {
		if it.Key == "kickoff.news" && it.Status == "conflict" {
			found = true
		}
	}
	if !found {
		t.Fatal("postponement not flagged")
	}
}

func TestOddsPapiKenyanOdds(t *testing.T) {
	s, db := freeSvc(t)
	seedJackpotWorld(t, db)
	f := newFakeFree(t)
	f.point(s)
	f.file(t, "/v4/fixtures", "op_fixtures.json", "application/json")
	f.h["/v4/odds-by-tournaments"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("bookmaker") == "pinnacle" {
			w.Write(freeSample(t, "op_odds_pinnacle.json"))
			return
		}
		w.Write(freeSample(t, "op_odds_by_tournaments.json"))
	}
	enableFree(t, s, nil)
	ctx := context.Background()
	n, matched, err := s.fetchOPFixtures(ctx, jpNow, jpNow.Add(48*time.Hour), PrioHigh, "op:fixtures")
	if err != nil || n != 2 || matched != 1 {
		t.Fatalf("fixtures %d %d %v", n, matched, err)
	}
	q := f.calls[0]
	if !strings.Contains(q, "sportId=10") || !strings.Contains(q, "bookmakers=betika%2Cpinnacle") || !strings.Contains(q, "hasOdds=true") {
		t.Fatalf("fixtures query %s", q)
	}
	var op OPFixture
	db.Where("fixture_id = 9100101").First(&op)
	if op.ID != "id1000003969653792" || op.TournamentID != 202 {
		t.Fatalf("op fixture %+v", op)
	}
	// Targets: the jackpot / top competition fixtures.
	tg := s.opTargets(jpNow)
	if len(tg) != 0 {
		// No jackpot or watchlist yet and the jackpot leagues aren't top priority.
		t.Fatalf("targets %+v", tg)
	}
	for _, bk := range []string{"betika", "pinnacle"} {
		if _, err := s.fetchOPOdds(ctx, bk, []int64{202}, PrioHigh, "op:odds:"+bk); err != nil {
			t.Fatal(err)
		}
	}
	var fx Fixture
	db.Where("id = 9100101").First(&fx)
	mv := s.marketView(fx, nil, "")
	if mv.FairSource != "Pinnacle (de-margined)" {
		t.Fatalf("fair %s", mv.FairSource)
	}
	ko := s.kenyanOddsFor(fx, mv)
	if !ko.Available || len(ko.Bookmakers) != 1 || ko.Bookmakers[0].Key != "betika" || !ko.Bookmakers[0].Kenyan || ko.Bookmakers[0].Prices[SelHome] != 2.44 ||
		ko.Bookmakers[0].VsFair[SelHome] >= 0 || ko.Best[SelDraw].Price != 3.35 || ko.FairSource != "Pinnacle (de-margined)" {
		t.Fatalf("kenyan %+v", ko)
	}
	var op2 OPFixture
	db.Where("fixture_id = 9100101").First(&op2)
	if op2.OddsAt == nil {
		t.Fatal("odds_at")
	}
	// Prices are history: the same prices again add no rows.
	before := count(t, db, "fb_odds")
	s.fetchOPOdds(ctx, "betika", []int64{202}, PrioHigh, "op:odds:betika")
	if count(t, db, "fb_odds") != before {
		t.Fatal("unchanged prices duplicated")
	}
}

func TestJackpotLifecycleIsolationGrading(t *testing.T) {
	s, db := freeSvc(t)
	seedJackpotWorld(t, db)
	// Game 12's fixture is unknown (only the jackpot's odds then).
	db.Exec("DELETE FROM fb_fixtures WHERE id = 9100112")
	text := jpText
	prize, price := 22500447.0, 99.0
	name, book := "SportPesa Midweek 20 Nov", "SportPesa"
	closes := time.Date(2026, 11, 20, 16, 45, 0, 0, time.UTC)
	in := JackpotInput{Name: &name, Bookmaker: &book, Prize: &prize, TicketPrice: &price, Text: &text, ClosesAt: &closes,
		Bonuses: []BonusTier{{12, 250000}, {11, 25000}, {10, 2500}}}
	v, err := s.CreateJackpot("orgA", "uA", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Games) != 13 || v.Jackpot.Currency != "KES" || v.Jackpot.Status != "open" {
		t.Fatalf("created %+v", v.Jackpot)
	}
	matched := 0
	for _, g := range v.Games {
		if g.Match.Status == "matched" {
			matched++
		}
	}
	if matched != 12 || v.Games[11].Match.Status != "unmatched" || *v.Games[7].FixtureID != 9100108 || *v.Games[10].FixtureID != 9100111 || *v.Games[2].FixtureID != 9100103 {
		for _, g := range v.Games {
			t.Logf("%d %s v %s → %+v", g.N, g.Home, g.Away, g.Match)
		}
		t.Fatalf("matched %d", matched)
	}
	// No other sources stored yet: the jackpot odds alone → 1 1 2 2 1 2 2 1 1 2 1 1 2, 1 in 166,222.
	a := v.Analysis
	if strings.Join(a.MostLikely.Picks, " ") != "1 1 2 2 1 2 2 1 1 2 1 1 2" || !near(a.MostLikely.OneIn, 166222, 1) {
		t.Fatalf("most likely %v 1 in %v", a.MostLikely.Picks, a.MostLikely.OneIn)
	}
	if len(a.TopTickets) != 10 || a.TopTickets[1].Changes[0].N != 3 || len(a.Cover) == 0 || a.Cover[0].Game != 3 || a.Cover[0].Cost != 198 ||
		len(a.AtLeast) != 4 || a.AtLeast[0].K != 13 || !near(a.ExpectedDraws, 3.43, 0.01) || a.EV.TicketPrice != 99 || len(a.Contrarian) == 0 {
		t.Fatalf("analysis %+v", a)
	}
	if a.Games[0].Sources[1].Used || a.Games[0].Sources[1].Why == "" || a.Games[0].Sources[0].Weight != 1 {
		t.Fatalf("sources %+v", a.Games[0].Sources)
	}
	if len(v.Tickets) != 1 || v.Tickets[0].Name != "Most likely" || v.Tickets[0].Combinations != 1 {
		t.Fatalf("auto ticket %+v", v.Tickets)
	}
	id := v.Jackpot.ID
	// Isolation: another organisation and another person in the same organisation see nothing.
	if _, err := s.JackpotViewOf("orgB", "uB", id, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal("org isolation")
	}
	if _, err := s.JackpotViewOf("orgA", "uA2", id, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal("person isolation")
	}
	if _, err := s.UpdateJackpot("orgB", "uB", id, JackpotInput{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Fatal("update isolation")
	}
	if err := s.DeleteJackpot("orgB", "uB", id); !errors.Is(err, ErrNotFound) {
		t.Fatal("delete isolation")
	}
	if rows, n := s.ListJackpots("orgB", "uB", "", 0, 0); n != 0 || len(rows) != 0 {
		t.Fatal("list isolation")
	}
	rows, n := s.ListJackpots("orgA", "uA", "open", 0, 0)
	if n != 1 || rows[0].MatchedGames != 12 || rows[0].MostLikely.OneIn != 166222 || rows[0].Tickets != 1 {
		t.Fatalf("list %+v", rows)
	}
	// Market prices for game 1 move its probabilities (Pinnacle 2.52 / 3.56 / 2.84).
	for sel, p := range map[string]float64{SelHome: 2.52, SelDraw: 3.56, SelAway: 2.84} {
		db.Create(&Odd{FixtureID: 9100101, Source: ProviderOddsPapi, Bookmaker: "pinnacle", BookmakerTitle: "Pinnacle", Market: Mkt1X2, Selection: sel, Price: p, FetchedAt: jpNow, CheckedAt: jpNow})
	}
	v, _ = s.JackpotViewOf("orgA", "uA", id, 5)
	g1 := v.Analysis.Games[0]
	// The list shows the page's (blended) figure, not the jackpot odds alone.
	if rows, _ := s.ListJackpots("orgA", "uA", "", 0, 0); rows[0].MostLikely.OneIn != v.Analysis.MostLikely.OneIn || rows[0].MostLikely.OneIn == 166222 {
		t.Fatalf("list %v page %v", rows[0].MostLikely.OneIn, v.Analysis.MostLikely.OneIn)
	}
	if !g1.Sources[1].Used || !near(g1.Sources[0].Weight+g1.Sources[1].Weight, 1, 1e-9) || g1.Sources[1].Label != "Pinnacle (de-margined)" || len(v.Analysis.TopTickets) != 5 {
		t.Fatalf("market source %+v", g1.Sources)
	}
	// Manual match for the unmatched game (a fixture added later), alias learned.
	db.Exec(`INSERT INTO fb_fixtures (id,competition_id,season,kickoff,status,home_id,away_id,fetched_at,updated_at) VALUES (9100112,107,2026,'2026-11-20 19:30:00+00','NS',9100223,9100224,now(),now())`)
	fid := int64(9100112)
	v, err = s.SetGameMatch("orgA", "uA", id, 12, &fid)
	if err != nil || v.Games[11].Match.Status != "manual" || *v.Games[11].FixtureID != 9100112 {
		t.Fatalf("manual %v %+v", err, v.Games[11])
	}
	var al TeamAlias
	if db.Where("alias = ?", normFree("LKS LODZ")).First(&al).Error != nil || al.TeamID != 9100224 || al.Provider != ProviderJackpotAlias {
		t.Fatalf("alias %+v", al)
	}
	// Tickets: a 2-double cover ticket; bad picks refused.
	picks := append([]string{}, v.Analysis.MostLikely.Picks...)
	picks[2], picks[0] = "12", "12"
	if v, err = s.AddTicket("orgA", "uA", id, "2 doubles", "cover", picks); err != nil || len(v.Tickets) != 2 || v.Tickets[1].Combinations != 4 || v.Tickets[1].Cost != 396 {
		t.Fatalf("ticket %v %+v", err, v.Tickets)
	}
	if _, err := s.AddTicket("orgA", "uA", id, "bad", "custom", []string{"1"}); err == nil {
		t.Fatal("short ticket")
	}
	if _, err := s.AddTicket("orgA", "uA", id, "bad", "custom", append(picks[:12:12], "3")); err == nil {
		t.Fatal("bad pick")
	}
	// Validation.
	bad := "A v B 2 3 4"
	if _, err := s.CreateJackpot("orgA", "uA", JackpotInput{Text: &bad}); err == nil {
		t.Fatal("one game accepted")
	}
	neg := -1.0
	if _, err := s.CreateJackpot("orgA", "uA", JackpotInput{Text: &text, Prize: &neg}); err == nil {
		t.Fatal("negative prize")
	}
	// Duplicate; unsaved analysis.
	d, err := s.DuplicateJackpot("orgA", "uA", id, "")
	if err != nil || d.Jackpot.ID == id || len(d.Games) != 13 || d.Jackpot.Status != "draft" || !strings.HasSuffix(d.Jackpot.Name, "(copy)") {
		t.Fatalf("duplicate %v %+v", err, d.Jackpot)
	}
	pv, err := s.AnalyseJackpot("orgB", JackpotInput{Text: &text})
	if err != nil || pv.Jackpot.ID != "" || pv.Analysis.NGames != 13 {
		t.Fatalf("preview %v", err)
	}
	if c := count(t, db, "fb_jackpots"); c != 2 {
		t.Fatalf("preview saved something: %d", c)
	}
	// After the games: results come in → graded automatically.
	s.SetClock(time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC))
	results := map[int64][2]int{9100101: {2, 1}, 9100102: {1, 1}, 9100103: {0, 1}, 9100104: {1, 2}, 9100105: {3, 0}, 9100106: {0, 0}, 9100107: {1, 2},
		9100108: {2, 2}, 9100109: {2, 0}, 9100110: {0, 1}, 9100111: {1, 0}, 9100112: {2, 1}}
	for fid, sc := range results {
		db.Exec(`UPDATE fb_fixtures SET status = 'FT', ft_home = ?, ft_away = ?, home_goals = ?, away_goals = ? WHERE id = ?`, sc[0], sc[1], sc[0], sc[1], fid)
	}
	db.Exec(`UPDATE fb_fixtures SET status = 'PST' WHERE id = 9100113`) // postponed: still waiting
	if n := s.GradeJackpots(); n != 2 {
		t.Fatalf("graded %d", n)
	}
	v, _ = s.JackpotViewOf("orgA", "uA", id, 0)
	if v.Grading == nil || v.Grading.Complete || v.Jackpot.Status == "graded" || v.Games[0].Result == nil || *v.Games[0].Result != "1" || *v.Games[0].ResultSource != ProviderAPIFootball {
		t.Fatalf("partial %+v", v.Grading)
	}
	// Game 13 abandoned → void; then complete.
	db.Exec(`UPDATE fb_fixtures SET status = 'ABD' WHERE id = 9100113`)
	s.GradeJackpots()
	v, _ = s.JackpotViewOf("orgA", "uA", id, 0)
	if v.Grading == nil || !v.Grading.Complete || v.Jackpot.Status != "graded" || !v.Games[12].Void {
		t.Fatalf("complete %+v", v.Grading)
	}
	// Most likely 1 1 2 2 1 2 2 1 1 2 1 1 (game 13 void): results 1 X 2 2 1 X 2 X 1 2 1 1 → 9 of 12.
	if v.Grading.CorrectMostLikely == nil || *v.Grading.CorrectMostLikely != 9 || v.Tickets[0].Correct == nil || *v.Tickets[0].Correct != 9 || v.Tickets[0].Of != 12 {
		t.Fatalf("grades %+v %+v", v.Grading, v.Tickets[0])
	}
	if *v.Tickets[1].Correct != 9 || v.Grading.BestTopN == nil {
		t.Fatalf("cover ticket %+v", v.Tickets[1])
	}
	// A manual result overrides (e.g. a corrected score).
	one := "X"
	v, _ = s.SetGameResult("orgA", "uA", id, 1, &one, false)
	if *v.Games[0].ResultSource != "manual" || *v.Tickets[0].Correct != 8 {
		t.Fatalf("manual result %+v", v.Tickets[0])
	}
	h := s.JackpotHistory("orgA", "uA", 0)
	if len(h["history"].([]JackpotSummary)) != 2 || h["stats"].(map[string]any)["best_correct"].(int) < 8 {
		t.Fatalf("history %+v", h["stats"])
	}
	// Update replaces games; delete removes everything.
	short := "Piast Gliwice v Korona Kielce 2.44 3.35 2.55\nArminia Bielefeld v Magdeburg 2.20 3.55 3.00"
	v, err = s.UpdateJackpot("orgA", "uA", d.Jackpot.ID, JackpotInput{Text: &short})
	if err != nil || len(v.Games) != 2 || v.Games[1].Match.Status != "matched" {
		t.Fatalf("update %v %+v", err, v.Games)
	}
	if err := s.DeleteJackpot("orgA", "uA", d.Jackpot.ID); err != nil || count(t, db, "fb_jackpot_games WHERE jackpot_id = '"+d.Jackpot.ID+"'") != 0 {
		t.Fatal("delete")
	}
}

// HTTP: permissions (staff can't review; super-admin routes), the
// envelope and org isolation through the API.
func TestFreeHandlersPermissions(t *testing.T) {
	s, db := freeSvc(t)
	seedJackpotWorld(t, db)
	seedSquadNews(t, s)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("organization_id", c.GetHeader("X-Org"))
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("user_role", c.GetHeader("X-Role"))
	})
	(&Handler{S: s}).RegisterRoutes(r.Group("/api/v1"))
	do := func(method, path, org, user, role, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Org", org)
		req.Header.Set("X-User", user)
		req.Header.Set("X-Role", role)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	facts := s.activeFacts(9100201, "")
	if len(facts) != 1 {
		t.Fatal("fact")
	}
	if code, out := do("POST", "/api/v1/football/team-news/"+facts[0].ID+"/review", "o1", "staff1", "staff", `{"action":"confirm"}`); code != 403 || out["error"].(map[string]any)["code"] != "FORBIDDEN" {
		t.Fatalf("staff review %d %v", code, out)
	}
	if code, out := do("POST", "/api/v1/football/team-news/"+facts[0].ID+"/review", "o1", "m1", "manager", `{"action":"confirm","note":"seen it"}`); code != 200 ||
		out["data"].(map[string]any)["fact"].(map[string]any)["confidence"] != "confirmed" {
		t.Fatalf("manager review %d %v", code, out)
	}
	if code, _ := do("GET", "/api/v1/super-admin/football/sources", "o1", "a1", "admin", ""); code != 403 {
		t.Fatalf("sources as admin %d", code)
	}
	code, out := do("PUT", "/api/v1/super-admin/football/sources", "o1", "s1", "super_admin", `{"oddspapi_key":"op-live-key-12345678","enabled":{"oddspapi":true},"budgets":{"oddspapi":200}}`)
	if code != 200 {
		t.Fatalf("save sources %d %v", code, out)
	}
	srcs := out["data"].(map[string]any)["sources"].([]any)
	op := srcs[0].(map[string]any)
	if op["key"] != "oddspapi" || op["key_hint"] != "••••5678" || op["has_key"] != true || op["budget"].(float64) != 200 || op["period"] != "month" {
		t.Fatalf("oddspapi row %v", op)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "op-live-key") {
		t.Fatal("key leaked")
	}
	if code, _ := do("PUT", "/api/v1/super-admin/football/sources", "o1", "s1", "super_admin", `{"budgets":{"oddspapi":-5}}`); code != 400 {
		t.Fatal("bad budget accepted")
	}
	// News rules: edit, test, reset.
	code, out = do("PUT", "/api/v1/super-admin/football/news-rules", "o1", "s1", "super_admin", `{"rules":[{"language":"pl","status":"injured","keywords":["kontuzja","uraz*"]}]}`)
	if code != 200 || len(out["data"].(map[string]any)["rules"].([]any)) != 1 || out["data"].(map[string]any)["defaults_changed"] != true {
		t.Fatalf("rules %d %v", code, out)
	}
	code, out = do("POST", "/api/v1/super-admin/football/news-rules/test", "o1", "s1", "super_admin", `{"text":"Błanik z urazem kolana","team_id":9100202}`)
	d := out["data"].(map[string]any)
	if code != 200 || d["language"] != "pl" || d["fact"].(map[string]any)["player_id"].(float64) != 9105104 {
		t.Fatalf("rules test %d %v", code, out)
	}
	if code, out = do("PUT", "/api/v1/super-admin/football/news-rules", "o1", "s1", "super_admin", `{"reset":true}`); code != 200 || len(out["data"].(map[string]any)["rules"].([]any)) != len(DefaultNewsRules()) {
		t.Fatal("reset")
	}
	// Feeds.
	if code, _ := do("POST", "/api/v1/super-admin/football/news-feeds", "o1", "s1", "super_admin", `{"url":"ftp://x","team_id":1}`); code != 400 {
		t.Fatal("bad feed")
	}
	code, out = do("POST", "/api/v1/super-admin/football/news-feeds", "o1", "s1", "super_admin", `{"url":"https://echodnia.eu/sport/rss","label":"Echo Dnia","language":"pl","team_id":9100202}`)
	if code != 200 || out["data"].(map[string]any)["enabled"] != true {
		t.Fatalf("feed %d %v", code, out)
	}
	// Jackpots through the API, isolated per person.
	body, _ := json.Marshal(map[string]any{"text": jpText, "prize": 22500447, "ticket_price": 99, "bookmaker": "SportPesa"})
	code, out = do("POST", "/api/v1/football/jackpots", "o1", "u1", "manager", string(body))
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	jid := out["data"].(map[string]any)["jackpot"].(map[string]any)["id"].(string)
	if code, _ := do("GET", "/api/v1/football/jackpots/"+jid, "o2", "u2", "manager", ""); code != 404 {
		t.Fatalf("other org %d", code)
	}
	if code, out = do("GET", "/api/v1/football/jackpots", "o1", "u1", "staff", ""); code != 200 || out["data"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("list %d %v", code, out)
	}
	code, out = do("POST", "/api/v1/football/jackpots/parse", "o1", "u1", "staff", `{"text":"1. GKS PIAST GLIWICE - KORONA KELCE 2.44 3.35 2.55\njunk","date":"2026-11-20"}`)
	g := out["data"].(map[string]any)["games"].([]any)[0].(map[string]any)
	if code != 200 || g["match"].(map[string]any)["status"] != "matched" || len(out["data"].(map[string]any)["skipped"].([]any)) != 1 {
		t.Fatalf("parse %d %v", code, out)
	}
	if code, out = do("GET", "/api/v1/football/fixtures/9100101/analysis", "o1", "u1", "staff", ""); code != 200 || out["data"].(map[string]any)["team_news"].(map[string]any)["can_review"] != false {
		t.Fatalf("analysis %d", code)
	}
	if code, _ = do("GET", "/api/v1/football/fixtures/9100101/news", "o1", "u1", "manager", ""); code != 200 {
		t.Fatal("news")
	}
	if code, _ = do("GET", "/api/v1/football/jackpots/candidates?home=Piast&away=Korona&date=2026-11-20", "o1", "u1", "manager", ""); code != 200 {
		t.Fatal("candidates")
	}
}

// One scheduler pass runs the free sources within budgets (fake server).
func TestRunFreePass(t *testing.T) {
	s, db := freeSvc(t)
	seedJackpotWorld(t, db)
	f := newFakeFree(t)
	f.point(s)
	f.file(t, "/2026-11-20", "ce_ratings.csv", "text/csv")
	f.file(t, "/Fixtures", "ce_fixtures.csv", "text/csv")
	f.file(t, "/v4/fixtures", "op_fixtures.json", "application/json")
	f.file(t, "/v4/odds-by-tournaments", "op_odds_by_tournaments.json", "application/json")
	f.file(t, "/rss/search", "gnews.rss", "application/rss+xml")
	f.file(t, "/api/v2/doc/doc", "gdelt.json", "application/json")
	f.file(t, "/new/POL.csv", "fd_new_POL.csv", "text/csv")
	enableFree(t, s, map[string]int{ProviderGNews: 3})
	db.Model(&FreeSettings{}).Where("id = 1").Update("fdcouk_new", "POL")
	// A jackpot makes its fixtures targets for prices and news.
	text := jpText
	if _, err := s.CreateJackpot("orgA", "uA", JackpotInput{Text: &text}); err != nil {
		t.Fatal(err)
	}
	sum := RunSummary{Cost: map[string]int{}, Tasks: []TaskRun{}, Notes: []string{}}
	s.runFree(context.Background(), RunOpts{Trigger: "schedule"}, &sum)
	kinds := map[string]string{}
	for _, tr := range sum.Tasks {
		if _, ok := kinds[tr.Kind]; !ok || tr.Status == "ok" {
			kinds[tr.Kind] = tr.Status
		}
	}
	for _, k := range []string{TCERatings, TCEFixtures, TOPFixtures, TNewsGoogle, TNewsGDELT, TFDFile} {
		if kinds[k] != "ok" {
			t.Fatalf("%s: %v (%+v)", k, kinds[k], sum.Tasks)
		}
	}
	// Budget 3 a day: high priority may use 97% → 2 requests.
	if sum.Cost[ProviderGNews] != 2 || sum.Skipped == 0 || sum.Cost[ProviderGDELT] > perPass[ProviderGDELT] || sum.Cost[ProviderFDCOUK] > perPass[ProviderFDCOUK] {
		t.Fatalf("google budget %+v skipped %d", sum.Cost, sum.Skipped)
	}
	// Fixtures OddsPapi has just mapped are priced in the same pass.
	got := false
	for _, tr := range sum.Tasks {
		if tr.Kind == TOPOdds && tr.Status == "ok" {
			got = true
		}
	}
	if !got {
		t.Fatalf("odds in the discovery pass %+v", sum.Tasks)
	}
	// The next pass doesn't price them again (12 h) nor rediscover (6 h).
	sum2 := RunSummary{Cost: map[string]int{}, Tasks: []TaskRun{}, Notes: []string{}}
	s.runFree(context.Background(), RunOpts{Trigger: "schedule"}, &sum2)
	if sum2.Cost[ProviderOddsPapi] != 0 {
		t.Fatalf("second pass %+v", sum2.Tasks)
	}
	var n int64
	db.Model(&Odd{}).Where("source = ? AND bookmaker = 'betika'", ProviderOddsPapi).Count(&n)
	if n != 3 {
		t.Fatalf("betika rows %d", n)
	}
	var logs int64
	db.Model(&FetchLog{}).Where("provider IN ?", FreeProviders).Count(&logs)
	if logs < 8 {
		t.Fatalf("fetch log %d", logs)
	}
}

// The UI agent's requests: games without odds (added from the match centre),
// evidence status + per-game source check, the fixtures list's team-news
// flag and the expected number correct in the grading.
func TestJackpotUIRequests(t *testing.T) {
	s, db := freeSvc(t)
	seedJackpotWorld(t, db)
	// Two sites confirm Félix (Piast) → a confirmed injured fact.
	seedSquadNews(t, s)
	gd, _ := ParseGDELT(freeSample(t, "gdelt.json"))
	s.storeNews(gd, []newsTeam{{ID: 9100201, Name: "Piast Gliwice"}}, true)
	f1, f5 := int64(9100101), int64(9100105)
	in := JackpotInput{Games: []GameInput{{Home: "Piast Gliwice", Away: "Korona Kielce", FixtureID: &f1}, {Home: "Arminia Bielefeld", Away: "Magdeburg", Odds: []float64{2.20, 3.55, 3.00}, FixtureID: &f5}}}
	v, err := s.CreateJackpot("orgA", "uA", in)
	if err != nil {
		t.Fatal(err)
	}
	g := v.Analysis.Games[0]
	if g.Odds["1"] != nil || v.Games[0].Odds["X"] != nil || g.Sources[0].Used || !strings.Contains(g.Sources[0].Why, "aren't typed") || !near(g.Probs["1"], 0.3333, 1e-4) {
		t.Fatalf("no odds game %+v", g)
	}
	if *v.Analysis.Games[1].Odds["1"] != 2.20 || len(v.Analysis.Notes) == 0 {
		t.Fatalf("odds game %+v", v.Analysis.Games[1])
	}
	hasTN := false
	for _, e := range g.Evidence {
		if e.Kind == "team_news" && e.Status == "confirmed" {
			hasTN = true
		}
	}
	if !hasTN || g.SourceCheck == nil || g.SourceCheck.Status == "" || len(g.SourceCheck.Items) == 0 {
		t.Fatalf("evidence %+v sc %+v", g.Evidence, g.SourceCheck)
	}
	// Without a fixture the odds are still required.
	if _, err := s.CreateJackpot("orgA", "uA", JackpotInput{Games: []GameInput{{Home: "A", Away: "B"}, {Home: "C", Away: "D", Odds: []float64{2, 3, 4}}}}); err == nil {
		t.Fatal("game without odds or fixture accepted")
	}
	// Fixtures list flag.
	rows := []FixtureRow{{ID: 9100101, Home: TeamRef{ID: 9100201}, Away: TeamRef{ID: 9100202}}, {ID: 9100105, Home: TeamRef{ID: 9100209}, Away: TeamRef{ID: 9100210}}}
	s.addTeamNewsCounts(rows, "orgA")
	if rows[0].TeamNews == nil || rows[0].TeamNews.Home != 1 || rows[0].TeamNews.Away != 0 || rows[1].TeamNews.Home != 0 {
		t.Fatalf("flags %+v %+v", rows[0].TeamNews, rows[1].TeamNews)
	}
	// Grading: expected correct of the most likely ticket.
	s.SetClock(time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC))
	db.Exec(`UPDATE fb_fixtures SET status = 'FT', ft_home = 1, ft_away = 0 WHERE id IN (9100101, 9100105)`)
	s.GradeJackpots()
	v, _ = s.JackpotViewOf("orgA", "uA", v.Jackpot.ID, 0)
	if v.Grading == nil || !v.Grading.Complete || v.Grading.ExpectedCorrect == nil || *v.Grading.ExpectedCorrect <= 0.5 || *v.Grading.ExpectedCorrect > 1.2 {
		t.Fatalf("expected correct %+v", v.Grading)
	}
	rs, _ := s.ListJackpots("orgA", "uA", "graded", 0, 0)
	if len(rs) != 1 || rs[0].Graded == nil || rs[0].Graded.ExpectedCorrect == nil {
		t.Fatalf("summary %+v", rs)
	}
}
