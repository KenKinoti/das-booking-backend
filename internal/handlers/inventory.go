package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Stock level classification shared by list filters and the report.
// A product is "low" when 0 < stock <= GREATEST(reorder_point, min_stock).
const invThresholdSQL = "GREATEST(products.reorder_point, products.min_stock)"

func invStockStatus(p models.Product) string {
	threshold := p.ReorderPoint
	if p.MinStock > threshold {
		threshold = p.MinStock
	}
	switch {
	case p.CurrentStock <= 0:
		return "out_of_stock"
	case threshold > 0 && p.CurrentStock <= threshold:
		return "low_stock"
	}
	return "in_stock"
}

type productView struct {
	models.Product
	StockStatus string  `json:"stock_status"`
	StockValue  float64 `json:"stock_value"`
}

func invView(p models.Product) productView {
	return productView{Product: p, StockStatus: invStockStatus(p), StockValue: erpRound(float64(p.CurrentStock) * p.CostPrice)}
}

var invSkuClean = regexp.MustCompile(`[^A-Z0-9-]+`)

// ---------------------------------------------------------------------------
// Products
// ---------------------------------------------------------------------------

type productInput struct {
	Name          *string  `json:"name"`
	Description   *string  `json:"description"`
	SKU           *string  `json:"sku"`
	Barcode       *string  `json:"barcode"`
	CategoryID    *string  `json:"category_id"`
	BrandID       *string  `json:"brand_id"`
	UnitOfMeasure *string  `json:"unit_of_measure"`
	CostPrice     *float64 `json:"cost_price"`
	SellingPrice  *float64 `json:"selling_price"`
	MinStock      *int     `json:"min_stock"`
	MaxStock      *int     `json:"max_stock"`
	ReorderPoint  *int     `json:"reorder_point"`
	CurrentStock  *int     `json:"current_stock"` // opening stock on create only
	IsActive      *bool    `json:"is_active"`
	ImageURL      *string  `json:"image_url"`
	Weight        *float64 `json:"weight"`
	Dimensions    *string  `json:"dimensions"`
}

func (h *Handler) applyProductInput(orgID string, in productInput, p *models.Product, creating bool) (map[string]interface{}, error) {
	cols := map[string]interface{}{}
	trim := func(s *string) string { return strings.TrimSpace(*s) }

	if in.Name != nil || creating {
		name := ""
		if in.Name != nil {
			name = trim(in.Name)
		}
		if name == "" {
			return nil, errors.New("Product name is required")
		}
		if len(name) > 255 {
			return nil, errors.New("Product name is too long")
		}
		p.Name, cols["name"] = name, name
	}
	if in.Description != nil {
		p.Description = trim(in.Description)
		cols["description"] = p.Description
	}
	if in.SKU != nil {
		sku := strings.ToUpper(trim(in.SKU))
		sku = strings.ReplaceAll(sku, " ", "-")
		if len(sku) > 100 {
			return nil, errors.New("SKU must be 100 characters or fewer")
		}
		if sku != "" || !creating {
			if sku == "" {
				return nil, errors.New("SKU cannot be empty")
			}
			p.SKU, cols["sku"] = sku, sku
		}
	}
	if in.Barcode != nil {
		b := trim(in.Barcode)
		if len(b) > 100 {
			return nil, errors.New("Barcode is too long")
		}
		p.Barcode, cols["barcode"] = b, b
	}
	if in.CategoryID != nil {
		id := trim(in.CategoryID)
		if id == "" {
			p.CategoryID, cols["category_id"] = nil, nil
		} else {
			var n int64
			h.DB.Model(&models.ProductCategory{}).Where("id = ? AND organization_id = ?", id, orgID).Count(&n)
			if n == 0 {
				return nil, errors.New("Category not found")
			}
			p.CategoryID, cols["category_id"] = &id, id
		}
	}
	if in.BrandID != nil {
		id := trim(in.BrandID)
		if id == "" {
			p.BrandID, cols["brand_id"] = nil, nil
		} else {
			var n int64
			h.DB.Model(&models.Brand{}).Where("id = ? AND organization_id = ?", id, orgID).Count(&n)
			if n == 0 {
				return nil, errors.New("Brand not found")
			}
			p.BrandID, cols["brand_id"] = &id, id
		}
	}
	if in.UnitOfMeasure != nil {
		u := trim(in.UnitOfMeasure)
		if u == "" {
			u = "each"
		}
		if len(u) > 20 {
			return nil, errors.New("Unit must be 20 characters or fewer")
		}
		p.UnitOfMeasure, cols["unit_of_measure"] = u, u
	} else if creating {
		p.UnitOfMeasure = "each"
	}
	money := func(v *float64, label, col string, dst *float64) error {
		if v == nil {
			return nil
		}
		if *v < 0 {
			return fmt.Errorf("%s cannot be negative", label)
		}
		if *v >= 1e8 {
			return fmt.Errorf("%s is too large", label)
		}
		*dst = erpRound(*v)
		cols[col] = *dst
		return nil
	}
	if err := money(in.CostPrice, "Cost price", "cost_price", &p.CostPrice); err != nil {
		return nil, err
	}
	if err := money(in.SellingPrice, "Selling price", "selling_price", &p.SellingPrice); err != nil {
		return nil, err
	}
	count := func(v *int, label, col string, dst *int) error {
		if v == nil {
			return nil
		}
		if *v < 0 {
			return fmt.Errorf("%s cannot be negative", label)
		}
		*dst = *v
		cols[col] = *v
		return nil
	}
	if err := count(in.MinStock, "Minimum stock", "min_stock", &p.MinStock); err != nil {
		return nil, err
	}
	if err := count(in.MaxStock, "Maximum stock", "max_stock", &p.MaxStock); err != nil {
		return nil, err
	}
	if err := count(in.ReorderPoint, "Reorder point", "reorder_point", &p.ReorderPoint); err != nil {
		return nil, err
	}
	if p.MaxStock > 0 && p.MinStock > p.MaxStock {
		return nil, errors.New("Minimum stock can't be more than maximum stock")
	}
	if creating && in.CurrentStock != nil {
		if *in.CurrentStock < 0 {
			return nil, errors.New("Opening stock cannot be negative")
		}
		p.CurrentStock = *in.CurrentStock
	}
	if in.IsActive != nil {
		p.IsActive, cols["is_active"] = *in.IsActive, *in.IsActive
	} else if creating {
		p.IsActive = true
	}
	if in.ImageURL != nil {
		p.ImageURL, cols["image_url"] = trim(in.ImageURL), trim(in.ImageURL)
	}
	if in.Weight != nil && *in.Weight >= 0 {
		p.Weight, cols["weight"] = *in.Weight, *in.Weight
	}
	if in.Dimensions != nil {
		p.Dimensions, cols["dimensions"] = trim(in.Dimensions), trim(in.Dimensions)
	}
	return cols, nil
}

func (h *Handler) skuTaken(orgID, sku, exceptID string) bool {
	var n int64
	q := h.DB.Model(&models.Product{}).Where("organization_id = ? AND UPPER(sku) = UPPER(?)", orgID, sku)
	if exceptID != "" {
		q = q.Where("id <> ?", exceptID)
	}
	q.Count(&n)
	return n > 0
}

// nextSKU builds a unique SKU from the product name, e.g. "Engine Oil" -> ENGOIL-0001.
func (h *Handler) nextSKU(orgID, name string) string {
	words := strings.Fields(strings.ToUpper(name))
	prefix := ""
	for _, w := range words {
		w = invSkuClean.ReplaceAllString(w, "")
		if w == "" {
			continue
		}
		if len(w) > 3 {
			w = w[:3]
		}
		prefix += w
		if len(prefix) >= 6 {
			break
		}
	}
	if prefix == "" {
		prefix = "SKU"
	}
	var n int64
	h.DB.Unscoped().Model(&models.Product{}).Where("organization_id = ?", orgID).Count(&n)
	for i := n + 1; i < n+10000; i++ {
		sku := fmt.Sprintf("%s-%04d", prefix, i)
		if !h.skuTaken(orgID, sku, "") {
			return sku
		}
	}
	return ""
}

func (h *Handler) GetProducts(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	var products []models.Product
	query := h.DB.Model(&models.Product{}).Where("products.organization_id = ?", orgID).Preload("Category").Preload("Brand")

	if categoryID := c.Query("category_id"); categoryID != "" {
		if categoryID == "none" {
			query = query.Where("products.category_id IS NULL")
		} else {
			query = query.Where("products.category_id = ?", categoryID)
		}
	}
	if brandID := c.Query("brand_id"); brandID != "" {
		query = query.Where("products.brand_id = ?", brandID)
	}
	if isActive := c.Query("is_active"); isActive != "" {
		if active, err := strconv.ParseBool(isActive); err == nil {
			query = query.Where("products.is_active = ?", active)
		}
	}
	switch c.Query("stock_status") {
	case "out_of_stock", "out":
		query = query.Where("products.current_stock <= 0")
	case "low_stock", "low":
		query = query.Where("products.current_stock > 0 AND products.current_stock <= " + invThresholdSQL + " AND " + invThresholdSQL + " > 0")
	case "in_stock", "in":
		query = query.Where("products.current_stock > 0 AND (products.current_stock > " + invThresholdSQL + " OR " + invThresholdSQL + " = 0)")
	case "attention":
		query = query.Where("(products.current_stock <= 0 OR products.current_stock <= " + invThresholdSQL + ")")
	}
	if search := strings.TrimSpace(c.Query("search")); search != "" {
		like := "%" + search + "%"
		query = query.Where("(products.name ILIKE ? OR products.sku ILIKE ? OR products.barcode ILIKE ?)", like, like, like)
	}

	if err := query.Order("products.name ASC").Find(&products).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to fetch products")
		return
	}

	views := make([]productView, 0, len(products))
	for _, p := range products {
		views = append(views, invView(p))
	}
	erpJSON(c, http.StatusOK, gin.H{"products": views, "total": len(views)})
}

func (h *Handler) GetProduct(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	var product models.Product
	if err := h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), orgID).
		Preload("Category").Preload("Brand").
		First(&product).Error; err != nil {
		erpErr(c, http.StatusNotFound, "Product not found")
		return
	}
	erpJSON(c, http.StatusOK, gin.H{"product": invView(product)})
}

func (h *Handler) CreateProduct(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	var in productInput
	if err := c.ShouldBindJSON(&in); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	var product models.Product
	if _, err := h.applyProductInput(orgID, in, &product, true); err != nil {
		erpErr(c, http.StatusBadRequest, err.Error())
		return
	}
	product.OrganizationID = orgID
	if product.SKU == "" {
		product.SKU = h.nextSKU(orgID, product.Name)
	} else if h.skuTaken(orgID, product.SKU, "") {
		erpErr(c, http.StatusConflict, fmt.Sprintf("SKU %s is already used by another product", product.SKU))
		return
	}

	opening := product.CurrentStock
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit(clause.Associations).Create(&product).Error; err != nil {
			return err
		}
		if in.IsActive != nil && !*in.IsActive {
			if err := tx.Model(&models.Product{}).Where("id = ?", product.ID).UpdateColumn("is_active", false).Error; err != nil {
				return err
			}
		}
		if opening > 0 {
			mv := models.InventoryMovement{
				OrganizationID:   orgID,
				ProductID:        product.ID,
				MovementType:     "in",
				Quantity:         opening,
				PreviousQuantity: 0,
				NewQuantity:      opening,
				UnitCost:         product.CostPrice,
				Reference:        "Opening stock",
				ReferenceType:    "adjustment",
				Notes:            "Opening stock when product was created",
				CreatedBy:        c.GetString("user_id"),
			}
			if err := tx.Omit(clause.Associations).Create(&mv).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			erpErr(c, http.StatusConflict, "That SKU is already in use")
			return
		}
		erpErr(c, http.StatusInternalServerError, "Failed to create product")
		return
	}

	h.DB.Preload("Category").Preload("Brand").Where("id = ?", product.ID).First(&product)
	erpJSON(c, http.StatusCreated, gin.H{"product": invView(product)})
}

func (h *Handler) UpdateProduct(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	productID := c.Param("id")

	var product models.Product
	if err := h.DB.Where("id = ? AND organization_id = ?", productID, orgID).First(&product).Error; err != nil {
		erpErr(c, http.StatusNotFound, "Product not found")
		return
	}

	var in productInput
	if err := c.ShouldBindJSON(&in); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	// Stock levels only change through adjustments / receiving so every
	// change is recorded as a movement.
	in.CurrentStock = nil
	cols, err := h.applyProductInput(orgID, in, &product, false)
	if err != nil {
		erpErr(c, http.StatusBadRequest, err.Error())
		return
	}
	if sku, ok := cols["sku"].(string); ok && h.skuTaken(orgID, sku, productID) {
		erpErr(c, http.StatusConflict, fmt.Sprintf("SKU %s is already used by another product", sku))
		return
	}
	if len(cols) > 0 {
		if err := h.DB.Model(&models.Product{}).Where("id = ? AND organization_id = ?", productID, orgID).Updates(cols).Error; err != nil {
			if strings.Contains(err.Error(), "duplicate key") {
				erpErr(c, http.StatusConflict, "That SKU is already in use")
				return
			}
			erpErr(c, http.StatusInternalServerError, "Failed to update product")
			return
		}
	}
	h.DB.Preload("Category").Preload("Brand").Where("id = ?", productID).First(&product)
	erpJSON(c, http.StatusOK, gin.H{"product": invView(product)})
}

func (h *Handler) DeleteProduct(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	productID := c.Param("id")

	var product models.Product
	if err := h.DB.Where("id = ? AND organization_id = ?", productID, orgID).First(&product).Error; err != nil {
		erpErr(c, http.StatusNotFound, "Product not found")
		return
	}
	var openLines int64
	h.DB.Model(&models.PurchaseOrderItem{}).
		Joins("JOIN purchase_orders ON purchase_orders.id = purchase_order_items.purchase_order_id").
		Where("purchase_order_items.product_id = ? AND purchase_orders.deleted_at IS NULL AND purchase_orders.status IN ?", productID, []string{poDraft, poSent, "confirmed", poPartial}).
		Count(&openLines)
	if openLines > 0 {
		erpErr(c, http.StatusConflict, "This product is on an open purchase order. Receive or cancel the order first, or mark the product inactive.")
		return
	}

	if err := h.DB.Where("id = ? AND organization_id = ?", productID, orgID).Delete(&models.Product{}).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to delete product")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Product deleted successfully"})
}

// ---------------------------------------------------------------------------
// Categories
// ---------------------------------------------------------------------------

type categoryView struct {
	models.ProductCategory
	ProductCount int64 `json:"product_count"`
}

func (h *Handler) GetProductCategories(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	var categories []models.ProductCategory
	if err := h.DB.Where("organization_id = ?", orgID).Order("name ASC").Find(&categories).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to fetch categories")
		return
	}
	type cnt struct {
		CategoryID string
		N          int64
	}
	var counts []cnt
	h.DB.Model(&models.Product{}).Select("category_id, COUNT(*) AS n").
		Where("organization_id = ? AND category_id IS NOT NULL", orgID).Group("category_id").Scan(&counts)
	cm := map[string]int64{}
	for _, r := range counts {
		cm[r.CategoryID] = r.N
	}
	views := make([]categoryView, 0, len(categories))
	for _, cat := range categories {
		views = append(views, categoryView{ProductCategory: cat, ProductCount: cm[cat.ID]})
	}
	erpJSON(c, http.StatusOK, gin.H{"categories": views})
}

type categoryInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	ParentID    *string `json:"parent_id"`
	IsActive    *bool   `json:"is_active"`
}

func (h *Handler) CreateProductCategory(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	var in categoryInput
	if err := c.ShouldBindJSON(&in); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	name := ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	if name == "" {
		erpErr(c, http.StatusBadRequest, "Category name is required")
		return
	}
	if len(name) > 100 {
		erpErr(c, http.StatusBadRequest, "Category name must be 100 characters or fewer")
		return
	}
	var dup int64
	h.DB.Model(&models.ProductCategory{}).Where("organization_id = ? AND LOWER(name) = LOWER(?)", orgID, name).Count(&dup)
	if dup > 0 {
		erpErr(c, http.StatusConflict, "A category with this name already exists")
		return
	}
	category := models.ProductCategory{OrganizationID: orgID, Name: name, IsActive: true}
	if in.Description != nil {
		category.Description = strings.TrimSpace(*in.Description)
	}
	if in.ParentID != nil && *in.ParentID != "" {
		var n int64
		h.DB.Model(&models.ProductCategory{}).Where("id = ? AND organization_id = ?", *in.ParentID, orgID).Count(&n)
		if n == 0 {
			erpErr(c, http.StatusBadRequest, "Parent category not found")
			return
		}
		category.ParentID = in.ParentID
	}

	if err := h.DB.Omit(clause.Associations).Create(&category).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to create category")
		return
	}
	erpJSON(c, http.StatusCreated, gin.H{"category": categoryView{ProductCategory: category}})
}

func (h *Handler) UpdateProductCategory(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	id := c.Param("id")
	var category models.ProductCategory
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&category).Error; err != nil {
		erpErr(c, http.StatusNotFound, "Category not found")
		return
	}
	var in categoryInput
	if err := c.ShouldBindJSON(&in); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	cols := map[string]interface{}{}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			erpErr(c, http.StatusBadRequest, "Category name is required")
			return
		}
		if len(name) > 100 {
			erpErr(c, http.StatusBadRequest, "Category name must be 100 characters or fewer")
			return
		}
		var dup int64
		h.DB.Model(&models.ProductCategory{}).Where("organization_id = ? AND LOWER(name) = LOWER(?) AND id <> ?", orgID, name, id).Count(&dup)
		if dup > 0 {
			erpErr(c, http.StatusConflict, "A category with this name already exists")
			return
		}
		cols["name"] = name
	}
	if in.Description != nil {
		cols["description"] = strings.TrimSpace(*in.Description)
	}
	if in.IsActive != nil {
		cols["is_active"] = *in.IsActive
	}
	if len(cols) > 0 {
		if err := h.DB.Model(&models.ProductCategory{}).Where("id = ?", id).Updates(cols).Error; err != nil {
			erpErr(c, http.StatusInternalServerError, "Failed to update category")
			return
		}
	}
	h.DB.Where("id = ?", id).First(&category)
	var n int64
	h.DB.Model(&models.Product{}).Where("organization_id = ? AND category_id = ?", orgID, id).Count(&n)
	erpJSON(c, http.StatusOK, gin.H{"category": categoryView{ProductCategory: category, ProductCount: n}})
}

// DeleteProductCategory removes a category; its products become uncategorised.
func (h *Handler) DeleteProductCategory(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	id := c.Param("id")
	var category models.ProductCategory
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&category).Error; err != nil {
		erpErr(c, http.StatusNotFound, "Category not found")
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.Product{}).Where("organization_id = ? AND category_id = ?", orgID, id).UpdateColumn("category_id", nil).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ProductCategory{}).Where("organization_id = ? AND parent_id = ?", orgID, id).UpdateColumn("parent_id", nil).Error; err != nil {
			return err
		}
		return tx.Delete(&category).Error
	})
	if err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to delete category")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Category deleted"})
}

// ---------------------------------------------------------------------------
// Brands
// ---------------------------------------------------------------------------

func (h *Handler) GetBrands(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	var brands []models.Brand
	if err := h.DB.Where("organization_id = ?", orgID).Order("name ASC").Find(&brands).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to fetch brands")
		return
	}
	erpJSON(c, http.StatusOK, gin.H{"brands": brands})
}

func (h *Handler) CreateBrand(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Website     string `json:"website"`
		LogoURL     string `json:"logo_url"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		erpErr(c, http.StatusBadRequest, "Brand name is required (max 100 characters)")
		return
	}
	var dup int64
	h.DB.Model(&models.Brand{}).Where("organization_id = ? AND LOWER(name) = LOWER(?)", orgID, in.Name).Count(&dup)
	if dup > 0 {
		erpErr(c, http.StatusConflict, "A brand with this name already exists")
		return
	}
	brand := models.Brand{OrganizationID: orgID, Name: in.Name, Description: strings.TrimSpace(in.Description), Website: strings.TrimSpace(in.Website), LogoURL: strings.TrimSpace(in.LogoURL), IsActive: true}
	if err := h.DB.Omit(clause.Associations).Create(&brand).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to create brand")
		return
	}
	erpJSON(c, http.StatusCreated, gin.H{"brand": brand})
}

// ---------------------------------------------------------------------------
// Inventory items, adjustments and movements
// ---------------------------------------------------------------------------

func (h *Handler) GetInventoryItems(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	var items []models.InventoryItem
	query := h.DB.Where("organization_id = ?", orgID).Preload("Product").Preload("Location")
	if productID := c.Query("product_id"); productID != "" {
		query = query.Where("product_id = ?", productID)
	}
	if locationID := c.Query("location_id"); locationID != "" {
		query = query.Where("location_id = ?", locationID)
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	if err := query.Order("created_at DESC").Find(&items).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to fetch inventory items")
		return
	}
	erpJSON(c, http.StatusOK, gin.H{"inventory_items": items})
}

// AdjustInventory changes a product's stock level and records a movement.
// movement_type: "in"/"add" (add quantity), "out"/"remove" (subtract),
// "adjustment"/"set" (set the stock level to quantity).
func (h *Handler) AdjustInventory(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	var req struct {
		ProductID    string  `json:"product_id"`
		Quantity     *int    `json:"quantity"`
		MovementType string  `json:"movement_type"`
		UnitCost     float64 `json:"unit_cost"`
		Reference    string  `json:"reference"`
		Reason       string  `json:"reason"`
		Notes        string  `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	if req.ProductID == "" {
		erpErr(c, http.StatusBadRequest, "product_id is required")
		return
	}
	if req.Quantity == nil || *req.Quantity < 0 {
		erpErr(c, http.StatusBadRequest, "Enter a quantity of 0 or more")
		return
	}
	mt := strings.ToLower(strings.TrimSpace(req.MovementType))
	switch mt {
	case "add":
		mt = "in"
	case "remove":
		mt = "out"
	case "set":
		mt = "adjustment"
	}
	if mt != "in" && mt != "out" && mt != "adjustment" {
		erpErr(c, http.StatusBadRequest, "movement_type must be in, out or adjustment")
		return
	}
	if (mt == "in" || mt == "out") && *req.Quantity == 0 {
		erpErr(c, http.StatusBadRequest, "Quantity must be at least 1")
		return
	}
	notes := strings.TrimSpace(req.Notes)
	reason := strings.TrimSpace(req.Reason)
	if reason != "" {
		if notes != "" {
			notes = reason + " – " + notes
		} else {
			notes = reason
		}
	}
	reference := strings.TrimSpace(req.Reference)
	if len(reference) > 100 {
		reference = reference[:100]
	}

	var movement models.InventoryMovement
	var httpErr int
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		var product models.Product
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", req.ProductID, orgID).First(&product).Error; err != nil {
			httpErr = http.StatusNotFound
			return errors.New("Product not found")
		}
		prev := product.CurrentStock
		q := *req.Quantity
		next := prev
		delta := q
		switch mt {
		case "in":
			next = prev + q
		case "out":
			next = prev - q
			if next < 0 {
				httpErr = http.StatusBadRequest
				return fmt.Errorf("Only %d in stock – can't remove %d", prev, q)
			}
		case "adjustment":
			next = q
			delta = q - prev
			if delta == 0 {
				httpErr = http.StatusBadRequest
				return errors.New("Stock is already at that level")
			}
		}
		if err := tx.Model(&models.Product{}).Where("id = ?", product.ID).UpdateColumn("current_stock", next).Error; err != nil {
			return err
		}
		cost := req.UnitCost
		if cost <= 0 {
			cost = product.CostPrice
		}
		movement = models.InventoryMovement{
			OrganizationID:   orgID,
			ProductID:        product.ID,
			MovementType:     mt,
			Quantity:         delta,
			PreviousQuantity: prev,
			NewQuantity:      next,
			UnitCost:         cost,
			Reference:        reference,
			ReferenceType:    "adjustment",
			Notes:            notes,
			CreatedBy:        c.GetString("user_id"),
		}
		if mt == "out" {
			movement.Quantity = -q
		}
		return tx.Omit(clause.Associations).Create(&movement).Error
	})
	if err != nil {
		if httpErr == 0 {
			erpErr(c, http.StatusInternalServerError, "Failed to adjust stock")
			return
		}
		erpErr(c, httpErr, err.Error())
		return
	}

	var product models.Product
	h.DB.Preload("Category").Where("id = ?", req.ProductID).First(&product)
	erpJSON(c, http.StatusOK, gin.H{
		"message":           "Inventory adjusted successfully",
		"previous_quantity": movement.PreviousQuantity,
		"new_quantity":      movement.NewQuantity,
		"movement":          movement,
		"product":           invView(product),
	})
}

func (h *Handler) GetInventoryMovements(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	limit := 100
	if l, err := strconv.Atoi(c.Query("limit")); err == nil && l > 0 {
		limit = l
		if limit > 500 {
			limit = 500
		}
	}
	var movements []models.InventoryMovement
	query := h.DB.Where("organization_id = ?", orgID).
		Preload("Product", func(db *gorm.DB) *gorm.DB {
			return db.Unscoped().Select("id, organization_id, name, sku, unit_of_measure")
		}).
		Preload("Creator", func(db *gorm.DB) *gorm.DB {
			return db.Unscoped().Select("id, first_name, last_name, email")
		}).
		Order("created_at DESC").Limit(limit)

	if productID := c.Query("product_id"); productID != "" {
		query = query.Where("product_id = ?", productID)
	}
	if movementType := c.Query("movement_type"); movementType != "" {
		query = query.Where("movement_type = ?", movementType)
	}
	if refType := c.Query("reference_type"); refType != "" {
		query = query.Where("reference_type = ?", refType)
	}

	if err := query.Find(&movements).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to fetch inventory movements")
		return
	}
	erpJSON(c, http.StatusOK, gin.H{"movements": movements})
}

// ---------------------------------------------------------------------------
// Locations
// ---------------------------------------------------------------------------

func (h *Handler) GetInventoryLocations(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	var locations []models.InventoryLocation
	if err := h.DB.Where("organization_id = ?", orgID).Order("name ASC").Find(&locations).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to fetch locations")
		return
	}
	erpJSON(c, http.StatusOK, gin.H{"locations": locations})
}

func (h *Handler) CreateInventoryLocation(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		erpErr(c, http.StatusBadRequest, "Location name is required (max 100 characters)")
		return
	}
	if in.Type == "" {
		in.Type = "warehouse"
	}
	location := models.InventoryLocation{OrganizationID: orgID, Name: in.Name, Description: strings.TrimSpace(in.Description), Type: in.Type, IsActive: true}
	if err := h.DB.Omit(clause.Associations).Create(&location).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to create location")
		return
	}
	erpJSON(c, http.StatusCreated, gin.H{"location": location})
}

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

func (h *Handler) GetInventoryReport(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	type catRow struct {
		CategoryID   *string `json:"category_id"`
		CategoryName *string `json:"category_name"`
		ProductCount int64   `json:"product_count"`
		TotalValue   float64 `json:"total_value"`
	}
	var report struct {
		TotalProducts      int64         `json:"total_products"`
		ActiveProducts     int64         `json:"active_products"`
		TotalUnits         int64         `json:"total_units"`
		TotalStockValue    float64       `json:"total_stock_value"`
		TotalRetailValue   float64       `json:"total_retail_value"`
		LowStockCount      int64         `json:"low_stock_count"`
		OutOfStockCount    int64         `json:"out_of_stock_count"`
		LowStockProducts   []productView `json:"low_stock_products"`
		OutOfStockProducts []productView `json:"out_of_stock_products"`
		CategoryBreakdown  []catRow      `json:"category_breakdown"`
	}

	base := func() *gorm.DB {
		return h.DB.Model(&models.Product{}).Where("products.organization_id = ? AND products.is_active = ?", orgID, true)
	}
	h.DB.Model(&models.Product{}).Where("organization_id = ?", orgID).Count(&report.TotalProducts)
	base().Count(&report.ActiveProducts)
	base().Select("COALESCE(SUM(GREATEST(current_stock,0)),0), COALESCE(SUM(GREATEST(current_stock,0) * cost_price),0), COALESCE(SUM(GREATEST(current_stock,0) * selling_price),0)").
		Row().Scan(&report.TotalUnits, &report.TotalStockValue, &report.TotalRetailValue)

	lowWhere := "products.current_stock > 0 AND products.current_stock <= " + invThresholdSQL + " AND " + invThresholdSQL + " > 0"
	var low, out []models.Product
	base().Where(lowWhere).Preload("Category").Order("products.current_stock ASC").Find(&low)
	base().Where("products.current_stock <= 0").Preload("Category").Order("products.name ASC").Find(&out)
	report.LowStockCount, report.OutOfStockCount = int64(len(low)), int64(len(out))
	report.LowStockProducts, report.OutOfStockProducts = []productView{}, []productView{}
	for _, p := range low {
		report.LowStockProducts = append(report.LowStockProducts, invView(p))
	}
	for _, p := range out {
		report.OutOfStockProducts = append(report.OutOfStockProducts, invView(p))
	}

	h.DB.Table("products").
		Select("products.category_id, product_categories.name AS category_name, COUNT(*) AS product_count, COALESCE(SUM(GREATEST(products.current_stock,0) * products.cost_price),0) AS total_value").
		Joins("LEFT JOIN product_categories ON products.category_id = product_categories.id AND product_categories.deleted_at IS NULL").
		Where("products.organization_id = ? AND products.deleted_at IS NULL AND products.is_active = ?", orgID, true).
		Group("products.category_id, product_categories.name").
		Order("total_value DESC").
		Scan(&report.CategoryBreakdown)
	if report.CategoryBreakdown == nil {
		report.CategoryBreakdown = []catRow{}
	}
	report.TotalStockValue = erpRound(report.TotalStockValue)
	report.TotalRetailValue = erpRound(report.TotalRetailValue)

	erpJSON(c, http.StatusOK, gin.H{"report": report})
}
