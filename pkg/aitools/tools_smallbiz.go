package aitools

import (
	"math"
	"net/url"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// major converts a minor-unit amount (cents) to the currency's major unit.
func major(minor float64, cur string) float64 {
	d := currency.Decimals(cur)
	return math.Round(minor) / math.Pow10(d)
}

var moneyTotalKeys = []string{"income", "expenses", "income_net", "expenses_net", "profit", "net_cash", "tax_collected", "tax_paid", "tax_payable"}

// majorTotals converts a totals object (minor units) to major units.
func majorTotals(t map[string]any, cur string) map[string]any {
	out := map[string]any{}
	for k, v := range t {
		out[k] = v
	}
	if c := tS(t, "currency"); c != "" {
		cur = c
	}
	for _, k := range moneyTotalKeys {
		if _, ok := t[k]; ok {
			out[k] = major(tF(t, k), cur)
		}
	}
	delete(out, "decimals")
	return out
}

func init() {
	Register(Tool{
		Name:  "cashbook_list",
		Title: "Cash book entries",
		Description: "Lists money in / money out entries from the Business hub cash book for a period, with totals per " +
			"currency and the running balance. Use for 'what did we spend on X', 'income this month'.",
		InputSchema: tObj(map[string]any{
			"type":   tEnum("income (money in) or expense (money out)", "income", "expense"),
			"period": tEnum("Period (default this_month). custom uses from/to.", "this_month", "last_month", "this_quarter", "last_quarter", "this_fy", "last_fy", "ytd", "all", "custom"),
			"from":   tDate("Custom period start"),
			"to":     tDate("Custom period end"),
			"query":  tStr("Search description / reference"),
			"limit":  tLimit(),
		}),
		ReadOnly: true,
		Module:   "smallbiz",
		Handler:  cashbookList,
	})
	Register(Tool{
		Name:  "cashbook_add_entry",
		Title: "Record income or expense",
		Description: "Records money in (income) or money out (expense) in the cash book, e.g. 'spent 45 on fuel today'. " +
			"Amount includes tax; tax is worked out from tax_rate when given. category may be a category name " +
			"(matched to existing categories). Returns the entry.",
		InputSchema: tObj(map[string]any{
			"type":           tEnum("income or expense", "income", "expense"),
			"amount":         tNum("Amount including tax (> 0)"),
			"date":           tDate("Date (default today)"),
			"description":    tStr("What it was for"),
			"category":       tStr("Category name, e.g. Fuel, Sales (optional)"),
			"payment_method": tStr("e.g. cash, card, bank, mobile money"),
			"reference":      tStr("Receipt / reference number"),
			"currency":       tStr("ISO currency (default the business currency)"),
			"tax_rate":       tNum("Tax percent included in the amount (default 0)"),
		}, "type", "amount"),
		Module:  "smallbiz",
		Handler: cashbookAdd,
	})
	Register(Tool{
		Name:        "reminders_list",
		Title:       "List reminders",
		Description: "Lists Business hub reminders (bills to pay, renewals, tasks) with due dates, amounts and whether they are overdue.",
		InputSchema: tObj(map[string]any{
			"status": tEnum("Which reminders (default open)", "open", "overdue", "done", "all"),
		}),
		ReadOnly: true,
		Module:   "smallbiz",
		Handler:  remindersList,
	})
	Register(Tool{
		Name:        "reminders_add",
		Title:       "Add reminder",
		Description: "Adds a reminder with a due date, optional amount and repeat (monthly/quarterly/yearly). Returns the reminder.",
		InputSchema: tObj(map[string]any{
			"title":      tStr("What to remember, e.g. 'Pay rent'"),
			"due_date":   tDate("Due date"),
			"amount":     tNum("Optional amount"),
			"currency":   tStr("Currency for the amount"),
			"recurrence": tEnum("Repeat (default none)", "none", "monthly", "quarterly", "yearly"),
			"notes":      tStr("Notes"),
		}, "title", "due_date"),
		Module:  "smallbiz",
		Handler: remindersAdd,
	})
	Register(Tool{
		Name:        "reminders_complete",
		Title:       "Complete reminder",
		Description: "Marks a reminder done (a repeating reminder schedules its next occurrence). Returns the reminder and any next one.",
		InputSchema: tObj(map[string]any{"reminder_id": tStr("Reminder id")}, "reminder_id"),
		Module:      "smallbiz",
		Handler:     remindersComplete,
	})
	Register(Tool{
		Name:  "cashbook_profit_and_loss",
		Title: "Cash book profit & loss",
		Description: "Cash-book-only profit and loss for a financial year (Business hub → Reports → Cash book P&L): money in and out " +
			"by category and month. It does NOT include invoices, POS, bills or payroll — for the business's profit use profit_and_loss.",
		InputSchema: tObj(map[string]any{
			"fy":       tInt("Financial year start year, e.g. 2025 (default current)", 1970, 2200),
			"currency": tStr("Currency (default the business currency)"),
		}),
		ReadOnly: true,
		Module:   "smallbiz",
		Handler:  profitAndLoss,
	})
}

func entryBrief(e map[string]any) map[string]any {
	m := tPick(e, "id", "type", "currency", "description", "category_name:category", "payment_method", "reference")
	cur := tS(e, "currency")
	m["amount"] = major(tF(e, "amount_minor"), cur)
	if tF(e, "tax_minor") != 0 {
		m["tax"] = major(tF(e, "tax_minor"), cur)
	}
	if _, ok := e["balance_minor"]; ok {
		m["running_balance"] = major(tF(e, "balance_minor"), cur)
	}
	m["date"] = tDay(tS(e, "date"))
	m["url"] = tURL("/business?tab=cashbook")
	return m
}

func cashbookList(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{"per_page": {itoa(argLimit(a))}}
	period := argStr(a, "period")
	if argStr(a, "from") != "" || argStr(a, "to") != "" {
		period = "custom"
	}
	if period == "" {
		period = "this_month"
	}
	q.Set("period", period)
	for _, k := range []string{"type", "from", "to"} {
		if s := argStr(a, k); s != "" {
			q.Set(k, s)
		}
	}
	if s := argStr(a, "query"); s != "" {
		q.Set("q", s)
	}
	v, err := c.get("/smallbiz/entries", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	rows := []map[string]any{}
	for _, e := range tList(d, "entries") {
		rows = append(rows, entryBrief(e))
	}
	total := int(tF(d, "total"))
	out := tResult("entries", rows, total, total > len(rows))
	totals := []map[string]any{}
	for _, t := range tList(d, "totals") {
		totals = append(totals, majorTotals(t, tS(d, "currency")))
	}
	out["totals"] = totals
	out["currency"] = d["currency"]
	out["period"] = period
	if p, ok := d["period"]; ok {
		out["period"] = p
	}
	return out, nil
}

func (c *Ctx) smallbizCategory(name, typ string) (string, error) {
	v, err := c.get("/smallbiz/categories", nil)
	if err != nil {
		return "", err
	}
	var partial string
	names := []string{}
	for _, cat := range tList(v, "categories") {
		if typ != "" && tS(cat, "type") != typ {
			continue
		}
		n := tS(cat, "name")
		names = append(names, n)
		if strings.EqualFold(n, name) {
			return tS(cat, "id"), nil
		}
		if partial == "" && (lowerContains(n, name) || lowerContains(name, n)) {
			partial = tS(cat, "id")
		}
	}
	if partial != "" {
		return partial, nil
	}
	return "", tErr("No %s category called %q. Existing: %s (or leave category out)", typ, name, strings.Join(names, ", "))
}

func cashbookAdd(c *Ctx, a map[string]any) (any, error) {
	typ := argStr(a, "type")
	if typ != "income" && typ != "expense" {
		return nil, tErr("type must be income or expense")
	}
	amt, ok := argNum(a, "amount")
	if !ok || amt <= 0 {
		return nil, tErr("amount must be more than zero")
	}
	date := argStr(a, "date")
	if date == "" {
		date = c.today()
	}
	body := map[string]any{"type": typ, "amount": amt, "date": date}
	copyArgs(body, a, "description", "payment_method", "reference", "currency", "tax_rate")
	if cat := argStr(a, "category"); cat != "" {
		id, err := c.smallbizCategory(cat, typ)
		if err != nil {
			return nil, err
		}
		body["category_id"] = id
	}
	v, err := c.api("POST", "/smallbiz/entries", nil, body)
	if err != nil {
		return nil, err
	}
	out := entryBrief(tObjOf(v, "entry"))
	out["created"] = true
	return out, nil
}

func reminderBrief(r map[string]any) map[string]any {
	m := tPick(r, "id", "title", "notes", "currency", "recurrence", "done", "overdue", "days_left")
	if tF(r, "amount_minor") != 0 {
		m["amount"] = major(tF(r, "amount_minor"), tS(r, "currency"))
	}
	m["due_date"] = tDay(tS(r, "due_date"))
	m["url"] = tURL("/business?tab=reminders")
	return m
}

func remindersList(c *Ctx, a map[string]any) (any, error) {
	st := argStr(a, "status")
	if st == "" {
		st = "open"
	}
	v, err := c.get("/smallbiz/reminders", url.Values{"status": {st}})
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, r := range tList(v, "reminders") {
		rows = append(rows, reminderBrief(r))
	}
	return tResult("reminders", rows, 0, false), nil
}

func remindersAdd(c *Ctx, a map[string]any) (any, error) {
	if err := argRequire(a, "title", "due_date"); err != nil {
		return nil, err
	}
	body := map[string]any{"recurrence": "none"}
	copyArgs(body, a, "title", "due_date", "amount", "currency", "recurrence", "notes")
	v, err := c.api("POST", "/smallbiz/reminders", nil, body)
	if err != nil {
		return nil, err
	}
	out := reminderBrief(tObjOf(v, "reminder"))
	out["created"] = true
	return out, nil
}

func remindersComplete(c *Ctx, a map[string]any) (any, error) {
	id := argStr(a, "reminder_id")
	if id == "" {
		return nil, tErr("reminder_id is required")
	}
	// Toggle flips done; don't un-complete a finished reminder.
	if v, err := c.get("/smallbiz/reminders", url.Values{"status": {"done"}}); err == nil {
		for _, r := range tList(v, "reminders") {
			if tS(r, "id") == id {
				out := reminderBrief(r)
				out["message"] = "Already done"
				return out, nil
			}
		}
	}
	v, err := c.api("POST", "/smallbiz/reminders/"+url.PathEscape(id)+"/toggle", nil, nil)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	out := reminderBrief(tObjOf(d, "reminder"))
	if n, ok := d["next"].(map[string]any); ok && n != nil {
		out["next"] = reminderBrief(n)
	}
	return out, nil
}

func profitAndLoss(c *Ctx, a map[string]any) (any, error) {
	q := url.Values{}
	if fy := argInt(a, "fy", 0); fy > 0 {
		q.Set("fy", itoa(fy))
	}
	if s := argStr(a, "currency"); s != "" {
		q.Set("currency", strings.ToUpper(s))
	}
	v, err := c.get("/smallbiz/reports/pnl", q)
	if err != nil {
		return nil, err
	}
	d := tObjOf(v)
	cur := tS(d, "currency")
	out := tPick(d, "fy", "label", "from", "to", "currency", "tax_name")
	if t, ok := d["totals"].(map[string]any); ok {
		out["totals"] = majorTotals(t, cur)
	}
	for _, k := range []string{"income", "expenses"} {
		rows := []map[string]any{}
		for _, r := range tList(d, k) {
			rows = append(rows, map[string]any{"category": tS(r, "name"), "total": major(tF(r, "total"), cur)})
		}
		out[k+"_by_category"] = rows
	}
	months := []map[string]any{}
	for _, mth := range tList(d, "months") {
		if tF(mth, "income") == 0 && tF(mth, "expenses") == 0 {
			continue
		}
		months = append(months, map[string]any{"month": tS(mth, "month"), "income": major(tF(mth, "income"), cur), "expenses": major(tF(mth, "expenses"), cur), "profit": major(tF(mth, "profit"), cur)})
	}
	out["by_month"] = months // months with activity only
	out["url"] = tURL("/business?tab=reports")
	out["note"] = "Cash book only. The whole-business profit (invoices, POS, bills, payroll, cash book) comes from profit_and_loss."
	return out, nil
}
