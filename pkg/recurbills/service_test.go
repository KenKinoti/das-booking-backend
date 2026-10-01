package recurbills

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`DROP SCHEMA IF EXISTS recurbills_test CASCADE`)
	boot.Exec(`CREATE SCHEMA recurbills_test`)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=recurbills_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(append(Models(), &finance.ChartOfAccount{}, &finance.JournalEntry{}, &finance.JournalEntryLine{}, &finance.Bill{}, &finance.BillLineItem{},
		&finance.Vendor{}, &finance.Payment{}, &finance.BankAccount{}, &finance.BankTransaction{}, &finance.GeneralLedger{}, &finance.AccountBalance{}, &finance.AuditTrail{},
		&fxrates.ManualRate{}, &notify.Notification{})...); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) primary key, name text, email text, phone text, address_street text, address_suburb text, address_state text, address_postcode text,
			country_code text, currency text, timezone text, region_source text, address_country text)`,
		`CREATE TABLE users (id varchar(255) primary key, email text, role text, first_name text, last_name text, organization_id varchar(255), is_active boolean default true, created_at timestamptz, deleted_at timestamptz)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

type sent struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (s *sent) list() []mail.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mail.Message{}, s.msgs...)
}

type fixture struct {
	db     *gorm.DB
	s      *Service
	org    string
	vendor string
	now    time.Time
	out    *sent
}

func setup(t *testing.T, cur string) *fixture {
	db := testDB(t)
	t.Setenv("SMTP_HOST", "smtp.test.invalid")
	t.Setenv("SMTP_FROM", "Billing <billing@test.example>")
	f := &fixture{db: db, org: uuid.NewString(), out: &sent{}}
	db.Exec(`INSERT INTO organizations (id, name, email, country_code, currency, timezone, region_source) VALUES (?, 'Mursa Hosting', 'hello@mursa.example', 'AU', ?, 'Australia/Sydney', 'user')`, f.org, cur)
	db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id, created_at) VALUES (?, 'owner@mursa.example', 'admin', 'Ada', 'Owner', ?, now()),
		(?, 'staff@mursa.example', 'care_worker', 'Sam', 'Staff', ?, now())`, uuid.NewString(), f.org, uuid.NewString(), f.org)
	ms := mail.NewService(db)
	ms.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error {
		f.out.mu.Lock()
		f.out.msgs = append(f.out.msgs, m)
		f.out.mu.Unlock()
		return nil
	}
	f.s = &Service{DB: db, Fin: finance.NewService(db), FX: &fxrates.Service{DB: db}, Mail: ms, AppURL: "https://app.example",
		Now: func() time.Time { return f.now }}
	v, err := f.s.Fin.CreateVendor(f.org, finance.VendorInput{Name: "Synergy Wholesale"})
	if err != nil {
		t.Fatal(err)
	}
	f.vendor = v.ID
	return f
}

// at sets "now" to 09:00 Sydney on day.
func (f *fixture) at(day string) {
	loc, _ := time.LoadLocation("Australia/Sydney")
	t, _ := time.ParseInLocation("2006-01-02 15:04", day+" 09:00", loc)
	f.now = t
}

func (f *fixture) bills(t *testing.T) []finance.Bill {
	var out []finance.Bill
	f.db.Preload("LineItems").Where("organization_id = ?", f.org).Order("bill_date").Find(&out)
	return out
}

func TestGenerateIdempotentCatchUpSkipPause(t *testing.T) {
	f := setup(t, "AUD")
	f.at("2026-10-01")
	r, err := f.s.Create(f.org, "u1", Input{Name: "Synergy hosting", VendorID: f.vendor, Amount: 89.25, TaxRate: 10, Frequency: FreqMonthly,
		StartDate: "2026-10-31", CreateAs: "approved", AdvanceDays: ptr(7), DueDays: 0})
	if err != nil {
		t.Fatal(err)
	}
	if r.NextDate != "2026-10-31" || r.Summary != "Monthly on the 31st" {
		t.Fatalf("decorate: %+v", r)
	}
	if g := f.s.Generate(f.org); g.Created != 0 {
		t.Fatalf("created too early: %+v", g)
	}
	f.at("2026-10-24") // 7 days ahead of 31 Oct
	g := f.s.Generate(f.org)
	if g.Created != 1 || len(g.Errors) != 0 {
		t.Fatalf("first run: %+v", g)
	}
	// Idempotent: a second run (and a concurrent one) creates nothing.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.s.Generate(f.org) }()
	}
	wg.Wait()
	bills := f.bills(t)
	if len(bills) != 1 || bills[0].Total != 89.25 || bills[0].Status != "unpaid" || bills[0].BillDate.Format(dayFmt) != "2026-10-31" {
		t.Fatalf("bills after reruns: %d %+v", len(bills), bills)
	}
	if !strings.Contains(bills[0].LineItems[0].Description, "Oct 2026") || bills[0].TaxAmount != 8.11 {
		t.Fatalf("line: %+v tax %.2f", bills[0].LineItems[0], bills[0].TaxAmount)
	}
	// Server down for three months: catch-up creates the missed months once,
	// clamped to month ends (30 Nov, 31 Dec, 31 Jan).
	f.at("2027-01-26")
	g = f.s.Generate(f.org)
	if g.Created != 3 {
		t.Fatalf("catch-up: %+v", g)
	}
	got := []string{}
	for _, b := range f.bills(t) {
		got = append(got, b.BillDate.Format(dayFmt))
	}
	eq(t, got, []string{"2026-10-31", "2026-11-30", "2026-12-31", "2027-01-31"})
	// Skip February: no bill, no reminders; unskip brings it back.
	if err := f.s.Skip(f.org, "u1", r.ID, "2027-02-28", "holiday"); err != nil {
		t.Fatal(err)
	}
	f.at("2027-02-22")
	if g := f.s.Generate(f.org); g.Created != 0 {
		t.Fatalf("skipped period generated: %+v", g)
	}
	if err := f.s.Unskip(f.org, r.ID, "2027-02-28"); err != nil {
		t.Fatal(err)
	}
	if g := f.s.Generate(f.org); g.Created != 1 {
		t.Fatalf("unskipped period not generated: %+v", g)
	}
	// Pause in March, resume in May: the paused months are not caught up.
	if _, err := f.s.SetPaused(f.org, r.ID, true); err != nil {
		t.Fatal(err)
	}
	f.at("2027-03-30")
	if g := f.s.Generate(f.org); g.Created != 0 || g.Schedules != 0 {
		t.Fatalf("paused schedule generated: %+v", g)
	}
	f.at("2027-05-02")
	if _, err := f.s.SetPaused(f.org, r.ID, false); err != nil {
		t.Fatal(err)
	}
	f.s.Generate(f.org)
	f.at("2027-05-25")
	f.s.Generate(f.org)
	got = []string{}
	for _, b := range f.bills(t) {
		got = append(got, b.BillDate.Format(dayFmt))
	}
	eq(t, got, []string{"2026-10-31", "2026-11-30", "2026-12-31", "2027-01-31", "2027-02-28", "2027-05-31"})
}

func ptr(i int) *int { return &i }

func TestEndAfterEstimateAndForeignCurrency(t *testing.T) {
	f := setup(t, "KES")
	f.at("2026-10-01")
	if _, err := f.s.FX.SaveManual(f.org, "u1", "AUD", "KES", 85, "test"); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.Create(f.org, "u1", Input{Name: "Google Workspace", VendorID: f.vendor, Amount: 36, Currency: "AUD", Frequency: FreqWeekly,
		StartDate: "2026-10-02", EndMode: EndAfter, EndAfter: 2, CreateAs: "approved", AmountMode: AmountEstimate, AdvanceDays: ptr(0)})
	if err != nil {
		t.Fatal(err)
	}
	f.at("2026-10-20")
	g := f.s.Generate(f.org)
	if g.Created != 2 {
		t.Fatalf("end after 2: %+v", g)
	}
	for _, b := range f.bills(t) {
		// Estimates are always drafts (never counted until confirmed),
		// converted to the home currency at the organisation's rate.
		if b.Status != "draft" || b.Total != 3060 || !strings.Contains(b.Notes, "A$36.00 at 85 KES per AUD") {
			t.Fatalf("bill %+v notes %q", b, b.Notes)
		}
	}
	got, _ := f.s.Get(f.org, r.ID)
	if got.Status != StatusEnded {
		t.Fatalf("status %s", got.Status)
	}
}

func TestRemindersStagesIdempotentSnoozeAndDigest(t *testing.T) {
	f := setup(t, "AUD")
	f.at("2026-10-01")
	r, err := f.s.Create(f.org, "u1", Input{Name: "Synergy Wholesale hosting", VendorID: f.vendor, Amount: 89.25, Frequency: FreqMonthly, StartDate: "2026-10-25",
		CreateAs: "approved", Service: "Web hosting", SynergyDomain: "mursa.example", ExtraRecipients: []string{"ops@mursa.example"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 7 days before: bill generated + first reminder.
	f.at("2026-10-18")
	res := f.s.RunOrg(ctx, f.org, RunOptions{})
	if res.Generated.Created != 1 || res.Reminders != 1 || res.Failed != 0 {
		t.Fatalf("run 1: %+v", res)
	}
	msgs := f.out.list()
	m := msgs[0]
	want := "Upcoming: Synergy Wholesale hosting A$89.25 due 25 Oct (in 7 days) — keep services online"
	if m.Subject != want {
		t.Fatalf("subject %q", m.Subject)
	}
	to := strings.Join(m.To, ",")
	if !strings.Contains(to, "owner@mursa.example") || !strings.Contains(to, "ops@mursa.example") || strings.Contains(to, "staff@mursa.example") {
		t.Fatalf("recipients %v", m.To)
	}
	for _, s := range []string{"Mark as paid", "https://app.example/bills?pay=", "View bill", "Needs manual payment", "mursa.example", "Not recorded yet"} {
		if !strings.Contains(m.HTML, s) {
			t.Fatalf("html lacks %q", s)
		}
	}
	// Same day again, and later that week: never re-sent.
	f.s.RunOrg(ctx, f.org, RunOptions{})
	f.at("2026-10-21")
	f.s.RunOrg(ctx, f.org, RunOptions{})
	if n := len(f.out.list()); n != 1 {
		t.Fatalf("re-sent: %d", n)
	}
	// Day before → before_1.
	f.at("2026-10-24")
	f.s.RunOrg(ctx, f.org, RunOptions{})
	msgs = f.out.list()
	if len(msgs) != 2 || !strings.Contains(msgs[1].Subject, "(tomorrow)") {
		t.Fatalf("before_1: %d %q", len(msgs), msgs[len(msgs)-1].Subject)
	}
	// Before 08:00 nothing goes out unless forced.
	loc, _ := time.LoadLocation("Australia/Sydney")
	f.now = time.Date(2026, 10, 25, 6, 0, 0, 0, loc)
	if res := f.s.RunOrg(ctx, f.org, RunOptions{}); !res.SkippedHour || res.Reminders != 0 {
		t.Fatalf("send hour: %+v", res)
	}
	if res := f.s.RunOrg(ctx, f.org, RunOptions{Force: true}); res.Reminders != 1 || !strings.HasPrefix(f.out.list()[2].Subject, "Due today:") {
		t.Fatalf("forced due: %+v", res)
	}
	// Overdue: urgent tone; snoozing stops it.
	f.at("2026-10-28")
	f.s.RunOrg(ctx, f.org, RunOptions{})
	msgs = f.out.list()
	last := msgs[len(msgs)-1]
	if !strings.HasPrefix(last.Subject, "Overdue: Synergy Wholesale hosting A$89.25 was due 25 Oct (3 days ago)") || !strings.Contains(last.HTML, "Overdue bill") {
		t.Fatalf("overdue: %q", last.Subject)
	}
	var bill finance.Bill
	f.db.Where("organization_id = ?", f.org).First(&bill)
	if _, err := f.s.Snooze(f.org, bill.ID, "", "", 7, ""); err != nil {
		t.Fatal(err)
	}
	f.at("2026-10-31")
	before := len(f.out.list())
	f.s.RunOrg(ctx, f.org, RunOptions{})
	if len(f.out.list()) != before {
		t.Fatalf("snoozed bill reminded: %q", f.out.list()[len(f.out.list())-1].Subject)
	}
	// Paying stops reminders; the upcoming view shows the period as paid.
	if _, err := f.s.Fin.RecordBillPayment(f.org, "u1", bill.ID, finance.PaymentInput{Amount: 89.25, PaymentDate: "2026-10-31"}); err != nil {
		t.Fatal(err)
	}
	f.at("2026-11-06")
	f.s.RunOrg(ctx, f.org, RunOptions{})
	for _, m := range f.out.list()[before:] {
		if strings.Contains(m.Subject, "25 Oct") {
			t.Fatalf("paid bill reminded: %q", m.Subject)
		}
	}
	// Log: one row per stage.
	var stages []string
	f.db.Model(&ReminderLog{}).Where("organization_id = ? AND bill_id = ?", f.org, bill.ID).Order("created_at").Pluck("stage", &stages)
	eq(t, stages, []string{"before_7", "before_1", "due", "overdue_1"})
	// Upcoming: next period is projected; totals per month.
	up := f.s.UpcomingFor(f.org, 90)
	var nov *UpcomingItem
	for i := range up.Items {
		if up.Items[i].Period == "2026-11-25" {
			nov = &up.Items[i]
		}
	}
	if nov == nil || nov.Amount != 89.25 || nov.RecurringID != r.ID {
		t.Fatalf("upcoming: %+v", up.Items)
	}
	if len(up.Months) == 0 || up.Months[0].Month != "2026-11" {
		t.Fatalf("months %+v", up.Months)
	}
}

func TestDigestAndTrackMode(t *testing.T) {
	f := setup(t, "AUD")
	f.at("2026-10-01")
	// A one-off overdue bill and a track-mode schedule (bills come from Expenses).
	if _, err := f.s.Fin.CreateBill(f.org, "u1", finance.BillInput{VendorID: f.vendor, BillDate: "2026-09-01", DueDate: "2026-09-20",
		LineItems: []finance.BillLineInput{{Description: "Domain renewal", Quantity: 1, UnitPrice: 30}}}); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.Create(f.org, "u1", Input{Name: "Google Workspace", VendorID: f.vendor, Amount: 36, Frequency: FreqMonthly, StartDate: "2026-10-05",
		Mode: ModeTrack, AutoPay: true})
	if err != nil {
		t.Fatal(err)
	}
	d := f.s.Digest(f.org)
	if len(d.Overdue) != 1 || len(d.Upcoming) != 1 || d.Total != 66 || !strings.Contains(d.Subject, "1 overdue, 1 due in the next 14 days") {
		t.Fatalf("digest %+v", d)
	}
	msg := f.s.digestMessage(f.org, d, []string{"owner@mursa.example"})
	for _, s := range []string{"OVERDUE (1)", "Domain renewal", "Google Workspace", "auto-pay", "Weekly bills digest"} {
		if !strings.Contains(msg.Text+msg.HTML, s) {
			t.Fatalf("digest lacks %q:\n%s", s, msg.Text)
		}
	}
	// Weekly digest goes out once on Monday 08:00.
	f.at("2026-10-05") // a Monday
	res := f.s.RunOrg(context.Background(), f.org, RunOptions{})
	if res.Digest != "sent" {
		t.Fatalf("digest not sent: %+v", res)
	}
	if res2 := f.s.RunOrg(context.Background(), f.org, RunOptions{}); res2.Digest != "" {
		t.Fatalf("digest re-sent: %+v", res2)
	}
	// Track mode: the imported bill (Expenses) is matched, not duplicated.
	if _, err := f.s.Fin.CreateBill(f.org, "u1", finance.BillInput{VendorID: f.vendor, BillDate: "2026-10-06", DueDate: "2026-10-06",
		LineItems: []finance.BillLineInput{{Description: "Google Workspace Oct", Quantity: 1, UnitPrice: 36}}}); err != nil {
		t.Fatal(err)
	}
	g := f.s.Generate(f.org)
	if g.Matched != 1 || g.Created != 0 {
		t.Fatalf("track: %+v", g)
	}
	var n int64
	f.db.Model(&finance.Bill{}).Where("organization_id = ?", f.org).Count(&n)
	if n != 2 {
		t.Fatalf("bills %d", n)
	}
	// Track mode periods can be marked paid without a bill.
	if err := f.s.MarkPaid(f.org, "u1", r.ID, "2026-11-05", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.s.MarkPaid(f.org, "u1", r.ID, "2026-11-06", ""); err == nil {
		t.Fatal("non-period accepted")
	}
}

func TestSnoozeProjectedThenGenerate(t *testing.T) {
	f := setup(t, "AUD")
	f.at("2026-10-01")
	r, err := f.s.Create(f.org, "u1", Input{Name: "Rent", VendorID: f.vendor, Amount: 1000, Frequency: FreqMonthly, StartDate: "2026-10-20", CreateAs: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	// Snoozing a period that has no bill yet doesn't create one early.
	if _, err := f.s.Snooze(f.org, "", r.ID, "2026-10-20", 0, "2026-10-16"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.bills(t)); n != 0 {
		t.Fatalf("snooze created %d bills", n)
	}
	f.at("2026-10-13")
	res := f.s.RunOrg(context.Background(), f.org, RunOptions{})
	if res.Generated.Created != 1 || res.Reminders != 0 {
		t.Fatalf("snoozed run: %+v", res)
	}
	f.at("2026-10-16")
	if res := f.s.RunOrg(context.Background(), f.org, RunOptions{}); res.Reminders != 1 {
		t.Fatalf("after snooze: %+v", res)
	}
	if res := f.s.RunOrg(context.Background(), f.org, RunOptions{}); res.Generated.Created != 0 || res.Reminders != 0 {
		t.Fatalf("rerun: %+v", res)
	}
}
