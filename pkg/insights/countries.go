package insights

import (
	"sort"
	"strings"
)

// CountryInfo is a market the trade views know: ISO 3166-1 alpha-2 / alpha-3
// (the World Bank API answers with alpha-3), display name and main currency.
type CountryInfo struct {
	ISO2     string `json:"iso2"`
	ISO3     string `json:"iso3"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

// Countries is the list offered for target markets and used to label client
// countries. Kept short on purpose: the markets a services exporter in East
// Africa is likely to sell to. Any other alpha-2 code still works in the trade
// tables (it is shown by its code).
var Countries = []CountryInfo{
	{"KE", "KEN", "Kenya", "KES"}, {"AU", "AUS", "Australia", "AUD"}, {"UG", "UGA", "Uganda", "UGX"}, {"TZ", "TZA", "Tanzania", "TZS"},
	{"RW", "RWA", "Rwanda", "RWF"}, {"GB", "GBR", "United Kingdom", "GBP"}, {"US", "USA", "United States", "USD"},
	{"AE", "ARE", "United Arab Emirates", "AED"}, {"ZA", "ZAF", "South Africa", "ZAR"}, {"NG", "NGA", "Nigeria", "NGN"},
	{"GH", "GHA", "Ghana", "GHS"}, {"ET", "ETH", "Ethiopia", "ETB"}, {"BI", "BDI", "Burundi", "BIF"}, {"SS", "SSD", "South Sudan", "SSP"},
	{"CD", "COD", "DR Congo", "CDF"}, {"SO", "SOM", "Somalia", "SOS"}, {"ZM", "ZMB", "Zambia", "ZMW"}, {"MW", "MWI", "Malawi", "MWK"},
	{"MZ", "MOZ", "Mozambique", "MZN"}, {"BW", "BWA", "Botswana", "BWP"}, {"NA", "NAM", "Namibia", "NAD"}, {"MU", "MUS", "Mauritius", "MUR"},
	{"EG", "EGY", "Egypt", "EGP"}, {"MA", "MAR", "Morocco", "MAD"}, {"SN", "SEN", "Senegal", "XOF"}, {"CI", "CIV", "Côte d'Ivoire", "XOF"},
	{"NZ", "NZL", "New Zealand", "NZD"}, {"CA", "CAN", "Canada", "CAD"}, {"IE", "IRL", "Ireland", "EUR"}, {"DE", "DEU", "Germany", "EUR"},
	{"FR", "FRA", "France", "EUR"}, {"NL", "NLD", "Netherlands", "EUR"}, {"ES", "ESP", "Spain", "EUR"}, {"IT", "ITA", "Italy", "EUR"},
	{"BE", "BEL", "Belgium", "EUR"}, {"SE", "SWE", "Sweden", "SEK"}, {"NO", "NOR", "Norway", "NOK"}, {"DK", "DNK", "Denmark", "DKK"},
	{"CH", "CHE", "Switzerland", "CHF"}, {"IN", "IND", "India", "INR"}, {"SG", "SGP", "Singapore", "SGD"}, {"MY", "MYS", "Malaysia", "MYR"},
	{"CN", "CHN", "China", "CNY"}, {"JP", "JPN", "Japan", "JPY"}, {"HK", "HKG", "Hong Kong", "HKD"}, {"SA", "SAU", "Saudi Arabia", "SAR"},
	{"QA", "QAT", "Qatar", "QAR"}, {"IL", "ISR", "Israel", "ILS"}, {"TR", "TUR", "Türkiye", "TRY"}, {"BR", "BRA", "Brazil", "BRL"},
	{"MX", "MEX", "Mexico", "MXN"}, {"PH", "PHL", "Philippines", "PHP"}, {"ID", "IDN", "Indonesia", "IDR"}, {"PK", "PAK", "Pakistan", "PKR"},
	{"BD", "BGD", "Bangladesh", "BDT"}, {"FJ", "FJI", "Fiji", "FJD"}, {"PG", "PNG", "Papua New Guinea", "PGK"},
}

var (
	countryByISO2 = map[string]CountryInfo{}
	countryByISO3 = map[string]CountryInfo{}
	// currencyHome is the country a currency is taken to belong to when a
	// document has no client country (first match in Countries; EUR has none).
	currencyHome = map[string]string{}
)

func init() {
	for _, c := range Countries {
		countryByISO2[c.ISO2] = c
		countryByISO3[c.ISO3] = c
		if c.Currency != "EUR" && c.Currency != "XOF" {
			if _, ok := currencyHome[c.Currency]; !ok {
				currencyHome[c.Currency] = c.ISO2
			}
		}
	}
}

// CountryOf looks a country up by alpha-2 or alpha-3 code.
func CountryOf(code string) (CountryInfo, bool) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if c, ok := countryByISO2[code]; ok {
		return c, true
	}
	c, ok := countryByISO3[code]
	return c, ok
}

// CountryName is the display name of an alpha-2 code (the code itself when unknown).
func CountryName(iso2 string) string {
	if c, ok := countryByISO2[strings.ToUpper(iso2)]; ok {
		return c.Name
	}
	if iso2 == "" {
		return "Unknown"
	}
	return strings.ToUpper(iso2)
}

// CountriesSorted returns the list ordered by name (for pickers).
func CountriesSorted() []CountryInfo {
	out := append([]CountryInfo(nil), Countries...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
