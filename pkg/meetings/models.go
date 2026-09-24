// Package meetings implements scheduled calls: CRUD, iCalendar invites sent by
// email, and transcripts with an AI (or local extractive) summary.
package meetings

import (
	"time"

	// Embed the IANA timezone database so LoadLocation works in slim containers.
	_ "time/tzdata"
)

// Meeting statuses.
const (
	StatusScheduled  = "scheduled"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusCancelled  = "cancelled"
)

// Location types.
const (
	LocationInApp    = "in_app"
	LocationExternal = "external"
	LocationPhysical = "physical"
)

// DefaultTimezone is used when a request does not name one.
const DefaultTimezone = "Australia/Sydney"

// Meeting is a scheduled call owned by an organisation.
type Meeting struct {
	ID              string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID           string     `json:"org_id" gorm:"type:varchar(255);index;not null"`
	Title           string     `json:"title" gorm:"type:varchar(255);not null"`
	Description     string     `json:"description" gorm:"type:text"`
	StartAt         time.Time  `json:"start_at" gorm:"index"`
	EndAt           time.Time  `json:"end_at"`
	Timezone        string     `json:"timezone" gorm:"type:varchar(64)"`
	LocationType    string     `json:"location_type" gorm:"type:varchar(20)"`
	Location        string     `json:"location" gorm:"type:varchar(500)"`
	JoinURL         string     `json:"join_url" gorm:"type:varchar(1000)"`
	OrganizerID     string     `json:"organizer_id" gorm:"type:varchar(255)"`
	OrganizerName   string     `json:"organizer_name" gorm:"type:varchar(255)"`
	OrganizerEmail  string     `json:"organizer_email" gorm:"type:varchar(255)"`
	Status          string     `json:"status" gorm:"type:varchar(20);index"`
	Recurrence      string     `json:"recurrence" gorm:"type:varchar(20)"`
	RecurrenceCount int        `json:"recurrence_count"`
	ReminderMinutes int        `json:"reminder_minutes"`
	Sequence        int        `json:"sequence"`
	InvitesSentAt   *time.Time `json:"invites_sent_at"`
	CancelledAt     *time.Time `json:"cancelled_at"`
	CreatedBy       string     `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`

	Participants  []Participant `json:"participants" gorm:"foreignKey:MeetingID;constraint:OnDelete:CASCADE"`
	Transcript    *Transcript   `json:"transcript,omitempty" gorm:"foreignKey:MeetingID;constraint:OnDelete:CASCADE"`
	HasTranscript bool          `json:"has_transcript" gorm:"-"`
}

func (Meeting) TableName() string { return "meetings" }

// Participant is an invitee of a meeting.
type Participant struct {
	ID         string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	MeetingID  string     `json:"meeting_id" gorm:"type:varchar(64);index;not null"`
	OrgID      string     `json:"org_id" gorm:"type:varchar(255);index"`
	Name       string     `json:"name" gorm:"type:varchar(255)"`
	Email      string     `json:"email" gorm:"type:varchar(255)"`
	CustomerID string     `json:"customer_id,omitempty" gorm:"type:varchar(255)"`
	UserID     string     `json:"user_id,omitempty" gorm:"type:varchar(255)"`
	Response   string     `json:"response" gorm:"type:varchar(20)"` // needs_action, accepted, declined, tentative
	InvitedAt  *time.Time `json:"invited_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (Participant) TableName() string { return "meeting_participants" }

// ActionItem is a follow-up extracted from a transcript.
type ActionItem struct {
	Text  string `json:"text"`
	Owner string `json:"owner,omitempty"`
	Due   string `json:"due,omitempty"`
}

// Transcript holds the text of a meeting and its generated summary.
type Transcript struct {
	ID            string       `json:"id" gorm:"type:varchar(64);primaryKey"`
	MeetingID     string       `json:"meeting_id" gorm:"type:varchar(64);uniqueIndex;not null"`
	OrgID         string       `json:"org_id" gorm:"type:varchar(255);index"`
	Text          string       `json:"text" gorm:"type:text"`
	Notes         string       `json:"notes" gorm:"type:text"`
	Language      string       `json:"language" gorm:"type:varchar(20)"`
	Summary       string       `json:"summary" gorm:"type:text"`
	KeyDecisions  []string     `json:"key_decisions" gorm:"type:text;serializer:json"`
	ActionItems   []ActionItem `json:"action_items" gorm:"type:text;serializer:json"`
	SummarySource string       `json:"summary_source" gorm:"type:varchar(20)"` // anthropic | local
	CreatedBy     string       `json:"created_by" gorm:"type:varchar(255)"`
	CreatedByName string       `json:"created_by_name" gorm:"type:varchar(255)"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

func (Transcript) TableName() string { return "meeting_transcripts" }
