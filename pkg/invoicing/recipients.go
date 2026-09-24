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
	Source string `json:"source"` // client | primary_billing | contact | document
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
}

func (c customerRow) name() string { return strings.TrimSpace(c.FirstName + " " + c.LastName) }

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
	IsPrimaryBilling bool
}

func (h *Handler) hasTable(name string) bool { return h.DB.Migrator().HasTable(name) }

const customerCols = "id, first_name, last_name, email, phone, address_street, address_suburb, address_state, address_postcode, address_country, country_code, currency"

func (h *Handler) loadCustomers(org string, ids []string, limit int) []customerRow {
	out := []customerRow{}
	if !h.hasTable("customers") {
		return out
	}
	q := h.DB.Table("customers").Select(customerCols).Where("organization_id = ? AND deleted_at IS NULL", org)
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
	h.DB.Table("customer_contacts").Select("name, email, role, cc_invoices, cc_quotes, cc_reminders, is_primary_billing").
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
	b := customerBilling{CustomerID: cu.ID, Name: cu.name(), Email: cu.Email, Phone: cu.Phone, Address: cu.address(),
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
	kind := kindFor(inv, c.Query("kind"))
	to, cc := h.recipientsFor(org, inv, kind)
	ok(c, http.StatusOK, gin.H{"kind": kind, "to": to, "cc": cc, "email_enabled": h.Mailer != nil && h.Mailer.Configured()})
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
