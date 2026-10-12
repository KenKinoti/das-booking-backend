package barstock

import "math"

// AvgCost is the weighted average cost per base unit after a receipt:
// new = (on_hand × avg + qty_in × cost_in) ÷ (on_hand + qty_in);
// when nothing (or less than nothing) is on hand the new cost is cost_in.
func AvgCost(onHand, avg, qtyIn, costIn float64) float64 {
	if qtyIn <= 0 {
		return avg
	}
	if onHand <= 0 {
		return costIn
	}
	return (onHand*avg + qtyIn*costIn) / (onHand + qtyIn)
}

// AvailLine is one recipe line with the stock available for it.
type AvailLine struct {
	Qty   float64 // base units per serving
	Stock float64 // base units on hand
}

// Availability is how many servings can be made: the minimum over the lines
// of floor(stock ÷ line qty) (never below 0). ok is false when there are no
// stock lines (nothing limits the item).
func Availability(lines []AvailLine) (n float64, ok bool) {
	first := true
	for _, l := range lines {
		if l.Qty <= 0 {
			continue
		}
		v := math.Floor(l.Stock/l.Qty + eps)
		if v < 0 {
			v = 0
		}
		if first || v < n {
			n, first = v, false
		}
	}
	return n, !first
}

// Reorder is the suggested order for one item: max(0, par − on hand − on
// order) in stock units, rounded UP to whole packs.
func Reorder(par, onHand, onOrder float64, packSize int) (units float64, packs int) {
	raw := par - onHand - onOrder
	if raw <= eps {
		return 0, 0
	}
	if packSize < 1 {
		packSize = 1
	}
	packs = int(math.Ceil(raw/float64(packSize) - eps))
	return float64(packs * packSize), packs
}

// TopUp is the whole stock units needed to bring a bar back to par.
func TopUp(par, onHand float64) float64 {
	raw := par - onHand
	if raw <= eps {
		return 0
	}
	return math.Ceil(raw - eps)
}

// Flows are the period quantities of one item at one location (base units,
// all positive except Adjustments, which is signed).
type Flows struct {
	Opening      float64 `json:"opening"`
	Received     float64 `json:"received"`
	TransfersIn  float64 `json:"transfers_in"`
	TransfersOut float64 `json:"transfers_out"`
	Sold         float64 `json:"sold"`
	Wastage      float64 `json:"wastage"`
	Returns      float64 `json:"returns"`
	Adjustments  float64 `json:"adjustments"`
}

// Expected = opening + received + transfers_in − transfers_out − sold − wastage − returns (+ manual adjustments).
func (f Flows) Expected() float64 {
	return round3(f.Opening + f.Received + f.TransfersIn - f.TransfersOut - f.Sold - f.Wastage - f.Returns + f.Adjustments)
}

// Add folds one ledger movement (signed qty) into the flows. Count
// adjustments and opening entries are not flows (they set the opening).
func (f *Flows) Add(kind string, qty float64) {
	switch kind {
	case KindReceive:
		f.Received += qty
	case KindTransferIn:
		f.TransfersIn += qty
	case KindTransferOut:
		f.TransfersOut -= qty
	case KindSale, KindSaleVoid:
		f.Sold -= qty
	case KindWastage:
		f.Wastage -= qty
	case KindReturnSupplier:
		f.Returns -= qty
	case KindManualAdjust:
		f.Adjustments += qty
	}
}

// Round rounds every field to 3 decimals.
func (f Flows) Round() Flows {
	return Flows{round3(f.Opening), round3(f.Received), round3(f.TransfersIn), round3(f.TransfersOut), round3(f.Sold), round3(f.Wastage), round3(f.Returns), round3(f.Adjustments)}
}

// VariancePct = variance ÷ (sold + wastage) × 100; nil when nothing was sold or wasted.
func VariancePct(variance, sold, wastage float64) *float64 {
	d := sold + wastage
	if d <= eps {
		return nil
	}
	v := math.Round(variance/d*100*100) / 100
	return &v
}

// Flagged reports whether a variance breaks a threshold (0 = threshold off).
func Flagged(pct *float64, value float64, pctThreshold, amountThreshold float64) (bool, []string) {
	var why []string
	if pctThreshold > 0 && pct != nil && math.Abs(*pct) > pctThreshold+eps {
		why = append(why, "pct")
	}
	if amountThreshold > 0 && math.Abs(value) > amountThreshold+eps {
		why = append(why, "amount")
	}
	return len(why) > 0, why
}

// WastageNeedsApproval: comps and staff drinks above the per-shift allowance
// (servings already used + this entry) wait for a manager unless the person
// recording it can approve. Other reasons never wait.
func WastageNeedsApproval(reason string, usedServings, servings, allowance float64, canApprove bool) bool {
	if canApprove {
		return false
	}
	if reason != "comp" && reason != "staff_drink" {
		return false
	}
	return usedServings+servings > allowance+eps
}

// Discrepancy of a transfer line: issued − received (positive = lost in transit).
func Discrepancy(issued, received float64) float64 { return round3(issued - received) }

// ShareLoss splits a loss equally between the staff on shift.
func ShareLoss(loss float64, staff int) float64 {
	if staff <= 0 {
		return 0
	}
	return loss / float64(staff)
}

// PourCostPct = cost ÷ net sales × 100 (nil without sales).
func PourCostPct(cost, net float64) *float64 {
	if net <= eps {
		return nil
	}
	v := math.Round(cost/net*100*100) / 100
	return &v
}

// NetExVAT is the net (after discounts, without VAT) of a POS line: the line
// total scaled by the sale-level discount, less the line's tax when the sale
// was tax inclusive.
func NetExVAT(lineTotal, subTotal, saleDiscount, taxAmount float64, taxInclusive bool) float64 {
	factor := 1.0
	if subTotal > 0 {
		factor = (subTotal - saleDiscount) / subTotal
	}
	base := lineTotal * factor
	if taxInclusive {
		base -= taxAmount
	}
	return base
}
