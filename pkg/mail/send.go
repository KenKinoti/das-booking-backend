package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strings"
	"time"
)

// Timeout bounds one whole SMTP conversation.
const Timeout = 20 * time.Second

// Attachment is a file attached to a message.
type Attachment struct {
	Filename    string
	ContentType string // e.g. application/pdf; defaults to application/octet-stream
	Data        []byte
}

// Message is one email. Addresses may be bare ("a@b.co") or "Name <a@b.co>".
type Message struct {
	To      []string
	CC      []string
	BCC     []string
	ReplyTo string
	Subject string
	Text    string
	HTML    string
	// Calendar adds a text/calendar alternative (meeting invitations) with
	// the given iTIP method (REQUEST, CANCEL). Add the .ics as an Attachment
	// too for clients that only look at attachments.
	Calendar       string
	CalendarMethod string
	Attachments    []Attachment
	// FromName overrides the configured display name for this message.
	FromName string
	// MessageID is the Message-ID header value without angle brackets
	// (NewMessageID); generated when empty. Callers set it to log the send.
	MessageID string
}

// NewMessageID returns a fresh Message-ID ("hex@domain") for fromEmail.
func NewMessageID(fromEmail string) string {
	domain := "dasyin.local"
	if at := strings.LastIndex(fromEmail, "@"); at >= 0 && at < len(fromEmail)-1 {
		domain = strings.ToLower(headerClean(fromEmail[at+1:]))
	}
	return randomHex(16) + "@" + domain
}

// Envelope is the normalised recipient set of a message.
type Envelope struct {
	To, CC, BCC []*mail.Address
}

// All returns every recipient address, in order (To, CC, BCC).
func (e Envelope) All() []string {
	out := []string{}
	for _, l := range [][]*mail.Address{e.To, e.CC, e.BCC} {
		for _, a := range l {
			out = append(out, a.Address)
		}
	}
	return out
}

// Prepare validates and de-duplicates recipients: every address appears once
// (To wins over CC, CC over BCC), invalid addresses are an error.
func Prepare(msg Message) (Envelope, error) {
	seen := map[string]bool{}
	var bad []string
	parse := func(list []string) []*mail.Address {
		out := []*mail.Address{}
		for _, raw := range list {
			for _, part := range splitAddresses(raw) {
				a, err := mail.ParseAddress(part)
				if err != nil || !ValidAddress(a.Address) {
					bad = append(bad, strings.TrimSpace(part))
					continue
				}
				a.Address = strings.ToLower(a.Address)
				a.Name = headerClean(a.Name)
				if seen[a.Address] {
					continue
				}
				seen[a.Address] = true
				out = append(out, a)
			}
		}
		return out
	}
	env := Envelope{To: parse(msg.To), CC: parse(msg.CC), BCC: parse(msg.BCC)}
	if len(bad) > 0 {
		return env, &Error{Code: "INVALID_RECIPIENT", Msg: fmt.Sprintf("Not a valid email address: %s", strings.Join(bad, ", "))}
	}
	if len(env.To)+len(env.CC)+len(env.BCC) == 0 {
		return env, &Error{Code: "NO_RECIPIENT", Msg: "Add at least one recipient"}
	}
	if len(env.To)+len(env.CC)+len(env.BCC) > 100 {
		return env, &Error{Code: "TOO_MANY_RECIPIENTS", Msg: "Too many recipients (max 100)"}
	}
	return env, nil
}

// splitAddresses splits "a@x, B <b@y>" on commas/semicolons that are not
// inside quotes or angle brackets.
func splitAddresses(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, inA := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
		case r == '<' && !inQ:
			inA = true
		case r == '>' && !inQ:
			inA = false
		case (r == ',' || r == ';' || r == '\n') && !inQ && !inA:
			if t := strings.TrimSpace(cur.String()); t != "" {
				out = append(out, t)
			}
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if t := strings.TrimSpace(cur.String()); t != "" {
		out = append(out, t)
	}
	return out
}

func headerClean(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// encodeHeader RFC 2047-encodes non-ASCII header text.
func encodeHeader(s string) string {
	s = headerClean(s)
	for _, r := range s {
		if r >= 0x80 {
			return mime.BEncoding.Encode("UTF-8", s)
		}
	}
	return s
}

// addressHeader writes "Name: a, b" folding long lists so no line exceeds
// the SMTP limit.
func addressHeader(buf *bytes.Buffer, name string, list []*mail.Address) {
	if len(list) == 0 {
		return
	}
	line := name + ": "
	buf.WriteString(line)
	col := len(line)
	for i, a := range list {
		s := formatAddress(a)
		if i > 0 {
			buf.WriteString(",")
			col++
			if col+len(s)+1 > 76 {
				buf.WriteString("\r\n")
				col = 0
			}
			buf.WriteString(" ")
			col++
		}
		buf.WriteString(s)
		col += len(s)
	}
	buf.WriteString("\r\n")
}

func normalizeNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

func qp(s string) string {
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(normalizeNewlines(s)))
	_ = w.Close()
	return b.String()
}

func b64Lines(data []byte) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String()
}

// Build renders the complete RFC 5322 message. BCC recipients never appear in
// the headers.
func Build(cfg Config, msg Message, env Envelope, now time.Time) []byte {
	var buf bytes.Buffer
	fromName := cfg.FromName
	if strings.TrimSpace(msg.FromName) != "" {
		fromName = msg.FromName
	}
	from := &mail.Address{Name: headerClean(fromName), Address: cfg.FromEmail}
	buf.WriteString("From: " + formatAddress(from) + "\r\n")
	addressHeader(&buf, "To", env.To)
	addressHeader(&buf, "Cc", env.CC)
	if len(env.To) == 0 && len(env.CC) == 0 {
		buf.WriteString("To: undisclosed-recipients:;\r\n")
	}
	replyTo := strings.TrimSpace(msg.ReplyTo)
	if replyTo == "" {
		replyTo = cfg.ReplyTo
	}
	if replyTo != "" {
		if a, err := mail.ParseAddress(replyTo); err == nil {
			buf.WriteString("Reply-To: " + formatAddress(a) + "\r\n")
		}
	}
	buf.WriteString("Subject: " + encodeHeader(msg.Subject) + "\r\n")
	buf.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	mid := strings.Trim(headerClean(msg.MessageID), "<>")
	if mid == "" || !strings.Contains(mid, "@") || strings.ContainsAny(mid, " <>") {
		mid = NewMessageID(cfg.FromEmail)
	}
	buf.WriteString("Message-ID: <" + mid + ">\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("X-Mailer: DASYIN ERP\r\n")

	type part struct{ header, body string }
	var alts []part
	alts = append(alts, part{"Content-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable", qp(msg.Text)})
	if msg.HTML != "" {
		alts = append(alts, part{"Content-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable", qp(msg.HTML)})
	}
	if msg.Calendar != "" {
		method := strings.ToUpper(strings.TrimSpace(msg.CalendarMethod))
		if method == "" {
			method = "REQUEST"
		}
		alts = append(alts, part{"Content-Type: text/calendar; charset=UTF-8; method=" + method + "\r\nContent-Transfer-Encoding: base64", b64Lines([]byte(msg.Calendar))})
	}

	writeAlt := func() {
		if len(alts) == 1 {
			buf.WriteString(alts[0].header + "\r\n\r\n" + alts[0].body)
			if !strings.HasSuffix(alts[0].body, "\r\n") {
				buf.WriteString("\r\n")
			}
			return
		}
		b := "alt-" + randomHex(12)
		buf.WriteString("Content-Type: multipart/alternative; boundary=\"" + b + "\"\r\n\r\n")
		for _, p := range alts {
			buf.WriteString("--" + b + "\r\n" + p.header + "\r\n\r\n" + p.body)
			if !strings.HasSuffix(p.body, "\r\n") {
				buf.WriteString("\r\n")
			}
		}
		buf.WriteString("--" + b + "--\r\n")
	}

	if len(msg.Attachments) == 0 {
		writeAlt()
		return buf.Bytes()
	}
	mixed := "mix-" + randomHex(12)
	buf.WriteString("Content-Type: multipart/mixed; boundary=\"" + mixed + "\"\r\n\r\n")
	buf.WriteString("--" + mixed + "\r\n")
	writeAlt()
	for _, a := range msg.Attachments {
		ct := strings.TrimSpace(a.ContentType)
		if ct == "" {
			ct = "application/octet-stream"
		}
		name := headerClean(a.Filename)
		if name == "" {
			name = "attachment"
		}
		mt, params, err := mime.ParseMediaType(ct)
		if err != nil {
			mt, params = "application/octet-stream", map[string]string{}
		}
		params["name"] = name
		buf.WriteString("--" + mixed + "\r\n")
		buf.WriteString("Content-Type: " + mime.FormatMediaType(mt, params) + "\r\n")
		buf.WriteString("Content-Disposition: " + mime.FormatMediaType("attachment", map[string]string{"filename": name}) + "\r\n")
		buf.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		buf.WriteString(b64Lines(a.Data))
	}
	buf.WriteString("--" + mixed + "--\r\n")
	return buf.Bytes()
}

// ---------------------------------------------------------------- errors

// Error is a send failure with a stable code and a human message.
type Error struct {
	Code string // NOT_CONFIGURED, AUTH_FAILED, CONNECTION_REFUSED, TIMEOUT, DNS, TLS, STARTTLS_UNSUPPORTED, RECIPIENT_REJECTED, SENDER_REJECTED, INVALID_RECIPIENT, NO_RECIPIENT, INSECURE_AUTH, SMTP
	Msg  string
	Err  error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// ErrNotConfigured is returned when no outgoing mail account is set up.
var ErrNotConfigured = &Error{Code: "NOT_CONFIGURED", Msg: "Email sending isn't set up. Configure an SMTP account under Settings → Outgoing email."}

// Code returns the error code of err ("" when it is not a *Error).
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func addr(cfg Config) string { return net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)) }

// friendly turns low-level errors into messages a user can act on.
func friendly(stage string, err error, cfg Config) error {
	if err == nil {
		return nil
	}
	var me *Error
	if errors.As(err, &me) {
		return err
	}
	raw := err.Error()
	low := strings.ToLower(raw)
	gmail := IsGmail(cfg.Host)
	code := smtpCode(err)
	switch {
	case isTimeout(err):
		hint := ""
		if cfg.Security != SecuritySSL && cfg.Port == 465 {
			hint = " Port 465 needs SSL/TLS security."
		}
		return &Error{Code: "TIMEOUT", Err: err, Msg: fmt.Sprintf("The SMTP server %s did not respond in time (%s).%s", addr(cfg), stage, hint)}
	case stage == "auth" || code == 535 || code == 534 || strings.Contains(low, "username and password not accepted") || strings.Contains(low, "authentication failed"):
		if gmail {
			return &Error{Code: "AUTH_FAILED", Err: err, Msg: "Gmail rejected the username/app password — use an App Password (Google Account → Security → 2-Step Verification → App passwords), not your normal Gmail password. Server said: " + raw}
		}
		return &Error{Code: "AUTH_FAILED", Err: err, Msg: "The SMTP server rejected the username or password. Server said: " + raw}
	case strings.Contains(low, "connection refused"):
		return &Error{Code: "CONNECTION_REFUSED", Err: err, Msg: fmt.Sprintf("Could not connect to %s — the connection was refused. Check the host and port.", addr(cfg))}
	case strings.Contains(low, "no such host") || strings.Contains(low, "server misbehaving"):
		return &Error{Code: "DNS", Err: err, Msg: fmt.Sprintf("The SMTP host %q could not be found. Check the spelling.", cfg.Host)}
	case strings.Contains(low, "first record does not look like a tls handshake") || strings.Contains(low, "tls: ") || strings.Contains(low, "x509") || strings.Contains(low, "certificate"):
		hint := ""
		if cfg.Security == SecuritySSL && cfg.Port != 465 {
			hint = " Port " + fmt.Sprint(cfg.Port) + " usually uses STARTTLS, not SSL."
		} else if strings.Contains(low, "first record") {
			hint = " This port does not speak TLS — use STARTTLS on 587 or SSL on 465."
		}
		return &Error{Code: "TLS", Err: err, Msg: "Secure (TLS) connection to " + addr(cfg) + " failed: " + raw + "." + hint}
	case stage == "rcpt":
		return &Error{Code: "RECIPIENT_REJECTED", Err: err, Msg: "The server refused a recipient: " + raw}
	case stage == "mail":
		msg := "The server refused the sender address " + cfg.FromEmail + ": " + raw
		if gmail {
			msg += " — with Gmail the From address must be the Gmail account or a verified “Send mail as” alias."
		}
		return &Error{Code: "SENDER_REJECTED", Err: err, Msg: msg}
	}
	return &Error{Code: "SMTP", Err: err, Msg: fmt.Sprintf("Sending failed (%s): %s", stage, raw)}
}

func smtpCode(err error) int {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code
	}
	return 0
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "i/o timeout")
}

// ---------------------------------------------------------------- SMTP

type plainAuth struct{ user, pass string }

func (a plainAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}
func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

type loginAuth struct{ user, pass string }

func (a loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }
func (a loginAuth) Next(from []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	p := strings.ToLower(strings.TrimSpace(string(from)))
	switch {
	case strings.Contains(p, "username") || strings.Contains(p, "user"):
		return []byte(a.user), nil
	case strings.Contains(p, "password") || strings.Contains(p, "pass"):
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("unexpected LOGIN prompt %q", from)
}

func localName() string {
	h, err := os.Hostname()
	if err != nil || h == "" || strings.ContainsAny(h, " \r\n") {
		return "localhost"
	}
	return h
}

// session opens an SMTP connection, negotiates TLS and signs in. The caller
// must Close the client.
func session(ctx context.Context, cfg Config) (*smtp.Client, net.Conn, error) {
	if cfg.Port <= 0 {
		cfg.Port = 587
	}
	cfg.Security = NormalizeSecurity(cfg.Security, cfg.Port)
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(Timeout)
	}
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	if cfg.TLSConfig != nil {
		tlsCfg = cfg.TLSConfig.Clone()
		if tlsCfg.ServerName == "" {
			tlsCfg.ServerName = cfg.Host
		}
	}
	d := &net.Dialer{Timeout: time.Until(deadline)}
	conn, err := d.DialContext(ctx, "tcp", addr(cfg))
	if err != nil {
		return nil, nil, friendly("connect", err, cfg)
	}
	_ = conn.SetDeadline(deadline)
	if cfg.Security == SecuritySSL {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, nil, friendly("tls", err, cfg)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return nil, nil, friendly("greeting", err, cfg)
	}
	if err := c.Hello(localName()); err != nil {
		c.Close()
		return nil, nil, friendly("hello", err, cfg)
	}
	tlsOn := cfg.Security == SecuritySSL
	if cfg.Security == SecurityAuto {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				c.Close()
				return nil, nil, friendly("tls", err, cfg)
			}
			tlsOn = true
		}
	}
	if cfg.Security == SecurityStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			c.Close()
			return nil, nil, &Error{Code: "STARTTLS_UNSUPPORTED", Msg: fmt.Sprintf("%s does not offer STARTTLS. Choose SSL/TLS (port 465) or, for a local relay only, no encryption.", addr(cfg))}
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			c.Close()
			return nil, nil, friendly("tls", err, cfg)
		}
		tlsOn = true
	}
	if cfg.Username != "" {
		if !tlsOn && !isLocal(cfg.Host) {
			c.Close()
			return nil, nil, &Error{Code: "INSECURE_AUTH", Msg: "Refusing to send the SMTP password over an unencrypted connection — choose STARTTLS (587) or SSL/TLS (465)."}
		}
		ok, mechs := c.Extension("AUTH")
		if !ok {
			c.Close()
			return nil, nil, &Error{Code: "AUTH_FAILED", Msg: "The server does not accept sign-in (no AUTH). Remove the username or use your provider's submission port."}
		}
		pw := CleanPassword(cfg.Host, cfg.Password)
		var a smtp.Auth = plainAuth{cfg.Username, pw}
		up := " " + strings.ToUpper(mechs) + " "
		if !strings.Contains(up, " PLAIN ") && strings.Contains(up, " LOGIN ") {
			a = loginAuth{cfg.Username, pw}
		}
		if err := c.Auth(a); err != nil {
			c.Close()
			return nil, nil, friendly("auth", err, cfg)
		}
	}
	return c, conn, nil
}

// Probe connects, negotiates TLS and signs in without sending anything.
func Probe(ctx context.Context, cfg Config) error {
	if cfg.Host == "" {
		return &Error{Code: "NOT_CONFIGURED", Msg: "Enter an SMTP host first"}
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	c, _, err := session(ctx, cfg)
	if err != nil {
		return err
	}
	_ = c.Quit()
	return nil
}

// Send delivers msg with cfg. Every To/CC/BCC address gets its own RCPT TO;
// BCC addresses are not written to the headers.
func Send(ctx context.Context, cfg Config, msg Message) error {
	if cfg.Problem != "" {
		return &Error{Code: "NOT_CONFIGURED", Msg: cfg.Problem}
	}
	if !cfg.Ready() {
		return ErrNotConfigured
	}
	env, err := Prepare(msg)
	if err != nil {
		return err
	}
	if cfg.Port <= 0 {
		cfg.Port = 587
	}
	cfg.Security = NormalizeSecurity(cfg.Security, cfg.Port)
	raw := Build(cfg, msg, env, time.Now())
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	c, _, err := session(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Mail(cfg.FromEmail); err != nil {
		return friendly("mail", err, cfg)
	}
	for _, r := range env.All() {
		if err := c.Rcpt(r); err != nil {
			return friendly("rcpt", fmt.Errorf("%s: %w", r, err), cfg)
		}
	}
	w, err := c.Data()
	if err != nil {
		return friendly("data", err, cfg)
	}
	if _, err := w.Write(raw); err != nil {
		return friendly("data", err, cfg)
	}
	if err := w.Close(); err != nil {
		return friendly("data", err, cfg)
	}
	_ = c.Quit()
	return nil
}

func isLocal(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// formatAddress renders "Name <a@b>" (RFC 2047-encoding the name when
// needed) or the bare address when there is no name.
func formatAddress(a *mail.Address) string {
	if strings.TrimSpace(a.Name) == "" {
		return a.Address
	}
	return a.String()
}
