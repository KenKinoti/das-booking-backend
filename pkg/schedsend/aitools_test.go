package schedsend

import (
	"context"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

// The AI / MCP tools run through this package's API in-process as the user.
func TestAIToolsScheduleAndList(t *testing.T) {
	w := newWorld(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	const secret = "ai-test-secret"
	auth := r.Group("/api/v1", func(c *gin.Context) {
		tok, err := jwt.Parse(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "), func(*jwt.Token) (any, error) { return []byte(secret), nil })
		if err != nil {
			c.AbortWithStatus(401)
			return
		}
		cl := tok.Claims.(jwt.MapClaims)
		c.Set("org_id", cl["org_id"])
		c.Set("user_id", cl["user_id"])
		c.Set("user_role", cl["role"])
		c.Set("user_email", cl["email"])
	})
	(&Handler{S: w.s}).RegisterRoutes(auth, r.Group("/api/v1/public"))
	prevEngine, prevSecret := aitools.Engine, aitools.JWTSecret
	aitools.Engine, aitools.JWTSecret = r, secret
	t.Cleanup(func() { aitools.Engine, aitools.JWTSecret = prevEngine, prevSecret })

	cu := w.mkCustomer(w.org, "Wanjiru", "wanjiru@x.test", "", "")
	w.mkInvoice(w.org, cu, "sent", 13920, 13920, "2026-12-02")
	ctx := &aitools.Ctx{Context: context.Background(), Who: aitools.Identity{UserID: w.owner.User, OrgID: w.org, Role: "owner", Email: w.owner.Email, TZ: "Africa/Nairobi"}}

	sched, ok := aitools.Get("schedule_message")
	if !ok || sched.ReadOnly || sched.Destructive {
		t.Fatal("schedule_message is a write tool (the user confirms before it runs)")
	}
	list, ok := aitools.Get("scheduled_list")
	if !ok || !list.ReadOnly {
		t.Fatal("scheduled_list is read-only")
	}
	// Preview saves nothing.
	pv, err := sched.Handler(ctx, map[string]any{"what": "customer", "customer_ids": []any{cu}, "send_at": tomorrow8, "subject": "Hi", "message": "Hello {first_name}", "preview": true})
	if err != nil {
		t.Fatal(err)
	}
	if p := pv.(map[string]any); p["saved"] != false || p["recipients"] != float64(1) {
		t.Fatalf("preview: %+v", p)
	}
	var n int64
	w.db.Model(&Message{}).Count(&n)
	if n != 0 {
		t.Fatal("preview must not save")
	}
	// "Send invoice INV-1001 tomorrow at 8am".
	res, err := sched.Handler(ctx, map[string]any{"what": "invoice", "invoice_number": "inv-1001", "send_at": tomorrow8})
	if err != nil {
		t.Fatal(err)
	}
	out := res.(map[string]any)
	if out["next_local"] != tomorrow8 || out["timezone"] != "Africa/Nairobi" || !strings.Contains(out["url"].(string), "/communication/scheduled?open=") {
		t.Fatalf("scheduled: %+v", out)
	}
	// Errors come back in plain words.
	if _, err := sched.Handler(ctx, map[string]any{"what": "invoice", "invoice_number": "INV-9999", "send_at": tomorrow8}); err == nil || !strings.Contains(err.Error(), "INV-9999") {
		t.Fatalf("unknown number: %v", err)
	}
	staff := &aitools.Ctx{Context: context.Background(), Who: aitools.Identity{UserID: w.staff.User, OrgID: w.org, Role: "staff"}}
	if _, err := sched.Handler(staff, map[string]any{"what": "team", "send_at": tomorrow8, "subject": "x", "message": "y"}); err == nil || !strings.Contains(err.Error(), "managers") {
		t.Fatalf("permissions apply: %v", err)
	}
	lr, err := list.Handler(ctx, map[string]any{"tab": "upcoming"})
	if err != nil {
		t.Fatal(err)
	}
	items := lr.(map[string]any)["items"].([]map[string]any)
	if len(items) != 1 || items[0]["kind"] != KindInvoice || items[0]["doc_number"] != "INV-1001" {
		t.Fatalf("list: %+v", items)
	}
}
