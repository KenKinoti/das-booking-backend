package meetings

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"
	netmail "net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Mailer sends calendar invitations. Implemented by SMTPMailer; tests may
// substitute a fake.
type Mailer interface {
	Configured() bool
	SendInvite(msg InviteMessage) error
}

// InviteMessage is one email carrying an iCalendar invitation.
type InviteMessage struct {
	To       string
	ToName   string
	Subject  string
	Text     string
	HTML     string
	ICS      string
	Method   string // REQUEST or CANCEL
	FromName string
}

// SMTPMailer sends email via SMTP using the same environment variables as the
// invoicing module: SMTP_HOST, SMTP_PORT, SMTP_USERNAME, SMTP_PASSWORD, SMTP_FROM.
type SMTPMailer struct {
	Host, Port, Username, Password, From string
}

// SMTPMailerFromEnv reads SMTP settings from the environment.
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

// Configured reports whether invitations can be emailed.
func (m *SMTPMailer) Configured() bool { return m != nil && m.Host != "" && m.From != "" }

func headerSafe(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

func addressOnly(from string) string {
	if i := strings.LastIndex(from, "<"); i >= 0 {
		if j := strings.LastIndex(from, ">"); j > i {
			return strings.TrimSpace(from[i+1 : j])
		}
	}
	return strings.TrimSpace(from)
}

func boundary() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "dasmeet-" + hex.EncodeToString(b)
}

func writeBase64(buf *bytes.Buffer, data string) {
	enc := base64.StdEncoding.EncodeToString([]byte(data))
	for len(enc) > 76 {
		buf.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	buf.WriteString(enc + "\r\n")
}

// BuildMIME renders the message as multipart/mixed containing a
// multipart/alternative (text/plain, text/html, text/calendar; method=...)
// plus an invite.ics attachment, which is what Gmail and Outlook need to show
// Accept/Decline controls.
func BuildMIME(from string, msg InviteMessage) []byte {
	method := strings.ToUpper(msg.Method)
	if method == "" {
		method = MethodRequest
	}
	mixed, alt := boundary(), boundary()
	var buf bytes.Buffer
	fromHeader := headerSafe(from)
	if msg.FromName != "" && !strings.Contains(from, "<") {
		fromHeader = mime.QEncoding.Encode("utf-8", headerSafe(msg.FromName)) + " <" + headerSafe(from) + ">"
	}
	to := headerSafe(msg.To)
	if msg.ToName != "" {
		to = mime.QEncoding.Encode("utf-8", headerSafe(msg.ToName)) + " <" + to + ">"
	}
	fmt.Fprintf(&buf, "From: %s\r\n", fromHeader)
	fmt.Fprintf(&buf, "To: %s\r\n", to)
	fmt.Fprintf(&buf, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", headerSafe(msg.Subject)))
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "Message-ID: <%s@dasyin-erp>\r\n", boundary())
	buf.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", mixed)

	fmt.Fprintf(&buf, "--%s\r\n", mixed)
	fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", alt)

	fmt.Fprintf(&buf, "--%s\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", alt)
	writeBase64(&buf, msg.Text)
	if msg.HTML != "" {
		fmt.Fprintf(&buf, "--%s\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", alt)
		writeBase64(&buf, msg.HTML)
	}
	fmt.Fprintf(&buf, "--%s\r\nContent-Type: text/calendar; charset=UTF-8; method=%s\r\nContent-Transfer-Encoding: base64\r\n\r\n", alt, method)
	writeBase64(&buf, msg.ICS)
	fmt.Fprintf(&buf, "--%s--\r\n", alt)

	fmt.Fprintf(&buf, "--%s\r\n", mixed)
	fmt.Fprintf(&buf, "Content-Type: application/ics; name=\"invite.ics\"\r\nContent-Disposition: attachment; filename=\"invite.ics\"\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	writeBase64(&buf, msg.ICS)
	fmt.Fprintf(&buf, "--%s--\r\n", mixed)
	return buf.Bytes()
}

// SendInvite delivers one invitation through pkg/mail.
func (m *SMTPMailer) SendInvite(msg InviteMessage) error {
	if !m.Configured() {
		return fmt.Errorf("SMTP is not configured")
	}
	port, _ := strconv.Atoi(m.Port)
	if port <= 0 {
		port = 587
	}
	cfg := mail.Config{Host: m.Host, Port: port, Username: m.Username, Password: m.Password,
		Security: mail.NormalizeSecurity("auto", port), FromEmail: strings.ToLower(addressOnly(m.From)), Source: mail.SourceEnvironment}
	if i := strings.Index(m.From, "<"); i > 0 {
		cfg.FromName = strings.Trim(strings.TrimSpace(m.From[:i]), `"`)
	}
	return mail.Send(context.Background(), cfg, inviteMessage(msg))
}

// inviteMessage converts an invitation to a pkg/mail message: text + HTML +
// text/calendar alternatives plus an invite.ics attachment, which is what
// Gmail and Outlook need to show Accept/Decline controls.
func inviteMessage(msg InviteMessage) mail.Message {
	method := strings.ToUpper(msg.Method)
	if method == "" {
		method = MethodRequest
	}
	to := strings.TrimSpace(headerSafe(msg.To))
	if msg.ToName != "" {
		to = (&netmail.Address{Name: headerSafe(msg.ToName), Address: to}).String()
	}
	return mail.Message{
		To: []string{to}, Subject: msg.Subject, Text: msg.Text, HTML: msg.HTML,
		Calendar: msg.ICS, CalendarMethod: method, FromName: msg.FromName,
		Attachments: []mail.Attachment{{Filename: "invite.ics", ContentType: "application/ics", Data: []byte(msg.ICS)}},
	}
}

// OrgMailer sends invitations with the organisation's resolved outgoing email
// account (organisation settings → platform default → SMTP_* env).
type OrgMailer struct {
	Svc *mail.Service
	Org string
}

// Configured reports whether the organisation can send email.
func (o *OrgMailer) Configured() bool {
	cfg, _ := o.Svc.Resolve(o.Org)
	return cfg.Ready()
}

// SendInvite delivers one invitation and records the result.
func (o *OrgMailer) SendInvite(msg InviteMessage) error {
	_, err := o.Svc.SendAs(context.Background(), o.Org, inviteMessage(msg))
	return err
}
