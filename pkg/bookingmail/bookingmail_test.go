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

func TestBuildICS(t *testing.T) {
	b := bk("A", "scheduled", time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC))
	b.ID = "4f1c"
	ics := BuildICS(ICSInput{Booking: b, Method: "REQUEST", Sequence: 2, Summary: "Full service; oil — Jane, Doe", Description: "Line 1\nLine 2",
		OrganizerEmail: "bookings@shop.example", AttendeeName: "Sam Staff", AttendeeEmail: "sam@shop.example", Now: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	for _, s := range []string{"METHOD:REQUEST\r\n", "UID:booking-4f1c@dasyin-erp\r\n", "SEQUENCE:2\r\n", "DTSTART:20261006T070000Z\r\n", "DTEND:20261006T083000Z\r\n",
		`SUMMARY:Full service\; oil — Jane\, Doe`, `DESCRIPTION:Line 1\nLine 2`, "STATUS:CONFIRMED", "BEGIN:VALARM", `ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;CN="Sam Staff":mailto:sam@shop.example`} {
		if !strings.Contains(strings.ReplaceAll(ics, "\r\n ", ""), s) { // unfolded
			t.Fatalf("ics lacks %q:\n%s", s, ics)
		}
	}
	c := BuildICS(ICSInput{Booking: b, Method: "CANCEL", Sequence: 3, Summary: "x"})
	if !strings.Contains(c, "METHOD:CANCEL") || !strings.Contains(c, "STATUS:CANCELLED") || !strings.Contains(c, "UID:booking-4f1c@dasyin-erp") || strings.Contains(c, "VALARM") {
		t.Fatalf("cancel ics:\n%s", c)
	}
	for _, l := range strings.Split(ics, "\r\n") {
		if len(l) > 75 {
			t.Fatalf("unfolded line %q", l)
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
	if err := db.AutoMigrate(append(Models(), &notify.Notification{})...); err != nil {
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
	n := &Notifier{DB: db, Mail: ms, AppURL: "https://erp.example", Async: true, Retries: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond},
		Now: func() time.Time { return time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC) }}
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
	if m.CalendarMethod != "REQUEST" || len(m.Attachments) != 1 || m.Attachments[0].Filename != "booking.ics" ||
		!strings.Contains(m.Calendar, "SEQUENCE:0") || !strings.Contains(m.Calendar, "UID:booking-bk-1@dasyin-erp") || !strings.Contains(m.Calendar, "ORGANIZER;CN=\"Nairobi Auto\":mailto:bookings@shop.example") {
		t.Fatalf("create ics:\n%s", m.Calendar)
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
