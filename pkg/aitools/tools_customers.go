package aitools

import (
	"net/url"
	"strings"
)

func init() {
	addrSchema := tObj(map[string]any{
		"street":   tStr("Street address"),
		"suburb":   tStr("Suburb / city"),
		"state":    tStr("State / region"),
		"postcode": tStr("Postcode / ZIP"),
	})
	custProps := func() map[string]any {
		return map[string]any{
			"first_name":   tStr("First name (or the business name for companies)"),
			"last_name":    tStr("Last name"),
			"email":        tStr("Email address"),
			"phone":        tStr("Phone number"),
			"country_code": tStr("ISO 3166-1 alpha-2 country code, e.g. AU, KE, US, GB. Sets the country and the default billing currency."),
			"currency":     tStr("Billing currency override (ISO 4217, e.g. USD). Leave out to use the country's currency."),
			"address":      addrSchema,
			"notes":        tStr("Internal notes"),
		}
	}
	Register(Tool{
		Name:  "customers_search",
		Title: "Search customers",
		Description: "Finds customers by name, email, phone, suburb or country. Returns id, name, contact details, country, " +
			"billing currency, booking count, total spent and a link. Use before creating a customer to avoid duplicates, " +
			"or to get a customer_id for invoices and bookings. Empty query lists the most recent customers.",
		InputSchema: tObj(map[string]any{
			"query":  tStr("Search text (name, email, phone, suburb, country)"),
			"status": tEnum("Filter by status", "active", "inactive", "all"),
			"limit":  tLimit(),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  customersSearch,
	})
	Register(Tool{
		Name:  "customers_get",
		Title: "Get customer",
		Description: "Full customer record: contact details, address, country/currency, contacts (CC recipients for invoices, " +
			"quotes, reminders, bookings), recent bookings and open invoices. Accepts a customer_id or a name/email to look up.",
		InputSchema: tObj(map[string]any{
			"customer_id": tStr("Customer id"),
			"customer":    tStr("Name or email to look up when you don't have the id"),
		}),
		ReadOnly: true,
		Module:   "core",
		Handler:  customersGet,
	})
	createProps := custProps()
	Register(Tool{
		Name:  "customers_create",
		Title: "Create customer",
		Description: "Creates a customer. first_name plus an email or phone are required. Country defaults to the " +
			"organisation's (Australia) when not given; the billing currency follows the country unless currency is set. " +
			"Fails if a customer with the same email exists — search first. Returns the new customer with its id and link.",
		InputSchema: tObj(createProps, "first_name"),
		Module:      "core",
		Handler:     customersCreate,
	})
	updProps := custProps()
	updProps["customer_id"] = tStr("Customer id to update")
	updProps["is_active"] = tBool("Set false to deactivate the customer, true to reactivate")
	Register(Tool{
		Name:  "customers_update",
		Title: "Update customer",
		Description: "Updates only the fields you pass on a customer (name, email, phone, country, currency, address, notes, " +
			"active flag). Address fields are merged with the existing address. Returns the updated customer.",
		InputSchema: tObj(updProps, "customer_id"),
		Module:      "core",
		Handler:     customersUpdate,
	})
	contactProps := func() map[string]any {
		return map[string]any{
			"name":               tStr("Contact's name"),
			"email":              tStr("Contact's email"),
			"phone":              tStr("Contact's phone"),
			"role":               tStr("Role, e.g. Accounts payable"),
			"cc_invoices":        tBool("CC this contact on invoices (default true for new contacts)"),
			"cc_quotes":          tBool("CC on quotes"),
			"cc_reminders":       tBool("CC on payment reminders"),
			"cc_bookings":        tBool("CC on booking emails"),
			"is_primary_billing": tBool("Make this the primary billing contact (invoices are sent To this address)"),
			"notes":              tStr("Notes"),
		}
	}
	cp := contactProps()
	cp["customer_id"] = tStr("Customer id")
	Register(Tool{
		Name:        "customers_add_contact",
		Title:       "Add customer contact",
		Description: "Adds a contact person to a customer (e.g. accounts department) who can be CC'd on invoices, quotes, reminders or bookings. email is required.",
		InputSchema: tObj(cp, "customer_id", "email"),
		Module:      "core",
		Handler:     customersAddContact,
	})
	up := contactProps()
	up["customer_id"] = tStr("Customer id")
	up["contact_id"] = tStr("Contact id (from customers_get)")
	Register(Tool{
		Name:        "customers_update_contact",
		Title:       "Update customer contact",
		Description: "Changes a customer contact's details or CC flags. Only fields you pass are changed.",
		InputSchema: tObj(up, "customer_id", "contact_id"),
		Module:      "core",
		Handler:     customersUpdateContact,
	})
	idempotentTools["customers_update"] = true
	idempotentTools["customers_update_contact"] = true
}

func customerBrief(cu map[string]any) map[string]any {
	m := map[string]any{"id": tS(cu, "id"), "name": tJoinName(cu)}
	for k, v := range tPick(cu, "email", "phone", "country_name:country", "billing_currency", "bookings_count", "total_spent", "contacts_count", "is_active") {
		m[k] = v
	}
	if id := tS(cu, "id"); id != "" {
		m["url"] = tURL("/customers?customer=" + id)
	}
	return m
}

func customersSearch(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if s := argStr(a, "query"); s != "" {
		q.Set("search", s)
	}
	if s := argStr(a, "status"); s == "active" || s == "inactive" {
		q.Set("status", s)
	}
	v, err := c.get("/customers", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "customers")
	total := len(list)
	list, cut := tLimitList(list, argLimit(a))
	out := make([]map[string]any, 0, len(list))
	for _, cu := range list {
		out = append(out, customerBrief(cu))
	}
	return tResult("customers", out, total, cut), nil
}

// resolveCustomerID returns the customer id from customer_id or a name/email
// lookup (customer). It fails with the candidates when ambiguous.
func (c *Ctx) resolveCustomerID(a map[string]any, idKey, nameKey string) (string, error) {
	if id := argStr(a, idKey); id != "" {
		return id, nil
	}
	name := argStr(a, nameKey)
	if name == "" {
		return "", tErr("%s (or %s) is required", idKey, nameKey)
	}
	id, _, err := c.findCustomer(name)
	return id, err
}

// findCustomer looks a customer up by name or email. Returns ("", nil, nil)
// when nothing matched.
func (c *Ctx) findCustomer(name string) (string, map[string]any, error) {
	v, err := c.get("/customers", url.Values{"search": {name}})
	if err != nil {
		return "", nil, err
	}
	list := tList(v, "customers")
	if len(list) == 0 {
		return "", nil, tErr("No customer matches %q. Create one with customers_create or pass customer_id.", name)
	}
	if len(list) == 1 {
		return tS(list[0], "id"), list[0], nil
	}
	for _, cu := range list {
		if strings.EqualFold(tS(cu, "email"), name) || strings.EqualFold(tJoinName(cu), name) {
			return tS(cu, "id"), cu, nil
		}
	}
	cands := []map[string]any{}
	top, _ := tLimitList(list, 8)
	for _, cu := range top {
		cands = append(cands, map[string]any{"id": tS(cu, "id"), "name": tJoinName(cu), "email": tS(cu, "email")})
	}
	return "", nil, &ToolError{Msg: "Several customers match " + quote(name) + "; pass customer_id. Candidates: " + jsonString(cands)}
}

func customersGet(c *Ctx, a map[string]any) (any, error) {
	id, err := c.resolveCustomerID(a, "customer_id", "customer")
	if err != nil {
		return nil, err
	}
	v, err := c.get("/customers/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	cu := tObjOf(v, "customer")
	out := customerBrief(cu)
	for k, val := range tPick(cu, "country_code", "currency:currency_override", "currency_source", "address", "notes", "last_booking_at", "next_booking_at", "created_at") {
		out[k] = val
	}
	contacts := []map[string]any{}
	for _, ct := range tList(tData(v), "contacts") {
		contacts = append(contacts, tPick(ct, "id", "name", "email", "phone", "role", "cc_invoices", "cc_quotes", "cc_reminders", "cc_bookings", "is_primary_billing"))
	}
	out["contacts"] = contacts
	bookings := []map[string]any{}
	bl, _ := tLimitList(tList(tData(v), "bookings"), 10)
	for _, b := range bl {
		m := tPick(b, "id", "status", "services", "total_price", "notes")
		m["start_time"] = c.localTime(tS(b, "start_time"))
		m["url"] = tURL("/bookings?booking=" + tS(b, "id"))
		bookings = append(bookings, m)
	}
	out["recent_bookings"] = bookings
	if inv, err := c.get("/invoicing/invoices", url.Values{"type": {"all"}, "customer_id": {id}, "per_page": {"10"}}); err == nil {
		docs := []map[string]any{}
		for _, d := range tList(inv, "invoices") {
			docs = append(docs, invoiceBrief(d))
		}
		out["recent_invoices_and_quotes"] = docs
	}
	return out, nil
}

func customerBody(a map[string]any) map[string]any {
	body := map[string]any{}
	copyArgs(body, a, "first_name", "last_name", "email", "phone", "country_code", "currency", "notes", "is_active")
	return body
}

func customersCreate(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "first_name"); err != nil {
		return nil, err
	}
	body := customerBody(a)
	if addr, ok := a["address"].(map[string]any); ok {
		body["address"] = addr
	}
	v, err := c.api("POST", "/customers", nil, body)
	if err != nil {
		return nil, err
	}
	out := customerBrief(tObjOf(v, "customer"))
	out["created"] = true
	return out, nil
}

func customersUpdate(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "customer_id")
	if id == "" {
		return nil, tErr("customer_id is required")
	}
	body := customerBody(a)
	if addr, ok := a["address"].(map[string]any); ok {
		cur, err := c.get("/customers/"+url.PathEscape(id), nil)
		if err != nil {
			return nil, err
		}
		merged := map[string]any{}
		if old, ok := tObjOf(cur, "customer")["address"].(map[string]any); ok {
			for k, v := range old {
				merged[k] = v
			}
		}
		for k, v := range addr {
			merged[k] = v
		}
		body["address"] = merged
	}
	if len(body) == 0 {
		return nil, tErr("Nothing to update: pass at least one field")
	}
	v, err := c.api("PUT", "/customers/"+url.PathEscape(id), nil, body)
	if err != nil {
		return nil, err
	}
	out := customerBrief(tObjOf(v, "customer"))
	out["updated_fields"] = keysOf(body)
	return out, nil
}

func contactBody(a map[string]any) map[string]any {
	body := map[string]any{}
	copyArgs(body, a, "name", "email", "phone", "role", "cc_invoices", "cc_quotes", "cc_reminders", "cc_bookings", "is_primary_billing", "notes")
	return body
}

func customersAddContact(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "customer_id", "email"); err != nil {
		return nil, err
	}
	v, err := c.api("POST", "/customers/"+url.PathEscape(argStr(a, "customer_id"))+"/contacts", nil, contactBody(a))
	if err != nil {
		return nil, err
	}
	ct := tObjOf(v, "contact")
	out := tPick(ct, "id", "customer_id", "name", "email", "phone", "role", "cc_invoices", "cc_quotes", "cc_reminders", "cc_bookings", "is_primary_billing")
	out["url"] = tURL("/customers?customer=" + argStr(a, "customer_id"))
	return out, nil
}

func customersUpdateContact(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "customer_id", "contact_id"); err != nil {
		return nil, err
	}
	body := contactBody(a)
	if len(body) == 0 {
		return nil, tErr("Nothing to update: pass at least one field")
	}
	v, err := c.api("PUT", "/customers/"+url.PathEscape(argStr(a, "customer_id"))+"/contacts/"+url.PathEscape(argStr(a, "contact_id")), nil, body)
	if err != nil {
		return nil, err
	}
	ct := tObjOf(v, "contact")
	out := tPick(ct, "id", "customer_id", "name", "email", "phone", "role", "cc_invoices", "cc_quotes", "cc_reminders", "cc_bookings", "is_primary_billing")
	out["url"] = tURL("/customers?customer=" + argStr(a, "customer_id"))
	return out, nil
}
