package invoicing

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"mime/quotedprintable"
	"net/smtp"
	"regexp"
	"strings"
)

// HTMLMailer is implemented by mailers that can send a multipart/alternative
// (plain text + HTML) message.
type HTMLMailer interface {
	SendHTML(to string, cc []string, subject, text, htmlBody string) error
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"']+`)

// EmailHTML renders the invoice email: the company logo at the top (or the
// business name in bold when there is no logo), then the message with links
// made clickable. logoURL must be absolute so mail clients can load it.
func EmailHTML(business, logoURL, accent, message string) string {
	if !regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`).MatchString(accent) {
		accent = "#4f46e5"
	}
	var head string
	if strings.HasPrefix(logoURL, "https://") || strings.HasPrefix(logoURL, "http://") {
		head = fmt.Sprintf(`<img src="%s" alt="%s" style="display:block;max-height:64px;max-width:220px;height:auto;width:auto;border:0;outline:none">`,
			html.EscapeString(logoURL), html.EscapeString(business))
	} else {
		head = fmt.Sprintf(`<div style="font-size:20px;font-weight:700;color:#111322">%s</div>`, html.EscapeString(business))
	}
	// Escape, then linkify, then keep line breaks.
	var body strings.Builder
	msg := strings.ReplaceAll(message, "\r\n", "\n")
	last := 0
	for _, m := range urlRe.FindAllStringIndex(msg, -1) {
		body.WriteString(html.EscapeString(msg[last:m[0]]))
		u := strings.TrimRight(msg[m[0]:m[1]], ".,);")
		body.WriteString(fmt.Sprintf(`<a href="%s" style="color:%s">%s</a>`, html.EscapeString(u), accent, html.EscapeString(u)))
		last = m[0] + len(u)
	}
	body.WriteString(html.EscapeString(msg[last:]))
	content := strings.ReplaceAll(body.String(), "\n", "<br>\n")
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>` +
		`<body style="margin:0;padding:0;background:#f6f7fb">` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f6f7fb"><tr><td align="center" style="padding:24px 12px">` +
		`<table role="presentation" width="600" cellpadding="0" cellspacing="0" style="max-width:600px;width:100%;background:#ffffff;border-radius:12px;border-top:4px solid ` + accent + `">` +
		`<tr><td style="padding:28px 32px 8px">` + head + `</td></tr>` +
		`<tr><td style="padding:16px 32px 32px;font:15px/1.6 -apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1a1d2e">` + content + `</td></tr>` +
		`</table></td></tr></table></body></html>`
}

// SendHTML sends a multipart/alternative message (text + HTML) to `to`,
// copying `cc`.
func (m *SMTPMailer) SendHTML(to string, cc []string, subject, text, htmlBody string) error {
	to, cc, _ = MergeRecipients(to, cc)
	if to == "" {
		return fmt.Errorf("no valid recipient")
	}
	msg := BuildHTMLMessage(m.From, to, cc, subject, text, htmlBody)
	var auth smtp.Auth
	if m.Username != "" {
		auth = smtp.PlainAuth("", m.Username, m.Password, m.Host)
	}
	return smtp.SendMail(m.Host+":"+m.Port, auth, envelopeAddress(m.From), append([]string{to}, cc...), msg)
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
