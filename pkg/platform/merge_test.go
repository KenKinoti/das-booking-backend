package platform

import (
	"bytes"
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

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

func TestNormalizeOrgName(t *testing.T) {
	same := [][2]string{
		{"DASYIN", "dasyin"}, {"Dasyin Pty Ltd", "DASYIN"}, {"Acme & Sons", "acme and sons, inc."}, {" Bright-Care  LLC", "brightcare"},
	}
	for _, p := range same {
		if NormalizeOrgName(p[0]) != NormalizeOrgName(p[1]) {
			t.Errorf("%q and %q should match (%q vs %q)", p[0], p[1], NormalizeOrgName(p[0]), NormalizeOrgName(p[1]))
		}
	}
	if NormalizeOrgName("Acme") == NormalizeOrgName("Acme Two") {
		t.Error("different names matched")
	}
	if NormalizeOrgName("Co") != "co" {
		t.Errorf("a name made only of a suffix must survive: %q", NormalizeOrgName("Co"))
	}
}

// Integration test: TEST_DATABASE_URL=postgres://…/das_test?sslmode=disable&search_path=erp
// (create the erp schema first).
func mergeSetup(t *testing.T) (*gorm.DB, *gin.Engine, string) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("CREATE SCHEMA IF NOT EXISTS erp")
	if err := db.AutoMigrate(&models.Organization{}, &models.User{}, &models.RefreshToken{}, &models.AuditLog{}, &OrgStatus{}); err != nil {
		t.Fatal(err)
	}
	if err := invoicing.Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE IF NOT EXISTS mt_products (id varchar(64) PRIMARY KEY, organization_id varchar(255) NOT NULL, name text, sku varchar(100), deleted_at timestamptz)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS mt_products_org_sku ON mt_products (organization_id, sku) WHERE deleted_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS mt_links (id varchar(64) PRIMARY KEY, org_id varchar(255), user_ref varchar(64))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS mt_links_org_user ON mt_links (org_id, user_ref)`,
		`CREATE TABLE IF NOT EXISTS mt_prefs (organization_id varchar(255) PRIMARY KEY, value text)`,
	} {
		if err := db.Exec(s).Error; err != nil {
			t.Fatal(err)
		}
	}
	actor := "actor-" + uuid.NewString()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api", func(c *gin.Context) {
		c.Set("org_id", actor)
		c.Set("user_id", "super-1")
		c.Set("user_role", c.GetHeader("X-Role"))
		c.Next()
	})
	NewHandler(db, nil).RegisterRoutes(g)
	return db, r, actor
}

func mcall(t *testing.T, r *gin.Engine, role, method, path string, body interface{}) (int, map[string]interface{}) {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Role", role)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func mkOrg(t *testing.T, db *gorm.DB, name string, created time.Time) string {
	o := models.Organization{ID: uuid.NewString(), Name: name, BusinessType: "general", CreatedAt: created, UpdatedAt: created, EnabledModules: []string{}, ModulesConfig: map[string]bool{}}
	if err := db.Omit("Users", "Customers", "Vehicles", "Services", "Bookings", "ABN").Create(&o).Error; err != nil {
		t.Fatal(err)
	}
	return o.ID
}

func mkUser(t *testing.T, db *gorm.DB, org, email string) string {
	u := models.User{ID: uuid.NewString(), Email: email, PasswordHash: "x", FirstName: "A", LastName: "B", Role: "admin", OrganizationID: org, IsActive: true}
	if err := db.Omit("Organization").Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	db.Create(&models.RefreshToken{ID: uuid.NewString(), UserID: u.ID, Token: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour)})
	return u.ID
}

func mkInvoice(t *testing.T, db *gorm.DB, org, docType, number string, total float64) string {
	inv := invoicing.Invoice{ID: uuid.NewString(), OrganizationID: org, DocType: docType, Number: number, Status: "sent", ClientName: "C",
		Currency: "AUD", Total: total, BalanceDue: total, IssueDate: time.Now(), DueDate: time.Now(), PublicToken: uuid.NewString()}
	if err := db.Omit("Items", "Payments", "Activities").Create(&inv).Error; err != nil {
		t.Fatal(err)
	}
	return inv.ID
}

func TestMergeDuplicates(t *testing.T) {
	db, r, _ := mergeSetup(t)
	name := "Merge Test " + uuid.NewString()[:8]
	keep := mkOrg(t, db, name, time.Now().Add(-48*time.Hour))
	dup := mkOrg(t, db, strings.ToUpper(name)+" Pty Ltd", time.Now().Add(-24*time.Hour))
	other := mkOrg(t, db, "Unrelated "+uuid.NewString()[:8], time.Now())
	db.Table("organizations").Where("id = ?", dup).Updates(map[string]interface{}{"email": "hello@" + keep[:8] + ".test", "phone": "0400 000 000"})

	// Users on both sides
	mkUser(t, db, keep, "k-"+keep[:8]+"@test.com")
	du1 := mkUser(t, db, dup, "d1-"+dup[:8]+"@test.com")
	mkUser(t, db, dup, "d2-"+dup[:8]+"@test.com")

	// Overlapping invoice numbers (per doc type), a quote with the same number
	// as an invoice is fine.
	mkInvoice(t, db, keep, "invoice", "INV-1001", 100)
	mkInvoice(t, db, keep, "invoice", "INV-1002", 100)
	clash := mkInvoice(t, db, dup, "invoice", "INV-1001", 50)
	mkInvoice(t, db, dup, "invoice", "INV-1003", 70)
	quote := mkInvoice(t, db, dup, "quote", "INV-1002", 10)
	// A pre-existing "-M1" forces the next free suffix.
	mkInvoice(t, db, keep, "invoice", "INV-1001-M1", 1)

	// Settings singletons: both have one → keep target's. mt_prefs only dup → moved.
	db.Exec("INSERT INTO billing_settings (organization_id, business_name, currency) VALUES (?, 'KEEP', 'AUD'), (?, 'DUP', 'KES')", keep, dup)
	db.Exec("INSERT INTO mt_prefs (organization_id, value) VALUES (?, 'dup-pref')", dup)
	// Unique (org, sku): clash renamed; (org, user_ref): clash dropped.
	db.Exec("INSERT INTO mt_products (id, organization_id, name, sku) VALUES (?, ?, 'Widget', 'W-1'), (?, ?, 'Widget', 'W-1'), (?, ?, 'Gadget', 'G-1')",
		uuid.NewString(), keep, uuid.NewString(), dup, uuid.NewString(), dup)
	db.Exec("INSERT INTO mt_links (id, org_id, user_ref) VALUES (?, ?, 'u1'), (?, ?, 'u1'), (?, ?, 'u2')",
		uuid.NewString(), keep, uuid.NewString(), dup, uuid.NewString(), dup)

	// Only super admins
	if code, _ := mcall(t, r, "admin", "GET", "/api/super-admin/platform/organizations/duplicates", nil); code != 403 {
		t.Fatalf("admin reached duplicates: %d", code)
	}

	// Detection
	code, res := mcall(t, r, "super_admin", "GET", "/api/super-admin/platform/organizations/duplicates", nil)
	if code != 200 {
		t.Fatalf("duplicates: %d %v", code, res)
	}
	var group map[string]interface{}
	for _, g := range res["data"].(map[string]interface{})["groups"].([]interface{}) {
		gm := g.(map[string]interface{})
		for _, o := range gm["organizations"].([]interface{}) {
			if o.(map[string]interface{})["id"] == keep {
				group = gm
			}
		}
	}
	if group == nil {
		t.Fatal("duplicate group not detected")
	}
	orgs := group["organizations"].([]interface{})
	if len(orgs) != 2 {
		t.Fatalf("expected 2 orgs in group, got %d", len(orgs))
	}
	for _, o := range orgs {
		om := o.(map[string]interface{})
		if om["id"] == other {
			t.Fatal("unrelated org grouped")
		}
		if om["id"] == dup {
			counts := om["counts"].(map[string]interface{})
			if counts["users"].(float64) != 2 || counts["billing_invoices"].(float64) != 3 {
				t.Fatalf("dup counts wrong: %v", counts)
			}
		}
	}
	// Dup has 2 users, keep 1 → suggestion is the dup (most users).
	if group["suggested_keep_id"] != dup {
		t.Fatalf("suggested keep %v, want %v", group["suggested_keep_id"], dup)
	}

	// Dry run: report but no change
	body := map[string]interface{}{"keep_id": keep, "merge_ids": []string{dup}, "dry_run": true}
	code, res = mcall(t, r, "super_admin", "POST", "/api/super-admin/platform/organizations/merge", body)
	if code != 200 {
		t.Fatalf("dry run: %d %v", code, res)
	}
	rep := res["data"].(map[string]interface{})
	if rep["dry_run"] != true || rep["users_moved"].(float64) != 2 {
		t.Fatalf("dry run report: %v", rep)
	}
	renum := rep["renumbered"].([]interface{})
	var sawInv, sawSku bool
	for _, x := range renum {
		m := x.(map[string]interface{})
		if m["table"] == "billing_invoices" && m["from"] == "INV-1001" && m["to"] == "INV-1001-M2" {
			sawInv = true
		}
		if m["table"] == "mt_products" && m["from"] == "W-1" && m["to"] == "W-1-M1" {
			sawSku = true
		}
		if m["table"] == "billing_invoices" && m["from"] == "INV-1002" {
			t.Fatal("a quote must not be renumbered because of an invoice with the same number")
		}
	}
	if !sawInv || !sawSku {
		t.Fatalf("renumbering not reported: %v", renum)
	}
	var n int64
	db.Table("users").Where("organization_id = ?", dup).Count(&n)
	if n != 2 {
		t.Fatal("dry run changed data")
	}

	// Real merge needs the typed name
	body["dry_run"] = false
	if code, _ = mcall(t, r, "super_admin", "POST", "/api/super-admin/platform/organizations/merge", body); code != 400 {
		t.Fatalf("merge without confirmation: %d", code)
	}
	body["confirm_name"] = name
	code, res = mcall(t, r, "super_admin", "POST", "/api/super-admin/platform/organizations/merge", body)
	if code != 200 {
		t.Fatalf("merge: %d %v", code, res)
	}

	// Everything now belongs to keep
	for _, tbl := range []string{"users", "billing_invoices"} {
		db.Table(tbl).Where("organization_id = ?", dup).Count(&n)
		if n != 0 {
			t.Fatalf("%s still has %d rows on the duplicate", tbl, n)
		}
	}
	db.Table("users").Where("organization_id = ?", keep).Count(&n)
	if n != 3 {
		t.Fatalf("keep has %d users, want 3", n)
	}
	var num string
	db.Table("billing_invoices").Select("number").Where("id = ?", clash).Scan(&num)
	if num != "INV-1001-M2" {
		t.Fatalf("clashing invoice number %q", num)
	}
	db.Table("billing_invoices").Select("number").Where("id = ?", quote).Scan(&num)
	if num != "INV-1002" {
		t.Fatalf("quote renumbered to %q", num)
	}
	var bn string
	db.Table("billing_settings").Select("business_name").Where("organization_id = ?", keep).Scan(&bn)
	if bn != "KEEP" {
		t.Fatalf("target settings replaced: %q", bn)
	}
	db.Table("billing_settings").Where("organization_id = ?", dup).Count(&n)
	if n != 0 {
		t.Fatal("duplicate settings left behind")
	}
	var pref string
	db.Raw("SELECT value FROM mt_prefs WHERE organization_id = ?", keep).Scan(&pref)
	if pref != "dup-pref" {
		t.Fatalf("singleton not moved when target had none: %q", pref)
	}
	db.Raw("SELECT COUNT(*) FROM mt_links WHERE org_id = ?", keep).Scan(&n)
	if n != 2 { // u1 (keep's) + u2 (moved); dup's u1 dropped
		t.Fatalf("mt_links: %d", n)
	}
	db.Raw("SELECT COUNT(*) FROM mt_products WHERE organization_id = ?", keep).Scan(&n)
	if n != 3 {
		t.Fatalf("mt_products: %d", n)
	}
	// Details filled, duplicate archived and marked merged, sessions revoked, audited.
	var o struct {
		Email     string
		DeletedAt *time.Time
	}
	db.Raw("SELECT email, NULL::timestamptz AS deleted_at FROM organizations WHERE id = ?", keep).Scan(&o)
	if !strings.HasPrefix(o.Email, "hello@") {
		t.Fatalf("email not filled from the duplicate: %q", o.Email)
	}
	db.Raw("SELECT deleted_at FROM organizations WHERE id = ?", dup).Scan(&o)
	if o.DeletedAt == nil {
		t.Fatal("duplicate not archived")
	}
	var st string
	db.Raw("SELECT status FROM platform_org_status WHERE organization_id = ?", dup).Scan(&st)
	if st != "merged" {
		t.Fatalf("status %q", st)
	}
	var revoked bool
	db.Raw("SELECT is_revoked FROM refresh_tokens WHERE user_id = ?", du1).Scan(&revoked)
	if !revoked {
		t.Fatal("moved user's refresh token not revoked")
	}
	db.Table("audit_logs").Where("organization_id = ? AND action = ?", keep, "organization.merge").Count(&n)
	if n != 1 {
		t.Fatal("merge not audited")
	}
	// A second merge of the same (now archived) org fails cleanly.
	if code, _ = mcall(t, r, "super_admin", "POST", "/api/super-admin/platform/organizations/merge", body); code != 404 {
		t.Fatalf("re-merge: %d", code)
	}
}

func TestMergeRefusesOwnOrganization(t *testing.T) {
	db, r, actor := mergeSetup(t)
	keep := mkOrg(t, db, "Own "+uuid.NewString()[:6], time.Now())
	code, _ := mcall(t, r, "super_admin", "POST", "/api/super-admin/platform/organizations/merge",
		map[string]interface{}{"keep_id": keep, "merge_ids": []string{actor}, "dry_run": true})
	if code != 400 {
		t.Fatalf("merging away own org: %d", code)
	}
}
