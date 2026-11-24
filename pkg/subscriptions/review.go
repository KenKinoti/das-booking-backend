package subscriptions

// Pending review (pkg/outbox): while the organisation reviews client
// messages, the client emails of a subscription (the period's invoice,
// reminders, the final notice, the suspension notice — all sent through the
// invoicing API — and the "active again" notice sent here) wait for an
// approver. The timeline event is "waiting" (review_id) until the item is
// decided; the period counts as emailed (so dunning starts) only once its
// invoice email really went. A suspension waits while its warning notice
// waits, and the client always gets the full final-notice period after the
// notice actually went.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
)

// resultSender is an Invoicer that returns the send answer (APIInvoicer).
type resultSender interface {
	SendResult(ctx context.Context, who Actor, invoiceID string, body map[string]any) (map[string]any, error)
}

// SendResult POSTs /invoices/:id/send and returns the answer.
func (a *APIInvoicer) SendResult(ctx context.Context, who Actor, id string, body map[string]any) (map[string]any, error) {
	if body == nil {
		body = map[string]any{}
	}
	if _, ok := body["mode"]; !ok {
		body["mode"] = "email"
	}
	return a.call(ctx, who, "POST", "/invoices/"+id+"/send", body)
}

// clientSend emails the client through the invoicing API. reviewID is set
// when the email waits in Pending review.
func (s *Service) clientSend(ctx context.Context, who Actor, invoiceID string, body map[string]any, meta map[string]any) (string, error) {
	if s.Inv == nil {
		return "", errors.New("invoicing is not available")
	}
	if meta != nil && outbox.Active() {
		body["review"] = meta
	}
	rs, ok := s.Inv.(resultSender)
	if !ok {
		return "", s.Inv.Send(ctx, who, invoiceID, body)
	}
	d, err := rs.SendResult(ctx, who, invoiceID, body)
	if err != nil {
		return "", err
	}
	if q, _ := d["queued"].(bool); q {
		if rv, ok := d["review"].(map[string]any); ok {
			id, _ := rv["id"].(string)
			return id, nil
		}
	}
	return "", nil
}

// meta is the review description of a client email.
func (s *Service) meta(sub *Subscription, row *Event, trigger string, dated bool) map[string]any {
	m := map[string]any{"trigger": outbox.Clip(trigger, 290), "trigger_kind": outbox.TriggerAutomatic, "dated": dated,
		"context": []outbox.ContextLine{{Label: "Subscription", Value: strings.TrimSpace(sub.Number + " · " + sub.Name)}}}
	if row != nil {
		m["origin"], m["origin_ref"], m["key"] = "subscriptions", row.ID, "subscriptions:"+row.ID
	}
	return m
}

// finishSend closes a claimed client event: sent, failed, or waiting.
func (s *Service) finishSend(row *Event, reviewID string, err error) {
	if err == nil && reviewID != "" {
		s.DB.Model(&Event{}).Where("id = ?", row.ID).Updates(map[string]any{"status": "waiting", "review_id": reviewID, "error": "", "updated_at": s.now(),
			"recipients": clip(row.Recipients, 4000), "subject": clip(row.Subject, 480)})
		row.Status, row.ReviewID = "waiting", reviewID
		return
	}
	s.finish(row, "sent", err)
}

// ---------------------------------------------------------------- suspension waits

// suspensionWaits says why the suspension of per must wait ("" = it may go):
// a client notice for it still waits for approval, or the final notice went
// (after approval) less than final_notice_days ago.
func (s *Service) suspensionWaits(org string, sub *Subscription, per *BillingPeriod, st Settings, d Dates, today time.Time) string {
	var waiting []Event
	s.DB.Where("period_id = ? AND audience = ? AND status = ? AND stage <> ?", per.ID, ToClient, "waiting", "invoice").Order("created_at DESC").Find(&waiting)
	if len(waiting) > 0 {
		w := waiting[0]
		for _, e := range waiting {
			if e.Stage == "final_notice" {
				w = e
				break
			}
		}
		what := "the final notice"
		if w.Stage != "final_notice" {
			what = "the reminder"
		}
		if d.Suspend != nil {
			outbox.UpdateFlags(org, w.ReviewID, d.Suspend, "Blocking a suspension on "+shortDate(*d.Suspend), "suspension")
		}
		return "Suspension waits: " + what + " to the client is waiting for approval in Pending review"
	}
	if st.FinalNoticeDays > 0 {
		var sent []Event
		s.DB.Where("period_id = ? AND audience = ? AND stage = ? AND status = ? AND review_id <> ''", per.ID, ToClient, "final_notice", "sent").
			Order("updated_at DESC").Limit(1).Find(&sent)
		if len(sent) > 0 {
			went := Day(sent[0].UpdatedAt.In(s.Loc(org)))
			earliest := went.AddDate(0, 0, st.FinalNoticeDays)
			if today.Before(earliest) {
				return fmt.Sprintf("Suspension moved to %s: the final notice went on %s (after approval), so the client gets the full %d days", longDate(earliest),
					longDate(went), st.FinalNoticeDays)
			}
		}
	}
	return ""
}

// ReviewHold is shown on a subscription whose suspension waits for a notice.
type ReviewHold struct {
	ReviewID string    `json:"review_id"`
	Message  string    `json:"message"`
	Since    time.Time `json:"since"`
}

func (s *Service) reviewHold(sub *Subscription) *ReviewHold {
	var ev []Event
	s.DB.Where("subscription_id = ? AND stage = ? AND created_at >= ?", sub.ID, "suspend_waiting", s.now().AddDate(0, 0, -3)).Order("created_at DESC").Limit(1).Find(&ev)
	if len(ev) == 0 {
		return nil
	}
	var w []Event
	s.DB.Where("period_id = ? AND audience = ? AND status = ?", ev[0].PeriodID, ToClient, "waiting").Order("created_at DESC").Limit(1).Find(&w)
	if len(w) == 0 && !strings.HasPrefix(ev[0].Subject, "Suspension moved") {
		return nil
	}
	h := &ReviewHold{Message: ev[0].Subject, Since: ev[0].CreatedAt}
	if len(w) > 0 {
		h.ReviewID = w[0].ReviewID
	}
	return h
}

// ---------------------------------------------------------------- outcomes

// RegisterReview lets Pending review send the notices this package owns and
// tells it the outcome of the emails it started.
func (s *Service) RegisterReview() {
	rr := &reviewReleaser{s: s}
	outbox.Register("subscriptions", rr)
	outbox.RegisterOrigin("subscriptions", rr.outcome)
}

type reviewReleaser struct{ s *Service }

// outcome settles the timeline event of a decided email.
func (rr *reviewReleaser) outcome(ctx context.Context, it *outbox.Item) {
	s := rr.s
	var e Event
	if it.OriginRef == "" || s.DB.Where("id = ?", it.OriginRef).Take(&e).Error != nil {
		return
	}
	now := s.now()
	upd := map[string]any{"updated_at": now}
	switch it.Status {
	case outbox.StatusSent:
		upd["status"], upd["error"] = "sent", ""
		if it.To != "" {
			upd["recipients"] = clip(strings.Join(append([]string{it.To}, it.CC...), ", "), 4000)
		}
		if it.Subject != "" {
			upd["subject"] = clip(it.Subject, 480)
		}
	case outbox.StatusRejected:
		why := "Not sent — rejected in Pending review"
		if it.DecidedByName != "" {
			why += " by " + it.DecidedByName
		}
		if it.DecisionNote != "" {
			why += ": " + it.DecisionNote
		}
		upd["status"], upd["error"] = "rejected", clip(why, 1000)
	case outbox.StatusSuperseded:
		upd["status"], upd["error"] = "superseded", clip("Not sent — "+it.SupersededReason, 1000)
	case outbox.StatusFailed:
		upd["error"] = clip(it.Error, 1000) // still waiting: it can be retried
	default:
		return
	}
	s.DB.Model(&Event{}).Where("id = ? AND status IN ?", e.ID, []string{"waiting", "failed"}).Updates(upd)
	if it.Status == outbox.StatusSent && e.Stage == "invoice" && e.PeriodID != "" {
		s.DB.Model(&BillingPeriod{}).Where("id = ?", e.PeriodID).Updates(map[string]any{"sent": true, "updated_at": now})
	}
}

// ---------------------------------------------------------------- direct notices

type noticePayload struct {
	SubscriptionID string               `json:"subscription_id"`
	EventID        string               `json:"event_id"`
	Status         string               `json:"status"`
	Tone           string               `json:"tone"`
	Facts          []invoicing.EmailRow `json:"facts"`
	Want           string               `json:"want"` // the subscription status the notice is about (active)
}

// queueDirect puts a direct client notice in Pending review.
func (s *Service) queueDirect(org string, sub *Subscription, to, name, subject, message, status, tone string, facts []invoicing.EmailRow, eventID string) (string, error) {
	cu, _ := s.customer(org, sub.CustomerID)
	it := &outbox.Item{OrganizationID: org, Kind: outbox.KindSubscriptionNotice, Module: "subscriptions", Channel: outbox.ChanEmail, To: to, ToName: name,
		Subject: subject, Message: message, Editable: []string{outbox.FieldTo, outbox.FieldSubject, outbox.FieldMessage},
		Payload:    outbox.Payload(noticePayload{SubscriptionID: sub.ID, EventID: eventID, Status: status, Tone: tone, Facts: facts, Want: "active"}),
		RecordType: "subscription", RecordID: sub.ID, RecordNumber: sub.Number, RecordLabel: strings.TrimSpace("Subscription " + sub.Number),
		RecordLink: subLink(sub.ID, ""), CustomerID: sub.CustomerID, CustomerName: firstNonEmpty(cu.Name, name),
		Trigger: "Automatic: payment received — " + serviceName(sub) + " is active again", TriggerKind: outbox.TriggerAutomatic,
		Context: []outbox.ContextLine{{Label: "Subscription", Value: strings.TrimSpace(sub.Number + " · " + sub.Name)}}}
	if eventID != "" {
		it.Origin, it.OriginRef, it.Key = "subscriptions", eventID, "subscriptions:"+eventID
	}
	q, err := outbox.Enqueue(nil, it)
	if err != nil {
		return "", err
	}
	return q.ID, nil
}

func (rr *reviewReleaser) payload(it *outbox.Item) noticePayload {
	var p noticePayload
	_ = outbox.PayloadOf(it, &p)
	if p.SubscriptionID == "" {
		p.SubscriptionID = it.RecordID
	}
	return p
}

// Recheck: is the subscription still in the state the notice is about?
func (rr *reviewReleaser) Recheck(ctx context.Context, it *outbox.Item) string {
	p := rr.payload(it)
	sub, err := rr.s.load(it.OrganizationID, p.SubscriptionID)
	if err != nil {
		return "The subscription was deleted"
	}
	switch sub.Status {
	case StatusSuspended:
		return "The subscription was suspended again"
	case StatusCancelled:
		return "The subscription was cancelled"
	}
	return ""
}

// Send emails the notice (the branded "Your services" email).
func (rr *reviewReleaser) Send(ctx context.Context, it *outbox.Item) (outbox.Result, error) {
	s := rr.s
	p := rr.payload(it)
	sub, err := s.load(it.OrganizationID, p.SubscriptionID)
	if err != nil {
		return outbox.Result{}, errors.New("the subscription was deleted")
	}
	msg := s.directMessage(it.OrganizationID, sub, it.Subject, it.Message, p.Status, p.Tone, p.Facts)
	msg.To = []string{it.To}
	if _, err := s.Mail.SendAs(ctx, it.OrganizationID, msg); err != nil {
		return outbox.Result{}, err
	}
	return outbox.Result{Text: "Sent to " + it.To}, nil
}

// Preview renders the notice.
func (rr *reviewReleaser) Preview(ctx context.Context, it *outbox.Item) outbox.Preview {
	s := rr.s
	p := rr.payload(it)
	pv := outbox.Preview{Subject: it.Subject, To: it.To, Text: it.Message}
	sub, err := s.load(it.OrganizationID, p.SubscriptionID)
	if err != nil {
		pv.Error = "The subscription was deleted, so there is nothing to preview."
		return pv
	}
	msg := s.directMessage(it.OrganizationID, sub, it.Subject, it.Message, p.Status, p.Tone, p.Facts)
	cfg, _ := mail.Resolve(s.DB, it.OrganizationID)
	pv.HTML, pv.Text, pv.From, pv.ReplyTo = msg.HTML, msg.Text, cfg.Display(), cfg.ReplyTo
	return pv
}

// Closed: the outcome hook records it on the timeline.
func (rr *reviewReleaser) Closed(ctx context.Context, it *outbox.Item) {}

func uuidNew() string { return uuid.NewString() }

// recordWithID stores an event keeping its id (Pending review refers to it).
func (s *Service) recordWithID(org string, e Event) {
	now := s.now()
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	e.OrganizationID, e.CreatedAt, e.UpdatedAt = org, now, now
	e.Key = clip(e.Key, 250)
	e.Subject = clip(e.Subject, 480)
	if e.Status == "" {
		e.Status = "done"
	}
	s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&e)
}
