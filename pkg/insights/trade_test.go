package insights

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
)

// tradeFixtures: a Kenyan (KES) business with domestic clients, two
// Australian clients billed in AUD, a Ugandan client billed in USD without a
// locked rate, and a client with no country. "Today" is 30 Sep 2026; today's
// rates are manual: 1 AUD = 90, 1 USD = 130, 1 EUR = 150 KES.
//
//	#1  K1  KES 100,000            10 Mar 2026  paid 20 Mar (10 days)
//	#2  K1  KES  50,000             1 Aug 2026  open
//	#3  A1  AUD   1,000 @ 80        5 Apr 2026  paid 5 May (30 days; May rate 85)
//	#4  A1  AUD     500 @ 80        1 Sep 2026  open
//	#5  A2  AUD     200 @ 100      10 Sep 2026  50 paid 20 Sep (this month: today's rate), 150 open
//	#6  U1  USD     100 (no rate)  15 Jun 2026  open
//	#7  N1  KES  10,000             1 Jul 2026  open, client without a country → assumed domestic
//	#8  N1  AUD     100 @ 80        2 Jul 2026  open, client without a country → Australia (from the currency)
//	CN  A1  AUD    −100 @ 80       20 Apr 2026  credit note
//	#10 A1  AUD     300 @ 80        1 Apr 2025  (a year earlier) paid 15 Jan 2026 — no rate recorded for that month
//	#11 K1  KES  60,000             1 May 2025  (a year earlier)
func tradeFixtures(t *testing.T) (*Intel, *gorm.DB) {
	db := testDB(t)
	if err := MigrateIntel(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&expenses.FxMonth{}); err != nil {
		t.Fatal(err)
	}
	if err := fxrates.Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE expense_items ADD COLUMN currency text, ADD COLUMN net numeric, ADD COLUMN vendor_name text`,
		`CREATE TABLE users (id varchar(255) PRIMARY KEY, organization_id varchar(255), email text, role text, is_active boolean, created_at timestamptz, deleted_at timestamptz)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	db.Exec(`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES (?, 'Test Web Co', 'KE', 'KES', 'Africa/Nairobi', 'user')`, org)
	for id, cc := range map[string]string{"K1": "KE", "A1": "AU", "A2": "au", "U1": "UG", "N1": ""} {
		db.Exec(`INSERT INTO customers (id, organization_id, first_name, last_name, company_name, country_code, created_at) VALUES (?, ?, 'C', ?, ?, ?, now())`, id, org, id, "Company "+id, cc)
	}
	f := &fx{t: t, db: db}
	i1 := f.inv("invoice", "sent", "K1", "KES", 1, "2026-03-10", 100000, 0)
	f.pay(i1, "2026-03-20", 100000)
	f.inv("invoice", "sent", "K1", "KES", 1, "2026-08-01", 50000, 0)
	i3 := f.inv("invoice", "sent", "A1", "AUD", 0.0125, "2026-04-05", 1000, 0)
	f.pay(i3, "2026-05-05", 1000)
	f.inv("invoice", "sent", "A1", "AUD", 0.0125, "2026-09-01", 500, 0)
	i5 := f.inv("invoice", "sent", "A2", "AUD", 0.01, "2026-09-10", 200, 0)
	f.pay(i5, "2026-09-20", 50)
	f.inv("invoice", "sent", "U1", "USD", 0, "2026-06-15", 100, 0)
	f.inv("invoice", "sent", "N1", "KES", 1, "2026-07-01", 10000, 0)
	f.inv("invoice", "sent", "N1", "AUD", 0.0125, "2026-07-02", 100, 0)
	f.inv("credit_note", "paid", "A1", "AUD", 0.0125, "2026-04-20", 100, 0)
	i10 := f.inv("invoice", "sent", "A1", "AUD", 0.0125, "2025-04-01", 300, 0)
	f.pay(i10, "2026-01-15", 300)
	f.inv("invoice", "sent", "K1", "KES", 1, "2025-05-01", 60000, 0)
	f.inv("invoice", "draft", "A1", "AUD", 0.0125, "2026-09-05", 9999, 0)
	f.inv("invoice", "void", "A1", "AUD", 0.0125, "2026-09-05", 8888, 0)
	// Month rates recorded by Expenses (AUD → KES).
	for m, r := range map[string]float64{"2026-05": 85, "2026-06": 86, "2026-07": 84, "2026-08": 88} {
		db.Create(&expenses.FxMonth{ID: uuid.NewString(), OrganizationID: org, FromCurrency: "AUD", ToCurrency: "KES", Month: m, Rate: r, Source: "live"})
	}
	// Foreign-currency costs: Synergy (AUD) and Google Workspace (EUR); others ignored.
	db.Exec(`INSERT INTO expense_items (id, organization_id, status, category, invoice_date, home_net, currency, net, vendor_name) VALUES
		('t1', ?, 'approved', 'hosting', '2026-06-01', 25800, 'AUD', 300, 'Synergy Wholesale'),
		('t2', ?, 'approved', 'hosting', '2026-07-01', 8400, 'AUD', 100, 'Synergy Wholesale'),
		('t3', ?, 'approved', 'software', '2026-07-01', 7500, 'EUR', 50, 'Google Workspace'),
		('t4', ?, 'pending', 'hosting', '2026-07-01', 99999, 'AUD', 999, 'Synergy Wholesale'),
		('t5', ?, 'approved', 'other', '2026-07-01', 5000, 'KES', 5000, 'Local supplier'),
		('t6', ?, 'approved', 'hosting', '2024-01-01', 8000, 'AUD', 100, 'Synergy Wholesale')`, org, org, org, org, org, org)
	fxs := &fxrates.Service{DB: db}
	for cur, r := range map[string]float64{"AUD": 90, "USD": 130, "EUR": 150} {
		if _, err := fxs.SaveManual(org, "u1", cur, "KES", r, "test"); err != nil {
			t.Fatal(err)
		}
	}
	s := New(db, fxs, nil, nil, nil)
	s.Now = func() time.Time { return d("2026-09-30") }
	return NewIntel(s, NewFetcher("http://127.0.0.1:1")), db
}

func fp(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || !near(*got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestTradeByCountryAndCurrency(t *testing.T) {
	i, _ := tradeFixtures(t)
	tr := i.TradeFor(org, Opts{Range: rng("2025-10-01", "2026-09-30"), Today: d("2026-09-30")})
	tt := tr.Totals
	if tr.Currency != "KES" || tr.HomeCountry != "KE" {
		t.Fatalf("home %s %s", tr.Currency, tr.HomeCountry)
	}
	// Domestic 100,000 + 50,000 + 10,000; export AU 80,000 + 40,000 + 20,000 + 8,000 − 8,000, UG 100 USD × 130.
	if !near(tt.Domestic, 160000) || !near(tt.Export, 153000) || !near(tt.Revenue, 313000) {
		t.Fatalf("totals %+v", tt)
	}
	fp(t, "export share", tt.ExportPct, 48.9)
	fp(t, "previous export share", tt.PrevExportPct, 28.6) // 24,000 of 84,000
	fp(t, "export YoY", tt.ExportYoY, 537.5)
	if tt.ExportClients != 4 || tt.ExportMarkets != 2 || tt.InferredDocs != 1 || tt.AssumedDocs != 1 || tt.AssumedClients != 1 || tt.Excluded != 0 {
		t.Fatalf("counts %+v", tt)
	}
	if len(tr.Countries) != 3 {
		t.Fatalf("countries %+v", tr.Countries)
	}
	ke, au, ug := tr.Countries[0], tr.Countries[1], tr.Countries[2]
	if ke.Country != "KE" || !ke.Domestic || !near(ke.Revenue, 160000) || ke.Invoices != 3 || ke.Clients != 2 || !near(ke.SharePct, 51.1) || !near(ke.Prev, 60000) {
		t.Fatalf("KE %+v", ke)
	}
	fp(t, "KE days to pay", ke.AvgDaysToPay, 10)
	if au.Country != "AU" || au.Domestic || !near(au.Revenue, 140000) || au.Invoices != 4 || au.Clients != 3 || !near(au.AvgInvoice, 37000) || au.Inferred != 1 || au.PaidInvoices != 1 || au.Name != "Australia" {
		t.Fatalf("AU %+v", au)
	}
	fp(t, "AU days to pay", au.AvgDaysToPay, 30)
	fp(t, "AU YoY", au.YoY, 483.3)
	if ug.Country != "UG" || !near(ug.Revenue, 13000) || ug.AvgDaysToPay != nil {
		t.Fatalf("UG %+v", ug)
	}
	// By currency: amounts in the currency and at the locked (or today's) rate.
	by := map[string]TradeCurrency{}
	for _, c := range tr.Currencies {
		by[c.Currency] = c
	}
	if c := by["AUD"]; !near(c.Amount, 1700) || !near(c.Home, 140000) || c.Docs != 5 || !c.Foreign {
		t.Fatalf("AUD %+v", c)
	}
	if c := by["KES"]; !near(c.Amount, 160000) || c.Foreign || c.Docs != 3 {
		t.Fatalf("KES %+v", c)
	}
	if c := by["USD"]; !near(c.Amount, 100) || !near(c.Home, 13000) {
		t.Fatalf("USD %+v", c)
	}
	// Trend: 12 months, September = 40,000 + 20,000 export, nothing domestic.
	if len(tr.Months) != 12 || tr.Months[0].Month != "2025-10" || tr.Months[11].Month != "2026-09" {
		t.Fatalf("months %+v", tr.Months)
	}
	if m := tr.Months[11]; !near(m.Export, 60000) || m.Domestic != 0 || *m.ExportPct != 100 {
		t.Fatalf("September %+v", m)
	}
	if m := tr.Months[0]; m.ExportPct != nil || m.Export != 0 {
		t.Fatalf("empty month %+v", m)
	}
	// Top export clients: A1 80,000 + 40,000 − 8,000 = 112,000.
	if len(tr.TopExport) != 4 || tr.TopExport[0].Name != "Company A1" || !near(tr.TopExport[0].Revenue, 112000) || tr.TopExport[0].Country != "AU" || !near(tr.TopExport[0].SharePct, 73.2) || tr.TopExport[0].Currencies != "AUD" {
		t.Fatalf("top export %+v", tr.TopExport)
	}
	if len(tr.Notes) < 3 || !strings.Contains(strings.Join(tr.Notes, " "), "assumed domestic") || !strings.Contains(strings.Join(tr.Notes, " "), "filed under the currency's country") {
		t.Fatalf("notes %v", tr.Notes)
	}
}

func TestTradeFXGainLossAndHedge(t *testing.T) {
	i, db := tradeFixtures(t)
	tr := i.TradeFor(org, Opts{Range: rng("2025-10-01", "2026-09-30"), Today: d("2026-09-30")})
	rc := tr.Receivables
	// Open AUD: 500 @ 80 + 150 @ 100 + 100 @ 80 = 750 AUD, 63,000 KES when invoiced; today 750 × 90 = 67,500.
	if len(rc.Rows) != 2 || rc.Rows[0].Currency != "AUD" {
		t.Fatalf("receivable rows %+v", rc.Rows)
	}
	aud := rc.Rows[0]
	if aud.Invoices != 3 || !near(aud.Balance, 750) || !near(aud.LockedBalance, 750) || !near(aud.LockedHome, 63000) || aud.TodayRate != 90 || aud.NoLocked != 0 || aud.RateSource != "manual" {
		t.Fatalf("AUD receivables %+v", aud)
	}
	fp(t, "AUD today value", aud.TodayHome, 67500)
	fp(t, "AUD unrealised = 750 × 90 − 63,000", aud.Unrealised, 4500)
	fp(t, "AUD unrealised %", aud.UnrealisedPct, 7.1)
	fp(t, "AUD average locked rate", aud.AvgLockedRate, 84)
	// USD invoice without a locked rate: listed, but no gain / loss.
	usd := rc.Rows[1]
	if usd.Currency != "USD" || usd.NoLocked != 1 || !near(usd.Balance, 100) || usd.Unrealised != nil || usd.TodayHome != nil || usd.LockedHome != 0 {
		t.Fatalf("USD receivables %+v", usd)
	}
	if !near(rc.LockedHome, 63000) || !near(rc.TodayHome, 67500) || !near(rc.Unrealised, 4500) || rc.Invoices != 4 || !near(rc.HomeBalance, 120000) { // open KES: #2, #7 and last year's #11
		t.Fatalf("receivable totals %+v", rc)
	}
	// A weaker AUD today turns the gain into a loss: 750 × 70 − 63,000 = −10,500.
	if _, err := i.FX.SaveManual(org, "u1", "AUD", "KES", 70, ""); err != nil {
		t.Fatal(err)
	}
	loss := i.TradeFor(org, Opts{Range: rng("2025-10-01", "2026-09-30"), Today: d("2026-09-30")}).Receivables
	fp(t, "unrealised loss", loss.Rows[0].Unrealised, -10500)
	i.FX.SaveManual(org, "u1", "AUD", "KES", 90, "")

	// Realised: 1,000 AUD paid in May at 85 vs 80 locked = +5,000; 50 AUD paid this month at today's 90 vs 100 locked = −500;
	// 300 AUD paid in January: no rate recorded for that month → not counted.
	re := tr.Realised
	if len(re.Rows) != 1 || re.Counted != 2 || re.Skipped != 1 {
		t.Fatalf("realised %+v", re)
	}
	r := re.Rows[0]
	if r.Currency != "AUD" || r.Payments != 2 || !near(r.Amount, 1050) || !near(r.AtInvoice, 85000) || !near(r.AtPayment, 89500) || !near(r.Gain, 4500) || r.Skipped != 1 {
		t.Fatalf("AUD realised %+v", r)
	}
	if !near(re.Gain, 4500) || !strings.Contains(re.Note, "1 payment not counted") {
		t.Fatalf("realised total %+v", re)
	}
	// Once January's rate is recorded the payment counts: 300 × 82 − 300 × 80 = +600.
	db.Create(&expenses.FxMonth{ID: uuid.NewString(), OrganizationID: org, FromCurrency: "AUD", ToCurrency: "KES", Month: "2026-01", Rate: 82, Source: "manual", Manual: true})
	re2 := i.TradeFor(org, Opts{Range: rng("2025-10-01", "2026-09-30"), Today: d("2026-09-30")}).Realised
	if re2.Skipped != 0 || !near(re2.Gain, 5100) {
		t.Fatalf("realised with January %+v", re2)
	}

	// Costs in foreign currencies: approved items in the range only.
	if len(tr.Costs) != 2 || tr.Costs[0].Currency != "AUD" || !near(tr.Costs[0].Amount, 400) || !near(tr.Costs[0].Home, 34200) || tr.Costs[0].Items != 2 || tr.Costs[0].Vendors[0].Name != "Synergy Wholesale" {
		t.Fatalf("costs %+v", tr.Costs)
	}
	if c := tr.Costs[1]; c.Currency != "EUR" || !near(c.Amount, 50) || c.Vendors[0].Name != "Google Workspace" {
		t.Fatalf("EUR costs %+v", c)
	}
	// Natural hedge: AUD 1,700 in vs 400 out → 23.5% covered, 1,300 exposed (117,000 KES; 5% = 5,850).
	if len(tr.Hedge) != 3 {
		t.Fatalf("hedge %+v", tr.Hedge)
	}
	h := tr.Hedge[0]
	if h.Currency != "AUD" || !near(h.In, 1700) || !near(h.Out, 400) || !near(h.Net, 1300) || h.Rate != 90 {
		t.Fatalf("AUD hedge %+v", h)
	}
	fp(t, "cover", h.CoverPct, 23.5)
	fp(t, "net home", h.NetHome, 117000)
	fp(t, "5% move", h.Move5Home, 5850)
	if !strings.Contains(h.Summary, "AUD 1,700 in vs AUD 400 out") || !strings.Contains(h.Summary, "23.5%") || !strings.Contains(h.Summary, "KES 5,850") {
		t.Fatalf("summary %q", h.Summary)
	}
	if u := tr.Hedge[1]; u.Currency != "USD" || !near(u.Net, 100) || u.CoverPct == nil || *u.CoverPct != 0 || !strings.Contains(u.Summary, "all of it is exposed") {
		t.Fatalf("USD hedge %+v", u)
	}
	if e := tr.Hedge[2]; e.Currency != "EUR" || !near(e.Net, -50) || e.CoverPct != nil || !near(*e.Move5Home, 375) || !strings.Contains(e.Summary, "no EUR income") {
		t.Fatalf("EUR hedge %+v", e)
	}
}

func TestTradeDocsAndCSV(t *testing.T) {
	i, _ := tradeFixtures(t)
	o := Opts{Range: rng("2025-10-01", "2026-09-30"), Today: d("2026-09-30")}
	au := i.TradeDocs(org, o, TradeDocsFilter{Country: "au"})
	sum := 0.0
	for _, x := range au {
		sum += x.NetHome
	}
	if len(au) != 5 || !near(sum, 140000) {
		t.Fatalf("AU documents n=%d sum=%v", len(au), sum)
	}
	if x := i.TradeDocs(org, o, TradeDocsFilter{Country: "export"}); len(x) != 6 {
		t.Fatalf("export documents %d", len(x))
	}
	if x := i.TradeDocs(org, o, TradeDocsFilter{Country: "domestic"}); len(x) != 3 {
		t.Fatalf("domestic documents %d", len(x))
	}
	open := i.TradeDocs(org, o, TradeDocsFilter{Currency: "AUD", Open: true})
	if len(open) != 3 {
		t.Fatalf("open AUD %d", len(open))
	}
	var un float64
	basis := map[string]int{}
	for _, x := range open {
		un += *x.Unrealised
		basis[x.Basis]++
	}
	if !near(un, 4500) || basis["currency"] != 1 || basis["client"] != 2 {
		t.Fatalf("open AUD unrealised %v basis %v", un, basis)
	}
	tr := i.TradeFor(org, o)
	for _, ds := range TradeDatasets {
		if ds.Key == "markets" {
			continue
		}
		b := &strings.Builder{}
		if err := WriteTradeCSV(b, ds.Key, tr, au, nil); err != nil {
			t.Fatalf("%s: %v", ds.Key, err)
		}
		rows, err := csv.NewReader(strings.NewReader(b.String())).ReadAll()
		if err != nil || len(rows) < 2 {
			t.Fatalf("%s: %v rows=%d", ds.Key, err, len(rows))
		}
		for _, r := range rows[1:] {
			if len(r) != len(rows[0]) {
				t.Fatalf("%s: ragged row", ds.Key)
			}
		}
		if ds.Key == "receivables" && (rows[0][7] != "unrealised_fx_kes" || rows[1][7] != "4500") {
			t.Fatalf("receivables csv %v", rows[:2])
		}
	}
	if WriteTradeCSV(&strings.Builder{}, "nope", tr, nil, nil) == nil || TradeDataset("nope") {
		t.Fatal("unknown dataset accepted")
	}
}

// ---------- target markets ----------

func TestVolatilityAndScore(t *testing.T) {
	// Month-to-month changes +10%, −10%, +10% → mean 3.33, sample std dev √(266.67 / 2) = 11.547.
	v, ok := MonthlyVolatility([]float64{100, 110, 99, 108.9})
	if !ok || math.Abs(v-11.547) > 0.001 {
		t.Fatalf("monthly volatility %v %v", v, ok)
	}
	if _, ok := MonthlyVolatility([]float64{100, 110, 99}); ok {
		t.Fatal("three rates are not enough")
	}
	a, ok := AnnualVolatility([]float64{100, 110, 99, 108.9})
	if !ok || math.Abs(a-11.547/math.Sqrt(12)) > 0.001 {
		t.Fatalf("annual volatility %v", a)
	}
	mv := func(v float64) *MarketValue { return &MarketValue{Value: v, Year: 2024} }
	rows := []MarketRow{
		{Country: "KE", Name: "Home", Currency: "KES", Revenue: 1000, SharePct: 99, Clients: 5, FXVol: fptr(0), FXBasis: "home",
			Indicators: map[string]*MarketValue{"NY.GDP.PCAP.CD": mv(1000), "IT.NET.USER.ZS": mv(20), "IC.BUS.NREG": mv(100), "NE.TRD.GNFS.ZS": mv(30)}},
		{Country: "AU", Name: "Rich", Currency: "AUD", Revenue: 10, SharePct: 1, Clients: 1, FXVol: fptr(2), FXBasis: "monthly",
			Indicators: map[string]*MarketValue{"NY.GDP.PCAP.CD": mv(100000), "IT.NET.USER.ZS": mv(100), "IC.BUS.NREG": mv(10000), "NE.TRD.GNFS.ZS": mv(60)}},
		{Country: "UG", Name: "Sparse", Currency: "UGX", FXVol: fptr(4), FXBasis: "annual",
			Indicators: map[string]*MarketValue{"NY.GDP.PCAP.CD": mv(10000), "IT.NET.USER.ZS": mv(60), "IC.BUS.NREG": nil}},
	}
	ScoreMarkets(rows, DefaultWeights(), "KE")
	// Home: only currency stability (15) and foothold (20) score → 35.
	// Rich: 25 + 15 + 15 + 10 + 15 × 0.5 + 20 × ln(11)/ln(1001) (0.347) = 79.4.
	// Sparse: 4 factors (weights 25 + 15 + 15 + 20 = 75): (25 × 0.5 + 15 × 0.5) ÷ 75 = 26.7.
	if rows[0].Score != 35 || rows[1].Score != 79.4 || rows[2].Score != 26.7 {
		t.Fatalf("scores %v %v %v", rows[0].Score, rows[1].Score, rows[2].Score)
	}
	if rows[1].Rank != 1 || rows[0].Rank != 2 || rows[2].Rank != 3 || !rows[0].Home || rows[2].Factors != 4 || rows[1].Factors != 6 {
		t.Fatalf("ranks / factors %+v", rows)
	}
	var pts float64
	for _, p := range rows[1].Parts {
		pts += p.Points
	}
	if math.Abs(pts-rows[1].Score) > 0.11 {
		t.Fatalf("parts %v do not add up to the score %v", pts, rows[1].Score)
	}
	if p := rows[2].Parts[2]; p.Key != "new_business" || p.Norm != nil || p.Points != 0 || p.Raw != "no data" {
		t.Fatalf("missing factor %+v", p)
	}
	if !strings.Contains(rows[1].Note, "You already bill") || !strings.Contains(rows[2].Note, "No clients there yet") || !strings.Contains(rows[2].Note, "4 of 6 factors") || !strings.HasPrefix(rows[0].Note, "Your home market") {
		t.Fatalf("notes: %q | %q | %q", rows[0].Note, rows[1].Note, rows[2].Note)
	}
	// Weights change the ranking: only "foothold" → home first.
	ScoreMarkets(rows, map[string]float64{"foothold": 10}, "KE")
	if rows[0].Rank != 1 || rows[0].Score != 100 || rows[2].Score != 0 {
		t.Fatalf("foothold only: %+v", rows)
	}
}

// fakeTradeWB answers the trade indicators for KEN, AUS and UGA.
func fakeTradeWB(t *testing.T) *httptest.Server {
	vals := map[string]map[string]float64{
		"NY.GDP.PCAP.CD": {"KEN": 2000, "AUS": 64000, "UGA": 1000},
		"IT.NET.USER.ZS": {"KEN": 35, "AUS": 96, "UGA": 10},
		"IC.BUS.NREG":    {"KEN": 60000, "AUS": 300000},
		"NE.TRD.GNFS.ZS": {"KEN": 30, "AUS": 45, "UGA": 40},
		"BX.GSR.NFSV.CD": {"KEN": 8e9, "AUS": 84e9, "UGA": 2e9},
		"BX.GSR.CCIS.CD": {"KEN": 8.5e8, "AUS": 3.1e9, "UGA": 4.8e7},
		"BX.GSR.CCIS.ZS": {"KEN": 10.6, "AUS": 3.4, "UGA": 1.9},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/country/"), "/indicator/")
		if len(parts) != 2 {
			w.WriteHeader(404)
			return
		}
		code := parts[1]
		rows := []map[string]any{}
		for _, c := range strings.Split(parts[0], ";") {
			for y := 2025; y >= 2020; y-- {
				var v any
				switch {
				case code == "PA.NUS.FCRF":
					// KES per USD constant 130; UGX per USD 3,700 growing 5% then falling 5% alternately → cross rate moves.
					base := map[string]float64{"KEN": 130, "AUS": 1.5, "UGA": 3700}[c]
					if c == "UGA" && y%2 == 0 {
						base *= 1.05
					}
					if base > 0 {
						v = base
					}
				case vals[code] != nil:
					if x, ok := vals[code][c]; ok && y <= 2024 {
						v = x
					}
				default:
					w.Write([]byte(`[{"message":[{"id":"120","key":"Invalid value","value":"The provided parameter value is not valid"}]}]`))
					return
				}
				rows = append(rows, map[string]any{"indicator": map[string]string{"id": code, "value": code}, "country": map[string]string{"id": c[:2], "value": c}, "countryiso3code": c, "date": fmt.Sprint(y), "value": v})
			}
		}
		json.NewEncoder(w).Encode([]any{map[string]any{"page": 1, "pages": 1, "per_page": 1000, "total": len(rows), "lastupdated": "2026-07-13"}, rows})
	}))
}

func TestTargetMarkets(t *testing.T) {
	i, _ := tradeFixtures(t)
	srv := fakeTradeWB(t)
	defer srv.Close()
	i.WB = &WorldBank{BaseURL: srv.URL, Client: srv.Client(), TTL: time.Hour, Now: i.Now}
	ctx := context.Background()
	if err := i.SaveMarketConfig(org, []string{"ke", "AUS", "UG"}, nil); err != nil {
		t.Fatal(err)
	}
	m := i.MarketsFor(ctx, org, false)
	if len(m.Rows) != 3 || !m.Custom || len(m.Errors) != 0 || m.HomeCountry != "KE" {
		t.Fatalf("markets %+v errors %v", m.Countries, m.Errors)
	}
	by := map[string]MarketRow{}
	for _, r := range m.Rows {
		by[r.Country] = r
	}
	au, ke, ug := by["AU"], by["KE"], by["UG"]
	// Revenue of the last 12 months, same figures as the trade view.
	if !near(au.Revenue, 140000) || au.Clients != 3 || !near(ke.Revenue, 160000) || !near(ug.Revenue, 13000) {
		t.Fatalf("revenue AU %v KE %v UG %v", au.Revenue, ke.Revenue, ug.Revenue)
	}
	if v := au.Indicators["NY.GDP.PCAP.CD"]; v == nil || v.Value != 64000 || v.Year != 2024 {
		t.Fatalf("AU GDP per capita %+v", v)
	}
	if ug.Indicators["IC.BUS.NREG"] != nil || ug.Factors != 5 {
		t.Fatalf("UG missing indicator: %+v factors %d", ug.Indicators["IC.BUS.NREG"], ug.Factors)
	}
	// AUD: monthly rates 85, 86, 84, 88 → changes +1.176%, −2.326%, +4.762% → std dev 3.54.
	if au.FXBasis != "monthly" || au.FXPoints != 4 || au.FXVol == nil || math.Abs(*au.FXVol-3.54) > 0.011 {
		t.Fatalf("AUD volatility %+v %s", au.FXVol, au.FXBasis)
	}
	if ke.FXBasis != "home" || *ke.FXVol != 0 || !ke.Home {
		t.Fatalf("home row %+v", ke)
	}
	// UGX: yearly averages only (no month rates) → estimated from the World Bank series.
	if ug.FXBasis != "annual" || ug.FXVol == nil || *ug.FXVol <= 0 {
		t.Fatalf("UGX volatility %+v %s", ug.FXVol, ug.FXBasis)
	}
	if au.Rank != 1 || au.Score <= ke.Score || ke.Score <= ug.Score || au.Score > 100 {
		t.Fatalf("scores AU %v KE %v UG %v", au.Score, ke.Score, ug.Score)
	}
	if !strings.Contains(au.Note, "You already bill 140,000 there") || !strings.Contains(au.Note, "quote in AUD") {
		t.Fatalf("AU note %q", au.Note)
	}
	// Validation and reset.
	if i.SaveMarketConfig(org, []string{"KE"}, nil) == nil || i.SaveMarketConfig(org, []string{"KE", "XX"}, nil) == nil ||
		i.SaveMarketConfig(org, []string{"KE", "AU"}, map[string]float64{"foothold": -1}) == nil ||
		i.SaveMarketConfig(org, []string{"KE", "AU"}, map[string]float64{"purchasing_power": 0, "digital": 0, "new_business": 0, "openness": 0, "fx_stability": 0, "foothold": 0}) == nil {
		t.Fatal("invalid market configuration accepted")
	}
	if err := i.SaveMarketConfig(org, []string{"KE", "AU", "UG"}, map[string]float64{"foothold": 60}); err != nil {
		t.Fatal(err)
	}
	if w := i.MarketsFor(ctx, org, false).Weights; w["foothold"] != 60 || w["digital"] != 15 {
		t.Fatalf("weights %v", w)
	}
	if err := i.ResetMarketConfig(org); err != nil {
		t.Fatal(err)
	}
	def := i.MarketsFor(ctx, org, false)
	if def.Custom || len(def.Rows) != len(DefaultTargetMarkets) || def.Weights["foothold"] != 20 {
		t.Fatalf("defaults after reset: custom=%v rows=%d", def.Custom, len(def.Rows))
	}
	// Countries the World Bank has nothing for still appear, scored on what is known.
	gb := MarketRow{}
	for _, r := range def.Rows {
		if r.Country == "GB" {
			gb = r
		}
	}
	if gb.Country != "GB" || gb.Indicators["NY.GDP.PCAP.CD"] != nil || gb.Factors != 1 || !strings.Contains(gb.Note, "1 of 6 factors") {
		t.Fatalf("GB %+v", gb)
	}
	b := &strings.Builder{}
	if err := WriteTradeCSV(b, "markets", nil, nil, def); err != nil || !strings.Contains(b.String(), "NY.GDP.PCAP.CD") || strings.Count(b.String(), "\n") != len(def.Rows)+1 {
		t.Fatalf("markets csv: %v\n%s", err, b.String())
	}
}
