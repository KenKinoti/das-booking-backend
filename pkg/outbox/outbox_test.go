package outbox

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// ---------------------------------------------------------------- harness

type fakeReleaser struct {
	mu       sync.Mutex
	sent     []string
	recheck  map[string]string
	failNext error
	closed   []string
}

func (f *fakeReleaser) Recheck(ctx context.Context, it *Item) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recheck[it.ID]
}

func (f *fakeReleaser) Send(ctx context.Context, it *Item) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return Result{}, err
	}
	f.sent = append(f.sent, it.ID+"|"+it.Subject)
	return Result{Text: "Sent to " + it.To, Ref: "ref-" + it.ID}, nil
}

func (f *fakeReleaser) Preview(ctx context.Context, it *Item) Preview {
	return Preview{Subject: it.Subject, To: it.To, HTML: "<html>" + it.Message + "</html>", Text: it.Message}
}

func (f *fakeReleaser) Closed(ctx context.Context, it *Item) {
	f.mu.Lock()
	f.closed = append(f.closed, it.ID+"|"+it.Status)
	f.mu.Unlock()
}

func (f *fakeReleaser) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type env struct {
	db    *gorm.DB
	s     *Service
	org   string
	owner string
	staff string
	rel   *fakeReleaser
	mu    *sync.Mutex
	mails *[]mail.Message
	clock *time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	base, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	base.Exec(`DROP SCHEMA IF EXISTS outbox_test CASCADE`)
	base.Exec(`CREATE SCHEMA outbox_test`)
	if sq, err := base.DB(); err == nil {
		sq.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=outbox_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if sq, err := db.DB(); err == nil {
		t.Cleanup(func() { sq.Close() })
	}
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) primary key, name text, country_code text, currency text, timezone text, region_source text, address_country text, created_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE users (id varchar(255) primary key, email text, role text, first_name text, last_name text, organization_id varchar(255), is_active boolean default true,
			created_at timestamptz default now(), deleted_at timestamptz)`,
		`CREATE TABLE audit_logs (id varchar(36) primary key, organization_id varchar(36), user_id varchar(36), action varchar(100), entity_type varchar(50), entity_id varchar(36),
			old_values jsonb, new_values jsonb, ip_address varchar(45), user_agent varchar(500), created_at timestamptz)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(&notify.Notification{}); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, org: uuid.NewString(), owner: uuid.NewString(), staff: uuid.NewString(), rel: &fakeReleaser{recheck: map[string]string{}}, mu: &sync.Mutex{}, mails: &[]mail.Message{}}
	db.Exec(`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES (?, 'Mvua Traders', 'KE', 'KES', 'Africa/Nairobi', 'admin')`, e.org)
	db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id) VALUES (?, 'amina.owner@mvua.test', 'owner', 'Amina', 'Wekesa', ?)`, e.owner, e.org)
	db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id) VALUES (?, 'jabali.staff@mvua.test', 'staff', 'Jabali', 'Otieno', ?)`, e.staff, e.org)
	enc, _ := mail.EncryptSecret("pw")
	_ = enc
	ms := mail.NewService(db)
	ms.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error {
		e.mu.Lock()
		*e.mails = append(*e.mails, m)
		e.mu.Unlock()
		return nil
	}
	t.Setenv("SMTP_HOST", "smtp.env.test")
	t.Setenv("SMTP_PORT", "587")
	t.Setenv("SMTP_FROM", "noreply@env.test")
	t.Setenv("SMTP_USERNAME", "u")
	t.Setenv("SMTP_PASSWORD", "p")
	ms.Invalidate()
	now := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC) // 09:00 in Nairobi
	e.clock = &now
	e.s = &Service{DB: db, Mail: ms, AppURL: "https://app.example.com", Now: func() time.Time { return *e.clock }}
	Install(e.s)
	t.Cleanup(Uninstall)
	Register("test", e.rel)
	notify.Disabled = true
	t.Cleanup(func() { notify.Disabled = false })
	return e
}

func (e *env) at(t time.Time) { *e.clock = t }

func (e *env) item(t *testing.T, mut func(*Item)) *Item {
	t.Helper()
	it := &Item{OrganizationID: e.org, Kind: KindInvoiceEmail, Module: "test", Channel: ChanEmail, To: "wanjiru@kamau.test", ToName: "Wanjiru Kamau",
		Subject: "Invoice INV-1001", Message: "Hi Wanjiru, here is your invoice.", Editable: []string{FieldTo, FieldCC, FieldBCC, FieldSubject, FieldMessage},
		RecordNumber: "INV-1001", CustomerName: "Kamau Traders", CreatedBy: e.staff}
	if mut != nil {
		mut(it)
	}
	out, err := Enqueue(nil, it)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *env) ownerA() Actor { return Actor{Org: e.org, User: e.owner, Role: "owner"} }
func (e *env) staffA() Actor { return Actor{Org: e.org, User: e.staff, Role: "staff"} }

func (e *env) reload(t *testing.T, id string) *Item {
	t.Helper()
	it, err := e.s.load(e.org, id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *env) sentMails() []mail.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]mail.Message{}, *e.mails...)
}

// ---------------------------------------------------------------- the gate

func TestGateDecide(t *testing.T) {
	e := setup(t)
	if d := Decide(e.org, Who{UserID: e.staff}, true); !d.Wait || d.SentNow {
		t.Fatalf("staff (even with send_now) waits: %+v", d)
	}
	if d := Decide(e.org, Who{UserID: e.owner}, false); !d.Wait {
		t.Fatalf("owner without the tick waits: %+v", d)
	}
	if d := Decide(e.org, Who{UserID: e.owner}, true); d.Wait || !d.SentNow {
		t.Fatalf("owner with the tick sends now: %+v", d)
	}
	if d := Decide(e.org, Who{}, false); !d.Wait {
		t.Fatalf("automatic sends wait: %+v", d)
	}
	if d := LiveReply(e.org, Who{UserID: e.owner, Role: "owner"}); d.Wait {
		t.Fatalf("an approver's live reply goes: %+v", d)
	}
	if d := LiveReply(e.org, Who{UserID: e.staff, Role: "staff"}); !d.Wait {
		t.Fatalf("staff live reply waits: %+v", d)
	}
	// Admins can't send at once when the setting says so.
	if _, err := e.s.SaveSettings(e.ownerA(), func(st *Settings) error { st.AdminsCanSendNow = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if d := Decide(e.org, Who{UserID: e.owner}, true); !d.Wait {
		t.Fatalf("admins_can_send_now off: waits: %+v", d)
	}
	// Review off: everything goes as before — and a saved false stays false.
	if _, err := e.s.SaveSettings(e.ownerA(), func(st *Settings) error { st.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if st := e.s.SettingsFor(e.org); st.Enabled || !st.saved {
		t.Fatalf("saved off: %+v", st)
	}
	if d := Decide(e.org, Who{UserID: e.staff}, false); d.Wait || d.Reviewing {
		t.Fatalf("review off: %+v", d)
	}
	// Not installed: off.
	Uninstall()
	if d := Decide(e.org, Who{UserID: e.staff}, false); d.Wait || Active() {
		t.Fatalf("not installed: %+v", d)
	}
	Install(e.s)
	// OUTBOX_DEFAULT=off: organisations without a saved row start off.
	other := uuid.NewString()
	e.s.DefaultOff = true
	if e.s.Enabled(other) {
		t.Fatal("DefaultOff")
	}
	e.s.DefaultOff = false
	if !e.s.Enabled(other) {
		t.Fatal("on by default for every organisation")
	}
}

func TestPermissions(t *testing.T) {
	e := setup(t)
	if !e.s.CanApprove(e.org, e.owner, "") || e.s.CanApprove(e.org, e.staff, "") {
		t.Fatal("legacy roles (looked up)")
	}
	for role, want := range map[string]bool{"owner": true, "admin": true, "org_admin": true, "super_admin": true, "manager": false, "staff": false} {
		if got := e.s.CanApprove(e.org, "x", role); got != want {
			t.Fatalf("%s: %v", role, got)
		}
	}
	// Business-type roles: base admin, or an explicit outbox approve grant (not the * wildcard).
	RoleOf = func(org, user, legacy string) (string, map[string][]string, bool) {
		switch user {
		case "mgr":
			return "manager", map[string][]string{"*": {"view", "create", "edit", "delete", "approve"}}, true
		case "clerk":
			return "staff", map[string][]string{"outbox": {"view", "approve"}}, true
		case "boss":
			return "admin", nil, true
		}
		return "", nil, false
	}
	t.Cleanup(func() { RoleOf = nil })
	if e.s.CanApprove(e.org, "mgr", "manager") || !e.s.CanApprove(e.org, "clerk", "staff") || !e.s.CanApprove(e.org, "boss", "staff") {
		t.Fatal("role permissions")
	}
	it := e.item(t, nil)
	if _, err := e.s.Update(e.staffA(), it.ID, Edits{Subject: strp("x")}); !isCode(err, "FORBIDDEN") {
		t.Fatalf("staff edit: %v", err)
	}
	if _, _, err := e.s.Approve(context.Background(), e.staffA(), it.ID, "now", nil, ""); !isCode(err, "FORBIDDEN") {
		t.Fatalf("staff approve: %v", err)
	}
	if _, err := e.s.Reject(context.Background(), e.staffA(), it.ID, ""); !isCode(err, "FORBIDDEN") {
		t.Fatalf("staff reject: %v", err)
	}
	if _, err := e.s.Bulk(context.Background(), e.staffA(), "approve", []string{it.ID}, "", ""); !isCode(err, "FORBIDDEN") {
		t.Fatalf("staff bulk: %v", err)
	}
	// Staff see only their own items; approvers everything.
	other := e.item(t, func(i *Item) { i.CreatedBy = "" })
	rows, total, _ := e.s.List(e.staffA(), Query{Tab: "waiting"})
	if total != 1 || rows[0].ID != it.ID {
		t.Fatalf("staff list: %d %+v", total, rows)
	}
	_, total, _ = e.s.List(e.ownerA(), Query{Tab: "waiting"})
	if total != 2 {
		t.Fatalf("owner list: %d", total)
	}
	_ = other
}

func strp(s string) *string { return &s }

func isCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// ---------------------------------------------------------------- items

func TestEnqueueEditsAndAudit(t *testing.T) {
	e := setup(t)
	it := e.item(t, func(i *Item) { i.Key = "k1"; i.CC = []string{"accounts@kamau.test"} })
	dup := e.item(t, func(i *Item) { i.Key = "k1" })
	if dup.ID != it.ID {
		t.Fatal("same key → same item")
	}
	if it.Trigger != "Sent by Jabali Otieno" || it.TriggerKind != TriggerManual || it.Status != StatusPending {
		t.Fatalf("defaults: %q %q %q", it.Trigger, it.TriggerKind, it.Status)
	}
	auto := e.item(t, func(i *Item) { i.CreatedBy = ""; i.Trigger = "" })
	if auto.TriggerKind != TriggerAutomatic || auto.Trigger != "Automatic" {
		t.Fatalf("automatic: %+v", auto)
	}
	// Validation.
	for _, c := range []struct {
		e    Edits
		code string
		f    string
	}{
		{Edits{To: strp("")}, "VALIDATION_ERROR", "to"},
		{Edits{To: strp("not an email")}, "VALIDATION_ERROR", "to"},
		{Edits{CC: &[]string{"x@"}}, "VALIDATION_ERROR", "cc"},
		{Edits{Subject: strp("  ")}, "VALIDATION_ERROR", "subject"},
		{Edits{Message: strp(" ")}, "VALIDATION_ERROR", "message"},
		{Edits{WhatsAppText: strp("hi")}, "NOT_EDITABLE", "whatsapp_text"},
	} {
		_, err := e.s.Update(e.ownerA(), it.ID, c.e)
		var ae *Error
		if !errors.As(err, &ae) || ae.Code != c.code || ae.Field != c.f {
			t.Fatalf("%+v: %v", c.e, err)
		}
	}
	// No change, no event.
	if _, err := e.s.Update(e.ownerA(), it.ID, Edits{Subject: strp(it.Subject)}); err != nil {
		t.Fatal(err)
	}
	var n int64
	e.db.Model(&Event{}).Where("item_id = ? AND action = ?", it.ID, "edited").Count(&n)
	if n != 0 {
		t.Fatal("unchanged value is not an edit")
	}
	out, err := e.s.Update(e.ownerA(), it.ID, Edits{Subject: strp("Your invoice INV-1001"), CC: &[]string{"accounts@kamau.test", "boss@kamau.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Edited || out.Subject != "Your invoice INV-1001" || len(out.CC) != 2 {
		t.Fatalf("edited: %+v", out)
	}
	var ev Event
	e.db.Where("item_id = ? AND action = ?", it.ID, "edited").Take(&ev)
	if ev.UserID != e.owner || ev.UserName != "Amina Wekesa" || ev.Changes["subject"].To != "Your invoice INV-1001" || !strings.Contains(ev.Text, "changed the CC and the subject") {
		t.Fatalf("audit: %+v", ev)
	}
	// SMS length; WhatsApp parameter count.
	sms := e.item(t, func(i *Item) { i.Channel, i.To, i.Editable = ChanSMS, "+254712000001", []string{FieldTo, FieldMessage} })
	if _, err := e.s.Update(e.ownerA(), sms.ID, Edits{Message: strp(strings.Repeat("a", 919))}); !isCode(err, "VALIDATION_ERROR") {
		t.Fatalf("sms length: %v", err)
	}
	if _, err := e.s.Update(e.ownerA(), sms.ID, Edits{To: strp("hello")}); !isCode(err, "VALIDATION_ERROR") {
		t.Fatalf("sms number: %v", err)
	}
	waIt := e.item(t, func(i *Item) {
		i.Channel, i.To, i.WAMode, i.WATemplate, i.WAParams, i.WAParamCount, i.Editable = ChanWhatsApp, "+254712000002", "template", "payment_reminder", []string{"a", "b"}, 2, []string{FieldWAParams}
	})
	if _, err := e.s.Update(e.ownerA(), waIt.ID, Edits{WhatsAppParams: &[]string{"only one"}}); !isCode(err, "VALIDATION_ERROR") {
		t.Fatalf("param count: %v", err)
	}
	if got, err := e.s.Update(e.ownerA(), waIt.ID, Edits{WhatsAppParams: &[]string{" Baraka ", "INV-9"}}); err != nil || got.WAParams[0] != "Baraka" {
		t.Fatalf("params: %v %+v", err, got)
	}
	// Detail: preview, history, actions.
	d := e.s.DetailOf(context.Background(), e.reload(t, it.ID), true)
	if d.Preview.HTML == "" || len(d.History) != 2 || !d.Actions["approve_now"] || d.Actions["approve_scheduled"] || d.Actions["retry"] {
		t.Fatalf("detail: %+v", d)
	}
	ds := e.s.DetailOf(context.Background(), e.reload(t, it.ID), false)
	for k, v := range ds.Actions {
		if v {
			t.Fatalf("non-approver action %s", k)
		}
	}
}

func TestApproveNowScheduledRejectRetry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := e.item(t, nil)
	out, outcome, err := e.s.Approve(ctx, e.ownerA(), a.ID, "now", &Edits{Subject: strp("Edited subject")}, "")
	if err != nil || outcome != OutSent || out.Status != StatusSent || out.ResultRef != "ref-"+a.ID || e.rel.sentCount() != 1 || e.rel.sent[0] != a.ID+"|Edited subject" {
		t.Fatalf("approve now: %v %s %+v %v", err, outcome, out, e.rel.sent)
	}
	if _, _, err := e.s.Approve(ctx, e.ownerA(), a.ID, "now", nil, ""); !isCode(err, "NOT_PENDING") {
		t.Fatalf("twice: %v", err)
	}
	if e.rel.sentCount() != 1 {
		t.Fatal("exactly once")
	}
	// Scheduled: needs a later send time; released by the engine at it.
	b := e.item(t, nil)
	if _, _, err := e.s.Approve(ctx, e.ownerA(), b.ID, "scheduled", nil, ""); !isCode(err, "NO_SCHEDULED_TIME") {
		t.Fatalf("no time: %v", err)
	}
	later := e.clock.Add(3 * time.Hour)
	c := e.item(t, func(i *Item) { i.SendAt = &later })
	if _, outcome, err := e.s.Approve(ctx, e.ownerA(), c.ID, "scheduled", nil, ""); err != nil || outcome != OutApproved {
		t.Fatalf("scheduled: %v %s", err, outcome)
	}
	if r, _ := e.s.RunOnce(ctx, time.Second); r.Released != 0 {
		t.Fatalf("not before its time: %+v", r)
	}
	// Editing an approved item sends it back for approval.
	if got, _ := e.s.Update(e.ownerA(), c.ID, Edits{Message: strp("New text")}); got.Status != StatusPending || got.ApprovedBy != "" {
		t.Fatalf("edit after approval: %+v", got)
	}
	e.s.Approve(ctx, e.ownerA(), c.ID, "scheduled", nil, "")
	e.at(later.Add(time.Minute))
	if r, _ := e.s.RunOnce(ctx, time.Second); r.Released != 1 || r.Sent != 1 {
		t.Fatalf("released at its time: %+v", r)
	}
	if e.reload(t, c.ID).Status != StatusSent {
		t.Fatal("scheduled sent")
	}
	// Reject: the module is told.
	rj, err := e.s.Reject(ctx, e.ownerA(), b.ID, "Wrong amount")
	if err != nil || rj.Status != StatusRejected || rj.DecisionNote != "Wrong amount" || rj.DecidedByName != "Amina Wekesa" {
		t.Fatalf("reject: %v %+v", err, rj)
	}
	if len(e.rel.closed) != 1 || e.rel.closed[0] != b.ID+"|rejected" {
		t.Fatalf("closed hook: %v", e.rel.closed)
	}
	if _, _, err := e.s.Approve(ctx, e.ownerA(), b.ID, "now", nil, ""); !isCode(err, "NOT_PENDING") {
		t.Fatalf("approve rejected: %v", err)
	}
	// A failed send keeps the error; retry sends it.
	f := e.item(t, nil)
	e.rel.failNext = errors.New("550 mailbox unavailable")
	got, outcome, err := e.s.Approve(ctx, e.ownerA(), f.ID, "now", nil, "")
	if !isCode(err, "SEND_FAILED") || outcome != OutFailed || got.Status != StatusFailed || !strings.Contains(got.Error, "550") {
		t.Fatalf("failed: %v %s %+v", err, outcome, got)
	}
	if got, outcome, err := e.s.Retry(ctx, e.ownerA(), f.ID); err != nil || outcome != OutSent || got.Error != "" {
		t.Fatalf("retry: %v %s %+v", err, outcome, got)
	}
	var acts []string
	e.db.Model(&Event{}).Where("item_id = ?", f.ID).Order("at, id").Pluck("action", &acts)
	if strings.Join(acts, ",") != "queued,approved,failed,retried,sent" {
		t.Fatalf("history: %v", acts)
	}
}

func TestRecheckSupersedesAndBulk(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := e.item(t, func(i *Item) { i.Kind, i.Dated = KindReminderEmail, true })
	e.rel.recheck[a.ID] = "Paid in full — the reminder isn't needed"
	// At release.
	out, outcome, err := e.s.Approve(ctx, e.ownerA(), a.ID, "now", nil, "")
	if err != nil || outcome != OutSuperseded || out.SupersededReason != "Paid in full — the reminder isn't needed" || e.rel.sentCount() != 0 {
		t.Fatalf("superseded at release: %v %s %+v", err, outcome, out)
	}
	// By the engine's re-check of waiting items.
	b := e.item(t, nil)
	e.rel.recheck[b.ID] = "The invoice was voided"
	if r, _ := e.s.RunOnce(ctx, time.Second); r.Superseded != 1 {
		t.Fatalf("engine recheck: %+v", r)
	}
	if e.reload(t, b.ID).Status != StatusSuperseded {
		t.Fatal("b superseded")
	}
	// Bulk: never stops on a failure.
	c, d, f := e.item(t, nil), e.item(t, nil), e.item(t, nil)
	res, err := e.s.Bulk(ctx, e.ownerA(), "approve", []string{c.ID, a.ID, d.ID}, "now", "")
	if err != nil || len(res) != 3 || !res[0].OK || res[1].OK || !res[2].OK || res[0].Outcome != OutSent {
		t.Fatalf("bulk approve: %v %+v", err, res)
	}
	res, err = e.s.Bulk(ctx, e.ownerA(), "reject", []string{f.ID}, "", "Duplicate")
	if err != nil || !res[0].OK || res[0].Status != StatusRejected {
		t.Fatalf("bulk reject: %v %+v", err, res)
	}
	if _, err := e.s.Bulk(ctx, e.ownerA(), "approve", nil, "", ""); !isCode(err, "VALIDATION_ERROR") {
		t.Fatal("empty bulk")
	}
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	if _, err := e.s.Bulk(ctx, e.ownerA(), "approve", ids, "", ""); !isCode(err, "VALIDATION_ERROR") {
		t.Fatal("bulk > 100")
	}
	cnt := e.s.CountsFor(e.ownerA())
	if cnt.Superseded30d != 2 || cnt.Rejected30d != 1 || cnt.Sent7d != 2 || cnt.Waiting != 0 {
		t.Fatalf("counts: %+v", cnt)
	}
	rows, total, _ := e.s.List(e.ownerA(), Query{Tab: "closed"})
	if total != 3 || len(rows) != 3 {
		t.Fatalf("closed tab: %d", total)
	}
}

func TestStaleAndInterrupted(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	rem := e.item(t, func(i *Item) { i.Kind, i.Dated = KindReminderEmail, true })
	inv := e.item(t, nil) // not dated: never stale
	st := e.s.SettingsFor(e.org)
	if e.s.RowOf(rem, st).Stale {
		t.Fatal("not stale yet")
	}
	e.at(e.clock.Add(7*24*time.Hour + time.Minute))
	if r := e.s.RowOf(e.reload(t, rem.ID), st); !r.Stale || r.StaleSince == nil {
		t.Fatal("stale after 7 days")
	}
	if e.s.RowOf(e.reload(t, inv.ID), st).Stale {
		t.Fatal("undated items are never stale")
	}
	r, _ := e.s.RunOnce(ctx, time.Second)
	r2, _ := e.s.RunOnce(ctx, time.Second)
	if r.Stale != 1 || r2.Stale != 0 {
		t.Fatalf("stale event once: %+v %+v", r, r2)
	}
	if c := e.s.CountsFor(e.ownerA()); c.Stale != 1 {
		t.Fatalf("stale count: %+v", c)
	}
	_, total, _ := e.s.List(e.ownerA(), Query{Tab: "waiting", Flag: "stale"})
	if total != 1 {
		t.Fatalf("stale filter: %d", total)
	}
	// Still approvable.
	if _, outcome, err := e.s.Approve(ctx, e.ownerA(), rem.ID, "now", nil, ""); err != nil || outcome != OutSent {
		t.Fatalf("approve stale: %v", err)
	}
	// A send a stopped server left half-done is failed, never repeated.
	x := e.item(t, nil)
	old := e.clock.Add(-11 * time.Minute)
	e.db.Model(&Item{}).Where("id = ?", x.ID).Updates(map[string]any{"status": StatusSending, "claimed_at": old})
	if r, _ := e.s.RunOnce(ctx, time.Second); r.Interrupted != 1 {
		t.Fatalf("interrupted: %+v", r)
	}
	if got := e.reload(t, x.ID); got.Status != StatusFailed || !strings.Contains(got.Error, "Interrupted") {
		t.Fatalf("interrupted item: %+v", got)
	}
	// Time-sensitive.
	soon := e.clock.Add(24 * time.Hour)
	ts := e.item(t, func(i *Item) {
		i.Deadline, i.Blocks, i.Flag = &soon, "suspension", "Blocking a suspension on Wed 25 Nov"
	})
	if r := e.s.RowOf(ts, st); !r.TimeSensitive {
		t.Fatal("time-sensitive")
	}
}

// ---------------------------------------------------------------- update emails

func TestUpdateEmailTiming(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	digests := func(kind string) []mail.Message {
		out := []mail.Message{}
		for _, m := range e.sentMails() {
			if (kind == "soon" && strings.Contains(m.Subject, "waiting for your approval")) || (kind == "daily" && strings.HasPrefix(m.Subject, "Pending review:")) {
				out = append(out, m)
			}
		}
		return out
	}
	// 09:00 Nairobi (after 07:30: today's daily goes with the first run that has something waiting).
	// Start at 02:00 Nairobi to keep the daily out of the way first.
	e.at(time.Date(2026, 11, 24, 23, 0, 0, 0, time.UTC)) // 02:00 on 25 Nov in Nairobi
	a := e.item(t, func(i *Item) { i.RecordNumber = "INV-2001" })
	e.item(t, func(i *Item) { i.RecordNumber = "INV-2002" }) // same batch
	e.at(e.clock.Add(10 * time.Minute))
	if r, _ := e.s.RunOnce(ctx, time.Second); r.SoonEmails != 0 {
		t.Fatalf("not 15 minutes yet: %+v", r)
	}
	e.at(e.clock.Add(6 * time.Minute)) // 16 min after the first
	if r, _ := e.s.RunOnce(ctx, time.Second); r.SoonEmails != 1 {
		t.Fatalf("soon after: %+v", r)
	}
	soon := digests("soon")
	if len(soon) != 1 || soon[0].Subject != "2 messages are waiting for your approval" || !strings.Contains(soon[0].HTML, "https://app.example.com/outbox?open="+a.ID) ||
		!strings.Contains(soon[0].Text, "Review all: https://app.example.com/outbox") || len(soon[0].To) != 1 || soon[0].To[0] != "amina.owner@mvua.test" {
		t.Fatalf("soon email: %+v", soon)
	}
	// One batch = one email; a new item 5 minutes later waits for the hour.
	e.at(e.clock.Add(5 * time.Minute))
	c := e.item(t, func(i *Item) { i.RecordNumber = "INV-2003"; i.Kind = KindQuoteEmail })
	e.at(e.clock.Add(20 * time.Minute)) // 20 min after c, 25 after the last email
	if r, _ := e.s.RunOnce(ctx, time.Second); r.SoonEmails != 0 {
		t.Fatalf("at most one an hour: %+v", r)
	}
	e.at(e.clock.Add(36 * time.Minute)) // 61 min after the last email
	if r, _ := e.s.RunOnce(ctx, time.Second); r.SoonEmails != 1 {
		t.Fatalf("next after an hour: %+v", r)
	}
	soon = digests("soon")
	if len(soon) != 2 || !strings.HasPrefix(soon[1].Subject, "A message is waiting for your approval: Quote email INV-2003") || strings.Contains(soon[1].Text, "INV-2001") ||
		!strings.Contains(soon[1].Text, "(3 waiting in all)") {
		t.Fatalf("second soon email: %+v", soon[1].Subject)
	}
	_ = c
	// Daily at 07:30 Nairobi: not at 07:29, once at 07:30, never twice a day.
	e.at(time.Date(2026, 11, 25, 4, 29, 0, 0, time.UTC)) // 07:29 EAT
	if r, _ := e.s.RunOnce(ctx, time.Second); r.DailyEmails != 0 {
		t.Fatalf("07:29: %+v", r)
	}
	e.at(time.Date(2026, 11, 25, 4, 30, 0, 0, time.UTC)) // 07:30 EAT
	if r, _ := e.s.RunOnce(ctx, time.Second); r.DailyEmails != 1 {
		t.Fatalf("07:30: %+v", r)
	}
	e.at(time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC))
	if r, _ := e.s.RunOnce(ctx, time.Second); r.DailyEmails != 0 {
		t.Fatalf("once a day: %+v", r)
	}
	daily := digests("daily")
	if len(daily) != 1 || daily[0].Subject != "Pending review: 3 messages waiting" || strings.Index(daily[0].Text, "INV-2001") > strings.Index(daily[0].Text, "INV-2003") {
		t.Fatalf("daily: %+v", daily)
	}
	// The organisation's time zone decides (Sydney: 07:30 AEDT = 20:30 UTC the day before).
	e.db.Exec(`UPDATE organizations SET timezone = 'Australia/Sydney' WHERE id = ?`, e.org)
	e.at(time.Date(2026, 11, 25, 20, 29, 0, 0, time.UTC))
	if r, _ := e.s.RunOnce(ctx, time.Second); r.DailyEmails != 0 {
		t.Fatalf("07:29 Sydney: %+v", r)
	}
	e.at(time.Date(2026, 11, 25, 20, 31, 0, 0, time.UTC))
	if r, _ := e.s.RunOnce(ctx, time.Second); r.DailyEmails != 1 {
		t.Fatalf("07:31 Sydney (26 Nov): %+v", r)
	}
	// Settings: daily time and switches.
	if _, err := e.s.SaveSettings(e.ownerA(), func(st *Settings) error { st.Daily, st.SoonAfter = false, false; return nil }); err != nil {
		t.Fatal(err)
	}
	e.item(t, nil)
	e.at(time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC))
	if r, _ := e.s.RunOnce(ctx, time.Second); r.DailyEmails+r.SoonEmails != 0 {
		t.Fatalf("switched off: %+v", r)
	}
	// Nothing waiting: no daily.
	if _, err := e.s.SaveSettings(e.ownerA(), func(st *Settings) error { st.Daily = true; return nil }); err != nil {
		t.Fatal(err)
	}
	e.db.Model(&Item{}).Where("organization_id = ?", e.org).Update("status", StatusSent)
	e.at(time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC))
	if r, _ := e.s.RunOnce(ctx, time.Second); r.DailyEmails != 0 {
		t.Fatalf("nothing waiting: %+v", r)
	}
	// Who gets them follows the EmailsFor hook (Who gets emails).
	EmailsFor = func(db *gorm.DB, org, category string, ids []string) []string {
		if category != Category {
			t.Fatalf("category %s", category)
		}
		return []string{}
	}
	t.Cleanup(func() { EmailsFor = nil })
	if got := e.s.DigestRecipients(e.org); len(got) != 0 {
		t.Fatalf("hook: %v", got)
	}
}

func TestSettingsAndAudit(t *testing.T) {
	e := setup(t)
	st := e.s.SettingsFor(e.org)
	if !st.Enabled || !st.AdminsCanSendNow || st.StaleDays != 7 || st.DailyTime != "07:30" || !st.SoonAfter || !st.Daily || st.saved {
		t.Fatalf("defaults: %+v", st)
	}
	if _, err := e.s.SaveSettings(e.ownerA(), func(s *Settings) error { s.StaleDays, s.DailyTime = 3, "06:45"; return nil }); err != nil {
		t.Fatal(err)
	}
	st = e.s.SettingsFor(e.org)
	if st.StaleDays != 3 || st.DailyTime != "06:45" || !st.Enabled || st.UpdatedByName != "Amina Wekesa" {
		t.Fatalf("saved: %+v", st)
	}
	var n int64
	e.db.Table("audit_logs").Where("organization_id = ? AND action = ?", e.org, "outbox.settings").Count(&n)
	if n != 1 {
		t.Fatalf("settings audited: %d", n)
	}
	SentNow(e.org, e.owner, "invoice", uuid.NewString(), "Invoice email", "x@y.test")
	if c := e.s.CountsFor(e.ownerA()); c.SentNow7d != 1 || !c.CanSendNow {
		t.Fatalf("sent now counted: %+v", c)
	}
}
