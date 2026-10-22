package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// AlertsCategory is the pkg/emailprefs category of the admin alerts and the
// daily digest (owners and administrators; Settings → Who gets emails).
const AlertsCategory = "client_billing"

func money(v float64, cur string) string { return invoicing.FormatMoney(v, cur) }

func longDate(t time.Time) string  { return t.Format("Mon 2 Jan 2006") }
func shortDate(t time.Time) string { return t.Format("Mon 2 Jan") }

// periodText is "1 Nov – 30 Nov 2026" (the end is exclusive).
func periodText(sp Span) string {
	last := sp.End.AddDate(0, 0, -1)
	if sp.Start.Year() == last.Year() {
		return sp.Start.Format("2 Jan") + " – " + last.Format("2 Jan 2006")
	}
	return sp.Start.Format("2 Jan 2006") + " – " + last.Format("2 Jan 2006")
}

// what is the subscription's service in a word: hosting, domain, email …
func what(sub *Subscription) string {
	switch {
	case sub.HasKind(KindHosting):
		return "hosting"
	case sub.HasKind(KindDomain):
		return "domain"
	case sub.HasKind(KindEmail):
		return "email"
	case sub.HasKind(KindMaintenance):
		return "website maintenance"
	}
	if sub.SiteDomain != "" {
		return "hosting"
	}
	return "services"
}

// serviceName is "hosting for wanjiku.co.ke" / the subscription's name.
func serviceName(sub *Subscription) string {
	if d := firstNonEmpty(sub.SiteDomain, sub.Domain); d != "" {
		w := what(sub)
		if w == "domain" {
			return "domain " + d
		}
		return w + " for " + d
	}
	return sub.Name
}

func possessive(name string) string {
	if strings.HasSuffix(strings.ToLower(name), "s") {
		return name + "’"
	}
	return name + "’s"
}

// subLink is the in-app path of a subscription (its drawer), with an action
// to confirm (extend, paid, suspend, skip) and its parameters.
func subLink(id, action string, kv ...string) string {
	p := "/subscriptions/" + id
	if action == "" {
		return p
	}
	q := url.Values{"action": {action}}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return p + "?" + q.Encode()
}

// ---------------------------------------------------------------- organisation

type orgInfo struct {
	Name, Email, Phone, Address, Accent, Logo string
}

func (s *Service) orgInfo(org string) orgInfo {
	var o struct {
		Name, Email, Phone, AddressStreet, AddressSuburb, AddressState, AddressPostcode string
	}
	s.DB.Raw(`SELECT name, COALESCE(email,'') AS email, COALESCE(phone,'') AS phone, COALESCE(address_street,'') AS address_street, COALESCE(address_suburb,'') AS address_suburb,
		COALESCE(address_state,'') AS address_state, COALESCE(address_postcode,'') AS address_postcode FROM organizations WHERE id = ?`, org).Scan(&o)
	addr := strings.Join(nonEmpty(o.AddressStreet, strings.TrimSpace(o.AddressSuburb+" "+o.AddressState+" "+o.AddressPostcode)), ", ")
	info := orgInfo{Name: firstNonEmpty(o.Name, "DASYIN ERP"), Email: o.Email, Phone: o.Phone, Address: addr}
	if hasTable(s.DB, "billing_settings") {
		var bs []struct{ AccentColor, BusinessName string }
		s.DB.Raw(`SELECT COALESCE(accent_color,'') AS accent_color, COALESCE(business_name,'') AS business_name FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&bs)
		if len(bs) > 0 {
			info.Accent = bs[0].AccentColor
			// The name on invoices and their emails.
			info.Name = firstNonEmpty(strings.TrimSpace(bs[0].BusinessName), info.Name)
		}
	}
	if s.LogoURL != nil {
		info.Logo = s.LogoURL(org)
	}
	return info
}

func (s *Service) appLink(path string) string {
	base := strings.TrimRight(strings.TrimSpace(s.AppURL), "/")
	if base == "" {
		return ""
	}
	return base + path
}

// AdminRecipients are the owners and administrators who get client billing
// alerts (Settings → Who gets emails decides: pkg/emailprefs, category
// client_billing) plus the settings' extra addresses.
func (s *Service) AdminRecipients(org string, st Settings) []string {
	var ids []string
	s.DB.Raw(`SELECT id FROM users WHERE organization_id = ? AND deleted_at IS NULL AND (is_active IS NULL OR is_active) AND role IN ? ORDER BY created_at`,
		org, adminRoles).Scan(&ids)
	raw := emailprefs.UserEmails(s.DB, org, AlertsCategory, ids)
	raw = append(raw, emailprefs.FilterAddresses(s.DB, org, AlertsCategory, st.Recipients)...)
	v, _ := mail.ParseList(strings.Join(raw, ","))
	return v
}

// ---------------------------------------------------------------- events

// claim reserves an event key; ok false when it already happened (or is
// happening). A failed send is retried after 30 minutes, 3 attempts at most.
func (s *Service) claim(org string, e Event) (*Event, bool) {
	now := s.now()
	e.ID, e.OrganizationID, e.Status, e.Attempts, e.CreatedAt, e.UpdatedAt = uuid.NewString(), org, "sending", 1, now, now
	e.Key = clip(e.Key, 250)
	tx := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&e)
	if tx.Error == nil && tx.RowsAffected == 1 {
		return &e, true
	}
	var ex Event
	if !found(s.DB.Where("organization_id = ? AND key = ?", org, e.Key), &ex) {
		return nil, false
	}
	if ex.Status != "failed" || ex.Attempts >= 3 || now.Sub(ex.UpdatedAt) < 30*time.Minute {
		return nil, false
	}
	res := s.DB.Model(&Event{}).Where("id = ? AND status = ? AND attempts = ?", ex.ID, "failed", ex.Attempts).
		Updates(map[string]any{"status": "sending", "attempts": ex.Attempts + 1, "updated_at": now})
	if res.RowsAffected != 1 {
		return nil, false
	}
	ex.Attempts++
	return &ex, true
}

func (s *Service) finish(e *Event, status string, err error) {
	upd := map[string]any{"status": status, "error": "", "updated_at": s.now(), "recipients": clip(e.Recipients, 4000), "subject": clip(e.Subject, 480), "in_app": e.InApp}
	if err != nil {
		upd["status"], upd["error"] = "failed", clip(err.Error(), 1000)
	}
	s.DB.Model(&Event{}).Where("id = ?", e.ID).Updates(upd)
}

// record stores an event that needs no claim (state changes, admin actions).
func (s *Service) record(org string, e Event) {
	now := s.now()
	e.ID, e.OrganizationID, e.CreatedAt, e.UpdatedAt = uuid.NewString(), org, now, now
	e.Key = clip(e.Key, 250)
	e.Subject = clip(e.Subject, 480)
	if e.Status == "" {
		e.Status = "done"
	}
	s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&e)
}

// ---------------------------------------------------------------- admin alerts

// Alert is one message to the administrators.
type Alert struct {
	Type     string // notify type suffix, e.g. "suspension"
	Severity string // info | warning | error | success
	Subject  string
	Lead     string // first paragraph
	Body     string // more paragraphs
	Facts    []invoicing.EmailRow
	Link     string // in-app path
	LinkText string
	// Quick are one-click actions (in-app paths that open a confirmation).
	Quick  []invoicing.EmailAction
	Tone   string // ok | warn | danger | info
	Status string
	Entity string // subscription id
	Amount string
	Label  string
}

// alertAdmins emails the administrators and adds an in-app notification.
func (s *Service) alertAdmins(ctx context.Context, org string, st Settings, a Alert) ([]string, int, error) {
	inApp := 0
	if st.InApp {
		inApp, _ = notify.Send(s.DB, org, notify.Admins(), notify.Message{Type: "subscriptions." + a.Type, Severity: a.Severity, Title: a.Subject,
			Body: clip(firstNonEmpty(a.Lead, a.Body), 1900), Link: a.Link, EntityType: "subscription", EntityID: a.Entity})
	}
	to := s.AdminRecipients(org, st)
	if len(to) == 0 {
		return nil, inApp, errors.New("no administrator gets client billing emails — check Settings → Who gets emails")
	}
	info := s.orgInfo(org)
	msg := a.Lead
	if a.Body != "" {
		msg += "\n\n" + a.Body
	}
	actions := []invoicing.EmailAction{}
	if u := s.appLink(a.Link); u != "" && a.Link != "" {
		actions = append(actions, invoicing.EmailAction{Label: firstNonEmpty(a.LinkText, "Open the subscription"), URL: u, Primary: true})
	}
	groups := []invoicing.EmailActionGroup{}
	if len(a.Quick) > 0 && s.AppURL != "" {
		g := invoicing.EmailActionGroup{Label: "One click (you confirm in DASYIN)"}
		for _, q := range a.Quick {
			g.Actions = append(g.Actions, invoicing.EmailAction{Label: q.Label, URL: s.appLink(q.URL)})
		}
		groups = append(groups, g)
	}
	label := firstNonEmpty(a.Label, "Client billing")
	v := invoicing.EmailView{Kind: "reminder", Business: invoicing.EmailBusiness{Name: info.Name, Address: info.Address, Email: info.Email, Phone: info.Phone},
		LogoURL: info.Logo, Accent: info.Accent, Preheader: a.Subject, DocLabel: label, Message: msg, AmountLabel: "Unpaid", Amount: a.Amount,
		StatusLine: a.Status, StatusTone: firstNonEmpty(a.Tone, "info"), Facts: a.Facts, Actions: actions, Groups: groups,
		Note: "You're receiving this as an administrator of " + info.Name + ". Choose who gets client billing emails in Settings → Who gets emails."}
	if a.Amount == "" {
		v.AmountLabel = ""
	}
	h, t := invoicing.RenderEmail(v)
	_, err := s.Mail.SendAs(ctx, org, mail.Message{To: to, Subject: a.Subject, HTML: h, Text: t})
	return to, inApp, err
}

// quickActions are the one-click actions on an unpaid invoice.
func quickActions(sub *Subscription, suspended bool) []invoicing.EmailAction {
	q := []invoicing.EmailAction{
		{Label: "Give 7 more days", URL: subLink(sub.ID, "extend", "days", "7")},
		{Label: "Give 14 more days", URL: subLink(sub.ID, "extend", "days", "14")},
		{Label: "Mark as paid", URL: subLink(sub.ID, "paid")},
	}
	if !suspended {
		q = append(q, invoicing.EmailAction{Label: "Suspend now", URL: subLink(sub.ID, "suspend")},
			invoicing.EmailAction{Label: "Don't suspend this time", URL: subLink(sub.ID, "skip")})
	}
	return q
}

// suspensionAlert is "<client> will be suspended in N days / tomorrow".
func (s *Service) suspensionAlert(org string, sub *Subscription, per *BillingPeriod, d Dates, today time.Time) Alert {
	cu, _ := s.customer(org, sub.CustomerID)
	name := firstNonEmpty(cu.Name, "The client")
	left := DaysBetween(today, *d.Suspend)
	when := fmt.Sprintf("in %d days", left)
	switch left {
	case 1:
		when = "tomorrow"
	case 0:
		when = "today"
	}
	a := Alert{Type: "suspension", Severity: notify.Warning, Link: subLink(sub.ID, ""), LinkText: "Open the subscription", Entity: sub.ID,
		Facts: s.callFacts(org, sub, per, d), Amount: money(per.Balance, per.Currency), Tone: "warn", Label: "Suspension warning", Quick: quickActions(sub, false)}
	if left <= 1 {
		a.Severity, a.Tone = notify.Error, "danger"
	}
	a.Subject = fmt.Sprintf("%s will be suspended %s (%s) — %s", name, when, shortDate(*d.Suspend), serviceName(sub))
	a.Status = fmt.Sprintf("Suspended %s unless paid", when)
	od := DaysBetween(d.Due, today)
	due := fmt.Sprintf("was due on %s (%d day%s ago)", longDate(d.Due), od, plural(od))
	if od <= 0 {
		due = fmt.Sprintf("is due on %s", longDate(d.Due))
	}
	a.Lead = fmt.Sprintf("Invoice %s for %s %s and is unpaid. Unless it is paid, %s will be suspended on %s.", per.InvoiceNumber, money(per.Balance, per.Currency), due,
		serviceName(sub), longDate(*d.Suspend))
	how := "It is suspended at the provider by hand: you'll get a task with the steps."
	if s.automatable(org, sub) {
		how = "It is suspended at Synergy Wholesale automatically (through the API)."
	}
	a.Body = "Call the client now"
	if cu.Phone != "" {
		a.Body += " on " + cu.Phone
	}
	a.Body += ". " + how + " If they need more time, give it in one click; if they paid another way, mark the invoice paid."
	if d.Deferred {
		a.Body += fmt.Sprintf(" (The suspension was moved from %s so you hear about it at least %d days ahead.)", longDate(*d.Planned), MinNoticeDays)
	}
	return a
}

// ---------------------------------------------------------------- client emails

// clientReminder emails the client through the invoice's own reminder email
// (branded, PDF attached, the pay link), with the subscription's wording.
func (s *Service) clientReminder(ctx context.Context, who Actor, sub *Subscription, p *BillingPeriod, subject, message string) error {
	if s.Inv == nil {
		return errors.New("invoicing is not available")
	}
	return s.Inv.Send(ctx, who, p.InvoiceID, map[string]any{"kind": "reminder", "subject": subject, "message": message, "app_url": s.AppURL})
}

// clientDirect emails the client a notice that is not about an unpaid
// invoice (service active again).
func (s *Service) clientDirect(ctx context.Context, org string, sub *Subscription, subject, message, status, tone string, facts []invoicing.EmailRow) (string, error) {
	cu, _ := s.customer(org, sub.CustomerID)
	if !mail.ValidAddress(cu.Email) {
		return "", errors.New("the customer has no email address")
	}
	info := s.orgInfo(org)
	actions := []invoicing.EmailAction{}
	if u := s.PortalURL(org, sub.CustomerID); u != "" {
		actions = append(actions, invoicing.EmailAction{Label: "Your services", URL: u, Primary: true})
	}
	v := invoicing.EmailView{Kind: "reminder", Business: invoicing.EmailBusiness{Name: info.Name, Address: info.Address, Email: info.Email, Phone: info.Phone},
		LogoURL: info.Logo, Accent: info.Accent, Preheader: subject, DocLabel: "Your services", Number: sub.Number, Message: message,
		StatusLine: status, StatusTone: tone, Facts: facts, Actions: actions}
	h, t := invoicing.RenderEmail(v)
	_, err := s.Mail.SendAs(ctx, org, mail.Message{To: []string{cu.Email}, Subject: subject, HTML: h, Text: t})
	return cu.Email, err
}

func (s *Service) portalLine(org string, sub *Subscription) string {
	if u := s.PortalURL(org, sub.CustomerID); u != "" {
		return "\n\nSee your services and what's due: " + u
	}
	return ""
}

// invoiceMessage is the first email of a period (the invoice email, with
// the {{…}} placeholders of the invoicing module).
func (s *Service) invoiceMessage(org string, sub *Subscription, sp Span) (string, string) {
	subject := "Invoice {{number}} from {{business}} — " + serviceName(sub) + " (" + periodText(sp) + ")"
	msg := "Hi {{client}},\n\nHere is invoice {{number}} for {{total}} for your " + serviceName(sub) + ", " + periodText(sp) + ". It is due on {{due_date}}." +
		"\n\nView and pay online: {{pay_link}}" + s.portalLine(org, sub) + "\n\nThank you for your business,\n{{business}}"
	return subject, msg
}

// reminderMessage is a client reminder before, on or after the due date.
func (s *Service) reminderMessage(org string, sub *Subscription, d Dates, today time.Time) (string, string) {
	svc := serviceName(sub)
	days := DaysBetween(today, d.Due)
	var subject, lead string
	switch {
	case days > 1:
		subject = fmt.Sprintf("Reminder: invoice {{number}} for your %s is due on %s", svc, shortDate(d.Due))
		lead = fmt.Sprintf("This is a friendly reminder that invoice {{number}} for {{amount_due}} for your %s is due on %s — in %d days.", svc, longDate(d.Due), days)
	case days == 1:
		subject = fmt.Sprintf("Due tomorrow: invoice {{number}} for your %s", svc)
		lead = fmt.Sprintf("Invoice {{number}} for {{amount_due}} for your %s is due tomorrow, %s.", svc, longDate(d.Due))
	case days == 0:
		subject = fmt.Sprintf("Due today: invoice {{number}} for your %s", svc)
		lead = fmt.Sprintf("Invoice {{number}} for {{amount_due}} for your %s is due today.", svc)
	default:
		subject = fmt.Sprintf("Overdue: invoice {{number}} for your %s", svc)
		lead = fmt.Sprintf("Invoice {{number}} for {{amount_due}} was due on %s (%d day%s ago) and is still unpaid.", longDate(d.Due), -days, plural(days))
		if d.Suspend != nil {
			subject += " — please pay by " + shortDate(d.Suspend.AddDate(0, 0, -1))
			lead += fmt.Sprintf(" To keep your %s running, please pay it before %s.", svc, longDate(*d.Suspend))
		}
	}
	msg := "Hi {{client}},\n\n" + lead + "\n\nPay online: {{pay_link}}" + s.portalLine(org, sub) +
		"\n\nIf you've already paid, please ignore this message — and thank you.\n\n{{business}}"
	return subject, msg
}

// finalNoticeMessage is "your service will be suspended on <date>".
func (s *Service) finalNoticeMessage(org string, sub *Subscription, d Dates) (string, string) {
	svc := serviceName(sub)
	subject := fmt.Sprintf("Final notice: your %s will be suspended on %s", svc, shortDate(*d.Suspend))
	msg := fmt.Sprintf("Hi {{client}},\n\nInvoice {{number}} for {{amount_due}} was due on %s and is still unpaid.", longDate(d.Due)) +
		fmt.Sprintf("\n\nThis is the final notice: unless it is paid, your %s will be suspended on %s — the website and email on it will stop working until it is paid.", svc, longDate(*d.Suspend)) +
		"\n\nPay online now: {{pay_link}}" + s.portalLine(org, sub) + "\n\nIf you need more time, reply to this email or call us today.\n\n{{business}}"
	return subject, msg
}

func (s *Service) suspendedMessage(org string, sub *Subscription, due time.Time) (string, string) {
	svc := serviceName(sub)
	subject := fmt.Sprintf("Suspended: your %s — invoice {{number}} is unpaid", svc)
	msg := fmt.Sprintf("Hi {{client}},\n\nInvoice {{number}} for {{amount_due}} was due on %s and is still unpaid, so your %s has been suspended today.", longDate(due), svc) +
		"\n\nPay the invoice to restore it — the service comes back as soon as the payment arrives." +
		"\n\nPay online: {{pay_link}}" + s.portalLine(org, sub) + "\n\nQuestions? Just reply to this email.\n\n{{business}}"
	return subject, msg
}

// ---------------------------------------------------------------- admin wording

func (s *Service) callFacts(org string, sub *Subscription, p *BillingPeriod, d Dates) []invoicing.EmailRow {
	cu, _ := s.customer(org, sub.CustomerID)
	f := []invoicing.EmailRow{{Label: "Client", Value: firstNonEmpty(cu.Name, "—"), Strong: true}}
	if cu.Contact != "" && cu.Contact != cu.Name {
		f = append(f, invoicing.EmailRow{Label: "Contact", Value: cu.Contact})
	}
	if cu.Phone != "" {
		f = append(f, invoicing.EmailRow{Label: "Phone", Value: cu.Phone})
	}
	if cu.Email != "" {
		f = append(f, invoicing.EmailRow{Label: "Email", Value: cu.Email})
	}
	f = append(f, invoicing.EmailRow{Label: "Service", Value: sub.Name})
	if p != nil {
		f = append(f, invoicing.EmailRow{Label: "Invoice", Value: firstNonEmpty(p.InvoiceNumber, "—") + " · " + money(p.Balance, p.Currency) + " due " + d.Due.Format("2 Jan 2006")})
	}
	if d.Suspend != nil {
		f = append(f, invoicing.EmailRow{Label: "Suspension", Value: longDate(*d.Suspend), Strong: true})
	}
	if sup := SupplierLabel(sub.Supplier); sup != "" {
		f = append(f, invoicing.EmailRow{Label: "Hosted at", Value: strings.TrimSpace(sup + " " + sub.SupplierPlan)})
	}
	return f
}

// sentSubject is a client email's subject as the client saw it (the invoicing
// email fills {{number}} and {{business}}), for the timeline.
func (s *Service) sentSubject(org, subject, number string) string {
	return strings.NewReplacer("{{number}}", number, "{{business}}", s.orgInfo(org).Name).Replace(subject)
}
