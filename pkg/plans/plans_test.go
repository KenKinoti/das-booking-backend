package plans

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func testCatalog() *Catalog {
	return &Catalog{Modules: DefaultModules(), Industries: DefaultIndustries(), Tiers: DefaultTiers()}
}

var usdOnly = StaticRates{}

func TestSmallPriceGrowsWithModules(t *testing.T) {
	cat := testCatalog()
	one, err := Price(cat, Selection{Tier: "small", Modules: []string{"invoicing"}}, usdOnly)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Price(cat, Selection{Tier: "small", Modules: []string{"invoicing", "pos"}}, usdOnly)
	if err != nil {
		t.Fatal(err)
	}
	if one.Monthly != 9+15 {
		t.Fatalf("one module: want 24, got %v", one.Monthly)
	}
	if two.Monthly != 9+15+25 {
		t.Fatalf("two modules: want 49, got %v", two.Monthly)
	}
	if !reflect.DeepEqual(two.Entitlements, []string{"core", "invoicing", "pos"}) {
		t.Fatalf("entitlements: %v", two.Entitlements)
	}
}

func TestSmallMaxSystems(t *testing.T) {
	cat := testCatalog()
	_, err := Price(cat, Selection{Tier: "small", Modules: []string{"invoicing", "pos", "crm"}}, usdOnly)
	if !errors.Is(err, ErrTooManyModules) {
		t.Fatalf("want ErrTooManyModules, got %v", err)
	}
	// core and duplicates don't count
	if _, err := Price(cat, Selection{Tier: "small", Modules: []string{"core", "pos", "POS", "crm"}}, usdOnly); err != nil {
		t.Fatalf("core/dupes should not count: %v", err)
	}
	// limit is configurable
	for i := range cat.Tiers {
		if cat.Tiers[i].Key == "small" {
			cat.Tiers[i].MaxModules = 3
		}
	}
	if _, err := Price(cat, Selection{Tier: "small", Modules: []string{"invoicing", "pos", "crm"}}, usdOnly); err != nil {
		t.Fatalf("raised limit: %v", err)
	}
	if _, err := Price(cat, Selection{Tier: "small"}, usdOnly); !errors.Is(err, ErrNoModules) {
		t.Fatalf("want ErrNoModules, got %v", err)
	}
	if _, err := Price(cat, Selection{Tier: "small", Modules: []string{"teleport"}}, usdOnly); !errors.Is(err, ErrUnknownModule) {
		t.Fatalf("want ErrUnknownModule, got %v", err)
	}
}

func TestSMEBundleAndDiscountedAddons(t *testing.T) {
	cat := testCatalog()
	q, err := Price(cat, Selection{Tier: "sme", Industry: "retail", Modules: []string{"pos", "accounting"}}, usdOnly)
	if err != nil {
		t.Fatal(err)
	}
	// pos is in the retail bundle; accounting is an add-on at 20% off 29 = 23.2
	if !reflect.DeepEqual(q.Addons, []string{"accounting"}) {
		t.Fatalf("addons: %v", q.Addons)
	}
	if q.MonthlyUSD != 79+23.2 {
		t.Fatalf("monthly usd: %v", q.MonthlyUSD)
	}
	for _, m := range []string{"pos", "inventory", "ecommerce", "invoicing", "crm", "accounting"} {
		found := false
		for _, e := range q.Entitlements {
			found = found || e == m
		}
		if !found {
			t.Fatalf("missing entitlement %s in %v", m, q.Entitlements)
		}
	}
}

func TestLargeIncludesEverything(t *testing.T) {
	cat := testCatalog()
	q, err := Price(cat, Selection{Tier: "large", Modules: []string{"pos"}}, usdOnly)
	if err != nil {
		t.Fatal(err)
	}
	if q.Monthly != 399 || len(q.Addons) != 0 {
		t.Fatalf("large: monthly %v addons %v", q.Monthly, q.Addons)
	}
	if len(q.Entitlements) != len(cat.Modules) {
		t.Fatalf("large entitlements %d, want %d", len(q.Entitlements), len(cat.Modules))
	}
	custom := 250.0
	q, err = Price(cat, Selection{Tier: "large", CustomPriceUSD: &custom}, usdOnly)
	if err != nil || q.Monthly != 250 || !q.CustomPrice {
		t.Fatalf("custom price: %v %+v", err, q)
	}
}

func TestAnnualDiscountAndCurrency(t *testing.T) {
	cat := testCatalog()
	rates := StaticRates{"KES": 129, "KWD": 0.307, "UGX": 3700}
	q, err := Price(cat, Selection{Tier: "small", Modules: []string{"invoicing"}, Cycle: "annual"}, rates)
	if err != nil {
		t.Fatal(err)
	}
	// 24/month * 12 = 288, 15% off = 244.8 → nice rounded 245
	if q.Total != 245 || q.Cycle != CycleAnnual {
		t.Fatalf("annual total %v", q.Total)
	}
	k, err := Price(cat, Selection{Tier: "small", Modules: []string{"invoicing"}, Currency: "KES"}, rates)
	if err != nil {
		t.Fatal(err)
	}
	// 9*129=1161 → 1160, 15*129=1935 → 1940 (3 significant figures)
	if k.Monthly != 1160+1940 || k.Decimals != 2 {
		t.Fatalf("KES monthly %v", k.Monthly)
	}
	u, _ := Price(cat, Selection{Tier: "small", Modules: []string{"invoicing"}, Currency: "UGX"}, rates)
	if u.Decimals != 0 || u.Monthly != 33300+55500 {
		t.Fatalf("UGX monthly %v", u.Monthly)
	}
	if _, err := Price(cat, Selection{Tier: "small", Modules: []string{"invoicing"}, Currency: "XYZ"}, rates); !errors.Is(err, ErrUnsupportedCurr) {
		t.Fatalf("want unsupported currency, got %v", err)
	}
	if _, err := Price(cat, Selection{Tier: "small", Modules: []string{"invoicing"}, Currency: "EUR"}, rates); !errors.Is(err, ErrRateNotAvailable) {
		t.Fatalf("want rate unavailable, got %v", err)
	}
}

func TestNiceRound(t *testing.T) {
	cases := []struct {
		v    float64
		dec  int
		want float64
	}{
		{3741.12, 2, 3740}, {44950, 2, 45000}, {26.68, 2, 27}, {8.903, 3, 8.903}, {4.994, 2, 4.99}, {399, 2, 399}, {123456, 0, 123000},
	}
	for _, c := range cases {
		if got := NiceRound(c.v, c.dec); got != c.want {
			t.Errorf("NiceRound(%v,%d) = %v, want %v", c.v, c.dec, got, c.want)
		}
	}
}

func TestAccessOfDegradesGracefully(t *testing.T) {
	cat := testCatalog()
	sub := &Subscription{Tier: "small", Industry: "general", Modules: []string{"invoicing", "pos", "crm"}, Status: StatusActive}
	a := AccessOf(cat, sub)
	if !a.Has("invoicing") || !a.Has("pos") || a.Has("crm") || !a.Has("core") {
		t.Fatalf("access %v", a.ModuleList())
	}
	sub.Status = StatusCancelled
	if a := AccessOf(cat, sub); a.Has("invoicing") || !a.Has("core") {
		t.Fatalf("cancelled access %v", a.ModuleList())
	}
}

func TestRuleMapping(t *testing.T) {
	cases := map[string]string{
		"/api/v1/invoicing/invoices":        "invoicing",
		"/api/v1/pos/transactions":          "pos",
		"/api/v1/bookings":                  "bookings",
		"/api/v1/services/abc":              "bookings",
		"/api/v1/events/1/tickets":          "events",
		"/api/v1/inventory/products":        "inventory",
		"/api/v1/suppliers/purchase-orders": "inventory",
		"/api/v1/finance/accounts":          "accounting",
		"/api/v1/crm/deals":                 "crm",
		"/api/v1/customers/1":               "crm",
		"/api/v1/meetings":                  "communication",
		"/api/v1/messaging/conversations":   "communication",
		"/api/v1/video/rooms":               "communication",
		"/api/v1/pm/projects":               "projects",
		"/api/v1/production/work-orders":    "manufacturing",
		"/api/v1/hcm/employees":             "hr",
		"/api/v1/ecommerce/orders":          "ecommerce",
		"/api/v1/smallbiz/summary":          "smallbiz",
		"/api/v1/documents":                 "documents",
	}
	for path, want := range cases {
		r, ok := MatchRule(path)
		if !ok || r.Modules[0] != want {
			t.Errorf("%s → %v (ok=%v), want %s", path, r.Modules, ok, want)
		}
	}
	for _, path := range []string{"/api/v1/analytics/overview", "/api/v1/plans/me", "/api/v1/posts", "/api/v1/users", "/api/v1/organization"} {
		if r, ok := MatchRule(path); ok {
			t.Errorf("%s should be unmapped, got %v", path, r.Modules)
		}
	}
}

func TestAllowedSharedReads(t *testing.T) {
	inv := Access{Modules: map[string]bool{"core": true, "invoicing": true}}
	check := func(a Access, method, path string, want bool) {
		t.Helper()
		if got, _ := Allowed(a, method, path); got != want {
			t.Errorf("%s %s = %v, want %v", method, path, got, want)
		}
	}
	check(inv, "GET", "/api/v1/customers", true)          // any org can read customers
	check(inv, "POST", "/api/v1/customers", true)         // invoicing may create customers
	check(inv, "GET", "/api/v1/inventory/products", true) // product picker
	check(inv, "POST", "/api/v1/inventory/products", false)
	check(inv, "GET", "/api/v1/services", true)
	check(inv, "GET", "/api/v1/pos/transactions", false)
	check(inv, "GET", "/api/v1/finance/accounts", false)
	events := Access{Modules: map[string]bool{"core": true, "events": true}}
	check(events, "GET", "/api/v1/customers", true)
	check(Access{Modules: map[string]bool{"core": true, "projects": true}}, "DELETE", "/api/v1/customers/1", false)
}

func TestMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	small := Access{Modules: map[string]bool{"core": true, "invoicing": true, "pos": true}, MaxUsers: 2}
	e := &Enforcer{
		Lookup: func(org string) (Access, error) { return small, nil },
		Identify: func(c *gin.Context) (Identity, bool) {
			return Identity{Org: "o1", Role: c.GetHeader("X-Role")}, c.GetHeader("X-Role") != ""
		},
		Users: func(string) int64 { return 2 },
	}
	r := gin.New()
	r.Use(e.Middleware())
	ok := func(c *gin.Context) { c.Status(200) }
	for _, p := range []string{"/api/v1/invoicing/invoices", "/api/v1/crm/deals", "/api/v1/analytics/overview", "/api/v1/public/plans/catalog", "/api/v1/users"} {
		r.GET(p, ok)
	}
	r.POST("/api/v1/users", ok)
	do := func(method, path, role string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		if role != "" {
			req.Header.Set("X-Role", role)
		}
		r.ServeHTTP(w, req)
		return w
	}
	if w := do("GET", "/api/v1/invoicing/invoices", "admin"); w.Code != 200 {
		t.Fatalf("entitled: %d", w.Code)
	}
	w := do("GET", "/api/v1/crm/deals", "admin")
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("not entitled: %d", w.Code)
	}
	if body := w.Body.String(); !contains(body, `"MODULE_NOT_IN_PLAN"`) || !contains(body, `"module":"crm"`) {
		t.Fatalf("body: %s", body)
	}
	if w := do("GET", "/api/v1/crm/deals", "super_admin"); w.Code != 200 {
		t.Fatalf("super admin bypass: %d", w.Code)
	}
	if w := do("GET", "/api/v1/crm/deals", ""); w.Code != 200 {
		t.Fatalf("unauthenticated requests are left to the auth middleware: %d", w.Code)
	}
	if w := do("GET", "/api/v1/analytics/overview", "staff"); w.Code != 200 {
		t.Fatalf("core: %d", w.Code)
	}
	if w := do("POST", "/api/v1/users", "admin"); w.Code != http.StatusPaymentRequired || !contains(w.Body.String(), "USER_LIMIT_REACHED") {
		t.Fatalf("user limit: %d %s", w.Code, w.Body.String())
	}
	if w := do("GET", "/api/v1/users", "admin"); w.Code != 200 {
		t.Fatalf("listing users: %d", w.Code)
	}
	// Lookup failures fail open.
	e.Lookup = func(string) (Access, error) { return Access{}, errors.New("db down") }
	if w := do("GET", "/api/v1/crm/deals", "admin"); w.Code != 200 {
		t.Fatalf("fail open: %d", w.Code)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
