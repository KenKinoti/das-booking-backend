package onboarding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orghooks"
)

const secret = "onboarding-test-secret"

// schemaDDL creates the columns the detectors read (a cut-down copy of the
// app's tables; TestDetectorsOnRealSchema checks them against the real one).
var schemaDDL = []string{
	`CREATE TABLE organizations (id varchar(255) PRIMARY KEY, name varchar(255), business_type varchar(100) DEFAULT 'general', phone varchar(50), address_street varchar(255), address_suburb varchar(255), created_at timestamptz, deleted_at timestamptz)`,
	`CREATE TABLE users (id varchar(255) PRIMARY KEY, email varchar(255), first_name varchar(100), phone varchar(50), role varchar(50), organization_id varchar(255), last_login_at timestamptz, created_at timestamptz DEFAULT now(), deleted_at timestamptz)`,
	`CREATE TABLE bookings (id varchar(255) PRIMARY KEY, organization_id varchar(255), staff_id varchar(255), status varchar(30), deleted_at timestamptz)`,
	`CREATE TABLE customers (id varchar(255) PRIMARY KEY, organization_id varchar(255), deleted_at timestamptz)`,
	`CREATE TABLE billing_invoices (id varchar(255) PRIMARY KEY, organization_id varchar(255), doc_type varchar(20), created_by varchar(255), deleted_at timestamptz)`,
	`CREATE TABLE billing_payments (id varchar(255) PRIMARY KEY, organization_id varchar(255))`,
	`CREATE TABLE bills (id varchar(255) PRIMARY KEY, organization_id varchar(255))`,
	`CREATE TABLE pos_transactions (id varchar(255) PRIMARY KEY, organization_id varchar(255), cashier_id varchar(255), status varchar(30), deleted_at timestamptz)`,
	`CREATE TABLE cash_drawers (id varchar(255) PRIMARY KEY, organization_id varchar(255), opened_by varchar(255), deleted_at timestamptz)`,
	`CREATE TABLE services (id varchar(255) PRIMARY KEY, organization_id varchar(255), deleted_at timestamptz)`,
	`CREATE TABLE products (id varchar(255) PRIMARY KEY, organization_id varchar(255), selling_price numeric, deleted_at timestamptz)`,
	`CREATE TABLE suppliers (id varchar(255) PRIMARY KEY, organization_id varchar(255), deleted_at timestamptz)`,
	`CREATE TABLE purchase_orders (id varchar(255) PRIMARY KEY, organization_id varchar(255), deleted_at timestamptz)`,
	`CREATE TABLE mrp_work_orders (id varchar(255) PRIMARY KEY, org_id varchar(255), started_at timestamptz, completed_at timestamptz)`,
	`CREATE TABLE events (id varchar(255) PRIMARY KEY, organization_id varchar(255), deleted_at timestamptz)`,
	`CREATE TABLE event_registrations (id varchar(255) PRIMARY KEY, organization_id varchar(255), checked_in_by varchar(255), deleted_at timestamptz)`,
	`CREATE TABLE pm_projects (id varchar(255) PRIMARY KEY, org_id varchar(255))`,
	`CREATE TABLE pm_tasks (id varchar(255) PRIMARY KEY, org_id varchar(255))`,
	`CREATE TABLE bar_recipes (pos_product_id varchar(255) PRIMARY KEY, organization_id varchar(255))`,
	`CREATE TABLE bar_deliveries (id varchar(255) PRIMARY KEY, organization_id varchar(255), created_by varchar(255))`,
	`CREATE TABLE bar_counts (id varchar(255) PRIMARY KEY, organization_id varchar(255), status varchar(10), created_by varchar(255), submitted_by varchar(255))`,
	`CREATE TABLE bar_transfers (id varchar(255) PRIMARY KEY, organization_id varchar(255), requested_by varchar(255), issued_by varchar(255))`,
	`CREATE TABLE bar_wastage (id varchar(255) PRIMARY KEY, organization_id varchar(255), recorded_by varchar(255), decided_by varchar(255))`,
	`CREATE TABLE org_logos (organization_id varchar(255) PRIMARY KEY)`,
	`CREATE TABLE organization_brandings (id varchar(255) PRIMARY KEY, organization_id varchar(255), logo_url text)`,
}

func openSchema(t *testing.T, ddl bool) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	schema := "onboarding_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	if ddl {
		for _, s := range schemaDDL {
			if err := db.Exec(s).Error; err != nil {
				t.Fatal(s, err)
			}
		}
	}
	return db
}

type env struct {
	t   *testing.T
	db  *gorm.DB
	ind *industry.Service
	svc *Service
	r   *gin.Engine
}

func newEnv(t *testing.T) *env {
	db := openSchema(t, true)
	if err := industry.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	ind := industry.NewService(db)
	ind.TTL = 0
	svc := NewService(db, ind)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	auth := r.Group("/api/v1", func(c *gin.Context) {
		// what internal/middleware.AuthRequired puts in the context
		tok, err := jwt.Parse(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "), func(*jwt.Token) (interface{}, error) { return []byte(secret), nil })
		if err != nil || !tok.Valid {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		cl := tok.Claims.(jwt.MapClaims)
		c.Set("user_id", cl["user_id"])
		c.Set("user_role", cl["role"])
		c.Set("org_id", cl["org_id"])
	})
	(&Handler{Svc: svc, JWTSecret: secret}).RegisterRoutes(auth)
	return &env{t: t, db: db, ind: ind, svc: svc, r: r}
}

func (e *env) exec(sql string, args ...interface{}) {
	e.t.Helper()
	if err := e.db.Exec(sql, args...).Error; err != nil {
		e.t.Fatal(sql, err)
	}
}

// newOrg creates an organisation of type typ with an owner (via the
// industry service, like the platform's "new organisation") and staff.
func (e *env) newOrg(typ, name string, staff map[string]string) (string, string, map[string]string) {
	e.t.Helper()
	org, owner := uuid.NewString(), uuid.NewString()
	e.exec(`INSERT INTO organizations (id, name, business_type, created_at) VALUES (?, ?, ?, now())`, org, name, typ)
	e.exec(`INSERT INTO users (id, email, first_name, role, organization_id, created_at) VALUES (?, ?, 'Wanjiru', 'admin', ?, now() - interval '1 hour')`, owner, owner+"@t.test", org)
	if err := e.ind.OnOrgCreated(org, typ, "test"); err != nil {
		e.t.Fatal(err)
	}
	ids := map[string]string{}
	for first, roleKey := range staff {
		id := uuid.NewString()
		e.exec(`INSERT INTO users (id, email, first_name, role, organization_id) VALUES (?, ?, ?, 'staff', ?)`, id, id+"@t.test", first, org)
		if _, _, err := e.ind.Assign(org, id, roleKey, owner); err != nil {
			e.t.Fatal(roleKey, err)
		}
		ids[first] = id
	}
	return org, owner, ids
}

func token(user, role, org string, impersonating bool) string {
	cl := jwt.MapClaims{"user_id": user, "role": role, "org_id": org, "exp": time.Now().Add(time.Hour).Unix()}
	if impersonating {
		cl["impersonating"] = true
	}
	s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, cl).SignedString([]byte(secret))
	return s
}

type resp struct {
	Success bool `json:"success"`
	Data    View `json:"data"`
	Preview bool `json:"preview"`
	Error   struct {
		Code string `json:"code"`
	} `json:"error"`
	status int
}

func (e *env) call(method, path, tok string, body interface{}) resp {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	var out resp
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	out.status = w.Code
	return out
}

func stepOf(v View, key string) (StepView, bool) {
	for _, s := range v.Steps {
		if s.Key == key {
			return s, true
		}
	}
	return StepView{}, false
}

func keysOf(v View) string {
	out := []string{}
	for _, s := range v.Steps {
		out = append(out, s.Key)
	}
	return strings.Join(out, ",")
}

// Every detector query runs (on the cut-down schema) and is false for an
// empty organisation.
func TestDetectorsRunAndStartFalse(t *testing.T) {
	e := newEnv(t)
	org, owner, _ := e.newOrg("nightclub", "Club Habari", nil)
	e.exec(`UPDATE industry_org_settings SET setup_status = 'pending' WHERE organization_id = ?`, org)
	for key := range detectors {
		var yes bool
		for _, sql := range detectors[key].queries {
			if err := e.db.Raw(sql, map[string]interface{}{"org": org, "user": owner}).Scan(&yes).Error; err != nil {
				t.Errorf("%s: %v", key, err)
			}
		}
		if key == "type_chosen" {
			if !Detect(e.db, key, org, owner) {
				t.Errorf("type_chosen: a nightclub created with its type is confirmed")
			}
			continue
		}
		if Detect(e.db, key, org, owner) {
			t.Errorf("%s is true for an empty organisation", key)
		}
	}
	if Detect(e.db, "nope", org, owner) || Detect(e.db, "customer", "", owner) {
		t.Fatal("unknown key / no org")
	}
	if !UserScoped("my_pos_sale") || UserScoped("pos_sale") {
		t.Fatal("UserScoped")
	}
}

// The detector SQL matches the real application schema (run with
// REAL_DATABASE_URL pointing at a migrated DASYIN database).
func TestDetectorsOnRealSchema(t *testing.T) {
	dsn := os.Getenv("REAL_DATABASE_URL")
	if dsn == "" {
		t.Skip("REAL_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s, err := db.DB(); err == nil {
			s.Close()
		}
	}()
	for key, d := range detectors {
		for _, sql := range d.queries {
			var yes bool
			if err := db.Raw(sql, map[string]interface{}{"org": "00000000-0000-0000-0000-000000000000", "user": "nobody"}).Scan(&yes).Error; err != nil {
				t.Errorf("%s: %v", key, err)
			}
		}
	}
}

// Auto-detection ticks steps from cheap org/user-scoped checks.
func TestAutoDetection(t *testing.T) {
	e := newEnv(t)
	org, owner, staff := e.newOrg("nightclub", "Club Habari", map[string]string{"Brian": "bartender", "Aisha": "store_keeper"})
	other, otherOwner, otherStaff := e.newOrg("nightclub", "Other club", map[string]string{"Otto": "bartender"})
	brian, aisha := staff["Brian"], staff["Aisha"]
	bt := token(brian, "staff", org, false)

	v := e.call("GET", "/onboarding/me", bt, nil).Data
	if v.RoleKey != "bartender" || v.RoleName != "Bartender" || v.TypeKey != "nightclub" || v.FirstName != "Brian" || v.OrgName != "Club Habari" {
		t.Fatalf("bartender view %+v", v)
	}
	if keysOf(v) != "bar,sale,request,count,wastage" || v.DoneCount != 0 || v.Welcomed || v.Dismissed {
		t.Fatalf("bartender steps %s done %d", keysOf(v), v.DoneCount)
	}
	if s, _ := stepOf(v, "sale"); !s.Auto || s.Done || s.Title != "Ring up a tot, a bottle and a cocktail" || s.To != "/pos" {
		t.Fatalf("sale step %+v", s)
	}
	if s, _ := stepOf(v, "bar"); s.Auto {
		t.Fatalf("picking a bar is ticked by the person: %+v", s)
	}

	// Someone else's sale (other org, or another person here) does not count.
	e.exec(`INSERT INTO pos_transactions (id, organization_id, cashier_id, status) VALUES (?, ?, ?, 'completed')`, uuid.NewString(), other, otherStaff["Otto"])
	e.exec(`INSERT INTO pos_transactions (id, organization_id, cashier_id, status) VALUES (?, ?, ?, 'completed')`, uuid.NewString(), org, aisha)
	e.exec(`INSERT INTO pos_transactions (id, organization_id, cashier_id, status) VALUES (?, ?, ?, 'voided')`, uuid.NewString(), org, brian)
	if s, _ := stepOf(e.call("GET", "/onboarding/me", bt, nil).Data, "sale"); s.Done {
		t.Fatal("sale ticked by someone else's sale")
	}
	e.exec(`INSERT INTO pos_transactions (id, organization_id, cashier_id, status) VALUES (?, ?, ?, 'completed')`, uuid.NewString(), org, brian)
	e.exec(`INSERT INTO bar_transfers (id, organization_id, requested_by) VALUES (?, ?, ?)`, uuid.NewString(), org, brian)
	e.exec(`INSERT INTO bar_wastage (id, organization_id, recorded_by) VALUES (?, ?, ?)`, uuid.NewString(), org, brian)
	v = e.call("GET", "/onboarding/me", bt, nil).Data
	for k, want := range map[string]bool{"sale": true, "request": true, "wastage": true, "count": false, "bar": false} {
		if s, _ := stepOf(v, k); s.Done != want {
			t.Errorf("bartender %s done=%v want %v", k, s.Done, want)
		}
	}
	if v.DoneCount != 3 || v.Percent != 60 || v.AllDone {
		t.Fatalf("progress %d %d%% %v", v.DoneCount, v.Percent, v.AllDone)
	}

	// Store keeper: delivery, issuing a request, counting.
	kt := token(aisha, "staff", org, false)
	if keysOf(e.call("GET", "/onboarding/me", kt, nil).Data) != "delivery,issue,count,levels" {
		t.Fatal("store keeper steps")
	}
	e.exec(`INSERT INTO bar_deliveries (id, organization_id, created_by) VALUES (?, ?, ?)`, uuid.NewString(), org, aisha)
	e.exec(`UPDATE bar_transfers SET issued_by = ? WHERE organization_id = ?`, aisha, org)
	e.exec(`INSERT INTO bar_counts (id, organization_id, status, created_by) VALUES (?, ?, 'submitted', ?)`, uuid.NewString(), org, aisha)
	v = e.call("GET", "/onboarding/me", kt, nil).Data
	if v.DoneCount != 3 {
		t.Fatalf("store keeper done %d: %+v", v.DoneCount, v.Steps)
	}

	// Owner: setup, staff, delivery, an approved count; prices once every menu item has one.
	ot := token(owner, "admin", org, false)
	v = e.call("GET", "/onboarding/me", ot, nil).Data
	if keysOf(v) != "setup,prices,team,delivery,count,variance" || v.RoleKey != "owner" || v.RoleName != "Owner" {
		t.Fatalf("owner %s %s", keysOf(v), v.RoleKey)
	}
	done := map[string]bool{}
	for _, s := range v.Steps {
		done[s.Key] = s.Done
	}
	if done["setup"] || !done["team"] || !done["delivery"] || done["count"] || done["prices"] {
		t.Fatalf("owner done %v", done)
	}
	e.exec(`UPDATE industry_org_settings SET setup_status = 'done' WHERE organization_id = ?`, org)
	e.exec(`UPDATE bar_counts SET status = 'approved' WHERE organization_id = ?`, org)
	p1, p2 := uuid.NewString(), uuid.NewString()
	e.exec(`INSERT INTO products (id, organization_id, selling_price) VALUES (?, ?, 250), (?, ?, 0)`, p1, org, p2, org)
	e.exec(`INSERT INTO bar_recipes (pos_product_id, organization_id) VALUES (?, ?), (?, ?)`, p1, org, p2, org)
	e.ind.Invalidate(org)
	v = e.call("GET", "/onboarding/me", ot, nil).Data
	for _, s := range v.Steps {
		done[s.Key] = s.Done
	}
	if !done["setup"] || !done["count"] || done["prices"] {
		t.Fatalf("owner after setup/count %v", done)
	}
	e.exec(`UPDATE products SET selling_price = 300 WHERE id = ?`, p2)
	if s, _ := stepOf(e.call("GET", "/onboarding/me", ot, nil).Data, "prices"); !s.Done {
		t.Fatal("every menu item priced → prices done")
	}
	// The other club's owner is untouched by all of this (only "team": Otto).
	ov := e.call("GET", "/onboarding/me", token(otherOwner, "admin", other, false), nil).Data
	if s, _ := stepOf(ov, "team"); ov.DoneCount != 1 || !s.Done {
		t.Fatalf("other club owner has %d done: %+v", ov.DoneCount, ov.Steps)
	}

	// General business owner: logo / details / customers / invoices / staff.
	gorg, gowner, gstaff := e.newOrg("general", "Mama Mboga", map[string]string{"Sam": "staff"})
	gt := token(gowner, "admin", gorg, false)
	gv := e.call("GET", "/onboarding/me", gt, nil).Data
	if keysOf(gv) != "type,details,logo,customer,invoice,team" || gv.DoneCount != 1 { // team: Sam
		t.Fatalf("general owner %s done %d", keysOf(gv), gv.DoneCount)
	}
	e.exec(`INSERT INTO org_logos (organization_id) VALUES (?)`, gorg)
	e.exec(`UPDATE organizations SET phone = '+254700000000' WHERE id = ?`, gorg)
	e.exec(`INSERT INTO customers (id, organization_id) VALUES (?, ?)`, uuid.NewString(), gorg)
	e.exec(`INSERT INTO billing_invoices (id, organization_id, doc_type, created_by) VALUES (?, ?, 'quote', ?)`, uuid.NewString(), gorg, gowner)
	gv = e.call("GET", "/onboarding/me", gt, nil).Data
	if gv.DoneCount != 4 { // team, logo, details, customer — a quote is not an invoice
		t.Fatalf("general owner done %d %+v", gv.DoneCount, gv.Steps)
	}
	// A general staff member sees the generic staff steps; their own invoice ticks theirs.
	st := token(gstaff["Sam"], "staff", gorg, false)
	sv := e.call("GET", "/onboarding/me", st, nil).Data
	if keysOf(sv) != "phone,customers,bookings,sale,invoice" || sv.RoleName != "Staff member" {
		t.Fatalf("general staff %s %s", keysOf(sv), sv.RoleName)
	}
	e.exec(`INSERT INTO billing_invoices (id, organization_id, doc_type, created_by) VALUES (?, ?, 'invoice', ?)`, uuid.NewString(), gorg, gowner)
	if s, _ := stepOf(e.call("GET", "/onboarding/me", st, nil).Data, "invoice"); s.Done {
		t.Fatal("the owner's invoice ticked the staff member's step")
	}
	if s, _ := stepOf(e.call("GET", "/onboarding/me", gt, nil).Data, "invoice"); !s.Done {
		t.Fatal("owner invoice")
	}
}

// The progress API: welcome, tick / untick, unknown steps, dismiss, show
// again, restart.
func TestProgressAPI(t *testing.T) {
	e := newEnv(t)
	org, _, staff := e.newOrg("nightclub", "Club Habari", map[string]string{"Brian": "bartender"})
	brian := staff["Brian"]
	tok := token(brian, "staff", org, false)
	r := e.call("GET", "/onboarding/me", tok, nil)
	if r.status != 200 || !r.Success || r.Data.Welcomed || r.Preview {
		t.Fatalf("get %+v", r)
	}
	r = e.call("POST", "/onboarding/me/welcomed", tok, nil)
	if r.status != 200 || !r.Data.Welcomed {
		t.Fatalf("welcomed %+v", r)
	}
	var at1 time.Time
	e.db.Raw(`SELECT welcomed_at FROM onboarding_progress WHERE organization_id = ? AND user_id = ?`, org, brian).Scan(&at1)
	e.call("POST", "/onboarding/me/welcomed", tok, nil)
	var at2 time.Time
	e.db.Raw(`SELECT welcomed_at FROM onboarding_progress WHERE organization_id = ? AND user_id = ?`, org, brian).Scan(&at2)
	if at1.IsZero() || !at1.Equal(at2) {
		t.Fatalf("welcomed_at kept: %v %v", at1, at2)
	}

	r = e.call("POST", "/onboarding/me/steps/bar", tok, map[string]bool{"done": true})
	if s, _ := stepOf(r.Data, "bar"); r.status != 200 || !s.Done || r.Data.DoneCount != 1 || r.Data.Percent != 20 {
		t.Fatalf("tick %+v", r)
	}
	r = e.call("POST", "/onboarding/me/steps/count", tok, nil) // no body = done
	if s, _ := stepOf(r.Data, "count"); !s.Done {
		t.Fatal("tick without a body")
	}
	r = e.call("POST", "/onboarding/me/steps/bar", tok, map[string]bool{"done": false})
	if s, _ := stepOf(r.Data, "bar"); s.Done || r.Data.DoneCount != 1 {
		t.Fatalf("untick %+v", r.Data)
	}
	var stored string
	e.db.Raw(`SELECT done_steps FROM onboarding_progress WHERE organization_id = ? AND user_id = ?`, org, brian).Scan(&stored)
	if !strings.Contains(stored, `"count"`) || strings.Contains(stored, `"bar"`) {
		t.Fatalf("done_steps %s", stored)
	}
	if r = e.call("POST", "/onboarding/me/steps/variance", tok, nil); r.status != 404 || r.Error.Code != "STEP_NOT_FOUND" {
		t.Fatalf("an owner's step for a bartender: %d %s", r.status, r.Error.Code)
	}
	if r = e.call("POST", "/onboarding/me/steps/bar", tok, "nope"); r.status != 400 {
		t.Fatalf("bad body %d", r.status)
	}

	r = e.call("POST", "/onboarding/me/dismiss", tok, nil)
	if !r.Data.Dismissed {
		t.Fatal("dismiss")
	}
	r = e.call("POST", "/onboarding/me/dismiss", tok, map[string]bool{"dismissed": false})
	if r.Data.Dismissed || !r.Data.Welcomed || r.Data.DoneCount != 1 {
		t.Fatalf("show again keeps progress %+v", r.Data)
	}
	// All done: every step ticked.
	for _, s := range r.Data.Steps {
		r = e.call("POST", "/onboarding/me/steps/"+s.Key, tok, map[string]bool{"done": true})
	}
	if !r.Data.AllDone || r.Data.Percent != 100 {
		t.Fatalf("all done %+v", r.Data)
	}
	r = e.call("POST", "/onboarding/me/restart", tok, nil)
	if r.Data.Welcomed || r.Data.Dismissed || r.Data.DoneCount != 0 {
		t.Fatalf("restart %+v", r.Data)
	}
	var n int64
	e.db.Model(&Progress{}).Where("organization_id = ? AND user_id = ?", org, brian).Count(&n)
	if n != 0 {
		t.Fatal("restart removes the row")
	}
	// No organisation in the token.
	if r = e.call("GET", "/onboarding/me", token(brian, "staff", "", false), nil); r.status != 401 {
		t.Fatalf("no org %d", r.status)
	}
}

// A super admin viewing an organisation gets the owner's steps read-only;
// on their own platform session nothing.
func TestPreviewMode(t *testing.T) {
	e := newEnv(t)
	org, owner, _ := e.newOrg("nightclub", "Club Habari", map[string]string{"Brian": "bartender"})
	sa := uuid.NewString()
	e.exec(`INSERT INTO users (id, email, first_name, role, organization_id) VALUES (?, 'kennedy@t.test', 'Kennedy', 'super_admin', 'platform-org')`, sa)
	// The owner has ticked something and been welcomed.
	ot := token(owner, "admin", org, false)
	e.call("POST", "/onboarding/me/welcomed", ot, nil)
	e.call("POST", "/onboarding/me/steps/variance", ot, nil)

	view := token(sa, "super_admin", org, true)
	r := e.call("GET", "/onboarding/me", view, nil)
	if r.status != 200 || !r.Preview || !r.Data.Preview || r.Data.RoleKey != "owner" || r.Data.FirstName != "Wanjiru" || keysOf(r.Data) != "setup,prices,team,delivery,count,variance" {
		t.Fatalf("preview %+v", r)
	}
	if s, _ := stepOf(r.Data, "variance"); !s.Done || !r.Data.Welcomed {
		t.Fatal("the preview shows the owner's progress")
	}
	before := progressRows(e)
	for _, c := range []struct {
		path string
		body interface{}
	}{{"/onboarding/me/welcomed", nil}, {"/onboarding/me/steps/team", map[string]bool{"done": true}}, {"/onboarding/me/steps/variance", map[string]bool{"done": false}}, {"/onboarding/me/dismiss", nil}, {"/onboarding/me/restart", nil}} {
		r = e.call("POST", c.path, view, c.body)
		if r.status != 200 || !r.Preview || !r.Data.Preview {
			t.Fatalf("%s in preview: %d %+v", c.path, r.status, r)
		}
	}
	if after := progressRows(e); after != before {
		t.Fatalf("preview wrote: %s → %s", before, after)
	}
	if r = e.call("POST", "/onboarding/me/steps/nope", view, nil); r.status != 404 {
		t.Fatalf("unknown step in preview %d", r.status)
	}
	// A forged claim without the super admin role is not a preview.
	if r = e.call("GET", "/onboarding/me", token(owner, "admin", org, true), nil); r.Preview {
		t.Fatal("impersonating claim on an admin token")
	}
	// Platform session: hidden.
	r = e.call("GET", "/onboarding/me", token(sa, "super_admin", org, false), nil)
	if r.status != 200 || !r.Data.Hidden || len(r.Data.Steps) != 0 || r.Data.Preview {
		t.Fatalf("platform session %+v", r.Data)
	}
	if r = e.call("POST", "/onboarding/me/welcomed", token(sa, "super_admin", org, false), nil); r.status != 200 || !r.Data.Hidden {
		t.Fatalf("platform session write %+v", r)
	}
	if after := progressRows(e); after != before {
		t.Fatal("platform session wrote a row")
	}
}

func progressRows(e *env) string {
	var rows []Progress
	e.db.Order("organization_id, user_id").Find(&rows)
	out := []string{}
	for _, p := range rows {
		out = append(out, fmt.Sprintf("%s/%s/%v/%v/%v", p.OrganizationID, p.UserID, p.WelcomedAt != nil, p.DismissedAt != nil, p.DoneSteps))
	}
	return strings.Join(out, "|")
}

// The handover reset hook deletes only that organisation's rows.
func TestResetHookDeletesOnlyThatOrg(t *testing.T) {
	e := newEnv(t)
	a, aOwner, aStaff := e.newOrg("nightclub", "Club A", map[string]string{"Brian": "bartender"})
	b, bOwner, _ := e.newOrg("salon", "Salon B", nil)
	for _, w := range []struct{ user, role, org string }{{aOwner, "admin", a}, {aStaff["Brian"], "staff", a}, {bOwner, "admin", b}} {
		e.call("POST", "/onboarding/me/welcomed", token(w.user, w.role, w.org, false), nil)
	}
	count := func(org string) (n int64) {
		e.db.Model(&Progress{}).Where("organization_id = ?", org).Count(&n)
		return
	}
	if count(a) != 2 || count(b) != 1 {
		t.Fatalf("rows %d %d", count(a), count(b))
	}
	RegisterResetHook()
	var hook *orghooks.ResetHook
	for _, h := range orghooks.ResetHooks() {
		if h.Name == "onboarding" {
			h := h
			hook = &h
		}
	}
	if hook == nil {
		t.Fatal("onboarding reset hook not registered")
	}
	if err := e.db.Transaction(func(tx *gorm.DB) error { return hook.Fn(tx, a, orghooks.ResetOptions{KeepStaff: true}) }); err != nil {
		t.Fatal(err)
	}
	if count(a) != 0 || count(b) != 1 {
		t.Fatalf("after reset %d %d", count(a), count(b))
	}
	// Everyone in A starts fresh: the welcome sheet shows again.
	if v := e.call("GET", "/onboarding/me", token(aOwner, "admin", a, false), nil).Data; v.Welcomed {
		t.Fatal("owner still welcomed after reset")
	}
	// No table yet (module not migrated): the hook is a no-op, not an error.
	bare := openSchema(t, false)
	if err := ResetOrg(bare, a, orghooks.ResetOptions{}); err != nil {
		t.Fatal(err)
	}
}

// First install: people who already signed in are not greeted again;
// people who never signed in (a new client's owner) get the welcome.
func TestMigrateBackfill(t *testing.T) {
	db := openSchema(t, true)
	org := uuid.NewString()
	seen, fresh, sa := uuid.NewString(), uuid.NewString(), uuid.NewString()
	db.Exec(`INSERT INTO users (id, role, organization_id, last_login_at) VALUES (?, 'admin', ?, now()), (?, 'staff', ?, NULL), (?, 'super_admin', ?, now())`, seen, org, fresh, org, sa, org)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var ids []string
	db.Model(&Progress{}).Where("welcomed_at IS NOT NULL").Pluck("user_id", &ids)
	if len(ids) != 1 || ids[0] != seen {
		t.Fatalf("backfilled %v", ids)
	}
	// Running again changes nothing (only on first creation).
	db.Exec(`UPDATE users SET last_login_at = now() WHERE id = ?`, fresh)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var n int64
	db.Model(&Progress{}).Count(&n)
	if n != 1 {
		t.Fatalf("second migrate added rows: %d", n)
	}
}

// Steps follow the plan and the role at run time: a module outside the plan
// drops its steps.
func TestStepsFollowPlanAndRole(t *testing.T) {
	e := newEnv(t)
	org, owner, staff := e.newOrg("nightclub", "Club Habari", map[string]string{"Hosi": "host", "Cash": "cashier"})
	e.ind.PlanModules = func(string) (map[string]bool, error) { return map[string]bool{"inventory": true, "pos": true}, nil }
	e.ind.Invalidate("")
	v := e.call("GET", "/onboarding/me", token(owner, "admin", org, false), nil).Data
	if keysOf(v) != "setup,prices,team,delivery,count,variance" {
		t.Fatalf("owner with pos+inventory plan %s", keysOf(v))
	}
	// Host: reservations, guests and events are not in this plan.
	hv := e.call("GET", "/onboarding/me", token(staff["Hosi"], "staff", org, false), nil).Data
	if len(hv.Steps) != 0 || hv.RoleKey != "host" {
		t.Fatalf("host without bookings/crm/events in the plan: %s", keysOf(hv))
	}
	cv := e.call("GET", "/onboarding/me", token(staff["Cash"], "staff", org, false), nil).Data
	if keysOf(cv) != "drawer,order" {
		t.Fatalf("cashier without bookings %s", keysOf(cv))
	}
	e.ind.PlanModules = nil
	e.ind.Invalidate("")
	if keysOf(e.call("GET", "/onboarding/me", token(staff["Hosi"], "staff", org, false), nil).Data) != "reservation,guest,tonight,checkin" {
		t.Fatal("host with every module")
	}
}
