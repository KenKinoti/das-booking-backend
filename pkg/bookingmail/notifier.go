package bookingmail

import (
	"context"
	"fmt"
	"log"
	netmail "net/mail"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Notifier sends staff booking emails.
type Notifier struct {
	DB      *gorm.DB
	Mail    *mail.Service
	AppURL  string
	LogoURL func(org string) string
	Now     func() time.Time
	// APIURL is the public origin of this API (PUBLIC_API_URL, no trailing
	// slash, no /api/v1): the base of the public calendar links in emails.
	APIURL string
	// Secret signs the public calendar links (the server secret). Without it
	// there are no public links.
	Secret []byte
	// Terms returns the organisation's business-type words ("booking" →
	// "Job card"); nil = the default words.
	Terms func(org string) map[string]string
	// Async sends in the background (the API never waits for SMTP).
	Async bool
	// Retries are the waits before attempts 2, 3 … (default 5s, 30s).
	Retries []time.Duration
	wg      sync.WaitGroup
}

// Default is the process-wide notifier (set by cmd/production); nil = off.
var Default *Notifier

// New builds an asynchronous notifier.
func New(db *gorm.DB) *Notifier {
	return &Notifier{DB: db, Mail: mail.Shared(db), Now: time.Now, Async: true, Retries: []time.Duration{5 * time.Second, 30 * time.Second}}
}

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// Wait blocks until background sends finish (tests).
func (n *Notifier) Wait() { n.wg.Wait() }

// Created is called after a booking was created (b fully loaded).
func Created(b *models.Booking, actorID string) {
	if Default != nil && b != nil {
		Default.Handle(nil, b, actorID)
	}
}

// Updated is called after a booking changed (before = the row before the
// change, after = fully loaded fresh row).
func Updated(before, after *models.Booking, actorID string) {
	if Default != nil && before != nil && after != nil {
		Default.Handle(before, after, actorID)
	}
}

// Deleted is called after a booking was deleted (b loaded before deletion).
func Deleted(b *models.Booking, actorID string) {
	if Default != nil && b != nil {
		Default.Handle(b, nil, actorID)
	}
}

// SettingsFor returns the organisation's settings (defaults when none saved).
func (n *Notifier) SettingsFor(org string) Settings {
	var s Settings
	if n.DB.Where("organization_id = ?", org).First(&s).Error != nil {
		return DefaultSettings(org)
	}
	return s
}

// WantsEmails reports a user's own "Email me about my bookings" choice
// (kept in pkg/emailprefs since 2.12; the old UserPref rows were moved there).
func (n *Notifier) WantsEmails(org, userID string) bool {
	return emailprefs.SelfOn(n.DB, org, userID, emailprefs.Bookings)
}

// Handle plans and queues the emails for one change.
func (n *Notifier) Handle(before, after *models.Booking, actorID string) []Plan {
	plans := PlanEmails(before, after)
	b := after
	if b == nil {
		b = before
	}
	if b == nil {
		return plans
	}
	org := b.OrganizationID
	// One SEQUENCE bump per change — shared by everyone emailed about it and
	// by the calendar file / feed, also when nobody is emailed, so adding the
	// file again always updates the event in a calendar app.
	seq := n.nextSequence(org, b.ID, before != nil)
	if len(plans) == 0 {
		return plans
	}
	if !n.SettingsFor(org).StaffEmails {
		return nil
	}
	snap := *b
	if after == nil {
		// Deleted: the calendar entry keeps the last known times.
		snap.Status = "cancelled"
	}
	for _, p := range plans {
		p := p
		job := func() { n.deliver(&snap, before != nil, after == nil, p, seq, actorID) }
		if n.Async {
			n.wg.Add(1)
			go func() {
				defer n.wg.Done()
				defer func() {
					if r := recover(); r != nil {
						log.Printf("bookingmail: panic: %v", r)
					}
				}()
				job()
			}()
		} else {
			job()
		}
	}
	return plans
}

// nextSequence returns the SEQUENCE for a change: 0 for a new booking, the
// previous one + 1 afterwards (1 for the first change of a booking that
// predates this table).
func (n *Notifier) nextSequence(org, bookingID string, changed bool) int {
	now := n.now()
	first := 0
	if changed {
		first = 1
	}
	n.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "booking_id"}},
		DoUpdates: clause.Assignments(map[string]any{"sequence": gorm.Expr("booking_calendar_state.sequence + 1"), "updated_at": now}),
	}).Create(&CalState{BookingID: bookingID, OrganizationID: org, Sequence: first, UpdatedAt: now})
	var st CalState
	n.DB.Where("booking_id = ?", bookingID).First(&st)
	return st.Sequence
}

// Sequence is the current calendar SEQUENCE of a booking (0 when it never changed).
func (n *Notifier) Sequence(bookingID string) int {
	var st CalState
	if n.DB.Where("booking_id = ?", bookingID).First(&st).Error != nil {
		return 0
	}
	return st.Sequence
}

func (n *Notifier) sequences(ids []string) map[string]int {
	out := map[string]int{}
	for i := 0; i < len(ids); i += 500 {
		j := i + 500
		if j > len(ids) {
			j = len(ids)
		}
		var rows []CalState
		n.DB.Where("booking_id IN ?", ids[i:j]).Find(&rows)
		for _, r := range rows {
			out[r.BookingID] = r.Sequence
		}
	}
	return out
}

type staffUser struct {
	ID, Email, FirstName, LastName, OrganizationID string
	IsActive                                       *bool
}

func (n *Notifier) user(id string) *staffUser {
	var u staffUser
	if n.DB.Session(&gorm.Session{NewDB: true}).Table("users").Select("id, email, first_name, last_name, organization_id, is_active").
		Where("id = ? AND deleted_at IS NULL", id).Take(&u).Error != nil {
		return nil
	}
	return &u
}

func (n *Notifier) logRow(l Log) *Log {
	now := n.now()
	l.ID, l.CreatedAt, l.UpdatedAt = uuid.NewString(), now, now
	if l.Key == "" {
		l.Key = l.ID
	}
	n.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&l)
	return &l
}

func (n *Notifier) deliver(b *models.Booking, changed, deleted bool, p Plan, seq int, actorID string) {
	org := b.OrganizationID
	key := fmt.Sprintf("%s|%s|%s|%d", b.ID, p.UserID, p.Event, seq)
	u := n.user(p.UserID)
	row := Log{OrganizationID: org, Key: clip(key, 250), BookingID: b.ID, UserID: p.UserID, Event: p.Event, Method: p.Method, Sequence: seq, Status: "queued"}
	if u == nil || u.OrganizationID != org {
		row.Status, row.Error = "skipped", "staff member not found"
		n.logRow(row)
		return
	}
	row.Email = strings.ToLower(strings.TrimSpace(u.Email))
	// In-app notification for the changes the booking API doesn't announce
	// itself (new bookings and status changes already notify the staff).
	if changed && p.UserID != actorID && (p.Event == EvAssigned || p.Event == EvUnassigned || p.Event == EvRescheduled || (p.Event == EvCancelled && deleted)) {
		n.inApp(b, p, actorID)
	}
	// Who gets emails (pkg/emailprefs): the organisation's mode, an admin's
	// switches and the staff member's own choice.
	if okk, why := emailprefs.Check(n.DB, org, p.UserID, emailprefs.Bookings); !okk {
		row.Status, row.Error = "skipped", why
		n.logRow(row)
		return
	}
	if !mail.ValidAddress(row.Email) {
		row.Status, row.Error = "skipped", "staff member has no valid email address"
		log.Printf("bookingmail: booking %s: %s %s has no valid email — skipped", b.ID, u.FirstName, u.LastName)
		n.logRow(row)
		return
	}
	msg := n.Message(b, p, seq, u)
	row.Subject = clip(msg.Subject, 480)
	lr := n.logRow(row)
	attempts := 1 + len(n.Retries)
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(n.Retries[i-1])
		}
		_, err = n.Mail.SendAs(context.Background(), org, msg)
		code := mail.Code(err)
		n.DB.Model(&Log{}).Where("id = ?", lr.ID).Updates(map[string]any{"attempts": i + 1, "updated_at": n.now()})
		if err == nil || code == "NOT_CONFIGURED" || code == "INVALID_RECIPIENT" || code == "NO_RECIPIENT" {
			break
		}
	}
	upd := map[string]any{"status": "sent", "error": "", "updated_at": n.now()}
	if err != nil {
		upd["status"], upd["error"] = "failed", clip(err.Error(), 1000)
		log.Printf("bookingmail: booking %s → %s: %v", b.ID, row.Email, err)
	}
	n.DB.Model(&Log{}).Where("id = ?", lr.ID).Updates(upd)
}

func (n *Notifier) inApp(b *models.Booking, p Plan, actorID string) {
	when := n.when(b)
	title := map[string]string{EvAssigned: "Booking assigned to you", EvUnassigned: "Booking moved to someone else", EvRescheduled: "Your booking was rescheduled",
		EvCancelled: "Your booking was cancelled"}[p.Event]
	sev := notify.Info
	if p.Event == EvCancelled || p.Event == EvUnassigned {
		sev = notify.Warning
	}
	notify.Send(n.DB, b.OrganizationID, notify.Users(p.UserID), notify.Message{Type: "booking.staff_" + p.Event, Severity: sev, Title: title,
		Body: CustomerName(b) + " · " + when, Link: "/bookings?booking=" + b.ID, EntityType: "booking", EntityID: b.ID, ActorID: actorID, ActorName: notify.UserName(n.DB, actorID)})
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// Loc is the organisation's time zone.
func (n *Notifier) Loc(org string) *time.Location {
	if tz := orgprefs.Get(n.DB, org).Timezone; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.UTC
}

func (n *Notifier) when(b *models.Booking) string {
	loc := n.Loc(b.OrganizationID)
	s, e := b.StartTime.In(loc), b.EndTime.In(loc)
	return s.Format("Mon 2 Jan 2006, 15:04") + " – " + e.Format("15:04")
}

type orgRow struct {
	Name, Email, Phone, AddressStreet, AddressSuburb, AddressState, AddressPostcode string
}

func (n *Notifier) org(id string) (orgRow, string) {
	var o orgRow
	n.DB.Raw(`SELECT name, COALESCE(email,'') AS email, COALESCE(phone,'') AS phone, COALESCE(address_street,'') AS address_street, COALESCE(address_suburb,'') AS address_suburb,
		COALESCE(address_state,'') AS address_state, COALESCE(address_postcode,'') AS address_postcode FROM organizations WHERE id = ?`, id).Scan(&o)
	parts := []string{}
	for _, p := range []string{o.AddressStreet, strings.TrimSpace(o.AddressSuburb + " " + o.AddressState + " " + o.AddressPostcode)} {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, strings.TrimSpace(p))
		}
	}
	return o, strings.Join(parts, ", ")
}

func (n *Notifier) link(path string) string {
	base := strings.TrimRight(strings.TrimSpace(n.AppURL), "/")
	if base == "" {
		return ""
	}
	return base + path
}

// term is the organisation's word for key ("booking" → "Job card").
func (n *Notifier) term(org, key, fallback string) string {
	if n.Terms != nil {
		if v := strings.TrimSpace(n.Terms(org)[key]); v != "" {
			return v
		}
	}
	return fallback
}

// orgCtx is what every calendar entry of one organisation shares.
type orgCtx struct {
	ID       string
	Org      orgRow
	Addr     string
	Loc      *time.Location
	Reminder int
	Category string // the organisation's word for a booking
}

func (n *Notifier) orgCtx(org string) orgCtx {
	o, addr := n.org(org)
	return orgCtx{ID: org, Org: o, Addr: addr, Loc: n.Loc(org), Reminder: n.SettingsFor(org).Reminder(), Category: n.term(org, "booking", "Booking")}
}

// servicesText is the comma-separated service list ("Booking" when there is none).
func servicesText(b *models.Booking) string {
	if s := strings.Join(ServiceNames(b), ", "); s != "" {
		return s
	}
	return "Booking"
}

// Event describes a booking's calendar entry (shared by the invitation, the
// add-to-calendar file, the web calendar links and the feed).
func (n *Notifier) Event(b *models.Booking, seq int, oc orgCtx) ICSInput {
	cust := CustomerName(b)
	desc := []string{"Customer: " + cust}
	if ph := strings.TrimSpace(b.Customer.Phone); ph != "" {
		desc = append(desc, "Phone: "+ph)
	}
	if svcs := ServiceNames(b); len(svcs) > 0 {
		desc = append(desc, "Services: "+strings.Join(svcs, ", "))
	}
	if v := VehicleText(b); v != "" {
		desc = append(desc, "Vehicle: "+v)
	}
	if nt := strings.TrimSpace(b.Notes); nt != "" {
		desc = append(desc, "Notes: "+nt)
	}
	// Without services the entry is named after what the organisation calls a booking.
	what := strings.Join(ServiceNames(b), ", ")
	if what == "" {
		what = oc.Category
	}
	return ICSInput{Booking: b, Sequence: seq, Summary: what + " — " + cust, Description: strings.Join(desc, "\n"), Location: oc.Addr,
		URL: n.link("/bookings?booking=" + b.ID), LinkLabel: "Open " + strings.ToLower(oc.Category), Category: oc.Category, Reminder: oc.Reminder, Now: n.now()}
}

// gone: the booking no longer happens (cancelled or deleted).
func gone(b *models.Booking) bool { return b.Status == "cancelled" || b.DeletedAt.Valid }

// BookingICS is a booking's add-to-calendar file and its file name: the
// PUBLISH file, or the CANCEL file once the booking is cancelled or deleted.
func (n *Notifier) BookingICS(b *models.Booking) (ics, filename string, cancelled bool) {
	oc := n.orgCtx(b.OrganizationID)
	ev := n.Event(b, n.Sequence(b.ID), oc)
	cancelled = gone(b)
	ev.Method = AddMethod(cancelled)
	return BuildAddICS(ev), ICSFilename(b, oc.Loc, cancelled), cancelled
}

// PublicICSURL is the signed, login-free address of a booking's calendar file
// ("" when the API origin or the secret is unknown).
func (n *Notifier) PublicICSURL(apiBase string, b *models.Booking) string {
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if apiBase == "" || len(n.Secret) == 0 {
		return ""
	}
	return apiBase + PublicICSPath(n.Secret, b.OrganizationID, b.ID)
}

// CalendarLinks are the ways to add one booking to a calendar.
type CalendarLinks struct {
	Filename    string `json:"filename"`
	Cancelled   bool   `json:"cancelled"`
	ICS         string `json:"ics_url"`          // public signed .ics (Apple Calendar and other apps)
	Google      string `json:"google_url"`       // "" for a cancelled booking
	Outlook     string `json:"outlook_url"`      // Microsoft 365
	OutlookLive string `json:"outlook_live_url"` // outlook.com
}

// LinksFor builds the add-to-calendar links of a booking.
func (n *Notifier) LinksFor(apiBase string, b *models.Booking, ev ICSInput, loc *time.Location) CalendarLinks {
	l := CalendarLinks{Cancelled: gone(b), ICS: n.PublicICSURL(apiBase, b)}
	l.Filename = ICSFilename(b, loc, l.Cancelled)
	if !l.Cancelled {
		l.Google, l.Outlook, l.OutlookLive = GoogleCalendarURL(ev), OutlookCalendarURL(OutlookWorkURL, ev), OutlookCalendarURL(OutlookLiveURL, ev)
	}
	return l
}

// Message renders one staff email. When the plan has a method it carries the
// calendar data twice: the inline invitation (REQUEST / CANCEL) for the mail
// client's event card and the add-to-calendar file (PUBLISH / CANCEL) as the
// attachment, plus "Add to calendar" buttons for a booking that happens.
func (n *Notifier) Message(b *models.Booking, p Plan, seq int, u *staffUser) mail.Message {
	org := b.OrganizationID
	oc := n.orgCtx(org)
	loc, o, addr := oc.Loc, oc.Org, oc.Addr
	start, end := b.StartTime.In(loc), b.EndTime.In(loc)
	cust := CustomerName(b)
	svcs := ServiceNames(b)
	svcText := servicesText(b)
	first := strings.TrimSpace(u.FirstName)
	if first == "" {
		first = "there"
	}
	day := start.Format("Mon 2 Jan")
	when := start.Format("Monday 2 January 2006") + " at " + start.Format("15:04")
	tzName := loc.String()
	cancel := p.Method == MethodCancel
	fileName := ICSFilename(b, loc, cancel)
	var subject, msg, label, status, tone string
	switch p.Event {
	case EvAssigned:
		subject = fmt.Sprintf("New booking: %s — %s, %s %s", cust, svcText, day, start.Format("15:04"))
		msg = fmt.Sprintf("Hi %s,\n\nYou have a booking with %s on %s (%s).", first, cust, when, tzName)
		label, status, tone = "New booking", "Assigned to you", "ok"
	case EvRescheduled:
		subject = fmt.Sprintf("Booking rescheduled: %s — now %s %s", cust, day, start.Format("15:04"))
		msg = fmt.Sprintf("Hi %s,\n\nYour booking with %s has moved to %s (%s). Open the attached calendar file to update the entry in your calendar.", first, cust, when, tzName)
		label, status, tone = "Booking rescheduled", "New time", "warn"
	case EvUnassigned:
		subject = fmt.Sprintf("Booking reassigned: %s on %s %s is no longer yours", cust, day, start.Format("15:04"))
		msg = fmt.Sprintf("Hi %s,\n\nThe booking with %s on %s has been given to someone else. You don't need to attend — the attached update removes it from your calendar.", first, cust, when)
		label, status, tone = "Booking reassigned", "No longer assigned to you", "info"
	case EvCancelled:
		subject = fmt.Sprintf("Booking cancelled: %s — %s %s", cust, day, start.Format("15:04"))
		msg = fmt.Sprintf("Hi %s,\n\nThe booking with %s on %s has been cancelled. The attached update removes it from your calendar.", first, cust, when)
		label, status, tone = "Booking cancelled", "Cancelled", "danger"
	case EvNoShow:
		subject = fmt.Sprintf("No-show: %s — %s %s", cust, day, start.Format("15:04"))
		msg = fmt.Sprintf("Hi %s,\n\n%s did not show up for the booking on %s. It's been marked as a no-show.", first, cust, when)
		label, status, tone = "Marked as no-show", "No-show", "warn"
	}
	facts := []invoicing.EmailRow{{Label: "Customer", Value: cust}}
	if ph := strings.TrimSpace(b.Customer.Phone); ph != "" {
		facts = append(facts, invoicing.EmailRow{Label: "Phone", Value: ph})
	}
	if len(svcs) > 0 {
		lbl := "Service"
		if len(svcs) > 1 {
			lbl = "Services"
		}
		facts = append(facts, invoicing.EmailRow{Label: lbl, Value: svcText})
	}
	facts = append(facts, invoicing.EmailRow{Label: "Date", Value: start.Format("Mon 2 Jan 2006")},
		invoicing.EmailRow{Label: "Time", Value: start.Format("15:04") + " – " + end.Format("15:04") + " (" + tzName + ")"})
	if d := Duration(b); d != "" {
		facts = append(facts, invoicing.EmailRow{Label: "Duration", Value: d})
	}
	if v := VehicleText(b); v != "" {
		facts = append(facts, invoicing.EmailRow{Label: "Vehicle", Value: v})
	}
	if addr != "" {
		facts = append(facts, invoicing.EmailRow{Label: "Location", Value: addr})
	}
	if nt := strings.TrimSpace(b.Notes); nt != "" {
		msg += "\n\nNotes: " + nt
	}
	open := n.link("/bookings?booking=" + b.ID)
	actions := []invoicing.EmailAction{}
	if open != "" {
		actions = append(actions, invoicing.EmailAction{Label: "Open booking", URL: open, Primary: true})
	}
	ev := n.Event(b, seq, oc)
	var groups []invoicing.EmailActionGroup
	note := "Turn these emails off in My profile → Email notifications."
	switch {
	case p.Method == "":
	case cancel:
		note = "Opening the attached calendar file (.ics) removes the booking from your calendar. " + note
	default:
		// One tap per calendar: the web calendars open their "new event" page
		// filled in, everything else opens the .ics in the calendar app.
		l := n.LinksFor(n.APIURL, b, ev, loc)
		g := invoicing.EmailActionGroup{Label: "Add to your calendar", Actions: []invoicing.EmailAction{{Label: "Google Calendar", URL: l.Google}, {Label: "Outlook", URL: l.Outlook}},
			Links: []invoicing.EmailAction{{Label: "Outlook.com (personal account)", URL: l.OutlookLive}}}
		if l.ICS != "" {
			g.Actions = append(g.Actions, invoicing.EmailAction{Label: "Apple / other", URL: l.ICS})
		}
		groups = append(groups, g)
		note = "On an iPhone, iPad or Mac, or in another calendar app: open the attached calendar file (.ics) and choose Add. Later changes update the same entry. " + note
	}
	logo := ""
	if n.LogoURL != nil {
		logo = n.LogoURL(org)
	}
	var accent []string
	n.DB.Raw(`SELECT accent_color FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&accent)
	v := invoicing.EmailView{Kind: "booking", Business: invoicing.EmailBusiness{Name: nzs(o.Name, "DASYIN ERP"), Address: addr, Email: o.Email, Phone: o.Phone},
		LogoURL: logo, Preheader: subject, DocLabel: label, Number: day, Message: msg, AmountLabel: "When",
		Amount: start.Format("Mon 2 Jan, 15:04") + "–" + end.Format("15:04"), StatusLine: status, StatusTone: tone, Facts: facts, Actions: actions, Groups: groups, Note: note}
	if len(accent) > 0 {
		v.Accent = accent[0]
	}
	htmlOut, text := invoicing.RenderEmail(v)
	to := u.Email
	if nm := strings.TrimSpace(u.FirstName + " " + u.LastName); nm != "" {
		to = (&netmail.Address{Name: nm, Address: strings.TrimSpace(u.Email)}).String()
	}
	m := mail.Message{To: []string{to}, Subject: subject, HTML: htmlOut, Text: text}
	if p.Method != "" {
		cfg, _ := n.Mail.Resolve(org)
		// Inline invitation: what the mail client turns into its event card.
		inv := ev
		inv.Method, inv.OrganizerName, inv.OrganizerEmail = p.Method, nzs(o.Name, cfg.FromName), cfg.FromEmail
		inv.AttendeeName, inv.AttendeeEmail = strings.TrimSpace(u.FirstName+" "+u.LastName), strings.ToLower(u.Email)
		m.Calendar, m.CalendarMethod = BuildICS(inv), p.Method
		// Attachment: the add-to-calendar file a calendar app simply adds.
		add := ev
		add.Method = AddMethod(cancel)
		m.Attachments = []mail.Attachment{{Filename: fileName, ContentType: "text/calendar; charset=utf-8; method=" + add.Method, Data: []byte(BuildAddICS(add))}}
	}
	return m
}

func nzs(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
