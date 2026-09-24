package meetings

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"os"
	"strings"
	"time"
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

// SendInvite delivers one invitation. Port 465 uses implicit TLS; other
// ports use STARTTLS when the server offers it.
func (m *SMTPMailer) SendInvite(msg InviteMessage) error {
	if !m.Configured() {
		return fmt.Errorf("SMTP is not configured")
	}
	to := strings.TrimSpace(headerSafe(msg.To))
	body := BuildMIME(m.From, msg)
	sender := addressOnly(m.From)
	var auth smtp.Auth
	if m.Username != "" {
		auth = smtp.PlainAuth("", m.Username, m.Password, m.Host)
	}
	addr := net.JoinHostPort(m.Host, m.Port)
	if m.Port != "465" {
		return smtp.SendMail(addr, auth, sender, []string{to}, body)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, &tls.Config{ServerName: m.Host})
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(sender); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
