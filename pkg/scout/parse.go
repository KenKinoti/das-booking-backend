package scout

import (
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// The model's answer is data: it is parsed into fixed fields, every string is
// cleaned and clipped, and nothing in it is ever executed or rendered as HTML.

// rawSource / rawOpp / rawSignal are the answer as the model wrote it. Numbers
// may come as strings, so loose types are used.
type rawSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type rawOpp struct {
	Title      string          `json:"title"`
	Type       string          `json:"type"`
	Buyer      string          `json:"buyer"`
	Summary    string          `json:"summary"`
	WhyItFits  string          `json:"why_it_fits"`
	EstValue   json.RawMessage `json:"est_value"`
	Currency   string          `json:"currency"`
	Deadline   json.RawMessage `json:"deadline"`
	Country    string          `json:"country"`
	Location   string          `json:"location"`
	Effort     string          `json:"effort"`
	Confidence json.RawMessage `json:"confidence"`
	NextStep   string          `json:"next_step"`
	Sources    []rawSource     `json:"sources"`
}

type rawSignal struct {
	Market  string      `json:"market"`
	Country string      `json:"country"`
	Sector  string      `json:"sector"`
	Signal  string      `json:"signal"`
	SoWhat  string      `json:"so_what"`
	Sources []rawSource `json:"sources"`
}

type rawAnswer struct {
	Opportunities []rawOpp    `json:"opportunities"`
	Signals       []rawSignal `json:"market_signals"`
}

// ErrNoJSON means the answer had no readable JSON object.
var ErrNoJSON = errors.New("the AI answer did not contain the expected JSON")

var reFence = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

// ExtractAnswer finds the JSON object in the model's text: a ```json fence,
// else the first {...} that decodes to an object with "opportunities" or
// "market_signals".
func ExtractAnswer(text string) (*rawAnswer, error) {
	try := func(s string) *rawAnswer {
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(s), &probe) != nil {
			return nil
		}
		_, a := probe["opportunities"]
		_, b := probe["market_signals"]
		if !a && !b {
			return nil
		}
		var ans rawAnswer
		// Field by field so one malformed list does not lose the other.
		if v, ok := probe["opportunities"]; ok {
			var list []json.RawMessage
			if json.Unmarshal(v, &list) == nil {
				for _, it := range list {
					var o rawOpp
					if json.Unmarshal(it, &o) == nil {
						ans.Opportunities = append(ans.Opportunities, o)
					} else {
						ans.Opportunities = append(ans.Opportunities, rawOpp{}) // counted as invalid
					}
				}
			}
		}
		if v, ok := probe["market_signals"]; ok {
			var list []json.RawMessage
			if json.Unmarshal(v, &list) == nil {
				for _, it := range list {
					var s rawSignal
					if json.Unmarshal(it, &s) == nil {
						ans.Signals = append(ans.Signals, s)
					}
				}
			}
		}
		return &ans
	}
	for _, m := range reFence.FindAllStringSubmatch(text, -1) {
		if a := try(m[1]); a != nil {
			return a, nil
		}
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			continue
		}
		if a := try(string(v)); a != nil {
			return a, nil
		}
	}
	return nil, ErrNoJSON
}

// ---------------------------------------------------------------- text

var reSpace = regexp.MustCompile(`\s+`)

// cleanText drops control characters and markup-looking angle brackets and
// collapses white space.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case unicode.IsControl(r), r == '\u200b', r == '\ufeff':
			return -1
		case r == '<':
			return '‹'
		case r == '>':
			return '›'
		}
		return r
	}, s)
	return strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// ---------------------------------------------------------------- URLs

var trackingParams = map[string]bool{"fbclid": true, "gclid": true, "mc_cid": true, "mc_eid": true, "ref": true, "ref_src": true, "igshid": true}

// NormURL is the comparable form of a URL: http(s) only, lower-case host
// without "www.", no fragment, no tracking parameters, no trailing slash.
// "" when the URL is not a usable http(s) link.
func NormURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2000 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	if host == "" || !strings.Contains(host, ".") {
		return ""
	}
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || trackingParams[strings.ToLower(k)] {
			q.Del(k)
		}
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var qs []string
	for _, k := range keys {
		for _, v := range q[k] {
			qs = append(qs, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	p := strings.TrimRight(u.EscapedPath(), "/")
	out := host + p
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		out = host + ":" + port + p
	}
	if len(qs) > 0 {
		out += "?" + strings.Join(qs, "&")
	}
	return out
}

// safeURL is a URL fit to show as a link (http/https only).
func safeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	return u.String()
}

func domainOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// CleanDomain turns "https://www.tenders.go.ke/path" or "tenders.go.ke" into
// the bare form the web search tool accepts ("tenders.go.ke/path").
func CleanDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimPrefix(s, "www.")
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	host := s
	if i := strings.IndexByte(s, '/'); i >= 0 {
		host = s[:i]
	}
	if !reHost.MatchString(host) {
		return ""
	}
	return s
}

var reHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// verifySources keeps the sources whose URL appeared in the search results
// (or the answer's own citations, which point at results), at most 6.
func verifySources(in []rawSource, ex *Exchange) []SourceRef {
	out := []SourceRef{}
	seen := map[string]bool{}
	for _, s := range in {
		n := NormURL(s.URL)
		if n == "" || seen[n] {
			continue
		}
		res, ok := ex.Results[n]
		if !ok && !ex.Cited[n] {
			continue
		}
		seen[n] = true
		link := safeURL(s.URL)
		if ok && safeURL(res.URL) != "" {
			link = safeURL(res.URL) // the URL exactly as the search returned it
		}
		if link == "" {
			continue
		}
		title := clip(cleanText(s.Title), 200)
		if title == "" {
			title = res.Title
		}
		if title == "" {
			title = domainOf(link)
		}
		out = append(out, SourceRef{Title: title, URL: link, Domain: domainOf(link), PageAge: res.PageAge})
		if len(out) == 6 {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------- enums

func normType(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.NewReplacer("-", "_", " ", "_", "/", "_").Replace(t)
	switch t {
	case TypeTender, TypeGrant, TypePartnership, TypeLead, TypeNewMarket, TypeProduct:
		return t
	}
	switch {
	case strings.Contains(t, "tender"), strings.Contains(t, "rfp"), strings.Contains(t, "rfq"), strings.Contains(t, "eoi"),
		strings.Contains(t, "procure"), strings.Contains(t, "bid"), strings.Contains(t, "expression_of_interest"):
		return TypeTender
	case strings.Contains(t, "grant"), strings.Contains(t, "fund"), strings.Contains(t, "call_for_proposal"), strings.Contains(t, "award"):
		return TypeGrant
	case strings.Contains(t, "partner"), strings.Contains(t, "reseller"), strings.Contains(t, "alliance"):
		return TypePartnership
	case strings.Contains(t, "lead"), strings.Contains(t, "prospect"), strings.Contains(t, "client"), strings.Contains(t, "customer"):
		return TypeLead
	case strings.Contains(t, "market"), strings.Contains(t, "expansion"), strings.Contains(t, "country"):
		return TypeNewMarket
	case strings.Contains(t, "product"), strings.Contains(t, "service"), strings.Contains(t, "offering"):
		return TypeProduct
	}
	return ""
}

func level(s string) string {
	switch t := strings.ToLower(strings.TrimSpace(s)); {
	case strings.HasPrefix(t, "low"), t == "small", t == "easy":
		return "low"
	case strings.HasPrefix(t, "med"), t == "moderate", t == "mid":
		return "medium"
	case strings.HasPrefix(t, "high"), t == "large", t == "hard":
		return "high"
	}
	return ""
}

// confidenceOf accepts "high" / "medium" / "low" or a number 0–1 (or 0–100).
func confidenceOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if l := level(s); l != "" {
			return l
		}
		if f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64); err == nil {
			return confNum(f)
		}
		return ""
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return confNum(f)
	}
	return ""
}

func confNum(f float64) string {
	if f > 1 {
		f /= 100
	}
	switch {
	case f >= 0.7:
		return "high"
	case f >= 0.4:
		return "medium"
	case f > 0:
		return "low"
	}
	return ""
}

// countryCode accepts an ISO alpha-2 code or a country name.
func countryCode(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) == 2 {
		if c := strings.ToUpper(s); currency.IsCountry(c) {
			return c
		}
	}
	return currency.CountryCode(s)
}

// ---------------------------------------------------------------- money

var currencySymbols = []struct{ sym, code string }{
	{"US$", "USD"}, {"USD", "USD"}, {"AU$", "AUD"}, {"A$", "AUD"}, {"AUD", "AUD"}, {"KSHS", "KES"}, {"KSH", "KES"}, {"KES", "KES"},
	{"EUR", "EUR"}, {"€", "EUR"}, {"GBP", "GBP"}, {"£", "GBP"}, {"UGX", "UGX"}, {"TZS", "TZS"}, {"RWF", "RWF"}, {"NZ$", "NZD"}, {"$", "USD"},
}

var reNumber = regexp.MustCompile(`(\d[\d,\s]*(?:\.\d+)?)\s*(k|m|mn|million|bn|b|billion|thousand)?\b`)

// MaxValue bounds an estimated value (anything larger is not believable for
// the businesses this serves and is dropped as noise).
const MaxValue = 1e11

// parseValue reads a number or a money string ("KES 2.5M", "US$50,000",
// "50k"); it returns the value and a currency found in the string.
func parseValue(raw json.RawMessage) (*float64, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return saneValue(f), ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, ""
	}
	up := strings.ToUpper(strings.TrimSpace(s))
	cur := ""
	for _, c := range currencySymbols {
		if strings.Contains(up, c.sym) {
			cur = c.code
			break
		}
	}
	m := reNumber.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return nil, cur
	}
	n, err := strconv.ParseFloat(strings.NewReplacer(",", "", " ", "").Replace(m[1]), 64)
	if err != nil {
		return nil, cur
	}
	switch m[2] {
	case "k", "thousand":
		n *= 1e3
	case "m", "mn", "million":
		n *= 1e6
	case "b", "bn", "billion":
		n *= 1e9
	}
	return saneValue(n), cur
}

func saneValue(f float64) *float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > MaxValue {
		return nil
	}
	v := math.Round(f*100) / 100
	return &v
}

func normCurrency(s, fallback string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) == 3 && currency.IsSupported(s) {
		return s
	}
	for _, c := range currencySymbols {
		if s == c.sym {
			return c.code
		}
	}
	return fallback
}

// ---------------------------------------------------------------- dates

var dateLayouts = []string{
	"2006-01-02", "2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04",
	"2 January 2006", "2 Jan 2006", "02 January 2006", "02 Jan 2006", "January 2, 2006", "Jan 2, 2006", "January 2 2006", "Jan 2 2006",
	"Monday, 2 January 2006", "Monday 2 January 2006", "Mon, 2 Jan 2006", "2006/01/02",
}

var reOrdinal = regexp.MustCompile(`(\d)(st|nd|rd|th)\b`)
var reDMY = regexp.MustCompile(`^(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{4})$`)

// parseDeadline reads a deadline: a date string (ISO, "5 December 2026",
// "December 5, 2026", "05/12/2026" read day first as in Kenya and Australia)
// or null / "rolling" / "open". The text form is kept for display when it is
// not a date.
func parseDeadline(raw json.RawMessage) (*time.Time, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, ""
	}
	s = cleanText(s)
	if s == "" {
		return nil, ""
	}
	t := reOrdinal.ReplaceAllString(s, "$1")
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(t, "(EAT)"), "EAT"))
	for _, l := range dateLayouts {
		if d, err := time.Parse(l, t); err == nil {
			dd := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
			return &dd, ""
		}
	}
	if m := reDMY.FindStringSubmatch(t); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		day, mon := a, b
		if b > 12 && a <= 12 { // 12/25/2026 can only be month first
			day, mon = b, a
		}
		if mon >= 1 && mon <= 12 && day >= 1 && day <= 31 {
			d := time.Date(y, time.Month(mon), day, 0, 0, 0, 0, time.UTC)
			if d.Day() == day {
				return &d, ""
			}
		}
	}
	return nil, clip(s, 80)
}

// ---------------------------------------------------------------- validation

// Candidate is a validated opportunity from one answer (not stored yet).
type Candidate struct {
	Opportunity
}

// SignalCand is a validated market signal.
type SignalCand struct {
	Signal
}

// Stats counts what validation dropped.
type Stats struct {
	Found, Unverified, Expired, Duplicates, Invalid, Filtered int
	SignalsFound                                              int
}

// Validate turns the answer into candidates: required fields present,
// sources verified against the search results, dates parsed (past deadlines
// dropped), values sane, duplicates within the answer removed.
func Validate(ans *rawAnswer, ex *Exchange, today time.Time, defCur string) ([]Candidate, []SignalCand, Stats) {
	st := Stats{Found: len(ans.Opportunities), SignalsFound: len(ans.Signals)}
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	out := []Candidate{}
	for _, o := range ans.Opportunities {
		title := clip(cleanText(o.Title), 280)
		typ := normType(o.Type)
		if len([]rune(title)) < 6 || typ == "" {
			st.Invalid++
			continue
		}
		srcs := verifySources(o.Sources, ex)
		if len(srcs) == 0 {
			st.Unverified++
			continue
		}
		dl, dlText := parseDeadline(o.Deadline)
		if dl != nil {
			if dl.Before(today) {
				st.Expired++
				continue
			}
			if dl.After(today.AddDate(5, 0, 0)) { // not a believable deadline
				dl, dlText = nil, ""
			}
		}
		val, valCur := parseValue(o.EstValue)
		cur := normCurrency(o.Currency, valCur)
		if val == nil {
			cur = normCurrency(o.Currency, "")
		} else if cur == "" {
			cur = defCur
		}
		c := Candidate{Opportunity{
			Type: typ, Title: title, TitleKey: titleKey(title), Buyer: clip(cleanText(o.Buyer), 200), Summary: clip(cleanText(o.Summary), 1200),
			WhyItFits: clip(cleanText(o.WhyItFits), 800), EstValue: val, Currency: cur, Deadline: dl, DeadlineText: dlText,
			Country: countryCode(o.Country), Location: clip(cleanText(o.Location), 160), Effort: level(o.Effort),
			Confidence: confidenceOf(o.Confidence), NextStep: clip(cleanText(o.NextStep), 600), Sources: srcs,
		}}
		if c.Country == "" && c.Location != "" {
			// "Nairobi, Kenya" → KE
			parts := strings.Split(c.Location, ",")
			c.Country = countryCode(parts[len(parts)-1])
		}
		dup := false
		for i := range out {
			if SameOpportunity(&out[i].Opportunity, &c.Opportunity) {
				dup = true
				// Keep the richer one's sources together.
				out[i].Sources = mergeSources(out[i].Sources, c.Sources)
				break
			}
		}
		if dup {
			st.Duplicates++
			continue
		}
		out = append(out, c)
	}
	sigs := []SignalCand{}
	for _, s := range ans.Signals {
		text := clip(cleanText(s.Signal), 600)
		if len([]rune(text)) < 8 {
			continue
		}
		srcs := verifySources(s.Sources, ex)
		if len(srcs) == 0 {
			continue
		}
		sc := SignalCand{Signal{Market: clip(cleanText(s.Market), 160), Country: countryCode(s.Country), Sector: clip(cleanText(s.Sector), 120),
			Signal: text, SignalKey: titleKey(text), SoWhat: clip(cleanText(s.SoWhat), 600), Sources: srcs}}
		if sc.Country == "" && sc.Market != "" {
			for _, p := range strings.FieldsFunc(sc.Market, func(r rune) bool { return r == ',' || r == '—' || r == '-' || r == '–' || r == '/' }) {
				if cc := countryCode(p); cc != "" {
					sc.Country = cc
					break
				}
			}
		}
		dup := false
		for _, x := range sigs {
			if x.Country == sc.Country && similar(x.SignalKey, sc.SignalKey) >= 0.8 {
				dup = true
				break
			}
		}
		if !dup {
			sigs = append(sigs, sc)
		}
	}
	return out, sigs, st
}

func mergeSources(a, b []SourceRef) []SourceRef {
	seen := map[string]bool{}
	out := []SourceRef{}
	for _, s := range append(append([]SourceRef{}, a...), b...) {
		n := NormURL(s.URL)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, s)
		if len(out) == 6 {
			break
		}
	}
	return out
}
