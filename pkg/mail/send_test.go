package mail

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail/mailtest"
)

func baseCfg(s *mailtest.Server, sec string) Config {
	return Config{Host: s.Host, Port: s.Port, Security: sec, FromEmail: "billing@acme.test", FromName: "Acme Billing", TLSConfig: s.ClientTLS()}
}

func TestBuildHeadersCcBccReplyToUTF8(t *testing.T) {
	cfg := Config{FromEmail: "billing@acme.test", FromName: "Café Ünïcode", ReplyTo: "accounts@acme.test"}
	msg := Message{To: []string{"Client <client@x.test>"}, CC: []string{"a@x.test", "CLIENT@x.test"}, BCC: []string{"secret@x.test", "a@x.test"},
		Subject: "Rechnung für März — 100 €\r\nBcc: evil@x.test", Text: "Hallo\nWelt", HTML: "<p>Hallo</p>"}
	env, err := Prepare(msg)
	if err != nil {
		t.Fatal(err)
	}
	if got := env.All(); !reflect.DeepEqual(got, []string{"client@x.test", "a@x.test", "secret@x.test"}) {
		t.Fatalf("recipients = %v", got)
	}
	raw := string(Build(cfg, msg, env, time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)))
	head := raw[:strings.Index(raw, "\r\n\r\n")]
	if strings.Contains(strings.ToLower(head), "secret@x.test") || strings.Contains(strings.ToLower(head), "bcc") {
		t.Fatalf("BCC leaked into headers:\n%s", head)
	}
	if strings.Contains(raw, "evil@x.test") && !strings.Contains(raw, "=?") {
		t.Fatalf("header injection:\n%s", head)
	}
	m, err := netmail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	dec := new(mime.WordDecoder)
	subj, err := dec.DecodeHeader(m.Header.Get("Subject"))
	if err != nil || strings.Join(strings.Fields(subj), " ") != "Rechnung für März — 100 € Bcc: evil@x.test" || len(m.Header["Bcc"]) > 0 {
		t.Errorf("subject = %q (%v)", subj, err)
	}
	from, err := m.Header.AddressList("From")
	if err != nil || from[0].Name != "Café Ünïcode" || from[0].Address != "billing@acme.test" {
		t.Errorf("from = %v %v", from, err)
	}
	if got := m.Header.Get("Cc"); got != "a@x.test" {
		t.Errorf("Cc = %q", got)
	}
	if got := m.Header.Get("Reply-To"); got != "accounts@acme.test" {
		t.Errorf("Reply-To = %q", got)
	}
	if m.Header.Get("Message-ID") == "" || !strings.HasSuffix(m.Header.Get("Message-ID"), "@acme.test>") {
		t.Errorf("Message-ID = %q", m.Header.Get("Message-ID"))
	}
	if m.Header.Get("Date") != "Wed, 30 Sep 2026 10:00:00 +0000" {
		t.Errorf("Date = %q", m.Header.Get("Date"))
	}
	mt, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if mt != "multipart/alternative" {
		t.Fatalf("content type %s", mt)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	var types []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, strings.Split(p.Header.Get("Content-Type"), ";")[0])
		b, _ := io.ReadAll(p) // multipart decodes quoted-printable
		if strings.HasPrefix(p.Header.Get("Content-Type"), "text/plain") && string(b) != "Hallo\r\nWelt" {
			t.Errorf("text body = %q", b)
		}
	}
	if !reflect.DeepEqual(types, []string{"text/plain", "text/html"}) {
		t.Errorf("parts = %v", types)
	}
}

func TestBuildAttachmentsAndCalendar(t *testing.T) {
	cfg := Config{FromEmail: "x@acme.test"}
	msg := Message{To: []string{"a@x.test"}, Subject: "Invite", Text: "hi", HTML: "<b>hi</b>", Calendar: "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n", CalendarMethod: "request",
		Attachments: []Attachment{{Filename: "invite.ics", ContentType: "application/ics", Data: []byte("BEGIN:VCALENDAR")}, {Filename: "Rechnung März.pdf", ContentType: "application/pdf", Data: make([]byte, 200)}}}
	env, _ := Prepare(msg)
	raw := string(Build(cfg, msg, env, time.Now()))
	m, err := netmail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if mt != "multipart/mixed" {
		t.Fatalf("top = %s", mt)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	var names []string
	first := true
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if first {
			first = false
			ct := p.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "multipart/alternative") {
				t.Fatalf("first part %s", ct)
			}
			_, ap, _ := mime.ParseMediaType(ct)
			ar := multipart.NewReader(p, ap["boundary"])
			var alts []string
			for {
				q, err := ar.NextPart()
				if err == io.EOF {
					break
				}
				alts = append(alts, q.Header.Get("Content-Type"))
			}
			if len(alts) != 3 || !strings.Contains(alts[2], "text/calendar") || !strings.Contains(alts[2], "method=REQUEST") {
				t.Errorf("alternatives = %v", alts)
			}
			continue
		}
		names = append(names, p.FileName())
		for _, l := range strings.Split(raw, "\r\n") {
			if len(l) > 998 {
				t.Fatal("line too long")
			}
		}
	}
	if !reflect.DeepEqual(names, []string{"invite.ics", "Rechnung März.pdf"}) {
		t.Errorf("attachments = %v", names)
	}
}

func TestPrepareValidation(t *testing.T) {
	if _, err := Prepare(Message{To: []string{"nope"}}); Code(err) != "INVALID_RECIPIENT" {
		t.Errorf("invalid: %v", err)
	}
	if _, err := Prepare(Message{}); Code(err) != "NO_RECIPIENT" {
		t.Errorf("empty: %v", err)
	}
	env, err := Prepare(Message{To: []string{"a@x.test, B <b@x.test>; c@x.test"}, CC: []string{"A@X.TEST"}})
	if err != nil || len(env.To) != 3 || len(env.CC) != 0 {
		t.Errorf("split/dedupe: %v %v", env, err)
	}
}

func TestSendSTARTTLSWithAuth(t *testing.T) {
	s := mailtest.Start(t, mailtest.StartTLS, "me@gmail.test", "abcdefghijklmnop")
	cfg := baseCfg(s, "tls") // legacy stored name for STARTTLS
	cfg.Username, cfg.Password = "me@gmail.test", "abcdefghijklmnop"
	err := Send(context.Background(), cfg, Message{To: []string{"client@x.test"}, CC: []string{"cc@x.test"}, BCC: []string{"records@x.test"}, Subject: "Invoice", Text: "Hi"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := s.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	m := msgs[0]
	if !m.TLS || m.Auth != "me@gmail.test" || m.From != "billing@acme.test" {
		t.Errorf("tls=%v auth=%q from=%q", m.TLS, m.Auth, m.From)
	}
	if !reflect.DeepEqual(m.Rcpt, []string{"client@x.test", "cc@x.test", "records@x.test"}) {
		t.Errorf("rcpt = %v", m.Rcpt)
	}
	if strings.Contains(m.Data, "records@x.test") {
		t.Error("BCC visible in message")
	}
}

func TestSendImplicitTLS(t *testing.T) {
	s := mailtest.Start(t, mailtest.Implicit, "u", "p")
	cfg := baseCfg(s, "ssl")
	cfg.Username, cfg.Password = "u", "p"
	if err := Send(context.Background(), cfg, Message{To: []string{"a@x.test"}, Subject: "S", Text: "T"}); err != nil {
		t.Fatal(err)
	}
	if m := s.Messages(); len(m) != 1 || !m[0].TLS || m[0].Auth != "u" {
		t.Fatalf("messages = %+v", m)
	}
}

func TestSendAuthLoginOnly(t *testing.T) {
	s := mailtest.Start(t, mailtest.StartTLS, "u", "p")
	s.OnlyLogin = true
	cfg := baseCfg(s, SecurityStartTLS)
	cfg.Username, cfg.Password = "u", "p"
	if err := Send(context.Background(), cfg, Message{To: []string{"a@x.test"}, Subject: "S", Text: "T"}); err != nil {
		t.Fatal(err)
	}
	if m := s.Messages(); len(m) != 1 || m[0].Mechs != "LOGIN" {
		t.Fatalf("messages = %+v", m)
	}
}

func TestGmailAppPasswordSpacesStripped(t *testing.T) {
	s := mailtest.Start(t, mailtest.StartTLS, "me@gmail.com", "abcdefghijklmnop")
	cfg := baseCfg(s, SecurityStartTLS)
	cfg.Host = "smtp.gmail.com" // name used only for password cleaning / TLS name
	cfg.Username, cfg.Password = "me@gmail.com", "abcd efgh ijkl mnop"
	// Dial the test server by overriding the host after cleaning.
	cfg.Password = CleanPassword(cfg.Host, cfg.Password)
	cfg.Host = s.Host
	if err := Send(context.Background(), cfg, Message{To: []string{"a@x.test"}, Subject: "S", Text: "T"}); err != nil {
		t.Fatal(err)
	}
	if got := CleanPassword("smtp.example.com", "a b"); got != "a b" {
		t.Errorf("non-gmail password changed: %q", got)
	}
}

func TestAuthFailureMapping(t *testing.T) {
	s := mailtest.Start(t, mailtest.StartTLS, "me@gmail.com", "right")
	cfg := baseCfg(s, SecurityStartTLS)
	cfg.Username, cfg.Password = "me@gmail.com", "wrong"
	err := Send(context.Background(), cfg, Message{To: []string{"a@x.test"}, Subject: "S", Text: "T"})
	if Code(err) != "AUTH_FAILED" {
		t.Fatalf("code = %q (%v)", Code(err), err)
	}
	// Gmail hint.
	gerr := friendly("auth", err.(*Error).Err, Config{Host: "smtp.gmail.com", Port: 587})
	if !strings.Contains(gerr.Error(), "App Password") {
		t.Errorf("gmail message = %q", gerr)
	}
}

func TestTLSUntrustedCertificate(t *testing.T) {
	s := mailtest.Start(t, mailtest.StartTLS, "", "")
	cfg := baseCfg(s, SecurityStartTLS)
	cfg.TLSConfig = nil // system roots don't trust the self-signed cert
	err := Send(context.Background(), cfg, Message{To: []string{"a@x.test"}, Subject: "S", Text: "T"})
	if Code(err) != "TLS" {
		t.Fatalf("code = %q (%v)", Code(err), err)
	}
}

func TestStartTLSRequiredButMissing(t *testing.T) {
	s := mailtest.Start(t, mailtest.Plain, "", "")
	err := Send(context.Background(), baseCfg(s, SecurityStartTLS), Message{To: []string{"a@x.test"}, Subject: "S", Text: "T"})
	if Code(err) != "STARTTLS_UNSUPPORTED" {
		t.Fatalf("code = %q (%v)", Code(err), err)
	}
	// "none" works against a plain relay.
	if err := Send(context.Background(), baseCfg(s, SecurityNone), Message{To: []string{"a@x.test"}, Subject: "S", Text: "T"}); err != nil {
		t.Fatal(err)
	}
}

func TestInsecureAuthRefused(t *testing.T) {
	s := mailtest.StartAt(t, "127.0.0.2", mailtest.Plain, "", "")
	cfg := baseCfg(s, SecurityNone) // host 127.0.0.2 is not "local": passwords must not go in clear
	cfg.Username, cfg.Password = "u", "p"
	err := Send(context.Background(), cfg, Message{To: []string{"a@x.test"}, Subject: "S", Text: "T"})
	if c := Code(err); c != "INSECURE_AUTH" {
		t.Fatalf("code = %q (%v)", c, err)
	}
}

func TestConnectionRefusedAndRecipientRejected(t *testing.T) {
	err := Send(context.Background(), Config{Host: "127.0.0.1", Port: 1, Security: SecurityNone, FromEmail: "a@x.test"}, Message{To: []string{"b@x.test"}, Text: "x"})
	if Code(err) != "CONNECTION_REFUSED" {
		t.Errorf("code = %q (%v)", Code(err), err)
	}
	s := mailtest.Start(t, mailtest.Plain, "", "")
	err = Send(context.Background(), baseCfg(s, SecurityNone), Message{To: []string{"reject@x.test"}, Text: "x"})
	if Code(err) != "RECIPIENT_REJECTED" {
		t.Errorf("code = %q (%v)", Code(err), err)
	}
}

func TestSendNotConfigured(t *testing.T) {
	if err := Send(context.Background(), Config{}, Message{To: []string{"a@x.test"}}); Code(err) != "NOT_CONFIGURED" {
		t.Errorf("err = %v", err)
	}
}

func TestNormalizeAndParse(t *testing.T) {
	cases := []struct {
		sec  string
		port int
		want string
	}{{"tls", 587, SecurityStartTLS}, {"", 587, SecurityStartTLS}, {"tls", 465, SecuritySSL}, {"ssl", 465, SecuritySSL}, {"none", 25, SecurityNone}, {"auto", 25, SecurityAuto}}
	for _, c := range cases {
		if got := NormalizeSecurity(c.sec, c.port); got != c.want {
			t.Errorf("NormalizeSecurity(%q,%d) = %q, want %q", c.sec, c.port, got, c.want)
		}
	}
	v, bad := ParseList("A@x.test, b@x.test;a@x.test\nnope")
	if !reflect.DeepEqual(v, []string{"a@x.test", "b@x.test"}) || !reflect.DeepEqual(bad, []string{"nope"}) {
		t.Errorf("ParseList = %v %v", v, bad)
	}
}
