package currency

import (
	"strings"
	"unicode"
)

// accents folds the accented letters that appear in country names.
var accents = strings.NewReplacer("å", "a", "á", "a", "à", "a", "ã", "a", "â", "a", "ä", "a", "ç", "c", "é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ï", "i", "ñ", "n", "ó", "o", "ô", "o", "ö", "o", "õ", "o", "ú", "u", "ü", "u")

// Country is an ISO 3166-1 alpha-2 country with its common English name.
type Country struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// countryData is "CODE:Name" pairs separated by "|" (ISO 3166-1 alpha-2).
// Keep in sync with das-booking-frontend/src/utils/currencies.js (COUNTRIES).
const countryData = "AD:Andorra|AE:United Arab Emirates|AF:Afghanistan|AG:Antigua and Barbuda|AI:Anguilla|AL:Albania|AM:Armenia|AO:Angola|AQ:Antarctica|AR:Argentina|AS:American Samoa|AT:Austria|AU:Australia|AW:Aruba|AX:Åland Islands|AZ:Azerbaijan|" +
	"BA:Bosnia and Herzegovina|BB:Barbados|BD:Bangladesh|BE:Belgium|BF:Burkina Faso|BG:Bulgaria|BH:Bahrain|BI:Burundi|BJ:Benin|BL:Saint Barthélemy|BM:Bermuda|BN:Brunei|BO:Bolivia|BQ:Caribbean Netherlands|BR:Brazil|BS:Bahamas|BT:Bhutan|BV:Bouvet Island|BW:Botswana|BY:Belarus|BZ:Belize|" +
	"CA:Canada|CC:Cocos (Keeling) Islands|CD:DR Congo|CF:Central African Republic|CG:Congo|CH:Switzerland|CI:Côte d'Ivoire|CK:Cook Islands|CL:Chile|CM:Cameroon|CN:China|CO:Colombia|CR:Costa Rica|CU:Cuba|CV:Cape Verde|CW:Curaçao|CX:Christmas Island|CY:Cyprus|CZ:Czechia|" +
	"DE:Germany|DJ:Djibouti|DK:Denmark|DM:Dominica|DO:Dominican Republic|DZ:Algeria|EC:Ecuador|EE:Estonia|EG:Egypt|EH:Western Sahara|ER:Eritrea|ES:Spain|ET:Ethiopia|FI:Finland|FJ:Fiji|FK:Falkland Islands|FM:Micronesia|FO:Faroe Islands|FR:France|" +
	"GA:Gabon|GB:United Kingdom|GD:Grenada|GE:Georgia|GF:French Guiana|GG:Guernsey|GH:Ghana|GI:Gibraltar|GL:Greenland|GM:Gambia|GN:Guinea|GP:Guadeloupe|GQ:Equatorial Guinea|GR:Greece|GS:South Georgia and the South Sandwich Islands|GT:Guatemala|GU:Guam|GW:Guinea-Bissau|GY:Guyana|" +
	"HK:Hong Kong|HM:Heard Island and McDonald Islands|HN:Honduras|HR:Croatia|HT:Haiti|HU:Hungary|ID:Indonesia|IE:Ireland|IL:Israel|IM:Isle of Man|IN:India|IO:British Indian Ocean Territory|IQ:Iraq|IR:Iran|IS:Iceland|IT:Italy|JE:Jersey|JM:Jamaica|JO:Jordan|JP:Japan|" +
	"KE:Kenya|KG:Kyrgyzstan|KH:Cambodia|KI:Kiribati|KM:Comoros|KN:Saint Kitts and Nevis|KP:North Korea|KR:South Korea|KW:Kuwait|KY:Cayman Islands|KZ:Kazakhstan|LA:Laos|LB:Lebanon|LC:Saint Lucia|LI:Liechtenstein|LK:Sri Lanka|LR:Liberia|LS:Lesotho|LT:Lithuania|LU:Luxembourg|LV:Latvia|LY:Libya|" +
	"MA:Morocco|MC:Monaco|MD:Moldova|ME:Montenegro|MF:Saint Martin|MG:Madagascar|MH:Marshall Islands|MK:North Macedonia|ML:Mali|MM:Myanmar|MN:Mongolia|MO:Macao|MP:Northern Mariana Islands|MQ:Martinique|MR:Mauritania|MS:Montserrat|MT:Malta|MU:Mauritius|MV:Maldives|MW:Malawi|MX:Mexico|MY:Malaysia|MZ:Mozambique|" +
	"NA:Namibia|NC:New Caledonia|NE:Niger|NF:Norfolk Island|NG:Nigeria|NI:Nicaragua|NL:Netherlands|NO:Norway|NP:Nepal|NR:Nauru|NU:Niue|NZ:New Zealand|OM:Oman|PA:Panama|PE:Peru|PF:French Polynesia|PG:Papua New Guinea|PH:Philippines|PK:Pakistan|PL:Poland|PM:Saint Pierre and Miquelon|PN:Pitcairn Islands|PR:Puerto Rico|PS:Palestine|PT:Portugal|PW:Palau|PY:Paraguay|" +
	"QA:Qatar|RE:Réunion|RO:Romania|RS:Serbia|RU:Russia|RW:Rwanda|SA:Saudi Arabia|SB:Solomon Islands|SC:Seychelles|SD:Sudan|SE:Sweden|SG:Singapore|SH:Saint Helena|SI:Slovenia|SJ:Svalbard and Jan Mayen|SK:Slovakia|SL:Sierra Leone|SM:San Marino|SN:Senegal|SO:Somalia|SR:Suriname|SS:South Sudan|ST:São Tomé and Príncipe|SV:El Salvador|SX:Sint Maarten|SY:Syria|SZ:Eswatini|" +
	"TC:Turks and Caicos Islands|TD:Chad|TF:French Southern Territories|TG:Togo|TH:Thailand|TJ:Tajikistan|TK:Tokelau|TL:Timor-Leste|TM:Turkmenistan|TN:Tunisia|TO:Tonga|TR:Türkiye|TT:Trinidad and Tobago|TV:Tuvalu|TW:Taiwan|TZ:Tanzania|UA:Ukraine|UG:Uganda|UM:U.S. Outlying Islands|US:United States|UY:Uruguay|UZ:Uzbekistan|" +
	"VA:Vatican City|VC:Saint Vincent and the Grenadines|VE:Venezuela|VG:British Virgin Islands|VI:U.S. Virgin Islands|VN:Vietnam|VU:Vanuatu|WF:Wallis and Futuna|WS:Samoa|XK:Kosovo|YE:Yemen|YT:Mayotte|ZA:South Africa|ZM:Zambia|ZW:Zimbabwe"

// Countries lists every ISO 3166-1 alpha-2 country, sorted by code.
var Countries []Country

var countryByCode = map[string]string{}
var countryByName = map[string]string{}

// countryAliases maps common alternative spellings to a code (lower-case,
// accents stripped) so free-text addresses can be backfilled.
var countryAliases = map[string]string{
	"aus": "AU", "oz": "AU", "commonwealth of australia": "AU",
	"nz": "NZ", "aotearoa": "NZ",
	"usa": "US", "u.s.a.": "US", "u.s.": "US", "united states of america": "US", "america": "US",
	"uk": "GB", "u.k.": "GB", "great britain": "GB", "britain": "GB", "england": "GB", "scotland": "GB", "wales": "GB", "northern ireland": "GB",
	"uae": "AE", "u.a.e.": "AE", "emirates": "AE", "dubai": "AE", "abu dhabi": "AE",
	"ksa": "SA", "kingdom of saudi arabia": "SA",
	"holland": "NL", "the netherlands": "NL", "deutschland": "DE", "espana": "ES", "italia": "IT", "suisse": "CH", "schweiz": "CH",
	"republic of ireland": "IE", "eire": "IE",
	"korea": "KR", "republic of korea": "KR", "south korea": "KR",
	"russian federation": "RU", "viet nam": "VN", "czech republic": "CZ", "turkey": "TR",
	"ivory coast": "CI", "cote divoire": "CI", "cape verde": "CV", "cabo verde": "CV", "swaziland": "SZ", "burma": "MM", "macau": "MO",
	"peoples republic of china": "CN", "prc": "CN", "mainland china": "CN",
	"hongkong": "HK", "hong kong sar": "HK",
	"republic of kenya": "KE", "tanzania, united republic of": "TZ", "united republic of tanzania": "TZ",
	"democratic republic of the congo": "CD", "drc": "CD", "republic of the congo": "CG",
	"south africa, republic of": "ZA", "rsa": "ZA",
	"east timor": "TL", "vatican": "VA", "holy see": "VA", "macedonia": "MK", "the gambia": "GM", "the bahamas": "BS",
	"bolivia, plurinational state of": "BO", "iran, islamic republic of": "IR", "syrian arab republic": "SY", "lao pdr": "LA",
	"moldova, republic of": "MD", "brunei darussalam": "BN", "taiwan, province of china": "TW",
}

// euro area members and territories/microstates that use the euro (2026).
var euroCountries = []string{
	"AT", "BE", "BG", "HR", "CY", "EE", "FI", "FR", "DE", "GR", "IE", "IT", "LV", "LT", "LU", "MT", "NL", "PT", "SK", "SI", "ES",
	"AD", "MC", "SM", "VA", "ME", "XK", "AX", "BL", "GF", "GP", "MF", "MQ", "PM", "RE", "YT", "TF",
}

// countryCurrency maps a country code to its (supported) currency.
var countryCurrency = map[string]string{
	"AU": "AUD", "CX": "AUD", "CC": "AUD", "NF": "AUD", "KI": "AUD", "NR": "AUD", "TV": "AUD", "HM": "AUD",
	"NZ": "NZD", "CK": "NZD", "NU": "NZD", "PN": "NZD", "TK": "NZD",
	"US": "USD", "PR": "USD", "GU": "USD", "AS": "USD", "MP": "USD", "VI": "USD", "UM": "USD", "EC": "USD", "SV": "USD", "PA": "USD",
	"TL": "USD", "FM": "USD", "MH": "USD", "PW": "USD", "VG": "USD", "TC": "USD", "BQ": "USD", "IO": "USD",
	"GB": "GBP", "IM": "GBP", "JE": "GBP", "GG": "GBP", "GS": "GBP",
	"CN": "CNY", "IN": "INR", "JP": "JPY", "CH": "CHF", "LI": "CHF", "CA": "CAD", "SG": "SGD", "HK": "HKD",
	"AE": "AED", "SA": "SAR", "KW": "KWD",
	"ZA": "ZAR", "NG": "NGN", "EG": "EGP", "KE": "KES", "GH": "GHS", "MA": "MAD", "EH": "MAD", "TZ": "TZS", "UG": "UGX",
}

func init() {
	for _, pair := range strings.Split(countryData, "|") {
		code, name, _ := strings.Cut(pair, ":")
		Countries = append(Countries, Country{Code: code, Name: name})
		countryByCode[code] = name
		countryByName[foldName(name)] = code
	}
	for k, v := range countryAliases {
		countryByName[foldName(k)] = v
	}
	for _, c := range euroCountries {
		countryCurrency[c] = "EUR"
	}
}

// foldName lower-cases, strips accents and punctuation, and collapses spaces.
func foldName(s string) string {
	var b strings.Builder
	space := false
	for _, r := range accents.Replace(strings.ToLower(strings.TrimSpace(s))) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		case r == '&':
			if b.Len() > 0 {
				b.WriteString(" and")
			}
			space = true
		case r == '\'' || r == '’' || r == '.':
			// "Côte d'Ivoire" → "cote divoire", "U.S.A." → "usa"
		default:
			space = true
		}
	}
	return b.String()
}

// IsCountry reports whether code is a known ISO 3166-1 alpha-2 code.
func IsCountry(code string) bool {
	_, ok := countryByCode[strings.ToUpper(strings.TrimSpace(code))]
	return ok
}

// CountryName returns the display name for a country code ("" if unknown).
func CountryName(code string) string {
	return countryByCode[strings.ToUpper(strings.TrimSpace(code))]
}

// CountryCode recognises a country from a code ("KE") or a free-text name
// ("Kenya", "UK", "United States of America"). Returns "" when unknown.
func CountryCode(nameOrCode string) string {
	s := strings.TrimSpace(nameOrCode)
	if s == "" {
		return ""
	}
	if len(s) == 2 {
		if up := strings.ToUpper(s); IsCountry(up) {
			return up
		}
	}
	if code, ok := countryByName[foldName(s)]; ok {
		return code
	}
	// Short aliases stored with punctuation ("u.s.a.") fold the same way.
	return ""
}

// CurrencyForCountry returns the supported currency used in a country, and
// false when the country's currency isn't supported (callers then fall back
// to the organisation's default currency).
func CurrencyForCountry(code string) (string, bool) {
	cur, ok := countryCurrency[strings.ToUpper(strings.TrimSpace(code))]
	return cur, ok
}

// ForCustomer resolves the currency to invoice a customer in: their own
// preferred currency, else their country's currency, else orgDefault.
// source is "customer", "country" or "default".
func ForCustomer(preferred, countryCode, countryName, orgDefault string) (cur, source string) {
	if p := strings.ToUpper(strings.TrimSpace(preferred)); p != "" && IsSupported(p) {
		return p, "customer"
	}
	code := strings.ToUpper(strings.TrimSpace(countryCode))
	if code == "" {
		code = CountryCode(countryName)
	}
	if c, ok := CurrencyForCountry(code); ok {
		return c, "country"
	}
	return strings.ToUpper(orgDefault), "default"
}
