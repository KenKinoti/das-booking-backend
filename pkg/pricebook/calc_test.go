package pricebook

import (
	"math"
	"strings"
	"testing"
	"time"
)

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.0051 {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

var testToday = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

func day(s string) *time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &d
}

// fixture: Synergy (AUD), hosting.com (USD), a self-hosted server (KES) and
// two competitors; home KES at AUD 86 and USD 129.
func fixture() Data {
	sups := []Supplier{
		{ID: "syn", Name: "Synergy Wholesale", Type: TypeWholesale, Currency: "AUD"},
		{ID: "hc", Name: "hosting.com", Type: TypeWholesale, Currency: "USD"},
		{ID: "self", Name: "Self-hosted server", Type: TypeSelfHosted, Currency: "KES", Capacity: 40, CurrentSites: 12, Costs: []CostLine{
			{Key: "server", Label: "Server / VPS", Kind: "fixed", Amount: 40, Currency: "USD"},
			{Key: "panel", Label: "Control-panel licence", Kind: "fixed", Amount: 15, Currency: "USD"},
			{Key: "backups", Label: "Backups", Kind: "fixed", Amount: 500},
			{Key: "time", Label: "Your time", Kind: "time", Hours: 2, Rate: 1500},
		}},
		{ID: "ca", Name: "Example Host Kenya", Type: TypeCompetitor, Currency: "KES"},
		{ID: "cb", Name: "Sample Web Africa", Type: TypeCompetitor, Currency: "KES"},
	}
	asOf := day("2026-09-28")
	n := 0
	it := func(sup, kind, name, tld, period, cur string, price float64, reg *float64, disk *float64, sites, email *int) Item {
		n++
		x := Item{ID: "i" + itoa(n), SupplierID: sup, Kind: kind, Name: name, NameKey: NameKey(name), TLD: tld, Period: period, Price: price, Currency: cur, RegularPrice: reg,
			DiskGB: disk, Sites: sites, EmailAccounts: email, AsOf: asOf, Active: true}
		if IsDomainKind(kind) && name == "" {
			x.Name = DomainItemName(kind, tld)
		}
		return x
	}
	items := []Item{
		it("syn", KindHosting, "Starter Env", "", PeriodMonth, "AUD", 6.50, nil, fp(2), ip(1), ip(5)),
		it("syn", KindHosting, "Standard Env", "", PeriodMonth, "AUD", 11.00, nil, fp(5), ip(1), ip(10)),
		it("syn", KindHosting, "Business Env", "", PeriodMonth, "AUD", 19.50, nil, fp(10), ip(1), ip(Unlimited)),
		it("syn", KindRegister, "", ".co.ke", PeriodYear, "AUD", 21.00, nil, nil, nil, nil),
		it("syn", KindRenew, "", ".co.ke", PeriodYear, "AUD", 23.50, nil, nil, nil, nil),
		it("syn", KindTransfer, "", ".co.ke", PeriodYear, "AUD", 19.00, nil, nil, nil, nil),
		it("syn", KindRegister, "", ".com", PeriodYear, "AUD", 16.20, nil, nil, nil, nil),
		it("syn", KindRenew, "", ".com", PeriodYear, "AUD", 17.80, nil, nil, nil, nil),
		it("syn", KindSSL, "Standard SSL", "", PeriodYear, "AUD", 12.00, nil, nil, nil, nil),
		it("syn", KindEmail, "Mailbox 5 GB", "", PeriodMonth, "AUD", 1.20, nil, nil, nil, nil),
		it("hc", KindHosting, "Launch", "", PeriodYear, "USD", 35.88, fp(131.88), fp(15), ip(1), ip(10)),
		it("hc", KindHosting, "Reseller 50", "", PeriodMonth, "USD", 24.99, nil, fp(60), ip(50), ip(Unlimited)),
		it("hc", KindRegister, "", ".com", PeriodYear, "USD", 11.99, fp(19.99), nil, nil, nil),
		it("hc", KindRenew, "", ".com", PeriodYear, "USD", 19.99, nil, nil, nil, nil),
		it("ca", KindHosting, "Basic", "", PeriodYear, "KES", 2500, fp(3500), fp(5), ip(1), ip(10)),
		it("ca", KindRegister, "", ".co.ke", PeriodYear, "KES", 999, fp(1200), nil, nil, nil),
		it("cb", KindHosting, "Bronze", "", PeriodYear, "KES", 3600, nil, fp(10), ip(1), ip(20)),
		it("cb", KindRegister, "", ".co.ke", PeriodYear, "KES", 1100, nil, nil, nil, nil),
	}
	r := Rates{Home: "KES", Per: map[string]float64{"AUD": 86, "USD": 129}, Info: map[string]RateInfo{
		"AUD": {Currency: "AUD", Rate: 86, DefaultRate: 86, Source: "manual"},
		"USD": {Currency: "USD", Rate: 129, DefaultRate: 129, Source: "manual"},
	}}
	return Data{Home: "KES", Suppliers: sups, Items: items, Rates: r, Tiers: DefaultTiers(), Today: testToday}
}

func run(t *testing.T, in CompareInput) (*CompareResult, Data) {
	t.Helper()
	d := fixture()
	return Compare(in.Normalize(d.Tiers), d), d
}

func opt(t *testing.T, r *CompareResult, tld, key string) *Option {
	t.Helper()
	o := r.FindOption(tld, key)
	if o == nil {
		t.Fatalf("option %s not found for %q", key, tld)
	}
	return o
}

func TestItemCost(t *testing.T) {
	for _, c := range []struct {
		name                  string
		price, regular        float64
		months, term          int
		first, ongoing, total float64
	}{
		{"monthly, 1 year", 6.5, 6.5, 1, 1, 78, 78, 78},
		{"monthly, 2 years", 6.5, 6.5, 1, 2, 78, 78, 156},
		{"monthly intro price: first month only", 1, 10, 1, 1, 111, 120, 111},
		{"yearly intro price", 35.88, 131.88, 12, 1, 35.88, 131.88, 35.88},
		{"yearly intro price over 2 years", 35.88, 131.88, 12, 2, 35.88, 131.88, 167.76},
		{"paid every 2 years", 100, 100, 24, 2, 100, 50, 100},
		{"2-year intro then regular", 80, 120, 24, 2, 80, 60, 80},
		{"one-off", 49, 49, 0, 1, 49, 0, 49},
		{"no regular price = the price", 12, 0, 12, 2, 12, 12, 24},
	} {
		got := ItemCost(c.price, c.regular, c.months, c.term)
		near(t, c.name+" first year", got.FirstYear, c.first)
		near(t, c.name+" ongoing", got.Ongoing, c.ongoing)
		near(t, c.name+" term", got.Term, c.total)
	}
}

func TestRoundingPerCurrency(t *testing.T) {
	near(t, "KES 2 decimals", RoundCur(1234.565, "KES"), 1234.57)
	near(t, "half cent rounds up despite float noise", RoundCur(2.675, "AUD"), 2.68)
	if got := RoundCur(1234.5, "UGX"); got != 1235 {
		t.Fatalf("UGX has no minor units: %v", got)
	}
	if got := RoundCur(1.23456, "KWD"); got != 1.235 {
		t.Fatalf("KWD has 3 decimals: %v", got)
	}
	r := Rates{Home: "UGX", Per: map[string]float64{"AUD": 2450.337}}
	if got := r.ToHome(99, "AUD"); got != 242583 {
		t.Fatalf("99 AUD in UGX: %v", got)
	}
	if got := r.ToHome(500, "UGX"); got != 500 {
		t.Fatalf("home stays home: %v", got)
	}
	k := Rates{Home: "KES", Per: map[string]float64{"AUD": 86, "USD": 129}}
	near(t, "USD → AUD through home", k.Convert(10, "USD", "AUD"), 15)
	near(t, "same currency is exact", k.Convert(12.34, "AUD", "AUD"), 12.34)
	if k.Convert(10, "EUR", "KES") != 0 || k.Rate("EUR") != 0 {
		t.Fatal("a missing rate converts to 0")
	}
}

func TestCompareSingleSupplier(t *testing.T) {
	r, _ := run(t, CompareInput{TLDs: []string{"CO.KE"}, Tier: "starter", TermYears: 1})
	if len(r.Combos) != 1 || r.Combos[0].TLD != ".co.ke" {
		t.Fatalf("combos: %+v", r.Combos)
	}
	if r.Need != "Starter hosting (2 GB or more)" {
		t.Fatalf("need: %q", r.Need)
	}
	s := opt(t, r, ".co.ke", "s:syn")
	if !s.Complete || s.Currency != "AUD" {
		t.Fatalf("synergy option: %+v", s)
	}
	// Domain 21.00 + hosting 6.50 × 12 = 99.00 AUD in year 1; renews at 23.50 + 78.00.
	near(t, "domain year 1", s.Domain.FirstYear, 21)
	near(t, "domain renewal", s.Domain.Ongoing, 23.5)
	if s.Hosting.Name != "Starter Env" || s.Hosting.Months != 1 {
		t.Fatalf("hosting: %+v", s.Hosting)
	}
	near(t, "hosting monthly", s.Hosting.Monthly, 6.5)
	near(t, "hosting × 12", s.Hosting.FirstYear, 78)
	near(t, "first year AUD", s.FirstYear, 99)
	near(t, "ongoing AUD", s.Ongoing, 101.5)
	near(t, "first year KES", s.FirstYearH, 21*86+78*86)
	near(t, "ongoing KES", s.OngoingH, 23.5*86+78*86)
	near(t, "term = first year for 1 year", s.TermH, s.FirstYearH)
	if s.PromoTrap {
		t.Fatal("a 2.5% dearer renewal is not a promo trap")
	}
	// hosting.com does not sell .co.ke: incomplete, never the cheapest.
	h := opt(t, r, ".co.ke", "s:hc")
	if h.Complete || len(h.Missing) != 1 || h.Missing[0] != "domain" || h.Cheapest {
		t.Fatalf("hosting.com: %+v", h)
	}
	if h.Hosting == nil || h.Hosting.Name != "Launch" {
		t.Fatalf("hosting.com hosting: %+v", h.Hosting)
	}
	// Rates used are reported.
	if len(r.Rates) != 2 || r.Rates[0].Currency != "AUD" || r.Rates[0].Rate != 86 || r.Rates[1].Currency != "USD" {
		t.Fatalf("rates: %+v", r.Rates)
	}
}

func TestCompareCheapestMixAndPromoTrap(t *testing.T) {
	r, _ := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", TermYears: 1})
	c := r.Combo(".co.ke")
	m := c.Mix
	if m == nil || !m.Complete || m.SameSupplier {
		t.Fatalf("mix: %+v", m)
	}
	// Domain from Synergy (21.00 AUD) + hosting from hosting.com (35.88 USD intro).
	if m.Domain.SupplierID != "syn" || m.Hosting.SupplierID != "hc" || m.Hosting.Name != "Launch" {
		t.Fatalf("mix parts: %s + %s (%s)", m.Domain.SupplierName, m.Hosting.SupplierName, m.Hosting.Name)
	}
	near(t, "mix first year", m.FirstYearH, 21*86+35.88*129)
	near(t, "mix ongoing", m.OngoingH, 23.5*86+131.88*129)
	if m.Currency != "" || len(m.ByCurrency) != 2 {
		t.Fatalf("a mix in two currencies has no single-currency total: %q %+v", m.Currency, m.ByCurrency)
	}
	if !m.PromoTrap || !m.Cheapest || c.Best != "mix" {
		t.Fatalf("mix flags: trap=%v cheapest=%v best=%s", m.PromoTrap, m.Cheapest, c.Best)
	}
	found := false
	for _, f := range m.Flags {
		if f.Type == "promo" && strings.Contains(f.Message, "renews at") && strings.Contains(f.Message, "Launch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("promo flag missing: %+v", m.Flags)
	}
	near(t, "saving vs best single supplier", m.Saving, (21*86+78*86)-(21*86+35.88*129))
	// … but Synergy is the cheapest to renew, and says so.
	if c.BestOngoing != "s:syn" || !opt(t, r, ".co.ke", "s:syn").LowestOngoing || m.LowestOngoing {
		t.Fatalf("lowest ongoing: %s", c.BestOngoing)
	}
	if opt(t, r, ".co.ke", "s:syn").Cheapest {
		t.Fatal("only one option is the cheapest")
	}

	// Over two years the intro price is gone: everything from Synergy wins.
	r2, _ := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", TermYears: 2})
	c2 := r2.Combo(".co.ke")
	if c2.Best != "s:syn" || !c2.Mix.SameSupplier || c2.Mix.Cheapest {
		t.Fatalf("2 years: best=%s mix=%+v", c2.Best, c2.Mix)
	}
	s := opt(t, r2, ".co.ke", "s:syn")
	near(t, "2-year term AUD", s.Term, 21+23.5+156)
	near(t, "2-year term KES", s.TermH, (21+23.5)*86+156*86)
	near(t, "first year unchanged", s.FirstYear, 99)
}

func TestComparePromoDomainAndRenewalItem(t *testing.T) {
	r, _ := run(t, CompareInput{TLDs: []string{".com"}, Tier: "starter", TermYears: 2})
	h := opt(t, r, ".com", "s:hc")
	if !h.Complete {
		t.Fatalf("hosting.com sells .com: %+v", h.Missing)
	}
	near(t, ".com year 1", h.Domain.FirstYear, 11.99)
	near(t, ".com renewal", h.Domain.Ongoing, 19.99)
	near(t, ".com over 2 years", h.Domain.Term, 11.99+19.99)
	if !h.Domain.Promo || !h.PromoTrap {
		t.Fatalf("intro domain + intro hosting must be flagged: %+v", h.Flags)
	}
	near(t, "USD first year", h.FirstYear, 11.99+35.88)
	near(t, "USD ongoing", h.Ongoing, 19.99+131.88)
	near(t, "USD term", h.Term, 11.99+19.99+35.88+131.88)
	// No renewal item listed: assumed the same as registration, and said so.
	d := fixture()
	items := []Item{}
	for _, it := range d.Items {
		if !(it.SupplierID == "syn" && it.Kind == KindRenew) {
			items = append(items, it)
		}
	}
	d.Items = items
	r3 := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"}.Normalize(d.Tiers), d)
	s := opt(t, r3, ".co.ke", "s:syn")
	if !s.Domain.Assumed {
		t.Fatal("renewal must be marked as assumed")
	}
	near(t, "assumed renewal", s.Domain.Ongoing, 21)
	ok := false
	for _, f := range s.Flags {
		ok = ok || f.Type == "assumed_renewal"
	}
	if !ok {
		t.Fatalf("assumed_renewal flag missing: %+v", s.Flags)
	}
}

func TestCompareTransfer(t *testing.T) {
	r, _ := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", DomainAction: "transfer"})
	s := opt(t, r, ".co.ke", "s:syn")
	near(t, "transfer price", s.Domain.FirstYear, 19)
	near(t, "then the renewal price", s.Domain.Ongoing, 23.5)
}

func TestCompareNeedPicksPlan(t *testing.T) {
	r, _ := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "standard"})
	if got := opt(t, r, ".co.ke", "s:syn").Hosting.Name; got != "Standard Env" {
		t.Fatalf("standard tier (5 GB): %s", got)
	}
	r, _ = run(t, CompareInput{TLDs: []string{".co.ke"}, MinDiskGB: 8})
	if got := opt(t, r, ".co.ke", "s:syn").Hosting.Name; got != "Business Env" {
		t.Fatalf("8 GB: %s", got)
	}
	// Nothing at Synergy has 20 GB: hosting is missing, with the reason.
	r, _ = run(t, CompareInput{TLDs: []string{".co.ke"}, MinDiskGB: 20})
	s := opt(t, r, ".co.ke", "s:syn")
	if s.Complete || s.Hosting != nil || len(s.Reasons) == 0 || !strings.Contains(s.Reasons[0], "10 GB disk, 20 GB needed") {
		t.Fatalf("20 GB: %+v", s)
	}
	// 3 sites: only the 50-site reseller plan qualifies.
	r, _ = run(t, CompareInput{MinSites: 3})
	h := opt(t, r, "", "s:hc")
	if h.Hosting == nil || h.Hosting.Name != "Reseller 50" || !h.Complete {
		t.Fatalf("3 sites: %+v", h.Hosting)
	}
}

func TestCompareSharedPlan(t *testing.T) {
	// One 24.99 USD reseller plan spread over 20 client sites.
	d := fixture()
	items := []Item{}
	for _, it := range d.Items {
		if it.Name != "Launch" {
			items = append(items, it)
		}
	}
	d.Items = items
	r := Compare(CompareInput{Tier: "starter", ShareSites: 20}.Normalize(d.Tiers), d)
	h := opt(t, r, "", "s:hc")
	if h.Hosting.SharedBy != 20 {
		t.Fatalf("shared by %d", h.Hosting.SharedBy)
	}
	near(t, "per site per year", h.Hosting.FirstYear, 24.99*12/20)
	near(t, "per site per year KES", h.FirstYearH, RoundCur(RoundCur(24.99*12/20, "USD")*129, "KES"))
	ok := false
	for _, f := range h.Flags {
		ok = ok || f.Type == "shared"
	}
	if !ok {
		t.Fatal("shared flag missing")
	}
	// 60 GB over 2 GB sites: at most 30 share it, whatever is asked; the plan allows 50.
	r = Compare(CompareInput{Tier: "starter", ShareSites: 80}.Normalize(d.Tiers), d)
	if got := opt(t, r, "", "s:hc").Hosting.SharedBy; got != 30 {
		t.Fatalf("disk caps sharing at 30, got %d", got)
	}
	r = Compare(CompareInput{MinDiskGB: 1, ShareSites: 80}.Normalize(d.Tiers), d)
	if got := opt(t, r, "", "s:hc").Hosting.SharedBy; got != 50 {
		t.Fatalf("the plan's 50 sites cap sharing, got %d", got)
	}
	// Single-site plans are never shared.
	if got := opt(t, r, "", "s:syn").Hosting.SharedBy; got != 1 {
		t.Fatalf("single-site plan shared by %d", got)
	}
}

func TestComparePinnedMix(t *testing.T) {
	// Year 1 the cheapest hosting is hosting.com's intro price; pinning the
	// hosting to the server (and the domain to Synergy) prices that mix instead.
	r, _ := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", HostingFrom: "self", DomainFrom: "syn"})
	c := r.Combo(".co.ke")
	m := c.Mix
	if m == nil || !m.Pinned || m.Hosting.SupplierID != "self" || m.Domain.SupplierID != "syn" || m.SameSupplier {
		t.Fatalf("pinned mix: %+v", m)
	}
	near(t, "pinned mix first year", m.FirstYearH, 21*86+882.92*12)
	if m.Cheapest || c.Best != "s:syn" {
		t.Fatalf("a pinned mix is only the cheapest when it is: best=%s", c.Best)
	}
	// Pinning both halves to one supplier is that supplier's own option.
	r, _ = run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", HostingFrom: "syn", DomainFrom: "syn"})
	if m := r.Combo(".co.ke").Mix; m == nil || !m.SameSupplier || m.SupplierID != "syn" {
		t.Fatalf("same-supplier pin: %+v", m)
	}
	// A supplier without that half: no mix, and the reason.
	r, _ = run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", DomainFrom: "hc"})
	if c := r.Combo(".co.ke"); c.Mix != nil || !strings.Contains(c.MixNote, "hosting.com has no .co.ke price") {
		t.Fatalf("missing half: mix=%v note=%q", c.Mix, c.MixNote)
	}
	// Unknown ids are ignored.
	r, _ = run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", DomainFrom: "nope"})
	if m := r.Combo(".co.ke").Mix; m == nil || m.Pinned || m.Hosting.SupplierID != "hc" {
		t.Fatalf("unknown pin: %+v", m)
	}
}

func TestCompareSelfHostedInMix(t *testing.T) {
	// Server: (40 + 15) USD × 129 + 500 + 2 × 1500 = 10,595 KES a month.
	r, _ := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", TermYears: 2, SelfHostedBasis: "capacity"})
	s := opt(t, r, ".co.ke", "s:self")
	if s.Complete || s.Hosting == nil {
		t.Fatalf("self-hosted has hosting but no domain: %+v", s)
	}
	near(t, "per site per month at capacity (40)", s.Hosting.Monthly, 264.88)
	near(t, "per site per year", s.Hosting.FirstYear, 264.88*12)
	// At capacity the server beats every plan over 2 years: mix = Synergy domain + own server.
	m := r.Combo(".co.ke").Mix
	if m.Hosting.SupplierID != "self" || m.Domain.SupplierID != "syn" || !m.Cheapest {
		t.Fatalf("mix: %s + %s cheapest=%v", m.Domain.SupplierName, m.Hosting.SupplierName, m.Cheapest)
	}
	near(t, "mix term", m.TermH, (21+23.5)*86+264.88*24)
	// At the current 12 sites it costs 882.92 a month per site.
	r, _ = run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", TermYears: 2})
	near(t, "per site at 12 sites", opt(t, r, ".co.ke", "s:self").Hosting.Monthly, 882.92)
	if r.Combo(".co.ke").Mix.Hosting.SupplierID == "self" {
		t.Fatal("at 12 sites the server is not the cheapest")
	}
	// The split says what share of the cost is really in USD.
	sp := opt(t, r, ".co.ke", "s:self").Hosting.Split
	if len(sp) != 2 || sp[0].Currency != "KES" || sp[1].Currency != "USD" {
		t.Fatalf("split: %+v", sp)
	}
	near(t, "USD share", sp[1].Share, 7095.0/10595.0)
	// No costs entered: the server is not offered (it would always win at zero).
	d := fixture()
	d.Suppliers[2].Costs = DefaultCostLines()
	r0 := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"}.Normalize(d.Tiers), d)
	if o := r0.FindOption(".co.ke", "s:self"); o.Hosting != nil || len(o.Reasons) == 0 {
		t.Fatalf("empty server must not be priced: %+v", o)
	}
}

func TestCompareAddons(t *testing.T) {
	// With only the Starter plan (5 mailboxes included), 8 mailboxes are bought: 8 × 1.20 a month.
	d := fixture()
	items := []Item{}
	for _, it := range d.Items {
		if it.Name != "Standard Env" && it.Name != "Business Env" {
			items = append(items, it)
		}
	}
	d.Items = items
	r := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", IncludeSSL: true, Mailboxes: 8}.Normalize(d.Tiers), d)
	s := opt(t, r, ".co.ke", "s:syn")
	if s.Email == nil || s.Email.Included {
		t.Fatalf("email must be priced: %+v", s.Email)
	}
	near(t, "8 mailboxes a year", s.Email.FirstYear, 8*1.2*12)
	near(t, "ssl", s.SSL.FirstYear, 12)
	near(t, "first year", s.FirstYear, 21+78+12+115.2)
	near(t, "first year KES", s.FirstYearH, 21*86+78*86+12*86+115.2*86)

	// With every plan listed, a bigger plan that includes the mailboxes is
	// cheaper than the small plan plus 8 mailboxes: 132 < 78 + 115.20.
	r, _ = run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", Mailboxes: 8})
	s = opt(t, r, ".co.ke", "s:syn")
	if s.Hosting.Name != "Standard Env" || s.Email == nil || !s.Email.Included {
		t.Fatalf("cheapest bundle: %s, email %+v", s.Hosting.Name, s.Email)
	}
	near(t, "bundle first year", s.FirstYear, 21+132)
	// 4 mailboxes fit in the Starter plan.
	r, _ = run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", Mailboxes: 4})
	s = opt(t, r, ".co.ke", "s:syn")
	if s.Hosting.Name != "Starter Env" || s.Email == nil || !s.Email.Included || s.Email.FirstYear != 0 {
		t.Fatalf("4 mailboxes are included: %+v", s.Email)
	}
	near(t, "first year without extras", s.FirstYear, 99)
	// hosting.com lists no SSL: assumed included and flagged, not an error.
	r, _ = run(t, CompareInput{Tier: "starter", IncludeSSL: true})
	h := opt(t, r, "", "s:hc")
	if h.SSL == nil || !h.SSL.Included || !h.SSL.Assumed || !h.Complete {
		t.Fatalf("assumed SSL: %+v", h.SSL)
	}
	// A plan without enough mailboxes at a supplier with no mailbox price: said, not hidden.
	r, _ = run(t, CompareInput{Tier: "starter", Mailboxes: 25})
	h = opt(t, r, "", "s:hc")
	if h.Hosting.Name != "Reseller 50" {
		t.Fatalf("25 mailboxes: the plan with unlimited mailboxes is the only full answer, got %s", h.Hosting.Name)
	}
}

func TestCompareCompetitorsAreBenchmarks(t *testing.T) {
	r, _ := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"})
	c := r.Combo(".co.ke")
	for _, o := range c.Options {
		if o.SupplierType == TypeCompetitor {
			t.Fatalf("a competitor is not an option to buy: %s", o.SupplierName)
		}
	}
	if len(c.Benchmarks) != 2 || c.Market == nil {
		t.Fatalf("benchmarks: %d", len(c.Benchmarks))
	}
	// A: 999 + 2500 = 3499 then 1200 + 3500 = 4700; B: 1100 + 3600 = 4700 both years.
	if c.Market.FirstYear.Count != 2 {
		t.Fatalf("count: %+v", c.Market)
	}
	near(t, "cheapest", c.Market.FirstYear.Min, 3499)
	near(t, "median", c.Market.FirstYear.Median, (3499+4700)/2.0)
	near(t, "most expensive", c.Market.FirstYear.Max, 4700)
	near(t, "renewal min", c.Market.Ongoing.Min, 4700)
	// The mix never uses a competitor even when it is cheaper.
	if c.Mix.Domain.SupplierID == "ca" || c.Mix.Hosting.SupplierID == "ca" {
		t.Fatal("the mix must not buy from a competitor")
	}
}

func TestCompetitorsIgnoreSharing(t *testing.T) {
	// A competitor's "unlimited sites" plan is still what ONE customer pays:
	// spreading reseller plans over 11 of your sites must not move the market.
	d := fixture()
	d.Items = append(d.Items, Item{ID: "cu", SupplierID: "ca", Kind: KindHosting, Name: "Unlimited", NameKey: "unlimited", Period: PeriodYear, Price: 2000, Currency: "KES",
		DiskGB: fp(Unlimited), Sites: ip(Unlimited), AsOf: day("2026-09-28"), Active: true})
	one := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"}.Normalize(d.Tiers), d).Combo(".co.ke")
	many := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", ShareSites: 11}.Normalize(d.Tiers), d).Combo(".co.ke")
	if *one.Market != *many.Market {
		t.Fatalf("market moved with share_sites: %+v vs %+v", *one.Market, *many.Market)
	}
	// A: 999 + 2,000 (the unlimited plan, whole) then 1,200 + 2,000.
	near(t, "cheapest competitor", many.Market.FirstYear.Min, 2999)
	near(t, "its renewal", many.Market.Ongoing.Min, 3200)
	for _, b := range many.Benchmarks {
		if b.Hosting.SharedBy != 1 {
			t.Fatalf("%s: competitor plan shared by %d", b.SupplierName, b.Hosting.SharedBy)
		}
		for _, f := range b.Flags {
			if f.Type == "shared" {
				t.Fatalf("%s: a competitor has no shared flag", b.SupplierName)
			}
		}
	}
	// Your own reseller plan IS spread.
	found := false
	for _, o := range many.Options {
		if o.SupplierID == "hc" && o.Hosting != nil && o.Hosting.Name == "Reseller 50" && o.Hosting.SharedBy == 11 {
			found = true
		}
	}
	if !found {
		t.Fatal("the wholesale reseller plan should be spread over 11 sites")
	}
}

func TestCompareRenewalOnlyDomain(t *testing.T) {
	// Statements only teach the renewal price: the supplier is still a complete option.
	d := fixture()
	items := []Item{}
	for _, it := range d.Items {
		if !(it.SupplierID == "syn" && it.TLD == ".co.ke" && (it.Kind == KindRegister || it.Kind == KindTransfer)) {
			items = append(items, it)
		}
	}
	d.Items = items
	for _, action := range []string{"register", "transfer"} {
		r := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", TermYears: 2, DomainAction: action}.Normalize(d.Tiers), d)
		s := opt(t, r, ".co.ke", "s:syn")
		if !s.Complete || s.Domain == nil || !s.Domain.Assumed || s.Domain.AssumedWhat != "registration" {
			t.Fatalf("%s: renewal-only supplier must be complete: %+v", action, s.Domain)
		}
		near(t, "first year from the renewal price", s.Domain.FirstYear, 23.5)
		near(t, "renewal", s.Domain.Ongoing, 23.5)
		near(t, "2 years", s.Domain.Term, 47)
		near(t, "option first year", s.FirstYear, 23.5+78)
		want := map[string]string{"register": "No registration price listed — assumed the same as the renewal", "transfer": "No transfer price listed — assumed the same as the renewal"}[action]
		if s.Domain.Note != want {
			t.Fatalf("note: %q", s.Domain.Note)
		}
		ok := false
		for _, f := range s.Flags {
			ok = ok || (f.Type == "assumed_registration" && f.Message == want+".")
		}
		if !ok {
			t.Fatalf("assumed_registration flag missing: %+v", s.Flags)
		}
		if r.Combo(".co.ke").Best != "s:syn" {
			t.Fatalf("it competes like any option: best=%s", r.Combo(".co.ke").Best)
		}
	}
	// A registration price, when listed, always wins over the assumption.
	r, _ := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"})
	if dp := opt(t, r, ".co.ke", "s:syn").Domain; dp.Assumed || dp.FirstYear != 21 {
		t.Fatalf("listed registration: %+v", dp)
	}
	// A renewal billed for longer than the term does not fit.
	for i := range d.Items {
		if d.Items[i].SupplierID == "syn" && d.Items[i].Kind == KindRenew && d.Items[i].TLD == ".co.ke" {
			d.Items[i].Period = Period2Y
		}
	}
	r = Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", TermYears: 1}.Normalize(d.Tiers), d)
	if s := opt(t, r, ".co.ke", "s:syn"); s.Complete || s.Domain != nil {
		t.Fatalf("a 2-year renewal does not fit a 1-year term: %+v", s.Domain)
	}
}

// withRegistrar adds a registrar that is dear to register with (30 AUD) but
// the cheapest to renew (10 AUD a year).
func withRegistrar(d Data) Data {
	d.Suppliers = append(d.Suppliers, Supplier{ID: "reg", Name: "Registrar X", Type: TypeWholesale, Currency: "AUD"})
	asOf := day("2026-09-28")
	d.Items = append(d.Items,
		Item{ID: "rx1", SupplierID: "reg", Kind: KindRegister, Name: ".co.ke registration", TLD: ".co.ke", Period: PeriodYear, Price: 30, Currency: "AUD", AsOf: asOf, Active: true},
		Item{ID: "rx2", SupplierID: "reg", Kind: KindRenew, Name: ".co.ke renewal", TLD: ".co.ke", Period: PeriodYear, Price: 10, Currency: "AUD", AsOf: asOf, Active: true})
	return d
}

func TestCompareRenewalMix(t *testing.T) {
	d := withRegistrar(fixture())
	r := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"}.Normalize(d.Tiers), d)
	c := r.Combo(".co.ke")
	// Year 1: Synergy's domain (21) + hosting.com's intro price — a promo trap.
	if c.Best != "mix" || c.Mix.Domain.SupplierID != "syn" || c.Mix.Hosting.SupplierID != "hc" || !c.Mix.PromoTrap {
		t.Fatalf("mix: %+v", c.Mix)
	}
	// To renew: the registrar's domain (10 a year) + Synergy's hosting (78 a year).
	m := c.MixOngoing
	if m == nil || m.Key != "mix_ongoing" || m.Domain.SupplierID != "reg" || m.Hosting.SupplierID != "syn" || m.Hosting.Name != "Starter Env" || !m.Complete {
		t.Fatalf("renewal mix: %+v", m)
	}
	near(t, "renewal mix first year", m.FirstYearH, 30*86+78*86)
	near(t, "renewal mix ongoing", m.OngoingH, 10*86+78*86)
	if m.SupplierName != "Registrar X + Synergy Wholesale" || m.Label != "Domain from Registrar X + hosting from Synergy Wholesale" || m.Currency != "AUD" {
		t.Fatalf("labels: %q / %q / %q", m.SupplierName, m.Label, m.Currency)
	}
	// It is the cheapest to renew, ahead of every single supplier, and says what it saves a year.
	if c.BestOngoing != "mix_ongoing" || !m.LowestOngoing || m.Cheapest || opt(t, r, ".co.ke", "s:syn").LowestOngoing || c.Mix.LowestOngoing {
		t.Fatalf("best ongoing: %s", c.BestOngoing)
	}
	near(t, "saves a year vs Synergy alone (8,729)", m.SavingOngoing, 8729-(10*86+78*86))
	if got := r.FindOption(".co.ke", "mix_ongoing"); got != m {
		t.Fatal("FindOption(mix_ongoing)")
	}
	// The retail helper prices it like any option: no promo, so the dearer year is year 1.
	res := Retail(m, c.Benchmarks, RetailInput{Percent: 35, Markets: []RetailMarket{{Currency: "KES", Rounding: "none"}}}.Normalize("KES"), d.Rates)
	if res.Key != "mix_ongoing" || res.BasisUsed != "first_year" {
		t.Fatalf("retail: %s %s", res.Key, res.BasisUsed)
	}
	near(t, "retail cost", res.CostHome, 30*86+78*86)

	// Over 2 years the term-cheapest mix IS that pair (30 + 10 < 21 + 23.50): not shown twice.
	r2 := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", TermYears: 2}.Normalize(d.Tiers), d)
	c2 := r2.Combo(".co.ke")
	if c2.Mix.Domain.SupplierID != "reg" || c2.Mix.Hosting.SupplierID != "syn" || c2.MixOngoing != nil {
		t.Fatalf("same pair as the mix: mix=%s+%s renewal=%v", c2.Mix.Domain.SupplierName, c2.Mix.Hosting.SupplierName, c2.MixOngoing)
	}
	if c2.Best != "mix" || c2.BestOngoing != "mix" || r2.FindOption(".co.ke", "mix_ongoing") != nil {
		t.Fatalf("2 years: best=%s ongoing=%s", c2.Best, c2.BestOngoing)
	}
	// Both halves from one supplier: that supplier's own option already shows it.
	r3, _ := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"})
	if c3 := r3.Combo(".co.ke"); c3.MixOngoing != nil || c3.BestOngoing != "s:syn" {
		t.Fatalf("one supplier is cheapest to renew: %v / %s", c3.MixOngoing, c3.BestOngoing)
	}
	// A pinned half: the user is choosing the mix.
	r4 := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter", HostingFrom: "hc"}.Normalize(d.Tiers), d)
	if r4.Combo(".co.ke").MixOngoing != nil {
		t.Fatal("no renewal mix while a half is pinned")
	}
	// Hosting or domain only: nothing to mix.
	r5 := Compare(CompareInput{Tier: "starter"}.Normalize(d.Tiers), d)
	if r5.Combo("").MixOngoing != nil {
		t.Fatal("hosting only has no mix")
	}
}

func TestRenewalMixPicksThePlanThatIsCheapestToRenew(t *testing.T) {
	// hosting.com: "Launch" is cheapest in year 1 (35.88, renews 131.88);
	// "Steady" (40.00 every year) is cheapest to renew. Its own option shows
	// Launch; the renewal mix must use Steady.
	d := fixture()
	d.Items = append(d.Items, Item{ID: "steady", SupplierID: "hc", Kind: KindHosting, Name: "Steady", NameKey: "steady", Period: PeriodYear, Price: 40, Currency: "USD",
		DiskGB: fp(5), Sites: ip(1), AsOf: day("2026-09-28"), Active: true})
	r := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"}.Normalize(d.Tiers), d)
	c := r.Combo(".co.ke")
	if got := opt(t, r, ".co.ke", "s:hc").Hosting.Name; got != "Launch" {
		t.Fatalf("hosting.com's own option is its cheapest over the term: %s", got)
	}
	if c.Mix.Hosting.Name != "Launch" {
		t.Fatalf("term mix: %s", c.Mix.Hosting.Name)
	}
	m := c.MixOngoing
	if m == nil || m.Domain.SupplierID != "syn" || m.Hosting.SupplierID != "hc" || m.Hosting.Name != "Steady" {
		t.Fatalf("renewal mix: %+v", m)
	}
	near(t, "ongoing", m.OngoingH, 23.5*86+40*129)
	near(t, "first year", m.FirstYearH, 21*86+40*129)
	if c.BestOngoing != "mix_ongoing" || m.PromoTrap {
		t.Fatalf("best ongoing %s, trap %v", c.BestOngoing, m.PromoTrap)
	}
	if len(m.ByCurrency) != 2 {
		t.Fatalf("two currencies: %+v", m.ByCurrency)
	}
}

func TestMultiSitePlansCount(t *testing.T) {
	r, _ := run(t, CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"})
	if r.MultiSitePlans != 1 { // hosting.com's Reseller 50
		t.Fatalf("multi-site plans: %d", r.MultiSitePlans)
	}
	r, _ = run(t, CompareInput{TLDs: []string{".com"}, NoHosting: true})
	if r.MultiSitePlans != 0 {
		t.Fatalf("domain only: %d", r.MultiSitePlans)
	}
	// A competitor's unlimited plan does not count: it is never spread.
	d := fixture()
	d.Items = append(d.Items, Item{ID: "cu", SupplierID: "ca", Kind: KindHosting, Name: "Unlimited", Period: PeriodYear, Price: 2000, Currency: "KES", DiskGB: fp(Unlimited), Sites: ip(Unlimited), Active: true})
	if got := Compare(CompareInput{Tier: "starter"}.Normalize(d.Tiers), d).MultiSitePlans; got != 1 {
		t.Fatalf("competitor plans counted: %d", got)
	}
}

func TestCompareHostingOnlyAndDomainOnly(t *testing.T) {
	r, _ := run(t, CompareInput{Tier: "starter"})
	c := r.Combo("")
	if c == nil || c.Label != "Hosting only" || c.Mix != nil {
		t.Fatalf("hosting only: %+v", c)
	}
	if s := opt(t, r, "", "s:syn"); !s.Complete || s.Domain != nil {
		t.Fatalf("synergy hosting only: %+v", s)
	}
	r, _ = run(t, CompareInput{TLDs: []string{".com"}, NoHosting: true})
	h := opt(t, r, ".com", "s:hc")
	if !h.Complete || h.Hosting != nil {
		t.Fatalf("domain only: %+v", h)
	}
	near(t, "domain only", h.FirstYear, 11.99)
	// 16.20 AUD = 1,393.20 KES beats 11.99 USD = 1,546.71 KES.
	if best := r.Combo(".com").Best; best != "s:syn" {
		t.Fatalf("best: %s", best)
	}
}

func TestCompareWhatIfRateAndMissingRate(t *testing.T) {
	d := fixture()
	d.Rates.Per["AUD"] = 100 // the service applies overrides to Per
	r := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"}.Normalize(d.Tiers), d)
	near(t, "what-if AUD 100", opt(t, r, ".co.ke", "s:syn").FirstYearH, 9900)
	// No USD rate: hosting.com cannot be compared and says so.
	d = fixture()
	delete(d.Rates.Per, "USD")
	delete(d.Rates.Info, "USD")
	r = Compare(CompareInput{TLDs: []string{".com"}, Tier: "starter"}.Normalize(d.Tiers), d)
	h := opt(t, r, ".com", "s:hc")
	if h.Complete {
		t.Fatal("no rate: not comparable")
	}
	ok := false
	for _, f := range h.Flags {
		ok = ok || f.Type == "no_rate"
	}
	if !ok {
		t.Fatalf("no_rate flag missing: %+v", h.Flags)
	}
	if r.Combo(".com").Best != "s:syn" {
		t.Fatalf("best: %s", r.Combo(".com").Best)
	}
}

func TestCompareStaleAndInactive(t *testing.T) {
	d := fixture()
	for i := range d.Items {
		if d.Items[i].Name == "Starter Env" {
			d.Items[i].AsOf = day("2026-05-01") // 154 days old
		}
		if d.Items[i].Name == "Launch" {
			d.Items[i].Active = false
		}
	}
	r := Compare(CompareInput{TLDs: []string{".co.ke"}, Tier: "starter"}.Normalize(d.Tiers), d)
	s := opt(t, r, ".co.ke", "s:syn")
	ok := false
	for _, f := range s.Flags {
		ok = ok || (f.Type == "stale" && strings.Contains(f.Message, "older than 90 days"))
	}
	if !s.Hosting.Stale || !ok {
		t.Fatalf("stale price must be flagged: %+v", s.Flags)
	}
	if h := opt(t, r, ".co.ke", "s:hc"); h.Hosting == nil || h.Hosting.Name == "Launch" {
		t.Fatalf("inactive plans are not used: %+v", h.Hosting)
	}
}

func TestNormalize(t *testing.T) {
	in := CompareInput{TLDs: []string{"CO.KE", ".co.ke", "*.com", "bad tld!", ""}, Tier: "Starter", TermYears: 7, ShareSites: -3, Mailboxes: -1, Rates: map[string]float64{"aud": 90, "USD": -1}}.Normalize(DefaultTiers())
	if strings.Join(in.TLDs, ",") != ".co.ke,.com" {
		t.Fatalf("tlds: %v", in.TLDs)
	}
	if in.Tier != "starter" || in.MinDiskGB != 2 || in.MinSites != 1 || in.TermYears != 1 || in.ShareSites != 1 || in.Mailboxes != 0 || in.DomainAction != "register" || in.SelfHostedBasis != "current" {
		t.Fatalf("defaults: %+v", in)
	}
	if len(in.Rates) != 1 || in.Rates["AUD"] != 90 {
		t.Fatalf("rates: %v", in.Rates)
	}
	if got := (CompareInput{Tier: "platinum", MinDiskGB: 3}).Normalize(DefaultTiers()); got.Tier != "" || got.MinDiskGB != 3 {
		t.Fatalf("unknown tier keeps the custom need: %+v", got)
	}
}
