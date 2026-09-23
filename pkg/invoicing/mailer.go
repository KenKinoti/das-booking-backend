package invoicing

import (
	"fmt"
	"net/smtp"
	"os"
	"strings"
)

// SMTPMailer sends plain-text email using SMTP settings from the environment:
// SMTP_HOST, SMTP_PORT, SMTP_USERNAME, SMTP_PASSWORD, SMTP_FROM.
type SMTPMailer struct {
	Host, Port, Username, Password, From string
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
	clean := func(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }
	to, subject = clean(to), clean(subject)
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s", m.From, to, subject, body)
	var auth smtp.Auth
	if m.Username != "" {
		auth = smtp.PlainAuth("", m.Username, m.Password, m.Host)
	}
	return smtp.SendMail(m.Host+":"+m.Port, auth, m.From, []string{to}, []byte(msg))
}
