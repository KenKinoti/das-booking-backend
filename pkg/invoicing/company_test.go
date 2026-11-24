package invoicing

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Billing a company: the company is the bill-to name, the person is "Attn:"
// on the PDF and is greeted in the email; customers with a company name fill
// this in automatically; updates keep it unless told otherwise.
func TestCompanyBilling(t *testing.T) {
	e := setupMailEnv(t)
	if err := e.db.AutoMigrate(&models.Customer{}, &models.CustomerContact{}); err != nil {
		t.Skipf("customer tables: %v", err)
	}
	cust := models.Customer{ID: uuid.NewString(), OrganizationID: e.org, FirstName: "Jane", LastName: "Wanjiru",
		CompanyName: "Kilima Traders Ltd", TaxNumber: "P051234567X", Email: "accounts@kilima.test"}
	if err := e.db.Omit("Organization", "Vehicles", "Bookings").Create(&cust).Error; err != nil {
		t.Skipf("customer: %v", err)
	}

	// 1. Linked to a company customer: filled from the record.
	inv := newInvoice(t, e, "KES", 5000, map[string]interface{}{"customer_id": cust.ID, "client_name": "Jane Wanjiru", "client_email": "accounts@kilima.test"})
	id := inv["id"].(string)
	if inv["client_name"] != "Kilima Traders Ltd" || inv["client_is_company"] != true || inv["client_contact"] != "Jane Wanjiru" || inv["client_tax_number"] != "P051234567X" {
		t.Fatalf("from customer: name=%v company=%v contact=%v tax=%v", inv["client_name"], inv["client_is_company"], inv["client_contact"], inv["client_tax_number"])
	}

	// 2. The PDF shows the company and "Attn:"; the email greets the person.
	*e.sent = nil
	if code, res := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email"}); code != 200 || len(*e.sent) == 0 {
		t.Fatalf("send: %d %v", code, res)
	}
	m := (*e.sent)[0]
	if !strings.Contains(m.Text, "Hi Jane Wanjiru,") {
		t.Fatalf("greeting:\n%s", m.Text)
	}
	txt := pdfText(t, m.Attachments[0].Data)
	if !strings.Contains(txt, "Kilima Traders Ltd") || !strings.Contains(txt, "Attn: Jane Wanjiru") {
		t.Fatalf("PDF bill-to:\n%s", txt)
	}

	// 3. An update that doesn't mention it keeps company billing.
	body := map[string]interface{}{"customer_id": cust.ID, "client_name": "Kilima Traders Ltd", "client_email": "accounts@kilima.test", "currency": "KES",
		"items": []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": 6000, "tax_rate": 0}}}
	code, res := call(t, e.r, e.org, "PUT", "/api/invoices/"+id, body)
	if code != 200 || data(res)["client_is_company"] != true || data(res)["client_contact"] != "Jane Wanjiru" {
		t.Fatalf("update keeps company: %d %v", code, res)
	}
	// Duplicates keep it too.
	_, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/duplicate", nil)
	if d := data(res); d["client_is_company"] != true || d["client_contact"] != "Jane Wanjiru" {
		t.Fatalf("duplicate: %v", d)
	}
	// Switching to a person clears the contact.
	body["client_is_company"], body["client_name"] = false, "Jane Wanjiru"
	_, res = call(t, e.r, e.org, "PUT", "/api/invoices/"+id, body)
	if d := data(res); d["client_is_company"] != false || d["client_contact"] != "" || d["client_name"] != "Jane Wanjiru" {
		t.Fatalf("back to a person: %v", d)
	}

	// 4. A one-off company with no customer record; no contact → no "Attn:".
	one := newInvoice(t, e, "KES", 1000, map[string]interface{}{"client_name": "Baraka Hardware", "client_is_company": true, "client_contact": "  "})
	if one["client_is_company"] != true || one["client_contact"] != "" {
		t.Fatalf("one-off company: %v", one)
	}
	*e.sent = nil
	call(t, e.r, e.org, "POST", "/api/invoices/"+one["id"].(string)+"/send", map[string]interface{}{"mode": "email"})
	if len(*e.sent) == 0 || !strings.Contains((*e.sent)[0].Text, "Hi Baraka Hardware,") || strings.Contains(pdfText(t, (*e.sent)[0].Attachments[0].Data), "Attn:") {
		t.Fatalf("company without contact: %d emails", len(*e.sent))
	}

	// 5. A different name typed on purpose for a company customer is respected.
	other := newInvoice(t, e, "KES", 1000, map[string]interface{}{"customer_id": cust.ID, "client_name": "Kilima Traders (Mombasa branch)"})
	if other["client_name"] != "Kilima Traders (Mombasa branch)" || other["client_is_company"] != false {
		t.Fatalf("typed name: %v / %v", other["client_name"], other["client_is_company"])
	}
	// ... and an explicit "person" wins over the record.
	person := newInvoice(t, e, "KES", 1000, map[string]interface{}{"customer_id": cust.ID, "client_name": "Jane Wanjiru", "client_is_company": false})
	if person["client_name"] != "Jane Wanjiru" || person["client_is_company"] != false {
		t.Fatalf("explicit person: %v", person)
	}

	// 6. Validation names the right field.
	code, res = call(t, e.r, e.org, "POST", "/api/invoices", map[string]interface{}{"client_name": " ", "client_is_company": true, "currency": "KES",
		"items": []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": 1}}})
	if msg, _ := res["message"].(string); code != 400 || !strings.Contains(msg+errMessage(res), "Company name is required") {
		t.Fatalf("validation: %d %v", code, res)
	}

	// 7. The editor's client list and customer defaults know it's a company.
	_, res = call(t, e.r, e.org, "GET", "/api/clients", nil)
	found := false
	for _, x := range res["data"].([]interface{}) {
		c := x.(map[string]interface{})
		if c["name"] == "Kilima Traders Ltd" {
			found = c["is_company"] == true && c["contact"] == "Jane Wanjiru"
		}
	}
	if !found {
		t.Fatalf("clients list: %v", res["data"])
	}
	_, res = call(t, e.r, e.org, "GET", "/api/customer-defaults/"+cust.ID, nil)
	if d := data(res); d["name"] != "Kilima Traders Ltd" || d["is_company"] != true || d["contact"] != "Jane Wanjiru" || d["tax_number"] != "P051234567X" {
		t.Fatalf("customer defaults: %v", d)
	}
}

func errMessage(res map[string]interface{}) string {
	e, _ := res["error"].(map[string]interface{})
	s, _ := e["message"].(string)
	return s
}
