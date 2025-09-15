package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Organization represents any business organization (garage, salon, retail, etc.)
type Organization struct {
	ID          string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	Name        string         `json:"name" gorm:"type:varchar(255);not null"`
	BusinessType string        `json:"business_type" gorm:"type:varchar(100);not null;default:'general';index"` // garage, salon, retail, manufacturing, etc.
	ABN         string         `json:"abn" gorm:"type:varchar(11);unique"`
	Phone       string         `json:"phone" gorm:"type:varchar(20)"`
	Email       string         `json:"email" gorm:"type:varchar(255)"`
	Website     string         `json:"website" gorm:"type:varchar(255)"`
	Description string         `json:"description" gorm:"type:text"`
	Address     Address        `json:"address" gorm:"embedded;embeddedPrefix:address_"`
	BusinessHours BusinessHours `json:"business_hours" gorm:"embedded;embeddedPrefix:hours_"`
	BookingSettings BookingSettings `json:"booking_settings" gorm:"embedded;embeddedPrefix:booking_"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Users        []User        `json:"users,omitempty" gorm:"foreignKey:OrganizationID"`
	Customers    []Customer    `json:"customers,omitempty" gorm:"foreignKey:OrganizationID"`
	Vehicles     []Vehicle     `json:"vehicles,omitempty" gorm:"foreignKey:OrganizationID"`
	Services     []Service     `json:"services,omitempty" gorm:"foreignKey:OrganizationID"`
	Bookings     []Booking     `json:"bookings,omitempty" gorm:"foreignKey:OrganizationID"`
}

// User represents system users (staff, admins, etc.)
type User struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	Email          string         `json:"email" gorm:"type:varchar(255);uniqueIndex;not null"`
	PasswordHash   string         `json:"-" gorm:"type:varchar(255);not null"`
	FirstName      string         `json:"first_name" gorm:"type:varchar(100);not null"`
	LastName       string         `json:"last_name" gorm:"type:varchar(100);not null"`
	Phone          string         `json:"phone" gorm:"type:varchar(20)"`
	Role           string         `json:"role" gorm:"type:varchar(50);not null;index"` // admin, manager, care_worker, support_coordinator
	RoleID         *string        `json:"role_id,omitempty" gorm:"type:varchar(255);index"` // New role-based system
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	IsActive       bool           `json:"is_active" gorm:"default:true;index"`
	LastLoginAt    *time.Time     `json:"last_login_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization    Organization     `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Shifts          []Shift          `json:"shifts,omitempty" gorm:"foreignKey:StaffID"`
	UploadedDocs    []Document       `json:"uploaded_documents,omitempty" gorm:"foreignKey:UploadedBy"`
	UserPermissions []UserPermission `json:"permissions,omitempty" gorm:"foreignKey:UserID"`
	RefreshTokens   []RefreshToken   `json:"-" gorm:"foreignKey:UserID"`
}

// Customer represents business customers (vehicle owners, salon clients, etc.)
type Customer struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	FirstName      string         `json:"first_name" gorm:"type:varchar(100);not null"`
	LastName       string         `json:"last_name" gorm:"type:varchar(100);not null"`
	Email          string         `json:"email" gorm:"type:varchar(255);index"`
	Phone          string         `json:"phone" gorm:"type:varchar(20);not null"`
	DateOfBirth    *time.Time     `json:"date_of_birth,omitempty"`
	Address        Address        `json:"address" gorm:"embedded;embeddedPrefix:address_"`
	Notes          string         `json:"notes" gorm:"type:text"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	IsActive       bool           `json:"is_active" gorm:"default:true;index"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Vehicles     []Vehicle    `json:"vehicles,omitempty" gorm:"foreignKey:CustomerID"`
	Bookings     []Booking    `json:"bookings,omitempty" gorm:"foreignKey:CustomerID"`
}

// Vehicle represents vehicles for garage businesses
type Vehicle struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	CustomerID     string         `json:"customer_id" gorm:"type:varchar(255);not null;index"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Make           string         `json:"make" gorm:"type:varchar(50);not null"`
	Model          string         `json:"model" gorm:"type:varchar(50);not null"`
	Year           int            `json:"year" gorm:"not null"`
	LicensePlate   string         `json:"license_plate" gorm:"type:varchar(20);index"`
	VIN            string         `json:"vin" gorm:"type:varchar(50);unique"`
	Color          string         `json:"color" gorm:"type:varchar(30)"`
	Mileage        int            `json:"mileage" gorm:"default:0"`
	Notes          string         `json:"notes" gorm:"type:text"`
	IsActive       bool           `json:"is_active" gorm:"default:true;index"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Customer     Customer      `json:"customer,omitempty" gorm:"foreignKey:CustomerID"`
	Organization Organization  `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Bookings     []Booking     `json:"bookings,omitempty" gorm:"foreignKey:VehicleID"`
}

// Service represents services offered by the business
type Service struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Name           string         `json:"name" gorm:"type:varchar(255);not null"`
	Description    string         `json:"description" gorm:"type:text"`
	Category       string         `json:"category" gorm:"type:varchar(100);not null;index"` // maintenance, repair, beauty, etc.
	Duration       int            `json:"duration" gorm:"not null"` // Duration in minutes
	Price          float64        `json:"price" gorm:"type:decimal(10,2);not null"`
	IsActive       bool           `json:"is_active" gorm:"default:true;index"`
	RequiresVehicle bool          `json:"requires_vehicle" gorm:"default:false"` // For garage services
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Bookings     []Booking    `json:"bookings,omitempty" gorm:"many2many:booking_services"`
}

// Booking represents appointments/bookings
type Booking struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	CustomerID     string         `json:"customer_id" gorm:"type:varchar(255);not null;index"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	VehicleID      *string        `json:"vehicle_id,omitempty" gorm:"type:varchar(255);index"` // Optional for non-garage businesses
	StaffID        *string        `json:"staff_id,omitempty" gorm:"type:varchar(255);index"`
	StartTime      time.Time      `json:"start_time" gorm:"not null;index"`
	EndTime        time.Time      `json:"end_time" gorm:"not null;index"`
	Status         string         `json:"status" gorm:"type:varchar(50);default:'scheduled';index"` // scheduled, confirmed, in_progress, completed, cancelled, no_show
	TotalPrice     float64        `json:"total_price" gorm:"type:decimal(10,2);default:0"`
	Notes          string         `json:"notes" gorm:"type:text"`
	InternalNotes  string         `json:"internal_notes" gorm:"type:text"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Customer     Customer      `json:"customer,omitempty" gorm:"foreignKey:CustomerID"`
	Organization Organization  `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Vehicle      *Vehicle      `json:"vehicle,omitempty" gorm:"foreignKey:VehicleID"`
	Staff        *User         `json:"staff,omitempty" gorm:"foreignKey:StaffID"`
	Services     []Service     `json:"services,omitempty" gorm:"many2many:booking_services"`
}

// Participant represents care recipients
type Participant struct {
	ID             string             `json:"id" gorm:"type:varchar(255);primaryKey"`
	FirstName      string             `json:"first_name" gorm:"type:varchar(100);not null"`
	LastName       string             `json:"last_name" gorm:"type:varchar(100);not null"`
	DateOfBirth    time.Time          `json:"date_of_birth" gorm:"not null;index"`
	NDISNumber     string             `json:"ndis_number" gorm:"type:varchar(10);uniqueIndex"`
	Email          string             `json:"email" gorm:"type:varchar(255)"`
	Phone          string             `json:"phone" gorm:"type:varchar(20)"`
	Address        Address            `json:"address" gorm:"embedded;embeddedPrefix:address_"`
	MedicalInfo    MedicalInformation `json:"medical_information" gorm:"embedded;embeddedPrefix:medical_"`
	Funding        FundingInformation `json:"funding" gorm:"embedded;embeddedPrefix:funding_"`
	OrganizationID string             `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	IsActive       bool               `json:"is_active" gorm:"default:true;index"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
	DeletedAt      gorm.DeletedAt     `json:"-" gorm:"index"`

	// Relationships
	Organization      Organization       `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	EmergencyContacts []EmergencyContact `json:"emergency_contacts,omitempty" gorm:"foreignKey:ParticipantID"`
	Shifts            []Shift            `json:"shifts,omitempty" gorm:"foreignKey:ParticipantID"`
	Documents         []Document         `json:"documents,omitempty" gorm:"foreignKey:ParticipantID"`
	CarePlans         []CarePlan         `json:"care_plans,omitempty" gorm:"foreignKey:ParticipantID"`
}

// EmergencyContact represents participant emergency contacts
type EmergencyContact struct {
	ID            string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	ParticipantID string    `json:"participant_id" gorm:"type:varchar(255);not null;index"`
	Name          string    `json:"name" gorm:"type:varchar(200);not null"`
	Relationship  string    `json:"relationship" gorm:"type:varchar(50);not null"`
	Phone         string    `json:"phone" gorm:"type:varchar(20);not null"`
	Email         string    `json:"email" gorm:"type:varchar(255)"`
	IsPrimary     bool      `json:"is_primary" gorm:"default:false"`
	IsActive      bool      `json:"is_active" gorm:"default:true"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// Relationships
	Participant Participant `json:"participant,omitempty" gorm:"foreignKey:ParticipantID"`
}

// Shift represents scheduled work shifts
type Shift struct {
	ID              string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	ParticipantID   string         `json:"participant_id" gorm:"type:varchar(255);not null;index"`
	StaffID         string         `json:"staff_id" gorm:"type:varchar(255);not null;index"`
	StartTime       time.Time      `json:"start_time" gorm:"not null;index"`
	EndTime         time.Time      `json:"end_time" gorm:"not null;index"`
	ActualStartTime *time.Time     `json:"actual_start_time,omitempty"`
	ActualEndTime   *time.Time     `json:"actual_end_time,omitempty"`
	ServiceType     string         `json:"service_type" gorm:"type:varchar(100);not null;index"`
	Location        string         `json:"location" gorm:"type:varchar(100);not null"`
	Status          string         `json:"status" gorm:"type:varchar(50);default:'scheduled';index"` // scheduled, in_progress, completed, cancelled, no_show
	HourlyRate      float64        `json:"hourly_rate" gorm:"type:decimal(10,2);not null"`
	TotalCost       float64        `json:"total_cost" gorm:"type:decimal(10,2)"`
	Notes           string         `json:"notes" gorm:"type:text"`
	CompletionNotes string         `json:"completion_notes" gorm:"type:text"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Participant Participant `json:"participant,omitempty" gorm:"foreignKey:ParticipantID"`
	Staff       User        `json:"staff,omitempty" gorm:"foreignKey:StaffID"`
}

// Document represents uploaded files and documents
type Document struct {
	ID               string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	ParticipantID    *string        `json:"participant_id,omitempty" gorm:"type:varchar(255);index"`
	UploadedBy       string         `json:"uploaded_by" gorm:"type:varchar(255);not null;index"`
	Filename         string         `json:"filename" gorm:"type:varchar(255);not null"`
	OriginalFilename string         `json:"original_filename" gorm:"type:varchar(255);not null"`
	Title            string         `json:"title" gorm:"type:varchar(255);not null"`
	Description      string         `json:"description" gorm:"type:text"`
	Category         string         `json:"category" gorm:"type:varchar(100);not null;index"` // care_plan, medical_record, incident_report, assessment, etc.
	FileType         string         `json:"file_type" gorm:"type:varchar(100);not null"`
	FileSize         int64          `json:"file_size" gorm:"not null"`
	FilePath         string         `json:"file_path" gorm:"type:varchar(500);not null"`
	URL              string         `json:"url" gorm:"type:varchar(500)"`
	IsActive         bool           `json:"is_active" gorm:"default:true;index"`
	ExpiryDate       *time.Time     `json:"expiry_date,omitempty" gorm:"index"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	DeletedAt        gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Participant    *Participant `json:"participant,omitempty" gorm:"foreignKey:ParticipantID"`
	UploadedByUser User         `json:"uploaded_by_user,omitempty" gorm:"foreignKey:UploadedBy"`
}

// CarePlan represents participant care plans
type CarePlan struct {
	ID            string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	ParticipantID string         `json:"participant_id" gorm:"type:varchar(255);not null;index"`
	Title         string         `json:"title" gorm:"type:varchar(255);not null"`
	Description   string         `json:"description" gorm:"type:text"`
	Goals         string         `json:"goals" gorm:"type:text"` // JSON string of goals
	StartDate     time.Time      `json:"start_date" gorm:"not null"`
	EndDate       *time.Time     `json:"end_date,omitempty"`
	Status        string         `json:"status" gorm:"type:varchar(50);default:'active';index"` // active, completed, cancelled
	CreatedBy     string         `json:"created_by" gorm:"type:varchar(255);not null"`
	ApprovedBy    *string        `json:"approved_by,omitempty" gorm:"type:varchar(255)"`
	ApprovedAt    *time.Time     `json:"approved_at,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	DeletedAt     gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Participant Participant `json:"participant,omitempty" gorm:"foreignKey:ParticipantID"`
	Creator     User        `json:"creator,omitempty" gorm:"foreignKey:CreatedBy"`
	Approver    *User       `json:"approver,omitempty" gorm:"foreignKey:ApprovedBy"`
}

// RefreshToken stores JWT refresh tokens
type RefreshToken struct {
	ID        string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	UserID    string    `json:"user_id" gorm:"type:varchar(255);not null;index"`
	Token     string    `json:"token" gorm:"type:varchar(255);not null;uniqueIndex"`
	ExpiresAt time.Time `json:"expires_at" gorm:"not null;index"`
	IsRevoked bool      `json:"is_revoked" gorm:"default:false;index"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	User User `json:"user,omitempty" gorm:"foreignKey:UserID"`
}

// UserPermission represents user permissions
type UserPermission struct {
	ID         string    `json:"id" gorm:"type:varchar(255);primaryKey"`
	UserID     string    `json:"user_id" gorm:"type:varchar(255);not null;index"`
	Permission string    `json:"permission" gorm:"type:varchar(100);not null"` // read_participants, create_shifts, etc.
	CreatedAt  time.Time `json:"created_at"`

	// Relationships
	User User `json:"user,omitempty" gorm:"foreignKey:UserID"`
}

// Embedded structs for common data patterns

// Address represents physical addresses
type Address struct {
	Street   string `json:"street" gorm:"type:varchar(255)"`
	Suburb   string `json:"suburb" gorm:"type:varchar(100)"`
	State    string `json:"state" gorm:"type:varchar(50)"`
	Postcode string `json:"postcode" gorm:"type:varchar(10)"`
	Country  string `json:"country" gorm:"type:varchar(100);default:'Australia'"`
}

// NDISReg represents NDIS registration information
type NDISReg struct {
	RegistrationNumber string     `json:"registration_number" gorm:"type:varchar(50)"`
	RegistrationStatus string     `json:"registration_status" gorm:"type:varchar(50);default:'active'"`
	ExpiryDate         *time.Time `json:"expiry_date,omitempty"`
}

// MedicalInformation represents participant medical details
type MedicalInformation struct {
	Conditions  string `json:"conditions" gorm:"type:text"`  // JSON array of conditions
	Medications string `json:"medications" gorm:"type:text"` // JSON array of medications
	Allergies   string `json:"allergies" gorm:"type:text"`   // JSON array of allergies
	DoctorName  string `json:"doctor_name" gorm:"type:varchar(255)"`
	DoctorPhone string `json:"doctor_phone" gorm:"type:varchar(20)"`
	Notes       string `json:"notes" gorm:"type:text"`
}

// FundingInformation represents NDIS funding details
type FundingInformation struct {
	TotalBudget     float64    `json:"total_budget" gorm:"type:decimal(12,2);default:0"`
	UsedBudget      float64    `json:"used_budget" gorm:"type:decimal(12,2);default:0"`
	RemainingBudget float64    `json:"remaining_budget" gorm:"type:decimal(12,2);default:0"`
	BudgetYear      string     `json:"budget_year" gorm:"type:varchar(20)"` // e.g., "2025-2026"
	PlanStartDate   *time.Time `json:"plan_start_date,omitempty"`
	PlanEndDate     *time.Time `json:"plan_end_date,omitempty"`
}

// BusinessHours represents business operating hours
type BusinessHours struct {
	MondayOpen     string `json:"monday_open" gorm:"type:varchar(5)"` // e.g., "09:00"
	MondayClose    string `json:"monday_close" gorm:"type:varchar(5)"`
	TuesdayOpen    string `json:"tuesday_open" gorm:"type:varchar(5)"`
	TuesdayClose   string `json:"tuesday_close" gorm:"type:varchar(5)"`
	WednesdayOpen  string `json:"wednesday_open" gorm:"type:varchar(5)"`
	WednesdayClose string `json:"wednesday_close" gorm:"type:varchar(5)"`
	ThursdayOpen   string `json:"thursday_open" gorm:"type:varchar(5)"`
	ThursdayClose  string `json:"thursday_close" gorm:"type:varchar(5)"`
	FridayOpen     string `json:"friday_open" gorm:"type:varchar(5)"`
	FridayClose    string `json:"friday_close" gorm:"type:varchar(5)"`
	SaturdayOpen   string `json:"saturday_open" gorm:"type:varchar(5)"`
	SaturdayClose  string `json:"saturday_close" gorm:"type:varchar(5)"`
	SundayOpen     string `json:"sunday_open" gorm:"type:varchar(5)"`
	SundayClose    string `json:"sunday_close" gorm:"type:varchar(5)"`
	Timezone       string `json:"timezone" gorm:"type:varchar(50);default:'Australia/Adelaide'"`
}

// BookingSettings represents booking system configuration
type BookingSettings struct {
	EnableOnlineBooking   bool   `json:"enable_online_booking" gorm:"default:true"`
	BookingWindow         int    `json:"booking_window" gorm:"default:30"` // Days in advance customers can book
	MinAdvanceBooking     int    `json:"min_advance_booking" gorm:"default:1"` // Hours in advance required
	MaxAdvanceBooking     int    `json:"max_advance_booking" gorm:"default:720"` // Hours in advance allowed (30 days)
	DefaultSlotDuration   int    `json:"default_slot_duration" gorm:"default:60"` // Minutes
	BufferTime           int    `json:"buffer_time" gorm:"default:15"` // Minutes between bookings
	RequireApproval      bool   `json:"require_approval" gorm:"default:false"`
	SendConfirmations    bool   `json:"send_confirmations" gorm:"default:true"`
	SendReminders        bool   `json:"send_reminders" gorm:"default:true"`
	ReminderHours        int    `json:"reminder_hours" gorm:"default:24"` // Hours before appointment
	AllowCancellation    bool   `json:"allow_cancellation" gorm:"default:true"`
	CancellationWindow   int    `json:"cancellation_window" gorm:"default:24"` // Hours before appointment
}

// BeforeCreate hooks for generating UUIDs
func (o *Organization) BeforeCreate(tx *gorm.DB) (err error) {
	if o.ID == "" {
		o.ID = uuid.New().String()
	}
	return
}

func (u *User) BeforeCreate(tx *gorm.DB) (err error) {
	if u.ID == "" {
		u.ID = uuid.New().String()
	}
	return
}

func (p *Participant) BeforeCreate(tx *gorm.DB) (err error) {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	return
}

func (e *EmergencyContact) BeforeCreate(tx *gorm.DB) (err error) {
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	return
}

func (s *Shift) BeforeCreate(tx *gorm.DB) (err error) {
	if s.ID == "" {
		s.ID = uuid.New().String()
	}
	// Calculate total cost based on duration and hourly rate
	duration := s.EndTime.Sub(s.StartTime).Hours()
	s.TotalCost = duration * s.HourlyRate
	return
}

func (d *Document) BeforeCreate(tx *gorm.DB) (err error) {
	if d.ID == "" {
		d.ID = uuid.New().String()
	}
	return
}

func (c *CarePlan) BeforeCreate(tx *gorm.DB) (err error) {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	return
}

func (r *RefreshToken) BeforeCreate(tx *gorm.DB) (err error) {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	return
}

func (p *UserPermission) BeforeCreate(tx *gorm.DB) (err error) {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	return
}

func (c *Customer) BeforeCreate(tx *gorm.DB) (err error) {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	return
}

func (v *Vehicle) BeforeCreate(tx *gorm.DB) (err error) {
	if v.ID == "" {
		v.ID = uuid.New().String()
	}
	return
}

func (s *Service) BeforeCreate(tx *gorm.DB) (err error) {
	if s.ID == "" {
		s.ID = uuid.New().String()
	}
	return
}

func (b *Booking) BeforeCreate(tx *gorm.DB) (err error) {
	if b.ID == "" {
		b.ID = uuid.New().String()
	}
	// Calculate total price based on services
	var totalPrice float64
	for _, service := range b.Services {
		totalPrice += service.Price
	}
	b.TotalPrice = totalPrice
	return
}

// BeforeUpdate hooks for maintaining data consistency
func (s *Shift) BeforeUpdate(tx *gorm.DB) (err error) {
	// Recalculate total cost if times have changed
	if s.EndTime.After(s.StartTime) {
		duration := s.EndTime.Sub(s.StartTime).Hours()
		s.TotalCost = duration * s.HourlyRate
	}
	return
}

func (p *Participant) BeforeUpdate(tx *gorm.DB) (err error) {
	// Recalculate remaining budget
	p.Funding.RemainingBudget = p.Funding.TotalBudget - p.Funding.UsedBudget
	return
}

// Database migration function
func MigrateDB(db *gorm.DB) error {
	// Handle custom migrations manually
	if err := handleCustomMigrations(db); err != nil {
		return err
	}

	return db.AutoMigrate(
		&Organization{},
		&User{},
		&Customer{},
		&Vehicle{},
		&Service{},
		&Booking{},
		&Participant{},
		&EmergencyContact{},
		&Shift{},
		&Document{},
		&CarePlan{},
		&RefreshToken{},
		&UserPermission{},
		// ERP Models
		&OrganizationModules{},
		&Product{},
		&ProductCategory{},
		&Brand{},
		&Supplier{},
		&PurchaseOrder{},
		&PurchaseOrderItem{},
		&InventoryItem{},
		&InventoryLocation{},
		&InventoryMovement{},
		// POS Models
		&POSTransaction{},
		&POSItem{},
		&POSPayment{},
		&CashDrawer{},
		&Discount{},
		&TaxRate{},
		&LaybyPayment{},
		&LaybyItem{},
		&LaybyPaymentEntry{},
	)
}

// Handle custom migrations
func handleCustomMigrations(db *gorm.DB) error {
	// Handle organizations table
	if db.Migrator().HasTable(&Organization{}) {
		// Handle business_type column
		if !db.Migrator().HasColumn(&Organization{}, "business_type") {
			if err := db.Exec("ALTER TABLE organizations ADD COLUMN business_type varchar(100) DEFAULT 'general'").Error; err != nil {
				return err
			}
			if err := db.Exec("UPDATE organizations SET business_type = 'general' WHERE business_type IS NULL").Error; err != nil {
				return err
			}
			if err := db.Exec("ALTER TABLE organizations ALTER COLUMN business_type SET NOT NULL").Error; err != nil {
				return err
			}
		}
	}
	
	// Handle users table
	if db.Migrator().HasTable(&User{}) {
		// Handle password_hash column
		if !db.Migrator().HasColumn(&User{}, "password_hash") {
			if err := db.Exec("ALTER TABLE users ADD COLUMN password_hash varchar(255) DEFAULT ''").Error; err != nil {
				return err
			}
			if err := db.Exec("UPDATE users SET password_hash = '' WHERE password_hash IS NULL").Error; err != nil {
				return err
			}
			if err := db.Exec("ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL").Error; err != nil {
				return err
			}
		}
		
		// Handle first_name column
		if !db.Migrator().HasColumn(&User{}, "first_name") {
			if err := db.Exec("ALTER TABLE users ADD COLUMN first_name varchar(100) DEFAULT ''").Error; err != nil {
				return err
			}
			if err := db.Exec("UPDATE users SET first_name = '' WHERE first_name IS NULL").Error; err != nil {
				return err
			}
			if err := db.Exec("ALTER TABLE users ALTER COLUMN first_name SET NOT NULL").Error; err != nil {
				return err
			}
		}
		
		// Handle last_name column
		if !db.Migrator().HasColumn(&User{}, "last_name") {
			if err := db.Exec("ALTER TABLE users ADD COLUMN last_name varchar(100) DEFAULT ''").Error; err != nil {
				return err
			}
			if err := db.Exec("UPDATE users SET last_name = '' WHERE last_name IS NULL").Error; err != nil {
				return err
			}
			if err := db.Exec("ALTER TABLE users ALTER COLUMN last_name SET NOT NULL").Error; err != nil {
				return err
			}
		}
		
		// Handle role column
		if !db.Migrator().HasColumn(&User{}, "role") {
			if err := db.Exec("ALTER TABLE users ADD COLUMN role varchar(50) DEFAULT 'user'").Error; err != nil {
				return err
			}
			if err := db.Exec("UPDATE users SET role = 'user' WHERE role IS NULL").Error; err != nil {
				return err
			}
			if err := db.Exec("ALTER TABLE users ALTER COLUMN role SET NOT NULL").Error; err != nil {
				return err
			}
		}
		
		// Handle organization_id column
		if !db.Migrator().HasColumn(&User{}, "organization_id") {
			if err := db.Exec("ALTER TABLE users ADD COLUMN organization_id varchar(255) DEFAULT ''").Error; err != nil {
				return err
			}
			if err := db.Exec("UPDATE users SET organization_id = 'org_default' WHERE organization_id IS NULL OR organization_id = ''").Error; err != nil {
				return err
			}
			if err := db.Exec("ALTER TABLE users ALTER COLUMN organization_id SET NOT NULL").Error; err != nil {
				return err
			}
		}
	}
	
	return nil
}

// Index creation function for better performance
func CreateIndexes(db *gorm.DB) error {
	// Composite indexes for better query performance
	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_shifts_participant_date ON shifts(participant_id, start_time)").Error; err != nil {
		return err
	}

	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_shifts_staff_date ON shifts(staff_id, start_time)").Error; err != nil {
		return err
	}

	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_documents_participant_category ON documents(participant_id, category)").Error; err != nil {
		return err
	}

	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_participants_ndis_org ON participants(ndis_number, organization_id)").Error; err != nil {
		return err
	}

	return nil
}

// Sample data seeding function (for development/testing)
func SeedDatabase(db *gorm.DB) error {
	// Create sample garage organization
	garageOrg := Organization{
		ID:           "org_garage",
		Name:         "DASYIN Auto Service Center",
		BusinessType: "garage",
		ABN:          "12345678901",
		Phone:        "+61887654321",
		Email:        "info@dasyingarage.com.au",
		Website:      "https://dasyingarage.com.au",
		Description:  "Full-service automotive repair and maintenance center",
		Address: Address{
			Street:   "123 Auto Service St",
			Suburb:   "Adelaide",
			State:    "SA",
			Postcode: "5000",
			Country:  "Australia",
		},
		BusinessHours: BusinessHours{
			MondayOpen:    "08:00",
			MondayClose:   "17:00",
			TuesdayOpen:   "08:00",
			TuesdayClose:  "17:00",
			WednesdayOpen: "08:00",
			WednesdayClose: "17:00",
			ThursdayOpen:  "08:00",
			ThursdayClose: "17:00",
			FridayOpen:    "08:00",
			FridayClose:   "17:00",
			SaturdayOpen:  "09:00",
			SaturdayClose: "14:00",
			Timezone:      "Australia/Adelaide",
		},
		BookingSettings: BookingSettings{
			EnableOnlineBooking: true,
			BookingWindow:       30,
			MinAdvanceBooking:   2,
			MaxAdvanceBooking:   720,
			DefaultSlotDuration: 60,
			BufferTime:         15,
			RequireApproval:    false,
			SendConfirmations:  true,
			SendReminders:      true,
			ReminderHours:      24,
			AllowCancellation:  true,
			CancellationWindow: 24,
		},
	}

	if err := db.FirstOrCreate(&garageOrg, "id = ?", garageOrg.ID).Error; err != nil {
		return err
	}

	// Create sample salon organization
	salonOrg := Organization{
		ID:           "org_salon",
		Name:         "DASYIN Beauty Salon",
		BusinessType: "salon",
		ABN:          "98765432109",
		Phone:        "+61887654322",
		Email:        "info@dasyinsalon.com.au",
		Website:      "https://dasyinsalon.com.au",
		Description:  "Premium beauty salon offering hair, nail, and skin treatments",
		Address: Address{
			Street:   "456 Beauty Blvd",
			Suburb:   "Adelaide",
			State:    "SA",
			Postcode: "5001",
			Country:  "Australia",
		},
		BusinessHours: BusinessHours{
			TuesdayOpen:    "09:00",
			TuesdayClose:   "18:00",
			WednesdayOpen:  "09:00",
			WednesdayClose: "18:00",
			ThursdayOpen:   "09:00",
			ThursdayClose:  "20:00",
			FridayOpen:     "09:00",
			FridayClose:    "18:00",
			SaturdayOpen:   "08:00",
			SaturdayClose:  "16:00",
			Timezone:       "Australia/Adelaide",
		},
		BookingSettings: BookingSettings{
			EnableOnlineBooking: true,
			BookingWindow:       60,
			MinAdvanceBooking:   4,
			MaxAdvanceBooking:   1440,
			DefaultSlotDuration: 30,
			BufferTime:         10,
			RequireApproval:    true,
			SendConfirmations:  true,
			SendReminders:      true,
			ReminderHours:      48,
			AllowCancellation:  true,
			CancellationWindow: 48,
		},
	}

	if err := db.FirstOrCreate(&salonOrg, "id = ?", salonOrg.ID).Error; err != nil {
		return err
	}

	// Create default healthcare organization (legacy)
	healthcareOrg := Organization{
		ID:           "org_healthcare",
		Name:         "DASYIN - ADL Services",
		BusinessType: "healthcare",
		ABN:          "12345678902",
		Phone:        "+61887654323",
		Email:        "info@dasyin.com.au",
		Website:      "https://dasyin.com.au",
		Description:  "Healthcare and disability support services",
		Address: Address{
			Street:   "789 Health Ave",
			Suburb:   "Adelaide",
			State:    "SA",
			Postcode: "5000",
			Country:  "Australia",
		},
	}

	if err := db.FirstOrCreate(&healthcareOrg, "id = ?", healthcareOrg.ID).Error; err != nil {
		return err
	}

	// Create admin users for each organization
	garageAdmin := User{
		ID:             "user_garage_admin",
		Email:          "admin@dasyingarage.com.au",
		FirstName:      "Ken",
		LastName:       "Kinoti",
		Role:           "admin",
		OrganizationID: garageOrg.ID,
		IsActive:       true,
		PasswordHash:   "$2a$10$n0NvhICRgFPZq/EaeWxW6un3Xrym3.23GJpk4wYchZmpxgETxQani", // "Test123!@#"
	}

	salonAdmin := User{
		ID:             "user_salon_admin",
		Email:          "admin@dasyinsalon.com.au",
		FirstName:      "Sarah",
		LastName:       "Johnson",
		Role:           "admin",
		OrganizationID: salonOrg.ID,
		IsActive:       true,
		PasswordHash:   "$2a$10$n0NvhICRgFPZq/EaeWxW6un3Xrym3.23GJpk4wYchZmpxgETxQani", // "Test123!@#"
	}

	healthcareAdmin := User{
		ID:             "user_healthcare_admin",
		Email:          "kennedy@dasyin.com.au",
		FirstName:      "Kennedy",
		LastName:       "Kinoti",
		Role:           "super_admin",
		OrganizationID: healthcareOrg.ID,
		IsActive:       true,
		PasswordHash:   "$2a$10$n0NvhICRgFPZq/EaeWxW6un3Xrym3.23GJpk4wYchZmpxgETxQani", // "Test123!@#"
	}

	// Create admin users
	admins := []User{garageAdmin, salonAdmin, healthcareAdmin}
	for _, admin := range admins {
		var existingUser User
		err := db.Where("email = ?", admin.Email).First(&existingUser).Error
		if err != nil {
			// User doesn't exist, create it
			if err := db.Create(&admin).Error; err != nil {
				return err
			}
		} else {
			// User exists, update to ensure correct details
			if err := db.Model(&existingUser).Updates(map[string]interface{}{
				"password_hash":   admin.PasswordHash,
				"role":            admin.Role,
				"is_active":       true,
				"first_name":      admin.FirstName,
				"last_name":       admin.LastName,
				"organization_id": admin.OrganizationID,
			}).Error; err != nil {
				return err
			}
		}
	}

	// Add sample garage services
	garageServices := []Service{
		{ID: "service_oil_change", OrganizationID: garageOrg.ID, Name: "Oil Change", Description: "Full synthetic oil change with filter replacement", Category: "maintenance", Duration: 30, Price: 85.00, RequiresVehicle: true, IsActive: true},
		{ID: "service_brake_service", OrganizationID: garageOrg.ID, Name: "Brake Service", Description: "Complete brake inspection and service", Category: "repair", Duration: 90, Price: 180.00, RequiresVehicle: true, IsActive: true},
		{ID: "service_tire_rotation", OrganizationID: garageOrg.ID, Name: "Tire Rotation", Description: "Professional tire rotation and balancing", Category: "maintenance", Duration: 45, Price: 65.00, RequiresVehicle: true, IsActive: true},
		{ID: "service_engine_diagnostic", OrganizationID: garageOrg.ID, Name: "Engine Diagnostic", Description: "Comprehensive engine diagnostic scan", Category: "diagnostic", Duration: 60, Price: 120.00, RequiresVehicle: true, IsActive: true},
	}

	// Add sample salon services
	salonServices := []Service{
		{ID: "service_haircut", OrganizationID: salonOrg.ID, Name: "Hair Cut & Style", Description: "Professional haircut and styling", Category: "hair", Duration: 45, Price: 65.00, RequiresVehicle: false, IsActive: true},
		{ID: "service_hair_color", OrganizationID: salonOrg.ID, Name: "Hair Coloring", Description: "Full hair coloring service", Category: "hair", Duration: 120, Price: 150.00, RequiresVehicle: false, IsActive: true},
		{ID: "service_manicure", OrganizationID: salonOrg.ID, Name: "Manicure", Description: "Professional manicure service", Category: "nails", Duration: 30, Price: 35.00, RequiresVehicle: false, IsActive: true},
		{ID: "service_facial", OrganizationID: salonOrg.ID, Name: "Facial Treatment", Description: "Relaxing facial treatment", Category: "skincare", Duration: 60, Price: 85.00, RequiresVehicle: false, IsActive: true},
	}

	allServices := append(garageServices, salonServices...)
	for _, service := range allServices {
		db.FirstOrCreate(&service, "id = ?", service.ID)
	}

	// Add sample customers
	garageCustomers := []Customer{
		{ID: "customer_john_smith", FirstName: "John", LastName: "Smith", Email: "john.smith@email.com", Phone: "+61412345678", OrganizationID: garageOrg.ID, IsActive: true, Address: Address{Street: "123 Main St", Suburb: "Adelaide", State: "SA", Postcode: "5000", Country: "Australia"}},
		{ID: "customer_sarah_jones", FirstName: "Sarah", LastName: "Jones", Email: "sarah.jones@email.com", Phone: "+61423456789", OrganizationID: garageOrg.ID, IsActive: true, Address: Address{Street: "456 Oak Ave", Suburb: "Adelaide", State: "SA", Postcode: "5001", Country: "Australia"}},
	}

	salonCustomers := []Customer{
		{ID: "customer_emma_wilson", FirstName: "Emma", LastName: "Wilson", Email: "emma.wilson@email.com", Phone: "+61434567890", OrganizationID: salonOrg.ID, IsActive: true, Address: Address{Street: "789 Pine Rd", Suburb: "Adelaide", State: "SA", Postcode: "5002", Country: "Australia"}},
		{ID: "customer_lisa_brown", FirstName: "Lisa", LastName: "Brown", Email: "lisa.brown@email.com", Phone: "+61445678901", OrganizationID: salonOrg.ID, IsActive: true, Address: Address{Street: "321 Elm St", Suburb: "Adelaide", State: "SA", Postcode: "5003", Country: "Australia"}},
	}

	allCustomers := append(garageCustomers, salonCustomers...)
	for _, customer := range allCustomers {
		db.FirstOrCreate(&customer, "id = ?", customer.ID)
	}

	// Add sample vehicles for garage customers
	vehicles := []Vehicle{
		{ID: "vehicle_john_car", CustomerID: "customer_john_smith", OrganizationID: garageOrg.ID, Make: "Toyota", Model: "Camry", Year: 2020, LicensePlate: "ABC123", VIN: "JT2BF22K5X0123456", Color: "Silver", Mileage: 45000, IsActive: true},
		{ID: "vehicle_sarah_car", CustomerID: "customer_sarah_jones", OrganizationID: garageOrg.ID, Make: "Honda", Model: "Accord", Year: 2019, LicensePlate: "DEF456", VIN: "1HGBH41JXMN654321", Color: "Blue", Mileage: 32000, IsActive: true},
	}

	for _, vehicle := range vehicles {
		db.FirstOrCreate(&vehicle, "id = ?", vehicle.ID)
	}

	// Add default permissions for admin users
	permissions := []string{
		"create_users", "read_users", "update_users", "delete_users",
		"create_customers", "read_customers", "update_customers", "delete_customers",
		"create_vehicles", "read_vehicles", "update_vehicles", "delete_vehicles",
		"create_services", "read_services", "update_services", "delete_services",
		"create_bookings", "read_bookings", "update_bookings", "delete_bookings",
		"create_participants", "read_participants", "update_participants", "delete_participants",
		"create_shifts", "read_shifts", "update_shifts", "delete_shifts",
		"create_documents", "read_documents", "update_documents", "delete_documents",
		"create_care_plans", "read_care_plans", "update_care_plans", "delete_care_plans",
		"view_reports", "manage_organization",
		// ERP & POS permissions
		"manage_inventory", "manage_products", "manage_suppliers", "manage_purchase_orders",
		"pos_access", "manage_discounts", "manage_tax_rates", "cash_drawer_access",
		"layby_access", "view_pos_reports",
	}

	for _, admin := range admins {
		for _, perm := range permissions {
			userPerm := UserPermission{
				UserID:     admin.ID,
				Permission: perm,
			}
			db.FirstOrCreate(&userPerm, "user_id = ? AND permission = ?", admin.ID, perm)
		}
	}

	// Create default module configurations for each organization
	organizations := []string{garageOrg.ID, salonOrg.ID, healthcareOrg.ID}
	for _, orgID := range organizations {
		orgModules := OrganizationModules{
			OrganizationID:       orgID,
			InventoryEnabled:     true,
			SupplierEnabled:      true,
			PurchaseOrderEnabled: true,
			POSEnabled:          true,
			CRMEnabled:          true,
			ReportsEnabled:      true,
		}
		db.FirstOrCreate(&orgModules, "organization_id = ?", orgID)

		// Create default tax rates (Australian GST)
		gstRate := TaxRate{
			OrganizationID: orgID,
			Name:          "GST",
			Rate:          10.00,
			IsDefault:     true,
			IsActive:      true,
		}
		db.FirstOrCreate(&gstRate, "organization_id = ? AND name = ?", orgID, "GST")

		// Create default inventory locations
		locations := []InventoryLocation{
			{OrganizationID: orgID, Name: "Main Warehouse", Description: "Primary storage location", Type: "warehouse", IsActive: true},
			{OrganizationID: orgID, Name: "Showroom", Description: "Display area", Type: "showroom", IsActive: true},
		}
		if orgID == garageOrg.ID {
			locations = append(locations, InventoryLocation{OrganizationID: orgID, Name: "Service Bay", Description: "Workshop area", Type: "service_bay", IsActive: true})
		}

		for _, location := range locations {
			db.FirstOrCreate(&location, "organization_id = ? AND name = ?", orgID, location.Name)
		}

		// Create sample product categories based on business type
		var categories []ProductCategory
		switch orgID {
		case garageOrg.ID:
			categories = []ProductCategory{
				{OrganizationID: orgID, Name: "Engine Parts", Description: "Engine components and parts", IsActive: true},
				{OrganizationID: orgID, Name: "Brake System", Description: "Brake pads, discs, and components", IsActive: true},
				{OrganizationID: orgID, Name: "Oils & Fluids", Description: "Engine oils, coolants, brake fluids", IsActive: true},
				{OrganizationID: orgID, Name: "Tires", Description: "Car tires and wheels", IsActive: true},
			}
		case salonOrg.ID:
			categories = []ProductCategory{
				{OrganizationID: orgID, Name: "Hair Care", Description: "Shampoos, conditioners, treatments", IsActive: true},
				{OrganizationID: orgID, Name: "Nail Care", Description: "Nail polishes, tools, treatments", IsActive: true},
				{OrganizationID: orgID, Name: "Skin Care", Description: "Facial products and treatments", IsActive: true},
				{OrganizationID: orgID, Name: "Tools & Equipment", Description: "Salon tools and equipment", IsActive: true},
			}
		default:
			categories = []ProductCategory{
				{OrganizationID: orgID, Name: "General", Description: "General products and supplies", IsActive: true},
			}
		}

		for _, category := range categories {
			db.FirstOrCreate(&category, "organization_id = ? AND name = ?", orgID, category.Name)
		}
	}

	return nil
}
