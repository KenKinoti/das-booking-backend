// Package schedsend is "Schedule send": every kind of communication the ERP
// sends, at a chosen time or on a schedule.
//
// One model (Message) covers
//   - invoice / quote / receipt emails and payment reminders — sent through the
//     invoicing module's own send path (in-process API call as the person who
//     scheduled it), so the pre-send checks, the branded email with the PDF,
//     the email log and the per-recipient open / view tracking are exactly
//     what "Send" does;
//   - customer messages by email, SMS or WhatsApp to one customer, chosen
//     customers or a group (everyone with an email / phone, a customer type or
//     country, customers with overdue invoices, customers with active
//     subscriptions);
//   - recurring messages (a monthly statement of account with each customer's
//     open invoices and balance, a monthly check-in …);
//   - team messages (in-app notification + optional email / WhatsApp).
//
// A Message is sent at a wall-clock time in the creator's time zone, or
// repeats (daily / weekly on chosen days / monthly on day N or the last day /
// yearly; until a date or a number of times). Text can use merge fields
// ({first_name}, {balance}, {pay_link} …) filled per recipient.
//
// The engine (one leader at a time, Postgres advisory lock) turns each due
// occurrence into a Run and one Delivery per recipient and channel. Deliveries
// are idempotent (unique per message, occurrence, recipient and channel; a
// delivery is claimed before it is sent and never sent twice), retried for
// temporary errors and rate limited for bulk sends. Everything is checked
// again at send time: a voided or paid invoice, an accepted quote, a deleted
// or opted-out customer is skipped with the reason. When the server was down,
// an occurrence up to 6 hours late is still sent; older ones are marked missed
// and the creator is told. The creator is also told when a send fails.
//
// Marketing messages carry an unsubscribe link and respect a customer's "no
// marketing messages" choice (OptOut); service (transactional) messages don't.
// WhatsApp uses the organisation's own number only (wa.Resolve with
// allowPlatform=false): free text inside the 24-hour window, otherwise the
// chosen approved template, otherwise the recipient is skipped with the reason.
// SMS goes through Twilio or Africa's Talking (sms.go).
package schedsend

import (
	"time"
)

// Kinds of scheduled message.
const (
	KindInvoice   = "invoice"   // invoice or quote email (invoicing send path)
	KindReceipt   = "receipt"   // payment receipt email (invoicing send path)
	KindReminder  = "reminder"  // payment reminder (email via invoicing; SMS / WhatsApp here)
	KindCustomer  = "customer"  // message to customers
	KindStatement = "statement" // statement of account (open invoices + balance per customer)
	KindTeam      = "team"      // message to staff
)

// Channels.
const (
	ChanEmail    = "email"
	ChanSMS      = "sms"
	ChanWhatsApp = "whatsapp"
	ChanInApp    = "inapp"
)

// Message statuses.
const (
	StatusScheduled = "scheduled" // waiting for NextRunAt
	StatusPaused    = "paused"    // kept, not sent until resumed
	StatusRunning   = "running"   // an occurrence is being sent
	StatusDone      = "done"      // nothing left to send
	StatusCancelled = "cancelled"
)

// Run statuses (one occurrence).
const (
	RunSending = "sending"
	RunSent    = "sent"    // every delivery sent (or nothing to send but skips)
	RunPartial = "partial" // some sent, some failed
	RunFailed  = "failed"  // nothing sent, something failed
	RunSkipped = "skipped" // every recipient skipped
	RunMissed  = "missed"  // more than CatchUpWindow late (server down)
	// RunWaiting: nothing failed, some deliveries wait for approval in
	// Pending review (pkg/outbox, review.go).
	RunWaiting = "waiting"
)

// Delivery statuses.
const (
	DelPending   = "pending"
	DelSending   = "sending" // claimed by the engine
	DelRetry     = "retry"   // temporary error, NextAttemptAt
	DelSent      = "sent"
	DelDelivered = "delivered"
	DelRead      = "read" // read (WhatsApp) / opened (email)
	DelFailed    = "failed"
	DelSkipped   = "skipped"
	// DelWaiting: waiting for approval in Pending review (ReviewID).
	DelWaiting = "waiting"
)

// Repeat frequencies.
const (
	FreqNone    = "none"
	FreqDaily   = "daily"
	FreqWeekly  = "weekly"
	FreqMonthly = "monthly"
	FreqYearly  = "yearly"
)

// Audience modes (customer, statement and reminder messages; team).
const (
	AudOne    = "one"    // one customer
	AudChosen = "chosen" // chosen customers
	AudGroup  = "group"  // a group of customers
	AudStaff  = "staff"  // team messages
)

// Customer groups.
const (
	GroupAllEmail      = "all_email"            // every customer with an email address
	GroupAllPhone      = "all_phone"            // every customer with a phone number
	GroupType          = "customer_type"        // business (has a company name) | individual
	GroupCountry       = "country"              // ISO country code
	GroupOverdue       = "overdue"              // customers with overdue invoices
	GroupSubscriptions = "active_subscriptions" // customers with an active client subscription
	GroupOpenBalance   = "open_balance"         // customers with any unpaid invoice (statements)
)

// DefaultTimezone is used when neither the creator nor the organisation has one.
const DefaultTimezone = "Africa/Nairobi"

// CatchUpWindow: an occurrence this late (the server was down) is still sent.
const CatchUpWindow = 6 * time.Hour

// Recurrence is when a message repeats.
type Recurrence struct {
	Freq     string `json:"freq"`               // none | daily | weekly | monthly | yearly
	Interval int    `json:"interval,omitempty"` // every N days / weeks / months / years (default 1)
	// Weekdays (weekly): 0 = Sunday … 6 = Saturday; empty = the start day.
	Weekdays []int `json:"weekdays,omitempty"`
	// MonthDay (monthly): 1–31 (shorter months use their last day), -1 = the
	// last day of the month; 0 = the start day.
	MonthDay int    `json:"month_day,omitempty"`
	Until    string `json:"until,omitempty"` // last local date (inclusive), YYYY-MM-DD
	Count    int    `json:"count,omitempty"` // at most this many sends (0 = no limit)
	// StopWhenPaid (payment reminders): stop once the invoice is paid.
	StopWhenPaid bool `json:"stop_when_paid,omitempty"`
}

// Audience is who a customer or team message goes to.
type Audience struct {
	Mode        string   `json:"mode"` // one | chosen | group | staff
	CustomerIDs []string `json:"customer_ids,omitempty"`
	Group       string   `json:"group,omitempty"`       // group key (AudGroup)
	GroupValue  string   `json:"group_value,omitempty"` // customer type / country code
	// Staff: chosen users, or everyone with one of the levels (owner, manager,
	// staff); both empty = every active user.
	UserIDs []string `json:"user_ids,omitempty"`
	Levels  []string `json:"levels,omitempty"`
	// SkipZeroBalance (statements): leave out customers with nothing open.
	SkipZeroBalance bool `json:"skip_zero_balance,omitempty"`
}

// InvoiceSend is what the invoicing send dialog would post (the same
// recipients, subject and message), kept for the send time.
type InvoiceSend struct {
	To             string   `json:"to"`
	CC             []string `json:"cc"`
	BCC            []string `json:"bcc"`
	Subject        string   `json:"subject"`
	Message        string   `json:"message"`
	AppURL         string   `json:"app_url,omitempty"`
	AcceptWarnings bool     `json:"accept_warnings"`
	// Set reports whether To/CC/BCC were chosen (false = the invoice's
	// suggested recipients at send time).
	Set bool `json:"set"`
}

// WhatsAppPlan is how a message goes out on WhatsApp.
type WhatsAppPlan struct {
	Template string `json:"template,omitempty"` // approved template name ("" = free text only)
	Language string `json:"language,omitempty"`
	// Params maps the template's {{1}}, {{2}} … to merge fields or text
	// ("{first_name}", "{amount_due}", "Thank you").
	Params []string `json:"params,omitempty"`
	// URLButton is the dynamic part of the template's URL button.
	URLButton string `json:"url_button,omitempty"`
	// TemplateOnly sends the template even when the 24-hour window is open.
	TemplateOnly bool `json:"template_only,omitempty"`
}

// Message is one scheduled communication (all its occurrences).
type Message struct {
	ID             string `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string `json:"-" gorm:"type:varchar(255);not null;index"`
	Kind           string `json:"kind" gorm:"type:varchar(16);not null;index"`
	Title          string `json:"title" gorm:"type:varchar(200)"`
	// Channels: email | sms | whatsapp | inapp.
	Channels []string `json:"channels" gorm:"type:text;serializer:json"`
	// Marketing: offers / promotions — unsubscribe link, opt-outs respected.
	Marketing bool `json:"marketing" gorm:"not null;default:false"`

	// Invoice kinds.
	InvoiceID    string       `json:"invoice_id,omitempty" gorm:"type:varchar(64);index"`
	PaymentID    string       `json:"payment_id,omitempty" gorm:"type:varchar(64)"`
	DocNumber    string       `json:"doc_number,omitempty" gorm:"type:varchar(64)"`
	DocType      string       `json:"doc_type,omitempty" gorm:"type:varchar(16)"`
	CustomerName string       `json:"customer_name,omitempty" gorm:"type:varchar(255)"`
	InvoiceSend  InvoiceSend  `json:"invoice_send" gorm:"type:text;serializer:json"`
	Audience     Audience     `json:"audience" gorm:"type:text;serializer:json"`
	Subject      string       `json:"subject" gorm:"type:varchar(300)"`
	Body         string       `json:"body" gorm:"type:text"`     // email / in-app text with {merge} fields
	SMSBody      string       `json:"sms_body" gorm:"type:text"` // "" = Body
	WhatsApp     WhatsAppPlan `json:"whatsapp" gorm:"column:whatsapp;type:text;serializer:json"`

	// Schedule: LocalSendAt is the wall clock in Timezone ("2006-01-02T15:04").
	Timezone    string     `json:"timezone" gorm:"type:varchar(64);not null"`
	LocalSendAt string     `json:"local_send_at" gorm:"type:varchar(20);not null"`
	Repeat      Recurrence `json:"repeat" gorm:"column:repeat_rule;type:text;serializer:json"`
	NextRunAt   *time.Time `json:"next_run_at" gorm:"index"`
	// NextLocal is NextRunAt as wall-clock text in Timezone (for display).
	NextLocal   string     `json:"next_local" gorm:"-"`
	Occurrences int        `json:"occurrences" gorm:"not null;default:0"` // occurrences handled (sent, skipped or missed)
	Status      string     `json:"status" gorm:"type:varchar(12);not null;index"`
	LastResult  string     `json:"last_result" gorm:"type:varchar(12)"` // status of the last run
	LastRunAt   *time.Time `json:"last_run_at"`
	LastNote    string     `json:"last_note" gorm:"type:varchar(500)"`
	EndedReason string     `json:"ended_reason" gorm:"type:varchar(300)"`
	// ClaimedAt: when an engine claimed it (RUNNING); stale claims are released.
	ClaimedAt *time.Time `json:"-"`

	CreatedBy     string `json:"created_by" gorm:"type:varchar(255);index"`
	CreatedByName string `json:"created_by_name" gorm:"type:varchar(255)"`
	UpdatedBy     string `json:"updated_by" gorm:"type:varchar(255)"`
	// Reviewed: an approver ticked "I've reviewed it" on this one-off
	// message — its client deliveries don't wait in Pending review.
	Reviewed       bool       `json:"reviewed" gorm:"not null;default:false"`
	ReviewedBy     string     `json:"-" gorm:"type:varchar(255)"`
	ReviewedByName string     `json:"reviewed_by_name" gorm:"type:varchar(255)"`
	ReviewedAt     *time.Time `json:"reviewed_at"`
	// ReviewNote says why an "I've reviewed it" tick was not kept (answer only).
	ReviewNote  string     `json:"review_note,omitempty" gorm:"-"`
	CancelledAt *time.Time `json:"cancelled_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (Message) TableName() string { return "scheduled_messages" }

// Run is one occurrence of a message.
type Run struct {
	ID             string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	MessageID      string     `json:"message_id" gorm:"type:varchar(36);not null;uniqueIndex:idx_ss_run,priority:1"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);not null;index"`
	OccurrenceAt   time.Time  `json:"occurrence_at" gorm:"not null;uniqueIndex:idx_ss_run,priority:2"`
	Manual         bool       `json:"manual" gorm:"not null;default:false"` // "Send now"
	Status         string     `json:"status" gorm:"type:varchar(12);not null;index"`
	Total          int        `json:"total"`
	Sent           int        `json:"sent"`
	Failed         int        `json:"failed"`
	Skipped        int        `json:"skipped"`
	Note           string     `json:"note" gorm:"type:varchar(500)"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	Notified       bool       `json:"-" gorm:"not null;default:false"`
}

// TableName is fixed.
func (Run) TableName() string { return "scheduled_message_runs" }

// Delivery is one recipient on one channel in one run.
type Delivery struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	RunID          string    `json:"run_id" gorm:"type:varchar(36);not null;index"`
	MessageID      string    `json:"message_id" gorm:"type:varchar(36);not null;uniqueIndex:idx_ss_delivery,priority:1"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;index"`
	OccurrenceAt   time.Time `json:"occurrence_at" gorm:"not null;uniqueIndex:idx_ss_delivery,priority:2"`
	// RecipientKey: customer:<id> | user:<id> | invoice:<id>.
	RecipientKey  string `json:"recipient_key" gorm:"type:varchar(300);not null;uniqueIndex:idx_ss_delivery,priority:3"`
	Channel       string `json:"channel" gorm:"type:varchar(12);not null;uniqueIndex:idx_ss_delivery,priority:4"`
	RecipientType string `json:"recipient_type" gorm:"type:varchar(12)"` // customer | user | invoice
	RecipientID   string `json:"recipient_id" gorm:"type:varchar(255);index"`
	Name          string `json:"name" gorm:"type:varchar(255)"`
	Address       string `json:"address" gorm:"type:varchar(320)"` // email or phone
	Status        string `json:"status" gorm:"type:varchar(12);not null;index"`
	Reason        string `json:"reason" gorm:"type:varchar(500)"`
	Mode          string `json:"mode" gorm:"type:varchar(16)"` // whatsapp: text | template
	Template      string `json:"template,omitempty" gorm:"type:varchar(120)"`
	Preview       string `json:"preview" gorm:"type:text"`
	ProviderID    string `json:"provider_id" gorm:"type:varchar(200);index"` // wamid, SMS id, invoice email log id, Message-ID
	// ReviewID: its Pending-review item while it waits (and after).
	ReviewID      string     `json:"review_id" gorm:"type:varchar(64);index"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at" gorm:"index"`
	ClaimedAt     *time.Time `json:"-"`
	SentAt        *time.Time `json:"sent_at"`
	DeliveredAt   *time.Time `json:"delivered_at"`
	ReadAt        *time.Time `json:"read_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (Delivery) TableName() string { return "scheduled_message_deliveries" }

// OptOut: the customer gets no marketing messages (service messages such as
// invoices, receipts, reminders and statements still go out).
type OptOut struct {
	OrganizationID string    `json:"-" gorm:"type:varchar(255);primaryKey"`
	CustomerID     string    `json:"customer_id" gorm:"type:varchar(255);primaryKey"`
	Source         string    `json:"source" gorm:"type:varchar(20)"` // link | staff | sms
	MessageID      string    `json:"message_id" gorm:"type:varchar(36)"`
	By             string    `json:"by" gorm:"type:varchar(255)"`
	CreatedAt      time.Time `json:"created_at"`
}

// TableName is fixed.
func (OptOut) TableName() string { return "customer_marketing_optouts" }

// SMSSettings is the organisation's SMS provider choice and the Africa's
// Talking account (Twilio keeps its existing integration settings).
type SMSSettings struct {
	OrganizationID string     `json:"-" gorm:"type:varchar(255);primaryKey"`
	Provider       string     `json:"provider" gorm:"type:varchar(20)"` // africastalking | twilio | "" (whichever is set up)
	ATUsername     string     `json:"at_username" gorm:"type:varchar(120)"`
	ATAPIKey       string     `json:"-" gorm:"type:varchar(500)"` // encrypted (mail.EncryptSecret)
	ATSenderID     string     `json:"at_sender_id" gorm:"type:varchar(20)"`
	LastTestAt     *time.Time `json:"last_test_at"`
	LastTestOK     bool       `json:"last_test_ok"`
	LastTestNote   string     `json:"last_test_note" gorm:"type:varchar(500)"`
	UpdatedBy      string     `json:"updated_by" gorm:"type:varchar(255)"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (SMSSettings) TableName() string { return "schedsend_sms_settings" }

// Models lists the tables.
func Models() []interface{} {
	return []interface{}{&Message{}, &Run{}, &Delivery{}, &OptOut{}, &SMSSettings{}}
}
