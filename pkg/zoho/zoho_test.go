package zoho

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Integration tests run against Postgres when TEST_DATABASE_URL is set.
func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := invoicing.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Customer{}, &models.CustomerContact{}, &models.Service{}); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

type env struct {
	t    *testing.T
	db   *gorm.DB
	fake *fakeZoho
	h    *Handler
	r    *gin.Engine
	org  string
}

func newEnv(t *testing.T, nInvoices int) *env {
	db := testDB(t)
	f := newFakeZoho(t)
	f.seed(nInvoices)
	t.Setenv("ZOHO_ACCOUNTS_URL", f.srv.URL)
	t.Setenv("ZOHO_API_URL", f.srv.URL)
	t.Setenv("PUBLIC_API_URL", "https://api.example.test")
	org := "zoho-test-" + uuid.NewString()
	h := NewHandler(db)
	h.Engine.Sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	h.Engine.ClientHook = func(c *Client) { c.RequestsPerMinute = 100000 }
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1", func(c *gin.Context) {
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_id", "u1")
		c.Set("user_role", "admin")
		c.Next()
	})
	h.RegisterRoutes(g)
	h.RegisterPublic(r.Group("/api/v1/public"))
	invoicing.RegisterAttachmentRoutes(g.Group("/invoicing"), db)
	return &env{t: t, db: db, fake: f, h: h, r: r, org: org}
}

func (e *env) call(method, path string, body interface{}) (int, map[string]interface{}) {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Org", e.org)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func dataOf(m map[string]interface{}) map[string]interface{} {
	d, _ := m["data"].(map[string]interface{})
	return d
}

// connect runs the OAuth flow against the fake: save app → connect URL →
// callback with a code → tokens stored encrypted.
func (e *env) connect() {
	t := e.t
	code, out := e.call("PUT", "/api/v1/imports/zoho/connection", map[string]string{"dc": "com.au", "client_id": "cid", "client_secret": "csecret"})
	if code != 200 {
		t.Fatalf("save connection: %d %v", code, out)
	}
	if ru := dataOf(out)["redirect_uri"]; ru != "https://api.example.test/api/v1/public/zoho/callback" {
		t.Fatalf("redirect uri = %v", ru)
	}
	code, out = e.call("POST", "/api/v1/imports/zoho/connect", map[string]string{"return_to": "http://localhost:5431/imports/zoho"})
	if code != 200 {
		t.Fatalf("connect: %d %v", code, out)
	}
	u, _ := url.Parse(dataOf(out)["url"].(string))
	q := u.Query()
	for k, want := range map[string]string{"access_type": "offline", "prompt": "consent", "response_type": "code", "client_id": "cid",
		"scope": strings.Join(Scopes, ","), "redirect_uri": "https://api.example.test/api/v1/public/zoho/callback"} {
		if q.Get(k) != want {
			t.Fatalf("auth url %s = %q, want %q", k, q.Get(k), want)
		}
	}
	// Forged state is refused.
	req := httptest.NewRequest("GET", "/api/v1/public/zoho/callback?code=good-code&state=forged", nil)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("forged state: %d", w.Code)
	}
	// Untrusted accounts server is refused.
	st := q.Get("state")
	cb := "/api/v1/public/zoho/callback?code=good-code&location=au&state=" + st + "&accounts-server=" + url.QueryEscape("https://evil.example")
	w = httptest.NewRecorder()
	e.r.ServeHTTP(w, httptest.NewRequest("GET", cb, nil))
	if w.Code != 302 || !strings.Contains(w.Header().Get("Location"), "zoho=error") {
		t.Fatalf("evil accounts server: %d %s", w.Code, w.Header().Get("Location"))
	}
	// A fresh state with the real accounts server works.
	_, out = e.call("POST", "/api/v1/imports/zoho/connect", map[string]string{"return_to": "http://localhost:5431/imports/zoho"})
	u, _ = url.Parse(dataOf(out)["url"].(string))
	cb = "/api/v1/public/zoho/callback?code=good-code&location=au&state=" + u.Query().Get("state") + "&accounts-server=" + url.QueryEscape(e.fake.srv.URL)
	w = httptest.NewRecorder()
	e.r.ServeHTTP(w, httptest.NewRequest("GET", cb, nil))
	loc := w.Header().Get("Location")
	if w.Code != 302 || !strings.HasPrefix(loc, "http://localhost:5431/imports/zoho?") || !strings.Contains(loc, "zoho=connected") {
		t.Fatalf("callback: %d %s", w.Code, loc)
	}
	var conn Connection
	e.db.Where("organization_id = ?", e.org).First(&conn)
	if !mail.IsEncrypted(conn.RefreshToken) || !mail.IsEncrypted(conn.ClientSecret) {
		t.Fatal("secrets must be stored encrypted")
	}
	if conn.ZohoOrgID != "777" || conn.ZohoCurrency != "AUD" {
		t.Fatalf("organisation not picked: %+v", conn)
	}
}

func (e *env) runPreview(opts *Options) map[string]interface{} {
	body := map[string]interface{}{}
	if opts != nil {
		body["options"] = opts
	}
	code, out := e.call("POST", "/api/v1/imports/zoho/preview", body)
	if code != 200 {
		e.t.Fatalf("preview: %d %v", code, out)
	}
	id := dataOf(out)["id"].(string)
	e.h.Engine.Wait(id, 20*time.Second)
	_, out = e.call("GET", "/api/v1/imports/jobs/"+id, nil)
	j := dataOf(out)
	if j["status"] != JobPreview {
		e.t.Fatalf("preview status %v: %v %v", j["status"], j["message"], j["error"])
	}
	return j
}

func (e *env) start(id string, opts *Options) map[string]interface{} {
	body := map[string]interface{}{}
	if opts != nil {
		body["options"] = opts
	}
	code, out := e.call("POST", "/api/v1/imports/jobs/"+id+"/start", body)
	if code != 200 {
		e.t.Fatalf("start: %d %v", code, out)
	}
	e.h.Engine.Wait(id, 60*time.Second)
	_, out = e.call("GET", "/api/v1/imports/jobs/"+id, nil)
	return dataOf(out)
}

func counts(j map[string]interface{}, ent string) map[string]interface{} {
	c, _ := j["counts"].(map[string]interface{})
	m, _ := c[ent].(map[string]interface{})
	if m == nil {
		return map[string]interface{}{}
	}
	return m
}

func (e *env) inv(number string) invoicing.Invoice {
	var inv invoicing.Invoice
	if err := e.db.Preload("Items").Preload("Payments").Preload("Activities").Where("organization_id = ? AND number = ?", e.org, number).First(&inv).Error; err != nil {
		e.t.Fatalf("invoice %s not found: %v", number, err)
	}
	return inv
}

func near(a, b float64) bool { return a-b < 0.0051 && b-a < 0.0051 }

func TestZohoAPIImport(t *testing.T) {
	e := newEnv(t, 230) // 230 invoices → two pages of 200
	e.connect()

	pv := e.runPreview(nil)
	prev := pv["preview"].(map[string]interface{})
	var invPrev map[string]interface{}
	for _, x := range prev["entities"].([]interface{}) {
		m := x.(map[string]interface{})
		if m["entity"] == EntInvoices {
			invPrev = m
		}
		if m["entity"] == EntContacts && m["count"].(float64) != 3 {
			t.Fatalf("vendors must be left out: %v", m["count"])
		}
	}
	if invPrev == nil || invPrev["count"].(float64) != 230 {
		t.Fatalf("invoice count in preview: %v", invPrev)
	}
	warn := strings.Join(toStrings(prev["warnings"]), " | ")
	if !strings.Contains(warn, "not applied to an invoice") || !strings.Contains(warn, "no email address") {
		t.Fatalf("expected warnings, got %s", warn)
	}
	num := prev["numbering"].([]interface{})
	if len(num) == 0 || num[0].(map[string]interface{})["next"] != "EST-000012" && num[len(num)-1].(map[string]interface{})["next"] != "INV-000231" {
		t.Fatalf("numbering preview: %v", num)
	}

	// The first API call gets a 429 → retried.
	e.fake.mu.Lock()
	e.fake.throttleN = 1
	e.fake.mu.Unlock()
	j := e.start(pv["id"].(string), nil)
	if j["status"] != JobCompleted {
		t.Fatalf("job status %v: %v %v", j["status"], j["message"], j["error"])
	}
	if c := counts(j, EntInvoices); c["created"].(float64) != 230 {
		t.Fatalf("invoices created: %v", c)
	}
	if c := counts(j, EntContacts); c["created"].(float64) != 3 {
		t.Fatalf("contacts: %v", c)
	}
	if c := counts(j, EntPayments); c["created"].(float64) != 3 {
		t.Fatalf("payments: %v", c)
	}

	// Customers and contact people.
	var acme models.Customer
	e.db.Where("organization_id = ? AND external_id = ?", e.org, "c1").First(&acme)
	if acme.FirstName != "Acme Pty Ltd" || acme.Email != "accounts@acme.test" || acme.Source != "zoho" || acme.TaxNumber != "51 824 753 556" || acme.Address.Suburb != "Sydney" || acme.CountryCode != "AU" {
		t.Fatalf("acme: %+v", acme)
	}
	if acme.PaymentTermsDays == nil || *acme.PaymentTermsDays != 14 {
		t.Fatal("payment terms not imported")
	}
	var cts []models.CustomerContact
	e.db.Where("customer_id = ?", acme.ID).Order("email").Find(&cts)
	if len(cts) != 2 || !cts[0].IsPrimaryBilling || cts[0].CCInvoices || !cts[1].CCInvoices || cts[1].Email != "bob@acme.test" {
		t.Fatalf("contacts: %+v", cts)
	}
	var sam models.Customer
	e.db.Where("organization_id = ? AND external_id = ?", e.org, "c3").First(&sam)
	if sam.FirstName != "Sam" || sam.LastName != "Smith" || sam.IsActive {
		t.Fatalf("individual/inactive: %+v", sam)
	}

	// Paid in two parts.
	i1 := e.inv("INV-000001")
	if i1.Status != invoicing.StatusPaid || len(i1.Payments) != 2 || !near(i1.AmountPaid, 550) || !near(i1.Total, 550) || i1.BalanceDue != 0 || i1.ImportFlag != "" {
		t.Fatalf("i1: status %s paid %v total %v bal %v flag %q pays %d", i1.Status, i1.AmountPaid, i1.Total, i1.BalanceDue, i1.ImportFlag, len(i1.Payments))
	}
	methods := i1.Payments[0].Method + "," + i1.Payments[1].Method
	if !strings.Contains(methods, "bank_transfer") || !strings.Contains(methods, "card") {
		t.Fatalf("methods: %s", methods)
	}
	if i1.PaidAt == nil || i1.PaidAt.Format("2006-01-02") != "2025-02-20" || i1.Reference != "PO-77" || i1.ExternalURL == "" {
		t.Fatalf("i1 details: %+v", i1)
	}
	if i1.ConvertedFromID == nil {
		t.Fatal("invoice made from a quote must link to it")
	}
	var quote invoicing.Invoice
	e.db.Where("id = ?", *i1.ConvertedFromID).First(&quote)
	if quote.Status != invoicing.StatusConverted || quote.Number != "EST-000010" || quote.DocType != invoicing.TypeQuote {
		t.Fatalf("quote: %s %s", quote.Number, quote.Status)
	}
	hist := ""
	for _, a := range i1.Activities {
		hist += a.Type + ":" + a.Message + "\n"
	}
	if !strings.Contains(hist, "imported_email:Imported from Zoho: Invoice emailed to accounts@acme.test (on 1 Feb 2025)") || !strings.Contains(hist, "imported_payment") {
		t.Fatalf("history: %s", hist)
	}
	// Original PDF.
	var att invoicing.Attachment
	if err := e.db.Where("invoice_id = ? AND kind = 'original'", i1.ID).First(&att).Error; err != nil || !bytes.HasPrefix(att.Data, []byte("%PDF")) {
		t.Fatalf("pdf: %v", err)
	}
	code, out := e.call("GET", "/api/v1/invoicing/invoices/"+i1.ID+"/attachments", nil)
	if code != 200 || len(out["data"].([]interface{})) != 1 {
		t.Fatalf("attachments list: %d %v", code, out)
	}

	// Multi-currency, part paid.
	i2 := e.inv("INV-000002")
	if i2.Currency != "USD" || i2.Status != invoicing.StatusPartial || !near(i2.Total, 900) || !near(i2.BalanceDue, 400) || !near(i2.ExchangeRate, 0.666667) || i2.RateSource != "zoho" {
		t.Fatalf("i2: %s %s total %v bal %v rate %v src %s", i2.Currency, i2.Status, i2.Total, i2.BalanceDue, i2.ExchangeRate, i2.RateSource)
	}
	if !near(i2.TotalBase, 1350) {
		t.Fatalf("i2 base total %v", i2.TotalBase)
	}
	// Void.
	i3 := e.inv("INV-000003")
	if i3.Status != invoicing.StatusVoid || i3.BalanceDue != 0 || i3.VoidedAt == nil {
		t.Fatalf("i3: %+v", i3.Status)
	}
	// Settled by a credit note; shipping + adjustment line.
	i4 := e.inv("INV-000004")
	if i4.Status != invoicing.StatusPaid || !near(i4.CreditsApplied, 230) || i4.BalanceDue != 0 || len(i4.Payments) != 0 || !near(i4.ShippingAmount, 15) || len(i4.Items) != 2 || i4.ImportFlag != "" {
		t.Fatalf("i4: %s credits %v bal %v items %d flag %q", i4.Status, i4.CreditsApplied, i4.BalanceDue, len(i4.Items), i4.ImportFlag)
	}
	var cn invoicing.Invoice
	e.db.Where("organization_id = ? AND number = ? AND doc_type = ?", e.org, "CN-00001", invoicing.TypeCreditNote).First(&cn)
	if cn.ID == "" || !near(cn.Total, 230) || cn.Status != invoicing.StatusPaid {
		t.Fatalf("credit note: %+v", cn)
	}
	// Zoho's rounding kept and flagged.
	i5 := e.inv("INV-000005")
	if !near(i5.Total, 99.00) || !near(i5.TaxTotal, 9.00) || !near(i5.BalanceDue, 99) || !strings.Contains(i5.ImportFlag, "Totals kept") {
		t.Fatalf("i5: total %v tax %v flag %q", i5.Total, i5.TaxTotal, i5.ImportFlag)
	}
	if !near(i5.Subtotal-i5.DiscountTotal+i5.TaxTotal+i5.ShippingAmount, i5.Total) {
		t.Fatalf("i5 breakdown doesn't add up: %v %v %v", i5.Subtotal, i5.DiscountTotal, i5.TaxTotal)
	}
	if i6 := e.inv("INV-000006"); i6.Status != invoicing.StatusDraft {
		t.Fatalf("draft: %s", i6.Status)
	}

	// No notifications / emails: activity types are import-only.
	var types []string
	e.db.Model(&invoicing.Activity{}).Where("organization_id = ?", e.org).Distinct().Pluck("type", &types)
	for _, ty := range types {
		if !strings.HasPrefix(ty, "imported") {
			t.Fatalf("unexpected activity type %q", ty)
		}
	}

	// Numbering continues after the highest imported numbers.
	var s invoicing.Settings
	e.db.Where("organization_id = ?", e.org).First(&s)
	if s.InvoicePrefix != "INV-" || s.NextInvoiceNumber != 231 || s.NumberPadding != 6 || s.QuotePrefix != "EST-" || s.NextQuoteNumber != 12 {
		t.Fatalf("numbering: %+v", s)
	}
	// Reconciliation.
	sum := j["summary"].(map[string]interface{})
	for _, x := range sum["reconciliation"].([]interface{}) {
		r := x.(map[string]interface{})
		if r["ok"] != true {
			t.Fatalf("reconciliation mismatch: %v", r)
		}
	}
	// Tax rates.
	var taxes int64
	e.db.Model(&invoicing.TaxRate{}).Where("organization_id = ?", e.org).Count(&taxes)
	if taxes != 2 {
		t.Fatalf("taxes: %d", taxes)
	}

	// ---------- re-sync: nothing duplicated, changes applied, local edits kept ----------
	e.fake.mu.Lock()
	e.fake.invoices[4]["status"] = "paid"
	e.fake.invoices[4]["balance"] = 0
	e.fake.invoices[4]["payments"] = []interface{}{map[string]interface{}{"payment_id": "pay9", "invoice_payment_id": "ip9", "payment_mode": "Cash", "date": "2025-06-01", "amount": 99}}
	e.fake.invoices[4]["last_modified_time"] = "2025-06-01T10:00:00+1000"
	e.fake.invoices[5]["notes"] = "changed in zoho"
	e.fake.invoices[5]["last_modified_time"] = "2025-06-02T10:00:00+1000"
	e.fake.invoices = append(e.fake.invoices, map[string]interface{}{"invoice_id": "inew", "invoice_number": "INV-000231", "customer_id": "c2", "customer_name": "Globex Inc", "status": "sent",
		"date": "2025-08-01", "due_date": "2099-08-15", "currency_code": "USD", "exchange_rate": 1.6, "line_items": []interface{}{map[string]interface{}{"name": "New", "quantity": 1, "rate": 50, "item_total": 50}},
		"sub_total": 50, "total": 50, "balance": 50, "last_modified_time": "2025-08-01T10:00:00+1000"})
	e.fake.mu.Unlock()
	// Local edit to the draft after import.
	time.Sleep(3 * time.Second)
	e.db.Model(&invoicing.Invoice{}).Where("organization_id = ? AND number = ?", e.org, "INV-000006").Updates(map[string]interface{}{"title": "edited here", "updated_at": time.Now()})
	callsBefore := e.fake.calls["/invoices/i100"]
	pv2 := e.runPreview(nil)
	j2 := e.start(pv2["id"].(string), nil)
	if j2["status"] != JobCompleted {
		t.Fatalf("re-sync: %v %v", j2["status"], j2["error"])
	}
	var n int64
	e.db.Model(&invoicing.Invoice{}).Where("organization_id = ? AND doc_type = 'invoice'", e.org).Count(&n)
	if n != 231 {
		t.Fatalf("after re-sync %d invoices, want 231", n)
	}
	e.db.Model(&models.Customer{}).Where("organization_id = ?", e.org).Count(&n)
	if n != 3 {
		t.Fatalf("customers duplicated: %d", n)
	}
	if e.fake.calls["/invoices/i100"] != callsBefore {
		t.Fatal("unchanged invoices must not be fetched again")
	}
	c2 := counts(j2, EntInvoices)
	if c2["created"].(float64) != 1 || c2["updated"].(float64) != 1 || c2["skipped"].(float64) != 1 || c2["unchanged"].(float64) != 228 {
		t.Fatalf("re-sync counts: %v", c2)
	}
	if i5 := e.inv("INV-000005"); i5.Status != invoicing.StatusPaid || len(i5.Payments) != 1 || i5.BalanceDue != 0 {
		t.Fatalf("i5 after re-sync: %s %d %v", i5.Status, len(i5.Payments), i5.BalanceDue)
	}
	if i6 := e.inv("INV-000006"); i6.Title != "edited here" || i6.Notes == "changed in zoho" {
		t.Fatal("local edit must win over the re-sync")
	}
	e.db.Where("organization_id = ?", e.org).First(&s)
	if s.NextInvoiceNumber != 232 {
		t.Fatalf("next number after re-sync: %d", s.NextInvoiceNumber)
	}

	// ---------- undo the re-sync: only its new invoice and payment go ----------
	code, out = e.call("POST", "/api/v1/imports/jobs/"+pv2["id"].(string)+"/undo", map[string]bool{"confirm": true})
	if code != 200 {
		t.Fatalf("undo: %d %v", code, out)
	}
	e.db.Model(&invoicing.Invoice{}).Where("organization_id = ? AND doc_type = 'invoice'", e.org).Count(&n)
	if n != 230 {
		t.Fatalf("after undo of re-sync %d invoices", n)
	}
	if i5 := e.inv("INV-000005"); len(i5.Payments) != 0 || !near(i5.BalanceDue, 99) {
		t.Fatalf("payment added by the re-sync must be removed: %d %v", len(i5.Payments), i5.BalanceDue)
	}
	// Undo the first import: everything except the locally edited draft (and its customer).
	code, out = e.call("POST", "/api/v1/imports/jobs/"+pv["id"].(string)+"/undo", map[string]bool{"confirm": true})
	if code != 200 {
		t.Fatalf("undo 1: %d %v", code, out)
	}
	e.db.Model(&invoicing.Invoice{}).Where("organization_id = ?", e.org).Count(&n)
	if n != 1 {
		t.Fatalf("after undo %d documents remain, want 1 (the edited draft)", n)
	}
	e.db.Model(&models.Customer{}).Where("organization_id = ?", e.org).Count(&n)
	if n != 1 {
		t.Fatalf("after undo %d customers remain, want 1 (has the edited draft)", n)
	}
}

func toStrings(v interface{}) []string {
	var out []string
	if a, ok := v.([]interface{}); ok {
		for _, x := range a {
			out = append(out, x.(string))
		}
	}
	return out
}

func TestTokenRefreshAndDailyLimit(t *testing.T) {
	e := newEnv(t, 6)
	e.connect()
	// Expired access token mid-run → refreshed once, job completes.
	pv := e.runPreview(nil)
	e.fake.mu.Lock()
	e.fake.expireNext = true
	before := e.fake.refreshes
	e.fake.mu.Unlock()
	j := e.start(pv["id"].(string), nil)
	if j["status"] != JobCompleted {
		t.Fatalf("status %v %v", j["status"], j["error"])
	}
	if e.fake.refreshes <= before {
		t.Fatal("expected a token refresh after 401")
	}
	// Daily limit: the job pauses with a resume time instead of failing.
	e.fake.mu.Lock()
	e.fake.invoices[0]["last_modified_time"] = "2026-01-01T00:00:00+1000"
	e.fake.mu.Unlock()
	pv2 := e.runPreview(nil)
	e.fake.mu.Lock()
	e.fake.dailyLimit = true
	e.fake.mu.Unlock()
	j2 := e.start(pv2["id"].(string), nil)
	if j2["status"] != JobPaused || j2["resume_at"] == nil {
		t.Fatalf("daily limit: status %v resume %v", j2["status"], j2["resume_at"])
	}
	// Limit lifted → resume finishes the job.
	e.fake.mu.Lock()
	e.fake.dailyLimit = false
	e.fake.mu.Unlock()
	code, out := e.call("POST", "/api/v1/imports/jobs/"+pv2["id"].(string)+"/resume", nil)
	if code != 200 {
		t.Fatalf("resume: %d %v", code, out)
	}
	e.h.Engine.Wait(pv2["id"].(string), 30*time.Second)
	_, out = e.call("GET", "/api/v1/imports/jobs/"+pv2["id"].(string), nil)
	if dataOf(out)["status"] != JobCompleted {
		t.Fatalf("after resume: %v", dataOf(out)["status"])
	}
}

func TestExistingCustomerMatching(t *testing.T) {
	e := newEnv(t, 6)
	// An existing customer with the same email, and an existing invoice number.
	existing := models.Customer{ID: uuid.NewString(), OrganizationID: e.org, FirstName: "ACME (old)", Email: "accounts@acme.test", IsActive: true}
	e.db.Omit(clause.Associations).Create(&existing)
	e.db.Create(&invoicing.Invoice{ID: uuid.NewString(), OrganizationID: e.org, DocType: "invoice", Number: "INV-000003", Status: "draft", PublicToken: uuid.NewString(), IssueDate: time.Now(), DueDate: time.Now(), Currency: "AUD"})
	e.connect()
	pv := e.runPreview(nil)
	dups := pv["preview"].(map[string]interface{})["duplicates"].([]interface{})
	if len(dups) != 2 {
		t.Fatalf("duplicates: %v", dups)
	}
	j := e.start(pv["id"].(string), nil)
	if j["status"] != JobCompleted {
		t.Fatalf("%v %v", j["status"], j["error"])
	}
	var n int64
	e.db.Model(&models.Customer{}).Where("organization_id = ?", e.org).Count(&n)
	if n != 3 { // merged into the existing customer
		t.Fatalf("customers: %d", n)
	}
	var cu models.Customer
	e.db.Where("id = ?", existing.ID).First(&cu)
	if cu.FirstName != "ACME (old)" || cu.TaxNumber != "51 824 753 556" {
		t.Fatalf("merge must keep existing values and fill blanks: %+v", cu)
	}
	var docs int64
	e.db.Model(&invoicing.Invoice{}).Where("organization_id = ? AND number = 'INV-000003'", e.org).Count(&docs)
	if docs != 1 {
		t.Fatal("existing invoice number must be skipped by default")
	}
	e.db.Model(&invoicing.Invoice{}).Where("organization_id = ? AND customer_id = ?", e.org, existing.ID).Count(&docs)
	if docs == 0 {
		t.Fatal("documents must go to the merged customer")
	}
}

// ---------- files ----------

func upload(e *env, paths ...string) map[string]interface{} {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			e.t.Fatal(err)
		}
		fw, _ := mw.CreateFormFile("files", filepath.Base(p))
		_, _ = fw.Write(b)
	}
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/imports/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Org", e.org)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if w.Code != 200 {
		e.t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	out := map[string]interface{}{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return dataOf(out)
}

func TestDetectZohoExports(t *testing.T) {
	want := map[string]string{
		"Contacts.csv": FileContacts, "Contact_Persons.csv": FilePersons, "Item.csv": FileItems, "Invoice.csv": FileInvoices,
		"Estimate.csv": FileQuotes, "Credit_Note.csv": FileCredits, "Customer_Payment.csv": FilePayments,
		"generic_customers.csv": FileGenCust, "generic_invoices.csv": FileGenInv,
	}
	for name, kind := range want {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		h, rows, err := ReadTable(name, b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := DetectKind(name, h); got != kind {
			t.Errorf("%s detected as %q, want %q", name, got, kind)
		}
		if len(rows) == 0 {
			t.Errorf("%s: no rows", name)
		}
		m := AutoMap(kind, h)
		for _, f := range kindByName(kind).Fields {
			if f.Required && m[f.Key] == "" {
				t.Errorf("%s: required field %s not mapped", name, f.Key)
			}
		}
	}
	// XLSX with the same content as Contacts.csv.
	b, _ := os.ReadFile(filepath.Join("testdata", "Contacts.xlsx"))
	h, rows, err := ReadTable("Contacts.xlsx", b)
	if err != nil || DetectKind("Contacts.xlsx", h) != FileContacts || len(rows) != 3 {
		t.Fatalf("xlsx: %v %v %d", err, h, len(rows))
	}
	// Old .xls refused with advice.
	if _, _, err := ReadTable("x.xls", []byte{0xD0, 0xCF, 0x11, 0xE0, 0, 0}); err == nil || !strings.Contains(err.Error(), "CSV") {
		t.Fatalf("xls: %v", err)
	}
}

func TestZohoFileImport(t *testing.T) {
	e := newEnv(t, 6)
	dir := "testdata"
	res := upload(e, filepath.Join(dir, "Contacts.csv"), filepath.Join(dir, "Contact_Persons.csv"), filepath.Join(dir, "Item.csv"), filepath.Join(dir, "Invoice.csv"),
		filepath.Join(dir, "Estimate.csv"), filepath.Join(dir, "Credit_Note.csv"), filepath.Join(dir, "Customer_Payment.csv"))
	jobID := res["job"].(map[string]interface{})["id"].(string)
	code, out := e.call("POST", "/api/v1/imports/jobs/"+jobID+"/preview", nil)
	if code != 200 {
		t.Fatalf("preview: %d %v", code, out)
	}
	e.h.Engine.Wait(jobID, 20*time.Second)
	_, out = e.call("GET", "/api/v1/imports/jobs/"+jobID, nil)
	if dataOf(out)["status"] != JobPreview {
		t.Fatalf("preview: %v %v", dataOf(out)["status"], dataOf(out)["error"])
	}
	j := e.start(jobID, nil)
	if j["status"] != JobCompleted {
		t.Fatalf("file import: %v %v", j["status"], j["error"])
	}
	inv := e.inv("INV-000101")
	if len(inv.Items) != 2 || !near(inv.Total, 330) || inv.Status != invoicing.StatusPaid || len(inv.Payments) != 1 || inv.BalanceDue != 0 || inv.Source != "zoho" {
		t.Fatalf("INV-000101: items %d total %v status %s pays %d bal %v", len(inv.Items), inv.Total, inv.Status, len(inv.Payments), inv.BalanceDue)
	}
	if inv.IssueDate.Format("2006-01-02") != "2025-03-14" {
		t.Fatalf("dd/mm date: %v", inv.IssueDate)
	}
	inv2 := e.inv("INV-000102")
	if inv2.Currency != "USD" || inv2.Status != invoicing.StatusPartial || !near(inv2.BalanceDue, 60) || !near(inv2.ExchangeRate, 0.666667) {
		t.Fatalf("INV-000102: %s %s %v %v", inv2.Currency, inv2.Status, inv2.BalanceDue, inv2.ExchangeRate)
	}
	var cust models.Customer
	e.db.Where("organization_id = ? AND external_id = ?", e.org, "460000000026049").First(&cust)
	var cts int64
	e.db.Model(&models.CustomerContact{}).Where("customer_id = ?", cust.ID).Count(&cts)
	if cust.FirstName != "Acme Pty Ltd" || cts != 2 {
		t.Fatalf("customer from files: %+v contacts %d", cust, cts)
	}
	var q invoicing.Invoice
	e.db.Where("organization_id = ? AND number = 'EST-000005'", e.org).First(&q)
	if q.DocType != invoicing.TypeQuote || q.Status != invoicing.StatusAccepted {
		t.Fatalf("quote: %+v", q.Status)
	}
	var s invoicing.Settings
	e.db.Where("organization_id = ?", e.org).First(&s)
	if s.NextInvoiceNumber != 103 || s.NextQuoteNumber != 6 {
		t.Fatalf("numbering: %d %d", s.NextInvoiceNumber, s.NextQuoteNumber)
	}
	// Same files again → nothing new.
	res2 := upload(e, filepath.Join(dir, "Contacts.csv"), filepath.Join(dir, "Invoice.csv"), filepath.Join(dir, "Customer_Payment.csv"))
	id2 := res2["job"].(map[string]interface{})["id"].(string)
	e.call("POST", "/api/v1/imports/jobs/"+id2+"/preview", nil)
	e.h.Engine.Wait(id2, 20*time.Second)
	j2 := e.start(id2, nil)
	if c := counts(j2, EntInvoices); c["created"].(float64) != 0 || c["unchanged"].(float64) != 2 {
		t.Fatalf("second file import: %v", c)
	}
	var n int64
	e.db.Model(&invoicing.Invoice{}).Where("organization_id = ? AND doc_type = 'invoice'", e.org).Count(&n)
	if n != 2 {
		t.Fatalf("duplicates after re-import: %d", n)
	}
	// Then the API import of the same organisation links to the same records
	// (Zoho ids match): contacts are not duplicated.
}

func TestGenericCSV(t *testing.T) {
	e := newEnv(t, 6)
	res := upload(e, filepath.Join("testdata", "generic_customers.csv"), filepath.Join("testdata", "generic_invoices.csv"))
	jobID := res["job"].(map[string]interface{})["id"].(string)
	e.call("POST", "/api/v1/imports/jobs/"+jobID+"/preview", nil)
	e.h.Engine.Wait(jobID, 20*time.Second)
	j := e.start(jobID, nil)
	if j["status"] != JobCompleted {
		t.Fatalf("%v %v", j["status"], j["error"])
	}
	inv := e.inv("1001")
	if inv.Source != "csv" || !near(inv.Total, 220) || inv.Status != invoicing.StatusPaid || len(inv.Payments) != 1 {
		t.Fatalf("generic invoice: %+v total %v status %s pays %d", inv.Source, inv.Total, inv.Status, len(inv.Payments))
	}
	inv2 := e.inv("1002")
	if !near(inv2.Total, 55) || !near(inv2.BalanceDue, 55) || inv2.CustomerID == nil {
		t.Fatalf("generic invoice 2: %v %v", inv2.Total, inv2.BalanceDue)
	}
	var s invoicing.Settings
	e.db.Where("organization_id = ?", e.org).First(&s)
	if s.InvoicePrefix != "" || s.NextInvoiceNumber != 1003 {
		t.Fatalf("numbering for plain numbers: %q %d", s.InvoicePrefix, s.NextInvoiceNumber)
	}
}

func TestNumberingContinuation(t *testing.T) {
	db := testDB(t)
	org := "num-" + uuid.NewString()
	var n *invoicing.Numbering
	_ = db.Transaction(func(tx *gorm.DB) error {
		var err error
		n, err = invoicing.ContinueNumbering(tx, org, invoicing.TypeInvoice, []string{"INV-000123", "INV-000009", "OLD-5", "INV-000120"})
		return err
	})
	if n == nil || n.Prefix != "INV-" || n.Next != 124 || n.NextNumber != "INV-000124" || n.Padding != 6 {
		t.Fatalf("%+v", n)
	}
	// Never collides with documents already numbered higher.
	db.Create(&invoicing.Invoice{ID: uuid.NewString(), OrganizationID: org, DocType: "invoice", Number: "INV-000499", Status: "draft", PublicToken: uuid.NewString(), IssueDate: time.Now(), DueDate: time.Now(), Currency: "AUD"})
	_ = db.Transaction(func(tx *gorm.DB) error {
		n, _ = invoicing.ContinueNumbering(tx, org, invoicing.TypeInvoice, []string{"INV-000123"})
		return nil
	})
	if n.Next != 500 {
		t.Fatalf("went backwards: %d", n.Next)
	}
	if !invoicing.RestoreNumbering(db, org, n) {
		t.Fatal("restore")
	}
}

func TestParseHelpers(t *testing.T) {
	cases := map[string]string{"2025-03-14": "2025-03-14", "14/03/2025": "2025-03-14", "03/04/2025": "2025-04-03", "14 Mar 2025": "2025-03-14", "2025-03-14T10:00:00+1000": "2025-03-14"}
	for in, want := range cases {
		d, ok := parseDay(in, true)
		if !ok || d.Format("2006-01-02") != want {
			t.Errorf("%s → %v %v", in, d, ok)
		}
	}
	if d, _ := parseDay("03/14/2025", true); d.Format("2006-01-02") != "2025-03-14" {
		t.Errorf("mm/dd fallback: %v", d)
	}
	for in, want := range map[string]float64{"1,234.50": 1234.5, "10.00%": 10, "$12": 12, "(5.00)": -5} {
		if v, _ := parseNum(in); v != want {
			t.Errorf("%s → %v", in, v)
		}
	}
	for in, want := range map[string]string{"Bank Transfer": "bank_transfer", "Credit Card": "card", "Check": "cheque", "Cash": "cash", "M-Pesa": "mobile_money", "Bitcoin": "other"} {
		if got := mapMethod(in); got != want {
			t.Errorf("%s → %s", in, got)
		}
	}
	for in, want := range map[string]string{"overdue": "sent", "viewed": "sent", "partially_paid": "partial", "paid": "paid", "void": "void", "draft": "draft"} {
		if got := mapStatus(KindInvoice, in); got != want {
			t.Errorf("status %s → %s", in, got)
		}
	}
	if !trustedAccounts("https://accounts.zoho.com.au") || trustedAccounts("https://accounts.zoho.com.au.evil.com") || trustedAccounts("http://accounts.zoho.com") {
		t.Error("trustedAccounts")
	}
	if FindDC("").Accounts != "https://accounts.zoho.com.au" && os.Getenv("ZOHO_ACCOUNTS_URL") == "" {
		t.Error("default DC must be Australia")
	}
	_ = http.StatusOK
}
