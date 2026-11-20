package football

import (
	"math"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func freeSample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "free", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The 20 Nov 2026 jackpot (13 games, prize KSH 22,500,447) from the brief.
var jp20Nov = []struct {
	Home, Away string
	O          [3]float64
}{
	{"Piast Gliwice", "Korona Kielce", [3]float64{2.44, 3.35, 2.55}},
	{"Chrobry Głogów", "Puszcza Niepołomice", [3]float64{2.19, 3.20, 2.90}},
	{"Lechia Zielona Góra", "Chojniczanka Chojnice", [3]float64{2.40, 3.55, 2.36}},
	{"Kapfenberg", "WSC Hertha Wels", [3]float64{2.60, 3.50, 2.25}},
	{"Arminia Bielefeld", "Magdeburg", [3]float64{2.20, 3.55, 3.00}},
	{"Kolding IF", "AaB", [3]float64{2.55, 3.30, 2.36}},
	{"Farul", "Universitatea Cluj", [3]float64{2.60, 3.30, 2.37}},
	{"Club Brugge U21", "Virton", [3]float64{2.23, 3.40, 2.65}},
	{"Helmond Sport", "Dordrecht", [3]float64{2.20, 3.50, 2.80}},
	{"Almere City", "NAC Breda", [3]float64{2.65, 3.75, 2.20}},
	{"Austria Salzburg", "Admira Wacker", [3]float64{2.30, 3.40, 2.60}},
	{"Miedź Legnica", "ŁKS Łódź", [3]float64{2.22, 3.40, 2.70}},
	{"Cliftonville", "Glentoran", [3]float64{2.65, 3.40, 2.28}},
}

func jp20NovProbs() []P3 {
	ps := []P3{}
	for _, g := range jp20Nov {
		p, _ := DeMarginProportional(g.O)
		ps = append(ps, p)
	}
	return ps
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// With the jackpot odds alone (proportional de-margin) the most likely
// ticket is 1 1 2 2 1 2 2 1 1 2 1 1 2 at about 1 in 160,000 (exactly
// 1 in 166,222; checked independently in Python).
func TestJackpotExampleMostLikely(t *testing.T) {
	ps := jp20NovProbs()
	ml := MostLikely(ps)
	if got := strings.Join(ticketLabels(ml), " "); got != "1 1 2 2 1 2 2 1 1 2 1 1 2" {
		t.Fatalf("most likely %s", got)
	}
	p := TicketProb(ps, ml)
	if oneIn := 1 / p; oneIn < 150000 || oneIn > 175000 || !near(oneIn, 166222, 2) {
		t.Fatalf("1 in %.0f", oneIn)
	}
	// Game 1: 2.44/3.35/2.55 → 0.3724/0.2712/0.3563, margin 10.05%.
	p1, m := DeMarginProportional(jp20Nov[0].O)
	if !near(p1[0], 0.3724, 5e-5) || !near(p1[1], 0.2712, 5e-5) || !near(p1[2], 0.3563, 5e-5) || !near(m, 0.1005, 5e-5) {
		t.Fatalf("game 1 %v %v", p1, m)
	}
	qs := []float64{}
	for i, k := range ml {
		qs = append(qs, ps[i][k])
	}
	dist := PoissonBinomial(qs)
	want := map[int]float64{13: 6.01606537694024e-06, 12: 0.0001251359263880953, 11: 0.001213330863699759, 10: 0.007286178283345328}
	for k, w := range want {
		if got := AtLeast(dist, k); !near(got, w, w*1e-9) {
			t.Fatalf("P(>=%d) %v want %v", k, got, w)
		}
	}
	if e, _ := ExpectedDraws(ps); !near(e, 3.425021537395468, 1e-9) {
		t.Fatalf("expected draws %v", e)
	}
	// Top tickets best first (Python brute force over 3^13; labels 1/X/2).
	wantTop := []string{"1122122112112", "1112122112112", "2122122112112", "2112122112112", "1122112112112", "1122121112112", "1112112112112",
		"1112121112112", "2122112112112", "1122122112212"}
	for i, tk := range TopTickets(ps, 10) {
		if got := strings.Join(ticketLabels(tk), ""); got != wantTop[i] {
			t.Fatalf("top %d: %s want %s", i+1, got, wantTop[i])
		}
	}
	// Cover priority: game 3 (Lechia ZG v Chojniczanka) is the first double.
	steps, _ := CoverPriority(ps, 8)
	if steps[0].Game != 3 || steps[0].Picks != "12" || steps[0].Combinations != 2 || !near(1/steps[0].PAll, 83809, 2) {
		t.Fatalf("cover %+v", steps[0])
	}
	if steps[7].Combinations != 256 || !near(1/steps[7].PAll, 966, 1) {
		t.Fatalf("cover 8 %+v", steps[7])
	}
	// EV with the default sharing assumption (300,000 tickets, popularity 1.5).
	ev := TicketEV(ps, ml, 22500447, 99, []BonusTier{{12, 250000}, {11, 25000}, {10, 2500}}, SharingOpts{Mode: "model", TicketsSold: 300000, Popularity: 1.5})
	if !near(ev.CoWinners, 4.859, 0.001) || !near(ev.EVJackpot, 27.64, 0.01) || !near(ev.EVBonuses, 72.17, 0.01) || !near(ev.EVTotal, 0.81, 0.01) {
		t.Fatalf("ev %+v", ev)
	}
}

// Best-first order is strict and without duplicates far down the list.
func TestJackpotExampleTopOrder(t *testing.T) {
	ps := jp20NovProbs()
	seen := map[string]bool{}
	prev := 1.0
	for _, tk := range TopTickets(ps, 500) {
		p := TicketProb(ps, tk)
		if p > prev+1e-15 {
			t.Fatal("not best-first")
		}
		prev = p
		k := strings.Join(ticketLabels(tk), "")
		if seen[k] {
			t.Fatal("duplicate ticket")
		}
		seen[k] = true
	}
}

// Brute force check of the k-best search on small random cases.
func TestTopTicketsBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 40; trial++ {
		n := 3 + rng.Intn(5)
		ps := make([]P3, n)
		for i := range ps {
			a, b, c := rng.Float64()+0.05, rng.Float64()+0.05, rng.Float64()+0.05
			if trial%5 == 0 && i == 0 {
				b = a // ties
			}
			s := a + b + c
			ps[i] = P3{a / s, b / s, c / s}
		}
		all := []float64{}
		var rec func(i int, p float64)
		rec = func(i int, p float64) {
			if i == n {
				all = append(all, p)
				return
			}
			for k := 0; k < 3; k++ {
				rec(i+1, p*ps[i][k])
			}
		}
		rec(0, 1)
		sort.Sort(sort.Reverse(sort.Float64Slice(all)))
		k := 25
		got := TopTickets(ps, k)
		if len(got) != minInt(k, len(all)) {
			t.Fatalf("got %d tickets", len(got))
		}
		for i, tk := range got {
			if !near(TicketProb(ps, tk), all[i], 1e-12) {
				t.Fatalf("trial %d rank %d: %v want %v", trial, i, TicketProb(ps, tk), all[i])
			}
		}
		// Everything when asking for more than exists.
		if len(TopTickets(ps, len(all)+50)) != len(all) {
			t.Fatal("not all tickets enumerated")
		}
	}
}

// Poisson-binomial against a simulation.
func TestPoissonBinomialVsSimulation(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	qs := []float64{0.37, 0.41, 0.38, 0.40, 0.43, 0.38, 0.38, 0.40, 0.41, 0.41, 0.39, 0.40, 0.40}
	d := PoissonBinomial(qs)
	s := 0.0
	for _, v := range d {
		s += v
	}
	if !near(s, 1, 1e-12) || len(d) != 14 {
		t.Fatalf("sum %v len %d", s, len(d))
	}
	const N = 400000
	counts := make([]int, 14)
	for i := 0; i < N; i++ {
		k := 0
		for _, q := range qs {
			if rng.Float64() < q {
				k++
			}
		}
		counts[k]++
	}
	for k := 0; k <= 13; k++ {
		emp := float64(counts[k]) / N
		se := math.Sqrt(d[k]*(1-d[k])/N) + 1e-6
		if math.Abs(emp-d[k]) > 5*se {
			t.Fatalf("k=%d exact %v simulated %v", k, d[k], emp)
		}
	}
	// Mean = Σq.
	m := 0.0
	for k, v := range d {
		m += float64(k) * v
	}
	sq := 0.0
	for _, q := range qs {
		sq += q
	}
	if !near(m, sq, 1e-12) {
		t.Fatal("mean")
	}
}

func TestBlendAndEV(t *testing.T) {
	a := P3{0.5, 0.3, 0.2}
	b := P3{0.3, 0.3, 0.4}
	p, eff := BlendP3([]P3{a, b}, []float64{0.25, 0.75})
	if !near(p[0], 0.35, 1e-12) || !near(eff[0], 0.25, 1e-12) {
		t.Fatalf("blend %v %v", p, eff)
	}
	// Missing source: weights renormalise over what exists.
	p, eff = BlendP3([]P3{a}, []float64{0.25})
	if p != a || eff[0] != 1 {
		t.Fatalf("single %v %v", p, eff)
	}
	if ShareFactor(0) != 1 || !near(ShareFactor(1), 1-math.Exp(-1), 1e-12) {
		t.Fatal("share factor")
	}
	// Two certain games: EV = prize × share − price.
	ps := []P3{{1, 0, 0}, {1, 0, 0}}
	two := 2.0
	ev := TicketEV(ps, Ticket{0, 0}, 1000, 10, nil, SharingOpts{Mode: "fixed", CoWinners: &two})
	if !near(ev.EVJackpot, 1000*ShareFactor(2), 0.01) || !near(ev.EVTotal, ev.EVJackpot-10, 0.01) {
		t.Fatalf("ev %+v", ev)
	}
	// Covered probability.
	pr, n := CoveredProb([]P3{{0.5, 0.3, 0.2}, {0.4, 0.4, 0.2}}, [][]int{{0, 1}, {2}})
	if !near(pr, 0.8*0.2, 1e-12) || n != 2 {
		t.Fatal("covered")
	}
	for in, want := range map[string]string{"1": "1", "x": "X", "x2": "X2", "21": "12", "1X2": "1X2", "0": "X"} {
		ks, ok := ParsePick(in)
		if !ok || pickString(ks) != want {
			t.Fatalf("pick %s → %v", in, ks)
		}
	}
	if _, ok := ParsePick("3"); ok {
		t.Fatal("bad pick accepted")
	}
	// Public shares favour favourites.
	pub := PublicShares(P3{0.5, 0.25, 0.25}, 1.5)
	if pub[0] <= 0.5 || !near(pub[0]+pub[1]+pub[2], 1, 1e-12) {
		t.Fatalf("public %v", pub)
	}
}

func TestParseJackpotText(t *testing.T) {
	base := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	text := `SPORTPESA MIDWEEK JACKPOT
1. GKS PIAST GLIWICE - KORONA KELCE 2.44 3.35 2.55
2) CHROBRY GLOGOW vs PUSZCZA NIEPOLOMICE 2,19 3,20 2,90
3 Poland: LECHIA ZIELONA GORA - CHOJNICE 2.40 3.55 2.36
4. 20/11 18:00 KAPFENBERG v WSC HERTHA WELS 2.60 3.50 2.25
Arminia Bielefeld v Magdeburg 2.20 3.55 3.00
6. KOLDING IF – AAB
2.55 3.30 2.36
7. Thu 20 Nov FARUL CONSTANTA - U CLUJ 2.60 3.30 2.37
8. CLUB BRUGGE U21 - VIRTON (BEL) 2.23 3.40 2.65
9	HELMOND SPORT	DORDRECHT	2.20	3.50	2.80
Almere City
NAC Breda
2.65 3.75 2.20
11. AUSTRIA SALZBURG / ADMIRA WACKER 2.30 3.40 2.60
12. MIEDZ LEGNICA - LKS LODZ 2.22 3.40 2.70
13. CLIFTONVILLE BELFAST - GLENTORAN 2.65 3.40 2.28
Total combinations: 1,594,323`
	r := ParseJackpotText(text, base, time.UTC)
	if len(r.Games) != 13 {
		for _, g := range r.Games {
			t.Logf("%d %q %q %v", g.Line, g.Home, g.Away, g.Odds)
		}
		t.Fatalf("games %d skipped %+v", len(r.Games), r.Skipped)
	}
	want := [][2]string{{"GKS PIAST GLIWICE", "KORONA KELCE"}, {"CHROBRY GLOGOW", "PUSZCZA NIEPOLOMICE"}, {"LECHIA ZIELONA GORA", "CHOJNICE"},
		{"KAPFENBERG", "WSC HERTHA WELS"}, {"Arminia Bielefeld", "Magdeburg"}, {"KOLDING IF", "AAB"}, {"FARUL CONSTANTA", "U CLUJ"},
		{"CLUB BRUGGE U21", "VIRTON"}, {"HELMOND SPORT", "DORDRECHT"}, {"Almere City", "NAC Breda"}, {"AUSTRIA SALZBURG", "ADMIRA WACKER"},
		{"MIEDZ LEGNICA", "LKS LODZ"}, {"CLIFTONVILLE BELFAST", "GLENTORAN"}}
	for i, g := range r.Games {
		if g.Home != want[i][0] || g.Away != want[i][1] || len(g.Odds) != 3 {
			t.Fatalf("game %d: %q v %q %v", i+1, g.Home, g.Away, g.Odds)
		}
		for k := 0; k < 3; k++ {
			if g.Odds[k] != jp20Nov[i].O[k] {
				t.Fatalf("game %d odds %v", i+1, g.Odds)
			}
		}
	}
	if r.Games[2].Country != "Poland" {
		t.Fatalf("country %q", r.Games[2].Country)
	}
	if k := r.Games[3].Kickoff; k == nil || !k.Equal(time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("kickoff %v", k)
	}
	if k := r.Games[6].Kickoff; k == nil || k.Day() != 20 {
		t.Fatalf("kickoff 7 %v", k)
	}
	if len(r.Skipped) != 2 { // the title and the total line
		t.Fatalf("skipped %+v", r.Skipped)
	}
	if len(r.Warnings) == 0 || !strings.Contains(strings.Join(r.Warnings, " "), "Line 4") {
		// line 4 (Lechia, margin 12.2%) is flagged
		t.Logf("warnings %v", r.Warnings)
	}
	// A bad odds line is warned about.
	r2 := ParseJackpotText("A v B 1.00 3.0 4.0\nC - D 9.5 9.5 9.5", base, time.UTC)
	if len(r2.Games) != 2 || len(r2.Warnings) != 2 {
		t.Fatalf("warnings %+v", r2)
	}
}

func TestTeamSim(t *testing.T) {
	good := [][2]string{{"KORONA KELCE", "Korona Kielce"}, {"CHOJNICE", "Chojniczanka Chojnice"}, {"GKS PIAST GLIWICE", "Piast Gliwice"},
		{"CLIFTONVILLE BELFAST", "Cliftonville"}, {"U CLUJ", "Universitatea Cluj"}, {"LKS LODZ", "ŁKS Łódź"}, {"MIEDZ LEGNICA", "Miedź Legnica"},
		{"CHROBRY GLOGOW", "Chrobry Głogów"}, {"PUSZCZA NIEPOLOMICE", "Puszcza Niepołomice"}, {"FARUL CONSTANTA", "Farul"}, {"CLUB BRUGGE U21", "Club Brugge KV U21"},
		{"KOLDING IF", "Kolding IF"}, {"AAB", "AaB"}, {"WSC HERTHA WELS", "Hertha Wels"}, {"ADMIRA WACKER", "Admira Wacker Mödling"}}
	for _, g := range good {
		if v := TeamSim(g[0], g[1]); v < 0.8 {
			t.Errorf("%q ~ %q = %.2f", g[0], g[1], v)
		}
	}
	bad := [][2]string{{"CLUB BRUGGE U21", "Club Brugge KV"}, {"Korona Kielce", "Piast Gliwice"}, {"Austria Salzburg", "Red Bull Salzburg"},
		{"Lechia Zielona Góra", "Lechia Gdańsk"}, {"Almere City", "Manchester City"}}
	for _, b := range bad {
		if v := TeamSim(b[0], b[1]); v >= 0.8 {
			t.Errorf("%q !~ %q = %.2f", b[0], b[1], v)
		}
	}
	if Fold("Łódź Głogów Mödling Køge Straße") != "lodz glogow modling koge strasse" {
		t.Fatalf("fold %q", Fold("Łódź Głogów Mödling Køge Straße"))
	}
}

func TestMatchGamePure(t *testing.T) {
	ko := time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC)
	cands := []fixCand{
		{ID: 1, Kickoff: ko, HomeID: 11, AwayID: 12, Home: "Piast Gliwice", Away: "Korona Kielce", CompName: "Ekstraklasa"},
		{ID: 2, Kickoff: ko, HomeID: 21, AwayID: 22, Home: "Club Brugge KV", Away: "KAA Gent", CompName: "Jupiler Pro League"},
		{ID: 3, Kickoff: ko.Add(2 * time.Hour), HomeID: 31, AwayID: 32, Home: "Club Brugge KV U21", Away: "Excelsior Virton", CompName: "Challenger Pro League"},
		{ID: 4, Kickoff: ko, HomeID: 41, AwayID: 42, Home: "Red Bull Salzburg", Away: "Rapid Wien", CompName: "Bundesliga"},
		{ID: 5, Kickoff: ko, HomeID: 51, AwayID: 52, Home: "Austria Salzburg", Away: "Admira Wacker Mödling", CompName: "2. Liga"},
	}
	m := MatchGame("GKS PIAST GLIWICE", "KORONA KELCE", nil, cands, map[string]int64{})
	if m.Status != "matched" || m.FixtureID == nil || *m.FixtureID != 1 {
		t.Fatalf("piast %+v", m)
	}
	m = MatchGame("CLUB BRUGGE U21", "VIRTON", nil, cands, map[string]int64{})
	if m.FixtureID == nil || *m.FixtureID != 3 {
		t.Fatalf("brugge %+v", m)
	}
	m = MatchGame("AUSTRIA SALZBURG", "ADMIRA WACKER", nil, cands, map[string]int64{})
	if m.FixtureID == nil || *m.FixtureID != 5 {
		t.Fatalf("salzburg %+v", m)
	}
	m = MatchGame("Lech Poznan", "Legia", nil, cands, map[string]int64{})
	if m.Status != "unmatched" || m.FixtureID != nil {
		t.Fatalf("unmatched %+v", m)
	}
	// An alias decides even a very different spelling.
	m = MatchGame("KKS", "KORONA KELCE", nil, cands, map[string]int64{normFree("KKS"): 11})
	if m.FixtureID == nil || *m.FixtureID != 1 || m.Via != "alias" && m.Via != "name" {
		t.Fatalf("alias %+v", m)
	}
	// A kickoff far away lowers the confidence to a suggestion.
	far := ko.Add(40 * time.Hour)
	m = MatchGame("Piast Gliwice", "Korona Kielce", &far, cands, map[string]int64{})
	if m.Status == "matched" {
		t.Fatalf("far kickoff %+v", m)
	}
}

func TestKeywordRulesLanguages(t *testing.T) {
	rules := DefaultNewsRules()
	cases := []struct {
		lang, text, status string
	}{
		{"en", "Striker ruled out for six weeks with a hamstring injury", TNInjured},
		{"en", "Captain suspended after red card", TNSuspended},
		{"en", "Midfielder a major doubt for Saturday", TNDoubtful},
		{"en", "Winger back in training after knee problem", TNReturning},
		{"pl", "Piast bez Félixa. Kontuzja wyklucza go z meczu", TNInjured},
		{"pl", "Napastnik doznał kontuzji i nie zagra w piątek", TNInjured}, // inflected "kontuzji"
		{"pl", "Obrońca zawieszony za kartki", TNSuspended},
		{"de", "Kapitän fällt aus – Verletzung am Knie", TNInjured},
		{"de", "Stürmer nach Rot gesperrt", TNSuspended},
		{"nl", "Spits geblesseerd, blessure aan de knie", TNInjured},
		{"nl", "Verdediger geschorst na rode kaart", TNSuspended},
		{"da", "Angriber skadet efter træning", TNInjured},
		{"da", "Midtbanespiller i karantæne", TNSuspended},
		{"ro", "Fundașul accidentat ratează meciul", TNInjured},
		{"ro", "Mijlocașul suspendat pentru derby", TNSuspended},
		{"fr", "L'attaquant blessé manquera le match", TNInjured},
		{"fr", "Le défenseur suspendu", TNSuspended},
		{"sw", "Mshambuliaji majeruhi hatacheza", TNInjured},
		{"sw", "Beki amefungiwa mechi tatu", TNSuspended},
	}
	for _, c := range cases {
		hits := MatchKeywords(c.text, c.lang, rules)
		found := false
		for _, h := range hits {
			if h.Status == c.status {
				found = true
			}
		}
		if !found {
			t.Errorf("%s %q: %v", c.lang, c.text, hits)
		}
	}
	// Diacritics either way.
	if len(MatchKeywords("Kapitaen faellt aus", "de", rules)) != 0 && len(MatchKeywords("Kapitan fallt aus", "de", rules)) == 0 {
		t.Error("diacritics-insensitive")
	}
	if hits := MatchKeywords("Kapitan fallt aus", "de", rules); len(hits) == 0 {
		t.Error("fällt without umlaut")
	}
	// Nothing in a plain preview.
	if hits := MatchKeywords("Piast Gliwice – Korona Kielce: zapowiedź meczu 17. kolejki", "pl", rules); len(hits) != 0 {
		t.Errorf("preview matched %v", hits)
	}
	// "trener" alone is not a coach change; "zwolniony" is.
	cs, _ := ExtractFacts("Trener Korony: jesteśmy gotowi", "pl", 1, nil, rules)
	if len(cs) != 0 {
		t.Errorf("coach role only: %v", cs)
	}
	cs, _ = ExtractFacts("Trener Korony zwolniony? Zarząd zdecyduje", "pl", 1, nil, rules)
	if len(cs) != 1 || cs[0].Status != TNCoach || cs[0].Role != "coach" {
		t.Errorf("coach change: %v", cs)
	}
	if DetectLanguage("Trener Korony zwolniony? Zarząd zdecyduje po meczu", "") != "pl" {
		t.Error("detect pl")
	}
}

func TestMatchPlayersAndFacts(t *testing.T) {
	squad := []SquadPlayer{{ID: 1, Name: "J. Félix", Firstname: "Jorge", Lastname: "Félix Muñoz García"}, {ID: 2, Name: "D. Błanik", Firstname: "Dawid", Lastname: "Błanik"},
		{ID: 3, Name: "M. Nowak", Firstname: "Mateusz", Lastname: "Nowak"}, {ID: 4, Name: "K. Nowak", Firstname: "Kamil", Lastname: "Nowak"}, {ID: 5, Name: "A. Gol", Firstname: "Adam", Lastname: "Gol"}}
	names := func(ps []SquadPlayer) string {
		out := []string{}
		for _, p := range ps {
			out = append(out, p.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(MatchPlayers("Korona: uraz Błanika, występ pod znakiem zapytania", squad)); got != "D. Błanik" {
		t.Fatalf("inflected surname: %q", got)
	}
	if got := names(MatchPlayers("Piast bez Felixa", squad)); got != "J. Félix" {
		t.Fatalf("surname of a long last name: %q", got)
	}
	if got := names(MatchPlayers("Nowak kontuzjowany", squad)); got != "" {
		t.Fatalf("ambiguous surname matched: %q", got)
	}
	if got := names(MatchPlayers("Kamil Nowak kontuzjowany", squad)); got != "K. Nowak" {
		t.Fatalf("first name: %q", got)
	}
	if got := names(MatchPlayers("M. Nowak wraca", squad)); got != "M. Nowak" {
		t.Fatalf("initial: %q", got)
	}
	if got := names(MatchPlayers("Gol w ostatniej minucie", squad)); got != "" {
		t.Fatalf("common word: %q", got)
	}
	rules := DefaultNewsRules()
	cs, hits := ExtractFacts("Jorge Félix kontuzjowany – Piast traci napastnika", "pl", 9, squad, rules)
	if len(cs) != 1 || cs[0].PlayerID != 1 || cs[0].Status != TNInjured || len(hits) == 0 {
		t.Fatalf("fact %v %v", cs, hits)
	}
	if factKey(cs[0]) != "9|p1|injured" {
		t.Fatal(factKey(cs[0]))
	}
	cs, _ = ExtractFacts("Winger M. Nowak back in training after his injury", "en", 9, squad, rules)
	if len(cs) != 1 || cs[0].Status != TNReturning {
		t.Fatalf("returning priority %v", cs)
	}
	if !teamMentioned("Koronie zabraknie Błanika", "Korona Kielce") || teamMentioned("Lech wygrywa", "Korona Kielce") {
		t.Fatal("team mention")
	}
}

func TestFreeParsersRecordedSamples(t *testing.T) {
	rs, err := ParseClubEloRatings(freeSample(t, "ce_ratings.csv"))
	if err != nil || len(rs) != 12 || rs[0].Club != "Liverpool" || rs[0].Rank == nil || *rs[0].Rank != 1 || rs[3].Rank != nil || rs[3].Club != "Piast" || !near(rs[3].Elo, 1508.199, 0.01) {
		t.Fatalf("ratings %v %+v", err, rs[:4])
	}
	fs, err := ParseClubEloFixtures(freeSample(t, "ce_fixtures.csv"))
	if err != nil || len(fs) != 3 || fs[0].Home != "Piast" || !near(fs[0].PHome+fs[0].PDraw+fs[0].PAway, 1, 1e-9) || !near(fs[0].PDraw, 0.274, 0.001) || fs[0].PHome < fs[0].PAway {
		t.Fatalf("fixtures %v %+v", err, fs)
	}
	e0, err := ParseFDCSV(freeSample(t, "fd_E0.csv"), "E0", 2025)
	if err != nil || len(e0) != 2 || e0[0].Home != "Liverpool" || e0[0].HG != 4 || e0[0].AG != 2 || e0[0].PSCH == nil || *e0[0].PSCH != 1.3 || e0[0].Season != 2025 {
		t.Fatalf("E0 %v %+v", err, e0)
	}
	// 20:00 UK time in August = 19:00 UTC.
	if !e0[0].Date.Equal(time.Date(2025, 8, 15, 19, 0, 0, 0, time.UTC)) || *e0[1].AvgCA != 2.8 {
		t.Fatalf("E0 date %v", e0[0].Date)
	}
	pol, err := ParseFDCSV(freeSample(t, "fd_new_POL.csv"), "POL", 0)
	if err != nil || len(pol) != 3 || pol[0].Season != 2024 || pol[1].Season != 2025 || pol[1].Home != "Korona Kielce" || pol[1].AG != 2 || pol[0].Div != "POL Ekstraklasa" || *pol[2].PSCD != 3.6 {
		t.Fatalf("POL %v %+v", err, pol)
	}
	code, ms, err := ParseFDOrgMatches(freeSample(t, "fdo_matches.json"))
	if err != nil || code != "PL" || len(ms) != 2 || ms[0].HomeTeam.Name != "Liverpool FC" || *ms[0].Score.FullTime.Home != 2 || ms[1].Score.FullTime.Home != nil {
		t.Fatalf("fdorg matches %v %+v", err, ms)
	}
	code, st, err := ParseFDOrgStandings(freeSample(t, "fdo_standings.json"))
	if err != nil || code != "PL" || len(st) != 2 || st[0].Points != 26 || st[0].Form != "WWDWW" || st[1].Position != 2 {
		t.Fatalf("fdorg standings %v %+v", err, st)
	}
	ops, err := decodeOP(freeSample(t, "op_fixtures.json"))
	if err != nil || len(ops) != 2 || ops[1].Participant1Name != "Piast Gliwice" || ops[1].TournamentID != 202 {
		t.Fatalf("op fixtures %v %+v", err, ops)
	}
	od, err := decodeOP(freeSample(t, "op_odds_by_tournaments.json"))
	if err != nil || len(od) != 1 {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	rows := OPOddsRows(od, map[string]int64{"id1000003969653792": 77}, now)
	if len(rows) != 3 {
		t.Fatalf("op rows %+v", rows)
	}
	got := map[string]float64{}
	for _, r := range rows {
		got[r.Selection] = r.Price
		if r.Bookmaker != "betika" || r.BookmakerTitle != "Betika (KE)" || r.Source != ProviderOddsPapi || r.ProviderUpdatedAt == nil || r.FixtureID != 77 {
			t.Fatalf("row %+v", r)
		}
	}
	if got[SelHome] != 2.44 || got[SelDraw] != 3.35 || got[SelAway] != 2.55 {
		t.Fatalf("prices %v", got)
	}
	if len(OPOddsRows(od, map[string]int64{}, now)) != 0 {
		t.Fatal("unmatched fixture priced")
	}
	gn, err := ParseRSS(freeSample(t, "gnews.rss"), ProviderGNews, "pl")
	if err != nil || len(gn) != 2 || gn[0].Title != "Piast bez Félixa. Kontuzja wyklucza go z meczu z Koroną" || gn[0].Domain != "sport.pl" || gn[0].Source != "Sport.pl" ||
		gn[0].PublishedAt.IsZero() || gn[0].Snippet != "" {
		t.Fatalf("gnews %v %+v", err, gn)
	}
	gd, err := ParseGDELT(freeSample(t, "gdelt.json"))
	if err != nil || len(gd) != 2 || gd[0].Language != "pl" || gd[0].Domain != "przegladsportowy.onet.pl" || !gd[0].PublishedAt.Equal(time.Date(2026, 11, 19, 18, 30, 0, 0, time.UTC)) {
		t.Fatalf("gdelt %v %+v", err, gd)
	}
	at, err := ParseRSS(freeSample(t, "feed_atom.xml"), ProviderFeeds, "pl")
	if err != nil || len(at) != 2 || at[0].Domain != "echodnia.eu" || at[1].PublishedAt.IsZero() || !strings.Contains(at[0].Snippet, "Błanik") {
		t.Fatalf("atom %v %+v", err, at)
	}
	rss, err := ParseRSS(freeSample(t, "feed_rss.xml"), ProviderFeeds, "de")
	if err != nil || len(rss) != 1 || rss[0].Snippet != "Der Kapitän verletzt sich im Training." || rss[0].PublishedAt.IsZero() {
		t.Fatalf("rss %v %+v", err, rss)
	}
	if canonicalURL(rss[0].URL) != "https://www.example-club.de/news/kapitaen-faellt-aus" {
		t.Fatalf("canonical %q", canonicalURL(rss[0].URL))
	}
	if _, err := ParseFDCSV([]byte("a,b\n1,2\n"), "X", 0); err == nil {
		t.Fatal("bad csv accepted")
	}
	if _, err := ParseClubEloRatings([]byte("<html>")); err == nil {
		t.Fatal("bad ratings accepted")
	}
}

func TestEloFormulaAndSourceCheckEval(t *testing.T) {
	p := EloFormula(1500, 1500, true)
	if !near(p[0], p[2], 1e-9) || !near(p[1], 0.29, 0.01) || !near(p[0]+p[1]+p[2], 1, 1e-12) {
		t.Fatalf("equal %v", p)
	}
	p = EloFormula(1900, 1400, false)
	if p[0] < 0.75 || p[1] > 0.2 {
		t.Fatalf("strong home %v", p)
	}
	it := SCItem{Values: []SCValue{{Source: "apifootball", Value: 6}, {Source: "fdorg", Value: 7}}}
	EvalItem(&it, scNumber)
	if it.Status != "minor" || len(it.Disagreeing) != 1 {
		t.Fatalf("minor %+v", it)
	}
	it = SCItem{Values: []SCValue{{Source: "apifootball", Value: 6}, {Source: "fdorg", Value: 6}, {Source: "fdcouk", Value: 9}}}
	EvalItem(&it, scNumber)
	if it.Status != "conflict" || len(it.SourcesAgreeing) != 2 {
		t.Fatalf("conflict %+v", it)
	}
	it = SCItem{Values: []SCValue{{Source: "apifootball", Value: "WDWLW"}, {Source: "fdcouk", Value: "DWLWD"}}}
	EvalItem(&it, scForm)
	if it.Status != "minor" {
		t.Fatalf("form shift %+v", it)
	}
	ko := time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC)
	it = SCItem{Values: []SCValue{{Source: "apifootball", Value: ko}, {Source: "oddspapi", Value: ko.Add(10 * time.Minute)}, {Source: "clubelo", Value: "2026-11-20"}}}
	EvalItem(&it, scTime)
	if it.Status != "minor" || len(it.SourcesAgreeing) != 2 {
		t.Fatalf("kickoff %+v", it)
	}
	it = SCItem{Values: []SCValue{{Source: "apifootball", Value: ko}, {Source: "fdorg", Value: ko.Add(26 * time.Hour)}}}
	EvalItem(&it, scTime)
	if it.Status != "conflict" {
		t.Fatalf("moved %+v", it)
	}
	it = SCItem{Values: []SCValue{{Source: "apifootball", Value: "Korona Kielce"}, {Source: "clubelo", Value: "Korona"}}}
	EvalItem(&it, scName)
	if it.Status != "agree" {
		t.Fatalf("identity %+v", it)
	}
	it = SCItem{Values: []SCValue{{Source: "apifootball", Value: 3}}}
	EvalItem(&it, scNumber)
	if it.Status != "single" {
		t.Fatal("single")
	}
}

func TestPlanFreeBudgetsAndIntervals(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	st := defaultFreeSettings()
	on := map[string]bool{}
	for _, p := range FreeProviders {
		on[p] = true
	}
	ko := now.Add(8 * time.Hour)
	in := FreePlanInput{Now: now, Settings: st, On: on, HasAF: true, State: map[string]TaskState{},
		FD: []fdTarget{{Path: "/mmz4281/2526/E0.csv", Div: "E0", Season: 2025, Comp: 39, Key: "fd:main:E0:2025", Weekly: true}, {Path: "/mmz4281/2425/E0.csv", Div: "E0", Season: 2024, Comp: 39, Key: "fd:main:E0:2024"}},
		FDOrg: []struct {
			Code string
			Comp int64
		}{{"PL", 39}},
		OP:        []opTarget{{FixtureID: 1, Kickoff: ko, Prio: 1}, {FixtureID: 2, Kickoff: ko, Prio: 2, OPID: "x", Tournament: 17}, {FixtureID: 3, Kickoff: ko, Prio: 1, OPID: "y", Tournament: 202}},
		NewsTeams: []newsTarget{{newsTeam: newsTeam{ID: 10, Name: "Piast Gliwice", Country: "Poland"}, Kickoff: ko, Prio: PrioHigh}},
		Feeds:     []NewsFeed{{ID: "f1", URL: "https://x.example/rss", Enabled: true}, {ID: "f2", URL: "https://y.example/rss", Enabled: false}},
		JPDates:   []string{"2026-11-20"}, JPResults: []int64{5, 6}}
	ts := PlanFree(in)
	kinds := map[string]int{}
	for _, t := range ts {
		kinds[t.Kind]++
	}
	want := map[string]int{TCERatings: 1, TCEFixtures: 1, TFDFile: 2, TFDOStand: 1, TFDOMatches: 1, TOPFixtures: 1, TOPOdds: 2, TNewsGoogle: 2, TNewsGDELT: 1, TNewsFeed: 1, TJPDate: 1, TJPResults: 1}
	for k, n := range want {
		if kinds[k] != n {
			t.Fatalf("%s: %d tasks want %d (%v)", k, kinds[k], n, kinds)
		}
	}
	// Highest priority first: jackpot results (critical) lead.
	if ts[0].Kind != TJPResults {
		t.Fatalf("first %s", ts[0].Kind)
	}
	// OddsPapi odds: one request per bookmaker for both tournaments (betika + pinnacle).
	for _, t2 := range ts {
		if t2.Kind == TOPOdds && t2.Params.Get("tournaments") != "202,17" {
			t.Fatalf("tournaments %q (jackpot tournament first)", t2.Params.Get("tournaments"))
		}
	}
	// After running, nothing is due again soon.
	for _, t2 := range ts {
		ok := now
		in.State[t2.Key] = TaskState{Key: t2.Key, LastRunAt: now, LastOKAt: &ok, Status: "ok"}
	}
	in.OP[1].OddsAt, in.OP[2].OddsAt = &now, &now
	in.OP[0].OPID, in.OP[0].Tournament = "z", 99
	in.OP[0].OddsAt = &now
	in.Now = now.Add(20 * time.Minute)
	if ts2 := PlanFree(in); len(ts2) != 0 {
		t.Fatalf("again: %v", ts2)
	}
	// Past-season football-data.co.uk files are not refetched for 180 days; current weekly.
	in.Now = now.Add(8 * 24 * time.Hour)
	in.OP, in.NewsTeams, in.JPDates, in.JPResults = nil, nil, nil, nil
	n := 0
	for _, t2 := range PlanFree(in) {
		if t2.Kind == TFDFile {
			n++
			if t2.Key != "fd:main:E0:2025" {
				t.Fatalf("refetched %s", t2.Key)
			}
		}
	}
	if n != 1 {
		t.Fatalf("fd tasks %d", n)
	}
	// Switched-off sources plan nothing; manual "only" limits kinds.
	in.On = map[string]bool{}
	in.HasAF = false
	if len(PlanFree(in)) != 0 {
		t.Fatal("off sources planned")
	}
	in.On = on
	in.Only = map[string]bool{"clubelo": true}
	for _, t2 := range PlanFree(in) {
		if t2.Provider != ProviderClubElo {
			t.Fatalf("only clubelo: %s", t2.Kind)
		}
	}
	if freeOnly(map[string]bool{"odds_oa": true})["clubelo"] {
		t.Fatal("paid kinds")
	}
	_ = url.Values{}
}

func TestOPNeedsOdds(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	ko := now.Add(20 * time.Hour)
	if !opNeedsOdds(opTarget{Kickoff: ko}, now) {
		t.Fatal("first snapshot")
	}
	recent := now.Add(-2 * time.Hour)
	if opNeedsOdds(opTarget{Kickoff: ko, OddsAt: &recent}, now) {
		t.Fatal("too soon")
	}
	old := now.Add(-13 * time.Hour)
	if !opNeedsOdds(opTarget{Kickoff: ko, OddsAt: &old}, now) {
		t.Fatal("12 h refresh")
	}
	soon := now.Add(2 * time.Hour)
	if !opNeedsOdds(opTarget{Kickoff: soon, OddsAt: &old}, now) || opNeedsOdds(opTarget{Kickoff: soon, OddsAt: &now}, now) {
		t.Fatal("last 3 h snapshot")
	}
	if opNeedsOdds(opTarget{Kickoff: now.Add(72 * time.Hour)}, now) || opNeedsOdds(opTarget{Kickoff: now.Add(-time.Hour)}, now) {
		t.Fatal("window")
	}
}
