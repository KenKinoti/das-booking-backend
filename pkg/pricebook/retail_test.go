package pricebook

import (
	"errors"
	"strings"
	"testing"
)

func TestFriendly(t *testing.T) {
	for _, c := range []struct {
		v    float64
		cur  string
		want float64
	}{
		{4321, "KES", 4390}, {4990, "KES", 4990}, {4990.01, "KES", 5090}, {4900.5, "KES", 4990}, {13813.36, "KES", 13890}, {1000, "KES", 1090},
		{124300, "KES", 124390},
		{487.3, "KES", 490}, {490, "KES", 490}, {999.5, "KES", 1000}, {100, "KES", 100},
		{49.2, "AUD", 49.95}, {49.95, "AUD", 49.95}, {49.97, "AUD", 50.95}, {8.3, "AUD", 8.95}, {1, "AUD", 1.95},
		{0.42, "AUD", 0.45}, {0.5, "AUD", 0.5},
		{49.2, "UGX", 50}, {0.4, "UGX", 1}, {4321, "UGX", 4390},
		{0, "KES", 0}, {-5, "KES", 0},
	} {
		if got := Friendly(c.v, c.cur); got != c.want {
			t.Errorf("Friendly(%v, %s) = %v, want %v", c.v, c.cur, got, c.want)
		}
		if c.v > 0 && Friendly(c.v, c.cur) < c.v {
			t.Errorf("Friendly(%v) rounded down", c.v)
		}
	}
	if got := RoundUp(4321, RetailMarket{Currency: "KES", Rounding: "step", Step: 50}); got != 4350 {
		t.Fatalf("step 50: %v", got)
	}
	if got := RoundUp(4350, RetailMarket{Currency: "KES", Rounding: "step", Step: 50}); got != 4350 {
		t.Fatalf("already on the step: %v", got)
	}
	if got := RoundUp(4321.456, RetailMarket{Currency: "KES", Rounding: "none"}); got != 4321.46 {
		t.Fatalf("none: %v", got)
	}
}

func TestPriceForMargin(t *testing.T) {
	// Cost 8,514; margin 35%; fee 2.9% of what the customer pays; VAT 16%; prices exclude tax.
	//   N = 8514 ÷ (1 − 0.35 − 0.029 × 1.16) = 8514 ÷ 0.61636 = 13,813.36 → 13,890
	m := RetailMarket{Currency: "KES", TaxRate: 16, FeePercent: 2.9, Rounding: "friendly"}
	p, err := PriceFor(8514, "margin", 35, m)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "unrounded", p.Unrounded, 13813.36)
	near(t, "price", p.Price, 13890)
	near(t, "net", p.Net, 13890)
	near(t, "gross", p.Gross, 13890*1.16)
	near(t, "tax", p.Tax, 13890*0.16)
	near(t, "fee", p.Fee, 13890*1.16*0.029)
	near(t, "profit", p.Profit, 13890-467.26-8514)
	if p.MarginPct < 35 || p.MarginPct > 35.6 {
		t.Fatalf("rounding up never lowers the margin: %v", p.MarginPct)
	}
	near(t, "markup", p.MarkupPct, 57.7)

	// Without rounding the margin is exactly the target.
	m.Rounding = "none"
	p, _ = PriceFor(8514, "margin", 35, m)
	near(t, "exact margin", p.MarginPct, 35)
	near(t, "exact price", p.Price, 13813.36)

	// Prices quoted including tax: the gross is the friendly figure.
	m = RetailMarket{Currency: "KES", TaxRate: 16, FeePercent: 2.9, PricesIncludeTax: true, Rounding: "friendly"}
	p, _ = PriceFor(8514, "margin", 35, m)
	near(t, "gross unrounded", p.Unrounded, 16023.49)
	near(t, "gross", p.Gross, 16090)
	near(t, "price is the gross", p.Price, 16090)
	near(t, "net", p.Net, 16090/1.16)
	near(t, "tax", p.Tax, 16090-16090/1.16)
	near(t, "fee on the gross", p.Fee, 16090*0.029)
	if p.MarginPct < 35 {
		t.Fatalf("margin: %v", p.MarginPct)
	}

	// A fixed fee per payment is covered too.
	m = RetailMarket{Currency: "AUD", FeePercent: 1.75, FeeFixed: 0.30, Rounding: "none"}
	p, _ = PriceFor(100, "margin", 20, m)
	near(t, "with fixed fee", p.Net, (100+0.30)/(1-0.20-0.0175))
	near(t, "margin with fixed fee", p.MarginPct, 20)
}

func TestPriceForMarkupAndErrors(t *testing.T) {
	// Markup 50% on 100 with a 4.8% fee and no tax: N = 150 ÷ 0.952 = 157.56 → 158.95 (.95 ending under 1,000? no: 100–999 → next 10)
	p, err := PriceFor(100, "markup", 50, RetailMarket{Currency: "AUD", FeePercent: 4.8, Rounding: "friendly"})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "unrounded", p.Unrounded, 157.56)
	near(t, "friendly", p.Price, 160)
	near(t, "fee", p.Fee, 7.68)
	near(t, "profit", p.Profit, 160-7.68-100)
	near(t, "markup", p.MarkupPct, 52.3)
	p, _ = PriceFor(100, "markup", 50, RetailMarket{Currency: "AUD", FeePercent: 4.8, Rounding: "none"})
	near(t, "exact markup", p.MarkupPct, 50)

	if _, err := PriceFor(100, "margin", 99, RetailMarket{Currency: "KES", FeePercent: 2.9, TaxRate: 16}); !errors.Is(err, ErrNoMargin) {
		t.Fatalf("margin + fees ≥ 100%% must fail: %v", err)
	}
	if _, err := PriceFor(100, "margin", 100, RetailMarket{Currency: "KES"}); !errors.Is(err, ErrNoMargin) {
		t.Fatalf("100%% margin must fail: %v", err)
	}
	// Zero cost and zero margin are fine.
	p, err = PriceFor(0, "margin", 30, RetailMarket{Currency: "KES"})
	if err != nil || p.Price != 0 || p.Fee != 0 {
		t.Fatalf("zero cost: %+v %v", p, err)
	}
}

func TestRetailForOption(t *testing.T) {
	r, d := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"})
	c := r.Combo(".co.ke")
	syn := opt(t, r, ".co.ke", "s:syn")
	in := RetailInput{Mode: "margin", Percent: 35, Markets: []RetailMarket{
		{Currency: "KES", TaxRate: 16, FeePercent: 2.9},
		{Currency: "AUD", FeePercent: 4.8},
	}}.Normalize("KES")
	res := Retail(syn, c.Benchmarks, in, d.Rates)
	// Basis "higher": the renewal year (101.50 AUD) costs more than year 1 (99.00 AUD).
	if res.Basis != "higher" || res.BasisUsed != "ongoing" {
		t.Fatalf("basis: %s / %s", res.Basis, res.BasisUsed)
	}
	near(t, "cost home", res.CostHome, 101.5*86)
	if len(res.Markets) != 2 {
		t.Fatalf("markets: %d", len(res.Markets))
	}
	k := res.Markets[0]
	near(t, "KES cost", k.Cost, 8729)
	near(t, "nothing of it is in KES", k.FixedCost, 0)
	near(t, "all of it moves with AUD", k.MovingCost, 8729)
	// N = 8729 ÷ 0.61636 = 14,162.18 → 14,190
	near(t, "KES yearly", k.Yearly.Price, 14190)
	near(t, "KES profit", k.Yearly.Profit, 14190-RoundCur(14190*1.16*0.029, "KES")-8729)
	near(t, "profit in home", k.Yearly.ProfitHome, k.Yearly.Profit)
	// Monthly: 8729 ÷ 12 = 727.42 → N = 1,180.18 → 1,190
	near(t, "KES monthly cost", k.Monthly.Cost, 727.42)
	near(t, "KES monthly", k.Monthly.Price, 1190)
	// Sensitivity: AUD 10% dearer → cost 9,601.90; 10% cheaper → 7,856.10; the price stays.
	if len(k.Sensitivity) != 2 || k.Sensitivity[0].ShockPct != 10 || k.Sensitivity[1].ShockPct != -10 {
		t.Fatalf("sensitivity: %+v", k.Sensitivity)
	}
	near(t, "+10% cost", k.Sensitivity[0].Cost, 9601.9)
	near(t, "+10% profit", k.Sensitivity[0].Profit, k.Yearly.Profit-872.9)
	near(t, "−10% cost", k.Sensitivity[1].Cost, 7856.1)
	if !(k.Sensitivity[0].MarginPct < k.Yearly.MarginPct && k.Yearly.MarginPct < k.Sensitivity[1].MarginPct) {
		t.Fatalf("margins must move with FX: %+v vs %v", k.Sensitivity, k.Yearly.MarginPct)
	}
	// Against the two competitors (first year 3,499 and 4,700; renewal 4,700 and 4,700).
	if len(k.Positions) != 2 || k.Positions[0].Basis != "first_year" || k.Positions[0].Where != "most_expensive" || k.Positions[0].Rank != 3 || k.Positions[0].Of != 3 {
		t.Fatalf("position: %+v", k.Positions)
	}
	near(t, "median", k.Positions[0].Stats.Median, 4099.5)

	// AUD market: the cost is already in AUD, so FX does not move the margin.
	a := res.Markets[1]
	near(t, "AUD cost exact", a.Cost, 101.5)
	near(t, "AUD fixed", a.FixedCost, 101.5)
	near(t, "AUD moving", a.MovingCost, 0)
	// N = 101.5 ÷ (1 − 0.35 − 0.048) = 168.60 → 170
	near(t, "AUD yearly", a.Yearly.Price, 170)
	near(t, "AUD profit", a.Yearly.Profit, 170-8.16-101.5)
	near(t, "AUD profit in KES", a.Yearly.ProfitHome, (170-8.16-101.5)*86)
	near(t, "AUD sensitivity flat", a.Sensitivity[0].MarginPct, a.Yearly.MarginPct)
	// Monthly: 8.46 → 14.05 → 14.95
	near(t, "AUD monthly", a.Monthly.Price, 14.95)
	if !strings.Contains(res.CostNote, "Synergy Wholesale") || !strings.Contains(res.CostNote, "KES 8,729.00") {
		t.Fatalf("cost note: %s", res.CostNote)
	}
}

func TestRetailBasisAndMixedCurrencies(t *testing.T) {
	r, d := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"})
	c := r.Combo(".co.ke")
	// The mix is cheap in year 1 (intro hosting) and dear afterwards.
	res := Retail(c.Mix, c.Benchmarks, RetailInput{Basis: "first_year", Percent: 30, Markets: []RetailMarket{{Currency: "KES", Rounding: "none"}}}.Normalize("KES"), d.Rates)
	near(t, "first-year basis", res.CostHome, 21*86+35.88*129)
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "margin drops from year 2") {
		t.Fatalf("pricing on a promo year must warn: %v", res.Warnings)
	}
	res = Retail(c.Mix, c.Benchmarks, RetailInput{Percent: 30, Markets: []RetailMarket{{Currency: "KES", Rounding: "none"}, {Currency: "USD", Rounding: "none"}}}.Normalize("KES"), d.Rates)
	if res.BasisUsed != "ongoing" {
		t.Fatalf("higher basis: %s", res.BasisUsed)
	}
	near(t, "ongoing basis", res.CostHome, 23.5*86+131.88*129)
	// USD market: the hosting (131.88 USD) is fixed, the domain (23.50 AUD = 15.67 USD) moves.
	u := res.Markets[1]
	near(t, "USD fixed", u.FixedCost, 131.88)
	near(t, "USD moving", u.MovingCost, 23.5*86/129)
	near(t, "USD cost", u.Cost, 131.88+23.5*86/129)
	near(t, "+10% moves only the AUD part", u.Sensitivity[0].Cost, 131.88+23.5*86/129*1.1)

	// Self-hosted: the server's USD share stays put in a USD market.
	r2, d2 := run(t, CompareInput{Tier: "starter", SelfHostedBasis: "capacity"})
	self := opt(t, r2, "", "s:self")
	res = Retail(self, nil, RetailInput{Percent: 30, Markets: []RetailMarket{{Currency: "USD", Rounding: "none"}, {Currency: "KES", Rounding: "none"}}}.Normalize("KES"), d2.Rates)
	u = res.Markets[0]
	yearly := 264.88 * 12
	near(t, "USD share fixed", u.FixedCost, yearly/129*7095/10595)
	near(t, "KES share moves", u.MovingCost, yearly/129*3500/10595)
	k := res.Markets[1]
	near(t, "KES share fixed at home", k.FixedCost, yearly*3500/10595)
	near(t, "USD share moves at home", k.MovingCost, yearly*7095/10595)
	if len(k.Positions) != 0 {
		t.Fatal("no competitors: no position")
	}
}

func TestPlace(t *testing.T) {
	others := []float64{3499, 4700, 6000}
	for _, c := range []struct {
		price float64
		where string
		rank  int
	}{{3000, "cheapest", 1}, {3499, "below_median", 1}, {4000, "below_median", 2}, {4700, "below_median", 2}, {5000, "above_median", 3}, {6000, "above_median", 3}, {7000, "most_expensive", 4}} {
		p := Place(c.price, others, "first_year", "KES")
		if p.Where != c.where || p.Rank != c.rank || p.Of != 4 {
			t.Errorf("Place(%v) = %s rank %d of %d, want %s rank %d", c.price, p.Where, p.Rank, p.Of, c.where, c.rank)
		}
	}
	p := Place(5170, others, "ongoing", "KES")
	near(t, "vs median", p.VsMedianPct, 10)
	if !strings.Contains(p.Text, "10% above the median") || !strings.Contains(p.Text, "renewal") {
		t.Fatalf("text: %s", p.Text)
	}
	if e := Place(100, nil, "ongoing", "KES"); e.Where != "" || e.Stats.Count != 0 {
		t.Fatalf("no competitors: %+v", e)
	}
}

func TestRetailNormalize(t *testing.T) {
	in := RetailInput{Mode: "x", Basis: "y", Percent: -5, Shock: 0, Markets: []RetailMarket{{Currency: "kes", TaxRate: -1, Rounding: "odd"}, {Currency: "KES"}, {Currency: "ZZZ"}}}.Normalize("AUD")
	if in.Mode != "margin" || in.Basis != "higher" || in.Percent != 0 || in.Shock != 10 || len(in.Markets) != 1 || in.Markets[0].Currency != "KES" || in.Markets[0].TaxRate != 0 || in.Markets[0].Rounding != "friendly" {
		t.Fatalf("normalised: %+v", in)
	}
	if got := (RetailInput{}).Normalize("AUD"); len(got.Markets) != 1 || got.Markets[0].Currency != "AUD" {
		t.Fatalf("default market is home: %+v", got.Markets)
	}
}
