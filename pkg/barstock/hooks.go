package barstock

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// This file holds the hooks internal/handlers calls (wired in
// cmd/production/routes_bar.go): POS sales and voids, and receiving a
// purchase order on the Suppliers page. Each runs inside the caller's DB
// transaction, never raises an error for organisations without bar stock,
// and only touches products that bar stock manages.

// ManagedProducts returns which of the POS products have their stock kept by
// bar stock (a recipe, or a stock item sold as itself). POS skips the
// integer stock check and decrement for them.
func (s *Service) ManagedProducts(tx *gorm.DB, org string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	ids = uniq(ids)
	if len(ids) == 0 || !tablesReady(tx) {
		return out, nil
	}
	var found []string
	if err := tx.Raw(`SELECT pos_product_id FROM bar_recipes WHERE organization_id = ? AND pos_product_id IN ?
		UNION SELECT product_id FROM bar_items WHERE organization_id = ? AND product_id IN ?`, org, ids, org, ids).Scan(&found).Error; err != nil {
		return out, err
	}
	for _, id := range found {
		out[id] = true
	}
	return out, nil
}

// saleLocation picks the bar a sale comes from: the requested location, the
// terminal's bar (via the cash drawer), the default bar, else the only bar.
func (s *Service) saleLocation(tx *gorm.DB, org, requested string, drawerID *string) string {
	if requested = strings.TrimSpace(requested); requested != "" {
		if _, err := s.location(tx, org, requested); err == nil {
			return requested
		}
	}
	if drawerID != nil && *drawerID != "" {
		var term []string
		tx.Table("cash_drawers").Where("id = ? AND organization_id = ?", *drawerID, org).Limit(1).Pluck("terminal_id", &term)
		if len(term) > 0 && term[0] != "" {
			var t Terminal
			if tx.Where("organization_id = ? AND terminal_id = ?", org, term[0]).Limit(1).Find(&t).Error == nil && t.LocationID != "" {
				if _, err := s.location(tx, org, t.LocationID); err == nil {
					return t.LocationID
				}
			}
		}
	}
	return s.defaultBar(tx, org)
}

// SaleResult reports what OnSale did.
type SaleResult struct {
	LocationID string
	Touched    []string // stock items whose quantity changed
	Oversold   []string // names sold beyond the recorded stock
}

// OnSale deducts the recipes of the bar-managed lines of a POS sale from the
// selling bar (sale movements, ref = POS transaction) and records the lines
// for pour cost. It never blocks a sale unless the organisation turned on
// block_when_out.
func (s *Service) OnSale(tx *gorm.DB, org, user, requestedLoc string, txn *models.POSTransaction, items []models.POSItem) (SaleResult, error) {
	res := SaleResult{}
	pids := []string{}
	for _, it := range items {
		if it.ProductID != nil {
			pids = append(pids, *it.ProductID)
		}
	}
	managed, err := s.ManagedProducts(tx, org, pids)
	if err != nil || len(managed) == 0 {
		return res, err
	}
	loc := s.saleLocation(tx, org, requestedLoc, txn.CashDrawerID)
	res.LocationID = loc
	rs, err := s.recipesFor(tx, org, keys(managed))
	if err != nil {
		return res, err
	}
	stockIDs := []string{}
	for _, ls := range rs.lines {
		for _, l := range ls {
			stockIDs = append(stockIDs, l.ProductID)
		}
	}
	stockItems, err := s.Items(tx, org, uniq(stockIDs))
	if err != nil {
		return res, err
	}
	st := s.Settings(tx, org)
	at := txn.CreatedAt
	if at.IsZero() {
		at = s.now()
	}
	bd := BusinessDate(at, s.Location(tx, org), st.DayCutoff)
	need := map[string]float64{}
	for _, it := range items {
		if it.ProductID == nil || !managed[*it.ProductID] {
			continue
		}
		lines := rs.lines[*it.ProductID]
		cost := 0.0
		for _, l := range lines {
			si, ok := stockItems[l.ProductID]
			if !ok {
				continue
			}
			q := l.Qty * float64(it.Quantity)
			if si.Track {
				need[l.ProductID] += q
			}
			cost += q * si.AvgCost
		}
		net := NetExVAT(it.TotalPrice, txn.SubTotal, txn.DiscountAmount, it.TaxAmount, txn.TaxInclusive)
		row := SaleLine{ID: uuid.NewString(), OrganizationID: org, BusinessDate: bd, POSTransactionID: txn.ID, POSItemID: it.ID,
			POSProductID: *it.ProductID, LocationID: loc, Name: it.ItemName, Qty: it.Quantity, Gross: math.Round(it.TotalPrice*10000) / 10000,
			Net: math.Round(net*10000) / 10000, Cost: math.Round(cost*10000) / 10000, Category: recipeCategory(lines, stockItems), CreatedAt: at}
		if err := tx.Create(&row).Error; err != nil {
			return res, err
		}
	}
	if loc == "" || len(need) == 0 {
		if loc == "" && len(need) > 0 {
			log.Printf("barstock: org %s sold bar items with no bar set up; stock not deducted (sale %s)", org, txn.TransactionNumber)
		}
		return res, nil
	}
	ids := keys(need)
	stock, err := s.stockAt(tx, org, loc, ids)
	if err != nil {
		return res, err
	}
	locName := ""
	if l, err := s.location(tx, org, loc); err == nil {
		locName = l.Name
	}
	for _, pid := range ids {
		q := round3(need[pid])
		si := stockItems[pid]
		over := stock[pid] < q-eps
		note := "Sold via POS"
		if over {
			if st.BlockWhenOut {
				sp := SpecOf(&si.Item, st)
				return res, &Error{Status: http.StatusConflict, Msg: fmt.Sprintf("Not enough %s at %s (%s left)", si.Name, locName, sp.Text(math.Max(stock[pid], 0)))}
			}
			note = "Sold more than recorded at " + locName
			res.Oversold = append(res.Oversold, si.Name)
		}
		if _, err := s.post(tx, org, mv{Loc: loc, Product: pid, Qty: -q, Kind: KindSale, RefType: "pos_sale", RefID: txn.ID, RefLabel: txn.TransactionNumber,
			Note: note, UnitCost: si.AvgCost, User: user, BD: bd, Oversold: over, At: at}); err != nil {
			return res, err
		}
	}
	res.Touched = ids
	return res, s.sync(tx, org, ids)
}

// VoidResult reports what OnVoid reversed.
type VoidResult struct {
	Handled map[string]bool // POS products whose stock bar stock restored
	Touched []string
}

// OnVoid reverses the bar movements of a voided POS sale (sale_void, on the
// sale's business date) and marks its lines voided.
func (s *Service) OnVoid(tx *gorm.DB, org, user string, txn *models.POSTransaction, reason string) (VoidResult, error) {
	res := VoidResult{Handled: map[string]bool{}}
	if !tablesReady(tx) {
		return res, nil
	}
	var lines []SaleLine
	if err := tx.Where("organization_id = ? AND pos_transaction_id = ?", org, txn.ID).Find(&lines).Error; err != nil {
		return res, err
	}
	for _, l := range lines {
		res.Handled[l.POSProductID] = true
	}
	if len(lines) > 0 {
		if err := tx.Model(&SaleLine{}).Where("organization_id = ? AND pos_transaction_id = ?", org, txn.ID).Update("voided", true).Error; err != nil {
			return res, err
		}
	}
	var done int64
	tx.Model(&Movement{}).Where("organization_id = ? AND ref_type = 'pos_sale' AND ref_id = ? AND kind = ?", org, txn.ID, KindSaleVoid).Count(&done)
	if done > 0 {
		return res, nil
	}
	var mvs []Movement
	if err := tx.Where("organization_id = ? AND ref_type = 'pos_sale' AND ref_id = ? AND kind = ?", org, txn.ID, KindSale).Order("product_id").Find(&mvs).Error; err != nil {
		return res, err
	}
	note := "POS sale voided"
	if reason = strings.TrimSpace(reason); reason != "" {
		note += ": " + reason
	}
	for _, m := range mvs {
		if _, err := s.post(tx, org, mv{Loc: m.LocationID, Product: m.ProductID, Qty: -m.Qty, Kind: KindSaleVoid, RefType: "pos_sale", RefID: txn.ID,
			RefLabel: txn.TransactionNumber, Note: note, UnitCost: m.UnitCost, User: user, BD: m.BusinessDate}); err != nil {
			return res, err
		}
		res.Touched = append(res.Touched, m.ProductID)
	}
	return res, s.sync(tx, org, res.Touched)
}

// receiveStock books units (stock units) of an item into a location at a
// unit cost per stock unit: weighted average cost, last cost, price history
// and products.cost_price.
func (s *Service) receiveStock(tx *gorm.DB, org, user, loc, productID string, units, unitCost float64, supplierID, refType, refID, refLabel, bd, note string) error {
	var it Item
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("organization_id = ? AND product_id = ?", org, productID).First(&it).Error; err != nil {
		return notFound("Stock item not found")
	}
	size := it.UnitSize
	if size <= 0 {
		size = 1
	}
	base := round3(units * size)
	if base <= 0 {
		return nil
	}
	total, err := s.stockAt(tx, org, "", []string{productID})
	if err != nil {
		return err
	}
	costBase := unitCost / size
	avg := money6(AvgCost(total[productID], it.AvgCost, base, costBase))
	upd := map[string]any{"avg_cost": avg, "last_cost": math.Round(unitCost*10000) / 10000, "updated_at": s.now()}
	if supplierID != "" {
		upd["last_supplier_id"] = supplierID
	}
	if err := tx.Model(&Item{}).Where("product_id = ?", productID).Updates(upd).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.Product{}).Where("id = ?", productID).Update("cost_price", math.Round(avg*size*100)/100).Error; err != nil {
		return err
	}
	if _, err := s.post(tx, org, mv{Loc: loc, Product: productID, Qty: base, Kind: KindReceive, RefType: refType, RefID: refID, RefLabel: refLabel,
		Note: note, UnitCost: costBase, User: user, BD: bd}); err != nil {
		return err
	}
	date := bd
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	return tx.Create(&PriceHistory{ID: uuid.NewString(), OrganizationID: org, ProductID: productID, SupplierID: supplierID,
		UnitCost: math.Round(unitCost*10000) / 10000, Currency: s.Currency(tx, org), Date: date, Source: refType, RefID: refID, CreatedAt: s.now()}).Error
}

// OnPOReceive books a purchase order line received on the Suppliers page
// into the main store when the product is a bar stock item. handled=false
// leaves the legacy integer stock update to the caller.
func (s *Service) OnPOReceive(tx *gorm.DB, org, user string, order *models.PurchaseOrder, line *models.PurchaseOrderItem, qty int) (bool, int, error) {
	if qty <= 0 || !tablesReady(tx) {
		return false, 0, nil
	}
	var it Item
	if tx.Where("organization_id = ? AND product_id = ?", org, line.ProductID).Limit(1).Find(&it).Error != nil || it.ProductID == "" || !it.Track {
		return false, 0, nil
	}
	loc, err := s.mainStore(tx, org)
	if err != nil {
		log.Printf("barstock: PO %s received for bar item %s but no main store is set; using the legacy stock update", order.OrderNumber, line.ProductID)
		return false, 0, nil
	}
	bd := s.BusinessDateAt(tx, org, s.now())
	if err := s.receiveStock(tx, org, user, loc, line.ProductID, float64(qty), line.UnitCost, order.SupplierID, "purchase_order", order.ID, order.OrderNumber, bd,
		"Received against "+order.OrderNumber); err != nil {
		return false, 0, err
	}
	if err := s.sync(tx, org, []string{line.ProductID}); err != nil {
		return false, 0, err
	}
	var cur []int
	tx.Table("products").Where("id = ?", line.ProductID).Pluck("current_stock", &cur)
	n := 0
	if len(cur) > 0 {
		n = cur[0]
	}
	return true, n, nil
}
