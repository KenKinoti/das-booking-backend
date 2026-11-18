package scout

import (
	"log"
	"math"
	"sort"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/insights"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// The business profile is built from the organisation's own data — never
// with client names: what it sells comes from the service lines of its
// invoice lines (pkg/insights classification) and its catalogue (services,
// products); where its customers are and their sectors are counts.

// sectorRules classify a customer's (company) name into a sector.
var sectorRules = []struct {
	Key, Name string
	Words     []string
}{
	{"ngo", "NGOs, CBOs & community groups", []string{"cbo", "ngo", "foundation", "trust", "women group", "womens group", "youth group", "self help", "self-help", "community", "association", "society", "initiative", "charity", "organisation", "organization", "network", "welfare", "resource centre", "resource center", "knowledge center", "knowledge centre"}},
	{"education", "Schools, colleges & training", []string{"school", "college", "academy", "university", "institute", "polytechnic", "training", "education", "kindergarten"}},
	{"faith", "Churches & faith groups", []string{"church", "ministry", "ministries", "mosque", "parish", "diocese", "fellowship", "chapel", "cathedral"}},
	{"health", "Health & care", []string{"clinic", "hospital", "medical", "health", "pharmacy", "dental", "care", "wellness", "ndis"}},
	{"hospitality", "Hospitality, travel & nightlife", []string{"hotel", "lodge", "resort", "restaurant", "cafe", "café", "bar", "club", "lounge", "camp", "safari", "tours", "travel", "bistro", "grill"}},
	{"agri", "Farming & agribusiness", []string{"farm", "agri", "dairy", "co-operative", "cooperative", "coop", "valley", "ranch", "growers"}},
	{"finance", "Finance & SACCOs", []string{"sacco", "microfinance", "capital", "investment", "insurance", "bank", "credit"}},
	{"government", "Government & public bodies", []string{"county", "government", "authority", "council", "commission", "board", "parastatal"}},
	{"construction", "Construction & property", []string{"construction", "builders", "building", "properties", "realty", "real estate", "estate", "homes", "engineering"}},
	{"professional", "Professional services", []string{"consult", "advocates", "law", "legal", "accountants", "accounting", "associates", "partners", "agency", "marketing"}},
	{"tech", "Technology & IT", []string{"tech", "solutions", "systems", "digital", "software", "ict", "networks", "web", "data", "cloud"}},
	{"retail", "Retail & trade", []string{"shop", "store", "supermarket", "mart", "traders", "trading", "enterprises", "supplies", "wholesale", "boutique", "salon", "beauty", "motors", "auto"}},
}

// SectorOf classifies a name ("" when nothing matches).
func SectorOf(name string) (key, label string) {
	n := " " + strings.ToLower(name) + " "
	for _, r := range sectorRules {
		for _, w := range r.Words {
			// Short words must stand alone ("bar" in "Barasa" is not a bar).
			if len(w) <= 4 {
				if containsWord(n, w) {
					return r.Key, r.Name
				}
				continue
			}
			if strings.Contains(n, w) {
				return r.Key, r.Name
			}
		}
	}
	return "", ""
}

func containsWord(hay, w string) bool {
	i := 0
	for {
		j := strings.Index(hay[i:], w)
		if j < 0 {
			return false
		}
		a, b := i+j, i+j+len(w)
		left := a == 0 || !isWordByte(hay[a-1])
		right := b >= len(hay) || !isWordByte(hay[b])
		if left && right {
			return true
		}
		i = a + 1
	}
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c >= 0x80
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

// quiet is the database without SQL logging (a missing optional table is
// not an error worth a log line).
func quiet(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{NewDB: true, Logger: logger.Default.LogMode(logger.Silent)})
}

// BuildProfile reads the organisation's data. It never fails: missing
// tables or data just leave parts empty.
func (s *Service) BuildProfile(org string) Profile {
	db := quiet(s.DB)
	p := orgprefs.Get(s.DB, org)
	pr := Profile{HomeCountry: p.CountryCode, Currency: p.Currency, Timezone: p.Timezone, Offerings: []Offering{}, Countries: []Share{}, Sectors: []Share{}, TargetMarkets: []string{}}
	var name string
	db.Raw(`SELECT COALESCE(name,'') FROM organizations WHERE id = ?`, org).Row().Scan(&name)
	pr.OrgName = strings.TrimSpace(name)
	if s.BusinessType != nil {
		pr.BusinessType = s.BusinessType(org)
	}

	// --- What it sells: invoice lines (last 24 months) by service line.
	since := s.now().AddDate(-2, 0, 0).Format("2006-01-02")
	var lines []struct {
		Description string
		UnitPrice   float64
		Currency    string
	}
	if err := db.Raw(`SELECT COALESCE(it.description,'') AS description, COALESCE(it.unit_price,0) AS unit_price, UPPER(COALESCE(NULLIF(i.currency,''), ?)) AS currency
		FROM billing_invoice_items it JOIN billing_invoices i ON i.id = it.invoice_id
		WHERE i.organization_id = ? AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status NOT IN ('draft','void') AND i.issue_date >= ?
		ORDER BY i.issue_date DESC LIMIT 5000`, p.Currency, org, since).Scan(&lines).Error; err != nil {
		log.Printf("scout: profile: invoice lines: %v", err)
	}
	rules := (&insights.Service{DB: db}).Rules(org)
	type agg struct {
		n      int
		prices map[string][]float64
	}
	byLine := map[string]*agg{}
	for _, l := range lines {
		line := insights.Classify(rules, l.Description)
		a := byLine[line]
		if a == nil {
			a = &agg{prices: map[string][]float64{}}
			byLine[line] = a
		}
		a.n++
		if l.UnitPrice > 0 {
			a.prices[l.Currency] = append(a.prices[l.Currency], l.UnitPrice)
		}
	}
	for _, line := range insights.LineOrder {
		a := byLine[line]
		if a == nil || line == insights.LineOther {
			continue
		}
		o := Offering{Name: insights.LineLabels[line], Line: insights.LineLabels[line], Source: "invoices", Sold: a.n}
		best := 0
		for cur, ps := range a.prices {
			if len(ps) > best {
				best, o.Currency, o.Price = len(ps), cur, math.Round(median(ps))
			}
		}
		pr.Offerings = append(pr.Offerings, o)
	}
	sort.SliceStable(pr.Offerings, func(i, j int) bool { return pr.Offerings[i].Sold > pr.Offerings[j].Sold })

	// --- Catalogue: services and products by name (no client data there).
	var svcs []struct {
		Name     string
		Price    float64
		Currency string
	}
	db.Raw(`SELECT name, COALESCE(price,0) AS price, UPPER(COALESCE(NULLIF(currency,''), ?)) AS currency FROM services
		WHERE organization_id = ? AND deleted_at IS NULL AND COALESCE(is_active, true) ORDER BY price DESC LIMIT 20`, p.Currency, org).Scan(&svcs)
	for _, x := range svcs {
		if n := clip(cleanText(x.Name), 80); n != "" {
			pr.Offerings = append(pr.Offerings, Offering{Name: n, Line: lineOf(rules, n), Price: x.Price, Currency: x.Currency, Source: "services"})
		}
	}
	var prods []struct {
		Name  string
		Price float64
	}
	db.Raw(`SELECT name, COALESCE(selling_price,0) AS price FROM products
		WHERE organization_id = ? AND deleted_at IS NULL AND COALESCE(is_active, true) ORDER BY selling_price DESC NULLS LAST LIMIT 12`, org).Scan(&prods)
	for _, x := range prods {
		if n := clip(cleanText(x.Name), 80); n != "" {
			pr.Offerings = append(pr.Offerings, Offering{Name: n, Line: lineOf(rules, n), Price: x.Price, Currency: p.Currency, Source: "products"})
		}
	}
	if len(pr.Offerings) > 24 {
		pr.Offerings = pr.Offerings[:24]
	}

	// --- Customers: countries and sectors (counts only).
	var custs []struct {
		Name, CountryCode, Currency string
	}
	db.Raw(`SELECT COALESCE(NULLIF(TRIM(company_name),''), TRIM(COALESCE(first_name,'') || ' ' || COALESCE(last_name,''))) AS name,
		UPPER(COALESCE(country_code,'')) AS country_code, UPPER(COALESCE(currency,'')) AS currency
		FROM customers WHERE organization_id = ? AND deleted_at IS NULL LIMIT 20000`, org).Scan(&custs)
	cc := map[string]int{}
	sec := map[string]int{}
	secName := map[string]string{}
	for _, c := range custs {
		code := c.CountryCode
		if !currency.IsCountry(code) {
			code = orgprefs.CountryForCurrency(c.Currency)
		}
		if !currency.IsCountry(code) {
			code = p.CountryCode // unknown: counted at home
		}
		if code != "" {
			cc[code]++
		}
		if k, label := SectorOf(c.Name); k != "" {
			sec[k]++
			secName[k] = label
		} else if strings.TrimSpace(c.Name) != "" {
			sec["other"]++
			secName["other"] = "Other businesses & individuals"
		}
	}
	total := len(custs)
	for code, n := range cc {
		pr.Countries = append(pr.Countries, Share{Key: code, Name: currency.CountryName(code), Customers: n, SharePct: pct(n, total)})
	}
	sort.Slice(pr.Countries, func(i, j int) bool {
		if pr.Countries[i].Customers != pr.Countries[j].Customers {
			return pr.Countries[i].Customers > pr.Countries[j].Customers
		}
		return pr.Countries[i].Key < pr.Countries[j].Key
	})
	for k, n := range sec {
		pr.Sectors = append(pr.Sectors, Share{Key: k, Name: secName[k], Customers: n, SharePct: pct(n, total)})
	}
	sort.Slice(pr.Sectors, func(i, j int) bool {
		if (pr.Sectors[i].Key == "other") != (pr.Sectors[j].Key == "other") {
			return pr.Sectors[j].Key == "other"
		}
		if pr.Sectors[i].Customers != pr.Sectors[j].Customers {
			return pr.Sectors[i].Customers > pr.Sectors[j].Customers
		}
		return pr.Sectors[i].Key < pr.Sectors[j].Key
	})

	// --- Target markets saved in Insights → Trade & exports.
	var ins insights.IntelSettings
	if db.Where("organization_id = ?", org).Limit(1).Find(&ins).Error == nil {
		for _, m := range ins.TargetMarkets {
			if m = strings.ToUpper(strings.TrimSpace(m)); currency.IsCountry(m) {
				pr.TargetMarkets = append(pr.TargetMarkets, m)
			}
		}
	}
	return pr
}

func lineOf(rules []insights.ServiceRule, name string) string {
	if l := insights.Classify(rules, name); l != insights.LineOther {
		return insights.LineLabels[l]
	}
	return ""
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return math.Round(float64(n)/float64(total)*1000) / 10
}

// DefaultFocus is the first focus: the countries the business serves and
// targets, its main customer sectors, what it sells as keywords, every
// opportunity type and the suggested sources for those countries.
func DefaultFocus(p Profile) Focus {
	f := Focus{Types: []string{TypeTender, TypeGrant, TypePartnership, TypeLead, TypeNewMarket, TypeProduct}, MinValueCurrency: p.Currency,
		Countries: []string{}, Regions: []string{}, Sectors: []string{}, Keywords: []string{}, Exclusions: []string{}, BlockedDomains: []string{}}
	seen := map[string]bool{}
	add := func(c string) {
		c = strings.ToUpper(c)
		if c != "" && !seen[c] && len(f.Countries) < 8 {
			seen[c] = true
			f.Countries = append(f.Countries, c)
		}
	}
	add(p.HomeCountry)
	for i, c := range p.Countries {
		if i < 4 {
			add(c.Key)
		}
	}
	for _, c := range p.TargetMarkets {
		add(c)
	}
	for _, s := range p.Sectors {
		if s.Key != "other" && len(f.Sectors) < 5 {
			f.Sectors = append(f.Sectors, s.Name)
		}
	}
	kw := map[string]bool{}
	for _, o := range p.Offerings {
		k := strings.ToLower(o.Name)
		if o.Source == "invoices" && !kw[k] && len(f.Keywords) < 8 {
			kw[k] = true
			f.Keywords = append(f.Keywords, o.Name)
		}
	}
	for _, o := range p.Offerings {
		k := strings.ToLower(o.Name)
		if !kw[k] && len(f.Keywords) < 8 {
			kw[k] = true
			f.Keywords = append(f.Keywords, o.Name)
		}
	}
	f.Sources = SuggestFor(f.Countries)
	return f
}

// cleanFocus validates a focus sent by the owner.
func cleanFocus(f Focus, home string) Focus {
	list := func(in []string, max, n int) []string {
		out := []string{}
		seen := map[string]bool{}
		for _, s := range in {
			s = clip(cleanText(s), n)
			k := strings.ToLower(s)
			if s == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, s)
			if len(out) == max {
				break
			}
		}
		return out
	}
	out := Focus{Regions: list(f.Regions, 10, 60), Sectors: list(f.Sectors, 15, 60), Keywords: list(f.Keywords, 25, 60), Exclusions: list(f.Exclusions, 25, 60),
		Notes: clip(strings.TrimSpace(f.Notes), 1000), OnlySources: f.OnlySources, Countries: []string{}, Types: []string{}, Sources: []Source{}, BlockedDomains: []string{}}
	seen := map[string]bool{}
	for _, c := range f.Countries {
		if cc := countryCode(c); cc != "" && !seen[cc] && len(out.Countries) < 20 {
			seen[cc] = true
			out.Countries = append(out.Countries, cc)
		}
	}
	for _, t := range f.Types {
		if nt := normType(t); nt != "" && !seen["t:"+nt] {
			seen["t:"+nt] = true
			out.Types = append(out.Types, nt)
		}
	}
	if f.MinValue > 0 && f.MinValue < MaxValue {
		out.MinValue = math.Round(f.MinValue*100) / 100
	}
	out.MinValueCurrency = normCurrency(f.MinValueCurrency, home)
	for _, s := range f.Sources {
		d := CleanDomain(s.Domain)
		if d == "" || seen["d:"+d] || len(out.Sources) >= 30 {
			continue
		}
		seen["d:"+d] = true
		out.Sources = append(out.Sources, Source{Domain: d, Label: clip(cleanText(s.Label), 80), Country: countryCode(s.Country)})
	}
	for _, b := range f.BlockedDomains {
		if d := CleanDomain(b); d != "" && !seen["b:"+d] && len(out.BlockedDomains) < 30 {
			seen["b:"+d] = true
			out.BlockedDomains = append(out.BlockedDomains, d)
		}
	}
	if len(out.Sources) == 0 {
		out.OnlySources = false
	}
	return out
}

// cleanProfile keeps the owner's edits within limits.
func cleanProfile(p Profile) Profile {
	p.OrgName = clip(cleanText(p.OrgName), 120)
	p.BusinessType = clip(cleanText(p.BusinessType), 80)
	p.Description = clip(strings.TrimSpace(p.Description), 1500)
	p.HomeCountry = countryCode(p.HomeCountry)
	offs := []Offering{}
	for _, o := range p.Offerings {
		o.Name = clip(cleanText(o.Name), 80)
		if o.Name == "" {
			continue
		}
		o.Line = clip(cleanText(o.Line), 60)
		if o.Price < 0 || o.Price > MaxValue || math.IsNaN(o.Price) {
			o.Price = 0
		}
		o.Currency = normCurrency(o.Currency, p.Currency)
		if o.Source == "" {
			o.Source = "manual"
		}
		offs = append(offs, o)
		if len(offs) == 40 {
			break
		}
	}
	p.Offerings = offs
	if p.Countries == nil {
		p.Countries = []Share{}
	}
	if p.Sectors == nil {
		p.Sectors = []Share{}
	}
	if p.TargetMarkets == nil {
		p.TargetMarkets = []string{}
	}
	return p
}
