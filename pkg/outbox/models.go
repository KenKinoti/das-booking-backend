// Package outbox is "Pending review": while an organisation's review setting
// is on (the default), no client message — email, SMS or WhatsApp to a
// customer, their contacts, guests — leaves DASYIN without approval.
//
// Every module that sends to clients asks the gate first (Decide). When the
// message must wait, the module writes an Item holding everything needed to
// send it later (recipients, subject, the text with its placeholders filled
// in, WhatsApp template and parameters, a module payload; attachments are
// references made again at release, e.g. the invoice PDF) and keeps its own
// state honest ("waiting for approval": an invoice is not sent, a schedule
// delivery waits, a subscription notice waits).
//
// Approvers (permission outbox.approve: owners and admins) edit, preview,
// approve (now, or for the item's scheduled time), reject (with a note) or
// do that in bulk; every edit and decision is an Event. Before an item goes,
// the module re-checks it (a reminder for an invoice that is now paid is
// superseded instead of sent) and then sends it through its normal path (the
// same tracking, receipts, delivery logs). The module registers a Releaser for
// the items it owns; a module that started an item through another module's
// send path (a schedule delivery of an invoice email) registers an origin hook
// to hear the outcome.
//
// The engine (one leader, Postgres advisory lock) releases approved items at
// their time, re-checks waiting ones, marks stale ones and emails the
// reviewers soon after messages start waiting (≤ 1 an hour) plus a daily
// summary at 07:30 in the organisation's time zone.
package outbox

import (
	"time"
)

// Item statuses.
const (
	StatusPending    = "pending"
	StatusApproved   = "approved"
	StatusSending    = "sending"
	StatusSent       = "sent"
	StatusRejected   = "rejected"
	StatusSuperseded = "superseded"
	StatusFailed     = "failed"
)

// Channels.
const (
	ChanEmail    = "email"
	ChanSMS      = "sms"
	ChanWhatsApp = "whatsapp"
)

// Trigger kinds.
const (
	TriggerManual    = "manual"
	TriggerAutomatic = "automatic"
	TriggerScheduled = "scheduled"
)

// Kinds.
const (
	KindInvoiceEmail       = "invoice_email"
	KindQuoteEmail         = "quote_email"
	KindReminderEmail      = "reminder_email"
	KindReceiptEmail       = "receipt_email"
	KindNoticeEmail        = "notice_email"
	KindStatement          = "statement"
	KindCustomerMessage    = "customer_message"
	KindPaymentReminder    = "payment_reminder"
	KindSubscriptionNotice = "subscription_notice"
	KindMeetingInvite      = "meeting_invite"
	KindMeetingCancel      = "meeting_cancel"
	KindWhatsAppReply      = "whatsapp_reply"
	KindWhatsAppTemplate   = "whatsapp_template"
)

// KindLabels are the readable kind names.
var KindLabels = map[string]string{
	KindInvoiceEmail: "Invoice email", KindQuoteEmail: "Quote email", KindReminderEmail: "Payment reminder", KindReceiptEmail: "Receipt",
	KindNoticeEmail: "Disregard notice", KindStatement: "Statement of account", KindCustomerMessage: "Customer message",
	KindPaymentReminder: "Payment reminder", KindSubscriptionNotice: "Subscription notice", KindMeetingInvite: "Meeting invitation",
	KindMeetingCancel: "Meeting cancellation", KindWhatsAppReply: "WhatsApp reply", KindWhatsAppTemplate: "WhatsApp message",
}

// Editable field names.
const (
	FieldTo       = "to"
	FieldCC       = "cc"
	FieldBCC      = "bcc"
	FieldSubject  = "subject"
	FieldMessage  = "message"
	FieldWAText   = "whatsapp_text"
	FieldWAParams = "whatsapp_params"
)

// Item is one client message waiting for (or past) review.
type Item struct {
	ID             string `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string `json:"-" gorm:"type:varchar(255);not null;index:idx_outbox_org_status,priority:1"`
	// Key dedupes items a module may try to create twice ("" = none).
	Key     string `json:"-" gorm:"type:varchar(255);index"`
	Kind    string `json:"kind" gorm:"type:varchar(40);not null;index"`
	Module  string `json:"module" gorm:"type:varchar(30);not null"`
	Channel string `json:"channel" gorm:"type:varchar(12);not null"`
	Status  string `json:"status" gorm:"type:varchar(12);not null;index:idx_outbox_org_status,priority:2"`

	To      string   `json:"to" gorm:"type:varchar(500)"`
	ToName  string   `json:"to_name" gorm:"type:varchar(255)"`
	CC      []string `json:"cc" gorm:"type:text;serializer:json"`
	BCC     []string `json:"bcc" gorm:"type:text;serializer:json"`
	Subject string   `json:"subject" gorm:"type:varchar(500)"`
	Message string   `json:"message" gorm:"type:text"`

	// WhatsApp: mode text | template | auto (text inside the 24 h window, else the template).
	WAMode       string   `json:"-" gorm:"column:wa_mode;type:varchar(12)"`
	WAText       string   `json:"-" gorm:"column:wa_text;type:text"`
	WATemplate   string   `json:"-" gorm:"column:wa_template;type:varchar(120)"`
	WALanguage   string   `json:"-" gorm:"column:wa_language;type:varchar(20)"`
	WAParams     []string `json:"-" gorm:"column:wa_params;type:text;serializer:json"`
	WAParamCount int      `json:"-" gorm:"column:wa_param_count"`
	WAButton     string   `json:"-" gorm:"column:wa_button;type:varchar(1000)"`

	// Editable lists the fields a reviewer may change.
	Editable []string `json:"-" gorm:"type:text;serializer:json"`
	// Payload is the module's own data (JSON) for sending later.
	Payload string `json:"-" gorm:"type:text"`
	// Origin / OriginRef: the module that started it when it isn't the
	// module that sends it (schedsend delivery id, subscription event id).
	Origin    string `json:"-" gorm:"type:varchar(30);index"`
	OriginRef string `json:"-" gorm:"type:varchar(64);index"`

	RecordType   string   `json:"-" gorm:"type:varchar(30)"`
	RecordID     string   `json:"-" gorm:"type:varchar(64);index"`
	RecordNumber string   `json:"-" gorm:"type:varchar(64)"`
	RecordLabel  string   `json:"-" gorm:"type:varchar(255)"`
	RecordLink   string   `json:"-" gorm:"type:varchar(500)"`
	CustomerID   string   `json:"-" gorm:"type:varchar(255);index"`
	CustomerName string   `json:"-" gorm:"type:varchar(255)"`
	Amount       *float64 `json:"amount"`
	Currency     string   `json:"currency" gorm:"type:varchar(8)"`
	// Context lines shown in the review drawer (label → value pairs).
	Context []ContextLine `json:"-" gorm:"type:text;serializer:json"`

	Trigger       string `json:"trigger" gorm:"type:varchar(300)"`
	TriggerKind   string `json:"trigger_kind" gorm:"type:varchar(12)"`
	CreatedBy     string `json:"-" gorm:"type:varchar(255);index"`
	CreatedByName string `json:"-" gorm:"type:varchar(255)"`

	SendAt   *time.Time `json:"send_at" gorm:"index"`
	Dated    bool       `json:"-" gorm:"not null;default:false"`
	StaleAt  *time.Time `json:"-"`
	Deadline *time.Time `json:"deadline"`
	Flag     string     `json:"flag" gorm:"type:varchar(300)"`
	Blocks   string     `json:"blocks" gorm:"type:varchar(30)"`
	Edited   bool       `json:"edited" gorm:"not null;default:false"`

	ApprovedBy     string     `json:"-" gorm:"type:varchar(255)"`
	ApprovedByName string     `json:"-" gorm:"type:varchar(255)"`
	ApprovedAt     *time.Time `json:"approved_at"`
	ApproveMode    string     `json:"approve_mode" gorm:"type:varchar(12)"`
	DecidedBy      string     `json:"-" gorm:"type:varchar(255)"`
	DecidedByName  string     `json:"-" gorm:"type:varchar(255)"`
	DecidedAt      *time.Time `json:"decided_at"`
	DecisionNote   string     `json:"decision_note" gorm:"type:varchar(600)"`

	ClaimedAt        *time.Time `json:"-"`
	SentAt           *time.Time `json:"sent_at"`
	Result           string     `json:"result" gorm:"type:varchar(600)"`
	ResultRef        string     `json:"-" gorm:"type:varchar(255)"`
	Error            string     `json:"error" gorm:"type:text"`
	Attempts         int        `json:"attempts"`
	SupersededReason string     `json:"superseded_reason" gorm:"type:varchar(500)"`
	CheckedAt        *time.Time `json:"-"`
	// NotifiedAt: listed in a soon-after or daily update email.
	NotifiedAt *time.Time `json:"-" gorm:"index"`

	CreatedAt time.Time `json:"created_at" gorm:"index"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Item) TableName() string { return "outbox_items" }

// ContextLine is one "label: value" line in the review drawer.
type ContextLine struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Event is one audited step of an item.
type Event struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;index"`
	ItemID         string    `json:"-" gorm:"type:varchar(64);not null;index"`
	At             time.Time `json:"at"`
	Action         string    `json:"action" gorm:"type:varchar(20);not null"`
	UserID         string    `json:"-" gorm:"type:varchar(255)"`
	UserName       string    `json:"-" gorm:"type:varchar(255)"`
	Text           string    `json:"text" gorm:"type:varchar(600)"`
	Changes        Changes   `json:"changes" gorm:"type:text;serializer:json"`
	Note           string    `json:"note" gorm:"type:varchar(600)"`
}

// TableName is fixed.
func (Event) TableName() string { return "outbox_events" }

// Change is one edited field.
type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// Changes maps field → change (nil when none).
type Changes map[string]Change

// Settings is an organisation's review setting (no row = the defaults).
type Settings struct {
	OrganizationID string `json:"-" gorm:"type:varchar(255);primaryKey"`
	// No gorm defaults: a saved false must stay false (gorm skips zero
	// values that have a default on insert). No row = the defaults.
	Enabled          bool      `json:"enabled" gorm:"not null"`
	AdminsCanSendNow bool      `json:"admins_can_send_now" gorm:"not null"`
	StaleDays        int       `json:"stale_days" gorm:"not null"`
	SoonAfter        bool      `json:"soon_after" gorm:"not null"`
	Daily            bool      `json:"daily" gorm:"not null"`
	DailyTime        string    `json:"daily_time" gorm:"type:varchar(5);not null"`
	UpdatedBy        string    `json:"-" gorm:"type:varchar(255)"`
	UpdatedByName    string    `json:"-" gorm:"type:varchar(255)"`
	CreatedAt        time.Time `json:"-"`
	UpdatedAt        time.Time `json:"-"`
	saved            bool
}

// TableName is fixed.
func (Settings) TableName() string { return "outbox_settings" }

// Digest is one update email to the reviewers.
type Digest struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_outbox_digest,priority:1"`
	Kind           string    `json:"kind" gorm:"type:varchar(10);not null;uniqueIndex:idx_outbox_digest,priority:2"` // soon | daily
	Key            string    `json:"key" gorm:"type:varchar(40);not null;uniqueIndex:idx_outbox_digest,priority:3"`  // soon: unix minute; daily: local date
	SentAt         time.Time `json:"sent_at" gorm:"index"`
	Items          int       `json:"items"`
	Waiting        int       `json:"waiting"`
	Recipients     string    `json:"recipients" gorm:"type:text"`
	Error          string    `json:"error" gorm:"type:text"`
}

// TableName is fixed.
func (Digest) TableName() string { return "outbox_digests" }

// Models lists the tables.
func Models() []interface{} { return []interface{}{&Item{}, &Event{}, &Settings{}, &Digest{}} }

// Defaults.
const (
	DefaultStaleDays   = 7
	DefaultDailyTime   = "07:30"
	SoonAfterDelay     = 15 * time.Minute
	SoonAfterMinGap    = time.Hour
	PrepareAhead       = 24 * time.Hour
	RecheckEvery       = 10 * time.Minute
	StaleSending       = 10 * time.Minute
	ClosedListDays     = 30
	DeadlineSoonWindow = 72 * time.Hour
)
