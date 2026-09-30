package invoicing

import (
	"context"
	"os"
	"reflect"
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

// Invoices are sent through the organisation's saved email settings (pkg/mail):
// no env SMTP, "Always CC/BCC" merged with the document's CCs.
func TestSendUsesSavedEmailSettings(t *testing.T) {
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

	svc := mail.NewService(db)
	var sent []mail.Message
	var used []mail.Config
	svc.Transport = func(_ context.Context, cfg mail.Config, m mail.Message) error {
		sent, used = append(sent, m), append(used, cfg)
		return nil
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(db)
	h.Mail = svc
	h.RegisterRoutes(r.Group("/api", func(c *gin.Context) { c.Set("org_id", c.GetHeader("X-Org")); c.Set("user_id", "u1"); c.Next() }))

	item := []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": 100}}
	_, res := call(t, r, org, "POST", "/api/invoices", map[string]interface{}{"client_name": "Acme", "client_email": "client@x.test", "cc_emails": []string{"boss@x.test"}, "items": item})
	id := data(res)["id"].(string)

	// Nothing configured (unless a platform default exists in this DB).
	if _, src := svc.Resolve(org); src == mail.SourceNone {
		code, res := call(t, r, org, "GET", "/api/settings", nil)
		if code != 200 || data(res)["email_enabled"] != false {
			t.Fatalf("expected email disabled: %v", res)
		}
		if code, _ := call(t, r, org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email"}); code != 503 {
			t.Fatalf("expected 503, got %d", code)
		}
	}

	enc, _ := mail.EncryptSecret("pw")
	row := models.IntegrationSettings{ID: uuid.NewString(), OrganizationID: org, EmailSettings: models.EmailSettings{
		Enabled: true, SMTPHost: "smtp.gmail.com", SMTPPort: 587, SMTPSecurity: "tls", SMTPUsername: "shop@gmail.com", SMTPPassword: enc,
		FromName: "Shop", FromEmail: "shop@gmail.com", AlwaysCC: "accounts@x.test, boss@x.test", AlwaysBCC: "records@x.test"}}
	if err := db.Omit("Organization").Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	svc.Invalidate()

	code, res := call(t, r, org, "GET", "/api/settings", nil)
	if code != 200 || data(res)["email_enabled"] != true {
		t.Fatalf("expected email enabled: %v", res)
	}
	code, res = call(t, r, org, "GET", "/api/invoices/"+id+"/recipients", nil)
	d := data(res)
	if code != 200 || len(d["cc"].([]interface{})) != 2 || len(d["bcc"].([]interface{})) != 1 {
		t.Fatalf("recipients should include Always CC/BCC once: %v", d)
	}
	if s := d["sender"].(map[string]interface{}); s["from"] != "Shop <shop@gmail.com>" || s["source"] != "organisation" {
		t.Fatalf("sender = %v", s)
	}

	// No explicit cc/bcc: suggested + Always CC, Always BCC.
	code, res = call(t, r, org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email", "subject": "Invoice", "message": "Hi"})
	if code != 200 || data(res)["emailed"] != true || len(sent) != 1 {
		t.Fatalf("send: %d %v", code, res)
	}
	m := sent[0]
	if !reflect.DeepEqual(m.To, []string{"client@x.test"}) || strings.Join(m.CC, ",") != "boss@x.test,accounts@x.test" || strings.Join(m.BCC, ",") != "records@x.test" {
		t.Fatalf("recipients to=%v cc=%v bcc=%v", m.To, m.CC, m.BCC)
	}
	if used[0].FromEmail != "shop@gmail.com" || m.HTML == "" {
		t.Fatalf("config/HTML wrong: %+v", used[0])
	}
	// The document keeps its own CCs only (not the Always CC addresses).
	_, res = call(t, r, org, "GET", "/api/invoices/"+id, nil)
	if cc := data(res)["cc_emails"].([]interface{}); len(cc) != 1 || cc[0] != "boss@x.test" {
		t.Fatalf("cc_emails = %v", cc)
	}

	// Explicit lists from the dialog win; BCC never repeats To/CC.
	_, _ = call(t, r, org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email", "cc": []string{"x@x.test"}, "bcc": []string{"x@x.test", "client@x.test", "audit@x.test"}, "subject": "S", "message": "M"})
	m = sent[1]
	if strings.Join(m.CC, ",") != "x@x.test" || strings.Join(m.BCC, ",") != "audit@x.test" {
		t.Fatalf("explicit: cc=%v bcc=%v", m.CC, m.BCC)
	}
	if st := svc.Status(org); st.LastSuccessAt == nil {
		t.Errorf("last success not recorded: %+v", st)
	}
}
