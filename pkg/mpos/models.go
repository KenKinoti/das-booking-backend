package mpos

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// SalePhoto is a picture attached to a POS sale: the customer's M-Pesa
// confirmation, a damaged or returned bottle. JPEG only (the till compresses
// it on the device), at most MaxPhotoBytes, a few per sale.
type SalePhoto struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;index"`
	TransactionID  string    `json:"transaction_id" gorm:"type:varchar(255);not null;index"`
	UploadedBy     string    `json:"uploaded_by" gorm:"type:varchar(255);index"`
	Kind           string    `json:"kind" gorm:"type:varchar(20)"` // payment | damage | other
	Note           string    `json:"note" gorm:"type:varchar(200)"`
	ContentType    string    `json:"content_type" gorm:"type:varchar(40)"`
	Size           int       `json:"size"`
	Width          int       `json:"width"`
	Height         int       `json:"height"`
	Data           []byte    `json:"-" gorm:"type:bytea"`
	CreatedAt      time.Time `json:"created_at"`

	UploaderName string `json:"uploader_name,omitempty" gorm:"-"`
}

// TableName keeps the photos next to the other POS tables.
func (SalePhoto) TableName() string { return "pos_sale_photos" }

// BeforeCreate gives the photo a UUID.
func (p *SalePhoto) BeforeCreate(*gorm.DB) error {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	return nil
}

// Migrate creates the photo table and the sale indexes this package relies on
// (the pos_transactions columns themselves come with the core POS model).
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&SalePhoto{}); err != nil {
		return err
	}
	stmts := []string{
		// one sale per till idempotency key (empty = no key)
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_pos_txn_client_ref ON pos_transactions (organization_id, client_ref) WHERE client_ref IS NOT NULL AND client_ref <> ''`,
		// quick picks / people summary scan an organisation's recent sales by person
		`CREATE INDEX IF NOT EXISTS idx_pos_txn_org_cashier_created ON pos_transactions (organization_id, cashier_id, created_at)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			return err
		}
	}
	return nil
}
