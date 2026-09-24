package branding_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := branding.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := invoicing.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func pngOf(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// router mimics production: X-Org / X-Role headers stand in for the JWT.
func router(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := branding.NewHandler(db)
	auth := r.Group("/api/v1", func(c *gin.Context) {
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_role", c.GetHeader("X-Role"))
		c.Set("user_id", "u-"+c.GetHeader("X-Org"))
		c.Next()
	})
	h.RegisterRoutes(auth)
	h.RegisterPublic(r.Group("/api/v1/public"))
	inv := invoicing.NewHandler(db)
	invoicing.LogoURL = func(r *http.Request, org string) string { return h.Svc.URLFor(r, org) }
	inv.RegisterRoutes(auth.Group("/invoicing"))
	return r
}

func do(r http.Handler, method, path, org, role string, body []byte, ctype string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("X-Org", org)
	req.Header.Set("X-Role", role)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func jsonUpload(data []byte) []byte {
	b, _ := json.Marshal(map[string]string{"data_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)})
	return b
}

func multipartUpload(t *testing.T, name string, data []byte) ([]byte, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func dataOf(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var res struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad json %s", w.Body.String())
	}
	return res.Data
}

func TestUploadIsolationAndServing(t *testing.T) {
	db := testDB(t)
	r := router(db)
	orgA, orgB := "org-a-"+uuid.NewString(), "org-b-"+uuid.NewString()
	logoA := pngOf(t, 60, 20, color.RGBA{255, 0, 0, 255})
	logoB := pngOf(t, 30, 30, color.RGBA{0, 0, 255, 255})

	// staff can't upload; admins can (multipart); super admins can (JSON data URI)
	if w := do(r, "PUT", "/api/v1/organization/logo", orgA, "staff", jsonUpload(logoA), "application/json", nil); w.Code != 403 {
		t.Fatalf("staff upload should be 403, got %d", w.Code)
	}
	body, ct := multipartUpload(t, "a.png", logoA)
	w := do(r, "PUT", "/api/v1/organization/logo", orgA, "admin", body, ct, nil)
	if w.Code != 200 {
		t.Fatalf("admin upload: %d %s", w.Code, w.Body.String())
	}
	infoA := dataOf(t, w)
	urlA, _ := infoA["logo_url"].(string)
	if infoA["has_logo"] != true || !strings.Contains(urlA, "/api/v1/public/org-logo/"+orgA+"?v=") || !strings.HasPrefix(urlA, "http://") {
		t.Fatalf("upload response: %v", infoA)
	}
	if w := do(r, "PUT", "/api/v1/organization/logo", orgB, "super_admin", jsonUpload(logoB), "application/json", nil); w.Code != 200 {
		t.Fatalf("super admin upload: %d %s", w.Code, w.Body.String())
	}

	// unsafe / wrong uploads are rejected with a readable reason
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	sb, _ := json.Marshal(map[string]string{"data_url": "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(svg)})
	if w := do(r, "PUT", "/api/v1/organization/logo", orgA, "admin", sb, "application/json", nil); w.Code != 400 || !strings.Contains(w.Body.String(), "script") {
		t.Fatalf("svg with script should 400: %d %s", w.Code, w.Body.String())
	}
	big, ct := multipartUpload(t, "big.png", append(pngOf(t, 2, 2, color.White), make([]byte, branding.MaxLogoBytes)...))
	if w := do(r, "PUT", "/api/v1/organization/logo", orgA, "admin", big, ct, nil); w.Code != 413 {
		t.Fatalf("oversized upload should 413, got %d %s", w.Code, w.Body.String())
	}
	txt, ct := multipartUpload(t, "logo.png", []byte("just text pretending to be a png"))
	if w := do(r, "PUT", "/api/v1/organization/logo", orgA, "admin", txt, ct, nil); w.Code != 400 {
		t.Fatalf("fake png should 400, got %d", w.Code)
	}

	// the public endpoint serves each org's own image
	pub := do(r, "GET", "/api/v1/public/org-logo/"+orgA, "", "", nil, "", nil)
	if pub.Code != 200 || !bytes.Equal(pub.Body.Bytes(), logoA) || pub.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("public A: %d %s", pub.Code, pub.Header().Get("Content-Type"))
	}
	if pub.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(pub.Header().Get("Cache-Control"), "public") ||
		!strings.Contains(pub.Header().Get("Cache-Control"), "max-age=300") {
		t.Fatalf("public headers: %v", pub.Header())
	}
	etag := pub.Header().Get("ETag")
	if etag == "" || !strings.Contains(urlA, strings.Trim(etag, `"`)) {
		t.Fatalf("etag %q should version the URL %q", etag, urlA)
	}
	if w := do(r, "GET", "/api/v1/public/org-logo/"+orgB, "", "", nil, "", nil); !bytes.Equal(w.Body.Bytes(), logoB) {
		t.Fatal("public B served the wrong image")
	}
	// ETag → 304
	if w := do(r, "GET", "/api/v1/public/org-logo/"+orgA, "", "", nil, "", map[string]string{"If-None-Match": etag}); w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("If-None-Match should 304, got %d", w.Code)
	}
	if w := do(r, "GET", "/api/v1/public/org-logo/"+orgA, "", "", nil, "", map[string]string{"If-None-Match": `"stale"`}); w.Code != 200 {
		t.Fatalf("stale etag should 200, got %d", w.Code)
	}

	// invoicing settings expose the org logo read-only, per org
	w = do(r, "GET", "/api/v1/invoicing/settings", orgB, "admin", nil, "", nil)
	var sres struct {
		Data struct {
			Settings map[string]interface{} `json:"settings"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sres)
	if u, _ := sres.Data.Settings["logo_url"].(string); !strings.Contains(u, "/org-logo/"+orgB+"?v=") {
		t.Fatalf("invoicing settings logo_url: %q", u)
	}
	// saving invoice settings can't change the logo
	sres.Data.Settings["logo_url"] = "data:image/png;base64,AAAA"
	sb, _ = json.Marshal(sres.Data.Settings)
	w = do(r, "PUT", "/api/v1/invoicing/settings", orgB, "admin", sb, "application/json", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/org-logo/"+orgB) {
		t.Fatalf("settings save: %d %s", w.Code, w.Body.String())
	}

	// org B removing its logo never touches org A
	if w := do(r, "DELETE", "/api/v1/organization/logo", orgB, "manager", nil, "", nil); w.Code != 403 {
		t.Fatalf("manager delete should 403, got %d", w.Code)
	}
	if w := do(r, "DELETE", "/api/v1/organization/logo", orgB, "admin", nil, "", nil); w.Code != 200 || dataOf(t, w)["has_logo"] != false {
		t.Fatalf("delete B: %d %s", w.Code, w.Body.String())
	}
	if w := do(r, "GET", "/api/v1/public/org-logo/"+orgB, "", "", nil, "", nil); w.Code != 404 {
		t.Fatalf("B should 404 after delete, got %d", w.Code)
	}
	if w := do(r, "GET", "/api/v1/public/org-logo/"+orgA, "", "", nil, "", nil); w.Code != 200 {
		t.Fatalf("A must still have its logo, got %d", w.Code)
	}

	// SVG logos are served with a locked-down CSP
	good := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>`)
	sb, _ = json.Marshal(map[string]string{"data_url": "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(good)})
	if w := do(r, "PUT", "/api/v1/organization/logo", orgB, "admin", sb, "application/json", nil); w.Code != 200 {
		t.Fatalf("svg upload: %d %s", w.Code, w.Body.String())
	}
	w = do(r, "GET", "/api/v1/public/org-logo/"+orgB, "", "", nil, "", nil)
	if w.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("svg headers: %v", w.Header())
	}
	db.Where("organization_id IN ?", []string{orgA, orgB}).Delete(&branding.OrgLogo{})
	db.Exec("DELETE FROM billing_settings WHERE organization_id IN ?", []string{orgA, orgB})
}

func TestBackfillMovesInvoicingLogo(t *testing.T) {
	db := testDB(t)
	orgC, orgD := "org-c-"+uuid.NewString(), "org-d-"+uuid.NewString()
	logo := pngOf(t, 16, 8, color.Black)
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(logo)
	if err := db.Create(&invoicing.Settings{OrganizationID: orgC, LogoURL: uri, Currency: "AUD"}).Error; err != nil {
		t.Fatal(err)
	}
	// org D already has a company logo: its old invoicing logo is not moved
	existing := pngOf(t, 4, 4, color.White)
	if _, err := branding.NewService(db).Save(orgD, "u", existing); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&invoicing.Settings{OrganizationID: orgD, LogoURL: uri, Currency: "AUD"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := branding.Backfill(db); err != nil {
		t.Fatal(err)
	}
	var l branding.OrgLogo
	if err := db.Where("organization_id = ?", orgC).Take(&l).Error; err != nil || !bytes.Equal(l.Data, logo) || l.ContentType != "image/png" {
		t.Fatalf("logo not moved: %v", err)
	}
	var s invoicing.Settings
	db.Where("organization_id = ?", orgC).Take(&s)
	if s.LogoURL != "" {
		t.Fatal("old invoicing logo should be cleared after the move")
	}
	var d branding.OrgLogo
	db.Where("organization_id = ?", orgD).Take(&d)
	if !bytes.Equal(d.Data, existing) {
		t.Fatal("backfill must not overwrite an existing company logo")
	}
	// idempotent: nothing more to move for C; deleting C's logo doesn't resurrect it
	branding.NewService(db).Delete(orgC)
	if _, err := branding.Backfill(db); err != nil {
		t.Fatal(err)
	}
	if m, _ := branding.NewService(db).Meta(orgC); m != nil {
		t.Fatal("removed logo came back after a second backfill")
	}
	db.Where("organization_id IN ?", []string{orgC, orgD}).Delete(&branding.OrgLogo{})
	db.Exec("DELETE FROM billing_settings WHERE organization_id IN ?", []string{orgC, orgD})
}
