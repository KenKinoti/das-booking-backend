package schedsend

// Pending review (pkg/outbox): while the organisation reviews client
// messages, a customer delivery (email, SMS, WhatsApp — never team
// messages) is not sent at its time: it becomes a Pending-review item and the
// delivery waits ("waiting", review_id). Invoice / quote / receipt /
// reminder emails go through the invoicing send path, which queues them
// itself; the delivery then waits on that item. Approved, the item goes out
// through the same channel code as before (deliver) and the delivery and its
// run are settled; rejected or no longer needed, the delivery is skipped with
// the reason. Client occurrences are prepared up to 24 hours ahead so they
// can be approved for their time (prepareAhead).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// Waiting texts.
const (
	waitingReason = "Waiting for approval in Pending review"
	endedWaiting  = "Waiting for approval in Pending review"
)

type reviewedKey struct{}
type prepareKey struct{}

// withReviewed marks a send an approver reviewed ("Send now — I've reviewed it").
func withReviewed(ctx context.Context) context.Context {
	return context.WithValue(ctx, reviewedKey{}, true)
}

func reviewedCtx(ctx context.Context, m *Message) bool {
	if ctx != nil {
		if v, _ := ctx.Value(reviewedKey{}).(bool); v {
			return true
		}
	}
	return m.Reviewed && !m.Repeat.Repeats()
}

func preparing(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(prepareKey{}).(bool)
	return v
}

// gate decides for one customer delivery.
func (s *Service) gate(ctx context.Context, m *Message) outbox.Decision {
	if !outbox.Active() {
		return outbox.Decision{}
	}
	if preparing(ctx) {
		return outbox.Decision{Wait: true, Reviewing: true}
	}
	return outbox.Decide(m.OrganizationID, outbox.Who{UserID: m.CreatedBy}, reviewedCtx(ctx, m))
}

// reviewMeta is the "review" body of an invoicing send (invoice deliveries).
func (s *Service) reviewMeta(ctx context.Context, m *Message, d *Delivery) map[string]any {
	meta := map[string]any{"trigger": s.trigger(m), "trigger_kind": outbox.TriggerScheduled, "origin": "schedsend", "origin_ref": d.ID,
		"dated": m.Kind == KindReminder, "key": "schedsend:" + d.ID}
	occ := d.OccurrenceAt
	meta["send_at"] = occ
	if preparing(ctx) {
		meta["force"] = true
	}
	return meta
}

func (s *Service) trigger(m *Message) string {
	t := "Scheduled: " + firstNonEmpty(m.Title, kindLabel(m.Kind))
	if m.Repeat.Repeats() {
		t += " (" + m.Repeat.Describe(m.LocalSendAt) + ")"
	}
	return outbox.Clip(t, 290)
}

func kindLabel(k string) string {
	switch k {
	case KindInvoice:
		return "invoice email"
	case KindReceipt:
		return "receipt"
	case KindReminder:
		return "payment reminder"
	case KindStatement:
		return "statement of account"
	}
	return "customer message"
}

// waitOn marks a delivery as waiting on review item id.
func (s *Service) waitOn(d *Delivery, id string, r *RunResult) {
	now := Now()
	fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(map[string]any{"status": DelWaiting, "reason": waitingReason, "review_id": id,
		"claimed_at": nil, "updated_at": now})
	_ = r
}

// content is one delivery's text, rendered for its recipient.
type content struct {
	Subject, Body, SMS string
	WAText             string
	WAParams           []string
	WAButton           string
	open               []OpenInvoice
	unsub              string
	team               bool
}

// render fills the message's merge fields for one recipient.
func (s *Service) render(m *Message, vals map[string]string, open []OpenInvoice, unsub string, team bool) content {
	c := content{open: open, unsub: unsub, team: team}
	c.Subject, _ = Render(m.Subject, vals)
	c.Body, _ = Render(m.Body, vals)
	short := firstNonEmpty(m.SMSBody, m.Body)
	c.SMS, _ = Render(short, vals)
	c.WAText = c.SMS
	if unsub != "" {
		c.SMS += " Reply STOP to opt out."
	}
	c.WAParams = make([]string, 0, len(m.WhatsApp.Params))
	for _, p := range m.WhatsApp.Params {
		v, _ := Render(p, vals)
		c.WAParams = append(c.WAParams, wa.CleanParam(v))
	}
	c.WAButton, _ = Render(m.WhatsApp.URLButton, vals)
	c.WAButton = strings.TrimSpace(c.WAButton)
	return c
}

// deliver sends one delivery's content on its channel (the send path of
// sendOne, also used when Pending review releases it).
func (s *Service) deliver(ctx context.Context, m *Message, d *Delivery, rec Recipient, c content, fail func(error), skip func(string), sent func(string, string)) {
	org := m.OrganizationID
	switch d.Channel {
	case ChanInApp:
		_, err := notifySend(s, org, rec.ID, m, c.Subject, c.Body)
		if err != nil {
			fail(err)
			return
		}
		sent("", c.Subject+"\n\n"+c.Body)
	case ChanEmail:
		html, text := s.emailFor(org, m, c.Subject, c.Body, c.open, c.unsub, s.pixelURL(org, d.ID), c.team)
		if c.unsub != "" {
			text += "\n\nUnsubscribe from offers: " + c.unsub
		}
		id, err := s.sendMail(ctx, org, mail.Message{To: []string{d.Address}, Subject: c.Subject, HTML: html, Text: text})
		if err != nil {
			fail(err)
			return
		}
		sent(id, c.Subject+"\n\n"+c.Body)
	case ChanSMS:
		res, err := s.sendSMS(ctx, org, d.Address, c.SMS, d.ID)
		if err != nil {
			fail(err)
			return
		}
		sent(res.ID, c.SMS)
	case ChanWhatsApp:
		s.sendWhatsApp(ctx, m, d, c, fail, skip, sent)
	default:
		skip("Unknown channel")
	}
}

func outboxKindOf(m *Message) string {
	switch m.Kind {
	case KindStatement:
		return outbox.KindStatement
	case KindReminder:
		return outbox.KindPaymentReminder
	}
	return outbox.KindCustomerMessage
}

type reviewPayload struct {
	DeliveryID string `json:"delivery_id"`
	MessageID  string `json:"message_id"`
	RunID      string `json:"run_id"`
}

// queue turns a customer delivery into a Pending-review item.
func (s *Service) queue(ctx context.Context, m *Message, d *Delivery, rec Recipient, c content, r *RunResult) {
	org := m.OrganizationID
	it := &outbox.Item{OrganizationID: org, Key: "schedsend:" + d.ID, Kind: outboxKindOf(m), Module: "schedsend", Channel: d.Channel, To: d.Address, ToName: rec.Name(),
		Payload: outbox.Payload(reviewPayload{DeliveryID: d.ID, MessageID: m.ID, RunID: d.RunID}), Origin: "schedsend", OriginRef: d.ID,
		RecordType: "scheduled_message", RecordID: m.ID, RecordLabel: "Scheduled: " + firstNonEmpty(m.Title, kindLabel(m.Kind)),
		RecordLink: "/communication/scheduled?open=" + m.ID, CustomerID: rec.ID, CustomerName: rec.Name(), Trigger: s.trigger(m),
		TriggerKind: outbox.TriggerScheduled, CreatedBy: m.CreatedBy, CreatedByName: m.CreatedByName, Dated: m.Kind == KindReminder}
	occ := d.OccurrenceAt
	it.SendAt = &occ
	switch d.Channel {
	case ChanEmail:
		it.Subject, it.Message = c.Subject, c.Body
		it.Editable = []string{outbox.FieldTo, outbox.FieldSubject, outbox.FieldMessage}
	case ChanSMS:
		it.Message = c.SMS
		it.Editable = []string{outbox.FieldTo, outbox.FieldMessage}
	case ChanWhatsApp:
		it.WAText, it.WAParams, it.WATemplate, it.WALanguage, it.WAButton = c.WAText, c.WAParams, m.WhatsApp.Template, m.WhatsApp.Language, c.WAButton
		it.WAParamCount = len(c.WAParams)
		switch {
		case m.WhatsApp.Template == "":
			it.WAMode = "text"
			it.Editable = []string{outbox.FieldTo, outbox.FieldWAText}
		case m.WhatsApp.TemplateOnly || c.WAText == "":
			it.WAMode = "template"
			it.Editable = []string{outbox.FieldTo, outbox.FieldWAParams}
		default:
			it.WAMode = "auto"
			it.Editable = []string{outbox.FieldTo, outbox.FieldWAText, outbox.FieldWAParams}
		}
		it.Message = c.WAText
	}
	ctxLines := []outbox.ContextLine{{Label: "Scheduled message", Value: firstNonEmpty(m.Title, kindLabel(m.Kind))}}
	if m.Kind == KindReminder && m.InvoiceID != "" {
		inv := s.Invoice(org, m.InvoiceID)
		if inv.Found {
			amt := inv.BalanceDue
			it.Amount, it.Currency = &amt, inv.Currency
			it.RecordType, it.RecordID, it.RecordNumber = inv.DocType, inv.ID, inv.Number
			it.RecordLabel = strings.TrimSpace(map[string]string{"quote": "Quote"}[inv.DocType] + " " + inv.Number)
			if inv.DocType != "quote" {
				it.RecordLabel = "Invoice " + inv.Number
				it.RecordLink = "/invoices/" + inv.ID
			} else {
				it.RecordLink = "/quotes/" + inv.ID
			}
			ctxLines = append(ctxLines, outbox.ContextLine{Label: "Invoice", Value: fmt.Sprintf("%s · %s %0.2f still to pay", inv.Number, inv.Currency, inv.BalanceDue)})
		}
	}
	if m.Marketing {
		ctxLines = append(ctxLines, outbox.ContextLine{Label: "Marketing", Value: "Offers / promotions: carries an unsubscribe link; customers who opted out are skipped"})
	}
	it.Context = ctxLines
	q, err := outbox.Enqueue(nil, it)
	if err != nil {
		retry := true
		s.settle(d, DelFailed, "Couldn't put it in Pending review: "+err.Error(), &retry, r)
		return
	}
	s.waitOn(d, q.ID, r)
}

// ---------------------------------------------------------------- releaser

type reviewReleaser struct{ s *Service }

// RegisterReview lets Pending review send schedule deliveries.
func (s *Service) RegisterReview() {
	rr := &reviewReleaser{s: s}
	outbox.Register("schedsend", rr)
	outbox.RegisterOrigin("schedsend", rr.outcome)
}

func (rr *reviewReleaser) load(it *outbox.Item) (*Message, *Delivery, error) {
	s := rr.s
	var p reviewPayload
	_ = outbox.PayloadOf(it, &p)
	if p.DeliveryID == "" {
		p.DeliveryID = it.OriginRef
	}
	var d Delivery
	if err := fresh(s.DB).Where("id = ?", p.DeliveryID).Take(&d).Error; err != nil {
		return nil, nil, errors.New("the scheduled delivery was deleted")
	}
	var m Message
	if err := fresh(s.DB).Where("id = ?", d.MessageID).Take(&m).Error; err != nil {
		return nil, nil, errors.New("the scheduled message was deleted")
	}
	return &m, &d, nil
}

// Recheck: still worth sending?
func (rr *reviewReleaser) Recheck(ctx context.Context, it *outbox.Item) string {
	s := rr.s
	m, d, err := rr.load(it)
	if err != nil {
		msg := err.Error()
		return strings.ToUpper(msg[:1]) + msg[1:]
	}
	if m.Status == StatusCancelled {
		return "The scheduled message was cancelled"
	}
	if d.RecipientType == "customer" {
		c := s.Customer(m.OrganizationID, d.RecipientID)
		if c == nil || c.Deleted {
			return "The customer was deleted"
		}
		if m.Marketing && c.OptedOut {
			return "The customer opted out of marketing messages"
		}
	}
	if m.Kind == KindReminder || m.Kind == KindInvoice || m.Kind == KindReceipt {
		if why, _ := s.checkInvoice(m, s.Invoice(m.OrganizationID, m.InvoiceID)); why != "" {
			return why
		}
	}
	return ""
}

// recipient rebuilds the customer of a delivery.
func (rr *reviewReleaser) recipient(m *Message, d *Delivery) (Recipient, []OpenInvoice, string, error) {
	s := rr.s
	org := m.OrganizationID
	c := s.Customer(org, d.RecipientID)
	if c == nil || c.Deleted {
		return Recipient{}, nil, "", errors.New("the customer was deleted")
	}
	open := s.openInvoices(org, []string{c.ID})[c.ID]
	unsub := ""
	if m.Marketing {
		unsub = s.UnsubscribeURL(org, c.ID)
	}
	return *c, open, unsub, nil
}

func (rr *reviewReleaser) contentOf(it *outbox.Item, open []OpenInvoice, unsub string) content {
	c := content{Subject: it.Subject, Body: it.Message, SMS: it.Message, WAText: it.WAText, WAParams: append([]string{}, it.WAParams...), WAButton: it.WAButton,
		open: open, unsub: unsub}
	return c
}

// Send delivers the (possibly edited) content on its channel.
func (rr *reviewReleaser) Send(ctx context.Context, it *outbox.Item) (outbox.Result, error) {
	s := rr.s
	m, d, err := rr.load(it)
	if err != nil {
		return outbox.Result{}, err
	}
	now := Now()
	res := fresh(s.DB).Model(&Delivery{}).Where("id = ? AND status IN ?", d.ID, []string{DelWaiting, DelFailed}).
		Updates(map[string]any{"status": DelSending, "claimed_at": now, "attempts": gormExpr("attempts + 1"), "address": it.To, "updated_at": now})
	if res.RowsAffected == 0 {
		return outbox.Result{}, errors.New("the scheduled delivery isn't waiting any more (" + d.Status + ")")
	}
	d.Address, d.Status = it.To, DelSending
	rec, open, unsub, err := rr.recipient(m, d)
	if err != nil {
		s.settle(d, DelFailed, err.Error(), nil, nil)
		s.finishRun(d.RunID)
		return outbox.Result{}, err
	}
	// The WhatsApp mode chosen by the reviewer's edits.
	mm := *m
	if it.WAMode == "template" {
		mm.WhatsApp.TemplateOnly = true
	}
	if it.WAMode == "text" {
		mm.WhatsApp.Template = ""
	}
	var sendErr error
	var ref, preview string
	fail := func(e error) {
		sendErr = e
		_, why := temporary(d.Channel, e)
		s.settle(d, DelFailed, why, nil, nil)
	}
	skip := func(why string) {
		sendErr = errors.New(why)
		s.settle(d, DelSkipped, why, nil, nil)
	}
	sent := func(provider, pv string) {
		ref, preview = provider, pv
		fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(map[string]any{"provider_id": clip(provider, 190), "preview": clip(pv, 4000), "mode": d.Mode, "template": d.Template})
		s.settle(d, DelSent, "", nil, nil)
	}
	s.deliver(ctx, &mm, d, rec, rr.contentOf(it, open, unsub), fail, skip, sent)
	s.finishRun(d.RunID)
	if sendErr == nil && ref == "" {
		// Settled without the callbacks (e.g. WhatsApp isn't set up).
		var cur Delivery
		if fresh(s.DB).Where("id = ?", d.ID).Take(&cur).Error == nil && cur.Status == DelFailed {
			sendErr = errors.New(firstNonEmpty(cur.Reason, "not sent"))
		}
	}
	if sendErr != nil {
		return outbox.Result{}, sendErr
	}
	_ = preview
	text := "Sent to " + it.To
	if d.Channel == ChanWhatsApp && d.Mode == "template" {
		text += " (WhatsApp template " + d.Template + ")"
	}
	return outbox.Result{Text: text, Ref: ref}, nil
}

// Preview renders the message as the customer will get it.
func (rr *reviewReleaser) Preview(ctx context.Context, it *outbox.Item) outbox.Preview {
	s := rr.s
	pv := outbox.Preview{Subject: it.Subject, To: it.To, Text: it.Message}
	m, d, err := rr.load(it)
	if err != nil {
		pv.Error = "The preview couldn't be made: " + err.Error()
		return pv
	}
	org := m.OrganizationID
	switch it.Channel {
	case ChanEmail:
		_, open, unsub, err := rr.recipient(m, d)
		if err != nil {
			pv.Error = "The preview couldn't be made: " + err.Error()
			return pv
		}
		html, text := s.emailFor(org, m, it.Subject, it.Message, open, unsub, "", false)
		if unsub != "" {
			text += "\n\nUnsubscribe from offers: " + unsub
		}
		cfg, _ := mail.Resolve(fresh(s.DB), org)
		pv.HTML, pv.Text, pv.From, pv.ReplyTo = html, text, cfg.Display(), cfg.ReplyTo
	case ChanSMS:
		pv.Text = it.Message
	case ChanWhatsApp:
		open := s.wa().WindowOpen(org, strings.TrimPrefix(it.To, "+"), windowNow())
		pv.WindowOpen = &open
		switch {
		case it.WAMode == "text" || (it.WAMode == "auto" && open && it.WAText != ""):
			pv.Text = it.WAText
		case it.WATemplate != "":
			pv.Text = "Template " + it.WATemplate + ": " + strings.Join(it.WAParams, " · ")
			if it.WAMode == "auto" {
				pv.Text += "\n(No WhatsApp conversation with this person in the last 24 hours, so the approved template goes.)"
			}
		default:
			pv.Text = it.WAText
			pv.Error = "No WhatsApp conversation with this person in the last 24 hours and no template — it can't go until they write to you."
		}
		if a, err := s.wa().Resolve(org); err == nil {
			if acct, ok := a.(*wa.Account); ok && acct != nil {
				pv.From = "Your WhatsApp Business number (ID " + acct.PhoneNumberID + ")"
			}
		}
	}
	return pv
}

// Closed: the outcome hook settles the delivery.
func (rr *reviewReleaser) Closed(ctx context.Context, it *outbox.Item) {}

// outcome settles a waiting delivery when its item is decided (also for
// invoice emails queued by the invoicing send path).
func (rr *reviewReleaser) outcome(ctx context.Context, it *outbox.Item) {
	s := rr.s
	if it.OriginRef == "" {
		return
	}
	var d Delivery
	if fresh(s.DB).Where("id = ?", it.OriginRef).Take(&d).Error != nil {
		return
	}
	now := Now()
	q := fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID)
	switch it.Status {
	case outbox.StatusSent:
		upd := map[string]any{"status": DelSent, "reason": "", "sent_at": now, "updated_at": now, "claimed_at": nil}
		if d.RecipientType == "invoice" {
			upd["provider_id"] = clip(firstNonEmpty(it.ResultRef, it.ID), 190)
			upd["preview"] = clip(it.Subject, 4000)
			to := it.To
			if n := len(it.CC); n > 0 {
				to += fmt.Sprintf(" (+%d CC)", n)
			}
			upd["address"] = clip(to, 320)
		}
		q.Where("status IN ?", []string{DelWaiting, DelFailed}).Updates(upd)
	case outbox.StatusRejected:
		why := "Rejected in Pending review"
		if it.DecidedByName != "" {
			why += " by " + it.DecidedByName
		}
		if it.DecisionNote != "" {
			why += ": " + it.DecisionNote
		}
		q.Where("status IN ?", []string{DelWaiting, DelFailed}).Updates(map[string]any{"status": DelSkipped, "reason": clip(why, 490), "updated_at": now, "claimed_at": nil})
	case outbox.StatusSuperseded:
		q.Where("status IN ?", []string{DelWaiting, DelFailed}).Updates(map[string]any{"status": DelSkipped, "reason": clip("Not sent — "+it.SupersededReason, 490), "updated_at": now, "claimed_at": nil})
	case outbox.StatusFailed:
		q.Where("status = ?", DelWaiting).Updates(map[string]any{"status": DelFailed, "reason": clip(it.Error, 490), "updated_at": now})
	default:
		return
	}
	s.finishRun(d.RunID)
}

// ---------------------------------------------------------------- prepare ahead

// prepareAhead turns client occurrences due in the next 24 hours into
// Pending-review items now, so they can be approved for their time.
func (s *Service) prepareAhead(ctx context.Context, r *RunResult) {
	if !outbox.Active() {
		return
	}
	now := Now()
	var due []Message
	fresh(s.DB).Where("status = ? AND kind <> ? AND next_run_at > ? AND next_run_at <= ?", StatusScheduled, KindTeam, now, now.Add(outbox.PrepareAhead)).
		Order("next_run_at").Limit(25).Find(&due)
	for i := range due {
		if ctx.Err() != nil {
			return
		}
		m := due[i]
		if reviewedCtx(ctx, &m) || !outbox.Reviewing(m.OrganizationID) || has(m.Channels, ChanInApp) {
			continue
		}
		res := fresh(s.DB).Model(&Message{}).Where("id = ? AND status = ? AND next_run_at = ?", m.ID, StatusScheduled, m.NextRunAt).
			Updates(map[string]any{"status": StatusRunning, "claimed_at": now})
		if res.Error != nil || res.RowsAffected == 0 {
			continue
		}
		r.Messages++
		s.process(context.WithValue(ctx, prepareKey{}, true), &m, now, r)
	}
}

// SendNowReviewed is SendNow with the "I've reviewed it" tick of an approver.
func (s *Service) SendNowReviewed(a Actor, id string, sendNow bool) (*Message, error) {
	reviewed := sendNow && outbox.Active() && outbox.Decide(a.Org, outbox.Who{UserID: a.User, Role: a.Role}, true).SentNow
	return s.sendNow(a, id, reviewed)
}

// reviewInput applies the "I've reviewed it" tick of a create / update.
func (s *Service) reviewInput(a Actor, m *Message, in Input) string {
	want := in.Reviewed || in.SendNow
	m.Reviewed, m.ReviewedBy, m.ReviewedByName, m.ReviewedAt = false, "", "", nil
	if !want || !outbox.Active() {
		return ""
	}
	if m.Repeat.Repeats() {
		return "Repeating messages are reviewed each time in Pending review — the tick only applies to one-off messages."
	}
	if !outbox.Decide(a.Org, outbox.Who{UserID: a.User, Role: a.Role}, true).SentNow {
		return "Only owners and admins can mark a message as reviewed — it will wait in Pending review."
	}
	now := Now()
	m.Reviewed, m.ReviewedBy, m.ReviewedByName, m.ReviewedAt = true, a.User, firstNonEmpty(a.Name, a.Email), &now
	return ""
}

var _ = time.Now
