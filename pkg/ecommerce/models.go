package ecommerce

import (
	"time"
)

// Platform represents e-commerce platforms (Shopify, WooCommerce, etc.)
type Platform struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Name           string    `json:"name" gorm:"not null"` // shopify, woocommerce, magento
	DisplayName    string    `json:"display_name" gorm:"not null"`
	APIEndpoint    string    `json:"api_endpoint"`
	APIKey         string    `json:"api_key"`
	APISecret      string    `json:"api_secret"`
	StoreURL       string    `json:"store_url"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	LastSyncAt     *time.Time `json:"last_sync_at"`
	SyncStatus     string    `json:"sync_status" gorm:"default:'pending'"` // pending, syncing, completed, failed
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// OnlineOrder represents orders from e-commerce platforms
type OnlineOrder struct {
	ID             string              `json:"id" gorm:"primaryKey"`
	OrganizationID string              `json:"organization_id" gorm:"not null"`
	PlatformID     string              `json:"platform_id" gorm:"not null"`
	ExternalID     string              `json:"external_id" gorm:"not null"` // Order ID from platform
	OrderNumber    string              `json:"order_number" gorm:"not null"`
	CustomerEmail  string              `json:"customer_email"`
	CustomerName   string              `json:"customer_name"`
	Status         string              `json:"status" gorm:"not null"` // pending, processing, shipped, delivered, cancelled
	PaymentStatus  string              `json:"payment_status" gorm:"not null"` // pending, paid, refunded
	SubTotal       float64             `json:"subtotal" gorm:"not null"`
	TaxAmount      float64             `json:"tax_amount" gorm:"default:0"`
	ShippingAmount float64             `json:"shipping_amount" gorm:"default:0"`
	Total          float64             `json:"total" gorm:"not null"`
	Currency       string              `json:"currency" gorm:"default:'USD'"`
	OrderDate      time.Time           `json:"order_date" gorm:"not null"`
	ShippingAddress string             `json:"shipping_address"`
	BillingAddress string              `json:"billing_address"`
	Notes          string              `json:"notes"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	LineItems      []OnlineOrderItem   `json:"line_items" gorm:"foreignKey:OnlineOrderID"`
}

// OnlineOrderItem represents line items in online orders
type OnlineOrderItem struct {
	ID            string    `json:"id" gorm:"primaryKey"`
	OnlineOrderID string    `json:"online_order_id" gorm:"not null"`
	ExternalID    string    `json:"external_id"` // Product ID from platform
	SKU           string    `json:"sku"`
	ProductName   string    `json:"product_name" gorm:"not null"`
	Quantity      int       `json:"quantity" gorm:"not null"`
	UnitPrice     float64   `json:"unit_price" gorm:"not null"`
	LineTotal     float64   `json:"line_total" gorm:"not null"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ProductSync represents product synchronization status
type ProductSync struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	PlatformID     string    `json:"platform_id" gorm:"not null"`
	LocalProductID string    `json:"local_product_id" gorm:"not null"`
	ExternalProductID string `json:"external_product_id" gorm:"not null"`
	SyncStatus     string    `json:"sync_status" gorm:"default:'pending'"` // pending, synced, failed
	LastSyncAt     *time.Time `json:"last_sync_at"`
	ErrorMessage   string    `json:"error_message"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// InventorySync represents inventory level synchronization
type InventorySync struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	PlatformID     string    `json:"platform_id" gorm:"not null"`
	ProductSyncID  string    `json:"product_sync_id" gorm:"not null"`
	LocalQuantity  int       `json:"local_quantity" gorm:"not null"`
	RemoteQuantity int       `json:"remote_quantity" gorm:"not null"`
	SyncDirection  string    `json:"sync_direction" gorm:"not null"` // to_platform, from_platform, bidirectional
	SyncStatus     string    `json:"sync_status" gorm:"default:'pending'"` // pending, synced, failed
	LastSyncAt     *time.Time `json:"last_sync_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CustomerSync represents customer data synchronization
type CustomerSync struct {
	ID               string    `json:"id" gorm:"primaryKey"`
	OrganizationID   string    `json:"organization_id" gorm:"not null"`
	PlatformID       string    `json:"platform_id" gorm:"not null"`
	LocalCustomerID  string    `json:"local_customer_id" gorm:"not null"`
	ExternalCustomerID string  `json:"external_customer_id" gorm:"not null"`
	SyncStatus       string    `json:"sync_status" gorm:"default:'pending'"` // pending, synced, failed
	LastSyncAt       *time.Time `json:"last_sync_at"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// SyncLog represents synchronization activity logs
type SyncLog struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	PlatformID     string    `json:"platform_id" gorm:"not null"`
	SyncType       string    `json:"sync_type" gorm:"not null"` // products, inventory, orders, customers
	Action         string    `json:"action" gorm:"not null"` // create, update, delete
	Status         string    `json:"status" gorm:"not null"` // success, failed, partial
	RecordsProcessed int     `json:"records_processed" gorm:"default:0"`
	RecordsSuccess   int     `json:"records_success" gorm:"default:0"`
	RecordsFailed    int     `json:"records_failed" gorm:"default:0"`
	ErrorMessage     string  `json:"error_message"`
	StartedAt        time.Time `json:"started_at"`
	CompletedAt      *time.Time `json:"completed_at"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Dashboard represents e-commerce integration dashboard metrics
type Dashboard struct {
	ConnectedPlatforms int64   `json:"connected_platforms"`
	TotalOnlineOrders  int64   `json:"total_online_orders"`
	SyncedProducts     int64   `json:"synced_products"`
	PendingSyncs       int64   `json:"pending_syncs"`
	OnlineRevenue      float64 `json:"online_revenue"`
	SyncSuccessRate    float64 `json:"sync_success_rate"`
}