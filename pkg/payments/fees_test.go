package payments

import (
	"testing"
)

func settingsWith(methods ...string) *Settings {
	s := DefaultSettings("org")
	if len(methods) > 0 {
		s.Methods = methods
	}
	return &s
}

func TestFeeAndCap(t *testing.T) {
	r := FeeRule{Percent: 1.4, Cap: 2000}
	if f := Fee(r, 10000, 0, "NGN"); f != 140 {
		t.Fatalf("fee %v", f)
	}
	if f := Fee(r, 1_000_000, 0, "NGN"); f != 2000 {
		t.Fatalf("cap not applied: %v", f)
	}
	if f := Fee(r, 1_000_000, 7.5, "NGN"); f != 2150 {
		t.Fatalf("VAT on the capped fee: %v", f)
	}
	if f := Fee(FeeRule{Percent: 2.9, Fixed: 1}, 100, 0, "ZAR"); f != 3.9 {
		t.Fatalf("fixed: %v", f)
	}
	// Zero-decimal currency rounds to whole units.
	if f := Fee(FeeRule{Percent: 3}, 12345, 0, "UGX"); f != 370 {
		t.Fatalf("UGX fee %v", f)
	}
}

func TestSurchargeGrossUp(t *testing.T) {
	r := FeeRule{Percent: 2.9}
	s := Surcharge(r, 1000, 0, "KES")
	// After the fee on (amount + surcharge) the business keeps at least 1000.
	if net := 1000 + s - Fee(r, 1000+s, 0, "KES"); net < 999.995 {
		t.Fatalf("surcharge %v leaves %v", s, net)
	}
	if s > 30 {
		t.Fatalf("surcharge too high: %v", s)
	}
	if Surcharge(r, 0, 0, "KES") != 0 {
		t.Fatal("zero amount")
	}
}

func TestLeastCostKES(t *testing.T) {
	opts := Options(settingsWith(), Context{Amount: 10000, Currency: "KES", OrgCountry: "KE", OrgCurrency: "KES"})
	if len(opts) != 2 {
		t.Fatalf("KES should offer M-Pesa and card, got %+v", opts)
	}
	if opts[0].Method != "mpesa" || !opts[0].Recommended || opts[0].Fee != 290 {
		t.Fatalf("M-Pesa should be cheapest & recommended: %+v", opts[0])
	}
	if opts[1].Method != "card" || opts[1].Fee != 320 || opts[1].Recommended || opts[1].Scope != ScopeLocal {
		t.Fatalf("card: %+v", opts[1])
	}
	// A customer abroad pays with an international card rate.
	opts = Options(settingsWith(), Context{Amount: 10000, Currency: "KES", CustomerCountry: "GB", OrgCountry: "KE", OrgCurrency: "KES"})
	if opts[1].Fee != 480 || opts[1].Scope != ScopeInternational {
		t.Fatalf("international card: %+v", opts[1])
	}
}

func TestLeastCostOtherCurrencies(t *testing.T) {
	// GHS: mobile money and bank transfer 2% tie → mobile money first (order), card 2.6% last.
	opts := Options(settingsWith(), Context{Amount: 500, Currency: "GHS", OrgCountry: "GH", OrgCurrency: "GHS"})
	if opts[0].Method != "mobilemoneyghana" || opts[len(opts)-1].Method != "card" {
		t.Fatalf("GHS order %+v", opts)
	}
	// USD: only cards, international pricing.
	opts = Options(settingsWith(), Context{Amount: 100, Currency: "USD", OrgCountry: "KE", OrgCurrency: "KES"})
	if len(opts) != 1 || opts[0].Method != "card" || opts[0].Fee != 4.8 {
		t.Fatalf("USD %+v", opts)
	}
	// Methods the business turned off are not offered.
	opts = Options(settingsWith("card"), Context{Amount: 10000, Currency: "KES", OrgCountry: "KE", OrgCurrency: "KES"})
	if len(opts) != 1 || opts[0].Method != "card" || !opts[0].Recommended {
		t.Fatalf("allowed methods: %+v", opts)
	}
	// An edited fee table changes the recommendation.
	s := settingsWith()
	s.Fees = []FeeRule{{Method: "mpesa", Currency: "KES", Percent: 5}, {Method: "card", Currency: "KES", Scope: ScopeLocal, Percent: 1}}
	opts = Options(s, Context{Amount: 10000, Currency: "KES", OrgCountry: "KE", OrgCurrency: "KES"})
	if opts[0].Method != "card" {
		t.Fatalf("custom fees: %+v", opts)
	}
}

func TestCustomerPaysFee(t *testing.T) {
	s := settingsWith()
	s.FeeBearer = BearerCustomer
	opts := Options(s, Context{Amount: 10000, Currency: "KES", OrgCountry: "KE", OrgCurrency: "KES"})
	o := opts[0]
	if o.Surcharge <= 0 || o.Total != 10000+o.Surcharge {
		t.Fatalf("surcharge %+v", o)
	}
	if o.Total-o.Fee < 10000-1e-9 || o.Total-o.Fee > 10000.02 {
		t.Fatalf("business should net the invoice amount: %+v", o)
	}
}

func TestCardScope(t *testing.T) {
	cases := []struct{ cur, cust, orgC, orgCur, want string }{
		{"KES", "", "KE", "KES", ScopeLocal},
		{"KES", "KE", "KE", "KES", ScopeLocal},
		{"KES", "US", "KE", "KES", ScopeInternational},
		{"NGN", "", "KE", "KES", ScopeInternational}, // not the business's currency
		{"USD", "", "US", "USD", ScopeInternational},
	}
	for _, c := range cases {
		if got := CardScope(c.cur, c.cust, c.orgC, c.orgCur); got != c.want {
			t.Errorf("%+v → %s", c, got)
		}
	}
}

func TestDefaultFeesSane(t *testing.T) {
	for _, r := range DefaultFees() {
		if _, ok := MethodByKey(r.Method); !ok || r.Percent <= 0 || r.Percent > 10 {
			t.Errorf("bad default %+v", r)
		}
	}
	if _, ok := FindRule(DefaultFees(), "card", "EUR", ScopeLocal); !ok {
		t.Error("EUR card should fall back to the international rule")
	}
}
