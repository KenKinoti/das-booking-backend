package schedsend

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

const lockKey = "schedsend:engine"

// Retry backoff for temporary errors (attempt 1 failed → wait 2 min …).
var retryBackoff = []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// MaxAttempts per delivery.
const MaxAttempts = 4

// Stale claims: a message or delivery claimed this long ago by an engine that
// stopped (crash, deploy) is released.
const (
	staleMessage  = 15 * time.Minute
	staleDelivery = 10 * time.Minute
)

// RunResult summarises one engine pass.
type RunResult struct {
	Ran      bool `json:"ran"`
	Messages int  `json:"messages"`
	Sent     int  `json:"sent"`
	Failed   int  `json:"failed"`
	Skipped  int  `json:"skipped"`
	Missed   int  `json:"missed"`
	Retried  int  `json:"retried"`
}

// withLock runs fn while holding the advisory lock (one leader across
// instances); wait > 0 keeps trying that long, 0 gives up at once.
func (s *Service) withLock(ctx context.Context, wait time.Duration, fn func()) (bool, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	got := false
	err := fresh(s.DB).WithContext(ctx).Connection(func(tx *gorm.DB) error {
		end := time.Now().Add(wait)
		for {
			if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", lockKey).Scan(&got).Error; err != nil {
				return err
			}
			if got || time.Now().After(end) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		if !got {
			return nil
		}
		defer tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", lockKey)
		fn()
		return nil
	})
	return got, err
}

// RunOnce sends what is due, then the retries — under the lock.
func (s *Service) RunOnce(ctx context.Context, wait time.Duration) (RunResult, error) {
	var r RunResult
	ok, err := s.withLock(ctx, wait, func() {
		r.Ran = true
		s.releaseStale()
		s.sendDue(ctx, &r)
		s.prepareAhead(ctx, &r) // Pending review: client occurrences of the next 24 h (review.go)
		s.retryDue(ctx, &r)
	})
	if err == nil && !ok {
		err = errors.New("another server is sending scheduled messages right now — try again in a moment")
	}
	return r, err
}

// Start runs the engine every interval.
func (s *Service) Start(ctx context.Context, every, firstDelay time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	wa.OnStatus(s.onWhatsAppStatus)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(firstDelay):
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("schedsend: run failed: %v", rec)
					}
				}()
				r, err := s.RunOnce(ctx, 0)
				if err == nil && r.Messages+r.Retried > 0 {
					log.Printf("schedsend: %d message(s): %d sent, %d failed, %d skipped, %d missed, %d retried", r.Messages, r.Sent, r.Failed, r.Skipped, r.Missed, r.Retried)
				}
			}()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// releaseStale frees work an engine claimed and never finished.
func (s *Service) releaseStale() {
	now := Now()
	fresh(s.DB).Model(&Message{}).Where("status = ? AND claimed_at < ?", StatusRunning, now.Add(-staleMessage)).
		Updates(map[string]any{"status": StatusScheduled, "claimed_at": nil})
	// A delivery claimed but never confirmed may or may not have gone out:
	// it is not sent again (nobody gets a message twice).
	var stuck []Delivery
	fresh(s.DB).Where("status = ? AND claimed_at < ?", DelSending, now.Add(-staleDelivery)).Limit(500).Find(&stuck)
	for _, d := range stuck {
		fresh(s.DB).Model(&Delivery{}).Where("id = ? AND status = ?", d.ID, DelSending).Updates(map[string]any{"status": DelFailed,
			"reason": "Interrupted while sending (the server restarted) — not sent again so nobody gets it twice; check before resending", "updated_at": now})
		s.finishRun(d.RunID)
	}
}

func (s *Service) sendDue(ctx context.Context, r *RunResult) {
	now := Now()
	var due []Message
	fresh(s.DB).Where("status = ? AND next_run_at <= ?", StatusScheduled, now).Order("next_run_at").Limit(25).Find(&due)
	for i := range due {
		if ctx.Err() != nil {
			return
		}
		m := due[i]
		// Claim it (another engine or "Send now" may have it).
		res := fresh(s.DB).Model(&Message{}).Where("id = ? AND status = ? AND next_run_at = ?", m.ID, StatusScheduled, m.NextRunAt).
			Updates(map[string]any{"status": StatusRunning, "claimed_at": now})
		if res.Error != nil || res.RowsAffected == 0 {
			continue
		}
		r.Messages++
		s.process(ctx, &m, now, r)
	}
}

// process handles the due occurrence of m (claimed): missed, or sent.
func (s *Service) process(ctx context.Context, m *Message, now time.Time, r *RunResult) {
	sch := Schedule{Start: m.LocalSendAt, TZ: m.Timezone, Repeat: m.Repeat}
	occ := *m.NextRunAt
	done := m.Occurrences
	ended := ""
	missed := 0
	// Catch-up: an occurrence more than CatchUpWindow late is missed; the
	// schedule moves on to the next one (missed ones are never sent in a burst).
	for now.Sub(occ) > CatchUpWindow {
		missed++
		done++
		if missed <= 10 {
			s.recordMissed(m, occ, now)
		}
		nx, ok := sch.After(occ, done)
		if !ok {
			ended = "Missed: the server was down at the scheduled time (more than 6 hours late)"
			break
		}
		occ = nx
	}
	if missed > 0 {
		r.Missed += missed
		s.notifyMissed(m, missed, occ, ended == "")
		if ended != "" {
			s.advance(m, done, nil, ended, RunMissed, "Missed while the server was down")
			return
		}
		if occ.After(now) {
			s.advance(m, done, &occ, "", RunMissed, "Missed while the server was down")
			return
		}
	}
	run, stop := s.runOccurrence(ctx, m, occ, false, r)
	done++
	if stop != "" {
		s.advance(m, done, nil, stop, run.Status, run.Note)
		return
	}
	nx, ok := sch.After(occ, done)
	if !ok {
		ended := ""
		if run.Status == RunWaiting {
			ended = endedWaiting // Pending review: it goes once approved
		}
		s.advance(m, done, nil, ended, run.Status, run.Note)
		return
	}
	s.advance(m, done, &nx, "", run.Status, run.Note)
}

// advance moves m to its next occurrence (nil = it has ended).
func (s *Service) advance(m *Message, done int, next *time.Time, ended, result, note string) {
	now := Now()
	upd := map[string]any{"occurrences": done, "claimed_at": nil, "last_result": result, "last_run_at": now, "last_note": clip(note, 490), "updated_at": now}
	if next == nil {
		upd["status"] = StatusDone
		upd["next_run_at"] = nil
		if ended == "" {
			if m.Repeat.Repeats() {
				ended = "Schedule finished"
			} else {
				ended = "Sent"
			}
		}
		upd["ended_reason"] = clip(ended, 290)
	} else {
		upd["status"] = StatusScheduled
		upd["next_run_at"] = *next
	}
	// Only if still ours (a cancel during the send wins).
	res := fresh(s.DB).Model(&Message{}).Where("id = ? AND status = ?", m.ID, StatusRunning).Updates(upd)
	if res.RowsAffected == 0 {
		delete(upd, "status")
		delete(upd, "next_run_at")
		delete(upd, "ended_reason")
		fresh(s.DB).Model(&Message{}).Where("id = ?", m.ID).Updates(upd)
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (s *Service) recordMissed(m *Message, occ, now time.Time) {
	run := Run{ID: uuid.NewString(), MessageID: m.ID, OrganizationID: m.OrganizationID, OccurrenceAt: occ, Status: RunMissed,
		Note:      fmt.Sprintf("Missed: due %s (%s), the server was down until %s", FormatLocal(occ, m.Timezone), m.Timezone, FormatLocal(now, m.Timezone)),
		StartedAt: now, FinishedAt: &now, Notified: true}
	fresh(s.DB).Clauses(clause.OnConflict{DoNothing: true}).Create(&run)
}

// ---------------------------------------------------------------- runs

// runOccurrence sends one occurrence (resuming a run that was interrupted).
// stop is set when the whole schedule should end (an invoice was paid …).
func (s *Service) runOccurrence(ctx context.Context, m *Message, occ time.Time, manual bool, r *RunResult) (Run, string) {
	now := Now()
	run := Run{ID: uuid.NewString(), MessageID: m.ID, OrganizationID: m.OrganizationID, OccurrenceAt: occ, Manual: manual, Status: RunSending, StartedAt: now}
	fresh(s.DB).Clauses(clause.OnConflict{DoNothing: true}).Create(&run)
	var cur Run
	if err := fresh(s.DB).Where("message_id = ? AND occurrence_at = ?", m.ID, occ).Take(&cur).Error; err != nil {
		return Run{Status: RunFailed, Note: err.Error()}, ""
	}
	if cur.Status != RunSending {
		return cur, "" // already handled (idempotent)
	}
	stop := s.expand(m, &cur)
	s.sendPending(ctx, m, &cur, r)
	fin := s.finishRun(cur.ID)
	if fin != nil {
		cur = *fin
	} else {
		fresh(s.DB).Where("id = ?", cur.ID).Take(&cur) // still open (waiting for approval)
	}
	return cur, stop
}

// expand writes one delivery per recipient and channel (once per run).
func (s *Service) expand(m *Message, run *Run) string {
	var n int64
	fresh(s.DB).Model(&Delivery{}).Where("run_id = ?", run.ID).Count(&n)
	if n > 0 {
		return s.stopReason(m)
	}
	org := m.OrganizationID
	now := Now()
	rows := []Delivery{}
	add := func(key, typ, id, name, addr, channel, status, reason string) {
		rows = append(rows, Delivery{ID: uuid.NewString(), RunID: run.ID, MessageID: m.ID, OrganizationID: org, OccurrenceAt: run.OccurrenceAt,
			RecipientKey: key, RecipientType: typ, RecipientID: id, Name: clip(name, 250), Address: clip(addr, 320), Channel: channel,
			Status: status, Reason: clip(reason, 490), CreatedAt: now, UpdatedAt: now})
	}
	stop := ""
	switch m.Kind {
	case KindInvoice, KindReceipt, KindReminder:
		inv := s.Invoice(org, m.InvoiceID)
		skip, end := s.checkInvoice(m, inv)
		if end {
			stop = skip
		}
		to := firstNonEmpty(m.InvoiceSend.To, inv.ClientEmail)
		if has(m.Channels, ChanEmail) {
			st := DelPending
			if skip != "" {
				st = DelSkipped
			}
			add("invoice:"+m.InvoiceID, "invoice", m.InvoiceID, firstNonEmpty(inv.ClientName, m.CustomerName), to, ChanEmail, st, skip)
		}
		if m.Kind == KindReminder && (has(m.Channels, ChanSMS) || has(m.Channels, ChanWhatsApp)) {
			c := s.Customer(org, inv.CustomerID)
			for _, ch := range []string{ChanSMS, ChanWhatsApp} {
				if !has(m.Channels, ch) {
					continue
				}
				if c == nil {
					add("customer:"+inv.CustomerID, "customer", inv.CustomerID, inv.ClientName, "", ch, DelSkipped, firstNonEmpty(skip, "The invoice isn't linked to a customer"))
					continue
				}
				addr, why := s.phoneOf(org, *c)
				st, reason := DelPending, why
				if skip != "" {
					st, reason = DelSkipped, skip
				} else if c.Deleted {
					st, reason = DelSkipped, "The customer was deleted"
				} else if why != "" {
					st = DelSkipped
				}
				add(c.Key(), "customer", c.ID, c.Name(), addr, ch, st, reason)
			}
		}
	case KindCustomer, KindStatement:
		recips, err := s.Customers(org, m.Audience)
		if err != nil {
			run.Note = err.Error()
			break
		}
		var open map[string][]OpenInvoice
		if m.Kind == KindStatement && m.Audience.SkipZeroBalance {
			ids := []string{}
			for _, c := range recips {
				ids = append(ids, c.ID)
			}
			open = s.openInvoices(org, ids)
		}
		for _, c := range recips {
			for _, ch := range m.Channels {
				addr, why := s.target(m, c, ch, open)
				st := DelPending
				if why != "" {
					st = DelSkipped
				}
				add(c.Key(), "customer", c.ID, firstNonEmpty(c.Name(), "Deleted customer"), addr, ch, st, why)
			}
		}
	case KindTeam:
		people, err := s.Staff(org, m.Audience)
		if err != nil {
			run.Note = err.Error()
			break
		}
		for _, u := range people {
			for _, ch := range m.Channels {
				addr, why := s.target(m, u, ch, nil)
				st := DelPending
				if why != "" {
					st = DelSkipped
				}
				add(u.Key(), "user", u.ID, u.Name(), addr, ch, st, why)
			}
		}
	}
	if len(rows) > 0 {
		fresh(s.DB).Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 200)
	}
	if run.Note != "" {
		fresh(s.DB).Model(&Run{}).Where("id = ?", run.ID).Update("note", clip(run.Note, 490))
	}
	return stop
}

func (s *Service) stopReason(m *Message) string {
	if m.Kind != KindReminder {
		return ""
	}
	skip, end := s.checkInvoice(m, s.Invoice(m.OrganizationID, m.InvoiceID))
	if end {
		return skip
	}
	return ""
}

// phoneOf is the recipient's number in E.164 ("" + why when unusable).
func (s *Service) phoneOf(org string, r Recipient) (string, string) {
	if strings.TrimSpace(r.Phone) == "" {
		return "", "No phone number"
	}
	country := firstNonEmpty(r.Country, s.orgCountry(org), "KE")
	p, err := E164(r.Phone, country)
	if err != nil {
		return r.Phone, err.Error()
	}
	return p, ""
}

// checkInvoice re-checks an invoice before sending: skip says why it must
// not go out; end means the whole schedule stops (reminders: paid, void …).
func (s *Service) checkInvoice(m *Message, inv InvoiceState) (skip string, end bool) {
	word := "invoice"
	if inv.DocType == "quote" {
		word = "quote"
	}
	switch {
	case !inv.Found || inv.Deleted:
		return "The " + word + " was deleted", true
	case inv.Status == "void":
		return "The " + word + " was voided", true
	case inv.Status == "converted":
		return "The quote was converted to an invoice", true
	}
	if inv.CustomerID != "" {
		if c := s.Customer(m.OrganizationID, inv.CustomerID); c != nil && c.Deleted {
			return "The customer was deleted", true
		}
	}
	paid := inv.DocType == "invoice" && (inv.Status == "paid" || (inv.Total > 0 && inv.BalanceDue <= 0 && inv.Status != "draft"))
	switch m.Kind {
	case KindInvoice:
		if paid {
			return "Already paid in full", true
		}
		if inv.DocType == "quote" {
			switch inv.Status {
			case "accepted":
				return "The quote was already accepted", true
			case "declined":
				return "The quote was declined", true
			}
		}
	case KindReminder:
		if paid {
			return "Paid — reminders stopped", true
		}
		if inv.DocType == "quote" && inv.Status == "accepted" {
			return "The quote was accepted — reminders stopped", true
		}
		if inv.Status == "draft" {
			return "The invoice is still a draft (send it first)", false
		}
	case KindReceipt:
		if !s.paymentExists(m.OrganizationID, inv.ID, m.PaymentID) {
			return "The payment was removed", true
		}
	}
	return "", false
}

// sendPending sends every pending (and due retry) delivery of run.
func (s *Service) sendPending(ctx context.Context, m *Message, run *Run, r *RunResult) {
	var list []Delivery
	fresh(s.DB).Where("run_id = ? AND (status = ? OR (status = ? AND next_attempt_at <= ?))", run.ID, DelPending, DelRetry, Now()).
		Order("created_at, id").Find(&list)
	gap := s.Gap
	if gap == 0 {
		gap = 150 * time.Millisecond
	}
	var who *Actor
	for i := range list {
		if ctx.Err() != nil {
			return
		}
		if i > 0 && gap > 0 && list[i].Channel != ChanInApp {
			time.Sleep(gap) // a small rate limit for bulk sends
		}
		d := list[i]
		now := Now()
		res := fresh(s.DB).Model(&Delivery{}).Where("id = ? AND status IN ?", d.ID, []string{DelPending, DelRetry}).
			Updates(map[string]any{"status": DelSending, "claimed_at": now, "attempts": gorm.Expr("attempts + 1"), "updated_at": now})
		if res.RowsAffected == 0 {
			continue // someone else has it
		}
		d.Attempts++
		if who == nil && (m.Kind == KindInvoice || m.Kind == KindReceipt || m.Kind == KindReminder) {
			a, err := s.creator(m)
			if err != nil {
				s.settle(&d, DelFailed, err.Error(), nil, r)
				continue
			}
			who = &a
		}
		s.sendOne(ctx, m, &d, who, r)
	}
}

// creator is the person who scheduled m, as they are now.
func (s *Service) creator(m *Message) (Actor, error) {
	var u []struct {
		ID, Email, Role, OrganizationID string
		Active                          bool
	}
	fresh(s.DB).Raw(`SELECT id, COALESCE(email,'') AS email, COALESCE(role,'') AS role, COALESCE(organization_id,'') AS organization_id,
		(deleted_at IS NULL AND (is_active IS NULL OR is_active)) AS active FROM users WHERE id = ?`, m.CreatedBy).Scan(&u)
	if len(u) == 0 || !u[0].Active {
		return Actor{}, fmt.Errorf("%s, who scheduled this, no longer has an active account — duplicate it to send it as yourself", firstNonEmpty(m.CreatedByName, "The person"))
	}
	org := m.OrganizationID
	if u[0].Role != "super_admin" && u[0].OrganizationID != org {
		return Actor{}, errors.New("the person who scheduled this is no longer in this organisation")
	}
	return Actor{Org: org, User: u[0].ID, Role: u[0].Role, Email: u[0].Email, Name: m.CreatedByName}, nil
}

// settle records a delivery outcome (retrying temporary failures).
func (s *Service) settle(d *Delivery, status, reason string, retry *bool, r *RunResult) {
	now := Now()
	upd := map[string]any{"status": status, "reason": clip(reason, 490), "claimed_at": nil, "updated_at": now}
	if status == DelFailed && retry != nil && *retry && d.Attempts < MaxAttempts {
		wait := retryBackoff[minInt(d.Attempts-1, len(retryBackoff)-1)]
		upd["status"] = DelRetry
		upd["next_attempt_at"] = now.Add(wait)
		upd["reason"] = clip(reason+" — will try again at "+now.Add(wait).Format("15:04")+" UTC", 490)
		status = DelRetry
	}
	if status == DelSent {
		upd["sent_at"] = now
		upd["next_attempt_at"] = nil
	}
	fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(upd)
	if r != nil {
		switch status {
		case DelSent:
			r.Sent++
		case DelFailed:
			r.Failed++
		case DelSkipped:
			r.Skipped++
		}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sendOne sends one claimed delivery.
func (s *Service) sendOne(ctx context.Context, m *Message, d *Delivery, who *Actor, r *RunResult) {
	org := m.OrganizationID
	fail := func(err error) {
		retry, why := temporary(d.Channel, err)
		s.settle(d, DelFailed, why, &retry, r)
	}
	skip := func(why string) { s.settle(d, DelSkipped, why, nil, r) }
	sent := func(provider, preview string) {
		fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(map[string]any{"provider_id": clip(provider, 190), "preview": clip(preview, 4000), "mode": d.Mode, "template": d.Template})
		s.settle(d, DelSent, "", nil, r)
	}

	// Invoice emails: the invoicing send path, checked again just before.
	if d.RecipientType == "invoice" {
		inv := s.Invoice(org, m.InvoiceID)
		if why, _ := s.checkInvoice(m, inv); why != "" {
			skip(why)
			return
		}
		kind := m.Kind
		if kind == KindInvoice && inv.DocType == "quote" {
			kind = "quote"
		}
		snd := m.InvoiceSend
		body := map[string]any{"kind": kind, "mode": "email", "subject": snd.Subject, "message": snd.Message,
			"app_url": firstNonEmpty(snd.AppURL, s.AppURL), "accept_warnings": snd.AcceptWarnings}
		if m.Kind == KindReceipt {
			body["payment_id"] = m.PaymentID
		}
		if snd.Set {
			body["to"], body["cc"], body["bcc"] = snd.To, nonNil(snd.CC), nonNil(snd.BCC)
		}
		// Pending review (review.go): the invoicing send path queues it.
		if outbox.Active() {
			body["review"] = s.reviewMeta(ctx, m, d)
			if reviewedCtx(ctx, m) {
				body["send_now"] = true
			}
		}
		res, err := s.Invoices.Send(ctx, *who, m.InvoiceID, body)
		if err != nil {
			fail(err)
			return
		}
		if res.Queued {
			s.waitOn(d, res.ReviewID, r)
			return
		}
		if !res.Emailed {
			s.settle(d, DelFailed, "Email isn't set up for this organisation, so it wasn't sent", nil, r)
			return
		}
		to := res.To
		if n := len(res.CC); n > 0 {
			to += fmt.Sprintf(" (+%d CC)", n)
		}
		fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Update("address", clip(to, 320))
		sent(res.LogID, res.Subject)
		return
	}

	// Customer / staff messages: merge values per recipient.
	var rec Recipient
	var open []OpenInvoice
	var focus *OpenInvoice
	team := d.RecipientType == "user"
	if team {
		people, _ := s.Staff(org, Audience{UserIDs: []string{d.RecipientID}})
		if len(people) == 0 {
			skip("The person no longer has an active account")
			return
		}
		rec = people[0]
	} else {
		c := s.Customer(org, d.RecipientID)
		if c == nil || c.Deleted {
			skip("The customer was deleted")
			return
		}
		if m.Marketing && c.OptedOut {
			skip("Opted out of marketing messages")
			return
		}
		rec = *c
		open = s.openInvoices(org, []string{rec.ID})[rec.ID]
		if m.Kind == KindReminder {
			inv := s.Invoice(org, m.InvoiceID)
			if why, _ := s.checkInvoice(m, inv); why != "" {
				skip(why)
				return
			}
			focus = &OpenInvoice{ID: inv.ID, Number: inv.Number, Currency: inv.Currency, BalanceDue: inv.BalanceDue, DueDate: inv.DueDate, PublicToken: inv.PublicToken}
		}
	}
	unsub := ""
	if m.Marketing && !team {
		unsub = s.UnsubscribeURL(org, rec.ID)
	}
	vals := s.Values(org, rec, open, focus, unsub)
	c := s.render(m, vals, open, unsub, team)

	// Pending review (review.go): client messages wait for approval; team
	// messages never do.
	if !team && d.Channel != ChanInApp {
		dec := s.gate(ctx, m)
		if dec.Wait {
			s.queue(ctx, m, d, rec, c, r)
			return
		}
		if dec.SentNow {
			sentNow := sent
			sent = func(provider, preview string) {
				sentNow(provider, preview)
				outbox.SentNow(org, m.CreatedBy, "scheduled_message", m.ID, "Scheduled "+kindLabel(m.Kind), d.Address)
			}
		}
	}
	s.deliver(ctx, m, d, rec, c, fail, skip, sent)
}

// notifySend is the in-app channel of team messages.
func notifySend(s *Service, org, userID string, m *Message, title, body string) (int, error) {
	return notify.Send(fresh(s.DB), org, notify.Users(userID), notify.Message{Type: "message.team", Severity: notify.Info, Title: title, Body: body,
		Link: "/notifications", EntityType: "scheduled_message", EntityID: m.ID, ActorID: m.CreatedBy, ActorName: m.CreatedByName})
}

func gormExpr(expr string) any { return gorm.Expr(expr) }

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// sendWhatsApp: free text inside the 24-hour window, else the chosen
// template, else skipped with the reason.
func (s *Service) sendWhatsApp(ctx context.Context, m *Message, d *Delivery, c content, fail func(error), skip func(string), sent func(string, string)) {
	org := m.OrganizationID
	acct, err := s.wa().Resolve(org)
	if err != nil {
		s.settle(d, DelFailed, err.Error(), nil, nil)
		return
	}
	to := strings.TrimPrefix(d.Address, "+")
	text := c.WAText
	record := func(res *wa.Result, kind, tpl string) {
		var a *wa.Account
		if x, ok := acct.(*wa.Account); ok {
			a = x
		}
		wa.Record(fresh(s.DB), org, a, to, res, kind, tpl, "schedule", d.ID)
	}
	if text != "" && !m.WhatsApp.TemplateOnly && s.wa().WindowOpen(org, to, windowNow()) {
		res, err := acct.SendText(ctx, to, text, true)
		if err == nil {
			d.Mode = "text"
			record(res, "text", "")
			sent(res.MessageID, text)
			return
		}
		var we *wa.Error
		if !errors.As(err, &we) || !we.OutsideWindow() {
			fail(err)
			return
		}
		// The window closed in the meantime: fall back to the template.
	}
	if m.WhatsApp.Template == "" {
		skip("No WhatsApp conversation with this person in the last 24 hours and no template chosen — WhatsApp only allows an approved template")
		return
	}
	params := c.WAParams
	if params == nil {
		params = []string{}
	}
	res, err := acct.SendTemplate(ctx, to, wa.Template{Name: m.WhatsApp.Template, Language: m.WhatsApp.Language, BodyParams: params, URLButton: strings.TrimSpace(c.WAButton)})
	if err != nil {
		fail(err)
		return
	}
	d.Mode, d.Template = "template", m.WhatsApp.Template
	record(res, "template", m.WhatsApp.Template)
	sent(res.MessageID, "Template "+m.WhatsApp.Template+": "+strings.Join(params, " · "))
}

// finishRun closes a run once nothing is pending; returns it (nil = still open).
func (s *Service) finishRun(runID string) *Run {
	var run Run
	if fresh(s.DB).Where("id = ?", runID).Take(&run).Error != nil {
		return nil
	}
	type cnt struct {
		Status string
		N      int
	}
	var rows []cnt
	fresh(s.DB).Model(&Delivery{}).Select("status, COUNT(*) AS n").Where("run_id = ?", runID).Group("status").Scan(&rows)
	c := map[string]int{}
	total := 0
	for _, x := range rows {
		c[x.Status] = x.N
		total += x.N
	}
	sentN := c[DelSent] + c[DelDelivered] + c[DelRead]
	run.Total, run.Sent, run.Failed, run.Skipped = total, sentN, c[DelFailed], c[DelSkipped]
	open := c[DelPending] + c[DelSending] + c[DelRetry]
	waiting := c[DelWaiting] // Pending review (review.go)
	upd := map[string]any{"total": run.Total, "sent": run.Sent, "failed": run.Failed, "skipped": run.Skipped}
	if open == 0 && waiting > 0 && (run.Status == RunSending || run.Status == RunWaiting) {
		// Everything else is done; the rest waits for approval.
		note := fmt.Sprintf("%d waiting for approval in Pending review", waiting)
		if p := runNote(run, ""); p != "" {
			note = p + ", " + note
		}
		upd["status"], upd["note"] = RunWaiting, clip(note, 490)
		fresh(s.DB).Model(&Run{}).Where("id = ? AND status IN ?", runID, []string{RunSending, RunWaiting}).Updates(upd)
		fresh(s.DB).Model(&Message{}).Where("id = ? AND (last_run_at IS NULL OR last_run_at <= ?)", run.MessageID, Now()).
			Updates(map[string]any{"last_result": RunWaiting, "last_note": clip(note, 490)})
		return nil
	}
	if open > 0 || (run.Status != RunSending && run.Status != RunWaiting) {
		fresh(s.DB).Model(&Run{}).Where("id = ?", runID).Updates(upd)
		if open > 0 {
			return nil
		}
		return &run
	}
	switch {
	case total == 0:
		run.Status = RunSkipped
		if run.Note == "" {
			run.Note = "Nobody to send to"
		}
	case run.Failed > 0 && sentN > 0:
		run.Status = RunPartial
	case run.Failed > 0:
		run.Status = RunFailed
	case sentN > 0:
		run.Status = RunSent
	default:
		run.Status = RunSkipped
	}
	if run.Note == "" {
		run.Note = runNote(run, s.firstReason(runID))
	}
	now := Now()
	run.FinishedAt = &now
	upd["status"], upd["finished_at"], upd["note"] = run.Status, now, clip(run.Note, 490)
	res := fresh(s.DB).Model(&Run{}).Where("id = ? AND status IN ?", runID, []string{RunSending, RunWaiting}).Updates(upd)
	if res.RowsAffected == 0 {
		return &run
	}
	// A one-off that waited for approval has now really finished.
	fresh(s.DB).Model(&Message{}).Where("id = ? AND ended_reason = ?", run.MessageID, endedWaiting).
		Update("ended_reason", map[bool]string{true: "Sent", false: clip(run.Note, 290)}[run.Status == RunSent])
	// Recurring messages already moved on; keep the last result current.
	fresh(s.DB).Model(&Message{}).Where("id = ? AND (last_run_at IS NULL OR last_run_at <= ?)", run.MessageID, now).
		Updates(map[string]any{"last_result": run.Status, "last_note": clip(run.Note, 490), "last_run_at": now})
	if run.Failed > 0 {
		s.notifyFailed(&run)
	}
	return &run
}

func (s *Service) firstReason(runID string) string {
	var d Delivery
	if fresh(s.DB).Where("run_id = ? AND status IN ?", runID, []string{DelFailed, DelSkipped}).Order("CASE WHEN status = 'failed' THEN 0 ELSE 1 END, created_at").
		Limit(1).Find(&d).Error != nil || d.ID == "" {
		return ""
	}
	return d.Reason
}

func runNote(r Run, reason string) string {
	parts := []string{}
	if r.Sent > 0 {
		parts = append(parts, fmt.Sprintf("%d sent", r.Sent))
	}
	if r.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", r.Failed))
	}
	if r.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", r.Skipped))
	}
	n := strings.Join(parts, ", ")
	if reason != "" && (r.Failed > 0 || r.Sent == 0) {
		n += " — " + reason
	}
	return n
}

// retryDue sends deliveries whose retry time has come.
func (s *Service) retryDue(ctx context.Context, r *RunResult) {
	var runs []string
	fresh(s.DB).Model(&Delivery{}).Distinct("run_id").Where("status = ? AND next_attempt_at <= ?", DelRetry, Now()).Limit(50).Pluck("run_id", &runs)
	for _, id := range runs {
		var run Run
		if fresh(s.DB).Where("id = ?", id).Take(&run).Error != nil {
			continue
		}
		var m Message
		if fresh(s.DB).Where("id = ?", run.MessageID).Take(&m).Error != nil {
			continue
		}
		if m.Status == StatusCancelled {
			fresh(s.DB).Model(&Delivery{}).Where("run_id = ? AND status = ?", id, DelRetry).
				Updates(map[string]any{"status": DelSkipped, "reason": "Cancelled before the retry", "updated_at": Now()})
			s.finishRun(id)
			continue
		}
		r.Retried++
		s.sendPending(ctx, &m, &run, r)
		s.finishRun(id)
	}
}

// ---------------------------------------------------------------- send now

// SendNow sends a message immediately (in the background): a one-off is then
// done; a recurring one keeps its schedule.
func (s *Service) SendNow(a Actor, id string) (*Message, error) { return s.sendNow(a, id, false) }

// sendNow is SendNow; reviewed: an approver ticked "I've reviewed it" (the
// client deliveries don't wait in Pending review).
func (s *Service) sendNow(a Actor, id string, reviewed bool) (*Message, error) {
	m, err := s.Get(a.Org, id)
	if err != nil {
		return nil, err
	}
	if err := s.canManage(a, m); err != nil {
		return nil, err
	}
	prev := m.Status
	if prev != StatusScheduled && prev != StatusPaused {
		return nil, &Error{409, "CONFLICT", "It can't be sent now (it is " + m.Status + ")."}
	}
	now := Now()
	res := fresh(s.DB).Model(&Message{}).Where("id = ? AND status = ?", id, prev).Updates(map[string]any{"status": StatusRunning, "claimed_at": now})
	if res.RowsAffected == 0 {
		return nil, &Error{409, "BUSY", "It is being sent right now."}
	}
	occ := now.Truncate(time.Second)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("schedsend: send now: %v", rec)
			}
		}()
		var r RunResult
		ctx := context.Background()
		if reviewed {
			ctx = withReviewed(ctx)
		}
		run, stop := s.runOccurrence(ctx, m, occ, true, &r)
		if !m.Repeat.Repeats() {
			s.advance(m, m.Occurrences+1, nil, firstNonEmpty(stop, "Sent now by "+firstNonEmpty(a.Name, a.Email)), run.Status, run.Note)
			return
		}
		upd := map[string]any{"status": prev, "claimed_at": nil, "last_result": run.Status, "last_run_at": Now(), "last_note": clip(run.Note, 490)}
		if stop != "" {
			upd["status"], upd["next_run_at"], upd["ended_reason"] = StatusDone, nil, stop
		}
		fresh(s.DB).Model(&Message{}).Where("id = ? AND status = ?", m.ID, StatusRunning).Updates(upd)
	}()
	m.Status = StatusRunning
	return m, nil
}

// ---------------------------------------------------------------- notices

func (s *Service) tellCreator(m *Message, msg notify.Message) {
	target := notify.Users(m.CreatedBy)
	var active int64
	fresh(s.DB).Raw(`SELECT COUNT(*) FROM users WHERE id = ? AND deleted_at IS NULL AND (is_active IS NULL OR is_active)`, m.CreatedBy).Scan(&active)
	if active == 0 {
		target = notify.Admins()
	}
	msg.Link = "/communication/scheduled?open=" + m.ID
	msg.EntityType, msg.EntityID = "scheduled_message", m.ID
	if _, err := notify.Send(fresh(s.DB), m.OrganizationID, target, msg); err != nil {
		log.Printf("schedsend: notify: %v", err)
	}
}

func (s *Service) notifyFailed(run *Run) {
	res := fresh(s.DB).Model(&Run{}).Where("id = ? AND NOT notified", run.ID).Update("notified", true)
	if res.RowsAffected == 0 {
		return
	}
	var m Message
	if fresh(s.DB).Where("id = ?", run.MessageID).Take(&m).Error != nil {
		return
	}
	title := "Scheduled send failed: " + m.Title
	if run.Sent > 0 {
		title = fmt.Sprintf("Scheduled send partly failed: %s", m.Title)
	}
	s.tellCreator(&m, notify.Message{Type: "schedsend.failed", Severity: notify.Error, Title: title,
		Body: fmt.Sprintf("%d of %d not sent. %s", run.Failed, run.Total, s.firstReason(run.ID))})
}

func (s *Service) notifyMissed(m *Message, n int, next time.Time, continues bool) {
	body := fmt.Sprintf("%d scheduled send(s) were more than 6 hours late because the server was down, so they were not sent.", n)
	if continues {
		body += " Next send: " + FormatLocal(next, m.Timezone) + " (" + m.Timezone + ")."
	} else {
		body += " Use “Send now” or duplicate it if it should still go out."
	}
	s.tellCreator(m, notify.Message{Type: "schedsend.missed", Severity: notify.Warning, Title: "Missed scheduled send: " + m.Title, Body: body})
}

// ---------------------------------------------------------------- statuses

// onWhatsAppStatus moves WhatsApp deliveries along sent → delivered → read, or failed.
func (s *Service) onWhatsAppStatus(ev wa.StatusEvent) {
	s.applyStatus(ChanWhatsApp, ev.MessageID, ev.Status, ev.Error, ev.At)
}

var statusRank = map[string]int{DelSent: 1, DelDelivered: 2, DelRead: 3}

// applyStatus records a provider status (delivered / read / failed).
func (s *Service) applyStatus(channel, providerID, status, reason string, at time.Time) {
	if providerID == "" {
		return
	}
	var d Delivery
	if fresh(s.DB).Where("provider_id = ? AND channel = ?", providerID, channel).Limit(1).Find(&d).Error != nil || d.ID == "" {
		return
	}
	if at.IsZero() {
		at = Now()
	}
	upd := map[string]any{"updated_at": Now()}
	switch status {
	case "delivered":
		if statusRank[d.Status] < 2 {
			upd["status"] = DelDelivered
		}
		upd["delivered_at"] = at
	case "read", "opened":
		upd["status"] = DelRead
		upd["read_at"] = at
		if d.DeliveredAt == nil {
			upd["delivered_at"] = at
		}
	case "failed", "undelivered":
		upd["status"] = DelFailed
		upd["reason"] = clip(firstNonEmpty(reason, "Not delivered"), 490)
	default:
		return
	}
	fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(upd)
	if status == "failed" || status == "undelivered" {
		// Recount the run (it may now have failures) and tell the creator.
		fresh(s.DB).Model(&Run{}).Where("id = ?", d.RunID).Update("notified", false)
		var run Run
		if fresh(s.DB).Where("id = ?", d.RunID).Take(&run).Error == nil && run.Status != RunSending {
			fresh(s.DB).Model(&Run{}).Where("id = ?", run.ID).Update("status", RunSending)
			s.finishRun(run.ID)
		}
	}
}

// levelOK keeps the emailprefs import for level names in one place.
var _ = emailprefs.LevelOwner
