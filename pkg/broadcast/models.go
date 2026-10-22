// Package broadcast is Platform admin → Announcements: messages from the
// DASYIN team (super admins) to the people who use the system — product
// updates, important reminders, offers (upsell) and urgent service notices.
//
// An announcement is written once (title, simple rich text, an optional
// call-to-action button and image), aimed at an audience (organisations,
// business types, plan tiers, countries, role level, recent activity) and
// sent now or at a time in a chosen time zone, optionally every week or
// month until an end date. Sending writes one Delivery per recipient and an
// in-app notification ("From DASYIN"; urgent ones also show as a banner on
// the dashboard). A person who has not read it in the app gets an email after
// a delay — 1 hour for urgent, 3 hours otherwise, or what the announcement
// says (0/1/3/6/24 hours or never) — through the platform's email account.
//
// Offers carry a one-click unsubscribe link (Kenya's Data Protection Act 2019
// and Australia's Spam Act 2003 require a working opt-out in commercial
// messages): it stops offer emails and in-app offers for that person at once. Updates and reminders
// follow the person's "Updates from DASYIN" email switch (pkg/emailprefs,
// category platform_updates); urgent service notices are always sent.
//
// Before important dates (pkg/occasions: Mashujaa Day, Christmas, Black
// Friday …) every super admin is nudged in the app and by email — 14 and 3
// days ahead by default — with a ready draft and a one-click "Schedule".
//
// A scheduler (one instance at a time, Postgres advisory lock) sends due
// announcements, then the email escalations, then the nudges.
package broadcast

import (
	"time"
)

// Kinds.
const (
	KindUpdate    = "update"
	KindReminder  = "reminder"
	KindPromotion = "promotion"
	KindUrgent    = "urgent"
)

// Priorities.
const (
	PriorityUrgent = "urgent"
	PriorityNormal = "normal"
)

// Statuses.
const (
	StatusDraft     = "draft"
	StatusScheduled = "scheduled"
	StatusSending   = "sending"
	StatusSent      = "sent"
	StatusCancelled = "cancelled"
)

// Recurrence.
const (
	RepeatNone    = "none"
	RepeatWeekly  = "weekly"
	RepeatMonthly = "monthly"
)

// Role levels of the audience.
const (
	LevelOwners   = "owners"   // owners and admins only
	LevelManagers = "managers" // managers and above
	LevelEveryone = "everyone"
)

// Email statuses of a delivery.
const (
	EmailPending   = "pending"    // waiting for the delay to pass
	EmailSending   = "sending"    // claimed by the scheduler
	EmailSent      = "sent"       // emailed
	EmailFailed    = "failed"     // the send failed (EmailNote says why)
	EmailSkipped   = "skipped"    // not emailed (EmailNote says why)
	EmailNotNeeded = "not_needed" // read (or dismissed) in the app first
	EmailNever     = "never"      // the announcement is in-app only
)

// NeverEmail is EmailDelayMinutes for in-app only announcements.
const NeverEmail = -1

// DelayChoices are the email delays (minutes) the composer offers.
var DelayChoices = []int{0, 60, 180, 360, 1440, NeverEmail}

// DefaultTimezone is where the DASYIN team works.
const DefaultTimezone = "Africa/Nairobi"

// Audience selects recipients. Empty lists mean "any".
type Audience struct {
	OrganizationIDs []string `json:"organization_ids"`
	BusinessTypes   []string `json:"business_types"`
	PlanTiers       []string `json:"plan_tiers"`
	Countries       []string `json:"countries"`
	// RoleLevel: owners | managers | everyone (default everyone).
	RoleLevel string `json:"role_level"`
	// ActiveWithinDays: signed in within the last N days (0 = anyone).
	ActiveWithinDays int `json:"active_within_days"`
	// ExcludeSuperAdmins leaves the DASYIN team out (the default).
	ExcludeSuperAdmins bool `json:"exclude_super_admins"`
}

// Announcement is one message (and, when it repeats, every occurrence of it).
type Announcement struct {
	ID       string `json:"id" gorm:"type:varchar(36);primaryKey"`
	Title    string `json:"title" gorm:"type:varchar(200);not null"`
	Body     string `json:"body" gorm:"type:text"`      // sanitised HTML
	BodyText string `json:"body_text" gorm:"type:text"` // plain text of Body
	Kind     string `json:"kind" gorm:"type:varchar(20);not null;default:'update';index"`
	Priority string `json:"priority" gorm:"type:varchar(10);not null;default:'normal'"`
	CTALabel string `json:"cta_label" gorm:"type:varchar(60)"`
	CTAURL   string `json:"cta_url" gorm:"type:varchar(1000)"`
	ImageURL string `json:"image_url" gorm:"type:varchar(1000)"`
	// EmailDelayMinutes: email people who haven't read it after this long
	// (NeverEmail = in-app only).
	EmailDelayMinutes int      `json:"email_delay_minutes" gorm:"not null"` // no default tag: 0 is a real value
	Audience          Audience `json:"audience" gorm:"type:text;serializer:json"`

	// Schedule. LocalSendAt is the wall-clock time in Timezone
	// ("2006-01-02T15:04"); SendAt is the same instant in UTC. Empty = send
	// when "Send now" is pressed.
	Timezone    string     `json:"timezone" gorm:"type:varchar(64);not null;default:'Africa/Nairobi'"`
	LocalSendAt string     `json:"local_send_at" gorm:"type:varchar(20)"`
	SendAt      *time.Time `json:"send_at"`
	Recurrence  string     `json:"recurrence" gorm:"type:varchar(10);not null;default:'none'"`
	RepeatUntil string     `json:"repeat_until" gorm:"type:varchar(10)"` // last day (local, inclusive)
	NextSendAt  *time.Time `json:"next_send_at" gorm:"index"`
	// Expiry: stop showing it in the app (and stop emailing) after.
	LocalExpiresAt string     `json:"local_expires_at" gorm:"type:varchar(20)"`
	ExpiresAt      *time.Time `json:"expires_at"`

	Status      string     `json:"status" gorm:"type:varchar(12);not null;default:'draft';index"`
	Occurrences int        `json:"occurrences" gorm:"not null;default:0"`
	LastSentAt  *time.Time `json:"last_sent_at"`
	CancelledAt *time.Time `json:"cancelled_at"`
	LastError   string     `json:"last_error" gorm:"type:text"`

	// The occasion it was planned for (pkg/occasions), if any.
	OccasionKey  string `json:"occasion_key" gorm:"type:varchar(80);index"`
	OccasionDate string `json:"occasion_date" gorm:"type:varchar(10)"`
	OccasionName string `json:"occasion_name" gorm:"type:varchar(160)"`
	// AIDraft: the text was drafted by AI (shown as "AI draft" until edited).
	AIDraft bool `json:"ai_draft" gorm:"not null;default:false"`

	CreatedBy     string    `json:"created_by" gorm:"type:varchar(255)"`
	CreatedByName string    `json:"created_by_name" gorm:"type:varchar(255)"`
	UpdatedBy     string    `json:"updated_by" gorm:"type:varchar(255)"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Announcement) TableName() string { return "broadcast_announcements" }

// Delivery is one announcement occurrence for one person.
type Delivery struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	AnnouncementID string    `json:"announcement_id" gorm:"type:varchar(36);not null;uniqueIndex:idx_bc_delivery,priority:1;index"`
	OccurrenceAt   time.Time `json:"occurrence_at" gorm:"not null;uniqueIndex:idx_bc_delivery,priority:2"`
	UserID         string    `json:"user_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_bc_delivery,priority:3;index"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Email          string    `json:"email" gorm:"type:varchar(255)"`
	Name           string    `json:"name" gorm:"type:varchar(255)"`
	OrgName        string    `json:"org_name" gorm:"type:varchar(255)"`
	Kind           string    `json:"kind" gorm:"type:varchar(20)"`
	IsTest         bool      `json:"is_test" gorm:"not null;default:false"`

	DeliveredAt *time.Time `json:"delivered_at"`
	ReadAt      *time.Time `json:"read_at"`
	DismissedAt *time.Time `json:"dismissed_at"`
	ClickedAt   *time.Time `json:"clicked_at"`

	EmailDueAt   *time.Time `json:"email_due_at" gorm:"index"`
	EmailedAt    *time.Time `json:"emailed_at"`
	EmailStatus  string     `json:"email_status" gorm:"type:varchar(12);not null;default:'pending';index"`
	EmailNote    string     `json:"email_note" gorm:"type:varchar(500)"`
	Unsubscribed bool       `json:"unsubscribed" gorm:"not null;default:false"`
	CreatedAt    time.Time  `json:"created_at"`
}

// TableName is fixed.
func (Delivery) TableName() string { return "broadcast_deliveries" }

// Unsubscribe: the person gets no offers (email or in-app) any more.
type Unsubscribe struct {
	UserID         string    `json:"user_id" gorm:"type:varchar(255);primaryKey"`
	Email          string    `json:"email" gorm:"type:varchar(255)"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255)"`
	AnnouncementID string    `json:"announcement_id" gorm:"type:varchar(36)"` // the offer they unsubscribed from
	Source         string    `json:"source" gorm:"type:varchar(20)"`          // email | app
	CreatedAt      time.Time `json:"created_at"`
}

// TableName is fixed.
func (Unsubscribe) TableName() string { return "broadcast_unsubscribes" }

// Nudge records one "plan a message" nudge so it is sent once.
type Nudge struct {
	ID           string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OccasionKey  string    `json:"occasion_key" gorm:"type:varchar(80);not null;uniqueIndex:idx_bc_nudge,priority:1"`
	OccasionDate string    `json:"occasion_date" gorm:"type:varchar(10);not null;uniqueIndex:idx_bc_nudge,priority:2"`
	LeadDays     int       `json:"lead_days" gorm:"not null;uniqueIndex:idx_bc_nudge,priority:3"`
	OccasionName string    `json:"occasion_name" gorm:"type:varchar(160)"`
	Recipients   int       `json:"recipients"`
	Emailed      int       `json:"emailed"`
	SentAt       time.Time `json:"sent_at"`
}

// TableName is fixed.
func (Nudge) TableName() string { return "broadcast_nudges" }

// Skip: a super admin chose "skip this year" for an occasion.
type Skip struct {
	OccasionKey string    `json:"occasion_key" gorm:"type:varchar(80);primaryKey"`
	Year        int       `json:"year" gorm:"primaryKey;autoIncrement:false"`
	By          string    `json:"by" gorm:"type:varchar(255)"`
	ByName      string    `json:"by_name" gorm:"type:varchar(255)"`
	CreatedAt   time.Time `json:"created_at"`
}

// TableName is fixed.
func (Skip) TableName() string { return "broadcast_occasion_skips" }

// Settings is the one platform-wide settings row (id "platform").
type Settings struct {
	ID string `json:"-" gorm:"type:varchar(20);primaryKey"`
	// NudgeLeadDays: nudge this many days before an important occasion.
	NudgeLeadDays []int `json:"nudge_lead_days" gorm:"type:text;serializer:json"`
	NudgeEmail    bool  `json:"nudge_email" gorm:"not null"`
	NudgesOn      bool  `json:"nudges_on" gorm:"not null"`
	// SendHour is the local hour "Schedule for <date>" uses (8 = 08:00).
	SendHour  int       `json:"send_hour" gorm:"not null"`
	Timezone  string    `json:"timezone" gorm:"type:varchar(64)"`
	UpdatedBy string    `json:"updated_by" gorm:"type:varchar(255)"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Settings) TableName() string { return "broadcast_settings" }

// Models lists the tables.
func Models() []interface{} {
	return []interface{}{&Announcement{}, &Delivery{}, &Unsubscribe{}, &Nudge{}, &Skip{}, &Settings{}}
}
