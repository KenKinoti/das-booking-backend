package barstock

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Export datasets (GET /bar/reports/export/:dataset.csv).
var ExportDatasets = []string{"stock", "movements", "variance", "wastage", "valuation"}

func f3(v float64) string { return strconv.FormatFloat(round3(v), 'f', -1, 64) }

func anyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case *float64:
		if x == nil {
			return ""
		}
		return strconv.FormatFloat(*x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	}
	return fmt.Sprint(v)
}

// Export renders a dataset as CSV rows (money columns only with money=true).
func (s *Service) Export(org string, a access, dataset string, c *gin.Context) ([][]string, error) {
	db := s.DB
	st := s.Settings(db, org)
	loc := strings.TrimSpace(c.Query("location_id"))
	locs, err := a.locFilter(loc)
	if err != nil {
		return nil, err
	}
	money := a.Approve
	cur := s.Currency(db, org)
	rows := [][]string{}
	add := func(r []string, m ...string) {
		if money {
			r = append(r, m...)
		}
		rows = append(rows, r)
	}
	switch dataset {
	case "stock":
		add([]string{"product_id", "name", "sku", "category", "location", "qty", "base_unit", "qty_text", "stock_units", "par", "reorder_point"}, "avg_cost_per_unit", "value", "currency")
		items, err := s.Items(db, org, nil)
		if err != nil {
			return nil, err
		}
		byLoc, _ := s.stockByLocation(db, org, nil)
		names := s.locationNames(db, org)
		pars := s.pars(db, org)
		lm := map[string]bool{}
		for _, l := range locs {
			lm[l] = true
		}
		for _, pid := range keys(items) {
			it := items[pid]
			sp := SpecOf(&it.Item, st)
			for _, l := range keys(byLoc[pid]) {
				if locs != nil && !lm[l] {
					continue
				}
				q := byLoc[pid][l]
				p := pars[parKey{l, pid}]
				add([]string{pid, it.Name, it.SKU, it.Category, names[l], f3(q), it.BaseUnit, sp.Text(q), f3(sp.StockUnits(q)), f3(p.Par), f3(p.ReorderPoint)},
					anyStr(RoundMoney(it.AvgCost*it.UnitSize, cur)), anyStr(RoundMoney(math.Max(q, 0)*it.AvgCost, cur)), cur)
			}
		}
	case "movements":
		add([]string{"created_at", "business_date", "location", "product", "kind", "qty", "base_unit", "qty_text", "ref_type", "ref", "user", "note", "oversold"}, "unit_cost", "value", "currency")
		q := db.Where("organization_id = ?", org)
		if locs != nil {
			q = q.Where("location_id IN ?", locs)
		}
		if v := c.Query("from"); ValidDay(v) {
			q = q.Where("business_date >= ?", v)
		}
		if v := c.Query("to"); ValidDay(v) {
			q = q.Where("business_date <= ?", v)
		}
		if v := strings.TrimSpace(c.Query("product_id")); v != "" {
			q = q.Where("product_id = ?", v)
		}
		var mvs []Movement
		if err := q.Order("created_at").Limit(50000).Find(&mvs).Error; err != nil {
			return nil, err
		}
		items, _ := s.Items(db, org, nil)
		names := s.locationNames(db, org)
		users := []string{}
		for _, m := range mvs {
			users = append(users, m.UserID)
		}
		un := s.userNames(db, users)
		for _, m := range mvs {
			name, text, base := m.ProductID, f3(m.Qty), ""
			if it, ok := items[m.ProductID]; ok {
				name, text, base = it.Name, SpecOf(&it.Item, st).Text(m.Qty), it.BaseUnit
			}
			add([]string{m.CreatedAt.UTC().Format(time.RFC3339), m.BusinessDate, names[m.LocationID], name, m.Kind, f3(m.Qty), base, text, m.RefType,
				m.RefLabel, un[m.UserID], m.Note, anyStr(m.Oversold)}, anyStr(money6(m.UnitCost)), anyStr(RoundMoney(m.Qty*m.UnitCost, cur)), cur)
		}
	case "variance":
		v, err := s.Variance(db, org, VarianceQuery{LocationID: loc, From: c.Query("from"), To: c.Query("to"), CountID: c.Query("count_id"), Locs: a.Locs})
		if err != nil {
			return nil, err
		}
		add([]string{"count", "location", "product", "category", "opening", "received", "transfers_in", "transfers_out", "sold", "wastage", "returns",
			"adjustments", "expected", "counted", "variance", "variance_text", "variance_pct", "variance_tots", "flagged"}, "unit_cost", "variance_value", "currency")
		for _, r := range v["items"].([]gin.H) {
			add([]string{anyStr(r["count_number"]), anyStr(r["location"]), anyStr(r["name"]), anyStr(r["category"]), anyStr(r["opening"]), anyStr(r["received"]),
				anyStr(r["transfers_in"]), anyStr(r["transfers_out"]), anyStr(r["sold"]), anyStr(r["wastage"]), anyStr(r["returns"]), anyStr(r["adjustments"]),
				anyStr(r["expected"]), anyStr(r["counted"]), anyStr(r["variance"]), anyStr(r["variance_text"]), anyStr(r["variance_pct"]),
				anyStr(r["variance_tots"]), anyStr(r["flagged"])}, anyStr(r["unit_cost"]), anyStr(r["variance_value"]), cur)
		}
	case "wastage":
		add([]string{"business_date", "location", "product", "reason", "qty", "qty_text", "servings", "status", "staff", "recorded_by", "note"}, "value", "currency")
		q := db.Where("organization_id = ?", org)
		if locs != nil {
			q = q.Where("location_id IN ?", locs)
		}
		if v := c.Query("from"); ValidDay(v) {
			q = q.Where("business_date >= ?", v)
		}
		if v := c.Query("to"); ValidDay(v) {
			q = q.Where("business_date <= ?", v)
		}
		var ws []Wastage
		if err := q.Order("created_at").Limit(50000).Find(&ws).Error; err != nil {
			return nil, err
		}
		for _, w := range s.wastageViews(db, org, ws) {
			add([]string{anyStr(w["business_date"]), anyStr(w["location"]), anyStr(w["name"]), anyStr(w["reason"]), anyStr(w["qty"]), anyStr(w["qty_text"]),
				anyStr(w["servings"]), anyStr(w["status"]), anyStr(w["staff"]), anyStr(w["recorded_by_name"]), anyStr(w["note"])}, anyStr(w["value"]), cur)
		}
	case "valuation":
		v, err := s.Valuation(db, org, locs)
		if err != nil {
			return nil, err
		}
		add([]string{"product_id", "product", "category", "on_hand", "on_hand_text", "stock_units"}, "avg_cost_per_unit", "value", "currency")
		list := v["items"].([]gin.H)
		sort.SliceStable(list, func(i, j int) bool {
			return anyStr(list[i]["category"])+anyStr(list[i]["name"]) < anyStr(list[j]["category"])+anyStr(list[j]["name"])
		})
		for _, r := range list {
			add([]string{anyStr(r["product_id"]), anyStr(r["name"]), anyStr(r["category"]), anyStr(r["on_hand"]), anyStr(r["on_hand_text"]), anyStr(r["on_hand_units"])},
				anyStr(r["avg_cost_per_unit"]), anyStr(r["value"]), cur)
		}
	default:
		return nil, notFound("Unknown export %q (use %s)", dataset, strings.Join(ExportDatasets, ", "))
	}
	return rows, nil
}

func (h *Handler) export(c *gin.Context) {
	a := h.access(c)
	file := c.Param("file")
	dataset := strings.TrimSuffix(file, ".csv")
	rows, err := h.S.Export(a.Org, a, dataset, c)
	if err != nil {
		h.fail(c, err)
		return
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.WriteAll(rows)
	name := fmt.Sprintf("bar-%s-%s.csv", dataset, h.S.Today(h.S.DB, a.Org))
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
}
