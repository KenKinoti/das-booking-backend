// Package smallbiz is the "Business hub": a simple cash book, reminders,
// business profile and reports for sole traders and small businesses that
// don't need full double-entry accounting. All money is stored as integer
// minor units (cents, or mills for 3-decimal currencies).
package smallbiz

import (
	"time"

	"gorm.io/gorm"
)

const (
	TypeIncome  = "income"
	TypeExpense = "expense"
)

// Settings is the org-scoped business profile.
type Settings struct {
	OrganizationID     string    `json:"organization_id" gorm:"type:varchar(64);primaryKey"`
	Configured         bool      `json:"configured" gorm:"default:false"`
	CategoriesSeeded   bool      `json:"-" gorm:"default:false"`
	TradingName        string    `json:"trading_name" gorm:"type:varchar(200)"`
	OwnerName          string    `json:"owner_name" gorm:"type:varchar(200)"`
	RegistrationNumber string    `json:"registration_number" gorm:"type:varchar(100)"`
	TaxNumber          string    `json:"tax_number" gorm:"type:varchar(100)"`
	Industry           string    `json:"industry" gorm:"type:varchar(120)"`
	Email              string    `json:"email" gorm:"type:varchar(200)"`
	Phone              string    `json:"phone" gorm:"type:varchar(60)"`
	Address            string    `json:"address" gorm:"type:text"`
	FinancialYearStart int       `json:"financial_year_start" gorm:"default:7"` // month 1-12
	Currency           string    `json:"currency" gorm:"type:varchar(3)"`
	TaxName            string    `json:"tax_name" gorm:"type:varchar(20)"`
	TaxRate            float64   `json:"tax_rate"`
	TaxRegistered      bool      `json:"tax_registered" gorm:"default:false"`
	OpeningBalance     int64     `json:"opening_balance_minor"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func (Settings) TableName() string { return "smallbiz_settings" }

// Category groups cash book entries.
type Category struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(64);index;not null"`
	Name           string    `json:"name" gorm:"type:varchar(80);not null"`
	Type           string    `json:"type" gorm:"type:varchar(10);not null"`
	TaxFree        bool      `json:"tax_free"`
	SortOrder      int       `json:"sort_order"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	EntryCount     int64     `json:"entry_count" gorm:"-"`
}

func (Category) TableName() string { return "smallbiz_categories" }

// Entry is one line in the cash book. Amount is the tax-inclusive total.
type Entry struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(64);index:idx_smb_entries_org_date;not null"`
	Type           string    `json:"type" gorm:"type:varchar(10);not null"`
	Date           time.Time `json:"date" gorm:"type:date;index:idx_smb_entries_org_date;not null"`
	Amount         int64     `json:"amount_minor" gorm:"not null"`
	Currency       string    `json:"currency" gorm:"type:varchar(3);not null"`
	TaxRate        float64   `json:"tax_rate"`
	Tax            int64     `json:"tax_minor"`
	CategoryID     string    `json:"category_id" gorm:"type:varchar(36);index"`
	PaymentMethod  string    `json:"payment_method" gorm:"type:varchar(40)"`
	Description    string    `json:"description" gorm:"type:varchar(500)"`
	Reference      string    `json:"reference" gorm:"type:varchar(120)"`
	AttachmentURL  string    `json:"attachment_url" gorm:"type:varchar(1000)"`
	AttachmentName string    `json:"attachment_name" gorm:"type:varchar(200)"`
	AttachmentData string    `json:"attachment_data,omitempty" gorm:"type:text"`
	HasAttachment  bool      `json:"has_attachment"`
	// LinkType marks an entry that records money already counted elsewhere in
	// the ERP (an invoice payment, a supplier bill, a POS sale, a pay run …) or
	// money that isn't profit at all (a transfer, loan or owner's money). The
	// Profit & loss report leaves linked entries out so nothing counts twice.
	LinkType  string         `json:"link_type" gorm:"type:varchar(20);default:''"`
	CreatedBy string         `json:"created_by" gorm:"type:varchar(64)"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`

	// Derived
	CategoryName string `json:"category_name" gorm:"-"`
	Balance      *int64 `json:"balance_minor,omitempty" gorm:"-"`
}

func (Entry) TableName() string { return "smallbiz_entries" }

// Link types for Entry.LinkType.
var LinkTypes = map[string]string{
	"invoice":        "Invoice payment",
	"bill":           "Supplier bill",
	"pos":            "POS sale",
	"payroll":        "Pay run",
	"purchase_order": "Purchase order",
	"event":          "Event ticket",
	"transfer":       "Transfer, loan or owner money",
}

// Reminder is a to-do with an optional amount and recurrence.
type Reminder struct {
	ID             string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string     `json:"-" gorm:"type:varchar(64);index;not null"`
	Title          string     `json:"title" gorm:"type:varchar(200);not null"`
	Notes          string     `json:"notes" gorm:"type:text"`
	DueDate        time.Time  `json:"due_date" gorm:"type:date;not null"`
	Amount         int64      `json:"amount_minor"`
	Currency       string     `json:"currency" gorm:"type:varchar(3)"`
	Recurrence     string     `json:"recurrence" gorm:"type:varchar(12);default:'none'"`
	AnchorDay      int        `json:"-"`
	Done           bool       `json:"done" gorm:"index"`
	DoneAt         *time.Time `json:"done_at"`
	NextID         string     `json:"next_id" gorm:"type:varchar(36)"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (Reminder) TableName() string { return "smallbiz_reminders" }

// Models lists tables for AutoMigrate.
func Models() []interface{} {
	return []interface{}{&Settings{}, &Category{}, &Entry{}, &Reminder{}}
}

// Migrate creates / updates the smallbiz tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }

type seedCat struct {
	Name, Type string
	TaxFree    bool
}

// DefaultCategories are seeded the first time an organisation opens the hub.
var DefaultCategories = []seedCat{
	{"Sales", TypeIncome, false},
	{"Services", TypeIncome, false},
	{"Other income", TypeIncome, false},
	{"Stock / COGS", TypeExpense, false},
	{"Rent", TypeExpense, false},
	{"Utilities", TypeExpense, false},
	{"Wages", TypeExpense, true},
	{"Transport / Fuel", TypeExpense, false},
	{"Marketing", TypeExpense, false},
	{"Software", TypeExpense, false},
	{"Bank fees", TypeExpense, true},
	{"Tax", TypeExpense, true},
	{"Other", TypeExpense, false},
}
