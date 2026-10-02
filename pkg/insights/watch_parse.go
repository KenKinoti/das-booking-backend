package insights

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Feed parsing for the industry watch: RSS 2.0, RSS 1.0 (RDF) and Atom 1.0
// with encoding/xml only. Only what a feed says about an article is kept — a
// title, a link, a date and a short plain-text summary taken from the feed's
// own description. Articles themselves are never fetched or stored.

// SummaryMax is the longest stored summary, in characters.
const SummaryMax = 400

// FeedItem is one entry of a feed.
type FeedItem struct {
	GUID      string     `json:"guid,omitempty"`
	Title     string     `json:"title"`
	Link      string     `json:"link"`
	Summary   string     `json:"summary"`
	Published *time.Time `json:"published_at"`
}

// ParsedFeed is a parsed feed document.
type ParsedFeed struct {
	Kind      string     `json:"kind"` // rss | atom | rdf
	Title     string     `json:"title"`
	Link      string     `json:"link"`
	Items     []FeedItem `json:"items"`
	Truncated bool       `json:"truncated"` // cut at the item or size cap
}

// ErrNotFeed is returned when a document is not RSS or Atom.
var ErrNotFeed = errors.New("this address does not serve an RSS or Atom feed")

type xText struct {
	XMLName xml.Name
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
	Inner   string `xml:",innerxml"`
}

type xLink struct {
	XMLName xml.Name
	Href    string `xml:"href,attr"`
	Rel     string `xml:"rel,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

type xEntry struct {
	Titles    []xText `xml:"title"`
	Links     []xLink `xml:"link"`
	GUID      string  `xml:"guid"`
	ID        string  `xml:"id"`
	PubDate   string  `xml:"pubDate"`
	DCDate    string  `xml:"http://purl.org/dc/elements/1.1/ date"`
	Published string  `xml:"published"`
	Updated   string  `xml:"updated"`
	Descs     []xText `xml:"description"`
	Summaries []xText `xml:"summary"`
	Contents  []xText `xml:"content"`
	Encoded   []xText `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
}

// Namespaces whose title / description / link are the item's own (others,
// such as media: or itunes:, describe attachments).
func ownNS(space string) bool {
	switch space {
	case "", "http://www.w3.org/2005/Atom", "http://purl.org/rss/1.0/", "http://purl.org/atom/ns#", "http://backend.userland.com/rss2":
		return true
	}
	return false
}

func pickText(list []xText) string {
	best := ""
	for _, t := range list {
		if !ownNS(t.XMLName.Space) {
			continue
		}
		v := t.Text
		if strings.TrimSpace(v) == "" || strings.EqualFold(t.Type, "xhtml") {
			v = t.Inner // content held in child elements (Atom type="xhtml")
		}
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	for _, t := range list {
		if strings.TrimSpace(t.Text) != "" {
			best = t.Text
			break
		}
	}
	return best
}

func pickLink(list []xLink) string {
	// RSS: <link>https://…</link>; Atom: <link rel="alternate" href="…"/>.
	for _, l := range list {
		if ownNS(l.XMLName.Space) && l.XMLName.Space != "http://www.w3.org/2005/Atom" && strings.TrimSpace(l.Text) != "" {
			return strings.TrimSpace(l.Text)
		}
	}
	first := ""
	for _, l := range list {
		h := strings.TrimSpace(l.Href)
		if h == "" {
			continue
		}
		rel := strings.ToLower(strings.TrimSpace(l.Rel))
		if rel == "" || rel == "alternate" {
			if l.Type == "" || strings.Contains(l.Type, "html") {
				return h
			}
			if first == "" {
				first = h
			}
		}
	}
	if first != "" {
		return first
	}
	for _, l := range list {
		if strings.TrimSpace(l.Text) != "" {
			return strings.TrimSpace(l.Text)
		}
	}
	return ""
}

// charsetReader decodes the single-byte encodings old feeds still declare.
func charsetReader(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf-8", "utf8", "":
		return in, nil
	case "us-ascii", "ascii", "iso-8859-1", "iso8859-1", "latin1", "latin-1", "windows-1252", "cp1252", "iso-8859-15":
		b, err := io.ReadAll(in)
		if err != nil {
			return nil, err
		}
		out := make([]rune, 0, len(b))
		for _, c := range b {
			if c >= 0x80 && c <= 0x9f {
				out = append(out, cp1252[c-0x80])
			} else {
				out = append(out, rune(c))
			}
		}
		return strings.NewReader(string(out)), nil
	}
	return nil, fmt.Errorf("unsupported feed encoding %q", label)
}

var cp1252 = [32]rune{0x20AC, 0xFFFD, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0xFFFD, 0x017D, 0xFFFD,
	0xFFFD, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0xFFFD, 0x017E, 0x0178}

// ParseFeed reads an RSS / Atom document, keeping at most maxItems entries.
// The document is decoded as a stream: when it is cut short (size cap) the
// entries read so far are returned with Truncated set.
func ParseFeed(r io.Reader, maxItems int) (*ParsedFeed, error) {
	if maxItems <= 0 {
		maxItems = 50
	}
	d := xml.NewDecoder(r)
	d.Strict = false // tolerate stray entities; no HTML auto-close: <link> is a real element in RSS
	d.Entity = xml.HTMLEntity
	d.CharsetReader = charsetReader
	f := &ParsedFeed{Items: []FeedItem{}}
	stack := []string{}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if f.Kind != "" && len(f.Items) > 0 {
				f.Truncated = true // cut at the size cap: keep what was read
				break
			}
			if f.Kind == "" {
				return nil, ErrNotFeed
			}
			return nil, fmt.Errorf("the feed could not be read (%s)", shortXMLErr(err))
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if len(stack) == 0 {
				switch name {
				case "rss":
					f.Kind = "rss"
				case "feed":
					f.Kind = "atom"
				case "rdf":
					f.Kind = "rdf"
				default:
					return nil, ErrNotFeed
				}
				stack = append(stack, name)
				continue
			}
			parent := stack[len(stack)-1]
			switch {
			case name == "item" || (name == "entry" && f.Kind == "atom"):
				if len(f.Items) >= maxItems {
					f.Truncated = true
					return f, nil
				}
				var e xEntry
				if err := d.DecodeElement(&e, &t); err != nil {
					if len(f.Items) > 0 {
						f.Truncated = true
						return f, nil
					}
					return nil, fmt.Errorf("the feed could not be read (%s)", shortXMLErr(err))
				}
				if it, ok := entryItem(&e); ok {
					f.Items = append(f.Items, it)
				}
				continue
			case name == "title" && (parent == "channel" || parent == "feed") && f.Title == "":
				var x xText
				if d.DecodeElement(&x, &t) == nil {
					f.Title = clipRunes(PlainText(pickText([]xText{x})), 200)
				}
				continue
			case name == "link" && (parent == "channel" || parent == "feed") && f.Link == "":
				var x xLink
				if d.DecodeElement(&x, &t) == nil {
					rel := strings.ToLower(x.Rel)
					if strings.TrimSpace(x.Text) != "" {
						f.Link = strings.TrimSpace(x.Text)
					} else if x.Href != "" && (rel == "" || rel == "alternate") {
						f.Link = strings.TrimSpace(x.Href)
					}
				}
				continue
			}
			stack = append(stack, name)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if f.Kind == "" {
		return nil, ErrNotFeed
	}
	return f, nil
}

func shortXMLErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	return clipRunes(s, 120)
}

func entryItem(e *xEntry) (FeedItem, bool) {
	it := FeedItem{Title: clipRunes(PlainText(pickText(e.Titles)), 300), Link: pickLink(e.Links)}
	it.GUID = strings.TrimSpace(e.GUID)
	if it.GUID == "" {
		it.GUID = strings.TrimSpace(e.ID)
	}
	if it.Link == "" && looksLikeURL(it.GUID) {
		it.Link = it.GUID // <guid isPermaLink="true">
	}
	desc := pickText(e.Descs)
	if strings.TrimSpace(desc) == "" {
		desc = pickText(e.Summaries)
	}
	if strings.TrimSpace(desc) == "" {
		desc = pickText(e.Encoded)
	}
	if strings.TrimSpace(desc) == "" {
		desc = pickText(e.Contents)
	}
	it.Summary = Summarise(desc)
	for _, raw := range []string{e.PubDate, e.Published, e.DCDate, e.Updated} {
		if t, ok := ParseFeedDate(raw); ok {
			it.Published = &t
			break
		}
	}
	if it.Title == "" {
		it.Title = clipRunes(it.Summary, 120)
	}
	if it.Title == "" || (it.Link == "" && it.GUID == "") {
		return it, false
	}
	return it, true
}

func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// ---------- text ----------

var (
	reBlock      = regexp.MustCompile(`(?is)<(script|style|noscript|iframe|svg|figure)\b.*?</(script|style|noscript|iframe|svg|figure)\s*>`)
	reComment    = regexp.MustCompile(`(?s)<!--.*?-->`)
	reCDATA      = regexp.MustCompile(`(?s)<!\[CDATA\[(.*?)\]\]>`)
	reBreak      = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/h[1-6]|/tr|/blockquote)\b[^>]*>`)
	reTag        = regexp.MustCompile(`(?s)</?[a-zA-Z!?][^<>]*>`)
	reSpace      = regexp.MustCompile(`[\s\x{00a0}\x{200b}\x{feff}]+`)
	reAppeared   = regexp.MustCompile(`(?i)\s*The post .{1,200} (appeared|first appeared) first on .{1,120}\.?$|\s*The post .{1,200} first appeared on .{1,120}\.?$`)
	reReadMore   = regexp.MustCompile(`(?i)\s*(\[?(read more|continue reading|more)\s*(…|\.\.\.|&hellip;|»|→)?\]?|\[\s*(…|\.\.\.)\s*\])\s*$`)
	reEllipsisBr = regexp.MustCompile(`\s*\[\s*(…|\.\.\.)\s*\]`)
)

// PlainText turns feed HTML into one line of plain text.
func PlainText(s string) string {
	if s == "" {
		return ""
	}
	for pass := 0; pass < 2; pass++ {
		s = reCDATA.ReplaceAllString(s, "$1")
		s = reComment.ReplaceAllString(s, " ")
		s = reBlock.ReplaceAllString(s, " ")
		s = reBreak.ReplaceAllString(s, " ")
		s = reTag.ReplaceAllString(s, "")
		s = html.UnescapeString(s)
		if !strings.Contains(s, "<") {
			break // no markup left (escaped HTML is stripped on the second pass)
		}
	}
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || (unicode.IsControl(r) && r != '\n' && r != '\t') {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
}

// Summarise makes the short plain-text summary stored with an item: markup
// and feed boilerplate removed, at most SummaryMax characters, cut at a word.
func Summarise(desc string) string {
	s := PlainText(desc)
	s = reAppeared.ReplaceAllString(s, "")
	s = reReadMore.ReplaceAllString(s, "")
	s = reEllipsisBr.ReplaceAllString(s, "…")
	return clipWords(strings.TrimSpace(s), SummaryMax)
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n]))
}

// clipWords cuts s to at most n characters at a word boundary, adding "…".
func clipWords(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)[:n-1]
	cut := len(r)
	for i := len(r) - 1; i > n*2/3; i-- {
		if unicode.IsSpace(r[i]) {
			cut = i
			break
		}
	}
	return strings.TrimRight(string(r[:cut]), " ,;:–—-") + "…"
}

// ---------- dates ----------

var feedDateLayouts = []string{
	time.RFC1123Z, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04 -0700", "Mon, 2 January 2006 15:04:05 -0700", "Mon, 2 Jan 06 15:04:05 -0700",
	"Monday, 2 January 2006 15:04:05 -0700", "Monday, 2 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 -0700", "2 Jan 2006 15:04 -0700", "2 January 2006 15:04:05 -0700",
	time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04Z07:00", "2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05Z07:00",
	"Mon, 2 Jan 2006 15:04:05", "2 Jan 2006 15:04:05", "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04",
	"Mon, 2 Jan 2006", "2 Jan 2006", "2 January 2006", "January 2, 2006", "Jan 2, 2006", "Monday, January 2, 2006", "2006-01-02", "2006/01/02",
}

// Time-zone abbreviations feeds use; Go only knows the local one, so they are
// rewritten to numeric offsets before parsing.
var zoneOffsets = map[string]string{
	"UT": "+0000", "UTC": "+0000", "GMT": "+0000", "Z": "+0000", "WET": "+0000",
	"EAT": "+0300", "CAT": "+0200", "SAST": "+0200", "WAT": "+0100", "EET": "+0200", "EEST": "+0300", "CET": "+0100", "CEST": "+0200", "BST": "+0100", "IST": "+0530",
	"AEST": "+1000", "AEDT": "+1100", "ACST": "+0930", "ACDT": "+1030", "AWST": "+0800", "NZST": "+1200", "NZDT": "+1300", "SGT": "+0800", "HKT": "+0800", "JST": "+0900",
	"EST": "-0500", "EDT": "-0400", "CST": "-0600", "CDT": "-0500", "MST": "-0700", "MDT": "-0600", "PST": "-0800", "PDT": "-0700",
}

var (
	reZoneAbbr  = regexp.MustCompile(`\s+([A-Za-z]{1,5})$`)
	reGMTOffset = regexp.MustCompile(`\s+(?:GMT|UTC)\s*([+-]\d{2}):?(\d{2})$`)
	reOrdinal   = regexp.MustCompile(`(\d)(st|nd|rd|th)\b`)
)

// ParseFeedDate reads the date formats found in real feeds (RFC 822 / 1123
// with numeric or named zones, RFC 3339, and looser variants). Dates without a
// zone are taken as UTC. ok is false for empty, unreadable or absurd dates.
func ParseFeedDate(raw string) (time.Time, bool) {
	s := strings.TrimSpace(reSpace.ReplaceAllString(raw, " "))
	if s == "" {
		return time.Time{}, false
	}
	s = strings.TrimSuffix(s, ".")
	s = reOrdinal.ReplaceAllString(s, "$1")
	if m := reGMTOffset.FindStringSubmatch(s); m != nil { // "… GMT+0300"
		s = reGMTOffset.ReplaceAllString(s, " "+m[1]+m[2])
	} else if m := reZoneAbbr.FindStringSubmatch(s); m != nil {
		if off, ok := zoneOffsets[strings.ToUpper(m[1])]; ok && !isMonthOrDay(m[1]) {
			s = s[:len(s)-len(m[0])] + " " + off
		}
	}
	for _, l := range feedDateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			if t.Year() < 1995 || t.Year() > 2100 {
				return time.Time{}, false
			}
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func isMonthOrDay(s string) bool {
	switch strings.ToLower(s) {
	case "jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "sept", "oct", "nov", "dec", "mon", "tue", "wed", "thu", "fri", "sat", "sun":
		return true
	}
	return false
}

// ---------- identity ----------

var trackingParams = map[string]bool{"fbclid": true, "gclid": true, "mc_cid": true, "mc_eid": true, "ref": true, "source": true, "cmpid": true, "ocid": true, "igshid": true}

// CanonicalLink normalises an article address so the same article reached
// through two feeds (or with tracking parameters) is stored once.
func CanonicalLink(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(link)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	u.Fragment = ""
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || trackingParams[lk] {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	if u.Path != "/" {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	return u.String()
}

// ItemHash is the de-duplication key of an item: its canonical link, or its
// guid when the feed gives no link.
func ItemHash(it FeedItem) string {
	key := ""
	if strings.TrimSpace(it.Link) != "" {
		key = "l:" + CanonicalLink(it.Link)
	} else {
		key = "g:" + strings.TrimSpace(it.GUID)
	}
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:20])
}

// ResolveLinks makes relative item links absolute against the feed address.
func ResolveLinks(f *ParsedFeed, feedURL string) {
	base, err := url.Parse(feedURL)
	if err != nil {
		return
	}
	abs := func(l string) string {
		l = strings.TrimSpace(l)
		if l == "" {
			return ""
		}
		u, err := url.Parse(l)
		if err != nil {
			return ""
		}
		r := base.ResolveReference(u)
		if r.Scheme != "https" && r.Scheme != "http" {
			return "" // javascript:, data:, mailto: … are never stored
		}
		return r.String()
	}
	f.Link = abs(f.Link)
	keep := f.Items[:0]
	for _, it := range f.Items {
		it.Link = abs(it.Link)
		if it.Link == "" && it.GUID == "" {
			continue
		}
		keep = append(keep, it)
	}
	f.Items = keep
}

// sniffFeed reports whether the first bytes of a body look like XML at all
// (so an HTML page gets a clear message instead of a parser error).
func sniffFeed(head []byte) bool {
	h := bytes.ToLower(bytes.TrimLeft(head, " \t\r\n\xef\xbb\xbf"))
	if bytes.HasPrefix(h, []byte("<!doctype html")) || bytes.HasPrefix(h, []byte("<html")) {
		return false
	}
	return bytes.HasPrefix(h, []byte("<"))
}
