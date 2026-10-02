package insights

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

// ---------- parsing ----------

const rssSample = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel>
  <title>Tech &amp; Business Kenya</title>
  <atom:link href="https://news.example.co.ke/feed/" rel="self" type="application/rss+xml" />
  <link>https://news.example.co.ke</link>
  <description>News</description>
  <image><title>Logo title</title><url>https://news.example.co.ke/logo.png</url><link>https://news.example.co.ke/logo</link></image>
  <item>
    <title><![CDATA[Safaricom cuts M-Pesa fees for <b>small businesses</b>]]></title>
    <link>https://news.example.co.ke/mpesa-fees/?utm_source=rss&amp;utm_medium=feed</link>
    <guid isPermaLink="false">https://news.example.co.ke/?p=101</guid>
    <pubDate>Thu, 01 Oct 2026 09:30:00 EAT</pubDate>
    <media:title>Photo: a phone</media:title>
    <description><![CDATA[<p>Safaricom has cut <a href="https://x.example">M-Pesa</a> fees &amp; charges for SMEs in Nairobi.&nbsp;The move follows a KRA review.</p><script>alert(1)</script><p>The post <a href="https://news.example.co.ke/mpesa-fees/">Safaricom cuts M-Pesa fees</a> appeared first on <a href="https://news.example.co.ke">Tech Kenya</a>.</p>]]></description>
    <content:encoded><![CDATA[<p>FULL ARTICLE BODY THAT MUST NOT BE STORED</p>]]></content:encoded>
  </item>
  <item>
    <title>Hosting prices rise as the shilling weakens</title>
    <link>https://news.example.co.ke/hosting-prices/</link>
    <pubDate>30 Sep 2026 18:05 GMT+0300</pubDate>
    <description>&lt;p&gt;Web hosting and domain renewals priced in dollars cost more. [&amp;#8230;]&lt;/p&gt;</description>
  </item>
  <item>
    <title>Only a guid</title>
    <guid isPermaLink="true">https://news.example.co.ke/only-guid/</guid>
    <dc:date>2026-09-29T07:00:00+03:00</dc:date>
    <content:encoded><![CDATA[<p>Summary taken from content when there is no description.</p>]]></content:encoded>
  </item>
  <item>
    <title>Bad date, relative link</title>
    <link>/relative/path</link>
    <pubDate>sometime last week</pubDate>
    <description>` + "LONGTEXT" + `</description>
  </item>
  <item><description>No title and no link: dropped</description></item>
</channel>
</rss>`

func TestParseRSS(t *testing.T) {
	long := strings.Repeat("Cloud outage report word ", 40)
	f, err := ParseFeed(strings.NewReader(strings.Replace(rssSample, "LONGTEXT", long, 1)), 50)
	if err != nil {
		t.Fatal(err)
	}
	ResolveLinks(f, "https://news.example.co.ke/feed/")
	if f.Kind != "rss" || f.Title != "Tech & Business Kenya" || f.Link != "https://news.example.co.ke" {
		t.Fatalf("channel: %+v", f)
	}
	if len(f.Items) != 4 {
		t.Fatalf("items = %d, want 4: %+v", len(f.Items), f.Items)
	}
	a := f.Items[0]
	if a.Title != "Safaricom cuts M-Pesa fees for small businesses" {
		t.Errorf("title (CDATA + tags, media:title ignored): %q", a.Title)
	}
	want := "Safaricom has cut M-Pesa fees & charges for SMEs in Nairobi. The move follows a KRA review."
	if a.Summary != want {
		t.Errorf("summary = %q\n            want %q", a.Summary, want)
	}
	if strings.Contains(a.Summary, "FULL ARTICLE") || strings.Contains(a.Summary, "alert") || strings.Contains(a.Summary, "appeared first") {
		t.Errorf("summary leaked content / script / boilerplate: %q", a.Summary)
	}
	if a.Published == nil || a.Published.Format(time.RFC3339) != "2026-10-01T06:30:00Z" {
		t.Errorf("EAT date: %v", a.Published)
	}
	if a.GUID != "https://news.example.co.ke/?p=101" {
		t.Errorf("guid %q", a.GUID)
	}
	b := f.Items[1]
	if b.Summary != "Web hosting and domain renewals priced in dollars cost more." { // the trailing "[…]" is dropped
		t.Errorf("escaped HTML summary: %q", b.Summary)
	}
	if b.Published == nil || b.Published.Format(time.RFC3339) != "2026-09-30T15:05:00Z" {
		t.Errorf("GMT+0300 date without seconds: %v", b.Published)
	}
	c := f.Items[2]
	if c.Link != "https://news.example.co.ke/only-guid/" || c.Summary != "Summary taken from content when there is no description." {
		t.Errorf("guid permalink / content fallback: %+v", c)
	}
	if c.Published == nil || c.Published.Format(time.RFC3339) != "2026-09-29T04:00:00Z" {
		t.Errorf("dc:date: %v", c.Published)
	}
	d := f.Items[3]
	if d.Published != nil {
		t.Errorf("unreadable date should be nil, got %v", d.Published)
	}
	if d.Link != "https://news.example.co.ke/relative/path" {
		t.Errorf("relative link: %q", d.Link)
	}
	if n := len([]rune(d.Summary)); n > SummaryMax || !strings.HasSuffix(d.Summary, "…") || strings.HasSuffix(d.Summary, " …") {
		t.Errorf("summary length %d, text %q", n, d.Summary)
	}
}

const atomSample = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title type="text">Workspace Updates</title>
  <link rel="self" href="https://updates.example.com/feeds/posts/default"/>
  <link rel="alternate" type="text/html" href="https://updates.example.com/"/>
  <updated>2026-10-01T10:00:00Z</updated>
  <entry>
    <id>tag:example.com,2026:post-1</id>
    <title type="html">Gemini in Gmail &amp;amp; Docs: new &lt;em&gt;AI&lt;/em&gt; features</title>
    <link rel="replies" type="application/atom+xml" href="https://updates.example.com/feeds/1/comments"/>
    <link rel="alternate" type="text/html" href="https://updates.example.com/2026/10/gemini.html"/>
    <published>2026-10-01T09:00:00.123-07:00</published>
    <updated>2026-10-01T09:30:00Z</updated>
    <summary type="html">&lt;p&gt;Admins can now turn on &lt;b&gt;Gemini&lt;/b&gt; for Google Workspace.&lt;/p&gt;</summary>
    <content type="html">&lt;p&gt;FULL CONTENT&lt;/p&gt;</content>
  </entry>
  <entry>
    <id>tag:example.com,2026:post-2</id>
    <title>XHTML content, no summary</title>
    <link href="/2026/09/xhtml.html"/>
    <updated>2026-09-28T12:00:00Z</updated>
    <content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>Rollout pace: <strong>gradual</strong> over 15 days.</p></div></content>
  </entry>
</feed>`

func TestParseAtom(t *testing.T) {
	f, err := ParseFeed(strings.NewReader(atomSample), 50)
	if err != nil {
		t.Fatal(err)
	}
	ResolveLinks(f, "https://updates.example.com/feeds/posts/default")
	if f.Kind != "atom" || f.Title != "Workspace Updates" || f.Link != "https://updates.example.com/" || len(f.Items) != 2 {
		t.Fatalf("feed %+v", f)
	}
	a := f.Items[0]
	if a.Title != "Gemini in Gmail & Docs: new AI features" {
		t.Errorf("title %q", a.Title)
	}
	if a.Link != "https://updates.example.com/2026/10/gemini.html" {
		t.Errorf("alternate link: %q", a.Link)
	}
	if a.Summary != "Admins can now turn on Gemini for Google Workspace." {
		t.Errorf("summary %q", a.Summary)
	}
	if a.Published == nil || a.Published.Format(time.RFC3339) != "2026-10-01T16:00:00Z" {
		t.Errorf("published %v", a.Published)
	}
	b := f.Items[1]
	if b.Link != "https://updates.example.com/2026/09/xhtml.html" || b.Summary != "Rollout pace: gradual over 15 days." {
		t.Errorf("xhtml entry %+v", b)
	}
	if b.Published == nil || b.Published.Format("2006-01-02") != "2026-09-28" {
		t.Errorf("updated as date: %v", b.Published)
	}
}

func TestParseOddFeeds(t *testing.T) {
	rdf := `<?xml version="1.0"?><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel rdf:about="https://t.example.org/"><title>Trade news</title><link>https://t.example.org/</link></channel>
<item rdf:about="https://t.example.org/a"><title>Services exports rise</title><link>https://t.example.org/a</link><description>Exports of services grew.</description><dc:date>2026-09-30</dc:date></item>
</rdf:RDF>`
	f, err := ParseFeed(strings.NewReader(rdf), 50)
	if err != nil || f.Kind != "rdf" || len(f.Items) != 1 || f.Items[0].Title != "Services exports rise" || f.Items[0].Published == nil {
		t.Fatalf("rdf: %v %+v", err, f)
	}
	if _, err := ParseFeed(strings.NewReader("<!doctype html><html><head><title>Hi</title></head><body><p>page</p></body></html>"), 50); err != ErrNotFeed {
		t.Fatalf("html page: %v", err)
	}
	if _, err := ParseFeed(strings.NewReader(`{"json": true}`), 50); err != ErrNotFeed {
		t.Fatalf("json: %v", err)
	}
	// Latin-1 with a stray HTML entity.
	latin := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss version=\"2.0\"><channel><title>Caf\xe9</title><item><title>Prix &eacute;lev\xe9s</title><link>https://l.example.org/1</link></item></channel></rss>"
	f, err = ParseFeed(strings.NewReader(latin), 50)
	if err != nil || f.Title != "Café" || f.Items[0].Title != "Prix élevés" {
		t.Fatalf("latin-1: %v %+v", err, f)
	}
	// Cut off mid-document (size cap): the complete items are kept.
	b := &strings.Builder{}
	b.WriteString(`<rss version="2.0"><channel><title>Big</title>`)
	for k := 0; k < 80; k++ {
		fmt.Fprintf(b, `<item><title>Item %d</title><link>https://b.example.org/%d</link></item>`, k, k)
	}
	whole := b.String() + `</channel></rss>`
	f, err = ParseFeed(strings.NewReader(whole), 50)
	if err != nil || len(f.Items) != 50 || !f.Truncated {
		t.Fatalf("item cap: %v n=%d truncated=%v", err, len(f.Items), f.Truncated)
	}
	cut := whole[:strings.Index(whole, "Item 7<")]
	f, err = ParseFeed(strings.NewReader(cut), 50)
	if err != nil || len(f.Items) != 7 || !f.Truncated {
		t.Fatalf("cut body: %v n=%d truncated=%v", err, len(f.Items), f.Truncated)
	}
}

func TestParseFeedDate(t *testing.T) {
	cases := map[string]string{
		"Thu, 01 Oct 2026 09:30:00 +0300":         "2026-10-01T06:30:00Z",
		"Thu, 1 Oct 2026 09:30:00 GMT":            "2026-10-01T09:30:00Z",
		"Thu, 01 Oct 2026 09:30:00 EAT":           "2026-10-01T06:30:00Z",
		"Thu, 01 Oct 2026 09:30:00 AEST":          "2026-09-30T23:30:00Z",
		"Thu, 01 Oct 2026 09:30:00 PDT":           "2026-10-01T16:30:00Z",
		"Thu, 01 Oct 2026 09:30 +0000":            "2026-10-01T09:30:00Z",
		"01 Oct 2026 09:30:00 Z":                  "2026-10-01T09:30:00Z",
		"2026-10-01T09:30:00Z":                    "2026-10-01T09:30:00Z",
		"2026-10-01T09:30:00.250+10:00":           "2026-09-30T23:30:00Z",
		"2026-10-01T09:30:00+1000":                "2026-09-30T23:30:00Z",
		"2026-10-01 09:30:00":                     "2026-10-01T09:30:00Z",
		"2026-10-01":                              "2026-10-01T00:00:00Z",
		"October 1, 2026":                         "2026-10-01T00:00:00Z",
		"1st October 2026":                        "2026-10-01T00:00:00Z",
		"  Thu,  01  Oct 2026 09:30:00 UT  ":      "2026-10-01T09:30:00Z",
		"Thursday, 1 October 2026 09:30:00 +0300": "2026-10-01T06:30:00Z",
	}
	for in, want := range cases {
		got, ok := ParseFeedDate(in)
		if !ok || got.Format(time.RFC3339) != want {
			t.Errorf("ParseFeedDate(%q) = %v %v, want %s", in, got.Format(time.RFC3339), ok, want)
		}
	}
	for _, bad := range []string{"", "yesterday", "0001-01-01T00:00:00Z", "Thu, 01 Oct 1970 00:00:00 GMT", "32 Foo 2026"} {
		if _, ok := ParseFeedDate(bad); ok {
			t.Errorf("ParseFeedDate(%q) should fail", bad)
		}
	}
}

func TestPlainTextAndDedupeKeys(t *testing.T) {
	if got := PlainText(`<div>Hello<br/>world &amp; <a href="x">friends</a><style>p{}</style> 5 &lt; 6</div>`); got != "Hello world & friends 5 < 6" {
		t.Errorf("PlainText: %q", got)
	}
	if got := Summarise("Short text. Read more »"); got != "Short text." {
		t.Errorf("read-more: %q", got)
	}
	a := FeedItem{Link: "https://www.Example.com/post/?utm_source=x&id=7&fbclid=abc#top"}
	b := FeedItem{Link: "http://example.com/post?id=7"}
	c := FeedItem{Link: "https://example.com/post?id=8"}
	if ItemHash(a) != ItemHash(b) {
		t.Errorf("same article, different hash: %s vs %s", CanonicalLink(a.Link), CanonicalLink(b.Link))
	}
	if ItemHash(a) == ItemHash(c) {
		t.Error("different articles share a hash")
	}
	if ItemHash(FeedItem{GUID: "g1"}) == ItemHash(FeedItem{GUID: "g2"}) || ItemHash(FeedItem{GUID: "g1"}) != ItemHash(FeedItem{GUID: "g1"}) {
		t.Error("guid hash")
	}
}

// ---------- SSRF guard ----------

func TestSSRFGuard(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.254", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255",
		"192.0.2.1", "198.18.0.1", "::1", "::", "fe80::1", "fc00::1", "fd12:3456::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::7f00:1", "64:ff9b::a00:1", "2002:7f00:1::1", "2001:db8::1"}
	for _, s := range blocked {
		if IsPublicIP(net.ParseIP(s)) {
			t.Errorf("IsPublicIP(%s) = true, want blocked", s)
		}
		if err := dialControl("tcp", net.JoinHostPort(s, "443"), nil); err == nil {
			t.Errorf("dialControl(%s) allowed", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "93.184.216.34", "1.1.1.1", "2606:4700:4700::1111", "64:ff9b::808:808"} {
		if !IsPublicIP(net.ParseIP(s)) {
			t.Errorf("IsPublicIP(%s) = false, want public", s)
		}
	}
	if IsPublicIP(nil) || dialControl("tcp", "not-an-ip:443", nil) == nil {
		t.Error("nil / unparsable address must be refused")
	}

	bad := []string{"http://example.com/feed", "ftp://example.com/feed", "https://127.0.0.1/feed", "https://[::1]/feed", "https://localhost/feed", "https://intranet/feed",
		"https://router.local/feed", "https://db.internal/x", "https://user:pw@example.com/feed", "https://example.com:8443/feed", "https://169.254.169.254/latest/meta-data", "", "https://exa mple.com/"}
	for _, u := range bad {
		if _, err := ValidateFeedURL(u, false); err == nil {
			t.Errorf("ValidateFeedURL(%q) accepted", u)
		}
	}
	for in, want := range map[string]string{"example.com/feed": "https://example.com/feed", "https://Blog.Example.COM": "https://blog.example.com/", "https://example.com:443/rss#frag": "https://example.com/rss"} {
		if got, err := ValidateFeedURL(in, false); err != nil || got != want {
			t.Errorf("ValidateFeedURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	// The guarded client refuses to connect to a local server, by IP and by a
	// name that resolves to loopback (the check runs after DNS resolution).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<rss/>")) }))
	defer srv.Close()
	cl := newFeedClient(true)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	for _, u := range []string{srv.URL, "http://localhost:" + port + "/"} {
		resp, err := cl.Get(u)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("guarded client connected to %s", u)
		}
		if !strings.Contains(err.Error(), errBlockedAddr.Error()) {
			t.Errorf("%s: unexpected error %v", u, err)
		}
		if msg := fetchErrText(err); !strings.Contains(msg, "private or internal network") {
			t.Errorf("message for a blocked address: %q", msg)
		}
	}

	// Redirect hops are validated like the first address.
	hop := func(raw string, n int) error {
		u, _ := url.Parse(raw)
		return checkFeedRedirect(&http.Request{URL: u}, make([]*http.Request, n), false)
	}
	if hop("https://example.org/feed", 1) != nil {
		t.Error("https redirect to a public host refused")
	}
	for _, u := range []string{"http://example.org/feed", "https://127.0.0.1/", "https://localhost/x", "https://169.254.169.254/"} {
		if hop(u, 1) == nil {
			t.Errorf("redirect to %s allowed", u)
		}
	}
	if hop("https://example.org/feed", feedMaxHops) == nil {
		t.Error("redirect loop allowed")
	}
	// A fetcher without the test override refuses a private address outright.
	f := NewFetcher("")
	if _, err := f.Fetch(context.Background(), srv.URL, "", ""); err == nil {
		t.Error("Fetch accepted an http://127.0.0.1 address")
	}
}

// ---------- fetching ----------

type fakeFeeds struct {
	hits        int32
	conditional int32
	items       int
}

func (f *fakeFeeds) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.hits, 1)
		if r.Header.Get("User-Agent") == "" || !strings.Contains(r.Header.Get("Accept"), "application/rss+xml") {
			w.WriteHeader(400)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/feed.xml"):
			etag := fmt.Sprintf(`"v%d"`, f.items)
			if r.Header.Get("If-None-Match") == etag {
				atomic.AddInt32(&f.conditional, 1)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			w.Header().Set("Last-Modified", "Thu, 01 Oct 2026 09:00:00 GMT")
			w.Header().Set("Content-Type", "application/rss+xml")
			fmt.Fprint(w, `<rss version="2.0"><channel><title>Fake Tech</title><link>https://fake.example.org/</link>`)
			for k := 0; k < f.items; k++ {
				fmt.Fprintf(w, `<item><title>Hosting news %d: domain prices in Kenya</title><link>https://fake.example.org/n/%d?utm_source=rss</link><pubDate>Thu, 01 Oct 2026 0%d:00:00 +0000</pubDate><description>Story %d about web hosting.</description></item>`, k, k, k%10, k)
			}
			fmt.Fprint(w, `</channel></rss>`)
		case strings.HasSuffix(r.URL.Path, "/mirror.xml"): // the same articles through another feed
			fmt.Fprint(w, `<rss version="2.0"><channel><title>Mirror</title><item><title>Hosting news 0: domain prices in Kenya</title><link>http://www.fake.example.org/n/0/</link></item>
<item><title>Something else about exports</title><link>https://fake.example.org/n/else</link></item></channel></rss>`)
		case strings.HasSuffix(r.URL.Path, "/page"):
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!DOCTYPE html><html><body>not a feed</body></html>")
		case strings.HasSuffix(r.URL.Path, "/moved"):
			http.Redirect(w, r, strings.Replace(r.URL.Path, "/moved", "/feed.xml", 1), http.StatusMovedPermanently)
		case strings.HasSuffix(r.URL.Path, "/huge.xml"):
			fmt.Fprint(w, `<rss version="2.0"><channel><title>Huge</title><item><title>First</title><link>https://fake.example.org/h/1</link></item><item><title>`)
			w.Write([]byte(strings.Repeat("x", 3<<20)))
		case strings.HasSuffix(r.URL.Path, "/slow"):
			time.Sleep(1500 * time.Millisecond)
			fmt.Fprint(w, `<rss version="2.0"><channel><title>Slow</title></channel></rss>`)
		default:
			w.WriteHeader(404)
		}
	}
}

func TestFetchConditionalAndLimits(t *testing.T) {
	ff := &fakeFeeds{items: 60}
	srv := httptest.NewServer(ff.handler())
	defer srv.Close()
	f := NewFetcher(srv.URL)
	ctx := context.Background()

	res, err := f.Fetch(ctx, "https://fake.example.org/feed.xml", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Feed == nil || len(res.Feed.Items) != 50 || !res.Feed.Truncated || res.ETag != `"v60"` || res.LastModified == "" {
		t.Fatalf("first fetch: items=%d etag=%q %+v", len(res.Feed.Items), res.ETag, res)
	}
	// Conditional GET: unchanged → 304, nothing parsed.
	res2, err := f.Fetch(ctx, "https://fake.example.org/feed.xml", res.ETag, res.LastModified)
	if err != nil || !res2.NotModified || res2.Feed != nil || res2.ETag != res.ETag {
		t.Fatalf("conditional fetch: %+v %v", res2, err)
	}
	if atomic.LoadInt32(&ff.conditional) != 1 {
		t.Fatal("If-None-Match was not sent")
	}
	// Redirects are followed (test override) and the result parsed.
	if r, err := f.Fetch(ctx, "https://fake.example.org/moved", "", ""); err != nil || r.Feed.Title != "Fake Tech" {
		t.Fatalf("redirect: %v", err)
	}
	// A web page and a missing feed give readable errors.
	if _, err := f.Fetch(ctx, "https://fake.example.org/page", "", ""); err == nil || !strings.Contains(err.Error(), "web page") {
		t.Fatalf("html page: %v", err)
	}
	if _, err := f.Fetch(ctx, "https://fake.example.org/nothing", "", ""); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("404: %v", err)
	}
	// Body cap: 2 MB are read, the complete items before the cut are kept.
	r, err := f.Fetch(ctx, "https://fake.example.org/huge.xml", "", "")
	if err != nil || len(r.Feed.Items) != 1 || !r.Feed.Truncated || r.Bytes > feedMaxBytes {
		t.Fatalf("size cap: err=%v bytes=%d %+v", err, r.Bytes, r.Feed)
	}
	// Timeout.
	f.Client.Timeout = 300 * time.Millisecond
	if _, err := f.Fetch(ctx, "https://fake.example.org/slow", "", ""); err == nil || !strings.Contains(err.Error(), "10 seconds") {
		t.Fatalf("timeout: %v", err)
	}
	// The stored address must itself be a public https address, even with the override.
	for _, u := range []string{"http://fake.example.org/feed.xml", "https://localhost/feed.xml", "https://10.0.0.1/feed.xml"} {
		if _, err := f.Fetch(ctx, u, "", ""); err == nil {
			t.Errorf("Fetch(%s) accepted", u)
		}
	}
}

// ---------- tagging & relevance ----------

func TestTaggingAndRelevance(t *testing.T) {
	if !hasWord("new ai tools", "ai") || hasWord("he said", "ai") || hasWord("maintain", "ai") || !hasWord("domains are up", "domain") || !hasWord("visit example.co.ke now", ".co.ke") ||
		!hasWord("m-pesa fees", "m-pesa") || hasWord("exporting", "export") || !hasWord("exports rise", "export") || !hasWord("taxes rise", "tax") || hasWord("taxi fares", "tax") || !hasWord("the kes fell", "kes") || hasWord("cakes", "kes") {
		t.Fatal("hasWord")
	}
	c := &WatchContext{Rules: DefaultWatchRules(), Home: "KE", Markets: []string{"AU"}, TypeKey: "agency", TypeName: "Web & IT agency", TypeTopics: businessTopics["agency"],
		LineTopics: map[string][]string{TopicHosting: {"Hosting"}}}
	c.prepare()
	topics, countries, score, reasons := ScoreItem(c, "Hosting and domain prices rise in Kenya", "Registrars say .co.ke renewals cost more as the shilling weakens.", nil, "")
	// hosting: hosting, domain, registrar, .co.ke → 3+ hits (1.5) + title (0.25) = 1.75; boost 1 + 0.5 (agency) + 0.5 (sells hosting) = 2 → 12 × 1.75 × 2 = 42
	// fx: shilling → 1 hit, body only → 12 × 1 × 1 = 12;  country: Kenya = home → 15.  Total 69.
	if strings.Join(topics, ",") != "hosting,fx" || strings.Join(countries, ",") != "KE" {
		t.Fatalf("topics %v countries %v", topics, countries)
	}
	if score != 69 {
		t.Fatalf("score = %v, want 69 (%+v)", score, reasons)
	}
	if reasons[0].Key != TopicHosting || reasons[0].Points != 42 || !strings.Contains(reasons[0].Detail, "you sell Hosting") || !strings.Contains(reasons[0].Detail, "Web & IT agency") {
		t.Fatalf("top reason %+v", reasons[0])
	}
	if reasons[1].Kind != "country" || reasons[1].Detail != "your home market" || reasons[1].Points != 15 {
		t.Fatalf("country reason %+v", reasons[1])
	}
	// A client country scores 10; a source's own topic counts at half strength; the cap is 100.
	_, countries, score, reasons = ScoreItem(c, "Sydney council announces a street festival", "", []string{TopicRegulation}, "AU")
	if score != 6+10 || strings.Join(countries, ",") != "AU" || reasons[0].Detail != "you have clients there" {
		t.Fatalf("source topic + market: score %v %+v", score, reasons)
	}
	_, _, score, _ = ScoreItem(c, "AI cloud hosting domain ransomware payment export tax wordpress", "m-pesa shopify kra cloudflare chatgpt vulnerability exchange rate nairobi", nil, "")
	if score != 100 {
		t.Fatalf("cap: %v", score)
	}
	// Competitors (weight 3) and disabled topics.
	rules := DefaultWatchRules()
	for k := range rules {
		if rules[k].Key == TopicCompetitors {
			rules[k].Keywords = []string{"acme web"}
		}
		if rules[k].Key == TopicFX {
			rules[k].Enabled = false
		}
	}
	c2 := &WatchContext{Rules: rules, Home: "KE"}
	topics, _, score, _ = ScoreItem(c2, "Acme Web opens a Mombasa office", "The shilling was steady.", nil, "")
	if strings.Join(topics, ",") != "competitors" || score != 12*3*1.25+15 {
		t.Fatalf("competitors: %v %v", topics, score)
	}
	if _, _, s, r := ScoreItem(c2, "Garden show opens", "Flowers.", nil, ""); s != 0 || len(r) != 0 {
		t.Fatalf("irrelevant item scored %v", s)
	}
}

func TestCatalogueShape(t *testing.T) {
	seen := map[string]bool{}
	valid := map[string]bool{}
	for _, r := range DefaultWatchRules() {
		valid[r.Key] = true
	}
	groups := map[string]int{}
	for _, c := range DefaultCatalog {
		if c.Key == "" || seen[c.Key] || seen[c.URL] {
			t.Errorf("duplicate or empty catalogue entry %q", c.Key)
		}
		seen[c.Key], seen[c.URL] = true, true
		if u, err := ValidateFeedURL(c.URL, false); err != nil || u != c.URL {
			t.Errorf("%s: address %q is not a normalised public https address (%v → %q)", c.Key, c.URL, err, u)
		}
		for _, tp := range c.Topics {
			if !valid[tp] {
				t.Errorf("%s: unknown topic %q", c.Key, tp)
			}
		}
		if c.Verified != "" {
			if _, err := time.Parse(dayFmt, c.Verified); err != nil {
				t.Errorf("%s: Verified must be a YYYY-MM-DD date, got %q", c.Key, c.Verified)
			}
		}
		// Only a verified source may ever be switched on without a live check.
		if c.Verified == "" && seedByDefault(c, c.Country, map[string]bool{c.Country: true}, "agency") {
			t.Errorf("%s: an unverified source would be seeded", c.Key)
		}
		groups[c.Group]++
	}
	for _, g := range []string{"local", "regulator", "vendor", "trade"} {
		if groups[g] == 0 {
			t.Errorf("no catalogue source in group %s", g)
		}
	}
	v := CatalogFeed{Key: "x", Country: "AU", Verified: "2026-10-02", Types: []string{"agency"}}
	if !seedByDefault(v, "KE", map[string]bool{"AU": true}, "agency") || seedByDefault(v, "KE", map[string]bool{}, "agency") || seedByDefault(v, "AU", nil, "salon") {
		t.Error("seedByDefault: country / business type rules")
	}
}

// TestCatalogueLive fetches every catalogue source with the production
// fetcher and reports which answer with a readable feed. It needs outbound
// internet access, so it only runs on request:
//
//	WATCH_VERIFY_LIVE=1 go test ./pkg/insights -run TestCatalogueLive -v
func TestCatalogueLive(t *testing.T) {
	if os.Getenv("WATCH_VERIFY_LIVE") != "1" {
		t.Skip("set WATCH_VERIFY_LIVE=1 to check the catalogue against the live sites")
	}
	f := NewFetcher("")
	okN := 0
	for _, c := range DefaultCatalog {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		res, err := f.Fetch(ctx, c.URL, "", "")
		cancel()
		if err != nil {
			t.Logf("FAIL  %-22s %s — %v", c.Key, c.URL, err)
			continue
		}
		okN++
		first := ""
		if len(res.Feed.Items) > 0 {
			first = res.Feed.Items[0].Title
		}
		t.Logf("OK    %-22s %s — %s, %d items, title %q, first %q", c.Key, c.URL, res.Feed.Kind, len(res.Feed.Items), res.Feed.Title, first)
	}
	t.Logf("%d of %d sources answered with a feed on %s", okN, len(DefaultCatalog), time.Now().Format(dayFmt))
}

// ---------- storage: dedupe, conditional GET, retention, filters ----------

func watchFixture(t *testing.T) (*Intel, *gorm.DB, *fakeFeeds) {
	db := testDB(t)
	if err := MigrateIntel(db); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES (?, 'Test', 'KE', 'KES', 'Africa/Nairobi', 'user')`, org)
	db.Exec(`INSERT INTO customers (id, organization_id, first_name, last_name, company_name, country_code, created_at) VALUES ('AU1', ?, 'A', 'U', 'Aussie Co', 'AU', now())`, org)
	ff := &fakeFeeds{items: 3}
	srv := httptest.NewServer(ff.handler())
	t.Cleanup(srv.Close)
	s := New(db, nil, nil, nil, nil)
	now := d("2026-10-02").Add(9 * time.Hour)
	s.Now = func() time.Time { return now }
	i := NewIntel(s, NewFetcher(srv.URL))
	i.BusinessType = func(string) (string, string) { return "agency", "Web & IT agency" }
	return i, db, ff
}

func TestWatchStoreDedupeRetention(t *testing.T) {
	i, db, ff := watchFixture(t)
	ctx := context.Background()

	// Nothing is seeded while the catalogue is unverified; everything is a suggestion.
	i.EnsureSeeded(org)
	if n := len(i.Feeds(org)); n != 0 {
		t.Fatalf("%d sources seeded from an unverified catalogue", n)
	}
	if len(i.Suggestions(org)) != len(DefaultCatalog) {
		t.Fatal("suggestions")
	}

	// Preview does not store anything.
	p, _, err := i.PreviewFeed(ctx, "fake.example.org/feed.xml")
	if err != nil || p.Title != "Fake Tech" || p.Count != 3 || len(p.Latest) != 3 || p.Latest[0].Title != "Hosting news 2: domain prices in Kenya" {
		t.Fatalf("preview %+v %v", p, err)
	}
	var n int64
	db.Model(&WatchItem{}).Count(&n)
	if n != 0 {
		t.Fatal("preview stored items")
	}
	// Adding validates by fetching: a page that is not a feed is refused and nothing is saved.
	if _, _, err := i.AddFeed(ctx, org, "u1", FeedInput{URL: "https://fake.example.org/page"}); err == nil {
		t.Fatal("a web page was added as a source")
	}
	if _, _, err := i.AddFeed(ctx, org, "u1", FeedInput{URL: "http://fake.example.org/feed.xml"}); err == nil {
		t.Fatal("http address accepted")
	}
	f1, run, err := i.AddFeed(ctx, org, "u1", FeedInput{URL: "https://fake.example.org/feed.xml", Country: "ke", Topics: []string{TopicHosting}})
	if err != nil || run.New != 3 || f1.Title != "Fake Tech" || f1.Country != "KE" || !f1.Enabled || f1.LastStatus != "ok" || f1.Items != 3 {
		t.Fatalf("add: %+v %+v %v", f1, run, err)
	}
	if _, _, err := i.AddFeed(ctx, org, "u1", FeedInput{URL: "https://www.fake.example.org/feed.xml/"}); err == nil || !strings.Contains(err.Error(), "already follow") {
		t.Fatalf("duplicate source: %v", err)
	}
	// A second feed carrying one of the same articles (other host spelling, http, trailing slash): stored once.
	_, run2, err := i.AddFeed(ctx, org, "u1", FeedInput{URL: "https://fake.example.org/mirror.xml", Title: "Mirror feed"})
	if err != nil || run2.Items != 2 || run2.New != 1 {
		t.Fatalf("mirror: %+v %v", run2, err)
	}
	db.Model(&WatchItem{}).Where("organization_id = ?", org).Count(&n)
	if n != 4 {
		t.Fatalf("items = %d, want 4 (3 + 1 new from the mirror)", n)
	}

	// Refresh: unchanged feed → conditional GET → 304; nothing new, no duplicates.
	before := atomic.LoadInt32(&ff.conditional)
	sum := i.Refresh(ctx, org, "")
	if sum.Busy || sum.Checked != 2 || sum.New != 0 || sum.Errors != 0 {
		t.Fatalf("refresh %+v", sum)
	}
	if atomic.LoadInt32(&ff.conditional) != before+1 {
		t.Fatal("refresh did not send the stored ETag")
	}
	if f := i.feedByID(org, f1.ID); f.LastStatus != "not_modified" || f.Failures != 0 {
		t.Fatalf("status after 304: %+v", f)
	}
	// The feed grows: only the new items are added.
	ff.items = 5
	sum = i.Refresh(ctx, org, f1.ID)
	if sum.New != 2 || sum.Feeds[0].Items != 5 {
		t.Fatalf("refresh after new items %+v", sum)
	}
	// A failing source records its error and keeps its items.
	db.Model(&WatchFeed{}).Where("id = ?", f1.ID).Update("url", "https://fake.example.org/gone.xml")
	sum = i.Refresh(ctx, org, f1.ID)
	if sum.Errors != 1 || !strings.Contains(sum.Feeds[0].Error, "HTTP 404") {
		t.Fatalf("failing source %+v", sum)
	}
	if f := i.feedByID(org, f1.ID); f.LastStatus != "error" || f.Failures != 1 || !strings.Contains(f.LastError, "404") || f.Items != 5 {
		t.Fatalf("error status: %+v", f)
	}
	db.Model(&WatchFeed{}).Where("id = ?", f1.ID).Update("url", "https://fake.example.org/feed.xml")

	// List: relevance order, filters, facets.
	l := i.List(org, WatchFilter{})
	if l.Total != 6 || l.All != 6 || l.Unread != 6 || l.Feeds != 2 || l.Failing != 1 || l.Suggested != len(DefaultCatalog) {
		t.Fatalf("list totals %+v", l)
	}
	top := l.Items[0]
	// hosting: "hosting", "domain" (+ source topic) → 1.25 + 0.25 title = 1.5 × boost 1.5 (agency) → 27; Kenya (home) 15 → 42.
	if top.Score != 42 || strings.Join(top.Topics, ",") != "hosting" || strings.Join(top.Countries, ",") != "KE" || top.Source != "Fake Tech" || len(top.Reasons) != 2 {
		t.Fatalf("top item %+v", top)
	}
	if got := i.List(org, WatchFilter{Topic: "trade"}); got.Total != 1 || got.Items[0].Title != "Something else about exports" {
		t.Fatalf("topic filter: %d", got.Total)
	}
	if got := i.List(org, WatchFilter{Country: "KE"}); got.Total != 5 {
		t.Fatalf("country filter: %d", got.Total)
	}
	if got := i.List(org, WatchFilter{FeedID: f1.ID, Q: "NEWS 4"}); got.Total != 1 {
		t.Fatalf("source + search: %d", got.Total)
	}
	if len(l.Topics) != 2 || l.Topics[0].Key != "hosting" || l.Topics[0].Count != 5 || l.Countries[0].Key != "KE" || len(l.Sources) != 2 {
		t.Fatalf("facets %+v %+v %+v", l.Topics, l.Countries, l.Sources)
	}

	// Save / dismiss / read.
	yes, no := true, false
	it, err := i.SetItem(org, top.ID, ItemPatch{Saved: &yes, Read: &yes})
	if err != nil || !it.Saved || it.ReadAt == nil {
		t.Fatalf("save %+v %v", it, err)
	}
	other := l.Items[1]
	if it, _ := i.SetItem(org, other.ID, ItemPatch{Dismissed: &yes}); !it.Dismissed || it.ReadAt == nil {
		t.Fatalf("dismiss %+v", it)
	}
	if _, err := i.SetItem("other-org", top.ID, ItemPatch{Saved: &no}); err == nil {
		t.Fatal("another organisation changed an item")
	}
	if got := i.List(org, WatchFilter{Saved: true}); got.Total != 1 {
		t.Fatalf("saved filter %d", got.Total)
	}
	if got := i.List(org, WatchFilter{Unread: true}); got.Total != 4 || got.Unread != 4 {
		t.Fatalf("unread filter %d / %d", got.Total, got.Unread)
	}
	if got := i.List(org, WatchFilter{Dismissed: true}); got.Total != 1 {
		t.Fatalf("dismissed filter %d", got.Total)
	}
	if lt := i.Latest(org, 3); len(lt.Items) != 3 || lt.Items[0].ID != top.ID || lt.Feeds != 2 || lt.Unread != 4 {
		t.Fatalf("latest %+v", lt)
	}
	// A dismissed item does not come back on the next refresh.
	ff.items = 6
	if sum := i.Refresh(ctx, org, f1.ID); sum.New != 1 {
		t.Fatalf("refresh after dismiss %+v", sum)
	}
	db.Model(&WatchItem{}).Where("organization_id = ?", org).Count(&n)
	if n != 7 {
		t.Fatalf("items = %d, want 7", n)
	}

	// Editing the rules re-scores stored items.
	rules := DefaultWatchRules()
	for k := range rules {
		if rules[k].Key == TopicHosting {
			rules[k].Weight = 2
		}
	}
	if _, err := i.SaveWatchRules(org, rules); err != nil {
		t.Fatal(err)
	}
	if n := i.Rescore(org); n == 0 {
		t.Fatal("nothing re-scored after a rule change")
	}
	if got := i.List(org, WatchFilter{Saved: true}); got.Items[0].Score != 69 { // 27 × 2 + 15
		t.Fatalf("score after weight change: %v", got.Items[0].Score)
	}
	if _, err := i.SaveWatchRules(org, []WatchRule{{Label: "X", Keywords: []string{"a"}}}); err == nil {
		t.Fatal("one-letter keyword accepted")
	}
	if r := i.ResetWatchRules(org); len(r) != len(DefaultWatchRules()) {
		t.Fatal("reset")
	}

	// Retention: 180 days; saved items are kept.
	old := i.now().AddDate(0, 0, -WatchRetentionDays-5)
	db.Model(&WatchItem{}).Where("organization_id = ?", org).Updates(map[string]any{"first_seen_at": old, "published_at": old})
	recent := l.Items[2]
	db.Model(&WatchItem{}).Where("id = ?", recent.ID).Updates(map[string]any{"first_seen_at": i.now().AddDate(0, 0, -WatchRetentionDays+1), "published_at": nil})
	if pruned := i.Prune(org); pruned != 5 {
		t.Fatalf("pruned %d, want 5 (7 − saved − still inside the window)", pruned)
	}
	var left []WatchItem
	db.Where("organization_id = ?", org).Order("saved DESC").Find(&left)
	if len(left) != 2 || !left[0].Saved || left[0].ID != top.ID || left[1].ID != recent.ID {
		t.Fatalf("after retention: %+v", left)
	}
	// Items older than the window are not re-imported from a feed that still lists them.
	i.Now = func() time.Time { return d("2027-06-01") }
	db.Model(&WatchFeed{}).Where("id = ?", f1.ID).Updates(map[string]any{"etag": "", "last_modified": ""})
	if sum := i.Refresh(ctx, org, f1.ID); sum.New != 0 {
		t.Fatalf("old items re-imported: %+v", sum)
	}

	// Removing a source drops its items but keeps saved ones.
	if err := i.DeleteFeed(org, f1.ID); err != nil {
		t.Fatal(err)
	}
	db.Model(&WatchItem{}).Where("organization_id = ? AND feed_id = ?", org, f1.ID).Count(&n)
	if n != 1 {
		t.Fatalf("items of the removed source = %d, want the saved one", n)
	}
	if i.DeleteFeed(org, f1.ID) == nil {
		t.Fatal("deleting twice succeeded")
	}
}

func TestWatchSeedsOnlyVerifiedSources(t *testing.T) {
	i, db, _ := watchFixture(t)
	saved := DefaultCatalog
	defer func() { DefaultCatalog = saved }()
	// Two verified entries (one for a country the organisation has nothing to do with) and an unverified one.
	DefaultCatalog = []CatalogFeed{
		{Key: "ok_ke", Title: "Verified KE", URL: "https://fake.example.org/feed.xml", Group: "local", Country: "KE", Topics: []string{TopicHosting}, Verified: "2026-10-02"},
		{Key: "ok_br", Title: "Verified BR", URL: "https://fake.example.org/br.xml", Group: "local", Country: "BR", Verified: "2026-10-02"},
		{Key: "unverified", Title: "Unverified", URL: "https://fake.example.org/mirror.xml", Group: "local"},
	}
	i.EnsureSeeded(org)
	feeds := i.Feeds(org)
	if len(feeds) != 1 || feeds[0].CatalogKey != "ok_ke" || !feeds[0].Enabled || feeds[0].Topics[0] != TopicHosting || feeds[0].LastStatus != "" {
		t.Fatalf("seeded %+v", feeds)
	}
	if sug := i.Suggestions(org); len(sug) != 2 {
		t.Fatalf("suggestions %+v", sug)
	}
	// Seeding happens once per source: a removed source does not come back.
	if err := i.DeleteFeed(org, feeds[0].ID); err != nil {
		t.Fatal(err)
	}
	i.EnsureSeeded(org)
	if n := len(i.Feeds(org)); n != 0 {
		t.Fatalf("a removed default source was added again (%d)", n)
	}
	// A source verified in a later release is added then.
	DefaultCatalog = append(DefaultCatalog, CatalogFeed{Key: "later", Title: "Later", URL: "https://fake.example.org/later.xml", Group: "trade", Verified: "2026-11-01"})
	i.EnsureSeeded(org)
	if f := i.Feeds(org); len(f) != 1 || f[0].CatalogKey != "later" {
		t.Fatalf("later source %+v", f)
	}
	var n int64
	db.Model(&WatchItem{}).Count(&n)
	if n != 0 {
		t.Fatal("seeding fetched items")
	}
}

func TestWatchSchedulerAndSuggested(t *testing.T) {
	i, db, _ := watchFixture(t)
	ctx := context.Background()
	// "Check & add": every suggestion is fetched; the fake serves none of the catalogue addresses.
	res := i.AddSuggested(ctx, org, "u1", []string{"techweez", "wto_news"})
	if len(res.Added) != 0 || len(res.Failed) != 2 || !strings.Contains(res.Failed[0].Error, "404") {
		t.Fatalf("suggested %+v", res)
	}
	if n := len(i.Feeds(org)); n != 0 {
		t.Fatalf("%d unreadable suggestions were added", n)
	}
	f, _, err := i.AddFeed(ctx, org, "u1", FeedInput{URL: "https://fake.example.org/feed.xml"})
	if err != nil {
		t.Fatal(err)
	}
	// Scheduler: refreshes once, then not again within the interval.
	db.Model(&IntelSettings{}).Where("organization_id = ?", org).Update("brief_auto", false)
	if r, _ := i.RunScheduled(ctx); r != 1 {
		t.Fatalf("first scheduled run refreshed %d organisations", r)
	}
	if r, _ := i.RunScheduled(ctx); r != 0 {
		t.Fatalf("second run within the interval refreshed %d", r)
	}
	st := i.Settings(org)
	if st.WatchLastRun == nil || !strings.Contains(st.WatchLastStatus, "1 sources checked") {
		t.Fatalf("settings after run %+v", st)
	}
	later := i.now().Add(watchEvery + time.Minute)
	i.Now = func() time.Time { return later }
	if r, _ := i.RunScheduled(ctx); r != 1 {
		t.Fatal("no refresh after the interval")
	}
	// A disabled source is not fetched by the scheduler.
	off := false
	if _, err := i.UpdateFeed(ctx, org, f.ID, FeedPatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	later = later.Add(watchEvery + time.Minute)
	if r, _ := i.RunScheduled(ctx); r != 0 {
		t.Fatal("scheduler refreshed an organisation without enabled sources")
	}
	// While another session holds the lock a manual refresh reports busy.
	held := make(chan struct{})
	release := make(chan struct{})
	go i.withLock(ctx, "insights-watch:"+org, func() { close(held); <-release })
	<-held
	if sum := i.Refresh(ctx, org, ""); !sum.Busy {
		t.Fatal("refresh ran while the lock was held")
	}
	close(release)
}
