// Package mcp is the DASYIN ERP Model Context Protocol server (Streamable
// HTTP transport at /mcp) with personal access tokens, an OAuth 2.1
// authorization server for one-click connectors (claude.ai / Claude Desktop)
// and an audit log of every AI tool call. Tools come from pkg/aitools.
package mcp

import (
	"time"

	"gorm.io/gorm"
)

// Token is a personal access token (PAT) for MCP clients. Only its SHA-256
// hash is stored; the plain value is shown once on creation.
type Token struct {
	ID         string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID      string     `json:"org_id" gorm:"type:varchar(255);not null;index"`
	UserID     string     `json:"user_id" gorm:"type:varchar(255);not null;index"`
	Name       string     `json:"name" gorm:"type:varchar(120);not null"`
	Prefix     string     `json:"prefix" gorm:"type:varchar(16);not null"`
	Hash       string     `json:"-" gorm:"type:varchar(64);not null;uniqueIndex"`
	Scope      string     `json:"scope" gorm:"type:varchar(20);not null;default:'read'"` // read | read_write
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func (Token) TableName() string { return "mcp_tokens" }

// OAuthClient is a dynamically registered (public, PKCE) OAuth client.
type OAuthClient struct {
	ID           string     `json:"client_id" gorm:"type:varchar(64);primaryKey"`
	Name         string     `json:"client_name" gorm:"type:varchar(120);not null"`
	URI          string     `json:"client_uri" gorm:"type:varchar(500)"`
	RedirectURIs []string   `json:"redirect_uris" gorm:"type:text;serializer:json"`
	RegisteredIP string     `json:"-" gorm:"type:varchar(64)"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at"`
}

func (OAuthClient) TableName() string { return "mcp_oauth_clients" }

// OAuthRequest is a pending /oauth/authorize request awaiting the user's
// consent in the web app.
type OAuthRequest struct {
	ID            string     `gorm:"type:varchar(64);primaryKey"`
	ClientID      string     `gorm:"type:varchar(64);not null"`
	RedirectURI   string     `gorm:"type:text;not null"`
	State         string     `gorm:"type:text"`
	CodeChallenge string     `gorm:"type:varchar(128);not null"`
	Scope         string     `gorm:"type:varchar(100)"`
	Resource      string     `gorm:"type:text"`
	CreatedAt     time.Time  `gorm:"not null"`
	ExpiresAt     time.Time  `gorm:"not null;index"`
	ConsumedAt    *time.Time ``
}

func (OAuthRequest) TableName() string { return "mcp_oauth_requests" }

// OAuthCode is a one-time authorization code (hashed).
type OAuthCode struct {
	Hash          string     `gorm:"type:varchar(64);primaryKey"`
	ClientID      string     `gorm:"type:varchar(64);not null"`
	GrantID       string     `gorm:"type:varchar(64);not null"`
	RedirectURI   string     `gorm:"type:text;not null"`
	CodeChallenge string     `gorm:"type:varchar(128);not null"`
	Resource      string     `gorm:"type:text"`
	ExpiresAt     time.Time  `gorm:"not null;index"`
	UsedAt        *time.Time ``
}

func (OAuthCode) TableName() string { return "mcp_oauth_codes" }

// OAuthGrant is a user's approval of a client: a "connected app".
type OAuthGrant struct {
	ID         string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	ClientID   string     `json:"client_id" gorm:"type:varchar(64);not null;index"`
	OrgID      string     `json:"org_id" gorm:"type:varchar(255);not null;index"`
	UserID     string     `json:"user_id" gorm:"type:varchar(255);not null;index"`
	Scope      string     `json:"scope" gorm:"type:varchar(20);not null"` // read | read_write
	Resource   string     `json:"resource" gorm:"type:text"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func (OAuthGrant) TableName() string { return "mcp_oauth_grants" }

// OAuthToken is an opaque access or refresh token (hashed).
type OAuthToken struct {
	Hash      string     `gorm:"type:varchar(64);primaryKey"`
	GrantID   string     `gorm:"type:varchar(64);not null;index"`
	Kind      string     `gorm:"type:varchar(10);not null"` // access | refresh
	CreatedAt time.Time  `gorm:"not null"`
	ExpiresAt time.Time  `gorm:"not null;index"`
	UsedAt    *time.Time `` // refresh token rotated
	RevokedAt *time.Time ``
}

func (OAuthToken) TableName() string { return "mcp_oauth_tokens" }

// Activity is one audited AI tool call (MCP or the in-app assistant).
type Activity struct {
	ID         string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID      string    `json:"org_id" gorm:"type:varchar(255);not null;index"`
	UserID     string    `json:"user_id" gorm:"type:varchar(255);not null;index"`
	Source     string    `json:"source" gorm:"type:varchar(20);not null"` // token | oauth | assistant | test
	TokenID    string    `json:"token_id" gorm:"type:varchar(64)"`
	ClientName string    `json:"client_name" gorm:"type:varchar(120)"`
	Tool       string    `json:"tool" gorm:"type:varchar(100);not null;index"`
	ReadOnly   bool      `json:"read_only"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error" gorm:"type:text"`
	DurationMS int64     `json:"duration_ms"`
	Args       string    `json:"args" gorm:"type:text"`
	CreatedAt  time.Time `json:"created_at" gorm:"index"`
}

func (Activity) TableName() string { return "ai_activity" }

// Migrate creates the MCP tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&Token{}, &OAuthClient{}, &OAuthRequest{}, &OAuthCode{}, &OAuthGrant{}, &OAuthToken{}, &Activity{})
}
