package projects

import (
	"time"
)

// Project represents client projects
type Project struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Name           string    `json:"name" gorm:"not null"`
	Description    string    `json:"description"`
	CustomerID     string    `json:"customer_id" gorm:"not null"`
	ProjectManager string    `json:"project_manager"` // User ID
	Status         string    `json:"status" gorm:"default:'planning'"` // planning, active, on_hold, completed, cancelled
	Priority       string    `json:"priority" gorm:"default:'medium'"` // low, medium, high, urgent
	StartDate      time.Time `json:"start_date"`
	EndDate        time.Time `json:"end_date"`
	ActualEndDate  *time.Time `json:"actual_end_date"`
	Budget         float64   `json:"budget" gorm:"default:0"`
	ActualCost     float64   `json:"actual_cost" gorm:"default:0"`
	BillableHours  float64   `json:"billable_hours" gorm:"default:0"`
	NonBillableHours float64 `json:"non_billable_hours" gorm:"default:0"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Tasks          []Task    `json:"tasks" gorm:"foreignKey:ProjectID"`
	TimeEntries    []TimeEntry `json:"time_entries" gorm:"foreignKey:ProjectID"`
	Milestones     []Milestone `json:"milestones" gorm:"foreignKey:ProjectID"`
}

// Task represents project tasks
type Task struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	ProjectID      string    `json:"project_id" gorm:"not null"`
	Name           string    `json:"name" gorm:"not null"`
	Description    string    `json:"description"`
	AssignedTo     *string   `json:"assigned_to"` // User ID
	Status         string    `json:"status" gorm:"default:'todo'"` // todo, in_progress, completed, blocked
	Priority       string    `json:"priority" gorm:"default:'medium'"` // low, medium, high, urgent
	EstimatedHours float64   `json:"estimated_hours" gorm:"default:0"`
	ActualHours    float64   `json:"actual_hours" gorm:"default:0"`
	StartDate      *time.Time `json:"start_date"`
	DueDate        *time.Time `json:"due_date"`
	CompletedDate  *time.Time `json:"completed_date"`
	ParentTaskID   *string   `json:"parent_task_id"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	TimeEntries    []TimeEntry `json:"time_entries" gorm:"foreignKey:TaskID"`
}

// TimeEntry represents time tracking for projects/tasks
type TimeEntry struct {
	ID          string    `json:"id" gorm:"primaryKey"`
	ProjectID   string    `json:"project_id" gorm:"not null"`
	TaskID      *string   `json:"task_id"`
	UserID      string    `json:"user_id" gorm:"not null"`
	Date        time.Time `json:"date" gorm:"not null"`
	Hours       float64   `json:"hours" gorm:"not null"`
	Description string    `json:"description"`
	IsBillable  bool      `json:"is_billable" gorm:"default:true"`
	HourlyRate  float64   `json:"hourly_rate" gorm:"default:0"`
	Amount      float64   `json:"amount" gorm:"default:0"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Milestone represents project milestones
type Milestone struct {
	ID          string    `json:"id" gorm:"primaryKey"`
	ProjectID   string    `json:"project_id" gorm:"not null"`
	Name        string    `json:"name" gorm:"not null"`
	Description string    `json:"description"`
	DueDate     time.Time `json:"due_date" gorm:"not null"`
	Status      string    `json:"status" gorm:"default:'pending'"` // pending, completed, overdue
	CompletedDate *time.Time `json:"completed_date"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Resource represents project resources (team members)
type Resource struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	UserID         string    `json:"user_id" gorm:"not null"`
	Role           string    `json:"role" gorm:"not null"`
	HourlyRate     float64   `json:"hourly_rate" gorm:"default:0"`
	Capacity       float64   `json:"capacity" gorm:"default:40"` // hours per week
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ProjectResource represents resource allocation to projects
type ProjectResource struct {
	ID           string    `json:"id" gorm:"primaryKey"`
	ProjectID    string    `json:"project_id" gorm:"not null"`
	ResourceID   string    `json:"resource_id" gorm:"not null"`
	Allocation   float64   `json:"allocation" gorm:"default:100"` // percentage
	StartDate    time.Time `json:"start_date"`
	EndDate      time.Time `json:"end_date"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Dashboard represents project management dashboard metrics
type Dashboard struct {
	ActiveProjects    int64   `json:"active_projects"`
	CompletedProjects int64   `json:"completed_projects"`
	OverdueProjects   int64   `json:"overdue_projects"`
	TotalBudget       float64 `json:"total_budget"`
	ActualCost        float64 `json:"actual_cost"`
	TotalBillableHours float64 `json:"total_billable_hours"`
	UtilizationRate   float64 `json:"utilization_rate"`
}