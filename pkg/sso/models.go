package sso

import (
	"time"

	"gorm.io/gorm"
)

// Identity links a DASYIN user to an account at a sign-in provider, by the
// provider's stable subject id ("sub").
type Identity struct {
	ID        string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	UserID    string     `json:"user_id" gorm:"type:varchar(255);index;not null"`
	Provider  string     `json:"provider" gorm:"type:varchar(20);uniqueIndex:ux_user_identity,priority:1;not null"`
	Subject   string     `json:"-" gorm:"type:varchar(255);uniqueIndex:ux_user_identity,priority:2;not null"`
	Email     string     `json:"email" gorm:"type:varchar(255)"`
	TenantID  string     `json:"tenant_id,omitempty" gorm:"type:varchar(64)"`
	LinkedAt  time.Time  `json:"linked_at"`
	LastUsed  *time.Time `json:"last_used"`
	CreatedAt time.Time  `json:"-"`
}

// TableName keeps the table name explicit.
func (Identity) TableName() string { return "user_identities" }

// Flow is one sign-in (or account-linking) attempt between /start and
// /callback: state, nonce and the PKCE verifier. Single use, 10 minutes.
type Flow struct {
	State     string    `gorm:"type:varchar(80);primaryKey"`
	Ticket    string    `gorm:"type:varchar(80);index"` // link flows: one-time start ticket
	Provider  string    `gorm:"type:varchar(20)"`
	Nonce     string    `gorm:"type:varchar(80)"`
	Verifier  string    `gorm:"type:varchar(128)"`
	Redirect  string    `gorm:"type:text"`         // app path to return to
	Mode      string    `gorm:"type:varchar(10)"`  // login | link
	UserID    string    `gorm:"type:varchar(255)"` // link: the signed-in user
	Started   bool      // link flows: /start was used (ticket spent)
	ExpiresAt time.Time `gorm:"index"`
	CreatedAt time.Time
}

// TableName keeps the table name explicit.
func (Flow) TableName() string { return "sso_flows" }

// Code is a one-time code the app exchanges for a session (POST
// /public/sso/exchange). Only its SHA-256 is stored; 2 minutes, single use.
type Code struct {
	Hash      string    `gorm:"type:varchar(64);primaryKey"`
	UserID    string    `gorm:"type:varchar(255)"`
	Provider  string    `gorm:"type:varchar(20)"`
	Redirect  string    `gorm:"type:text"`
	ExpiresAt time.Time `gorm:"index"`
	UsedAt    *time.Time
	CreatedAt time.Time
}

// TableName keeps the table name explicit.
func (Code) TableName() string { return "sso_codes" }

// ProviderConfig holds the platform's sign-in provider settings (super
// admin → System settings → Sign-in providers). Empty values fall back to
// the environment. The client secret is encrypted (pkg/mail EncryptSecret).
type ProviderConfig struct {
	Provider     string    `json:"provider" gorm:"type:varchar(20);primaryKey"`
	Enabled      bool      `json:"enabled"`
	ClientID     string    `json:"client_id" gorm:"type:varchar(255)"`
	ClientSecret string    `json:"-" gorm:"type:text"`
	Tenant       string    `json:"tenant" gorm:"type:varchar(100)"`
	UpdatedBy    string    `json:"updated_by" gorm:"type:varchar(255)"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TableName keeps the table name explicit.
func (ProviderConfig) TableName() string { return "sso_provider_configs" }

// DomainRule lets anyone with a verified email at Domain join the
// organisation with Role on their first single sign-on (Settings → Security).
type DomainRule struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	Domain         string    `json:"domain" gorm:"type:varchar(255);uniqueIndex;not null"`
	Role           string    `json:"role" gorm:"type:varchar(50);not null"`
	CreatedBy      string    `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt      time.Time `json:"created_at"`
}

// TableName keeps the table name explicit.
func (DomainRule) TableName() string { return "sso_domain_rules" }

// Migrate creates the tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&Identity{}, &Flow{}, &Code{}, &ProviderConfig{}, &DomainRule{})
}
