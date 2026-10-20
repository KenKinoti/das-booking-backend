package bookingmail

import (
	"context"
	"errors"
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

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

func sp(s string) *string { return &s }

func bk(staff string, status string, start time.Time) *models.Booking {
	b := &models.Booking{ID: "b1", OrganizationID: "o1", Status: status, StartTime: start, EndTime: start.Add(90 * time.Minute)}
	if staff != "" {
		b.StaffID = sp(staff)
	}
	return b
}

func plans(p []Plan) string {
	out := []string{}
	for _, x := range p {
		out = append(out, x.UserID+":"+x.Event+":"+x.Method)
	}
	return strings.Join(out, ",")
}

func TestPlanEmails(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC)
	cases := []struct {
		name          string
		before, after *models.Booking
		want          string
	}{
		{"create", nil, bk("A", "scheduled", t0), "A:assigned:REQUEST"},
		{"create unassigned", nil, bk("", "scheduled", t0), ""},
		{"create cancelled", nil, bk("A", "cancelled", t0), ""},
		{"reschedule", bk("A", "scheduled", t0), bk("A", "scheduled", t0.Add(time.Hour)), "A:rescheduled:REQUEST"},
		{"reassign", bk("A", "confirmed", t0), bk("B", "confirmed", t0), "A:unassigned:CANCEL,B:assigned:REQUEST"},
		{"reassign and move", bk("A", "confirmed", t0), bk("B", "confirmed", t0.Add(time.Hour)), "A:unassigned:CANCEL,B:assigned:REQUEST"},
		{"assign", bk("", "scheduled", t0), bk("B", "scheduled", t0), "B:assigned:REQUEST"},
		{"unassign", bk("A", "scheduled", t0), bk("", "scheduled", t0), "A:unassigned:CANCEL"},
		{"cancel", bk("A", "scheduled", t0), bk("A", "cancelled", t0), "A:cancelled:CANCEL"},
		{"cancel + reassign", bk("A", "scheduled", t0), bk("B", "cancelled", t0), "B:cancelled:CANCEL,A:cancelled:CANCEL"},
		{"no-show", bk("A", "confirmed", t0), bk("A", "no_show", t0), "A:no_show:"},
		{"reopen", bk("A", "cancelled", t0), bk("A", "scheduled", t0), "A:assigned:REQUEST"},
		{"complete", bk("A", "in_progress", t0), bk("A", "completed", t0), ""},
		{"notes only", bk("A", "scheduled", t0), bk("A", "confirmed", t0), ""},
		{"delete", bk("A", "scheduled", t0), nil, "A:cancelled:CANCEL"},
		{"delete cancelled", bk("A", "cancelled", t0), nil, ""},
	}
	for _, c := range cases {
		if got := plans(PlanEmails(c.before, c.after)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- with a database

func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`DROP SCHEMA IF EXISTS bookingmail_test CASCADE`)
	boot.Exec(`CREATE SCHEMA bookingmail_test`)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=bookingmail_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(append(append(Models(), &notify.Notification{}), emailprefs.Models()...)...); err != nil {
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
	// Bookings and what their calendar entries show (calendar files, feeds).
	// Plain SQL: AutoMigrate would pull in every related model.
	for _, q := range []string{
		`CREATE TABLE customers (id varchar(255) primary key, first_name text, last_name text, email text, phone text, organization_id varchar(255), created_at timestamptz, updated_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE vehicles (id varchar(255) primary key, customer_id varchar(255), organization_id varchar(255), make text, model text, year int, license_plate text, created_at timestamptz, updated_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE services (id varchar(255) primary key, organization_id varchar(255), name text, duration int, price numeric(10,2), created_at timestamptz, updated_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE bookings (id varchar(255) primary key, customer_id varchar(255), organization_id varchar(255), vehicle_id varchar(255), staff_id varchar(255), start_time timestamptz, end_time timestamptz,
			status varchar(50), total_price numeric(10,2) default 0, notes text, internal_notes text, created_at timestamptz, updated_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE booking_services (booking_id varchar(255), service_id varchar(255), primary key (booking_id, service_id))`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

type box struct {
	mu    sync.Mutex
	msgs  []mail.Message
	fails int
}

func TestNotifierRecipientsICSAndTimezone(t *testing.T) {
	db := testDB(t)
	t.Setenv("SMTP_HOST", "smtp.test.invalid")
	t.Setenv("SMTP_FROM", "Shop <bookings@shop.example>")
	org := uuid.NewString()
	db.Exec(`INSERT INTO organizations (id, name, address_street, address_suburb, country_code, currency, timezone, region_source) VALUES (?, 'Nairobi Auto', '12 Ngong Rd', 'Kilimani', 'KE', 'KES', 'Africa/Nairobi', 'user')`, org)
	a, b, noMail, optOut := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id, created_at) VALUES
		(?, 'amina@shop.example', 'care_worker', 'Amina', 'Otieno', ?, now()), (?, 'brian@shop.example', 'care_worker', 'Brian', 'Kip', ?, now()),
		(?, '', 'care_worker', 'No', 'Mail', ?, now()), (?, 'opt@shop.example', 'care_worker', 'Opt', 'Out', ?, now())`, a, org, b, org, noMail, org, optOut, org)
	db.Create(&UserPref{UserID: optOut, OrganizationID: org, BookingEmails: false})
	// The old "Email me about my bookings" choice moves into Who gets emails at start-up.
	if n, err := emailprefs.MigrateLegacyBookingPrefs(db); err != nil || n != 1 {
		t.Fatalf("legacy prefs moved %d: %v", n, err)
	}
	out := &box{}
	ms := mail.NewService(db)
	ms.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error {
		out.mu.Lock()
		defer out.mu.Unlock()
		if out.fails > 0 {
			out.fails--
			return errors.New("421 try again later")
		}
		out.msgs = append(out.msgs, m)
		return nil
	}
	n := &Notifier{DB: db, Mail: ms, AppURL: "https://erp.example", APIURL: "https://api.erp.example", Secret: []byte("test-secret-0123456789abcdef"), Async: true,
		Retries: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}, Now: func() time.Time { return time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC) }}
	start := time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC) // 10:00 in Nairobi
	mk := func(staff string, st time.Time, status string) *models.Booking {
		bb := &models.Booking{ID: "bk-1", OrganizationID: org, Status: status, StartTime: st, EndTime: st.Add(90 * time.Minute), StaffID: sp(staff), Notes: "Bring the spare key",
			Customer: models.Customer{FirstName: "Jane", LastName: "Wanjiru", Phone: "+254 700 111222"},
			Services: []models.Service{{Name: "Full service"}, {Name: "Oil change"}}, Vehicle: &models.Vehicle{Make: "Toyota", Model: "Axio", Year: 2016, LicensePlate: "KDA 123A"}}
		return bb
	}
	wait := func(k int) []mail.Message {
		n.Wait()
		out.mu.Lock()
		defer out.mu.Unlock()
		if len(out.msgs) != k {
			t.Fatalf("want %d emails, got %d", k, len(out.msgs))
		}
		return append([]mail.Message{}, out.msgs...)
	}

	// Created: Amina gets an invitation (SMTP fails once, the retry delivers it).
	out.fails = 1
	n.Handle(nil, mk(a, start, "scheduled"), "actor")
	m := wait(1)[0]
	if m.Subject != "New booking: Jane Wanjiru — Full service, Oil change, Tue 6 Oct 10:00" || m.To[0] != `"Amina Otieno" <amina@shop.example>` {
		t.Fatalf("create: %q %v", m.Subject, m.To)
	}
	for _, s := range []string{"+254 700 111222", "10:00 – 11:30 (Africa/Nairobi)", "1 h 30 min", "2016 Toyota Axio · KDA 123A", "12 Ngong Rd, Kilimani", "Open booking",
		"https://erp.example/bookings?booking=bk-1", "Bring the spare key"} {
		if !strings.Contains(m.HTML, s) {
			t.Fatalf("create email lacks %q", s)
		}
	}
	// Inline part: an invitation (REQUEST) from the organisation to the staff member.
	if m.CalendarMethod != "REQUEST" || !strings.Contains(m.Calendar, "METHOD:REQUEST\r\n") || !strings.Contains(m.Calendar, "SEQUENCE:0") || !strings.Contains(m.Calendar, "UID:booking-bk-1@dasyin-erp") ||
		!strings.Contains(m.Calendar, "ORGANIZER;CN=\"Nairobi Auto\":mailto:bookings@shop.example") {
		t.Fatalf("create invitation:\n%s", m.Calendar)
	}
	_, inv := oneEvent(t, m.Calendar)
	if a := inv.all("ATTENDEE"); len(a) != 1 || a[0].Value != "mailto:amina@shop.example" || !strings.Contains(a[0].Params, "PARTSTAT=NEEDS-ACTION;RSVP=FALSE") {
		t.Fatalf("invitation attendee %+v", a)
	}
	// Attachment: the add-to-calendar file (PUBLISH, nobody invited), same UID and SEQUENCE.
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "booking-2026-10-06-1000-jane-wanjiru.ics" || m.Attachments[0].ContentType != "text/calendar; charset=utf-8; method=PUBLISH" {
		t.Fatalf("attachment %+v", m.Attachments)
	}
	att := string(m.Attachments[0].Data)
	acal, aev := oneEvent(t, att)
	if acal.get(t, "METHOD") != "PUBLISH" || aev.get(t, "UID") != inv.get(t, "UID") || aev.get(t, "SEQUENCE") != "0" || strings.Contains(att, "ORGANIZER") || strings.Contains(att, "ATTENDEE") ||
		len(aev.subs("VALARM")) != 1 || aev.subs("VALARM")[0].get(t, "TRIGGER") != "-PT30M" {
		t.Fatalf("attachment:\n%s", att)
	}
	// "Add to calendar" buttons: Google, Outlook, and the signed public .ics for Apple / other apps.
	pub := "https://api.erp.example/api/v1/public/bookings/bk-1/calendar.ics?t=" + LinkToken(n.Secret, org, "bk-1")
	for _, s := range []string{"Add to your calendar", ">Google Calendar</a>", ">Outlook</a>", ">Apple / other</a>", "Outlook.com (personal account)",
		"https://calendar.google.com/calendar/render?action=TEMPLATE&amp;text=Full%20service%2C%20Oil%20change%20%E2%80%94%20Jane%20Wanjiru&amp;dates=20261006T070000Z/20261006T083000Z",
		"https://outlook.office.com/calendar/0/deeplink/compose?path=/calendar/action/compose&amp;rru=addevent&amp;subject=", "startdt=2026-10-06T07%3A00%3A00Z&amp;enddt=2026-10-06T08%3A30%3A00Z",
		"https://outlook.live.com/calendar/0/deeplink/compose?", strings.ReplaceAll(pub, "&", "&amp;"), "open the attached calendar file (.ics) and choose Add"} {
		if !strings.Contains(m.HTML, s) {
			t.Fatalf("create email lacks %q", s)
		}
	}
	for _, s := range []string{"Add to your calendar\nGoogle Calendar: https://calendar.google.com/calendar/render?action=TEMPLATE&text=", "\nOutlook: https://outlook.office.com/calendar/0/deeplink/compose?",
		"\nApple / other: " + pub + "\n", "\nOutlook.com (personal account): https://outlook.live.com/"} {
		if !strings.Contains(m.Text, s) {
			t.Fatalf("plain text lacks %q:\n%s", s, m.Text)
		}
	}
	var lg Log
	db.Where("booking_id = ? AND event = ?", "bk-1", EvAssigned).First(&lg)
	if lg.Status != "sent" || lg.Attempts != 2 {
		t.Fatalf("log %+v", lg)
	}

	// Rescheduled: same UID, SEQUENCE 1.
	n.Handle(mk(a, start, "scheduled"), mk(a, start.Add(2*time.Hour), "scheduled"), "actor")
	m = wait(2)[1]
	if !strings.HasPrefix(m.Subject, "Booking rescheduled: Jane Wanjiru — now Tue 6 Oct 12:00") || !strings.Contains(m.Calendar, "SEQUENCE:1") || m.CalendarMethod != "REQUEST" {
		t.Fatalf("reschedule: %q\n%s", m.Subject, m.Calendar)
	}

	// Reassigned A → B: A gets CANCEL, B gets REQUEST, both SEQUENCE 2.
	n.Handle(mk(a, start, "scheduled"), mk(b, start, "scheduled"), "actor")
	ms2 := wait(4)[2:]
	got := map[string]mail.Message{}
	for _, x := range ms2 {
		got[x.To[0]] = x
	}
	ca, cb := got[`"Amina Otieno" <amina@shop.example>`], got[`"Brian Kip" <brian@shop.example>`]
	if ca.CalendarMethod != "CANCEL" || !strings.Contains(ca.Calendar, "STATUS:CANCELLED") || !strings.Contains(ca.Subject, "no longer yours") || !strings.Contains(ca.Calendar, "SEQUENCE:2") {
		t.Fatalf("unassigned: %q\n%s", ca.Subject, ca.Calendar)
	}
	// The cancellation attachment removes the same event; no "add to calendar" buttons.
	xcal, xev := oneEvent(t, string(ca.Attachments[0].Data))
	if xcal.get(t, "METHOD") != "CANCEL" || xev.get(t, "STATUS") != "CANCELLED" || xev.get(t, "UID") != "booking-bk-1@dasyin-erp" || xev.get(t, "SEQUENCE") != "2" ||
		ca.Attachments[0].ContentType != "text/calendar; charset=utf-8; method=CANCEL" || ca.Attachments[0].Filename != "booking-2026-10-06-1000-jane-wanjiru-cancelled.ics" {
		t.Fatalf("cancel attachment %+v\n%s", ca.Attachments[0].Filename, ca.Attachments[0].Data)
	}
	if strings.Contains(ca.HTML, "Google Calendar") || strings.Contains(ca.HTML, "calendar.ics?t=") || strings.Contains(ca.Text, "Add to your calendar") || !strings.Contains(ca.HTML, "the attached update removes it from your calendar") {
		t.Fatalf("cancellation email offers add-to-calendar links")
	}
	if _, bev := oneEvent(t, string(cb.Attachments[0].Data)); bev.get(t, "SEQUENCE") != "2" || bev.get(t, "STATUS") != "CONFIRMED" {
		t.Fatalf("assigned attachment")
	}
	if cb.CalendarMethod != "REQUEST" || !strings.Contains(cb.Calendar, "SEQUENCE:2") || !strings.HasPrefix(cb.Subject, "New booking:") {
		t.Fatalf("assigned: %q", cb.Subject)
	}
	var notes int64
	db.Model(&notify.Notification{}).Where("user_id IN ?", []string{a, b}).Count(&notes)
	if notes != 3 { // rescheduled (A), unassigned (A), assigned (B)
		t.Fatalf("in-app notifications %d", notes)
	}

	// Cancelled → CANCEL to Brian.
	n.Handle(mk(b, start, "scheduled"), mk(b, start, "cancelled"), "actor")
	m = wait(5)[4]
	if m.CalendarMethod != "CANCEL" || !strings.HasPrefix(m.Subject, "Booking cancelled:") || !strings.Contains(m.Calendar, "SEQUENCE:3") {
		t.Fatalf("cancel: %q", m.Subject)
	}

	// No email address / opted out: skipped and logged, nothing sent.
	n.Handle(nil, mk(noMail, start, "scheduled"), "actor")
	n.Handle(nil, mk(optOut, start, "scheduled"), "actor")
	wait(5)
	var skipped []Log
	db.Where("status = ?", "skipped").Find(&skipped)
	if len(skipped) != 2 {
		t.Fatalf("skipped %+v", skipped)
	}
	// Organisation switch off: nothing planned.
	db.Save(&Settings{OrganizationID: org, StaffEmails: false, AgendaHour: 7})
	if p := n.Handle(nil, mk(a, start, "scheduled"), "actor"); p != nil {
		t.Fatalf("org off still planned %v", p)
	}
}
