package pricebook

import (
	"os"
	"strings"
	"testing"
)

func TestCSVHeaderIsTheContract(t *testing.T) {
	const want = "supplier,supplier_type,supplier_currency,kind,name,tld,period,price,regular_price,setup_fee,disk_gb,sites,email_accounts,as_of,source_url,notes"
	if CSVHeader != want {
		t.Fatalf("the CSV header is a published contract:\n got %s\nwant %s", CSVHeader, want)
	}
	if len(CSVColumns) != 16 || len(CSVDocs()) != 16 {
		t.Fatalf("columns %d, docs %d", len(CSVColumns), len(CSVDocs()))
	}
	for i, d := range CSVDocs() {
		if d.Name != CSVColumns[i] {
			t.Fatalf("doc %d is %s, column is %s", i, d.Name, CSVColumns[i])
		}
	}
	// The template is the header plus comments only: importing it adds nothing.
	tpl := string(TemplateCSV())
	if !strings.HasPrefix(tpl, want+"\n") {
		t.Fatalf("template must start with the header: %q", tpl[:80])
	}
	rows, err := ParseCSV(TemplateCSV())
	if err != nil || len(rows) != 0 {
		t.Fatalf("template rows: %d, %v", len(rows), err)
	}
}

func TestParseAmount(t *testing.T) {
	for _, c := range []struct {
		in    string
		want  float64
		blank bool
		ok    bool
	}{
		{"6.50", 6.5, false, true}, {" 6.5 ", 6.5, false, true}, {"A$1,234.50", 1234.5, false, true}, {"KES 4,990", 4990, false, true}, {"KSh 4 990", 4990, false, true},
		{"$12", 12, false, true}, {"US$ 2.99", 2.99, false, true}, {"1.234,50", 1234.5, false, true}, {"12,5", 12.5, false, true}, {"0,99", 0.99, false, true},
		{"1,234", 1234, false, true}, {"12,345,678", 12345678, false, true}, {"1.234.567", 1234567, false, true}, {"€9", 9, false, true}, {"£1,000.00", 1000, false, true},
		{"2.99/mo", 2.99, false, true}, {"free", 0, false, true}, {"Included", 0, false, true}, {"0", 0, false, true},
		{"", 0, true, true}, {"  ", 0, true, true}, {"-", 0, true, true}, {"n/a", 0, true, true},
		{"abc", 0, false, false}, {"12.3.4,5,6", 0, false, false}, {"-5", -5, false, true}, {"(5.00)", -5, false, true},
	} {
		v, blank, ok := ParseAmount(c.in)
		if ok != c.ok || blank != c.blank || (ok && v != c.want) {
			t.Errorf("ParseAmount(%q) = %v blank=%v ok=%v, want %v blank=%v ok=%v", c.in, v, blank, ok, c.want, c.blank, c.ok)
		}
	}
}

func TestParseFields(t *testing.T) {
	for in, want := range map[string]string{
		"hosting_plan": KindHosting, "Hosting Plan": KindHosting, " HOSTING ": KindHosting, "hosting-plan": KindHosting, "Domain Register": KindRegister, "registration": KindRegister,
		"domain_renew": KindRenew, "Renewal": KindRenew, "Domain-Transfer": KindTransfer, "SSL": KindSSL, "Email Mailbox": KindEmail, "mailbox": KindEmail, "Other": KindOther,
	} {
		if got, ok := ParseKind(in); !ok || got != want {
			t.Errorf("ParseKind(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	if _, ok := ParseKind("vps"); ok {
		t.Error("unknown kind accepted")
	}
	for in, want := range map[string]string{
		"month": PeriodMonth, "Monthly": PeriodMonth, "mo": PeriodMonth, "year": PeriodYear, "Yearly": PeriodYear, "annual": PeriodYear, "1y": PeriodYear, "12 months": PeriodYear,
		"2y": Period2Y, "2 years": Period2Y, "24m": Period2Y, "3Y": Period3Y, "36 months": Period3Y, "one_off": PeriodOneOff, "One-off": PeriodOneOff, "once": PeriodOneOff,
	} {
		if got, ok := ParsePeriod(in); !ok || got != want {
			t.Errorf("ParsePeriod(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	if _, ok := ParsePeriod("fortnight"); ok {
		t.Error("unknown period accepted")
	}
	for in, want := range map[string]string{".co.ke": ".co.ke", "CO.KE": ".co.ke", " .Com ": ".com", "*.com.au": ".com.au", "com": ".com", "..org": ".org", "": "", "co ke": "", "ke!": ""} {
		if got := NormTLD(in); got != want {
			t.Errorf("NormTLD(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"wholesale": TypeWholesale, "Self-Hosted": TypeSelfHosted, "self hosted": TypeSelfHosted, "COMPETITOR": TypeCompetitor, "retail": TypeCompetitor} {
		if got, ok := ParseType(in); !ok || got != want {
			t.Errorf("ParseType(%q) = %q %v", in, got, ok)
		}
	}
	for in, want := range map[string]string{"aud": "AUD", "A$": "AUD", "KSh": "KES", "usd": "USD", "$": "USD", "US$": "USD"} {
		if got, ok := ParseCurrency(in); !ok || got != want {
			t.Errorf("ParseCurrency(%q) = %q %v", in, got, ok)
		}
	}
	if _, ok := ParseCurrency("dollars"); ok {
		t.Error("unknown currency accepted")
	}
	for in, want := range map[string]float64{"10": 10, "10GB": 10, "10 gb": 10, "512MB": 0.5, "1 TB": 1024, "2.5": 2.5, "unlimited": -1, "Unmetered": -1, "1,024 MB": 1} {
		got, ok := ParseDiskGB(in)
		if !ok || got == nil || *got != want {
			t.Errorf("ParseDiskGB(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	if v, ok := ParseDiskGB(""); !ok || v != nil {
		t.Error("blank disk is unknown, not an error")
	}
	if _, ok := ParseDiskGB("lots"); ok {
		t.Error("bad disk accepted")
	}
	for in, want := range map[string]int{"1": 1, "50": 50, "Unlimited": -1, "1,000": 1000, "0": 0} {
		got, ok := ParseCount(in)
		if !ok || got == nil || *got != want {
			t.Errorf("ParseCount(%q) = %v %v", in, got, ok)
		}
	}
	if _, ok := ParseCount("2.5"); ok {
		t.Error("fractional count accepted")
	}
	for in, want := range map[string]string{"2026-10-02": "2026-10-02", "02/10/2026": "2026-10-02", "2 Oct 2026": "2026-10-02", "Oct 2, 2026": "2026-10-02", "2026/10/02": "2026-10-02"} {
		got, ok := ParseDay(in)
		if !ok || got == nil || got.Format("2006-01-02") != want {
			t.Errorf("ParseDay(%q) = %v %v", in, got, ok)
		}
	}
	if _, ok := ParseDay("soon"); ok {
		t.Error("bad date accepted")
	}
	if _, ok := ParseDay("1999-01-01"); ok {
		t.Error("a 1999 price date is a typo")
	}
}

const messyCSV = "\xef\xbb\xbfSupplier , KIND,Name,TLD,Period,Price,Regular_Price,Supplier_Currency,supplier_type,Disk_GB,Sites,Email_Accounts,As_Of,Setup_Fee,Notes,source_url,extra\r\n" +
	"# a comment line, ignored\r\n" +
	"\r\n" +
	"  Synergy   Wholesale , Hosting Plan , Starter  Env ,, Monthly , A$6.50 ,, aud , Wholesale , 2 GB , 1 , 5 , 28/09/2026 , , incl. GST , example.test/hosting , ignored\r\n" +
	"Synergy Wholesale,DOMAIN_REGISTER,,CO.KE,Yearly,\"1,021.00\",,AUD,,,,,2026-09-28,,,,\r\n" +
	"Synergy Wholesale,domain renew,.co.ke,,,23.50,,,,,,,,,,,\r\n" +
	"hosting.com,hosting,Launch,,annual,$35.88,$131.88,USD,wholesale,15GB,1,10,,0,intro,,\r\n" +
	"hosting.com,hosting,Grow,,year,71.88,,usd,,unlimited,Unlimited,unlimited,,,,,\r\n" +
	"Example Host Kenya,hosting_plan,Basic,,year,\"KES 2,500\",\"3,500\",KES,Competitor,5,1,10,,,,,\r\n" +
	",hosting_plan,No supplier,,month,5,,,,,,,,,,,\r\n" +
	"Synergy Wholesale,vps,Bad kind,,month,5,,,,,,,,,,,\r\n" +
	"Synergy Wholesale,hosting_plan,Bad price,,month,abc,,,,,,,,,,,\r\n" +
	"Synergy Wholesale,hosting_plan,No price,,month,,,,,,,,,,,,\r\n" +
	"Synergy Wholesale,domain_register,,,year,10,,,,,,,,,,,\r\n" +
	"Synergy Wholesale,domain_register,,.com,month,10,,,,,,,,,,,\r\n" +
	"Synergy Wholesale,hosting_plan,Bad bits,,fortnight,-4,x,ZZZ,nobody,big,2.5,many,soon,-1,,,\r\n" +
	"Synergy Wholesale,hosting_plan,starter env,,month,7.00,,AUD,,2,1,5,2026-09-29,,,,\r\n"

func TestParseCSVTolerance(t *testing.T) {
	rows, err := ParseCSV([]byte(messyCSV))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 14 {
		t.Fatalf("rows: %d", len(rows))
	}
	r := rows[0]
	if len(r.Errors) != 0 || r.Supplier != "Synergy Wholesale" || r.Kind != KindHosting || r.Name != "Starter Env" || r.Period != PeriodMonth || r.Price != 6.5 || r.SupplierCurrency != "AUD" ||
		r.SupplierType != TypeWholesale || *r.DiskGB != 2 || *r.Sites != 1 || *r.EmailAccounts != 5 || r.AsOf != "2026-09-28" || r.Notes != "incl. GST" || r.SourceURL != "https://example.test/hosting" || r.Line != 4 {
		t.Fatalf("row 1: %+v", r)
	}
	// The same plan again on the last line (different case): the last one wins.
	if r.Action != "duplicate" || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "replaced by line 17") {
		t.Fatalf("duplicate: %q %v", r.Action, r.Warnings)
	}
	if last := rows[13]; last.Price != 7 || last.Action != "" || last.Key() != r.Key() {
		t.Fatalf("last row: %+v", last)
	}
	r = rows[1]
	if len(r.Errors) != 0 || r.Kind != KindRegister || r.TLD != ".co.ke" || r.Name != ".co.ke registration" || r.Price != 1021 || r.Period != PeriodYear {
		t.Fatalf("domain row: %+v", r)
	}
	// TLD typed in the name column, blank period → year with a warning, blank currency and date.
	r = rows[2]
	if len(r.Errors) != 0 || r.Kind != KindRenew || r.TLD != ".co.ke" || r.Name != ".co.ke renewal" || r.Period != PeriodYear || r.SupplierCurrency != "" || r.AsOf != "" || len(r.Warnings) != 1 {
		t.Fatalf("renew row: %+v", r)
	}
	r = rows[3]
	if len(r.Errors) != 0 || r.Price != 35.88 || r.RegularPrice == nil || *r.RegularPrice != 131.88 || *r.DiskGB != 15 || r.Period != PeriodYear || r.SupplierCurrency != "USD" {
		t.Fatalf("promo row: %+v", r)
	}
	r = rows[4]
	if len(r.Errors) != 0 || *r.DiskGB != Unlimited || *r.Sites != Unlimited || *r.EmailAccounts != Unlimited || r.RegularPrice != nil {
		t.Fatalf("unlimited row: %+v", r)
	}
	r = rows[5]
	if len(r.Errors) != 0 || r.Price != 2500 || *r.RegularPrice != 3500 || r.SupplierType != TypeCompetitor {
		t.Fatalf("competitor row: %+v", r)
	}
	wantErr := []string{"supplier is required", `kind "vps"`, `price "abc" is not a number`, "price is required", "tld is required", "a domain price is per year"}
	for i, w := range wantErr {
		r := rows[6+i]
		if r.Action != "error" || len(r.Errors) == 0 || !strings.Contains(strings.Join(r.Errors, "; "), w) {
			t.Errorf("row %d: want error %q, got %v", 7+i, w, r.Errors)
		}
	}
	bad := strings.Join(rows[12].Errors, "; ")
	for _, w := range []string{"supplier_type", "supplier_currency", "period", "price cannot be negative", "regular_price", "setup_fee cannot be negative", "disk_gb", "sites", "email_accounts", "as_of"} {
		if !strings.Contains(bad, w) {
			t.Errorf("every problem of a row is reported: missing %q in %s", w, bad)
		}
	}
}

func TestCleanURL(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "  ": "", "https://example.test/pricing": "https://example.test/pricing", "http://example.test": "http://example.test",
		"example.test/hosting": "https://example.test/hosting", "example.test:8443/x": "https://example.test:8443/x", "HTTPS://Example.test": "HTTPS://Example.test",
	} {
		if got, ok := CleanURL(in); !ok || got != want {
			t.Errorf("CleanURL(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "javascript://%0aalert(1)", "data:text/html,x", "ftp://example.test/x", "see the website", "localhost", "https://", "x\"onmouseover=\"1.test"} {
		if got, ok := CleanURL(bad); ok {
			t.Errorf("CleanURL(%q) accepted as %q", bad, got)
		}
	}
	// In a CSV a bad link is dropped with a warning; the price still imports.
	rows, err := ParseCSV([]byte(CSVHeader + "\nSynergy,,,hosting_plan,Starter,,month,5,,,,,,,javascript:alert(1),\n"))
	if err != nil || len(rows) != 1 || len(rows[0].Errors) != 0 || rows[0].SourceURL != "" || len(rows[0].Warnings) != 1 {
		t.Fatalf("bad link in a CSV: %+v %v", rows, err)
	}
}

func TestParseCSVFileProblems(t *testing.T) {
	for name, data := range map[string]string{
		"empty":         "",
		"only spaces":   " \n\n",
		"no header":     "Synergy,hosting_plan,Starter,,month,6.5\n",
		"missing price": "supplier,kind,name\nA,hosting_plan,B\n",
	} {
		if _, err := ParseCSV([]byte(data)); err == nil {
			t.Errorf("%s: want an error", name)
		} else if _, ok := err.(*ParseError); !ok {
			t.Errorf("%s: want a ParseError, got %T", name, err)
		}
	}
	// Semicolon-separated (Excel in some locales) with decimal commas.
	rows, err := ParseCSV([]byte("supplier;kind;name;tld;period;price\nSynergy;hosting_plan;Starter;;month;6,50\n"))
	if err != nil || len(rows) != 1 || len(rows[0].Errors) != 0 || rows[0].Price != 6.5 {
		t.Fatalf("semicolons: %+v %v", rows, err)
	}
	// Header only: no rows, no error.
	rows, err = ParseCSV([]byte(CSVHeader + "\n"))
	if err != nil || len(rows) != 0 {
		t.Fatalf("header only: %d %v", len(rows), err)
	}
}

func TestSampleCSV(t *testing.T) {
	data, err := os.ReadFile("testdata/sample-prices.csv")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), CSVHeader+"\n") {
		t.Fatal("the sample must use the exact header")
	}
	rows, err := ParseCSV(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 28 {
		t.Fatalf("sample rows: %d", len(rows))
	}
	sup := map[string]int{}
	for _, r := range rows {
		if len(r.Errors) != 0 || len(r.Warnings) != 0 || r.Action != "" {
			t.Errorf("line %d: %v %v %s", r.Line, r.Errors, r.Warnings, r.Action)
		}
		sup[r.Supplier]++
		if !strings.Contains(r.Notes, "SAMPLE") {
			t.Errorf("line %d: sample prices must say they are made up", r.Line)
		}
	}
	if len(sup) != 5 || sup["Synergy Wholesale"] != 12 || sup["hosting.com"] != 7 {
		t.Fatalf("suppliers: %v", sup)
	}
}

func TestExportCSVRoundTrip(t *testing.T) {
	d := fixture()
	byID := map[string]Supplier{}
	for _, s := range d.Suppliers {
		byID[s.ID] = s
	}
	d.Items[0].Notes = "has, a comma and a \"quote\""
	d.Items[0].SetupFee = 5
	out := ExportCSV(d.Items, byID)
	if !strings.HasPrefix(string(out), CSVHeader+"\n") {
		t.Fatalf("export header: %q", string(out)[:120])
	}
	rows, err := ParseCSV(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(d.Items) {
		t.Fatalf("rows %d, items %d", len(rows), len(d.Items))
	}
	for i, r := range rows {
		it := d.Items[i]
		if len(r.Errors) != 0 || len(r.Warnings) != 0 {
			t.Fatalf("row %d: %v %v", i, r.Errors, r.Warnings)
		}
		if r.Supplier != byID[it.SupplierID].Name || r.SupplierType != byID[it.SupplierID].Type || r.SupplierCurrency != it.Currency || r.Kind != it.Kind || r.Name != it.Name || r.TLD != it.TLD ||
			r.Period != it.Period || r.Price != it.Price || !samePtr(r.RegularPrice, it.RegularPrice) || r.SetupFee != it.SetupFee || !samePtr(r.DiskGB, it.DiskGB) || !sameInt(r.Sites, it.Sites) ||
			!sameInt(r.EmailAccounts, it.EmailAccounts) || r.AsOf != it.AsOf.Format("2006-01-02") || r.Notes != it.Notes {
			t.Fatalf("row %d does not round-trip:\n got %+v\nwant %+v", i, r, it)
		}
	}
}
