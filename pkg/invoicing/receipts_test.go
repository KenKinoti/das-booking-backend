package invoicing

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// ---------------------------------------------------------------- words

func TestAmountInWords(t *testing.T) {
	cases := []struct {
		v    float64
		cur  string
		want string
	}{
		{7500, "KES", "Seven thousand five hundred Kenyan shillings only"},
		{7500.5, "KES", "Seven thousand five hundred Kenyan shillings and fifty cents only"},
		{1, "KES", "One Kenyan shilling only"},
		{0.01, "USD", "Zero US dollars and one cent only"},
		{1234.56, "JPY", "One thousand two hundred and thirty-five Japanese yen only"}, // 0 decimals: rounded
		{1.25, "KWD", "One Kuwaiti dinar and two hundred and fifty fils only"},
		{12.005, "KWD", "Twelve Kuwaiti dinars and five fils only"},
		{1000005, "UGX", "One million and five Ugandan shillings only"},
		{2500000.75, "NGN", "Two million five hundred thousand Nigerian naira and seventy-five kobo only"},
		{115, "GBP", "One hundred and fifteen pounds sterling only"},
	}
	for _, c := range cases {
		if got := AmountInWords(c.v, c.cur); got != c.want {
			t.Errorf("AmountInWords(%v, %s) = %q, want %q", c.v, c.cur, got, c.want)
		}
	}
	if got := NumberInWords(1105); got != "one thousand one hundred and five" {
		t.Errorf("1105 → %q", got)
	}
	if got := NumberInWords(21000019); got != "twenty-one million and nineteen" {
		t.Errorf("21000019 → %q", got)
	}
}

// ---------------------------------------------------------------- helpers

type rcEnv struct {
	mailEnv
}

func newInvoice(t *testing.T, e mailEnv, cur string, total float64, extra map[string]interface{}) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{"client_name": "Wanjiru Ltd", "client_email": "billing@wanjiru.test", "currency": cur, "status": "sent",
		"items": []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": total, "tax_rate": 0}}}
	for k, v := range extra {
		body[k] = v
	}
	code, res := call(t, e.r, e.org, "POST", "/api/invoices", body)
	if code != 201 {
		t.Fatalf("create invoice: %d %v", code, res)
	}
	return data(res)
}

func payments(inv map[string]interface{}) []map[string]interface{} {
	out := []map[string]interface{}{}
	list, _ := inv["payments"].([]interface{})
	for _, p := range list {
		out = append(out, p.(map[string]interface{}))
	}
	return out
}

func errCode(res map[string]interface{}) string {
	e, _ := res["error"].(map[string]interface{})
	s, _ := e["code"].(string)
	return s
}

func errMsg(res map[string]interface{}) string {
	e, _ := res["error"].(map[string]interface{})
	s, _ := e["message"].(string)
	return s
}

// ---------------------------------------------------------------- edit

func TestEditPaymentValidationAndStatus(t *testing.T) {
	e := setupMailEnv(t)
	inv := newInvoice(t, e, "KES", 10000, nil)
	id := inv["id"].(string)

	// Overpayment on record is blocked with a clear message.
	code, res := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 10001, "method": "cash"})
	if code != 400 || errCode(res) != "OVERPAYMENT" || !strings.Contains(errMsg(res), "KSh 10,000.00") {
		t.Fatalf("overpayment should be blocked: %d %v", code, res)
	}
	// Too many decimals, future date and unknown methods are rejected.
	for _, body := range []map[string]interface{}{
		{"amount": 10.001, "method": "cash"},
		{"amount": 10, "method": "cash", "date": time.Now().AddDate(0, 0, 5).Format("2006-01-02")},
		{"amount": 10, "method": "barter"},
		{"amount": 0, "method": "cash"},
	} {
		if code, res := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/payments", body); code != 400 {
			t.Fatalf("payment %v should be rejected: %d %v", body, code, res)
		}
	}
	// A custom part payment.
	code, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 5000, "method": "mpesa", "reference": "QK1", "date": "2026-09-01"})
	inv = data(res)
	if code != 200 || inv["status"] != StatusPartial || num(inv["balance_due"]) != 5000 || inv["last_payment_id"] == nil {
		t.Fatalf("part payment: %d %v", code, res)
	}
	ps := payments(inv)
	pid := ps[0]["id"].(string)
	if ps[0]["method"] != "mobile_money" || ps[0]["receipt_number"] == "" || ps[0]["origin"] != OriginManual {
		t.Fatalf("payment fields: %v", ps[0])
	}
	rn := ps[0]["receipt_number"].(string)
	put := func(body map[string]interface{}) (int, map[string]interface{}) {
		return call(t, e.r, e.org, "PUT", "/api/invoices/"+id+"/payments/"+pid, body)
	}
	// partial → partial with a reason, logged with old → new amounts.
	code, res = put(map[string]interface{}{"amount": 7500, "reason": "typo in amount"})
	inv = data(res)["invoice"].(map[string]interface{})
	if code != 200 || inv["status"] != StatusPartial || num(inv["amount_paid"]) != 7500 || num(inv["balance_due"]) != 2500 {
		t.Fatalf("edit to 7500: %d %v", code, res)
	}
	act := inv["activities"].([]interface{})[0].(map[string]interface{})
	want := "Payment edited (" + rn + "): KSh 5,000.00 → KSh 7,500.00 (typo in amount)"
	if act["type"] != "payment_edited" || act["message"] != want || act["user_id"] != "u1" {
		t.Fatalf("activity = %v, want %q", act, want)
	}
	p := payments(inv)[0]
	if p["receipt_number"] != rn || p["revised_at"] == nil || p["revised_by"] != "u1" {
		t.Fatalf("receipt number must be kept and the revision recorded: %v", p)
	}
	// partial → paid → partial → (delete) sent
	if code, res = put(map[string]interface{}{"amount": 10000}); code != 200 || data(res)["invoice"].(map[string]interface{})["status"] != StatusPaid {
		t.Fatalf("edit to full: %d %v", code, res)
	}
	if code, res = put(map[string]interface{}{"amount": 2000, "date": "2026-09-03", "method": "bank_transfer", "reference": "EFT-9"}); code != 200 {
		t.Fatalf("edit down: %d %v", code, res)
	}
	inv = data(res)["invoice"].(map[string]interface{})
	if inv["status"] != StatusPartial || inv["paid_at"] != nil || num(inv["balance_due"]) != 8000 {
		t.Fatalf("paid → partial: %v %v %v", inv["status"], inv["paid_at"], inv["balance_due"])
	}
	p = payments(inv)[0]
	if !strings.HasPrefix(p["date"].(string), "2026-09-03") || p["method"] != "bank_transfer" || p["reference"] != "EFT-9" {
		t.Fatalf("fields not saved: %v", p)
	}
	// Validation on edit.
	for _, c := range []struct {
		body map[string]interface{}
		code string
	}{
		{map[string]interface{}{"amount": 0}, "VALIDATION_ERROR"},
		{map[string]interface{}{"amount": -5}, "VALIDATION_ERROR"},
		{map[string]interface{}{"amount": 12000}, "OVERPAYMENT"},
		{map[string]interface{}{"amount": 1.234}, "VALIDATION_ERROR"},
		{map[string]interface{}{"date": time.Now().AddDate(0, 0, 3).Format("2006-01-02")}, "VALIDATION_ERROR"},
		{map[string]interface{}{"method": "barter"}, "VALIDATION_ERROR"},
	} {
		if code, res := put(c.body); code < 400 || errCode(res) != c.code {
			t.Fatalf("edit %v should fail with %s: %d %v", c.body, c.code, code, res)
		}
	}
	// A second payment: the sum of payments can't exceed the total.
	code, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 3000, "method": "cash"})
	if code != 200 {
		t.Fatalf("second payment: %d %v", code, res)
	}
	if code, res = put(map[string]interface{}{"amount": 7001}); code != 400 || !strings.Contains(errMsg(res), "KSh 7,000.00") {
		t.Fatalf("sum above total should be blocked naming the max: %d %v", code, res)
	}
	if code, res = put(map[string]interface{}{"amount": 7000}); code != 200 || data(res)["invoice"].(map[string]interface{})["status"] != StatusPaid {
		t.Fatalf("edit to the exact remainder: %d %v", code, res)
	}
	// No change: no activity, changed=false.
	if code, res = put(map[string]interface{}{"amount": 7000}); code != 200 || data(res)["changed"] != false {
		t.Fatalf("no-op edit: %d %v", code, res)
	}
	// Delete everything → back to sent; the receipt link shows "Cancelled".
	inv = data(res)["invoice"].(map[string]interface{})
	for _, p := range payments(inv) {
		if code, res := call(t, e.r, e.org, "DELETE", "/api/invoices/"+id+"/payments/"+p["id"].(string)+"?reason=entered+twice", nil); code != 200 {
			t.Fatalf("delete: %d %v", code, res)
		}
	}
	_, res = call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	if data(res)["status"] != StatusSent || num(data(res)["amount_paid"]) != 0 {
		t.Fatalf("after deleting payments: %v", data(res)["status"])
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, httptest.NewRequest("GET", "/public/invoices/"+inv["public_token"].(string)+"/receipts/"+pid, nil))
	if w.Code != 200 {
		t.Fatalf("cancelled receipt link should still work: %d %s", w.Code, w.Body.String())
	}
	txt := pdfText(t, w.Body.Bytes())
	for _, s := range []string{"CANCELLED", rn, "entered twice", "no longer valid"} {
		if !strings.Contains(txt, s) {
			t.Fatalf("cancelled receipt missing %q:\n%s", s, txt)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), rn+".pdf") {
		t.Fatalf("filename: %s", w.Header().Get("Content-Disposition"))
	}
}

func TestEditLockedPayments(t *testing.T) {
	e := setupMailEnv(t)
	inv := newInvoice(t, e, "KES", 5000, nil)
	id := inv["id"].(string)
	var full Invoice
	e.db.Where("id = ?", id).Preload("Payments").Take(&full)
	p, _, err := RecordOnlinePayment(e.db, OnlinePaymentInput{InvoiceID: id, Amount: 2000, Method: "Flutterwave · M-Pesa", Reference: "FLW-1", Notes: "Flutterwave tx_ref abc"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ReceiptNumber == "" {
		t.Fatalf("online payments get a receipt number too")
	}
	put := func(body map[string]interface{}) (int, map[string]interface{}) {
		return call(t, e.r, e.org, "PUT", "/api/invoices/"+id+"/payments/"+p.ID, body)
	}
	code, res := put(map[string]interface{}{"amount": 2500})
	if code != 409 || errCode(res) != "PAYMENT_LOCKED" || !strings.Contains(errMsg(res), "Flutterwave") {
		t.Fatalf("online amount edit must be blocked: %d %v", code, res)
	}
	if code, res = put(map[string]interface{}{"date": "2026-01-01"}); code != 409 {
		t.Fatalf("online date edit must be blocked: %d %v", code, res)
	}
	if code, res = put(map[string]interface{}{"notes": "Paid by the director", "reference": "FLW-1b"}); code != 200 {
		t.Fatalf("notes/reference edit allowed: %d %v", code, res)
	}
	got := payments(data(res)["invoice"].(map[string]interface{}))[0]
	if got["notes"] != "Paid by the director" || got["reference"] != "FLW-1b" || got["origin"] != OriginOnline || got["lock_reason"] == "" {
		t.Fatalf("online payment after edit: %v", got)
	}
	// Imported (Zoho) payments behave the same.
	z := Payment{ID: uuid.NewString(), InvoiceID: id, OrganizationID: e.org, Amount: 1000, Date: time.Now(), Method: "bank_transfer", Source: "zoho", ExternalID: "z1"}
	if err := e.db.Create(&z).Error; err != nil {
		t.Fatal(err)
	}
	code, res = call(t, e.r, e.org, "PUT", "/api/invoices/"+id+"/payments/"+z.ID, map[string]interface{}{"amount": 900})
	if code != 409 || !strings.Contains(errMsg(res), "Zoho Books") {
		t.Fatalf("zoho amount edit must be blocked: %d %v", code, res)
	}
}

// Deposits entered in the editor are edited in place — never duplicated.
func TestEditDepositFromEditor(t *testing.T) {
	e := setupMailEnv(t)
	inv := newInvoice(t, e, "KES", 20000, map[string]interface{}{"deposit": map[string]interface{}{"amount": 5000, "date": "2026-09-01", "method": "cash"}})
	id := inv["id"].(string)
	ps := payments(inv)
	if len(ps) != 1 || ps[0]["is_deposit"] != true || ps[0]["origin"] != OriginDeposit {
		t.Fatalf("deposit: %v", ps)
	}
	dep := ps[0]["id"].(string)
	body := map[string]interface{}{"client_name": "Wanjiru Ltd", "client_email": "billing@wanjiru.test", "currency": "KES",
		"items":   []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": 20000, "tax_rate": 0}},
		"deposit": map[string]interface{}{"payment_id": dep, "amount": 6000, "date": "2026-09-02", "method": "mobile_money", "reference": "QK9", "reason": "client sent more"}}
	code, res := call(t, e.r, e.org, "PUT", "/api/invoices/"+id, body)
	if code != 200 {
		t.Fatalf("update with deposit edit: %d %v", code, res)
	}
	inv = data(res)
	ps = payments(inv)
	if len(ps) != 1 || ps[0]["id"] != dep || num(ps[0]["amount"]) != 6000 || ps[0]["method"] != "mobile_money" || ps[0]["is_deposit"] != true {
		t.Fatalf("deposit must be edited in place: %v", ps)
	}
	if num(inv["amount_paid"]) != 6000 || num(inv["balance_due"]) != 14000 {
		t.Fatalf("amounts: %v %v", inv["amount_paid"], inv["balance_due"])
	}
	found := false
	for _, a := range inv["activities"].([]interface{}) {
		if m := a.(map[string]interface{}); m["type"] == "payment_edited" && strings.Contains(m["message"].(string), "Deposit edited") && strings.Contains(m["message"].(string), "client sent more") {
			found = true
		}
	}
	if !found {
		t.Fatalf("deposit edit not logged")
	}
	// Saving again without changes doesn't add a payment.
	if code, res = call(t, e.r, e.org, "PUT", "/api/invoices/"+id, body); code != 200 || len(payments(data(res))) != 1 {
		t.Fatalf("second save: %d %v", code, res)
	}
	// Lowering the total below the deposit while lowering the deposit works.
	body["items"] = []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": 3000, "tax_rate": 0}}
	body["deposit"] = map[string]interface{}{"payment_id": dep, "amount": 3000, "date": "2026-09-02", "method": "mobile_money", "reference": "QK9"}
	if code, res = call(t, e.r, e.org, "PUT", "/api/invoices/"+id, body); code != 200 || data(res)["status"] != StatusPaid {
		t.Fatalf("total and deposit lowered together: %d %v", code, res)
	}
}

// ---------------------------------------------------------------- numbering

func TestReceiptNumberingAndBackfill(t *testing.T) {
	e := setupMailEnv(t)
	inv := newInvoice(t, e, "KES", 9000, nil)
	id := inv["id"].(string)
	var nums []string
	for i := 0; i < 3; i++ {
		_, res := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 1000, "method": "cash"})
		ps := payments(data(res))
		nums = append(nums, ps[len(ps)-1]["receipt_number"].(string))
	}
	if strings.Join(nums, ",") != "RCT-0001,RCT-0002,RCT-0003" {
		t.Fatalf("numbers = %v", nums)
	}
	// Another organisation has its own sequence.
	other := setupMailEnv(t)
	oinv := newInvoice(t, other, "KES", 100, nil)
	_, res := call(t, other.r, other.org, "POST", "/api/invoices/"+oinv["id"].(string)+"/payments", map[string]interface{}{"amount": 100, "method": "cash"})
	if payments(data(res))[0]["receipt_number"] != "RCT-0001" {
		t.Fatalf("per-organisation sequence: %v", payments(data(res))[0]["receipt_number"])
	}
	// Prefix / next number from Invoice settings; a used number is skipped.
	_, res = call(t, e.r, e.org, "GET", "/api/settings", nil)
	s := data(res)["settings"].(map[string]interface{})
	if s["receipt_prefix"] != "RCT-" || num(s["next_receipt_number"]) != 4 {
		t.Fatalf("settings: %v %v", s["receipt_prefix"], s["next_receipt_number"])
	}
	s["receipt_prefix"], s["next_receipt_number"] = "RC/", 3
	if code, res := call(t, e.r, e.org, "PUT", "/api/settings", s); code != 200 {
		t.Fatalf("save settings: %d %v", code, res)
	}
	_, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 1000, "method": "cash"})
	ps := payments(data(res))
	if got := ps[len(ps)-1]["receipt_number"]; got != "RC/0003" {
		t.Fatalf("new prefix: %v", got)
	}
	// Partial settings updates (no receipt fields) keep the receipt settings.
	call(t, e.r, e.org, "PUT", "/api/settings", map[string]interface{}{"business_name": "Acme Ltd", "next_invoice_number": 2000, "next_quote_number": 1001, "default_due_days": 14, "quote_valid_days": 30})
	_, res = call(t, e.r, e.org, "GET", "/api/settings", nil)
	if s := data(res)["settings"].(map[string]interface{}); s["receipt_prefix"] != "RC/" || num(s["next_receipt_number"]) != 4 {
		t.Fatalf("receipt settings lost: %v %v", s["receipt_prefix"], s["next_receipt_number"])
	}
	// Unknown placeholders in the receipt template are rejected.
	s["receipt_subject"] = "Receipt {{receipt_no}}"
	if code, res := call(t, e.r, e.org, "PUT", "/api/settings", s); code != 400 || !strings.Contains(errMsg(res), "receipt_no") {
		t.Fatalf("bad placeholder: %d %v", code, res)
	}

	// Backfill: payments without numbers (made before receipts existed) get
	// numbers in date order; a second run changes nothing.
	b := setupMailEnv(t)
	binv := newInvoice(t, b, "KES", 9000, nil)
	bid := binv["id"].(string)
	d := func(s string) time.Time { x, _ := time.Parse("2006-01-02", s); return x }
	legacy := []Payment{
		{ID: uuid.NewString(), Amount: 300, Date: d("2026-03-03")},
		{ID: uuid.NewString(), Amount: 100, Date: d("2026-01-01")},
		{ID: uuid.NewString(), Amount: 200, Date: d("2026-02-02")},
	}
	for i := range legacy {
		legacy[i].InvoiceID, legacy[i].OrganizationID, legacy[i].Method = bid, b.org, "cash"
		if err := b.db.Create(&legacy[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	b.db.Model(&Payment{}).Where("organization_id = ?", b.org).UpdateColumn("receipt_number", "")
	b.db.Model(&Settings{}).Where("organization_id = ?", b.org).UpdateColumn("next_receipt_number", 1)
	if n := BackfillReceiptNumbers(b.db); n < 3 {
		t.Fatalf("backfill numbered %d", n)
	}
	var got []Payment
	b.db.Where("organization_id = ?", b.org).Order("receipt_number").Find(&got)
	if len(got) != 3 || got[0].Amount != 100 || got[1].Amount != 200 || got[2].Amount != 300 || got[0].ReceiptNumber != "RCT-0001" || got[2].ReceiptNumber != "RCT-0003" {
		t.Fatalf("backfill order: %+v", got)
	}
	BackfillReceiptNumbers(b.db)
	var again []Payment
	b.db.Where("organization_id = ?", b.org).Order("receipt_number").Find(&again)
	for i := range got {
		if again[i].ReceiptNumber != got[i].ReceiptNumber {
			t.Fatalf("backfill is not idempotent")
		}
	}
}

// ---------------------------------------------------------------- PDF

func TestReceiptPDFRevisedAndPaidInFull(t *testing.T) {
	d := func(s string) time.Time { x, _ := time.Parse("2006-01-02", s); return x }
	inv := &Invoice{ID: "i1", DocType: TypeInvoice, Number: "INV-1001", ClientName: "Wanjiru Ltd", ClientEmail: "b@w.test", Currency: "KES",
		IssueDate: d("2026-09-01"), DueDate: d("2026-09-15"), Total: 10000}
	rev := d("2026-09-05")
	p1 := Payment{ID: "p1", Amount: 2500, Date: d("2026-09-02"), Method: "mobile_money", Reference: "QK1", ReceiptNumber: "RCT-0001", CreatedAt: d("2026-09-02")}
	p2 := Payment{ID: "p2", Amount: 7500.5, Date: d("2026-09-04"), Method: "cash", ReceiptNumber: "RCT-0002", Notes: "Balance cleared", RevisedAt: &rev, CreatedAt: d("2026-09-04")}
	inv.Total = 10000.5
	inv.Payments = []Payment{p1, p2}
	ApplyPayments(inv, d("2026-09-05"))

	data, err := RenderReceiptPDF(inv, p1, PDFBusiness{Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	txt := pdfText(t, data)
	for _, s := range []string{"RCT-0001", "Wanjiru Ltd", "KSh 2,500.00", "Two thousand five hundred Kenyan shillings only", "INV-1001", "KSh 10,000.50", "KSh 7,500.50"} {
		if !strings.Contains(txt, s) {
			t.Fatalf("first receipt missing %q:\n%s", s, txt)
		}
	}
	if strings.Contains(txt, "PAID IN FULL") {
		t.Fatalf("the first (part) receipt must not say paid in full:\n%s", txt)
	}
	data, _ = RenderReceiptPDF(inv, p2, PDFBusiness{Name: "Acme"})
	txt = pdfText(t, data)
	for _, s := range []string{"RCT-0002", "PAID IN FULL", "Revised 5 Sep 2026", "Paid in full", "Balance cleared", "Seven thousand five hundred Kenyan shillings and fifty cents only"} {
		if !strings.Contains(txt, s) {
			t.Fatalf("second receipt missing %q:\n%s", s, txt)
		}
	}
	if ReceiptFilename(inv, p2) != "RCT-0002.pdf" {
		t.Fatalf("filename %s", ReceiptFilename(inv, p2))
	}
	// JPY (no decimals) and KWD (3 decimals).
	for _, c := range []struct {
		cur, amount, words string
		v                  float64
	}{{"JPY", "¥12,345", "Twelve thousand three hundred and forty-five Japanese yen only", 12345}, {"KWD", "KWD 1,250.125", "One thousand two hundred and fifty Kuwaiti dinars and one hundred and twenty-five fils only", 1250.125}} {
		j := &Invoice{ID: "j", DocType: TypeInvoice, Number: "INV-7", ClientName: "C", Currency: c.cur, Total: c.v}
		jp := Payment{ID: "jp", Amount: c.v, Date: d("2026-09-02"), Method: "card", ReceiptNumber: "RCT-0009"}
		j.Payments = []Payment{jp}
		ApplyPayments(j, d("2026-09-02"))
		data, _ := RenderReceiptPDF(j, jp, PDFBusiness{Name: "Acme"})
		txt := pdfText(t, data)
		if !strings.Contains(txt, c.amount) || !strings.Contains(txt, c.words) || !strings.Contains(txt, "PAID IN FULL") {
			t.Fatalf("%s receipt:\n%s", c.cur, txt)
		}
	}
}

// ---------------------------------------------------------------- email

func TestReceiptEmail(t *testing.T) {
	e := setupMailEnv(t)
	if err := e.db.AutoMigrate(&models.Customer{}, &models.CustomerContact{}); err != nil {
		t.Skipf("customer tables: %v", err)
	}
	cust := models.Customer{ID: uuid.NewString(), OrganizationID: e.org, FirstName: "Wanjiru", LastName: "Ltd", Email: "owner@wanjiru.test"}
	if err := e.db.Omit("Organization", "Vehicles", "Bookings").Create(&cust).Error; err != nil {
		t.Skipf("customer: %v", err)
	}
	yes, no := true, false
	for _, ct := range []models.CustomerContact{
		{ID: uuid.NewString(), CustomerID: cust.ID, OrganizationID: e.org, Email: "ap@wanjiru.test", Name: "Accounts", IsPrimaryBilling: true, CCInvoices: true, CCReceipts: &yes},
		{ID: uuid.NewString(), CustomerID: cust.ID, OrganizationID: e.org, Email: "boss@wanjiru.test", CCInvoices: true, CCReceipts: &yes},
		{ID: uuid.NewString(), CustomerID: cust.ID, OrganizationID: e.org, Email: "sales@wanjiru.test", CCInvoices: true, CCReceipts: &no},
	} {
		if err := e.db.Select("*").Create(&ct).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Custom receipt template from Invoice settings.
	_, res := call(t, e.r, e.org, "GET", "/api/settings", nil)
	s := data(res)["settings"].(map[string]interface{})
	s["receipt_subject"] = "{{business_name}}: receipt {{receipt_number}} for {{invoice_number}}"
	s["receipt_message"] = "Dear {{client_name}}, we received {{amount_received}} on {{payment_date}} by {{payment_method}}. Balance: {{balance_due}}."
	if code, res := call(t, e.r, e.org, "PUT", "/api/settings", s); code != 200 {
		t.Fatalf("settings: %d %v", code, res)
	}
	inv := newInvoice(t, e, "KES", 10000, map[string]interface{}{"customer_id": cust.ID, "client_email": "owner@wanjiru.test"})
	id := inv["id"].(string)
	*e.sent = nil
	code, res := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 4000, "method": "mobile_money", "reference": "QK7", "date": "2026-09-20", "email_receipt": true})
	if code != 200 {
		t.Fatalf("record + email: %d %v", code, res)
	}
	inv = data(res)
	re, _ := inv["receipt_email"].(map[string]interface{})
	if re == nil || re["emailed"] != true {
		t.Fatalf("receipt not emailed: %v", inv["receipt_email"])
	}
	// One copy per person (per-recipient tracking, on by default).
	if len(*e.sent) != 2 {
		t.Fatalf("sent %d emails", len(*e.sent))
	}
	m := (*e.sent)[0]
	rn := payments(inv)[0]["receipt_number"].(string)
	if m.To[0] != "ap@wanjiru.test" || len(m.CC) != 0 || (*e.sent)[1].To[0] != "boss@wanjiru.test" || !strings.Contains(m.Text, "Also sent to: boss@wanjiru.test") {
		t.Fatalf("recipients: to=%v cc=%v / %v (sales@ opted out of receipts)", m.To, m.CC, (*e.sent)[1].To)
	}
	if m.Subject != "Acme Ltd: receipt "+rn+" for "+inv["number"].(string) {
		t.Fatalf("subject: %q", m.Subject)
	}
	if !strings.Contains(m.Text, "Dear Wanjiru Ltd, we received KSh 4,000.00 on 20 Sep 2026 by Mobile money. Balance: KSh 6,000.00.") {
		t.Fatalf("message placeholders:\n%s", m.Text)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != rn+".pdf" {
		t.Fatalf("attachment: %+v", m.Attachments)
	}
	if txt := pdfText(t, m.Attachments[0].Data); !strings.Contains(txt, rn) || !strings.Contains(txt, "Four thousand Kenyan shillings only") {
		t.Fatalf("attached receipt:\n%s", txt)
	}
	if !strings.Contains(m.HTML, "Payment received — thank you") || !strings.Contains(m.HTML, "View invoice") || !strings.Contains(m.HTML, "Balance remaining KSh 6,000.00") {
		t.Fatalf("html summary card")
	}
	// Logged as a receipt on the Emails tab.
	_, res = call(t, e.r, e.org, "GET", "/api/invoices/"+id+"/email-log", nil)
	log := data(res)["log"].([]interface{})
	if len(log) == 0 || log[0].(map[string]interface{})["kind"] != "receipt" || log[0].(map[string]interface{})["status"] != "sent" {
		t.Fatalf("email log: %v", log)
	}

	// Edit + "Email revised receipt": the subject says revised, the number is kept.
	pid := payments(inv)[0]["id"].(string)
	*e.sent = nil
	code, res = call(t, e.r, e.org, "PUT", "/api/invoices/"+id+"/payments/"+pid, map[string]interface{}{"amount": 10000, "email_receipt": true})
	if code != 200 || len(*e.sent) != 2 {
		t.Fatalf("edit + email: %d %v (%d sent)", code, res, len(*e.sent))
	}
	m = (*e.sent)[0]
	if !strings.HasPrefix(m.Subject, "Revised: ") || !strings.Contains(m.Subject, rn) || !strings.Contains(m.Text, "Balance: KSh 0.00") || !strings.Contains(m.Text, "Paid in full") {
		t.Fatalf("revised receipt: %q\n%s", m.Subject, m.Text)
	}
	if txt := pdfText(t, m.Attachments[0].Data); !strings.Contains(txt, "PAID IN FULL") || !strings.Contains(txt, "Revised") {
		t.Fatalf("revised PDF:\n%s", txt)
	}

	// Preflight + preview for the "Send receipt" dialog, and the draft.
	code, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/preflight", map[string]interface{}{"kind": "receipt", "payment_id": pid, "mode": "email"})
	pf := data(res)
	if code != 200 || pf["preview"].(map[string]interface{})["kind"] != "receipt" || num(pf["errors"]) != 0 {
		t.Fatalf("preflight: %d %v", code, res)
	}
	atts := pf["preview"].(map[string]interface{})["attachments"].([]interface{})
	if atts[0].(map[string]interface{})["filename"] != rn+".pdf" {
		t.Fatalf("preview attachment: %v", atts)
	}
	_, res = call(t, e.r, e.org, "GET", "/api/invoices/"+id+"/recipients?kind=receipt", nil)
	if cc := data(res)["cc"].([]interface{}); len(cc) != 1 || cc[0].(map[string]interface{})["email"] != "boss@wanjiru.test" {
		t.Fatalf("receipt recipients: %v", data(res))
	}
	// A wrong amount typed into the message is caught by the checks.
	code, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/preflight", map[string]interface{}{"kind": "receipt", "payment_id": pid, "mode": "email", "message": "We got KSh 9,999.00, thanks"})
	if code != 200 || num(data(res)["errors"]) == 0 {
		t.Fatalf("wrong amount should be an error: %v", data(res))
	}
	// Without email set up, record still works and says why nothing was sent.
	plain := setupMailEnv(t)
	plain.h.Mail = nil
	pinv := newInvoice(t, plain, "KES", 100, nil)
	code, res = call(t, plain.r, plain.org, "POST", "/api/invoices/"+pinv["id"].(string)+"/payments", map[string]interface{}{"amount": 50, "method": "cash", "email_receipt": true})
	if code != 200 || data(res)["receipt_email"].(map[string]interface{})["emailed"] != false || !strings.Contains(fmt.Sprint(data(res)["receipt_email"]), "set up") {
		t.Fatalf("no email configured: %d %v", code, data(res)["receipt_email"])
	}
	// AutoReceipt (online payments) emails without a request.
	*e.sent = nil
	inv2 := newInvoice(t, e, "KES", 3000, map[string]interface{}{"customer_id": cust.ID})
	op, _, err := RecordOnlinePayment(e.db, OnlinePaymentInput{InvoiceID: inv2["id"].(string), Amount: 3000, Method: "Flutterwave · Card", Reference: "FLW-9", Notes: "Flutterwave tx_ref t9"})
	if err != nil {
		t.Fatal(err)
	}
	if r := e.h.AutoReceipt(e.org, inv2["id"].(string), op.ID); !r.Emailed || len(*e.sent) != 2 || (*e.sent)[0].Attachments[0].Filename != op.ReceiptNumber+".pdf" {
		t.Fatalf("auto receipt: %+v", r)
	}
}
