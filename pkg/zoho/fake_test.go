package zoho

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeZoho is an in-memory Zoho accounts + Books API server.
type fakeZoho struct {
	t           *testing.T
	srv         *httptest.Server
	mu          sync.Mutex
	codes       map[string]bool
	access      map[string]bool
	refreshes   int
	calls       map[string]int
	throttleN   int  // answer the next N API calls with 429
	dailyLimit  bool // answer every API call with a daily-limit 429
	expireNext  bool // answer the next API call with 401 (expired token)
	contacts    []map[string]interface{}
	invoices    []map[string]interface{}
	estimates   []map[string]interface{}
	creditnotes []map[string]interface{}
	payments    []map[string]interface{}
	items       []map[string]interface{}
	recurring   []map[string]interface{}
	comments    map[string][]map[string]interface{}
	orgCurrency string
}

func newFakeZoho(t *testing.T) *fakeZoho {
	f := &fakeZoho{t: t, codes: map[string]bool{"good-code": true}, access: map[string]bool{}, calls: map[string]int{}, comments: map[string][]map[string]interface{}{}, orgCurrency: "AUD"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeZoho) json(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeZoho) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/oauth/v2/token":
		_ = r.ParseForm()
		v := r.Form
		if v.Get("client_id") != "cid" || v.Get("client_secret") != "csecret" {
			f.json(w, 200, map[string]string{"error": "invalid_client"})
			return
		}
		switch v.Get("grant_type") {
		case "authorization_code":
			if !f.codes[v.Get("code")] {
				f.json(w, 200, map[string]string{"error": "invalid_code"})
				return
			}
			delete(f.codes, v.Get("code"))
			tok := "at-" + strconv.Itoa(len(f.access)+1)
			f.access[tok] = true
			f.json(w, 200, map[string]interface{}{"access_token": tok, "refresh_token": "rt-1", "api_domain": f.srv.URL, "token_type": "Bearer", "expires_in": 3600})
		case "refresh_token":
			if v.Get("refresh_token") != "rt-1" {
				f.json(w, 200, map[string]string{"error": "invalid_code"})
				return
			}
			f.refreshes++
			tok := "at-r" + strconv.Itoa(f.refreshes)
			f.access[tok] = true
			f.json(w, 200, map[string]interface{}{"access_token": tok, "api_domain": f.srv.URL, "token_type": "Bearer", "expires_in": 3600})
		}
		return
	case p == "/oauth/v2/token/revoke":
		f.json(w, 200, map[string]string{"status": "success"})
		return
	}
	if !strings.HasPrefix(p, "/books/v3") {
		http.NotFound(w, r)
		return
	}
	p = strings.TrimPrefix(p, "/books/v3")
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Zoho-oauthtoken ")
	if f.expireNext {
		f.expireNext = false
		delete(f.access, tok)
	}
	if !f.access[tok] {
		f.json(w, 401, map[string]interface{}{"code": 57, "message": "You are not authorized to perform this operation"})
		return
	}
	if f.dailyLimit {
		f.json(w, 429, map[string]interface{}{"code": 1070, "message": "The API call for this organization has exceeded the maximum call rate limit of 1000 per day."})
		return
	}
	if f.throttleN > 0 {
		f.throttleN--
		w.Header().Set("Retry-After", "1")
		f.json(w, 429, map[string]interface{}{"code": 44, "message": "You have made too many requests continuously. Please try again after some time."})
		return
	}
	f.calls[p]++
	q := r.URL.Query()
	if p != "/organizations" && q.Get("organization_id") != "777" {
		f.json(w, 400, map[string]interface{}{"code": 6041, "message": "organization_id missing"})
		return
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	list := func(key string, rows []map[string]interface{}) {
		page, _ := strconv.Atoi(q.Get("page"))
		per, _ := strconv.Atoi(q.Get("per_page"))
		if page < 1 {
			page = 1
		}
		if per < 1 {
			per = 200
		}
		start, end := (page-1)*per, page*per
		if start > len(rows) {
			start = len(rows)
		}
		if end > len(rows) {
			end = len(rows)
		}
		f.json(w, 200, map[string]interface{}{"code": 0, "message": "success", key: rows[start:end],
			"page_context": map[string]interface{}{"page": page, "per_page": per, "has_more_page": end < len(rows)}})
	}
	find := func(rows []map[string]interface{}, idKey, id string) map[string]interface{} {
		for _, r := range rows {
			if fmt.Sprint(r[idKey]) == id {
				return r
			}
		}
		return nil
	}
	switch {
	case p == "/organizations":
		f.json(w, 200, map[string]interface{}{"code": 0, "organizations": []map[string]interface{}{{"organization_id": "777", "name": "Dasyin Pty Ltd", "currency_code": f.orgCurrency, "is_default_org": true}}})
	case p == "/settings/taxes":
		f.json(w, 200, map[string]interface{}{"code": 0, "taxes": []map[string]interface{}{{"tax_name": "GST", "tax_percentage": 10}, {"tax_name": "GST Free", "tax_percentage": 0}}})
	case p == "/items":
		list("items", f.items)
	case p == "/contacts":
		list("contacts", f.contacts)
	case p == "/invoices":
		list("invoices", f.invoices)
	case p == "/estimates":
		list("estimates", f.estimates)
	case p == "/creditnotes":
		list("creditnotes", f.creditnotes)
	case p == "/customerpayments":
		list("customerpayments", f.payments)
	case p == "/recurringinvoices":
		list("recurring_invoices", f.recurring)
	case len(parts) == 2 && parts[0] == "contacts":
		f.json(w, 200, map[string]interface{}{"code": 0, "contact": find(f.contacts, "contact_id", parts[1])})
	case len(parts) == 2 && (parts[0] == "invoices" || parts[0] == "estimates" || parts[0] == "creditnotes" || parts[0] == "recurringinvoices"):
		rows, idKey, key := f.invoices, "invoice_id", "invoice"
		switch parts[0] {
		case "estimates":
			rows, idKey, key = f.estimates, "estimate_id", "estimate"
		case "creditnotes":
			rows, idKey, key = f.creditnotes, "creditnote_id", "creditnote"
		case "recurringinvoices":
			rows, idKey, key = f.recurring, "recurring_invoice_id", "recurring_invoice"
		}
		d := find(rows, idKey, parts[1])
		if d == nil {
			f.json(w, 404, map[string]interface{}{"code": 1002, "message": "not found"})
			return
		}
		if q.Get("accept") == "pdf" {
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.4 fake " + fmt.Sprint(d["invoice_number"], d["estimate_number"], d["creditnote_number"])))
			return
		}
		f.json(w, 200, map[string]interface{}{"code": 0, key: d})
	case len(parts) == 3 && parts[2] == "comments":
		f.json(w, 200, map[string]interface{}{"code": 0, "comments": f.comments[parts[1]]})
	default:
		f.json(w, 404, map[string]interface{}{"code": 5, "message": "Invalid URL " + p})
	}
}

// seed fills the fake with a realistic organisation.
func (f *fakeZoho) seed(nInvoices int) {
	f.items = []map[string]interface{}{
		{"item_id": "it1", "name": "Website maintenance", "description": "Monthly care plan", "rate": 250, "unit": "month", "tax_name": "GST", "tax_percentage": 10, "product_type": "service", "status": "active"},
		{"item_id": "it2", "name": "Consulting hour", "rate": 180, "unit": "hr", "product_type": "service", "status": "active"},
	}
	f.contacts = []map[string]interface{}{
		{"contact_id": "c1", "contact_name": "Acme Pty Ltd", "company_name": "Acme Pty Ltd", "contact_type": "customer", "customer_sub_type": "business", "status": "active",
			"email": "accounts@acme.test", "phone": "+61 2 9000 0000", "currency_code": "AUD", "payment_terms": 14, "tax_reg_no": "51 824 753 556",
			"billing_address": map[string]interface{}{"attention": "Accounts", "address": "1 George St", "city": "Sydney", "state": "NSW", "zip": "2000", "country": "Australia"},
			"contact_persons": []map[string]interface{}{
				{"contact_person_id": "p1", "first_name": "Jane", "last_name": "Doe", "email": "accounts@acme.test", "is_primary_contact": true, "designation": "Accounts payable"},
				{"contact_person_id": "p2", "first_name": "Bob", "last_name": "Lee", "email": "bob@acme.test", "is_primary_contact": false, "phone": "0400 000 000"},
				{"contact_person_id": "p3", "first_name": "No", "last_name": "Email", "email": "", "is_primary_contact": false},
			},
			"last_modified_time": "2025-01-01T10:00:00+1000"},
		{"contact_id": "c2", "contact_name": "Globex Inc", "company_name": "Globex Inc", "contact_type": "customer", "customer_sub_type": "business", "status": "active",
			"email": "ap@globex.test", "currency_code": "USD", "billing_address": map[string]interface{}{"address": "5 Main St", "city": "Austin", "state": "TX", "zip": "73301", "country": "United States"},
			"contact_persons":    []map[string]interface{}{{"contact_person_id": "p4", "first_name": "Hank", "last_name": "Scorpio", "email": "ap@globex.test", "is_primary_contact": true}},
			"last_modified_time": "2025-01-01T10:00:00+1000"},
		{"contact_id": "c3", "contact_name": "Sam Smith", "first_name": "Sam", "last_name": "Smith", "contact_type": "customer", "customer_sub_type": "individual", "status": "inactive",
			"email": "", "last_modified_time": "2025-01-01T10:00:00+1000"},
		{"contact_id": "v1", "contact_name": "Office Supplies Co", "contact_type": "vendor", "last_modified_time": "2025-01-01T10:00:00+1000"},
	}
	line := func(name string, qty, rate, tax float64) map[string]interface{} {
		return map[string]interface{}{"line_item_id": "l-" + name, "item_id": "it1", "name": name, "quantity": qty, "rate": rate, "tax_name": "GST", "tax_percentage": tax, "item_total": qty * rate}
	}
	// INV-000001: paid in two parts (bank + card).
	f.invoices = append(f.invoices, map[string]interface{}{"invoice_id": "i1", "invoice_number": "INV-000001", "customer_id": "c1", "customer_name": "Acme Pty Ltd", "status": "paid",
		"date": "2025-02-01", "due_date": "2025-02-15", "currency_code": "AUD", "exchange_rate": 1, "line_items": []interface{}{line("Website maintenance", 2, 250, 10)},
		"sub_total": 500, "tax_total": 50, "total": 550, "balance": 0, "payment_made": 550, "credits_applied": 0, "notes": "Thanks!", "terms": "14 days",
		"reference_number": "PO-77", "invoice_url": "https://zoho.example/inv/i1", "estimate_id": "e1", "last_payment_date": "2025-02-20",
		"payments": []interface{}{
			map[string]interface{}{"payment_id": "pay1", "payment_number": "1", "invoice_payment_id": "ip1", "payment_mode": "Bank Transfer", "date": "2025-02-10", "amount": 300, "reference_number": "EFT123"},
			map[string]interface{}{"payment_id": "pay2", "payment_number": "2", "invoice_payment_id": "ip2", "payment_mode": "Credit Card", "date": "2025-02-20", "amount": 250},
		},
		"last_modified_time": "2025-02-20T10:00:00+1000", "created_time": "2025-02-01T09:00:00+1000"})
	// INV-000002: USD, part-paid, entity discount 10%.
	f.invoices = append(f.invoices, map[string]interface{}{"invoice_id": "i2", "invoice_number": "INV-000002", "customer_id": "c2", "customer_name": "Globex Inc", "status": "partially_paid",
		"date": "2025-03-01", "due_date": "2025-03-31", "currency_code": "USD", "exchange_rate": 1.5, "discount_type": "entity_level", "discount_percent": 10, "is_discount_before_tax": true,
		"line_items": []interface{}{line("Consulting", 10, 100, 0)}, "sub_total": 1000, "tax_total": 0, "total": 900, "balance": 400, "payment_made": 500, "credits_applied": 0,
		"payments":           []interface{}{map[string]interface{}{"payment_id": "pay3", "invoice_payment_id": "ip3", "payment_mode": "PayPal", "date": "2025-03-10", "amount": 500}},
		"last_modified_time": "2025-03-10T10:00:00+1000"})
	// INV-000003: void.
	f.invoices = append(f.invoices, map[string]interface{}{"invoice_id": "i3", "invoice_number": "INV-000003", "customer_id": "c1", "customer_name": "Acme Pty Ltd", "status": "void",
		"date": "2025-03-05", "due_date": "2025-03-19", "currency_code": "AUD", "exchange_rate": 1, "line_items": []interface{}{line("Oops", 1, 100, 10)},
		"sub_total": 100, "tax_total": 10, "total": 110, "balance": 0, "last_modified_time": "2025-03-06T10:00:00+1000"})
	// INV-000004: settled by a credit note; shipping + adjustment.
	f.invoices = append(f.invoices, map[string]interface{}{"invoice_id": "i4", "invoice_number": "INV-000004", "customer_id": "c1", "customer_name": "Acme Pty Ltd", "status": "paid",
		"date": "2025-04-01", "due_date": "2025-04-15", "currency_code": "AUD", "exchange_rate": 1, "line_items": []interface{}{line("Hosting", 1, 200, 10)},
		"shipping_charge": 15, "adjustment": -5, "adjustment_description": "Rounding", "sub_total": 200, "tax_total": 20, "total": 230, "balance": 0, "payment_made": 0,
		"credits_applied": 230, "credits": []interface{}{map[string]interface{}{"creditnote_id": "cn1", "creditnote_number": "CN-00001", "credited_amount": 230}},
		"last_modified_time": "2025-04-02T10:00:00+1000"})
	// INV-000005: overdue, viewed, line discount and a total Zoho rounded differently.
	f.invoices = append(f.invoices, map[string]interface{}{"invoice_id": "i5", "invoice_number": "INV-000005", "customer_id": "c3", "customer_name": "Sam Smith", "status": "overdue",
		"date": "2025-05-01", "due_date": "2025-05-15", "currency_code": "AUD", "exchange_rate": 1,
		"line_items": []interface{}{map[string]interface{}{"line_item_id": "l5", "name": "Logo design", "quantity": 1, "rate": 99.99, "discount": "10.00%", "tax_percentage": 10, "item_total": 89.99}},
		"sub_total":  89.99, "tax_total": 9.00, "total": 99.00, "balance": 99.00, "last_modified_time": "2025-05-01T10:00:00+1000"})
	// Draft.
	f.invoices = append(f.invoices, map[string]interface{}{"invoice_id": "i6", "invoice_number": "INV-000006", "customer_id": "c1", "customer_name": "Acme Pty Ltd", "status": "draft",
		"date": "2025-06-01", "due_date": "2025-06-15", "currency_code": "AUD", "line_items": []interface{}{line("Draft work", 1, 100, 10)},
		"sub_total": 100, "tax_total": 10, "total": 110, "balance": 110, "last_modified_time": "2025-06-01T10:00:00+1000"})
	for i := 7; i <= nInvoices; i++ {
		f.invoices = append(f.invoices, map[string]interface{}{"invoice_id": fmt.Sprintf("i%d", i), "invoice_number": fmt.Sprintf("INV-%06d", i), "customer_id": "c1", "customer_name": "Acme Pty Ltd",
			"status": "sent", "date": "2025-07-01", "due_date": "2099-07-15", "currency_code": "AUD", "exchange_rate": 1, "line_items": []interface{}{line("Bulk", 1, 10, 10)},
			"sub_total": 10, "tax_total": 1, "total": 11, "balance": 11, "last_modified_time": "2025-07-01T10:00:00+1000"})
	}
	f.estimates = []map[string]interface{}{
		{"estimate_id": "e1", "estimate_number": "EST-000010", "customer_id": "c1", "customer_name": "Acme Pty Ltd", "status": "invoiced", "date": "2025-01-20", "expiry_date": "2025-02-20",
			"currency_code": "AUD", "exchange_rate": 1, "line_items": []interface{}{line("Website maintenance", 2, 250, 10)}, "sub_total": 500, "tax_total": 50, "total": 550,
			"last_modified_time": "2025-02-01T10:00:00+1000"},
		{"estimate_id": "e2", "estimate_number": "EST-000011", "customer_id": "c2", "customer_name": "Globex Inc", "status": "sent", "date": "2025-06-01", "expiry_date": "2099-07-01",
			"currency_code": "USD", "exchange_rate": 1.5, "line_items": []interface{}{line("Audit", 1, 1000, 0)}, "sub_total": 1000, "tax_total": 0, "total": 1000,
			"last_modified_time": "2025-06-01T10:00:00+1000"},
	}
	f.creditnotes = []map[string]interface{}{
		{"creditnote_id": "cn1", "creditnote_number": "CN-00001", "customer_id": "c1", "customer_name": "Acme Pty Ltd", "status": "closed", "date": "2025-04-02",
			"currency_code": "AUD", "exchange_rate": 1, "line_items": []interface{}{line("Goodwill credit", 1, 209.09, 10)}, "sub_total": 209.09, "tax_total": 20.91, "total": 230, "balance": 0,
			"last_modified_time": "2025-04-02T10:00:00+1000"},
	}
	f.payments = []map[string]interface{}{
		{"payment_id": "pay1", "payment_number": "1", "customer_name": "Acme Pty Ltd", "amount": 300, "unused_amount": 0, "currency_code": "AUD"},
		{"payment_id": "pay4", "payment_number": "4", "customer_name": "Globex Inc", "amount": 120, "unused_amount": 120, "currency_code": "USD"},
	}
	f.comments["i1"] = []map[string]interface{}{
		{"comment_id": "cm1", "description": "Invoice emailed to accounts@acme.test", "commented_by": "Kennedy", "date": "2025-02-01"},
		{"comment_id": "cm2", "description": "Payment of AUD 300.00 received", "commented_by": "System", "date": "2025-02-10"},
	}
}

func (f *fakeZoho) authURLHost() string {
	u, _ := url.Parse(f.srv.URL)
	return u.Host
}
