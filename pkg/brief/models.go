package brief

import (
	"time"

	"gorm.io/gorm"
)

// Days rules.
const (
	DaysEveryDay = "daily"
	DaysWeekdays = "weekdays"
)

// DefaultTime is when a brief is sent unless the person chooses another time.
const DefaultTime = "08:00"

// Snapshot kinds.
const (
	KindScheduled = "scheduled" // the daily brief (once per person per local date)
	KindTest      = "test"      // "Send me a test now" (the latest test of the day)
)

// Snapshot statuses.
const (
	StatusSending = "sending"
	StatusSent    = "sent"    // every chosen channel went out
	StatusPartial = "partial" // one channel went out, another failed
	StatusFailed  = "failed"  // nothing went out
	StatusRetry   = "retry"   // a channel failed with a temporary error; retried soon
	StatusMissed  = "missed"  // the server was down for more than the catch-up window
)

// Channel statuses (EmailStatus / WAStatus).
const (
	ChOff       = ""          // channel not chosen
	ChSending   = "sending"   // being sent
	ChSent      = "sent"      // email accepted by the mail server
	ChAccepted  = "accepted"  // WhatsApp accepted by Meta (statuses follow on the webhook)
	ChDelivered = "delivered" // WhatsApp delivered to the phone
	ChRead      = "read"      // WhatsApp read
	ChFailed    = "failed"
	ChRetry     = "retry" // temporary failure: tried again soon
)

// Settings are one person's morning brief choices (brief_settings). No row
// means the defaults: off, 08:00 every day in the organisation's time zone,
// by email, every section.
type Settings struct {
	UserID         string `json:"-" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string `json:"-" gorm:"type:varchar(255);not null;index"`
	Enabled        bool   `json:"enabled"`
	Time           string `json:"time" gorm:"type:varchar(5);not null;default:'08:00'"`  // HH:MM local
	Days           string `json:"days" gorm:"type:varchar(10);not null;default:'daily'"` // daily | weekdays
	Timezone       string `json:"timezone" gorm:"type:varchar(64)"`                      // "" = the organisation's
	Email          bool   `json:"email" gorm:"not null;default:true"`
	WhatsApp       bool   `json:"whatsapp" gorm:"column:whatsapp;not null;default:false"`
	// SectionsOff are the section keys the person switched off.
	SectionsOff []string `json:"sections_off" gorm:"type:text;serializer:json"`
	// AlsoDigests keeps the separate bill-reminder and client-billing digest
	// emails coming while the brief already covers them (default off).
	AlsoDigests bool `json:"also_digests"`
	// ScheduleFrom: the schedule (on, time, days, zone) last changed at this
	// instant; briefs due before it are neither sent late nor "missed".
	ScheduleFrom time.Time `json:"-"`
	CreatedAt    time.Time `json:"-"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Settings) TableName() string { return "brief_settings" }

// Snapshot is one brief as it was built and delivered (brief_snapshots): the
// history at /brief/:date and the record that makes sending idempotent
// (unique per person, local date and kind).
type Snapshot struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);not null;index"`
	UserID         string     `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_brief_snap_day,priority:1"`
	Date           string     `json:"date" gorm:"type:varchar(10);not null;uniqueIndex:idx_brief_snap_day,priority:2"`
	Kind           string     `json:"kind" gorm:"type:varchar(12);not null;uniqueIndex:idx_brief_snap_day,priority:3"`
	Timezone       string     `json:"timezone" gorm:"type:varchar(64)"`
	ScheduledFor   *time.Time `json:"scheduled_for"`
	Status         string     `json:"status" gorm:"type:varchar(12);index"`
	Headline       string     `json:"headline" gorm:"type:text"`
	Data           *Brief     `json:"brief,omitempty" gorm:"type:text;serializer:json"`
	Attempts       int        `json:"attempts"`
	NextRetryAt    *time.Time `json:"next_retry_at,omitempty" gorm:"index"`
	Late           bool       `json:"late"` // sent after the scheduled time (server catching up)

	EmailStatus string     `json:"email_status" gorm:"type:varchar(12)"`
	EmailTo     string     `json:"email_to" gorm:"type:varchar(255)"`
	EmailError  string     `json:"email_error,omitempty" gorm:"type:text"`
	EmailAt     *time.Time `json:"email_at,omitempty"`

	WAStatus    string     `json:"whatsapp_status" gorm:"column:wa_status;type:varchar(12)"`
	WATo        string     `json:"whatsapp_to" gorm:"column:wa_to;type:varchar(32)"`
	WAKind      string     `json:"whatsapp_kind" gorm:"column:wa_kind;type:varchar(12)"` // template | text
	WAPlatform  bool       `json:"whatsapp_platform" gorm:"column:wa_platform"`          // sent from DASYIN's number
	WAMessageID string     `json:"-" gorm:"column:wa_message_id;type:varchar(200);index"`
	WAError     string     `json:"whatsapp_error,omitempty" gorm:"column:wa_error;type:text"`
	WAAt        *time.Time `json:"whatsapp_at,omitempty" gorm:"column:wa_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Snapshot) TableName() string { return "brief_snapshots" }

// Config is the platform's brief settings (brief_config, one row): the
// WhatsApp template super admins chose in System settings → Notifications.
type Config struct {
	ID         int       `json:"-" gorm:"primaryKey"`
	WATemplate string    `json:"whatsapp_template" gorm:"column:wa_template;type:varchar(120)"`
	WALanguage string    `json:"whatsapp_language" gorm:"column:wa_language;type:varchar(16)"`
	UpdatedBy  string    `json:"updated_by" gorm:"type:varchar(255)"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Config) TableName() string { return "brief_config" }

// Models lists the tables.
func Models() []interface{} { return []interface{}{&Settings{}, &Snapshot{}, &Config{}} }

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }
