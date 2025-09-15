package finance

import (
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

func (h *Handler) GetChartOfAccounts(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	accounts, err := h.service.GetChartOfAccounts(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": accounts})
}

func (h *Handler) CreateAccount(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	var account ChartOfAccount
	if err := c.ShouldBindJSON(&account); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	account.OrganizationID = orgID
	if err := h.service.CreateAccount(&account); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": account})
}

// Journal Entry handlers
func (h *Handler) GetJournalEntries(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	// For now, return empty array - would implement with service method
	c.JSON(http.StatusOK, gin.H{"success": true, "data": []JournalEntry{}})
}

func (h *Handler) CreateJournalEntry(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	var entry JournalEntry
	if err := c.ShouldBindJSON(&entry); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	entry.OrganizationID = orgID
	if err := h.service.CreateJournalEntry(&entry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": entry})
}

func (h *Handler) PostJournalEntry(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	entryID := c.Param("id")
	userID := c.GetString("user_id")
	if userID == "" {
		userID = "system"
	}

	if err := h.service.PostJournalEntry(entryID, userID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Journal entry posted successfully"})
}

// Financial Report handlers
func (h *Handler) GetTrialBalance(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	asOfDateStr := c.Query("as_of_date")
	asOfDate := time.Now()
	if asOfDateStr != "" {
		if parsed, err := time.Parse("2006-01-02", asOfDateStr); err == nil {
			asOfDate = parsed
		}
	}

	trialBalance, err := h.service.GetTrialBalance(orgID, asOfDate)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": trialBalance})
}

func (h *Handler) GetProfitLoss(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	startDateStr := c.Query("start_date")
	endDateStr := c.Query("end_date")

	startDate := time.Now().AddDate(0, -1, 0) // Default to last month
	endDate := time.Now()

	if startDateStr != "" {
		if parsed, err := time.Parse("2006-01-02", startDateStr); err == nil {
			startDate = parsed
		}
	}
	if endDateStr != "" {
		if parsed, err := time.Parse("2006-01-02", endDateStr); err == nil {
			endDate = parsed
		}
	}

	profitLoss, err := h.service.GetProfitLoss(orgID, startDate, endDate)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": profitLoss})
}

func (h *Handler) GetBalanceSheet(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	asOfDateStr := c.Query("as_of_date")
	asOfDate := time.Now()
	if asOfDateStr != "" {
		if parsed, err := time.Parse("2006-01-02", asOfDateStr); err == nil {
			asOfDate = parsed
		}
	}

	balanceSheet, err := h.service.GetBalanceSheet(orgID, asOfDate)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": balanceSheet})
}

func (h *Handler) GetGeneralLedger(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	accountID := c.Query("account_id")
	startDateStr := c.Query("start_date")
	endDateStr := c.Query("end_date")

	var startDate, endDate time.Time
	if startDateStr != "" {
		if parsed, err := time.Parse("2006-01-02", startDateStr); err == nil {
			startDate = parsed
		}
	}
	if endDateStr != "" {
		if parsed, err := time.Parse("2006-01-02", endDateStr); err == nil {
			endDate = parsed
		}
	}

	ledgerEntries, err := h.service.GetGeneralLedger(orgID, accountID, startDate, endDate)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": ledgerEntries})
}

func (h *Handler) GetDashboard(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	dashboard, err := h.service.GetDashboard(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": dashboard})
}

// Invoice handlers
func (h *Handler) GetInvoices(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	invoices, err := h.service.GetInvoices(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": invoices})
}

func (h *Handler) CreateInvoice(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	var invoice Invoice
	if err := c.ShouldBindJSON(&invoice); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	invoice.OrganizationID = orgID
	if err := h.service.CreateInvoice(&invoice); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": invoice})
}

// Bill handlers
func (h *Handler) GetBills(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	bills, err := h.service.GetBills(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": bills})
}

func (h *Handler) CreateBill(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	var bill Bill
	if err := c.ShouldBindJSON(&bill); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	bill.OrganizationID = orgID
	if err := h.service.CreateBill(&bill); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": bill})
}

// Vendor handlers
func (h *Handler) GetVendors(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	vendors, err := h.service.GetVendors(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": vendors})
}

func (h *Handler) CreateVendor(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	var vendor Vendor
	if err := c.ShouldBindJSON(&vendor); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	vendor.OrganizationID = orgID
	if err := h.service.CreateVendor(&vendor); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": vendor})
}

// Bank Account handlers
func (h *Handler) GetBankAccounts(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	accounts, err := h.service.GetBankAccounts(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": accounts})
}

func (h *Handler) CreateBankAccount(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	var account BankAccount
	if err := c.ShouldBindJSON(&account); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	account.OrganizationID = orgID
	if err := h.service.CreateBankAccount(&account); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": account})
}

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	// Chart of Accounts
	r.GET("/chart-of-accounts", h.GetChartOfAccounts)
	r.POST("/chart-of-accounts", h.CreateAccount)

	// Invoicing & AR
	r.GET("/invoices", h.GetInvoices)
	r.POST("/invoices", h.CreateInvoice)

	// Bills & AP
	r.GET("/bills", h.GetBills)
	r.POST("/bills", h.CreateBill)

	// Vendors
	r.GET("/vendors", h.GetVendors)
	r.POST("/vendors", h.CreateVendor)

	// Banking
	r.GET("/bank-accounts", h.GetBankAccounts)
	r.POST("/bank-accounts", h.CreateBankAccount)

	// Journal Entries & Ledger
	r.GET("/journal-entries", h.GetJournalEntries)
	r.POST("/journal-entries", h.CreateJournalEntry)
	r.POST("/journal-entries/:id/post", h.PostJournalEntry)

	// Financial Reports
	r.GET("/reports/trial-balance", h.GetTrialBalance)
	r.GET("/reports/profit-loss", h.GetProfitLoss)
	r.GET("/reports/balance-sheet", h.GetBalanceSheet)
	r.GET("/reports/general-ledger", h.GetGeneralLedger)

	// Dashboard
	r.GET("/dashboard", h.GetDashboard)
}