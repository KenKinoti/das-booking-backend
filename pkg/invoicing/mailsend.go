package invoicing

import (
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Sender describes who invoice emails come from, for the send dialog.
type Sender struct {
	Ready     bool     `json:"ready"`
	From      string   `json:"from"`
	FromEmail string   `json:"from_email"`
	FromName  string   `json:"from_name"`
	Source    string   `json:"source"`
	AlwaysCC  []string `json:"always_cc"`
	AlwaysBCC []string `json:"always_bcc"`
	Problem   string   `json:"problem,omitempty"`
}

// mailConfig resolves the organisation's outgoing mail configuration through
// pkg/mail. ok is false when the handler uses a legacy Mailer (tests).
func (h *Handler) mailConfig(org string) (mail.Config, bool) {
	if h.Mailer != nil || h.Mail == nil {
		return mail.Config{}, false
	}
	cfg, _ := h.Mail.Resolve(org)
	return cfg, true
}

// emailEnabled reports whether invoices can be emailed directly for org.
func (h *Handler) emailEnabled(org string) bool {
	if cfg, ok := h.mailConfig(org); ok {
		return cfg.Ready()
	}
	return h.Mailer != nil && h.Mailer.Configured()
}

// sender returns the effective sender (nil with a legacy Mailer).
func (h *Handler) sender(org string) *Sender {
	cfg, ok := h.mailConfig(org)
	if !ok {
		return nil
	}
	s := &Sender{Ready: cfg.Ready(), From: cfg.Display(), FromEmail: cfg.FromEmail, FromName: cfg.FromName,
		Source: cfg.Source, AlwaysCC: cfg.CC, AlwaysBCC: cfg.BCC, Problem: cfg.Problem}
	if s.AlwaysCC == nil {
		s.AlwaysCC = []string{}
	}
	if s.AlwaysBCC == nil {
		s.AlwaysBCC = []string{}
	}
	return s
}

// withoutAddresses drops every address of remove from list (case-insensitive).
func withoutAddresses(list, remove []string) []string {
	if len(remove) == 0 {
		return list
	}
	drop := map[string]bool{}
	for _, r := range remove {
		drop[strings.ToLower(strings.TrimSpace(r))] = true
	}
	out := []string{}
	for _, e := range list {
		if !drop[strings.ToLower(strings.TrimSpace(e))] {
			out = append(out, e)
		}
	}
	return out
}
