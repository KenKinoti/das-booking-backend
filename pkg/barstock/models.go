// Package barstock is club / bar stock for DASYIN ERP (nightclubs, bars,
// lounges): bottles and tots, stores and bars with transfers, deliveries
// with weighted average cost, wastage with approvals, stock takes with
// variance per item / category / location / shift, reorders into the
// existing purchase orders and POS recipe deduction (see hooks.go).
//
// The contract with the frontend and the business-type agents is
// /home/claude/tools/BARSTOCK_SPEC.md (v2.11.0).
//
// Data model (all tables organisation-scoped, quantities numeric(14,3) in
// BASE units — ml for poured liquids, units for counted items):
//
//	bar_settings        one row per organisation (tot size, day cutoff, thresholds, night report …)
//	bar_items           a products row flagged as a stock item (sizes, packs, weights, avg cost)
//	bar_stock           qty per (location, product)
//	bar_movements       the ledger; every stock change writes one row (kind, ref, unit cost, business date)
//	bar_recipes(+lines) POS product → stock lines (a stock item sold as itself is an implicit 1:1 recipe)
//	bar_par             par level and reorder point per (location, product), in stock units
//	bar_location_staff  optional staff → location assignment
//	bar_terminals       POS terminal → bar
//	bar_price_history   unit cost per supplier and date
//	bar_deliveries(+lines), bar_transfers(+lines), bar_wastage, bar_counts(+lines)
//	bar_sale_lines      one row per bar-managed POS line: net ex VAT, cost, category, business date
//	bar_night_reports   night report sends
package barstock

import (
	"time"

	"gorm.io/gorm"
)

// Base units.
const (
	BaseML   = "ml"
	BaseUnit = "unit"
)

// Movement kinds.
const (
	KindReceive        = "receive"
	KindTransferOut    = "transfer_out"
	KindTransferIn     = "transfer_in"
	KindSale           = "sale"
	KindSaleVoid       = "sale_void"
	KindWastage        = "wastage"
	KindCountAdjust    = "count_adjust"
	KindOpening        = "opening"
	KindReturnSupplier = "return_supplier"
	KindManualAdjust   = "manual_adjust"
)

// MovementKinds lists every ledger kind.
var MovementKinds = []string{KindReceive, KindTransferOut, KindTransferIn, KindSale, KindSaleVoid, KindWastage,
	KindCountAdjust, KindOpening, KindReturnSupplier, KindManualAdjust}

// Categories of stock items.
var Categories = []string{"spirits", "wine", "beer", "cider", "soft_drinks", "mixers", "water", "energy", "garnish", "consumables", "food", "other"}

// CategoryLabels are readable category names.
var CategoryLabels = map[string]string{
	"spirits": "Spirits", "wine": "Wine", "beer": "Beer", "cider": "Cider", "soft_drinks": "Soft drinks", "mixers": "Mixers",
	"water": "Water", "energy": "Energy drinks", "garnish": "Garnish", "consumables": "Consumables", "food": "Food", "other": "Other",
}

// Location types.
var LocationTypes = []string{"store", "bar", "kitchen", "event", "other"}

// Wastage reasons.
var WastageReasons = []string{"breakage", "spillage", "comp", "staff_drink", "expired", "damaged_on_delivery", "returned_to_supplier", "other"}

// Settings is an organisation's bar stock configuration.
type Settings struct {
	OrganizationID          string              `json:"-" gorm:"type:varchar(255);primaryKey"`
	TotML                   float64             `json:"tot_ml" gorm:"type:numeric(14,3)"`
	DayCutoff               string              `json:"day_cutoff" gorm:"type:varchar(5)"`
	BlockWhenOut            bool                `json:"block_when_out"`
	VariancePctThreshold    float64             `json:"variance_pct_threshold" gorm:"type:numeric(8,2)"`
	VarianceAmountThreshold float64             `json:"variance_amount_threshold" gorm:"type:numeric(16,4)"`
	CompAllowancePerShift   float64             `json:"comp_allowance_per_shift" gorm:"type:numeric(10,2)"`
	PourCostTargets         map[string]*float64 `json:"pour_cost_targets" gorm:"type:text;serializer:json"`
	NightReportEnabled      bool                `json:"-"`
	NightReportSendAt       string              `json:"-" gorm:"type:varchar(5)"`
	DefaultBarID            *string             `json:"default_bar_id" gorm:"type:varchar(255)"`
	MainStoreID             *string             `json:"main_store_id" gorm:"type:varchar(255)"`
	UpdatedBy               string              `json:"-" gorm:"type:varchar(255)"`
	CreatedAt               time.Time           `json:"-"`
	UpdatedAt               time.Time           `json:"updated_at"`
}

func (Settings) TableName() string { return "bar_settings" }

// Item flags a products row as a bar stock item.
type Item struct {
	ProductID           string   `gorm:"type:varchar(255);primaryKey"`
	OrganizationID      string   `gorm:"type:varchar(255);not null;index"`
	BaseUnit            string   `gorm:"type:varchar(10)"`
	UnitSize            float64  `gorm:"type:numeric(14,3)"`
	UnitLabel           string   `gorm:"type:varchar(30)"`
	PackSize            int      `gorm:"type:integer"`
	PackLabel           string   `gorm:"type:varchar(30)"`
	TotML               *float64 `gorm:"type:numeric(14,3)"`
	FullWeightG         *float64 `gorm:"type:numeric(12,1)"`
	EmptyWeightG        *float64 `gorm:"type:numeric(12,1)"`
	Category            string   `gorm:"type:varchar(30);index"`
	PreferredSupplierID *string  `gorm:"type:varchar(255);index"`
	Track               bool     `gorm:"type:boolean"`
	Active              bool     `gorm:"type:boolean"`
	AvgCost             float64  `gorm:"type:numeric(18,6)"` // per base unit, home currency
	LastCost            float64  `gorm:"type:numeric(16,4)"` // per stock unit
	LastSupplierID      *string  `gorm:"type:varchar(255)"`
	CatalogKey          string   `gorm:"type:varchar(60);index"`
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func (Item) TableName() string { return "bar_items" }

// Stock is the quantity of one product at one location (base units).
type Stock struct {
	OrganizationID string  `gorm:"type:varchar(255);primaryKey"`
	LocationID     string  `gorm:"type:varchar(255);primaryKey"`
	ProductID      string  `gorm:"type:varchar(255);primaryKey;index"`
	Qty            float64 `gorm:"type:numeric(14,3)"`
	UpdatedAt      time.Time
}

func (Stock) TableName() string { return "bar_stock" }

// Movement is one ledger row. Qty is signed (base units).
type Movement struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;index:idx_bar_mv_org_loc_prod,priority:1"`
	LocationID     string    `json:"location_id" gorm:"type:varchar(255);index:idx_bar_mv_org_loc_prod,priority:2"`
	ProductID      string    `json:"product_id" gorm:"type:varchar(255);index:idx_bar_mv_org_loc_prod,priority:3"`
	Qty            float64   `json:"qty" gorm:"type:numeric(14,3)"`
	Kind           string    `json:"kind" gorm:"type:varchar(20);index"`
	RefType        string    `json:"ref_type" gorm:"type:varchar(30)"`
	RefID          string    `json:"ref_id" gorm:"type:varchar(255);index"`
	RefLabel       string    `json:"ref_label" gorm:"type:varchar(80)"`
	UnitCost       float64   `json:"unit_cost" gorm:"type:numeric(18,6)"` // per base unit
	UserID         string    `json:"user_id" gorm:"type:varchar(255)"`
	BusinessDate   string    `json:"business_date" gorm:"type:varchar(10);index"`
	Note           string    `json:"note" gorm:"type:text"`
	Oversold       bool      `json:"oversold"`
	CreatedAt      time.Time `json:"created_at" gorm:"index"`
}

func (Movement) TableName() string { return "bar_movements" }

// Recipe maps a POS product to stock lines.
type Recipe struct {
	POSProductID   string `gorm:"type:varchar(255);primaryKey"`
	OrganizationID string `gorm:"type:varchar(255);not null;index"`
	Kind           string `gorm:"type:varchar(20)"` // tot | bottle | self | custom
	SourceID       string `gorm:"type:varchar(255);index"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (Recipe) TableName() string { return "bar_recipes" }

// RecipeLine is one stock line of a recipe (qty in base units).
type RecipeLine struct {
	ID             string  `gorm:"type:varchar(36);primaryKey"`
	OrganizationID string  `gorm:"type:varchar(255);not null;index"`
	POSProductID   string  `gorm:"type:varchar(255);index"`
	ProductID      string  `gorm:"type:varchar(255);index"`
	Qty            float64 `gorm:"type:numeric(14,3)"`
	Unit           string  `gorm:"type:varchar(10)"`
	EnteredQty     float64 `gorm:"type:numeric(14,3)"`
	Sort           int
}

func (RecipeLine) TableName() string { return "bar_recipe_lines" }

// Par is the par level and reorder point of a product at a location (stock units).
type Par struct {
	OrganizationID string  `gorm:"type:varchar(255);primaryKey"`
	LocationID     string  `gorm:"type:varchar(255);primaryKey"`
	ProductID      string  `gorm:"type:varchar(255);primaryKey"`
	Par            float64 `gorm:"type:numeric(14,3)"`
	ReorderPoint   float64 `gorm:"type:numeric(14,3)"`
	UpdatedAt      time.Time
}

func (Par) TableName() string { return "bar_par" }

// LocationStaff limits a user to locations (optional).
type LocationStaff struct {
	OrganizationID string `gorm:"type:varchar(255);not null;index"`
	LocationID     string `gorm:"type:varchar(255);primaryKey"`
	UserID         string `gorm:"type:varchar(255);primaryKey;index"`
}

func (LocationStaff) TableName() string { return "bar_location_staff" }

// Terminal maps a POS terminal to the bar it sells from.
type Terminal struct {
	OrganizationID string `gorm:"type:varchar(255);primaryKey"`
	TerminalID     string `gorm:"type:varchar(50);primaryKey"`
	LocationID     string `gorm:"type:varchar(255)"`
	UpdatedAt      time.Time
}

func (Terminal) TableName() string { return "bar_terminals" }

// PriceHistory is a unit cost paid (per stock unit).
type PriceHistory struct {
	ID             string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;index:idx_bar_ph_org_prod,priority:1"`
	ProductID      string    `json:"product_id" gorm:"type:varchar(255);index:idx_bar_ph_org_prod,priority:2"`
	SupplierID     string    `json:"supplier_id" gorm:"type:varchar(255);index"`
	UnitCost       float64   `json:"unit_cost" gorm:"type:numeric(16,4)"`
	Currency       string    `json:"currency" gorm:"type:varchar(3)"`
	Date           string    `json:"date" gorm:"type:varchar(10)"`
	Source         string    `json:"source" gorm:"type:varchar(20)"` // delivery | purchase_order
	RefID          string    `json:"ref_id" gorm:"type:varchar(255)"`
	CreatedAt      time.Time `json:"created_at"`
}

func (PriceHistory) TableName() string { return "bar_price_history" }

// Delivery is goods received (with or without a purchase order).
type Delivery struct {
	ID             string    `gorm:"type:varchar(36);primaryKey"`
	OrganizationID string    `gorm:"type:varchar(255);not null;index"`
	SupplierID     string    `gorm:"type:varchar(255);index"`
	POID           *string   `gorm:"type:varchar(255);index"`
	DeliveryNote   string    `gorm:"type:varchar(80)"`
	LocationID     string    `gorm:"type:varchar(255)"`
	Date           string    `gorm:"type:varchar(10)"`
	BusinessDate   string    `gorm:"type:varchar(10)"`
	Note           string    `gorm:"type:text"`
	TotalCost      float64   `gorm:"type:numeric(16,4)"`
	Currency       string    `gorm:"type:varchar(3)"`
	CreatedBy      string    `gorm:"type:varchar(255)"`
	CreatedAt      time.Time `gorm:"index"`
}

func (Delivery) TableName() string { return "bar_deliveries" }

// DeliveryLine is one product received (stock units).
type DeliveryLine struct {
	ID             string  `gorm:"type:varchar(36);primaryKey"`
	OrganizationID string  `gorm:"type:varchar(255);not null;index"`
	DeliveryID     string  `gorm:"type:varchar(36);index"`
	ProductID      string  `gorm:"type:varchar(255)"`
	Unit           string  `gorm:"type:varchar(10)"`
	EnteredQty     float64 `gorm:"type:numeric(14,3)"`
	QtyUnits       float64 `gorm:"type:numeric(14,3)"`
	UnitCost       float64 `gorm:"type:numeric(16,4)"` // per stock unit
	Damaged        float64 `gorm:"type:numeric(14,3)"` // stock units
	LineTotal      float64 `gorm:"type:numeric(16,4)"`
}

func (DeliveryLine) TableName() string { return "bar_delivery_lines" }

// Transfer statuses.
const (
	TrRequested = "requested"
	TrIssued    = "issued"
	TrReceived  = "received"
	TrCancelled = "cancelled"
)

// Transfer moves stock between locations (request → issue → receive, or direct).
type Transfer struct {
	ID             string `gorm:"type:varchar(36);primaryKey"`
	OrganizationID string `gorm:"type:varchar(255);not null;uniqueIndex:idx_bar_tr_num,priority:1"`
	Number         string `gorm:"type:varchar(20);uniqueIndex:idx_bar_tr_num,priority:2"`
	FromLocationID string `gorm:"type:varchar(255);index"`
	ToLocationID   string `gorm:"type:varchar(255);index"`
	Mode           string `gorm:"type:varchar(10)"`
	Status         string `gorm:"type:varchar(12);index"`
	Note           string `gorm:"type:text"`
	BusinessDate   string `gorm:"type:varchar(10)"`
	HasDiscrepancy bool   `gorm:"type:boolean"`
	RequestedBy    string `gorm:"type:varchar(255)"`
	IssuedBy       string `gorm:"type:varchar(255)"`
	ReceivedBy     string `gorm:"type:varchar(255)"`
	CancelledBy    string `gorm:"type:varchar(255)"`
	IssuedAt       *time.Time
	ReceivedAt     *time.Time
	CancelledAt    *time.Time
	CreatedAt      time.Time `gorm:"index"`
	UpdatedAt      time.Time
}

func (Transfer) TableName() string { return "bar_transfers" }

// TransferLine quantities are base units.
type TransferLine struct {
	ID             string   `gorm:"type:varchar(36);primaryKey"`
	OrganizationID string   `gorm:"type:varchar(255);not null;index"`
	TransferID     string   `gorm:"type:varchar(36);index"`
	ProductID      string   `gorm:"type:varchar(255)"`
	Unit           string   `gorm:"type:varchar(10)"` // unit the request was entered in
	QtyRequested   float64  `gorm:"type:numeric(14,3)"`
	QtyIssued      *float64 `gorm:"type:numeric(14,3)"`
	QtyReceived    *float64 `gorm:"type:numeric(14,3)"`
	Discrepancy    float64  `gorm:"type:numeric(14,3)"` // issued − received
	UnitCost       float64  `gorm:"type:numeric(18,6)"`
	Sort           int
}

func (TransferLine) TableName() string { return "bar_transfer_lines" }

// Wastage statuses.
const (
	WPending  = "pending"
	WApproved = "approved"
	WRejected = "rejected"
)

// Wastage is a loss entry (base units).
type Wastage struct {
	ID             string  `gorm:"type:varchar(36);primaryKey"`
	OrganizationID string  `gorm:"type:varchar(255);not null;index"`
	LocationID     string  `gorm:"type:varchar(255);index"`
	ProductID      string  `gorm:"type:varchar(255);index"`
	Qty            float64 `gorm:"type:numeric(14,3)"`
	Unit           string  `gorm:"type:varchar(10)"`
	EnteredQty     float64 `gorm:"type:numeric(14,3)"`
	Reason         string  `gorm:"type:varchar(30);index"`
	Note           string  `gorm:"type:text"`
	StaffID        string  `gorm:"type:varchar(255)"`
	Status         string  `gorm:"type:varchar(10);index"`
	BusinessDate   string  `gorm:"type:varchar(10);index"`
	UnitCost       float64 `gorm:"type:numeric(18,6)"`
	Servings       float64 `gorm:"type:numeric(14,3)"`
	RecordedBy     string  `gorm:"type:varchar(255)"`
	DecidedBy      string  `gorm:"type:varchar(255)"`
	DecidedAt      *time.Time
	DecisionNote   string    `gorm:"type:text"`
	RefType        string    `gorm:"type:varchar(30)"`
	RefID          string    `gorm:"type:varchar(255)"`
	CreatedAt      time.Time `gorm:"index"`
}

func (Wastage) TableName() string { return "bar_wastage" }

// Count statuses.
const (
	CDraft     = "draft"
	CSubmitted = "submitted"
	CApproved  = "approved"
)

// Count is a stock take at a location.
type Count struct {
	ID             string   `gorm:"type:varchar(36);primaryKey"`
	OrganizationID string   `gorm:"type:varchar(255);not null;uniqueIndex:idx_bar_cnt_num,priority:1"`
	Number         string   `gorm:"type:varchar(20);uniqueIndex:idx_bar_cnt_num,priority:2"`
	LocationID     string   `gorm:"type:varchar(255);index"`
	Type           string   `gorm:"type:varchar(10)"`
	Blind          bool     `gorm:"type:boolean"`
	Status         string   `gorm:"type:varchar(10);index"`
	BusinessDate   string   `gorm:"type:varchar(10);index"`
	StaffIDs       []string `gorm:"type:text;serializer:json"`
	Note           string   `gorm:"type:text"`
	RecountRound   int      `gorm:"type:integer"`
	CreatedBy      string   `gorm:"type:varchar(255)"`
	SubmittedBy    string   `gorm:"type:varchar(255)"`
	SubmittedAt    *time.Time
	ApprovedBy     string `gorm:"type:varchar(255)"`
	ApprovedAt     *time.Time
	VarianceValue  float64   `gorm:"type:numeric(16,4)"`
	LossValue      float64   `gorm:"type:numeric(16,4)"`
	CreatedAt      time.Time `gorm:"index"`
	UpdatedAt      time.Time
}

func (Count) TableName() string { return "bar_counts" }

// CountLine is one item of a count. Quantities are base units; the
// breakdown columns are filled when the count is approved.
type CountLine struct {
	ID             string     `gorm:"type:varchar(36);primaryKey"`
	OrganizationID string     `gorm:"type:varchar(255);not null;index"`
	CountID        string     `gorm:"type:varchar(36);index"`
	ProductID      string     `gorm:"type:varchar(255);index"`
	FullUnits      *float64   `gorm:"type:numeric(14,3)"`
	PartialMode    string     `gorm:"type:varchar(10)"`
	PartialValue   float64    `gorm:"type:numeric(14,3)"`
	Counted        *float64   `gorm:"type:numeric(14,3)"`
	CountedAt      *time.Time `gorm:"index"`
	CountedBy      string     `gorm:"type:varchar(255)"`
	Recount        bool       `gorm:"type:boolean"`
	Sort           int
	// Filled on approval.
	PrevLineID   string  `gorm:"type:varchar(36)"`
	Opening      float64 `gorm:"type:numeric(14,3)"`
	Received     float64 `gorm:"type:numeric(14,3)"`
	TransfersIn  float64 `gorm:"type:numeric(14,3)"`
	TransfersOut float64 `gorm:"type:numeric(14,3)"`
	Sold         float64 `gorm:"type:numeric(14,3)"`
	Wasted       float64 `gorm:"type:numeric(14,3)"`
	Returns      float64 `gorm:"type:numeric(14,3)"`
	Adjustments  float64 `gorm:"type:numeric(14,3)"`
	Expected     float64 `gorm:"type:numeric(14,3)"`
	Variance     float64 `gorm:"type:numeric(14,3)"`
	UnitCost     float64 `gorm:"type:numeric(18,6)"`
}

func (CountLine) TableName() string { return "bar_count_lines" }

// SaleLine is one bar-managed POS line (for pour cost and revenue by category).
type SaleLine struct {
	ID               string    `gorm:"type:varchar(36);primaryKey"`
	OrganizationID   string    `gorm:"type:varchar(255);not null;index:idx_bar_sale_org_date,priority:1"`
	BusinessDate     string    `gorm:"type:varchar(10);index:idx_bar_sale_org_date,priority:2"`
	POSTransactionID string    `gorm:"type:varchar(255);index"`
	POSItemID        string    `gorm:"type:varchar(255)"`
	POSProductID     string    `gorm:"type:varchar(255);index"`
	LocationID       string    `gorm:"type:varchar(255);index"`
	Name             string    `gorm:"type:varchar(255)"`
	Qty              int       `gorm:"type:integer"`
	Gross            float64   `gorm:"type:numeric(16,4)"`
	Net              float64   `gorm:"type:numeric(16,4)"` // ex VAT, after discounts
	Cost             float64   `gorm:"type:numeric(16,4)"`
	Category         string    `gorm:"type:varchar(30)"`
	Voided           bool      `gorm:"type:boolean"`
	CreatedAt        time.Time `gorm:"index"`
}

func (SaleLine) TableName() string { return "bar_sale_lines" }

// NightReport records one night report send.
type NightReport struct {
	OrganizationID string `gorm:"type:varchar(255);primaryKey"`
	BusinessDate   string `gorm:"type:varchar(10);primaryKey"`
	SentAt         time.Time
	Recipients     string `gorm:"type:text"`
	Error          string `gorm:"type:text"`
}

func (NightReport) TableName() string { return "bar_night_reports" }

// AllModels are migrated by Migrate.
func AllModels() []interface{} {
	return []interface{}{&Settings{}, &Item{}, &Stock{}, &Movement{}, &Recipe{}, &RecipeLine{}, &Par{}, &LocationStaff{}, &Terminal{},
		&PriceHistory{}, &Delivery{}, &DeliveryLine{}, &Transfer{}, &TransferLine{}, &Wastage{}, &Count{}, &CountLine{}, &SaleLine{}, &NightReport{}}
}

// Migrate creates / updates the bar stock tables (idempotent).
func Migrate(db *gorm.DB) error {
	for _, m := range AllModels() {
		if err := db.AutoMigrate(m); err != nil {
			return err
		}
	}
	setReady(true)
	return nil
}
