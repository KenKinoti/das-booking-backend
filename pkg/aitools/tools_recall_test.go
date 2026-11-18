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

// invoices_recall_email: destructive (confirm first); a held email is
// undone, a sent one recalled with the notice and the user is told what
// can't be taken back. invoices_send asks for the undo hold and reports it.
func TestRecallEmailTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	held := true
	var posts []string
	var bodies []map[string]any
	r.GET("/api/v1/invoicing/invoices/:id", func(c *gin.Context) {
		c.JSON(200, gin.H{"success": true, "data": gin.H{"id": "inv1", "number": "INV-1043", "doc_type": "invoice", "status": "sent",
			"email_status": gin.H{"state": "sent", "held": 0, "recalled": 1, "summary_log_id": "log1", "recipients": []gin.H{
				{"email": "a@x.test", "role": "to", "state": "opened", "recalled": true, "recall": gin.H{"text": "Recalled 18 Nov 10:42 by Ken — links off", "links_off": true}}}}}})
	})
	r.GET("/api/v1/invoicing/invoices/:id/email-log", func(c *gin.Context) {
		log := []gin.H{{"id": "log1", "status": "sent", "mode": "email", "kind": "invoice", "to": "a@x.test"}}
		if held {
			log = append([]gin.H{{"id": "log2", "status": "held", "mode": "email", "kind": "invoice", "to": "a@x.test"}}, log...)
		}
		c.JSON(200, gin.H{"success": true, "data": gin.H{"log": log}})
	})
	r.POST("/api/v1/invoicing/invoices/:id/emails/:log/:action", func(c *gin.Context) {
		posts = append(posts, c.Param("log")+"/"+c.Param("action"))
		raw, _ := io.ReadAll(c.Request.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		if c.Param("action") == "undo" {
			c.JSON(200, gin.H{"success": true, "data": gin.H{"undone": true}})
			return
		}
		c.JSON(200, gin.H{"success": true, "data": gin.H{"all_links": false, "recalled": []gin.H{{"email": "a@x.test"}},
			"notice": gin.H{"sent": true, "to": "a@x.test"}, "attachment_note": "The email had INV-1043.pdf attached — that copy stays in their inbox and can't be taken back."}})
	})
	r.POST("/api/v1/invoicing/invoices/:id/send", func(c *gin.Context) {
		raw, _ := io.ReadAll(c.Request.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		c.JSON(200, gin.H{"success": true, "data": gin.H{"invoice": gin.H{"id": "inv1", "number": "INV-1043", "status": "draft"}, "emailed": false,
			"held": true, "hold": gin.H{"log_id": "log3", "seconds": 30, "send_at": "2026-11-19T10:42:30Z"}, "to": "a@x.test"}})
	})
	Engine, JWTSecret = r, "s3cret"
	t.Cleanup(func() { Engine, JWTSecret = nil, "" })
	c := &Ctx{Context: context.Background(), Who: Identity{UserID: "u1", OrgID: "o1", Role: "admin"}}

	tool, ok := Get("invoices_recall_email")
	if !ok || !tool.Destructive || tool.ReadOnly || tool.Module != "invoicing" {
		t.Fatalf("tool %+v", tool)
	}
	out, err := tool.Handler(c, map[string]any{"invoice_id": "inv1"})
	if err != nil {
		t.Fatal(err)
	}
	if m := out.(map[string]any); m["undone"] != true || posts[0] != "log2/undo" {
		t.Fatalf("a held email should be undone: %v %v", m, posts)
	}
	held = false
	out, err = tool.Handler(c, map[string]any{"invoice_id": "inv1", "reason": "wrong amount", "recipients": []any{"a@x.test"}})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if posts[1] != "log1/recall" || bodies[1]["notice"] != true || bodies[1]["reason"] != "wrong amount" || len(bodies[1]["recipients"].([]any)) != 1 {
		t.Fatalf("recall request: %v %v", posts, bodies[1])
	}
	if msg := m["message"].(string); !strings.Contains(msg, "can't be taken back") || !strings.Contains(msg, "can't be deleted from their inbox") || !strings.Contains(msg, "notice was emailed") {
		t.Fatalf("message: %s", msg)
	}

	// invoices_send holds like the Send button and says so.
	send, _ := Get("invoices_send")
	out, err = send.Handler(c, map[string]any{"invoice_id": "inv1"})
	if err != nil {
		t.Fatal(err)
	}
	s := out.(map[string]any)
	if bodies[len(bodies)-1]["hold"] != true || s["held"] != true || s["emailed"] != false || s["email_log_id"] != "log3" || !strings.Contains(s["note"].(string), "undo") {
		t.Fatalf("send: %v", s)
	}
	// invoices_get shows the recall.
	get, _ := Get("invoices_get")
	out, _ = get.Handler(c, map[string]any{"invoice_id": "inv1"})
	es := out.(map[string]any)["email_status"].(map[string]any)
	rs := es["recipients"].([]map[string]any)
	if es["recalled"] != float64(1) || es["summary_log_id"] != "log1" || rs[0]["recalled"] != true || rs[0]["recall"].(map[string]any)["links_off"] != true {
		t.Fatalf("email_status: %v", es)
	}
	if _, has := es["held"]; has {
		t.Fatalf("held=0 should be left out: %v", es)
	}
	_ = http.StatusOK
}
