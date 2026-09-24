package aitools

import (
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
		Description: "Full invoice or quote: client, line items, totals, tax, payments, activity history, the public share link " +
			"and app link. Pass invoice_id, or number (e.g. INV-1001 / QUO-1001).",
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
			"The invoice is not sent — use invoices_send afterwards if the user wants it emailed. Returns number, totals and link.",
		InputSchema: tObj(docProps(false), "items"),
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
		Description: "Records a payment received against an invoice (full or partial). The invoice becomes partial or paid " +
			"automatically. Amount cannot exceed the balance due. Returns the new status and balance.",
		InputSchema: tObj(map[string]any{
			"invoice_id": tStr("Invoice id"),
			"amount":     tNum("Amount received in the invoice currency. Leave out to pay the full balance."),
			"date":       tDate("Payment date (default today)"),
			"method":     tEnum("Payment method (default bank_transfer)", "bank_transfer", "cash", "card", "cheque", "mobile_money", "paypal", "stripe", "other"),
			"reference":  tStr("Payment reference / transaction id"),
			"notes":      tStr("Notes"),
		}, "invoice_id"),
		Module:  "invoicing",
		Handler: invoicesAddPayment,
	})
	Register(Tool{
		Name:  "invoices_send",
		Title: "Email invoice or quote",
		Description: "Emails an invoice or quote to the client (and the customer's CC contacts) with a link to view/pay/accept " +
			"online, and marks it sent. kind=reminder sends a payment reminder for an unpaid invoice. Requires the server's " +
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
		Name:        "quotes_convert",
		Title:       "Convert quote to invoice",
		Description: "Turns a quote into a new draft invoice (copying client and lines) and marks the quote converted. Returns the new invoice.",
		InputSchema: tObj(map[string]any{"quote_id": tStr("Quote id")}, "quote_id"),
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
		m := tPick(p, "id", "amount", "method", "reference")
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
	return out
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
	copyArgs(body, a, "date", "method", "reference", "notes")
	v, err := c.api("POST", "/invoicing/invoices/"+url.PathEscape(tS(inv, "id"))+"/payments", nil, body)
	if err != nil {
		return nil, err
	}
	out := invoiceBrief(tObjOf(v))
	out["payment_recorded"] = amt
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
	if argHas(a, "cc") {
		body["cc"] = argStrList(a, "cc")
	}
	v, err := c.Call("POST", "/invoicing/invoices/"+url.PathEscape(id)+"/send", nil, body)
	if err != nil {
		if ae, ok := err.(*APIError); ok && ae.Status == 503 {
			msg := "Email isn't set up on this server (SMTP not configured), so nothing was sent."
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
	for k, val := range tPick(d, "emailed", "to", "cc", "kind", "skipped") {
		out[k] = val
	}
	return out, nil
}

func quotesConvert(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "quote_id")
	if id == "" {
		return nil, tErr("quote_id is required")
	}
	v, err := c.api("POST", "/invoicing/invoices/"+url.PathEscape(id)+"/convert", nil, nil)
	if err != nil {
		return nil, err
	}
	out := invoiceBrief(tObjOf(v))
	out["converted_from_quote"] = id
	return out, nil
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
