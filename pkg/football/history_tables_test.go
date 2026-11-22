package football

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestHistoryTablesFromXgabora recomputes the embedded History table from
// Club Football Match Data (https://github.com/xgabora/Club-Football-Match-Data-2000-2025,
// MIT) and checks every figure within rounding. Skipped by default:
//
//	FBMODEL_XGABORA=/home/claude/fbdata-sets/xgabora/data/Matches.csv go test ./pkg/football -run TestHistoryTablesFromXgabora -v
//
// FBMODEL_XGABORA_PRINT=1 also prints the figures for history_tables.go.
// Method: matches from 1 July 2019 with all three 1X2 odds (OddHome,
// OddDraw, OddAway — Bet365) and a result; proportional de-margin; the
// favourite is the highest chance; tiers on that chance.
func TestHistoryTablesFromXgabora(t *testing.T) {
	path := os.Getenv("FBMODEL_XGABORA")
	if path == "" {
		t.Skip("FBMODEL_XGABORA not set (path to the xgabora Matches.csv)")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	idx := map[string]int{}
	for i, h := range head {
		idx[strings.TrimSpace(h)] = i
	}
	for _, c := range []string{"Division", "MatchDate", "FTHome", "FTAway", "FTResult", "OddHome", "OddDraw", "OddAway"} {
		if _, ok := idx[c]; !ok {
			t.Fatalf("column %s missing", c)
		}
	}
	tiers := map[string]*histAgg{}
	groups := map[string]map[string]*histAgg{}
	all := &histAgg{}
	scores := map[string]int{}
	divs := map[string]bool{}
	drawFav, over25, btts := 0, 0, 0
	from, to := "9999", ""
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		get := func(c string) string { return strings.TrimSpace(rec[idx[c]]) }
		date := get("MatchDate")
		if date < "2019-07-01" {
			continue
		}
		res := get("FTResult")
		if res != "H" && res != "D" && res != "A" {
			continue
		}
		var o [3]float64
		okOdds := true
		for i, c := range []string{"OddHome", "OddDraw", "OddAway"} {
			v, err := strconv.ParseFloat(get(c), 64)
			if err != nil || v <= 1 {
				okOdds = false
			}
			o[i] = v
		}
		if !okOdds {
			continue
		}
		p, _ := DeMarginProportional(o)
		out := map[string]int{"H": 0, "D": 1, "A": 2}[res]
		order := []int{0, 1, 2}
		sort.SliceStable(order, func(a, b int) bool { return p[order[a]] > p[order[b]] })
		k := order[0]
		if k == 1 {
			drawFav++
		}
		tk := tierOf(p[k])
		other := 1
		if k == 1 {
			other = order[1]
		}
		div := get("Division")
		divs[div] = true
		g := histDivGroup(div)
		if groups[g] == nil {
			groups[g] = map[string]*histAgg{}
		}
		for _, a := range []*histAgg{getAgg(tiers, tk), getAgg(groups[g], tk), getAgg(groups[g], "all"), all} {
			a.n++
			if out == k {
				a.fav++
			}
			if out == order[0] || out == order[1] {
				a.dc++
			}
			if out == k || out == other {
				a.pod++
			}
			if out == 1 {
				a.draw++
			}
		}
		hg, _ := strconv.ParseFloat(get("FTHome"), 64)
		ag, _ := strconv.ParseFloat(get("FTAway"), 64)
		scores[fmt.Sprintf("%d-%d", int(hg), int(ag))]++
		if hg+ag > 2.5 {
			over25++
		}
		if hg > 0 && ag > 0 {
			btts++
		}
		if date < from {
			from = date
		}
		if date > to {
			to = date
		}
	}
	rnd := func(x float64) float64 { return math.Round(x*10000) / 10000 }
	chk := func(what string, got, want float64) {
		if math.Abs(got-want) > 0.00051 {
			t.Errorf("%s: computed %.4f, embedded %.4f", what, got, want)
		}
	}
	if all.n != History.N || len(divs) != History.Divisions || from != History.From || to != History.To {
		t.Fatalf("sample: %d matches, %d divisions, %s → %s (embedded %d, %d, %s → %s)", all.n, len(divs), from, to, History.N, History.Divisions, History.From, History.To)
	}
	for _, tr := range History.Tiers {
		a := tiers[tr.Key]
		if a.n != tr.N {
			t.Errorf("%s n %d vs %d", tr.Key, a.n, tr.N)
		}
		chk(tr.Key+" share", float64(a.n)/float64(all.n), tr.Share)
		chk(tr.Key+" favourite won", float64(a.fav)/float64(a.n), tr.FavouriteWon)
		chk(tr.Key+" double chance", float64(a.dc)/float64(a.n), tr.DoubleChanceWon)
		chk(tr.Key+" pick or draw", float64(a.pod)/float64(a.n), tr.PickOrDrawWon)
		chk(tr.Key+" draw rate", float64(a.draw)/float64(a.n), tr.DrawRate)
	}
	chk("favourite overall", float64(all.fav)/float64(all.n), History.Overall.FavouriteWon)
	chk("draw rate", float64(all.draw)/float64(all.n), History.Overall.DrawRate)
	chk("over 2.5", float64(over25)/float64(all.n), History.Overall.Over25)
	chk("btts", float64(btts)/float64(all.n), History.Overall.BTTS)
	if drawFav != History.Overall.DrawFavouriteMatches {
		t.Errorf("draw favourite %d vs %d", drawFav, History.Overall.DrawFavouriteMatches)
	}
	for _, sc := range History.Scores {
		chk("score "+sc.Score, float64(scores[sc.Score])/float64(all.n), sc.Share)
	}
	for _, hg := range History.Groups {
		ga := groups[hg.Group]["all"]
		if ga == nil || ga.n != hg.N {
			t.Errorf("group %s n", hg.Group)
			continue
		}
		chk(hg.Group+" favourite", float64(ga.fav)/float64(ga.n), hg.FavouriteWon)
		chk(hg.Group+" draws", float64(ga.draw)/float64(ga.n), hg.DrawRate)
		for _, tr := range hg.Tiers {
			a := groups[hg.Group][tr.Key]
			if a.n != tr.N {
				t.Errorf("%s/%s n %d vs %d", hg.Group, tr.Key, a.n, tr.N)
			}
			chk(hg.Group+"/"+tr.Key+" favourite", float64(a.fav)/float64(a.n), tr.FavouriteWon)
			chk(hg.Group+"/"+tr.Key+" double chance", float64(a.dc)/float64(a.n), tr.DoubleChanceWon)
		}
	}
	if os.Getenv("FBMODEL_XGABORA_PRINT") == "1" {
		for _, k := range TierKeys {
			a := tiers[k]
			t.Logf("ht(%q, %.4f, %d, %.4f, %.4f, %.4f, %.4f)", k, rnd(float64(a.n)/float64(all.n)), a.n, rnd(float64(a.fav)/float64(a.n)),
				rnd(float64(a.dc)/float64(a.n)), rnd(float64(a.pod)/float64(a.n)), rnd(float64(a.draw)/float64(a.n)))
		}
		t.Logf("overall fav %.4f draws %.4f drawfav %d over25 %.4f btts %.4f, %d matches, %d divisions, %s → %s", rnd(float64(all.fav)/float64(all.n)),
			rnd(float64(all.draw)/float64(all.n)), drawFav, rnd(float64(over25)/float64(all.n)), rnd(float64(btts)/float64(all.n)), all.n, len(divs), from, to)
	}
}

type histAgg struct{ n, fav, dc, pod, draw int }

func getAgg(m map[string]*histAgg, k string) *histAgg {
	if m[k] == nil {
		m[k] = &histAgg{}
	}
	return m[k]
}
