package payments

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// ---------------------------------------------------------------- fake Flutterwave

type fakeTx struct {
	ID       int64
	TxRef    string
	Amount   float64
	Currency string
	Status   string
	AppFee   float64
	Option   string
}

type fakeFlw struct {
	mu      sync.Mutex
	secret  string
	created []PaymentRequest
	txs     map[int64]*fakeTx
	next    int64
	calls   int
}

func newFakeFlw(secret string) (*fakeFlw, *httptest.Server) {
	f := &fakeFlw{secret: secret, txs: map[int64]*fakeTx{}, next: 9000}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	return f, srv
}

func (f *fakeFlw) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+f.secret {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"status":"error","message":"Invalid authorization key","data":null}`))
		return
	}
	send := func(v interface{}) {
		b, _ := json.Marshal(map[string]interface{}{"status": "success", "message": "ok", "data": v})
		_, _ = w.Write(b)
	}
	txJSON := func(t *fakeTx) map[string]interface{} {
		return map[string]interface{}{"id": t.ID, "tx_ref": t.TxRef, "flw_ref": fmt.Sprintf("FLW-MOCK-%d", t.ID), "amount": t.Amount,
			"charged_amount": t.Amount, "app_fee": t.AppFee, "currency": t.Currency, "status": t.Status, "payment_type": t.Option}
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/v3/payments":
		var req PaymentRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.created = append(f.created, req)
		send(map[string]string{"link": "https://checkout.fake/pay/" + req.TxRef})
	case r.Method == "GET" && r.URL.Path == "/v3/balances":
		send([]map[string]interface{}{{"currency": "KES", "available_balance": 0}})
	case r.Method == "GET" && r.URL.Path == "/v3/transactions/verify_by_reference":
		ref := r.URL.Query().Get("tx_ref")
		for _, t := range f.txs {
			if t.TxRef == ref {
				send(txJSON(t))
				return
			}
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"status":"error","message":"No transaction was found for this id","data":null}`))
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v3/transactions/") && strings.HasSuffix(r.URL.Path, "/verify"):
		var id int64
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/v3/transactions/"), "%d", &id)
		if t := f.txs[id]; t != nil {
			send(txJSON(t))
			return
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"status":"error","message":"No transaction was found for this id","data":null}`))
	default:
		w.WriteHeader(404)
	}
}

// pay simulates the customer completing the hosted checkout.
func (f *fakeFlw) pay(txRef string, amount float64, cur, status string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	opt := ""
	for _, c := range f.created {
		if c.TxRef == txRef {
			opt = c.PaymentOptions
		}
	}
	f.txs[f.next] = &fakeTx{ID: f.next, TxRef: txRef, Amount: amount, Currency: cur, Status: status, AppFee: amount * 0.029, Option: opt}
	return f.next
}

// ---------------------------------------------------------------- setup

func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`CREATE SCHEMA IF NOT EXISTS paypkg_test`)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=paypkg_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := invoicing.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

type env struct {
	db    *gorm.DB
	flw   *fakeFlw
	srv   *httptest.Server
	r     *gin.Engine
	h     *Handler
	org   string
	token string
	invID string
}

func setup(t *testing.T, partial bool, bearer string) *env {
	t.Setenv("ENCRYPTION_KEY", "pay-test-key")
	db := testDB(t)
	secret := "FLWSECK_TEST-abc123def456-X"
	flw, srv := newFakeFlw(secret)
	t.Cleanup(srv.Close)
	t.Setenv("FLUTTERWAVE_BASE_URL", srv.URL)
	org := "org-" + uuid.NewString()
	e := &env{db: db, flw: flw, srv: srv, org: org}
	enc := func(s string) string { v, _ := mail.EncryptSecret(s); return v }
	st := DefaultSettings(org)
	st.Enabled, st.PublicKey, st.SecretKey, st.WebhookHash = true, "FLWPUBK_TEST-xyz-X", enc(secret), enc("hush-123")
	st.AllowPartial, st.MinPartial, st.FeeBearer = partial, 1000, bearer
	if err := db.Create(&st).Error; err != nil {
		t.Fatal(err)
	}
	e.token = "tok" + strings.ReplaceAll(uuid.NewString(), "-", "")
	e.invID = uuid.NewString()
	inv := invoicing.Invoice{ID: e.invID, OrganizationID: org, DocType: invoicing.TypeInvoice, Number: "INV-1005", Status: invoicing.StatusSent,
		ClientName: "Wanjiku", ClientEmail: "w@x.test", Currency: "KES", IssueDate: time.Now(), DueDate: time.Now().AddDate(0, 0, 14),
		Subtotal: 10000, Total: 10000, BalanceDue: 10000, PublicToken: e.token}
	if err := db.Create(&inv).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("organization_id = ?", org).Delete(&Transaction{})
		db.Where("organization_id = ?", org).Delete(&Settings{})
		db.Where("organization_id = ?", org).Delete(&invoicing.Payment{})
		db.Where("organization_id = ?", org).Delete(&invoicing.Activity{})
		db.Unscoped().Where("organization_id = ?", org).Delete(&invoicing.Invoice{})
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(db)
	h.Async = false
	h.PublicURL = "https://app.test"
	h.RegisterPublic(r.Group("/public"))
	h.RegisterRoutes(r.Group("/api", func(c *gin.Context) {
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_role", c.GetHeader("X-Role"))
		c.Set("user_id", "u1")
		c.Next()
	}))
	e.r, e.h = r, h
	return e
}

func (e *env) call(t *testing.T, method, path string, body interface{}, hdr map[string]string) (int, map[string]interface{}) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Org", e.org)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if d, ok := out["data"].(map[string]interface{}); ok {
		return w.Code, d
	}
	return w.Code, out
}

func (e *env) invoice(t *testing.T) invoicing.Invoice {
	var inv invoicing.Invoice
	if err := e.db.Preload("Payments").Where("id = ?", e.invID).First(&inv).Error; err != nil {
		t.Fatal(err)
	}
	return inv
}

// ---------------------------------------------------------------- tests

func TestPayFlowIdempotent(t *testing.T) {
	e := setup(t, false, BearerBusiness)
	code, page := e.call(t, "GET", "/public/pay/"+e.token, nil, nil)
	if code != 200 || page["payable"] != true {
		t.Fatalf("pay page %d %v", code, page)
	}
	opts := page["options"].([]interface{})
	first := opts[0].(map[string]interface{})
	if first["method"] != "mpesa" || first["recommended"] != true {
		t.Fatalf("M-Pesa should be recommended for KES: %v", opts)
	}
	code, co := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "mpesa"}, nil)
	if code != 200 || !strings.HasPrefix(co["link"].(string), "https://checkout.fake/pay/") {
		t.Fatalf("checkout %d %v", code, co)
	}
	req := e.flw.created[0]
	if req.PaymentOptions != "mpesa" || req.Amount != 10000 || req.Currency != "KES" || req.RedirectURL != "https://app.test/pay/"+e.token+"/return" || req.Customer.Email != "w@x.test" {
		t.Fatalf("payment request %+v", req)
	}
	if !strings.Contains(req.TxRef, e.invID) {
		t.Fatalf("tx_ref should carry the invoice id: %s", req.TxRef)
	}
	ref := co["tx_ref"].(string)
	id := e.flw.pay(ref, 10000, "KES", "successful")

	code, v := e.call(t, "POST", "/public/pay/"+e.token+"/verify", map[string]interface{}{"tx_ref": ref, "transaction_id": fmt.Sprint(id), "status": "successful"}, nil)
	if code != 200 || v["status"] != TxSuccessful || v["receipt_path"] == "" {
		t.Fatalf("verify %d %v", code, v)
	}
	inv := e.invoice(t)
	if inv.Status != invoicing.StatusPaid || inv.BalanceDue != 0 || len(inv.Payments) != 1 || inv.Payments[0].Method != "Flutterwave · M-Pesa" {
		t.Fatalf("invoice after payment: %s bal=%v payments=%+v", inv.Status, inv.BalanceDue, inv.Payments)
	}
	// Redirect again, webhook, verify again: still one payment.
	e.call(t, "POST", "/public/pay/"+e.token+"/verify", map[string]interface{}{"tx_ref": ref, "transaction_id": fmt.Sprint(id)}, nil)
	body := map[string]interface{}{"event": "charge.completed", "data": map[string]interface{}{"id": id, "tx_ref": ref, "status": "successful", "amount": 10000}}
	if code, _ := e.call(t, "POST", "/public/payments/flutterwave/webhook", body, map[string]string{"verif-hash": "hush-123"}); code != 200 {
		t.Fatalf("webhook %d", code)
	}
	if n := len(e.invoice(t).Payments); n != 1 {
		t.Fatalf("payment recorded %d times", n)
	}
	var tx Transaction
	e.db.Where("tx_ref = ?", ref).Take(&tx)
	if tx.AppFee != 290 || !tx.FeeCharged || tx.FlwTransactionID != fmt.Sprint(id) || tx.InvoicePaymentID == "" {
		t.Fatalf("transaction %+v", tx)
	}
	// Paid invoice: nothing more to pay.
	code, page = e.call(t, "GET", "/public/pay/"+e.token, nil, nil)
	if page["payable"] != false {
		t.Fatalf("paid invoice still payable: %v", page)
	}
	if code, _ := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "mpesa"}, nil); code != 409 {
		t.Fatalf("checkout on a paid invoice: %d", code)
	}
}

func TestVerifyMismatchesNotRecorded(t *testing.T) {
	cases := []struct {
		name, cur, status string
		amount            float64
	}{
		{"short amount", "KES", "successful", 9000},
		{"wrong currency", "USD", "successful", 10000},
		{"failed", "KES", "failed", 10000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, false, BearerBusiness)
			_, co := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "card"}, nil)
			ref := co["tx_ref"].(string)
			id := e.flw.pay(ref, c.amount, c.cur, c.status)
			code, v := e.call(t, "POST", "/public/pay/"+e.token+"/verify", map[string]interface{}{"tx_ref": ref, "transaction_id": fmt.Sprint(id)}, nil)
			if code != 200 || v["status"] != TxFailed || v["reason"] == "" {
				t.Fatalf("verify %d %v", code, v)
			}
			if inv := e.invoice(t); len(inv.Payments) != 0 || inv.BalanceDue != 10000 {
				t.Fatalf("must not record: %+v", inv.Payments)
			}
		})
	}
	// A transaction id that belongs to another tx_ref is ignored.
	e := setup(t, false, BearerBusiness)
	_, co := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "mpesa"}, nil)
	other := e.flw.pay("someone-else", 10000, "KES", "successful")
	_, v := e.call(t, "POST", "/public/pay/"+e.token+"/verify", map[string]interface{}{"tx_ref": co["tx_ref"], "transaction_id": fmt.Sprint(other)}, nil)
	if v["status"] != TxPending || len(e.invoice(t).Payments) != 0 {
		t.Fatalf("foreign transaction: %v", v)
	}
}

func TestWebhookHashAndReverify(t *testing.T) {
	e := setup(t, false, BearerBusiness)
	_, co := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "mpesa"}, nil)
	ref := co["tx_ref"].(string)
	id := e.flw.pay(ref, 10000, "KES", "successful")
	body := map[string]interface{}{"event": "charge.completed", "data": map[string]interface{}{"id": id, "tx_ref": ref, "status": "successful"}}
	if code, _ := e.call(t, "POST", "/public/payments/flutterwave/webhook", body, map[string]string{"verif-hash": "wrong"}); code != 401 {
		t.Fatalf("bad hash accepted: %d", code)
	}
	if code, _ := e.call(t, "POST", "/public/payments/flutterwave/webhook", body, nil); code != 401 {
		t.Fatalf("missing hash accepted: %d", code)
	}
	if len(e.invoice(t).Payments) != 0 {
		t.Fatal("recorded without a valid hash")
	}
	// A webhook claiming success for an unpaid transaction is re-verified and ignored.
	_, co2 := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "card"}, nil)
	fake := map[string]interface{}{"event": "charge.completed", "data": map[string]interface{}{"tx_ref": co2["tx_ref"], "status": "successful", "amount": 10000}}
	if code, _ := e.call(t, "POST", "/public/payments/flutterwave/webhook", fake, map[string]string{"verif-hash": "hush-123"}); code != 200 {
		t.Fatalf("webhook %d", code)
	}
	if len(e.invoice(t).Payments) != 0 {
		t.Fatal("webhook body was trusted")
	}
	// Valid webhook records the payment.
	if code, _ := e.call(t, "POST", "/public/payments/flutterwave/webhook", body, map[string]string{"verif-hash": "hush-123"}); code != 200 {
		t.Fatalf("webhook %d", code)
	}
	if inv := e.invoice(t); len(inv.Payments) != 1 || inv.Status != invoicing.StatusPaid {
		t.Fatalf("webhook did not record: %+v", inv.Payments)
	}
	// Unknown tx_ref: acknowledged, ignored.
	if code, _ := e.call(t, "POST", "/public/payments/flutterwave/webhook", map[string]interface{}{"event": "charge.completed", "data": map[string]interface{}{"tx_ref": "nope"}}, map[string]string{"verif-hash": "x"}); code != 200 {
		t.Fatalf("unknown ref: %d", code)
	}
}

func TestPartialPaymentAndSurcharge(t *testing.T) {
	e := setup(t, true, BearerCustomer)
	if code, _ := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "mpesa", "amount": 500}, nil); code != 400 {
		t.Fatalf("below minimum accepted: %d", code)
	}
	if code, _ := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "mpesa", "amount": 20000}, nil); code != 400 {
		t.Fatalf("overpayment accepted: %d", code)
	}
	_, page := e.call(t, "GET", "/public/pay/"+e.token+"?amount=4000", nil, nil)
	opt := page["options"].([]interface{})[0].(map[string]interface{})
	if page["amount"].(float64) != 4000 || opt["surcharge"].(float64) <= 0 {
		t.Fatalf("partial options %v", page)
	}
	_, co := e.call(t, "POST", "/public/pay/"+e.token+"/checkout", map[string]interface{}{"method": "mpesa", "amount": 4000}, nil)
	charge := co["amount"].(float64)
	if charge <= 4000 {
		t.Fatalf("surcharge not added: %v", charge)
	}
	id := e.flw.pay(co["tx_ref"].(string), charge, "KES", "successful")
	_, v := e.call(t, "POST", "/public/pay/"+e.token+"/verify", map[string]interface{}{"tx_ref": co["tx_ref"], "transaction_id": fmt.Sprint(id)}, nil)
	if v["status"] != TxSuccessful {
		t.Fatalf("verify %v", v)
	}
	inv := e.invoice(t)
	if inv.Status != invoicing.StatusPartial || inv.BalanceDue != 6000 || inv.Payments[0].Amount != 4000 {
		t.Fatalf("partial: %s %v %+v", inv.Status, inv.BalanceDue, inv.Payments)
	}
}

func TestSettingsSecretsMasked(t *testing.T) {
	e := setup(t, false, BearerBusiness)
	admin := map[string]string{"X-Role": "admin"}
	if code, _ := e.call(t, "PUT", "/api/payments/settings", map[string]interface{}{"enabled": true}, map[string]string{"X-Role": "staff"}); code != 403 {
		t.Fatalf("staff can edit: %d", code)
	}
	code, v := e.call(t, "PUT", "/api/payments/settings", map[string]interface{}{"secret_key": "FLWSECK_TEST-newsecret999-X", "webhook_hash": "", "fee_bearer": "customer"}, admin)
	if code != 200 {
		t.Fatalf("save %d %v", code, v)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "newsecret999") || strings.Contains(string(raw), "hush-123") {
		t.Fatalf("secret leaked: %s", raw)
	}
	if sk := v["secret_key"].(map[string]interface{}); sk["set"] != true || !strings.HasSuffix(sk["hint"].(string), "99-X") {
		t.Fatalf("mask %v", sk)
	}
	if wh := v["webhook_hash"].(map[string]interface{}); wh["set"] != true {
		t.Fatal("empty input must keep the stored hash")
	}
	var st Settings
	e.db.Where("organization_id = ?", e.org).Take(&st)
	if !mail.IsEncrypted(st.SecretKey) || Secret(st.SecretKey) != "FLWSECK_TEST-newsecret999-X" {
		t.Fatal("secret not encrypted at rest")
	}
	// Live key in test mode is refused.
	if code, v := e.call(t, "PUT", "/api/payments/settings", map[string]interface{}{"secret_key": "FLWSECK-livekey-X", "mode": "test"}, admin); code != 400 {
		t.Fatalf("mode mismatch accepted: %v", v)
	}
	// Test connection against the fake API.
	e.call(t, "PUT", "/api/payments/settings", map[string]interface{}{"secret_key": "FLWSECK_TEST-abc123def456-X"}, admin)
	if code, v := e.call(t, "POST", "/api/payments/settings/test", nil, admin); code != 200 || v["ok"] != true {
		t.Fatalf("test connection %d %v", code, v)
	}
	if code, _ := e.call(t, "POST", "/api/payments/settings/test", map[string]string{"secret_key": "FLWSECK_TEST-wrong-X"}, admin); code != 502 {
		t.Fatalf("wrong key should fail: %d", code)
	}
}
