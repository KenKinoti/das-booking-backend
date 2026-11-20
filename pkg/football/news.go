package football

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"
)

// News sources (free; only titles, short snippets, links and dates are
// stored — never full articles):
//   Google News RSS search: GET https://news.google.com/rss/search?q=<query>&hl=<hl>&gl=<gl>&ceid=<gl>:<lang>
//     RSS 2.0 items: <title>Headline - Source</title><link>…</link><pubDate>RFC1123</pubDate><description>HTML</description><source url="https://site">Site</source>
//   GDELT DOC 2.0 (https://blog.gdeltproject.org/gdelt-doc-2-0-api-debuts/): GET https://api.gdeltproject.org/api/v2/doc/doc?query=<q incl. sourcelang:polish>&mode=artlist&format=json&maxrecords=50&timespan=7d&sort=datedesc
//     {"articles":[{"url","url_mobile","title","seendate":"20261119T143000Z","socialimage","domain","language":"Polish","sourcecountry"}]}
//   RSS 2.0 / Atom feeds configured by a super admin (club sites, league sites, local press).

// RawNews is a parsed item before storage.
type RawNews struct {
	Title       string
	Snippet     string
	URL         string
	Source      string
	Domain      string
	PublishedAt time.Time
	Language    string
	Provider    string
}

type rssDoc struct {
	XMLName xml.Name
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			GUID        string `xml:"guid"`
			PubDate     string `xml:"pubDate"`
			Date        string `xml:"date"` // dc:date
			Description string `xml:"description"`
			Source      struct {
				URL  string `xml:"url,attr"`
				Name string `xml:",chardata"`
			} `xml:"source"`
		} `xml:"item"`
	} `xml:"channel"`
	// Atom.
	Entries []struct {
		Title string `xml:"title"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
		Updated   string `xml:"updated"`
		Published string `xml:"published"`
		Summary   string `xml:"summary"`
		Content   string `xml:"content"`
	} `xml:"entry"`
	Title string `xml:"title"`
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// plainText strips HTML and entities and clips to n characters.
func plainText(s string, n int) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reMultiSp.ReplaceAllString(s, " ")
	return clip(strings.TrimSpace(s), n)
}

func parseFeedTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, f := range []string{time.RFC1123Z, time.RFC1123, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", time.RFC3339, "2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04:05", "20060102T150405Z", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func hostOf(u string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil || p.Host == "" {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(p.Hostname()), "www.")
}

// ParseRSS reads RSS 2.0 or Atom (Google News RSS included: the source site
// comes from <source url> and the " - Source" title suffix is removed).
func ParseRSS(body []byte, provider, lang string) ([]RawNews, error) {
	var d rssDoc
	if err := xml.Unmarshal(body, &d); err != nil {
		// Lenient second try (HTML entities such as &nbsp;, sloppy feeds).
		// No HTML auto-closing: <link> is a real element in RSS.
		d = rssDoc{}
		dec := xml.NewDecoder(strings.NewReader(string(body)))
		dec.Strict = false
		dec.Entity = xml.HTMLEntity
		if err := dec.Decode(&d); err != nil {
			return nil, fmt.Errorf("not an RSS/Atom feed: %w", err)
		}
	}
	if d.XMLName.Local != "rss" && d.XMLName.Local != "feed" && d.XMLName.Local != "RDF" {
		return nil, fmt.Errorf("not an RSS/Atom feed (<%s>)", d.XMLName.Local)
	}
	out := []RawNews{}
	for _, it := range d.Channel.Items {
		link := strings.TrimSpace(it.Link)
		if link == "" {
			link = strings.TrimSpace(it.GUID)
		}
		title := plainText(it.Title, 300)
		src := strings.TrimSpace(it.Source.Name)
		dom := hostOf(it.Source.URL)
		if src != "" && strings.HasSuffix(title, " - "+src) {
			title = strings.TrimSpace(strings.TrimSuffix(title, " - "+src))
		}
		if dom == "" {
			dom = hostOf(link)
		}
		if src == "" {
			src = dom
		}
		t := parseFeedTime(it.PubDate)
		if t.IsZero() {
			t = parseFeedTime(it.Date)
		}
		snip := plainText(it.Description, 300)
		if provider == ProviderGNews {
			// Google's description repeats the headline and source.
			if strings.HasPrefix(snip, title) {
				snip = ""
			}
		}
		if title == "" || link == "" {
			continue
		}
		out = append(out, RawNews{Title: title, Snippet: snip, URL: link, Source: clip(src, 160), Domain: dom, PublishedAt: t, Language: lang, Provider: provider})
	}
	for _, e := range d.Entries {
		link := ""
		for _, l := range e.Links {
			if l.Rel == "" || l.Rel == "alternate" {
				link = l.Href
				break
			}
		}
		if link == "" && len(e.Links) > 0 {
			link = e.Links[0].Href
		}
		t := parseFeedTime(e.Published)
		if t.IsZero() {
			t = parseFeedTime(e.Updated)
		}
		snip := e.Summary
		if snip == "" {
			snip = e.Content
		}
		title := plainText(e.Title, 300)
		if title == "" || link == "" {
			continue
		}
		dom := hostOf(link)
		out = append(out, RawNews{Title: title, Snippet: plainText(snip, 300), URL: link, Source: firstNonEmpty(plainText(d.Title, 160), dom), Domain: dom, PublishedAt: t, Language: lang, Provider: provider})
	}
	return out, nil
}

// gdeltLang maps GDELT's language names to codes.
var gdeltLang = map[string]string{"english": "en", "polish": "pl", "german": "de", "dutch": "nl", "danish": "da", "romanian": "ro", "french": "fr", "swahili": "sw",
	"spanish": "es", "italian": "it", "portuguese": "pt"}

// gdeltSourceLang is GDELT's sourcelang: operator value per code.
var gdeltSourceLang = map[string]string{"en": "english", "pl": "polish", "de": "german", "nl": "dutch", "da": "danish", "ro": "romanian", "fr": "french", "sw": "swahili"}

// ParseGDELT reads a DOC 2.0 ArtList JSON response.
func ParseGDELT(body []byte) ([]RawNews, error) {
	var d struct {
		Articles []struct {
			URL      string `json:"url"`
			Title    string `json:"title"`
			SeenDate string `json:"seendate"`
			Domain   string `json:"domain"`
			Language string `json:"language"`
		} `json:"articles"`
	}
	b := strings.TrimSpace(string(body))
	if b == "" || b == "{}" {
		return []RawNews{}, nil
	}
	if err := json.Unmarshal([]byte(b), &d); err != nil {
		return nil, fmt.Errorf("unexpected GDELT response: %w", err)
	}
	out := []RawNews{}
	for _, a := range d.Articles {
		if a.URL == "" || a.Title == "" {
			continue
		}
		dom := strings.TrimPrefix(strings.ToLower(a.Domain), "www.")
		if dom == "" {
			dom = hostOf(a.URL)
		}
		out = append(out, RawNews{Title: plainText(a.Title, 300), URL: a.URL, Source: dom, Domain: dom, PublishedAt: parseFeedTime(a.SeenDate),
			Language: gdeltLang[strings.ToLower(a.Language)], Provider: ProviderGDELT})
	}
	return out, nil
}

// canonicalURL drops tracking parameters and fragments (dedupe by link).
func canonicalURL(u string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil {
		return strings.TrimSpace(u)
	}
	p.Fragment = ""
	q := p.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || lk == "fbclid" || lk == "gclid" || lk == "oc" {
			q.Del(k)
		}
	}
	p.RawQuery = q.Encode()
	p.Host = strings.ToLower(p.Host)
	return p.String()
}

func urlKey(u string) string {
	h := sha256.Sum256([]byte(canonicalURL(u)))
	return hex.EncodeToString(h[:16])
}

// titleKey is a near-duplicate key of a headline.
func titleKey(t string) string {
	ws := tokenize(t)
	if len(ws) > 14 {
		ws = ws[:14]
	}
	return clip(strings.Join(ws, " "), 300)
}

// nearDuplicate: token overlap ≥ 0.85 between two headlines.
func nearDuplicate(a, b string) bool {
	ta, tb := tokenize(a), tokenize(b)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	set := map[string]bool{}
	for _, t := range ta {
		set[t] = true
	}
	inter := 0
	for _, t := range tb {
		if set[t] {
			inter++
		}
	}
	return float64(inter)/float64(maxInt(len(ta), len(tb))) >= 0.85
}

// newsTeam is a team news is fetched for.
type newsTeam struct {
	ID      int64
	Name    string
	Country string
}

// storeNews dedupes and stores items for a team (or a feed's competition),
// extracts facts and returns (new items, facts touched).
func (s *Service) storeNews(raws []RawNews, teams []newsTeam, requireMention bool) (int, int) {
	if len(raws) == 0 {
		return 0, 0
	}
	now := s.Now()
	rules := s.newsRules()
	squads := map[int64][]SquadPlayer{}
	added, facts := 0, 0
	for _, r := range raws {
		if r.PublishedAt.IsZero() {
			r.PublishedAt = now
		}
		if r.PublishedAt.Before(now.AddDate(0, 0, -21)) {
			continue // old news is not team news
		}
		key := urlKey(r.URL)
		text := r.Title + ". " + r.Snippet
		lang := DetectLanguage(text, r.Language)
		var matched []newsTeam
		for _, t := range teams {
			if !requireMention || teamMentioned(text, t.Name) {
				matched = append(matched, t)
			}
		}
		if len(matched) == 0 {
			continue
		}
		var item NewsItem
		exists := s.DB.Where("url_key = ?", key).Limit(1).Find(&item).RowsAffected > 0
		if !exists {
			// Near-identical headline from the same site (or syndicated
			// copies anywhere) within 3 days: not a new, independent item.
			var near []NewsItem
			tk := titleKey(r.Title)
			s.DB.Where("published_at > ? AND (title_key = ? OR domain = ?)", r.PublishedAt.Add(-72*time.Hour), tk, r.Domain).Limit(200).Find(&near)
			dup := false
			for _, n := range near {
				if n.TitleKey == tk || nearDuplicate(n.Title, r.Title) {
					dup = true
					item = n
					break
				}
			}
			if dup {
				exists = true
			} else {
				item = NewsItem{ID: uuid.NewString(), URL: clip(r.URL, 2000), URLKey: key, TitleKey: tk, Title: r.Title, Snippet: r.Snippet, Source: r.Source,
					Domain: clip(r.Domain, 160), PublishedAt: r.PublishedAt, Language: lang, Provider: r.Provider, Keywords: []string{}, Statuses: []string{},
					TeamIDs: []int64{}, Players: []string{}, FactIDs: []string{}, FetchedAt: now}
			}
		}
		item.Postponed = item.Postponed || containsAny(tokenize(text), postponedWords)
		for _, t := range matched {
			if _, ok := squads[t.ID]; !ok {
				squads[t.ID] = s.squadOf(t.ID)
			}
			cands, hits := ExtractFacts(text, lang, t.ID, squads[t.ID], rules)
			for _, h := range hits {
				item.Keywords = addUniq(item.Keywords, h.Keyword)
				item.Statuses = addUniq(item.Statuses, h.Status)
			}
			for _, c := range cands {
				if c.Player != "" {
					item.Players = addUniq(item.Players, c.Player)
				}
			}
			if !containsInt(item.TeamIDs, t.ID) {
				item.TeamIDs = append(item.TeamIDs, t.ID)
			}
			if !exists {
				// facts need the item stored first
			}
			if len(cands) > 0 {
				if !exists {
					s.DB.Create(&item)
					exists = true
					added++
				}
				for _, id := range s.recordFacts(&item, cands) {
					item.FactIDs = addUniq(item.FactIDs, id)
					facts++
				}
			}
		}
		if !exists {
			s.DB.Create(&item)
			added++
		} else {
			s.DB.Model(&NewsItem{}).Where("id = ?", item.ID).Updates(map[string]any{"keywords": mustJSON(item.Keywords), "statuses": mustJSON(item.Statuses),
				"team_ids": mustJSON(item.TeamIDs), "players": mustJSON(item.Players), "fact_ids": mustJSON(item.FactIDs), "postponed": item.Postponed})
		}
		for _, t := range item.TeamIDs {
			s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&NewsTeam{NewsID: item.ID, TeamID: t})
		}
	}
	return added, facts
}

func containsAny(words []string, kws []string) bool {
	for _, k := range kws {
		if phraseIn(words, k) {
			return true
		}
	}
	return false
}

func containsInt(xs []int64, x int64) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// newsLangOf picks the news language and Google edition of a team's country.
func newsLangOf(country string) (lang, hl, gl string) {
	switch country {
	case "Poland":
		return "pl", "pl", "PL"
	case "Germany":
		return "de", "de", "DE"
	case "Austria":
		return "de", "de", "AT"
	case "Switzerland":
		return "de", "de", "CH"
	case "Netherlands":
		return "nl", "nl", "NL"
	case "Belgium":
		return "nl", "nl", "BE"
	case "Denmark":
		return "da", "da", "DK"
	case "Romania":
		return "ro", "ro", "RO"
	case "France":
		return "fr", "fr", "FR"
	case "Kenya", "Tanzania", "Uganda":
		return "en", "en-KE", "KE"
	}
	return "en", "", ""
}

// fetchGoogleNews runs one Google News RSS search for a team.
func (s *Service) fetchGoogleNews(ctx context.Context, t newsTeam, english bool, prio int, task string) (int, int, error) {
	st := s.FreeSettingsGet()
	lang, hl, gl := newsLangOf(t.Country)
	ceid := ""
	if english || hl == "" {
		lang, hl, gl, ceid = "en", st.NewsGoogle["hl"], st.NewsGoogle["gl"], st.NewsGoogle["ceid"]
	} else {
		ceid = gl + ":" + lang
	}
	q := fmt.Sprintf("\"%s\" when:7d", t.Name)
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderGNews, Path: "/rss/search", Prio: prio, Task: task,
		Params: url.Values{"q": {q}, "hl": {hl}, "gl": {gl}, "ceid": {ceid}}, Accept: "application/rss+xml, application/xml"})
	if err != nil {
		return 0, 0, err
	}
	items, err := ParseRSS(out.Body, ProviderGNews, lang)
	if err != nil {
		return 0, 0, err
	}
	s.setResults(ProviderGNews, task, len(items))
	a, f := s.storeNews(items, []newsTeam{t}, true)
	return a, f, nil
}

// fetchGDELT runs one GDELT DOC 2.0 search for a team.
func (s *Service) fetchGDELT(ctx context.Context, t newsTeam, prio int, task string) (int, int, error) {
	lang, _, _ := newsLangOf(t.Country)
	q := fmt.Sprintf("\"%s\"", t.Name)
	if sl, ok := gdeltSourceLang[lang]; ok && lang != "en" {
		q += " (sourcelang:" + sl + " OR sourcelang:english)"
	}
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderGDELT, Path: "/api/v2/doc/doc", Prio: prio, Task: task,
		Params: url.Values{"query": {q}, "mode": {"artlist"}, "format": {"json"}, "maxrecords": {"50"}, "timespan": {"7d"}, "sort": {"datedesc"}}})
	if err != nil {
		return 0, 0, err
	}
	items, err := ParseGDELT(out.Body)
	if err != nil {
		return 0, 0, err
	}
	s.setResults(ProviderGDELT, task, len(items))
	a, f := s.storeNews(items, []newsTeam{t}, true)
	return a, f, nil
}

// fetchFeed reads one configured feed.
func (s *Service) fetchFeed(ctx context.Context, fd NewsFeed, prio int, task string) (int, int, error) {
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderFeeds, Path: fd.URL, Prio: prio, Task: task, Accept: "application/rss+xml, application/atom+xml, application/xml, text/xml"})
	now := s.Now()
	if err != nil {
		s.DB.Model(&NewsFeed{}).Where("id = ?", fd.ID).Updates(map[string]any{"last_fetched_at": now, "last_error": clip(err.Error(), 300)})
		return 0, 0, err
	}
	items, err := ParseRSS(out.Body, ProviderFeeds, fd.Language)
	if err != nil {
		s.DB.Model(&NewsFeed{}).Where("id = ?", fd.ID).Updates(map[string]any{"last_fetched_at": now, "last_error": clip(err.Error(), 300)})
		s.markLogError(ProviderFeeds, task, "unreadable: "+err.Error())
		return 0, 0, err
	}
	s.setResults(ProviderFeeds, task, len(items))
	var teams []newsTeam
	require := true
	switch {
	case fd.TeamID != 0:
		teams = []newsTeam{{ID: fd.TeamID, Name: s.teamName(fd.TeamID)}}
		require = false // a club's own feed is about the club
	case fd.CompetitionID != 0:
		for _, t := range s.teamsOfComp(fd.CompetitionID) {
			teams = append(teams, newsTeam{ID: t.ID, Name: t.Name})
		}
	}
	a, f := 0, 0
	if len(teams) > 0 {
		a, f = s.storeNews(items, teams, require)
	}
	s.DB.Model(&NewsFeed{}).Where("id = ?", fd.ID).Updates(map[string]any{"last_fetched_at": now, "last_error": "", "items": gormExpr("items + ?", a)})
	return a, f, nil
}

// NewsOut is a news item in a payload.
type NewsOut struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Snippet         string    `json:"snippet"`
	Source          string    `json:"source"`
	Domain          string    `json:"domain"`
	URL             string    `json:"url"`
	PublishedAt     time.Time `json:"published_at"`
	Language        string    `json:"language"`
	Provider        string    `json:"provider"`
	MatchedKeywords []string  `json:"matched_keywords"`
	Statuses        []string  `json:"statuses"`
	TeamIDs         []int64   `json:"team_ids"`
	Players         []string  `json:"players"`
	FactIDs         []string  `json:"fact_ids"`
	FetchedAt       time.Time `json:"fetched_at"`
}

func newsOut(n NewsItem) NewsOut {
	o := NewsOut{ID: n.ID, Title: n.Title, Snippet: n.Snippet, Source: n.Source, Domain: n.Domain, URL: n.URL, PublishedAt: n.PublishedAt, Language: n.Language,
		Provider: n.Provider, MatchedKeywords: nonNil(n.Keywords), Statuses: nonNil(n.Statuses), TeamIDs: n.TeamIDs, Players: nonNil(n.Players), FactIDs: nonNil(n.FactIDs), FetchedAt: n.FetchedAt}
	if o.TeamIDs == nil {
		o.TeamIDs = []int64{}
	}
	return o
}

// NewsFor lists news of teams (newest first).
func (s *Service) NewsFor(teams []int64, q string, limit int) ([]NewsOut, int64) {
	if limit <= 0 || limit > 200 {
		limit = 40
	}
	tx := s.DB.Model(&NewsItem{}).Where("id IN (SELECT news_id FROM fb_news_teams WHERE team_id IN ?)", append(teams, -1)).Where("published_at > ?", s.Now().AddDate(0, 0, -21))
	if t := strings.TrimSpace(q); t != "" {
		tx = tx.Where("LOWER(title) LIKE ?", "%"+strings.ToLower(t)+"%")
	}
	var n int64
	tx.Count(&n)
	var rows []NewsItem
	tx.Order("published_at DESC").Limit(limit).Find(&rows)
	out := []NewsOut{}
	for _, r := range rows {
		out = append(out, newsOut(r))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PublishedAt.After(out[j].PublishedAt) })
	return out, n
}
