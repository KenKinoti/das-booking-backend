package meetings

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func sampleMeeting() *Meeting {
	start := time.Date(2026, 10, 5, 23, 0, 0, 0, time.UTC) // 10:00 Sydney (AEDT)
	return &Meeting{
		ID: "abc-123", Title: "Quarterly review; budget, plans", Description: "Agenda:\n1. Numbers\n2. Hiring \\ plans",
		StartAt: start, EndAt: start.Add(45 * time.Minute), Timezone: "Australia/Sydney",
		JoinURL: "https://app.example.com/meetings/abc-123", OrganizerName: "Kennedy K", OrganizerEmail: "kennedy@example.com",
		Status: StatusScheduled, Sequence: 2, ReminderMinutes: 15, Recurrence: "weekly", RecurrenceCount: 4,
		CreatedAt: start.Add(-48 * time.Hour), UpdatedAt: start.Add(-24 * time.Hour),
		Participants: []Participant{
			{Name: "Alice \"Al\" Smith", Email: "alice@example.com", Response: "needs_action"},
			{Name: "Bob", Email: "bob@example.com", Response: "accepted"},
		},
	}
}

func unfold(s string) string { return strings.ReplaceAll(s, "\r\n ", "") }

func TestBuildICSRequest(t *testing.T) {
	now := time.Date(2026, 9, 24, 1, 2, 3, 0, time.UTC)
	ics := BuildICS(sampleMeeting(), ICSOptions{Method: MethodRequest, Now: now})

	for _, line := range strings.Split(strings.TrimSuffix(ics, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Errorf("line exceeds 75 octets (%d): %q", len(line), line)
		}
	}
	if strings.Contains(strings.ReplaceAll(ics, "\r\n", ""), "\n") {
		t.Error("bare LF found; all lines must end with CRLF")
	}
	u := unfold(ics)
	want := []string{
		"BEGIN:VCALENDAR\r\n", "VERSION:2.0\r\n", "METHOD:REQUEST\r\n", "BEGIN:VEVENT\r\n",
		"UID:abc-123@dasyin-erp\r\n", "DTSTAMP:20260924T010203Z\r\n",
		"DTSTART:20261005T230000Z\r\n", "DTEND:20261005T234500Z\r\n", "SEQUENCE:2\r\n",
		`SUMMARY:Quarterly review\; budget\, plans` + "\r\n",
		`DESCRIPTION:Agenda:\n1. Numbers\n2. Hiring \\ plans\n\nJoin: https://app.example.com/meetings/abc-123`,
		"ORGANIZER;CN=\"Kennedy K\":mailto:kennedy@example.com\r\n",
		"ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE;CN=\"Alice Al Smith\":mailto:alice@example.com\r\n",
		"PARTSTAT=ACCEPTED;RSVP=TRUE;CN=\"Bob\":mailto:bob@example.com\r\n",
		"RRULE:FREQ=WEEKLY;COUNT=4\r\n", "STATUS:CONFIRMED\r\n",
		"BEGIN:VALARM\r\n", "TRIGGER:-PT15M\r\n", "END:VEVENT\r\n", "END:VCALENDAR\r\n",
	}
	for _, w := range want {
		if !strings.Contains(u, w) {
			t.Errorf("ICS missing %q\n%s", w, u)
		}
	}
	if !strings.HasSuffix(ics, "END:VCALENDAR\r\n") {
		t.Error("ICS must end with END:VCALENDAR CRLF")
	}
}

func TestBuildICSCancel(t *testing.T) {
	m := sampleMeeting()
	m.Status = StatusCancelled
	ics := unfold(BuildICS(m, ICSOptions{Method: MethodCancel}))
	for _, w := range []string{"METHOD:CANCEL\r\n", "STATUS:CANCELLED\r\n", "SEQUENCE:2\r\n"} {
		if !strings.Contains(ics, w) {
			t.Errorf("cancel ICS missing %q", w)
		}
	}
	if strings.Contains(ics, "RRULE") || strings.Contains(ics, "VALARM") {
		t.Error("cancel ICS should not contain RRULE or VALARM")
	}
	one := unfold(BuildICS(m, ICSOptions{Method: MethodCancel, Attendees: m.Participants[:1]}))
	if strings.Contains(one, "bob@example.com") {
		t.Error("attendee override not honoured")
	}
}

func TestFoldLineUTF8(t *testing.T) {
	line := "SUMMARY:" + strings.Repeat("é🙂a", 40)
	f := FoldLine(line)
	parts := strings.Split(f, "\r\n")
	if len(parts) < 2 {
		t.Fatal("expected folding")
	}
	for i, p := range parts {
		if len(p) > 75 {
			t.Errorf("part %d too long: %d", i, len(p))
		}
		if i > 0 && !strings.HasPrefix(p, " ") {
			t.Errorf("continuation %d must start with space", i)
		}
		if !utf8.ValidString(p) {
			t.Errorf("part %d splits a UTF-8 sequence", i)
		}
	}
	if unfold(f) != line {
		t.Error("unfolding does not restore the original line")
	}
	if FoldLine("SHORT:x") != "SHORT:x" {
		t.Error("short lines must not change")
	}
}

func TestEscapeText(t *testing.T) {
	got := EscapeText("a,b;c\\d\r\ne\nf")
	want := `a\,b\;c\\d\ne\nf`
	if got != want {
		t.Errorf("EscapeText = %q, want %q", got, want)
	}
}

func TestBuildMIME(t *testing.T) {
	msg := BuildMIME("noreply@example.com", InviteMessage{To: "a@example.com", ToName: "A", Subject: "Invitation: Test", Text: "hi", HTML: "<p>hi</p>", ICS: "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n", Method: MethodRequest, FromName: "Kennedy"})
	s := string(msg)
	for _, w := range []string{"multipart/mixed", "multipart/alternative", "text/calendar; charset=UTF-8; method=REQUEST", `filename="invite.ics"`, "text/plain", "text/html"} {
		if !strings.Contains(s, w) {
			t.Errorf("MIME missing %q", w)
		}
	}
}

const sampleTranscript = `[10:00:02] You: Welcome everyone, today we are reviewing the website launch plan and the marketing budget.
[10:01:10] Alice: The website launch is scheduled for next month and the design is almost finished.
[10:02:30] Bob: The marketing budget for the launch is currently twenty thousand dollars.
[10:03:05] Alice: We agreed to move the website launch to the fifteenth of November.
[10:04:00] Bob: Sarah will prepare the launch email campaign by Friday.
[10:05:12] You: I will send the updated budget to finance by next Monday.
[10:06:40] Alice: Do we have enough time for testing?
[10:07:00] Bob: We need to follow up with the hosting provider about the launch traffic.
[10:08:00] You: The launch plan and the budget look good, thanks everyone.`

func TestExtractiveSummary(t *testing.T) {
	r := ExtractiveSummary(sampleTranscript)
	if r.Source != "local" {
		t.Errorf("source = %q", r.Source)
	}
	if r.Summary == "" || strings.Contains(r.Summary, "[10:") || strings.Contains(r.Summary, "You:") {
		t.Errorf("summary should be non-empty and free of timestamps/labels: %q", r.Summary)
	}
	if !strings.Contains(strings.ToLower(r.Summary), "launch") {
		t.Errorf("summary should mention the dominant topic: %q", r.Summary)
	}
	if len(r.KeyDecisions) != 1 || !strings.Contains(r.KeyDecisions[0], "agreed to move") {
		t.Errorf("decisions = %#v", r.KeyDecisions)
	}
	byText := map[string]ActionItem{}
	for _, a := range r.ActionItems {
		byText[a.Text] = a
		if strings.HasSuffix(a.Text, "?") {
			t.Errorf("questions are not action items: %q", a.Text)
		}
	}
	sarah, found := byText["Sarah will prepare the launch email campaign by Friday."]
	if !found || sarah.Owner != "Sarah" || sarah.Due != "Friday" {
		t.Errorf("Sarah action = %#v (found %v); all=%#v", sarah, found, r.ActionItems)
	}
	me, found := byText["I will send the updated budget to finance by next Monday."]
	if !found || me.Owner != "You" || me.Due != "next Monday" {
		t.Errorf("first-person action = %#v (found %v)", me, found)
	}
	if _, found := byText["We need to follow up with the hosting provider about the launch traffic."]; !found {
		t.Error("follow up action not detected")
	}
}

func TestExtractiveSummaryEmpty(t *testing.T) {
	r := ExtractiveSummary("   ")
	if r.Summary == "" || r.KeyDecisions == nil || r.ActionItems == nil {
		t.Errorf("empty input should still produce a result: %#v", r)
	}
}

func TestTranscriptText(t *testing.T) {
	m := sampleMeeting()
	tr := &Transcript{Text: "You: hello", Summary: "Short.", KeyDecisions: []string{"Ship it."}, ActionItems: []ActionItem{{Text: "Send notes.", Owner: "Bob", Due: "Friday"}}, UpdatedAt: m.StartAt}
	s := TranscriptText(m, tr)
	iT, iS, iD, iA := strings.Index(s, "TRANSCRIPT\r\n="), strings.Index(s, "SUMMARY"), strings.Index(s, "KEY DECISIONS"), strings.Index(s, "ACTION ITEMS")
	if !(iT > 0 && iT < iS && iS < iD && iD < iA) {
		t.Errorf("sections out of order:\n%s", s)
	}
	for _, w := range []string{"Title:        Quarterly review", "alice@example.com", "1. [ ] Send notes. — Owner: Bob — Due: Friday", "Tuesday 6 October 2026"} {
		if !strings.Contains(s, w) {
			t.Errorf("txt missing %q", w)
		}
	}
}
