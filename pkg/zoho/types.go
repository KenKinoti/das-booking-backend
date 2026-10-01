package zoho

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

// Entities, in import order. Quotes come before invoices so an invoice made
// from a quote can be linked to it.
const (
	EntSettings   = "settings"
	EntItems      = "items"
	EntContacts   = "contacts"
	EntPersons    = "contact_persons"
	EntQuotes     = "quotes"
	EntInvoices   = "invoices"
	EntPayments   = "payments"
	EntCredits    = "credit_notes"
	EntRecurring  = "recurring"
	EntPDFs       = "pdfs"
	EntHistory    = "history"
	entContactRef = "contact"
	entPersonRef  = "contact_person"
	entItemRef    = "item"
	entDocRef     = "doc" // invoices, quotes, credit notes share one namespace per kind: doc:<kind>
	entPaymentRef = "payment"
	entPDFRef     = "pdf"
)

// EntityLabels are shown in the UI.
var EntityLabels = map[string]string{
	EntSettings: "Organisation settings", EntItems: "Items", EntContacts: "Customers", EntPersons: "Contact people",
	EntQuotes: "Quotes (estimates)", EntInvoices: "Invoices", EntPayments: "Payments", EntCredits: "Credit notes",
	EntRecurring: "Recurring profiles", EntPDFs: "Original PDFs", EntHistory: "Email & history",
}

// Address is a postal address from the source.
type Address struct {
	Attention string `json:"attention,omitempty"`
	Street    string `json:"street,omitempty"`
	Street2   string `json:"street2,omitempty"`
	City      string `json:"city,omitempty"`
	State     string `json:"state,omitempty"`
	Zip       string `json:"zip,omitempty"`
	Country   string `json:"country,omitempty"`
	Phone     string `json:"phone,omitempty"`
}

// Person is a contact person of a customer.
type Person struct {
	ExtID     string `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	Mobile    string `json:"mobile"`
	Role      string `json:"role"`
	Primary   bool   `json:"primary"`
}

// Contact is a customer.
type Contact struct {
	ExtID      string   `json:"id"`
	Name       string   `json:"name"` // display name
	Company    string   `json:"company"`
	FirstName  string   `json:"first_name"`
	LastName   string   `json:"last_name"`
	Individual bool     `json:"individual"`
	Email      string   `json:"email"`
	Phone      string   `json:"phone"`
	Mobile     string   `json:"mobile"`
	Website    string   `json:"website"`
	Currency   string   `json:"currency"`
	TermsDays  *int     `json:"terms_days"`
	TaxNumber  string   `json:"tax_number"`
	Notes      string   `json:"notes"`
	Active     bool     `json:"active"`
	Billing    Address  `json:"billing"`
	Shipping   Address  `json:"shipping"`
	Persons    []Person `json:"persons"`
	Modified   string   `json:"-"`
	Row        int      `json:"-"`
	ListOnly   bool     `json:"-"` // only list fields are known (API: details not fetched yet)
}

// Item is a product or service.
type Item struct {
	ExtID       string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Rate        float64 `json:"rate"`
	Unit        string  `json:"unit"`
	SKU         string  `json:"sku"`
	TaxName     string  `json:"tax_name"`
	TaxPercent  float64 `json:"tax_percent"`
	Type        string  `json:"type"` // service | goods
	Active      bool    `json:"active"`
	Row         int     `json:"-"`
}

// Line is a document line.
type Line struct {
	ExtID       string   `json:"id"`
	ItemID      string   `json:"item_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Quantity    float64  `json:"quantity"`
	Unit        string   `json:"unit"`
	Rate        float64  `json:"rate"`
	DiscountPct float64  `json:"discount_pct"`
	DiscountAmt float64  `json:"discount_amt"`
	TaxName     string   `json:"tax_name"`
	TaxPercent  float64  `json:"tax_percent"`
	TaxAmount   *float64 `json:"tax_amount"`
	Total       float64  `json:"total"` // after line discount, before tax
}

// Pay is a payment applied to one document.
type Pay struct {
	ExtID     string  `json:"id"` // unique per payment+invoice
	Number    string  `json:"number"`
	Date      string  `json:"date"`
	Amount    float64 `json:"amount"`
	Mode      string  `json:"mode"`
	Reference string  `json:"reference"`
	Notes     string  `json:"notes"`
}

// Comment is one history entry of a document.
type Comment struct {
	Date string `json:"date"` // YYYY-MM-DD or RFC3339
	Text string `json:"text"`
	By   string `json:"by"`
	Kind string `json:"kind"` // email | history
}

// Doc is an invoice, quote (estimate) or credit note.
type Doc struct {
	Kind             string   `json:"kind"` // invoice | quote | credit_note
	ExtID            string   `json:"id"`
	Number           string   `json:"number"`
	ContactID        string   `json:"contact_id"`
	CustomerName     string   `json:"customer_name"`
	Email            string   `json:"email"`
	Status           string   `json:"status"` // source status
	Date             string   `json:"date"`
	DueDate          string   `json:"due_date"`
	Currency         string   `json:"currency"`
	ExchangeRate     float64  `json:"exchange_rate"` // source: base units per 1 document currency unit
	Reference        string   `json:"reference"`
	Lines            []Line   `json:"lines"`
	SubTotal         float64  `json:"sub_total"`
	DiscountPct      float64  `json:"discount_pct"`
	DiscountAmt      float64  `json:"discount_amt"`
	DiscountAfterTax bool     `json:"discount_after_tax"`
	Shipping         float64  `json:"shipping"`
	Adjustment       float64  `json:"adjustment"`
	AdjustmentDesc   string   `json:"adjustment_desc"`
	TaxTotal         float64  `json:"tax_total"`
	Total            float64  `json:"total"`
	Balance          float64  `json:"balance"`
	PaymentMade      float64  `json:"payment_made"`
	Credits          *float64 `json:"credits"`
	WriteOff         float64  `json:"write_off"`
	InclusiveTax     bool     `json:"inclusive_tax"`
	Notes            string   `json:"notes"`
	Terms            string   `json:"terms"`
	URL              string   `json:"url"`
	BillingAddress   string   `json:"billing_address"`
	EstimateID       string   `json:"estimate_id"`
	Payments         []Pay    `json:"payments"`
	LastPaymentDate  string   `json:"last_payment_date"`
	Created          string   `json:"created"`
	// Recurring profiles
	RecurName     string `json:"recur_name,omitempty"`
	RecurInterval string `json:"recur_interval,omitempty"`
	RecurNext     string `json:"recur_next,omitempty"`
	RecurEnd      string `json:"recur_end,omitempty"`

	Modified   string `json:"-"`
	Row        int    `json:"-"`
	ListOnly   bool   `json:"-"`
	TaxUnknown bool   `json:"-"` // the source gave no tax amount: keep the calculated tax
	// BalanceUnknown: the source gave no balance (generic files): it is the
	// total less the payments imported.
	BalanceUnknown bool `json:"-"`
}

// Dataset is everything read from export files (CSV path).
type Dataset struct {
	BaseCurrency string
	Items        []Item
	Contacts     []Contact
	Docs         []Doc // quotes, invoices, credit notes
	Errors       []JobError
	// UnappliedPayments: money received that was not applied to an invoice.
	Unapplied []string
}

// Options chosen in the preview.
type Options struct {
	// Entities to import (default all but recurring).
	Entities map[string]bool `json:"entities"`
	// OnCustomerMatch: what to do when a customer already exists here
	// (same email or name): merge (link and fill blanks) | skip | create.
	OnCustomerMatch string `json:"on_customer_match"`
	// OnDocMatch: an invoice/quote with the same number already exists here:
	// skip | overwrite (take it over with the imported data).
	OnDocMatch string `json:"on_doc_match"`
	// Decisions override the defaults per record: "contact:<id>" / "doc:<kind>:<id>" → action.
	Decisions map[string]string `json:"decisions"`
	// Overwrite: on re-sync, imported data replaces local edits made since.
	Overwrite bool `json:"overwrite"`
	// ItemsToCatalog: add items to the services catalog.
	ItemsToCatalog bool `json:"items_to_catalog"`
	// ContinueNumbering: next invoice/quote numbers follow the imported ones.
	ContinueNumbering *bool `json:"continue_numbering"`
	// RequestsPerMinute caps API calls (default 90; Zoho allows 100).
	RequestsPerMinute int `json:"requests_per_minute,omitempty"`
}

func (o Options) want(ent string) bool {
	if o.Entities == nil {
		return ent != EntRecurring
	}
	v, ok := o.Entities[ent]
	if !ok {
		return ent != EntRecurring
	}
	return v
}

func (o Options) continueNumbering() bool { return o.ContinueNumbering == nil || *o.ContinueNumbering }

// Preview describes what an import would do.
type Preview struct {
	OrgName      string          `json:"org_name,omitempty"`
	BaseCurrency string          `json:"base_currency"`
	HomeCurrency string          `json:"home_currency"`
	Entities     []EntityPreview `json:"entities"`
	Duplicates   []Duplicate     `json:"duplicates"`
	Currencies   []CurrencySum   `json:"currencies"`
	Warnings     []string        `json:"warnings"`
	Numbering    []NumberPreview `json:"numbering"`
	Partial      bool            `json:"partial,omitempty"` // API: counts from list pages only
	BuiltAt      string          `json:"built_at"`
}

// EntityPreview is the count and a few sample rows of one entity.
type EntityPreview struct {
	Entity  string              `json:"entity"`
	Label   string              `json:"label"`
	Count   int                 `json:"count"`
	New     int                 `json:"new"`
	Linked  int                 `json:"linked"` // already imported before (re-sync)
	Matches int                 `json:"matches"`
	Columns []string            `json:"columns"`
	Sample  []map[string]string `json:"sample"`
}

// Duplicate is a source record that matches one already here.
type Duplicate struct {
	Key        string `json:"key"` // decision key
	Entity     string `json:"entity"`
	Label      string `json:"label"`
	MatchID    string `json:"match_id"`
	MatchLabel string `json:"match_label"`
	MatchBy    string `json:"match_by"`
	Default    string `json:"default"`
}

// CurrencySum totals documents per currency.
type CurrencySum struct {
	Currency    string  `json:"currency"`
	Invoices    int     `json:"invoices"`
	Total       float64 `json:"total"`
	Outstanding float64 `json:"outstanding"`
	Supported   bool    `json:"supported"`
}

// NumberPreview is how numbering will continue.
type NumberPreview struct {
	DocType string `json:"doc_type"`
	Highest string `json:"highest"`
	Next    string `json:"next"`
}

// Summary is the post-import report.
type Summary struct {
	Numbering      []*invoicing.Numbering `json:"numbering"`
	Reconciliation []Reconciliation       `json:"reconciliation"`
	Flags          int                    `json:"flags"`
	Warnings       []string               `json:"warnings"`
	Undo           *UndoResult            `json:"undo,omitempty"`
}

// Reconciliation compares source and local totals per currency.
type Reconciliation struct {
	Currency          string  `json:"currency"`
	Documents         int     `json:"documents"`
	SourceTotal       float64 `json:"source_total"`
	LocalTotal        float64 `json:"local_total"`
	SourceOutstanding float64 `json:"source_outstanding"`
	LocalOutstanding  float64 `json:"local_outstanding"`
	OK                bool    `json:"ok"`
}

// UndoResult reports an undo.
type UndoResult struct {
	At      string         `json:"at"`
	Deleted map[string]int `json:"deleted"`
	Kept    map[string]int `json:"kept"`
	KeptWhy []string       `json:"kept_why"`
}

func hashOf(v interface{}) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:16])
}
