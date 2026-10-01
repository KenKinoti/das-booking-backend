package orgprefs

// Tax and time zone defaults by country.
//
// The organisation's country decides the suggested sales tax (name + standard
// rate) and time zone. New organisations get them automatically (invoicing
// settings are created from them); when the country changes, or an existing
// organisation's settings conflict with its country (e.g. a Kenyan business
// with "GST 10%" and Australia/Adelaide), the app suggests the update and
// applies it only after the user confirms. Existing invoices never change.

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CountryDefault is the standard tax and time zone of a country.
type CountryDefault struct {
	Country   string   `json:"country"`
	TaxLabel  string   `json:"tax_label"`
	TaxRate   float64  `json:"tax_rate"` // standard rate, percent
	Timezone  string   `json:"timezone"`
	Timezones []string `json:"timezones,omitempty"` // several zones: ask which
	TaxIDName string   `json:"tax_id_label,omitempty"`
	Note      string   `json:"note,omitempty"`
}

var auZones = []string{"Australia/Sydney", "Australia/Melbourne", "Australia/Brisbane", "Australia/Adelaide", "Australia/Perth", "Australia/Darwin", "Australia/Hobart"}

// countryDefaults: standard rates (2026). Reduced / zero rates and levies are
// noted, not applied.
var countryDefaults = map[string]CountryDefault{
	"KE": {TaxLabel: "VAT", TaxRate: 16, Timezone: "Africa/Nairobi", TaxIDName: "KRA PIN"},
	"UG": {TaxLabel: "VAT", TaxRate: 18, Timezone: "Africa/Kampala", TaxIDName: "TIN"},
	"TZ": {TaxLabel: "VAT", TaxRate: 18, Timezone: "Africa/Dar_es_Salaam", TaxIDName: "TIN"},
	"RW": {TaxLabel: "VAT", TaxRate: 18, Timezone: "Africa/Kigali", TaxIDName: "TIN"},
	"ET": {TaxLabel: "VAT", TaxRate: 15, Timezone: "Africa/Addis_Ababa", TaxIDName: "TIN"},
	"NG": {TaxLabel: "VAT", TaxRate: 7.5, Timezone: "Africa/Lagos", TaxIDName: "TIN"},
	"GH": {TaxLabel: "VAT", TaxRate: 15, Timezone: "Africa/Accra", TaxIDName: "TIN", Note: "Ghana also charges the NHIL (2.5%) and GETFund levy (2.5%) on most supplies — add them as separate tax rates if they apply to you."},
	"ZA": {TaxLabel: "VAT", TaxRate: 15, Timezone: "Africa/Johannesburg", TaxIDName: "VAT number"},
	"EG": {TaxLabel: "VAT", TaxRate: 14, Timezone: "Africa/Cairo", TaxIDName: "Tax ID"},
	"MA": {TaxLabel: "TVA", TaxRate: 20, Timezone: "Africa/Casablanca", TaxIDName: "ICE"},
	"ZM": {TaxLabel: "VAT", TaxRate: 16, Timezone: "Africa/Lusaka", TaxIDName: "TPIN"},
	"AU": {TaxLabel: "GST", TaxRate: 10, Timezone: "Australia/Sydney", Timezones: auZones, TaxIDName: "ABN"},
	"NZ": {TaxLabel: "GST", TaxRate: 15, Timezone: "Pacific/Auckland", TaxIDName: "GST number"},
	"GB": {TaxLabel: "VAT", TaxRate: 20, Timezone: "Europe/London", TaxIDName: "VAT number"},
	"IE": {TaxLabel: "VAT", TaxRate: 23, Timezone: "Europe/Dublin", TaxIDName: "VAT number"},
	"DE": {TaxLabel: "MwSt", TaxRate: 19, Timezone: "Europe/Berlin", TaxIDName: "USt-IdNr"},
	"FR": {TaxLabel: "TVA", TaxRate: 20, Timezone: "Europe/Paris", TaxIDName: "N° TVA"},
	"ES": {TaxLabel: "IVA", TaxRate: 21, Timezone: "Europe/Madrid", TaxIDName: "NIF"},
	"IT": {TaxLabel: "IVA", TaxRate: 22, Timezone: "Europe/Rome", TaxIDName: "Partita IVA"},
	"NL": {TaxLabel: "BTW", TaxRate: 21, Timezone: "Europe/Amsterdam", TaxIDName: "BTW-id"},
	"BE": {TaxLabel: "BTW/TVA", TaxRate: 21, Timezone: "Europe/Brussels", TaxIDName: "VAT number"},
	"AT": {TaxLabel: "USt", TaxRate: 20, Timezone: "Europe/Vienna", TaxIDName: "UID"},
	"PT": {TaxLabel: "IVA", TaxRate: 23, Timezone: "Europe/Lisbon", TaxIDName: "NIF"},
	"CH": {TaxLabel: "MWST", TaxRate: 8.1, Timezone: "Europe/Zurich", TaxIDName: "UID"},
	"IN": {TaxLabel: "GST", TaxRate: 18, Timezone: "Asia/Kolkata", TaxIDName: "GSTIN", Note: "18% is the most common GST slab; use 5%, 12% or 28% where your goods or services fall in those slabs."},
	"SG": {TaxLabel: "GST", TaxRate: 9, Timezone: "Asia/Singapore", TaxIDName: "GST reg. no."},
	"AE": {TaxLabel: "VAT", TaxRate: 5, Timezone: "Asia/Dubai", TaxIDName: "TRN"},
	"SA": {TaxLabel: "VAT", TaxRate: 15, Timezone: "Asia/Riyadh", TaxIDName: "VAT number"},
	"JP": {TaxLabel: "Consumption tax", TaxRate: 10, Timezone: "Asia/Tokyo", TaxIDName: "Invoice reg. no."},
	"CN": {TaxLabel: "VAT", TaxRate: 13, Timezone: "Asia/Shanghai", TaxIDName: "Tax ID"},
	"HK": {TaxLabel: "Tax", TaxRate: 0, Timezone: "Asia/Hong_Kong", Note: "Hong Kong has no sales tax / VAT."},
	"CA": {TaxLabel: "GST", TaxRate: 5, Timezone: "America/Toronto", Timezones: []string{"America/Toronto", "America/Vancouver", "America/Edmonton", "America/Winnipeg", "America/Halifax", "America/St_Johns"},
		TaxIDName: "GST/HST number", Note: "Ontario and the Atlantic provinces charge HST (13–15%) instead; some provinces add PST — set the rate for your province."},
	"US": {TaxLabel: "Sales tax", TaxRate: 0, Timezone: "America/New_York", Timezones: []string{"America/New_York", "America/Chicago", "America/Denver", "America/Phoenix", "America/Los_Angeles", "America/Anchorage", "Pacific/Honolulu"},
		TaxIDName: "EIN", Note: "Sales tax depends on your state and city — set your combined rate."},
}

// DefaultsFor returns the country's tax / time zone defaults (ok=false when unknown).
func DefaultsFor(country string) (CountryDefault, bool) {
	cc := strings.ToUpper(strings.TrimSpace(country))
	d, ok := countryDefaults[cc]
	if !ok {
		if tz := TimezoneForCountry(cc); tz != "" {
			return CountryDefault{Country: cc, Timezone: tz}, false
		}
		return CountryDefault{Country: cc}, false
	}
	d.Country = cc
	if len(d.Timezones) == 0 {
		d.Timezones = []string{d.Timezone}
	}
	return d, true
}

// DefaultTax returns the tax label / rate a new organisation in country
// should start with (AU GST 10% when the country is unknown).
func DefaultTax(country string) (string, float64, string) {
	if d, ok := DefaultsFor(country); ok {
		return d.TaxLabel, d.TaxRate, d.TaxIDName
	}
	return "GST", 10, "ABN"
}

// Mismatch is a conflict between the organisation's country and its tax /
// time zone settings.
type Mismatch struct {
	Key             string         `json:"key"`
	Tax             bool           `json:"tax"`
	Timezone        bool           `json:"timezone"`
	CurrentTaxLabel string         `json:"current_tax_label"`
	CurrentTaxRate  float64        `json:"current_tax_rate"`
	CurrentTimezone string         `json:"current_timezone"`
	Suggested       CountryDefault `json:"suggested"`
	Message         string         `json:"message"`
}

// OrgNotice remembers dismissed one-time notices (org_notices).
type OrgNotice struct {
	OrganizationID string    `gorm:"type:varchar(255);primaryKey"`
	Key            string    `gorm:"type:varchar(200);primaryKey"`
	DismissedAt    time.Time `json:"dismissed_at"`
}

// TableName is fixed.
func (OrgNotice) TableName() string { return "org_notices" }

func taxSettings(db *gorm.DB, org string) (string, float64, bool) {
	if !hasTable(db, "billing_settings") {
		return "", 0, false
	}
	var row struct {
		TaxLabel       string
		DefaultTaxRate float64
		N              int
	}
	db.Raw("SELECT COALESCE(tax_label,'') AS tax_label, COALESCE(default_tax_rate,0) AS default_tax_rate, 1 AS n FROM billing_settings WHERE organization_id = ? LIMIT 1", org).Scan(&row)
	return row.TaxLabel, row.DefaultTaxRate, row.N == 1
}

func zoneOK(d CountryDefault, tz string) bool {
	for _, z := range d.Timezones {
		if z == tz {
			return true
		}
	}
	return false
}

func shortZone(tz string) string {
	if i := strings.LastIndex(tz, "/"); i >= 0 {
		tz = tz[i+1:]
	}
	return strings.ReplaceAll(tz, "_", " ")
}

func taxText(label string, rate float64) string {
	return strings.TrimSpace(fmt.Sprintf("%s %s%%", label, strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", rate), "0"), ".")))
}

// CheckMismatch compares the organisation's tax and time zone with its
// country (nil when they agree, the country is unknown or the notice for
// this exact conflict was dismissed).
func CheckMismatch(db *gorm.DB, org string, includeDismissed bool) *Mismatch {
	p := Get(db, org)
	d, ok := DefaultsFor(p.CountryCode)
	if !ok || p.CountryCode == "" {
		return nil
	}
	label, rate, has := taxSettings(db, org)
	m := &Mismatch{CurrentTaxLabel: label, CurrentTaxRate: rate, CurrentTimezone: p.Timezone, Suggested: d}
	// Rates of 0 are a deliberate choice (e.g. not registered for VAT); only a
	// different, non-zero rate or another country's tax name is a conflict.
	m.Tax = has && d.TaxRate > 0 && rate > 0 && (rate != d.TaxRate || !strings.EqualFold(strings.TrimSpace(label), d.TaxLabel))
	m.Timezone = !zoneOK(d, p.Timezone)
	if !m.Tax && !m.Timezone {
		return nil
	}
	m.Key = fmt.Sprintf("country-defaults:%s:%s:%s", p.CountryCode, strings.ReplaceAll(taxText(label, rate), " ", ""), p.Timezone)
	parts, fixes := []string{}, []string{}
	if m.Tax {
		parts = append(parts, "tax is set to "+taxText(label, rate))
		fixes = append(fixes, taxText(d.TaxLabel, d.TaxRate))
	}
	if m.Timezone {
		parts = append(parts, "time zone to "+shortZone(p.Timezone))
		if len(d.Timezones) > 1 {
			fixes = append(fixes, "a "+p.CountryName+" time zone")
		} else {
			fixes = append(fixes, d.Timezone)
		}
	}
	m.Message = fmt.Sprintf("Your business is in %s but %s — update to %s?", p.CountryName, strings.Join(parts, " and "), strings.Join(fixes, " and "))
	if !includeDismissed {
		var n int64
		db.Model(&OrgNotice{}).Where("organization_id = ? AND key = ?", org, m.Key).Count(&n)
		if n > 0 {
			return nil
		}
	}
	return m
}

// DismissNotice hides a one-time notice for good.
func DismissNotice(db *gorm.DB, org, key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("missing notice key")
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&OrgNotice{OrganizationID: org, Key: key, DismissedAt: time.Now()}).Error
}

// ApplyInput: what to update from the country defaults.
type ApplyInput struct {
	Tax      bool   `json:"tax"`
	Timezone string `json:"timezone"` // "" = keep
}

// ApplyDefaults updates the default tax (invoicing settings and the default
// tax rate) and / or the time zone. Existing invoices keep their tax.
func ApplyDefaults(db *gorm.DB, org string, in ApplyInput) (Prefs, error) {
	p := Get(db, org)
	d, ok := DefaultsFor(p.CountryCode)
	if in.Tax && !ok {
		return p, fmt.Errorf("No tax defaults for %s — set your tax in Invoice settings", nonEmptyStr(p.CountryName, "this country"))
	}
	if tz := strings.TrimSpace(in.Timezone); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return p, fmt.Errorf("Unknown time zone %s", tz)
		}
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if in.Tax && hasTable(tx, "billing_settings") {
			upd := map[string]any{"default_tax_rate": d.TaxRate, "tax_label": d.TaxLabel}
			if d.TaxIDName != "" && hasColumn(tx, "billing_settings", "tax_number_label") {
				// Only replace the other country's default label, never a custom one.
				tx.Exec("UPDATE billing_settings SET tax_number_label = ? WHERE organization_id = ? AND COALESCE(tax_number, '') = '' AND tax_number_label IN ('ABN','')", d.TaxIDName, org)
			}
			if err := tx.Table("billing_settings").Where("organization_id = ?", org).Updates(upd).Error; err != nil {
				return err
			}
			if hasTable(tx, "billing_tax_rates") {
				tx.Exec("UPDATE billing_tax_rates SET is_default = false WHERE organization_id = ?", org)
				var id string
				tx.Raw("SELECT id FROM billing_tax_rates WHERE organization_id = ? AND LOWER(name) = LOWER(?) AND rate = ? LIMIT 1", org, d.TaxLabel, d.TaxRate).Row().Scan(&id)
				if id != "" {
					tx.Exec("UPDATE billing_tax_rates SET is_default = true WHERE id = ?", id)
				} else {
					tx.Exec("INSERT INTO billing_tax_rates (id, organization_id, name, rate, is_default, created_at) VALUES (gen_random_uuid()::text, ?, ?, ?, true, now())", org, d.TaxLabel, d.TaxRate)
				}
			}
		}
		if tz := strings.TrimSpace(in.Timezone); tz != "" && tz != p.Timezone {
			p.Timezone = tz
			p.Source = SourceUser
			if err := write(tx, org, p); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Get(db, org), err
	}
	return Get(db, org), nil
}

func nonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
