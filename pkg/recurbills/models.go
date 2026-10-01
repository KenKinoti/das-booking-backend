// Package recurbills adds recurring supplier bills to Finance → Bills:
// schedules (weekly … yearly or every N months) that create the next bill as
// a draft or approved bill, an "upcoming bills" view, and admin reminder
// emails / in-app notifications so services (hosting, domains, Google
// Workspace …) stay paid and online, plus a weekly digest.
//
// Counting: generated bills are ordinary finance bills, so the P&L's
// count-once rules apply unchanged — drafts (including every "estimate"
// bill until its amount is confirmed) are never counted. A schedule linked to
// an Expenses subscription runs in "track" mode by default: Expenses already
// turns those invoices (Synergy API / statements, Gmail) into paid bills, so
// the schedule creates no bill of its own; it reminds you and matches the
// imported bill to the occurrence instead — nothing is counted twice.
//
// Idempotency: occurrences are unique per (schedule, period date), reminder
// sends are unique per (bill or occurrence, stage, channel) and digests per
// (organisation, week).
package recurbills

import (
	"time"
)

// Bill modes.
const (
	ModeCreate = "create" // the schedule creates finance bills
	ModeTrack  = "track"  // bills arrive from Expenses; the schedule reminds and matches
)

// Amount modes.
const (
	AmountFixed    = "fixed"
	AmountEstimate = "estimate" // the bill is created as a draft to confirm each time
)

// Schedule statuses.
const (
	StatusActive = "active"
	StatusPaused = "paused"
	StatusEnded  = "ended"
)

// Occurrence statuses.
const (
	OccGenerated = "generated" // a bill exists (BillID)
	OccSkipped   = "skipped"   // skipped by a person (or while paused)
	OccMatched   = "matched"   // track mode: an imported bill was matched
	OccPaid      = "paid"      // track mode: marked paid without a bill
)

// RecurringBill is a schedule that produces supplier bills.
type RecurringBill struct {
	ID             string `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Name           string `json:"name" gorm:"type:varchar(255);not null"`
	VendorID       string `json:"vendor_id" gorm:"type:varchar(255);not null;index"`
	AccountID      string `json:"account_id" gorm:"type:varchar(255)"`
	Description    string `json:"description" gorm:"type:varchar(500)"`
	Reference      string `json:"reference" gorm:"type:varchar(255)"`
	Notes          string `json:"notes" gorm:"type:text"`

	Amount     float64 `json:"amount"`
	TaxRate    float64 `json:"tax_rate"`
	Currency   string  `json:"currency" gorm:"type:varchar(3)"`
	AmountMode string  `json:"amount_mode" gorm:"type:varchar(16);not null;default:'fixed'"`

	Frequency string     `json:"frequency" gorm:"type:varchar(16);not null"`
	Interval  int        `json:"interval" gorm:"not null;default:1"`
	StartDate time.Time  `json:"start_date" gorm:"type:date;not null"`
	EndMode   string     `json:"end_mode" gorm:"type:varchar(16);not null;default:'never'"`
	EndAfter  int        `json:"end_after"`
	EndDate   *time.Time `json:"end_date" gorm:"type:date"`

	CreateAs    string `json:"create_as" gorm:"type:varchar(16);not null;default:'draft'"` // draft | approved
	DueDays     int    `json:"due_days"`
	AdvanceDays int    `json:"advance_days" gorm:"not null;default:7"`
	AutoPay     bool   `json:"auto_pay"` // paid automatically by card / direct debit
	Mode        string `json:"mode" gorm:"type:varchar(16);not null;default:'create'"`

	// Service link: free text plus optional Synergy domain / Expenses vendor
	// and detected subscription (Expenses → Subscriptions key).
	Service          string     `json:"service" gorm:"type:varchar(255)"`
	SynergyDomain    string     `json:"synergy_domain" gorm:"type:varchar(255)"`
	ExpenseVendorID  string     `json:"expense_vendor_id" gorm:"type:varchar(64)"`
	SubscriptionKey  string     `json:"subscription_key" gorm:"type:varchar(500);index"`
	ReminderLeads    []int      `json:"reminder_leads" gorm:"type:text;serializer:json"` // nil = organisation default
	ExtraRecipients  []string   `json:"extra_recipients" gorm:"type:text;serializer:json"`
	RemindersOff     bool       `json:"reminders_off"`
	Status           string     `json:"status" gorm:"type:varchar(16);not null;default:'active';index"`
	PausedAt         *time.Time `json:"paused_at"`
	ResumedAt        *time.Time `json:"resumed_at" gorm:"type:date"` // occurrences before this date are not caught up
	LastGeneratedFor *time.Time `json:"last_generated_for" gorm:"type:date"`
	CreatedBy        string     `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`

	// Computed
	VendorName  string   `json:"vendor_name" gorm:"-"`
	AccountName string   `json:"account_name" gorm:"-"`
	Summary     string   `json:"summary" gorm:"-"`
	NextDate    string   `json:"next_date,omitempty" gorm:"-"`
	NextDue     string   `json:"next_due,omitempty" gorm:"-"`
	HomeAmount  *float64 `json:"home_amount,omitempty" gorm:"-"`
	HomeCurr    string   `json:"home_currency,omitempty" gorm:"-"`
	BillCount   int      `json:"bill_count" gorm:"-"`
	LastPaid    string   `json:"last_paid,omitempty" gorm:"-"`
}

// TableName is fixed.
func (RecurringBill) TableName() string { return "finance_recurring_bills" }

// Occurrence is one period of a schedule once something happened to it
// (bill created, skipped, matched, paid). Unique per schedule + period.
type Occurrence struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	RecurringID    string     `json:"recurring_id" gorm:"type:varchar(64);not null;uniqueIndex:idx_recocc_period,priority:1"`
	Period         time.Time  `json:"period" gorm:"type:date;not null;uniqueIndex:idx_recocc_period,priority:2"` // scheduled bill date
	Seq            int        `json:"seq"`
	DueDate        time.Time  `json:"due_date" gorm:"type:date"`
	Status         string     `json:"status" gorm:"type:varchar(16);not null"`
	BillID         string     `json:"bill_id" gorm:"type:varchar(255);index"`
	Amount         float64    `json:"amount"` // original currency
	Currency       string     `json:"currency" gorm:"type:varchar(3)"`
	HomeAmount     float64    `json:"home_amount"`
	FxRate         float64    `json:"fx_rate"`
	SnoozedUntil   *time.Time `json:"snoozed_until" gorm:"type:date"`
	PaidAt         *time.Time `json:"paid_at"`
	Note           string     `json:"note" gorm:"type:varchar(500)"`
	CreatedBy      string     `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (Occurrence) TableName() string { return "finance_recurring_occurrences" }

// BillSnooze pauses reminders for a one-off bill.
type BillSnooze struct {
	BillID         string    `json:"bill_id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Until          time.Time `json:"until" gorm:"type:date"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (BillSnooze) TableName() string { return "finance_bill_snoozes" }

// Settings are the organisation's bill-reminder settings.
type Settings struct {
	OrganizationID   string     `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	Enabled          bool       `json:"enabled"`
	LeadDays         []int      `json:"lead_days" gorm:"type:text;serializer:json"`
	OnDue            bool       `json:"on_due"`
	OverdueEvery     int        `json:"overdue_every"`
	OverdueMax       int        `json:"overdue_max"`
	IncludeOneOff    bool       `json:"include_one_off"` // remind about ordinary bills too
	Digest           bool       `json:"digest"`
	DigestWeekday    int        `json:"digest_weekday"` // 1 = Monday
	DigestHour       int        `json:"digest_hour"`
	SendHour         int        `json:"send_hour"` // reminders go out from this local hour
	NotifyAdmins     bool       `json:"notify_admins"`
	UseNotifyList    bool       `json:"use_notification_recipients"`
	Recipients       []string   `json:"recipients" gorm:"type:text;serializer:json"`
	InApp            bool       `json:"in_app"`
	LastRunAt        *time.Time `json:"last_run_at"`
	LastDigestPeriod string     `json:"last_digest_period" gorm:"type:varchar(16)"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (Settings) TableName() string { return "finance_bill_reminder_settings" }

// DefaultSettings for an organisation that never saved any.
func DefaultSettings(org string) Settings {
	return Settings{OrganizationID: org, Enabled: true, LeadDays: []int{7, 1}, OnDue: true, OverdueEvery: 3, OverdueMax: 3,
		Digest: true, DigestWeekday: 1, DigestHour: 8, SendHour: 8, NotifyAdmins: true, UseNotifyList: true, Recipients: []string{}, InApp: true}
}

// ReminderLog is every reminder / digest sent (or attempted). The unique key
// makes each (target, stage, channel) go out once.
type ReminderLog struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_billrem_key,priority:1;index"`
	Key            string    `json:"key" gorm:"type:varchar(255);not null;uniqueIndex:idx_billrem_key,priority:2"` // bill:<id>:<stage>:email | digest:<week>
	Kind           string    `json:"kind" gorm:"type:varchar(20);not null"`                                        // reminder | digest
	Stage          string    `json:"stage" gorm:"type:varchar(32)"`
	BillID         string    `json:"bill_id" gorm:"type:varchar(255);index"`
	RecurringID    string    `json:"recurring_id" gorm:"type:varchar(64)"`
	Subject        string    `json:"subject" gorm:"type:varchar(500)"`
	Recipients     string    `json:"recipients" gorm:"type:text"`
	Status         string    `json:"status" gorm:"type:varchar(16);not null"` // sending | sent | failed | skipped
	Error          string    `json:"error" gorm:"type:text"`
	Attempts       int       `json:"attempts"`
	InApp          int       `json:"in_app"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (ReminderLog) TableName() string { return "finance_bill_reminder_log" }

// Models lists the tables for AutoMigrate.
func Models() []interface{} {
	return []interface{}{&RecurringBill{}, &Occurrence{}, &BillSnooze{}, &Settings{}, &ReminderLog{}}
}
