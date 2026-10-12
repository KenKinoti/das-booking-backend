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

// recipeSet holds recipes (explicit, plus the implicit 1:1 recipe of stock
// items sold as themselves) for some POS products.
type recipeSet struct {
	recipes  map[string]Recipe
	lines    map[string][]RecipeLine
	implicit map[string]bool // stock items without an explicit recipe
}

// recipesFor loads the recipes of the given POS products (all when nil).
// Stock items without an explicit recipe get their implicit 1:1 recipe.
func (s *Service) recipesFor(db *gorm.DB, org string, posIDs []string) (recipeSet, error) {
	rs := recipeSet{recipes: map[string]Recipe{}, lines: map[string][]RecipeLine{}, implicit: map[string]bool{}}
	var recs []Recipe
	q := db.Where("organization_id = ?", org)
	if posIDs != nil {
		if len(posIDs) == 0 {
			return rs, nil
		}
		q = q.Where("pos_product_id IN ?", posIDs)
	}
	if err := q.Find(&recs).Error; err != nil {
		return rs, err
	}
	for _, r := range recs {
		rs.recipes[r.POSProductID] = r
	}
	var lines []RecipeLine
	lq := db.Where("organization_id = ?", org)
	if posIDs != nil {
		lq = lq.Where("pos_product_id IN ?", posIDs)
	}
	if err := lq.Order("sort, id").Find(&lines).Error; err != nil {
		return rs, err
	}
	for _, l := range lines {
		if _, ok := rs.recipes[l.POSProductID]; ok {
			rs.lines[l.POSProductID] = append(rs.lines[l.POSProductID], l)
		}
	}
	// Implicit 1:1 for stock items sold as themselves.
	var items []Item
	iq := db.Where("organization_id = ?", org)
	if posIDs != nil {
		iq = iq.Where("product_id IN ?", posIDs)
	}
	if err := iq.Find(&items).Error; err != nil {
		return rs, err
	}
	for _, it := range items {
		if _, ok := rs.recipes[it.ProductID]; ok {
			continue
		}
		unit := "unit"
		if it.BaseUnit == BaseML {
			unit = "bottle"
		}
		rs.recipes[it.ProductID] = Recipe{POSProductID: it.ProductID, OrganizationID: org, Kind: "self", SourceID: it.ProductID}
		rs.implicit[it.ProductID] = true
		rs.lines[it.ProductID] = []RecipeLine{{POSProductID: it.ProductID, ProductID: it.ProductID, Qty: it.UnitSize, Unit: unit, EnteredQty: 1}}
	}
	return rs, nil
}

// LineInput is one recipe line as entered.
type LineInput struct {
	ProductID string  `json:"product_id"`
	Qty       float64 `json:"qty"`
	Unit      string  `json:"unit"` // ml | unit | tot | bottle (default: base units)
}

// RecipeInput creates or replaces a recipe.
type RecipeInput struct {
	POSProductID string      `json:"pos_product_id"`
	Name         *string     `json:"name"`
	Price        *float64    `json:"price"`
	Category     string      `json:"category"` // product category label for a new menu item (optional)
	Lines        []LineInput `json:"lines"`    // omitted on PUT = keep the lines; [] = no stock lines
}

func (s *Service) saveRecipe(tx *gorm.DB, org, posID string, in RecipeInput, creating bool) (string, error) {
	st := s.Settings(tx, org)
	if creating && posID == "" {
		name := ""
		if in.Name != nil {
			name = strings.TrimSpace(*in.Name)
		}
		if name == "" {
			return "", badReq("Enter a name for the menu item (or choose a POS product)")
		}
		p := models.Product{ID: uuid.NewString(), OrganizationID: org, Name: name, UnitOfMeasure: "each", IsActive: true}
		if in.Price != nil {
			if *in.Price < 0 {
				return "", badReq("Price cannot be negative")
			}
			p.SellingPrice = math.Round(*in.Price*100) / 100
		}
		if in.Category != "" {
			label := in.Category
			if l, ok := CategoryLabels[in.Category]; ok {
				label = l
			}
			p.CategoryID = ensureCategory(tx, org, label)
		}
		if err := tx.Omit(clause.Associations).Create(&p).Error; err != nil {
			return "", err
		}
		posID = p.ID
	} else {
		var p models.Product
		if err := tx.Where("id = ? AND organization_id = ?", posID, org).First(&p).Error; err != nil {
			return "", notFound("POS product not found")
		}
		upd := map[string]any{}
		if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
			upd["name"] = strings.TrimSpace(*in.Name)
		}
		if in.Price != nil {
			if *in.Price < 0 {
				return "", badReq("Price cannot be negative")
			}
			upd["selling_price"] = math.Round(*in.Price*100) / 100
		}
		if len(upd) > 0 {
			if err := tx.Model(&models.Product{}).Where("id = ?", posID).Updates(upd).Error; err != nil {
				return "", err
			}
		}
	}
	var exists Recipe
	has := tx.Where("pos_product_id = ? AND organization_id = ?", posID, org).Limit(1).Find(&exists).Error == nil && exists.POSProductID != ""
	if creating && has {
		return "", conflict("This product already has a recipe — edit it instead")
	}
	var self Item
	isItem := tx.Where("organization_id = ? AND product_id = ?", org, posID).Limit(1).Find(&self).Error == nil && self.ProductID != ""
	if !creating && has && in.Lines == nil {
		// Only the name / price changed: keep the lines.
		return posID, nil
	}

	ids := []string{}
	for _, l := range in.Lines {
		ids = append(ids, l.ProductID)
	}
	items, err := s.Items(tx, org, uniq(ids))
	if err != nil {
		return "", err
	}
	rows := []RecipeLine{}
	for i, l := range in.Lines {
		it, ok := items[l.ProductID]
		if !ok {
			return "", badReq("Line %d: choose a stock item", i+1)
		}
		if l.Qty <= 0 {
			return "", badReq("Line %d: quantity must be more than 0", i+1)
		}
		if isItem && l.ProductID != posID {
			return "", badReq("A stock item sold as itself can only use itself")
		}
		base, err := SpecOf(&it.Item, st).ToBase(l.Qty, l.Unit)
		if err != nil {
			return "", badReq("Line %d (%s): %s", i+1, it.Name, err.Error())
		}
		unit := strings.ToLower(strings.TrimSpace(l.Unit))
		if unit == "" {
			unit = it.BaseUnit
		}
		rows = append(rows, RecipeLine{ID: uuid.NewString(), OrganizationID: org, POSProductID: posID, ProductID: l.ProductID, Qty: base, Unit: unit, EnteredQty: round3(l.Qty), Sort: i})
	}
	kind := "custom"
	if has {
		kind = exists.Kind
		if kind == "" {
			kind = "custom"
		}
	}
	if isItem {
		kind = "self"
	}
	if err := tx.Where("pos_product_id = ? AND organization_id = ?", posID, org).Delete(&RecipeLine{}).Error; err != nil {
		return "", err
	}
	if has {
		if err := tx.Model(&Recipe{}).Where("pos_product_id = ?", posID).Updates(map[string]any{"kind": kind, "updated_at": s.now()}).Error; err != nil {
			return "", err
		}
	} else if err := tx.Create(&Recipe{POSProductID: posID, OrganizationID: org, Kind: kind, SourceID: firstProduct(rows), CreatedAt: s.now(), UpdatedAt: s.now()}).Error; err != nil {
		return "", err
	}
	for i := range rows {
		if err := tx.Create(&rows[i]).Error; err != nil {
			return "", err
		}
	}
	// A menu item's stock figure follows the bar stock from now on.
	if !isItem {
		if len(rows) == 0 {
			tx.Model(&models.Product{}).Where("id = ?", posID).Update("current_stock", 0)
		}
		return posID, s.sync(tx, org, ids)
	}
	return posID, nil
}

func firstProduct(rows []RecipeLine) string {
	if len(rows) > 0 {
		return rows[0].ProductID
	}
	return ""
}

// recipeCtx carries what recipe views need.
type recipeCtx struct {
	items     map[string]*ItemRow
	st        Settings
	cur       string
	rate      float64
	inclusive bool
}

func (s *Service) newRecipeCtx(db *gorm.DB, org string) (recipeCtx, error) {
	items, err := s.Items(db, org, nil)
	if err != nil {
		return recipeCtx{}, err
	}
	r, inc := s.taxInfo(db, org)
	return recipeCtx{items: items, st: s.Settings(db, org), cur: s.Currency(db, org), rate: r, inclusive: inc}, nil
}

// recipeCategory is the category of the line with the largest cost (else the first line).
func recipeCategory(lines []RecipeLine, items map[string]*ItemRow) string {
	best, bestCost := "", -1.0
	for _, l := range lines {
		it, ok := items[l.ProductID]
		if !ok {
			continue
		}
		c := l.Qty * it.AvgCost
		if c > bestCost {
			best, bestCost = it.Category, c
		}
	}
	if best == "" {
		return "other"
	}
	return best
}

func (s *Service) recipeViewWith(x recipeCtx, posID, name string, price float64, rec Recipe, lines []RecipeLine) gin.H {
	cost := 0.0
	out := []gin.H{}
	for _, l := range lines {
		it, ok := x.items[l.ProductID]
		lv := gin.H{"product_id": l.ProductID, "qty": l.Qty, "unit": l.Unit, "entered_qty": l.EnteredQty}
		if ok {
			sp := SpecOf(&it.Item, x.st)
			lc := l.Qty * it.AvgCost
			cost += lc
			lv["name"], lv["category"], lv["base_unit"] = it.Name, it.Category, it.BaseUnit
			lv["qty_text"] = sp.QtyText(l.Qty, l.Unit)
			lv["cost"] = RoundMoney(lc, x.cur)
		}
		out = append(out, lv)
	}
	net := netPrice(price, x.rate, x.inclusive)
	v := gin.H{"pos_product_id": posID, "name": name, "price": price, "net_price": RoundMoney(net, x.cur), "kind": rec.Kind,
		"source_id": rec.SourceID, "category": recipeCategory(lines, x.items), "lines": out, "cost": RoundMoney(cost, x.cur),
		"pour_cost_pct": PourCostPct(cost, net), "margin": RoundMoney(net-cost, x.cur), "is_stock_item": x.items[posID] != nil,
		"tax_rate": x.rate, "prices_include_tax": x.inclusive}
	if net > 0 {
		v["margin_pct"] = math.Round((net-cost)/net*10000) / 100
	} else {
		v["margin_pct"] = nil
	}
	if t := x.st.PourCostTargets[recipeCategory(lines, x.items)]; t != nil {
		v["pour_cost_target"] = *t
	}
	return v
}

func (s *Service) recipeView(db *gorm.DB, org, posID string) (gin.H, error) {
	rs, err := s.recipesFor(db, org, []string{posID})
	if err != nil {
		return nil, err
	}
	rec, ok := rs.recipes[posID]
	if !ok {
		return nil, notFound("Recipe not found")
	}
	var p models.Product
	if err := db.Where("id = ? AND organization_id = ?", posID, org).First(&p).Error; err != nil {
		return nil, notFound("POS product not found")
	}
	x, err := s.newRecipeCtx(db, org)
	if err != nil {
		return nil, err
	}
	return s.recipeViewWith(x, posID, p.Name, p.SellingPrice, rec, rs.lines[posID]), nil
}

func (h *Handler) listRecipes(c *gin.Context) {
	a := h.access(c)
	db := h.S.DB
	rs, err := h.S.recipesFor(db, a.Org, nil)
	if err != nil {
		h.fail(c, err)
		return
	}
	x, err := h.S.newRecipeCtx(db, a.Org)
	if err != nil {
		h.fail(c, err)
		return
	}
	var prods []models.Product
	if ids := keys(rs.recipes); len(ids) > 0 {
		db.Where("organization_id = ? AND id IN ?", a.Org, ids).Find(&prods)
	}
	kind := c.Query("kind")
	withSelf := c.Query("include_self") != "0"
	withImplicit := qBool(c, "include_implicit")
	qs := strings.ToLower(strings.TrimSpace(c.Query("q")))
	out := []gin.H{}
	for _, p := range prods {
		rec := rs.recipes[p.ID]
		if kind != "" && rec.Kind != kind {
			continue
		}
		if (!withSelf && rec.Kind == "self") || (rs.implicit[p.ID] && !withImplicit) {
			continue
		}
		if qs != "" && !strings.Contains(strings.ToLower(p.Name), qs) {
			continue
		}
		v := h.S.recipeViewWith(x, p.ID, p.Name, p.SellingPrice, rec, rs.lines[p.ID])
		v["active"] = p.IsActive
		v["implicit"] = rs.implicit[p.ID]
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	h.ok(c, http.StatusOK, a, out)
}

func (h *Handler) getRecipe(c *gin.Context) {
	a := h.access(c)
	v, err := h.S.recipeView(h.S.DB, a.Org, c.Param("pos_product_id"))
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, v)
}

func (h *Handler) createRecipe(c *gin.Context) { h.writeRecipe(c, true) }
func (h *Handler) updateRecipe(c *gin.Context) { h.writeRecipe(c, false) }

func (h *Handler) writeRecipe(c *gin.Context, creating bool) {
	a := h.access(c)
	if err := a.need(a.Edit, "edit recipes"); err != nil {
		h.fail(c, err)
		return
	}
	var in RecipeInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	posID := strings.TrimSpace(in.POSProductID)
	if !creating {
		posID = c.Param("pos_product_id")
	}
	var id string
	err := h.S.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		id, err = h.S.saveRecipe(tx, a.Org, posID, in, creating)
		return err
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	v, err := h.S.recipeView(h.S.DB, a.Org, id)
	if err != nil {
		h.fail(c, err)
		return
	}
	status := http.StatusOK
	if creating {
		status = http.StatusCreated
	}
	h.ok(c, status, a, v)
}

func (h *Handler) deleteRecipe(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Delete, "delete recipes"); err != nil {
		h.fail(c, err)
		return
	}
	id := c.Param("pos_product_id")
	err := h.S.DB.Transaction(func(tx *gorm.DB) error {
		var r Recipe
		if err := tx.Where("pos_product_id = ? AND organization_id = ?", id, a.Org).First(&r).Error; err != nil {
			return notFound("Recipe not found")
		}
		if err := tx.Where("pos_product_id = ? AND organization_id = ?", id, a.Org).Delete(&RecipeLine{}).Error; err != nil {
			return err
		}
		if err := tx.Where("pos_product_id = ? AND organization_id = ?", id, a.Org).Delete(&Recipe{}).Error; err != nil {
			return err
		}
		if qBool(c, "delete_product") {
			var n int64
			tx.Model(&Item{}).Where("product_id = ?", id).Count(&n)
			if n == 0 {
				return tx.Where("id = ? AND organization_id = ?", id, a.Org).Delete(&models.Product{}).Error
			}
		}
		return nil
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, gin.H{"deleted": true, "pos_product_id": id})
}

// ---------------------------------------------------------------- availability

// Availability lists how many of each bar-managed POS product a location
// can make: min over the recipe lines of floor(stock ÷ line qty). POS shows
// it as a badge and never disables the product.
func (s *Service) Availability(db *gorm.DB, org, loc string) ([]gin.H, error) {
	rs, err := s.recipesFor(db, org, nil)
	if err != nil {
		return nil, err
	}
	items, err := s.Items(db, org, nil)
	if err != nil {
		return nil, err
	}
	stock, err := s.stockAt(db, org, loc, nil)
	if err != nil {
		return nil, err
	}
	pars := s.pars(db, org)
	st := s.Settings(db, org)
	var active []string
	if ids := keys(rs.recipes); len(ids) > 0 {
		db.Table("products").Where("organization_id = ? AND id IN ? AND deleted_at IS NULL AND is_active", org, ids).Pluck("id", &active)
	}
	out := []gin.H{}
	for _, pid := range active {
		rec := rs.recipes[pid]
		lines := rs.lines[pid]
		al := []AvailLine{}
		low := false
		label := "serving"
		for _, l := range lines {
			it, ok := items[l.ProductID]
			if !ok || !it.Track {
				continue
			}
			q := stock[l.ProductID]
			al = append(al, AvailLine{Qty: l.Qty, Stock: q})
			if p, ok := pars[parKey{loc, l.ProductID}]; ok && p.ReorderPoint > 0 && SpecOf(&it.Item, st).StockUnits(q) <= p.ReorderPoint+eps {
				low = true
			}
		}
		switch rec.Kind {
		case "tot":
			label = "tot"
		case "bottle", "self":
			if it, ok := items[rec.SourceID]; ok {
				label = it.UnitLabel
			}
		}
		n, limited := Availability(al)
		var avail any
		if limited {
			avail = int(n)
			if n <= 0 {
				low = true
			}
		}
		out = append(out, gin.H{"product_id": pid, "available": avail, "unit_label": label, "low": low, "kind": rec.Kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["product_id"].(string) < out[j]["product_id"].(string) })
	return out, nil
}

func (h *Handler) posAvailability(c *gin.Context) {
	a := h.access(c)
	loc := strings.TrimSpace(c.Query("location_id"))
	if loc == "" {
		loc = h.S.defaultBar(h.S.DB, a.Org)
	}
	if loc == "" {
		h.ok(c, http.StatusOK, a, gin.H{"location_id": nil, "items": []any{}})
		return
	}
	if _, err := h.S.location(h.S.DB, a.Org, loc); err != nil {
		h.fail(c, err)
		return
	}
	items, err := h.S.Availability(h.S.DB, a.Org, loc)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, gin.H{"location_id": loc, "items": items})
}
