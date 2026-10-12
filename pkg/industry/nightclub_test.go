package industry

import (
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/plans"
)

// ---------------------------------------------------------------- Nightclub / bar / lounge

func TestNightclubTypeDefinition(t *testing.T) {
	bt := typeByKey(t, "nightclub")
	if bt.Name != "Nightclub / bar / lounge" || bt.Icon != "fa-solid fa-martini-glass-citrus" || bt.PlanIndustry != "hospitality" || !bt.Active {
		t.Fatalf("header: %+v", bt)
	}
	if msg := validateType(&bt); msg != "" {
		t.Fatal(msg)
	}
	// Every module exists in the plans catalogue.
	known := map[string]bool{}
	for _, m := range ModuleKeys() {
		known[m] = true
	}
	want := []string{"bookings", "crm", "events", "hr", "inventory", "pos"}
	got := append([]string{}, bt.Modules...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("modules %v", got)
	}
	for _, m := range bt.Modules {
		if !known[m] {
			t.Fatalf("module %s is not a plans module", m)
		}
	}
	// The plan industry bundle exists and covers the core of a club.
	var ind *plans.Industry
	for _, i := range plans.DefaultIndustries() {
		if i.Key == bt.PlanIndustry {
			i := i
			ind = &i
		}
	}
	if ind == nil {
		t.Fatalf("plan industry %s missing", bt.PlanIndustry)
	}
	for _, m := range []string{"pos", "inventory", "bookings", "hr"} {
		if !contains(ind.Modules, m) {
			t.Fatalf("hospitality bundle lacks %s", m)
		}
	}
	// Words.
	terms := Terms(bt)
	for k, v := range map[string]string{
		"customers": "Guests", "customer": "Guest", "bookings": "Table reservations", "booking": "Reservation",
		"inventory": "Bar stock", "inventory_item": "Stock item", "services": "Drinks menu", "service": "Menu item",
		"staff": "Staff", "events": "Events",
	} {
		if terms[k] != v {
			t.Fatalf("term %s = %q, want %q", k, terms[k], v)
		}
	}
	// Booking defaults: tables for a night out.
	if bt.BookingDefaults["resource_label"] != "Table" || bt.BookingDefaults["default_duration"] != 240 || bt.BookingDefaults["vehicle_required"] != false {
		t.Fatalf("booking defaults %v", bt.BookingDefaults)
	}
	if strings.Join(bt.DashboardWidgets, ",") != "bar_tonight,sales_today,reservations_today,low_stock" {
		t.Fatalf("widgets %v", bt.DashboardWidgets)
	}
	if !contains(bt.Features, "bar_stock") || strings.Join(bt.HiddenPaths, ",") != "/crm,/inventory" {
		t.Fatalf("features %v hidden %v", bt.Features, bt.HiddenPaths)
	}
	// Checklist: bars → stock items → tots & menu → delivery → stock take → first sale.
	keys, links := []string{}, []string{}
	for _, c := range bt.Checklist {
		keys = append(keys, c.Key)
		links = append(links, c.To)
	}
	if strings.Join(keys, ",") != "bars,stock_items,menu,delivery,count,pos" {
		t.Fatalf("checklist %v", keys)
	}
	if strings.Join(links, " ") != "/bar-stock?tab=setup /bar-stock?tab=stock /bar-stock?tab=recipes /bar-stock?tab=purchasing /bar-stock?tab=counts /pos" {
		t.Fatalf("checklist links %v", links)
	}
	// Legacy business_type values that mean a club.
	for _, s := range []string{"nightclub", "Bar", " lounge ", "pub"} {
		if TypeKeyFor(s) != "nightclub" {
			t.Fatalf("TypeKeyFor(%q) = %s", s, TypeKeyFor(s))
		}
	}
	if TypeKeyFor("restaurant") != "restaurant" || TypeKeyFor("cafe") != "restaurant" {
		t.Fatal("restaurant aliases changed")
	}
}

func TestNightclubRolePermissions(t *testing.T) {
	bt := typeByKey(t, "nightclub")
	roleKeys := []string{}
	homes := map[string]string{}
	for _, r := range bt.Roles {
		roleKeys = append(roleKeys, r.Key)
		homes[r.Key] = r.Home
		if msg := validPerms(r.Perms); msg != "" {
			t.Fatalf("%s: %s", r.Key, msg)
		}
	}
	if strings.Join(roleKeys, ",") != "owner,bar_manager,store_keeper,bartender,cashier,host" {
		t.Fatalf("roles %v", roleKeys)
	}
	if homes["store_keeper"] != "/bar-stock" || homes["bartender"] != "/pos" || homes["cashier"] != "/pos" || homes["host"] != "/bookings" || homes["bar_manager"] != "" {
		t.Fatalf("homes %v", homes)
	}
	names := map[string]string{}
	for _, r := range bt.Roles {
		names[r.Key] = r.Name
	}
	if names["bar_manager"] != "Bar manager" || names["store_keeper"] != "Store keeper" || names["bartender"] != "Bartender" || names["cashier"] != "Cashier / waiter" || names["host"] != "Host / door" {
		t.Fatalf("names %v", names)
	}

	// Full plan, so only the type and the role decide.
	acts := func(role, module string) string {
		a := accessOf(t, "nightclub", true, everyModule, nil, roleOf(t, "nightclub", role))
		out := []string{}
		for _, act := range AllActions {
			if a.Can(module, act) {
				out = append(out, act)
			}
		}
		return strings.Join(out, ",")
	}
	cases := []struct{ role, module, want string }{
		{"owner", "inventory", "view,create,edit,delete,approve"},
		{"owner", "events", "view,create,edit,delete,approve"},
		{"owner", "invoicing", ""}, // hidden by the type (an admin can enable it as an extra)
		{"bar_manager", "inventory", "view,create,edit,delete,approve"},
		{"bar_manager", "pos", "view,create,edit,delete,approve"},
		{"bar_manager", "bookings", "view,create,edit,delete"},
		{"bar_manager", "events", "view,create,edit,delete"},
		{"bar_manager", "crm", "view,create,edit"},
		{"bar_manager", "hr", "view"},
		{"bar_manager", PermCore, "view"},
		{"bar_manager", PermTeam, "view"},
		{"store_keeper", "inventory", "view,create,edit"},
		{"store_keeper", "pos", ""},
		{"store_keeper", PermCore, ""},
		{"bartender", "pos", "view,create"},
		{"bartender", "inventory", "view,create"},
		{"bartender", "bookings", ""},
		{"bartender", PermCore, ""},
		{"cashier", "pos", "view,create"},
		{"cashier", "bookings", "view"},
		{"cashier", "inventory", ""},
		{"host", "bookings", "view,create,edit"},
		{"host", "events", "view,edit"},
		{"host", "crm", "view,create"},
		{"host", "pos", ""},
		{"host", "inventory", ""},
	}
	for _, c := range cases {
		if got := acts(c.role, c.module); got != c.want {
			t.Errorf("%s on %s: %q, want %q", c.role, c.module, got, c.want)
		}
	}

	// Legacy users.role values map onto the club's roles.
	roles := []OrgRole{}
	for i, rt := range bt.Roles {
		roles = append(roles, templateRole("o", "nightclub", rt, i))
	}
	for legacy, key := range map[string]string{"admin": "owner", "manager": "bar_manager", "staff": "bartender", "care_worker": "bartender"} {
		if r := legacyRole(roles, legacy); r == nil || r.Key != key {
			t.Fatalf("legacy %s → %+v, want %s", legacy, r, key)
		}
	}
}

// The plan still comes first: the hospitality bundle has no events or CRM,
// so the host's events/guest-list permissions only apply once the plan has them.
func TestNightclubPlanPrecedence(t *testing.T) {
	hospitality := []string{"pos", "inventory", "bookings", "hr", "invoicing"}
	host := accessOf(t, "nightclub", true, hospitality, nil, roleOf(t, "nightclub", "host"))
	if host.Can("events", ActEdit) || host.Can("crm", ActCreate) {
		t.Fatal("events/crm must stay off without them in the plan")
	}
	if !host.Can("bookings", ActCreate) {
		t.Fatal("host takes reservations")
	}
	owner := accessOf(t, "nightclub", true, hospitality, nil, roleOf(t, "nightclub", "owner"))
	if owner.Can("invoicing", ActView) {
		t.Fatal("invoicing is in the plan but not part of the club type")
	}
	owner = accessOf(t, "nightclub", true, hospitality, []string{"invoicing"}, roleOf(t, "nightclub", "owner"))
	if !owner.Can("invoicing", ActCreate) {
		t.Fatal("an admin can switch invoicing back on")
	}
	mods := owner.EffectiveModules(ModuleKeys())
	if strings.Join(mods, ",") != "bookings,hr,inventory,invoicing,pos" {
		t.Fatalf("effective %v", mods)
	}
}

// Requests: the inventory API answers per role; /bar (pkg/bar) follows the
// inventory module once its plans rule is registered.
func TestNightclubDecide(t *testing.T) {
	all := everyModule
	bartender := accessOf(t, "nightclub", true, all, nil, roleOf(t, "nightclub", "bartender"))
	keeper := accessOf(t, "nightclub", true, all, nil, roleOf(t, "nightclub", "store_keeper"))
	manager := accessOf(t, "nightclub", true, all, nil, roleOf(t, "nightclub", "bar_manager"))
	cashier := accessOf(t, "nightclub", true, all, nil, roleOf(t, "nightclub", "cashier"))
	type dc struct {
		name         string
		a            Access
		method, path string
		code         string
	}
	cases := []dc{
		{"bartender reads stock", bartender, "GET", "/api/v1/inventory/products", ""},
		{"bartender sells", bartender, "POST", "/api/v1/pos/transactions", ""},
		{"bartender cannot edit products", bartender, "PUT", "/api/v1/inventory/products/p1", "ROLE_FORBIDDEN"},
		{"bartender cannot see reservations", bartender, "GET", "/api/v1/bookings", "ROLE_FORBIDDEN"},
		{"bartender: invoicing hidden for clubs", bartender, "GET", "/api/v1/invoicing/invoices", "MODULE_HIDDEN_FOR_TYPE"},
		{"store keeper receives a PO", keeper, "POST", "/api/v1/inventory/purchase-orders/po1/receive", ""},
		{"store keeper cannot delete", keeper, "DELETE", "/api/v1/inventory/products/p1", "ROLE_FORBIDDEN"},
		{"store keeper cannot sell", keeper, "POST", "/api/v1/pos/transactions", "ROLE_FORBIDDEN"},
		{"cashier sees reservations", cashier, "GET", "/api/v1/bookings", ""},
		{"cashier cannot book", cashier, "POST", "/api/v1/bookings", "ROLE_FORBIDDEN"},
		{"manager deletes", manager, "DELETE", "/api/v1/inventory/products/p1", ""},
	}
	if _, ok := plans.MatchRule("/api/v1/bar/counts"); ok {
		cases = append(cases,
			dc{"bartender starts a count", bartender, "POST", "/api/v1/bar/counts", ""},
			dc{"bartender records wastage", bartender, "POST", "/api/v1/bar/wastage", ""},
			dc{"bartender cannot approve a count", bartender, "POST", "/api/v1/bar/counts/c1/approve", "ROLE_FORBIDDEN"},
			dc{"bartender cannot change settings", bartender, "PUT", "/api/v1/bar/settings", "ROLE_FORBIDDEN"},
			dc{"store keeper cannot approve wastage", keeper, "POST", "/api/v1/bar/wastage/w1/approve", "ROLE_FORBIDDEN"},
			dc{"store keeper issues a transfer", keeper, "POST", "/api/v1/bar/transfers/t1/issue", ""},
			dc{"store keeper cannot delete an item", keeper, "DELETE", "/api/v1/bar/items/p1", "ROLE_FORBIDDEN"},
			dc{"manager approves a count", manager, "POST", "/api/v1/bar/counts/c1/approve", ""},
			dc{"host has no bar stock", accessOf(t, "nightclub", true, all, nil, roleOf(t, "nightclub", "host")), "GET", "/api/v1/bar/items", "ROLE_FORBIDDEN"},
		)
	} else {
		t.Log("no plans rule for /bar yet (pkg/bar not merged) — /bar cases skipped")
	}
	for _, c := range cases {
		d := Decide(c.a, c.method, c.path, nil)
		if d.Code != c.code || d.Allowed != (c.code == "") {
			t.Errorf("%s: %s %s → allowed=%v code=%q, want %q", c.name, c.method, c.path, d.Allowed, d.Code, c.code)
		}
	}
}

// ---------------------------------------------------------------- database

// A new club starts confirmed with the wizard pending, the first admin as
// Owner and the hospitality plan bundle.
func TestNightclubNewOrg(t *testing.T) {
	db := testDB(t)
	if err := plans.Migrate(db); err != nil {
		t.Fatal(err)
	}
	f := seedOrg(t, db, "general")
	// Created after plans were installed → a new organisation (trial plan).
	db.Exec(`UPDATE organizations SET created_at = now() + interval '1 minute' WHERE id = ?`, f.org)
	svc := NewService(db)
	ps := plans.NewService(db, nil)
	svc.PlanModules = func(org string) (map[string]bool, error) {
		a, err := ps.AccessFor(org)
		return a.Modules, err
	}
	if err := svc.OnOrgCreated(f.org, "nightclub", "test"); err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Settings(f.org)
	if st.TypeKey != "nightclub" || !st.Confirmed || st.SetupStatus != SetupPending {
		t.Fatalf("settings %+v", st)
	}
	r, _ := svc.RoleFor(f.org, f.admin, "admin")
	if r == nil || r.Key != "owner" || r.TypeKey != "nightclub" {
		t.Fatalf("owner %+v", r)
	}
	sub, err := ps.Subscription(f.org)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Industry != "hospitality" {
		t.Fatalf("plan industry %s", sub.Industry)
	}
	a, _ := svc.AccessFor(f.org, f.admin, "admin")
	mods := a.EffectiveModules(ModuleKeys())
	for _, m := range []string{"pos", "inventory", "bookings", "hr"} {
		if !contains(mods, m) {
			t.Fatalf("new club lacks %s: %v (plan %v)", m, mods, a.Plan)
		}
	}
	if contains(mods, "invoicing") {
		t.Fatalf("invoicing must stay hidden for a club: %v", mods)
	}
}

// Switching an existing organisation to the club type and back: words,
// modules and roles follow the type; users keep a role of their level.
func TestNightclubSwitchAndBack(t *testing.T) {
	db := testDB(t)
	svc := NewService(db)
	svc.PlanModules = planAll
	f := seedOrg(t, db, "general")
	if _, err := MigrateLegacy(db); err != nil {
		t.Fatal(err)
	}
	custom := OrgRole{ID: uuid.NewString(), OrganizationID: f.org, Key: "custom-dj", Name: "DJ", Base: BaseStaff, Scope: ScopeAll, Perms: Perms{"events": view}, Custom: true}
	if err := db.Create(&custom).Error; err != nil {
		t.Fatal(err)
	}
	keyOf := func(user, legacy string) string {
		r, _ := svc.RoleFor(f.org, user, legacy)
		if r == nil {
			return ""
		}
		return r.Key
	}
	if keyOf(f.staff, "staff") != "staff" || keyOf(f.manager, "manager") != "manager" {
		t.Fatal("legacy general roles")
	}
	before, _ := svc.AccessFor(f.org, f.staff, "staff")
	if before.Restricted || !before.Can("invoicing", ActCreate) {
		t.Fatal("unconfirmed general org keeps every module")
	}

	// → club
	if _, err := svc.SetType(f.org, "nightclub", "test"); err != nil {
		t.Fatal(err)
	}
	if k := keyOf(f.admin, "admin"); k != "owner" {
		t.Fatalf("admin → %s", k)
	}
	if k := keyOf(f.manager, "manager"); k != "bar_manager" {
		t.Fatalf("manager → %s", k)
	}
	if k := keyOf(f.staff, "staff"); k != "bartender" {
		t.Fatalf("staff → %s", k)
	}
	var keys []string
	db.Model(&OrgRole{}).Where("organization_id = ?", f.org).Order("key").Pluck("key", &keys)
	if strings.Join(keys, ",") != "bar_manager,bartender,cashier,custom-dj,host,owner,store_keeper" {
		t.Fatalf("club roles %v", keys)
	}
	a, _ := svc.AccessFor(f.org, f.staff, "staff")
	if !a.Restricted || a.Can("invoicing", ActView) || !a.Can("pos", ActCreate) || a.Can(PermCore, ActView) {
		t.Fatalf("bartender access %+v", a)
	}
	if Terms(svc.Type("nightclub"))["inventory"] != "Bar stock" {
		t.Fatal("club words")
	}
	// The staff member is moved to the Store keeper; then the type is re-applied (idempotent).
	if _, _, err := svc.Assign(f.org, f.tech, "store_keeper", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetType(f.org, "nightclub", "test"); err != nil {
		t.Fatal(err)
	}
	if k := keyOf(f.tech, "staff"); k != "store_keeper" {
		t.Fatalf("re-applying the type kept %s", k)
	}
	var n int64
	db.Model(&OrgRole{}).Where("organization_id = ?", f.org).Count(&n)
	if n != 7 {
		t.Fatalf("re-applying duplicated roles: %d", n)
	}

	// ← back to general
	if _, err := svc.SetType(f.org, "general", "test"); err != nil {
		t.Fatal(err)
	}
	for user, want := range map[string]string{f.admin: "owner", f.manager: "manager", f.staff: "staff", f.tech: "staff"} {
		legacy := map[string]string{f.admin: "admin", f.manager: "manager"}[user]
		if legacy == "" {
			legacy = "staff"
		}
		if k := keyOf(user, legacy); k != want {
			t.Fatalf("back to general: %s → %s, want %s", legacy, k, want)
		}
	}
	keys = nil
	db.Model(&OrgRole{}).Where("organization_id = ?", f.org).Order("key").Pluck("key", &keys)
	if strings.Join(keys, ",") != "custom-dj,manager,owner,staff,viewer" {
		t.Fatalf("general roles %v", keys)
	}
	a, _ = svc.AccessFor(f.org, f.staff, "staff")
	if a.Restricted || !a.Can("invoicing", ActCreate) || a.TypeKey != "general" {
		t.Fatalf("general access %+v", a)
	}
	if Terms(svc.Type("general"))["inventory"] != "Inventory" {
		t.Fatal("general words")
	}

	// Another legacy general organisation was never touched.
	g := seedOrg(t, db, "general")
	MigrateLegacy(db)
	ga, _ := svc.AccessFor(g.org, g.staff, "staff")
	gs, _ := svc.Settings(g.org)
	if ga.TypeKey != "general" || ga.Restricted || gs.Confirmed || gs.SetupStatus != SetupLegacy {
		t.Fatalf("legacy org %+v %+v", ga, gs)
	}
}

// Legacy "bar" organisations map to the club type but keep every module
// until an admin confirms it.
func TestLegacyBarOrgUnrestricted(t *testing.T) {
	db := testDB(t)
	svc := NewService(db)
	svc.PlanModules = planAll
	f := seedOrg(t, db, "bar")
	if _, err := MigrateLegacy(db); err != nil {
		t.Fatal(err)
	}
	a, _ := svc.AccessFor(f.org, f.admin, "admin")
	if a.TypeKey != "nightclub" || a.Restricted || !a.ModuleOn("invoicing") || !a.Can("accounting", ActEdit) {
		t.Fatalf("legacy bar org %+v", a)
	}
	if r, _ := svc.RoleFor(f.org, f.staff, "staff"); r == nil || r.Key != "bartender" {
		t.Fatalf("legacy bar staff → %+v", r)
	}
	// Seeding is additive: the type exists in an existing registry.
	var n int64
	db.Model(&BusinessType{}).Where("key = ?", "nightclub").Count(&n)
	if n != 1 {
		t.Fatal("nightclub not seeded")
	}
}
