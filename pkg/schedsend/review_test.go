package schedsend

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
)

// reviewWorld is the schedule world with Pending review installed (same clock).
type reviewWorld struct {
	*world
	ob *outbox.Service
}

func newReviewWorld(t *testing.T) *reviewWorld {
	w := newWorld(t)
	if err := outbox.Migrate(w.db); err != nil {
		t.Fatal(err)
	}
	ob := outbox.Install(&outbox.Service{DB: w.db, Now: func() time.Time { return w.now }})
	t.Cleanup(outbox.Uninstall)
	w.s.RegisterReview()
	return &reviewWorld{world: w, ob: ob}
}

func (w *reviewWorld) approver() outbox.Actor {
	return outbox.Actor{Org: w.org, User: w.owner.User, Role: "owner", Name: "Amani Test"}
}

func (w *reviewWorld) item(id string) *outbox.Item { return outbox.Get(id) }

func (w *reviewWorld) obRun() outbox.RunResult {
	w.t.Helper()
	r, err := w.ob.RunOnce(context.Background(), 5*time.Second)
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

// Customer email, SMS and WhatsApp deliveries are prepared ahead and wait;
// nothing is sent until approved; approved, they go through the same
// channel code (edits included) and the deliveries and run are settled.
func TestReviewCustomerDeliveriesWaitAndRelease(t *testing.T) {
	w := newReviewWorld(t)
	cu := w.mkCustomer(w.org, "Wanjiru", "wanjiru@savanna.test", "0712345678", "Savanna Bistro")
	inv := w.mkInvoice(w.org, cu, "sent", 5800, 5800, "2026-11-10")
	m := w.create(w.owner, Input{Kind: KindCustomer, Channels: []string{ChanEmail, ChanSMS}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}},
		Subject: "Office closed on Monday", Body: "Hi {first_name}, our office is closed on Monday.", SMSBody: "Hi {first_name}, closed Mon.",
		LocalSendAt: tomorrow8, Timezone: "Africa/Nairobi"})
	rem := w.create(w.owner, Input{Kind: KindReminder, InvoiceID: inv, Channels: []string{ChanWhatsApp},
		WhatsApp:    WhatsAppPlan{Template: "payment_reminder", Language: "en", Params: []string{"{first_name}", "{invoice_number}", "{amount_due}"}},
		LocalSendAt: tomorrow8, Timezone: "Africa/Nairobi"})
	// 9 hours before: prepared ahead, everything waits.
	w.run()
	ds := w.deliveries(m.ID)
	if len(ds) != 2 || ds[0].Status != DelWaiting || ds[1].Status != DelWaiting || ds[0].ReviewID == "" || ds[0].Reason != waitingReason {
		t.Fatalf("waiting deliveries: %+v", ds)
	}
	if r := w.runs(m.ID); len(r) != 1 || r[0].Status != RunWaiting || !strings.Contains(r[0].Note, "2 waiting for approval") {
		t.Fatalf("run: %+v", r)
	}
	if mm := w.msg(m.ID); mm.Status != StatusDone || mm.EndedReason != endedWaiting || mm.LastResult != RunWaiting {
		t.Fatalf("message: %s %q %s", mm.Status, mm.EndedReason, mm.LastResult)
	}
	wd := w.deliveries(rem.ID)
	if len(wd) != 1 || wd[0].Status != DelWaiting {
		t.Fatalf("whatsapp delivery: %+v", wd)
	}
	if len(w.mails) != 0 || len(w.sms) != 0 || len(w.wa.sends) != 0 {
		t.Fatalf("nothing sent: %d %d %d", len(w.mails), len(w.sms), len(w.wa.sends))
	}
	var emailD, smsD Delivery
	for _, d := range ds {
		if d.Channel == ChanEmail {
			emailD = d
		} else {
			smsD = d
		}
	}
	ei := w.item(emailD.ReviewID)
	if ei.Kind != outbox.KindCustomerMessage || ei.TriggerKind != outbox.TriggerScheduled || ei.SendAt == nil || !ei.SendAt.Equal(at8) ||
		ei.Message != "Hi Wanjiru, our office is closed on Monday." || ei.Subject != "Office closed on Monday" {
		t.Fatalf("email item: %+v", ei)
	}
	si := w.item(smsD.ReviewID)
	if si.Channel != ChanSMS || si.Message != "Hi Wanjiru, closed Mon." {
		t.Fatalf("sms item: %+v", si)
	}
	wi := w.item(wd[0].ReviewID)
	if wi.Channel != ChanWhatsApp || wi.WATemplate != "payment_reminder" || len(wi.WAParams) != 3 || wi.WAParams[0] != "Wanjiru" || !wi.Dated || wi.RecordNumber == "" {
		t.Fatalf("whatsapp item: %+v", wi)
	}
	// Previews.
	if pv := w.ob.DetailOf(context.Background(), ei, true).Preview; !strings.Contains(pv.HTML, "closed on Monday") {
		t.Fatalf("email preview: %+v", pv)
	}
	// Approve the email now (edited), the SMS for its time, the WhatsApp with edited values.
	msg := "Hi Wanjiru, our office is closed on Monday 1 Dec. Back on Tuesday."
	if _, outcome, err := w.ob.Approve(context.Background(), w.approver(), ei.ID, "now", &outbox.Edits{Message: &msg}, ""); err != nil || outcome != outbox.OutSent {
		t.Fatalf("approve email: %v %s", err, outcome)
	}
	if len(w.mails) != 1 || !strings.Contains(w.mails[0].HTML, "Monday 1 Dec") || w.mails[0].To[0] != "wanjiru@savanna.test" {
		t.Fatalf("email sent: %+v", w.mails)
	}
	if _, outcome, err := w.ob.Approve(context.Background(), w.approver(), si.ID, "scheduled", nil, ""); err != nil || outcome != outbox.OutApproved {
		t.Fatalf("approve sms for later: %v %s", err, outcome)
	}
	params := []string{"Wanjiru", "INV-1001", "KES 5,800.00"}
	if _, outcome, err := w.ob.Approve(context.Background(), w.approver(), wi.ID, "now", &outbox.Edits{WhatsAppParams: &params}, ""); err != nil || outcome != outbox.OutSent {
		t.Fatalf("approve whatsapp: %v %s", err, outcome)
	}
	if len(w.wa.sends) != 1 || w.wa.sends[0].Kind != "template" || w.wa.sends[0].Params[2] != "KES 5,800.00" {
		t.Fatalf("whatsapp sent: %+v", w.wa.sends)
	}
	if d := w.deliveries(rem.ID)[0]; d.Status != DelSent || d.ProviderID == "" || d.Mode != "template" {
		t.Fatalf("whatsapp delivery settled: %+v", d)
	}
	if r := w.obRun(); r.Released != 0 || len(w.sms) != 0 {
		t.Fatalf("not before its time: %+v", r)
	}
	w.setNow(at8.Add(time.Minute))
	if r := w.obRun(); r.Released != 1 || r.Sent != 1 {
		t.Fatalf("released at its time: %+v", r)
	}
	if len(w.sms) != 1 || w.sms[0].Text != "Hi Wanjiru, closed Mon." {
		t.Fatalf("sms sent: %+v", w.sms)
	}
	for _, d := range w.deliveries(m.ID) {
		if d.Status != DelSent {
			t.Fatalf("delivery %s: %s", d.Channel, d.Status)
		}
	}
	if r := w.runs(m.ID)[0]; r.Status != RunSent || r.Sent != 2 {
		t.Fatalf("run finished: %+v", r)
	}
	if mm := w.msg(m.ID); mm.EndedReason != "Sent" {
		t.Fatalf("ended reason: %q", mm.EndedReason)
	}
	// Nothing goes twice.
	w.run()
	w.obRun()
	if len(w.mails) != 1 || len(w.sms) != 1 || len(w.wa.sends) != 1 {
		t.Fatal("exactly once")
	}
}

// Rejected → skipped with the note; superseded (paid) → skipped with the reason.
func TestReviewRejectAndSupersede(t *testing.T) {
	w := newReviewWorld(t)
	cu := w.mkCustomer(w.org, "Baraka", "baraka@savanna.test", "0712000111", "")
	inv := w.mkInvoice(w.org, cu, "sent", 3200, 3200, "2026-11-10")
	rem := w.create(w.owner, Input{Kind: KindReminder, InvoiceID: inv, Channels: []string{ChanSMS}, SMSBody: "Hi {first_name}, {invoice_number} is overdue.",
		LocalSendAt: tomorrow8, Timezone: "Africa/Nairobi"})
	news := w.create(w.owner, Input{Kind: KindCustomer, Channels: []string{ChanEmail}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}},
		Subject: "News", Body: "Hi {first_name}", LocalSendAt: tomorrow8, Timezone: "Africa/Nairobi"})
	w.run()
	rd, nd := w.deliveries(rem.ID)[0], w.deliveries(news.ID)[0]
	if rd.Status != DelWaiting || nd.Status != DelWaiting {
		t.Fatalf("waiting: %s %s", rd.Status, nd.Status)
	}
	if _, err := w.ob.Reject(context.Background(), w.approver(), nd.ReviewID, "Not now"); err != nil {
		t.Fatal(err)
	}
	if d := w.deliveries(news.ID)[0]; d.Status != DelSkipped || d.Reason != "Rejected in Pending review by Amani Test: Not now" {
		t.Fatalf("rejected delivery: %+v", d)
	}
	if r := w.runs(news.ID)[0]; r.Status != RunSkipped {
		t.Fatalf("run: %+v", r)
	}
	w.db.Exec(`UPDATE billing_invoices SET status = 'paid', balance_due = 0 WHERE id = ?`, inv)
	if r := w.obRun(); r.Superseded != 1 {
		t.Fatalf("superseded: %+v", r)
	}
	if d := w.deliveries(rem.ID)[0]; d.Status != DelSkipped || !strings.Contains(d.Reason, "Paid — reminders stopped") {
		t.Fatalf("superseded delivery: %+v", d)
	}
	if len(w.sms) != 0 || len(w.mails) != 0 {
		t.Fatal("nothing sent")
	}
}

// Team messages never wait; an approver's "I've reviewed it" one-off goes at
// its time; staff's tick is ignored (it waits) with a note; review off sends as before.
func TestReviewTeamReviewedAndOff(t *testing.T) {
	w := newReviewWorld(t)
	team := w.create(w.owner, Input{Kind: KindTeam, Channels: []string{ChanInApp, ChanEmail}, Audience: Audience{Mode: AudStaff, UserIDs: []string{w.staff.User}},
		Subject: "Stock take", Body: "Stock take on Friday", LocalSendAt: tomorrow8, Timezone: "Africa/Nairobi"})
	cu := w.mkCustomer(w.org, "Achieng", "achieng@savanna.test", "0712000222", "")
	rv := w.create(w.owner, Input{Kind: KindCustomer, Channels: []string{ChanEmail}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}},
		Subject: "Thanks", Body: "Hi {first_name}", LocalSendAt: tomorrow8, Timezone: "Africa/Nairobi", Reviewed: true})
	if !rv.Reviewed || rv.ReviewedByName == "" {
		t.Fatalf("reviewed by the owner: %+v", rv)
	}
	st := w.create(w.manager, Input{Kind: KindCustomer, Channels: []string{ChanEmail}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}},
		Subject: "Hello", Body: "Hi {first_name}", LocalSendAt: tomorrow8, Timezone: "Africa/Nairobi", Reviewed: true})
	if st.Reviewed || st.ReviewNote == "" {
		t.Fatalf("a manager's tick is ignored: %+v", st)
	}
	rep := w.create(w.owner, Input{Kind: KindCustomer, Channels: []string{ChanEmail}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}},
		Subject: "Weekly", Body: "Hi", LocalSendAt: tomorrow8, Timezone: "Africa/Nairobi", Repeat: Recurrence{Freq: FreqWeekly, Interval: 1, Weekdays: []int{4}}, Reviewed: true})
	if rep.Reviewed || !strings.Contains(rep.ReviewNote, "Repeating") {
		t.Fatalf("repeating messages are reviewed each time: %+v", rep)
	}
	w.run() // 9 h ahead: only the unreviewed client ones are prepared
	if len(w.deliveries(team.ID)) != 0 || len(w.deliveries(rv.ID)) != 0 {
		t.Fatal("team and reviewed messages are not prepared ahead")
	}
	if d := w.deliveries(st.ID); len(d) != 1 || d[0].Status != DelWaiting {
		t.Fatalf("manager's waits: %+v", d)
	}
	w.setNow(at8.Add(time.Minute))
	w.run()
	if d := w.deliveries(team.ID); len(d) != 2 || d[0].Status != DelSent || d[1].Status != DelSent {
		t.Fatalf("team sent at its time: %+v", d)
	}
	if d := w.deliveries(rv.ID); len(d) != 1 || d[0].Status != DelSent {
		t.Fatalf("reviewed one-off sent at its time: %+v", d)
	}
	// Review off: a new customer message goes at its time.
	w.ob.SaveSettings(w.approver(), func(s *outbox.Settings) error { s.Enabled = false; return nil })
	w.setNow(at8.Add(2 * time.Hour))
	off := w.create(w.owner, Input{Kind: KindCustomer, Channels: []string{ChanSMS}, Audience: Audience{Mode: AudOne, CustomerIDs: []string{cu}},
		SMSBody: "Hi", Body: "Hi", LocalSendAt: "2026-11-19T14:00", Timezone: "Africa/Nairobi"})
	w.run()
	if len(w.deliveries(off.ID)) != 0 {
		t.Fatal("review off: not prepared ahead")
	}
	w.setNow(time.Date(2026, 11, 19, 11, 1, 0, 0, time.UTC))
	w.run()
	if d := w.deliveries(off.ID); len(d) != 1 || d[0].Status != DelSent {
		t.Fatalf("review off sends at its time: %+v", d)
	}
}

// queuedInvoices answers like the invoicing API when Pending review queues the email.
type queuedInvoices struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (q *queuedInvoices) Send(_ context.Context, who Actor, id string, body map[string]any) (InvoiceResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, body)
	rv, _ := body["review"].(map[string]any)
	it, err := outbox.Enqueue(nil, &outbox.Item{OrganizationID: who.Org, Kind: outbox.KindInvoiceEmail, Module: "fakeinv", Channel: outbox.ChanEmail, To: "x@savanna.test",
		Subject: "Invoice", Origin: "schedsend", OriginRef: rv["origin_ref"].(string)})
	if err != nil {
		return InvoiceResult{}, err
	}
	return InvoiceResult{Queued: true, ReviewID: it.ID}, nil
}

type okReleaser struct{}

func (okReleaser) Recheck(context.Context, *outbox.Item) string { return "" }
func (okReleaser) Send(_ context.Context, it *outbox.Item) (outbox.Result, error) {
	return outbox.Result{Text: "Sent to " + it.To, Ref: it.ID}, nil
}
func (okReleaser) Preview(context.Context, *outbox.Item) outbox.Preview { return outbox.Preview{} }
func (okReleaser) Closed(context.Context, *outbox.Item)                 {}

// Invoice deliveries: the invoicing send path queues them (meta passed); the
// delivery waits and is settled by the outcome hook.
func TestReviewInvoiceDeliveryWaitsOnInvoicing(t *testing.T) {
	w := newReviewWorld(t)
	q := &queuedInvoices{}
	w.s.Invoices = q
	outbox.Register("fakeinv", okReleaser{})
	cu := w.mkCustomer(w.org, "Njeri", "njeri@savanna.test", "0712000333", "")
	inv := w.mkInvoice(w.org, cu, "draft", 8400, 8400, "2026-12-02")
	m := w.create(w.owner, Input{Kind: KindInvoice, InvoiceID: inv, LocalSendAt: tomorrow8, Timezone: "Africa/Nairobi", InvoiceSend: &InvoiceSend{AcceptWarnings: true}})
	w.run()
	if len(q.calls) != 1 {
		t.Fatalf("invoicing called once (prepared ahead): %d", len(q.calls))
	}
	rv := q.calls[0]["review"].(map[string]any)
	if rv["origin"] != "schedsend" || rv["trigger_kind"] != outbox.TriggerScheduled || rv["force"] != true || !strings.HasPrefix(rv["trigger"].(string), "Scheduled: ") {
		t.Fatalf("review meta: %v", rv)
	}
	d := w.deliveries(m.ID)[0]
	if d.Status != DelWaiting || d.ReviewID == "" {
		t.Fatalf("waits on invoicing: %+v", d)
	}
	if _, outcome, err := w.ob.Approve(context.Background(), w.approver(), d.ReviewID, "now", nil, ""); err != nil || outcome != outbox.OutSent {
		t.Fatalf("approve: %v %s", err, outcome)
	}
	if d := w.deliveries(m.ID)[0]; d.Status != DelSent || d.ProviderID != d.ReviewID {
		t.Fatalf("settled by the hook: %+v", d)
	}
	if r := w.runs(m.ID)[0]; r.Status != RunSent {
		t.Fatalf("run: %+v", r)
	}
}

var _ = uuid.NewString
