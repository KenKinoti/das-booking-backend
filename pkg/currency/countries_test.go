package currency

import "testing"

func TestCountryCode(t *testing.T) {
	cases := map[string]string{
		"Australia": "AU", "australia ": "AU", "AU": "AU", "ke": "KE", "Kenya": "KE", "UK": "GB", "United Kingdom": "GB",
		"USA": "US", "U.S.A.": "US", "United States of America": "US", "Côte d'Ivoire": "CI", "Cote d'Ivoire": "CI",
		"Åland Islands": "AX", "Bosnia & Herzegovina": "BA", "Japan": "JP", "Narnia": "", "": "", "ZZ": "",
	}
	for in, want := range cases {
		if got := CountryCode(in); got != want {
			t.Errorf("CountryCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCurrencyForCountry(t *testing.T) {
	cases := map[string]string{
		"AU": "AUD", "NZ": "NZD", "US": "USD", "GB": "GBP", "DE": "EUR", "FR": "EUR", "IE": "EUR", "CN": "CNY", "IN": "INR",
		"JP": "JPY", "CH": "CHF", "CA": "CAD", "SG": "SGD", "HK": "HKD", "AE": "AED", "SA": "SAR", "KW": "KWD", "ZA": "ZAR",
		"NG": "NGN", "EG": "EGP", "ke": "KES", "GH": "GHS", "MA": "MAD", "TZ": "TZS", "UG": "UGX",
	}
	for code, want := range cases {
		got, ok := CurrencyForCountry(code)
		if !ok || got != want {
			t.Errorf("CurrencyForCountry(%q) = %q,%v want %q", code, got, ok, want)
		}
	}
	if _, ok := CurrencyForCountry("BR"); ok {
		t.Error("BRL is not supported; expected fallback")
	}
	// Every mapped currency must be supported and every supported currency reachable.
	reach := map[string]bool{}
	for c, cur := range countryCurrency {
		if !IsSupported(cur) {
			t.Errorf("%s maps to unsupported %s", c, cur)
		}
		if !IsCountry(c) {
			t.Errorf("%s is not a known country", c)
		}
		reach[cur] = true
	}
	for _, s := range Supported {
		if !reach[s] {
			t.Errorf("no country maps to %s", s)
		}
	}
	if len(Countries) < 240 {
		t.Errorf("expected full ISO list, got %d", len(Countries))
	}
}
