package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Supplier handlers

func (h *Handler) GetSuppliers(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var suppliers []models.Supplier
	query := h.DB.Where("organization_id = ?", orgID)

	// Filter by active status
	if isActive := c.Query("is_active"); isActive != "" {
		if active, err := strconv.ParseBool(isActive); err == nil {
			query = query.Where("is_active = ?", active)
		}
	}

	// Search by name or email
	if search := c.Query("search"); search != "" {
		query = query.Where("name ILIKE ? OR email ILIKE ?", "%"+search+"%", "%"+search+"%")
	}

	if err := query.Find(&suppliers).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch suppliers"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"suppliers": suppliers})
}

func (h *Handler) GetSupplier(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	supplierID := c.Param("id")

	var supplier models.Supplier
	if err := h.DB.Where("id = ? AND organization_id = ?", supplierID, orgID).
		Preload("PurchaseOrders").
		First(&supplier).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Supplier not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"supplier": supplier})
}

func (h *Handler) CreateSupplier(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var supplier models.Supplier
	if err := c.ShouldBindJSON(&supplier); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	supplier.OrganizationID = orgID

	if err := h.DB.Create(&supplier).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create supplier"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"supplier": supplier})
}

func (h *Handler) UpdateSupplier(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	supplierID := c.Param("id")

	var supplier models.Supplier
	if err := h.DB.Where("id = ? AND organization_id = ?", supplierID, orgID).First(&supplier).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Supplier not found"})
		return
	}

	var updateData models.Supplier
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Prevent changing organization ID
	updateData.OrganizationID = orgID
	updateData.ID = supplierID

	if err := h.DB.Model(&supplier).Updates(updateData).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update supplier"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"supplier": supplier})
}

func (h *Handler) DeleteSupplier(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	supplierID := c.Param("id")

	// Check if supplier has any purchase orders
	var count int64
	h.DB.Model(&models.PurchaseOrder{}).Where("supplier_id = ?", supplierID).Count(&count)
	if count > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot delete supplier with existing purchase orders"})
		return
	}

	if err := h.DB.Where("id = ? AND organization_id = ?", supplierID, orgID).Delete(&models.Supplier{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete supplier"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Supplier deleted successfully"})
}

// Purchase Order handlers

func (h *Handler) GetPurchaseOrders(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var orders []models.PurchaseOrder
	query := h.DB.Where("organization_id = ?", orgID).
		Preload("Supplier").
		Preload("Creator").
		Preload("Items.Product").
		Order("created_at DESC")

	// Filter by status
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	// Filter by supplier
	if supplierID := c.Query("supplier_id"); supplierID != "" {
		query = query.Where("supplier_id = ?", supplierID)
	}

	if err := query.Find(&orders).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch purchase orders"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"purchase_orders": orders})
}

func (h *Handler) GetPurchaseOrder(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	orderID := c.Param("id")

	var order models.PurchaseOrder
	if err := h.DB.Where("id = ? AND organization_id = ?", orderID, orgID).
		Preload("Supplier").
		Preload("Creator").
		Preload("Items.Product").
		First(&order).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Purchase order not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"purchase_order": order})
}

func (h *Handler) CreatePurchaseOrder(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var req struct {
		SupplierID   string                        `json:"supplier_id" binding:"required"`
		OrderDate    string                        `json:"order_date" binding:"required"`
		ExpectedDate *string                       `json:"expected_date"`
		Notes        string                        `json:"notes"`
		Items        []models.PurchaseOrderItem    `json:"items" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate supplier exists
	var supplier models.Supplier
	if err := h.DB.Where("id = ? AND organization_id = ?", req.SupplierID, orgID).First(&supplier).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Supplier not found"})
		return
	}

	// Create purchase order
	order := models.PurchaseOrder{
		OrganizationID: orgID,
		SupplierID:     req.SupplierID,
		Status:         "draft",
		Notes:          req.Notes,
		CreatedBy:      c.GetString("user_id"),
	}

	// Parse dates
	if orderDate, err := parseDate(req.OrderDate); err == nil {
		order.OrderDate = orderDate
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid order date format"})
		return
	}

	if req.ExpectedDate != nil {
		if expectedDate, err := parseDate(*req.ExpectedDate); err == nil {
			order.ExpectedDate = &expectedDate
		}
	}

	// Start transaction
	tx := h.DB.Begin()

	if err := tx.Create(&order).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create purchase order"})
		return
	}

	// Create order items and calculate totals
	var subTotal float64
	for _, item := range req.Items {
		item.PurchaseOrderID = order.ID
		if err := tx.Create(&item).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create order item"})
			return
		}
		subTotal += item.TotalCost
	}

	// Update order totals
	order.SubTotal = subTotal
	order.TotalAmount = subTotal + order.TaxAmount + order.ShippingCost

	if err := tx.Model(&order).Updates(map[string]interface{}{
		"sub_total":    order.SubTotal,
		"total_amount": order.TotalAmount,
	}).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update order totals"})
		return
	}

	tx.Commit()

	// Reload with relationships
	h.DB.Where("id = ?", order.ID).
		Preload("Supplier").
		Preload("Items.Product").
		First(&order)

	c.JSON(http.StatusCreated, gin.H{"purchase_order": order})
}

func (h *Handler) UpdatePurchaseOrderStatus(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	orderID := c.Param("id")

	var req struct {
		Status string `json:"status" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var order models.PurchaseOrder
	if err := h.DB.Where("id = ? AND organization_id = ?", orderID, orgID).First(&order).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Purchase order not found"})
		return
	}

	if err := h.DB.Model(&order).Update("status", req.Status).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update status"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Status updated successfully", "purchase_order": order})
}

func (h *Handler) ReceivePurchaseOrder(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	orderID := c.Param("id")

	var req struct {
		ReceivedItems []struct {
			ItemID           string `json:"item_id" binding:"required"`
			QuantityReceived int    `json:"quantity_received" binding:"required"`
		} `json:"received_items" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Start transaction
	tx := h.DB.Begin()

	var order models.PurchaseOrder
	if err := tx.Where("id = ? AND organization_id = ?", orderID, orgID).
		Preload("Items.Product").
		First(&order).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusNotFound, gin.H{"error": "Purchase order not found"})
		return
	}

	// Process received items
	for _, receivedItem := range req.ReceivedItems {
		var orderItem models.PurchaseOrderItem
		if err := tx.Where("id = ? AND purchase_order_id = ?", receivedItem.ItemID, orderID).
			Preload("Product").
			First(&orderItem).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusNotFound, gin.H{"error": "Order item not found"})
			return
		}

		// Update received quantity
		if err := tx.Model(&orderItem).Update("quantity_received", receivedItem.QuantityReceived).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update received quantity"})
			return
		}

		// Update product stock
		newStock := orderItem.Product.CurrentStock + receivedItem.QuantityReceived
		if err := tx.Model(&orderItem.Product).Update("current_stock", newStock).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update product stock"})
			return
		}

		// Create inventory movement
		movement := models.InventoryMovement{
			OrganizationID:   orgID,
			ProductID:        orderItem.ProductID,
			MovementType:     "in",
			Quantity:         receivedItem.QuantityReceived,
			PreviousQuantity: orderItem.Product.CurrentStock,
			NewQuantity:      newStock,
			UnitCost:         orderItem.UnitCost,
			Reference:        order.OrderNumber,
			ReferenceType:    "purchase_order",
			Notes:            "Received from purchase order",
			CreatedBy:        c.GetString("user_id"),
		}

		if err := tx.Create(&movement).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create inventory movement"})
			return
		}
	}

	// Update order status to received
	now := getCurrentTime()
	if err := tx.Model(&order).Updates(map[string]interface{}{
		"status":        "received",
		"received_date": &now,
	}).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update order status"})
		return
	}

	tx.Commit()

	c.JSON(http.StatusOK, gin.H{"message": "Purchase order received successfully"})
}

func (h *Handler) GetSupplierReport(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var report struct {
		TotalSuppliers   int64                   `json:"total_suppliers"`
		ActiveSuppliers  int64                   `json:"active_suppliers"`
		TotalPurchases   float64                `json:"total_purchases"`
		TopSuppliers     []map[string]interface{} `json:"top_suppliers"`
		RecentOrders     []models.PurchaseOrder  `json:"recent_orders"`
	}

	// Total and active suppliers
	h.DB.Model(&models.Supplier{}).Where("organization_id = ?", orgID).Count(&report.TotalSuppliers)
	h.DB.Model(&models.Supplier{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&report.ActiveSuppliers)

	// Total purchases
	h.DB.Model(&models.PurchaseOrder{}).
		Where("organization_id = ? AND status = ?", orgID, "received").
		Select("SUM(total_amount)").
		Row().Scan(&report.TotalPurchases)

	// Top suppliers by purchase amount
	h.DB.Table("purchase_orders").
		Select("suppliers.name as supplier_name, COUNT(*) as order_count, SUM(total_amount) as total_amount").
		Joins("JOIN suppliers ON purchase_orders.supplier_id = suppliers.id").
		Where("purchase_orders.organization_id = ?", orgID).
		Group("suppliers.id, suppliers.name").
		Order("total_amount DESC").
		Limit(5).
		Scan(&report.TopSuppliers)

	// Recent orders
	h.DB.Where("organization_id = ?", orgID).
		Preload("Supplier").
		Order("created_at DESC").
		Limit(10).
		Find(&report.RecentOrders)

	c.JSON(http.StatusOK, gin.H{"report": report})
}