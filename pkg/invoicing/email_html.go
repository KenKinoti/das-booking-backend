package invoicing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime/quotedprintable"
	"regexp"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// HTMLMailer is implemented by mailers that can send a multipart/alternative
// (plain text + HTML) message.
type HTMLMailer interface {
	SendHTML(to string, cc []string, subject, text, htmlBody string) error
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"']+`)

// EmailHTML renders a message in the shared email template: the company logo
// at the top (or the business name when there is no logo), then the message
// with links made clickable. logoURL must be absolute so mail clients can load it.
func EmailHTML(business, logoURL, accent, message string) string {
	return EmailHTMLWithActions(business, logoURL, accent, message, nil, "")
}

// EmailAction is a button in an email (View invoice, Pay now …).
type EmailAction struct {
	Label   string
	URL     string
	Primary bool
}

// EmailHTMLWithActions renders a simple branded email (the shared template,
// see RenderEmail) with call-to-action buttons and an optional small note.
func EmailHTMLWithActions(business, logoURL, accent, message string, actions []EmailAction, note string) string {
	out, _ := RenderEmail(EmailView{Business: EmailBusiness{Name: business}, LogoURL: logoURL, Accent: accent, Message: message, Actions: actions, Note: note})
	return out
}

// SendHTML sends a multipart/alternative message (text + HTML) to `to`,
// copying `cc`.
func (m *SMTPMailer) SendHTML(to string, cc []string, subject, text, htmlBody string) error {
	to, cc, _ = MergeRecipients(to, cc)
	if to == "" {
		return fmt.Errorf("no valid recipient")
	}
	return mail.Send(context.Background(), m.config(), mail.Message{To: []string{to}, CC: cc, Subject: subject, Text: text, HTML: htmlBody})
}

// BuildHTMLMessage renders an RFC 5322 multipart/alternative message.
func BuildHTMLMessage(from, to string, cc []string, subject, text, htmlBody string) []byte {
	plain := string(BuildMessage(from, to, cc, subject, ""))
	// Reuse the headers from the plain builder, minus its content type.
	hdr := plain[:strings.Index(plain, "MIME-Version:")]
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	boundary := "dasyin-" + hex.EncodeToString(b)
	// Quoted-printable keeps every line under the SMTP 998-character limit.
	qp := func(s string) string {
		var buf bytes.Buffer
		w := quotedprintable.NewWriter(&buf)
		_, _ = w.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")))
		_ = w.Close()
		return buf.String()
	}
	var sb strings.Builder
	sb.WriteString(hdr)
	sb.WriteString("MIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
	sb.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	sb.WriteString(qp(text) + "\r\n")
	sb.WriteString("--" + boundary + "\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	sb.WriteString(qp(htmlBody) + "\r\n")
	sb.WriteString("--" + boundary + "--\r\n")
	return []byte(sb.String())
}
