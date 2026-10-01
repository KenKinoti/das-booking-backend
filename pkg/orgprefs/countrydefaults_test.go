package orgprefs

import (
	"strings"
	"testing"
)

func TestCountryDefaultsTable(t *testing.T) {
	for cc, want := range map[string]struct {
		label string
		rate  float64
		tz    string
	}{"KE": {"VAT", 16, "Africa/Nairobi"}, "AU": {"GST", 10, "Australia/Sydney"}, "UG": {"VAT", 18, "Africa/Kampala"}, "TZ": {"VAT", 18, "Africa/Dar_es_Salaam"},
		"RW": {"VAT", 18, "Africa/Kigali"}, "NG": {"VAT", 7.5, "Africa/Lagos"}, "GH": {"VAT", 15, "Africa/Accra"}, "ZA": {"VAT", 15, "Africa/Johannesburg"},
		"GB": {"VAT", 20, "Europe/London"}, "NZ": {"GST", 15, "Pacific/Auckland"}} {
		d, ok := DefaultsFor(cc)
		if !ok || d.TaxLabel != want.label || d.TaxRate != want.rate || d.Timezone != want.tz {
			t.Errorf("%s: %+v", cc, d)
		}
	}
	if d, _ := DefaultsFor("AU"); len(d.Timezones) < 5 {
		t.Error("Australia has several time zones to choose from")
	}
	if d, _ := DefaultsFor("GH"); !strings.Contains(d.Note, "NHIL") {
		t.Error("Ghana levies note")
	}
	if l, r, id := DefaultTax("KE"); l != "VAT" || r != 16 || id != "KRA PIN" {
		t.Errorf("default tax KE %s %v %s", l, r, id)
	}
}

func TestMismatchApplyDismiss(t *testing.T) {
	db := dbFor(t)
	db.Exec(`ALTER TABLE billing_settings ADD COLUMN IF NOT EXISTS tax_label varchar(30), ADD COLUMN IF NOT EXISTS default_tax_rate numeric, ADD COLUMN IF NOT EXISTS tax_number varchar(100), ADD COLUMN IF NOT EXISTS tax_number_label varchar(50)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS billing_tax_rates (id varchar(64) PRIMARY KEY, organization_id varchar(255), name varchar(100), rate numeric, is_default boolean, created_at timestamptz)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS billing_invoices (id varchar(64) PRIMARY KEY, organization_id varchar(255), tax_total numeric)`)
	id := org(t, db, map[string]interface{}{"address_country": "Kenya"})
	db.Exec("INSERT INTO billing_settings (organization_id, currency, tax_label, default_tax_rate, tax_number_label) VALUES (?, 'KES', 'GST', 10, 'ABN')", id)
	db.Exec("INSERT INTO billing_invoices (id, organization_id, tax_total) VALUES (?, ?, 10)", id+"-inv", id)
	db.Table("organizations").Where("id = ?", id).Updates(map[string]interface{}{"country_code": "KE", "currency": "KES", "timezone": "Australia/Adelaide", "region_source": "user"})

	m := CheckMismatch(db, id, false)
	if m == nil || !m.Tax || !m.Timezone || m.Message != "Your business is in Kenya but tax is set to GST 10% and time zone to Adelaide — update to VAT 16% and Africa/Nairobi?" {
		t.Fatalf("mismatch %+v", m)
	}
	// Dismissed: hidden for this exact conflict.
	if err := DismissNotice(db, id, m.Key); err != nil {
		t.Fatal(err)
	}
	if CheckMismatch(db, id, false) != nil || CheckMismatch(db, id, true) == nil {
		t.Fatal("dismiss")
	}
	p, err := ApplyDefaults(db, id, ApplyInput{Tax: true, Timezone: "Africa/Nairobi"})
	if err != nil || p.Timezone != "Africa/Nairobi" {
		t.Fatalf("apply %+v %v", p, err)
	}
	var label, idl string
	var rate, invTax float64
	db.Raw("SELECT tax_label, default_tax_rate, tax_number_label FROM billing_settings WHERE organization_id = ?", id).Row().Scan(&label, &rate, &idl)
	db.Raw("SELECT tax_total FROM billing_invoices WHERE id = ?", id+"-inv").Row().Scan(&invTax)
	if label != "VAT" || rate != 16 || idl != "KRA PIN" || invTax != 10 {
		t.Fatalf("after apply: %s %v %s invoice tax %v", label, rate, idl, invTax)
	}
	var def string
	db.Raw("SELECT name FROM billing_tax_rates WHERE organization_id = ? AND is_default", id).Row().Scan(&def)
	if def != "VAT" || CheckMismatch(db, id, true) != nil {
		t.Fatalf("default tax rate %q", def)
	}
	// A 0% rate (not VAT-registered) is a choice, not a conflict.
	db.Exec("UPDATE billing_settings SET default_tax_rate = 0, tax_label = 'VAT' WHERE organization_id = ?", id)
	if CheckMismatch(db, id, true) != nil {
		t.Fatal("0% should not be flagged")
	}
}
