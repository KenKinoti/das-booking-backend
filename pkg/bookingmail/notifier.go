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

// WantsEmails reports a user's "Email me about my bookings" choice.
func (n *Notifier) WantsEmails(userID string) bool {
	var p UserPref
	if n.DB.Where("user_id = ?", userID).First(&p).Error != nil {
		return true
	}
	return p.BookingEmails
}

// Handle plans and queues the emails for one change.
func (n *Notifier) Handle(before, after *models.Booking, actorID string) []Plan {
	plans := PlanEmails(before, after)
	if len(plans) == 0 {
		return plans
	}
	b := after
	if b == nil {
		b = before
	}
	org := b.OrganizationID
	if !n.SettingsFor(org).StaffEmails {
		return nil
	}
	// One SEQUENCE bump per change (shared by everyone emailed about it).
	seq := n.nextSequence(org, b.ID)
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

func (n *Notifier) nextSequence(org, bookingID string) int {
	now := n.now()
	n.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "booking_id"}},
		DoUpdates: clause.Assignments(map[string]any{"sequence": gorm.Expr("booking_calendar_state.sequence + 1"), "updated_at": now}),
	}).Create(&CalState{BookingID: bookingID, OrganizationID: org, Sequence: 0, UpdatedAt: now})
	var st CalState
	n.DB.Where("booking_id = ?", bookingID).First(&st)
	return st.Sequence
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
	if !n.WantsEmails(p.UserID) {
		row.Status, row.Error = "skipped", "turned off in the staff member's profile"
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

// Message renders one staff email (with the .ics when the plan has a method).
func (n *Notifier) Message(b *models.Booking, p Plan, seq int, u *staffUser) mail.Message {
	org := b.OrganizationID
	loc := n.Loc(org)
	o, addr := n.org(org)
	start, end := b.StartTime.In(loc), b.EndTime.In(loc)
	cust := CustomerName(b)
	svcs := ServiceNames(b)
	svcText := strings.Join(svcs, ", ")
	if svcText == "" {
		svcText = "Booking"
	}
	first := strings.TrimSpace(u.FirstName)
	if first == "" {
		first = "there"
	}
	day := start.Format("Mon 2 Jan")
	when := start.Format("Monday 2 January 2006") + " at " + start.Format("15:04")
	tzName := loc.String()
	var subject, msg, label, status, tone string
	switch p.Event {
	case EvAssigned:
		subject = fmt.Sprintf("New booking: %s — %s, %s %s", cust, svcText, day, start.Format("15:04"))
		msg = fmt.Sprintf("Hi %s,\n\nYou have a booking with %s on %s (%s).", first, cust, when, tzName)
		label, status, tone = "New booking", "Assigned to you", "ok"
	case EvRescheduled:
		subject = fmt.Sprintf("Booking rescheduled: %s — now %s %s", cust, day, start.Format("15:04"))
		msg = fmt.Sprintf("Hi %s,\n\nYour booking with %s has moved to %s (%s). Your calendar entry is updated by the attached invitation.", first, cust, when, tzName)
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
	note := "Turn these emails off in My profile → Booking emails."
	if p.Method != "" {
		if p.Method == "CANCEL" {
			note = "The attached booking.ics removes the event from your calendar. " + note
		} else {
			note = "Add it to your calendar with the attached booking.ics — later changes update the same event. " + note
		}
	}
	logo := ""
	if n.LogoURL != nil {
		logo = n.LogoURL(org)
	}
	var accent []string
	n.DB.Raw(`SELECT accent_color FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&accent)
	v := invoicing.EmailView{Kind: "booking", Business: invoicing.EmailBusiness{Name: nzs(o.Name, "DASYIN ERP"), Address: addr, Email: o.Email, Phone: o.Phone},
		LogoURL: logo, Preheader: subject, DocLabel: label, Number: day, Message: msg, AmountLabel: "When",
		Amount: start.Format("Mon 2 Jan, 15:04") + "–" + end.Format("15:04"), StatusLine: status, StatusTone: tone, Facts: facts, Actions: actions, Note: note}
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
		desc := []string{"Customer: " + cust}
		if ph := strings.TrimSpace(b.Customer.Phone); ph != "" {
			desc = append(desc, "Phone: "+ph)
		}
		if len(svcs) > 0 {
			desc = append(desc, "Services: "+svcText)
		}
		if v := VehicleText(b); v != "" {
			desc = append(desc, "Vehicle: "+v)
		}
		if nt := strings.TrimSpace(b.Notes); nt != "" {
			desc = append(desc, "Notes: "+nt)
		}
		if open != "" {
			desc = append(desc, "Open booking: "+open)
		}
		ics := BuildICS(ICSInput{Booking: b, Method: p.Method, Sequence: seq, Summary: svcText + " — " + cust, Description: strings.Join(desc, "\n"),
			Location: addr, URL: open, OrganizerName: nzs(o.Name, cfg.FromName), OrganizerEmail: cfg.FromEmail, AttendeeName: strings.TrimSpace(u.FirstName + " " + u.LastName),
			AttendeeEmail: strings.ToLower(u.Email), Now: n.now()})
		m.Calendar, m.CalendarMethod = ics, p.Method
		m.Attachments = []mail.Attachment{{Filename: "booking.ics", ContentType: "text/calendar; method=" + p.Method, Data: []byte(ics)}}
	}
	return m
}

func nzs(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
