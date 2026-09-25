package models

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// POSUserRef is a lightweight view of a user (cashier, drawer operator) for POS responses.
type POSUserRef struct {
	ID        string `json:"id" gorm:"type:varchar(255);primaryKey"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
}

// TableName maps POSUserRef onto the users table.
func (POSUserRef) TableName() string { return "users" }

// POSCustomerRef is a lightweight view of a customer for POS responses.
type POSCustomerRef struct {
	ID        string `json:"id" gorm:"type:varchar(255);primaryKey"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
}

// TableName maps POSCustomerRef onto the customers table.
func (POSCustomerRef) TableName() string { return "customers" }

// POSTransaction represents a point of sale transaction
type POSTransaction struct {
	ID                string  `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID    string  `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	CustomerID        *string `json:"customer_id,omitempty" gorm:"type:varchar(255);index"`
	CashierID         string  `json:"cashier_id" gorm:"type:varchar(255);not null;index"`
	CashDrawerID      *string `json:"cash_drawer_id,omitempty" gorm:"type:varchar(255);index"`
	TransactionNumber string  `json:"transaction_number" gorm:"type:varchar(50);uniqueIndex"`
	Type              string  `json:"type" gorm:"type:varchar(20);default:'sale';index"`        // sale, return, exchange
	Status            string  `json:"status" gorm:"type:varchar(20);default:'completed';index"` // completed, voided
	SubTotal          float64 `json:"sub_total" gorm:"type:decimal(10,2);not null"`
	TaxAmount         float64 `json:"tax_amount" gorm:"type:decimal(10,2);default:0"`
	TaxInclusive      bool    `json:"tax_inclusive" gorm:"default:false"`
	DiscountAmount    float64 `json:"discount_amount" gorm:"type:decimal(10,2);default:0"`
	TotalAmount       float64 `json:"total_amount" gorm:"type:decimal(10,2);not null"`
	TenderAmount      float64 `json:"tender_amount" gorm:"type:decimal(10,2);not null"`
	ChangeAmount      float64 `json:"change_amount" gorm:"type:decimal(10,2);default:0"`
	// Currency the sale was made in (the organisation's home currency then).
	Currency       string         `json:"currency" gorm:"type:varchar(3)"`
	Notes          string         `json:"notes" gorm:"type:text"`
	ReceiptPrinted bool           `json:"receipt_printed" gorm:"default:false"`
	VoidReason     string         `json:"void_reason,omitempty" gorm:"type:text"`
	VoidedAt       *time.Time     `json:"voided_at,omitempty"`
	VoidedBy       *string        `json:"voided_by,omitempty" gorm:"type:varchar(255)"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Customer *POSCustomerRef `json:"customer,omitempty" gorm:"foreignKey:CustomerID"`
	Cashier  *POSUserRef     `json:"cashier,omitempty" gorm:"foreignKey:CashierID"`
	Items    []POSItem       `json:"items" gorm:"foreignKey:TransactionID"`
	Payments []POSPayment    `json:"payments" gorm:"foreignKey:TransactionID"`
}

// POSItem represents individual items in a POS transaction
type POSItem struct {
	ID              string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	TransactionID   string    `json:"transaction_id" gorm:"type:varchar(255);not null;index"`
	ProductID       *string   `json:"product_id,omitempty" gorm:"type:varchar(255);index"`
	ServiceID       *string   `json:"service_id,omitempty" gorm:"type:varchar(255);index"`
	ItemName        string    `json:"item_name" gorm:"type:varchar(255);not null"`
	SKU             string    `json:"sku" gorm:"type:varchar(100)"`
	ItemType        string    `json:"item_type" gorm:"type:varchar(20);not null"` // product, service, custom
	Quantity        int       `json:"quantity" gorm:"not null;default:1"`
	UnitPrice       float64   `json:"unit_price" gorm:"type:decimal(10,2);not null"`
	TotalPrice      float64   `json:"total_price" gorm:"type:decimal(10,2);not null"` // qty*unit - line discount
	DiscountPercent float64   `json:"discount_percent" gorm:"type:decimal(5,2);default:0"`
	DiscountAmount  float64   `json:"discount_amount" gorm:"type:decimal(10,2);default:0"`
	TaxRate         float64   `json:"tax_rate" gorm:"type:decimal(5,2);default:0"`
	TaxAmount       float64   `json:"tax_amount" gorm:"type:decimal(10,2);default:0"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// POSPayment represents payment methods used in a transaction
type POSPayment struct {
	ID            string     `json:"id" gorm:"type:varchar(255);primaryKey"`
	TransactionID string     `json:"transaction_id" gorm:"type:varchar(255);not null;index"`
	Method        string     `json:"method" gorm:"type:varchar(20);not null"` // cash, card, eftpos, other
	Amount        float64    `json:"amount" gorm:"type:decimal(10,2);not null"`
	Reference     string     `json:"reference" gorm:"type:varchar(100)"`
	Status        string     `json:"status" gorm:"type:varchar(20);default:'completed'"`
	ProcessedAt   *time.Time `json:"processed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// CashDrawer represents cash drawer management
type CashDrawer struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	TerminalID     string         `json:"terminal_id" gorm:"type:varchar(50);not null;index"`
	OpenedBy       string         `json:"opened_by" gorm:"type:varchar(255);not null"`
	ClosedBy       *string        `json:"closed_by,omitempty" gorm:"type:varchar(255)"`
	OpeningAmount  float64        `json:"opening_amount" gorm:"type:decimal(10,2);not null"`
	ClosingAmount  float64        `json:"closing_amount" gorm:"type:decimal(10,2);default:0"`
	ExpectedAmount float64        `json:"expected_amount" gorm:"type:decimal(10,2);default:0"`
	Variance       float64        `json:"variance" gorm:"type:decimal(10,2);default:0"`
	Status         string         `json:"status" gorm:"type:varchar(20);default:'open'"` // open, closed
	OpenedAt       time.Time      `json:"opened_at" gorm:"not null"`
	ClosedAt       *time.Time     `json:"closed_at,omitempty"`
	Notes          string         `json:"notes" gorm:"type:text"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	OpenedByUser *POSUserRef `json:"opened_by_user,omitempty" gorm:"foreignKey:OpenedBy"`
	ClosedByUser *POSUserRef `json:"closed_by_user,omitempty" gorm:"foreignKey:ClosedBy"`

	// Computed (not stored): cash takings since the drawer opened
	CashSales  float64 `json:"cash_sales" gorm:"-"`
	CashChange float64 `json:"cash_change" gorm:"-"`
	SalesCount int64   `json:"sales_count" gorm:"-"`
}

// Discount represents discount rules and promotions
type Discount struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Name           string         `json:"name" gorm:"type:varchar(100);not null"`
	Description    string         `json:"description" gorm:"type:text"`
	Type           string         `json:"type" gorm:"type:varchar(20);not null"` // percentage, fixed_amount, buy_x_get_y
	Value          float64        `json:"value" gorm:"type:decimal(10,2);not null"`
	MinPurchase    float64        `json:"min_purchase" gorm:"type:decimal(10,2);default:0"`
	MaxDiscount    float64        `json:"max_discount" gorm:"type:decimal(10,2);default:0"`
	ApplicableTo   string         `json:"applicable_to" gorm:"type:varchar(20);default:'all'"` // all, specific_products, specific_categories
	StartDate      time.Time      `json:"start_date" gorm:"not null"`
	EndDate        *time.Time     `json:"end_date,omitempty"`
	IsActive       bool           `json:"is_active" gorm:"default:true;index"`
	UsageLimit     int            `json:"usage_limit" gorm:"default:0"` // 0 = unlimited
	UsageCount     int            `json:"usage_count" gorm:"default:0"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`
}

// TaxRate represents tax configuration
type TaxRate struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Name           string         `json:"name" gorm:"type:varchar(50);not null"`
	Rate           float64        `json:"rate" gorm:"type:decimal(5,2);not null"` // e.g., 10.00 for 10% GST
	IsDefault      bool           `json:"is_default" gorm:"default:false"`
	IsActive       bool           `json:"is_active" gorm:"default:true;index"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`
}

// LaybyPayment represents layby/payment plan transactions
type LaybyPayment struct {
	ID              string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID  string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	CustomerID      string         `json:"customer_id" gorm:"type:varchar(255);not null;index"`
	LaybyNumber     string         `json:"layby_number" gorm:"type:varchar(50);uniqueIndex"`
	TotalAmount     float64        `json:"total_amount" gorm:"type:decimal(10,2);not null"`
	PaidAmount      float64        `json:"paid_amount" gorm:"type:decimal(10,2);default:0"`
	RemainingAmount float64        `json:"remaining_amount" gorm:"type:decimal(10,2);not null"`
	DepositPercent  float64        `json:"deposit_percent" gorm:"type:decimal(5,2);default:0"`
	Status          string         `json:"status" gorm:"type:varchar(20);default:'active'"` // active, completed, cancelled
	DueDate         *time.Time     `json:"due_date,omitempty"`
	CompletedAt     *time.Time     `json:"completed_at,omitempty"`
	Notes           string         `json:"notes" gorm:"type:text"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization        `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Customer     Customer            `json:"customer,omitempty" gorm:"foreignKey:CustomerID"`
	Items        []LaybyItem         `json:"items,omitempty" gorm:"foreignKey:LaybyPaymentID"`
	Payments     []LaybyPaymentEntry `json:"payments,omitempty" gorm:"foreignKey:LaybyPaymentID"`
}

// LaybyItem represents items in a layby
type LaybyItem struct {
	ID             string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	LaybyPaymentID string    `json:"layby_payment_id" gorm:"type:varchar(255);not null;index"`
	ProductID      *string   `json:"product_id,omitempty" gorm:"type:varchar(255);index"`
	ServiceID      *string   `json:"service_id,omitempty" gorm:"type:varchar(255);index"`
	ItemName       string    `json:"item_name" gorm:"type:varchar(255);not null"`
	Quantity       int       `json:"quantity" gorm:"not null;default:1"`
	UnitPrice      float64   `json:"unit_price" gorm:"type:decimal(10,2);not null"`
	TotalPrice     float64   `json:"total_price" gorm:"type:decimal(10,2);not null"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	// Relationships
	LaybyPayment LaybyPayment `json:"layby_payment,omitempty" gorm:"foreignKey:LaybyPaymentID"`
	Product      *Product     `json:"product,omitempty" gorm:"foreignKey:ProductID"`
	Service      *Service     `json:"service,omitempty" gorm:"foreignKey:ServiceID"`
}

// LaybyPaymentEntry represents individual payments made towards a layby
type LaybyPaymentEntry struct {
	ID             string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	LaybyPaymentID string    `json:"layby_payment_id" gorm:"type:varchar(255);not null;index"`
	Amount         float64   `json:"amount" gorm:"type:decimal(10,2);not null"`
	Method         string    `json:"method" gorm:"type:varchar(20);not null"` // cash, card, eftpos
	Reference      string    `json:"reference" gorm:"type:varchar(100)"`
	ReceivedBy     string    `json:"received_by" gorm:"type:varchar(255);not null"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	// Relationships
	LaybyPayment   LaybyPayment `json:"layby_payment,omitempty" gorm:"foreignKey:LaybyPaymentID"`
	ReceivedByUser User         `json:"received_by_user,omitempty" gorm:"foreignKey:ReceivedBy"`
}

// BeforeCreate hooks for generating UUIDs
func (pt *POSTransaction) BeforeCreate(tx *gorm.DB) (err error) {
	if pt.ID == "" {
		pt.ID = uuid.New().String()
	}
	if pt.TransactionNumber == "" {
		pt.TransactionNumber = "S" + time.Now().Format("060102") + "-" + strings.ToUpper(uuid.New().String()[:8])
	}
	return
}

func (pi *POSItem) BeforeCreate(tx *gorm.DB) (err error) {
	if pi.ID == "" {
		pi.ID = uuid.New().String()
	}
	return
}

func (pp *POSPayment) BeforeCreate(tx *gorm.DB) (err error) {
	if pp.ID == "" {
		pp.ID = uuid.New().String()
	}
	return
}

func (cd *CashDrawer) BeforeCreate(tx *gorm.DB) (err error) {
	if cd.ID == "" {
		cd.ID = uuid.New().String()
	}
	return
}

func (d *Discount) BeforeCreate(tx *gorm.DB) (err error) {
	if d.ID == "" {
		d.ID = uuid.New().String()
	}
	return
}

func (tr *TaxRate) BeforeCreate(tx *gorm.DB) (err error) {
	if tr.ID == "" {
		tr.ID = uuid.New().String()
	}
	return
}

func (lp *LaybyPayment) BeforeCreate(tx *gorm.DB) (err error) {
	if lp.ID == "" {
		lp.ID = uuid.New().String()
	}
	if lp.LaybyNumber == "" {
		lp.LaybyNumber = "LB-" + time.Now().Format("20060102") + "-" + uuid.New().String()[:6]
	}
	lp.RemainingAmount = lp.TotalAmount - lp.PaidAmount
	return
}

func (li *LaybyItem) BeforeCreate(tx *gorm.DB) (err error) {
	if li.ID == "" {
		li.ID = uuid.New().String()
	}
	li.TotalPrice = float64(li.Quantity) * li.UnitPrice
	return
}

func (lpe *LaybyPaymentEntry) BeforeCreate(tx *gorm.DB) (err error) {
	if lpe.ID == "" {
		lpe.ID = uuid.New().String()
	}
	return
}

// BeforeUpdate hooks for maintaining data consistency
func (lp *LaybyPayment) BeforeUpdate(tx *gorm.DB) (err error) {
	lp.RemainingAmount = lp.TotalAmount - lp.PaidAmount
	if lp.RemainingAmount <= 0 && lp.Status == "active" {
		lp.Status = "completed"
		now := time.Now()
		lp.CompletedAt = &now
	}
	return
}

func (li *LaybyItem) BeforeUpdate(tx *gorm.DB) (err error) {
	li.TotalPrice = float64(li.Quantity) * li.UnitPrice
	return
}
