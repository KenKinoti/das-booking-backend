package inventory

import (
	"time"
)

// Product represents an inventory item
type Product struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	SKU            string    `json:"sku" gorm:"unique;not null"`
	Name           string    `json:"name" gorm:"not null"`
	Description    string    `json:"description"`
	Category       string    `json:"category"`
	Brand          string    `json:"brand"`
	CostPrice      float64   `json:"cost_price" gorm:"not null"`
	SellingPrice   float64   `json:"selling_price" gorm:"not null"`
	QuantityOnHand int       `json:"quantity_on_hand" gorm:"default:0"`
	ReorderLevel   int       `json:"reorder_level" gorm:"default:10"`
	MaxStockLevel  int       `json:"max_stock_level" gorm:"default:100"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// StockMovement represents inventory movements (in/out)
type StockMovement struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	ProductID      string    `json:"product_id" gorm:"not null"`
	MovementType   string    `json:"movement_type" gorm:"not null"` // in, out, adjustment
	Quantity       int       `json:"quantity" gorm:"not null"`
	CostPerUnit    float64   `json:"cost_per_unit"`
	Reference      string    `json:"reference"`
	Notes          string    `json:"notes"`
	MovementDate   time.Time `json:"movement_date" gorm:"not null"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Product        Product   `json:"product" gorm:"foreignKey:ProductID"`
}

// PurchaseOrder represents purchase orders for procurement
type PurchaseOrder struct {
	ID             string                `json:"id" gorm:"primaryKey"`
	OrganizationID string                `json:"organization_id" gorm:"not null"`
	PONumber       string                `json:"po_number" gorm:"unique;not null"`
	VendorID       string                `json:"vendor_id" gorm:"not null"`
	OrderDate      time.Time             `json:"order_date" gorm:"not null"`
	ExpectedDate   *time.Time            `json:"expected_date"`
	Status         string                `json:"status" gorm:"default:'draft'"` // draft, sent, received, cancelled
	SubTotal       float64               `json:"subtotal" gorm:"not null"`
	TaxAmount      float64               `json:"tax_amount" gorm:"default:0"`
	Total          float64               `json:"total" gorm:"not null"`
	Notes          string                `json:"notes"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
	LineItems      []PurchaseOrderLine   `json:"line_items" gorm:"foreignKey:PurchaseOrderID"`
}

// PurchaseOrderLine represents line items in purchase orders
type PurchaseOrderLine struct {
	ID               string    `json:"id" gorm:"primaryKey"`
	PurchaseOrderID  string    `json:"purchase_order_id" gorm:"not null"`
	ProductID        string    `json:"product_id" gorm:"not null"`
	Quantity         int       `json:"quantity" gorm:"not null"`
	UnitPrice        float64   `json:"unit_price" gorm:"not null"`
	LineTotal        float64   `json:"line_total" gorm:"not null"`
	ReceivedQuantity int       `json:"received_quantity" gorm:"default:0"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Product          Product   `json:"product" gorm:"foreignKey:ProductID"`
}

// Warehouse represents storage locations
type Warehouse struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Name           string    `json:"name" gorm:"not null"`
	Code           string    `json:"code" gorm:"unique;not null"`
	Address        string    `json:"address"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// InventoryLevel represents stock levels by warehouse
type InventoryLevel struct {
	ID          string    `json:"id" gorm:"primaryKey"`
	ProductID   string    `json:"product_id" gorm:"not null;index"`
	WarehouseID string    `json:"warehouse_id" gorm:"not null;index"`
	Quantity    int       `json:"quantity" gorm:"not null"`
	ReorderLevel int      `json:"reorder_level" gorm:"default:10"`
	MaxLevel    int       `json:"max_level" gorm:"default:100"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Product     Product   `json:"product" gorm:"foreignKey:ProductID"`
	Warehouse   Warehouse `json:"warehouse" gorm:"foreignKey:WarehouseID"`
}

// Category represents product categories
type Category struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Name           string    `json:"name" gorm:"not null"`
	Description    string    `json:"description"`
	ParentID       *string   `json:"parent_id"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Dashboard represents inventory dashboard metrics
type Dashboard struct {
	ProductsCount        int64   `json:"products_count"`
	LowStockCount        int64   `json:"low_stock_count"`
	TotalInventoryValue  float64 `json:"total_inventory_value"`
	PendingPOs           int64   `json:"pending_pos"`
	TopProducts          []Product `json:"top_products,omitempty"`
	WarehouseCount       int64   `json:"warehouse_count"`
}