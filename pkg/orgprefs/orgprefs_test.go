package orgprefs

import (
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestValidate(t *testing.T) {
	cur := Prefs{CountryCode: "AU", Currency: "AUD", Timezone: "Australia/Sydney", Source: SourceInferred}
	// A new country brings its currency and time zone.
	p, err := Validate(cur, Input{CountryCode: "ke"})
	if err != nil || p.CountryCode != "KE" || p.Currency != "KES" || p.Timezone != "Africa/Nairobi" {
		t.Fatalf("kenya: %+v %v", p, err)
	}
	// Country names work too; the currency can be overridden.
	p, err = Validate(cur, Input{CountryCode: "Japan", Currency: "usd"})
	if err != nil || p.CountryCode != "JP" || p.Currency != "USD" || p.Decimals != 2 {
		t.Fatalf("japan/usd: %+v %v", p, err)
	}
	p, _ = Validate(cur, Input{CountryCode: "JP"})
	if p.Currency != "JPY" || p.Decimals != 0 {
		t.Fatalf("jpy: %+v", p)
	}
	// Currency only keeps the country.
	p, _ = Validate(cur, Input{Currency: "NZD"})
	if p.CountryCode != "AU" || p.Currency != "NZD" {
		t.Fatalf("currency only: %+v", p)
	}
	// Currency only with no country picks the currency's country.
	p, err = Validate(Prefs{}, Input{Currency: "KES"})
	if err != nil || p.CountryCode != "KE" {
		t.Fatalf("infer country: %+v %v", p, err)
	}
	for _, bad := range []Input{{CountryCode: "Atlantis"}, {Currency: "XXX"}, {CountryCode: "AU", Timezone: "Mars/Base"}} {
		if _, err := Validate(cur, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if _, err := Validate(Prefs{}, Input{}); err == nil {
		t.Error("no country accepted")
	}
	// A user-chosen time zone isn't replaced when the country changes.
	p, _ = Validate(Prefs{CountryCode: "AU", Currency: "AUD", Timezone: "Australia/Perth", Source: SourceUser}, Input{CountryCode: "NZ"})
	if p.Timezone != "Australia/Perth" {
		t.Fatalf("tz replaced: %+v", p)
	}
}

func dbFor(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE IF NOT EXISTS organizations (id varchar(255) PRIMARY KEY, name varchar(255), address_street varchar(255), address_suburb varchar(100),
			address_state varchar(50), address_postcode varchar(10), address_country varchar(100) DEFAULT 'Australia', hours_timezone varchar(50),
			created_at timestamptz, updated_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE IF NOT EXISTS billing_settings (organization_id varchar(255) PRIMARY KEY, currency varchar(3) DEFAULT 'AUD')`,
		`CREATE TABLE IF NOT EXISTS organization_settings (id varchar(36) PRIMARY KEY, organization_id varchar(36) UNIQUE, timezone varchar(50), currency varchar(3), updated_at timestamptz)`,
		`CREATE TABLE IF NOT EXISTS plan_subscriptions (id varchar(64) PRIMARY KEY, organization_id varchar(255) UNIQUE, tier varchar(20), currency varchar(3), default_currency varchar(3))`,
		`CREATE TABLE IF NOT EXISTS smallbiz_settings (organization_id varchar(64) PRIMARY KEY, currency varchar(3), opening_balance bigint)`,
	} {
		if err := db.Exec(s).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func org(t *testing.T, db *gorm.DB, fields map[string]interface{}) string {
	id := uuid.NewString()
	fields["id"], fields["name"] = id, "Org "+id[:6]
	if err := db.Table("organizations").Create(fields).Error; err != nil {
		t.Fatal(err)
	}
	return id
}

func TestInferAndBackfill(t *testing.T) {
	db := dbFor(t)
	// Invoicing in KES with the default "Australia" address → Kenya.
	kes := org(t, db, map[string]interface{}{"address_country": "Australia"})
	db.Exec("INSERT INTO billing_settings (organization_id, currency) VALUES (?, 'KES')", kes)
	// A real Japanese address, no settings → Japan / JPY.
	jp := org(t, db, map[string]interface{}{"address_country": "Japan", "address_suburb": "Shibuya"})
	// Invoicing in AUD (the old default) → Australia, inferred from a real settings row.
	au := org(t, db, map[string]interface{}{"address_country": "Australia"})
	db.Exec("INSERT INTO billing_settings (organization_id, currency) VALUES (?, 'AUD')", au)
	// Plan default currency only.
	gbp := org(t, db, map[string]interface{}{"address_country": ""})
	db.Exec("INSERT INTO plan_subscriptions (id, organization_id, tier, currency, default_currency) VALUES (?, ?, 'small', 'USD', 'GBP')", uuid.NewString(), gbp)
	// Nothing at all → AU / AUD, still needs setup.
	none := org(t, db, map[string]interface{}{"address_country": "Australia"})

	if _, err := Backfill(db); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id, country, cur string
		setup            bool
	}{{kes, "KE", "KES", false}, {jp, "JP", "JPY", false}, {au, "AU", "AUD", false}, {gbp, "GB", "GBP", false}, {none, "AU", "AUD", true}}
	for _, c := range cases {
		p := Get(db, c.id)
		if p.CountryCode != c.country || p.Currency != c.cur || p.NeedsSetup != c.setup {
			t.Errorf("%s: got %+v, want %s/%s setup=%v", c.id[:6], p, c.country, c.cur, c.setup)
		}
	}
	if Get(db, jp).Timezone != "Asia/Tokyo" {
		t.Errorf("jp tz %q", Get(db, jp).Timezone)
	}
	// Unknown organisation → defaults.
	if Currency(db, "missing-"+uuid.NewString()) != DefaultCurrency {
		t.Error("missing org default")
	}
}

func TestSetMirrors(t *testing.T) {
	db := dbFor(t)
	id := org(t, db, map[string]interface{}{"address_country": "Australia"})
	db.Exec("INSERT INTO billing_settings (organization_id, currency) VALUES (?, 'AUD')", id)
	db.Exec("INSERT INTO organization_settings (id, organization_id, timezone, currency) VALUES (?, ?, 'Australia/Sydney', 'AUD')", uuid.NewString(), id)
	db.Exec("INSERT INTO plan_subscriptions (id, organization_id, tier, currency, default_currency) VALUES (?, ?, 'small', 'USD', 'AUD')", uuid.NewString(), id)
	db.Exec("INSERT INTO smallbiz_settings (organization_id, currency, opening_balance) VALUES (?, 'AUD', 0)", id)

	p, err := Set(db, id, Input{CountryCode: "KE"}, SourceUser)
	if err != nil {
		t.Fatal(err)
	}
	if p.Currency != "KES" || p.CountryCode != "KE" || p.NeedsSetup || p.Source != SourceUser {
		t.Fatalf("set: %+v", p)
	}
	var billing, orgSet, plan, hub, addr string
	db.Raw("SELECT currency FROM billing_settings WHERE organization_id = ?", id).Scan(&billing)
	db.Raw("SELECT currency FROM organization_settings WHERE organization_id = ?", id).Scan(&orgSet)
	db.Raw("SELECT default_currency FROM plan_subscriptions WHERE organization_id = ?", id).Scan(&plan)
	db.Raw("SELECT currency FROM smallbiz_settings WHERE organization_id = ?", id).Scan(&hub)
	db.Raw("SELECT address_country FROM organizations WHERE id = ?", id).Scan(&addr)
	if billing != "KES" || orgSet != "KES" || plan != "KES" || hub != "KES" || addr != "Kenya" {
		t.Fatalf("mirrors: billing=%s org=%s plan=%s hub=%s addr=%s", billing, orgSet, plan, hub, addr)
	}
	// A hub with an opening balance keeps its base currency.
	db.Exec("UPDATE smallbiz_settings SET opening_balance = 5000 WHERE organization_id = ?", id)
	if _, err := Set(db, id, Input{CountryCode: "JP"}, SourceUser); err != nil {
		t.Fatal(err)
	}
	db.Raw("SELECT currency FROM smallbiz_settings WHERE organization_id = ?", id).Scan(&hub)
	if hub != "KES" || Currency(db, id) != "JPY" {
		t.Fatalf("hub %s org %s", hub, Currency(db, id))
	}
	if _, err := Set(db, id, Input{Currency: "XXX"}, SourceUser); err == nil {
		t.Fatal("bad currency saved")
	}
}
