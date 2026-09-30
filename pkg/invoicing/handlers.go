package invoicing

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Mailer sends an invoice email. It is optional; when nil, "send" only records
// the invoice as sent and the client opens the user's mail app instead.
type Mailer interface {
	Configured() bool
	Send(to, subject, body string) error
}

// LogoURL resolves an organisation's company logo URL (absolute when the
// origin is known). It is set by the branding module; the organisation logo
// is the only logo used on documents — billing_settings.logo_url is legacy.
var LogoURL func(r *http.Request, org string) string

func logoFor(c *gin.Context, org string) string {
	if LogoURL == nil {
		return ""
	}
	var r *http.Request
	if c != nil {
		r = c.Request
	}
	return LogoURL(r, org)
}

// Handler serves the invoicing API.
type Handler struct {
	DB        *gorm.DB
	Mailer    Mailer        // legacy/test mailer; when set it is used instead of Mail
	Mail      *mail.Service // resolves the organisation's outgoing email account
	PublicURL string        // e.g. https://das-erp-frontend.web.app
	Now       func() time.Time
}

func NewHandler(db *gorm.DB) *Handler {
	return &Handler{DB: db, Now: time.Now}
}

// RegisterRoutes mounts the authenticated API. The caller must attach auth
// middleware that sets "org_id" and "user_id" in the gin context.
func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/stats", h.Stats)
	r.GET("/settings", h.GetSettings)
	r.PUT("/settings", h.UpdateSettings)
	r.GET("/tax-rates", h.ListTaxRates)
	r.POST("/tax-rates", h.CreateTaxRate)
	r.PUT("/tax-rates/:id", h.UpdateTaxRate)
	r.DELETE("/tax-rates/:id", h.DeleteTaxRate)
	r.GET("/clients", h.Clients)
	r.GET("/customer-defaults/:customerId", h.CustomerDefaults)
	r.GET("/export", h.ExportCSV)
	r.POST("/recurring/run", h.RunRecurring)

	r.GET("/invoices", h.List)
	r.POST("/invoices", h.Create)
	r.GET("/invoices/:id", h.Get)
	r.PUT("/invoices/:id", h.Update)
	r.DELETE("/invoices/:id", h.Delete)
	r.POST("/invoices/:id/send", h.Send)
	r.POST("/invoices/:id/send-test", h.SendTest)
	r.POST("/invoices/:id/preflight", h.Preflight)
	r.GET("/invoices/:id/email-draft", h.EmailDraft)
	r.GET("/invoices/:id/email-log", h.EmailLogList)
	r.POST("/invoices/:id/rate", h.LockRate)
	r.GET("/invoices/:id/recipients", h.Recipients)
	r.POST("/invoices/:id/mark-sent", h.MarkSent)
	r.POST("/invoices/:id/void", h.Void)
	r.POST("/invoices/:id/duplicate", h.Duplicate)
	r.POST("/invoices/:id/convert", h.Convert)
	r.POST("/invoices/:id/accept", h.Accept)
	r.POST("/invoices/:id/decline", h.Decline)
	r.POST("/invoices/:id/payments", h.AddPayment)
	r.DELETE("/invoices/:id/payments/:paymentId", h.DeletePayment)
	r.POST("/invoices/:id/regenerate-link", h.RegenerateLink)
	r.GET("/invoices/:id/pdf", h.PDF)
	r.GET("/invoices/:id/pay-link", h.PayLink)
	r.GET("/invoices/:id/payments/:paymentId/receipt", h.ReceiptPDF)
}

// RegisterPublicRoutes mounts unauthenticated, token-based client views.
func (h *Handler) RegisterPublicRoutes(r *gin.RouterGroup) {
	r.GET("/invoices/:token", h.PublicView)
	r.POST("/invoices/:token/accept", h.PublicAccept)
	r.GET("/invoices/:token/pdf", h.PublicPDF)
	r.GET("/invoices/:token/receipts/:paymentId", h.PublicReceiptPDF)
}

// ---------- helpers ----------

type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func failErr(c *gin.Context, err error) {
	var ce *checkError
	if errors.As(err, &ce) {
		c.JSON(ce.status, gin.H{"success": false, "error": gin.H{"code": ce.code, "message": ce.msg, "checks": ce.checks}})
		return
	}
	var ae *apiError
	if errors.As(err, &ae) {
		fail(c, ae.status, ae.code, ae.msg)
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Invoice not found")
		return
	}
	fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
}

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func ctxString(c *gin.Context, key string) string {
	v, exists := c.Get(key)
	if !exists || v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	default:
		return fmt.Sprint(s)
	}
}

func (h *Handler) org(c *gin.Context) (string, bool) {
	org := ctxString(c, "org_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
		return "", false
	}
	return org, true
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// nowFor is the current time in the caller's time zone (?tz=), so "today"
// (overdue) and "this month" follow the user's calendar.
func (h *Handler) nowFor(c *gin.Context) time.Time { return h.now().In(reqtz.Location(c)) }

func newToken() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t, nil
		}
	}
	return time.Parse(time.RFC3339, s)
}

func (h *Handler) settingsFor(tx *gorm.DB, org string, lock bool) (*Settings, error) {
	var s Settings
	q := tx
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := q.Where("organization_id = ?", org).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		s = defaultSettings(org)
		s.Currency = orgprefs.Currency(tx, org)
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&s).Error; err != nil {
			return nil, err
		}
		if lock {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("organization_id = ?", org).First(&s).Error; err != nil {
				return nil, err
			}
		}
	} else if err != nil {
		return &s, err
	}
	// The default currency for new documents is always the organisation's
	// home currency (pkg/orgprefs); the stored copy is only a mirror.
	s.Currency = orgprefs.Currency(tx, org)
	return &s, nil
}

func defaultSettings(org string) Settings {
	return Settings{
		OrganizationID: org, TaxNumberLabel: "ABN", AccentColor: "#4f46e5", Currency: "AUD",
		InvoicePrefix: "INV-", NextInvoiceNumber: 1001, QuotePrefix: "QUO-", NextQuoteNumber: 1001,
		NumberPadding: 4, DefaultDueDays: 14, QuoteValidDays: 30, DefaultTaxRate: 10, TaxLabel: "GST",
		DefaultTerms: "Payment is due within 14 days of the invoice date.",
		EmailSubject: "{{type}} {{number}} from {{business}}",
		EmailMessage: "Hi {{client}},\n\nPlease find {{type_lower}} {{number}} for {{total}}, due {{due_date}}.\n\nView it online: {{link}}\n\nThank you for your business,\n{{business}}",
	}
}

// nextNumber reserves the next document number inside tx (row locked).
func (h *Handler) nextNumber(tx *gorm.DB, org, docType string) (string, error) {
	s, err := h.settingsFor(tx, org, true)
	if err != nil {
		return "", err
	}
	pad := s.NumberPadding
	if pad < 1 || pad > 10 {
		pad = 4
	}
	var number string
	for attempt := 0; attempt < 50; attempt++ {
		if docType == TypeQuote {
			number = fmt.Sprintf("%s%0*d", s.QuotePrefix, pad, s.NextQuoteNumber)
			s.NextQuoteNumber++
		} else {
			number = fmt.Sprintf("%s%0*d", s.InvoicePrefix, pad, s.NextInvoiceNumber)
			s.NextInvoiceNumber++
		}
		var n int64
		tx.Model(&Invoice{}).Where("organization_id = ? AND doc_type = ? AND number = ?", org, docType, number).Count(&n)
		if n == 0 {
			break
		}
	}
	if err := tx.Model(&Settings{}).Where("organization_id = ?", org).
		Updates(map[string]interface{}{"next_invoice_number": s.NextInvoiceNumber, "next_quote_number": s.NextQuoteNumber}).Error; err != nil {
		return "", err
	}
	return number, nil
}

func (h *Handler) load(tx *gorm.DB, org, id string, full bool) (*Invoice, error) {
	var inv Invoice
	q := tx.Where("id = ? AND organization_id = ?", id, org).
		Preload("Items", func(db *gorm.DB) *gorm.DB { return db.Order("position ASC") }).
		Preload("Payments", func(db *gorm.DB) *gorm.DB { return db.Order("date ASC, created_at ASC") })
	if full {
		q = q.Preload("Activities", func(db *gorm.DB) *gorm.DB { return db.Order("created_at DESC") })
	}
	if err := q.First(&inv).Error; err != nil {
		return nil, err
	}
	Derive(&inv, h.now())
	return &inv, nil
}

func (h *Handler) log(tx *gorm.DB, inv *Invoice, typ, msg, user string) {
	tx.Create(&Activity{ID: uuid.NewString(), InvoiceID: inv.ID, OrganizationID: inv.OrganizationID, Type: typ, Message: msg, UserID: user, CreatedAt: h.now()})
}

func docLabel(t string) string {
	if t == TypeQuote {
		return "Quote"
	}
	return "Invoice"
}

// ---------- input ----------

type itemInput struct {
	ItemType        string  `json:"item_type"`
	ReferenceID     string  `json:"reference_id"`
	Description     string  `json:"description"`
	Quantity        float64 `json:"quantity"`
	Unit            string  `json:"unit"`
	UnitPrice       float64 `json:"unit_price"`
	DiscountPercent float64 `json:"discount_percent"`
	TaxRate         float64 `json:"tax_rate"`
	TaxName         string  `json:"tax_name"`
	// OriginalUnitPrice / OriginalCurrency: the catalog price a converted
	// line came from. The server re-prices such lines at the locked rate.
	OriginalUnitPrice *float64 `json:"original_unit_price"`
	OriginalCurrency  string   `json:"original_currency"`
}

type invoiceInput struct {
	rateInput
	DocType            string      `json:"doc_type"`
	Status             string      `json:"status"`
	CustomerID         *string     `json:"customer_id"`
	ClientName         string      `json:"client_name"`
	ClientEmail        string      `json:"client_email"`
	ClientPhone        string      `json:"client_phone"`
	ClientAddress      string      `json:"client_address"`
	ClientTaxNumber    string      `json:"client_tax_number"`
	Reference          string      `json:"reference"`
	Title              string      `json:"title"`
	IssueDate          string      `json:"issue_date"`
	DueDate            string      `json:"due_date"`
	Currency           string      `json:"currency"`
	PricesIncludeTax   *bool       `json:"prices_include_tax"`
	DiscountType       string      `json:"discount_type"`
	DiscountValue      float64     `json:"discount_value"`
	ShippingAmount     float64     `json:"shipping_amount"`
	Notes              string      `json:"notes"`
	Terms              string      `json:"terms"`
	IsRecurring        bool        `json:"is_recurring"`
	RecurrenceInterval string      `json:"recurrence_interval"`
	NextRecurrenceDate string      `json:"next_recurrence_date"`
	RecurrenceEndDate  string      `json:"recurrence_end_date"`
	CCEmails           *[]string   `json:"cc_emails"`
	Items              []itemInput `json:"items"`
	// Deposit is money already received for this invoice (optional). It is
	// recorded as a real payment when the invoice is saved.
	Deposit *paymentInput `json:"deposit"`
}

func (in *invoiceInput) apply(inv *Invoice, s *Settings, now time.Time) error {
	if strings.TrimSpace(in.ClientName) == "" {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Client name is required"}
	}
	if len(in.Items) == 0 {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Add at least one line item"}
	}
	if len(in.Items) > 500 {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Too many line items"}
	}
	issue, err := parseDate(in.IssueDate)
	if err != nil {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Invalid issue date"}
	}
	if issue.IsZero() {
		issue = dateOnly(now)
	}
	due, err := parseDate(in.DueDate)
	if err != nil {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Invalid due date"}
	}
	if due.IsZero() {
		days := s.DefaultDueDays
		if inv.DocType == TypeQuote {
			days = s.QuoteValidDays
		}
		due = issue.AddDate(0, 0, days)
	}
	if due.Before(issue) {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Due date cannot be before the issue date"}
	}
	if in.CustomerID != nil && strings.TrimSpace(*in.CustomerID) == "" {
		in.CustomerID = nil
	}
	inv.CustomerID = in.CustomerID
	inv.ClientName = strings.TrimSpace(in.ClientName)
	inv.ClientEmail = strings.TrimSpace(in.ClientEmail)
	inv.ClientPhone = in.ClientPhone
	inv.ClientAddress = in.ClientAddress
	inv.ClientTaxNumber = in.ClientTaxNumber
	inv.Reference = in.Reference
	inv.Title = in.Title
	inv.IssueDate = issue
	inv.DueDate = due
	inv.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if inv.Currency == "" {
		inv.Currency = strings.ToUpper(s.Currency)
	}
	if !currency.IsSupported(inv.Currency) {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Currency " + inv.Currency + " is not supported"}
	}
	if in.CCEmails != nil {
		_, cc, skipped := MergeRecipients(inv.ClientEmail, *in.CCEmails)
		if len(skipped) > 0 {
			return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Invalid CC email: " + strings.Join(skipped, ", ")}
		}
		if len(cc) > 50 {
			return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Too many CC recipients (max 50)"}
		}
		inv.CCEmails = cc
	}
	if in.PricesIncludeTax != nil {
		inv.PricesIncludeTax = *in.PricesIncludeTax
	}
	inv.DiscountType = in.DiscountType
	if inv.DiscountType != "amount" {
		inv.DiscountType = "percent"
	}
	inv.DiscountValue = in.DiscountValue
	if inv.DiscountValue < 0 {
		inv.DiscountValue = 0
	}
	inv.ShippingAmount = in.ShippingAmount
	inv.Notes = in.Notes
	inv.Terms = in.Terms
	if inv.DocType == TypeInvoice {
		inv.IsRecurring = in.IsRecurring
		inv.RecurrenceInterval = in.RecurrenceInterval
		inv.NextRecurrenceDate = nil
		inv.RecurrenceEndDate = nil
		if in.IsRecurring {
			switch in.RecurrenceInterval {
			case "weekly", "fortnightly", "monthly", "quarterly", "yearly":
			default:
				return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Choose how often this invoice repeats"}
			}
			next, _ := parseDate(in.NextRecurrenceDate)
			if next.IsZero() {
				next = NextDate(issue, in.RecurrenceInterval)
			}
			inv.NextRecurrenceDate = &next
			if end, _ := parseDate(in.RecurrenceEndDate); !end.IsZero() {
				inv.RecurrenceEndDate = &end
			}
		}
	}
	items := make([]InvoiceItem, 0, len(in.Items))
	for _, it := range in.Items {
		if strings.TrimSpace(it.Description) == "" && it.UnitPrice == 0 {
			continue // ignore blank rows
		}
		if it.TaxRate < 0 || it.TaxRate > 100 {
			return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Tax rate must be between 0 and 100"}
		}
		var orig *float64
		oc := strings.ToUpper(strings.TrimSpace(it.OriginalCurrency))
		if it.OriginalUnitPrice != nil && *it.OriginalUnitPrice >= 0 && currency.IsSupported(oc) {
			v := *it.OriginalUnitPrice
			orig = &v
		} else {
			oc = ""
		}
		items = append(items, InvoiceItem{
			ID: uuid.NewString(), InvoiceID: inv.ID, ItemType: it.ItemType, ReferenceID: it.ReferenceID,
			Description: it.Description, Quantity: it.Quantity, Unit: it.Unit, UnitPrice: it.UnitPrice,
			OriginalUnitPrice: orig, OriginalCurrency: oc,
			DiscountPercent: it.DiscountPercent, TaxRate: it.TaxRate, TaxName: it.TaxName,
		})
	}
	if len(items) == 0 {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Add at least one line item"}
	}
	inv.Items = items
	Recalculate(inv)
	ApplyPayments(inv, now)
	return nil
}

// ---------- invoices ----------

func (h *Handler) List(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	h.runRecurring(org, ctxString(c, "user_id"))
	today := dateOnly(h.nowFor(c)).Format("2006-01-02")
	q := h.DB.Model(&Invoice{}).Where("organization_id = ?", org)
	docType := c.DefaultQuery("type", TypeInvoice)
	if docType != "all" {
		q = q.Where("doc_type = ?", docType)
	}
	switch st := c.Query("status"); st {
	case "", "all":
	case StatusOverdue:
		q = q.Where("status IN ? AND due_date < ?", []string{StatusSent, StatusPartial}, today)
	case "unpaid":
		q = q.Where("status IN ?", []string{StatusSent, StatusPartial})
	case "open": // quotes still awaiting a decision or accepted but not invoiced (matches stats.open_quotes)
		q = q.Where("((status = ? AND (due_date IS NULL OR due_date >= ?)) OR status = ?)", StatusSent, today, StatusAccepted)
	case StatusExpired:
		q = q.Where("status IN ? AND due_date < ?", []string{StatusSent, StatusDraft}, today)
	case "due": // unpaid and not yet past the due date ("Not yet due" in receivables aging)
		q = q.Where("status IN ? AND (due_date IS NULL OR due_date >= ?)", []string{StatusSent, StatusPartial}, today)
	case StatusSent, StatusDraft, StatusPartial:
		// Filters follow the displayed status (see Derive): a sent/part-paid
		// invoice past its due date is "overdue", a sent/draft quote past its
		// expiry is "expired" — so they are not listed under sent/partial/draft.
		derived := (docType == TypeQuote && st != StatusPartial) || (docType != TypeQuote && st != StatusDraft)
		if derived {
			q = q.Where("status = ? AND (due_date IS NULL OR due_date >= ?)", st, today)
		} else {
			q = q.Where("status = ?", st)
		}
	case "recurring":
		q = q.Where("is_recurring = ?", true)
	default:
		q = q.Where("status = ?", st)
	}
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		like := "%" + strings.ToLower(s) + "%"
		q = q.Where("LOWER(number) LIKE ? OR LOWER(client_name) LIKE ? OR LOWER(client_email) LIKE ? OR LOWER(reference) LIKE ? OR LOWER(title) LIKE ?", like, like, like, like, like)
	}
	if cid := c.Query("customer_id"); cid != "" {
		q = q.Where("customer_id = ?", cid)
	}
	if from, err := parseDate(c.Query("from")); err == nil && !from.IsZero() {
		q = q.Where("issue_date >= ?", from)
	}
	if to, err := parseDate(c.Query("to")); err == nil && !to.IsZero() {
		q = q.Where("issue_date <= ?", to)
	}
	// paid_from / paid_to: invoices with a payment dated in the range (the
	// cash-basis drill-down from the Profit & loss report).
	pf, errF := parseDate(c.Query("paid_from"))
	pt, errT := parseDate(c.Query("paid_to"))
	if (errF == nil && !pf.IsZero()) || (errT == nil && !pt.IsZero()) {
		sub := h.DB.Model(&Payment{}).Select("invoice_id").Where("organization_id = ?", org)
		if errF == nil && !pf.IsZero() {
			sub = sub.Where("date >= ?", pf)
		}
		if errT == nil && !pt.IsZero() {
			sub = sub.Where("date <= ?", pt)
		}
		q = q.Where("id IN (?)", sub)
	}
	var total int64
	q.Count(&total)

	order := "issue_date DESC, created_at DESC"
	switch c.Query("sort") {
	case "due_date":
		order = "due_date ASC"
	case "total":
		order = "total DESC"
	case "client":
		order = "client_name ASC"
	case "number":
		order = "number DESC"
	case "oldest":
		order = "issue_date ASC, created_at ASC"
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	per, _ := strconv.Atoi(c.DefaultQuery("per_page", "25"))
	if page < 1 {
		page = 1
	}
	if per < 1 || per > 200 {
		per = 25
	}
	var list []Invoice
	if err := q.Order(order).Offset((page - 1) * per).Limit(per).Find(&list).Error; err != nil {
		failErr(c, err)
		return
	}
	for i := range list {
		Derive(&list[i], h.nowFor(c))
	}
	ok(c, http.StatusOK, gin.H{"invoices": list, "total": total, "page": page, "per_page": per})
}

func (h *Handler) Get(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	inv, err := h.load(h.DB, org, c.Param("id"), true)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, inv)
}

func (h *Handler) Create(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in invoiceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	user := ctxString(c, "user_id")
	var created *Invoice
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		s, err := h.settingsFor(tx, org, false)
		if err != nil {
			return err
		}
		inv := &Invoice{ID: uuid.NewString(), OrganizationID: org, DocType: TypeInvoice, Status: StatusDraft, PublicToken: newToken(), CreatedBy: user, PricesIncludeTax: s.PricesIncludeTax}
		if in.DocType == TypeQuote {
			inv.DocType = TypeQuote
			if in.Deposit != nil && in.Deposit.Amount != 0 {
				return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Deposits can only be recorded on invoices — convert the quote first"}
			}
		}
		// No currency chosen: invoice the linked customer in their currency.
		if strings.TrimSpace(in.Currency) == "" && in.CustomerID != nil {
			in.Currency = h.customerCurrency(tx, org, *in.CustomerID, s.Currency)
		}
		if err := in.apply(inv, s, h.now()); err != nil {
			return err
		}
		if err := h.finishAmounts(tx, inv, in.rateInput, "", ""); err != nil {
			return err
		}
		num, err := h.nextNumber(tx, org, inv.DocType)
		if err != nil {
			return err
		}
		inv.Number = num
		items := inv.Items
		inv.Items = nil
		if err := tx.Create(inv).Error; err != nil {
			return err
		}
		for i := range items {
			items[i].InvoiceID = inv.ID
		}
		if err := tx.Create(&items).Error; err != nil {
			return err
		}
		h.log(tx, inv, "created", docLabel(inv.DocType)+" "+inv.Number+" created", user)
		if in.Status == StatusSent {
			t := h.now()
			inv.Status = StatusSent
			inv.SentAt = &t
			if err := tx.Model(inv).Updates(map[string]interface{}{"status": StatusSent, "sent_at": t}).Error; err != nil {
				return err
			}
			h.log(tx, inv, "sent", "Marked as sent", user)
		}
		if in.Deposit != nil && inv.DocType == TypeInvoice {
			if err := h.recordPayment(tx, inv, *in.Deposit, user, true); err != nil {
				return err
			}
		}
		created, err = h.load(tx, org, inv.ID, true)
		return err
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusCreated, created)
}

func (h *Handler) Update(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in invoiceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	user := ctxString(c, "user_id")
	var out *Invoice
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		inv, err := h.load(tx, org, c.Param("id"), false)
		if err != nil {
			return err
		}
		if inv.Status == StatusVoid || inv.Status == StatusConverted {
			return &apiError{http.StatusConflict, "LOCKED", "This document can no longer be edited"}
		}
		s, err := h.settingsFor(tx, org, false)
		if err != nil {
			return err
		}
		prevCur, prevBase := inv.Currency, inv.BaseCurrency
		if err := in.apply(inv, s, h.now()); err != nil {
			return err
		}
		if err := h.finishAmounts(tx, inv, in.rateInput, prevCur, prevBase); err != nil {
			return err
		}
		if inv.DocType == TypeInvoice && inv.Total+0.004 < inv.AmountPaid {
			return &apiError{http.StatusConflict, "TOTAL_BELOW_PAID", "The new total is less than the amount already paid"}
		}
		if err := tx.Where("invoice_id = ?", inv.ID).Delete(&InvoiceItem{}).Error; err != nil {
			return err
		}
		items := inv.Items
		for i := range items {
			items[i].InvoiceID = inv.ID
		}
		if err := tx.Create(&items).Error; err != nil {
			return err
		}
		inv.Items = nil
		inv.Payments = nil
		if err := tx.Omit(clause.Associations).Save(inv).Error; err != nil {
			return err
		}
		h.log(tx, inv, "updated", "Details updated", user)
		if in.Deposit != nil && in.Deposit.Amount != 0 {
			fresh, err := h.load(tx, org, inv.ID, false)
			if err != nil {
				return err
			}
			if err := h.recordPayment(tx, fresh, *in.Deposit, user, true); err != nil {
				return err
			}
		}
		out, err = h.load(tx, org, inv.ID, true)
		return err
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, out)
}

func (h *Handler) Delete(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		inv, err := h.load(tx, org, c.Param("id"), false)
		if err != nil {
			return err
		}
		if inv.DocType == TypeInvoice && inv.Status != StatusDraft && inv.Status != StatusVoid {
			return &apiError{http.StatusConflict, "NOT_DELETABLE", "Only draft or void invoices can be deleted. Void it first."}
		}
		tx.Where("invoice_id = ?", inv.ID).Delete(&InvoiceItem{})
		tx.Where("invoice_id = ?", inv.ID).Delete(&Payment{})
		tx.Where("invoice_id = ?", inv.ID).Delete(&Activity{})
		return tx.Delete(inv).Error
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) markSent(tx *gorm.DB, inv *Invoice, user, msg string) error {
	if inv.Status == StatusVoid || inv.Status == StatusConverted {
		return &apiError{http.StatusConflict, "LOCKED", "This document can't be sent"}
	}
	t := h.now()
	updates := map[string]interface{}{"sent_at": t}
	if inv.Status == StatusDraft {
		updates["status"] = StatusSent
		inv.Status = StatusSent
	}
	inv.SentAt = &t
	if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Updates(updates).Error; err != nil {
		return err
	}
	h.log(tx, inv, "sent", msg, user)
	return nil
}

func (h *Handler) MarkSent(c *gin.Context) {
	h.simpleAction(c, func(tx *gorm.DB, inv *Invoice, user string) error {
		// Official status change: the amounts and exchange rate must be right.
		if checks := h.runChecks(tx, inv, nil, checkOptions{Mode: "mark"}); countSeverity(checks, "error") > 0 {
			return blocked(checks, false)
		}
		return h.markSent(tx, inv, user, "Marked as sent")
	})
}

type sendInput struct {
	To      string    `json:"to"`
	CC      *[]string `json:"cc"`  // nil: use the suggested CCs (incl. "Always CC"); [] sends to To only
	BCC     *[]string `json:"bcc"` // nil: the email settings' "Always BCC"
	Subject string    `json:"subject"`
	Message string    `json:"message"`
	Kind    string    `json:"kind"` // invoice | quote | reminder | receipt
	// PaymentID is the payment a receipt email is about (kind "receipt").
	PaymentID string `json:"payment_id"`
	// Mode "email" requires server email (SMTP) and fails clearly without it;
	// "mailto" only records the send (the user's email app sends it). Empty
	// keeps the old behaviour: email when configured, else mailto.
	Mode string `json:"mode"`
	// AppURL is the web app's origin (window.location.origin), used for the
	// View / Pay links when PUBLIC_APP_URL is not set.
	AppURL string `json:"app_url"`
	// AcceptWarnings: the pre-send checks always run and errors always block.
	// false makes warnings block too (the send dialog passes true only after
	// the user chose "Send anyway"); nil (API callers) lets warnings through.
	AcceptWarnings *bool `json:"accept_warnings"`
}

// Send emails the document when SMTP is configured; otherwise it marks it sent
// and returns a ready-made mailto payload for the client's own mail app.
// Customer contacts flagged for this kind of email are copied automatically
// unless the caller passes an explicit cc list. The pre-send checks run first
// (see runChecks) and every attempt is written to invoice_email_log.
func (h *Handler) Send(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in sendInput
	_ = c.ShouldBindJSON(&in)
	user := ctxString(c, "user_id")
	mcfg, viaMail := h.mailConfig(org)
	configured := h.emailEnabled(org)
	if in.Mode == "email" && !configured {
		msg := "Email sending isn't set up yet. Use “Open in email app” instead, or ask your administrator to set up outgoing email under Settings → Outgoing email."
		if viaMail && mcfg.Problem != "" {
			msg = "Email sending isn't ready: " + mcfg.Problem
		}
		fail(c, http.StatusServiceUnavailable, "SMTP_NOT_CONFIGURED", msg)
		return
	}
	useEmail := configured && in.Mode != "mailto"
	mode := "mailto"
	if useEmail {
		mode = "email"
	}
	var out *Invoice
	var built *builtEmail
	var entry *EmailLog
	var checks []Check
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		inv, err := h.load(tx, org, c.Param("id"), false)
		if err != nil {
			return err
		}
		kind := kindOf(inv, in.Kind)
		if kind == KindReminder && inv.Status != StatusSent && inv.Status != StatusPartial {
			return &apiError{http.StatusConflict, "NOT_REMINDABLE", "Reminders can only be sent for unpaid invoices"}
		}
		if kind != KindReminder && kind != KindReceipt && (inv.Status == StatusVoid || inv.Status == StatusConverted) {
			return &apiError{http.StatusConflict, "LOCKED", "This document can't be sent"}
		}
		b, err := h.buildEmail(tx, c, inv, in, mcfg, viaMail)
		if err != nil {
			return err
		}
		built = b
		checks = h.runChecks(tx, inv, b, checkOptions{Mode: mode, Configured: configured, MailCfg: mcfg, ViaMail: viaMail})
		entry = h.newLog(inv, b, mode, user, "", checks)
		if countSeverity(checks, "error") > 0 {
			entry.Status = "blocked"
			return blocked(checks, false)
		}
		if countSeverity(checks, "warning") > 0 && in.AcceptWarnings != nil && !*in.AcceptWarnings {
			entry.Status = "blocked"
			return blocked(checks, true)
		}
		rec := Recipients{Kind: kind, To: b.To, CC: b.CC, BCC: b.BCC}
		if kind == KindReceipt {
			rec.Kind = KindInvoice
		}
		if useEmail {
			start := time.Now()
			var sendErr error
			switch {
			case viaMail:
				msg := mail.Message{To: []string{b.To}, CC: b.CC, BCC: b.BCC, Subject: b.Subject, Text: b.Text, HTML: b.HTML,
					Attachments: []mail.Attachment{b.Attachment}, MessageID: mail.NewMessageID(mcfg.FromEmail), ReplyTo: mcfg.ReplyTo}
				entry.MessageID = msg.MessageID
				sendErr = h.Mail.SendWith(c.Request.Context(), org, mcfg, msg)
			default:
				if m, isHTML := h.Mailer.(HTMLMailer); isHTML {
					sendErr = m.SendHTML(b.To, b.CC, b.Subject, b.Text, b.HTML)
				} else if m, isCC := h.Mailer.(CCMailer); isCC {
					sendErr = m.SendWithCC(b.To, b.CC, b.Subject, b.Text)
				} else {
					if len(b.CC) > 0 {
						return &apiError{http.StatusBadRequest, "CC_UNSUPPORTED", "The configured mailer can't copy other recipients"}
					}
					sendErr = h.Mailer.Send(b.To, b.Subject, b.Text)
				}
			}
			entry.DurationMS = int(time.Since(start).Milliseconds())
			if sendErr != nil {
				entry.Status, entry.Error, entry.ErrorCode = "failed", sendErr.Error(), mail.Code(sendErr)
				return &apiError{http.StatusBadGateway, "EMAIL_FAILED", "Email could not be sent: " + sendErr.Error()}
			}
			entry.Status = "sent"
			entry.Response = "250 accepted"
			if viaMail {
				entry.Response = "250 accepted by " + mcfg.Host
			}
			rec.Emailed = true
		} else {
			entry.Status = "recorded"
			entry.Response = "Handed to the user's email app (mailto)"
		}
		// Remember the CCs so reminders and recurring copies reuse them — but
		// not the settings' "Always CC" addresses, which are added at send time.
		keepCC := b.CC
		if viaMail {
			keepCC = withoutAddresses(b.CC, withoutAddresses(mcfg.CC, inv.CCEmails))
		}
		if kind != KindReceipt && !equalStrings(keepCC, inv.CCEmails) {
			if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Update("cc_emails", ccJSON(keepCC)).Error; err != nil {
				return err
			}
		}
		msg := recipientsMessage(rec)
		switch kind {
		case KindReceipt:
			what := "Receipt for " + FormatMoney(b.Payment.Amount, inv.Currency)
			if rec.Emailed {
				msg = what + " emailed to " + rec.To
			} else {
				msg = what + " sent via email app to " + rec.To
			}
			if len(rec.CC) > 0 {
				msg += " · CC " + strings.Join(rec.CC, ", ")
			}
			h.logRecipients(tx, inv, "receipt_sent", msg, user, &rec)
		case KindReminder:
			t := h.now()
			if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Update("sent_at", t).Error; err != nil {
				return err
			}
			h.logRecipients(tx, inv, "reminder", msg, user, &rec)
		default:
			t := h.now()
			updates := map[string]interface{}{"sent_at": t}
			if inv.Status == StatusDraft {
				updates["status"] = StatusSent
			}
			if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Updates(updates).Error; err != nil {
				return err
			}
			h.logRecipients(tx, inv, "sent", msg, user, &rec)
		}
		out, err = h.load(tx, org, inv.ID, true)
		return err
	})
	if entry != nil && entry.Status != "" {
		entry.UserEmail = h.userEmail(user)
		if err := h.DB.Create(entry).Error; err != nil {
			fmt.Printf("invoicing: email log: %v\n", err)
		}
	}
	if err != nil {
		failErr(c, err)
		return
	}
	skipped := built.Skipped
	if skipped == nil {
		skipped = []string{}
	}
	bccOut := built.BCC
	if bccOut == nil {
		bccOut = []string{}
	}
	ccOut := built.CC
	if ccOut == nil {
		ccOut = []string{}
	}
	resp := gin.H{"invoice": out, "emailed": entry.Status == "sent", "to": built.To, "cc": ccOut, "bcc": bccOut, "kind": built.Kind, "skipped": skipped,
		"subject": built.Subject, "message": built.Message, "checks": checks, "log_id": entry.ID, "message_id": entry.MessageID}
	if s := h.sender(org); s != nil {
		resp["sender"] = s
	}
	if built.PDFErr == nil && built.Attachment.Filename != "" {
		resp["attachment"] = built.Attachment.Filename
		resp["view_url"] = built.ViewURL
		resp["pay_url"] = built.PayURL
	}
	ok(c, http.StatusOK, resp)
}

func (h *Handler) logRecipients(tx *gorm.DB, inv *Invoice, typ, msg, user string, rec *Recipients) {
	tx.Create(&Activity{ID: uuid.NewString(), InvoiceID: inv.ID, OrganizationID: inv.OrganizationID, Type: typ, Message: msg, UserID: user, Recipients: rec, CreatedAt: h.now()})
}

func recipientsMessage(r Recipients) string {
	what := "Sent"
	if r.Kind == KindReminder {
		what = "Reminder sent"
	}
	var b strings.Builder
	switch {
	case r.Emailed:
		b.WriteString(what + " by email to " + r.To)
	case r.To != "":
		b.WriteString(what + " via email app to " + r.To)
	default:
		b.WriteString("Marked as sent")
	}
	if len(r.CC) > 0 {
		b.WriteString(" · CC " + strings.Join(r.CC, ", "))
	}
	return b.String()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ccJSON encodes a CC list for a raw column update (serializer:json column).
func ccJSON(cc []string) string {
	if len(cc) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(cc)
	return string(b)
}

func (h *Handler) simpleAction(c *gin.Context, fn func(tx *gorm.DB, inv *Invoice, user string) error) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	user := ctxString(c, "user_id")
	var out *Invoice
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		inv, err := h.load(tx, org, c.Param("id"), false)
		if err != nil {
			return err
		}
		if err := fn(tx, inv, user); err != nil {
			return err
		}
		out, err = h.load(tx, org, inv.ID, true)
		return err
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, out)
}

func (h *Handler) Void(c *gin.Context) {
	h.simpleAction(c, func(tx *gorm.DB, inv *Invoice, user string) error {
		if inv.Status == StatusVoid {
			return &apiError{http.StatusConflict, "ALREADY_VOID", "Already void"}
		}
		if inv.AmountPaid > 0 {
			return &apiError{http.StatusConflict, "HAS_PAYMENTS", "Remove recorded payments before voiding"}
		}
		t := h.now()
		if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Updates(map[string]interface{}{"status": StatusVoid, "voided_at": t, "is_recurring": false}).Error; err != nil {
			return err
		}
		h.log(tx, inv, "voided", docLabel(inv.DocType)+" voided", user)
		return nil
	})
}

func (h *Handler) Accept(c *gin.Context) {
	h.simpleAction(c, func(tx *gorm.DB, inv *Invoice, user string) error {
		if inv.DocType != TypeQuote {
			return &apiError{http.StatusBadRequest, "NOT_QUOTE", "Only quotes can be accepted"}
		}
		if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Update("status", StatusAccepted).Error; err != nil {
			return err
		}
		h.log(tx, inv, "accepted", "Quote accepted", user)
		return nil
	})
}

func (h *Handler) Decline(c *gin.Context) {
	h.simpleAction(c, func(tx *gorm.DB, inv *Invoice, user string) error {
		if inv.DocType != TypeQuote {
			return &apiError{http.StatusBadRequest, "NOT_QUOTE", "Only quotes can be declined"}
		}
		if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Update("status", StatusDeclined).Error; err != nil {
			return err
		}
		h.log(tx, inv, "declined", "Quote declined", user)
		return nil
	})
}

func (h *Handler) RegenerateLink(c *gin.Context) {
	h.simpleAction(c, func(tx *gorm.DB, inv *Invoice, user string) error {
		if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Update("public_token", newToken()).Error; err != nil {
			return err
		}
		h.log(tx, inv, "link", "Share link regenerated (old link disabled)", user)
		return nil
	})
}

// copyInvoice clones src into a new draft of docType inside tx.
// keepRate copies src's locked exchange rate (quote → invoice, recurring
// copies: the amounts stay identical); otherwise today's rate is locked and
// catalog lines are re-priced at it.
func (h *Handler) copyInvoice(tx *gorm.DB, src *Invoice, docType, user string, issue time.Time, keepRate bool) (*Invoice, error) {
	s, err := h.settingsFor(tx, src.OrganizationID, false)
	if err != nil {
		return nil, err
	}
	days := s.DefaultDueDays
	if docType == TypeQuote {
		days = s.QuoteValidDays
	}
	if !src.DueDate.IsZero() && !src.IssueDate.IsZero() && src.DocType == docType {
		days = int(src.DueDate.Sub(src.IssueDate).Hours() / 24)
	}
	n := &Invoice{
		ID: uuid.NewString(), OrganizationID: src.OrganizationID, DocType: docType, Status: StatusDraft,
		CustomerID: src.CustomerID, ClientName: src.ClientName, ClientEmail: src.ClientEmail, ClientPhone: src.ClientPhone,
		ClientAddress: src.ClientAddress, ClientTaxNumber: src.ClientTaxNumber, Reference: src.Reference, Title: src.Title,
		IssueDate: dateOnly(issue), DueDate: dateOnly(issue).AddDate(0, 0, days), Currency: src.Currency,
		PricesIncludeTax: src.PricesIncludeTax, DiscountType: src.DiscountType, DiscountValue: src.DiscountValue,
		ShippingAmount: src.ShippingAmount, Notes: src.Notes, Terms: src.Terms, PublicToken: newToken(), CreatedBy: user,
		CCEmails: append([]string(nil), src.CCEmails...),
	}
	for _, it := range src.Items {
		it.ID = uuid.NewString()
		it.InvoiceID = n.ID
		n.Items = append(n.Items, it)
	}
	n.BaseCurrency = src.BaseCurrency
	if keepRate && (src.ExchangeRate > 0 || src.Currency == src.BaseCurrency) && src.BaseCurrency != "" {
		n.ExchangeRate, n.RateSource, n.RateDate, n.RateProvider = src.ExchangeRate, src.RateSource, src.RateDate, src.RateProvider
		Recalculate(n)
		setBaseTotal(n)
	} else {
		n.BaseCurrency = ""
		if err := h.finishAmounts(tx, n, rateInput{}, "", ""); err != nil {
			return nil, err
		}
	}
	num, err := h.nextNumber(tx, n.OrganizationID, docType)
	if err != nil {
		return nil, err
	}
	n.Number = num
	items := n.Items
	n.Items = nil
	if err := tx.Create(n).Error; err != nil {
		return nil, err
	}
	if len(items) > 0 {
		if err := tx.Create(&items).Error; err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (h *Handler) Duplicate(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	user := ctxString(c, "user_id")
	var out *Invoice
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		src, err := h.load(tx, org, c.Param("id"), false)
		if err != nil {
			return err
		}
		n, err := h.copyInvoice(tx, src, src.DocType, user, h.now(), false)
		if err != nil {
			return err
		}
		h.log(tx, n, "created", "Duplicated from "+src.Number, user)
		out, err = h.load(tx, org, n.ID, true)
		return err
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusCreated, out)
}

// Convert turns a quote into a new draft invoice.
func (h *Handler) Convert(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	user := ctxString(c, "user_id")
	// Optional body: {"deposit": {amount, date, method, reference}}
	var body struct {
		Deposit *paymentInput `json:"deposit"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
			return
		}
	}
	var out *Invoice
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		src, err := h.load(tx, org, c.Param("id"), false)
		if err != nil {
			return err
		}
		if src.DocType != TypeQuote {
			return &apiError{http.StatusBadRequest, "NOT_QUOTE", "Only quotes can be converted"}
		}
		if src.Status == StatusConverted || src.Status == StatusVoid || src.Status == StatusDeclined {
			return &apiError{http.StatusConflict, "LOCKED", "This quote can't be converted"}
		}
		n, err := h.copyInvoice(tx, src, TypeInvoice, user, h.now(), true)
		if err != nil {
			return err
		}
		n.ConvertedFromID = &src.ID
		tx.Model(&Invoice{}).Where("id = ?", n.ID).Update("converted_from_id", src.ID)
		tx.Model(&Invoice{}).Where("id = ?", src.ID).Updates(map[string]interface{}{"status": StatusConverted, "converted_to_id": n.ID})
		h.log(tx, src, "converted", "Converted to invoice "+n.Number, user)
		h.log(tx, n, "created", "Created from quote "+src.Number, user)
		if body.Deposit != nil && body.Deposit.Amount != 0 {
			fresh, err := h.load(tx, org, n.ID, false)
			if err != nil {
				return err
			}
			if err := h.recordPayment(tx, fresh, *body.Deposit, user, true); err != nil {
				return err
			}
		}
		out, err = h.load(tx, org, n.ID, true)
		return err
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusCreated, out)
}

type paymentInput struct {
	Amount    float64 `json:"amount"`
	Date      string  `json:"date"`
	Method    string  `json:"method"`
	Reference string  `json:"reference"`
	Notes     string  `json:"notes"`
	// keepMethod stores Method as given (online payments, e.g.
	// "Flutterwave · M-Pesa").
	keepMethod bool
}

func (h *Handler) AddPayment(c *gin.Context) {
	var in paymentInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	h.simpleAction(c, func(tx *gorm.DB, inv *Invoice, user string) error {
		return h.recordPayment(tx, inv, in, user, false)
	})
}

// PaymentMethods are the accepted payment methods.
var PaymentMethods = []string{"bank_transfer", "card", "cash", "mobile_money", "cheque", "paypal", "stripe", "other"}

func validMethod(m string) bool {
	for _, x := range PaymentMethods {
		if x == m {
			return true
		}
	}
	return false
}

// recordPayment validates and records a payment (or a deposit taken when the
// invoice was written) on inv, updates paid / balance / status and logs it.
// inv must be loaded with its payments.
func (h *Handler) recordPayment(tx *gorm.DB, inv *Invoice, in paymentInput, user string, deposit bool) error {
	what := "Payment"
	if deposit {
		what = "Deposit"
	}
	if inv.DocType != TypeInvoice {
		return &apiError{http.StatusBadRequest, "NOT_INVOICE", "Payments can only be recorded on invoices"}
	}
	if inv.Status == StatusVoid {
		return &apiError{http.StatusConflict, "LOCKED", "Void invoices can't take payments"}
	}
	amt := roundCur(in.Amount, inv.Currency)
	if amt <= 0 {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", what + " amount must be greater than zero"}
	}
	if deposit && math.Abs(amt-in.Amount) > 1e-9 {
		d := currency.Decimals(inv.Currency)
		msg := fmt.Sprintf("%s amounts can have at most %d decimal places", inv.Currency, d)
		if d == 0 {
			msg = inv.Currency + " amounts can't have decimals"
		}
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", msg}
	}
	if amt > inv.BalanceDue+0.4/math.Pow10(currency.Decimals(inv.Currency)) {
		if deposit {
			return &apiError{http.StatusBadRequest, "OVERPAYMENT", fmt.Sprintf("The amount already paid can't be more than the invoice total (%s %s)", inv.Currency, fmtAmount(inv.BalanceDue, inv.Currency))}
		}
		return &apiError{http.StatusBadRequest, "OVERPAYMENT", fmt.Sprintf("Payment exceeds the balance due (%s %s)", inv.Currency, fmtAmount(inv.BalanceDue, inv.Currency))}
	}
	d, err := parseDate(in.Date)
	if err != nil {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Invalid " + strings.ToLower(what) + " date"}
	}
	if d.IsZero() {
		d = dateOnly(h.now())
	}
	if deposit && d.After(dateOnly(h.now()).AddDate(0, 0, 1)) {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", what + " date can't be in the future"}
	}
	method := strings.ToLower(strings.TrimSpace(in.Method))
	method = strings.NewReplacer(" ", "_", "-", "_").Replace(method)
	if in.keepMethod {
		method = strings.TrimSpace(in.Method)
	}
	switch method {
	case "":
		method = "bank_transfer"
		if deposit {
			method = "cash"
		}
	case "mpesa", "m_pesa", "mobile", "mobilemoney":
		method = "mobile_money"
	case "bank", "transfer", "eft":
		method = "bank_transfer"
	case "credit_card", "debit_card":
		method = "card"
	}
	if deposit && !validMethod(method) {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Choose a payment method: " + strings.ReplaceAll(strings.Join(PaymentMethods, ", "), "_", " ")}
	}
	if len(in.Reference) > 255 {
		return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Reference is too long (255 characters max)"}
	}
	p := Payment{ID: uuid.NewString(), InvoiceID: inv.ID, OrganizationID: inv.OrganizationID, Amount: amt, Date: d, Method: method,
		Reference: strings.TrimSpace(in.Reference), Notes: in.Notes, IsDeposit: deposit, CreatedBy: user, CreatedAt: h.now()}
	if err := tx.Create(&p).Error; err != nil {
		return err
	}
	inv.Payments = append(inv.Payments, p)
	ApplyPayments(inv, h.now())
	if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Updates(map[string]interface{}{"amount_paid": inv.AmountPaid, "balance_due": inv.BalanceDue, "status": inv.Status, "paid_at": inv.PaidAt}).Error; err != nil {
		return err
	}
	label := "Payment of"
	if deposit {
		label = "Deposit of"
	}
	h.log(tx, inv, "payment", fmt.Sprintf("%s %s %s recorded (%s)", label, inv.Currency, fmtAmount(amt, inv.Currency), methodText(method, in.keepMethod)), user)
	return nil
}

// methodText is how a payment method reads in the activity log.
func methodText(m string, raw bool) string {
	if raw {
		return m
	}
	return strings.ReplaceAll(m, "_", " ")
}

func (h *Handler) DeletePayment(c *gin.Context) {
	pid := c.Param("paymentId")
	h.simpleAction(c, func(tx *gorm.DB, inv *Invoice, user string) error {
		idx := -1
		for i, p := range inv.Payments {
			if p.ID == pid {
				idx = i
			}
		}
		if idx < 0 {
			return &apiError{http.StatusNotFound, "NOT_FOUND", "Payment not found"}
		}
		p := inv.Payments[idx]
		if err := tx.Delete(&Payment{}, "id = ?", pid).Error; err != nil {
			return err
		}
		inv.Payments = append(inv.Payments[:idx], inv.Payments[idx+1:]...)
		ApplyPayments(inv, h.now())
		if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Updates(map[string]interface{}{"amount_paid": inv.AmountPaid, "balance_due": inv.BalanceDue, "status": inv.Status, "paid_at": inv.PaidAt}).Error; err != nil {
			return err
		}
		h.log(tx, inv, "payment_removed", fmt.Sprintf("Payment of %s %s removed", inv.Currency, fmtAmount(p.Amount, inv.Currency)), user)
		return nil
	})
}

// ---------- recurring ----------

func (h *Handler) runRecurring(org, user string) int {
	today := dateOnly(h.now())
	var due []Invoice
	h.DB.Where("organization_id = ? AND doc_type = ? AND is_recurring = ? AND status <> ? AND next_recurrence_date <= ?", org, TypeInvoice, true, StatusVoid, today).
		Preload("Items").Limit(100).Find(&due)
	created := 0
	for i := range due {
		src := &due[i]
		_ = h.DB.Transaction(func(tx *gorm.DB) error {
			// Re-read under lock so concurrent requests don't double-generate.
			var fresh Invoice
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", src.ID).First(&fresh).Error; err != nil {
				return err
			}
			for guard := 0; fresh.IsRecurring && fresh.NextRecurrenceDate != nil && !dateOnly(*fresh.NextRecurrenceDate).After(today) && guard < 24; guard++ {
				issue := dateOnly(*fresh.NextRecurrenceDate)
				if fresh.RecurrenceEndDate != nil && issue.After(dateOnly(*fresh.RecurrenceEndDate)) {
					fresh.IsRecurring = false
					break
				}
				n, err := h.copyInvoice(tx, src, TypeInvoice, user, issue, true)
				if err != nil {
					return err
				}
				n.RecurrenceParentID = &src.ID
				tx.Model(&Invoice{}).Where("id = ?", n.ID).Update("recurrence_parent_id", src.ID)
				h.log(tx, n, "created", "Generated automatically from recurring invoice "+src.Number, user)
				next := NextDate(issue, fresh.RecurrenceInterval)
				fresh.NextRecurrenceDate = &next
				created++
			}
			return tx.Model(&Invoice{}).Where("id = ?", src.ID).Updates(map[string]interface{}{"next_recurrence_date": fresh.NextRecurrenceDate, "is_recurring": fresh.IsRecurring}).Error
		})
	}
	return created
}

func (h *Handler) RunRecurring(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	n := h.runRecurring(org, ctxString(c, "user_id"))
	ok(c, http.StatusOK, gin.H{"created": n})
}

// ---------- stats ----------

type monthPoint struct {
	Month    string  `json:"month"`
	Invoiced float64 `json:"invoiced"`
	Paid     float64 `json:"paid"`
}

func (h *Handler) Stats(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	h.runRecurring(org, ctxString(c, "user_id"))
	now := h.nowFor(c)
	today := dateOnly(now)
	s, _ := h.settingsFor(h.DB, org, false)
	cur := "AUD"
	if s != nil && s.Currency != "" {
		cur = strings.ToUpper(s.Currency)
	}
	var invs []Invoice
	if err := h.DB.Select("id, doc_type, status, total, amount_paid, balance_due, issue_date, due_date, client_name, customer_id, currency, base_currency, exchange_rate, total_base").
		Where("organization_id = ?", org).Find(&invs).Error; err != nil {
		failErr(c, err)
		return
	}
	var payments []Payment
	h.DB.Where("organization_id = ? AND date >= ?", org, today.AddDate(0, -11, -today.Day()+1)).Find(&payments)
	// Totals are in the organisation's currency; documents in other currencies
	// are reported separately (other_currencies) rather than added in.
	invCur := map[string]string{}
	// Each foreign-currency figure is also given in the home currency,
	// converted at the rate locked on each invoice ("converted at…").
	type otherTotal struct {
		Currency        string  `json:"currency"`
		Open            int     `json:"open"`
		Outstanding     float64 `json:"outstanding"`
		Overdue         float64 `json:"overdue"`
		OutstandingBase float64 `json:"outstanding_base"`
		OverdueBase     float64 `json:"overdue_base"`
		NoRate          int     `json:"no_rate"`
	}
	others := map[string]*otherTotal{}
	var conv struct {
		Outstanding       float64 `json:"outstanding"`
		Overdue           float64 `json:"overdue"`
		InvoicedThisMonth float64 `json:"invoiced_this_month"`
		Open              int     `json:"open"`
		NoRate            int     `json:"no_rate"`
	}

	aging := map[string]float64{"current": 0, "1_30": 0, "31_60": 0, "61_90": 0, "90_plus": 0}
	var outstanding, overdue, drafts, paidThisMonth, invoicedThisMonth float64
	var outstandingN, overdueN, draftsN int
	counts := map[string]int{}
	clients := map[string]float64{}
	months := make([]monthPoint, 12)
	idx := map[string]int{}
	for i := 0; i < 12; i++ {
		m := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, i-11, 0)
		key := m.Format("2006-01")
		months[i] = monthPoint{Month: key}
		idx[key] = i
	}
	thisMonth := today.Format("2006-01")
	openQuotes := 0.0
	for i := range invs {
		inv := &invs[i]
		ic := strings.ToUpper(strings.TrimSpace(inv.Currency))
		if ic == "" {
			ic = cur
		}
		invCur[inv.ID] = ic
		Derive(inv, now)
		counts[inv.DocType+":"+inv.DisplayStatus]++
		if inv.Status == StatusVoid {
			continue
		}
		if ic != cur {
			if inv.DocType == TypeInvoice && (inv.Status == StatusSent || inv.Status == StatusPartial) && inv.BalanceDue > 0 {
				o := others[ic]
				if o == nil {
					o = &otherTotal{Currency: ic}
					others[ic] = o
				}
				o.Open++
				o.Outstanding = round2(o.Outstanding + inv.BalanceDue)
				bb, okb := ToBase(inv, inv.BalanceDue)
				if !okb || inv.BaseCurrency != cur {
					o.NoRate++
					conv.NoRate++
				} else {
					conv.Open++
					o.OutstandingBase = roundCur(o.OutstandingBase+bb, cur)
					conv.Outstanding = roundCur(conv.Outstanding+bb, cur)
				}
				if inv.DisplayStatus == StatusOverdue {
					o.Overdue = round2(o.Overdue + inv.BalanceDue)
					if okb && inv.BaseCurrency == cur {
						o.OverdueBase = roundCur(o.OverdueBase+bb, cur)
						conv.Overdue = roundCur(conv.Overdue+bb, cur)
					}
				}
			}
			if inv.DocType == TypeInvoice && inv.Status != StatusDraft && inv.IssueDate.Format("2006-01") == thisMonth && inv.BaseCurrency == cur && inv.ExchangeRate > 0 {
				conv.InvoicedThisMonth = roundCur(conv.InvoicedThisMonth+inv.TotalBase, cur)
			}
			continue
		}
		if inv.DocType == TypeQuote {
			if inv.DisplayStatus == StatusSent || inv.DisplayStatus == StatusAccepted {
				openQuotes += inv.Total
			}
			continue
		}
		if inv.Status == StatusDraft {
			drafts += inv.Total
			draftsN++
			continue
		}
		if k, okk := idx[inv.IssueDate.Format("2006-01")]; okk {
			months[k].Invoiced += inv.Total
		}
		if inv.IssueDate.Format("2006-01") == thisMonth {
			invoicedThisMonth += inv.Total
		}
		if inv.Status == StatusSent || inv.Status == StatusPartial {
			outstanding += inv.BalanceDue
			outstandingN++
			clients[inv.ClientName] += inv.BalanceDue
			switch d := inv.DaysOverdue; {
			case inv.DisplayStatus != StatusOverdue:
				aging["current"] += inv.BalanceDue
			case d <= 30:
				aging["1_30"] += inv.BalanceDue
			case d <= 60:
				aging["31_60"] += inv.BalanceDue
			case d <= 90:
				aging["61_90"] += inv.BalanceDue
			default:
				aging["90_plus"] += inv.BalanceDue
			}
			if inv.DisplayStatus == StatusOverdue {
				overdue += inv.BalanceDue
				overdueN++
			}
		}
	}
	for _, p := range payments {
		if ic, found := invCur[p.InvoiceID]; !found || ic != cur {
			continue // deleted invoice or another currency
		}
		if k, okk := idx[p.Date.Format("2006-01")]; okk {
			months[k].Paid += p.Amount
		}
		if p.Date.Format("2006-01") == thisMonth {
			paidThisMonth += p.Amount
		}
	}
	for i := range months {
		months[i].Invoiced = round2(months[i].Invoiced)
		months[i].Paid = round2(months[i].Paid)
	}
	for k := range aging {
		aging[k] = round2(aging[k])
	}
	type owed struct {
		Client string  `json:"client"`
		Amount float64 `json:"amount"`
	}
	top := []owed{}
	for k, v := range clients {
		top = append(top, owed{k, round2(v)})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Amount > top[j].Amount })
	if len(top) > 5 {
		top = top[:5]
	}
	otherList := []otherTotal{}
	for _, o := range others {
		otherList = append(otherList, *o)
	}
	sort.Slice(otherList, func(i, j int) bool { return otherList[i].Currency < otherList[j].Currency })
	ok(c, http.StatusOK, gin.H{
		"currency": cur, "outstanding": round2(outstanding), "overdue": round2(overdue), "drafts": round2(drafts),
		"paid_this_month": round2(paidThisMonth), "invoiced_this_month": round2(invoicedThisMonth), "open_quotes": round2(openQuotes),
		"aging": aging, "monthly": months, "top_outstanding": top, "counts": counts, "other_currencies": otherList,
		"converted": conv, "converted_note": "Other currencies converted to " + cur + " at the exchange rate locked on each invoice — not included in the totals above.",
		"outstanding_count": outstandingN, "overdue_count": overdueN, "drafts_count": draftsN,
	})
}

// ---------- settings & tax rates ----------

func (h *Handler) GetSettings(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	s, err := h.settingsFor(h.DB, org, false)
	if err != nil {
		failErr(c, err)
		return
	}
	// Read-only: the company logo is managed under Settings → Business details.
	s.LogoURL = logoFor(c, org)
	ok(c, http.StatusOK, gin.H{"settings": s, "email_enabled": h.emailEnabled(org), "sender": h.sender(org)})
}

func (h *Handler) UpdateSettings(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in Settings
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	if in.NextInvoiceNumber < 1 || in.NextQuoteNumber < 1 {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Next numbers must be positive")
		return
	}
	if in.DefaultDueDays < 0 || in.DefaultDueDays > 365 || in.QuoteValidDays < 0 || in.QuoteValidDays > 365 {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Days must be between 0 and 365")
		return
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	cur, err := h.settingsFor(h.DB, org, false)
	if err != nil {
		failErr(c, err)
		return
	}
	// The currency here is the organisation's home currency: changing it
	// changes the default for new records in every module (existing
	// documents keep theirs).
	if in.Currency == "" {
		in.Currency = cur.Currency
	}
	if !currency.IsSupported(in.Currency) {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Currency "+in.Currency+" is not supported")
		return
	}
	if in.Currency != cur.Currency {
		if _, err := orgprefs.Set(h.DB, org, orgprefs.Input{Currency: in.Currency}, orgprefs.SourceUser); err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
	}
	// The logo is no longer edited here (PUT /organization/logo); keep
	// whatever legacy value is stored.
	in.LogoURL = cur.LogoURL
	in.OrganizationID = org
	in.UpdatedAt = h.now()
	if err := h.DB.Save(&in).Error; err != nil {
		failErr(c, err)
		return
	}
	in.LogoURL = logoFor(c, org)
	ok(c, http.StatusOK, gin.H{"settings": in, "email_enabled": h.emailEnabled(org), "sender": h.sender(org)})
}

func (h *Handler) ListTaxRates(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var rates []TaxRate
	h.DB.Where("organization_id = ?", org).Order("is_default DESC, rate DESC").Find(&rates)
	if len(rates) == 0 {
		rates = []TaxRate{
			{ID: uuid.NewString(), OrganizationID: org, Name: "GST", Rate: 10, IsDefault: true, CreatedAt: h.now()},
			{ID: uuid.NewString(), OrganizationID: org, Name: "GST Free", Rate: 0, CreatedAt: h.now()},
		}
		h.DB.Create(&rates)
	}
	ok(c, http.StatusOK, rates)
}

type taxInput struct {
	Name      string  `json:"name"`
	Rate      float64 `json:"rate"`
	IsDefault bool    `json:"is_default"`
}

func (h *Handler) saveTax(c *gin.Context, id string) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in taxInput
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Name) == "" || in.Rate < 0 || in.Rate > 100 {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Give the tax a name and a rate between 0 and 100")
		return
	}
	var t TaxRate
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if id != "" {
			if err := tx.Where("id = ? AND organization_id = ?", id, org).First(&t).Error; err != nil {
				return err
			}
		} else {
			t = TaxRate{ID: uuid.NewString(), OrganizationID: org, CreatedAt: h.now()}
		}
		t.Name, t.Rate, t.IsDefault = strings.TrimSpace(in.Name), in.Rate, in.IsDefault
		if t.IsDefault {
			tx.Model(&TaxRate{}).Where("organization_id = ?", org).Update("is_default", false)
			tx.Model(&Settings{}).Where("organization_id = ?", org).Updates(map[string]interface{}{"default_tax_rate": t.Rate, "tax_label": t.Name})
		}
		return tx.Save(&t).Error
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, t)
}

func (h *Handler) CreateTaxRate(c *gin.Context) { h.saveTax(c, "") }
func (h *Handler) UpdateTaxRate(c *gin.Context) { h.saveTax(c, c.Param("id")) }

func (h *Handler) DeleteTaxRate(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	res := h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), org).Delete(&TaxRate{})
	if res.RowsAffected == 0 {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Tax rate not found")
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

// Clients returns previously invoiced clients (for autocomplete), merged with
// the organisation's customer records. Each carries the currency a new
// document for it should use: the linked customer's billing currency, else
// the currency last used for that client.
func (h *Handler) Clients(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	type client struct {
		CustomerID     *string  `json:"customer_id"`
		Name           string   `json:"name"`
		Email          string   `json:"email"`
		Phone          string   `json:"phone"`
		Address        string   `json:"address"`
		TaxNumber      string   `json:"tax_number"`
		Source         string   `json:"source"`
		Currency       string   `json:"currency"`
		CurrencySource string   `json:"currency_source"` // customer | country | default | last_used
		LastCurrency   string   `json:"last_currency,omitempty"`
		CountryCode    string   `json:"country_code,omitempty"`
		CountryName    string   `json:"country_name,omitempty"`
		CCEmails       []string `json:"cc_emails"`
	}
	s, err := h.settingsFor(h.DB, org, false)
	if err != nil {
		failErr(c, err)
		return
	}
	customers := h.loadCustomers(org, nil, 2000)
	byID := map[string]customerRow{}
	for _, cu := range customers {
		byID[cu.ID] = cu
	}
	// Contacts copied on invoices, per customer (for the document's CC list).
	ccBy := map[string][]string{}
	if h.hasTable("customer_contacts") {
		var rows []struct {
			CustomerID string
			Email      string
		}
		h.DB.Table("customer_contacts").Select("customer_id, email").
			Where("organization_id = ? AND deleted_at IS NULL AND cc_invoices = ? AND is_primary_billing = ?", org, true, false).
			Order("created_at ASC").Scan(&rows)
		for _, r := range rows {
			ccBy[r.CustomerID] = append(ccBy[r.CustomerID], strings.ToLower(r.Email))
		}
	}
	withCustomer := func(cl *client, cu customerRow) {
		cl.Currency, cl.CurrencySource = currency.ForCustomer(cu.Currency, cu.CountryCode, cu.AddressCountry, s.Currency)
		cl.CountryCode = cu.CountryCode
		if cl.CountryCode == "" {
			cl.CountryCode = currency.CountryCode(cu.AddressCountry)
		}
		cl.CountryName = currency.CountryName(cl.CountryCode)
		if cl.CountryName == "" {
			cl.CountryName = cu.AddressCountry
		}
		if cc := ccBy[cu.ID]; cc != nil {
			cl.CCEmails = cc
		}
	}

	out := []client{}
	seen := map[string]bool{}
	var rows []Invoice
	h.DB.Select("customer_id, client_name, client_email, client_phone, client_address, client_tax_number, currency, created_at").
		Where("organization_id = ?", org).Order("created_at DESC").Limit(1000).Find(&rows)
	for _, r := range rows {
		key := strings.ToLower(r.ClientName + "|" + r.ClientEmail)
		if seen[key] || r.ClientName == "" {
			continue
		}
		seen[key] = true
		last := strings.ToUpper(r.Currency)
		if last == "" {
			last = strings.ToUpper(s.Currency)
		}
		cl := client{CustomerID: r.CustomerID, Name: r.ClientName, Email: r.ClientEmail, Phone: r.ClientPhone, Address: r.ClientAddress,
			TaxNumber: r.ClientTaxNumber, Source: "invoices", Currency: last, CurrencySource: "last_used", LastCurrency: last, CCEmails: []string{}}
		if r.CustomerID != nil {
			if cu, found := byID[*r.CustomerID]; found {
				withCustomer(&cl, cu)
			}
		}
		out = append(out, cl)
	}
	for _, cu := range customers {
		name := cu.name()
		key := strings.ToLower(name + "|" + cu.Email)
		if seen[key] || name == "" {
			continue
		}
		seen[key] = true
		id := cu.ID
		cl := client{CustomerID: &id, Name: name, Email: cu.Email, Phone: cu.Phone, Address: cu.address(), Source: "customers", CCEmails: []string{}}
		withCustomer(&cl, cu)
		out = append(out, cl)
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	ok(c, http.StatusOK, out)
}

func nonEmpty(parts ...string) []string {
	r := []string{}
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			r = append(r, p)
		}
	}
	return r
}

func (h *Handler) ExportCSV(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	docType := c.DefaultQuery("type", TypeInvoice)
	var list []Invoice
	q := h.DB.Where("organization_id = ?", org)
	if docType != "all" {
		q = q.Where("doc_type = ?", docType)
	}
	q.Order("issue_date DESC").Find(&list)
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%ss-%s.csv", docType, h.now().Format("2006-01-02")))
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"Number", "Type", "Status", "Client", "Email", "Reference", "Issue date", "Due date", "Currency", "Subtotal", "Discount", "Tax", "Total", "Paid", "Balance"})
	for i := range list {
		inv := &list[i]
		Derive(inv, h.now())
		_ = w.Write([]string{inv.Number, inv.DocType, inv.DisplayStatus, csvSafe(inv.ClientName), csvSafe(inv.ClientEmail), csvSafe(inv.Reference),
			inv.IssueDate.Format("2006-01-02"), inv.DueDate.Format("2006-01-02"), inv.Currency,
			money(inv.Subtotal), money(inv.DiscountTotal), money(inv.TaxTotal), money(inv.Total), money(inv.AmountPaid), money(inv.BalanceDue)})
	}
	w.Flush()
}

// csvSafe neutralises spreadsheet formula injection.
func csvSafe(s string) string {
	if s != "" && strings.ContainsAny(s[:1], "=+-@") {
		return "'" + s
	}
	return s
}

func money(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// ---------- public ----------

func (h *Handler) publicLoad(token string) (*Invoice, *Settings, error) {
	if len(token) < 20 {
		return nil, nil, gorm.ErrRecordNotFound
	}
	var inv Invoice
	if err := h.DB.Where("public_token = ?", token).
		Preload("Items", func(db *gorm.DB) *gorm.DB { return db.Order("position ASC") }).
		Preload("Payments").First(&inv).Error; err != nil {
		return nil, nil, err
	}
	if inv.Status == StatusDraft {
		return nil, nil, gorm.ErrRecordNotFound
	}
	s, err := h.settingsFor(h.DB, inv.OrganizationID, false)
	if err != nil {
		return nil, nil, err
	}
	Derive(&inv, h.now())
	return &inv, s, nil
}

func (h *Handler) PublicView(c *gin.Context) {
	inv, s, err := h.publicLoad(c.Param("token"))
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "This link is invalid or has expired")
		return
	}
	if inv.ViewedAt == nil {
		t := h.now()
		h.DB.Model(&Invoice{}).Where("id = ?", inv.ID).Update("viewed_at", t)
		h.log(h.DB, inv, "viewed", "Viewed by client", "")
	}
	inv.CreatedBy = ""
	inv.PublicToken = ""
	for i := range inv.Payments {
		inv.Payments[i].CreatedBy = ""
		inv.Payments[i].Notes = ""
	}
	s.LogoURL = logoFor(c, inv.OrganizationID)
	if s.BusinessName == "" && h.DB.Migrator().HasTable("organizations") {
		h.DB.Raw("SELECT name FROM organizations WHERE id = ?", inv.OrganizationID).Scan(&s.BusinessName)
	}
	ok(c, http.StatusOK, gin.H{"invoice": inv, "business": s, "pay_enabled": h.payable(inv)})
}

func (h *Handler) PublicAccept(c *gin.Context) {
	inv, _, err := h.publicLoad(c.Param("token"))
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "This link is invalid or has expired")
		return
	}
	if inv.DocType != TypeQuote || inv.Status != StatusSent || inv.DisplayStatus == StatusExpired {
		fail(c, http.StatusConflict, "NOT_ACCEPTABLE", "This quote can no longer be accepted")
		return
	}
	h.DB.Model(&Invoice{}).Where("id = ?", inv.ID).Update("status", StatusAccepted)
	h.log(h.DB, inv, "accepted", "Accepted by client online", "")
	ok(c, http.StatusOK, gin.H{"accepted": true})
}
