package brief

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
)

// Built-in sections. Other packages add theirs with Register.
func init() {
	managing := func(v *Viewer) bool { return v.Managing }
	Register("bills", "Bills & payables", 10, billsSection, Icon("fa-solid fa-file-invoice-dollar"),
		Requires(managing, "For owners, admins and managers"),
		Describe("New supplier bills (incl. Synergy invoices in Expenses), what is due today and this week, overdue bills, the Synergy balance and renewals in the next 14 days."))
	Register("money", "Money in", 20, moneySection, Icon("fa-solid fa-hand-holding-dollar"),
		Requires(func(v *Viewer) bool { return v.Managing && v.Can("invoicing") }, "For owners, admins and managers with invoicing"),
		Describe("Payments received yesterday, the largest overdue invoices and client subscriptions that are past due or about to be suspended."))
	Register("projects", "Projects", 30, projectsSection, Icon("fa-solid fa-diagram-project"),
		Requires(func(v *Viewer) bool { return v.Can("projects") }, "Needs the Projects module"),
		Describe("Progress of each active project (Jira and your own), what was done since your last brief, overdue, due in 7 days and high-priority or blocked issues."))
	Register("opportunities", "Opportunities", 40, opportunitiesSection, Icon("fa-solid fa-lightbulb"),
		Requires(managing, "For owners, admins and managers"),
		Describe("The opportunities worth going for, with estimated values, and new items from your industry watch."))
	Register("today", "Today", 50, todaySection, Icon("fa-solid fa-calendar-day"),
		Describe("Your bookings and meetings today and reminders that are due."))
}

// ---------------------------------------------------------------- bills

type billRow struct {
	Key, Title, Detail, Link, Tag string
	Amount                        float64
	Due                           time.Time
	New                           bool
}

func billsSection(ctx context.Context, in *Input) (*Section, error) {
	sec := &Section{Link: "/bills?view=upcoming"}
	src := in.svc.sources()
	home := in.Currency
	today := in.Today
	week := today.AddDate(0, 0, 6)
	listed := map[string]bool{}

	var overdue, dueToday, dueWeek []billRow
	newBills := map[string]bool{}
	if in.Viewer.Can("accounting") && hasTable(in.DB, "bills") {
		var ids []string
		in.DB.Raw(`SELECT id FROM bills WHERE organization_id = ? AND created_at > ?`, in.Org, in.Since).Scan(&ids)
		for _, id := range ids {
			newBills[id] = true
		}
		up := src.bills.UpcomingFor(in.Org, 14)
		for _, it := range up.Items {
			switch it.Status {
			case "paid", "skipped":
				continue
			}
			due, err := time.Parse("2006-01-02", it.DueDate)
			if err != nil || due.After(week) {
				continue
			}
			if it.SnoozedUntil != "" && it.SnoozedUntil > dayStr(today) {
				continue
			}
			r := billRow{Key: it.Key, Title: upcomingTitle(it), Amount: it.Amount, Due: due, New: it.BillID != "" && newBills[it.BillID]}
			details := []string{}
			if it.BillNumber != "" {
				details = append(details, it.BillNumber)
			}
			if it.OriginalCurrency != "" && it.OriginalCurrency != home && it.OriginalAmount > 0 {
				details = append(details, Money(it.OriginalAmount, it.OriginalCurrency))
			}
			if it.AutoPay {
				details = append(details, "auto-pay")
			} else {
				details = append(details, "pay manually")
			}
			switch {
			case it.Status == "draft":
				details = append(details, "draft to approve")
			case it.Kind == "projected" && it.Mode == recurbills.ModeTrack:
				details = append(details, "invoice not imported yet")
			case it.Kind == "projected":
				details = append(details, "expected")
			}
			if it.Estimate {
				details = append(details, "estimate")
			}
			if it.BillID != "" {
				r.Link = "/bills?bill=" + it.BillID
			} else if it.RecurringID != "" {
				r.Link = "/bills?view=recurring&schedule=" + it.RecurringID
			} else {
				r.Link = "/bills?view=upcoming"
			}
			days := int(due.Sub(today).Hours() / 24)
			r.Detail = strings.Join(append([]string{dueWords(days, due)}, details...), " · ")
			if it.BillID != "" {
				listed[it.BillID] = true
			}
			switch {
			case days < 0:
				r.Tag = "Overdue"
				overdue = append(overdue, r)
			case days == 0:
				r.Tag = "Due today"
				dueToday = append(dueToday, r)
			default:
				dueWeek = append(dueWeek, r)
			}
		}
	}
	toItems := func(rows []billRow, tone string, max int) ([]Item, float64) {
		var total float64
		items := []Item{}
		sort.SliceStable(rows, func(i, j int) bool {
			if !rows[i].Due.Equal(rows[j].Due) {
				return rows[i].Due.Before(rows[j].Due)
			}
			return rows[i].Amount > rows[j].Amount
		})
		for _, r := range rows {
			total += r.Amount
			if len(items) >= max {
				continue
			}
			it := Item{Key: r.Key, Title: r.Title, Detail: r.Detail, Amount: r2(r.Amount), Currency: home, Value: Money(r.Amount, home), Link: r.Link, Tag: r.Tag, Tone: tone}
			if r.New {
				it.Tag = strings.TrimSpace(strings.TrimPrefix(it.Tag+" · New", " · "))
				if it.Tag == "" {
					it.Tag = "New"
				}
			}
			items = append(items, it)
		}
		return items, r2(total)
	}
	oItems, oTotal := toItems(overdue, "danger", 6)
	tItems, tTotal := toItems(dueToday, "warn", 6)
	wItems, wTotal := toItems(dueWeek, "", 8)

	// New incoming bills since the last brief that aren't listed above.
	newItems := []Item{}
	nNew := 0
	if in.Viewer.Can("accounting") && hasTable(in.DB, "bills") {
		var rows []struct {
			ID, BillNumber, Reference, Status, Currency, Vendor string
			Total, PaidAmount                                   float64
			DueDate                                             time.Time
		}
		in.DB.Raw(`SELECT b.id, COALESCE(b.bill_number,'') AS bill_number, COALESCE(b.reference,'') AS reference, COALESCE(b.status,'') AS status,
			COALESCE(NULLIF(b.currency,''), ?) AS currency, COALESCE(v.name,'') AS vendor, b.total, COALESCE(b.paid_amount,0) AS paid_amount, b.due_date
			FROM bills b LEFT JOIN vendors v ON v.id = b.vendor_id
			WHERE b.organization_id = ? AND b.created_at > ? AND b.status IN ('draft','unpaid','partial') ORDER BY b.created_at DESC LIMIT 30`, home, in.Org, in.Since).Scan(&rows)
		for _, b := range rows {
			if listed[b.ID] {
				continue
			}
			nNew++
			if len(newItems) >= 6 {
				continue
			}
			title := strings.TrimSpace(nz(b.Vendor, "Supplier") + " bill " + nz(b.Reference, b.BillNumber))
			due := time.Date(b.DueDate.Year(), b.DueDate.Month(), b.DueDate.Day(), 0, 0, 0, 0, time.UTC)
			det := "due " + due.Format("Mon 2 Jan")
			if b.Status == "draft" {
				det += " · draft to approve"
			}
			newItems = append(newItems, Item{Key: "bill:" + b.ID, Title: title, Detail: det, Amount: r2(b.Total - b.PaidAmount), Currency: b.Currency,
				Value: Money(b.Total-b.PaidAmount, b.Currency), Link: "/bills?bill=" + b.ID, Tag: "New"})
		}
	}
	if hasTable(in.DB, "expense_items") {
		var rows []struct {
			ID, VendorName, InvoiceNumber, Service, Currency, HomeCurrency, Source, Kind string
			Total, HomeTotal                                                             float64
		}
		in.DB.Raw(`SELECT id, COALESCE(vendor_name,'') AS vendor_name, COALESCE(invoice_number,'') AS invoice_number, COALESCE(service,'') AS service,
			COALESCE(currency,'') AS currency, COALESCE(home_currency,'') AS home_currency, COALESCE(source,'') AS source, COALESCE(kind,'') AS kind,
			COALESCE(total,0) AS total, COALESCE(home_total,0) AS home_total
			FROM expense_items WHERE organization_id = ? AND status = 'pending' AND kind IN ('invoice','receipt') AND created_at > ?
			ORDER BY created_at DESC LIMIT 30`, in.Org, in.Since).Scan(&rows)
		for _, x := range rows {
			nNew++
			if len(newItems) >= 8 {
				continue
			}
			title := strings.TrimSpace(nz(x.VendorName, "Supplier") + " " + map[bool]string{true: "receipt", false: "invoice"}[x.Kind == "receipt"] + " " + x.InvoiceNumber)
			det := "In the Expenses inbox — review and approve"
			if x.Service != "" {
				det = x.Service + " · " + det
			}
			cur := nz(x.Currency, home)
			value := Money(x.Total, cur)
			if x.HomeCurrency != "" && x.HomeCurrency != cur && x.HomeTotal > 0 {
				det = "≈ " + Money(x.HomeTotal, x.HomeCurrency) + " · " + det
			}
			newItems = append(newItems, Item{Key: "exp:" + x.ID, Title: title, Detail: det, Amount: r2(x.Total), Currency: cur, Value: value, Link: "/expenses?tab=inbox", Tag: "New"})
		}
	}

	sec.Add("new", "New since your last brief", newItems, nNew, "/expenses?tab=inbox")
	sec.Add("overdue", "Overdue", oItems, len(overdue), "/bills?view=upcoming")
	sec.Add("today", "Due today", tItems, len(dueToday), "/bills?view=upcoming")
	sec.Add("week", "Due later this week", wItems, len(dueWeek), "/bills?view=upcoming")

	// Synergy Wholesale balance and top-up.
	syn := []Item{}
	if k, ok := synergyBalance(in); ok {
		syn = append(syn, k.item)
		if k.topup > 0 {
			sec.Say(15, "Synergy top-up needed (%s)", Money(k.topup, k.cur))
		}
	}
	sec.Add("synergy", "Synergy Wholesale", syn, len(syn), "/expenses?tab=synergy")

	// Hosting / domain renewals in the next 14 days.
	ren := []Item{}
	nRen := 0
	for _, r := range src.exp.Renewals(in.Org, 14) {
		if r.ExpiresOn == nil {
			continue
		}
		exp := time.Date(r.ExpiresOn.Year(), r.ExpiresOn.Month(), r.ExpiresOn.Day(), 0, 0, 0, 0, time.UTC)
		days := int(exp.Sub(today).Hours() / 24)
		if days < 0 || days > 14 {
			continue
		}
		nRen++
		if len(ren) >= 6 {
			continue
		}
		parts := []string{strings.Title(nz(r.Kind, "service")), renewWords(days, exp)}
		if r.AutoRenew {
			parts = append(parts, "auto-renews")
		} else {
			parts = append(parts, "renew manually")
		}
		if r.CustomerName != "" {
			parts = append(parts, r.CustomerName)
		}
		it := Item{Key: "ren:" + r.ID, Title: r.Name, Detail: strings.Join(parts, " · "), Link: "/expenses?tab=renewals"}
		cost := r.Cost
		if cost == nil {
			cost = r.CostEstimate
		}
		if cost != nil && *cost > 0 {
			it.Amount, it.Currency, it.Value = r2(*cost), nz(r.Currency, home), Money(*cost, nz(r.Currency, home))
		}
		if days <= 3 && !r.AutoRenew {
			it.Tone = "warn"
		}
		ren = append(ren, it)
	}
	sec.Add("renewals", "Renewals in the next 14 days", ren, nRen, "/expenses?tab=renewals")

	// Headline and summary.
	nDue, nOver := len(dueToday)+len(dueWeek), len(overdue)
	switch {
	case nDue > 0 && nOver > 0:
		sec.Say(10, "%s due this week (%s) and %d overdue (%s)", plural(nDue, "bill", "bills"), Money(tTotal+wTotal, home), nOver, Money(oTotal, home))
	case nOver > 0:
		sec.Say(10, "%s overdue (%s)", plural(nOver, "bill", "bills"), Money(oTotal, home))
	case nDue > 0:
		sec.Say(20, "%s due this week (%s)", plural(nDue, "bill", "bills"), Money(tTotal+wTotal, home))
	}
	if nNew > 0 && len(overdue)+len(dueToday)+len(dueWeek) == 0 {
		sec.Say(35, "%s in", plural(nNew, "new bill", "new bills"))
	}
	sum := []string{}
	if n := len(dueToday) + len(dueWeek); n > 0 {
		sum = append(sum, fmt.Sprintf("%s due this week", Money(tTotal+wTotal, home)))
	}
	if len(overdue) > 0 {
		sum = append(sum, fmt.Sprintf("%s overdue", Money(oTotal, home)))
	}
	if nNew > 0 {
		sum = append(sum, plural(nNew, "new bill", "new bills"))
	}
	if nRen > 0 {
		sum = append(sum, plural(nRen, "renewal", "renewals")+" in 14 days")
	}
	sec.Summary = strings.Join(sum, " · ")
	return sec, nil
}

func upcomingTitle(it recurbills.UpcomingItem) string {
	if it.RecurringName != "" {
		return it.RecurringName
	}
	n := it.Vendor
	if it.Name != "" && !strings.EqualFold(it.Name, it.Vendor) {
		if n != "" {
			n += " — "
		}
		n += it.Name
	}
	return nz(n, "Bill")
}

func dueWords(days int, due time.Time) string {
	switch {
	case days == 0:
		return "due today"
	case days == 1:
		return "due tomorrow"
	case days > 1:
		return "due " + due.Format("Mon 2 Jan")
	case days == -1:
		return "1 day overdue"
	}
	return fmt.Sprintf("%d days overdue", -days)
}

func renewWords(days int, at time.Time) string {
	switch days {
	case 0:
		return "renews today"
	case 1:
		return "renews tomorrow"
	}
	return fmt.Sprintf("renews %s (in %d days)", at.Format("Mon 2 Jan"), days)
}

type synergyInfo struct {
	item  Item
	topup float64
	cur   string
}

// synergyBalance reads the Synergy Wholesale prepaid balance and whether a
// top-up is needed (from the statements' runway, else the low-balance alert).
func synergyBalance(in *Input) (synergyInfo, bool) {
	src := in.svc.sources()
	var out synergyInfo
	out.cur = "AUD"
	var bal *float64
	asOf := ""
	if src.exp.HasSynergyData(in.Org) {
		rep := src.exp.SynergyReport(in.Org, in.Today, "charge")
		if rep != nil && rep.KPIs.Balance != nil {
			k := rep.KPIs
			bal = k.Balance
			asOf = k.BalanceAt
			out.topup = k.TopupNeeded
			out.item = Item{Key: "synergy:balance", Title: "Synergy Wholesale balance", Amount: r2(*k.Balance), Currency: "AUD", Value: Money(*k.Balance, "AUD"), Link: "/expenses?tab=synergy"}
			switch {
			case k.TopupNeeded > 0 && k.NextChargeDate != "":
				d, _ := time.Parse("2006-01-02", k.NextChargeDate)
				out.item.Detail = fmt.Sprintf("Top up %s before %s (next hosting charge)", Money(k.TopupNeeded, "AUD"), d.Format("Mon 2 Jan"))
				out.item.Tag, out.item.Tone = "Top-up needed", "danger"
			case k.TopupNeeded > 0:
				out.item.Detail = fmt.Sprintf("Top up %s", Money(k.TopupNeeded, "AUD"))
				out.item.Tag, out.item.Tone = "Top-up needed", "danger"
			case k.RunwayDate != "":
				d, _ := time.Parse("2006-01-02", k.RunwayDate)
				out.item.Detail = "Covers the hosting charges until " + d.Format("2 Jan")
				out.item.Tone = "ok"
			default:
				out.item.Detail = "Enough for the next hosting charge"
				out.item.Tone = "ok"
			}
		}
	}
	if bal == nil && hasTable(in.DB, "expense_connections") {
		var rows []struct {
			Balance   *float64
			BalanceAt *time.Time
			Settings  string
		}
		in.DB.Raw(`SELECT balance, balance_at, COALESCE(settings,'') AS settings FROM expense_connections WHERE organization_id = ? AND kind = 'synergy' AND balance IS NOT NULL LIMIT 1`, in.Org).Scan(&rows)
		if len(rows) == 0 || rows[0].Balance == nil {
			return out, false
		}
		low := 20.0
		var st struct {
			LowBalance float64 `json:"low_balance"`
		}
		if jsonUnmarshal(rows[0].Settings, &st) == nil && st.LowBalance > 0 {
			low = st.LowBalance
		}
		b := *rows[0].Balance
		bal = &b
		if rows[0].BalanceAt != nil {
			asOf = rows[0].BalanceAt.In(in.Loc).Format("2006-01-02")
		}
		out.item = Item{Key: "synergy:balance", Title: "Synergy Wholesale balance", Amount: r2(b), Currency: "AUD", Value: Money(b, "AUD"), Link: "/expenses?tab=synergy"}
		if b < low {
			out.topup = r2(math.Max(low-b, 0))
			out.item.Detail = fmt.Sprintf("Below your low-balance alert — top up at least %s (to %s)", Money(out.topup, "AUD"), Money(low, "AUD"))
			out.item.Tag, out.item.Tone = "Top-up needed", "danger"
		} else {
			out.item.Detail = "Above your low-balance alert (" + Money(low, "AUD") + ")"
			out.item.Tone = "ok"
		}
	}
	if bal == nil {
		return out, false
	}
	if asOf != "" {
		if d, err := time.Parse("2006-01-02", asOf); err == nil {
			out.item.Detail += " · balance on " + d.Format("2 Jan")
		}
	}
	return out, true
}

// ---------------------------------------------------------------- money in

func moneySection(ctx context.Context, in *Input) (*Section, error) {
	sec := &Section{Link: "/invoices"}
	home := in.Currency
	yesterday := in.Today.AddDate(0, 0, -1)
	sum := []string{}

	if hasTable(in.DB, "billing_payments") {
		var rows []struct {
			ID, InvoiceID, Number, ClientName, Currency, BaseCurrency, Method string
			Amount, ExchangeRate                                              float64
		}
		in.DB.Raw(`SELECT p.id, p.invoice_id, COALESCE(i.number,'') AS number, COALESCE(i.client_name,'') AS client_name, COALESCE(NULLIF(i.currency,''), ?) AS currency,
			COALESCE(i.base_currency,'') AS base_currency, COALESCE(p.method,'') AS method, p.amount, COALESCE(i.exchange_rate,0) AS exchange_rate
			FROM billing_payments p JOIN billing_invoices i ON i.id = p.invoice_id
			WHERE p.organization_id = ? AND p.date::date = ?::date AND i.deleted_at IS NULL ORDER BY p.amount DESC LIMIT 200`, home, in.Org, dayStr(yesterday)).Scan(&rows)
		var total float64
		items := []Item{}
		for _, p := range rows {
			h := toHome(p.Amount, p.Currency, p.BaseCurrency, p.ExchangeRate, home)
			total += h
			if len(items) >= 5 {
				continue
			}
			det := p.Number
			if p.Method != "" {
				det = strings.TrimSpace(det + " · " + strings.ReplaceAll(p.Method, "_", " "))
			}
			items = append(items, Item{Key: "pay:" + p.ID, Title: nz(p.ClientName, "Client"), Detail: det, Amount: r2(p.Amount), Currency: p.Currency,
				Value: Money(p.Amount, p.Currency), Link: "/invoices/" + p.InvoiceID, Tone: "ok"})
		}
		if len(rows) > 0 {
			sec.Add("received", "Received yesterday — "+Money(total, home), items, len(rows), "/invoices?status=paid")
			sec.Say(60, "%s received yesterday", Money(total, home))
			sum = append(sum, Money(total, home)+" received yesterday")
		}
	}

	// Client subscriptions first: their invoices are listed with them, not again under overdue invoices.
	subInvoices := map[string]bool{}
	addSubs := func() {}
	if hasTable(in.DB, "client_subscriptions") {
		d := in.svc.sources().subs.Digest(in.Org)
		susp, past := []Item{}, []Item{}
		nSusp, nPast := 0, 0
		for _, it := range d.Suspends {
			if it.DaysToSuspend != nil && *it.DaysToSuspend <= 3 {
				subInvoices[it.InvoiceID] = true
				nSusp++
				if len(susp) < 5 {
					when := "today"
					if *it.DaysToSuspend == 1 {
						when = "tomorrow"
					} else if *it.DaysToSuspend > 1 {
						if t, err := time.Parse("2006-01-02", it.SuspendOn); err == nil {
							when = "on " + t.Format("Mon 2 Jan")
						}
					}
					susp = append(susp, Item{Key: "sub:" + it.SubscriptionID, Title: strings.TrimSpace(it.Customer + " — " + it.Service),
						Detail: "Suspended " + when + " unless paid" + phoneSuffix(it.Phone), Amount: r2(it.Balance), Currency: it.Currency,
						Value: Money(it.Balance, it.Currency), Link: "/subscriptions/" + it.SubscriptionID, Tag: "Suspends " + strings.TrimPrefix(when, "on "), Tone: "danger"})
				}
				continue
			}
			d.Overdue = append(d.Overdue, it)
		}
		sort.SliceStable(d.Overdue, func(i, j int) bool { return d.Overdue[i].Days > d.Overdue[j].Days })
		for _, it := range d.Overdue {
			subInvoices[it.InvoiceID] = true
			nPast++
			if len(past) >= 5 {
				continue
			}
			det := plural(it.Days, "day past due", "days past due")
			if it.Invoice != "" {
				det = it.Invoice + " · " + det
			}
			past = append(past, Item{Key: "sub:" + it.SubscriptionID, Title: strings.TrimSpace(it.Customer + " — " + it.Service), Detail: det,
				Amount: r2(it.Balance), Currency: it.Currency, Value: Money(it.Balance, it.Currency), Link: "/subscriptions/" + it.SubscriptionID, Tag: "Past due", Tone: "warn"})
		}
		addSubs = func() {
			sec.Add("suspensions", "Suspensions in the next 3 days", susp, nSusp, "/subscriptions")
			sec.Add("subs_past_due", "Client subscriptions past due", past, nPast, "/subscriptions")
			if nSusp > 0 {
				sec.Say(22, "%s in 3 days", plural(nSusp, "client suspension", "client suspensions"))
			}
			if nPast > 0 {
				sec.Say(45, "%s past due", plural(nPast, "subscription", "subscriptions"))
				sum = append(sum, plural(nPast, "subscription", "subscriptions")+" past due")
			}
		}
	}
	if hasTable(in.DB, "billing_invoices") {
		var rows []struct {
			ID, Number, ClientName, Currency, BaseCurrency string
			BalanceDue, ExchangeRate                       float64
			DueDate                                        time.Time
		}
		in.DB.Raw(`SELECT id, COALESCE(number,'') AS number, COALESCE(client_name,'') AS client_name, COALESCE(NULLIF(currency,''), ?) AS currency,
			COALESCE(base_currency,'') AS base_currency, balance_due, COALESCE(exchange_rate,0) AS exchange_rate, due_date
			FROM billing_invoices WHERE organization_id = ? AND deleted_at IS NULL AND doc_type = 'invoice' AND status IN ('sent','partial','overdue')
			AND balance_due > 0.005 AND due_date::date < ?::date LIMIT 500`, home, in.Org, dayStr(in.Today)).Scan(&rows)
		type inv struct {
			item Item
			home float64
		}
		list := []inv{}
		var total float64
		for _, r := range rows {
			if subInvoices[r.ID] {
				continue
			}
			h := toHome(r.BalanceDue, r.Currency, r.BaseCurrency, r.ExchangeRate, home)
			total += h
			due := time.Date(r.DueDate.Year(), r.DueDate.Month(), r.DueDate.Day(), 0, 0, 0, 0, time.UTC)
			days := int(in.Today.Sub(due).Hours() / 24)
			list = append(list, inv{home: h, item: Item{Key: "inv:" + r.ID, Title: strings.TrimSpace(r.Number + " · " + nz(r.ClientName, "Client")),
				Detail: fmt.Sprintf("%s · due %s", plural(days, "day overdue", "days overdue"), due.Format("2 Jan")), Amount: r2(r.BalanceDue), Currency: r.Currency,
				Value: Money(r.BalanceDue, r.Currency), Link: "/invoices/" + r.ID, Tag: "Overdue", Tone: map[bool]string{true: "danger", false: "warn"}[days > 30]}})
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].home > list[j].home })
		items := []Item{}
		for _, x := range list {
			if len(items) >= 5 {
				break
			}
			items = append(items, x.item)
		}
		if len(list) > 0 {
			sec.Add("overdue", map[bool]string{true: "Other overdue invoices — ", false: "Overdue invoices — "}[len(subInvoices) > 0]+Money(total, home), items, len(list), "/invoices?status=overdue")
			sec.Say(25, "%s overdue (%s)", plural(len(list), "invoice", "invoices"), Money(total, home))
			sum = append(sum, Money(total, home)+" overdue")
		}
	}

	addSubs()
	sec.Summary = strings.Join(sum, " · ")
	return sec, nil
}

func phoneSuffix(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	return " — call " + strings.TrimSpace(p)
}

// toHome converts an invoice amount to the home currency at the invoice's
// locked rate (ExchangeRate = units of currency per 1 home unit).
func toHome(v float64, cur, base string, rate float64, home string) float64 {
	if cur == home || cur == "" {
		return v
	}
	if rate > 0 && (base == "" || base == home) {
		return v / rate
	}
	return v
}

// ---------------------------------------------------------------- projects

var highPriorities = map[string]bool{"highest": true, "high": true, "critical": true, "blocker": true, "urgent": true, "p1": true, "p0": true}

func projectsSection(ctx context.Context, in *Input) (*Section, error) {
	sec := &Section{Link: "/projects"}
	if !hasTable(in.DB, "pm_projects") {
		return sec, nil
	}
	if in.jiraNote != "" {
		sec.Notes = append(sec.Notes, in.jiraNote)
	}
	var projects []struct {
		ID, Key, Name, Source, ExternalURL string
	}
	in.DB.Raw(`SELECT id, COALESCE(key,'') AS key, name, COALESCE(source,'local') AS source, COALESCE(external_url,'') AS external_url
		FROM pm_projects WHERE org_id = ? AND status = 'active' ORDER BY name`, in.Org).Scan(&projects)
	if len(projects) == 0 {
		return sec, nil
	}
	ids := make([]string, len(projects))
	byID := map[string]int{}
	for i, p := range projects {
		ids[i] = p.ID
		byID[p.ID] = i
	}
	type task struct {
		ID, ProjectID, Key, Title, Status, StatusCategory, Priority, AssigneeName, Source, URL string
		DueDate                                                                                *time.Time
		CompletedAt                                                                            *time.Time
	}
	var tasks []task
	in.DB.Raw(`SELECT id, project_id, COALESCE(key,'') AS key, title, COALESCE(status,'') AS status, COALESCE(status_category,'todo') AS status_category,
		COALESCE(priority,'') AS priority, COALESCE(assignee_name,'') AS assignee_name, COALESCE(source,'local') AS source, COALESCE(url,'') AS url,
		due_date, completed_at FROM pm_tasks WHERE org_id = ? AND project_id IN ?`, in.Org, ids).Scan(&tasks)
	type stat struct{ total, done, prog, overdue, doneSince, due7 int }
	stats := make([]stat, len(projects))
	week := in.Today.AddDate(0, 0, 7)
	var overdue, due7, hot, done []Item
	nOver, nDue, nHot, nDone := 0, 0, 0, 0
	jiraOver, jiraDone := 0, 0
	taskLink := func(t task) string { return "/projects?project=" + t.ProjectID }
	assignee := func(t task) string {
		if t.AssigneeName != "" {
			return " · " + t.AssigneeName
		}
		return ""
	}
	title := func(t task) string { return strings.TrimSpace(strings.Join([]string{t.Key, t.Title}, " ")) }
	for _, t := range tasks {
		i := byID[t.ProjectID]
		st := &stats[i]
		st.total++
		isDone := t.StatusCategory == "done"
		if isDone {
			st.done++
			if t.CompletedAt != nil && t.CompletedAt.After(in.Since) {
				st.doneSince++
				nDone++
				if t.Source == "jira" {
					jiraDone++
				}
				if len(done) < 5 {
					done = append(done, Item{Key: "task:" + t.ID, Title: title(t), Detail: projects[i].Name + assignee(t), Link: taskLink(t), External: t.URL, Tag: "Done", Tone: "ok"})
				}
			}
			continue
		}
		if t.StatusCategory == "in_progress" {
			st.prog++
		}
		var due time.Time
		if t.DueDate != nil {
			due = time.Date(t.DueDate.Year(), t.DueDate.Month(), t.DueDate.Day(), 0, 0, 0, 0, time.UTC)
		}
		listedHere := false
		switch {
		case t.DueDate != nil && due.Before(in.Today):
			st.overdue++
			nOver++
			if t.Source == "jira" {
				jiraOver++
			}
			days := int(in.Today.Sub(due).Hours() / 24)
			if len(overdue) < 6 {
				overdue = append(overdue, Item{Key: "task:" + t.ID, Title: title(t), Detail: fmt.Sprintf("%s · due %s (%s)%s", projects[i].Name, due.Format("2 Jan"),
					plural(days, "day late", "days late"), assignee(t)), Link: taskLink(t), External: t.URL, Tag: "Overdue", Tone: "danger"})
			}
			listedHere = true
		case t.DueDate != nil && !due.After(week):
			st.due7++
			nDue++
			if len(due7) < 6 {
				w := "due " + due.Format("Mon 2 Jan")
				if due.Equal(in.Today) {
					w = "due today"
				}
				due7 = append(due7, Item{Key: "task:" + t.ID, Title: title(t), Detail: projects[i].Name + " · " + w + assignee(t), Link: taskLink(t), External: t.URL, Tag: nz(t.Status, "To do")})
			}
			listedHere = true
		}
		blocked := strings.Contains(strings.ToLower(t.Status), "block")
		if (highPriorities[strings.ToLower(strings.TrimSpace(t.Priority))] || blocked) && !listedHere {
			nHot++
			if len(hot) < 5 {
				tag := t.Priority
				if blocked {
					tag = "Blocked"
				}
				hot = append(hot, Item{Key: "task:" + t.ID, Title: title(t), Detail: projects[i].Name + " · " + nz(t.Status, "To do") + assignee(t), Link: taskLink(t), External: t.URL, Tag: tag, Tone: "warn"})
			}
		}
	}
	// Progress per active project (with tasks), busiest first.
	order := make([]int, 0, len(projects))
	for i := range projects {
		if stats[i].total > 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := stats[order[a]], stats[order[b]]
		if x.overdue != y.overdue {
			return x.overdue > y.overdue
		}
		return x.total-x.done > y.total-y.done
	})
	prog := []Item{}
	for _, i := range order {
		st := stats[i]
		if len(prog) >= 8 {
			break
		}
		pct := int(math.Floor(float64(st.done) / float64(st.total) * 100))
		parts := []string{fmt.Sprintf("%d of %d done", st.done, st.total)}
		if st.doneSince > 0 {
			parts = append(parts, fmt.Sprintf("%d since your last brief", st.doneSince))
		}
		if st.prog > 0 {
			parts = append(parts, fmt.Sprintf("%d in progress", st.prog))
		}
		if st.overdue > 0 {
			parts = append(parts, fmt.Sprintf("%d overdue", st.overdue))
		}
		if st.due7 > 0 {
			parts = append(parts, fmt.Sprintf("%d due in 7 days", st.due7))
		}
		p := projects[i]
		name := p.Name
		if p.Key != "" {
			name = p.Key + " — " + p.Name
		}
		pc := pct
		it := Item{Key: "proj:" + p.ID, Title: name, Detail: strings.Join(parts, " · "), Value: fmt.Sprintf("%d%%", pct), Progress: &pc, Link: "/projects?project=" + p.ID, External: p.ExternalURL}
		if p.Source == "jira" {
			it.Tag = "Jira"
		}
		if st.overdue > 0 {
			it.Tone = "warn"
		}
		prog = append(prog, it)
	}
	sec.Add("progress", "Active projects", prog, len(order), "/projects")
	sec.Add("overdue", "Overdue", overdue, nOver, "/projects")
	sec.Add("hot", "High priority or blocked", hot, nHot, "/projects")
	sec.Add("due", "Due in the next 7 days", due7, nDue, "/projects")
	sec.Add("done", "Done since your last brief", done, nDone, "/projects")
	if nOver > 0 {
		if jiraOver == nOver {
			sec.Say(30, "%s overdue", plural(nOver, "Jira issue", "Jira issues"))
		} else {
			sec.Say(30, "%s overdue", plural(nOver, "task", "tasks"))
		}
	}
	if nHot > 0 {
		sec.Say(33, "%s high priority or blocked", plural(nHot, "issue", "issues"))
	}
	if nDone > 0 && nOver == 0 {
		sec.Say(65, "%s done since your last brief", plural(nDone, "task", "tasks"))
	}
	sum := []string{plural(len(order), "active project", "active projects")}
	if nOver > 0 {
		sum = append(sum, fmt.Sprintf("%d overdue", nOver))
	}
	if nDue > 0 {
		sum = append(sum, fmt.Sprintf("%d due in 7 days", nDue))
	}
	if nDone > 0 {
		sum = append(sum, fmt.Sprintf("%d done since your last brief", nDone))
	}
	sec.Summary = strings.Join(sum, " · ")
	return sec, nil
}

// ---------------------------------------------------------------- opportunities

func opportunitiesSection(ctx context.Context, in *Input) (*Section, error) {
	sec := &Section{Link: "/insights?tab=opportunities"}
	home := in.Currency
	opps := in.svc.sources().ins.OpportunitiesFor(in.Org, in.Today)
	type opp struct {
		item  Item
		value float64
		isNew bool
	}
	list := []opp{}
	for _, o := range opps.Items {
		switch o.Type {
		case "collection", "risk": // overdue invoices are under Money in; risks aren't opportunities
			continue
		}
		if o.Value <= 0 {
			continue
		}
		key := "opp:" + o.Key
		val := Approx(o.Value, nz(opps.Currency, home))
		if o.ValueKind == "per_year" {
			val += "/yr"
		}
		det := o.Summary
		if len(o.Items) > 0 && o.Items[0].Name != "" {
			det = strings.TrimSpace(o.Items[0].Name + " — " + o.Items[0].Detail)
			if len(o.Items) > 1 {
				det += fmt.Sprintf(" (+%d more)", len(o.Items)-1)
			}
		}
		it := Item{Key: key, Title: o.Title, Detail: det, Amount: r2(o.Value), Currency: nz(opps.Currency, home), Value: val, Link: nz(o.Link, "/insights?tab=opportunities")}
		isNew := in.IsNew("opportunities", key)
		if isNew && in.Previous != nil {
			it.Tag = "New"
		}
		if o.Severity == "high" {
			it.Tone = "ok"
		}
		list = append(list, opp{item: it, value: o.Value, isNew: isNew})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].value > list[j].value })
	items := []Item{}
	var newVal, allVal float64
	nNew := 0
	for i, o := range list {
		allVal += o.value
		if o.isNew {
			nNew++
			newVal += o.value
		}
		if i < 5 {
			items = append(items, o.item)
		}
	}
	sec.Add("top", "Worth going for", items, len(list), "/insights?tab=opportunities")
	if nNew > 0 {
		sec.Say(40, "%s worth %s", plural(nNew, "new opportunity", "new opportunities"), Approx(newVal, home))
	} else if len(list) > 0 {
		sec.Say(75, "%s worth %s", plural(len(list), "opportunity", "opportunities"), Approx(allVal, home))
	}

	if hasTable(in.DB, "insights_watch_items") {
		var rows []struct {
			ID, Title, Source, Link string
			Score                   float64
		}
		in.DB.Raw(`SELECT id, title, COALESCE(source,'') AS source, COALESCE(link,'') AS link, COALESCE(score,0) AS score FROM insights_watch_items
			WHERE organization_id = ? AND NOT dismissed AND first_seen_at > ? ORDER BY score DESC, first_seen_at DESC LIMIT 20`, in.Org, in.Since).Scan(&rows)
		news := []Item{}
		for _, r := range rows {
			if len(news) >= 3 {
				break
			}
			news = append(news, Item{Key: "watch:" + r.ID, Title: r.Title, Detail: r.Source, Link: "/insights?tab=watch", External: r.Link})
		}
		sec.Add("watch", "New in your industry watch", news, len(rows), "/insights?tab=watch")
		if len(rows) > 0 {
			sec.Say(85, "%s", plural(len(rows), "industry headline", "industry headlines"))
		}
	}
	sum := []string{}
	if len(list) > 0 {
		sum = append(sum, fmt.Sprintf("%s worth %s", plural(len(list), "opportunity", "opportunities"), Approx(allVal, home)))
	}
	if nNew > 0 && in.Previous != nil {
		sum = append(sum, fmt.Sprintf("%d new", nNew))
	}
	sec.Summary = strings.Join(sum, " · ")
	return sec, nil
}

// ---------------------------------------------------------------- today

func todaySection(ctx context.Context, in *Input) (*Section, error) {
	sec := &Section{}
	v := in.Viewer
	start := time.Date(in.Today.Year(), in.Today.Month(), in.Today.Day(), 0, 0, 0, 0, in.Loc)
	end := start.AddDate(0, 0, 1)
	sum := []string{}

	if v.Can("bookings") && hasTable(in.DB, "bookings") {
		own := !v.Managing || v.Scope == "own"
		q := `SELECT b.id, b.start_time, b.end_time, COALESCE(b.status,'') AS status,
			TRIM(COALESCE(c.first_name,'') || ' ' || COALESCE(c.last_name,'')) AS customer, COALESCE(c.company_name,'') AS company,
			COALESCE((SELECT string_agg(s.name, ', ') FROM booking_services bs JOIN services s ON s.id = bs.service_id WHERE bs.booking_id = b.id), '') AS services,
			TRIM(COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,'')) AS staff
			FROM bookings b LEFT JOIN customers c ON c.id = b.customer_id LEFT JOIN users u ON u.id = b.staff_id
			WHERE b.organization_id = ? AND b.deleted_at IS NULL AND b.start_time >= ? AND b.start_time < ? AND COALESCE(b.status,'') NOT IN ('cancelled','no_show')`
		args := []any{in.Org, start, end}
		if own {
			q += ` AND b.staff_id = ?`
			args = append(args, v.UserID)
		}
		var rows []struct {
			ID, Status, Customer, Company, Services, Staff string
			StartTime, EndTime                             time.Time
		}
		in.DB.Raw(q+` ORDER BY b.start_time LIMIT 100`, args...).Scan(&rows)
		items := []Item{}
		for _, b := range rows {
			if len(items) >= 8 {
				break
			}
			who := nz(strings.TrimSpace(b.Customer), b.Company)
			det := []string{}
			if b.Services != "" {
				det = append(det, b.Services)
			}
			if !own && b.Staff != "" {
				det = append(det, "with "+b.Staff)
			}
			items = append(items, Item{Key: "bk:" + b.ID, Title: b.StartTime.In(in.Loc).Format("15:04") + " · " + nz(who, "Booking"), Detail: strings.Join(det, " · "),
				Link: "/bookings?booking=" + b.ID})
		}
		title := "Bookings"
		if own {
			title = "Your bookings"
		}
		sec.Add("bookings", title, items, len(rows), "/bookings")
		if len(rows) > 0 {
			sec.Say(50, "%s today", plural(len(rows), "booking", "bookings"))
			sum = append(sum, plural(len(rows), "booking", "bookings"))
		}
	}

	if v.Can("communication") && hasTable(in.DB, "meetings") {
		var rows []struct {
			ID, Title, LocationType string
			StartAt, EndAt          time.Time
		}
		in.DB.Raw(`SELECT m.id, m.title, COALESCE(m.location_type,'') AS location_type, m.start_at, m.end_at FROM meetings m
			WHERE m.org_id = ? AND m.start_at >= ? AND m.start_at < ? AND COALESCE(m.status,'') <> 'cancelled' AND m.cancelled_at IS NULL
			AND (m.organizer_id = ? OR LOWER(COALESCE(m.organizer_email,'')) = LOWER(?) OR EXISTS (SELECT 1 FROM meeting_participants p WHERE p.meeting_id = m.id
				AND (p.user_id = ? OR (? <> '' AND LOWER(COALESCE(p.email,'')) = LOWER(?)))))
			ORDER BY m.start_at LIMIT 50`, in.Org, start, end, v.UserID, v.Email, v.UserID, v.Email, v.Email).Scan(&rows)
		items := []Item{}
		for _, m := range rows {
			if len(items) >= 6 {
				break
			}
			det := m.StartAt.In(in.Loc).Format("15:04") + "–" + m.EndAt.In(in.Loc).Format("15:04")
			if m.LocationType != "" {
				det += " · " + strings.ReplaceAll(m.LocationType, "_", " ")
			}
			items = append(items, Item{Key: "mt:" + m.ID, Title: m.Title, Detail: det, Link: "/meetings"})
		}
		sec.Add("meetings", "Meetings", items, len(rows), "/meetings")
		if len(rows) > 0 {
			sec.Say(48, "%s today", plural(len(rows), "meeting", "meetings"))
			sum = append(sum, plural(len(rows), "meeting", "meetings"))
		}
	}

	if v.Managing && v.Can("smallbiz") && hasTable(in.DB, "smallbiz_reminders") {
		var rows []struct {
			ID, Title, Currency string
			DueDate             time.Time
			Amount              int64
		}
		in.DB.Raw(`SELECT id, title, COALESCE(currency,'') AS currency, due_date, COALESCE(amount,0) AS amount FROM smallbiz_reminders
			WHERE organization_id = ? AND NOT done AND due_date::date <= ?::date ORDER BY due_date LIMIT 50`, in.Org, dayStr(in.Today)).Scan(&rows)
		items := []Item{}
		for _, r := range rows {
			if len(items) >= 6 {
				break
			}
			due := time.Date(r.DueDate.Year(), r.DueDate.Month(), r.DueDate.Day(), 0, 0, 0, 0, time.UTC)
			days := int(in.Today.Sub(due).Hours() / 24)
			it := Item{Key: "rem:" + r.ID, Title: r.Title, Detail: "due today", Link: "/business?tab=reminders", Tone: "warn"}
			if days > 0 {
				it.Detail, it.Tone, it.Tag = plural(days, "day overdue", "days overdue"), "danger", "Overdue"
			}
			if r.Amount != 0 {
				cur := nz(r.Currency, in.Currency)
				it.Amount, it.Currency, it.Value = float64(r.Amount)/100, cur, Money(float64(r.Amount)/100, cur)
			}
			items = append(items, it)
		}
		sec.Add("reminders", "Reminders due", items, len(rows), "/business?tab=reminders")
		if len(rows) > 0 {
			sec.Say(52, "%s due", plural(len(rows), "reminder", "reminders"))
			sum = append(sum, plural(len(rows), "reminder", "reminders")+" due")
		}
	}
	sec.Summary = strings.Join(sum, " · ")
	return sec, nil
}

// ---------------------------------------------------------------- helpers

func nz(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return b
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }
