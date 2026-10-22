package aitools

import (
	"fmt"
	"net/url"
	"strings"
)

func init() {
	itemSchema := tObj(map[string]any{
		"description":      tStr("What is being charged for"),
		"quantity":         tNum("Quantity (default 1)"),
		"unit_price":       tNum("Price per unit in the document currency"),
		"unit":             tStr("Unit label, e.g. hours, each"),
		"tax_rate":         tNum("Tax percent 0-100. Leave out to use the organisation's default tax rate; 0 for tax-free."),
		"discount_percent": tNum("Line discount percent 0-100"),
		"product_id":       tStr("Optional inventory product id this line is for"),
		"service_id":       tStr("Optional service id this line is for"),
	}, "description", "unit_price")
	docProps := func(quote bool) map[string]any {
		due := "Due date"
		if quote {
			due = "Valid-until (expiry) date"
		}
		return map[string]any{
			"customer_id":  tStr("Existing customer id (preferred). Name, email, address, currency and CC contacts are filled from the customer."),
			"customer":     tStr("Customer name or email to look up when you don't have the id"),
			"client_name":  tStr("Client name for a one-off client with no customer record"),
			"client_email": tStr("Client email (one-off client, or to override the customer's)"),
			"items":        map[string]any{"type": "array", "description": "Line items (at least one)", "items": itemSchema, "minItems": 1},
			"currency":     tStr("ISO 4217 currency (default: the customer's billing currency, else the organisation's)"),
			"issue_date":   tDate("Issue date (default today)"),
			"due_date":     tDate(due + " (default from invoicing settings)"),
			"reference":    tStr("Reference / PO number"),
			"title":        tStr("Short title shown on the document"),
			"notes":        tStr("Notes shown to the client"),
			"terms":        tStr("Terms shown to the client"),
		}
	}
	depositProps := map[string]any{
		"amount_paid":       tNum("Money ALREADY received for this invoice (a deposit / part payment), in the invoice currency. Recorded as a real payment: the invoice becomes partially paid (or paid)."),
		"payment_date":      tDate("Date the amount_paid was received (default today)"),
		"payment_method":    tEnum("How amount_paid was received (default cash)", "cash", "card", "bank_transfer", "mobile_money", "cheque", "paypal", "stripe", "other"),
		"payment_reference": tStr("Receipt / transaction reference for amount_paid, e.g. an M-Pesa code"),
	}
	listProps := map[string]any{
		"doc_type": tEnum("invoice (default), quote, or all", "invoice", "quote", "all"),
		"status": tEnum("Filter by displayed status. unpaid = sent+partial; overdue = unpaid past due; open = quotes awaiting a decision; due = unpaid not yet due",
			"all", "draft", "sent", "partial", "paid", "overdue", "unpaid", "due", "void", "open", "accepted", "declined", "expired", "converted", "recurring"),
		"customer_id": tStr("Only this customer's documents"),
		"query":       tStr("Search number, client name/email, reference or title"),
		"from":        tDate("Issued on or after"),
		"to":          tDate("Issued on or before"),
		"sort":        tEnum("Sort order (default newest first)", "newest", "oldest", "due_date", "total", "client", "number"),
		"limit":       tLimit(),
	}
	Register(Tool{
		Name:  "invoices_list",
		Title: "List invoices & quotes",
		Description: "Lists invoices and/or quotes with filters (status, customer, date range, text). Returns number, client, " +
			"status, total, balance due, currency, dates and a link for each, plus the total count. Use status=overdue for " +
			"'who owes us money', doc_type=quote for quotes.",
		InputSchema: tObj(listProps),
		ReadOnly:    true,
		Module:      "invoicing",
		Handler:     invoicesList,
	})
	Register(Tool{
		Name:  "invoices_get",
		Title: "Get invoice or quote",
		Description: "Full invoice or quote: client, line items, totals, tax, payments, activity history, the public share link, " +
			"app link and email_status: the last email about it and, per recipient, whether their copy was delivered, opened " +
			"(opened_automatic = likely a mail app or scanner, not a person), viewed through their own link or paid via it. " +
			"Pass invoice_id, or number (e.g. INV-1001 / QUO-1001).",
		InputSchema: tObj(map[string]any{
			"invoice_id": tStr("Invoice or quote id"),
			"number":     tStr("Document number, e.g. INV-1001"),
		}),
		ReadOnly: true,
		Module:   "invoicing",
		Handler:  invoicesGet,
	})
	Register(Tool{
		Name:  "invoices_create",
		Title: "Create invoice",
		Description: "Creates a DRAFT invoice for a customer (customer_id or customer name/email) or a one-off client " +
			"(client_name). Line items need description and unit_price; tax defaults to the organisation's rate. " +
			"If the client already paid part (a deposit), pass amount_paid (+ payment_date/method/reference): it is recorded " +
			"as a payment and the invoice shows the balance due. " +
			"The invoice is not sent — use invoices_send afterwards if the user wants it emailed. Returns number, totals and link.",
		InputSchema: tObj(withProps(docProps(false), depositProps), "items"),
		Module:      "invoicing",
		Handler:     func(c *Ctx, a map[string]any) (any, error) { return docCreate(c, a, "invoice") },
	})
	Register(Tool{
		Name:  "quotes_create",
		Title: "Create quote",
		Description: "Creates a DRAFT quote (estimate) for a customer or one-off client, same fields as invoices_create; " +
			"due_date is the expiry date. Convert it later with quotes_convert. Returns number, totals and link.",
		InputSchema: tObj(docProps(true), "items"),
		Module:      "invoicing",
		Handler:     func(c *Ctx, a map[string]any) (any, error) { return docCreate(c, a, "quote") },
	})
	up := docProps(false)
	delete(up, "customer")
	up["invoice_id"] = tStr("Invoice or quote id to update")
	up["items"] = map[string]any{"type": "array", "description": "Replacement line items (replaces ALL existing lines). Leave out to keep the current lines.", "items": itemSchema}
	up["client_name"] = tStr("Client name")
	Register(Tool{
		Name:  "invoices_update",
		Title: "Update invoice or quote",
		Description: "Edits an invoice or quote that is not void/converted (normally a draft): dates, client details, " +
			"reference, notes, terms, currency or line items. Only fields you pass change; passing items replaces all lines. " +
			"Returns the updated totals.",
		InputSchema: tObj(up, "invoice_id"),
		Module:      "invoicing",
		Handler:     invoicesUpdate,
	})
	Register(Tool{
		Name:  "invoices_add_payment",
		Title: "Record invoice payment",
		Description: "Records a payment received against an invoice (full or partial — any amount up to the balance due; " +
			"overpayments are refused). The invoice becomes partial or paid automatically and the payment gets a receipt " +
			"number (e.g. RCT-0001). email_receipt=true emails the PDF receipt to the client (billing contact + contacts " +
			"copied on receipts). Returns the new status, balance, receipt number and whether the receipt was emailed.",
		InputSchema: tObj(map[string]any{
			"invoice_id":    tStr("Invoice id"),
			"amount":        tNum("Amount received in the invoice currency. Leave out to pay the full balance."),
			"date":          tDate("Payment date (default today; can't be in the future)"),
			"method":        tEnum("Payment method (default bank_transfer)", "bank_transfer", "cash", "card", "cheque", "mobile_money", "flutterwave", "paypal", "stripe", "other"),
			"reference":     tStr("Payment reference / transaction id, e.g. an M-Pesa code"),
			"notes":         tStr("Notes (shown on the receipt)"),
			"email_receipt": tBool("Email the receipt to the client (needs outgoing email to be set up)"),
		}, "invoice_id"),
		Module:  "invoicing",
		Handler: invoicesAddPayment,
	})
	Register(Tool{
		Name:  "invoices_update_payment",
		Title: "Edit invoice payment",
		Description: "Corrects a payment already recorded on an invoice (incl. a deposit): amount, date, method, reference, " +
			"notes, with an optional reason that is logged (\"Payment edited: KSh 5,000 → KSh 7,500 (reason)\"). The total of " +
			"all payments can't exceed the invoice total; paid / partial / sent status and the balance are recalculated. The " +
			"receipt keeps its number and is marked revised. Online (Flutterwave) and imported (Zoho) payments only allow " +
			"reference and notes. Pick the payment by payment_id or receipt_number (or leave both out when there is only one). " +
			"email_receipt=true emails the revised receipt to the client.",
		InputSchema: tObj(map[string]any{
			"invoice_id":     tStr("Invoice id"),
			"number":         tStr("Invoice number, e.g. INV-1001 (instead of invoice_id)"),
			"payment_id":     tStr("Payment id"),
			"receipt_number": tStr("Receipt number, e.g. RCT-0003 (instead of payment_id)"),
			"amount":         tNum("New amount in the invoice currency"),
			"date":           tDate("New payment date (can't be in the future)"),
			"method":         tEnum("New payment method", "bank_transfer", "cash", "card", "cheque", "mobile_money", "flutterwave", "paypal", "stripe", "other"),
			"reference":      tStr("New reference"),
			"notes":          tStr("New notes"),
			"reason":         tStr("Why it was changed (logged on the invoice)"),
			"email_receipt":  tBool("Email the revised receipt to the client"),
		}),
		Module:  "invoicing",
		Handler: invoicesUpdatePayment,
	})
	Register(Tool{
		Name:  "invoices_send_receipt",
		Title: "Email payment receipt",
		Description: "Emails the receipt for one payment to the client: professional email with the PDF receipt attached " +
			"(e.g. RCT-0001.pdf), amount received, receipt and invoice numbers, balance remaining or paid in full, and a View " +
			"invoice button. Recipients default to the billing contact, contacts copied on receipts and the email settings' " +
			"Always CC/BCC. The pre-send checks run first (wrong amounts in the message are refused). Logged in the invoice's " +
			"Emails tab. Pick the payment by payment_id or receipt_number (or leave both out when there is only one).",
		InputSchema: tObj(map[string]any{
			"invoice_id":     tStr("Invoice id"),
			"number":         tStr("Invoice number, e.g. INV-1001 (instead of invoice_id)"),
			"payment_id":     tStr("Payment id"),
			"receipt_number": tStr("Receipt number, e.g. RCT-0003"),
			"to":             tStr("Recipient email (default: the billing contact / client email)"),
			"cc":             tArr("CC emails. Leave out to use the defaults; [] for none.", tStr("email")),
			"subject":        tStr("Subject (default from Invoice settings → receipt template)"),
			"message":        tStr("Message (default from the receipt template)"),
		}),
		Module:  "invoicing",
		Handler: invoicesSendReceipt,
	})
	Register(Tool{
		Name:  "invoices_send",
		Title: "Email invoice or quote",
		Description: "Emails an invoice or quote to the client (and the customer's CC contacts) and marks it sent. The email " +
			"always has the PDF copy attached (e.g. INV-1005.pdf) and a View button (quotes: View & accept); invoices with a " +
			"balance also get a Pay now link when online payments (Flutterwave) are on. Subject and message default to the " +
			"invoice settings. kind=reminder sends a payment reminder for an unpaid invoice. Requires the server's " +
			"email (SMTP) to be configured — otherwise returns a clear message with the share link so the user can send it " +
			"themselves; mark_as_sent_only=true just marks it sent without emailing.",
		InputSchema: tObj(map[string]any{
			"invoice_id":        tStr("Invoice or quote id"),
			"to":                tStr("Recipient email (default: the primary billing contact or client email)"),
			"cc":                tArr("CC emails. Leave out to use the customer's contacts flagged for this kind of email; [] for none.", tStr("email")),
			"subject":           tStr("Email subject (default from settings)"),
			"message":           tStr("Email body (default from settings)"),
			"kind":              tEnum("invoice/quote (default by document type) or reminder", "invoice", "quote", "reminder"),
			"mark_as_sent_only": tBool("Don't email; only mark as sent"),
		}, "invoice_id"),
		Module:  "invoicing",
		Handler: invoicesSend,
	})
	Register(Tool{
		Name:  "invoices_payment_link",
		Title: "Get invoice payment link",
		Description: "Returns the link where the client can pay an invoice online (card, M-Pesa, mobile money, bank — the " +
			"cheapest method is suggested) and the view link. When online payments are off, the invoice is a draft or has no " +
			"balance, says why and returns only the view link. Pass invoice_id or number.",
		InputSchema: tObj(map[string]any{
			"invoice_id": tStr("Invoice id"),
			"number":     tStr("Invoice number, e.g. INV-1001"),
		}),
		ReadOnly: true,
		Module:   "invoicing",
		Handler:  invoicesPaymentLink,
	})
	Register(Tool{
		Name:  "quotes_convert",
		Title: "Convert quote to invoice",
		Description: "Turns a quote into a new draft invoice (copying client and lines) and marks the quote converted. " +
			"Pass amount_paid (+ payment_date/method/reference) when the client already paid a deposit. Returns the new invoice.",
		InputSchema: tObj(withProps(map[string]any{"quote_id": tStr("Quote id")}, depositProps), "quote_id"),
		Module:      "invoicing",
		Handler:     quotesConvert,
	})
	Register(Tool{
		Name:  "invoices_void",
		Title: "Void invoice or quote",
		Description: "Voids an invoice or quote so it no longer counts as owed (cannot be undone; stops recurrence). " +
			"Fails if payments are recorded. Always confirm with the user first.",
		InputSchema: tObj(map[string]any{
			"invoice_id": tStr("Invoice or quote id"),
			"reason":     tStr("Why it is being voided (for your summary)"),
		}, "invoice_id"),
		Destructive: true,
		Module:      "invoicing",
		Handler:     invoicesVoid,
	})
	idempotentTools["invoices_update"] = true
	idempotentTools["invoices_void"] = true
	idempotentTools["invoices_send"] = false
}

func docPath(inv map[string]any) string {
	if tS(inv, "doc_type") == "quote" {
		return "/quotes/" + tS(inv, "id")
	}
	return "/invoices/" + tS(inv, "id")
}

func invoiceBrief(inv map[string]any) map[string]any {
	m := tPick(inv, "id", "doc_type", "number", "client_name", "client_email", "customer_id", "currency", "total", "amount_paid", "balance_due", "days_overdue", "reference", "title")
	st := tS(inv, "display_status")
	if st == "" {
		st = tS(inv, "status")
	}
	m["status"] = st
	if tF(inv, "days_overdue") == 0 {
		delete(m, "days_overdue")
	}
	if d := tDay(tS(inv, "issue_date")); d != "" {
		m["issue_date"] = d
	}
	if d := tDay(tS(inv, "due_date")); d != "" {
		if tS(inv, "doc_type") == "quote" {
			m["valid_until"] = d
		} else {
			m["due_date"] = d
		}
	}
	if tS(inv, "id") != "" {
		m["url"] = tURL(docPath(inv))
	}
	return m
}

func invoiceFull(c *Ctx, inv map[string]any) map[string]any {
	out := invoiceBrief(inv)
	for k, v := range tPick(inv, "client_phone", "client_address", "client_tax_number", "subtotal", "discount_total", "tax_total", "shipping_amount", "prices_include_tax", "notes", "terms", "cc_emails", "is_recurring", "recurrence_interval", "sent_at", "viewed_at", "paid_at", "converted_from_id", "converted_to_id") {
		out[k] = v
	}
	items := []map[string]any{}
	for _, it := range tList(inv["items"]) {
		items = append(items, tPick(it, "description", "quantity", "unit", "unit_price", "discount_percent", "tax_rate", "tax_name", "amount", "item_type", "reference_id"))
	}
	out["items"] = items
	pays := []map[string]any{}
	for _, p := range tList(inv["payments"]) {
		m := tPick(p, "id", "amount", "method", "reference", "is_deposit")
		m["date"] = tDay(tS(p, "date"))
		pays = append(pays, m)
	}
	if len(pays) > 0 {
		out["payments"] = pays
	}
	acts := []map[string]any{}
	al := tList(inv["activities"])
	if len(al) > 10 {
		al = al[len(al)-10:]
	}
	for _, x := range al {
		acts = append(acts, tPick(x, "type", "message", "created_at:at"))
	}
	if len(acts) > 0 {
		out["activity"] = acts
	}
	if tok := tS(inv, "public_token"); tok != "" && AppURL() != "" {
		out["public_link"] = tURL("/i/" + tok)
	}
	if es, ok := inv["email_status"].(map[string]any); ok && tS(es, "state") != "" && tS(es, "state") != "none" {
		out["email_status"] = emailStatusBrief(es)
	}
	return out
}

// emailStatusBrief is the last email about the document and, per person,
// what happened to their copy (pkg/invoicing emailstatus.go).
func emailStatusBrief(es map[string]any) map[string]any {
	m := tPick(es, "state", "last_kind", "last_sent_at", "last_to", "last_error", "sent", "failed", "tracking", "summary", "people", "opened", "auto_opened", "viewed", "paid_via", "open_tracking", "viewed_at")
	rs := []map[string]any{}
	for _, r := range tList(es["recipients"]) {
		rs = append(rs, tPick(r, "email", "role", "state", "timeline", "error", "opens", "auto_opens", "opened_automatic", "opened_at", "last_open_at", "open_via", "views", "viewed_at", "pay_page_at", "accepted_at", "paid_at", "paid_amount"))
	}
	if len(rs) > 0 {
		m["recipients"] = rs
	}
	if tS(es, "tracking") == "per_recipient" {
		m["note"] = "Each person got their own copy. \"opened\" needs the email's images to load (some apps such as Outlook hide opens, and Apple Mail may open automatically — flagged as opened_automatic); \"viewed\" (they used their own View/Pay link) is reliable."
	}
	return m
}

func invoicesList(c *Ctx, a map[string]any) (any, error) {
	limit := argLimit(a)
	q := url.Values{"per_page": {itoa(limit)}}
	dt := argStr(a, "doc_type")
	if dt == "" {
		dt = "invoice"
	}
	q.Set("type", dt)
	if s := argStr(a, "status"); s != "" {
		q.Set("status", s)
	}
	if s := argStr(a, "customer_id"); s != "" {
		q.Set("customer_id", s)
	}
	if s := argStr(a, "query"); s != "" {
		q.Set("q", s)
	}
	for _, k := range []string{"from", "to"} {
		if s := argStr(a, k); s != "" {
			q.Set(k, s)
		}
	}
	if s := argStr(a, "sort"); s != "" && s != "newest" {
		q.Set("sort", s)
	}
	v, err := c.get("/invoicing/invoices", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "invoices")
	out := make([]map[string]any, 0, len(list))
	sums := map[string]float64{}
	for _, inv := range list {
		out = append(out, invoiceBrief(inv))
		switch tS(inv, "display_status") {
		case "sent", "partial", "overdue":
			sums[tS(inv, "currency")] += tF(inv, "balance_due")
		}
	}
	total := int(tF(tObjOf(v), "total"))
	res := tResult("documents", out, total, total > len(out))
	bal := map[string]any{}
	for cur, s := range sums {
		if s != 0 {
			bal[cur] = tRound(s)
		}
	}
	if len(bal) > 0 {
		res["outstanding_on_page"] = bal
	}
	return res, nil
}

func (c *Ctx) loadInvoice(a map[string]any, idKey string) (map[string]any, error) {
	id := argStr(a, idKey)
	if id == "" {
		num := argStr(a, "number")
		if num == "" {
			return nil, tErr("%s or number is required", idKey)
		}
		v, err := c.get("/invoicing/invoices", url.Values{"type": {"all"}, "q": {num}, "per_page": {"50"}})
		if err != nil {
			return nil, err
		}
		for _, inv := range tList(v, "invoices") {
			if strings.EqualFold(tS(inv, "number"), num) {
				id = tS(inv, "id")
				break
			}
		}
		if id == "" {
			return nil, tErr("No invoice or quote numbered %s", num)
		}
	}
	v, err := c.get("/invoicing/invoices/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return tObjOf(v), nil
}

func invoicesGet(c *Ctx, a map[string]any) (any, error) {
	inv, err := c.loadInvoice(a, "invoice_id")
	if err != nil {
		return nil, err
	}
	return invoiceFull(c, inv), nil
}

// buildItems converts tool items to the API's item input.
func (c *Ctx) buildItems(raw []any) ([]map[string]any, error) {
	var defRate *float64
	var taxName string
	items := []map[string]any{}
	for i, x := range raw {
		it, ok := x.(map[string]any)
		if !ok {
			return nil, tErr("items[%d] must be an object", i)
		}
		desc := argStr(it, "description")
		price, okp := argNum(it, "unit_price")
		if desc == "" || !okp {
			return nil, tErr("items[%d] needs description and unit_price", i)
		}
		qty, okq := argNum(it, "quantity")
		if !okq {
			qty = 1
		}
		m := map[string]any{"description": desc, "quantity": qty, "unit_price": price, "item_type": "service"}
		if u := argStr(it, "unit"); u != "" {
			m["unit"] = u
		}
		if d, ok := argNum(it, "discount_percent"); ok {
			m["discount_percent"] = d
		}
		if r, ok := argNum(it, "tax_rate"); ok {
			m["tax_rate"] = r
		} else {
			if defRate == nil {
				r := 0.0
				if s, err := c.get("/invoicing/settings", nil); err == nil {
					sm := tObjOf(s, "settings")
					r = tF(sm, "default_tax_rate")
					taxName = tS(sm, "tax_label")
				}
				defRate = &r
			}
			m["tax_rate"] = *defRate
		}
		if taxName != "" {
			m["tax_name"] = taxName
		}
		if p := argStr(it, "product_id"); p != "" {
			m["item_type"], m["reference_id"] = "product", p
		} else if s := argStr(it, "service_id"); s != "" {
			m["item_type"], m["reference_id"] = "service", s
		}
		items = append(items, m)
	}
	if len(items) == 0 {
		return nil, tErr("Add at least one line item")
	}
	return items, nil
}

func docCreate(c *Ctx, a map[string]any, docType string) (any, error) {
	body := map[string]any{"doc_type": docType}
	custID := argStr(a, "customer_id")
	if custID == "" && argStr(a, "customer") != "" {
		id, _, err := c.findCustomer(argStr(a, "customer"))
		if err != nil {
			return nil, err
		}
		custID = id
	}
	if custID != "" {
		v, err := c.get("/invoicing/customer-defaults/"+url.PathEscape(custID), url.Values{"type": {docType}})
		if err != nil {
			return nil, err
		}
		d := tObjOf(v)
		body["customer_id"] = custID
		body["client_name"] = tS(d, "name")
		body["client_email"] = tS(d, "email")
		if to, ok := d["to"].(map[string]any); ok && tS(to, "email") != "" {
			body["client_email"] = tS(to, "email")
		}
		body["client_phone"] = tS(d, "phone")
		body["client_address"] = tS(d, "address")
		body["currency"] = tS(d, "currency")
		if cc, ok := d["cc_emails"].([]any); ok {
			body["cc_emails"] = cc
		}
	} else if argStr(a, "client_name") == "" {
		return nil, tErr("Pass customer_id, customer (name/email) or client_name")
	}
	copyArgs(body, a, "client_name", "client_email", "currency", "issue_date", "due_date", "reference", "title", "notes", "terms")
	if cur, ok := body["currency"].(string); ok {
		body["currency"] = strings.ToUpper(cur)
	}
	items, err := c.buildItems(argList(a, "items"))
	if err != nil {
		return nil, err
	}
	body["items"] = items
	if docType == "invoice" {
		if dep, err := depositArg(a); err != nil {
			return nil, err
		} else if dep != nil {
			body["deposit"] = dep
		}
	}
	v, err := c.api("POST", "/invoicing/invoices", nil, body)
	if err != nil {
		return nil, err
	}
	out := invoiceFull(c, tObjOf(v))
	out["created"] = true
	out["next_step"] = "It is a draft. Use invoices_send to email it."
	return out, nil
}

func invoicesUpdate(c *Ctx, a map[string]any) (any, error) {
	inv, err := c.loadInvoice(a, "invoice_id")
	if err != nil {
		return nil, err
	}
	body := map[string]any{"doc_type": tS(inv, "doc_type")}
	for _, k := range []string{"customer_id", "client_name", "client_email", "client_phone", "client_address", "client_tax_number", "reference", "title", "currency", "prices_include_tax", "discount_type", "discount_value", "shipping_amount", "notes", "terms", "is_recurring", "recurrence_interval", "cc_emails"} {
		if v, ok := inv[k]; ok && v != nil {
			body[k] = v
		}
	}
	for _, k := range []string{"issue_date", "due_date", "next_recurrence_date", "recurrence_end_date"} {
		if s := tDay(tS(inv, k)); s != "" {
			body[k] = s
		}
	}
	copyArgs(body, a, "customer_id", "client_name", "client_email", "currency", "issue_date", "due_date", "reference", "title", "notes", "terms")
	if argHas(a, "items") {
		items, err := c.buildItems(argList(a, "items"))
		if err != nil {
			return nil, err
		}
		body["items"] = items
	} else {
		items := []map[string]any{}
		for _, it := range tList(inv["items"]) {
			items = append(items, tPick(it, "item_type", "reference_id", "description", "quantity", "unit", "unit_price", "discount_percent", "tax_rate", "tax_name"))
		}
		body["items"] = items
	}
	v, err := c.api("PUT", "/invoicing/invoices/"+url.PathEscape(tS(inv, "id")), nil, body)
	if err != nil {
		return nil, err
	}
	out := invoiceFull(c, tObjOf(v))
	out["updated"] = true
	return out, nil
}

func invoicesAddPayment(c *Ctx, a map[string]any) (any, error) {
	inv, err := c.loadInvoice(a, "invoice_id")
	if err != nil {
		return nil, err
	}
	amt, ok := argNum(a, "amount")
	if !ok {
		amt = tF(inv, "balance_due")
	}
	body := map[string]any{"amount": amt}
	copyArgs(body, a, "date", "method", "reference", "notes", "email_receipt")
	if AppURL() != "" {
		body["app_url"] = AppURL()
	}
	v, err := c.api("POST", "/invoicing/invoices/"+url.PathEscape(tS(inv, "id"))+"/payments", nil, body)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	out := invoiceBrief(d)
	out["payment_recorded"] = amt
	pid := tS(d, "last_payment_id")
	if p := paymentByID(d, pid); p != nil {
		out["payment"] = paymentBrief(p)
	}
	if r, ok := d["receipt_email"].(map[string]any); ok {
		out["receipt_email"] = r
	}
	return out, nil
}

// paymentByID finds a payment on an invoice response.
func paymentByID(inv map[string]any, id string) map[string]any {
	for _, p := range tList(inv, "payments") {
		if tS(p, "id") == id {
			return p
		}
	}
	return nil
}

func paymentBrief(p map[string]any) map[string]any {
	m := tPick(p, "id", "receipt_number", "amount", "method", "reference", "notes", "is_deposit", "origin", "lock_reason", "revised_at")
	if d := tDay(tS(p, "date")); d != "" {
		m["date"] = d
	}
	return m
}

// findPayment loads the invoice and picks the payment by payment_id or
// receipt_number (or the only / latest payment when neither is given).
func (c *Ctx) findPayment(a map[string]any) (map[string]any, map[string]any, error) {
	inv, err := c.loadInvoice(a, "invoice_id")
	if err != nil {
		return nil, nil, err
	}
	full, err := c.get("/invoicing/invoices/"+url.PathEscape(tS(inv, "id")), nil)
	if err != nil {
		return nil, nil, err
	}
	d := tObjOf(full)
	list := tList(d, "payments")
	if len(list) == 0 {
		return nil, nil, tErr("%s has no payments", tS(d, "number"))
	}
	pid, rn := argStr(a, "payment_id"), argStr(a, "receipt_number")
	for _, p := range list {
		if (pid != "" && tS(p, "id") == pid) || (rn != "" && strings.EqualFold(tS(p, "receipt_number"), rn)) {
			return d, p, nil
		}
	}
	if pid != "" || rn != "" {
		return nil, nil, tErr("payment not found on %s", tS(d, "number"))
	}
	if len(list) > 1 {
		nums := []string{}
		for _, p := range list {
			nums = append(nums, tS(p, "receipt_number")+" ("+fmt.Sprint(p["amount"])+")")
		}
		return nil, nil, tErr("%s has several payments — pass payment_id or receipt_number: %s", tS(d, "number"), strings.Join(nums, ", "))
	}
	return d, list[0], nil
}

func invoicesUpdatePayment(c *Ctx, a map[string]any) (any, error) {
	inv, p, err := c.findPayment(a)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	copyArgs(body, a, "amount", "date", "method", "reference", "notes", "reason", "email_receipt")
	if AppURL() != "" {
		body["app_url"] = AppURL()
	}
	v, err := c.api("PUT", "/invoicing/invoices/"+url.PathEscape(tS(inv, "id"))+"/payments/"+url.PathEscape(tS(p, "id")), nil, body)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	newInv := tObjOf(d, "invoice")
	out := invoiceBrief(newInv)
	out["changed"] = d["changed"]
	if np := paymentByID(newInv, tS(p, "id")); np != nil {
		out["payment"] = paymentBrief(np)
	}
	out["previous_amount"] = p["amount"]
	if r, ok := d["receipt_email"].(map[string]any); ok {
		out["receipt_email"] = r
	}
	return out, nil
}

func invoicesSendReceipt(c *Ctx, a map[string]any) (any, error) {
	inv, p, err := c.findPayment(a)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"mode": "email", "kind": "receipt", "payment_id": tS(p, "id")}
	copyArgs(body, a, "to", "subject", "message")
	if argHas(a, "cc") {
		body["cc"] = argStrList(a, "cc")
	}
	if AppURL() != "" {
		body["app_url"] = AppURL()
	}
	v, err := c.Call("POST", "/invoicing/invoices/"+url.PathEscape(tS(inv, "id"))+"/send", nil, body)
	if err != nil {
		if ae, ok := err.(*APIError); ok && ae.Status == 503 {
			return nil, &ToolError{Msg: "Outgoing email isn't set up for this organisation (Settings → Outgoing email), so the receipt wasn't sent. The receipt PDF can be downloaded from the invoice's payments list."}
		}
		return nil, &ToolError{Msg: ErrorText(err)}
	}
	d := tObjOf(v)
	out := invoiceBrief(tObjOf(d, "invoice"))
	out["payment"] = paymentBrief(p)
	for k, val := range tPick(d, "emailed", "to", "cc", "bcc", "subject", "attachment", "skipped") {
		out[k] = val
	}
	return out, nil
}

func invoicesSend(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "invoice_id")
	if id == "" {
		return nil, tErr("invoice_id is required")
	}
	if b, _ := argBool(a, "mark_as_sent_only"); b {
		v, err := c.api("POST", "/invoicing/invoices/"+url.PathEscape(id)+"/mark-sent", nil, nil)
		if err != nil {
			return nil, err
		}
		out := invoiceBrief(tObjOf(v))
		out["emailed"] = false
		out["message"] = "Marked as sent (no email was sent)."
		return out, nil
	}
	body := map[string]any{"mode": "email"}
	copyArgs(body, a, "to", "subject", "message", "kind")
	if AppURL() != "" {
		body["app_url"] = AppURL()
	}
	if argHas(a, "cc") {
		body["cc"] = argStrList(a, "cc")
	}
	v, err := c.Call("POST", "/invoicing/invoices/"+url.PathEscape(id)+"/send", nil, body)
	if err != nil {
		if ae, ok := err.(*APIError); ok && ae.Status == 503 {
			msg := "Outgoing email isn't set up for this organisation (no SMTP account in Settings → Outgoing email, no platform default), so nothing was sent."
			if inv, err2 := c.get("/invoicing/invoices/"+url.PathEscape(id), nil); err2 == nil {
				if tok := tS(tObjOf(inv), "public_token"); tok != "" && AppURL() != "" {
					msg += " Share this link with the client instead: " + tURL("/i/"+tok) + " — then call invoices_send with mark_as_sent_only=true."
				}
			}
			return nil, &ToolError{Msg: msg}
		}
		return nil, &ToolError{Msg: ErrorText(err)}
	}
	d := tObjOf(v)
	out := invoiceBrief(tObjOf(d, "invoice"))
	for k, val := range tPick(d, "emailed", "to", "cc", "bcc", "kind", "skipped", "sender", "attachment", "view_url", "pay_url") {
		out[k] = val
	}
	return out, nil
}

func invoicesPaymentLink(c *Ctx, a map[string]any) (any, error) {
	inv, err := c.loadInvoice(a, "invoice_id")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if AppURL() != "" {
		q.Set("app_url", AppURL())
	}
	v, err := c.get("/invoicing/invoices/"+url.PathEscape(tS(inv, "id"))+"/pay-link", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	out := map[string]any{"invoice_id": tS(inv, "id")}
	for k, val := range tPick(d, "number", "payable", "url", "view_url", "reason", "balance_due", "currency", "enabled") {
		out[k] = val
	}
	if tS(d, "url") != "" {
		out["pay_url"] = tS(d, "url")
	}
	return out, nil
}

func quotesConvert(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "quote_id")
	if id == "" {
		return nil, tErr("quote_id is required")
	}
	var body any
	dep, err := depositArg(a)
	if err != nil {
		return nil, err
	}
	if dep != nil {
		body = map[string]any{"deposit": dep}
	}
	v, err := c.api("POST", "/invoicing/invoices/"+url.PathEscape(id)+"/convert", nil, body)
	if err != nil {
		return nil, err
	}
	out := invoiceBrief(tObjOf(v))
	out["converted_from_quote"] = id
	if dep != nil {
		out["deposit_recorded"] = dep["amount"]
	}
	return out, nil
}

// depositArg maps amount_paid / payment_* to the API's deposit object (nil
// when no amount was given).
func depositArg(a map[string]any) (map[string]any, error) {
	amt, ok := argNum(a, "amount_paid")
	if !ok || amt == 0 {
		return nil, nil
	}
	if amt < 0 {
		return nil, tErr("amount_paid must be greater than zero")
	}
	dep := map[string]any{"amount": amt}
	if s := argStr(a, "payment_date"); s != "" {
		dep["date"] = s
	}
	if s := argStr(a, "payment_method"); s != "" {
		dep["method"] = s
	}
	if s := argStr(a, "payment_reference"); s != "" {
		dep["reference"] = s
	}
	return dep, nil
}

// withProps returns a copy of a with b's properties added.
func withProps(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func invoicesVoid(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "invoice_id")
	if id == "" {
		return nil, tErr("invoice_id is required")
	}
	v, err := c.api("POST", "/invoicing/invoices/"+url.PathEscape(id)+"/void", nil, nil)
	if err != nil {
		return nil, err
	}
	out := invoiceBrief(tObjOf(v))
	out["voided"] = true
	return out, nil
}
