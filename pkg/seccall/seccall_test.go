package seccall

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---------------------------------------------------------------- helpers

func testDSN(t *testing.T, schema string) string {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	boot.Exec(`CREATE SCHEMA ` + schema)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}

func openDB(t *testing.T, dsn string) *gorm.DB {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), NowFunc: func() time.Time { return time.Now().UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s, err := db.DB(); err == nil {
			s.Close()
		}
	})
	return db
}

func testDB(t *testing.T) *gorm.DB {
	db := openDB(t, testDSN(t, "seccall_test"))
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE users (id varchar(64) primary key, email text, first_name text, last_name text, role text, organization_id varchar(64))`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time            { return c.t }
func (c *clock) add(d time.Duration)       { c.t = c.t.Add(d) }
func newClock() *clock                     { return &clock{t: time.Now().UTC()} }
func svcAt(db *gorm.DB, c *clock) *Service { s := New(db); s.Now = c.now; return s }

// key material as a browser would hold it
type linkKey struct {
	check string // base64url 16 bytes
	gcm   cipher.AEAD
}

func newLink(t *testing.T) linkKey {
	k := make([]byte, 32)
	rand.Read(k)
	c := make([]byte, 16)
	rand.Read(c)
	block, _ := aes.NewCipher(k)
	g, _ := cipher.NewGCM(block)
	return linkKey{check: base64.RawURLEncoding.EncodeToString(c), gcm: g}
}

func (l linkKey) seal(plain string, aad string) string {
	iv := make([]byte, 12)
	rand.Read(iv)
	ct := l.gcm.Seal(nil, iv, []byte(plain), []byte(aad))
	return base64.RawURLEncoding.EncodeToString(iv) + "." + base64.RawURLEncoding.EncodeToString(ct)
}

func (l linkKey) open(s, aad string) (string, error) {
	iv, ct, _ := strings.Cut(s, ".")
	ivb, _ := base64.RawURLEncoding.DecodeString(iv)
	ctb, _ := base64.RawURLEncoding.DecodeString(ct)
	p, err := l.gcm.Open(nil, ivb, ctb, []byte(aad))
	return string(p), err
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "ERR:" + err.Error()
}

func mustCode(t *testing.T, err error, want string) {
	t.Helper()
	if got := code(err); got != want {
		t.Fatalf("want %s, got %s (%v)", want, got, err)
	}
}

const (
	orgA  = "org-a"
	orgB  = "org-b"
	hostA = "user-host-a"
)

func mkRoom(t *testing.T, s *Service, l linkKey, in CreateInput) *RoomView {
	t.Helper()
	in.LinkCheck = l.check
	v, err := s.Create(orgA, hostA, "manager", "Kennedy K", in)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func join(t *testing.T, s *Service, room string, l linkKey, name, ip string) *JoinResult {
	t.Helper()
	r, err := s.JoinGuest(room, JoinInput{Name: name, LinkCheck: l.check}, ip)
	if err != nil {
		t.Fatalf("join %s: %v", name, err)
	}
	return r
}

func pollNow(t *testing.T, s *Service, room, tok string, since int64) *PollResult {
	t.Helper()
	res, err := s.Poll(t.Context(), room, tok, since, time.Millisecond, "203.0.113.9")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return res
}

func events(res *PollResult) []string {
	out := []string{}
	for _, m := range res.Messages {
		if m.Kind == "evt" {
			var e map[string]any
			json.Unmarshal(m.Event, &e)
			out = append(out, e["type"].(string))
		}
	}
	return out
}

// ---------------------------------------------------------------- pure tests

func TestValidateCiphertext(t *testing.T) {
	l := newLink(t)
	good := l.seal(`{"v":1,"n":1,"t":"offer","sdp":"v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\n"}`, "aad")
	if err := ValidateCiphertext(good, 64*1024); err != nil {
		t.Fatalf("real AES-GCM output refused: %v", err)
	}
	sdp := "v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\na=fingerprint:sha-256 AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89\r\n"
	iv := base64.RawURLEncoding.EncodeToString(make([]byte, 12))
	utf16 := make([]byte, 0, 2*len(sdp))
	for _, c := range []byte(sdp) {
		utf16 = append(utf16, c, 0)
	}
	bad := map[string]string{
		"plain SDP":            sdp,
		"JSON":                 `{"type":"offer","sdp":"v=0"}`,
		"no IV":                base64.RawURLEncoding.EncodeToString([]byte(sdp)),
		"base64 of SDP":        iv + "." + base64.RawURLEncoding.EncodeToString([]byte(sdp)),
		"base64 of UTF-16 SDP": iv + "." + base64.RawURLEncoding.EncodeToString(utf16),
		"zeros":                iv + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 200)),
		"short IV":             "AAAA." + strings.Split(good, ".")[1],
		"too short":            iv + "." + base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3}),
		"padded base64":        iv + "." + base64.URLEncoding.EncodeToString([]byte("0123456789abcdefg")),
		"three parts":          good + ".AAAA",
		"candidate":            "candidate:842163049 1 udp 1677729535 198.51.100.7 52114 typ srflx raddr 0.0.0.0 rport 0",
	}
	for name, v := range bad {
		if err := ValidateCiphertext(v, 64*1024); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	big := make([]byte, 70*1024)
	rand.Read(big)
	if err := ValidateCiphertext(iv+"."+base64.RawURLEncoding.EncodeToString(big), 64*1024); err == nil {
		t.Error("oversized message accepted")
	}
}

func TestCoturnCredential(t *testing.T) {
	// coturn use-auth-secret / TURN REST API: password = base64(HMAC-SHA1(secret, "expiry:id"))
	exp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	u, c := CoturnCredential("turn-shared-secret-0123456789", "pA1b2C3d4E5f6G7h", exp)
	if u != "1767225600:pA1b2C3d4E5f6G7h" || c != "iD8RSKS95Ec9fviq4xiui/bXSfY=" {
		t.Fatalf("got %s / %s", u, c)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !l.allow("k", 3, time.Minute, now) {
			t.Fatal("refused within limit")
		}
	}
	if l.allow("k", 3, time.Minute, now.Add(30*time.Second)) {
		t.Fatal("4th allowed")
	}
	if !l.allow("other", 3, time.Minute, now) {
		t.Fatal("keys are not independent")
	}
	if !l.allow("k", 3, time.Minute, now.Add(61*time.Second)) {
		t.Fatal("window did not reset")
	}
}

func TestIDsAndHashes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewRoomID()
		if !ValidRoomID(id) || seen[id] {
			t.Fatalf("bad/duplicate room id %q", id)
		}
		seen[id] = true
	}
	if ValidRoomID("../../etc") || ValidRoomID("") {
		t.Fatal("invalid ids accepted")
	}
	if _, ok := HashCheck("short"); ok {
		t.Fatal("short check accepted")
	}
	h, ok := HashCheck(base64.RawURLEncoding.EncodeToString(make([]byte, 16)))
	sum := sha256.Sum256(make([]byte, 16))
	if !ok || h != hex.EncodeToString(sum[:]) {
		t.Fatal("check hash")
	}
}
