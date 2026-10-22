package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// Admin actions on one subscription (the one-click actions of the alerts,
// the drawer and the AI tools). Callers hold the organisation's lock (see
// Handler) so they never race the engine.

// oldestUnpaid is the subscription's oldest unpaid invoice (sent or not).
func (s *Service) oldestUnpaid(subID, periodID string) (*BillingPeriod, error) {
	var per BillingPeriod
	q := s.DB.Where("subscription_id = ? AND status = ?", subID, PerInvoiced)
	if periodID != "" {
		q = q.Where("id = ?", periodID)
	}
	if !found(q.Order("due_date, period_start"), &per) {
		return nil, invalid("there is no unpaid invoice on this subscription")
	}
	return &per, nil
}

// ExtendResult says what giving more time changed.
type ExtendResult struct {
	*Subscription
	Days        int    `json:"days"`
	SuspendOn   string `json:"suspend_on"`
	Unsuspended bool   `json:"unsuspended"`
}

// Extend gives the client more time on the unpaid invoice: the suspension
// (and the final notice and admin alerts before it) moves `days` later. A
// suspended subscription is unsuspended and suspended again `days` from
// today unless paid.
func (s *Service) Extend(ctx context.Context, org, id, user string, days int, note string) (*ExtendResult, error) {
	sub, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	if sub.Status == StatusCancelled {
		return nil, invalid("the subscription is cancelled")
	}
	if days < 1 || days > 60 {
		return nil, invalid("give 1 to 60 more days")
	}
	per, err := s.oldestUnpaid(id, "")
	if err != nil {
		return nil, err
	}
	st := s.SettingsFor(org)
	today := s.Today(org)
	grace := graceFor(sub, st)
	extra := per.ExtraDays + days
	wasSuspended := sub.Status == StatusSuspended
	if wasSuspended {
		// From today: suspended again `days` from now unless paid.
		if e := DaysBetween(Day(per.DueDate).AddDate(0, 0, grace), today.AddDate(0, 0, days)); e > per.ExtraDays {
			extra = e
		} else {
			extra = per.ExtraDays
		}
	}
	if extra > 365 {
		return nil, invalid("that is more than a year after the due date — cancel the invoice instead")
	}
	s.DB.Model(&BillingPeriod{}).Where("id = ?", per.ID).Updates(map[string]any{"extra_days": extra, "updated_at": s.now()})
	per.ExtraDays = extra
	res := newResult(org, today)
	if wasSuspended {
		s.unsuspend(ctx, org, sub, st, today, "admin", note, days, &res)
	}
	d := s.datesFor(sub, per, st, s.warnedFor(per.ID), today)
	out := &ExtendResult{Days: days, Unsuspended: wasSuspended}
	subject := fmt.Sprintf("More time given on invoice %s: +%d day%s", per.InvoiceNumber, days, plural(days))
	if d.Suspend != nil {
		out.SuspendOn = DayStr(*d.Suspend)
		subject += " — suspension moved to " + longDate(*d.Suspend)
	}
	s.record(org, Event{Key: fmt.Sprintf("%s|extend|%d", per.ID, s.now().UnixNano()), SubscriptionID: id, PeriodID: per.ID, Stage: "extend", Audience: ToAdmin,
		Day: today, CreatedBy: user, Subject: subject + optNote(note)})
	g, err := s.Get(org, id)
	if err != nil {
		return nil, err
	}
	out.Subscription = g
	return out, nil
}

// MarkPaidInput records a payment on the unpaid invoice.
type MarkPaidInput struct {
	PeriodID     string   `json:"period_id"`
	Amount       *float64 `json:"amount"` // default: the balance
	Method       string   `json:"method"`
	Reference    string   `json:"reference"`
	Date         string   `json:"date"`
	Notes        string   `json:"notes"`
	EmailReceipt bool     `json:"email_receipt"`
}

// MarkPaid records the payment through the invoicing API (so receipts,
// balances and the P&L are exactly as for any payment); the subscription
// then moves forward (unsuspended when it was suspended).
func (s *Service) MarkPaid(ctx context.Context, org, id, user, role string, in MarkPaidInput) (*Subscription, error) {
	sub, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	per, err := s.oldestUnpaid(id, in.PeriodID)
	if err != nil {
		return nil, err
	}
	amount := per.Balance
	if in.Amount != nil {
		amount = *in.Amount
	}
	if amount <= 0 || amount > per.Balance+0.004 {
		return nil, invalid("the payment must be more than zero and at most the balance (%s)", money(per.Balance, per.Currency))
	}
	method := strings.TrimSpace(in.Method)
	if method == "" {
		method = "bank_transfer"
	}
	today := s.Today(org)
	// Payments are dated by the real calendar (the invoicing API refuses
	// future dates), whatever the subscription clock says.
	real := LocalDay(time.Now(), s.Loc(org))
	date := DayStr(real)
	if in.Date != "" {
		d, ok := ParseDay(in.Date)
		if !ok || d.After(real) {
			return nil, invalid("the payment date must be today or earlier (YYYY-MM-DD)")
		}
		date = DayStr(d)
	}
	if s.Inv == nil {
		return nil, errors.New("invoicing is not available")
	}
	who := s.userActor(org, user, role)
	body := map[string]any{"amount": RoundCur(amount, per.Currency), "method": method, "reference": clip(in.Reference, 120), "date": date,
		"notes": clip(firstNonEmpty(in.Notes, "Recorded from subscription "+sub.Number), 480), "email_receipt": in.EmailReceipt, "app_url": s.AppURL}
	if err := s.Inv.Pay(ctx, who, per.InvoiceID, body); err != nil {
		return nil, err
	}
	s.record(org, Event{Key: fmt.Sprintf("%s|mark_paid|%d", per.ID, s.now().UnixNano()), SubscriptionID: id, PeriodID: per.ID, Stage: "mark_paid", Audience: ToAdmin,
		Day: today, CreatedBy: user, Subject: fmt.Sprintf("%s recorded on invoice %s (%s%s)", money(amount, per.Currency), per.InvoiceNumber, strings.ReplaceAll(method, "_", " "),
			map[bool]string{true: ", ref " + clip(in.Reference, 60), false: ""}[strings.TrimSpace(in.Reference) != ""])})
	s.SyncInvoice(ctx, org, per.InvoiceID)
	return s.Get(org, id)
}

// SuspendNow suspends the subscription today (an administrator's decision:
// no waiting for the alerts).
func (s *Service) SuspendNow(ctx context.Context, org, id, user, note string) (*Subscription, error) {
	sub, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	if sub.Status == StatusSuspended {
		return nil, invalid("it is already suspended")
	}
	if sub.Status == StatusCancelled {
		return nil, invalid("the subscription is cancelled")
	}
	per, err := s.oldestUnpaid(id, "")
	if err != nil {
		return nil, invalid("there is no unpaid invoice — nothing to suspend it for")
	}
	res := newResult(org, s.Today(org))
	if !s.suspend(ctx, org, sub, per, s.SettingsFor(org), s.Today(org), "admin", note, &res) {
		return nil, invalid("it could not be suspended — try again")
	}
	s.DB.Model(&Event{}).Where("subscription_id = ? AND stage = ? AND created_by = ''", id, "suspend_now").Update("created_by", user)
	return s.Get(org, id)
}

// SkipSuspension sets "don't suspend this time" on the unpaid invoice (undo
// puts the suspension back on). The client reminders continue.
func (s *Service) SkipSuspension(org, id, user, note string, undo bool) (*Subscription, error) {
	sub, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	if sub.Status == StatusCancelled {
		return nil, invalid("the subscription is cancelled")
	}
	per, err := s.oldestUnpaid(id, "")
	if err != nil {
		return nil, err
	}
	s.DB.Model(&BillingPeriod{}).Where("id = ?", per.ID).Updates(map[string]any{"no_suspend": !undo, "updated_at": s.now()})
	stage, subject := "skip", fmt.Sprintf("Won't be suspended for invoice %s", per.InvoiceNumber)
	if undo {
		stage, subject = "unskip", fmt.Sprintf("Suspension back on for invoice %s", per.InvoiceNumber)
		s.ensureNotice(sub, s.SettingsFor(org), s.Today(org))
	}
	s.record(org, Event{Key: fmt.Sprintf("%s|%s|%d", per.ID, stage, s.now().UnixNano()), SubscriptionID: id, PeriodID: per.ID, Stage: stage, Audience: ToAdmin,
		Day: s.Today(org), CreatedBy: user, Subject: subject + optNote(note)})
	return s.Get(org, id)
}

// Unsuspend restores a suspended subscription by hand. With an invoice
// still overdue, days gives the client that much more time.
func (s *Service) Unsuspend(ctx context.Context, org, id, user string, days int, note string) (*Subscription, error) {
	sub, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	if sub.Status != StatusSuspended {
		return nil, invalid("only a suspended subscription can be unsuspended")
	}
	today := s.Today(org)
	var overdue int64
	s.DB.Model(&BillingPeriod{}).Where("subscription_id = ? AND status = ? AND due_date < ?", id, PerInvoiced, today).Count(&overdue)
	if overdue > 0 {
		if days < 1 {
			return nil, invalid("an invoice is still overdue — give the client more time (e.g. 7 days) or mark it as paid")
		}
		r, err := s.Extend(ctx, org, id, user, days, note)
		if err != nil {
			return nil, err
		}
		return r.Subscription, nil
	}
	res := newResult(org, today)
	s.unsuspend(ctx, org, sub, s.SettingsFor(org), today, "admin", note, 0, &res)
	return s.Get(org, id)
}

// CancelInput cancels a subscription.
type CancelInput struct {
	When string `json:"when"` // now | end_of_term
	Note string `json:"note"`
}

// Cancel cancels now ("Cancel … at the provider" tasks are created — never
// done automatically) or at the end of the current term (no more invoices).
func (s *Service) Cancel(ctx context.Context, org, id, user string, in CancelInput) (*Subscription, error) {
	sub, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	if sub.Status == StatusCancelled {
		return nil, invalid("the subscription is already cancelled")
	}
	today := s.Today(org)
	switch in.When {
	case "end_of_term":
		end := Day(sub.NextPeriodStart)
		if end.Before(today) {
			end = today
		}
		s.DB.Model(&Subscription{}).Where("id = ?", id).Updates(map[string]any{"end_date": end, "updated_at": s.now()})
		s.record(org, Event{Key: fmt.Sprintf("%s|end|%d", id, s.now().UnixNano()), SubscriptionID: id, Stage: "cancel_scheduled", Audience: ToAdmin, Day: today, CreatedBy: user,
			Subject: "Ends on " + longDate(end) + " — no invoices after that" + optNote(in.Note)})
	case "", "now":
		s.DB.Model(&Subscription{}).Where("id = ?", id).Updates(map[string]any{"status": StatusCancelled, "cancelled_at": today, "cancel_reason": "admin", "updated_at": s.now()})
		sub.Status = StatusCancelled
		s.cancelTasks(ctx, org, sub, today)
		// Open suspend / unsuspend tasks no longer apply.
		s.DB.Model(&Task{}).Where("subscription_id = ? AND status = ? AND kind IN ?", id, TaskOpen, []string{TaskSuspend, TaskRestore}).
			Updates(map[string]any{"status": TaskDismissed, "note": "Not needed: the subscription was cancelled.", "updated_at": s.now()})
		s.record(org, Event{Key: fmt.Sprintf("%s|cancel_admin|%d", id, s.now().UnixNano()), SubscriptionID: id, Stage: "cancelled", Audience: ToAdmin, Day: today, CreatedBy: user,
			Subject: "Cancelled by an administrator" + optNote(in.Note)})
	default:
		return nil, invalid("cancel now or at the end of the term")
	}
	return s.Get(org, id)
}

// SendReminder emails the client a reminder for the oldest unpaid invoice now.
func (s *Service) SendReminder(ctx context.Context, org, id, user, role string) (string, error) {
	sub, err := s.load(org, id)
	if err != nil {
		return "", err
	}
	per, err := s.oldestUnpaid(id, "")
	if err != nil {
		return "", err
	}
	today := s.Today(org)
	st := s.SettingsFor(org)
	d := s.datesFor(sub, per, st, s.warnedFor(per.ID), today)
	subject, msg := s.reminderMessage(org, sub, d, today)
	if err := s.clientReminder(ctx, s.userActor(org, user, role), sub, per, subject, msg); err != nil {
		return "", err
	}
	if !per.Sent {
		s.DB.Model(&BillingPeriod{}).Where("id = ?", per.ID).Update("sent", true)
	}
	cu, _ := s.customer(org, sub.CustomerID)
	s.record(org, Event{Key: fmt.Sprintf("%s|manual_reminder|%d", per.ID, s.now().UnixNano()), SubscriptionID: id, PeriodID: per.ID, Stage: "manual_reminder",
		Audience: ToClient, Day: today, CreatedBy: user, Subject: s.sentSubject(org, subject, per.InvoiceNumber), Recipients: cu.Email, Status: "sent"})
	return cu.Email, nil
}

// ---------------------------------------------------------------- digest

// DigestItem is one line of the daily digest.
type DigestItem struct {
	SubscriptionID string  `json:"subscription_id"`
	Number         string  `json:"number"`
	Customer       string  `json:"customer"`
	Phone          string  `json:"phone,omitempty"`
	Service        string  `json:"service"`
	Invoice        string  `json:"invoice,omitempty"`
	InvoiceID      string  `json:"invoice_id,omitempty"`
	Balance        float64 `json:"balance"`
	Currency       string  `json:"currency"`
	Due            string  `json:"due,omitempty"`
	SuspendOn      string  `json:"suspend_on,omitempty"`
	DaysToSuspend  *int    `json:"days_to_suspend,omitempty"`
	Days           int     `json:"days"` // overdue (+) or until due (−)
	Status         string  `json:"status"`
	NoSuspend      bool    `json:"no_suspend,omitempty"`
}

// DigestData is everything upcoming, overdue and about to be suspended.
type DigestData struct {
	Subject   string       `json:"subject"`
	Today     string       `json:"today"`
	Suspends  []DigestItem `json:"suspensions"` // suspended within 7 days unless paid
	Overdue   []DigestItem `json:"overdue"`
	DueSoon   []DigestItem `json:"due_soon"` // due in the next 7 days
	Suspended []DigestItem `json:"suspended"`
	Drafts    []DigestItem `json:"drafts"`
	Domains   []DomainDue  `json:"domains"`
	Tasks     []Task       `json:"tasks"`
	Count     int          `json:"count"`
}

// Digest collects the next 7 days, everything overdue, the suspensions of
// the next 7 days, suspended services, unsent invoices, domains expiring in
// 30 days and open provider tasks.
func (s *Service) Digest(org string) DigestData {
	today := s.Today(org)
	d := DigestData{Today: DayStr(today), Suspends: []DigestItem{}, Overdue: []DigestItem{}, DueSoon: []DigestItem{}, Suspended: []DigestItem{}, Drafts: []DigestItem{}}
	subs := s.List(org, Filter{Status: "open"})
	byID := map[string]*Subscription{}
	ids := []string{}
	for i := range subs {
		byID[subs[i].ID] = &subs[i]
		ids = append(ids, subs[i].ID)
	}
	var pers []BillingPeriod
	if len(ids) > 0 {
		s.DB.Where("subscription_id IN ? AND status = ?", ids, PerInvoiced).Order("due_date").Find(&pers)
	}
	firstOf := map[string]bool{}
	for _, p := range pers {
		sub := byID[p.SubscriptionID]
		it := DigestItem{SubscriptionID: sub.ID, Number: sub.Number, Customer: sub.CustomerName, Phone: sub.CustomerPhone, Service: sub.Name,
			Invoice: p.InvoiceNumber, InvoiceID: p.InvoiceID, Balance: p.Balance, Currency: p.Currency, Due: DayStr(p.DueDate), Days: DaysBetween(p.DueDate, today),
			Status: sub.Status, NoSuspend: p.NoSuspend || !sub.AutoSuspend}
		if !p.Sent {
			d.Drafts = append(d.Drafts, it)
			continue
		}
		first := !firstOf[sub.ID]
		firstOf[sub.ID] = true
		if first && sub.Open != nil {
			it.SuspendOn, it.DaysToSuspend = sub.Open.SuspendOn, sub.Open.DaysToSuspend
			if sub.Status == StatusSuspended {
				d.Suspended = append(d.Suspended, it)
				continue
			}
			if it.DaysToSuspend != nil && *it.DaysToSuspend >= 0 && *it.DaysToSuspend <= 7 {
				d.Suspends = append(d.Suspends, it)
				continue
			}
		}
		switch {
		case it.Days > 0:
			d.Overdue = append(d.Overdue, it)
		case it.Days >= -7:
			d.DueSoon = append(d.DueSoon, it)
		}
	}
	sort.SliceStable(d.Suspends, func(i, j int) bool { return d.Suspends[i].SuspendOn < d.Suspends[j].SuspendOn })
	d.Domains = s.DomainsDue(org, subs, today, 30)
	d.Tasks = s.Tasks(org, TaskFilter{Status: TaskOpen})
	d.Count = len(d.Suspends) + len(d.Overdue) + len(d.DueSoon) + len(d.Suspended) + len(d.Drafts) + len(d.Tasks) + len(d.Domains)
	parts := []string{}
	if n := len(d.Suspends); n > 0 {
		parts = append(parts, fmt.Sprintf("%d to be suspended this week", n))
	}
	if n := len(d.Overdue); n > 0 {
		parts = append(parts, fmt.Sprintf("%d overdue", n))
	}
	if n := len(d.Suspended); n > 0 {
		parts = append(parts, fmt.Sprintf("%d suspended", n))
	}
	if n := len(d.DueSoon); n > 0 {
		parts = append(parts, fmt.Sprintf("%d due in 7 days", n))
	}
	if n := len(d.Domains); n > 0 {
		parts = append(parts, fmt.Sprintf("%d domain%s to renew", n, plural(n)))
	}
	if n := len(d.Tasks); n > 0 {
		parts = append(parts, fmt.Sprintf("%d task%s", n, plural(n)))
	}
	if n := len(d.Drafts); n > 0 {
		parts = append(parts, fmt.Sprintf("%d draft%s to send", n, plural(n)))
	}
	if len(parts) == 0 {
		d.Subject = "Client billing today: nothing due, overdue or about to be suspended"
	} else {
		d.Subject = "Client billing today: " + strings.Join(parts, ", ")
	}
	return d
}

// SendDigest emails the digest once per key (manual = always). An automatic
// digest with nothing in it is not sent.
func (s *Service) SendDigest(ctx context.Context, org, key string, manual bool) (string, error) {
	st := s.SettingsFor(org)
	today := s.Today(org)
	if manual {
		key = fmt.Sprintf("digest|manual|%d", s.now().UnixNano())
	}
	row, ok := s.claim(org, Event{Key: key, Stage: "digest", Audience: ToAdmin, Day: today})
	if !ok {
		return "", nil
	}
	d := s.Digest(org)
	markDay := func() {
		if !manual {
			s.ensureSettings(s.DB, org)
			s.DB.Model(&Settings{}).Where("organization_id = ?", org).Update("last_digest", DayStr(today))
		}
	}
	if d.Count == 0 && !manual {
		row.Subject = d.Subject
		s.finish(row, "skipped", nil)
		markDay()
		return "skipped", nil
	}
	inApp := 0
	if st.InApp {
		inApp, _ = notify.Send(s.DB, org, notify.Admins(), notify.Message{Type: "subscriptions.digest", Severity: notify.Info, Title: d.Subject,
			Body: fmt.Sprintf("%d to be suspended this week · %d overdue · %d suspended · %d due in 7 days · %d domains to renew", len(d.Suspends), len(d.Overdue), len(d.Suspended),
				len(d.DueSoon), len(d.Domains)), Link: "/subscriptions", EntityType: "subscription_digest", EntityID: clip(key, 250)})
	}
	to := s.AdminRecipients(org, st)
	row.Subject, row.Recipients, row.InApp = d.Subject, strings.Join(to, ", "), inApp
	var err error
	if len(to) == 0 {
		err = errors.New("no administrator gets client billing emails — check Settings → Who gets emails")
	} else {
		_, err = s.Mail.SendAs(ctx, org, s.digestMessage(org, d, to))
	}
	s.finish(row, "sent", err)
	if err != nil {
		return "failed", err
	}
	markDay()
	return "sent", nil
}

func (s *Service) digestMessage(org string, d DigestData, to []string) mail.Message {
	info := s.orgInfo(org)
	var b strings.Builder
	line := func(it DigestItem, extra string) {
		b.WriteString(fmt.Sprintf("• %s — %s · %s %s", it.Customer, it.Service, it.Invoice, money(it.Balance, it.Currency)))
		if extra != "" {
			b.WriteString(" · " + extra)
		}
		if it.Phone != "" {
			b.WriteString(" · " + it.Phone)
		}
		if u := s.appLink(subLink(it.SubscriptionID, "")); u != "" {
			b.WriteString(" — " + u)
		}
		b.WriteString("\n")
	}
	day := func(v string) string {
		if t, ok := ParseDay(v); ok {
			return shortDate(t)
		}
		return v
	}
	if len(d.Suspends) > 0 {
		b.WriteString(fmt.Sprintf("TO BE SUSPENDED THIS WEEK UNLESS PAID (%d) — call them\n", len(d.Suspends)))
		for _, it := range d.Suspends {
			when := "suspended " + day(it.SuspendOn)
			if it.DaysToSuspend != nil {
				switch *it.DaysToSuspend {
				case 0:
					when = "suspended today"
				case 1:
					when = "suspended tomorrow"
				default:
					when = fmt.Sprintf("suspended in %d days (%s)", *it.DaysToSuspend, day(it.SuspendOn))
				}
			}
			line(it, when)
		}
		b.WriteString("\n")
	}
	if len(d.Overdue) > 0 {
		b.WriteString(fmt.Sprintf("OVERDUE (%d)\n", len(d.Overdue)))
		for _, it := range d.Overdue {
			extra := fmt.Sprintf("%d day%s overdue", it.Days, plural(it.Days))
			if it.NoSuspend {
				extra += ", not suspended"
			} else if it.SuspendOn != "" {
				extra += ", suspended " + day(it.SuspendOn)
			}
			line(it, extra)
		}
		b.WriteString("\n")
	}
	if len(d.Suspended) > 0 {
		b.WriteString(fmt.Sprintf("SUSPENDED (%d)\n", len(d.Suspended)))
		for _, it := range d.Suspended {
			line(it, "until paid")
		}
		b.WriteString("\n")
	}
	if len(d.DueSoon) > 0 {
		b.WriteString(fmt.Sprintf("DUE IN THE NEXT 7 DAYS (%d)\n", len(d.DueSoon)))
		for _, it := range d.DueSoon {
			line(it, "due "+day(it.Due))
		}
		b.WriteString("\n")
	}
	if len(d.Domains) > 0 {
		b.WriteString(fmt.Sprintf("DOMAINS EXPIRING IN 30 DAYS (%d)\n", len(d.Domains)))
		for _, x := range d.Domains {
			b.WriteString(fmt.Sprintf("• %s — %s · expires %s (%d days)%s\n", x.Domain, firstNonEmpty(x.Customer, "no client"), day(x.Expires), x.DaysLeft,
				map[bool]string{true: " · auto-renew on at Synergy", false: ""}[x.AutoRenew]))
		}
		b.WriteString("\n")
	}
	if len(d.Drafts) > 0 {
		b.WriteString(fmt.Sprintf("DRAFT INVOICES TO SEND (%d)\n", len(d.Drafts)))
		for _, it := range d.Drafts {
			line(it, "due "+day(it.Due))
		}
		b.WriteString("\n")
	}
	if len(d.Tasks) > 0 {
		b.WriteString(fmt.Sprintf("TO DO AT THE PROVIDERS (%d)\n", len(d.Tasks)))
		for _, t := range d.Tasks {
			b.WriteString("• " + t.Title + "\n")
		}
		b.WriteString("\n")
	}
	if d.Count == 0 {
		b.WriteString("Nothing is due in the next 7 days, overdue or about to be suspended.\n")
	}
	facts := []invoicing.EmailRow{{Label: "Suspended this week unless paid", Value: fmt.Sprint(len(d.Suspends))}, {Label: "Overdue", Value: fmt.Sprint(len(d.Overdue))},
		{Label: "Suspended", Value: fmt.Sprint(len(d.Suspended))}, {Label: "Due in 7 days", Value: fmt.Sprint(len(d.DueSoon))},
		{Label: "Domains to renew", Value: fmt.Sprint(len(d.Domains))}, {Label: "Tasks", Value: fmt.Sprint(len(d.Tasks))}}
	tone, status := "ok", "All on track"
	switch {
	case len(d.Suspends) > 0 || len(d.Suspended) > 0:
		tone, status = "danger", fmt.Sprintf("%d to be suspended · %d suspended", len(d.Suspends), len(d.Suspended))
	case len(d.Overdue) > 0:
		tone, status = "warn", fmt.Sprintf("%d overdue", len(d.Overdue))
	case len(d.DueSoon) > 0:
		tone, status = "info", fmt.Sprintf("%d due soon", len(d.DueSoon))
	}
	actions := []invoicing.EmailAction{}
	if u := s.appLink("/subscriptions"); u != "" {
		actions = append(actions, invoicing.EmailAction{Label: "Open subscriptions", URL: u, Primary: true})
	}
	dt, _ := ParseDay(d.Today)
	v := invoicing.EmailView{Kind: "reminder", Business: invoicing.EmailBusiness{Name: info.Name, Address: info.Address, Email: info.Email, Phone: info.Phone},
		LogoURL: info.Logo, Accent: info.Accent, Preheader: d.Subject, DocLabel: "Daily client billing digest", Number: dt.Format("2 Jan 2006"),
		Message: b.String(), StatusLine: status, StatusTone: tone, Facts: facts, Actions: actions,
		Note: "Sent daily to the administrators of " + info.Name + ". Change it in Subscriptions → Settings, or who gets it in Settings → Who gets emails."}
	h, t := invoicing.RenderEmail(v)
	return mail.Message{To: to, Subject: d.Subject, HTML: h, Text: t}
}

// ensureNotice keeps the 3-day notice when the suspension of the unpaid
// invoice is switched back on after its date passed (auto-suspend turned
// on again, "don't suspend this time" undone): it moves to MinNoticeDays
// from today, and the admins are alerted today.
func (s *Service) ensureNotice(sub *Subscription, st Settings, today time.Time) {
	if !sub.AutoSuspend || sub.Status == StatusSuspended || sub.Status == StatusCancelled {
		return
	}
	var per BillingPeriod
	if !found(s.DB.Where("subscription_id = ? AND status = ? AND sent AND NOT no_suspend", sub.ID, PerInvoiced).Order("due_date, period_start"), &per) {
		return
	}
	base := Day(per.DueDate).AddDate(0, 0, graceFor(sub, st))
	min := today.AddDate(0, 0, MinNoticeDays)
	if base.AddDate(0, 0, per.ExtraDays).Before(min) {
		s.DB.Model(&BillingPeriod{}).Where("id = ?", per.ID).Updates(map[string]any{"extra_days": DaysBetween(base, min), "updated_at": s.now()})
	}
}
