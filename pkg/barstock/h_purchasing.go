package barstock

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Open purchase order statuses (internal/handlers/suppliers.go).
var openPOStatuses = []string{"draft", "sent", "confirmed", "partially_received"}

// onOrder is the outstanding quantity (stock units) per product on open POs.
func (s *Service) onOrder(db *gorm.DB, org string) map[string]float64 {
	type r struct {
		ProductID string
		Qty       float64
	}
	var rows []r
	db.Raw(`SELECT i.product_id, SUM(GREATEST(i.quantity - i.quantity_received, 0)) AS qty FROM purchase_order_items i
		JOIN purchase_orders o ON o.id = i.purchase_order_id AND o.deleted_at IS NULL
		WHERE o.organization_id = ? AND o.status IN ? GROUP BY i.product_id`, org, openPOStatuses).Scan(&rows)
	out := map[string]float64{}
	for _, x := range rows {
		out[x.ProductID] = x.Qty
	}
	return out
}

type bestPrice struct {
	Cost       float64
	SupplierID string
}

// bestPrices is the lowest unit cost paid per product in the last 90 days.
func (s *Service) bestPrices(db *gorm.DB, org string) map[string]bestPrice {
	since := s.now().AddDate(0, 0, -90).Format("2006-01-02")
	var rows []PriceHistory
	db.Where("organization_id = ? AND date >= ? AND unit_cost > 0", org, since).Order("unit_cost, date DESC").Find(&rows)
	out := map[string]bestPrice{}
	for _, r := range rows {
		if _, ok := out[r.ProductID]; !ok {
			out[r.ProductID] = bestPrice{r.UnitCost, r.SupplierID}
		}
	}
	return out
}

// Reorder suggests what to buy for a location (default the main store;
// "total" = every location): max(0, par − on hand − on order) rounded up to
// whole packs, when on hand + on order is at or below the reorder point (or
// below par when no reorder point is set). Grouped by preferred supplier.
func (s *Service) Reorder(db *gorm.DB, org, loc string) (gin.H, error) {
	total := loc == "total" || loc == "all"
	if !total {
		if loc == "" {
			m, err := s.mainStore(db, org)
			if err != nil {
				return nil, err
			}
			loc = m
		} else if _, err := s.location(db, org, loc); err != nil {
			return nil, err
		}
	}
	items, err := s.Items(db, org, nil)
	if err != nil {
		return nil, err
	}
	x := s.newItemCtx(db, org)
	where := loc
	if total {
		where = ""
	}
	stock, err := s.stockAt(db, org, where, nil)
	if err != nil {
		return nil, err
	}
	ordered := s.onOrder(db, org)
	best := s.bestPrices(db, org)
	groups := map[string]*struct {
		lines []gin.H
		value float64
	}{}
	for _, pid := range keys(items) {
		it := items[pid]
		if !it.Track || !it.Active || !it.IsActive {
			continue
		}
		var par, rp float64
		if total {
			par, rp, _ = x.parFor(pid, nil)
		} else {
			p := x.pars[parKey{loc, pid}]
			par, rp = p.Par, p.ReorderPoint
		}
		if par <= 0 {
			continue
		}
		sp := SpecOf(&it.Item, x.st)
		onHand := sp.StockUnits(stock[pid])
		oo := ordered[pid]
		if rp > 0 && onHand+oo > rp+eps {
			continue
		}
		units, packs := Reorder(par, onHand, oo, it.PackSize)
		if units <= 0 {
			continue
		}
		sup := ""
		if it.PreferredSupplierID != nil {
			sup = *it.PreferredSupplierID
		} else if it.LastSupplierID != nil {
			sup = *it.LastSupplierID
		}
		cost := it.LastCost
		if cost <= 0 {
			cost = it.AvgCost * it.UnitSize
		}
		line := gin.H{"product_id": pid, "name": it.Name, "category": it.Category, "unit_label": it.UnitLabel, "pack_size": it.PackSize,
			"pack_label": it.PackLabel, "on_hand": round3(onHand), "on_hand_text": sp.Text(stock[pid]), "on_order": round3(oo), "par": par,
			"reorder_point": rp, "suggest_units": units, "suggest_packs": packs, "last_cost": RoundMoney(it.LastCost, x.cur),
			"unit_cost": RoundMoney(cost, x.cur), "line_total": RoundMoney(cost*units, x.cur)}
		if b, ok := best[pid]; ok {
			line["best_cost"], line["best_supplier_id"], line["best_supplier"] = b.Cost, b.SupplierID, x.suppliers[b.SupplierID]
		} else {
			line["best_cost"], line["best_supplier_id"], line["best_supplier"] = nil, nil, nil
		}
		g := groups[sup]
		if g == nil {
			g = &struct {
				lines []gin.H
				value float64
			}{}
			groups[sup] = g
		}
		g.lines = append(g.lines, line)
		g.value += cost * units
	}
	out := []gin.H{}
	for _, sid := range keys(groups) {
		g := groups[sid]
		var sup any
		if sid != "" {
			sup = gin.H{"id": sid, "name": x.suppliers[sid]}
		}
		out = append(out, gin.H{"supplier_id": nilIfEmpty(sid), "supplier": sup, "lines": g.lines, "total_cost": RoundMoney(g.value, x.cur)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["supplier_id"] != nil && out[j]["supplier_id"] == nil })
	var locID any = loc
	if total {
		locID = "total"
	}
	return gin.H{"location_id": locID, "groups": out, "currency": x.cur}, nil
}

func (h *Handler) reorder(c *gin.Context) {
	a := h.access(c)
	out, err := h.S.Reorder(h.S.DB, a.Org, strings.TrimSpace(c.Query("location_id")))
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, out)
}

// CreatePOInput is POST /bar/reorder/create-pos.
type CreatePOInput struct {
	Groups []struct {
		SupplierID string `json:"supplier_id"`
		Lines      []struct {
			ProductID string   `json:"product_id"`
			QtyUnits  float64  `json:"qty_units"`
			UnitCost  *float64 `json:"unit_cost"`
		} `json:"lines"`
	} `json:"groups"`
	ExpectedDate string `json:"expected_date"`
	Notes        string `json:"notes"`
}

// orderDay is a calendar date stored at midday UTC (like internal/handlers).
func orderDay(day string) time.Time {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		t = time.Now().UTC().Truncate(24 * time.Hour)
	}
	return t.Add(12 * time.Hour)
}

// CreatePOs makes one draft purchase order per supplier using the existing
// purchase order model and numbering.
func (s *Service) CreatePOs(org, user string, in CreatePOInput) ([]string, error) {
	type line struct {
		pid  string
		qty  int
		cost float64
	}
	bySup := map[string][]line{}
	order := []string{}
	for gi, g := range in.Groups {
		sid := strings.TrimSpace(g.SupplierID)
		if sid == "" {
			return nil, badReq("Group %d: choose a supplier", gi+1)
		}
		for li, l := range g.Lines {
			if l.QtyUnits <= 0 {
				continue
			}
			if math.Abs(l.QtyUnits-math.Round(l.QtyUnits)) > eps {
				return nil, badReq("Group %d line %d: order whole units", gi+1, li+1)
			}
			cost := -1.0
			if l.UnitCost != nil {
				if *l.UnitCost < 0 {
					return nil, badReq("Group %d line %d: unit cost cannot be negative", gi+1, li+1)
				}
				cost = *l.UnitCost
			}
			if _, ok := bySup[sid]; !ok {
				order = append(order, sid)
			}
			bySup[sid] = append(bySup[sid], line{l.ProductID, int(math.Round(l.QtyUnits)), cost})
		}
	}
	if len(order) == 0 {
		return nil, badReq("Nothing to order")
	}
	ids := []string{}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		cur := s.Currency(tx, org)
		today := s.Today(tx, org)
		var expected *time.Time
		if ValidDay(in.ExpectedDate) {
			e := orderDay(in.ExpectedDate)
			expected = &e
		}
		for _, sid := range order {
			var n int64
			tx.Table("suppliers").Where("id = ? AND organization_id = ? AND deleted_at IS NULL", sid, org).Count(&n)
			if n == 0 {
				return badReq("Supplier not found")
			}
			pids := []string{}
			for _, l := range bySup[sid] {
				pids = append(pids, l.pid)
			}
			items, err := s.Items(tx, org, uniq(pids))
			if err != nil {
				return err
			}
			merged := map[string]*line{}
			lorder := []string{}
			for _, l := range bySup[sid] {
				it, ok := items[l.pid]
				if !ok {
					return badReq("Stock item not found")
				}
				if l.cost < 0 {
					l.cost = it.LastCost
					if l.cost <= 0 {
						l.cost = it.AvgCost * it.UnitSize
					}
				}
				if m, ok := merged[l.pid]; ok {
					m.qty += l.qty
					continue
				}
				ll := l
				merged[l.pid] = &ll
				lorder = append(lorder, l.pid)
			}
			po := models.PurchaseOrder{ID: uuid.NewString(), OrganizationID: org, SupplierID: sid, Status: "draft", OrderDate: orderDay(today),
				ExpectedDate: expected, CreatedBy: user, Notes: strings.TrimSpace(in.Notes)}
			if po.Notes == "" {
				po.Notes = "Bar stock reorder"
			}
			if s.NextPONumber != nil {
				po.OrderNumber = s.NextPONumber(tx, org)
			}
			rows := []models.PurchaseOrderItem{}
			sub := 0.0
			for _, pid := range lorder {
				l := merged[pid]
				cost := math.Round(l.cost*100) / 100
				total := math.Round(float64(l.qty)*cost*100) / 100
				rows = append(rows, models.PurchaseOrderItem{ID: uuid.NewString(), ProductID: pid, Quantity: l.qty, UnitCost: cost, TotalCost: total})
				sub += total
			}
			po.SubTotal = RoundMoney(sub, cur)
			po.TotalAmount = po.SubTotal
			if err := tx.Omit(clause.Associations).Create(&po).Error; err != nil {
				return err
			}
			for i := range rows {
				rows[i].PurchaseOrderID = po.ID
				if err := tx.Omit(clause.Associations).Create(&rows[i]).Error; err != nil {
					return err
				}
			}
			ids = append(ids, po.ID)
		}
		return nil
	})
	return ids, err
}

func (s *Service) poViews(db *gorm.DB, org string, ids []string, statuses []string) []gin.H {
	q := db.Where("organization_id = ?", org)
	if ids != nil {
		q = q.Where("id IN ?", ids)
	}
	if statuses != nil {
		q = q.Where("status IN ?", statuses)
	}
	var pos []models.PurchaseOrder
	q.Order("expected_date NULLS LAST, order_date DESC").Find(&pos)
	if len(pos) == 0 {
		return []gin.H{}
	}
	pids := []string{}
	for _, p := range pos {
		pids = append(pids, p.ID)
	}
	var lines []models.PurchaseOrderItem
	db.Where("purchase_order_id IN ?", pids).Order("created_at").Find(&lines)
	prodIDs := []string{}
	for _, l := range lines {
		prodIDs = append(prodIDs, l.ProductID)
	}
	items, _ := s.Items(db, org, uniq(prodIDs))
	names := map[string]string{}
	type pn struct{ ID, Name string }
	var pns []pn
	if len(prodIDs) > 0 {
		db.Table("products").Select("id, name").Where("id IN ?", uniq(prodIDs)).Scan(&pns)
	}
	for _, p := range pns {
		names[p.ID] = p.Name
	}
	sups := s.supplierNames(db, org)
	byPO := map[string][]gin.H{}
	barLines := map[string]int{}
	for _, l := range lines {
		_, isBar := items[l.ProductID]
		if isBar {
			barLines[l.PurchaseOrderID]++
		}
		v := gin.H{"item_id": l.ID, "product_id": l.ProductID, "name": names[l.ProductID], "quantity": l.Quantity, "quantity_received": l.QuantityReceived,
			"outstanding": max(0, l.Quantity-l.QuantityReceived), "unit_cost": l.UnitCost, "line_total": l.TotalCost, "is_bar_item": isBar}
		if it, ok := items[l.ProductID]; ok {
			v["unit_label"], v["pack_size"], v["pack_label"] = it.UnitLabel, it.PackSize, it.PackLabel
		}
		byPO[l.PurchaseOrderID] = append(byPO[l.PurchaseOrderID], v)
	}
	out := []gin.H{}
	for _, p := range pos {
		out = append(out, gin.H{"id": p.ID, "order_number": p.OrderNumber, "status": p.Status, "supplier_id": p.SupplierID, "supplier": sups[p.SupplierID],
			"order_date": p.OrderDate.Format("2006-01-02"), "expected_date": dayOf(p.ExpectedDate), "total_cost": p.TotalAmount,
			"lines": byPO[p.ID], "bar_lines": barLines[p.ID], "overdue": p.ExpectedDate != nil && p.ExpectedDate.Format("2006-01-02") < s.now().Format("2006-01-02")})
	}
	return out
}

func dayOf(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format("2006-01-02")
}

func (h *Handler) createPOs(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Approve, "create purchase orders from bar stock"); err != nil {
		h.fail(c, err)
		return
	}
	var in CreatePOInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	ids, err := h.S.CreatePOs(a.Org, a.User, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusCreated, a, h.S.poViews(h.S.DB, a.Org, ids, nil))
}

func (h *Handler) openPOs(c *gin.Context) {
	a := h.access(c)
	out := []gin.H{}
	for _, p := range h.S.poViews(h.S.DB, a.Org, nil, openPOStatuses) {
		if p["bar_lines"].(int) > 0 || qBool(c, "all") {
			out = append(out, p)
		}
	}
	h.ok(c, http.StatusOK, a, out)
}

// ---------------------------------------------------------------- deliveries

// DeliveryInput is POST /bar/deliveries.
type DeliveryInput struct {
	POID         string `json:"po_id"`
	SupplierID   string `json:"supplier_id"`
	DeliveryNote string `json:"delivery_note"`
	Date         string `json:"date"`
	LocationID   string `json:"location_id"`
	Note         string `json:"note"`
	Lines        []struct {
		ProductID string   `json:"product_id"`
		Qty       float64  `json:"qty"`
		Unit      string   `json:"unit"`      // unit (stock units, default) | pack
		UnitCost  *float64 `json:"unit_cost"` // per stock unit; default the PO line / last cost
		Damaged   float64  `json:"damaged"`   // stock units, recorded as wastage damaged_on_delivery
	} `json:"lines"`
}

// ReceiveDelivery books goods in: stock at the location, weighted average
// cost, price history, PO received quantities, damaged units as wastage.
func (s *Service) ReceiveDelivery(org, user string, a access, in DeliveryInput) (string, error) {
	if err := a.need(a.Edit, "receive deliveries"); err != nil {
		return "", err
	}
	if len(in.Lines) == 0 {
		return "", badReq("Add at least one line")
	}
	id := uuid.NewString()
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		loc := strings.TrimSpace(in.LocationID)
		if loc == "" {
			m, err := s.mainStore(tx, org)
			if err != nil {
				return err
			}
			loc = m
		}
		if _, err := s.location(tx, org, loc); err != nil {
			return err
		}
		if err := a.needLoc(loc); err != nil {
			return err
		}
		var po *models.PurchaseOrder
		poLines := map[string]*models.PurchaseOrderItem{}
		if strings.TrimSpace(in.POID) != "" {
			var p models.PurchaseOrder
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", in.POID, org).First(&p).Error; err != nil {
				return notFound("Purchase order not found")
			}
			if !contains(openPOStatuses, p.Status) {
				return conflict("Can't receive against a %s purchase order", strings.ReplaceAll(p.Status, "_", " "))
			}
			po = &p
			var ls []models.PurchaseOrderItem
			tx.Where("purchase_order_id = ?", p.ID).Order("created_at").Find(&ls)
			for i := range ls {
				if _, ok := poLines[ls[i].ProductID]; !ok {
					poLines[ls[i].ProductID] = &ls[i]
				}
			}
			if in.SupplierID == "" {
				in.SupplierID = p.SupplierID
			}
		}
		sid := strings.TrimSpace(in.SupplierID)
		if sid != "" {
			var n int64
			tx.Table("suppliers").Where("id = ? AND organization_id = ? AND deleted_at IS NULL", sid, org).Count(&n)
			if n == 0 {
				return badReq("Supplier not found")
			}
		}
		st := s.Settings(tx, org)
		bd := s.BusinessDateAt(tx, org, s.now())
		date := bd
		if ValidDay(in.Date) {
			date, bd = in.Date, in.Date
		}
		ids := []string{}
		for _, l := range in.Lines {
			ids = append(ids, l.ProductID)
		}
		items, err := s.Items(tx, org, uniq(ids))
		if err != nil {
			return err
		}
		label := strings.TrimSpace(in.DeliveryNote)
		if label == "" && po != nil {
			label = po.OrderNumber
		}
		cur := s.Currency(tx, org)
		total := 0.0
		touched := []string{}
		for i, l := range in.Lines {
			it, ok := items[l.ProductID]
			if !ok {
				return badReq("Line %d: choose a stock item", i+1)
			}
			if l.Qty <= 0 {
				return badReq("Line %d (%s): quantity must be more than 0", i+1, it.Name)
			}
			unit := strings.ToLower(strings.TrimSpace(l.Unit))
			units := l.Qty
			switch unit {
			case "", "unit", "units", "bottle", "stock":
				unit = "unit"
			case "pack", "packs", "case", "cases":
				unit = "pack"
				units = l.Qty * float64(max(it.PackSize, 1))
			default:
				return badReq("Line %d: unit must be unit or pack", i+1)
			}
			units = round3(units)
			if l.Damaged < 0 || l.Damaged > units+eps {
				return badReq("Line %d (%s): damaged must be between 0 and the quantity delivered", i+1, it.Name)
			}
			cost := it.LastCost
			if pl, ok := poLines[l.ProductID]; ok && pl.UnitCost > 0 {
				cost = pl.UnitCost
			}
			if l.UnitCost != nil {
				if *l.UnitCost < 0 {
					return badReq("Line %d: unit cost cannot be negative", i+1)
				}
				cost = *l.UnitCost
			}
			note := "Delivery " + label
			if err := s.receiveStock(tx, org, user, loc, l.ProductID, units, cost, sid, "delivery", id, label, bd, strings.TrimSpace(note)); err != nil {
				return err
			}
			lineTotal := units * cost
			total += lineTotal
			if l.Damaged > 0 {
				var fresh Item
				tx.Where("product_id = ?", l.ProductID).First(&fresh)
				w := Wastage{ID: uuid.NewString(), OrganizationID: org, LocationID: loc, ProductID: l.ProductID, Qty: round3(l.Damaged * it.UnitSize),
					Unit: "bottle", EnteredQty: round3(l.Damaged), Reason: "damaged_on_delivery", Note: "Delivery " + label, StaffID: user,
					Status: WApproved, BusinessDate: bd, UnitCost: fresh.AvgCost, RecordedBy: user, DecidedBy: user, RefType: "delivery", RefID: id, CreatedAt: s.now()}
				now := s.now()
				w.DecidedAt = &now
				w.Servings = SpecOf(&it.Item, st).Servings(w.Qty)
				if err := tx.Create(&w).Error; err != nil {
					return err
				}
				if err := s.postWastage(tx, org, user, &w); err != nil {
					return err
				}
			}
			if pl, ok := poLines[l.ProductID]; ok {
				if math.Abs(units-math.Round(units)) > eps {
					return badReq("Line %d (%s): receive whole units against a purchase order", i+1, it.Name)
				}
				pl.QuantityReceived += int(math.Round(units))
				if err := tx.Model(&models.PurchaseOrderItem{}).Where("id = ?", pl.ID).UpdateColumn("quantity_received", pl.QuantityReceived).Error; err != nil {
					return err
				}
			}
			if err := tx.Create(&DeliveryLine{ID: uuid.NewString(), OrganizationID: org, DeliveryID: id, ProductID: l.ProductID, Unit: unit,
				EnteredQty: round3(l.Qty), QtyUnits: units, UnitCost: math.Round(cost*10000) / 10000, Damaged: round3(l.Damaged),
				LineTotal: RoundMoney(lineTotal, cur)}).Error; err != nil {
				return err
			}
			touched = append(touched, l.ProductID)
		}
		var poID *string
		if po != nil {
			poID = &po.ID
			var ls []models.PurchaseOrderItem
			tx.Where("purchase_order_id = ?", po.ID).Find(&ls)
			complete := true
			for _, l := range ls {
				if l.QuantityReceived < l.Quantity {
					complete = false
				}
			}
			cols := map[string]any{"status": "partially_received"}
			if complete {
				now := time.Now()
				cols["status"], cols["received_date"] = "received", &now
			}
			if err := tx.Model(&models.PurchaseOrder{}).Where("id = ?", po.ID).UpdateColumns(cols).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&Delivery{ID: id, OrganizationID: org, SupplierID: sid, POID: poID, DeliveryNote: strings.TrimSpace(in.DeliveryNote), LocationID: loc,
			Date: date, BusinessDate: bd, Note: strings.TrimSpace(in.Note), TotalCost: RoundMoney(total, cur), Currency: cur, CreatedBy: user, CreatedAt: s.now()}).Error; err != nil {
			return err
		}
		return s.sync(tx, org, touched)
	})
	return id, err
}

func (s *Service) deliveryViews(db *gorm.DB, org string, ds []Delivery) []gin.H {
	if len(ds) == 0 {
		return []gin.H{}
	}
	ids, users, poIDs := []string{}, []string{}, []string{}
	for _, d := range ds {
		ids = append(ids, d.ID)
		users = append(users, d.CreatedBy)
		if d.POID != nil {
			poIDs = append(poIDs, *d.POID)
		}
	}
	var lines []DeliveryLine
	db.Where("organization_id = ? AND delivery_id IN ?", org, ids).Find(&lines)
	pids := []string{}
	for _, l := range lines {
		pids = append(pids, l.ProductID)
	}
	items, _ := s.Items(db, org, uniq(pids))
	st := s.Settings(db, org)
	sups := s.supplierNames(db, org)
	locs := s.locationNames(db, org)
	names := s.userNames(db, users)
	poNum := map[string]string{}
	type pn struct{ ID, OrderNumber string }
	var pns []pn
	if len(poIDs) > 0 {
		db.Table("purchase_orders").Select("id, order_number").Where("id IN ?", poIDs).Scan(&pns)
	}
	for _, p := range pns {
		poNum[p.ID] = p.OrderNumber
	}
	byD := map[string][]gin.H{}
	for _, l := range lines {
		v := gin.H{"product_id": l.ProductID, "unit": l.Unit, "qty": l.EnteredQty, "qty_units": l.QtyUnits, "damaged": l.Damaged,
			"unit_cost": l.UnitCost, "line_total": l.LineTotal}
		if it, ok := items[l.ProductID]; ok {
			v["name"] = it.Name
			v["qty_text"] = SpecOf(&it.Item, st).Text(l.QtyUnits * it.UnitSize)
		}
		byD[l.DeliveryID] = append(byD[l.DeliveryID], v)
	}
	out := []gin.H{}
	for _, d := range ds {
		v := gin.H{"id": d.ID, "delivery_note": d.DeliveryNote, "supplier_id": d.SupplierID, "supplier": sups[d.SupplierID], "po_id": d.POID,
			"location_id": d.LocationID, "location": locs[d.LocationID], "date": d.Date, "business_date": d.BusinessDate, "note": d.Note,
			"total_cost": d.TotalCost, "currency": d.Currency, "created_by_name": names[d.CreatedBy], "created_at": d.CreatedAt, "lines": byD[d.ID]}
		if d.POID != nil {
			v["po_number"] = poNum[*d.POID]
		}
		out = append(out, v)
	}
	return out
}

func (h *Handler) listDeliveries(c *gin.Context) {
	a := h.access(c)
	q := h.S.DB.Where("organization_id = ?", a.Org)
	if v := c.Query("from"); ValidDay(v) {
		q = q.Where("date >= ?", v)
	}
	if v := c.Query("to"); ValidDay(v) {
		q = q.Where("date <= ?", v)
	}
	if v := strings.TrimSpace(c.Query("supplier_id")); v != "" {
		q = q.Where("supplier_id = ?", v)
	}
	if a.Locs != nil {
		q = q.Where("location_id IN ?", keys(a.Locs))
	}
	var ds []Delivery
	if err := q.Order("created_at DESC").Limit(max(1, min(qInt(c, "limit", 100), 1000))).Find(&ds).Error; err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, h.S.deliveryViews(h.S.DB, a.Org, ds))
}

func (h *Handler) createDelivery(c *gin.Context) {
	a := h.access(c)
	var in DeliveryInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	id, err := h.S.ReceiveDelivery(a.Org, a.User, a, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	var d Delivery
	h.S.DB.Where("id = ?", id).First(&d)
	h.ok(c, http.StatusCreated, a, h.S.deliveryViews(h.S.DB, a.Org, []Delivery{d})[0])
}
