package pnl

import (
	"testing"
	"time"
)

func d(s string) time.Time { return parseDay(s) }

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

var sep = Period{d("2026-09-01"), d("2026-09-30")}

func base() *Input {
	return &Input{Home: "AUD", OrgHome: "AUD", Decimals: 2, Loc: time.UTC,
		InvoiceNumbers: map[string]bool{}, BillRefs: map[string]bool{}, PONumbers: map[string]bool{}, PayRunNumbers: map[string]bool{}}
}

func amt(s *Statement, key string) int64 {
	if l := s.Lines[key]; l != nil {
		return l.Amount
	}
	return 0
}

func ex(s *Statement, key string) (int, int64) {
	if e := s.Excluded[key]; e != nil {
		return e.Count, e.Amount
	}
	return 0, 0
}

func TestInvoicesAccrual(t *testing.T) {
	in := base()
	in.Invoices = []Invoice{
		{ID: "a", DocType: "invoice", Status: "sent", Currency: "AUD", IssueDate: d("2026-09-05"), Total: 110, Tax: 10},
		{ID: "b", DocType: "invoice", Status: "paid", Currency: "", IssueDate: d("2026-09-30"), Total: 220, Tax: 20},
		{ID: "c", DocType: "invoice", Status: "draft", Currency: "AUD", IssueDate: d("2026-09-10"), Total: 1000, Tax: 0},
		{ID: "e", DocType: "invoice", Status: "void", Currency: "AUD", IssueDate: d("2026-09-10"), Total: 500, Tax: 0},
		{ID: "f", DocType: "quote", Status: "sent", Currency: "AUD", IssueDate: d("2026-09-10"), Total: 700, Tax: 0},
		{ID: "g", DocType: "invoice", Status: "sent", Currency: "USD", IssueDate: d("2026-09-10"), Total: 900, Tax: 0},
		{ID: "h", DocType: "invoice", Status: "sent", Currency: "AUD", IssueDate: d("2026-10-01"), Total: 50, Tax: 0},
		{ID: "i", DocType: "invoice", Status: "sent", Currency: "AUD", IssueDate: d("2026-08-31"), Total: 60, Tax: 0},
		// credit note: a negative invoice reduces revenue (and tax)
		{ID: "cn", DocType: "invoice", Status: "sent", Currency: "AUD", IssueDate: d("2026-09-12"), Total: -55, Tax: -5},
	}
	s := Compute(in, sep, Accrual)
	if got := amt(s, "invoices"); got != 25000 { // 100 + 200 - 50
		t.Fatalf("invoices = %d, want 25000", got)
	}
	if s.TaxCollected != 2500 {
		t.Fatalf("tax collected = %d, want 2500", s.TaxCollected)
	}
	if n, v := ex(s, ExInvoiceVoid); n != 1 || v != 50000 {
		t.Fatalf("void excluded = %d %d", n, v)
	}
	// the USD invoice shows up only in a USD statement
	usd := *in
	usd.Home = "USD"
	if got := amt(Compute(&usd, sep, Accrual), "invoices"); got != 90000 {
		t.Fatalf("usd invoices = %d", got)
	}
}

func TestInvoicesCashBasis(t *testing.T) {
	in := base()
	in.Invoices = []Invoice{
		{ID: "a", DocType: "invoice", Status: "partial", Currency: "AUD", IssueDate: d("2026-07-05"), Total: 110, Tax: 10},
		{ID: "u", DocType: "invoice", Status: "paid", Currency: "USD", IssueDate: d("2026-09-05"), Total: 100, Tax: 0},
	}
	in.InvoicePayments = []InvoicePayment{
		{InvoiceID: "a", Date: d("2026-09-02"), Amount: 55},  // half: 50 ex tax + 5 tax
		{InvoiceID: "a", Date: d("2026-10-02"), Amount: 55},  // next month
		{InvoiceID: "u", Date: d("2026-09-03"), Amount: 100}, // other currency
		{InvoiceID: "missing", Date: d("2026-09-03"), Amount: 999},
	}
	s := Compute(in, sep, Cash)
	if got := amt(s, "invoices"); got != 5000 {
		t.Fatalf("cash invoices = %d, want 5000", got)
	}
	if s.TaxCollected != 500 {
		t.Fatalf("tax = %d", s.TaxCollected)
	}
	// accrual ignores payments: the invoice was issued in July
	if got := amt(Compute(in, sep, Accrual), "invoices"); got != 0 {
		t.Fatalf("accrual = %d", got)
	}
}

func TestPOSRefundsVoidsAndTimeZone(t *testing.T) {
	in := base()
	in.POS = []POSTxn{
		{Type: "sale", Status: "completed", At: ts("2026-09-10T03:00:00Z"), Total: 110, Tax: 10},
		{Type: "return", Status: "completed", At: ts("2026-09-11T03:00:00Z"), Total: -22, Tax: -2},
		{Type: "sale", Status: "voided", At: ts("2026-09-12T03:00:00Z"), Total: 33, Tax: 3},
		// 22:30 UTC on 30 Sep is already 1 Oct in Nairobi (UTC+3)
		{Type: "sale", Status: "completed", At: ts("2026-09-30T22:30:00Z"), Total: 10, Tax: 0},
		// 21:30 UTC on 31 Aug is 1 Sep in Nairobi
		{Type: "sale", Status: "completed", At: ts("2026-08-31T21:30:00Z"), Total: 7, Tax: 0},
	}
	s := Compute(in, sep, Accrual) // UTC
	if got := amt(s, "pos"); got != 10000-2000+1000 {
		t.Fatalf("pos utc = %d", got)
	}
	if n, v := ex(s, ExPOSVoided); n != 1 || v != 3300 {
		t.Fatalf("voided = %d %d", n, v)
	}
	nbo, _ := time.LoadLocation("Africa/Nairobi")
	in.Loc = nbo
	s = Compute(in, sep, Accrual)
	if got := amt(s, "pos"); got != 10000-2000+700 {
		t.Fatalf("pos nairobi = %d", got)
	}
	if s.TaxCollected != 800 {
		t.Fatalf("pos tax = %d", s.TaxCollected)
	}
	// POS is the same on a cash basis, and never in another currency's statement
	if got := amt(Compute(in, sep, Cash), "pos"); got != 8700 {
		t.Fatalf("pos cash = %d", got)
	}
	usd := *in
	usd.Home = "USD"
	if got := amt(Compute(&usd, sep, Accrual), "pos"); got != 0 {
		t.Fatalf("pos in usd = %d", got)
	}
	// Sales record their currency: after the organisation moved to KES, its
	// KES sales count in a KES statement and old AUD sales stay in AUD.
	mixed := base()
	mixed.OrgHome = "KES"
	mixed.POS = []POSTxn{
		{Type: "sale", Status: "completed", Currency: "AUD", At: ts("2026-09-10T03:00:00Z"), Total: 110, Tax: 10},
		{Type: "sale", Status: "completed", Currency: "KES", At: ts("2026-09-10T04:00:00Z"), Total: 500, Tax: 0},
		{Type: "sale", Status: "completed", At: ts("2026-09-10T05:00:00Z"), Total: 40, Tax: 0}, // blank = org currency (KES)
	}
	if got := amt(Compute(mixed, sep, Accrual), "pos"); got != 10000 {
		t.Fatalf("aud statement pos = %d", got)
	}
	mixed.Home = "KES"
	if got := amt(Compute(mixed, sep, Accrual), "pos"); got != 54000 {
		t.Fatalf("kes statement pos = %d", got)
	}
}

func TestBillLinkedToPOCountsOnce(t *testing.T) {
	in := base()
	in.Receipts = []StockReceipt{
		{POID: "po1", PONumber: "PO-1", At: ts("2026-09-03T01:00:00Z"), Value: 300}, // billed below → excluded
		{POID: "po2", PONumber: "PO-2", At: ts("2026-09-04T01:00:00Z"), Value: 120}, // only a draft bill → counted
		{POID: "po3", PONumber: "PO-3", At: ts("2026-09-05T01:00:00Z"), Value: 80},  // no bill → counted
	}
	in.Bills = []Bill{
		{ID: "b1", Status: "unpaid", POID: "po1", Date: d("2026-09-06"), Lines: []BillLine{{AccountName: "Other Expenses", AccountType: "Expense", Amount: 310, Tax: 31}}},
		{ID: "b2", Status: "draft", POID: "po2", Date: d("2026-09-06"), Lines: []BillLine{{AccountName: "Other Expenses", AccountType: "Expense", Amount: 120, Tax: 12}}},
	}
	s := Compute(in, sep, Accrual)
	if got := amt(s, "stock_received"); got != 20000 {
		t.Fatalf("stock received = %d, want 20000", got)
	}
	if got := amt(s, "bills_cos"); got != 31000 {
		t.Fatalf("billed stock = %d, want 31000 (bill linked to a PO is cost of sales)", got)
	}
	if n, v := ex(s, ExPOBilled); n != 1 || v != 30000 {
		t.Fatalf("po billed excluded = %d %d", n, v)
	}
	tot := s.Totals()
	if tot.CostOfSales != 51000 || tot.Expenses != 0 {
		t.Fatalf("totals %+v", tot)
	}
	if s.TaxPaid != 3100 {
		t.Fatalf("tax paid = %d", s.TaxPaid)
	}
}

func TestBillsAccrualVsCash(t *testing.T) {
	in := base()
	in.Bills = []Bill{{ID: "b", Status: "partial", Date: d("2026-08-20"), Lines: []BillLine{
		{AccountName: "Rent", AccountType: "Expense", Amount: 800, Tax: 80},
		{AccountName: "Cost of Goods Sold", AccountType: "Expense", SubType: "Cost of Sales", Amount: 200, Tax: 20},
		{AccountName: "Office Equipment", AccountType: "Asset", SubType: "Fixed Asset", Amount: 100, Tax: 10},
		{AccountName: "Inventory", AccountType: "Asset", SubType: "Current Asset", Amount: 100, Tax: 10},
	}}}
	in.BillPayments = []BillPayment{{BillID: "b", Date: d("2026-09-15"), Amount: 660}} // half of 1320
	aug := Period{d("2026-08-01"), d("2026-08-31")}
	a := Compute(in, aug, Accrual)
	if amt(a, "exp:rent") != 80000 || amt(a, "bills_cos") != 30000 {
		t.Fatalf("accrual rent=%d cos=%d", amt(a, "exp:rent"), amt(a, "bills_cos"))
	}
	if n, v := ex(a, ExBillCapital); n != 1 || v != 10000 {
		t.Fatalf("capital = %d %d", n, v)
	}
	if amt(Compute(in, sep, Accrual), "exp:rent") != 0 {
		t.Fatal("accrual counts the bill in its bill month only")
	}
	c := Compute(in, sep, Cash)
	if amt(c, "exp:rent") != 40000 || amt(c, "bills_cos") != 15000 {
		t.Fatalf("cash rent=%d cos=%d", amt(c, "exp:rent"), amt(c, "bills_cos"))
	}
	if c.TaxPaid != 6000 {
		t.Fatalf("cash tax paid = %d", c.TaxPaid)
	}
	if amt(Compute(in, aug, Cash), "exp:rent") != 0 {
		t.Fatal("cash basis counts the payment month only")
	}
}

func TestCashBookDeduplication(t *testing.T) {
	in := base()
	in.InvoiceNumbers = map[string]bool{"inv-1001": true}
	in.BillRefs = map[string]bool{"bill-0001": true}
	in.PayRunNumbers = map[string]bool{"pr-0001": true}
	in.POS = []POSTxn{{Number: "POS-77", Status: "reference-only"}}
	day := d("2026-09-10")
	in.Cash = []CashEntry{
		{Type: "income", Currency: "AUD", Category: "Sales", Date: day, Amount: 11000, Tax: 1000},                       // counted
		{Type: "income", Currency: "AUD", Category: "Sales", Date: day, Amount: 5000, LinkType: "invoice"},              // linked
		{Type: "income", Currency: "AUD", Category: "Sales", Date: day, Amount: 7000, Reference: "INV-1001"},            // matches invoice
		{Type: "income", Currency: "AUD", Category: "Sales", Date: day, Amount: 3000, Reference: " pos-77 "},            // matches POS sale
		{Type: "income", Currency: "AUD", Category: "Other income", Date: day, Amount: 9000, LinkType: "transfer"},      // owner money
		{Type: "expense", Currency: "AUD", Category: "Rent", Date: day, Amount: 22000, Tax: 2000},                       // counted
		{Type: "expense", Currency: "AUD", Category: "Wages", Date: day, Amount: 4000, Reference: "PR-0001"},            // matches pay run
		{Type: "expense", Currency: "AUD", Category: "Office", Date: day, Amount: 1500, Reference: "BILL-0001"},         // matches bill
		{Type: "expense", Currency: "AUD", Category: "Tax", Date: day, Amount: 6000},                                    // tax payment
		{Type: "expense", Currency: "AUD", Category: "Stock / COGS", Date: day, Amount: 3300, Tax: 300},                 // cost of sales
		{Type: "expense", Currency: "KES", Category: "Rent", Date: day, Amount: 100000},                                 // other currency
		{Type: "expense", Currency: "AUD", Category: "Rent", Date: d("2026-10-01"), Amount: 999},                        // next month
		{Type: "income", Currency: "AUD", Category: "Sales", Date: day, Amount: 1234, Reference: "INV-9999"},            // unknown ref → counted
		{Type: "expense", Currency: "AUD", Category: "Software", Date: day, Amount: 500, Reference: "not-a-doc-number"}, // counted
		{Type: "expense", Currency: "AUD", Category: "rent", Date: day, Amount: 100, LinkType: "bill"},                  // linked bill
		{Type: "expense", Currency: "AUD", Category: "Marketing", Date: day, Amount: 100, LinkType: "purchase_order"},   // linked PO
		{Type: "expense", Currency: "AUD", Category: "Marketing", Date: day, Amount: 100, LinkType: "payroll"},          // linked pay run
		{Type: "income", Currency: "AUD", Category: "Sales", Date: day, Amount: 100, LinkType: "event"},                 // linked event
		{Type: "income", Currency: "AUD", Category: "Sales", Date: day, Amount: 100, LinkType: "pos"},                   // linked POS
	}
	in.Bills = []Bill{{ID: "b", Status: "unpaid", Date: day, Lines: []BillLine{{AccountName: "Rent", AccountType: "Expense", Amount: 500}}}}
	s := Compute(in, sep, Accrual)
	if got := amt(s, "cashbook_income"); got != 10000+1234 {
		t.Fatalf("cash income = %d", got)
	}
	if got := amt(s, "exp:rent"); got != 20000+50000 { // cash book + bill share the "Rent" line
		t.Fatalf("rent = %d", got)
	}
	if l := s.Lines["exp:rent"]; l.Parts["cashbook"] != 20000 || l.Parts["bills"] != 50000 {
		t.Fatalf("rent parts %+v", l.Parts)
	}
	if got := amt(s, "cashbook_cos"); got != 3000 {
		t.Fatalf("cash cos = %d", got)
	}
	if got := amt(s, "exp:software"); got != 500 {
		t.Fatalf("software = %d", got)
	}
	if n, v := ex(s, ExCashLinked); n != 6 || v != 5000+100*5 {
		t.Fatalf("linked = %d %d", n, v)
	}
	if n, v := ex(s, ExCashMatched); n != 4 || v != 7000+3000+4000+1500 {
		t.Fatalf("matched = %d %d", n, v)
	}
	if n, _ := ex(s, ExCashTransfer); n != 1 {
		t.Fatal("transfer not excluded")
	}
	if n, v := ex(s, ExCashTax); n != 1 || v != 6000 {
		t.Fatalf("tax payment = %d %d", n, v)
	}
	if s.TaxCollected != 1000 || s.TaxPaid != 2000+300 {
		t.Fatalf("tax %d / %d", s.TaxCollected, s.TaxPaid)
	}
	// the cash book is cash: same numbers on both bases
	c := Compute(in, sep, Cash)
	if amt(c, "cashbook_income") != amt(s, "cashbook_income") || amt(c, "exp:software") != 500 {
		t.Fatal("cash book differs by basis")
	}
}

func TestPayrollBasis(t *testing.T) {
	in := base()
	paid := ts("2026-10-01T09:00:00Z")
	in.PayRuns = []PayRun{
		{Status: "paid", Currency: "AUD", PayDate: d("2026-09-30"), PaidAt: &paid, Gross: 5000, Deductions: 1000, Net: 4000},
		{Status: "draft", Currency: "AUD", PayDate: d("2026-09-15"), Gross: 9999},
		{Status: "paid", Currency: "KES", PayDate: d("2026-09-15"), PaidAt: &paid, Gross: 7777},
	}
	a := Compute(in, sep, Accrual)
	if got := amt(a, "payroll"); got != 500000 {
		t.Fatalf("accrual payroll = %d", got)
	}
	if l := a.Lines["payroll"]; l.Parts["deductions"] != 100000 || l.Parts["net"] != 400000 {
		t.Fatalf("payroll parts %+v", l.Parts)
	}
	if got := amt(Compute(in, sep, Cash), "payroll"); got != 0 {
		t.Fatalf("cash payroll in Sep = %d (paid on 1 Oct)", got)
	}
	oct := Period{d("2026-10-01"), d("2026-10-31")}
	if got := amt(Compute(in, oct, Cash), "payroll"); got != 500000 {
		t.Fatalf("cash payroll in Oct = %d", got)
	}
}

func TestEventsOnlineAndTotals(t *testing.T) {
	in := base()
	at := ts("2026-09-10T10:00:00Z")
	in.Events = []EventReg{
		{PaymentStatus: "paid", Currency: "AUD", At: at, Amount: 25},
		{PaymentStatus: "paid", Currency: "", At: at, Amount: 25},
		{PaymentStatus: "pending", Currency: "AUD", At: at, Amount: 25},
		{PaymentStatus: "refunded", Currency: "AUD", At: at, Amount: 25},
		{PaymentStatus: "paid", Currency: "USD", At: at, Amount: 25},
	}
	in.Online = []OnlineOrder{
		{Status: "completed", PaymentStatus: "paid", Currency: "AUD", At: at, Total: 110, Tax: 10},
		{Status: "processing", PaymentStatus: "paid", Currency: "AUD", At: at, Total: 55, Tax: 5},
		{Status: "cancelled", PaymentStatus: "paid", Currency: "AUD", At: at, Total: 99},
		{Status: "completed", PaymentStatus: "refunded", Currency: "AUD", At: at, Total: 99},
		{Status: "pending", PaymentStatus: "unpaid", Currency: "AUD", At: at, Total: 99},
	}
	in.Cash = []CashEntry{{Type: "expense", Currency: "AUD", Category: "Rent", Date: d("2026-09-02"), Amount: 3000}}
	s := Compute(in, sep, Accrual)
	if amt(s, "events") != 5000 || amt(s, "online") != 15000 {
		t.Fatalf("events=%d online=%d", amt(s, "events"), amt(s, "online"))
	}
	tot := s.Totals()
	if tot.Revenue != 20000 || tot.Expenses != 3000 || tot.NetProfit != 17000 || tot.GrossProfit != 20000 || tot.TotalExpenses != 3000 {
		t.Fatalf("totals %+v", tot)
	}
	lines := s.SortedLines(SecIncome)
	if len(lines) != 2 || lines[0].Key != "online" || lines[1].Key != "events" {
		t.Fatalf("income order %v", lines)
	}
}

func TestZeroDecimalCurrencyRounding(t *testing.T) {
	in := base()
	in.Home, in.OrgHome, in.Decimals = "JPY", "JPY", 0
	in.Invoices = []Invoice{{ID: "a", DocType: "invoice", Status: "sent", Currency: "JPY", IssueDate: d("2026-09-05"), Total: 1100.4, Tax: 100.2}}
	if got := amt(Compute(in, sep, Accrual), "invoices"); got != 1000 {
		t.Fatalf("jpy = %d", got)
	}
}

func TestResolvePeriodAndCompare(t *testing.T) {
	today := d("2026-09-25")
	cases := []struct{ rng, from, to string }{
		{"30d", "2026-08-27", "2026-09-25"},
		{"7d", "2026-09-19", "2026-09-25"},
		{"12m", "2025-10-01", "2026-09-25"},
		{"this_month", "2026-09-01", "2026-09-25"},
		{"last_month", "2026-08-01", "2026-08-31"},
		{"this_quarter", "2026-07-01", "2026-09-25"},
		{"last_quarter", "2026-04-01", "2026-06-30"},
		{"ytd", "2026-01-01", "2026-09-25"},
		{"this_fy", "2026-07-01", "2026-09-25"},
		{"last_fy", "2025-07-01", "2026-06-30"},
	}
	for _, c := range cases {
		p, err := ResolvePeriod(today, "", "", c.rng, 7)
		if err != nil || p.From.Format(dayFmt) != c.from || p.To.Format(dayFmt) != c.to {
			t.Fatalf("%s → %v %v %v", c.rng, p.From, p.To, err)
		}
	}
	p, err := ResolvePeriod(today, "2026-09-01", "2026-09-30", "30d", 7)
	if err != nil || p.Days() != 30 {
		t.Fatal("explicit range", p, err)
	}
	prev := ComparePeriod(p, "previous")
	if prev.From.Format(dayFmt) != "2026-08-02" || prev.To.Format(dayFmt) != "2026-08-31" {
		t.Fatalf("previous %v %v", prev.From, prev.To)
	}
	if ComparePeriod(p, "none") != nil {
		t.Fatal("none")
	}
	if _, err := ResolvePeriod(today, "2026-09-10", "2026-09-01", "", 7); err == nil {
		t.Fatal("reversed range accepted")
	}
}

func TestChangePct(t *testing.T) {
	if v := change(150, 100); v == nil || *v != 50 {
		t.Fatal("50%")
	}
	if v := change(-50, 100); v == nil || *v != -150 {
		t.Fatal("-150%")
	}
	if change(10, 0) != nil {
		t.Fatal("new value has no %")
	}
	if v := change(0, 0); v == nil || *v != 0 {
		t.Fatal("0/0")
	}
}

func TestProcessorFeesAndSurcharges(t *testing.T) {
	in := base()
	in.ProcessorFees = []ProcessorFee{
		{Currency: "AUD", At: ts("2026-09-10T10:00:00Z"), Fee: 2.9},
		{Currency: "AUD", At: ts("2026-09-11T10:00:00Z"), Fee: 3.1, Surcharge: 3.1},
		{Currency: "KES", At: ts("2026-09-11T10:00:00Z"), Fee: 50}, // other currency
		{Currency: "AUD", At: ts("2026-10-02T10:00:00Z"), Fee: 9},  // outside the period
	}
	for _, basis := range []string{Accrual, Cash} {
		s := Compute(in, sep, basis)
		if amt(s, "processing_fees") != 600 || amt(s, "surcharges") != 310 {
			t.Fatalf("%s: fees %d surcharges %d", basis, amt(s, "processing_fees"), amt(s, "surcharges"))
		}
		if s.Lines["processing_fees"].Section != SecExpenses || s.Lines["surcharges"].Section != SecIncome {
			t.Fatal("sections")
		}
		if tt := s.Totals(); tt.NetProfit != 310-600 {
			t.Fatalf("net %d", tt.NetProfit)
		}
	}
}

func TestCreditNotesReduceAccrualRevenue(t *testing.T) {
	in := base()
	in.Invoices = []Invoice{
		{ID: "i1", DocType: "invoice", Status: "sent", Currency: "AUD", IssueDate: d("2026-09-10"), Total: 1100, Tax: 100},
		{ID: "c1", DocType: "credit_note", Status: "open", Currency: "AUD", IssueDate: d("2026-09-11"), Total: 220, Tax: 20},
		{ID: "c2", DocType: "credit_note", Status: "void", Currency: "AUD", IssueDate: d("2026-09-12"), Total: 330, Tax: 30},
		{ID: "c3", DocType: "credit_note", Status: "open", Currency: "AUD", IssueDate: d("2026-10-02"), Total: 110, Tax: 10},
	}
	s := Compute(in, sep, Accrual)
	if got := amt(s, "invoices"); got != 80000 { // 1000 - 200
		t.Fatalf("invoices = %d, want 80000", got)
	}
	if s.TaxCollected != 8000 {
		t.Fatalf("tax collected = %d, want 8000", s.TaxCollected)
	}
	// Cash basis: a credit note is not money in or out.
	if got := amt(Compute(in, sep, Cash), "invoices"); got != 0 {
		t.Fatalf("cash invoices = %d, want 0", got)
	}
}
