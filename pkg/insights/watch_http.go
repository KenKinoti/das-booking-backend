package insights

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Fetching feeds safely. A feed address is typed in by a user, so the fetcher
// is treated as a request-forgery target:
//
//   - https only, standard port, a public host name (no IP literals, no
//     credentials, no localhost / .local / .internal names);
//   - the address actually dialled is checked after DNS resolution, on every
//     connection (so a name that resolves — or later re-resolves — to a
//     private, loopback, link-local or metadata address is refused);
//   - redirects are followed at most 5 times and each hop is re-validated the
//     same way; no proxy is used (a proxy would do the dialling instead);
//   - 10 s for the whole exchange, 2 MB of body, 50 items.
//
// INDUSTRY_WATCH_BASE_URL (tests only) sends every request to a local fake:
// https://host/path is fetched from {base}/host/path and the checks above are
// relaxed for it.

const (
	feedTimeout  = 10 * time.Second
	feedMaxBytes = 2 << 20
	feedMaxItems = 50
	feedMaxHops  = 5
	feedAgent    = "DASYIN-ERP industry watch (feed reader; +https://dasyin.com)"
)

var errBlockedAddr = errors.New("refusing to connect to a non-public address")

var blockedNets = func() []*net.IPNet {
	out := []*net.IPNet{}
	for _, c := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// nat64 and 6to4 prefixes embed an IPv4 address that is checked as well.
var (
	_, nat64Net, _  = net.ParseCIDR("64:ff9b::/96")
	_, sixToFour, _ = net.ParseCIDR("2002::/16")
)

// IsPublicIP reports whether ip is a globally routable unicast address a
// feed may be fetched from.
func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4 // also unwraps IPv4-mapped IPv6 (::ffff:a.b.c.d)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return false
		}
	}
	if len(ip) == net.IPv6len {
		if nat64Net.Contains(ip) {
			return IsPublicIP(net.IP(ip[12:16]))
		}
		if sixToFour.Contains(ip) {
			return IsPublicIP(net.IP(ip[2:6]))
		}
	}
	return true
}

// dialControl runs on every connection, after DNS resolution.
func dialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // IPv6 zone
	}
	if !IsPublicIP(net.ParseIP(host)) {
		return errBlockedAddr
	}
	return nil
}

var reFeedHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// ValidateFeedURL checks a feed address and returns it normalised. With
// allowPrivate (tests) http and local hosts are accepted.
func ValidateFeedURL(raw string, allowPrivate bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("Enter the address of an RSS or Atom feed")
	}
	if len(raw) > 500 {
		return "", errors.New("That address is too long")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errors.New("That doesn't look like a web address")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(allowPrivate && scheme == "http") {
		return "", errors.New("The feed address must start with https://")
	}
	if u.User != nil {
		return "", errors.New("Don't include a username or password in the address")
	}
	host := strings.ToLower(u.Hostname())
	if !allowPrivate {
		if p := u.Port(); p != "" && p != "443" {
			return "", errors.New("Custom ports aren't supported; use the feed's normal https address")
		}
		if net.ParseIP(host) != nil || strings.HasPrefix(host, "[") {
			return "", errors.New("Use the site's domain name, not an IP address")
		}
		if !reFeedHost.MatchString(host) || host == "localhost" {
			return "", errors.New("Enter a public web address, e.g. https://example.com/feed/")
		}
		for _, suf := range []string{".localhost", ".local", ".internal", ".lan", ".home", ".corp", ".intranet", ".test", ".invalid", ".example"} {
			if strings.HasSuffix(host, suf) {
				return "", errors.New("Enter a public web address, e.g. https://example.com/feed/")
			}
		}
	}
	u.Scheme, u.Fragment = scheme, ""
	u.Host = host
	if p := u.Port(); p != "" && !(scheme == "https" && p == "443") {
		u.Host = net.JoinHostPort(host, p)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// newFeedClient builds the HTTP client. guarded = the production behaviour.
func newFeedClient(guarded bool) *http.Client {
	dialer := &net.Dialer{Timeout: 6 * time.Second}
	if guarded {
		dialer.Control = dialControl
	}
	tr := &http.Transport{
		Proxy:                  nil, // a proxy would bypass the dial-time address check
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    6 * time.Second,
		ResponseHeaderTimeout:  8 * time.Second,
		MaxIdleConns:           8,
		IdleConnTimeout:        30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   feedTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return checkFeedRedirect(req, via, !guarded)
		},
	}
}

// checkFeedRedirect validates one redirect hop.
func checkFeedRedirect(req *http.Request, via []*http.Request, allowPrivate bool) error {
	if len(via) >= feedMaxHops {
		return errors.New("too many redirects")
	}
	if _, err := ValidateFeedURL(req.URL.String(), allowPrivate); err != nil {
		return fmt.Errorf("redirected to an address that is not allowed (%s)", req.URL.Host)
	}
	return nil
}

// Fetcher downloads and parses feeds.
type Fetcher struct {
	Client       *http.Client
	BaseURL      string // tests: every request goes to {BaseURL}/{host}{path}
	AllowPrivate bool
	MaxBytes     int64
	MaxItems     int
}

// NewFetcherFromEnv builds the fetcher (INDUSTRY_WATCH_BASE_URL for tests).
func NewFetcherFromEnv() *Fetcher {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("INDUSTRY_WATCH_BASE_URL")), "/")
	if base != "" {
		log.Printf("insights watch: INDUSTRY_WATCH_BASE_URL is set — every feed is fetched from %s and the address checks are relaxed (tests only; never set this in production)", base)
	}
	return NewFetcher(base)
}

// NewFetcher builds a fetcher; a non-empty base is the test override.
func NewFetcher(base string) *Fetcher {
	f := &Fetcher{BaseURL: base, AllowPrivate: base != "", MaxBytes: feedMaxBytes, MaxItems: feedMaxItems}
	f.Client = newFeedClient(base == "")
	return f
}

// FetchResult is one download.
type FetchResult struct {
	Status       int
	NotModified  bool
	ETag         string
	LastModified string
	Feed         *ParsedFeed
	Bytes        int64
}

func (f *Fetcher) target(feedURL string) string {
	if f.BaseURL == "" {
		return feedURL
	}
	u, err := url.Parse(feedURL)
	if err != nil {
		return feedURL
	}
	return f.BaseURL + "/" + u.Host + u.RequestURI()
}

// FeedError is a readable fetch failure (shown next to the source).
type FeedError struct {
	Status int
	Msg    string
}

func (e *FeedError) Error() string { return e.Msg }

// Fetch downloads feedURL (a validated https address) with a conditional GET
// and parses it. A 304 returns NotModified without a feed.
func (f *Fetcher) Fetch(ctx context.Context, feedURL, etag, lastModified string) (*FetchResult, error) {
	if _, err := ValidateFeedURL(feedURL, false); err != nil {
		return nil, &FeedError{Msg: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.target(feedURL), nil)
	if err != nil {
		return nil, &FeedError{Msg: "That doesn't look like a web address"}
	}
	req.Header.Set("User-Agent", feedAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.8, */*;q=0.1")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
	cl := f.Client
	if cl == nil {
		cl = newFeedClient(!f.AllowPrivate)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, &FeedError{Msg: fetchErrText(err)}
	}
	defer resp.Body.Close()
	res := &FetchResult{Status: resp.StatusCode, ETag: clipRunes(resp.Header.Get("ETag"), 250), LastModified: clipRunes(resp.Header.Get("Last-Modified"), 80)}
	if resp.StatusCode == http.StatusNotModified {
		res.NotModified = true
		if res.ETag == "" {
			res.ETag = etag
		}
		if res.LastModified == "" {
			res.LastModified = lastModified
		}
		return res, nil
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("The site answered HTTP %d", resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusGone:
			msg = fmt.Sprintf("No feed at this address (HTTP %d)", resp.StatusCode)
		case http.StatusForbidden, http.StatusUnauthorized:
			msg = fmt.Sprintf("The site refused the request (HTTP %d) — it may block feed readers", resp.StatusCode)
		case http.StatusTooManyRequests:
			msg = "The site is rate-limiting requests (HTTP 429); it will be tried again later"
		}
		return res, &FeedError{Status: resp.StatusCode, Msg: msg}
	}
	max := f.MaxBytes
	if max <= 0 {
		max = feedMaxBytes
	}
	cr := &countReader{r: io.LimitReader(resp.Body, max)}
	head := make([]byte, 512)
	n, _ := io.ReadFull(cr, head)
	head = head[:n]
	if !sniffFeed(head) {
		return res, &FeedError{Status: resp.StatusCode, Msg: "This address serves a web page, not an RSS or Atom feed"}
	}
	feed, err := ParseFeed(io.MultiReader(bytes.NewReader(head), cr), f.MaxItems)
	res.Bytes = cr.n
	if err != nil {
		if errors.Is(err, ErrNotFeed) {
			return res, &FeedError{Status: resp.StatusCode, Msg: "This address does not serve an RSS or Atom feed"}
		}
		if cr.n >= max {
			return res, &FeedError{Status: resp.StatusCode, Msg: "The feed is larger than 2 MB and could not be read"}
		}
		return res, &FeedError{Status: resp.StatusCode, Msg: capFirst(err.Error())}
	}
	ResolveLinks(feed, feedURL)
	res.Feed = feed
	return res, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func fetchErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if errors.Is(ue.Err, errBlockedAddr) || strings.Contains(ue.Error(), errBlockedAddr.Error()) {
			return "That address points to a private or internal network and can't be used"
		}
		if ue.Timeout() {
			return "The site took longer than 10 seconds to respond"
		}
		msg := ue.Err.Error()
		switch {
		case strings.Contains(msg, "not allowed"), strings.Contains(msg, "too many redirects"):
			return capFirst(msg)
		case strings.Contains(msg, "no such host"):
			return "The site's address could not be found (DNS)"
		case strings.Contains(msg, "certificate"), strings.Contains(msg, "tls:"), strings.Contains(msg, "x509"):
			return "The site's security certificate could not be verified"
		case strings.Contains(msg, "connection refused"):
			return "The site refused the connection"
		}
	}
	return "The site could not be reached"
}
