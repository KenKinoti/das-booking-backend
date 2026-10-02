// Package pricebook holds supplier price lists (wholesale suppliers, a
// self-hosted server and competitors) and compares what a domain + hosting
// combo costs from each of them, so a reseller can set retail prices.
//
//   - Suppliers and their price list items (hosting plans, domain
//     registration / renewal / transfer, SSL, mailboxes), with price history,
//     CSV import / export and an "as of" age warning.
//   - A self-hosted cost model (monthly fixed costs ÷ sites) with break-even
//     against each wholesale plan.
//   - Combo comparison per supplier and the cheapest mix, in the supplier's
//     currency and the organisation's home currency (pkg/fxrates).
//   - A retail pricing helper (margin or markup, payment fees, tax, friendly
//     rounding, FX sensitivity) that can save catalog services.
//
// The maths lives in calc.go, selfhost.go and retail.go as pure functions so
// it is unit-tested without a database.
package pricebook

import (
	"net/url"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Supplier types.
const (
	TypeWholesale  = "wholesale"   // you buy from them and resell
	TypeSelfHosted = "self_hosted" // your own server: a monthly cost shared by the sites on it
	TypeCompetitor = "competitor"  // retail prices in the market: a benchmark, never an option to buy
)

// Item kinds.
const (
	KindHosting  = "hosting_plan"
	KindRegister = "domain_register"
	KindRenew    = "domain_renew"
	KindTransfer = "domain_transfer"
	KindSSL      = "ssl"
	KindEmail    = "email_mailbox"
	KindOther    = "other"
)

// Billing periods.
const (
	PeriodMonth  = "month"
	PeriodYear   = "year"
	Period2Y     = "2y"
	Period3Y     = "3y"
	PeriodOneOff = "one_off"
)

// Unlimited marks "unlimited" disk, sites or mailboxes.
const Unlimited = -1

// StaleDays is how old an "as of" date may be before a price is flagged
// "check price".
const StaleDays = 90

// Kinds in display order with their labels.
var Kinds = []struct{ Key, Label string }{
	{KindHosting, "Hosting plan"}, {KindRegister, "Domain registration"}, {KindRenew, "Domain renewal"},
	{KindTransfer, "Domain transfer"}, {KindSSL, "SSL certificate"}, {KindEmail, "Email mailbox"}, {KindOther, "Other"},
}

// Periods in display order with their labels and length in months
// (0 = paid once).
var Periods = []struct {
	Key, Label string
	Months     int
}{
	{PeriodMonth, "Monthly", 1}, {PeriodYear, "Yearly", 12}, {Period2Y, "Every 2 years", 24}, {Period3Y, "Every 3 years", 36}, {PeriodOneOff, "One-off", 0},
}

// KindLabel is the readable name of a kind.
func KindLabel(k string) string {
	for _, x := range Kinds {
		if x.Key == k {
			return x.Label
		}
	}
	return k
}

// IsKind reports whether k is a known kind.
func IsKind(k string) bool {
	for _, x := range Kinds {
		if x.Key == k {
			return true
		}
	}
	return false
}

// IsDomainKind reports whether a kind is priced per TLD.
func IsDomainKind(k string) bool { return k == KindRegister || k == KindRenew || k == KindTransfer }

// PeriodMonths is the length of a billing period in months (0 for one-off
// and unknown periods).
func PeriodMonths(p string) int {
	for _, x := range Periods {
		if x.Key == p {
			return x.Months
		}
	}
	return 0
}

// IsPeriod reports whether p is a known billing period.
func IsPeriod(p string) bool {
	for _, x := range Periods {
		if x.Key == p {
			return true
		}
	}
	return false
}

// IsType reports whether t is a known supplier type.
func IsType(t string) bool { return t == TypeWholesale || t == TypeSelfHosted || t == TypeCompetitor }

// CostLine is one monthly cost of a self-hosted server. Kind "time" is
// Hours × Rate (your own time); every other line is Amount per month.
type CostLine struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Kind     string  `json:"kind"`     // fixed | time
	Amount   float64 `json:"amount"`   // per month (fixed lines)
	Hours    float64 `json:"hours"`    // per month (time lines)
	Rate     float64 `json:"rate"`     // per hour (time lines)
	Currency string  `json:"currency"` // "" = the supplier's currency
	Note     string  `json:"note,omitempty"`
}

// Supplier is a price list owner.
type Supplier struct {
	ID             string `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_pbs_org_name,priority:1"`
	Name           string `json:"name" gorm:"type:varchar(160);not null"`
	NameKey        string `json:"-" gorm:"type:varchar(160);not null;uniqueIndex:idx_pbs_org_name,priority:2"`
	Type           string `json:"type" gorm:"type:varchar(20);not null"`
	Currency       string `json:"currency" gorm:"type:varchar(3);not null"`
	Website        string `json:"website" gorm:"type:varchar(255)"`
	Notes          string `json:"notes" gorm:"type:text"`
	// System marks the seeded suppliers: synergy | hosting_com | self_hosted.
	System string `json:"system" gorm:"type:varchar(20)"`
	// Self-hosted cost model.
	Costs        []CostLine `json:"costs" gorm:"type:text;serializer:json"`
	Capacity     int        `json:"capacity"`      // sites one server holds
	CurrentSites int        `json:"current_sites"` // sites on it now
	DiskGB       float64    `json:"disk_gb"`       // server disk (0 = not stated)
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (Supplier) TableName() string { return "pricebook_suppliers" }

// Item is one price on a supplier's list. Price is what is charged per
// billing Period (month: per month; year: per year; 2y: per two years). When
// Promo is set, Price is the introductory price of the first period and
// RegularPrice what every later period costs.
type Item struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_pbi_key,priority:1;index:idx_pbi_org_kind,priority:1"`
	SupplierID     string     `json:"supplier_id" gorm:"type:varchar(64);not null;uniqueIndex:idx_pbi_key,priority:2"`
	Kind           string     `json:"kind" gorm:"type:varchar(20);not null;uniqueIndex:idx_pbi_key,priority:3;index:idx_pbi_org_kind,priority:2"`
	Name           string     `json:"name" gorm:"type:varchar(200);not null"`
	NameKey        string     `json:"-" gorm:"type:varchar(200);not null;uniqueIndex:idx_pbi_key,priority:4"`
	TLD            string     `json:"tld" gorm:"column:tld;type:varchar(40);not null;default:'';uniqueIndex:idx_pbi_key,priority:5"`
	Period         string     `json:"period" gorm:"type:varchar(10);not null;uniqueIndex:idx_pbi_key,priority:6"`
	Price          float64    `json:"price"`
	Currency       string     `json:"currency" gorm:"type:varchar(3);not null"`
	SetupFee       float64    `json:"setup_fee"`
	Promo          bool       `json:"promo"`
	RegularPrice   *float64   `json:"regular_price"`
	DiskGB         *float64   `json:"disk_gb"` // -1 = unlimited
	Sites          *int       `json:"sites"`   // -1 = unlimited
	Bandwidth      string     `json:"bandwidth" gorm:"type:varchar(60)"`
	EmailAccounts  *int       `json:"email_accounts"` // -1 = unlimited
	AsOf           *time.Time `json:"as_of" gorm:"type:date"`
	SourceURL      string     `json:"source_url" gorm:"type:text"`
	Notes          string     `json:"notes" gorm:"type:text"`
	Active         bool       `json:"active"`
	Source         string     `json:"source" gorm:"type:varchar(30)"` // manual | csv | synergy_statements | synergy_api
	// The price before the last change (the full trail is in PriceChange).
	PrevPrice      *float64   `json:"prev_price"`
	PriceChangedAt *time.Time `json:"price_changed_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	// Filled in on read.
	SupplierName string `json:"supplier_name" gorm:"-"`
	SupplierType string `json:"supplier_type" gorm:"-"`
	AgeDays      *int   `json:"age_days" gorm:"-"`
	Stale        bool   `json:"stale" gorm:"-"`
}

// TableName is fixed.
func (Item) TableName() string { return "pricebook_items" }

// PriceChange records the price an item had before it was changed.
type PriceChange struct {
	ID              string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID  string     `json:"-" gorm:"type:varchar(255);not null;index:idx_pbh_item,priority:1"`
	ItemID          string     `json:"item_id" gorm:"type:varchar(64);not null;index:idx_pbh_item,priority:2"`
	Currency        string     `json:"currency" gorm:"type:varchar(3)"`
	Price           float64    `json:"price"` // the old price
	RegularPrice    *float64   `json:"regular_price"`
	SetupFee        float64    `json:"setup_fee"`
	AsOf            *time.Time `json:"as_of" gorm:"type:date"` // the old price's date
	NewPrice        float64    `json:"new_price"`
	NewRegularPrice *float64   `json:"new_regular_price"`
	NewSetupFee     float64    `json:"new_setup_fee"`
	Source          string     `json:"source" gorm:"type:varchar(30)"`
	ChangedBy       string     `json:"changed_by" gorm:"type:varchar(255)"`
	ChangedAt       time.Time  `json:"changed_at"`
}

// TableName is fixed.
func (PriceChange) TableName() string { return "pricebook_price_changes" }

// Scenario is a saved comparison: what was asked, the retail settings and a
// snapshot of the answer at the time.
type Scenario struct {
	ID             string         `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string         `json:"-" gorm:"type:varchar(255);not null;index"`
	Name           string         `json:"name" gorm:"type:varchar(160);not null"`
	Input          map[string]any `json:"input" gorm:"type:text;serializer:json"`
	Snapshot       map[string]any `json:"snapshot" gorm:"type:text;serializer:json"`
	CreatedBy      string         `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// TableName is fixed.
func (Scenario) TableName() string { return "pricebook_scenarios" }

// Tier is a named hosting need.
type Tier struct {
	Key       string  `json:"key"`
	Label     string  `json:"label"`
	MinDiskGB float64 `json:"min_disk_gb"`
	MinSites  int     `json:"min_sites"`
}

// DefaultTiers are the hosting needs offered until an organisation edits them.
func DefaultTiers() []Tier {
	return []Tier{
		{Key: "starter", Label: "Starter", MinDiskGB: 2, MinSites: 1},
		{Key: "standard", Label: "Standard", MinDiskGB: 5, MinSites: 1},
		{Key: "business", Label: "Business", MinDiskGB: 10, MinSites: 1},
	}
}

// Settings holds an organisation's price book options.
type Settings struct {
	OrganizationID string     `json:"-" gorm:"type:varchar(255);primaryKey"`
	Tiers          []Tier     `json:"tiers" gorm:"type:text;serializer:json"`
	SeededAt       *time.Time `json:"seeded_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (Settings) TableName() string { return "pricebook_settings" }

// Migrate creates the tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&Supplier{}, &Item{}, &PriceChange{}, &Scenario{}, &Settings{})
}

var reSpaces = regexp.MustCompile(`\s+`)

// NameKey normalises a name for case-insensitive matching.
func NameKey(s string) string {
	return strings.ToLower(reSpaces.ReplaceAllString(strings.TrimSpace(s), " "))
}

// NormTLD normalises a TLD to lower case with one leading dot:
// "CO.KE", "*.co.ke", "example.co.ke"-style suffixes typed as ".co.ke".
// It returns "" for something that is not a TLD.
func NormTLD(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.TrimPrefix(t, "*")
	t = strings.Trim(t, ". \t")
	if t == "" {
		return ""
	}
	for _, r := range t {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '.' && r != '-' {
			return ""
		}
	}
	if strings.Contains(t, "..") || len(t) > 38 {
		return ""
	}
	return "." + t
}

// CleanURL normalises a web address typed by a person: a missing scheme
// becomes https://, and only http and https links are kept (the links are
// shown as clickable, so "javascript:" and the like are refused). ok is false
// for something that is not a web address.
func CleanURL(raw string) (out string, ok bool) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", true
	}
	if strings.ContainsAny(u, " \t\r\n<>\"") || len(u) > 2000 {
		return "", false
	}
	low := strings.ToLower(u)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		if strings.Contains(u, ":") && !strings.Contains(strings.SplitN(u, ":", 2)[0], ".") {
			// some other scheme (javascript:, data:, ftp: …)
			return "", false
		}
		u = "https://" + u
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || !strings.Contains(p.Host, ".") {
		return "", false
	}
	return u, true
}

// Day truncates a time to its calendar date (UTC midnight).
func Day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Age fills the item's age in days and whether it is stale as of today.
func (it *Item) Age(today time.Time) {
	it.AgeDays, it.Stale = nil, false
	if it.AsOf == nil {
		// No date at all: nobody knows how old the price is.
		it.Stale = true
		return
	}
	d := int(Day(today).Sub(Day(*it.AsOf)).Hours() / 24)
	if d < 0 {
		d = 0
	}
	it.AgeDays = &d
	it.Stale = d > StaleDays
}

// Regular is the price every period after the first costs.
func (it Item) Regular() float64 {
	if it.RegularPrice != nil && *it.RegularPrice > 0 {
		return *it.RegularPrice
	}
	return it.Price
}

// IsPromo reports whether the first period is cheaper than the later ones.
func (it Item) IsPromo() bool {
	return it.RegularPrice != nil && *it.RegularPrice > it.Price+0.0001
}
