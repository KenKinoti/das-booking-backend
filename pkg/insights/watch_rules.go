package insights

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Tagging and relevance for the industry watch. Everything is a plain keyword
// rule the organisation can read and edit — no model, no hidden weighting.
//
//	An item is tagged with a topic when one of the topic's keywords appears in
//	its title or summary as a whole word (case-insensitive; a trailing "s" is
//	allowed). Topics a source is filed under also apply, at half strength.
//
//	score = min(100, Σ topic points + country points)
//	topic points   = 12 × topic weight × match × boost
//	  match        = 1 (one keyword) · 1.25 (two) · 1.5 (three or more),
//	                 +0.25 when a keyword is in the title, 0.5 when the topic
//	                 only comes from the source
//	  boost        = 1 + 0.5 (topic matters to your business type)
//	                   + 0.5 (topic belongs to a service line you sold in the last 12 months)
//	country points = 15 when the item is about your home country,
//	                 10 when it is about a country you have clients in
const (
	topicBase       = 12.0
	homeCountryPts  = 15.0
	marketPts       = 10.0
	WatchFormulaDoc = "Score (0–100) = topic points + country points. Each matched topic gives 12 × its weight × match strength (1 for one keyword, 1.25 for two, 1.5 for three or more, +0.25 if in the title; 0.5 when only the source is filed under the topic) × boost (+50% if the topic matters to your business type, +50% if it belongs to a service line you sold in the last 12 months). Country: +15 for your home country, +10 for a country you have clients in."
)

// WatchRule is one topic with its keywords (insights_watch_rules; the built-in
// defaults apply until an organisation edits them).
type WatchRule struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_ins_wrule,priority:1"`
	Key            string    `json:"key" gorm:"type:varchar(40);not null;uniqueIndex:idx_ins_wrule,priority:2"`
	Label          string    `json:"label" gorm:"type:varchar(80);not null"`
	Keywords       []string  `json:"keywords" gorm:"type:text;serializer:json"`
	Weight         float64   `json:"weight"`
	Enabled        bool      `json:"enabled"`
	Position       int       `json:"position"`
	Hint           string    `json:"hint,omitempty" gorm:"-"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName is fixed.
func (WatchRule) TableName() string { return "insights_watch_rules" }

// Topic keys.
const (
	TopicHosting     = "hosting"
	TopicCloud       = "cloud"
	TopicAI          = "ai"
	TopicPayments    = "payments"
	TopicEcommerce   = "ecommerce"
	TopicSecurity    = "cybersecurity"
	TopicRegulation  = "regulation"
	TopicTrade       = "trade"
	TopicFX          = "fx"
	TopicPlatforms   = "platforms"
	TopicCompetitors = "competitors"
	TopicSectors     = "client_sectors"
)

var topicHints = map[string]string{
	TopicCompetitors: "Type the names of competitors (agencies, hosts, software) — one per keyword. Items naming them score highest.",
	TopicSectors:     "The sectors your clients are in. News about them is news about your pipeline.",
}

// DefaultWatchRules is the built-in topic list.
func DefaultWatchRules() []WatchRule {
	mk := func(pos int, key, label string, weight float64, kw ...string) WatchRule {
		return WatchRule{Key: key, Label: label, Keywords: kw, Weight: weight, Enabled: true, Position: pos, Hint: topicHints[key]}
	}
	return []WatchRule{
		mk(10, TopicHosting, "Hosting & domains", 1, "hosting", "web host", "domain", "domain name", "dns", "cpanel", "registrar", "registry", ".ke", ".co.ke", ".com.au", ".au",
			"kenic", "auda", "icann", "tld", "ssl certificate", "data centre", "data center", "datacenter", "colocation", "vps", "synergy wholesale", "uptime"),
		mk(20, TopicCloud, "Cloud", 1, "cloud", "aws", "amazon web services", "azure", "google cloud", "gcp", "cloudflare", "kubernetes", "serverless", "cdn", "saas",
			"outage", "edge computing", "object storage"),
		mk(30, TopicAI, "AI & automation", 1, "ai", "artificial intelligence", "machine learning", "generative", "llm", "chatgpt", "openai", "anthropic", "claude", "gemini",
			"copilot", "automation", "agentic", "chatbot", "ai agent"),
		mk(40, TopicPayments, "Payments & fintech", 1, "m-pesa", "mpesa", "flutterwave", "paystack", "pesapal", "fintech", "mobile money", "payment", "payments", "stripe",
			"paypal", "visa", "mastercard", "card payments", "digital lending", "wallet", "remittance", "airtel money", "pesalink", "open banking"),
		mk(50, TopicEcommerce, "E-commerce", 1, "e-commerce", "ecommerce", "online shop", "online store", "woocommerce", "shopify", "marketplace", "jumia", "checkout",
			"online retail", "online shopping"),
		mk(60, TopicSecurity, "Cybersecurity", 1, "cybersecurity", "cyber security", "cyber", "ransomware", "phishing", "malware", "vulnerability", "vulnerabilities",
			"breach", "data breach", "hack", "hacked", "hackers", "zero-day", "ddos", "cve", "exploit", "security advisory", "security update", "two-factor", "mfa", "patch"),
		mk(70, TopicRegulation, "Regulation & tax", 1, "regulation", "regulations", "regulator", "regulatory", "tax", "vat", "gst", "etims", "kra", "kenya revenue authority",
			"communications authority", "odpc", "data protection", "privacy act", "auda", "acma", "ato", "licence", "license", "licensing", "compliance", "finance bill",
			"digital service tax", "withholding tax", "legislation", "gazette"),
		mk(80, TopicTrade, "Trade & export", 1, "export", "exports", "exporter", "exporters", "imports", "trade", "tariff", "tariffs", "afcfta", "eac", "east african community",
			"wto", "unctad", "free trade", "cross-border", "outsourcing", "bpo", "offshoring", "services trade", "austrade", "keproba", "kentrade", "market access", "customs"),
		mk(90, TopicFX, "FX & economy", 1, "exchange rate", "forex", "shilling", "kes", "aud", "dollar", "currency", "inflation", "interest rate", "interest rates",
			"central bank", "cbk", "rba", "gdp", "economy", "economic", "recession", "depreciation", "devaluation"),
		mk(100, TopicPlatforms, "Web & software platforms", 1, "wordpress", "woocommerce", "plugin", "plugins", "gutenberg", "google workspace", "gmail", "microsoft 365",
			"php", "erp", "elementor", "theme", "core update"),
		mk(110, TopicCompetitors, "Competitors", 3),
		mk(120, TopicSectors, "Clients' sectors", 1, "ngo", "ngos", "non-profit", "nonprofit", "charity", "sme", "smes", "small business", "small businesses", "startup",
			"startups", "agriculture", "tourism", "education", "healthcare", "real estate", "cooperative", "sacco"),
	}
}

// businessTopics: the topics that matter to a business type (pkg/industry keys).
var businessTopics = map[string][]string{
	"agency":     {TopicHosting, TopicCloud, TopicAI, TopicEcommerce, TopicSecurity, TopicPlatforms, TopicPayments},
	"consulting": {TopicAI, TopicRegulation, TopicTrade, TopicFX},
	"retail":     {TopicEcommerce, TopicPayments, TopicRegulation},
	"restaurant": {TopicPayments, TopicRegulation},
	"mechanic":   {TopicPayments, TopicRegulation},
	"salon":      {TopicPayments, TopicRegulation},
	"spa":        {TopicPayments, TopicRegulation},
	"clinic":     {TopicRegulation, TopicSecurity},
	"ndis":       {TopicRegulation},
}

// lineTopics: the topics tied to a service line (pkg/insights service lines).
var lineTopics = map[string][]string{
	LineHosting:     {TopicHosting, TopicCloud, TopicSecurity},
	LineDomains:     {TopicHosting, TopicRegulation},
	LineWorkspace:   {TopicCloud, TopicPlatforms},
	LineWebDev:      {TopicPlatforms, TopicEcommerce, TopicAI},
	LineMaintenance: {TopicSecurity, TopicPlatforms},
	LineSoftware:    {TopicAI, TopicCloud},
	LineConsulting:  {TopicAI, TopicTrade},
}

// countryKeywords tags the countries an item is about.
var countryKeywords = map[string][]string{
	"KE": {"kenya", "kenyan", "kenyans", "nairobi", "mombasa", "kisumu", "safaricom", "m-pesa", "mpesa", "kra", "cbk", ".co.ke", "kenic"},
	"AU": {"australia", "australian", "australians", "sydney", "melbourne", "brisbane", "perth", "canberra", "adelaide", ".com.au", "auda", "acma", "acsc"},
	"UG": {"uganda", "ugandan", "kampala"},
	"TZ": {"tanzania", "tanzanian", "dar es salaam", "dodoma"},
	"RW": {"rwanda", "rwandan", "kigali"},
	"GB": {"united kingdom", "britain", "british", "london", "uk"},
	"US": {"united states", "u.s.", "usa", "silicon valley"},
	"AE": {"uae", "dubai", "abu dhabi", "emirates"},
	"ZA": {"south africa", "south african", "johannesburg", "cape town"},
	"NG": {"nigeria", "nigerian", "lagos"},
	"GH": {"ghana", "ghanaian", "accra"},
	"ET": {"ethiopia", "ethiopian", "addis ababa"},
	"IN": {"india", "indian"},
	"CN": {"china", "chinese"},
}

var countryKeyOrder = func() []string {
	out := make([]string, 0, len(countryKeywords))
	for k := range countryKeywords {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}()

// WatchRules returns the organisation's topics (defaults until edited).
func (s *Service) WatchRules(org string) []WatchRule {
	var rows []WatchRule
	s.DB.Where("organization_id = ?", org).Order("position, key").Find(&rows)
	if len(rows) == 0 {
		return DefaultWatchRules()
	}
	for i := range rows {
		rows[i].Hint = topicHints[rows[i].Key]
		if rows[i].Keywords == nil {
			rows[i].Keywords = []string{}
		}
	}
	return rows
}

func slugKey(s string) string {
	b := &strings.Builder{}
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "_"):
			b.WriteByte('_')
		}
	}
	return clipRunes(strings.Trim(b.String(), "_"), 40)
}

// cleanKeywords trims, lower-cases and de-duplicates keywords.
func cleanKeywords(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, k := range in {
		k = strings.ToLower(strings.TrimSpace(reSpace.ReplaceAllString(k, " ")))
		if k == "" || seen[k] {
			continue
		}
		if len([]rune(k)) < 2 {
			return nil, fmt.Errorf("keyword %q is too short (use at least 2 characters)", k)
		}
		if len(k) > 60 {
			return nil, fmt.Errorf("keyword %q is too long (60 characters at most)", clipRunes(k, 30)+"…")
		}
		seen[k] = true
		out = append(out, k)
	}
	if len(out) > 80 {
		return nil, fmt.Errorf("a topic can have up to 80 keywords")
	}
	return out, nil
}

// SaveWatchRules replaces the organisation's topics.
func (s *Service) SaveWatchRules(org string, in []WatchRule) ([]WatchRule, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("keep at least one topic (or reset to the defaults)")
	}
	if len(in) > 30 {
		return nil, fmt.Errorf("up to 30 topics")
	}
	seen := map[string]bool{}
	now := s.now()
	rows := make([]WatchRule, 0, len(in))
	for i, r := range in {
		r.Label = strings.TrimSpace(r.Label)
		if r.Label == "" {
			return nil, fmt.Errorf("every topic needs a name")
		}
		if len(r.Label) > 80 {
			return nil, fmt.Errorf("topic name %q is too long", clipRunes(r.Label, 30)+"…")
		}
		key := slugKey(r.Key)
		if key == "" {
			key = slugKey(r.Label)
		}
		if key == "" {
			return nil, fmt.Errorf("topic %q needs a name with letters or digits", r.Label)
		}
		if seen[key] {
			return nil, fmt.Errorf("two topics are called %q", r.Label)
		}
		seen[key] = true
		kw, err := cleanKeywords(r.Keywords)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", r.Label, err)
		}
		if math.IsNaN(r.Weight) || r.Weight < 0 || r.Weight > 5 {
			return nil, fmt.Errorf("%s: the weight must be between 0 and 5", r.Label)
		}
		rows = append(rows, WatchRule{ID: uuid.NewString(), OrganizationID: org, Key: key, Label: r.Label, Keywords: kw, Weight: math.Round(r.Weight*100) / 100,
			Enabled: r.Enabled, Position: (i + 1) * 10, UpdatedAt: now})
	}
	tx := s.DB.Begin()
	if err := tx.Where("organization_id = ?", org).Delete(&WatchRule{}).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Create(&rows).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	return s.WatchRules(org), nil
}

// ResetWatchRules restores the built-in topics.
func (s *Service) ResetWatchRules(org string) []WatchRule {
	s.DB.Where("organization_id = ?", org).Delete(&WatchRule{})
	return DefaultWatchRules()
}

// ---------- matching ----------

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// hasWord reports whether kw occurs in text (both lower-case) as a whole word
// or phrase. A plural "s" / "es" after the keyword is accepted.
func hasWord(text, kw string) bool {
	if kw == "" {
		return false
	}
	first, _ := firstRune(kw)
	last, _ := lastRune(kw)
	for from := 0; from < len(text); {
		i := strings.Index(text[from:], kw)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(kw)
		okBefore := true
		if isWordRune(first) && i > 0 {
			r, _ := lastRune(text[:i])
			okBefore = !isWordRune(r)
		}
		okAfter := true
		if isWordRune(last) && end < len(text) {
			rest := text[end:]
			r, _ := firstRune(rest)
			if isWordRune(r) {
				okAfter = false
				for _, suf := range []string{"s", "es"} {
					if strings.HasPrefix(rest, suf) {
						after := rest[len(suf):]
						if after == "" {
							okAfter = true
						} else if r2, _ := firstRune(after); !isWordRune(r2) {
							okAfter = true
						}
					}
				}
			}
		}
		if okBefore && okAfter {
			return true
		}
		from = i + 1
	}
	return false
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}

func lastRune(s string) (rune, int) {
	r := []rune(s)
	if len(r) == 0 {
		return 0, 0
	}
	return r[len(r)-1], len(string(r[len(r)-1]))
}

// WatchReason is one "why it's relevant" chip.
type WatchReason struct {
	Kind   string  `json:"kind"` // topic | country
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Points float64 `json:"points"`
	Detail string  `json:"detail"`
}

// WatchContext is what relevance is scored against for one organisation.
type WatchContext struct {
	Rules      []WatchRule         `json:"-"`
	Home       string              `json:"home_country"`
	Markets    []string            `json:"client_countries"`
	TypeKey    string              `json:"business_type"`
	TypeName   string              `json:"business_type_name"`
	TypeTopics []string            `json:"business_topics"`
	Lines      []string            `json:"service_lines"` // service lines sold in the last 12 months
	LineTopics map[string][]string `json:"line_topics"`   // topic → labels of the lines that boost it
	markets    map[string]bool
	typeTopics map[string]bool
}

func (c *WatchContext) prepare() {
	c.markets, c.typeTopics = map[string]bool{}, map[string]bool{}
	for _, m := range c.Markets {
		c.markets[m] = true
	}
	for _, t := range c.TypeTopics {
		c.typeTopics[t] = true
	}
	if c.LineTopics == nil {
		c.LineTopics = map[string][]string{}
	}
}

// ScoreItem tags an item and scores its relevance. feedTopics / feedCountry
// are what the source is filed under.
func ScoreItem(c *WatchContext, title, summary string, feedTopics []string, feedCountry string) (topics, countries []string, score float64, reasons []WatchReason) {
	if c.markets == nil {
		c.prepare()
	}
	t := strings.ToLower(title)
	body := t + " \n " + strings.ToLower(summary)
	fromFeed := map[string]bool{}
	for _, k := range feedTopics {
		fromFeed[k] = true
	}
	topics, countries, reasons = []string{}, []string{}, []WatchReason{}
	for _, r := range c.Rules {
		if !r.Enabled {
			continue
		}
		hits := []string{}
		inTitle := false
		for _, kw := range r.Keywords {
			if hasWord(body, kw) {
				hits = append(hits, kw)
				if hasWord(t, kw) {
					inTitle = true
				}
			}
		}
		if len(hits) == 0 && !fromFeed[r.Key] {
			continue
		}
		topics = append(topics, r.Key)
		match := 0.5
		detail := "the source is filed under this topic"
		if len(hits) > 0 {
			switch {
			case len(hits) >= 3:
				match = 1.5
			case len(hits) == 2:
				match = 1.25
			default:
				match = 1
			}
			if inTitle {
				match += 0.25
			}
			show := hits
			if len(show) > 4 {
				show = show[:4]
			}
			detail = "matched “" + strings.Join(show, "”, “") + "”"
			if inTitle {
				detail += " (in the title)"
			}
		}
		boost := 1.0
		if c.typeTopics[r.Key] {
			boost += 0.5
			detail += " · matters to a " + nonEmpty(c.TypeName, "business like yours")
		}
		if ls := c.LineTopics[r.Key]; len(ls) > 0 {
			boost += 0.5
			detail += " · you sell " + strings.Join(ls, ", ")
		}
		pts := r1(topicBase * r.Weight * match * boost)
		score += pts
		reasons = append(reasons, WatchReason{Kind: "topic", Key: r.Key, Label: r.Label, Points: pts, Detail: detail})
	}
	seen := map[string]bool{}
	addCountry := func(cc string) {
		cc = strings.ToUpper(strings.TrimSpace(cc))
		if cc == "" || seen[cc] {
			return
		}
		seen[cc] = true
		countries = append(countries, cc)
	}
	for _, cc := range countryKeyOrder {
		for _, kw := range countryKeywords[cc] {
			if hasWord(body, kw) {
				addCountry(cc)
				break
			}
		}
	}
	addCountry(feedCountry)
	sort.Strings(countries)
	// Country points: the best one counts (home beats a client country).
	best, bestCC, bestWhy := 0.0, "", ""
	for _, cc := range countries {
		switch {
		case cc == c.Home && homeCountryPts > best:
			best, bestCC, bestWhy = homeCountryPts, cc, "your home market"
		case c.markets[cc] && marketPts > best:
			best, bestCC, bestWhy = marketPts, cc, "you have clients there"
		}
	}
	if best > 0 {
		score += best
		reasons = append(reasons, WatchReason{Kind: "country", Key: bestCC, Label: CountryName(bestCC), Points: best, Detail: bestWhy})
	}
	sort.SliceStable(reasons, func(i, j int) bool { return reasons[i].Points > reasons[j].Points })
	if score > 100 {
		score = 100
	}
	return topics, countries, r1(score), reasons
}
