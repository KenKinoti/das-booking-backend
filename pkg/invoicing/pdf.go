package invoicing

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	_ "image/jpeg" // logo decoders
	"image/png"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// DejaVu Sans (Bitstream Vera derivative, free licence — fonts/LICENSE-DejaVu.txt)
// covers every currency symbol the platform uses (₦ ₵ ₹ € £ ¥ …).
//
//go:embed fonts/DejaVuSans.ttf
var fontRegular []byte

//go:embed fonts/DejaVuSans-Bold.ttf
var fontBold []byte

// PDFBusiness is the "from" block of a document.
type PDFBusiness struct {
	Name, Address, Email, Phone, Website string
	TaxNumber, TaxNumberLabel, TaxLabel  string
	PaymentInstructions, Accent          string
	// Logo is a PNG or JPEG image (SVG/WebP logos are not embedded: the
	// business name is shown instead).
	Logo []byte
}

// PDFOptions are the links printed on a document.
type PDFOptions struct {
	PayURL  string // "Pay online" link (invoices with a balance, payments enabled)
	ViewURL string // the public link
}

// ---------------------------------------------------------------- money

var currencySymbols = map[string]string{
	"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥", "CHF": "CHF ", "CAD": "CA$", "AUD": "A$", "NZD": "NZ$",
	"CNY": "CN¥", "INR": "₹", "SGD": "S$", "HKD": "HK$", "AED": "AED ", "SAR": "SAR ", "KWD": "KWD ",
	"ZAR": "R ", "NGN": "₦", "EGP": "E£", "KES": "KSh ", "GHS": "GH₵", "MAD": "MAD ", "TZS": "TSh ", "UGX": "USh ",
	"RWF": "RF ", "ZMW": "ZK ", "XOF": "CFA ", "XAF": "FCFA ",
}

// FormatMoney formats v in cur with the currency's symbol, thousands
// separators and its number of decimals (JPY/UGX 0, KWD 3), e.g. "KSh 1,250.00".
func FormatMoney(v float64, cur string) string {
	cur = strings.ToUpper(strings.TrimSpace(cur))
	d := currency.Decimals(cur)
	neg := v < 0
	s := strconv.FormatFloat(math.Abs(roundCur(v, cur)), 'f', d, 64)
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	var b strings.Builder
	for i, r := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	sym, ok := currencySymbols[cur]
	if !ok {
		sym = cur + " "
	}
	out := sym + b.String() + frac
	if neg {
		return "−" + out
	}
	return out
}

// PDFFilename is the attachment / download name, e.g. "INV-1005.pdf".
func PDFFilename(inv *Invoice) string {
	n := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(inv.Number, "-")
	n = strings.Trim(n, "-.")
	if n == "" {
		n = docLabel(inv.DocType)
	}
	return n + ".pdf"
}

// ---------------------------------------------------------------- helpers

type rgb struct{ r, g, b int }

func parseHex(s string, def rgb) rgb {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return def
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return def
	}
	return rgb{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}
}

func tint(c rgb, f float64) rgb {
	m := func(x int) int { return int(math.Round(float64(x) + (255-float64(x))*f)) }
	return rgb{m(c.r), m(c.g), m(c.b)}
}

var (
	cInk   = rgb{26, 29, 46}
	cMuted = rgb{110, 118, 140}
	cLine  = rgb{226, 229, 238}
	cHead  = rgb{246, 247, 251}
)

// logoPNG returns the logo re-encoded as a plain PNG (fpdf cannot read
// interlaced / 16-bit PNGs), with its pixel size, or nil when it is not a
// raster image we can embed.
func logoPNG(data []byte) ([]byte, int, int) {
	if len(data) == 0 {
		return nil, 0, 0
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, 0, 0
	}
	b := img.Bounds()
	return buf.Bytes(), b.Dx(), b.Dy()
}

func pdfDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2 Jan 2006")
}

func qtyText(q float64) string {
	if q == math.Trunc(q) {
		return strconv.FormatFloat(q, 'f', 0, 64)
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(q, 'f', 3, 64), "0"), ".")
}

func pctText(p float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(p, 'f', 2, 64), "0"), ".") + "%"
}

// taxGroup is the tax charged at one rate.
type taxGroup struct {
	Label string
	Rate  float64
	Tax   float64
}

// TaxByRate groups line tax by name and rate, highest rate first.
func TaxByRate(inv *Invoice, defLabel string) []taxGroup {
	m := map[string]*taxGroup{}
	for _, it := range inv.Items {
		if it.TaxRate <= 0 {
			continue
		}
		name := strings.TrimSpace(it.TaxName)
		if name == "" {
			name = defLabel
		}
		if name == "" {
			name = "Tax"
		}
		k := fmt.Sprintf("%s|%g", name, it.TaxRate)
		g := m[k]
		if g == nil {
			g = &taxGroup{Label: name, Rate: it.TaxRate}
			m[k] = g
		}
		g.Tax += it.LineTax
	}
	out := []taxGroup{}
	for _, g := range m {
		g.Tax = roundCur(g.Tax, inv.Currency)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rate != out[j].Rate {
			return out[i].Rate > out[j].Rate
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// ---------------------------------------------------------------- document

type pdfDoc struct {
	*fpdf.Fpdf
	accent rgb
	cur    string
	left   float64
	right  float64 // x of the right margin
	width  float64
}

const (
	pdfMargin = 16.0
	pdfFoot   = 16.0
)

func newPDF(title, author, accent, cur, footerLeft string) *pdfDoc {
	f := fpdf.New("P", "mm", "A4", "")
	f.AddUTF8FontFromBytes("dv", "", fontRegular)
	f.AddUTF8FontFromBytes("dv", "B", fontBold)
	f.SetMargins(pdfMargin, pdfMargin+4, pdfMargin)
	f.SetAutoPageBreak(false, pdfFoot)
	f.SetTitle(title, true)
	f.SetAuthor(author, true)
	f.SetCreator("DASYIN ERP", true)
	f.AliasNbPages("{nb}")
	d := &pdfDoc{Fpdf: f, accent: parseHex(accent, rgb{79, 70, 229}), cur: cur}
	pw, _ := f.GetPageSize()
	d.left, d.right, d.width = pdfMargin, pw-pdfMargin, pw-2*pdfMargin
	f.SetHeaderFunc(func() {
		d.fill(d.accent)
		f.Rect(0, 0, pw, 3, "F")
	})
	f.SetFooterFunc(func() {
		_, ph := f.GetPageSize()
		f.SetY(ph - 11)
		d.font("", 8, cMuted)
		f.SetX(d.left)
		f.CellFormat(d.width/2, 5, footerLeft, "", 0, "L", false, 0, "")
		f.CellFormat(d.width/2, 5, fmt.Sprintf("Page %d of {nb}", f.PageNo()), "", 0, "R", false, 0, "")
	})
	return d
}

func (d *pdfDoc) font(style string, size float64, c rgb) {
	d.SetFont("dv", style, size)
	d.SetTextColor(c.r, c.g, c.b)
}

func (d *pdfDoc) fill(c rgb) { d.SetFillColor(c.r, c.g, c.b) }
func (d *pdfDoc) draw(c rgb) { d.SetDrawColor(c.r, c.g, c.b) }

func (d *pdfDoc) money(v float64) string { return FormatMoney(v, d.cur) }

func (d *pdfDoc) bottom() float64 {
	_, ph := d.GetPageSize()
	return ph - pdfFoot - 4
}

// ensure starts a new page when h millimetres don't fit.
func (d *pdfDoc) ensure(h float64) bool {
	if d.GetY()+h > d.bottom() {
		d.AddPage()
		return true
	}
	return false
}

// lines wraps text to width w (explicit newlines kept).
func (d *pdfDoc) lines(text string, w float64) []string {
	out := []string{}
	for _, para := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(para) == "" {
			out = append(out, "")
			continue
		}
		out = append(out, d.SplitText(para, w)...)
	}
	return out
}

// para writes a labelled block of wrapped text, breaking across pages.
func (d *pdfDoc) para(label, text string, x, w float64) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	d.ensure(12)
	d.SetXY(x, d.GetY())
	d.font("B", 7.5, cMuted)
	d.CellFormat(w, 5, strings.ToUpper(label), "", 2, "L", false, 0, "")
	d.font("", 9, cInk)
	for _, l := range d.lines(text, w) {
		d.ensure(4.6)
		d.SetX(x)
		d.CellFormat(w, 4.6, l, "", 2, "L", false, 0, "")
	}
	d.Ln(3)
}

// header draws the business block, logo and title; returns the y below it.
func (d *pdfDoc) header(b PDFBusiness, title, number, stamp string) {
	y := d.GetY()
	x := d.left
	leftW := d.width * 0.58
	hasLogo := false
	if data, pw, ph := logoPNG(b.Logo); data != nil && pw > 0 && ph > 0 {
		opt := fpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}
		d.RegisterImageOptionsReader("logo", opt, bytes.NewReader(data))
		if d.Ok() {
			maxW, maxH := 55.0, 18.0
			w := maxW
			h := w * float64(ph) / float64(pw)
			if h > maxH {
				h = maxH
				w = h * float64(pw) / float64(ph)
			}
			d.ImageOptions("logo", x, y, w, h, false, opt, 0, "")
			y += h + 4
			hasLogo = true
		}
	}
	d.SetXY(x, y)
	name := b.Name
	if name == "" {
		name = "Your business"
	}
	if !hasLogo {
		d.font("B", 16, cInk)
		for _, l := range d.lines(name, leftW) {
			d.SetX(x)
			d.CellFormat(leftW, 7, l, "", 2, "L", false, 0, "")
		}
	} else {
		d.font("B", 11, cInk)
		d.SetX(x)
		d.CellFormat(leftW, 5.5, name, "", 2, "L", false, 0, "")
	}
	d.font("", 8.5, cMuted)
	details := []string{}
	if b.Address != "" {
		details = append(details, d.lines(b.Address, leftW)...)
	}
	for _, s := range []string{b.Email, b.Phone, b.Website} {
		if strings.TrimSpace(s) != "" {
			details = append(details, strings.TrimSpace(s))
		}
	}
	if b.TaxNumber != "" {
		lbl := b.TaxNumberLabel
		if lbl == "" {
			lbl = "Tax no."
		}
		details = append(details, lbl+" "+b.TaxNumber)
	}
	for _, l := range details {
		d.SetX(x)
		d.CellFormat(leftW, 4.2, l, "", 2, "L", false, 0, "")
	}
	leftBottom := d.GetY()

	// Title block (right)
	rw := d.width - leftW
	rx := d.right - rw
	d.SetXY(rx, pdfMargin+4)
	d.font("B", 22, cInk)
	d.CellFormat(rw, 10, title, "", 2, "R", false, 0, "")
	d.SetX(rx)
	d.font("", 11, cMuted)
	d.CellFormat(rw, 6, number, "", 2, "R", false, 0, "")
	if stamp != "" {
		col := map[string]rgb{"PAID": {22, 163, 74}, "OVERDUE": {220, 38, 38}, "VOID": {110, 118, 140}, "ACCEPTED": {22, 163, 74}, "DRAFT": {110, 118, 140}, "PARTIALLY PAID": {217, 119, 6}, "PAID IN FULL": {22, 163, 74}, "CANCELLED": {220, 38, 38}}[stamp]
		d.font("B", 9, col)
		w := d.GetStringWidth(stamp) + 6
		d.Ln(2)
		sy := d.GetY()
		d.draw(col)
		d.SetLineWidth(0.5)
		d.Rect(d.right-w, sy, w, 7, "D")
		d.SetXY(d.right-w, sy)
		d.CellFormat(w, 7, stamp, "", 2, "C", false, 0, "")
		d.SetLineWidth(0.2)
	}
	d.SetY(math.Max(leftBottom, d.GetY()) + 6)
}

// kv writes "label ... value" right-aligned rows in [x, x+w].
func (d *pdfDoc) kv(x, w float64, label, value string, bold bool, c rgb) {
	st := ""
	size := 9.0
	if bold {
		st = "B"
		size = 10
	}
	d.SetX(x)
	d.font(st, size, cMuted)
	if bold {
		d.font(st, size, cInk)
	}
	d.CellFormat(w*0.55, 6, label, "", 0, "L", false, 0, "")
	d.font(st, size, c)
	d.CellFormat(w*0.45, 6, value, "", 1, "R", false, 0, "")
}

func stampFor(inv *Invoice) string {
	s := inv.DisplayStatus
	if s == "" {
		s = inv.Status
	}
	switch s {
	case StatusPaid, StatusOverdue, StatusVoid, StatusAccepted, StatusDraft:
		return strings.ToUpper(s)
	case StatusPartial:
		return "PARTIALLY PAID"
	}
	return ""
}

// DocTitle is the heading of the document ("Tax invoice" when the business
// shows a tax number, like the on-screen document).
func DocTitle(inv *Invoice, b PDFBusiness) string {
	if inv.DocType == TypeQuote {
		return "Quote"
	}
	if b.TaxNumber != "" {
		return "Tax invoice"
	}
	return "Invoice"
}

// RenderPDF renders an invoice or quote as a PDF, laid out like the
// on-screen document.
func RenderPDF(inv *Invoice, b PDFBusiness, opt PDFOptions) ([]byte, error) {
	isQuote := inv.DocType == TypeQuote
	title := DocTitle(inv, b)
	d := newPDF(title+" "+inv.Number, b.Name, b.Accent, inv.Currency, inv.Number+(map[bool]string{true: " · " + b.Name, false: ""}[b.Name != ""]))
	d.AddPage()
	d.header(b, title, inv.Number, stampFor(inv))

	// Bill to + dates
	y := d.GetY()
	colW := d.width * 0.5
	d.SetXY(d.left, y)
	d.font("B", 7.5, cMuted)
	d.CellFormat(colW, 5, "BILL TO", "", 2, "L", false, 0, "")
	d.font("B", 10.5, cInk)
	for _, l := range d.lines(inv.ClientName, colW-4) {
		d.SetX(d.left)
		d.CellFormat(colW, 5.5, l, "", 2, "L", false, 0, "")
	}
	d.font("", 9, cMuted)
	var client []string
	if inv.ClientAddress != "" {
		client = append(client, d.lines(inv.ClientAddress, colW-4)...)
	}
	for _, s := range []string{inv.ClientEmail, inv.ClientPhone} {
		if s != "" {
			client = append(client, s)
		}
	}
	if inv.ClientTaxNumber != "" {
		lbl := b.TaxNumberLabel
		if lbl == "" {
			lbl = "Tax no."
		}
		client = append(client, lbl+" "+inv.ClientTaxNumber)
	}
	for _, l := range client {
		d.SetX(d.left)
		d.CellFormat(colW, 4.4, l, "", 2, "L", false, 0, "")
	}
	leftBottom := d.GetY()

	rx := d.left + colW + 6
	rw := d.right - rx
	d.SetXY(rx, y)
	due := "Due date"
	if isQuote {
		due = "Valid until"
	}
	d.kv(rx, rw, "Issue date", pdfDate(inv.IssueDate), false, cInk)
	d.kv(rx, rw, due, pdfDate(inv.DueDate), false, cInk)
	if inv.Reference != "" {
		d.kv(rx, rw, "Reference", inv.Reference, false, cInk)
	}
	// Amount due box
	d.Ln(2)
	by := d.GetY()
	d.fill(tint(d.accent, 0.9))
	d.Rect(rx, by, rw, 12, "F")
	d.SetXY(rx+3, by)
	lbl, amt := "Amount due", inv.BalanceDue
	if isQuote {
		lbl, amt = "Total", inv.Total
	}
	d.font("B", 9, cInk)
	d.CellFormat(rw*0.45, 12, lbl, "", 0, "L", false, 0, "")
	d.font("B", 13, d.accent)
	d.CellFormat(rw*0.55-6, 12, d.money(amt), "", 0, "R", false, 0, "")
	d.SetY(math.Max(leftBottom, by+12) + 7)

	if inv.Title != "" {
		d.font("B", 12, cInk)
		for _, l := range d.lines(inv.Title, d.width) {
			d.SetX(d.left)
			d.CellFormat(d.width, 6, l, "", 2, "L", false, 0, "")
		}
		d.Ln(2)
	}

	d.itemsTable(inv)
	d.totals(inv, b)

	// Notes, payment instructions, pay link, terms
	d.Ln(2)
	d.para("Notes", inv.Notes, d.left, d.width)
	if !isQuote {
		d.para("How to pay", b.PaymentInstructions, d.left, d.width)
	}
	if !isQuote && opt.PayURL != "" && inv.BalanceDue > 0 && inv.Status != StatusVoid {
		d.ensure(16)
		d.payBox(opt.PayURL, inv.BalanceDue)
	}
	if opt.ViewURL != "" {
		d.ensure(10)
		d.SetX(d.left)
		d.font("", 8.5, cMuted)
		d.CellFormat(d.GetStringWidth("View online: ")+1, 5, "View online: ", "", 0, "L", false, 0, "")
		d.font("", 8.5, d.accent)
		d.CellFormat(0, 5, opt.ViewURL, "", 1, "L", false, 0, opt.ViewURL)
		d.Ln(2)
	}
	d.para("Terms", inv.Terms, d.left, d.width)

	var buf bytes.Buffer
	if err := d.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (d *pdfDoc) payBox(url string, bal float64) {
	y := d.GetY()
	h := 14.0
	d.fill(tint(d.accent, 0.9))
	d.Rect(d.left, y, d.width, h, "F")
	btnW := 34.0
	d.fill(d.accent)
	d.Rect(d.right-btnW-3, y+3, btnW, h-6, "F")
	d.SetXY(d.right-btnW-3, y+3)
	d.font("B", 10, rgb{255, 255, 255})
	d.CellFormat(btnW, h-6, "Pay online", "", 0, "C", false, 0, url)
	d.LinkString(d.right-btnW-3, y+3, btnW, h-6, url)
	d.SetXY(d.left+4, y+2)
	d.font("B", 9.5, cInk)
	d.CellFormat(d.width-btnW-12, 5, "Pay "+d.money(bal)+" online — card, mobile money or bank", "", 2, "L", false, 0, "")
	d.SetX(d.left + 4)
	d.font("", 8, d.accent)
	u := url
	for d.GetStringWidth(u) > d.width-btnW-14 && len(u) > 10 {
		u = u[:len(u)-4]
	}
	if u != url {
		u += "…"
	}
	d.CellFormat(d.width-btnW-12, 5, u, "", 0, "L", false, 0, url)
	d.SetY(y + h + 5)
}

type col struct {
	title string
	w     float64
	align string
}

func (d *pdfDoc) itemsTable(inv *Invoice) {
	hasDisc, hasTax := false, false
	for _, it := range inv.Items {
		if it.DiscountPercent > 0 {
			hasDisc = true
		}
		if it.TaxRate > 0 {
			hasTax = true
		}
	}
	cols := []col{{"Description", 0, "L"}, {"Qty", 18, "R"}, {"Price", 28, "R"}}
	if hasDisc {
		cols = append(cols, col{"Disc.", 15, "R"})
	}
	if hasTax {
		cols = append(cols, col{"Tax", 15, "R"})
	}
	cols = append(cols, col{"Amount", 30, "R"})
	fixed := 0.0
	for _, c := range cols[1:] {
		fixed += c.w
	}
	cols[0].w = d.width - fixed
	head := func() {
		d.fill(cHead)
		d.Rect(d.left, d.GetY(), d.width, 8, "F")
		d.font("B", 8, cMuted)
		d.SetX(d.left)
		for i, c := range cols {
			pad := ""
			if i == 0 {
				pad = " "
			}
			d.CellFormat(c.w, 8, pad+strings.ToUpper(c.title), "", 0, c.align, false, 0, "")
		}
		d.Ln(8)
	}
	head()
	lh := 4.6
	for _, it := range inv.Items {
		d.font("", 9, cInk)
		desc := d.lines(it.Description, cols[0].w-4)
		if len(desc) == 0 {
			desc = []string{""}
		}
		qty := qtyText(it.Quantity)
		if it.Unit != "" {
			qty += " " + it.Unit
		}
		vals := []string{"", qty, d.money(it.UnitPrice)}
		if hasDisc {
			v := "—"
			if it.DiscountPercent > 0 {
				v = pctText(it.DiscountPercent)
			}
			vals = append(vals, v)
		}
		if hasTax {
			v := "—"
			if it.TaxRate > 0 {
				v = pctText(it.TaxRate)
			}
			vals = append(vals, v)
		}
		vals = append(vals, d.money(it.LineTotal))
		// A long description continues on the next page, with the header.
		first := true
		for len(desc) > 0 {
			fit := int((d.bottom() - d.GetY() - 3) / lh)
			if fit < 1 || (first && fit < len(desc) && fit < 3) {
				d.AddPage()
				head()
				continue
			}
			n := len(desc)
			if n > fit {
				n = fit
			}
			y := d.GetY() + 1.5
			for i := 0; i < n; i++ {
				d.SetXY(d.left+2, y+float64(i)*lh)
				d.font("", 9, cInk)
				d.CellFormat(cols[0].w-2, lh, desc[i], "", 0, "L", false, 0, "")
			}
			if first {
				x := d.left + cols[0].w
				for i := 1; i < len(cols); i++ {
					d.SetXY(x, y)
					d.font(map[bool]string{true: "B", false: ""}[i == len(cols)-1], 9, cInk)
					d.CellFormat(cols[i].w, lh, vals[i], "", 0, cols[i].align, false, 0, "")
					x += cols[i].w
				}
			}
			desc = desc[n:]
			first = false
			d.SetY(y + float64(n)*lh + 1.5)
			if len(desc) == 0 {
				d.draw(cLine)
				d.Line(d.left, d.GetY(), d.right, d.GetY())
			}
		}
	}
	d.Ln(3)
}

func (d *pdfDoc) totals(inv *Invoice, b PDFBusiness) {
	w := 82.0
	x := d.right - w
	type row struct {
		label, value string
		bold         bool
		color        rgb
	}
	rows := []row{{"Subtotal", d.money(inv.Subtotal), false, cInk}}
	if inv.DiscountTotal > 0 {
		rows = append(rows, row{"Discount", "−" + d.money(inv.DiscountTotal), false, cInk})
	}
	if inv.ShippingAmount > 0 {
		rows = append(rows, row{"Shipping", d.money(inv.ShippingAmount), false, cInk})
	}
	incl := ""
	if inv.PricesIncludeTax {
		incl = " (included)"
	}
	groups := TaxByRate(inv, b.TaxLabel)
	for _, g := range groups {
		rows = append(rows, row{fmt.Sprintf("%s %s%s", g.Label, pctText(g.Rate), incl), d.money(g.Tax), false, cInk})
	}
	if len(groups) == 0 && inv.TaxTotal > 0 {
		lbl := b.TaxLabel
		if lbl == "" {
			lbl = "Tax"
		}
		rows = append(rows, row{lbl + incl, d.money(inv.TaxTotal), false, cInk})
	}
	rows = append(rows, row{"Total " + inv.Currency, d.money(inv.Total), true, cInk})
	if inv.DocType == TypeInvoice && inv.AmountPaid > 0 {
		dep := 0.0
		for _, p := range inv.Payments {
			if p.IsDeposit {
				dep += p.Amount
			}
		}
		if dep > 0 {
			rows = append(rows, row{"Deposit received", "−" + d.money(dep), false, cInk})
		}
		if other := inv.AmountPaid - dep; other > 0.0001 {
			lbl := "Paid"
			if dep > 0 {
				lbl = "Paid since"
			}
			rows = append(rows, row{lbl, "−" + d.money(other), false, cInk})
		}
		rows = append(rows, row{"Balance due", d.money(inv.BalanceDue), true, d.accent})
	}
	d.ensure(float64(len(rows))*6 + 6)
	for _, r := range rows {
		if r.bold {
			d.draw(cLine)
			d.Line(x, d.GetY()+0.5, d.right, d.GetY()+0.5)
			d.Ln(1)
		}
		d.kv(x, w, r.label, r.value, r.bold, r.color)
	}
	// Payments received (dates and methods), under the totals.
	if inv.DocType == TypeInvoice && len(inv.Payments) > 0 {
		d.Ln(2)
		d.font("B", 7.5, cMuted)
		d.ensure(10)
		d.SetX(d.left)
		d.CellFormat(d.width, 5, "PAYMENTS RECEIVED", "", 2, "L", false, 0, "")
		for _, p := range inv.Payments {
			d.ensure(5)
			d.SetX(d.left)
			d.font("", 8.5, cInk)
			label := pdfDate(p.Date) + " · " + PaymentMethodLabel(p.Method)
			if p.IsDeposit {
				label += " · deposit"
			}
			if p.Reference != "" {
				label += " · " + p.Reference
			}
			d.CellFormat(d.width-40, 4.6, label, "", 0, "L", false, 0, "")
			d.CellFormat(40, 4.6, d.money(p.Amount), "", 1, "R", false, 0, "")
		}
	}
	d.Ln(3)
}

// PaymentMethodLabel is the human name of a stored payment method.
func PaymentMethodLabel(m string) string {
	switch m {
	case "bank_transfer":
		return "Bank transfer"
	case "card":
		return "Card"
	case "cash":
		return "Cash"
	case "mobile_money":
		return "Mobile money"
	case "cheque":
		return "Cheque"
	case "flutterwave":
		return "Flutterwave"
	case "paypal":
		return "PayPal"
	case "stripe":
		return "Stripe"
	case "other":
		return "Other"
	}
	return m
}

// ReceiptOptions mark a receipt cancelled (its payment was removed).
type ReceiptOptions struct {
	CancelledAt  *time.Time
	CancelReason string
}

// RenderReceiptPDF renders the receipt for one payment on an invoice.
func RenderReceiptPDF(inv *Invoice, p Payment, b PDFBusiness) ([]byte, error) {
	return RenderReceipt(inv, p, b, ReceiptOptions{})
}

// RenderReceipt renders a payment receipt: receipt number, date, who paid,
// the amount in figures and words, method / reference, the invoice and what
// was paid and left to pay as at this payment, notes, and a PAID IN FULL,
// CANCELLED or "Revised" mark.
func RenderReceipt(inv *Invoice, p Payment, b PDFBusiness, o ReceiptOptions) ([]byte, error) {
	number := p.ReceiptNumber
	if number == "" {
		number = "For " + inv.Number
	}
	f := figuresAt(inv, p)
	footer := "Receipt " + p.ReceiptNumber + " · " + inv.Number
	if p.ReceiptNumber == "" {
		footer = "Receipt for " + inv.Number
	}
	d := newPDF("Receipt "+strings.TrimPrefix(number, "For "), b.Name, b.Accent, inv.Currency, footer)
	d.AddPage()
	stamp := ""
	switch {
	case o.CancelledAt != nil:
		stamp = "CANCELLED"
	case f.Balance <= 0:
		stamp = "PAID IN FULL"
	}
	d.header(b, "Receipt", number, stamp)
	if o.CancelledAt != nil || p.RevisedAt != nil {
		d.SetX(d.left)
		if o.CancelledAt != nil {
			d.fill(rgb{254, 226, 226})
			d.font("B", 9.5, rgb{185, 28, 28})
			msg := "Cancelled " + pdfDate(*o.CancelledAt) + " — this payment was removed from invoice " + inv.Number + ". This receipt is no longer valid."
			if strings.TrimSpace(o.CancelReason) != "" {
				msg += " Reason: " + strings.TrimSpace(o.CancelReason)
			}
			for _, l := range d.lines(msg, d.width-8) {
				d.SetX(d.left)
				d.CellFormat(d.width, 6, "  "+l, "", 2, "L", true, 0, "")
			}
		} else {
			d.font("B", 9.5, rgb{180, 83, 9})
			d.CellFormat(d.width, 6, "Revised "+pdfDate(*p.RevisedAt)+" — this replaces any earlier copy of receipt "+p.ReceiptNumber+".", "", 2, "L", false, 0, "")
		}
		d.Ln(3)
	}

	// Received from / receipt details.
	colW := d.width / 2
	y0 := d.GetY()
	d.SetXY(d.left, y0)
	d.font("B", 7.5, cMuted)
	d.CellFormat(colW, 5, "RECEIVED FROM", "", 2, "L", false, 0, "")
	d.font("B", 11, cInk)
	d.SetX(d.left)
	d.CellFormat(colW, 6, inv.ClientName, "", 2, "L", false, 0, "")
	d.font("", 9, cMuted)
	for _, l := range append(nonEmpty(inv.ClientEmail, inv.ClientPhone), d.lines(inv.ClientAddress, colW-4)...) {
		if strings.TrimSpace(l) == "" {
			continue
		}
		d.SetX(d.left)
		d.CellFormat(colW, 4.4, l, "", 2, "L", false, 0, "")
	}
	leftEnd := d.GetY()
	d.SetY(y0)
	rx := d.left + colW + 6
	rw := colW - 6
	d.kv(rx, rw, "Receipt no.", firstNonBlank(p.ReceiptNumber, "—"), false, cInk)
	d.kv(rx, rw, "Payment date", pdfDate(p.Date), false, cInk)
	d.kv(rx, rw, "Method", PaymentMethodLabel(p.Method), false, cInk)
	if p.Reference != "" {
		d.kv(rx, rw, "Reference", p.Reference, false, cInk)
	}
	d.SetY(math.Max(leftEnd, d.GetY()) + 5)

	// Amount received.
	by := d.GetY()
	d.fill(tint(d.accent, 0.9))
	d.Rect(d.left, by, d.width, 24, "F")
	d.SetXY(d.left+5, by+2)
	d.font("B", 10, cInk)
	d.CellFormat(d.width/2, 10, "Amount received", "", 0, "L", false, 0, "")
	d.font("B", 18, d.accent)
	d.CellFormat(d.width/2-10, 10, d.money(p.Amount), "", 1, "R", false, 0, "")
	d.SetX(d.left + 5)
	d.font("", 9, cInk)
	words := AmountInWords(p.Amount, inv.Currency)
	for _, l := range d.lines("In words: "+words, d.width-10) {
		d.SetX(d.left + 5)
		d.CellFormat(d.width-10, 4.8, l, "", 2, "L", false, 0, "")
	}
	d.SetY(math.Max(d.GetY(), by+24) + 6)

	// Invoice position as at this payment.
	d.font("B", 7.5, cMuted)
	d.SetX(d.left)
	d.CellFormat(d.width, 5, "APPLIED TO INVOICE "+strings.ToUpper(inv.Number), "", 2, "L", false, 0, "")
	x, w := d.left, d.width
	d.kv(x, w, "Invoice number", inv.Number, false, cInk)
	d.kv(x, w, "Invoice date", pdfDate(inv.IssueDate), false, cInk)
	d.kv(x, w, "Invoice total", d.money(f.Total), false, cInk)
	if inv.CreditsApplied > 0 {
		d.kv(x, w, "Credits applied", "−"+d.money(inv.CreditsApplied), false, cInk)
	}
	if inv.WriteOffAmount > 0 {
		d.kv(x, w, "Written off", "−"+d.money(inv.WriteOffAmount), false, cInk)
	}
	d.kv(x, w, "Total paid to date", d.money(f.PaidToDate), false, cInk)
	d.draw(cLine)
	d.Line(x, d.GetY()+0.5, d.right, d.GetY()+0.5)
	d.Ln(1)
	if f.Balance <= 0 {
		d.kv(x, w, "Balance remaining", "Paid in full", true, rgb{22, 163, 74})
	} else {
		d.kv(x, w, "Balance remaining", d.money(f.Balance), true, d.accent)
	}
	d.Ln(5)
	d.para("Notes", p.Notes, d.left, d.width)
	d.ensure(12)
	d.font("", 9, cMuted)
	d.SetX(d.left)
	thanks := "Thank you for your payment."
	if b.Email != "" {
		thanks += " Questions about this receipt? Contact " + b.Email + "."
	}
	d.MultiCell(d.width, 5, thanks, "", "L", false)
	var buf bytes.Buffer
	if err := d.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func firstNonBlank(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------- data

// orgLogo reads the organisation's logo image from org_logos.
func orgLogo(db *gorm.DB, org string) []byte {
	if !db.Migrator().HasTable("org_logos") {
		return nil
	}
	var row struct {
		ContentType string
		Data        []byte
	}
	db.Raw("SELECT content_type, data FROM org_logos WHERE organization_id = ?", org).Scan(&row)
	if row.ContentType != "image/png" && row.ContentType != "image/jpeg" {
		return nil // SVG / WebP: the business name is shown instead
	}
	return row.Data
}

// PDFBusinessFor builds the business block for an organisation.
func PDFBusinessFor(db *gorm.DB, s *Settings, org string) PDFBusiness {
	b := PDFBusiness{Name: s.BusinessName, Address: s.Address, Email: s.Email, Phone: s.Phone, Website: s.Website,
		TaxNumber: s.TaxNumber, TaxNumberLabel: s.TaxNumberLabel, TaxLabel: s.TaxLabel,
		PaymentInstructions: s.PaymentInstructions, Accent: s.AccentColor}
	if b.Name == "" && db.Migrator().HasTable("organizations") {
		db.Raw("SELECT name FROM organizations WHERE id = ?", org).Scan(&b.Name)
	}
	b.Logo = orgLogo(db, org)
	return b
}
