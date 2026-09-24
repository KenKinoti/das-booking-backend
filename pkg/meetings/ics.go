package meetings

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ICS methods.
const (
	MethodRequest = "REQUEST"
	MethodCancel  = "CANCEL"
	MethodPublish = "PUBLISH"
)

const icsTimeFormat = "20060102T150405Z"

// ICSOptions controls calendar generation.
type ICSOptions struct {
	Method string
	// Attendees overrides the meeting participants (e.g. to cancel for a
	// single removed participant). Nil means all participants.
	Attendees []Participant
	Now       time.Time
	// Domain is used for the UID (<id>@Domain).
	Domain string
}

// EventUID returns the stable iCalendar UID for a meeting.
func EventUID(m *Meeting, domain string) string {
	if domain == "" {
		domain = "dasyin-erp"
	}
	return m.ID + "@" + domain
}

// BuildICS renders an RFC 5545 VCALENDAR for the meeting.
func BuildICS(m *Meeting, opt ICSOptions) string {
	method := strings.ToUpper(opt.Method)
	if method == "" {
		method = MethodRequest
	}
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	attendees := opt.Attendees
	if attendees == nil {
		attendees = m.Participants
	}

	var lines []string
	add := func(s string) { lines = append(lines, s) }

	add("BEGIN:VCALENDAR")
	add("PRODID:-//DASYIN//ERP Meetings//EN")
	add("VERSION:2.0")
	add("CALSCALE:GREGORIAN")
	add("METHOD:" + method)
	add("BEGIN:VEVENT")
	add("UID:" + EventUID(m, opt.Domain))
	add("DTSTAMP:" + now.UTC().Format(icsTimeFormat))
	add("DTSTART:" + m.StartAt.UTC().Format(icsTimeFormat))
	add("DTEND:" + m.EndAt.UTC().Format(icsTimeFormat))
	add(fmt.Sprintf("SEQUENCE:%d", m.Sequence))
	add("SUMMARY:" + EscapeText(m.Title))

	desc := m.Description
	if m.JoinURL != "" {
		if desc != "" {
			desc += "\n\n"
		}
		desc += "Join: " + m.JoinURL
	}
	if m.Timezone != "" {
		if loc, err := time.LoadLocation(m.Timezone); err == nil {
			if desc != "" {
				desc += "\n"
			}
			desc += fmt.Sprintf("Time: %s – %s (%s)", m.StartAt.In(loc).Format("Mon 2 Jan 2006 15:04"), m.EndAt.In(loc).Format("15:04"), m.Timezone)
		}
	}
	if desc != "" {
		add("DESCRIPTION:" + EscapeText(desc))
	}
	loc := m.Location
	if loc == "" {
		loc = m.JoinURL
	}
	if loc != "" {
		add("LOCATION:" + EscapeText(loc))
	}
	if m.JoinURL != "" {
		add("URL:" + m.JoinURL)
	}
	if m.OrganizerEmail != "" {
		add("ORGANIZER" + cnParam(m.OrganizerName) + ":mailto:" + m.OrganizerEmail)
	}
	for _, p := range attendees {
		if p.Email == "" {
			continue
		}
		partstat := "NEEDS-ACTION"
		switch p.Response {
		case "accepted":
			partstat = "ACCEPTED"
		case "declined":
			partstat = "DECLINED"
		case "tentative":
			partstat = "TENTATIVE"
		}
		add("ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=" + partstat + ";RSVP=TRUE" + cnParam(p.Name) + ":mailto:" + p.Email)
	}
	if rr := recurrenceRule(m); rr != "" && method != MethodCancel {
		add("RRULE:" + rr)
	}
	if method == MethodCancel || m.Status == StatusCancelled {
		add("STATUS:CANCELLED")
	} else {
		add("STATUS:CONFIRMED")
	}
	add("TRANSP:OPAQUE")
	add("CREATED:" + m.CreatedAt.UTC().Format(icsTimeFormat))
	add("LAST-MODIFIED:" + m.UpdatedAt.UTC().Format(icsTimeFormat))
	if m.ReminderMinutes > 0 && method != MethodCancel {
		add("BEGIN:VALARM")
		add("ACTION:DISPLAY")
		add("DESCRIPTION:" + EscapeText("Reminder: "+m.Title))
		add(fmt.Sprintf("TRIGGER:-PT%dM", m.ReminderMinutes))
		add("END:VALARM")
	}
	add("END:VEVENT")
	add("END:VCALENDAR")

	var b strings.Builder
	for _, l := range lines {
		b.WriteString(FoldLine(l))
		b.WriteString("\r\n")
	}
	return b.String()
}

func recurrenceRule(m *Meeting) string {
	freq := ""
	switch m.Recurrence {
	case "daily":
		freq = "DAILY"
	case "weekly":
		freq = "WEEKLY"
	default:
		return ""
	}
	count := m.RecurrenceCount
	if count <= 0 {
		count = 10
	}
	return fmt.Sprintf("FREQ=%s;COUNT=%d", freq, count)
}

// cnParam renders ;CN="..." with characters that are illegal in a quoted
// parameter value removed.
func cnParam(name string) string {
	name = strings.TrimSpace(strings.NewReplacer("\"", "", "\r", " ", "\n", " ").Replace(name))
	if name == "" {
		return ""
	}
	return `;CN="` + name + `"`
}

// EscapeText escapes a TEXT property value per RFC 5545 §3.3.11.
func EscapeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`).Replace(s)
}

// FoldLine folds a content line to at most 75 octets per physical line
// (RFC 5545 §3.1) without splitting UTF-8 sequences. Continuation lines
// begin with a single space.
func FoldLine(line string) string {
	const limit = 75
	if len(line) <= limit {
		return line
	}
	var b strings.Builder
	cur := 0 // octets on the current physical line
	max := limit
	for len(line) > 0 {
		_, size := utf8.DecodeRuneInString(line)
		if cur+size > max {
			b.WriteString("\r\n ")
			cur = 0
			max = limit - 1 // leading space counts toward the 75 octets
		}
		b.WriteString(line[:size])
		cur += size
		line = line[size:]
	}
	return b.String()
}
