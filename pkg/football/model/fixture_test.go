package model

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// testdata/synthetic_league.csv is a small, fully synthetic fixture: ten
// invented clubs, two double round-robin seasons simulated from known
// Dixon–Coles parameters (seed 2024), with bookmaker odds = true probability
// plus a 5% margin, rounded to two decimals like real prices. It contains no
// real data. Regenerate with FBMODEL_WRITE_FIXTURE=1 go test -run TestWriteFixture.
const fixturePath = "testdata/synthetic_league.csv"

var fixtureTeams = []string{"Bonde United", "Jua Town", "Kilima Stars", "Mlima Rovers", "Msitu Wanderers",
	"Mto Rangers", "Pwani FC", "Savanna City", "Upepo Albion", "Ziwa Athletic"}

func TestWriteFixture(t *testing.T) {
	if os.Getenv("FBMODEL_WRITE_FIXTURE") == "" {
		t.Skip("set FBMODEL_WRITE_FIXTURE=1 to regenerate testdata")
	}
	l := newSimLeague(10, 2024)
	// rename teams
	ren := map[string]string{}
	for i, tm := range l.teams {
		ren[tm] = fixtureTeams[i]
	}
	att, def := map[string]float64{}, map[string]float64{}
	for k, v := range l.att {
		att[ren[k]] = v
		def[ren[k]] = l.def[k]
	}
	l.att, l.def = att, def
	for i := range l.teams {
		l.teams[i] = fixtureTeams[i]
	}
	ms := l.simulate(2, time.Date(2023, 8, 12, 0, 0, 0, 0, time.UTC), 2025)
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"id", "date", "home", "away", "hg", "ag", "odds_h", "odds_d", "odds_a"})
	for _, m := range ms {
		o := marketOdds(l.probs(m.Home, m.Away), 0.05)
		_ = w.Write([]string{m.ID, m.Date.Format("2006-01-02"), m.Home, m.Away, strconv.Itoa(m.HG), strconv.Itoa(m.AG),
			fmt.Sprintf("%.2f", o[0]), fmt.Sprintf("%.2f", o[1]), fmt.Sprintf("%.2f", o[2])})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	for _, tm := range l.teams {
		t.Logf("%-16s attack %+.4f defence %+.4f", tm, l.att[tm], l.def[tm])
	}
}

func loadFixture(t *testing.T) ([]Match, map[string][3]float64) {
	t.Helper()
	f, err := os.Open(filepath.FromSlash(fixturePath))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var ms []Match
	odds := map[string][3]float64{}
	for _, r := range rows[1:] {
		d, _ := time.Parse("2006-01-02", r[1])
		hg, _ := strconv.Atoi(r[4])
		ag, _ := strconv.Atoi(r[5])
		var o [3]float64
		for k := 0; k < 3; k++ {
			o[k], _ = strconv.ParseFloat(r[6+k], 64)
		}
		ms = append(ms, Match{ID: r[0], Date: d, Home: r[2], Away: r[3], HG: hg, AG: ag, League: "SYN"})
		odds[r[0]] = o
	}
	return ms, odds
}

// Golden values: any change to the fitting, prediction or backtest maths
// shows up here. Update deliberately (and say why in the commit).
func TestFixtureRegression(t *testing.T) {
	ms, odds := loadFixture(t)
	if len(ms) != 180 {
		t.Fatalf("fixture has %d matches", len(ms))
	}
	asOf := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	dco := DCOptions{Xi: 0.0019, MaxGoals: 10, MinMatches: 6, Prior: 4}
	elo := EloOptions{K: 20, HomeAdv: 65, Init: 1500, GoalDiffScale: true, DrawModel: "davidson"}
	p, err := FitDixonColes(ms, asOf, dco)
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := p.Predict("Kilima Stars", "Pwani FC", Adjust{})
	el := NewElo(elo)
	for _, m := range ms {
		el.Update(m)
	}
	eh, ed, ea := el.Probabilities("Kilima Stars", "Pwani FC", false)
	bt := WalkForward(ms, odds, BacktestOptions{Start: ms[90].Date, EdgeMin: 0.03, DC: dco, Elo: elo,
		BlendW: []float64{0.8, 0.2}, Kelly: 0.25, KellyCap: 0.02, DeVig: DeVigShin})
	got := map[string]float64{
		"home_adv": p.HomeAdv, "rho": p.Rho, "intercept": p.Intercept, "loglik": p.LogLik,
		"att_kilima": p.Attack["Kilima Stars"], "def_pwani": p.Defence["Pwani FC"],
		"p_home": pr.PHome, "p_draw": pr.PDraw, "over25": pr.Over["2.5"], "ah_m025": pr.AHHome["-0.25"],
		"elo_kilima": el.Rating("Kilima Stars"), "elo_ph": eh, "elo_pd": ed, "elo_pa": ea,
		"bt_n": float64(bt.N), "bt_rps": bt.RPS, "bt_mkt_rps": bt.MarketRPS, "bt_logloss": bt.LogLoss,
		"bt_bets": float64(len(bt.Bets)), "bt_pnl": bt.PnL,
	}
	if os.Getenv("FBMODEL_PRINT_GOLDEN") != "" {
		for _, k := range sortedKeys(got) {
			fmt.Printf("\t\t%q: %.10g,\n", k, got[k])
		}
	}
	for _, k := range sortedKeys(golden) {
		near(t, k, got[k], golden[k], 1e-6*math.Max(1, math.Abs(golden[k])))
	}
}

var golden = map[string]float64{
	"ah_m025":    0.5783192352,
	"att_kilima": 0.4468185587,
	"bt_bets":    69,
	"bt_logloss": 0.9939183397,
	"bt_mkt_rps": 0.1961172154,
	"bt_n":       90,
	"bt_pnl":     -0.2082828159,
	"bt_rps":     0.2043649908,
	"def_pwani":  0.2607338946,
	"elo_kilima": 1613.24285,
	"elo_pa":     0.1917115434,
	"elo_pd":     0.2725722178,
	"elo_ph":     0.5357162389,
	"home_adv":   0.2473432308,
	"intercept":  0.08154556654,
	"loglik":     -261.0047113,
	"over25":     0.5142370694,
	"p_draw":     0.2868176691,
	"p_home":     0.4953831477,
	"rho":        -0.1913990165,
}
