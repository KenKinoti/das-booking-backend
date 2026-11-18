package schedsend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// ---------------------------------------------------------------- fixture

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`DROP SCHEMA IF EXISTS schedsend_test CASCADE`)
	boot.Exec(`CREATE SCHEMA schedsend_test`)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=schedsend_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) primary key, name text, email text, phone text, country_code text, currency text, timezone text,
			region_source text, address_country text, created_at timestamptz default now(), deleted_at timestamptz)`,
		`CREATE TABLE users (id varchar(255) primary key, email text, role text, first_name text, last_name text, phone text, organization_id varchar(255),
			is_active boolean default true, created_at timestamptz default now(), deleted_at timestamptz)`,
		`CREATE TABLE customers (id varchar(255) primary key, organization_id varchar(255), first_name text, last_name text, email text, phone text,
			company_name text, country_code varchar(2), is_active boolean default true, created_at timestamptz default now(), deleted_at timestamptz)`,
		`CREATE TABLE billing_invoices (id varchar(64) primary key, organization_id varchar(255), doc_type varchar(20) default 'invoice', status varchar(20),
			number varchar(64), customer_id varchar(255), client_name text, client_email text, currency varchar(3), total numeric default 0,
			balance_due numeric default 0, due_date date, public_token varchar(64), created_at timestamptz default now(), deleted_at timestamptz)`,
		`CREATE TABLE billing_payments (id varchar(64) primary key, organization_id varchar(255), invoice_id varchar(64), amount numeric)`,
		`CREATE TABLE client_subscriptions (id varchar(64) primary key, organization_id varchar(255), customer_id varchar(255), status varchar(16))`,
		`CREATE TABLE audit_logs (id varchar(36) primary key, organization_id varchar(36), user_id varchar(36), action varchar(100), entity_type varchar(50),
			entity_id varchar(64), old_values jsonb, new_values jsonb, ip_address varchar(45), user_agent varchar(500), created_at timestamptz)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []func(*gorm.DB) error{notify.Migrate, wa.Migrate, Migrate} {
		if err := m(db); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		SetNow(nil)
		SetClockOffset(0)
	})
	return db
}

// fakes ------------------------------------------------------------

type invCall struct {
	Who  Actor
	ID   string
	Body map[string]any
}

type fakeInvoices struct {
	mu    sync.Mutex
	calls []invCall
	fail  error
}

func (f *fakeInvoices) Send(_ context.Context, who Actor, id string, body map[string]any) (InvoiceResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, invCall{who, id, body})
	if f.fail != nil {
		return InvoiceResult{}, f.fail
	}
	return InvoiceResult{Emailed: true, LogID: fmt.Sprintf("log-%d", len(f.calls)), To: fmt.Sprint(body["to"]), Subject: "Invoice"}, nil
}

type sent struct {
	To, Kind, Text, Template string
	Params                   []string
}

type fakeWA struct {
	mu       sync.Mutex
	open     map[string]bool
	sends    []sent
	failText error
	failTpl  map[string][]error // per number: consumed one per template send
	notSetUp bool
}

func (f *fakeWA) Resolve(org string) (WAAccount, error) {
	if f.notSetUp {
		return nil, wa.ErrNotConfigured
	}
	return f, nil
}
func (f *fakeWA) WindowOpen(org, phone string, now time.Time) bool { return f.open[phone] }
func (f *fakeWA) SendText(_ context.Context, to, body string, _ bool) (*wa.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failText != nil {
		return nil, f.failText
	}
	f.sends = append(f.sends, sent{To: to, Kind: "text", Text: body})
	return &wa.Result{MessageID: fmt.Sprintf("wamid.%d", len(f.sends))}, nil
}
func (f *fakeWA) SendTemplate(_ context.Context, to string, t wa.Template) (*wa.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.failTpl[to]; len(l) > 0 {
		err := l[0]
		f.failTpl[to] = l[1:]
		if err != nil {
			return nil, err
		}
	}
	f.sends = append(f.sends, sent{To: to, Kind: "template", Template: t.Name, Params: t.BodyParams})
	return &wa.Result{MessageID: fmt.Sprintf("wamid.%d", len(f.sends))}, nil
}

type world struct {
	t       *testing.T
	db      *gorm.DB
	s       *Service
	inv     *fakeInvoices
	wa      *fakeWA
	mails   []mail.Message
	sms     []sent
	smsFail []error
	mu      sync.Mutex
	now     time.Time
	// organisations and people
	org, org2                               string
	owner, manager, staff, readOnly, owner2 Actor
}

func (w *world) setNow(t time.Time) {
	w.now = t
	SetNow(func() time.Time { return w.now })
}

func newWorld(t *testing.T) *world {
	db := testDB(t)
	w := &world{t: t, db: db, inv: &fakeInvoices{}, wa: &fakeWA{open: map[string]bool{}}}
	w.s = New(db)
	w.s.Secret = []byte("test-secret")
	w.s.APIURL = "https://api.dasyin.test"
	w.s.AppURL = "https://app.dasyin.test"
	w.s.Invoices = w.inv
	w.s.WA = w.wa
	w.s.Gap = -1
	w.s.Mail = func(_ context.Context, org string, m mail.Message) (string, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.mails = append(w.mails, m)
		return uuid.NewString() + "@dasyin.test", nil
	}
	w.s.SMS = func(_ context.Context, org, to, text, ref string) (SMSResult, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if len(w.smsFail) > 0 {
			err := w.smsFail[0]
			w.smsFail = w.smsFail[1:]
			if err != nil {
				return SMSResult{}, err
			}
		}
		w.sms = append(w.sms, sent{To: to, Text: text})
		return SMSResult{Provider: ProviderAT, ID: "ATXid_" + uuid.NewString()[:8]}, nil
	}
	w.setNow(time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)) // Wed 23:00 in Nairobi
	w.org = w.mkOrg("Savanna Web Studio", "KE", "Africa/Nairobi")
	w.org2 = w.mkOrg("Coral Reef Lodge", "KE", "Africa/Nairobi")
	w.owner = w.mkUser(w.org, "owner", "Amani", "+254711000001")
	w.manager = w.mkUser(w.org, "manager", "Neema", "+254711000002")
	w.staff = w.mkUser(w.org, "staff", "Otieno", "0711000003")
	w.readOnly = w.mkUser(w.org, "read_only", "Zawadi", "")
	w.owner2 = w.mkUser(w.org2, "owner", "Juma", "")
	return w
}

func (w *world) mkOrg(name, cc, tz string) string {
	id := uuid.NewString()
	w.db.Exec(`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES (?, ?, ?, 'KES', ?, 'user')`, id, name, cc, tz)
	return id
}

func (w *world) mkUser(org, role, first, phone string) Actor {
	id := uuid.NewString()
	email := strings.ToLower(first) + "@" + org[:8] + ".test"
	w.db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, phone, organization_id) VALUES (?, ?, ?, ?, 'Test', ?, ?)`, id, email, role, first, phone, org)
	return Actor{Org: org, User: id, Role: role, Name: first + " Test", Email: email}
}

func (w *world) mkCustomer(org, first, email, phone, company string) string {
	id := uuid.NewString()
	w.db.Exec(`INSERT INTO customers (id, organization_id, first_name, last_name, email, phone, company_name, country_code) VALUES (?, ?, ?, 'Kamau', ?, ?, ?, 'KE')`, id, org, first, email, phone, company)
	return id
}

func (w *world) mkInvoice(org, customer, status string, total, balance float64, due string) string {
	id := uuid.NewString()
	var n int64
	w.db.Raw(`SELECT COUNT(*) FROM billing_invoices`).Scan(&n)
	var cname, cemail string
	w.db.Raw(`SELECT COALESCE(first_name,''), COALESCE(email,'') FROM customers WHERE id = ?`, customer).Row().Scan(&cname, &cemail)
	w.db.Exec(`INSERT INTO billing_invoices (id, organization_id, doc_type, status, number, customer_id, client_name, client_email, currency, total, balance_due, due_date, public_token)
		VALUES (?, ?, 'invoice', ?, ?, ?, ?, ?, 'KES', ?, ?, ?, ?)`, id, org, status, fmt.Sprintf("INV-%d", 1001+n), customer, cname, cemail, total, balance, due, uuid.NewString())
	return id
}

func (w *world) run() RunResult {
	w.t.Helper()
	r, err := w.s.RunOnce(context.Background(), 5*time.Second)
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *world) create(a Actor, in Input) *Message {
	w.t.Helper()
	m, err := w.s.Create(a, in)
	if err != nil {
		w.t.Fatalf("create: %v", err)
	}
	return m
}

func (w *world) msg(id string) Message {
	var m Message
	w.db.Where("id = ?", id).Take(&m)
	return m
}

func (w *world) deliveries(id string) []Delivery {
	var d []Delivery
	w.db.Where("message_id = ?", id).Order("created_at, name, channel").Find(&d)
	return d
}

func (w *world) runs(id string) []Run {
	var r []Run
	w.db.Where("message_id = ?", id).Order("occurrence_at").Find(&r)
	return r
}

func (w *world) notes(user, typ string) int {
	var n int64
	w.db.Model(&notify.Notification{}).Where("user_id = ? AND type = ?", user, typ).Count(&n)
	return int(n)
}

// nairobi 08:00 tomorrow (Thu 19 Nov 2026) = 05:00Z.
const tomorrow8 = "2026-11-19T08:00"

var at8 = time.Date(2026, 11, 19, 5, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------- tests

func TestInvoiceEmailSentOnceAtTheTime(t *testing.T) {
	w := newWorld(t)
	cu := w.mkCustomer(w.org, "Wanjiru", "wanjiru@savanna.test", "0712345678", "Savanna Bistro")
	inv := w.mkInvoice(w.org, cu, "draft", 13920, 13920, "2026-12-02")
	m := w.create(w.owner, Input{Kind: KindInvoice, InvoiceID: inv, LocalSendAt: tomorrow8, Timezone: "Africa/Nairobi",
		InvoiceSend: &InvoiceSend{To: "wanjiru@savanna.test", CC: []string{"accounts@savanna.test"}, Subject: "Invoice {{number}}", AcceptWarnings: true}})
	if !m.NextRunAt.Equal(at8) || m.NextLocal != tomorrow8 || m.Title != "Invoice INV-1001 to Wanjiru" {
		t.Fatalf("schedule: %v %s %q", m.NextRunAt, m.NextLocal, m.Title)
	}
	w.setNow(at8.Add(-time.Minute))
	w.run()
	if len(w.inv.calls) != 0 {
		t.Fatal("nothing before 08:00")
	}
	w.setNow(at8.Add(30 * time.Second))
	w.run()
	if len(w.inv.calls) != 1 {
		t.Fatalf("one send, got %d", len(w.inv.calls))
	}
	c := w.inv.calls[0]
	if c.ID != inv || c.Who.User != w.owner.User || c.Body["kind"] != "invoice" || c.Body["mode"] != "email" || c.Body["to"] != "wanjiru@savanna.test" ||
		c.Body["subject"] != "Invoice {{number}}" || c.Body["accept_warnings"] != true {
		t.Fatalf("send body: %+v", c)
	}
	if cc := c.Body["cc"].([]string); len(cc) != 1 || cc[0] != "accounts@savanna.test" {
		t.Fatalf("cc kept: %v", c.Body["cc"])
	}
	got := w.msg(m.ID)
	d := w.deliveries(m.ID)
	if got.Status != StatusDone || got.LastResult != RunSent || len(d) != 1 || d[0].Status != DelSent || d[0].ProviderID != "log-1" {
		t.Fatalf("after: %s %s %+v", got.Status, got.LastResult, d)
	}
	// Idempotent: later runs and a replay of the same occurrence send nothing.
	w.run()
	run, _ := w.s.runOccurrence(context.Background(), &got, at8, false, &RunResult{})
	if len(w.inv.calls) != 1 || run.Status != RunSent {
		t.Fatalf("idempotent: %d calls, run %s", len(w.inv.calls), run.Status)
	}
}

func TestInvoiceSkipRules(t *testing.T) {
	w := newWorld(t)
	cu := w.mkCustomer(w.org, "Wanjiru", "wanjiru@savanna.test", "0712345678", "")
	voided := w.mkInvoice(w.org, cu, "sent", 1000, 1000, "2026-12-02")
	paid := w.mkInvoice(w.org, cu, "sent", 1000, 1000, "2026-12-02")
	quote := w.mkInvoice(w.org, cu, "sent", 500, 500, "2026-12-02")
	w.db.Exec(`UPDATE billing_invoices SET doc_type = 'quote' WHERE id = ?`, quote)
	deleted := w.mkInvoice(w.org, w.mkCustomer(w.org, "Gone", "gone@x.test", "", ""), "sent", 900, 900, "2026-12-02")
	ids := map[string]string{}
	for k, inv := range map[string]string{"voided": voided, "paid": paid, "quote": quote, "deleted": deleted} {
		ids[k] = w.create(w.owner, Input{Kind: KindInvoice, InvoiceID: inv, LocalSendAt: tomorrow8}).ID
	}
	w.db.Exec(`UPDATE billing_invoices SET status = 'void' WHERE id = ?`, voided)
	w.db.Exec(`UPDATE billing_invoices SET status = 'paid', balance_due = 0 WHERE id = ?`, paid)
	w.db.Exec(`UPDATE billing_invoices SET status = 'accepted' WHERE id = ?`, quote)
	w.db.Exec(`UPDATE customers SET deleted_at = now() WHERE first_name = 'Gone'`)
	w.setNow(at8.Add(time.Minute))
	w.run()
	if len(w.inv.calls) != 0 {
		t.Fatalf("nothing is sent: %d", len(w.inv.calls))
	}
	want := map[string]string{"voided": "voided", "paid": "Already paid", "quote": "already accepted", "deleted": "customer was deleted"}
	for k, id := range ids {
		d := w.deliveries(id)
		if len(d) != 1 || d[0].Status != DelSkipped || !strings.Contains(d[0].Reason, want[k]) || w.msg(id).LastResult != RunSkipped {
			t.Fatalf("%s: %+v", k, d)
		}
	}
	// Creating a scheduled send for a void invoice is refused up front.
	if _, err := w.s.Create(w.owner, Input{Kind: KindInvoice, InvoiceID: voided, LocalSendAt: "2026-11-20T08:00"}); err == nil {
		t.Fatal("void invoice can't be scheduled")
	}
}

func TestRemindersEveryNDaysUntilPaid(t *testing.T) {
	w := newWorld(t)
	cu := w.mkCustomer(w.org, "Baraka", "baraka@x.test", "0712000102", "")
	inv := w.mkInvoice(w.org, cu, "sent", 3480, 3480, "2026-10-15")
	m := w.create(w.owner, Input{Kind: KindReminder, InvoiceID: inv, Channels: []string{ChanEmail, ChanSMS}, LocalSendAt: tomorrow8,
		Repeat: Recurrence{Freq: FreqDaily, Interval: 7, Count: 5}})
	if !m.Repeat.StopWhenPaid || m.SMSBody == "" {
		t.Fatalf("reminder defaults: %+v", m.Repeat)
	}
	w.setNow(at8.Add(time.Minute))
	w.run()
	if len(w.inv.calls) != 1 || w.inv.calls[0].Body["kind"] != "reminder" || len(w.sms) != 1 {
		t.Fatalf("first reminder: %d emails, %d sms", len(w.inv.calls), len(w.sms))
	}
	if w.sms[0].To != "+254712000102" || !strings.Contains(w.sms[0].Text, "INV-1001") || !strings.Contains(w.sms[0].Text, "3,480.00") || !strings.Contains(w.sms[0].Text, "https://app.dasyin.test/i/") {
		t.Fatalf("sms: %+v", w.sms[0])
	}
	got := w.msg(m.ID)
	if got.Status != StatusScheduled || !got.NextRunAt.Equal(at8.AddDate(0, 0, 7)) {
		t.Fatalf("next reminder in 7 days: %s %v", got.Status, got.NextRunAt)
	}
	w.db.Exec(`UPDATE billing_invoices SET status = 'paid', balance_due = 0 WHERE id = ?`, inv)
	w.setNow(at8.AddDate(0, 0, 7).Add(time.Minute))
	w.run()
	got = w.msg(m.ID)
	if len(w.inv.calls) != 1 || len(w.sms) != 1 || got.Status != StatusDone || !strings.Contains(got.EndedReason, "Paid") {
		t.Fatalf("stops when paid: %d %d %s %q", len(w.inv.calls), len(w.sms), got.Status, got.EndedReason)
	}
}

func TestCatchUpAfterDowntime(t *testing.T) {
	w := newWorld(t)
	cu := w.mkCustomer(w.org, "Akinyi", "akinyi@x.test", "0712000101", "")
	in := Input{Kind: KindCustomer, Channels: []string{ChanEmail}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}}, Subject: "Hello {first_name}", Body: "Hi", LocalSendAt: tomorrow8}
	late := w.create(w.owner, in)
	missed := w.create(w.owner, in)
	in.Repeat = Recurrence{Freq: FreqDaily}
	daily := w.create(w.owner, in)
	// Down until 13:00 (5 h late): the first is still sent.
	w.setNow(at8.Add(5 * time.Hour))
	w.db.Model(&Message{}).Where("id IN ?", []string{missed.ID}).Update("status", StatusPaused) // not yet
	w.db.Model(&Message{}).Where("id = ?", daily.ID).Update("status", StatusPaused)
	w.run()
	if w.msg(late.ID).LastResult != RunSent || len(w.mails) != 1 {
		t.Fatalf("5 h late is still sent: %s %d", w.msg(late.ID).LastResult, len(w.mails))
	}
	// 7 h late: missed, the creator is told, nothing is sent.
	w.db.Model(&Message{}).Where("id IN ?", []string{missed.ID, daily.ID}).Update("status", StatusScheduled)
	w.setNow(at8.Add(7 * time.Hour))
	w.run()
	mm := w.msg(missed.ID)
	if mm.Status != StatusDone || mm.LastResult != RunMissed || len(w.mails) != 1 || w.notes(w.owner.User, "schedsend.missed") < 1 {
		t.Fatalf("missed: %s %s mails=%d notes=%d", mm.Status, mm.LastResult, len(w.mails), w.notes(w.owner.User, "schedsend.missed"))
	}
	// The daily one: today's occurrence missed, next is tomorrow 08:00 (no burst).
	dm := w.msg(daily.ID)
	if dm.Status != StatusScheduled || !dm.NextRunAt.Equal(at8.AddDate(0, 0, 1)) || dm.LastResult != RunMissed {
		t.Fatalf("daily after downtime: %s %v %s", dm.Status, dm.NextRunAt, dm.LastResult)
	}
	// Down for 3 days: 3 missed occurrences recorded, then back on schedule.
	w.setNow(at8.AddDate(0, 0, 4).Add(-time.Hour))
	w.run()
	dm = w.msg(daily.ID)
	if r := w.runs(daily.ID); len(r) != 4 || dm.Occurrences != 4 || !dm.NextRunAt.Equal(at8.AddDate(0, 0, 4)) || len(w.mails) != 1 {
		t.Fatalf("3 days down: runs=%d occ=%d next=%v mails=%d", len(r), dm.Occurrences, dm.NextRunAt, len(w.mails))
	}
	w.setNow(at8.AddDate(0, 0, 4).Add(time.Minute))
	w.run()
	if len(w.mails) != 2 {
		t.Fatalf("back on time: %d mails", len(w.mails))
	}
}

func TestOptOutAndUnsubscribe(t *testing.T) {
	w := newWorld(t)
	in1 := w.mkCustomer(w.org, "Akinyi", "akinyi@x.test", "", "")
	out1 := w.mkCustomer(w.org, "Chebet", "chebet@x.test", "", "")
	w.db.Create(&OptOut{OrganizationID: w.org, CustomerID: out1, Source: "staff", CreatedAt: Now()})
	in := Input{Kind: KindCustomer, Channels: []string{ChanEmail}, Marketing: true, Audience: Audience{Mode: AudChosen, CustomerIDs: []string{in1, out1}},
		Subject: "Holiday offer", Body: "Hi {first_name}, 20% off hosting.", LocalSendAt: tomorrow8}
	mk := w.create(w.owner, in)
	in.Marketing = false
	in.Subject = "We're closed on Friday"
	svc := w.create(w.owner, in)
	w.setNow(at8.Add(time.Minute))
	w.run()
	d := w.deliveries(mk.ID)
	var skipped, sentN int
	for _, x := range d {
		if x.Status == DelSkipped && x.Reason == "Opted out of marketing messages" && x.RecipientID == out1 {
			skipped++
		}
		if x.Status == DelSent {
			sentN++
		}
	}
	if skipped != 1 || sentN != 1 {
		t.Fatalf("marketing: %+v", d)
	}
	var offer mail.Message
	for _, m := range w.mails {
		if m.Subject == "Holiday offer" {
			offer = m
		}
	}
	if !strings.Contains(offer.Text, "/api/v1/public/schedsend/unsubscribe?t=") || !strings.Contains(offer.HTML, "Unsubscribe") {
		t.Fatal("marketing email carries an unsubscribe link")
	}
	if n := len(w.deliveries(svc.ID)); n != 2 || w.msg(svc.ID).LastResult != RunSent {
		t.Fatalf("service message ignores the opt-out: %d %s", n, w.msg(svc.ID).LastResult)
	}
	// The link: GET asks, POST unsubscribes.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	(&Handler{S: w.s}).RegisterRoutes(r.Group("/api/v1"), r.Group("/api/v1/public"))
	link := w.s.UnsubscribeURL(w.org, in1)
	u, _ := url.Parse(link)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Unsubscribe") {
		t.Fatalf("GET: %d", rec.Code)
	}
	var n int64
	w.db.Model(&OptOut{}).Where("customer_id = ?", in1).Count(&n)
	if n != 0 {
		t.Fatal("GET must not unsubscribe (mail scanners)")
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, u.RequestURI(), nil))
	w.db.Model(&OptOut{}).Where("organization_id = ? AND customer_id = ? AND source = 'link'", w.org, in1).Count(&n)
	if rec.Code != 200 || n != 1 {
		t.Fatalf("POST: %d %d", rec.Code, n)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/public/schedsend/unsubscribe?t=bad.token.x", nil))
	if rec.Code != 400 {
		t.Fatalf("bad token: %d", rec.Code)
	}
}

func TestWhatsAppWindowAndTemplate(t *testing.T) {
	w := newWorld(t)
	a := w.mkCustomer(w.org, "Akinyi", "", "0712000101", "")
	b := w.mkCustomer(w.org, "Baraka", "", "0712000102", "")
	c := w.mkCustomer(w.org, "Chebet", "", "0712000103", "")
	noPhone := w.mkCustomer(w.org, "Dalmas", "d@x.test", "", "")
	w.wa.open["254712000101"] = true
	in := Input{Kind: KindCustomer, Channels: []string{ChanWhatsApp}, Audience: Audience{Mode: AudChosen, CustomerIDs: []string{a, b, c, noPhone}},
		SMSBody: "Hi {first_name}, we're closed on Friday.", WhatsApp: WhatsAppPlan{Template: "dasyin_update", Language: "en", Params: []string{"{first_name}", "we're closed on Friday"}},
		LocalSendAt: tomorrow8}
	m := w.create(w.manager, in)
	// Chebet's template send hits a rate limit once (retried later).
	w.wa.failTpl = map[string][]error{"254712000103": {&wa.Error{Code: 130429, Message: "rate limit"}}}
	w.setNow(at8.Add(time.Minute))
	w.run()
	byTo := map[string]sent{}
	for _, s := range w.wa.sends {
		byTo[s.To] = s
	}
	if s := byTo["254712000101"]; s.Kind != "text" || s.Text != "Hi Akinyi, we're closed on Friday." {
		t.Fatalf("open window → free text: %+v", s)
	}
	if s := byTo["254712000102"]; s.Kind != "template" || s.Template != "dasyin_update" || s.Params[0] != "Baraka" {
		t.Fatalf("no window → template: %+v", s)
	}
	var retry, noPh Delivery
	for _, d := range w.deliveries(m.ID) {
		switch d.RecipientID {
		case c:
			retry = d
		case noPhone:
			noPh = d
		}
	}
	if retry.Status != DelRetry || retry.NextAttemptAt == nil || noPh.Status != DelSkipped || noPh.Reason != "No phone number" {
		t.Fatalf("retry / skip: %+v %+v", retry, noPh)
	}
	if w.runs(m.ID)[0].Status != RunSending {
		t.Fatal("the run stays open until the retry")
	}
	w.setNow(retry.NextAttemptAt.Add(time.Second))
	w.run()
	if s := w.wa.sends[len(w.wa.sends)-1]; s.To != "254712000103" || s.Kind != "template" {
		t.Fatalf("retried: %+v", s)
	}
	r := w.runs(m.ID)[0]
	if r.Status != RunSent || r.Sent != 3 || r.Skipped != 1 {
		t.Fatalf("run: %+v", r)
	}
	// Free text refused (the window closed meanwhile) → falls back to the template.
	w.wa.failText = &wa.Error{Code: 131047, Message: "Re-engagement message"}
	in.Audience = Audience{Mode: AudOne, CustomerIDs: []string{a}}
	in.LocalSendAt = "2026-11-20T08:00"
	m2 := w.create(w.manager, in)
	w.setNow(at8.AddDate(0, 0, 1).Add(time.Minute))
	w.run()
	if d := w.deliveries(m2.ID); len(d) != 1 || d[0].Status != DelSent || d[0].Mode != "template" {
		t.Fatalf("fallback to template: %+v", d)
	}
	// No template and no window → skipped with the reason.
	w.wa.failText = nil
	in.WhatsApp = WhatsAppPlan{}
	in.Audience = Audience{Mode: AudOne, CustomerIDs: []string{b}}
	in.LocalSendAt = "2026-11-21T08:00"
	m3 := w.create(w.manager, in)
	w.setNow(at8.AddDate(0, 0, 2).Add(time.Minute))
	w.run()
	if d := w.deliveries(m3.ID); len(d) != 1 || d[0].Status != DelSkipped || !strings.Contains(d[0].Reason, "24 hours") {
		t.Fatalf("no template: %+v", d)
	}
	// Delivery statuses from the webhook move the delivery along.
	w.s.onWhatsAppStatus(wa.StatusEvent{MessageID: byTo["254712000102"].Text + "wamid.2", Status: "read"})
	w.s.applyStatus(ChanWhatsApp, "wamid.2", "read", "", Now())
	var d2 Delivery
	w.db.Where("provider_id = ?", "wamid.2").Take(&d2)
	if d2.Status != DelRead || d2.ReadAt == nil || d2.DeliveredAt == nil {
		t.Fatalf("read receipt: %+v", d2)
	}
}

func TestFailuresRetryThenNotifyCreator(t *testing.T) {
	w := newWorld(t)
	cu := w.mkCustomer(w.org, "Baraka", "", "0712000102", "")
	temp := &SMSError{Msg: "Africa's Talking had a temporary problem; it will be retried", Retryable: true}
	w.smsFail = []error{temp, temp, temp, temp}
	m := w.create(w.owner, Input{Kind: KindCustomer, Channels: []string{ChanSMS}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}}, SMSBody: "Hi {first_name}", LocalSendAt: tomorrow8})
	w.setNow(at8.Add(time.Minute))
	for i := 0; i < 6; i++ {
		w.run()
		w.setNow(Now().Add(31 * time.Minute))
	}
	d := w.deliveries(m.ID)
	if len(d) != 1 || d[0].Status != DelFailed || d[0].Attempts != MaxAttempts || len(w.sms) != 0 {
		t.Fatalf("gives up after %d attempts: %+v", MaxAttempts, d)
	}
	if w.msg(m.ID).LastResult != RunFailed || w.notes(w.owner.User, "schedsend.failed") != 1 {
		t.Fatalf("creator told once: %s %d", w.msg(m.ID).LastResult, w.notes(w.owner.User, "schedsend.failed"))
	}
	// A permanent error is not retried.
	w.smsFail = []error{&SMSError{Msg: "This isn't a valid phone number"}}
	m2 := w.create(w.owner, Input{Kind: KindCustomer, Channels: []string{ChanSMS}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}}, SMSBody: "Hi", LocalSendAt: FormatLocal(Now().Add(time.Hour), "Africa/Nairobi")})
	w.setNow(Now().Add(2 * time.Hour))
	w.run()
	if d := w.deliveries(m2.ID); d[0].Status != DelFailed || d[0].Attempts != 1 || d[0].Reason != "This isn't a valid phone number" {
		t.Fatalf("permanent: %+v", d)
	}
}

func TestStaleClaimIsNotResent(t *testing.T) {
	w := newWorld(t)
	cu := w.mkCustomer(w.org, "Akinyi", "akinyi@x.test", "", "")
	m := w.create(w.owner, Input{Kind: KindCustomer, Channels: []string{ChanEmail}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}}, Subject: "Hi", Body: "Hello", LocalSendAt: tomorrow8})
	// Simulate a crash: the run and a claimed delivery exist, the message is "running".
	occ := at8
	run := Run{ID: uuid.NewString(), MessageID: m.ID, OrganizationID: w.org, OccurrenceAt: occ, Status: RunSending, StartedAt: occ}
	w.db.Create(&run)
	claimed := occ.Add(time.Minute)
	w.db.Create(&Delivery{ID: uuid.NewString(), RunID: run.ID, MessageID: m.ID, OrganizationID: w.org, OccurrenceAt: occ, RecipientKey: "customer:" + cu,
		Channel: ChanEmail, RecipientType: "customer", RecipientID: cu, Address: "akinyi@x.test", Status: DelSending, ClaimedAt: &claimed, Attempts: 1})
	w.db.Model(&Message{}).Where("id = ?", m.ID).Updates(map[string]any{"status": StatusRunning, "claimed_at": claimed})
	w.setNow(occ.Add(20 * time.Minute))
	w.run() // releases both claims
	w.run() // resumes the run
	d := w.deliveries(m.ID)
	if len(w.mails) != 0 || len(d) != 1 || d[0].Status != DelFailed || !strings.Contains(d[0].Reason, "not sent again") {
		t.Fatalf("at most once: mails=%d %+v", len(w.mails), d)
	}
	if got := w.msg(m.ID); got.Status != StatusDone {
		t.Fatalf("message finished: %s", got.Status)
	}
}

func TestTeamMessageInAppAndEmail(t *testing.T) {
	w := newWorld(t)
	m := w.create(w.manager, Input{Kind: KindTeam, Channels: []string{ChanEmail, ChanWhatsApp}, Audience: Audience{Mode: AudStaff, UserIDs: []string{w.staff.User, w.owner.User}},
		Subject: "Stock count Saturday", Body: "Hi {first_name}, stock count at 9:00.", LocalSendAt: tomorrow8})
	if m.Channels[0] != ChanInApp {
		t.Fatalf("in-app always first: %v", m.Channels)
	}
	w.setNow(at8.Add(time.Minute))
	w.run()
	var n int64
	w.db.Model(&notify.Notification{}).Where("type = 'message.team' AND title = 'Stock count Saturday' AND user_id IN ?", []string{w.staff.User, w.owner.User}).Count(&n)
	if n != 2 || len(w.mails) != 2 {
		t.Fatalf("in-app %d, emails %d", n, len(w.mails))
	}
	var bodyOK bool
	w.db.Raw(`SELECT COUNT(*) > 0 FROM notifications WHERE body = 'Hi Otieno, stock count at 9:00.'`).Scan(&bodyOK)
	if !bodyOK {
		t.Fatal("merge fields per person")
	}
	// WhatsApp to users.phone with the organisation's number (no window, no template → skipped).
	var wa []Delivery
	w.db.Where("message_id = ? AND channel = ?", m.ID, ChanWhatsApp).Find(&wa)
	if len(wa) != 2 {
		t.Fatalf("whatsapp rows: %+v", wa)
	}
	for _, d := range wa {
		if d.Status != DelSkipped {
			t.Fatalf("no window, no template: %+v", d)
		}
		if d.RecipientID == w.staff.User && d.Address != "+254711000003" {
			t.Fatalf("phone normalised with the org country: %s", d.Address)
		}
	}
}

func TestPermissionsAndIsolation(t *testing.T) {
	w := newWorld(t)
	c1 := w.mkCustomer(w.org, "Akinyi", "a@x.test", "", "")
	c2 := w.mkCustomer(w.org, "Baraka", "b@x.test", "", "")
	other := w.mkCustomer(w.org2, "Juma", "j@x.test", "", "")
	inv := w.mkInvoice(w.org, c1, "sent", 100, 100, "2026-12-01")
	one := Input{Kind: KindCustomer, Channels: []string{ChanEmail}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{c1}}, Subject: "Hi", Body: "Hello", LocalSendAt: tomorrow8}
	chosen := one
	chosen.Audience = Audience{Mode: AudChosen, CustomerIDs: []string{c1, c2}}
	group := one
	group.Audience = Audience{Mode: AudGroup, Group: GroupAllEmail}
	team := Input{Kind: KindTeam, Audience: Audience{Mode: AudStaff}, Subject: "Hi", Body: "Hello", LocalSendAt: tomorrow8}
	forb := func(a Actor, in Input, want bool) {
		t.Helper()
		_, err := w.s.Create(a, in)
		var e *Error
		got := errors.As(err, &e) && e.Status == 403
		if got != want {
			t.Fatalf("%s %s/%s: forbidden=%v want %v (%v)", a.Role, in.Kind, in.Audience.Mode, got, want, err)
		}
	}
	forb(w.staff, one, false)
	forb(w.staff, chosen, true)
	forb(w.staff, group, true)
	forb(w.staff, team, true)
	forb(w.manager, group, false)
	forb(w.manager, team, false)
	forb(w.readOnly, one, true)
	// Invoices follow the role's "send invoices" permission.
	w.s.Can = func(org, user, role, module, act string) bool { return role != "staff" }
	forb(w.staff, Input{Kind: KindInvoice, InvoiceID: inv, LocalSendAt: tomorrow8}, true)
	forb(w.manager, Input{Kind: KindInvoice, InvoiceID: inv, LocalSendAt: tomorrow8}, false)
	w.s.Can = nil
	// Another organisation's customers and invoices can't be used.
	bad := one
	bad.Audience = Audience{Mode: AudOne, CustomerIDs: []string{other}}
	if _, err := w.s.Create(w.owner, bad); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("other org's customer: %v", err)
	}
	if _, err := w.s.Create(w.owner2, Input{Kind: KindInvoice, InvoiceID: inv, LocalSendAt: tomorrow8}); err == nil {
		t.Fatal("other org's invoice")
	}
	mine := w.create(w.staff, one)
	mgr := w.create(w.manager, group)
	// Staff see their own; managers see all; the other organisation sees none.
	var own, all int64
	w.db.Model(&Message{}).Where("organization_id = ? AND created_by = ?", w.org, w.staff.User).Count(&own)
	w.db.Model(&Message{}).Where("organization_id = ?", w.org).Count(&all)
	l, _, _ := w.s.List(w.staff, ListFilter{Tab: "upcoming"})
	if len(l) != int(own) || own < 2 || all <= own {
		t.Fatalf("staff list: %d (own %d, all %d)", len(l), own, all)
	}
	for _, x := range l {
		if x.CreatedBy != w.staff.User {
			t.Fatal("staff only see their own")
		}
	}
	_ = mine
	l, c, _ := w.s.List(w.manager, ListFilter{Tab: "upcoming"})
	if len(l) != int(all) || c.Upcoming != all {
		t.Fatalf("manager list: %d %+v", len(l), c)
	}
	l, _, _ = w.s.List(w.owner2, ListFilter{Tab: "upcoming"})
	if len(l) != 0 {
		t.Fatal("other org sees nothing")
	}
	if _, err := w.s.Get(w.org2, mgr.ID); err == nil {
		t.Fatal("get across orgs")
	}
	if _, err := w.s.SetStatus(w.owner2, mgr.ID, "cancel"); err == nil {
		t.Fatal("cancel across orgs")
	}
	if _, err := w.s.SetStatus(w.staff, mgr.ID, "pause"); err == nil {
		t.Fatal("staff can't pause a manager's message")
	}
	if _, err := w.s.GetDetail(w.staff, mgr.ID, ""); err == nil {
		t.Fatal("staff can't open a manager's message")
	}
	// Group recipients come from the organisation only.
	r, _ := w.s.Customers(w.org, Audience{Mode: AudGroup, Group: GroupAllEmail})
	if len(r) != 2 {
		t.Fatalf("group: %d", len(r))
	}
}

func TestGroupsAndStatement(t *testing.T) {
	w := newWorld(t)
	biz := w.mkCustomer(w.org, "Akinyi", "a@x.test", "0712000101", "Akinyi Studio")
	ind := w.mkCustomer(w.org, "Baraka", "b@x.test", "", "")
	au := w.mkCustomer(w.org, "Chloe", "", "0412000000", "")
	w.db.Exec(`UPDATE customers SET country_code = 'AU' WHERE id = ?`, au)
	w.mkInvoice(w.org, biz, "sent", 1000, 1000, "2026-10-01") // overdue
	w.mkInvoice(w.org, biz, "partial", 2000, 500, "2026-12-30")
	w.mkInvoice(w.org, ind, "sent", 700, 700, "2026-12-30") // open, not overdue
	w.db.Exec(`INSERT INTO client_subscriptions (id, organization_id, customer_id, status) VALUES ('s1', ?, ?, 'active'), ('s2', ?, ?, 'cancelled')`, w.org, ind, w.org, biz)
	count := func(a Audience) int {
		t.Helper()
		r, err := w.s.Customers(w.org, a)
		if err != nil {
			t.Fatal(err)
		}
		return len(r)
	}
	if count(Audience{Mode: AudGroup, Group: GroupAllEmail}) != 2 || count(Audience{Mode: AudGroup, Group: GroupAllPhone}) != 2 ||
		count(Audience{Mode: AudGroup, Group: GroupType, GroupValue: "business"}) != 1 || count(Audience{Mode: AudGroup, Group: GroupType, GroupValue: "individual"}) != 2 ||
		count(Audience{Mode: AudGroup, Group: GroupCountry, GroupValue: "au"}) != 1 || count(Audience{Mode: AudGroup, Group: GroupOverdue}) != 1 ||
		count(Audience{Mode: AudGroup, Group: GroupOpenBalance}) != 2 || count(Audience{Mode: AudGroup, Group: GroupSubscriptions}) != 1 {
		t.Fatal("group sizes")
	}
	m := w.create(w.owner, Input{Kind: KindStatement, Channels: []string{ChanEmail}, Audience: Audience{Mode: AudGroup, Group: GroupAllEmail, SkipZeroBalance: true},
		LocalSendAt: "2026-12-01T08:00", Repeat: Recurrence{Freq: FreqMonthly, MonthDay: 1}})
	w.db.Exec(`UPDATE billing_invoices SET status = 'paid', balance_due = 0 WHERE customer_id = ?`, ind)
	w.setNow(time.Date(2026, 12, 1, 5, 1, 0, 0, time.UTC))
	w.run()
	if len(w.mails) != 1 || !strings.Contains(w.mails[0].Subject, "Statement of account — December 2026") {
		t.Fatalf("statement: %d %v", len(w.mails), w.mails)
	}
	if !strings.Contains(w.mails[0].Text, "INV-1001") || !strings.Contains(w.mails[0].Text, "1,500.00") {
		t.Fatalf("lists open invoices and the balance: %s", w.mails[0].Text)
	}
	var skip Delivery
	w.db.Where("message_id = ? AND recipient_id = ?", m.ID, ind).Take(&skip)
	if skip.Status != DelSkipped || skip.Reason != "Nothing outstanding" {
		t.Fatalf("zero balance skipped: %+v", skip)
	}
	if got := w.msg(m.ID); !got.NextRunAt.Equal(time.Date(2027, 1, 1, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("next statement: %v", got.NextRunAt)
	}
	w.setNow(time.Date(2027, 1, 1, 5, 2, 0, 0, time.UTC))
	w.run()
	if len(w.runs(m.ID)) != 2 || len(w.mails) != 2 {
		t.Fatalf("second month: runs=%d mails=%d", len(w.runs(m.ID)), len(w.mails))
	}
}

func TestTimezonesAndEditSendNow(t *testing.T) {
	w := newWorld(t)
	cu := w.mkCustomer(w.org, "Akinyi", "a@x.test", "", "")
	in := Input{Kind: KindCustomer, Channels: []string{ChanEmail}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}}, Subject: "Hi", Body: "Hello",
		LocalSendAt: "2026-11-20T08:00", Timezone: "Australia/Sydney"}
	m := w.create(w.owner, in)
	if want := time.Date(2026, 11, 19, 21, 0, 0, 0, time.UTC); !m.NextRunAt.Equal(want) {
		t.Fatalf("Sydney 08:00 (AEDT) = %v, got %v", want, m.NextRunAt)
	}
	// Past times are refused.
	in.LocalSendAt = "2026-11-18T08:00"
	in.Timezone = "Africa/Nairobi"
	if _, err := w.s.Create(w.owner, in); err == nil {
		t.Fatal("a time in the past")
	}
	// Edit the time and the subject; pause / resume.
	in.LocalSendAt, in.Subject = "2026-11-23T09:30", "Hi {first_name}"
	up, err := w.s.Update(w.owner, m.ID, in)
	if err != nil || up.Subject != "Hi {first_name}" || up.NextLocal != "2026-11-23T09:30" || up.Timezone != "Africa/Nairobi" {
		t.Fatalf("update: %v %+v", err, up)
	}
	if p, _ := w.s.SetStatus(w.owner, m.ID, "pause"); p.Status != StatusPaused {
		t.Fatal("pause")
	}
	if p, _ := w.s.SetStatus(w.owner, m.ID, "resume"); p.Status != StatusScheduled {
		t.Fatal("resume")
	}
	// Send now: sent once, the one-off is done.
	if _, err := w.s.SendNow(w.owner, m.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50 && w.msg(m.ID).Status != StatusDone; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if got := w.msg(m.ID); got.Status != StatusDone || len(w.mails) != 1 || w.mails[0].Subject != "Hi Akinyi" {
		t.Fatalf("send now: %s %d", got.Status, len(w.mails))
	}
	if r := w.runs(m.ID); len(r) != 1 || !r[0].Manual {
		t.Fatalf("manual run: %+v", r)
	}
	if _, err := w.s.Update(w.owner, m.ID, in); err == nil {
		t.Fatal("a finished message can't be edited")
	}
	// Duplicate → a paused copy; cancel it.
	cp, err := w.s.Duplicate(w.owner, m.ID)
	if err != nil || cp.Status != StatusPaused || !strings.HasPrefix(cp.Title, "Copy of") {
		t.Fatalf("duplicate: %v %+v", err, cp)
	}
	if c, _ := w.s.SetStatus(w.owner, cp.ID, "cancel"); c.Status != StatusCancelled {
		t.Fatal("cancel")
	}
	w.setNow(time.Date(2026, 12, 30, 0, 0, 0, 0, time.UTC))
	w.run()
	if len(w.mails) != 1 {
		t.Fatal("a cancelled message never sends")
	}
	// Recurring "send now" keeps the schedule.
	in.LocalSendAt, in.Repeat = "2027-01-04T08:00", Recurrence{Freq: FreqWeekly}
	rec := w.create(w.owner, in)
	next := *rec.NextRunAt
	w.s.SendNow(w.owner, rec.ID)
	for i := 0; i < 50 && w.msg(rec.ID).Status != StatusScheduled; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if got := w.msg(rec.ID); got.Status != StatusScheduled || !got.NextRunAt.Equal(next) || len(w.mails) != 2 {
		t.Fatalf("recurring send now: %s %v %d", got.Status, got.NextRunAt, len(w.mails))
	}
}

func TestSMSDeliveryReportsAndHandlers(t *testing.T) {
	w := newWorld(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	auth := r.Group("/api/v1")
	auth.Use(func(c *gin.Context) {
		c.Set("org_id", w.owner.Org)
		c.Set("user_id", w.owner.User)
		c.Set("user_role", w.owner.Role)
		c.Set("user_email", w.owner.Email)
	})
	(&Handler{S: w.s}).RegisterRoutes(auth, r.Group("/api/v1/public"))
	cu := w.mkCustomer(w.org, "Baraka", "", "0712000102", "")
	body := `{"kind":"customer","channels":["sms"],"audience":{"mode":"one","customer_ids":["` + cu + `"]},"sms_body":"Hi {first_name}","local_send_at":"` + tomorrow8 + `","timezone":"Africa/Nairobi"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedsend/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var n int64
	w.db.Raw(`SELECT COUNT(*) FROM audit_logs WHERE action = 'schedsend.create' AND organization_id = ?`, w.org).Scan(&n)
	if n != 1 {
		t.Fatal("audit log entry for create")
	}
	w.setNow(at8.Add(time.Minute))
	w.run()
	var d Delivery
	w.db.Where("channel = ?", ChanSMS).Take(&d)
	form := url.Values{"id": {d.ProviderID}, "status": {"Failed"}, "phoneNumber": {"+254712000102"}, "failureReason": {"AbsentSubscriber"}}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/public/sms/africastalking/delivery", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ServeHTTP(rec, req)
	w.db.Where("id = ?", d.ID).Take(&d)
	if rec.Code != 200 || d.Status != DelFailed || !strings.Contains(d.Reason, "phone is off") {
		t.Fatalf("AT failed report: %d %+v", rec.Code, d)
	}
	if w.notes(w.owner.User, "schedsend.failed") != 1 {
		t.Fatal("a failed delivery report tells the creator")
	}
	// Twilio callbacks must be signed with the organisation's auth token.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/public/sms/twilio/status", strings.NewReader(url.Values{"MessageSid": {d.ProviderID}, "MessageStatus": {"delivered"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("unsigned Twilio callback: %d", rec.Code)
	}
	// Test hooks are off unless enabled.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/schedsend/test-clock", strings.NewReader(`{"advance":"1h"}`)))
	if rec.Code != 404 {
		t.Fatalf("test clock without SCHEDSEND_TEST_CLOCK: %d", rec.Code)
	}
}
