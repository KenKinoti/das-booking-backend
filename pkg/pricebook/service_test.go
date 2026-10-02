package pricebook

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
)

const org = "org-1"

// testDB opens TEST_DATABASE_URL in a fresh schema with the tables used here:
// an organisation in Kenya (home KES) with manual rates AUD 86 and USD 129.
func testDB(t *testing.T) (*Service, *gorm.DB) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	schema := "pb_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	admin.Exec("CREATE SCHEMA " + schema)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
		`CREATE TABLE organizations (id varchar(255) PRIMARY KEY, name text, country_code varchar(2), currency varchar(3), timezone text, region_source text, address_country text)`,
		`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES ('org-1', 'Test Org', 'KE', 'KES', 'Africa/Nairobi', 'user'), ('org-2', 'Other Org', 'AU', 'AUD', 'Australia/Sydney', 'user')`,
		`CREATE TABLE services (id varchar(255) PRIMARY KEY, organization_id varchar(255) NOT NULL, name varchar(255) NOT NULL, description text, category varchar(100) NOT NULL, duration bigint NOT NULL,
			price numeric(10,2) NOT NULL, currency varchar(3) NOT NULL DEFAULT '', price_includes_tax boolean NOT NULL DEFAULT false, is_active boolean DEFAULT true, requires_vehicle boolean DEFAULT false,
			created_at timestamptz, updated_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE billing_settings (organization_id varchar(255) PRIMARY KEY, default_tax_rate numeric, tax_label varchar(30), prices_include_tax boolean)`,
		`INSERT INTO billing_settings VALUES ('org-1', 16, 'VAT', false)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := fxrates.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(expenses.SynergyModels()...); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	fx := &fxrates.Service{DB: db}
	for cur, rate := range map[string]float64{"AUD": 86, "USD": 129} {
		if _, err := fx.SaveManual(org, "tester", cur, "KES", rate, "test"); err != nil {
			t.Fatal(err)
		}
	}
	return New(db, fx), db
}

func sample(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/sample-prices.csv")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSeedOnce(t *testing.T) {
	s, db := testDB(t)
	o := s.OverviewFor(org, testToday)
	if len(o.Suppliers) != 3 || o.Home != "KES" {
		t.Fatalf("seed: %d suppliers, home %s", len(o.Suppliers), o.Home)
	}
	if o.Context.KnownSites != 0 || o.Context.KnownSitesSource != "" {
		t.Fatalf("nothing known about hosted sites yet: %d %q", o.Context.KnownSites, o.Context.KnownSitesSource)
	}
	want := []struct{ name, typ, cur, sys string }{{"Synergy Wholesale", TypeWholesale, "AUD", "synergy"}, {"hosting.com", TypeWholesale, "USD", "hosting_com"}, {"Self-hosted server", TypeSelfHosted, "KES", "self_hosted"}}
	for i, w := range want {
		g := o.Suppliers[i]
		if g.Name != w.name || g.Type != w.typ || g.Currency != w.cur || g.System != w.sys || g.Items != 0 {
			t.Fatalf("supplier %d: %+v", i, g.Supplier)
		}
	}
	if len(o.Suppliers[2].Costs) != len(DefaultCostLines()) {
		t.Fatalf("the self-hosted supplier starts with the cost checklist: %d lines", len(o.Suppliers[2].Costs))
	}
	// The page context: VAT from the invoice settings, the payment fee for KES, rates with their source.
	c := o.Context
	if c.TaxRate != 16 || c.TaxLabel != "VAT" || c.SecondCurrency != "AUD" || c.CSVHeader != CSVHeader || len(c.Tiers) != 3 {
		t.Fatalf("context: %+v", c)
	}
	if len(c.Rates) < 2 || c.Rates[0].Currency != "AUD" || c.Rates[0].Rate != 86 || c.Rates[0].Source != fxrates.SourceManual {
		t.Fatalf("rates: %+v", c.Rates)
	}
	// Deleting a seeded supplier does not bring it back.
	if err := s.DeleteSupplier(org, o.Suppliers[1].ID); err != nil {
		t.Fatal(err)
	}
	if got := s.OverviewFor(org, testToday).Suppliers; len(got) != 2 {
		t.Fatalf("after delete: %d", len(got))
	}
	// Another organisation gets its own, in its own currency.
	o2 := s.OverviewFor("org-2", testToday)
	if len(o2.Suppliers) != 3 || o2.Suppliers[2].Currency != "AUD" || o2.Context.SecondCurrency != "USD" {
		t.Fatalf("org-2: %+v", o2.Suppliers)
	}
	var n int64
	db.Model(&Supplier{}).Count(&n)
	if n != 5 {
		t.Fatalf("suppliers in total: %d", n)
	}
}

func TestImportDryRunUpsertHistory(t *testing.T) {
	s, db := testDB(t)
	s.EnsureSeed(org)
	count := func() (items, sups int64) {
		db.Model(&Item{}).Where("organization_id = ?", org).Count(&items)
		db.Model(&Supplier{}).Where("organization_id = ?", org).Count(&sups)
		return
	}
	// Dry run: the full preview, nothing written.
	pre, err := s.Import(org, "u1", sample(t), true, testToday)
	if err != nil {
		t.Fatal(err)
	}
	if !pre.DryRun || pre.Total != 28 || pre.Created != 28 || pre.Updated != 0 || pre.Errors != 0 || len(pre.NewSuppliers) != 3 {
		t.Fatalf("dry run: %+v", pre)
	}
	if pre.Rows[0].Action != "create" || pre.Rows[0].Currency != "AUD" {
		t.Fatalf("dry-run row: %+v", pre.Rows[0])
	}
	if i, sp := count(); i != 0 || sp != 3 {
		t.Fatalf("a dry run must not write: %d items, %d suppliers", i, sp)
	}
	// The real import does exactly what the preview said.
	res, err := s.Import(org, "u1", sample(t), false, testToday)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != pre.Created || res.Errors != 0 || len(res.NewSuppliers) != 3 {
		t.Fatalf("import: %+v", res)
	}
	if i, sp := count(); i != 28 || sp != 6 {
		t.Fatalf("after import: %d items, %d suppliers", i, sp)
	}
	var comp Supplier
	db.Where("organization_id = ? AND name = ?", org, "Demo Hosting Australia").Take(&comp)
	if comp.Type != TypeCompetitor || comp.Currency != "AUD" {
		t.Fatalf("new supplier from the file: %+v", comp)
	}
	items := s.Items(org, ItemFilter{Q: "launch"}, testToday)
	if len(items) != 1 || !items[0].Promo || *items[0].RegularPrice != 131.88 || items[0].Source != "csv" || items[0].SupplierName != "hosting.com" || items[0].Stale {
		t.Fatalf("launch: %+v", items)
	}
	// The SSL price is dated 2026-05-15: older than 90 days.
	stale := s.Items(org, ItemFilter{Stale: true}, testToday)
	if len(stale) != 1 || stale[0].Name != "Standard SSL" || *stale[0].AgeDays != 140 {
		t.Fatalf("stale: %+v", stale)
	}
	// Importing the same file again changes nothing.
	again, _ := s.Import(org, "u1", sample(t), false, testToday)
	if again.Unchanged != 28 || again.Created != 0 || again.Updated != 0 || len(again.NewSuppliers) != 0 {
		t.Fatalf("re-import: %+v", again)
	}
	var hist int64
	db.Model(&PriceChange{}).Count(&hist)
	if hist != 0 {
		t.Fatalf("no price changed yet: %d", hist)
	}

	// A messy update: different case and spacing, a currency symbol, one new
	// row, one bad row. It upserts by supplier + kind + name + TLD + period.
	update := CSVHeader + "\n" +
		"synergy  wholesale,,,Hosting Plan,STARTER ENV,,Monthly,A$7.25,,,2 GB,1,5,2026-10-01,,price rise\n" +
		"SYNERGY WHOLESALE,,,domain_register,,CO.KE,year,21.00,,,,,,2026-09-28,https://example.test/synergy/domains,SAMPLE made-up price\n" +
		"Synergy Wholesale,,,hosting_plan,Mega Env,,month,31,,,20,1,unlimited,,,\n" +
		"Synergy Wholesale,,,hosting_plan,Broken,,month,abc,,,,,,,,\n"
	pre, err = s.Import(org, "u1", []byte(update), true, testToday)
	if err != nil {
		t.Fatal(err)
	}
	if pre.Updated != 1 || pre.Unchanged != 1 || pre.Created != 1 || pre.Errors != 1 || len(pre.NewSuppliers) != 0 {
		t.Fatalf("update preview: %+v", pre)
	}
	if r := pre.Rows[0]; r.Action != "update" || r.OldPrice == nil || *r.OldPrice != 6.5 || r.Currency != "AUD" {
		t.Fatalf("update row: %+v", r)
	}
	if i, _ := count(); i != 28 {
		t.Fatal("the preview wrote something")
	}
	if _, err = s.Import(org, "u1", []byte(update), false, testToday); err != nil {
		t.Fatal(err)
	}
	if i, sp := count(); i != 29 || sp != 6 {
		t.Fatalf("after update: %d items, %d suppliers", i, sp)
	}
	st := s.Items(org, ItemFilter{Q: "starter env"}, testToday)[0]
	if st.Price != 7.25 || st.PrevPrice == nil || *st.PrevPrice != 6.5 || st.PriceChangedAt == nil || st.AsOf.Format("2006-01-02") != "2026-10-01" || st.Notes != "price rise" || st.Name != "STARTER ENV" {
		t.Fatalf("updated item: %+v", st)
	}
	_, hrows, err := s.History(org, st.ID, testToday)
	if err != nil || len(hrows) != 1 || hrows[0].Price != 6.5 || hrows[0].NewPrice != 7.25 || hrows[0].AsOf.Format("2006-01-02") != "2026-09-28" || hrows[0].Source != "csv" || hrows[0].ChangedBy != "u1" {
		t.Fatalf("history: %+v %v", hrows, err)
	}
	// A blank as_of on a new row is today.
	mega := s.Items(org, ItemFilter{Q: "mega env"}, testToday)[0]
	if mega.AsOf.Format("2006-01-02") != "2026-10-02" || *mega.EmailAccounts != Unlimited || mega.Currency != "AUD" {
		t.Fatalf("mega: %+v", mega)
	}

	// Export → import: nothing to do (the round trip is lossless).
	out := s.Export(org, ItemFilter{Active: "1"}, testToday)
	if !strings.HasPrefix(string(out), CSVHeader+"\n") {
		t.Fatal("export must start with the exact header")
	}
	rt, err := s.Import(org, "u1", out, true, testToday)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Total != 29 || rt.Unchanged != 29 || rt.Errors != 0 {
		for _, r := range rt.Rows {
			if r.Action != "unchanged" {
				t.Logf("line %d %s: %v %v", r.Line, r.Action, r.Errors, r.Warnings)
			}
		}
		t.Fatalf("round trip: %+v", rt)
	}
	// A future date and an unknown file are refused politely.
	fut, _ := s.Import(org, "u1", []byte(CSVHeader+"\nSynergy Wholesale,,,hosting_plan,Future,,month,5,,,,,,2027-01-01,,\n"), true, testToday)
	if fut.Errors != 1 || !strings.Contains(fut.Rows[0].Errors[0], "future") {
		t.Fatalf("future date: %+v", fut.Rows)
	}
	var pe *ParseError
	if _, err := s.Import(org, "u1", []byte("hello\nworld\n"), true, testToday); !errors.As(err, &pe) {
		t.Fatalf("not a price list: %v", err)
	}
	// Other organisations see nothing of this.
	if got := s.Items("org-2", ItemFilter{}, testToday); len(got) != 0 {
		t.Fatalf("org-2 sees %d items", len(got))
	}
}

func TestItemCRUDAndHistory(t *testing.T) {
	s, _ := testDB(t)
	s.EnsureSeed(org)
	syn := s.Suppliers(org)[0]
	it, err := s.SaveItem(org, "u1", "", ItemInput{SupplierID: Of(syn.ID), Kind: Of("hosting plan"), Name: Of("  Starter   Env "), Period: Of("monthly"), Price: Of(6.5), DiskGB: Of(2.0), Sites: Of(1)}, "manual", testToday)
	if err != nil {
		t.Fatal(err)
	}
	if it.Name != "Starter Env" || it.Kind != KindHosting || it.Period != PeriodMonth || it.Currency != "AUD" || !it.Active || it.AsOf.Format("2006-01-02") != "2026-10-02" || it.SupplierName != "Synergy Wholesale" || it.Stale {
		t.Fatalf("created: %+v", it)
	}
	// The same key again is refused.
	if _, err := s.SaveItem(org, "u1", "", ItemInput{SupplierID: Of(syn.ID), Kind: Of(KindHosting), Name: Of("starter env"), Period: Of("month"), Price: Of(9.0)}, "manual", testToday); err == nil || !strings.Contains(err.Error(), "already has this price") {
		t.Fatalf("duplicate: %v", err)
	}
	// Inline edit of one field leaves the others alone.
	up, err := s.SaveItem(org, "u2", it.ID, ItemInput{Notes: Of("incl. GST")}, "manual", testToday)
	if err != nil || up.Price != 6.5 || up.Notes != "incl. GST" || *up.DiskGB != 2 || up.PrevPrice != nil {
		t.Fatalf("partial update: %+v %v", up, err)
	}
	// A price change keeps the old price; the date moves to today.
	later := testToday.AddDate(0, 0, 30)
	up, err = s.SaveItem(org, "u2", it.ID, ItemInput{Price: Of(7.0), RegularPrice: Of(9.5)}, "manual", later)
	if err != nil || up.Price != 7 || *up.PrevPrice != 6.5 || !up.Promo || up.AsOf.Format("2006-01-02") != "2026-11-01" {
		t.Fatalf("price change: %+v %v", up, err)
	}
	up, err = s.SaveItem(org, "u2", it.ID, ItemInput{Price: Of(7.5), RegularPrice: Null[float64](), DiskGB: Null[float64](), AsOf: Of("2026-10-30")}, "manual", later)
	if err != nil {
		t.Fatal(err)
	}
	if up.RegularPrice != nil || up.Promo || up.DiskGB != nil || *up.PrevPrice != 7 || up.AsOf.Format("2006-01-02") != "2026-10-30" {
		t.Fatalf("cleared fields: %+v", up)
	}
	_, h, _ := s.History(org, it.ID, later)
	if len(h) != 2 || h[0].Price != 7 || h[0].NewPrice != 7.5 || *h[0].RegularPrice != 9.5 || h[0].NewRegularPrice != nil || h[1].Price != 6.5 || h[1].NewPrice != 7 || h[1].ChangedBy != "u2" {
		t.Fatalf("history: %+v", h)
	}
	// Domains: TLD normalised, named after it, yearly only.
	d, err := s.SaveItem(org, "u1", "", ItemInput{SupplierID: Of(syn.ID), Kind: Of(KindRegister), TLD: Of("CO.KE"), Price: Of(21.0)}, "manual", testToday)
	if err != nil || d.TLD != ".co.ke" || d.Name != ".co.ke registration" || d.Period != PeriodYear {
		t.Fatalf("domain: %+v %v", d, err)
	}
	for name, in := range map[string]ItemInput{
		"no tld":         {SupplierID: Of(syn.ID), Kind: Of(KindRenew), Price: Of(1.0)},
		"monthly domain": {SupplierID: Of(syn.ID), Kind: Of(KindRenew), TLD: Of(".com"), Period: Of("month"), Price: Of(1.0)},
		"negative":       {SupplierID: Of(syn.ID), Kind: Of(KindHosting), Name: Of("X"), Price: Of(-1.0)},
		"no name":        {SupplierID: Of(syn.ID), Kind: Of(KindHosting), Price: Of(1.0)},
		"bad kind":       {SupplierID: Of(syn.ID), Kind: Of("vps"), Name: Of("X"), Price: Of(1.0)},
		"bad supplier":   {SupplierID: Of("nope"), Kind: Of(KindHosting), Name: Of("X"), Price: Of(1.0)},
		"future date":    {SupplierID: Of(syn.ID), Kind: Of(KindHosting), Name: Of("X"), Price: Of(1.0), AsOf: Of("2027-06-01")},
		"bad currency":   {SupplierID: Of(syn.ID), Kind: Of(KindHosting), Name: Of("X"), Price: Of(1.0), Currency: Of("ZZZ")},
	} {
		var ve *ValidationError
		if _, err := s.SaveItem(org, "u1", "", in, "manual", testToday); !errors.As(err, &ve) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}
	// Another organisation cannot touch it.
	if _, err := s.SaveItem("org-2", "x", it.ID, ItemInput{Price: Of(1.0)}, "manual", testToday); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org update: %v", err)
	}
	if err := s.DeleteItem("org-2", it.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org delete: %v", err)
	}
	// Deactivated prices leave the comparison; deleting removes the history too.
	if _, err := s.SaveItem(org, "u1", it.ID, ItemInput{Active: Of(false)}, "manual", testToday); err != nil {
		t.Fatal(err)
	}
	if got := s.Items(org, ItemFilter{Active: "1"}, testToday); len(got) != 1 {
		t.Fatalf("active items: %d", len(got))
	}
	if err := s.DeleteItem(org, it.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.History(org, it.ID, testToday); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestSuppliersAndSelfHosted(t *testing.T) {
	s, _ := testDB(t)
	s.EnsureSeed(org)
	if _, err := s.Import(org, "u1", sample(t), false, testToday); err != nil {
		t.Fatal(err)
	}
	self := s.Suppliers(org)[2]
	if self.Type != TypeSelfHosted {
		t.Fatalf("order: wholesale, self-hosted, competitors — got %s", self.Name)
	}
	costs := []CostLine{
		{Key: "server", Label: "Server / VPS", Kind: "fixed", Amount: 40, Currency: "usd"},
		{Key: "panel", Label: "Control-panel licence", Kind: "fixed", Amount: 15, Currency: "USD"},
		{Key: "backups", Label: "Backups", Kind: "fixed", Amount: 500, Currency: "KES"},
		{Key: "time", Label: "Your time", Kind: "time", Hours: 2, Rate: 1500},
		{Label: "", Kind: "fixed"},
	}
	capacity, sites := 40, 12
	if _, err := s.SaveSupplier(org, self.ID, SupplierInput{Costs: &costs, Capacity: &capacity, CurrentSites: &sites}); err != nil {
		t.Fatal(err)
	}
	m, err := s.SelfHosted(org, self.ID, CompareInput{Tier: "starter"}, testToday)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "monthly total", m.MonthlyTotal, 10595)
	if len(m.Rows) != 4 || m.Rows[2].Currency != "KES" {
		t.Fatalf("rows: %+v", m.Rows)
	}
	if len(m.BreakEven) != 2 || m.BreakEven[0].SupplierName != "Synergy Wholesale" || m.BreakEven[0].Sites != 19 || m.BreakEven[1].Sites != 8 {
		t.Fatalf("break-even: %+v", m.BreakEven)
	}
	// No Synergy data here: the known site count is the server's.
	if n, src := s.KnownSites(org, testToday); n != 12 || src != "self_hosted" {
		t.Fatalf("known sites: %d from %q", n, src)
	}
	// Not a self-hosted supplier.
	if _, err := s.SelfHosted(org, s.Suppliers(org)[0].ID, CompareInput{}, testToday); err == nil {
		t.Fatal("a wholesale supplier has no cost model")
	}
	// Names are unique per organisation; validation is friendly.
	name := "synergy wholesale"
	var ve *ValidationError
	if _, err := s.SaveSupplier(org, "", SupplierInput{Name: &name}); !errors.As(err, &ve) {
		t.Fatalf("duplicate name: %v", err)
	}
	neg := -1.0
	if _, err := s.SaveSupplier(org, self.ID, SupplierInput{Costs: &[]CostLine{{Label: "Server", Amount: neg}}}); !errors.As(err, &ve) {
		t.Fatalf("negative cost: %v", err)
	}
	n2, typ, cur := "Rival Hosting", "competitor", "kes"
	sp, err := s.SaveSupplier(org, "", SupplierInput{Name: &n2, Type: &typ, Currency: &cur})
	if err != nil || sp.Type != TypeCompetitor || sp.Currency != "KES" {
		t.Fatalf("new supplier: %+v %v", sp, err)
	}
	// Deleting a supplier removes its prices.
	syn := s.Suppliers(org)[0]
	if err := s.DeleteSupplier(org, syn.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.Items(org, ItemFilter{SupplierID: syn.ID}, testToday); len(got) != 0 {
		t.Fatalf("prices left behind: %d", len(got))
	}
	if err := s.DeleteSupplier("org-2", self.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org delete: %v", err)
	}
}

func TestCompareRetailAndSaveService(t *testing.T) {
	s, db := testDB(t)
	s.EnsureSeed(org)
	if _, err := s.Import(org, "u1", sample(t), false, testToday); err != nil {
		t.Fatal(err)
	}
	r := s.Compare(org, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", TermYears: 2}, testToday)
	if r.Home != "KES" || len(r.Rates) != 2 || r.Rates[0].Source != fxrates.SourceManual || r.Rates[0].Rate != 86 || r.Rates[0].AsOf == nil {
		t.Fatalf("rates with their source and date: %+v", r.Rates)
	}
	c := r.Combo(".co.ke")
	syn := r.FindOption(".co.ke", "")
	if c.Best == "mix" || syn == nil || syn.SupplierName != "Synergy Wholesale" {
		t.Fatalf("best over 2 years: %s", c.Best)
	}
	near(t, "first year", syn.FirstYearH, 99*86)
	near(t, "term", syn.TermH, (21+23.5+156)*86)
	// Three competitors sell the combo: 3,499 / 4,700 and (64.00 + 7.95 × 12) AUD = 13,708.40.
	if len(c.Benchmarks) != 3 || c.Market == nil || c.Market.FirstYear.Count != 3 || c.Market.FirstYear.Median != 4700 {
		t.Fatalf("benchmarks: %d, market %+v", len(c.Benchmarks), c.Market)
	}
	near(t, "dearest competitor", c.Market.FirstYear.Max, 64*86+7.95*12*86)
	// What-if: AUD at 100.
	r = s.Compare(org, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", TermYears: 2, Rates: map[string]float64{"AUD": 100}}, testToday)
	if ri := r.Rates[0]; !ri.Overridden || ri.Rate != 100 || ri.DefaultRate != 86 || ri.Source != "what-if" {
		t.Fatalf("what-if rate: %+v", ri)
	}
	near(t, "what-if first year", r.FindOption(".co.ke", c.Best).FirstYearH, 9900)

	// Retail: 35% margin, VAT 16%, M-Pesa 2.9% → KES 14,190 a year.
	req := RetailRequest{Compare: CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", TermYears: 2}, TLD: ".co.ke", Key: c.Best,
		Retail: RetailInput{Mode: "margin", Percent: 35, Markets: []RetailMarket{{Currency: "KES", TaxRate: 16, FeePercent: 2.9}, {Currency: "AUD", FeePercent: 4.8}}}}
	res, _, err := s.Retail(org, req, testToday)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "cost", res.CostHome, 8729)
	near(t, "KES yearly", res.Markets[0].Yearly.Price, 14190)
	near(t, "AUD yearly", res.Markets[1].Yearly.Price, 170)
	if len(res.Markets[0].Positions) != 2 {
		t.Fatalf("positions: %+v", res.Markets[0].Positions)
	}
	if _, _, err := s.Retail(org, RetailRequest{Compare: req.Compare, TLD: ".co.ke", Key: "s:nope", Retail: req.Retail}, testToday); err == nil {
		t.Fatal("unknown option must fail")
	}

	// Save as service: the catalog row has the price, the currency and the cost note.
	out, err := s.SaveServices(org, []ServiceInput{
		{Name: "Starter hosting + .co.ke domain — yearly", Price: res.Markets[0].Yearly.Price, Currency: "KES", Description: res.CostNote},
		{Name: "Starter hosting + .co.ke domain — yearly (AUD)", Price: res.Markets[1].Yearly.Price, Currency: "aud", PriceIncludesTax: true},
	})
	if err != nil || len(out) != 2 || out[0].Action != "created" || out[0].Category != DefaultServiceCategory {
		t.Fatalf("save services: %+v %v", out, err)
	}
	var row struct {
		Name, Description, Category, Currency string
		Price                                 float64
		Duration                              int
		IsActive, PriceIncludesTax            bool
	}
	db.Raw("SELECT * FROM services WHERE id = ?", out[0].ID).Scan(&row)
	if row.Price != 14190 || row.Currency != "KES" || !row.IsActive || row.PriceIncludesTax || !strings.Contains(row.Description, "Cost KES 8,729.00 a year") || row.Category != "Hosting & domains" {
		t.Fatalf("service row: %+v", row)
	}
	db.Raw("SELECT * FROM services WHERE id = ?", out[1].ID).Scan(&row)
	if row.Price != 170 || row.Currency != "AUD" || !row.PriceIncludesTax {
		t.Fatalf("AUD service: %+v", row)
	}
	// Saving again updates the price instead of adding a second service.
	out2, err := s.SaveServices(org, []ServiceInput{{Name: "starter hosting + .co.ke domain — yearly", Price: 14990, Currency: "KES"}})
	if err != nil || out2[0].Action != "updated" || out2[0].ID != out[0].ID {
		t.Fatalf("update service: %+v %v", out2, err)
	}
	var n int64
	db.Table("services").Where("organization_id = ?", org).Count(&n)
	if n != 2 {
		t.Fatalf("services: %d", n)
	}
	var ve *ValidationError
	if _, err := s.SaveServices(org, []ServiceInput{{Name: " ", Price: 1, Currency: "KES"}}); !errors.As(err, &ve) {
		t.Fatalf("nameless service: %v", err)
	}
	if _, err := s.SaveServices(org, []ServiceInput{{Name: "X", Price: 1, Currency: "ZZZ"}}); !errors.As(err, &ve) {
		t.Fatalf("bad currency: %v", err)
	}
}

func TestScenariosAndTiers(t *testing.T) {
	s, _ := testDB(t)
	in := map[string]any{"compare": map[string]any{"tlds": []any{".co.ke"}, "tier": "starter"}}
	sc, err := s.SaveScenario(org, "u1", "", ScenarioInput{Name: "  Starter  .co.ke ", Input: in, Snapshot: map[string]any{"price": 14190.0}})
	if err != nil || sc.Name != "Starter .co.ke" || sc.CreatedBy != "u1" {
		t.Fatalf("save: %+v %v", sc, err)
	}
	// The same name again replaces it.
	sc2, err := s.SaveScenario(org, "u2", "", ScenarioInput{Name: "starter .co.ke", Snapshot: map[string]any{"price": 14990.0}})
	if err != nil || sc2.ID != sc.ID {
		t.Fatalf("replace: %+v %v", sc2, err)
	}
	list := s.Scenarios(org)
	if len(list) != 1 || list[0].Snapshot["price"] != 14990.0 || list[0].Input["compare"] == nil {
		t.Fatalf("list: %+v", list)
	}
	if _, err := s.SaveScenario(org, "u1", "", ScenarioInput{Name: ""}); err == nil {
		t.Fatal("a scenario needs a name")
	}
	if got := s.Scenarios("org-2"); len(got) != 0 {
		t.Fatal("scenarios are per organisation")
	}
	if err := s.DeleteScenario("org-2", sc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org delete: %v", err)
	}
	if err := s.DeleteScenario(org, sc.ID); err != nil {
		t.Fatal(err)
	}
	// Tiers can be changed and drive the comparison.
	tiers, err := s.SaveTiers(org, []Tier{{Key: "starter", Label: "Starter", MinDiskGB: 4, MinSites: 1}, {Key: "pro", Label: "Pro", MinDiskGB: 8}})
	if err != nil || len(tiers) != 2 || tiers[1].MinSites != 1 {
		t.Fatalf("tiers: %+v %v", tiers, err)
	}
	if got := (CompareInput{Tier: "starter"}).Normalize(s.Tiers(org)); got.MinDiskGB != 4 {
		t.Fatalf("tier in use: %+v", got)
	}
	if _, err := s.SaveTiers(org, nil); err == nil {
		t.Fatal("at least one tier")
	}
}

func TestAutofillSynergy(t *testing.T) {
	s, db := testDB(t)
	s.EnsureSeed(org)
	// Nothing uploaded, API not connected: says so, adds nothing.
	res, err := s.AutofillSynergy(context.Background(), org, "u1", AutofillOptions{}, nil, testToday)
	if err != nil || res.Plans != 0 || res.Created != 0 || res.APIConnected || !strings.Contains(res.Message, "Nothing to fill in yet") {
		t.Fatalf("empty autofill: %+v %v", res, err)
	}
	// Made-up statements: two plans charged monthly and one renewal.
	d := func(s string) time.Time { return *day(s) }
	snap := d("2026-09-01")
	for i, u := range []expenses.SynergyUsage{
		{Domain: "alpha.co.ke", Plan: "Basic Env", LimitMB: 2048, UsageMB: 900},
		{Domain: "beta.com.au", Plan: "Mega Env", LimitMB: 10240, UsageMB: 5000},
	} {
		u.ID, u.OrganizationID, u.SnapshotDate, u.Status = "u"+itoa(i), org, snap, "Active"
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i, x := range []expenses.SynergyTxn{
		{Date: d("2026-09-01"), Kind: expenses.SynHostingMonthly, Domain: "alpha.co.ke", Debit: 4.25, Description: "alpha.co.ke (from 1 Sep 2026 to 1 Oct 2026)"},
		{Date: d("2026-10-01"), Kind: expenses.SynHostingMonthly, Domain: "alpha.co.ke", Debit: 4.25, Description: "alpha.co.ke (from 1 Oct 2026 to 1 Nov 2026)"},
		{Date: d("2026-10-01"), Kind: expenses.SynHostingMonthly, Domain: "beta.com.au", Debit: 15.40, Description: "beta.com.au (from 1 Oct 2026 to 1 Nov 2026)"},
		{Date: d("2026-09-14"), Kind: expenses.SynDomainRenewal, Domain: "beta.com.au", Debit: 12.40, Description: "Auto renewal of beta.com.au for 1 year"},
	} {
		x.ID, x.OrganizationID, x.DedupeKey, x.Seq, x.Amount, x.ProductType = "t"+itoa(i), org, "k"+itoa(i), i, x.Debit, "Web Hosting"
		if err := db.Create(&x).Error; err != nil {
			t.Fatal(err)
		}
	}
	res, err = s.AutofillSynergy(context.Background(), org, "u1", AutofillOptions{}, nil, testToday)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plans != 2 || res.Domains != 1 || res.Created != 3 || res.DomainSource != "statements" || res.APIConnected || !res.HasStatements {
		t.Fatalf("statements autofill: %+v", res)
	}
	syn := s.Suppliers(org)[0]
	items := s.Items(org, ItemFilter{SupplierID: syn.ID}, testToday)
	if len(items) != 3 {
		t.Fatalf("items: %d", len(items))
	}
	basic, mega, renew := items[0], items[1], items[2]
	if basic.Name != "Basic Env" || basic.Price != 4.25 || basic.Period != PeriodMonth || *basic.DiskGB != 2 || *basic.Sites != 1 || basic.Currency != "AUD" || basic.Source != "synergy_statements" ||
		!strings.Contains(basic.Notes, "Learned from your Synergy statements") {
		t.Fatalf("basic: %+v", basic)
	}
	if mega.Name != "Mega Env" || mega.Price != 15.4 || *mega.DiskGB != 10 {
		t.Fatalf("mega: %+v", mega)
	}
	if renew.Kind != KindRenew || renew.TLD != ".com.au" || renew.Price != 12.4 || renew.AsOf.Format("2006-01-02") != "2026-09-14" {
		t.Fatalf("renewal learned from the statement: %+v", renew)
	}
	// The app now knows how many client sites are hosted (for spreading reseller plans).
	if n, src := s.KnownSites(org, testToday); n != 2 || src != "synergy" {
		t.Fatalf("known sites: %d from %q", n, src)
	}
	if c := s.ContextFor(org); c.KnownSites != 2 || c.KnownSitesSource != "synergy" {
		t.Fatalf("context known sites: %d %q", c.KnownSites, c.KnownSitesSource)
	}
	// The statements only taught a .com.au renewal: Synergy is still a complete .com.au option.
	rr := s.Compare(org, CompareInput{TLDs: []string{".com.au"}, Tier: "starter"}, testToday)
	if o := rr.FindOption(".com.au", "s:"+syn.ID); o == nil || !o.Complete || !o.Domain.Assumed || o.Domain.AssumedWhat != "registration" || o.Domain.FirstYear != 12.4 {
		t.Fatalf("renewal-only supplier: %+v", o)
	}
	// With the API: register / renew / transfer for the TLDs in use and the common ones.
	api := func(ctx context.Context, db *gorm.DB, o string) ([]DomainPrice, error) {
		return []DomainPrice{
			{TLD: ".co.ke", Register: fp(21), Renew: fp(23.5), Transfer: fp(21)},
			{TLD: ".com.au", Register: fp(9.9), Renew: fp(12.9)},
			{TLD: ".xyz", Register: fp(3.5), Renew: fp(19)},
		}, nil
	}
	res, err = s.AutofillSynergy(context.Background(), org, "u1", AutofillOptions{}, api, testToday.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	// 5 domain prices (.xyz is not in use), 4 of them new; the .com.au renewal changes 12.40 → 12.90; both plans unchanged.
	if !res.APIConnected || res.DomainSource != "api" || res.Domains != 5 || res.Created != 4 || res.Updated != 1 || res.Unchanged != 2 {
		t.Fatalf("api autofill: %+v", res)
	}
	items = s.Items(org, ItemFilter{SupplierID: syn.ID, Q: ".com.au renewal"}, testToday)
	if len(items) != 1 || items[0].Price != 12.9 || *items[0].PrevPrice != 12.4 || items[0].Source != "synergy_api" {
		t.Fatalf("renewal after the API: %+v", items)
	}
	if got := s.Items(org, ItemFilter{SupplierID: syn.ID, Q: ".xyz"}, testToday); len(got) != 0 {
		t.Fatal(".xyz was not asked for")
	}
	// Asking for it pulls it; running again changes nothing.
	res, _ = s.AutofillSynergy(context.Background(), org, "u1", AutofillOptions{TLDs: []string{"xyz"}}, api, testToday.AddDate(0, 0, 1))
	if res.Created != 2 || res.Updated != 0 || res.Unchanged != 7 {
		t.Fatalf("xyz: %+v", res)
	}
	// An API failure falls back to the statements and reports the error.
	bad := func(ctx context.Context, db *gorm.DB, o string) ([]DomainPrice, error) {
		return nil, &expenses.SynergyError{Status: "ERR_LOGIN_FAILED", Message: "Synergy Wholesale: Unable to login", Hint: "whitelist the IP"}
	}
	res, _ = s.AutofillSynergy(context.Background(), org, "u1", AutofillOptions{}, bad, testToday.AddDate(0, 0, 1))
	if res.APIError == "" || !strings.Contains(res.APIError, "whitelist") || res.DomainSource != "statements" {
		t.Fatalf("api failure: %+v", res)
	}
	// The comparison now works from the learned prices.
	r := s.Compare(org, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"}, testToday)
	o := r.FindOption(".co.ke", "s:"+syn.ID)
	if o == nil || !o.Complete || o.Hosting.Name != "Basic Env" {
		t.Fatalf("compare after autofill: %+v", o)
	}
	near(t, "first year AUD", o.FirstYear, 21+4.25*12)
}

func TestParseDomainPricing(t *testing.T) {
	m := map[string]any{"status": "OK", "pricing": []any{
		map[string]any{"tld": "com.au", "register_1_year": "9.90", "renew": "12.40"},
		map[string]any{"tld": ".CO.KE", "register_1_year": "21.00", "renew": "23.50", "transfer": "21.00"},
		map[string]any{"tld": "com", "renew": "15.50"},
		map[string]any{"tld": "", "renew": "1"},
		map[string]any{"tld": "bad", "renew": "x"},
		"junk",
	}}
	got := ParseDomainPricing(m)
	if len(got) != 3 || got[0].TLD != ".co.ke" || *got[0].Register != 21 || *got[0].Transfer != 21 || got[1].TLD != ".com" || got[1].Register != nil || *got[1].Renew != 15.5 || got[2].TLD != ".com.au" {
		t.Fatalf("pricing: %+v", got)
	}
	// A single entry decodes as an object, not a list.
	one := ParseDomainPricing(map[string]any{"pricing": map[string]any{"item": map[string]any{"tld": "ke", "register_1_year": "30"}}})
	if len(one) != 1 || one[0].TLD != ".ke" {
		t.Fatalf("single entry: %+v", one)
	}
	for in, want := range map[string]string{"example.co.ke": ".co.ke", "shop.example.com.au": ".com.au", "example.com": ".com", "EXAMPLE.ORG.": ".org", "a.b.africa": ".africa", "localhost": "", "x.or.ke": ".or.ke"} {
		if got := TLDOfDomain(in); got != want {
			t.Errorf("TLDOfDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
