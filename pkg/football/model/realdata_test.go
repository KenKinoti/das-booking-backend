package model

// Real-data walk-forward backtests on football-data.co.uk CSVs. Skipped
// unless FBMODEL_DATA points at a directory laid out as <league>/<yyyy>.csv
// (e.g. E0/1415.csv = Premier League 2014/15). Nothing here is needed at run
// time; it documents and reproduces the numbers in README.md:
//
//	FBMODEL_DATA=/path/fd go test -run TestRealData -v -timeout 30m ./pkg/football/model/
//	FBMODEL_DATA=/path/fd FBMODEL_TUNE=1 go test -run TestRealDataTune -v -timeout 60m ./pkg/football/model/

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fdData struct {
	league  string
	ms      []Match
	odds    map[string][3]float64
	maxOdds map[string][3]float64 // market-maximum closing odds (2019/20+)
	avgOdds map[string][3]float64 // market-average closing odds (2019/20+)
	source  map[string]int        // odds column used → count
	season  map[string]string
	missing int
}

func parseFDDate(s string) (time.Time, bool) {
	for _, layout := range []string{"02/01/2006", "02/01/06", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func loadFD(t *testing.T, dir, league string) fdData {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, league, "*.csv"))
	sort.Strings(files)
	d := fdData{league: league, odds: map[string][3]float64{}, source: map[string]int{}, season: map[string]string{},
		maxOdds: map[string][3]float64{}, avgOdds: map[string][3]float64{}}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
		r := csv.NewReader(bytes.NewReader(raw))
		r.FieldsPerRecord = -1
		rows, err := r.ReadAll()
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		col := map[string]int{}
		for i, h := range rows[0] {
			col[strings.TrimSpace(h)] = i
		}
		get := func(row []string, name string) string {
			if i, ok := col[name]; ok && i < len(row) {
				return strings.TrimSpace(row[i])
			}
			return ""
		}
		season := strings.TrimSuffix(filepath.Base(f), ".csv")
		for n, row := range rows[1:] {
			date, ok := parseFDDate(get(row, "Date"))
			home, away := get(row, "HomeTeam"), get(row, "AwayTeam")
			hg, e1 := strconv.Atoi(get(row, "FTHG"))
			ag, e2 := strconv.Atoi(get(row, "FTAG"))
			if !ok || home == "" || away == "" || e1 != nil || e2 != nil {
				continue
			}
			if tm := get(row, "Time"); tm != "" {
				if hm, err := time.Parse("15:04", tm); err == nil {
					date = date.Add(time.Duration(hm.Hour())*time.Hour + time.Duration(hm.Minute())*time.Minute)
				}
			}
			id := fmt.Sprintf("%s-%s-%03d", league, season, n+1)
			m := Match{ID: id, Date: date, Home: home, Away: away, HG: hg, AG: ag, League: league}
			if x, err := strconv.ParseFloat(get(row, "HxG"), 64); err == nil {
				if y, err := strconv.ParseFloat(get(row, "AxG"), 64); err == nil {
					m.HxG, m.AxG = &x, &y
				}
			}
			d.ms = append(d.ms, m)
			d.season[id] = season
			triple := func(pre string) ([3]float64, bool) {
				var o [3]float64
				for k, s := range []string{"H", "D", "A"} {
					v, err := strconv.ParseFloat(get(row, pre+s), 64)
					if err != nil || v <= 1 {
						return o, false
					}
					o[k] = v
				}
				return o, true
			}
			found := false
			for _, pre := range []string{"PSC", "AvgC", "B365C"} {
				if o, ok := triple(pre); ok {
					d.odds[id] = o
					d.source[pre]++
					found = true
					break
				}
			}
			if o, ok := triple("MaxC"); ok {
				d.maxOdds[id] = o
			}
			if o, ok := triple("AvgC"); ok {
				d.avgOdds[id] = o
			}
			if !found {
				d.missing++
			}
		}
	}
	return d
}

func dataDir(t *testing.T) string {
	dir := os.Getenv("FBMODEL_DATA")
	if dir == "" {
		t.Skip("set FBMODEL_DATA to a football-data.co.uk CSV directory to run real-data backtests")
	}
	return dir
}

var (
	tuneStart = time.Date(2014, 7, 1, 0, 0, 0, 0, time.UTC)
	testStart = time.Date(2019, 7, 1, 0, 0, 0, 0, time.UTC)
)

// rowsScore scores a blend of the stored DC/Elo/market rows.
func rowsScore(rows []BacktestRow, w []float64, logPool bool) (rps, ll, brier float64) {
	n := 0.0
	for _, r := range rows {
		ps := []Probs{r.DC, r.Elo, r.Market}
		var p Probs
		if logPool {
			p = BlendLogLinear(ps, w)
		} else {
			p = Blend(ps, w)
		}
		rps += RPS(p, r.Outcome)
		ll += LogLoss(p, r.Outcome)
		brier += Brier(p, r.Outcome)
		n++
	}
	return rps / n, ll / n, brier / n
}

func TestRealDataTune(t *testing.T) {
	dir := dataDir(t)
	if os.Getenv("FBMODEL_TUNE") == "" {
		t.Skip("set FBMODEL_TUNE=1")
	}
	d := loadFD(t, dir, "E0")
	var hist []Match
	for _, m := range d.ms {
		if m.Date.Before(testStart) {
			hist = append(hist, m)
		}
	}
	t.Logf("EPL tuning: %d matches before %s, evaluated from %s", len(hist), testStart.Format("2006-01-02"), tuneStart.Format("2006-01-02"))
	mkt := WalkForward(hist, d.odds, BacktestOptions{Start: tuneStart, BlendW: []float64{0, 1}, KeepRows: true})
	t.Logf("market (Shin) on tuning seasons: N=%d RPS %.5f LL %.5f Brier %.5f", mkt.N, mkt.MarketRPS, mkt.MarketLogLoss, mkt.MarketBrier)
	for _, dv := range []string{DeVigProportional, DeVigPower} {
		r := WalkForward(hist, d.odds, BacktestOptions{Start: tuneStart, BlendW: []float64{0, 1}, DeVig: dv})
		t.Logf("market (%s): RPS %.5f LL %.5f", dv, r.MarketRPS, r.MarketLogLoss)
	}

	// 1. Elo grid.
	type eloRes struct {
		o       EloOptions
		rps, ll float64
	}
	var eres []eloRes
	for _, k := range []float64{8, 10, 12, 15, 20, 25} {
		for _, ha := range []float64{50, 65, 80, 100} {
			for _, off := range []float64{0, -50, -100} {
				for _, dm := range []string{"davidson", "ordlogit"} {
					eo := EloOptions{K: k, HomeAdv: ha, Init: 1500, GoalDiffScale: true, DrawModel: dm, NewTeamOffset: off}
					r := WalkForward(hist, d.odds, BacktestOptions{Start: tuneStart, BlendW: []float64{0, 1}, Elo: eo})
					eres = append(eres, eloRes{eo, r.RPS, r.LogLoss})
				}
			}
		}
	}
	sort.Slice(eres, func(i, j int) bool { return eres[i].rps < eres[j].rps })
	for _, e := range eres[:8] {
		t.Logf("elo K=%.0f HA=%.0f new=%+.0f %s: RPS %.5f LL %.5f", e.o.K, e.o.HomeAdv, e.o.NewTeamOffset, e.o.DrawModel, e.rps, e.ll)
	}
	bestElo := eres[0].o
	for _, gd := range []bool{false} {
		eo := bestElo
		eo.GoalDiffScale = gd
		r := WalkForward(hist, d.odds, BacktestOptions{Start: tuneStart, BlendW: []float64{0, 1}, Elo: eo})
		t.Logf("elo best without goal-difference scaling: RPS %.5f LL %.5f", r.RPS, r.LogLoss)
	}

	// 2. Dixon–Coles grid.
	type dcRes struct {
		o       DCOptions
		rps, ll float64
		rows    []BacktestRow
	}
	var dres []dcRes
	for _, xi := range []float64{0.001, 0.0015, 0.0019, 0.0025, 0.003} {
		for _, pr := range []float64{0, 2, 4, 8} {
			for _, nt := range []float64{0, -0.15, -0.3} {
				o := DCOptions{Xi: xi, MaxGoals: 10, MinMatches: 6, Prior: pr, NewTeamAttack: nt, NewTeamDefence: nt, MinWeight: 0.05}
				r := WalkForward(hist, d.odds, BacktestOptions{Start: tuneStart, BlendW: []float64{1, 0}, DC: o, Elo: bestElo, KeepRows: true})
				dres = append(dres, dcRes{o, r.RPS, r.LogLoss, r.Rows})
			}
		}
	}
	sort.Slice(dres, func(i, j int) bool { return dres[i].rps < dres[j].rps })
	for _, e := range dres[:12] {
		t.Logf("dc xi=%.4f prior=%.0f newteam=%+.2f: RPS %.5f LL %.5f", e.o.Xi, e.o.Prior, e.o.NewTeamAttack, e.rps, e.ll)
	}
	for _, e := range dres {
		if e.o.Xi == 0.0019 && e.o.Prior == 0 && e.o.NewTeamAttack == 0 {
			t.Logf("reference dc xi=0.0019 prior=0 newteam=0 (plain DC): RPS %.5f LL %.5f", e.rps, e.ll)
		}
	}
	best := dres[0]
	// MinWeight sensitivity for the best config.
	for _, mw := range []float64{0.02, 0.1} {
		o := best.o
		o.MinWeight = mw
		r := WalkForward(hist, d.odds, BacktestOptions{Start: tuneStart, BlendW: []float64{1, 0}, DC: o, Elo: bestElo})
		t.Logf("best dc with MinWeight %.2f: RPS %.5f LL %.5f", mw, r.RPS, r.LogLoss)
	}

	// 3. Blend weights on the stored rows (best DC rows + best Elo).
	full := WalkForward(hist, d.odds, BacktestOptions{Start: tuneStart, BlendW: []float64{1, 1}, DC: best.o, Elo: bestElo, KeepRows: true})
	for _, logPool := range []bool{false, true} {
		for _, wd := range []float64{1, 0.9, 0.8, 0.7, 0.65, 0.6, 0.5, 0.4} {
			rps, ll, br := rowsScore(full.Rows, []float64{wd, 1 - wd, 0}, logPool)
			t.Logf("blend DC %.1f / Elo %.1f (log=%v): RPS %.5f LL %.5f Brier %.5f", wd, 1-wd, logPool, rps, ll, br)
		}
	}
	for _, wm := range []float64{0.5, 0.7, 0.8, 0.9} {
		rps, ll, _ := rowsScore(full.Rows, []float64{0.8 * (1 - wm), 0.2 * (1 - wm), wm}, false)
		t.Logf("blend model %.1f / market %.1f: RPS %.5f LL %.5f", 1-wm, wm, rps, ll)
	}
}

type btLine struct {
	name string
	r    BacktestResult
}

func fmtBT(l btLine) string {
	r := l.r
	return fmt.Sprintf("| %s | %d | %.4f | %.4f | %+.4f ± %.4f | %.4f | %.4f | %.4f | %.4f | %.3f | %.3f |",
		l.name, r.N, r.RPS, r.MarketRPS, r.RPSDiff, r.RPSDiffSE, r.LogLoss, r.MarketLogLoss, r.Brier, r.MarketBrier, r.ECE, r.MarketECE)
}

func betLine(name string, r BacktestResult) string {
	return fmt.Sprintf("| %s | %d | %.2f | %+.1f%% | %+.1f%% ± %.1f%% | %+.1f%% | %.1f%% |",
		name, len(r.Bets), r.AvgOdds, 100*r.Yield, 100*r.FlatYield, 100*r.FlatYieldSE, 100*r.ROI, 100*r.MaxDrawdown)
}

// TestRealData runs the chosen defaults out of sample (from 2019/20) on
// every league in FBMODEL_DATA and prints Markdown tables.
func TestRealData(t *testing.T) {
	dir := dataDir(t)
	leagues := strings.Fields(os.Getenv("FBMODEL_LEAGUES"))
	if len(leagues) == 0 {
		leagues = []string{"E0", "D1", "SP1", "I1", "F1"}
	}
	var acc []btLine
	var bets []string
	var cal []string
	var shop []string
	for _, lg := range leagues {
		d := loadFD(t, dir, lg)
		if len(d.ms) == 0 {
			continue
		}
		t.Logf("%s: %d matches, odds sources %v, %d without closing odds", lg, len(d.ms), d.source, d.missing)
		start := time.Now()
		base := BacktestOptions{Start: testStart, KeepRows: true}
		r := WalkForward(d.ms, d.odds, base)
		t.Logf("%s walk-forward: %d refits in %v", lg, r.Refits, time.Since(start))
		acc = append(acc, btLine{lg + " model (DC+Elo)", r})
		for _, c := range r.Components {
			t.Logf("  %-12s N=%d RPS %.5f LL %.5f Brier %.5f", c.Name, c.N, c.RPS, c.LogLoss, c.Brier)
		}
		t.Logf("  draw rate %.3f, mean predicted draw %.3f", r.DrawRate, r.PredDrawRate)
		mb := WalkForward(d.ms, d.odds, BacktestOptions{Start: testStart, BlendW: []float64{0.16, 0.04, 0.8}})
		acc = append(acc, btLine{lg + " model+market (20/80)", mb})
		if lg == "E0" {
			tr := WalkForward(d.ms, d.odds, BacktestOptions{Start: tuneStart, End: testStart})
			for _, c := range tr.Components {
				t.Logf("  E0 tuning period 2014/15–2018/19 with the defaults: %-12s N=%d RPS %.5f LL %.5f Brier %.5f", c.Name, c.N, c.RPS, c.LogLoss, c.Brier)
			}
			// Per season.
			bySeason := map[string][]BacktestRow{}
			for _, row := range r.Rows {
				bySeason[d.season[row.MatchID]] = append(bySeason[d.season[row.MatchID]], row)
			}
			for _, s := range sortedKeys(bySeason) {
				rows := bySeason[s]
				var mp, kp []Probs
				var oc []int
				for _, row := range rows {
					mp = append(mp, row.Model)
					kp = append(kp, row.Market)
					oc = append(oc, row.Outcome)
				}
				a, b := Score("model", mp, oc), Score("market", kp, oc)
				t.Logf("  E0 %s: N=%d model RPS %.4f LL %.4f | market RPS %.4f LL %.4f", s, a.N, a.RPS, a.LogLoss, b.RPS, b.LogLoss)
			}
			for _, b := range r.Calib {
				cal = append(cal, fmt.Sprintf("| %.1f–%.1f | %d | %.3f | %.3f (%.3f–%.3f) |", b.Lo, b.Hi, b.N, b.MeanP, b.Freq, b.CILo, b.CIHi))
			}
		}
		// Betting at the market-average (≈ a typical bookmaker) and the
		// market-maximum closing price instead of Pinnacle's; and pure price
		// shopping (forecast = de-margined Pinnacle, bet at the best price).
		for _, edge := range []float64{0.02, 0.05} {
			ra := WalkForward(d.ms, d.odds, BacktestOptions{Start: testStart, EdgeMin: edge, BetOdds: d.avgOdds})
			shop = append(shop, betLine(fmt.Sprintf("%s model, average price, edge ≥ %.0f%%", lg, 100*edge), ra))
			rm := WalkForward(d.ms, d.odds, BacktestOptions{Start: testStart, EdgeMin: edge, BetOdds: d.maxOdds})
			shop = append(shop, betLine(fmt.Sprintf("%s model, best price, edge ≥ %.0f%%", lg, 100*edge), rm))
			rp := WalkForward(d.ms, d.odds, BacktestOptions{Start: testStart, EdgeMin: edge, BetOdds: d.maxOdds, BlendW: []float64{0, 0, 1}})
			shop = append(shop, betLine(fmt.Sprintf("%s Pinnacle fair p, best price, edge ≥ %.0f%%", lg, 100*edge), rp))
		}
		for _, edge := range []float64{0.02, 0.05, 0.10} {
			br := WalkForward(d.ms, d.odds, BacktestOptions{Start: testStart, EdgeMin: edge})
			bets = append(bets, betLine(fmt.Sprintf("%s edge ≥ %.0f%%", lg, 100*edge), br))
			if lg == "E0" {
				sel := map[string][2]float64{}
				for _, b := range br.Bets {
					v := sel[b.Selection]
					v[0]++
					v[1] += b.PnL / b.Stake
					sel[b.Selection] = v
				}
				for _, k := range sortedKeys(sel) {
					t.Logf("  E0 edge %.0f%% %s bets: %0.f, flat yield %+.1f%%", 100*edge, k, sel[k][0], 100*sel[k][1]/sel[k][0])
				}
			}
		}
	}
	fmt.Println("\n| Value bets at other closing prices (benchmark: Pinnacle) | bets | avg odds | yield (¼ Kelly) | flat yield ± SE | ROI | max drawdown |")
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, b := range shop {
		fmt.Println(b)
	}
	fmt.Println("\n| League / forecast | N | RPS | market RPS | RPS diff (model − market) | log loss | market LL | Brier | market Brier | ECE | market ECE |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|")
	for _, l := range acc {
		fmt.Println(fmtBT(l))
	}
	fmt.Println("\n| Value bets at closing odds | bets | avg odds | yield (¼ Kelly) | flat yield ± SE | ROI | max drawdown |")
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, b := range bets {
		fmt.Println(b)
	}
	fmt.Println("\n| EPL forecast bin | N | mean forecast | observed (95% CI) |")
	fmt.Println("|---|---|---|---|")
	for _, c := range cal {
		fmt.Println(c)
	}
	_ = math.Abs
}

// understatNames maps understat team names to football-data names.
var understatNames = map[string]string{"Manchester City": "Man City", "Manchester United": "Man United",
	"Newcastle United": "Newcastle", "Nottingham Forest": "Nottm Forest", "West Bromwich Albion": "West Brom",
	"Wolverhampton Wanderers": "Wolves"}

// attachUnderstatXG adds understat xG (vaastav/Fantasy-Premier-League
// data/<season>/understat/understat_<Team>.csv, MIT licence) to EPL matches.
func attachUnderstatXG(t *testing.T, root string, ms []Match) int {
	files, _ := filepath.Glob(filepath.Join(root, "*", "understat", "understat_*.csv"))
	type key struct {
		day  string
		home string
	}
	xg := map[key][2]float64{}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "understat_"), ".csv")
		name = strings.ReplaceAll(name, "_", " ")
		if m, ok := understatNames[name]; ok {
			name = m
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
		if err != nil || len(rows) < 2 {
			continue
		}
		col := map[string]int{}
		for i, h := range rows[0] {
			col[h] = i
		}
		for _, r := range rows[1:] {
			if r[col["h_a"]] != "h" {
				continue
			}
			x, e1 := strconv.ParseFloat(r[col["xG"]], 64)
			y, e2 := strconv.ParseFloat(r[col["xGA"]], 64)
			if e1 != nil || e2 != nil || len(r[col["date"]]) < 10 {
				continue
			}
			xg[key{r[col["date"]][:10], name}] = [2]float64{x, y}
		}
	}
	n := 0
	for i := range ms {
		k := key{ms[i].Date.Format("2006-01-02"), ms[i].Home}
		if v, ok := xg[k]; ok {
			a, b := v[0], v[1]
			ms[i].HxG, ms[i].AxG = &a, &b
			n++
		}
	}
	return n
}

func TestRealDataXG(t *testing.T) {
	dir := dataDir(t)
	root := os.Getenv("FBMODEL_XG")
	if root == "" {
		t.Skip("set FBMODEL_XG to vaastav/Fantasy-Premier-League/data")
	}
	d := loadFD(t, dir, "E0")
	n := attachUnderstatXG(t, root, d.ms)
	t.Logf("attached understat xG to %d EPL matches", n)
	periods := []struct {
		name       string
		start, end time.Time
	}{
		{"tune 2020/21–2021/22", time.Date(2020, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2022, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"test 2022/23–2024/25", time.Date(2022, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, p := range periods {
		var base []BacktestRow
		for _, u := range []float64{0, 0.25, 0.5, 0.75, 1} {
			o := DefaultDCOptions()
			o.UseXG = u
			r := WalkForward(d.ms, d.odds, BacktestOptions{Start: p.start, End: p.end, DC: o, BlendW: []float64{1, 0}, KeepRows: true})
			bl := WalkForward(d.ms, d.odds, BacktestOptions{Start: p.start, End: p.end, DC: o})
			line := fmt.Sprintf("%s UseXG=%.2f: DC RPS %.5f LL %.5f | DC+Elo RPS %.5f | market RPS %.5f (N=%d)", p.name, u, r.RPS, r.LogLoss, bl.RPS, r.MarketRPS, r.N)
			if u == 0 {
				base = r.Rows
			} else {
				var diff []float64
				for i := range r.Rows {
					diff = append(diff, RPS(r.Rows[i].Model, r.Rows[i].Outcome)-RPS(base[i].Model, base[i].Outcome))
				}
				m, se := meanSE(diff)
				line += fmt.Sprintf(" | vs goals-only %+.5f ± %.5f", m, se)
			}
			t.Log(line)
		}
	}
}

// TestRealDataExportFit writes one EPL fit window (rows with weights) and the
// fitted parameters to FBMODEL_XCHECK (a directory) so an independent
// implementation (scipy, see README) can re-fit and compare.
func TestRealDataExportFit(t *testing.T) {
	dir := dataDir(t)
	out := os.Getenv("FBMODEL_XCHECK")
	if out == "" {
		t.Skip("set FBMODEL_XCHECK to an output directory")
	}
	d := loadFD(t, dir, "E0")
	asOf := testStart
	o := DCOptions{Xi: 0.0019, MaxGoals: 10, Prior: 4, MinMatches: 6, MinWeight: 0.05}
	start := time.Now()
	p, err := FitDixonColes(d.ms, asOf, o)
	el := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fit: %d matches, %d teams, %d iterations, %v", p.Matches, len(p.Attack), p.Iter, el)
	var b strings.Builder
	b.WriteString("home,away,hg,ag,w\n")
	for _, m := range d.ms {
		if !m.Date.Before(asOf) {
			continue
		}
		w := math.Exp(-o.Xi * asOf.Sub(m.Date).Hours() / 24)
		if w < o.MinWeight {
			continue
		}
		fmt.Fprintf(&b, "%s,%s,%d,%d,%.17g\n", m.Home, m.Away, m.HG, m.AG, w)
	}
	if err := os.WriteFile(filepath.Join(out, "rows.csv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	fmt.Fprintf(&b, "param,value\nintercept,%.17g\nhome,%.17g\nrho,%.17g\nloglik,%.17g\n", p.Intercept, p.HomeAdv, p.Rho, p.LogLik)
	for _, tm := range sortedKeys(p.Attack) {
		fmt.Fprintf(&b, "att:%s,%.17g\ndef:%s,%.17g\n", tm, p.Attack[tm], tm, p.Defence[tm])
	}
	for _, tm := range sortedKeys(p.TeamMatches) {
		fmt.Fprintf(&b, "n:%s,%d\n", tm, p.TeamMatches[tm])
	}
	if err := os.WriteFile(filepath.Join(out, "params.csv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
