package smallbiz

import (
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseMinor(t *testing.T) {
	cases := []struct {
		in   string
		dec  int
		want int64
		err  bool
	}{
		{"12.34", 2, 1234, false},
		{"1,234.5", 2, 123450, false},
		{"0.1", 2, 10, false},
		{".5", 2, 50, false},
		{"7", 2, 700, false},
		{"12.340", 2, 1234, false}, // trailing zeros are fine
		{"12.345", 2, 0, true},
		{"1500", 0, 1500, false}, // JPY / UGX
		{"1500.00", 0, 1500, false},
		{"1500.5", 0, 0, true},
		{"1.234", 3, 1234, false}, // KWD
		{"1.2", 3, 1200, false},
		{"-3.10", 2, -310, false},
		{"abc", 2, 0, true},
		{"", 2, 0, true},
		{"1.2.3", 2, 0, true},
		{"0.10000000000000000001", 2, 0, true},
	}
	for _, c := range cases {
		got, err := ParseMinor(c.in, c.dec)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("ParseMinor(%q,%d) = %d, %v; want %d, err=%v", c.in, c.dec, got, err, c.want, c.err)
		}
	}
	// no float drift: 0.1 + 0.2 as cents
	a, _ := ParseMinor("0.1", 2)
	b, _ := ParseMinor("0.2", 2)
	if a+b != 30 {
		t.Fatalf("0.1+0.2 = %d cents", a+b)
	}
}

func TestFormatMinor(t *testing.T) {
	if s := FormatMinor(123450, 2); s != "1234.50" {
		t.Fatal(s)
	}
	if s := FormatMinor(-5, 2); s != "-0.05" {
		t.Fatal(s)
	}
	if s := FormatMinor(1500, 0); s != "1500" {
		t.Fatal(s)
	}
	if s := FormatMinor(1234, 3); s != "1.234" {
		t.Fatal(s)
	}
}

func TestTaxFromInclusive(t *testing.T) {
	cases := []struct {
		amount int64
		rate   float64
		want   int64
	}{
		{11000, 10, 1000}, // $110 incl. 10% GST -> $10
		{1000, 10, 91},    // $10 -> 90.909c -> 91c
		{12000, 20, 2000}, // £120 incl. 20% VAT
		{10750, 7.5, 750}, // NGN 7.5%
		{11600, 16, 1600}, // KES 16%
		{5000, 0, 0},
		{1, 10, 0},
		{-11000, 10, -1000},
		{2300, 15, 300}, // NZD
	}
	for _, c := range cases {
		if got := TaxFromInclusive(c.amount, c.rate); got != c.want {
			t.Errorf("TaxFromInclusive(%d, %v) = %d, want %d", c.amount, c.rate, got, c.want)
		}
	}
	if ValidRate(-1) || ValidRate(101) || !ValidRate(0) || !ValidRate(8.1) {
		t.Fatal("ValidRate bounds wrong")
	}
}

func TestFYStartAndLabel(t *testing.T) {
	// Australian FY starts 1 July
	if got := FYStartDate(d("2026-09-24"), 7); !got.Equal(d("2026-07-01")) {
		t.Fatal(got)
	}
	if got := FYStartDate(d("2026-06-30"), 7); !got.Equal(d("2025-07-01")) {
		t.Fatal(got)
	}
	if got := FYStartDate(d("2026-03-05"), 1); !got.Equal(d("2026-01-01")) {
		t.Fatal(got)
	}
	if FYLabel(d("2026-07-01")) != "FY 2026–27" || FYLabel(d("2026-01-01")) != "2026" || FYLabel(d("2099-04-01")) != "FY 2099–00" {
		t.Fatal(FYLabel(d("2026-07-01")), FYLabel(d("2099-04-01")))
	}
}

func TestResolvePeriod(t *testing.T) {
	today := d("2026-09-24")
	check := func(key, from, to string) {
		t.Helper()
		p, err := ResolvePeriod(key, "", "", today, 7)
		if err != nil {
			t.Fatal(err)
		}
		if !p.From.Equal(d(from)) || !p.To.Equal(d(to)) {
			t.Errorf("%s = %s..%s, want %s..%s", key, p.From.Format("2006-01-02"), p.To.Format("2006-01-02"), from, to)
		}
	}
	check("this_month", "2026-09-01", "2026-09-30")
	check("last_month", "2026-08-01", "2026-08-31")
	check("this_quarter", "2026-07-01", "2026-09-30")
	check("last_quarter", "2026-04-01", "2026-06-30")
	check("this_fy", "2026-07-01", "2027-06-30")
	check("last_fy", "2025-07-01", "2026-06-30")
	check("ytd", "2026-07-01", "2026-09-24")

	// January: last month / last quarter cross the year boundary
	p, _ := ResolvePeriod("last_month", "", "", d("2026-01-15"), 1)
	if !p.From.Equal(d("2025-12-01")) || !p.To.Equal(d("2025-12-31")) {
		t.Fatal(p)
	}
	p, _ = ResolvePeriod("last_quarter", "", "", d("2026-02-10"), 1)
	if !p.From.Equal(d("2025-10-01")) || !p.To.Equal(d("2025-12-31")) {
		t.Fatal(p)
	}
	// leap year Feb
	p, _ = ResolvePeriod("this_month", "", "", d("2028-02-10"), 1)
	if !p.To.Equal(d("2028-02-29")) || !p.Until().Equal(d("2028-03-01")) {
		t.Fatal(p)
	}
	// custom
	p, err := ResolvePeriod("", "2026-01-01", "2026-01-31", today, 7)
	if err != nil || p.Key != "custom" || !p.Contains(d("2026-01-31")) || p.Contains(d("2026-02-01")) {
		t.Fatal(p, err)
	}
	if _, err := ResolvePeriod("", "2026-02-01", "2026-01-01", today, 7); err == nil {
		t.Fatal("expected error for reversed range")
	}
	if _, err := ResolvePeriod("fortnight", "", "", today, 7); err == nil {
		t.Fatal("expected error for unknown period")
	}
}

func TestAddMonthsAnchoredAndNextDue(t *testing.T) {
	// 31 Jan monthly: Feb clamps to 28, March returns to 31 (anchor kept)
	feb := AddMonthsAnchored(d("2026-01-31"), 1, 31)
	if !feb.Equal(d("2026-02-28")) {
		t.Fatal(feb)
	}
	mar := AddMonthsAnchored(feb, 1, 31)
	if !mar.Equal(d("2026-03-31")) {
		t.Fatal(mar)
	}
	if got, _ := NextDue(d("2026-07-28"), "quarterly", 28); !got.Equal(d("2026-10-28")) {
		t.Fatal(got)
	}
	if got, _ := NextDue(d("2028-02-29"), "yearly", 29); !got.Equal(d("2029-02-28")) {
		t.Fatal(got)
	}
	if got, _ := NextDue(d("2026-11-15"), "monthly", 15); !got.Equal(d("2026-12-15")) {
		t.Fatal(got)
	}
	if got, _ := NextDue(d("2026-12-15"), "monthly", 15); !got.Equal(d("2027-01-15")) {
		t.Fatal(got)
	}
	if _, ok := NextDue(d("2026-01-01"), "none", 1); ok {
		t.Fatal("none should not recur")
	}
}

func sample() []Entry {
	return []Entry{
		{ID: "a", Type: TypeIncome, Date: d("2026-08-03"), Amount: 110000, Tax: 10000, TaxRate: 10, Currency: "AUD", CategoryID: "sales"},
		{ID: "b", Type: TypeExpense, Date: d("2026-08-05"), Amount: 220000, Tax: 20000, TaxRate: 10, Currency: "AUD", CategoryID: "rent"},
		{ID: "c", Type: TypeIncome, Date: d("2026-09-01"), Amount: 55000, Tax: 5000, TaxRate: 10, Currency: "AUD", CategoryID: "sales"},
		{ID: "d", Type: TypeExpense, Date: d("2026-09-02"), Amount: 3000, Tax: 0, TaxRate: 0, Currency: "AUD", CategoryID: "fees"},
		{ID: "e", Type: TypeExpense, Date: d("2026-09-02"), Amount: 9900, Tax: 900, TaxRate: 10, Currency: "AUD", CategoryID: ""},
		{ID: "f", Type: TypeIncome, Date: d("2026-09-10"), Amount: 50000, Tax: 0, Currency: "JPY", CategoryID: "sales"},
	}
}

func TestSummarize(t *testing.T) {
	tt := Summarize(sample(), "AUD")
	if tt.Income != 165000 || tt.Expenses != 232900 || tt.IncomeNet != 150000 || tt.ExpensesNet != 212000 {
		t.Fatalf("%+v", tt)
	}
	if tt.Profit != -62000 || tt.NetCash != -67900 || tt.TaxCollected != 15000 || tt.TaxPaid != 20900 || tt.TaxPayable != -5900 || tt.Count != 5 {
		t.Fatalf("%+v", tt)
	}
	if j := Summarize(sample(), "JPY"); j.Income != 50000 || j.Count != 1 {
		t.Fatalf("%+v", j)
	}
}

func TestRunningBalances(t *testing.T) {
	es := sample()
	// same-day entries ordered by created_at
	es[3].CreatedAt = d("2026-09-02").Add(2 * time.Hour)
	es[4].CreatedAt = d("2026-09-02").Add(1 * time.Hour)
	b := RunningBalances(es, "AUD", 100000)
	want := map[string]int64{"a": 210000, "b": -10000, "c": 45000, "e": 35100, "d": 32100, "f": 50000}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("balance[%s] = %d, want %d", k, b[k], v)
		}
	}
}

func TestMonthlyTotalsAndCategories(t *testing.T) {
	rows := MonthlyTotals(sample(), "AUD", d("2026-07-01"), d("2026-09-30"))
	if len(rows) != 3 || rows[0].Month != "2026-07" || rows[0].Count != 0 {
		t.Fatalf("%+v", rows)
	}
	if rows[1].Profit != 100000-200000 || rows[2].IncomeNet != 50000 || rows[2].ExpensesNet != 3000+9000 {
		t.Fatalf("%+v", rows)
	}
	// FY across calendar years has 12 months
	if n := len(MonthlyTotals(nil, "AUD", d("2026-07-01"), d("2027-06-30"))); n != 12 {
		t.Fatal(n)
	}
	cats := ByCategory(sample(), "AUD", TypeExpense, map[string]string{"rent": "Rent", "fees": "Bank fees"})
	if len(cats) != 3 || cats[0].Name != "Rent" || cats[0].Amount != 200000 || cats[1].Name != "Uncategorised" || cats[1].Amount != 9000 || cats[2].Amount != 3000 {
		t.Fatalf("%+v", cats)
	}
}

func TestTaxByRate(t *testing.T) {
	rates := TaxByRate(sample(), "AUD")
	if len(rates) != 2 || rates[0].Rate != 0 || rates[1].Rate != 10 {
		t.Fatalf("%+v", rates)
	}
	if rates[0].Purchases != 3000 || rates[1].Sales != 165000 || rates[1].TaxCollected != 15000 || rates[1].TaxPaid != 20900 {
		t.Fatalf("%+v", rates)
	}
}

func TestValidateAttachmentData(t *testing.T) {
	if err := validateAttachmentData("data:image/png;base64,iVBORw0KGgo="); err != nil {
		t.Fatal(err)
	}
	if err := validateAttachmentData("data:text/html;base64,PGh0bWw+"); err == nil {
		t.Fatal("html should be rejected")
	}
	big := make([]byte, (MaxAttachmentBytes/3+10)*4)
	for i := range big {
		big[i] = 'A'
	}
	if err := validateAttachmentData("data:image/jpeg;base64," + string(big)); err == nil {
		t.Fatal("oversized attachment should be rejected")
	}
}
