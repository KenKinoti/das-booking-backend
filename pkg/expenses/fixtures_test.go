package expenses

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
)

// Realistic billing emails (structure and wording as sent by Google
// Workspace and Synergy Wholesale in 2026; amounts and ids are made up).

const googleInvoiceEmail = `Google Workspace

Your Google Workspace monthly invoice is available. Please find the PDF
document attached at the bottom of this email.

IMPORTANT: The balance will be automatically charged so you don't need to
take any action.

Domain example.com.au
Name EXAMPLE PTY LTD
Invoice number 5671877634
Payments profile ID 1234-5678-9012

If you want to view your payment history or update your payment info, visit
your account by following the instructions below:
Sign in to your Admin console at admin.google.com<https://admin.google.com>.

Google Workspace customer ID:
example.com.au

Google Cloud EMEA Limited, Velasco, Clanwilliam Place, Dublin 2, Ireland`

const googlePaymentEmail = `Google Workspace

Payment received

Your payment of €36.00 was applied to Google Workspace on Sep 29, 2026.

------------------------------------------------------------------------------------

Google Workspace customer ID:
example.com.au

Payments profile ID:
2125-8064-1364

Google LLC 1600 Amphitheatre Parkway, Mountain View, CA 94043`

const googleDeclinedEmail = `Google Workspace

Your financial institution declined a payment from your Visa  • • • • 4970
associated with the Google Workspace account for example.com.au on Sep
1, 2026. They provided the following reason:
Unfortunately, your account has insufficient funds for this transaction.`

const synergyReceiptEmail = `Payment Receipt for Invoice 2746402

Dear Kennedy Example,

This is a payment receipt for invoice 2746402

-----------------------------------------------------------

Sub Total: $45.00 AUD
Credit: $0.00 AUD
Total: $45.00 AUD

Amount: $45.00 AUD
Transaction #dwhyfsx5
Total Paid: $45.00 AUD
Remaining Balance: $45.00 AUD
Status: Paid

You may review your invoice history at any time by logging in to your
Synergy Wholesale account.

© 2026 Nexigen Digital Pty Ltd trading as Synergy
Wholesale`

const synergyLowBalanceEmail = `Synergy Wholesale - Low Balance Notification

Hi Kennedy,

Our system has detected that your account balance has now dropped below your set limit of $10.00. Your balance is currently -$58.83. We have`

const synergyStatementEmail = `September Transaction Statement

Hi Kennedy,

Your monthly transaction summary for September 2026 is now ready
and attached below.`

const synergyStatementCSV = "Date,Description,Reference,Debit,Credit,Balance\n" +
	"01/09/2026,Account top up,2746240,,20.00,20.00\n" +
	"02/09/2026,Domain Renewal - example.com.au (1 year),,14.95,,5.05\n" +
	"15/09/2026,cPanel Hosting - Economy (example.com.au),,9.90,,-4.85\n"

// Lines of a Google Workspace invoice PDF.
var googleInvoicePDFLines = []string{
	"Invoice", "Invoice number: 5671877634", "Invoice date: Aug 31, 2026",
	"Summary for Aug 1, 2026 - Aug 31, 2026",
	"Google Workspace Business Starter", "Subtotal in EUR €30.00", "VAT (20%) €6.00", "Total in EUR €36.00",
}

// Lines of a Synergy Wholesale (WHMCS-style) tax invoice PDF.
var synergyInvoicePDFLines = []string{
	"Tax Invoice", "Invoice #2746402", "Invoice Date: 30/09/2026", "Due Date: 30/09/2026",
	"Description Total", "Domain Renewal - example.com.au (1 Year/s) $40.91 AUD",
	"Sub Total $40.91 AUD", "10.00% GST $4.09 AUD", "Credit $0.00 AUD", "Total $45.00 AUD",
}

func pdfEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(s)
}

// makePDF builds a small valid PDF. cid=true writes the text with a 2-byte
// Identity-H style encoding and a ToUnicode CMap (like Google's PDFs), else
// plain WinAnsi literals. The content stream is Flate-compressed.
func makePDF(lines []string, cid bool) []byte {
	var content bytes.Buffer
	content.WriteString("BT /F1 10 Tf 50 780 Td 14 TL\n")
	codes := map[rune]int{}
	next := 3
	for i, l := range lines {
		if i > 0 {
			content.WriteString("T*\n")
		}
		if cid {
			var h strings.Builder
			for _, r := range l {
				c, ok := codes[r]
				if !ok {
					c = next
					next++
					codes[r] = c
				}
				fmt.Fprintf(&h, "%04X", c)
			}
			content.WriteString("<" + h.String() + "> Tj\n")
		} else {
			// WinAnsi: € is 0x80.
			l = strings.ReplaceAll(l, "€", "\x80")
			content.WriteString("(" + pdfEscape(l) + ") Tj\n")
		}
	}
	content.WriteString("ET\n")
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(content.Bytes())
	w.Close()

	var cmap bytes.Buffer
	if cid {
		cmap.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n/CMapName /Adobe-Identity-UCS def\n1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
		fmt.Fprintf(&cmap, "%d beginbfchar\n", len(codes))
		rs := make([]rune, 0, len(codes))
		for r := range codes {
			rs = append(rs, r)
		}
		sort.Slice(rs, func(i, j int) bool { return codes[rs[i]] < codes[rs[j]] })
		for _, r := range rs {
			c := codes[r]
			u := utf16.Encode([]rune{r})
			hx := ""
			for _, x := range u {
				hx += fmt.Sprintf("%04X", x)
			}
			fmt.Fprintf(&cmap, "<%04X> <%s>\n", c, hx)
		}
		cmap.WriteString("endbfchar\nendcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	}

	var b bytes.Buffer
	offs := []int{}
	obj := func(s string) {
		offs = append(offs, b.Len())
		b.WriteString(s)
	}
	b.WriteString("%PDF-1.5\n%\xe2\xe3\xcf\xd3\n")
	obj("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	obj("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	obj("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>\nendobj\n")
	obj(fmt.Sprintf("4 0 obj\n<< /Length %d /Filter /FlateDecode >>\nstream\n", z.Len()))
	b.Write(z.Bytes())
	b.WriteString("\nendstream\nendobj\n")
	if cid {
		obj("5 0 obj\n<< /Type /Font /Subtype /Type0 /BaseFont /ABCDEF+Roboto /Encoding /Identity-H /ToUnicode 6 0 R >>\nendobj\n")
		obj(fmt.Sprintf("6 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", cmap.Len(), cmap.String()))
	} else {
		obj("5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>\nendobj\n")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return b.Bytes()
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

var _ = hex.EncodeToString
