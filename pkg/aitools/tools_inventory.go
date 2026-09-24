package aitools

import (
	"net/url"
)

func init() {
	Register(Tool{
		Name:  "products_search",
		Title: "Search products",
		Description: "Finds inventory products by name, SKU or barcode, optionally by stock status. Returns id, name, SKU, " +
			"selling and cost price, current stock, reorder point, stock status and link.",
		InputSchema: tObj(map[string]any{
			"query":        tStr("Name, SKU or barcode"),
			"stock_status": tEnum("Filter by stock status", "in_stock", "low_stock", "out_of_stock", "attention"),
			"limit":        tLimit(),
		}),
		ReadOnly: true,
		Module:   "inventory",
		Handler:  productsSearch,
	})
	Register(Tool{
		Name:        "products_get",
		Title:       "Get product",
		Description: "Full product record: prices, stock levels (current, min, max, reorder point), category, brand, stock value and recent stock movements.",
		InputSchema: tObj(map[string]any{"product_id": tStr("Product id")}, "product_id"),
		ReadOnly:    true,
		Module:      "inventory",
		Handler:     productsGet,
	})
	Register(Tool{
		Name:  "inventory_adjust_stock",
		Title: "Adjust stock",
		Description: "Changes a product's stock level and logs a stock movement: movement_type in (add received units), " +
			"out (remove damaged/used units) or set (count correction to an exact level). Returns previous and new stock.",
		InputSchema: tObj(map[string]any{
			"product_id":    tStr("Product id"),
			"movement_type": tEnum("in = add, out = remove, set = set the exact level", "in", "out", "set"),
			"quantity":      tInt("Units to add/remove, or the new level for set", 0, 1000000),
			"reason":        tStr("Why, e.g. 'Stocktake', 'Damaged'"),
			"reference":     tStr("Reference, e.g. delivery note"),
			"unit_cost":     tNum("Cost per unit for received stock"),
		}, "product_id", "movement_type", "quantity"),
		Module:  "inventory",
		Handler: inventoryAdjust,
	})
	Register(Tool{
		Name:        "inventory_low_stock",
		Title:       "Low & out of stock",
		Description: "Products at or below their reorder point and products out of stock, with stock value totals. Use to decide what to reorder (see purchase_orders_create).",
		InputSchema: tObj(map[string]any{}),
		ReadOnly:    true,
		Module:      "inventory",
		Handler:     inventoryLowStock,
	})
	Register(Tool{
		Name:        "suppliers_list",
		Title:       "List suppliers",
		Description: "Lists suppliers (id, name, contact person, email, phone, payment terms, rating, active). Use to find a supplier_id for purchase orders.",
		InputSchema: tObj(map[string]any{
			"query": tStr("Filter by name, contact or email"),
			"limit": tLimit(),
		}),
		ReadOnly: true,
		Module:   "inventory",
		Handler:  suppliersList,
	})
	Register(Tool{
		Name:  "purchase_orders_create",
		Title: "Create purchase order",
		Description: "Creates a purchase order to a supplier for inventory products (draft by default, or sent). Items need " +
			"product_id, quantity and unit_cost (defaults to the product's cost price). Stock is only added when the PO is " +
			"received in the app. Returns the PO number, total and link.",
		InputSchema: tObj(map[string]any{
			"supplier_id": tStr("Supplier id (see suppliers_list)"),
			"items": map[string]any{"type": "array", "minItems": 1, "description": "Lines", "items": tObj(map[string]any{
				"product_id": tStr("Product id"),
				"quantity":   tInt("Units to order", 1, 1000000),
				"unit_cost":  tNum("Cost per unit (default: product cost price)"),
			}, "product_id", "quantity")},
			"expected_date": tDate("Expected delivery date"),
			"status":        tEnum("draft (default) or sent", "draft", "sent"),
			"notes":         tStr("Notes to the supplier"),
			"tax_amount":    tNum("Tax amount"),
			"shipping_cost": tNum("Shipping cost"),
		}, "supplier_id", "items"),
		Module:  "inventory",
		Handler: purchaseOrdersCreate,
	})
	Register(Tool{
		Name:  "pos_sales_summary",
		Title: "Point-of-sale summary",
		Description: "Point-of-sale takings for a day or range (default today): number of sales, total, tax, discounts, " +
			"average sale, voids, sales by payment method, top products and the latest transactions.",
		InputSchema: tObj(map[string]any{
			"date":               tDate("Single day (default today)"),
			"from":               tDate("Range start"),
			"to":                 tDate("Range end"),
			"transactions_limit": tInt("How many recent transactions to include (default 10, 0 for none)", 0, 50),
		}),
		ReadOnly: true,
		Module:   "pos",
		Handler:  posSummary,
	})
}

func productBrief(p map[string]any) map[string]any {
	m := tPick(p, "id", "name", "sku", "selling_price", "cost_price", "current_stock", "reorder_point", "stock_status", "is_active")
	if cat, ok := p["category"].(map[string]any); ok && tS(cat, "name") != "" {
		m["category"] = tS(cat, "name")
	}
	m["url"] = tURL("/inventory")
	return m
}

func productsSearch(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if s := argStr(a, "query"); s != "" {
		q.Set("search", s)
	}
	if s := argStr(a, "stock_status"); s != "" {
		q.Set("stock_status", s)
	}
	v, err := c.get("/inventory/products", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "products")
	total := len(list)
	list, cut := tLimitList(list, argLimit(a))
	out := []map[string]any{}
	for _, p := range list {
		out = append(out, productBrief(p))
	}
	return tResult("products", out, total, cut), nil
}

func productsGet(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "product_id")
	if id == "" {
		return nil, tErr("product_id is required")
	}
	v, err := c.get("/inventory/products/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	p := tObjOf(v, "product")
	out := productBrief(p)
	for k, val := range tPick(p, "description", "barcode", "unit_of_measure", "min_stock", "max_stock", "stock_value", "brand.name:brand") {
		out[k] = val
	}
	if mv, err := c.get("/inventory/movements", url.Values{"product_id": {id}, "limit": {"10"}}); err == nil {
		rows := []map[string]any{}
		l, _ := tLimitList(tList(mv, "movements"), 10)
		for _, x := range l {
			rows = append(rows, tPick(x, "movement_type", "quantity", "previous_quantity:previous_stock", "new_quantity:new_stock", "reference", "notes", "created_at"))
		}
		out["recent_movements"] = rows
	}
	return out, nil
}

func inventoryAdjust(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "product_id", "movement_type"); err != nil {
		return nil, err
	}
	qty, ok := argNum(a, "quantity")
	if !ok {
		return nil, tErr("quantity is required")
	}
	body := map[string]any{"product_id": argStr(a, "product_id"), "movement_type": argStr(a, "movement_type"), "quantity": int(qty)}
	copyArgs(body, a, "reason", "reference", "unit_cost")
	v, err := c.api("POST", "/inventory/items/adjust", nil, body)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	out := map[string]any{"adjusted": true}
	if mv, ok := d["movement"].(map[string]any); ok {
		for k, val := range tPick(mv, "movement_type", "quantity", "previous_quantity:previous_stock", "new_quantity:new_stock") {
			out[k] = val
		}
	}
	if p, ok := d["product"].(map[string]any); ok {
		out["product"] = productBrief(p)
	}
	if len(out) == 1 {
		out["result"] = d
	}
	return out, nil
}

func inventoryLowStock(c *Ctx, a map[string]any) (any, error) {
	v, err := c.get("/inventory/report", nil)
	if err != nil {
		return nil, err
	}
	r := tObjOf(v, "report")
	out := tPick(r, "total_products", "low_stock_count", "out_of_stock_count", "total_stock_value", "total_retail_value")
	for _, k := range []string{"low_stock_products", "out_of_stock_products"} {
		rows := []map[string]any{}
		for _, p := range tList(r[k]) {
			rows = append(rows, productBrief(p))
		}
		out[k] = rows
	}
	return out, nil
}

func suppliersList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if s := argStr(a, "query"); s != "" {
		q.Set("search", s)
	}
	v, err := c.get("/suppliers", q)
	if err != nil {
		return nil, err
	}
	list := tList(v, "suppliers")
	total := len(list)
	list, cut := tLimitList(list, argLimit(a))
	out := []map[string]any{}
	for _, s := range list {
		m := tPick(s, "id", "name", "contact_person", "email", "phone", "payment_terms", "rating", "current_balance", "is_active")
		m["url"] = tURL("/suppliers")
		out = append(out, m)
	}
	return tResult("suppliers", out, total, cut), nil
}

func purchaseOrdersCreate(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "supplier_id"); err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for i, x := range argList(a, "items") {
		it, ok := x.(map[string]any)
		if !ok {
			return nil, tErr("items[%d] must be an object", i)
		}
		pid := argStr(it, "product_id")
		if pid == "" {
			return nil, tErr("items[%d].product_id is required", i)
		}
		m := map[string]any{"product_id": pid, "quantity": argInt(it, "quantity", 0)}
		if cost, ok := argNum(it, "unit_cost"); ok {
			m["unit_cost"] = cost
		} else {
			pv, err := c.get("/inventory/products/"+url.PathEscape(pid), nil)
			if err != nil {
				return nil, err
			}
			m["unit_cost"] = tF(tObjOf(pv, "product"), "cost_price")
		}
		items = append(items, m)
	}
	if len(items) == 0 {
		return nil, tErr("Add at least one item")
	}
	body := map[string]any{"supplier_id": argStr(a, "supplier_id"), "items": items}
	copyArgs(body, a, "expected_date", "status", "notes", "tax_amount", "shipping_cost")
	v, err := c.api("POST", "/suppliers/purchase-orders", nil, body)
	if err != nil {
		return nil, err
	}
	po := tObjOf(v, "purchase_order", "order")
	out := tPick(po, "id", "order_number", "status", "sub_total", "tax_amount", "shipping_cost", "total_amount", "supplier.name:supplier")
	if d := tDay(tS(po, "expected_date")); d != "" {
		out["expected_date"] = d
	}
	out["lines"] = len(tList(po["items"]))
	out["url"] = tURL("/suppliers")
	out["created"] = true
	return out, nil
}

func posSummary(c *Ctx, a map[string]any) (any, error) {
	from, to := argStr(a, "from"), argStr(a, "to")
	if d := argStr(a, "date"); d != "" {
		from, to = d, d
	}
	if from == "" {
		from = c.today()
	}
	if to == "" {
		to = from
	}
	if !validDate(from) || !validDate(to) {
		return nil, tErr("Dates must be YYYY-MM-DD")
	}
	q := url.Values{"start_date": {from}, "end_date": {to}}
	if c.Who.TZ != "" {
		q.Set("tz", c.Who.TZ)
	}
	v, err := c.get("/pos/report", q)
	if err != nil {
		return nil, err
	}
	r := tObjOf(v, "report")
	out := tPick(r, "total_transactions", "total_sales", "total_tax", "total_discounts", "average_transaction", "voided_transactions", "voided_amount", "sales_by_payment_method")
	if tp := tList(r["top_products"]); len(tp) > 0 {
		tp, _ = tLimitList(tp, 5)
		out["top_products"] = tp
	}
	out["from"], out["to"] = from, to
	n := argInt(a, "transactions_limit", 10)
	if n > 0 {
		tq := url.Values{"start_date": {from}, "end_date": {to}, "limit": {itoa(n)}}
		if c.Who.TZ != "" {
			tq.Set("tz", c.Who.TZ)
		}
		if tv, err := c.get("/pos/transactions", tq); err == nil {
			rows := []map[string]any{}
			for _, t := range tList(tv, "transactions") {
				m := tPick(t, "id", "transaction_number", "type", "status", "total_amount", "tax_amount", "discount_amount")
				m["at"] = c.localTime(tS(t, "created_at"))
				if cu, ok := t["customer"].(map[string]any); ok && tS(cu, "id") != "" {
					m["customer"] = tJoinName(cu)
				}
				methods := []string{}
				for _, p := range tList(t["payments"]) {
					methods = append(methods, tS(p, "payment_method"))
				}
				if len(methods) > 0 {
					m["payment_methods"] = methods
				}
				rows = append(rows, m)
			}
			out["recent_transactions"] = rows
			out["transactions_url"] = tURL("/pos-transactions")
		}
	}
	return out, nil
}
