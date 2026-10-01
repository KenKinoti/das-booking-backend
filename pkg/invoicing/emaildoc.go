package invoicing

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// KindReceipt is a "payment received" email for one payment.
const KindReceipt = "receipt"

// ---------------------------------------------------------------- placeholders

// Placeholders are the {{names}} subject and message templates may use. Every
// value is formatted on the server from the stored, recalculated document in
// the document's own currency — the browser never formats amounts that go out.
var Placeholders = []struct{ Name, Help string }{
	{"client", "Client name"}, {"business", "Your business name"},
	{"type", "Invoice / Quote"}, {"type_lower", "invoice / quote"}, {"number", "Document number"},
	{"total", "Document total"}, {"amount_due", "Balance still to pay"}, {"balance_due", "Same as amount_due"},
	{"amount_paid", "Paid so far (incl. deposits)"}, {"deposit", "Deposit received"},
	{"subtotal", "Subtotal"}, {"tax", "Tax total"}, {"currency", "Currency code"},
	{"issue_date", "Issue date"}, {"due_date", "Due date / valid until"}, {"days_overdue", "Days overdue"},
	{"link", "Client view link"}, {"pay_link", "Online payment link"}, {"reference", "Reference"},
	{"payment_amount", "Receipt: amount received"}, {"payment_date", "Receipt: payment date"}, {"payment_method", "Receipt: method"},
	{"balance_sentence", "Receipt: balance remaining / paid in full"},
	{"receipt_number", "Receipt: receipt number"}, {"amount_received", "Receipt: amount received"},
	{"invoice_number", "Invoice number"}, {"client_name", "Client name"}, {"business_name", "Your business name"},
}

// ReceiptPlaceholders are the placeholders offered for receipt templates.
var ReceiptPlaceholders = []string{"receipt_number", "amount_received", "balance_due", "invoice_number", "client_name", "business_name", "payment_date", "payment_method", "balance_sentence", "link"}

const (
	defaultReceiptSubject = "Receipt {{receipt_number}} — payment received for invoice {{invoice_number}}"
	defaultReceiptMessage = "Hi {{client_name}},\n\nThank you — we've received your payment of {{amount_received}} for invoice {{invoice_number}} on {{payment_date}} ({{payment_method}}).\n\n{{balance_sentence}}\n\nYour receipt {{receipt_number}} is attached for your records.\n\n{{business_name}}"
)

var placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.]+)\s*\}\}`)

// fillTemplate replaces known {{name}} placeholders (spaces allowed) and
// returns the names it could not resolve.
func fillTemplate(t string, vars map[string]string) (string, []string) {
	var missing []string
	out := placeholderRe.ReplaceAllStringFunc(t, func(m string) string {
		k := placeholderRe.FindStringSubmatch(m)[1]
		if v, ok := vars[k]; ok {
			return v
		}
		missing = append(missing, k)
		return m
	})
	return out, missing
}

// renderTemplate fills {{name}} placeholders (unknown ones are left as is).
func renderTemplate(t string, vars map[string]string) string {
	out, _ := fillTemplate(t, vars)
	return out
}

func depositTotal(inv *Invoice) float64 {
	d := 0.0
	for _, p := range inv.Payments {
		if p.IsDeposit {
			d += p.Amount
		}
	}
	return roundCur(d, inv.Currency)
}

// emailVars are the placeholder values for doc (the client-facing copy).
func emailVars(doc *Invoice, business, viewURL, payURL string, pay *Payment) map[string]string {
	m := func(v float64) string { return FormatMoney(v, doc.Currency) }
	typ := docLabel(doc.DocType)
	v := map[string]string{
		"type": typ, "type_lower": strings.ToLower(typ), "number": doc.Number, "business": business,
		"client": doc.ClientName, "reference": doc.Reference, "title": doc.Title,
		"total": m(doc.Total), "amount_due": m(doc.BalanceDue), "balance_due": m(doc.BalanceDue),
		"amount_paid": m(doc.AmountPaid), "deposit": m(depositTotal(doc)), "subtotal": m(doc.Subtotal),
		"tax": m(doc.TaxTotal), "discount": m(doc.DiscountTotal), "shipping": m(doc.ShippingAmount),
		"currency": doc.Currency, "issue_date": pdfDate(doc.IssueDate), "due_date": pdfDate(doc.DueDate),
		"days_overdue": strconv.Itoa(doc.DaysOverdue), "link": viewURL, "pay_link": payURL,
		"invoice_number": doc.Number, "client_name": doc.ClientName, "business_name": business,
	}
	if payURL == "" {
		v["pay_link"] = viewURL
	}
	if doc.DocType == TypeQuote {
		v["amount_due"], v["balance_due"] = m(doc.Total), m(doc.Total)
	}
	if doc.BalanceDue <= 0 && doc.DocType == TypeInvoice {
		v["balance_sentence"] = "This invoice is now paid in full."
	} else {
		v["balance_sentence"] = "The remaining balance is " + m(doc.BalanceDue) + ", due " + pdfDate(doc.DueDate) + "."
	}
	if pay != nil {
		f := figuresAt(doc, *pay)
		v["payment_amount"] = m(pay.Amount)
		v["amount_received"] = m(pay.Amount)
		v["payment_date"] = pdfDate(pay.Date)
		v["payment_method"] = PaymentMethodLabel(pay.Method)
		v["payment_reference"] = pay.Reference
		v["receipt_number"] = pay.ReceiptNumber
		v["balance_due"], v["amount_due"], v["amount_paid"] = m(f.Balance), m(f.Balance), m(f.PaidToDate)
		if f.Balance <= 0 {
			v["balance_sentence"] = "This invoice is now paid in full."
		} else {
			v["balance_sentence"] = "The remaining balance is " + m(f.Balance) + ", due " + pdfDate(doc.DueDate) + "."
		}
	}
	return v
}

const (
	defaultInvoiceMessage = "Hi {{client}},\n\nPlease find {{type_lower}} {{number}} for {{total}}, due {{due_date}}.\n\nView it online: {{link}}\n\nThank you for your business,\n{{business}}"
	defaultQuoteMessage   = "Hi {{client}},\n\nPlease find quote {{number}} for {{total}}, valid until {{due_date}}.\n\nView and accept it online: {{link}}\n\nThank you,\n{{business}}"
)

// legacyDefaultMessages are earlier built-in defaults: a quote whose settings
// still hold one of them gets the quote wording instead.
var legacyDefaultMessages = map[string]bool{
	defaultInvoiceMessage: true,
	"Hi {{client}},\n\nPlease find {{type_lower}} {{number}} for {{total}}, due {{due_date}}.\n\nView it online: {{link}}\n\nThank you,\n{{business}}": true,
}

// defaultTemplates returns the subject and message templates for kind.
func defaultTemplates(s *Settings, doc *Invoice, kind string) (string, string) {
	subject, message := s.EmailSubject, s.EmailMessage
	if strings.TrimSpace(subject) == "" {
		subject = "{{type}} {{number}} from {{business}}"
	}
	if strings.TrimSpace(message) == "" {
		message = defaultInvoiceMessage
	}
	switch kind {
	case KindQuote:
		if legacyDefaultMessages[message] {
			message = defaultQuoteMessage
		}
	case KindReminder:
		if doc.DisplayStatus == StatusOverdue {
			subject = "Overdue: Invoice {{number}} from {{business}}"
			message = "Hi {{client}},\n\nThis is a reminder that invoice {{number}} was due on {{due_date}} and {{amount_due}} is still outstanding.\n\nYou can view and pay it online: {{link}}\n\nIf you've already paid, please ignore this message — and thank you.\n\n{{business}}"
		} else {
			subject = "Reminder: Invoice {{number}} from {{business}}"
			message = "Hi {{client}},\n\nThis is a friendly reminder that invoice {{number}} for {{amount_due}} is due on {{due_date}}.\n\nYou can view and pay it online: {{link}}\n\nIf you've already paid, please ignore this message.\n\nThank you,\n{{business}}"
		}
	case KindReceipt:
		subject, message = s.ReceiptSubject, s.ReceiptMessage
		if strings.TrimSpace(subject) == "" {
			subject = defaultReceiptSubject
		}
		if strings.TrimSpace(message) == "" {
			message = defaultReceiptMessage
		}
	}
	return subject, message
}

// ---------------------------------------------------------------- building

// builtEmail is one fully rendered email, exactly as it would be sent.
type builtEmail struct {
	Kind       string
	Doc        *Invoice // the client-facing copy
	Payment    *Payment
	Subject    string
	Message    string // the user's message after placeholders
	HTML, Text string
	To         string
	CC, BCC    []string
	Skipped    []string
	From       string
	Source     string
	ReplyTo    string
	LogoURL    string
	Business   PDFBusiness
	ViewURL    string
	PayURL     string
	Attachment mail.Attachment
	PDFErr     error
	Unresolved []string
	Amounts    map[string]string
	View       EmailView
}

func kindOf(inv *Invoice, requested string) string {
	if requested == KindReceipt && inv.DocType == TypeInvoice {
		return KindReceipt
	}
	return kindFor(inv, requested)
}

// resolveRecipients merges the To / CC / BCC lists as the send dialog does.
func (h *Handler) resolveRecipients(org string, inv *Invoice, kind string, in sendInput, mcfg mail.Config, viaMail bool) (string, []string, []string, []string, error) {
	sugTo, sugCC := h.recipientsFor(org, inv, kind)
	to := strings.TrimSpace(in.To)
	if to == "" && sugTo != nil {
		to = sugTo.Email
	}
	var ccIn []string
	if in.CC != nil {
		ccIn = *in.CC
	} else {
		for _, r := range sugCC {
			ccIn = append(ccIn, r.Email)
		}
		if viaMail {
			ccIn = append(ccIn, mcfg.CC...)
		}
	}
	to, cc, skipped := MergeRecipients(to, ccIn)
	var bccIn []string
	if in.BCC != nil {
		bccIn = *in.BCC
	} else if viaMail {
		bccIn = mcfg.BCC
	}
	_, bcc, skippedBCC := MergeRecipients("", append(append([]string{to}, cc...), bccIn...))
	bcc = withoutAddresses(bcc, append([]string{to}, cc...))
	skipped = append(skipped, skippedBCC...)
	if strings.TrimSpace(in.To) != "" && to == "" {
		return "", nil, nil, skipped, &apiError{http.StatusBadRequest, "INVALID_EMAIL", "“" + strings.TrimSpace(in.To) + "” is not a valid email address"}
	}
	if len(cc) > 50 {
		return "", nil, nil, skipped, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Too many CC recipients (max 50)"}
	}
	if len(bcc) > 20 {
		return "", nil, nil, skipped, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Too many BCC recipients (max 20)"}
	}
	return to, cc, bcc, skipped, nil
}

// buildEmail renders the email for inv exactly as Send would send it.
func (h *Handler) buildEmail(tx *gorm.DB, c *gin.Context, inv *Invoice, in sendInput, mcfg mail.Config, viaMail bool) (*builtEmail, error) {
	org := inv.OrganizationID
	kind := kindOf(inv, in.Kind)
	b := &builtEmail{Kind: kind}
	var err error
	b.To, b.CC, b.BCC, b.Skipped, err = h.resolveRecipients(org, inv, kind, in, mcfg, viaMail)
	if err != nil {
		return nil, err
	}
	if kind == KindReceipt {
		for i := range inv.Payments {
			if inv.Payments[i].ID == in.PaymentID {
				p := inv.Payments[i]
				b.Payment = &p
			}
		}
		if b.Payment == nil {
			return nil, &apiError{http.StatusNotFound, "NOT_FOUND", "Payment not found"}
		}
	}
	s, serr := h.settingsFor(tx, org, false)
	if serr != nil {
		s = &Settings{}
	}
	b.Business = PDFBusinessFor(tx, s, org)
	var cc = c
	b.LogoURL = logoFor(cc, org)

	doc := *inv
	if (kind == KindInvoice || kind == KindQuote) && doc.Status == StatusDraft {
		doc.Status = StatusSent // the copy the client gets is the sent one
	}
	Derive(&doc, h.now())
	b.Doc = &doc
	base := h.AppBase(c, in.AppURL)
	b.ViewURL = ViewURL(base, doc.PublicToken)
	if doc.DocType == TypeInvoice && h.payable(&doc) {
		b.PayURL = PayURL(base, doc.PublicToken)
	}
	vars := emailVars(&doc, b.Business.Name, b.ViewURL, b.PayURL, b.Payment)
	subjT, msgT := defaultTemplates(s, &doc, kind)
	if kind == KindReceipt && b.Payment.RevisedAt != nil {
		subjT = "Revised: " + subjT
	}
	if strings.TrimSpace(in.Subject) != "" {
		subjT = in.Subject
	}
	if strings.TrimSpace(in.Message) != "" {
		msgT = in.Message
	}
	var m1, m2 []string
	b.Subject, m1 = fillTemplate(subjT, vars)
	b.Subject = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(b.Subject))
	b.Message, m2 = fillTemplate(msgT, vars)
	b.Unresolved = uniqueStrings(append(m1, m2...))

	// Sender and reply-to.
	if viaMail {
		b.From, b.Source = mcfg.Display(), mcfg.Source
		b.ReplyTo = mcfg.ReplyTo
		if b.ReplyTo == "" {
			b.ReplyTo = mcfg.FromEmail
		}
	}
	replyNote := ""
	if b.ReplyTo != "" {
		replyNote = "Questions? Just reply to this email — it goes to " + b.ReplyTo + "."
	} else if b.Business.Email != "" {
		replyNote = "Questions? Email " + b.Business.Email + "."
	}

	// Attachment.
	if kind == KindReceipt {
		data, perr := RenderReceiptPDF(&doc, *b.Payment, b.Business)
		b.PDFErr = perr
		b.Attachment = mail.Attachment{Filename: ReceiptFilename(&doc, *b.Payment), ContentType: "application/pdf", Data: data}
	} else {
		data, perr := h.DocumentPDF(tx, &doc, base)
		b.PDFErr = perr
		b.Attachment = mail.Attachment{Filename: PDFFilename(&doc), ContentType: "application/pdf", Data: data}
	}

	b.View = h.emailView(&doc, kind, b, replyNote, s.AccentColor)
	b.HTML, b.Text = RenderEmail(b.View)
	b.Amounts = map[string]string{}
	if b.View.Amount != "" {
		b.Amounts[b.View.AmountLabel] = b.View.Amount
	}
	for _, r := range b.View.Totals {
		b.Amounts[r.Label] = r.Value
	}
	return b, nil
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// emailView lays out the summary card, lines and totals for kind.
func (h *Handler) emailView(doc *Invoice, kind string, b *builtEmail, replyNote, accent string) EmailView {
	m := func(v float64) string { return FormatMoney(v, doc.Currency) }
	v := EmailView{Kind: kind, Accent: accent, Number: doc.Number, Message: b.Message, LogoURL: b.LogoURL, ReplyTo: replyNote,
		Business: EmailBusiness{Name: b.Business.Name, Address: b.Business.Address, Email: b.Business.Email, Phone: b.Business.Phone,
			Website: b.Business.Website, TaxNumberLabel: b.Business.TaxNumberLabel, TaxNumber: b.Business.TaxNumber}}
	if LogoSize != nil && v.LogoURL != "" {
		v.LogoW, v.LogoH = LogoSize(doc.OrganizationID)
	}
	title := DocTitle(doc, b.Business)
	due := pdfDate(doc.DueDate)
	switch kind {
	case KindQuote:
		v.DocLabel = "Quote"
		v.AmountLabel, v.Amount = "Quote total", m(doc.Total)
		if doc.DisplayStatus == StatusExpired {
			v.StatusLine, v.StatusTone = "Expired on "+due, "warn"
		} else {
			v.StatusLine, v.StatusTone = "Valid until "+due, "info"
		}
		v.Facts = []EmailRow{{Label: "Quote number", Value: doc.Number}, {Label: "Issue date", Value: pdfDate(doc.IssueDate)}, {Label: "Valid until", Value: due}}
		v.Preheader = fmt.Sprintf("Quote %s · %s · valid until %s", doc.Number, m(doc.Total), due)
	case KindReceipt:
		p := *b.Payment
		f := figuresAt(doc, p)
		v.DocLabel = "Payment receipt"
		if p.ReceiptNumber != "" {
			v.Number = p.ReceiptNumber
		}
		v.AmountLabel, v.Amount = "Payment received — thank you", m(p.Amount)
		if f.Balance <= 0 {
			v.StatusLine, v.StatusTone = "Paid in full", "ok"
		} else {
			v.StatusLine, v.StatusTone = "Balance remaining "+m(f.Balance), "info"
		}
		if p.RevisedAt != nil {
			v.StatusLine += " · revised " + pdfDate(*p.RevisedAt)
		}
		if p.ReceiptNumber != "" {
			v.Facts = append(v.Facts, EmailRow{Label: "Receipt no.", Value: p.ReceiptNumber})
		}
		v.Facts = append(v.Facts, EmailRow{Label: "Invoice", Value: doc.Number}, EmailRow{Label: "Payment date", Value: pdfDate(p.Date)}, EmailRow{Label: "Method", Value: PaymentMethodLabel(p.Method)})
		if p.Reference != "" {
			v.Facts = append(v.Facts, EmailRow{Label: "Reference", Value: p.Reference})
		}
		v.Facts = append(v.Facts, EmailRow{Label: "Invoice total", Value: m(f.Total)}, EmailRow{Label: "Paid to date", Value: m(f.PaidToDate)})
		v.Preheader = fmt.Sprintf("Thank you — %s received for invoice %s", m(p.Amount), doc.Number)
		if p.ReceiptNumber != "" {
			v.Preheader += " · receipt " + p.ReceiptNumber
		}
	default:
		v.DocLabel = title
		if kind == KindReminder {
			v.DocLabel = "Payment reminder"
		}
		switch {
		case doc.BalanceDue <= 0 && doc.AmountPaid > 0:
			v.AmountLabel, v.Amount = "Invoice total", m(doc.Total)
			v.StatusLine, v.StatusTone = "Paid in full — thank you", "ok"
		default:
			v.AmountLabel, v.Amount = "Amount due", m(doc.BalanceDue)
			switch {
			case doc.DisplayStatus == StatusOverdue:
				days := "day"
				if doc.DaysOverdue != 1 {
					days = "days"
				}
				v.StatusLine, v.StatusTone = fmt.Sprintf("Overdue by %d %s — was due %s", doc.DaysOverdue, days, due), "danger"
			case doc.AmountPaid > 0:
				v.StatusLine, v.StatusTone = "Part-paid — "+m(doc.AmountPaid)+" received", "info"
			default:
				v.StatusLine, v.StatusTone = "Due "+due, "info"
			}
		}
		v.Facts = []EmailRow{{Label: "Invoice number", Value: doc.Number}, {Label: "Issue date", Value: pdfDate(doc.IssueDate)}, {Label: "Due date", Value: due}}
		v.Preheader = fmt.Sprintf("%s %s · %s %s · due %s", title, doc.Number, strings.ToLower(v.AmountLabel), v.Amount, due)
	}
	if doc.Reference != "" && kind != KindReceipt {
		v.Facts = append(v.Facts, EmailRow{Label: "Reference", Value: doc.Reference})
	}
	// Buttons.
	switch {
	case kind == KindQuote:
		v.Actions = []EmailAction{{Label: "View & accept quote", URL: b.ViewURL, Primary: true}}
	case kind == KindReceipt:
		v.Actions = []EmailAction{{Label: "View invoice", URL: b.ViewURL, Primary: false}}
	default:
		v.Actions = []EmailAction{{Label: "View invoice", URL: b.ViewURL, Primary: b.PayURL == ""}}
		if b.PayURL != "" {
			v.Actions = append(v.Actions, EmailAction{Label: "Pay now", URL: b.PayURL, Primary: true})
		}
	}
	if b.ViewURL == "" {
		v.Actions = nil
	}
	// Line items (first five) and totals.
	if kind != KindReceipt {
		for i, it := range doc.Items {
			if i == 5 {
				v.MoreLines = len(doc.Items) - 5
				break
			}
			desc := strings.TrimSpace(strings.SplitN(strings.ReplaceAll(it.Description, "\r\n", "\n"), "\n", 2)[0])
			if r := []rune(desc); len(r) > 90 {
				desc = string(r[:88]) + "…"
			}
			if desc == "" {
				desc = "Item " + strconv.Itoa(i+1)
			}
			detail := qtyText(it.Quantity) + " × " + m(it.UnitPrice)
			if it.DiscountPercent > 0 {
				detail += " · " + pctText(it.DiscountPercent) + " off"
			}
			v.Lines = append(v.Lines, EmailLine{Description: desc, Detail: detail, Amount: m(it.LineTotal)})
		}
		if doc.DiscountTotal > 0 {
			v.Totals = append(v.Totals, EmailRow{Label: "Discount", Value: "−" + m(doc.DiscountTotal)})
		}
		if doc.ShippingAmount > 0 {
			v.Totals = append(v.Totals, EmailRow{Label: "Shipping", Value: m(doc.ShippingAmount)})
		}
		if doc.TaxTotal > 0 {
			lbl := b.Business.TaxLabel
			if lbl == "" {
				lbl = "Tax"
			}
			if doc.PricesIncludeTax {
				lbl += " (included)"
			}
			v.Totals = append(v.Totals, EmailRow{Label: lbl, Value: m(doc.TaxTotal)})
		}
		v.Totals = append(v.Totals, EmailRow{Label: "Total " + doc.Currency, Value: m(doc.Total), Strong: doc.AmountPaid <= 0 || doc.DocType == TypeQuote})
		if doc.DocType == TypeInvoice && doc.AmountPaid > 0 {
			v.Totals = append(v.Totals, EmailRow{Label: "Received", Value: "−" + m(doc.AmountPaid)}, EmailRow{Label: "Balance due", Value: m(doc.BalanceDue), Strong: true})
		}
	}
	if b.Attachment.Filename != "" && b.PDFErr == nil {
		if kind == KindReceipt {
			v.Note = "Your receipt (" + b.Attachment.Filename + ") is attached."
		} else {
			v.Note = "A PDF copy (" + b.Attachment.Filename + ") is attached."
		}
	}
	return v
}

// ---------------------------------------------------------------- checks

// money amounts written in a message: "KSh 1,500.00", "$20", "USD 1,000", "1,000.50 KES".
var (
	moneyTokenRe  *regexp.Regexp
	moneySuffixRe *regexp.Regexp
	symbolToCur   = map[string]string{}
)

func init() {
	codes := []string{}
	for c := range currencySymbols {
		codes = append(codes, c)
	}
	for _, c := range []string{"USD", "EUR", "GBP", "JPY", "CHF", "CAD", "AUD", "NZD", "CNY", "INR", "SGD", "HKD", "AED", "SAR", "KWD", "ZAR", "NGN", "EGP", "KES", "GHS", "MAD", "TZS", "UGX"} {
		symbolToCur[c] = c
	}
	syms := []string{}
	for c, s := range currencySymbols {
		s = strings.TrimSpace(s)
		if s == "" || s == "R" || s == c {
			continue
		}
		if _, dup := symbolToCur[s]; !dup {
			symbolToCur[s] = c
		}
		syms = append(syms, s)
	}
	syms = append(syms, "$", "¥", "Ksh", "KShs", "Kshs")
	symbolToCur["$"], symbolToCur["¥"], symbolToCur["Ksh"], symbolToCur["KShs"], symbolToCur["Kshs"] = "USD", "JPY", "KES", "KES", "KES"
	all := append(codes, syms...)
	sort.Slice(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	q := make([]string, 0, len(all))
	seen := map[string]bool{}
	for _, s := range all {
		if !seen[s] {
			seen[s] = true
			q = append(q, regexp.QuoteMeta(s))
		}
	}
	num := `(\d{1,3}(?:,\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?)`
	moneyTokenRe = regexp.MustCompile(`(` + strings.Join(q, "|") + `)\s?` + num)
	moneySuffixRe = regexp.MustCompile(num + `\s?(` + strings.Join(codes, "|") + `)\b`)
}

type moneyMention struct {
	Text     string
	Currency string
	Value    float64
}

// findAmounts lists the money amounts written in s. A bare "$" counts as the
// document's currency when that is a dollar currency.
func findAmounts(s, docCur string) []moneyMention {
	var out []moneyMention
	taken := map[int]bool{}
	for _, m := range moneyTokenRe.FindAllStringSubmatchIndex(s, -1) {
		sym := s[m[2]:m[3]]
		// A code must not be the tail of a word ("BUSD 5").
		if m[2] > 0 && isWordByte(s[m[2]-1]) && isWordByte(sym[0]) {
			continue
		}
		cur := symbolToCur[sym]
		if strings.HasSuffix(sym, "$") && sym == "$" && strings.HasSuffix(strings.TrimSpace(currencySymbols[docCur]), "$") {
			cur = docCur
		}
		if sym == "¥" && docCur == "CNY" {
			cur = "CNY"
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(s[m[4]:m[5]], ",", ""), 64)
		if err != nil || cur == "" {
			continue
		}
		taken[m[0]] = true
		out = append(out, moneyMention{Text: s[m[0]:m[1]], Currency: cur, Value: v})
	}
	for _, m := range moneySuffixRe.FindAllStringSubmatchIndex(s, -1) {
		if taken[m[0]] {
			continue
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(s[m[2]:m[3]], ",", ""), 64)
		if err != nil {
			continue
		}
		out = append(out, moneyMention{Text: s[m[0]:m[1]], Currency: s[m[4]:m[5]], Value: v})
	}
	return out
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// knownAmounts are every amount that legitimately appears on doc.
func knownAmounts(doc *Invoice, pay *Payment) []float64 {
	out := []float64{doc.Total, doc.BalanceDue, doc.AmountPaid, doc.Subtotal, doc.TaxTotal, doc.DiscountTotal, doc.ShippingAmount, depositTotal(doc)}
	for _, p := range doc.Payments {
		out = append(out, p.Amount)
	}
	for _, it := range doc.Items {
		out = append(out, it.LineTotal, it.UnitPrice, it.LineTax, it.LineSubtotal)
	}
	if pay != nil {
		f := figuresAt(doc, *pay)
		out = append(out, pay.Amount, f.PaidToDate, f.Balance)
	}
	return out
}

func matchesAny(v float64, list []float64, cur string) bool {
	tol := 0.5 / math.Pow10(currencyDecimalsOf(cur))
	for _, k := range list {
		if math.Abs(roundCur(k, cur)-v) < tol {
			return true
		}
	}
	return false
}

func currencyDecimalsOf(cur string) int {
	switch strings.ToUpper(cur) {
	case "JPY", "UGX":
		return 0
	case "KWD":
		return 3
	}
	return 2
}

func hasMoreDecimals(v float64, cur string) bool {
	return math.Abs(roundCur(v, cur)-v) > 1e-9*math.Max(1, math.Abs(v))
}

func ck(id, sev, title, detail string) Check {
	return Check{ID: id, Severity: sev, Title: title, Detail: detail}
}

// checkOptions describe the send being checked.
type checkOptions struct {
	Mode       string // email | mailto | test | mark
	Configured bool
	MailCfg    mail.Config
	ViaMail    bool
}

// runChecks validates stored document inv and (when b is not nil) the email
// built for it. Errors block sending, warnings need "Send anyway".
func (h *Handler) runChecks(tx *gorm.DB, inv *Invoice, b *builtEmail, o checkOptions) []Check {
	var out []Check
	add := func(c Check) { out = append(out, c) }
	cur := inv.Currency
	m := func(v float64) string { return FormatMoney(v, cur) }
	today := dateOnly(h.now())

	if b != nil && o.Mode != "mark" {
		// Recipients.
		switch {
		case o.Mode == "test":
			add(ck("recipients", "info", "Test send", "Only you receive this copy — the client and CCs are not emailed."))
		case b.To == "" && o.Mode == "mailto":
			add(ck("recipients", "warning", "No recipient", "Your email app will open without a To address."))
		case b.To == "":
			add(ck("recipients", "error", "No recipient", "Add the client's email address."))
		default:
			d := "To " + b.To
			if len(b.CC) > 0 {
				d += " · CC " + strings.Join(b.CC, ", ")
			}
			if len(b.BCC) > 0 {
				d += " · BCC " + strings.Join(b.BCC, ", ")
			}
			add(ck("recipients", "ok", "Recipients", d))
		}
		if len(b.Skipped) > 0 {
			add(ck("recipients_skipped", "warning", "Invalid addresses will be skipped", strings.Join(b.Skipped, ", ")))
		}
		// Sender.
		switch {
		case o.Mode == "mailto":
			add(ck("sender", "info", "Sent from your email app", "Nothing is sent by the server — your email app opens with this message."))
		case !o.Configured:
			d := "Set up outgoing email under Settings → Outgoing email."
			if o.ViaMail && o.MailCfg.Problem != "" {
				d = o.MailCfg.Problem
			}
			add(ck("sender", "error", "Email sending isn't set up", d))
		case o.ViaMail:
			add(ck("sender", "ok", "Sender", "From "+o.MailCfg.Display()+" ("+mail.SourceLabel(o.MailCfg.Source)+")"))
		default:
			add(ck("sender", "ok", "Sender", "Server email"))
		}
	}

	// Totals recalculate identically.
	re := *inv
	re.Items = append([]InvoiceItem(nil), inv.Items...)
	Recalculate(&re)
	ApplyPayments(&re, h.now())
	var diffs []string
	cmp := func(label string, stored, fresh float64) {
		if math.Abs(stored-fresh) > 0.4/math.Pow10(currencyDecimalsOf(cur)) {
			diffs = append(diffs, fmt.Sprintf("%s stored %s, recalculated %s", label, m(stored), m(fresh)))
		}
	}
	cmp("Subtotal", inv.Subtotal, re.Subtotal)
	cmp("Discount", inv.DiscountTotal, re.DiscountTotal)
	cmp("Tax", inv.TaxTotal, re.TaxTotal)
	cmp("Total", inv.Total, re.Total)
	cmp("Paid", inv.AmountPaid, re.AmountPaid)
	cmp("Balance due", inv.BalanceDue, re.BalanceDue)
	if len(diffs) > 0 {
		add(ck("totals", "error", "Totals don't recalculate to the stored amounts", strings.Join(diffs, "; ")+". Open the editor and save to recalculate."))
	} else {
		add(ck("totals", "ok", "Totals verified", "Total "+m(inv.Total)+" · balance due "+m(inv.BalanceDue)+" (recalculated on the server)"))
	}

	// Currency decimals.
	var bad []string
	chk := func(label string, v float64) {
		if hasMoreDecimals(v, cur) {
			bad = append(bad, fmt.Sprintf("%s %s", label, strconv.FormatFloat(v, 'f', -1, 64)))
		}
	}
	chk("total", inv.Total)
	chk("balance", inv.BalanceDue)
	chk("paid", inv.AmountPaid)
	chk("shipping", inv.ShippingAmount)
	chk("tax", inv.TaxTotal)
	for _, p := range inv.Payments {
		chk("payment", p.Amount)
	}
	for _, it := range inv.Items {
		chk("line", it.LineTotal)
	}
	d := currencyDecimalsOf(cur)
	if len(bad) > 0 {
		add(ck("decimals", "error", fmt.Sprintf("Amounts have more than %d decimals for %s", d, cur), strings.Join(bad, ", ")))
	} else {
		add(ck("decimals", "ok", "Currency decimals", fmt.Sprintf("%s amounts use %d decimal places", cur, d)))
	}

	// Exchange rate.
	switch {
	case inv.BaseCurrency == "" || inv.Currency == inv.BaseCurrency:
		add(ck("fx", "ok", "Currency", "In your home currency ("+cur+") — no conversion"))
	case inv.ExchangeRate <= 0:
		add(ck("fx", "error", "No exchange rate locked", cur+" document without a rate to "+inv.BaseCurrency+". Use “Update rate” (or enter a rate) before sending."))
	default:
		when := "—"
		if inv.RateDate != nil {
			when = inv.RateDate.In(time.UTC).Format("2 Jan 2006 15:04") + " UTC"
		}
		desc := RateText(inv) + " · " + inv.RateSource + " rate from " + when
		switch {
		case inv.RateSource == fxrates.SourceFallback:
			add(ck("fx", "warning", "Approximate exchange rate", desc+". Live rates were unavailable — check it or enter the rate you want."))
		case inv.RateSource == fxrates.SourceManual:
			add(ck("fx", "warning", "Manual exchange rate", desc+". Make sure it is the rate you agreed."))
		case inv.Status == StatusDraft && inv.RateDate != nil && h.now().Sub(*inv.RateDate) > fxrates.StaleAfter:
			add(ck("fx", "warning", "Exchange rate is more than 24 hours old", desc+". Use “Update rate” in the editor for today's rate."))
		default:
			add(ck("fx", "ok", "Exchange rate locked", desc))
		}
		// Converted lines agree with the rate.
		var off []string
		for i, it := range inv.Items {
			if it.OriginalUnitPrice != nil && strings.EqualFold(it.OriginalCurrency, inv.BaseCurrency) {
				want := fxrates.Convert(*it.OriginalUnitPrice, inv.ExchangeRate, cur)
				if math.Abs(want-it.UnitPrice) > 0.4/math.Pow10(d) {
					off = append(off, fmt.Sprintf("line %d: %s (expected %s from %s)", i+1, m(it.UnitPrice), m(want), FormatMoney(*it.OriginalUnitPrice, inv.BaseCurrency)))
				}
			}
		}
		if len(off) > 0 {
			add(ck("fx_lines", "warning", "Converted prices don't match the locked rate", strings.Join(off, "; ")))
		}
	}

	if o.Mode == "mark" || b == nil {
		return out
	}

	// Dates.
	due := dateOnly(inv.DueDate)
	switch {
	case inv.DocType == TypeInvoice && inv.Status == StatusDraft && !due.IsZero() && due.Before(today):
		add(ck("due_date", "warning", "Due date is in the past", "This new invoice is due "+pdfDate(inv.DueDate)+" — it will show as overdue straight away."))
	case inv.DocType == TypeQuote && !due.IsZero() && due.Before(today):
		add(ck("due_date", "warning", "Quote has expired", "Valid until "+pdfDate(inv.DueDate)+". Extend it before sending."))
	default:
		add(ck("due_date", "ok", "Dates", "Issued "+pdfDate(inv.IssueDate)+" · due "+pdfDate(inv.DueDate)))
	}
	// Tax.
	switch {
	case inv.TaxTotal > 0 && b.Business.TaxNumber == "":
		add(ck("tax", "warning", "Tax charged but no tax number", "Add your tax number under Invoicing settings so the document is a valid tax invoice."))
	case inv.TaxTotal > 0:
		add(ck("tax", "ok", "Tax", m(inv.TaxTotal)+" · "+b.Business.TaxNumberLabel+" "+b.Business.TaxNumber))
	default:
		add(ck("tax", "info", "No tax on this document", ""))
	}
	// Logo.
	switch {
	case b.LogoURL == "":
		add(ck("logo", "warning", "No company logo", "The email header shows your business name instead. Upload a logo under Settings → Business details."))
	case !strings.HasPrefix(b.LogoURL, "https://"):
		add(ck("logo", "info", "Logo is not on https", "Some email apps block images that aren't served over https: "+b.LogoURL))
	default:
		add(ck("logo", "ok", "Logo", "Shown in the email header"))
	}
	// Links.
	if b.ViewURL == "" {
		add(ck("links", "warning", "No online link", "The app address is unknown (set PUBLIC_APP_URL) — the email has no View button."))
	}
	if inv.DocType == TypeInvoice && b.Kind != KindReceipt {
		switch {
		case !h.onlinePay(inv.OrganizationID):
			add(ck("pay_link", "info", "Online payments are off", "No Pay now button — the client pays as described on the invoice."))
		case b.PayURL != "":
			add(ck("pay_link", "ok", "Pay now button included", b.PayURL))
		case b.Doc.BalanceDue > 0:
			add(ck("pay_link", "warning", "Pay link unavailable", "Online payments are on but the pay link could not be built."))
		}
	}
	// PDF.
	if b.PDFErr != nil {
		add(ck("pdf", "error", "The PDF could not be created", b.PDFErr.Error()))
	} else if len(b.Attachment.Data) > 0 {
		add(ck("pdf", "ok", "PDF attached", fmt.Sprintf("%s · %d KB", b.Attachment.Filename, (len(b.Attachment.Data)+1023)/1024)))
	}
	// Placeholders.
	if len(b.Unresolved) > 0 {
		names := make([]string, len(b.Unresolved))
		for i, n := range b.Unresolved {
			names[i] = "{{" + n + "}}"
		}
		add(ck("placeholders", "error", "Unknown placeholders in the subject or message", strings.Join(names, ", ")+" would be sent as is. Remove them or use one of the listed placeholders."))
	} else {
		add(ck("placeholders", "ok", "Placeholders filled", "No {{…}} left in the subject or message"))
	}
	// Amounts written in the subject / message.
	known := knownAmounts(b.Doc, b.Payment)
	var wrong, other []string
	matched := 0
	for _, part := range []struct{ where, text string }{{"subject", b.Subject}, {"message", b.Message}} {
		for _, a := range findAmounts(part.text, cur) {
			switch {
			case a.Currency == cur:
				if matchesAny(a.Value, known, cur) {
					matched++
				} else {
					wrong = append(wrong, "“"+a.Text+"” in the "+part.where)
				}
			case a.Currency == inv.BaseCurrency && inv.ExchangeRate > 0:
				if tb, okb := ToBase(inv, inv.Total); okb && math.Abs(tb-a.Value) < 0.5/math.Pow10(currencyDecimalsOf(a.Currency)) {
					matched++
				} else {
					other = append(other, "“"+a.Text+"” ("+a.Currency+") in the "+part.where)
				}
			default:
				other = append(other, "“"+a.Text+"” ("+a.Currency+") in the "+part.where)
			}
		}
	}
	summary := "total " + m(b.Doc.Total) + ", balance due " + m(b.Doc.BalanceDue)
	if b.Doc.AmountPaid > 0 {
		summary += ", paid " + m(b.Doc.AmountPaid)
	}
	if len(wrong) > 0 {
		add(ck("amounts", "error", "An amount in the email doesn't match "+inv.Number, strings.Join(wrong, "; ")+" — "+inv.Number+" has "+summary+". Use {{total}} / {{amount_due}} or correct the text."))
	} else if matched > 0 {
		add(ck("amounts", "ok", "Amounts match the "+strings.ToLower(docLabel(inv.DocType)), summary))
	} else {
		add(ck("amounts", "ok", "Amounts", "Shown from the "+strings.ToLower(docLabel(inv.DocType))+": "+summary))
	}
	if len(other) > 0 {
		add(ck("amounts_currency", "warning", "The message mentions another currency", strings.Join(other, "; ")+". The "+strings.ToLower(docLabel(inv.DocType))+" is in "+cur+"."))
	}
	// Balance.
	switch {
	case b.Kind == KindReminder && b.Doc.BalanceDue <= 0:
		add(ck("balance", "error", "Nothing to remind about", "The invoice has no balance due."))
	case inv.DocType == TypeInvoice && b.Kind != KindReceipt && b.Doc.BalanceDue <= 0 && inv.Total > 0:
		add(ck("balance", "info", "Paid in full", "The email says the invoice is paid — it doesn't ask for payment."))
	}
	if inv.Total <= 0 {
		add(ck("zero", "warning", "The total is zero", ""))
	}
	// Duplicate send.
	if o.Mode != "test" && tx.Migrator().HasTable(&EmailLog{}) {
		var last EmailLog
		if err := tx.Where("invoice_id = ? AND kind = ? AND status = ? AND mode = ? AND created_at > ?", inv.ID, b.Kind, "sent", "email", h.now().Add(-10*time.Minute)).
			Order("created_at DESC").Limit(1).Find(&last).Error; err == nil && last.ID != "" {
			ago := "just now"
			if mins := int(h.now().Sub(last.CreatedAt).Minutes()); mins == 1 {
				ago = "1 minute ago"
			} else if mins > 1 {
				ago = fmt.Sprintf("%d minutes ago", mins)
			}
			add(ck("duplicate", "warning", "Already emailed a few minutes ago", fmt.Sprintf("This %s was emailed to %s %s.", b.Kind, last.To, ago)))
		}
	}
	return out
}

func countSeverity(checks []Check, sev string) int {
	n := 0
	for _, c := range checks {
		if c.Severity == sev {
			n++
		}
	}
	return n
}

func firstOf(checks []Check, sev string) *Check {
	for i := range checks {
		if checks[i].Severity == sev {
			return &checks[i]
		}
	}
	return nil
}

// checkError is a blocked send: the checks travel with the error.
type checkError struct {
	apiError
	checks []Check
}

func (e *checkError) Error() string { return e.msg }

func blocked(checks []Check, warningsOnly bool) error {
	if !warningsOnly {
		c := firstOf(checks, "error")
		msg := "Fix the problems found before sending"
		if c != nil {
			msg = c.Title
			if c.Detail != "" {
				msg += ": " + c.Detail
			}
		}
		return &checkError{apiError{http.StatusUnprocessableEntity, "PREFLIGHT_FAILED", msg}, checks}
	}
	n := countSeverity(checks, "warning")
	return &checkError{apiError{http.StatusConflict, "PREFLIGHT_WARNINGS", fmt.Sprintf("%d warning(s) — review them and choose “Send anyway”", n)}, checks}
}

// ---------------------------------------------------------------- endpoints

// preview is what the send dialog shows before sending.
func previewOf(b *builtEmail, token string) gin.H {
	atts := []gin.H{}
	if b.PDFErr == nil && len(b.Attachment.Data) > 0 {
		atts = append(atts, gin.H{"filename": b.Attachment.Filename, "size": len(b.Attachment.Data), "content_type": "application/pdf"})
	}
	cc, bcc := b.CC, b.BCC
	if cc == nil {
		cc = []string{}
	}
	if bcc == nil {
		bcc = []string{}
	}
	return gin.H{"kind": b.Kind, "subject": b.Subject, "message": b.Message, "html": b.HTML, "text": b.Text, "to": b.To, "cc": cc, "bcc": bcc,
		"from": b.From, "source": b.Source, "reply_to": b.ReplyTo, "attachments": atts, "view_url": b.ViewURL, "pay_url": b.PayURL,
		"amounts": b.Amounts, "payment_id": paymentID(b.Payment)}
}

func paymentID(p *Payment) string {
	if p == nil {
		return ""
	}
	return p.ID
}

// Preflight serves POST /invoices/:id/preflight: the checks and an exact
// preview of the email (same body as /send).
func (h *Handler) Preflight(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in sendInput
	_ = c.ShouldBindJSON(&in)
	mcfg, viaMail := h.mailConfig(org)
	configured := h.emailEnabled(org)
	inv, err := h.load(h.DB, org, c.Param("id"), false)
	if err != nil {
		failErr(c, err)
		return
	}
	mode := in.Mode
	switch mode {
	case "mailto", "test", "mark":
	default:
		mode = "email"
		if !configured && in.Mode == "" {
			mode = "mailto"
		}
	}
	if mode == "mark" {
		checks := h.runChecks(h.DB, inv, nil, checkOptions{Mode: "mark"})
		ok(c, http.StatusOK, gin.H{"checks": checks, "errors": countSeverity(checks, "error"), "warnings": countSeverity(checks, "warning"), "ok": countSeverity(checks, "error") == 0})
		return
	}
	if kindOf(inv, in.Kind) == KindReminder && inv.Status != StatusSent && inv.Status != StatusPartial {
		fail(c, http.StatusConflict, "NOT_REMINDABLE", "Reminders can only be sent for unpaid invoices")
		return
	}
	if mode == "test" {
		in.To, in.CC, in.BCC = h.userEmail(ctxString(c, "user_id")), &[]string{}, &[]string{}
	}
	b, err := h.buildEmail(h.DB, c, inv, in, mcfg, viaMail)
	if err != nil {
		failErr(c, err)
		return
	}
	checks := h.runChecks(h.DB, inv, b, checkOptions{Mode: mode, Configured: configured, MailCfg: mcfg, ViaMail: viaMail})
	ok(c, http.StatusOK, gin.H{"checks": checks, "errors": countSeverity(checks, "error"), "warnings": countSeverity(checks, "warning"),
		"ok": countSeverity(checks, "error") == 0, "mode": mode, "preview": previewOf(b, inv.PublicToken), "email_enabled": configured})
}

// EmailDraft serves GET /invoices/:id/email-draft?kind=&payment_id=: the
// subject and message the send dialog starts from, filled on the server.
func (h *Handler) EmailDraft(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	inv, err := h.load(h.DB, org, c.Param("id"), false)
	if err != nil {
		failErr(c, err)
		return
	}
	kind := kindOf(inv, c.Query("kind"))
	s, _ := h.settingsFor(h.DB, org, false)
	if s == nil {
		s = &Settings{}
	}
	doc := *inv
	if (kind == KindInvoice || kind == KindQuote) && doc.Status == StatusDraft {
		doc.Status = StatusSent
	}
	Derive(&doc, h.now())
	var pay *Payment
	if kind == KindReceipt {
		for i := range inv.Payments {
			if inv.Payments[i].ID == c.Query("payment_id") {
				p := inv.Payments[i]
				pay = &p
			}
		}
		if pay == nil {
			fail(c, http.StatusNotFound, "NOT_FOUND", "Payment not found")
			return
		}
	}
	base := h.AppBase(c, c.Query("app_url"))
	view := ViewURL(base, doc.PublicToken)
	payURL := ""
	if doc.DocType == TypeInvoice && h.payable(&doc) {
		payURL = PayURL(base, doc.PublicToken)
	}
	biz := PDFBusinessFor(h.DB, s, org).Name
	vars := emailVars(&doc, biz, view, payURL, pay)
	subjT, msgT := defaultTemplates(s, &doc, kind)
	if kind == KindReceipt && pay.RevisedAt != nil {
		subjT = "Revised: " + subjT
	}
	ph := []gin.H{}
	for _, p := range Placeholders {
		ph = append(ph, gin.H{"name": p.Name, "help": p.Help, "value": vars[p.Name]})
	}
	ok(c, http.StatusOK, gin.H{"kind": kind, "subject": renderTemplate(subjT, vars), "message": renderTemplate(msgT, vars),
		"subject_template": subjT, "message_template": msgT, "placeholders": ph})
}

// userEmail is the signed-in user's address ("" when unknown).
func (h *Handler) userEmail(user string) string {
	if user == "" || !h.DB.Migrator().HasTable("users") {
		return ""
	}
	var e string
	h.DB.Raw("SELECT email FROM users WHERE id::text = ? LIMIT 1", user).Scan(&e)
	return strings.ToLower(strings.TrimSpace(e))
}

// SendTest serves POST /invoices/:id/send-test: the same email, sent only to
// the signed-in user (subject prefixed "[Test]"). Nothing about the document
// changes.
func (h *Handler) SendTest(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in sendInput
	_ = c.ShouldBindJSON(&in)
	user := ctxString(c, "user_id")
	me := h.userEmail(user)
	if me == "" {
		fail(c, http.StatusBadRequest, "NO_EMAIL", "Your user account has no email address")
		return
	}
	mcfg, viaMail := h.mailConfig(org)
	if !h.emailEnabled(org) || !viaMail {
		fail(c, http.StatusServiceUnavailable, "SMTP_NOT_CONFIGURED", "Email sending isn't set up yet — a test can't be sent.")
		return
	}
	inv, err := h.load(h.DB, org, c.Param("id"), false)
	if err != nil {
		failErr(c, err)
		return
	}
	in.To, in.CC, in.BCC = me, &[]string{}, &[]string{}
	b, err := h.buildEmail(h.DB, c, inv, in, mcfg, viaMail)
	if err != nil {
		failErr(c, err)
		return
	}
	checks := h.runChecks(h.DB, inv, b, checkOptions{Mode: "test", Configured: true, MailCfg: mcfg, ViaMail: true})
	if b.PDFErr != nil {
		fail(c, http.StatusInternalServerError, "PDF_FAILED", "Could not create the PDF: "+b.PDFErr.Error())
		return
	}
	entry := h.newLog(inv, b, "test", user, me, checks)
	msg := mail.Message{To: []string{me}, Subject: "[Test] " + b.Subject, Text: b.Text, HTML: b.HTML,
		Attachments: []mail.Attachment{b.Attachment}, MessageID: mail.NewMessageID(mcfg.FromEmail), ReplyTo: mcfg.ReplyTo}
	entry.Subject, entry.MessageID = msg.Subject, msg.MessageID
	start := time.Now()
	sendErr := h.Mail.SendWith(c.Request.Context(), org, mcfg, msg)
	entry.DurationMS = int(time.Since(start).Milliseconds())
	if sendErr != nil {
		entry.Status, entry.Error, entry.ErrorCode = "failed", sendErr.Error(), mail.Code(sendErr)
		h.DB.Create(entry)
		fail(c, http.StatusBadGateway, "EMAIL_FAILED", "The test email could not be sent: "+sendErr.Error())
		return
	}
	entry.Status, entry.Response = "sent", "250 accepted by "+mcfg.Host
	h.DB.Create(entry)
	h.log(h.DB, inv, "email_test", "Test email sent to "+me, user)
	ok(c, http.StatusOK, gin.H{"to": me, "subject": msg.Subject, "message_id": msg.MessageID, "log": entry, "checks": checks})
}

func (h *Handler) newLog(inv *Invoice, b *builtEmail, mode, user, userEmail string, checks []Check) *EmailLog {
	e := &EmailLog{ID: uuid.NewString(), OrganizationID: inv.OrganizationID, InvoiceID: inv.ID, Kind: b.Kind, Mode: mode, UserID: user, UserEmail: userEmail,
		FromAddress: b.From, Source: b.Source, To: b.To, CC: b.CC, BCC: b.BCC, Subject: b.Subject, Checks: checks, Amounts: b.Amounts,
		PaymentID: paymentID(b.Payment), CreatedAt: h.now()}
	if e.CC == nil {
		e.CC = []string{}
	}
	if e.BCC == nil {
		e.BCC = []string{}
	}
	if b.PDFErr == nil && b.Attachment.Filename != "" {
		e.Attachments = []string{fmt.Sprintf("%s (%d KB)", b.Attachment.Filename, (len(b.Attachment.Data)+1023)/1024)}
	}
	return e
}

// EmailLogList serves GET /invoices/:id/email-log (newest first).
func (h *Handler) EmailLogList(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	inv, err := h.load(h.DB, org, c.Param("id"), false)
	if err != nil {
		failErr(c, err)
		return
	}
	list := []EmailLog{}
	h.DB.Where("organization_id = ? AND invoice_id = ?", org, inv.ID).Order("created_at DESC").Limit(100).Find(&list)
	st := EmailStatuses(h.DB, org, []Invoice{*inv})[inv.ID]
	ok(c, http.StatusOK, gin.H{"log": list, "summary": st, "viewed_at": inv.ViewedAt, "timezone": orgprefs.Get(h.DB, org).Timezone})
}
