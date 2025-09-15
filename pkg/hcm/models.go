package hcm

import (
	"time"
)

// Employee represents company employees
type Employee struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	EmployeeNumber string    `json:"employee_number" gorm:"unique;not null"`
	FirstName      string    `json:"first_name" gorm:"not null"`
	LastName       string    `json:"last_name" gorm:"not null"`
	Email          string    `json:"email" gorm:"unique;not null"`
	Phone          string    `json:"phone"`
	DateOfBirth    *time.Time `json:"date_of_birth"`
	HireDate       time.Time `json:"hire_date" gorm:"not null"`
	TerminationDate *time.Time `json:"termination_date"`
	JobTitle       string    `json:"job_title" gorm:"not null"`
	Department     string    `json:"department"`
	ReportsTo      *string   `json:"reports_to"` // Manager's Employee ID
	EmploymentType string    `json:"employment_type" gorm:"not null"` // full_time, part_time, contract, intern
	Status         string    `json:"status" gorm:"default:'active'"` // active, inactive, terminated
	Salary         float64   `json:"salary" gorm:"default:0"`
	HourlyRate     float64   `json:"hourly_rate" gorm:"default:0"`
	Address        string    `json:"address"`
	EmergencyContact string  `json:"emergency_contact"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	TimeEntries    []TimeEntry `json:"time_entries" gorm:"foreignKey:EmployeeID"`
	PayrollRecords []PayrollRecord `json:"payroll_records" gorm:"foreignKey:EmployeeID"`
}

// TimeEntry represents employee time tracking
type TimeEntry struct {
	ID         string    `json:"id" gorm:"primaryKey"`
	EmployeeID string    `json:"employee_id" gorm:"not null"`
	Date       time.Time `json:"date" gorm:"not null"`
	ClockIn    time.Time `json:"clock_in"`
	ClockOut   *time.Time `json:"clock_out"`
	BreakTime  int       `json:"break_time" gorm:"default:0"` // minutes
	TotalHours float64   `json:"total_hours" gorm:"default:0"`
	RegularHours float64 `json:"regular_hours" gorm:"default:0"`
	OvertimeHours float64 `json:"overtime_hours" gorm:"default:0"`
	Location   string    `json:"location"` // GPS coordinates or office
	Notes      string    `json:"notes"`
	Status     string    `json:"status" gorm:"default:'pending'"` // pending, approved, rejected
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Leave represents employee leave requests
type Leave struct {
	ID         string    `json:"id" gorm:"primaryKey"`
	EmployeeID string    `json:"employee_id" gorm:"not null"`
	LeaveType  string    `json:"leave_type" gorm:"not null"` // vacation, sick, personal, maternity, etc.
	StartDate  time.Time `json:"start_date" gorm:"not null"`
	EndDate    time.Time `json:"end_date" gorm:"not null"`
	Days       int       `json:"days" gorm:"not null"`
	Reason     string    `json:"reason"`
	Status     string    `json:"status" gorm:"default:'pending'"` // pending, approved, rejected
	ApprovedBy *string   `json:"approved_by"` // Manager's Employee ID
	ApprovedAt *time.Time `json:"approved_at"`
	Notes      string    `json:"notes"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// PayrollPeriod represents payroll processing periods
type PayrollPeriod struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	PeriodName     string    `json:"period_name" gorm:"not null"`
	StartDate      time.Time `json:"start_date" gorm:"not null"`
	EndDate        time.Time `json:"end_date" gorm:"not null"`
	PayDate        time.Time `json:"pay_date" gorm:"not null"`
	Status         string    `json:"status" gorm:"default:'draft'"` // draft, processing, completed, paid
	TotalGrossPay  float64   `json:"total_gross_pay" gorm:"default:0"`
	TotalDeductions float64  `json:"total_deductions" gorm:"default:0"`
	TotalNetPay    float64   `json:"total_net_pay" gorm:"default:0"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	PayrollRecords []PayrollRecord `json:"payroll_records" gorm:"foreignKey:PayrollPeriodID"`
}

// PayrollRecord represents individual employee payroll records
type PayrollRecord struct {
	ID               string    `json:"id" gorm:"primaryKey"`
	PayrollPeriodID  string    `json:"payroll_period_id" gorm:"not null"`
	EmployeeID       string    `json:"employee_id" gorm:"not null"`
	RegularHours     float64   `json:"regular_hours" gorm:"default:0"`
	OvertimeHours    float64   `json:"overtime_hours" gorm:"default:0"`
	RegularPay       float64   `json:"regular_pay" gorm:"default:0"`
	OvertimePay      float64   `json:"overtime_pay" gorm:"default:0"`
	Bonus            float64   `json:"bonus" gorm:"default:0"`
	Commission       float64   `json:"commission" gorm:"default:0"`
	GrossPay         float64   `json:"gross_pay" gorm:"default:0"`
	FederalTax       float64   `json:"federal_tax" gorm:"default:0"`
	StateTax         float64   `json:"state_tax" gorm:"default:0"`
	SocialSecurity   float64   `json:"social_security" gorm:"default:0"`
	Medicare         float64   `json:"medicare" gorm:"default:0"`
	HealthInsurance  float64   `json:"health_insurance" gorm:"default:0"`
	Retirement401k   float64   `json:"retirement_401k" gorm:"default:0"`
	OtherDeductions  float64   `json:"other_deductions" gorm:"default:0"`
	TotalDeductions  float64   `json:"total_deductions" gorm:"default:0"`
	NetPay           float64   `json:"net_pay" gorm:"default:0"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Performance represents employee performance reviews
type Performance struct {
	ID          string    `json:"id" gorm:"primaryKey"`
	EmployeeID  string    `json:"employee_id" gorm:"not null"`
	ReviewerID  string    `json:"reviewer_id" gorm:"not null"` // Manager's Employee ID
	ReviewPeriod string   `json:"review_period" gorm:"not null"` // Q1 2024, Annual 2024, etc.
	ReviewDate  time.Time `json:"review_date" gorm:"not null"`
	OverallRating int     `json:"overall_rating" gorm:"not null"` // 1-5 scale
	Goals       string    `json:"goals"`
	Achievements string   `json:"achievements"`
	AreasOfImprovement string `json:"areas_of_improvement"`
	ManagerComments    string `json:"manager_comments"`
	EmployeeComments   string `json:"employee_comments"`
	Status      string    `json:"status" gorm:"default:'draft'"` // draft, completed, acknowledged
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Department represents organizational departments
type Department struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"not null"`
	Name           string    `json:"name" gorm:"not null"`
	Code           string    `json:"code" gorm:"unique;not null"`
	Description    string    `json:"description"`
	ManagerID      *string   `json:"manager_id"` // Employee ID
	Budget         float64   `json:"budget" gorm:"default:0"`
	IsActive       bool      `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Dashboard represents HCM dashboard metrics
type Dashboard struct {
	TotalEmployees     int64   `json:"total_employees"`
	ActiveEmployees    int64   `json:"active_employees"`
	NewHiresThisMonth  int64   `json:"new_hires_this_month"`
	PendingTimeEntries int64   `json:"pending_time_entries"`
	PendingLeaveRequests int64 `json:"pending_leave_requests"`
	TotalPayroll       float64 `json:"total_payroll"`
	AverageRating      float64 `json:"average_rating"`
	TurnoverRate       float64 `json:"turnover_rate"`
}