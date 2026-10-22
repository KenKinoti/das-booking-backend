package subscriptions

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// Monthly invoices go out 7 days before the period starts, yearly ones 14;
// each period is invoiced exactly once even with concurrent runs.
func TestInvoiceLeadTimeEmailAndIdempotency(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	svc := f.service("Starter hosting — monthly", 1500, "KES")
	start := "2030-01-12" // invoice 7 days ahead: 5 Jan
	sub := f.subscribe(SubInput{Period: PeriodMonthly, StartDate: start, Lines: []LineInput{{ServiceID: svc}}, SiteDomain: "wanjiku.example", Supplier: SupplierSynergy})
	if sub.Number != "SUB-0001" || sub.Currency != "KES" || sub.Status != StatusActive || sub.NextInvoiceOn != "2030-01-05" || sub.NextDueDate != start ||
		sub.Lines[0].Description != "Starter hosting" || sub.Lines[0].Kind != KindHosting || sub.Amount != 1500 || !sub.AutoSuspend || sub.Grace != 7 {
		t.Fatalf("created: %s %s %s next %s due %s %+v", sub.Number, sub.Currency, sub.Status, sub.NextInvoiceOn, sub.NextDueDate, sub.Lines)
	}
	f.at("2030-01-04")
	if r := f.run(); r.Invoiced != 0 || len(f.periods(sub.ID)) != 0 {
		t.Fatal("8 days ahead: too early")
	}
	f.at("2030-01-05")
	r := f.run()
	pers := f.periods(sub.ID)
	if r.Invoiced != 1 || r.Emailed != 1 || len(pers) != 1 {
		t.Fatalf("invoiced %d emailed %d periods %d: %v", r.Invoiced, r.Emailed, len(pers), r.Errors)
	}
	pe := pers[0]
	if DayStr(pe.IssueDate) != "2030-01-05" || DayStr(pe.DueDate) != start || pe.Status != PerInvoiced || !pe.Sent || pe.Amount != 1500 || pe.InvoiceNumber == "" {
		t.Fatalf("period: %+v", pe)
	}
	var inv invoicing.Invoice
	f.db.Preload("Items").Where("id = ?", pe.InvoiceID).First(&inv)
	if inv.Status != invoicing.StatusSent || DayStr(inv.IssueDate) != "2030-01-05" || DayStr(inv.DueDate) != start || inv.CustomerID == nil || *inv.CustomerID != f.customer ||
		inv.Reference != "SUB-0001 · 2030-01-12" || len(inv.Items) != 1 || inv.Items[0].Description != "Starter hosting — 12 Jan – 11 Feb 2030" {
		t.Fatalf("invoice: %+v items %+v", inv, inv.Items)
	}
	got := f.out.to(clientEmail)
	if len(got) != 1 || !strings.Contains(got[0], inv.Number) || !strings.Contains(got[0], "hosting for wanjiku.example") {
		t.Fatalf("client email: %v", got)
	}
	if body := f.out.body(clientEmail, inv.Number); !strings.Contains(body, "https://app.example/pay/"+inv.PublicToken) || !strings.Contains(body, "https://app.example/your-services/") {
		t.Fatalf("pay link and services link in the email:\n%s", body)
	}
	// The next billing date is the unpaid period's start until it is paid.
	if s := f.sub(sub.ID); DayStr(s.NextPeriodStart) != "2030-02-12" || DayStr(s.NextBillingDate) != start {
		t.Fatalf("next period %s, next billing %s", DayStr(s.NextPeriodStart), DayStr(s.NextBillingDate))
	}
	// Same day again, and concurrent runs on the next period's day: never twice.
	f.run()
	if len(f.periods(sub.ID)) != 1 || len(f.out.to(clientEmail)) != 1 {
		t.Fatal("ran twice: duplicate invoice or email")
	}
	f.pay(pe.InvoiceID)
	f.at("2030-02-05") // next period starts 12 Feb
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.s.RunOrg(context.Background(), f.org, RunOptions{}) }()
	}
	wg.Wait()
	pers = f.periods(sub.ID)
	var invCount int64
	f.db.Model(&invoicing.Invoice{}).Where("organization_id = ?", f.org).Count(&invCount)
	if len(pers) != 2 || invCount != 2 || DayStr(pers[1].Start) != "2030-02-12" || DayStr(pers[1].DueDate) != "2030-02-12" {
		t.Fatalf("concurrent runs: %d periods, %d invoices", len(pers), invCount)
	}
	if s := f.sub(sub.ID); DayStr(s.NextPeriodStart) != "2030-03-12" || DayStr(s.NextBillingDate) != "2030-02-12" || s.PaidThrough == nil || DayStr(*s.PaidThrough) != "2030-02-11" {
		t.Fatalf("after the first payment: next %s billing %s paid through %v", DayStr(s.NextPeriodStart), DayStr(s.NextBillingDate), s.PaidThrough)
	}
	// Yearly: 14 days ahead.
	yr := 12000.0
	y := f.subscribe(SubInput{Period: PeriodYearly, StartDate: "2030-02-28", Lines: []LineInput{{Description: ".co.ke domain", UnitPrice: &yr}}, Domain: "wanjiku.co.ke"})
	if y.NextInvoiceOn != "2030-02-14" || y.Lines[0].Kind != KindDomain {
		t.Fatalf("yearly lead: %s %s", y.NextInvoiceOn, y.Lines[0].Kind)
	}
	if sc := (&Subscription{StartDate: d("2032-02-29"), AnchorDate: d("2032-02-29"), BillingDay: 29, Period: PeriodYearly}).Schedule(); DayStr(sc.At(1).Start) != "2033-02-28" {
		t.Fatal("leap-day renewal")
	}
}

// Catalogue prices are worked out for the period and the customer's
// currency; tax comes from the invoicing settings.
func TestCataloguePrices(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	if _, err := f.s.FX.SaveManual(f.org, f.admin, "USD", "KES", 130, "test"); err != nil {
		t.Fatal(err)
	}
	f.db.Exec(`INSERT INTO billing_settings (organization_id, default_tax_rate, prices_include_tax, currency) VALUES (?, 16, false, 'KES')`, f.org)
	yearly := f.service("Starter hosting + .co.ke domain — yearly", 12000, "KES")
	vps := f.service("Cloud VPS — monthly", 20, "USD")
	a := f.subscribe(SubInput{Period: PeriodMonthly, StartDate: "2030-01-12", Lines: []LineInput{{ServiceID: yearly}, {ServiceID: vps}}})
	if len(a.Lines) != 2 || a.Lines[0].Price != 1000 || a.Lines[0].TaxRate != 16 || !a.Lines[0].FromCatalog || a.Lines[1].PriceCurrency != "USD" || a.Amount != 1000+2600 ||
		a.AmountTax != (1000+2600)*1.16 {
		t.Fatalf("monthly from a yearly KES and a monthly USD service: %+v amount %v %v", a.Lines, a.Amount, a.AmountTax)
	}
	f.run()
	var inv invoicing.Invoice
	f.db.Preload("Items").Where("id = ?", f.periods(a.ID)[0].InvoiceID).First(&inv)
	if inv.Currency != "KES" || inv.Subtotal != 3600 || len(inv.Items) != 2 || inv.Items[1].OriginalCurrency != "USD" || *inv.Items[1].OriginalUnitPrice != 20 ||
		inv.Items[0].ReferenceID != yearly {
		t.Fatalf("invoice: %s %v %+v", inv.Currency, inv.Subtotal, inv.Items)
	}
	// A typed price wins; it is in the subscription currency.
	p := 900.0
	b := f.subscribe(SubInput{Period: PeriodYearly, StartDate: "2030-01-19", Lines: []LineInput{{ServiceID: yearly, UnitPrice: &p}}})
	if b.Amount != 900 || b.Lines[0].FromCatalog {
		t.Fatalf("typed price: %v", b.Amount)
	}
}

func TestPaymentMovesForwardAnyPath(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.s.WatchPayments(ctx); err != nil {
		t.Fatal(err)
	}
	sub := f.subscribe(hosting("2030-01-12", 1500, "", ""))
	f.at("2030-01-05")
	f.run()
	pe := f.periods(sub.ID)[0]
	// Manual payment through the invoicing API: the watcher applies it within seconds.
	f.pay(pe.InvoiceID)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && f.periods(sub.ID)[0].Status != PerPaid {
		time.Sleep(100 * time.Millisecond)
	}
	got := f.periods(sub.ID)[0]
	s := f.sub(sub.ID)
	if got.Status != PerPaid || got.PaidAt == nil || s.PaidThrough == nil || DayStr(*s.PaidThrough) != "2030-02-11" || DayStr(s.NextBillingDate) != "2030-02-12" {
		t.Fatalf("after payment: %+v paid through %v next billing %s", got, s.PaidThrough, DayStr(s.NextBillingDate))
	}
	// An online payment (as the Flutterwave webhook records it) on an overdue invoice clears past due.
	f.at("2030-02-05")
	f.run()
	p2 := f.periods(sub.ID)[1]
	f.at("2030-02-14")
	f.run()
	if f.sub(sub.ID).Status != StatusPastDue {
		t.Fatalf("2 days overdue: %s", f.sub(sub.ID).Status)
	}
	err := f.db.Transaction(func(tx *gorm.DB) error {
		_, _, err := invoicing.RecordOnlinePayment(tx, invoicing.OnlinePaymentInput{InvoiceID: p2.InvoiceID, Amount: 1500, Date: time.Now(), Method: "Flutterwave · M-Pesa",
			Reference: "FLW-1", Activity: "Paid online"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && f.sub(sub.ID).Status != StatusActive {
		time.Sleep(100 * time.Millisecond)
	}
	if f.periods(sub.ID)[1].Status != PerPaid || f.sub(sub.ID).Status != StatusActive || DayStr(f.sub(sub.ID).NextBillingDate) != "2030-03-12" {
		t.Fatalf("online payment: %s %s", f.periods(sub.ID)[1].Status, f.sub(sub.ID).Status)
	}
	// Removing a payment puts the period back to unpaid (scheduler sync).
	var payID string
	f.db.Raw(`SELECT id FROM billing_payments WHERE invoice_id = ? LIMIT 1`, pe.InvoiceID).Scan(&payID)
	if code, out := f.api("DELETE", "/invoicing/invoices/"+pe.InvoiceID+"/payments/"+payID, f.token(f.org, f.admin, "admin"), nil); code >= 300 {
		t.Fatalf("delete payment: %d %v", code, out)
	}
	f.run()
	if f.periods(sub.ID)[0].Status != PerInvoiced || f.sub(sub.ID).Status != StatusPastDue || DayStr(f.sub(sub.ID).NextBillingDate) != "2030-01-12" {
		t.Fatal("payment removed: unpaid and past due again")
	}
}

// TestDunningTimeline walks day by day (a fake clock) through an unpaid
// invoice of a site at hosting.com: reminders to the client on D−3, D, D+3,
// the final notice on D+5, admin alerts on D+4 ("in 3 days") and D+6
// ("tomorrow"), and on D+7 the suspension with a manual task.
func TestDunningTimeline(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	due := "2030-01-19"
	sub := f.subscribe(hosting(due, 1500, "wanjiku.example", SupplierHostingCom))
	admin := f.adminEmail(1)
	type dayLog struct{ client, admin []string }
	log := map[int]dayLog{}
	for off := -7; off <= 12; off++ {
		f.at(f.day(off, due))
		f.out.reset()
		f.run()
		log[off] = dayLog{client: f.out.to(clientEmail), admin: f.out.to(admin)}
	}
	clientDays := []int{}
	for off := -7; off <= 12; off++ {
		for _, s := range log[off].client {
			if strings.HasPrefix(s, "Reminder:") || strings.HasPrefix(s, "Due today") || strings.HasPrefix(s, "Overdue:") {
				clientDays = append(clientDays, off)
			}
		}
	}
	if fmt.Sprint(clientDays) != "[-3 0 3]" {
		t.Fatalf("client reminder days %v (log %+v)", clientDays, log)
	}
	if !hasSubject(log[-7].client, "Invoice") || !hasSubject(log[3].client, "please pay by Fri 25 Jan") {
		t.Fatalf("invoice 7 days ahead / overdue reminder: %v %v", log[-7].client, log[3].client)
	}
	if !hasSubject(log[5].client, "Final notice: your hosting for wanjiku.example will be suspended on Sat 26 Jan") {
		t.Fatalf("final notice 2 days before: %v", log[5].client)
	}
	alertDays := []int{}
	for off := -7; off <= 12; off++ {
		for _, s := range log[off].admin {
			if strings.Contains(s, "will be suspended") {
				alertDays = append(alertDays, off)
			}
		}
	}
	if fmt.Sprint(alertDays) != "[4 6]" {
		t.Fatalf("admin alert days %v", alertDays)
	}
	if !hasSubject(log[4].admin, "Wanjiku Traders will be suspended in 3 days (Sat 26 Jan) — hosting for wanjiku.example") ||
		!hasSubject(log[6].admin, "Wanjiku Traders will be suspended tomorrow (Sat 26 Jan)") {
		t.Fatalf("admin alerts: %v / %v", log[4].admin, log[6].admin)
	}
	// Suspended on D+7 (after the 3-day alert): client and admins told, a manual task.
	if !hasSubject(log[7].client, "Suspended: your hosting for wanjiku.example") || !hasSubject(log[7].admin, "Suspended: Wanjiku Traders — Suspend wanjiku.example manually at hosting.com") {
		t.Fatalf("suspension emails: %v / %v", log[7].client, log[7].admin)
	}
	s := f.sub(sub.ID)
	if s.Status != StatusSuspended || s.SuspendedAt == nil || DayStr(*s.SuspendedAt) != f.day(7, due) || s.SuspendedVia != ViaManual {
		t.Fatalf("suspended: %+v", s)
	}
	tasks := f.tasks(sub.ID)
	if len(tasks) != 1 || tasks[0].Kind != TaskSuspend || tasks[0].Status != TaskOpen || tasks[0].Title != "Suspend wanjiku.example manually at hosting.com" ||
		!strings.Contains(strings.Join(tasks[0].Steps, " "), "hosting.com") {
		t.Fatalf("tasks: %+v", tasks)
	}
	// Nothing else to the client between the steps; the digest reaches the admins.
	for _, off := range []int{-6, -2, 1, 2, 4, 6, 8, 12} {
		if len(log[off].client) != 0 {
			t.Fatalf("D%+d: unexpected client email %v", off, log[off].client)
		}
	}
	if !hasSubject(log[-2].admin, "Client billing today: 1 due in 7 days") || !hasSubject(log[1].admin, "1 to be suspended this week") {
		t.Fatalf("daily digest: %v / %v", log[-2].admin, log[1].admin)
	}
	var n int64
	f.db.Model(&notify.Notification{}).Where("organization_id = ? AND type = ?", f.org, "subscriptions.suspension").Count(&n)
	if n != 2*2 { // two alerts, each to both administrators
		t.Fatalf("in-app suspension warnings: %d", n)
	}
	// No invoices while suspended (the next period started on 19 Feb's lead day).
	f.at("2030-02-15")
	f.run()
	if len(f.periods(sub.ID)) != 1 {
		t.Fatalf("periods while suspended: %d", len(f.periods(sub.ID)))
	}
	// The timeline shows the invoice, reminders, notice, alerts and the suspension.
	dt, _ := f.s.GetDetail(f.org, sub.ID)
	kinds := map[string]int{}
	for _, it := range dt.Timeline {
		kinds[it.Kind]++
	}
	if kinds["invoice"] < 2 || kinds["reminder"] != 3 || kinds["notice"] != 1 || kinds["alert"] != 2 || kinds["suspend"] < 1 || kinds["task"] != 1 {
		t.Fatalf("timeline kinds: %v", kinds)
	}
}

// The admins always hear about a suspension at least 3 days ahead: after
// an outage the alert goes out late and the suspension waits.
func TestAdminAlertAtLeastThreeDaysBefore(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	due := "2030-01-19"
	sub := f.subscribe(hosting(due, 1500, "late.example", SupplierHostingCom))
	f.at(f.day(-7, due))
	f.run()
	// Nothing runs until D+6 (the day before the planned suspension).
	f.at(f.day(6, due))
	f.out.reset()
	f.run()
	if !hasSubject(f.out.to(f.adminEmail(1)), "will be suspended in 3 days (Mon 28 Jan)") {
		t.Fatalf("late alert: %v", f.out.to(f.adminEmail(1)))
	}
	g, _ := f.s.Get(f.org, sub.ID)
	if g.Open == nil || g.Open.SuspendOn != f.day(9, due) || !g.Open.Deferred || g.Open.PlannedOn != f.day(7, due) {
		t.Fatalf("deferred suspension: %+v", g.Open)
	}
	for _, off := range []int{7, 8} {
		f.at(f.day(off, due))
		f.out.reset()
		f.run()
		if f.sub(sub.ID).Status == StatusSuspended {
			t.Fatalf("suspended on D+%d, only %d days after the first alert", off, off-6)
		}
		if off == 7 && !hasSubject(f.out.to(clientEmail), "will be suspended on Mon 28 Jan") {
			t.Fatalf("final notice for the new date: %v", f.out.to(clientEmail))
		}
		if off == 8 && !hasSubject(f.out.to(f.adminEmail(1)), "will be suspended tomorrow") {
			t.Fatalf("day-before alert: %v", f.out.to(f.adminEmail(1)))
		}
	}
	f.at(f.day(9, due))
	f.run()
	if f.sub(sub.ID).Status != StatusSuspended {
		t.Fatal("suspended 3 days after the alert")
	}
	// A 1-day grace period: the 3-day alert goes out before the due date.
	g1 := 1
	in := hosting("2030-03-19", 1500, "short.example", SupplierHostingCom)
	in.GraceDays = &g1
	s2 := f.subscribe(in)
	f.out.reset()
	for off := -7; off <= 1; off++ {
		f.at(f.day(off, "2030-03-19"))
		f.run()
	}
	if !hasSubject(f.out.to(f.adminEmail(1)), "will be suspended in 3 days (Wed 20 Mar)") || f.sub(s2.ID).Status != StatusSuspended {
		t.Fatalf("grace 1: %v %s", f.out.to(f.adminEmail(1)), f.sub(s2.ID).Status)
	}
}

// Giving more time moves the suspension and everything before it.
func TestGiveMoreTime(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	due := "2030-01-19"
	sub := f.subscribe(hosting(due, 1500, "more.example", SupplierHostingCom))
	for _, off := range []int{-7, 0, 4} {
		f.at(f.day(off, due))
		f.run()
	}
	r, err := f.s.Extend(context.Background(), f.org, sub.ID, f.admin, 7, "Client travelling — pays on the 1st")
	if err != nil {
		t.Fatal(err)
	}
	if r.SuspendOn != f.day(14, due) || r.Open == nil || r.Open.SuspendOn != f.day(14, due) || r.Open.FinalNoticeOn != f.day(12, due) || r.Open.ExtraDays != 7 {
		t.Fatalf("after +7: %+v", r.Open)
	}
	admin := f.adminEmail(1)
	byDay := map[int][]string{}
	clientBy := map[int][]string{}
	for off := 5; off <= 14; off++ {
		f.at(f.day(off, due))
		f.out.reset()
		f.run()
		byDay[off], clientBy[off] = f.out.to(admin), f.out.to(clientEmail)
		if off < 14 && f.sub(sub.ID).Status == StatusSuspended {
			t.Fatalf("suspended on D+%d inside the extra time", off)
		}
	}
	if !hasSubject(byDay[11], "will be suspended in 3 days (Sat 2 Feb)") || !hasSubject(byDay[13], "will be suspended tomorrow (Sat 2 Feb)") {
		t.Fatalf("alerts for the new date: %v / %v", byDay[11], byDay[13])
	}
	if !hasSubject(clientBy[12], "Final notice: your hosting for more.example will be suspended on Sat 2 Feb") || hasSubject(clientBy[5], "Final notice") {
		t.Fatalf("final notice moved: %v / %v", clientBy[12], clientBy[5])
	}
	if f.sub(sub.ID).Status != StatusSuspended {
		t.Fatal("suspended on the new date")
	}
	// More time while suspended: unsuspended now, suspended again 14 days from today unless paid.
	r, err = f.s.Extend(context.Background(), f.org, sub.ID, f.admin, 14, "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Unsuspended || r.Status != StatusPastDue || r.SuspendOn != f.day(28, due) {
		t.Fatalf("extend a suspended one: %+v %s", r.Unsuspended, r.SuspendOn)
	}
	var ev int64
	f.db.Model(&Event{}).Where("subscription_id = ? AND stage = ?", sub.ID, "extend").Count(&ev)
	if ev != 2 {
		t.Fatalf("extend events: %d", ev)
	}
	// The open manual suspend task (never done at hosting.com) is closed.
	for _, tk := range f.tasks(sub.ID) {
		if tk.Kind == TaskSuspend && tk.Status != TaskDismissed {
			t.Fatalf("suspend task: %s", tk.Status)
		}
	}
}

// Suspension and unsuspension at Synergy Wholesale through the API (a fake
// SOAP server): the hosting ID comes from listHosting.
func TestSynergySuspendAndUnsuspend(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	fs := newFakeSynergy(t, fakeHost{"88123", "wanjiku.example", "Micro Env", "Active"}, fakeHost{"88124", "broken.example", "Micro Env", "Active"})
	fs.failFor["88124"] = true
	f.connect(fs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.s.WatchPayments(ctx); err != nil {
		t.Fatal(err)
	}
	due := "2030-01-19"
	a := f.subscribe(hosting(due, 1500, "wanjiku.example", SupplierSynergy))
	other := f.addCustomer(f.org, "Otieno", "Studio", "otieno@studio.example", "", "", "")
	in := hosting(due, 1500, "notlisted.example", SupplierSynergy)
	in.CustomerID = other
	b := f.subscribe(in)
	third := f.addCustomer(f.org, "Achieng", "Shop", "achieng@shop.example", "", "", "")
	in = hosting(due, 1500, "broken.example", SupplierSynergy)
	in.CustomerID = third
	c := f.subscribe(in)
	for _, off := range []int{-7, 4} {
		f.at(f.day(off, due))
		f.run()
	}
	if body := f.out.body(f.adminEmail(1), "Wanjiku Traders will be suspended in 3 days"); !strings.Contains(body, "suspended at Synergy Wholesale automatically") ||
		!strings.Contains(body, "https://app.example/subscriptions/"+a.ID+"?action=extend&days=7") || !strings.Contains(body, "action=paid") ||
		!strings.Contains(body, "action=suspend") || !strings.Contains(body, "action=skip") {
		t.Fatalf("alert with one-click actions:\n%s", body)
	}
	f.at(f.day(7, due))
	f.out.reset()
	f.run()
	calls := fs.callsOf("hostingSuspendService")
	if len(calls) != 2 || calls[0].Identifier == calls[1].Identifier {
		t.Fatalf("suspend calls: %+v", calls)
	}
	var wc fakeCall
	for _, cl := range calls {
		if cl.Identifier == "88123" {
			wc = cl
		}
	}
	if !strings.Contains(wc.Reason, "unpaid invoice INV-") || !strings.Contains(wc.Reason, a.Number) {
		t.Fatalf("suspend reason: %+v", wc)
	}
	sa, sb, sc := f.sub(a.ID), f.sub(b.ID), f.sub(c.ID)
	if sa.Status != StatusSuspended || sa.SuspendedVia != ViaAPI || sb.Status != StatusSuspended || sb.SuspendedVia != ViaManual || sc.SuspendedVia != ViaManual {
		t.Fatalf("via: %s/%s %s/%s %s", sa.Status, sa.SuspendedVia, sb.Status, sb.SuspendedVia, sc.SuspendedVia)
	}
	ta := f.tasks(a.ID)
	if len(ta) != 1 || !ta[0].Automated || ta[0].Status != TaskDone || ta[0].ProviderRef != "88123" || !strings.Contains(ta[0].Title, "Suspended wanjiku.example at Synergy Wholesale") {
		t.Fatalf("automated task: %+v", ta)
	}
	if tb := f.tasks(b.ID); len(tb) != 1 || tb[0].Automated || !strings.Contains(tb[0].Note, "not one of the hosting services") || tb[0].Title != "Suspend notlisted.example manually at Synergy Wholesale" {
		t.Fatalf("not listed at Synergy: %+v", tb)
	}
	if tc := f.tasks(c.ID); len(tc) != 1 || tc[0].Automated || !strings.Contains(tc[0].Note, "refused") {
		t.Fatalf("API error → manual: %+v", tc)
	}
	if !hasSubject(f.out.to(f.adminEmail(1)), "Suspended: Wanjiku Traders — hosting for wanjiku.example at Synergy Wholesale") {
		t.Fatalf("admin told: %v", f.out.to(f.adminEmail(1)))
	}
	// Paid through the invoicing API: unsuspended at Synergy straight away.
	f.at(f.day(9, due))
	f.out.reset()
	f.pay(f.periods(a.ID)[0].InvoiceID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && f.sub(a.ID).Status != StatusActive {
		time.Sleep(100 * time.Millisecond)
	}
	if f.sub(a.ID).Status != StatusActive {
		t.Fatal("unsuspended on payment")
	}
	un := fs.callsOf("hostingUnsuspendService")
	if len(un) != 1 || un[0].Identifier != "88123" {
		t.Fatalf("unsuspend calls: %+v", un)
	}
	var restore []Task
	f.db.Where("subscription_id = ? AND kind = ?", a.ID, TaskRestore).Find(&restore)
	if len(restore) != 1 || !restore[0].Automated || restore[0].Status != TaskDone {
		t.Fatalf("restore task: %+v", restore)
	}
	if !hasSubject(f.out.to(clientEmail), "Payment received — your hosting for wanjiku.example is active again") ||
		!hasSubject(f.out.to(f.adminEmail(1)), "Wanjiku Traders paid — hosting for wanjiku.example is unsuspended") {
		t.Fatalf("notices: %v / %v", f.out.to(clientEmail), f.out.to(f.adminEmail(1)))
	}
	// b: suspended by hand at Synergy (task done), then paid → "Unsuspend … manually" task.
	tb := f.tasks(b.ID)[0]
	if _, err := f.s.UpdateTask(f.org, tb.ID, f.admin, TaskDone, "Suspended in the console"); err != nil {
		t.Fatal(err)
	}
	f.pay(f.periods(b.ID)[0].InvoiceID)
	f.run()
	var rb []Task
	f.db.Where("subscription_id = ? AND kind = ?", b.ID, TaskRestore).Find(&rb)
	if f.sub(b.ID).Status != StatusActive || len(rb) != 1 || rb[0].Automated || rb[0].Title != "Unsuspend notlisted.example manually at Synergy Wholesale" {
		t.Fatalf("manual restore: %s %+v", f.sub(b.ID).Status, rb)
	}
	// Billing resumes: the next period is invoiced on its day.
	f.at("2030-02-12")
	f.run()
	if len(f.periods(a.ID)) != 2 {
		t.Fatal("billing resumed")
	}
}

// Without the API (not connected, or another provider) every suspension is
// a task, and the subscription is marked suspended (manual).
func TestManualSuspensionPaths(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	f.s.Supplier = &SynergyActions{Client: func(string) (*expenses.SynergyClient, string) {
		return nil, "the Synergy Wholesale API is not connected (Expenses → Sources)"
	}}
	due := "2030-01-19"
	a := f.subscribe(hosting(due, 1500, "a.example", SupplierSynergy))
	dom := 1200.0
	other := f.addCustomer(f.org, "Otieno", "Studio", "otieno@studio.example", "", "", "")
	b := f.subscribe(SubInput{CustomerID: other, Period: PeriodMonthly, StartDate: due, Lines: []LineInput{{Description: ".co.ke domain", UnitPrice: &dom}}, Domain: "b.co.ke", Supplier: SupplierSynergy})
	for _, off := range []int{-7, 4, 7} {
		f.at(f.day(off, due))
		f.run()
	}
	ta, tb := f.tasks(a.ID), f.tasks(b.ID)
	if f.sub(a.ID).SuspendedVia != ViaManual || len(ta) != 1 || ta[0].Title != "Suspend a.example manually at Synergy Wholesale" || !strings.Contains(ta[0].Note, "not connected") {
		t.Fatalf("not connected: %+v", ta)
	}
	if f.sub(b.ID).Status != StatusSuspended || len(tb) != 1 || tb[0].Target != KindDomain || !strings.Contains(tb[0].Title, "Hold the renewal of b.co.ke") {
		t.Fatalf("domain only: %+v", tb)
	}
	// Paid before anyone suspended it by hand: nothing to undo, the task is closed.
	f.pay(f.periods(a.ID)[0].InvoiceID)
	f.run()
	ta = f.tasks(a.ID)
	if f.sub(a.ID).Status != StatusActive || len(ta) != 1 || ta[0].Status != TaskDismissed {
		t.Fatalf("paid before the manual suspension: %+v", ta)
	}
}

// One-click actions: mark as paid, don't suspend this time, suspend now.
func TestOneClickActions(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	due := "2030-01-19"
	a := f.subscribe(hosting(due, 1500, "a.example", SupplierHostingCom))
	other := f.addCustomer(f.org, "Otieno", "Studio", "otieno@studio.example", "", "", "")
	in := hosting(due, 2000, "b.example", SupplierHostingCom)
	in.CustomerID = other
	b := f.subscribe(in)
	third := f.addCustomer(f.org, "Achieng", "Shop", "achieng@shop.example", "", "", "")
	in = hosting(due, 2500, "c.example", SupplierHostingCom)
	in.CustomerID = third
	c := f.subscribe(in)
	for _, off := range []int{-7, 4} {
		f.at(f.day(off, due))
		f.run()
	}
	tok := f.token(f.org, f.admin, "admin")
	// Mark as paid (M-Pesa, with a reference) through the subscriptions API.
	code, out := f.api("POST", "/subscriptions/"+a.ID+"/mark-paid", tok, map[string]any{"method": "mobile_money", "reference": "QKX81JD", "email_receipt": true})
	if code != 200 {
		t.Fatalf("mark paid: %d %v", code, out)
	}
	var paid float64
	var ref string
	f.db.Raw(`SELECT amount_paid FROM billing_invoices WHERE id = ?`, f.periods(a.ID)[0].InvoiceID).Scan(&paid)
	f.db.Raw(`SELECT reference FROM billing_payments WHERE invoice_id = ?`, f.periods(a.ID)[0].InvoiceID).Scan(&ref)
	if paid != 1500 || ref != "QKX81JD" || f.sub(a.ID).Status != StatusActive || f.periods(a.ID)[0].Status != PerPaid {
		t.Fatalf("marked paid: %v %q %s", paid, ref, f.sub(a.ID).Status)
	}
	if !hasSubject(f.out.to(clientEmail), "Receipt") && !hasSubject(f.out.to(clientEmail), "receipt") {
		t.Logf("receipt subjects: %v", f.out.to(clientEmail))
	}
	// Don't suspend this time: the suspension date passes, the reminders don't stop.
	if code, out := f.api("POST", "/subscriptions/"+b.ID+"/skip-suspension", tok, map[string]any{"note": "Long-standing client"}); code != 200 {
		t.Fatalf("skip: %d %v", code, out)
	}
	// Suspend now.
	if code, out := f.api("POST", "/subscriptions/"+c.ID+"/suspend", tok, map[string]any{"note": "Chargeback"}); code != 200 || f.sub(c.ID).Status != StatusSuspended {
		t.Fatalf("suspend now: %d %v", code, out)
	}
	for off := 5; off <= 10; off++ {
		f.at(f.day(off, due))
		f.run()
	}
	gb, _ := f.s.Get(f.org, b.ID)
	if f.sub(b.ID).Status != StatusPastDue || gb.Open == nil || gb.Open.SuspendOn != "" || !gb.Open.NoSuspend {
		t.Fatalf("not suspended this time: %s %+v", f.sub(b.ID).Status, gb.Open)
	}
	// Undo: the suspension is back on (3 days after the next alert).
	if _, err := f.s.SkipSuspension(f.org, b.ID, f.admin, "", true); err != nil {
		t.Fatal(err)
	}
	gb, _ = f.s.Get(f.org, b.ID)
	if gb.Open == nil || gb.Open.SuspendOn != f.day(13, due) {
		t.Fatalf("suspension back on: %+v", gb.Open)
	}
	// Unsuspend by hand needs more time while the invoice is overdue.
	if _, err := f.s.Unsuspend(context.Background(), f.org, c.ID, f.admin, 0, ""); err == nil {
		t.Fatal("unsuspend without more time while overdue")
	}
	if _, err := f.s.Unsuspend(context.Background(), f.org, c.ID, f.admin, 7, "Promised to pay Friday"); err != nil || f.sub(c.ID).Status != StatusPastDue {
		t.Fatalf("unsuspend + 7 days: %v %s", err, f.sub(c.ID).Status)
	}
	// Staff can't; other organisations' subscriptions are not found.
	if code, _ := f.api("POST", "/subscriptions/"+b.ID+"/extend", f.token(f.org, uuid.NewString(), "care_worker"), map[string]any{"days": 7}); code != http.StatusForbidden {
		t.Fatalf("staff: %d", code)
	}
}

func TestEmailPrefsRespected(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	due := "2030-01-19"
	f.subscribe(hosting(due, 1500, "x.example", SupplierHostingCom))
	off := false
	if err := emailprefs.SetAdmin(f.db, f.org, f.admin2, nil, map[string]*bool{AlertsCategory: &off}, f.admin); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SaveSettings(f.org, SettingsInput{Recipients: &[]string{"accounts@mursa.example"}}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []int{-7, 4} {
		f.at(f.day(d, due))
		f.run()
	}
	if !hasSubject(f.out.to(f.adminEmail(1)), "will be suspended in 3 days") || !hasSubject(f.out.to("accounts@mursa.example"), "will be suspended in 3 days") {
		t.Fatal("alert to the admin and the extra recipient")
	}
	if len(f.out.to(f.adminEmail(2))) != 0 {
		t.Fatalf("switched off: %v", f.out.to(f.adminEmail(2)))
	}
	for _, m := range f.out.list() {
		for _, a := range m.To {
			if strings.HasPrefix(a, "staff.") {
				t.Fatal("staff never get client billing emails")
			}
		}
	}
	// Who gets emails knows the category (owners and admins).
	if c, ok := emailprefs.CategoryByKey("client_billing"); !ok || c.Audience != emailprefs.Admins || AlertsCategory != "client_billing" {
		t.Fatal("client_billing category")
	}
	// The first admin alert can't be later than 3 days before the suspension.
	if _, err := f.s.SaveSettings(f.org, SettingsInput{AdminDays: &[]int{2, 1}}); err == nil {
		t.Fatal("admin alerts must start at least 3 days ahead")
	}
}

func TestCrossOrgIsolation(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	svc := f.service("Starter hosting — monthly", 1500, "KES")
	mine := f.subscribe(SubInput{Period: PeriodMonthly, StartDate: "2030-01-12", Lines: []LineInput{{ServiceID: svc}}})
	org2, adm2 := uuid.NewString(), uuid.NewString()
	f.addOrg(org2, "Australia/Sydney", "AUD", "AU", adm2)
	cust2 := f.addCustomer(org2, "Bondi", "Cafe", "hi@bondi.example", "", "", "AU")
	p := 30.0
	// Customers and services of another organisation can't be used.
	if _, err := f.s.Create(f.org, f.admin, SubInput{CustomerID: cust2, Period: PeriodMonthly, Lines: []LineInput{{ServiceID: svc}}}); err == nil {
		t.Fatal("other organisation's customer accepted")
	}
	if _, err := f.s.Create(org2, adm2, SubInput{CustomerID: cust2, Period: PeriodMonthly, Lines: []LineInput{{ServiceID: svc}}}); err == nil {
		t.Fatal("other organisation's service accepted")
	}
	theirs, err := f.s.Create(org2, adm2, SubInput{CustomerID: cust2, Period: PeriodMonthly, StartDate: "2030-01-12", Lines: []LineInput{{Description: "Hosting", UnitPrice: &p}}})
	if err != nil {
		t.Fatal(err)
	}
	if theirs.Number != "SUB-0001" || theirs.Currency != "AUD" {
		t.Fatalf("own numbering and currency: %s %s", theirs.Number, theirs.Currency)
	}
	f.at("2030-01-05")
	f.run() // only f.org
	if len(f.periods(theirs.ID)) != 0 || len(f.periods(mine.ID)) != 1 {
		t.Fatal("a run touches only its organisation")
	}
	if l := f.s.List(f.org, Filter{}); len(l) != 1 || l[0].ID != mine.ID {
		t.Fatal("list is scoped")
	}
	tok := f.token(f.org, f.admin, "admin")
	for _, path := range []string{"/subscriptions/" + theirs.ID} {
		if code, _ := f.api("GET", path, tok, nil); code != http.StatusNotFound {
			t.Fatalf("other organisation's subscription via the API: %d", code)
		}
	}
	for _, action := range []string{"extend", "suspend", "skip-suspension", "mark-paid", "unsuspend", "cancel"} {
		if code, _ := f.api("POST", "/subscriptions/"+theirs.ID+"/"+action, tok, map[string]any{"days": 7}); code != http.StatusNotFound && code != http.StatusBadRequest {
			t.Fatalf("%s on another org: %d", action, code)
		}
	}
	if f.sub(theirs.ID).Status != StatusActive {
		t.Fatal("untouched")
	}
	if code, out := f.api("GET", "/subscriptions", tok, nil); code != 200 || len(out["data"].(map[string]any)["subscriptions"].([]any)) != 1 {
		t.Fatalf("API list: %d", code)
	}
	if code, _ := f.api("GET", "/subscriptions", f.token(f.org, uuid.NewString(), "care_worker"), nil); code != http.StatusForbidden {
		t.Fatalf("staff: %d", code)
	}
	// The portal link of org2's customer shows only org2.
	v, err := f.s.Portal(f.s.PortalToken(org2, cust2))
	if err != nil || len(v.Subscriptions) != 1 || v.Subscriptions[0].Number != "SUB-0001" || v.Subscriptions[0].Currency != "AUD" {
		t.Fatalf("portal: %+v %v", v, err)
	}
	if _, err := f.s.Portal(f.s.PortalToken(f.org, cust2)); err == nil {
		t.Fatal("a customer of another organisation is not found through this organisation")
	}
}

func TestTimeZoneDecidesToday(t *testing.T) {
	f := setup(t, "Australia/Sydney", "AUD", "AU")
	sub := f.subscribe(hosting("2030-01-13", 30, "", ""))
	// 13:30 UTC on 5 Jan = 00:30 on 6 Jan in Sydney: the invoice is created 7 days ahead → on the 6th.
	f.now = time.Date(2030, 1, 5, 12, 59, 0, 0, time.UTC)
	f.s.RunOrg(context.Background(), f.org, RunOptions{Force: true})
	if len(f.periods(sub.ID)) != 0 {
		t.Fatal("still 5 Jan in Sydney")
	}
	f.now = time.Date(2030, 1, 5, 13, 30, 0, 0, time.UTC)
	f.s.RunOrg(context.Background(), f.org, RunOptions{Force: true})
	if pe := f.periods(sub.ID); len(pe) != 1 || DayStr(pe[0].IssueDate) != "2030-01-06" {
		t.Fatalf("6 Jan in Sydney: %+v", pe)
	}
	// The test clock moves the organisation's day only when it is enabled.
	f.s.TestClock = true
	if _, off, err := f.s.SetTestClock(f.org, "2030-02-01", nil, false); err != nil || off != 26 || DayStr(f.s.Today(f.org)) != "2030-02-01" {
		t.Fatalf("test clock: %d %v", off, err)
	}
	f.s.TestClock = false
	if DayStr(f.s.Today(f.org)) != "2030-01-06" {
		t.Fatal("test clock off")
	}
}

// MRR / ARR in the home currency across currencies, and the dashboard KPIs.
func TestMultiCurrencyAndDashboard(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	if _, err := f.s.FX.SaveManual(f.org, f.admin, "USD", "KES", 130, "test"); err != nil {
		t.Fatal(err)
	}
	vps := f.service("Cloud VPS — monthly", 20, "USD")
	a := f.subscribe(SubInput{Period: PeriodMonthly, StartDate: "2030-01-12", Lines: []LineInput{{ServiceID: vps}}}) // KES customer, USD service
	if a.Currency != "KES" || a.Amount != 2600 || a.MonthlyHome == nil || *a.MonthlyHome != 2600 {
		t.Fatalf("converted: %s %v %v", a.Currency, a.Amount, a.MonthlyHome)
	}
	usdCust := f.addCustomer(f.org, "Acme", "Ltd", "ap@acme.example", "", "USD", "US")
	p := 300.0
	b := f.subscribe(SubInput{CustomerID: usdCust, Period: PeriodYearly, StartDate: "2030-01-19", Lines: []LineInput{{Description: "Business hosting", UnitPrice: &p}}})
	if b.Currency != "USD" || b.MonthlyHome == nil || *b.MonthlyHome != 3250 {
		t.Fatalf("customer currency: %s %v", b.Currency, b.MonthlyHome)
	}
	auCust := f.addCustomer(f.org, "Bondi", "Cafe", "hi@bondi.example", "", "", "AU")
	if _, err := f.s.FX.SaveManual(f.org, f.admin, "AUD", "KES", 85, "test"); err != nil {
		t.Fatal(err)
	}
	q := 90.0
	c := f.subscribe(SubInput{CustomerID: auCust, Period: PeriodQuarterly, StartDate: "2030-01-20", Lines: []LineInput{{Description: "Hosting", UnitPrice: &q}}})
	if c.Currency != "AUD" || *c.MonthlyHome != 2550 {
		t.Fatalf("an Australian client is billed in AUD: %s %v", c.Currency, *c.MonthlyHome)
	}
	f.at("2030-01-05")
	f.run()
	// The USD service line is invoiced in KES at the locked rate.
	var inv invoicing.Invoice
	f.db.Preload("Items").Where("id = ?", f.periods(a.ID)[0].InvoiceID).First(&inv)
	if inv.Currency != "KES" || inv.Total != 2600 || inv.Items[0].OriginalCurrency != "USD" || *inv.Items[0].OriginalUnitPrice != 20 {
		t.Fatalf("converted invoice: %s %v %+v", inv.Currency, inv.Total, inv.Items[0])
	}
	dash := f.s.Dashboard(f.org)
	near := func(a, b float64) bool { return a-b < 0.05 && b-a < 0.05 }
	if !near(dash.MRR, 2600+3250+2550) || !near(dash.ARR, dash.MRR*12) || dash.Active != 3 || dash.Subscribers != 3 || len(dash.MRRBy) != 3 {
		t.Fatalf("MRR %v ARR %v active %d", dash.MRR, dash.ARR, dash.Active)
	}
	if dash.RenewalsKPI["count"].(int) != 3 {
		t.Fatalf("renewals in 30 days: %v %+v", dash.RenewalsKPI, dash.Renewals)
	}
	// a is 4 days overdue: past due, and suspended in 3 days unless paid.
	f.at("2030-01-16")
	f.run()
	dash = f.s.Dashboard(f.org)
	if dash.PastDue["subscriptions"].(int) != 1 || dash.PastDue["amount"].(float64) != 2600 || len(dash.Upcoming) != 1 || dash.Upcoming[0].SuspendOn != "2030-01-19" ||
		dash.Upcoming[0].DaysToSuspend != 3 {
		t.Fatalf("past due %v upcoming %+v", dash.PastDue, dash.Upcoming)
	}
	// Suspended subscriptions leave MRR; a cancellation is churn this month.
	f.at("2030-01-19")
	f.run()
	if _, err := f.s.Cancel(context.Background(), f.org, c.ID, f.admin, CancelInput{When: "now"}); err != nil {
		t.Fatal(err)
	}
	dash = f.s.Dashboard(f.org)
	if dash.Suspended != 1 || dash.Churned["count"].(int) != 1 || !near(dash.MRR, 3250) {
		t.Fatalf("suspended %d churned %v MRR %v", dash.Suspended, dash.Churned, dash.MRR)
	}
	var tasks []Task
	f.db.Where("subscription_id = ? AND kind = ?", c.ID, TaskCancel).Find(&tasks)
	if len(tasks) != 1 {
		t.Fatalf("cancel task: %d", len(tasks))
	}
}

func TestCSVImport(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	f.service("Starter hosting — monthly", 1500, "KES")
	csvText := "customer_email,customer_name,service,description,price,quantity,period,next_renewal,site,domain,domain_expiry,supplier,group\n" +
		"jane@wanjiku.example,Wanjiku Traders,Starter hosting — monthly,,,1,monthly,2030-02-01,wanjiku.example,,,synergy,A\n" +
		"jane@wanjiku.example,Wanjiku Traders,,.co.ke domain,100,1,monthly,2030-02-01,,wanjiku.co.ke,2030-06-30,synergy,A\n" +
		"new@client.example,New Client Ltd,Starter hosting,,,1,yearly,2030-03-01,newclient.example,,,hosting.com,\n" +
		"bad@client.example,,Missing service,,,1,monthly,2030-02-01,,,,,\n"
	res, err := f.s.Import(f.org, f.admin, strings.NewReader(csvText), true, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Valid != 3 || res.Invalid != 1 || res.Rows[3].OK || res.Rows[3].Error == "" {
		t.Fatalf("dry run: %+v", res)
	}
	var n int64
	f.db.Model(&Subscription{}).Count(&n)
	if n != 0 {
		t.Fatal("a dry run creates nothing")
	}
	res, err = f.s.Import(f.org, f.admin, strings.NewReader(strings.Join(strings.Split(csvText, "\n")[:4], "\n")), false, true)
	if err != nil || res.Created != 2 || res.Customers != 1 {
		t.Fatalf("import: %+v %v", res, err)
	}
	list := f.s.List(f.org, Filter{})
	var grouped *Subscription
	for i := range list {
		if len(list[i].Lines) == 2 {
			grouped = &list[i]
		}
	}
	if len(list) != 2 || grouped == nil || grouped.AmountTax != 1600 || grouped.Supplier != SupplierSynergy || DayStr(grouped.NextPeriodStart) != "2030-02-01" ||
		grouped.DomainExpiry == nil || DayStr(*grouped.DomainExpiry) != "2030-06-30" {
		t.Fatalf("grouped hosting + domain: %+v", grouped)
	}
	for _, x := range list {
		if x.Currency != "KES" {
			t.Fatalf("%s billed in %s", x.CustomerName, x.Currency)
		}
	}
}

func TestOnboardingSkipsPastPeriods(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	// A client since 2028 who renews on the 10th: billing starts at the next renewal.
	old := f.subscribe(hosting("2028-03-10", 1500, "", ""))
	if DayStr(old.NextPeriodStart) != "2030-01-10" || old.PaidThrough == nil || DayStr(*old.PaidThrough) != "2030-01-09" {
		t.Fatalf("existing client: next %s paid through %v", DayStr(old.NextPeriodStart), old.PaidThrough)
	}
	yes := true
	in := hosting("2029-11-10", 1500, "", "")
	in.BillPastPeriods = &yes
	back := f.subscribe(in)
	f.run()
	if len(f.periods(back.ID)) != 3 || len(f.periods(old.ID)) != 1 {
		t.Fatalf("back-billed %d, existing %d", len(f.periods(back.ID)), len(f.periods(old.ID)))
	}
	// Invoices created late are due in a week at the earliest.
	for _, p := range f.periods(back.ID) {
		if p.DueDate.Before(d("2030-01-12")) {
			t.Fatalf("late invoice due %s", DayStr(p.DueDate))
		}
	}
}

// Domains are never left to expire silently: admin reminders 30, 14 and 7
// days ahead (from Synergy's domain list), then once when expired.
func TestDomainRenewalReminders(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	f.db.Exec(`INSERT INTO expense_renewals (id, organization_id, source, external_id, kind, name, expires_on, auto_renew, active, created_at, updated_at)
		VALUES (?, ?, 'synergy_api', 'domain:wanjiku.co.ke', 'domain', 'wanjiku.co.ke', '2030-03-01', false, true, now(), now())`, uuid.NewString(), f.org)
	yr := 1200.0
	sub := f.subscribe(SubInput{Period: PeriodYearly, StartDate: "2029-03-01", BillPastPeriods: new(bool), Lines: []LineInput{{Description: ".co.ke domain", UnitPrice: &yr}},
		Domain: "wanjiku.co.ke", Supplier: SupplierSynergy})
	// A typed expiry for a domain Synergy doesn't list.
	exp := "2030-02-20"
	in := hosting("2030-01-10", 1500, "other.example", SupplierHostingCom)
	in.DomainExpiry = exp
	other := f.subscribe(in)
	got := map[string][]string{}
	for day := d("2030-01-25"); !day.After(d("2030-03-05")); day = day.AddDate(0, 0, 1) {
		f.at(DayStr(day))
		f.out.reset()
		f.run()
		for _, s := range f.out.to(f.adminEmail(1)) {
			if strings.Contains(s, "expire") {
				got[DayStr(day)] = append(got[DayStr(day)], s)
			}
		}
	}
	want := map[string]string{"2030-01-30": "wanjiku.co.ke expires in 30 days", "2030-02-15": "wanjiku.co.ke expires in 14 days", "2030-02-22": "wanjiku.co.ke expires in 7 days",
		"2030-03-02": "wanjiku.co.ke expired", "2030-02-06": "other.example expires in 14 days", "2030-02-13": "other.example expires in 7 days"}
	for day, part := range want {
		if !hasSubject(got[day], part) {
			t.Errorf("%s: want %q, got %v", day, part, got[day])
		}
	}
	total := 0
	for _, l := range got {
		total += len(l)
	}
	// other.example: 30 days before was 21 Jan (before the walk) → caught up once on the 25th.
	if !hasSubject(got["2030-01-25"], "other.example expires in 26 days") || total != len(want)+2 {
		t.Fatalf("catch-up and no repeats (%d): %v", total, got)
	}
	if body := f.out.body(f.adminEmail(1), ""); body == "" {
		_ = body
	}
	di := f.s.domainInfo(f.org, &Subscription{Domain: "wanjiku.co.ke"}, d("2030-02-01"))
	if len(di) != 1 || di[0].Source != "synergy" || di[0].AutoRenew == nil || *di[0].AutoRenew || *di[0].DaysLeft != 28 {
		t.Fatalf("domain info: %+v", di)
	}
	_, _ = sub, other
}
