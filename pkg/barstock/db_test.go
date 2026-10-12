package barstock

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

const torg = "org-bar"

// testEnv is a fresh schema on TEST_DATABASE_URL with the tables bar stock
// touches, an organisation in Kenya (Africa/Nairobi, KES) and a fake clock.
type testEnv struct {
	t     *testing.T
	db    *gorm.DB
	s     *Service
	clock time.Time
	mgr   access // approve on everything
	bt    access // bartender: view + create
	sk    access // store keeper: view + create + edit
}

func newTestEnv(t *testing.T) *testEnv {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	schema := "bar_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	admin.Exec("CREATE SCHEMA " + schema)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent),
		DisableForeignKeyConstraintWhenMigrating: true, NowFunc: func() time.Time { return time.Now().UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := db.DB(); err == nil {
			d.Close()
		}
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if d, err := admin.DB(); err == nil {
			d.Close()
		}
	})
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) PRIMARY KEY, name text, email text, phone text, address_street text, address_suburb text,
			address_state text, address_postcode text, country_code varchar(2), currency varchar(3), timezone text, region_source text, address_country text)`,
		`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES ('org-bar', 'Club Test', 'KE', 'KES', 'Africa/Nairobi', 'user'),
			('org-other', 'Other', 'KE', 'KES', 'Africa/Nairobi', 'user')`,
		`CREATE TABLE users (id varchar(255) PRIMARY KEY, organization_id varchar(255), email text, password_hash text NOT NULL DEFAULT '', first_name text, last_name text, role text,
			is_active boolean DEFAULT true, deleted_at timestamptz)`,
		`INSERT INTO users (id, organization_id, email, first_name, last_name, role) VALUES ('u-mgr','org-bar','mgr@club.test','Mary','Manager','manager'),
			('u-bt1','org-bar','bt1@club.test','Ben','Bartender','staff'), ('u-bt2','org-bar','bt2@club.test','Bea','Bartender','staff'),
			('u-sk','org-bar','sk@club.test','Sam','Store','staff')`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []any{&models.Product{}, &models.ProductCategory{}, &models.InventoryLocation{}, &models.Supplier{}, &models.PurchaseOrder{},
		&models.PurchaseOrderItem{}, &models.CashDrawer{}, &models.POSTransaction{}, &models.POSItem{}} {
		if err := db.AutoMigrate(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, db: db, clock: time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)} // Fri 18:00 in Nairobi
	e.s = New(db)
	e.s.Now = func() time.Time { return e.clock }
	n := 0
	e.s.NextPONumber = func(tx *gorm.DB, org string) string { n++; return fmt.Sprintf("PO-%05d", n) }
	e.mgr = access{Org: torg, User: "u-mgr", Role: "manager", View: true, Create: true, Edit: true, Delete: true, Approve: true}
	e.bt = access{Org: torg, User: "u-bt1", Role: "staff", View: true, Create: true}
	e.sk = access{Org: torg, User: "u-sk", Role: "staff", View: true, Create: true, Edit: true}
	return e
}

func (e *testEnv) tick(d time.Duration) { e.clock = e.clock.Add(d) }

// at sets the clock to a local Nairobi time.
func (e *testEnv) at(local string) {
	nbo, _ := time.LoadLocation("Africa/Nairobi")
	t, err := time.ParseInLocation("2006-01-02 15:04", local, nbo)
	if err != nil {
		e.t.Fatal(err)
	}
	e.clock = t.UTC()
}

func (e *testEnv) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) setup() (store, bar, vip string, items map[string]string) {
	e.t.Helper()
	in := SetupInput{TotML: ptr(25.0), DayCutoff: ptr("06:00"), CatalogKeys: []string{"spirit_jameson_750", "beer_tusker_lager_500", "spirit_bacardi_750",
		"garnish_mint", "mixer_soda_300", "mixer_lime_cordial_1l"}, MakeMenuItems: true}
	in.Locations = append(in.Locations, struct {
		Name         string `json:"name"`
		Type         string `json:"type"`
		IsMainStore  bool   `json:"is_main_store"`
		IsDefaultBar bool   `json:"is_default_bar"`
	}{Name: "Main store", Type: "store", IsMainStore: true}, struct {
		Name         string `json:"name"`
		Type         string `json:"type"`
		IsMainStore  bool   `json:"is_main_store"`
		IsDefaultBar bool   `json:"is_default_bar"`
	}{Name: "Main bar", Type: "bar"}, struct {
		Name         string `json:"name"`
		Type         string `json:"type"`
		IsMainStore  bool   `json:"is_main_store"`
		IsDefaultBar bool   `json:"is_default_bar"`
	}{Name: "VIP bar", Type: "bar"})
	res, err := e.s.Setup(torg, "u-mgr", in)
	e.must(err)
	if len(res.LocationsCreated) != 3 || len(res.ItemsCreated) != 6 {
		e.t.Fatalf("setup = %+v", res)
	}
	locs, _ := e.s.locations(e.db, torg)
	for _, l := range locs {
		switch l.Name {
		case "Main store":
			store = l.ID
		case "Main bar":
			bar = l.ID
		case "VIP bar":
			vip = l.ID
		}
	}
	all, _ := e.s.Items(e.db, torg, nil)
	items = map[string]string{}
	for id, it := range all {
		items[it.CatalogKey] = id
	}
	return
}

func (e *testEnv) stock(loc, pid string) float64 {
	m, err := e.s.stockAt(e.db, torg, loc, []string{pid})
	e.must(err)
	return m[pid]
}

func (e *testEnv) product(id string) models.Product {
	var p models.Product
	e.must(e.db.Where("id = ?", id).First(&p).Error)
	return p
}

func (e *testEnv) recipeOf(kind, source string) string {
	var r Recipe
	e.must(e.db.Where("organization_id = ? AND kind = ? AND source_id = ?", torg, kind, source).First(&r).Error)
	return r.POSProductID
}

func (e *testEnv) supplier(name string) string {
	s := models.Supplier{ID: uuid.NewString(), OrganizationID: torg, Name: name, IsActive: true}
	e.must(e.db.Omit("Organization", "PurchaseOrders").Create(&s).Error)
	return s.ID
}

type dl = struct {
	ProductID string   `json:"product_id"`
	Qty       float64  `json:"qty"`
	Unit      string   `json:"unit"`
	UnitCost  *float64 `json:"unit_cost"`
	Damaged   float64  `json:"damaged"`
}

// sale runs the POS sale hook inside a transaction like pos.go does.
func (e *testEnv) sale(loc string, lines map[string]int, price float64) (*models.POSTransaction, SaleResult, error) {
	txn := &models.POSTransaction{ID: uuid.NewString(), OrganizationID: torg, CashierID: "u-bt1", TransactionNumber: "S-" + uuid.NewString()[:6],
		CreatedAt: e.clock, TaxInclusive: true}
	items := []models.POSItem{}
	sub := 0.0
	for pid, q := range lines {
		p := pid
		total := float64(q) * price
		tax := math.Round(total*16/116*100) / 100
		items = append(items, models.POSItem{ID: uuid.NewString(), TransactionID: txn.ID, ProductID: &p, ItemName: e.product(pid).Name, Quantity: q,
			UnitPrice: price, TotalPrice: total, TaxRate: 16, TaxAmount: tax})
		sub += total
	}
	txn.SubTotal, txn.TotalAmount = sub, sub
	var res SaleResult
	err := e.db.Transaction(func(tx *gorm.DB) error {
		var err error
		res, err = e.s.OnSale(tx, torg, "u-bt1", loc, txn, items)
		return err
	})
	return txn, res, err
}

func TestDBSetupIdempotentAndSync(t *testing.T) {
	e := newTestEnv(t)
	store, bar, _, items := e.setup()
	st := e.s.Settings(e.db, torg)
	if st.MainStoreID == nil || *st.MainStoreID != store || st.DefaultBarID == nil || *st.DefaultBarID != bar {
		t.Fatalf("main store / default bar not set: %+v", st)
	}
	// Re-running the wizard creates nothing new.
	in := SetupInput{CatalogKeys: []string{"spirit_jameson_750", "beer_tusker_lager_500"}, MakeMenuItems: true}
	in.Locations = append(in.Locations, struct {
		Name         string `json:"name"`
		Type         string `json:"type"`
		IsMainStore  bool   `json:"is_main_store"`
		IsDefaultBar bool   `json:"is_default_bar"`
	}{Name: "MAIN BAR", Type: "bar"})
	res, err := e.s.Setup(torg, "u-mgr", in)
	e.must(err)
	if len(res.LocationsCreated)+len(res.ItemsCreated)+len(res.MenuItemsCreated) != 0 {
		t.Fatalf("second setup created %+v", res)
	}
	var n int64
	e.db.Model(&Recipe{}).Where("organization_id = ?", torg).Count(&n)
	// Jameson + Bacardi tot & bottle (4) + 1:1 for beer and soda (2).
	if n != 6 {
		t.Errorf("recipes = %d", n)
	}
	// Tusker is sold as itself: managed with an explicit 1:1 recipe.
	m, _ := e.s.ManagedProducts(e.db, torg, []string{items["beer_tusker_lager_500"], items["garnish_mint"], "nope"})
	if !m[items["beer_tusker_lager_500"]] || !m[items["garnish_mint"]] || m["nope"] {
		t.Errorf("managed = %v", m)
	}
	// Stock moves keep products.current_stock in step (floor of bottles) and menu availability.
	jam := items["spirit_jameson_750"]
	e.must(e.db.Transaction(func(tx *gorm.DB) error {
		if err := e.s.receiveStock(tx, torg, "u-mgr", store, jam, 2.5, 2400, "", "test", "t1", "", "2026-10-09", ""); err != nil {
			return err
		}
		return e.s.sync(tx, torg, []string{jam})
	}))
	if p := e.product(jam); p.CurrentStock != 2 || p.CostPrice != 2400 {
		t.Errorf("current_stock = %d cost %v, want 2 / 2400", p.CurrentStock, p.CostPrice)
	}
	if p := e.product(e.recipeOf("tot", jam)); p.CurrentStock != 75 {
		t.Errorf("tot availability = %d, want 75", p.CurrentStock)
	}
	if p := e.product(e.recipeOf("bottle", jam)); p.CurrentStock != 2 {
		t.Errorf("bottle availability = %d", p.CurrentStock)
	}
}

func TestDBDeliveriesAvgCostAndPO(t *testing.T) {
	e := newTestEnv(t)
	store, _, _, items := e.setup()
	jam, beer := items["spirit_jameson_750"], items["beer_tusker_lager_500"]
	kenya := e.supplier("Kenya Wine Agencies")
	ids, err := e.s.CreatePOs(torg, "u-mgr", CreatePOInput{Groups: []struct {
		SupplierID string `json:"supplier_id"`
		Lines      []struct {
			ProductID string   `json:"product_id"`
			QtyUnits  float64  `json:"qty_units"`
			UnitCost  *float64 `json:"unit_cost"`
		} `json:"lines"`
	}{{SupplierID: kenya, Lines: []struct {
		ProductID string   `json:"product_id"`
		QtyUnits  float64  `json:"qty_units"`
		UnitCost  *float64 `json:"unit_cost"`
	}{{jam, 12, ptr(2400.0)}, {beer, 50, ptr(180.0)}}}}})
	e.must(err)
	var po models.PurchaseOrder
	e.must(e.db.Where("id = ?", ids[0]).First(&po).Error)
	if po.Status != "draft" || po.OrderNumber != "PO-00001" || po.TotalAmount != 12*2400+50*180 {
		t.Fatalf("po = %+v", po)
	}
	// First delivery: 6 bottles of Jameson at the PO price, 1 crate of beer with 2 broken.
	_, err = e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{POID: po.ID, DeliveryNote: "DN-1", Lines: []dl{{jam, 6, "unit", nil, 0}, {beer, 2, "pack", nil, 2}}})
	e.must(err)
	it, _ := e.s.item(e.db, torg, jam)
	if !near(it.AvgCost, 3.2) || it.LastCost != 2400 {
		t.Errorf("avg = %v last = %v", it.AvgCost, it.LastCost)
	}
	if got := e.stock(store, beer); got != 48 {
		t.Errorf("beer in store = %v (50 delivered − 2 broken)", got)
	}
	var w Wastage
	e.must(e.db.Where("reason = 'damaged_on_delivery'").First(&w).Error)
	if w.Qty != 2 || w.Status != WApproved {
		t.Errorf("damaged wastage = %+v", w)
	}
	e.must(e.db.Where("id = ?", po.ID).First(&po).Error)
	if po.Status != "partially_received" {
		t.Errorf("po status = %s", po.Status)
	}
	// Second delivery at a new price: weighted average over all locations.
	e.tick(time.Hour)
	_, err = e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{POID: po.ID, Lines: []dl{{jam, 6, "unit", ptr(2700.0), 0}}})
	e.must(err)
	it, _ = e.s.item(e.db, torg, jam)
	want := (4500*3.2 + 4500*3.6) / 9000
	if !near(it.AvgCost, want) {
		t.Errorf("avg after 2nd = %v want %v", it.AvgCost, want)
	}
	if p := e.product(jam); p.CostPrice != math.Round(want*750*100)/100 || p.CurrentStock != 12 {
		t.Errorf("product = %v / %d", p.CostPrice, p.CurrentStock)
	}
	e.must(e.db.Where("id = ?", po.ID).First(&po).Error)
	if po.Status != "received" {
		t.Errorf("po status = %s", po.Status)
	}
	var ph []PriceHistory
	e.db.Where("product_id = ?", jam).Order("created_at").Find(&ph)
	if len(ph) != 2 || ph[0].UnitCost != 2400 || ph[1].UnitCost != 2700 || ph[0].SupplierID != kenya {
		t.Errorf("price history = %+v", ph)
	}
	// A closed PO can't be received again.
	if _, err := e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{POID: po.ID, Lines: []dl{{jam, 1, "unit", nil, 0}}}); err == nil {
		t.Error("received PO accepted another delivery")
	}
	// Bartenders can't receive deliveries.
	if _, err := e.s.ReceiveDelivery(torg, "u-bt1", e.bt, DeliveryInput{Lines: []dl{{jam, 1, "unit", nil, 0}}}); err == nil {
		t.Error("bartender received a delivery")
	}
	// Purchase order received on the Suppliers page (hook): into the main store.
	po2 := models.PurchaseOrder{ID: uuid.NewString(), OrganizationID: torg, SupplierID: kenya, OrderNumber: "PO-X", Status: "sent", OrderDate: e.clock, CreatedBy: "u-mgr"}
	e.must(e.db.Omit("Organization", "Supplier", "Creator", "Items").Create(&po2).Error)
	line := models.PurchaseOrderItem{ID: uuid.NewString(), PurchaseOrderID: po2.ID, ProductID: jam, Quantity: 3, UnitCost: 2700}
	e.must(e.db.Omit("PurchaseOrder", "Product").Create(&line).Error)
	var handled bool
	var newStock int
	e.must(e.db.Transaction(func(tx *gorm.DB) error {
		var err error
		handled, newStock, err = e.s.OnPOReceive(tx, torg, "u-mgr", &po2, &line, 3)
		return err
	}))
	if !handled || newStock != 15 || e.stock(store, jam) != 15*750 {
		t.Errorf("PO hook handled=%v stock=%d store=%v", handled, newStock, e.stock(store, jam))
	}
	// Non-bar product: the legacy path keeps it.
	other := models.Product{ID: uuid.NewString(), OrganizationID: torg, Name: "T-shirt", IsActive: true}
	e.must(e.db.Omit("Organization", "Category", "Brand", "InventoryItems", "POSItems", "PurchaseOrderItems").Create(&other).Error)
	ol := models.PurchaseOrderItem{ProductID: other.ID, Quantity: 2, UnitCost: 5}
	e.must(e.db.Transaction(func(tx *gorm.DB) error {
		h, _, err := e.s.OnPOReceive(tx, torg, "u-mgr", &po2, &ol, 2)
		if h {
			t.Error("non-bar product handled by bar stock")
		}
		return err
	}))
}

func TestDBTransfers(t *testing.T) {
	e := newTestEnv(t)
	store, bar, vip, items := e.setup()
	jam, beer := items["spirit_jameson_750"], items["beer_tusker_lager_500"]
	_, err := e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{Lines: []dl{{jam, 1, "pack", ptr(2400.0), 0}, {beer, 2, "pack", ptr(180.0), 0}}})
	e.must(err)
	// The bartender asks for 6 bottles of Jameson and 24 beers.
	id, err := e.s.CreateTransfer(torg, "u-bt1", e.bt, TransferInput{FromLocationID: store, ToLocationID: bar, Lines: []LineInput{{jam, 6, ""}, {beer, 1, "pack"}}})
	e.must(err)
	if e.stock(store, jam) != 9000 {
		t.Fatal("a request must not move stock")
	}
	if err := e.s.IssueTransfer(torg, "u-bt1", e.bt, id, nil); err == nil {
		t.Error("bartender issued a transfer")
	}
	// Store keeper issues 5 bottles (store short) and all the beer.
	e.must(e.s.IssueTransfer(torg, "u-sk", e.sk, id, []QtyLine{{ProductID: jam, Qty: 5}}))
	// Tusker comes in crates of 25.
	if e.stock(store, jam) != 7*750 || e.stock(store, beer) != 25 || e.stock(bar, jam) != 0 {
		t.Errorf("after issue: store jam %v beer %v bar %v", e.stock(store, jam), e.stock(store, beer), e.stock(bar, jam))
	}
	// The bar (a bartender) receives 4 bottles: one went missing between store and bar.
	limited := access{Org: torg, User: "u-bt2", View: true, Create: true, Locs: map[string]bool{vip: true}}
	if err := e.s.ReceiveTransfer(torg, "u-bt2", limited, id, nil); err == nil {
		t.Error("a bartender assigned to another bar received the transfer")
	}
	e.must(e.s.ReceiveTransfer(torg, "u-bt1", e.bt, id, []QtyLine{{ProductID: jam, Qty: 4}}))
	var tr Transfer
	e.must(e.db.Where("id = ?", id).First(&tr).Error)
	var lines []TransferLine
	e.db.Where("transfer_id = ?", id).Order("sort").Find(&lines)
	if tr.Status != TrReceived || !tr.HasDiscrepancy || lines[0].Discrepancy != 750 || lines[1].Discrepancy != 0 || *lines[0].QtyIssued != 3750 {
		t.Errorf("transfer = %+v lines %+v", tr, lines)
	}
	if e.stock(bar, jam) != 3000 || e.stock(bar, beer) != 25 {
		t.Errorf("bar = %v / %v", e.stock(bar, jam), e.stock(bar, beer))
	}
	// Direct transfer (managers / store keepers only) moves at once.
	if _, err := e.s.CreateTransfer(torg, "u-bt1", e.bt, TransferInput{FromLocationID: store, ToLocationID: vip, Mode: "direct", Lines: []LineInput{{jam, 1, "bottle"}}}); err == nil {
		t.Error("bartender made a direct transfer")
	}
	_, err = e.s.CreateTransfer(torg, "u-sk", e.sk, TransferInput{FromLocationID: store, ToLocationID: vip, Mode: "direct", Lines: []LineInput{{jam, 2, "bottle"}}})
	e.must(err)
	if e.stock(vip, jam) != 1500 || e.stock(store, jam) != 5*750 {
		t.Errorf("direct: vip %v store %v", e.stock(vip, jam), e.stock(store, jam))
	}
	// Cancelling an issued transfer returns the stock to the store.
	id2, err := e.s.CreateTransfer(torg, "u-bt1", e.bt, TransferInput{FromLocationID: store, ToLocationID: bar, Lines: []LineInput{{jam, 1, ""}}})
	e.must(err)
	e.must(e.s.IssueTransfer(torg, "u-sk", e.sk, id2, nil))
	e.must(e.s.CancelTransfer(torg, "u-sk", e.sk, id2))
	if e.stock(store, jam) != 5*750 {
		t.Errorf("cancel did not return stock: %v", e.stock(store, jam))
	}
	// Top up to par: par 6 at the bar, 4 on hand → 2.
	e.must(e.s.savePars(e.db, torg, "", []ParInput{{LocationID: bar, ProductID: jam, Par: 6, ReorderPoint: 2}, {LocationID: bar, ProductID: beer, Par: 48}}))
	sug, err := e.s.SuggestTopUp(e.db, torg, bar)
	e.must(err)
	got := map[string]float64{}
	for _, l := range sug["lines"].([]gin.H) {
		got[l["product_id"].(string)] = l["qty"].(float64)
	}
	if got[jam] != 2 || got[beer] != 23 {
		t.Errorf("top up = %v", got)
	}
}

func TestDBSaleHookBusinessDateVoid(t *testing.T) {
	e := newTestEnv(t)
	store, bar, _, items := e.setup()
	jam, beer, rum, soda, mint, lime := items["spirit_jameson_750"], items["beer_tusker_lager_500"], items["spirit_bacardi_750"], items["mixer_soda_300"], items["garnish_mint"], items["mixer_lime_cordial_1l"]
	_, err := e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{Lines: []dl{{jam, 2, "unit", ptr(2400.0), 0}, {beer, 1, "pack", ptr(180.0), 0},
		{rum, 1, "unit", ptr(1500.0), 0}, {soda, 1, "pack", ptr(48.0), 0}, {mint, 1, "pack", ptr(100.0), 0}, {lime, 1, "unit", ptr(600.0), 0}}})
	e.must(err)
	_, err = e.s.CreateTransfer(torg, "u-sk", e.sk, TransferInput{FromLocationID: store, ToLocationID: bar, Mode: "direct",
		Lines: []LineInput{{jam, 2, ""}, {beer, 24, ""}, {rum, 1, ""}, {soda, 24, ""}, {mint, 20, "unit"}, {lime, 1, ""}}})
	e.must(err)
	// A mojito recipe: 50 ml rum, 20 ml lime, 1 mint sprig, 1 soda.
	var mojito string
	e.must(e.db.Transaction(func(tx *gorm.DB) error {
		var err error
		mojito, err = e.s.saveRecipe(tx, torg, "", RecipeInput{Name: ptr("Mojito"), Price: ptr(800.0),
			Lines: []LineInput{{rum, 2, "tot"}, {lime, 20, "ml"}, {mint, 1, "unit"}, {soda, 1, "unit"}}}, true)
		return err
	}))
	tot := e.recipeOf("tot", jam)
	// 23:30 Friday: 3 Jameson tots + 2 beers + a mojito.
	e.at("2026-10-09 23:30")
	_, res, err := e.sale("", map[string]int{tot: 3, beer: 2, mojito: 1}, 300)
	e.must(err)
	if len(res.Oversold) != 0 || res.LocationID != bar {
		t.Errorf("sale = %+v", res)
	}
	// 01:30 Saturday: belongs to Friday night.
	e.at("2026-10-10 01:30")
	late, _, err := e.sale(bar, map[string]int{tot: 2}, 300)
	e.must(err)
	var mvs []Movement
	e.db.Where("ref_id = ? AND kind = ?", late.ID, KindSale).Find(&mvs)
	if len(mvs) != 1 || mvs[0].BusinessDate != "2026-10-09" || mvs[0].Qty != -50 {
		t.Errorf("late sale movements = %+v", mvs)
	}
	var sl SaleLine
	e.must(e.db.Where("pos_transaction_id = ?", late.ID).First(&sl).Error)
	if sl.BusinessDate != "2026-10-09" || sl.Category != "spirits" || math.Abs(sl.Net-(600-math.Round(600.0*16/116*100)/100)) > 1e-6 || math.Abs(sl.Cost-160) > 1e-6 {
		t.Errorf("sale line = %+v (%q %q %v %v)", sl, sl.BusinessDate, sl.Category, sl.Net, sl.Cost)
	}
	// 06:30 Saturday is a new business day.
	e.at("2026-10-10 06:30")
	day, _, err := e.sale(bar, map[string]int{beer: 1}, 250)
	e.must(err)
	e.db.Where("ref_id = ?", day.ID).Find(&mvs)
	if mvs[0].BusinessDate != "2026-10-10" {
		t.Errorf("morning sale business date = %s", mvs[0].BusinessDate)
	}
	// Stock: Jameson 1500 − 125, beer 24 − 3, rum 750 − 50, mint 20 − 1, lime 1000 − 20, soda 24 − 1.
	for pid, want := range map[string]float64{jam: 1375, beer: 21, rum: 700, mint: 19, lime: 980, soda: 23} {
		if got := e.stock(bar, pid); got != want {
			t.Errorf("bar stock of %s = %v want %v", e.product(pid).Name, got, want)
		}
	}
	if p := e.product(jam); p.CurrentStock != 1 {
		t.Errorf("jameson current_stock = %d", p.CurrentStock)
	}
	// Selling more than recorded never blocks by default but is flagged.
	e.tick(time.Minute)
	_, res, err = e.sale(bar, map[string]int{e.recipeOf("bottle", jam): 2}, 6000)
	e.must(err)
	if len(res.Oversold) != 1 || e.stock(bar, jam) != 1375-1500 {
		t.Errorf("oversold = %+v stock %v", res, e.stock(bar, jam))
	}
	// With block_when_out the sale fails with 409.
	_, err = e.s.applySettings(e.db, torg, "u-mgr", settingsInput{BlockWhenOut: ptr(true)})
	e.must(err)
	_, _, err = e.sale(bar, map[string]int{tot: 1}, 300)
	var be *Error
	if err == nil || !strings.Contains(err.Error(), "Not enough") || !asErr(err, &be) || be.Status != http.StatusConflict {
		t.Errorf("blocked sale err = %v", err)
	}
	_, err = e.s.applySettings(e.db, torg, "u-mgr", settingsInput{BlockWhenOut: ptr(false)})
	e.must(err)
	// Void the late sale: reversed on Friday's business date, once.
	e.tick(time.Minute)
	for i := 0; i < 2; i++ {
		e.must(e.db.Transaction(func(tx *gorm.DB) error {
			res, err := e.s.OnVoid(tx, torg, "u-mgr", late, "wrong drink")
			if !res.Handled[tot] {
				t.Errorf("void did not handle the tot: %+v", res)
			}
			return err
		}))
	}
	var voids []Movement
	e.db.Where("ref_id = ? AND kind = ?", late.ID, KindSaleVoid).Find(&voids)
	if len(voids) != 1 || voids[0].Qty != 50 || voids[0].BusinessDate != "2026-10-09" {
		t.Errorf("voids = %+v", voids)
	}
	sales := e.s.Sales(e.db, torg, "2026-10-09", "2026-10-09", nil)
	// Friday: tots 3×300, beers 2×300, mojito 300 (late sale voided) — all VAT inclusive.
	gross := 3*300.0 + 2*300 + 300
	if sales.Gross != gross || sales.Sales != 1 {
		t.Errorf("friday sales = %+v", sales)
	}
	// Products POS does not manage are untouched.
	if m, _ := e.s.ManagedProducts(e.db, torg, []string{"some-other-product"}); len(m) != 0 {
		t.Error("unexpected managed product")
	}
}

func asErr(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestDBWastageApprovals(t *testing.T) {
	e := newTestEnv(t)
	store, bar, _, items := e.setup()
	jam := items["spirit_jameson_750"]
	_, err := e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{Lines: []dl{{jam, 2, "unit", ptr(2400.0), 0}}})
	e.must(err)
	_, err = e.s.CreateTransfer(torg, "u-sk", e.sk, TransferInput{FromLocationID: store, ToLocationID: bar, Mode: "direct", Lines: []LineInput{{jam, 2, ""}}})
	e.must(err)
	_, err = e.s.applySettings(e.db, torg, "u-mgr", settingsInput{CompAllowancePerShift: ptr(2.0)})
	e.must(err)
	e.at("2026-10-09 22:00")
	// Spillage moves stock at once.
	id1, err := e.s.RecordWastage(torg, "u-bt1", e.bt, WastageInput{LocationID: bar, ProductID: jam, Qty: 30, Unit: "ml", Reason: "spillage"})
	e.must(err)
	// Two comps within the allowance, the third waits.
	_, err = e.s.RecordWastage(torg, "u-bt1", e.bt, WastageInput{LocationID: bar, ProductID: jam, Qty: 2, Unit: "tot", Reason: "comp"})
	e.must(err)
	id3, err := e.s.RecordWastage(torg, "u-bt1", e.bt, WastageInput{LocationID: bar, ProductID: jam, Qty: 1, Unit: "tot", Reason: "comp", Note: "DJ"})
	e.must(err)
	// Another bartender has their own allowance.
	_, err = e.s.RecordWastage(torg, "u-bt2", access{Org: torg, User: "u-bt2", View: true, Create: true}, WastageInput{LocationID: bar, ProductID: jam, Qty: 1, Unit: "tot", Reason: "staff_drink"})
	e.must(err)
	var ws []Wastage
	e.db.Where("organization_id = ?", torg).Order("created_at").Find(&ws)
	status := map[string]string{}
	for _, w := range ws {
		status[w.ID] = w.Status
	}
	if status[id1] != WApproved || status[id3] != WPending {
		t.Fatalf("statuses = %v", status)
	}
	if got := e.stock(bar, jam); got != 1500-30-50-25 {
		t.Errorf("stock after wastage = %v", got)
	}
	if err := e.s.DecideWastage(torg, "u-bt1", e.bt, id3, true, ""); err == nil {
		t.Error("bartender approved a comp")
	}
	e.must(e.s.DecideWastage(torg, "u-mgr", e.mgr, id3, true, "ok"))
	if got := e.stock(bar, jam); got != 1500-30-50-25-25 {
		t.Errorf("stock after approval = %v", got)
	}
	if err := e.s.DecideWastage(torg, "u-mgr", e.mgr, id3, false, ""); err == nil {
		t.Error("decided twice")
	}
	// A rejected entry never moves stock.
	id5, err := e.s.RecordWastage(torg, "u-bt1", e.bt, WastageInput{LocationID: bar, ProductID: jam, Qty: 1, Unit: "tot", Reason: "comp"})
	e.must(err)
	e.must(e.s.DecideWastage(torg, "u-mgr", e.mgr, id5, false, "no"))
	if got := e.stock(bar, jam); got != 1370 {
		t.Errorf("stock after rejection = %v", got)
	}
	// Next business day: the allowance is fresh.
	e.at("2026-10-10 21:00")
	id6, err := e.s.RecordWastage(torg, "u-bt1", e.bt, WastageInput{LocationID: bar, ProductID: jam, Qty: 1, Unit: "tot", Reason: "comp"})
	e.must(err)
	var w6 Wastage
	e.db.Where("id = ?", id6).First(&w6)
	if w6.Status != WApproved || w6.BusinessDate != "2026-10-10" {
		t.Errorf("next night comp = %+v", w6)
	}
	// Returned to supplier is a return, not wastage.
	_, err = e.s.RecordWastage(torg, "u-sk", e.sk, WastageInput{LocationID: store, ProductID: jam, Qty: 0, Unit: "bottle", Reason: "returned_to_supplier"})
	if err == nil {
		t.Error("zero quantity accepted")
	}
}

func TestDBCountVarianceRecountBlind(t *testing.T) {
	e := newTestEnv(t)
	store, bar, _, items := e.setup()
	jam, beer := items["spirit_jameson_750"], items["beer_tusker_lager_500"]
	e.must(e.s.updateItem(e.db, torg, jam, ItemInput{FullWeightG: ptr(1250.0), EmptyWeightG: ptr(500.0)}))
	_, err := e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{Lines: []dl{{jam, 6, "unit", ptr(2400.0), 0}, {beer, 2, "pack", ptr(180.0), 0}}})
	e.must(err)
	e.tick(time.Minute)
	_, err = e.s.CreateTransfer(torg, "u-sk", e.sk, TransferInput{FromLocationID: store, ToLocationID: bar, Mode: "direct", Lines: []LineInput{{jam, 4, ""}, {beer, 48, ""}}})
	e.must(err)
	tot := e.recipeOf("tot", jam)
	e.at("2026-10-09 22:00")
	_, _, err = e.sale(bar, map[string]int{tot: 20, beer: 30}, 300) // 500 ml + 30 beers
	e.must(err)
	e.tick(time.Minute)
	_, err = e.s.RecordWastage(torg, "u-bt1", e.bt, WastageInput{LocationID: bar, ProductID: jam, Qty: 25, Unit: "ml", Reason: "spillage"})
	e.must(err)
	// Closing count at 03:00 Saturday (Friday night), blind, two bartenders on shift.
	e.at("2026-10-10 03:00")
	cid, err := e.s.CreateCount(torg, "u-bt1", e.bt, CountInput{LocationID: bar, Type: "closing", Blind: true, StaffIDs: []string{"u-bt1", "u-bt2"}})
	e.must(err)
	if _, err := e.s.CreateCount(torg, "u-bt1", e.bt, CountInput{LocationID: bar, Type: "spot", ProductIDs: []string{jam}}); err == nil {
		t.Error("second open count at the same location")
	}
	var c Count
	e.must(e.db.Where("id = ?", cid).First(&c).Error)
	if c.BusinessDate != "2026-10-09" {
		t.Errorf("count business date = %s", c.BusinessDate)
	}
	// Jameson: 3 bottles + 0.4 by tenths = 2550 ml; expected 3000 − 500 − 25 = 2475 → +75 ml.
	// Beer: 17 cans; expected 18 → −1.
	e.tick(time.Minute)
	e.must(e.s.SaveCountLines(torg, "u-bt1", e.bt, cid, []CountLineInput{{ProductID: jam, FullUnits: ptr(3.0), PartialMode: "tenths", PartialValue: 0.4}}))
	if err := e.s.SubmitCount(torg, "u-bt1", e.bt, cid); err == nil || !strings.Contains(err.Error(), "not counted") {
		t.Errorf("submit with uncounted lines: %v", err)
	}
	e.must(e.s.SaveCountLines(torg, "u-bt1", e.bt, cid, []CountLineInput{{ProductID: beer, FullUnits: ptr(17.0)}}))
	// Blind: the bartender's view shows no expected quantity.
	cnt, lines, _ := e.s.loadCount(e.db, torg, cid, false)
	v, err := e.s.countView(e.db, torg, e.bt, cnt, lines, true)
	e.must(err)
	for _, l := range v["lines"].([]gin.H) {
		if _, ok := l["expected"]; ok {
			t.Error("bartender sees expected")
		}
	}
	e.must(e.s.SubmitCount(torg, "u-bt1", e.bt, cid))
	if err := e.s.ApproveCount(torg, "u-bt1", e.bt, cid); err == nil {
		t.Error("bartender approved a count")
	}
	// The manager sees the variance before approving and asks for a recount of the beer.
	cnt, lines, _ = e.s.loadCount(e.db, torg, cid, false)
	v, _ = e.s.countView(e.db, torg, e.mgr, cnt, lines, true)
	for _, l := range v["lines"].([]gin.H) {
		if l["product_id"] == beer && (l["expected"] != 18.0 || l["variance"] != -1.0) {
			t.Errorf("beer preview = %v / %v", l["expected"], l["variance"])
		}
	}
	e.must(e.s.RecountCount(torg, "u-mgr", e.mgr, cid, []string{beer}))
	e.must(e.db.Where("id = ?", cid).First(&c).Error)
	if c.Status != CDraft || c.RecountRound != 1 {
		t.Errorf("recount status = %s round %d", c.Status, c.RecountRound)
	}
	e.tick(time.Minute)
	e.must(e.s.SaveCountLines(torg, "u-bt2", e.bt, cid, []CountLineInput{{ProductID: beer, FullUnits: ptr(16.0)}}))
	e.must(e.s.SubmitCount(torg, "u-bt1", e.bt, cid))
	e.must(e.s.ApproveCount(torg, "u-mgr", e.mgr, cid))
	if got := e.stock(bar, jam); got != 2550 {
		t.Errorf("jameson after approval = %v", got)
	}
	if got := e.stock(bar, beer); got != 16 {
		t.Errorf("beer after approval = %v", got)
	}
	var cl []CountLine
	e.db.Where("count_id = ?", cid).Find(&cl)
	for _, l := range cl {
		switch l.ProductID {
		case jam:
			if l.Opening != 0 || l.TransfersIn != 3000 || l.Sold != 500 || l.Wasted != 25 || l.Expected != 2475 || l.Variance != 75 {
				t.Errorf("jameson line = %+v", l)
			}
		case beer:
			if l.TransfersIn != 48 || l.Sold != 30 || l.Expected != 18 || l.Variance != -2 {
				t.Errorf("beer line = %+v", l)
			}
		}
	}
	rep, err := e.s.Variance(e.db, torg, VarianceQuery{CountID: cid})
	e.must(err)
	tot2 := rep["totals"].(gin.H)
	wantValue := 75*3.2 + (-2)*180
	if tot2["variance_value"] != RoundMoney(wantValue, "KES") || tot2["loss_value"] != 360.0 {
		t.Errorf("variance totals = %v", tot2)
	}
	for _, it := range rep["items"].([]gin.H) {
		if it["product_id"] == beer {
			pct := it["variance_pct"].(*float64)
			if *pct != math.Round(-2.0/30*10000)/100 || it["flagged"] != true {
				t.Errorf("beer pct = %v flagged %v", *pct, it["flagged"])
			}
		}
		if it["product_id"] == jam && it["variance_tots"] != 3.0 {
			t.Errorf("jameson tots = %v", it["variance_tots"])
		}
	}
	staff := rep["staff"].([]gin.H)
	if len(staff) != 2 || staff[0]["loss_value"] != 180.0 {
		t.Errorf("staff shares = %v", staff)
	}
	sales := rep["sales"].(SalesSummary)
	if sales.Revenue == 0 || sales.Cost == 0 {
		t.Errorf("pour cost data missing: %+v", sales)
	}
	// Next night: the closing count is the opening; weight counting.
	e.at("2026-10-10 22:00")
	_, _, err = e.sale(bar, map[string]int{tot: 10}, 300)
	e.must(err)
	e.at("2026-10-11 03:00")
	cid2, err := e.s.CreateCount(torg, "u-bt1", e.bt, CountInput{LocationID: bar, Type: "spot", ProductIDs: []string{jam}})
	e.must(err)
	e.tick(time.Minute)
	if err := e.s.SaveCountLines(torg, "u-bt1", e.bt, cid2, []CountLineInput{{ProductID: jam, FullUnits: ptr(2.0), PartialMode: "weight", PartialValue: 1300}}); err == nil {
		t.Error("weight above full accepted")
	}
	// 2 bottles + (800−500)/(1250−500)×750 = 300 ml → 1800; expected 2550 − 250 = 2300 → −500.
	e.must(e.s.SaveCountLines(torg, "u-bt1", e.bt, cid2, []CountLineInput{{ProductID: jam, FullUnits: ptr(2.0), PartialMode: "weight", PartialValue: 800}}))
	e.must(e.s.SubmitCount(torg, "u-bt1", e.bt, cid2))
	e.must(e.s.ApproveCount(torg, "u-mgr", e.mgr, cid2))
	e.db.Where("count_id = ?", cid2).Find(&cl)
	if len(cl) != 1 || cl[0].Opening != 2550 || cl[0].Sold != 250 || cl[0].Expected != 2300 || cl[0].Variance != -500 || cl[0].PrevLineID == "" {
		t.Errorf("second count line = %+v", cl)
	}
	// The period report sums both nights.
	rep, err = e.s.Variance(e.db, torg, VarianceQuery{LocationID: bar, From: "2026-10-09", To: "2026-10-10"})
	e.must(err)
	for _, it := range rep["items"].([]gin.H) {
		if it["product_id"] == jam && (it["variance"] != -425.0 || it["opening"] != 0.0 || it["sold"] != 750.0) {
			t.Errorf("period jameson = %v", it)
		}
	}
	if len(rep["counts"].([]gin.H)) != 2 {
		t.Errorf("period counts = %d", len(rep["counts"].([]gin.H)))
	}
}

func TestDBReorder(t *testing.T) {
	e := newTestEnv(t)
	store, _, _, items := e.setup()
	jam, beer, rum := items["spirit_jameson_750"], items["beer_tusker_lager_500"], items["spirit_bacardi_750"]
	kwal, ebl := e.supplier("KWAL"), e.supplier("EABL")
	e.must(e.s.updateItem(e.db, torg, jam, ItemInput{PreferredSupplierID: &kwal}))
	e.must(e.s.updateItem(e.db, torg, rum, ItemInput{PreferredSupplierID: &kwal}))
	e.must(e.s.updateItem(e.db, torg, beer, ItemInput{PreferredSupplierID: &ebl}))
	_, err := e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{SupplierID: kwal, Lines: []dl{{jam, 5, "unit", ptr(2400.0), 0}, {beer, 20, "unit", ptr(180.0), 0}}})
	e.must(err)
	e.must(e.s.savePars(e.db, torg, "", []ParInput{{LocationID: store, ProductID: jam, Par: 24, ReorderPoint: 6}, {LocationID: store, ProductID: beer, Par: 100},
		{LocationID: store, ProductID: rum, Par: 12, ReorderPoint: 3}}))
	r, err := e.s.Reorder(e.db, torg, "")
	e.must(err)
	got := map[string][2]float64{}
	for _, g := range r["groups"].([]gin.H) {
		for _, l := range g["lines"].([]gin.H) {
			got[l["product_id"].(string)] = [2]float64{l["suggest_units"].(float64), float64(l["suggest_packs"].(int))}
		}
	}
	// Jameson: 24 − 5 = 19 → 2 cases of 12 = 24. Beer: 100 − 20 = 80 → 4 crates of 25 = 100. Rum: 12 − 0 → 1 case.
	if got[jam] != [2]float64{24, 2} || got[beer] != [2]float64{100, 4} || got[rum] != [2]float64{12, 1} {
		t.Errorf("reorder = %v", got)
	}
	if len(r["groups"].([]gin.H)) != 2 {
		t.Errorf("groups = %d", len(r["groups"].([]gin.H)))
	}
	// Ordering them puts them on order: no new suggestion.
	in := CreatePOInput{}
	for _, g := range r["groups"].([]gin.H) {
		grp := struct {
			SupplierID string `json:"supplier_id"`
			Lines      []struct {
				ProductID string   `json:"product_id"`
				QtyUnits  float64  `json:"qty_units"`
				UnitCost  *float64 `json:"unit_cost"`
			} `json:"lines"`
		}{SupplierID: g["supplier_id"].(string)}
		for _, l := range g["lines"].([]gin.H) {
			grp.Lines = append(grp.Lines, struct {
				ProductID string   `json:"product_id"`
				QtyUnits  float64  `json:"qty_units"`
				UnitCost  *float64 `json:"unit_cost"`
			}{l["product_id"].(string), l["suggest_units"].(float64), nil})
		}
		in.Groups = append(in.Groups, grp)
	}
	ids, err := e.s.CreatePOs(torg, "u-mgr", in)
	e.must(err)
	if len(ids) != 2 {
		t.Fatalf("POs = %v", ids)
	}
	var pos []models.PurchaseOrder
	e.db.Where("organization_id = ?", torg).Order("order_number").Find(&pos)
	if pos[0].OrderNumber != "PO-00001" || pos[1].OrderNumber != "PO-00002" || pos[0].Status != "draft" {
		t.Errorf("po numbers = %s %s", pos[0].OrderNumber, pos[1].OrderNumber)
	}
	r, _ = e.s.Reorder(e.db, torg, "")
	if len(r["groups"].([]gin.H)) != 0 {
		t.Errorf("still suggested after ordering: %v", r["groups"])
	}
	if _, err := e.s.CreatePOs(torg, "u-mgr", CreatePOInput{Groups: in.Groups[:1]}); err != nil {
		t.Errorf("second order: %v", err)
	}
	bad := in
	bad.Groups = in.Groups[:1]
	bad.Groups[0].Lines[0].QtyUnits = 1.5
	if _, err := e.s.CreatePOs(torg, "u-mgr", bad); err == nil {
		t.Error("fractional PO quantity accepted")
	}
}

// TestDBHandlersMoneyAndLimits checks the HTTP layer: money fields are
// stripped for users without approve, and location assignments limit views.
func TestDBHandlersMoneyAndLimits(t *testing.T) {
	e := newTestEnv(t)
	store, bar, vip, items := e.setup()
	jam := items["spirit_jameson_750"]
	_, err := e.s.ReceiveDelivery(torg, "u-sk", e.sk, DeliveryInput{Lines: []dl{{jam, 3, "unit", ptr(2400.0), 0}}})
	e.must(err)
	_, err = e.s.CreateTransfer(torg, "u-sk", e.sk, TransferInput{FromLocationID: store, ToLocationID: bar, Mode: "direct", Lines: []LineInput{{jam, 1, ""}}})
	e.must(err)
	e.must(e.db.Create(&LocationStaff{OrganizationID: torg, LocationID: vip, UserID: "u-bt2"}).Error)
	gin.SetMode(gin.TestMode)
	e.s.Can = func(org, user, role, module, act string) bool { return legacyCan(role, act) }
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("org_id", torg)
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("user_role", c.GetHeader("X-Role"))
	})
	(&Handler{S: e.s}).RegisterRoutes(r.Group("/api/v1"))
	call := func(user, role, method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-User", user)
		req.Header.Set("X-Role", role)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code, out := call("u-mgr", "manager", "GET", "/api/v1/bar/items", "")
	if code != 200 || !strings.Contains(fmt.Sprint(out), "avg_cost") {
		t.Fatalf("manager items: %d %v", code, out)
	}
	code, out = call("u-bt1", "staff", "GET", "/api/v1/bar/items/"+jam, "")
	raw, _ := json.Marshal(out)
	for _, k := range []string{`"avg_cost"`, `"value"`, `"last_cost"`, `"price_history"`, `"unit_cost"`} {
		if strings.Contains(string(raw), k) {
			t.Errorf("bartender sees %s: %s", k, raw)
		}
	}
	if code != 200 || !strings.Contains(string(raw), `"on_hand_text"`) {
		t.Errorf("bartender item: %d %s", code, raw)
	}
	code, _ = call("u-bt1", "staff", "PUT", "/api/v1/bar/settings", `{"tot_ml":30}`)
	if code != 403 {
		t.Errorf("bartender changed settings: %d", code)
	}
	code, _ = call("u-mgr", "manager", "PUT", "/api/v1/bar/settings", `{"tot_ml":30,"day_cutoff":"05:00","pour_cost_targets":{"spirits":20}}`)
	if code != 200 || e.s.Settings(e.db, torg).TotML != 30 {
		t.Errorf("manager settings: %d", code)
	}
	code, _ = call("u-mgr", "manager", "PUT", "/api/v1/bar/settings", `{"day_cutoff":"18:00"}`)
	if code != 400 {
		t.Errorf("late cutoff accepted: %d", code)
	}
	// u-bt2 is assigned to the VIP bar only.
	code, out = call("u-bt2", "staff", "GET", "/api/v1/bar/locations", "")
	if l := out["data"].([]any); code != 200 || len(l) != 1 {
		t.Errorf("assigned locations = %v", out["data"])
	}
	code, _ = call("u-bt2", "staff", "GET", "/api/v1/bar/stock?location_id="+bar, "")
	if code != 403 {
		t.Errorf("assigned bartender read another bar: %d", code)
	}
	code, _ = call("u-bt2", "staff", "POST", "/api/v1/bar/wastage", fmt.Sprintf(`{"location_id":%q,"product_id":%q,"qty":1,"unit":"tot","reason":"spillage"}`, bar, jam))
	if code != 403 {
		t.Errorf("assigned bartender recorded wastage elsewhere: %d", code)
	}
	code, out = call("u-bt1", "staff", "GET", "/api/v1/bar/reports/export/stock.csv", "")
	_ = out
	if code != 200 {
		t.Errorf("csv export = %d", code)
	}
	req := httptest.NewRequest("GET", "/api/v1/bar/reports/export/valuation.csv", nil)
	req.Header.Set("X-User", "u-mgr")
	req.Header.Set("X-Role", "manager")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "avg_cost_per_unit") || !strings.Contains(w.Header().Get("Content-Type"), "text/csv") {
		t.Errorf("manager valuation csv: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("GET", "/api/v1/bar/reports/export/valuation.csv", nil)
	req.Header.Set("X-User", "u-bt1")
	req.Header.Set("X-Role", "staff")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "avg_cost") || strings.Contains(w.Body.String(), "value") {
		t.Errorf("bartender csv has money: %s", w.Body.String())
	}
	code, out = call("u-bt1", "staff", "GET", "/api/v1/bar/pos-availability?location_id="+bar, "")
	if code != 200 {
		t.Errorf("availability = %d", code)
	}
	for _, x := range out["data"].(map[string]any)["items"].([]any) {
		m := x.(map[string]any)
		if m["product_id"] == e.recipeOf("tot", jam) && m["available"] != 30.0 {
			t.Errorf("tots available at bar = %v", m["available"])
		}
	}
	// Other organisations see nothing.
	r2 := gin.New()
	r2.Use(func(c *gin.Context) { c.Set("org_id", "org-other"); c.Set("user_id", "x"); c.Set("user_role", "admin") })
	(&Handler{S: e.s}).RegisterRoutes(r2.Group("/api/v1"))
	req = httptest.NewRequest("GET", "/api/v1/bar/items/"+jam, nil)
	w = httptest.NewRecorder()
	r2.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Errorf("other org read an item: %d", w.Code)
	}
}

func TestDBItemEditsAndRecipes(t *testing.T) {
	e := newTestEnv(t)
	store, _, _, items := e.setup()
	// An existing Inventory product with 7 in stock becomes a stock item: its stock is the opening stock of the main store.
	vod := models.Product{ID: uuid.NewString(), OrganizationID: torg, Name: "Vodka X 750 ml", SKU: "VX-" + uuid.NewString()[:6], CostPrice: 1500, CurrentStock: 7, IsActive: true}
	e.must(e.db.Omit("Organization", "Category", "Brand", "InventoryItems", "POSItems", "PurchaseOrderItems").Create(&vod).Error)
	e.must(e.db.Transaction(func(tx *gorm.DB) error {
		_, err := e.s.createItem(tx, torg, "u-mgr", ItemInput{ProductID: vod.ID, Category: "spirits", UnitSize: ptr(750.0)})
		return err
	}))
	if e.stock(store, vod.ID) != 5250 || e.product(vod.ID).CurrentStock != 7 {
		t.Errorf("opening stock = %v, current %d", e.stock(store, vod.ID), e.product(vod.ID).CurrentStock)
	}
	if it, _ := e.s.item(e.db, torg, vod.ID); !near(it.AvgCost, 2) {
		t.Errorf("opening avg = %v", it.AvgCost)
	}
	// A price-only PUT keeps the recipe lines; lines: [] clears them.
	jam := items["spirit_jameson_750"]
	tot := e.recipeOf("tot", jam)
	e.must(e.db.Transaction(func(tx *gorm.DB) error {
		_, err := e.s.saveRecipe(tx, torg, tot, RecipeInput{Price: ptr(350.0)}, false)
		return err
	}))
	var n int64
	e.db.Model(&RecipeLine{}).Where("pos_product_id = ?", tot).Count(&n)
	if n != 1 || e.product(tot).SellingPrice != 350 {
		t.Errorf("price-only PUT: %d lines, price %v", n, e.product(tot).SellingPrice)
	}
	// Entry fee: a menu item without stock lines — managed, never blocked, nothing deducted.
	var fee string
	e.must(e.db.Transaction(func(tx *gorm.DB) error {
		var err error
		fee, err = e.s.saveRecipe(tx, torg, "", RecipeInput{Name: ptr("Entry"), Price: ptr(1000.0), Lines: []LineInput{}}, true)
		return err
	}))
	if m, _ := e.s.ManagedProducts(e.db, torg, []string{fee}); !m[fee] {
		t.Error("entry fee is not managed")
	}
	_, _, err := e.sale("", map[string]int{fee: 3}, 1000)
	e.must(err)
	// A new bottle size moves the "one bottle" recipes with it.
	e.must(e.s.updateItem(e.db, torg, jam, ItemInput{UnitSize: ptr(1000.0)}))
	var bl RecipeLine
	e.must(e.db.Where("pos_product_id = ?", e.recipeOf("bottle", jam)).First(&bl).Error)
	var tl RecipeLine
	e.must(e.db.Where("pos_product_id = ?", tot).First(&tl).Error)
	if bl.Qty != 1000 || tl.Qty != 25 {
		t.Errorf("after resize: bottle line %v, tot line %v", bl.Qty, tl.Qty)
	}
	// Validation.
	for _, in := range []ItemInput{{Name: "x", Category: "spirits"}, {Name: "x", Category: "nope"}, {Name: "x", BaseUnit: "litre"},
		{Name: "x", Category: "spirits", UnitSize: ptr(750.0), FullWeightG: ptr(500.0), EmptyWeightG: ptr(600.0)},
		{Name: "x", Category: "spirits", UnitSize: ptr(750.0), FullWeightG: ptr(500.0)}, {Name: "x", Category: "spirits", UnitSize: ptr(750.0), TotML: ptr(800.0)}} {
		if err := e.db.Transaction(func(tx *gorm.DB) error { _, err := e.s.createItem(tx, torg, "u-mgr", in); return err }); err == nil {
			t.Errorf("invalid item accepted: %+v", in)
		}
	}
}
