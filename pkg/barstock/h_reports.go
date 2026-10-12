package barstock

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------- sales / pour cost

// SalesSummary is bar sales (net ex VAT) with cost and pour cost.
type SalesSummary struct {
	From       string          `json:"from"`
	To         string          `json:"to"`
	Categories []SalesCategory `json:"categories"`
	Items      []SalesItem     `json:"items"`
	Revenue    float64         `json:"revenue"`
	Gross      float64         `json:"gross"`
	Cost       float64         `json:"cost_of_sales"`
	PourCost   *float64        `json:"pour_cost_pct"`
	Margin     float64         `json:"gross_profit"`
	Lines      int             `json:"lines"`
	Sales      int             `json:"transactions"`
}

// SalesCategory is revenue and pour cost of one category.
type SalesCategory struct {
	Category string   `json:"category"`
	Label    string   `json:"label"`
	Qty      int      `json:"qty"`
	Revenue  float64  `json:"revenue"`
	Gross    float64  `json:"gross"`
	Cost     float64  `json:"cost"`
	PourCost *float64 `json:"pour_cost_pct"`
	Target   *float64 `json:"pour_cost_target"`
	Margin   float64  `json:"gross_profit"`
	OnTarget *bool    `json:"on_target"`
}

// SalesItem is one menu item's sales.
type SalesItem struct {
	POSProductID string   `json:"pos_product_id"`
	Name         string   `json:"name"`
	Category     string   `json:"category"`
	Qty          int      `json:"qty"`
	Revenue      float64  `json:"revenue"`
	Cost         float64  `json:"cost"`
	PourCost     *float64 `json:"pour_cost_pct"`
	Margin       float64  `json:"gross_profit"`
}

// Sales sums the bar sale lines of business dates [from, to] (locs nil = all).
func (s *Service) Sales(db *gorm.DB, org, from, to string, locs []string) SalesSummary {
	st := s.Settings(db, org)
	cur := s.Currency(db, org)
	out := SalesSummary{From: from, To: to, Categories: []SalesCategory{}, Items: []SalesItem{}}
	q := db.Model(&SaleLine{}).Where("organization_id = ? AND NOT COALESCE(voided,false) AND business_date >= ? AND business_date <= ?", org, from, to)
	if locs != nil {
		q = q.Where("location_id IN ?", locs)
	}
	var rows []SaleLine
	q.Find(&rows)
	cats := map[string]*SalesCategory{}
	items := map[string]*SalesItem{}
	txns := map[string]bool{}
	for _, r := range rows {
		txns[r.POSTransactionID] = true
		c := cats[r.Category]
		if c == nil {
			c = &SalesCategory{Category: r.Category, Label: CategoryLabels[r.Category]}
			cats[r.Category] = c
		}
		c.Qty += r.Qty
		c.Revenue += r.Net
		c.Gross += r.Gross
		c.Cost += r.Cost
		it := items[r.POSProductID]
		if it == nil {
			it = &SalesItem{POSProductID: r.POSProductID, Name: r.Name, Category: r.Category}
			items[r.POSProductID] = it
		}
		it.Qty += r.Qty
		it.Revenue += r.Net
		it.Cost += r.Cost
		out.Revenue += r.Net
		out.Gross += r.Gross
		out.Cost += r.Cost
	}
	for _, k := range keys(cats) {
		c := cats[k]
		c.PourCost = PourCostPct(c.Cost, c.Revenue)
		c.Revenue, c.Gross, c.Cost = RoundMoney(c.Revenue, cur), RoundMoney(c.Gross, cur), RoundMoney(c.Cost, cur)
		c.Margin = RoundMoney(c.Revenue-c.Cost, cur)
		if t := st.PourCostTargets[k]; t != nil {
			c.Target = ptr(*t)
			if c.PourCost != nil {
				c.OnTarget = ptr(*c.PourCost <= *t+eps)
			}
		}
		out.Categories = append(out.Categories, *c)
	}
	for _, k := range keys(items) {
		it := items[k]
		it.PourCost = PourCostPct(it.Cost, it.Revenue)
		it.Revenue, it.Cost = RoundMoney(it.Revenue, cur), RoundMoney(it.Cost, cur)
		it.Margin = RoundMoney(it.Revenue-it.Cost, cur)
		out.Items = append(out.Items, *it)
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Revenue > out.Items[j].Revenue })
	out.PourCost = PourCostPct(out.Cost, out.Revenue)
	out.Revenue, out.Gross, out.Cost = RoundMoney(out.Revenue, cur), RoundMoney(out.Gross, cur), RoundMoney(out.Cost, cur)
	out.Margin = RoundMoney(out.Revenue-out.Cost, cur)
	out.Lines, out.Sales = len(rows), len(txns)
	return out
}

func (s *Service) rangeOf(db *gorm.DB, org string, c *gin.Context, defDays int) (string, string) {
	to := c.Query("to")
	if !ValidDay(to) {
		to = s.Today(db, org)
	}
	from := c.Query("from")
	if !ValidDay(from) {
		from = AddDays(to, -(defDays - 1))
	}
	if from > to {
		from, to = to, from
	}
	return from, to
}

func (a access) locFilter(loc string) ([]string, error) {
	if loc != "" {
		if !a.canLoc(loc) {
			return nil, forbidden("You are not assigned to that location")
		}
		return []string{loc}, nil
	}
	if a.Locs != nil {
		return keys(a.Locs), nil
	}
	return nil, nil
}

func (h *Handler) pourCostReport(c *gin.Context) {
	a := h.access(c)
	locs, err := a.locFilter(strings.TrimSpace(c.Query("location_id")))
	if err != nil {
		h.fail(c, err)
		return
	}
	from, to := h.S.rangeOf(h.S.DB, a.Org, c, 7)
	h.ok(c, http.StatusOK, a, h.S.Sales(h.S.DB, a.Org, from, to, locs))
}

// ---------------------------------------------------------------- variance

// VarianceQuery selects the counts of a variance report.
type VarianceQuery struct {
	LocationID string
	From, To   string
	CountID    string
	Locs       map[string]bool // location limits of the caller (nil = all)
}

type varAgg struct {
	loc, pid     string
	Flows        Flows
	counted      float64
	variance     float64
	value        float64
	soldValue    float64
	wastageValue float64
	lastCount    string
	unitCost     float64
}

// Variance builds the variance report for one count or the approved counts of
// a period: per item, category, location, count and shift (staff on shift
// share the losses equally), with flags and the pour cost of the period.
func (s *Service) Variance(db *gorm.DB, org string, q VarianceQuery) (gin.H, error) {
	st := s.Settings(db, org)
	cur := s.Currency(db, org)
	var counts []Count
	cq := db.Where("organization_id = ?", org)
	if q.CountID != "" {
		cq = cq.Where("id = ? AND status IN ?", q.CountID, []string{CSubmitted, CApproved})
	} else {
		cq = cq.Where("status = ?", CApproved)
		if q.LocationID != "" {
			cq = cq.Where("location_id = ?", q.LocationID)
		}
		if q.Locs != nil {
			cq = cq.Where("location_id IN ?", keys(q.Locs))
		}
		if ValidDay(q.From) {
			cq = cq.Where("business_date >= ?", q.From)
		}
		if ValidDay(q.To) {
			cq = cq.Where("business_date <= ?", q.To)
		}
		if !ValidDay(q.From) && !ValidDay(q.To) {
			cq = cq.Order("business_date DESC, approved_at DESC").Limit(1)
		}
	}
	if err := cq.Order("business_date, created_at").Find(&counts).Error; err != nil {
		return nil, err
	}
	if q.CountID != "" && len(counts) == 0 {
		return nil, notFound("Submitted or approved count not found")
	}
	if q.Locs != nil {
		for _, c := range counts {
			if !q.Locs[c.LocationID] {
				return nil, forbidden("You are not assigned to that location")
			}
		}
	}
	items, err := s.Items(db, org, nil)
	if err != nil {
		return nil, err
	}
	locNames := s.locationNames(db, org)
	staffIDs := []string{}
	for _, c := range counts {
		staffIDs = append(staffIDs, c.StaffIDs...)
	}
	names := s.userNames(db, staffIDs)

	aggs := map[parKey]*varAgg{}
	order := []parKey{}
	countViews := []gin.H{}
	shifts := []gin.H{}
	type staffAgg struct {
		shifts      int
		loss, net   float64
		lossTots    float64
		countsNames []string
	}
	staff := map[string]*staffAgg{}
	from, to := "", ""
	locSet := map[string]bool{}
	for ci := range counts {
		c := &counts[ci]
		locSet[c.LocationID] = true
		pf, pt := s.countPeriod(db, org, c)
		if from == "" || pf < from {
			from = pf
		}
		if pt > to {
			to = pt
		}
		var lines []CountLine
		db.Where("count_id = ? AND organization_id = ?", c.ID, org).Order("sort, id").Find(&lines)
		cValue, cLoss, cLossTots := 0.0, 0.0, 0.0
		cFlags := 0
		for li := range lines {
			l := &lines[li]
			if l.Counted == nil {
				continue
			}
			it, ok := items[l.ProductID]
			if !ok {
				continue
			}
			var r lineCalc
			cost := l.UnitCost
			if c.Status == CApproved {
				r = lineCalc{Flows: Flows{Opening: l.Opening, Received: l.Received, TransfersIn: l.TransfersIn, TransfersOut: l.TransfersOut,
					Sold: l.Sold, Wastage: l.Wasted, Returns: l.Returns, Adjustments: l.Adjustments}, Expected: l.Expected, Variance: l.Variance}
			} else {
				r, _ = s.calcLine(db, org, c, l)
				cost = it.AvgCost
			}
			k := parKey{c.LocationID, l.ProductID}
			ag := aggs[k]
			if ag == nil {
				ag = &varAgg{loc: c.LocationID, pid: l.ProductID}
				ag.Flows.Opening = r.Opening
				aggs[k] = ag
				order = append(order, k)
			}
			f := &ag.Flows
			f.Received += r.Received
			f.TransfersIn += r.TransfersIn
			f.TransfersOut += r.TransfersOut
			f.Sold += r.Sold
			f.Wastage += r.Wastage
			f.Returns += r.Returns
			f.Adjustments += r.Adjustments
			ag.counted = *l.Counted
			ag.variance += r.Variance
			val := r.Variance * cost
			ag.value += val
			ag.soldValue += r.Sold * cost
			ag.wastageValue += r.Wastage * cost
			ag.unitCost = cost
			ag.lastCount = c.Number
			cValue += val
			if val < 0 {
				cLoss -= val
			}
			sp := SpecOf(&it.Item, st)
			if r.Variance < 0 && it.BaseUnit == BaseML && sp.TotML > 0 {
				cLossTots += -r.Variance / sp.TotML
			}
			if fl, _ := Flagged(VariancePct(r.Variance, r.Sold, r.Wastage), val, st.VariancePctThreshold, st.VarianceAmountThreshold); fl {
				cFlags++
			}
		}
		staffList := []gin.H{}
		n := len(c.StaffIDs)
		for _, u := range c.StaffIDs {
			staffList = append(staffList, gin.H{"id": u, "name": names[u]})
			sa := staff[u]
			if sa == nil {
				sa = &staffAgg{}
				staff[u] = sa
			}
			sa.shifts++
			sa.loss += ShareLoss(cLoss, n)
			sa.net += ShareLoss(cValue, n)
			sa.lossTots += ShareLoss(cLossTots, n)
			sa.countsNames = append(sa.countsNames, c.Number)
		}
		countViews = append(countViews, gin.H{"id": c.ID, "number": c.Number, "type": c.Type, "status": c.Status, "location_id": c.LocationID,
			"location": locNames[c.LocationID], "business_date": c.BusinessDate, "period_from": pf, "period_to": pt,
			"variance_value": RoundMoney(cValue, cur), "loss_value": RoundMoney(cLoss, cur), "flagged_count": cFlags})
		shifts = append(shifts, gin.H{"count_id": c.ID, "number": c.Number, "business_date": c.BusinessDate, "location_id": c.LocationID,
			"location": locNames[c.LocationID], "staff": staffList, "variance_value": RoundMoney(cValue, cur), "loss_value": RoundMoney(cLoss, cur),
			"loss_value_share": RoundMoney(ShareLoss(cLoss, n), cur), "loss_tots": round3(cLossTots), "loss_tots_share": round3(ShareLoss(cLossTots, n))})
	}

	itemRows := []gin.H{}
	flags := []gin.H{}
	type catAgg struct{ value, loss, sold, wasted float64 }
	cats := map[string]*catAgg{}
	type locAgg struct {
		value, loss float64
		flags       int
	}
	locs := map[string]*locAgg{}
	totValue, totLoss, totGain := 0.0, 0.0, 0.0
	for _, k := range order {
		ag := aggs[k]
		it := items[ag.pid]
		sp := SpecOf(&it.Item, st)
		f := ag.Flows.Round()
		variance := round3(ag.variance)
		pct := VariancePct(variance, f.Sold, f.Wastage)
		fl, why := Flagged(pct, ag.value, st.VariancePctThreshold, st.VarianceAmountThreshold)
		row := gin.H{"product_id": ag.pid, "name": it.Name, "category": it.Category, "location_id": ag.loc, "location": locNames[ag.loc],
			"base_unit": it.BaseUnit, "unit_label": it.UnitLabel, "opening": f.Opening, "received": f.Received, "transfers_in": f.TransfersIn,
			"transfers_out": f.TransfersOut, "sold": f.Sold, "wastage": f.Wastage, "returns": f.Returns, "adjustments": f.Adjustments,
			"counted": round3(ag.counted), "expected": round3(ag.counted - variance), "variance": variance, "variance_text": sp.Text(variance),
			"variance_pct": pct, "unit_cost": money6(ag.unitCost), "variance_value": RoundMoney(ag.value, cur), "flagged": fl, "flag_reasons": why,
			"count_number": ag.lastCount, "counted_text": sp.Text(ag.counted), "expected_text": sp.Text(ag.counted - variance)}
		if it.BaseUnit == BaseML && sp.TotML > 0 {
			row["variance_tots"] = round3(variance / sp.TotML)
		} else {
			row["variance_tots"] = nil
		}
		itemRows = append(itemRows, row)
		if fl {
			flags = append(flags, row)
		}
		ca := cats[it.Category]
		if ca == nil {
			ca = &catAgg{}
			cats[it.Category] = ca
		}
		ca.value += ag.value
		ca.sold += ag.soldValue
		ca.wasted += ag.wastageValue
		la := locs[ag.loc]
		if la == nil {
			la = &locAgg{}
			locs[ag.loc] = la
		}
		la.value += ag.value
		if fl {
			la.flags++
		}
		if ag.value < 0 {
			ca.loss -= ag.value
			la.loss -= ag.value
			totLoss -= ag.value
		} else {
			totGain += ag.value
		}
		totValue += ag.value
	}
	sort.SliceStable(itemRows, func(i, j int) bool {
		vi, _ := itemRows[i]["variance_value"].(float64)
		vj, _ := itemRows[j]["variance_value"].(float64)
		return vi < vj
	})
	locList := keys(locSet)
	sales := s.Sales(db, org, nonEmpty(from, s.Today(db, org)), nonEmpty(to, s.Today(db, org)), locList)
	salesByCat := map[string]SalesCategory{}
	for _, c := range sales.Categories {
		salesByCat[c.Category] = c
	}
	catRows := []gin.H{}
	allCats := map[string]bool{}
	for k := range cats {
		allCats[k] = true
	}
	for k := range salesByCat {
		allCats[k] = true
	}
	for _, k := range keys(allCats) {
		ca := cats[k]
		if ca == nil {
			ca = &catAgg{}
		}
		sc := salesByCat[k]
		var pct *float64
		if d := ca.sold + ca.wasted; d > eps {
			pct = ptr(math.Round(ca.value/d*10000) / 100)
		}
		catRows = append(catRows, gin.H{"category": k, "label": CategoryLabels[k], "variance_value": RoundMoney(ca.value, cur),
			"loss_value": RoundMoney(ca.loss, cur), "variance_pct": pct, "revenue": sc.Revenue, "cost": sc.Cost, "pour_cost_pct": sc.PourCost,
			"pour_cost_target": sc.Target})
	}
	locRows := []gin.H{}
	for _, l := range keys(locs) {
		la := locs[l]
		locRows = append(locRows, gin.H{"location_id": l, "location": locNames[l], "variance_value": RoundMoney(la.value, cur),
			"loss_value": RoundMoney(la.loss, cur), "flagged_count": la.flags})
	}
	staffRows := []gin.H{}
	for _, u := range keys(staff) {
		sa := staff[u]
		staffRows = append(staffRows, gin.H{"user_id": u, "name": names[u], "shifts": sa.shifts, "counts": sa.countsNames,
			"loss_value": RoundMoney(sa.loss, cur), "net_variance_value": RoundMoney(sa.net, cur), "loss_tots": round3(sa.lossTots)})
	}
	sort.SliceStable(staffRows, func(i, j int) bool { return staffRows[i]["loss_tots"].(float64) > staffRows[j]["loss_tots"].(float64) })
	return gin.H{
		"period":     gin.H{"from": nilIfEmpty(from), "to": nilIfEmpty(to)},
		"counts":     countViews,
		"items":      itemRows,
		"categories": catRows,
		"locations":  locRows,
		"shifts":     shifts,
		"staff":      staffRows,
		"flags":      flags,
		"totals": gin.H{"variance_value": RoundMoney(totValue, cur), "loss_value": RoundMoney(totLoss, cur), "gain_value": RoundMoney(totGain, cur),
			"items": len(itemRows), "flagged_count": len(flags), "revenue": sales.Revenue, "cost_of_sales": sales.Cost, "pour_cost_pct": sales.PourCost},
		"sales":      sales,
		"thresholds": gin.H{"variance_pct": st.VariancePctThreshold, "variance_amount": st.VarianceAmountThreshold},
		"currency":   cur,
	}, nil
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (h *Handler) variance(c *gin.Context) {
	a := h.access(c)
	q := VarianceQuery{LocationID: strings.TrimSpace(c.Query("location_id")), From: c.Query("from"), To: c.Query("to"),
		CountID: strings.TrimSpace(c.Query("count_id")), Locs: a.Locs}
	if q.LocationID != "" {
		if err := a.needLoc(q.LocationID); err != nil {
			h.fail(c, err)
			return
		}
	}
	out, err := h.S.Variance(h.S.DB, a.Org, q)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, out)
}

// ---------------------------------------------------------------- valuation & slow movers

// Valuation is stock on hand × average cost.
func (s *Service) Valuation(db *gorm.DB, org string, locs []string) (gin.H, error) {
	items, err := s.Items(db, org, nil)
	if err != nil {
		return nil, err
	}
	st := s.Settings(db, org)
	cur := s.Currency(db, org)
	byLoc, err := s.stockByLocation(db, org, nil)
	if err != nil {
		return nil, err
	}
	lm := map[string]bool{}
	for _, l := range locs {
		lm[l] = true
	}
	names := s.locationNames(db, org)
	rows := []gin.H{}
	cat := map[string]float64{}
	loc := map[string]float64{}
	total := 0.0
	for _, pid := range keys(items) {
		it := items[pid]
		q := 0.0
		for l, v := range byLoc[pid] {
			if locs == nil || lm[l] {
				q += v
				loc[l] += math.Max(v, 0) * it.AvgCost
			}
		}
		if q == 0 {
			continue
		}
		sp := SpecOf(&it.Item, st)
		val := math.Max(q, 0) * it.AvgCost
		cat[it.Category] += val
		total += val
		rows = append(rows, gin.H{"product_id": pid, "name": it.Name, "category": it.Category, "on_hand": round3(q), "on_hand_text": sp.Text(q),
			"on_hand_units": round3(sp.StockUnits(q)), "avg_cost": money6(it.AvgCost), "avg_cost_per_unit": RoundMoney(it.AvgCost*it.UnitSize, cur),
			"value": RoundMoney(val, cur)})
	}
	catRows := []gin.H{}
	for _, k := range keys(cat) {
		catRows = append(catRows, gin.H{"category": k, "label": CategoryLabels[k], "value": RoundMoney(cat[k], cur)})
	}
	locRows := []gin.H{}
	for _, k := range keys(loc) {
		locRows = append(locRows, gin.H{"location_id": k, "location": names[k], "value": RoundMoney(loc[k], cur)})
	}
	return gin.H{"items": rows, "categories": catRows, "locations": locRows, "value_total": RoundMoney(total, cur), "currency": cur}, nil
}

func (h *Handler) valuationReport(c *gin.Context) {
	a := h.access(c)
	locs, err := a.locFilter(strings.TrimSpace(c.Query("location_id")))
	if err != nil {
		h.fail(c, err)
		return
	}
	out, err := h.S.Valuation(h.S.DB, a.Org, locs)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, out)
}

// SlowMovers lists stock that sold least over the last days.
func (s *Service) SlowMovers(db *gorm.DB, org string, days int, locs []string) (gin.H, error) {
	if days < 1 || days > 365 {
		days = 30
	}
	items, err := s.Items(db, org, nil)
	if err != nil {
		return nil, err
	}
	st := s.Settings(db, org)
	cur := s.Currency(db, org)
	today := s.Today(db, org)
	since := AddDays(today, -(days - 1))
	type sr struct {
		ProductID string
		Sold      float64
		Last      string
	}
	var sold []sr
	q := db.Model(&Movement{}).Select("product_id, -SUM(qty) AS sold, MAX(business_date) AS last").
		Where("organization_id = ? AND kind IN ? AND business_date >= ?", org, []string{KindSale, KindSaleVoid}, since)
	if locs != nil {
		q = q.Where("location_id IN ?", locs)
	}
	q.Group("product_id").Scan(&sold)
	sm := map[string]sr{}
	for _, x := range sold {
		sm[x.ProductID] = x
	}
	var lastEver []sr
	lq := db.Model(&Movement{}).Select("product_id, MAX(business_date) AS last").Where("organization_id = ? AND kind = ?", org, KindSale)
	if locs != nil {
		lq = lq.Where("location_id IN ?", locs)
	}
	lq.Group("product_id").Scan(&lastEver)
	le := map[string]string{}
	for _, x := range lastEver {
		le[x.ProductID] = x.Last
	}
	stock, _ := s.stockAt(db, org, "", nil)
	if locs != nil {
		stock = map[string]float64{}
		for _, l := range locs {
			m, _ := s.stockAt(db, org, l, nil)
			for k, v := range m {
				stock[k] += v
			}
		}
	}
	rows := []gin.H{}
	for _, pid := range keys(items) {
		it := items[pid]
		q := stock[pid]
		if q <= 0 || !it.Track {
			continue
		}
		sp := SpecOf(&it.Item, st)
		x := sm[pid]
		perDay := math.Max(x.Sold, 0) / float64(days)
		var cover any
		if perDay > eps {
			cover = math.Round(q/perDay*10) / 10
		}
		var last any
		if le[pid] != "" {
			last = le[pid]
		}
		rows = append(rows, gin.H{"product_id": pid, "name": it.Name, "category": it.Category, "on_hand": round3(q), "on_hand_text": sp.Text(q),
			"sold": round3(math.Max(x.Sold, 0)), "sold_text": sp.Text(math.Max(x.Sold, 0)), "last_sale_date": last, "days_of_cover": cover,
			"value": RoundMoney(q*it.AvgCost, cur), "_v": q * it.AvgCost, "_s": sp.StockUnits(math.Max(x.Sold, 0))})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		si, sj := rows[i]["_s"].(float64), rows[j]["_s"].(float64)
		if math.Abs(si-sj) > eps {
			return si < sj
		}
		return rows[i]["_v"].(float64) > rows[j]["_v"].(float64)
	})
	for _, r := range rows {
		delete(r, "_v")
		delete(r, "_s")
	}
	return gin.H{"days": days, "from": since, "to": today, "items": rows}, nil
}

func (h *Handler) slowMovers(c *gin.Context) {
	a := h.access(c)
	locs, err := a.locFilter(strings.TrimSpace(c.Query("location_id")))
	if err != nil {
		h.fail(c, err)
		return
	}
	out, err := h.S.SlowMovers(h.S.DB, a.Org, qInt(c, "days", 30), locs)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, out)
}

// ---------------------------------------------------------------- overview

// dayBounds is the UTC time range of a business date (cutoff to cutoff).
func dayBounds(day string, loc *time.Location, cutoff string) (time.Time, time.Time) {
	d, err := time.Parse("2006-01-02", day)
	if err != nil {
		d = time.Now()
	}
	h, m, ok := ParseHM(cutoff)
	if !ok {
		h, m = 6, 0
	}
	start := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
	end := time.Date(d.Year(), d.Month(), d.Day()+1, h, m, 0, 0, loc)
	return start.UTC(), end.UTC()
}

// Overview is the KPI block for a business date.
func (s *Service) Overview(db *gorm.DB, org, day string, locs map[string]bool) (gin.H, error) {
	st := s.Settings(db, org)
	cur := s.Currency(db, org)
	if !ValidDay(day) {
		day = s.Today(db, org)
	}
	var locList []string
	if locs != nil {
		locList = keys(locs)
	}
	sales := s.Sales(db, org, day, day, locList)
	start, end := dayBounds(day, s.Location(db, org), st.DayCutoff)
	var pos struct {
		Total float64
		N     int64
	}
	db.Raw(`SELECT COALESCE(SUM(total_amount),0) AS total, COUNT(*) AS n FROM pos_transactions
		WHERE organization_id = ? AND status = 'completed' AND deleted_at IS NULL AND created_at >= ? AND created_at < ?`, org, start, end).Scan(&pos)

	wq := db.Model(&Wastage{}).Where("organization_id = ? AND business_date = ? AND status = ?", org, day, WApproved)
	if locList != nil {
		wq = wq.Where("location_id IN ?", locList)
	}
	var ws []Wastage
	wq.Find(&ws)
	byReason := map[string]*struct {
		n    int
		v    float64
		serv float64
	}{}
	wv, cv, cserv := 0.0, 0.0, 0.0
	cn := 0
	for _, w := range ws {
		r := byReason[w.Reason]
		if r == nil {
			r = &struct {
				n    int
				v    float64
				serv float64
			}{}
			byReason[w.Reason] = r
		}
		r.n++
		r.v += w.Qty * w.UnitCost
		r.serv += w.Servings
		if w.Reason == "comp" || w.Reason == "staff_drink" {
			cn++
			cv += w.Qty * w.UnitCost
			cserv += w.Servings
		} else {
			wv += w.Qty * w.UnitCost
		}
	}
	reasons := []gin.H{}
	for _, k := range keys(byReason) {
		r := byReason[k]
		reasons = append(reasons, gin.H{"reason": k, "entries": r.n, "servings": round3(r.serv), "value": RoundMoney(r.v, cur)})
	}

	// Low stock at bars.
	items, _ := s.Items(db, org, nil)
	locRows, _ := s.locations(db, org)
	pars := s.pars(db, org)
	byLoc, _ := s.stockByLocation(db, org, nil)
	low := []gin.H{}
	values := []gin.H{}
	stockValue := 0.0
	for _, l := range locRows {
		if locs != nil && !locs[l.ID] {
			continue
		}
		v := 0.0
		for pid, it := range items {
			q := byLoc[pid][l.ID]
			v += math.Max(q, 0) * it.AvgCost
			if l.Type != "bar" || !it.Track || !it.Active {
				continue
			}
			p, ok := pars[parKey{l.ID, pid}]
			if !ok || (p.Par <= 0 && p.ReorderPoint <= 0) {
				continue
			}
			sp := SpecOf(&it.Item, st)
			units := sp.StockUnits(q)
			if q <= 0 || (p.ReorderPoint > 0 && units <= p.ReorderPoint+eps) {
				low = append(low, gin.H{"location_id": l.ID, "location": l.Name, "product_id": pid, "name": it.Name, "on_hand": round3(q),
					"on_hand_text": sp.Text(q), "par": p.Par, "reorder_point": p.ReorderPoint})
			}
		}
		stockValue += v
		values = append(values, gin.H{"location_id": l.ID, "location": l.Name, "type": l.Type, "value": RoundMoney(v, cur)})
	}
	sort.SliceStable(low, func(i, j int) bool { return low[i]["name"].(string) < low[j]["name"].(string) })

	var tr struct{ Requested, Issued int64 }
	tq := `SELECT COUNT(*) FILTER (WHERE status = 'requested') AS requested, COUNT(*) FILTER (WHERE status = 'issued') AS issued FROM bar_transfers WHERE organization_id = ?`
	if locList != nil {
		db.Raw(tq+" AND (from_location_id IN ? OR to_location_id IN ?)", org, locList, locList).Scan(&tr)
	} else {
		db.Raw(tq, org).Scan(&tr)
	}
	var pw, pc, oc, over int64
	db.Model(&Wastage{}).Where("organization_id = ? AND status = ?", org, WPending).Count(&pw)
	db.Model(&Count{}).Where("organization_id = ? AND status = ?", org, CSubmitted).Count(&pc)
	db.Model(&Count{}).Where("organization_id = ? AND status = ?", org, CDraft).Count(&oc)
	db.Model(&Movement{}).Where("organization_id = ? AND business_date = ? AND oversold", org, day).Count(&over)

	var last []Count
	lq := db.Where("organization_id = ? AND status = ?", org, CApproved)
	if locList != nil {
		lq = lq.Where("location_id IN ?", locList)
	}
	lq.Order("approved_at DESC").Limit(1).Find(&last)
	var lastCount any
	if len(last) > 0 {
		lc := last[0]
		var nflag int64
		lastCount = gin.H{"id": lc.ID, "number": lc.Number, "location_id": lc.LocationID, "location": s.locationNames(db, org)[lc.LocationID],
			"business_date": lc.BusinessDate, "variance_value": lc.VarianceValue, "loss_value": lc.LossValue, "flagged_count": nflag}
		if v, err := s.Variance(db, org, VarianceQuery{CountID: lc.ID}); err == nil {
			lastCount.(gin.H)["flagged_count"] = v["totals"].(gin.H)["flagged_count"]
		}
	}
	return gin.H{
		"business_date": day, "currency": cur, "day_cutoff": st.DayCutoff,
		"sales":           gin.H{"revenue": sales.Revenue, "gross": sales.Gross, "transactions": sales.Sales, "lines": sales.Lines, "categories": sales.Categories},
		"pos_sales_total": RoundMoney(pos.Total, cur), "pos_transactions": pos.N,
		"cost_of_sales": sales.Cost, "pour_cost_pct": sales.PourCost,
		"wastage":           gin.H{"entries": len(ws) - cn, "value": RoundMoney(wv, cur), "by_reason": reasons},
		"comps":             gin.H{"entries": cn, "servings": round3(cserv), "value": RoundMoney(cv, cur)},
		"low_stock_at_bars": low,
		"pending_transfers": gin.H{"requested": tr.Requested, "issued": tr.Issued, "total": tr.Requested + tr.Issued},
		"pending_approvals": gin.H{"wastage": pw, "counts": pc, "total": pw + pc},
		"open_counts":       oc, "oversold_lines": over,
		"last_count_variance": lastCount, "stock_value": RoundMoney(stockValue, cur), "stock_value_by_location": values,
	}, nil
}

func (h *Handler) overview(c *gin.Context) {
	a := h.access(c)
	out, err := h.S.Overview(h.S.DB, a.Org, c.Query("date"), a.Locs)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, out)
}
