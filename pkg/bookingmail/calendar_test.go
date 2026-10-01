package bookingmail

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// ---------------------------------------------------------------- a small strict iCalendar parser

type icsProp struct {
	Name   string
	Params string // raw ";KEY=VALUE…" (may be empty)
	Value  string // raw value (still escaped)
}

type icsComp struct {
	Name  string
	Props []icsProp
	Subs  []*icsComp
}

func (c *icsComp) all(name string) []icsProp {
	var out []icsProp
	for _, p := range c.Props {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// get returns the single value of a property ("" when absent); more than one is an error.
func (c *icsComp) get(t *testing.T, name string) string {
	t.Helper()
	ps := c.all(name)
	if len(ps) > 1 {
		t.Fatalf("%s: %s appears %d times", c.Name, name, len(ps))
	}
	if len(ps) == 0 {
		return ""
	}
	return ps[0].Value
}

func (c *icsComp) has(name string) bool { return len(c.all(name)) > 0 }

func (c *icsComp) subs(name string) []*icsComp {
	var out []*icsComp
	for _, s := range c.Subs {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

var (
	icsLineRe = regexp.MustCompile(`^([A-Z][A-Z0-9-]*)((?:;[A-Za-z0-9-]+=(?:"[^"]*"|[^;:,"]*)(?:,(?:"[^"]*"|[^;:,"]*))*)*):(.*)$`)
	icsUTCRe  = regexp.MustCompile(`^\d{8}T\d{6}Z$`)
)

// unescapeText reverses RFC 5545 TEXT escaping and rejects stray backslashes.
func unescapeText(t *testing.T, v string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' {
			b.WriteByte(v[i])
			continue
		}
		i++
		if i >= len(v) {
			t.Fatalf("dangling backslash in %q", v)
		}
		switch v[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		case '\\', ';', ',':
			b.WriteByte(v[i])
		default:
			t.Fatalf("bad escape \\%c in %q", v[i], v)
		}
	}
	return b.String()
}

// parseICS validates the file structurally and returns the VCALENDAR:
// CRLF line ends only, every physical line ≤ 75 octets and valid UTF-8,
// well-formed content lines, balanced BEGIN/END, and the required properties
// of VCALENDAR, VEVENT and VALARM.
func parseICS(t *testing.T, ics string) *icsComp {
	t.Helper()
	if !strings.HasSuffix(ics, "\r\n") {
		t.Fatalf("file does not end with CRLF")
	}
	if strings.Contains(strings.ReplaceAll(ics, "\r\n", ""), "\n") || strings.Contains(strings.ReplaceAll(ics, "\r\n", ""), "\r") {
		t.Fatalf("bare CR or LF in file:\n%q", ics)
	}
	phys := strings.Split(strings.TrimSuffix(ics, "\r\n"), "\r\n")
	var lines []string
	for i, l := range phys {
		if len(l) > 75 {
			t.Fatalf("line %d is %d octets (> 75): %q", i+1, len(l), l)
		}
		if l == "" {
			t.Fatalf("line %d is empty", i+1)
		}
		if !utf8.ValidString(l) {
			t.Fatalf("line %d splits a UTF-8 sequence: %q", i+1, l)
		}
		if l[0] == ' ' || l[0] == '\t' {
			if len(lines) == 0 {
				t.Fatalf("continuation line first")
			}
			lines[len(lines)-1] += l[1:]
			continue
		}
		lines = append(lines, l)
	}
	root := &icsComp{Name: "ROOT"}
	stack := []*icsComp{root}
	for _, l := range lines {
		m := icsLineRe.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("not a content line: %q", l)
		}
		for _, r := range l {
			if r < 0x20 && r != '\t' {
				t.Fatalf("control character in %q", l)
			}
		}
		name, params, value := m[1], m[2], m[3]
		cur := stack[len(stack)-1]
		switch name {
		case "BEGIN":
			c := &icsComp{Name: value}
			cur.Subs = append(cur.Subs, c)
			stack = append(stack, c)
		case "END":
			if len(stack) == 1 || cur.Name != value {
				t.Fatalf("END:%s does not close %s", value, cur.Name)
			}
			stack = stack[:len(stack)-1]
		default:
			if cur == root {
				t.Fatalf("property outside VCALENDAR: %q", l)
			}
			cur.Props = append(cur.Props, icsProp{Name: name, Params: params, Value: value})
		}
	}
	if len(stack) != 1 {
		t.Fatalf("unclosed %s", stack[len(stack)-1].Name)
	}
	if len(root.Subs) != 1 || root.Subs[0].Name != "VCALENDAR" {
		t.Fatalf("want exactly one VCALENDAR")
	}
	cal := root.Subs[0]
	if cal.get(t, "VERSION") != "2.0" || cal.get(t, "PRODID") == "" || cal.get(t, "METHOD") == "" {
		t.Fatalf("VCALENDAR needs VERSION:2.0, PRODID and METHOD")
	}
	uids := map[string]bool{}
	for _, s := range cal.Subs {
		if s.Name != "VEVENT" {
			t.Fatalf("unexpected component %s", s.Name)
		}
		for _, req := range []string{"UID", "DTSTAMP", "DTSTART", "DTEND", "SEQUENCE", "SUMMARY", "STATUS", "TRANSP", "CLASS", "CREATED", "LAST-MODIFIED"} {
			if s.get(t, req) == "" {
				t.Fatalf("VEVENT lacks %s", req)
			}
		}
		for _, dt := range []string{"DTSTAMP", "DTSTART", "DTEND", "CREATED", "LAST-MODIFIED"} {
			if !icsUTCRe.MatchString(s.get(t, dt)) {
				t.Fatalf("%s is not a UTC date-time: %q", dt, s.get(t, dt))
			}
		}
		if s.get(t, "DTEND") <= s.get(t, "DTSTART") {
			t.Fatalf("DTEND %s is not after DTSTART %s", s.get(t, "DTEND"), s.get(t, "DTSTART"))
		}
		if uids[s.get(t, "UID")] {
			t.Fatalf("duplicate UID %s", s.get(t, "UID"))
		}
		uids[s.get(t, "UID")] = true
		for _, txt := range []string{"SUMMARY", "DESCRIPTION", "LOCATION", "CATEGORIES"} {
			unescapeText(t, s.get(t, txt))
		}
		for _, a := range s.Subs {
			if a.Name != "VALARM" || a.get(t, "ACTION") == "" || a.get(t, "TRIGGER") == "" || a.get(t, "DESCRIPTION") == "" {
				t.Fatalf("bad VALARM %+v", a)
			}
		}
	}
	return cal
}

func oneEvent(t *testing.T, ics string) (*icsComp, *icsComp) {
	t.Helper()
	cal := parseICS(t, ics)
	ev := cal.subs("VEVENT")
	if len(ev) != 1 {
		t.Fatalf("want one VEVENT, got %d", len(ev))
	}
	return cal, ev[0]
}

// ---------------------------------------------------------------- fixtures

func sampleBooking() *models.Booking {
	start := time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC) // 09:00 in Nairobi
	return &models.Booking{ID: "4f1c-77", OrganizationID: "org-1", Status: "scheduled", StartTime: start, EndTime: start.Add(90 * time.Minute),
		CreatedAt: time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 29, 9, 30, 0, 0, time.UTC),
		Customer: models.Customer{FirstName: "Jane", LastName: "Wanjiru", Phone: "+254 700 111222"}}
}

func sampleEvent() ICSInput {
	return ICSInput{Booking: sampleBooking(), Sequence: 2, Summary: "Full service, Oil change — Jane Wanjiru",
		Description: "Customer: Jane Wanjiru\nPhone: +254 700 111222\nServices: Full service, Oil change\nVehicle: 2016 Toyota Axio · KDA 123A\nNotes: Bring the spare key; park at the back",
		Location:    "12 Ngong Rd, Kilimani", URL: "https://erp.example/bookings?booking=4f1c-77", Reminder: 30, Category: "Booking",
		OrganizerName: "Nairobi Auto", OrganizerEmail: "bookings@shop.example", AttendeeName: "Amina Otieno", AttendeeEmail: "amina@shop.example",
		Now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

// ---------------------------------------------------------------- the two flavours

func TestAddToCalendarFileIsAPlainPublishedEvent(t *testing.T) {
	in := sampleEvent()
	in.Method = MethodPublish
	ics := BuildAddICS(in)
	cal, ev := oneEvent(t, ics)
	if cal.get(t, "METHOD") != "PUBLISH" || cal.get(t, "CALSCALE") != "GREGORIAN" {
		t.Fatalf("calendar head: %+v", cal.Props)
	}
	// Nothing that makes a calendar app treat it as somebody else's invitation.
	for _, bad := range []string{"ORGANIZER", "ATTENDEE", "METHOD:REQUEST", "PARTSTAT", "RSVP", "mailto:"} {
		if strings.Contains(ics, bad) {
			t.Fatalf("add-to-calendar file contains %q:\n%s", bad, ics)
		}
	}
	// A calendar name makes Outlook open the file as a separate calendar.
	if cal.has("X-WR-CALNAME") {
		t.Fatalf("single event file must not name a calendar")
	}
	want := map[string]string{"UID": "booking-4f1c-77@dasyin-erp", "SEQUENCE": "2", "DTSTAMP": "20261001T120000Z", "DTSTART": "20261003T060000Z", "DTEND": "20261003T073000Z",
		"CREATED": "20260928T080000Z", "LAST-MODIFIED": "20260929T093000Z", "STATUS": "CONFIRMED", "TRANSP": "OPAQUE", "CLASS": "PUBLIC", "CATEGORIES": "Booking",
		"URL": "https://erp.example/bookings?booking=4f1c-77", "X-MICROSOFT-CDO-BUSYSTATUS": "BUSY", "SUMMARY": `Full service\, Oil change — Jane Wanjiru`, "LOCATION": `12 Ngong Rd\, Kilimani`}
	for k, v := range want {
		if got := ev.get(t, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	desc := unescapeText(t, ev.get(t, "DESCRIPTION"))
	for _, s := range []string{"Customer: Jane Wanjiru", "Phone: +254 700 111222", "Services: Full service, Oil change", "Vehicle: 2016 Toyota Axio · KDA 123A",
		"Notes: Bring the spare key; park at the back", "Open booking: https://erp.example/bookings?booking=4f1c-77"} {
		if !strings.Contains(desc, s) {
			t.Errorf("description lacks %q:\n%s", s, desc)
		}
	}
	al := ev.subs("VALARM")
	if len(al) != 1 || al[0].get(t, "TRIGGER") != "-PT30M" || al[0].get(t, "ACTION") != "DISPLAY" {
		t.Fatalf("alarm: %+v", al)
	}
}

func TestReminderLeadTime(t *testing.T) {
	for _, m := range []int{0, 15, 30, 60} {
		in := sampleEvent()
		in.Reminder = m
		_, ev := oneEvent(t, BuildAddICS(in))
		al := ev.subs("VALARM")
		if m == 0 {
			if len(al) != 0 {
				t.Fatalf("no reminder still has an alarm")
			}
			continue
		}
		if len(al) != 1 || al[0].get(t, "TRIGGER") != fmt.Sprintf("-PT%dM", m) {
			t.Fatalf("reminder %d: %+v", m, al)
		}
	}
	// The setting: default 30, only the offered choices are kept.
	if (Settings{}).Reminder() != 30 || DefaultSettings("o").Reminder() != 30 {
		t.Fatal("default reminder is 30 minutes")
	}
	none, odd := 0, 45
	if (Settings{ReminderMinutes: &none}).Reminder() != 0 || (Settings{ReminderMinutes: &odd}).Reminder() != 30 {
		t.Fatal("reminder choices")
	}
	if !ValidReminder(0) || !ValidReminder(15) || !ValidReminder(60) || ValidReminder(45) || ValidReminder(-1) {
		t.Fatal("ValidReminder")
	}
}

func TestUIDAndSequenceAcrossUpdates(t *testing.T) {
	a := sampleEvent()
	a.Sequence = 0
	b := sampleEvent()
	b.Sequence = 1
	b.Booking.StartTime = b.Booking.StartTime.Add(2 * time.Hour)
	b.Booking.EndTime = b.Booking.EndTime.Add(2 * time.Hour)
	_, ea := oneEvent(t, BuildAddICS(a))
	_, eb := oneEvent(t, BuildAddICS(b))
	_, ia := oneEvent(t, BuildICS(a))
	if ea.get(t, "UID") != eb.get(t, "UID") || ea.get(t, "UID") != ia.get(t, "UID") || ea.get(t, "UID") != UID("4f1c-77") {
		t.Fatalf("UID changed: %s / %s / %s", ea.get(t, "UID"), eb.get(t, "UID"), ia.get(t, "UID"))
	}
	if ea.get(t, "SEQUENCE") != "0" || eb.get(t, "SEQUENCE") != "1" || eb.get(t, "DTSTART") != "20261003T080000Z" {
		t.Fatalf("sequence / time: %s %s %s", ea.get(t, "SEQUENCE"), eb.get(t, "SEQUENCE"), eb.get(t, "DTSTART"))
	}
}

func TestCancelFile(t *testing.T) {
	in := sampleEvent()
	in.Method, in.Sequence = MethodCancel, 3
	ics := BuildAddICS(in)
	cal, ev := oneEvent(t, ics)
	if cal.get(t, "METHOD") != "CANCEL" || ev.get(t, "STATUS") != "CANCELLED" || ev.get(t, "UID") != "booking-4f1c-77@dasyin-erp" || ev.get(t, "SEQUENCE") != "3" {
		t.Fatalf("cancel file:\n%s", ics)
	}
	if ev.get(t, "TRANSP") != "TRANSPARENT" || len(ev.subs("VALARM")) != 0 || strings.Contains(ics, "ORGANIZER") || strings.Contains(ics, "ATTENDEE") {
		t.Fatalf("cancel file keeps blocking time, an alarm or an attendee:\n%s", ics)
	}
	// A complete event, so it opens fine when the booking was never added.
	if ev.get(t, "SUMMARY") == "" || ev.get(t, "DTSTART") != "20261003T060000Z" || ev.get(t, "DESCRIPTION") == "" {
		t.Fatalf("cancel file is not a complete event")
	}
	// Cancelled flag (deleted bookings) gives the same file.
	in2 := sampleEvent()
	in2.Method, in2.Sequence, in2.Cancelled = MethodPublish, 3, true
	if BuildAddICS(in2) != ics {
		t.Fatalf("Cancelled flag differs from METHOD CANCEL")
	}
}

func TestInlineInvitationIsStillARequest(t *testing.T) {
	in := sampleEvent()
	in.Method = MethodRequest
	ics := BuildICS(in)
	cal, ev := oneEvent(t, ics)
	if cal.get(t, "METHOD") != "REQUEST" {
		t.Fatalf("method %s", cal.get(t, "METHOD"))
	}
	org := ev.all("ORGANIZER")
	att := ev.all("ATTENDEE")
	if len(org) != 1 || org[0].Value != "mailto:bookings@shop.example" || org[0].Params != `;CN="Nairobi Auto"` {
		t.Fatalf("organizer %+v", org)
	}
	if len(att) != 1 || att[0].Value != "mailto:amina@shop.example" ||
		att[0].Params != `;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=FALSE;CN="Amina Otieno"` {
		t.Fatalf("attendee %+v", att)
	}
	if ev.get(t, "STATUS") != "CONFIRMED" || ev.get(t, "SEQUENCE") != "2" || ev.get(t, "UID") != "booking-4f1c-77@dasyin-erp" || cal.has("X-WR-CALNAME") {
		t.Fatalf("invitation:\n%s", ics)
	}
	// Empty method defaults to REQUEST.
	in.Method = ""
	if c, _ := oneEvent(t, BuildICS(in)); c.get(t, "METHOD") != "REQUEST" {
		t.Fatal("default method")
	}
	// Cancellation keeps iTIP semantics: organiser + attendee, no RSVP.
	in.Method = MethodCancel
	cc, ce := oneEvent(t, BuildICS(in))
	ca := ce.all("ATTENDEE")
	if cc.get(t, "METHOD") != "CANCEL" || ce.get(t, "STATUS") != "CANCELLED" || len(ce.all("ORGANIZER")) != 1 || len(ca) != 1 ||
		strings.Contains(ca[0].Params, "PARTSTAT") || strings.Contains(ca[0].Params, "RSVP") || len(ce.subs("VALARM")) != 0 {
		t.Fatalf("cancel invitation: %+v", ce.Props)
	}
}

func TestFoldingAndEscaping(t *testing.T) {
	in := sampleEvent()
	in.Summary = "Brake pads; discs, fluid \\ check — Zoë Müller-O’Brien 张伟"
	in.Description = "Notes: first line\r\nsecond; line, with a back\\slash\nÜmlaute äöü and emoji 🚗🔧 " + strings.Repeat("Straße 日本語 ", 30) + "\n" + strings.Repeat("x", 300)
	in.Location = "Unit 4, 12 Ngong Rd; Nairobi\nKenya"
	in.OrganizerName = "Shop \"Quoted\"; Co,\nLtd"
	in.AttendeeName = strings.Repeat("Amina Wanjirũ ", 8)
	in.URL = "https://erp.example/bookings?booking=4f1c-77&x=" + strings.Repeat("a", 120)
	for name, ics := range map[string]string{"add": BuildAddICS(in), "invite": BuildICS(in)} {
		_, ev := oneEvent(t, ics) // CRLF, ≤ 75 octets, UTF-8 intact, escapes valid
		if got := unescapeText(t, ev.get(t, "SUMMARY")); got != in.Summary {
			t.Errorf("%s summary round trip: %q", name, got)
		}
		if raw := ev.get(t, "SUMMARY"); !strings.Contains(raw, `pads\; discs\, fluid \\ check`) {
			t.Errorf("%s summary not escaped: %q", name, raw)
		}
		desc := unescapeText(t, ev.get(t, "DESCRIPTION"))
		wantDesc := strings.ReplaceAll(in.Description, "\r\n", "\n") + "\nOpen booking: " + in.URL
		if desc != wantDesc {
			t.Errorf("%s description round trip:\n got %q\nwant %q", name, desc, wantDesc)
		}
		if raw := ev.get(t, "DESCRIPTION"); !strings.Contains(raw, `first line\nsecond\; line\, with a back\\slash\n`) {
			t.Errorf("%s description not escaped: %q", name, raw[:80])
		}
		// One-line properties never carry a line break.
		if loc := unescapeText(t, ev.get(t, "LOCATION")); loc != "Unit 4, 12 Ngong Rd; Nairobi Kenya" {
			t.Errorf("%s location %q", name, loc)
		}
		if ev.get(t, "URL") != in.URL {
			t.Errorf("%s url %q", name, ev.get(t, "URL"))
		}
	}
	_, ev := oneEvent(t, BuildICS(in))
	if p := ev.all("ORGANIZER")[0].Params; p != `;CN="Shop Quoted ; Co, Ltd"` {
		t.Errorf("organizer CN %q", p)
	}
	// Folded lines really are folded (a long line exists once unfolded).
	if !strings.Contains(BuildAddICS(in), "\r\n ") {
		t.Fatal("nothing was folded")
	}
	// Control characters never reach the file.
	in.Summary = "Tab\there\x00\x07 bell"
	if _, ev := oneEvent(t, BuildAddICS(in)); ev.get(t, "SUMMARY") != "Tab here bell" {
		t.Errorf("control characters: %q", ev.get(t, "SUMMARY"))
	}
	if UID("a\r\nb c") != "booking-abc@dasyin-erp" {
		t.Errorf("uid %q", UID("a\r\nb c"))
	}
}

func TestFilename(t *testing.T) {
	nairobi, _ := time.LoadLocation("Africa/Nairobi")
	b := sampleBooking()
	cases := []struct {
		first, last string
		loc         *time.Location
		cancelled   bool
		want        string
	}{
		{"Jane", "Wanjiru", nairobi, false, "booking-2026-10-03-0900-jane-wanjiru.ics"},
		{"Jane", "Wanjiru", nil, false, "booking-2026-10-03-0600-jane-wanjiru.ics"},
		{"Jane", "Wanjiru", nairobi, true, "booking-2026-10-03-0900-jane-wanjiru-cancelled.ics"},
		{"Zoë", "Müller-O’Brien", nairobi, false, "booking-2026-10-03-0900-zoe-muller-o-brien.ics"},
		{"José", "Núñez / Straße", nairobi, false, "booking-2026-10-03-0900-jose-nunez-strasse.ics"},
		{"张", "伟", nairobi, false, "booking-2026-10-03-0900.ics"},
		{"", "", nairobi, false, "booking-2026-10-03-0900.ics"},
		{`../../etc/passwd "x"`, "\r\nContent-Type: x", nairobi, false, "booking-2026-10-03-0900-etc-passwd-x-content-type-x.ics"},
		{strings.Repeat("Bartholomew ", 6), "Featherstonehaugh", nairobi, false, "booking-2026-10-03-0900-bartholomew-bartholomew-bartholo.ics"},
	}
	safe := regexp.MustCompile(`^[a-z0-9-]+\.ics$`)
	for _, c := range cases {
		b.Customer.FirstName, b.Customer.LastName = c.first, c.last
		got := ICSFilename(b, c.loc, c.cancelled)
		if got != c.want {
			t.Errorf("%q %q → %q, want %q", c.first, c.last, got, c.want)
		}
		if !safe.MatchString(got) || len(got) > 80 {
			t.Errorf("unsafe file name %q", got)
		}
	}
}

func TestSignedLinkToken(t *testing.T) {
	secret := []byte("test-secret-0123456789abcdef")
	tok := LinkToken(secret, "org-1", "bk-1")
	if len(tok) != 43 || strings.ContainsAny(tok, "+/=") {
		t.Fatalf("token %q is not unpadded base64url of 32 bytes", tok)
	}
	if !VerifyLinkToken(secret, "org-1", "bk-1", tok) {
		t.Fatal("valid token rejected")
	}
	if tok != LinkToken(secret, "org-1", "bk-1") {
		t.Fatal("token is not deterministic")
	}
	flip := []byte(tok)
	if flip[10] == 'A' {
		flip[10] = 'B'
	} else {
		flip[10] = 'A'
	}
	for name, bad := range map[string][4]string{
		"tampered":      {"org-1", "bk-1", string(flip), string(secret)},
		"truncated":     {"org-1", "bk-1", tok[:20], string(secret)},
		"empty":         {"org-1", "bk-1", "", string(secret)},
		"other org":     {"org-2", "bk-1", tok, string(secret)},
		"other booking": {"org-1", "bk-2", tok, string(secret)},
		"other secret":  {"org-1", "bk-1", tok, "another-secret-0123456789"},
		"no secret":     {"org-1", "bk-1", tok, ""},
		"no org":        {"", "bk-1", LinkToken(secret, "", "bk-1"), string(secret)},
	} {
		if VerifyLinkToken([]byte(bad[3]), bad[0], bad[1], bad[2]) {
			t.Errorf("%s token accepted", name)
		}
	}
	// The separator keeps (org, id) pairs apart.
	if LinkToken(secret, "org-1\nbk", "1") == LinkToken(secret, "org-1", "bk\n1") {
		t.Error("ambiguous token input")
	}
	p := PublicICSPath(secret, "org-1", "bk 1/x")
	if p != "/api/v1/public/bookings/bk%201%2Fx/calendar.ics?t="+LinkToken(secret, "org-1", "bk 1/x") {
		t.Errorf("path %q", p)
	}
	// No personal data in the link: only the booking id and the token.
	if strings.Contains(p, "org-1") || strings.Contains(strings.ToLower(p), "jane") {
		t.Errorf("link leaks data: %q", p)
	}
}

func TestGoogleAndOutlookLinks(t *testing.T) {
	in := sampleEvent()
	in.Summary = "Full service & oil change — Zoë + Jane"
	in.Description += "\nNotes: 50% deposit paid; call +254 700 111222 <before> arriving"

	g := GoogleCalendarURL(in)
	if !strings.HasPrefix(g, "https://calendar.google.com/calendar/render?action=TEMPLATE&text=") || strings.Contains(g, "+") || strings.Contains(g, " ") {
		t.Fatalf("google url %q", g)
	}
	if !strings.Contains(g, "&dates=20261003T060000Z/20261003T073000Z&") {
		t.Fatalf("google dates are not UTC basic format: %q", g)
	}
	gu, err := url.Parse(g)
	if err != nil {
		t.Fatal(err)
	}
	gq := gu.Query()
	if gq.Get("text") != in.Summary || gq.Get("location") != "12 Ngong Rd, Kilimani" || gq.Get("action") != "TEMPLATE" {
		t.Fatalf("google query %v", gq)
	}
	for _, s := range []string{"Customer: Jane Wanjiru\nPhone: +254 700 111222", "50% deposit paid; call +254 700 111222 <before> arriving", "\n\nOpen booking: https://erp.example/bookings?booking=4f1c-77"} {
		if !strings.Contains(gq.Get("details"), s) {
			t.Errorf("google details lack %q: %q", s, gq.Get("details"))
		}
	}

	for base, host := range map[string]string{OutlookWorkURL: "outlook.office.com", OutlookLiveURL: "outlook.live.com"} {
		o := OutlookCalendarURL(base, in)
		if !strings.HasPrefix(o, "https://"+host+"/calendar/0/deeplink/compose?path=/calendar/action/compose&rru=addevent&subject=") || strings.Contains(o, "+") || strings.Contains(o, " ") {
			t.Fatalf("outlook url %q", o)
		}
		ou, err := url.Parse(o)
		if err != nil {
			t.Fatal(err)
		}
		oq := ou.Query()
		if oq.Get("subject") != in.Summary || oq.Get("startdt") != "2026-10-03T06:00:00Z" || oq.Get("enddt") != "2026-10-03T07:30:00Z" || oq.Get("location") != "12 Ngong Rd, Kilimani" ||
			oq.Get("path") != "/calendar/action/compose" || oq.Get("rru") != "addevent" {
			t.Fatalf("outlook query %v", oq)
		}
		// The body is HTML: line breaks become <br>, markup is escaped.
		if body := oq.Get("body"); !strings.Contains(body, "Customer: Jane Wanjiru<br>Phone: +254 700 111222") || !strings.Contains(body, "&lt;before&gt; arriving") || strings.Contains(body, "\n") ||
			!strings.Contains(body, "Open booking: https://erp.example/bookings?booking=4f1c-77") {
			t.Fatalf("outlook body %q", body)
		}
	}

	// Times given in another zone still come out in UTC.
	nairobi, _ := time.LoadLocation("Africa/Nairobi")
	in.Booking.StartTime, in.Booking.EndTime = in.Booking.StartTime.In(nairobi), in.Booking.EndTime.In(nairobi)
	if !strings.Contains(GoogleCalendarURL(in), "dates=20261003T060000Z/20261003T073000Z") || !strings.Contains(OutlookCalendarURL(OutlookWorkURL, in), "startdt=2026-10-03T06%3A00%3A00Z") {
		t.Fatal("links are not in UTC")
	}

	// Long notes are cut so the link stays usable — the booking link survives.
	in.Description = "Notes: " + strings.Repeat("très long détail 日本語 ", 400)
	in.Summary = strings.Repeat("Very long summary ", 30)
	in.Location = strings.Repeat("Somewhere far away, ", 30)
	for _, u := range []string{GoogleCalendarURL(in), OutlookCalendarURL(OutlookWorkURL, in), OutlookCalendarURL(OutlookLiveURL, in)} {
		if len(u) > 1800 {
			t.Errorf("link is %d bytes", len(u))
		}
		pu, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		d := pu.Query().Get("details") + pu.Query().Get("body")
		if !strings.Contains(d, "Open booking: https://erp.example/bookings?booking=4f1c-77") {
			t.Errorf("link lost the booking link: %q", d)
		}
		if n := len([]rune(pu.Query().Get("text") + pu.Query().Get("subject"))); n > 150 {
			t.Errorf("title is %d runes", n)
		}
	}
}

func TestSubscribeLinks(t *testing.T) {
	l := SubscribeLinks("https://api.example.com/api/v1/public/calendar/tok_EN-123.ics", "DASYIN bookings — Amina Otieno")
	if l.URL != "https://api.example.com/api/v1/public/calendar/tok_EN-123.ics" || l.Webcal != "webcal://api.example.com/api/v1/public/calendar/tok_EN-123.ics" {
		t.Fatalf("%+v", l)
	}
	if l.Google != "https://calendar.google.com/calendar/r?cid=webcal%3A%2F%2Fapi.example.com%2Fapi%2Fv1%2Fpublic%2Fcalendar%2Ftok_EN-123.ics" {
		t.Fatalf("google %q", l.Google)
	}
	for u, host := range map[string]string{l.Outlook: "outlook.office.com", l.OutlookLive: "outlook.live.com"} {
		pu, err := url.Parse(u)
		if err != nil || pu.Host != host || pu.Path != "/calendar/0/addfromweb" || pu.Query().Get("url") != l.URL || pu.Query().Get("name") != "DASYIN bookings — Amina Otieno" || strings.Contains(u, "+") {
			t.Fatalf("outlook %q", u)
		}
	}
	if SubscribeLinks("http://localhost:8501/x.ics", "n").Webcal != "webcal://localhost:8501/x.ics" {
		t.Fatal("webcal for http")
	}
}

func TestFeedCalendar(t *testing.T) {
	a := sampleEvent()
	b := sampleEvent()
	bk := *b.Booking
	bk.ID = "second"
	b.Booking, b.Cancelled, b.Summary = &bk, true, "Cancelled: Oil change — Sam"
	b.Stamp = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	ics := BuildFeed(FeedInput{Name: "DASYIN bookings — Amina, Otieno", Description: "Bookings assigned to you", Events: []ICSInput{a, b}})
	cal := parseICS(t, ics)
	if cal.get(t, "METHOD") != "PUBLISH" || unescapeText(t, cal.get(t, "X-WR-CALNAME")) != "DASYIN bookings — Amina, Otieno" || cal.get(t, "X-PUBLISHED-TTL") != "PT30M" {
		t.Fatalf("feed head %+v", cal.Props)
	}
	ri := cal.all("REFRESH-INTERVAL")
	if len(ri) != 1 || ri[0].Params != ";VALUE=DURATION" || ri[0].Value != "PT30M" {
		t.Fatalf("refresh interval %+v", ri)
	}
	evs := cal.subs("VEVENT")
	if len(evs) != 2 || evs[0].get(t, "STATUS") != "CONFIRMED" || evs[1].get(t, "STATUS") != "CANCELLED" || evs[1].get(t, "DTSTAMP") != "20260930T000000Z" {
		t.Fatalf("feed events")
	}
	if strings.Contains(ics, "ORGANIZER") || strings.Contains(ics, "ATTENDEE") {
		t.Fatal("feed has attendees")
	}
	// An empty feed is still a valid calendar.
	if c := parseICS(t, BuildFeed(FeedInput{Name: "x"})); len(c.Subs) != 0 {
		t.Fatal("empty feed")
	}
}
