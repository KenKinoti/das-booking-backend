package subscriptions

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// Target is one service at a provider: a hosted site (hosting) or a
// registered domain.
type Target struct {
	SubscriptionID string
	Number         string
	Customer       string
	Supplier       string // synergy | hosting_com | other | ""
	Plan           string
	Domain         string
	Kind           string // hosting | domain | service
	Reason         string // e.g. "Unpaid invoice INV-1004"
	Ref            string // the provider's id once known (Synergy hosting ID)
}

// SupplierActions suspends and unsuspends hosting at a provider through its
// API (Synergy Wholesale: see synergy.go). Anything it does not handle
// becomes a task for an administrator.
type SupplierActions interface {
	// Handles reports whether t is suspended through the API for org, with
	// the provider's id of the service (and why not, in words, when it isn't).
	Handles(ctx context.Context, org string, t Target) (ref string, ok bool, why string)
	// Suspend / Unsuspend do it and say what was done.
	Suspend(ctx context.Context, org string, t Target) (string, error)
	Unsuspend(ctx context.Context, org string, t Target) (string, error)
}

// targets are the subscription's hosted sites (suspended) — or, for a
// domain-only subscription, its domain (its renewal is held) — or the
// service itself when no site or domain is linked.
func (s *Service) targets(sub *Subscription, customer, reason string) []Target {
	base := Target{SubscriptionID: sub.ID, Number: sub.Number, Customer: customer, Supplier: sub.Supplier, Plan: sub.SupplierPlan, Reason: reason}
	out := []Target{}
	seen := map[string]bool{}
	for _, d := range append([]string{sub.SiteDomain}, sub.Domains...) {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		t := base
		t.Domain, t.Kind = d, KindHosting
		out = append(out, t)
	}
	if len(out) == 0 && sub.Domain != "" {
		t := base
		t.Domain, t.Kind = sub.Domain, KindDomain
		out = append(out, t)
	}
	if len(out) == 0 {
		t := base
		t.Kind = "service"
		out = append(out, t)
	}
	return out
}

// automatable reports whether a suspension of sub goes through the API.
func (s *Service) automatable(org string, sub *Subscription) bool {
	if s.Supplier == nil || !s.SettingsFor(org).UseSynergyAPI {
		return false
	}
	for _, t := range s.targets(sub, "", "") {
		if t.Kind == KindHosting {
			if _, ok, _ := s.Supplier.Handles(context.Background(), org, t); ok {
				return true
			}
		}
	}
	return false
}

func providerName(sup string) string {
	return firstNonEmpty(SupplierLabel(sup), "the provider")
}

// TaskSteps are the exact steps for an administrator.
func TaskSteps(kind string, t Target) []string {
	site := firstNonEmpty(t.Domain, "the client's service")
	plan := ""
	if t.Plan != "" {
		plan = " (" + t.Plan + ")"
	}
	ref := ""
	if t.Reason != "" {
		ref = " — reason: " + t.Reason
	}
	var steps []string
	if t.Kind == KindDomain {
		switch kind {
		case TaskSuspend:
			steps = append(steps, fmt.Sprintf("%s is a domain only: leave it working (switching DNS off takes the client's email down too), but don't renew it at %s until the invoice is paid%s.", site, providerName(t.Supplier), ref))
			steps = append(steps, "If its auto-renew is on at the registrar, turn it off for now so it is not renewed at your cost.")
		case TaskRestore:
			steps = append(steps, fmt.Sprintf("Turn %s's auto-renew back on at %s, and renew it if it has expired.", site, providerName(t.Supplier)))
		case TaskCancel:
			steps = append(steps, fmt.Sprintf("Turn auto-renew off for %s at %s so it is not renewed at your cost (or transfer it out if the client asks).", site, providerName(t.Supplier)))
		}
		return append(steps, "Mark this task done.")
	}
	if t.Kind == "service" {
		steps = append(steps, map[string]string{TaskSuspend: "Stop the service for " + firstNonEmpty(t.Customer, "the client") + " until the invoice is paid" + ref + ".",
			TaskRestore: "Restart the service for " + firstNonEmpty(t.Customer, "the client") + ".",
			TaskCancel:  "Stop the service for " + firstNonEmpty(t.Customer, "the client") + " for good."}[kind])
		return append(steps, "Mark this task done.")
	}
	switch t.Supplier {
	case SupplierSynergy, "":
		who := "Synergy Wholesale (manage.synergywholesale.com)"
		if t.Supplier == "" {
			who = "the provider that hosts it (none is set on the subscription — add it so the steps are exact)"
		}
		steps = append(steps, "Sign in to "+who+".")
		switch kind {
		case TaskSuspend:
			steps = append(steps, fmt.Sprintf("Hosting → Manage → find %s%s → Suspend%s.", site, plan, ref))
		case TaskRestore:
			steps = append(steps, fmt.Sprintf("Hosting → Manage → find %s%s → Unsuspend.", site, plan))
		case TaskCancel:
			steps = append(steps, fmt.Sprintf("Hosting → Manage → %s%s → take a final backup (cPanel → Backup), then cancel the service.", site, plan))
		}
	case SupplierHostingCom:
		steps = append(steps, "Sign in to hosting.com (my.hosting.com).")
		switch kind {
		case TaskSuspend:
			steps = append(steps, fmt.Sprintf("Services → %s%s → Suspend the service%s (or open a ticket asking hosting.com to suspend it).", site, plan, ref))
		case TaskRestore:
			steps = append(steps, fmt.Sprintf("Services → %s%s → Unsuspend (or ask hosting.com support to unsuspend it).", site, plan))
		case TaskCancel:
			steps = append(steps, fmt.Sprintf("Services → %s%s → back it up, then Request cancellation.", site, plan))
		}
	default:
		switch kind {
		case TaskSuspend:
			steps = append(steps, fmt.Sprintf("Suspend %s%s at the provider that hosts it%s.", site, plan, ref))
		case TaskRestore:
			steps = append(steps, fmt.Sprintf("Unsuspend %s%s at the provider that hosts it.", site, plan))
		case TaskCancel:
			steps = append(steps, fmt.Sprintf("Back up and cancel %s%s at the provider; turn off any auto-renewal.", site, plan))
		}
	}
	switch kind {
	case TaskSuspend:
		steps = append(steps, "Check the website now shows the provider's suspended page, then mark this task done.")
	case TaskRestore:
		steps = append(steps, "Check the website and email work again, then mark this task done.")
	case TaskCancel:
		steps = append(steps, "Mark this task done once the service is cancelled.")
	}
	return steps
}

// TaskTitle is "Suspend wanjiku.co.ke manually at hosting.com".
func TaskTitle(kind string, t Target) string {
	what := firstNonEmpty(t.Domain, "the service")
	if t.Kind == "service" && t.Customer != "" {
		what = t.Customer + "'s service"
	}
	at := providerName(t.Supplier)
	if t.Kind == KindDomain {
		switch kind {
		case TaskSuspend:
			return fmt.Sprintf("Hold the renewal of %s at %s until paid", what, at)
		case TaskRestore:
			return fmt.Sprintf("Turn %s's renewal back on at %s", what, at)
		}
		return fmt.Sprintf("Turn off auto-renew for %s at %s", what, at)
	}
	switch kind {
	case TaskSuspend:
		return fmt.Sprintf("Suspend %s manually at %s", what, at)
	case TaskRestore:
		return fmt.Sprintf("Unsuspend %s manually at %s", what, at)
	}
	return fmt.Sprintf("Cancel %s at %s", what, at)
}

func (s *Service) addTask(org string, sub *Subscription, pid, kind, key string, t Target, today time.Time, done bool, note string) *Task {
	now := s.now()
	tk := Task{ID: uuid.NewString(), OrganizationID: org, Key: clip(key, 250), SubscriptionID: sub.ID, PeriodID: pid, Kind: kind, Status: TaskOpen,
		Title: clip(TaskTitle(kind, t), 480), Service: clip(sub.Name, 480), Supplier: sub.Supplier, SupplierPlan: sub.SupplierPlan, Domain: t.Domain, Target: t.Kind,
		Steps: TaskSteps(kind, t), DueDate: today, Note: clip(note, 990), ProviderRef: clip(t.Ref, 60), CreatedAt: now, UpdatedAt: now}
	if done {
		tk.Status, tk.Automated, tk.DoneAt, tk.DoneBy = TaskDone, true, &now, "system"
		verb := map[string]string{TaskSuspend: "Suspended", TaskRestore: "Unsuspended"}[kind]
		tk.Title = clip(fmt.Sprintf("%s %s at %s (API)", verb, firstNonEmpty(t.Domain, "the service"), providerName(t.Supplier)), 480)
	}
	tx := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&tk)
	if tx.Error != nil || tx.RowsAffected == 0 {
		var ex Task
		if found(s.DB.Where("organization_id = ? AND key = ?", org, tk.Key), &ex) {
			return &ex
		}
		return nil
	}
	return &tk
}

// suspendResult says what happened at the providers.
type suspendResult struct {
	Via    string
	Done   []string // done through the API
	Manual []*Task  // tasks for an administrator
}

// atProviders suspends (or unsuspends) every target: through the API when
// it handles it, else as a task.
func (s *Service) suspendAtProviders(ctx context.Context, org string, sub *Subscription, pid string, ts []Target, st Settings, today time.Time) suspendResult {
	r := suspendResult{}
	api, manual := 0, 0
	for _, t := range ts {
		key := "suspend|" + firstNonEmpty(pid, sub.ID) + "|" + DayStr(today) + "|" + firstNonEmpty(t.Domain, t.Kind)
		if t.Kind == KindHosting && s.Supplier != nil && st.UseSynergyAPI {
			if ref, ok, why := s.Supplier.Handles(ctx, org, t); ok {
				t.Supplier, t.Ref = SupplierSynergy, ref
				msg, err := s.Supplier.Suspend(ctx, org, t)
				if err == nil {
					s.addTask(org, sub, pid, TaskSuspend, key, t, today, true, msg)
					s.record(org, Event{Key: key + "|api", SubscriptionID: sub.ID, PeriodID: pid, Stage: "provider", Audience: ToAdmin, Day: today, Subject: msg})
					r.Done = append(r.Done, msg)
					api++
					continue
				}
				note := "The Synergy Wholesale API refused it: " + err.Error() + " — suspend it by hand."
				s.record(org, Event{Key: key + "|api", SubscriptionID: sub.ID, PeriodID: pid, Stage: "provider", Audience: ToAdmin, Day: today, Status: "failed",
					Subject: "Suspending " + t.Domain + " at Synergy Wholesale failed", Error: clip(err.Error(), 900)})
				if tk := s.addTask(org, sub, pid, TaskSuspend, key, t, today, false, note); tk != nil {
					r.Manual = append(r.Manual, tk)
				}
				manual++
				continue
			} else if why != "" {
				if tk := s.addTask(org, sub, pid, TaskSuspend, key, t, today, false, why); tk != nil {
					r.Manual = append(r.Manual, tk)
				}
				manual++
				continue
			}
		}
		if tk := s.addTask(org, sub, pid, TaskSuspend, key, t, today, false, ""); tk != nil {
			r.Manual = append(r.Manual, tk)
		}
		manual++
	}
	switch {
	case api > 0 && manual > 0:
		r.Via = ViaMixed
	case api > 0:
		r.Via = ViaAPI
	default:
		r.Via = ViaManual
	}
	return r
}

// suspend marks the subscription suspended, suspends it at the providers
// (API or tasks) and tells the client and the admins. why: unpaid | admin.
func (s *Service) suspend(ctx context.Context, org string, sub *Subscription, per *BillingPeriod, st Settings, today time.Time, why, note string, res *RunResult) bool {
	key := per.ID + "|suspend|" + DayStr(today)
	if why == "admin" {
		key = fmt.Sprintf("%s|suspend_now|%d", per.ID, s.now().UnixNano())
	}
	stage := "suspend"
	if why == "admin" {
		stage = "suspend_now"
	}
	row, ok := s.claim(org, Event{Key: key, SubscriptionID: sub.ID, PeriodID: per.ID, Stage: stage, Audience: ToAdmin, Day: today})
	if !ok {
		return false
	}
	upd := s.DB.Model(&Subscription{}).Where("id = ? AND status NOT IN ?", sub.ID, []string{StatusSuspended, StatusCancelled}).
		Updates(map[string]any{"status": StatusSuspended, "suspended_at": today, "updated_at": s.now()})
	if upd.RowsAffected == 0 {
		s.finish(row, "skipped", nil)
		return false
	}
	sub.Status, sub.SuspendedAt = StatusSuspended, &today
	cu, _ := s.customer(org, sub.CustomerID)
	reason := "unpaid invoice " + per.InvoiceNumber
	r := s.suspendAtProviders(ctx, org, sub, per.ID, s.targets(sub, cu.Name, reason), st, today)
	s.DB.Model(&Subscription{}).Where("id = ?", sub.ID).Update("suspended_via", r.Via)
	sub.SuspendedVia = r.Via
	if res != nil {
		res.Suspended++
		res.logf("%s: suspended (%s, %s)", sub.Number, reason, r.Via)
	}
	// The client is told (the invoice's reminder email with the pay link).
	if who, err := s.actor(org, sub.CreatedBy); err == nil {
		subject, msg := s.suspendedMessage(org, sub, per.DueDate)
		if crow, ok := s.claim(org, Event{Key: key + "|client", SubscriptionID: sub.ID, PeriodID: per.ID, Stage: "suspend", Audience: ToClient, Day: today}); ok {
			crow.Subject, crow.Recipients = s.sentSubject(org, subject, per.InvoiceNumber), cu.Email
			err := s.clientReminder(ctx, who, sub, per, subject, msg)
			s.finish(crow, "sent", err)
			if err != nil && res != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: suspension notice: %v", sub.Number, err))
			}
		}
	}
	d := Dates{Due: per.DueDate, Suspend: &today}
	a := Alert{Type: "suspended", Severity: notify.Error, Link: subLink(sub.ID, ""), LinkText: "Open the subscription", Entity: sub.ID,
		Facts: s.callFacts(org, sub, per, d), Amount: money(per.Balance, per.Currency), Tone: "danger", Status: "Suspended", Label: "Client billing",
		Quick: quickActions(sub, true)}
	who := "Invoice " + per.InvoiceNumber + " (" + money(per.Balance, per.Currency) + ") is unpaid"
	if why == "admin" {
		who = "An administrator suspended it" + optNote(note)
	}
	a.Lead = fmt.Sprintf("%s, so %s was suspended today and the client was told.", who, serviceName(sub))
	switch {
	case len(r.Manual) == 0:
		a.Subject = fmt.Sprintf("Suspended: %s — %s at %s", firstNonEmpty(cu.Name, "Client"), serviceName(sub), providerName(sub.Supplier))
		a.Body = "Done automatically: " + strings.Join(r.Done, " ") + "\n\nPayment at any time unsuspends it the same way."
	default:
		t := r.Manual[0]
		a.Subject = fmt.Sprintf("Suspended: %s — %s", firstNonEmpty(cu.Name, "Client"), t.Title)
		var b strings.Builder
		if len(r.Done) > 0 {
			b.WriteString("Done automatically: " + strings.Join(r.Done, " ") + "\n\n")
		}
		for _, t := range r.Manual {
			b.WriteString(t.Title + ":\n• " + strings.Join(t.Steps, "\n• ") + "\n")
			if t.Note != "" {
				b.WriteString("(" + t.Note + ")\n")
			}
			b.WriteString("\n")
		}
		b.WriteString("Then mark the task done in DASYIN. Payment at any time unsuspends the subscription; you'll get an \"Unsuspend\" task.")
		a.Body = b.String()
		a.LinkText = "Open the task"
	}
	to, inApp, err := s.alertAdmins(ctx, org, st, a)
	row.Subject, row.Recipients, row.InApp = a.Subject, strings.Join(to, ", "), inApp
	s.finish(row, "sent", err)
	return true
}

func optNote(n string) string {
	if strings.TrimSpace(n) == "" {
		return ""
	}
	return " — " + clip(n, 300)
}

// unsuspend restores a suspended subscription: paid (why "paid") or by an
// administrator ("admin", with extraDays more time on the unpaid invoice).
// What was suspended through the API is unsuspended through it; what an
// admin suspended by hand gets an "Unsuspend … manually" task; open suspend
// tasks are no longer needed.
func (s *Service) unsuspend(ctx context.Context, org string, sub *Subscription, st Settings, today time.Time, why, note string, extraDays int, res *RunResult) bool {
	var overdue int64
	s.DB.Model(&BillingPeriod{}).Where("subscription_id = ? AND status = ? AND due_date < ?", sub.ID, PerInvoiced, today).Count(&overdue)
	want := StatusActive
	if overdue > 0 {
		want = StatusPastDue
	}
	if s.DB.Model(&Subscription{}).Where("id = ? AND status = ?", sub.ID, StatusSuspended).Updates(map[string]any{"status": want, "suspended_at": nil, "suspended_via": "",
		"updated_at": s.now()}).RowsAffected == 0 {
		return false
	}
	since := today
	if sub.SuspendedAt != nil {
		since = Day(*sub.SuspendedAt)
	}
	sub.Status, sub.SuspendedAt = want, nil
	if res != nil {
		res.Unsuspended++
		res.logf("%s: unsuspended (%s)", sub.Number, why)
	}
	cu, _ := s.customer(org, sub.CustomerID)
	var tasks []Task
	s.DB.Where("subscription_id = ? AND kind = ? AND status IN ?", sub.ID, TaskSuspend, []string{TaskOpen, TaskDone}).Order("created_at").Find(&tasks)
	done, manual, dismissed := []string{}, []*Task{}, 0
	for i := range tasks {
		t := &tasks[i]
		var has int64
		s.DB.Model(&Task{}).Where("organization_id = ? AND key = ?", org, "restore|"+t.ID).Count(&has)
		if has > 0 {
			continue
		}
		if t.Status == TaskOpen {
			s.DB.Model(&Task{}).Where("id = ? AND status = ?", t.ID, TaskOpen).Updates(map[string]any{"status": TaskDismissed,
				"note": clip(strings.TrimSpace(t.Note+" Not needed: unsuspended before it was done at the provider."), 990), "done_at": s.now(), "updated_at": s.now()})
			dismissed++
			continue
		}
		tg := Target{SubscriptionID: sub.ID, Number: sub.Number, Customer: cu.Name, Supplier: t.Supplier, Plan: t.SupplierPlan, Domain: t.Domain, Kind: firstNonEmpty(t.Target, KindHosting),
			Ref: t.ProviderRef}
		if t.Automated {
			tg.Supplier = SupplierSynergy
		}
		key := "restore|" + t.ID
		if t.Automated && s.Supplier != nil {
			msg, err := s.Supplier.Unsuspend(ctx, org, tg)
			if err == nil {
				s.addTask(org, sub, t.PeriodID, TaskRestore, key, tg, today, true, msg)
				s.record(org, Event{Key: key + "|api", SubscriptionID: sub.ID, PeriodID: t.PeriodID, Stage: "provider", Audience: ToAdmin, Day: today, Subject: msg})
				done = append(done, msg)
				continue
			}
			s.record(org, Event{Key: key + "|api", SubscriptionID: sub.ID, PeriodID: t.PeriodID, Stage: "provider", Audience: ToAdmin, Day: today, Status: "failed",
				Subject: "Unsuspending " + t.Domain + " at Synergy Wholesale failed", Error: clip(err.Error(), 900)})
			if tk := s.addTask(org, sub, t.PeriodID, TaskRestore, key, tg, today, false, "The Synergy Wholesale API refused it: "+err.Error()+" — unsuspend it by hand."); tk != nil {
				manual = append(manual, tk)
			}
			continue
		}
		if tk := s.addTask(org, sub, t.PeriodID, TaskRestore, key, tg, today, false, ""); tk != nil {
			manual = append(manual, tk)
		}
	}
	evKey := fmt.Sprintf("%s|unsuspend|%s|%s", sub.ID, DayStr(since), DayStr(today))
	s.record(org, Event{Key: evKey, SubscriptionID: sub.ID, Stage: "unsuspend", Audience: ToAdmin, Day: today,
		Subject: map[string]string{"paid": "Unsuspended: paid", "admin": "Unsuspended by an administrator" + optNote(note)}[why]})
	if why == "paid" {
		subject := fmt.Sprintf("Payment received — your %s is active again", serviceName(sub))
		first := "there"
		if f := strings.Fields(cu.Contact + " " + cu.Name); len(f) > 0 {
			first = f[0]
		}
		msg := fmt.Sprintf("Hi %s,\n\nThank you — we've received your payment, and your %s is active again.", first, serviceName(sub)) +
			" If anything still doesn't work in the next hour, just reply to this email.\n\n" + s.orgInfo(org).Name
		if crow, ok := s.claim(org, Event{Key: evKey + "|client", SubscriptionID: sub.ID, Stage: "unsuspend", Audience: ToClient, Day: today}); ok {
			to, err := s.clientDirect(ctx, org, sub, subject, msg, "Active", "ok", []invoicing.EmailRow{{Label: "Service", Value: sub.Name}})
			crow.Subject, crow.Recipients = subject, to
			s.finish(crow, "sent", err)
		}
	}
	a := Alert{Type: "unsuspended", Severity: notify.Success, Link: subLink(sub.ID, ""), Entity: sub.ID, Tone: "ok", Status: "Active again", Label: "Client billing"}
	if why == "paid" {
		a.Subject = fmt.Sprintf("%s paid — %s is unsuspended", firstNonEmpty(cu.Name, "The client"), serviceName(sub))
		a.Lead = fmt.Sprintf("The overdue invoice for %s was paid, so the subscription is active again.", sub.Name)
	} else {
		a.Subject = fmt.Sprintf("%s was unsuspended by an administrator", sub.Name)
		a.Lead = fmt.Sprintf("%s is running again%s.", sub.Name, optNote(note))
		if extraDays > 0 {
			a.Lead += fmt.Sprintf(" The client has %d more days to pay.", extraDays)
		}
	}
	var b strings.Builder
	if len(done) > 0 {
		b.WriteString("Done automatically: " + strings.Join(done, " ") + "\n\n")
	}
	for _, t := range manual {
		b.WriteString(t.Title + ":\n• " + strings.Join(t.Steps, "\n• ") + "\n\n")
	}
	if len(manual) > 0 {
		a.Subject += " — " + manual[0].Title
		b.WriteString("Then mark the task done in DASYIN.")
		a.LinkText = "Open the task"
	} else if dismissed > 0 && len(done) == 0 {
		b.WriteString("It had not been suspended at the provider yet, so there is nothing to undo — the suspend task was closed.")
	}
	a.Body = strings.TrimSpace(b.String())
	if arow, ok := s.claim(org, Event{Key: evKey + "|admin", SubscriptionID: sub.ID, Stage: "unsuspend_alert", Audience: ToAdmin, Day: today}); ok {
		to, inApp, err := s.alertAdmins(ctx, org, st, a)
		arow.Subject, arow.Recipients, arow.InApp = a.Subject, strings.Join(to, ", "), inApp
		s.finish(arow, "sent", err)
	}
	return true
}

// cancelTasks adds "Cancel … at the provider" tasks (never done
// automatically: terminating hosting deletes the client's files).
func (s *Service) cancelTasks(ctx context.Context, org string, sub *Subscription, today time.Time) {
	cu, _ := s.customer(org, sub.CustomerID)
	ts := s.targets(sub, cu.Name, "")
	if sub.Domain != "" && sub.Domain != sub.SiteDomain {
		has := false
		for _, t := range ts {
			if t.Domain == sub.Domain {
				has = true
			}
		}
		if !has {
			ts = append(ts, Target{SubscriptionID: sub.ID, Number: sub.Number, Customer: cu.Name, Supplier: sub.Supplier, Domain: sub.Domain, Kind: KindDomain})
		}
	}
	for _, t := range ts {
		s.addTask(org, sub, "", TaskCancel, "cancel|"+sub.ID+"|"+DayStr(today)+"|"+firstNonEmpty(t.Domain, t.Kind), t, today, false, "")
	}
}

// TaskFilter narrows the task list.
type TaskFilter struct {
	Status         string
	SubscriptionID string
}

// Tasks lists provider tasks (open first).
func (s *Service) Tasks(org string, f TaskFilter) []Task {
	out := []Task{}
	q := s.DB.Where("organization_id = ?", org)
	if f.Status != "" && f.Status != "all" {
		q = q.Where("status = ?", f.Status)
	}
	if f.SubscriptionID != "" {
		q = q.Where("subscription_id = ?", f.SubscriptionID)
	}
	q.Order("CASE status WHEN 'open' THEN 0 ELSE 1 END, created_at DESC").Limit(500).Find(&out)
	ids := []string{}
	for _, t := range out {
		ids = append(ids, t.SubscriptionID)
	}
	if len(ids) > 0 {
		var subs []Subscription
		s.DB.Select("id, number, customer_id").Where("organization_id = ? AND id IN ?", org, ids).Find(&subs)
		cids := []string{}
		bySub := map[string]Subscription{}
		for _, x := range subs {
			bySub[x.ID] = x
			cids = append(cids, x.CustomerID)
		}
		cust := s.customers(org, cids)
		for i := range out {
			x := bySub[out[i].SubscriptionID]
			out[i].SubscriptionNumber = x.Number
			out[i].CustomerName = cust[x.CustomerID].Name
		}
	}
	return out
}

// UpdateTask marks a task done (or open again / dismissed) with a note.
func (s *Service) UpdateTask(org, id, user, status, note string) (*Task, error) {
	var t Task
	if s.DB.Where("organization_id = ? AND id = ?", org, id).First(&t).Error != nil {
		return nil, ErrNotFound
	}
	switch status {
	case TaskDone, TaskOpen, TaskDismissed:
	default:
		return nil, invalid("mark the task done, open or dismissed")
	}
	upd := map[string]any{"status": status, "updated_at": s.now()}
	if strings.TrimSpace(note) != "" {
		upd["note"] = clip(note, 990)
	}
	if status == TaskOpen {
		upd["done_at"], upd["done_by"] = nil, ""
	} else {
		upd["done_at"], upd["done_by"] = s.now(), user
	}
	s.DB.Model(&Task{}).Where("id = ?", t.ID).Updates(upd)
	s.DB.Where("id = ?", t.ID).First(&t)
	return &t, nil
}
