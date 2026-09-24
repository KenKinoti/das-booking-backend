package invoicing

import (
	"fmt"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
)

// SMTPMailer sends plain-text email using SMTP settings from the environment:
// SMTP_HOST, SMTP_PORT, SMTP_USERNAME, SMTP_PASSWORD, SMTP_FROM.
type SMTPMailer struct {
	Host, Port, Username, Password, From string
}

// CCMailer is implemented by mailers that can copy other recipients.
type CCMailer interface {
	SendWithCC(to string, cc []string, subject, body string) error
}

func SMTPMailerFromEnv() *SMTPMailer {
	port := os.Getenv("SMTP_PORT")
	if port == "" {
		port = "587"
	}
	from := os.Getenv("SMTP_FROM")
	if from == "" {
		from = os.Getenv("SMTP_USERNAME")
	}
	return &SMTPMailer{Host: os.Getenv("SMTP_HOST"), Port: port, Username: os.Getenv("SMTP_USERNAME"), Password: os.Getenv("SMTP_PASSWORD"), From: from}
}

func (m *SMTPMailer) Configured() bool { return m != nil && m.Host != "" && m.From != "" }

func (m *SMTPMailer) Send(to, subject, body string) error {
	return m.SendWithCC(to, nil, subject, body)
}

// SendWithCC sends one message to `to`, copying `cc` (visible Cc header). Every
// address is delivered with its own RCPT TO.
func (m *SMTPMailer) SendWithCC(to string, cc []string, subject, body string) error {
	to, cc, _ = MergeRecipients(to, cc)
	if to == "" {
		return fmt.Errorf("no valid recipient")
	}
	msg := BuildMessage(m.From, to, cc, subject, body)
	var auth smtp.Auth
	if m.Username != "" {
		auth = smtp.PlainAuth("", m.Username, m.Password, m.Host)
	}
	return smtp.SendMail(m.Host+":"+m.Port, auth, envelopeAddress(m.From), append([]string{to}, cc...), msg)
}

// headerClean strips CR/LF so values can't inject extra headers.
func headerClean(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
}

// envelopeAddress returns the bare address of "Name <a@b>" (or the input).
func envelopeAddress(s string) string {
	if a, err := mail.ParseAddress(headerClean(s)); err == nil {
		return a.Address
	}
	return headerClean(s)
}

// BuildMessage renders an RFC 5322 plain-text message with To and (when
// present) Cc headers.
func BuildMessage(from, to string, cc []string, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: " + headerClean(from) + "\r\n")
	b.WriteString("To: " + headerClean(to) + "\r\n")
	if len(cc) > 0 {
		clean := make([]string, 0, len(cc))
		for _, c := range cc {
			if c = headerClean(c); c != "" {
				clean = append(clean, c)
			}
		}
		if len(clean) > 0 {
			b.WriteString("Cc: " + strings.Join(clean, ", ") + "\r\n")
		}
	}
	b.WriteString("Subject: " + headerClean(subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}

// ValidEmail reports whether s is a single plain address like a@b.co.
func ValidEmail(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 254 || strings.ContainsAny(s, " \r\n,;<>") {
		return false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return false
	}
	at := strings.LastIndex(s, "@")
	return at > 0 && strings.Contains(s[at+1:], ".") && !strings.HasSuffix(s, ".")
}

// MergeRecipients normalises the To address and merges CC lists: trims and
// lower-cases, drops invalid addresses (returned in skipped), removes
// duplicates (case-insensitive) and anything equal to To. Order is kept.
func MergeRecipients(to string, ccLists ...[]string) (string, []string, []string) {
	to = strings.ToLower(strings.TrimSpace(to))
	skipped := []string{}
	if to != "" && !ValidEmail(to) {
		skipped = append(skipped, to)
		to = ""
	}
	seen := map[string]bool{}
	if to != "" {
		seen[to] = true
	}
	cc := []string{}
	for _, list := range ccLists {
		for _, raw := range list {
			// Accept "a@x.com, b@y.com" pasted into a single entry.
			for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
				e := strings.ToLower(strings.TrimSpace(part))
				if e == "" || seen[e] {
					continue
				}
				if !ValidEmail(e) {
					skipped = append(skipped, e)
					seen[e] = true
					continue
				}
				seen[e] = true
				cc = append(cc, e)
			}
		}
	}
	return to, cc, skipped
}
