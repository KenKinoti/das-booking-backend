// Package zoho brings an organisation's data in from Zoho Books — customers
// and their contact people, items, invoices, payments, credit notes, quotes
// (estimates), the original invoice PDFs and email history — either through
// a live OAuth connection or from Zoho's CSV/XLSX exports (plus generic CSV
// files from other tools). Both paths feed one import pipeline: preview →
// background job (throttled, resumable, cancellable) → report → undo.
package zoho

import (
	"net/url"
	"os"
	"strings"
)

// DataCenter is one Zoho region: its accounts (OAuth) server and API host.
type DataCenter struct {
	Code     string `json:"code"`
	Label    string `json:"label"`
	Accounts string `json:"accounts"`  // https://accounts.zoho.com.au
	API      string `json:"api"`       // https://www.zohoapis.com.au
	Console  string `json:"console"`   // https://api-console.zoho.com.au
	Books    string `json:"books_url"` // https://books.zoho.com.au
}

// DataCenters lists the supported regions (Australia first: the default).
var DataCenters = []DataCenter{
	{"com.au", "Australia (zoho.com.au)", "https://accounts.zoho.com.au", "https://www.zohoapis.com.au", "https://api-console.zoho.com.au", "https://books.zoho.com.au"},
	{"com", "United States (zoho.com)", "https://accounts.zoho.com", "https://www.zohoapis.com", "https://api-console.zoho.com", "https://books.zoho.com"},
	{"eu", "Europe (zoho.eu)", "https://accounts.zoho.eu", "https://www.zohoapis.eu", "https://api-console.zoho.eu", "https://books.zoho.eu"},
	{"uk", "United Kingdom (zoho.uk)", "https://accounts.zoho.uk", "https://www.zohoapis.uk", "https://api-console.zoho.uk", "https://books.zoho.uk"},
	{"in", "India (zoho.in)", "https://accounts.zoho.in", "https://www.zohoapis.in", "https://api-console.zoho.in", "https://books.zoho.in"},
	{"jp", "Japan (zoho.jp)", "https://accounts.zoho.jp", "https://www.zohoapis.jp", "https://api-console.zoho.jp", "https://books.zoho.jp"},
	{"ca", "Canada (zohocloud.ca)", "https://accounts.zohocloud.ca", "https://www.zohoapis.ca", "https://api-console.zohocloud.ca", "https://books.zohocloud.ca"},
	{"sa", "Saudi Arabia (zoho.sa)", "https://accounts.zoho.sa", "https://www.zohoapis.sa", "https://api-console.zoho.sa", "https://books.zoho.sa"},
}

// DefaultDC is used when none is chosen.
const DefaultDC = "com.au"

// Scopes are the read-only permissions requested (comma separated for Zoho).
// Items, taxes and currencies are covered by the settings scope.
var Scopes = []string{
	"ZohoBooks.contacts.READ",
	"ZohoBooks.settings.READ",
	"ZohoBooks.invoices.READ",
	"ZohoBooks.customerpayments.READ",
	"ZohoBooks.creditnotes.READ",
	"ZohoBooks.estimates.READ",
}

// FindDC returns the data centre with code (the default when unknown).
func FindDC(code string) DataCenter {
	code = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(code)), ".")
	for _, d := range DataCenters {
		if d.Code == code {
			return withOverrides(d)
		}
	}
	for _, d := range DataCenters {
		if d.Code == DefaultDC {
			return withOverrides(d)
		}
	}
	return DataCenters[0]
}

// withOverrides points every region at a fake server in tests
// (ZOHO_ACCOUNTS_URL / ZOHO_API_URL).
func withOverrides(d DataCenter) DataCenter {
	if v := strings.TrimRight(os.Getenv("ZOHO_ACCOUNTS_URL"), "/"); v != "" {
		d.Accounts = v
	}
	if v := strings.TrimRight(os.Getenv("ZOHO_API_URL"), "/"); v != "" {
		d.API = v
	}
	return d
}

// trustedAccounts reports whether an accounts server named in the OAuth
// callback ("accounts-server") is a real Zoho host (or the test override),
// so the client secret is never sent anywhere else.
func trustedAccounts(raw string) bool {
	if raw == "" {
		return false
	}
	if v := strings.TrimRight(os.Getenv("ZOHO_ACCOUNTS_URL"), "/"); v != "" && strings.TrimRight(raw, "/") == v {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	for _, d := range DataCenters {
		if strings.TrimRight(raw, "/") == d.Accounts {
			return true
		}
	}
	return false
}

// trustedAPI reports whether an api_domain returned by the token endpoint is
// a Zoho API host (or the test override).
func trustedAPI(raw string) bool {
	if v := strings.TrimRight(os.Getenv("ZOHO_API_URL"), "/"); v != "" && strings.TrimRight(raw, "/") == v {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return strings.HasPrefix(h, "www.zohoapis.") || strings.HasPrefix(h, "zohoapis.")
}
