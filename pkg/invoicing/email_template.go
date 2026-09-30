package invoicing

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// The one email template for everything the invoicing module sends: invoices,
// quotes, payment reminders and payment receipts. RenderEmail returns the HTML
// and the plain-text alternative, which mirror each other.
//
// The HTML is table-based with inline styles (Gmail, Outlook, Apple Mail),
// 600px wide and fluid below that, with bulletproof (VML) buttons for Outlook
// and a dark-mode palette for clients that honour prefers-color-scheme. The
// logo sits on a white tile so dark logos stay visible in dark mode.
// Golden files: testdata/email_*.golden (go test -run Golden -update).
// ---------------------------------------------------------------------------

// EmailBusiness is the sender block (header and footer).
type EmailBusiness struct {
	Name, Address, Email, Phone, Website string
	TaxNumberLabel, TaxNumber            string
}

// EmailLine is one line in the item summary.
type EmailLine struct {
	Description string
	Detail      string // e.g. "3 × KSh 1,500.00"
	Amount      string
}

// EmailRow is a label / value pair (facts and totals).
type EmailRow struct {
	Label, Value string
	Strong       bool
}

// EmailView is everything one email shows.
type EmailView struct {
	Kind      string // invoice | quote | reminder | receipt
	Business  EmailBusiness
	LogoURL   string // absolute http(s) URL, or "" for the business name
	LogoW     int    // natural size in pixels (0 when unknown)
	LogoH     int
	Accent    string
	Preheader string
	DocLabel  string // "Invoice", "Quote", "Payment reminder", "Payment receipt"
	Number    string
	Message   string // the user's message (plain text; links become clickable)

	AmountLabel string // "Amount due", "Quote total", "Payment received"
	Amount      string // formatted, e.g. "KSh 3,213.98"
	StatusLine  string // "Part-paid — KSh 100.00 received"
	StatusTone  string // ok | warn | danger | info

	Facts     []EmailRow
	Actions   []EmailAction
	Lines     []EmailLine
	MoreLines int
	Totals    []EmailRow
	Note      string // "A PDF copy (INV-1001.pdf) is attached."
	FXNote    string // "Amounts in USD. Converted from KES at …"
	ReplyTo   string // who a reply reaches
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

const (
	fontStack = `-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif`
	cText     = "#1a1d2e"
	cMutedTxt = "#5b6275"
	cBorder   = "#e4e7ee"
	cPage     = "#f3f4f8"
)

func esc(s string) string { return html.EscapeString(s) }

func toneColors(t string) (fg, bg string) {
	switch t {
	case "ok":
		return "#0f7a45", "#e6f6ee"
	case "warn":
		return "#8a5a00", "#fff4dc"
	case "danger":
		return "#b42318", "#fdecea"
	}
	return "#334155", "#eef1f6"
}

// logoBox scales w×h into at most 200×56 (height 48 when unknown).
func logoBox(w, h int) (int, int) {
	if w <= 0 || h <= 0 {
		return 0, 48
	}
	fw, fh := float64(w), float64(h)
	scale := 1.0
	if fh > 56 {
		scale = 56 / fh
	}
	if fw*scale > 200 {
		scale = 200 / fw
	}
	return int(fw*scale + 0.5), int(fh*scale + 0.5)
}

// linkify escapes text, makes http(s) links clickable and keeps line breaks.
func linkify(msg, accent string) string {
	var b strings.Builder
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	last := 0
	for _, m := range urlRe.FindAllStringIndex(msg, -1) {
		b.WriteString(esc(msg[last:m[0]]))
		u := strings.TrimRight(msg[m[0]:m[1]], ".,);")
		b.WriteString(fmt.Sprintf(`<a href="%s" style="color:%s;text-decoration:underline;word-break:break-all">%s</a>`, esc(u), accent, esc(u)))
		last = m[0] + len(u)
	}
	b.WriteString(esc(msg[last:]))
	return strings.ReplaceAll(b.String(), "\n", "<br>\n")
}

// button renders a bulletproof button (VML for Outlook, a padded link elsewhere).
func button(a EmailAction, accent string) string {
	bg, fg, border := accent, "#ffffff", accent
	cls := "btn"
	if !a.Primary {
		bg, fg = "#ffffff", accent
		cls = "btn btn2"
	}
	width := 8*len([]rune(a.Label)) + 56
	fill := `fillcolor="` + bg + `"`
	return fmt.Sprintf(`<td style="padding:0 10px 10px 0"><!--[if mso]><v:roundrect xmlns:v="urn:schemas-microsoft-com:vml" xmlns:w="urn:schemas-microsoft-com:office:word" href="%[1]s" style="height:46px;v-text-anchor:middle;width:%[6]dpx;" arcsize="18%%" strokecolor="%[4]s" strokeweight="2px" %[7]s><w:anchorlock/><center style="color:%[3]s;font-family:Arial,sans-serif;font-size:15px;font-weight:bold;">%[5]s</center></v:roundrect><![endif]--><!--[if !mso]><!-- --><a href="%[1]s" class="%[8]s" style="display:inline-block;background:%[2]s;color:%[3]s;border:2px solid %[4]s;border-radius:8px;padding:12px 22px;font:600 15px/20px %[9]s;text-decoration:none;mso-hide:all">%[5]s</a><!--<![endif]--></td>`,
		esc(a.URL), bg, fg, border, esc(a.Label), width, fill, cls, fontStack)
}

func validURL(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

// RenderEmail renders v as HTML and plain text.
func RenderEmail(v EmailView) (htmlOut, text string) {
	accent := v.Accent
	if !hexColor.MatchString(accent) {
		accent = "#4f46e5"
	}
	var b strings.Builder
	w := b.WriteString
	title := strings.TrimSpace(v.DocLabel + " " + v.Number)
	w(`<!doctype html>` + "\n")
	w(`<html lang="en" xmlns="http://www.w3.org/1999/xhtml" xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="urn:schemas-microsoft-com:office:office">` + "\n<head>\n")
	w(`<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="X-UA-Compatible" content="IE=edge">` + "\n")
	w(`<meta name="color-scheme" content="light dark"><meta name="supported-color-schemes" content="light dark">` + "\n")
	w(`<title>` + esc(title) + `</title>` + "\n")
	w(`<!--[if mso]><xml><o:OfficeDocumentSettings><o:PixelsPerInch>96</o:PixelsPerInch></o:OfficeDocumentSettings></xml><![endif]-->` + "\n")
	w(`<style>
body{margin:0;padding:0;width:100%!important;-webkit-text-size-adjust:100%;-ms-text-size-adjust:100%}
table{border-collapse:collapse;mso-table-lspace:0;mso-table-rspace:0}
img{border:0;outline:none;text-decoration:none;-ms-interpolation-mode:bicubic}
a{color:` + accent + `}
@media screen and (max-width:620px){
 .container{width:100%!important}
 .px{padding-left:20px!important;padding-right:20px!important}
 .amount{font-size:30px!important;line-height:36px!important}
 .stack{display:block!important;width:auto!important;text-align:left!important}
 .btn{display:block!important;text-align:center!important}
}
@media (prefers-color-scheme:dark){
 .page{background:#0f1117!important}
 .card{background:#181b24!important;border-color:#2a2f3b!important}
 .summary{background:#1f2330!important;border-color:#2a2f3b!important}
 .tx{color:#e8eaf0!important}
 .mt{color:#a3a9b8!important}
 .rule{border-color:#2a2f3b!important}
 .btn2{background:#181b24!important}
}
</style>
</head>
`)
	w(`<body class="page" style="margin:0;padding:0;background:` + cPage + `">` + "\n")
	if v.Preheader != "" {
		w(`<div style="display:none;font-size:1px;line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;mso-hide:all">` + esc(v.Preheader) + strings.Repeat("&#8199;&#65279;&#847; ", 12) + `</div>` + "\n")
	}
	w(`<table role="presentation" class="page" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="` + cPage + `" style="background:` + cPage + `"><tr><td align="center" style="padding:24px 12px">` + "\n")
	w(`<!--[if mso]><table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0"><tr><td><![endif]-->` + "\n")
	w(`<table role="presentation" class="container" width="600" cellpadding="0" cellspacing="0" border="0" style="width:600px;max-width:600px">` + "\n")

	// ---- card
	w(`<tr><td class="card" bgcolor="#ffffff" style="background:#ffffff;border:1px solid ` + cBorder + `;border-radius:12px">` + "\n")
	// header band
	w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td height="6" bgcolor="` + accent + `" style="height:6px;line-height:6px;font-size:6px;background:` + accent + `;border-radius:12px 12px 0 0">&nbsp;</td></tr></table>` + "\n")
	w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>`)
	w(`<td class="px stack" valign="middle" style="padding:22px 32px 18px">`)
	if validURL(v.LogoURL) {
		lw, lh := logoBox(v.LogoW, v.LogoH)
		size := ` height="` + fmt.Sprint(lh) + `"`
		style := `display:block;height:` + fmt.Sprint(lh) + `px;width:auto;max-width:200px`
		if lw > 0 {
			size = ` width="` + fmt.Sprint(lw) + `"` + size
			style = `display:block;width:` + fmt.Sprint(lw) + `px;height:` + fmt.Sprint(lh) + `px;max-width:200px`
		}
		w(`<table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr><td bgcolor="#ffffff" style="background:#ffffff;border-radius:6px;padding:4px">`)
		w(`<img src="` + esc(v.LogoURL) + `"` + size + ` alt="` + esc(v.Business.Name) + `" style="` + style + `;border:0;font:700 18px/1.2 ` + fontStack + `;color:` + cText + `">`)
		w(`</td></tr></table>`)
	} else {
		w(`<div class="tx" style="font:700 20px/1.3 ` + fontStack + `;color:` + cText + `">` + esc(v.Business.Name) + `</div>`)
	}
	w(`</td>`)
	if v.DocLabel != "" || v.Number != "" {
		w(`<td class="px stack" align="right" valign="middle" style="padding:22px 32px 18px;text-align:right">`)
		if v.DocLabel != "" {
			w(`<div class="mt" style="font:600 11px/1.4 ` + fontStack + `;letter-spacing:.08em;text-transform:uppercase;color:` + cMutedTxt + `">` + esc(v.DocLabel) + `</div>`)
		}
		if v.Number != "" {
			w(`<div class="tx" style="font:700 16px/1.4 ` + fontStack + `;color:` + cText + `">` + esc(v.Number) + `</div>`)
		}
		w(`</td>`)
	}
	w(`</tr></table>` + "\n")

	// message
	if strings.TrimSpace(v.Message) != "" {
		w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td class="px tx" style="padding:4px 32px 8px;font:15px/1.6 ` + fontStack + `;color:` + cText + `">`)
		w(linkify(strings.TrimSpace(v.Message), accent))
		w(`</td></tr></table>` + "\n")
	}

	// summary card
	if v.Amount != "" {
		fg, bg := toneColors(v.StatusTone)
		w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td class="px" style="padding:16px 32px 8px">`)
		w(`<table role="presentation" class="summary" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="#f8f9fc" style="background:#f8f9fc;border:1px solid ` + cBorder + `;border-radius:10px"><tr><td style="padding:20px 22px">`)
		w(`<div class="mt" style="font:600 12px/1.4 ` + fontStack + `;letter-spacing:.06em;text-transform:uppercase;color:` + cMutedTxt + `">` + esc(v.AmountLabel) + `</div>`)
		w(`<div class="amount tx" style="font:700 34px/42px ` + fontStack + `;color:` + cText + `;font-variant-numeric:tabular-nums;margin:2px 0 8px">` + esc(v.Amount) + `</div>`)
		if v.StatusLine != "" {
			w(`<table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr><td bgcolor="` + bg + `" style="background:` + bg + `;border-radius:999px;padding:4px 12px;font:600 13px/1.4 ` + fontStack + `;color:` + fg + `">` + esc(v.StatusLine) + `</td></tr></table>`)
		}
		if len(v.Facts) > 0 {
			w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:14px">`)
			for _, f := range v.Facts {
				w(`<tr><td class="mt" style="padding:3px 0;font:14px/1.5 ` + fontStack + `;color:` + cMutedTxt + `">` + esc(f.Label) + `</td><td class="tx" align="right" style="padding:3px 0;font:600 14px/1.5 ` + fontStack + `;color:` + cText + `;text-align:right">` + esc(f.Value) + `</td></tr>`)
			}
			w(`</table>`)
		}
		w(`</td></tr></table></td></tr></table>` + "\n")
	}

	// buttons
	var btns strings.Builder
	for _, a := range v.Actions {
		if validURL(a.URL) {
			btns.WriteString(button(a, accent))
		}
	}
	if btns.Len() > 0 {
		w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td class="px" style="padding:14px 32px 6px"><table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>` + btns.String() + `</tr></table></td></tr></table>` + "\n")
	}

	// line items
	if len(v.Lines) > 0 {
		w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td class="px" style="padding:12px 32px 4px">`)
		w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">`)
		w(`<tr><td class="mt rule" style="padding:0 0 6px;border-bottom:1px solid ` + cBorder + `;font:600 11px/1.4 ` + fontStack + `;letter-spacing:.06em;text-transform:uppercase;color:` + cMutedTxt + `">Items</td><td class="mt rule" align="right" style="padding:0 0 6px;border-bottom:1px solid ` + cBorder + `;font:600 11px/1.4 ` + fontStack + `;letter-spacing:.06em;text-transform:uppercase;color:` + cMutedTxt + `;text-align:right">Amount</td></tr>`)
		for _, l := range v.Lines {
			w(`<tr><td class="rule" style="padding:9px 12px 9px 0;border-bottom:1px solid ` + cBorder + `;font:14px/1.45 ` + fontStack + `"><span class="tx" style="color:` + cText + `">` + esc(l.Description) + `</span>`)
			if l.Detail != "" {
				w(`<br><span class="mt" style="font-size:13px;color:` + cMutedTxt + `">` + esc(l.Detail) + `</span>`)
			}
			w(`</td><td class="tx rule" align="right" valign="top" style="padding:9px 0;border-bottom:1px solid ` + cBorder + `;font:14px/1.45 ` + fontStack + `;color:` + cText + `;text-align:right;white-space:nowrap;font-variant-numeric:tabular-nums">` + esc(l.Amount) + `</td></tr>`)
		}
		if v.MoreLines > 0 {
			s := "s"
			if v.MoreLines == 1 {
				s = ""
			}
			w(`<tr><td colspan="2" class="mt" style="padding:8px 0 0;font:13px/1.4 ` + fontStack + `;color:` + cMutedTxt + `">and ` + fmt.Sprint(v.MoreLines) + ` more item` + s + ` — see the attached PDF</td></tr>`)
		}
		w(`</table>`)
		if len(v.Totals) > 0 {
			w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:8px">`)
			for _, t := range v.Totals {
				weight := "400"
				if t.Strong {
					weight = "700"
				}
				w(`<tr><td class="mt" style="padding:3px 0;font:` + weight + ` 14px/1.5 ` + fontStack + `;color:` + cMutedTxt + `">` + esc(t.Label) + `</td><td class="tx" align="right" style="padding:3px 0;font:` + weight + ` 14px/1.5 ` + fontStack + `;color:` + cText + `;text-align:right;white-space:nowrap;font-variant-numeric:tabular-nums">` + esc(t.Value) + `</td></tr>`)
			}
			w(`</table>`)
		}
		w(`</td></tr></table>` + "\n")
	}

	// notes
	var notes []string
	if v.FXNote != "" {
		notes = append(notes, v.FXNote)
	}
	if v.Note != "" {
		notes = append(notes, v.Note)
	}
	if len(notes) > 0 {
		w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td class="px mt" style="padding:12px 32px 0;font:13px/1.5 ` + fontStack + `;color:` + cMutedTxt + `">`)
		for i, n := range notes {
			if i > 0 {
				w(`<br>`)
			}
			w(esc(n))
		}
		w(`</td></tr></table>` + "\n")
	}
	w(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td style="height:26px;line-height:26px;font-size:26px">&nbsp;</td></tr></table>` + "\n")
	w(`</td></tr>` + "\n")

	// ---- footer
	w(`<tr><td class="px" align="center" style="padding:22px 32px 8px;text-align:center;font:12px/1.6 ` + fontStack + `;color:` + cMutedTxt + `">`)
	for i, l := range footerLines(v) {
		style := ` class="mt" style="color:` + cMutedTxt + `"`
		if i == 0 {
			style = ` class="mt" style="color:` + cMutedTxt + `;font-weight:600"`
		}
		w(`<div` + style + `>` + footerLinkify(l) + `</div>`)
	}
	if v.ReplyTo != "" {
		w(`<div class="mt" style="color:` + cMutedTxt + `;margin-top:10px">` + esc(v.ReplyTo) + `</div>`)
	}
	w(`<div class="mt" style="color:#9aa1b1;margin-top:12px;font-size:11px">Sent with DASYIN ERP</div>`)
	w(`</td></tr>` + "\n")
	w(`</table>` + "\n")
	w(`<!--[if mso]></td></tr></table><![endif]-->` + "\n")
	w(`</td></tr></table>` + "\n</body>\n</html>\n")
	return b.String(), renderText(v)
}

// footerLines: name, address (one line), contacts, tax number.
func footerLines(v EmailView) []string {
	bz := v.Business
	var out []string
	if bz.Name != "" {
		out = append(out, bz.Name)
	}
	if a := oneLine(bz.Address); a != "" {
		out = append(out, a)
	}
	if c := strings.Join(nonEmpty(bz.Phone, bz.Email, bz.Website), " · "); c != "" {
		out = append(out, c)
	}
	if bz.TaxNumber != "" {
		lbl := bz.TaxNumberLabel
		if lbl == "" {
			lbl = "Tax no."
		}
		out = append(out, lbl+" "+bz.TaxNumber)
	}
	return out
}

func oneLine(s string) string {
	parts := []string{}
	for _, p := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if p = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(p), ",")); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

var emailAddrRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// footerLinkify escapes a footer line and makes email addresses mailto links
// (and keeps them muted so they don't shout).
func footerLinkify(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range emailAddrRe.FindAllStringIndex(s, -1) {
		b.WriteString(esc(s[last:m[0]]))
		a := s[m[0]:m[1]]
		b.WriteString(`<a href="mailto:` + esc(a) + `" style="color:` + cMutedTxt + `;text-decoration:underline">` + esc(a) + `</a>`)
		last = m[1]
	}
	b.WriteString(esc(s[last:]))
	return b.String()
}

// renderText is the plain-text alternative.
func renderText(v EmailView) string {
	var b strings.Builder
	msg := strings.TrimSpace(strings.ReplaceAll(v.Message, "\r\n", "\n"))
	if msg != "" {
		b.WriteString(msg + "\n\n")
	}
	rule := strings.Repeat("-", 40) + "\n"
	b.WriteString(rule)
	head := strings.ToUpper(strings.TrimSpace(v.DocLabel + " " + v.Number))
	if head != "" {
		b.WriteString(head + "\n")
	}
	if v.Amount != "" {
		b.WriteString(v.AmountLabel + ": " + v.Amount + "\n")
	}
	if v.StatusLine != "" {
		b.WriteString(v.StatusLine + "\n")
	}
	for _, f := range v.Facts {
		b.WriteString(f.Label + ": " + f.Value + "\n")
	}
	var acts []string
	for _, a := range v.Actions {
		if validURL(a.URL) && !strings.Contains(msg, a.URL) {
			acts = append(acts, a.Label+": "+a.URL)
		}
	}
	if len(acts) > 0 {
		b.WriteString("\n" + strings.Join(acts, "\n") + "\n")
	}
	if len(v.Lines) > 0 {
		b.WriteString("\nItems\n")
		for _, l := range v.Lines {
			line := "- " + l.Description
			if l.Detail != "" {
				line += " (" + l.Detail + ")"
			}
			b.WriteString(line + " — " + l.Amount + "\n")
		}
		if v.MoreLines > 0 {
			s := "s"
			if v.MoreLines == 1 {
				s = ""
			}
			b.WriteString(fmt.Sprintf("and %d more item%s — see the attached PDF\n", v.MoreLines, s))
		}
		if len(v.Totals) > 0 {
			b.WriteString("\n")
			for _, t := range v.Totals {
				b.WriteString(t.Label + ": " + t.Value + "\n")
			}
		}
	}
	if v.FXNote != "" {
		b.WriteString("\n" + v.FXNote + "\n")
	}
	if v.Note != "" {
		b.WriteString("\n" + v.Note + "\n")
	}
	b.WriteString(rule)
	for _, l := range footerLines(v) {
		b.WriteString(l + "\n")
	}
	if v.ReplyTo != "" {
		b.WriteString(v.ReplyTo + "\n")
	}
	b.WriteString("Sent with DASYIN ERP\n")
	return b.String()
}
