package aitools

// Club / bar stock tools (pkg/barstock, /api/v1/bar/**). Read tools: stock
// levels, the variance report and reorder suggestions. Write tools (shown to
// the user for confirmation first, like every non-read-only tool): record
// wastage and request a transfer. Money fields only appear for users who may
// approve on inventory (the API strips them for everyone else).

import (
	"net/url"
	"regexp"
	"strings"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

func init() {
	Register(Tool{
		Name:  "bar_stock_levels",
		Title: "Bar stock levels",
		Description: "Club / bar stock on hand per stock item (bottles, kegs, cans, mixers) — in total or at one bar or store — shown as " +
			"\"3 bottles + 420 ml\", with par level, reorder point and a low flag (stock value and average cost for managers). " +
			"Also lists the locations (stores and bars) with their ids.",
		InputSchema: tObj(map[string]any{
			"location": tStr("Location id or name (e.g. 'Main bar'); omit for the total over all locations"),
			"category": tEnum("Only this category", "spirits", "wine", "beer", "cider", "soft_drinks", "mixers", "water", "energy", "garnish", "consumables", "food", "other"),
			"query":    tStr("Filter by name, SKU or barcode"),
			"low_only": tBool("Only items at or below their reorder point (or out)"),
			"limit":    tLimit(),
		}),
		ReadOnly: true,
		Module:   "inventory",
		Handler:  barStockLevels,
	})
	Register(Tool{
		Name:  "bar_variance_report",
		Title: "Bar stock variance",
		Description: "Stock-take variance for a bar or store: expected = opening + received + transfers in − transfers out − sold − wastage − " +
			"returns; variance = counted − expected (negative = loss), per item (also in tots and %), per category, per count / shift with the " +
			"staff on shift, flags over the thresholds, and pour cost for the period. Defaults to the latest approved count.",
		InputSchema: tObj(map[string]any{
			"location": tStr("Location id or name"),
			"from":     tDate("First business date (counts approved in the range)"),
			"to":       tDate("Last business date"),
			"count_id": tStr("One count's id instead of a period"),
			"limit":    tInt("How many items to return (largest losses first, default 20)", 1, 200),
		}),
		ReadOnly: true,
		Module:   "inventory",
		Handler:  barVarianceReport,
	})
	Register(Tool{
		Name:  "bar_reorder_suggestions",
		Title: "Bar reorder suggestions",
		Description: "What to buy: par level − on hand − on order, rounded up to whole packs (cases, crates), grouped by preferred supplier, with " +
			"last and lowest recent price for managers. Defaults to the main store; location 'total' uses every location.",
		InputSchema: tObj(map[string]any{
			"location": tStr("Location id or name, or 'total' (default: the main store)"),
		}),
		ReadOnly: true,
		Module:   "inventory",
		Handler:  barReorder,
	})
	Register(Tool{
		Name:  "bar_record_wastage",
		Title: "Record bar wastage",
		Description: "Records a loss at a bar or store: breakage, spillage, comp (free drink), staff_drink, expired, damaged_on_delivery, " +
			"returned_to_supplier or other. Quantity in ml, tots, bottles or units. Comps and staff drinks above the shift allowance wait for a " +
			"manager's approval; everything else reduces stock at once. Returns the entry with its status.",
		InputSchema: tObj(map[string]any{
			"location":   tStr("Location id or name"),
			"product_id": tStr("Stock item product id (see bar_stock_levels)"),
			"product":    tStr("Stock item name, when you don't have the id"),
			"qty":        tNum("Quantity"),
			"unit":       tEnum("Unit of qty (default: ml for poured items, units otherwise)", "ml", "tot", "bottle", "unit"),
			"reason":     tEnum("Why", "breakage", "spillage", "comp", "staff_drink", "expired", "damaged_on_delivery", "returned_to_supplier", "other"),
			"note":       tStr("Note, e.g. who the comp was for"),
		}, "location", "qty", "reason"),
		Module:  "inventory",
		Handler: barRecordWastage,
	})
	Register(Tool{
		Name:  "bar_request_transfer",
		Title: "Request a stock transfer",
		Description: "Asks for stock to be moved between locations, e.g. from the main store to a bar (status requested; the store keeper " +
			"issues it and the bar receives it in the app). Lines are stock items with a quantity in bottles / units (or ml, tots, packs).",
		InputSchema: tObj(map[string]any{
			"from_location": tStr("Location id or name the stock comes from (usually the main store)"),
			"to_location":   tStr("Location id or name that needs the stock"),
			"lines": map[string]any{"type": "array", "minItems": 1, "description": "Items", "items": tObj(map[string]any{
				"product_id": tStr("Stock item product id"),
				"product":    tStr("Stock item name, when you don't have the id"),
				"qty":        tNum("Quantity"),
				"unit":       tEnum("Unit (default bottle = stock units)", "bottle", "unit", "pack", "ml", "tot"),
			}, "qty")},
			"note": tStr("Note for the store keeper"),
		}, "from_location", "to_location", "lines"),
		Module:  "inventory",
		Handler: barRequestTransfer,
	})
}

// barLocation resolves a location id or name.
func (c *Ctx) barLocation(ref string) (string, string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", nil
	}
	v, err := c.get("/bar/locations", nil)
	if err != nil {
		return "", "", err
	}
	names := []string{}
	for _, l := range tList(v) {
		if tS(l, "id") == ref || strings.EqualFold(tS(l, "name"), ref) {
			return tS(l, "id"), tS(l, "name"), nil
		}
		names = append(names, tS(l, "name"))
	}
	if uuidRe.MatchString(ref) {
		return ref, "", nil
	}
	return "", "", tErr("No location called %q (locations: %s)", ref, strings.Join(names, ", "))
}

// barProduct resolves a stock item by id or name.
func (c *Ctx) barProduct(id, name string) (string, error) {
	if id = strings.TrimSpace(id); id != "" {
		return id, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", tErr("product_id or product is required")
	}
	v, err := c.get("/bar/items", url.Values{"q": {name}})
	if err != nil {
		return "", err
	}
	list := tList(v)
	for _, it := range list {
		if strings.EqualFold(tS(it, "name"), name) {
			return tS(it, "product_id"), nil
		}
	}
	if len(list) == 1 {
		return tS(list[0], "product_id"), nil
	}
	if len(list) == 0 {
		return "", tErr("No stock item matches %q", name)
	}
	cands := []string{}
	for i, it := range list {
		if i == 8 {
			break
		}
		cands = append(cands, tS(it, "name"))
	}
	return "", tErr("Several stock items match %q: %s — say which one", name, strings.Join(cands, "; "))
}

func barItemBrief(it map[string]any) map[string]any {
	return tPick(it, "product_id", "name", "category", "on_hand_text", "on_hand", "base_unit", "on_hand_units", "unit_label", "par",
		"reorder_point", "low", "out", "avg_cost_per_unit", "value", "selling_price")
}

func barStockLevels(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	locID, locName, err := c.barLocation(argStr(a, "location"))
	if err != nil {
		return nil, err
	}
	if locID != "" {
		q.Set("location_id", locID)
	}
	if s := argStr(a, "category"); s != "" {
		q.Set("category", s)
	}
	if s := argStr(a, "query"); s != "" {
		q.Set("q", s)
	}
	if b, ok := argBool(a, "low_only"); ok && b {
		q.Set("low", "1")
	}
	v, err := c.get("/bar/items", q)
	if err != nil {
		return nil, err
	}
	list := tList(v)
	total := len(list)
	list, cut := tLimitList(list, argLimit(a))
	items := []map[string]any{}
	for _, it := range list {
		items = append(items, barItemBrief(it))
	}
	out := tResult("items", items, total, cut)
	if locID != "" {
		out["location"] = map[string]any{"id": locID, "name": locName}
	} else {
		out["location"] = "all locations"
	}
	if lv, err := c.get("/bar/locations", nil); err == nil {
		locs := []map[string]any{}
		for _, l := range tList(lv) {
			locs = append(locs, tPick(l, "id", "name", "type", "is_main_store", "is_default_bar", "stock_value"))
		}
		out["locations"] = locs
	}
	out["url"] = tURL("/bar-stock?tab=stock")
	return out, nil
}

func barVarianceReport(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	locID, _, err := c.barLocation(argStr(a, "location"))
	if err != nil {
		return nil, err
	}
	if locID != "" {
		q.Set("location_id", locID)
	}
	for _, k := range []string{"from", "to"} {
		if s := argStr(a, k); s != "" {
			if !validDate(s) {
				return nil, tErr("%s must be YYYY-MM-DD", k)
			}
			q.Set(k, s)
		}
	}
	if s := argStr(a, "count_id"); s != "" {
		q.Set("count_id", s)
	}
	v, err := c.get("/bar/variance", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	n := argInt(a, "limit", 20)
	if n < 1 {
		n = 20
	}
	items := []map[string]any{}
	all := tList(d["items"])
	lim, cut := tLimitList(all, n)
	for _, it := range lim {
		items = append(items, tPick(it, "name", "location", "category", "opening", "received", "transfers_in", "transfers_out", "sold", "wastage",
			"expected", "counted", "variance", "variance_text", "variance_tots", "variance_pct", "variance_value", "flagged", "flag_reasons"))
	}
	out := map[string]any{"period": d["period"], "totals": d["totals"], "items": items, "item_count": len(all)}
	if cut {
		out["truncated"] = true
	}
	for _, k := range []string{"categories", "counts", "staff", "shifts"} {
		if l := tList(d[k]); len(l) > 0 {
			out[k] = l
		}
	}
	flags := []map[string]any{}
	for _, f := range tList(d["flags"]) {
		flags = append(flags, tPick(f, "name", "location", "variance_text", "variance_pct", "variance_value", "flag_reasons"))
	}
	out["flags"] = flags
	out["url"] = tURL("/bar-stock?tab=counts")
	return out, nil
}

func barReorder(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	ref := argStr(a, "location")
	if strings.EqualFold(ref, "total") || strings.EqualFold(ref, "all") {
		q.Set("location_id", "total")
	} else if ref != "" {
		id, _, err := c.barLocation(ref)
		if err != nil {
			return nil, err
		}
		q.Set("location_id", id)
	}
	v, err := c.get("/bar/reorder", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	groups := []map[string]any{}
	lines := 0
	for _, g := range tList(d["groups"]) {
		ls := []map[string]any{}
		for _, l := range tList(g["lines"]) {
			ls = append(ls, tPick(l, "product_id", "name", "on_hand", "on_hand_text", "on_order", "par", "reorder_point", "suggest_units",
				"suggest_packs", "pack_label", "unit_label", "last_cost", "best_cost", "best_supplier", "line_total"))
		}
		lines += len(ls)
		m := map[string]any{"supplier": g["supplier"], "lines": ls}
		if t, ok := g["total_cost"]; ok {
			m["total_cost"] = t
		}
		groups = append(groups, m)
	}
	return map[string]any{"location_id": d["location_id"], "groups": groups, "line_count": lines, "currency": d["currency"],
		"hint": "Managers create draft purchase orders from these in Bar stock → Purchasing.", "url": tURL("/bar-stock?tab=purchasing")}, nil
}

func barRecordWastage(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "location", "reason"); err != nil {
		return nil, err
	}
	qty, ok := argNum(a, "qty")
	if !ok || qty <= 0 {
		return nil, tErr("qty must be more than 0")
	}
	locID, _, err := c.barLocation(argStr(a, "location"))
	if err != nil {
		return nil, err
	}
	pid, err := c.barProduct(argStr(a, "product_id"), argStr(a, "product"))
	if err != nil {
		return nil, err
	}
	body := map[string]any{"location_id": locID, "product_id": pid, "qty": qty, "reason": argStr(a, "reason")}
	copyArgs(body, a, "unit", "note")
	v, err := c.api("POST", "/bar/wastage", nil, body)
	if err != nil {
		return nil, err
	}
	w := tObjOf(v)
	out := tPick(w, "id", "name", "location", "reason", "qty_text", "entered_text", "servings", "status", "business_date", "value", "note")
	out["recorded"] = true
	if tS(w, "status") == "pending" {
		out["hint"] = "Above the comp / staff drink allowance for this shift: a manager must approve it before stock moves."
	}
	out["url"] = tURL("/bar-stock?tab=wastage")
	return out, nil
}

func barRequestTransfer(c *Ctx, a map[string]any) (any, error) {
	from, _, err := c.barLocation(argStr(a, "from_location"))
	if err != nil {
		return nil, err
	}
	to, _, err := c.barLocation(argStr(a, "to_location"))
	if err != nil {
		return nil, err
	}
	if from == "" || to == "" {
		return nil, tErr("from_location and to_location are required")
	}
	lines := []map[string]any{}
	for i, x := range argList(a, "lines") {
		l, ok := x.(map[string]any)
		if !ok {
			return nil, tErr("lines[%d] must be an object", i)
		}
		pid, err := c.barProduct(argStr(l, "product_id"), argStr(l, "product"))
		if err != nil {
			return nil, err
		}
		qty, ok := argNum(l, "qty")
		if !ok || qty <= 0 {
			return nil, tErr("lines[%d].qty must be more than 0", i)
		}
		m := map[string]any{"product_id": pid, "qty": qty}
		copyArgs(m, l, "unit")
		lines = append(lines, m)
	}
	if len(lines) == 0 {
		return nil, tErr("Add at least one line")
	}
	body := map[string]any{"from_location_id": from, "to_location_id": to, "lines": lines, "mode": "request"}
	copyArgs(body, a, "note")
	v, err := c.api("POST", "/bar/transfers", nil, body)
	if err != nil {
		return nil, err
	}
	t := tObjOf(v)
	out := tPick(t, "id", "number", "status", "from_location", "to_location", "business_date", "note")
	ls := []map[string]any{}
	for _, l := range tList(t["lines"]) {
		ls = append(ls, tPick(l, "name", "requested_text", "requested_qty", "unit"))
	}
	out["lines"] = ls
	out["requested"] = true
	out["url"] = tURL("/bar-stock?tab=transfers")
	return out, nil
}
