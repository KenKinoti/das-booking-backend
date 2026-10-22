package sso

import (
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Provider ids.
const (
	Google    = "google"
	Microsoft = "microsoft"
)

// Providers in display order.
var Providers = []string{Google, Microsoft}

var providerNames = map[string]string{Google: "Google", Microsoft: "Microsoft"}

// microsoftConsumerTenant is the tenant id of personal Microsoft accounts
// (Outlook.com, Hotmail, Live, Xbox …).
const microsoftConsumerTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"

// Settings is the effective configuration of one provider.
type Settings struct {
	Provider     string
	Name         string
	Enabled      bool
	ClientID     string
	ClientSecret string
	Tenant       string // Microsoft: common | organizations | consumers | <tenant id>
	// DiscoveryURL and the issuer rule.
	DiscoveryURL string
	TestIssuer   bool // SSO_*_ISSUER override in effect (tests only)
	// Where each value came from (for the admin page): env | settings | "",
	// and which environment variable when env.
	ClientIDFrom     string
	ClientSecretFrom string
	ClientIDEnv      string
	ClientSecretEnv  string
	// Saved: a row exists in sso_provider_configs (its Enabled wins).
	Saved bool
	// Switched / SwitchedAt: the saved switch history (ProviderConfig);
	// UpdatedAt: when the saved settings last changed.
	Switched   string
	SwitchedAt *time.Time
	UpdatedAt  time.Time
	// SecretUnreadable: a secret is saved but can't be decrypted (the
	// server's ENCRYPTION_KEY / JWT_SECRET changed since it was saved).
	SecretUnreadable bool
}

// Configured: has the credentials needed to sign in.
func (s Settings) Configured() bool { return s.ClientID != "" && s.ClientSecret != "" }

// Usable: enabled and configured.
func (s Settings) Usable() bool { return s.Enabled && s.Configured() }

var tenantRe = regexp.MustCompile(`^[A-Za-z0-9.\-]{1,100}$`)

// ValidTenant accepts common/organizations/consumers, a GUID or a domain.
func ValidTenant(t string) bool { return tenantRe.MatchString(t) }

// CleanCredential removes what copy and paste adds to a client ID, secret or
// tenant: whitespace and line breaks anywhere (none of them ever contain
// any), zero-width characters and surrounding quotes.
func CleanCredential(v string) string {
	v = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r), r == '\u200b', r == '\u200c', r == '\u200d', r == '\u2060', r == '\ufeff':
			return -1
		}
		return r
	}, v)
	for len(v) >= 2 {
		f, l := v[0], v[len(v)-1]
		if f != l || (f != '"' && f != '\'' && f != '`') {
			break
		}
		v = v[1 : len(v)-1]
	}
	return v
}

// envFirst returns the first set variable: value, "env", its name.
func envFirst(keys ...string) (string, string, string) {
	for _, k := range keys {
		if v := CleanCredential(os.Getenv(k)); v != "" {
			return v, "env", k
		}
	}
	return "", "", ""
}

// testIssuer returns the SSO_<P>_ISSUER override — honoured only when
// SSO_TEST_ISSUERS=1 (never in a normal production deployment).
func testIssuer(provider string) string {
	if os.Getenv("SSO_TEST_ISSUERS") != "1" {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(os.Getenv("SSO_"+strings.ToUpper(provider)+"_ISSUER")), "/")
}

// Load returns the effective settings: values saved by the super admin win,
// the environment fills the gaps.
//
//	Google:    GOOGLE_OAUTH_CLIENT_ID / GOOGLE_OAUTH_CLIENT_SECRET, falling back to
//	           GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET (the Gmail expense client —
//	           one Google OAuth client can serve both if both redirect URIs are added).
//	Microsoft: MICROSOFT_CLIENT_ID / MICROSOFT_CLIENT_SECRET / MICROSOFT_TENANT (default common).
//
// Without saved settings a provider is enabled when its credentials exist.
func Load(db *gorm.DB, provider string) Settings {
	s := Settings{Provider: provider, Name: providerNames[provider]}
	var row ProviderConfig
	saved := db != nil && db.Where("provider = ?", provider).Take(&row).Error == nil
	switch provider {
	case Google:
		s.ClientID, s.ClientIDFrom, s.ClientIDEnv = envFirst("GOOGLE_OAUTH_CLIENT_ID", "GOOGLE_CLIENT_ID")
		s.ClientSecret, s.ClientSecretFrom, s.ClientSecretEnv = envFirst("GOOGLE_OAUTH_CLIENT_SECRET", "GOOGLE_CLIENT_SECRET")
	case Microsoft:
		s.ClientID, s.ClientIDFrom, s.ClientIDEnv = envFirst("MICROSOFT_CLIENT_ID")
		s.ClientSecret, s.ClientSecretFrom, s.ClientSecretEnv = envFirst("MICROSOFT_CLIENT_SECRET")
		s.Tenant, _, _ = envFirst("MICROSOFT_TENANT")
	default:
		return s
	}
	if saved {
		s.Saved, s.Switched, s.SwitchedAt, s.UpdatedAt = true, row.Switched, row.SwitchedAt, row.UpdatedAt
		if v := CleanCredential(row.ClientID); v != "" {
			s.ClientID, s.ClientIDFrom, s.ClientIDEnv = v, "settings", ""
		}
		if row.ClientSecret != "" {
			v, err := mail.DecryptSecret(row.ClientSecret)
			if v = CleanCredential(v); err == nil && v != "" {
				s.ClientSecret, s.ClientSecretFrom, s.ClientSecretEnv = v, "settings", ""
			} else {
				s.SecretUnreadable = true
			}
		}
		if v := CleanCredential(row.Tenant); v != "" {
			s.Tenant = v
		}
		s.Enabled = row.Enabled
	} else {
		s.Enabled = s.Configured()
	}
	if provider == Microsoft && !ValidTenant(s.Tenant) {
		s.Tenant = "common"
	}
	if iss := testIssuer(provider); iss != "" {
		s.TestIssuer = true
		s.DiscoveryURL = iss + "/.well-known/openid-configuration"
	} else if provider == Google {
		s.DiscoveryURL = "https://accounts.google.com/.well-known/openid-configuration"
	} else {
		s.DiscoveryURL = "https://login.microsoftonline.com/" + s.Tenant + "/v2.0/.well-known/openid-configuration"
	}
	return s
}

// issuerRule returns the issuer check for a provider given its discovery
// document issuer.
//   - Google: https://accounts.google.com (or the legacy accounts.google.com).
//   - Microsoft multi-tenant endpoints (common/organizations/consumers)
//     publish "https://login.microsoftonline.com/{tenantid}/v2.0": the token's
//     iss must equal it with {tenantid} replaced by the token's tid claim.
//   - Anything else (a single tenant, or a test issuer): exact match.
func issuerRule(provider, discIssuer string) func(string, *Claims) bool {
	return func(iss string, c *Claims) bool {
		if iss == "" {
			return false
		}
		if strings.Contains(discIssuer, "{tenantid}") {
			if c.TenantID == "" || !tenantRe.MatchString(c.TenantID) {
				return false
			}
			return iss == strings.ReplaceAll(discIssuer, "{tenantid}", c.TenantID)
		}
		if provider == Google && discIssuer == "https://accounts.google.com" {
			return iss == "https://accounts.google.com" || iss == "accounts.google.com"
		}
		return iss == discIssuer
	}
}

// VerifiedEmail returns the email address the provider vouches for, or ""
// when the token carries no verified address.
//
// Google: the "email" claim when "email_verified" is true.
//
// Microsoft (Entra ID) does not send email_verified, and the "email" claim of
// a work or school account is an editable attribute — it must not be trusted
// on its own. Accepted:
//   - personal Microsoft accounts (tid 9188040d-…): "email" — Microsoft
//     verifies these addresses;
//   - work/school accounts: "email" when the optional claim "xms_edov" (email
//     domain owner verified) is true — add it under Token configuration →
//     Optional claims in the app registration; otherwise
//     "preferred_username" when it is an email-shaped UPN (a UPN's domain must
//     be a domain the tenant has verified) and not a guest (#EXT#) UPN.
//   - an explicit email_verified=true is honoured too.
func VerifiedEmail(provider string, c *Claims) string {
	clean := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	isEmail := func(s string) bool {
		at := strings.LastIndex(s, "@")
		return at > 0 && at < len(s)-3 && strings.Contains(s[at:], ".") && !strings.ContainsAny(s, " \t<>\"#")
	}
	switch provider {
	case Google:
		if bool(c.EmailVerified) && isEmail(clean(c.Email)) {
			return clean(c.Email)
		}
	case Microsoft:
		e := clean(c.Email)
		if isEmail(e) && (bool(c.EmailVerified) || bool(c.EmailDomainOwner) || c.TenantID == microsoftConsumerTenant) {
			return e
		}
		if c.TenantID != microsoftConsumerTenant {
			if u := clean(c.PreferredUsername); isEmail(u) && !strings.Contains(strings.ToUpper(c.PreferredUsername), "#EXT#") {
				return u
			}
		}
	}
	return ""
}

// publicDomains can't be used for "anyone from this domain may join".
var publicDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true, "live.com": true, "msn.com": true,
	"yahoo.com": true, "ymail.com": true, "icloud.com": true, "me.com": true, "mac.com": true, "aol.com": true, "proton.me": true,
	"protonmail.com": true, "gmx.com": true, "mail.com": true, "zoho.com": true, "yandex.com": true, "hotmail.co.uk": true,
	"yahoo.co.uk": true, "outlook.co.uk": true, "live.co.uk": true, "bigpond.com": true, "yahoo.com.au": true, "hotmail.com.au": true,
	"outlook.com.au": true, "onmicrosoft.com": true,
}

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// NormDomain cleans a domain typed by an admin ("@Dasyin.com " → dasyin.com).
func NormDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, "@")
	d = strings.TrimSuffix(d, ".")
	return d
}

// DomainProblem explains why a domain can't be used ("" when it can).
func DomainProblem(d string) string {
	if !domainRe.MatchString(d) {
		return "Enter a domain like example.com"
	}
	if publicDomains[d] || strings.HasSuffix(d, ".onmicrosoft.com") {
		return d + " is a public email provider — anyone can get an address there"
	}
	return ""
}
