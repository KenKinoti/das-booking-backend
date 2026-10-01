package expenses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/pnl"
)

// Service tests run against Postgres when TEST_DATABASE_URL is set, in their
// own schema.
func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`DROP SCHEMA IF EXISTS expenses_test CASCADE`)
	boot.Exec(`CREATE SCHEMA expenses_test`)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	if !strings.Contains(dsn, "?") && strings.Contains(dsn, "&") {
		dsn = strings.Replace(dsn, "&", "?", 1)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=expenses_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(append(Models(), &finance.ChartOfAccount{}, &finance.JournalEntry{}, &finance.JournalEntryLine{}, &finance.Bill{}, &finance.BillLineItem{},
		&finance.Vendor{}, &finance.Payment{}, &finance.BankAccount{}, &finance.BankTransaction{}, &finance.GeneralLedger{}, &finance.AccountBalance{}, &finance.AuditTrail{},
		&fxrates.ManualRate{}, &notify.Notification{})...); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) primary key, name text, country_code text, currency text, timezone text, region_source text, address_country text)`,
		`CREATE TABLE users (id varchar(255) primary key, email text, role text, first_name text, last_name text, organization_id varchar(255), is_active boolean default true, created_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE customers (id varchar(255) primary key, organization_id varchar(255), first_name text, last_name text, email text, company_name text, website text, deleted_at timestamptz)`,
		`CREATE TABLE billing_invoice_items (id varchar(64) primary key, invoice_id varchar(64), description text, quantity numeric, unit text, line_subtotal numeric, line_discount numeric, line_total numeric)`,
		`CREATE TABLE billing_invoices (id varchar(64) primary key, organization_id varchar(255), number text, doc_type text, status text, customer_id varchar(255), currency text,
			total numeric, tax_total numeric, total_base numeric, issue_date date, deleted_at timestamptz)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	notify.Disabled = true
	return db
}

func newOrg(t *testing.T, db *gorm.DB, cur string) string {
	org := uuid.NewString()
	db.Exec(`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES (?, 'Test Co', 'AU', ?, 'Australia/Sydney', 'user')`, org, cur)
	return org
}

// fakeGoogle serves the OAuth token endpoint and the Gmail API.
type fakeGoogle struct {
	msgs     map[string]*Message
	atts     map[string][]byte
	listHits int
}

func gmailMsg(id, from, subject string, when time.Time, body string, atts ...[3]string) *Message {
	m := &Message{ID: id, ThreadID: id, InternalDate: fmt.Sprint(when.UnixMilli())}
	m.Payload.MimeType = "multipart/mixed"
	m.Payload.Headers = append(m.Payload.Headers, struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}{"From", from}, struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}{"Subject", subject})
	text := MessagePart{MimeType: "text/plain"}
	text.Body.Data = b64url([]byte(body))
	html := MessagePart{MimeType: "text/html"}
	html.Body.Data = b64url([]byte("<html><body><p>" + strings.ReplaceAll(body, "\n", "<br>") + "</p></body></html>"))
	alt := MessagePart{MimeType: "multipart/alternative", Parts: []MessagePart{text, html}}
	m.Payload.Parts = []MessagePart{alt}
	for _, a := range atts { // name, mime, attachment id
		p := MessagePart{MimeType: a[1], Filename: a[0]}
		p.Body.AttachmentID = a[2]
		m.Payload.Parts = append(m.Payload.Parts, p)
	}
	return m
}

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case p == "/token":
		r.ParseForm()
		if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csecret" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"invalid_client"}`)
			return
		}
		if r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code") != "good-code" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"Bad code"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3599,"scope":"`+GmailScope+`"}`)
	case r.Header.Get("Authorization") != "Bearer at-1":
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"code":401,"message":"Invalid Credentials"}}`)
	case p == "/gmail/v1/users/me/profile":
		fmt.Fprint(w, `{"emailAddress":"owner@example.com.au","messagesTotal":1000}`)
	case p == "/gmail/v1/users/me/messages":
		f.listHits++
		q := r.URL.Query().Get("q")
		ids := []map[string]string{}
		for id, m := range f.msgs {
			from := strings.ToLower(m.Header("From"))
			if (strings.Contains(q, "payments-noreply@google.com") && strings.Contains(from, "google.com")) ||
				(strings.Contains(q, "synergywholesale.com") && strings.Contains(from, "synergywholesale.com")) {
				ids = append(ids, map[string]string{"id": id, "threadId": id})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"messages": ids, "resultSizeEstimate": len(ids)})
	case strings.Contains(p, "/attachments/"):
		parts := strings.Split(p, "/")
		data, ok := f.atts[parts[len(parts)-1]]
		if !ok {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"size": len(data), "data": b64url(data)})
	case strings.HasPrefix(p, "/gmail/v1/users/me/messages/"):
		id := strings.TrimPrefix(p, "/gmail/v1/users/me/messages/")
		m, ok := f.msgs[id]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"code":404,"message":"Not Found"}}`)
			return
		}
		json.NewEncoder(w).Encode(m)
	default:
		w.WriteHeader(404)
	}
}

func setupGoogle(t *testing.T) *fakeGoogle {
	day := func(s string) time.Time { d, _ := time.Parse("2006-01-02 15:04", s); return d }
	f := &fakeGoogle{msgs: map[string]*Message{}, atts: map[string][]byte{
		"att-syn-pdf": makePDF(synergyInvoicePDFLines, false),
		"att-csv":     []byte(synergyStatementCSV),
	}}
	g := "Google Payments <payments-noreply@google.com>"
	s := "Synergy Wholesale <noreply@synergywholesale.com>"
	f.msgs["g-inv"] = gmailMsg("g-inv", g, "Google Workspace: Your invoice is available for example.com.au", day("2026-09-02 17:08"), googleInvoiceEmail)
	f.msgs["g-pay"] = gmailMsg("g-pay", g, "Google Workspace: We've received your payment for example.com.au", day("2026-09-03 23:32"),
		strings.Replace(googlePaymentEmail, "Sep 29, 2026", "Sep 3, 2026", 1))
	f.msgs["g-dec"] = gmailMsg("g-dec", g, "Google Workspace: Payment was declined for example.com.au", day("2026-09-01 13:31"), googleDeclinedEmail)
	f.msgs["s-rcpt"] = gmailMsg("s-rcpt", s, "Payment receipt for invoice 2746402", day("2026-09-30 03:20"), synergyReceiptEmail,
		[3]string{"invoice-2746402.pdf", "application/pdf", "att-syn-pdf"})
	f.msgs["s-low"] = gmailMsg("s-low", s, "Synergy Wholesale - Low Balance Notification", day("2026-09-30 00:43"), synergyLowBalanceEmail)
	f.msgs["s-stmt"] = gmailMsg("s-stmt", s, "Your monthly transaction statement for September", day("2026-09-30 14:23"), synergyStatementEmail,
		[3]string{"statement-September-2026.csv", "application/csv", "att-csv"})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("GOOGLE_OAUTH_TOKEN_URL", srv.URL+"/token")
	t.Setenv("GOOGLE_OAUTH_AUTH_URL", srv.URL+"/auth")
	t.Setenv("GMAIL_API_BASE", srv.URL)
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "csecret")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("EXPENSES_IP_ECHO_URL", "off")
	return f
}

func items(t *testing.T, s *Service, org string) map[string]Item {
	var all []Item
	s.DB.Where("organization_id = ?", org).Find(&all)
	out := map[string]Item{}
	for _, it := range all {
		out[it.ExternalID] = it
	}
	return out
}

func TestGmailSyncDedupeApprovePnL(t *testing.T) {
	db := testDB(t)
	fg := setupGoogle(t)
	org := newOrg(t, db, "AUD")
	s := NewService(db, &fxrates.Service{DB: db})
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	// Manual EUR→AUD rate for deterministic conversion.
	if _, err := s.FX.SaveManual(org, "u1", "EUR", "AUD", 1.65, "test"); err != nil {
		t.Fatal(err)
	}

	// Connect Gmail through the OAuth flow.
	if _, err := s.SaveGmail(org, GmailInput{Presets: []string{"google_workspace", "synergy"}}); err != nil {
		t.Fatal(err)
	}
	u, err := s.StartGmail(org, "u1", "https://api.example.com/api/v1/public/google/callback", "https://app.example.com")
	if err != nil || !strings.Contains(u, "scope=https%3A%2F%2Fwww.googleapis.com%2Fauth%2Fgmail.readonly") || !strings.Contains(u, "access_type=offline") {
		t.Fatalf("auth url %q %v", u, err)
	}
	state := s.connection(org, "gmail").State
	if _, err := s.FinishGmail(ctx, state, "bad-code", "x"); err == nil {
		t.Fatal("bad code should fail")
	}
	u, _ = s.StartGmail(org, "u1", "x", "")
	state = s.connection(org, "gmail").State
	if _, err := s.FinishGmail(ctx, state, "good-code", "x"); err != nil {
		t.Fatal(err)
	}
	if c := s.connection(org, "gmail"); c.Account != "owner@example.com.au" || dec(c.SecretEnc) != "rt-1" || c.SecretEnc == "rt-1" {
		t.Fatalf("connection %+v", c)
	}

	r := s.SyncGmail(ctx, org)
	if !r.OK {
		t.Fatalf("sync: %+v", r)
	}
	its := items(t, s, org)
	inv, pay := its["g-inv"], its["g-pay"]
	// The invoice email has no amount; the payment confirmation completes it.
	if inv.Status != StatusPending || inv.InvoiceNumber != "5671877634" || !near(inv.Total, 36) || inv.Currency != "EUR" {
		t.Fatalf("google invoice %+v", inv)
	}
	if !near(inv.FxRate, 1.65) || inv.FxSource != "manual" || !near(inv.HomeTotal, 59.40) || inv.HomeCurrency != "AUD" {
		t.Fatalf("fx %+v", inv)
	}
	if pay.Status != StatusDuplicate || pay.DuplicateOf != inv.ID {
		t.Fatalf("payment should complete the invoice: %+v", pay)
	}
	if d := its["g-dec"]; d.Kind != KindDeclined || d.Status != StatusIgnored {
		t.Fatalf("declined %+v", d)
	}
	rc := its["s-rcpt"]
	// The attached tax invoice gives the real GST (not the 1/11 estimate).
	if rc.Status != StatusPending || rc.InvoiceNumber != "2746402" || !near(rc.Total, 45) || !near(rc.Tax, 4.09) || rc.DocumentID == "" || rc.Category != CatDomains {
		t.Fatalf("synergy receipt %+v", rc)
	}
	if st := its["s-stmt"]; st.Status != StatusIgnored {
		t.Fatalf("statement should be stored, not counted: %+v", st)
	}
	if c := s.connection(org, "synergy"); c == nil || c.Balance == nil || !near(*c.Balance, -58.83) {
		t.Fatalf("low-balance email should record the balance: %+v", c)
	}
	n := len(its)

	// Re-sync never duplicates.
	s.DB.Model(&Connection{}).Where("organization_id = ? AND kind = 'gmail'", org).Update("last_status", "error") // force a full look-back
	r2 := s.SyncGmail(ctx, org)
	if !r2.OK || r2.Created != 0 || len(items(t, s, org)) != n {
		t.Fatalf("re-sync created items: %+v", r2)
	}

	// The same invoice uploaded as a PDF is detected as a duplicate and adds nothing.
	up, err := s.Upload(ctx, org, "invoice-2746402.pdf", "application/pdf", makePDF(synergyInvoicePDFLines, true), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if up.Items[0].Status != StatusDuplicate || up.Items[0].DuplicateOf != rc.ID {
		t.Fatalf("upload dup %+v", up.Items[0])
	}
	// Uploading the identical file again is idempotent.
	up2, _ := s.Upload(ctx, org, "copy.pdf", "application/pdf", makePDF(synergyInvoicePDFLines, true), "", "")
	if up2.Created != 0 || up2.Items[0].ID != up.Items[0].ID {
		t.Fatalf("re-upload %+v", up2)
	}

	// Approve both → paid supplier bills.
	a1, err := s.Approve(org, "u1", inv.ID, ApproveInput{})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.Approve(org, "u1", rc.ID, ApproveInput{})
	if err != nil {
		t.Fatal(err)
	}
	if a1.BillID == "" || a2.BillID == "" || a1.Status != StatusApproved {
		t.Fatalf("approve %+v %+v", a1, a2)
	}
	if _, err := s.Approve(org, "u1", up.Items[0].ID, ApproveInput{}); err == nil {
		t.Fatal("approving the duplicate must fail")
	}
	var bills []finance.Bill
	db.Where("organization_id = ?", org).Preload("LineItems").Find(&bills)
	if len(bills) != 2 {
		t.Fatalf("bills %d", len(bills))
	}
	for _, b := range bills {
		if b.Status != "paid" {
			t.Fatalf("bill not paid: %+v", b)
		}
	}

	// P&L: counted once, as operating expenses by category, excluding tax.
	check := func(wantEmail, wantDomains float64) {
		t.Helper()
		in := pnl.Load(db, org, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), time.UTC)
		in.Home, in.OrgHome, in.Decimals = "AUD", "AUD", 2
		for _, basis := range []string{pnl.Accrual, pnl.Cash} {
			st := pnl.Compute(in, pnl.Period{From: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}, basis)
			got := map[string]float64{}
			for _, l := range st.SortedLines(pnl.SecExpenses) {
				got[l.Label] = float64(l.Amount) / 100
			}
			if !near(got["Email & Workspace"], wantEmail) || !near(got["Domain Names"], wantDomains) {
				t.Fatalf("%s P&L lines %v", basis, got)
			}
			if !near(float64(st.Totals().Expenses)/100, wantEmail+wantDomains) {
				t.Fatalf("%s total %v", basis, st.Totals().Expenses)
			}
		}
	}
	check(59.40, 40.91)

	// Dashboard figures match (home currency, ex tax, by invoice date).
	d := s.DashboardFor(org, now, "2026-01-01", "2026-12-31")
	if !near(d.KPIs["ytd"].(float64), 100.31) {
		t.Fatalf("ytd %v", d.KPIs["ytd"])
	}
	if !near(d.KPIs["last_month"].(float64), 100.31) {
		t.Fatalf("last month %v (both invoices are dated in Sep… google by its payment-completed invoice date)", d.KPIs["last_month"])
	}

	// Undo approval removes the bill and the P&L line.
	if _, err := s.Unapprove(org, "u1", inv.ID); err != nil {
		t.Fatal(err)
	}
	check(0, 40.91)
	var nb int64
	db.Model(&finance.Bill{}).Where("organization_id = ?", org).Count(&nb)
	if nb != 1 {
		t.Fatalf("bills after undo %d", nb)
	}

	// Margin per client: attribute the domain to a client billed 120 + GST.
	cust := uuid.NewString()
	db.Exec(`INSERT INTO customers (id, organization_id, first_name, last_name) VALUES (?, ?, 'Acme', 'Bakery')`, cust, org)
	db.Exec(`INSERT INTO billing_invoices (id, organization_id, number, doc_type, status, customer_id, currency, total, tax_total, total_base, issue_date)
		VALUES (?, ?, 'INV-1', 'invoice', 'paid', ?, 'AUD', 132, 12, 132, '2026-09-10')`, uuid.NewString(), org, cust)
	if _, err := s.SetAllocations(org, rc.ID, []Allocation{{CustomerID: cust, Percent: 100}}); err != nil {
		t.Fatal(err)
	}
	ms := s.ClientMargins(org, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	if len(ms) != 1 || ms[0].Customer != "Acme Bakery" || !near(ms[0].Revenue, 120) || !near(ms[0].Costs, 40.91) || !near(ms[0].Margin, 79.09) {
		t.Fatalf("margins %+v", ms)
	}
	if _, err := s.SetAllocations(org, rc.ID, []Allocation{{CustomerID: cust, Percent: 60}, {CustomerID: cust, Percent: 50}}); err == nil {
		t.Fatal("duplicate client in split must fail")
	}
	_ = fg
}

func TestSynergySyncRenewalsAndAlerts(t *testing.T) {
	db := testDB(t)
	setupGoogle(t)
	f := &fakeSynergy{reseller: "R1", key: "K1", balance: "12.40"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("SYNERGY_API_URL", srv.URL)
	org := newOrg(t, db, "AUD")
	s := NewService(db, &fxrates.Service{DB: db})
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := s.SaveSynergy(org, SynergyInput{ResellerID: "R1", APIKey: "K1"}); err != nil {
		t.Fatal(err)
	}
	if c := s.connection(org, "synergy"); c.SecretEnc == "K1" || dec(c.SecretEnc) != "K1" {
		t.Fatal("api key must be stored encrypted")
	}
	r := s.SyncSynergy(ctx, org)
	if !r.OK || r.Renewals != 3 || r.Balance == nil || !near(*r.Balance, 12.40) {
		t.Fatalf("sync %+v", r)
	}
	rens := s.Renewals(org, 0)
	if len(rens) != 3 {
		t.Fatalf("renewals %d", len(rens))
	}
	var dom *Renewal
	for i := range rens {
		if rens[i].Name == "example.com.au" && rens[i].Kind == "domain" {
			dom = &rens[i]
		}
	}
	if dom == nil || dom.Cost == nil || !near(*dom.Cost, 11.95) || *dom.DaysLeft != 19 || !dom.AutoRenew {
		t.Fatalf("domain renewal %+v", dom)
	}
	d := s.DashboardFor(org, now, "", "")
	types := map[string]int{}
	for _, a := range d.Alerts {
		types[a.Type]++
	}
	if types["renewal_due"] != 2 || types["low_balance"] != 1 {
		t.Fatalf("alerts %v", types)
	}
	// Notifications go out once per alert.
	notify.Disabled = false
	db.Exec(`INSERT INTO users (id, email, role, organization_id, is_active) VALUES ('u-admin', 'a@x', 'admin', ?, true)`, org)
	n1 := s.NotifyAlerts(org)
	n2 := s.NotifyAlerts(org)
	notify.Disabled = true
	if n1 < 3 || n2 != 0 {
		t.Fatalf("notifications %d then %d", n1, n2)
	}
	// Re-sync is idempotent for renewals; a manual cost survives.
	if _, err := s.SaveRenewal(org, dom.ID, RenewalInput{Cost: fp(19.95)}); err != nil {
		t.Fatal(err)
	}
	s.SyncSynergy(ctx, org)
	var after Renewal
	db.Where("id = ?", dom.ID).First(&after)
	var cnt int64
	db.Model(&Renewal{}).Where("organization_id = ?", org).Count(&cnt)
	if cnt != 3 || after.Cost == nil || !near(*after.Cost, 19.95) || after.CostSource != "manual" {
		t.Fatalf("after re-sync %d %+v", cnt, after)
	}
	// IP not whitelisted: explicit error recorded on the connection.
	f.blocked = true
	r = s.SyncSynergy(ctx, org)
	if r.OK || !strings.Contains(r.Hint, "whitelisted") {
		t.Fatalf("blocked %+v", r)
	}
	if c := s.connection(org, "synergy"); c.LastStatus != "error" {
		t.Fatalf("status %+v", c)
	}
}

func fp(v float64) *float64 { return &v }

func TestManualExpenseAndScheduleLock(t *testing.T) {
	db := testDB(t)
	setupGoogle(t)
	org := newOrg(t, db, "KES")
	s := NewService(db, &fxrates.Service{DB: db})
	if _, err := s.FX.SaveManual(org, "u", "USD", "KES", 129.5, ""); err != nil {
		t.Fatal(err)
	}
	cur, total, tax, date := "USD", 14.0, 0.0, "2026-09-15"
	vend := "Figma"
	it, err := s.CreateManual(org, ItemInput{VendorName: &vend, Currency: &cur, Total: &total, Tax: &tax, InvoiceDate: &date})
	if err != nil {
		t.Fatal(err)
	}
	if !near(it.HomeTotal, 1813) || it.HomeCurrency != "KES" || it.Category != CatOther {
		t.Fatalf("%+v", it)
	}
	// Same vendor/amount/date entered again → duplicate.
	it2, err := s.CreateManual(org, ItemInput{VendorName: &vend, Currency: &cur, Total: &total, InvoiceDate: &date})
	if err != nil || it2.Status != StatusDuplicate {
		t.Fatalf("dup %+v %v", it2, err)
	}
	// Advisory lock: a second holder is refused while the first runs.
	inner := true
	got := s.withLock(context.Background(), "expenses-sync:"+org, func(_ *gorm.DB) {
		inner = s.withLock(context.Background(), "expenses-sync:"+org, func(_ *gorm.DB) {})
	})
	if !got || inner {
		t.Fatalf("lock got=%v inner=%v", got, inner)
	}
	// Schedule due logic.
	c := Connection{Settings: Settings{AutoSync: true, Hour: 6}}
	loc, _ := time.LoadLocation("Africa/Nairobi")
	if due(c, time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC), loc) { // 05:00 Nairobi
		t.Fatal("not due before the hour")
	}
	if !due(c, time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC), loc) {
		t.Fatal("due after the hour")
	}
	last := time.Date(2026, 10, 1, 3, 30, 0, 0, time.UTC)
	c.LastSyncAt = &last
	if due(c, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), loc) {
		t.Fatal("ran today already")
	}
}

// Gmail lists newest first, so a payment confirmation is often stored before
// the invoice it pays: the invoice (with number and PDF) must stay the record.
func TestPaymentBeforeInvoiceKeepsInvoice(t *testing.T) {
	db := testDB(t)
	setupGoogle(t)
	org := newOrg(t, db, "AUD")
	s := NewService(db, &fxrates.Service{DB: db})
	d := func(s string) *time.Time { x, _ := time.Parse(dayFmt, s); return &x }
	pay, _, err := s.Ingest(org, Incoming{Source: SourceGmail, ExternalID: "p1", Sender: "payments-noreply@google.com",
		X: Extracted{Kind: KindPayment, VendorKey: "google_workspace", Date: d("2026-09-03"), Currency: "EUR", Total: 43.2, HasTotal: true, Paid: boolp(true), Confidence: 0.8}})
	if err != nil || pay.Status != StatusPending {
		t.Fatalf("%+v %v", pay, err)
	}
	inv, _, err := s.Ingest(org, Incoming{Source: SourceGmail, ExternalID: "i1", Sender: "payments-noreply@google.com",
		X: Extracted{Kind: KindInvoice, VendorKey: "google_workspace", InvoiceNumber: "5671877610", Date: d("2026-09-01"), Currency: "EUR", Total: 43.2, Tax: 7.2, HasTotal: true, HasTax: true, Confidence: 1}})
	if err != nil || inv.Status != StatusPending || inv.PaidDate == nil || inv.PaidDate.Format(dayFmt) != "2026-09-03" {
		t.Fatalf("invoice %+v %v", inv, err)
	}
	var p Item
	db.Where("id = ?", pay.ID).First(&p)
	if p.Status != StatusDuplicate || p.DuplicateOf != inv.ID {
		t.Fatalf("payment should now be a duplicate of the invoice: %+v", p)
	}
}
