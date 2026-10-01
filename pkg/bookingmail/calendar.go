package bookingmail

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/meetings"
)

// ---------------------------------------------------------------------------
// Calendar data for a booking comes in two flavours, because mail clients and
// calendar apps read the same file very differently:
//
//   - Invitation (BuildICS) — the inline text/calendar part of the email only.
//     iTIP METHOD:REQUEST / CANCEL with ORGANIZER and ATTENDEE, so Gmail,
//     Outlook and Apple Mail draw their event card. The attendee carries
//     PARTSTAT=NEEDS-ACTION;RSVP=FALSE: NEEDS-ACTION is what RFC 5546 expects
//     in a REQUEST (Gmail and Apple Mail only draw the card for it), and
//     RSVP=FALSE tells the client no reply is wanted (Outlook then shows "no
//     response required" and does not mail the organiser).
//
//   - Add-to-calendar file (BuildAddICS) — the email attachment and every
//     download link. METHOD:PUBLISH with NO organiser or attendee: opened in
//     a calendar app it is a plain event the person owns ("Add to calendar" on
//     iPhone, an editable event on import into Google Calendar, "Save &
//     Close" in Outlook) rather than a read-only invitation from somebody
//     else's account. It deliberately has no X-WR-CALNAME — Outlook opens a
//     file with a calendar name as a separate new calendar instead of adding
//     the one event.
//
// Both share the UID and SEQUENCE of the booking, so adding the file again
// after a change updates the same event. A third form, the subscription feed
// (BuildFeed), lists many bookings under one named calendar.
// ---------------------------------------------------------------------------

// iCalendar methods.
const (
	MethodRequest = "REQUEST"
	MethodCancel  = "CANCEL"
	MethodPublish = "PUBLISH"
)

const (
	icsTime = "20060102T150405Z"
	prodID  = "-//DASYIN//ERP Bookings//EN"
)

// UID is the stable iCalendar UID of a booking.
func UID(bookingID string) string {
	return "booking-" + strings.Map(func(r rune) rune {
		if r < 0x21 || r == 0x7f {
			return -1
		}
		return r
	}, bookingID) + "@dasyin-erp"
}

// ICSInput is what a booking's calendar entry shows.
type ICSInput struct {
	Booking *models.Booking
	// Method: REQUEST | CANCEL for the invitation, PUBLISH | CANCEL for the
	// add-to-calendar file. Ignored by the feed.
	Method   string
	Sequence int
	Summary  string
	// Description without the link; "<LinkLabel>: <URL>" is appended.
	Description string
	Location    string
	URL         string
	LinkLabel   string // default "Open booking"
	Category    string // CATEGORIES, default "Booking"
	// Reminder is the alarm lead time in minutes (0 = no alarm).
	Reminder int
	// Cancelled marks the event STATUS:CANCELLED (feeds; CANCEL implies it).
	Cancelled bool
	// Invitation only.
	OrganizerName  string
	OrganizerEmail string
	AttendeeName   string
	AttendeeEmail  string
	Now            time.Time
	// Stamp overrides DTSTAMP (feeds use the booking's last change so the
	// file is identical until something changes). Zero = Now.
	Stamp time.Time
}

// text escapes a TEXT value: control characters are dropped, then RFC 5545
// escaping (\\ \; \, \n).
func text(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return meetings.EscapeText(strings.TrimSpace(s))
}

// oneLine collapses whitespace (SUMMARY, LOCATION and names are one line).
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// cnParam renders ;CN="…" (quotes and line breaks cannot be in a parameter).
func cnParam(name string) string {
	name = oneLine(strings.Map(func(r rune) rune {
		if r == '"' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, name))
	if name == "" {
		return ""
	}
	return `;CN="` + name + `"`
}

// calAddress cleans an email address for a mailto: value.
func calAddress(e string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r < 0x21 || r == 0x7f || r == ',' || r == ';' {
			return -1
		}
		return r
	}, e)
}

func uriValue(u string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r < 0x21 || r == 0x7f {
			return -1
		}
		return r
	}, u)
}

func (in ICSInput) linkLabel() string {
	if l := oneLine(in.LinkLabel); l != "" {
		return l
	}
	return "Open booking"
}

// details is the description with the link on its last line.
func (in ICSInput) details() string {
	d := strings.TrimSpace(in.Description)
	if u := uriValue(in.URL); u != "" {
		if d != "" {
			d += "\n"
		}
		d += in.linkLabel() + ": " + u
	}
	return d
}

const (
	kindInvite = iota
	kindAdd
	kindFeed
)

// vevent renders one VEVENT (unfolded lines).
func vevent(in ICSInput, kind int, cancelled bool) []string {
	b := in.Booking
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	stamp := in.Stamp
	if stamp.IsZero() {
		stamp = now
	}
	created := b.CreatedAt
	if created.IsZero() {
		created = stamp
	}
	modified := b.UpdatedAt
	if modified.IsZero() || modified.Before(created) {
		modified = created
	}
	summary := oneLine(in.Summary)
	if summary == "" {
		summary = "Booking"
	}
	lines := []string{"BEGIN:VEVENT", "UID:" + UID(b.ID), "DTSTAMP:" + stamp.UTC().Format(icsTime),
		"DTSTART:" + b.StartTime.UTC().Format(icsTime), "DTEND:" + b.EndTime.UTC().Format(icsTime),
		fmt.Sprintf("SEQUENCE:%d", in.Sequence), "SUMMARY:" + text(summary)}
	if d := in.details(); d != "" {
		lines = append(lines, "DESCRIPTION:"+text(d))
	}
	if l := oneLine(in.Location); l != "" {
		lines = append(lines, "LOCATION:"+text(l))
	}
	if u := uriValue(in.URL); u != "" {
		lines = append(lines, "URL:"+u)
	}
	if kind == kindInvite {
		if e := calAddress(in.OrganizerEmail); e != "" {
			lines = append(lines, "ORGANIZER"+cnParam(in.OrganizerName)+":mailto:"+e)
		}
		if e := calAddress(in.AttendeeEmail); e != "" {
			att := "ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT"
			if !cancelled {
				// See the note at the top of this file.
				att += ";PARTSTAT=NEEDS-ACTION;RSVP=FALSE"
			}
			lines = append(lines, att+cnParam(in.AttendeeName)+":mailto:"+e)
		}
	}
	if cancelled {
		lines = append(lines, "STATUS:CANCELLED", "TRANSP:TRANSPARENT")
	} else {
		lines = append(lines, "STATUS:CONFIRMED", "TRANSP:OPAQUE")
	}
	cat := oneLine(in.Category)
	if cat == "" {
		cat = "Booking"
	}
	lines = append(lines, "CLASS:PUBLIC", "CATEGORIES:"+text(cat),
		"CREATED:"+created.UTC().Format(icsTime), "LAST-MODIFIED:"+modified.UTC().Format(icsTime))
	if cancelled {
		lines = append(lines, "X-MICROSOFT-CDO-BUSYSTATUS:FREE")
	} else {
		lines = append(lines, "X-MICROSOFT-CDO-BUSYSTATUS:BUSY")
		if kind == kindInvite {
			lines = append(lines, "X-MICROSOFT-DISALLOW-COUNTER:TRUE")
		}
		if in.Reminder > 0 {
			lines = append(lines, "BEGIN:VALARM", "ACTION:DISPLAY", "DESCRIPTION:"+text(summary), fmt.Sprintf("TRIGGER:-PT%dM", in.Reminder), "END:VALARM")
		}
	}
	return append(lines, "END:VEVENT")
}

// calendar wraps events in a VCALENDAR: folded at 75 octets, CRLF line ends.
func calendar(method string, head []string, events ...[]string) string {
	lines := []string{"BEGIN:VCALENDAR", "PRODID:" + prodID, "VERSION:2.0", "CALSCALE:GREGORIAN", "METHOD:" + method}
	lines = append(lines, head...)
	for _, e := range events {
		lines = append(lines, e...)
	}
	lines = append(lines, "END:VCALENDAR")
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(meetings.FoldLine(l))
		sb.WriteString("\r\n")
	}
	return sb.String()
}

// BuildICS renders the invitation: the inline text/calendar part of an email
// (METHOD:REQUEST, or CANCEL), addressed from the organiser to the attendee.
func BuildICS(in ICSInput) string {
	method := MethodRequest
	if strings.EqualFold(in.Method, MethodCancel) || in.Cancelled {
		method = MethodCancel
	}
	return calendar(method, nil, vevent(in, kindInvite, method == MethodCancel))
}

// AddMethod is the METHOD of the add-to-calendar file for a plan / state.
func AddMethod(cancelled bool) string {
	if cancelled {
		return MethodCancel
	}
	return MethodPublish
}

// BuildAddICS renders the add-to-calendar file: METHOD:PUBLISH without an
// organiser or attendee (or METHOD:CANCEL + STATUS:CANCELLED for a booking
// that no longer happens — a complete event, so it also opens fine when the
// booking was never added).
func BuildAddICS(in ICSInput) string {
	cancelled := strings.EqualFold(in.Method, MethodCancel) || in.Cancelled
	return calendar(AddMethod(cancelled), nil, vevent(in, kindAdd, cancelled))
}

// FeedInput is a subscription feed.
type FeedInput struct {
	Name        string
	Description string
	Events      []ICSInput
	// Refresh is how often calendar apps are asked to re-fetch (default 30 min).
	Refresh time.Duration
}

// BuildFeed renders a subscription calendar (METHOD:PUBLISH, named, with a
// refresh hint). Events keep the UIDs of the emails, so apps that match on
// UID never show a booking twice.
func BuildFeed(f FeedInput) string {
	mins := int(f.Refresh / time.Minute)
	if mins <= 0 {
		mins = 30
	}
	dur := fmt.Sprintf("PT%dM", mins)
	name := oneLine(f.Name)
	if name == "" {
		name = "DASYIN bookings"
	}
	head := []string{"X-WR-CALNAME:" + text(name)}
	if d := oneLine(f.Description); d != "" {
		head = append(head, "X-WR-CALDESC:"+text(d))
	}
	head = append(head, "REFRESH-INTERVAL;VALUE=DURATION:"+dur, "X-PUBLISHED-TTL:"+dur)
	events := make([][]string, 0, len(f.Events))
	for _, e := range f.Events {
		events = append(events, vevent(e, kindFeed, e.Cancelled))
	}
	return calendar(MethodPublish, head, events...)
}

// ---------------------------------------------------------------- file name

var asciiFold = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'ā': "a", 'ă': "a", 'ą': "a", 'æ': "ae",
	'ç': "c", 'ć': "c", 'č': "c", 'ď': "d", 'đ': "d",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ē': "e", 'ė': "e", 'ę': "e", 'ě': "e",
	'ğ': "g", 'ì': "i", 'í': "i", 'î': "i", 'ï': "i", 'ī': "i", 'į': "i", 'ı': "i",
	'ł': "l", 'ľ': "l", 'ñ': "n", 'ń': "n", 'ň': "n",
	'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o", 'ō': "o", 'ő': "o", 'œ': "oe",
	'ř': "r", 'ś': "s", 'š': "s", 'ş': "s", 'ß': "ss", 'ť': "t", 'ţ': "t", 'þ': "th",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ū': "u", 'ů': "u", 'ű': "u", 'ų': "u",
	'ý': "y", 'ÿ': "y", 'ź': "z", 'ż': "z", 'ž': "z",
}

// slug turns s into lower-case ASCII words joined by "-" (max n bytes).
func slug(s string, n int) string {
	var b strings.Builder
	dash := true // no leading dash
	for _, r := range strings.ToLower(s) {
		rep := ""
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			rep = string(r)
		default:
			rep = asciiFold[r]
		}
		if rep == "" {
			if !dash {
				b.WriteByte('-')
				dash = true
			}
			continue
		}
		b.WriteString(rep)
		dash = false
	}
	out := b.String()
	if len(out) > n {
		out = out[:n]
	}
	return strings.Trim(out, "-")
}

// ICSFilename names a booking's calendar file with safe ASCII only, e.g.
// "booking-2026-10-03-0900-jane-wanjiru.ics" (start in loc).
func ICSFilename(b *models.Booking, loc *time.Location, cancelled bool) string {
	if loc == nil {
		loc = time.UTC
	}
	name := "booking-" + b.StartTime.In(loc).Format("2006-01-02-1504")
	if s := slug(strings.TrimSpace(b.Customer.FirstName+" "+b.Customer.LastName), 32); s != "" {
		name += "-" + s
	}
	if cancelled {
		name += "-cancelled"
	}
	return name + ".ics"
}

// ---------------------------------------------------------------- signed links

// LinkToken signs the public calendar link of a booking: HMAC-SHA256 over the
// purpose, the organisation and the booking id with the server secret. The
// link carries nothing but the booking id and this token.
func LinkToken(secret []byte, org, bookingID string) string {
	m := hmac.New(sha256.New, secret)
	// Length-prefixed so no two (organisation, booking) pairs sign the same bytes.
	fmt.Fprintf(m, "dasyin/booking-calendar-link/v1|%d:%s|%d:%s", len(org), org, len(bookingID), bookingID)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// VerifyLinkToken checks token in constant time (false without a secret).
func VerifyLinkToken(secret []byte, org, bookingID, token string) bool {
	if len(secret) == 0 || token == "" || org == "" || bookingID == "" {
		return false
	}
	want := LinkToken(secret, org, bookingID)
	return subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

// PublicICSPath is the API path (under the server origin) of a booking's
// public calendar file.
func PublicICSPath(secret []byte, org, bookingID string) string {
	return "/api/v1/public/bookings/" + url.PathEscape(bookingID) + "/calendar.ics?t=" + LinkToken(secret, org, bookingID)
}

// ---------------------------------------------------------------- web calendar links

// Compose pages of the web calendars.
const (
	GoogleTemplateURL = "https://calendar.google.com/calendar/render"
	OutlookWorkURL    = "https://outlook.office.com/calendar/0/deeplink/compose"
	OutlookLiveURL    = "https://outlook.live.com/calendar/0/deeplink/compose"
	// maxLinkLen keeps add-to-calendar links well inside what mail clients
	// and browsers accept.
	maxLinkLen = 1800
)

// enc percent-encodes a query value. Spaces become %20, never "+": Outlook
// on the web shows a literal plus sign.
func enc(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return ""
	}
	return strings.TrimSpace(string(r[:max-1])) + "…"
}

// linkDetails is the description for a web link, capped at max runes; the
// link to the booking always survives the cut.
func (in ICSInput) linkDetails(max int) string {
	d := clipRunes(strings.TrimSpace(strings.ReplaceAll(in.Description, "\r\n", "\n")), max)
	if u := uriValue(in.URL); u != "" {
		if d != "" {
			d += "\n\n"
		}
		d += in.linkLabel() + ": " + u
	}
	return d
}

var detailCaps = []int{900, 600, 400, 250, 120, 0}

// GoogleCalendarURL opens Google Calendar's "new event" page with the
// booking filled in (UTC times).
func GoogleCalendarURL(in ICSInput) string {
	b := in.Booking
	out := ""
	for _, c := range detailCaps {
		out = GoogleTemplateURL + "?action=TEMPLATE&text=" + enc(clipRunes(oneLine(in.Summary), 150)) +
			"&dates=" + b.StartTime.UTC().Format(icsTime) + "/" + b.EndTime.UTC().Format(icsTime)
		if d := in.linkDetails(c); d != "" {
			out += "&details=" + enc(d)
		}
		if l := clipRunes(oneLine(in.Location), 150); l != "" {
			out += "&location=" + enc(l)
		}
		if len(out) <= maxLinkLen {
			break
		}
	}
	return out
}

// OutlookCalendarURL opens Outlook on the web's "new event" page (base is
// OutlookWorkURL for Microsoft 365 or OutlookLiveURL for outlook.com). The
// body is HTML; times are UTC.
func OutlookCalendarURL(base string, in ICSInput) string {
	b := in.Booking
	const iso = "2006-01-02T15:04:05Z"
	out := ""
	for _, c := range detailCaps {
		out = base + "?path=/calendar/action/compose&rru=addevent&subject=" + enc(clipRunes(oneLine(in.Summary), 150)) +
			"&startdt=" + enc(b.StartTime.UTC().Format(iso)) + "&enddt=" + enc(b.EndTime.UTC().Format(iso))
		if d := in.linkDetails(c); d != "" {
			out += "&body=" + enc(strings.ReplaceAll(html.EscapeString(d), "\n", "<br>"))
		}
		if l := clipRunes(oneLine(in.Location), 150); l != "" {
			out += "&location=" + enc(l)
		}
		if len(out) <= maxLinkLen {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------- subscription links

// FeedLinks are the ways to subscribe to a feed address.
type FeedLinks struct {
	URL         string `json:"url"`              // https://…/calendar/<token>.ics
	Webcal      string `json:"webcal_url"`       // webcal://… (Apple Calendar, desktop apps)
	Google      string `json:"google_url"`       // Google Calendar "add by URL"
	Outlook     string `json:"outlook_url"`      // Microsoft 365
	OutlookLive string `json:"outlook_live_url"` // outlook.com
}

// SubscribeLinks builds the subscribe links for a feed address.
func SubscribeLinks(feedURL, name string) FeedLinks {
	webcal := feedURL
	if i := strings.Index(feedURL, "://"); i > 0 {
		webcal = "webcal" + feedURL[i:]
	}
	q := "?url=" + enc(feedURL) + "&name=" + enc(clipRunes(oneLine(name), 80))
	return FeedLinks{URL: feedURL, Webcal: webcal,
		Google:      "https://calendar.google.com/calendar/r?cid=" + enc(webcal),
		Outlook:     "https://outlook.office.com/calendar/0/addfromweb" + q,
		OutlookLive: "https://outlook.live.com/calendar/0/addfromweb" + q}
}
