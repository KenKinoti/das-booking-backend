package handlers

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func posRound(v float64) float64 { return math.Round(v*100) / 100 }

// posOrg returns the caller's organisation id or writes a 400 and returns false.
func (h *Handler) posOrg(c *gin.Context) (string, bool) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return "", false
	}
	return orgID, true
}

var posPaymentMethods = map[string]bool{"cash": true, "card": true, "eftpos": true, "bank_transfer": true, "voucher": true, "other": true}

// posDayBounds turns YYYY-MM-DD query values into an inclusive [start, end) range.
func posDayBounds(c *gin.Context) (*time.Time, *time.Time) {
	var from, to *time.Time
	if s := c.Query("start_date"); s != "" {
		if t, err := time.ParseInLocation("2006-01-02", s[:min(len(s), 10)], time.Local); err == nil {
			from = &t
		}
	}
	if s := c.Query("end_date"); s != "" {
		if t, err := time.ParseInLocation("2006-01-02", s[:min(len(s), 10)], time.Local); err == nil {
			t = t.AddDate(0, 0, 1)
			to = &t
		}
	}
	return from, to
}

func posPreloadTx(db *gorm.DB) *gorm.DB {
	return db.Preload("Customer").
		Preload("Cashier").
		Preload("Items", func(db *gorm.DB) *gorm.DB { return db.Order("created_at ASC") }).
		Preload("Payments", func(db *gorm.DB) *gorm.DB { return db.Order("created_at ASC") })
}

type posHTTPError struct {
	status int
	msg    string
}

func (e posHTTPError) Error() string { return e.msg }

// ---------------------------------------------------------------------------
// Transactions
// ---------------------------------------------------------------------------

type posItemInput struct {
	ProductID       *string  `json:"product_id"`
	ServiceID       *string  `json:"service_id"`
	ItemName        string   `json:"item_name"`
	UnitPrice       *float64 `json:"unit_price"` // only used for custom (non-catalogue) lines
	Quantity        int      `json:"quantity"`
	DiscountPercent float64  `json:"discount_percent"`
	DiscountAmount  float64  `json:"discount_amount"`
	TaxRate         *float64 `json:"tax_rate"`
}

type posPaymentInput struct {
	Method    string  `json:"method"`
	Amount    float64 `json:"amount"`
	Reference string  `json:"reference"`
}

func (h *Handler) CreatePOSTransaction(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	userID := c.GetString("user_id")

	var req struct {
		CustomerID      *string           `json:"customer_id"`
		CashDrawerID    *string           `json:"cash_drawer_id"`
		Items           []posItemInput    `json:"items"`
		Payments        []posPaymentInput `json:"payments"`
		DiscountAmount  float64           `json:"discount_amount"`
		DiscountPercent float64           `json:"discount_percent"`
		TaxRate         *float64          `json:"tax_rate"` // default rate for lines that don't specify one
		TaxInclusive    bool              `json:"tax_inclusive"`
		Notes           string            `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	if len(req.Items) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Add at least one item to the sale"})
		return
	}
	if len(req.Payments) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "At least one payment is required"})
		return
	}
	if req.DiscountAmount < 0 || req.DiscountPercent < 0 || req.DiscountPercent > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid discount"})
		return
	}
	if req.CustomerID != nil && strings.TrimSpace(*req.CustomerID) == "" {
		req.CustomerID = nil
	}
	if req.CashDrawerID != nil && strings.TrimSpace(*req.CashDrawerID) == "" {
		req.CashDrawerID = nil
	}

	txnID := uuid.New().String()
	var txn models.POSTransaction

	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if req.CustomerID != nil {
			var n int64
			tx.Model(&models.Customer{}).Where("id = ? AND organization_id = ?", *req.CustomerID, orgID).Count(&n)
			if n == 0 {
				return posHTTPError{http.StatusBadRequest, "Customer not found"}
			}
		}
		if req.CashDrawerID != nil {
			var n int64
			tx.Model(&models.CashDrawer{}).Where("id = ? AND organization_id = ? AND status = ?", *req.CashDrawerID, orgID, "open").Count(&n)
			if n == 0 {
				req.CashDrawerID = nil // drawer closed meanwhile; still record the sale
			}
		}

		items := make([]models.POSItem, 0, len(req.Items))
		needed := map[string]int{}
		products := map[string]models.Product{}

		for i, in := range req.Items {
			if in.Quantity <= 0 {
				return posHTTPError{http.StatusBadRequest, fmt.Sprintf("Line %d: quantity must be at least 1", i+1)}
			}
			item := models.POSItem{TransactionID: txnID, Quantity: in.Quantity}
			switch {
			case in.ProductID != nil && *in.ProductID != "":
				p, seen := products[*in.ProductID]
				if !seen {
					if err := tx.Where("id = ? AND organization_id = ?", *in.ProductID, orgID).First(&p).Error; err != nil {
						return posHTTPError{http.StatusBadRequest, "Product not found"}
					}
					if !p.IsActive {
						return posHTTPError{http.StatusBadRequest, p.Name + " is inactive and can't be sold"}
					}
					products[p.ID] = p
				}
				needed[p.ID] += in.Quantity
				pid := p.ID
				item.ProductID = &pid
				item.ItemName = p.Name
				item.SKU = p.SKU
				item.ItemType = "product"
				item.UnitPrice = p.SellingPrice
			case in.ServiceID != nil && *in.ServiceID != "":
				var s models.Service
				if err := tx.Where("id = ? AND organization_id = ?", *in.ServiceID, orgID).First(&s).Error; err != nil {
					return posHTTPError{http.StatusBadRequest, "Service not found"}
				}
				sid := s.ID
				item.ServiceID = &sid
				item.ItemName = s.Name
				item.ItemType = "service"
				item.UnitPrice = s.Price
			default:
				name := strings.TrimSpace(in.ItemName)
				if name == "" || in.UnitPrice == nil || *in.UnitPrice < 0 {
					return posHTTPError{http.StatusBadRequest, fmt.Sprintf("Line %d: choose a product or enter a name and price", i+1)}
				}
				item.ItemName = name
				item.ItemType = "custom"
				item.UnitPrice = posRound(*in.UnitPrice)
			}

			gross := posRound(float64(item.Quantity) * item.UnitPrice)
			disc := in.DiscountAmount
			if in.DiscountPercent > 0 && in.DiscountPercent <= 100 {
				item.DiscountPercent = in.DiscountPercent
				disc = gross * in.DiscountPercent / 100
			}
			disc = posRound(math.Max(0, math.Min(disc, gross)))
			item.DiscountAmount = disc
			item.TotalPrice = posRound(gross - disc)

			rate := 0.0
			if in.TaxRate != nil {
				rate = *in.TaxRate
			} else if req.TaxRate != nil {
				rate = *req.TaxRate
			}
			if rate < 0 || rate > 100 {
				return posHTTPError{http.StatusBadRequest, "Invalid tax rate"}
			}
			item.TaxRate = rate
			items = append(items, item)
		}

		// Stock check (aggregated per product so duplicate lines can't oversell)
		for pid, qty := range needed {
			p := products[pid]
			if p.CurrentStock < qty {
				if p.CurrentStock <= 0 {
					return posHTTPError{http.StatusConflict, p.Name + " is out of stock"}
				}
				return posHTTPError{http.StatusConflict, fmt.Sprintf("Only %d × %s in stock", p.CurrentStock, p.Name)}
			}
		}

		// Totals. sub_total = sum of line totals (after line discounts).
		var subTotal float64
		for _, it := range items {
			subTotal += it.TotalPrice
		}
		subTotal = posRound(subTotal)
		discount := req.DiscountAmount
		if req.DiscountPercent > 0 {
			discount = subTotal * req.DiscountPercent / 100
		}
		discount = posRound(math.Max(0, math.Min(discount, subTotal)))
		factor := 1.0
		if subTotal > 0 {
			factor = (subTotal - discount) / subTotal
		}
		var taxTotal float64
		for i := range items {
			base := items[i].TotalPrice * factor
			if req.TaxInclusive {
				items[i].TaxAmount = posRound(base * items[i].TaxRate / (100 + items[i].TaxRate))
			} else {
				items[i].TaxAmount = posRound(base * items[i].TaxRate / 100)
			}
			taxTotal += items[i].TaxAmount
		}
		taxTotal = posRound(taxTotal)
		total := posRound(subTotal - discount)
		if !req.TaxInclusive {
			total = posRound(total + taxTotal)
		}

		// Payments
		var paid, cashPaid float64
		payments := make([]models.POSPayment, 0, len(req.Payments))
		now := time.Now()
		for _, pin := range req.Payments {
			m := strings.ToLower(strings.TrimSpace(pin.Method))
			if !posPaymentMethods[m] {
				return posHTTPError{http.StatusBadRequest, "Unsupported payment method: " + pin.Method}
			}
			if pin.Amount <= 0 {
				continue
			}
			amt := posRound(pin.Amount)
			paid += amt
			if m == "cash" {
				cashPaid += amt
			}
			payments = append(payments, models.POSPayment{
				TransactionID: txnID, Method: m, Amount: amt, Reference: strings.TrimSpace(pin.Reference),
				Status: "completed", ProcessedAt: &now,
			})
		}
		paid = posRound(paid)
		if len(payments) == 0 && total > 0 {
			return posHTTPError{http.StatusBadRequest, "Enter a payment amount"}
		}
		if paid+0.004 < total {
			return posHTTPError{http.StatusBadRequest, fmt.Sprintf("Payment is short by %.2f", total-paid)}
		}
		change := posRound(paid - total)
		if change > cashPaid+0.004 {
			return posHTTPError{http.StatusBadRequest, "Non-cash payments can't exceed the amount due"}
		}

		txn = models.POSTransaction{
			ID:             txnID,
			OrganizationID: orgID,
			CustomerID:     req.CustomerID,
			CashierID:      userID,
			CashDrawerID:   req.CashDrawerID,
			Type:           "sale",
			Status:         "completed",
			SubTotal:       subTotal,
			TaxAmount:      taxTotal,
			TaxInclusive:   req.TaxInclusive,
			DiscountAmount: discount,
			TotalAmount:    total,
			TenderAmount:   paid,
			ChangeAmount:   change,
			Notes:          strings.TrimSpace(req.Notes),
		}
		if err := tx.Omit("Customer", "Cashier", "Items", "Payments").Create(&txn).Error; err != nil {
			return err
		}
		for i := range items {
			if err := tx.Create(&items[i]).Error; err != nil {
				return err
			}
		}
		for i := range payments {
			if err := tx.Create(&payments[i]).Error; err != nil {
				return err
			}
		}

		// Decrement stock atomically and log movements
		for pid, qty := range needed {
			p := products[pid]
			res := tx.Model(&models.Product{}).
				Where("id = ? AND organization_id = ? AND current_stock >= ?", pid, orgID, qty).
				UpdateColumn("current_stock", gorm.Expr("current_stock - ?", qty))
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return posHTTPError{http.StatusConflict, "Not enough stock for " + p.Name}
			}
			mv := models.InventoryMovement{
				OrganizationID:   orgID,
				ProductID:        pid,
				MovementType:     "out",
				Quantity:         qty,
				PreviousQuantity: p.CurrentStock,
				NewQuantity:      p.CurrentStock - qty,
				UnitCost:         p.CostPrice,
				Reference:        txn.TransactionNumber,
				ReferenceType:    "sale",
				Notes:            "Sold via POS",
				CreatedBy:        userID,
			}
			if err := tx.Create(&mv).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		var he posHTTPError
		if errors.As(err, &he) {
			c.JSON(he.status, gin.H{"error": he.msg})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record sale"})
		return
	}

	posPreloadTx(h.DB).Where("id = ? AND organization_id = ?", txnID, orgID).First(&txn)
	c.JSON(http.StatusCreated, gin.H{"transaction": txn})
}

// posFilteredQuery applies the list filters shared by the table and its summary.
func (h *Handler) posFilteredQuery(c *gin.Context, orgID string, withStatus bool) *gorm.DB {
	q := h.DB.Model(&models.POSTransaction{}).Where("pos_transactions.organization_id = ?", orgID)
	from, to := posDayBounds(c)
	if from != nil {
		q = q.Where("pos_transactions.created_at >= ?", *from)
	}
	if to != nil {
		q = q.Where("pos_transactions.created_at < ?", *to)
	}
	if v := c.Query("cashier_id"); v != "" {
		q = q.Where("pos_transactions.cashier_id = ?", v)
	}
	if v := c.Query("customer_id"); v != "" {
		q = q.Where("pos_transactions.customer_id = ?", v)
	}
	if v := c.Query("cash_drawer_id"); v != "" {
		q = q.Where("pos_transactions.cash_drawer_id = ?", v)
	}
	switch m := c.Query("payment_method"); m {
	case "":
	case "split":
		q = q.Where("(SELECT COUNT(*) FROM pos_payments pp WHERE pp.transaction_id = pos_transactions.id) > 1")
	default:
		q = q.Where("EXISTS (SELECT 1 FROM pos_payments pp WHERE pp.transaction_id = pos_transactions.id AND pp.method = ?)", m)
	}
	if s := strings.TrimSpace(c.Query("search")); s != "" {
		like := "%" + s + "%"
		q = q.Where(`(pos_transactions.transaction_number ILIKE ? OR pos_transactions.notes ILIKE ?
			OR EXISTS (SELECT 1 FROM customers cu WHERE cu.id = pos_transactions.customer_id AND (cu.first_name || ' ' || cu.last_name ILIKE ? OR cu.email ILIKE ? OR cu.phone ILIKE ?))
			OR EXISTS (SELECT 1 FROM pos_items pi WHERE pi.transaction_id = pos_transactions.id AND (pi.item_name ILIKE ? OR pi.sku ILIKE ?)))`,
			like, like, like, like, like, like, like)
	}
	if withStatus {
		if v := c.Query("status"); v != "" && v != "all" {
			q = q.Where("pos_transactions.status = ?", v)
		}
	}
	return q
}

func (h *Handler) GetPOSTransactions(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "25"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 500 {
		limit = 25
	}

	var total int64
	if err := h.posFilteredQuery(c, orgID, true).Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch transactions"})
		return
	}

	transactions := []models.POSTransaction{}
	if err := posPreloadTx(h.posFilteredQuery(c, orgID, true)).
		Order("pos_transactions.created_at DESC").
		Offset((page - 1) * limit).Limit(limit).
		Find(&transactions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch transactions"})
		return
	}

	// Summary over the same filters, ignoring status
	var summary struct {
		SalesTotal   float64 `json:"sales_total"`
		SalesCount   int64   `json:"sales_count"`
		AverageSale  float64 `json:"average_sale"`
		TaxTotal     float64 `json:"tax_total"`
		VoidedCount  int64   `json:"voided_count"`
		VoidedAmount float64 `json:"voided_amount"`
	}
	row := h.posFilteredQuery(c, orgID, false).Select(`
		COALESCE(SUM(CASE WHEN pos_transactions.status = 'completed' THEN total_amount END), 0),
		COUNT(CASE WHEN pos_transactions.status = 'completed' THEN 1 END),
		COALESCE(SUM(CASE WHEN pos_transactions.status = 'completed' THEN tax_amount END), 0),
		COUNT(CASE WHEN pos_transactions.status = 'voided' THEN 1 END),
		COALESCE(SUM(CASE WHEN pos_transactions.status = 'voided' THEN total_amount END), 0)`).Row()
	if row != nil {
		_ = row.Scan(&summary.SalesTotal, &summary.SalesCount, &summary.TaxTotal, &summary.VoidedCount, &summary.VoidedAmount)
	}
	if summary.SalesCount > 0 {
		summary.AverageSale = posRound(summary.SalesTotal / float64(summary.SalesCount))
	}

	c.JSON(http.StatusOK, gin.H{
		"transactions": transactions,
		"total":        total,
		"page":         page,
		"limit":        limit,
		"summary":      summary,
	})
}

func (h *Handler) GetPOSTransaction(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	var transaction models.POSTransaction
	if err := posPreloadTx(h.DB).Where("id = ? AND organization_id = ?", c.Param("id"), orgID).
		First(&transaction).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Transaction not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"transaction": transaction})
}

func (h *Handler) VoidPOSTransaction(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	userID := c.GetString("user_id")
	var req struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "A reason is required to void a sale"})
		return
	}

	id := c.Param("id")
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		var t models.POSTransaction
		if err := tx.Preload("Items").Where("id = ? AND organization_id = ?", id, orgID).First(&t).Error; err != nil {
			return posHTTPError{http.StatusNotFound, "Transaction not found"}
		}
		if t.Status == "voided" {
			return posHTTPError{http.StatusConflict, "This sale has already been voided"}
		}
		// Conditional update guards against a concurrent double void
		now := time.Now()
		res := tx.Model(&models.POSTransaction{}).Where("id = ? AND status <> ?", t.ID, "voided").Updates(map[string]interface{}{
			"status":      "voided",
			"void_reason": req.Reason,
			"voided_at":   now,
			"voided_by":   userID,
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return posHTTPError{http.StatusConflict, "This sale has already been voided"}
		}

		restore := map[string]int{}
		for _, it := range t.Items {
			if it.ProductID != nil {
				restore[*it.ProductID] += it.Quantity
			}
		}
		for pid, qty := range restore {
			var p models.Product
			if err := tx.Where("id = ? AND organization_id = ?", pid, orgID).First(&p).Error; err != nil {
				continue // product deleted since; nothing to restore
			}
			if err := tx.Model(&models.Product{}).Where("id = ?", pid).
				UpdateColumn("current_stock", gorm.Expr("current_stock + ?", qty)).Error; err != nil {
				return err
			}
			mv := models.InventoryMovement{
				OrganizationID:   orgID,
				ProductID:        pid,
				MovementType:     "in",
				Quantity:         qty,
				PreviousQuantity: p.CurrentStock,
				NewQuantity:      p.CurrentStock + qty,
				UnitCost:         p.CostPrice,
				Reference:        t.TransactionNumber,
				ReferenceType:    "void",
				Notes:            "POS sale voided: " + req.Reason,
				CreatedBy:        userID,
			}
			if err := tx.Create(&mv).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		var he posHTTPError
		if errors.As(err, &he) {
			c.JSON(he.status, gin.H{"error": he.msg})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to void transaction"})
		return
	}

	var t models.POSTransaction
	posPreloadTx(h.DB).Where("id = ? AND organization_id = ?", id, orgID).First(&t)
	c.JSON(http.StatusOK, gin.H{"message": "Transaction voided", "transaction": t})
}

// ---------------------------------------------------------------------------
// Cash drawer
// ---------------------------------------------------------------------------

// posDrawerTakings fills the computed cash fields for a drawer.
func (h *Handler) posDrawerTakings(orgID string, d *models.CashDrawer) {
	end := time.Now()
	if d.ClosedAt != nil {
		end = *d.ClosedAt
	}
	// Sales linked to this drawer, or (legacy) completed sales in the open window without a drawer link.
	scope := h.DB.Model(&models.POSTransaction{}).
		Where("organization_id = ? AND status = ?", orgID, "completed").
		Where("(cash_drawer_id = ? OR (cash_drawer_id IS NULL AND created_at >= ? AND created_at <= ?))", d.ID, d.OpenedAt, end)

	var ids []string
	scope.Pluck("id", &ids)
	d.SalesCount = int64(len(ids))
	d.CashSales, d.CashChange = 0, 0
	if len(ids) > 0 {
		h.DB.Model(&models.POSPayment{}).Where("transaction_id IN ? AND method = ?", ids, "cash").
			Select("COALESCE(SUM(amount), 0)").Row().Scan(&d.CashSales)
		h.DB.Model(&models.POSTransaction{}).Where("id IN ?", ids).
			Select("COALESCE(SUM(change_amount), 0)").Row().Scan(&d.CashChange)
	}
	d.CashSales = posRound(d.CashSales)
	d.CashChange = posRound(d.CashChange)
	if d.Status == "open" {
		d.ExpectedAmount = posRound(d.OpeningAmount + d.CashSales - d.CashChange)
	}
}

func (h *Handler) OpenCashDrawer(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	var req struct {
		TerminalID    string   `json:"terminal_id"`
		OpeningAmount *float64 `json:"opening_amount"`
		Notes         string   `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	if req.OpeningAmount == nil || *req.OpeningAmount < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter the opening float (0 or more)"})
		return
	}
	req.TerminalID = strings.TrimSpace(req.TerminalID)
	if req.TerminalID == "" {
		req.TerminalID = "main"
	}

	var n int64
	h.DB.Model(&models.CashDrawer{}).Where("organization_id = ? AND terminal_id = ? AND status = ?", orgID, req.TerminalID, "open").Count(&n)
	if n > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "A cash drawer is already open for this register"})
		return
	}

	drawer := models.CashDrawer{
		OrganizationID: orgID,
		TerminalID:     req.TerminalID,
		OpenedBy:       c.GetString("user_id"),
		OpeningAmount:  posRound(*req.OpeningAmount),
		ExpectedAmount: posRound(*req.OpeningAmount),
		Status:         "open",
		OpenedAt:       time.Now(),
		Notes:          strings.TrimSpace(req.Notes),
	}
	if err := h.DB.Omit("OpenedByUser", "ClosedByUser").Create(&drawer).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open cash drawer"})
		return
	}
	h.DB.Preload("OpenedByUser").First(&drawer, "id = ?", drawer.ID)
	c.JSON(http.StatusCreated, gin.H{"cash_drawer": drawer})
}

func (h *Handler) CloseCashDrawer(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	var req struct {
		ClosingAmount *float64 `json:"closing_amount"`
		Notes         string   `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	if req.ClosingAmount == nil || *req.ClosingAmount < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter the counted cash amount"})
		return
	}

	var drawer models.CashDrawer
	if err := h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), orgID).First(&drawer).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Cash drawer not found"})
		return
	}
	if drawer.Status == "closed" {
		c.JSON(http.StatusConflict, gin.H{"error": "Cash drawer already closed"})
		return
	}

	h.posDrawerTakings(orgID, &drawer)
	expected := drawer.ExpectedAmount
	now := time.Now()
	userID := c.GetString("user_id")
	counted := posRound(*req.ClosingAmount)
	notes := strings.TrimSpace(req.Notes)
	if drawer.Notes != "" && notes != "" {
		notes = drawer.Notes + "\n" + notes
	} else if notes == "" {
		notes = drawer.Notes
	}
	if err := h.DB.Model(&models.CashDrawer{}).Where("id = ?", drawer.ID).Updates(map[string]interface{}{
		"closed_by":       userID,
		"closing_amount":  counted,
		"expected_amount": expected,
		"variance":        posRound(counted - expected),
		"status":          "closed",
		"closed_at":       now,
		"notes":           notes,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to close cash drawer"})
		return
	}
	var out models.CashDrawer
	h.DB.Preload("OpenedByUser").Preload("ClosedByUser").First(&out, "id = ?", drawer.ID)
	h.posDrawerTakings(orgID, &out)
	c.JSON(http.StatusOK, gin.H{"cash_drawer": out})
}

func (h *Handler) GetCashDrawers(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	drawers := []models.CashDrawer{}
	query := h.DB.Where("organization_id = ?", orgID).
		Preload("OpenedByUser").
		Preload("ClosedByUser").
		Order("opened_at DESC")
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	if terminalID := c.Query("terminal_id"); terminalID != "" {
		query = query.Where("terminal_id = ?", terminalID)
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit < 1 || limit > 500 {
		limit = 50
	}
	if err := query.Limit(limit).Find(&drawers).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch cash drawers"})
		return
	}
	for i := range drawers {
		h.posDrawerTakings(orgID, &drawers[i])
	}
	c.JSON(http.StatusOK, gin.H{"cash_drawers": drawers})
}

// ---------------------------------------------------------------------------
// Discounts
// ---------------------------------------------------------------------------

func (h *Handler) GetDiscounts(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	discounts := []models.Discount{}
	query := h.DB.Where("organization_id = ?", orgID).Order("name ASC")
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
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	var discount models.Discount
	if err := c.ShouldBindJSON(&discount); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	discount.Name = strings.TrimSpace(discount.Name)
	if discount.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Discount name is required"})
		return
	}
	if discount.Type == "fixed" {
		discount.Type = "fixed_amount"
	}
	if discount.Type != "percentage" && discount.Type != "fixed_amount" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Discount type must be percentage or fixed_amount"})
		return
	}
	if discount.Value <= 0 || (discount.Type == "percentage" && discount.Value > 100) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter a valid discount value"})
		return
	}
	discount.ID = ""
	discount.OrganizationID = orgID
	if discount.StartDate.IsZero() {
		discount.StartDate = time.Now()
	}
	discount.IsActive = true
	if err := h.DB.Create(&discount).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create discount"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"discount": discount})
}

// ---------------------------------------------------------------------------
// Tax rates
// ---------------------------------------------------------------------------

func (h *Handler) GetTaxRates(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	taxRates := []models.TaxRate{}
	if err := h.DB.Where("organization_id = ?", orgID).Order("is_default DESC, name ASC").Find(&taxRates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch tax rates"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tax_rates": taxRates})
}

func (h *Handler) CreateTaxRate(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	var req struct {
		Name      string  `json:"name"`
		Rate      float64 `json:"rate"`
		IsDefault bool    `json:"is_default"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || req.Rate < 0 || req.Rate > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter a name and a rate between 0 and 100"})
		return
	}
	taxRate := models.TaxRate{OrganizationID: orgID, Name: req.Name, Rate: req.Rate, IsDefault: req.IsDefault, IsActive: true}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if req.IsDefault {
			if err := tx.Model(&models.TaxRate{}).Where("organization_id = ?", orgID).Update("is_default", false).Error; err != nil {
				return err
			}
		}
		return tx.Create(&taxRate).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create tax rate"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"tax_rate": taxRate})
}

// ---------------------------------------------------------------------------
// Reports
// ---------------------------------------------------------------------------

func (h *Handler) GetPOSReport(c *gin.Context) {
	orgID, ok := h.posOrg(c)
	if !ok {
		return
	}
	type report struct {
		TotalTransactions    int64                    `json:"total_transactions"`
		TotalSales           float64                  `json:"total_sales"`
		TotalTax             float64                  `json:"total_tax"`
		TotalDiscounts       float64                  `json:"total_discounts"`
		AverageTransaction   float64                  `json:"average_transaction"`
		VoidedTransactions   int64                    `json:"voided_transactions"`
		VoidedAmount         float64                  `json:"voided_amount"`
		TopProducts          []map[string]interface{} `json:"top_products"`
		SalesByPaymentMethod []map[string]interface{} `json:"sales_by_payment_method"`
		HourlySales          []map[string]interface{} `json:"hourly_sales"`
	}
	r := report{TopProducts: []map[string]interface{}{}, SalesByPaymentMethod: []map[string]interface{}{}, HourlySales: []map[string]interface{}{}}

	today := time.Now().Format("2006-01-02")
	start, err1 := time.ParseInLocation("2006-01-02", c.DefaultQuery("start_date", today), time.Local)
	end, err2 := time.ParseInLocation("2006-01-02", c.DefaultQuery("end_date", today), time.Local)
	if err1 != nil || err2 != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Dates must be YYYY-MM-DD"})
		return
	}
	end = end.AddDate(0, 0, 1)

	base := func() *gorm.DB {
		return h.DB.Model(&models.POSTransaction{}).
			Where("pos_transactions.organization_id = ? AND pos_transactions.created_at >= ? AND pos_transactions.created_at < ? AND pos_transactions.deleted_at IS NULL", orgID, start, end)
	}

	base().Where("status = ?", "completed").
		Select("COUNT(*), COALESCE(SUM(total_amount),0), COALESCE(SUM(tax_amount),0), COALESCE(SUM(discount_amount),0)").
		Row().Scan(&r.TotalTransactions, &r.TotalSales, &r.TotalTax, &r.TotalDiscounts)
	base().Where("status = ?", "voided").
		Select("COUNT(*), COALESCE(SUM(total_amount),0)").Row().Scan(&r.VoidedTransactions, &r.VoidedAmount)
	if r.TotalTransactions > 0 {
		r.AverageTransaction = posRound(r.TotalSales / float64(r.TotalTransactions))
	}

	base().Where("pos_transactions.status = ?", "completed").
		Joins("JOIN pos_items ON pos_items.transaction_id = pos_transactions.id").
		Select("pos_items.item_name, SUM(pos_items.quantity) AS total_quantity, SUM(pos_items.total_price) AS total_sales").
		Group("pos_items.item_name").Order("total_sales DESC").Limit(10).
		Scan(&r.TopProducts)

	base().Where("pos_transactions.status = ?", "completed").
		Joins("JOIN pos_payments ON pos_payments.transaction_id = pos_transactions.id").
		Select("pos_payments.method, SUM(pos_payments.amount) AS total_amount, COUNT(*) AS count").
		Group("pos_payments.method").Order("total_amount DESC").
		Scan(&r.SalesByPaymentMethod)

	base().Where("status = ?", "completed").
		Select("EXTRACT(HOUR FROM created_at) AS hour, SUM(total_amount) AS total_sales, COUNT(*) AS count").
		Group("EXTRACT(HOUR FROM created_at)").Order("hour").
		Scan(&r.HourlySales)

	c.JSON(http.StatusOK, gin.H{"report": r})
}
