package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// OrganizationModules represents which ERP modules are enabled for an organization
type OrganizationModules struct {
	ID                    string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID        string    `json:"organization_id" gorm:"type:varchar(255);not null;uniqueIndex"`
	InventoryEnabled      bool      `json:"inventory_enabled" gorm:"default:false"`
	SupplierEnabled       bool      `json:"supplier_enabled" gorm:"default:false"`
	PurchaseOrderEnabled  bool      `json:"purchase_order_enabled" gorm:"default:false"`
	POSEnabled           bool      `json:"pos_enabled" gorm:"default:false"`
	CRMEnabled           bool      `json:"crm_enabled" gorm:"default:false"`
	ReportsEnabled       bool      `json:"reports_enabled" gorm:"default:false"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
}

// Product represents items/products that can be sold or used in services
type Product struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Name           string         `json:"name" gorm:"type:varchar(255);not null"`
	Description    string         `json:"description" gorm:"type:text"`
	SKU            string         `json:"sku" gorm:"type:varchar(100);uniqueIndex"`
	Barcode        string         `json:"barcode" gorm:"type:varchar(100);index"`
	CategoryID     *string        `json:"category_id,omitempty" gorm:"type:varchar(255);index"`
	BrandID        *string        `json:"brand_id,omitempty" gorm:"type:varchar(255);index"`
	UnitOfMeasure  string         `json:"unit_of_measure" gorm:"type:varchar(20);default:'each'"` // each, kg, liter, etc.
	CostPrice      float64        `json:"cost_price" gorm:"type:decimal(10,2);default:0"`
	SellingPrice   float64        `json:"selling_price" gorm:"type:decimal(10,2);default:0"`
	MinStock       int            `json:"min_stock" gorm:"default:0"`
	MaxStock       int            `json:"max_stock" gorm:"default:0"`
	CurrentStock   int            `json:"current_stock" gorm:"default:0"`
	ReorderPoint   int            `json:"reorder_point" gorm:"default:0"`
	IsActive       bool           `json:"is_active" gorm:"default:true;index"`
	IsSerialized   bool           `json:"is_serialized" gorm:"default:false"`
	Weight         float64        `json:"weight" gorm:"type:decimal(8,3);default:0"`
	Dimensions     string         `json:"dimensions" gorm:"type:varchar(100)"` // L x W x H
	ImageURL       string         `json:"image_url" gorm:"type:varchar(500)"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization     Organization      `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Category         *ProductCategory  `json:"category,omitempty" gorm:"foreignKey:CategoryID"`
	Brand            *Brand           `json:"brand,omitempty" gorm:"foreignKey:BrandID"`
	InventoryItems   []InventoryItem  `json:"inventory_items,omitempty" gorm:"foreignKey:ProductID"`
	POSItems         []POSItem        `json:"pos_items,omitempty" gorm:"foreignKey:ProductID"`
	PurchaseOrderItems []PurchaseOrderItem `json:"purchase_order_items,omitempty" gorm:"foreignKey:ProductID"`
}

// ProductCategory represents product categorization
type ProductCategory struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Name           string         `json:"name" gorm:"type:varchar(100);not null"`
	Description    string         `json:"description" gorm:"type:text"`
	ParentID       *string        `json:"parent_id,omitempty" gorm:"type:varchar(255);index"`
	IsActive       bool           `json:"is_active" gorm:"default:true;index"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization     `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Parent       *ProductCategory `json:"parent,omitempty" gorm:"foreignKey:ParentID"`
	Children     []ProductCategory `json:"children,omitempty" gorm:"foreignKey:ParentID"`
	Products     []Product        `json:"products,omitempty" gorm:"foreignKey:CategoryID"`
}

// Brand represents product brands
type Brand struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Name           string         `json:"name" gorm:"type:varchar(100);not null"`
	Description    string         `json:"description" gorm:"type:text"`
	Website        string         `json:"website" gorm:"type:varchar(255)"`
	LogoURL        string         `json:"logo_url" gorm:"type:varchar(500)"`
	IsActive       bool           `json:"is_active" gorm:"default:true;index"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Products     []Product    `json:"products,omitempty" gorm:"foreignKey:BrandID"`
}

// Supplier represents vendors/suppliers
type Supplier struct {
	ID                string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID    string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Name              string         `json:"name" gorm:"type:varchar(255);not null"`
	ContactPerson     string         `json:"contact_person" gorm:"type:varchar(255)"`
	Email             string         `json:"email" gorm:"type:varchar(255);index"`
	Phone             string         `json:"phone" gorm:"type:varchar(20)"`
	Address           Address        `json:"address" gorm:"embedded;embeddedPrefix:address_"`
	ABN               string         `json:"abn" gorm:"type:varchar(11)"`
	PaymentTerms      string         `json:"payment_terms" gorm:"type:varchar(100)"` // NET30, COD, etc.
	CreditLimit       float64        `json:"credit_limit" gorm:"type:decimal(10,2);default:0"`
	CurrentBalance    float64        `json:"current_balance" gorm:"type:decimal(10,2);default:0"`
	Rating            int            `json:"rating" gorm:"default:0"` // 1-5 star rating
	Notes             string         `json:"notes" gorm:"type:text"`
	IsActive          bool           `json:"is_active" gorm:"default:true;index"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	DeletedAt         gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization   Organization   `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	PurchaseOrders []PurchaseOrder `json:"purchase_orders,omitempty" gorm:"foreignKey:SupplierID"`
}

// PurchaseOrder represents orders placed with suppliers
type PurchaseOrder struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	SupplierID     string         `json:"supplier_id" gorm:"type:varchar(255);not null;index"`
	OrderNumber    string         `json:"order_number" gorm:"type:varchar(50);uniqueIndex"`
	Status         string         `json:"status" gorm:"type:varchar(20);default:'draft';index"` // draft, sent, confirmed, received, cancelled
	OrderDate      time.Time      `json:"order_date" gorm:"not null;index"`
	ExpectedDate   *time.Time     `json:"expected_date,omitempty"`
	ReceivedDate   *time.Time     `json:"received_date,omitempty"`
	SubTotal       float64        `json:"sub_total" gorm:"type:decimal(10,2);default:0"`
	TaxAmount      float64        `json:"tax_amount" gorm:"type:decimal(10,2);default:0"`
	ShippingCost   float64        `json:"shipping_cost" gorm:"type:decimal(10,2);default:0"`
	TotalAmount    float64        `json:"total_amount" gorm:"type:decimal(10,2);default:0"`
	Notes          string         `json:"notes" gorm:"type:text"`
	CreatedBy      string         `json:"created_by" gorm:"type:varchar(255);not null"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization        `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Supplier     Supplier           `json:"supplier,omitempty" gorm:"foreignKey:SupplierID"`
	Creator      User               `json:"creator,omitempty" gorm:"foreignKey:CreatedBy"`
	Items        []PurchaseOrderItem `json:"items,omitempty" gorm:"foreignKey:PurchaseOrderID"`
}

// PurchaseOrderItem represents individual items in a purchase order
type PurchaseOrderItem struct {
	ID               string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	PurchaseOrderID  string    `json:"purchase_order_id" gorm:"type:varchar(255);not null;index"`
	ProductID        string    `json:"product_id" gorm:"type:varchar(255);not null;index"`
	Quantity         int       `json:"quantity" gorm:"not null"`
	UnitCost         float64   `json:"unit_cost" gorm:"type:decimal(10,2);not null"`
	TotalCost        float64   `json:"total_cost" gorm:"type:decimal(10,2);not null"`
	QuantityReceived int       `json:"quantity_received" gorm:"default:0"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`

	// Relationships
	PurchaseOrder PurchaseOrder `json:"purchase_order,omitempty" gorm:"foreignKey:PurchaseOrderID"`
	Product       Product       `json:"product,omitempty" gorm:"foreignKey:ProductID"`
}

// InventoryItem represents individual inventory tracking entries
type InventoryItem struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	ProductID      string         `json:"product_id" gorm:"type:varchar(255);not null;index"`
	LocationID     *string        `json:"location_id,omitempty" gorm:"type:varchar(255);index"`
	SerialNumber   string         `json:"serial_number" gorm:"type:varchar(100);index"`
	BatchNumber    string         `json:"batch_number" gorm:"type:varchar(100);index"`
	Quantity       int            `json:"quantity" gorm:"not null"`
	UnitCost       float64        `json:"unit_cost" gorm:"type:decimal(10,2);default:0"`
	ExpiryDate     *time.Time     `json:"expiry_date,omitempty"`
	Status         string         `json:"status" gorm:"type:varchar(20);default:'available';index"` // available, reserved, sold, damaged
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization      `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Product      Product          `json:"product,omitempty" gorm:"foreignKey:ProductID"`
	Location     *InventoryLocation `json:"location,omitempty" gorm:"foreignKey:LocationID"`
}

// InventoryLocation represents storage locations for inventory
type InventoryLocation struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Name           string         `json:"name" gorm:"type:varchar(100);not null"`
	Description    string         `json:"description" gorm:"type:text"`
	Type           string         `json:"type" gorm:"type:varchar(50);default:'warehouse'"` // warehouse, showroom, service_bay
	IsActive       bool           `json:"is_active" gorm:"default:true;index"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization   Organization    `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	InventoryItems []InventoryItem `json:"inventory_items,omitempty" gorm:"foreignKey:LocationID"`
}

// InventoryMovement tracks all inventory changes
type InventoryMovement struct {
	ID               string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID   string    `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	ProductID        string    `json:"product_id" gorm:"type:varchar(255);not null;index"`
	MovementType     string    `json:"movement_type" gorm:"type:varchar(20);not null;index"` // in, out, adjustment, transfer
	Quantity         int       `json:"quantity" gorm:"not null"`
	PreviousQuantity int       `json:"previous_quantity" gorm:"not null"`
	NewQuantity      int       `json:"new_quantity" gorm:"not null"`
	UnitCost         float64   `json:"unit_cost" gorm:"type:decimal(10,2);default:0"`
	Reference        string    `json:"reference" gorm:"type:varchar(100)"` // PO number, Sale ID, etc.
	ReferenceType    string    `json:"reference_type" gorm:"type:varchar(50)"` // purchase_order, sale, adjustment
	Notes            string    `json:"notes" gorm:"type:text"`
	CreatedBy        string    `json:"created_by" gorm:"type:varchar(255);not null"`
	CreatedAt        time.Time `json:"created_at"`

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Product      Product      `json:"product,omitempty" gorm:"foreignKey:ProductID"`
	Creator      User         `json:"creator,omitempty" gorm:"foreignKey:CreatedBy"`
}

// BeforeCreate hooks for generating UUIDs
func (om *OrganizationModules) BeforeCreate(tx *gorm.DB) (err error) {
	if om.ID == "" {
		om.ID = uuid.New().String()
	}
	return
}

func (p *Product) BeforeCreate(tx *gorm.DB) (err error) {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	if p.SKU == "" {
		p.SKU = "SKU-" + uuid.New().String()[:8]
	}
	return
}

func (pc *ProductCategory) BeforeCreate(tx *gorm.DB) (err error) {
	if pc.ID == "" {
		pc.ID = uuid.New().String()
	}
	return
}

func (b *Brand) BeforeCreate(tx *gorm.DB) (err error) {
	if b.ID == "" {
		b.ID = uuid.New().String()
	}
	return
}

func (s *Supplier) BeforeCreate(tx *gorm.DB) (err error) {
	if s.ID == "" {
		s.ID = uuid.New().String()
	}
	return
}

func (po *PurchaseOrder) BeforeCreate(tx *gorm.DB) (err error) {
	if po.ID == "" {
		po.ID = uuid.New().String()
	}
	if po.OrderNumber == "" {
		po.OrderNumber = "PO-" + time.Now().Format("20060102") + "-" + uuid.New().String()[:6]
	}
	return
}

func (poi *PurchaseOrderItem) BeforeCreate(tx *gorm.DB) (err error) {
	if poi.ID == "" {
		poi.ID = uuid.New().String()
	}
	poi.TotalCost = float64(poi.Quantity) * poi.UnitCost
	return
}

func (ii *InventoryItem) BeforeCreate(tx *gorm.DB) (err error) {
	if ii.ID == "" {
		ii.ID = uuid.New().String()
	}
	return
}

func (il *InventoryLocation) BeforeCreate(tx *gorm.DB) (err error) {
	if il.ID == "" {
		il.ID = uuid.New().String()
	}
	return
}

func (im *InventoryMovement) BeforeCreate(tx *gorm.DB) (err error) {
	if im.ID == "" {
		im.ID = uuid.New().String()
	}
	return
}

// BeforeUpdate hooks for maintaining data consistency
func (poi *PurchaseOrderItem) BeforeUpdate(tx *gorm.DB) (err error) {
	poi.TotalCost = float64(poi.Quantity) * poi.UnitCost
	return
}

func (po *PurchaseOrder) BeforeUpdate(tx *gorm.DB) (err error) {
	// Recalculate total from items
	var items []PurchaseOrderItem
	if err := tx.Where("purchase_order_id = ?", po.ID).Find(&items).Error; err == nil {
		var subTotal float64
		for _, item := range items {
			subTotal += item.TotalCost
		}
		po.SubTotal = subTotal
		po.TotalAmount = po.SubTotal + po.TaxAmount + po.ShippingCost
	}
	return
}