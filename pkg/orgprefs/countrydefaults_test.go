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

func TestStarterTaxRates(t *testing.T) {
	str := func(rs []StarterRate) string {
		var parts []string
		for _, r := range rs {
			s := taxText(r.Name, r.Rate)
			if r.IsDefault {
				s += "*"
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ", ")
	}
	for cc, want := range map[string]string{
		"KE": "VAT 16%*, Zero-rated 0%, VAT exempt 0%",
		"ke": "VAT 16%*, Zero-rated 0%, VAT exempt 0%",
		"UG": "VAT 18%*, Zero-rated 0%, VAT exempt 0%",
		"GB": "VAT 20%*, Zero-rated 0%, VAT exempt 0%",
		"NZ": "GST 15%*, Zero-rated 0%, GST exempt 0%",
		"AU": "GST 10%*, GST Free 0%",
		"US": "Sales tax 0%*",
		// Unknown country: the long-standing Australian default.
		"": "GST 10%*, GST Free 0%",
	} {
		if got := str(StarterTaxRates(cc)); got != want {
			t.Errorf("%q: %s, want %s", cc, got, want)
		}
	}
	// Only Australia (or an unknown country) starts with GST 10%.
	for cc := range countryDefaults {
		if cc == "AU" {
			continue
		}
		for _, r := range StarterTaxRates(cc) {
			if strings.EqualFold(r.Name, "GST") && r.Rate == 10 {
				t.Errorf("%s starts with GST 10%%", cc)
			}
		}
	}
}

// Older versions seeded "GST 10%" as the default tax rate for every
// organisation; a Kenyan business with that default (even with VAT 16% in its
// invoice settings) sees the notice, nothing changes until it confirms.
func TestMismatchDefaultTaxRateRow(t *testing.T) {
	db := dbFor(t)
	db.Exec(`ALTER TABLE billing_settings ADD COLUMN IF NOT EXISTS tax_label varchar(30), ADD COLUMN IF NOT EXISTS default_tax_rate numeric, ADD COLUMN IF NOT EXISTS tax_number varchar(100), ADD COLUMN IF NOT EXISTS tax_number_label varchar(50)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS billing_tax_rates (id varchar(64) PRIMARY KEY, organization_id varchar(255), name varchar(100), rate numeric, is_default boolean, created_at timestamptz)`)
	id := org(t, db, map[string]interface{}{"address_country": "Kenya"})
	db.Table("organizations").Where("id = ?", id).Updates(map[string]interface{}{"country_code": "KE", "currency": "KES", "timezone": "Africa/Nairobi", "region_source": "user"})
	db.Exec("INSERT INTO billing_settings (organization_id, currency, tax_label, default_tax_rate, tax_number_label) VALUES (?, 'KES', 'VAT', 16, 'KRA PIN')", id)
	db.Exec("INSERT INTO billing_tax_rates (id, organization_id, name, rate, is_default, created_at) VALUES (?, ?, 'GST', 10, true, now()), (?, ?, 'GST Free', 0, false, now())", id+"-gst", id, id+"-free", id)
	rows := func() string {
		var out []string
		var list []struct {
			Name      string
			Rate      float64
			IsDefault bool
		}
		db.Raw("SELECT name, rate, is_default FROM billing_tax_rates WHERE organization_id = ? ORDER BY is_default DESC, rate DESC, name", id).Scan(&list)
		for _, r := range list {
			s := taxText(r.Name, r.Rate)
			if r.IsDefault {
				s += "*"
			}
			out = append(out, s)
		}
		return strings.Join(out, ", ")
	}

	m := CheckMismatch(db, id, false)
	if m == nil || !m.Tax || m.Timezone || m.CurrentTaxLabel != "GST" || m.CurrentTaxRate != 10 ||
		m.Message != "Your business is in Kenya but tax is set to GST 10% — update to VAT 16%?" {
		t.Fatalf("mismatch %+v", m)
	}
	// Checking never changes the saved rates.
	if got := rows(); got != "GST 10%*, GST Free 0%" {
		t.Fatalf("rates changed by the check: %s", got)
	}
	// "Keep my settings" hides this exact conflict.
	if err := DismissNotice(db, id, m.Key); err != nil {
		t.Fatal(err)
	}
	if CheckMismatch(db, id, false) != nil {
		t.Fatal("dismissed notice still shown")
	}
	// The one-click fix: VAT 16% becomes the default; the old rate stays, unused.
	if _, err := ApplyDefaults(db, id, ApplyInput{Tax: true}); err != nil {
		t.Fatal(err)
	}
	if got := rows(); got != "VAT 16%*, GST 10%, GST Free 0%" {
		t.Fatalf("after apply: %s", got)
	}
	if m := CheckMismatch(db, id, true); m != nil {
		t.Fatalf("still a mismatch after apply: %+v", m)
	}
	// A 0% default (not VAT-registered) or a non-default GST row is not a conflict.
	db.Exec("UPDATE billing_tax_rates SET is_default = (name = 'GST Free') WHERE organization_id = ?", id)
	if m := CheckMismatch(db, id, true); m != nil {
		t.Fatalf("0%% default flagged: %+v", m)
	}
}
