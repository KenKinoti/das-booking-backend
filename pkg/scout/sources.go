package scout

import "strings"

// Suggested sources to favour, by country ("" = international). Each was
// checked to exist when this list was written (Nov 2026):
//
//	tenders.go.ke            Kenya Public Procurement Information Portal (PPIP) — https://openopps.com/sources/kenya-ppip/
//	egpkenya.go.ke           Kenya electronic Government Procurement (e-GP), live since April 2025 —
//	                         https://thekenyatimes.com/latest-kenya-times-news/how-to-register-and-bid-for-govt-tenders-in-the-new-procurement-system/
//	agpo.go.ke               Access to Government Procurement Opportunities (youth, women, PWD) — https://jorpex.com/guides/find-tenders-kenya/
//	mygov.go.ke/all-tenders  MyGov tender adverts (same guide)
//	nairobi.go.ke, narok.go.ke, kajiado.go.ke  county governments publishing tenders (search results, Nov 2026)
//	tenders.gov.au           AusTender — https://openopps.com/sources/austender/
//	grants.gov.au            GrantConnect (RSS at https://www.grants.gov.au/public_data/rss/rss.xml)
//	business.gov.au          grants & programs finder — https://business.gov.au/people/contractors/find-tenders-and-contracts
//	buy.nsw.gov.au, tenders.vic.gov.au, qtenders.epw.qld.gov.au  state portals —
//	                         https://www.business.qld.gov.au/running-business/marketing-sales/tendering/improve-approach/capability/search-government-tenders
//	ausconnect.dfat.gov.au   DFAT aid procurement — https://www.dfat.gov.au/about-us/business-opportunities/Pages/aid-procurement-pipeline
//	ungm.org                 UN Global Marketplace — https://www.ungm.org/Shared/KnowledgeCenter/Pages/AfDB
//	afdb.org                 African Development Bank procurement — https://openopps.com/sources/afdb/
//	projects.worldbank.org   World Bank procurement notices — https://openopps.com/sources/world-bank/
//	fundsforngos.org         grants for NGOs / CBOs — https://www2.fundsforngos.org/tag/funding-opportunities-and-resources-in-kenya/page/2/
//	developmentaid.org       donor grants and tenders — https://www.developmentaid.org/donors/view/118347/grants
var SuggestedSources = []Source{
	{Domain: "tenders.go.ke", Label: "Kenya PPIP — public tenders portal", Country: "KE"},
	{Domain: "egpkenya.go.ke", Label: "Kenya e-GP — government e-procurement", Country: "KE"},
	{Domain: "agpo.go.ke", Label: "AGPO — youth, women & PWD procurement", Country: "KE"},
	{Domain: "mygov.go.ke", Label: "MyGov Kenya — tender adverts", Country: "KE"},
	{Domain: "nairobi.go.ke", Label: "Nairobi City County", Country: "KE"},
	{Domain: "narok.go.ke", Label: "County Government of Narok", Country: "KE"},
	{Domain: "kajiado.go.ke", Label: "County Government of Kajiado", Country: "KE"},
	{Domain: "tenders.gov.au", Label: "AusTender — Australian Government tenders", Country: "AU"},
	{Domain: "grants.gov.au", Label: "GrantConnect — Australian Government grants", Country: "AU"},
	{Domain: "business.gov.au", Label: "business.gov.au — grants & programs", Country: "AU"},
	{Domain: "buy.nsw.gov.au", Label: "buy.nsw — NSW Government", Country: "AU"},
	{Domain: "tenders.vic.gov.au", Label: "Tenders VIC", Country: "AU"},
	{Domain: "qtenders.epw.qld.gov.au", Label: "QTenders — Queensland Government", Country: "AU"},
	{Domain: "ausconnect.dfat.gov.au", Label: "DFAT — Australian aid procurement", Country: "AU"},
	{Domain: "ungm.org", Label: "UNGM — United Nations Global Marketplace"},
	{Domain: "afdb.org", Label: "African Development Bank — procurement"},
	{Domain: "projects.worldbank.org", Label: "World Bank — procurement notices"},
	{Domain: "fundsforngos.org", Label: "FundsforNGOs — grants for NGOs & CBOs"},
	{Domain: "developmentaid.org", Label: "DevelopmentAid — donor grants & tenders"},
}

// SuggestFor returns the suggested sources for the given countries plus the
// international ones.
func SuggestFor(countries []string) []Source {
	want := map[string]bool{}
	for _, c := range countries {
		want[strings.ToUpper(c)] = true
	}
	out := []Source{}
	for _, s := range SuggestedSources {
		if s.Country == "" || want[s.Country] {
			out = append(out, s)
		}
	}
	return out
}
