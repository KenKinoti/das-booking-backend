package schedsend

import (
	"strings"
)

// target decides, for a customer and a channel, the address and — when it
// must not be sent — why. Used when an occurrence is expanded and by the
// composer's preview, so both say the same thing.
func (s *Service) target(m *Message, c Recipient, ch string, open map[string][]OpenInvoice) (addr, why string) {
	switch ch {
	case ChanEmail:
		addr = c.Email
		if !validEmail(addr) {
			why = "No email address"
			if addr != "" {
				why = "“" + addr + "” is not a valid email address"
			}
		}
	case ChanSMS, ChanWhatsApp:
		addr, why = s.phoneOf(m.OrganizationID, c)
	case ChanInApp:
		addr = "In the app"
	}
	switch {
	case c.Type == "customer" && c.Deleted:
		why = "The customer was deleted"
	case c.Type == "customer" && m.Marketing && c.OptedOut:
		why = "Opted out of marketing messages"
	case c.Type == "customer" && open != nil && len(open[c.ID]) == 0 && why == "":
		why = "Nothing outstanding"
	}
	return addr, why
}

// Sample is one recipient's message as it would be sent.
type Sample struct {
	Recipient Recipient `json:"recipient"`
	Name      string    `json:"name"`
	Channel   string    `json:"channel"`
	Address   string    `json:"address"`
	Send      bool      `json:"send"`
	Reason    string    `json:"reason,omitempty"`
	Subject   string    `json:"subject,omitempty"`
	Text      string    `json:"text,omitempty"`
	HTML      string    `json:"html,omitempty"`
	Mode      string    `json:"mode,omitempty"` // whatsapp: text | template
	Template  string    `json:"template,omitempty"`
	Params    []string  `json:"params,omitempty"`
}

// Preview is the composer's "who gets what".
type Preview struct {
	Recipients int            `json:"recipients"`
	PerChannel map[string]int `json:"per_channel"` // will be sent
	Skipped    map[string]int `json:"skipped"`     // reason → count
	Samples    []Sample       `json:"samples"`
	Options    []Recipient    `json:"options"` // recipients to pick for the preview (first 50)
	Upcoming   []string       `json:"upcoming"`
	RepeatText string         `json:"repeat_text"`
	Warnings   []string       `json:"warnings"`
}

// Preview renders m (unsaved) for one recipient (pick = id; "" = the first).
func (s *Service) Preview(m *Message, pick string) (*Preview, error) {
	org := m.OrganizationID
	p := &Preview{PerChannel: map[string]int{}, Skipped: map[string]int{}, Samples: []Sample{}, Options: []Recipient{}, Upcoming: []string{}, Warnings: []string{}}
	sch := Schedule{Start: m.LocalSendAt, TZ: m.Timezone, Repeat: m.Repeat}
	for _, t := range sch.Upcoming(5) {
		p.Upcoming = append(p.Upcoming, FormatLocal(t, m.Timezone))
	}
	p.RepeatText = m.Repeat.Describe(m.LocalSendAt)
	var recips []Recipient
	var err error
	var focus *OpenInvoice
	switch m.Kind {
	case KindTeam:
		recips, err = s.Staff(org, m.Audience)
	case KindReminder:
		inv := s.Invoice(org, m.InvoiceID)
		if c := s.Customer(org, inv.CustomerID); c != nil {
			recips = []Recipient{*c}
		}
		focus = &OpenInvoice{ID: inv.ID, Number: inv.Number, Currency: inv.Currency, BalanceDue: inv.BalanceDue, DueDate: inv.DueDate, PublicToken: inv.PublicToken}
	default:
		recips, err = s.Customers(org, m.Audience)
	}
	if err != nil {
		return nil, bad("%s", err.Error())
	}
	p.Recipients = len(recips)
	ids := make([]string, 0, len(recips))
	for _, c := range recips {
		if c.Type == "customer" {
			ids = append(ids, c.ID)
		}
	}
	allOpen := s.openInvoices(org, ids)
	var skipZero map[string][]OpenInvoice
	if m.Kind == KindStatement && m.Audience.SkipZeroBalance {
		skipZero = allOpen
	}
	chans := m.Channels
	if m.Kind == KindReminder {
		chans = without(chans, ChanEmail) // the email is previewed in the send dialog
	}
	waReady := true
	if has(chans, ChanWhatsApp) {
		if _, err := s.wa().Resolve(org); err != nil {
			waReady = false
			p.Warnings = append(p.Warnings, err.Error())
		}
	}
	if has(chans, ChanSMS) {
		if s.SMS == nil {
			if _, err := ResolveSMS(s.DB, org); err != nil {
				p.Warnings = append(p.Warnings, err.Error())
			}
		}
	}
	if has(chans, ChanEmail) {
		if s.Mail == nil {
			if ok, _, problem := s.EmailReady(org); !ok {
				p.Warnings = append(p.Warnings, firstNonEmpty(problem, "Email sending isn't set up (Settings → Outgoing email)"))
			}
		}
	}
	var chosen *Recipient
	for i := range recips {
		c := recips[i]
		if len(p.Options) < 50 {
			p.Options = append(p.Options, c)
		}
		if chosen == nil && (pick == "" || pick == c.ID) {
			chosen = &recips[i]
		}
		for _, ch := range chans {
			_, why := s.target(m, c, ch, skipZero)
			if why == "" && ch == ChanWhatsApp && !waReady {
				why = "WhatsApp is not set up"
			}
			if why == "" && ch == ChanWhatsApp && m.WhatsApp.Template == "" {
				if addr, _ := s.phoneOf(org, c); !s.wa().WindowOpen(org, strings.TrimPrefix(addr, "+"), windowNow()) {
					why = "No WhatsApp conversation in the last 24 hours and no template chosen"
				}
			}
			if why != "" {
				p.Skipped[why]++
			} else {
				p.PerChannel[ch]++
			}
		}
	}
	if chosen == nil {
		return p, nil
	}
	c := *chosen
	unsub := ""
	if m.Marketing && c.Type == "customer" {
		unsub = s.UnsubscribeURL(org, c.ID)
		if unsub == "" {
			unsub = "(unsubscribe link)"
		}
	}
	open := allOpen[c.ID]
	vals := s.Values(org, c, open, focus, unsub)
	short := firstNonEmpty(m.SMSBody, m.Body)
	for _, ch := range chans {
		addr, why := s.target(m, c, ch, skipZero)
		smp := Sample{Recipient: c, Name: c.Name(), Channel: ch, Address: addr, Send: why == "", Reason: why}
		switch ch {
		case ChanEmail, ChanInApp:
			smp.Subject, _ = Render(m.Subject, vals)
			smp.Text, _ = Render(m.Body, vals)
			if ch == ChanEmail {
				smp.HTML, _ = s.emailFor(org, m, smp.Subject, smp.Text, open, unsub, "", m.Kind == KindTeam)
			}
		case ChanSMS:
			smp.Text, _ = Render(short, vals)
			if unsub != "" {
				smp.Text += " Reply STOP to opt out."
			}
		case ChanWhatsApp:
			text, _ := Render(short, vals)
			windowOpen := s.wa().WindowOpen(org, strings.TrimPrefix(addr, "+"), windowNow())
			switch {
			case text != "" && !m.WhatsApp.TemplateOnly && windowOpen:
				smp.Mode, smp.Text = "text", text
			case m.WhatsApp.Template != "":
				smp.Mode, smp.Template = "template", m.WhatsApp.Template
				for _, prm := range m.WhatsApp.Params {
					v, _ := Render(prm, vals)
					smp.Params = append(smp.Params, v)
				}
				smp.Text = "Template " + m.WhatsApp.Template
				if !windowOpen && text != "" && !m.WhatsApp.TemplateOnly {
					smp.Text += " (no conversation in the last 24 hours, so free text isn't allowed)"
				}
			default:
				smp.Send = false
				if smp.Reason == "" {
					smp.Reason = "No WhatsApp conversation in the last 24 hours and no template chosen — it will be skipped"
				}
			}
			if !waReady {
				smp.Send, smp.Reason = false, "WhatsApp is not set up"
			}
		}
		p.Samples = append(p.Samples, smp)
	}
	return p, nil
}
