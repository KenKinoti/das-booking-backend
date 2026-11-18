package schedsend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Service schedules and sends.
type Service struct {
	DB     *gorm.DB
	AppURL string // PUBLIC_APP_URL: links in messages (pay, unsubscribe pages are on the API)
	APIURL string // PUBLIC_API_URL: unsubscribe and open-tracking links
	Secret []byte // signs unsubscribe / open links

	// Invoices sends invoice, quote, receipt and reminder emails through the
	// invoicing API (in-process, as the creator).
	Invoices InvoiceSender
	// Mail sends an email from the organisation's account; nil = pkg/mail.
	Mail func(ctx context.Context, org string, msg mail.Message) (string, error)
	// WA is the WhatsApp gateway; nil = pkg/wa.
	WA WhatsApp
	// SMS sends a text; nil = the organisation's provider (sms.go).
	SMS func(ctx context.Context, org, to, text, ref string) (SMSResult, error)
	// Can reports whether a user's role may act on a module (the business
	// type roles); nil = allowed.
	Can func(org, user, role, module, act string) bool
	// LogoURL is the organisation's logo for emails.
	LogoURL func(org string) string
	// Gap is the pause between bulk sends (rate limit; default 150 ms).
	Gap time.Duration

	runMu sync.Mutex
}

// New builds a service.
func New(db *gorm.DB) *Service { return &Service{DB: db} }

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }

// ---------------------------------------------------------------- org facts

func (s *Service) orgTZ(org string) string {
	if tz := strings.TrimSpace(orgprefs.Get(fresh(s.DB), org).Timezone); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	return DefaultTimezone
}

func (s *Service) orgCountry(org string) string { return orgprefs.Country(fresh(s.DB), org) }

func (s *Service) homeCurrency(org string) string {
	if c := orgprefs.Currency(fresh(s.DB), org); c != "" {
		return c
	}
	return "KES"
}

func (s *Service) orgName(org string) string {
	var name string
	if hasTable(s.DB, "billing_settings") {
		fresh(s.DB).Raw(`SELECT COALESCE(business_name,'') FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&name)
	}
	if strings.TrimSpace(name) == "" {
		fresh(s.DB).Raw(`SELECT COALESCE(name,'') FROM organizations WHERE id = ?`, org).Scan(&name)
	}
	return firstNonEmpty(name, "Your business")
}

// ---------------------------------------------------------------- actors

// Actor is who is acting.
type Actor struct {
	Org, User, Role, Name, Email string
}

// Level is the actor's level: owner | manager | staff.
func (a Actor) Level() string {
	if a.Role == "super_admin" {
		return emailprefs.LevelOwner
	}
	l, _ := emailprefs.LevelOf(a.Org, a.User, a.Role)
	return l
}

func (a Actor) managerUp() bool {
	l := a.Level()
	return l == emailprefs.LevelOwner || l == emailprefs.LevelManager
}

func (a Actor) readOnly() bool {
	if a.Role == "read_only" {
		return true
	}
	if emailprefs.RoleOf != nil && a.Role != "super_admin" {
		if base, _, ok := emailprefs.RoleOf(a.Org, a.User, a.Role); ok && base == "read_only" {
			return true
		}
	}
	return false
}

// Error is a request error with an HTTP status.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func bad(msg string, a ...any) error {
	return &Error{http.StatusBadRequest, "VALIDATION_ERROR", fmt.Sprintf(msg, a...)}
}

func forbidden(msg string) error { return &Error{http.StatusForbidden, "FORBIDDEN", msg} }

// canInvoices: the person may send invoices (POST …/send is "create").
func (s *Service) canInvoices(a Actor) bool {
	if a.Role == "super_admin" {
		return true
	}
	if a.readOnly() {
		return false
	}
	return s.Can == nil || s.Can(a.Org, a.User, a.Role, "invoicing", "create")
}

// Permitted checks whether a may create / change m.
func (s *Service) Permitted(a Actor, m *Message) error {
	switch m.Kind {
	case KindInvoice, KindReceipt, KindReminder:
		if !s.canInvoices(a) {
			return forbidden("You don't have permission to send invoices.")
		}
	case KindStatement:
		if !s.canInvoices(a) {
			return forbidden("You don't have permission to send invoices.")
		}
		if (m.Audience.Mode != AudOne || len(m.Audience.CustomerIDs) > 1) && !a.managerUp() {
			return forbidden("Statements to several customers can be scheduled by managers and above.")
		}
	case KindCustomer:
		if a.readOnly() {
			return forbidden("Your role can't send messages.")
		}
		if (m.Audience.Mode != AudOne || len(m.Audience.CustomerIDs) > 1) && !a.managerUp() {
			return forbidden("Messages to several customers can be scheduled by managers and above.")
		}
	case KindTeam:
		if !a.managerUp() {
			return forbidden("Team messages can be scheduled by managers and above.")
		}
	}
	return nil
}

// canManage: the creator, or a manager and above (with the kind's permission).
func (s *Service) canManage(a Actor, m *Message) error {
	if m.CreatedBy != a.User && !a.managerUp() {
		return forbidden("Only the person who scheduled this, or a manager, can change it.")
	}
	return s.Permitted(a, m)
}

// ---------------------------------------------------------------- input

// Input is a new or changed scheduled message.
type Input struct {
	Kind          string       `json:"kind"`
	Title         string       `json:"title"`
	Channels      []string     `json:"channels"`
	Marketing     bool         `json:"marketing"`
	InvoiceID     string       `json:"invoice_id"`
	InvoiceNumber string       `json:"invoice_number"`
	PaymentID     string       `json:"payment_id"`
	InvoiceSend   *InvoiceSend `json:"invoice_send"`
	Audience      Audience     `json:"audience"`
	Subject       string       `json:"subject"`
	Body          string       `json:"body"`
	SMSBody       string       `json:"sms_body"`
	WhatsApp      WhatsAppPlan `json:"whatsapp"`
	Timezone      string       `json:"timezone"`
	LocalSendAt   string       `json:"local_send_at"`
	Repeat        Recurrence   `json:"repeat"`
	Paused        bool         `json:"paused"`
}

func cleanChannels(in []string, allowed ...string) ([]string, error) {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	seen := map[string]bool{}
	out := []string{}
	for _, c := range in {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" || seen[c] {
			continue
		}
		if !ok[c] {
			return nil, bad("%s can't be used for this kind of message", channelLabel(c))
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, nil
}

func channelLabel(c string) string {
	switch c {
	case ChanEmail:
		return "Email"
	case ChanSMS:
		return "SMS"
	case ChanWhatsApp:
		return "WhatsApp"
	case ChanInApp:
		return "In-app notification"
	}
	return c
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Default wording.
const (
	defaultReminderText  = "Hi {first_name}, invoice {invoice_number} for {amount_due} was due on {due_date}. You can pay here: {pay_link} — {business}"
	defaultStatementSubj = "Statement of account — {month}"
	defaultStatementBody = "Hi {first_name},\n\nHere is your statement from {business}. You have {open_invoices} unpaid invoice(s) with {balance} still to pay:\n\n{statement}\n\nPay online: {pay_link}\n\nIf you've already paid, thank you — please ignore this message.\n\n{business}"
	defaultStatementSMS  = "Hi {first_name}, your balance with {business} is {balance} ({open_invoices} unpaid invoice(s)). Pay: {pay_link}"
)

// Apply validates in and fills m (a new message, or an existing one being
// edited). now is the package clock.
func (s *Service) Apply(a Actor, m *Message, in Input, now time.Time) error {
	org := a.Org
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if m.Kind != "" && kind != "" && kind != m.Kind {
		return bad("the kind of a scheduled message can't change — duplicate it instead")
	}
	if kind == "" {
		kind = m.Kind
	}
	m.Kind = kind
	m.Title = strings.TrimSpace(in.Title)
	m.Marketing = in.Marketing
	m.Subject = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(in.Subject))
	m.Body = strings.TrimSpace(in.Body)
	m.SMSBody = strings.TrimSpace(in.SMSBody)
	m.WhatsApp = in.WhatsApp
	m.WhatsApp.Template = strings.TrimSpace(m.WhatsApp.Template)
	m.Repeat = in.Repeat
	if err := m.Repeat.Normalize(); err != nil {
		return bad("%s", err.Error())
	}
	if len(m.Subject) > 250 {
		return bad("the subject is too long")
	}
	if len(m.Body) > 20000 || len(m.SMSBody) > 1600 {
		return bad("the message is too long")
	}
	var err error
	switch kind {
	case KindInvoice, KindReceipt:
		if m.Channels, err = cleanChannels([]string{ChanEmail}, ChanEmail); err != nil {
			return err
		}
		if m.Repeat.Repeats() {
			return bad("invoice, quote and receipt emails are sent once — use payment reminders to repeat")
		}
		if err := s.applyInvoice(org, m, in); err != nil {
			return err
		}
		m.Marketing = false
	case KindReminder:
		if m.Channels, err = cleanChannels(in.Channels, ChanEmail, ChanSMS, ChanWhatsApp); err != nil {
			return err
		}
		if len(m.Channels) == 0 {
			m.Channels = []string{ChanEmail}
		}
		if err := s.applyInvoice(org, m, in); err != nil {
			return err
		}
		if m.DocType != "invoice" {
			return bad("payment reminders are for invoices")
		}
		inv := s.Invoice(org, m.InvoiceID)
		if !(inv.Status == "sent" || inv.Status == "partial") || inv.BalanceDue <= 0 {
			return bad("reminders can only be scheduled for unpaid invoices that have been sent")
		}
		m.Repeat.StopWhenPaid = true
		m.Marketing = false
		if (has(m.Channels, ChanSMS) || has(m.Channels, ChanWhatsApp)) && m.SMSBody == "" {
			m.SMSBody = defaultReminderText
		}
		if inv.CustomerID != "" {
			m.Audience = Audience{Mode: AudOne, CustomerIDs: []string{inv.CustomerID}}
		} else {
			m.Audience = Audience{}
		}
		if (has(m.Channels, ChanSMS) || has(m.Channels, ChanWhatsApp)) && inv.CustomerID == "" {
			return bad("this invoice isn't linked to a customer, so there is no phone number for SMS or WhatsApp")
		}
	case KindCustomer, KindStatement:
		if m.Channels, err = cleanChannels(in.Channels, ChanEmail, ChanSMS, ChanWhatsApp); err != nil {
			return err
		}
		if len(m.Channels) == 0 {
			return bad("choose at least one channel: email, SMS or WhatsApp")
		}
		m.Audience = in.Audience
		m.Audience.CustomerIDs = uniq(m.Audience.CustomerIDs)
		m.Audience.UserIDs, m.Audience.Levels = nil, nil
		switch m.Audience.Mode {
		case AudOne:
			if len(m.Audience.CustomerIDs) != 1 {
				return bad("choose the customer")
			}
		case AudChosen:
			if len(m.Audience.CustomerIDs) == 0 {
				return bad("choose at least one customer")
			}
			if len(m.Audience.CustomerIDs) > MaxRecipients {
				return bad("choose at most %d customers", MaxRecipients)
			}
		case AudGroup:
			if _, err := s.Customers(org, m.Audience); err != nil {
				return bad("%s", err.Error())
			}
		default:
			return bad("choose who to send to: one customer, chosen customers or a group")
		}
		if m.Audience.Mode != AudGroup {
			// Every chosen customer must belong to the organisation.
			var n int64
			if hasTable(s.DB, "customers") {
				fresh(s.DB).Raw(`SELECT COUNT(*) FROM customers WHERE organization_id = ? AND id IN ? AND deleted_at IS NULL`, org, m.Audience.CustomerIDs).Scan(&n)
			}
			if int(n) != len(m.Audience.CustomerIDs) {
				return bad("some of the chosen customers were not found")
			}
		}
		if kind == KindStatement {
			m.Marketing = false
			if m.Subject == "" {
				m.Subject = defaultStatementSubj
			}
			if m.Body == "" {
				m.Body = defaultStatementBody
			}
			if m.SMSBody == "" && (has(m.Channels, ChanSMS) || has(m.Channels, ChanWhatsApp)) {
				m.SMSBody = defaultStatementSMS
			}
		}
		if has(m.Channels, ChanEmail) && (m.Subject == "" || m.Body == "") {
			return bad("add a subject and a message for the email")
		}
		if has(m.Channels, ChanSMS) && firstNonEmpty(m.SMSBody, m.Body) == "" {
			return bad("add the text message")
		}
		if has(m.Channels, ChanWhatsApp) && firstNonEmpty(m.SMSBody, m.Body) == "" && m.WhatsApp.Template == "" {
			return bad("add a message or choose a WhatsApp template")
		}
	case KindTeam:
		chans, err := cleanChannels(in.Channels, ChanInApp, ChanEmail, ChanWhatsApp)
		if err != nil {
			return err
		}
		m.Channels = append([]string{ChanInApp}, without(chans, ChanInApp)...)
		m.Audience = Audience{Mode: AudStaff, UserIDs: uniq(in.Audience.UserIDs), Levels: in.Audience.Levels}
		if _, err := s.Staff(org, m.Audience); err != nil {
			return bad("%s", err.Error())
		}
		m.Marketing = false
		if m.Subject == "" || m.Body == "" {
			return bad("add a title and a message")
		}
	default:
		return bad("kind must be invoice, receipt, reminder, customer, statement or team")
	}
	if kind != KindInvoice && kind != KindReceipt {
		team := kind == KindTeam
		for _, t := range []string{m.Subject, m.Body, m.SMSBody, m.WhatsApp.URLButton} {
			if unk := UnknownFields(t, team); len(unk) > 0 {
				return bad("unknown merge field {%s} — use one from the list", unk[0])
			}
		}
		for _, p := range m.WhatsApp.Params {
			if unk := UnknownFields(p, team); len(unk) > 0 {
				return bad("unknown merge field {%s} in the WhatsApp template", unk[0])
			}
		}
	}

	// Schedule.
	tz := strings.TrimSpace(in.Timezone)
	if tz == "" {
		tz = firstNonEmpty(m.Timezone, s.orgTZ(org))
	}
	if _, err := LoadZone(tz); err != nil {
		return bad("%s", err.Error())
	}
	local := strings.TrimSpace(in.LocalSendAt)
	if len(local) > 16 {
		local = local[:16]
	}
	sch := Schedule{Start: local, TZ: tz, Repeat: m.Repeat}
	first, err := sch.First()
	if err != nil {
		return bad("%s", err.Error())
	}
	if first.Before(now.Add(-2 * time.Minute)) {
		return bad("choose a time in the future (it is %s now in %s)", FormatLocal(now, tz), tz)
	}
	if first.After(now.AddDate(3, 0, 0)) {
		return bad("choose a time within the next 3 years")
	}
	m.Timezone, m.LocalSendAt = tz, local
	m.NextRunAt = &first
	m.Occurrences = 0
	if m.Title == "" {
		m.Title = s.defaultTitle(m)
	}
	if len(m.Title) > 200 {
		m.Title = m.Title[:200]
	}
	return nil
}

func without(list []string, v string) []string {
	out := []string{}
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func (s *Service) applyInvoice(org string, m *Message, in Input) error {
	id := strings.TrimSpace(in.InvoiceID)
	if id == "" && strings.TrimSpace(in.InvoiceNumber) != "" && hasTable(s.DB, "billing_invoices") {
		fresh(s.DB).Raw(`SELECT id FROM billing_invoices WHERE organization_id = ? AND UPPER(number) = UPPER(?) AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1`,
			org, strings.TrimSpace(in.InvoiceNumber)).Scan(&id)
	}
	if id == "" {
		if strings.TrimSpace(in.InvoiceNumber) != "" {
			return &Error{http.StatusNotFound, "NOT_FOUND", "No invoice or quote numbered " + strings.TrimSpace(in.InvoiceNumber)}
		}
		return bad("choose the invoice")
	}
	inv := s.Invoice(org, id)
	if !inv.Found || inv.Deleted {
		return &Error{http.StatusNotFound, "NOT_FOUND", "Invoice not found"}
	}
	if inv.Status == "void" || inv.Status == "converted" {
		return bad("this %s is %s and can't be sent", inv.DocType, inv.Status)
	}
	m.InvoiceID, m.DocNumber, m.DocType, m.CustomerName = inv.ID, inv.Number, inv.DocType, inv.ClientName
	if m.Kind == KindReceipt {
		m.PaymentID = strings.TrimSpace(in.PaymentID)
		if m.PaymentID == "" || !s.paymentExists(org, inv.ID, m.PaymentID) {
			return bad("choose the payment the receipt is for")
		}
	} else {
		m.PaymentID = ""
	}
	if in.InvoiceSend != nil {
		snd := *in.InvoiceSend
		snd.To = strings.ToLower(strings.TrimSpace(snd.To))
		if snd.To != "" && !validEmail(snd.To) {
			return bad("“%s” is not a valid email address", snd.To)
		}
		for _, l := range [][]string{snd.CC, snd.BCC} {
			for _, e := range l {
				if !validEmail(strings.TrimSpace(e)) {
					return bad("“%s” is not a valid email address", e)
				}
			}
		}
		if len(snd.CC) > 50 || len(snd.BCC) > 20 {
			return bad("too many copies (at most 50 CC and 20 BCC)")
		}
		snd.Set = snd.To != "" || len(snd.CC) > 0 || len(snd.BCC) > 0 || snd.Set
		m.InvoiceSend = snd
	} else if m.InvoiceSend.Subject == "" && m.InvoiceSend.Message == "" && !m.InvoiceSend.Set {
		m.InvoiceSend = InvoiceSend{AcceptWarnings: true}
	}
	if m.Kind == KindReminder && in.InvoiceSend == nil && (m.Subject != "" || m.Body != "") {
		m.InvoiceSend.Subject, m.InvoiceSend.Message = m.Subject, m.Body
	}
	return nil
}

func (s *Service) defaultTitle(m *Message) string {
	doc := "Invoice"
	if m.DocType == "quote" {
		doc = "Quote"
	}
	switch m.Kind {
	case KindInvoice:
		return strings.TrimSpace(fmt.Sprintf("%s %s to %s", doc, m.DocNumber, m.CustomerName))
	case KindReceipt:
		return strings.TrimSpace(fmt.Sprintf("Receipt for %s to %s", m.DocNumber, m.CustomerName))
	case KindReminder:
		return strings.TrimSpace(fmt.Sprintf("Payment reminder for %s", m.DocNumber))
	case KindStatement:
		return "Statement of account"
	case KindTeam:
		return firstNonEmpty(m.Subject, "Team message")
	}
	return firstNonEmpty(m.Subject, firstLine(m.Body), firstLine(m.SMSBody), "Customer message")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

// ---------------------------------------------------------------- CRUD

// Create schedules a new message.
func (s *Service) Create(a Actor, in Input) (*Message, error) {
	m := &Message{ID: uuid.NewString(), OrganizationID: a.Org, Status: StatusScheduled, CreatedBy: a.User, CreatedByName: a.Name, UpdatedBy: a.User}
	now := Now()
	if err := s.Apply(a, m, in, now); err != nil {
		return nil, err
	}
	if err := s.Permitted(a, m); err != nil {
		return nil, err
	}
	if in.Paused {
		m.Status = StatusPaused
	}
	m.CreatedAt, m.UpdatedAt = now, now
	if err := fresh(s.DB).Create(m).Error; err != nil {
		return nil, err
	}
	return s.decorate(m), nil
}

// Get loads one message of org.
func (s *Service) Get(org, id string) (*Message, error) {
	var m Message
	if err := fresh(s.DB).Where("organization_id = ? AND id = ?", org, id).Take(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &Error{http.StatusNotFound, "NOT_FOUND", "Scheduled message not found"}
		}
		return nil, err
	}
	return s.decorate(&m), nil
}

func (s *Service) decorate(m *Message) *Message {
	if m.NextRunAt != nil && (m.Status == StatusScheduled || m.Status == StatusPaused || m.Status == StatusRunning) {
		m.NextLocal = FormatLocal(*m.NextRunAt, m.Timezone)
	}
	if m.Channels == nil {
		m.Channels = []string{}
	}
	return m
}

// Update changes a message that is not being sent right now.
func (s *Service) Update(a Actor, id string, in Input) (*Message, error) {
	m, err := s.Get(a.Org, id)
	if err != nil {
		return nil, err
	}
	if err := s.canManage(a, m); err != nil {
		return nil, err
	}
	switch m.Status {
	case StatusRunning:
		return nil, &Error{http.StatusConflict, "BUSY", "It is being sent right now — try again in a minute."}
	case StatusDone, StatusCancelled:
		return nil, &Error{http.StatusConflict, "ENDED", "This message has finished — duplicate it to send it again."}
	}
	occ := m.Occurrences
	if err := s.Apply(a, m, in, Now()); err != nil {
		return nil, err
	}
	if err := s.Permitted(a, m); err != nil {
		return nil, err
	}
	// Occurrences already handled still count towards "N times".
	m.Occurrences = occ
	if m.Repeat.Count > 0 && occ >= m.Repeat.Count {
		return nil, bad("it has already been sent %d times — raise the number of times or duplicate it", occ)
	}
	if in.Paused {
		m.Status = StatusPaused
	} else if m.Status == StatusPaused && !in.Paused {
		m.Status = StatusScheduled
	}
	m.UpdatedBy, m.UpdatedAt = a.User, Now()
	res := fresh(s.DB).Model(&Message{}).Where("id = ? AND organization_id = ? AND status IN ?", m.ID, a.Org, []string{StatusScheduled, StatusPaused}).
		Select("title", "channels", "marketing", "invoice_id", "payment_id", "doc_number", "doc_type", "customer_name", "invoice_send", "audience",
			"subject", "body", "sms_body", "whatsapp", "timezone", "local_send_at", "repeat_rule", "next_run_at", "status", "updated_by", "updated_at").Updates(m)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &Error{http.StatusConflict, "BUSY", "It changed while you were editing — reload and try again."}
	}
	return s.Get(a.Org, m.ID)
}

// SetStatus pauses, resumes or cancels.
func (s *Service) SetStatus(a Actor, id, action string) (*Message, error) {
	m, err := s.Get(a.Org, id)
	if err != nil {
		return nil, err
	}
	if err := s.canManage(a, m); err != nil {
		return nil, err
	}
	now := Now()
	upd := map[string]any{"updated_by": a.User, "updated_at": now}
	var from []string
	switch action {
	case "pause":
		from = []string{StatusScheduled}
		upd["status"] = StatusPaused
	case "resume":
		from = []string{StatusPaused}
		upd["status"] = StatusScheduled
		// A paused message resumes at its next occurrence still in the future
		// (missed ones while paused are not sent in a burst).
		if m.NextRunAt != nil && m.NextRunAt.Before(now) {
			sch := Schedule{Start: m.LocalSendAt, TZ: m.Timezone, Repeat: m.Repeat}
			t, done := *m.NextRunAt, m.Occurrences
			for i := 0; i < 5000 && t.Before(now); i++ {
				nx, ok := sch.After(t, done)
				if !ok {
					if m.Repeat.Repeats() {
						return nil, bad("its schedule ended while it was paused")
					}
					// A one-off whose time passed: send it in a minute.
					nx = now.Add(time.Minute)
					t = nx
					break
				}
				t = nx
			}
			upd["next_run_at"] = t
		}
	case "cancel":
		from = []string{StatusScheduled, StatusPaused}
		upd["status"] = StatusCancelled
		upd["cancelled_at"] = now
		upd["ended_reason"] = "Cancelled by " + firstNonEmpty(a.Name, a.Email, "a user")
	default:
		return nil, bad("unknown action")
	}
	res := fresh(s.DB).Model(&Message{}).Where("id = ? AND organization_id = ? AND status IN ?", id, a.Org, from).Updates(upd)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &Error{http.StatusConflict, "CONFLICT", "It can't be " + map[string]string{"pause": "paused", "resume": "resumed", "cancel": "cancelled"}[action] + " now (it is " + m.Status + ")."}
	}
	return s.Get(a.Org, id)
}

// Duplicate copies a message as a new, paused one (next send: its original
// time when still ahead, else tomorrow at the same time).
func (s *Service) Duplicate(a Actor, id string) (*Message, error) {
	src, err := s.Get(a.Org, id)
	if err != nil {
		return nil, err
	}
	if err := s.Permitted(a, src); err != nil {
		return nil, err
	}
	now := Now()
	local := src.LocalSendAt
	if t, err := ParseLocal(local, src.Timezone); err != nil || !t.After(now) {
		loc, _ := LoadZone(src.Timezone)
		hm := "08:00"
		if len(local) == 16 {
			hm = local[11:]
		}
		local = now.In(loc).AddDate(0, 0, 1).Format(DateLayout) + "T" + hm
	}
	in := Input{Kind: src.Kind, Title: "Copy of " + src.Title, Channels: src.Channels, Marketing: src.Marketing, InvoiceID: src.InvoiceID, PaymentID: src.PaymentID,
		Audience: src.Audience, Subject: src.Subject, Body: src.Body, SMSBody: src.SMSBody, WhatsApp: src.WhatsApp, Timezone: src.Timezone, LocalSendAt: local,
		Repeat: src.Repeat, Paused: true}
	if src.Kind == KindInvoice || src.Kind == KindReceipt || src.Kind == KindReminder {
		snd := src.InvoiceSend
		in.InvoiceSend = &snd
	}
	if len(in.Title) > 200 {
		in.Title = in.Title[:200]
	}
	return s.Create(a, in)
}

// ---------------------------------------------------------------- list

// ListFilter selects messages.
type ListFilter struct {
	Tab       string // upcoming | recurring | sent | failed | all
	Kind      string
	Channel   string
	Search    string
	InvoiceID string
	Mine      bool
	Limit     int
}

// TabCounts are the numbers on the tabs.
type TabCounts struct {
	Upcoming  int64 `json:"upcoming"`
	Recurring int64 `json:"recurring"`
	Sent      int64 `json:"sent"`
	Failed    int64 `json:"failed"`
}

func tabWhere(q *gorm.DB, tab string) *gorm.DB {
	active := []string{StatusScheduled, StatusPaused, StatusRunning}
	switch tab {
	case "upcoming":
		return q.Where("status IN ? AND COALESCE(repeat_rule::jsonb->>'freq','none') IN ('none','')", active)
	case "recurring":
		return q.Where("status IN ? AND COALESCE(repeat_rule::jsonb->>'freq','none') NOT IN ('none','')", active)
	case "sent":
		return q.Where("status IN ?", []string{StatusDone, StatusCancelled})
	case "failed":
		return q.Where("last_result IN ?", []string{RunFailed, RunPartial, RunMissed})
	}
	return q
}

// List lists messages of a.Org (staff see their own).
func (s *Service) List(a Actor, f ListFilter) ([]Message, TabCounts, error) {
	base := func() *gorm.DB {
		q := fresh(s.DB).Model(&Message{}).Where("organization_id = ?", a.Org)
		if f.Mine || !a.managerUp() {
			q = q.Where("created_by = ?", a.User)
		}
		if f.Kind != "" {
			q = q.Where("kind = ?", f.Kind)
		}
		if f.Channel != "" {
			q = q.Where("channels LIKE ?", `%"`+f.Channel+`"%`)
		}
		if f.InvoiceID != "" {
			q = q.Where("invoice_id = ?", f.InvoiceID)
		}
		if t := strings.TrimSpace(f.Search); t != "" {
			like := "%" + strings.ToLower(t) + "%"
			q = q.Where("LOWER(title) LIKE ? OR LOWER(subject) LIKE ? OR LOWER(doc_number) LIKE ? OR LOWER(customer_name) LIKE ?", like, like, like, like)
		}
		return q
	}
	var c TabCounts
	tabWhere(base(), "upcoming").Count(&c.Upcoming)
	tabWhere(base(), "recurring").Count(&c.Recurring)
	tabWhere(base(), "sent").Count(&c.Sent)
	tabWhere(base(), "failed").Count(&c.Failed)
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	order := "next_run_at ASC NULLS LAST, created_at DESC"
	if f.Tab == "sent" || f.Tab == "failed" {
		order = "COALESCE(last_run_at, updated_at) DESC"
	}
	var out []Message
	if err := tabWhere(base(), f.Tab).Order(order).Limit(limit).Find(&out).Error; err != nil {
		return nil, c, err
	}
	for i := range out {
		s.decorate(&out[i])
	}
	if out == nil {
		out = []Message{}
	}
	return out, c, nil
}

// Upcoming is a badge for each invoice with something scheduled.
type Upcoming struct {
	InvoiceID string     `json:"invoice_id"`
	Count     int        `json:"count"`
	NextAt    *time.Time `json:"next_at"`
	NextLocal string     `json:"next_local"`
	Kinds     []string   `json:"kinds"`
	Paused    bool       `json:"paused"`
}

// Badges reports scheduled sends per invoice.
func (s *Service) Badges(org string, ids []string) map[string]*Upcoming {
	out := map[string]*Upcoming{}
	q := fresh(s.DB).Where("organization_id = ? AND invoice_id <> '' AND status IN ?", org, []string{StatusScheduled, StatusPaused, StatusRunning})
	if len(ids) > 0 {
		q = q.Where("invoice_id IN ?", ids)
	}
	var rows []Message
	q.Order("next_run_at").Limit(2000).Find(&rows)
	for _, m := range rows {
		b := out[m.InvoiceID]
		if b == nil {
			b = &Upcoming{InvoiceID: m.InvoiceID, Kinds: []string{}}
			out[m.InvoiceID] = b
		}
		b.Count++
		if !has(b.Kinds, m.Kind) {
			b.Kinds = append(b.Kinds, m.Kind)
		}
		if m.Status == StatusPaused {
			b.Paused = true
		}
		if m.NextRunAt != nil && m.Status != StatusPaused && (b.NextAt == nil || m.NextRunAt.Before(*b.NextAt)) {
			t := *m.NextRunAt
			b.NextAt = &t
			b.NextLocal = FormatLocal(t, m.Timezone)
		}
	}
	return out
}

// Detail is a message with its runs and the deliveries of one run.
type Detail struct {
	Message    *Message         `json:"message"`
	RepeatText string           `json:"repeat_text"`
	Upcoming   []string         `json:"upcoming"` // next send times (local)
	Runs       []Run            `json:"runs"`
	Run        *Run             `json:"run"`
	Deliveries []Delivery       `json:"deliveries"`
	Tracking   []InvoiceTracked `json:"tracking,omitempty"` // invoice emails: per-recipient opens / views
}

// InvoiceTracked is one person's copy of an invoice email (invoicing tracking).
type InvoiceTracked struct {
	LogID        string     `json:"log_id"`
	Email        string     `json:"email"`
	Role         string     `json:"role"`
	Status       string     `json:"status"`
	SentAt       *time.Time `json:"sent_at"`
	OpenedAt     *time.Time `json:"opened_at"`
	AutoOpened   bool       `json:"auto_opened"`
	ViewedAt     *time.Time `json:"viewed_at"`
	OpenTracking bool       `json:"open_tracking"`
}

// GetDetail loads a message with its history.
func (s *Service) GetDetail(a Actor, id, runID string) (*Detail, error) {
	m, err := s.Get(a.Org, id)
	if err != nil {
		return nil, err
	}
	if m.CreatedBy != a.User && !a.managerUp() {
		return nil, &Error{http.StatusNotFound, "NOT_FOUND", "Scheduled message not found"}
	}
	d := &Detail{Message: m, RepeatText: m.Repeat.Describe(m.LocalSendAt), Upcoming: []string{}, Runs: []Run{}, Deliveries: []Delivery{}}
	if m.NextRunAt != nil && (m.Status == StatusScheduled || m.Status == StatusPaused) {
		sch := Schedule{Start: m.LocalSendAt, TZ: m.Timezone, Repeat: m.Repeat}
		t, done := *m.NextRunAt, m.Occurrences
		d.Upcoming = append(d.Upcoming, FormatLocal(t, m.Timezone))
		for len(d.Upcoming) < 5 {
			nx, ok := sch.After(t, done+len(d.Upcoming)-1)
			if !ok {
				break
			}
			d.Upcoming = append(d.Upcoming, FormatLocal(nx, m.Timezone))
			t = nx
		}
	}
	fresh(s.DB).Where("message_id = ? AND organization_id = ?", m.ID, a.Org).Order("started_at DESC").Limit(50).Find(&d.Runs)
	for i := range d.Runs {
		if (runID == "" && i == 0) || d.Runs[i].ID == runID {
			r := d.Runs[i]
			d.Run = &r
		}
	}
	if d.Run != nil {
		fresh(s.DB).Where("run_id = ? AND organization_id = ?", d.Run.ID, a.Org).Order("created_at, name, channel").Find(&d.Deliveries)
		logIDs := []string{}
		for _, x := range d.Deliveries {
			if x.RecipientType == "invoice" && x.ProviderID != "" {
				logIDs = append(logIDs, x.ProviderID)
			}
		}
		d.Tracking = s.invoiceTracking(a.Org, logIDs)
	}
	return d, nil
}

func (s *Service) invoiceTracking(org string, logIDs []string) []InvoiceTracked {
	out := []InvoiceTracked{}
	if len(logIDs) == 0 || !hasTable(s.DB, "invoice_email_recipients") {
		return out
	}
	var rows []invoicing.EmailRecipient
	fresh(s.DB).Where("organization_id = ? AND email_log_id IN ?", org, logIDs).Order("position").Find(&rows)
	for _, r := range rows {
		t := InvoiceTracked{LogID: r.EmailLogID, Email: r.Email, Role: r.Role, Status: r.Status, SentAt: r.SentAt, OpenedAt: r.FirstHumanOpenAt,
			ViewedAt: r.FirstViewedAt, OpenTracking: r.OpenTracking}
		if t.OpenedAt == nil && r.FirstOpenedAt != nil {
			t.OpenedAt, t.AutoOpened = r.FirstOpenedAt, true
		}
		out = append(out, t)
	}
	return out
}
