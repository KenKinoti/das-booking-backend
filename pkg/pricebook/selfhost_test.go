package pricebook

import (
	"strings"
	"testing"
)

func TestBreakEvenSites(t *testing.T) {
	for _, c := range []struct {
		total, plan float64
		want        int
	}{{10595, 559, 19}, {10595, 1417.71, 8}, {1000, 100, 10}, {1001, 100, 11}, {50, 100, 1}, {0, 100, 0}, {1000, 0, 0}} {
		if got := BreakEvenSites(c.total, c.plan); got != c.want {
			t.Errorf("BreakEvenSites(%v, %v) = %d, want %d", c.total, c.plan, got, c.want)
		}
	}
	// At the break-even the server is at or below the plan; one site fewer it is not.
	n := BreakEvenSites(10595, 559)
	if 10595/float64(n) > 559 || 10595/float64(n-1) <= 559 {
		t.Fatalf("break-even %d is not the smallest number of sites", n)
	}
}

func TestSelfHostModel(t *testing.T) {
	d := fixture()
	self := d.Suppliers[2]
	m := SelfHostFull(self, d, CompareInput{Tier: "starter"}.Normalize(d.Tiers))
	// (40 + 15) USD × 129 = 7,095 + 500 backups + 2 h × 1,500 = 10,595 KES a month.
	near(t, "monthly total", m.MonthlyTotal, 10595)
	near(t, "monthly home", m.MonthlyHome, 10595)
	near(t, "yearly", m.YearlyHome, 127140)
	if len(m.Rows) != 4 {
		t.Fatalf("rows: %d", len(m.Rows))
	}
	near(t, "server line in its own currency", m.Rows[0].Monthly, 40)
	near(t, "server line in KES", m.Rows[0].MonthlyLocal, 5160)
	near(t, "time line", m.Rows[3].Monthly, 3000)
	near(t, "per site now (12)", *m.PerSiteCurrent, 882.92)
	near(t, "per site full (40)", *m.PerSiteCapacity, 264.88)
	near(t, "per site now, home", *m.PerSiteCurrentHome, 882.92)

	// Break-even vs each wholesale supplier's cheapest plan for the need.
	if len(m.BreakEven) != 2 {
		t.Fatalf("break-even rows: %+v", m.BreakEven)
	}
	syn, hc := m.BreakEven[0], m.BreakEven[1]
	if syn.SupplierID != "syn" || syn.Plan != "Starter Env" || hc.SupplierID != "hc" || hc.Plan != "Launch" {
		t.Fatalf("plans: %s / %s", syn.Plan, hc.Plan)
	}
	// Synergy Starter 6.50 AUD = 559 KES a month → ceil(10595 ÷ 559) = 19 sites.
	near(t, "synergy plan home", syn.PlanHome, 559)
	if syn.Sites != 19 || !syn.Reachable {
		t.Fatalf("synergy break-even: %+v", syn)
	}
	near(t, "saving now (12 sites)", syn.SavingCurrent, 559*12-10595)
	near(t, "saving full (40 sites)", syn.SavingCapacity, 559*40-10595)
	if !strings.Contains(syn.Text, "from 19 sites") || !strings.Contains(syn.Text, "7 more than now") {
		t.Fatalf("text: %s", syn.Text)
	}
	// hosting.com Launch at its regular price: 131.88 ÷ 12 = 10.99 USD = 1,417.71 KES → 8 sites.
	near(t, "hosting.com plan monthly", hc.PlanMonthly, 10.99)
	near(t, "hosting.com plan home", hc.PlanHome, 1417.71)
	if hc.Sites != 8 || !strings.Contains(hc.Text, "Already cheaper") {
		t.Fatalf("hosting.com break-even: %+v", hc)
	}

	// Chart: cost per site for 1 … 40 sites and a flat line per plan.
	if len(m.ChartX) != 40 || m.ChartX[0] != 1 || m.ChartX[39] != 40 || len(m.Chart) != 3 {
		t.Fatalf("chart: %d points, %d series", len(m.ChartX), len(m.Chart))
	}
	near(t, "1 site", m.Chart[0].Values[0], 10595)
	near(t, "19 sites", m.Chart[0].Values[18], 557.63)
	near(t, "plan line", m.Chart[1].Values[5], 559)
	if m.Chart[0].Values[18] > 559 || m.Chart[0].Values[17] <= 559 {
		t.Fatal("the curve must cross the Synergy line between 18 and 19 sites")
	}

	// A bigger need compares with a bigger plan.
	m = SelfHostFull(self, d, CompareInput{Tier: "business"}.Normalize(d.Tiers))
	if m.BreakEven[0].Plan != "Launch" || m.BreakEven[1].Plan != "Business Env" {
		t.Fatalf("business tier plans: %+v", m.BreakEven)
	}
	// 19.50 AUD = 1,677 KES → 7 sites.
	if m.BreakEven[1].Sites != 7 {
		t.Fatalf("business break-even: %d", m.BreakEven[1].Sites)
	}
}

func TestSelfHostEdgeCases(t *testing.T) {
	d := fixture()
	// Break-even beyond capacity.
	small := d.Suppliers[2]
	small.Capacity, small.CurrentSites = 10, 0
	m := SelfHostFull(small, d, CompareInput{Tier: "starter"}.Normalize(d.Tiers))
	if m.BreakEven[0].Reachable || !strings.Contains(m.BreakEven[0].Text, "more than the server holds (10)") {
		t.Fatalf("unreachable: %+v", m.BreakEven[0])
	}
	if m.PerSiteCurrent != nil {
		t.Fatal("no sites yet: no current cost per site")
	}
	if len(m.ChartX) != 19 {
		t.Fatalf("the chart runs to the furthest reachable break-even: %d", len(m.ChartX))
	}
	// Nothing entered yet.
	empty := Supplier{ID: "e", Name: "New server", Type: TypeSelfHosted, Currency: "KES", Costs: DefaultCostLines()}
	m = SelfHostFull(empty, d, CompareInput{}.Normalize(d.Tiers))
	if m.MonthlyTotal != 0 || len(m.Chart) != 0 || len(m.Warnings) < 2 || m.PerSiteCapacity != nil {
		t.Fatalf("empty model: %+v", m)
	}
	// A cost in a currency without a rate is left out and said.
	odd := Supplier{ID: "o", Name: "Odd", Type: TypeSelfHosted, Currency: "KES", Capacity: 5, Costs: []CostLine{{Label: "Server", Amount: 10, Currency: "EUR"}, {Label: "Power", Amount: 100}}}
	m = SelfHost(odd, d.Rates)
	near(t, "only the convertible line", m.MonthlyTotal, 100)
	if !m.Rows[0].NoRate || len(m.Warnings) != 1 {
		t.Fatalf("no-rate line: %+v %v", m.Rows[0], m.Warnings)
	}
	// Time lines are hours × rate; fixed lines ignore hours.
	near(t, "time", LineMonthly(CostLine{Kind: "time", Hours: 3, Rate: 20, Amount: 999}), 60)
	near(t, "fixed", LineMonthly(CostLine{Kind: "fixed", Hours: 3, Rate: 20, Amount: 45}), 45)
}
