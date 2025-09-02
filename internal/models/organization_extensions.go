package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// OrganizationBranding represents branding customization for organizations
type OrganizationBranding struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(36);not null;uniqueIndex"`
	LogoURL        string    `json:"logo_url" gorm:"type:varchar(500)"`
	PrimaryColor   string    `json:"primary_color" gorm:"type:varchar(7);default:'#667eea'"` // Hex color
	SecondaryColor string    `json:"secondary_color" gorm:"type:varchar(7);default:'#764ba2'"`
	AccentColor    string    `json:"accent_color" gorm:"type:varchar(7);default:'#10b981'"`
	ThemeName      string    `json:"theme_name" gorm:"type:varchar(50);default:'professional'"`
	CustomCSS      string    `json:"custom_css" gorm:"type:text"`
	FaviconURL     string    `json:"favicon_url" gorm:"type:varchar(500)"`
	CompanySlogan  string    `json:"company_slogan" gorm:"type:varchar(255)"`
	FooterText     string    `json:"footer_text" gorm:"type:text"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
}

// OrganizationSettings represents configuration settings for organizations
type OrganizationSettings struct {
	ID                    string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID        string    `json:"organization_id" gorm:"type:varchar(36);not null;uniqueIndex"`
	Timezone              string    `json:"timezone" gorm:"type:varchar(50);default:'Australia/Adelaide'"`
	DateFormat            string    `json:"date_format" gorm:"type:varchar(20);default:'DD/MM/YYYY'"`
	TimeFormat            string    `json:"time_format" gorm:"type:varchar(20);default:'24h'"`
	Currency              string    `json:"currency" gorm:"type:varchar(3);default:'AUD'"`
	Language              string    `json:"language" gorm:"type:varchar(10);default:'en-AU'"`
	DefaultShiftDuration  int       `json:"default_shift_duration" gorm:"default:120"` // minutes
	MaxShiftDuration      int       `json:"max_shift_duration" gorm:"default:720"`     // minutes
	MinShiftNotice        int       `json:"min_shift_notice" gorm:"default:30"`        // minutes
	RequireShiftNotes     bool      `json:"require_shift_notes" gorm:"default:false"`
	RequirePhotoEvidence  bool      `json:"require_photo_evidence" gorm:"default:false"`
	AutoAssignShifts      bool      `json:"auto_assign_shifts" gorm:"default:false"`
	EnableSMSNotifications bool      `json:"enable_sms_notifications" gorm:"default:true"`
	EnableEmailNotifications bool    `json:"enable_email_notifications" gorm:"default:true"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
}

// OrganizationSubscription represents billing and subscription details
type OrganizationSubscription struct {
	ID                  string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID      string     `json:"organization_id" gorm:"type:varchar(36);not null;uniqueIndex"`
	PlanName           string     `json:"plan_name" gorm:"type:varchar(50);not null"` // starter, professional, enterprise
	Status             string     `json:"status" gorm:"type:varchar(20);default:'active'"`  // active, suspended, cancelled
	BillingEmail       string     `json:"billing_email" gorm:"type:varchar(255);not null"`
	MonthlyRate        float64    `json:"monthly_rate" gorm:"type:decimal(10,2);not null"`
	MaxUsers           int        `json:"max_users" gorm:"not null;default:5"`
	MaxParticipants    int        `json:"max_participants" gorm:"not null;default:50"`
	MaxStorageGB       int        `json:"max_storage_gb" gorm:"not null;default:10"`
	HasCustomBranding  bool       `json:"has_custom_branding" gorm:"default:false"`
	HasAPIAccess       bool       `json:"has_api_access" gorm:"default:false"`
	HasAdvancedReports bool       `json:"has_advanced_reports" gorm:"default:false"`
	BillingCycle       string     `json:"billing_cycle" gorm:"type:varchar(20);default:'monthly'"` // monthly, yearly
	NextBillingDate    time.Time  `json:"next_billing_date" gorm:"not null"`
	TrialEndsAt        *time.Time `json:"trial_ends_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
}

// Role represents system roles with organization-specific permissions
type Role struct {
	ID             string           `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string           `json:"organization_id" gorm:"type:varchar(36);not null;index"`
	Name           string           `json:"name" gorm:"type:varchar(50);not null"`
	Description    string           `json:"description" gorm:"type:text"`
	IsSystem       bool             `json:"is_system" gorm:"default:false"` // System roles cannot be deleted
	IsActive       bool             `json:"is_active" gorm:"default:true"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	DeletedAt      gorm.DeletedAt   `json:"-" gorm:"index"`

	// Relationships
	Organization    Organization     `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	RolePermissions []RolePermission `json:"role_permissions,omitempty" gorm:"foreignKey:RoleID"`
	Users           []User           `json:"users,omitempty" gorm:"foreignKey:RoleID"`
}

// RolePermission represents permissions assigned to roles
type RolePermission struct {
	ID         string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	RoleID     string    `json:"role_id" gorm:"type:varchar(36);not null;index"`
	Permission string    `json:"permission" gorm:"type:varchar(100);not null"`
	CanCreate  bool      `json:"can_create" gorm:"default:false"`
	CanRead    bool      `json:"can_read" gorm:"default:false"`
	CanUpdate  bool      `json:"can_update" gorm:"default:false"`
	CanDelete  bool      `json:"can_delete" gorm:"default:false"`
	CreatedAt  time.Time `json:"created_at"`

	// Relationships
	Role Role `json:"role,omitempty" gorm:"foreignKey:RoleID"`
}

// OrganizationInvitation represents pending user invitations
type OrganizationInvitation struct {
	ID             string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(36);not null;index"`
	Email          string         `json:"email" gorm:"type:varchar(255);not null"`
	Role           string         `json:"role" gorm:"type:varchar(50);not null"`
	InvitedBy      string         `json:"invited_by" gorm:"type:varchar(36);not null"`
	Token          string         `json:"token" gorm:"type:varchar(255);not null;uniqueIndex"`
	Status         string         `json:"status" gorm:"type:varchar(20);default:'pending'"` // pending, accepted, expired
	ExpiresAt      time.Time      `json:"expires_at" gorm:"not null"`
	AcceptedAt     *time.Time     `json:"accepted_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Inviter      User         `json:"inviter,omitempty" gorm:"foreignKey:InvitedBy"`
}

// AuditLog represents system activity tracking
type AuditLog struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(36);not null;index"`
	UserID         *string   `json:"user_id,omitempty" gorm:"type:varchar(36);index"`
	Action         string    `json:"action" gorm:"type:varchar(100);not null;index"` // create_shift, update_participant, etc.
	EntityType     string    `json:"entity_type" gorm:"type:varchar(50);not null"`   // shift, participant, user, etc.
	EntityID       string    `json:"entity_id" gorm:"type:varchar(36);not null"`
	OldValues      string    `json:"old_values" gorm:"type:jsonb"`      // JSON of old values
	NewValues      string    `json:"new_values" gorm:"type:jsonb"`      // JSON of new values
	IPAddress      string    `json:"ip_address" gorm:"type:varchar(45)"`
	UserAgent      string    `json:"user_agent" gorm:"type:varchar(500)"`
	CreatedAt      time.Time `json:"created_at"`

	// Relationships
	Organization Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	User         *User        `json:"user,omitempty" gorm:"foreignKey:UserID"`
}

// BeforeCreate hooks for generating UUIDs
func (b *OrganizationBranding) BeforeCreate(tx *gorm.DB) (err error) {
	if b.ID == "" {
		b.ID = uuid.New().String()
	}
	return
}

func (s *OrganizationSettings) BeforeCreate(tx *gorm.DB) (err error) {
	if s.ID == "" {
		s.ID = uuid.New().String()
	}
	return
}

func (s *OrganizationSubscription) BeforeCreate(tx *gorm.DB) (err error) {
	if s.ID == "" {
		s.ID = uuid.New().String()
	}
	return
}

func (r *Role) BeforeCreate(tx *gorm.DB) (err error) {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	return
}

func (r *RolePermission) BeforeCreate(tx *gorm.DB) (err error) {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	return
}

func (i *OrganizationInvitation) BeforeCreate(tx *gorm.DB) (err error) {
	if i.ID == "" {
		i.ID = uuid.New().String()
	}
	if i.Token == "" {
		i.Token = uuid.New().String()
	}
	return
}

func (a *AuditLog) BeforeCreate(tx *gorm.DB) (err error) {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	return
}

// Update the migration function to include new models
func MigrateExtendedDB(db *gorm.DB) error {
	// First run the original migration
	if err := MigrateDB(db); err != nil {
		return err
	}

	// Then migrate the new models
	return db.AutoMigrate(
		&OrganizationBranding{},
		&OrganizationSettings{},
		&OrganizationSubscription{},
		&Role{},
		&RolePermission{},
		&OrganizationInvitation{},
		&AuditLog{},
	)
}

// Create default organization data
func SetupOrganizationDefaults(db *gorm.DB, orgID string) error {
	// Create default branding
	branding := OrganizationBranding{
		OrganizationID: orgID,
		PrimaryColor:   "#667eea",
		SecondaryColor: "#764ba2",
		AccentColor:    "#10b981",
		ThemeName:      "professional",
	}
	db.FirstOrCreate(&branding, "organization_id = ?", orgID)

	// Create default settings
	settings := OrganizationSettings{
		OrganizationID:        orgID,
		Timezone:              "Australia/Adelaide",
		DateFormat:            "DD/MM/YYYY",
		TimeFormat:            "24h",
		Currency:              "AUD",
		Language:              "en-AU",
		DefaultShiftDuration:  120,
		MaxShiftDuration:      720,
		MinShiftNotice:        30,
		EnableSMSNotifications: true,
		EnableEmailNotifications: true,
	}
	db.FirstOrCreate(&settings, "organization_id = ?", orgID)

	// Create default roles
	adminRole := Role{
		OrganizationID: orgID,
		Name:          "Administrator",
		Description:   "Full system access",
		IsSystem:      true,
		IsActive:      true,
	}
	db.FirstOrCreate(&adminRole, "organization_id = ? AND name = ?", orgID, "Administrator")

	managerRole := Role{
		OrganizationID: orgID,
		Name:          "Manager",
		Description:   "Can manage staff and participants",
		IsSystem:      true,
		IsActive:      true,
	}
	db.FirstOrCreate(&managerRole, "organization_id = ? AND name = ?", orgID, "Manager")

	careWorkerRole := Role{
		OrganizationID: orgID,
		Name:          "Care Worker",
		Description:   "Can complete assigned shifts",
		IsSystem:      true,
		IsActive:      true,
	}
	db.FirstOrCreate(&careWorkerRole, "organization_id = ? AND name = ?", orgID, "Care Worker")

	return nil
}