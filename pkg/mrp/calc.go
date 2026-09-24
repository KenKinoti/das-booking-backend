package mrp

import "math"

// Requirement is how much of one component a work order needs.
type Requirement struct {
	ProductID   string  `json:"product_id"`
	ProductName string  `json:"product_name"`
	ProductSKU  string  `json:"product_sku"`
	Unit        string  `json:"unit"`
	PerBatch    int     `json:"per_batch"`
	Required    int     `json:"required"`
	Available   int     `json:"available"`
	Shortage    int     `json:"shortage"`
	UnitCost    float64 `json:"unit_cost"`
	LineCost    float64 `json:"line_cost"`
}

// Required returns how many units of a component are consumed to build qty
// units when perBatch units of the component make outputQty units of product.
// Stock is counted in whole units, so partial units are rounded up.
func Required(perBatch, outputQty, qty int) int {
	if outputQty <= 0 {
		outputQty = 1
	}
	if perBatch <= 0 || qty <= 0 {
		return 0
	}
	// integer ceil(perBatch*qty/outputQty) without float error
	n := perBatch * qty
	return (n + outputQty - 1) / outputQty
}

// Requirements computes each component's need against current stock.
// Components listed more than once are merged.
func Requirements(comps []BOMComponent, outputQty, qty int, stock map[string]int, cost map[string]float64) []Requirement {
	idx := map[string]int{}
	out := []Requirement{}
	for _, c := range comps {
		if i, seen := idx[c.ProductID]; seen {
			out[i].PerBatch += c.Quantity
			continue
		}
		idx[c.ProductID] = len(out)
		out = append(out, Requirement{ProductID: c.ProductID, ProductName: c.ProductName, ProductSKU: c.ProductSKU, Unit: c.Unit, PerBatch: c.Quantity})
	}
	for i := range out {
		r := &out[i]
		r.Required = Required(r.PerBatch, outputQty, qty)
		r.Available = stock[r.ProductID]
		if r.Required > r.Available {
			r.Shortage = r.Required - r.Available
		}
		r.UnitCost = cost[r.ProductID]
		r.LineCost = math.Round(float64(r.Required)*r.UnitCost*100) / 100
	}
	return out
}

// UnitCost is the material cost of one finished unit.
func UnitCost(comps []BOMComponent, outputQty int, cost map[string]float64) float64 {
	if outputQty <= 0 {
		outputQty = 1
	}
	total := 0.0
	for _, c := range comps {
		total += float64(c.Quantity) * cost[c.ProductID]
	}
	return math.Round(total/float64(outputQty)*100) / 100
}
