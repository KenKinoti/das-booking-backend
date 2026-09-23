package invoicing

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Integration tests run against Postgres when TEST_DATABASE_URL is set, e.g.
// TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/das_test?sslmode=disable
func setup(t *testing.T) (*gin.Engine, string) {
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
	g := r.Group("/api", func(c *gin.Context) { c.Set("org_id", c.GetHeader("X-Org")); c.Set("user_id", "u1"); c.Next() })
	h.RegisterRoutes(g)
	h.RegisterPublicRoutes(r.Group("/public"))
	return r, org
}

func call(t *testing.T, r *gin.Engine, org, method, path string, body interface{}) (int, map[string]interface{}) {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Org", org)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func data(m map[string]interface{}) map[string]interface{} {
	d, _ := m["data"].(map[string]interface{})
	return d
}

func TestInvoiceLifecycle(t *testing.T) {
	r, org := setup(t)
	code, res := call(t, r, org, "POST", "/api/invoices", map[string]interface{}{
		"client_name": "Acme", "items": []map[string]interface{}{{"description": "Work", "quantity": 2, "unit_price": 100, "tax_rate": 10}},
	})
	if code != 201 {
		t.Fatalf("create: %d %v", code, res)
	}
	inv := data(res)
	id := inv["id"].(string)
	if inv["number"] != "INV-1001" || inv["total"].(float64) != 220 {
		t.Fatalf("unexpected invoice %v %v", inv["number"], inv["total"])
	}
	// numbers increment and are per organisation
	_, res2 := call(t, r, org, "POST", "/api/invoices", map[string]interface{}{"client_name": "B", "items": []map[string]interface{}{{"description": "x", "quantity": 1, "unit_price": 1}}})
	if data(res2)["number"] != "INV-1002" {
		t.Fatalf("expected INV-1002, got %v", data(res2)["number"])
	}
	// other organisations can't see it
	if code, _ := call(t, r, "other-org", "GET", "/api/invoices/"+id, nil); code != 404 {
		t.Fatalf("cross-org access should 404, got %d", code)
	}
	// drafts are not public
	token := inv["public_token"].(string)
	if code, _ := call(t, r, org, "GET", "/public/invoices/"+token, nil); code != 404 {
		t.Fatalf("draft should not be public, got %d", code)
	}
	if code, _ := call(t, r, org, "POST", "/api/invoices/"+id+"/mark-sent", nil); code != 200 {
		t.Fatal("mark sent failed")
	}
	if code, _ := call(t, r, org, "GET", "/public/invoices/"+token, nil); code != 200 {
		t.Fatalf("sent invoice should be public, got %d", code)
	}
	// overpayment rejected, partial then full
	if code, _ := call(t, r, org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 500}); code != 400 {
		t.Fatalf("overpayment should 400, got %d", code)
	}
	_, res = call(t, r, org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 20})
	if data(res)["status"] != "partial" {
		t.Fatalf("expected partial, got %v", data(res)["status"])
	}
	_, res = call(t, r, org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 200})
	if data(res)["status"] != "paid" || data(res)["balance_due"].(float64) != 0 {
		t.Fatalf("expected paid, got %v", data(res)["status"])
	}
	// paid invoices can't be deleted or voided
	if code, _ := call(t, r, org, "DELETE", "/api/invoices/"+id, nil); code != 409 {
		t.Fatalf("delete paid should 409, got %d", code)
	}
	if code, _ := call(t, r, org, "POST", "/api/invoices/"+id+"/void", nil); code != 409 {
		t.Fatalf("void with payments should 409, got %d", code)
	}
	// lowering total below paid is rejected
	if code, _ := call(t, r, org, "PUT", "/api/invoices/"+id, map[string]interface{}{"client_name": "Acme", "items": []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": 1}}}); code != 409 {
		t.Fatalf("edit below paid should 409, got %d", code)
	}
	code, res = call(t, r, org, "GET", "/api/stats", nil)
	if code != 200 || data(res)["paid_this_month"].(float64) != 220 {
		t.Fatalf("stats: %d %v", code, data(res)["paid_this_month"])
	}
}

func TestQuoteConvert(t *testing.T) {
	r, org := setup(t)
	_, res := call(t, r, org, "POST", "/api/invoices", map[string]interface{}{"doc_type": "quote", "client_name": "Q", "items": []map[string]interface{}{{"description": "Build", "quantity": 1, "unit_price": 1000, "tax_rate": 10}}})
	q := data(res)
	if q["number"] != "QUO-1001" {
		t.Fatalf("quote number %v", q["number"])
	}
	code, res := call(t, r, org, "POST", "/api/invoices/"+q["id"].(string)+"/convert", nil)
	if code != 201 || data(res)["doc_type"] != "invoice" || data(res)["total"].(float64) != 1100 {
		t.Fatalf("convert: %d %v", code, res)
	}
	if code, _ := call(t, r, org, "POST", "/api/invoices/"+q["id"].(string)+"/convert", nil); code != 409 {
		t.Fatalf("second convert should 409, got %d", code)
	}
}

func TestValidation(t *testing.T) {
	r, org := setup(t)
	if code, _ := call(t, r, org, "POST", "/api/invoices", map[string]interface{}{"client_name": "", "items": []map[string]interface{}{{"description": "x", "quantity": 1, "unit_price": 1}}}); code != 400 {
		t.Fatal("missing client should 400")
	}
	if code, _ := call(t, r, org, "POST", "/api/invoices", map[string]interface{}{"client_name": "x"}); code != 400 {
		t.Fatal("missing items should 400")
	}
	if code, _ := call(t, r, org, "POST", "/api/invoices", map[string]interface{}{"client_name": "x", "issue_date": "2026-02-10", "due_date": "2026-02-01", "items": []map[string]interface{}{{"description": "x", "quantity": 1, "unit_price": 1}}}); code != 400 {
		t.Fatal("due before issue should 400")
	}
}
