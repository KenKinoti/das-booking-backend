package football

// Helpers for provider API keys pasted by people: clean what copy-paste adds
// (quotes, a whole URL, "apiKey=", invisible characters from email or chat
// apps) and describe a saved key safely (last four characters and length
// only) so a refused key can be diagnosed without showing it.

import (
	"net/url"
	"strconv"
	"regexp"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

var (
	hex32Re   = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	keyParams = []string{"apiKey", "apikey", "api_key", "key", "token"}
)

// NormalizeKey cleans a pasted API key. It keeps printable ASCII only (keys
// never contain anything else, while copies from email and WhatsApp often
// carry zero-width spaces or non-breaking spaces), drops surrounding quotes
// and brackets, and pulls the key out of a pasted request URL, an
// "apiKey=…" pair or an "x-api-key: …" header line.
func NormalizeKey(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r > 32 && r < 127 {
			b.WriteRune(r)
		} else if r == ' ' || r == '\t' {
			b.WriteRune(' ')
		}
	}
	k := strings.TrimSpace(b.String())
	if strings.Contains(k, "://") || strings.Contains(k, "?") {
		if u, err := url.Parse(k); err == nil {
			q := u.Query()
			for _, p := range keyParams {
				if v := strings.TrimSpace(q.Get(p)); v != "" {
					k = v
					break
				}
			}
		}
	}
	low := strings.ToLower(k)
	for _, pre := range []string{"x-api-key:", "x-apisports-key:", "apikey=", "api_key=", "apikey:", "key="} {
		if strings.HasPrefix(low, pre) {
			k = strings.TrimSpace(k[len(pre):])
			low = strings.ToLower(k)
		}
	}
	if i := strings.IndexByte(k, '&'); i > 0 {
		k = k[:i]
	}
	k = strings.Trim(k, "\"'`<>()[]{}.,;: ")
	return k
}

// KeyHint describes a key without revealing it: "…1a2b (32 characters)".
func KeyHint(k string) string {
	if k == "" {
		return ""
	}
	tail := k
	if len(k) > 4 {
		tail = k[len(k)-4:]
	}
	return "…" + tail + " (" + strconv.Itoa(len(k)) + " characters)"
}

// KeyShapeHint warns when a key doesn't look like the provider's own keys
// (The Odds API and API-Football keys are 32 hexadecimal characters). It
// returns "" when the shape is fine or unknown.
func KeyShapeHint(provider, k string) string {
	if k == "" {
		return ""
	}
	switch provider {
	case ProviderOddsAPI:
		if !hex32Re.MatchString(k) {
			return "Keys from The Odds API (the-odds-api.com) are 32 characters, digits 0–9 and letters a–f. The saved key is " +
				strconv.Itoa(len(k)) + " characters, so it may be cut short or come from a different service — odds-api.io and OddsPapi are separate companies whose keys go in their own boxes."
		}
	case ProviderAPIFootball:
		if !hex32Re.MatchString(k) {
			return "API-Football keys from the api-sports.io dashboard are 32 characters, digits 0–9 and letters a–f. The saved key is " +
				strconv.Itoa(len(k)) + " characters — a RapidAPI key is different and doesn't work here."
		}
	}
	return ""
}

// savedKey returns the decrypted key for a provider and whether a stored
// value exists but could not be decrypted (for example after the server's
// encryption key changed).
func (s *Service) savedKey(provider string) (key string, unreadable bool) {
	st, err := s.Settings()
	if err != nil {
		return "", false
	}
	stored := st.APIFootballKey
	if provider == ProviderOddsAPI {
		stored = st.OddsAPIKey
	}
	if strings.TrimSpace(stored) == "" {
		return "", false
	}
	k, err := mail.DecryptSecret(stored)
	if err != nil {
		return "", true
	}
	return strings.TrimSpace(k), false
}
