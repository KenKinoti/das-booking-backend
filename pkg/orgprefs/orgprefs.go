// Package orgprefs is the single source of an organisation's regional
// preferences: the country it is based in (ISO 3166-1 alpha-2), its home
// currency (ISO 4217) and its time zone.
//
// They live on the organizations row (country_code, currency, timezone,
// region_source). Every module that needs a default currency — invoicing,
// POS, bookings, events, the small business hub, HR, finance, plans, the P&L,
// analytics and the AI assistant — calls Currency(db, org) instead of reading
// its own settings table, so changing the organisation's currency changes the
// default for NEW records everywhere. Existing records keep their currency.
//
// Set mirrors the values into the legacy per-module settings rows
// (organization_settings, billing_settings, plan_subscriptions and an empty
// small business hub) so older readers stay consistent.
package orgprefs

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	_ "time/tzdata" // time zones even on hosts without zoneinfo

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Fallbacks used when nothing is known about an organisation.
const (
	DefaultCountry  = "AU"
	DefaultCurrency = "AUD"
	DefaultTimezone = "Australia/Sydney"
)

// Where the stored values came from.
const (
	SourceUser     = "user"     // chosen by a person (settings, setup prompt, platform admin)
	SourceInferred = "inferred" // backfilled from the organisation's existing data
	SourceDefault  = "default"  // nothing to go on: AU / AUD
)

// Prefs are an organisation's regional preferences.
type Prefs struct {
	CountryCode string `json:"country_code"`
	CountryName string `json:"country_name"`
	Currency    string `json:"currency"`
	Decimals    int    `json:"currency_decimals"`
	Timezone    string `json:"timezone"`
	Source      string `json:"source"`
	// NeedsSetup is true until someone has confirmed where the business is
	// based and nothing in its data said so either.
	NeedsSetup bool `json:"needs_setup"`
}

// primaryCountry is the country a currency most likely means (for inferring
// a country from an organisation that only ever set a currency).
var primaryCountry = map[string]string{
	"USD": "US", "GBP": "GB", "JPY": "JP", "CHF": "CH", "CAD": "CA", "AUD": "AU", "NZD": "NZ", "CNY": "CN",
	"INR": "IN", "SGD": "SG", "HKD": "HK", "AED": "AE", "SAR": "SA", "KWD": "KW", "ZAR": "ZA", "NGN": "NG",
	"EGP": "EG", "KES": "KE", "GHS": "GH", "MAD": "MA", "TZS": "TZ", "UGX": "UG",
}

// countryTimezone is a sensible default time zone for a country.
var countryTimezone = map[string]string{
	"AU": "Australia/Sydney", "NZ": "Pacific/Auckland", "US": "America/New_York", "CA": "America/Toronto", "GB": "Europe/London",
	"IE": "Europe/Dublin", "DE": "Europe/Berlin", "FR": "Europe/Paris", "ES": "Europe/Madrid", "IT": "Europe/Rome", "NL": "Europe/Amsterdam",
	"BE": "Europe/Brussels", "AT": "Europe/Vienna", "PT": "Europe/Lisbon", "CH": "Europe/Zurich", "JP": "Asia/Tokyo", "CN": "Asia/Shanghai",
	"HK": "Asia/Hong_Kong", "SG": "Asia/Singapore", "IN": "Asia/Kolkata", "AE": "Asia/Dubai", "SA": "Asia/Riyadh", "KW": "Asia/Kuwait",
	"ZA": "Africa/Johannesburg", "NG": "Africa/Lagos", "EG": "Africa/Cairo", "KE": "Africa/Nairobi", "GH": "Africa/Accra",
	"MA": "Africa/Casablanca", "TZ": "Africa/Dar_es_Salaam", "UG": "Africa/Kampala", "RW": "Africa/Kigali", "ET": "Africa/Addis_Ababa",
}

// TimezoneForCountry returns a default time zone for a country ("" if unknown).
func TimezoneForCountry(code string) string {
	return countryTimezone[strings.ToUpper(strings.TrimSpace(code))]
}

// CountryForCurrency returns the country a currency most likely stands for.
func CountryForCurrency(cur string) string {
	return primaryCountry[strings.ToUpper(strings.TrimSpace(cur))]
}

// hasTable reports whether a table exists on the search path without ever
// raising an error (an error would abort the caller's transaction).
func hasTable(db *gorm.DB, name string) bool {
	var ok bool
	if err := db.Raw("SELECT to_regclass(?) IS NOT NULL", name).Row().Scan(&ok); err != nil {
		return false
	}
	return ok
}

func hasColumn(db *gorm.DB, table, col string) bool {
	var n int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`, table, col).Row().Scan(&n)
	return n > 0
}

func normCur(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// Get returns the organisation's preferences, falling back to what its
// settings say (and finally AU / AUD) when the organisation row has none.
func Get(db *gorm.DB, org string) Prefs {
	var row struct {
		CountryCode, Currency, Timezone, RegionSource, AddressCountry string
	}
	if org != "" && hasTable(db, "organizations") && hasColumn(db, "organizations", "country_code") {
		db.Raw(`SELECT COALESCE(country_code,'') AS country_code, COALESCE(currency,'') AS currency, COALESCE(timezone,'') AS timezone,
			COALESCE(region_source,'') AS region_source, COALESCE(address_country,'') AS address_country FROM organizations WHERE id = ? LIMIT 1`, org).Scan(&row)
	}
	p := Prefs{CountryCode: strings.ToUpper(strings.TrimSpace(row.CountryCode)), Currency: normCur(row.Currency), Timezone: strings.TrimSpace(row.Timezone), Source: row.RegionSource}
	if !currency.IsSupported(p.Currency) {
		p.Currency = legacyCurrency(db, org)
	}
	if !currency.IsSupported(p.Currency) {
		if c, ok := currency.CurrencyForCountry(p.CountryCode); ok {
			p.Currency = c
		} else {
			p.Currency = DefaultCurrency
		}
	}
	if !currency.IsCountry(p.CountryCode) {
		p.CountryCode = ""
	}
	if p.Timezone == "" {
		p.Timezone = legacyTimezone(db, org)
		if p.CountryCode != "" && p.CountryCode != "AU" && strings.HasPrefix(p.Timezone, "Australia/") {
			p.Timezone = "" // the settings default, not a choice
		}
	}
	if _, err := time.LoadLocation(p.Timezone); p.Timezone == "" || err != nil {
		if tz := TimezoneForCountry(p.CountryCode); tz != "" {
			p.Timezone = tz
		} else {
			p.Timezone = DefaultTimezone
		}
	}
	p.CountryName = currency.CountryName(p.CountryCode)
	p.Decimals = currency.Decimals(p.Currency)
	p.NeedsSetup = p.CountryCode == "" || (p.Source != SourceUser && p.Source != SourceInferred)
	return p
}

// Currency is the organisation's home currency (ISO 4217, upper case).
func Currency(db *gorm.DB, org string) string { return Get(db, org).Currency }

// Country is the organisation's ISO country code ("" when unknown).
func Country(db *gorm.DB, org string) string { return Get(db, org).CountryCode }

// legacyCurrency reads the currency the organisation used before it had one
// on its own row: invoicing settings, then organisation settings, then the
// plan's default currency.
func legacyCurrency(db *gorm.DB, org string) string {
	if org == "" {
		return ""
	}
	for _, q := range []struct{ table, sql string }{
		{"billing_settings", "SELECT COALESCE(currency,'') FROM billing_settings WHERE organization_id = ? LIMIT 1"},
		{"organization_settings", "SELECT COALESCE(currency,'') FROM organization_settings WHERE organization_id = ? LIMIT 1"},
		{"plan_subscriptions", "SELECT COALESCE(NULLIF(default_currency,''), currency, '') FROM plan_subscriptions WHERE organization_id = ? LIMIT 1"},
	} {
		if !hasTable(db, q.table) {
			continue
		}
		var cur string
		db.Raw(q.sql, org).Row().Scan(&cur)
		if cur = normCur(cur); currency.IsSupported(cur) {
			return cur
		}
	}
	return ""
}

func legacyTimezone(db *gorm.DB, org string) string {
	if org == "" || !hasTable(db, "organization_settings") {
		return ""
	}
	var tz string
	db.Raw("SELECT COALESCE(timezone,'') FROM organization_settings WHERE organization_id = ? LIMIT 1", org).Row().Scan(&tz)
	return strings.TrimSpace(tz)
}

// Input is a change to an organisation's preferences. Empty fields keep the
// current value; a country without a currency picks that country's currency.
type Input struct {
	CountryCode string `json:"country_code"`
	Currency    string `json:"currency"`
	Timezone    string `json:"timezone"`
}

// Validate normalises in against the current prefs and returns the values to
// store, or a user-facing error.
func Validate(cur Prefs, in Input) (Prefs, error) {
	next := cur
	cc := ""
	if raw := strings.TrimSpace(in.CountryCode); raw != "" {
		if cc = currency.CountryCode(raw); cc == "" {
			return cur, fmt.Errorf("Unknown country %q", raw)
		}
		next.CountryCode = cc
	}
	c := normCur(in.Currency)
	switch {
	case c != "":
		if !currency.IsSupported(c) {
			return cur, fmt.Errorf("Unsupported currency %s", c)
		}
		next.Currency = c
	case cc != "" && cc != cur.CountryCode:
		if def, ok := currency.CurrencyForCountry(cc); ok {
			next.Currency = def
		}
	}
	if tz := strings.TrimSpace(in.Timezone); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return cur, fmt.Errorf("Unknown time zone %s", tz)
		}
		next.Timezone = tz
	} else if cc != "" && cc != cur.CountryCode && cur.Source != SourceUser {
		if tz := TimezoneForCountry(cc); tz != "" {
			next.Timezone = tz
		}
	}
	if next.CountryCode == "" && c != "" {
		// Only a currency was given: assume the country it belongs to.
		next.CountryCode = CountryForCurrency(c)
	}
	if next.CountryCode == "" {
		return cur, errors.New("Choose the country your business is based in")
	}
	next.CountryName = currency.CountryName(next.CountryCode)
	next.Decimals = currency.Decimals(next.Currency)
	return next, nil
}

// Set validates and stores the organisation's preferences, mirrors them into
// the per-module settings rows and returns the result. source is one of the
// Source* constants.
func Set(db *gorm.DB, org string, in Input, source string) (Prefs, error) {
	if org == "" {
		return Prefs{}, errors.New("Organisation not found")
	}
	cur := Get(db, org)
	next, err := Validate(cur, in)
	if err != nil {
		return cur, err
	}
	if source == "" {
		source = SourceUser
	}
	next.Source = source
	err = db.Transaction(func(tx *gorm.DB) error { return write(tx, org, next) })
	if err != nil {
		return cur, err
	}
	return Get(db, org), nil
}

// write stores prefs on the organisation row and mirrors them.
func write(tx *gorm.DB, org string, p Prefs) error {
	upd := map[string]interface{}{
		"country_code": p.CountryCode, "currency": p.Currency, "timezone": p.Timezone, "region_source": p.Source,
	}
	// Keep the free-text address country readable and in step.
	if name := currency.CountryName(p.CountryCode); name != "" {
		upd["address_country"] = name
	}
	if err := tx.Table("organizations").Where("id = ?", org).Updates(upd).Error; err != nil {
		return err
	}
	return mirror(tx, org, p)
}

// mirror copies the currency / time zone into legacy settings rows.
func mirror(tx *gorm.DB, org string, p Prefs) error {
	if hasTable(tx, "organization_settings") {
		if err := tx.Exec("UPDATE organization_settings SET currency = ?, timezone = ?, updated_at = ? WHERE organization_id = ?", p.Currency, p.Timezone, time.Now(), org).Error; err != nil {
			return err
		}
	}
	if hasTable(tx, "billing_settings") {
		if err := tx.Exec("UPDATE billing_settings SET currency = ? WHERE organization_id = ?", p.Currency, org).Error; err != nil {
			return err
		}
	}
	if hasTable(tx, "plan_subscriptions") && hasColumn(tx, "plan_subscriptions", "default_currency") {
		if err := tx.Exec("UPDATE plan_subscriptions SET default_currency = ? WHERE organization_id = ?", p.Currency, org).Error; err != nil {
			return err
		}
	}
	// The hub's base currency follows the organisation until the owner has
	// entered an opening balance in it (that balance keeps its currency).
	if hasTable(tx, "smallbiz_settings") {
		if err := tx.Exec("UPDATE smallbiz_settings SET currency = ? WHERE organization_id = ? AND COALESCE(opening_balance,0) = 0", p.Currency, org).Error; err != nil {
			return err
		}
	}
	return nil
}

// Migrate adds the columns (models.Organization declares them too) and
// backfills organisations that have none.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&OrgNotice{}); err != nil {
		return err
	}
	if !hasTable(db, "organizations") {
		return nil
	}
	for _, stmt := range []string{
		"ALTER TABLE organizations ADD COLUMN IF NOT EXISTS country_code varchar(2)",
		"ALTER TABLE organizations ADD COLUMN IF NOT EXISTS currency varchar(3)",
		"ALTER TABLE organizations ADD COLUMN IF NOT EXISTS timezone varchar(64)",
		"ALTER TABLE organizations ADD COLUMN IF NOT EXISTS region_source varchar(16)",
	} {
		if err := db.Exec(stmt).Error; err != nil {
			return err
		}
	}
	n, err := Backfill(db)
	if n > 0 {
		log.Printf("orgprefs: backfilled country/currency for %d organisation(s)", n)
	}
	return err
}

// Infer works out country, currency and time zone for an organisation from
// its existing data. evidence is false when nothing pointed anywhere and the
// AU / AUD fallback was used.
func Infer(db *gorm.DB, org string) (p Prefs, evidence bool) {
	var o struct {
		AddressCountry, AddressStreet, AddressSuburb, AddressState, AddressPostcode, HoursTimezone string
	}
	db.Raw(`SELECT COALESCE(address_country,'') AS address_country, COALESCE(address_street,'') AS address_street,
		COALESCE(address_suburb,'') AS address_suburb, COALESCE(address_state,'') AS address_state,
		COALESCE(address_postcode,'') AS address_postcode, COALESCE(hours_timezone,'') AS hours_timezone
		FROM organizations WHERE id = ?`, org).Scan(&o)
	cur := legacyCurrency(db, org)
	addrCountry := currency.CountryCode(o.AddressCountry)
	// "Australia" is the column default, so on its own it proves nothing;
	// with a street / suburb / postcode filled in it does.
	addressReal := addrCountry != "" && (addrCountry != "AU" || o.AddressStreet != "" || o.AddressSuburb != "" || o.AddressPostcode != "" || o.AddressState != "")
	// The invoicing/org-settings currency defaulted to AUD too; a non-AUD
	// value was chosen on purpose.
	curReal := cur != "" && cur != DefaultCurrency

	switch {
	case addressReal:
		p.CountryCode = addrCountry
		evidence = true
	case curReal:
		p.CountryCode = CountryForCurrency(cur)
		evidence = p.CountryCode != ""
	}
	switch {
	case cur != "":
		p.Currency = cur
		evidence = evidence || curReal
	case p.CountryCode != "":
		if c, ok := currency.CurrencyForCountry(p.CountryCode); ok {
			p.Currency = c
		}
	}
	if cur == DefaultCurrency && p.CountryCode == "" {
		// Invoicing has been running in AUD: that is what the business uses.
		p.CountryCode = "AU"
		evidence = evidence || hasSettingsRow(db, org)
	}
	if p.CountryCode == "" {
		p.CountryCode = DefaultCountry
	}
	if p.Currency == "" {
		if c, ok := currency.CurrencyForCountry(p.CountryCode); ok {
			p.Currency = c
		} else {
			p.Currency = DefaultCurrency
		}
	}
	p.Timezone = legacyTimezone(db, org)
	if p.Timezone == "" {
		p.Timezone = strings.TrimSpace(o.HoursTimezone)
	}
	// Stored time zones default to Australia/Adelaide; that only means
	// something for an Australian business.
	if p.CountryCode != "AU" && strings.HasPrefix(p.Timezone, "Australia/") {
		p.Timezone = ""
	}
	if _, err := time.LoadLocation(p.Timezone); p.Timezone == "" || err != nil {
		p.Timezone = TimezoneForCountry(p.CountryCode)
		if p.Timezone == "" {
			p.Timezone = DefaultTimezone
		}
	}
	p.Source = SourceDefault
	if evidence {
		p.Source = SourceInferred
	}
	return p, evidence
}

func hasSettingsRow(db *gorm.DB, org string) bool {
	for _, t := range []string{"billing_settings", "organization_settings"} {
		if !hasTable(db, t) {
			continue
		}
		var n int64
		db.Raw("SELECT COUNT(*) FROM "+t+" WHERE organization_id = ?", org).Row().Scan(&n)
		if n > 0 {
			return true
		}
	}
	return false
}

// Backfill fills country/currency/time zone on organisations that have no
// currency yet. Returns how many were updated.
func Backfill(db *gorm.DB) (int, error) {
	var ids []string
	if err := db.Raw("SELECT id FROM organizations WHERE COALESCE(currency,'') = '' OR COALESCE(country_code,'') = ''").Scan(&ids).Error; err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		p, _ := Infer(db, id)
		upd := map[string]interface{}{"country_code": p.CountryCode, "currency": p.Currency, "timezone": p.Timezone, "region_source": p.Source}
		if err := db.Table("organizations").Where("id = ?", id).Updates(upd).Error; err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
