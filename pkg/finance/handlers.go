package finance

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// org returns the caller's organisation or aborts with 403.
func org(c *gin.Context) (string, bool) {
	id := c.GetString("organization_id")
	if id == "" {
		if v, ok := c.Get("org_id"); ok && v != nil {
			if s, ok := v.(string); ok {
				id = s
			}
		}
	}
	if id == "" {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "No organisation is associated with this account"})
		return "", false
	}
	return id, true
}

func user(c *gin.Context) string {
	if u := c.GetString("user_id"); u != "" {
		return u
	}
	return "system"
}

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

// fail maps service errors to HTTP responses.
func fail(c *gin.Context, err error) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": ve.Msg})
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "Not found"})
	default:
		log.Printf("finance: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Something went wrong — please try again"})
	}
}

func bind(c *gin.Context, v interface{}) bool {
	if err := c.ShouldBindJSON(v); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Invalid request: " + err.Error()})
		return false
	}
	return true
}

func queryDate(c *gin.Context, key string, def time.Time) time.Time {
	if d, ok := ParseDate(c.Query(key)); ok {
		return d
	}
	return def
}

// ---------------------------------------------------------------------------
// Chart of accounts

func (h *Handler) GetChartOfAccounts(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	accounts, err := h.service.GetChartOfAccounts(orgID)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, accounts)
}

func (h *Handler) CreateAccount(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in AccountInput
	if !bind(c, &in) {
		return
	}
	acc, err := h.service.CreateAccount(orgID, in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusCreated, acc)
}

func (h *Handler) UpdateAccount(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in AccountInput
	if !bind(c, &in) {
		return
	}
	acc, err := h.service.UpdateAccount(orgID, c.Param("id"), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, acc)
}

func (h *Handler) DeleteAccount(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	archived, err := h.service.DeleteAccount(orgID, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"archived": archived})
}

// ---------------------------------------------------------------------------
// Journal entries

func (h *Handler) GetJournalEntries(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	entries, err := h.service.ListJournalEntries(orgID, c.Query("status"), c.Query("q"), c.Query("source"),
		queryDate(c, "from", time.Time{}), queryDate(c, "to", time.Time{}))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, entries)
}

func (h *Handler) GetJournalEntry(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	e, err := h.service.GetJournalEntry(orgID, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, e)
}

func (h *Handler) CreateJournalEntry(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in JournalInput
	if !bind(c, &in) {
		return
	}
	e, err := h.service.CreateJournalEntry(orgID, user(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusCreated, e)
}

func (h *Handler) UpdateJournalEntry(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in JournalInput
	if !bind(c, &in) {
		return
	}
	e, err := h.service.UpdateJournalEntry(orgID, user(c), c.Param("id"), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, e)
}

func (h *Handler) DeleteJournalEntry(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	if err := h.service.DeleteJournalEntry(orgID, c.Param("id")); err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) PostJournalEntry(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	if err := h.service.PostJournalEntry(orgID, c.Param("id"), user(c)); err != nil {
		fail(c, err)
		return
	}
	e, err := h.service.GetJournalEntry(orgID, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, e)
}

func (h *Handler) ReverseJournalEntry(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in struct {
		Date string `json:"date"`
	}
	_ = c.ShouldBindJSON(&in)
	e, err := h.service.ReverseJournalEntry(orgID, c.Param("id"), user(c), in.Date)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, e)
}

// ---------------------------------------------------------------------------
// Reports

func (h *Handler) GetTrialBalance(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	rep, err := h.service.GetTrialBalance(orgID, queryDate(c, "as_of_date", today()))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, rep)
}

func (h *Handler) GetProfitLoss(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	t := today()
	start := queryDate(c, "start_date", time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC))
	end := queryDate(c, "end_date", t)
	if end.Before(start) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "End date must be on or after the start date"})
		return
	}
	pl, err := h.service.GetProfitLoss(orgID, start, end)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, pl)
}

func (h *Handler) GetBalanceSheet(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	bs, err := h.service.GetBalanceSheet(orgID, queryDate(c, "as_of_date", today()))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, bs)
}

func (h *Handler) GetGeneralLedger(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	entries, err := h.service.GetGeneralLedger(orgID, c.Query("account_id"), queryDate(c, "start_date", time.Time{}), queryDate(c, "end_date", time.Time{}))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, entries)
}

func (h *Handler) GetDashboard(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	d, err := h.service.GetDashboard(orgID)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, d)
}

func (h *Handler) GetSummary(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	d, err := h.service.GetSummary(orgID)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, d)
}

// ---------------------------------------------------------------------------
// Legacy invoices

func (h *Handler) GetInvoices(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	invoices, err := h.service.GetInvoices(orgID)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, invoices)
}

func (h *Handler) CreateInvoice(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var invoice Invoice
	if !bind(c, &invoice) {
		return
	}
	invoice.OrganizationID = orgID
	if err := h.service.CreateInvoice(&invoice); err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusCreated, invoice)
}

// ---------------------------------------------------------------------------
// Bills

func (h *Handler) GetBills(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	bills, err := h.service.GetBills(orgID, c.Query("status"), c.Query("q"), c.Query("vendor_id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, bills)
}

func (h *Handler) GetBill(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	b, err := h.service.GetBill(orgID, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, b)
}

func (h *Handler) CreateBill(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in BillInput
	if !bind(c, &in) {
		return
	}
	b, err := h.service.CreateBill(orgID, user(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusCreated, b)
}

func (h *Handler) UpdateBill(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in BillInput
	if !bind(c, &in) {
		return
	}
	b, err := h.service.UpdateBill(orgID, user(c), c.Param("id"), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, b)
}

func (h *Handler) ApproveBill(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	b, err := h.service.ApproveBill(orgID, user(c), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, b)
}

func (h *Handler) DeleteBill(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	if err := h.service.DeleteBill(orgID, user(c), c.Param("id")); err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) RecordBillPayment(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in PaymentInput
	if !bind(c, &in) {
		return
	}
	b, err := h.service.RecordBillPayment(orgID, user(c), c.Param("id"), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusCreated, b)
}

func (h *Handler) DeleteBillPayment(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	b, err := h.service.DeleteBillPayment(orgID, user(c), c.Param("id"), c.Param("paymentId"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, b)
}

// ---------------------------------------------------------------------------
// Vendors

func (h *Handler) GetVendors(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	vendors, err := h.service.GetVendors(orgID, c.Query("include_inactive") == "true", c.Query("q"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, vendors)
}

func (h *Handler) CreateVendor(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in VendorInput
	if !bind(c, &in) {
		return
	}
	v, err := h.service.CreateVendor(orgID, in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusCreated, v)
}

func (h *Handler) UpdateVendor(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in VendorInput
	if !bind(c, &in) {
		return
	}
	v, err := h.service.UpdateVendor(orgID, c.Param("id"), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, v)
}

func (h *Handler) DeleteVendor(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	archived, err := h.service.DeleteVendor(orgID, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"archived": archived})
}

// ---------------------------------------------------------------------------
// Banking

func (h *Handler) GetBankAccounts(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	accounts, err := h.service.GetBankAccounts(orgID, c.Query("include_inactive") == "true")
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, accounts)
}

func (h *Handler) CreateBankAccount(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in BankAccountInput
	if !bind(c, &in) {
		return
	}
	a, err := h.service.CreateBankAccount(orgID, user(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusCreated, a)
}

func (h *Handler) UpdateBankAccount(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in BankAccountInput
	if !bind(c, &in) {
		return
	}
	a, err := h.service.UpdateBankAccount(orgID, c.Param("id"), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, a)
}

func (h *Handler) DeleteBankAccount(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	archived, err := h.service.DeleteBankAccount(orgID, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"archived": archived})
}

func (h *Handler) GetBankTransactions(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	txns, err := h.service.ListBankTransactions(orgID, TxnFilter{BankAccountID: c.Query("bank_account_id"), Status: c.Query("status"),
		Direction: c.Query("direction"), Q: c.Query("q"), From: queryDate(c, "from", time.Time{}), To: queryDate(c, "to", time.Time{})})
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, txns)
}

func (h *Handler) CreateBankTransaction(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in BankTxnInput
	if !bind(c, &in) {
		return
	}
	t, err := h.service.CreateBankTransaction(orgID, user(c), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusCreated, t)
}

func (h *Handler) CategorizeBankTransaction(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in struct {
		AccountID string `json:"account_id"`
	}
	if !bind(c, &in) {
		return
	}
	t, err := h.service.CategorizeBankTransaction(orgID, user(c), c.Param("id"), in.AccountID)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, t)
}

func (h *Handler) ReconcileBankTransaction(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	var in struct {
		Reconciled bool `json:"reconciled"`
	}
	if !bind(c, &in) {
		return
	}
	t, err := h.service.SetReconciled(orgID, c.Param("id"), in.Reconciled)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, t)
}

func (h *Handler) DeleteBankTransaction(c *gin.Context) {
	orgID, okk := org(c)
	if !okk {
		return
	}
	if err := h.service.DeleteBankTransaction(orgID, user(c), c.Param("id")); err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	// Overview
	r.GET("/summary", h.GetSummary)
	r.GET("/dashboard", h.GetDashboard)

	// Chart of Accounts
	r.GET("/chart-of-accounts", h.GetChartOfAccounts)
	r.POST("/chart-of-accounts", h.CreateAccount)
	r.PUT("/chart-of-accounts/:id", h.UpdateAccount)
	r.DELETE("/chart-of-accounts/:id", h.DeleteAccount)

	// Invoicing & AR (legacy)
	r.GET("/invoices", h.GetInvoices)
	r.POST("/invoices", h.CreateInvoice)

	// Bills & AP
	r.GET("/bills", h.GetBills)
	r.POST("/bills", h.CreateBill)
	r.GET("/bills/:id", h.GetBill)
	r.PUT("/bills/:id", h.UpdateBill)
	r.DELETE("/bills/:id", h.DeleteBill)
	r.POST("/bills/:id/approve", h.ApproveBill)
	r.POST("/bills/:id/payments", h.RecordBillPayment)
	r.DELETE("/bills/:id/payments/:paymentId", h.DeleteBillPayment)

	// Vendors
	r.GET("/vendors", h.GetVendors)
	r.POST("/vendors", h.CreateVendor)
	r.PUT("/vendors/:id", h.UpdateVendor)
	r.DELETE("/vendors/:id", h.DeleteVendor)

	// Banking
	r.GET("/bank-accounts", h.GetBankAccounts)
	r.POST("/bank-accounts", h.CreateBankAccount)
	r.PUT("/bank-accounts/:id", h.UpdateBankAccount)
	r.DELETE("/bank-accounts/:id", h.DeleteBankAccount)
	r.GET("/bank-transactions", h.GetBankTransactions)
	r.POST("/bank-transactions", h.CreateBankTransaction)
	r.POST("/bank-transactions/:id/categorize", h.CategorizeBankTransaction)
	r.POST("/bank-transactions/:id/reconcile", h.ReconcileBankTransaction)
	r.DELETE("/bank-transactions/:id", h.DeleteBankTransaction)

	// Journal Entries & Ledger
	r.GET("/journal-entries", h.GetJournalEntries)
	r.POST("/journal-entries", h.CreateJournalEntry)
	r.GET("/journal-entries/:id", h.GetJournalEntry)
	r.PUT("/journal-entries/:id", h.UpdateJournalEntry)
	r.DELETE("/journal-entries/:id", h.DeleteJournalEntry)
	r.POST("/journal-entries/:id/post", h.PostJournalEntry)
	r.POST("/journal-entries/:id/reverse", h.ReverseJournalEntry)

	// Financial Reports
	r.GET("/reports/trial-balance", h.GetTrialBalance)
	r.GET("/reports/profit-loss", h.GetProfitLoss)
	r.GET("/reports/balance-sheet", h.GetBalanceSheet)
	r.GET("/reports/general-ledger", h.GetGeneralLedger)
}
