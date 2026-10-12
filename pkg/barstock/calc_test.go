package barstock

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAvgCost(t *testing.T) {
	// 6 bottles (4500 ml) at 3.20/ml, receive 12 bottles (9000 ml) at 3.50/ml.
	got := AvgCost(4500, 3.2, 9000, 3.5)
	want := (4500*3.2 + 9000*3.5) / 13500
	if !near(got, want) {
		t.Errorf("avg = %v want %v", got, want)
	}
	if got := AvgCost(0, 9, 750, 3.2); got != 3.2 {
		t.Errorf("empty stock takes the new cost, got %v", got)
	}
	if got := AvgCost(-100, 9, 750, 3.2); got != 3.2 {
		t.Errorf("negative stock takes the new cost, got %v", got)
	}
	if got := AvgCost(1000, 3, 0, 9); got != 3 {
		t.Errorf("nothing received keeps the cost, got %v", got)
	}
}

func TestAvailability(t *testing.T) {
	// Mojito: 50 ml rum (stock 1000), 20 ml lime (stock 90), 1 mint (stock 30), 100 ml soda (stock 0 units → 0)
	n, ok := Availability([]AvailLine{{50, 1000}, {20, 90}, {1, 30}})
	if !ok || n != 4 {
		t.Errorf("availability = %v %v, want 4", n, ok)
	}
	n, _ = Availability([]AvailLine{{25, 2670}})
	if n != 106 {
		t.Errorf("tots = %v", n)
	}
	n, _ = Availability([]AvailLine{{25, -50}})
	if n != 0 {
		t.Errorf("negative stock = %v", n)
	}
	if _, ok := Availability(nil); ok {
		t.Error("no lines = unlimited")
	}
	n, _ = Availability([]AvailLine{{750, 1499.999}})
	if n != 1 {
		t.Errorf("floor just below a bottle = %v", n)
	}
	n, _ = Availability([]AvailLine{{750, 1500}})
	if n != 2 {
		t.Errorf("exact = %v", n)
	}
}

func TestReorder(t *testing.T) {
	cases := []struct {
		par, onHand, onOrder float64
		pack                 int
		units                float64
		packs                int
	}{
		{24, 5, 0, 12, 24, 2},  // need 19 → 2 cases
		{24, 12, 0, 12, 12, 1}, // need 12 → 1 case exactly
		{24, 13, 0, 12, 12, 1}, // need 11 → 1 case
		{24, 30, 0, 12, 0, 0},  // above par
		{24, 5, 19, 12, 0, 0},  // on order covers it
		{24, 5, 10, 12, 12, 1}, // need 9 → 1 case
		{10, 2.5, 0, 1, 8, 8},  // single units round up
		{10, 2.5, 0, 0, 8, 8},  // pack 0 = 1
		{50, 0, 0, 25, 50, 2},  // crates of 25
	}
	for _, c := range cases {
		u, p := Reorder(c.par, c.onHand, c.onOrder, c.pack)
		if u != c.units || p != c.packs {
			t.Errorf("Reorder(%v,%v,%v,%d) = %v,%d want %v,%d", c.par, c.onHand, c.onOrder, c.pack, u, p, c.units, c.packs)
		}
	}
	if got := TopUp(12, 3.4); got != 9 {
		t.Errorf("top up = %v", got)
	}
	if got := TopUp(12, 12); got != 0 {
		t.Errorf("top up at par = %v", got)
	}
}

func TestFlowsAndVariance(t *testing.T) {
	var f Flows
	f.Opening = 1500
	for _, m := range []struct {
		k string
		q float64
	}{
		{KindReceive, 9000}, {KindTransferIn, 3000}, {KindTransferOut, -1500}, {KindSale, -2025}, {KindSaleVoid, 75},
		{KindWastage, -50}, {KindReturnSupplier, -750}, {KindManualAdjust, 10}, {KindCountAdjust, -999}, {KindOpening, 999},
	} {
		f.Add(m.k, m.q)
	}
	if f.Received != 9000 || f.TransfersIn != 3000 || f.TransfersOut != 1500 || f.Sold != 1950 || f.Wastage != 50 || f.Returns != 750 || f.Adjustments != 10 {
		t.Fatalf("flows = %+v", f)
	}
	// expected = 1500 + 9000 + 3000 − 1500 − 1950 − 50 − 750 + 10
	if got := f.Expected(); got != 9260 {
		t.Errorf("expected = %v", got)
	}
	counted := 9180.0
	variance := round3(counted - f.Expected())
	if variance != -80 {
		t.Errorf("variance = %v", variance)
	}
	pct := VariancePct(variance, f.Sold, f.Wastage)
	if pct == nil || *pct != -4 {
		t.Errorf("pct = %v", pct)
	}
	if VariancePct(-10, 0, 0) != nil {
		t.Error("pct without sales should be nil")
	}
	fl, why := Flagged(pct, -80*3.2, 3, 0)
	if !fl || len(why) != 1 || why[0] != "pct" {
		t.Errorf("flag pct = %v %v", fl, why)
	}
	fl, why = Flagged(pct, -256, 5, 200)
	if !fl || why[0] != "amount" {
		t.Errorf("flag amount = %v %v", fl, why)
	}
	if fl, _ := Flagged(pct, -256, 0, 0); fl {
		t.Error("thresholds off never flag")
	}
	if fl, _ := Flagged(pct, -256, 4, 0); fl {
		t.Error("exactly at the threshold is not flagged")
	}
}

func TestWastageApproval(t *testing.T) {
	cases := []struct {
		reason          string
		used, n, allow  float64
		approver, needs bool
	}{
		{"comp", 0, 1, 0, false, true},          // allowance 0: every comp waits
		{"comp", 0, 1, 2, false, false},         // within allowance
		{"comp", 1, 1, 2, false, false},         // exactly at the allowance
		{"comp", 2, 1, 2, false, true},          // above
		{"staff_drink", 1.5, 1, 2, false, true}, // shared allowance
		{"comp", 5, 30, 2, true, false},         // managers are never held
		{"spillage", 100, 100, 0, false, false}, // other reasons never wait
		{"breakage", 0, 1, 0, false, false},
	}
	for i, c := range cases {
		if got := WastageNeedsApproval(c.reason, c.used, c.n, c.allow, c.approver); got != c.needs {
			t.Errorf("case %d: needs approval = %v want %v", i, got, c.needs)
		}
	}
}

func TestTransferDiscrepancyAndShares(t *testing.T) {
	if got := Discrepancy(9000, 8250); got != 750 {
		t.Errorf("discrepancy = %v", got)
	}
	if got := Discrepancy(24, 24); got != 0 {
		t.Errorf("no discrepancy = %v", got)
	}
	if got := ShareLoss(300, 3); got != 100 {
		t.Errorf("share = %v", got)
	}
	if got := ShareLoss(300, 0); got != 0 {
		t.Errorf("share none = %v", got)
	}
}

func TestPourCostAndNet(t *testing.T) {
	p := PourCostPct(80, 400)
	if p == nil || *p != 20 {
		t.Errorf("pour cost = %v", p)
	}
	if PourCostPct(10, 0) != nil {
		t.Error("no sales = nil")
	}
	// 3 tots at 300 = 900 incl. 16% VAT, no discount: tax = 900×16/116.
	tax := math.Round(900.0*16/116*100) / 100
	if got := NetExVAT(900, 900, 0, tax, true); !near(got, 900-tax) {
		t.Errorf("net incl = %v", got)
	}
	// Exclusive VAT: the line total is already net.
	if got := NetExVAT(900, 900, 0, 144, false); got != 900 {
		t.Errorf("net excl = %v", got)
	}
	// A 10% sale discount scales the line.
	if got := NetExVAT(500, 1000, 100, 0, false); got != 450 {
		t.Errorf("net discounted = %v", got)
	}
}

func TestCatalog(t *testing.T) {
	if len(Catalog) < 25 || len(Catalog) > 45 {
		t.Errorf("catalogue has %d items", len(Catalog))
	}
	seen := map[string]bool{}
	cats := map[string]bool{}
	for _, c := range Catalog {
		if seen[c.Key] {
			t.Errorf("duplicate key %s", c.Key)
		}
		seen[c.Key] = true
		cats[c.Category] = true
		if !validCategory(c.Category) || c.UnitSize <= 0 || c.PackSize < 1 || c.UnitLabel == "" || c.PackLabel == "" {
			t.Errorf("bad entry %+v", c)
		}
		if c.BaseUnit != BaseML && c.BaseUnit != BaseUnit {
			t.Errorf("bad base unit %+v", c)
		}
	}
	for _, want := range []string{"beer", "spirits", "wine", "soft_drinks", "mixers", "energy", "water", "garnish"} {
		if !cats[want] {
			t.Errorf("catalogue lacks %s", want)
		}
	}
	sizes := map[float64]bool{}
	for _, c := range Catalog {
		if c.Category == "spirits" {
			sizes[c.UnitSize] = true
		}
	}
	for _, s := range []float64{250, 750, 1000} {
		if !sizes[s] {
			t.Errorf("no %v ml spirit", s)
		}
	}
}
