// Package plans packages the ERP into subscription tiers (Small, SME, Large)
// with per-industry module bundles, multi-currency price quotes and an
// entitlement middleware that keeps each organisation inside its plan.
//
// Everything commercial (module prices, tier rules, industry bundles and
// fallback FX rates) lives in the database so super admins can fine-tune it
// without a deploy. Defaults are seeded on first run.
package plans

import (
	"time"
)

// Module is an entitlement unit (matches the `module` keys of the frontend
// navigation). Core is always included and never priced.
type Module struct {
	Key             string    `json:"key" gorm:"type:varchar(40);primaryKey"`
	Name            string    `json:"name" gorm:"type:varchar(120);not null"`
	Description     string    `json:"description" gorm:"type:text"`
	Icon            string    `json:"icon" gorm:"type:varchar(80)"`
	MonthlyPriceUSD float64   `json:"monthly_price_usd" gorm:"type:decimal(12,2);not null;default:0"`
	Core            bool      `json:"core" gorm:"default:false"`
	Active          bool      `json:"active" gorm:"default:true"`
	SortOrder       int       `json:"sort_order" gorm:"default:0"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (Module) TableName() string { return "plan_modules" }

// Industry groups businesses and recommends a module bundle and tier.
type Industry struct {
	Key             string    `json:"key" gorm:"type:varchar(40);primaryKey"`
	Name            string    `json:"name" gorm:"type:varchar(120);not null"`
	Description     string    `json:"description" gorm:"type:text"`
	Icon            string    `json:"icon" gorm:"type:varchar(80)"`
	Modules         []string  `json:"modules" gorm:"type:text;serializer:json"`
	RecommendedTier string    `json:"recommended_tier" gorm:"type:varchar(20);default:'sme'"`
	Active          bool      `json:"active" gorm:"default:true"`
	SortOrder       int       `json:"sort_order" gorm:"default:0"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (Industry) TableName() string { return "plan_industries" }

// Tier holds the commercial rules of one package. Zero limits mean unlimited.
type Tier struct {
	Key                    string    `json:"key" gorm:"type:varchar(20);primaryKey"` // small | sme | large
	Name                   string    `json:"name" gorm:"type:varchar(80);not null"`
	Tagline                string    `json:"tagline" gorm:"type:varchar(255)"`
	BasePriceUSD           float64   `json:"base_price_usd" gorm:"type:decimal(12,2);not null;default:0"`
	MaxModules             int       `json:"max_modules" gorm:"default:0"` // non-core "systems"; 0 = unlimited
	MaxUsers               int       `json:"max_users" gorm:"default:0"`   // 0 = unlimited
	AddonDiscountPct       float64   `json:"addon_discount_pct" gorm:"type:decimal(6,2);default:0"`
	AnnualDiscountPct      float64   `json:"annual_discount_pct" gorm:"type:decimal(6,2);default:0"`
	IncludesIndustryBundle bool      `json:"includes_industry_bundle" gorm:"default:false"`
	IncludesAllModules     bool      `json:"includes_all_modules" gorm:"default:false"`
	Enterprise             bool      `json:"enterprise" gorm:"default:false"` // custom pricing / contract available
	TrialDays              int       `json:"trial_days" gorm:"default:14"`
	Features               []string  `json:"features" gorm:"type:text;serializer:json"`
	SortOrder              int       `json:"sort_order" gorm:"default:0"`
	UpdatedAt              time.Time `json:"updated_at"`
}

func (Tier) TableName() string { return "plan_tiers" }

// FXRate is a fallback exchange rate (units of Currency per 1 USD) used when
// live rates are unavailable. Editable by super admins.
type FXRate struct {
	Currency   string    `json:"currency" gorm:"type:varchar(3);primaryKey"`
	RatePerUSD float64   `json:"rate_per_usd" gorm:"type:decimal(20,6);not null"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (FXRate) TableName() string { return "plan_fx_rates" }

// Subscription is an organisation's plan.
type Subscription struct {
	ID              string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID  string     `json:"organization_id" gorm:"type:varchar(255);not null;uniqueIndex"`
	Tier            string     `json:"tier" gorm:"type:varchar(20);not null"`
	Industry        string     `json:"industry" gorm:"type:varchar(40);not null;default:'general'"`
	Modules         []string   `json:"modules" gorm:"type:text;serializer:json"` // selected modules (small) / add-ons (sme)
	Cycle           string     `json:"cycle" gorm:"type:varchar(10);not null;default:'monthly'"`
	Currency        string     `json:"currency" gorm:"type:varchar(3);not null;default:'USD'"`
	DefaultCurrency string     `json:"default_currency" gorm:"type:varchar(3)"` // organisation-wide default for other modules
	Status          string     `json:"status" gorm:"type:varchar(20);not null;default:'active'"`
	TrialEndsAt     *time.Time `json:"trial_ends_at,omitempty"`
	CustomPriceUSD  *float64   `json:"custom_price_usd,omitempty" gorm:"type:decimal(12,2)"` // enterprise override (monthly)
	MaxUsers        *int       `json:"max_users_override,omitempty"`
	PriceUSD        float64    `json:"price_usd" gorm:"type:decimal(12,2)"` // monthly-equivalent at last save
	Price           float64    `json:"price" gorm:"type:decimal(18,3)"`     // per cycle, in Currency, at last save
	Grandfathered   bool       `json:"grandfathered" gorm:"default:false"`
	Notes           string     `json:"notes" gorm:"type:text"`
	UpdatedBy       string     `json:"updated_by" gorm:"type:varchar(255)"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func (Subscription) TableName() string { return "plan_subscriptions" }

// Meta stores small key/value facts (e.g. when plans were installed).
type Meta struct {
	Key   string `gorm:"type:varchar(60);primaryKey"`
	Value string `gorm:"type:text"`
}

func (Meta) TableName() string { return "plan_meta" }

// Subscription statuses and billing cycles.
const (
	StatusTrial     = "trial"
	StatusActive    = "active"
	StatusPastDue   = "past_due"
	StatusCancelled = "cancelled"

	CycleMonthly = "monthly"
	CycleAnnual  = "annual"

	TierSmall = "small"
	TierSME   = "sme"
	TierLarge = "large"

	CoreModule = "core"
)

// ValidStatus reports whether s is a known subscription status.
func ValidStatus(s string) bool {
	switch s {
	case StatusTrial, StatusActive, StatusPastDue, StatusCancelled:
		return true
	}
	return false
}
