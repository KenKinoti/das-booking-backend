package football

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func router(s *Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1", func(c *gin.Context) {
		c.Set("organization_id", c.GetHeader("X-Org"))
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("user_role", c.GetHeader("X-Role"))
	})
	(&Handler{S: s}).RegisterRoutes(g)
	return r
}

func do(r http.Handler, method, path, org, user, role string, body any) (int, map[string]any) {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Org", org)
	req.Header.Set("X-User", user)
	req.Header.Set("X-Role", role)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestHandlers(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	s := newSvc(t, db, now)
	f := seedMatch(t, db, 5000010, now.Add(30*time.Hour))
	db.Create(&[]Team{{ID: 9001, Name: "Northbridge Athletic"}, {ID: 9002, Name: "Kingsport United"}})
	r := router(s)

	// Platform settings: super admins only; keys never returned.
	if code, _ := do(r, "GET", "/api/v1/super-admin/football/settings", "o1", "u1", "admin", nil); code != 403 {
		t.Fatal("admin read platform settings")
	}
	code, out := do(r, "PUT", "/api/v1/super-admin/football/settings", "o1", "su", "super_admin", map[string]any{"apifootball_key": "af-secret-key-123", "enabled_competitions": []int{39, 276}})
	b, _ := json.Marshal(out)
	if code != 200 || strings.Contains(string(b), "af-secret-key-123") || !strings.Contains(string(b), "••••-123") {
		t.Fatalf("settings %d %s", code, b)
	}
	var en []int64
	db.Model(&Competition{}).Where("enabled").Order("id").Pluck("id", &en)
	if len(en) != 2 || en[0] != 39 || en[1] != 276 {
		t.Fatalf("enabled %v", en)
	}
	if code, _ := do(r, "PUT", "/api/v1/super-admin/football/settings", "o1", "su", "super_admin", map[string]any{"apifootball_plan": "gold"}); code != 400 {
		t.Fatal("bad plan accepted")
	}
	// The test clock endpoint exists only with FOOTBALL_TEST_CLOCK.
	if code, _ := do(r, "POST", "/api/v1/super-admin/football/test/clock", "o1", "su", "super_admin", map[string]any{"now": now}); code != 200 {
		t.Fatal("test clock")
	}
	s2 := newSvc(t, db, now)
	s2.TestClock = false
	if code, _ := do(router(s2), "POST", "/api/v1/super-admin/football/test/clock", "o1", "su", "super_admin", map[string]any{"now": now}); code != 404 {
		t.Fatal("test clock without the switch")
	}
	// Bets: 201, 400 with a clear message, 409 over a limit, 404 for someone else's.
	code, out = do(r, "POST", "/api/v1/football/bets", "o1", "u1", "admin", map[string]any{"fixture_id": f.ID, "market": "1x2", "selection": "home", "odds": 2.1, "stake": 100, "bookmaker": "SportPesa"})
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	id := out["data"].(map[string]any)["id"].(string)
	code, out = do(r, "POST", "/api/v1/football/bets", "o1", "u1", "admin", map[string]any{"fixture_id": f.ID, "market": "ou", "selection": "over", "odds": 2.1, "stake": 100, "bookmaker": "SportPesa"})
	if code != 400 || !strings.Contains(out["message"].(string), "goals line") {
		t.Fatalf("validation %d %v", code, out)
	}
	do(r, "PUT", "/api/v1/football/limits", "o1", "u1", "admin", map[string]any{"daily_stake": 150})
	code, out = do(r, "POST", "/api/v1/football/bets", "o1", "u1", "admin", map[string]any{"fixture_id": f.ID, "market": "1x2", "selection": "draw", "odds": 3.1, "stake": 100, "bookmaker": "Betika"})
	if code != 409 || out["error"].(map[string]any)["code"] != "LIMIT_REACHED" || out["error"].(map[string]any)["until"] == nil {
		t.Fatalf("limit %d %v", code, out)
	}
	if code, _ := do(r, "GET", "/api/v1/football/bets/"+id, "o2", "u9", "admin", nil); code != 404 {
		t.Fatal("cross-organisation read")
	}
	if code, out := do(r, "GET", "/api/v1/football/bets", "o1", "u2", "manager", nil); code != 200 || out["data"].(map[string]any)["total"].(float64) != 0 {
		t.Fatal("a colleague's list")
	}
	// Reads.
	for _, p := range []string{"/api/v1/football/meta", "/api/v1/football/competitions", "/api/v1/football/fixtures?status=upcoming", "/api/v1/football/value",
		"/api/v1/football/model/performance?competition=39", "/api/v1/football/live", "/api/v1/football/bets/summary", "/api/v1/football/limits",
		"/api/v1/football/watchlist", "/api/v1/football/odds/manual", "/api/v1/football/bankroll", "/api/v1/football/standings/39",
		"/api/v1/football/teams/9001", "/api/v1/football/fixtures/5000010/analysis"} {
		if code, out := do(r, "GET", p, "o1", "u1", "admin", nil); code != 200 || out["success"] != true {
			t.Errorf("%s → %d %v", p, code, out["message"])
		}
	}
	if code, out := do(r, "GET", "/api/v1/football/fixtures/5000010/kelly?market=ou&selection=over&odds=2", "o1", "u1", "admin", nil); code != 400 || !strings.Contains(out["message"].(string), "line") {
		t.Fatalf("kelly without a line: %d %v", code, out)
	}
	if code, _ := do(r, "GET", "/api/v1/football/fixtures/999/kelly?market=1x2&selection=home&odds=2", "o1", "u1", "admin", nil); code != 404 {
		t.Fatal("kelly unknown fixture")
	}
	if code, _ := do(r, "GET", "/api/v1/football/fixtures/999/analysis", "o1", "u1", "admin", nil); code != 404 {
		t.Fatal("unknown fixture")
	}
	if code, _ := do(r, "GET", "/api/v1/football/fixtures?date=21/11/2026", "o1", "u1", "admin", nil); code != 400 {
		t.Fatal("bad date")
	}
	if code, _ := do(r, "GET", "/api/v1/football/fixtures", "", "", "admin", nil); code != 401 {
		t.Fatal("no organisation")
	}
}
