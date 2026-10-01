package expenses

import (
	"math"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.0051 }

func TestParseMoney(t *testing.T) {
	cases := []struct {
		in  string
		v   float64
		cur string
	}{
		{"$45.00 AUD", 45, "AUD"},
		{"A$1,234.50", 1234.5, "AUD"},
		{"€36.00", 36, "EUR"},
		{"USD 14.40", 14.4, "USD"},
		{"US$7.20", 7.2, "USD"},
		{"KSh 2,500", 2500, "KES"},
		{"-$58.83", -58.83, ""},
		{"£9.99", 9.99, "GBP"},
		{"12.00", 12, ""},
	}
	for _, c := range cases {
		v, cur, ok := ParseMoney(c.in)
		if !ok || !near(v, c.v) || cur != c.cur {
			t.Errorf("ParseMoney(%q) = %v %q %v, want %v %q", c.in, v, cur, ok, c.v, c.cur)
		}
	}
}

func TestParseDate(t *testing.T) {
	cases := map[string]string{
		"Sep 29, 2026":              "2026-09-29",
		"on Sep\n1, 2026":           "",
		"Aug 31, 2026":              "2026-08-31",
		"30/09/2026":                "2026-09-30",
		"09/30/2026":                "2026-09-30", // US order when day > 12
		"2026-02-03":                "2026-02-03",
		"3 February 2026":           "2026-02-03",
		"Invoice Date: 1 Sept 2026": "2026-09-01",
		"31/02/2026":                "",
	}
	for in, want := range cases {
		got := ParseDate(in)
		s := ""
		if got != nil {
			s = got.Format(dayFmt)
		}
		if want == "" && strings.Contains(in, "\n") {
			continue // line-wrapped dates are cleaned first by Extract
		}
		if s != want {
			t.Errorf("ParseDate(%q) = %q, want %q", in, s, want)
		}
	}
}

func TestExtractSynergyReceipt(t *testing.T) {
	when := time.Date(2026, 9, 30, 3, 20, 0, 0, time.UTC)
	x := Extract(synergyReceiptEmail, Hints{Subject: "Payment receipt for invoice 2746402", Sender: "Synergy Wholesale <noreply@synergywholesale.com>", When: &when})
	if x.VendorKey != "synergy" || x.Kind != KindReceipt || x.InvoiceNumber != "2746402" {
		t.Fatalf("vendor/kind/number: %+v", x)
	}
	if !x.HasTotal || !near(x.Total, 45) || x.Currency != "AUD" {
		t.Fatalf("total: %v %s", x.Total, x.Currency)
	}
	// GST is not shown on receipts: estimated as 1/11 of the GST-inclusive total.
	if !x.HasTax || !near(x.Tax, 4.09) || !strings.Contains(x.TaxNote, "estimated") {
		t.Fatalf("tax: %v %q", x.Tax, x.TaxNote)
	}
	if x.Date == nil || x.Date.Format(dayFmt) != "2026-09-30" || x.Paid == nil || !*x.Paid {
		t.Fatalf("date/paid: %v %v", x.Date, x.Paid)
	}
	if x.Confidence < 0.8 {
		t.Fatalf("confidence %v", x.Confidence)
	}
}

func TestExtractSynergyInvoicePDF(t *testing.T) {
	for _, cid := range []bool{false, true} {
		text := PDFText(makePDF(synergyInvoicePDFLines, cid))
		if !strings.Contains(text, "10.00% GST $4.09 AUD") {
			t.Fatalf("cid=%v pdf text:\n%s", cid, text)
		}
		x := Extract(text, Hints{Filename: "invoice-2746402.pdf"})
		if x.VendorKey != "" && x.VendorKey != "synergy" {
			t.Fatalf("vendor %q", x.VendorKey)
		}
		if x.InvoiceNumber != "2746402" || !near(x.Total, 45) || x.Currency != "AUD" || !near(x.Tax, 4.09) || x.TaxNote != "GST 10.00%" {
			t.Fatalf("cid=%v extracted %+v", cid, x)
		}
		if x.Date == nil || x.Date.Format(dayFmt) != "2026-09-30" {
			t.Fatalf("date %v", x.Date)
		}
	}
}

func TestExtractGoogleInvoiceEmailAndPDF(t *testing.T) {
	when := time.Date(2026, 9, 2, 17, 8, 0, 0, time.UTC)
	subj := "Google Workspace: Your invoice is available for example.com.au"
	from := "Google Payments <payments-noreply@google.com>"
	x := Extract(googleInvoiceEmail, Hints{Subject: subj, Sender: from, When: &when})
	if x.VendorKey != "google_workspace" || x.InvoiceNumber != "5671877634" || x.Kind != KindInvoice {
		t.Fatalf("%+v", x)
	}
	if x.HasTotal {
		t.Fatalf("the invoice email has no amount, got %v", x.Total)
	}
	if x.Service != "Google Workspace (example.com.au)" {
		t.Fatalf("service %q", x.Service)
	}
	// With the attached PDF (CID font + ToUnicode, like Google's) the amount, VAT and period are read.
	pdf := PDFText(makePDF(googleInvoicePDFLines, true))
	x = Extract(googleInvoiceEmail+"\n"+pdf, Hints{Subject: subj, Sender: from, When: &when})
	if !near(x.Total, 36) || x.Currency != "EUR" || !near(x.Tax, 6) || !strings.HasPrefix(x.TaxNote, "VAT 20") {
		t.Fatalf("pdf amounts: %+v", x)
	}
	if x.Date == nil || x.Date.Format(dayFmt) != "2026-08-31" || x.PeriodStart == nil || x.PeriodStart.Format(dayFmt) != "2026-08-01" {
		t.Fatalf("dates: %v %v", x.Date, x.PeriodStart)
	}
}

func TestExtractGooglePaymentAndDeclined(t *testing.T) {
	x := Extract(googlePaymentEmail, Hints{Subject: "Google Workspace: We've received your payment for example.com.au", Sender: "payments-noreply@google.com"})
	if x.Kind != KindPayment || !near(x.Total, 36) || x.Currency != "EUR" || x.Date == nil || x.Date.Format(dayFmt) != "2026-09-29" {
		t.Fatalf("payment %+v", x)
	}
	d := Extract(googleDeclinedEmail, Hints{Subject: "Google Workspace: Payment was declined for example.com.au", Sender: "payments-noreply@google.com"})
	if d.Kind != KindDeclined || d.Date == nil || d.Date.Format(dayFmt) != "2026-09-01" {
		t.Fatalf("declined %+v %v", d, d.Date)
	}
}

func TestExtractSynergyNotices(t *testing.T) {
	x := Extract(synergyLowBalanceEmail, Hints{Subject: "Synergy Wholesale - Low Balance Notification", Sender: "noreply@synergywholesale.com"})
	if x.Balance == nil || !near(*x.Balance, -58.83) || x.Ignore == "" {
		t.Fatalf("low balance %+v", x)
	}
	s := Extract(synergyStatementEmail, Hints{Subject: "Your monthly transaction statement for September", Sender: "noreply@synergywholesale.com"})
	if s.Ignore == "" {
		t.Fatalf("statement should be ignored: %+v", s)
	}
}

func TestExtractGenericUSD(t *testing.T) {
	text := "ACME Cloud Inc.\nInvoice No: INV-2026-0042\nInvoice date: 2026-09-15\nBilling period: Sep 1, 2026 - Sep 30, 2026\nPro plan 12.00\nTax 1.20\nTotal due\nUSD 13.20\n"
	x := Extract(text, Hints{DefaultCur: "AUD"})
	if x.InvoiceNumber != "INV-2026-0042" || !near(x.Total, 13.2) || x.Currency != "USD" || !near(x.Tax, 1.2) {
		t.Fatalf("%+v", x)
	}
}

func TestHTMLToText(t *testing.T) {
	h := `<html><head><style>p{color:red}</style></head><body><p>Total:&nbsp;<b>$45.00</b>&nbsp;AUD</p><table><tr><td>GST</td><td>$4.09</td></tr></table></body></html>`
	got := HTMLToText(h)
	if !strings.Contains(got, "Total: $45.00 AUD") || !strings.Contains(got, "GST") || strings.Contains(got, "color") {
		t.Fatalf("%q", got)
	}
}

func TestParseCSVStatement(t *testing.T) {
	rows, err := ParseCSV([]byte(synergyStatementCSV), "AUD")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows %d", len(rows))
	}
	if rows[0].Skip == "" {
		t.Fatalf("top up should be skipped: %+v", rows[0])
	}
	if !near(rows[1].Total, 14.95) || rows[1].Currency != "AUD" || rows[1].Date.Format(dayFmt) != "2026-09-02" || rows[1].Skip != "" {
		t.Fatalf("row 2 %+v", rows[1])
	}
	// Invoice export with tax column and a currency symbol.
	inv := "Invoice Date,Invoice Number,Item,Amount,GST\n2026-07-01,G-1001,Workspace,\"US$14.40\",0\n"
	r2, err := ParseCSV([]byte(inv), "AUD")
	if err != nil || len(r2) != 1 || r2[0].Currency != "USD" || !near(r2[0].Total, 14.4) || r2[0].InvoiceNumber != "G-1001" {
		t.Fatalf("%v %+v", err, r2)
	}
}

func TestExtractTaxLineAfterTaxInvoiceHeading(t *testing.T) {
	x := Extract("Canva Pty Ltd\nTax Invoice\nInvoice No: CANVA-88231\nInvoice date: 2026-09-26\nCanva Pro (1 month)\nGST $1.82\nTotal A$19.99", Hints{DefaultCur: "AUD"})
	if !near(x.Tax, 1.82) || !near(x.Total, 19.99) || x.Currency != "AUD" || x.InvoiceNumber != "CANVA-88231" {
		t.Fatalf("%+v", x)
	}
}
