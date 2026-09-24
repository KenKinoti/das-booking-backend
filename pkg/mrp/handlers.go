package mrp

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Handler serves /api/v1/mrp.
type Handler struct {
	DB  *gorm.DB
	Now func() time.Time
}

func NewHandler(db *gorm.DB) *Handler { return &Handler{DB: db} }

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// RegisterRoutes mounts the API on g (e.g. /api/v1/mrp).
func (h *Handler) RegisterRoutes(g *gin.RouterGroup) {
	g.GET("/summary", h.summary)
	g.GET("/boms", h.listBOMs)
	g.POST("/boms", h.createBOM)
	g.GET("/boms/:id", h.getBOM)
	g.PUT("/boms/:id", h.updateBOM)
	g.DELETE("/boms/:id", h.deleteBOM)

	g.GET("/work-orders", h.listWorkOrders)
	g.POST("/work-orders", h.createWorkOrder)
	g.GET("/work-orders/:id", h.getWorkOrder)
	g.PUT("/work-orders/:id", h.updateWorkOrder)
	g.DELETE("/work-orders/:id", h.deleteWorkOrder)
	g.POST("/work-orders/:id/start", h.startWorkOrder)
	g.POST("/work-orders/:id/complete", h.completeWorkOrder)
	g.POST("/work-orders/:id/cancel", h.cancelWorkOrder)
}

func fail(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"success": false, "error": msg, "message": msg})
}

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func ctxString(c *gin.Context, key string) string {
	v, exists := c.Get(key)
	if !exists || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func orgOf(c *gin.Context) (string, bool) {
	org := ctxString(c, "org_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "Organisation not found in token")
		return "", false
	}
	return org, true
}

func trimMax(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// products loads org products by id (including soft-deleted ones, for history).
func (h *Handler) products(db *gorm.DB, org string, ids []string) map[string]models.Product {
	out := map[string]models.Product{}
	if len(ids) == 0 {
		return out
	}
	var ps []models.Product
	db.Unscoped().Omit(clause.Associations).Where("organization_id = ? AND id IN ?", org, ids).Find(&ps)
	for _, p := range ps {
		out[p.ID] = p
	}
	return out
}

func stockAndCost(ps map[string]models.Product) (map[string]int, map[string]float64) {
	s, c := map[string]int{}, map[string]float64{}
	for id, p := range ps {
		s[id] = p.CurrentStock
		c[id] = p.CostPrice
	}
	return s, c
}

// decorateBOMs fills live stock/cost figures.
func (h *Handler) decorateBOMs(org string, boms []BOM) {
	ids := []string{}
	bomIDs := []string{}
	for _, b := range boms {
		bomIDs = append(bomIDs, b.ID)
		for _, c := range b.Components {
			ids = append(ids, c.ProductID)
		}
	}
	ps := h.products(h.DB, org, ids)
	_, cost := stockAndCost(ps)
	type cnt struct {
		BOMID string `gorm:"column:bom_id"`
		N     int64
	}
	var counts []cnt
	if len(bomIDs) > 0 {
		h.DB.Model(&WorkOrder{}).Select("bom_id, COUNT(*) AS n").Where("org_id = ? AND bom_id IN ?", org, bomIDs).Group("bom_id").Scan(&counts)
	}
	cm := map[string]int64{}
	for _, x := range counts {
		cm[x.BOMID] = x.N
	}
	for i := range boms {
		for j := range boms[i].Components {
			comp := &boms[i].Components[j]
			if p, found := ps[comp.ProductID]; found {
				comp.UnitCost = p.CostPrice
				comp.CurrentStock = p.CurrentStock
				comp.Unit = p.UnitOfMeasure
				comp.ProductName = p.Name
				comp.ProductSKU = p.SKU
			}
		}
		boms[i].UnitCost = UnitCost(boms[i].Components, boms[i].OutputQty, cost)
		boms[i].WorkOrders = cm[boms[i].ID]
	}
}

// ---------------------------------------------------------------------------
// Summary

func (h *Handler) summary(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	today := h.now().Format("2006-01-02")
	monthStart := time.Date(h.now().Year(), h.now().Month(), 1, 0, 0, 0, 0, h.now().Location())
	var planned, inProgress, overdue, boms, doneMonth int64
	var unitsMonth int64
	h.DB.Model(&WorkOrder{}).Where("org_id = ? AND status = ?", org, StatusPlanned).Count(&planned)
	h.DB.Model(&WorkOrder{}).Where("org_id = ? AND status = ?", org, StatusInProgress).Count(&inProgress)
	h.DB.Model(&WorkOrder{}).Where("org_id = ? AND status IN ? AND due_date <> '' AND due_date < ?", org, []string{StatusPlanned, StatusInProgress}, today).Count(&overdue)
	h.DB.Model(&BOM{}).Where("org_id = ?", org).Count(&boms)
	h.DB.Model(&WorkOrder{}).Where("org_id = ? AND status = ? AND completed_at >= ?", org, StatusDone, monthStart).Count(&doneMonth)
	h.DB.Model(&WorkOrder{}).Where("org_id = ? AND status = ? AND completed_at >= ?", org, StatusDone, monthStart).Select("COALESCE(SUM(quantity),0)").Scan(&unitsMonth)

	// Open orders that can't currently be built.
	open, _ := h.workOrders(org, func(q *gorm.DB) *gorm.DB {
		return q.Where("status IN ?", []string{StatusPlanned, StatusInProgress})
	})
	blocked := 0
	for _, w := range open {
		if w.Shortages > 0 {
			blocked++
		}
	}
	ok(c, http.StatusOK, gin.H{
		"planned": planned, "in_progress": inProgress, "overdue": overdue, "boms": boms,
		"done_this_month": doneMonth, "units_this_month": unitsMonth, "blocked": blocked,
	})
}

// ---------------------------------------------------------------------------
// BOMs

type componentInput struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

type bomInput struct {
	ProductID  string           `json:"product_id"`
	Name       string           `json:"name"`
	OutputQty  int              `json:"output_qty"`
	Notes      string           `json:"notes"`
	IsActive   *bool            `json:"is_active"`
	Components []componentInput `json:"components"`
}

func (h *Handler) listBOMs(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	q := h.DB.Preload("Components", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).Where("org_id = ?", org)
	if s := strings.ToLower(strings.TrimSpace(c.Query("q"))); s != "" {
		like := "%" + s + "%"
		q = q.Where("(LOWER(name) LIKE ? OR LOWER(product_name) LIKE ? OR LOWER(product_sku) LIKE ?)", like, like, like)
	}
	var rows []BOM
	if err := q.Order("name").Find(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not load bills of materials")
		return
	}
	h.decorateBOMs(org, rows)
	ok(c, http.StatusOK, gin.H{"boms": rows})
}

func (h *Handler) loadBOM(c *gin.Context, org, id string) (*BOM, bool) {
	var b BOM
	if err := h.DB.Preload("Components", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).
		Where("id = ? AND org_id = ?", id, org).First(&b).Error; err != nil {
		fail(c, http.StatusNotFound, "Bill of materials not found")
		return nil, false
	}
	list := []BOM{b}
	h.decorateBOMs(org, list)
	return &list[0], true
}

func (h *Handler) getBOM(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if b, found := h.loadBOM(c, org, c.Param("id")); found {
		ok(c, http.StatusOK, b)
	}
}

func (h *Handler) applyBOM(c *gin.Context, org string, b *BOM) ([]BOMComponent, bool) {
	var in bomInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return nil, false
	}
	if in.ProductID == "" {
		fail(c, http.StatusBadRequest, "Choose the product this recipe makes")
		return nil, false
	}
	if in.OutputQty <= 0 {
		in.OutputQty = 1
	}
	if in.OutputQty > 1_000_000 {
		fail(c, http.StatusBadRequest, "Output quantity is too large")
		return nil, false
	}
	if len(in.Components) == 0 {
		fail(c, http.StatusBadRequest, "Add at least one component")
		return nil, false
	}
	ids := []string{in.ProductID}
	for i, comp := range in.Components {
		if comp.ProductID == "" {
			fail(c, http.StatusBadRequest, fmt.Sprintf("Choose a product for component %d", i+1))
			return nil, false
		}
		if comp.ProductID == in.ProductID {
			fail(c, http.StatusBadRequest, "A product can't be a component of itself")
			return nil, false
		}
		if comp.Quantity <= 0 || comp.Quantity > 1_000_000 {
			fail(c, http.StatusBadRequest, fmt.Sprintf("Component %d needs a quantity of at least 1", i+1))
			return nil, false
		}
		ids = append(ids, comp.ProductID)
	}
	var ps []models.Product
	h.DB.Omit(clause.Associations).Where("organization_id = ? AND id IN ?", org, ids).Find(&ps)
	pm := map[string]models.Product{}
	for _, p := range ps {
		pm[p.ID] = p
	}
	out, found := pm[in.ProductID]
	if !found {
		fail(c, http.StatusBadRequest, "Finished product not found in inventory")
		return nil, false
	}
	comps := make([]BOMComponent, 0, len(in.Components))
	for i, comp := range in.Components {
		p, found := pm[comp.ProductID]
		if !found {
			fail(c, http.StatusBadRequest, fmt.Sprintf("Component %d isn't in your inventory", i+1))
			return nil, false
		}
		comps = append(comps, BOMComponent{ProductID: p.ID, ProductName: p.Name, ProductSKU: p.SKU, Quantity: comp.Quantity, Position: i})
	}
	b.ProductID = out.ID
	b.ProductName = out.Name
	b.ProductSKU = out.SKU
	b.Name = trimMax(in.Name, 160)
	if b.Name == "" {
		b.Name = out.Name
	}
	b.OutputQty = in.OutputQty
	b.Notes = trimMax(in.Notes, 5000)
	if in.IsActive != nil {
		b.IsActive = *in.IsActive
	}
	return comps, true
}

func (h *Handler) createBOM(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	b := BOM{OrgID: org, IsActive: true}
	comps, valid := h.applyBOM(c, org, &b)
	if !valid {
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Components").Create(&b).Error; err != nil {
			return err
		}
		for i := range comps {
			comps[i].BOMID = b.ID
		}
		return tx.Create(&comps).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not save bill of materials")
		return
	}
	if res, found := h.loadBOM(c, org, b.ID); found {
		ok(c, http.StatusCreated, res)
	}
}

func (h *Handler) updateBOM(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var b BOM
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&b).Error; err != nil {
		fail(c, http.StatusNotFound, "Bill of materials not found")
		return
	}
	comps, valid := h.applyBOM(c, org, &b)
	if !valid {
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Components").Save(&b).Error; err != nil {
			return err
		}
		if err := tx.Where("bom_id = ?", b.ID).Delete(&BOMComponent{}).Error; err != nil {
			return err
		}
		for i := range comps {
			comps[i].BOMID = b.ID
		}
		return tx.Create(&comps).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not save bill of materials")
		return
	}
	if res, found := h.loadBOM(c, org, b.ID); found {
		ok(c, http.StatusOK, res)
	}
}

func (h *Handler) deleteBOM(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var b BOM
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&b).Error; err != nil {
		fail(c, http.StatusNotFound, "Bill of materials not found")
		return
	}
	var open int64
	h.DB.Model(&WorkOrder{}).Where("bom_id = ? AND status IN ?", b.ID, []string{StatusPlanned, StatusInProgress}).Count(&open)
	if open > 0 {
		fail(c, http.StatusConflict, fmt.Sprintf("%d open work order(s) use this recipe. Finish or cancel them first.", open))
		return
	}
	// Soft delete keeps history for completed work orders.
	if err := h.DB.Delete(&b).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not delete bill of materials")
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

// ---------------------------------------------------------------------------
// Work orders

type workOrderInput struct {
	BOMID    string `json:"bom_id"`
	Quantity int    `json:"quantity"`
	DueDate  string `json:"due_date"`
	Notes    string `json:"notes"`
}

// workOrders loads work orders with their live requirements.
func (h *Handler) workOrders(org string, scope func(*gorm.DB) *gorm.DB) ([]WorkOrder, error) {
	var rows []WorkOrder
	q := h.DB.Where("org_id = ?", org)
	if scope != nil {
		q = scope(q)
	}
	if err := q.Order("CASE status WHEN 'in_progress' THEN 0 WHEN 'planned' THEN 1 WHEN 'done' THEN 2 ELSE 3 END, due_date, created_at DESC").Limit(500).Find(&rows).Error; err != nil {
		return nil, err
	}
	bomIDs := []string{}
	for _, w := range rows {
		bomIDs = append(bomIDs, w.BOMID)
	}
	var boms []BOM
	if len(bomIDs) > 0 {
		h.DB.Unscoped().Preload("Components").Where("id IN ?", bomIDs).Find(&boms)
	}
	bm := map[string]BOM{}
	pids := []string{}
	for _, b := range boms {
		bm[b.ID] = b
		for _, c := range b.Components {
			pids = append(pids, c.ProductID)
		}
	}
	ps := h.products(h.DB, org, pids)
	stock, cost := stockAndCost(ps)
	today := h.now().Format("2006-01-02")
	for i := range rows {
		w := &rows[i]
		b := bm[w.BOMID]
		w.BOMName = b.Name
		comps := append([]BOMComponent(nil), b.Components...)
		for j := range comps {
			if p, found := ps[comps[j].ProductID]; found {
				comps[j].ProductName, comps[j].ProductSKU, comps[j].Unit = p.Name, p.SKU, p.UnitOfMeasure
			}
		}
		w.Requirements = Requirements(comps, b.OutputQty, w.Quantity, stock, cost)
		open := w.Status == StatusPlanned || w.Status == StatusInProgress
		for _, r := range w.Requirements {
			if r.Shortage > 0 && open {
				w.Shortages++
			}
		}
		w.IsOverdue = open && w.DueDate != "" && w.DueDate < today
	}
	return rows, nil
}

func (h *Handler) listWorkOrders(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	status := c.Query("status")
	search := strings.ToLower(strings.TrimSpace(c.Query("q")))
	rows, err := h.workOrders(org, func(q *gorm.DB) *gorm.DB {
		switch status {
		case "", "all":
		case "open":
			q = q.Where("status IN ?", []string{StatusPlanned, StatusInProgress})
		default:
			q = q.Where("status = ?", status)
		}
		if search != "" {
			like := "%" + search + "%"
			q = q.Where("(LOWER(number) LIKE ? OR LOWER(product_name) LIKE ?)", like, like)
		}
		return q
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not load work orders")
		return
	}
	ok(c, http.StatusOK, gin.H{"work_orders": rows})
}

func (h *Handler) loadWorkOrder(c *gin.Context, org string) (*WorkOrder, bool) {
	id := c.Param("id")
	rows, err := h.workOrders(org, func(q *gorm.DB) *gorm.DB { return q.Where("id = ?", id) })
	if err != nil || len(rows) == 0 {
		fail(c, http.StatusNotFound, "Work order not found")
		return nil, false
	}
	return &rows[0], true
}

func (h *Handler) getWorkOrder(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if w, found := h.loadWorkOrder(c, org); found {
		ok(c, http.StatusOK, w)
	}
}

func (h *Handler) applyWorkOrder(c *gin.Context, org string, w *WorkOrder) bool {
	var in workOrderInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return false
	}
	var b BOM
	if in.BOMID == "" || h.DB.Where("id = ? AND org_id = ?", in.BOMID, org).First(&b).Error != nil {
		fail(c, http.StatusBadRequest, "Choose a bill of materials")
		return false
	}
	if in.Quantity <= 0 || in.Quantity > 1_000_000 {
		fail(c, http.StatusBadRequest, "Quantity must be at least 1")
		return false
	}
	due := strings.TrimSpace(in.DueDate)
	if due != "" {
		if _, err := time.Parse("2006-01-02", due); err != nil {
			fail(c, http.StatusBadRequest, "Due date must be YYYY-MM-DD")
			return false
		}
	}
	w.BOMID = b.ID
	w.ProductID = b.ProductID
	w.ProductName = b.ProductName
	w.Quantity = in.Quantity
	w.DueDate = due
	w.Notes = trimMax(in.Notes, 5000)
	return true
}

func (h *Handler) nextNumber(tx *gorm.DB, org string) string {
	var n int64
	tx.Model(&WorkOrder{}).Where("org_id = ?", org).Count(&n)
	for {
		n++
		num := fmt.Sprintf("WO-%04d", n)
		var dup int64
		tx.Model(&WorkOrder{}).Where("org_id = ? AND number = ?", org, num).Count(&dup)
		if dup == 0 {
			return num
		}
	}
}

func (h *Handler) createWorkOrder(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	w := WorkOrder{OrgID: org, Status: StatusPlanned, CreatedBy: ctxString(c, "user_id")}
	if !h.applyWorkOrder(c, org, &w) {
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		w.Number = h.nextNumber(tx, org)
		return tx.Create(&w).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not create work order")
		return
	}
	c.Params = append(c.Params, gin.Param{Key: "id", Value: w.ID})
	if res, found := h.loadWorkOrder(c, org); found {
		ok(c, http.StatusCreated, res)
	}
}

func (h *Handler) updateWorkOrder(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var w WorkOrder
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&w).Error; err != nil {
		fail(c, http.StatusNotFound, "Work order not found")
		return
	}
	if w.Status != StatusPlanned && w.Status != StatusInProgress {
		fail(c, http.StatusConflict, "Finished or cancelled work orders can't be edited")
		return
	}
	if !h.applyWorkOrder(c, org, &w) {
		return
	}
	if err := h.DB.Save(&w).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save work order")
		return
	}
	if res, found := h.loadWorkOrder(c, org); found {
		ok(c, http.StatusOK, res)
	}
}

func (h *Handler) deleteWorkOrder(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var w WorkOrder
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&w).Error; err != nil {
		fail(c, http.StatusNotFound, "Work order not found")
		return
	}
	if w.Status == StatusDone || w.Status == StatusInProgress {
		fail(c, http.StatusConflict, "Only planned or cancelled work orders can be deleted")
		return
	}
	h.DB.Delete(&w)
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) transition(c *gin.Context, from []string, to string) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var w WorkOrder
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&w).Error; err != nil {
		fail(c, http.StatusNotFound, "Work order not found")
		return
	}
	allowed := false
	for _, f := range from {
		if w.Status == f {
			allowed = true
		}
	}
	if !allowed {
		fail(c, http.StatusConflict, fmt.Sprintf("A %s work order can't be moved to %s", strings.ReplaceAll(w.Status, "_", " "), strings.ReplaceAll(to, "_", " ")))
		return
	}
	upd := map[string]interface{}{"status": to}
	if to == StatusInProgress {
		upd["started_at"] = h.now()
	}
	h.DB.Model(&WorkOrder{}).Where("id = ?", w.ID).Updates(upd)
	if res, found := h.loadWorkOrder(c, org); found {
		ok(c, http.StatusOK, res)
	}
}

func (h *Handler) startWorkOrder(c *gin.Context) {
	h.transition(c, []string{StatusPlanned}, StatusInProgress)
}

func (h *Handler) cancelWorkOrder(c *gin.Context) {
	h.transition(c, []string{StatusPlanned, StatusInProgress}, StatusCancelled)
}

// ShortageError lists components without enough stock.
type ShortageError struct{ Items []Requirement }

func (e *ShortageError) Error() string {
	parts := []string{}
	for _, r := range e.Items {
		parts = append(parts, fmt.Sprintf("%s (need %d, have %d)", r.ProductName, r.Required, r.Available))
	}
	return "Not enough stock: " + strings.Join(parts, ", ")
}

// Complete consumes components and adds finished goods in one transaction,
// writing inventory movements for each change.
func (h *Handler) Complete(org, woID, userID string) (*WorkOrder, error) {
	var done WorkOrder
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		var w WorkOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND org_id = ?", woID, org).First(&w).Error; err != nil {
			return gorm.ErrRecordNotFound
		}
		if w.Status != StatusPlanned && w.Status != StatusInProgress {
			return fmt.Errorf("conflict: this work order is already %s", strings.ReplaceAll(w.Status, "_", " "))
		}
		var b BOM
		if err := tx.Unscoped().Preload("Components").Where("id = ?", w.BOMID).First(&b).Error; err != nil {
			return errors.New("conflict: the bill of materials for this work order no longer exists")
		}
		ids := []string{b.ProductID}
		for _, comp := range b.Components {
			ids = append(ids, comp.ProductID)
		}
		var ps []models.Product
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Omit(clause.Associations).
			Where("organization_id = ? AND id IN ?", org, ids).Order("id").Find(&ps).Error; err != nil {
			return err
		}
		pm := map[string]models.Product{}
		for _, p := range ps {
			pm[p.ID] = p
		}
		if _, found := pm[b.ProductID]; !found {
			return errors.New("conflict: the finished product no longer exists in inventory")
		}
		for i := range b.Components {
			if p, found := pm[b.Components[i].ProductID]; found {
				b.Components[i].ProductName = p.Name
			} else {
				return fmt.Errorf("conflict: component %s no longer exists in inventory", b.Components[i].ProductName)
			}
		}
		stock, cost := stockAndCost(pm)
		reqs := Requirements(b.Components, b.OutputQty, w.Quantity, stock, cost)
		short := []Requirement{}
		for _, r := range reqs {
			if r.Shortage > 0 {
				short = append(short, r)
			}
		}
		if len(short) > 0 {
			return &ShortageError{Items: short}
		}
		move := func(p models.Product, qty int, typ, note string, unitCost float64) error {
			next := p.CurrentStock + qty
			if err := tx.Model(&models.Product{}).Where("id = ?", p.ID).UpdateColumn("current_stock", next).Error; err != nil {
				return err
			}
			m := models.InventoryMovement{
				OrganizationID: org, ProductID: p.ID, MovementType: typ, Quantity: qty,
				PreviousQuantity: p.CurrentStock, NewQuantity: next, UnitCost: unitCost,
				Reference: w.Number, ReferenceType: "work_order", Notes: note, CreatedBy: userID,
			}
			if err := tx.Omit(clause.Associations).Create(&m).Error; err != nil {
				return err
			}
			p.CurrentStock = next
			pm[p.ID] = p
			return nil
		}
		for _, r := range reqs {
			if r.Required == 0 {
				continue
			}
			if err := move(pm[r.ProductID], -r.Required, "out", "Consumed by "+w.Number, r.UnitCost); err != nil {
				return err
			}
		}
		unit := UnitCost(b.Components, b.OutputQty, cost)
		if err := move(pm[b.ProductID], w.Quantity, "in", "Produced by "+w.Number, unit); err != nil {
			return err
		}
		now := h.now()
		upd := map[string]interface{}{"status": StatusDone, "completed_at": now}
		if w.StartedAt == nil {
			upd["started_at"] = now
		}
		if err := tx.Model(&WorkOrder{}).Where("id = ?", w.ID).Updates(upd).Error; err != nil {
			return err
		}
		done = w
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &done, nil
}

func (h *Handler) completeWorkOrder(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	_, err := h.Complete(org, c.Param("id"), ctxString(c, "user_id"))
	if err != nil {
		var se *ShortageError
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			fail(c, http.StatusNotFound, "Work order not found")
		case errors.As(err, &se):
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": se.Error(), "message": se.Error(), "shortages": se.Items})
		case strings.HasPrefix(err.Error(), "conflict: "):
			msg := strings.TrimPrefix(err.Error(), "conflict: ")
			fail(c, http.StatusConflict, strings.ToUpper(msg[:1])+msg[1:])
		default:
			fail(c, http.StatusInternalServerError, "Could not complete work order")
		}
		return
	}
	if res, found := h.loadWorkOrder(c, org); found {
		ok(c, http.StatusOK, res)
	}
}
