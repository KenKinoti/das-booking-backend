package barstock

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TransferInput is POST /bar/transfers.
type TransferInput struct {
	FromLocationID string      `json:"from_location_id"`
	ToLocationID   string      `json:"to_location_id"`
	Lines          []LineInput `json:"lines"` // unit defaults to stock units (bottles, cans …)
	Mode           string      `json:"mode"`  // request | direct
	Note           string      `json:"note"`
}

// QtyLine is an issue / receive line; qty is in the unit given, else the
// unit the request line was entered in.
type QtyLine struct {
	ProductID string  `json:"product_id"`
	Qty       float64 `json:"qty"`
	Unit      string  `json:"unit"`
}

func (s *Service) loadTransfer(tx *gorm.DB, org, id string, lock bool) (*Transfer, []TransferLine, error) {
	var t Transfer
	q := tx.Where("id = ? AND organization_id = ?", id, org)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := q.First(&t).Error; err != nil {
		return nil, nil, notFound("Transfer not found")
	}
	var lines []TransferLine
	if err := tx.Where("transfer_id = ? AND organization_id = ?", id, org).Order("sort, id").Find(&lines).Error; err != nil {
		return nil, nil, err
	}
	return &t, lines, nil
}

// CreateTransfer records a request, or (direct) moves the stock at once.
func (s *Service) CreateTransfer(org, user string, a access, in TransferInput) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "request"
	}
	if mode != "request" && mode != "direct" {
		return "", badReq("Mode must be request or direct")
	}
	if mode == "direct" {
		if err := a.need(a.Edit, "move stock directly (managers and store keepers)"); err != nil {
			return "", err
		}
	} else if err := a.need(a.Create, "request transfers"); err != nil {
		return "", err
	}
	if in.FromLocationID == "" || in.ToLocationID == "" || in.FromLocationID == in.ToLocationID {
		return "", badReq("Choose two different locations")
	}
	if !a.canLoc(in.ToLocationID) && !a.canLoc(in.FromLocationID) {
		return "", forbidden("You are not assigned to either location")
	}
	if len(in.Lines) == 0 {
		return "", badReq("Add at least one item")
	}
	id := uuid.NewString()
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if _, err := s.location(tx, org, in.FromLocationID); err != nil {
			return err
		}
		if _, err := s.location(tx, org, in.ToLocationID); err != nil {
			return err
		}
		st := s.Settings(tx, org)
		ids := []string{}
		for _, l := range in.Lines {
			ids = append(ids, l.ProductID)
		}
		items, err := s.Items(tx, org, uniq(ids))
		if err != nil {
			return err
		}
		merged := map[string]*TransferLine{}
		order := []string{}
		for i, l := range in.Lines {
			it, ok := items[l.ProductID]
			if !ok {
				return badReq("Line %d: choose a stock item", i+1)
			}
			if l.Qty <= 0 {
				return badReq("Line %d: quantity must be more than 0", i+1)
			}
			unit := strings.ToLower(strings.TrimSpace(l.Unit))
			if unit == "" {
				unit = "bottle"
			}
			base, err := SpecOf(&it.Item, st).ToBase(l.Qty, unit)
			if err != nil {
				return badReq("Line %d (%s): %s", i+1, it.Name, err.Error())
			}
			if m, ok := merged[l.ProductID]; ok {
				m.QtyRequested = round3(m.QtyRequested + base)
				continue
			}
			merged[l.ProductID] = &TransferLine{ID: uuid.NewString(), OrganizationID: org, TransferID: id, ProductID: l.ProductID, Unit: unit,
				QtyRequested: base, UnitCost: it.AvgCost, Sort: len(order)}
			order = append(order, l.ProductID)
		}
		now := s.now()
		bd := BusinessDate(now, s.Location(tx, org), st.DayCutoff)
		t := Transfer{ID: id, OrganizationID: org, Number: nextNumber(tx, "bar_transfers", org, "TR"), FromLocationID: in.FromLocationID,
			ToLocationID: in.ToLocationID, Mode: mode, Status: TrRequested, Note: strings.TrimSpace(in.Note), BusinessDate: bd,
			RequestedBy: user, CreatedAt: now, UpdatedAt: now}
		if mode == "direct" {
			t.Status, t.IssuedBy, t.ReceivedBy, t.IssuedAt, t.ReceivedAt = TrReceived, user, user, &now, &now
		}
		if err := tx.Create(&t).Error; err != nil {
			return err
		}
		touched := []string{}
		for _, pid := range order {
			l := merged[pid]
			if mode == "direct" {
				l.QtyIssued, l.QtyReceived = ptr(l.QtyRequested), ptr(l.QtyRequested)
				if err := s.moveTransfer(tx, org, user, &t, l.ProductID, l.QtyRequested, l.QtyRequested, l.UnitCost, bd); err != nil {
					return err
				}
				touched = append(touched, pid)
			}
			if err := tx.Create(l).Error; err != nil {
				return err
			}
		}
		return s.sync(tx, org, touched)
	})
	return id, err
}

// moveTransfer posts transfer_out at the source and/or transfer_in at the destination.
func (s *Service) moveTransfer(tx *gorm.DB, org, user string, t *Transfer, pid string, out, in, cost float64, bd string) error {
	if out > 0 {
		if _, err := s.post(tx, org, mv{Loc: t.FromLocationID, Product: pid, Qty: -out, Kind: KindTransferOut, RefType: "transfer", RefID: t.ID,
			RefLabel: t.Number, UnitCost: cost, User: user, BD: bd, Note: "Transfer " + t.Number}); err != nil {
			return err
		}
	}
	if in > 0 {
		if _, err := s.post(tx, org, mv{Loc: t.ToLocationID, Product: pid, Qty: in, Kind: KindTransferIn, RefType: "transfer", RefID: t.ID,
			RefLabel: t.Number, UnitCost: cost, User: user, BD: bd, Note: "Transfer " + t.Number}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) qtyLines(tx *gorm.DB, org string, lines []TransferLine, in []QtyLine, def func(TransferLine) float64) (map[string]float64, error) {
	st := s.Settings(tx, org)
	ids := []string{}
	for _, l := range lines {
		ids = append(ids, l.ProductID)
	}
	items, err := s.Items(tx, org, ids)
	if err != nil {
		return nil, err
	}
	byID := map[string]TransferLine{}
	out := map[string]float64{}
	for _, l := range lines {
		byID[l.ProductID] = l
		if len(in) == 0 {
			out[l.ProductID] = def(l)
		}
	}
	for i, q := range in {
		l, ok := byID[q.ProductID]
		if !ok {
			return nil, badReq("Line %d: that item is not on this transfer", i+1)
		}
		if q.Qty < 0 {
			return nil, badReq("Line %d: quantity cannot be negative", i+1)
		}
		unit := q.Unit
		if unit == "" {
			unit = l.Unit
		}
		it, ok := items[q.ProductID]
		if !ok {
			return nil, badReq("Line %d: stock item not found", i+1)
		}
		base, err := SpecOf(&it.Item, st).ToBase(q.Qty, unit)
		if err != nil {
			return nil, badReq("Line %d (%s): %s", i+1, it.Name, err.Error())
		}
		out[q.ProductID] = base
	}
	// Lines not mentioned keep their default when only some were sent.
	if len(in) > 0 {
		for _, l := range lines {
			if _, ok := out[l.ProductID]; !ok {
				out[l.ProductID] = def(l)
			}
		}
	}
	return out, nil
}

// IssueTransfer sends a requested transfer (stock leaves the source).
func (s *Service) IssueTransfer(org, user string, a access, id string, in []QtyLine) error {
	if err := a.need(a.Edit, "issue transfers"); err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		t, lines, err := s.loadTransfer(tx, org, id, true)
		if err != nil {
			return err
		}
		if t.Status != TrRequested {
			return conflict("Only requested transfers can be issued (this one is %s)", t.Status)
		}
		qty, err := s.qtyLines(tx, org, lines, in, func(l TransferLine) float64 { return l.QtyRequested })
		if err != nil {
			return err
		}
		now := s.now()
		bd := s.BusinessDateAt(tx, org, now)
		items, _ := s.Items(tx, org, keysOfLines(lines))
		issuedAny := false
		for _, l := range lines {
			q := round3(qty[l.ProductID])
			cost := l.UnitCost
			if it, ok := items[l.ProductID]; ok {
				cost = it.AvgCost
			}
			if err := tx.Model(&TransferLine{}).Where("id = ?", l.ID).Updates(map[string]any{"qty_issued": q, "unit_cost": cost}).Error; err != nil {
				return err
			}
			if q > 0 {
				issuedAny = true
				if err := s.moveTransfer(tx, org, user, t, l.ProductID, q, 0, cost, bd); err != nil {
					return err
				}
			}
		}
		if !issuedAny {
			return badReq("Issue at least one item (or cancel the request)")
		}
		if err := tx.Model(&Transfer{}).Where("id = ?", t.ID).Updates(map[string]any{"status": TrIssued, "issued_by": user, "issued_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		return s.sync(tx, org, keysOfLines(lines))
	})
}

func keysOfLines(lines []TransferLine) []string {
	out := []string{}
	for _, l := range lines {
		out = append(out, l.ProductID)
	}
	return uniq(out)
}

// ReceiveTransfer books an issued transfer in at the destination; any
// difference to what was issued is the line's transfer discrepancy. The bar
// confirms what arrived: users with create may receive at a location they
// can use (store keepers and managers with edit anywhere).
func (s *Service) ReceiveTransfer(org, user string, a access, id string, in []QtyLine) error {
	if err := a.need(a.Edit || a.Create, "receive transfers"); err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		t, lines, err := s.loadTransfer(tx, org, id, true)
		if err != nil {
			return err
		}
		if !a.Edit {
			if err := a.needLoc(t.ToLocationID); err != nil {
				return err
			}
		}
		if t.Status != TrIssued {
			return conflict("Only issued transfers can be received (this one is %s)", t.Status)
		}
		issued := func(l TransferLine) float64 {
			if l.QtyIssued != nil {
				return *l.QtyIssued
			}
			return 0
		}
		qty, err := s.qtyLines(tx, org, lines, in, issued)
		if err != nil {
			return err
		}
		now := s.now()
		bd := s.BusinessDateAt(tx, org, now)
		disc := false
		for _, l := range lines {
			q := round3(qty[l.ProductID])
			d := Discrepancy(issued(l), q)
			if math.Abs(d) > eps {
				disc = true
			}
			if err := tx.Model(&TransferLine{}).Where("id = ?", l.ID).Updates(map[string]any{"qty_received": q, "discrepancy": d}).Error; err != nil {
				return err
			}
			if q > 0 {
				if err := s.moveTransfer(tx, org, user, t, l.ProductID, 0, q, l.UnitCost, bd); err != nil {
					return err
				}
			}
		}
		if err := tx.Model(&Transfer{}).Where("id = ?", t.ID).Updates(map[string]any{"status": TrReceived, "received_by": user, "received_at": now,
			"has_discrepancy": disc, "updated_at": now}).Error; err != nil {
			return err
		}
		return s.sync(tx, org, keysOfLines(lines))
	})
}

// CancelTransfer cancels a request, or returns an issued transfer to its source.
func (s *Service) CancelTransfer(org, user string, a access, id string) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		t, lines, err := s.loadTransfer(tx, org, id, true)
		if err != nil {
			return err
		}
		switch t.Status {
		case TrRequested:
			if t.RequestedBy != user {
				if err := a.need(a.Edit, "cancel other people's requests"); err != nil {
					return err
				}
			} else if err := a.need(a.Create, "cancel requests"); err != nil {
				return err
			}
		case TrIssued:
			if err := a.need(a.Edit, "cancel issued transfers"); err != nil {
				return err
			}
			bd := s.BusinessDateAt(tx, org, s.now())
			for _, l := range lines {
				if l.QtyIssued == nil || *l.QtyIssued <= 0 {
					continue
				}
				if _, err := s.post(tx, org, mv{Loc: t.FromLocationID, Product: l.ProductID, Qty: *l.QtyIssued, Kind: KindTransferIn, RefType: "transfer",
					RefID: t.ID, RefLabel: t.Number, UnitCost: l.UnitCost, User: user, BD: bd, Note: "Transfer " + t.Number + " cancelled — returned"}); err != nil {
					return err
				}
			}
			if err := s.sync(tx, org, keysOfLines(lines)); err != nil {
				return err
			}
		default:
			return conflict("A %s transfer can't be cancelled", t.Status)
		}
		now := s.now()
		return tx.Model(&Transfer{}).Where("id = ?", t.ID).Updates(map[string]any{"status": TrCancelled, "cancelled_by": user, "cancelled_at": now, "updated_at": now}).Error
	})
}

func (s *Service) transferViews(db *gorm.DB, org string, ts []Transfer) ([]gin.H, error) {
	if len(ts) == 0 {
		return []gin.H{}, nil
	}
	ids := []string{}
	users := []string{}
	for _, t := range ts {
		ids = append(ids, t.ID)
		users = append(users, t.RequestedBy, t.IssuedBy, t.ReceivedBy, t.CancelledBy)
	}
	var lines []TransferLine
	if err := db.Where("organization_id = ? AND transfer_id IN ?", org, ids).Order("sort, id").Find(&lines).Error; err != nil {
		return nil, err
	}
	pids := []string{}
	for _, l := range lines {
		pids = append(pids, l.ProductID)
	}
	items, _ := s.Items(db, org, uniq(pids))
	st := s.Settings(db, org)
	cur := s.Currency(db, org)
	locs := s.locationNames(db, org)
	names := s.userNames(db, users)
	byT := map[string][]gin.H{}
	discValue := map[string]float64{}
	value := map[string]float64{}
	for _, l := range lines {
		v := gin.H{"product_id": l.ProductID, "unit": l.Unit, "qty_requested": l.QtyRequested, "qty_issued": l.QtyIssued, "qty_received": l.QtyReceived,
			"discrepancy": l.Discrepancy, "unit_cost": money6(l.UnitCost)}
		if it, ok := items[l.ProductID]; ok {
			sp := SpecOf(&it.Item, st)
			v["name"], v["unit_label"] = it.Name, it.UnitLabel
			v["requested_qty"] = sp.FromBase(l.QtyRequested, l.Unit)
			v["requested_text"] = sp.Text(l.QtyRequested)
			if l.QtyIssued != nil {
				v["issued_qty"], v["issued_text"] = sp.FromBase(*l.QtyIssued, l.Unit), sp.Text(*l.QtyIssued)
			}
			if l.QtyReceived != nil {
				v["received_qty"], v["received_text"] = sp.FromBase(*l.QtyReceived, l.Unit), sp.Text(*l.QtyReceived)
			}
			v["discrepancy_text"] = sp.Text(l.Discrepancy)
		}
		q := l.QtyRequested
		if l.QtyIssued != nil {
			q = *l.QtyIssued
		}
		value[l.TransferID] += q * l.UnitCost
		discValue[l.TransferID] += l.Discrepancy * l.UnitCost
		v["value"] = RoundMoney(q*l.UnitCost, cur)
		byT[l.TransferID] = append(byT[l.TransferID], v)
	}
	out := []gin.H{}
	for _, t := range ts {
		ls := byT[t.ID]
		if ls == nil {
			ls = []gin.H{}
		}
		out = append(out, gin.H{"id": t.ID, "number": t.Number, "from_location_id": t.FromLocationID, "from_location": locs[t.FromLocationID],
			"to_location_id": t.ToLocationID, "to_location": locs[t.ToLocationID], "mode": t.Mode, "status": t.Status, "note": t.Note,
			"business_date": t.BusinessDate, "has_discrepancy": t.HasDiscrepancy, "requested_by": t.RequestedBy, "requested_by_name": names[t.RequestedBy],
			"issued_by_name": names[t.IssuedBy], "received_by_name": names[t.ReceivedBy], "created_at": t.CreatedAt, "issued_at": t.IssuedAt,
			"received_at": t.ReceivedAt, "cancelled_at": t.CancelledAt, "lines": ls, "line_count": len(ls),
			"value": RoundMoney(value[t.ID], cur), "discrepancy_value": RoundMoney(discValue[t.ID], cur)})
	}
	return out, nil
}

func (h *Handler) listTransfers(c *gin.Context) {
	a := h.access(c)
	q := h.S.DB.Where("organization_id = ?", a.Org)
	if v := strings.TrimSpace(c.Query("status")); v != "" && v != "all" {
		if v == "open" {
			q = q.Where("status IN ?", []string{TrRequested, TrIssued})
		} else {
			q = q.Where("status IN ?", strings.Split(v, ","))
		}
	}
	if v := strings.TrimSpace(c.Query("location_id")); v != "" {
		if err := a.needLoc(v); err != nil {
			h.fail(c, err)
			return
		}
		q = q.Where("(from_location_id = ? OR to_location_id = ?)", v, v)
	} else if a.Locs != nil {
		l := keys(a.Locs)
		q = q.Where("(from_location_id IN ? OR to_location_id IN ?)", l, l)
	}
	var ts []Transfer
	if err := q.Order("created_at DESC").Limit(max(1, min(qInt(c, "limit", 200), 1000))).Find(&ts).Error; err != nil {
		h.fail(c, err)
		return
	}
	out, err := h.S.transferViews(h.S.DB, a.Org, ts)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, out)
}

func (h *Handler) transferResponse(c *gin.Context, a access, id string, status int) {
	var t Transfer
	if err := h.S.DB.Where("id = ? AND organization_id = ?", id, a.Org).First(&t).Error; err != nil {
		h.fail(c, notFound("Transfer not found"))
		return
	}
	if !a.canLoc(t.FromLocationID) && !a.canLoc(t.ToLocationID) {
		h.fail(c, forbidden("You are not assigned to this transfer's locations"))
		return
	}
	out, err := h.S.transferViews(h.S.DB, a.Org, []Transfer{t})
	if err != nil || len(out) == 0 {
		h.fail(c, err)
		return
	}
	h.ok(c, status, a, out[0])
}

func (h *Handler) getTransfer(c *gin.Context) {
	h.transferResponse(c, h.access(c), c.Param("id"), http.StatusOK)
}

func (h *Handler) createTransfer(c *gin.Context) {
	a := h.access(c)
	var in TransferInput
	if err := bind(c, &in); err != nil {
		h.fail(c, err)
		return
	}
	id, err := h.S.CreateTransfer(a.Org, a.User, a, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.transferResponse(c, a, id, http.StatusCreated)
}

func bindLines(c *gin.Context) ([]QtyLine, error) {
	var in struct {
		Lines []QtyLine `json:"lines"`
	}
	if c.Request.ContentLength == 0 {
		return nil, nil
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		return nil, badReq("Invalid request: %s", err.Error())
	}
	return in.Lines, nil
}

func (h *Handler) issueTransfer(c *gin.Context) {
	a := h.access(c)
	lines, err := bindLines(c)
	if err == nil {
		err = h.S.IssueTransfer(a.Org, a.User, a, c.Param("id"), lines)
	}
	if err != nil {
		h.fail(c, err)
		return
	}
	h.transferResponse(c, a, c.Param("id"), http.StatusOK)
}

func (h *Handler) receiveTransfer(c *gin.Context) {
	a := h.access(c)
	lines, err := bindLines(c)
	if err == nil {
		err = h.S.ReceiveTransfer(a.Org, a.User, a, c.Param("id"), lines)
	}
	if err != nil {
		h.fail(c, err)
		return
	}
	h.transferResponse(c, a, c.Param("id"), http.StatusOK)
}

func (h *Handler) cancelTransfer(c *gin.Context) {
	a := h.access(c)
	if err := h.S.CancelTransfer(a.Org, a.User, a, c.Param("id")); err != nil {
		h.fail(c, err)
		return
	}
	h.transferResponse(c, a, c.Param("id"), http.StatusOK)
}

// SuggestTopUp proposes a transfer from the main store that brings a bar
// back to par (whole stock units).
func (s *Service) SuggestTopUp(db *gorm.DB, org, to string) (gin.H, error) {
	if _, err := s.location(db, org, to); err != nil {
		return nil, err
	}
	from, _ := s.mainStore(db, org)
	items, err := s.Items(db, org, nil)
	if err != nil {
		return nil, err
	}
	st := s.Settings(db, org)
	pars := s.pars(db, org)
	atBar, _ := s.stockAt(db, org, to, nil)
	atStore := map[string]float64{}
	if from != "" {
		atStore, _ = s.stockAt(db, org, from, nil)
	}
	lines := []gin.H{}
	for _, pid := range keys(items) {
		it := items[pid]
		p, ok := pars[parKey{to, pid}]
		if !ok || p.Par <= 0 || !it.Active || !it.Track {
			continue
		}
		sp := SpecOf(&it.Item, st)
		units := sp.StockUnits(atBar[pid])
		n := TopUp(p.Par, units)
		if n <= 0 {
			continue
		}
		storeUnits := sp.StockUnits(atStore[pid])
		lines = append(lines, gin.H{"product_id": pid, "name": it.Name, "category": it.Category, "unit": "bottle", "unit_label": it.UnitLabel,
			"par": p.Par, "on_hand": round3(units), "on_hand_text": sp.Text(atBar[pid]), "qty": n, "suggest_units": n,
			"store_on_hand": round3(storeUnits), "store_short": storeUnits < n-eps})
	}
	return gin.H{"from_location_id": nilIfEmpty(from), "to_location_id": to, "lines": lines, "generated_at": time.Now().UTC()}, nil
}

func (h *Handler) suggestTransfer(c *gin.Context) {
	a := h.access(c)
	to := strings.TrimSpace(c.Query("to_location_id"))
	if to == "" {
		to = h.S.defaultBar(h.S.DB, a.Org)
	}
	if err := a.needLoc(to); err != nil {
		h.fail(c, err)
		return
	}
	out, err := h.S.SuggestTopUp(h.S.DB, a.Org, to)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, out)
}
