package barstock

import (
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// movementViews renders ledger rows with names and readable quantities.
func (s *Service) movementViews(db *gorm.DB, org string, mvs []Movement, items map[string]*ItemRow, x itemCtx) []gin.H {
	need := []string{}
	users := []string{}
	for _, m := range mvs {
		if _, ok := items[m.ProductID]; !ok {
			need = append(need, m.ProductID)
		}
		users = append(users, m.UserID)
	}
	if len(need) > 0 {
		more, _ := s.Items(db, org, uniq(need))
		for k, v := range more {
			items[k] = v
		}
	}
	locs := s.locationNames(db, org)
	names := s.userNames(db, users)
	out := []gin.H{}
	for _, m := range mvs {
		v := gin.H{"id": m.ID, "location_id": m.LocationID, "location": locs[m.LocationID], "product_id": m.ProductID, "qty": m.Qty,
			"kind": m.Kind, "ref_type": m.RefType, "ref_id": m.RefID, "ref_label": m.RefLabel, "user_id": m.UserID, "user": names[m.UserID],
			"business_date": m.BusinessDate, "note": m.Note, "oversold": m.Oversold, "created_at": m.CreatedAt,
			"unit_cost": money6(m.UnitCost), "value": RoundMoney(m.Qty*m.UnitCost, x.cur)}
		if it, ok := items[m.ProductID]; ok {
			v["name"] = it.Name
			v["qty_text"] = SpecOf(&it.Item, x.st).Text(m.Qty)
		}
		out = append(out, v)
	}
	return out
}

func (h *Handler) listStock(c *gin.Context) {
	a := h.access(c)
	db := h.S.DB
	loc := strings.TrimSpace(c.Query("location_id"))
	if loc != "" {
		if err := a.needLoc(loc); err != nil {
			h.fail(c, err)
			return
		}
		if _, err := h.S.location(db, a.Org, loc); err != nil {
			h.fail(c, err)
			return
		}
	}
	items, err := h.S.Items(db, a.Org, nil)
	if err != nil {
		h.fail(c, err)
		return
	}
	x := h.S.newItemCtx(db, a.Org)
	byLoc, err := h.S.stockByLocation(db, a.Org, nil)
	if err != nil {
		h.fail(c, err)
		return
	}
	locRows, _ := h.S.locations(db, a.Org)
	out := []gin.H{}
	totalValue := 0.0
	for _, pid := range keys(items) {
		it := items[pid]
		if !(it.Active && it.IsActive) {
			if _, has := byLoc[pid]; !has {
				continue
			}
		}
		sp := SpecOf(&it.Item, x.st)
		if loc != "" {
			q := byLoc[pid][loc]
			p := x.pars[parKey{loc, pid}]
			v := h.S.itemView(it, x, q, p.Par, p.ReorderPoint)
			v["location_id"] = loc
			if p.Par > 0 {
				v["to_par"] = TopUp(p.Par, sp.StockUnits(q))
			}
			totalValue += math.Max(q, 0) * it.AvgCost
			out = append(out, v)
			continue
		}
		total := 0.0
		per := []gin.H{}
		for _, l := range locRows {
			if !a.canLoc(l.ID) {
				continue
			}
			q, has := byLoc[pid][l.ID]
			if !has {
				continue
			}
			total += q
			per = append(per, gin.H{"location_id": l.ID, "location": l.Name, "qty": q, "qty_text": sp.Text(q)})
		}
		par, rp, _ := x.parFor(pid, a.Locs)
		v := h.S.itemView(it, x, total, par, rp)
		v["by_location"] = per
		totalValue += math.Max(total, 0) * it.AvgCost
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ci, cj := out[i]["category"].(string), out[j]["category"].(string)
		if ci != cj {
			return ci < cj
		}
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	h.ok(c, http.StatusOK, a, gin.H{"location_id": nilIfEmpty(loc), "items": out, "stock_value": RoundMoney(totalValue, x.cur), "currency": x.cur})
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (h *Handler) listMovements(c *gin.Context) {
	a := h.access(c)
	db := h.S.DB
	q := db.Where("organization_id = ?", a.Org)
	if loc := strings.TrimSpace(c.Query("location_id")); loc != "" {
		if err := a.needLoc(loc); err != nil {
			h.fail(c, err)
			return
		}
		q = q.Where("location_id = ?", loc)
	} else if a.Locs != nil {
		q = q.Where("location_id IN ?", keys(a.Locs))
	}
	if v := strings.TrimSpace(c.Query("product_id")); v != "" {
		q = q.Where("product_id = ?", v)
	}
	if v := strings.TrimSpace(c.Query("kind")); v != "" {
		q = q.Where("kind IN ?", strings.Split(v, ","))
	}
	if v := strings.TrimSpace(c.Query("ref_id")); v != "" {
		q = q.Where("ref_id = ?", v)
	}
	if v := c.Query("from"); ValidDay(v) {
		q = q.Where("business_date >= ?", v)
	}
	if v := c.Query("to"); ValidDay(v) {
		q = q.Where("business_date <= ?", v)
	}
	limit := qInt(c, "limit", 200)
	if limit < 1 || limit > 2000 {
		limit = 200
	}
	var total int64
	q.Session(&gorm.Session{}).Model(&Movement{}).Count(&total)
	var mvs []Movement
	if err := q.Session(&gorm.Session{}).Order("created_at DESC, id").Offset(max(0, qInt(c, "offset", 0))).Limit(limit).Find(&mvs).Error; err != nil {
		h.fail(c, err)
		return
	}
	x := h.S.newItemCtx(db, a.Org)
	h.ok(c, http.StatusOK, a, gin.H{"movements": h.S.movementViews(db, a.Org, mvs, map[string]*ItemRow{}, x), "total": total})
}
