package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// POSTransaction represents a point of sale transaction
type POSTransaction struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	CustomerID     *string        `json:"customer_id,omitempty" gorm:"type:varchar(255);index"`
	CashierID      string         `json:"cashier_id" gorm:"type:varchar(255);not null;index"`
	TransactionNumber string      `json:"transaction_number" gorm:"type:varchar(50);uniqueIndex"`
	Type           string         `json:"type" gorm:"type:varchar(20);default:'sale';index"` // sale, return, exchange, void
	Status         string         `json:"status" gorm:"type:varchar(20);default:'completed';index"` // pending, completed, voided, refunded
	SubTotal       float64        `json:"sub_total" gorm:"type:decimal(10,2);not null"`
	TaxAmount      float64        `json:"tax_amount" gorm:"type:decimal(10,2);default:0"`
	DiscountAmount float64        `json:"discount_amount" gorm:"type:decimal(10,2);default:0"`
	TotalAmount    float64        `json:"total_amount" gorm:"type:decimal(10,2);not null"`
	TenderAmount   float64        `json:"tender_amount" gorm:"type:decimal(10,2);not null"`
	ChangeAmount   float64        `json:"change_amount" gorm:"type:decimal(10,2);default:0"`
	Notes          string         `json:"notes" gorm:"type:text"`
	ReceiptPrinted bool           `json:"receipt_printed" gorm:"default:false"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization   `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Customer     *Customer      `json:"customer,omitempty" gorm:"foreignKey:CustomerID"`
	Cashier      User           `json:"cashier,omitempty" gorm:"foreignKey:CashierID"`
	Items        []POSItem      `json:"items,omitempty" gorm:"foreignKey:TransactionID"`
	Payments     []POSPayment   `json:"payments,omitempty" gorm:"foreignKey:TransactionID"`
}

// POSItem represents individual items in a POS transaction
type POSItem struct {
	ID            string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	TransactionID string    `json:"transaction_id" gorm:"type:varchar(255);not null;index"`
	ProductID     *string   `json:"product_id,omitempty" gorm:"type:varchar(255);index"`
	ServiceID     *string   `json:"service_id,omitempty" gorm:"type:varchar(255);index"`
	ItemName      string    `json:"item_name" gorm:"type:varchar(255);not null"`
	ItemType      string    `json:"item_type" gorm:"type:varchar(20);not null"` // product, service, discount, fee
	Quantity      int       `json:"quantity" gorm:"not null;default:1"`
	UnitPrice     float64   `json:"unit_price" gorm:"type:decimal(10,2);not null"`
	TotalPrice    float64   `json:"total_price" gorm:"type:decimal(10,2);not null"`
	DiscountPercent float64 `json:"discount_percent" gorm:"type:decimal(5,2);default:0"`
	DiscountAmount float64  `json:"discount_amount" gorm:"type:decimal(10,2);default:0"`
	TaxRate       float64   `json:"tax_rate" gorm:"type:decimal(5,2);default:0"`
	TaxAmount     float64   `json:"tax_amount" gorm:"type:decimal(10,2);default:0"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// Relationships
	Transaction POSTransaction `json:"transaction,omitempty" gorm:"foreignKey:TransactionID"`
	Product     *Product       `json:"product,omitempty" gorm:"foreignKey:ProductID"`
	Service     *Service       `json:"service,omitempty" gorm:"foreignKey:ServiceID"`
}

// POSPayment represents payment methods used in a transaction
type POSPayment struct {
	ID            string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	TransactionID string    `json:"transaction_id" gorm:"type:varchar(255);not null;index"`
	Method        string    `json:"method" gorm:"type:varchar(20);not null"` // cash, card, eftpos, afterpay, layby
	Amount        float64   `json:"amount" gorm:"type:decimal(10,2);not null"`
	Reference     string    `json:"reference" gorm:"type:varchar(100)"` // Card transaction ID, etc.
	Status        string    `json:"status" gorm:"type:varchar(20);default:'completed'"` // pending, completed, failed
	ProcessedAt   *time.Time `json:"processed_at,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// Relationships
	Transaction POSTransaction `json:"transaction,omitempty" gorm:"foreignKey:TransactionID"`
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
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	OpenedByUser User         `json:"opened_by_user,omitempty" gorm:"foreignKey:OpenedBy"`
	ClosedByUser *User        `json:"closed_by_user,omitempty" gorm:"foreignKey:ClosedBy"`
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

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
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

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
}

// LaybyPayment represents layby/payment plan transactions
type LaybyPayment struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	CustomerID     string         `json:"customer_id" gorm:"type:varchar(255);not null;index"`
	LaybyNumber    string         `json:"layby_number" gorm:"type:varchar(50);uniqueIndex"`
	TotalAmount    float64        `json:"total_amount" gorm:"type:decimal(10,2);not null"`
	PaidAmount     float64        `json:"paid_amount" gorm:"type:decimal(10,2);default:0"`
	RemainingAmount float64       `json:"remaining_amount" gorm:"type:decimal(10,2);not null"`
	DepositPercent float64        `json:"deposit_percent" gorm:"type:decimal(5,2);default:0"`
	Status         string         `json:"status" gorm:"type:varchar(20);default:'active'"` // active, completed, cancelled
	DueDate        *time.Time     `json:"due_date,omitempty"`
	CompletedAt    *time.Time     `json:"completed_at,omitempty"`
	Notes          string         `json:"notes" gorm:"type:text"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization           `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Customer     Customer              `json:"customer,omitempty" gorm:"foreignKey:CustomerID"`
	Items        []LaybyItem           `json:"items,omitempty" gorm:"foreignKey:LaybyPaymentID"`
	Payments     []LaybyPaymentEntry   `json:"payments,omitempty" gorm:"foreignKey:LaybyPaymentID"`
}

// LaybyItem represents items in a layby
type LaybyItem struct {
	ID              string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	LaybyPaymentID  string    `json:"layby_payment_id" gorm:"type:varchar(255);not null;index"`
	ProductID       *string   `json:"product_id,omitempty" gorm:"type:varchar(255);index"`
	ServiceID       *string   `json:"service_id,omitempty" gorm:"type:varchar(255);index"`
	ItemName        string    `json:"item_name" gorm:"type:varchar(255);not null"`
	Quantity        int       `json:"quantity" gorm:"not null;default:1"`
	UnitPrice       float64   `json:"unit_price" gorm:"type:decimal(10,2);not null"`
	TotalPrice      float64   `json:"total_price" gorm:"type:decimal(10,2);not null"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`

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
	LaybyPayment LaybyPayment `json:"layby_payment,omitempty" gorm:"foreignKey:LaybyPaymentID"`
	ReceivedByUser User       `json:"received_by_user,omitempty" gorm:"foreignKey:ReceivedBy"`
}

// BeforeCreate hooks for generating UUIDs
func (pt *POSTransaction) BeforeCreate(tx *gorm.DB) (err error) {
	if pt.ID == "" {
		pt.ID = uuid.New().String()
	}
	if pt.TransactionNumber == "" {
		pt.TransactionNumber = "TXN-" + time.Now().Format("20060102") + "-" + uuid.New().String()[:6]
	}
	return
}

func (pi *POSItem) BeforeCreate(tx *gorm.DB) (err error) {
	if pi.ID == "" {
		pi.ID = uuid.New().String()
	}
	pi.TotalPrice = float64(pi.Quantity) * pi.UnitPrice
	pi.TotalPrice -= pi.DiscountAmount
	pi.TaxAmount = (pi.TotalPrice * pi.TaxRate) / 100
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
func (pi *POSItem) BeforeUpdate(tx *gorm.DB) (err error) {
	pi.TotalPrice = float64(pi.Quantity) * pi.UnitPrice
	pi.TotalPrice -= pi.DiscountAmount
	pi.TaxAmount = (pi.TotalPrice * pi.TaxRate) / 100
	return
}

func (pt *POSTransaction) BeforeUpdate(tx *gorm.DB) (err error) {
	// Recalculate totals from items
	var items []POSItem
	if err := tx.Where("transaction_id = ?", pt.ID).Find(&items).Error; err == nil {
		var subTotal, taxTotal float64
		for _, item := range items {
			subTotal += item.TotalPrice
			taxTotal += item.TaxAmount
		}
		pt.SubTotal = subTotal
		pt.TaxAmount = taxTotal
		pt.TotalAmount = pt.SubTotal + pt.TaxAmount - pt.DiscountAmount
		pt.ChangeAmount = pt.TenderAmount - pt.TotalAmount
	}
	return
}

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