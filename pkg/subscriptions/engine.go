package subscriptions

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

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// MaxCatchUp caps how many periods one run invoices per subscription.
const MaxCatchUp = 12

// LateDueDays: an invoice created after its normal date is due this many days
// after it is issued at the earliest.
const LateDueDays = 7

// RunOptions changes a run.
type RunOptions struct {
	// Force ignores the send hour (manual "Run now", the test clock).
	Force bool
}

// RunResult reports one run for an organisation.
type RunResult struct {
	Organization string   `json:"organization_id"`
	Today        string   `json:"today"`
	Invoiced     int      `json:"invoiced"`
	Emailed      int      `json:"emailed"`
	Reminders    int      `json:"reminders"`
	Notices      int      `json:"final_notices"`
	Alerts       int      `json:"alerts"`
	Suspended    int      `json:"suspended"`
	Unsuspended  int      `json:"unsuspended"`
	Domains      int      `json:"domain_reminders"`
	Ended        int      `json:"ended"`
	Synced       int      `json:"synced"`
	Digest       string   `json:"digest,omitempty"`
	SkippedHour  bool     `json:"skipped_send_hour,omitempty"`
	Log          []string `json:"log"`
	Errors       []string `json:"errors"`
}

func (r *RunResult) logf(format string, a ...any) { r.Log = append(r.Log, fmt.Sprintf(format, a...)) }

func newResult(org string, today time.Time) RunResult {
	return RunResult{Organization: org, Today: DayStr(today), Log: []string{}, Errors: []string{}}
}

// RunOrg creates the invoices that are due, syncs payments, runs dunning and
// suspensions, domain reminders and the daily digest for one organisation.
// Callers hold its lock.
func (s *Service) RunOrg(ctx context.Context, org string, opt RunOptions) RunResult {
	today := s.Today(org)
	res := newResult(org, today)
	st := s.SettingsFor(org)
	s.syncOrg(ctx, org, st, &res)
	if !st.Enabled {
		res.logf("Billing is switched off (Subscriptions → Settings).")
		return res
	}
	local := s.now().In(s.Loc(org))
	canSend := opt.Force || local.Hour() >= st.SendHour
	res.SkippedHour = !canSend
	var subs []Subscription
	s.DB.Preload("Lines", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).
		Where("organization_id = ? AND status <> ?", org, StatusCancelled).Order("created_at").Find(&subs)
	for i := range subs {
		if ctx.Err() != nil {
			break
		}
		sub := &subs[i]
		s.generate(ctx, org, sub, st, today, &res)
		if canSend {
			s.dun(ctx, org, sub, st, today, &res)
		}
		s.refreshStatus(ctx, org, sub, st, today, &res)
		s.refreshNextBilling(sub.ID)
	}
	if canSend {
		s.domainReminders(ctx, org, subs, st, today, &res)
	}
	if canSend && st.Digest && (opt.Force || local.Hour() >= st.DigestHour) && st.LastDigest != DayStr(today) {
		status, err := s.SendDigest(ctx, org, "digest|"+DayStr(today), false)
		res.Digest = status
		if err != nil {
			res.Errors = append(res.Errors, "digest: "+err.Error())
		}
	}
	s.ensureSettings(s.DB, org)
	s.DB.Model(&Settings{}).Where("organization_id = ?", org).Update("last_run_at", s.now())
	return res
}

// ---------------------------------------------------------------- invoices

// generate invoices every period whose invoice date has come.
func (s *Service) generate(ctx context.Context, org string, sub *Subscription, st Settings, today time.Time, res *RunResult) {
	if sub.Status == StatusSuspended || sub.Status == StatusCancelled {
		return // held until it is paid (or unsuspended)
	}
	sc := sub.Schedule()
	lead := Lead(sub.LeadDays, st, sub.Period)
	for i := 0; i < MaxCatchUp; i++ {
		sp, ok := sc.StartingOn(sub.NextPeriodStart)
		if !ok {
			sp = sc.Covering(sub.NextPeriodStart)
			res.Errors = append(res.Errors, fmt.Sprintf("%s: next period %s does not match the schedule — continuing from %s", sub.Number, DayStr(sub.NextPeriodStart), DayStr(sp.Start)))
		}
		if sub.EndDate != nil && !sp.Start.Before(Day(*sub.EndDate)) {
			return
		}
		issue := IssueDate(sp, lead)
		if issue.After(today) {
			return
		}
		due := DueDate(sp, sub.PaymentTermsDays)
		if !s.invoicePeriod(ctx, org, sub, sp, due, issue, today, st, res) {
			return
		}
		next := sc.Next(sp)
		upd := s.DB.Model(&Subscription{}).Where("id = ? AND next_period_start = ?", sub.ID, Day(sub.NextPeriodStart)).
			Updates(map[string]any{"next_period_start": next.Start, "updated_at": s.now()})
		if upd.RowsAffected == 0 {
			return // another run moved it
		}
		sub.NextPeriodStart = next.Start
	}
}

func (s *Service) periodRef(sub *Subscription, sp Span) string {
	return sub.Number + " · " + DayStr(sp.Start)
}

// invoicePeriod creates (and emails) one period's invoice. The period row
// is claimed first (unique per subscription + start), so two runs can never
// invoice a period twice. It reports whether the period is done.
func (s *Service) invoicePeriod(ctx context.Context, org string, sub *Subscription, sp Span, due, issue, today time.Time, st Settings, res *RunResult) bool {
	now := s.now()
	var seq int64
	s.DB.Model(&BillingPeriod{}).Where("subscription_id = ?", sub.ID).Count(&seq)
	per := BillingPeriod{ID: uuid.NewString(), OrganizationID: org, SubscriptionID: sub.ID, Start: sp.Start, End: sp.End, Seq: int(seq) + 1,
		Partial: sp.Partial, Fraction: sp.Fraction, IssueDate: issue, DueDate: due, Status: PerGenerating, Currency: sub.Currency, CreatedAt: now, UpdatedAt: now}
	if issue.Before(today) {
		// Created late (billing was held while suspended, or past periods are
		// billed now): issued today and due in a week at the earliest, so the
		// client is never overdue on an invoice they have just received.
		per.IssueDate = today
		if min := today.AddDate(0, 0, LateDueDays); per.DueDate.Before(min) {
			per.DueDate = min
		}
	}
	tx := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&per)
	if tx.Error != nil {
		res.Errors = append(res.Errors, sub.Number+": "+tx.Error.Error())
		return false
	}
	if tx.RowsAffected == 0 {
		var ex BillingPeriod
		if !found(s.DB.Where("subscription_id = ? AND period_start = ?", sub.ID, sp.Start), &ex) {
			return false
		}
		if ex.Status != PerGenerating {
			return true // already invoiced
		}
		if now.Sub(ex.UpdatedAt) < 10*time.Minute {
			return false // another run is on it
		}
		// A run died half-way: adopt its invoice if it was created, else retry.
		if inv := s.findInvoice(org, s.periodRef(sub, sp)); inv != nil {
			s.DB.Model(&BillingPeriod{}).Where("id = ?", ex.ID).Updates(map[string]any{"status": PerInvoiced, "invoice_id": inv.ID, "invoice_number": inv.Number,
				"amount": inv.Total, "balance": inv.BalanceDue, "updated_at": now})
			return true
		}
		if s.DB.Model(&BillingPeriod{}).Where("id = ? AND status = ? AND updated_at = ?", ex.ID, PerGenerating, ex.UpdatedAt).Update("updated_at", now).RowsAffected == 0 {
			return false
		}
		per = ex
	}
	body, total, err := s.invoiceBody(org, sub, sp, per)
	if err == nil && total <= 0 {
		// Nothing to charge (a free period): it is simply paid.
		paid := now
		s.DB.Model(&BillingPeriod{}).Where("id = ?", per.ID).Updates(map[string]any{"status": PerPaid, "paid_at": &paid, "amount": 0, "balance": 0, "sent": true, "updated_at": now})
		res.logf("%s: %s is free — nothing to invoice", sub.Number, periodText(sp))
		return true
	}
	var ref *InvoiceRef
	var who Actor
	if err == nil {
		who, err = s.actor(org, sub.CreatedBy)
	}
	if err == nil {
		if s.Inv == nil {
			err = errors.New("invoicing is not available")
		} else {
			ref, err = s.Inv.Create(ctx, who, body)
		}
	}
	if err != nil {
		s.DB.Where("id = ? AND status = ?", per.ID, PerGenerating).Delete(&BillingPeriod{})
		res.Errors = append(res.Errors, fmt.Sprintf("%s (%s): %v", sub.Number, DayStr(sp.Start), err))
		// Tell the admins once a day.
		if row, ok := s.claim(org, Event{Key: "invoice_failed|" + sub.ID + "|" + DayStr(sp.Start) + "|" + DayStr(today), SubscriptionID: sub.ID, Stage: "invoice_failed", Audience: ToAdmin, Day: today}); ok {
			a := Alert{Type: "invoice_failed", Severity: notify.Error, Subject: fmt.Sprintf("Couldn't create the invoice for %s (%s)", sub.Name, sub.Number),
				Lead: fmt.Sprintf("The invoice for %s (due %s) could not be created: %v", periodText(sp), longDate(due), err), Link: subLink(sub.ID, ""),
				Entity: sub.ID, Tone: "danger", Status: "Invoice not created", Label: "Client billing"}
			to, inApp, aerr := s.alertAdmins(ctx, org, st, a)
			row.Subject, row.Recipients, row.InApp = a.Subject, strings.Join(to, ", "), inApp
			s.finish(row, "sent", aerr)
		}
		return false
	}
	s.DB.Model(&BillingPeriod{}).Where("id = ?", per.ID).Updates(map[string]any{"status": PerInvoiced, "invoice_id": ref.ID, "invoice_number": ref.Number,
		"amount": ref.Total, "balance": ref.BalanceDue, "currency": ref.Currency, "error": "", "updated_at": s.now()})
	per.Status, per.InvoiceID, per.InvoiceNumber, per.Amount, per.Balance, per.Currency = PerInvoiced, ref.ID, ref.Number, ref.Total, ref.BalanceDue, ref.Currency
	res.Invoiced++
	res.logf("%s: invoice %s created for %s (%s, due %s)", sub.Number, ref.Number, periodText(sp), money(ref.Total, ref.Currency), DayStr(per.DueDate))
	if sub.AutoSend {
		s.emailInvoice(ctx, org, sub, &per, sp, st, today, res)
	} else if row, ok := s.claim(org, Event{Key: per.ID + "|draft", SubscriptionID: sub.ID, PeriodID: per.ID, Stage: "draft", Audience: ToAdmin, Day: today}); ok {
		a := Alert{Type: "draft", Severity: notify.Info, Subject: fmt.Sprintf("Draft invoice %s for %s is ready to send", ref.Number, sub.Name),
			Lead: fmt.Sprintf("Auto-send is off for %s: check invoice %s (%s, due %s) and send it to the client.", sub.Number, ref.Number, money(ref.Total, ref.Currency), longDate(per.DueDate)),
			Link: "/invoices/" + ref.ID, LinkText: "Review the invoice", Entity: sub.ID, Tone: "info", Status: "Draft to send", Label: "Client billing"}
		to, inApp, err := s.alertAdmins(ctx, org, st, a)
		row.Subject, row.Recipients, row.InApp = a.Subject, strings.Join(to, ", "), inApp
		s.finish(row, "sent", err)
	}
	return true
}

// findInvoice finds an invoice by its subscription reference (crash recovery).
func (s *Service) findInvoice(org, ref string) *InvoiceRef {
	var rows []InvoiceRef
	s.DB.Raw(`SELECT id, number, status, public_token, currency, total, balance_due FROM billing_invoices WHERE organization_id = ? AND reference = ? AND deleted_at IS NULL
		ORDER BY created_at LIMIT 1`, org, ref).Scan(&rows)
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

// invoiceBody is the invoicing API request for one period: one line per
// service with the period in its description.
func (s *Service) invoiceBody(org string, sub *Subscription, sp Span, per BillingPeriod) (map[string]any, float64, error) {
	cu, ok := s.customer(org, sub.CustomerID)
	if !ok {
		return nil, 0, errors.New("the customer was deleted")
	}
	ids := []string{}
	for _, l := range sub.Lines {
		if l.ServiceID != "" {
			ids = append(ids, l.ServiceID)
		}
	}
	cat := s.catalog(org, ids)
	tax := s.taxDefaults(org)
	items := []map[string]any{}
	total := 0.0
	pt := periodText(sp)
	for i := range sub.Lines {
		l := &sub.Lines[i]
		lc, ok := s.lineCalc(l, cat, sub.Period, sub.Currency, tax)
		if !ok {
			return nil, 0, fmt.Errorf("%s has no price — the catalogue service was removed; set a price on the subscription", l.Description)
		}
		unit := lc.price
		desc := l.Description + " — " + pt
		if sp.Partial {
			unit = Prorate(lc.price, sp.Fraction, lc.cur)
			full := DaysBetween(AddMonthsOn(sp.End, -sub.Schedule().Months, sub.BillingDay), sp.End)
			desc += fmt.Sprintf(" (prorated: %d of %d days)", sp.Days(), full)
		}
		it := map[string]any{"item_type": "service", "description": clip(desc, 480), "quantity": l.Quantity, "tax_rate": l.TaxRate}
		if l.ServiceID != "" {
			it["reference_id"] = l.ServiceID
		}
		if lc.cur != sub.Currency {
			v, ok := s.convert(org, unit, lc.cur, sub.Currency)
			if !ok {
				return nil, 0, fmt.Errorf("no exchange rate for %s → %s — set one in Invoice settings → Exchange rates", lc.cur, sub.Currency)
			}
			it["unit_price"], it["original_unit_price"], it["original_currency"] = RoundCur(v, sub.Currency), unit, lc.cur
			total += v * l.Quantity
		} else {
			it["unit_price"] = unit
			total += unit * l.Quantity
		}
		items = append(items, it)
	}
	notes := fmt.Sprintf("Subscription %s — %s (%s).", sub.Number, sub.Name, pt)
	if ds := sub.AllDomains(); len(ds) > 0 {
		notes += " Service: " + strings.Join(ds, ", ") + "."
	}
	if u := s.PortalURL(org, sub.CustomerID); u != "" {
		notes += "\nYour services: " + u
	}
	body := map[string]any{"customer_id": sub.CustomerID, "client_name": cu.Name, "client_email": cu.Email, "client_phone": cu.Phone, "client_address": cu.Address,
		"currency": sub.Currency, "issue_date": DayStr(per.IssueDate), "due_date": DayStr(per.DueDate), "reference": s.periodRef(sub, sp),
		"title": clip(sub.Name, 250), "notes": notes, "items": items}
	return body, total, nil
}

// emailInvoice sends the period's invoice email with the View / Pay link.
func (s *Service) emailInvoice(ctx context.Context, org string, sub *Subscription, per *BillingPeriod, sp Span, st Settings, today time.Time, res *RunResult) bool {
	row, ok := s.claim(org, Event{Key: per.ID + "|invoice", SubscriptionID: sub.ID, PeriodID: per.ID, Stage: "invoice", Audience: ToClient, Day: today})
	if !ok {
		return false
	}
	who, err := s.actor(org, sub.CreatedBy)
	subject, msg := s.invoiceMessage(org, sub, sp)
	row.Subject = s.sentSubject(org, subject, per.InvoiceNumber)
	cu, _ := s.customer(org, sub.CustomerID)
	row.Recipients = cu.Email
	if err == nil {
		err = s.Inv.Send(ctx, who, per.InvoiceID, map[string]any{"kind": "invoice", "subject": subject, "message": msg, "app_url": s.AppURL})
	}
	s.finish(row, "sent", err)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("%s: invoice %s not emailed: %v", sub.Number, per.InvoiceNumber, err))
		if row.Attempts == 1 {
			a := Alert{Type: "send_failed", Severity: notify.Error, Subject: fmt.Sprintf("Invoice %s for %s was not emailed", per.InvoiceNumber, cu.Name),
				Lead: fmt.Sprintf("The invoice for %s could not be emailed to the client: %v. It is retried twice; you can also send it from the invoice.", sub.Name, err),
				Link: "/invoices/" + per.InvoiceID, LinkText: "Open the invoice", Entity: sub.ID, Tone: "danger", Status: "Not emailed", Label: "Client billing"}
			s.alertAdmins(ctx, org, st, a)
		}
		return false
	}
	s.DB.Model(&BillingPeriod{}).Where("id = ?", per.ID).Updates(map[string]any{"sent": true, "updated_at": s.now()})
	per.Sent = true
	res.Emailed++
	res.logf("%s: invoice %s emailed to %s", sub.Number, per.InvoiceNumber, cu.Email)
	return true
}

// ---------------------------------------------------------------- dunning

// dun runs the dunning steps of the subscription's oldest unpaid invoice.
func (s *Service) dun(ctx context.Context, org string, sub *Subscription, st Settings, today time.Time, res *RunResult) {
	if sub.Status == StatusSuspended || sub.Status == StatusCancelled {
		return
	}
	var pers []BillingPeriod
	s.DB.Where("subscription_id = ? AND status = ? AND sent", sub.ID, PerInvoiced).Order("due_date, period_start").Limit(1).Find(&pers)
	if len(pers) == 0 {
		return
	}
	per := pers[0]
	// Never act on stale data: the invoice may have been paid a second ago.
	if s.syncPeriod(ctx, org, &per, st, res) {
		return
	}
	who, err := s.actor(org, sub.CreatedBy)
	if err != nil {
		res.Errors = append(res.Errors, sub.Number+": "+err.Error())
		return
	}
	pol := PolicyOf(st)
	d := s.datesFor(sub, &per, st, s.warnedFor(per.ID), today)
	plan := pol.Due(d, today, false)
	cu, _ := s.customer(org, sub.CustomerID)
	if c := plan.Client; c != nil {
		if row, ok := s.claim(org, Event{Key: c.EventKey(per.ID, d), SubscriptionID: sub.ID, PeriodID: per.ID, Stage: c.Key, Audience: ToClient, Day: today}); ok {
			subject, msg := s.reminderMessage(org, sub, d, today)
			row.Subject, row.Recipients = s.sentSubject(org, subject, per.InvoiceNumber), cu.Email
			err := s.clientReminder(ctx, who, sub, &per, subject, msg)
			s.finish(row, "sent", err)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: reminder %s: %v", sub.Number, c.Key, err))
			} else {
				res.Reminders++
				res.logf("%s: client reminder %s sent to %s", sub.Number, c.Key, cu.Email)
			}
		}
	}
	if a := plan.Admin; a != nil {
		if row, ok := s.claim(org, Event{Key: a.EventKey(per.ID, d), SubscriptionID: sub.ID, PeriodID: per.ID, Stage: a.Key, Audience: ToAdmin, Day: today}); ok {
			al := s.suspensionAlert(org, sub, &per, d, today)
			to, inApp, err := s.alertAdmins(ctx, org, st, al)
			row.Subject, row.Recipients, row.InApp = al.Subject, strings.Join(to, ", "), inApp
			s.finish(row, "sent", err)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: admin alert: %v", sub.Number, err))
			} else {
				res.Alerts++
				res.logf("%s: admin alert %s sent (suspension %s)", sub.Number, a.Key, DayStr(*d.Suspend))
			}
		}
	}
	if f := plan.FinalNotice; f != nil {
		if row, ok := s.claim(org, Event{Key: f.EventKey(per.ID, d), SubscriptionID: sub.ID, PeriodID: per.ID, Stage: "final_notice", Audience: ToClient, Day: today}); ok {
			subject, msg := s.finalNoticeMessage(org, sub, d)
			row.Subject, row.Recipients = s.sentSubject(org, subject, per.InvoiceNumber), cu.Email
			err := s.clientReminder(ctx, who, sub, &per, subject, msg)
			s.finish(row, "sent", err)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: final notice: %v", sub.Number, err))
			} else {
				res.Notices++
				res.logf("%s: final notice sent (suspension %s)", sub.Number, DayStr(*d.Suspend))
			}
		}
	}
	if plan.Suspend != nil {
		s.suspend(ctx, org, sub, &per, st, today, "unpaid", "", res)
	}
}

// refreshStatus keeps active / past_due right and ends subscriptions at
// their end date.
func (s *Service) refreshStatus(ctx context.Context, org string, sub *Subscription, st Settings, today time.Time, res *RunResult) {
	if sub.Status == StatusSuspended || sub.Status == StatusCancelled {
		return
	}
	if sub.EndDate != nil && !today.Before(Day(*sub.EndDate)) {
		var open int64
		s.DB.Model(&BillingPeriod{}).Where("subscription_id = ? AND status = ?", sub.ID, PerInvoiced).Count(&open)
		if open == 0 {
			if s.DB.Model(&Subscription{}).Where("id = ? AND status = ?", sub.ID, sub.Status).Updates(map[string]any{"status": StatusCancelled,
				"cancelled_at": today, "cancel_reason": "end_of_term", "updated_at": s.now()}).RowsAffected > 0 {
				sub.Status = StatusCancelled
				res.Ended++
				res.logf("%s: ended on %s (end of term)", sub.Number, DayStr(*sub.EndDate))
				s.record(org, Event{Key: sub.ID + "|ended", SubscriptionID: sub.ID, Stage: "ended", Audience: ToAdmin, Day: today, Subject: "Ended on " + longDate(Day(*sub.EndDate))})
				s.cancelTasks(ctx, org, sub, today)
				return
			}
		}
	}
	var overdue int64
	s.DB.Model(&BillingPeriod{}).Where("subscription_id = ? AND status = ? AND sent AND due_date < ?", sub.ID, PerInvoiced, today).Count(&overdue)
	want := StatusActive
	if overdue > 0 {
		want = StatusPastDue
	}
	if want != sub.Status {
		if s.DB.Model(&Subscription{}).Where("id = ? AND status = ?", sub.ID, sub.Status).Updates(map[string]any{"status": want, "updated_at": s.now()}).RowsAffected > 0 {
			res.logf("%s: %s → %s", sub.Number, sub.Status, want)
			sub.Status = want
		}
	}
}

// ---------------------------------------------------------------- payments

type invState struct {
	ID         string
	Status     string
	Total      float64
	BalanceDue float64
	SentAt     *time.Time
	PaidAt     *time.Time
	Deleted    bool
	Number     string
}

func (s *Service) invoiceStates(org string, ids []string) map[string]invState {
	out := map[string]invState{}
	if len(ids) == 0 {
		return out
	}
	var rows []invState
	s.DB.Raw(`SELECT id, status, total, balance_due, sent_at, paid_at, deleted_at IS NOT NULL AS deleted, number FROM billing_invoices WHERE organization_id = ? AND id IN ?`, org, ids).Scan(&rows)
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

// applyState updates a period from its invoice; true when it changed.
func (s *Service) applyState(per *BillingPeriod, inv invState, found bool) bool {
	status := per.Status
	switch {
	case !found || inv.Deleted, inv.Status == invoicing.StatusVoid:
		status = PerVoid
	case inv.BalanceDue <= 0.004 && inv.Total > 0:
		status = PerPaid
	default:
		status = PerInvoiced
	}
	sent := per.Sent || (found && (inv.Status != invoicing.StatusDraft || inv.SentAt != nil))
	if status == per.Status && sent == per.Sent && (!found || (inv.BalanceDue == per.Balance && inv.Total == per.Amount)) {
		return false
	}
	upd := map[string]any{"status": status, "sent": sent, "updated_at": s.now()}
	if found {
		upd["balance"], upd["amount"] = inv.BalanceDue, inv.Total
		if inv.Number != "" {
			upd["invoice_number"] = inv.Number
		}
	}
	if status == PerPaid && per.PaidAt == nil {
		t := s.now()
		if inv.PaidAt != nil {
			t = *inv.PaidAt
		}
		upd["paid_at"] = t
		per.PaidAt = &t
	}
	if status != PerPaid {
		upd["paid_at"] = nil
		per.PaidAt = nil
	}
	s.DB.Model(&BillingPeriod{}).Where("id = ?", per.ID).Updates(upd)
	per.Status, per.Sent = status, sent
	if found {
		per.Balance, per.Amount = inv.BalanceDue, inv.Total
	}
	return true
}

// syncOrg refreshes every invoiced period from its invoice and moves the
// subscriptions forward (paid → active, suspended → unsuspended).
func (s *Service) syncOrg(ctx context.Context, org string, st Settings, res *RunResult) {
	var pers []BillingPeriod
	s.DB.Where("organization_id = ? AND invoice_id <> '' AND status IN ?", org, []string{PerInvoiced, PerPaid, PerVoid}).Find(&pers)
	ids := []string{}
	for _, p := range pers {
		ids = append(ids, p.InvoiceID)
	}
	states := s.invoiceStates(org, ids)
	changed := map[string]bool{}
	for i := range pers {
		inv, ok := states[pers[i].InvoiceID]
		if s.applyState(&pers[i], inv, ok) {
			changed[pers[i].SubscriptionID] = true
			res.Synced++
		}
	}
	for id := range changed {
		s.afterPayment(ctx, org, id, st, res)
	}
	// Suspended subscriptions whose invoices were all settled while nothing ran.
	var held []string
	s.DB.Raw(`SELECT id FROM client_subscriptions c WHERE organization_id = ? AND status = ?
		AND NOT EXISTS (SELECT 1 FROM client_subscription_periods p WHERE p.subscription_id = c.id AND p.status = ? AND p.due_date < ?)`,
		org, StatusSuspended, PerInvoiced, s.Today(org)).Scan(&held)
	for _, id := range held {
		if !changed[id] {
			s.afterPayment(ctx, org, id, st, res)
		}
	}
}

// syncPeriod refreshes one period; true when it is no longer unpaid.
func (s *Service) syncPeriod(ctx context.Context, org string, per *BillingPeriod, st Settings, res *RunResult) bool {
	states := s.invoiceStates(org, []string{per.InvoiceID})
	inv, ok := states[per.InvoiceID]
	if s.applyState(per, inv, ok) {
		res.Synced++
		s.afterPayment(ctx, org, per.SubscriptionID, st, res)
	}
	return per.Status != PerInvoiced
}

// SyncInvoice applies a payment (or void) on one invoice straight away.
func (s *Service) SyncInvoice(ctx context.Context, org, invoiceID string) bool {
	var per BillingPeriod
	if !found(s.DB.Where("organization_id = ? AND invoice_id = ?", org, invoiceID), &per) {
		return false
	}
	res := newResult(org, s.Today(org))
	st := s.SettingsFor(org)
	states := s.invoiceStates(org, []string{invoiceID})
	inv, ok := states[invoiceID]
	if s.applyState(&per, inv, ok) {
		s.afterPayment(ctx, org, per.SubscriptionID, st, &res)
		return true
	}
	return false
}

// afterPayment moves the subscription forward: paid through, the next
// billing date, and back to active when nothing overdue is left
// (unsuspending a suspended one).
func (s *Service) afterPayment(ctx context.Context, org, subID string, st Settings, res *RunResult) {
	sub, err := s.load(org, subID)
	if err != nil {
		return
	}
	var last []BillingPeriod
	s.DB.Where("subscription_id = ? AND status = ?", subID, PerPaid).Order("period_end DESC").Limit(1).Find(&last)
	if len(last) > 0 {
		pt := last[0].End.AddDate(0, 0, -1)
		s.DB.Model(&Subscription{}).Where("id = ?", subID).Update("paid_through", pt)
		sub.PaidThrough = &pt
	}
	s.refreshNextBilling(subID)
	today := s.Today(org)
	var overdue int64
	s.DB.Model(&BillingPeriod{}).Where("subscription_id = ? AND status = ? AND due_date < ?", subID, PerInvoiced, today).Count(&overdue)
	if overdue > 0 {
		return
	}
	switch sub.Status {
	case StatusSuspended:
		s.unsuspend(ctx, org, sub, st, today, "paid", "", 0, res)
	case StatusPastDue:
		if s.DB.Model(&Subscription{}).Where("id = ? AND status = ?", subID, StatusPastDue).Updates(map[string]any{"status": StatusActive, "updated_at": s.now()}).RowsAffected > 0 {
			res.logf("%s: paid — active", sub.Number)
		}
	}
}

// ---------------------------------------------------------------- scheduler

// RunAll runs every organisation with subscriptions, one organisation at a
// time per instance (Postgres advisory lock).
func (s *Service) RunAll(ctx context.Context) int {
	var orgs []string
	s.DB.Raw(`SELECT DISTINCT organization_id FROM client_subscriptions`).Scan(&orgs)
	n := 0
	for _, org := range orgs {
		if ctx.Err() != nil {
			return n
		}
		ok := s.withLock(ctx, org, false, func() {
			r := s.RunOrg(ctx, org, RunOptions{})
			if r.Invoiced+r.Emailed+r.Reminders+r.Notices+r.Alerts+r.Suspended+r.Unsuspended+r.Domains > 0 || len(r.Errors) > 0 {
				log.Printf("subscriptions: %s — %d invoiced, %d emailed, %d reminders, %d final notices, %d alerts, %d suspended, %d unsuspended, %d domain reminders, %d errors",
					org, r.Invoiced, r.Emailed, r.Reminders, r.Notices, r.Alerts, r.Suspended, r.Unsuspended, r.Domains, len(r.Errors))
			}
			n++
		})
		if !ok {
			log.Printf("subscriptions: %s is running on another instance", org)
		}
	}
	return n
}

// RunOrgLocked runs one organisation under its lock (manual run); ok false
// when a run is already in progress.
func (s *Service) RunOrgLocked(ctx context.Context, org string, opt RunOptions) (RunResult, bool) {
	var r RunResult
	ok := s.withLock(ctx, org, false, func() { r = s.RunOrg(ctx, org, opt) })
	return r, ok
}

// Locked runs fn under the organisation's lock, waiting for it.
func (s *Service) Locked(ctx context.Context, org string, fn func()) bool {
	return s.withLock(ctx, org, true, fn)
}

func (s *Service) withLock(ctx context.Context, org string, wait bool, fn func()) bool {
	key := "subscriptions:" + org
	got := false
	err := s.DB.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		if wait {
			if err := tx.Exec("SELECT pg_advisory_lock(hashtext(?))", key).Error; err != nil {
				return err
			}
			got = true
		} else if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", key).Scan(&got).Error; err != nil {
			return err
		}
		if !got {
			return nil
		}
		defer tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", key)
		fn()
		return nil
	})
	if err != nil {
		log.Printf("subscriptions: lock %s: %v", key, err)
		return false
	}
	return got
}

// StartScheduler runs every `every` (SUBSCRIPTIONS_SCHEDULER=off disables it).
func (s *Service) StartScheduler(ctx context.Context, every, firstDelay time.Duration) {
	if every <= 0 {
		every = 10 * time.Minute
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("subscriptions: scheduler stopped: %v", r)
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(firstDelay):
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			s.RunAll(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// ---------------------------------------------------------------- payment hook

type invoiceEvent struct {
	org, invoice string
	payment      bool
}

var paymentTypes = map[string]bool{"payment": true, "online_payment": true, "payment_removed": true, "payment_edited": true, "voided": true, "sent": true, "reminder": true}

// WatchPayments applies invoice payments (any path: recorded by hand, the
// Flutterwave webhook or redirect, receipts) to subscriptions within
// seconds. It observes billing_activities with a gorm callback, so
// pkg/invoicing stays untouched; the scheduler's sync catches anything missed.
func (s *Service) WatchPayments(ctx context.Context) error {
	ch := make(chan invoiceEvent, 256)
	err := s.DB.Callback().Create().After("gorm:create").Register("subscriptions:invoice_activity", func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement == nil || tx.Statement.Table != "billing_activities" {
			return
		}
		a, ok := tx.Statement.Dest.(*invoicing.Activity)
		if !ok || a == nil || a.OrganizationID == "" || !paymentTypes[a.Type] {
			return
		}
		select {
		case ch <- invoiceEvent{org: a.OrganizationID, invoice: a.InvoiceID, payment: a.Type == "payment" || a.Type == "online_payment"}:
		default:
		}
	})
	if err != nil {
		return err
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("subscriptions: payment watcher stopped: %v", r)
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-ch:
				// Let the payment's transaction commit first.
				time.Sleep(700 * time.Millisecond)
				s.applyInvoiceEvent(ctx, ev)
			}
		}
	}()
	return nil
}

func (s *Service) applyInvoiceEvent(ctx context.Context, ev invoiceEvent) {
	var n int64
	s.DB.Model(&BillingPeriod{}).Where("organization_id = ? AND invoice_id = ?", ev.org, ev.invoice).Count(&n)
	if n == 0 {
		return
	}
	tries := 1
	if ev.payment {
		tries = 3 // the payment may still be committing
	}
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		changed := false
		s.Locked(ctx, ev.org, func() { changed = s.SyncInvoice(ctx, ev.org, ev.invoice) })
		if changed {
			return
		}
	}
}
