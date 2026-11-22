package aitools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The v2.18 tools are read-only and condense the real payloads (captured from
// the seeded env: /home/claude/tools/football-winrate-*.json shapes): verdict
// lines on football_fixtures, the scorecard, picks strongest first and the
// jackpot review / per-game marks. Wording stays honest (no "sure", "banker",
// "guaranteed", "fixed").
func TestFootballWinTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	verdict := gin.H{"pick": "1", "pick_label": "Arsenal win", "p": 0.6412, "kind": "live",
		"tier":       gin.H{"key": "strong", "label": "Strong", "history": gin.H{"hit_rate": 0.7183, "n": 12719}, "own": gin.H{"hit_rate": 0.69, "n": 41}, "note": ""},
		"safer":      gin.H{"selection": "1X", "label": "Arsenal or draw", "p": 0.86},
		"draw_alert": gin.H{"on": false, "why": ""}, "goals": gin.H{"line": 2.5, "pick": "over", "p": 0.56}, "btts": gin.H{"pick": "no", "p": 0.52},
		"top_scores": []gin.H{{"score": "2-0", "p": 0.12}, {"score": "1-0", "p": 0.11}}}
	var lastQ map[string]string
	r.GET("/api/v1/football/fixtures", func(c *gin.Context) {
		c.JSON(200, gin.H{"success": true, "data": gin.H{"fixtures": []gin.H{
			{"id": 9300001, "kickoff": "2026-11-22T16:30:00Z", "status": "NS", "home": gin.H{"name": "Arsenal"}, "away": gin.H{"name": "Chelsea"}, "competition": gin.H{"name": "Premier League", "id": 39},
				"verdict": verdict, "result_check": nil},
			{"id": 9300010, "kickoff": "2026-11-21T14:30:00Z", "status": "FT", "home": gin.H{"name": "Arsenal"}, "away": gin.H{"name": "Brentford"}, "competition": gin.H{"name": "Premier League", "id": 39},
				"score": gin.H{"home": 1, "away": 1}, "verdict": verdict, "result_check": gin.H{"label": "Wrong", "score": "1-1", "summary": "Wrong — it was a draw, 1-1 (the pick was Arsenal win, 64%)."}}}}})
	})
	r.GET("/api/v1/football/scorecard", func(c *gin.Context) {
		lastQ = map[string]string{"period": c.Query("period"), "tier": c.Query("tier"), "competition": c.Query("competition")}
		rows := []gin.H{}
		for i := 0; i < 25; i++ {
			rows = append(rows, gin.H{"fixture_id": 9100101 + i, "home": "Piast Gliwice", "away": "Korona Kielce", "pick_label": "Piast Gliwice win", "p": 0.37, "tier": "coinflip", "status": "correct", "summary": "Correct"})
		}
		c.JSON(200, gin.H{"success": true, "data": gin.H{"period": gin.H{"key": "weekend", "label": "Weekend of Fri 20 – Mon 23 Nov"},
			"totals":  gin.H{"graded": 13, "correct": 4, "expected": 5.16, "expected_low": 3, "expected_high": 7, "verdict": "bad_luck", "verdict_text": "4 of 13 right. …"},
			"by_tier": []gin.H{{"tier": "coinflip", "label": "Coin-flip", "graded": 13, "correct": 4, "hit_rate": 0.3077, "expected_rate": 0.397, "history_hit_rate": 0.3847}},
			"rows":    rows, "jackpots": []gin.H{{"name": "SportPesa Midweek 20 Nov", "correct": 4, "of": 13}}}})
	})
	r.GET("/api/v1/football/picks", func(c *gin.Context) {
		lastQ = map[string]string{"min_tier": c.Query("min_tier"), "limit": c.Query("limit")}
		c.JSON(200, gin.H{"success": true, "data": gin.H{"picks": []gin.H{{"fixture_id": 9300001, "kickoff": "2026-11-22T16:30:00Z", "competition": gin.H{"name": "Premier League"},
			"home": gin.H{"name": "Arsenal"}, "away": gin.H{"name": "Chelsea"}, "verdict": verdict}}, "count": 1, "accumulator": gin.H{"note": "multiply"}}})
	})
	jp, _ := os.ReadFile("/home/claude/tools/football-winrate-jackpot-review.json")
	r.GET("/api/v1/football/jackpots/:id", func(c *gin.Context) {
		if len(jp) == 0 {
			c.JSON(200, gin.H{"success": true, "data": gin.H{"jackpot": gin.H{"id": c.Param("id")}, "games": []gin.H{{"n": 1, "home": "A", "away": "B", "match": gin.H{"status": "matched"},
				"result_check": gin.H{"label": "Correct", "result": "1", "score": "2-0", "summary": "Correct"}}}, "analysis": gin.H{"games": []gin.H{{"n": 1, "verdict": verdict}}},
				"review": gin.H{"correct": 4, "of": 13, "safer_hits": 8, "text": []string{"x"}}}})
			return
		}
		c.Data(200, "application/json", jp)
	})
	Engine, JWTSecret = r, "s3cret"
	t.Cleanup(func() { Engine, JWTSecret = nil, "" })
	t.Setenv("PUBLIC_APP_URL", "https://app.example")
	c := &Ctx{Context: context.Background(), Who: Identity{UserID: "u1", OrgID: "o1", Role: "admin", TZ: "Africa/Nairobi"}}

	fx, _ := Get("football_fixtures")
	out, err := fx.Handler(c, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	items := out.(map[string]any)["fixtures"].([]map[string]any)
	v := items[0]["verdict"].(map[string]any)
	if v["line"] != "Arsenal win · 64% · Strong" || v["safer"] != "Arsenal or draw · 86%" || v["tier_history_hit_rate"] != 0.7183 || v["goals"] != "over 2.5 · 56%" ||
		v["top_scores"] != "2-0 (12%), 1-0 (11%)" || items[0]["result_check"] != nil {
		t.Fatalf("verdict line %v", v)
	}
	if rc := items[1]["result_check"].(map[string]any); rc["label"] != "Wrong" || rc["score"] != "1-1" {
		t.Fatalf("result check %v", rc)
	}
	sc, ok := Get("football_scorecard")
	if !ok || !sc.ReadOnly || sc.Destructive || sc.Module != "football" {
		t.Fatal("scorecard flags")
	}
	out, err = sc.Handler(c, map[string]any{"period": "weekend", "tier": "coinflip", "competition": float64(39)})
	if err != nil || lastQ["period"] != "weekend" || lastQ["tier"] != "coinflip" || lastQ["competition"] != "39" {
		t.Fatalf("scorecard %v %v", err, lastQ)
	}
	m := out.(map[string]any)
	if len(m["rows"].([]map[string]any)) != 20 || m["url"] != "https://app.example/football/scorecard" || m["totals"].(map[string]any)["correct"].(float64) != 4 {
		t.Fatalf("scorecard result %v", m)
	}
	pk, ok := Get("football_picks")
	if !ok || !pk.ReadOnly {
		t.Fatal("picks flags")
	}
	out, err = pk.Handler(c, map[string]any{"min_tier": "strong", "limit": float64(5)})
	if err != nil || lastQ["min_tier"] != "strong" || lastQ["limit"] != "5" {
		t.Fatalf("picks %v %v", err, lastQ)
	}
	p0 := out.(map[string]any)["picks"].([]map[string]any)[0]
	if p0["verdict"].(map[string]any)["line"] != "Arsenal win · 64% · Strong" || p0["url"] != "https://app.example/football/match/9300001" {
		t.Fatalf("pick %v", p0)
	}
	an, _ := Get("football_jackpot_analyse")
	out, err = an.Handler(c, map[string]any{"jackpot_id": "6b1f0c1e-0000-4000-8000-00000000000a"})
	if err != nil {
		t.Fatal(err)
	}
	res := out.(map[string]any)
	rv, ok := res["review"].(map[string]any)
	if !ok || rv["correct"].(float64) != 4 || rv["safer_hits"].(float64) != 8 || len(res["marks"].([]map[string]any)) == 0 {
		t.Fatalf("jackpot review %v", res["review"])
	}
	if g := res["games"].([]map[string]any)[0]; g["tier"] == nil || g["tier"] == "" {
		t.Fatalf("game tier %v", g)
	}
	b, _ := json.Marshal([]any{items, m, res})
	for _, w := range []string{"sure bet", "banker", "guaranteed win", "fixed match"} {
		if strings.Contains(strings.ToLower(string(b)), w) {
			t.Fatalf("output contains %q", w)
		}
	}
	for _, name := range []string{"football_scorecard", "football_picks"} {
		tl, _ := Get(name)
		low := strings.ToLower(tl.Description)
		if strings.Contains(low, "banker") || strings.Contains(low, "guaranteed") || !strings.Contains(low, "estimate") {
			t.Fatalf("%s description wording", name)
		}
	}
}
