package expenses

import "time"

// SynergyTxn is one Synergy Wholesale statement line (synergy_transactions).
// Unique per organisation by DedupeKey (date + type + description + amounts +
// balance), so re-uploading a statement or overlapping months never duplicates.
type SynergyTxn struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_syt_key,priority:1;index:idx_syt_org_date,priority:1"`
	DedupeKey      string     `json:"-" gorm:"type:varchar(64);not null;uniqueIndex:idx_syt_key,priority:2"`
	Date           time.Time  `json:"date" gorm:"not null;index:idx_syt_org_date,priority:2"`
	Seq            int        `json:"seq"`
	ProductType    string     `json:"product_type" gorm:"type:varchar(80)"`
	Description    string     `json:"description" gorm:"type:text"`
	Domain         string     `json:"domain" gorm:"type:varchar(255);index"`
	Kind           string     `json:"kind" gorm:"type:varchar(32);index"`
	Category       string     `json:"category" gorm:"type:varchar(32)"`
	PeriodFrom     *time.Time `json:"period_from" gorm:"type:date"`
	PeriodTo       *time.Time `json:"period_to" gorm:"type:date"`
	PlanFrom       string     `json:"plan_from" gorm:"type:varchar(120)"`
	PlanTo         string     `json:"plan_to" gorm:"type:varchar(120)"`
	Credit         float64    `json:"credit"`
	Debit          float64    `json:"debit"`
	Amount         float64    `json:"amount"` // debit − credit (AUD): cost positive, refund negative; top-ups negative but never an expense
	Balance        float64    `json:"balance"`
	HasBalance     bool       `json:"has_balance"`
	Month          string     `json:"month" gorm:"type:varchar(7);index"`         // charge month (cash basis)
	ServiceMonth   string     `json:"service_month" gorm:"type:varchar(7);index"` // month of the service period (accrual), else the charge month
	UploadID       string     `json:"upload_id" gorm:"type:varchar(64)"`
	CreatedAt      time.Time  `json:"created_at"`
}

// TableName is fixed.
func (SynergyTxn) TableName() string { return "synergy_transactions" }

// Row converts back to the parser's shape.
func (t SynergyTxn) Row() SynRow {
	return SynRow{Seq: t.Seq, Date: t.Date, ProductType: t.ProductType, Description: t.Description, Credit: t.Credit, Debit: t.Debit, Balance: t.Balance,
		HasBalance: t.HasBalance, Kind: t.Kind, Category: t.Category, Domain: t.Domain, PeriodFrom: t.PeriodFrom, PeriodTo: t.PeriodTo, PlanFrom: t.PlanFrom, PlanTo: t.PlanTo}
}

// SynergyUsage is one hosting service in a usage report (synergy_usage_snapshots),
// unique per organisation, snapshot date and domain.
type SynergyUsage struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_syu_key,priority:1"`
	SnapshotDate   time.Time `json:"snapshot_date" gorm:"type:date;not null;uniqueIndex:idx_syu_key,priority:2"`
	Domain         string    `json:"domain" gorm:"type:varchar(255);not null;uniqueIndex:idx_syu_key,priority:3"`
	Status         string    `json:"status" gorm:"type:varchar(40)"`
	Plan           string    `json:"plan" gorm:"type:varchar(120)"`
	UsageMB        float64   `json:"usage_mb"`
	LimitMB        float64   `json:"limit_mb"`
	PctUsed        float64   `json:"pct_used"`
	At75           bool      `json:"at_75" gorm:"column:at_75"`
	At100MB        bool      `json:"at_100mb" gorm:"column:at_100mb"`
	RemainingMB    float64   `json:"remaining_mb"`
	UploadID       string    `json:"upload_id" gorm:"type:varchar(64)"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (SynergyUsage) TableName() string { return "synergy_usage_snapshots" }

// SynergyUpload records one uploaded Synergy file and what it added.
type SynergyUpload struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;index"`
	Filename       string    `json:"filename" gorm:"type:varchar(255)"`
	SHA256         string    `json:"sha256" gorm:"type:varchar(64)"`
	FileType       string    `json:"file_type" gorm:"type:varchar(20)"` // statement | usage
	DocumentID     string    `json:"document_id" gorm:"type:varchar(64)"`
	SnapshotDate   string    `json:"snapshot_date" gorm:"type:varchar(10)"`
	PeriodFrom     string    `json:"period_from" gorm:"type:varchar(10)"`
	PeriodTo       string    `json:"period_to" gorm:"type:varchar(10)"`
	RowsRead       int       `json:"rows_read"`
	New            int       `json:"new"`
	Duplicates     int       `json:"duplicates"`
	Updated        int       `json:"updated"`
	Opening        *float64  `json:"opening_balance"`
	Closing        *float64  `json:"closing_balance"`
	Reconciled     bool      `json:"reconciled"`
	Warnings       []string  `json:"warnings" gorm:"type:text;serializer:json"`
	CreatedBy      string    `json:"created_by" gorm:"type:varchar(255)"`
	CreatedAt      time.Time `json:"created_at"`
}

// TableName is fixed.
func (SynergyUpload) TableName() string { return "synergy_uploads" }

// SynergySite holds what the user set for one hosted domain: the client it
// belongs to and what they are billed (overrides the automatic match).
type SynergySite struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_sys_key,priority:1"`
	Domain         string    `json:"domain" gorm:"type:varchar(255);not null;uniqueIndex:idx_sys_key,priority:2"`
	CustomerID     string    `json:"customer_id" gorm:"type:varchar(255)"` // "" = automatic match
	NoClient       bool      `json:"no_client"`                            // explicitly "not a client site" (own / internal)
	Price          *float64  `json:"price"`                                // monthly price billed to the client (overrides invoices)
	PriceCurrency  string    `json:"price_currency" gorm:"type:varchar(3)"`
	Notes          string    `json:"notes" gorm:"type:text"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (SynergySite) TableName() string { return "synergy_sites" }

// SynergyPlan is a hosting plan's monthly price / disk limit set by hand
// (learned values come from the statements and usage reports).
type SynergyPlan struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_syp_key,priority:1"`
	Key            string    `json:"key" gorm:"type:varchar(120);not null;uniqueIndex:idx_syp_key,priority:2"` // lower-case name
	Name           string    `json:"name" gorm:"type:varchar(120)"`
	MonthlyPrice   *float64  `json:"monthly_price"`
	LimitMB        *float64  `json:"limit_mb"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (SynergyPlan) TableName() string { return "synergy_plans" }

// SynergyModels lists the Synergy analytics tables.
func SynergyModels() []interface{} {
	return []interface{}{&SynergyTxn{}, &SynergyUsage{}, &SynergyUpload{}, &SynergySite{}, &SynergyPlan{}}
}

// StatementLine is one line of an expense created from Synergy statements
// (one domain or one fee, in AUD as charged, credits netted).
type StatementLine struct {
	Domain     string  `json:"domain"`
	Label      string  `json:"label"`
	Amount     float64 `json:"amount"`      // AUD incl. GST
	ChargeDate string  `json:"charge_date"` // latest charge date of this line (YYYY-MM-DD)
	ChargeMon  string  `json:"charge_month"`
	Count      int     `json:"count"`
	// ByMonth: the line's charges by charge month (AUD, before netting other
	// domains' credits) — the bill gets a payment per charge month.
	ByMonth   map[string]float64 `json:"by_month,omitempty"`
	LastByMon map[string]string  `json:"last_by_month,omitempty"`
	Credits   float64            `json:"credits,omitempty"` // credits netted into this line (AUD, positive)
}
