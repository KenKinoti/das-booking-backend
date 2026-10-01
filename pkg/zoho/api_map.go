package zoho

import (
	"encoding/json"
	"strconv"
	"strings"
)

// flexNum accepts 12.5, "12.5", "10.00%", "" and null.
type flexNum float64

func (f *flexNum) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, _ := parseNum(s)
	*f = flexNum(v)
	return nil
}

// flexStr accepts strings and numbers (Zoho ids are strings, but be lenient).
type flexStr string

func (f *flexStr) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexStr(v)
		return nil
	}
	*f = flexStr(s)
	return nil
}

// flexBool accepts true/false and "true"/"false".
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.ToLower(strings.TrimSpace(string(b))), `"`)
	*f = flexBool(s == "true" || s == "1" || s == "yes")
	return nil
}

// parseNum parses "1,234.50", "10%", "$12", "(5.00)", "AUD 250.00",
// "KES 1,500.00", "1.234,50 €" and "1e3".
func parseNum(s string) (float64, bool) {
	v, _, ok := parseMoney(s)
	return v, ok
}

// parseMoney parses an amount that may carry a currency code or symbol
// ("KES 1,500.00", "1500 AUD", "$12", "€1.234,50"). cur is the 3-letter code
// when one was written. Letters are never kept as part of the number (the old
// parser kept "E" for exponents, so "KES 1,500.00" or "EUR 10" became 0).
func parseMoney(s string) (v float64, cur string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, "", false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, "", true // plain numbers, including JSON exponents
	}
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg = true
		s = s[1 : len(s)-1]
	}
	// A currency code before or after the number.
	letters := strings.Builder{}
	var num strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r == '.', r == ',':
			num.WriteRune(r)
		case r == '-' || r == '\u2212':
			neg = !neg
		case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
			letters.WriteRune(r)
		}
	}
	if l := strings.ToUpper(letters.String()); len(l) == 3 {
		cur = l
	}
	n := num.String()
	if n == "" {
		return 0, "", false
	}
	lastDot, lastComma := strings.LastIndex(n, "."), strings.LastIndex(n, ",")
	switch {
	case lastDot >= 0 && lastComma >= 0:
		if lastComma > lastDot { // 1.234,50
			n = strings.ReplaceAll(n, ".", "")
			n = strings.Replace(n, ",", ".", 1)
		} else { // 1,234.50
			n = strings.ReplaceAll(n, ",", "")
		}
	case lastComma >= 0:
		// One comma followed by 1–2 digits is a decimal comma ("12,5"); otherwise thousands ("1,500").
		if strings.Count(n, ",") == 1 && len(n)-lastComma-1 != 3 {
			n = strings.Replace(n, ",", ".", 1)
		} else {
			n = strings.ReplaceAll(n, ",", "")
		}
	case strings.Count(n, ".") > 1: // 1.234.567
		n = strings.ReplaceAll(n, ".", "")
	}
	f, err := strconv.ParseFloat(n, 64)
	if err != nil {
		return 0, cur, false
	}
	if neg {
		f = -f
	}
	return f, cur, true
}

type zAddress struct {
	Attention flexStr `json:"attention"`
	Address   flexStr `json:"address"`
	Street2   flexStr `json:"street2"`
	City      flexStr `json:"city"`
	State     flexStr `json:"state"`
	Zip       flexStr `json:"zip"`
	Country   flexStr `json:"country"`
	Phone     flexStr `json:"phone"`
}

func (a zAddress) norm() Address {
	return Address{Attention: string(a.Attention), Street: string(a.Address), Street2: string(a.Street2), City: string(a.City),
		State: string(a.State), Zip: string(a.Zip), Country: string(a.Country), Phone: string(a.Phone)}
}

type zPerson struct {
	ContactPersonID  flexStr  `json:"contact_person_id"`
	FirstName        flexStr  `json:"first_name"`
	LastName         flexStr  `json:"last_name"`
	Email            flexStr  `json:"email"`
	Phone            flexStr  `json:"phone"`
	Mobile           flexStr  `json:"mobile"`
	Designation      flexStr  `json:"designation"`
	Department       flexStr  `json:"department"`
	IsPrimaryContact flexBool `json:"is_primary_contact"`
}

type zContact struct {
	ContactID        flexStr   `json:"contact_id"`
	ContactName      flexStr   `json:"contact_name"`
	CompanyName      flexStr   `json:"company_name"`
	ContactType      flexStr   `json:"contact_type"`
	CustomerSubType  flexStr   `json:"customer_sub_type"`
	Status           flexStr   `json:"status"`
	FirstName        flexStr   `json:"first_name"`
	LastName         flexStr   `json:"last_name"`
	Email            flexStr   `json:"email"`
	Phone            flexStr   `json:"phone"`
	Mobile           flexStr   `json:"mobile"`
	Website          flexStr   `json:"website"`
	CurrencyCode     flexStr   `json:"currency_code"`
	PaymentTerms     *flexNum  `json:"payment_terms"`
	Notes            flexStr   `json:"notes"`
	TaxRegNo         flexStr   `json:"tax_reg_no"`
	TaxNumber        flexStr   `json:"tax_number"`
	VatRegNo         flexStr   `json:"vat_reg_no"`
	GstNo            flexStr   `json:"gst_no"`
	BillingAddress   zAddress  `json:"billing_address"`
	ShippingAddress  zAddress  `json:"shipping_address"`
	ContactPersons   []zPerson `json:"contact_persons"`
	LastModifiedTime flexStr   `json:"last_modified_time"`
}

func (z zContact) norm() Contact {
	c := Contact{ExtID: string(z.ContactID), Name: strings.TrimSpace(string(z.ContactName)), Company: strings.TrimSpace(string(z.CompanyName)),
		FirstName: string(z.FirstName), LastName: string(z.LastName), Individual: strings.EqualFold(string(z.CustomerSubType), "individual"),
		Email: strings.ToLower(strings.TrimSpace(string(z.Email))), Phone: string(z.Phone), Mobile: string(z.Mobile), Website: string(z.Website),
		Currency: strings.ToUpper(string(z.CurrencyCode)), Notes: string(z.Notes), Active: !strings.EqualFold(string(z.Status), "inactive"),
		Billing: z.BillingAddress.norm(), Shipping: z.ShippingAddress.norm(), Modified: string(z.LastModifiedTime)}
	for _, t := range []flexStr{z.TaxRegNo, z.TaxNumber, z.VatRegNo, z.GstNo} {
		if c.TaxNumber == "" && strings.TrimSpace(string(t)) != "" {
			c.TaxNumber = strings.TrimSpace(string(t))
		}
	}
	if z.PaymentTerms != nil {
		d := int(*z.PaymentTerms)
		c.TermsDays = &d
	}
	for _, p := range z.ContactPersons {
		c.Persons = append(c.Persons, p.norm())
	}
	return c
}

func (p zPerson) norm() Person {
	role := strings.TrimSpace(string(p.Designation))
	if d := strings.TrimSpace(string(p.Department)); d != "" {
		if role != "" {
			role += ", " + d
		} else {
			role = d
		}
	}
	return Person{ExtID: string(p.ContactPersonID), FirstName: string(p.FirstName), LastName: string(p.LastName),
		Email: strings.ToLower(strings.TrimSpace(string(p.Email))), Phone: string(p.Phone), Mobile: string(p.Mobile), Role: role, Primary: bool(p.IsPrimaryContact)}
}

type zItem struct {
	ItemID        flexStr   `json:"item_id"`
	Name          flexStr   `json:"name"`
	Description   flexStr   `json:"description"`
	Rate          flexStr   `json:"rate"`       // selling price (Zoho base currency)
	SalesRate     flexStr   `json:"sales_rate"` // some Zoho apps / older payloads
	PricebookRate flexNum   `json:"pricebook_rate"`
	PurchaseRate  flexNum   `json:"purchase_rate"`
	Unit          flexStr   `json:"unit"`
	SKU           flexStr   `json:"sku"`
	TaxName       flexStr   `json:"tax_name"`
	TaxPercentage flexNum   `json:"tax_percentage"`
	ProductType   flexStr   `json:"product_type"`
	Status        flexStr   `json:"status"`
	CurrencyCode  flexStr   `json:"currency_code"`
	InclusiveA    *flexBool `json:"is_inclusive_tax"`
	InclusiveB    *flexBool `json:"is_tax_inclusive"`
	InclusiveC    *flexBool `json:"tax_inclusive"`
}

// itemRate picks the selling price: "rate" (Zoho's item selling price),
// then "sales_rate". Never pricebook_rate (a customer price list) or
// purchase_rate (the cost). Rates may arrive as "AUD 1,500.00".
func itemRate(rate, sales string) (float64, string) {
	if v, cur, ok := parseMoney(rate); ok && v != 0 {
		return v, cur
	}
	if v, cur, ok := parseMoney(sales); ok {
		return v, cur
	}
	return 0, ""
}

func (z zItem) norm() Item {
	t := strings.ToLower(string(z.ProductType))
	if t != "goods" {
		t = "service"
	}
	rate, cur := itemRate(string(z.Rate), string(z.SalesRate))
	if c := strings.ToUpper(strings.TrimSpace(string(z.CurrencyCode))); c != "" {
		cur = c
	}
	var incl *bool
	for _, b := range []*flexBool{z.InclusiveA, z.InclusiveB, z.InclusiveC} {
		if b != nil {
			v := bool(*b)
			incl = &v
			break
		}
	}
	return Item{ExtID: string(z.ItemID), Name: string(z.Name), Description: string(z.Description), Rate: rate, Unit: string(z.Unit),
		SKU: string(z.SKU), TaxName: string(z.TaxName), TaxPercent: float64(z.TaxPercentage), Type: t, Active: !strings.EqualFold(string(z.Status), "inactive"),
		Currency: cur, TaxInclusive: incl, PurchaseRate: float64(z.PurchaseRate)}
}

type zLine struct {
	LineItemID    flexStr `json:"line_item_id"`
	ItemID        flexStr `json:"item_id"`
	Name          flexStr `json:"name"`
	Description   flexStr `json:"description"`
	Rate          flexNum `json:"rate"`
	Quantity      flexNum `json:"quantity"`
	Unit          flexStr `json:"unit"`
	Discount      flexStr `json:"discount"`
	DiscountAmt   flexNum `json:"discount_amount"`
	TaxName       flexStr `json:"tax_name"`
	TaxPercentage flexNum `json:"tax_percentage"`
	ItemTotal     flexNum `json:"item_total"`
	ItemOrder     flexNum `json:"item_order"`
}

func (z zLine) norm() Line {
	l := Line{ExtID: string(z.LineItemID), ItemID: string(z.ItemID), Name: string(z.Name), Description: string(z.Description),
		Quantity: float64(z.Quantity), Unit: string(z.Unit), Rate: float64(z.Rate), TaxName: string(z.TaxName), TaxPercent: float64(z.TaxPercentage),
		Total: float64(z.ItemTotal)}
	d := strings.TrimSpace(string(z.Discount))
	if strings.HasSuffix(d, "%") {
		l.DiscountPct, _ = parseNum(d)
	} else if v, ok := parseNum(d); ok && v != 0 {
		l.DiscountAmt = v
	}
	if l.DiscountAmt == 0 && float64(z.DiscountAmt) != 0 && l.DiscountPct == 0 {
		l.DiscountAmt = float64(z.DiscountAmt)
	}
	return l
}

type zPayment struct {
	PaymentID        flexStr `json:"payment_id"`
	PaymentNumber    flexStr `json:"payment_number"`
	InvoicePaymentID flexStr `json:"invoice_payment_id"`
	PaymentMode      flexStr `json:"payment_mode"`
	Description      flexStr `json:"description"`
	Date             flexStr `json:"date"`
	ReferenceNumber  flexStr `json:"reference_number"`
	Amount           flexNum `json:"amount"`
}

type zCredit struct {
	CreditnoteID     flexStr `json:"creditnote_id"`
	CreditnoteNumber flexStr `json:"creditnote_number"`
	CreditedDate     flexStr `json:"credited_date"`
	CreditedAmount   flexNum `json:"credited_amount"`
}

type zTax struct {
	TaxName   flexStr `json:"tax_name"`
	TaxAmount flexNum `json:"tax_amount"`
}

// zDoc covers invoices, estimates, credit notes and recurring invoices.
type zDoc struct {
	InvoiceID           flexStr    `json:"invoice_id"`
	InvoiceNumber       flexStr    `json:"invoice_number"`
	EstimateID          flexStr    `json:"estimate_id"`
	EstimateNumber      flexStr    `json:"estimate_number"`
	CreditnoteID        flexStr    `json:"creditnote_id"`
	CreditnoteNumber    flexStr    `json:"creditnote_number"`
	RecurrenceID        flexStr    `json:"recurring_invoice_id"`
	RecurrenceName      flexStr    `json:"recurrence_name"`
	RecurrenceFreq      flexStr    `json:"recurrence_frequency"`
	RepeatEvery         flexNum    `json:"repeat_every"`
	NextInvoiceDate     flexStr    `json:"next_invoice_date"`
	EndDate             flexStr    `json:"end_date"`
	StartDate           flexStr    `json:"start_date"`
	CustomerID          flexStr    `json:"customer_id"`
	CustomerName        flexStr    `json:"customer_name"`
	Email               flexStr    `json:"email"`
	Status              flexStr    `json:"status"`
	Date                flexStr    `json:"date"`
	DueDate             flexStr    `json:"due_date"`
	ExpiryDate          flexStr    `json:"expiry_date"`
	CurrencyCode        flexStr    `json:"currency_code"`
	ExchangeRate        flexNum    `json:"exchange_rate"`
	ReferenceNumber     flexStr    `json:"reference_number"`
	LineItems           []zLine    `json:"line_items"`
	SubTotal            flexNum    `json:"sub_total"`
	DiscountType        flexStr    `json:"discount_type"`
	Discount            flexStr    `json:"discount"`
	DiscountPercent     flexNum    `json:"discount_percent"`
	DiscountAmount      flexNum    `json:"discount_amount"`
	IsDiscountBeforeTax *flexBool  `json:"is_discount_before_tax"`
	ShippingCharge      flexNum    `json:"shipping_charge"`
	Adjustment          flexNum    `json:"adjustment"`
	AdjustmentDesc      flexStr    `json:"adjustment_description"`
	TaxTotal            flexNum    `json:"tax_total"`
	Taxes               []zTax     `json:"taxes"`
	Total               flexNum    `json:"total"`
	Balance             flexNum    `json:"balance"`
	PaymentMade         flexNum    `json:"payment_made"`
	CreditsApplied      *flexNum   `json:"credits_applied"`
	WriteOffAmount      flexNum    `json:"write_off_amount"`
	IsInclusiveTax      flexBool   `json:"is_inclusive_tax"`
	Notes               flexStr    `json:"notes"`
	Terms               flexStr    `json:"terms"`
	InvoiceURL          flexStr    `json:"invoice_url"`
	EstimateURL         flexStr    `json:"estimate_url"`
	BillingAddress      zAddress   `json:"billing_address"`
	Payments            []zPayment `json:"payments"`
	Credits             []zCredit  `json:"credits"`
	LastPaymentDate     flexStr    `json:"last_payment_date"`
	CreatedTime         flexStr    `json:"created_time"`
	LastModifiedTime    flexStr    `json:"last_modified_time"`
}

func (z zDoc) norm(kind string) Doc {
	d := Doc{Kind: kind, ContactID: string(z.CustomerID), CustomerName: string(z.CustomerName), Email: strings.ToLower(string(z.Email)),
		Status: strings.ToLower(string(z.Status)), Date: string(z.Date), DueDate: string(z.DueDate), Currency: strings.ToUpper(string(z.CurrencyCode)),
		ExchangeRate: float64(z.ExchangeRate), Reference: string(z.ReferenceNumber), SubTotal: float64(z.SubTotal), Shipping: float64(z.ShippingCharge),
		Adjustment: float64(z.Adjustment), AdjustmentDesc: string(z.AdjustmentDesc), TaxTotal: float64(z.TaxTotal), Total: float64(z.Total),
		Balance: float64(z.Balance), PaymentMade: float64(z.PaymentMade), WriteOff: float64(z.WriteOffAmount), InclusiveTax: bool(z.IsInclusiveTax),
		Notes: string(z.Notes), Terms: string(z.Terms), URL: string(z.InvoiceURL), EstimateID: string(z.EstimateID),
		LastPaymentDate: string(z.LastPaymentDate), Created: string(z.CreatedTime), Modified: string(z.LastModifiedTime)}
	switch kind {
	case KindInvoice:
		d.ExtID, d.Number = string(z.InvoiceID), string(z.InvoiceNumber)
	case KindQuote:
		d.ExtID, d.Number, d.EstimateID = string(z.EstimateID), string(z.EstimateNumber), ""
		d.DueDate = string(z.ExpiryDate)
		d.URL = string(z.EstimateURL)
	case KindCredit:
		d.ExtID, d.Number = string(z.CreditnoteID), string(z.CreditnoteNumber)
	case KindRecurring:
		d.ExtID, d.Number = string(z.RecurrenceID), ""
		d.RecurName = string(z.RecurrenceName)
		d.RecurInterval = recurInterval(string(z.RecurrenceFreq), int(z.RepeatEvery))
		d.RecurNext, d.RecurEnd = string(z.NextInvoiceDate), string(z.EndDate)
		if d.Date == "" {
			d.Date = string(z.StartDate)
		}
	}
	if z.CreditsApplied != nil {
		v := float64(*z.CreditsApplied)
		d.Credits = &v
	} else if len(z.Credits) > 0 {
		v := 0.0
		for _, c := range z.Credits {
			v += float64(c.CreditedAmount)
		}
		d.Credits = &v
	}
	// Entity-level discount.
	if strings.EqualFold(string(z.DiscountType), "entity_level") || z.DiscountType == "" {
		ds := strings.TrimSpace(string(z.Discount))
		switch {
		case float64(z.DiscountPercent) > 0:
			d.DiscountPct = float64(z.DiscountPercent)
		case strings.HasSuffix(ds, "%"):
			d.DiscountPct, _ = parseNum(ds)
		case float64(z.DiscountAmount) > 0 && strings.EqualFold(string(z.DiscountType), "entity_level"):
			d.DiscountAmt = float64(z.DiscountAmount)
		default:
			if v, ok := parseNum(ds); ok && v > 0 && strings.EqualFold(string(z.DiscountType), "entity_level") {
				d.DiscountAmt = v
			}
		}
	}
	if z.IsDiscountBeforeTax != nil && !bool(*z.IsDiscountBeforeTax) && (d.DiscountPct > 0 || d.DiscountAmt > 0) {
		d.DiscountAfterTax = true
	}
	d.BillingAddress = formatAddress(z.BillingAddress.norm())
	for _, l := range z.LineItems {
		d.Lines = append(d.Lines, l.norm())
	}
	for _, p := range z.Payments {
		id := string(p.InvoicePaymentID)
		if id == "" {
			id = string(p.PaymentID) + ":" + d.ExtID
		}
		ref := strings.TrimSpace(string(p.PaymentNumber))
		if r := strings.TrimSpace(string(p.ReferenceNumber)); r != "" {
			if ref != "" {
				ref += " · " + r
			} else {
				ref = r
			}
		}
		d.Payments = append(d.Payments, Pay{ExtID: id, Number: string(p.PaymentNumber), Date: string(p.Date), Amount: float64(p.Amount),
			Mode: string(p.PaymentMode), Reference: ref, Notes: string(p.Description)})
	}
	return d
}

// Document kinds.
const (
	KindInvoice   = "invoice"
	KindQuote     = "quote"
	KindCredit    = "credit_note"
	KindRecurring = "recurring"
)

func recurInterval(freq string, every int) string {
	f := strings.ToLower(freq)
	if every <= 0 {
		every = 1
	}
	switch {
	case strings.HasPrefix(f, "week") && every == 2:
		return "fortnightly"
	case strings.HasPrefix(f, "week"):
		return "weekly"
	case strings.HasPrefix(f, "month") && every == 3:
		return "quarterly"
	case strings.HasPrefix(f, "year"):
		return "yearly"
	default:
		return "monthly"
	}
}

func formatAddress(a Address) string {
	var lines []string
	for _, s := range []string{a.Attention, a.Street, a.Street2} {
		if strings.TrimSpace(s) != "" {
			lines = append(lines, strings.TrimSpace(s))
		}
	}
	city := strings.TrimSpace(strings.Join(nonEmpty(a.City, a.State, a.Zip), " "))
	if city != "" {
		lines = append(lines, city)
	}
	if strings.TrimSpace(a.Country) != "" {
		lines = append(lines, strings.TrimSpace(a.Country))
	}
	return strings.Join(lines, "\n")
}

func nonEmpty(v ...string) []string {
	var out []string
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

type zComment struct {
	CommentID       flexStr `json:"comment_id"`
	Description     flexStr `json:"description"`
	CommentedBy     flexStr `json:"commented_by"`
	CommentType     flexStr `json:"comment_type"`
	Date            flexStr `json:"date"`
	Time            flexStr `json:"time"`
	OperationType   flexStr `json:"operation_type"`
	TransactionType flexStr `json:"transaction_type"`
}

func (z zComment) norm() Comment {
	text := strings.TrimSpace(string(z.Description))
	kind := "history"
	lt := strings.ToLower(text)
	if strings.Contains(lt, "emailed") || strings.Contains(lt, "sent to") || strings.Contains(lt, "email sent") || strings.Contains(lt, "mailed") {
		kind = "email"
	}
	return Comment{Date: string(z.Date), Text: text, By: string(z.CommentedBy), Kind: kind}
}
