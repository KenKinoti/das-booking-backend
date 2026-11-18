package brief

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/insights"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/meetings"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/projects2"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/smallbiz"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/subscriptions"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// DB tests run in their own schema when TEST_DATABASE_URL is set.

var schemaN int64

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	schema := fmt.Sprintf("brief_test_%d_%d", os.Getpid(), atomic.AddInt64(&schemaN, 1))
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	boot.Exec(`CREATE SCHEMA ` + schema)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) primary key, name text, email text, phone text, address_street text, address_suburb text, address_state text,
			address_postcode text, country_code text, currency text, timezone text, region_source text, address_country text, business_type text, hours_timezone text)`,
		`CREATE TABLE users (id varchar(255) primary key, email text, role text, first_name text, last_name text, phone text, organization_id varchar(255),
			is_active boolean default true, created_at timestamptz default now(), updated_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE customers (id varchar(255) primary key, first_name text not null default '', last_name text not null default '', email text, phone text not null default '',
			company_name text, organization_id varchar(255) not null, is_active boolean default true, created_at timestamptz, updated_at timestamptz, deleted_at timestamptz,
			address_street text, address_suburb text, address_state text, address_postcode text, address_country text, country_code text, currency text, notes text,
			tax_number text, website text, payment_terms_days bigint, source text, external_id text)`,
		`CREATE TABLE services (id varchar(255) primary key, organization_id varchar(255), name text, deleted_at timestamptz)`,
		`CREATE TABLE bookings (id varchar(255) primary key, customer_id varchar(255), organization_id varchar(255), staff_id varchar(255), start_time timestamptz,
			end_time timestamptz, status text default 'scheduled', deleted_at timestamptz)`,
		`CREATE TABLE booking_services (booking_id varchar(255), service_id varchar(255))`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	all := append([]interface{}{}, Models()...)
	all = append(all, &wa.MessageLog{}, &wa.Window{}, &models.IntegrationSettings{})
	all = append(all, projects2.Models()...)
	all = append(all, recurbills.Models()...)
	all = append(all, &finance.ChartOfAccount{}, &finance.JournalEntry{}, &finance.JournalEntryLine{}, &finance.Bill{}, &finance.BillLineItem{},
		&finance.Vendor{}, &finance.Payment{}, &finance.BankAccount{}, &finance.BankTransaction{}, &finance.GeneralLedger{}, &finance.AccountBalance{}, &finance.AuditTrail{},
		&fxrates.ManualRate{}, &notify.Notification{}, &mail.SendStatus{})
	all = append(all, expenses.Models()...)
	all = append(all, subscriptions.Models()...)
	all = append(all, invoicing.Models()...)
	all = append(all, emailprefs.Models()...)
	all = append(all, smallbiz.Models()...)
	if err := db.AutoMigrate(all...); err != nil {
		t.Fatal(err)
	}
	if err := meetings.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := insights.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

type fixture struct {
	t        *testing.T
	db       *gorm.DB
	s        *Service
	org      string
	platform string
	owner    string // Ann, admin, phone 0712 345 678
	admin2   string // Ben, admin
	staff    string // Sam, care_worker
	mu       sync.Mutex
	mails    []mail.Message
	clock    time.Time
	nairobi  *time.Location
}

func (f *fixture) at(s string) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.clock = t
	f.mu.Unlock()
}

func (f *fixture) sent() []mail.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mail.Message{}, f.mails...)
}

func (f *fixture) mailsTo(addr string) []mail.Message {
	out := []mail.Message{}
	for _, m := range f.sent() {
		for _, a := range m.To {
			if strings.EqualFold(a, addr) {
				out = append(out, m)
			}
		}
	}
	return out
}

// staffAccess: the general business type's "Staff member" (works across modules, base staff).
func access(org, user, role string) (industry.Access, error) {
	if role == "care_worker" {
		return industry.Access{Role: &industry.OrgRole{Base: industry.BaseStaff, Scope: industry.ScopeAll, Perms: industry.Perms{"*": {"view", "create", "edit"}}}}, nil
	}
	if role == "super_admin" {
		return industry.Access{SuperAdmin: true}, nil
	}
	return industry.Access{Role: &industry.OrgRole{Base: industry.BaseAdmin}}, nil
}

func setup(t *testing.T) *fixture {
	db := testDB(t)
	f := &fixture{t: t, db: db, org: uuid.NewString(), platform: uuid.NewString(), owner: uuid.NewString(), admin2: uuid.NewString(), staff: uuid.NewString()}
	f.nairobi, _ = time.LoadLocation("Africa/Nairobi")
	f.at("2026-11-19T04:00:00Z")
	// A Kenyan business whose saved time zone is wrong (Australia/Adelaide).
	db.Exec(`INSERT INTO organizations (id, name, email, country_code, currency, timezone, region_source) VALUES (?, 'Kilima Web Studio', 'hello@kilima.example', 'KE', 'KES', 'Australia/Adelaide', 'user'),
		(?, 'Platform HQ', 'ops@platform.example', 'KE', 'KES', 'Africa/Nairobi', 'user')`, f.org, f.platform)
	db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, phone, organization_id, created_at) VALUES
		(?, 'ann@kilima.example', 'admin', 'Ann', 'Wambui', '0712 345 678', ?, now() - interval '3 days'),
		(?, 'ben@kilima.example', 'admin', 'Ben', 'Otieno', '', ?, now() - interval '2 days'),
		(?, 'sam@kilima.example', 'care_worker', 'Sam', 'Kariuki', '0722 000 111', ?, now() - interval '1 day'),
		(?, 'root@platform.example', 'super_admin', 'Pat', 'Root', '', ?, now() - interval '30 days')`,
		f.owner, f.org, f.admin2, f.org, f.staff, f.org, uuid.NewString(), f.platform)
	f.s = New(db, nil)
	f.s.Now = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.clock }
	f.s.AppURL = "https://app.example"
	f.s.Access = access
	f.s.SendMail = func(_ context.Context, _ string, m mail.Message) error {
		f.mu.Lock()
		f.mails = append(f.mails, m)
		f.mu.Unlock()
		return nil
	}
	return f
}

func (f *fixture) enable(user string, mut func(*SettingsInput)) Settings {
	f.t.Helper()
	v, err := f.s.Viewer(user)
	if err != nil {
		f.t.Fatal(err)
	}
	on := true
	in := SettingsInput{Enabled: &on}
	if mut != nil {
		mut(&in)
	}
	st, err := f.s.SaveSettings(v, in)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

// onlySections switches every other section off.
func onlySections(keys ...string) func(*SettingsInput) {
	return func(in *SettingsInput) {
		in.Sections = map[string]bool{}
		for _, r := range sections() {
			in.Sections[r.Key] = contains(keys, r.Key)
		}
	}
}

func (f *fixture) snapshots(user string) []Snapshot {
	var out []Snapshot
	f.db.Where("user_id = ?", user).Order("date, kind").Find(&out)
	return out
}

// ---------------------------------------------------------------- fake Meta

type fakeMeta struct {
	mu     sync.Mutex
	sends  []map[string]any
	failN  int // answer the next N sends with a retryable error
	n      int
	closed map[string]bool // numbers whose window is closed even if the DB says open (window race)
}

func (m *fakeMeta) list() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any{}, m.sends...)
}

func newFakeMeta(t *testing.T) *fakeMeta {
	m := &fakeMeta{closed: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer platform-token" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"Invalid OAuth access token","code":190}}`)
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.failN > 0 {
				m.failN--
				w.WriteHeader(429)
				io.WriteString(w, `{"error":{"message":"Rate limit hit","code":130429}}`)
				return
			}
			if b["type"] == "text" && m.closed[fmt.Sprint(b["to"])] {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":{"message":"Re-engagement message","code":131047}}`)
				return
			}
			if tpl, ok := b["template"].(map[string]any); ok {
				comps, _ := tpl["components"].([]any)
				for _, c := range comps {
					cm := c.(map[string]any)
					ps, _ := cm["parameters"].([]any)
					for _, p := range ps {
						txt := fmt.Sprint(p.(map[string]any)["text"])
						if strings.ContainsAny(txt, "\n\t") || strings.Contains(txt, "     ") {
							w.WriteHeader(400)
							io.WriteString(w, `{"error":{"message":"Invalid parameter","code":132018}}`)
							return
						}
					}
				}
			}
			m.sends = append(m.sends, b)
			m.n++
			io.WriteString(w, fmt.Sprintf(`{"messaging_product":"whatsapp","contacts":[{"wa_id":"%v"}],"messages":[{"id":"wamid.T%d"}]}`, b["to"], m.n))
			return
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/message_templates") {
			io.WriteString(w, `{"data":[{"name":"dasyin_morning_brief","status":"APPROVED","language":"en","category":"UTILITY","components":[{"type":"BODY","text":"Good morning {{1}}, your DASYIN brief for {{2}}: {{3}}"},{"type":"BUTTONS","buttons":[{"type":"URL","url":"https://app.example/brief/{{1}}"}]}]}],"paging":{}}`)
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WHATSAPP_API_BASE", srv.URL)
	return m
}

// platformWhatsApp gives the platform (super admin) organisation a WhatsApp number.
func (f *fixture) platformWhatsApp() {
	row := models.IntegrationSettings{ID: uuid.NewString(), OrganizationID: f.platform,
		WhatsAppSettings: models.WhatsAppSettings{Enabled: true, AccessToken: "platform-token", PhoneNumberID: "999", BusinessAccountID: "888", APIVersion: "v21.0"}}
	if err := f.db.Omit(clause.Associations).Create(&row).Error; err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.s.SaveConfig("dasyin_morning_brief", "en", "test"); err != nil {
		f.t.Fatal(err)
	}
}

// ---------------------------------------------------------------- tests

func TestOrgZoneCorrectsWrongSavedZone(t *testing.T) {
	f := setup(t)
	if z := OrgZone(f.db, f.org); z != "Africa/Nairobi" {
		t.Fatalf("Kenyan org saved as Australia/Adelaide: %s", z)
	}
	au := uuid.NewString()
	f.db.Exec(`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES (?, 'Wattle Co', 'AU', 'AUD', 'Australia/Adelaide', 'user')`, au)
	if z := OrgZone(f.db, au); z != "Australia/Adelaide" {
		t.Fatalf("Australian org in Adelaide: %s", z)
	}
	st := f.enable(f.owner, onlySections("today"))
	if tz, _ := f.s.zoneOf(st); tz != "Africa/Nairobi" || st.Timezone != "" {
		t.Fatalf("default zone %s (stored %q)", tz, st.Timezone)
	}
}

func TestSchedulerOncePerDayNairobiInAdelaideOrg(t *testing.T) {
	f := setup(t)
	f.at("2026-11-18T12:00:00Z")
	f.enable(f.owner, onlySections("today"))
	// 07:59 Nairobi (04:59 UTC): not yet — although it is already 15:29 in Adelaide.
	f.at("2026-11-19T04:59:00Z")
	if r := f.s.RunDue(context.Background()); len(r.Sent) != 0 || !r.Leader {
		t.Fatalf("early run: %+v", r)
	}
	f.at("2026-11-19T05:00:20Z")
	r := f.s.RunDue(context.Background())
	if len(r.Sent) != 1 {
		t.Fatalf("08:00 Nairobi: %+v", r)
	}
	for i := 0; i < 3; i++ {
		f.at("2026-11-19T05:0" + strconv.Itoa(1+i) + ":00Z")
		if r := f.s.RunDue(context.Background()); len(r.Sent) != 0 {
			t.Fatalf("second run sent again: %+v", r)
		}
	}
	got := f.mailsTo("ann@kilima.example")
	if len(got) != 1 || !strings.HasPrefix(got[0].Subject, "Morning brief · Thu 19 Nov — ") {
		t.Fatalf("emails: %d %v", len(got), got)
	}
	snaps := f.snapshots(f.owner)
	if len(snaps) != 1 || snaps[0].Date != "2026-11-19" || snaps[0].Status != StatusSent || snaps[0].EmailStatus != ChSent || snaps[0].Timezone != "Africa/Nairobi" || snaps[0].Late {
		t.Fatalf("snapshot %+v", snaps)
	}
	// The next morning: one more.
	f.at("2026-11-20T05:00:05Z")
	if r := f.s.RunDue(context.Background()); len(r.Sent) != 1 {
		t.Fatalf("next day: %+v", r)
	}
	if n := len(f.mailsTo("ann@kilima.example")); n != 2 {
		t.Fatalf("%d emails after two days", n)
	}
}

func TestCatchUpWindowAndMissed(t *testing.T) {
	f := setup(t)
	f.at("2026-11-18T12:00:00Z")
	f.enable(f.owner, onlySections("today"))
	f.enable(f.admin2, func(in *SettingsInput) { onlySections("today")(in); tm := "06:00"; in.Time = &tm })
	// The server comes back at 10:55 Nairobi: Ann's 08:00 (2 h 55 late) still goes out, Ben's 06:00 (4 h 55) is missed.
	f.at("2026-11-19T07:55:00Z")
	r := f.s.RunDue(context.Background())
	if len(r.Sent) != 1 || len(r.Missed) != 1 {
		t.Fatalf("catch-up run: %+v", r)
	}
	ann := f.snapshots(f.owner)
	if len(ann) != 1 || !ann[0].Late || ann[0].Status != StatusSent {
		t.Fatalf("Ann %+v", ann)
	}
	ben := f.snapshots(f.admin2)
	if len(ben) != 1 || ben[0].Status != StatusMissed || ben[0].Date != "2026-11-19" || len(f.mailsTo("ben@kilima.example")) != 0 {
		t.Fatalf("Ben %+v", ben)
	}
	// Recorded once.
	if r := f.s.RunDue(context.Background()); len(r.Missed)+len(r.Sent) != 0 {
		t.Fatalf("again: %+v", r)
	}
}

func TestWeekdaysAndSwitchedOnAfterTheTime(t *testing.T) {
	f := setup(t)
	f.at("2026-11-20T12:00:00Z") // Friday
	f.enable(f.owner, func(in *SettingsInput) { onlySections("today")(in); d := DaysWeekdays; in.Days = &d })
	f.at("2026-11-21T05:01:00Z") // Saturday 08:01 Nairobi
	if r := f.s.RunDue(context.Background()); len(r.Sent)+len(r.Missed) != 0 {
		t.Fatalf("Saturday: %+v", r)
	}
	f.at("2026-11-23T05:01:00Z") // Monday
	if r := f.s.RunDue(context.Background()); len(r.Sent) != 1 {
		t.Fatalf("Monday: %+v", r)
	}
	// Ben switches on at 10:00 Nairobi with 08:00: nothing today (not late, not missed), tomorrow yes.
	f.at("2026-11-23T07:00:00Z")
	f.enable(f.admin2, onlySections("today"))
	f.at("2026-11-23T07:01:00Z")
	if r := f.s.RunDue(context.Background()); len(r.Sent)+len(r.Missed) != 0 {
		t.Fatalf("switched on after 08:00: %+v", r)
	}
	f.at("2026-11-24T05:00:30Z")
	if r := f.s.RunDue(context.Background()); len(r.Sent) != 2 {
		t.Fatalf("Tuesday: %+v", r)
	}
}

func seedBills(t *testing.T, f *fixture) {
	t.Helper()
	fin := finance.NewService(f.db)
	v, err := fin.CreateVendor(f.org, finance.VendorInput{Name: "Synergy Wholesale"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(num string, total float64, due string, created time.Time) {
		d, _ := time.Parse("2006-01-02", due)
		b := finance.Bill{ID: uuid.NewString(), OrganizationID: f.org, BillNumber: num, VendorID: v.ID, Reference: "SW-" + num, BillDate: d.AddDate(0, 0, -14),
			DueDate: d, Status: "unpaid", SubTotal: total, Total: total, Currency: "KES", CreatedAt: created, UpdatedAt: created}
		if err := f.db.Omit(clause.Associations).Create(&b).Error; err != nil {
			t.Fatal(err)
		}
		f.db.Create(&finance.BillLineItem{ID: uuid.NewString(), BillID: b.ID, Description: "Hosting " + num, Quantity: 1, UnitPrice: total, LineTotal: total})
	}
	old := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	mk("BILL-1", 20000, "2026-11-22", old)                                           // due Sun (3 days)
	mk("BILL-2", 21200, "2026-11-24", time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)) // due Tue (5 days), new since the last brief
	mk("BILL-3", 5000, "2026-11-14", old)                                            // 5 days overdue
	mk("BILL-4", 9999, "2026-12-15", old)                                            // later: not this week
	// A Synergy invoice waiting in the Expenses inbox, imported overnight.
	f.db.Create(&expenses.Item{ID: uuid.NewString(), OrganizationID: f.org, Source: expenses.SourceGmail, ExternalID: "msg-1", Status: expenses.StatusPending,
		Kind: expenses.KindInvoice, VendorName: "Synergy Wholesale", InvoiceNumber: "INV-88123", Currency: "AUD", Total: 120.5, HomeCurrency: "KES", HomeTotal: 10250,
		CreatedAt: time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC)})
}

func TestBillsSectionAndHeadlineMaths(t *testing.T) {
	f := setup(t)
	seedBills(t, f)
	f.at("2026-11-19T05:00:00Z")
	v, _ := f.s.Viewer(f.owner)
	st := f.s.SettingsFor(f.owner, f.org)
	st.SectionsOff = []string{"money", "projects", "opportunities", "today"}
	b := f.s.Build(context.Background(), v, st, BuildOptions{})
	if b.Headline != "2 bills due this week (KES 41,200) and 1 overdue (KES 5,000)" {
		t.Fatalf("headline %q", b.Headline)
	}
	sec := b.Section("bills")
	if sec == nil {
		t.Fatalf("no bills section: %+v", b.Hidden)
	}
	groups := map[string]Group{}
	for _, g := range sec.Groups {
		groups[g.Key] = g
	}
	if g := groups["overdue"]; len(g.Items) != 1 || g.Items[0].Value != "KES 5,000" || !strings.Contains(g.Items[0].Detail, "5 days overdue") || g.Items[0].Link == "" {
		t.Fatalf("overdue %+v", g)
	}
	if g := groups["week"]; len(g.Items) != 2 || g.Items[1].Tag != "New" || !strings.HasPrefix(g.Items[1].Link, "/bills?bill=") {
		t.Fatalf("week %+v", g)
	}
	// The new bill is listed under "due this week"; the inbox invoice is the "new" group.
	if g := groups["new"]; len(g.Items) != 1 || !strings.Contains(g.Items[0].Title, "Synergy Wholesale invoice INV-88123") || g.Items[0].Value != "AUD 120.50" || !strings.HasPrefix(g.Items[0].Detail, "≈ KES 10,250 · ") {
		t.Fatalf("new %+v", g)
	}
	if !strings.Contains(sec.Summary, "KES 41,200 due this week") {
		t.Fatalf("summary %q", sec.Summary)
	}
}

func TestSectionPermissionsStaffVsOwner(t *testing.T) {
	f := setup(t)
	seedBills(t, f)
	f.at("2026-11-19T05:00:00Z")
	// A booking for Sam today and one for Ann.
	cust := uuid.NewString()
	f.db.Exec(`INSERT INTO customers (id, first_name, last_name, organization_id) VALUES (?, 'Grace', 'Njeri', ?)`, cust, f.org)
	f.db.Exec(`INSERT INTO bookings (id, customer_id, organization_id, staff_id, start_time, end_time) VALUES (?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), cust, f.org, f.staff, time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC), time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC),
		uuid.NewString(), cust, f.org, f.owner, time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	staff, _ := f.s.Viewer(f.staff)
	if staff.Managing {
		t.Fatal("staff is not managing")
	}
	b := f.s.Build(context.Background(), staff, f.s.SettingsFor(f.staff, f.org), BuildOptions{})
	for _, s := range b.Sections {
		if s.Key == "bills" || s.Key == "money" || s.Key == "opportunities" {
			t.Fatalf("staff sees %s", s.Key)
		}
	}
	hidden := map[string]string{}
	for _, h := range b.Hidden {
		hidden[h.Key] = h.Reason
	}
	if hidden["bills"] != "not_allowed" || hidden["money"] != "not_allowed" || hidden["opportunities"] != "not_allowed" {
		t.Fatalf("hidden %v", hidden)
	}
	if strings.Contains(b.Headline, "KES") {
		t.Fatalf("staff headline shows money: %q", b.Headline)
	}
	today := b.Section("today")
	if today == nil || len(today.Groups) != 1 || len(today.Groups[0].Items) != 1 || today.Groups[0].Title != "Your bookings" || !strings.HasPrefix(today.Groups[0].Items[0].Title, "10:00 · Grace Njeri") {
		t.Fatalf("staff today %+v", today)
	}
	owner, _ := f.s.Viewer(f.owner)
	b = f.s.Build(context.Background(), owner, f.s.SettingsFor(f.owner, f.org), BuildOptions{})
	if b.Section("bills") == nil {
		t.Fatalf("owner has no bills: %+v", b.Hidden)
	}
	if td := b.Section("today"); td == nil || td.Groups[0].Title != "Bookings" || len(td.Groups[0].Items) != 2 {
		t.Fatalf("owner today %+v", td)
	}
	// Staff can't switch on a section they may not see by asking for it.
	if err := func() error {
		_, err := f.s.SaveSettings(staff, SettingsInput{Sections: map[string]bool{"bills": true}})
		return err
	}(); err != nil {
		t.Fatal(err)
	}
	b = f.s.Build(context.Background(), staff, f.s.SettingsFor(f.staff, f.org), BuildOptions{})
	if b.Section("bills") != nil {
		t.Fatal("staff got bills after switching the section on")
	}
}

func TestSettingsValidationAndPhone(t *testing.T) {
	f := setup(t)
	v, _ := f.s.Viewer(f.admin2)
	on, wa := true, true
	if _, err := f.s.SaveSettings(v, SettingsInput{Enabled: &on, WhatsApp: &wa}); err == nil || !strings.Contains(err.Error(), "WhatsApp number") {
		t.Fatalf("whatsapp without a number: %v", err)
	}
	bad := "12"
	if _, err := f.s.SaveSettings(v, SettingsInput{Phone: &bad}); err == nil {
		t.Fatal("bad number accepted")
	}
	ph := "0733 111 222"
	st, err := f.s.SaveSettings(v, SettingsInput{Enabled: &on, WhatsApp: &wa, Phone: &ph})
	if err != nil || !st.WhatsApp || !st.Enabled {
		t.Fatalf("save: %+v %v", st, err)
	}
	var phone string
	f.db.Raw(`SELECT phone FROM users WHERE id = ?`, f.admin2).Scan(&phone)
	if phone != "+254733111222" {
		t.Fatalf("users.phone = %q", phone)
	}
	tm, tz := "7:30", "Not/AZone"
	if _, err := f.s.SaveSettings(v, SettingsInput{Time: &tm, Timezone: &tz}); err == nil {
		t.Fatal("unknown zone accepted")
	}
	tz = "Europe/London"
	st, err = f.s.SaveSettings(v, SettingsInput{Time: &tm, Timezone: &tz})
	if err != nil || st.Time != "07:30" || st.Timezone != "Europe/London" {
		t.Fatalf("time/zone: %+v %v", st, err)
	}
	no := false
	if _, err := f.s.SaveSettings(v, SettingsInput{Email: &no, WhatsApp: &no}); err == nil {
		t.Fatal("no channel accepted")
	}
}

func TestWhatsAppTemplatePlatformFallbackAndWindow(t *testing.T) {
	f := setup(t)
	meta := newFakeMeta(t)
	f.platformWhatsApp()
	f.at("2026-11-18T12:00:00Z")
	wa1 := true
	f.enable(f.owner, func(in *SettingsInput) { onlySections("bills")(in); in.WhatsApp = &wa1 })
	seedBills(t, f)
	// A headline with characters Meta refuses in parameters still goes out clean.
	Register("zz_odd", "Odd", 5, func(ctx context.Context, in *Input) (*Section, error) {
		s := &Section{}
		s.Add("x", "X", []Item{{Title: "x"}}, 1, "")
		s.Say(1, "Line one\nline two\tand      spaces")
		return s, nil
	})
	defer Unregister("zz_odd")
	f.at("2026-11-19T05:00:30Z")
	if r := f.s.RunDue(context.Background()); len(r.Sent) != 1 {
		t.Fatalf("run %+v", r)
	}
	sends := meta.list()
	if len(sends) != 1 || sends[0]["type"] != "template" || sends[0]["to"] != "254712345678" {
		t.Fatalf("sends %v", sends)
	}
	tpl := sends[0]["template"].(map[string]any)
	if tpl["name"] != "dasyin_morning_brief" {
		t.Fatalf("template %v", tpl)
	}
	comps := tpl["components"].([]any)
	params := comps[0].(map[string]any)["parameters"].([]any)
	if len(params) != 3 || params[0].(map[string]any)["text"] != "Ann" || params[1].(map[string]any)["text"] != "Thu 19 Nov" {
		t.Fatalf("params %v", params)
	}
	head := params[2].(map[string]any)["text"].(string)
	if strings.ContainsAny(head, "\n\t") || strings.Contains(head, "    ") || !strings.HasPrefix(head, "Line one · line two and   spaces, 2 bills due this week (KES 41,200) and 1 overdue (KES 5,000)") {
		t.Fatalf("headline param %q", head)
	}
	if btn := comps[1].(map[string]any); btn["sub_type"] != "url" || btn["parameters"].([]any)[0].(map[string]any)["text"] != "2026-11-19" {
		t.Fatalf("button %v", btn)
	}
	snap := f.snapshots(f.owner)[0]
	if snap.WAStatus != ChAccepted || !snap.WAPlatform || snap.WAKind != "template" || snap.WAMessageID == "" || snap.Status != StatusSent {
		t.Fatalf("snapshot %+v", snap)
	}
	var lg wa.MessageLog
	f.db.Where("message_id = ?", snap.WAMessageID).First(&lg)
	if lg.Source != "brief" || lg.SourceRef != snap.ID || lg.OrganizationID != f.org || lg.SenderOrgID != f.platform || lg.Template != "dasyin_morning_brief" {
		t.Fatalf("message log %+v", lg)
	}
	// Delivery statuses from the webhook move the brief along.
	f.s.OnWhatsAppStatus(wa.StatusEvent{MessageID: snap.WAMessageID, Status: "delivered"})
	f.s.OnWhatsAppStatus(wa.StatusEvent{MessageID: snap.WAMessageID, Status: "read"})
	f.s.OnWhatsAppStatus(wa.StatusEvent{MessageID: snap.WAMessageID, Status: "delivered"}) // late, out of order
	f.db.First(&snap, "id = ?", snap.ID)
	if snap.WAStatus != ChRead {
		t.Fatalf("status %s", snap.WAStatus)
	}

	// Ann messaged DASYIN's number in the last 24 h: the fuller text goes instead.
	f.db.Create(&wa.Window{OrganizationID: f.platform, Phone: "254712345678", LastInboundAt: time.Now().Add(-2 * time.Hour)})
	v, _ := f.s.Viewer(f.owner)
	st := f.s.SettingsFor(f.owner, f.org)
	ts, err := f.s.SendNow(context.Background(), v, st, KindTest)
	if err != nil {
		t.Fatal(err)
	}
	sends = meta.list()
	last := sends[len(sends)-1]
	body := last["text"].(map[string]any)["body"].(string)
	if last["type"] != "text" || !strings.Contains(body, "*Bills & payables*") || !strings.Contains(body, "https://app.example/brief/2026-11-19") || ts.WAKind != "text" {
		t.Fatalf("text send %v / %+v", last, ts)
	}
	// The window closed between the check and the send: falls back to the template.
	meta.mu.Lock()
	meta.closed["254712345678"] = true
	meta.mu.Unlock()
	ts, _ = f.s.SendNow(context.Background(), v, st, KindTest)
	sends = meta.list()
	if sends[len(sends)-1]["type"] != "template" || ts.WAKind != "template" || ts.WAStatus != ChAccepted {
		t.Fatalf("fallback %v / %+v", sends[len(sends)-1], ts)
	}
	// Test sends don't count as the day's brief.
	var n int64
	f.db.Model(&Snapshot{}).Where("user_id = ? AND kind = ?", f.owner, KindTest).Count(&n)
	if n != 1 {
		t.Fatalf("%d test snapshots", n)
	}
}

func TestWhatsAppRetryMissingTemplateAndOwnNumber(t *testing.T) {
	f := setup(t)
	meta := newFakeMeta(t)
	f.platformWhatsApp()
	// Without a template chosen the WhatsApp part fails plainly; email still goes.
	f.s.SaveConfig("", "en", "test")
	f.at("2026-11-18T12:00:00Z")
	wa1 := true
	f.enable(f.owner, func(in *SettingsInput) { onlySections("today")(in); in.WhatsApp = &wa1 })
	v, _ := f.s.Viewer(f.owner)
	snap, _ := f.s.SendNow(context.Background(), v, f.s.SettingsFor(f.owner, f.org), KindTest)
	if snap.Status != StatusPartial || snap.EmailStatus != ChSent || snap.WAStatus != ChFailed || !strings.Contains(snap.WAError, "System settings → Notifications") {
		t.Fatalf("no template: %+v", snap)
	}
	f.s.SaveConfig("dasyin_morning_brief", "en", "test")
	// A rate limit is retried later and then goes out.
	meta.mu.Lock()
	meta.failN = 1
	meta.mu.Unlock()
	f.at("2026-11-19T05:00:10Z")
	r := f.s.RunDue(context.Background())
	if len(r.Sent) != 1 {
		t.Fatalf("run %+v", r)
	}
	var s Snapshot
	f.db.Where("user_id = ? AND kind = ?", f.owner, KindScheduled).First(&s)
	if s.Status != StatusRetry || s.WAStatus != ChRetry || s.EmailStatus != ChSent || s.NextRetryAt == nil || !strings.Contains(s.WAError, "rate limit") {
		t.Fatalf("after rate limit %+v", s)
	}
	emails := len(f.mailsTo("ann@kilima.example"))
	f.at("2026-11-19T05:06:00Z")
	r = f.s.RunDue(context.Background())
	if len(r.Retried) != 1 {
		t.Fatalf("retry run %+v", r)
	}
	f.db.First(&s, "id = ?", s.ID)
	if s.Status != StatusSent || s.WAStatus != ChAccepted || s.Attempts != 2 || len(f.mailsTo("ann@kilima.example")) != emails {
		t.Fatalf("after retry %+v (emails %d→%d)", s, emails, len(f.mailsTo("ann@kilima.example")))
	}
	// An organisation with its own WhatsApp number sends from it, not DASYIN's.
	own := models.IntegrationSettings{ID: uuid.NewString(), OrganizationID: f.org,
		WhatsAppSettings: models.WhatsAppSettings{Enabled: true, AccessToken: "platform-token", PhoneNumberID: "777", APIVersion: "v21.0"}}
	if err := f.db.Omit(clause.Associations).Create(&own).Error; err != nil {
		t.Fatal(err)
	}
	ts, _ := f.s.SendNow(context.Background(), v, f.s.SettingsFor(f.owner, f.org), KindTest)
	var lg wa.MessageLog
	f.db.Where("message_id = ?", ts.WAMessageID).First(&lg)
	if ts.WAPlatform || ts.WAStatus != ChAccepted || lg.SenderOrgID != f.org {
		t.Fatalf("own number: %+v / %+v", ts, lg)
	}
}

// ---------------------------------------------------------------- Jira

func fakeJira(t *testing.T, calls *int32, down *atomic.Bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if down.Load() {
			w.WriteHeader(503)
			io.WriteString(w, `{"errorMessages":["Service unavailable"]}`)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "me@kilima.example" || pass != "secret-token" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/api/3/field":
			io.WriteString(w, `[]`)
		case "/rest/api/3/project/search":
			io.WriteString(w, `{"values":[{"id":"10","key":"DA","name":"DASYIN App","projectTypeKey":"software","lead":{"displayName":"Ann"}}],"startAt":0,"maxResults":50,"total":1,"isLast":true}`)
		case "/rest/api/3/search/jql":
			io.WriteString(w, `{"issues":[
				{"id":"101","key":"DA-1","fields":{"summary":"Fix invoice PDF footer","status":{"name":"To Do","statusCategory":{"key":"new"}},"priority":{"name":"Medium"},"duedate":"2026-11-14","updated":"2026-11-10T10:00:00.000+0300"}},
				{"id":"102","key":"DA-2","fields":{"summary":"Release 2.15","status":{"name":"Done","statusCategory":{"key":"done"}},"resolutiondate":"2026-11-19T06:30:00.000+0300","updated":"2026-11-19T06:30:00.000+0300"}},
				{"id":"103","key":"DA-3","fields":{"summary":"Payments webhook retries","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"priority":{"name":"High"},"duedate":"2026-11-21","assignee":{"displayName":"Ben Otieno"}}},
				{"id":"104","key":"DA-4","fields":{"summary":"M-Pesa callback times out","status":{"name":"Blocked","statusCategory":{"key":"indeterminate"}},"priority":{"name":"Highest"}}}
			],"isLast":true}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestJiraSyncBeforeBuilding(t *testing.T) {
	f := setup(t)
	var calls int32
	var down atomic.Bool
	srv := fakeJira(t, &calls, &down)
	h := projects2.NewHandler(f.db, nil)
	h.HTTP = http.DefaultClient
	h.ValidateSite = func(s string) (string, error) { return s, nil }
	h.Now = f.s.Now
	f.s.JiraSync = func(ctx context.Context, org string) error { _, err := h.SyncOrg(ctx, org); return err }
	f.at("2026-11-19T05:00:00Z")
	old := f.s.Now().Add(-2 * time.Hour)
	f.db.Create(&projects2.JiraConnection{OrgID: f.org, SiteURL: srv.URL, Email: "me@kilima.example", TokenEnc: "secret-token", ProjectKeys: "DA", LastSyncAt: &old, LastSyncStatus: "success"})
	v, _ := f.s.Viewer(f.owner)
	st := f.s.SettingsFor(f.owner, f.org)
	st.SectionsOff = []string{"bills", "money", "opportunities", "today"}
	f.db.Create(&Snapshot{ID: uuid.NewString(), OrganizationID: f.org, UserID: f.owner, Date: "2026-11-18", Kind: KindScheduled, Status: StatusSent,
		CreatedAt: time.Date(2026, 11, 18, 5, 0, 0, 0, time.UTC)})

	b := f.s.Build(context.Background(), v, st, BuildOptions{SyncJira: true})
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("no Jira sync before building")
	}
	sec := b.Section("projects")
	if sec == nil || len(sec.Notes) != 0 {
		t.Fatalf("projects %+v hidden %+v", sec, b.Hidden)
	}
	if !strings.Contains(b.Headline, "1 Jira issue overdue") || !strings.Contains(b.Headline, "1 issue high priority or blocked") {
		t.Fatalf("headline %q", b.Headline)
	}
	g := map[string]Group{}
	for _, x := range sec.Groups {
		g[x.Key] = x
	}
	if p := g["progress"]; len(p.Items) != 1 || p.Items[0].Value != "25%" || p.Items[0].Tag != "Jira" || !strings.Contains(p.Items[0].Detail, "1 since your last brief") {
		t.Fatalf("progress %+v", p)
	}
	if o := g["overdue"]; len(o.Items) != 1 || !strings.HasPrefix(o.Items[0].Title, "DA-1 Fix invoice PDF footer") || o.Items[0].External != srv.URL+"/browse/DA-1" {
		t.Fatalf("overdue %+v", o)
	}
	if d := g["done"]; len(d.Items) != 1 || !strings.HasPrefix(d.Items[0].Title, "DA-2") {
		t.Fatalf("done %+v", d)
	}
	if d := g["due"]; len(d.Items) != 1 || !strings.HasPrefix(d.Items[0].Title, "DA-3") {
		t.Fatalf("due %+v", d)
	}
	if h := g["hot"]; len(h.Items) != 1 || !strings.HasPrefix(h.Items[0].Title, "DA-4") || h.Items[0].Tag != "Blocked" {
		t.Fatalf("hot %+v", h)
	}
	// Fresh (synced just now): no second sync.
	n := atomic.LoadInt32(&calls)
	f.s.Build(context.Background(), v, st, BuildOptions{SyncJira: true})
	if atomic.LoadInt32(&calls) != n {
		t.Fatal("synced again within the hour")
	}
	// Two hours later Jira is down: the brief still goes out with yesterday's figures and says so.
	down.Store(true)
	f.at("2026-11-19T07:10:00Z")
	b = f.s.Build(context.Background(), v, st, BuildOptions{SyncJira: true})
	if atomic.LoadInt32(&calls) == n {
		t.Fatal("no sync attempt when stale")
	}
	sec = b.Section("projects")
	if sec == nil || len(sec.Notes) != 1 || sec.Notes[0] != "Jira could not be reached; figures from Thu 19 Nov 08:00" {
		t.Fatalf("note %+v", sec)
	}
	if !strings.Contains(b.Headline, "1 Jira issue overdue") {
		t.Fatalf("headline without Jira %q", b.Headline)
	}
}

// ---------------------------------------------------------------- digests

func TestDigestsSkipPeopleWhoseBriefCoversThem(t *testing.T) {
	f := setup(t)
	t.Setenv("SMTP_HOST", "smtp.test.invalid")
	t.Setenv("SMTP_FROM", "Billing <billing@kilima.example>")
	seedBills(t, f)
	var mu sync.Mutex
	var got []mail.Message
	ms := mail.NewService(f.db)
	ms.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		return nil
	}
	recipients := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := []string{}
		for _, m := range got {
			out = append(out, strings.Join(m.To, ","))
		}
		got = nil
		return out
	}
	rb := recurbills.New(f.db, &fxrates.Service{DB: f.db}, nil)
	rb.Mail = ms
	rb.Now = f.s.Now
	recurbills.BriefCovers = func(org string) map[string]bool { return f.s.Covered(org, "bills") }
	subs := subscriptions.New(f.db, nil, nil)
	subs.Mail = ms
	subs.Now = f.s.Now
	subscriptions.BriefCovers = func(org string) map[string]bool { return f.s.Covered(org, "money") }
	t.Cleanup(func() { recurbills.BriefCovers, subscriptions.BriefCovers = nil, nil })
	ctx := context.Background()
	f.at("2026-11-23T06:00:00Z")

	// Nobody has the brief: both admins get the digests.
	if st, err := rb.SendDigest(ctx, f.org, "digest|2026-W48a", "2026-W48a"); err != nil || st != "sent" {
		t.Fatalf("digest %s %v", st, err)
	}
	if r := recipients(); len(r) != 1 || r[0] != "ann@kilima.example,ben@kilima.example" {
		t.Fatalf("recipients %v", r)
	}
	// Ann's brief lists bills: the weekly bills digest goes to Ben only.
	f.enable(f.owner, nil)
	if c := f.s.Covered(f.org, "bills"); !c["ann@kilima.example"] || len(c) != 1 {
		t.Fatalf("covered %v", c)
	}
	rb.SendDigest(ctx, f.org, "digest|2026-W48b", "2026-W48b")
	if r := recipients(); len(r) != 1 || r[0] != "ben@kilima.example" {
		t.Fatalf("recipients %v", r)
	}
	// A manual "Send now" still reaches everyone.
	rb.SendDigest(ctx, f.org, "", "manual")
	if r := recipients(); len(r) != 1 || !strings.Contains(r[0], "ann@") {
		t.Fatalf("manual %v", r)
	}
	// Ben's brief covers bills too: nothing is emailed, the digest is logged as skipped.
	f.enable(f.admin2, nil)
	st, err := rb.SendDigest(ctx, f.org, "digest|2026-W48c", "2026-W48c")
	if err != nil || st != "skipped" || len(recipients()) != 0 {
		t.Fatalf("all covered: %s %v", st, err)
	}
	var lg recurbills.ReminderLog
	f.db.Where("organization_id = ? AND key = ?", f.org, "digest|2026-W48c").First(&lg)
	if lg.Status != "skipped" || !strings.Contains(lg.Error, "morning brief") {
		t.Fatalf("log %+v", lg)
	}
	// Ann asks for the separate digests as well.
	yes := true
	f.enable(f.owner, func(in *SettingsInput) { in.AlsoDigests = &yes })
	rb.SendDigest(ctx, f.org, "digest|2026-W48d", "2026-W48d")
	if r := recipients(); len(r) != 1 || r[0] != "ann@kilima.example" {
		t.Fatalf("also digests %v", r)
	}
	// Ben switches the bills section off: he gets the digest again.
	f.enable(f.admin2, func(in *SettingsInput) { in.Sections = map[string]bool{"bills": false} })
	rb.SendDigest(ctx, f.org, "digest|2026-W48e", "2026-W48e")
	if r := recipients(); len(r) != 1 || r[0] != "ann@kilima.example,ben@kilima.example" {
		t.Fatalf("section off %v", r)
	}

	// The daily client billing digest (an open provider task makes it non-empty).
	f.db.Create(&subscriptions.Task{ID: uuid.NewString(), OrganizationID: f.org, Key: "t1", SubscriptionID: "s1", Kind: "suspend", Status: "open",
		Title: "Suspend example.co.ke", DueDate: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC), CreatedAt: time.Now(), UpdatedAt: time.Now()})
	// Ann: brief on with money, AlsoDigests on → still gets it; Ben: money on, no also → skipped.
	if st, err := subs.SendDigest(ctx, f.org, "digest|2026-11-23", false); err != nil || st != "sent" {
		t.Fatalf("subs digest %s %v", st, err)
	}
	if r := recipients(); len(r) != 1 || r[0] != "ann@kilima.example" {
		t.Fatalf("subs recipients %v", r)
	}
	no := false
	f.enable(f.owner, func(in *SettingsInput) { in.AlsoDigests = &no })
	st, err = subs.SendDigest(ctx, f.org, "digest|2026-11-24", false)
	if err != nil || st != "skipped" || len(recipients()) != 0 {
		t.Fatalf("subs all covered %s %v", st, err)
	}
	// Staff never count as covered (their brief has no money sections).
	f.enable(f.staff, nil)
	if c := f.s.Covered(f.org, "money"); c["sam@kilima.example"] {
		t.Fatalf("staff covered: %v", c)
	}
}

func TestHistoryHandlerShapes(t *testing.T) {
	f := setup(t)
	f.at("2026-11-18T12:00:00Z")
	f.enable(f.owner, onlySections("today"))
	f.at("2026-11-19T05:00:30Z")
	f.s.RunDue(context.Background())
	var s Snapshot
	f.db.Where("user_id = ?", f.owner).First(&s)
	if s.Data == nil || s.Data.Date != "2026-11-19" || s.Headline == "" || s.Data.Headline != s.Headline {
		t.Fatalf("snapshot data %+v", s)
	}
	if next := f.s.NextSend(f.s.SettingsFor(f.owner, f.org)); !next.Equal(time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("next send %s", next)
	}
	// Briefs older than KeepDays go; a deactivated person gets none.
	f.db.Create(&Snapshot{ID: uuid.NewString(), OrganizationID: f.org, UserID: f.owner, Date: "2026-07-01", Kind: KindScheduled, Status: StatusSent})
	f.db.Exec(`UPDATE users SET is_active = false WHERE id = ?`, f.owner)
	f.s.lastPrune = time.Time{}
	f.at("2026-11-20T05:00:30Z")
	if r := f.s.RunDue(context.Background()); len(r.Sent) != 0 || r.Checked != 0 {
		t.Fatalf("inactive person: %+v", r)
	}
	var n int64
	f.db.Model(&Snapshot{}).Where("user_id = ? AND date = ?", f.owner, "2026-07-01").Count(&n)
	if n != 0 {
		t.Fatal("old brief kept")
	}
}

func TestEmailFollowsWhoGetsEmails(t *testing.T) {
	f := setup(t)
	f.at("2026-11-18T12:00:00Z")
	f.enable(f.owner, onlySections("today"))
	v, _ := f.s.Viewer(f.owner)
	on := true
	if err := emailprefs.SetSelf(f.db, f.org, f.owner, &on, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := f.s.SendNow(context.Background(), v, f.s.SettingsFor(f.owner, f.org), KindTest)
	if err != nil {
		t.Fatal(err)
	}
	if snap.EmailStatus != ChFailed || !strings.Contains(snap.EmailError, "Not emailed") || len(f.mailsTo("ann@kilima.example")) != 0 {
		t.Fatalf("paused emails: %+v", snap)
	}
	off := false
	emailprefs.SetSelf(f.db, f.org, f.owner, &off, nil)
	snap, _ = f.s.SendNow(context.Background(), v, f.s.SettingsFor(f.owner, f.org), KindTest)
	if snap.EmailStatus != ChSent || len(f.mailsTo("ann@kilima.example")) != 1 {
		t.Fatalf("emails back on: %+v", snap)
	}
}
