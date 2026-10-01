package zoho

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

func TestParseMoney(t *testing.T) {
	cases := []struct {
		in   string
		v    float64
		cur  string
		okay bool
	}{
		{"KES 1,500.00", 1500, "KES", true}, // the old parser kept the "E" → 0
		{"EUR 10", 10, "EUR", true},
		{"SEK 99.50", 99.5, "SEK", true},
		{"AUD 250.00", 250, "AUD", true},
		{"1,500.00", 1500, "", true},
		{"1,500", 1500, "", true},
		{"12,5", 12.5, "", true},
		{"1.234,50 EUR", 1234.5, "EUR", true},
		{"1.234.567", 1234567, "", true},
		{"$12", 12, "", true},
		{"(5.00)", -5, "", true},
		{"-7.25", -7.25, "", true},
		{"1e3", 1000, "", true},
		{"10.00%", 10, "", true},
		{"", 0, "", false},
		{"abc", 0, "", false},
	}
	for _, c := range cases {
		v, cur, ok := parseMoney(c.in)
		if math.Abs(v-c.v) > 1e-9 || cur != c.cur || ok != c.okay {
			t.Errorf("%q → %v %q %v, want %v %q %v", c.in, v, cur, ok, c.v, c.cur, c.okay)
		}
	}
}

func TestItemRateFields(t *testing.T) {
	norm := func(js string) Item {
		var z zItem
		if err := json.Unmarshal([]byte(js), &z); err != nil {
			t.Fatal(err)
		}
		return z.norm()
	}
	// rate is the selling price; pricebook_rate (a price list) and purchase_rate (cost) never are.
	it := norm(`{"item_id":"1","name":"A","rate":99,"sales_rate":120,"pricebook_rate":50,"purchase_rate":40}`)
	if it.Rate != 99 || it.PurchaseRate != 40 {
		t.Errorf("rate precedence: %+v", it)
	}
	// rate missing / zero → sales_rate
	if it := norm(`{"item_id":"2","name":"B","rate":0,"sales_rate":"120.50"}`); it.Rate != 120.5 {
		t.Errorf("sales_rate fallback: %+v", it)
	}
	if it := norm(`{"item_id":"3","name":"C","rate":"KES 1,500.00","is_inclusive_tax":true}`); it.Rate != 1500 || it.Currency != "KES" || it.TaxInclusive == nil || !*it.TaxInclusive {
		t.Errorf("string rate / inclusive: %+v", it)
	}
	if it := norm(`{"item_id":"4","name":"D","rate":10}`); it.TaxInclusive != nil || it.Currency != "" {
		t.Errorf("no flags: %+v", it)
	}
}

func TestItemCSVColumns(t *testing.T) {
	csv := "Item ID,Item Name,Rate,Purchase Rate,Is Inclusive Tax,Status\n" +
		"1,Alpha,\"KES 1,500.00\",900,true,Active\n" +
		"2,Beta,\"1,250.50\",,,Active\n" +
		"3,Gamma,\"EUR 10\",,false,Inactive\n"
	headers, _, err := ReadTable("Item.csv", []byte(csv))
	if err != nil {
		t.Fatal(err)
	}
	if k := DetectKind("Item.csv", headers); k != FileItems {
		t.Fatalf("kind %s", k)
	}
	m := AutoMap(FileItems, headers)
	if m["rate"] != "Rate" || m["purchase_rate"] != "Purchase Rate" || m["tax_inclusive"] != "Is Inclusive Tax" {
		t.Fatalf("mapping: %v", m)
	}
	ds, err := BuildDataset([]Upload{{Name: "Item.csv", Kind: FileItems, Data: []byte(csv), Mapping: m}}, true)
	if err != nil || len(ds.Items) != 3 {
		t.Fatalf("dataset: %v %d", err, len(ds.Items))
	}
	a, b, g := ds.Items[0], ds.Items[1], ds.Items[2]
	if a.Rate != 1500 || a.Currency != "KES" || a.PurchaseRate != 900 || a.TaxInclusive == nil || !*a.TaxInclusive {
		t.Errorf("alpha: %+v", a)
	}
	if b.Rate != 1250.5 || b.Currency != "" || b.TaxInclusive != nil {
		t.Errorf("beta: %+v", b)
	}
	if g.Rate != 10 || g.Currency != "EUR" || g.Active || g.TaxInclusive == nil || *g.TaxInclusive {
		t.Errorf("gamma: %+v", g)
	}
}

func TestItemPriceKeepOrConvert(t *testing.T) {
	yes := true
	im := &Importer{Home: "KES", SrcBase: "AUD", RefSource: "zoho"}
	aud := Item{Name: "Web", Rate: 250}
	if p, c, incl := im.ItemPrice(aud); p != 250 || c != "AUD" || incl {
		t.Errorf("keep: %v %s %v", p, c, incl)
	}
	im.Opts.ItemPrices = ItemPriceOptions{Mode: "convert", Rate: 85.123}
	if p, c, _ := im.ItemPrice(aud); p != 21280.75 || c != "KES" {
		t.Errorf("convert: %v %s", p, c)
	}
	// An item priced in another currency is kept as it is.
	if p, c, _ := im.ItemPrice(Item{Rate: 10, Currency: "USD"}); p != 10 || c != "USD" {
		t.Errorf("other currency: %v %s", p, c)
	}
	// Tax-inclusive: the item's own flag wins over the option.
	im.Opts.ItemPrices.IncludeTax = true
	no := false
	if _, _, incl := im.ItemPrice(Item{Rate: 1, TaxInclusive: &no}); incl {
		t.Error("item flag should win")
	}
	if _, _, incl := im.ItemPrice(Item{Rate: 1}); !incl {
		t.Error("option should apply")
	}
	_ = yes
	// Zero-decimal currencies round to whole units.
	im = &Importer{Home: "JPY", SrcBase: "AUD", Opts: Options{ItemPrices: ItemPriceOptions{Mode: "convert", Rate: 97.4}}}
	if p, c, _ := im.ItemPrice(Item{Rate: 12.34}); p != 1202 || c != "JPY" {
		t.Errorf("jpy: %v %s", p, c)
	}
}

// Import with "keep", then repair services imported by the old (wrong) code.
func TestImportItemsAndFixPrices(t *testing.T) {
	db := testDB(t)
	org := "zoho-price-" + uuid.NewString()
	db.Exec(`INSERT INTO billing_settings (organization_id, currency) VALUES (?, 'KES')`, org)
	e := NewEngine(db)
	now := time.Now()
	e.Now = func() time.Time { return now }

	// 1) Import via the importer (keep AUD): services carry AUD.
	im := &Importer{DB: db, Org: org, JobID: "j1", RefSource: "zoho", Home: "KES", SrcBase: "AUD", Now: e.Now, Opts: Options{ItemsToCatalog: true}}
	im.UpsertItem(Item{ExtID: "it1", Name: "Website maintenance", Rate: 250, Active: true})
	var svc models.Service
	if err := db.Where("organization_id = ? AND name = ?", org, "Website maintenance").Take(&svc).Error; err != nil || svc.Price != 250 || svc.Currency != "AUD" {
		t.Fatalf("imported: %+v %v", svc, err)
	}
	// Re-running unchanged → unchanged; with conversion chosen → updated.
	im2 := &Importer{DB: db, Org: org, JobID: "j2", RefSource: "zoho", Home: "KES", SrcBase: "AUD", Now: e.Now, Opts: Options{ItemsToCatalog: true}}
	im2.UpsertItem(Item{ExtID: "it1", Name: "Website maintenance", Rate: 250, Active: true})
	if im2.count(EntItems).Unchanged != 1 {
		t.Fatalf("re-sync: %+v", im2.count(EntItems))
	}

	// 2) Services imported by the old code: AUD numbers stored as KES.
	mk := func(ext, name string, price float64, edited, created bool) string {
		id := uuid.NewString()
		db.Create(&models.Service{ID: id, OrganizationID: org, Name: name, Category: "Zoho Books items", Price: price, IsActive: true})
		synced := now.Add(-time.Hour)
		db.Create(&ExternalRef{OrganizationID: org, Source: "zoho", Entity: entItemRef, ExternalID: ext, LocalID: id, Label: name, Hash: "old", JobID: "old", Created: created, LastSynced: synced, CreatedAt: synced})
		upd := synced.Add(-time.Minute)
		if edited {
			upd = synced.Add(10 * time.Minute)
		}
		db.Model(&models.Service{}).Where("id = ?", id).UpdateColumn("updated_at", upd)
		return id
	}
	consult := mk("460000000027002", "Consulting hour", 180, false, true)
	edited := mk("460000000027003", "Hosting", 99, true, true)
	_ = mk("460000000027004", "Linked", 5, false, false)
	csv := "Item ID,Item Name,Rate,Status\n" +
		"460000000027002,Consulting hour,\"AUD 180.00\",Active\n" +
		"460000000027003,Hosting,\"AUD 1,200.00\",Active\n" +
		"460000000027004,Linked,\"AUD 7.00\",Active\n" +
		"460000000027999,Never imported,AUD 1.00,Active\n"
	headers, _, _ := ReadTable("Item.csv", []byte(csv))
	db.Create(&Upload{ID: uuid.NewString(), JobID: "old", OrgID: org, Name: "Item.csv", Kind: FileItems, Headers: headers, Mapping: AutoMap(FileItems, headers), Data: []byte(csv), CreatedAt: now})

	ctx := context.Background()
	res, err := e.FixPrices(ctx, org, FixPricesRequest{DryRun: true, Source: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "file" || res.SourceCurrency != "AUD" || res.HomeCurrency != "KES" || res.Changes != 1 || res.Edited != 2 || res.NotImported != 1 || res.Applied != 0 {
		t.Fatalf("dry run: %+v", res)
	}
	byName := map[string]FixPriceRow{}
	for _, r := range res.Rows {
		byName[r.Name] = r
	}
	if r := byName["Consulting hour"]; r.Status != "change" || r.OldCurrency != "KES" || r.NewCurrency != "AUD" || r.NewPrice != 180 {
		t.Fatalf("row: %+v", r)
	}
	svc = models.Service{}
	db.Where("id = ?", consult).Take(&svc)
	if svc.Currency != "" {
		t.Fatal("dry run changed data")
	}

	// Apply with conversion to KES.
	conv := ItemPriceOptions{Mode: "convert", Rate: 85}
	res, err = e.FixPrices(ctx, org, FixPricesRequest{Source: "file", ItemPrices: conv})
	if err != nil || res.Applied != 1 || res.Edited != 2 {
		t.Fatalf("apply: %+v %v", res, err)
	}
	svc = models.Service{}
	db.Where("id = ?", consult).Take(&svc)
	if svc.Price != 15300 || svc.Currency != "KES" {
		t.Fatalf("after apply: %+v", svc)
	}
	svc = models.Service{}
	db.Where("id = ?", edited).Take(&svc)
	if svc.Price != 99 {
		t.Fatal("locally edited service changed")
	}
	// Idempotent.
	res, _ = e.FixPrices(ctx, org, FixPricesRequest{Source: "file", ItemPrices: conv})
	if res.Applied != 0 || res.Changes != 0 || res.Unchanged != 1 || res.Edited != 2 {
		t.Fatalf("second apply: %+v", res)
	}
	// Including edited ones.
	res, _ = e.FixPrices(ctx, org, FixPricesRequest{Source: "file", ItemPrices: conv, IncludeEdited: true})
	if res.Applied != 2 {
		t.Fatalf("include edited: %+v", res)
	}
	svc = models.Service{}
	db.Where("id = ?", edited).Take(&svc)
	if svc.Price != 102000 || svc.Currency != "KES" {
		t.Fatalf("edited after: %+v", svc)
	}
	if !strings.Contains(res.SourceLabel, "Item.csv") {
		t.Errorf("label %q", res.SourceLabel)
	}
}
