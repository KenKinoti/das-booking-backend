// Package expenses fetches, reviews and tracks the business's running costs —
// Synergy Wholesale (SOAP API), Google Workspace and other vendors (Gmail
// billing emails), uploaded invoice PDFs/CSVs/images — and turns approved ones
// into paid supplier bills so the one P&L (pkg/pnl) counts each expense once.
//
// Flow:
//
//	source (API / Gmail / upload / manual)
//	   → Item (status "pending": the expense inbox, with extracted fields,
//	     confidence and the source document)
//	   → approve → finance Bill (vendor bill, coded to an expense account,
//	     in the organisation's home currency) + bill payment
//	   → P&L operating expenses (bills by account), dashboards, alerts.
//
// Idempotency: every item has (org, source, external_id) unique, so a re-sync
// never creates it twice; the same invoice arriving from several sources
// (API + email + upload) is detected by vendor + invoice number, or vendor +
// amount + currency + date (±3 days) and kept as one.
package expenses

import (
	"time"
)

// Item statuses.
const (
	StatusPending   = "pending"   // in the inbox, waiting for review
	StatusApproved  = "approved"  // a bill exists (BillID) and P&L counts it
	StatusIgnored   = "ignored"   // not an expense (top-up, statement, test …)
	StatusDuplicate = "duplicate" // same invoice as DuplicateOf
)

// Item kinds.
const (
	KindInvoice  = "invoice"  // an invoice or bill
	KindReceipt  = "receipt"  // a payment receipt for an invoice
	KindPayment  = "payment"  // "payment received" without invoice number
	KindDeclined = "declined" // a failed / declined payment notice (alert only)
	KindLine     = "line"     // one line of a CSV statement / export
)

// Sources.
const (
	SourceSynergy = "synergy_api"
	SourceGmail   = "gmail"
	SourceUpload  = "upload"
	SourceManual  = "manual"
)

// Categories (each maps to one expense account, see accounts.go).
const (
	CatHosting  = "hosting"
	CatDomains  = "domains"
	CatEmail    = "email"
	CatSoftware = "software"
	CatOther    = "other"
	// CatSupport ("support") is declared in synergy_parse.go: Synergy priority tickets.
)

// Categories lists the categories in display order.
var Categories = []string{CatHosting, CatDomains, CatEmail, CatSoftware, CatSupport, CatOther}

// CategoryLabels are the human names (also the expense-account names).
var CategoryLabels = map[string]string{
	CatHosting:  "Web Hosting",
	CatDomains:  "Domain Names",
	CatEmail:    "Email & Workspace",
	CatSoftware: "Software & Subscriptions",
	CatSupport:  "Support Fees",
	CatOther:    "Other Expenses",
}

// Vendor is a supplier whose costs are tracked here.
type Vendor struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_expv_org_key,priority:1"`
	Key            string    `json:"key" gorm:"type:varchar(64);not null;uniqueIndex:idx_expv_org_key,priority:2"` // synergy | google_workspace | custom-…
	Name           string    `json:"name" gorm:"type:varchar(255);not null"`
	Category       string    `json:"category" gorm:"type:varchar(32);not null;default:'other'"` // default category
	Senders        []string  `json:"senders" gorm:"type:text;serializer:json"`                  // email addresses / domains that send its bills
	Builtin        bool      `json:"builtin"`
	FinanceVendor  string    `json:"finance_vendor_id" gorm:"type:varchar(64)"` // finance vendors.id used for bills
	CountFrom      string    `json:"count_from" gorm:"type:varchar(20)"`        // synergy: invoices (default) | statement
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Vendor) TableName() string { return "expense_vendors" }

// Item is one expense document: an inbox entry until approved, then the link
// between the source document and the finance bill.
type Item struct {
	ID             string `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);not null;index;uniqueIndex:idx_expi_ext,priority:1"`
	Source         string `json:"source" gorm:"type:varchar(20);not null;uniqueIndex:idx_expi_ext,priority:2"`
	ExternalID     string `json:"external_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_expi_ext,priority:3"`
	Status         string `json:"status" gorm:"type:varchar(20);not null;index;default:'pending'"`
	Kind           string `json:"kind" gorm:"type:varchar(20);not null;default:'invoice'"`
	VendorID       string `json:"vendor_id" gorm:"type:varchar(64);index"`
	VendorName     string `json:"vendor_name" gorm:"type:varchar(255)"`

	InvoiceNumber string     `json:"invoice_number" gorm:"type:varchar(120);index"`
	InvoiceDate   *time.Time `json:"invoice_date" gorm:"type:date;index"`
	PeriodStart   *time.Time `json:"period_start" gorm:"type:date"`
	PeriodEnd     *time.Time `json:"period_end" gorm:"type:date"`
	Service       string     `json:"service" gorm:"type:varchar(255)"` // e.g. "Google Workspace Business Starter", "example.com.au renewal"
	Description   string     `json:"description" gorm:"type:text"`
	Category      string     `json:"category" gorm:"type:varchar(32);not null;default:'other'"`
	Paid          bool       `json:"paid" gorm:"not null;default:true"`
	PaidDate      *time.Time `json:"paid_date" gorm:"type:date"`

	// Original currency amounts (Total = Net + Tax).
	Currency string  `json:"currency" gorm:"type:varchar(3)"`
	Total    float64 `json:"total"`
	Tax      float64 `json:"tax"`
	Net      float64 `json:"net"`
	TaxNote  string  `json:"tax_note" gorm:"type:varchar(255)"` // e.g. "GST 1/11 of total (estimated)"

	// Home-currency amounts at the rate locked on the item.
	HomeCurrency string     `json:"home_currency" gorm:"type:varchar(3)"`
	FxRate       float64    `json:"fx_rate"` // units of home per 1 Currency
	FxSource     string     `json:"fx_source" gorm:"type:varchar(20)"`
	FxDate       *time.Time `json:"fx_date"`
	HomeTotal    float64    `json:"home_total"`
	HomeTax      float64    `json:"home_tax"`
	HomeNet      float64    `json:"home_net"`

	Confidence float64  `json:"confidence"`                     // 0..1
	Method     string   `json:"method" gorm:"type:varchar(20)"` // api | rules | pdf | csv | ai | manual
	AIExtract  bool     `json:"ai_extracted"`                   // "AI-extracted, please confirm"
	Warnings   []string `json:"warnings" gorm:"type:text;serializer:json"`
	Subject    string   `json:"subject" gorm:"type:varchar(500)"`    // email subject / file name
	Sender     string   `json:"sender" gorm:"type:varchar(255)"`     // email From
	Snippet    string   `json:"snippet" gorm:"type:text"`            // first ~1500 chars of the text it came from
	DocumentID string   `json:"document_id" gorm:"type:varchar(64)"` // stored PDF / file
	Notes      string   `json:"notes" gorm:"type:text"`

	// Lines: an expense created from Synergy statements (source
	// synergy_statement) is one month × category with a line per domain / fee.
	Lines []StatementLine `json:"lines,omitempty" gorm:"type:text;serializer:json"`
	Basis string          `json:"basis,omitempty" gorm:"type:varchar(10)"` // statement items: charge | service

	DedupeKey   string `json:"dedupe_key" gorm:"type:varchar(255);index"`
	DuplicateOf string `json:"duplicate_of" gorm:"type:varchar(64)"`

	CustomerID string `json:"customer_id" gorm:"type:varchar(255);index"` // whole item attributed to one client (see Allocations for splits)
	BillID     string `json:"bill_id" gorm:"type:varchar(64);index"`
	BillLinked bool   `json:"bill_linked"` // approval linked an existing bill instead of creating one

	ApprovedAt *time.Time `json:"approved_at"`
	ApprovedBy string     `json:"approved_by" gorm:"type:varchar(255)"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`

	// Derived
	Allocations  []Allocation `json:"allocations,omitempty" gorm:"-"`
	CustomerName string       `json:"customer_name,omitempty" gorm:"-"`
	BillNumber   string       `json:"bill_number,omitempty" gorm:"-"`
	DocumentName string       `json:"document_name,omitempty" gorm:"-"`
}

// TableName is fixed.
func (Item) TableName() string { return "expense_items" }

// Allocation attributes part of an approved expense to a client (customer),
// for margin per client. Percent of the item's home net amount.
type Allocation struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	ItemID         string    `json:"item_id" gorm:"type:varchar(64);not null;index"`
	CustomerID     string    `json:"customer_id" gorm:"type:varchar(255);not null;index"`
	Percent        float64   `json:"percent"`
	Note           string    `json:"note" gorm:"type:varchar(255)"`
	CreatedAt      time.Time `json:"created_at"`
	CustomerName   string    `json:"customer_name,omitempty" gorm:"-"`
}

// TableName is fixed.
func (Allocation) TableName() string { return "expense_allocations" }

// Document is a stored source file (PDF attachment, upload).
type Document struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_expd_sha,priority:1"`
	SHA256         string    `json:"sha256" gorm:"type:varchar(64);not null;uniqueIndex:idx_expd_sha,priority:2"`
	Filename       string    `json:"filename" gorm:"type:varchar(255)"`
	MimeType       string    `json:"mime_type" gorm:"type:varchar(120)"`
	Size           int       `json:"size"`
	Data           []byte    `json:"-" gorm:"type:bytea"`
	Text           string    `json:"-" gorm:"type:text"`
	Source         string    `json:"source" gorm:"type:varchar(20)"`
	CreatedAt      time.Time `json:"created_at"`
}

// TableName is fixed.
func (Document) TableName() string { return "expense_documents" }

// Connection is a configured source (one per kind per organisation).
type Connection struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_expc_org_kind,priority:1"`
	Kind           string     `json:"kind" gorm:"type:varchar(20);not null;uniqueIndex:idx_expc_org_kind,priority:2"` // synergy | gmail | schedule
	Enabled        bool       `json:"enabled"`
	Account        string     `json:"account" gorm:"type:varchar(255)"` // reseller ID / Gmail address
	SecretEnc      string     `json:"-" gorm:"type:text"`               // API key / refresh token (encrypted)
	ClientID       string     `json:"client_id" gorm:"type:varchar(255)"`
	ClientSecret   string     `json:"-" gorm:"type:text"` // encrypted OAuth client secret (optional; else env)
	Settings       Settings   `json:"settings" gorm:"type:text;serializer:json"`
	State          string     `json:"-" gorm:"type:varchar(128);index"` // pending OAuth state
	StateExpires   *time.Time `json:"-"`
	StateUser      string     `json:"-" gorm:"type:varchar(255)"`
	ReturnURL      string     `json:"-" gorm:"type:varchar(255)"` // app origin to return to after OAuth
	LastSyncAt     *time.Time `json:"last_sync_at"`
	LastStatus     string     `json:"last_status" gorm:"type:varchar(20)"` // ok | error
	LastError      string     `json:"last_error" gorm:"type:text"`
	LastResult     string     `json:"last_result" gorm:"type:text"`
	Balance        *float64   `json:"balance"`
	BalanceAt      *time.Time `json:"balance_at"`
	BalanceSource  string     `json:"balance_source" gorm:"type:varchar(20)"` // api | email | statement
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (Connection) TableName() string { return "expense_connections" }

// Settings are per-connection options.
type Settings struct {
	// Gmail
	Presets       []string `json:"presets,omitempty"`        // google_workspace, synergy
	CustomSenders []string `json:"custom_senders,omitempty"` // extra senders (address or @domain)
	LookbackDays  int      `json:"lookback_days,omitempty"`  // first sync window (default 400)
	// Synergy
	LowBalance float64 `json:"low_balance,omitempty"` // alert below (default 20)
	// Schedule (kind "schedule")
	AutoSync bool `json:"auto_sync,omitempty"`
	Hour     int  `json:"hour,omitempty"` // local hour of the daily sync (default 6)
}

// Renewal is something that will be charged again: a domain or hosting
// service at Synergy Wholesale, or a manual entry.
type Renewal struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_expr_ext,priority:1"`
	Source         string     `json:"source" gorm:"type:varchar(20);not null;uniqueIndex:idx_expr_ext,priority:2"`
	ExternalID     string     `json:"external_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_expr_ext,priority:3"`
	Kind           string     `json:"kind" gorm:"type:varchar(20);not null"` // domain | hosting | ssl | other
	Name           string     `json:"name" gorm:"type:varchar(255);not null"`
	Plan           string     `json:"plan" gorm:"type:varchar(255)"`
	VendorID       string     `json:"vendor_id" gorm:"type:varchar(64)"`
	Status         string     `json:"status" gorm:"type:varchar(60)"`
	ExpiresOn      *time.Time `json:"expires_on" gorm:"type:date;index"`
	AutoRenew      bool       `json:"auto_renew"`
	BillingPeriod  string     `json:"billing_period" gorm:"type:varchar(40)"`
	Cost           *float64   `json:"cost"` // expected renewal cost (Currency), from pricing or set by hand
	Currency       string     `json:"currency" gorm:"type:varchar(3)"`
	CostSource     string     `json:"cost_source" gorm:"type:varchar(40)"` // synergy_pricing | manual | history
	CustomerID     string     `json:"customer_id" gorm:"type:varchar(255);index"`
	Price          *float64   `json:"price"` // what you charge the client for it (optional)
	Active         bool       `json:"active" gorm:"not null;default:true"`
	SeenAt         *time.Time `json:"seen_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CustomerName   string     `json:"customer_name,omitempty" gorm:"-"`
	DaysLeft       *int       `json:"days_left,omitempty" gorm:"-"`
}

// TableName is fixed.
func (Renewal) TableName() string { return "expense_renewals" }

// AlertLog remembers alerts already notified (so a notification goes out once).
type AlertLog struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_expa_key,priority:1"`
	Key            string     `json:"key" gorm:"type:varchar(255);not null;uniqueIndex:idx_expa_key,priority:2"`
	Type           string     `json:"type" gorm:"type:varchar(40)"`
	DismissedAt    *time.Time `json:"dismissed_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// TableName is fixed.
func (AlertLog) TableName() string { return "expense_alert_log" }

// Models lists the tables for AutoMigrate.
func Models() []interface{} {
	return append([]interface{}{&Vendor{}, &Item{}, &Allocation{}, &Document{}, &Connection{}, &Renewal{}, &AlertLog{}}, SynergyModels()...)
}
