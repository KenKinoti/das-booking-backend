package barstock

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Count types.
var CountTypes = []string{"full", "spot", "opening", "closing"}

// CountInput is POST /bar/counts.
type CountInput struct {
	LocationID string   `json:"location_id"`
	Type       string   `json:"type"`
	Blind      bool     `json:"blind"`
	ProductIDs []string `json:"product_ids"`
	StaffIDs   []string `json:"staff_ids"`
	Note       string   `json:"note"`
}

// CountLineInput is one autosaved count entry.
type CountLineInput struct {
	ProductID    string   `json:"product_id"`
	FullUnits    *float64 `json:"full_units"`
	PartialMode  string   `json:"partial_mode"`
	PartialValue float64  `json:"partial_value"`
	Clear        bool     `json:"clear"`
}

func (s *Service) loadCount(tx *gorm.DB, org, id string, lock bool) (*Count, []CountLine, error) {
	var c Count
	q := tx.Where("id = ? AND organization_id = ?", id, org)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := q.First(&c).Error; err != nil {
		return nil, nil, notFound("Count not found")
	}
	var lines []CountLine
	if err := tx.Where("count_id = ? AND organization_id = ?", id, org).Order("sort, id").Find(&lines).Error; err != nil {
		return nil, nil, err
	}
	return &c, lines, nil
}

// CreateCount starts a stock take with one line per item to count.
func (s *Service) CreateCount(org, user string, a access, in CountInput) (string, error) {
	if err := a.need(a.Create, "start counts"); err != nil {
		return "", err
	}
	if err := a.needLoc(in.LocationID); err != nil {
		return "", err
	}
	typ := strings.ToLower(strings.TrimSpace(in.Type))
	if typ == "" {
		typ = "full"
	}
	if !contains(CountTypes, typ) {
		return "", badReq("Count type must be one of %s", strings.Join(CountTypes, ", "))
	}
	if typ == "spot" && len(uniq(in.ProductIDs)) == 0 {
		return "", badReq("Choose the items for a spot count")
	}
	id := uuid.NewString()
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "barstock:count:"+org+":"+in.LocationID).Error; err != nil {
			return err
		}
		if _, err := s.location(tx, org, in.LocationID); err != nil {
			return err
		}
		var open []Count
		tx.Where("organization_id = ? AND location_id = ? AND status IN ?", org, in.LocationID, []string{CDraft, CSubmitted}).Limit(1).Find(&open)
		if len(open) > 0 {
			return conflict("Count %s at this location is still open — finish or delete it first", open[0].Number)
		}
		items, err := s.Items(tx, org, nil)
		if err != nil {
			return err
		}
		chosen := []string{}
		if len(uniq(in.ProductIDs)) > 0 {
			for _, pid := range uniq(in.ProductIDs) {
				it, ok := items[pid]
				if !ok {
					return badReq("Stock item not found")
				}
				if !it.Track {
					return badReq("%s is not stock-tracked", it.Name)
				}
				chosen = append(chosen, pid)
			}
		} else {
			// Items held or stocked (par) at the location; all items for a first count.
			var held []string
			tx.Raw(`SELECT product_id FROM bar_stock WHERE organization_id = ? AND location_id = ?
				UNION SELECT product_id FROM bar_par WHERE organization_id = ? AND location_id = ? AND (par > 0 OR reorder_point > 0)`,
				org, in.LocationID, org, in.LocationID).Scan(&held)
			hm := map[string]bool{}
			for _, h := range held {
				hm[h] = true
			}
			for pid, it := range items {
				if it.Track && it.Active && it.IsActive && (len(hm) == 0 || hm[pid]) {
					chosen = append(chosen, pid)
				}
			}
		}
		if len(chosen) == 0 {
			return badReq("There are no stock items to count yet")
		}
		sort.Slice(chosen, func(i, j int) bool {
			a, b := items[chosen[i]], items[chosen[j]]
			if a.Category != b.Category {
				return a.Category < b.Category
			}
			return a.Name < b.Name
		})
		staff := uniq(in.StaffIDs)
		if len(staff) > 0 {
			var n int64
			tx.Table("users").Where("organization_id = ? AND id IN ? AND deleted_at IS NULL", org, staff).Count(&n)
			if int(n) != len(staff) {
				return badReq("One or more staff were not found")
			}
		} else {
			staff = []string{user}
		}
		now := s.now()
		c := Count{ID: id, OrganizationID: org, Number: nextNumber(tx, "bar_counts", org, "CNT"), LocationID: in.LocationID, Type: typ, Blind: in.Blind,
			Status: CDraft, BusinessDate: s.BusinessDateAt(tx, org, now), StaffIDs: staff, Note: strings.TrimSpace(in.Note), CreatedBy: user,
			CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&c).Error; err != nil {
			return err
		}
		for i, pid := range chosen {
			if err := tx.Create(&CountLine{ID: uuid.NewString(), OrganizationID: org, CountID: id, ProductID: pid, Sort: i}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

// SaveCountLines autosaves count entries (draft counts only). The first
// entry of a line stamps counted_at: the expected quantity is the ledger at
// that moment.
func (s *Service) SaveCountLines(org, user string, a access, id string, in []CountLineInput) error {
	if err := a.need(a.Create, "count stock"); err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		c, lines, err := s.loadCount(tx, org, id, true)
		if err != nil {
			return err
		}
		if err := a.needLoc(c.LocationID); err != nil {
			return err
		}
		if c.Status != CDraft {
			return conflict("This count is %s and can't be changed", c.Status)
		}
		byProd := map[string]*CountLine{}
		for i := range lines {
			byProd[lines[i].ProductID] = &lines[i]
		}
		ids := []string{}
		for _, l := range in {
			ids = append(ids, l.ProductID)
		}
		items, err := s.Items(tx, org, uniq(ids))
		if err != nil {
			return err
		}
		st := s.Settings(tx, org)
		now := s.now()
		for i, l := range in {
			it, ok := items[l.ProductID]
			if !ok {
				return badReq("Line %d: stock item not found", i+1)
			}
			line, ok := byProd[l.ProductID]
			if !ok {
				line = &CountLine{ID: uuid.NewString(), OrganizationID: org, CountID: id, ProductID: l.ProductID, Sort: len(byProd)}
				if err := tx.Create(line).Error; err != nil {
					return err
				}
				byProd[l.ProductID] = line
			}
			if l.Clear || l.FullUnits == nil {
				if err := tx.Model(&CountLine{}).Where("id = ?", line.ID).Updates(map[string]any{"full_units": nil, "partial_mode": "", "partial_value": 0,
					"counted": nil, "counted_at": nil, "counted_by": ""}).Error; err != nil {
					return err
				}
				continue
			}
			counted, err := SpecOf(&it.Item, st).CountedBase(*l.FullUnits, l.PartialMode, l.PartialValue, it.FullWeightG, it.EmptyWeightG)
			if err != nil {
				return badReq("%s: %s", it.Name, err.Error())
			}
			upd := map[string]any{"full_units": round3(*l.FullUnits), "partial_mode": strings.ToLower(strings.TrimSpace(l.PartialMode)),
				"partial_value": round3(l.PartialValue), "counted": counted, "counted_by": user, "recount": false}
			if line.CountedAt == nil {
				upd["counted_at"] = now
			}
			if err := tx.Model(&CountLine{}).Where("id = ?", line.ID).Updates(upd).Error; err != nil {
				return err
			}
		}
		return tx.Model(&Count{}).Where("id = ?", id).Update("updated_at", now).Error
	})
}

// lineCalc is the variance maths of one count line.
type lineCalc struct {
	Flows
	PrevLineID string
	Expected   float64
	Variance   float64
}

// calcLine works out a counted line against the ledger: expected = every
// movement at the location up to the moment the line was counted; the
// breakdown runs from the previous approved count of the item there.
func (s *Service) calcLine(db *gorm.DB, org string, c *Count, l *CountLine) (lineCalc, error) {
	var r lineCalc
	at := s.now()
	if l.CountedAt != nil {
		at = *l.CountedAt
	}
	var expected float64
	if err := db.Model(&Movement{}).Select("COALESCE(SUM(qty),0)").
		Where("organization_id = ? AND location_id = ? AND product_id = ? AND created_at <= ?", org, c.LocationID, l.ProductID, at).Row().Scan(&expected); err != nil {
		return r, err
	}
	r.Expected = round3(expected)
	type prev struct {
		ID        string
		Counted   float64
		CountedAt time.Time
	}
	var p []prev
	db.Raw(`SELECT l.id, l.counted, l.counted_at FROM bar_count_lines l JOIN bar_counts c ON c.id = l.count_id
		WHERE c.organization_id = ? AND c.location_id = ? AND c.status = ? AND c.id <> ? AND l.product_id = ? AND l.counted IS NOT NULL
		  AND l.counted_at IS NOT NULL AND l.counted_at < ? ORDER BY l.counted_at DESC LIMIT 1`, org, c.LocationID, CApproved, c.ID, l.ProductID, at).Scan(&p)
	type kq struct {
		Kind string
		Qty  float64
	}
	var rows []kq
	q := db.Model(&Movement{}).Select("kind, SUM(qty) AS qty").Where("organization_id = ? AND location_id = ? AND product_id = ? AND created_at <= ?", org, c.LocationID, l.ProductID, at)
	if len(p) > 0 {
		r.PrevLineID = p[0].ID
		r.Opening = p[0].Counted
		q = q.Where("created_at > ? AND kind NOT IN ?", p[0].CountedAt, []string{KindCountAdjust, KindOpening})
	}
	if err := q.Group("kind").Scan(&rows).Error; err != nil {
		return r, err
	}
	for _, x := range rows {
		if len(p) == 0 && (x.Kind == KindOpening || x.Kind == KindCountAdjust) {
			r.Opening += x.Qty
			continue
		}
		r.Add(x.Kind, x.Qty)
	}
	r.Flows = r.Flows.Round()
	// Anything the breakdown can't explain (manual adjustments, late
	// approvals of an earlier count) keeps expected equal to the ledger.
	r.Adjustments = round3(r.Adjustments + r.Expected - r.Flows.Expected())
	if l.Counted != nil {
		r.Variance = round3(*l.Counted - r.Expected)
	}
	return r, nil
}

// SubmitCount hands a finished count to a manager.
func (s *Service) SubmitCount(org, user string, a access, id string) error {
	if err := a.need(a.Create, "submit counts"); err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		c, lines, err := s.loadCount(tx, org, id, true)
		if err != nil {
			return err
		}
		if err := a.needLoc(c.LocationID); err != nil {
			return err
		}
		if c.Status != CDraft {
			return conflict("This count is already %s", c.Status)
		}
		missing := []string{}
		for _, l := range lines {
			if l.Counted == nil {
				missing = append(missing, l.ProductID)
			}
		}
		if len(missing) > 0 {
			items, _ := s.Items(tx, org, missing)
			names := []string{}
			for _, m := range missing {
				if it, ok := items[m]; ok {
					names = append(names, it.Name)
				}
			}
			sort.Strings(names)
			if len(names) > 5 {
				names = append(names[:5], "…")
			}
			return badReq("%d item(s) not counted yet: %s (enter 0 for none)", len(missing), strings.Join(names, ", "))
		}
		now := s.now()
		return tx.Model(&Count{}).Where("id = ?", id).Updates(map[string]any{"status": CSubmitted, "submitted_by": user, "submitted_at": now, "updated_at": now}).Error
	})
}

// ApproveCount posts the variance of every line (count_adjust, or opening
// for an opening count) and locks the count.
func (s *Service) ApproveCount(org, user string, a access, id string) error {
	if err := a.need(a.Approve, "approve counts"); err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		c, lines, err := s.loadCount(tx, org, id, true)
		if err != nil {
			return err
		}
		if c.Status != CSubmitted {
			return conflict("Only submitted counts can be approved (this one is %s)", c.Status)
		}
		ids := []string{}
		for _, l := range lines {
			ids = append(ids, l.ProductID)
		}
		items, err := s.Items(tx, org, ids)
		if err != nil {
			return err
		}
		kind := KindCountAdjust
		if c.Type == "opening" {
			kind = KindOpening
		}
		cur := s.Currency(tx, org)
		total, loss := 0.0, 0.0
		for i := range lines {
			l := &lines[i]
			if l.Counted == nil {
				continue
			}
			r, err := s.calcLine(tx, org, c, l)
			if err != nil {
				return err
			}
			cost := 0.0
			if it, ok := items[l.ProductID]; ok {
				cost = it.AvgCost
			}
			if err := tx.Model(&CountLine{}).Where("id = ?", l.ID).Updates(map[string]any{"prev_line_id": r.PrevLineID, "opening": r.Opening,
				"received": r.Received, "transfers_in": r.TransfersIn, "transfers_out": r.TransfersOut, "sold": r.Sold, "wasted": r.Wastage,
				"returns": r.Returns, "adjustments": r.Adjustments, "expected": r.Expected, "variance": r.Variance, "unit_cost": cost}).Error; err != nil {
				return err
			}
			if r.Variance != 0 {
				if _, err := s.post(tx, org, mv{Loc: c.LocationID, Product: l.ProductID, Qty: r.Variance, Kind: kind, RefType: "count", RefID: c.ID,
					RefLabel: c.Number, UnitCost: cost, User: user, BD: c.BusinessDate, Note: "Count " + c.Number}); err != nil {
					return err
				}
			}
			v := r.Variance * cost
			total += v
			if v < 0 {
				loss -= v
			}
		}
		now := s.now()
		if err := tx.Model(&Count{}).Where("id = ?", id).Updates(map[string]any{"status": CApproved, "approved_by": user, "approved_at": now,
			"variance_value": RoundMoney(total, cur), "loss_value": RoundMoney(loss, cur), "updated_at": now}).Error; err != nil {
			return err
		}
		return s.sync(tx, org, ids)
	})
}

// RecountCount sends lines of a submitted count back for a recount.
func (s *Service) RecountCount(org, user string, a access, id string, productIDs []string) error {
	if err := a.need(a.Approve, "ask for recounts"); err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		c, lines, err := s.loadCount(tx, org, id, true)
		if err != nil {
			return err
		}
		if c.Status != CSubmitted {
			return conflict("Only submitted counts can be sent back (this one is %s)", c.Status)
		}
		want := map[string]bool{}
		for _, p := range productIDs {
			want[p] = true
		}
		n := 0
		for _, l := range lines {
			if len(want) > 0 && !want[l.ProductID] {
				continue
			}
			n++
			if err := tx.Model(&CountLine{}).Where("id = ?", l.ID).Updates(map[string]any{"recount": true, "full_units": nil, "partial_mode": "",
				"partial_value": 0, "counted": nil, "counted_at": nil, "counted_by": ""}).Error; err != nil {
				return err
			}
		}
		if n == 0 {
			return badReq("Those items are not on this count")
		}
		return tx.Model(&Count{}).Where("id = ?", id).Updates(map[string]any{"status": CDraft, "recount_round": c.RecountRound + 1,
			"submitted_by": "", "submitted_at": nil, "updated_at": s.now()}).Error
	})
}

// countPeriod is the business dates a count covers (from the previous
// approved count at the location).
func (s *Service) countPeriod(db *gorm.DB, org string, c *Count) (string, string) {
	var prev []Count
	db.Where("organization_id = ? AND location_id = ? AND status = ? AND id <> ? AND (business_date < ? OR (business_date = ? AND created_at < ?))",
		org, c.LocationID, CApproved, c.ID, c.BusinessDate, c.BusinessDate, c.CreatedAt).Order("business_date DESC, created_at DESC").Limit(1).Find(&prev)
	if len(prev) == 0 {
		var first []string
		db.Model(&Movement{}).Where("organization_id = ? AND location_id = ? AND business_date <> ''", org, c.LocationID).Order("business_date").Limit(1).Pluck("business_date", &first)
		if len(first) > 0 && first[0] < c.BusinessDate {
			return first[0], c.BusinessDate
		}
		return c.BusinessDate, c.BusinessDate
	}
	from := prev[0].BusinessDate
	if prev[0].Type != "opening" && from < c.BusinessDate {
		from = AddDays(from, 1)
	}
	return from, c.BusinessDate
}

// countView renders a count. Expected quantities and variances are hidden
// while a blind count is being counted, and always for users without approve.
func (s *Service) countView(db *gorm.DB, org string, a access, c *Count, lines []CountLine, withLines bool) (gin.H, error) {
	st := s.Settings(db, org)
	cur := s.Currency(db, org)
	locs := s.locationNames(db, org)
	names := s.userNames(db, append(append([]string{}, c.StaffIDs...), c.CreatedBy, c.SubmittedBy, c.ApprovedBy))
	showExpected := a.Approve && !(c.Blind && c.Status == CDraft)
	ids := []string{}
	for _, l := range lines {
		ids = append(ids, l.ProductID)
	}
	items, err := s.Items(db, org, uniq(ids))
	if err != nil {
		return nil, err
	}
	var current map[string]float64
	if showExpected && c.Status != CApproved {
		current, _ = s.stockAt(db, org, c.LocationID, ids)
	}
	staff := []gin.H{}
	for _, u := range c.StaffIDs {
		staff = append(staff, gin.H{"id": u, "name": names[u]})
	}
	counted, flagged := 0, 0
	totalValue, lossValue := 0.0, 0.0
	out := []gin.H{}
	for i := range lines {
		l := &lines[i]
		if l.Counted != nil {
			counted++
		}
		it, ok := items[l.ProductID]
		if !ok {
			continue
		}
		sp := SpecOf(&it.Item, st)
		v := gin.H{"product_id": l.ProductID, "name": it.Name, "category": it.Category, "base_unit": it.BaseUnit, "unit_size": it.UnitSize,
			"unit_label": it.UnitLabel, "full_units": l.FullUnits, "partial_mode": l.PartialMode, "partial_value": l.PartialValue,
			"counted": l.Counted, "counted_at": l.CountedAt, "recount": l.Recount, "weighable": it.FullWeightG != nil && it.EmptyWeightG != nil,
			"full_weight_g": it.FullWeightG, "empty_weight_g": it.EmptyWeightG, "tot_ml": sp.TotML}
		if l.Counted != nil {
			v["counted_text"] = sp.Text(*l.Counted)
		}
		if showExpected {
			var r lineCalc
			if c.Status == CApproved {
				r = lineCalc{Flows: Flows{Opening: l.Opening, Received: l.Received, TransfersIn: l.TransfersIn, TransfersOut: l.TransfersOut,
					Sold: l.Sold, Wastage: l.Wasted, Returns: l.Returns, Adjustments: l.Adjustments}, Expected: l.Expected, Variance: l.Variance, PrevLineID: l.PrevLineID}
			} else if l.Counted != nil {
				r, _ = s.calcLine(db, org, c, l)
			} else {
				r.Expected = current[l.ProductID]
			}
			cost := it.AvgCost
			if c.Status == CApproved {
				cost = l.UnitCost
			}
			v["expected"], v["expected_text"] = r.Expected, sp.Text(r.Expected)
			v["flows"] = r.Flows
			if l.Counted != nil {
				pct := VariancePct(r.Variance, r.Sold, r.Wastage)
				val := r.Variance * cost
				f, why := Flagged(pct, val, st.VariancePctThreshold, st.VarianceAmountThreshold)
				v["variance"], v["variance_text"], v["variance_pct"] = r.Variance, sp.Text(r.Variance), pct
				v["variance_value"], v["unit_cost"] = RoundMoney(val, cur), money6(cost)
				v["flagged"], v["flag_reasons"] = f, why
				if it.BaseUnit == BaseML && sp.TotML > 0 {
					v["variance_tots"] = round3(r.Variance / sp.TotML)
				}
				if f {
					flagged++
				}
				totalValue += val
				if val < 0 {
					lossValue -= val
				}
			}
		}
		out = append(out, v)
	}
	from, to := s.countPeriod(db, org, c)
	view := gin.H{"id": c.ID, "number": c.Number, "location_id": c.LocationID, "location": locs[c.LocationID], "type": c.Type, "blind": c.Blind,
		"status": c.Status, "business_date": c.BusinessDate, "period_from": from, "period_to": to, "staff": staff, "staff_ids": c.StaffIDs, "note": c.Note,
		"recount_round": c.RecountRound, "created_by": c.CreatedBy, "created_by_name": names[c.CreatedBy], "submitted_by_name": names[c.SubmittedBy],
		"approved_by_name": names[c.ApprovedBy], "created_at": c.CreatedAt, "submitted_at": c.SubmittedAt, "approved_at": c.ApprovedAt,
		"line_count": len(lines), "counted_count": counted, "expected_hidden": !showExpected}
	if showExpected {
		if c.Status == CApproved {
			totalValue, lossValue = c.VarianceValue, c.LossValue
		}
		view["variance_value"], view["loss_value"], view["flagged_count"] = RoundMoney(totalValue, cur), RoundMoney(lossValue, cur), flagged
	}
	if withLines {
		view["lines"] = out
	}
	return view, nil
}

func (h *Handler) listCounts(c *gin.Context) {
	a := h.access(c)
	q := h.S.DB.Where("organization_id = ?", a.Org)
	if v := strings.TrimSpace(c.Query("location_id")); v != "" {
		if err := a.needLoc(v); err != nil {
			h.fail(c, err)
			return
		}
		q = q.Where("location_id = ?", v)
	} else if a.Locs != nil {
		q = q.Where("location_id IN ?", keys(a.Locs))
	}
	if v := strings.TrimSpace(c.Query("status")); v != "" && v != "all" {
		if v == "open" {
			q = q.Where("status IN ?", []string{CDraft, CSubmitted})
		} else {
			q = q.Where("status IN ?", strings.Split(v, ","))
		}
	}
	if v := strings.TrimSpace(c.Query("type")); v != "" {
		q = q.Where("type = ?", v)
	}
	var cs []Count
	if err := q.Order("created_at DESC").Limit(max(1, min(qInt(c, "limit", 100), 500))).Find(&cs).Error; err != nil {
		h.fail(c, err)
		return
	}
	out := []gin.H{}
	for i := range cs {
		var lines []CountLine
		h.S.DB.Where("count_id = ? AND organization_id = ?", cs[i].ID, a.Org).Find(&lines)
		v, err := h.S.countView(h.S.DB, a.Org, a, &cs[i], lines, false)
		if err != nil {
			h.fail(c, err)
			return
		}
		out = append(out, v)
	}
	h.ok(c, http.StatusOK, a, out)
}

func (h *Handler) countResponse(c *gin.Context, a access, id string, status int) {
	cnt, lines, err := h.S.loadCount(h.S.DB, a.Org, id, false)
	if err != nil {
		h.fail(c, err)
		return
	}
	if err := a.needLoc(cnt.LocationID); err != nil {
		h.fail(c, err)
		return
	}
	v, err := h.S.countView(h.S.DB, a.Org, a, cnt, lines, true)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, status, a, v)
}

func (h *Handler) getCount(c *gin.Context) {
	h.countResponse(c, h.access(c), c.Param("id"), http.StatusOK)
}

func (h *Handler) createCount(c *gin.Context) {
	a := h.access(c)
	var in CountInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	id, err := h.S.CreateCount(a.Org, a.User, a, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.countResponse(c, a, id, http.StatusCreated)
}

func (h *Handler) saveCountLines(c *gin.Context) {
	a := h.access(c)
	var in struct {
		Lines []CountLineInput `json:"lines"`
	}
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	if err := h.S.SaveCountLines(a.Org, a.User, a, c.Param("id"), in.Lines); err != nil {
		h.fail(c, err)
		return
	}
	h.countResponse(c, a, c.Param("id"), http.StatusOK)
}

func (h *Handler) submitCount(c *gin.Context) {
	a := h.access(c)
	if err := h.S.SubmitCount(a.Org, a.User, a, c.Param("id")); err != nil {
		h.fail(c, err)
		return
	}
	h.countResponse(c, a, c.Param("id"), http.StatusOK)
}

func (h *Handler) approveCount(c *gin.Context) {
	a := h.access(c)
	if err := h.S.ApproveCount(a.Org, a.User, a, c.Param("id")); err != nil {
		h.fail(c, err)
		return
	}
	h.countResponse(c, a, c.Param("id"), http.StatusOK)
}

func (h *Handler) recountCount(c *gin.Context) {
	a := h.access(c)
	var in struct {
		ProductIDs []string `json:"product_ids"`
	}
	if c.Request.ContentLength > 0 {
		if err := bind(c, &in); err != nil {
			h.fail(c, err)
			return
		}
	}
	if err := h.S.RecountCount(a.Org, a.User, a, c.Param("id"), in.ProductIDs); err != nil {
		h.fail(c, err)
		return
	}
	h.countResponse(c, a, c.Param("id"), http.StatusOK)
}

func (h *Handler) deleteCount(c *gin.Context) {
	a := h.access(c)
	id := c.Param("id")
	err := h.S.DB.Transaction(func(tx *gorm.DB) error {
		cnt, _, err := h.S.loadCount(tx, a.Org, id, true)
		if err != nil {
			return err
		}
		if cnt.Status != CDraft {
			return conflict("Only draft counts can be deleted")
		}
		if cnt.CreatedBy != a.User {
			if err := a.need(a.Edit, "delete other people's counts"); err != nil {
				return err
			}
		} else if err := a.need(a.Create, "delete counts"); err != nil {
			return err
		}
		if err := tx.Where("count_id = ? AND organization_id = ?", id, a.Org).Delete(&CountLine{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ? AND organization_id = ?", id, a.Org).Delete(&Count{}).Error
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, gin.H{"deleted": true, "id": id})
}
