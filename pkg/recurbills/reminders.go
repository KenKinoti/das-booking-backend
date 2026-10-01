package recurbills

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
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

func fmtMoney(v float64, cur string) string { return invoicing.FormatMoney(v, cur) }

// adminEmailRoles are the roles whose users receive bill reminders.
var adminEmailRoles = []string{"owner", "admin", "org_admin", "super_admin"}

// Recipients resolves who gets reminders for an organisation (and one
// schedule's extra recipients): admins, the outgoing-email "Notification
// recipients", the reminder settings' own list.
func (s *Service) Recipients(org string, st Settings, sched *RecurringBill) []string {
	var raw []string
	if st.NotifyAdmins {
		var emails []string
		s.DB.Raw(`SELECT email FROM users WHERE organization_id = ? AND deleted_at IS NULL AND (is_active IS NULL OR is_active) AND role IN ? ORDER BY created_at`,
			org, adminEmailRoles).Scan(&emails)
		raw = append(raw, emails...)
	}
	if st.UseNotifyList && s.Mail != nil {
		cfg, _ := s.Mail.Resolve(org)
		raw = append(raw, cfg.Notify...)
	}
	raw = append(raw, st.Recipients...)
	if sched != nil {
		raw = append(raw, sched.ExtraRecipients...)
	}
	v, _ := mail.ParseList(strings.Join(raw, ","))
	return v
}

// RunResult reports one scheduler run for an organisation.
type RunResult struct {
	Organization string    `json:"organization_id"`
	Generated    GenResult `json:"generated"`
	Reminders    int       `json:"reminders_sent"`
	Failed       int       `json:"reminders_failed"`
	InApp        int       `json:"in_app"`
	Digest       string    `json:"digest,omitempty"` // sent | failed | ""
	Sent         []string  `json:"sent"`
	Errors       []string  `json:"errors"`
	SkippedHour  bool      `json:"skipped_send_hour,omitempty"`
}

// RunOptions changes a run.
type RunOptions struct {
	// Force ignores the send hour (manual "Run now").
	Force bool
}

// claim reserves a log key; ok false when it was already sent (or is being
// sent / failed too often). A failed send is retried after 30 minutes, at
// most 3 attempts.
func (s *Service) claim(org string, row ReminderLog) (*ReminderLog, bool) {
	now := s.now()
	row.ID, row.OrganizationID, row.Status, row.Attempts, row.CreatedAt, row.UpdatedAt = uuid.NewString(), org, "sending", 1, now, now
	tx := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if tx.Error == nil && tx.RowsAffected == 1 {
		return &row, true
	}
	var ex ReminderLog
	if s.DB.Where("organization_id = ? AND key = ?", org, row.Key).First(&ex).Error != nil {
		return nil, false
	}
	if ex.Status != "failed" || ex.Attempts >= 3 || now.Sub(ex.UpdatedAt) < 30*time.Minute {
		return nil, false
	}
	res := s.DB.Model(&ReminderLog{}).Where("id = ? AND status = ? AND attempts = ?", ex.ID, "failed", ex.Attempts).
		Updates(map[string]any{"status": "sending", "attempts": ex.Attempts + 1, "updated_at": now})
	if res.RowsAffected != 1 {
		return nil, false
	}
	ex.Attempts++
	return &ex, true
}

func (s *Service) finish(row *ReminderLog, err error, inApp int) {
	upd := map[string]any{"status": "sent", "error": "", "updated_at": s.now(), "recipients": row.Recipients, "subject": row.Subject}
	if inApp > 0 {
		upd["in_app"] = inApp
	}
	if err != nil {
		upd["status"], upd["error"] = "failed", clip(err.Error(), 1000)
	}
	s.DB.Model(&ReminderLog{}).Where("id = ?", row.ID).Updates(upd)
}

// Policy returns the reminder policy for an item ("" reason when it gets reminders).
func (s *Service) Policy(st Settings, it *UpcomingItem) (ReminderPolicy, bool) {
	p := ReminderPolicy{Leads: st.LeadDays, OnDue: st.OnDue, OverdueEvery: st.OverdueEvery, OverdueMax: st.OverdueMax}
	if it.sched != nil {
		if it.sched.RemindersOff {
			return p, false
		}
		if it.sched.ReminderLeads != nil {
			p.Leads = it.sched.ReminderLeads
		}
		return p, true
	}
	return p, st.IncludeOneOff
}

// remindable statuses: still to be paid.
func remindable(status string) bool {
	switch status {
	case "draft", "unpaid", "partial", "overdue", "awaiting":
		return true
	}
	return false
}

// RunOrg generates due bills and sends due reminders and the weekly digest.
func (s *Service) RunOrg(ctx context.Context, org string, opt RunOptions) RunResult {
	res := RunResult{Organization: org, Sent: []string{}, Errors: []string{}}
	res.Generated = s.Generate(org)
	res.Errors = append(res.Errors, res.Generated.Errors...)
	st := s.SettingsFor(org)
	if !st.Enabled {
		return res
	}
	loc := s.Loc(org)
	local := s.now().In(loc)
	if !opt.Force && local.Hour() < st.SendHour {
		res.SkippedHour = true
	} else {
		s.sendReminders(ctx, org, st, &res)
	}
	if st.Digest && int(local.Weekday()) == st.DigestWeekday && local.Hour() >= st.DigestHour {
		y, w := local.ISOWeek()
		period := fmt.Sprintf("%d-W%02d", y, w)
		if st.LastDigestPeriod != period {
			status, err := s.SendDigest(ctx, org, "digest|"+period, period)
			res.Digest = status
			if err != nil {
				res.Errors = append(res.Errors, "digest: "+err.Error())
			}
		}
	}
	now := s.now()
	s.DB.Model(&Settings{}).Where("organization_id = ?", org).Update("last_run_at", now)
	return res
}

func (s *Service) sendReminders(ctx context.Context, org string, st Settings, res *RunResult) {
	today := s.Today(org)
	up := s.UpcomingFor(org, 61)
	for i := range up.Items {
		it := &up.Items[i]
		if !remindable(it.Status) {
			continue
		}
		if it.SnoozedUntil != "" && it.SnoozedUntil > dayStr(today) {
			continue
		}
		pol, on := s.Policy(st, it)
		if !on {
			continue
		}
		stage, ok := pol.Current(it.due, today)
		if !ok {
			continue
		}
		key := it.Key + "|" + stage.Key
		row, ok := s.claim(org, ReminderLog{Key: clip(key, 250), Kind: "reminder", Stage: stage.Key, BillID: it.BillID, RecurringID: it.RecurringID})
		if !ok {
			continue
		}
		to := s.Recipients(org, st, it.sched)
		msg, subject := s.reminderMessage(org, it, stage, to)
		row.Subject, row.Recipients = clip(subject, 480), strings.Join(to, ", ")
		var err error
		if len(to) == 0 {
			err = errors.New("no recipients — add admins with an email or recipients in Settings → Bill reminders")
		} else {
			_, err = s.Mail.SendAs(ctx, org, msg)
		}
		inApp := 0
		if st.InApp && row.Attempts == 1 {
			inApp = s.notifyItem(org, it, stage, subject)
			res.InApp += inApp
		}
		s.finish(row, err, inApp)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, subject+": "+err.Error())
			log.Printf("recurbills: reminder %s for %s: %v", key, org, err)
			continue
		}
		res.Reminders++
		res.Sent = append(res.Sent, subject)
	}
}

func (s *Service) notifyItem(org string, it *UpcomingItem, stage Stage, subject string) int {
	sev := notify.Info
	switch stage.Kind {
	case "due":
		sev = notify.Warning
	case "overdue":
		sev = notify.Error
	}
	link := "/bills?view=upcoming"
	if it.BillID != "" {
		link = "/bills?bill=" + it.BillID
	}
	ent := it.BillID
	if ent == "" {
		ent = it.Key
	}
	n, _ := notify.Send(s.DB, org, notify.Admins(), notify.Message{Type: "bills.reminder." + stage.Kind, Severity: sev, Title: subject,
		Body: itemBody(it), Link: link, EntityType: "bill", EntityID: clip(ent, 250)})
	return n
}

func itemTitle(it *UpcomingItem) string {
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
	return n
}

func itemAmount(it *UpcomingItem, home string) string {
	cur, amt := it.OriginalCurrency, it.OriginalAmount
	if it.Kind == "bill" && (cur == "" || cur == home) {
		cur, amt = home, it.Amount
	}
	if cur == "" {
		cur, amt = home, it.Amount
	}
	s := fmtMoney(amt, cur)
	if it.Estimate {
		s = "~" + s
	}
	return s
}

func itemBody(it *UpcomingItem) string {
	parts := []string{it.Vendor}
	if it.BillNumber != "" {
		parts = append(parts, it.BillNumber)
	}
	if it.AutoPay {
		parts = append(parts, "auto-pay")
	} else {
		parts = append(parts, "manual payment")
	}
	return strings.Join(parts, " · ")
}

func dueWords(days int) string {
	switch {
	case days == 0:
		return "today"
	case days == 1:
		return "tomorrow"
	case days > 1:
		return fmt.Sprintf("in %d days", days)
	case days == -1:
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", -days)
}

// orgInfo is the sender block of emails.
type orgInfo struct {
	Name, Email, Phone, Address, Accent, Logo string
}

func (s *Service) orgInfo(org string) orgInfo {
	var o struct {
		Name, Email, Phone, AddressStreet, AddressSuburb, AddressState, AddressPostcode string
	}
	s.DB.Raw(`SELECT name, COALESCE(email,'') AS email, COALESCE(phone,'') AS phone, COALESCE(address_street,'') AS address_street, COALESCE(address_suburb,'') AS address_suburb,
		COALESCE(address_state,'') AS address_state, COALESCE(address_postcode,'') AS address_postcode FROM organizations WHERE id = ?`, org).Scan(&o)
	addr := strings.Join(nonEmptyStrs(o.AddressStreet, strings.TrimSpace(o.AddressSuburb+" "+o.AddressState+" "+o.AddressPostcode)), ", ")
	info := orgInfo{Name: nz(o.Name, "DASYIN ERP"), Email: o.Email, Phone: o.Phone, Address: addr}
	var accent []string
	s.DB.Raw(`SELECT accent_color FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&accent)
	if len(accent) > 0 {
		info.Accent = accent[0]
	}
	if s.LogoURL != nil {
		info.Logo = s.LogoURL(org)
	}
	return info
}

func nonEmptyStrs(v ...string) []string {
	out := []string{}
	for _, x := range v {
		if strings.TrimSpace(x) != "" {
			out = append(out, strings.TrimSpace(x))
		}
	}
	return out
}

func (s *Service) appLink(path string) string {
	base := strings.TrimRight(strings.TrimSpace(s.AppURL), "/")
	if base == "" {
		return ""
	}
	return base + path
}

// ReminderLinks are the deep links of a reminder.
func (s *Service) ReminderLinks(it *UpcomingItem) (pay, view string) {
	switch {
	case it.BillID != "":
		return s.appLink("/bills?pay=" + it.BillID), s.appLink("/bills?bill=" + it.BillID)
	case it.RecurringID != "":
		return s.appLink("/bills?view=upcoming&paid=" + it.RecurringID + ":" + it.Period), s.appLink("/bills?view=recurring&schedule=" + it.RecurringID)
	}
	return "", s.appLink("/bills?view=upcoming")
}

// reminderMessage renders one reminder email.
func (s *Service) reminderMessage(org string, it *UpcomingItem, stage Stage, to []string) (mail.Message, string) {
	home := s.home(org)
	today := s.Today(org)
	days := daysBetween(today, it.due)
	name := itemTitle(it)
	amount := itemAmount(it, home)
	dueShort := it.due.Format("2 Jan")
	dueLong := it.due.Format("Monday 2 January 2006")
	var subject, msg, status, tone, label string
	switch {
	case days < 0:
		subject = fmt.Sprintf("Overdue: %s %s was due %s (%s) — pay now to keep services online", name, amount, dueShort, dueWords(days))
		msg = fmt.Sprintf("%s for %s was due on %s (%s) and is still unpaid.\n\nPlease pay it now — overdue hosting, domain and software bills can get services suspended.", name, amount, dueLong, dueWords(days))
		status, tone, label = fmt.Sprintf("Overdue — %s", dueWords(days)), "danger", "Overdue bill"
	case days == 0:
		subject = fmt.Sprintf("Due today: %s %s — keep services online", name, amount)
		msg = fmt.Sprintf("%s for %s is due today (%s).", name, amount, dueLong)
		status, tone, label = "Due today", "warn", "Bill due today"
	default:
		subject = fmt.Sprintf("Upcoming: %s %s due %s (%s) — keep services online", name, amount, dueShort, dueWords(days))
		msg = fmt.Sprintf("%s for %s is due on %s — %s.", name, amount, dueLong, dueWords(days))
		status, tone, label = "Due "+dueWords(days), "info", "Upcoming bill"
		if days <= 1 {
			tone = "warn"
		}
	}
	if days >= 0 {
		if it.AutoPay {
			msg += "\n\nIt is paid automatically by card / direct debit — make sure the card is valid and has enough funds so the service stays online."
		} else {
			msg += "\n\nIt needs a manual payment — pay it before the due date so the service stays online."
		}
	} else if it.AutoPay {
		msg += "\n\nThis bill is set to be paid automatically: if it was charged, mark it as paid; if the card was declined, update it and pay now."
	}
	if it.Status == "draft" {
		msg += "\n\nThe bill is still a draft — check it and approve it before paying."
	}
	if it.Estimate {
		msg += " The amount is an estimate — confirm the real amount on the bill."
	}
	if it.Status == "awaiting" || (it.Kind == "projected" && it.Mode == ModeTrack) {
		msg += "\n\nThe supplier's invoice hasn't been imported into Expenses yet; once it is, it is matched to this schedule automatically."
	}
	facts := []invoicing.EmailRow{{Label: "Vendor", Value: nz(it.Vendor, "—")}}
	facts = append(facts, invoicing.EmailRow{Label: "Amount", Value: amount})
	if it.OriginalCurrency != "" && it.OriginalCurrency != home && it.Amount > 0 {
		facts = append(facts, invoicing.EmailRow{Label: "In " + home, Value: "≈ " + fmtMoney(it.Amount, home)})
	}
	facts = append(facts, invoicing.EmailRow{Label: "Due date", Value: it.due.Format("Mon 2 Jan 2006")})
	if svc := strings.TrimSpace(strings.Join(nonEmptyStrs(it.Service, it.SynergyDomain), " · ")); svc != "" {
		facts = append(facts, invoicing.EmailRow{Label: "Service", Value: svc})
	}
	pay := "Needs manual payment"
	if it.AutoPay {
		pay = "Paid automatically (card / direct debit)"
	}
	facts = append(facts, invoicing.EmailRow{Label: "Payment", Value: pay})
	if it.LastPaid != "" {
		if d, ok := ParseDay(it.LastPaid); ok {
			facts = append(facts, invoicing.EmailRow{Label: "Last paid", Value: d.Format("2 Jan 2006")})
		}
	} else if it.RecurringID != "" {
		facts = append(facts, invoicing.EmailRow{Label: "Last paid", Value: "Not recorded yet"})
	}
	if it.BillNumber != "" {
		facts = append(facts, invoicing.EmailRow{Label: "Bill", Value: it.BillNumber})
	}
	payURL, viewURL := s.ReminderLinks(it)
	actions := []invoicing.EmailAction{}
	if payURL != "" {
		actions = append(actions, invoicing.EmailAction{Label: "Mark as paid", URL: payURL, Primary: true})
	}
	if viewURL != "" {
		lbl := "View bill"
		if it.BillID == "" {
			lbl = "View schedule"
		}
		actions = append(actions, invoicing.EmailAction{Label: lbl, URL: viewURL})
	}
	info := s.orgInfo(org)
	v := invoicing.EmailView{Kind: "reminder", Business: invoicing.EmailBusiness{Name: info.Name, Address: info.Address, Email: info.Email, Phone: info.Phone},
		LogoURL: info.Logo, Accent: info.Accent, Preheader: subject, DocLabel: label, Number: it.BillNumber, Message: msg,
		AmountLabel: "Amount due", Amount: amount, StatusLine: status, StatusTone: tone, Facts: facts, Actions: actions,
		Note: "You're receiving this as an administrator of " + info.Name + ". Change bill reminders in Settings → Bill reminders."}
	if days < 0 {
		v.AmountLabel = "Overdue amount"
	}
	htmlOut, text := invoicing.RenderEmail(v)
	return mail.Message{To: to, Subject: subject, HTML: htmlOut, Text: text}, subject
}

// ---------------------------------------------------------------- digest

// DigestData is what the weekly digest shows.
type DigestData struct {
	Subject  string         `json:"subject"`
	Overdue  []UpcomingItem `json:"overdue"`
	Upcoming []UpcomingItem `json:"upcoming"`
	Warnings []string       `json:"warnings"`
	Total    float64        `json:"total"`
	Currency string         `json:"currency"`
}

// Digest collects the next 14 days, everything overdue and balance warnings.
func (s *Service) Digest(org string) DigestData {
	up := s.UpcomingFor(org, 14)
	d := DigestData{Overdue: []UpcomingItem{}, Upcoming: []UpcomingItem{}, Warnings: []string{}, Currency: up.Currency}
	for _, it := range up.Items {
		if !remindable(it.Status) {
			continue
		}
		if it.Status == "overdue" {
			d.Overdue = append(d.Overdue, it)
		} else {
			d.Upcoming = append(d.Upcoming, it)
		}
		d.Total = r2(d.Total + it.Amount)
	}
	if s.Exp != nil {
		for _, a := range s.Exp.BalanceAlerts(org, s.Today(org)) {
			d.Warnings = append(d.Warnings, strings.TrimSpace(a.Title+" — "+a.Body))
		}
	}
	n := len(d.Overdue) + len(d.Upcoming)
	switch {
	case n == 0:
		d.Subject = "Bills this week: nothing due in the next 14 days"
	case len(d.Overdue) > 0:
		d.Subject = fmt.Sprintf("Bills this week: %d overdue, %d due in the next 14 days — %s", len(d.Overdue), len(d.Upcoming), fmtMoney(d.Total, d.Currency))
	default:
		d.Subject = fmt.Sprintf("Bills this week: %d due in the next 14 days — %s", len(d.Upcoming), fmtMoney(d.Total, d.Currency))
	}
	if len(d.Warnings) > 0 {
		d.Subject += " · balance warning"
	}
	return d
}

// SendDigest sends the weekly digest once per key ("" key = manual, always sends).
func (s *Service) SendDigest(ctx context.Context, org, key, period string) (string, error) {
	st := s.SettingsFor(org)
	manual := key == ""
	if manual {
		key = "digest|manual|" + s.now().UTC().Format(time.RFC3339Nano)
	}
	row, ok := s.claim(org, ReminderLog{Key: clip(key, 250), Kind: "digest", Stage: period})
	if !ok {
		return "", nil
	}
	d := s.Digest(org)
	to := s.Recipients(org, st, nil)
	msg := s.digestMessage(org, d, to)
	row.Subject, row.Recipients = clip(d.Subject, 480), strings.Join(to, ", ")
	var err error
	if len(to) == 0 {
		err = errors.New("no recipients — add admins with an email or recipients in Settings → Bill reminders")
	} else {
		_, err = s.Mail.SendAs(ctx, org, msg)
	}
	inApp := 0
	if st.InApp && row.Attempts == 1 {
		inApp, _ = notify.Send(s.DB, org, notify.Admins(), notify.Message{Type: "bills.digest", Severity: notify.Info, Title: d.Subject,
			Body: fmt.Sprintf("%d overdue · %d due in the next 14 days", len(d.Overdue), len(d.Upcoming)), Link: "/bills?view=upcoming", EntityType: "bill_digest", EntityID: clip(key, 250)})
	}
	s.finish(row, err, inApp)
	if !manual && err == nil {
		s.ensureSettingsRow(org)
		s.DB.Model(&Settings{}).Where("organization_id = ?", org).Update("last_digest_period", period)
	}
	if err != nil {
		return "failed", err
	}
	return "sent", nil
}

func (s *Service) ensureSettingsRow(org string) {
	var n int64
	s.DB.Model(&Settings{}).Where("organization_id = ?", org).Count(&n)
	if n == 0 {
		st := DefaultSettings(org)
		st.UpdatedAt = s.now()
		s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&st)
	}
}

func (s *Service) digestMessage(org string, d DigestData, to []string) mail.Message {
	info := s.orgInfo(org)
	var b strings.Builder
	line := func(it UpcomingItem) {
		b.WriteString(fmt.Sprintf("• %s — %s, due %s (%s)", itemTitle(&it), itemAmount(&it, d.Currency), it.due.Format("Mon 2 Jan"), dueWords(it.DaysUntilDue)))
		if it.AutoPay {
			b.WriteString(" · auto-pay")
		}
		if it.Status == "draft" {
			b.WriteString(" · draft to approve")
		}
		b.WriteString("\n")
	}
	if len(d.Overdue) > 0 {
		b.WriteString(fmt.Sprintf("OVERDUE (%d)\n", len(d.Overdue)))
		for _, it := range d.Overdue {
			line(it)
		}
		b.WriteString("\n")
	}
	if len(d.Upcoming) > 0 {
		b.WriteString(fmt.Sprintf("DUE IN THE NEXT 14 DAYS (%d)\n", len(d.Upcoming)))
		for _, it := range d.Upcoming {
			line(it)
		}
		b.WriteString("\n")
	}
	if len(d.Overdue)+len(d.Upcoming) == 0 {
		b.WriteString("Nothing is due in the next 14 days and nothing is overdue.\n\n")
	}
	if len(d.Warnings) > 0 {
		b.WriteString("BALANCE WARNINGS\n")
		for _, w := range d.Warnings {
			b.WriteString("• " + w + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Keep hosting, domains and subscriptions paid so services stay online.")
	facts := []invoicing.EmailRow{{Label: "Overdue", Value: fmt.Sprint(len(d.Overdue))}, {Label: "Due in 14 days", Value: fmt.Sprint(len(d.Upcoming))}}
	if len(d.Warnings) > 0 {
		facts = append(facts, invoicing.EmailRow{Label: "Balance warnings", Value: fmt.Sprint(len(d.Warnings))})
	}
	tone, status := "ok", "All on track"
	if len(d.Overdue) > 0 {
		tone, status = "danger", fmt.Sprintf("%d overdue", len(d.Overdue))
	} else if len(d.Warnings) > 0 {
		tone, status = "warn", "Balance warning"
	} else if len(d.Upcoming) > 0 {
		tone, status = "info", fmt.Sprintf("%d due soon", len(d.Upcoming))
	}
	actions := []invoicing.EmailAction{}
	if u := s.appLink("/bills?view=upcoming"); u != "" {
		actions = append(actions, invoicing.EmailAction{Label: "Open upcoming bills", URL: u, Primary: true})
	}
	v := invoicing.EmailView{Kind: "reminder", Business: invoicing.EmailBusiness{Name: info.Name, Address: info.Address, Email: info.Email, Phone: info.Phone},
		LogoURL: info.Logo, Accent: info.Accent, Preheader: d.Subject, DocLabel: "Weekly bills digest", Number: s.Today(org).Format("2 Jan 2006"),
		Message: b.String(), AmountLabel: "Due in the next 14 days (incl. overdue)", Amount: fmtMoney(d.Total, d.Currency), StatusLine: status, StatusTone: tone,
		Facts: facts, Actions: actions, Note: "Sent weekly to the administrators of " + info.Name + ". Change it in Settings → Bill reminders."}
	htmlOut, text := invoicing.RenderEmail(v)
	return mail.Message{To: to, Subject: d.Subject, HTML: htmlOut, Text: text}
}

// Log lists recent reminder sends.
func (s *Service) Log(org string, limit int) []ReminderLog {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []ReminderLog{}
	s.DB.Where("organization_id = ?", org).Order("created_at DESC").Limit(limit).Find(&out)
	return out
}

// ---------------------------------------------------------------- scheduler

// RunAll runs every organisation with schedules or reminder settings. One
// instance runs an organisation at a time (Postgres advisory lock).
func (s *Service) RunAll(ctx context.Context) int {
	var orgs []string
	s.DB.Raw(`SELECT DISTINCT organization_id FROM finance_recurring_bills
		UNION SELECT organization_id FROM finance_bill_reminder_settings WHERE enabled AND include_one_off`).Scan(&orgs)
	n := 0
	for _, org := range orgs {
		if ctx.Err() != nil {
			return n
		}
		ok := s.withLock(ctx, "recurbills:"+org, func() {
			r := s.RunOrg(ctx, org, RunOptions{})
			if r.Generated.Created+r.Reminders+r.Failed > 0 || r.Digest != "" {
				log.Printf("recurbills: %s — %d bills created, %d matched, %d reminders, %d failed, digest %q", org, r.Generated.Created, r.Generated.Matched, r.Reminders, r.Failed, r.Digest)
			}
			n++
		})
		if !ok {
			log.Printf("recurbills: %s is running on another instance", org)
		}
	}
	return n
}

// RunOrgLocked runs one organisation under its advisory lock (manual run).
func (s *Service) RunOrgLocked(ctx context.Context, org string, opt RunOptions) (RunResult, bool) {
	var r RunResult
	ok := s.withLock(ctx, "recurbills:"+org, func() { r = s.RunOrg(ctx, org, opt) })
	return r, ok
}

func (s *Service) withLock(ctx context.Context, key string, fn func()) bool {
	got := false
	err := s.DB.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", key).Scan(&got).Error; err != nil {
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
		log.Printf("recurbills: lock %s: %v", key, err)
		return false
	}
	return got
}

// StartScheduler runs every 10 minutes (RECURRING_BILLS_SCHEDULER=off disables).
func (s *Service) StartScheduler(ctx context.Context, every time.Duration, firstDelay time.Duration) {
	if every <= 0 {
		every = 10 * time.Minute
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("recurbills: scheduler stopped: %v", r)
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
