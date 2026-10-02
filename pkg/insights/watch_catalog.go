package insights

// The default catalogue of industry-watch sources for a web / IT services
// business in Kenya that also sells to Australia.
//
// VERIFICATION STATUS — read before changing Verified:
//
//	NONE of these addresses has been verified. They were to be checked with a
//	live request (does the URL answer with RSS / Atom?) before being seeded,
//	but on 2026-10-02 the build environment could not reach any of the hosts:
//	the outbound proxy refused them by policy (HTTP 403 on CONNECT) and the
//	alternative fetch tool was not authorised for them. The addresses below
//	are therefore CANDIDATES from general knowledge of each site, and every
//	Verified field is empty.
//
// Because of that, nothing here is seeded as an active source. Unverified
// entries are only offered under "Suggested sources"; adding one makes a live
// request first and it is refused when the address does not return a readable
// feed (see Intel.AddFeed). An entry is seeded and switched on by default only
// once Verified holds the date somebody confirmed it:
//
//	WATCH_VERIFY_LIVE=1 go test ./pkg/insights -run TestCatalogueLive -v
//
// fetches every entry with the production fetcher and prints which ones
// answer with a feed; put that day's date in Verified for those, and delete
// (or correct) the ones that fail.

// CatalogFeed is one suggested source.
type CatalogFeed struct {
	Key     string   `json:"key"`
	Title   string   `json:"title"`
	URL     string   `json:"url"`
	Site    string   `json:"site"`
	Group   string   `json:"group"`   // local | regulator | vendor | trade
	Country string   `json:"country"` // ISO2, "" = international
	Topics  []string `json:"topics"`
	About   string   `json:"about"`
	// Verified is the date (YYYY-MM-DD) the address was confirmed to serve
	// RSS / Atom; empty = never confirmed (not seeded, checked when added).
	Verified string `json:"verified"`
	// Types limits default-on seeding to these business types (empty = all).
	Types []string `json:"-"`
}

// Source groups, in display order.
var WatchGroups = []struct{ Key, Label string }{
	{"local", "Local tech & business"}, {"regulator", "Regulators"}, {"vendor", "Platforms & vendors"}, {"trade", "Trade & export"}, {"custom", "Added by you"},
}

var techTypes = []string{"agency", "consulting", "general", ""}

// DefaultCatalog lists the candidate sources (all unverified, see above).
var DefaultCatalog = []CatalogFeed{
	// --- local tech & business ---
	{Key: "techweez", Title: "Techweez", URL: "https://techweez.com/feed/", Site: "https://techweez.com", Group: "local", Country: "KE",
		Topics: []string{}, About: "Kenyan technology news: telecoms, fintech, devices, policy.", Types: techTypes},
	{Key: "techcabal", Title: "TechCabal", URL: "https://techcabal.com/feed/", Site: "https://techcabal.com", Group: "local",
		Topics: []string{}, About: "African technology and startups: funding, fintech, regulation.", Types: techTypes},
	{Key: "cioafrica", Title: "CIO Africa", URL: "https://cioafrica.co/feed/", Site: "https://cioafrica.co", Group: "local",
		Topics: []string{}, About: "Enterprise IT in Africa: cloud, security, digital transformation.", Types: techTypes},
	{Key: "businessdaily_tech", Title: "Business Daily Africa — Technology", URL: "https://www.businessdailyafrica.com/bd/corporate/technology/rss", Site: "https://www.businessdailyafrica.com",
		Group: "local", Country: "KE", Topics: []string{}, About: "Kenyan business coverage of technology and the economy."},
	{Key: "itnews_au", Title: "iTnews Australia", URL: "https://www.itnews.com.au/RSS/rss.ashx", Site: "https://www.itnews.com.au", Group: "local", Country: "AU",
		Topics: []string{}, About: "Australian business technology news.", Types: techTypes},
	// --- regulators ---
	{Key: "ca_kenya", Title: "Communications Authority of Kenya", URL: "https://www.ca.go.ke/rss.xml", Site: "https://www.ca.go.ke", Group: "regulator", Country: "KE",
		Topics: []string{TopicRegulation}, About: "Kenya's ICT regulator: licensing, sector statistics, consumer notices."},
	{Key: "kenic", Title: "KENIC (.ke registry)", URL: "https://kenic.or.ke/feed/", Site: "https://kenic.or.ke", Group: "regulator", Country: "KE",
		Topics: []string{TopicHosting}, About: "The .ke domain registry: policy, pricing and registrar notices.", Types: techTypes},
	{Key: "auda", Title: "auDA (.au domain administration)", URL: "https://www.auda.org.au/rss.xml", Site: "https://www.auda.org.au", Group: "regulator", Country: "AU",
		Topics: []string{TopicHosting, TopicRegulation}, About: ".au domain policy and registrar announcements.", Types: techTypes},
	{Key: "acsc_alerts", Title: "Australian Cyber Security Centre — alerts", URL: "https://www.cyber.gov.au/rss/alerts", Site: "https://www.cyber.gov.au", Group: "regulator", Country: "AU",
		Topics: []string{TopicSecurity}, About: "Security alerts and advisories for Australian organisations.", Types: techTypes},
	// --- platforms & vendors ---
	{Key: "google_workspace", Title: "Google Workspace Updates", URL: "https://workspaceupdates.googleblog.com/feeds/posts/default", Site: "https://workspaceupdates.googleblog.com",
		Group: "vendor", Topics: []string{TopicPlatforms}, About: "Feature releases and changes in Google Workspace.", Types: techTypes},
	{Key: "cloudflare", Title: "The Cloudflare Blog", URL: "https://blog.cloudflare.com/rss/", Site: "https://blog.cloudflare.com", Group: "vendor",
		Topics: []string{TopicCloud}, About: "Internet infrastructure, security and performance.", Types: techTypes},
	{Key: "wordpress_news", Title: "WordPress.org News", URL: "https://wordpress.org/news/feed/", Site: "https://wordpress.org/news/", Group: "vendor",
		Topics: []string{TopicPlatforms}, About: "WordPress releases and security updates.", Types: techTypes},
	{Key: "wptavern", Title: "WP Tavern", URL: "https://wptavern.com/feed", Site: "https://wptavern.com", Group: "vendor",
		Topics: []string{TopicPlatforms}, About: "WordPress ecosystem news: plugins, themes, hosting.", Types: techTypes},
	{Key: "synergy_wholesale", Title: "Synergy Wholesale", URL: "https://synergywholesale.com/feed/", Site: "https://synergywholesale.com", Group: "vendor", Country: "AU",
		Topics: []string{TopicHosting}, About: "Your hosting and domain wholesaler: product and pricing news.", Types: techTypes},
	{Key: "hosting_com", Title: "hosting.com blog", URL: "https://hosting.com/blog/feed/", Site: "https://hosting.com/blog/", Group: "vendor",
		Topics: []string{TopicHosting}, About: "Hosting industry and product news.", Types: techTypes},
	// --- trade & export ---
	{Key: "wto_news", Title: "WTO news", URL: "https://www.wto.org/library/rss/latest_news_e.xml", Site: "https://www.wto.org", Group: "trade",
		Topics: []string{TopicTrade}, About: "World Trade Organization: services trade, e-commerce, agreements."},
	{Key: "unctad", Title: "UNCTAD", URL: "https://unctad.org/rss.xml", Site: "https://unctad.org", Group: "trade",
		Topics: []string{TopicTrade}, About: "UN Trade and Development: digital economy and services trade."},
	{Key: "itc", Title: "International Trade Centre", URL: "https://www.intracen.org/rss.xml", Site: "https://www.intracen.org", Group: "trade",
		Topics: []string{TopicTrade}, About: "Support for small-business exporters in developing countries."},
	{Key: "eac", Title: "East African Community", URL: "https://www.eac.int/press-releases?format=feed&type=rss", Site: "https://www.eac.int", Group: "trade",
		Topics: []string{TopicTrade}, About: "EAC press releases: regional trade, customs and common market."},
	{Key: "kentrade", Title: "KenTrade", URL: "https://www.kentrade.go.ke/feed/", Site: "https://www.kentrade.go.ke", Group: "trade", Country: "KE",
		Topics: []string{TopicTrade}, About: "Kenya Trade Network Agency: trade facilitation notices."},
}

// CatalogByKey finds a catalogue entry.
func CatalogByKey(key string) *CatalogFeed {
	for i := range DefaultCatalog {
		if DefaultCatalog[i].Key == key {
			return &DefaultCatalog[i]
		}
	}
	return nil
}

// seedByDefault says whether a verified catalogue entry starts switched on
// for an organisation: sources about a country only where the organisation is
// based or has clients, and trade-specific ones only for matching business types.
func seedByDefault(c CatalogFeed, home string, markets map[string]bool, typeKey string) bool {
	if c.Verified == "" {
		return false
	}
	if c.Country != "" && c.Country != home && !markets[c.Country] {
		return false
	}
	if len(c.Types) == 0 {
		return true
	}
	for _, t := range c.Types {
		if t == typeKey {
			return true
		}
	}
	return false
}
