package invoicing

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type flakyMailer struct {
	fakeMailer
	fail bool
}

func (f *flakyMailer) SendWithCC(to string, cc []string, subject, body string) error {
	if f.fail {
		return errors.New("535 5.7.8 Username and Password not accepted")
	}
	return f.fakeMailer.SendWithCC(to, cc, subject, body)
}
func (f *flakyMailer) Send(to, subject, body string) error {
	return f.SendWithCC(to, nil, subject, body)
}

func TestEmailStatusSummary(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	org := "test-org-" + uuid.NewString()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(db)
	fm := &flakyMailer{fakeMailer: fakeMailer{configured: true}}
	h.Mailer = fm
	h.RegisterRoutes(r.Group("/api", func(c *gin.Context) { c.Set("org_id", c.GetHeader("X-Org")); c.Set("user_id", "u1"); c.Next() }))
	item := []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": 100, "tax_rate": 0}}
	_, res := call(t, r, org, "POST", "/api/invoices", map[string]interface{}{"client_name": "Amina", "client_email": "amina@example.co.ke",
		"cc_emails": []string{"acc@example.co.ke", "boss@example.co.ke"}, "items": item})
	id := data(res)["id"].(string)

	status := func() map[string]interface{} {
		_, res := call(t, r, org, "GET", "/api/invoices/"+id, nil)
		st, _ := data(res)["email_status"].(map[string]interface{})
		return st
	}
	if st := status(); st["state"] != "none" || st["sent"].(float64) != 0 {
		t.Fatalf("before sending %v", st)
	}
	if code, res := call(t, r, org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email"}); code != 200 {
		t.Fatalf("send %d %v", code, res)
	}
	st := status()
	if st["state"] != "sent" || st["sent"].(float64) != 1 || st["last_to"] != "amina@example.co.ke" || st["last_cc"].(float64) != 2 || st["last_kind"] != "invoice" || st["last_sent_at"] == nil {
		t.Fatalf("after send %v", st)
	}
	// A failed reminder: the chip says the last email failed (with the SMTP error).
	fm.fail = true
	call(t, r, org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email", "kind": "reminder", "subject": "Reminder", "message": "Pay"})
	st = status()
	if st["state"] != "failed" || st["failed"].(float64) != 1 || st["sent"].(float64) != 1 || st["last_error"] == "" {
		t.Fatalf("after failure %v", st)
	}
	// Retry works; the client then opens the link.
	fm.fail = false
	call(t, r, org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email", "kind": "reminder", "subject": "Reminder", "message": "Pay"})
	db.Model(&Invoice{}).Where("id = ?", id).Update("viewed_at", time.Now().Add(time.Minute))
	st = status()
	if st["state"] != "sent" || st["sent"].(float64) != 2 || st["viewed_after_last_send"] != true || st["by_kind"].(map[string]interface{})["reminder"].(float64) != 1 {
		t.Fatalf("after retry %v", st)
	}
	// The list carries it too, and the log has the summary and the time zone.
	_, res = call(t, r, org, "GET", "/api/invoices?type=invoice", nil)
	list := data(res)["invoices"].([]interface{})
	if es := list[0].(map[string]interface{})["email_status"].(map[string]interface{}); es["sent"].(float64) != 2 {
		t.Fatalf("list %v", es)
	}
	_, res = call(t, r, org, "GET", "/api/invoices/"+id+"/email-log", nil)
	if d := data(res); len(d["log"].([]interface{})) != 3 || d["summary"] == nil || d["timezone"] == "" {
		t.Fatalf("log %v", d)
	}
}
