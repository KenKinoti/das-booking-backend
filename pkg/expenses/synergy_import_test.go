package expenses

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
)

// Fixtures in testdata/synergy are anonymised copies of real Synergy
// Wholesale exports (same structure, amounts and edge cases, fake domains).

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "synergy", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const (
	fxAug   = "statement-August-2026.csv"
	fxSep   = "statement-September-2026.csv"
	fxUse01 = "hosting-account-usage-2026-09-01.csv"
	fxUse10 = "hosting-account-usage-2026-10-01.csv"
)

func TestClassifySynergy(t *testing.T) {
	d := func(s string) string { return s }
	cases := []struct {
		typ, desc        string
		cr, db           float64
		kind, dom, cat   string
		from, to         string
		planFrom, planTo string
	}{
		{"Web Hosting", "x-site.org (from 1 Sep 2026 to 1 Oct 2026)", 0, 14, SynHostingMonthly, "x-site.org", CatHosting, "2026-09-01", "2026-10-01", "", ""},
		{"Web Hosting", "x-site.org (Plan changed from Basic Env to Mini Powerful)", 0, 2.6, SynPlanUpgrade, "x-site.org", CatHosting, "", "", "Basic Env", "Mini Powerful"},
		{"Web Hosting", "big.org (Plan changed from Goliath ENV to Mega Env)", 5.16, 0, SynPlanDowngrade, "big.org", CatHosting, "", "", "Goliath ENV", "Mega Env"},
		{"Web Hosting", "Prorata for cancelled hosting service (shop.com.au)", 4.08, 0, SynCancelCredit, "shop.com.au", CatHosting, "", "", "", ""},
		{"Web Hosting", "new.co.ke - (Pro rata for 23 Sep 26 to 1 Nov 26)", 0, 5.38, SynHostingProrata, "new.co.ke", CatHosting, "2026-09-23", "2026-11-01", "", ""},
		{"Renewal", "Auto renewal of acme.com.au for 1 year", 0, 11.5, SynDomainRenewal, "acme.com.au", CatDomains, "", "", "", ""},
		{"Domain Delete", "Domain deletion for gone.com.au", 0, 0, SynDomainDelete, "gone.com.au", CatDomains, "", "", "", ""},
		{"Priority Ticket", "Submission - Outside 9-5", 0, 9.95, SynSupportFee, "", CatSupport, "", "", "", ""},
		{"Refill", "Payment via Credit Card", 45, 0, SynTopup, "", "", "", "", "", ""},
		{"Mystery", "Something new", 0, 1, SynOther, "", CatOther, "", "", "", ""},
	}
	for _, c := range cases {
		r := ClassifySynergy(c.typ, d(c.desc), c.cr, c.db)
		if r.Kind != c.kind || r.Domain != c.dom || r.Category != c.cat || r.PlanFrom != c.planFrom || r.PlanTo != c.planTo {
			t.Errorf("%q → kind=%s dom=%s cat=%s plans=%q→%q", c.desc, r.Kind, r.Domain, r.Category, r.PlanFrom, r.PlanTo)
		}
		if c.from != "" && (r.PeriodFrom == nil || r.PeriodFrom.Format(dayFmt) != c.from || r.PeriodTo.Format(dayFmt) != c.to) {
			t.Errorf("%q period %v %v", c.desc, r.PeriodFrom, r.PeriodTo)
		}
	}
	for _, s := range []string{"$1.54", "-$107.71", "$-1.00", "(2.50)", "1,234.50", "0.00"} {
		if _, ok := parseAmount(s); !ok {
			t.Errorf("parseAmount %q", s)
		}
	}
	if v, _ := parseAmount("-$107.71"); !near(v, -107.71) {
		t.Fatalf("negative balance %v", v)
	}
}

func TestParseSynergyStatementFixtures(t *testing.T) {
	if DetectSynergyFile(fixture(t, fxAug)) != SynFileStatement || DetectSynergyFile(fixture(t, fxUse10)) != SynFileUsage ||
		DetectSynergyFile([]byte("Date,Description,Debit,Credit\n")) != "" {
		t.Fatal("detect")
	}
	aug, w, err := ParseSynergyStatement(fixture(t, fxAug))
	if err != nil || len(aug) != 19 || len(w) != 0 {
		t.Fatalf("aug %d %v %v", len(aug), w, err)
	}
	// Newest first in the file: the last row is the oldest (Seq 0).
	if aug[18].Seq != 0 || aug[0].Seq != 18 || aug[0].Balance != -107.71 || !aug[0].HasBalance {
		t.Fatalf("order/balance %+v %+v", aug[0], aug[18])
	}
	sortSynRows(aug)
	if g := ReconcileSynergy(aug); len(g) != 0 {
		t.Fatalf("aug gaps %+v", g)
	}
	if o, c := OpeningBalance(aug), ClosingBalance(aug); !near(*o, 25.74) || !near(*c, -107.71) {
		t.Fatalf("aug opening %v closing %v", *o, *c)
	}
	// Plan change chain in one day: three lines, Micro → Basic → Mini Powerful → Hybrid.
	chain := []string{}
	for _, r := range aug {
		if r.Kind == SynPlanUpgrade {
			chain = append(chain, r.PlanFrom+">"+r.PlanTo)
		}
	}
	if strings.Join(chain, ",") != "Micro Env>Basic Env,Basic Env>Mini Powerful,Mini Powerful>Hybrid ENV" {
		t.Fatalf("chain %v", chain)
	}
	sep, _, _ := ParseSynergyStatement(fixture(t, fxSep))
	kinds := map[string]int{}
	var exp, top, canc float64
	for _, r := range sep {
		kinds[r.Kind]++
		if r.Kind == SynTopup {
			top += r.Credit
			continue
		}
		exp += r.Amount()
		if r.Kind == SynCancelCredit {
			canc += r.Credit
		}
	}
	if kinds[SynTopup] != 3 || kinds[SynCancelCredit] != 5 || kinds[SynDomainDelete] != 2 || kinds[SynSupportFee] != 1 || kinds[SynHostingProrata] != 1 ||
		kinds[SynPlanUpgrade] != 2 || kinds[SynPlanDowngrade] != 2 || kinds[SynHostingMonthly] != 15 {
		t.Fatalf("kinds %v", kinds)
	}
	if !near(top, 222.71) || !near(exp, 104.53) || !near(canc, 18.36) {
		t.Fatalf("sep top-ups %.2f expenses %.2f cancellations %.2f", top, exp, canc)
	}
	all := append(append([]SynRow{}, aug...), sep...)
	sortSynRows(all)
	if g := ReconcileSynergy(all); len(g) != 0 {
		t.Fatalf("aug+sep gaps %+v", g)
	}
	// A missing line is a gap.
	cut := append([]SynRow{}, all[:10]...)
	cut = append(cut, all[11:]...)
	if g := ReconcileSynergy(cut); len(g) != 1 || !strings.Contains(g[0].Message, "Balance jumps") {
		t.Fatalf("gap not found %+v", g)
	}
	// Same-second lines keep the file order (balance chain).
	if !sep[3].Date.Before(sep[2].Date) || sep[3].Seq >= sep[2].Seq {
		t.Fatalf("seq %+v %+v", sep[2], sep[3])
	}
}

func TestParseSynergyUsage(t *testing.T) {
	rows, w, err := ParseSynergyUsage(fixture(t, fxUse01))
	if err != nil || len(rows) != 15 || len(w) != 0 {
		t.Fatalf("%d %v %v", len(rows), w, err)
	}
	var av SynUsageRow
	for _, r := range rows {
		if r.Domain == "avocado-traders.co.ke" {
			av = r
		}
	}
	if av.RemainingMB != -10 || av.PctUsed != 101 || !av.At75 || av.Plan != "Micro Env" || av.LimitMB != 512 {
		t.Fatalf("over quota row %+v", av)
	}
	for name, want := range map[string]string{"hosting-account-usage-2026-10-01.csv": "2026-10-01", "usage_01-09-2026.csv": "2026-09-01", "usage20260815.csv": "2026-08-15",
		"usage 3 Oct 2026.csv": "2026-10-03"} {
		if d := SnapshotDateFromName(name); d == nil || d.Format(dayFmt) != want {
			t.Errorf("%s → %v", name, d)
		}
	}
	if SnapshotDateFromName("usage.csv") != nil {
		t.Fatal("no date expected")
	}
}

// fixtureTxns builds stored transactions from the fixtures (no database).
func fixtureTxns(t *testing.T, files ...string) []SynergyTxn {
	out := []SynergyTxn{}
	for _, f := range files {
		rows, _, err := ParseSynergyStatement(fixture(t, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			out = append(out, SynergyTxn{ID: uuid.NewString(), Date: r.Date, Seq: r.Seq, ProductType: r.ProductType, Description: r.Description, Domain: r.Domain, Kind: r.Kind,
				Category: r.Category, PeriodFrom: r.PeriodFrom, PeriodTo: r.PeriodTo, PlanFrom: r.PlanFrom, PlanTo: r.PlanTo, Credit: r.Credit, Debit: r.Debit, Amount: r.Amount(),
				Balance: r.Balance, HasBalance: r.HasBalance, Month: monthOf(r.Date), ServiceMonth: serviceMonth(r)})
		}
	}
	return out
}

func fixtureSnaps(t *testing.T, files ...string) []SynergyUsage {
	out := []SynergyUsage{}
	for _, f := range files {
		rows, _, _ := ParseSynergyUsage(fixture(t, f))
		d := SnapshotDateFromName(f)
		for _, r := range rows {
			out = append(out, SynergyUsage{SnapshotDate: *d, Domain: r.Domain, Status: r.Status, Plan: r.Plan, UsageMB: r.UsageMB, LimitMB: r.LimitMB, PctUsed: r.PctUsed, RemainingMB: r.RemainingMB})
		}
	}
	return out
}

func TestLearnPlans(t *testing.T) {
	plans, changes := LearnPlans(fixtureTxns(t, fxAug, fxSep), fixtureSnaps(t, fxUse01, fxUse10), nil)
	want := map[string]float64{"micro env": 4.25, "basic env": 5, "large env": 8.5, "mega env": 11, "hybrid env": 14, "goliath env": 16, "mini powerful": 8, "medium env": 6.5}
	for k, v := range want {
		p := plans[k]
		if p == nil || p.MonthlyPrice == nil || !near(*p.MonthlyPrice, v) {
			t.Errorf("plan %s = %+v (want %.2f)", k, p, v)
		}
	}
	if plans["medium env"].PriceSource != "plan change" || plans["micro env"].PriceSource != "charges" {
		t.Fatalf("sources %+v %+v", plans["medium env"], plans["micro env"])
	}
	if *plans["micro env"].LimitMB != 512 || *plans["goliath env"].LimitMB != 20480 || *plans["medium env"].LimitMB != 5120 {
		t.Fatal("limits")
	}
	if len(changes) != 0 {
		t.Fatalf("no price change expected: %v", changes)
	}
	// A manual price wins.
	p := 9.0
	plans, _ = LearnPlans(fixtureTxns(t, fxAug, fxSep), nil, []SynergyPlan{{Key: "medium env", Name: "Medium Env", MonthlyPrice: &p}})
	if *plans["medium env"].MonthlyPrice != 9 || plans["medium env"].PriceSource != "manual" {
		t.Fatal("manual")
	}
}

func findSite(r *SynergyReport, d string) *SynSite {
	for i := range r.Sites {
		if r.Sites[i].Domain == d {
			return &r.Sites[i]
		}
	}
	return nil
}

func findDecision(r *SynergyReport, typ string) *SynDecision {
	for i := range r.Decisions {
		if r.Decisions[i].Type == typ {
			return &r.Decisions[i]
		}
	}
	return nil
}

func TestBuildSynergyReport(t *testing.T) {
	today := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	in := SynInput{Today: today, Home: "AUD", Txns: fixtureTxns(t, fxSep, fxAug), Snaps: fixtureSnaps(t, fxUse10, fxUse01), LowBalance: 20,
		Clients: []SynClient{
			{ID: "c-avo", Name: "Avo Owner", Website: "https://www.avocado-traders.co.ke/"},
			{ID: "c-locks", Name: "Locks", Email: "admin@locks-studio.com.au"},
			{ID: "c-gmail", Name: "Gmail User", Email: "someone@gmail.com"},
			{ID: "c-water", Name: "Contact", Company: "Water Network Ltd"},
		},
		Billed: []SynBilledLine{
			{InvoiceID: "i1", Number: "INV-1", Date: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), CustomerID: "c-avo", Description: "Hosting avocado-traders.co.ke — September", MonthlyHome: 15},
			{InvoiceID: "i0", Number: "INV-0", Date: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC), CustomerID: "c-avo", Description: "Hosting avocado-traders.co.ke — August", MonthlyHome: 12},
			{InvoiceID: "i2", Number: "INV-2", Date: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), CustomerID: "c-locks", Description: "locks-studio.com.au hosting", MonthlyHome: 8},
		}}
	r := BuildSynergyReport(in)
	k := r.KPIs
	if !near(k.LastMonth, 104.53) || !near(k.YTD, 237.98) || k.ThisMonth != 0 || !near(k.TopupsTotal, 222.71) {
		t.Fatalf("spend kpis %+v", k)
	}
	if k.ActiveSites != 11 || k.CancelledSites != 5 || k.NewSites != 1 || !near(k.RunRate, 89.25) {
		t.Fatalf("sites kpis active=%d cancelled=%d new=%d run-rate=%.2f", k.ActiveSites, k.CancelledSites, k.NewSites, k.RunRate)
	}
	// Runway: $10.47 left, ~$89.25 charged around the 25th → top up $78.78 before 25 Oct.
	if k.Balance == nil || !near(*k.Balance, 10.47) || k.NextChargeDate != "2026-10-25" || !near(k.TopupNeeded, 78.78) || k.RunwayDays == nil || *k.RunwayDays != 24 {
		t.Fatalf("runway %+v", k)
	}
	if len(r.NegativeSpells) != 2 || r.NegativeSpells[0].From != "2026-08-25" || r.NegativeSpells[0].To != "2026-09-23" || !near(r.NegativeSpells[0].Lowest, -107.71) {
		t.Fatalf("negative spells %+v", r.NegativeSpells)
	}
	if !near(k.PlanChangesMonth, -5.94) || len(r.Gaps) != 0 {
		t.Fatalf("plan changes %.2f gaps %v", k.PlanChangesMonth, r.Gaps)
	}
	// Months (charge basis).
	ms := map[string]SynMonth{}
	for _, m := range r.Months {
		ms[m.Month] = m
	}
	if !near(ms["2026-08"].Total, 133.45) || !near(ms["2026-08"].Hosting, 121.95) || !near(ms["2026-08"].Domains, 11.5) || !near(ms["2026-09"].Support, 9.95) ||
		!near(ms["2026-09"].Topups, 222.71) || !near(ms["2026-09"].Credits, 26.62) {
		t.Fatalf("months %+v", r.Months)
	}
	// Service basis: September's hosting is the charge of 25–26 Aug.
	in.Basis = "service"
	rs := BuildSynergyReport(in)
	sm := map[string]SynMonth{}
	for _, m := range rs.Months {
		sm[m.Month] = m
	}
	if !near(sm["2026-08"].Total, 19.95) || !near(sm["2026-09"].Total, 104.53) || !near(sm["2026-10"].Total, 113.5) {
		t.Fatalf("service months %+v", rs.Months)
	}
	in.Basis = ""

	av := findSite(r, "avocado-traders.co.ke")
	if av == nil || !containsStr(av.Flags, "over_quota") || av.SuggestedPlan != "Basic Env" || !near(*av.SuggestedDelta, 0.75) || *av.DaysToFull != 0 {
		t.Fatalf("over quota site %+v", av)
	}
	// Client by website; billed from the latest invoice; margin.
	if av.CustomerID != "c-avo" || av.ClientMatch != "invoice" || !near(*av.Billed, 15) || !near(*av.Margin, 10.75) || !near(*av.MarginPct, 71.7) {
		t.Fatalf("avocado billing %+v", av)
	}
	locks := findSite(r, "locks-studio.com.au")
	if locks.CustomerID != "c-locks" || !containsStr(locks.Flags, "below_cost") || !near(*locks.Margin, -3) {
		t.Fatalf("below cost %+v", locks)
	}
	water := findSite(r, "water-network.org")
	if water.Plan != "Mega Env" || !containsStr(water.Flags, "near_full") || water.CustomerID != "c-water" || water.ClientMatch != "name" || water.Trend != "down" {
		t.Fatalf("downgraded site %+v", water)
	}
	sav := findSite(r, "savanna-women.org")
	if !containsStr(sav.Flags, "fast_growth") || sav.DaysToFull == nil || *sav.DaysToFull < 14 || *sav.DaysToFull > 17 || sav.SuggestedPlan != "Goliath ENV" {
		t.Fatalf("fast growth %+v (days %v)", sav, sav.DaysToFull)
	}
	if s := findSite(r, "info-hub.co.ke"); s.Status != "new" || s.Plan != "Medium Env" || !near(s.MonthlyCost, 6.5) {
		t.Fatalf("new site %+v", s)
	}
	gone := 0
	for _, d := range []string{"kilimo-supplies.co.ke", "green-house-advisory.com", "farm-feeds.co.ke", "budget-luxe.com.au", "ago-advisory.com.au"} {
		if s := findSite(r, d); s.Status == "cancelled" && s.MonthlyCost == 0 {
			gone++
		}
	}
	if gone != 5 {
		t.Fatal("5 cancelled sites expected")
	}
	if d := findSite(r, "acme-digital.com.au"); !near(d.DomainMonthly, 0.96) || !near(d.TotalMonthly, 11.96) {
		t.Fatalf("domain renewal per month %+v", d)
	}
	// Decisions.
	for _, typ := range []string{"over_quota", "near_full", "fast_growth", "plan_changes", "unbilled", "below_cost", "balance", "support_fees", "cancellations", "new_site", "downsize"} {
		if findDecision(r, typ) == nil {
			t.Errorf("decision %s missing", typ)
		}
	}
	if d := findDecision(r, "over_quota"); !strings.Contains(d.Title, "avocado-traders.co.ke is over quota (101%) on Micro Env — upgrade to Basic Env (+$0.75/mo)") {
		t.Fatalf("over quota title %q", d.Title)
	}
	if d := findDecision(r, "cancellations"); !strings.Contains(d.Title, "Cancelled 5 services → saving $22.75/mo") {
		t.Fatalf("cancellations %q", d.Title)
	}
	if d := findDecision(r, "balance"); !strings.Contains(d.Title, "25 Aug and 24 Sep") || !strings.Contains(d.Title, "top up $78.78 by 24 Oct") {
		t.Fatalf("balance %q", d.Title)
	}
	if d := findDecision(r, "support_fees"); !strings.Contains(d.Title, "$9.95 — 1") {
		t.Fatalf("support %q", d.Title)
	}
	if d := findDecision(r, "unbilled"); d.Numbers["count"] != 9 {
		t.Fatalf("unbilled %+v", d)
	}
	// Comparison Aug → Sep.
	c := r.Comparison
	if c == nil || !near(c.CostFrom, 133.45) || !near(c.CostTo, 104.53) || len(c.New) != 1 || c.New[0] != "info-hub.co.ke" || len(c.Cancelled) != 5 || len(c.PlanChanges) != 3 {
		t.Fatalf("comparison %+v", c)
	}
	// Alerts for the notifications.
	types := map[string]int{}
	for _, a := range r.Alerts {
		types[a.Type]++
	}
	if types["synergy_over_quota"] != 1 || types["synergy_quota"] < 1 || types["synergy_unbilled"] != 1 {
		t.Fatalf("alerts %v", types)
	}
	// Manual "not a client" removes the flags.
	in.Sites = []SynergySite{{Domain: "acme-digital.com", NoClient: true}}
	if s := findSite(BuildSynergyReport(in), "acme-digital.com"); containsStr(s.Flags, "unbilled") || containsStr(s.Flags, "no_client") {
		t.Fatalf("own site %+v", s)
	}
}

func TestMonthsCoveredAndMatch(t *testing.T) {
	cases := map[string]float64{"Hosting x.com (annual)": 12, "Web hosting x.com 12 months": 12, "Hosting x.com – October": 1, "Hosting 3 months x.com": 3, "x.com quarterly hosting": 3, "x.com hosting 2 years": 24}
	for d, want := range cases {
		if got := monthsCovered(d, "", 1); got != want {
			t.Errorf("%q → %v", d, got)
		}
	}
	if monthsCovered("x.com", "months", 6) != 6 || monthsCovered("x.com", "year", 1) != 12 {
		t.Fatal("unit")
	}
	cl := []SynClient{{ID: "a", Email: "x@gmail.com"}, {ID: "b", Company: "Hands-On Recruit Pty"}}
	if id, how := MatchClient("gmail.com", cl); id != "" || how != "" {
		t.Fatal("generic email domain must not match")
	}
	if id, how := MatchClient("handson-recruit.com.au", cl); id != "b" || how != "name" {
		t.Fatalf("name match %s %s", id, how)
	}
}

func uploadFixture(t *testing.T, s *Service, org, name string, date *time.Time) *SynergyUploadSummary {
	t.Helper()
	r, err := s.UploadAs(context.Background(), org, "u1", name, "text/csv", fixture(t, name), "", "", date)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if r.Synergy == nil {
		t.Fatalf("%s not routed to Synergy", name)
	}
	return r.Synergy
}

func TestSynergyUploadDedupeAndStatementExpenses(t *testing.T) {
	db := testDB(t)
	if err := db.AutoMigrate(&finance.BankAccount{}); err != nil {
		t.Fatal(err)
	}
	NotifyAfterUpload = false
	org := newOrg(t, db, "AUD")
	s := NewService(db, &fxrates.Service{DB: db})
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	// September first, then August (overlap-free but out of order).
	sep := uploadFixture(t, s, org, fxSep, nil)
	if sep.New != 31 || sep.Duplicates != 0 || !sep.Reconciled || sep.Continuity != "first" || !near(*sep.Opening, -107.71) || !near(*sep.Closing, 10.47) ||
		!near(sep.Topups, 222.71) || !near(sep.Expenses, 104.53) || sep.PeriodFrom != "2026-09-23" || sep.PeriodTo != "2026-09-30" {
		t.Fatalf("sep summary %+v", sep)
	}
	aug := uploadFixture(t, s, org, fxAug, nil)
	if aug.New != 19 || aug.Continuity != "ok" || len(aug.Gaps) != 0 {
		t.Fatalf("aug summary %+v", aug)
	}
	// Re-upload: nothing new.
	again := uploadFixture(t, s, org, fxSep, nil)
	if again.New != 0 || again.Duplicates != 31 {
		t.Fatalf("re-upload %+v", again)
	}
	var n int64
	db.Model(&SynergyTxn{}).Where("organization_id = ?", org).Count(&n)
	if n != 50 {
		t.Fatalf("transactions %d", n)
	}
	// An overlapping export (last days of Aug + Sep) adds nothing either.
	augRows := strings.Split(strings.TrimSpace(string(fixture(t, fxAug))), "\n")
	sepRows := strings.Split(strings.TrimSpace(string(fixture(t, fxSep))), "\n")
	overlap := strings.Join(append(append([]string{sepRows[0]}, sepRows[1:]...), augRows[1:4]...), "\n")
	r, err := s.UploadAs(context.Background(), org, "u1", "statement-custom.csv", "text/csv", []byte(overlap), "", "", nil)
	if err != nil || r.Synergy.New != 0 || r.Synergy.Duplicates != 34 {
		t.Fatalf("overlap %+v %v", r.Synergy, err)
	}
	// The balance follows the newest statement line.
	if c := s.connection(org, "synergy"); c == nil || c.Balance == nil || !near(*c.Balance, 10.47) || c.BalanceSource != "statement" {
		t.Fatalf("balance %+v", c)
	}
	// A statement with a missing month is flagged.
	gapped := strings.Replace(string(fixture(t, fxAug)), "$1.54", "$9.54", 1)
	g, err := s.UploadAs(context.Background(), org, "u1", "statement-bad.csv", "text/csv", []byte(gapped), "", "", nil)
	if err != nil || g.Synergy.Reconciled || len(g.Synergy.Gaps) == 0 {
		t.Fatalf("gap %+v %v", g.Synergy, err)
	}
	db.Where("organization_id = ? AND upload_id = ?", org, g.Synergy.UploadID).Delete(&SynergyTxn{})
	if _, err := s.ApplySynergyAccounting(org, "u1"); err != nil {
		t.Fatal(err)
	}

	// Usage reports: date from the file name; a chosen date overrides.
	u1 := uploadFixture(t, s, org, fxUse01, nil)
	if u1.New != 15 || u1.SnapshotDate != "2026-09-01" || u1.DateSource != "file name" {
		t.Fatalf("usage 1 %+v", u1)
	}
	u2 := uploadFixture(t, s, org, fxUse10, nil)
	if u2.New != 11 || u2.Previous != "2026-09-01" || len(u2.Gone) != 5 || len(u2.Appeared) != 1 || u2.Appeared[0] != "info-hub.co.ke" {
		t.Fatalf("usage 2 %+v", u2)
	}
	if u3 := uploadFixture(t, s, org, fxUse10, nil); u3.New != 0 || u3.Duplicates != 11 {
		t.Fatalf("usage re-upload %+v", u3)
	}
	if _, err := s.UploadAs(context.Background(), org, "u1", "usage.csv", "text/csv", fixture(t, fxUse10), "", "", nil); err == nil || !strings.Contains(err.Error(), "choose the date") {
		t.Fatalf("missing date %v", err)
	}
	chosen := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	if u4 := uploadFixture(t, s, org, fxUse10, &chosen); u4.SnapshotDate != "2026-09-15" || u4.DateSource != "chosen" {
		t.Fatalf("chosen date %+v", u4)
	}
	db.Where("organization_id = ? AND snapshot_date = ?", org, chosen).Delete(&SynergyUsage{})

	rep := s.SynergyReport(org, now, "")
	if !near(rep.KPIs.RunRate, 89.25) || rep.KPIs.OverQuota != 1 || len(rep.Uploads) == 0 {
		t.Fatalf("report %+v", rep.KPIs)
	}

	// The uploads created and approved the statement expenses automatically
	// (default: service charges by charge date, auto-approve on).
	v := s.synergyVendor(org)
	if v.CountFrom != "statement" {
		t.Fatalf("Synergy should be counted from statements: %q", v.CountFrom)
	}
	pv, err := s.StatementExpenses(org, StatementExpenseInput{})
	if err != nil || len(pv.Groups) != 4 || !near(pv.Total, 238.0-0.02) {
		t.Fatalf("preview %+v %v", pv, err)
	}
	want := map[string]float64{"2026-08|hosting": 121.95, "2026-08|domains": 11.5, "2026-09|hosting": 94.58, "2026-09|support": 9.95}
	for _, g := range pv.Groups {
		if !near(g.Total, want[g.Key]) || g.Change != "unchanged" || g.ItemStatus != StatusApproved {
			t.Errorf("group %s = %.2f %s %s", g.Key, g.Total, g.Change, g.ItemStatus)
		}
	}
	// A Synergy receipt entered by hand for a counted month cannot be approved (count once).
	amt, date := 45.0, "2026-09-30"
	rcpt, err := s.CreateManual(org, ItemInput{VendorID: &v.ID, InvoiceDate: &date, Total: &amt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(org, "u1", rcpt.ID, ApproveInput{}); err == nil || !strings.Contains(err.Error(), "already counted from the statement") {
		t.Fatalf("count-once guard: %v", err)
	}
	res, err := s.CreateStatementExpenses(org, "u1", StatementExpenseInput{UseStatement: true, Approve: true})
	if err != nil || res.Created != 0 || res.Unchanged != 4 || res.Ignored != 1 || len(res.Errors) != 0 || res.CountFrom != "statement" {
		t.Fatalf("create %+v %v", res, err)
	}
	if it, _ := s.Get(org, rcpt.ID); it.Status != StatusIgnored || it.BillID != "" {
		t.Fatalf("receipt should be ignored: %+v", it)
	}
	// One bill per month × category, line per domain, payments on the charge dates.
	var items []Item
	db.Where("organization_id = ? AND source = ?", org, synStatementSource).Find(&items)
	var billTotal float64
	for _, it := range items {
		b, err := s.Fin.GetBill(org, it.BillID)
		if err != nil {
			t.Fatal(err)
		}
		billTotal += b.Total
		pos := 0
		for _, l := range it.Lines {
			if l.Amount > 0 {
				pos++
			}
		}
		if b.Status != "paid" || len(b.LineItems) != pos {
			t.Fatalf("bill %+v", b)
		}
		if it.ExternalID == "2026-09|hosting" {
			if len(it.Lines) != 16 || b.Payments[0].PaymentDate.Format(dayFmt) != "2026-09-30" {
				t.Fatalf("sep hosting %d lines, payment %v", len(it.Lines), b.Payments[0].PaymentDate)
			}
		}
	}
	if !near(billTotal, 237.98) {
		t.Fatalf("bills total %.2f", billTotal)
	}
	// A new Synergy receipt from Gmail is ignored automatically now.
	d30 := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	gm, _, err := s.Ingest(org, Incoming{Source: SourceGmail, ExternalID: "gmail-rcpt-20", VendorID: v.ID, X: Extracted{Kind: KindReceipt, Total: 20, Currency: "AUD", Date: &d30, InvoiceNumber: "2746999"}})
	if err != nil || gm.Status != StatusIgnored || !strings.Contains(gm.Notes, "counted from the monthly statement") {
		t.Fatalf("gmail receipt should be ignored when counting statements: %+v %v", gm, err)
	}
	// Re-upload changes nothing; switching to the service basis regroups and re-approves.
	uploadFixture(t, s, org, fxAug, nil)
	db.Where("organization_id = ? AND source = ?", org, synStatementSource).Find(&items)
	if len(items) != 4 {
		t.Fatalf("re-upload created items: %d", len(items))
	}
	basis := "service"
	ar, err := s.SaveAccounting(org, "u1", AccountingInput{Basis: &basis})
	if err != nil || len(ar.Errors) != 0 {
		t.Fatalf("service basis %+v %v", ar, err)
	}
	var svc []Item
	db.Where("organization_id = ? AND source = ? AND status = ?", org, synStatementSource, StatusApproved).Find(&svc)
	byKey := map[string]Item{}
	var tot float64
	for _, it := range svc {
		byKey[it.ExternalID] = it
		tot += it.Total
	}
	oct := byKey["2026-10|hosting"]
	if !near(tot, 237.98) || oct.ID == "" || !near(oct.Total, 113.5) || oct.InvoiceDate.Format(dayFmt) != "2026-10-01" || oct.PaidDate.Format(dayFmt) != "2026-09-25" {
		t.Fatalf("service basis items %v total %.2f oct %+v", len(svc), tot, oct)
	}
	b, _ := s.Fin.GetBill(org, oct.BillID)
	if b.BillDate.Format(dayFmt) != "2026-10-01" || b.Payments[0].PaymentDate.Format(dayFmt) != "2026-09-25" {
		t.Fatalf("accrual bill date / cash payment date %v %v", b.BillDate, b.Payments[0].PaymentDate)
	}
}

func TestIsHostingLine(t *testing.T) {
	cases := map[string]bool{
		"example.co.ke hosting (Oct 2026)":           true,
		"example.co.ke":                              true,
		"Domain renewal example.co.ke 1 year":        true,
		"Website design & development example.co.ke": false,
		"example.co.ke website build":                false,
		"Monthly maintenance example.co.ke":          true,
	}
	for in, want := range cases {
		if got := isHostingLine(in); got != want {
			t.Errorf("isHostingLine(%q) = %v, want %v", in, got, want)
		}
	}
}
