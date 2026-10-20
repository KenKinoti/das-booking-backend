package bookingmail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// calEnv is a notifier with a movable clock, an organisation in Nairobi, two
// staff members, an admin and the calendar routes behind a fake sign-in
// (X-Org / X-User / X-Role headers).
type calEnv struct {
	t                   *testing.T
	db                  *gorm.DB
	n                   *Notifier
	r                   *gin.Engine
	now                 time.Time
	org, other          string
	amina, brian, admin string
	customer, otherCust string
	sent                []mail.Message
}

func newCalEnv(t *testing.T) *calEnv {
	db := testDB(t)
	t.Setenv("SMTP_HOST", "smtp.test.invalid")
	t.Setenv("SMTP_FROM", "Shop <bookings@shop.example>")
	e := &calEnv{t: t, db: db, now: time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC), org: uuid.NewString(), other: uuid.NewString(),
		amina: uuid.NewString(), brian: uuid.NewString(), admin: uuid.NewString(), customer: uuid.NewString(), otherCust: uuid.NewString()}
	db.Exec(`INSERT INTO organizations (id, name, address_street, address_suburb, country_code, currency, timezone, region_source) VALUES
		(?, 'Nairobi Auto', '12 Ngong Rd', 'Kilimani', 'KE', 'KES', 'Africa/Nairobi', 'user'), (?, 'Other Co', '', '', 'KE', 'KES', 'Africa/Nairobi', 'user')`, e.org, e.other)
	db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id, created_at) VALUES
		(?, 'amina@shop.example', 'care_worker', 'Amina', 'Otieno', ?, now()), (?, 'brian@shop.example', 'care_worker', 'Brian', 'Kip', ?, now()), (?, 'boss@shop.example', 'admin', 'Boss', 'Admin', ?, now())`,
		e.amina, e.org, e.brian, e.org, e.admin, e.org)
	db.Exec(`INSERT INTO customers (id, first_name, last_name, phone, organization_id, created_at, updated_at) VALUES (?, 'Jane', 'Wanjiru', '+254 700 111222', ?, now(), now()), (?, 'Otto', 'Other', '', ?, now(), now())`,
		e.customer, e.org, e.otherCust, e.other)
	ms := mail.NewService(db)
	ms.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error { e.sent = append(e.sent, m); return nil }
	e.n = &Notifier{DB: db, Mail: ms, AppURL: "https://erp.example", APIURL: "https://api.erp.example", Secret: []byte("test-secret-0123456789abcdef"),
		Now: func() time.Time { return e.now }}
	gin.SetMode(gin.TestMode)
	e.r = gin.New()
	auth := e.r.Group("/api/v1")
	auth.Use(func(c *gin.Context) {
		if c.GetHeader("X-User") == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("user_role", c.GetHeader("X-Role"))
	})
	h := &Handler{N: e.n}
	h.RegisterRoutes(auth)
	h.RegisterCalendarRoutes(auth, e.r.Group("/api/v1/public"))
	return e
}

// booking inserts a booking that starts `in` from now (1 h 30 min long).
func (e *calEnv) booking(org, customer, staff, status string, in time.Duration, updated time.Time) *models.Booking {
	e.t.Helper()
	b := &models.Booking{ID: uuid.NewString(), OrganizationID: org, CustomerID: customer, Status: status, StartTime: e.now.Add(in), EndTime: e.now.Add(in + 90*time.Minute),
		Notes: "Bring the spare key", CreatedAt: updated, UpdatedAt: updated}
	var staffID any
	if staff != "" {
		b.StaffID = sp(staff)
		staffID = staff
	}
	if err := e.db.Exec(`INSERT INTO bookings (id, customer_id, organization_id, staff_id, start_time, end_time, status, notes, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, customer, org, staffID, b.StartTime, b.EndTime, status, b.Notes, updated, updated).Error; err != nil {
		e.t.Fatal(err)
	}
	return b
}

// load returns the booking as the API hands it to the notifier.
func (e *calEnv) load(id string) *models.Booking {
	var b models.Booking
	if err := e.db.Unscoped().Preload("Customer").Preload("Vehicle").Preload("Services").First(&b, "id = ?", id).Error; err != nil {
		e.t.Fatal(err)
	}
	return &b
}

type who struct{ org, user, role string }

func (e *calEnv) do(method, path string, as *who, hdr map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.7:4000"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if as != nil {
		req.Header.Set("X-Org", as.org)
		req.Header.Set("X-User", as.user)
		req.Header.Set("X-Role", as.role)
	}
	for k, v := range hdr {
		if k == "RemoteAddr" {
			req.RemoteAddr = v
			continue
		}
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func (e *calEnv) data(w *httptest.ResponseRecorder) map[string]any {
	e.t.Helper()
	var out struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
		Error   string         `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.Success {
		e.t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	return out.Data
}

// feedPath extracts "/api/v1/public/calendar/<token>.ics" from a feed view.
func feedPath(t *testing.T, feed any) string {
	t.Helper()
	m, _ := feed.(map[string]any)
	u, _ := m["url"].(string)
	if m["enabled"] != true || !strings.HasPrefix(u, "https://api.erp.example/api/v1/public/calendar/") || !strings.HasSuffix(u, ".ics") {
		t.Fatalf("feed view %+v", feed)
	}
	return strings.TrimPrefix(u, "https://api.erp.example")
}

func TestBookingCalendarFileAndSignedLink(t *testing.T) {
	e := newCalEnv(t)
	staff := &who{e.org, e.amina, "care_worker"}
	b := e.booking(e.org, e.customer, e.amina, "scheduled", 51*time.Hour, e.now) // 3 Oct 09:00 UTC = 12:00 in Nairobi
	e.n.Handle(nil, e.load(b.ID), "actor")

	// Signed-in download.
	w := e.do("GET", "/api/v1/bookings/"+b.ID+"/calendar.ics", staff, nil, "")
	file := "booking-2026-10-03-1200-jane-wanjiru.ics"
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/calendar; charset=utf-8; method=PUBLISH" || w.Header().Get("Content-Disposition") != `attachment; filename="`+file+`"` ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v", w.Code, w.Header())
	}
	cal, ev := oneEvent(t, w.Body.String())
	if cal.get(t, "METHOD") != "PUBLISH" || ev.get(t, "UID") != "booking-"+b.ID+"@dasyin-erp" || ev.get(t, "SEQUENCE") != "0" || strings.Contains(w.Body.String(), "ORGANIZER") || strings.Contains(w.Body.String(), "ATTENDEE") {
		t.Fatalf("download body:\n%s", w.Body.String())
	}
	if d := unescapeText(t, ev.get(t, "DESCRIPTION")); !strings.Contains(d, "Customer: Jane Wanjiru") || !strings.Contains(d, "Notes: Bring the spare key") || !strings.Contains(d, "Open booking: https://erp.example/bookings?booking="+b.ID) {
		t.Fatalf("description %q", d)
	}
	// Another organisation, or nobody signed in, never sees it.
	if w := e.do("GET", "/api/v1/bookings/"+b.ID+"/calendar.ics", &who{e.other, "x", "admin"}, nil, ""); w.Code != 404 {
		t.Fatalf("other org download: %d", w.Code)
	}
	if w := e.do("GET", "/api/v1/bookings/"+b.ID+"/calendar.ics", nil, nil, ""); w.Code != 401 {
		t.Fatalf("anonymous download: %d", w.Code)
	}

	// Links for the app's "Add to calendar" menu.
	links := e.data(e.do("GET", "/api/v1/bookings/"+b.ID+"/calendar-links", staff, nil, ""))
	pub := "https://api.erp.example/api/v1/public/bookings/" + b.ID + "/calendar.ics?t=" + LinkToken(e.n.Secret, e.org, b.ID)
	if links["ics_url"] != pub || links["filename"] != file || links["cancelled"] != false ||
		!strings.HasPrefix(links["google_url"].(string), "https://calendar.google.com/calendar/render?action=TEMPLATE&text=Booking%20%E2%80%94%20Jane%20Wanjiru&dates=20261003T090000Z/20261003T103000Z") ||
		!strings.HasPrefix(links["outlook_url"].(string), "https://outlook.office.com/calendar/0/deeplink/compose?") || !strings.HasPrefix(links["outlook_live_url"].(string), "https://outlook.live.com/calendar/0/deeplink/compose?") {
		t.Fatalf("links %+v", links)
	}
	if w := e.do("GET", "/api/v1/bookings/"+b.ID+"/calendar-links", &who{e.other, "x", "admin"}, nil, ""); w.Code != 404 {
		t.Fatalf("other org links: %d", w.Code)
	}

	// Public signed link: no login, inline so phones hand it to the calendar app.
	path := strings.TrimPrefix(pub, "https://api.erp.example")
	w = e.do("GET", path, nil, nil, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/calendar; charset=utf-8; method=PUBLISH" || w.Header().Get("Content-Disposition") != `inline; filename="`+file+`"` ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("public: %d %v", w.Code, w.Header())
	}
	if _, pev := oneEvent(t, w.Body.String()); pev.get(t, "UID") != ev.get(t, "UID") || pev.get(t, "SEQUENCE") != "0" {
		t.Fatalf("public body:\n%s", w.Body.String())
	}
	// Tampered, missing, another booking's or another organisation's token: the same 404 as an unknown booking.
	b2 := e.booking(e.org, e.customer, e.amina, "scheduled", 80*time.Hour, e.now)
	bo := e.booking(e.other, e.otherCust, "", "scheduled", 80*time.Hour, e.now)
	good := LinkToken(e.n.Secret, e.org, b.ID)
	// A tampered token always differs from the good one (the last character is swapped for another).
	last := "A"
	if strings.HasSuffix(good, "A") {
		last = "B"
	}
	tampered := good[:len(good)-1] + last
	var notFound string
	for name, p := range map[string]string{
		"unknown booking": "/api/v1/public/bookings/" + uuid.NewString() + "/calendar.ics?t=" + good,
		"no token":        "/api/v1/public/bookings/" + b.ID + "/calendar.ics",
		"tampered":        "/api/v1/public/bookings/" + b.ID + "/calendar.ics?t=" + tampered,
		"other booking":   "/api/v1/public/bookings/" + b2.ID + "/calendar.ics?t=" + good,
		"other org":       "/api/v1/public/bookings/" + bo.ID + "/calendar.ics?t=" + LinkToken(e.n.Secret, e.org, bo.ID),
	} {
		w := e.do("GET", p, nil, map[string]string{"RemoteAddr": "198.51.100." + name[:1] + ":1"}, "")
		if w.Code != 404 || strings.Contains(w.Body.String(), "VCALENDAR") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if notFound == "" {
			notFound = w.Body.String()
		} else if w.Body.String() != notFound {
			t.Fatalf("%s answers differently: %q", name, w.Body.String())
		}
	}
	// Without a server secret there are no public links at all.
	sec := e.n.Secret
	e.n.Secret = nil
	if w := e.do("GET", path, nil, map[string]string{"RemoteAddr": "198.51.100.200:1"}, ""); w.Code != 404 {
		t.Fatalf("no secret: %d", w.Code)
	}
	if l := e.data(e.do("GET", "/api/v1/bookings/"+b.ID+"/calendar-links", staff, nil, "")); l["ics_url"] != "" {
		t.Fatalf("no secret link %v", l["ics_url"])
	}
	e.n.Secret = sec

	// Rescheduled: same UID, SEQUENCE 1, new time — also when nobody is emailed (staff emails off).
	e.db.Save(&Settings{OrganizationID: e.org, StaffEmails: false, AgendaHour: 7})
	sentBefore := len(e.sent) // the "New booking" email from the creation
	before := e.load(b.ID)
	e.now = e.now.Add(time.Hour)
	e.db.Exec(`UPDATE bookings SET start_time = start_time + interval '2 hours', end_time = end_time + interval '2 hours', updated_at = ? WHERE id = ?`, e.now, b.ID)
	e.n.Handle(before, e.load(b.ID), "actor")
	_, rev := oneEvent(t, e.do("GET", path, nil, nil, "").Body.String())
	if rev.get(t, "UID") != ev.get(t, "UID") || rev.get(t, "SEQUENCE") != "1" || rev.get(t, "DTSTART") != "20261003T110000Z" || rev.get(t, "LAST-MODIFIED") != "20261001T070000Z" {
		t.Fatalf("rescheduled: %+v", rev.Props)
	}
	if sentBefore != 1 || len(e.sent) != sentBefore {
		t.Fatalf("emails: %d before, %d after switching staff emails off", sentBefore, len(e.sent))
	}

	// Cancelled: the same link now returns the CANCEL file with a higher SEQUENCE.
	before = e.load(b.ID)
	e.db.Exec(`UPDATE bookings SET status = 'cancelled' WHERE id = ?`, b.ID)
	e.n.Handle(before, e.load(b.ID), "actor")
	w = e.do("GET", path, nil, nil, "")
	ccal, cev := oneEvent(t, w.Body.String())
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/calendar; charset=utf-8; method=CANCEL" || !strings.HasSuffix(w.Header().Get("Content-Disposition"), `-jane-wanjiru-cancelled.ics"`) ||
		ccal.get(t, "METHOD") != "CANCEL" || cev.get(t, "STATUS") != "CANCELLED" || cev.get(t, "SEQUENCE") != "2" || cev.get(t, "UID") != ev.get(t, "UID") {
		t.Fatalf("cancelled: %d %v\n%s", w.Code, w.Header(), w.Body.String())
	}
	if l := e.data(e.do("GET", "/api/v1/bookings/"+b.ID+"/calendar-links", staff, nil, "")); l["cancelled"] != true || l["google_url"] != "" || l["outlook_url"] != "" || l["ics_url"] != pub {
		t.Fatalf("cancelled links %+v", l)
	}

	// Deleted: still answers, with the CANCEL file (the booking keeps its last times).
	del := e.load(b2.ID)
	e.db.Exec(`UPDATE bookings SET deleted_at = ? WHERE id = ?`, e.now, b2.ID)
	e.n.Handle(del, nil, "actor")
	w = e.do("GET", "/api/v1/public/bookings/"+b2.ID+"/calendar.ics?t="+LinkToken(e.n.Secret, e.org, b2.ID), nil, nil, "")
	dcal, dev := oneEvent(t, w.Body.String())
	if w.Code != 200 || dcal.get(t, "METHOD") != "CANCEL" || dev.get(t, "STATUS") != "CANCELLED" || dev.get(t, "SEQUENCE") != "1" {
		t.Fatalf("deleted: %d\n%s", w.Code, w.Body.String())
	}
}

func TestCalendarFeed(t *testing.T) {
	e := newCalEnv(t)
	staff, admin := &who{e.org, e.amina, "care_worker"}, &who{e.org, e.admin, "admin"}
	day := 24 * time.Hour
	past10 := e.booking(e.org, e.customer, e.amina, "completed", -10*day, e.now.Add(-10*day))
	tooOld := e.booking(e.org, e.customer, e.amina, "completed", -31*day, e.now.Add(-31*day))
	soon := e.booking(e.org, e.customer, e.amina, "scheduled", 2*day, e.now.Add(-day))
	far := e.booking(e.org, e.customer, e.amina, "confirmed", 300*day, e.now.Add(-day))
	tooFar := e.booking(e.org, e.customer, e.amina, "scheduled", 400*day, e.now.Add(-day))
	cancelled := e.booking(e.org, e.customer, e.amina, "cancelled", 5*day, e.now.Add(-5*day))
	longCancelled := e.booking(e.org, e.customer, e.amina, "cancelled", 6*day, e.now.Add(-31*day))
	noShow := e.booking(e.org, e.customer, e.amina, "no_show", -2*day, e.now.Add(-2*day))
	deleted := e.booking(e.org, e.customer, e.amina, "scheduled", 7*day, e.now.Add(-3*day))
	e.db.Exec(`UPDATE bookings SET deleted_at = ? WHERE id = ?`, e.now.Add(-time.Hour), deleted.ID)
	brians := e.booking(e.org, e.customer, e.brian, "scheduled", 3*day, e.now.Add(-day))
	nobody := e.booking(e.org, e.customer, "", "scheduled", 4*day, e.now.Add(-day))
	foreign := e.booking(e.other, e.otherCust, "", "scheduled", 2*day, e.now.Add(-day))
	e.db.Create(&CalState{BookingID: soon.ID, OrganizationID: e.org, Sequence: 4, UpdatedAt: e.now})

	// My feed is created on first use and stays the same link afterwards.
	d := e.data(e.do("GET", "/api/v1/booking-emails/feed", staff, nil, ""))
	path := feedPath(t, d["user"])
	if _, has := d["org"]; has || d["can_manage_org"] != false || d["refresh_minutes"] != float64(30) {
		t.Fatalf("staff feed payload %+v", d)
	}
	u := d["user"].(map[string]any)
	if u["name"] != "DASYIN bookings — Amina Otieno" || u["webcal_url"] != "webcal://api.erp.example"+path ||
		!strings.HasPrefix(u["google_url"].(string), "https://calendar.google.com/calendar/r?cid=webcal%3A%2F%2Fapi.erp.example%2Fapi%2Fv1%2Fpublic%2Fcalendar%2F") ||
		!strings.HasPrefix(u["outlook_url"].(string), "https://outlook.office.com/calendar/0/addfromweb?url=https%3A%2F%2Fapi.erp.example%2Fapi%2Fv1%2Fpublic%2Fcalendar%2F") {
		t.Fatalf("feed links %+v", u)
	}
	if again := feedPath(t, e.data(e.do("GET", "/api/v1/booking-emails/feed", staff, nil, ""))["user"]); again != path {
		t.Fatalf("feed link changed without regenerating")
	}
	token := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/public/calendar/"), ".ics")
	if len(token) < 43 || strings.Contains(path, e.amina) || strings.Contains(path, "amina") {
		t.Fatalf("feed token %q", token)
	}

	// Contents: my bookings from 30 days ago to 12 months ahead, cancelled ones for 30 days.
	w := e.do("GET", path, nil, nil, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/calendar; charset=utf-8" || w.Header().Get("ETag") == "" || w.Header().Get("Last-Modified") == "" ||
		w.Header().Get("Cache-Control") != "private, no-cache" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "inline;") {
		t.Fatalf("feed: %d %v", w.Code, w.Header())
	}
	cal := parseICS(t, w.Body.String())
	if cal.get(t, "METHOD") != "PUBLISH" || unescapeText(t, cal.get(t, "X-WR-CALNAME")) != "DASYIN bookings — Amina Otieno" || cal.get(t, "X-PUBLISHED-TTL") != "PT30M" ||
		len(cal.all("REFRESH-INTERVAL")) != 1 || cal.all("REFRESH-INTERVAL")[0].Value != "PT30M" {
		t.Fatalf("feed head %+v", cal.Props)
	}
	got := map[string]*icsComp{}
	for _, ev := range cal.subs("VEVENT") {
		got[ev.get(t, "UID")] = ev
	}
	uid := func(b *models.Booking) string { return "booking-" + b.ID + "@dasyin-erp" } // the UID of the emails
	for name, b := range map[string]*models.Booking{"10 days ago": past10, "soon": soon, "in 300 days": far, "cancelled": cancelled, "no-show": noShow, "deleted": deleted} {
		if got[uid(b)] == nil {
			t.Fatalf("feed lacks the %s booking", name)
		}
	}
	for name, b := range map[string]*models.Booking{"31 days ago": tooOld, "400 days ahead": tooFar, "cancelled 31 days ago": longCancelled, "Brian's": brians, "unassigned": nobody, "other organisation's": foreign} {
		if got[uid(b)] != nil {
			t.Fatalf("feed has the %s booking", name)
		}
	}
	if len(got) != 6 {
		t.Fatalf("feed has %d events", len(got))
	}
	if ev := got[uid(soon)]; ev.get(t, "STATUS") != "CONFIRMED" || ev.get(t, "SEQUENCE") != "4" || unescapeText(t, ev.get(t, "SUMMARY")) != "Booking — Jane Wanjiru" ||
		len(ev.subs("VALARM")) != 1 || ev.subs("VALARM")[0].get(t, "TRIGGER") != "-PT30M" || ev.get(t, "DTSTAMP") != "20260930T060000Z" {
		t.Fatalf("soon: %+v", ev.Props)
	}
	if ev := got[uid(cancelled)]; ev.get(t, "STATUS") != "CANCELLED" || !strings.HasPrefix(ev.get(t, "SUMMARY"), "Cancelled: ") || len(ev.subs("VALARM")) != 0 {
		t.Fatalf("cancelled: %+v", ev.Props)
	}
	if ev := got[uid(deleted)]; ev.get(t, "STATUS") != "CANCELLED" || ev.get(t, "LAST-MODIFIED") != "20261001T050000Z" {
		t.Fatalf("deleted: %+v", ev.Props)
	}
	if ev := got[uid(noShow)]; ev.get(t, "STATUS") != "CONFIRMED" || !strings.HasPrefix(ev.get(t, "SUMMARY"), "No-show: ") || len(ev.subs("VALARM")) != 0 {
		t.Fatalf("no-show: %+v", ev.Props)
	}
	if len(got[uid(past10)].subs("VALARM")) != 0 {
		t.Fatalf("past booking has a reminder")
	}
	if strings.Contains(w.Body.String(), "ORGANIZER") || strings.Contains(w.Body.String(), "ATTENDEE") {
		t.Fatal("feed carries attendees")
	}
	// The address works with or without ".ics".
	if w2 := e.do("GET", strings.TrimSuffix(path, ".ics"), nil, nil, ""); w2.Code != 200 || w2.Body.String() != w.Body.String() {
		t.Fatalf("without .ics: %d", w2.Code)
	}

	// Conditional requests: nothing changed → 304 without a body.
	etag, lastMod := w.Header().Get("ETag"), w.Header().Get("Last-Modified")
	e.now = e.now.Add(10 * time.Minute)
	for name, h := range map[string]map[string]string{"If-None-Match": {"If-None-Match": etag}, "weak etag list": {"If-None-Match": `"other", W/` + etag}, "If-Modified-Since": {"If-Modified-Since": lastMod}} {
		w := e.do("GET", path, nil, h, "")
		if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("ETag") != etag || w.Header().Get("Last-Modified") != lastMod {
			t.Fatalf("%s: %d etag %q last-modified %q", name, w.Code, w.Header().Get("ETag"), w.Header().Get("Last-Modified"))
		}
	}
	if w := e.do("GET", path, nil, map[string]string{"If-None-Match": `"something-else"`, "If-Modified-Since": lastMod}, ""); w.Code != 200 {
		t.Fatalf("a different ETag must win over If-Modified-Since: %d", w.Code)
	}
	// A booking changes → new ETag, later Last-Modified, full body again.
	e.now = e.now.Add(10 * time.Minute)
	bf := e.load(soon.ID)
	e.db.Exec(`UPDATE bookings SET start_time = start_time + interval '1 hour', end_time = end_time + interval '1 hour', updated_at = ? WHERE id = ?`, e.now, soon.ID)
	e.n.Handle(bf, e.load(soon.ID), "actor")
	w = e.do("GET", path, nil, map[string]string{"If-None-Match": etag, "If-Modified-Since": lastMod}, "")
	if w.Code != 200 || w.Header().Get("ETag") == etag || w.Header().Get("Last-Modified") == lastMod {
		t.Fatalf("after a change: %d %v", w.Code, w.Header())
	}
	if w2 := e.do("GET", path, nil, map[string]string{"If-Modified-Since": lastMod}, ""); w2.Code != 200 {
		t.Fatalf("stale If-Modified-Since: %d", w2.Code)
	}
	for _, ev := range parseICS(t, w.Body.String()).subs("VEVENT") {
		if ev.get(t, "UID") == uid(soon) && (ev.get(t, "SEQUENCE") != "5" || ev.get(t, "DTSTART") != soon.StartTime.Add(time.Hour).UTC().Format(icsTime)) {
			t.Fatalf("changed booking in feed: %+v", ev.Props)
		}
	}
	// A booking that only drops out of the window changes the feed too (Last-Modified moves).
	etag2, lastMod2 := w.Header().Get("ETag"), w.Header().Get("Last-Modified")
	e.now = e.now.Add(21 * day) // past10 is now 31 days old
	w = e.do("GET", path, nil, map[string]string{"If-Modified-Since": lastMod2}, "")
	if w.Code != 200 || w.Header().Get("ETag") == etag2 || strings.Contains(w.Body.String(), uid(past10)) {
		t.Fatalf("window moved: %d", w.Code)
	}
	e.now = e.now.Add(-21 * day)

	// Reminder lead time is an organisation setting: 15 minutes, or none.
	if w := e.do("PUT", "/api/v1/booking-emails/settings", staff, nil, `{"reminder_minutes":15}`); w.Code != 403 {
		t.Fatalf("staff changed settings: %d", w.Code)
	}
	if w := e.do("PUT", "/api/v1/booking-emails/settings", admin, nil, `{"reminder_minutes":45}`); w.Code != 400 {
		t.Fatalf("odd reminder accepted: %d", w.Code)
	}
	st := e.data(e.do("GET", "/api/v1/booking-emails/settings", admin, nil, ""))["settings"].(map[string]any)
	if st["reminder_minutes"] != float64(30) {
		t.Fatalf("default reminder %v", st["reminder_minutes"])
	}
	st = e.data(e.do("PUT", "/api/v1/booking-emails/settings", admin, nil, `{"reminder_minutes":15}`))["settings"].(map[string]any)
	if st["reminder_minutes"] != float64(15) || st["staff_emails"] != true {
		t.Fatalf("saved settings %+v", st)
	}
	if body := e.do("GET", path, nil, nil, "").Body.String(); !strings.Contains(body, "TRIGGER:-PT15M") || strings.Contains(body, "TRIGGER:-PT30M") {
		t.Fatalf("feed ignores the reminder setting")
	}
	if _, ev := oneEvent(t, e.do("GET", "/api/v1/bookings/"+soon.ID+"/calendar.ics", staff, nil, "").Body.String()); ev.subs("VALARM")[0].get(t, "TRIGGER") != "-PT15M" {
		t.Fatalf("file ignores the reminder setting")
	}
	e.data(e.do("PUT", "/api/v1/booking-emails/settings", admin, nil, `{"reminder_minutes":0}`))
	if body := e.do("GET", path, nil, nil, "").Body.String(); strings.Contains(body, "VALARM") {
		t.Fatalf("no reminder still has alarms")
	}
	if st := e.data(e.do("GET", "/api/v1/booking-emails/settings", admin, nil, ""))["settings"].(map[string]any); st["reminder_minutes"] != float64(0) {
		t.Fatalf("none not kept: %v", st["reminder_minutes"])
	}

	// Regenerate: a new link, the old one is gone for good.
	nf := e.data(e.do("POST", "/api/v1/booking-emails/feed/regenerate", staff, nil, `{}`))["feed"]
	newPath := feedPath(t, nf)
	if newPath == path {
		t.Fatal("regenerate kept the link")
	}
	if w := e.do("GET", path, nil, nil, ""); w.Code != 410 || strings.Contains(w.Body.String(), "VCALENDAR") {
		t.Fatalf("old link: %d", w.Code)
	}
	if w := e.do("GET", newPath, nil, nil, ""); w.Code != 200 {
		t.Fatalf("new link: %d", w.Code)
	}
	if cur := feedPath(t, e.data(e.do("GET", "/api/v1/booking-emails/feed", staff, nil, ""))["user"]); cur != newPath {
		t.Fatal("GET does not return the regenerated link")
	}
	// A link that never existed is a 404.
	if w := e.do("GET", "/api/v1/public/calendar/"+strings.Repeat("A", 43)+".ics", nil, map[string]string{"RemoteAddr": "198.51.100.9:1"}, ""); w.Code != 404 {
		t.Fatalf("unknown link: %d", w.Code)
	}

	// Organisation feed: admins and managers only, everybody's bookings with the staff name in front.
	if w := e.do("POST", "/api/v1/booking-emails/feed/regenerate", staff, nil, `{"scope":"org"}`); w.Code != 403 {
		t.Fatalf("staff created the organisation feed: %d", w.Code)
	}
	if w := e.do("DELETE", "/api/v1/booking-emails/feed?scope=org", staff, nil, ""); w.Code != 403 {
		t.Fatalf("staff turned the organisation feed off: %d", w.Code)
	}
	ad := e.data(e.do("GET", "/api/v1/booking-emails/feed", admin, nil, ""))
	if o := ad["org"].(map[string]any); o["enabled"] != false || ad["can_manage_org"] != true {
		t.Fatalf("organisation feed is on before anyone asked: %+v", ad)
	}
	orgPath := feedPath(t, e.data(e.do("POST", "/api/v1/booking-emails/feed/regenerate", admin, nil, `{"scope":"org"}`))["feed"])
	w = e.do("GET", orgPath, nil, nil, "")
	ocal := parseICS(t, w.Body.String())
	if unescapeText(t, ocal.get(t, "X-WR-CALNAME")) != "DASYIN bookings — Nairobi Auto" {
		t.Fatalf("org feed name %q", ocal.get(t, "X-WR-CALNAME"))
	}
	sum := map[string]string{}
	for _, ev := range ocal.subs("VEVENT") {
		sum[ev.get(t, "UID")] = unescapeText(t, ev.get(t, "SUMMARY"))
	}
	if sum[uid(soon)] != "Amina Otieno: Booking — Jane Wanjiru" || sum[uid(brians)] != "Brian Kip: Booking — Jane Wanjiru" || sum[uid(nobody)] != "Unassigned: Booking — Jane Wanjiru" ||
		sum[uid(cancelled)] != "Amina Otieno: Cancelled: Booking — Jane Wanjiru" || sum[uid(foreign)] != "" {
		t.Fatalf("org feed summaries %+v", sum)
	}
	if orgPath == newPath || feedPath(t, e.data(e.do("GET", "/api/v1/booking-emails/feed", admin, nil, ""))["org"]) != orgPath {
		t.Fatal("organisation feed link")
	}
	// Turned off → 410, and it stays off until an admin turns it on again.
	e.data(e.do("DELETE", "/api/v1/booking-emails/feed?scope=org", admin, nil, ""))
	if w := e.do("GET", orgPath, nil, nil, ""); w.Code != 410 {
		t.Fatalf("revoked org feed: %d", w.Code)
	}
	if o := e.data(e.do("GET", "/api/v1/booking-emails/feed", admin, nil, ""))["org"].(map[string]any); o["enabled"] != false {
		t.Fatal("organisation feed came back by itself")
	}
	// My own feed turned off stays off as well (no silent re-creation).
	e.data(e.do("DELETE", "/api/v1/booking-emails/feed", staff, nil, ""))
	if u := e.data(e.do("GET", "/api/v1/booking-emails/feed", staff, nil, ""))["user"].(map[string]any); u["enabled"] != false {
		t.Fatal("my feed came back by itself")
	}
	newPath = feedPath(t, e.data(e.do("POST", "/api/v1/booking-emails/feed/regenerate", staff, nil, `{"scope":"user"}`))["feed"])

	// Business-type words: a workshop's feed is "job cards".
	e.n.Terms = func(string) map[string]string {
		return map[string]string{"booking": "Job card", "bookings": "Job cards"}
	}
	tb := e.do("GET", newPath, nil, nil, "").Body.String()
	if tc := parseICS(t, tb); unescapeText(t, tc.get(t, "X-WR-CALNAME")) != "DASYIN job cards — Amina Otieno" || !strings.Contains(tb, "CATEGORIES:Job card\r\n") || !strings.Contains(tb, `Open job card: https://erp.example/bookings?booking=`) || !strings.Contains(tb, "SUMMARY:Job card — Jane Wanjiru\r\n") {
		t.Fatalf("terms not used:\n%s", tb[:400])
	}
	e.n.Terms = nil

	// A staff member who left: the feed stops.
	e.db.Exec(`UPDATE users SET is_active = false WHERE id = ?`, e.amina)
	if w := e.do("GET", newPath, nil, nil, ""); w.Code != 410 {
		t.Fatalf("inactive user's feed: %d", w.Code)
	}
}

func TestPublicCalendarRateLimit(t *testing.T) {
	e := newCalEnv(t)
	guess := map[string]string{"RemoteAddr": "192.0.2.50:999"}
	codes := map[int]int{}
	var retry string
	for i := 0; i < 40; i++ {
		w := e.do("GET", "/api/v1/public/calendar/"+strings.Repeat("x", 43)+".ics", nil, guess, "")
		codes[w.Code]++
		if w.Code == 429 {
			retry = w.Header().Get("Retry-After")
		}
	}
	// Guessing is cut off after a handful of misses …
	if codes[404] == 0 || codes[404] > 15 || codes[429] < 25 || retry == "" || retry == "0" {
		t.Fatalf("guessing was not limited: %v (Retry-After %q)", codes, retry)
	}
	for i := 0; i < 5; i++ {
		if w := e.do("GET", "/api/v1/public/bookings/"+uuid.NewString()+"/calendar.ics?t=nope", nil, guess, ""); w.Code != 429 {
			t.Fatalf("signed links share the address limit: %d", w.Code)
		}
	}
	// … while a calendar app polling its own feed from another address is fine,
	b := e.booking(e.org, e.customer, e.amina, "scheduled", 48*time.Hour, e.now)
	ft, err := e.n.NewFeed(e.org, FeedUser, e.amina, e.amina)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if w := e.do("GET", FeedPath(ft.Token), nil, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), b.ID) {
			t.Fatalf("poll %d: %d", i, w.Code)
		}
	}
	// … until it hammers the feed (30 a minute), and the budget refills with time.
	for i := 0; i < 15; i++ {
		e.do("GET", FeedPath(ft.Token), nil, nil, "")
	}
	if w := e.do("GET", FeedPath(ft.Token), nil, nil, ""); w.Code != 429 {
		t.Fatalf("hammering a feed: %d", w.Code)
	}
	e.now = e.now.Add(2 * time.Minute)
	if w := e.do("GET", FeedPath(ft.Token), nil, nil, ""); w.Code != 200 {
		t.Fatalf("after waiting: %d", w.Code)
	}
	if w := e.do("GET", "/api/v1/public/calendar/"+strings.Repeat("x", 43)+".ics", nil, guess, ""); w.Code != 404 {
		t.Fatalf("guesser after waiting: %d", w.Code)
	}
}
