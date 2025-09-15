package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// POS Transaction handlers

func (h *Handler) CreatePOSTransaction(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var req struct {
		CustomerID     *string                `json:"customer_id"`
		Items          []models.POSItem       `json:"items" binding:"required"`
		Payments       []models.POSPayment    `json:"payments" binding:"required"`
		DiscountAmount float64               `json:"discount_amount"`
		Notes          string                `json:"notes"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Start transaction
	tx := h.DB.Begin()

	// Create POS transaction
	transaction := models.POSTransaction{
		OrganizationID: orgID,
		CustomerID:     req.CustomerID,
		CashierID:      c.GetString("user_id"),
		Type:           "sale",
		Status:         "completed",
		DiscountAmount: req.DiscountAmount,
		Notes:          req.Notes,
	}

	if err := tx.Create(&transaction).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create transaction"})
		return
	}

	// Process items and calculate totals
	var subTotal, taxTotal float64
	for _, item := range req.Items {
		item.TransactionID = transaction.ID

		// Validate product/service exists and update stock
		if item.ProductID != nil {
			var product models.Product
			if err := tx.Where("id = ? AND organization_id = ?", *item.ProductID, orgID).First(&product).Error; err != nil {
				tx.Rollback()
				c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
				return
			}

			// Check stock availability
			if product.CurrentStock < item.Quantity {
				tx.Rollback()
				c.JSON(http.StatusBadRequest, gin.H{"error": "Insufficient stock for product: " + product.Name})
				return
			}

			// Update product stock
			newStock := product.CurrentStock - item.Quantity
			if err := tx.Model(&product).Update("current_stock", newStock).Error; err != nil {
				tx.Rollback()
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update product stock"})
				return
			}

			// Create inventory movement
			movement := models.InventoryMovement{
				OrganizationID:   orgID,
				ProductID:        *item.ProductID,
				MovementType:     "out",
				Quantity:         item.Quantity,
				PreviousQuantity: product.CurrentStock,
				NewQuantity:      newStock,
				UnitCost:         product.CostPrice,
				Reference:        transaction.TransactionNumber,
				ReferenceType:    "sale",
				Notes:            "Sold via POS",
				CreatedBy:        c.GetString("user_id"),
			}

			if err := tx.Create(&movement).Error; err != nil {
				tx.Rollback()
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create inventory movement"})
				return
			}

			item.ItemName = product.Name
			item.UnitPrice = product.SellingPrice
		} else if item.ServiceID != nil {
			var service models.Service
			if err := tx.Where("id = ? AND organization_id = ?", *item.ServiceID, orgID).First(&service).Error; err != nil {
				tx.Rollback()
				c.JSON(http.StatusNotFound, gin.H{"error": "Service not found"})
				return
			}
			item.ItemName = service.Name
			item.UnitPrice = service.Price
		}

		// Calculate item totals
		item.TotalPrice = float64(item.Quantity) * item.UnitPrice
		item.TotalPrice -= item.DiscountAmount
		item.TaxAmount = (item.TotalPrice * item.TaxRate) / 100

		if err := tx.Create(&item).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create transaction item"})
			return
		}

		subTotal += item.TotalPrice
		taxTotal += item.TaxAmount
	}

	// Process payments
	var totalPaid float64
	for _, payment := range req.Payments {
		payment.TransactionID = transaction.ID
		payment.Status = "completed"
		now := time.Now()
		payment.ProcessedAt = &now

		if err := tx.Create(&payment).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create payment"})
			return
		}

		totalPaid += payment.Amount
	}

	// Update transaction totals
	transaction.SubTotal = subTotal
	transaction.TaxAmount = taxTotal
	transaction.TotalAmount = subTotal + taxTotal - req.DiscountAmount
	transaction.TenderAmount = totalPaid
	transaction.ChangeAmount = totalPaid - transaction.TotalAmount

	if err := tx.Model(&transaction).Updates(map[string]interface{}{
		"sub_total":     transaction.SubTotal,
		"tax_amount":    transaction.TaxAmount,
		"total_amount":  transaction.TotalAmount,
		"tender_amount": transaction.TenderAmount,
		"change_amount": transaction.ChangeAmount,
	}).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update transaction totals"})
		return
	}

	tx.Commit()

	// Reload with relationships
	h.DB.Where("id = ?", transaction.ID).
		Preload("Customer").
		Preload("Cashier").
		Preload("Items.Product").
		Preload("Items.Service").
		Preload("Payments").
		First(&transaction)

	c.JSON(http.StatusCreated, gin.H{"transaction": transaction})
}

func (h *Handler) GetPOSTransactions(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var transactions []models.POSTransaction
	query := h.DB.Where("organization_id = ?", orgID).
		Preload("Customer").
		Preload("Cashier").
		Preload("Items").
		Preload("Payments").
		Order("created_at DESC")

	// Filter by date range
	if startDate := c.Query("start_date"); startDate != "" {
		query = query.Where("created_at >= ?", startDate)
	}
	if endDate := c.Query("end_date"); endDate != "" {
		query = query.Where("created_at <= ?", endDate)
	}

	// Filter by cashier
	if cashierID := c.Query("cashier_id"); cashierID != "" {
		query = query.Where("cashier_id = ?", cashierID)
	}

	// Filter by customer
	if customerID := c.Query("customer_id"); customerID != "" {
		query = query.Where("customer_id = ?", customerID)
	}

	// Filter by status
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	// Pagination
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset := (page - 1) * limit

	query = query.Offset(offset).Limit(limit)

	if err := query.Find(&transactions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch transactions"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"transactions": transactions})
}

func (h *Handler) GetPOSTransaction(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	transactionID := c.Param("id")

	var transaction models.POSTransaction
	if err := h.DB.Where("id = ? AND organization_id = ?", transactionID, orgID).
		Preload("Customer").
		Preload("Cashier").
		Preload("Items.Product").
		Preload("Items.Service").
		Preload("Payments").
		First(&transaction).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Transaction not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"transaction": transaction})
}

func (h *Handler) VoidPOSTransaction(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	transactionID := c.Param("id")

	var req struct {
		Reason string `json:"reason" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Start transaction
	tx := h.DB.Begin()

	var transaction models.POSTransaction
	if err := tx.Where("id = ? AND organization_id = ?", transactionID, orgID).
		Preload("Items").
		First(&transaction).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusNotFound, gin.H{"error": "Transaction not found"})
		return
	}

	if transaction.Status == "voided" {
		tx.Rollback()
		c.JSON(http.StatusBadRequest, gin.H{"error": "Transaction already voided"})
		return
	}

	// Restore product stock for voided items
	for _, item := range transaction.Items {
		if item.ProductID != nil {
			var product models.Product
			if err := tx.Where("id = ?", *item.ProductID).First(&product).Error; err == nil {
				newStock := product.CurrentStock + item.Quantity
				tx.Model(&product).Update("current_stock", newStock)

				// Create inventory movement for void
				movement := models.InventoryMovement{
					OrganizationID:   orgID,
					ProductID:        *item.ProductID,
					MovementType:     "in",
					Quantity:         item.Quantity,
					PreviousQuantity: product.CurrentStock,
					NewQuantity:      newStock,
					UnitCost:         product.CostPrice,
					Reference:        transaction.TransactionNumber,
					ReferenceType:    "void",
					Notes:            "Transaction voided: " + req.Reason,
					CreatedBy:        c.GetString("user_id"),
				}
				tx.Create(&movement)
			}
		}
	}

	// Update transaction status
	if err := tx.Model(&transaction).Updates(map[string]interface{}{
		"status": "voided",
		"notes":  transaction.Notes + " | VOIDED: " + req.Reason,
	}).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to void transaction"})
		return
	}

	tx.Commit()

	c.JSON(http.StatusOK, gin.H{"message": "Transaction voided successfully"})
}

// Cash Drawer handlers

func (h *Handler) OpenCashDrawer(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var req struct {
		TerminalID    string  `json:"terminal_id" binding:"required"`
		OpeningAmount float64 `json:"opening_amount" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check if drawer is already open for this terminal
	var existingDrawer models.CashDrawer
	if err := h.DB.Where("terminal_id = ? AND status = ?", req.TerminalID, "open").First(&existingDrawer).Error; err == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cash drawer already open for this terminal"})
		return
	}

	drawer := models.CashDrawer{
		OrganizationID: orgID,
		TerminalID:     req.TerminalID,
		OpenedBy:       c.GetString("user_id"),
		OpeningAmount:  req.OpeningAmount,
		Status:         "open",
		OpenedAt:       time.Now(),
	}

	if err := h.DB.Create(&drawer).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open cash drawer"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"cash_drawer": drawer})
}

func (h *Handler) CloseCashDrawer(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	drawerID := c.Param("id")

	var req struct {
		ClosingAmount float64 `json:"closing_amount" binding:"required"`
		Notes         string  `json:"notes"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var drawer models.CashDrawer
	if err := h.DB.Where("id = ? AND organization_id = ?", drawerID, orgID).First(&drawer).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Cash drawer not found"})
		return
	}

	if drawer.Status == "closed" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cash drawer already closed"})
		return
	}

	// Calculate expected amount from transactions
	var expectedAmount float64
	h.DB.Model(&models.POSPayment{}).
		Joins("JOIN pos_transactions ON pos_payments.transaction_id = pos_transactions.id").
		Where("pos_transactions.organization_id = ? AND pos_payments.method = ? AND pos_transactions.created_at >= ?",
			orgID, "cash", drawer.OpenedAt).
		Select("SUM(pos_payments.amount)").
		Row().Scan(&expectedAmount)

	expectedAmount += drawer.OpeningAmount

	now := time.Now()
	updates := map[string]interface{}{
		"closed_by":       c.GetString("user_id"),
		"closing_amount":  req.ClosingAmount,
		"expected_amount": expectedAmount,
		"variance":        req.ClosingAmount - expectedAmount,
		"status":         "closed",
		"closed_at":      &now,
		"notes":          req.Notes,
	}

	if err := h.DB.Model(&drawer).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to close cash drawer"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"cash_drawer": drawer})
}

func (h *Handler) GetCashDrawers(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var drawers []models.CashDrawer
	query := h.DB.Where("organization_id = ?", orgID).
		Preload("OpenedByUser").
		Preload("ClosedByUser").
		Order("opened_at DESC")

	// Filter by status
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	// Filter by terminal
	if terminalID := c.Query("terminal_id"); terminalID != "" {
		query = query.Where("terminal_id = ?", terminalID)
	}

	if err := query.Find(&drawers).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch cash drawers"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"cash_drawers": drawers})
}

// Discount handlers

func (h *Handler) GetDiscounts(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var discounts []models.Discount
	query := h.DB.Where("organization_id = ?", orgID)

	// Filter by active status
	if isActive := c.Query("is_active"); isActive != "" {
		if active, err := strconv.ParseBool(isActive); err == nil {
			query = query.Where("is_active = ?", active)
		}
	}

	if err := query.Find(&discounts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch discounts"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"discounts": discounts})
}

func (h *Handler) CreateDiscount(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var discount models.Discount
	if err := c.ShouldBindJSON(&discount); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	discount.OrganizationID = orgID

	if err := h.DB.Create(&discount).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create discount"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"discount": discount})
}

// Tax Rate handlers

func (h *Handler) GetTaxRates(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var taxRates []models.TaxRate
	if err := h.DB.Where("organization_id = ?", orgID).Find(&taxRates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch tax rates"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"tax_rates": taxRates})
}

func (h *Handler) CreateTaxRate(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var taxRate models.TaxRate
	if err := c.ShouldBindJSON(&taxRate); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	taxRate.OrganizationID = orgID

	if err := h.DB.Create(&taxRate).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create tax rate"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"tax_rate": taxRate})
}

// POS Reports

func (h *Handler) GetPOSReport(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var report struct {
		TotalTransactions int64                   `json:"total_transactions"`
		TotalSales        float64                `json:"total_sales"`
		TotalTax          float64                `json:"total_tax"`
		AverageTransaction float64               `json:"average_transaction"`
		TopProducts       []map[string]interface{} `json:"top_products"`
		SalesByPaymentMethod []map[string]interface{} `json:"sales_by_payment_method"`
		HourlySales       []map[string]interface{} `json:"hourly_sales"`
	}

	// Date range
	startDate := c.DefaultQuery("start_date", time.Now().Format("2006-01-02"))
	endDate := c.DefaultQuery("end_date", time.Now().Format("2006-01-02"))

	// Total transactions and sales
	h.DB.Model(&models.POSTransaction{}).
		Where("organization_id = ? AND DATE(created_at) BETWEEN ? AND ? AND status = ?",
			orgID, startDate, endDate, "completed").
		Count(&report.TotalTransactions)

	h.DB.Model(&models.POSTransaction{}).
		Where("organization_id = ? AND DATE(created_at) BETWEEN ? AND ? AND status = ?",
			orgID, startDate, endDate, "completed").
		Select("SUM(total_amount), SUM(tax_amount)").
		Row().Scan(&report.TotalSales, &report.TotalTax)

	if report.TotalTransactions > 0 {
		report.AverageTransaction = report.TotalSales / float64(report.TotalTransactions)
	}

	// Top products
	h.DB.Table("pos_items").
		Select("pos_items.item_name, SUM(pos_items.quantity) as total_quantity, SUM(pos_items.total_price) as total_sales").
		Joins("JOIN pos_transactions ON pos_items.transaction_id = pos_transactions.id").
		Where("pos_transactions.organization_id = ? AND DATE(pos_transactions.created_at) BETWEEN ? AND ?",
			orgID, startDate, endDate).
		Group("pos_items.item_name").
		Order("total_sales DESC").
		Limit(10).
		Scan(&report.TopProducts)

	// Sales by payment method
	h.DB.Table("pos_payments").
		Select("pos_payments.method, SUM(pos_payments.amount) as total_amount").
		Joins("JOIN pos_transactions ON pos_payments.transaction_id = pos_transactions.id").
		Where("pos_transactions.organization_id = ? AND DATE(pos_transactions.created_at) BETWEEN ? AND ?",
			orgID, startDate, endDate).
		Group("pos_payments.method").
		Scan(&report.SalesByPaymentMethod)

	// Hourly sales
	h.DB.Table("pos_transactions").
		Select("EXTRACT(HOUR FROM created_at) as hour, SUM(total_amount) as total_sales").
		Where("organization_id = ? AND DATE(created_at) BETWEEN ? AND ? AND status = ?",
			orgID, startDate, endDate, "completed").
		Group("EXTRACT(HOUR FROM created_at)").
		Order("hour").
		Scan(&report.HourlySales)

	c.JSON(http.StatusOK, gin.H{"report": report})
}

// Helper functions

func parseDate(dateStr string) (time.Time, error) {
	return time.Parse("2006-01-02", dateStr)
}

func getCurrentTime() time.Time {
	return time.Now()
}