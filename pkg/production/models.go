package production

import (
	"time"
)

// BillOfMaterials represents product recipe/composition
type BillOfMaterials struct {
	ID             string      `json:"id" gorm:"primaryKey"`
	OrganizationID string      `json:"organization_id" gorm:"not null"`
	ProductID      string      `json:"product_id" gorm:"not null"`
	Name           string      `json:"name" gorm:"not null"`
	Version        string      `json:"version" gorm:"default:'1.0'"`
	IsActive       bool        `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	Components     []BOMComponent `json:"components" gorm:"foreignKey:BOMID"`
}

// BOMComponent represents components in BOM
type BOMComponent struct {
	ID            string    `json:"id" gorm:"primaryKey"`
	BOMID         string    `json:"bom_id" gorm:"not null"`
	ComponentID   string    `json:"component_id" gorm:"not null"` // Product ID
	Quantity      float64   `json:"quantity" gorm:"not null"`
	Unit          string    `json:"unit" gorm:"not null"`
	CostPerUnit   float64   `json:"cost_per_unit"`
	Notes         string    `json:"notes"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// WorkOrder represents production work orders
type WorkOrder struct {
	ID               string              `json:"id" gorm:"primaryKey"`
	OrganizationID   string              `json:"organization_id" gorm:"not null"`
	WorkOrderNumber  string              `json:"work_order_number" gorm:"unique;not null"`
	BOMID            string              `json:"bom_id" gorm:"not null"`
	ProductID        string              `json:"product_id" gorm:"not null"`
	QuantityPlanned  int                 `json:"quantity_planned" gorm:"not null"`
	QuantityProduced int                 `json:"quantity_produced" gorm:"default:0"`
	Status           string              `json:"status" gorm:"default:'planned'"` // planned, in_progress, completed, cancelled
	Priority         string              `json:"priority" gorm:"default:'medium'"` // low, medium, high, urgent
	PlannedStartDate time.Time           `json:"planned_start_date"`
	PlannedEndDate   time.Time           `json:"planned_end_date"`
	ActualStartDate  *time.Time          `json:"actual_start_date"`
	ActualEndDate    *time.Time          `json:"actual_end_date"`
	Notes            string              `json:"notes"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
	Operations       []WorkOrderOperation `json:"operations" gorm:"foreignKey:WorkOrderID"`
}

// WorkOrderOperation represents individual operations in work order
type WorkOrderOperation struct {
	ID                string     `json:"id" gorm:"primaryKey"`
	WorkOrderID       string     `json:"work_order_id" gorm:"not null"`
	OperationName     string     `json:"operation_name" gorm:"not null"`
	Sequence          int        `json:"sequence" gorm:"not null"`
	MachineID         *string    `json:"machine_id"`
	EstimatedDuration int        `json:"estimated_duration"` // minutes
	ActualDuration    int        `json:"actual_duration"`    // minutes
	Status            string     `json:"status" gorm:"default:'pending'"` // pending, in_progress, completed
	StartedAt         *time.Time `json:"started_at"`
	CompletedAt       *time.Time `json:"completed_at"`
	Notes             string     `json:"notes"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Machine represents production equipment
type Machine struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Name           string    `json:"name" gorm:"not null"`
	Code           string    `json:"code" gorm:"unique;not null"`
	Type           string    `json:"type"`
	Location       string    `json:"location"`
	Status         string    `json:"status" gorm:"default:'available'"` // available, in_use, maintenance, offline
	HourlyRate     float64   `json:"hourly_rate" gorm:"default:0"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// QualityCheck represents quality control checkpoints
type QualityCheck struct {
	ID               string    `json:"id" gorm:"primaryKey"`
	WorkOrderID      string    `json:"work_order_id" gorm:"not null"`
	CheckpointName   string    `json:"checkpoint_name" gorm:"not null"`
	CheckedQuantity  int       `json:"checked_quantity" gorm:"not null"`
	PassedQuantity   int       `json:"passed_quantity" gorm:"not null"`
	FailedQuantity   int       `json:"failed_quantity" gorm:"not null"`
	CheckedBy        string    `json:"checked_by"` // User ID
	CheckDate        time.Time `json:"check_date" gorm:"not null"`
	Notes            string    `json:"notes"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Dashboard represents production dashboard metrics
type Dashboard struct {
	ActiveWorkOrders    int64   `json:"active_work_orders"`
	CompletedWorkOrders int64   `json:"completed_work_orders"`
	MachinesInUse       int64   `json:"machines_in_use"`
	TotalMachines       int64   `json:"total_machines"`
	ProductionOutput    int64   `json:"production_output"`
	QualityRate         float64 `json:"quality_rate"`
}