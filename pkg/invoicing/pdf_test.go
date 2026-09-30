package invoicing

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
)

func sampleInvoice(cur string, n int) *Invoice {
	inv := &Invoice{ID: "inv-1", OrganizationID: "org", DocType: TypeInvoice, Number: "INV-1005", Status: StatusSent,
		ClientName: "Wanjiku Traders", ClientEmail: "pay@wanjiku.test", ClientAddress: "Moi Avenue\nNairobi",
		IssueDate: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), DueDate: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
		Currency: cur, Notes: "Thanks for your business.", Terms: "Payment within 14 days.", PublicToken: "tok_abcdefghijklmnopqrstuvwxyz"}
	for i := 0; i < n; i++ {
		rate := 16.0
		if i%3 == 0 {
			rate = 8
		}
		inv.Items = append(inv.Items, InvoiceItem{Position: i, Description: fmt.Sprintf("Line %d — consulting work with a long description that wraps onto a second line in the table to check wrapping", i+1),
			Quantity: 2, UnitPrice: 1234.5, TaxRate: rate, TaxName: "VAT"})
	}
	Recalculate(inv)
	inv.Payments = []Payment{{Amount: 100, Date: inv.IssueDate, Method: "Flutterwave · M-Pesa", IsDeposit: false}}
	ApplyPayments(inv, inv.IssueDate)
	return inv
}

// pdfText extracts the (compressed) content streams of a PDF so tests can
// look for text drawn with the embedded font (glyph ids, not characters) or
// plain metadata.
func pdfStreams(t *testing.T, pdf []byte) string {
	t.Helper()
	var out strings.Builder
	re := regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	for _, m := range re.FindAllSubmatch(pdf, -1) {
		r, err := zlib.NewReader(bytes.NewReader(m[1]))
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(r)
		out.Write(b)
	}
	return out.String()
}

func pageCount(pdf []byte) int {
	return len(regexp.MustCompile(`/Type /Page\b`).FindAll(pdf, -1))
}

func TestRenderPDFBasics(t *testing.T) {
	inv := sampleInvoice("KES", 3)
	b := PDFBusiness{Name: "Mursa Supplies", Email: "hi@mursa.test", TaxNumber: "P051", TaxNumberLabel: "KRA PIN", Accent: "#0f766e", PaymentInstructions: "M-Pesa paybill 123456"}
	data, err := RenderPDF(inv, b, PDFOptions{PayURL: "https://app.test/pay/tok", ViewURL: "https://app.test/i/tok"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		t.Fatalf("not a PDF: %q", data[:10])
	}
	// The title (metadata, UTF-16) carries the number; links are plain.
	if !bytes.Contains(data, []byte("https://app.test/pay/tok")) || !bytes.Contains(data, []byte("https://app.test/i/tok")) {
		t.Fatal("pay / view links missing")
	}
	if !bytes.Contains(data, utf16be("Tax invoice INV-1005")) {
		t.Fatal("title with invoice number missing")
	}
	if pageCount(data) != 1 {
		t.Fatalf("short invoice should be 1 page, got %d", pageCount(data))
	}
	if !bytes.Contains(data, []byte("/FontFile2")) {
		t.Fatal("unicode font not embedded")
	}
}

func utf16be(s string) []byte {
	var b bytes.Buffer
	for _, r := range s {
		b.WriteByte(byte(r >> 8))
		b.WriteByte(byte(r))
	}
	return b.Bytes()
}

func TestRenderPDFLongItemsPaginate(t *testing.T) {
	inv := sampleInvoice("USD", 60)
	data, err := RenderPDF(inv, PDFBusiness{Name: "Acme"}, PDFOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n := pageCount(data); n < 3 {
		t.Fatalf("60 wrapped lines should need several pages, got %d", n)
	}
	// "Page N of M" footer: the total page alias is replaced.
	if bytes.Contains(data, []byte("{nb}")) {
		t.Fatal("page alias not replaced")
	}
	// One very long description (longer than a page) still renders.
	inv = sampleInvoice("USD", 1)
	inv.Items[0].Description = strings.Repeat("A very long line of text for the description. ", 400)
	if _, err := RenderPDF(inv, PDFBusiness{Name: "Acme"}, PDFOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestRenderPDFLogo(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 120, 40))
	for x := 0; x < 120; x++ {
		for y := 0; y < 40; y++ {
			img.Set(x, y, color.NRGBA{200, 30, 30, uint8(x * 2)})
		}
	}
	var pngBuf, jpgBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)
	_ = jpeg.Encode(&jpgBuf, img, nil)
	inv := sampleInvoice("NGN", 2)
	for name, logo := range map[string][]byte{"png": pngBuf.Bytes(), "jpeg": jpgBuf.Bytes()} {
		data, err := RenderPDF(inv, PDFBusiness{Name: "Acme", Logo: logo}, PDFOptions{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Contains(data, []byte("/Subtype /Image")) {
			t.Fatalf("%s logo not embedded", name)
		}
	}
	// Not an image (e.g. SVG bytes): no image, no error, business name used.
	data, err := RenderPDF(inv, PDFBusiness{Name: "Acme", Logo: []byte("<svg/>")}, PDFOptions{})
	if err != nil || bytes.Contains(data, []byte("/Subtype /Image")) {
		t.Fatalf("svg logo should fall back to the name: %v", err)
	}
}

func TestFormatMoneyDecimals(t *testing.T) {
	cases := map[string]string{
		"JPY": "¥1,234,568", "UGX": "USh 1,234,568", "KWD": "KWD 1,234,567.890", "KES": "KSh 1,234,567.89",
		"NGN": "₦1,234,567.89", "GHS": "GH₵1,234,567.89", "INR": "₹1,234,567.89", "EUR": "€1,234,567.89",
	}
	for cur, want := range cases {
		if got := FormatMoney(1234567.89, cur); got != want {
			t.Errorf("%s: got %q want %q", cur, got, want)
		}
	}
	if got := FormatMoney(-5, "USD"); got != "−$5.00" {
		t.Errorf("negative: %q", got)
	}
	if got := FormatMoney(999, "USD"); got != "$999.00" {
		t.Errorf("no separator: %q", got)
	}
	// Every currency renders in a PDF (the font has the symbols).
	for _, cur := range []string{"JPY", "UGX", "KWD", "NGN", "GHS", "INR", "EUR", "GBP", "KES"} {
		if _, err := RenderPDF(sampleInvoice(cur, 2), PDFBusiness{Name: "Ünïcødé Ltd"}, PDFOptions{}); err != nil {
			t.Fatalf("%s: %v", cur, err)
		}
	}
}

func TestTaxByRateAndFilename(t *testing.T) {
	inv := sampleInvoice("KES", 4)
	g := TaxByRate(inv, "VAT")
	if len(g) != 2 || g[0].Rate != 16 || g[1].Rate != 8 {
		t.Fatalf("groups %+v", g)
	}
	if PDFFilename(&Invoice{Number: "INV-1005"}) != "INV-1005.pdf" || PDFFilename(&Invoice{Number: "A/B 7"}) != "A-B-7.pdf" {
		t.Fatal("filename")
	}
}

func TestReceiptPDF(t *testing.T) {
	inv := sampleInvoice("KES", 2)
	data, err := RenderReceiptPDF(inv, inv.Payments[0], PDFBusiness{Name: "Acme"})
	if err != nil || !bytes.HasPrefix(data, []byte("%PDF-")) {
		t.Fatalf("receipt: %v", err)
	}
}
