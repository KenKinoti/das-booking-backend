// Package mailtest is an in-process SMTP server for tests: plain, STARTTLS
// or implicit TLS with a self-signed certificate, optional AUTH PLAIN/LOGIN,
// recording every message.
package mailtest

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Mode selects how the server secures connections.
type Mode int

const (
	Plain    Mode = iota // no TLS
	StartTLS             // advertises and supports STARTTLS
	Implicit             // TLS from the first byte (port 465 style)
)

// Message is one received message.
type Message struct {
	From  string
	Rcpt  []string
	Data  string
	Auth  string // user that signed in ("" when none)
	TLS   bool   // whether the transaction ran over TLS
	Mechs string // AUTH mechanism used
}

// Server is a running test SMTP server.
type Server struct {
	Mode      Mode
	User      string // when set, AUTH is advertised and required credentials
	Pass      string
	OnlyLogin bool   // advertise only AUTH LOGIN
	Host      string // 127.0.0.1
	Port      int

	ln   net.Listener
	cert tls.Certificate
	pool *x509.CertPool

	mu   sync.Mutex
	msgs []Message
}

// Start launches a server on 127.0.0.1; it is closed when the test ends.
func Start(t testing.TB, mode Mode, user, pass string) *Server {
	t.Helper()
	return StartAt(t, "127.0.0.1", mode, user, pass)
}

// StartAt launches a server on the given loopback IP.
func StartAt(t testing.TB, ip string, mode Mode, user, pass string) *Server {
	t.Helper()
	s := &Server{Mode: mode, User: user, Pass: pass}
	s.makeCert(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	if mode == Implicit {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{s.cert}})
	}
	s.ln = ln
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	s.Host = host
	s.Port, _ = strconv.Atoi(port)
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

// ClientTLS returns a client TLS config that trusts the server certificate.
func (s *Server) ClientTLS() *tls.Config {
	return &tls.Config{RootCAs: s.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

// Messages returns what has been received so far.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

func (s *Server) makeCert(t testing.TB) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "mailtest"},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	s.pool = x509.NewCertPool()
	s.pool.AddCert(c)
	s.cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (s *Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	isTLS := s.Mode == Implicit
	r := bufio.NewReader(conn)
	w := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
	w("220 mailtest ESMTP ready")
	var cur Message
	authed := ""
	mech := ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			exts := []string{"250-mailtest", "250-SIZE 10485760", "250-8BITMIME"}
			if s.Mode == StartTLS && !isTLS {
				exts = append(exts, "250-STARTTLS")
			}
			if s.User != "" && isTLS {
				if s.OnlyLogin {
					exts = append(exts, "250-AUTH LOGIN")
				} else {
					exts = append(exts, "250-AUTH PLAIN LOGIN")
				}
			}
			exts[len(exts)-1] = "250 " + strings.TrimPrefix(exts[len(exts)-1], "250-")
			for _, e := range exts {
				w(e)
			}
		case strings.HasPrefix(up, "HELO"):
			w("250 mailtest")
		case up == "STARTTLS":
			if s.Mode != StartTLS || isTLS {
				w("502 not supported")
				continue
			}
			w("220 go ahead")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{s.cert}})
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			r = bufio.NewReader(conn)
			isTLS = true
		case strings.HasPrefix(up, "AUTH PLAIN"):
			arg := strings.TrimSpace(cmd[len("AUTH PLAIN"):])
			if arg == "" {
				w("334 ")
				l, _ := r.ReadString('\n')
				arg = strings.TrimSpace(l)
			}
			raw, _ := base64.StdEncoding.DecodeString(arg)
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 && parts[1] == s.User && parts[2] == s.Pass {
				authed, mech = parts[1], "PLAIN"
				w("235 2.7.0 Accepted")
			} else {
				w("535 5.7.8 Username and Password not accepted")
			}
		case strings.HasPrefix(up, "AUTH LOGIN"):
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
			l1, _ := r.ReadString('\n')
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
			l2, _ := r.ReadString('\n')
			u, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(l1))
			p, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(l2))
			if string(u) == s.User && string(p) == s.Pass {
				authed, mech = string(u), "LOGIN"
				w("235 2.7.0 Accepted")
			} else {
				w("535 5.7.8 Username and Password not accepted")
			}
		case strings.HasPrefix(up, "MAIL FROM:"):
			if s.User != "" && authed == "" {
				w("530 5.7.0 Authentication required")
				continue
			}
			cur = Message{From: strings.Trim(strings.TrimSpace(cmd[10:]), "<>"), Auth: authed, TLS: isTLS, Mechs: mech}
			if i := strings.Index(cur.From, ">"); i >= 0 {
				cur.From = cur.From[:i]
			}
			w("250 2.1.0 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			addr := strings.Trim(strings.TrimSpace(cmd[8:]), "<>")
			if strings.HasPrefix(addr, "reject") {
				w("550 5.1.1 no such user")
				continue
			}
			cur.Rcpt = append(cur.Rcpt, addr)
			w("250 2.1.5 ok")
		case up == "DATA":
			w("354 end with .")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				if strings.HasPrefix(l, "..") {
					l = l[1:]
				}
				b.WriteString(l)
			}
			cur.Data = b.String()
			s.mu.Lock()
			s.msgs = append(s.msgs, cur)
			s.mu.Unlock()
			w("250 2.0.0 queued")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}
