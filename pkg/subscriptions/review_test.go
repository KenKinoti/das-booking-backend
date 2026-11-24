package subscriptions

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
)

func (f *fixture) installReview(t *testing.T) *outbox.Service {
	t.Helper()
	if err := outbox.Migrate(f.db); err != nil {
		t.Fatal(err)
	}
	notify.Disabled = true
	t.Cleanup(func() { notify.Disabled = false })
	ob := outbox.Install(&outbox.Service{DB: f.db, Mail: f.s.Mail, Now: func() time.Time { return f.now }})
	t.Cleanup(outbox.Uninstall)
	return ob
}

func (f *fixture) waiting(sub string) []Event {
	var ev []Event
	f.db.Where("subscription_id = ? AND status = ?", sub, "waiting").Order("created_at").Find(&ev)
	return ev
}

// The client emails of a subscription wait in Pending review; the period
// counts as emailed (dunning starts) only once its invoice email went; the
// suspension waits while the final notice waits, and the client still gets
// the full notice period after it went.
func TestReviewSubscriptionNoticesAndSuspension(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	ob := f.installReview(t)
	admin := outbox.Actor{Org: f.org, User: f.admin, Role: "admin", Name: "Ada Owner"}
	approve := func(e Event) {
		t.Helper()
		if _, outcome, err := ob.Approve(context.Background(), admin, e.ReviewID, "now", nil, ""); err != nil || outcome != outbox.OutSent {
			t.Fatalf("approve %s: %v %s", e.Stage, err, outcome)
		}
	}
	due := "2030-01-19"
	sub := f.subscribe(hosting(due, 1500, "wanjiku.example", SupplierHostingCom))
	f.at(f.day(-7, due))
	f.out.reset()
	f.run()
	if len(f.out.to(clientEmail)) != 0 {
		t.Fatalf("nothing to the client yet: %v", f.out.to(clientEmail))
	}
	w := f.waiting(sub.ID)
	if len(w) != 1 || w[0].Stage != "invoice" || w[0].ReviewID == "" || f.periods(sub.ID)[0].Sent {
		t.Fatalf("invoice email waits, period not emailed: %+v %+v", w, f.periods(sub.ID))
	}
	it := outbox.Get(w[0].ReviewID)
	if it.Origin != "subscriptions" || it.TriggerKind != outbox.TriggerAutomatic || !strings.Contains(it.Trigger, sub.Number) {
		t.Fatalf("invoice item: %+v", it)
	}
	// Not emailed → no reminders while it waits.
	f.at(f.day(-3, due))
	f.run()
	if len(f.waiting(sub.ID)) != 1 {
		t.Fatal("no reminder before the invoice went")
	}
	approve(w[0])
	if !hasSubject(f.out.to(clientEmail), "Invoice") || !f.periods(sub.ID)[0].Sent {
		t.Fatalf("invoice sent, period emailed: %v", f.out.to(clientEmail))
	}
	var e Event
	f.db.Where("id = ?", w[0].ID).Take(&e)
	if e.Status != "sent" {
		t.Fatalf("event sent: %+v", e)
	}
	// Approve the client reminders; leave the final notice (D+5) waiting.
	var final Event
	for off := -2; off <= 8; off++ {
		f.at(f.day(off, due))
		f.run()
		for _, x := range f.waiting(sub.ID) {
			if x.Stage == "final_notice" {
				final = x
				continue
			}
			approve(x)
		}
	}
	if final.ID == "" {
		t.Fatal("final notice waits")
	}
	fi := outbox.Get(final.ReviewID)
	if fi.Blocks != "suspension" || fi.Deadline == nil || DayStr(*fi.Deadline) != f.day(7, due) || !strings.HasPrefix(fi.Flag, "Blocking a suspension on ") {
		t.Fatalf("final notice flagged: %+v", fi)
	}
	if s := f.sub(sub.ID); s.Status == StatusSuspended {
		t.Fatal("the suspension waits while the final notice waits")
	}
	var sw int64
	f.db.Model(&Event{}).Where("subscription_id = ? AND stage = ?", sub.ID, "suspend_waiting").Count(&sw)
	if sw < 1 {
		t.Fatal("suspend_waiting recorded")
	}
	dt, _ := f.s.GetDetail(f.org, sub.ID)
	if dt.ReviewHold == nil || dt.ReviewHold.ReviewID != final.ReviewID || !strings.Contains(dt.ReviewHold.Message, "final notice") {
		t.Fatalf("review hold: %+v", dt.ReviewHold)
	}
	// D+9: approved; the client gets the full 2 days → suspended on D+11.
	f.at(f.day(9, due))
	f.out.reset()
	approve(final)
	if !hasSubject(f.out.to(clientEmail), "Final notice") {
		t.Fatalf("final notice sent: %v", f.out.to(clientEmail))
	}
	f.run()
	f.at(f.day(10, due))
	f.run()
	if s := f.sub(sub.ID); s.Status == StatusSuspended {
		t.Fatal("not before the notice period after it went")
	}
	f.at(f.day(11, due))
	f.run()
	if s := f.sub(sub.ID); s.Status != StatusSuspended {
		t.Fatalf("suspended once the notice period passed: %s", s.Status)
	}
	// The suspension notice to the client waits too.
	if w := f.waiting(sub.ID); len(w) != 1 || w[0].Stage != "suspend" {
		t.Fatalf("suspension notice waits: %+v", w)
	}
}

// A rejected reminder is recorded on the timeline; "Remind now" waits unless
// an approver ticks Send now.
func TestReviewManualReminder(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	ob := f.installReview(t)
	due := "2030-01-19"
	sub := f.subscribe(hosting(due, 1500, "wanjiku.example", SupplierHostingCom))
	f.at(f.day(-7, due))
	f.run()
	w := f.waiting(sub.ID)
	ob.Approve(context.Background(), outbox.Actor{Org: f.org, User: f.admin, Role: "admin"}, w[0].ReviewID, "now", nil, "")
	f.out.reset()
	_, reviewID, err := f.s.SendReminderReview(context.Background(), f.org, sub.ID, f.admin, "admin", false)
	if err != nil || reviewID == "" || len(f.out.to(clientEmail)) != 0 {
		t.Fatalf("waits: %v %q %v", err, reviewID, f.out.to(clientEmail))
	}
	if _, err := ob.Reject(context.Background(), outbox.Actor{Org: f.org, User: f.admin, Role: "admin", Name: "Ada Owner"}, reviewID, "Called them instead"); err != nil {
		t.Fatal(err)
	}
	var e Event
	f.db.Where("review_id = ?", reviewID).Take(&e)
	if e.Status != "rejected" || !strings.Contains(e.Error, "Called them instead") {
		t.Fatalf("rejected on the timeline: %+v", e)
	}
	_, reviewID, err = f.s.SendReminderReview(context.Background(), f.org, sub.ID, f.admin, "admin", true)
	if err != nil || reviewID != "" || !hasSubject(f.out.to(clientEmail), "invoice") {
		t.Fatalf("send now by an admin: %v %q %v", err, reviewID, f.out.to(clientEmail))
	}
}
