// Package bookingmail emails staff about their bookings: assigned (new or
// re-assigned), rescheduled, unassigned, cancelled / deleted and no-show,
// each with calendar data in two flavours (see calendar.go): an inline iTIP
// invitation (METHOD:REQUEST or CANCEL) that mail clients show as an event
// card, and an attached "add to calendar" file (METHOD:PUBLISH, no organiser
// or attendee) that calendar apps simply add. Both share a stable UID per
// booking and an increasing SEQUENCE so calendar apps update or remove the
// event. Staff can also subscribe to a calendar feed (feed.go) and there is
// an optional daily agenda at 07:00 in the organisation's time zone.
//
// Sending never blocks the booking API: messages go out in the background
// with retries and every attempt is logged (booking_email_log). Staff can turn
// the emails off on their profile; admins can turn them off for the whole
// organisation in Settings → Notifications. Who receives them — the
// organisation's mode, an admin's per-person switches and the person's own
// choice — is decided by pkg/emailprefs.
//
// Customer emails: the codebase has no customer-facing booking email wired
// to the booking API (integrations.SendBookingConfirmation is unused), so
// customer contacts flagged cc_bookings are not emailed here.
package bookingmail

import "time"

// Settings are the organisation's booking-email switches.
type Settings struct {
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	StaffEmails    bool   `json:"staff_emails"`
	DailyAgenda    bool   `json:"daily_agenda"`
	AgendaHour     int    `json:"agenda_hour"`
	LastAgendaDate string `json:"last_agenda_date" gorm:"type:varchar(10)"`
	// ReminderMinutes is the calendar alarm lead time: nil = default (30),
	// 0 = no reminder, otherwise 15, 30 or 60. Read it with Reminder().
	ReminderMinutes *int      `json:"-"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// DefaultReminder is the calendar reminder lead time in minutes.
const DefaultReminder = 30

// ReminderChoices are the reminder lead times an organisation can pick
// (minutes before the start; 0 = no reminder).
var ReminderChoices = []int{0, 15, 30, 60}

// ValidReminder reports whether m is one of ReminderChoices.
func ValidReminder(m int) bool {
	for _, c := range ReminderChoices {
		if c == m {
			return true
		}
	}
	return false
}

// Reminder is the reminder lead time in minutes (0 = none).
func (s Settings) Reminder() int {
	if s.ReminderMinutes == nil || !ValidReminder(*s.ReminderMinutes) {
		return DefaultReminder
	}
	return *s.ReminderMinutes
}

// TableName is fixed.
func (Settings) TableName() string { return "booking_email_settings" }

// DefaultSettings: staff emails on, daily agenda off (07:00).
func DefaultSettings(org string) Settings {
	return Settings{OrganizationID: org, StaffEmails: true, DailyAgenda: false, AgendaHour: 7}
}

// UserPref is one user's "Email me about my bookings" choice (default on).
// Since 2.12 the choice lives in pkg/emailprefs (Who gets emails): the
// rows with booking_emails = false were moved there once and this table is
// no longer read or written.
type UserPref struct {
	UserID         string    `json:"user_id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);index"`
	BookingEmails  bool      `json:"booking_emails"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (UserPref) TableName() string { return "booking_email_user_prefs" }

// CalState keeps the iCalendar SEQUENCE of a booking.
type CalState struct {
	BookingID      string    `json:"booking_id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);index"`
	Sequence       int       `json:"sequence"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (CalState) TableName() string { return "booking_calendar_state" }

// Log is one staff email (sent, failed or skipped).
type Log struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);not null;index;uniqueIndex:idx_bmail_key,priority:1"`
	Key            string    `json:"key" gorm:"type:varchar(255);not null;uniqueIndex:idx_bmail_key,priority:2"`
	BookingID      string    `json:"booking_id" gorm:"type:varchar(255);index"`
	UserID         string    `json:"user_id" gorm:"type:varchar(255)"`
	Email          string    `json:"email" gorm:"type:varchar(255)"`
	Event          string    `json:"event" gorm:"type:varchar(32)"` // assigned | rescheduled | unassigned | cancelled | no_show | agenda
	Method         string    `json:"method" gorm:"type:varchar(16)"`
	Sequence       int       `json:"sequence"`
	Subject        string    `json:"subject" gorm:"type:varchar(500)"`
	Status         string    `json:"status" gorm:"type:varchar(16)"` // queued | sent | failed | skipped
	Error          string    `json:"error" gorm:"type:text"`
	Attempts       int       `json:"attempts"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Log) TableName() string { return "booking_email_log" }

// Feed scopes.
const (
	FeedUser = "user" // one staff member's assigned bookings
	FeedOrg  = "org"  // every booking of the organisation (admins / managers)
)

// FeedToken is the secret address of a calendar subscription feed. A feed is
// replaced by revoking its row and creating a new one, so old links stop
// working (410 Gone).
type FeedToken struct {
	ID             string `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);not null;index:idx_bcal_feed_owner,priority:1"`
	Scope          string `json:"scope" gorm:"type:varchar(8);not null;index:idx_bcal_feed_owner,priority:2"`
	// UserID owns a user feed; it is empty for the organisation feed.
	UserID string `json:"user_id" gorm:"type:varchar(255);index:idx_bcal_feed_owner,priority:3"`
	// Token is the secret in the feed address; it is looked up by its hash.
	Token      string     `json:"-" gorm:"type:varchar(128);not null"`
	TokenHash  string     `json:"-" gorm:"type:varchar(64);not null;uniqueIndex"`
	CreatedBy  string     `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	// ETag / ChangedAt remember the last content served, so Last-Modified is
	// exact even when an event merely drops out of the feed window.
	ETag      string     `json:"-" gorm:"column:etag;type:varchar(80)"`
	ChangedAt *time.Time `json:"-"`
}

// TableName is fixed.
func (FeedToken) TableName() string { return "booking_calendar_feeds" }

// Models lists the tables.
func Models() []interface{} {
	return []interface{}{&Settings{}, &UserPref{}, &CalState{}, &Log{}, &FeedToken{}}
}
