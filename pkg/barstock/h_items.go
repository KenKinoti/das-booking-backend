package barstock

import (
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// ItemInput creates or edits a stock item. Pointer fields left out keep
// their value on edit.
type ItemInput struct {
	ProductID           string     `json:"product_id"` // flag an existing product instead of creating one
	Name                string     `json:"name"`
	SKU                 *string    `json:"sku"`
	Barcode             *string    `json:"barcode"`
	Category            string     `json:"category"`
	BaseUnit            string     `json:"base_unit"`
	UnitSize            *float64   `json:"unit_size"`
	UnitLabel           string     `json:"unit_label"`
	PackSize            *int       `json:"pack_size"`
	PackLabel           string     `json:"pack_label"`
	TotML               *float64   `json:"tot_ml"`
	FullWeightG         *float64   `json:"full_weight_g"`
	EmptyWeightG        *float64   `json:"empty_weight_g"`
	PreferredSupplierID *string    `json:"preferred_supplier_id"`
	Track               *bool      `json:"track"`
	Active              *bool      `json:"active"`
	SellingPrice        *float64   `json:"selling_price"`
	CatalogKey          string     `json:"catalog_key"`
	SelfRecipe          bool       `json:"sold_as_is"` // also sell the item itself in POS (1:1 recipe)
	Pars                []ParInput `json:"pars"`
}

// ParInput is a par level / reorder point (stock units) at a location.
type ParInput struct {
	LocationID   string  `json:"location_id"`
	ProductID    string  `json:"product_id"`
	Par          float64 `json:"par"`
	ReorderPoint float64 `json:"reorder_point"`
}

func defaultBaseUnit(cat string) string {
	switch cat {
	case "spirits", "wine":
		return BaseML
	}
	return BaseUnit
}

// ensureCategory returns the product category id named label (created if missing).
func ensureCategory(tx *gorm.DB, org, label string) *string {
	if label == "" {
		return nil
	}
	var cat models.ProductCategory
	if err := tx.Where("organization_id = ? AND LOWER(name) = LOWER(?) AND deleted_at IS NULL", org, label).Limit(1).Find(&cat).Error; err == nil && cat.ID != "" {
		return &cat.ID
	}
	cat = models.ProductCategory{ID: uuid.NewString(), OrganizationID: org, Name: label, IsActive: true}
	if err := tx.Omit(clause.Associations).Create(&cat).Error; err != nil {
		return nil
	}
	return &cat.ID
}

func isUniqueViolation(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "23505") || strings.Contains(strings.ToLower(err.Error()), "duplicate key"))
}

// validateItem checks and normalises the unit fields of it.
func (s *Service) validateItem(tx *gorm.DB, org string, it *Item) error {
	if it.Category == "" {
		it.Category = "other"
	}
	if !validCategory(it.Category) {
		return badReq("Category must be one of %s", strings.Join(Categories, ", "))
	}
	if it.BaseUnit == "" {
		it.BaseUnit = defaultBaseUnit(it.Category)
	}
	if it.BaseUnit != BaseML && it.BaseUnit != BaseUnit {
		return badReq("Base unit must be ml or unit")
	}
	if it.UnitSize <= 0 {
		if it.BaseUnit == BaseML {
			return badReq("Enter the bottle size in ml (e.g. 750)")
		}
		it.UnitSize = 1
	}
	if it.UnitSize > 1000000 {
		return badReq("Unit size is too large")
	}
	it.UnitSize = round3(it.UnitSize)
	if strings.TrimSpace(it.UnitLabel) == "" {
		if it.BaseUnit == BaseML {
			it.UnitLabel = "bottle"
		} else {
			it.UnitLabel = "unit"
		}
	}
	it.UnitLabel = strings.TrimSpace(it.UnitLabel)
	if it.PackSize < 1 {
		it.PackSize = 1
	}
	if it.PackSize > 10000 {
		return badReq("Pack size is too large")
	}
	if strings.TrimSpace(it.PackLabel) == "" {
		if it.PackSize > 1 {
			it.PackLabel = "case"
		} else {
			it.PackLabel = it.UnitLabel
		}
	}
	it.PackLabel = strings.TrimSpace(it.PackLabel)
	if it.TotML != nil {
		if *it.TotML <= 0 || *it.TotML > it.UnitSize {
			return badReq("Tot size must be more than 0 and at most the bottle size")
		}
		if it.BaseUnit != BaseML {
			it.TotML = nil
		}
	}
	if (it.FullWeightG == nil) != (it.EmptyWeightG == nil) {
		return badReq("Enter both the full and the empty weight (or neither)")
	}
	if it.FullWeightG != nil {
		if *it.EmptyWeightG < 0 || *it.FullWeightG <= *it.EmptyWeightG {
			return badReq("The full weight must be more than the empty weight")
		}
	}
	if it.PreferredSupplierID != nil {
		v := strings.TrimSpace(*it.PreferredSupplierID)
		if v == "" {
			it.PreferredSupplierID = nil
		} else {
			var n int64
			tx.Table("suppliers").Where("id = ? AND organization_id = ? AND deleted_at IS NULL", v, org).Count(&n)
			if n == 0 {
				return badReq("Supplier not found")
			}
			it.PreferredSupplierID = &v
		}
	}
	return nil
}

func (s *Service) createItem(tx *gorm.DB, org, user string, in ItemInput) (*Item, error) {
	it := Item{OrganizationID: org, Category: strings.ToLower(strings.TrimSpace(in.Category)), BaseUnit: strings.ToLower(strings.TrimSpace(in.BaseUnit)),
		UnitLabel: in.UnitLabel, PackLabel: in.PackLabel, TotML: in.TotML, FullWeightG: in.FullWeightG, EmptyWeightG: in.EmptyWeightG,
		PreferredSupplierID: in.PreferredSupplierID, Track: true, Active: true, CatalogKey: in.CatalogKey}
	if in.UnitSize != nil {
		it.UnitSize = *in.UnitSize
	}
	if in.PackSize != nil {
		it.PackSize = *in.PackSize
	}
	if in.Track != nil {
		it.Track = *in.Track
	}
	if in.Active != nil {
		it.Active = *in.Active
	}
	if err := s.validateItem(tx, org, &it); err != nil {
		return nil, err
	}
	if in.SellingPrice != nil && *in.SellingPrice < 0 {
		return nil, badReq("Selling price cannot be negative")
	}
	opening := 0.0 // legacy stock of an existing product becomes opening stock in the main store
	openingCost := 0.0
	if in.ProductID != "" {
		var p models.Product
		if err := tx.Where("id = ? AND organization_id = ?", in.ProductID, org).First(&p).Error; err != nil {
			return nil, notFound("Product not found")
		}
		if p.CurrentStock > 0 && it.Track {
			opening, openingCost = float64(p.CurrentStock), p.CostPrice
		}
		var n int64
		tx.Model(&Item{}).Where("product_id = ?", p.ID).Count(&n)
		if n > 0 {
			return nil, conflict("%s is already a stock item", p.Name)
		}
		var r int64
		tx.Model(&Recipe{}).Where("pos_product_id = ? AND kind <> 'self'", p.ID).Count(&r)
		if r > 0 {
			return nil, conflict("%s is a menu item with a recipe; it can't also be a stock item", p.Name)
		}
		it.ProductID = p.ID
		upd := map[string]any{"unit_of_measure": uomFor(it.BaseUnit)}
		if in.SellingPrice != nil {
			upd["selling_price"] = math.Round(*in.SellingPrice*100) / 100
		}
		if p.CategoryID == nil {
			upd["category_id"] = ensureCategory(tx, org, CategoryLabels[it.Category])
		}
		if err := tx.Model(&models.Product{}).Where("id = ?", p.ID).Updates(upd).Error; err != nil {
			return nil, err
		}
	} else {
		name := strings.TrimSpace(in.Name)
		if name == "" {
			return nil, badReq("Enter a name")
		}
		p := models.Product{ID: uuid.NewString(), OrganizationID: org, Name: name, UnitOfMeasure: uomFor(it.BaseUnit), IsActive: true,
			CategoryID: ensureCategory(tx, org, CategoryLabels[it.Category])}
		if in.SKU != nil {
			p.SKU = strings.TrimSpace(*in.SKU)
		}
		if in.Barcode != nil {
			p.Barcode = strings.TrimSpace(*in.Barcode)
		}
		if in.SellingPrice != nil {
			p.SellingPrice = math.Round(*in.SellingPrice*100) / 100
		}
		if err := tx.Omit(clause.Associations).Create(&p).Error; err != nil {
			if isUniqueViolation(err) {
				return nil, conflict("That SKU is already used by another product")
			}
			return nil, err
		}
		it.ProductID = p.ID
	}
	it.CreatedAt, it.UpdatedAt = s.now(), s.now()
	if opening > 0 && it.UnitSize > 0 {
		it.AvgCost = money6(openingCost / it.UnitSize)
		it.LastCost = openingCost
	}
	if err := tx.Create(&it).Error; err != nil {
		return nil, err
	}
	keepStock := false
	if opening > 0 {
		if store, err := s.mainStore(tx, org); err != nil {
			keepStock = true // no store yet: keep the product's stock figure as it is
		} else if _, err := s.post(tx, org, mv{Loc: store, Product: it.ProductID, Qty: round3(opening * it.UnitSize), Kind: KindOpening, RefType: "opening",
			RefID: it.ProductID, Note: "Opening stock from Inventory", UnitCost: it.AvgCost, User: user, BD: s.BusinessDateAt(tx, org, s.now())}); err != nil {
			return nil, err
		}
	}
	if err := s.savePars(tx, org, it.ProductID, in.Pars); err != nil {
		return nil, err
	}
	if in.SelfRecipe {
		if _, err := s.ensureSelfRecipe(tx, org, it.ProductID); err != nil {
			return nil, err
		}
	}
	if keepStock {
		return &it, nil
	}
	return &it, s.sync(tx, org, []string{it.ProductID})
}

func uomFor(base string) string {
	if base == BaseML {
		return "ml"
	}
	return "each"
}

func (s *Service) savePars(tx *gorm.DB, org, productID string, pars []ParInput) error {
	for _, p := range pars {
		pid := p.ProductID
		if productID != "" {
			pid = productID
		}
		if pid == "" {
			return badReq("Each par line needs a product")
		}
		if p.Par < 0 || p.ReorderPoint < 0 {
			return badReq("Par levels cannot be negative")
		}
		if _, err := s.location(tx, org, p.LocationID); err != nil {
			return err
		}
		var n int64
		tx.Model(&Item{}).Where("organization_id = ? AND product_id = ?", org, pid).Count(&n)
		if n == 0 {
			return badReq("Stock item not found")
		}
		row := Par{OrganizationID: org, LocationID: p.LocationID, ProductID: pid, Par: round3(p.Par), ReorderPoint: round3(p.ReorderPoint), UpdatedAt: s.now()}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "organization_id"}, {Name: "location_id"}, {Name: "product_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"par", "reorder_point", "updated_at"})}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) updateItem(tx *gorm.DB, org, id string, in ItemInput) error {
	var it Item
	if err := tx.Where("organization_id = ? AND product_id = ?", org, id).First(&it).Error; err != nil {
		return notFound("Stock item not found")
	}
	oldSize := it.UnitSize
	if in.Category != "" {
		it.Category = strings.ToLower(strings.TrimSpace(in.Category))
	}
	if in.BaseUnit != "" && in.BaseUnit != it.BaseUnit {
		var n int64
		tx.Model(&Movement{}).Where("organization_id = ? AND product_id = ?", org, id).Count(&n)
		if n > 0 {
			return conflict("The base unit can't change once stock has moved")
		}
		it.BaseUnit = strings.ToLower(strings.TrimSpace(in.BaseUnit))
	}
	if in.UnitSize != nil {
		it.UnitSize = *in.UnitSize
	}
	if in.UnitLabel != "" {
		it.UnitLabel = in.UnitLabel
	}
	if in.PackSize != nil {
		it.PackSize = *in.PackSize
	}
	if in.PackLabel != "" {
		it.PackLabel = in.PackLabel
	}
	if in.TotML != nil {
		if *in.TotML <= 0 {
			it.TotML = nil
		} else {
			it.TotML = in.TotML
		}
	}
	if in.FullWeightG != nil || in.EmptyWeightG != nil {
		if (in.FullWeightG != nil && *in.FullWeightG <= 0) && (in.EmptyWeightG == nil || *in.EmptyWeightG <= 0) {
			it.FullWeightG, it.EmptyWeightG = nil, nil
		} else {
			if in.FullWeightG != nil {
				it.FullWeightG = in.FullWeightG
			}
			if in.EmptyWeightG != nil {
				it.EmptyWeightG = in.EmptyWeightG
			}
		}
	}
	if in.PreferredSupplierID != nil {
		it.PreferredSupplierID = in.PreferredSupplierID
	}
	if in.Track != nil {
		it.Track = *in.Track
	}
	if in.Active != nil {
		it.Active = *in.Active
	}
	if err := s.validateItem(tx, org, &it); err != nil {
		return err
	}
	it.UpdatedAt = s.now()
	if err := tx.Model(&Item{}).Where("product_id = ? AND organization_id = ?", id, org).Select("*").Omit("product_id", "organization_id", "created_at", "avg_cost", "last_cost", "last_supplier_id", "catalog_key").Updates(&it).Error; err != nil {
		return err
	}
	upd := map[string]any{"unit_of_measure": uomFor(it.BaseUnit), "category_id": ensureCategory(tx, org, CategoryLabels[it.Category])}
	if strings.TrimSpace(in.Name) != "" {
		upd["name"] = strings.TrimSpace(in.Name)
	}
	if in.SKU != nil && strings.TrimSpace(*in.SKU) != "" {
		upd["sku"] = strings.TrimSpace(*in.SKU)
	}
	if in.Barcode != nil {
		upd["barcode"] = strings.TrimSpace(*in.Barcode)
	}
	if in.SellingPrice != nil {
		if *in.SellingPrice < 0 {
			return badReq("Selling price cannot be negative")
		}
		upd["selling_price"] = math.Round(*in.SellingPrice*100) / 100
	}
	if in.Active != nil {
		upd["is_active"] = *in.Active
	}
	if err := tx.Model(&models.Product{}).Where("id = ? AND organization_id = ?", id, org).Updates(upd).Error; err != nil {
		if isUniqueViolation(err) {
			return conflict("That SKU is already used by another product")
		}
		return err
	}
	if err := s.savePars(tx, org, id, in.Pars); err != nil {
		return err
	}
	if it.UnitSize != oldSize {
		// "One bottle" recipes follow the new size.
		if err := tx.Exec(`UPDATE bar_recipe_lines SET qty = ? WHERE organization_id = ? AND product_id = ? AND unit IN ('bottle','unit') AND entered_qty = 1
			AND pos_product_id IN (SELECT pos_product_id FROM bar_recipes WHERE organization_id = ? AND source_id = ? AND kind IN ('bottle','self'))`,
			it.UnitSize, org, id, org, id).Error; err != nil {
			return err
		}
	}
	if in.SelfRecipe {
		if _, err := s.ensureSelfRecipe(tx, org, id); err != nil {
			return err
		}
	}
	return s.sync(tx, org, []string{id})
}

// ---------------------------------------------------------------- views

type parKey struct{ loc, prod string }

func (s *Service) pars(db *gorm.DB, org string) map[parKey]Par {
	var rows []Par
	db.Where("organization_id = ?", org).Find(&rows)
	out := map[parKey]Par{}
	for _, r := range rows {
		out[parKey{r.LocationID, r.ProductID}] = r
	}
	return out
}

func (s *Service) supplierNames(db *gorm.DB, org string) map[string]string {
	type r struct{ ID, Name string }
	var rows []r
	db.Table("suppliers").Select("id, name").Where("organization_id = ? AND deleted_at IS NULL", org).Scan(&rows)
	out := map[string]string{}
	for _, x := range rows {
		out[x.ID] = x.Name
	}
	return out
}

// taxInfo is the organisation's default VAT rate and whether menu prices include it.
func (s *Service) taxInfo(db *gorm.DB, org string) (rate float64, inclusive bool) {
	var ok bool
	if db.Raw("SELECT to_regclass('billing_settings') IS NOT NULL").Row().Scan(&ok); !ok {
		return 0, false
	}
	var row struct {
		DefaultTaxRate   float64
		PricesIncludeTax bool
	}
	db.Raw("SELECT COALESCE(default_tax_rate,0) AS default_tax_rate, COALESCE(prices_include_tax,false) AS prices_include_tax FROM billing_settings WHERE organization_id = ? LIMIT 1", org).Scan(&row)
	return row.DefaultTaxRate, row.PricesIncludeTax
}

// netPrice is a menu price without VAT.
func netPrice(price, rate float64, inclusive bool) float64 {
	if inclusive && rate > 0 {
		return price / (1 + rate/100)
	}
	return price
}

type itemCtx struct {
	st        Settings
	cur       string
	suppliers map[string]string
	pars      map[parKey]Par
	rate      float64
	inclusive bool
}

func (s *Service) newItemCtx(db *gorm.DB, org string) itemCtx {
	r, inc := s.taxInfo(db, org)
	return itemCtx{st: s.Settings(db, org), cur: s.Currency(db, org), suppliers: s.supplierNames(db, org), pars: s.pars(db, org), rate: r, inclusive: inc}
}

// parFor sums par / reorder points over the given locations (all when nil).
func (x itemCtx) parFor(pid string, locs map[string]bool) (par, rp float64, has bool) {
	for k, p := range x.pars {
		if k.prod != pid || (locs != nil && !locs[k.loc]) {
			continue
		}
		par += p.Par
		rp += p.ReorderPoint
		has = true
	}
	return
}

func (s *Service) itemView(it *ItemRow, x itemCtx, onHand float64, par, rp float64) gin.H {
	sp := SpecOf(&it.Item, x.st)
	units := sp.StockUnits(onHand)
	low := it.Track && (onHand <= 0 || (rp > 0 && units <= rp+eps))
	var sup any
	if it.PreferredSupplierID != nil {
		sup = gin.H{"id": *it.PreferredSupplierID, "name": x.suppliers[*it.PreferredSupplierID]}
	}
	v := gin.H{
		"product_id": it.ProductID, "name": it.Name, "sku": it.SKU, "barcode": it.Barcode, "category": it.Category,
		"category_label": CategoryLabels[it.Category], "base_unit": it.BaseUnit, "unit_size": it.UnitSize, "unit_label": it.UnitLabel,
		"pack_size": it.PackSize, "pack_label": it.PackLabel, "tot_ml": sp.TotML, "tot_ml_override": it.TotML,
		"full_weight_g": it.FullWeightG, "empty_weight_g": it.EmptyWeightG, "track": it.Track, "active": it.Active && it.IsActive,
		"selling_price": it.SellingPrice, "catalog_key": it.CatalogKey,
		"on_hand": round3(onHand), "on_hand_units": round3(units), "on_hand_text": sp.Text(onHand),
		"par": round3(par), "reorder_point": round3(rp), "low": low, "out": it.Track && onHand <= 0,
		"preferred_supplier_id": it.PreferredSupplierID, "preferred_supplier": sup,
		"avg_cost": money6(it.AvgCost), "avg_cost_per_unit": RoundMoney(it.AvgCost*it.UnitSize, x.cur),
		"value": RoundMoney(math.Max(onHand, 0)*it.AvgCost, x.cur), "last_cost": RoundMoney(it.LastCost, x.cur),
	}
	if it.BaseUnit == BaseML && sp.TotML > 0 {
		v["tots_per_unit"] = round3(it.UnitSize / sp.TotML)
		v["on_hand_tots"] = round3(onHand / sp.TotML)
		if t := x.st.PourCostTargets[it.Category]; t != nil && *t > 0 && it.AvgCost > 0 {
			gross := func(net float64) float64 {
				if x.inclusive {
					return net * (1 + x.rate/100)
				}
				return net
			}
			v["suggested_tot_price"] = RoundMoney(gross(it.AvgCost*sp.TotML/(*t/100)), x.cur)
			v["suggested_bottle_price"] = RoundMoney(gross(it.AvgCost*it.UnitSize/(*t/100)), x.cur)
		}
	}
	return v
}

func (h *Handler) listItems(c *gin.Context) {
	a := h.access(c)
	db := h.S.DB
	loc := strings.TrimSpace(c.Query("location_id"))
	if loc != "" {
		if err := a.needLoc(loc); err != nil {
			h.fail(c, err)
			return
		}
	}
	q := itemQuery(db, a.Org)
	if cat := strings.TrimSpace(c.Query("category")); cat != "" {
		q = q.Where("bar_items.category = ?", cat)
	}
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		like := "%" + s + "%"
		q = q.Where("(products.name ILIKE ? OR products.sku ILIKE ? OR products.barcode ILIKE ?)", like, like, like)
	}
	if v := c.Query("active"); v == "0" || v == "false" {
		q = q.Where("NOT (bar_items.active AND COALESCE(products.is_active,true))")
	} else if v != "all" {
		q = q.Where("bar_items.active AND COALESCE(products.is_active,true)")
	}
	var rows []ItemRow
	if err := q.Order("bar_items.category, products.name").Scan(&rows).Error; err != nil {
		h.fail(c, err)
		return
	}
	x := h.S.newItemCtx(db, a.Org)
	byLoc, err := h.S.stockByLocation(db, a.Org, nil)
	if err != nil {
		h.fail(c, err)
		return
	}
	locs := a.Locs
	if loc != "" {
		locs = map[string]bool{loc: true}
	}
	lowOnly := qBool(c, "low")
	out := []gin.H{}
	for i := range rows {
		it := &rows[i]
		onHand := 0.0
		for l, q := range byLoc[it.ProductID] {
			if locs == nil || locs[l] {
				onHand += q
			}
		}
		par, rp, _ := x.parFor(it.ProductID, locs)
		v := h.S.itemView(it, x, onHand, par, rp)
		if lowOnly && !v["low"].(bool) {
			continue
		}
		out = append(out, v)
	}
	h.ok(c, http.StatusOK, a, out)
}

func (h *Handler) getItem(c *gin.Context) {
	a := h.access(c)
	db := h.S.DB
	id := c.Param("product_id")
	it, err := h.S.item(db, a.Org, id)
	if err != nil {
		h.fail(c, err)
		return
	}
	x := h.S.newItemCtx(db, a.Org)
	sp := SpecOf(&it.Item, x.st)
	byLoc, _ := h.S.stockByLocation(db, a.Org, []string{id})
	locRows, _ := h.S.locations(db, a.Org)
	total := 0.0
	perLoc := []gin.H{}
	for _, l := range locRows {
		if !a.canLoc(l.ID) {
			continue
		}
		q := byLoc[id][l.ID]
		p := x.pars[parKey{l.ID, id}]
		_, hasStock := byLoc[id][l.ID]
		if !hasStock && p.Par == 0 && p.ReorderPoint == 0 {
			continue
		}
		total += q
		units := sp.StockUnits(q)
		perLoc = append(perLoc, gin.H{"location_id": l.ID, "location": l.Name, "type": l.Type, "qty": round3(q), "qty_text": sp.Text(q),
			"units": round3(units), "par": p.Par, "reorder_point": p.ReorderPoint,
			"low":   it.Track && (q <= 0 || (p.ReorderPoint > 0 && units <= p.ReorderPoint+eps)),
			"value": RoundMoney(math.Max(q, 0)*it.AvgCost, x.cur)})
	}
	par, rp, _ := x.parFor(id, a.Locs)
	v := h.S.itemView(it, x, total, par, rp)
	v["stock"] = perLoc

	var mvs []Movement
	mq := db.Where("organization_id = ? AND product_id = ?", a.Org, id)
	if a.Locs != nil {
		mq = mq.Where("location_id IN ?", keys(a.Locs))
	}
	mq.Order("created_at DESC").Limit(50).Find(&mvs)
	v["movements"] = h.S.movementViews(db, a.Org, mvs, map[string]*ItemRow{id: it}, x)

	var ph []PriceHistory
	db.Where("organization_id = ? AND product_id = ?", a.Org, id).Order("date DESC, created_at DESC").Limit(50).Find(&ph)
	hist := []gin.H{}
	for _, p := range ph {
		hist = append(hist, gin.H{"date": p.Date, "supplier_id": p.SupplierID, "supplier": x.suppliers[p.SupplierID], "unit_cost": p.UnitCost,
			"currency": p.Currency, "source": p.Source, "ref_id": p.RefID})
	}
	v["price_history"] = hist

	type ru struct {
		POSProductID string
		Name         string
		Qty          float64
		Kind         string
	}
	var uses []ru
	db.Raw(`SELECT l.pos_product_id, p.name, l.qty, COALESCE(r.kind,'') AS kind FROM bar_recipe_lines l
		JOIN products p ON p.id = l.pos_product_id AND p.deleted_at IS NULL
		LEFT JOIN bar_recipes r ON r.pos_product_id = l.pos_product_id
		WHERE l.organization_id = ? AND l.product_id = ? ORDER BY p.name`, a.Org, id).Scan(&uses)
	used := []gin.H{}
	for _, u := range uses {
		used = append(used, gin.H{"pos_product_id": u.POSProductID, "name": u.Name, "qty": u.Qty, "qty_text": sp.Text(u.Qty), "kind": u.Kind})
	}
	v["used_in"] = used
	h.ok(c, http.StatusOK, a, v)
}

func (h *Handler) createItem(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Edit, "add stock items"); err != nil {
		h.fail(c, err)
		return
	}
	var in ItemInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	in.CatalogKey = ""
	var pid string
	err := h.S.DB.Transaction(func(tx *gorm.DB) error {
		it, err := h.S.createItem(tx, a.Org, a.User, in)
		if err == nil {
			pid = it.ProductID
		}
		return err
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	h.respondItem(c, a, pid, http.StatusCreated)
}

func (h *Handler) respondItem(c *gin.Context, a access, id string, status int) {
	it, err := h.S.item(h.S.DB, a.Org, id)
	if err != nil {
		h.fail(c, err)
		return
	}
	x := h.S.newItemCtx(h.S.DB, a.Org)
	total, _ := h.S.stockAt(h.S.DB, a.Org, "", []string{id})
	par, rp, _ := x.parFor(id, a.Locs)
	h.ok(c, status, a, h.S.itemView(it, x, total[id], par, rp))
}

func (h *Handler) updateItem(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Edit, "edit stock items"); err != nil {
		h.fail(c, err)
		return
	}
	var in ItemInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	id := c.Param("product_id")
	if err := h.S.DB.Transaction(func(tx *gorm.DB) error { return h.S.updateItem(tx, a.Org, id, in) }); err != nil {
		h.fail(c, err)
		return
	}
	h.respondItem(c, a, id, http.StatusOK)
}

func (h *Handler) deleteItem(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Delete, "delete stock items"); err != nil {
		h.fail(c, err)
		return
	}
	id := c.Param("product_id")
	result := "deleted"
	err := h.S.DB.Transaction(func(tx *gorm.DB) error {
		var it Item
		if err := tx.Where("organization_id = ? AND product_id = ?", a.Org, id).First(&it).Error; err != nil {
			return notFound("Stock item not found")
		}
		var used []string
		tx.Raw(`SELECT p.name FROM bar_recipe_lines l JOIN products p ON p.id = l.pos_product_id AND p.deleted_at IS NULL
			WHERE l.organization_id = ? AND l.product_id = ? AND l.pos_product_id <> ?`, a.Org, id, id).Scan(&used)
		if len(used) > 0 {
			return conflict("Used in recipes: %s — remove it from them first", strings.Join(uniq(used), ", "))
		}
		var stock int64
		tx.Model(&Stock{}).Where("organization_id = ? AND product_id = ? AND qty <> 0", a.Org, id).Count(&stock)
		if stock > 0 {
			return conflict("This item still has stock — count it to zero or set it inactive")
		}
		var moved int64
		tx.Model(&Movement{}).Where("organization_id = ? AND product_id = ?", a.Org, id).Count(&moved)
		if moved > 0 || qBool(c, "archive") {
			// Keep the history: archive instead of deleting.
			result = "archived"
			if err := tx.Model(&Item{}).Where("product_id = ?", id).Updates(map[string]any{"active": false, "updated_at": h.S.now()}).Error; err != nil {
				return err
			}
			return tx.Model(&models.Product{}).Where("id = ? AND organization_id = ?", id, a.Org).Update("is_active", false).Error
		}
		for _, m := range []any{&Par{}, &Stock{}} {
			if err := tx.Where("organization_id = ? AND product_id = ?", a.Org, id).Delete(m).Error; err != nil {
				return err
			}
		}
		tx.Where("organization_id = ? AND pos_product_id = ?", a.Org, id).Delete(&RecipeLine{})
		tx.Where("organization_id = ? AND pos_product_id = ?", a.Org, id).Delete(&Recipe{})
		if err := tx.Where("product_id = ?", id).Delete(&Item{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ? AND organization_id = ?", id, a.Org).Delete(&models.Product{}).Error
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, gin.H{"product_id": id, "result": result})
}

func (h *Handler) putPar(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Edit, "set par levels"); err != nil {
		h.fail(c, err)
		return
	}
	var in struct {
		LocationID string     `json:"location_id"`
		Lines      []ParInput `json:"lines"`
	}
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	for i := range in.Lines {
		if in.Lines[i].LocationID == "" {
			in.Lines[i].LocationID = in.LocationID
		}
	}
	if err := h.S.DB.Transaction(func(tx *gorm.DB) error { return h.S.savePars(tx, a.Org, "", in.Lines) }); err != nil {
		h.fail(c, err)
		return
	}
	var rows []Par
	q := h.S.DB.Where("organization_id = ?", a.Org)
	if in.LocationID != "" {
		q = q.Where("location_id = ?", in.LocationID)
	}
	q.Find(&rows)
	out := []gin.H{}
	for _, r := range rows {
		out = append(out, gin.H{"location_id": r.LocationID, "product_id": r.ProductID, "par": r.Par, "reorder_point": r.ReorderPoint})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["product_id"].(string) < out[j]["product_id"].(string) })
	h.ok(c, http.StatusOK, a, out)
}

// ---------------------------------------------------------------- menu items

// MenuInput is POST /bar/items/:id/menu-items.
type MenuInput struct {
	Tot         bool     `json:"tot"`
	Bottle      bool     `json:"bottle"`
	TotPrice    *float64 `json:"tot_price"`
	BottlePrice *float64 `json:"bottle_price"`
}

// MenuMade is a menu item created (or found) for a stock item.
type MenuMade struct {
	POSProductID string  `json:"pos_product_id"`
	Name         string  `json:"name"`
	Kind         string  `json:"kind"`
	Price        float64 `json:"price"`
	Created      bool    `json:"created"`
}

func (s *Service) makeMenuItems(tx *gorm.DB, org, itemID string, in MenuInput) ([]MenuMade, error) {
	it, err := s.item(tx, org, itemID)
	if err != nil {
		return nil, err
	}
	st := s.Settings(tx, org)
	sp := SpecOf(&it.Item, st)
	out := []MenuMade{}
	type want struct {
		kind, name, unit string
		qty              float64
		price            *float64
	}
	var wants []want
	if in.Tot {
		if it.BaseUnit != BaseML {
			return nil, badReq("Tots only apply to items measured in ml")
		}
		wants = append(wants, want{"tot", it.Name + " tot", "tot", sp.TotML, in.TotPrice})
	}
	if in.Bottle {
		wants = append(wants, want{"bottle", it.Name + " " + it.UnitLabel, "bottle", it.UnitSize, in.BottlePrice})
	}
	if len(wants) == 0 {
		return nil, badReq("Choose tot and/or bottle")
	}
	var catID *string
	tx.Table("products").Select("category_id").Where("id = ?", itemID).Scan(&catID)
	for _, w := range wants {
		if w.price != nil && *w.price < 0 {
			return nil, badReq("Prices cannot be negative")
		}
		var ex Recipe
		if tx.Where("organization_id = ? AND kind = ? AND source_id = ?", org, w.kind, itemID).Limit(1).Find(&ex).Error == nil && ex.POSProductID != "" {
			var p models.Product
			if tx.Where("id = ? AND organization_id = ?", ex.POSProductID, org).First(&p).Error == nil {
				if w.price != nil {
					p.SellingPrice = math.Round(*w.price*100) / 100
					tx.Model(&models.Product{}).Where("id = ?", p.ID).Update("selling_price", p.SellingPrice)
				}
				out = append(out, MenuMade{POSProductID: p.ID, Name: p.Name, Kind: w.kind, Price: p.SellingPrice})
				continue
			}
			// The product was deleted: drop the stale recipe and make a new one.
			tx.Where("pos_product_id = ?", ex.POSProductID).Delete(&RecipeLine{})
			tx.Where("pos_product_id = ?", ex.POSProductID).Delete(&Recipe{})
		}
		p := models.Product{ID: uuid.NewString(), OrganizationID: org, Name: w.name, UnitOfMeasure: "each", IsActive: true, CategoryID: catID}
		if w.price != nil {
			p.SellingPrice = math.Round(*w.price*100) / 100
		}
		if err := tx.Omit(clause.Associations).Create(&p).Error; err != nil {
			return nil, err
		}
		if err := tx.Create(&Recipe{POSProductID: p.ID, OrganizationID: org, Kind: w.kind, SourceID: itemID, CreatedAt: s.now(), UpdatedAt: s.now()}).Error; err != nil {
			return nil, err
		}
		if err := tx.Create(&RecipeLine{ID: uuid.NewString(), OrganizationID: org, POSProductID: p.ID, ProductID: itemID, Qty: round3(w.qty), Unit: w.unit, EnteredQty: 1}).Error; err != nil {
			return nil, err
		}
		out = append(out, MenuMade{POSProductID: p.ID, Name: p.Name, Kind: w.kind, Price: p.SellingPrice, Created: true})
	}
	return out, s.sync(tx, org, []string{itemID})
}

// ensureSelfRecipe gives a stock item sold as itself its 1:1 recipe.
func (s *Service) ensureSelfRecipe(tx *gorm.DB, org, pid string) (bool, error) {
	var n int64
	tx.Model(&Recipe{}).Where("pos_product_id = ?", pid).Count(&n)
	if n > 0 {
		return false, nil
	}
	var it Item
	if err := tx.Where("organization_id = ? AND product_id = ?", org, pid).First(&it).Error; err != nil {
		return false, notFound("Stock item not found")
	}
	unit := "unit"
	if it.BaseUnit == BaseML {
		unit = "bottle"
	}
	if err := tx.Create(&Recipe{POSProductID: pid, OrganizationID: org, Kind: "self", SourceID: pid, CreatedAt: s.now(), UpdatedAt: s.now()}).Error; err != nil {
		return false, err
	}
	return true, tx.Create(&RecipeLine{ID: uuid.NewString(), OrganizationID: org, POSProductID: pid, ProductID: pid, Qty: it.UnitSize, Unit: unit, EnteredQty: 1}).Error
}

func (h *Handler) makeMenuItems(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Edit, "create menu items"); err != nil {
		h.fail(c, err)
		return
	}
	var in MenuInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	var made []MenuMade
	err := h.S.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		made, err = h.S.makeMenuItems(tx, a.Org, c.Param("product_id"), in)
		return err
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	out := []any{}
	for _, m := range made {
		if r, err := h.S.recipeView(h.S.DB, a.Org, m.POSProductID); err == nil {
			r["created"] = m.Created
			out = append(out, r)
		}
	}
	h.ok(c, http.StatusCreated, a, out)
}
