package aitools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The football tools read /football/… as the caller; ids decoded from JSON
// are formatted without an exponent; recording a bet is destructive (always
// confirmed) and posts exactly what the user agreed to.
func TestFootballTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var posted map[string]any
	paths := []string{}
	fixture := gin.H{"id": 1200146, "kickoff": "2026-11-21T17:30:00Z", "status": "NS", "competition": gin.H{"id": 39, "name": "Premier League"},
		"home": gin.H{"name": "Coldwater Rangers"}, "away": gin.H{"name": "Brackenfield"}, "score": gin.H{"home": nil, "away": nil},
		"model": gin.H{"home": 0.42, "draw": 0.25, "away": 0.33}, "market_1x2": gin.H{"home": 0.4, "draw": 0.27, "away": 0.33}, "market_source": "pinnacle (de-margined)",
		"best_odds": gin.H{"home": gin.H{"price": 2.5, "title": "Betway"}}, "value_count": 1,
		"edges": []gin.H{{"label": "Coldwater Rangers to win", "model_p": 0.42, "market_p": 0.4, "edge": 0.02, "best_price": 2.6, "bookmaker": "Betway", "ev": 0.09}}}
	r.GET("/api/v1/football/fixtures", func(c *gin.Context) {
		paths = append(paths, c.Request.URL.RequestURI())
		c.JSON(200, gin.H{"success": true, "data": gin.H{"fixtures": []gin.H{fixture}, "timezone": "Africa/Nairobi", "last_run_at": "2026-11-21T17:00:00Z", "disclaimer": "18+ only."}})
	})
	r.GET("/api/v1/football/fixtures/:id/analysis", func(c *gin.Context) {
		paths = append(paths, c.Request.URL.Path)
		c.JSON(200, gin.H{"success": true, "data": gin.H{"fixture": gin.H{"id": 1200146, "home": gin.H{"name": "Coldwater Rangers"}, "away": gin.H{"name": "Brackenfield"}, "competition": gin.H{"name": "Premier League"}},
			"as_of": "2026-11-21T17:00:00Z", "stale": false, "disclaimer": "18+ only.", "model": gin.H{"kind": "current", "blend": gin.H{"home": 0.42}},
			"market": gin.H{"selections": []gin.H{{"market": "1x2", "selection": "home", "label": "Coldwater Rangers to win", "fair_p": 0.4, "best": gin.H{"price": 2.6, "title": "Betway"}}}},
			"value": []gin.H{{"label": "Coldwater Rangers to win", "value": true, "edge": 0.02}}, "players": gin.H{"home": gin.H{"missing": []gin.H{{"name": "A. Player", "availability": "injured"}}}, "away": gin.H{}},
			"track_record": gin.H{"n": 5, "rps": 0.19}, "sources": []gin.H{{"provider": "apifootball", "object": "fixture", "fetched_at": "2026-11-21T17:00:00Z"}}}})
	})
	r.POST("/api/v1/football/bets", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		json.Unmarshal(b, &posted)
		c.JSON(http.StatusCreated, gin.H{"success": true, "data": gin.H{"id": "b1", "label": "Coldwater Rangers to win", "odds": 2.6, "stake": 500, "currency": "KES", "status": "open", "home": gin.H{"name": "Coldwater Rangers"}}})
	})
	Engine, JWTSecret = r, "s3cret"
	t.Cleanup(func() { Engine, JWTSecret = nil, "" })
	t.Setenv("PUBLIC_APP_URL", "https://app.example")
	c := &Ctx{Context: context.Background(), Who: Identity{UserID: "u1", OrgID: "o1", Role: "admin", TZ: "Africa/Nairobi"}}

	fxT, _ := Get("football_fixtures")
	out, err := fxT.Handler(c, map[string]any{"date": "2026-11-21", "limit": float64(5)})
	if err != nil || !fxT.ReadOnly || fxT.Module != "football" {
		t.Fatal(err)
	}
	card := out.(map[string]any)["fixtures"].([]map[string]any)[0]
	if card["url"] != "https://app.example/football/match/1200146" || card["best_odds"].(map[string]any)["home"] != "2.5 (Betway)" || card["value"] == nil {
		t.Fatalf("card %v", card)
	}
	an, _ := Get("football_match_analysis")
	out, err = an.Handler(c, map[string]any{"q": "Coldwater Rangers v Brackenfield"})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["url"] != "https://app.example/football/match/1200146" || m["as_of"] == nil || len(m["sources"].([]map[string]any)) != 1 || len(m["value"].([]map[string]any)) != 1 {
		t.Fatalf("analysis %v", m)
	}
	if paths[len(paths)-1] != "/api/v1/football/fixtures/1200146/analysis" || !strings.Contains(paths[1], "q=coldwater+rangers") {
		t.Fatalf("paths %v", paths)
	}
	if _, err := an.Handler(c, map[string]any{"q": "Nobody v Nothing"}); err == nil {
		t.Fatal("unknown match")
	}
	rb, _ := Get("football_record_bet")
	if !rb.Destructive || rb.ReadOnly {
		t.Fatal("record bet must always be confirmed")
	}
	if _, err := rb.Handler(c, map[string]any{"fixture_id": float64(1200146), "market": "1x2"}); err == nil {
		t.Fatal("missing fields accepted")
	}
	out, err = rb.Handler(c, map[string]any{"fixture_id": float64(1200146), "market": "1x2", "selection": "home", "odds": 2.6, "stake": float64(500), "bookmaker": "SportPesa", "placed_at": "2026-11-21T16:45"})
	if err != nil {
		t.Fatal(err)
	}
	if posted["fixture_id"] != float64(1200146) || posted["odds"] != 2.6 || posted["stake"] != float64(500) || posted["bookmaker"] != "SportPesa" || !strings.HasPrefix(posted["placed_at"].(string), "2026-11-21T16:45:00+03:00") {
		t.Fatalf("posted %v", posted)
	}
	if out.(map[string]any)["label"] != "Coldwater Rangers to win" {
		t.Fatal(out)
	}
	for _, n := range []string{"football_value_bets", "football_bets_summary"} {
		if tl, ok := Get(n); !ok || !tl.ReadOnly || Area(tl) != "Football analysis" {
			t.Fatalf("%s %+v", n, tl)
		}
	}
}
