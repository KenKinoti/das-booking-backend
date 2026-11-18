package aitools

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// brief_today reads /brief/today as the caller and returns the headline,
// the sections with absolute links and the delivery status.
func TestBriefTodayTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var seen []string
	h := func(c *gin.Context) {
		tok, _ := jwt.Parse(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "), func(*jwt.Token) (any, error) { return []byte("s3cret"), nil })
		claims, _ := tok.Claims.(jwt.MapClaims)
		seen = append(seen, c.Request.URL.Path+"|"+claims["user_id"].(string))
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"live": true, "url": "https://app.example/brief/2026-11-19",
			"brief": gin.H{"date": "2026-11-19", "date_label": "Thu 19 Nov", "timezone": "Africa/Nairobi", "currency": "KES",
				"headline": "2 bills due this week (KES 41,200), 1 Jira issue overdue",
				"sections": []gin.H{{"key": "bills", "title": "Bills & payables", "summary": "KES 41,200 due this week", "link": "/bills?view=upcoming",
					"groups": []gin.H{{"key": "week", "title": "Due later this week", "more": 2, "items": []gin.H{{"title": "Synergy Wholesale — Hosting", "value": "KES 18,500", "link": "/bills?bill=b1", "tag": "New"}}}}}}},
			"delivery": gin.H{"status": "sent", "email_status": "sent", "whatsapp_status": "read", "late": false}}})
	}
	r.GET("/api/v1/brief/today", h)
	r.GET("/api/v1/brief/day/:date", h)
	Engine, JWTSecret = r, "s3cret"
	t.Cleanup(func() { Engine, JWTSecret = nil, "" })
	t.Setenv("PUBLIC_APP_URL", "https://app.example")
	tool, ok := Get("brief_today")
	if !ok || !tool.ReadOnly || tool.Module != "core" {
		t.Fatalf("tool %+v", tool)
	}
	c := &Ctx{Context: context.Background(), Who: Identity{UserID: "u1", OrgID: "o1", Role: "admin", TZ: "Africa/Nairobi"}}
	out, err := tool.Handler(c, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["headline"] != "2 bills due this week (KES 41,200), 1 Jira issue overdue" || m["date"] != "2026-11-19" {
		t.Fatalf("out %v", m)
	}
	secs := m["sections"].([]map[string]any)
	g := secs[0]["groups"].([]map[string]any)[0]
	it := g["items"].([]map[string]any)[0]
	if secs[0]["url"] != "https://app.example/bills?view=upcoming" || it["url"] != "https://app.example/bills?bill=b1" || g["more"] != float64(2) {
		t.Fatalf("section %v", secs[0])
	}
	if d := m["delivery"].(map[string]any); d["whatsapp_status"] != "read" {
		t.Fatalf("delivery %v", d)
	}
	if _, err := tool.Handler(c, map[string]any{"date": "2026-11-17"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Handler(c, map[string]any{"date": "17/11/2026"}); err == nil {
		t.Fatal("bad date accepted")
	}
	if len(seen) != 2 || seen[0] != "/api/v1/brief/today|u1" || seen[1] != "/api/v1/brief/day/2026-11-17|u1" {
		t.Fatalf("paths %v", seen)
	}
}
