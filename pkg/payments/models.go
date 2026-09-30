package payments

import (
	"time"

	"gorm.io/gorm"
)

// Who pays the processor fee.
const (
	BearerBusiness = "business" // the business absorbs it (a P&L expense)
	BearerCustomer = "customer" // added to the customer's total as a surcharge
)

// Modes.
const (
	ModeTest = "test"
	ModeLive = "live"
)

// Settings is an organisation's online payment configuration. Secrets are
// stored encrypted (same scheme as SMTP passwords) and never returned.
type Settings struct {
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	Enabled        bool      `json:"enabled"`
	Provider       string    `json:"provider" gorm:"type:varchar(30);default:'flutterwave'"`
	Mode           string    `json:"mode" gorm:"type:varchar(10);default:'test'"`
	PublicKey      string    `json:"public_key" gorm:"type:varchar(255)"`
	SecretKey      string    `json:"-" gorm:"type:text"`
	EncryptionKey  string    `json:"-" gorm:"type:text"`
	WebhookHash    string    `json:"-" gorm:"type:text"`
	Methods        []string  `json:"methods" gorm:"type:text;serializer:json"`
	FeeBearer      string    `json:"fee_bearer" gorm:"type:varchar(20);default:'business'"`
	AllowPartial   bool      `json:"allow_partial"`
	MinPartial     float64   `json:"min_partial"`
	Fees           []FeeRule `json:"fees" gorm:"type:text;serializer:json"`
	FeeTaxPercent  float64   `json:"fee_tax_percent"`
	// Result of the last "Test connection".
	LastTestAt      *time.Time `json:"last_test_at"`
	LastTestOK      bool       `json:"last_test_ok"`
	LastTestMessage string     `json:"last_test_message" gorm:"type:text"`
	UpdatedAt       time.Time  `json:"updated_at"`
	UpdatedBy       string     `json:"updated_by" gorm:"type:varchar(255)"`
}

func (Settings) TableName() string { return "pay_settings" }

// Transaction statuses.
const (
	TxPending    = "pending"
	TxSuccessful = "successful"
	TxFailed     = "failed"
	TxCancelled  = "cancelled"
)

// Transaction is one online payment attempt (one Flutterwave checkout).
type Transaction struct {
	ID             string  `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string  `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	InvoiceID      string  `json:"invoice_id" gorm:"type:varchar(64);index;not null"`
	InvoiceNumber  string  `json:"invoice_number" gorm:"type:varchar(50)"`
	Provider       string  `json:"provider" gorm:"type:varchar(30);default:'flutterwave'"`
	Mode           string  `json:"mode" gorm:"type:varchar(10)"`
	TxRef          string  `json:"tx_ref" gorm:"type:varchar(100);uniqueIndex;not null"`
	Method         string  `json:"method" gorm:"type:varchar(40)"`
	MethodLabel    string  `json:"method_label" gorm:"type:varchar(60)"`
	Currency       string  `json:"currency" gorm:"type:varchar(3)"`
	Amount         float64 `json:"amount"`        // applied to the invoice
	Surcharge      float64 `json:"surcharge"`     // fee passed to the customer
	ChargeAmount   float64 `json:"charge_amount"` // sent to Flutterwave (amount + surcharge)
	EstimatedFee   float64 `json:"estimated_fee"`
	Status         string  `json:"status" gorm:"type:varchar(20);index;default:'pending'"`
	// Filled in from Flutterwave's verified transaction.
	FlwTransactionID string     `json:"flw_transaction_id" gorm:"type:varchar(64);index"`
	FlwRef           string     `json:"flw_ref" gorm:"type:varchar(120)"`
	PaymentType      string     `json:"payment_type" gorm:"type:varchar(40)"`
	ChargedAmount    float64    `json:"charged_amount"`
	AppFee           float64    `json:"app_fee"` // processor fee charged (Flutterwave app_fee)
	FeeCharged       bool       `json:"fee_charged"`
	InvoicePaymentID string     `json:"invoice_payment_id" gorm:"type:varchar(64)"`
	Unapplied        float64    `json:"unapplied"` // paid beyond the balance (refund by hand)
	Error            string     `json:"error" gorm:"type:text"`
	CustomerEmail    string     `json:"customer_email" gorm:"type:varchar(255)"`
	CompletedAt      *time.Time `json:"completed_at"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func (Transaction) TableName() string { return "pay_transactions" }

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&Settings{}, &Transaction{}) }
