package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Industry watch: news and developments from RSS / Atom sources, stored in the
// database with history (insights_watch_feeds, insights_watch_items), tagged
// and scored with the organisation's keyword rules.

// WatchRetentionDays is how long items are kept (saved items are kept for good).
const WatchRetentionDays = 180

// MaxWatchFeeds is the number of sources one organisation may follow.
const MaxWatchFeeds = 60

// Intel holds the dependencies of the industry watch, the trade & export view
// and the strategy brief. It extends Service (same database, FX and World Bank
// client).
type Intel struct {
	*Service
	Fetcher *Fetcher
	// BusinessType returns the organisation's business type (pkg/industry), read only.
	BusinessType func(org string) (key, name string)
	Mail         *mail.Service
	// AppURL is the web app origin used in email links (PUBLIC_APP_URL).
	AppURL string
	// LogoURL returns the organisation's absolute logo URL ("" for none).
	LogoURL func(org string) string
}

// NewIntel builds the extension.
func NewIntel(s *Service, f *Fetcher) *Intel {
	if f == nil {
		f = NewFetcher("")
	}
	return &Intel{Service: s, Fetcher: f}
}

// WatchFeed is one source an organisation follows.
type WatchFeed struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_ins_wfeed_url,priority:1"`
	URL            string     `json:"url" gorm:"type:varchar(500);not null;uniqueIndex:idx_ins_wfeed_url,priority:2"`
	Title          string     `json:"title" gorm:"type:varchar(200);not null"`
	SiteURL        string     `json:"site_url" gorm:"type:varchar(500)"`
	Group          string     `json:"group" gorm:"column:grp;type:varchar(20)"`
	Country        string     `json:"country" gorm:"type:varchar(2)"`
	Topics         []string   `json:"topics" gorm:"type:text;serializer:json"`
	Enabled        bool       `json:"enabled"`
	CatalogKey     string     `json:"catalog_key" gorm:"type:varchar(40)"`
	ETag           string     `json:"-" gorm:"column:etag;type:varchar(255)"`
	LastModified   string     `json:"-" gorm:"type:varchar(80)"`
	LastFetchedAt  *time.Time `json:"last_fetched_at"`
	LastOKAt       *time.Time `json:"last_ok_at" gorm:"column:last_ok_at"`
	LastStatus     string     `json:"last_status" gorm:"type:varchar(20)"` // ok | not_modified | error | "" (never fetched)
	LastError      string     `json:"last_error" gorm:"type:varchar(300)"`
	LastHTTP       int        `json:"last_http" gorm:"column:last_http"`
	LastItems      int        `json:"last_items"`
	LastNew        int        `json:"last_new"`
	Failures       int        `json:"failures"`
	CreatedBy      string     `json:"-" gorm:"type:varchar(255)"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Items          int        `json:"items" gorm:"-"` // stored items (derived)
}

// TableName is fixed.
func (WatchFeed) TableName() string { return "insights_watch_feeds" }

// WatchItem is one stored headline. Only the feed's own text is kept.
type WatchItem struct {
	ID             string        `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string        `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_ins_witem_hash,priority:1;index:idx_ins_witem_seen,priority:1"`
	Hash           string        `json:"-" gorm:"type:varchar(64);not null;uniqueIndex:idx_ins_witem_hash,priority:2"`
	FeedID         string        `json:"feed_id" gorm:"type:varchar(64);index"`
	GUID           string        `json:"-" gorm:"column:guid;type:varchar(500)"`
	Title          string        `json:"title" gorm:"type:varchar(300);not null"`
	Link           string        `json:"link" gorm:"type:varchar(1000)"`
	PublishedAt    *time.Time    `json:"published_at" gorm:"index"`
	Summary        string        `json:"summary" gorm:"type:varchar(1700)"` // ≤ 400 characters
	Source         string        `json:"source" gorm:"type:varchar(200)"`
	Topics         []string      `json:"topics" gorm:"type:text;serializer:json"`
	Countries      []string      `json:"countries" gorm:"type:text;serializer:json"`
	Score          float64       `json:"score" gorm:"index"`
	Reasons        []WatchReason `json:"reasons" gorm:"type:text;serializer:json"`
	Saved          bool          `json:"saved"`
	Dismissed      bool          `json:"dismissed"`
	ReadAt         *time.Time    `json:"read_at"`
	FirstSeenAt    time.Time     `json:"first_seen_at" gorm:"index:idx_ins_witem_seen,priority:2"`
	FeedTopics     []string      `json:"-" gorm:"type:text;serializer:json"` // what the source was filed under when the item arrived
	FeedCountry    string        `json:"-" gorm:"type:varchar(2)"`
}

// TableName is fixed.
func (WatchItem) TableName() string { return "insights_watch_items" }

// IntelSettings is one organisation's settings for the three views.
type IntelSettings struct {
	OrganizationID string `json:"-" gorm:"type:varchar(255);primaryKey"`
	// WatchSeededKeys are the catalogue sources already added by default
	// (never added again, so a source the user removed stays removed).
	WatchSeededKeys []string           `json:"-" gorm:"type:text;serializer:json"`
	WatchLastRun    *time.Time         `json:"watch_last_run"`
	WatchLastStatus string             `json:"watch_last_status" gorm:"type:varchar(255)"`
	TargetMarkets   []string           `json:"target_markets" gorm:"type:text;serializer:json"`
	Weights         map[string]float64 `json:"weights" gorm:"type:text;serializer:json"`
	BriefAuto       bool               `json:"brief_auto"`
	BriefEmail      bool               `json:"brief_email"`
	CreatedAt       time.Time          `json:"-"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// TableName is fixed.
func (IntelSettings) TableName() string { return "insights_settings" }

// MigrateIntel creates the tables of the industry watch, trade settings and briefs.
func MigrateIntel(db *gorm.DB) error {
	return db.AutoMigrate(&WatchFeed{}, &WatchItem{}, &WatchRule{}, &IntelSettings{}, &Brief{})
}

// Settings returns the organisation's settings, creating the row on first use
// (monthly briefs on, emailing them off).
func (i *Intel) Settings(org string) IntelSettings {
	var st IntelSettings
	if i.DB.Where("organization_id = ?", org).First(&st).Error == nil {
		return st
	}
	now := i.now()
	st = IntelSettings{OrganizationID: org, BriefAuto: true, CreatedAt: now, UpdatedAt: now}
	i.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&st)
	i.DB.Where("organization_id = ?", org).First(&st)
	return st
}

// ---------- context ----------

// Context collects what relevance is scored against: the keyword rules, the
// business type, the service lines sold in the last 12 months, the home
// country and the countries the organisation has clients in.
func (i *Intel) Context(org string) *WatchContext {
	c := &WatchContext{Rules: i.WatchRules(org), Home: i.homeCountry(org, i.Home(org)), Markets: []string{}, TypeTopics: []string{}, Lines: []string{}, LineTopics: map[string][]string{}}
	var ccs []string
	i.DB.Raw(`SELECT DISTINCT UPPER(country_code) FROM customers WHERE organization_id = ? AND deleted_at IS NULL AND COALESCE(country_code,'') <> '' ORDER BY 1`, org).Scan(&ccs)
	for _, cc := range ccs {
		if cc != c.Home && len(cc) == 2 {
			c.Markets = append(c.Markets, cc)
		}
	}
	if i.BusinessType != nil {
		c.TypeKey, c.TypeName = i.BusinessType(org)
		c.TypeTopics = append(c.TypeTopics, businessTopics[c.TypeKey]...)
	}
	// Service lines with revenue in the last 12 months.
	conv := i.conv(org, "")
	today := day(i.now())
	var lr []struct {
		Line string
		Amt  float64
	}
	i.scan(&lr, "WITH "+linesCTE(conv, i.Rules(org), Opts{})+`
SELECT line, COALESCE(SUM(amt), 0) AS amt FROM fl WHERE d > (@today)::date - interval '12 months' AND d <= (@today)::date GROUP BY line HAVING COALESCE(SUM(amt), 0) > 0 ORDER BY line`,
		map[string]interface{}{"org": org, "today": today.Format(dayFmt)})
	sold := map[string]bool{}
	for _, r := range lr {
		sold[r.Line] = true
	}
	for _, l := range LineOrder {
		if !sold[l] || l == LineOther {
			continue
		}
		c.Lines = append(c.Lines, l)
		for _, t := range lineTopics[l] {
			c.LineTopics[t] = append(c.LineTopics[t], LineLabels[l])
		}
	}
	c.prepare()
	return c
}

// ---------- sources ----------

func (i *Intel) countItems(org string, feeds []WatchFeed) {
	var rows []struct {
		FeedID string
		N      int
	}
	i.DB.Raw(`SELECT feed_id, COUNT(*) AS n FROM insights_watch_items WHERE organization_id = ? GROUP BY feed_id`, org).Scan(&rows)
	m := map[string]int{}
	for _, r := range rows {
		m[r.FeedID] = r.N
	}
	for k := range feeds {
		feeds[k].Items = m[feeds[k].ID]
		if feeds[k].Topics == nil {
			feeds[k].Topics = []string{}
		}
	}
}

// EnsureSeeded adds the verified catalogue sources that suit the organisation,
// each one once: a source the user later removes is not added again, and a
// source verified in a later release is added then. Unverified catalogue
// entries are never added here (see watch_catalog.go).
func (i *Intel) EnsureSeeded(org string) {
	pending := false
	for _, cf := range DefaultCatalog {
		if cf.Verified != "" {
			pending = true
			break
		}
	}
	if !pending {
		return
	}
	st := i.Settings(org)
	done := map[string]bool{}
	for _, k := range st.WatchSeededKeys {
		done[k] = true
	}
	var c *WatchContext
	keys := append([]string{}, st.WatchSeededKeys...)
	now := i.now()
	for _, cf := range DefaultCatalog {
		if cf.Verified == "" || done[cf.Key] {
			continue
		}
		if c == nil {
			c = i.Context(org)
		}
		keys = append(keys, cf.Key)
		if !seedByDefault(cf, c.Home, c.markets, c.TypeKey) {
			continue // not for this organisation: stays a suggestion
		}
		f := WatchFeed{ID: uuid.NewString(), OrganizationID: org, URL: cf.URL, Title: cf.Title, SiteURL: cf.Site, Group: cf.Group, Country: cf.Country,
			Topics: append([]string{}, cf.Topics...), Enabled: true, CatalogKey: cf.Key, CreatedBy: "system", CreatedAt: now, UpdatedAt: now}
		i.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&f)
	}
	if len(keys) != len(st.WatchSeededKeys) {
		i.DB.Model(&IntelSettings{}).Where("organization_id = ?", org).Updates(map[string]any{"watch_seeded_keys": jsonText(keys), "updated_at": now})
	}
}

// Feeds lists the organisation's sources.
func (i *Intel) Feeds(org string) []WatchFeed {
	out := []WatchFeed{}
	i.DB.Where("organization_id = ?", org).Order("LOWER(title), created_at").Find(&out)
	i.countItems(org, out)
	return out
}

// Suggestions lists the catalogue entries the organisation does not follow.
func (i *Intel) Suggestions(org string) []CatalogFeed {
	have := map[string]bool{}
	for _, f := range i.Feeds(org) {
		have[CanonicalLink(f.URL)] = true
	}
	out := []CatalogFeed{}
	for _, c := range DefaultCatalog {
		if !have[CanonicalLink(c.URL)] {
			out = append(out, c)
		}
	}
	return out
}

// FeedPreview is what a feed address returns right now.
type FeedPreview struct {
	URL    string     `json:"url"`
	Title  string     `json:"title"`
	Site   string     `json:"site_url"`
	Kind   string     `json:"kind"`
	Count  int        `json:"count"`
	Latest []FeedItem `json:"latest"` // newest 3
	Note   string     `json:"note,omitempty"`
}

func newestFirst(items []FeedItem) []FeedItem {
	out := append([]FeedItem(nil), items...)
	sort.SliceStable(out, func(a, b int) bool {
		pa, pb := out[a].Published, out[b].Published
		if pa == nil || pb == nil {
			return pa != nil && pb == nil
		}
		return pa.After(*pb)
	})
	return out
}

// PreviewFeed validates an address and fetches it without saving anything.
func (i *Intel) PreviewFeed(ctx context.Context, raw string) (*FeedPreview, string, error) {
	u, err := ValidateFeedURL(raw, false)
	if err != nil {
		return nil, "", err
	}
	res, err := i.Fetcher.Fetch(ctx, u, "", "")
	if err != nil {
		return nil, u, err
	}
	p := &FeedPreview{URL: u, Title: res.Feed.Title, Site: res.Feed.Link, Kind: res.Feed.Kind, Count: len(res.Feed.Items), Latest: []FeedItem{}}
	for k, it := range newestFirst(res.Feed.Items) {
		if k >= 3 {
			break
		}
		p.Latest = append(p.Latest, it)
	}
	if p.Count == 0 {
		p.Note = "The feed is valid but has no items right now."
	}
	return p, u, nil
}

// FeedInput is the editable part of a source.
type FeedInput struct {
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Country string   `json:"country"`
	Topics  []string `json:"topics"`
	Enabled *bool    `json:"enabled"`
	Catalog string   `json:"catalog_key"`
}

func (i *Intel) cleanFeedInput(org string, in *FeedInput) error {
	in.Title = strings.TrimSpace(in.Title)
	if len(in.Title) > 200 {
		return errors.New("The name is too long (200 characters at most)")
	}
	in.Country = strings.ToUpper(strings.TrimSpace(in.Country))
	if in.Country != "" {
		if len(in.Country) != 2 || !reCountry.MatchString(in.Country) {
			return errors.New("Country must be a 2-letter code such as KE or AU")
		}
	}
	valid := map[string]bool{}
	for _, r := range i.WatchRules(org) {
		valid[r.Key] = true
	}
	topics := []string{}
	seen := map[string]bool{}
	for _, t := range in.Topics {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		if !valid[t] {
			return fmt.Errorf("Unknown topic %q", t)
		}
		seen[t] = true
		topics = append(topics, t)
	}
	in.Topics = topics
	return nil
}

// AddFeed validates an address by fetching it and, when it returns a readable
// feed, stores the source and its current items. A catalogue key fills in the
// catalogue's title, country and topics.
func (i *Intel) AddFeed(ctx context.Context, org, user string, in FeedInput) (*WatchFeed, *FeedRun, error) {
	cat := CatalogByKey(in.Catalog)
	if cat != nil {
		if in.URL == "" {
			in.URL = cat.URL
		}
		if in.Title == "" {
			in.Title = cat.Title
		}
		if in.Country == "" {
			in.Country = cat.Country
		}
		if in.Topics == nil {
			in.Topics = append([]string{}, cat.Topics...)
		}
	}
	u, err := ValidateFeedURL(in.URL, false)
	if err != nil {
		return nil, nil, err
	}
	if err := i.cleanFeedInput(org, &in); err != nil {
		return nil, nil, err
	}
	var feeds []WatchFeed
	i.DB.Where("organization_id = ?", org).Find(&feeds)
	if len(feeds) >= MaxWatchFeeds {
		return nil, nil, fmt.Errorf("You can follow up to %d sources — remove one first", MaxWatchFeeds)
	}
	for _, f := range feeds {
		if CanonicalLink(f.URL) == CanonicalLink(u) {
			return nil, nil, fmt.Errorf("You already follow this source (%s)", f.Title)
		}
	}
	res, err := i.Fetcher.Fetch(ctx, u, "", "")
	if err != nil {
		return nil, nil, err
	}
	now := i.now()
	f := &WatchFeed{ID: uuid.NewString(), OrganizationID: org, URL: u, Title: nonEmpty(in.Title, nonEmpty(res.Feed.Title, hostOf(u))), SiteURL: res.Feed.Link,
		Group: "custom", Country: in.Country, Topics: in.Topics, Enabled: true, CreatedBy: user, CreatedAt: now, UpdatedAt: now}
	if in.Enabled != nil {
		f.Enabled = *in.Enabled
	}
	if cat != nil && CanonicalLink(cat.URL) == CanonicalLink(u) {
		f.CatalogKey, f.Group = cat.Key, cat.Group
		if f.SiteURL == "" {
			f.SiteURL = cat.Site
		}
	}
	f.Title = clipRunes(f.Title, 200)
	if err := i.DB.Create(f).Error; err != nil {
		return nil, nil, errors.New("Could not save the source")
	}
	i.Settings(org) // the scheduler only visits organisations with a settings row
	run := i.applyFetch(org, f, res, nil, i.Context(org))
	i.countItems(org, []WatchFeed{*f})
	out := i.feedByID(org, f.ID)
	return out, run, nil
}

func hostOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if k := strings.IndexByte(s, '/'); k >= 0 {
		s = s[:k]
	}
	return strings.TrimPrefix(s, "www.")
}

func (i *Intel) feedByID(org, id string) *WatchFeed {
	var f WatchFeed
	if i.DB.Where("organization_id = ? AND id = ?", org, id).First(&f).Error != nil {
		return nil
	}
	l := []WatchFeed{f}
	i.countItems(org, l)
	return &l[0]
}

// SuggestedResult reports adding several catalogue sources.
type SuggestedResult struct {
	Added  []WatchFeed     `json:"added"`
	Failed []SuggestedFail `json:"failed"`
	New    int             `json:"new_items"`
}

// SuggestedFail is a catalogue source that did not answer with a feed.
type SuggestedFail struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

// AddSuggested checks catalogue sources live and adds the ones that answer
// with a readable feed (keys empty = every suggestion).
func (i *Intel) AddSuggested(ctx context.Context, org, user string, keys []string) *SuggestedResult {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	res := &SuggestedResult{Added: []WatchFeed{}, Failed: []SuggestedFail{}}
	sug := i.Suggestions(org)
	type out struct {
		f   *WatchFeed
		run *FeedRun
		err error
	}
	results := make([]out, len(sug))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for k, c := range sug {
		if len(want) > 0 && !want[c.Key] {
			continue
		}
		wg.Add(1)
		go func(k int, c CatalogFeed) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			f, run, err := i.AddFeed(ctx, org, user, FeedInput{Catalog: c.Key})
			results[k] = out{f, run, err}
		}(k, c)
	}
	wg.Wait()
	for k, c := range sug {
		r := results[k]
		switch {
		case r.err != nil:
			res.Failed = append(res.Failed, SuggestedFail{Key: c.Key, Title: c.Title, URL: c.URL, Error: r.err.Error()})
		case r.f != nil:
			res.Added = append(res.Added, *r.f)
			if r.run != nil {
				res.New += r.run.New
			}
		}
	}
	return res
}

// FeedPatch is a partial change to a source (nil = leave).
type FeedPatch struct {
	Title   *string   `json:"title"`
	Country *string   `json:"country"`
	Topics  *[]string `json:"topics"`
	Enabled *bool     `json:"enabled"`
}

// UpdateFeed changes a source's name, country, topics or enabled state.
// Switching on a source that never returned a feed checks it first.
func (i *Intel) UpdateFeed(ctx context.Context, org, id string, p FeedPatch) (*WatchFeed, error) {
	f := i.feedByID(org, id)
	if f == nil {
		return nil, gorm.ErrRecordNotFound
	}
	in := FeedInput{Title: f.Title, Country: f.Country, Topics: f.Topics}
	if p.Title != nil {
		in.Title = *p.Title
		if strings.TrimSpace(in.Title) == "" {
			return nil, errors.New("Give the source a name")
		}
	}
	if p.Country != nil {
		in.Country = *p.Country
	}
	if p.Topics != nil {
		in.Topics = *p.Topics
	}
	if err := i.cleanFeedInput(org, &in); err != nil {
		return nil, err
	}
	up := map[string]any{"updated_at": i.now()}
	rescore := false
	if in.Title != f.Title {
		up["title"] = in.Title
	}
	if in.Country != f.Country {
		up["country"] = in.Country
		rescore = true
	}
	if strings.Join(in.Topics, ",") != strings.Join(f.Topics, ",") {
		up["topics"] = jsonText(in.Topics)
		rescore = true
	}
	if p.Enabled != nil && *p.Enabled != f.Enabled {
		if *p.Enabled && f.LastOKAt == nil {
			if _, err := i.Fetcher.Fetch(ctx, f.URL, "", ""); err != nil {
				return nil, fmt.Errorf("This source can't be switched on yet: %s", lowerFirst(err.Error()))
			}
		}
		up["enabled"] = *p.Enabled
	}
	if err := i.DB.Model(&WatchFeed{}).Where("organization_id = ? AND id = ?", org, id).Updates(up).Error; err != nil {
		return nil, errors.New("Could not save the source")
	}
	if _, ok := up["title"]; ok {
		i.DB.Model(&WatchItem{}).Where("organization_id = ? AND feed_id = ?", org, id).Update("source", in.Title)
	}
	if rescore {
		i.DB.Model(&WatchItem{}).Where("organization_id = ? AND feed_id = ?", org, id).Updates(map[string]any{"feed_topics": jsonText(in.Topics), "feed_country": in.Country})
		i.Rescore(org)
	}
	return i.feedByID(org, id), nil
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// DeleteFeed removes a source and its items (saved items are kept).
func (i *Intel) DeleteFeed(org, id string) error {
	res := i.DB.Where("organization_id = ? AND id = ?", org, id).Delete(&WatchFeed{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return i.DB.Where("organization_id = ? AND feed_id = ? AND NOT saved", org, id).Delete(&WatchItem{}).Error
}

// ---------- refresh ----------

// FeedRun is the outcome of fetching one source.
type FeedRun struct {
	FeedID string `json:"feed_id"`
	Title  string `json:"title"`
	Status string `json:"status"` // ok | not_modified | error
	Items  int    `json:"items"`
	New    int    `json:"new"`
	Error  string `json:"error,omitempty"`
}

// RefreshSummary is the outcome of a refresh.
type RefreshSummary struct {
	Feeds   []FeedRun `json:"feeds"`
	Checked int       `json:"checked"`
	New     int       `json:"new"`
	Errors  int       `json:"errors"`
	Pruned  int       `json:"pruned"`
	Busy    bool      `json:"busy,omitempty"` // another refresh holds the lock
	At      string    `json:"at"`
}

// applyFetch stores the outcome of one fetch: the source's status and, for a
// 200, the new items.
func (i *Intel) applyFetch(org string, f *WatchFeed, res *FetchResult, ferr error, c *WatchContext) *FeedRun {
	now := i.now()
	run := &FeedRun{FeedID: f.ID, Title: f.Title}
	up := map[string]any{"last_fetched_at": now, "updated_at": now}
	if res != nil {
		up["last_http"] = res.Status
	}
	switch {
	case ferr != nil:
		run.Status, run.Error = "error", ferr.Error()
		up["last_status"], up["last_error"], up["failures"] = "error", clipRunes(ferr.Error(), 290), gorm.Expr("failures + 1")
	case res.NotModified:
		run.Status = "not_modified"
		up["last_status"], up["last_error"], up["failures"], up["last_ok_at"], up["last_new"] = "not_modified", "", 0, now, 0
	default:
		n := i.ingest(org, f, res.Feed, c, now)
		run.Status, run.Items, run.New = "ok", len(res.Feed.Items), n
		up["last_status"], up["last_error"], up["failures"], up["last_ok_at"] = "ok", "", 0, now
		up["last_items"], up["last_new"], up["etag"], up["last_modified"] = len(res.Feed.Items), n, res.ETag, res.LastModified
		if f.SiteURL == "" && res.Feed.Link != "" {
			up["site_url"] = clipRunes(res.Feed.Link, 490)
		}
	}
	i.DB.Model(&WatchFeed{}).Where("id = ?", f.ID).Updates(up)
	return run
}

// ingest stores the items of a parsed feed that are not known yet. Items
// published before the retention window are skipped (they would be pruned).
func (i *Intel) ingest(org string, f *WatchFeed, feed *ParsedFeed, c *WatchContext, now time.Time) int {
	cutoff := now.AddDate(0, 0, -WatchRetentionDays)
	added := 0
	seen := map[string]bool{}
	for _, it := range feed.Items {
		pub := it.Published
		if pub != nil {
			if pub.Before(cutoff) {
				continue
			}
			if pub.After(now.Add(24 * time.Hour)) {
				t := now
				pub = &t // a date in the future is a feed error
			}
		}
		h := ItemHash(it)
		if seen[h] {
			continue
		}
		seen[h] = true
		topics, countries, score, reasons := ScoreItem(c, it.Title, it.Summary, f.Topics, f.Country)
		row := WatchItem{ID: uuid.NewString(), OrganizationID: org, Hash: h, FeedID: f.ID, GUID: clipRunes(it.GUID, 490), Title: clipRunes(it.Title, 300),
			Link: clipRunes(it.Link, 990), PublishedAt: pub, Summary: it.Summary, Source: f.Title, Topics: topics, Countries: countries, Score: score, Reasons: reasons,
			FirstSeenAt: now, FeedTopics: f.Topics, FeedCountry: f.Country}
		res := i.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if res.Error != nil {
			log.Printf("insights watch: store item: %v", res.Error)
			continue
		}
		added += int(res.RowsAffected)
	}
	return added
}

// Prune deletes items older than the retention window, except saved ones.
func (i *Intel) Prune(org string) int {
	cutoff := i.now().AddDate(0, 0, -WatchRetentionDays)
	res := i.DB.Where("organization_id = ? AND NOT saved AND first_seen_at < ? AND COALESCE(published_at, first_seen_at) < ?", org, cutoff, cutoff).Delete(&WatchItem{})
	return int(res.RowsAffected)
}

// withLock runs fn holding a Postgres advisory lock (one refresh per
// organisation across instances); false when another session holds it.
func (i *Intel) withLock(ctx context.Context, key string, fn func()) bool {
	got := false
	err := i.DB.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", key).Scan(&got).Error; err != nil {
			return err
		}
		if !got {
			return nil
		}
		defer tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", key)
		fn()
		return nil
	})
	if err != nil {
		log.Printf("insights: lock %s: %v", key, err)
		return false
	}
	return got
}

// Refresh fetches the organisation's enabled sources (or one source), stores
// new items and prunes old ones.
func (i *Intel) Refresh(ctx context.Context, org, feedID string) *RefreshSummary {
	sum := &RefreshSummary{Feeds: []FeedRun{}}
	ok := i.withLock(ctx, "insights-watch:"+org, func() { i.refresh(ctx, org, feedID, sum) })
	if !ok {
		sum.Busy = true
	}
	sum.At = i.now().UTC().Format(time.RFC3339)
	return sum
}

func (i *Intel) refresh(ctx context.Context, org, feedID string, sum *RefreshSummary) {
	var feeds []WatchFeed
	q := i.DB.Where("organization_id = ?", org)
	if feedID != "" {
		q = q.Where("id = ?", feedID)
	} else {
		q = q.Where("enabled")
	}
	q.Order("LOWER(title), created_at").Find(&feeds)
	c := i.Context(org)
	runs := make([]*FeedRun, len(feeds))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for k := range feeds {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			f := &feeds[k]
			res, err := i.Fetcher.Fetch(ctx, f.URL, f.ETag, f.LastModified)
			runs[k] = i.applyFetch(org, f, res, err, c)
		}(k)
	}
	wg.Wait()
	for _, r := range runs {
		sum.Feeds = append(sum.Feeds, *r)
		sum.Checked++
		sum.New += r.New
		if r.Status == "error" {
			sum.Errors++
		}
	}
	sum.Pruned = i.Prune(org)
	if feedID == "" {
		now := i.now()
		status := fmt.Sprintf("%d sources checked, %d new items", sum.Checked, sum.New)
		if sum.Errors > 0 {
			status += fmt.Sprintf(", %d with errors", sum.Errors)
		}
		i.Settings(org)
		i.DB.Model(&IntelSettings{}).Where("organization_id = ?", org).Updates(map[string]any{"watch_last_run": now, "watch_last_status": status, "updated_at": now})
	}
}

// Rescore re-tags and re-scores every stored item (after the rules, a source's
// topics or the business context changed).
func (i *Intel) Rescore(org string) int {
	c := i.Context(org)
	var items []WatchItem
	i.DB.Where("organization_id = ?", org).Find(&items)
	n := 0
	for _, it := range items {
		topics, countries, score, reasons := ScoreItem(c, it.Title, it.Summary, it.FeedTopics, it.FeedCountry)
		if score == it.Score && strings.Join(topics, ",") == strings.Join(it.Topics, ",") && strings.Join(countries, ",") == strings.Join(it.Countries, ",") && len(reasons) == len(it.Reasons) {
			same := true
			for k := range reasons {
				if reasons[k] != it.Reasons[k] {
					same = false
					break
				}
			}
			if same {
				continue
			}
		}
		i.DB.Model(&WatchItem{}).Where("id = ?", it.ID).Updates(map[string]any{"topics": jsonText(topics), "countries": jsonText(countries), "score": score, "reasons": jsonText(reasons)})
		n++
	}
	return n
}

// ---------- reading ----------

// WatchFilter narrows the item list.
type WatchFilter struct {
	Topic     string
	Country   string
	FeedID    string
	Q         string
	Saved     bool
	Unread    bool
	Dismissed bool
	Days      int // 0 = all
	Sort      string
	Limit     int
	Offset    int
}

// Facet is a filter value with its item count.
type Facet struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// WatchList is GET /insights/watch.
type WatchList struct {
	Items     []WatchItem   `json:"items"`
	Total     int64         `json:"total"`
	Unread    int           `json:"unread"`
	Saved     int           `json:"saved"`
	All       int           `json:"all"`
	Topics    []Facet       `json:"topics"`
	Countries []Facet       `json:"countries"`
	Sources   []Facet       `json:"sources"`
	Feeds     int           `json:"feeds"`
	Enabled   int           `json:"feeds_enabled"`
	Failing   int           `json:"feeds_failing"`
	Suggested int           `json:"suggested"`
	LastRun   *time.Time    `json:"last_run"`
	LastNote  string        `json:"last_status"`
	Context   *WatchContext `json:"context"`
	Formula   string        `json:"formula"`
	Retention int           `json:"retention_days"`
	Today     string        `json:"today"`
}

const itemDate = "COALESCE(published_at, first_seen_at)"

func (i *Intel) itemQuery(org string, f WatchFilter) *gorm.DB {
	q := i.DB.Model(&WatchItem{}).Where("organization_id = ?", org)
	if f.Dismissed {
		q = q.Where("dismissed")
	} else {
		q = q.Where("NOT dismissed")
	}
	if f.Saved {
		q = q.Where("saved")
	}
	if f.Unread {
		q = q.Where("read_at IS NULL")
	}
	if t := slugKey(f.Topic); t != "" {
		q = q.Where("topics LIKE ?", `%"`+t+`"%`)
	}
	if cc := strings.ToUpper(strings.TrimSpace(f.Country)); len(cc) == 2 && reCountry.MatchString(cc) {
		q = q.Where("countries LIKE ?", `%"`+cc+`"%`)
	}
	if f.FeedID != "" {
		q = q.Where("feed_id = ?", f.FeedID)
	}
	if s := strings.TrimSpace(f.Q); s != "" {
		like := "%" + likeEscape(strings.ToLower(s)) + "%"
		q = q.Where("(LOWER(title) LIKE ? OR LOWER(summary) LIKE ? OR LOWER(source) LIKE ?)", like, like, like)
	}
	if f.Days > 0 && !f.Saved {
		q = q.Where(itemDate+" >= ?", i.now().AddDate(0, 0, -f.Days))
	}
	return q
}

func fixItems(items []WatchItem) {
	for k := range items {
		if items[k].Topics == nil {
			items[k].Topics = []string{}
		}
		if items[k].Countries == nil {
			items[k].Countries = []string{}
		}
		if items[k].Reasons == nil {
			items[k].Reasons = []WatchReason{}
		}
	}
}

// List returns stored items with filter counts.
func (i *Intel) List(org string, f WatchFilter) *WatchList {
	i.EnsureSeeded(org)
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 30
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	st := i.Settings(org)
	out := &WatchList{Items: []WatchItem{}, Topics: []Facet{}, Countries: []Facet{}, Sources: []Facet{}, LastRun: st.WatchLastRun, LastNote: st.WatchLastStatus,
		Formula: WatchFormulaDoc, Retention: WatchRetentionDays, Today: day(i.now()).Format(dayFmt)}
	q := i.itemQuery(org, f)
	q.Count(&out.Total)
	order := "score DESC, " + itemDate + " DESC, id"
	if f.Sort == "newest" {
		order = itemDate + " DESC, score DESC, id"
	}
	q.Order(order).Limit(f.Limit).Offset(f.Offset).Find(&out.Items)
	fixItems(out.Items)

	// Filter counts over everything that is not dismissed.
	var all []WatchItem
	i.DB.Select("feed_id", "topics", "countries", "saved", "read_at").Where("organization_id = ? AND NOT dismissed", org).Find(&all)
	rules := i.WatchRules(org)
	tc, cc, sc := map[string]int{}, map[string]int{}, map[string]int{}
	for _, it := range all {
		out.All++
		if it.Saved {
			out.Saved++
		}
		if it.ReadAt == nil {
			out.Unread++
		}
		for _, t := range it.Topics {
			tc[t]++
		}
		for _, c := range it.Countries {
			cc[c]++
		}
		sc[it.FeedID]++
	}
	for _, r := range rules {
		if tc[r.Key] > 0 {
			out.Topics = append(out.Topics, Facet{Key: r.Key, Label: r.Label, Count: tc[r.Key]})
		}
	}
	for c, n := range cc {
		out.Countries = append(out.Countries, Facet{Key: c, Label: CountryName(c), Count: n})
	}
	sort.Slice(out.Countries, func(a, b int) bool {
		if out.Countries[a].Count != out.Countries[b].Count {
			return out.Countries[a].Count > out.Countries[b].Count
		}
		return out.Countries[a].Label < out.Countries[b].Label
	})
	feeds := i.Feeds(org)
	for _, fd := range feeds {
		out.Feeds++
		if fd.Enabled {
			out.Enabled++
			if fd.LastStatus == "error" {
				out.Failing++
			}
		}
		if sc[fd.ID] > 0 {
			out.Sources = append(out.Sources, Facet{Key: fd.ID, Label: fd.Title, Count: sc[fd.ID]})
		}
	}
	out.Suggested = len(i.Suggestions(org))
	out.Context = i.Context(org)
	return out
}

// WatchLatest is the dashboard card.
type WatchLatest struct {
	Items   []WatchItem `json:"items"`
	Unread  int         `json:"unread"`
	Feeds   int         `json:"feeds"`
	LastRun *time.Time  `json:"last_run"`
	Days    int         `json:"days"`
}

// Latest returns the n most relevant items of the last 14 days (falling back
// to the most recent ones when nothing arrived in that time).
func (i *Intel) Latest(org string, n int) *WatchLatest {
	if n <= 0 || n > 20 {
		n = 3
	}
	out := &WatchLatest{Items: []WatchItem{}, Days: 14}
	var cnt int64
	i.DB.Model(&WatchFeed{}).Where("organization_id = ?", org).Count(&cnt)
	out.Feeds = int(cnt)
	if cnt == 0 {
		return out
	}
	var st IntelSettings
	if i.DB.Where("organization_id = ?", org).First(&st).Error == nil {
		out.LastRun = st.WatchLastRun
	}
	base := func() *gorm.DB { return i.DB.Where("organization_id = ? AND NOT dismissed", org) }
	base().Where(itemDate+" >= ?", i.now().AddDate(0, 0, -out.Days)).Order("score DESC, " + itemDate + " DESC, id").Limit(n).Find(&out.Items)
	if len(out.Items) == 0 {
		out.Days = 0
		base().Order(itemDate + " DESC, score DESC, id").Limit(n).Find(&out.Items)
	}
	fixItems(out.Items)
	var unread int64
	base().Model(&WatchItem{}).Where("read_at IS NULL").Count(&unread)
	out.Unread = int(unread)
	return out
}

// ItemPatch changes an item's flags (nil = leave).
type ItemPatch struct {
	Saved     *bool `json:"saved"`
	Dismissed *bool `json:"dismissed"`
	Read      *bool `json:"read"`
}

// SetItem saves, dismisses or marks an item read.
func (i *Intel) SetItem(org, id string, p ItemPatch) (*WatchItem, error) {
	var it WatchItem
	if i.DB.Where("organization_id = ? AND id = ?", org, id).First(&it).Error != nil {
		return nil, gorm.ErrRecordNotFound
	}
	up := map[string]any{}
	now := i.now()
	if p.Saved != nil {
		up["saved"] = *p.Saved
		if *p.Saved {
			up["dismissed"] = false
		}
	}
	if p.Dismissed != nil {
		up["dismissed"] = *p.Dismissed
		if *p.Dismissed {
			up["saved"] = false
			if it.ReadAt == nil {
				up["read_at"] = now
			}
		}
	}
	if p.Read != nil {
		if *p.Read {
			if it.ReadAt == nil {
				up["read_at"] = now
			}
		} else {
			up["read_at"] = nil
		}
	}
	if len(up) > 0 {
		if err := i.DB.Model(&WatchItem{}).Where("id = ?", it.ID).Updates(up).Error; err != nil {
			return nil, err
		}
	}
	i.DB.Where("id = ?", it.ID).First(&it)
	l := []WatchItem{it}
	fixItems(l)
	return &l[0], nil
}

// MarkAllRead marks every unread item read.
func (i *Intel) MarkAllRead(org string) int {
	res := i.DB.Model(&WatchItem{}).Where("organization_id = ? AND read_at IS NULL AND NOT dismissed", org).Update("read_at", i.now())
	return int(res.RowsAffected)
}

// TopHeadlines returns the most relevant items first seen or published in a
// period (for the strategy brief).
func (i *Intel) TopHeadlines(org string, from, to time.Time, n int) []WatchItem {
	out := []WatchItem{}
	i.DB.Where("organization_id = ? AND NOT dismissed AND "+itemDate+" >= ? AND "+itemDate+" < ?", org, from, to.AddDate(0, 0, 1)).
		Order("score DESC, " + itemDate + " DESC, id").Limit(n).Find(&out)
	fixItems(out)
	return out
}
