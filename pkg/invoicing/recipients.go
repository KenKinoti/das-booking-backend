package invoicing

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Email kinds decide which customer contacts are copied.
const (
	KindInvoice  = "invoice"
	KindQuote    = "quote"
	KindReminder = "reminder"
)

// Recipient is a suggested To/CC address with where it came from.
type Recipient struct {
	Email  string `json:"email"`
	Name   string `json:"name,omitempty"`
	Role   string `json:"role,omitempty"`
	Source string `json:"source"` // client | primary_billing | contact | document | settings
}

// customerRow is the subset of the core customers table invoicing reads.
type customerRow struct {
	ID              string
	FirstName       string
	LastName        string
	Email           string
	Phone           string
	AddressStreet   string
	AddressSuburb   string
	AddressState    string
	AddressPostcode string
	AddressCountry  string
	CountryCode     string
	Currency        string
	CompanyName     string
	TaxNumber       string
}

func (c customerRow) name() string { return strings.TrimSpace(c.FirstName + " " + c.LastName) }

// isCompany: the customer record has a company name.
func (c customerRow) isCompany() bool { return strings.TrimSpace(c.CompanyName) != "" }

// billName is the name a document is billed to: the company when there is one.
func (c customerRow) billName() string {
	if c.isCompany() {
		return strings.TrimSpace(c.CompanyName)
	}
	return c.name()
}

// contact is the person at a company customer ("" when the person's name is
// just the company name again, or for individuals).
func (c customerRow) contact() string {
	if !c.isCompany() {
		return ""
	}
	n := c.name()
	if strings.EqualFold(n, strings.TrimSpace(c.CompanyName)) {
		return ""
	}
	return n
}

func (c customerRow) address() string {
	lines := nonEmpty(c.AddressStreet, strings.TrimSpace(strings.Join(nonEmpty(c.AddressSuburb, c.AddressState, c.AddressPostcode), " ")))
	if c.AddressCountry != "" && c.CountryCode != "" && c.CountryCode != "AU" {
		lines = append(lines, c.AddressCountry)
	}
	return strings.Join(lines, "\n")
}

type contactRow struct {
	Name             string
	Email            string
	Role             string
	CCInvoices       bool
	CCQuotes         bool
	CCReminders      bool
	CCReceipts       bool
	IsPrimaryBilling bool
}

func (h *Handler) hasTable(name string) bool { return h.DB.Migrator().HasTable(name) }

const customerCols = "id, first_name, last_name, email, phone, address_street, address_suburb, address_state, address_postcode, address_country, country_code, currency"

// customerSelect adds the business columns when the customers table has them
// (it always does in production; some test schemas are minimal).
func (h *Handler) customerSelect() string {
	cols := customerCols
	m := h.DB.Migrator()
	if m.HasColumn("customers", "company_name") {
		cols += ", COALESCE(company_name, '') AS company_name"
	}
	if m.HasColumn("customers", "tax_number") {
		cols += ", COALESCE(tax_number, '') AS tax_number"
	}
	return cols
}

// companyFromCustomer fills in company billing for a document linked to a
// customer whose record has a company name, unless the request says itself
// whether it bills a company. The person on the record becomes the contact.
func (h *Handler) companyFromCustomer(tx *gorm.DB, org string, in *invoiceInput) {
	if in.ClientIsCompany != nil || in.CustomerID == nil || strings.TrimSpace(*in.CustomerID) == "" || !h.hasTable("customers") {
		return
	}
	var cu customerRow
	if err := tx.Table("customers").Select(h.customerSelect()).
		Where("id = ? AND organization_id = ? AND deleted_at IS NULL", *in.CustomerID, org).Limit(1).Scan(&cu).Error; err != nil || cu.ID == "" || !cu.isCompany() {
		return
	}
	name := strings.TrimSpace(in.ClientName)
	if name != "" && !strings.EqualFold(name, cu.billName()) && !strings.EqualFold(name, cu.name()) {
		return // a different name was typed on purpose
	}
	yes := true
	in.ClientIsCompany = &yes
	in.ClientName = cu.billName()
	if in.ClientContact == nil {
		contact := cu.contact()
		in.ClientContact = &contact
	}
	if strings.TrimSpace(in.ClientTaxNumber) == "" {
		in.ClientTaxNumber = cu.TaxNumber
	}
}

func (h *Handler) loadCustomers(org string, ids []string, limit int) []customerRow {
	out := []customerRow{}
	if !h.hasTable("customers") {
		return out
	}
	q := h.DB.Table("customers").Select(h.customerSelect()).Where("organization_id = ? AND deleted_at IS NULL", org)
	if ids != nil {
		if len(ids) == 0 {
			return out
		}
		q = q.Where("id IN ?", ids)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	q.Scan(&out)
	return out
}

func (h *Handler) loadContacts(org, customerID string) []contactRow {
	out := []contactRow{}
	if customerID == "" || !h.hasTable("customer_contacts") {
		return out
	}
	cols := "name, email, role, cc_invoices, cc_quotes, cc_reminders, is_primary_billing"
	if h.DB.Migrator().HasColumn("customer_contacts", "cc_receipts") {
		cols += ", cc_receipts"
	} else {
		cols += ", cc_invoices AS cc_receipts"
	}
	h.DB.Table("customer_contacts").Select(cols).
		Where("organization_id = ? AND customer_id = ? AND deleted_at IS NULL", org, customerID).
		Order("is_primary_billing DESC, created_at ASC").Scan(&out)
	return out
}

func (c contactRow) wants(kind string) bool {
	switch kind {
	case KindQuote:
		return c.CCQuotes
	case KindReminder:
		return c.CCReminders
	case KindReceipt:
		return c.CCReceipts
	default:
		return c.CCInvoices
	}
}

// customerBilling is what a new document for a customer should start from.
type customerBilling struct {
	CustomerID     string      `json:"customer_id"`
	Name           string      `json:"name"`
	Email          string      `json:"email"`
	Phone          string      `json:"phone"`
	Address        string      `json:"address"`
	IsCompany      bool        `json:"is_company"`
	Contact        string      `json:"contact"`    // the person at a company customer
	TaxNumber      string      `json:"tax_number"` // from the customer record
	Currency       string      `json:"currency"`
	CurrencySource string      `json:"currency_source"` // customer | country | default
	CountryCode    string      `json:"country_code"`
	CountryName    string      `json:"country_name"`
	To             *Recipient  `json:"to"`
	CC             []Recipient `json:"cc"`
	CCEmails       []string    `json:"cc_emails"` // for the document (invoice/quote contacts)
}

func (h *Handler) billingFor(cu customerRow, orgDefault, kind, org string) customerBilling {
	cur, src := currency.ForCustomer(cu.Currency, cu.CountryCode, cu.AddressCountry, orgDefault)
	code := cu.CountryCode
	if code == "" {
		code = currency.CountryCode(cu.AddressCountry)
	}
	name := currency.CountryName(code)
	if name == "" {
		name = cu.AddressCountry
	}
	b := customerBilling{CustomerID: cu.ID, Name: cu.billName(), Email: cu.Email, Phone: cu.Phone, Address: cu.address(),
		IsCompany: cu.isCompany(), Contact: cu.contact(), TaxNumber: cu.TaxNumber,
		Currency: cur, CurrencySource: src, CountryCode: code, CountryName: name, CC: []Recipient{}, CCEmails: []string{}}
	to, cc := suggest(cu.Email, h.loadContacts(org, cu.ID), nil, kind)
	b.To, b.CC = to, cc
	for _, r := range cc {
		b.CCEmails = append(b.CCEmails, r.Email)
	}
	return b
}

// suggest picks the To address (primary billing contact, else the client
// email) and the CC list (the document's saved CCs plus contacts flagged for
// this kind of email), de-duplicated and without invalid addresses.
func suggest(clientEmail string, contacts []contactRow, docCC []string, kind string) (*Recipient, []Recipient) {
	var to *Recipient
	for _, c := range contacts {
		if c.IsPrimaryBilling && ValidEmail(strings.ToLower(c.Email)) {
			to = &Recipient{Email: strings.ToLower(c.Email), Name: c.Name, Role: c.Role, Source: "primary_billing"}
			break
		}
	}
	if to == nil && ValidEmail(strings.ToLower(strings.TrimSpace(clientEmail))) {
		to = &Recipient{Email: strings.ToLower(strings.TrimSpace(clientEmail)), Source: "client"}
	}
	seen := map[string]bool{}
	if to != nil {
		seen[to.Email] = true
	}
	cc := []Recipient{}
	add := func(r Recipient) {
		r.Email = strings.ToLower(strings.TrimSpace(r.Email))
		if r.Email == "" || seen[r.Email] || !ValidEmail(r.Email) {
			return
		}
		seen[r.Email] = true
		cc = append(cc, r)
	}
	byEmail := map[string]contactRow{}
	for _, c := range contacts {
		byEmail[strings.ToLower(c.Email)] = c
	}
	for _, e := range docCC {
		if c, ok := byEmail[strings.ToLower(strings.TrimSpace(e))]; ok {
			if kind == KindReceipt && !c.CCReceipts {
				continue // the contact doesn't want receipts
			}
			add(Recipient{Email: e, Name: c.Name, Role: c.Role, Source: "contact"})
		} else {
			add(Recipient{Email: e, Source: "document"})
		}
	}
	for _, c := range contacts {
		if c.wants(kind) {
			add(Recipient{Email: c.Email, Name: c.Name, Role: c.Role, Source: "contact"})
		}
	}
	return to, cc
}

func kindFor(inv *Invoice, requested string) string {
	if inv.DocType == TypeQuote {
		return KindQuote
	}
	if requested == KindReminder && inv.Status != StatusDraft {
		return KindReminder
	}
	return KindInvoice
}

func (h *Handler) recipientsFor(org string, inv *Invoice, kind string) (*Recipient, []Recipient) {
	var contacts []contactRow
	clientEmail := inv.ClientEmail
	if inv.CustomerID != nil && *inv.CustomerID != "" {
		contacts = h.loadContacts(org, *inv.CustomerID)
		if clientEmail == "" {
			if cs := h.loadCustomers(org, []string{*inv.CustomerID}, 1); len(cs) == 1 {
				clientEmail = cs[0].Email
			}
		}
	}
	return suggest(clientEmail, contacts, inv.CCEmails, kind)
}

// Recipients GET /invoices/:id/recipients?kind=invoice|quote|reminder
func (h *Handler) Recipients(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	inv, err := h.load(h.DB, org, c.Param("id"), false)
	if err != nil {
		failErr(c, err)
		return
	}
	kind := kindOf(inv, c.Query("kind"))
	to, cc := h.recipientsFor(org, inv, kind)
	out := gin.H{"kind": kind, "to": to, "cc": cc, "bcc": []Recipient{}, "email_enabled": h.emailEnabled(org)}
	if s := h.sender(org); s != nil {
		// The email settings' "Always CC/BCC" addresses, merged and de-duplicated
		// so the send dialog shows every recipient.
		seen := map[string]bool{}
		if to != nil {
			seen[to.Email] = true
		}
		for _, r := range cc {
			seen[r.Email] = true
		}
		for _, e := range s.AlwaysCC {
			if !seen[e] {
				seen[e] = true
				cc = append(cc, Recipient{Email: e, Name: "Always CC", Source: "settings"})
			}
		}
		bcc := []Recipient{}
		for _, e := range s.AlwaysBCC {
			if !seen[e] {
				seen[e] = true
				bcc = append(bcc, Recipient{Email: e, Name: "Always BCC", Source: "settings"})
			}
		}
		out["cc"], out["bcc"], out["sender"] = cc, bcc, s
	}
	ok(c, http.StatusOK, out)
}

// CustomerDefaults GET /customer-defaults/:customerId?type=invoice|quote
// returns everything a new document for this customer should be prefilled with.
func (h *Handler) CustomerDefaults(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	cs := h.loadCustomers(org, []string{c.Param("customerId")}, 1)
	if len(cs) == 0 {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Customer not found")
		return
	}
	s, err := h.settingsFor(h.DB, org, false)
	if err != nil {
		failErr(c, err)
		return
	}
	kind := KindInvoice
	if c.Query("type") == TypeQuote {
		kind = KindQuote
	}
	ok(c, http.StatusOK, h.billingFor(cs[0], s.Currency, kind, org))
}

// customerCurrency is the billing currency for a customer id ("" if unknown).
func (h *Handler) customerCurrency(tx *gorm.DB, org, customerID, orgDefault string) string {
	if customerID == "" || !h.hasTable("customers") {
		return ""
	}
	var cu customerRow
	if err := tx.Table("customers").Select(customerCols).Where("id = ? AND organization_id = ? AND deleted_at IS NULL", customerID, org).Limit(1).Scan(&cu).Error; err != nil || cu.ID == "" {
		return ""
	}
	cur, _ := currency.ForCustomer(cu.Currency, cu.CountryCode, cu.AddressCountry, orgDefault)
	return cur
}
