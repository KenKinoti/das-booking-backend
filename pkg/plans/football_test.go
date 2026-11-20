package plans

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Football analysis (v2.16) is a sellable module with a /football rule and
// part of the hospitality bundle; databases seeded before get it once.
func TestFootballModuleAndBundleAddition(t *testing.T) {
	if r, ok := MatchRule("/api/v1/football/fixtures/1/analysis"); !ok || r.Modules[0] != "football" {
		t.Fatalf("rule %+v", r)
	}
	if _, ok := MatchRule("/api/v1/super-admin/football/settings"); ok {
		t.Fatal("platform settings are not a module route")
	}
	if ModuleLabel("football") != "Football analysis" {
		t.Fatal("label")
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	schema := "plans_fb_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	admin.Exec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var ind Industry
	db.Where("key = 'hospitality'").First(&ind)
	if !strings.Contains(strings.Join(ind.Modules, ","), "football") {
		t.Fatalf("fresh bundle %v", ind.Modules)
	}
	// A database seeded before v2.16: added once; removal sticks.
	db.Model(&Industry{}).Where("key = 'hospitality'").Select("modules").Updates(&Industry{Modules: []string{"pos", "inventory"}})
	db.Where("key = ?", "added:hospitality:football").Delete(&Meta{})
	if err := Seed(db); err != nil {
		t.Fatal(err)
	}
	db.Where("key = 'hospitality'").First(&ind)
	if strings.Join(ind.Modules, ",") != "pos,inventory,football" {
		t.Fatalf("upgrade %v", ind.Modules)
	}
	db.Model(&Industry{}).Where("key = 'hospitality'").Select("modules").Updates(&Industry{Modules: []string{"pos"}})
	Seed(db)
	db.Where("key = 'hospitality'").First(&ind)
	if strings.Join(ind.Modules, ",") != "pos" {
		t.Fatalf("re-added %v", ind.Modules)
	}
	var m Module
	if db.Where("key = 'football'").First(&m).Error != nil || !m.Active || m.MonthlyPriceUSD <= 0 {
		t.Fatalf("module %+v", m)
	}
}
