// Package industry tailors DASYIN to the kind of business an organisation
// runs. A business type (mechanic, salon, clinic, …) decides which modules are
// shown, the words the app uses (job cards instead of bookings, patients
// instead of customers), the role templates staff are given, a setup
// checklist and a sample service catalogue.
//
// Precedence (see Decide):
//
//	plan entitlements ∩ business-type modules (∪ admin-enabled extras)
//	  → the user's role (module × action permissions, data scope)
//	  → super_admin bypasses everything.
//
// The registry lives in the database (seeded from DefaultTypes) so super
// admins can edit it without a deploy.
package industry

import (
	"time"
)

// Actions a role can be granted on a module.
const (
	ActView    = "view"
	ActCreate  = "create"
	ActEdit    = "edit"
	ActDelete  = "delete"
	ActApprove = "approve"
)

// AllActions in display order.
var AllActions = []string{ActView, ActCreate, ActEdit, ActDelete, ActApprove}

// Base permission levels of a role. The legacy users.role string is kept in
// step with it (read_only users are stored as "staff").
const (
	BaseAdmin    = "admin"
	BaseManager  = "manager"
	BaseStaff    = "staff"
	BaseReadOnly = "read_only"
)

// Data scopes.
const (
	ScopeAll = "all"
	ScopeOwn = "own"
)

// Setup states of an organisation.
const (
	SetupPending = "pending" // created with a business type; the wizard runs on first admin sign-in
	SetupDone    = "done"
	SetupLegacy  = "legacy" // existed before business types (or created without one)
)

// Pseudo modules used only for role permissions: "core" (dashboard,
// analytics, reports, expenses) and "team" (staff & users).
const (
	PermCore = "core"
	PermTeam = "team"
)

// AllModules marks a business type that shows every module of the plan.
const AllModules = "*"

// Perms maps a module key (or "*" for every module) to granted actions.
type Perms map[string][]string

// RoleTemplate is a role a business type offers.
type RoleTemplate struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Base        string `json:"base"`  // admin | manager | staff | read_only
	Scope       string `json:"scope"` // all | own (bookings / job cards assigned to them)
	Perms       Perms  `json:"permissions"`
	// Legacy marks the template legacy users.role values map to
	// ("admin", "manager", "staff").
	Legacy string `json:"legacy,omitempty"`
	// Home is where the user lands when the dashboard is not available to them.
	Home string `json:"home,omitempty"`
	// Steps are the "Getting started" steps for people with this role, in
	// their own words (pkg/onboarding; see Step and StepsFor in defaults.go).
	Steps []Step `json:"steps,omitempty"`
}

// ChecklistItem is a setup task suggested for a business type.
type ChecklistItem struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
	To    string `json:"to,omitempty"`
}

// SampleService is a suggested catalogue entry. Prices are examples only.
type SampleService struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Duration int     `json:"duration"` // minutes
	Price    float64 `json:"price"`    // 0 = set your own
	Example  bool    `json:"example"`
}

// BusinessType is one entry of the registry.
type BusinessType struct {
	Key          string `json:"key" gorm:"type:varchar(40);primaryKey"`
	Name         string `json:"name" gorm:"type:varchar(120);not null"`
	Description  string `json:"description" gorm:"type:text"`
	Icon         string `json:"icon" gorm:"type:varchar(80)"`
	PlanIndustry string `json:"plan_industry" gorm:"type:varchar(40)"` // pkg/plans industry bundle
	// Modules shown for this type ("*" = all of the plan).
	Modules []string `json:"modules" gorm:"type:text;serializer:json"`
	// HiddenPaths are navigation entries hidden even though their module is on
	// (e.g. the CRM pipeline for a workshop).
	HiddenPaths      []string          `json:"hidden_paths" gorm:"type:text;serializer:json"`
	Terms            map[string]string `json:"terms" gorm:"type:text;serializer:json"`
	Roles            []RoleTemplate    `json:"roles" gorm:"type:text;serializer:json"`
	Checklist        []ChecklistItem   `json:"checklist" gorm:"type:text;serializer:json"`
	SampleServices   []SampleService   `json:"sample_services" gorm:"type:text;serializer:json"`
	BookingDefaults  map[string]any    `json:"booking_defaults" gorm:"type:text;serializer:json"`
	DashboardWidgets []string          `json:"dashboard_widgets" gorm:"type:text;serializer:json"`
	Features         []string          `json:"features" gorm:"type:text;serializer:json"`
	Active           bool              `json:"active" gorm:"default:true"`
	SortOrder        int               `json:"sort_order" gorm:"default:0"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

func (BusinessType) TableName() string { return "industry_types" }

// OrgSettings is an organisation's business type choice.
type OrgSettings struct {
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	TypeKey        string `json:"type" gorm:"type:varchar(40);not null;default:'general'"`
	// Confirmed is true once an admin chose the type (wizard or settings).
	// Unconfirmed organisations keep every module (no surprise lock-outs).
	Confirmed    bool      `json:"confirmed" gorm:"default:false"`
	SetupStatus  string    `json:"setup_status" gorm:"type:varchar(20);not null;default:'legacy'"`
	SetupStep    string    `json:"setup_step" gorm:"type:varchar(30)"`
	ExtraModules []string  `json:"extra_modules" gorm:"type:text;serializer:json"`
	UpdatedBy    string    `json:"updated_by" gorm:"type:varchar(255)"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (OrgSettings) TableName() string { return "industry_org_settings" }

// OrgRole is a role of one organisation (copied from a template, then editable).
type OrgRole struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_industry_role_key"`
	Key            string    `json:"key" gorm:"type:varchar(60);not null;uniqueIndex:idx_industry_role_key"`
	TypeKey        string    `json:"type" gorm:"type:varchar(40)"` // template's business type ("" = custom)
	Name           string    `json:"name" gorm:"type:varchar(80);not null"`
	Description    string    `json:"description" gorm:"type:text"`
	Base           string    `json:"base" gorm:"type:varchar(20);not null;default:'staff'"`
	Scope          string    `json:"scope" gorm:"type:varchar(10);not null;default:'all'"`
	Perms          Perms     `json:"permissions" gorm:"type:text;serializer:json"`
	Legacy         string    `json:"legacy" gorm:"type:varchar(20)"`
	Home           string    `json:"home" gorm:"type:varchar(80)"`
	Custom         bool      `json:"custom" gorm:"default:false"`
	SortOrder      int       `json:"sort_order" gorm:"default:0"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (OrgRole) TableName() string { return "industry_org_roles" }

// UserRole assigns a user to one of their organisation's roles.
type UserRole struct {
	UserID         string    `json:"user_id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	RoleID         string    `json:"role_id" gorm:"type:varchar(36);not null;index"`
	AssignedBy     string    `json:"assigned_by" gorm:"type:varchar(255)"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (UserRole) TableName() string { return "industry_user_roles" }

// Meta stores install facts (e.g. when business types were introduced).
type Meta struct {
	Key   string `gorm:"type:varchar(60);primaryKey"`
	Value string `gorm:"type:text"`
}

func (Meta) TableName() string { return "industry_meta" }
