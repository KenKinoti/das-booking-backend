package broadcast

import (
	"strings"
	"testing"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/occasions"
)

func TestSanitizeKeepsTheSimpleRichText(t *testing.T) {
	in := `<div>Hello <b>owners</b> &amp; <i>managers</i></div><div><br></div>` +
		`<ul><li>One <a href="https://dasyin.com/x?a=1&amp;b=2">link</a></li><li>Two <a href="/plan">plans</a></li></ul>` +
		`<p onclick="evil()">Para<script>alert(1)</script> <span style="color:red">kept</span></p>` +
		`<a href="javascript:alert(1)">no link</a><img src=x onerror=alert(1)><style>p{}</style><iframe src="https://x"></iframe>`
	out, text := Sanitize(in)
	want := `<p>Hello <strong>owners</strong> &amp; <em>managers</em></p>` +
		`<ul><li>One <a href="https://dasyin.com/x?a=1&amp;b=2" target="_blank" rel="noopener noreferrer">link</a></li><li>Two <a href="/plan">plans</a></li></ul>` +
		`<p>Para kept</p><p>no link</p>`
	if out != want {
		t.Fatalf("sanitised\n got %s\nwant %s", out, want)
	}
	for _, bad := range []string{"script", "onclick", "javascript", "onerror", "<img", "style", "iframe", "alert"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q survived: %s", bad, out)
		}
	}
	if text != "Hello owners & managers\n\n• One link\n• Two plans\n\nPara kept\n\nno link" {
		t.Fatalf("text %q", text)
	}
	// Sanitising is stable.
	if again, _ := Sanitize(out); again != out {
		t.Fatalf("not idempotent:\n%s\n%s", out, again)
	}
}

func TestSanitizeTricks(t *testing.T) {
	cases := map[string]string{
		`<a href="JaVaScRiPt:alert(1)">x</a>`:                 `<p>x</p>`,
		`<a href=" data:text/html;base64,xx">x</a>`:           `<p>x</p>`,
		`<a href="//evil.example/">x</a>`:                     `<p>x</p>`,
		`<a href="mailto:help@dasyin.com">mail</a>`:           `<p><a href="mailto:help@dasyin.com" target="_blank" rel="noopener noreferrer">mail</a></p>`,
		`<p title="a>b">ok</p>`:                               `<p>ok</p>`,
		`<scr<script>ipt>alert(1)</script>`:                   `<p>ipt&gt;alert(1)</p>`,
		`1 < 2 and <3`:                                        `<p>1 &lt; 2 and &lt;3</p>`,
		`<!-- <script>x</script> -->hi`:                       `<p>hi</p>`,
		`<ul><li>a<ul><li>b</li></ul></li><li>c</li></ul>`:    `<ul><li>a<ul><li>b</li></ul></li><li>c</li></ul>`,
		`<li>orphan</li>`:                                     `<p>orphan</p>`,
		`<p><br></p><p>&nbsp;</p>`:                            ``,
		`<strong><a href="https://a.co">x</a></strong> after`: `<p><strong><a href="https://a.co" target="_blank" rel="noopener noreferrer">x</a></strong> after</p>`,
		`<a href="https://user:pw@a.co">creds</a>`:            `<p>creds</p>`,
		`<svg><a href="https://a.co">x</a></svg>visible`:      `<p>visible</p>`,
		`<b>unclosed <i>both`:                                 `<p><strong>unclosed <em>both</em></strong></p>`,
		`<h1>Heading</h1>text`:                                `<p>Heading</p><p>text</p>`,
		`<ol><li>first</li><li>second</li></ol>`:              `<ol><li>first</li><li>second</li></ol>`,
		`<a href="https://a.co"><a href="https://b.co">x</a>`: `<p><a href="https://a.co" target="_blank" rel="noopener noreferrer">x</a></p>`,
		`<p>a</p><p>b</p>`:                                    `<p>a</p><p>b</p>`,
		`<div>line one<br>line two</div>`:                     `<p>line one<br>line two</p>`,
		`<button>no</button><textarea>no</textarea>yes`:       `<p>yes</p>`,
		`<a href="https://a.co" onmouseover="x()">safe</a>`:   `<p><a href="https://a.co" target="_blank" rel="noopener noreferrer">safe</a></p>`,
		"<a href='https://a.co/\"onmouseover=x'>q</a>":        `<p>q</p>`,
		`<p>tab	and
newline</p>`: `<p>tab and newline</p>`,
	}
	for in, want := range cases {
		if got, _ := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q)\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestPlainTextBecomesParagraphsAndLists(t *testing.T) {
	out, _ := Sanitize("Hello all\nsecond line\n\n- one\n- two https://dasyin.com/x.\n\n1. first\n2. second")
	want := `<p>Hello all<br>second line</p><ul><li>one</li><li>two <a href="https://dasyin.com/x" target="_blank" rel="noopener noreferrer">https://dasyin.com/x</a>.</li></ul><ol><li>first</li><li>second</li></ol>`
	if out != want {
		t.Fatalf("got %s", out)
	}
	if txt := PlainText(out, true, ""); txt != "Hello all\nsecond line\n\n• one\n• two https://dasyin.com/x.\n\n1. first\n2. second" {
		t.Fatalf("text %q", txt)
	}
	em := EmailHTML(`<p>See <a href="/plan">plans</a></p>`, "#4f46e5", "https://app.dasyin.com")
	if !strings.Contains(em, `href="https://app.dasyin.com/plan"`) || !strings.Contains(em, `style="margin:0 0 14px`) {
		t.Fatalf("email html %s", em)
	}
	if txt := PlainText(`<p>See <a href="/plan">plans</a></p>`, true, "https://app.dasyin.com"); txt != "See plans (https://app.dasyin.com/plan)" {
		t.Fatalf("text links %q", txt)
	}
}

func TestLinksForButtonsAndImages(t *testing.T) {
	for in, ok := range map[string]bool{"/plan": true, "https://dasyin.com/offer": true, "http://dasyin.com": false, "mailto:a@b.co": false,
		"javascript:alert(1)": false, "//evil.com": false, "ftp://x": false, "": false} {
		if _, got := SafeCTA(in); got != ok {
			t.Errorf("SafeCTA(%q) = %v", in, got)
		}
	}
	for in, ok := range map[string]bool{"https://cdn.dasyin.com/a.png": true, "http://cdn/a.png": false, "/img.png": false, "data:image/png;base64,xx": false} {
		if _, got := SafeImage(in); got != ok {
			t.Errorf("SafeImage(%q) = %v", in, got)
		}
	}
}

func TestApplyValidates(t *testing.T) {
	SetClockOffset(0)
	a := &Announcement{}
	if err := Apply(a, Input{Title: "", Body: "x"}); err == nil || err.(*FieldError).Field != "title" {
		t.Fatalf("title: %v", err)
	}
	if err := Apply(a, Input{Title: "T", Body: "<p> </p><script>x</script>"}); err == nil || err.(*FieldError).Field != "body" {
		t.Fatalf("body: %v", err)
	}
	if err := Apply(a, Input{Title: "T", Body: "x", CTALabel: "Go", CTAURL: "javascript:x"}); err == nil || err.(*FieldError).Field != "cta_url" {
		t.Fatalf("cta: %v", err)
	}
	if err := Apply(a, Input{Title: "T", Body: "x", ImageURL: "http://x/a.png"}); err == nil || err.(*FieldError).Field != "image_url" {
		t.Fatalf("image: %v", err)
	}
	bad := 120
	if err := Apply(a, Input{Title: "T", Body: "x", EmailDelayMinutes: &bad}); err == nil {
		t.Fatal("delay 2h accepted")
	}
	if err := Apply(a, Input{Title: "T", Body: "x", Timezone: "Mars/Base"}); err == nil {
		t.Fatal("bad zone accepted")
	}
	// Urgent: priority urgent and the 1-hour default; others 3 hours.
	if err := Apply(a, Input{Title: "Outage", Body: "x", Kind: KindUrgent}); err != nil || a.Priority != PriorityUrgent || a.EmailDelayMinutes != 60 || !ServiceNotice(a) {
		t.Fatalf("urgent %v %+v", err, a)
	}
	if err := Apply(a, Input{Title: "News", Body: "x", Kind: KindUpdate}); err != nil || a.EmailDelayMinutes != 180 || ServiceNotice(a) {
		t.Fatalf("update %v %+v", err, a)
	}
	never := NeverEmail
	if err := Apply(a, Input{Title: "Offer", Body: "x", Kind: KindPromotion, Priority: PriorityUrgent, EmailDelayMinutes: &never}); err != nil || a.EmailDelayMinutes != NeverEmail || ServiceNotice(a) {
		t.Fatalf("an urgent offer is never a service notice: %v %+v", err, a)
	}
	// Time zones: 08:00 in Nairobi is 05:00 UTC; in Sydney (AEDT, UTC+11) 21:00 the day before.
	if err := Apply(a, Input{Title: "T", Body: "x", LocalSendAt: "2027-01-20T08:00", Timezone: "Africa/Nairobi"}); err != nil || a.SendAt.Format(time.RFC3339) != "2027-01-20T05:00:00Z" {
		t.Fatalf("nairobi %v %v", err, a.SendAt)
	}
	if err := Apply(a, Input{Title: "T", Body: "x", LocalSendAt: "2027-01-20T08:00", Timezone: "Australia/Sydney"}); err != nil || a.SendAt.Format(time.RFC3339) != "2027-01-19T21:00:00Z" {
		t.Fatalf("sydney %v %v", err, a.SendAt)
	}
	if err := Apply(a, Input{Title: "T", Body: "x", LocalSendAt: "2027-01-20T08:00", LocalExpiresAt: "2027-01-20T07:00"}); err == nil {
		t.Fatal("expiry before the send accepted")
	}
	if err := Apply(a, Input{Title: "T", Body: "x", LocalSendAt: "2027-01-20T08:00", Recurrence: RepeatWeekly, RepeatUntil: "2027-01-01"}); err == nil {
		t.Fatal("end before start accepted")
	}
}

func TestRecurrenceAndZones(t *testing.T) {
	// Monthly on the 31st: the last day of short months, without drifting.
	want := []string{"2027-01-31T08:00", "2027-02-28T08:00", "2027-03-31T08:00", "2027-04-30T08:00"}
	for k, w := range want {
		at, err := Occurrence("2027-01-31T08:00", "Africa/Nairobi", RepeatMonthly, k)
		if err != nil || FormatLocal(at, "Africa/Nairobi") != w {
			t.Fatalf("monthly %d: %v %s", k, err, FormatLocal(at, "Africa/Nairobi"))
		}
	}
	// Weekly across Sydney's daylight-saving change (5 Apr 2026): same 09:00 local, the UTC hour moves.
	a, _ := Occurrence("2026-03-31T09:00", "Australia/Sydney", RepeatWeekly, 0)
	b, _ := Occurrence("2026-03-31T09:00", "Australia/Sydney", RepeatWeekly, 1)
	if a.Format(time.RFC3339) != "2026-03-30T22:00:00Z" || b.Format(time.RFC3339) != "2026-04-06T23:00:00Z" || FormatLocal(b, "Australia/Sydney") != "2026-04-07T09:00" {
		t.Fatalf("dst %s %s", a, b)
	}
	// NextSend skips missed occurrences and stops at the end date.
	ann := &Announcement{LocalSendAt: "2027-01-04T08:00", Timezone: "Africa/Nairobi", Recurrence: RepeatWeekly, RepeatUntil: "2027-01-25", Occurrences: 1}
	now, _ := ParseLocal("2027-01-19T09:00", "Africa/Nairobi")
	if n := NextSend(ann, now); n == nil || FormatLocal(*n, "Africa/Nairobi") != "2027-01-25T08:00" {
		t.Fatalf("next %v", n)
	}
	now, _ = ParseLocal("2027-01-25T09:00", "Africa/Nairobi")
	if n := NextSend(ann, now); n != nil {
		t.Fatalf("after the end: %v", n)
	}
	if n := NextSend(&Announcement{Recurrence: RepeatNone}, now); n != nil {
		t.Fatal("one-off repeats")
	}
	if got := UpcomingSends("2027-01-04T08:00", "Africa/Nairobi", RepeatWeekly, "2027-01-20", 10); len(got) != 3 {
		t.Fatalf("upcoming %v", got)
	}
	if DaysUntil(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) != 14 {
		t.Fatal("days until")
	}
}

func TestTokens(t *testing.T) {
	s := &Service{Secret: []byte("secret-1")}
	tok := s.Token(PurposeUnsub, "d-1", "")
	if id, _, err := s.ParseToken(tok, PurposeUnsub); err != nil || id != "d-1" {
		t.Fatalf("parse %v %s", err, id)
	}
	if _, _, err := s.ParseToken(tok, PurposeOpen); err == nil {
		t.Fatal("an unsubscribe token opened")
	}
	other := &Service{Secret: []byte("secret-2")}
	if _, _, err := other.ParseToken(tok, PurposeUnsub); err == nil {
		t.Fatal("another server's token accepted")
	}
	parts := strings.SplitN(tok, ".", 2)
	forged := s.Token(PurposeUnsub, "d-2", "")
	if _, _, err := s.ParseToken(strings.SplitN(forged, ".", 2)[0]+"."+parts[1], PurposeUnsub); err == nil {
		t.Fatal("swapped signature accepted")
	}
	if id, target, err := s.ParseToken(s.Token(PurposeOpen, "d-3", "cta"), PurposeOpen); err != nil || id != "d-3" || target != "cta" {
		t.Fatalf("open %v %s %s", err, id, target)
	}
	if MaskEmail("kennedy@dasyin.com") != "ke••••@dasyin.com" || MaskEmail("x") != "" {
		t.Fatalf("mask %s", MaskEmail("kennedy@dasyin.com"))
	}
}

func TestDraftsPerOccasionKind(t *testing.T) {
	SetClockOffset(0)
	st := Settings{SendHour: 8, Timezone: DefaultTimezone, NudgeLeadDays: DefaultLeadDays}
	day := time.Date(Now().Year()+1, 10, 20, 0, 0, 0, 0, time.UTC)
	d := (&Service{}).DraftFor(occasions.Occasion{Key: "mashujaa_day", Name: "Mashujaa Day", Kind: occasions.KindPublicHoliday, Date: day, Countries: []string{"KE"}, Important: true}, st)
	if d.Kind != KindReminder || !strings.Contains(d.Body, "Mashujaa Day") || d.LocalSendAt != day.Format(DateLayout)+"T08:00" || d.Timezone != "Africa/Nairobi" ||
		len(d.Audience.Countries) != 1 || d.Audience.Countries[0] != "KE" || !d.Audience.ExcludeSuperAdmins {
		t.Fatalf("holiday draft %+v", d)
	}
	if _, txt := Sanitize(d.Body); txt == "" {
		t.Fatal("draft body empty")
	}
	bf := (&Service{}).DraftFor(occasions.Occasion{Key: "black_friday", Name: "Black Friday", Kind: occasions.KindCommercial, Date: day, Important: true}, st)
	if bf.Kind != KindPromotion || bf.CTAURL != "/plan" || bf.Audience.RoleLevel != LevelOwners || bf.LocalSendAt != day.AddDate(0, 0, -5).Format(DateLayout)+"T08:00" {
		t.Fatalf("commercial draft %+v", bf)
	}
	au := (&Service{}).DraftFor(occasions.Occasion{Key: "australia_day", Name: "Australia Day", Kind: occasions.KindPublicHoliday, Date: day, Countries: []string{"AU"}}, st)
	if au.Timezone != "Australia/Sydney" {
		t.Fatalf("au zone %s", au.Timezone)
	}
	if got := cleanLeads([]int{3, 14, 3, -1, 200}); len(got) != 2 || got[0] != 14 || got[1] != 3 {
		t.Fatalf("leads %v", got)
	}
}

func TestParseAIDraft(t *testing.T) {
	r, err := parseAIDraft("Sure! {\"title\":\"Happy Mashujaa Day\",\"body_html\":\"<p>Hi <script>x</script>there</p>\",\"body_sw_html\":\"<p>Heri ya Mashujaa</p>\"}", AIRequest{Swahili: true}, "m")
	if err != nil || r.Title != "Happy Mashujaa Day" || !r.Swahili || r.Body != "<p>Hi there</p><p><strong>Kwa Kiswahili</strong></p><p>Heri ya Mashujaa</p>" {
		t.Fatalf("json %v %+v", err, r)
	}
	r, err = parseAIDraft("Just a plain answer\n\nwith two paragraphs", AIRequest{OccasionName: "Diwali"}, "m")
	if err != nil || r.Title != "Diwali" || r.Body != "<p>Just a plain answer</p><p>with two paragraphs</p>" {
		t.Fatalf("text %v %+v", err, r)
	}
	if _, err := parseAIDraft("  ", AIRequest{}, "m"); err == nil {
		t.Fatal("empty accepted")
	}
	p := (&Service{}).aiPrompt(AIRequest{OccasionName: "Mashujaa Day", OccasionDate: "2026-10-20", Kind: KindReminder, Swahili: true, Audience: Audience{Countries: []string{"KE"}, RoleLevel: LevelOwners}})
	for _, w := range []string{"Mashujaa Day on Tuesday 20 October 2026", "Kenya", "Kiswahili", "body_html", "owners"} {
		if !strings.Contains(p, w) {
			t.Fatalf("prompt lacks %q:\n%s", w, p)
		}
	}
}

func TestEmailRendering(t *testing.T) {
	s := &Service{Secret: []byte("k"), AppURL: "https://app.dasyin.test", APIURL: "https://api.dasyin.test"}
	a := &Announcement{ID: "a1", Title: "Black Friday deals", Kind: KindPromotion, Priority: PriorityNormal, CTALabel: "See plans", CTAURL: "/plan",
		ImageURL: "https://cdn.dasyin.test/bf.png"}
	a.Body, a.BodyText = Sanitize("<p>Big <strong>savings</strong></p><ul><li>One</li></ul>")
	d := &Delivery{ID: "d1", Email: "owner@club.test"}
	m := s.EmailFor(a, d, false)
	if m.Subject != "Black Friday deals" || m.To[0] != "owner@club.test" {
		t.Fatalf("headers %+v", m)
	}
	for _, w := range []string{"<strong>savings</strong>", "https://cdn.dasyin.test/bf.png", "Unsubscribe from DASYIN offers", "https://app.dasyin.test/unsubscribe?t=",
		"https://api.dasyin.test/api/v1/public/announcements/open?t=", "See plans", "Offer from DASYIN"} {
		if !strings.Contains(m.HTML, w) {
			t.Fatalf("html lacks %q", w)
		}
	}
	if strings.Contains(m.HTML, bodyMarker) || !strings.Contains(m.Text, "Big savings") || !strings.Contains(m.Text, "Unsubscribe from DASYIN offers: https://app.dasyin.test/unsubscribe?t=") {
		t.Fatalf("text %s", m.Text)
	}
	u := &Announcement{ID: "a2", Title: "Maintenance tonight", Kind: KindUrgent, Priority: PriorityUrgent}
	u.Body, u.BodyText = Sanitize("We restart at 23:00.")
	m = s.EmailFor(u, d, true)
	if m.Subject != "[Test] Urgent: Maintenance tonight" || strings.Contains(m.HTML, "Unsubscribe") || !strings.Contains(m.HTML, "sent even if you turned off update emails") {
		t.Fatalf("urgent %s", m.Subject)
	}
}

func TestCalendarOnePerDay(t *testing.T) {
	day := time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)
	s := &Service{Occasions: func(from, to time.Time, country string) []occasions.Occasion {
		return []occasions.Occasion{
			{Key: "christmas_day", Name: "Christmas Day", Kind: occasions.KindObservance, Date: day, Important: true},
			{Key: "christmas_day", Name: "Christmas Day", Kind: occasions.KindPublicHoliday, Date: day, Countries: []string{"KE", "AU"}},
			{Key: "boxing_day", Name: "Boxing Day", Kind: occasions.KindPublicHoliday, Date: day.AddDate(0, 0, 1), Countries: []string{"KE", "AU"}, Important: true},
		}
	}}
	got := s.calendar(day, day.AddDate(0, 0, 1))
	if len(got) != 2 || got[0].Kind != occasions.KindPublicHoliday || !got[0].Important || len(got[0].Countries) != 2 || got[1].Key != "boxing_day" {
		t.Fatalf("calendar %+v", got)
	}
}
