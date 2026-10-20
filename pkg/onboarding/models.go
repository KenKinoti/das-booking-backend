// Package onboarding is "Getting started": a short list of steps on how to
// use DASYIN, written from each person's point of view — their role and
// their organisation's business type (an Owner of a club, a Bartender, a
// Store keeper, a Receptionist at a spa…).
//
// The steps live next to the role templates (industry.StepsFor in
// pkg/industry/defaults.go). This package keeps each person's progress
// (welcomed, dismissed, steps they ticked), ticks steps by itself where a
// cheap check can tell (a POS sale made, a delivery received, a count
// approved…) and serves /api/v1/onboarding/me.
package onboarding

import (
	"time"
)

// Progress is one person's "Getting started" state in one organisation.
type Progress struct {
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	UserID         string     `json:"user_id" gorm:"type:varchar(255);primaryKey"`
	WelcomedAt     *time.Time `json:"welcomed_at"`
	DismissedAt    *time.Time `json:"dismissed_at"`
	// DoneSteps maps a step key to when the person ticked it (RFC 3339).
	DoneSteps map[string]string `json:"done_steps" gorm:"type:text;serializer:json"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// TableName keeps the table name stable.
func (Progress) TableName() string { return "onboarding_progress" }

// StepView is a step as the API returns it.
type StepView struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	To          string `json:"to"`
	Done        bool   `json:"done"`
	// Auto is true when the step ticks itself once the person has done it.
	Auto bool `json:"auto"`
}

// View is GET /onboarding/me.
type View struct {
	RoleKey   string     `json:"role_key"`
	RoleName  string     `json:"role_name"`
	RoleBase  string     `json:"role_base"`
	TypeKey   string     `json:"type_key"`
	TypeName  string     `json:"type_name"`
	FirstName string     `json:"first_name"`
	OrgName   string     `json:"org_name"`
	Steps     []StepView `json:"steps"`
	Welcomed  bool       `json:"welcomed"`
	Dismissed bool       `json:"dismissed"`
	DoneCount int        `json:"done_count"`
	Total     int        `json:"total"`
	Percent   int        `json:"percent"`
	AllDone   bool       `json:"all_done"`
	// Preview: a super admin viewing an organisation sees its owner's steps
	// read-only (writes are accepted but not saved).
	Preview bool `json:"preview"`
	// Hidden: nothing to show (a super admin on their own platform session).
	Hidden bool `json:"hidden"`
}
