package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
)

// ---------------------------------------------------------------------------
// Shared helpers for the supplier / inventory module
// ---------------------------------------------------------------------------

// Purchase order statuses. "confirmed" is a legacy value treated like "sent".
const (
	poDraft     = "draft"
	poSent      = "sent"
	poPartial   = "partially_received"
	poReceived  = "received"
	poCancelled = "cancelled"
)

var erpEmailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// erpJSON writes v as JSON after pruning empty relationship objects (the ERP
// models embed non-pointer relations such as Organization/Creator which would
// otherwise serialise as large blocks of empty fields).
func erpJSON(c *gin.Context, status int, v interface{}) {
	raw, err := json.Marshal(v)
	if err != nil {
		c.JSON(status, v)
		return
	}
	var generic interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		c.JSON(status, v)
		return
	}
	c.JSON(status, erpPrune(generic))
}

func erpPrune(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if m, ok := val.(map[string]interface{}); ok {
				if id, has := m["id"]; has && (id == "" || id == nil) {
					delete(t, k)
					continue
				}
			}
			if k == "organization" {
				delete(t, k)
				continue
			}
			t[k] = erpPrune(val)
		}
		return t
	case []interface{}:
		for i := range t {
			t[i] = erpPrune(t[i])
		}
		return t
	}
	return v
}

func erpErr(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": msg})
}

func erpOrg(h *Handler, c *gin.Context) (string, bool) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		erpErr(c, http.StatusBadRequest, "Organization ID required")
		return "", false
	}
	return orgID, true
}

func erpRound(v float64) float64 {
	if v < 0 {
		return -float64(int64(-v*100+0.5)) / 100
	}
	return float64(int64(v*100+0.5)) / 100
}

func erpParseDay(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			// Store calendar dates at midday UTC so they read back as the same
			// day in any server/database time zone.
			t = t.Add(12 * time.Hour)
			return &t, nil
		}
	}
	return nil, fmt.Errorf("invalid date %q (use YYYY-MM-DD)", s)
}

func erpToday() time.Time {
	d, _ := erpParseDay(time.Now().Format("2006-01-02"))
	return *d
}

func erpNormalizePOStatus(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "ordered", "confirmed":
		return poSent
	case "partial", "partially-received", "partially received":
		return poPartial
	case "canceled":
		return poCancelled
	}
	return s
}

// ---------------------------------------------------------------------------
// Suppliers
// ---------------------------------------------------------------------------

type supplierInput struct {
	Name          *string         `json:"name"`
	ContactPerson *string         `json:"contact_person"`
	Email         *string         `json:"email"`
	Phone         *string         `json:"phone"`
	Address       *models.Address `json:"address"`
	ABN           *string         `json:"abn"`
	PaymentTerms  *string         `json:"payment_terms"`
	CreditLimit   *float64        `json:"credit_limit"`
	Rating        *int            `json:"rating"`
	Notes         *string         `json:"notes"`
	IsActive      *bool           `json:"is_active"`
}

// apply validates the input and copies it onto s. It returns the changed
// column map (for updates).
func (in supplierInput) apply(s *models.Supplier, creating bool) (map[string]interface{}, error) {
	cols := map[string]interface{}{}
	str := func(p *string) string { return strings.TrimSpace(*p) }
	if in.Name != nil || creating {
		name := ""
		if in.Name != nil {
			name = str(in.Name)
		}
		if name == "" {
			return nil, errors.New("Supplier name is required")
		}
		if len(name) > 255 {
			return nil, errors.New("Supplier name is too long")
		}
		s.Name = name
		cols["name"] = name
	}
	if in.ContactPerson != nil {
		s.ContactPerson = str(in.ContactPerson)
		cols["contact_person"] = s.ContactPerson
	}
	if in.Email != nil {
		e := str(in.Email)
		if e != "" && !erpEmailRe.MatchString(e) {
			return nil, errors.New("Enter a valid email address")
		}
		s.Email = e
		cols["email"] = e
	}
	if in.Phone != nil {
		p := str(in.Phone)
		if len(p) > 20 {
			return nil, errors.New("Phone number must be 20 characters or fewer")
		}
		s.Phone = p
		cols["phone"] = p
	}
	if in.ABN != nil {
		abn := strings.ReplaceAll(str(in.ABN), " ", "")
		if len(abn) > 11 {
			return nil, errors.New("ABN must be 11 digits")
		}
		s.ABN = abn
		cols["abn"] = abn
	}
	if in.PaymentTerms != nil {
		s.PaymentTerms = str(in.PaymentTerms)
		cols["payment_terms"] = s.PaymentTerms
	}
	if in.CreditLimit != nil {
		if *in.CreditLimit < 0 {
			return nil, errors.New("Credit limit cannot be negative")
		}
		s.CreditLimit = *in.CreditLimit
		cols["credit_limit"] = s.CreditLimit
	}
	if in.Rating != nil {
		r := *in.Rating
		if r < 0 || r > 5 {
			return nil, errors.New("Rating must be between 0 and 5")
		}
		s.Rating = r
		cols["rating"] = r
	}
	if in.Notes != nil {
		s.Notes = strings.TrimSpace(*in.Notes)
		cols["notes"] = s.Notes
	}
	if in.IsActive != nil {
		s.IsActive = *in.IsActive
		cols["is_active"] = s.IsActive
	} else if creating {
		s.IsActive = true
	}
	if in.Address != nil {
		a := *in.Address
		a.Street, a.Suburb, a.State, a.Postcode, a.Country = strings.TrimSpace(a.Street), strings.TrimSpace(a.Suburb), strings.TrimSpace(a.State), strings.TrimSpace(a.Postcode), strings.TrimSpace(a.Country)
		if len(a.Postcode) > 10 || len(a.State) > 50 {
			return nil, errors.New("Address state/postcode is too long")
		}
		if a.Country == "" {
			a.Country = "Australia"
		}
		s.Address = a
		cols["address_street"], cols["address_suburb"], cols["address_state"] = a.Street, a.Suburb, a.State
		cols["address_postcode"], cols["address_country"] = a.Postcode, a.Country
	}
	return cols, nil
}

type supplierStats struct {
	SupplierID  string     `json:"-"`
	OrderCount  int64      `json:"order_count"`
	OpenOrders  int64      `json:"open_orders"`
	TotalSpend  float64    `json:"total_spend"`
	LastOrderAt *time.Time `json:"last_order_date"`
}

func (h *Handler) supplierStatsMap(orgID string, ids []string) map[string]supplierStats {
	out := map[string]supplierStats{}
	if len(ids) == 0 {
		return out
	}
	var rows []supplierStats
	h.DB.Model(&models.PurchaseOrder{}).
		Select(`supplier_id,
			COUNT(*) AS order_count,
			COUNT(*) FILTER (WHERE status IN ('sent','confirmed','partially_received')) AS open_orders,
			COALESCE(SUM(total_amount) FILTER (WHERE status NOT IN ('draft','cancelled')),0) AS total_spend,
			MAX(order_date) AS last_order_at`).
		Where("organization_id = ? AND supplier_id IN ?", orgID, ids).
		Group("supplier_id").
		Scan(&rows)
	for _, r := range rows {
		out[r.SupplierID] = r
	}
	return out
}

type supplierView struct {
	models.Supplier
	supplierStats
}

func (h *Handler) GetSuppliers(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	var suppliers []models.Supplier
	query := h.DB.Where("organization_id = ?", orgID)

	status := c.Query("status")
	if isActive := c.Query("is_active"); isActive != "" {
		if active, err := strconv.ParseBool(isActive); err == nil {
			query = query.Where("is_active = ?", active)
		}
	} else if status == "active" {
		query = query.Where("is_active = ?", true)
	} else if status == "inactive" {
		query = query.Where("is_active = ?", false)
	}

	if search := strings.TrimSpace(c.Query("search")); search != "" {
		like := "%" + search + "%"
		query = query.Where("(name ILIKE ? OR email ILIKE ? OR contact_person ILIKE ? OR phone ILIKE ?)", like, like, like, like)
	}

	if err := query.Order("is_active DESC, name ASC").Find(&suppliers).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to fetch suppliers")
		return
	}

	ids := make([]string, 0, len(suppliers))
	for _, s := range suppliers {
		ids = append(ids, s.ID)
	}
	stats := h.supplierStatsMap(orgID, ids)
	views := make([]supplierView, 0, len(suppliers))
	for _, s := range suppliers {
		views = append(views, supplierView{Supplier: s, supplierStats: stats[s.ID]})
	}
	erpJSON(c, http.StatusOK, gin.H{"suppliers": views, "total": len(views)})
}

func (h *Handler) GetSupplier(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	supplierID := c.Param("id")

	var supplier models.Supplier
	if err := h.DB.Where("id = ? AND organization_id = ?", supplierID, orgID).
		Preload("PurchaseOrders", func(db *gorm.DB) *gorm.DB {
			return db.Where("organization_id = ?", orgID).Order("order_date DESC, created_at DESC")
		}).
		Preload("PurchaseOrders.Items").
		First(&supplier).Error; err != nil {
		erpErr(c, http.StatusNotFound, "Supplier not found")
		return
	}
	stats := h.supplierStatsMap(orgID, []string{supplier.ID})
	erpJSON(c, http.StatusOK, gin.H{"supplier": supplierView{Supplier: supplier, supplierStats: stats[supplier.ID]}})
}

func (h *Handler) CreateSupplier(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	var in supplierInput
	if err := c.ShouldBindJSON(&in); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	var supplier models.Supplier
	if _, err := in.apply(&supplier, true); err != nil {
		erpErr(c, http.StatusBadRequest, err.Error())
		return
	}
	supplier.OrganizationID = orgID

	var dup int64
	h.DB.Model(&models.Supplier{}).Where("organization_id = ? AND LOWER(name) = LOWER(?)", orgID, supplier.Name).Count(&dup)
	if dup > 0 {
		erpErr(c, http.StatusConflict, "A supplier with this name already exists")
		return
	}

	// Omit associations so gorm never tries to upsert the embedded Organization.
	if err := h.DB.Omit(clause.Associations).Create(&supplier).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to create supplier")
		return
	}
	if in.IsActive != nil && !*in.IsActive {
		// gorm skips false for columns with a default – persist explicitly.
		h.DB.Model(&supplier).Update("is_active", false)
	}

	erpJSON(c, http.StatusCreated, gin.H{"supplier": supplierView{Supplier: supplier}})
}

func (h *Handler) UpdateSupplier(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	supplierID := c.Param("id")

	var supplier models.Supplier
	if err := h.DB.Where("id = ? AND organization_id = ?", supplierID, orgID).First(&supplier).Error; err != nil {
		erpErr(c, http.StatusNotFound, "Supplier not found")
		return
	}

	var in supplierInput
	if err := c.ShouldBindJSON(&in); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	cols, err := in.apply(&supplier, false)
	if err != nil {
		erpErr(c, http.StatusBadRequest, err.Error())
		return
	}
	if name, ok := cols["name"]; ok {
		var dup int64
		h.DB.Model(&models.Supplier{}).Where("organization_id = ? AND LOWER(name) = LOWER(?) AND id <> ?", orgID, name, supplierID).Count(&dup)
		if dup > 0 {
			erpErr(c, http.StatusConflict, "A supplier with this name already exists")
			return
		}
	}
	if len(cols) > 0 {
		if err := h.DB.Model(&models.Supplier{}).Where("id = ? AND organization_id = ?", supplierID, orgID).Updates(cols).Error; err != nil {
			erpErr(c, http.StatusInternalServerError, "Failed to update supplier")
			return
		}
	}
	h.DB.Where("id = ?", supplierID).First(&supplier)
	stats := h.supplierStatsMap(orgID, []string{supplierID})
	erpJSON(c, http.StatusOK, gin.H{"supplier": supplierView{Supplier: supplier, supplierStats: stats[supplierID]}})
}

func (h *Handler) DeleteSupplier(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	supplierID := c.Param("id")

	var supplier models.Supplier
	if err := h.DB.Where("id = ? AND organization_id = ?", supplierID, orgID).First(&supplier).Error; err != nil {
		erpErr(c, http.StatusNotFound, "Supplier not found")
		return
	}

	var count int64
	h.DB.Model(&models.PurchaseOrder{}).Where("organization_id = ? AND supplier_id = ?", orgID, supplierID).Count(&count)
	if count > 0 {
		erpErr(c, http.StatusConflict, fmt.Sprintf("%s has %d purchase order(s) and can't be deleted. Mark the supplier inactive instead.", supplier.Name, count))
		return
	}

	if err := h.DB.Where("id = ? AND organization_id = ?", supplierID, orgID).Delete(&models.Supplier{}).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to delete supplier")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Supplier deleted successfully"})
}

// ---------------------------------------------------------------------------
// Purchase orders
// ---------------------------------------------------------------------------

type poItemInput struct {
	ID        string  `json:"id"`
	ProductID string  `json:"product_id"`
	Quantity  int     `json:"quantity"`
	UnitCost  float64 `json:"unit_cost"`
}

type poInput struct {
	SupplierID   *string       `json:"supplier_id"`
	OrderDate    *string       `json:"order_date"`
	ExpectedDate *string       `json:"expected_date"`
	Status       *string       `json:"status"`
	Notes        *string       `json:"notes"`
	TaxAmount    *float64      `json:"tax_amount"`
	ShippingCost *float64      `json:"shipping_cost"`
	Items        []poItemInput `json:"items"`
}

func (h *Handler) loadPO(db *gorm.DB, orgID, id string) (*models.PurchaseOrder, error) {
	var order models.PurchaseOrder
	err := db.Where("id = ? AND organization_id = ?", id, orgID).
		Preload("Supplier").
		Preload("Creator").
		Preload("Items", func(db *gorm.DB) *gorm.DB { return db.Order("created_at ASC") }).
		Preload("Items.Product", func(db *gorm.DB) *gorm.DB { return db.Unscoped() }).
		First(&order).Error
	if err != nil {
		return nil, err
	}
	return &order, nil
}

// validatePOItems checks products belong to the org and builds item rows.
func (h *Handler) validatePOItems(orgID string, in []poItemInput) ([]models.PurchaseOrderItem, float64, error) {
	if len(in) == 0 {
		return nil, 0, errors.New("Add at least one line item")
	}
	ids := make([]string, 0, len(in))
	for i, it := range in {
		if strings.TrimSpace(it.ProductID) == "" {
			return nil, 0, fmt.Errorf("Line %d: choose a product", i+1)
		}
		if it.Quantity <= 0 {
			return nil, 0, fmt.Errorf("Line %d: quantity must be at least 1", i+1)
		}
		if it.UnitCost < 0 {
			return nil, 0, fmt.Errorf("Line %d: unit cost cannot be negative", i+1)
		}
		ids = append(ids, it.ProductID)
	}
	var found int64
	var distinct []string
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			distinct = append(distinct, id)
		}
	}
	h.DB.Model(&models.Product{}).Where("organization_id = ? AND id IN ?", orgID, distinct).Count(&found)
	if int(found) != len(distinct) {
		return nil, 0, errors.New("One or more products were not found")
	}
	items := make([]models.PurchaseOrderItem, 0, len(in))
	var sub float64
	for _, it := range in {
		cost := erpRound(it.UnitCost)
		total := erpRound(float64(it.Quantity) * cost)
		items = append(items, models.PurchaseOrderItem{ProductID: it.ProductID, Quantity: it.Quantity, UnitCost: cost, TotalCost: total})
		sub += total
	}
	return items, erpRound(sub), nil
}

func (h *Handler) GetPurchaseOrders(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	var orders []models.PurchaseOrder
	query := h.DB.Where("purchase_orders.organization_id = ?", orgID).
		Preload("Supplier").
		Preload("Items").
		Preload("Items.Product", func(db *gorm.DB) *gorm.DB { return db.Unscoped() }).
		Order("purchase_orders.order_date DESC, purchase_orders.created_at DESC")

	if status := erpNormalizePOStatus(c.Query("status")); status != "" && status != "all" {
		switch status {
		case poSent:
			query = query.Where("purchase_orders.status IN ?", []string{poSent, "confirmed"})
		case "open":
			query = query.Where("purchase_orders.status IN ?", []string{poSent, "confirmed", poPartial})
		case "overdue":
			query = query.Where("purchase_orders.status IN ? AND purchase_orders.expected_date < ?", []string{poSent, "confirmed", poPartial}, reqtz.DateString(c))
		default:
			query = query.Where("purchase_orders.status = ?", status)
		}
	}
	if supplierID := c.Query("supplier_id"); supplierID != "" {
		query = query.Where("purchase_orders.supplier_id = ?", supplierID)
	}
	if search := strings.TrimSpace(c.Query("search")); search != "" {
		like := "%" + search + "%"
		query = query.Joins("LEFT JOIN suppliers s ON s.id = purchase_orders.supplier_id").
			Where("(purchase_orders.order_number ILIKE ? OR s.name ILIKE ? OR purchase_orders.notes ILIKE ?)", like, like, like)
	}

	if err := query.Find(&orders).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to fetch purchase orders")
		return
	}
	for i := range orders {
		orders[i].Status = erpNormalizePOStatus(orders[i].Status)
	}

	// Status counts for tabs.
	type cnt struct {
		Status string
		N      int64
	}
	var counts []cnt
	h.DB.Model(&models.PurchaseOrder{}).Select("status, COUNT(*) AS n").Where("organization_id = ?", orgID).Group("status").Scan(&counts)
	cm := map[string]int64{"all": 0}
	for _, r := range counts {
		cm[erpNormalizePOStatus(r.Status)] += r.N
		cm["all"] += r.N
	}

	erpJSON(c, http.StatusOK, gin.H{"purchase_orders": orders, "total": len(orders), "counts": cm})
}

func (h *Handler) GetPurchaseOrder(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	order, err := h.loadPO(h.DB, orgID, c.Param("id"))
	if err != nil {
		erpErr(c, http.StatusNotFound, "Purchase order not found")
		return
	}
	order.Status = erpNormalizePOStatus(order.Status)
	erpJSON(c, http.StatusOK, gin.H{"purchase_order": order})
}

// nextPONumber returns the next sequential number for the organisation.
func (h *Handler) nextPONumber(tx *gorm.DB, orgID string) string {
	var n int64
	tx.Unscoped().Model(&models.PurchaseOrder{}).Where("organization_id = ?", orgID).Count(&n)
	for i := n + 1; i < n+1000; i++ {
		num := fmt.Sprintf("PO-%05d", i)
		var exists int64
		tx.Unscoped().Model(&models.PurchaseOrder{}).Where("organization_id = ? AND order_number = ?", orgID, num).Count(&exists)
		if exists == 0 {
			return num
		}
	}
	return "" // model hook falls back to a random number
}

func (h *Handler) CreatePurchaseOrder(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	var req poInput
	if err := c.ShouldBindJSON(&req); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	if req.SupplierID == nil || *req.SupplierID == "" {
		erpErr(c, http.StatusBadRequest, "Choose a supplier")
		return
	}

	var supplier models.Supplier
	if err := h.DB.Where("id = ? AND organization_id = ?", *req.SupplierID, orgID).First(&supplier).Error; err != nil {
		erpErr(c, http.StatusBadRequest, "Supplier not found")
		return
	}

	items, subTotal, err := h.validatePOItems(orgID, req.Items)
	if err != nil {
		erpErr(c, http.StatusBadRequest, err.Error())
		return
	}

	order := models.PurchaseOrder{
		OrganizationID: orgID,
		SupplierID:     supplier.ID,
		Status:         poDraft,
		CreatedBy:      c.GetString("user_id"),
		OrderDate:      erpToday(),
	}
	if req.Status != nil {
		st := erpNormalizePOStatus(*req.Status)
		if st != poDraft && st != poSent {
			erpErr(c, http.StatusBadRequest, "A new purchase order must be draft or sent")
			return
		}
		order.Status = st
	}
	if req.Notes != nil {
		order.Notes = strings.TrimSpace(*req.Notes)
	}
	if req.OrderDate != nil {
		d, err := erpParseDay(*req.OrderDate)
		if err != nil {
			erpErr(c, http.StatusBadRequest, "Invalid order date")
			return
		}
		if d != nil {
			order.OrderDate = *d
		}
	}
	if req.ExpectedDate != nil {
		d, err := erpParseDay(*req.ExpectedDate)
		if err != nil {
			erpErr(c, http.StatusBadRequest, "Invalid expected date")
			return
		}
		order.ExpectedDate = d
	}
	if req.TaxAmount != nil {
		if *req.TaxAmount < 0 {
			erpErr(c, http.StatusBadRequest, "Tax cannot be negative")
			return
		}
		order.TaxAmount = erpRound(*req.TaxAmount)
	}
	if req.ShippingCost != nil {
		if *req.ShippingCost < 0 {
			erpErr(c, http.StatusBadRequest, "Shipping cannot be negative")
			return
		}
		order.ShippingCost = erpRound(*req.ShippingCost)
	}
	order.SubTotal = subTotal
	order.TotalAmount = erpRound(subTotal + order.TaxAmount + order.ShippingCost)

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		order.OrderNumber = h.nextPONumber(tx, orgID)
		if err := tx.Omit(clause.Associations).Create(&order).Error; err != nil {
			return err
		}
		for i := range items {
			items[i].PurchaseOrderID = order.ID
			if err := tx.Omit(clause.Associations).Create(&items[i]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to create purchase order")
		return
	}

	created, _ := h.loadPO(h.DB, orgID, order.ID)
	erpJSON(c, http.StatusCreated, gin.H{"purchase_order": created})
}

// UpdatePurchaseOrder edits a draft/sent order that has not been received yet.
func (h *Handler) UpdatePurchaseOrder(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	id := c.Param("id")
	order, err := h.loadPO(h.DB, orgID, id)
	if err != nil {
		erpErr(c, http.StatusNotFound, "Purchase order not found")
		return
	}
	st := erpNormalizePOStatus(order.Status)
	if st != poDraft && st != poSent {
		erpErr(c, http.StatusConflict, "Only draft or sent purchase orders can be edited")
		return
	}

	var req poInput
	if err := c.ShouldBindJSON(&req); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	cols := map[string]interface{}{}
	if req.SupplierID != nil && *req.SupplierID != order.SupplierID {
		var s models.Supplier
		if err := h.DB.Where("id = ? AND organization_id = ?", *req.SupplierID, orgID).First(&s).Error; err != nil {
			erpErr(c, http.StatusBadRequest, "Supplier not found")
			return
		}
		cols["supplier_id"] = s.ID
	}
	if req.Notes != nil {
		cols["notes"] = strings.TrimSpace(*req.Notes)
	}
	if req.OrderDate != nil {
		d, err := erpParseDay(*req.OrderDate)
		if err != nil || d == nil {
			erpErr(c, http.StatusBadRequest, "Invalid order date")
			return
		}
		cols["order_date"] = *d
	}
	if req.ExpectedDate != nil {
		d, err := erpParseDay(*req.ExpectedDate)
		if err != nil {
			erpErr(c, http.StatusBadRequest, "Invalid expected date")
			return
		}
		cols["expected_date"] = d
	}
	tax, ship := order.TaxAmount, order.ShippingCost
	if req.TaxAmount != nil {
		if *req.TaxAmount < 0 {
			erpErr(c, http.StatusBadRequest, "Tax cannot be negative")
			return
		}
		tax = erpRound(*req.TaxAmount)
	}
	if req.ShippingCost != nil {
		if *req.ShippingCost < 0 {
			erpErr(c, http.StatusBadRequest, "Shipping cannot be negative")
			return
		}
		ship = erpRound(*req.ShippingCost)
	}
	if req.Status != nil {
		ns := erpNormalizePOStatus(*req.Status)
		if ns != poDraft && ns != poSent {
			erpErr(c, http.StatusBadRequest, "Use the status endpoint to change this status")
			return
		}
		cols["status"] = ns
	}
	sub := order.SubTotal
	var items []models.PurchaseOrderItem
	if req.Items != nil {
		items, sub, err = h.validatePOItems(orgID, req.Items)
		if err != nil {
			erpErr(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	cols["tax_amount"], cols["shipping_cost"], cols["sub_total"] = tax, ship, sub
	cols["total_amount"] = erpRound(sub + tax + ship)

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if items != nil {
			if err := tx.Where("purchase_order_id = ?", id).Delete(&models.PurchaseOrderItem{}).Error; err != nil {
				return err
			}
			for i := range items {
				items[i].PurchaseOrderID = id
				if err := tx.Omit(clause.Associations).Create(&items[i]).Error; err != nil {
					return err
				}
			}
		}
		// UpdateColumns skips the model's BeforeUpdate hook (we set totals ourselves).
		return tx.Model(&models.PurchaseOrder{}).Where("id = ? AND organization_id = ?", id, orgID).UpdateColumns(cols).Error
	})
	if err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to update purchase order")
		return
	}
	updated, _ := h.loadPO(h.DB, orgID, id)
	erpJSON(c, http.StatusOK, gin.H{"purchase_order": updated})
}

// DeletePurchaseOrder removes a draft or cancelled order with nothing received.
func (h *Handler) DeletePurchaseOrder(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	id := c.Param("id")
	order, err := h.loadPO(h.DB, orgID, id)
	if err != nil {
		erpErr(c, http.StatusNotFound, "Purchase order not found")
		return
	}
	st := erpNormalizePOStatus(order.Status)
	for _, it := range order.Items {
		if it.QuantityReceived > 0 {
			erpErr(c, http.StatusConflict, "Stock has been received against this order, so it can't be deleted")
			return
		}
	}
	if st != poDraft && st != poCancelled {
		erpErr(c, http.StatusConflict, "Only draft or cancelled purchase orders can be deleted")
		return
	}
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("purchase_order_id = ?", id).Delete(&models.PurchaseOrderItem{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ? AND organization_id = ?", id, orgID).Delete(&models.PurchaseOrder{}).Error
	})
	if err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to delete purchase order")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Purchase order deleted"})
}

func (h *Handler) UpdatePurchaseOrderStatus(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	orderID := c.Param("id")

	var req struct {
		Status string `json:"status" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		erpErr(c, http.StatusBadRequest, "Status is required")
		return
	}

	order, err := h.loadPO(h.DB, orgID, orderID)
	if err != nil {
		erpErr(c, http.StatusNotFound, "Purchase order not found")
		return
	}
	from := erpNormalizePOStatus(order.Status)
	to := erpNormalizePOStatus(req.Status)
	received := 0
	for _, it := range order.Items {
		received += it.QuantityReceived
	}

	allowed := map[string][]string{
		poDraft:     {poSent, poCancelled},
		poSent:      {poDraft, poCancelled},
		poPartial:   {poReceived}, // close the order short
		poCancelled: {poDraft},
		poReceived:  {},
	}
	valid := false
	for _, s := range allowed[from] {
		if s == to {
			valid = true
		}
	}
	if to == from {
		valid = true
	}
	if !valid {
		erpErr(c, http.StatusConflict, fmt.Sprintf("Can't change a %s order to %s", strings.ReplaceAll(from, "_", " "), strings.ReplaceAll(to, "_", " ")))
		return
	}
	if to == poCancelled && received > 0 {
		erpErr(c, http.StatusConflict, "Stock has already been received on this order")
		return
	}

	cols := map[string]interface{}{"status": to}
	if to == poReceived && order.ReceivedDate == nil {
		now := time.Now()
		cols["received_date"] = &now
	}
	if err := h.DB.Model(&models.PurchaseOrder{}).Where("id = ? AND organization_id = ?", orderID, orgID).UpdateColumns(cols).Error; err != nil {
		erpErr(c, http.StatusInternalServerError, "Failed to update status")
		return
	}
	updated, _ := h.loadPO(h.DB, orgID, orderID)
	erpJSON(c, http.StatusOK, gin.H{"message": "Status updated successfully", "purchase_order": updated})
}

// ReceivePurchaseOrder books stock in against a purchase order. Each entry in
// received_items is the quantity received in THIS delivery (not cumulative).
// With {"receive_all": true} every outstanding quantity is received.
func (h *Handler) ReceivePurchaseOrder(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}
	orderID := c.Param("id")
	userID := c.GetString("user_id")

	var req struct {
		ReceiveAll    bool   `json:"receive_all"`
		Notes         string `json:"notes"`
		ReceivedItems []struct {
			ItemID           string `json:"item_id"`
			QuantityReceived int    `json:"quantity_received"`
		} `json:"received_items"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		erpErr(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}

	var status string
	var httpErr int
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		var order models.PurchaseOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", orderID, orgID).First(&order).Error; err != nil {
			httpErr = http.StatusNotFound
			return errors.New("Purchase order not found")
		}
		st := erpNormalizePOStatus(order.Status)
		if st != poSent && st != poPartial && st != poDraft {
			httpErr = http.StatusConflict
			return fmt.Errorf("Can't receive stock on a %s order", strings.ReplaceAll(st, "_", " "))
		}
		var items []models.PurchaseOrderItem
		if err := tx.Where("purchase_order_id = ?", order.ID).Find(&items).Error; err != nil {
			return err
		}
		byID := map[string]*models.PurchaseOrderItem{}
		for i := range items {
			byID[items[i].ID] = &items[i]
		}

		qty := map[string]int{}
		if req.ReceiveAll {
			for _, it := range items {
				if rem := it.Quantity - it.QuantityReceived; rem > 0 {
					qty[it.ID] = rem
				}
			}
		} else {
			for _, r := range req.ReceivedItems {
				if r.QuantityReceived == 0 {
					continue
				}
				it, ok := byID[r.ItemID]
				if !ok {
					httpErr = http.StatusBadRequest
					return errors.New("Order line not found")
				}
				if r.QuantityReceived < 0 {
					httpErr = http.StatusBadRequest
					return errors.New("Received quantity cannot be negative")
				}
				qty[it.ID] += r.QuantityReceived
				if qty[it.ID] > it.Quantity-it.QuantityReceived {
					httpErr = http.StatusBadRequest
					return fmt.Errorf("Can't receive more than the %d outstanding on a line", it.Quantity-it.QuantityReceived)
				}
			}
		}
		if len(qty) == 0 {
			httpErr = http.StatusBadRequest
			return errors.New("Enter a quantity to receive")
		}

		notes := "Received against " + order.OrderNumber
		if strings.TrimSpace(req.Notes) != "" {
			notes += " – " + strings.TrimSpace(req.Notes)
		}
		for itemID, q := range qty {
			it := byID[itemID]
			var product models.Product
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND organization_id = ?", it.ProductID, orgID).First(&product).Error; err != nil {
				httpErr = http.StatusConflict
				return errors.New("A product on this order no longer exists")
			}
			prev := product.CurrentStock
			next := prev + q
			if err := tx.Model(&models.Product{}).Where("id = ?", product.ID).UpdateColumn("current_stock", next).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.PurchaseOrderItem{}).Where("id = ?", it.ID).UpdateColumn("quantity_received", it.QuantityReceived+q).Error; err != nil {
				return err
			}
			it.QuantityReceived += q
			mv := models.InventoryMovement{
				OrganizationID:   orgID,
				ProductID:        product.ID,
				MovementType:     "in",
				Quantity:         q,
				PreviousQuantity: prev,
				NewQuantity:      next,
				UnitCost:         it.UnitCost,
				Reference:        order.OrderNumber,
				ReferenceType:    "purchase_order",
				Notes:            notes,
				CreatedBy:        userID,
			}
			if err := tx.Omit(clause.Associations).Create(&mv).Error; err != nil {
				return err
			}
		}

		complete := true
		for _, it := range items {
			if it.QuantityReceived < it.Quantity {
				complete = false
			}
		}
		cols := map[string]interface{}{"status": poPartial}
		status = poPartial
		if complete {
			now := time.Now()
			cols["status"], cols["received_date"] = poReceived, &now
			status = poReceived
		}
		return tx.Model(&models.PurchaseOrder{}).Where("id = ?", order.ID).UpdateColumns(cols).Error
	})
	if err != nil {
		if httpErr == 0 {
			httpErr = http.StatusInternalServerError
			erpErr(c, httpErr, "Failed to receive stock")
			return
		}
		erpErr(c, httpErr, err.Error())
		return
	}

	updated, _ := h.loadPO(h.DB, orgID, orderID)
	msg := "Stock received – order fully received"
	if status == poPartial {
		msg = "Stock received – order partially received"
	}
	erpJSON(c, http.StatusOK, gin.H{"message": msg, "purchase_order": updated})
}

func (h *Handler) GetSupplierReport(c *gin.Context) {
	orgID, ok := erpOrg(h, c)
	if !ok {
		return
	}

	type topSupplier struct {
		SupplierID   string  `json:"supplier_id"`
		SupplierName string  `json:"supplier_name"`
		OrderCount   int64   `json:"order_count"`
		TotalAmount  float64 `json:"total_amount"`
	}
	var report struct {
		TotalSuppliers    int64                  `json:"total_suppliers"`
		ActiveSuppliers   int64                  `json:"active_suppliers"`
		TotalPurchases    float64                `json:"total_purchases"`
		SpendThisMonth    float64                `json:"spend_this_month"`
		OrdersThisMonth   int64                  `json:"orders_this_month"`
		OpenOrders        int64                  `json:"open_orders"`
		OpenOrderValue    float64                `json:"open_order_value"`
		OverdueDeliveries int64                  `json:"overdue_deliveries"`
		DraftOrders       int64                  `json:"draft_orders"`
		TopSuppliers      []topSupplier          `json:"top_suppliers"`
		RecentOrders      []models.PurchaseOrder `json:"recent_orders"`
	}

	h.DB.Model(&models.Supplier{}).Where("organization_id = ?", orgID).Count(&report.TotalSuppliers)
	h.DB.Model(&models.Supplier{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&report.ActiveSuppliers)

	committed := []string{poSent, "confirmed", poPartial, poReceived}
	open := []string{poSent, "confirmed", poPartial}
	// Calendar dates in the user's time zone (?tz=); order/expected dates are
	// stored as dates, so compare them as dates.
	now := reqtz.Now(c)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	today := now.Format("2006-01-02")

	h.DB.Model(&models.PurchaseOrder{}).Where("organization_id = ? AND status IN ?", orgID, committed).
		Select("COALESCE(SUM(total_amount),0)").Row().Scan(&report.TotalPurchases)
	h.DB.Model(&models.PurchaseOrder{}).Where("organization_id = ? AND status IN ? AND order_date >= ?", orgID, committed, monthStart).
		Select("COALESCE(SUM(total_amount),0)").Row().Scan(&report.SpendThisMonth)
	h.DB.Model(&models.PurchaseOrder{}).Where("organization_id = ? AND status IN ? AND order_date >= ?", orgID, committed, monthStart).Count(&report.OrdersThisMonth)
	h.DB.Model(&models.PurchaseOrder{}).Where("organization_id = ? AND status IN ?", orgID, open).Count(&report.OpenOrders)
	h.DB.Model(&models.PurchaseOrder{}).Where("organization_id = ? AND status IN ?", orgID, open).
		Select("COALESCE(SUM(total_amount),0)").Row().Scan(&report.OpenOrderValue)
	h.DB.Model(&models.PurchaseOrder{}).Where("organization_id = ? AND status IN ? AND expected_date IS NOT NULL AND expected_date < ?", orgID, open, today).Count(&report.OverdueDeliveries)
	h.DB.Model(&models.PurchaseOrder{}).Where("organization_id = ? AND status = ?", orgID, poDraft).Count(&report.DraftOrders)

	h.DB.Table("purchase_orders").
		Select("suppliers.id AS supplier_id, suppliers.name AS supplier_name, COUNT(*) AS order_count, COALESCE(SUM(purchase_orders.total_amount),0) AS total_amount").
		Joins("JOIN suppliers ON purchase_orders.supplier_id = suppliers.id").
		Where("purchase_orders.organization_id = ? AND purchase_orders.deleted_at IS NULL AND purchase_orders.status IN ?", orgID, committed).
		Group("suppliers.id, suppliers.name").
		Order("total_amount DESC").
		Limit(5).
		Scan(&report.TopSuppliers)
	if report.TopSuppliers == nil {
		report.TopSuppliers = []topSupplier{}
	}

	h.DB.Where("organization_id = ?", orgID).
		Preload("Supplier").
		Order("created_at DESC").
		Limit(10).
		Find(&report.RecentOrders)

	report.TotalPurchases = erpRound(report.TotalPurchases)
	report.SpendThisMonth = erpRound(report.SpendThisMonth)
	report.OpenOrderValue = erpRound(report.OpenOrderValue)
	erpJSON(c, http.StatusOK, gin.H{"report": report})
}
