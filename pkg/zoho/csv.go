package zoho

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// File kinds.
const (
	FileContacts = "zoho_contacts"
	FilePersons  = "zoho_contact_persons"
	FileItems    = "zoho_items"
	FileInvoices = "zoho_invoices"
	FileQuotes   = "zoho_estimates"
	FileCredits  = "zoho_credit_notes"
	FilePayments = "zoho_payments"
	FileGenCust  = "generic_customers"
	FileGenInv   = "generic_invoices"
	FileIgnore   = "ignore"
)

// Field is a column the importer understands.
type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Required bool     `json:"required,omitempty"`
	Aliases  []string `json:"-"`
}

// FileKind describes one kind of file.
type FileKind struct {
	Kind   string  `json:"kind"`
	Label  string  `json:"label"`
	Zoho   bool    `json:"zoho"`
	Fields []Field `json:"fields"`
}

var addrFields = []Field{
	{Key: "billing_attention", Label: "Billing attention", Aliases: []string{"Billing Attention"}},
	{Key: "billing_street", Label: "Billing street", Aliases: []string{"Billing Address", "Billing Street", "Billing Address Line 1", "Address", "Street", "Address 1", "Address Line 1"}},
	{Key: "billing_street2", Label: "Billing street 2", Aliases: []string{"Billing Street2", "Billing Street 2", "Billing Address Line 2", "Address 2", "Address Line 2"}},
	{Key: "billing_city", Label: "Billing city / suburb", Aliases: []string{"Billing City", "City", "Suburb", "Town"}},
	{Key: "billing_state", Label: "Billing state", Aliases: []string{"Billing State", "State", "Region", "Province"}},
	{Key: "billing_zip", Label: "Billing postcode", Aliases: []string{"Billing Code", "Billing Zip", "Billing Zip Code", "Billing Postcode", "Billing Postal Code", "Postcode", "Post Code", "Zip", "Zip Code", "Postal Code"}},
	{Key: "billing_country", Label: "Billing country", Aliases: []string{"Billing Country", "Country"}},
	{Key: "billing_phone", Label: "Billing phone", Aliases: []string{"Billing Phone"}},
}

var contactFields = append([]Field{
	{Key: "contact_id", Label: "Contact ID", Aliases: []string{"Contact ID", "Customer ID", "Client ID", "ID"}},
	{Key: "display_name", Label: "Display name", Required: true, Aliases: []string{"Display Name", "Contact Name", "Customer Name", "Client Name", "Name", "Customer", "Client", "Full Name"}},
	{Key: "company_name", Label: "Company name", Aliases: []string{"Company Name", "Company", "Business Name", "Organisation", "Organization"}},
	{Key: "first_name", Label: "First name", Aliases: []string{"First Name", "Given Name", "Firstname"}},
	{Key: "last_name", Label: "Last name", Aliases: []string{"Last Name", "Surname", "Family Name", "Lastname"}},
	{Key: "email", Label: "Email", Aliases: []string{"EmailID", "Email ID", "Email", "Email Address", "E-mail", "Primary Email"}},
	{Key: "phone", Label: "Phone", Aliases: []string{"Phone", "Work Phone", "Phone Number", "Telephone", "Business Phone"}},
	{Key: "mobile", Label: "Mobile", Aliases: []string{"MobilePhone", "Mobile Phone", "Mobile", "Cell"}},
	{Key: "website", Label: "Website", Aliases: []string{"Website", "Web", "URL"}},
	{Key: "currency", Label: "Currency", Aliases: []string{"Currency Code", "Currency"}},
	{Key: "payment_terms", Label: "Payment terms (days)", Aliases: []string{"Payment Terms", "Terms (days)", "Due Days"}},
	{Key: "tax_number", Label: "Tax number (ABN/VAT/GST)", Aliases: []string{"Tax Registration Number", "Tax Reg No", "Tax Reg. No.", "ABN", "GST Identification Number (GSTIN)", "GST Number", "VAT Registration Number", "VAT Number", "TRN", "KRA PIN", "PIN", "Tax Number", "Tax ID"}},
	{Key: "notes", Label: "Notes", Aliases: []string{"Notes", "Remarks", "Comments"}},
	{Key: "status", Label: "Status (active/inactive)", Aliases: []string{"Status"}},
	{Key: "contact_type", Label: "Contact type (customer/vendor)", Aliases: []string{"Contact Type"}},
	{Key: "sub_type", Label: "Customer sub type (business/individual)", Aliases: []string{"Customer Sub Type"}},
}, addrFields...)

var personFields = []Field{
	{Key: "contact_id", Label: "Contact ID", Aliases: []string{"Contact ID", "Customer ID"}},
	{Key: "contact_name", Label: "Customer (display name)", Aliases: []string{"Display Name", "Contact Name", "Customer Name", "Company Name"}},
	{Key: "person_id", Label: "Contact person ID", Aliases: []string{"Contact Person ID", "ContactPerson ID"}},
	{Key: "first_name", Label: "First name", Aliases: []string{"First Name"}},
	{Key: "last_name", Label: "Last name", Aliases: []string{"Last Name"}},
	{Key: "email", Label: "Email", Required: true, Aliases: []string{"EmailID", "Email ID", "Email Address", "Email"}},
	{Key: "phone", Label: "Phone", Aliases: []string{"Phone", "Work Phone"}},
	{Key: "mobile", Label: "Mobile", Aliases: []string{"MobilePhone", "Mobile Phone", "Mobile"}},
	{Key: "designation", Label: "Designation", Aliases: []string{"Designation", "Title", "Job Title"}},
	{Key: "department", Label: "Department", Aliases: []string{"Department"}},
	{Key: "is_primary", Label: "Primary contact", Aliases: []string{"Is Primary Contact", "Primary Contact", "Is Primary"}},
}

var itemFields = []Field{
	{Key: "item_id", Label: "Item ID", Aliases: []string{"Item ID", "Product ID"}},
	{Key: "name", Label: "Name", Required: true, Aliases: []string{"Item Name", "Name", "Product Name", "Service Name"}},
	{Key: "sku", Label: "SKU", Aliases: []string{"SKU", "Code", "Item Code"}},
	{Key: "description", Label: "Description", Aliases: []string{"Description", "Sales Description", "Item Description"}},
	{Key: "rate", Label: "Rate / price", Aliases: []string{"Rate", "Selling Price", "Sales Rate", "Price", "Unit Price"}},
	{Key: "unit", Label: "Unit", Aliases: []string{"Usage unit", "Usage Unit", "Unit"}},
	{Key: "tax_name", Label: "Tax name", Aliases: []string{"Tax Name", "Tax"}},
	{Key: "tax_pct", Label: "Tax %", Aliases: []string{"Tax Percentage", "Tax %", "Tax Rate"}},
	{Key: "product_type", Label: "Type (goods/service)", Aliases: []string{"Product Type", "Item Type", "Type"}},
	{Key: "status", Label: "Status", Aliases: []string{"Status"}},
}

var docFields = append([]Field{
	{Key: "doc_id", Label: "Document ID", Aliases: []string{"Invoice ID", "Estimate ID", "CreditNotes ID", "Credit Note ID", "CreditNote ID", "Quote ID"}},
	{Key: "number", Label: "Number", Required: true, Aliases: []string{"Invoice Number", "Estimate Number", "Credit Note Number", "Quote Number", "Invoice #", "Invoice No", "Invoice No.", "Invoice", "Number", "Document Number"}},
	{Key: "date", Label: "Date", Required: true, Aliases: []string{"Invoice Date", "Estimate Date", "Credit Note Date", "Quote Date", "Issue Date", "Date"}},
	{Key: "due_date", Label: "Due / expiry date", Aliases: []string{"Due Date", "Expiry Date", "Valid Until", "Due"}},
	{Key: "status", Label: "Status", Aliases: []string{"Invoice Status", "Estimate Status", "Credit Note Status", "Quote Status", "Status"}},
	{Key: "customer_id", Label: "Customer ID", Aliases: []string{"Customer ID", "Contact ID"}},
	{Key: "customer_name", Label: "Customer name", Required: true, Aliases: []string{"Customer Name", "Customer", "Client Name", "Client", "Bill To", "Contact Name", "Display Name"}},
	{Key: "email", Label: "Customer email", Aliases: []string{"Customer Email", "Email", "EmailID", "Primary Contact EmailID", "Client Email"}},
	{Key: "currency", Label: "Currency", Aliases: []string{"Currency Code", "Currency"}},
	{Key: "exchange_rate", Label: "Exchange rate", Aliases: []string{"Exchange Rate"}},
	{Key: "reference", Label: "Reference / PO", Aliases: []string{"PurchaseOrder", "Purchase Order", "Reference Number", "Reference#", "Reference", "Order Number", "PO Number"}},
	{Key: "estimate_number", Label: "Quote it came from", Aliases: []string{"Estimate Number"}},
	{Key: "subtotal", Label: "Subtotal", Aliases: []string{"SubTotal", "Sub Total", "Subtotal"}},
	{Key: "tax_total", Label: "Tax total", Aliases: []string{"Tax Total", "Total Tax", "Tax Amount", "GST", "Tax"}},
	{Key: "total", Label: "Total", Aliases: []string{"Total", "Invoice Total", "Grand Total", "Total Amount", "Amount"}},
	{Key: "balance", Label: "Balance due", Aliases: []string{"Balance", "Balance Due", "Amount Due", "Outstanding", "Due Amount"}},
	{Key: "amount_paid", Label: "Amount paid", Aliases: []string{"Payment Made", "Amount Paid", "Paid", "Paid Amount"}},
	{Key: "paid_date", Label: "Paid date", Aliases: []string{"Paid Date", "Payment Date", "Last Payment Date"}},
	{Key: "shipping", Label: "Shipping", Aliases: []string{"Shipping Charge", "Shipping", "Freight"}},
	{Key: "adjustment", Label: "Adjustment", Aliases: []string{"Adjustment"}},
	{Key: "adjustment_desc", Label: "Adjustment description", Aliases: []string{"Adjustment Description"}},
	{Key: "discount_type", Label: "Discount type", Aliases: []string{"Discount Type"}},
	{Key: "entity_discount_pct", Label: "Document discount %", Aliases: []string{"Entity Discount Percent", "Discount Percent"}},
	{Key: "entity_discount_amt", Label: "Document discount amount", Aliases: []string{"Entity Discount Amount"}},
	{Key: "discount_before_tax", Label: "Discount before tax", Aliases: []string{"Is Discount Before Tax"}},
	{Key: "inclusive_tax", Label: "Prices include tax", Aliases: []string{"Is Inclusive Tax", "Tax Inclusive", "Amounts Are"}},
	{Key: "notes", Label: "Notes", Aliases: []string{"Notes", "Customer Notes"}},
	{Key: "terms", Label: "Terms", Aliases: []string{"Terms & Conditions", "Terms and Conditions", "Terms"}},
	{Key: "line_id", Label: "Line ID", Aliases: []string{"Line Item ID"}},
	{Key: "item_id", Label: "Item ID", Aliases: []string{"Item ID", "Product ID"}},
	{Key: "item_name", Label: "Item name", Aliases: []string{"Item Name", "Product", "Item", "Service"}},
	{Key: "item_desc", Label: "Item description", Aliases: []string{"Item Desc", "Item Description", "Description", "Line Description"}},
	{Key: "quantity", Label: "Quantity", Aliases: []string{"Quantity", "Qty", "Hours"}},
	{Key: "unit", Label: "Unit", Aliases: []string{"Usage unit", "Usage Unit", "Unit"}},
	{Key: "item_price", Label: "Unit price", Aliases: []string{"Item Price", "Unit Price", "Rate", "Price"}},
	{Key: "line_discount", Label: "Line discount", Aliases: []string{"Discount"}},
	{Key: "line_discount_amt", Label: "Line discount amount", Aliases: []string{"Discount Amount"}},
	{Key: "item_tax", Label: "Line tax name", Aliases: []string{"Item Tax", "Tax Name"}},
	{Key: "item_tax_pct", Label: "Line tax %", Aliases: []string{"Item Tax %", "Item Tax Percentage", "Tax Percentage", "Tax %", "Tax Rate", "GST %"}},
	{Key: "item_tax_amount", Label: "Line tax amount", Aliases: []string{"Item Tax Amount"}},
	{Key: "item_total", Label: "Line amount", Aliases: []string{"Item Total", "Line Total", "Line Amount"}},
}, addrFields...)

var paymentFields = []Field{
	{Key: "payment_id", Label: "Payment ID", Aliases: []string{"CustomerPayment ID", "Customer Payment ID", "Payment ID"}},
	{Key: "number", Label: "Payment number", Aliases: []string{"Payment Number", "Payment #", "Receipt Number"}},
	{Key: "date", Label: "Payment date", Required: true, Aliases: []string{"Date", "Payment Date"}},
	{Key: "mode", Label: "Payment mode", Aliases: []string{"Mode", "Payment Mode", "Payment Method", "Method"}},
	{Key: "customer_id", Label: "Customer ID", Aliases: []string{"Customer ID"}},
	{Key: "customer_name", Label: "Customer name", Aliases: []string{"Customer Name", "Customer"}},
	{Key: "amount", Label: "Payment amount", Aliases: []string{"Amount", "Payment Amount", "Amount Received"}},
	{Key: "unused", Label: "Unused amount", Aliases: []string{"Unused Amount", "Excess Amount"}},
	{Key: "reference", Label: "Reference", Aliases: []string{"Reference Number", "Reference#", "Reference"}},
	{Key: "description", Label: "Description", Aliases: []string{"Description", "Notes"}},
	{Key: "currency", Label: "Currency", Aliases: []string{"Currency Code", "Currency"}},
	{Key: "invoice_number", Label: "Invoice number", Required: true, Aliases: []string{"Invoice Number", "Invoice #", "Invoice"}},
	{Key: "applied", Label: "Amount applied to invoice", Aliases: []string{"Amount Applied to Invoice", "Amount Applied", "Applied Amount", "Invoice Amount Applied"}},
	{Key: "applied_date", Label: "Applied date", Aliases: []string{"Invoice Payment Applied Date", "Applied Date"}},
}

// FileKinds lists the supported file kinds and their fields.
var FileKinds = []FileKind{
	{FileContacts, "Zoho Books — Contacts (Contacts.csv)", true, contactFields},
	{FilePersons, "Zoho Books — Contact persons (Contact_Persons.csv)", true, personFields},
	{FileItems, "Zoho Books — Items (Item.csv)", true, itemFields},
	{FileInvoices, "Zoho Books — Invoices (Invoice.csv)", true, docFields},
	{FileQuotes, "Zoho Books — Estimates / quotes (Estimate.csv)", true, docFields},
	{FileCredits, "Zoho Books — Credit notes (Credit_Note.csv)", true, docFields},
	{FilePayments, "Zoho Books — Customer payments (Customer_Payment.csv)", true, paymentFields},
	{FileGenCust, "Customers from another tool (CSV)", false, contactFields},
	{FileGenInv, "Invoices from another tool (CSV)", false, docFields},
}

func kindByName(k string) *FileKind {
	for i := range FileKinds {
		if FileKinds[i].Kind == k {
			return &FileKinds[i]
		}
	}
	return nil
}

func normHeader(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '%' || r == '#' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ReadTable reads a CSV (comma, semicolon or tab separated; UTF-8 with or
// without BOM) or an .xlsx file. Old binary .xls files are refused.
func ReadTable(name string, data []byte) ([]string, [][]string, error) {
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0xD0, 0xCF, 0x11, 0xE0}) {
		return nil, nil, errors.New("old Excel .xls files can't be read — in Zoho Books export as CSV (or XLSX), or save the file as .xlsx / .csv in Excel")
	}
	var rows [][]string
	if len(data) >= 2 && data[0] == 'P' && data[1] == 'K' {
		r, err := readXLSX(data)
		if err != nil {
			return nil, nil, err
		}
		rows = r
	} else {
		data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
		if bytes.HasPrefix(data, []byte{0xFF, 0xFE}) || bytes.HasPrefix(data, []byte{0xFE, 0xFF}) {
			return nil, nil, errors.New("UTF-16 files aren't supported — save the file as CSV UTF-8")
		}
		first := data
		if i := bytes.IndexByte(data, '\n'); i > 0 {
			first = data[:i]
		}
		r := csv.NewReader(bytes.NewReader(data))
		r.FieldsPerRecord = -1
		r.LazyQuotes = true
		switch {
		case bytes.Count(first, []byte{'\t'}) > bytes.Count(first, []byte{','}):
			r.Comma = '\t'
		case bytes.Count(first, []byte{';'}) > bytes.Count(first, []byte{','}):
			r.Comma = ';'
		}
		all, err := r.ReadAll()
		if err != nil {
			return nil, nil, fmt.Errorf("could not read %s as CSV: %v", name, err)
		}
		rows = all
	}
	// Skip empty leading rows.
	for len(rows) > 0 && strings.TrimSpace(strings.Join(rows[0], "")) == "" {
		rows = rows[1:]
	}
	if len(rows) == 0 {
		return nil, nil, errors.New(name + " is empty")
	}
	headers := make([]string, len(rows[0]))
	for i, h := range rows[0] {
		headers[i] = strings.TrimSpace(h)
	}
	var body [][]string
	for _, r := range rows[1:] {
		if strings.TrimSpace(strings.Join(r, "")) == "" {
			continue
		}
		body = append(body, r)
	}
	return headers, body, nil
}

// DetectKind guesses the file kind from its headers (and file name).
func DetectKind(name string, headers []string) string {
	h := map[string]bool{}
	for _, x := range headers {
		h[normHeader(x)] = true
	}
	has := func(names ...string) bool {
		for _, n := range names {
			if h[normHeader(n)] {
				return true
			}
		}
		return false
	}
	ln := strings.ToLower(name)
	switch {
	case has("CustomerPayment ID", "Payment Number") || (has("Amount Applied to Invoice") && has("Mode")):
		return FilePayments
	case has("Credit Note Number", "CreditNotes ID"):
		return FileCredits
	case has("Estimate Number", "Estimate ID") && !has("Invoice Number"):
		return FileQuotes
	case has("Invoice Number") && has("Invoice ID", "Invoice Status", "Item Name", "Item Price"):
		return FileInvoices
	case has("Contact Person ID") || (strings.Contains(ln, "person") && has("Contact ID")):
		return FilePersons
	case has("Contact ID", "Contact Type", "Customer Sub Type") && has("Display Name", "Contact Name"):
		return FileContacts
	case has("Item ID") && has("Item Name", "Rate") && !has("Invoice Number"):
		return FileItems
	case has("Invoice Number", "Invoice #", "Invoice No", "Invoice", "Number") && has("Total", "Amount", "Unit Price", "Price", "Rate", "Invoice Total"):
		return FileGenInv
	case has("Email", "Email Address", "Name", "Customer Name", "Client Name", "Company", "First Name"):
		return FileGenCust
	}
	return ""
}

// AutoMap maps each field of kind to the first matching header.
func AutoMap(kind string, headers []string) map[string]string {
	fk := kindByName(kind)
	out := map[string]string{}
	if fk == nil {
		return out
	}
	byNorm := map[string]string{}
	for _, x := range headers {
		if _, ok := byNorm[normHeader(x)]; !ok {
			byNorm[normHeader(x)] = x
		}
	}
	used := map[string]bool{}
	for _, f := range fk.Fields {
		for _, a := range f.Aliases {
			if hd, ok := byNorm[normHeader(a)]; ok && !used[hd] {
				out[f.Key] = hd
				used[hd] = true
				break
			}
		}
	}
	// Generic invoices: a single "Amount" column with several rows per
	// invoice is the line amount, not the total — leave it as total; the
	// user can change it in the mapping.
	return out
}

// table is a parsed file with its mapping.
type table struct {
	upload  Upload
	headers []string
	rows    [][]string
	idx     map[string]int // field → column
}

func newTable(u Upload) (*table, error) {
	headers, rows, err := ReadTable(u.Name, u.Data)
	if err != nil {
		return nil, err
	}
	t := &table{upload: u, headers: headers, rows: rows, idx: map[string]int{}}
	for field, header := range u.Mapping {
		if header == "" {
			continue
		}
		for i, hd := range headers {
			if hd == header {
				t.idx[field] = i
				break
			}
		}
	}
	return t, nil
}

func (t *table) get(row []string, field string) string {
	i, ok := t.idx[field]
	if !ok || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

func (t *table) num(row []string, field string) float64 {
	v, _ := parseNum(t.get(row, field))
	return v
}

func (t *table) has(field string) bool { _, ok := t.idx[field]; return ok }

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "y", "1", "primary":
		return true
	}
	return false
}

// BuildDataset turns uploaded files into normalised records. Rows that can't
// be used are reported in Errors.
func BuildDataset(uploads []Upload, dayFirst bool) (*Dataset, error) {
	ds := &Dataset{}
	var tables []*table
	for _, u := range uploads {
		if u.Kind == "" || u.Kind == FileIgnore {
			continue
		}
		t, err := newTable(u)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", u.Name, err)
		}
		tables = append(tables, t)
	}
	rowErr := func(t *table, i int, msg string) {
		ds.Errors = append(ds.Errors, JobError{Entity: t.upload.Kind, Label: t.upload.Name, Severity: "error", Message: msg, Row: i + 2})
	}

	// Items.
	for _, t := range tables {
		if t.upload.Kind != FileItems {
			continue
		}
		for i, r := range t.rows {
			name := t.get(r, "name")
			if name == "" {
				rowErr(t, i, "Item without a name")
				continue
			}
			typ := strings.ToLower(t.get(r, "product_type"))
			if typ != "goods" {
				typ = "service"
			}
			ds.Items = append(ds.Items, Item{ExtID: t.get(r, "item_id"), Name: name, Description: t.get(r, "description"), Rate: t.num(r, "rate"),
				Unit: t.get(r, "unit"), SKU: t.get(r, "sku"), TaxName: t.get(r, "tax_name"), TaxPercent: t.num(r, "tax_pct"), Type: typ,
				Active: !strings.EqualFold(t.get(r, "status"), "inactive"), Row: i + 2})
		}
	}

	// Contacts.
	byID := map[string]int{}
	byName := map[string]int{}
	for _, t := range tables {
		if t.upload.Kind != FileContacts && t.upload.Kind != FileGenCust {
			continue
		}
		for i, r := range t.rows {
			if ct := strings.ToLower(t.get(r, "contact_type")); ct == "vendor" || ct == "supplier" {
				continue
			}
			c := Contact{ExtID: t.get(r, "contact_id"), Name: t.get(r, "display_name"), Company: t.get(r, "company_name"),
				FirstName: t.get(r, "first_name"), LastName: t.get(r, "last_name"), Email: strings.ToLower(t.get(r, "email")),
				Phone: t.get(r, "phone"), Mobile: t.get(r, "mobile"), Website: t.get(r, "website"), Currency: strings.ToUpper(t.get(r, "currency")),
				TaxNumber: t.get(r, "tax_number"), Notes: t.get(r, "notes"), Active: !strings.EqualFold(t.get(r, "status"), "inactive"),
				Individual: strings.EqualFold(t.get(r, "sub_type"), "individual"), Row: i + 2,
				Billing: Address{Attention: t.get(r, "billing_attention"), Street: t.get(r, "billing_street"), Street2: t.get(r, "billing_street2"),
					City: t.get(r, "billing_city"), State: t.get(r, "billing_state"), Zip: t.get(r, "billing_zip"), Country: t.get(r, "billing_country"), Phone: t.get(r, "billing_phone")}}
			if t.upload.Kind == FileGenCust && c.ExtID != "" {
				c.ExtID = "id:" + c.ExtID
			}
			if pt := t.get(r, "payment_terms"); pt != "" {
				if v, ok := parseNum(pt); ok {
					d := int(v)
					c.TermsDays = &d
				}
			}
			if c.Name == "" {
				c.Name = firstNonEmpty(c.Company, strings.TrimSpace(c.FirstName+" "+c.LastName))
			}
			if t.upload.Kind == FileGenCust && c.FirstName != "" && c.Company == "" {
				c.Individual = true
			}
			if c.Name == "" && c.Email == "" {
				rowErr(t, i, "Customer without a name or email")
				continue
			}
			if c.Email != "" && (c.FirstName != "" || c.LastName != "") {
				c.Persons = append(c.Persons, Person{FirstName: c.FirstName, LastName: c.LastName, Email: c.Email, Phone: c.Phone, Mobile: c.Mobile, Primary: true})
			}
			ds.Contacts = append(ds.Contacts, c)
			idx := len(ds.Contacts) - 1
			if c.ExtID != "" {
				byID[c.ExtID] = idx
			}
			if k := lowerTrim(c.Name); k != "" {
				byName[k] = idx
			}
		}
	}
	for _, t := range tables {
		if t.upload.Kind != FilePersons {
			continue
		}
		for i, r := range t.rows {
			idx, ok := byID[t.get(r, "contact_id")]
			if !ok {
				idx, ok = byName[lowerTrim(t.get(r, "contact_name"))]
			}
			if !ok {
				rowErr(t, i, "Contact person for a customer that is not in the contacts file")
				continue
			}
			role := strings.Join(nonEmpty(t.get(r, "designation"), t.get(r, "department")), ", ")
			p := Person{ExtID: t.get(r, "person_id"), FirstName: t.get(r, "first_name"), LastName: t.get(r, "last_name"), Email: strings.ToLower(t.get(r, "email")),
				Phone: t.get(r, "phone"), Mobile: t.get(r, "mobile"), Role: role, Primary: truthy(t.get(r, "is_primary"))}
			c := &ds.Contacts[idx]
			dup := false
			for j := range c.Persons {
				if p.Email != "" && strings.EqualFold(c.Persons[j].Email, p.Email) {
					// Same person as the contact's primary: keep the richer record.
					c.Persons[j].ExtID = firstNonEmpty(p.ExtID, c.Persons[j].ExtID)
					c.Persons[j].Role = firstNonEmpty(p.Role, c.Persons[j].Role)
					dup = true
				}
			}
			if !dup {
				if p.Primary {
					for j := range c.Persons {
						c.Persons[j].Primary = false
					}
				}
				c.Persons = append(c.Persons, p)
			}
		}
	}

	// Documents (one row per line, grouped by number).
	type key struct{ kind, num string }
	docIdx := map[key]int{}
	for _, t := range tables {
		kind := ""
		switch t.upload.Kind {
		case FileInvoices, FileGenInv:
			kind = KindInvoice
		case FileQuotes:
			kind = KindQuote
		case FileCredits:
			kind = KindCredit
		default:
			continue
		}
		generic := t.upload.Kind == FileGenInv
		for i, r := range t.rows {
			num := t.get(r, "number")
			if num == "" {
				rowErr(t, i, "Row without a document number")
				continue
			}
			k := key{kind, strings.ToLower(num)}
			di, seen := docIdx[k]
			if !seen {
				d := Doc{Kind: kind, ExtID: t.get(r, "doc_id"), Number: num, ContactID: t.get(r, "customer_id"), CustomerName: t.get(r, "customer_name"),
					Email: strings.ToLower(t.get(r, "email")), Status: strings.ToLower(t.get(r, "status")), Date: t.get(r, "date"), DueDate: t.get(r, "due_date"),
					Currency: strings.ToUpper(t.get(r, "currency")), ExchangeRate: t.num(r, "exchange_rate"), Reference: t.get(r, "reference"),
					SubTotal: t.num(r, "subtotal"), Total: t.num(r, "total"), Shipping: t.num(r, "shipping"), Adjustment: t.num(r, "adjustment"),
					AdjustmentDesc: t.get(r, "adjustment_desc"), InclusiveTax: truthy(t.get(r, "inclusive_tax")) || strings.EqualFold(t.get(r, "inclusive_tax"), "tax inclusive"),
					Notes: t.get(r, "notes"), Terms: t.get(r, "terms"), Row: i + 2, TaxUnknown: true, LastPaymentDate: t.get(r, "paid_date"),
					BillingAddress: formatAddress(Address{Attention: t.get(r, "billing_attention"), Street: t.get(r, "billing_street"), Street2: t.get(r, "billing_street2"),
						City: t.get(r, "billing_city"), State: t.get(r, "billing_state"), Zip: t.get(r, "billing_zip"), Country: t.get(r, "billing_country")})}
				if generic {
					d.ExtID = ""
					if d.ContactID != "" {
						d.ContactID = "id:" + d.ContactID
					}
				}
				if t.has("tax_total") {
					d.TaxTotal, d.TaxUnknown = t.num(r, "tax_total"), false
				}
				if d.Date == "" {
					rowErr(t, i, "Document "+num+" has no date")
					continue
				}
				if p := t.num(r, "entity_discount_pct"); p > 0 {
					d.DiscountPct = p
				} else if a := t.num(r, "entity_discount_amt"); a > 0 {
					d.DiscountAmt = a
				}
				if v := strings.ToLower(t.get(r, "discount_before_tax")); v == "false" && (d.DiscountPct > 0 || d.DiscountAmt > 0) {
					d.DiscountAfterTax = true
				}
				if t.has("balance") {
					d.Balance = t.num(r, "balance")
				} else {
					d.BalanceUnknown = true // total − payments once the total is known
				}
				if t.has("amount_paid") && t.num(r, "amount_paid") > 0 {
					d.PaymentMade = t.num(r, "amount_paid")
				}
				ds.Docs = append(ds.Docs, d)
				di = len(ds.Docs) - 1
				docIdx[k] = di
			}
			d := &ds.Docs[di]
			if t.has("item_tax_amount") {
				if d.TaxUnknown && !t.has("tax_total") {
					d.TaxUnknown = false
				}
				if !t.has("tax_total") {
					d.TaxTotal += t.num(r, "item_tax_amount")
				}
			}
			name, desc := t.get(r, "item_name"), t.get(r, "item_desc")
			qty, price := t.num(r, "quantity"), t.num(r, "item_price")
			if name == "" && desc == "" && price == 0 && !t.has("item_total") {
				continue
			}
			if name == "" && desc == "" && price == 0 && t.num(r, "item_total") == 0 {
				continue
			}
			if !t.has("quantity") || t.get(r, "quantity") == "" {
				qty = 1
			}
			if price == 0 && t.has("item_total") && qty != 0 {
				price = t.num(r, "item_total") / qty
			}
			l := Line{ExtID: t.get(r, "line_id"), ItemID: t.get(r, "item_id"), Name: name, Description: desc, Quantity: qty, Unit: t.get(r, "unit"), Rate: price,
				TaxName: t.get(r, "item_tax"), TaxPercent: t.num(r, "item_tax_pct"), Total: t.num(r, "item_total")}
			ld := t.get(r, "line_discount")
			if strings.HasSuffix(ld, "%") {
				l.DiscountPct, _ = parseNum(ld)
			} else if a := t.num(r, "line_discount_amt"); a > 0 {
				l.DiscountAmt = a
			} else if v, ok := parseNum(ld); ok && v > 0 {
				l.DiscountAmt = v
			}
			d.Lines = append(d.Lines, l)
		}
	}

	// Payments applied to invoices.
	invByNum := map[string]int{}
	for i, d := range ds.Docs {
		if d.Kind == KindInvoice {
			invByNum[strings.ToLower(d.Number)] = i
		}
	}
	unappliedSeen := map[string]bool{}
	hasPayFile := false
	for _, t := range tables {
		if t.upload.Kind != FilePayments {
			continue
		}
		hasPayFile = true
		for i, r := range t.rows {
			pid := firstNonEmpty(t.get(r, "payment_id"), t.get(r, "number"), fmt.Sprintf("row%d", i+2))
			if u := t.num(r, "unused"); u > 0.004 && !unappliedSeen[pid] {
				unappliedSeen[pid] = true
				ds.Unapplied = append(ds.Unapplied, fmt.Sprintf("Payment %s from %s has %s %.2f not applied to any invoice — not imported (record it as a deposit or credit if needed).",
					firstNonEmpty(t.get(r, "number"), pid), firstNonEmpty(t.get(r, "customer_name"), "a customer"), t.get(r, "currency"), u))
			}
			in := strings.ToLower(t.get(r, "invoice_number"))
			if in == "" {
				continue
			}
			di, ok := invByNum[in]
			if !ok {
				rowErr(t, i, "Payment "+firstNonEmpty(t.get(r, "number"), pid)+" is for invoice "+t.get(r, "invoice_number")+", which is not in the invoice file")
				continue
			}
			amt := t.num(r, "amount")
			if t.has("applied") {
				amt = t.num(r, "applied")
			}
			if amt == 0 {
				continue
			}
			ref := strings.Join(nonEmpty(t.get(r, "number"), t.get(r, "reference")), " · ")
			d := &ds.Docs[di]
			d.Payments = append(d.Payments, Pay{ExtID: pid + ":" + firstNonEmpty(d.ExtID, d.Number), Number: t.get(r, "number"),
				Date: firstNonEmpty(t.get(r, "applied_date"), t.get(r, "date")), Amount: amt, Mode: t.get(r, "mode"), Reference: ref, Notes: t.get(r, "description")})
		}
	}

	// Derive what the files left out.
	for i := range ds.Docs {
		d := &ds.Docs[i]
		paid := 0.0
		for _, p := range d.Payments {
			paid += p.Amount
		}
		if d.Kind == KindInvoice && len(d.Payments) == 0 && d.PaymentMade > 0 {
			d.Payments = append(d.Payments, Pay{ExtID: "paid:" + firstNonEmpty(d.ExtID, d.Number), Date: firstNonEmpty(d.LastPaymentDate, d.DueDate, d.Date),
				Amount: d.PaymentMade, Mode: "other", Reference: "Recorded as paid in the import file"})
			paid = d.PaymentMade
		}
		if d.BalanceUnknown {
			st := mapStatus(d.Kind, d.Status)
			if st == "paid" || st == "void" {
				d.Balance, d.BalanceUnknown = 0, false
			}
		}
		_ = paid
	}
	if !hasPayFile {
		for _, d := range ds.Docs {
			if d.Kind == KindInvoice && d.ExtID != "" {
				ds.Unapplied = append(ds.Unapplied, "No customer payments file was included — paid invoices will show as settled without payment records. Add Customer_Payment.csv to import the payments.")
				break
			}
		}
	}
	sort.SliceStable(ds.Docs, func(i, j int) bool { return docOrder(ds.Docs[i].Kind) < docOrder(ds.Docs[j].Kind) })
	return ds, nil
}

func docOrder(k string) int {
	switch k {
	case KindQuote:
		return 0
	case KindInvoice:
		return 1
	case KindCredit:
		return 2
	}
	return 3
}

// GuessDayFirst looks at date columns: dd/mm when any first part > 12,
// mm/dd when any second part > 12, else def.
func GuessDayFirst(ds *Dataset, def bool) bool {
	for _, d := range ds.Docs {
		for _, s := range []string{d.Date, d.DueDate} {
			p := strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '.' })
			if len(p) != 3 || len(p[0]) > 2 {
				continue
			}
			a, _ := parseNum(p[0])
			b, _ := parseNum(p[1])
			if a > 12 {
				return true
			}
			if b > 12 {
				return false
			}
		}
	}
	return def
}

var _ = time.Now
