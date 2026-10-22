// Package subscriptions bills clients on a subscription model and closes
// (suspends) services that are not paid, with the administrators warned at
// least three days ahead so they can call the client first.
//
//   - A subscription is a customer with one or more catalogue services (the
//     services table, e.g. "Starter hosting" + ".co.ke domain", or a domain
//     alone) at a price per billing period (monthly, quarterly, yearly) in
//     the customer's currency, a start date, payment terms, a grace period,
//     auto-suspend on/off and the hosted site(s) / domain(s) it covers (from
//     the Synergy Wholesale sites data or typed). Status: active | past_due |
//     suspended | cancelled.
//   - Billing engine (engine.go): a scheduler (Postgres advisory lock per
//     organisation, like pkg/recurbills) creates each period's invoice through
//     the invoicing API N days before the period starts (7 for monthly and
//     quarterly, 14 for yearly by default), emails it with the View / Pay link
//     and records it on the subscription. A paid invoice — by any path: a
//     manual payment, the Flutterwave webhook or redirect — moves the next
//     billing date, clears past due and unsuspends.
//   - Dunning (dunning.go): client reminders 3 days before, on and 3 days
//     after the due date and a final notice 2 days before suspension; admin
//     alerts 3 days and 1 day before suspension with one-click actions (give
//     more time, mark as paid, suspend now, don't suspend this time) and a
//     daily digest (pkg/emailprefs category client_billing). Suspension never
//     comes sooner than 3 days after the first admin alert.
//   - Suspension (supplier.go, synergy.go): on the suspension date, hosting
//     at Synergy Wholesale is suspended through its API (hostingSuspendService)
//     when the organisation has the API connected; otherwise (hosting.com, other
//     suppliers, API off) the admins get a "Suspend <domain> manually at
//     <provider>" task and the subscription is marked suspended (manual).
//     Payment unsuspends the same way.
//   - Domains (domains.go): renewal reminders to the admins 30, 14 and 7 days
//     before a linked domain expires (from Synergy domain info when available).
//   - Dashboard (metrics.go): MRR / ARR in the home currency, active, past due,
//     suspended, churned this month, renewals in 30 days, upcoming suspensions.
//   - Synergy sites (sites.go): one-click and bulk subscriptions for the hosted
//     sites matched to customers, priced from the catalogue or the retail helper.
//   - A signed public "Your services" page per customer (portal.go).
package subscriptions

import (
	"time"

	"gorm.io/gorm"
)

// Billing periods.
const (
	PeriodMonthly   = "monthly"
	PeriodQuarterly = "quarterly"
	PeriodYearly    = "yearly"
)

// Periods in display order with their length in months.
var Periods = []struct {
	Key, Label string
	Months     int
}{
	{PeriodMonthly, "Monthly", 1}, {PeriodQuarterly, "Quarterly", 3}, {PeriodYearly, "Yearly", 12},
}

// PeriodMonths is the length of a period in months (0 when unknown).
func PeriodMonths(p string) int {
	for _, x := range Periods {
		if x.Key == p {
			return x.Months
		}
	}
	return 0
}

// PeriodLabel is the readable name of a period.
func PeriodLabel(p string) string {
	for _, x := range Periods {
		if x.Key == p {
			return x.Label
		}
	}
	return p
}

// Line kinds (what a service is).
const (
	KindHosting     = "hosting"
	KindDomain      = "domain"
	KindEmail       = "email"
	KindMaintenance = "maintenance"
	KindOther       = "other"
)

// Kinds in display order.
var Kinds = []struct{ Key, Label string }{
	{KindHosting, "Hosting"}, {KindDomain, "Domain"}, {KindEmail, "Email"}, {KindMaintenance, "Maintenance"}, {KindOther, "Other"},
}

// IsKind reports whether k is a line kind.
func IsKind(k string) bool {
	for _, x := range Kinds {
		if x.Key == k {
			return true
		}
	}
	return false
}

// Subscription statuses.
const (
	StatusActive    = "active"
	StatusPastDue   = "past_due"
	StatusSuspended = "suspended"
	StatusCancelled = "cancelled"
)

// How a suspension was carried out.
const (
	ViaAPI    = "api"    // suspended at Synergy Wholesale through its API
	ViaManual = "manual" // an admin suspends it at the provider (task)
	ViaMixed  = "mixed"  // some services through the API, some by hand
	ViaNone   = "none"   // nothing to suspend at a provider (e.g. a service with no hosting)
)

// Period (invoice) statuses.
const (
	PerGenerating = "generating" // claimed: the invoice is being created
	PerInvoiced   = "invoiced"   // invoice created, not paid
	PerPaid       = "paid"
	PerVoid       = "void" // invoice voided or deleted in invoicing
)

// Supplier keys.
const (
	SupplierSynergy    = "synergy"
	SupplierHostingCom = "hosting_com"
	SupplierOther      = "other"
	SupplierNone       = ""
)

// Subscription is one client's recurring service.
type Subscription struct {
	ID             string `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);not null;index;uniqueIndex:idx_sub_number,priority:1"`
	Number         string `json:"number" gorm:"type:varchar(40);not null;uniqueIndex:idx_sub_number,priority:2"`
	CustomerID     string `json:"customer_id" gorm:"type:varchar(255);not null;index"`
	Name           string `json:"name" gorm:"type:varchar(255)"`
	Currency       string `json:"currency" gorm:"type:varchar(3);not null"`
	Period         string `json:"period" gorm:"type:varchar(16);not null"`

	StartDate  time.Time `json:"start_date" gorm:"type:date;not null"`
	BillingDay int       `json:"billing_day" gorm:"not null"`
	Prorate    bool      `json:"prorate"`
	// AnchorDate starts the first full period; a prorated first period runs
	// from StartDate to AnchorDate.
	AnchorDate time.Time `json:"anchor_date" gorm:"type:date;not null"`
	// NextPeriodStart is the start of the next period to invoice.
	NextPeriodStart time.Time `json:"next_period_start" gorm:"type:date;not null;index"`
	// NextBillingDate is the start of the earliest period not paid yet (the
	// unpaid invoice's period, else the next period to invoice): paying the
	// invoice moves it on.
	NextBillingDate  time.Time  `json:"next_billing_date" gorm:"type:date;index"`
	PaidThrough      *time.Time `json:"paid_through" gorm:"type:date"`
	PaymentTermsDays int        `json:"payment_terms_days"` // due = period start + terms
	LeadDays         *int       `json:"lead_days"`          // nil = the organisation's default for the period
	AutoSend         bool       `json:"auto_send"`
	GraceDays        *int       `json:"grace_days"` // suspended this many days after the due date; nil = the organisation's
	AutoSuspend      bool       `json:"auto_suspend"`
	EndDate          *time.Time `json:"end_date" gorm:"type:date"` // last day of service (cancel at the end of the term)

	Status       string     `json:"status" gorm:"type:varchar(16);not null;index"`
	SuspendedAt  *time.Time `json:"suspended_at" gorm:"type:date"`
	SuspendedVia string     `json:"suspended_via" gorm:"type:varchar(10)"` // api | manual | mixed | none
	CancelledAt  *time.Time `json:"cancelled_at" gorm:"type:date"`
	CancelReason string     `json:"cancel_reason" gorm:"type:varchar(32)"` // admin | end_of_term

	// The service it covers: the hosted site (hosting), the registered
	// domain, more sites / domains, and where they are hosted.
	SiteDomain   string     `json:"site_domain" gorm:"type:varchar(255);index"`
	Domain       string     `json:"domain" gorm:"type:varchar(255)"`
	Domains      []string   `json:"domains" gorm:"type:text;serializer:json"` // more hosted sites / domains
	Supplier     string     `json:"supplier" gorm:"type:varchar(20)"`         // synergy | hosting_com | other | ""
	SupplierPlan string     `json:"supplier_plan" gorm:"type:varchar(200)"`
	DomainExpiry *time.Time `json:"domain_expiry" gorm:"type:date"` // typed; Synergy's domain info wins when available

	// CostMonthly overrides what the service costs per month (home currency).
	CostMonthly *float64  `json:"cost_monthly"`
	Notes       string    `json:"notes" gorm:"type:text"`
	Source      string    `json:"source" gorm:"type:varchar(20)"` // "" | csv | synergy
	CreatedBy   string    `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	Lines []Line `json:"lines" gorm:"foreignKey:SubscriptionID;constraint:OnDelete:CASCADE"`

	// Filled in on read.
	CustomerName  string      `json:"customer_name" gorm:"-"`
	CustomerEmail string      `json:"customer_email" gorm:"-"`
	CustomerPhone string      `json:"customer_phone,omitempty" gorm:"-"`
	Amount        float64     `json:"amount" gorm:"-"`       // per period, before tax, subscription currency
	AmountTax     float64     `json:"amount_tax" gorm:"-"`   // per period incl. tax
	MonthlyHome   *float64    `json:"monthly_home" gorm:"-"` // MRR contribution (before tax)
	HomeCurrency  string      `json:"home_currency" gorm:"-"`
	NextDueDate   string      `json:"next_due_date,omitempty" gorm:"-"`
	NextInvoiceOn string      `json:"next_invoice_date,omitempty" gorm:"-"`
	Lead          int         `json:"lead" gorm:"-"`
	Grace         int         `json:"grace" gorm:"-"`
	Open          *OpenPeriod `json:"open,omitempty" gorm:"-"` // the oldest unpaid invoice and its dunning dates
	Cost          *CostView   `json:"cost,omitempty" gorm:"-"`
	Margin        *float64    `json:"margin,omitempty" gorm:"-"`
	MarginPct     *float64    `json:"margin_pct,omitempty" gorm:"-"`
	OpenTasks     int         `json:"open_tasks" gorm:"-"`
	Summary       string      `json:"summary,omitempty" gorm:"-"`
}

// TableName is fixed.
func (Subscription) TableName() string { return "client_subscriptions" }

// Line is one catalogue service (or other item) on a subscription.
type Line struct {
	ID             string  `json:"id" gorm:"type:varchar(64);primaryKey"`
	SubscriptionID string  `json:"subscription_id" gorm:"type:varchar(64);not null;index"`
	OrganizationID string  `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Position       int     `json:"position"`
	ServiceID      string  `json:"service_id" gorm:"type:varchar(255);index"` // the catalogue service ("" = a typed item)
	Kind           string  `json:"kind" gorm:"type:varchar(20)"`
	Description    string  `json:"description" gorm:"type:varchar(500)"`
	Quantity       float64 `json:"quantity"`
	// UnitPrice per period in the subscription currency; nil follows the
	// catalogue service's price (worked out for the period, converted on each
	// invoice at its locked rate).
	UnitPrice *float64 `json:"unit_price"`
	TaxRate   float64  `json:"tax_rate"`

	// Filled in on read.
	ServiceName   string  `json:"service_name,omitempty" gorm:"-"`
	Price         float64 `json:"price" gorm:"-"`          // per period, per unit (in PriceCurrency)
	PriceCurrency string  `json:"price_currency" gorm:"-"` // the currency of Price
	FromCatalog   bool    `json:"from_catalog" gorm:"-"`   // follows the catalogue price
	Missing       bool    `json:"missing,omitempty" gorm:"-"`
}

// TableName is fixed.
func (Line) TableName() string { return "client_subscription_lines" }

// BillingPeriod is one invoiced period of a subscription. The unique
// (subscription, start) key makes every period invoiced once.
type BillingPeriod struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	SubscriptionID string     `json:"subscription_id" gorm:"type:varchar(64);not null;uniqueIndex:idx_subper_start,priority:1"`
	Start          time.Time  `json:"start" gorm:"column:period_start;type:date;not null;uniqueIndex:idx_subper_start,priority:2"`
	End            time.Time  `json:"end" gorm:"column:period_end;type:date;not null"` // exclusive
	Seq            int        `json:"seq"`
	Partial        bool       `json:"partial"`
	Fraction       float64    `json:"fraction"` // share of a full period (proration)
	IssueDate      time.Time  `json:"issue_date" gorm:"type:date"`
	DueDate        time.Time  `json:"due_date" gorm:"type:date;index"`
	InvoiceID      string     `json:"invoice_id" gorm:"type:varchar(64);index"`
	InvoiceNumber  string     `json:"invoice_number" gorm:"type:varchar(50)"`
	Status         string     `json:"status" gorm:"type:varchar(16);not null;index"`
	Amount         float64    `json:"amount"` // invoice total
	Currency       string     `json:"currency" gorm:"type:varchar(3)"`
	Balance        float64    `json:"balance"`
	Sent           bool       `json:"sent"`
	PaidAt         *time.Time `json:"paid_at"`
	// ExtraDays: more time given on this invoice (moves the suspension).
	ExtraDays int `json:"extra_days"`
	// NoSuspend: "don't suspend this time" — this invoice never suspends the service.
	NoSuspend bool      `json:"no_suspend"`
	Error     string    `json:"error" gorm:"type:text"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName is fixed.
func (BillingPeriod) TableName() string { return "client_subscription_periods" }

// Task kinds.
const (
	TaskSuspend = "suspend"
	TaskRestore = "restore"
	TaskCancel  = "cancel"
)

// Task statuses.
const (
	TaskOpen      = "open"
	TaskDone      = "done"
	TaskDismissed = "dismissed"
)

// Task is something done at the provider for one hosted site or domain:
// suspend, restore (unsuspend) or cancel. Automated tasks were done through
// the Synergy Wholesale API; the others are for an administrator.
type Task struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);not null;index;uniqueIndex:idx_subtask_key,priority:1"`
	Key            string     `json:"key" gorm:"type:varchar(255);not null;uniqueIndex:idx_subtask_key,priority:2"`
	SubscriptionID string     `json:"subscription_id" gorm:"type:varchar(64);not null;index"`
	PeriodID       string     `json:"period_id" gorm:"type:varchar(64)"`
	Kind           string     `json:"kind" gorm:"type:varchar(16);not null"`
	Status         string     `json:"status" gorm:"type:varchar(16);not null;index"`
	Title          string     `json:"title" gorm:"type:varchar(500)"`
	Service        string     `json:"service" gorm:"type:varchar(500)"`
	Supplier       string     `json:"supplier" gorm:"type:varchar(20)"`
	SupplierPlan   string     `json:"supplier_plan" gorm:"type:varchar(200)"`
	Domain         string     `json:"domain" gorm:"type:varchar(255)"`
	Target         string     `json:"target" gorm:"type:varchar(16)"`       // hosting | domain | service
	ProviderRef    string     `json:"provider_ref" gorm:"type:varchar(64)"` // the provider's id (Synergy hosting ID)
	Steps          []string   `json:"steps" gorm:"type:text;serializer:json"`
	DueDate        time.Time  `json:"due_date" gorm:"type:date"`
	DoneAt         *time.Time `json:"done_at"`
	DoneBy         string     `json:"done_by" gorm:"type:varchar(255)"`
	Note           string     `json:"note" gorm:"type:varchar(1000)"`
	Automated      bool       `json:"automated"` // done through the Synergy Wholesale API
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	CustomerName       string `json:"customer_name,omitempty" gorm:"-"`
	SubscriptionNumber string `json:"subscription_number,omitempty" gorm:"-"`
}

// TableName is fixed.
func (Task) TableName() string { return "client_subscription_tasks" }

// Event audiences.
const (
	ToClient = "client"
	ToAdmin  = "admin"
)

// Event is one thing that happened (an email sent, attempted or skipped, a
// suspension, an admin action). The unique key makes every step happen once.
type Event struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);not null;uniqueIndex:idx_subevt_key,priority:1;index"`
	Key            string    `json:"key" gorm:"type:varchar(255);not null;uniqueIndex:idx_subevt_key,priority:2"`
	SubscriptionID string    `json:"subscription_id" gorm:"type:varchar(64);index"`
	PeriodID       string    `json:"period_id" gorm:"type:varchar(64);index"`
	Stage          string    `json:"stage" gorm:"type:varchar(40)"`
	Audience       string    `json:"audience" gorm:"type:varchar(10)"` // client | admin
	Day            time.Time `json:"day" gorm:"type:date"`             // the subscription clock's date
	Subject        string    `json:"subject" gorm:"type:varchar(500)"`
	Recipients     string    `json:"recipients" gorm:"type:text"`
	Status         string    `json:"status" gorm:"type:varchar(16);not null"` // sending | sent | failed | skipped | done
	Error          string    `json:"error" gorm:"type:text"`
	Attempts       int       `json:"attempts"`
	InApp          int       `json:"in_app"`
	CreatedBy      string    `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Event) TableName() string { return "client_subscription_events" }

// PlanMap remembers which catalogue service (and price) a Synergy hosting
// plan is sold as, so the next site on that plan is one click.
type PlanMap struct {
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	PlanKey        string    `json:"plan_key" gorm:"type:varchar(200);primaryKey"` // lower-case Synergy plan name
	Plan           string    `json:"plan" gorm:"type:varchar(200)"`
	ServiceID      string    `json:"service_id" gorm:"type:varchar(255)"`
	Period         string    `json:"period" gorm:"type:varchar(16)"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (PlanMap) TableName() string { return "client_subscription_plan_map" }

// MinNoticeDays: the administrators always hear about a suspension at least
// this many days before it happens (the suspension waits when they didn't).
const MinNoticeDays = 3

// Settings are an organisation's billing and dunning settings.
type Settings struct {
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	Enabled        bool   `json:"enabled"` // billing engine on
	// Invoices are created this many days before the period starts.
	LeadMonthly   int  `json:"lead_monthly"`
	LeadQuarterly int  `json:"lead_quarterly"`
	LeadYearly    int  `json:"lead_yearly"`
	AutoSend      bool `json:"auto_send"`    // default for new subscriptions
	GraceDays     int  `json:"grace_days"`   // default for new subscriptions: suspended this many days after the due date
	AutoSuspend   bool `json:"auto_suspend"` // default for new subscriptions
	// Client reminders: days relative to the due date (−3 = three days before).
	ClientDays []int `json:"client_days" gorm:"type:text;serializer:json"`
	// FinalNoticeDays: "service will be suspended on …" this many days before suspension (0 = off).
	FinalNoticeDays int `json:"final_notice_days"`
	// AdminDays: admin alerts this many days before suspension (the earliest is at least MinNoticeDays).
	AdminDays  []int `json:"admin_days" gorm:"type:text;serializer:json"`
	Digest     bool  `json:"digest"`
	DigestHour int   `json:"digest_hour"`
	SendHour   int   `json:"send_hour"`
	InApp      bool  `json:"in_app"`
	// DomainDays: admin reminders this many days before a linked domain expires.
	DomainDays []int    `json:"domain_days" gorm:"type:text;serializer:json"`
	Recipients []string `json:"recipients" gorm:"type:text;serializer:json"` // extra admin addresses
	// UseSynergyAPI: suspend / unsuspend Synergy Wholesale hosting through the API when connected.
	UseSynergyAPI bool `json:"use_synergy_api"`
	// TargetMargin: suggested prices for Synergy sites with no catalogue service (margin % on the Synergy cost).
	TargetMargin   float64    `json:"target_margin"`
	NumberPrefix   string     `json:"number_prefix" gorm:"type:varchar(12)"`
	NextNumber     int        `json:"next_number"`
	TestOffsetDays int        `json:"test_offset_days"` // SUBSCRIPTIONS_TEST_CLOCK only
	LastRunAt      *time.Time `json:"last_run_at"`
	LastDigest     string     `json:"last_digest" gorm:"type:varchar(16)"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (Settings) TableName() string { return "subscription_settings" }

// DefaultSettings for an organisation that never saved any.
func DefaultSettings(org string) Settings {
	return Settings{OrganizationID: org, Enabled: true, LeadMonthly: 7, LeadQuarterly: 7, LeadYearly: 14, AutoSend: true, GraceDays: 7, AutoSuspend: true,
		ClientDays: []int{-3, 0, 3}, FinalNoticeDays: 2, AdminDays: []int{3, 1}, Digest: true, DigestHour: 8, SendHour: 8, InApp: true,
		DomainDays: []int{30, 14, 7}, Recipients: []string{}, UseSynergyAPI: true, TargetMargin: 50, NumberPrefix: "SUB-", NextNumber: 1}
}

// Models lists the tables for AutoMigrate.
func Models() []interface{} {
	return []interface{}{&Subscription{}, &Line{}, &BillingPeriod{}, &Task{}, &Event{}, &Settings{}, &PlanMap{}}
}

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }
