package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Product handlers

func (h *Handler) GetProducts(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var products []models.Product
	query := h.DB.Where("organization_id = ?", orgID).Preload("Category").Preload("Brand")

	// Filter by category
	if categoryID := c.Query("category_id"); categoryID != "" {
		query = query.Where("category_id = ?", categoryID)
	}

	// Filter by active status
	if isActive := c.Query("is_active"); isActive != "" {
		if active, err := strconv.ParseBool(isActive); err == nil {
			query = query.Where("is_active = ?", active)
		}
	}

	// Search by name or SKU
	if search := c.Query("search"); search != "" {
		query = query.Where("name ILIKE ? OR sku ILIKE ?", "%"+search+"%", "%"+search+"%")
	}

	if err := query.Find(&products).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch products"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"products": products})
}

func (h *Handler) GetProduct(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	productID := c.Param("id")

	var product models.Product
	if err := h.DB.Where("id = ? AND organization_id = ?", productID, orgID).
		Preload("Category").
		Preload("Brand").
		Preload("InventoryItems").
		First(&product).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"product": product})
}

func (h *Handler) CreateProduct(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var product models.Product
	if err := c.ShouldBindJSON(&product); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	product.OrganizationID = orgID

	if err := h.DB.Create(&product).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create product"})
		return
	}

	// Create initial inventory entry if stock provided
	if product.CurrentStock > 0 {
		inventoryItem := models.InventoryItem{
			OrganizationID: orgID,
			ProductID:      product.ID,
			Quantity:       product.CurrentStock,
			UnitCost:       product.CostPrice,
			Status:         "available",
		}
		h.DB.Create(&inventoryItem)

		// Create inventory movement record
		movement := models.InventoryMovement{
			OrganizationID:   orgID,
			ProductID:        product.ID,
			MovementType:     "in",
			Quantity:         product.CurrentStock,
			PreviousQuantity: 0,
			NewQuantity:      product.CurrentStock,
			UnitCost:         product.CostPrice,
			Reference:        "Initial Stock",
			ReferenceType:    "adjustment",
			Notes:            "Initial stock entry",
			CreatedBy:        c.GetString("user_id"),
		}
		h.DB.Create(&movement)
	}

	c.JSON(http.StatusCreated, gin.H{"product": product})
}

func (h *Handler) UpdateProduct(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	productID := c.Param("id")

	var product models.Product
	if err := h.DB.Where("id = ? AND organization_id = ?", productID, orgID).First(&product).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}

	var updateData models.Product
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Prevent changing organization ID
	updateData.OrganizationID = orgID
	updateData.ID = productID

	if err := h.DB.Model(&product).Updates(updateData).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update product"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"product": product})
}

func (h *Handler) DeleteProduct(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	productID := c.Param("id")

	if err := h.DB.Where("id = ? AND organization_id = ?", productID, orgID).Delete(&models.Product{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete product"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Product deleted successfully"})
}

// Product Category handlers

func (h *Handler) GetProductCategories(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var categories []models.ProductCategory
	if err := h.DB.Where("organization_id = ?", orgID).Preload("Children").Find(&categories).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch categories"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"categories": categories})
}

func (h *Handler) CreateProductCategory(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var category models.ProductCategory
	if err := c.ShouldBindJSON(&category); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	category.OrganizationID = orgID

	if err := h.DB.Create(&category).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create category"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"category": category})
}

// Brand handlers

func (h *Handler) GetBrands(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var brands []models.Brand
	if err := h.DB.Where("organization_id = ?", orgID).Find(&brands).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch brands"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"brands": brands})
}

func (h *Handler) CreateBrand(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var brand models.Brand
	if err := c.ShouldBindJSON(&brand); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	brand.OrganizationID = orgID

	if err := h.DB.Create(&brand).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create brand"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"brand": brand})
}

// Inventory Item handlers

func (h *Handler) GetInventoryItems(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var items []models.InventoryItem
	query := h.DB.Where("organization_id = ?", orgID).Preload("Product").Preload("Location")

	// Filter by product
	if productID := c.Query("product_id"); productID != "" {
		query = query.Where("product_id = ?", productID)
	}

	// Filter by location
	if locationID := c.Query("location_id"); locationID != "" {
		query = query.Where("location_id = ?", locationID)
	}

	// Filter by status
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	if err := query.Find(&items).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch inventory items"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"inventory_items": items})
}

func (h *Handler) AdjustInventory(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var req struct {
		ProductID    string  `json:"product_id" binding:"required"`
		Quantity     int     `json:"quantity" binding:"required"`
		MovementType string  `json:"movement_type" binding:"required"` // in, out, adjustment
		UnitCost     float64 `json:"unit_cost"`
		Reference    string  `json:"reference"`
		Notes        string  `json:"notes"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get current product stock
	var product models.Product
	if err := h.DB.Where("id = ? AND organization_id = ?", req.ProductID, orgID).First(&product).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}

	previousQuantity := product.CurrentStock
	var newQuantity int

	switch req.MovementType {
	case "in":
		newQuantity = previousQuantity + req.Quantity
	case "out":
		newQuantity = previousQuantity - req.Quantity
		if newQuantity < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Insufficient stock"})
			return
		}
	case "adjustment":
		newQuantity = req.Quantity
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid movement type"})
		return
	}

	// Update product stock
	if err := h.DB.Model(&product).Update("current_stock", newQuantity).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update stock"})
		return
	}

	// Create inventory movement record
	movement := models.InventoryMovement{
		OrganizationID:   orgID,
		ProductID:        req.ProductID,
		MovementType:     req.MovementType,
		Quantity:         req.Quantity,
		PreviousQuantity: previousQuantity,
		NewQuantity:      newQuantity,
		UnitCost:         req.UnitCost,
		Reference:        req.Reference,
		ReferenceType:    "manual",
		Notes:            req.Notes,
		CreatedBy:        c.GetString("user_id"),
	}

	if err := h.DB.Create(&movement).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create movement record"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":           "Inventory adjusted successfully",
		"previous_quantity": previousQuantity,
		"new_quantity":      newQuantity,
		"movement":          movement,
	})
}

func (h *Handler) GetInventoryMovements(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var movements []models.InventoryMovement
	query := h.DB.Where("organization_id = ?", orgID).Preload("Product").Preload("Creator").Order("created_at DESC")

	// Filter by product
	if productID := c.Query("product_id"); productID != "" {
		query = query.Where("product_id = ?", productID)
	}

	// Filter by movement type
	if movementType := c.Query("movement_type"); movementType != "" {
		query = query.Where("movement_type = ?", movementType)
	}

	if err := query.Find(&movements).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch inventory movements"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"movements": movements})
}

// Inventory Location handlers

func (h *Handler) GetInventoryLocations(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var locations []models.InventoryLocation
	if err := h.DB.Where("organization_id = ?", orgID).Find(&locations).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch locations"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"locations": locations})
}

func (h *Handler) CreateInventoryLocation(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var location models.InventoryLocation
	if err := c.ShouldBindJSON(&location); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	location.OrganizationID = orgID

	if err := h.DB.Create(&location).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create location"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"location": location})
}

// Inventory reports

func (h *Handler) GetInventoryReport(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var report struct {
		TotalProducts     int64                   `json:"total_products"`
		LowStockProducts  []models.Product        `json:"low_stock_products"`
		OutOfStockProducts []models.Product       `json:"out_of_stock_products"`
		TotalStockValue   float64                `json:"total_stock_value"`
		CategoryBreakdown []map[string]interface{} `json:"category_breakdown"`
	}

	// Total products
	h.DB.Model(&models.Product{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&report.TotalProducts)

	// Low stock products (below reorder point)
	h.DB.Where("organization_id = ? AND current_stock <= reorder_point AND current_stock > 0", orgID).
		Preload("Category").Find(&report.LowStockProducts)

	// Out of stock products
	h.DB.Where("organization_id = ? AND current_stock = 0", orgID).
		Preload("Category").Find(&report.OutOfStockProducts)

	// Total stock value
	var products []models.Product
	h.DB.Where("organization_id = ?", orgID).Find(&products)
	for _, product := range products {
		report.TotalStockValue += float64(product.CurrentStock) * product.CostPrice
	}

	// Category breakdown
	h.DB.Table("products").
		Select("product_categories.name as category_name, COUNT(*) as product_count, SUM(current_stock * cost_price) as total_value").
		Joins("LEFT JOIN product_categories ON products.category_id = product_categories.id").
		Where("products.organization_id = ?", orgID).
		Group("product_categories.name").
		Scan(&report.CategoryBreakdown)

	c.JSON(http.StatusOK, gin.H{"report": report})
}