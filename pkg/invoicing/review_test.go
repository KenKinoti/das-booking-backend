package invoicing

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
)

// reviewEnv is the recall environment with Pending review installed and a
// router whose requests carry a user and role (X-User / X-Role).
type reviewEnv struct {
	recallEnv
	rr  *gin.Engine
	svc *outbox.Service
}

func setupReviewEnv(t *testing.T) reviewEnv {
	t.Helper()
	e := setupRecallEnv(t)
	if err := outbox.Migrate(e.db); err != nil {
		t.Fatal(err)
	}
	notify.Disabled = true
	t.Cleanup(func() { notify.Disabled = false })
	svc := outbox.Install(&outbox.Service{DB: e.db, Mail: e.h.Mail, Now: time.Now})
	t.Cleanup(outbox.Uninstall)
	t.Cleanup(func() {
		e.db.Where("organization_id = ?", e.org).Delete(&outbox.Item{})
		e.db.Where("organization_id = ?", e.org).Delete(&outbox.Event{})
	})
	r := gin.New()
	e.h.RegisterRoutes(r.Group("/api", func(c *gin.Context) {
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("user_role", c.GetHeader("X-Role"))
		c.Next()
	}))
	return reviewEnv{recallEnv: e, rr: r, svc: svc}
}

func (e reviewEnv) as(t *testing.T, user, role, method, path string, body map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	return callAs(t, e.rr, e.org, user, role, method, path, body)
}

func (e reviewEnv) owner() outbox.Actor {
	return outbox.Actor{Org: e.org, User: "owner-1", Role: "owner", Name: "Amina Wekesa"}
}

func (e reviewEnv) sendAs(t *testing.T, user, role, id string, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	b := map[string]interface{}{"mode": "email", "accept_warnings": true}
	for k, v := range body {
		b[k] = v
	}
	code, res := e.as(t, user, role, "POST", "/api/invoices/"+id+"/send", b)
	if code != 200 {
		t.Fatalf("send: %d %v", code, res)
	}
	return data(res)
}

// Staff send → waits (nothing sent, still a draft, log "awaiting_approval");
// approved with an edited subject → the normal send path (tracked copies,
// PDF, marked sent, the same log entry becomes "sent").
func TestReviewInvoiceQueuedThenReleased(t *testing.T) {
	e := setupReviewEnv(t)
	inv := e.draft(t, TypeInvoice, "wairimu@grocers.test", []string{"accounts@grocers.test"})
	id := inv["id"].(string)
	out := e.sendAs(t, "staff-1", "staff", id, map[string]interface{}{"send_now": true, "subject": "Invoice {{number}}", "message": "Hi,\n\nView it online: {{link}}"})
	rv, _ := out["review"].(map[string]interface{})
	if out["queued"] != true || out["emailed"] != false || rv == nil || rv["id"] != out["log_id"] || rv["status"] != "pending" {
		t.Fatalf("queued answer: %v", out)
	}
	if e.sentCount() != 0 || e.status(t, id) != StatusDraft {
		t.Fatalf("nothing sent, still a draft: %d %s", e.sentCount(), e.status(t, id))
	}
	logID := out["log_id"].(string)
	if l := e.logByID(t, id, logID); l["status"] != "awaiting_approval" || l["review_id"] != logID {
		t.Fatalf("log: %v", l)
	}
	st := EmailStatuses(e.db, e.org, []Invoice{{ID: id}})[id]
	if st.State != "awaiting_approval" || st.AwaitingApproval != 1 || st.AwaitingReviewID != logID {
		t.Fatalf("status: %+v", st)
	}
	it := outbox.Get(logID)
	if it == nil || it.Kind != outbox.KindInvoiceEmail || it.To != "wairimu@grocers.test" || len(it.CC) != 1 || !strings.HasPrefix(it.Subject, "Invoice INV-") || strings.Contains(it.Message, "{{") {
		t.Fatalf("item: %+v", it)
	}
	// A second send of the same kind warns that one already waits.
	_, pre := e.as(t, "staff-1", "staff", "POST", "/api/invoices/"+id+"/preflight", map[string]interface{}{"mode": "email"})
	if !strings.Contains(strings.ToLower(asJSON(pre)), "already waiting for approval") {
		t.Fatalf("duplicate warning: %v", pre)
	}
	// Preview through the releaser: branded HTML + the PDF.
	d := e.svc.DetailOf(context.Background(), it, true)
	if !strings.Contains(d.Preview.HTML, "<") || len(d.Preview.Attachments) != 1 || d.Preview.Attachments[0].URL != "/invoicing/invoices/"+id+"/pdf" {
		t.Fatalf("preview: %+v", d.Preview)
	}
	sub := "Your invoice — thank you"
	got, outcome, err := e.svc.Approve(context.Background(), e.owner(), logID, "now", &outbox.Edits{Subject: &sub}, "")
	if err != nil || outcome != outbox.OutSent || got.Status != outbox.StatusSent {
		t.Fatalf("approve: %v %s %+v", err, outcome, got)
	}
	msgs := e.messages()
	if len(msgs) != 2 || msgs[0].Subject != sub || len(msgs[0].Attachments) == 0 {
		t.Fatalf("sent copies: %d %+v", len(msgs), msgs)
	}
	if e.status(t, id) != StatusSent {
		t.Fatal("marked sent once it went")
	}
	if l := e.logByID(t, id, logID); l["status"] != "sent" || len(l["recipients"].([]interface{})) != 2 {
		t.Fatalf("same entry sent with tracking: %v", l)
	}
}

// An approver's "Send now — I've reviewed it" goes at once (audited); an
// approver without the tick, and staff with it, wait.
func TestReviewSendNowAndStaff(t *testing.T) {
	e := setupReviewEnv(t)
	a := e.draft(t, TypeInvoice, "otieno@clinic.test", nil)["id"].(string)
	out := e.sendAs(t, "admin-1", "admin", a, map[string]interface{}{"send_now": true})
	rv, _ := out["review"].(map[string]interface{})
	if out["queued"] != false || out["emailed"] != true || rv == nil || rv["sent_now"] != true || e.sentCount() != 1 {
		t.Fatalf("send now: %v", out)
	}
	if e.auditCount("outbox.send_now") != 1 {
		t.Fatal("send now audited")
	}
	b := e.draft(t, TypeInvoice, "njeri@school.test", nil)["id"].(string)
	if out := e.sendAs(t, "admin-1", "admin", b, nil); out["queued"] != true {
		t.Fatalf("admin without the tick waits: %v", out)
	}
	c := e.draft(t, TypeQuote, "baraka@hardware.test", nil)["id"].(string)
	out = e.sendAs(t, "staff-1", "staff", c, map[string]interface{}{"send_now": true})
	if out["queued"] != true || e.sentCount() != 1 {
		t.Fatalf("staff's tick is ignored: %v", out)
	}
	if it := outbox.Get(out["log_id"].(string)); it.Kind != outbox.KindQuoteEmail || it.Trigger == "" {
		t.Fatalf("quote item: %+v", it)
	}
	// Review off: everything sends as before (no review object).
	e.svc.SaveSettings(e.owner(), func(st *outbox.Settings) error { st.Enabled = false; return nil })
	d := e.draft(t, TypeInvoice, "achieng@bakery.test", nil)["id"].(string)
	out = e.sendAs(t, "staff-1", "staff", d, nil)
	if out["queued"] != false || out["review"] != nil || out["emailed"] != true || e.sentCount() != 2 {
		t.Fatalf("review off: %v", out)
	}
}

// A reminder for an invoice that is then paid is superseded at the engine's
// re-check; a rejected email says so in the log and the timeline.
func TestReviewSupersedeAndReject(t *testing.T) {
	e := setupReviewEnv(t)
	id := e.draft(t, TypeInvoice, "wambui@salon.test", nil)["id"].(string)
	e.sendAs(t, "admin-1", "admin", id, map[string]interface{}{"send_now": true})
	rem := e.sendAs(t, "staff-1", "staff", id, map[string]interface{}{"kind": "reminder"})
	remID := rem["log_id"].(string)
	if it := outbox.Get(remID); it.Kind != outbox.KindReminderEmail || !it.Dated {
		t.Fatalf("reminder item: %+v", it)
	}
	code, res := e.as(t, "admin-1", "admin", "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 4321, "date": time.Now().Format("2006-01-02"), "method": "cash"})
	if code != 200 {
		t.Fatalf("pay: %d %v", code, res)
	}
	if r, err := e.svc.RunOnce(context.Background(), time.Second); err != nil || r.Superseded != 1 {
		t.Fatalf("recheck: %v %+v", err, r)
	}
	it := outbox.Get(remID)
	if it.Status != outbox.StatusSuperseded || it.SupersededReason != "Paid in full — the reminder isn't needed" {
		t.Fatalf("superseded: %+v", it)
	}
	if l := e.logByID(t, id, remID); l["status"] != "superseded" || !strings.Contains(l["response"].(string), "Paid in full") {
		t.Fatalf("log: %v", l)
	}
	if e.sentCount() != 1 {
		t.Fatal("the reminder never went")
	}
	// Reject.
	q := e.draft(t, TypeQuote, "kiprono@farm.test", nil)["id"].(string)
	qid := e.sendAs(t, "staff-1", "staff", q, nil)["log_id"].(string)
	if _, err := e.svc.Reject(context.Background(), e.owner(), qid, "Wrong prices"); err != nil {
		t.Fatal(err)
	}
	if l := e.logByID(t, q, qid); l["status"] != "rejected" || !strings.Contains(l["response"].(string), "Amina Wekesa: Wrong prices") {
		t.Fatalf("rejected log: %v", l)
	}
	var n int64
	e.db.Model(&Activity{}).Where("invoice_id = ? AND type = ?", q, "review_rejected").Count(&n)
	if n != 1 {
		t.Fatal("timeline entry")
	}
	if e.status(t, q) != StatusDraft {
		t.Fatal("still a draft")
	}
}

// A receipt from the payment dialog waits; approved, it goes with the receipt PDF.
func TestReviewReceipt(t *testing.T) {
	e := setupReviewEnv(t)
	id := e.draft(t, TypeInvoice, "mutua@hardware.test", nil)["id"].(string)
	e.sendAs(t, "admin-1", "admin", id, map[string]interface{}{"send_now": true})
	code, res := e.as(t, "staff-1", "staff", "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 1000, "date": time.Now().Format("2006-01-02"),
		"method": "mpesa", "reference": "QX1", "email_receipt": true})
	if code != 200 {
		t.Fatalf("pay: %d %v", code, res)
	}
	re := data(res)["receipt_email"].(map[string]interface{})
	rv, _ := re["review"].(map[string]interface{})
	if re["queued"] != true || rv == nil || e.sentCount() != 1 {
		t.Fatalf("receipt waits: %v", re)
	}
	it := outbox.Get(rv["id"].(string))
	if it.Kind != outbox.KindReceiptEmail || it.Amount == nil || *it.Amount != 1000 {
		t.Fatalf("receipt item: %+v", it)
	}
	if _, outcome, err := e.svc.Approve(context.Background(), e.owner(), it.ID, "now", nil, ""); err != nil || outcome != outbox.OutSent {
		t.Fatalf("approve: %v", err)
	}
	msgs := e.messages()
	last := msgs[len(msgs)-1]
	if len(last.Attachments) != 1 || !strings.HasPrefix(last.Attachments[0].Filename, "RCT-") {
		t.Fatalf("receipt pdf: %d attachment(s)", len(last.Attachments))
	}
	// Automatic receipts (online payments) wait too, as "Automatic: online payment received".
	var pid string
	e.db.Model(&Payment{}).Where("invoice_id = ?", id).Order("created_at DESC").Limit(1).Pluck("id", &pid)
	r := e.h.AutoReceipt(e.org, id, pid)
	if !r.Queued || r.Review == nil {
		t.Fatalf("auto receipt: %+v", r)
	}
	if it := outbox.Get(r.Review.ID); it.Trigger != "Automatic: online payment received" || it.TriggerKind != outbox.TriggerAutomatic {
		t.Fatalf("auto receipt item: %+v", it)
	}
}

func asJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func callAs(t *testing.T, r *gin.Engine, org, user, role, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Org", org)
	req.Header.Set("X-User", user)
	req.Header.Set("X-Role", role)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

var _ = uuid.NewString
