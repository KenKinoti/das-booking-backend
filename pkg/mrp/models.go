// Package mrp is a small manufacturing module: bills of materials built from
// inventory products and work orders that consume components and produce
// finished goods through the inventory ledger.
package mrp

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Work order statuses.
const (
	StatusPlanned    = "planned"
	StatusInProgress = "in_progress"
	StatusDone       = "done"
	StatusCancelled  = "cancelled"
)

// BOM (bill of materials) says which components make OutputQty units of a product.
type BOM struct {
	ID          string         `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID       string         `json:"org_id" gorm:"type:varchar(255);not null;index"`
	ProductID   string         `json:"product_id" gorm:"type:varchar(255);not null;index"`
	ProductName string         `json:"product_name" gorm:"type:varchar(255)"`
	ProductSKU  string         `json:"product_sku" gorm:"type:varchar(100)"`
	Name        string         `json:"name" gorm:"type:varchar(160);not null"`
	OutputQty   int            `json:"output_qty" gorm:"not null;default:1"`
	Notes       string         `json:"notes" gorm:"type:text"`
	IsActive    bool           `json:"is_active" gorm:"not null;default:true"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`
	Components  []BOMComponent `json:"components" gorm:"foreignKey:BOMID"`

	UnitCost   float64 `json:"unit_cost" gorm:"-"`
	WorkOrders int64   `json:"work_orders" gorm:"-"`
}

func (BOM) TableName() string { return "mrp_boms" }

// BOMComponent is one input line of a BOM.
type BOMComponent struct {
	ID          string `json:"id" gorm:"type:varchar(64);primaryKey"`
	BOMID       string `json:"bom_id" gorm:"column:bom_id;type:varchar(64);not null;index"`
	ProductID   string `json:"product_id" gorm:"type:varchar(255);not null"`
	ProductName string `json:"product_name" gorm:"type:varchar(255)"`
	ProductSKU  string `json:"product_sku" gorm:"type:varchar(100)"`
	Quantity    int    `json:"quantity" gorm:"not null"`
	Position    int    `json:"position" gorm:"not null;default:0"`

	UnitCost     float64 `json:"unit_cost" gorm:"-"`
	CurrentStock int     `json:"current_stock" gorm:"-"`
	Unit         string  `json:"unit" gorm:"-"`
}

func (BOMComponent) TableName() string { return "mrp_bom_components" }

// WorkOrder is a job to build Quantity units of a product from its BOM.
type WorkOrder struct {
	ID          string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID       string     `json:"org_id" gorm:"type:varchar(255);not null;index"`
	Number      string     `json:"number" gorm:"type:varchar(30);not null"`
	BOMID       string     `json:"bom_id" gorm:"column:bom_id;type:varchar(64);not null;index"`
	ProductID   string     `json:"product_id" gorm:"type:varchar(255);not null;index"`
	ProductName string     `json:"product_name" gorm:"type:varchar(255)"`
	Quantity    int        `json:"quantity" gorm:"not null"`
	DueDate     string     `json:"due_date" gorm:"type:varchar(10)"`
	Status      string     `json:"status" gorm:"type:varchar(20);not null;default:'planned';index"`
	Notes       string     `json:"notes" gorm:"type:text"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	CreatedBy   string     `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	BOMName      string        `json:"bom_name" gorm:"-"`
	Requirements []Requirement `json:"requirements,omitempty" gorm:"-"`
	Shortages    int           `json:"shortages" gorm:"-"`
	IsOverdue    bool          `json:"is_overdue" gorm:"-"`
}

func (WorkOrder) TableName() string { return "mrp_work_orders" }

func (b *BOM) BeforeCreate(*gorm.DB) error {
	if b.ID == "" {
		b.ID = uuid.New().String()
	}
	return nil
}
func (b *BOMComponent) BeforeCreate(*gorm.DB) error {
	if b.ID == "" {
		b.ID = uuid.New().String()
	}
	return nil
}
func (w *WorkOrder) BeforeCreate(*gorm.DB) error {
	if w.ID == "" {
		w.ID = uuid.New().String()
	}
	return nil
}

// Migrate creates the MRP tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&BOM{}, &BOMComponent{}, &WorkOrder{})
}
