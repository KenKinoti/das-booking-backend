package invoicing

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// orgIn adds an organisation in country (the columns pkg/orgprefs reads).
func orgIn(t *testing.T, country, currency, tz string) string {
	t.Helper()
	db, err := gorm.Open(postgres.Open(os.Getenv("TEST_DATABASE_URL")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE IF NOT EXISTS organizations (id varchar(255) PRIMARY KEY, name varchar(255))`,
		`ALTER TABLE organizations ADD COLUMN IF NOT EXISTS country_code varchar(2), ADD COLUMN IF NOT EXISTS currency varchar(3),
			ADD COLUMN IF NOT EXISTS timezone varchar(64), ADD COLUMN IF NOT EXISTS region_source varchar(16)`,
	} {
		if err := db.Exec(s).Error; err != nil {
			t.Fatal(err)
		}
	}
	org := "test-org-" + uuid.NewString()
	if err := db.Exec(`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES (?, 'Tax Test Co', ?, ?, ?, 'user')`,
		org, country, currency, tz).Error; err != nil {
		t.Fatal(err)
	}
	return org
}

func rateList(t *testing.T, res map[string]interface{}) string {
	t.Helper()
	list, _ := res["data"].([]interface{})
	var out []string
	for _, x := range list {
		r := x.(map[string]interface{})
		s := fmt.Sprintf("%s %v%%", r["name"], r["rate"])
		if r["is_default"] == true {
			s += "*"
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// New organisations start with their country's tax (HIGH bug: everyone got
// "GST 10%"), and the invoice settings agree with the default rate.
func TestTaxRatesFollowCountry(t *testing.T) {
	r, _ := setup(t)
	for _, c := range []struct{ country, cur, tz, want, label string }{
		{"KE", "KES", "Africa/Nairobi", "VAT 16%*, VAT exempt 0%, Zero-rated 0%", "VAT"},
		{"AU", "AUD", "Australia/Sydney", "GST 10%*, GST Free 0%", "GST"},
		{"UG", "UGX", "Africa/Kampala", "VAT 18%*, VAT exempt 0%, Zero-rated 0%", "VAT"},
	} {
		org := orgIn(t, c.country, c.cur, c.tz)
		code, res := call(t, r, org, "GET", "/api/tax-rates", nil)
		got := rateList(t, res)
		// the two 0% rows have no fixed order between them
		if code != 200 || !sameRates(got, c.want) {
			t.Errorf("%s: %d %s, want %s", c.country, code, got, c.want)
		}
		// Seeded once: a second call returns the same rows.
		if _, res2 := call(t, r, org, "GET", "/api/tax-rates", nil); rateList(t, res2) != got {
			t.Errorf("%s: reseeded: %s", c.country, rateList(t, res2))
		}
		_, res = call(t, r, org, "GET", "/api/settings", nil)
		s, _ := data(res)["settings"].(map[string]interface{})
		if s["tax_label"] != c.label || !strings.HasPrefix(c.want, fmt.Sprintf("%s %v%%*", c.label, s["default_tax_rate"])) {
			t.Errorf("%s: settings %v %v", c.country, s["tax_label"], s["default_tax_rate"])
		}
	}

	// Rates an organisation already has are never changed (the country
	// notice offers the fix instead).
	org := orgIn(t, "KE", "KES", "Africa/Nairobi")
	if code, _ := call(t, r, org, "POST", "/api/tax-rates", map[string]interface{}{"name": "GST", "rate": 10, "is_default": true}); code != 200 {
		t.Fatalf("create rate: %d", code)
	}
	if _, res := call(t, r, org, "GET", "/api/tax-rates", nil); rateList(t, res) != "GST 10%*" {
		t.Fatalf("existing rates changed: %s", rateList(t, res))
	}
}

func sameRates(a, b string) bool {
	x, y := strings.Split(a, ", "), strings.Split(b, ", ")
	if len(x) != len(y) || x[0] != y[0] {
		return false
	}
	seen := map[string]int{}
	for _, s := range x {
		seen[s]++
	}
	for _, s := range y {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
