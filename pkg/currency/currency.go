// Package currency lists the ISO 4217 currencies the platform supports.
// Keep in sync with das-booking-frontend/src/utils/currencies.js.
package currency

import "strings"

// Supported currency codes, grouped as in the UI.
var Supported = []string{
	// Major
	"USD", "EUR", "GBP", "JPY", "CHF", "CAD",
	// Asia-Pacific
	"AUD", "NZD", "CNY", "INR", "SGD", "HKD",
	// Middle East
	"AED", "SAR", "KWD",
	// Africa
	"ZAR", "NGN", "EGP", "KES", "GHS", "MAD", "TZS", "UGX",
}

// Decimals returns the number of minor units (JPY, UGX have none; KWD has 3).
func Decimals(code string) int {
	switch strings.ToUpper(code) {
	case "JPY", "UGX":
		return 0
	case "KWD":
		return 3
	}
	return 2
}

// IsSupported reports whether code is one of the supported currencies.
func IsSupported(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, c := range Supported {
		if c == code {
			return true
		}
	}
	return false
}
