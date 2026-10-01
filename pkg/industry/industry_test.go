package industry

import (
	"encoding/json"
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

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/plans"
)

// ---------------------------------------------------------------- pure

func typeByKey(t *testing.T, key string) BusinessType {
	for _, bt := range DefaultTypes() {
		if bt.Key == key {
			return bt
		}
	}
	t.Fatalf("no type %s", key)
	return BusinessType{}
}

func roleOf(t *testing.T, typeKey, roleKey string) *OrgRole {
	bt := typeByKey(t, typeKey)
	for i, rt := range bt.Roles {
		if rt.Key == roleKey {
			r := templateRole("org", typeKey, rt, i)
			return &r
		}
	}
	t.Fatalf("no role %s/%s", typeKey, roleKey)
	return nil
}

func accessOf(t *testing.T, typeKey string, confirmed bool, plan []string, extras []string, role *OrgRole) Access {
	bt := typeByKey(t, typeKey)
	a := Access{TypeKey: typeKey, TypeMods: map[string]bool{}, Plan: map[string]bool{"core": true}, Role: role, UserID: "u1"}
	all := false
	for _, m := range bt.Modules {
		if m == AllModules {
			all = true
		}
		a.TypeMods[m] = true
	}
	for _, m := range extras {
		a.TypeMods[m] = true
	}
	for _, m := range plan {
		a.Plan[m] = true
	}
	a.Restricted = confirmed && !all
	return a
}

var everyModule = []string{"smallbiz", "invoicing", "pos", "ecommerce", "crm", "bookings", "events", "inventory", "manufacturing", "accounting", "hr", "projects", "documents", "communication"}

func TestDefaultTypesAreValid(t *testing.T) {
	want := []string{"mechanic", "salon", "spa", "clinic", "ndis", "consulting", "agency", "retail", "restaurant", "events", "construction", "manufacturer", "education", "nonprofit", "general"}
	types := DefaultTypes()
	if len(types) != len(want) {
		t.Fatalf("want %d types, got %d", len(want), len(types))
	}
	for i, k := range want {
		bt := types[i]
		if bt.Key != k {
			t.Fatalf("type %d: want %s got %s", i, k, bt.Key)
		}
		if msg := validateType(&bt); msg != "" {
			t.Fatalf("%s: %s", k, msg)
		}
		legacy := map[string]bool{}
		for _, r := range bt.Roles {
			if r.Legacy != "" {
				legacy[r.Legacy] = true
			}
		}
		for _, l := range []string{"admin", "manager", "staff"} {
			if !legacy[l] {
				t.Fatalf("%s: no role for legacy %s", k, l)
			}
		}
	}
}

// Precedence matrix: plan ∩ type (∪ extras) → role → super admin.
func TestPrecedenceMatrix(t *testing.T) {
	sme := []string{"bookings", "invoicing", "inventory", "crm", "pos", "events", "hr"} // plan includes events and hr
	tech := roleOf(t, "mechanic", "technician")
	owner := roleOf(t, "mechanic", "owner")
	advisor := roleOf(t, "mechanic", "service_advisor")
	cases := []struct {
		name   string
		a      Access
		module string
		act    string
		want   bool
	}{
		{"owner: in plan and type", accessOf(t, "mechanic", true, sme, nil, owner), "invoicing", ActDelete, true},
		{"owner: in plan, not in type → hidden", accessOf(t, "mechanic", true, sme, nil, owner), "events", ActView, false},
		{"owner: in type, not in plan → off", accessOf(t, "mechanic", true, []string{"bookings"}, nil, owner), "invoicing", ActView, false},
		{"owner: extra module enabled by admin", accessOf(t, "mechanic", true, sme, []string{"events"}, owner), "events", ActView, true},
		{"owner: extra module but not in plan", accessOf(t, "mechanic", true, sme, []string{"projects"}, owner), "projects", ActView, false},
		{"unconfirmed type keeps every plan module", accessOf(t, "mechanic", false, sme, nil, owner), "events", ActView, true},
		{"general keeps every plan module", accessOf(t, "general", true, sme, nil, roleOf(t, "general", "staff")), "events", ActEdit, true},
		{"general staff cannot delete", accessOf(t, "general", true, sme, nil, roleOf(t, "general", "staff")), "events", ActDelete, false},
		{"general read-only can view", accessOf(t, "general", true, sme, nil, roleOf(t, "general", "viewer")), "crm", ActView, true},
		{"general read-only cannot create", accessOf(t, "general", true, sme, nil, roleOf(t, "general", "viewer")), "crm", ActCreate, false},
		{"technician views job cards", accessOf(t, "mechanic", true, sme, nil, tech), "bookings", ActView, true},
		{"technician updates job cards", accessOf(t, "mechanic", true, sme, nil, tech), "bookings", ActEdit, true},
		{"technician cannot delete job cards", accessOf(t, "mechanic", true, sme, nil, tech), "bookings", ActDelete, false},
		{"technician cannot invoice", accessOf(t, "mechanic", true, sme, nil, tech), "invoicing", ActCreate, false},
		{"technician views parts", accessOf(t, "mechanic", true, sme, nil, tech), "inventory", ActView, true},
		{"technician cannot edit parts", accessOf(t, "mechanic", true, sme, nil, tech), "inventory", ActEdit, false},
		{"advisor invoices", accessOf(t, "mechanic", true, sme, nil, advisor), "invoicing", ActCreate, true},
		{"role cannot reach a module the type hides", accessOf(t, "mechanic", true, sme, nil, roleOf(t, "mechanic", "workshop_manager")), "events", ActView, false},
		{"super admin bypasses", Access{SuperAdmin: true, Restricted: true, TypeMods: map[string]bool{}}, "events", ActDelete, true},
	}
	for _, c := range cases {
		if got := c.a.Can(c.module, c.act); got != c.want {
			t.Errorf("%s: Can(%s,%s)=%v want %v", c.name, c.module, c.act, got, c.want)
		}
	}
}

func TestDecideCodes(t *testing.T) {
	sme := []string{"bookings", "invoicing", "inventory", "crm", "pos", "events"}
	tech := accessOf(t, "mechanic", true, sme, nil, roleOf(t, "mechanic", "technician"))
	cases := []struct {
		a      Access
		method string
		path   string
		code   string
		own    bool
	}{
		{tech, "GET", "/api/v1/events/abc", "MODULE_HIDDEN_FOR_TYPE", false},
		{tech, "POST", "/api/v1/invoicing/invoices", "ROLE_FORBIDDEN", false},
		{tech, "DELETE", "/api/v1/bookings/b1", "ROLE_FORBIDDEN", false},
		{tech, "GET", "/api/v1/bookings", "", true},
		{tech, "PATCH", "/api/v1/bookings/b1/status", "", true},
		{tech, "GET", "/api/v1/inventory/products", "", false},
		{tech, "GET", "/api/v1/customers", "", false}, // shared look-up (read any)
		{tech, "POST", "/api/v1/customers", "ROLE_FORBIDDEN", false},
		{tech, "GET", "/api/v1/users", "", false}, // not a module route
		{accessOf(t, "mechanic", true, sme, nil, roleOf(t, "mechanic", "owner")), "DELETE", "/api/v1/bookings/b1", "", false},
	}
	for _, c := range cases {
		d := Decide(c.a, c.method, c.path, nil)
		if d.Code != c.code || d.Allowed != (c.code == "") || d.OwnScope != c.own {
			t.Errorf("%s %s: got allowed=%v code=%q own=%v, want code=%q own=%v", c.method, c.path, d.Allowed, d.Code, d.OwnScope, c.code, c.own)
		}
	}
	if ActionFor("POST", "/finance/bills/x/approve") != ActApprove || ActionFor("POST", "/invoicing/invoices/x/void") != ActApprove || ActionFor("PATCH", "/x") != ActEdit {
		t.Fatal("ActionFor")
	}
}

func TestLegacyRoleMapping(t *testing.T) {
	for _, typeKey := range []string{"general", "mechanic", "salon", "clinic", "ndis"} {
		bt := typeByKey(t, typeKey)
		roles := []OrgRole{}
		for i, rt := range bt.Roles {
			roles = append(roles, templateRole("o", typeKey, rt, i))
		}
		for legacy, base := range map[string]string{"admin": BaseAdmin, "manager": BaseManager, "staff": "", "care_worker": "", "support_coordinator": ""} {
			r := legacyRole(roles, legacy)
			if r == nil {
				t.Fatalf("%s: no role for %s", typeKey, legacy)
			}
			if base != "" && r.Base != base {
				t.Fatalf("%s: %s maps to %s (%s)", typeKey, legacy, r.Key, r.Base)
			}
			if base == "" && (r.Base == BaseAdmin || r.Base == BaseManager) {
				t.Fatalf("%s: %s must not map to %s", typeKey, legacy, r.Key)
			}
		}
	}
	gen := typeByKey(t, "general")
	roles := []OrgRole{}
	for i, rt := range gen.Roles {
		roles = append(roles, templateRole("o", "general", rt, i))
	}
	if r := legacyRole(roles, "admin"); r.Key != "owner" {
		t.Fatal("admin → owner")
	}
	if r := legacyRole(roles, "manager"); r.Key != "manager" {
		t.Fatal("manager → manager")
	}
	if r := legacyRole(roles, "staff"); r.Key != "staff" {
		t.Fatal("staff → staff")
	}
	// A demoted admin must not keep the owner role.
	if consistent(&roles[0], "staff") {
		t.Fatal("owner role must not stay with a staff user")
	}
}

// ---------------------------------------------------------------- database

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	// Each test gets its own empty schema, so tables left by a full app
	// migration in the same database (with stricter columns) never interfere.
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	schema := "industry_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if sqlDB, err := admin.DB(); err == nil {
			sqlDB.Close()
		}
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := db.DB(); err == nil {
		t.Cleanup(func() { sqlDB.Close() })
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS organizations (id varchar(255) PRIMARY KEY, name varchar(255), business_type varchar(100) DEFAULT 'general', created_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE IF NOT EXISTS users (id varchar(255) PRIMARY KEY, email varchar(255), role varchar(50), organization_id varchar(255), deleted_at timestamptz)`,
		`CREATE TABLE IF NOT EXISTS bookings (id varchar(255) PRIMARY KEY, organization_id varchar(255), staff_id varchar(255), deleted_at timestamptz)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

type fixture struct {
	org, admin, manager, staff, tech string
}

func seedOrg(t *testing.T, db *gorm.DB, businessType string) fixture {
	f := fixture{org: uuid.NewString(), admin: uuid.NewString(), manager: uuid.NewString(), staff: uuid.NewString(), tech: uuid.NewString()}
	db.Exec(`INSERT INTO organizations (id, name, business_type, created_at) VALUES (?, ?, ?, now())`, f.org, "Test "+businessType, businessType)
	for id, role := range map[string]string{f.admin: "admin", f.manager: "manager", f.staff: "staff", f.tech: "staff"} {
		db.Exec(`INSERT INTO users (id, email, role, organization_id) VALUES (?, ?, ?, ?)`, id, id+"@t.test", role, f.org)
	}
	return f
}

func TestLegacyMigrationIdempotent(t *testing.T) {
	db := testDB(t)
	f := seedOrg(t, db, "general")
	if _, err := MigrateLegacy(db); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...interface{}) int64 {
		var n int64
		db.Raw(q, args...).Scan(&n)
		return n
	}
	roles1 := count(`SELECT count(*) FROM industry_org_roles WHERE organization_id = ?`, f.org)
	users1 := count(`SELECT count(*) FROM industry_user_roles WHERE organization_id = ?`, f.org)
	if roles1 != 4 || users1 != 4 {
		t.Fatalf("first run: %d roles, %d assignments", roles1, users1)
	}
	rep, err := MigrateLegacy(db)
	if err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM industry_org_roles WHERE organization_id = ?`, f.org) != roles1 || count(`SELECT count(*) FROM industry_user_roles WHERE organization_id = ?`, f.org) != users1 {
		t.Fatal("second run changed rows")
	}
	_ = rep
	svc := NewService(db)
	st, _ := svc.Settings(f.org)
	if st.TypeKey != "general" || st.Confirmed || st.SetupStatus != SetupLegacy {
		t.Fatalf("settings: %+v", st)
	}
	r, _ := svc.RoleFor(f.org, f.admin, "admin")
	if r == nil || r.Key != "owner" {
		t.Fatalf("admin → %+v", r)
	}
	r, _ = svc.RoleFor(f.org, f.manager, "manager")
	if r == nil || r.Key != "manager" {
		t.Fatalf("manager → %+v", r)
	}
	r, _ = svc.RoleFor(f.org, f.staff, "staff")
	if r == nil || r.Key != "staff" {
		t.Fatalf("staff → %+v", r)
	}
	// A legacy "garage" organisation maps to mechanic but stays unrestricted.
	g := seedOrg(t, db, "garage")
	MigrateLegacy(db)
	a, _ := svc.AccessFor(g.org, g.admin, "admin")
	if a.TypeKey != "mechanic" || a.Restricted {
		t.Fatalf("legacy garage: %+v", a)
	}
}

func planAll(org string) (map[string]bool, error) {
	m := map[string]bool{"core": true}
	for _, k := range everyModule {
		m[k] = true
	}
	return m, nil
}

func router(t *testing.T, svc *Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	e := &plans.Enforcer{
		Lookup: func(org string) (plans.Access, error) {
			m, _ := planAll(org)
			return plans.Access{Modules: m}, nil
		},
		Identify: func(c *gin.Context) (plans.Identity, bool) {
			return plans.Identity{Org: c.GetHeader("X-Org"), Role: c.GetHeader("X-Role"), User: c.GetHeader("X-User")}, true
		},
		After: (&Guard{Svc: svc}).Check,
	}
	r.Use(e.Middleware())
	echo := func(c *gin.Context) { c.JSON(200, gin.H{"staff_id": c.Query("staff_id"), "path": c.Request.URL.Path}) }
	for _, p := range []string{"/api/v1/bookings", "/api/v1/bookings/:id", "/api/v1/bookings/:id/status", "/api/v1/events", "/api/v1/invoicing/invoices", "/api/v1/inventory/products"} {
		r.GET(p, echo)
		r.POST(p, echo)
		r.PUT(p, echo)
		r.PATCH(p, echo)
		r.DELETE(p, echo)
	}
	return r
}

func call(r *gin.Engine, method, path string, f fixture, user, role string) (int, map[string]interface{}) {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("X-Org", f.org)
	req.Header.Set("X-User", user)
	req.Header.Set("X-Role", role)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func errCode(body map[string]interface{}) string {
	if e, ok := body["error"].(map[string]interface{}); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func TestMiddlewareAndOwnScope(t *testing.T) {
	db := testDB(t)
	svc := NewService(db)
	svc.PlanModules = planAll
	f := seedOrg(t, db, "general")
	if err := svc.OnOrgCreated(f.org, "mechanic", "test"); err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Settings(f.org)
	if st.TypeKey != "mechanic" || !st.Confirmed || st.SetupStatus != SetupPending {
		t.Fatalf("new mechanic org: %+v", st)
	}
	if _, _, err := svc.Assign(f.org, f.tech, "technician", "test"); err != nil {
		t.Fatal(err)
	}
	mine, theirs := uuid.NewString(), uuid.NewString()
	db.Exec(`INSERT INTO bookings (id, organization_id, staff_id) VALUES (?, ?, ?), (?, ?, ?)`, mine, f.org, f.tech, theirs, f.org, f.staff)
	r := router(t, svc)

	// Owner: everything in the mechanic type; events hidden by the type.
	if code, _ := call(r, "DELETE", "/api/v1/bookings/"+theirs, f, f.admin, "admin"); code != 200 {
		t.Fatalf("owner delete booking: %d", code)
	}
	if code, body := call(r, "GET", "/api/v1/events", f, f.admin, "admin"); code != 403 || errCode(body) != "MODULE_HIDDEN_FOR_TYPE" {
		t.Fatalf("events for mechanic: %d %v", code, body)
	}
	// Admin enables events as an extra module.
	svc.SetExtras(f.org, []string{"events"}, "test")
	if code, _ := call(r, "GET", "/api/v1/events", f, f.admin, "admin"); code != 200 {
		t.Fatalf("events after extras: %d", code)
	}
	// Technician.
	if code, body := call(r, "POST", "/api/v1/invoicing/invoices", f, f.tech, "staff"); code != 403 || errCode(body) != "ROLE_FORBIDDEN" {
		t.Fatalf("technician invoice: %d %v", code, body)
	}
	if code, body := call(r, "DELETE", "/api/v1/bookings/"+mine, f, f.tech, "staff"); code != 403 || errCode(body) != "ROLE_FORBIDDEN" {
		t.Fatalf("technician delete: %d %v", code, body)
	}
	code, body := call(r, "GET", "/api/v1/bookings?staff_id=someone-else", f, f.tech, "staff")
	if code != 200 || body["staff_id"] != f.tech {
		t.Fatalf("technician list must be forced to own: %d %v", code, body)
	}
	if code, _ := call(r, "GET", "/api/v1/bookings/"+mine, f, f.tech, "staff"); code != 200 {
		t.Fatalf("technician own booking: %d", code)
	}
	if code, _ := call(r, "PATCH", "/api/v1/bookings/"+mine+"/status", f, f.tech, "staff"); code != 200 {
		t.Fatalf("technician update own status: %d", code)
	}
	if code, body := call(r, "GET", "/api/v1/bookings/"+theirs, f, f.tech, "staff"); code != 403 || errCode(body) != "ROLE_FORBIDDEN" {
		t.Fatalf("technician other booking: %d %v", code, body)
	}
	if code, body := call(r, "PUT", "/api/v1/bookings/"+theirs, f, f.tech, "staff"); code != 403 || errCode(body) != "ROLE_FORBIDDEN" {
		t.Fatalf("technician edit other booking: %d %v", code, body)
	}
	if code, _ := call(r, "GET", "/api/v1/inventory/products", f, f.tech, "staff"); code != 200 {
		t.Fatalf("technician parts: %d", code)
	}
	// Owner list is not filtered.
	if _, body := call(r, "GET", "/api/v1/bookings", f, f.admin, "admin"); body["staff_id"] != "" {
		t.Fatalf("owner list filtered: %v", body)
	}
	// Super admin bypasses.
	if code, _ := call(r, "DELETE", "/api/v1/bookings/"+mine, f, "sa", "super_admin"); code != 200 {
		t.Fatalf("super admin: %d", code)
	}
	// A demoted technician (legacy role changed to admin elsewhere) is not
	// stuck with the old template: users.role wins when inconsistent.
	ra, _ := svc.RoleFor(f.org, f.tech, "admin")
	if ra == nil || ra.Key != "owner" {
		t.Fatalf("inconsistent assignment: %+v", ra)
	}

	// Switching to salon: terms and roles follow, technician → receptionist (legacy staff).
	if _, err := svc.SetType(f.org, "salon", "test"); err != nil {
		t.Fatal(err)
	}
	rt, _ := svc.RoleFor(f.org, f.tech, "staff")
	if rt == nil || rt.TypeKey != "salon" {
		t.Fatalf("after salon switch: %+v", rt)
	}
	var n int64
	db.Model(&OrgRole{}).Where("organization_id = ? AND key = ?", f.org, "technician").Count(&n)
	if n != 0 {
		t.Fatal("mechanic-only roles should be removed")
	}
	if Terms(svc.Type("salon"))["bookings"] != "Appointments" {
		t.Fatal("salon terms")
	}
	_ = time.Now
}

func TestGeneralOrgUnaffected(t *testing.T) {
	db := testDB(t)
	svc := NewService(db)
	svc.PlanModules = planAll
	f := seedOrg(t, db, "general")
	r := router(t, svc)
	for _, p := range []string{"/api/v1/events", "/api/v1/invoicing/invoices", "/api/v1/bookings", "/api/v1/inventory/products"} {
		for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
			if code, _ := call(r, m, p, f, f.admin, "admin"); code != 200 {
				t.Fatalf("general admin %s %s: %d", m, p, code)
			}
			if code, _ := call(r, m, p, f, f.manager, "manager"); code != 200 {
				t.Fatalf("general manager %s %s: %d", m, p, code)
			}
		}
		if code, body := call(r, "GET", p, f, f.staff, "staff"); code != 200 || body["staff_id"] != "" {
			t.Fatalf("general staff GET %s: %d %v", p, code, body)
		}
	}
}
