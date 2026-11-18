package invoicing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// ---------------------------------------------------------------- harness

// recallEnv is a mail environment with a UUID organisation (so audit rows
// fit), a counting transport and the public routes.
type recallEnv struct {
	mailEnv
	mu    *sync.Mutex
	count *int64
}

func setupRecallEnv(t *testing.T) recallEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	t.Setenv("SMTP_HOST", "")
	t.Setenv("PUBLIC_API_URL", "https://api.example.com")
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := db.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.IntegrationSettings{}, &mail.SendStatus{}, &fxrates.ManualRate{}, &models.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	org := uuid.NewString()
	t.Cleanup(func() { db.Unscoped().Where("organization_id = ?", org).Delete(&models.IntegrationSettings{}) })
	enc, _ := mail.EncryptSecret("pw")
	row := models.IntegrationSettings{ID: uuid.NewString(), OrganizationID: org, EmailSettings: models.EmailSettings{
		Enabled: true, SMTPHost: "smtp.x.test", SMTPPort: 587, SMTPSecurity: "tls", SMTPUsername: "u@x.test", SMTPPassword: enc, FromEmail: "shop@x.test"}}
	if err := db.Omit("Organization").Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	svc := mail.NewService(db)
	sent := &[]mail.Message{}
	mu := &sync.Mutex{}
	var n int64
	svc.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error {
		mu.Lock()
		*sent = append(*sent, m)
		mu.Unlock()
		atomic.AddInt64(&n, 1)
		return nil
	}
	FX = &fxrates.Service{DB: db, Live: staticLive{"AUD": 1.5, "KES": 130}, LiveAsOf: func() *time.Time { t := time.Now(); return &t }}
	t.Cleanup(func() { FX = nil })
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(db)
	h.Mail = svc
	h.PublicURL = "https://app.example.com"
	h.RegisterRoutes(r.Group("/api", func(c *gin.Context) {
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_id", "u1")
		c.Next()
	}))
	h.RegisterPublicRoutes(r.Group("/public"))
	call(t, r, org, "PUT", "/api/settings", map[string]interface{}{"business_name": "Mvua Traders", "email": "hello@mvua.test", "tax_number": "123",
		"tax_number_label": "PIN", "invoice_prefix": "INV-", "next_invoice_number": 1001, "quote_prefix": "QUO-", "next_quote_number": 1001,
		"number_padding": 4, "default_due_days": 14, "quote_valid_days": 30, "tax_label": "VAT"})
	t.Cleanup(func() { HoldOverride = nil })
	return recallEnv{mailEnv{r: r, org: org, sent: sent, db: db, h: h}, mu, &n}
}

func (e recallEnv) sentCount() int { return int(atomic.LoadInt64(e.count)) }

func (e recallEnv) messages() []mail.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]mail.Message{}, *e.sent...)
}

func (e recallEnv) holdFor(d time.Duration) {
	HoldOverride = func(string) time.Duration { return d }
}

// draft creates a draft invoice (or quote) for To + CCs.
func (e recallEnv) draft(t *testing.T, docType, to string, cc []string) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{"client_name": "Wairimu Grocers", "client_email": to, "cc_emails": cc, "currency": "KES", "doc_type": docType,
		"items": []map[string]interface{}{{"description": "Delivery", "quantity": 1, "unit_price": 4321, "tax_rate": 0}}}
	code, res := call(t, e.r, e.org, "POST", "/api/invoices", body)
	if code != 201 {
		t.Fatalf("create: %d %v", code, res)
	}
	return data(res)
}

func (e recallEnv) send(t *testing.T, id string, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	b := map[string]interface{}{"mode": "email", "subject": "Invoice {{number}}", "message": "Hi,\n\nView it online: {{link}}\n\nThanks"}
	for k, v := range body {
		b[k] = v
	}
	code, res := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/send", b)
	if code != 200 {
		t.Fatalf("send: %d %v", code, res)
	}
	return data(res)
}

func (e recallEnv) logs(t *testing.T, id string) []map[string]interface{} {
	t.Helper()
	_, res := call(t, e.r, e.org, "GET", "/api/invoices/"+id+"/email-log", nil)
	out := []map[string]interface{}{}
	for _, x := range data(res)["log"].([]interface{}) {
		out = append(out, x.(map[string]interface{}))
	}
	return out
}

func (e recallEnv) logByID(t *testing.T, id, logID string) map[string]interface{} {
	for _, l := range e.logs(t, id) {
		if l["id"] == logID {
			return l
		}
	}
	t.Fatalf("log %s not found", logID)
	return nil
}

func (e recallEnv) status(t *testing.T, id string) string {
	_, res := call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	return data(res)["status"].(string)
}

func (e recallEnv) get(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36")
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func (e recallEnv) post(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func publicToken(t *testing.T, e recallEnv, id string) string {
	var inv Invoice
	if err := e.db.Select("public_token").Where("id = ?", id).Take(&inv).Error; err != nil {
		t.Fatal(err)
	}
	return inv.PublicToken
}

// tokensByPerson maps each To/CC address to the token in their copy.
func tokensByPerson(t *testing.T, msgs []mail.Message) map[string]string {
	out := map[string]string{}
	for _, m := range msgs {
		if len(m.To) == 1 {
			if l := rTokRe.FindStringSubmatch(m.HTML); l != nil {
				out[m.To[0]] = l[1]
			}
		}
	}
	return out
}

func (e recallEnv) auditCount(action string) int64 {
	var n int64
	e.db.Table("audit_logs").Where("organization_id = ? AND action = ?", e.org, action).Count(&n)
	return n
}

// ---------------------------------------------------------------- undo send

// A held email: checked, then nothing goes and the document stays a draft
// until the timer sends it — exactly once, even when the engine, the sweep
// and a "Send now" race for it.
func TestHoldSendsExactlyOnce(t *testing.T) {
	e := setupRecallEnv(t)
	e.holdFor(400 * time.Millisecond)
	eng := StartHolds(e.h, 50*time.Millisecond)
	defer eng.Stop()

	inv := e.draft(t, TypeInvoice, "amina@client.test", []string{"brian@client.test"})
	id := inv["id"].(string)
	res := e.send(t, id, map[string]interface{}{"hold": true})
	if res["held"] != true || res["emailed"] != false {
		t.Fatalf("expected a held send: %v", res)
	}
	logID := res["log_id"].(string)
	if e.sentCount() != 0 || e.status(t, id) != StatusDraft {
		t.Fatalf("held: %d messages, status %s", e.sentCount(), e.status(t, id))
	}
	l := e.logByID(t, id, logID)
	if l["status"] != "held" || l["hold"].(map[string]interface{})["state"] != HoldHeld {
		t.Fatalf("log while held: %v", l)
	}
	_, gres := call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	st := data(gres)["email_status"].(map[string]interface{})
	if st["held"].(float64) != 1 || st["held_log_id"] != logID || st["state"] != "none" || st["held_until"] == nil {
		t.Fatalf("email status while held: %v", st)
	}
	if !waitFor(t, 3*time.Second, func() bool { return e.sentCount() == 2 }) {
		t.Fatalf("not sent after the hold: %d", e.sentCount())
	}
	time.Sleep(300 * time.Millisecond) // sweeps run; nothing more goes
	if e.sentCount() != 2 || e.status(t, id) != StatusSent {
		t.Fatalf("after hold: %d messages, status %s", e.sentCount(), e.status(t, id))
	}
	l = e.logByID(t, id, logID)
	if l["status"] != "sent" || len(l["recipients"].([]interface{})) != 2 || l["hold"].(map[string]interface{})["state"] != HoldSent {
		t.Fatalf("log after hold: %v", l)
	}
	if _, err := e.h.releaseHold(logID, true, "u1"); err != errNotHeld {
		t.Fatalf("a sent hold must not go again: %v", err)
	}

	// Race: many releasers at the due time — one sends.
	eng.Stop()
	before := e.sentCount()
	res = e.send(t, id, map[string]interface{}{"hold": true, "cc": []string{}})
	logID = res["log_id"].(string)
	time.Sleep(450 * time.Millisecond)
	var wg sync.WaitGroup
	var wins int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := e.h.releaseHold(logID, i%2 == 0, ""); err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || e.sentCount() != before+1 {
		t.Fatalf("race: %d winners, %d new messages", wins, e.sentCount()-before)
	}
}

// A restart: held emails are found again and sent on time (not early, not
// twice); a send interrupted half-way is reported, never repeated.
func TestHoldRestartRecovery(t *testing.T) {
	e := setupRecallEnv(t)
	e.holdFor(600 * time.Millisecond)
	// No engine: the server "stopped" right after holding.
	a := e.draft(t, TypeInvoice, "akinyi@client.test", nil)
	b := e.draft(t, TypeInvoice, "baraka@client.test", nil)
	resA := e.send(t, a["id"].(string), map[string]interface{}{"hold": true})
	time.Sleep(700 * time.Millisecond) // a is overdue at restart
	e.holdFor(1500 * time.Millisecond)
	resB := e.send(t, b["id"].(string), map[string]interface{}{"hold": true})
	if e.sentCount() != 0 {
		t.Fatalf("nothing should go without an engine yet: %d", e.sentCount())
	}
	// A half-done send from before the restart.
	stale := time.Now().Add(-10 * time.Minute)
	c := e.draft(t, TypeInvoice, "chebet@client.test", nil)
	resC := e.send(t, c["id"].(string), map[string]interface{}{"hold": true})
	e.db.Model(&EmailHold{}).Where("id = ?", resC["log_id"]).Updates(map[string]interface{}{"state": HoldSending, "claimed_at": stale})

	eng := StartHolds(e.h, time.Hour) // restart: recovery only, no sweep
	defer eng.Stop()
	if !waitFor(t, time.Second, func() bool { return e.sentCount() == 1 }) {
		t.Fatalf("the overdue email should go at once: %d", e.sentCount())
	}
	if m := e.messages(); m[0].To[0] != "akinyi@client.test" {
		t.Fatalf("wrong email first: %v", m[0].To)
	}
	time.Sleep(300 * time.Millisecond)
	if e.sentCount() != 1 {
		t.Fatalf("b went early")
	}
	if !waitFor(t, 3*time.Second, func() bool { return e.sentCount() == 2 }) {
		t.Fatalf("b was not sent after the restart")
	}
	time.Sleep(200 * time.Millisecond)
	if e.sentCount() != 2 {
		t.Fatalf("sent twice: %d", e.sentCount())
	}
	_ = resA
	_ = resB
	var hc EmailHold
	e.db.Where("id = ?", resC["log_id"]).Take(&hc)
	lc := e.logByID(t, c["id"].(string), resC["log_id"].(string))
	if hc.State != HoldInterrupted || lc["status"] != "failed" || lc["error_code"] != "INTERRUPTED" || e.status(t, c["id"].(string)) != StatusDraft {
		t.Fatalf("interrupted send: hold %s, log %v", hc.State, lc)
	}
	// A second restart changes nothing.
	eng2 := StartHolds(e.h, time.Hour)
	defer eng2.Stop()
	time.Sleep(200 * time.Millisecond)
	if e.sentCount() != 2 {
		t.Fatalf("a restart re-sent: %d", e.sentCount())
	}
}

// Undo within the window: nothing is sent, the status is untouched, it is
// recorded (activity + audit); too late afterwards. "Send now" skips the
// wait. Receipts from the payment dialog are held too.
func TestUndoAndSendNow(t *testing.T) {
	e := setupRecallEnv(t)
	e.holdFor(800 * time.Millisecond)
	eng := StartHolds(e.h, 50*time.Millisecond)
	defer eng.Stop()

	inv := e.draft(t, TypeInvoice, "otieno@client.test", nil)
	id := inv["id"].(string)
	res := e.send(t, id, map[string]interface{}{"hold": true})
	logID := res["log_id"].(string)
	code, ur := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+logID+"/undo", nil)
	if code != 200 || data(ur)["undone"] != true {
		t.Fatalf("undo: %d %v", code, ur)
	}
	time.Sleep(1200 * time.Millisecond)
	if e.sentCount() != 0 || e.status(t, id) != StatusDraft {
		t.Fatalf("undone email went: %d messages, status %s", e.sentCount(), e.status(t, id))
	}
	if l := e.logByID(t, id, logID); l["status"] != "cancelled" || !strings.Contains(l["response"].(string), "nothing was sent") {
		t.Fatalf("undone log: %v", l)
	}
	code, ur = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+logID+"/undo", nil)
	if code != 409 || !strings.Contains(ur["error"].(map[string]interface{})["message"].(string), "already undone") {
		t.Fatalf("second undo: %d %v", code, ur)
	}
	if e.auditCount("invoice_email.undo") != 1 {
		t.Fatalf("undo not audited")
	}
	_, full := call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	found := false
	for _, a := range data(full)["activities"].([]interface{}) {
		if a.(map[string]interface{})["type"] == "email_undone" {
			found = true
		}
	}
	if !found {
		t.Fatalf("undo not in the activity history")
	}

	// Send now.
	e.holdFor(time.Minute)
	res = e.send(t, id, map[string]interface{}{"hold": true})
	logID = res["log_id"].(string)
	code, sn := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+logID+"/send-now", nil)
	if code != 200 || data(sn)["emailed"] != true || e.sentCount() != 1 || e.status(t, id) != StatusSent {
		t.Fatalf("send now: %d %v (%d sent)", code, sn, e.sentCount())
	}
	code, ur = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+logID+"/undo", nil)
	if code != 409 || !strings.Contains(ur["error"].(map[string]interface{})["message"].(string), "Too late") {
		t.Fatalf("undo after sending: %d %v", code, ur)
	}
	if e.auditCount("invoice_email.send_now") != 1 {
		t.Fatalf("send now not audited")
	}

	// Off: no hold at all.
	call(t, e.r, e.org, "PUT", "/api/email-options", map[string]interface{}{"undo_seconds": 0})
	HoldOverride = nil
	res = e.send(t, id, map[string]interface{}{"hold": true})
	if res["held"] == true || res["emailed"] != true {
		t.Fatalf("undo off still held: %v", res)
	}

	// The payment dialog's receipt is held as well.
	call(t, e.r, e.org, "PUT", "/api/email-options", map[string]interface{}{"undo_seconds": 10})
	before := e.sentCount()
	code, pr := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 100, "method": "cash", "email_receipt": true, "hold": true})
	rec := data(pr)["receipt_email"].(map[string]interface{})
	if code != 200 || rec["held"] != true || rec["seconds"].(float64) != 10 || e.sentCount() != before {
		t.Fatalf("held receipt: %d %v", code, rec)
	}
	code, _ = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+rec["log_id"].(string)+"/undo", nil)
	if code != 200 {
		t.Fatalf("undo receipt: %d", code)
	}
}

// ---------------------------------------------------------------- recall

// Recall one person: their links show the withdrawn page (no amounts), the
// other person's still work; open data is kept; the timeline tells it.
func TestRecallOneRecipient(t *testing.T) {
	e := setupRecallEnv(t)
	inv := e.draft(t, TypeInvoice, "amina@client.test", []string{"brian@client.test"})
	id := inv["id"].(string)
	res := e.send(t, id, nil)
	logID := res["log_id"].(string)
	toks := tokensByPerson(t, e.messages())
	pub := publicToken(t, e, id)
	if len(toks) != 2 {
		t.Fatalf("tokens: %v", toks)
	}
	// Brian opens his copy before the recall.
	e.get("/public/email-open/" + toks["brian@client.test"] + ".gif")
	time.Sleep(10 * time.Millisecond)

	code, rr := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+logID+"/recall", map[string]interface{}{"recipients": []string{"brian@client.test"}})
	if code != 200 {
		t.Fatalf("recall: %d %v", code, rr)
	}
	d := data(rr)
	if d["all_links"] != false || len(d["recalled"].([]interface{})) != 1 || !strings.Contains(d["attachment_note"].(string), ".pdf") {
		t.Fatalf("recall result: %v", d)
	}

	w := e.get("/public/invoices/" + pub + "?r=" + toks["brian@client.test"])
	body := w.Body.String()
	if w.Code != http.StatusGone || !strings.Contains(body, "withdrawn by Mvua Traders on") || !strings.Contains(body, "Please contact them for the current version") {
		t.Fatalf("withdrawn page: %d %s", w.Code, body)
	}
	if strings.Contains(body, "4,321") || strings.Contains(body, "4321") || strings.Contains(body, "Delivery") {
		t.Fatalf("the withdrawn page must not show amounts or lines: %s", body)
	}
	if w := e.get("/public/invoices/" + pub + "?r=" + toks["amina@client.test"]); w.Code != 200 {
		t.Fatalf("Amina's link must still work: %d", w.Code)
	}
	if Withdrawn(e.db, pub, toks["brian@client.test"]) == nil || Withdrawn(e.db, pub, toks["amina@client.test"]) != nil || Withdrawn(e.db, pub, "") != nil {
		t.Fatalf("Withdrawn() wrong")
	}
	// Payment through the recalled link is refused (pkg/payments asks this).
	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	gc.Request = httptest.NewRequest("GET", "/", nil)
	if !RefuseWithdrawn(gc, e.db, pub, toks["brian@client.test"]) || gc.Writer.Status() != http.StatusGone {
		t.Fatalf("pay through a recalled link must be refused")
	}
	if w := e.get("/public/invoices/" + pub + "/pdf?r=" + toks["brian@client.test"]); w.Code != http.StatusGone {
		t.Fatalf("PDF through a recalled link: %d", w.Code)
	}

	// Open data kept, and still counted after the recall (another app, so
	// it isn't the same open fetched twice).
	req := httptest.NewRequest("GET", "/public/email-open/"+toks["brian@client.test"]+".gif", nil)
	req.Header.Set("User-Agent", gmailUA)
	e.r.ServeHTTP(httptest.NewRecorder(), req)
	l := e.logByID(t, id, logID)
	var brian map[string]interface{}
	for _, x := range l["recipients"].([]interface{}) {
		if r := x.(map[string]interface{}); r["email"] == "brian@client.test" {
			brian = r
		}
	}
	rc := brian["recall"].(map[string]interface{})
	if brian["recalled"] != true || rc["opened_before"] != true || brian["opens"].(float64) < 2 ||
		!strings.Contains(brian["timeline"].(string), "Recalled") || !strings.Contains(rc["text"].(string), "opened before recall: yes") ||
		!strings.Contains(rc["text"].(string), "can't be taken back") {
		t.Fatalf("Brian's status: %v", brian)
	}
	_, gres := call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	st := data(gres)["email_status"].(map[string]interface{})
	if st["recalled"].(float64) != 1 || !strings.Contains(st["summary"].(string), "recalled for 1 of 2") || st["summary_log_id"] != logID {
		t.Fatalf("summary: %v", st)
	}
	if e.auditCount("invoice_email.recall") != 1 {
		t.Fatalf("recall not audited")
	}
	// Recalling him again is refused; Amina still can be.
	code, rr = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+logID+"/recall", map[string]interface{}{"recipients": []string{"brian@client.test"}})
	if code != 409 {
		t.Fatalf("second recall: %d %v", code, rr)
	}
}

// Recall everyone, every link off, with a disregard notice: the old link
// shows the withdrawn page, the notice goes to each person (tracked), a
// later send issues new links.
func TestRecallAllLinksWithNotice(t *testing.T) {
	e := setupRecallEnv(t)
	inv := e.draft(t, TypeInvoice, "amina@client.test", []string{"brian@client.test"})
	id, number := inv["id"].(string), inv["number"].(string)
	res := e.send(t, id, nil)
	logID := res["log_id"].(string)
	old := publicToken(t, e, id)
	*e.sent = nil
	atomic.StoreInt64(e.count, 0)

	code, rr := call(t, e.r, e.org, "GET", "/api/invoices/"+id+"/emails/"+logID+"/recall", nil)
	pv := data(rr)
	if code != 200 || pv["tracked"] != true || len(pv["recipients"].([]interface{})) != 2 || !strings.Contains(pv["notice_subject"].(string), "Please disregard invoice "+number) {
		t.Fatalf("preview: %d %v", code, rr)
	}
	code, rr = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+logID+"/recall", map[string]interface{}{
		"all_links": true, "notice": true, "reason": "The delivery date was wrong"})
	d := data(rr)
	if code != 200 || d["all_links"] != true || d["notice"].(map[string]interface{})["sent"] != true {
		t.Fatalf("recall: %d %v", code, rr)
	}
	msgs := e.messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 notice copies, got %d", len(msgs))
	}
	for _, m := range msgs {
		if !strings.HasPrefix(m.Subject, "Please disregard invoice "+number+" sent on ") || !strings.Contains(m.Text, "The delivery date was wrong") ||
			!strings.Contains(m.Text, "Please also delete the PDF") || len(m.Attachments) != 0 || !pixelRe.MatchString(m.HTML) {
			t.Fatalf("notice: %s\n%s", m.Subject, m.Text)
		}
		if strings.Contains(m.Text, "4,321") {
			t.Fatalf("the notice must not repeat amounts")
		}
	}
	nowTok := publicToken(t, e, id)
	if nowTok == old {
		t.Fatalf("the public link was not replaced")
	}
	if w := e.get("/public/invoices/" + old); w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "WITHDRAWN") {
		t.Fatalf("old link: %d %s", w.Code, w.Body.String())
	}
	if w := e.get("/public/invoices/" + nowTok); w.Code != 200 {
		t.Fatalf("new link: %d", w.Code)
	}
	if RetiredInvoiceID(e.db, old) != id {
		t.Fatalf("retired token not found")
	}
	// The notice is tracked per person; its open shows on the recall.
	ptok := pixelRe.FindStringSubmatch(msgs[0].HTML)[1]
	e.get("/public/email-open/" + ptok + ".gif")
	l := e.logByID(t, id, logID)
	for _, x := range l["recipients"].([]interface{}) {
		r := x.(map[string]interface{})
		rc := r["recall"].(map[string]interface{})
		if rc["notice"] != "sent" || rc["all_links"] != true {
			t.Fatalf("recall status %v", rc)
		}
		if r["email"] == msgs[0].To[0] && !strings.Contains(rc["text"].(string), "disregard notice sent, opened") {
			t.Fatalf("notice open not shown: %v", rc["text"])
		}
	}
	var notice map[string]interface{}
	for _, x := range e.logs(t, id) {
		if x["kind"] == KindNotice {
			notice = x
		}
	}
	if notice == nil || notice["tracking"] != TrackPerRecipient || len(notice["recipients"].([]interface{})) != 2 {
		t.Fatalf("notice log: %v", notice)
	}
	_, gres := call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	st := data(gres)["email_status"].(map[string]interface{})
	if st["notices"].(float64) != 1 || st["recalled"].(float64) != 2 || st["last_kind"] != KindInvoice {
		t.Fatalf("status after notice: %v", st)
	}
	if e.auditCount("invoice_email.notice") != 1 {
		t.Fatalf("notice not audited")
	}
	// A later send issues new links that work.
	*e.sent = nil
	e.send(t, id, map[string]interface{}{"cc": []string{}})
	tok := rTokRe.FindStringSubmatch(e.messages()[0].HTML)[1]
	if !strings.Contains(e.messages()[0].HTML, nowTok) {
		t.Fatalf("the new send must use the new link")
	}
	if w := e.get("/public/invoices/" + nowTok + "?r=" + tok); w.Code != 200 {
		t.Fatalf("new email's link: %d", w.Code)
	}
}

// Quotes: accepting through a recalled link is refused. Receipts: links
// off + notice naming the receipt. Recall and correct: edit, or void and a
// corrected copy.
func TestRecallQuotesReceiptsAndCorrect(t *testing.T) {
	e := setupRecallEnv(t)
	q := e.draft(t, TypeQuote, "njeri@client.test", nil)
	qid := q["id"].(string)
	res := e.send(t, qid, map[string]interface{}{"subject": "Quote {{number}}"})
	tok := tokensByPerson(t, e.messages())["njeri@client.test"]
	pub := publicToken(t, e, qid)
	code, rr := call(t, e.r, e.org, "POST", "/api/invoices/"+qid+"/emails/"+res["log_id"].(string)+"/recall", map[string]interface{}{"correct": "edit"})
	if code != 200 || data(rr)["correct"].(map[string]interface{})["action"] != "edit" {
		t.Fatalf("recall quote: %d %v", code, rr)
	}
	if w := e.post("/public/invoices/" + pub + "/accept?r=" + tok); w.Code != http.StatusGone {
		t.Fatalf("accept through a recalled link: %d %s", w.Code, w.Body.String())
	}
	if w := e.get("/public/invoices/" + pub + "?r=" + tok); w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "This quote was withdrawn") {
		t.Fatalf("quote withdrawn page: %d %s", w.Code, w.Body.String())
	}
	if e.status(t, qid) != StatusSent {
		t.Fatalf("the quote itself is not changed by a recall")
	}

	// Receipt.
	inv := e.draft(t, TypeInvoice, "kamau@client.test", nil)
	id := inv["id"].(string)
	e.send(t, id, nil)
	*e.sent = nil
	_, pr := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 1000, "method": "mobile_money", "email_receipt": true})
	rec := data(pr)["receipt_email"].(map[string]interface{})
	if rec["emailed"] != true {
		t.Fatalf("receipt not emailed: %v", rec)
	}
	rtok := tokensByPerson(t, e.messages())["kamau@client.test"]
	*e.sent = nil
	code, rr = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+rec["log_id"].(string)+"/recall", map[string]interface{}{"notice": true, "correct": "replace"})
	if code != 200 || data(rr)["correct"] != nil {
		t.Fatalf("receipt recall (no correction for receipts): %d %v", code, rr)
	}
	m := e.messages()
	if len(m) != 1 || !strings.Contains(m[0].Subject, "Please disregard receipt RCT-") || !strings.Contains(m[0].Subject, "for invoice "+inv["number"].(string)) {
		t.Fatalf("receipt notice: %v", m)
	}
	if w := e.get("/public/invoices/" + publicToken(t, e, id) + "?r=" + rtok); w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "This receipt was withdrawn") {
		t.Fatalf("receipt link: %d %s", w.Code, w.Body.String())
	}

	// Recall and correct: void + corrected copy is refused with payments;
	// without payments the original is voided and a draft copy started.
	logs := e.logs(t, id)
	var invLog string
	for _, l := range logs {
		if l["kind"] == KindInvoice && l["status"] == "sent" {
			invLog = l["id"].(string)
		}
	}
	code, rr = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+invLog+"/recall", map[string]interface{}{"correct": "replace"})
	if code != 409 || errCode(rr) != "HAS_PAYMENTS" {
		t.Fatalf("replace with payments: %d %v", code, rr)
	}
	inv2 := e.draft(t, TypeInvoice, "wanjiku@client.test", nil)
	id2 := inv2["id"].(string)
	res2 := e.send(t, id2, nil)
	code, rr = call(t, e.r, e.org, "POST", "/api/invoices/"+id2+"/emails/"+res2["log_id"].(string)+"/recall", map[string]interface{}{"correct": "replace"})
	corr := data(rr)["correct"].(map[string]interface{})
	if code != 200 || corr["action"] != "replace" || corr["invoice_id"] == id2 || e.status(t, id2) != StatusVoid || e.status(t, corr["invoice_id"].(string)) != StatusDraft {
		t.Fatalf("replace: %d %v", code, rr)
	}
}

// What can't be recalled, and emails without per-person links.
func TestRecallRules(t *testing.T) {
	e := setupRecallEnv(t)
	e.holdFor(time.Minute)
	inv := e.draft(t, TypeInvoice, "amina@client.test", []string{"brian@client.test"})
	id := inv["id"].(string)
	held := e.send(t, id, map[string]interface{}{"hold": true})
	// Sending it again while one is waiting: a warning, not a silent double send.
	_, pf0 := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/preflight", map[string]interface{}{"mode": "email"})
	warned := false
	for _, c := range data(pf0)["checks"].([]interface{}) {
		if c := c.(map[string]interface{}); c["id"] == "duplicate" && c["title"] == "Already waiting to go" {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning while an email is held: %v", data(pf0)["checks"])
	}
	code, rr := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+held["log_id"].(string)+"/recall", map[string]interface{}{})
	if code != 409 || !strings.Contains(rr["error"].(map[string]interface{})["message"].(string), "Undo") {
		t.Fatalf("recall a held email: %d %v", code, rr)
	}
	call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+held["log_id"].(string)+"/undo", nil)
	HoldOverride = nil

	// One message to everyone: only "every link" can switch it off.
	call(t, e.r, e.org, "PUT", "/api/email-tracking", map[string]interface{}{"track_recipients": false})
	res := e.send(t, id, nil)
	pub := publicToken(t, e, id)
	code, rr = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/emails/"+res["log_id"].(string)+"/recall", map[string]interface{}{})
	if code != 200 || data(rr)["all_links"] != true || data(rr)["all_links_forced"] != true {
		t.Fatalf("single-message recall: %d %v", code, rr)
	}
	if w := e.get("/public/invoices/" + pub); w.Code != http.StatusGone {
		t.Fatalf("old link after forced switch-off: %d", w.Code)
	}

	// Options: only the listed windows; the PDF switch makes emails link-only.
	if code, _ := call(t, e.r, e.org, "PUT", "/api/email-options", map[string]interface{}{"undo_seconds": 45}); code != 400 {
		t.Fatalf("45 s accepted: %d", code)
	}
	code, rr = call(t, e.r, e.org, "PUT", "/api/email-options", map[string]interface{}{"attach_pdf": false})
	if code != 200 || data(rr)["attach_pdf"] != false || data(rr)["undo_seconds"].(float64) != float64(DefaultUndoSeconds) {
		t.Fatalf("options: %d %v", code, rr)
	}
	call(t, e.r, e.org, "PUT", "/api/email-tracking", map[string]interface{}{"track_recipients": true})
	_, pf := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/preflight", map[string]interface{}{"mode": "email"})
	foundInfo := false
	for _, c := range data(pf)["checks"].([]interface{}) {
		if c := c.(map[string]interface{}); c["id"] == "pdf" && c["severity"] == "info" {
			foundInfo = true
		}
	}
	if !foundInfo {
		t.Fatalf("no link-only check: %v", data(pf)["checks"])
	}
	*e.sent = nil
	res = e.send(t, id, map[string]interface{}{"cc": []string{}})
	if m := e.messages(); len(m) != 1 || len(m[0].Attachments) != 0 || strings.Contains(m[0].Text, "is attached") {
		t.Fatalf("link-only email carried a PDF")
	}
	code, rr = call(t, e.r, e.org, "GET", "/api/invoices/"+id+"/emails/"+res["log_id"].(string)+"/recall", nil)
	if code != 200 || data(rr)["attachment"] != "" {
		t.Fatalf("link-only preview: %v", rr)
	}
	if e.auditCount("invoice_email.options") < 1 {
		t.Fatalf("options change not audited")
	}
}

// Recall, undo and send-now are guarded exactly like sending: the same
// module and action for every role.
func TestRecallPermissionsMatchSend(t *testing.T) {
	send := "/api/v1/invoicing/invoices/x/send"
	paths := []string{"/api/v1/invoicing/invoices/x/emails/y/recall", "/api/v1/invoicing/invoices/x/emails/y/undo", "/api/v1/invoicing/invoices/x/emails/y/send-now"}
	roles := []*industry.OrgRole{
		{Name: "Viewer", Base: industry.BaseReadOnly},
		{Name: "Cashier", Base: industry.BaseStaff, Perms: industry.Perms{"invoicing": {"view"}}},
		{Name: "Clerk", Base: industry.BaseStaff, Perms: industry.Perms{"invoicing": {"view", "create"}}},
		{Name: "Manager", Base: industry.BaseManager, Perms: industry.Perms{"*": {"view", "create", "edit"}}},
	}
	for _, r := range roles {
		a := industry.Access{Role: r}
		want := industry.Decide(a, http.MethodPost, send, nil)
		for _, p := range paths {
			got := industry.Decide(a, http.MethodPost, p, nil)
			if got.Allowed != want.Allowed || got.Action != want.Action || got.Module != want.Module {
				t.Fatalf("%s on %s: %+v, send: %+v", r.Name, p, got, want)
			}
		}
	}
	if industry.Decide(industry.Access{Role: roles[0]}, http.MethodPost, paths[0], nil).Allowed {
		t.Fatalf("read-only must not recall")
	}
	if !industry.Decide(industry.Access{Role: roles[2]}, http.MethodPost, paths[0], nil).Allowed {
		t.Fatalf("someone who may send may recall")
	}
}

// The timeline text, without a database.
func TestRecallStatusText(t *testing.T) {
	at := time.Date(2026, 11, 18, 7, 42, 0, 0, time.UTC)
	nairobi, _ := time.LoadLocation("Africa/Nairobi")
	opened := at.Add(8 * time.Minute)
	s := recallStatusOf(EmailRecall{RecalledAt: at, RecalledByName: "Ken", LinksOff: true, OpenedBefore: false, HadAttachment: true,
		Attachment: "INV-1043.pdf (12 KB)", NoticeRecipientID: "n1"}, map[string]EmailRecipient{"n1": {ID: "n1", Status: "sent", FirstHumanOpenAt: &opened}}, nairobi)
	want := "Recalled 18 Nov 10:42 by Ken — links off · opened before recall: no · disregard notice sent, opened 10:50 · the attached INV-1043.pdf can't be taken back"
	if s.Text != want {
		t.Fatalf("\n got %q\nwant %q", s.Text, want)
	}
	s = recallStatusOf(EmailRecall{RecalledAt: at, AllLinks: true, LinksOff: true, AutoOpenedBefore: true}, nil, time.UTC)
	if !strings.Contains(s.Text, "every link to the document off") || !strings.Contains(s.Text, "only automatically") || strings.Contains(s.Text, "notice") {
		t.Fatalf("text: %s", s.Text)
	}
	if recallable(&EmailLog{Mode: "test", Status: "sent"}) == "" || recallable(&EmailLog{Mode: "email", Status: "sent", Kind: KindNotice}) == "" ||
		recallable(&EmailLog{Mode: "email", Status: "sent", Kind: KindInvoice}) != "" {
		t.Fatalf("recallable rules")
	}
}
