// Package hr is a small, real HR & payroll module: employees, departments,
// leave requests, timesheets and simple pay runs. Everything is scoped by
// organisation (org_id from the JWT).
package hr

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Employment types.
const (
	EmpFullTime   = "full_time"
	EmpPartTime   = "part_time"
	EmpCasual     = "casual"
	EmpContractor = "contractor"
)

// Pay types.
const (
	PayHourly = "hourly"
	PaySalary = "salary"
)

// Pay frequencies.
const (
	FreqWeekly      = "weekly"
	FreqFortnightly = "fortnightly"
	FreqMonthly     = "monthly"
)

// Department groups employees.
type Department struct {
	ID          string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID       string    `json:"org_id" gorm:"type:varchar(255);not null;index"`
	Name        string    `json:"name" gorm:"type:varchar(120);not null"`
	Description string    `json:"description" gorm:"type:text"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	EmployeeCount int64 `json:"employee_count" gorm:"-"`
}

func (Department) TableName() string { return "hr_departments" }

// Employee is a person on the payroll. UserID optionally links to an app user.
type Employee struct {
	ID             string         `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID          string         `json:"org_id" gorm:"type:varchar(255);not null;index"`
	UserID         *string        `json:"user_id" gorm:"type:varchar(255);index"`
	FirstName      string         `json:"first_name" gorm:"type:varchar(100);not null"`
	LastName       string         `json:"last_name" gorm:"type:varchar(100)"`
	Email          string         `json:"email" gorm:"type:varchar(255)"`
	Phone          string         `json:"phone" gorm:"type:varchar(40)"`
	Position       string         `json:"position" gorm:"type:varchar(120)"`
	DepartmentID   *string        `json:"department_id" gorm:"type:varchar(64);index"`
	EmploymentType string         `json:"employment_type" gorm:"type:varchar(20);not null;default:'full_time'"`
	StartDate      string         `json:"start_date" gorm:"type:varchar(10)"`
	PayType        string         `json:"pay_type" gorm:"type:varchar(10);not null;default:'hourly'"`
	PayRate        float64        `json:"pay_rate" gorm:"type:decimal(14,4);not null;default:0"` // hourly rate, or annual salary
	Currency       string         `json:"currency" gorm:"type:varchar(3);not null;default:'AUD'"`
	Status         string         `json:"status" gorm:"type:varchar(20);not null;default:'active';index"`
	Notes          string         `json:"notes" gorm:"type:text"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	DepartmentName string `json:"department_name" gorm:"-"`
	FullName       string `json:"full_name" gorm:"-"`
}

func (Employee) TableName() string { return "hr_employees" }

// LeaveRequest is a request for time off.
type LeaveRequest struct {
	ID         string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID      string     `json:"org_id" gorm:"type:varchar(255);not null;index"`
	EmployeeID string     `json:"employee_id" gorm:"type:varchar(64);not null;index"`
	Type       string     `json:"type" gorm:"type:varchar(20);not null"`
	StartDate  string     `json:"start_date" gorm:"type:varchar(10);not null"`
	EndDate    string     `json:"end_date" gorm:"type:varchar(10);not null"`
	Days       float64    `json:"days" gorm:"type:decimal(6,2);not null;default:0"`
	Reason     string     `json:"reason" gorm:"type:text"`
	Status     string     `json:"status" gorm:"type:varchar(20);not null;default:'pending';index"`
	DecidedBy  string     `json:"decided_by" gorm:"type:varchar(255)"`
	DecidedAt  *time.Time `json:"decided_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`

	EmployeeName string `json:"employee_name" gorm:"-"`
}

func (LeaveRequest) TableName() string { return "hr_leave_requests" }

// Timesheet is one day's worked hours for an employee.
type Timesheet struct {
	ID         string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID      string     `json:"org_id" gorm:"type:varchar(255);not null;index"`
	EmployeeID string     `json:"employee_id" gorm:"type:varchar(64);not null;index"`
	Date       string     `json:"date" gorm:"type:varchar(10);not null;index"`
	Hours      float64    `json:"hours" gorm:"type:decimal(6,2);not null"`
	Notes      string     `json:"notes" gorm:"type:text"`
	Status     string     `json:"status" gorm:"type:varchar(20);not null;default:'submitted';index"`
	ApprovedBy string     `json:"approved_by" gorm:"type:varchar(255)"`
	ApprovedAt *time.Time `json:"approved_at"`
	PayRunID   *string    `json:"pay_run_id" gorm:"type:varchar(64);index"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`

	EmployeeName string `json:"employee_name" gorm:"-"`
}

func (Timesheet) TableName() string { return "hr_timesheets" }

// PayRun is a payroll for one period.
type PayRun struct {
	ID              string       `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID           string       `json:"org_id" gorm:"type:varchar(255);not null;index"`
	Number          string       `json:"number" gorm:"type:varchar(30);not null"`
	Frequency       string       `json:"frequency" gorm:"type:varchar(20);not null"`
	PeriodStart     string       `json:"period_start" gorm:"type:varchar(10);not null"`
	PeriodEnd       string       `json:"period_end" gorm:"type:varchar(10);not null"`
	PayDate         string       `json:"pay_date" gorm:"type:varchar(10)"`
	Currency        string       `json:"currency" gorm:"type:varchar(3);not null"`
	DeductionRate   float64      `json:"deduction_rate" gorm:"type:decimal(6,3);not null;default:0"`
	Status          string       `json:"status" gorm:"type:varchar(20);not null;default:'draft';index"`
	TotalGross      float64      `json:"total_gross" gorm:"type:decimal(14,2);not null;default:0"`
	TotalDeductions float64      `json:"total_deductions" gorm:"type:decimal(14,2);not null;default:0"`
	TotalNet        float64      `json:"total_net" gorm:"type:decimal(14,2);not null;default:0"`
	EmployeeCount   int          `json:"employee_count" gorm:"not null;default:0"`
	Notes           string       `json:"notes" gorm:"type:text"`
	PaidAt          *time.Time   `json:"paid_at"`
	CreatedBy       string       `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	Lines           []PayRunLine `json:"lines,omitempty" gorm:"foreignKey:PayRunID;constraint:OnDelete:CASCADE"`
}

func (PayRun) TableName() string { return "hr_pay_runs" }

// PayRunLine is one employee's pay in a pay run.
type PayRunLine struct {
	ID           string  `json:"id" gorm:"type:varchar(64);primaryKey"`
	PayRunID     string  `json:"pay_run_id" gorm:"type:varchar(64);not null;index"`
	EmployeeID   string  `json:"employee_id" gorm:"type:varchar(64);not null;index"`
	EmployeeName string  `json:"employee_name" gorm:"type:varchar(220)"`
	Position     string  `json:"position" gorm:"type:varchar(120)"`
	PayType      string  `json:"pay_type" gorm:"type:varchar(10)"`
	Hours        float64 `json:"hours" gorm:"type:decimal(10,2);not null;default:0"`
	Rate         float64 `json:"rate" gorm:"type:decimal(14,4);not null;default:0"`
	Gross        float64 `json:"gross" gorm:"type:decimal(14,2);not null;default:0"`
	Deductions   float64 `json:"deductions" gorm:"type:decimal(14,2);not null;default:0"`
	Net          float64 `json:"net" gorm:"type:decimal(14,2);not null;default:0"`
}

func (PayRunLine) TableName() string { return "hr_pay_run_lines" }

func newID() string { return uuid.New().String() }

func (d *Department) BeforeCreate(*gorm.DB) error {
	if d.ID == "" {
		d.ID = newID()
	}
	return nil
}
func (e *Employee) BeforeCreate(*gorm.DB) error {
	if e.ID == "" {
		e.ID = newID()
	}
	return nil
}
func (l *LeaveRequest) BeforeCreate(*gorm.DB) error {
	if l.ID == "" {
		l.ID = newID()
	}
	return nil
}
func (t *Timesheet) BeforeCreate(*gorm.DB) error {
	if t.ID == "" {
		t.ID = newID()
	}
	return nil
}
func (p *PayRun) BeforeCreate(*gorm.DB) error {
	if p.ID == "" {
		p.ID = newID()
	}
	return nil
}
func (l *PayRunLine) BeforeCreate(*gorm.DB) error {
	if l.ID == "" {
		l.ID = newID()
	}
	return nil
}

// Migrate creates / updates the HR tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&Department{}, &Employee{}, &LeaveRequest{}, &Timesheet{}, &PayRun{}, &PayRunLine{})
}
