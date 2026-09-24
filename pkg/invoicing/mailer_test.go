package invoicing

import (
	"bufio"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestBuildMessageHeaders(t *testing.T) {
	msg := string(BuildMessage("Acme <billing@acme.test>", "client@x.test", []string{"a@x.test", "b@x.test"}, "Invoice INV-1\r\nBcc: evil@x.test", "Hi\nthere"))
	head, body, found := strings.Cut(msg, "\r\n\r\n")
	if !found {
		t.Fatal("no header/body separator")
	}
	want := []string{"From: Acme <billing@acme.test>", "To: client@x.test", "Cc: a@x.test, b@x.test", "Subject: Invoice INV-1  Bcc: evil@x.test"}
	for _, w := range want {
		if !strings.Contains(head, w+"\r\n") {
			t.Errorf("missing header %q in:\n%s", w, head)
		}
	}
	if strings.Contains(head, "\r\nBcc:") {
		t.Error("header injection via subject")
	}
	if body != "Hi\r\nthere" {
		t.Errorf("body not CRLF-normalised: %q", body)
	}
	if strings.Contains(string(BuildMessage("f@x.test", "t@x.test", nil, "s", "b")), "Cc:") {
		t.Error("Cc header must be omitted when there are no CCs")
	}
}

func TestMergeRecipients(t *testing.T) {
	to, cc, skipped := MergeRecipients(" Client@X.test ",
		[]string{"a@x.test", "A@x.test", "client@x.test", "not-an-email", "b@x.test, c@x.test"},
		[]string{"b@x.test", "d@x", "e@x.test"})
	if to != "client@x.test" {
		t.Errorf("to = %q", to)
	}
	if want := []string{"a@x.test", "b@x.test", "c@x.test", "e@x.test"}; !reflect.DeepEqual(cc, want) {
		t.Errorf("cc = %v, want %v", cc, want)
	}
	if want := []string{"not-an-email", "d@x"}; !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}
	to, cc, skipped = MergeRecipients("bad address", nil)
	if to != "" || len(cc) != 0 || len(skipped) != 1 {
		t.Errorf("invalid To should be dropped: %q %v %v", to, cc, skipped)
	}
}

func TestSuggestRecipients(t *testing.T) {
	contacts := []contactRow{
		{Name: "Pat", Email: "Pat@Client.test", IsPrimaryBilling: true, CCInvoices: true},
		{Name: "Acc", Email: "accounts@client.test", CCInvoices: true, CCReminders: true},
		{Name: "Sales", Email: "sales@client.test", CCQuotes: true},
	}
	to, cc := suggest("owner@client.test", contacts, []string{"extra@client.test", "accounts@client.test"}, KindInvoice)
	if to == nil || to.Email != "pat@client.test" || to.Source != "primary_billing" {
		t.Fatalf("primary billing contact should be To: %+v", to)
	}
	got := []string{}
	for _, r := range cc {
		got = append(got, r.Email+"/"+r.Source)
	}
	if want := []string{"extra@client.test/document", "accounts@client.test/contact"}; !reflect.DeepEqual(got, want) {
		t.Errorf("invoice cc = %v, want %v", got, want)
	}
	_, cc = suggest("owner@client.test", contacts, nil, KindQuote)
	if len(cc) != 1 || cc[0].Email != "sales@client.test" {
		t.Errorf("quote cc = %+v", cc)
	}
	to, cc = suggest("owner@client.test", contacts[1:], nil, KindReminder)
	if to.Email != "owner@client.test" || to.Source != "client" || len(cc) != 1 || cc[0].Email != "accounts@client.test" {
		t.Errorf("reminder: to=%+v cc=%+v", to, cc)
	}
}

// fakeSMTP is a minimal SMTP server that records envelopes and data.
type fakeSMTP struct {
	ln    net.Listener
	mu    sync.Mutex
	from  string
	rcpts []string
	data  string
	done  chan struct{}
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, done: make(chan struct{})}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(f.done)
		r := bufio.NewReader(conn)
		w := func(s string) { conn.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.TrimSpace(line)
			up := strings.ToUpper(cmd)
			switch {
			case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
				w("250 fake")
			case strings.HasPrefix(up, "MAIL FROM:"):
				f.mu.Lock()
				f.from = strings.Trim(cmd[10:], "<> ")
				f.mu.Unlock()
				w("250 ok")
			case strings.HasPrefix(up, "RCPT TO:"):
				f.mu.Lock()
				f.rcpts = append(f.rcpts, strings.Trim(cmd[8:], "<> "))
				f.mu.Unlock()
				w("250 ok")
			case up == "DATA":
				w("354 go ahead")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				f.mu.Lock()
				f.data = b.String()
				f.mu.Unlock()
				w("250 queued")
			case up == "QUIT":
				w("221 bye")
				return
			default:
				w("250 ok")
			}
		}
	}()
	return f
}

func TestSMTPMailerSendsCC(t *testing.T) {
	f := startFakeSMTP(t)
	defer f.ln.Close()
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	m := &SMTPMailer{Host: host, Port: port, From: "Acme Billing <billing@acme.test>"}
	if err := m.SendWithCC("client@x.test", []string{"a@x.test", "CLIENT@x.test", "bad", "b@x.test"}, "Invoice INV-1", "Hello"); err != nil {
		t.Fatal(err)
	}
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.from != "billing@acme.test" {
		t.Errorf("MAIL FROM = %q", f.from)
	}
	if want := []string{"client@x.test", "a@x.test", "b@x.test"}; !reflect.DeepEqual(f.rcpts, want) {
		t.Errorf("RCPT TO = %v, want %v", f.rcpts, want)
	}
	if !strings.Contains(f.data, "Cc: a@x.test, b@x.test\r\n") || !strings.Contains(f.data, "To: client@x.test\r\n") {
		t.Errorf("headers wrong:\n%s", f.data)
	}
}
