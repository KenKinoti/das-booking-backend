package barstock

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WastageInput is POST /bar/wastage.
type WastageInput struct {
	LocationID string  `json:"location_id"`
	ProductID  string  `json:"product_id"`
	Qty        float64 `json:"qty"`
	Unit       string  `json:"unit"` // ml | unit | tot | bottle (default: base units)
	Reason     string  `json:"reason"`
	Note       string  `json:"note"`
	StaffID    string  `json:"staff_id"`
}

func wastageKind(reason string) string {
	if reason == "returned_to_supplier" {
		return KindReturnSupplier
	}
	return KindWastage
}

// usedAllowance is the comp + staff drink servings already approved for a
// staff member on a business date (their shift).
func (s *Service) usedAllowance(tx *gorm.DB, org, staff, bd string) float64 {
	var v float64
	tx.Model(&Wastage{}).Select("COALESCE(SUM(servings),0)").
		Where("organization_id = ? AND staff_id = ? AND business_date = ? AND reason IN ? AND status = ?", org, staff, bd, []string{"comp", "staff_drink"}, WApproved).
		Row().Scan(&v)
	return v
}

// RecordWastage records a loss. Comps and staff drinks above the shift
// allowance wait for approval; everything else moves stock at once.
func (s *Service) RecordWastage(org, user string, a access, in WastageInput) (string, error) {
	if err := a.need(a.Create, "record wastage"); err != nil {
		return "", err
	}
	if err := a.needLoc(in.LocationID); err != nil {
		return "", err
	}
	reason := strings.ToLower(strings.TrimSpace(in.Reason))
	if !validReason(reason) {
		return "", badReq("Reason must be one of %s", strings.Join(WastageReasons, ", "))
	}
	if in.Qty <= 0 {
		return "", badReq("Quantity must be more than 0")
	}
	id := uuid.NewString()
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if _, err := s.location(tx, org, in.LocationID); err != nil {
			return err
		}
		it, err := s.item(tx, org, in.ProductID)
		if err != nil {
			return err
		}
		st := s.Settings(tx, org)
		sp := SpecOf(&it.Item, st)
		base, err := sp.ToBase(in.Qty, in.Unit)
		if err != nil {
			return badReq("%s", err.Error())
		}
		if base <= 0 {
			return badReq("Quantity must be more than 0")
		}
		staff := strings.TrimSpace(in.StaffID)
		if staff == "" {
			staff = user
		} else {
			var n int64
			tx.Table("users").Where("id = ? AND organization_id = ? AND deleted_at IS NULL", staff, org).Count(&n)
			if n == 0 {
				return badReq("Staff member not found")
			}
		}
		now := s.now()
		bd := BusinessDate(now, s.Location(tx, org), st.DayCutoff)
		servings := sp.Servings(base)
		unit := strings.ToLower(strings.TrimSpace(in.Unit))
		if unit == "" {
			unit = it.BaseUnit
		}
		w := Wastage{ID: id, OrganizationID: org, LocationID: in.LocationID, ProductID: in.ProductID, Qty: base, Unit: unit, EnteredQty: round3(in.Qty),
			Reason: reason, Note: strings.TrimSpace(in.Note), StaffID: staff, Status: WApproved, BusinessDate: bd, UnitCost: it.AvgCost,
			Servings: servings, RecordedBy: user, CreatedAt: now}
		if WastageNeedsApproval(reason, s.usedAllowance(tx, org, staff, bd), servings, st.CompAllowancePerShift, a.Approve) {
			w.Status = WPending
		} else {
			w.DecidedAt = &now
			if a.Approve {
				w.DecidedBy = user
			}
		}
		if err := tx.Create(&w).Error; err != nil {
			return err
		}
		if w.Status == WApproved {
			return s.postWastage(tx, org, user, &w)
		}
		return nil
	})
	return id, err
}

func (s *Service) postWastage(tx *gorm.DB, org, user string, w *Wastage) error {
	note := "Wastage: " + w.Reason
	if w.Note != "" {
		note += " — " + w.Note
	}
	if _, err := s.post(tx, org, mv{Loc: w.LocationID, Product: w.ProductID, Qty: -w.Qty, Kind: wastageKind(w.Reason), RefType: "wastage", RefID: w.ID,
		Note: note, UnitCost: w.UnitCost, User: user, BD: w.BusinessDate}); err != nil {
		return err
	}
	return s.sync(tx, org, []string{w.ProductID})
}

// DecideWastage approves (moves stock) or rejects a pending entry.
func (s *Service) DecideWastage(org, user string, a access, id string, approve bool, note string) error {
	if err := a.need(a.Approve, "approve wastage"); err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var w Wastage
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", id, org).First(&w).Error; err != nil {
			return notFound("Wastage entry not found")
		}
		if w.Status != WPending {
			return conflict("This entry is already %s", w.Status)
		}
		now := s.now()
		w.DecidedBy, w.DecidedAt, w.DecisionNote = user, &now, strings.TrimSpace(note)
		if approve {
			w.Status = WApproved
			var it Item
			if tx.Where("product_id = ?", w.ProductID).First(&it).Error == nil {
				w.UnitCost = it.AvgCost
			}
		} else {
			w.Status = WRejected
		}
		if err := tx.Model(&Wastage{}).Where("id = ?", w.ID).Updates(map[string]any{"status": w.Status, "decided_by": user, "decided_at": now,
			"decision_note": w.DecisionNote, "unit_cost": w.UnitCost}).Error; err != nil {
			return err
		}
		if approve {
			return s.postWastage(tx, org, user, &w)
		}
		return nil
	})
}

func (s *Service) wastageViews(db *gorm.DB, org string, rows []Wastage) []gin.H {
	pids, users := []string{}, []string{}
	for _, w := range rows {
		pids = append(pids, w.ProductID)
		users = append(users, w.StaffID, w.RecordedBy, w.DecidedBy)
	}
	items, _ := s.Items(db, org, uniq(pids))
	st := s.Settings(db, org)
	cur := s.Currency(db, org)
	locs := s.locationNames(db, org)
	names := s.userNames(db, users)
	out := []gin.H{}
	for _, w := range rows {
		v := gin.H{"id": w.ID, "location_id": w.LocationID, "location": locs[w.LocationID], "product_id": w.ProductID, "qty": w.Qty, "unit": w.Unit,
			"entered_qty": w.EnteredQty, "reason": w.Reason, "note": w.Note, "staff_id": w.StaffID, "staff": names[w.StaffID], "status": w.Status,
			"business_date": w.BusinessDate, "servings": w.Servings, "recorded_by": w.RecordedBy, "recorded_by_name": names[w.RecordedBy],
			"decided_by_name": names[w.DecidedBy], "decided_at": w.DecidedAt, "decision_note": w.DecisionNote, "created_at": w.CreatedAt,
			"ref_type": w.RefType, "ref_id": w.RefID, "unit_cost": money6(w.UnitCost), "value": RoundMoney(w.Qty*w.UnitCost, cur)}
		if it, ok := items[w.ProductID]; ok {
			sp := SpecOf(&it.Item, st)
			v["name"], v["category"] = it.Name, it.Category
			v["qty_text"] = sp.Text(w.Qty)
			v["entered_text"] = sp.QtyText(w.Qty, w.Unit)
		}
		out = append(out, v)
	}
	return out
}

func (h *Handler) listWastage(c *gin.Context) {
	a := h.access(c)
	q := h.S.DB.Where("organization_id = ?", a.Org)
	if v := c.Query("from"); ValidDay(v) {
		q = q.Where("business_date >= ?", v)
	}
	if v := c.Query("to"); ValidDay(v) {
		q = q.Where("business_date <= ?", v)
	}
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
		q = q.Where("status IN ?", strings.Split(v, ","))
	}
	if v := strings.TrimSpace(c.Query("reason")); v != "" {
		q = q.Where("reason IN ?", strings.Split(v, ","))
	}
	var rows []Wastage
	if err := q.Order("created_at DESC").Limit(max(1, min(qInt(c, "limit", 500), 2000))).Find(&rows).Error; err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, h.S.wastageViews(h.S.DB, a.Org, rows))
}

func (h *Handler) wastageResponse(c *gin.Context, a access, id string, status int) {
	var w Wastage
	if err := h.S.DB.Where("id = ? AND organization_id = ?", id, a.Org).First(&w).Error; err != nil {
		h.fail(c, notFound("Wastage entry not found"))
		return
	}
	h.ok(c, status, a, h.S.wastageViews(h.S.DB, a.Org, []Wastage{w})[0])
}

func (h *Handler) createWastage(c *gin.Context) {
	a := h.access(c)
	var in WastageInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	id, err := h.S.RecordWastage(a.Org, a.User, a, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.wastageResponse(c, a, id, http.StatusCreated)
}

func decisionNote(c *gin.Context) string {
	var in struct {
		Note string `json:"note"`
	}
	if c.Request.ContentLength > 0 {
		_ = c.ShouldBindJSON(&in)
	}
	return in.Note
}

func (h *Handler) approveWastage(c *gin.Context) {
	a := h.access(c)
	if err := h.S.DecideWastage(a.Org, a.User, a, c.Param("id"), true, decisionNote(c)); err != nil {
		h.fail(c, err)
		return
	}
	h.wastageResponse(c, a, c.Param("id"), http.StatusOK)
}

func (h *Handler) rejectWastage(c *gin.Context) {
	a := h.access(c)
	if err := h.S.DecideWastage(a.Org, a.User, a, c.Param("id"), false, decisionNote(c)); err != nil {
		h.fail(c, err)
		return
	}
	h.wastageResponse(c, a, c.Param("id"), http.StatusOK)
}
