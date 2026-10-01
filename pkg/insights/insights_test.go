package insights

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

// ---------- pure helpers ----------

func near(a, b float64) bool { return math.Abs(a-b) < 0.051 }

func TestHHIAndLevels(t *testing.T) {
	if h := HHI([]float64{3800, 1000, 500, 3000}); h != 3584 {
		t.Fatalf("HHI = %v, want 3584", h)
	}
	if HHI([]float64{1}) != 10000 || HHI(nil) != 0 || HHI([]float64{0, -5}) != 0 {
		t.Fatal("HHI edge cases")
	}
	if concentrationLevel(1499) != "low" || concentrationLevel(1500) != "moderate" || concentrationLevel(2600) != "high" {
		t.Fatal("levels")
	}
}

func TestClassifyAndLineSQL(t *testing.T) {
	r := DefaultRules()
	cases := map[string]string{
		"Web hosting — example-client.co.ke (12 months)":                    LineHosting,
		"Website design & development — deposit 50% (example-client.co.ke)": LineWebDev,
		"Website maintenance & support — quarter":                           LineMaintenance,
		"Google Workspace Business Starter — 3 users":                       LineWorkspace,
		"Domain registration renewal — example.co.ke":                       LineDomains,
		"DASYIN ERP software licence (12 months)":                           LineSoftware,
		"Consulting — data analysis & reporting (hours)":                    LineConsulting,
		"Printer paper": LineOther,
	}
	for d, want := range cases {
		if got := Classify(r, d); got != want {
			t.Errorf("Classify(%q) = %s, want %s", d, got, want)
		}
	}
	sql := LineSQL([]ServiceRule{{Line: LineHosting, Priority: 1, Keywords: []string{"50%_off", "o'brien"}}}, "x")
	if !strings.Contains(sql, `'%50\%\_off%'`) || !strings.Contains(sql, `'%o''brien%'`) {
		t.Fatalf("LineSQL escaping: %s", sql)
	}
	if LineSQL(nil, "x") != "'other'" {
		t.Fatal("empty rules")
	}
}

func TestMonthsCoveredAndBehaviour(t *testing.T) {
	for d, want := range map[string]float64{"Hosting (12 months)": 12, "Hosting 6 months": 6, "Hosting 2 years": 24, "Hosting monthly": 1, "Hosting quarterly": 3, "Annual hosting": 12, "Hosting": 12} {
		if got := MonthsCovered(d); got != want {
			t.Errorf("MonthsCovered(%q) = %v, want %v", d, got, want)
		}
	}
	if Behaviour(0, 0) != "new" || Behaviour(3, -2) != "prompt" || Behaviour(3, 10) != "ok" || Behaviour(3, 30) != "slow" || Behaviour(3, 60) != "very_slow" {
		t.Fatal("Behaviour")
	}
}

func TestParseSeriesCSV(t *testing.T) {
	in := "\xef\xbb\xbfPeriod,Registrations\n2023,\"98,000\"\n2024-Q1,25000\nQ2 2024,26000\nMar 2024,9000\n2024-13,1\nbad,2\n2023,5\n"
	pts, warn, err := ParseSeriesCSV(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 4 || pts[0].Period != "2023" || pts[0].Value != 98000 || pts[1].Period != "2024-03" || pts[2].Period != "2024-Q1" || pts[3].Period != "2024-Q2" {
		t.Fatalf("points %+v", pts)
	}
	if len(warn) != 3 {
		t.Fatalf("warnings %v", warn)
	}
	if _, _, err := ParseSeriesCSV(strings.NewReader("a,b\nx,y\n")); err == nil {
		t.Fatal("expected error for no data")
	}
	pts, _, _ = ParseSeriesCSV(strings.NewReader("year;value\n2020;1,5\n"))
	if len(pts) != 1 || pts[0].Value != 15 {
		t.Fatalf("semicolon CSV: %+v", pts)
	}
}

// ---------- World Bank client ----------

type fakeWB struct {
	calls int32
	fail  atomic.Bool
}

func (f *fakeWB) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.calls, 1)
		if f.fail.Load() {
			w.WriteHeader(503)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/v2/country/") {
			w.WriteHeader(404)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/country/"), "/indicator/")
		code := parts[1]
		if code == "BAD.CODE" {
			w.Write([]byte(`[{"message":[{"id":"120","key":"Invalid value","value":"The provided parameter value is not valid"}]}]`))
			return
		}
		if code == "EMPTY.CODE" {
			w.Write([]byte(`[{"page":1,"pages":0,"per_page":50,"total":0,"lastupdated":"2026-07-13"},null]`))
			return
		}
		if r.URL.Query().Get("format") != "json" || r.URL.Query().Get("date") != "2019:2024" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		// 2 countries × 6 years, per_page from the query, latest year null.
		rows := []map[string]any{}
		for _, c := range strings.Split(parts[0], ";") {
			for y := 2024; y >= 2019; y-- {
				var v any = float64(y-2000) + map[string]float64{"KEN": 0.5, "AUS": 70}[c]
				if y == 2024 {
					v = nil
				}
				rows = append(rows, map[string]any{"indicator": map[string]string{"id": code, "value": "Test indicator"}, "country": map[string]string{"id": c[:2], "value": c + " name"},
					"countryiso3code": c, "date": fmt.Sprint(y), "value": v})
			}
		}
		per := 5
		fmt.Sscanf(r.URL.Query().Get("per_page"), "%d", &per)
		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		pages := (len(rows) + per - 1) / per
		end := page * per
		if end > len(rows) {
			end = len(rows)
		}
		meta := map[string]any{"page": page, "pages": pages, "per_page": per, "total": len(rows), "lastupdated": "2026-07-13"}
		json.NewEncoder(w).Encode([]any{meta, rows[(page-1)*per : end]})
	}
}

func TestWorldBankPaginationNullsCaching(t *testing.T) {
	f := &fakeWB{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	wb := &WorldBank{BaseURL: srv.URL, Client: srv.Client(), TTL: 24 * time.Hour, PerPage: 5, Now: func() time.Time { return now }}
	r := wb.Fetch(context.Background(), "IT.NET.USER.ZS", []string{"KEN", "AUS"}, 2019, 2024, false)
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if got := atomic.LoadInt32(&f.calls); got != 3 { // 12 rows / 5 per page
		t.Fatalf("calls = %d, want 3 pages", got)
	}
	ke := r.Series["KEN"]
	if ke == nil || len(ke.Points) != 6 || ke.Points[0].Year != 2019 || ke.Points[5].Value != nil || r.LastUpdated != "2026-07-13" {
		t.Fatalf("KEN series %+v", ke)
	}
	st := Stat(ke, "pct")
	if st.LatestYear != 2023 || !near(*st.Latest, 23.5) || st.PrevYear != 2022 || !near(*st.Change, 1) || st.ChangeUnit != "pts" || !near(*st.FiveYear, 4) {
		t.Fatalf("stat %+v", st)
	}
	stn := Stat(r.Series["AUS"], "number")
	if stn.ChangeUnit != "%" || !near(*stn.Change, r1((93.0-92.0)/92.0*100)) || stn.CAGR == nil {
		t.Fatalf("number stat %+v", stn)
	}
	// Cached: no new call within 24 h.
	r2 := wb.Fetch(context.Background(), "IT.NET.USER.ZS", []string{"AUS", "KEN"}, 2019, 2024, false)
	if !r2.Cached || atomic.LoadInt32(&f.calls) != 3 {
		t.Fatalf("expected cache hit (calls %d)", f.calls)
	}
	// Expired + API down → stale copy with an explanation.
	now = now.Add(25 * time.Hour)
	f.fail.Store(true)
	r3 := wb.Fetch(context.Background(), "IT.NET.USER.ZS", []string{"KEN", "AUS"}, 2019, 2024, false)
	if !r3.Stale || r3.Series["KEN"] == nil || !strings.Contains(r3.Error, "HTTP 503") {
		t.Fatalf("stale fallback: %+v", r3)
	}
	f.fail.Store(false)
	// API error object and empty data.
	if e := wb.Fetch(context.Background(), "BAD.CODE", []string{"KEN"}, 2019, 2024, false); !strings.Contains(e.Error, "Invalid value") {
		t.Fatalf("error object: %q", e.Error)
	}
	if e := wb.Fetch(context.Background(), "EMPTY.CODE", []string{"KEN"}, 2019, 2024, false); e.Error != "" || len(e.Series) != 0 {
		t.Fatalf("empty: %+v", e)
	}
	if e := wb.Fetch(context.Background(), "x; DROP", []string{"KEN"}, 2019, 2024, false); !strings.Contains(e.Error, "invalid indicator") {
		t.Fatalf("validation: %q", e.Error)
	}
}

func TestNotesAndFX(t *testing.T) {
	ser := map[string]*WBSeries{
		"KEN": {Country: "KEN", Points: []WBPoint{{2019, fptr(100)}, {2024, fptr(130)}}},
		"AUS": {Country: "AUS", Points: []WBPoint{{2019, fptr(1.4)}, {2024, fptr(1.5)}}},
	}
	pts := fxAnnual(ser, "AUD", "KES")
	if len(pts) != 2 || !near(*pts[0].Value, 71.43) || !near(*pts[1].Value, 86.67) {
		t.Fatalf("AUD/KES %+v", pts)
	}
	card := &IndicatorCard{Indicator: DefaultIndicators[2], Countries: []CountryStat{Stat(&WBSeries{Country: "KEN", CountryName: "Kenya", Points: []WBPoint{{2023, fptr(50000)}, {2024, fptr(55000)}}}, "number")}}
	if n := Note(card); !strings.Contains(n, "55,000 new businesses") || !strings.Contains(n, "+10.0%") {
		t.Fatalf("note: %s", n)
	}
}

// ---------- SQL analytics on Postgres fixtures ----------

// testDB opens TEST_DATABASE_URL in a fresh schema with the tables used here.
func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	schema := "ins_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	admin.Exec("CREATE SCHEMA " + schema)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := db.DB(); err == nil {
			d.Close()
		}
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if d, err := admin.DB(); err == nil {
			d.Close()
		}
	})
	if err := db.AutoMigrate(&invoicing.Invoice{}, &invoicing.InvoiceItem{}, &invoicing.Payment{}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) PRIMARY KEY, name text, country_code varchar(2), currency varchar(3), timezone text, region_source text, address_country text)`,
		`CREATE TABLE customers (id varchar(255) PRIMARY KEY, organization_id varchar(255), first_name text, last_name text, company_name text, email text, country_code text, currency text, source text, created_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE expense_items (id varchar(64) PRIMARY KEY, organization_id varchar(255), status text, category text, invoice_date date, home_net numeric)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

const org = "org-1"

type fx struct {
	t  *testing.T
	db *gorm.DB
	n  int
}

func d(s string) time.Time { t, _ := time.Parse(dayFmt, s); return t }

// inv adds a document; lines are (description, line_total) and net = sum − tax.
func (f *fx) inv(docType, status, cust, cur string, rate float64, issue string, net, tax float64, lines ...any) string {
	f.n++
	id := uuid.NewString()
	cid := cust
	i := invoicing.Invoice{ID: id, OrganizationID: org, DocType: docType, Number: fmt.Sprintf("D-%03d", f.n), Status: status, CustomerID: &cid, ClientName: "Client " + cust,
		IssueDate: d(issue), DueDate: d(issue).AddDate(0, 0, 14), Currency: cur, BaseCurrency: "KES", ExchangeRate: rate, Subtotal: net, TaxTotal: tax, Total: net + tax,
		BalanceDue: net + tax, PublicToken: uuid.NewString()}
	if err := f.db.Create(&i).Error; err != nil {
		f.t.Fatal(err)
	}
	for k := 0; k+1 < len(lines); k += 2 {
		f.db.Create(&invoicing.InvoiceItem{ID: uuid.NewString(), InvoiceID: id, Position: k, Description: lines[k].(string), LineTotal: lines[k+1].(float64)})
	}
	return id
}

func (f *fx) pay(id, date string, amount float64) {
	if err := f.db.Exec(`INSERT INTO billing_payments (id, invoice_id, organization_id, amount, date) VALUES (?, ?, ?, ?, ?)`, uuid.NewString(), id, org, amount, d(date)).Error; err != nil {
		f.t.Fatal(err)
	}
	f.db.Exec("UPDATE billing_invoices SET amount_paid = amount_paid + ?, balance_due = GREATEST(0, balance_due - ?), status = CASE WHEN balance_due - ? <= 0.005 THEN 'paid' ELSE 'partial' END WHERE id = ?", amount, amount, amount, id)
}

func fixtures(t *testing.T) (*Service, *gorm.DB) {
	db := testDB(t)
	db.Exec(`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES (?, 'Test', 'KE', 'KES', 'Africa/Nairobi', 'user')`, org)
	for _, c := range []string{"A", "B", "C", "D"} {
		db.Exec(`INSERT INTO customers (id, organization_id, first_name, last_name, company_name, created_at) VALUES (?, ?, 'C', ?, ?, now())`, c, org, c, "Company "+c)
	}
	f := &fx{t: t, db: db}
	a1 := f.inv("invoice", "sent", "A", "KES", 1, "2024-01-10", 1000, 160, "Website development", 696.0, "Web hosting example.com (12 months)", 464.0)
	f.pay(a1, "2024-01-20", 1160)
	a2 := f.inv("invoice", "sent", "A", "KES", 1, "2024-04-10", 1000, 160, "Website maintenance retainer", 1160.0)
	f.pay(a2, "2024-05-10", 1160)
	a3 := f.inv("invoice", "sent", "A", "KES", 1, "2025-02-10", 2000, 320, "Consulting — data analysis", 2320.0)
	f.pay(a3, "2025-02-15", 2320)
	b1 := f.inv("invoice", "sent", "B", "AUD", 0.01, "2024-02-05", 10, 0, "Google Workspace (12 months)", 10.0) // 10 AUD = 1,000 KES (locked)
	f.pay(b1, "2024-03-06", 10)
	c1 := f.inv("invoice", "sent", "C", "KES", 1, "2024-01-15", 500, 80, "Domain registration renewal", 580.0)
	f.pay(c1, "2024-01-16", 580)
	f.inv("invoice", "sent", "D", "KES", 1, "2025-06-01", 3000, 480, "Website design", 3480.0)
	f.inv("credit_note", "paid", "A", "KES", 1, "2025-03-01", 200, 0, "Goodwill credit", 200.0)
	f.inv("invoice", "draft", "A", "KES", 1, "2025-03-05", 9999, 0, "Draft", 9999.0)
	f.inv("invoice", "void", "B", "KES", 1, "2025-03-05", 8888, 0, "Void", 8888.0)
	// Quotes: won, lost, open.
	f.inv("quote", "converted", "A", "KES", 1, "2025-05-01", 5000, 0, "Phase 2", 5000.0)
	f.inv("quote", "declined", "B", "KES", 1, "2025-06-01", 4000, 0, "App", 4000.0)
	f.inv("quote", "sent", "C", "KES", 1, "2025-09-20", 6000, 0, "Redesign", 6000.0)
	db.Exec(`INSERT INTO expense_items VALUES ('e1', ?, 'approved', 'hosting', '2024-06-01', 300), ('e2', ?, 'approved', 'other', '2024-06-01', 50), ('e3', ?, 'pending', 'hosting', '2024-06-01', 999)`, org, org, org)
	s := New(db, nil, nil, nil, nil)
	s.Now = func() time.Time { return d("2025-09-30") }
	return s, db
}

func rng(from, to string) Range { return Range{From: d(from), To: d(to)} }

func TestRevenueBreakdownSQL(t *testing.T) {
	s, _ := fixtures(t)
	o := Opts{Range: rng("2024-01-01", "2025-12-31")}
	r := s.RevenueBreakdown(org, o)
	if !near(r.Total, 8300) || r.Clients != 4 || r.Docs != 7 {
		t.Fatalf("total %v clients %d docs %d", r.Total, r.Clients, r.Docs)
	}
	if len(r.Conv.Rates) != 1 || r.Conv.Rates[0].Currency != "AUD" || r.Conv.Rates[0].Locked != 1 || !strings.Contains(r.Conv.Note, "locked") {
		t.Fatalf("conversion %+v", r.Conv)
	}
	got := map[string]float64{}
	for _, c := range r.ByClient {
		got[c.Key] = c.Revenue
	}
	if !near(got["A"], 3800) || !near(got["B"], 1000) || !near(got["C"], 500) || !near(got["D"], 3000) || r.ByClient[0].Key != "A" || !near(r.ByClient[0].SharePct, 45.8) {
		t.Fatalf("by client %+v", r.ByClient)
	}
	lines := map[string]float64{}
	var sum float64
	for _, l := range r.ByLine {
		lines[l.Key] = l.Revenue
		sum += l.Revenue
	}
	if !near(sum, 8300) || !near(lines[LineWebDev], 600+3000) || !near(lines[LineHosting], 400) || !near(lines[LineWorkspace], 1000) || !near(lines[LineOther], -200) {
		t.Fatalf("by line %+v", lines)
	}
	// YoY with LAG(12): Jan 2025 vs Jan 2024.
	for _, m := range r.Months {
		if m.Month == "2025-01" && (m.PrevYear == nil || !near(*m.PrevYear, 1500)) {
			t.Fatalf("2025-01 prev year %+v", m)
		}
		if m.Month == "2025-02" && (m.YoYPct == nil || !near(*m.YoYPct, r1((2000.0-1000)/1000*100))) {
			t.Fatalf("2025-02 yoy %+v", m)
		}
	}
	// Drill-down: client A, hosting line.
	dr := s.RevenueBreakdown(org, Opts{Range: o.Range, Client: "A", Line: LineHosting})
	if len(dr.Lines) != 1 || !near(dr.Lines[0].AmountHome, 400) || !near(dr.Total, 400) {
		t.Fatalf("drill %+v", dr.Lines)
	}
	// Currency filter: only AUD, in AUD.
	au := s.RevenueBreakdown(org, Opts{Range: o.Range, Currency: "AUD"})
	if !near(au.Total, 10) || au.Conv.Currency != "AUD" {
		t.Fatalf("AUD filter %v %s", au.Total, au.Conv.Currency)
	}
}

func TestCohortsConcentrationChurnCLV(t *testing.T) {
	s, _ := fixtures(t)
	c := s.ClientAnalytics(org, Opts{Range: rng("2024-01-01", "2025-09-30"), Grain: "quarter"})
	if len(c.Cohorts) != 2 {
		t.Fatalf("cohorts %+v", c.Cohorts)
	}
	q1 := c.Cohorts[0]
	if q1.Cohort != "2024-Q1" || q1.Size != 3 || q1.Active[1] != 1 || q1.Active[4] != 1 || !near(q1.Retention[1], 33.3) || q1.Active[2] != 0 {
		t.Fatalf("2024-Q1 cohort %+v", q1)
	}
	// 2024-Q1 → 2025-Q3 is offset 6; offset 7 has not happened yet.
	if q1.Active[7] != -1 || q1.Active[6] != 0 {
		t.Fatalf("future periods %+v", q1.Active)
	}
	if c.Cohorts[1].Cohort != "2025-Q2" || c.Cohorts[1].Size != 1 {
		t.Fatalf("2025-Q2 cohort %+v", c.Cohorts[1])
	}
	k := c.Concentration
	if k.Clients != 4 || k.HHI != 3584 || !near(k.Top1Pct, 45.8) || k.Level != "high" || len(k.Lorenz) != 4 || !near(k.Lorenz[3], 100) || k.Top5Pct != 100 {
		t.Fatalf("concentration %+v", k)
	}
	ch := c.Churn
	if ch.Active != 1 || ch.AtRisk != 1 || ch.Lapsed != 2 || ch.RatePct == nil || !near(*ch.RatePct, 66.7) {
		t.Fatalf("churn %+v rate %v", ch, ch.RatePct)
	}
	st := map[string]string{}
	for _, x := range ch.Clients {
		st[x.Key] = x.Status
	}
	if st["A"] != "at_risk" || st["B"] != "lapsed" || st["C"] != "lapsed" {
		t.Fatalf("churn statuses %v", st)
	}
	var y2025 *YearChurn
	for i := range ch.ByYear {
		if ch.ByYear[i].Year == 2025 {
			y2025 = &ch.ByYear[i]
		}
	}
	if y2025 == nil || y2025.Start != 3 || y2025.Retained != 1 || y2025.New != 1 {
		t.Fatalf("2025 churn %+v", ch.ByYear)
	}
	v := c.CLV
	if !near(v.ExpectedYears, 1.5) || !near(v.MarginPct, r1((8300.0-300)/8300*100)) {
		t.Fatalf("clv %+v", v)
	}
	rows := map[string]CLVRow{}
	for _, x := range v.Top {
		rows[x.Key] = x
	}
	// A: 3,800 over 15 months (Jan 2024 – Mar 2025) → 3,040 a year, at risk; D: under a year → 3,000.
	if a := rows["A"]; a.TenureMo != 15 || !near(a.AnnualValue, 3040) || a.Status != "at_risk" || !near(a.CLV, r2(3040*v.MarginPct/100*1.5)) || v.Top[0].Key != "A" {
		t.Fatalf("clv A %+v", a)
	}
	if dd := rows["D"]; !near(dd.AnnualValue, 3000) || dd.Status != "active" || rows["B"].CLV != 0 {
		t.Fatalf("clv D %+v B %+v", dd, rows["B"])
	}
}

func TestPaymentsDSO(t *testing.T) {
	s, _ := fixtures(t)
	p := s.PaymentAnalytics(org, Opts{Range: rng("2024-01-01", "2025-06-30"), Today: d("2025-06-30")})
	if p.Paid != 5 || !near(p.AvgDays, 14.2) || !near(p.OnTimePct, 60) || !near(p.MedianDay, 10) {
		t.Fatalf("payments avg %v on time %v median %v paid %d", p.AvgDays, p.OnTimePct, p.MedianDay, p.Paid)
	}
	var jun, jan *DSOPoint
	for i := range p.Months {
		switch p.Months[i].Month {
		case "2025-06":
			jun = &p.Months[i]
		case "2024-01":
			jan = &p.Months[i]
		}
	}
	if jun == nil || !near(jun.Receivables, 3480) || !near(jun.Sales90, 3480) || !near(jun.DSO, 90) {
		t.Fatalf("June DSO %+v", jun)
	}
	// End of Jan 2024: A1 and C1 paid; nothing open.
	if jan == nil || !near(jan.Receivables, 0) || !near(jan.Sales90, 1740) {
		t.Fatalf("Jan DSO %+v", jan)
	}
	byC := map[string]PayClient{}
	for _, c := range p.Clients {
		byC[c.Key] = c
	}
	if d := byC["D"]; !near(d.Overdue, 3480) || d.OverdueDays != 15 || d.Behaviour != "new" {
		t.Fatalf("client D %+v", d)
	}
	if b := byC["B"]; !near(b.AvgDays, 30) || !near(b.AvgLate, 16) || b.Behaviour != "slow" {
		t.Fatalf("client B %+v", b)
	}
}

func TestSeasonalityAndServiceLines(t *testing.T) {
	s, _ := fixtures(t)
	se := s.SeasonalityAnalytics(org, Opts{Range: rng("2024-01-01", "2025-12-31")})
	if len(se.Years) != 2 || se.Matrix[0][0] == nil || !near(*se.Matrix[0][0], 1500) || se.Peak != "February" {
		t.Fatalf("seasonality %+v peak %s", se.Matrix, se.Peak)
	}
	sl := s.ServiceLines(org, Opts{Range: rng("2024-01-01", "2024-12-31")})
	var host *LineMargin
	for i := range sl.Lines {
		if sl.Lines[i].Line == LineHosting {
			host = &sl.Lines[i]
		}
	}
	if host == nil || !near(host.Revenue, 400) || !near(host.Cost, 300) || !near(host.Margin, 100) || !near(sl.Overhead, 50) || !near(sl.Revenue, 3500) {
		t.Fatalf("service lines %+v overhead %v", sl.Lines, sl.Overhead)
	}
	// Editing the mapping re-classifies: "maintenance retainer" → consulting.
	if _, err := s.SaveRules(org, []ServiceRule{{Line: LineConsulting, Keywords: []string{"retainer"}}}); err != nil {
		t.Fatal(err)
	}
	sl = s.ServiceLines(org, Opts{Range: rng("2024-01-01", "2024-12-31")})
	found := false
	for _, l := range sl.Lines {
		if l.Line == LineConsulting && near(l.Revenue, 1000) {
			found = true
		}
	}
	if !found || sl.Defaults {
		t.Fatalf("custom mapping not applied: %+v", sl.Lines)
	}
	if _, err := s.SaveRules(org, []ServiceRule{{Line: "nope"}}); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestOpportunityRules(t *testing.T) {
	s, _ := fixtures(t)
	o := s.OpportunitiesFor(org, d("2025-06-30"))
	by := map[string]Opportunity{}
	for _, x := range o.Items {
		by[x.Key] = x
	}
	col, okc := by["collection"]
	if !okc || col.Count != 1 || !near(col.Value, 3480) || col.Items[0].CustomerID != "D" {
		t.Fatalf("collection %+v", col)
	}
	// Lapsed with ≥ 2 invoices: none (B and C bought once); A is quiet for 4.5 months → still active.
	if _, ok := by["winback"]; ok {
		t.Fatalf("unexpected winback %+v", by["winback"])
	}
	p := o.Pipeline
	if p.Won12m != 1 || p.Lost12m != 1 || p.ConversionPct == nil || *p.ConversionPct != 50 {
		t.Fatalf("pipeline %+v", p)
	}
	// Website clients without maintenance: D (web dev, no maintenance in 12 months); A has maintenance only in 2024 → also listed.
	mt := by["web_no_maintenance"]
	if mt.Count != 2 {
		t.Fatalf("maintenance %+v", mt)
	}
	// Concentration: last 12 months only A (2,000 − 200 credit) and D (3,000) → high.
	if r, ok := by["concentration"]; !ok || r.Severity != "high" || r.ValueKind != "at_risk" {
		t.Fatalf("concentration %+v", r)
	}
	for _, x := range o.Items {
		if x.ValueBasis == "" || x.Summary == "" {
			t.Fatalf("opportunity without basis/summary: %+v", x)
		}
	}
}

func TestDashboardSnapshot(t *testing.T) {
	s, db := fixtures(t)
	// A monthly recurring template (KES 1,000 net) and a quarterly one in AUD (30 AUD net → 3,000 KES / 3).
	nx := d("2025-07-01")
	for _, r := range []struct {
		cur      string
		rate     float64
		net      float64
		interval string
	}{{"KES", 1, 1000, "monthly"}, {"AUD", 0.01, 30, "quarterly"}} {
		id := uuid.NewString()
		c := "A"
		db.Create(&invoicing.Invoice{ID: id, OrganizationID: org, DocType: "invoice", Number: "R-" + r.interval, Status: "sent", CustomerID: &c, ClientName: "Client A", IssueDate: d("2025-06-01"),
			DueDate: d("2025-06-08"), Currency: r.cur, BaseCurrency: "KES", ExchangeRate: r.rate, Subtotal: r.net, Total: r.net, BalanceDue: 0, AmountPaid: r.net, IsRecurring: true,
			RecurrenceInterval: r.interval, NextRecurrenceDate: &nx, PublicToken: uuid.NewString()})
	}
	db.Exec("UPDATE billing_invoices SET status = 'paid' WHERE number LIKE 'R-%'")
	dash := s.DashboardFor(org, d("2025-06-20"), time.UTC)
	sn := dash.Snapshot
	if !near(sn.RevenueMTD, 3000+1000+3000) || !near(sn.Outstanding, 3480) || sn.OverdueN != 1 || !near(sn.Overdue, 3480) {
		t.Fatalf("snapshot %+v", sn)
	}
	if !near(sn.MRR, 2000) || sn.MRRTemplates != 2 || !near(sn.ARR, 24000) {
		t.Fatalf("MRR %+v", sn)
	}
	if dash.History != nil {
		t.Fatal("history without Zoho data")
	}
	cf := dash.Cash
	if len(cf.Weeks) != 13 || !near(cf.Overdue, 3480) || !near(cf.Recurring, 1000*3+3000) {
		t.Fatalf("cash forecast %+v", cf)
	}
	db.Exec("UPDATE billing_invoices SET source = 'zoho' WHERE number = 'D-001'")
	h := s.DashboardFor(org, d("2025-06-20"), time.UTC).History
	if h == nil || h.Imported["invoices"] != 1 || h.Years != 2 || len(h.ByYear) != 2 || h.TopClients[0].Key != "A" {
		t.Fatalf("history %+v", h)
	}
}

func TestExportCSV(t *testing.T) {
	s, _ := fixtures(t)
	var b strings.Builder
	n, err := s.WriteDataset(&b, org, "invoices", rng("2024-01-01", "2025-12-31"))
	if err != nil || n != 7 {
		t.Fatalf("invoices export n=%d err=%v", n, err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if lines[0] != strings.Join(DatasetByKey("invoices").Columns, ",") || len(lines) != 8 {
		t.Fatalf("header / rows: %q", lines[0])
	}
	b.Reset()
	if n, err := s.WriteDataset(&b, org, "invoice_lines", rng("2024-01-01", "2024-01-31")); err != nil || n != 3 || !strings.Contains(b.String(), ",hosting,") {
		t.Fatalf("lines export n=%d err=%v\n%s", n, err, b.String())
	}
	b.Reset()
	if n, err := s.WriteDataset(&b, org, "payments", rng("2024-01-01", "2025-12-31")); err != nil || n != 5 {
		t.Fatalf("payments n=%d err=%v", n, err)
	}
}
