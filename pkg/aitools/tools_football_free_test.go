package aitools

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/gin-gonic/gin"
)

// The jackpot tools are read-only, post the pasted text to /jackpots/analyse
// (nothing saved) and condense the answer; football_match_analysis carries
// team news (confirmed vs reported), the source check, ClubElo and Kenyan odds.
func TestFootballFreeTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var posted map[string]any
	analysis := gin.H{"n_games": 2, "most_likely": gin.H{"picks": []string{"1", "2"}, "one_in": 7.1, "p": 0.14},
		"games": []gin.H{{"n": 1, "home": "Piast Gliwice", "away": "Korona Kielce", "probs": gin.H{"1": 0.37}, "pick": "1",
			"sources":  []gin.H{{"key": "jackpot", "used": true, "weight": 0.4}, {"key": "market", "used": true, "weight": 0.6}, {"key": "model", "used": false}},
			"evidence": []gin.H{{"kind": "team_news", "text": "Korona Kielce: 1 team-news fact(s), 0 confirmed"}}}},
		"top_tickets": []gin.H{{"rank": 1, "picks": []string{"1", "2"}, "one_in": 7.1}, {"rank": 2, "picks": []string{"X", "2"}, "one_in": 9}},
		"cover":       []gin.H{{"step": 1, "game": 1, "picks": "1X", "combinations": 2, "cost": 198}}, "at_least": []gin.H{{"k": 2, "p": 0.14}},
		"ev": gin.H{"ev_total": -60.5, "explain": "…"}, "contrarian": []gin.H{{"label": "Draw in game 1", "p_ratio": 0.7}}, "disclaimer": "18+ only."}
	r.POST("/api/v1/football/jackpots/analyse", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		json.Unmarshal(b, &posted)
		c.JSON(200, gin.H{"success": true, "data": gin.H{"jackpot": gin.H{"id": ""}, "games": []gin.H{{"n": 1, "home": "PIAST", "away": "KORONA", "match": gin.H{"status": "matched"}},
			{"n": 2, "home": "X", "away": "Y", "match": gin.H{"status": "unmatched"}}}, "analysis": analysis}})
	})
	r.GET("/api/v1/football/jackpots/:id", func(c *gin.Context) {
		c.JSON(200, gin.H{"success": true, "data": gin.H{"jackpot": gin.H{"id": c.Param("id"), "name": "Midweek"}, "games": []gin.H{}, "analysis": analysis,
			"grading": gin.H{"complete": true, "correct_most_likely": 1}}})
	})
	r.GET("/api/v1/football/jackpots", func(c *gin.Context) {
		c.JSON(200, gin.H{"success": true, "data": gin.H{"total": 1, "jackpots": []gin.H{{"id": "j1", "name": "Midweek", "status": c.Query("status"), "most_likely": gin.H{"one_in": 166222}}}}})
	})
	Engine, JWTSecret = r, "s3cret"
	t.Cleanup(func() { Engine, JWTSecret = nil, "" })
	t.Setenv("PUBLIC_APP_URL", "https://app.example")
	c := &Ctx{Context: context.Background(), Who: Identity{UserID: "u1", OrgID: "o1", Role: "admin", TZ: "Africa/Nairobi"}}

	an, ok := Get("football_jackpot_analyse")
	if !ok || !an.ReadOnly || an.Module != "football" || an.Destructive {
		t.Fatal("tool flags")
	}
	if _, err := an.Handler(c, map[string]any{}); err == nil {
		t.Fatal("no input accepted")
	}
	out, err := an.Handler(c, map[string]any{"text": "Piast v Korona 2.44 3.35 2.55\nX v Y 2 3 4", "prize": float64(22500447), "top_n": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if posted["text"] == nil || posted["prize"].(float64) != 22500447 || posted["top_n"].(float64) != 1 {
		t.Fatalf("posted %v", posted)
	}
	g := m["games"].([]map[string]any)[0]
	if g["sources_used"] != "jackpot 40%, market 60%" || len(m["top_tickets"].([]map[string]any)) != 1 || len(m["unmatched_games"].([]string)) != 1 || m["url"] != "https://app.example/football/jackpots" {
		t.Fatalf("result %v", m)
	}
	out, err = an.Handler(c, map[string]any{"jackpot_id": "j1"})
	if err != nil || out.(map[string]any)["url"] != "https://app.example/football/jackpots/j1" || out.(map[string]any)["grading"] == nil {
		t.Fatalf("by id %v %v", out, err)
	}
	ls, _ := Get("football_jackpots")
	out, err = ls.Handler(c, map[string]any{"status": "graded"})
	if err != nil || out.(map[string]any)["jackpots"].([]map[string]any)[0]["status"] != "graded" || !ls.ReadOnly {
		t.Fatalf("list %v %v", out, err)
	}
	// Match analysis summary additions.
	res := map[string]any{"model": map[string]any{}}
	footballFreeSummary(map[string]any{
		"team_news": map[string]any{"home": []any{map[string]any{"team": "Piast", "player": "J. Félix", "status": "injured", "confidence": "confirmed", "used_in_model": true,
			"sources": []any{map[string]any{"domain": "sport.pl", "url": "https://sport.pl/x"}}}, map[string]any{"player": "Old", "dismissed": true}}, "away": []any{}, "counts": map[string]any{"confirmed": 1}},
		"news":         map[string]any{"items": []any{map[string]any{"title": "T", "domain": "sport.pl", "url": "https://sport.pl/x"}}},
		"source_check": map[string]any{"summary": map[string]any{"status": "minor"}, "items": []any{map[string]any{"label": "position", "status": "minor"}, map[string]any{"label": "points", "status": "agree"}}},
		"clubelo":      map[string]any{"available": true, "probs": map[string]any{"home": 0.41}},
		"kenyan_odds":  map[string]any{"available": true, "bookmakers": []any{map[string]any{"title": "Betika (KE)", "prices": map[string]any{"home": 2.44}}}},
		"model":        map[string]any{"org_adjusted": true, "org_note": "n"},
	}, res)
	tn := res["team_news"].(map[string]any)["facts"].([]map[string]any)
	if len(tn) != 1 || tn[0]["confidence"] != "confirmed" || tn[0]["sources"].([]string)[0] != "sport.pl (https://sport.pl/x)" {
		t.Fatalf("team news %v", tn)
	}
	if len(res["source_check"].(map[string]any)["issues"].([]map[string]any)) != 1 || res["kenyan_odds"].(map[string]any)["available"] != true || res["model"].(map[string]any)["org_adjusted"] != true {
		t.Fatalf("summary %v", res)
	}
}
