package occasions

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`DROP SCHEMA IF EXISTS occasions_test CASCADE`)
	boot.Exec(`CREATE SCHEMA occasions_test`)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=occasions_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE organizations (id varchar(255) primary key, name text, country_code text, currency text, timezone text, region_source text, address_country text)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

type harness struct {
	t   *testing.T
	db  *gorm.DB
	svc *Service
	r   *gin.Engine
}

func newHarness(t *testing.T) *harness {
	gin.SetMode(gin.TestMode)
	db := testDB(t)
	svc := NewService(db)
	r := gin.New()
	auth := r.Group("/api/v1", func(c *gin.Context) {
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("user_role", c.GetHeader("X-Role"))
	})
	(&Handler{DB: db, Svc: svc}).RegisterRoutes(auth)
	return &harness{t, db, svc, r}
}

func (h *harness) org(country, tz string) string {
	id := uuid.NewString()
	h.db.Exec("INSERT INTO organizations (id, name, country_code, timezone, region_source) VALUES (?, 'Org', ?, ?, 'user')", id, country, tz)
	return id
}

type who struct{ org, user, role string }

func (h *harness) call(method, path string, body interface{}, w who) (int, map[string]interface{}) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Org", w.org)
	req.Header.Set("X-User", w.user)
	req.Header.Set("X-Role", w.role)
	rec := httptest.NewRecorder()
	h.r.ServeHTTP(rec, req)
	out := map[string]interface{}{}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func data(m map[string]interface{}) map[string]interface{} {
	d, _ := m["data"].(map[string]interface{})
	return d
}

func themeOf(m map[string]interface{}) (string, string) {
	d := data(m)
	th, _ := d["theme"].(string)
	reason, _ := d["reason"].(string)
	return th, reason
}

func TestThemeEndpointAndSettings(t *testing.T) {
	h := newHarness(t)
	ke, au, nowhere := h.org("KE", "Africa/Nairobi"), h.org("AU", "Australia/Sydney"), h.org("", "")
	owner := who{ke, uuid.NewString(), "admin"}
	staff := who{ke, uuid.NewString(), "staff"}
	auOwner := who{au, uuid.NewString(), "admin"}
	sa := who{ke, uuid.NewString(), "super_admin"}

	t.Setenv("OCCASIONS_TODAY", "2026-10-30")
	code, out := h.call("GET", "/occasions/theme", nil, staff)
	if th, reason := themeOf(out); code != 200 || th != "halloween" || reason != ReasonOn {
		t.Fatalf("halloween for KE staff: %d %v", code, out)
	}
	occ := data(out)["occasion"].(map[string]interface{})
	if occ["greeting"] != "Happy Halloween" || data(out)["today"] != "2026-10-30" || data(out)["country"] != "KE" {
		t.Errorf("payload %v", data(out))
	}
	// The organisation's country picks national days.
	t.Setenv("OCCASIONS_TODAY", "2026-10-20")
	if _, out := h.call("GET", "/occasions/theme", nil, staff); data(out)["theme"] != "kenya_national" {
		t.Errorf("Mashujaa for KE: %v", data(out))
	}
	if _, out := h.call("GET", "/occasions/theme", nil, auOwner); data(out)["theme"] != "" || data(out)["reason"] != ReasonNone {
		t.Errorf("AU has no Mashujaa Day: %v", data(out))
	}
	if _, out := h.call("GET", "/occasions/theme?country=au", nil, staff); data(out)["theme"] != "" {
		t.Errorf("?country=AU: %v", data(out))
	}
	// No country known: only occasions marked everywhere.
	if _, out := h.call("GET", "/occasions/theme", nil, who{nowhere, uuid.NewString(), "admin"}); data(out)["theme"] != "" {
		t.Errorf("no country on Mashujaa Day: %v", data(out))
	}
	t.Setenv("OCCASIONS_TODAY", "2026-10-30")

	// Organisation switch: staff may not; owners may.
	if code, _ := h.call("PUT", "/occasions/settings/org", map[string]bool{"enabled": false}, staff); code != http.StatusForbidden {
		t.Errorf("staff changed org setting: %d", code)
	}
	if code, out := h.call("PUT", "/occasions/settings/org", map[string]bool{"enabled": false}, owner); code != 200 || data(out)["settings"].(map[string]interface{})["org"] != false {
		t.Fatalf("owner off: %d %v", code, out)
	}
	if _, out := h.call("GET", "/occasions/theme", nil, staff); data(out)["theme"] != "" || data(out)["reason"] != ReasonOffOrg {
		t.Errorf("org off: %v", data(out))
	}
	// Other organisations are unaffected.
	if _, out := h.call("GET", "/occasions/theme", nil, auOwner); data(out)["theme"] != "halloween" {
		t.Errorf("AU org still on: %v", data(out))
	}
	// A person can keep them for their own screen, or follow the org again.
	h.call("PUT", "/occasions/settings/me", map[string]interface{}{"show": true}, staff)
	if _, out := h.call("GET", "/occasions/theme", nil, staff); data(out)["theme"] != "halloween" {
		t.Errorf("user on while org off: %v", data(out))
	}
	h.call("PUT", "/occasions/settings/me", map[string]interface{}{"show": nil}, staff)
	if _, out := h.call("GET", "/occasions/theme", nil, staff); data(out)["reason"] != ReasonOffOrg {
		t.Errorf("follow org again: %v", data(out))
	}
	h.call("PUT", "/occasions/settings/org", map[string]bool{"enabled": true}, owner)
	h.call("PUT", "/occasions/settings/me", map[string]interface{}{"show": false}, staff)
	if _, out := h.call("GET", "/occasions/theme", nil, staff); data(out)["reason"] != ReasonOffUser {
		t.Errorf("user off: %v", data(out))
	}
	if _, out := h.call("GET", "/occasions/theme", nil, owner); data(out)["theme"] != "halloween" {
		t.Errorf("user off is personal: %v", data(out))
	}
	if code, out := h.call("GET", "/occasions/settings", nil, staff); code != 200 || data(out)["settings"].(map[string]interface{})["user"] != false || data(out)["settings"].(map[string]interface{})["can_edit_org"] != false {
		t.Errorf("settings %v", out)
	}
	if code, _ := h.call("PUT", "/occasions/settings/org", map[string]string{"enabled": "yes"}, owner); code != 400 {
		t.Errorf("bad body %d", code)
	}

	// Preview: super admins only.
	if _, out := h.call("GET", "/occasions/theme?preview=christmas", nil, sa); data(out)["theme"] != "christmas" || data(out)["reason"] != ReasonPreview {
		t.Errorf("preview christmas %v", data(out))
	}
	if _, out := h.call("GET", "/occasions/theme?preview=mashujaa_day", nil, sa); data(out)["occasion"].(map[string]interface{})["greeting"] != "Happy Mashujaa Day" {
		t.Errorf("preview mashujaa %v", data(out))
	}
	if _, out := h.call("GET", "/occasions/theme?preview=christmas", nil, owner); data(out)["theme"] != "halloween" {
		t.Errorf("non-admin preview ignored: %v", data(out))
	}
	if code, _ := h.call("GET", "/occasions/theme?preview=nonsense", nil, sa); code != 404 {
		t.Errorf("unknown preview %d", code)
	}
	if _, out := h.call("GET", "/occasions/theme?date=2026-12-12", nil, sa); data(out)["theme"] != "kenya_national" {
		t.Errorf("super admin ?date: %v", data(out))
	}
}

func TestSuperAdminOccasions(t *testing.T) {
	h := newHarness(t)
	ke := h.org("KE", "Africa/Nairobi")
	sa := who{ke, uuid.NewString(), "super_admin"}
	owner := who{ke, uuid.NewString(), "admin"}
	t.Setenv("OCCASIONS_TODAY", "2026-10-30")

	if code, _ := h.call("GET", "/super-admin/occasions", nil, owner); code != http.StatusForbidden {
		t.Fatalf("owner on super-admin list: %d", code)
	}
	code, out := h.call("GET", "/super-admin/occasions?from=2026-10-01&to=2026-12-31&country=KE", nil, sa)
	if code != 200 || data(out)["global_enabled"] != true || len(data(out)["occasions"].([]interface{})) < 8 || len(data(out)["themes"].([]interface{})) != len(Themes) {
		t.Fatalf("list: %d %v", code, out)
	}
	keys := data(out)["keys"].([]interface{})
	var sawChristmas bool
	for _, k := range keys {
		m := k.(map[string]interface{})
		if m["key"] == "christmas_day" {
			sawChristmas = true
			if len(m["countries"].([]interface{})) != 0 {
				t.Errorf("christmas is everywhere: %v", m)
			}
		}
	}
	if !sawChristmas {
		t.Error("keys")
	}
	if code, _ := h.call("GET", "/super-admin/occasions?from=2020-01-01&to=2030-01-01", nil, sa); code != 400 {
		t.Error("huge range")
	}

	// Per-occasion switch.
	if code, _ := h.call("PUT", "/super-admin/occasions/toggles/halloween", map[string]bool{"enabled": false}, sa); code != 200 {
		t.Fatal(code)
	}
	if _, out := h.call("GET", "/occasions/theme", nil, owner); data(out)["theme"] != "" {
		t.Errorf("halloween off: %v", data(out))
	}
	if code, _ := h.call("PUT", "/super-admin/occasions/toggles/not_a_day", map[string]bool{"enabled": false}, sa); code != 404 {
		t.Error("unknown key")
	}
	h.call("PUT", "/super-admin/occasions/toggles/halloween", map[string]bool{"enabled": true}, sa)

	// Global switch.
	h.call("PUT", "/super-admin/occasions/settings", map[string]bool{"enabled": false}, sa)
	if _, out := h.call("GET", "/occasions/theme", nil, owner); data(out)["reason"] != ReasonOffGlobal {
		t.Errorf("global off: %v", data(out))
	}
	h.call("PUT", "/super-admin/occasions/settings", map[string]bool{"enabled": true}, sa)

	// Custom occasions: create, validate, resolve, update, delete.
	in := map[string]interface{}{"name": "DASYIN anniversary", "date": "2026-10-29", "theme_from": "2026-10-28", "theme_to": "2026-10-30", "countries": []string{"KE"},
		"theme": "new_year", "accent": "#0b5394", "greeting": "Happy anniversary, DASYIN", "important": true}
	code, out = h.call("POST", "/super-admin/occasions/custom", in, sa)
	if code != 201 || data(out)["accent_dark"] == "" || data(out)["key"] == "" {
		t.Fatalf("create: %d %v", code, out)
	}
	id := data(out)["id"].(string)
	if _, out := h.call("GET", "/occasions/theme", nil, owner); data(out)["theme"] != "new_year" || data(out)["occasion"].(map[string]interface{})["palette"] == nil {
		t.Errorf("custom wins on 30 Oct: %v", data(out))
	}
	if _, out := h.call("GET", "/super-admin/occasions/resolve?date=2026-10-29&country=AU", nil, sa); data(out)["occasion"].(map[string]interface{})["key"] != "halloween" {
		t.Errorf("custom is KE only: %v", data(out))
	}
	bad := map[string]interface{}{"name": "x", "date": "2026-10-29", "theme": "new_year", "accent": "#fff59d"}
	if code, out := h.call("POST", "/super-admin/occasions/custom", bad, sa); code != 400 || out["field"] != "accent" {
		t.Errorf("low-contrast accent: %d %v", code, out)
	}
	in["enabled"] = false
	if code, out := h.call("PUT", "/super-admin/occasions/custom/"+id, in, sa); code != 200 || data(out)["enabled"] != false {
		t.Fatalf("update: %d %v", code, out)
	}
	if _, out := h.call("GET", "/occasions/theme", nil, owner); data(out)["theme"] != "halloween" {
		t.Errorf("disabled custom: %v", data(out))
	}
	if _, out := h.call("GET", "/super-admin/occasions/custom", nil, sa); len(data(out)["custom"].([]interface{})) != 1 {
		t.Error("list custom")
	}
	if code, _ := h.call("DELETE", "/super-admin/occasions/custom/"+id, nil, sa); code != 200 {
		t.Error("delete")
	}
	if code, _ := h.call("DELETE", "/super-admin/occasions/custom/"+id, nil, sa); code != 404 {
		t.Error("delete twice")
	}
	var n int64
	h.db.Model(&CustomOccasion{}).Count(&n)
	if n != 0 {
		t.Error("row left")
	}
}
