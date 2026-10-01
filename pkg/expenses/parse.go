package expenses

import (
	"html"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Extracted is what the rules (or AI) found in a document's text.
type Extracted struct {
	Kind          string
	VendorKey     string // synergy | google_workspace | "" (unknown)
	InvoiceNumber string
	Date          *time.Time // invoice (or payment) date
	PaidDate      *time.Time
	PeriodStart   *time.Time
	PeriodEnd     *time.Time
	Currency      string
	Total         float64
	Tax           float64
	HasTotal      bool
	HasTax        bool
	TaxNote       string
	Service       string
	Paid          *bool
	Balance       *float64 // account balance mentioned (Synergy low-balance notices)
	Ignore        string   // reason to ignore (statement email, notice …)
	Confidence    float64
	Method        string
	Warnings      []string
}

// ---------- money ----------

var currencySymbols = []struct{ sym, code string }{
	{"A$", "AUD"}, {"AU$", "AUD"}, {"AUD$", "AUD"}, {"US$", "USD"}, {"NZ$", "NZD"}, {"C$", "CAD"}, {"CA$", "CAD"}, {"S$", "SGD"},
	{"KSh", "KES"}, {"Ksh", "KES"}, {"€", "EUR"}, {"£", "GBP"}, {"¥", "JPY"}, {"₹", "INR"}, {"R ", "ZAR"},
}

// moneyRe matches an amount with an optional currency code/symbol before or after.
var moneyRe = regexp.MustCompile(`(?i)(-)?\s*((?:[A-Z]{3}\s?)?(?:A\$|AU\$|AUD\$|US\$|NZ\$|CA\$|C\$|S\$|KSh|\$|€|£|¥|₹)?)\s?(-)?\s?(\d{1,3}(?:[,\s]\d{3})+(?:\.\d{1,3})?|\d+(?:\.\d{1,3})?)\s?((?:[A-Z]{3})\b)?`)

// ParseMoney reads the first amount in s: value, currency ("" when only "$"
// or nothing) and whether one was found.
func ParseMoney(s string) (float64, string, bool) {
	m := moneyRe.FindStringSubmatch(s)
	if m == nil {
		return 0, "", false
	}
	num := strings.NewReplacer(",", "", " ", "").Replace(m[4])
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, "", false
	}
	if m[1] != "" || m[3] != "" {
		v = -v
	}
	cur := ""
	prefix := strings.TrimSpace(m[2])
	for _, cs := range currencySymbols {
		if strings.HasSuffix(prefix, strings.TrimSpace(cs.sym)) && strings.TrimSpace(cs.sym) != "R" {
			cur = cs.code
			break
		}
	}
	if len(prefix) >= 3 && isCode(strings.ToUpper(prefix[:3])) {
		cur = strings.ToUpper(prefix[:3])
	}
	if m[5] != "" && isCode(strings.ToUpper(m[5])) {
		cur = strings.ToUpper(m[5])
	}
	return math.Round(v*1000) / 1000, cur, true
}

var knownCodes = map[string]bool{"AUD": true, "USD": true, "EUR": true, "GBP": true, "NZD": true, "KES": true, "CAD": true, "SGD": true, "JPY": true,
	"INR": true, "ZAR": true, "UGX": true, "TZS": true, "RWF": true, "NGN": true, "GHS": true, "CHF": true, "HKD": true, "AED": true}

func isCode(s string) bool { return knownCodes[s] }

// ---------- dates ----------

var months = map[string]time.Month{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12}

var (
	dateMDY = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sept?|oct|nov|dec)[a-z]*\.?\s+(\d{1,2}),?\s+(\d{4})\b`)
	dateDMY = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(jan|feb|mar|apr|may|jun|jul|aug|sept?|oct|nov|dec)[a-z]*\.?,?\s+(\d{4})\b`)
	dateISO = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	dateNum = regexp.MustCompile(`\b(\d{1,2})[/.](\d{1,2})[/.](\d{4})\b`)
)

func mkDate(y int, m time.Month, d int) *time.Time {
	if y < 2000 || y > 2100 || m < 1 || m > 12 || d < 1 || d > 31 {
		return nil
	}
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if t.Month() != m {
		return nil
	}
	return &t
}

// ParseDate reads the first date in s. Numeric dates are day-first
// (Australia / Kenya / Europe) unless the first number is above 12.
func ParseDate(s string) *time.Time {
	type hit struct {
		pos int
		t   *time.Time
	}
	var hits []hit
	if m := dateMDY.FindStringSubmatchIndex(s); m != nil {
		y, _ := strconv.Atoi(s[m[6]:m[7]])
		d, _ := strconv.Atoi(s[m[4]:m[5]])
		if t := mkDate(y, months[strings.ToLower(s[m[2]:m[3]])[:3]], d); t != nil {
			hits = append(hits, hit{m[0], t})
		}
	}
	if m := dateDMY.FindStringSubmatchIndex(s); m != nil {
		y, _ := strconv.Atoi(s[m[6]:m[7]])
		d, _ := strconv.Atoi(s[m[2]:m[3]])
		if t := mkDate(y, months[strings.ToLower(s[m[4]:m[5]])[:3]], d); t != nil {
			hits = append(hits, hit{m[0], t})
		}
	}
	if m := dateISO.FindStringSubmatchIndex(s); m != nil {
		y, _ := strconv.Atoi(s[m[2]:m[3]])
		mo, _ := strconv.Atoi(s[m[4]:m[5]])
		d, _ := strconv.Atoi(s[m[6]:m[7]])
		if t := mkDate(y, time.Month(mo), d); t != nil {
			hits = append(hits, hit{m[0], t})
		}
	}
	if m := dateNum.FindStringSubmatchIndex(s); m != nil {
		a, _ := strconv.Atoi(s[m[2]:m[3]])
		b, _ := strconv.Atoi(s[m[4]:m[5]])
		y, _ := strconv.Atoi(s[m[6]:m[7]])
		d, mo := a, b
		if b > 12 && a <= 12 {
			d, mo = b, a
		}
		if t := mkDate(y, time.Month(mo), d); t != nil {
			hits = append(hits, hit{m[0], t})
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })
	return hits[0].t
}

// ---------- text helpers ----------

var (
	tagBreak = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/tr|/h[1-6]|/li|/table)\s*/?>`)
	tagCell  = regexp.MustCompile(`(?i)<\s*/t[dh]\s*>`)
	tagStyle = regexp.MustCompile(`(?is)<(style|script|head)[^>]*>.*?</(style|script|head)>`)
	tagAny   = regexp.MustCompile(`(?s)<[^>]*>`)
	spaces   = regexp.MustCompile(`[ \t\x{00a0}]+`)
	blankLns = regexp.MustCompile(`\n\s*\n+`)
)

// HTMLToText converts an HTML email body to readable plain text.
func HTMLToText(h string) string {
	h = tagStyle.ReplaceAllString(h, " ")
	h = tagBreak.ReplaceAllString(h, "\n")
	h = tagCell.ReplaceAllString(h, " \t ")
	h = tagAny.ReplaceAllString(h, " ")
	h = html.UnescapeString(h)
	return CleanText(h)
}

// CleanText normalises whitespace line by line.
func CleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spaces.ReplaceAllString(l, " "))
	}
	return strings.TrimSpace(blankLns.ReplaceAllString(strings.Join(lines, "\n"), "\n"))
}

// lineAfter returns the rest of the first line matching re (case-insensitive label).
func findGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[len(m)-1])
	}
	return ""
}

// amountAfter finds the money amount after the first match of label: on the
// same line, else on the next line (PDF tables put values on their own line).
func amountAfter(label *regexp.Regexp, s string) (float64, string, bool) {
	loc := label.FindStringIndex(s)
	if loc == nil {
		return 0, "", false
	}
	rest := s[loc[1]:]
	lines := strings.SplitN(rest, "\n", 3)
	for i, l := range lines {
		if i > 1 {
			break
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		if v, c, ok := ParseMoney(l); ok {
			return v, c, true
		}
		if i == 0 && strings.TrimSpace(l) != "" && !strings.ContainsAny(l, "0123456789") && len(strings.TrimSpace(l)) > 3 {
			return 0, "", false // the label is followed by other words
		}
	}
	return 0, "", false
}

// ---------- rules ----------

var (
	reSynergyReceiptSubj = regexp.MustCompile(`(?i)payment receipt for invoice\s*#?\s*(\d+)`)
	reSynergyInvoiceSubj = regexp.MustCompile(`(?i)(?:customer|new)?\s*invoice\s*#?\s*(\d{5,})`)
	reSynergyLowBal      = regexp.MustCompile(`(?i)your balance is currently\s*(-?\s*\$?\s*-?[\d,]+\.\d{2})`)
	reGoogleInvoiceSubj  = regexp.MustCompile(`(?i)google workspace:\s*your invoice is available`)
	reGooglePaidSubj     = regexp.MustCompile(`(?i)google workspace:\s*we've received your payment|payment received`)
	reGoogleDeclinedSubj = regexp.MustCompile(`(?i)payment (was )?declined|payment failed|couldn't process your payment|unable to process your payment`)
	reGooglePaidBody     = regexp.MustCompile(`(?i)your payment of\s+([^\s]+(?:\s?[A-Z]{3})?)\s+was applied to (.+?) on ([A-Za-z]{3,9}\.? \d{1,2},? \d{4})`)
	reGoogleDomain       = regexp.MustCompile(`(?i)(?:invoice is available for|payment for|account for)\s+([a-z0-9.-]+\.[a-z]{2,})`)

	reInvNo = []*regexp.Regexp{
		regexp.MustCompile(`(?i)invoice\s*(?:number|no\.?|num|#)\s*[:#]?\s*([A-Z0-9][A-Z0-9-]{3,})`),
		regexp.MustCompile(`(?i)invoice\s*#\s*([A-Z0-9][A-Z0-9-]{3,})`),
		regexp.MustCompile(`(?i)tax invoice\s*[:#]?\s*([0-9][A-Z0-9-]{3,})`),
		regexp.MustCompile(`(?i)receipt\s*(?:number|no\.?|#)\s*[:#]?\s*([A-Z0-9][A-Z0-9-]{3,})`),
	}
	reInvDate = regexp.MustCompile(`(?i)(?:invoice date|date of issue|issue date|invoice issued|date issued|billing date|statement date)\s*[:\-]?\s*([^\n]{0,40})`)
	reDueDate = regexp.MustCompile(`(?i)(?:date paid|payment date|paid on)\s*[:\-]?\s*([^\n]{0,40})`)
	rePeriod  = regexp.MustCompile(`(?i)(?:summary for|billing period|service period|period)\s*[:\-]?\s*([A-Za-z]{3,9}\.? \d{1,2},? \d{4}|\d{1,2} [A-Za-z]{3,9} \d{4}|\d{1,2}/\d{1,2}/\d{4})\s*(?:-|–|to)\s*([A-Za-z]{3,9}\.? \d{1,2},? \d{4}|\d{1,2} [A-Za-z]{3,9} \d{4}|\d{1,2}/\d{1,2}/\d{4})`)

	reTotalLabels = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\btotal amount due in [A-Z]{3}`),
		regexp.MustCompile(`(?i)\btotal in [A-Z]{3}`),
		regexp.MustCompile(`(?im)^\s*(?:grand\s+)?total(?:\s+\(?(?:inc|incl|including)\.?\s*(?:gst|vat|tax)\)?)?\s*[:\-]?`),
		regexp.MustCompile(`(?i)amount due\s*[:\-]?`),
		regexp.MustCompile(`(?i)total paid\s*[:\-]?`),
		regexp.MustCompile(`(?i)amount paid\s*[:\-]?`),
		regexp.MustCompile(`(?im)^\s*amount\s*[:\-]`),
		regexp.MustCompile(`(?i)\btotal\s*[:\-]`),
	}
	reTaxPct  = regexp.MustCompile(`(?i)(\d{1,2}(?:\.\d{1,2})?)\s*%\s*(gst|vat|tax)\s*[:\-]?\s*([^\n]{0,40})`)
	reTaxPct2 = regexp.MustCompile(`(?i)(gst|vat|tax)\s*\(?\s*(\d{1,2}(?:\.\d{1,2})?)\s*%\s*\)?\s*[:\-]?\s*([^\n]{0,40})`)
	reTaxLine = regexp.MustCompile(`(?im)^\s*(?:total\s+)?(?:gst|vat|tax)(?:\s+amount)?\s*[:\-]?\s*([^\n%]{0,40})$`)
	reInCode  = regexp.MustCompile(`(?i)\bin ([A-Z]{3})\b`)
	reInclGST = regexp.MustCompile(`(?i)(?:includes|including|incl\.?)\s+(?:gst|vat|tax)\s+(?:of\s+)?([^\n]{0,30})`)
)

// Hints guide extraction.
type Hints struct {
	Subject    string
	Sender     string
	Filename   string
	When       *time.Time // email date (fallback for the document date)
	DefaultCur string     // currency for a bare "$" (vendor default or the org's)
}

// VendorFor guesses the vendor key from the sender / subject / text.
func VendorFor(sender, subject, text string) string {
	s := strings.ToLower(sender + " " + subject)
	t := strings.ToLower(text)
	switch {
	case strings.Contains(s, "synergywholesale") || strings.Contains(s, "synergy wholesale"):
		return "synergy"
	case strings.Contains(s, "google workspace") || (strings.Contains(s, "payments-noreply@google.com") && strings.Contains(t, "workspace")):
		return "google_workspace"
	case strings.Contains(t, "synergy wholesale") || strings.Contains(t, "nexigen digital"):
		return "synergy"
	case strings.Contains(t, "google workspace") || strings.Contains(t, "g suite"):
		return "google_workspace"
	}
	return ""
}

// Extract applies the rules to a document's text.
func Extract(text string, h Hints) Extracted {
	text = CleanText(text)
	e := Extracted{Kind: KindInvoice, Method: "rules"}
	e.VendorKey = VendorFor(h.Sender, h.Subject, text)
	subj := h.Subject
	all := subj + "\n" + text

	// --- vendor-specific notices ---
	switch e.VendorKey {
	case "synergy":
		if m := reSynergyLowBal.FindStringSubmatch(text); m != nil {
			raw := strings.ReplaceAll(strings.ReplaceAll(m[1], " ", ""), "$", "")
			if v, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", ""), 64); err == nil {
				e.Balance = &v
			}
			e.Ignore = "Low balance notice (not an expense)"
			return e
		}
		if regexp.MustCompile(`(?i)transaction statement`).MatchString(all) {
			e.Ignore = "Monthly transaction statement (its CSV attachment is read separately)"
			return e
		}
		if h.DefaultCur == "" {
			h.DefaultCur = "AUD"
		}
		if m := reSynergyReceiptSubj.FindStringSubmatch(all); m != nil {
			e.InvoiceNumber = m[1]
			e.Kind = KindReceipt
		} else if m := reSynergyInvoiceSubj.FindStringSubmatch(subj); m != nil {
			e.InvoiceNumber = m[1]
		}
		e.Service = "Synergy Wholesale"
	case "google_workspace":
		if d := reGoogleDomain.FindStringSubmatch(subj); d != nil {
			e.Service = "Google Workspace (" + d[1] + ")"
		} else {
			e.Service = "Google Workspace"
		}
		switch {
		case reGoogleDeclinedSubj.MatchString(subj):
			e.Kind = KindDeclined
			e.Date = ParseDate(text)
			f := false
			e.Paid = &f
		case reGooglePaidSubj.MatchString(subj) || reGooglePaidBody.MatchString(text):
			e.Kind = KindPayment
			if m := reGooglePaidBody.FindStringSubmatch(text); m != nil {
				if v, c, ok := ParseMoney(m[1]); ok {
					e.Total, e.Currency, e.HasTotal = v, c, true
				}
				e.Date = ParseDate(m[3])
				e.PaidDate = e.Date
			}
			t := true
			e.Paid = &t
		}
		if reGoogleInvoiceSubj.MatchString(subj) {
			t := true
			e.Paid = &t // "The balance will be automatically charged"
		}
	}

	// --- invoice number ---
	if e.InvoiceNumber == "" {
		for _, re := range reInvNo {
			if v := findGroup(re, all); v != "" && strings.ContainsAny(v, "0123456789") {
				e.InvoiceNumber = strings.Trim(v, ".-")
				break
			}
		}
	}

	// --- dates ---
	if e.Date == nil {
		if v := findGroup(reInvDate, text); v != "" {
			e.Date = ParseDate(v)
		}
	}
	if e.PaidDate == nil {
		if v := findGroup(reDueDate, text); v != "" {
			e.PaidDate = ParseDate(v)
		}
	}
	if m := rePeriod.FindStringSubmatch(text); m != nil {
		e.PeriodStart, e.PeriodEnd = ParseDate(m[1]), ParseDate(m[2])
	}
	if e.Date == nil && e.PeriodEnd != nil {
		e.Date = e.PeriodEnd
	}

	// --- total ---
	if !e.HasTotal {
		for _, re := range reTotalLabels {
			if v, c, ok := amountAfter(re, text); ok && v != 0 {
				e.Total, e.Currency, e.HasTotal = v, c, true
				if c == "" {
					if cm := reInCode.FindStringSubmatch(re.FindString(text)); cm != nil && isCode(strings.ToUpper(cm[1])) {
						e.Currency = strings.ToUpper(cm[1])
					}
				}
				break
			}
		}
	}

	// --- tax ---
	if m := reTaxPct.FindStringSubmatch(text); m != nil {
		if v, _, ok := ParseMoney(m[3]); ok {
			e.Tax, e.HasTax = v, true
			e.TaxNote = strings.ToUpper(m[2]) + " " + m[1] + "%"
		}
	} else if m := reTaxPct2.FindStringSubmatch(text); m != nil {
		if v, _, ok := ParseMoney(m[3]); ok {
			e.Tax, e.HasTax = v, true
			e.TaxNote = strings.ToUpper(m[1]) + " " + m[2] + "%"
		}
	} else if m := reInclGST.FindStringSubmatch(text); m != nil {
		if v, _, ok := ParseMoney(m[1]); ok {
			e.Tax, e.HasTax = v, true
			e.TaxNote = "Tax included"
		}
	} else {
		for _, m := range reTaxLine.FindAllStringSubmatch(text, -1) {
			if v, _, ok := ParseMoney(m[1]); ok && strings.ContainsAny(m[1], "0123456789") {
				e.Tax, e.HasTax = v, true
				e.TaxNote = "Tax"
				break
			}
		}
	}
	if e.HasTax && e.HasTotal && (e.Tax < 0 || e.Tax >= e.Total) {
		e.Tax, e.HasTax, e.TaxNote = 0, false, ""
	}

	// --- currency defaults ---
	curExplicit := e.Currency != ""
	if e.Currency == "" {
		if m := regexp.MustCompile(`\b(AUD|USD|EUR|GBP|NZD|KES|CAD|SGD)\b`).FindStringSubmatch(text); m != nil {
			e.Currency, curExplicit = m[1], true
		}
	}
	if e.Currency == "" {
		e.Currency = h.DefaultCur
	}

	// Synergy Wholesale is an Australian GST-registered supplier and its AUD
	// prices include GST; receipts do not show the GST, so estimate it.
	if e.VendorKey == "synergy" && !e.HasTax && e.HasTotal && e.Currency == "AUD" && e.Total > 0 {
		e.Tax = math.Round(e.Total/11*100) / 100
		e.HasTax = true
		e.TaxNote = "GST 1/11 of total (estimated — Synergy prices include GST)"
	}

	if e.Date == nil && h.When != nil {
		d := Day(*h.When)
		e.Date = &d
		e.Warnings = append(e.Warnings, "Date taken from the email")
	}
	if e.Kind == KindReceipt && e.PaidDate == nil {
		e.PaidDate = e.Date
	}
	if e.Paid == nil && (e.Kind == KindReceipt || regexp.MustCompile(`(?i)status:\s*paid|\bpaid\b in full|payment received|amount paid`).MatchString(text)) {
		t := true
		e.Paid = &t
	}

	// --- confidence ---
	c := 0.0
	if e.HasTotal {
		c += 0.45
	}
	if curExplicit {
		c += 0.15
	} else if e.Currency != "" {
		c += 0.05
	}
	if e.InvoiceNumber != "" {
		c += 0.2
	}
	if e.Date != nil {
		c += 0.1
	}
	if e.HasTax || e.VendorKey != "" {
		c += 0.1
	}
	if e.Kind == KindDeclined {
		c = math.Max(c, 0.8)
	}
	e.Confidence = math.Min(1, math.Round(c*100)/100)
	if !e.HasTotal && e.Kind != KindDeclined {
		e.Warnings = append(e.Warnings, "No amount found — enter it or attach the invoice PDF")
	}
	return e
}

// Day returns t's calendar date as a UTC midnight.
func Day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
