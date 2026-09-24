package mrp

import (
	"errors"
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

func TestRequired(t *testing.T) {
	cases := []struct{ per, out, qty, want int }{
		{2, 1, 5, 10},
		{1, 4, 5, 2}, // 1 per 4 units → 1.25 → 2
		{3, 2, 4, 6}, // exact
		{0, 1, 5, 0},
		{5, 0, 2, 10}, // bad output qty treated as 1
	}
	for _, c := range cases {
		if got := Required(c.per, c.out, c.qty); got != c.want {
			t.Errorf("Required(%d,%d,%d)=%d want %d", c.per, c.out, c.qty, got, c.want)
		}
	}
}

func TestRequirementsMergesAndFlagsShortage(t *testing.T) {
	comps := []BOMComponent{{ProductID: "a", Quantity: 2}, {ProductID: "b", Quantity: 1}, {ProductID: "a", Quantity: 1}}
	reqs := Requirements(comps, 1, 4, map[string]int{"a": 10, "b": 4}, map[string]float64{"a": 1.5, "b": 2})
	if len(reqs) != 2 {
		t.Fatal(reqs)
	}
	if reqs[0].Required != 12 || reqs[0].Shortage != 2 || reqs[0].LineCost != 18 {
		t.Fatalf("a: %+v", reqs[0])
	}
	if reqs[1].Required != 4 || reqs[1].Shortage != 0 {
		t.Fatalf("b: %+v", reqs[1])
	}
	if uc := UnitCost(comps, 2, map[string]float64{"a": 1.5, "b": 2}); uc != 3.25 {
		t.Fatal("unit cost", uc)
	}
}

func TestCompleteConsumesStock(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Skip(err)
	}
	if err := db.AutoMigrate(&models.Product{}, &models.InventoryMovement{}); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	org := "mrp-test-org"
	db.Exec("DELETE FROM products WHERE organization_id = ?", org)
	db.Exec("DELETE FROM inventory_movements WHERE organization_id = ?", org)
	db.Exec("DELETE FROM mrp_work_orders WHERE org_id = ?", org)
	for _, p := range []models.Product{
		{ID: "mrp-table", OrganizationID: org, Name: "Table", SKU: "MRP-T", CurrentStock: 1},
		{ID: "mrp-leg", OrganizationID: org, Name: "Leg", SKU: "MRP-L", CurrentStock: 10, CostPrice: 5},
		{ID: "mrp-top", OrganizationID: org, Name: "Top", SKU: "MRP-P", CurrentStock: 2, CostPrice: 40},
	} {
		if err := db.Omit("Organization", "Category", "Brand").Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	b := BOM{OrgID: org, ProductID: "mrp-table", ProductName: "Table", Name: "Table", OutputQty: 1,
		Components: []BOMComponent{{ProductID: "mrp-leg", ProductName: "Leg", Quantity: 4}, {ProductID: "mrp-top", ProductName: "Top", Quantity: 1}}}
	if err := db.Create(&b).Error; err != nil {
		t.Fatal(err)
	}
	h := NewHandler(db)
	w := WorkOrder{OrgID: org, Number: "WO-T1", BOMID: b.ID, ProductID: "mrp-table", Quantity: 3, Status: StatusPlanned}
	db.Create(&w)
	// 3 tables need 12 legs (have 10) and 3 tops (have 2).
	_, err = h.Complete(org, w.ID, "u1")
	var se *ShortageError
	if !errors.As(err, &se) || len(se.Items) != 2 {
		t.Fatalf("expected shortage, got %v", err)
	}
	db.Model(&WorkOrder{}).Where("id = ?", w.ID).Update("quantity", 2)
	if _, err := h.Complete(org, w.ID, "u1"); err != nil {
		t.Fatal(err)
	}
	stock := func(id string) int {
		var p models.Product
		db.Omit("Organization").Where("id = ?", id).First(&p)
		return p.CurrentStock
	}
	if stock("mrp-leg") != 2 || stock("mrp-top") != 0 || stock("mrp-table") != 3 {
		t.Fatalf("stock legs=%d tops=%d tables=%d", stock("mrp-leg"), stock("mrp-top"), stock("mrp-table"))
	}
	var n int64
	db.Model(&models.InventoryMovement{}).Where("organization_id = ? AND reference = ?", org, "WO-T1").Count(&n)
	if n != 3 {
		t.Fatal("movements", n)
	}
	if _, err := h.Complete(org, w.ID, "u1"); err == nil {
		t.Fatal("completed twice")
	}
}
