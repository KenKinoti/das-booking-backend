package invoicing

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/*.golden")

// goldenDoc is a part-paid KES invoice with a discount, shipping, tax-inclusive
// prices, a deposit and seven lines (the email shows five).
func goldenDoc(docType, status string) *Invoice {
	d := func(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }
	inv := &Invoice{ID: "doc-1", OrganizationID: "org-1", DocType: docType, Number: "INV-1042", Status: status, ClientName: "Wanjiku & Sons",
		IssueDate: d("2026-09-01"), DueDate: d("2026-09-15"), Currency: "KES", PricesIncludeTax: true, DiscountType: "percent", DiscountValue: 5,
		ShippingAmount: 350, PublicToken: "tok"}
	if docType == TypeQuote {
		inv.Number = "QUO-1007"
	}
	for i, it := range []struct {
		d    string
		q, p float64
	}{{"Website design — home page\nand 3 inner pages", 1, 45000}, {"Hosting (12 months)", 12, 1500}, {"Domain", 1, 1800}, {"Logo refresh", 1, 12000},
		{"Copywriting", 6, 2500}, {"SEO setup", 1, 8000}, {"Training session", 2, 3500}} {
		inv.Items = append(inv.Items, InvoiceItem{ID: string(rune('a' + i)), Description: it.d, Quantity: it.q, UnitPrice: it.p, TaxRate: 16, TaxName: "VAT"})
	}
	if docType == TypeInvoice {
		inv.Payments = []Payment{{ID: "p1", Amount: 20000, Date: d("2026-09-02"), Method: "mobile_money", IsDeposit: true, Reference: "QJK7XY"}}
	}
	Recalculate(inv)
	ApplyPayments(inv, d("2026-09-02"))
	return inv
}

func goldenView(kind string, today time.Time) EmailView {
	docType := TypeInvoice
	if kind == KindQuote {
		docType = TypeQuote
	}
	inv := goldenDoc(docType, StatusSent)
	Derive(inv, today)
	b := &builtEmail{Kind: kind, Doc: inv, LogoURL: "https://api.example.com/api/v1/public/org-logo/org-1?v=3",
		Business: PDFBusiness{Name: "Dasyin Studio Ltd", Address: "Kenyatta Ave 12\nNairobi, Kenya", Email: "billing@dasyin.test", Phone: "+254 700 123456",
			Website: "https://dasyin.test", TaxNumber: "P051234567X", TaxNumberLabel: "KRA PIN", TaxLabel: "VAT"},
		ViewURL: "https://app.example.com/i/tok", Attachment: attachmentName(kind, inv)}
	if kind != KindQuote && kind != KindReceipt {
		b.PayURL = "https://app.example.com/pay/tok"
	}
	if kind == KindReceipt {
		p := inv.Payments[0]
		b.Payment = &p
	}
	s := &Settings{}
	subjT, msgT := defaultTemplates(s, inv, kind)
	vars := emailVars(inv, b.Business.Name, b.ViewURL, b.PayURL, b.Payment)
	b.Subject, _ = fillTemplate(subjT, vars)
	b.Message, _ = fillTemplate(msgT, vars)
	h := &Handler{Now: func() time.Time { return today }}
	v := h.emailView(inv, kind, b, "Questions? Just reply to this email — it goes to billing@dasyin.test.", "#0f766e")
	v.LogoW, v.LogoH = 400, 120
	return v
}

func attachmentName(kind string, inv *Invoice) (a mail.Attachment) {
	a.Filename = PDFFilename(inv)
	if kind == KindReceipt {
		a.Filename = "Receipt-" + a.Filename
	}
	a.Data = []byte("%PDF")
	return
}

func TestEmailGolden(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		today      string
	}{{"invoice", KindInvoice, "2026-09-03"}, {"quote", KindQuote, "2026-09-03"}, {"reminder_overdue", KindReminder, "2026-09-30"}, {"receipt", KindReceipt, "2026-09-03"}} {
		t.Run(tc.name, func(t *testing.T) {
			today, _ := time.Parse("2006-01-02", tc.today)
			htmlOut, text := RenderEmail(goldenView(tc.kind, today))
			for _, part := range []struct{ ext, got string }{{"html", htmlOut}, {"txt", text}} {
				if strings.Contains(part.got, "{{") {
					t.Fatalf("%s: unresolved placeholder", part.ext)
				}
				p := filepath.Join("testdata", "email_"+tc.name+"."+part.ext+".golden")
				if *updateGolden {
					if err := os.WriteFile(p, []byte(part.got), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("%v (run with -update)", err)
				}
				if string(want) != part.got {
					t.Fatalf("%s differs from %s (run go test -run Golden -update and review the diff)", part.ext, p)
				}
			}
			// The HTML and text show the same amounts.
			for _, a := range findAmounts(text, "KES") {
				if !strings.Contains(htmlOut, a.Text) {
					t.Fatalf("text shows %q, HTML doesn't", a.Text)
				}
			}
		})
	}
}
