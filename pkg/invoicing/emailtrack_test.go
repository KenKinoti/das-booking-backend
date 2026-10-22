package invoicing

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

var (
	pixelRe = regexp.MustCompile(`/api/v1/public/email-open/([A-Za-z0-9_-]{32})\.gif`)
	rTokRe  = regexp.MustCompile(`[?&]r=([A-Za-z0-9_-]{32})`)
)

const (
	appleProxyUA = "Mozilla/5.0"
	desktopUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36"
	gmailUA      = "Mozilla/5.0 (Windows NT 5.1; rv:11.0) Gecko Firefox/11.0 (via ggpht.com GoogleImageProxy)"
	appleMailUA  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko)"
)

// trackEnv is setupMailEnv with a controllable clock and an API origin for
// the open image.
type trackEnv struct {
	mailEnv
	clock *time.Time
}

func setupTrackEnv(t *testing.T) trackEnv {
	t.Helper()
	t.Setenv("PUBLIC_API_URL", "https://api.example.com")
	e := setupMailEnv(t)
	if sqlDB, err := e.db.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() }) // don't hold connections on the shared server
	}
	now := time.Now()
	clock := &now
	e.h.Now = func() time.Time { return *clock }
	return trackEnv{e, clock}
}

func (e trackEnv) advance(d time.Duration) { *e.clock = e.clock.Add(d) }

// get fetches a public URL path with a user agent.
func (e trackEnv) get(path, ua string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	req.RemoteAddr = "203.0.113.57:5000"
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func (e trackEnv) pixel(tok, ua string) *httptest.ResponseRecorder {
	return e.get("/public/email-open/"+tok+".gif", ua)
}

func (e trackEnv) status(t *testing.T, id string) map[string]interface{} {
	t.Helper()
	_, res := call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	st, _ := data(res)["email_status"].(map[string]interface{})
	return st
}

func recipientsOf(st map[string]interface{}) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	list, _ := st["recipients"].([]interface{})
	for _, x := range list {
		r := x.(map[string]interface{})
		out[r["email"].(string)] = r
	}
	return out
}

func tokenIn(t *testing.T, m mail.Message) (pixel, link string) {
	t.Helper()
	if p := pixelRe.FindStringSubmatch(m.HTML); p != nil {
		pixel = p[1]
	}
	if l := rTokRe.FindStringSubmatch(m.HTML); l != nil {
		link = l[1]
	}
	return
}

// sendTo creates a sent invoice for To + CCs and emails it.
func (e trackEnv) sendTo(t *testing.T, to string, cc, bcc []string) (string, []mail.Message) {
	t.Helper()
	inv := newInvoice(t, e.mailEnv, "KES", 2500, map[string]interface{}{"client_email": to, "cc_emails": cc})
	id := inv["id"].(string)
	*e.sent = nil
	body := map[string]interface{}{"mode": "email", "subject": "Invoice {{number}}", "message": "Hi,\n\nView it online: {{link}}\n\nThanks"}
	if bcc != nil {
		body["bcc"] = bcc
	}
	if code, res := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/send", body); code != 200 {
		t.Fatalf("send: %d %v", code, res)
	}
	return id, append([]mail.Message{}, *e.sent...)
}

// One copy per person, each addressed only to them, with its own token in
// the open image and in every link (message text and buttons), an "Also sent
// to" line, and an untracked BCC copy.
func TestTrackedCopiesPerRecipient(t *testing.T) {
	e := setupTrackEnv(t)
	id, sent := e.sendTo(t, "amina@client.test", []string{"brian@client.test", "carol@client.test"}, []string{"records@shop.test"})
	if len(sent) != 4 {
		t.Fatalf("expected 3 personal copies + 1 BCC copy, got %d", len(sent))
	}
	people := []string{"amina@client.test", "brian@client.test", "carol@client.test"}
	cfg := mail.Config{Host: "smtp.x.test", Port: 587, FromEmail: "shop@x.test", FromName: "Shop"}
	seen := map[string]bool{}
	ids := map[string]bool{}
	for i, p := range people {
		m := sent[i]
		if len(m.To) != 1 || m.To[0] != p || len(m.CC) != 0 || len(m.BCC) != 0 {
			t.Fatalf("copy %d: to=%v cc=%v bcc=%v", i, m.To, m.CC, m.BCC)
		}
		env, err := mail.Prepare(m)
		if err != nil {
			t.Fatal(err)
		}
		raw := string(mail.Build(cfg, m, env, time.Now()))
		hdr := raw[:strings.Index(raw, "\r\n\r\n")]
		if !strings.Contains(hdr, "\r\nTo: "+p+"\r\n") || strings.Contains(hdr, "\r\nCc:") || strings.Contains(hdr, "Bcc") || !strings.Contains(hdr, "Message-ID: <"+m.MessageID+">") {
			t.Fatalf("copy %d headers:\n%s", i, hdr)
		}
		if ids[m.MessageID] {
			t.Fatalf("Message-ID reused: %s", m.MessageID)
		}
		ids[m.MessageID] = true
		others := []string{}
		for _, o := range people {
			if o != p {
				others = append(others, o)
			}
		}
		line := "Also sent to: " + strings.Join(others, ", ")
		if !strings.Contains(m.Text, line) || !strings.Contains(m.HTML, line) {
			t.Fatalf("copy %d lacks %q", i, line)
		}
		px, link := tokenIn(t, m)
		if px == "" || px != link || !validRecipientToken(px) {
			t.Fatalf("copy %d tokens: pixel %q link %q", i, px, link)
		}
		if seen[px] {
			t.Fatalf("token reused across copies")
		}
		seen[px] = true
		// Every link to the document carries this copy's token — the {{link}}
		// in the message text, the buttons and the text part.
		if n := strings.Count(m.Text, "?r="+px); n < 1 {
			t.Fatalf("copy %d text links: %d\n%s", i, n, m.Text)
		}
		for _, u := range regexp.MustCompile(`https://app\.example\.com/(?:i|pay)/[0-9a-f]+[^"\s<]*`).FindAllString(m.HTML+m.Text, -1) {
			if !strings.Contains(u, "r="+px) {
				t.Fatalf("copy %d has a link without its token: %s", i, u)
			}
		}
		for other := range seen {
			if other != px && strings.Contains(m.HTML+m.Text, other) {
				t.Fatalf("copy %d contains another copy's token", i)
			}
		}
	}
	bcc := sent[3]
	if len(bcc.To) != 0 || len(bcc.BCC) != 1 || bcc.BCC[0] != "records@shop.test" || strings.Contains(bcc.HTML, "email-open/") ||
		!strings.Contains(bcc.Text, "Copy of the email sent to amina@client.test, brian@client.test, carol@client.test (BCC — this copy isn't tracked).") {
		t.Fatalf("bcc copy: to=%v bcc=%v\n%s", bcc.To, bcc.BCC, bcc.Text)
	}
	env, _ := mail.Prepare(bcc)
	raw := string(mail.Build(cfg, bcc, env, time.Now()))
	if !strings.Contains(raw, "To: undisclosed-recipients:;") || strings.Contains(raw, "records@shop.test") {
		t.Fatalf("bcc copy must not show its address:\n%s", raw[:300])
	}
	// Status: 3 people, nobody opened yet; the BCC row is marked untracked.
	st := e.status(t, id)
	if st["tracking"] != TrackPerRecipient || num(st["people"]) != 3 || num(st["opened"]) != 0 || st["summary"] != "Opened by 0 of 3" {
		t.Fatalf("status %v", st)
	}
	rs := recipientsOf(st)
	if rs["records@shop.test"]["tracked"] != false || rs["records@shop.test"]["state"] != "untracked" || rs["amina@client.test"]["state"] != "sent" || rs["amina@client.test"]["role"] != "to" || rs["carol@client.test"]["role"] != "cc" {
		t.Fatalf("recipients %v", rs)
	}
	// The email log lists every copy under its send.
	_, res := call(t, e.r, e.org, "GET", "/api/invoices/"+id+"/email-log", nil)
	lg := data(res)["log"].([]interface{})[0].(map[string]interface{})
	if lg["tracking"] != TrackPerRecipient || num(lg["copies"]) != 4 || len(lg["recipients"].([]interface{})) != 4 || data(res)["tracking"] == nil {
		t.Fatalf("email log %v", lg)
	}
}

// Tokens are 24 random bytes (URL-safe base64, 32 characters) and never repeat.
func TestRecipientTokens(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 5000; i++ {
		tok := newRecipientToken()
		if !validRecipientToken(tok) || seen[tok] {
			t.Fatalf("bad or repeated token %q", tok)
		}
		seen[tok] = true
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("a", 31) + "/", strings.Repeat("a", 31) + "."} {
		if validRecipientToken(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
	if got := tokeniseLinks("See https://a.test/i/abc. Or https://a.test/i/abc?x=1 and https://a.test/i/abcdef", []string{"https://a.test/i/abc"}, "T"); got != "See https://a.test/i/abc?r=T. Or https://a.test/i/abc?r=T&x=1 and https://a.test/i/abcdef" {
		t.Fatalf("tokeniseLinks: %s", got)
	}
}

// The open image: a no-store GIF for any token; opens counted once per fetch
// burst, first/last kept, user agent and network only; fast.
func TestPixelRecordsOpens(t *testing.T) {
	e := setupTrackEnv(t)
	id, sent := e.sendTo(t, "amina@client.test", []string{"brian@client.test"}, nil)
	tokA, _ := tokenIn(t, sent[0])
	e.advance(5 * time.Minute)

	w := e.pixel(tokA, desktopUA)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/gif" || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") || !bytes.Equal(w.Body.Bytes(), pixelGIF) || len(pixelGIF) != 43 {
		t.Fatalf("pixel response %d %v %d bytes", w.Code, w.Header(), w.Body.Len())
	}
	// Unknown / malformed tokens: the same image, nothing recorded.
	for _, tok := range []string{newRecipientToken(), "nope", "%2e%2e"} {
		if w := e.pixel(tok, desktopUA); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pixelGIF) {
			t.Fatalf("unknown token %q: %d", tok, w.Code)
		}
	}
	first := *e.clock
	e.advance(time.Second)
	e.pixel(tokA, desktopUA) // the same client fetching twice — counted once
	var r EmailRecipient
	e.db.Where("token = ?", tokA).Take(&r)
	same := func(a *time.Time, b time.Time) bool { return a != nil && a.Sub(b).Abs() < time.Millisecond }
	if r.OpenCount != 1 || !same(r.FirstOpenedAt, first) || r.LastIPClass != "203.0.113.0/24" || r.LastUserAgent != desktopUA {
		t.Fatalf("after first open: %+v", r)
	}
	if strings.Contains(r.LastIPClass, "57") {
		t.Fatalf("full IP stored: %s", r.LastIPClass)
	}
	e.advance(10 * time.Minute)
	e.pixel(tokA, desktopUA)
	e.advance(time.Hour)
	e.pixel(tokA, gmailUA) // Gmail's image proxy is a real open
	e.db.Where("token = ?", tokA).Take(&r)
	if r.OpenCount != 3 || r.AutoOpenCount != 0 || !same(r.FirstOpenedAt, first) || !same(r.LastOpenedAt, *e.clock) || r.OpenVia != "gmail" {
		t.Fatalf("after three opens: %+v", r)
	}
	st := e.status(t, id)
	ra := recipientsOf(st)["amina@client.test"]
	if ra["state"] != "opened" || num(ra["opens"]) != 3 || st["summary"] != "Opened by 1 of 2" || !strings.Contains(ra["timeline"].(string), "(3×)") {
		t.Fatalf("status %v / %v", st, ra)
	}
	// Fast: recording is three indexed queries.
	start := time.Now()
	for i := 0; i < 40; i++ {
		e.advance(time.Minute)
		e.pixel(tokA, desktopUA)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("40 opens took %v", d)
	}
}

func TestAutomaticOpenHeuristic(t *testing.T) {
	sent := time.Now()
	at := func(d time.Duration) time.Time { return sent.Add(d) }
	cases := []struct {
		ua, ip string
		after  time.Duration
		auto   bool
		via    string
	}{
		{appleProxyUA, "104.28.1.1", time.Hour, true, "apple_proxy"}, // Apple Mail Privacy Protection
		{desktopUA, "17.58.1.9", time.Hour, true, "apple_proxy"},     // Apple's network
		{gmailUA, "66.249.1.1", 2 * time.Second, false, "gmail"},     // Gmail proxy = a person opening it
		{"YahooMailProxy; https://help.yahoo.com", "1.2.3.4", time.Hour, false, "yahoo"},
		{"Mozilla/5.0 (compatible; Barracuda Sentinel)", "1.2.3.4", time.Hour, true, "scanner"},
		{"python-requests/2.31", "1.2.3.4", time.Hour, true, "scanner"},
		{"", "1.2.3.4", time.Hour, true, "scanner"},
		{appleMailUA, "1.2.3.4", 4 * time.Second, true, "prefetch"}, // preloaded on delivery
		{appleMailUA, "1.2.3.4", 3 * time.Minute, false, "apple_mail"},
		{desktopUA, "1.2.3.4", 4 * time.Second, false, ""}, // a real browser, even if quick
		{"Microsoft Office/16.0 (Windows NT 10.0; Microsoft Outlook 16.0.17126; Pro)", "1.2.3.4", time.Hour, false, ""},
	}
	for _, c := range cases {
		auto, via := classifyOpen(c.ua, ipClass(c.ip), &sent, at(c.after))
		if auto != c.auto || via != c.via {
			t.Errorf("%q from %s after %v: auto=%v via=%q, want %v %q", c.ua, c.ip, c.after, auto, via, c.auto, c.via)
		}
	}
	if ipClass("2001:db8:1234:5678::1") != "2001:db8:1234::/48" || ipClass("bad") != "" {
		t.Errorf("ipClass v6 = %s", ipClass("2001:db8:1234:5678::1"))
	}

	// End to end: an Apple proxy open alone is "opened (automatic)" and not
	// counted in "Opened by"; a later real open is.
	e := setupTrackEnv(t)
	id, msgs := e.sendTo(t, "amina@client.test", []string{"brian@client.test"}, nil)
	tokB, _ := tokenIn(t, msgs[1])
	e.advance(2 * time.Second)
	e.pixel(tokB, appleMailUA) // within 10 s, a proxy-like client
	e.advance(time.Minute)
	e.pixel(tokB, appleProxyUA)
	st := e.status(t, id)
	rb := recipientsOf(st)["brian@client.test"]
	if rb["state"] != "opened_auto" || rb["opened_automatic"] != true || num(rb["auto_opens"]) != 2 || num(st["opened"]) != 0 || num(st["auto_opened"]) != 1 ||
		!strings.Contains(rb["timeline"].(string), "(automatic, may not be a person)") || st["summary"] != "Opened by 0 of 2 · 1 automatic open" {
		t.Fatalf("automatic: %v / %v", st["summary"], rb)
	}
	e.advance(time.Hour)
	e.pixel(tokB, desktopUA)
	rb = recipientsOf(e.status(t, id))["brian@client.test"]
	if rb["state"] != "opened" || rb["opened_automatic"] != false || num(rb["opens"]) != 1 || num(rb["auto_opens"]) != 2 {
		t.Fatalf("after a real open: %v", rb)
	}
}

// Using a recipient's own link records who viewed (and paid); links without a
// token, older emails and other documents' tokens keep working as before.
func TestViewedPerRecipientAndLegacyLinks(t *testing.T) {
	e := setupTrackEnv(t)
	id, msgs := e.sendTo(t, "amina@client.test", []string{"brian@client.test", "carol@client.test"}, []string{"records@shop.test"})
	_, res := call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	pub := data(res)["public_token"].(string)
	tokA, _ := tokenIn(t, msgs[0])
	tokB, _ := tokenIn(t, msgs[1])
	_, tokBCC := tokenIn(t, msgs[3])
	if tokBCC == "" {
		t.Fatal("bcc copy has no link token")
	}
	e.advance(time.Minute)

	// The BCC copy's link isn't the client.
	if w := e.get("/public/invoices/"+pub+"?r="+tokBCC, desktopUA); w.Code != 200 {
		t.Fatalf("bcc view %d", w.Code)
	}
	if st := e.status(t, id); st["viewed_at"] != nil {
		t.Fatalf("a BCC copy's view counted as the client's: %v", st["viewed_at"])
	}
	// B clicks View invoice.
	e.advance(time.Minute)
	if w := e.get("/public/invoices/"+pub+"?r="+tokB, desktopUA); w.Code != 200 || strings.Contains(w.Body.String(), tokB) {
		t.Fatalf("view %d", w.Code)
	}
	e.get("/public/invoices/"+pub+"?r="+tokB, desktopUA)
	st := e.status(t, id)
	rb := recipientsOf(st)["brian@client.test"]
	if rb["state"] != "viewed" || num(rb["views"]) != 2 || num(st["viewed"]) != 1 || num(st["opened"]) != 1 || st["viewed_at"] == nil ||
		st["summary"] != "Opened by 1 of 3 · viewed by 1" || !strings.Contains(rb["timeline"].(string), "Viewed invoice") {
		t.Fatalf("after B's view: %v / %v", st["summary"], rb)
	}
	_, res = call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	if acts := data(res)["activities"].([]interface{}); acts[0].(map[string]interface{})["message"] != "Viewed by brian@client.test" {
		t.Fatalf("activity %v", acts[0])
	}
	// A's token on another invoice's link does nothing; the page still loads.
	otherID, _ := e.sendTo(t, "zed@client.test", nil, nil)
	_, res = call(t, e.r, e.org, "GET", "/api/invoices/"+otherID, nil)
	otherPub := data(res)["public_token"].(string)
	e.advance(time.Minute)
	if w := e.get("/public/invoices/"+otherPub+"?r="+tokA, desktopUA); w.Code != 200 {
		t.Fatalf("foreign token %d", w.Code)
	}
	if ra := recipientsOf(e.status(t, id))["amina@client.test"]; num(ra["views"]) != 0 {
		t.Fatalf("token used on another document was recorded: %v", ra)
	}
	// Legacy link (no token, or a garbage one): works, the first view is
	// "Viewed by client".
	e.advance(time.Minute)
	// (The foreign token above already counted as a plain link.)
	if w := e.get("/public/invoices/"+otherPub+"?r=garbage", desktopUA); w.Code != 200 {
		t.Fatalf("legacy %d", w.Code)
	}
	_, res = call(t, e.r, e.org, "GET", "/api/invoices/"+otherID, nil)
	if d := data(res); d["viewed_at"] == nil || d["activities"].([]interface{})[0].(map[string]interface{})["message"] != "Viewed by client" || strings.Count(fmtActs(d["activities"]), "Viewed by") != 1 {
		t.Fatalf("legacy view: %v", fmtActs(d["activities"]))
	}

	// C opens the pay page from their link and starts a checkout that
	// succeeds: "paid via link" (sendBeacon posts text/plain).
	e.advance(time.Minute)
	beacon := func(body string) int {
		req := httptest.NewRequest("POST", "/public/invoices/"+pub+"/track", strings.NewReader(body))
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		w := httptest.NewRecorder()
		e.r.ServeHTTP(w, req)
		return w.Code
	}
	tokC, _ := tokenIn(t, msgs[2])
	ref := "DAS-TEST-" + uuid.NewString()[:8]
	if beacon(`{"r":"`+tokC+`","event":"pay_page"}`) != 204 || beacon(`{"r":"`+tokC+`","event":"checkout","tx_ref":"`+ref+`"}`) != 204 || beacon(`not json`) != 204 {
		t.Fatal("track endpoint should always answer 204")
	}
	rc := recipientsOf(e.status(t, id))["carol@client.test"]
	if rc["pay_page_at"] == nil || rc["state"] != "viewed" || !strings.Contains(rc["timeline"].(string), "Opened the pay page") {
		t.Fatalf("pay page: %v", rc)
	}
	ensurePayTransactions(t, e)
	if err := e.db.Exec(`INSERT INTO pay_transactions (id, organization_id, invoice_id, tx_ref, status, amount, currency, completed_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		uuid.NewString(), e.org, id, ref, "successful", 2500, "KES", *e.clock, *e.clock, *e.clock).Error; err != nil {
		t.Fatal(err)
	}
	st = e.status(t, id)
	rc = recipientsOf(st)["carol@client.test"]
	if rc["state"] != "paid" || num(rc["paid_amount"]) != 2500 || st["paid_via"] != "carol@client.test" || !strings.Contains(rc["timeline"].(string), "Paid via link") {
		t.Fatalf("paid via link: %v / %v", st["paid_via"], rc)
	}
	if st["summary"] != "Opened by 2 of 3 · viewed by 2 · paid via carol@client.test’s link" {
		t.Fatalf("summary %q", st["summary"])
	}
}

// pay_transactions belongs to pkg/payments (which imports this package):
// create the columns the attribution reads when the table isn't there.
func ensurePayTransactions(t *testing.T, e trackEnv) {
	t.Helper()
	if e.db.Migrator().HasTable("pay_transactions") {
		return
	}
	if err := e.db.Exec(`CREATE TABLE pay_transactions (id varchar(64) PRIMARY KEY, organization_id varchar(255) NOT NULL, invoice_id varchar(64) NOT NULL,
		tx_ref varchar(100) NOT NULL UNIQUE, status varchar(20), amount numeric, currency varchar(3), completed_at timestamptz, created_at timestamptz, updated_at timestamptz)`).Error; err != nil {
		t.Fatal(err)
	}
}

// Quotes: accepting through a recipient's link credits that person.
func TestQuoteAcceptedViaLink(t *testing.T) {
	e := setupTrackEnv(t)
	code, res := call(t, e.r, e.org, "POST", "/api/invoices", map[string]interface{}{"doc_type": "quote", "client_name": "Amina", "client_email": "amina@client.test",
		"cc_emails": []string{"brian@client.test"}, "currency": "KES", "items": []map[string]interface{}{{"description": "Design", "quantity": 1, "unit_price": 900, "tax_rate": 0}}})
	if code != 201 {
		t.Fatalf("quote %d %v", code, res)
	}
	id := data(res)["id"].(string)
	*e.sent = nil
	if code, res := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email"}); code != 200 {
		t.Fatalf("send quote %d %v", code, res)
	}
	msgs := *e.sent
	if len(msgs) != 2 || !strings.Contains(msgs[1].HTML, "View &amp; accept quote") {
		t.Fatalf("quote copies %d", len(msgs))
	}
	tokB, _ := tokenIn(t, msgs[1])
	_, res = call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	pub := data(res)["public_token"].(string)
	e.advance(time.Minute)
	req := httptest.NewRequest("POST", "/public/invoices/"+pub+"/accept?r="+tokB, nil)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("accept %d %s", w.Code, w.Body.String())
	}
	_, res = call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	d := data(res)
	rb := recipientsOf(d["email_status"].(map[string]interface{}))["brian@client.test"]
	if d["status"] != "accepted" || rb["accepted_at"] == nil || rb["state"] != "viewed" || d["activities"].([]interface{})[0].(map[string]interface{})["message"] != "Accepted online by brian@client.test" {
		t.Fatalf("accepted via link: %v / %v", d["status"], rb)
	}
}

// Organisation settings: per-recipient off = one message as before; open
// tracking off = own copies and links, no image (and old images stop
// counting); the footer notice only when chosen.
func TestTrackingSettings(t *testing.T) {
	e := setupTrackEnv(t)
	_, res := call(t, e.r, e.org, "GET", "/api/email-tracking", nil)
	if d := data(res); d["track_recipients"] != true || d["track_opens"] != true || d["open_notice"] != false {
		t.Fatalf("defaults %v", d)
	}
	// Footer notice on.
	call(t, e.r, e.org, "PUT", "/api/email-tracking", map[string]interface{}{"open_notice": true})
	_, msgs := e.sendTo(t, "amina@client.test", []string{"brian@client.test"}, nil)
	if !strings.Contains(msgs[0].HTML, esc(OpenNoticeText)) || !strings.Contains(msgs[0].Text, OpenNoticeText) {
		t.Fatal("footer notice missing")
	}
	oldTok, _ := tokenIn(t, msgs[0])

	// Open tracking off: own copies with link tokens, no image, no notice.
	if code, res := call(t, e.r, e.org, "PUT", "/api/email-tracking", map[string]interface{}{"track_opens": false}); code != 200 || data(res)["track_opens"] != false || data(res)["open_notice"] != true {
		t.Fatalf("put %d %v", code, res)
	}
	id, msgs := e.sendTo(t, "amina@client.test", []string{"brian@client.test"}, nil)
	px, link := tokenIn(t, msgs[0])
	if len(msgs) != 2 || px != "" || link == "" || strings.Contains(msgs[0].HTML, esc(OpenNoticeText)) {
		t.Fatalf("opens off: %d copies, pixel %q link %q", len(msgs), px, link)
	}
	if st := e.status(t, id); st["open_tracking"] != false || st["tracking"] != TrackPerRecipient {
		t.Fatalf("status %v", st)
	}
	// An image in an email sent while tracking was on no longer counts.
	e.advance(time.Hour)
	e.pixel(oldTok, desktopUA)
	var r EmailRecipient
	e.db.Where("token = ?", oldTok).Take(&r)
	if r.OpenCount != 0 {
		t.Fatalf("open recorded with tracking off")
	}

	// Per-recipient off: one message to everyone, as before.
	call(t, e.r, e.org, "PUT", "/api/email-tracking", map[string]interface{}{"track_recipients": false, "track_opens": true})
	id, msgs = e.sendTo(t, "amina@client.test", []string{"brian@client.test", "carol@client.test"}, []string{"records@shop.test"})
	m := msgs[0]
	if len(msgs) != 1 || m.To[0] != "amina@client.test" || strings.Join(m.CC, ",") != "brian@client.test,carol@client.test" || strings.Join(m.BCC, ",") != "records@shop.test" ||
		strings.Contains(m.HTML, "email-open/") || strings.Contains(m.HTML, "?r=") || strings.Contains(m.Text, "Also sent to") {
		t.Fatalf("single message: %d to=%v cc=%v", len(msgs), m.To, m.CC)
	}
	st := e.status(t, id)
	if st["tracking"] != TrackSingle || st["summary"] != "" || recipientsOf(st)["brian@client.test"]["state"] != "untracked" {
		t.Fatalf("single status %v", st)
	}
	_, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/preflight", map[string]interface{}{"mode": "email"})
	if !strings.Contains(fmtChecks(data(res)["checks"]), "Sent as one message") {
		t.Fatalf("preflight checks %v", data(res)["checks"])
	}
}

func fmtActs(v interface{}) string {
	var b strings.Builder
	list, _ := v.([]interface{})
	for _, a := range list {
		b.WriteString(a.(map[string]interface{})["message"].(string) + "; ")
	}
	return b.String()
}

func fmtChecks(v interface{}) string {
	var b strings.Builder
	list, _ := v.([]interface{})
	for _, c := range list {
		m := c.(map[string]interface{})
		b.WriteString(m["id"].(string) + ":" + m["title"].(string) + ";")
	}
	return b.String()
}

// The send dialog preview is the To person's copy: "Also sent to", no token.
func TestPreviewShowsAlsoSentTo(t *testing.T) {
	e := setupTrackEnv(t)
	inv := newInvoice(t, e.mailEnv, "KES", 700, map[string]interface{}{"client_email": "amina@client.test", "cc_emails": []string{"brian@client.test"}})
	code, res := call(t, e.r, e.org, "POST", "/api/invoices/"+inv["id"].(string)+"/preflight", map[string]interface{}{"mode": "email"})
	p := data(res)["preview"].(map[string]interface{})
	if code != 200 || !strings.Contains(p["html"].(string), "Also sent to: brian@client.test") || strings.Contains(p["html"].(string), "email-open/") || strings.Contains(p["html"].(string), "?r=") {
		t.Fatalf("preview %d", code)
	}
	if !strings.Contains(fmtChecks(data(res)["checks"]), "tracking:2 separate copies") {
		t.Fatalf("checks %v", fmtChecks(data(res)["checks"]))
	}
}

// One refused copy doesn't fail the send; all refused does.
func TestPartialDeliveryFailure(t *testing.T) {
	e := setupTrackEnv(t)
	sent := e.sent
	e.h.Mail.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error {
		if len(m.To) == 1 && strings.HasPrefix(m.To[0], "bad") {
			return &mail.Error{Code: "RECIPIENT_REJECTED", Msg: "550 5.1.1 mailbox unavailable"}
		}
		*sent = append(*sent, m)
		return nil
	}
	id, msgs := e.sendTo(t, "amina@client.test", []string{"bad@client.test", "carol@client.test"}, nil)
	if len(msgs) != 2 {
		t.Fatalf("delivered %d", len(msgs))
	}
	st := e.status(t, id)
	rs := recipientsOf(st)
	if st["state"] != "sent" || rs["bad@client.test"]["state"] != "failed" || !strings.Contains(rs["bad@client.test"]["error"].(string), "550") || rs["carol@client.test"]["state"] != "sent" {
		t.Fatalf("partial: %v / %v", st["state"], rs)
	}
	_, res := call(t, e.r, e.org, "GET", "/api/invoices/"+id+"/email-log", nil)
	lg := data(res)["log"].([]interface{})[0].(map[string]interface{})
	if lg["status"] != "sent" || lg["error_code"] != "PARTIAL" || num(lg["copies_failed"]) != 1 {
		t.Fatalf("log %v", lg)
	}
	// Everybody refused: the send fails like before.
	e.h.Mail.Transport = func(context.Context, mail.Config, mail.Message) error { return errors.New("421 try later") }
	inv := newInvoice(t, e.mailEnv, "KES", 100, map[string]interface{}{"client_email": "amina@client.test", "cc_emails": []string{"brian@client.test"}})
	if code, _ := call(t, e.r, e.org, "POST", "/api/invoices/"+inv["id"].(string)+"/send", map[string]interface{}{"mode": "email"}); code != http.StatusBadGateway {
		t.Fatalf("all refused should be 502, got %d", code)
	}
	if st := e.status(t, inv["id"].(string)); st["state"] != "failed" || recipientsOf(st)["brian@client.test"]["state"] != "failed" {
		t.Fatalf("all failed: %v", st)
	}
}

// More than maxTrackedRecipients people (or the setting off) → one message;
// "Also sent to" never lists the reader.
func TestTracksSeparatelyAndAlsoSentTo(t *testing.T) {
	on := TrackingPrefs{TrackRecipients: true, TrackOpens: true}
	b := &builtEmail{To: "a@x.test", CC: []string{"b@x.test"}}
	if !tracksSeparately(on, b) || tracksSeparately(TrackingPrefs{}, b) || tracksSeparately(on, &builtEmail{}) {
		t.Fatal("tracksSeparately basics")
	}
	many := &builtEmail{To: "a@x.test"}
	for i := 0; i < maxTrackedRecipients; i++ {
		many.CC = append(many.CC, "cc"+strings.Repeat("x", i)+"@x.test")
	}
	if tracksSeparately(on, many) {
		t.Fatalf("%d people should go out as one message", 1+len(many.CC))
	}
	if got := alsoSentLine([]string{"a@x.test", "B@x.test", "c@x.test"}, "b@x.test"); got != "Also sent to: a@x.test, c@x.test" {
		t.Fatalf("also sent: %q", got)
	}
	if alsoSentLine([]string{"a@x.test"}, "a@x.test") != "" {
		t.Fatal("a single recipient has no Also sent to line")
	}
	if PixelURL("", "t") != "" || PixelURL("https://api.x", "tok") != "https://api.x/api/v1/public/email-open/tok.gif" {
		t.Fatal("PixelURL")
	}
}

// The per-person summary follows the last delivered invoice / reminder, not
// the receipt sent after a payment.
func TestSummaryFollowsDocumentEmailNotReceipt(t *testing.T) {
	e := setupTrackEnv(t)
	id, msgs := e.sendTo(t, "amina@client.test", []string{"brian@client.test"}, nil)
	_, res := call(t, e.r, e.org, "GET", "/api/invoices/"+id, nil)
	pub := data(res)["public_token"].(string)
	_, tokB := tokenIn(t, msgs[1])
	e.advance(time.Minute)
	e.get("/public/invoices/"+pub+"?r="+tokB, desktopUA)
	e.advance(time.Minute)
	if code, res := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/payments", map[string]interface{}{"amount": 2500, "method": "cash", "email_receipt": true}); code != 200 {
		t.Fatalf("payment %d %v", code, res)
	}
	st := e.status(t, id)
	if st["last_kind"] != KindReceipt || st["summary_kind"] != KindInvoice || st["summary"] != "Opened by 1 of 2 · viewed by 1" {
		t.Fatalf("summary after the receipt: last %v, summary %v %q", st["last_kind"], st["summary_kind"], st["summary"])
	}
}
