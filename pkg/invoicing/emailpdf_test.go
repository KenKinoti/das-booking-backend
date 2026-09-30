package invoicing

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Invoice / quote / reminder emails carry the PDF and the View / Pay links.
func TestEmailHasPDFAndLinks(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	t.Setenv("SMTP_HOST", "")
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.IntegrationSettings{}, &mail.SendStatus{}); err != nil {
		t.Fatal(err)
	}
	org := "test-org-" + uuid.NewString()
	t.Cleanup(func() { db.Unscoped().Where("organization_id = ?", org).Delete(&models.IntegrationSettings{}) })
	enc, _ := mail.EncryptSecret("pw")
	row := models.IntegrationSettings{ID: uuid.NewString(), OrganizationID: org, EmailSettings: models.EmailSettings{
		Enabled: true, SMTPHost: "smtp.x.test", SMTPPort: 587, SMTPSecurity: "tls", SMTPUsername: "u@x.test", SMTPPassword: enc, FromEmail: "shop@x.test"}}
	if err := db.Omit("Organization").Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	svc := mail.NewService(db)
	var sent []mail.Message
	svc.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error { sent = append(sent, m); return nil }

	payOn := true
	OnlinePay = func(*gorm.DB, string) bool { return payOn }
	t.Cleanup(func() { OnlinePay = nil })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(db)
	h.Mail = svc
	h.PublicURL = "https://app.example.com"
	h.RegisterRoutes(r.Group("/api", func(c *gin.Context) { c.Set("org_id", c.GetHeader("X-Org")); c.Set("user_id", "u1"); c.Next() }))
	h.RegisterPublicRoutes(r.Group("/public"))

	item := []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": 1000}}
	_, res := call(t, r, org, "POST", "/api/invoices", map[string]interface{}{"client_name": "Acme", "client_email": "client@x.test", "currency": "KES", "items": item})
	inv := data(res)
	id, num := inv["id"].(string), inv["number"].(string)
	tok := inv["public_token"].(string)

	// Empty subject / message: defaults from the settings are used.
	code, res := call(t, r, org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email"})
	if code != 200 || len(sent) != 1 {
		t.Fatalf("send: %d %v", code, res)
	}
	m := sent[0]
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != num+".pdf" || m.Attachments[0].ContentType != "application/pdf" || !bytes.HasPrefix(m.Attachments[0].Data, []byte("%PDF-")) {
		t.Fatalf("PDF attachment missing: %+v", m.Attachments)
	}
	view, pay := "https://app.example.com/i/"+tok, "https://app.example.com/pay/"+tok
	for _, part := range []string{m.HTML, m.Text} {
		if !strings.Contains(part, view) || !strings.Contains(part, pay) {
			t.Fatalf("links missing in:\n%s", part)
		}
	}
	if !strings.Contains(m.HTML, ">View invoice</a>") || !strings.Contains(m.HTML, ">Pay now</a>") || !strings.Contains(m.Text, num+".pdf") {
		t.Fatalf("buttons / note missing: %s", m.HTML)
	}
	if m.Subject == "" || !strings.Contains(m.Subject, num) {
		t.Fatalf("default subject: %q", m.Subject)
	}
	// The PDF itself carries the pay link.
	if !bytes.Contains(m.Attachments[0].Data, []byte(pay)) {
		t.Fatal("pay link missing from the PDF")
	}

	// Reminder: attachment + pay link too.
	call(t, r, org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email", "kind": "reminder", "subject": "R", "message": "Please pay"})
	if m := sent[1]; len(m.Attachments) != 1 || !strings.Contains(m.HTML, pay) {
		t.Fatalf("reminder: %+v", m.Attachments)
	}

	// Online payments off: no Pay button.
	payOn = false
	call(t, r, org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email", "subject": "S", "message": "M"})
	if m := sent[2]; strings.Contains(m.HTML, "/pay/") || !strings.Contains(m.HTML, view) {
		t.Fatalf("pay link shown while payments are off")
	}
	payOn = true

	// Quote: View & accept, no Pay.
	_, res = call(t, r, org, "POST", "/api/invoices", map[string]interface{}{"doc_type": "quote", "client_name": "Acme", "client_email": "client@x.test", "items": item})
	q := data(res)
	call(t, r, org, "POST", "/api/invoices/"+q["id"].(string)+"/send", map[string]interface{}{"mode": "email", "subject": "Q", "message": "Quote"})
	if m := sent[3]; !strings.Contains(m.HTML, "View &amp; accept quote") || strings.Contains(m.HTML, "/pay/") || m.Attachments[0].Filename != q["number"].(string)+".pdf" {
		t.Fatalf("quote email wrong: %s", m.HTML)
	}

	// PDF endpoints: authenticated and public.
	for _, p := range []string{"/api/invoices/" + id + "/pdf", "/public/invoices/" + tok + "/pdf?download=1"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("X-Org", org)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/pdf" || !bytes.HasPrefix(w.Body.Bytes(), []byte("%PDF-")) {
			t.Fatalf("%s: %d %s", p, w.Code, w.Header().Get("Content-Type"))
		}
		if !strings.Contains(w.Header().Get("Content-Disposition"), num+".pdf") {
			t.Fatalf("filename: %s", w.Header().Get("Content-Disposition"))
		}
	}
	// Another organisation can't download it.
	req := httptest.NewRequest("GET", "/api/invoices/"+id+"/pdf", nil)
	req.Header.Set("X-Org", "someone-else")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("cross-org PDF: %d", w.Code)
	}
}
