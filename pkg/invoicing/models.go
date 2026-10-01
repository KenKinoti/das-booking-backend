// Package invoicing implements a complete, organisation-scoped invoicing
// module: invoices and quotes with line items, taxes and discounts,
// payments, activity history, recurring schedules, public share links and
// per-organisation settings (numbering, defaults, branding).
package invoicing

import (
	"time"

	"gorm.io/gorm"
)

// Document types
const (
	TypeInvoice = "invoice"
	TypeQuote   = "quote"
	// TypeCreditNote is a credit note brought in from another system (pkg/zoho).
	// Credit notes are kept for reference only: they are not listed with
	// invoices or quotes and are not counted as revenue; the credit they gave
	// is recorded on each invoice it was applied to (Invoice.CreditsApplied).
	TypeCreditNote = "credit_note"
)

// Statuses. "overdue" and "expired" are derived at read time and never stored.
const (
	StatusDraft     = "draft"
	StatusSent      = "sent"
	StatusPartial   = "partial"
	StatusPaid      = "paid"
	StatusVoid      = "void"
	StatusAccepted  = "accepted"
	StatusDeclined  = "declined"
	StatusConverted = "converted"
	StatusOverdue   = "overdue"
	StatusExpired   = "expired"
)

// Settings holds per-organisation invoicing configuration.
type Settings struct {
	OrganizationID      string    `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	BusinessName        string    `json:"business_name" gorm:"type:varchar(255)"`
	Email               string    `json:"email" gorm:"type:varchar(255)"`
	Phone               string    `json:"phone" gorm:"type:varchar(50)"`
	Website             string    `json:"website" gorm:"type:varchar(255)"`
	Address             string    `json:"address" gorm:"type:text"`
	TaxNumber           string    `json:"tax_number" gorm:"type:varchar(100)"`
	TaxNumberLabel      string    `json:"tax_number_label" gorm:"type:varchar(50);default:'ABN'"`
	LogoURL             string    `json:"logo_url" gorm:"type:text"`
	AccentColor         string    `json:"accent_color" gorm:"type:varchar(20);default:'#4f46e5'"`
	Currency            string    `json:"currency" gorm:"type:varchar(3);default:'AUD'"`
	InvoicePrefix       string    `json:"invoice_prefix" gorm:"type:varchar(20);default:'INV-'"`
	NextInvoiceNumber   int       `json:"next_invoice_number" gorm:"default:1001"`
	QuotePrefix         string    `json:"quote_prefix" gorm:"type:varchar(20);default:'QUO-'"`
	NextQuoteNumber     int       `json:"next_quote_number" gorm:"default:1001"`
	NumberPadding       int       `json:"number_padding" gorm:"default:4"`
	DefaultDueDays      int       `json:"default_due_days" gorm:"default:14"`
	QuoteValidDays      int       `json:"quote_valid_days" gorm:"default:30"`
	DefaultTaxRate      float64   `json:"default_tax_rate" gorm:"default:10"`
	TaxLabel            string    `json:"tax_label" gorm:"type:varchar(30);default:'GST'"`
	PricesIncludeTax    bool      `json:"prices_include_tax" gorm:"default:false"`
	DefaultNotes        string    `json:"default_notes" gorm:"type:text"`
	DefaultTerms        string    `json:"default_terms" gorm:"type:text"`
	PaymentInstructions string    `json:"payment_instructions" gorm:"type:text"`
	EmailSubject        string    `json:"email_subject" gorm:"type:varchar(255)"`
	EmailMessage        string    `json:"email_message" gorm:"type:text"`
	UpdatedAt           time.Time `json:"updated_at"`
}

func (Settings) TableName() string { return "billing_settings" }

// TaxRate is a named, reusable tax percentage.
type TaxRate struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	Name           string    `json:"name" gorm:"type:varchar(100);not null"`
	Rate           float64   `json:"rate" gorm:"not null"`
	IsDefault      bool      `json:"is_default"`
	CreatedAt      time.Time `json:"created_at"`
}

func (TaxRate) TableName() string { return "billing_tax_rates" }

// Invoice is an invoice or a quote (DocType).
type Invoice struct {
	ID              string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID  string    `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	DocType         string    `json:"doc_type" gorm:"type:varchar(20);index;not null;default:'invoice'"`
	Number          string    `json:"number" gorm:"type:varchar(50);index"`
	Status          string    `json:"status" gorm:"type:varchar(20);index;not null;default:'draft'"`
	CustomerID      *string   `json:"customer_id" gorm:"type:varchar(255);index"`
	ClientName      string    `json:"client_name" gorm:"type:varchar(255)"`
	ClientEmail     string    `json:"client_email" gorm:"type:varchar(255)"`
	ClientPhone     string    `json:"client_phone" gorm:"type:varchar(50)"`
	ClientAddress   string    `json:"client_address" gorm:"type:text"`
	ClientTaxNumber string    `json:"client_tax_number" gorm:"type:varchar(100)"`
	Reference       string    `json:"reference" gorm:"type:varchar(255)"`
	Title           string    `json:"title" gorm:"type:varchar(255)"`
	IssueDate       time.Time `json:"issue_date" gorm:"type:date;index"`
	DueDate         time.Time `json:"due_date" gorm:"type:date;index"`
	Currency        string    `json:"currency" gorm:"type:varchar(3);default:'AUD'"`
	// Exchange rate locked when the document was created (or when the user
	// chose "Update rate" on a draft): ExchangeRate units of Currency per 1
	// BaseCurrency (the organisation's home currency). TotalBase is Total
	// converted at that rate, for reporting in the home currency.
	BaseCurrency     string     `json:"base_currency" gorm:"type:varchar(3)"`
	ExchangeRate     float64    `json:"exchange_rate"`
	RateSource       string     `json:"rate_source" gorm:"type:varchar(20)"`
	RateDate         *time.Time `json:"rate_date"`
	RateProvider     string     `json:"rate_provider" gorm:"type:varchar(120)"`
	TotalBase        float64    `json:"total_base"`
	PricesIncludeTax bool       `json:"prices_include_tax"`
	DiscountType     string     `json:"discount_type" gorm:"type:varchar(10);default:'percent'"`
	DiscountValue    float64    `json:"discount_value"`
	ShippingAmount   float64    `json:"shipping_amount"`
	Subtotal         float64    `json:"subtotal"`
	DiscountTotal    float64    `json:"discount_total"`
	TaxTotal         float64    `json:"tax_total"`
	Total            float64    `json:"total"`
	AmountPaid       float64    `json:"amount_paid"`
	BalanceDue       float64    `json:"balance_due"`
	Notes            string     `json:"notes" gorm:"type:text"`
	Terms            string     `json:"terms" gorm:"type:text"`
	// CreditsApplied / WriteOffAmount reduce the balance like payments but
	// are not money received (credit notes applied and amounts written off,
	// e.g. in Zoho Books before the invoice was imported).
	CreditsApplied float64 `json:"credits_applied"`
	WriteOffAmount float64 `json:"write_off_amount"`
	// Source is where the document came from ("" = created here, "zoho",
	// "csv"); ExternalID / ExternalURL identify it there. ImportFlag notes a
	// difference found when importing (e.g. totals kept from the source).
	Source      string `json:"source,omitempty" gorm:"type:varchar(20);index"`
	ExternalID  string `json:"external_id,omitempty" gorm:"type:varchar(100)"`
	ExternalURL string `json:"external_url,omitempty" gorm:"type:text"`
	ImportFlag  string `json:"import_flag,omitempty" gorm:"type:text"`
	// CCEmails are copied on every email about this document (sends,
	// reminders) and carried over to duplicates and recurring copies.
	CCEmails        []string   `json:"cc_emails" gorm:"type:text;serializer:json"`
	PublicToken     string     `json:"public_token" gorm:"type:varchar(64);uniqueIndex"`
	SentAt          *time.Time `json:"sent_at"`
	ViewedAt        *time.Time `json:"viewed_at"`
	PaidAt          *time.Time `json:"paid_at"`
	VoidedAt        *time.Time `json:"voided_at"`
	ConvertedFromID *string    `json:"converted_from_id" gorm:"type:varchar(64)"`
	ConvertedToID   *string    `json:"converted_to_id" gorm:"type:varchar(64)"`
	// Recurring schedule (invoices only). When enabled, a copy of this invoice is
	// generated every interval starting at NextRecurrenceDate.
	IsRecurring        bool           `json:"is_recurring"`
	RecurrenceInterval string         `json:"recurrence_interval" gorm:"type:varchar(20)"`
	NextRecurrenceDate *time.Time     `json:"next_recurrence_date" gorm:"type:date"`
	RecurrenceEndDate  *time.Time     `json:"recurrence_end_date" gorm:"type:date"`
	RecurrenceParentID *string        `json:"recurrence_parent_id" gorm:"type:varchar(64);index"`
	CreatedBy          string         `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	DeletedAt          gorm.DeletedAt `json:"-" gorm:"index"`

	Items      []InvoiceItem `json:"items,omitempty" gorm:"foreignKey:InvoiceID;constraint:OnDelete:CASCADE"`
	Payments   []Payment     `json:"payments,omitempty" gorm:"foreignKey:InvoiceID;constraint:OnDelete:CASCADE"`
	Activities []Activity    `json:"activities,omitempty" gorm:"foreignKey:InvoiceID;constraint:OnDelete:CASCADE"`

	// Derived fields (not stored)
	DisplayStatus string `json:"display_status" gorm:"-"`
	DaysOverdue   int    `json:"days_overdue" gorm:"-"`
}

func (Invoice) TableName() string { return "billing_invoices" }

// InvoiceItem is a single line on an invoice or quote.
type InvoiceItem struct {
	ID          string  `json:"id" gorm:"type:varchar(64);primaryKey"`
	InvoiceID   string  `json:"invoice_id" gorm:"type:varchar(64);index;not null"`
	Position    int     `json:"position"`
	ItemType    string  `json:"item_type" gorm:"type:varchar(20);default:'service'"`
	ReferenceID string  `json:"reference_id" gorm:"type:varchar(255)"`
	Description string  `json:"description" gorm:"type:text"`
	Quantity    float64 `json:"quantity"`
	Unit        string  `json:"unit" gorm:"type:varchar(30)"`
	UnitPrice   float64 `json:"unit_price"`
	// OriginalUnitPrice / OriginalCurrency: the catalog price this line was
	// converted from (nil when the price was typed in the document currency).
	OriginalUnitPrice *float64 `json:"original_unit_price"`
	OriginalCurrency  string   `json:"original_currency" gorm:"type:varchar(3)"`
	DiscountPercent   float64  `json:"discount_percent"`
	TaxRate           float64  `json:"tax_rate"`
	TaxName           string   `json:"tax_name" gorm:"type:varchar(50)"`
	LineSubtotal      float64  `json:"line_subtotal"`
	LineDiscount      float64  `json:"line_discount"`
	LineTax           float64  `json:"line_tax"`
	LineTotal         float64  `json:"line_total"`
}

func (InvoiceItem) TableName() string { return "billing_invoice_items" }

// Payment records money received against an invoice.
type Payment struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	InvoiceID      string    `json:"invoice_id" gorm:"type:varchar(64);index;not null"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	Amount         float64   `json:"amount" gorm:"not null"`
	Date           time.Time `json:"date" gorm:"type:date;index"`
	Method         string    `json:"method" gorm:"type:varchar(30)"`
	Reference      string    `json:"reference" gorm:"type:varchar(255)"`
	Notes          string    `json:"notes" gorm:"type:text"`
	// IsDeposit marks money already received when the invoice was written
	// (entered in the invoice editor or when converting a quote).
	IsDeposit bool `json:"is_deposit" gorm:"default:false"`
	// Source / ExternalID: payments imported from another system (pkg/zoho).
	Source     string    `json:"source,omitempty" gorm:"type:varchar(20)"`
	ExternalID string    `json:"external_id,omitempty" gorm:"type:varchar(100)"`
	CreatedBy  string    `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt  time.Time `json:"created_at"`
}

func (Payment) TableName() string { return "billing_payments" }

// Activity is an audit/history entry on an invoice.
type Activity struct {
	ID             string `json:"id" gorm:"type:varchar(64);primaryKey"`
	InvoiceID      string `json:"invoice_id" gorm:"type:varchar(64);index;not null"`
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);index"`
	Type           string `json:"type" gorm:"type:varchar(30)"`
	Message        string `json:"message" gorm:"type:text"`
	UserID         string `json:"user_id" gorm:"type:varchar(255)"`
	// Recipients records who an email went to (sent / reminder activities).
	Recipients *Recipients `json:"recipients,omitempty" gorm:"type:text;serializer:json"`
	CreatedAt  time.Time   `json:"created_at"`
}

// Recipients is the To/CC list used for one send.
type Recipients struct {
	Kind    string   `json:"kind"` // invoice | quote | reminder
	To      string   `json:"to"`
	CC      []string `json:"cc"`
	BCC     []string `json:"bcc,omitempty"`
	Emailed bool     `json:"emailed"` // false when handed to the user's email app
}

func (Activity) TableName() string { return "billing_activities" }

// Models lists everything that needs migrating.
func Models() []interface{} {
	return []interface{}{&Settings{}, &TaxRate{}, &Invoice{}, &InvoiceItem{}, &Payment{}, &Activity{}, &EmailLog{}, &Attachment{}}
}

// EmailLog records every email about a document (sent, test or failed) with
// what was checked before sending and the amounts it showed — so a send can
// be audited and debugged from the invoice's activity tab.
type EmailLog struct {
	ID             string            `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string            `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	InvoiceID      string            `json:"invoice_id" gorm:"type:varchar(64);index;not null"`
	Kind           string            `json:"kind" gorm:"type:varchar(20)"`   // invoice | quote | reminder | receipt
	Mode           string            `json:"mode" gorm:"type:varchar(20)"`   // email | test | mailto
	Status         string            `json:"status" gorm:"type:varchar(20)"` // sent | failed | recorded
	UserID         string            `json:"user_id" gorm:"type:varchar(255)"`
	UserEmail      string            `json:"user_email" gorm:"type:varchar(255)"`
	FromAddress    string            `json:"from" gorm:"type:varchar(320)"`
	Source         string            `json:"source" gorm:"type:varchar(30)"`
	To             string            `json:"to" gorm:"type:varchar(320)"`
	CC             []string          `json:"cc" gorm:"type:text;serializer:json"`
	BCC            []string          `json:"bcc" gorm:"type:text;serializer:json"`
	Subject        string            `json:"subject" gorm:"type:text"`
	MessageID      string            `json:"message_id" gorm:"type:varchar(255)"`
	Response       string            `json:"response" gorm:"type:text"`
	ErrorCode      string            `json:"error_code" gorm:"type:varchar(40)"`
	Error          string            `json:"error" gorm:"type:text"`
	Checks         []Check           `json:"checks" gorm:"type:text;serializer:json"`
	Amounts        map[string]string `json:"amounts" gorm:"type:text;serializer:json"`
	Attachments    []string          `json:"attachments" gorm:"type:text;serializer:json"`
	PaymentID      string            `json:"payment_id,omitempty" gorm:"type:varchar(64)"`
	DurationMS     int               `json:"duration_ms"`
	CreatedAt      time.Time         `json:"created_at" gorm:"index"`
}

func (EmailLog) TableName() string { return "invoice_email_log" }

// Check is one pre-send validation result.
type Check struct {
	ID       string `json:"id"`
	Severity string `json:"severity"` // error | warning | info | ok
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
}

// Migrate creates/updates the invoicing tables.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(Models()...); err != nil {
		return err
	}
	BackfillBase(db)
	return nil
}
