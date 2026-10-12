package barstock

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// wireHooks connects the internal/handlers hooks to s exactly like
// cmd/production/routes_bar.go does.
func wireHooks(s *Service) func() {
	handlers.BarManagedProducts = s.ManagedProducts
	handlers.BarSaleHook = func(tx *gorm.DB, b handlers.BarSale) ([]string, error) {
		res, err := s.OnSale(tx, b.OrgID, b.UserID, b.LocationID, b.Txn, b.Items)
		return res.Touched, err
	}
	handlers.BarVoidHook = func(tx *gorm.DB, org, user string, t *models.POSTransaction, reason string) (map[string]bool, error) {
		res, err := s.OnVoid(tx, org, user, t, reason)
		return res.Handled, err
	}
	handlers.BarPOReceiveHook = s.OnPOReceive
	return unwireHooks
}

func unwireHooks() {
	handlers.BarManagedProducts, handlers.BarSaleHook, handlers.BarVoidHook, handlers.BarPOReceiveHook = nil, nil, nil, nil
}

// TestDBRealPOSHandler runs the real POS and purchase order handlers with
// and without the bar stock hooks: non-bar products behave exactly as
// before, bar-managed products are deducted from the bar.
func TestDBRealPOSHandler(t *testing.T) {
	e := newTestEnv(t)
	for _, m := range []any{&models.POSPayment{}, &models.InventoryMovement{}} {
		e.must(e.db.AutoMigrate(m))
	}
	t.Cleanup(unwireHooks)
	store, bar, _, items := e.setup()
	jam, beer := items["spirit_jameson_750"], items["beer_tusker_lager_500"]
	_, err := e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{Lines: []dl{{jam, 2, "unit", ptr(2400.0), 0}, {beer, 1, "pack", ptr(180.0), 0}}})
	e.must(err)
	_, err = e.s.CreateTransfer(torg, "u-sk", e.sk, TransferInput{FromLocationID: store, ToLocationID: bar, Mode: "direct", Lines: []LineInput{{jam, 2, ""}, {beer, 24, ""}}})
	e.must(err)
	tot := e.recipeOf("tot", jam)
	e.must(e.db.Model(&models.Product{}).Where("id IN ?", []string{tot, beer}).Update("selling_price", 300).Error)
	shirt := models.Product{ID: uuid.NewString(), OrganizationID: torg, Name: "T-shirt", SKU: "TS-" + uuid.NewString()[:6], SellingPrice: 1000, CurrentStock: 5, IsActive: true}
	e.must(e.db.Omit("Organization", "Category", "Brand", "InventoryItems", "POSItems", "PurchaseOrderItems").Create(&shirt).Error)

	gin.SetMode(gin.TestMode)
	h := handlers.NewHandler(e.db, &config.Config{})
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("org_id", torg); c.Set("user_id", "u-bt1"); c.Set("user_role", "staff") })
	r.POST("/pos", h.CreatePOSTransaction)
	r.POST("/pos/:id/void", h.VoidPOSTransaction)
	r.POST("/po/:id/receive", h.ReceivePurchaseOrder)
	post := func(path, body string) (int, map[string]any) {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	sale := func(lines string) string {
		return `{"items":[` + lines + `],"payments":[{"method":"cash","amount":100000}],"tax_rate":16,"tax_inclusive":true}`
	}
	shirtMovements := func() []models.InventoryMovement {
		var mv []models.InventoryMovement
		e.db.Where("product_id = ?", shirt.ID).Order("created_at").Find(&mv)
		return mv
	}
	pick := func(out map[string]any) string {
		tx := out["transaction"].(map[string]any)
		items := []string{}
		for _, it := range tx["items"].([]any) {
			m := it.(map[string]any)
			items = append(items, fmt.Sprint(m["item_name"], m["quantity"], m["unit_price"], m["total_price"], m["tax_amount"]))
		}
		return fmt.Sprint(tx["status"], tx["sub_total"], tx["tax_amount"], tx["total_amount"], tx["change_amount"], items)
	}

	// 1. Without bar stock (hooks nil): the legacy behaviour.
	unwireHooks()
	code, before := post("/pos", sale(fmt.Sprintf(`{"product_id":%q,"quantity":2}`, shirt.ID)))
	if code != 201 {
		t.Fatalf("legacy sale = %d %v", code, before)
	}
	mv1 := shirtMovements()
	if e.product(shirt.ID).CurrentStock != 3 || len(mv1) != 1 || mv1[0].MovementType != "out" || mv1[0].Quantity != 2 || mv1[0].PreviousQuantity != 5 || mv1[0].NewQuantity != 3 {
		t.Fatalf("legacy stock/movements: %d %+v", e.product(shirt.ID).CurrentStock, mv1)
	}
	// 2. With bar stock wired: the same sale gives the same result.
	wireHooks(e.s)
	code, after := post("/pos", sale(fmt.Sprintf(`{"product_id":%q,"quantity":2}`, shirt.ID)))
	if code != 201 || pick(before) != pick(after) {
		t.Fatalf("hooked non-bar sale differs:\n%v\n%v", pick(before), pick(after))
	}
	mv2 := shirtMovements()
	if e.product(shirt.ID).CurrentStock != 1 || len(mv2) != 2 || mv2[1].PreviousQuantity != 3 || mv2[1].NewQuantity != 1 || mv2[1].ReferenceType != "sale" {
		t.Fatalf("hooked non-bar stock/movements: %d %+v", e.product(shirt.ID).CurrentStock, mv2)
	}
	var barRows int64
	e.db.Model(&SaleLine{}).Count(&barRows)
	if barRows != 0 {
		t.Errorf("non-bar sale wrote bar sale lines: %d", barRows)
	}
	// Out of stock still blocks non-bar products.
	code, out := post("/pos", sale(fmt.Sprintf(`{"product_id":%q,"quantity":5}`, shirt.ID)))
	if code != 409 || !strings.Contains(fmt.Sprint(out["error"]), "in stock") {
		t.Errorf("non-bar oversell = %d %v", code, out)
	}
	// 3. A mixed sale: bar items never block (tot product has current_stock from availability, beer 1:1).
	e.must(e.db.Model(&models.Product{}).Where("id = ?", tot).Update("current_stock", 0).Error)
	code, out = post("/pos", sale(fmt.Sprintf(`{"product_id":%q,"quantity":4},{"product_id":%q,"quantity":3},{"product_id":%q,"quantity":1}`, tot, beer, shirt.ID)))
	if code != 201 {
		t.Fatalf("mixed sale = %d %v", code, out)
	}
	txID := out["transaction"].(map[string]any)["id"].(string)
	if e.stock(bar, jam) != 1500-100 || e.stock(bar, beer) != 21 || e.product(shirt.ID).CurrentStock != 0 {
		t.Errorf("mixed sale stock: jam %v beer %v shirt %d", e.stock(bar, jam), e.stock(bar, beer), e.product(shirt.ID).CurrentStock)
	}
	var legacyBar int64
	e.db.Model(&models.InventoryMovement{}).Where("product_id IN ?", []string{tot, beer, jam}).Count(&legacyBar)
	if legacyBar != 0 {
		t.Errorf("bar products got legacy movements: %d", legacyBar)
	}
	if p := e.product(beer); p.CurrentStock != 22 { // 21 at the bar + 1 in the store
		t.Errorf("beer current_stock synced = %d", p.CurrentStock)
	}
	if p := e.product(tot); p.CurrentStock != 56 {
		t.Errorf("tot availability = %d", p.CurrentStock)
	}
	// 4. Void through the real handler: bar stock and the T-shirt both come back.
	code, out = post("/pos/"+txID+"/void", `{"reason":"test"}`)
	if code != 200 {
		t.Fatalf("void = %d %v", code, out)
	}
	if e.stock(bar, jam) != 1500 || e.stock(bar, beer) != 24 || e.product(shirt.ID).CurrentStock != 1 {
		t.Errorf("after void: jam %v beer %v shirt %d", e.stock(bar, jam), e.stock(bar, beer), e.product(shirt.ID).CurrentStock)
	}
	if p := e.product(tot); p.CurrentStock != 60 {
		t.Errorf("tot availability after void = %d (the void must not add 4 to it)", p.CurrentStock)
	}
	// 5. block_when_out turns an oversell into a 409 and rolls back the whole sale.
	_, err = e.s.applySettings(e.db, torg, "u-mgr", settingsInput{BlockWhenOut: ptr(true)})
	e.must(err)
	code, out = post("/pos", sale(fmt.Sprintf(`{"product_id":%q,"quantity":61}`, tot)))
	if code != 409 || !strings.Contains(fmt.Sprint(out["error"]), "Not enough") {
		t.Errorf("blocked sale = %d %v", code, out)
	}
	var n int64
	e.db.Model(&models.POSTransaction{}).Count(&n)
	if n != 3 {
		t.Errorf("transactions after a blocked sale = %d", n)
	}
	// 6. Receiving a PO on the Suppliers page books bar items into the main store.
	sup := e.supplier("KWAL")
	po := models.PurchaseOrder{ID: uuid.NewString(), OrganizationID: torg, SupplierID: sup, OrderNumber: "PO-R1", Status: "sent", OrderDate: e.clock, CreatedBy: "u-mgr"}
	e.must(e.db.Omit("Organization", "Supplier", "Creator", "Items").Create(&po).Error)
	jl := models.PurchaseOrderItem{ID: uuid.NewString(), PurchaseOrderID: po.ID, ProductID: jam, Quantity: 6, UnitCost: 3000}
	sl := models.PurchaseOrderItem{ID: uuid.NewString(), PurchaseOrderID: po.ID, ProductID: shirt.ID, Quantity: 4, UnitCost: 400}
	e.must(e.db.Omit("PurchaseOrder", "Product").Create(&jl).Error)
	e.must(e.db.Omit("PurchaseOrder", "Product").Create(&sl).Error)
	code, out = post("/po/"+po.ID+"/receive", `{"receive_all":true}`)
	if code != 200 {
		t.Fatalf("PO receive = %d %v", code, out)
	}
	if e.stock(store, jam) != 4500 || e.product(jam).CurrentStock != 8 || e.product(shirt.ID).CurrentStock != 5 {
		t.Errorf("PO receive: store jam %v, jam current %d, shirt %d", e.stock(store, jam), e.product(jam).CurrentStock, e.product(shirt.ID).CurrentStock)
	}
	it, _ := e.s.item(e.db, torg, jam)
	if !near(it.AvgCost, (1500*3.2+4500*4)/6000) {
		t.Errorf("avg after PO receive = %v", it.AvgCost)
	}
	var jm models.InventoryMovement
	e.must(e.db.Where("product_id = ? AND reference_type = 'purchase_order'", jam).First(&jm).Error)
	if jm.NewQuantity != 8 || jm.Quantity != 6 {
		t.Errorf("legacy PO movement for the bar item = %+v", jm)
	}
}
