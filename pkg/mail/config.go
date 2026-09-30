// Package mail is the single outgoing-email implementation used by every
// module (invoices, quotes, reminders, meeting invitations, test emails, AI
// tools). It resolves which SMTP account to use for an organisation and
// sends properly formed MIME messages over STARTTLS / implicit TLS.
//
// Resolution order (see Resolve):
//  1. the organisation's own saved email settings (enabled, with a host),
//  2. the platform default: the settings saved by a super admin in
//     System settings → Email (stored on the platform owner's organisation),
//  3. the SMTP_* environment variables,
//  4. nothing — sending is not set up.
package mail

import (
	"crypto/tls"
	"net/mail"
	"os"
	"strconv"
	"strings"
)

// Sources reported by Resolve.
const (
	SourceOrganisation = "organisation"
	SourcePlatform     = "platform"
	SourceEnvironment  = "environment"
	SourceNone         = ""
)

// Security modes.
const (
	SecurityStartTLS = "starttls" // plain connect, then STARTTLS (port 587)
	SecuritySSL      = "ssl"      // implicit TLS from the first byte (port 465)
	SecurityNone     = "none"     // no encryption (local relays only)
	// SecurityAuto upgrades with STARTTLS when the server offers it (the
	// behaviour of the old SMTP_* environment configuration).
	SecurityAuto = "auto"
)

// Config is a fully resolved outgoing-mail account.
type Config struct {
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Username  string   `json:"username"`
	Password  string   `json:"-"` // decrypted; never serialised
	Security  string   `json:"security"`
	FromEmail string   `json:"from_email"`
	FromName  string   `json:"from_name"`
	ReplyTo   string   `json:"reply_to"`
	CC        []string `json:"always_cc"`
	BCC       []string `json:"always_bcc"`
	Notify    []string `json:"notification_recipients"`

	Source  string `json:"source"`            // organisation | platform | environment | ""
	OrgID   string `json:"-"`                 // organisation whose saved settings were used
	Note    string `json:"note,omitempty"`    // e.g. "saved settings are switched off"
	Problem string `json:"problem,omitempty"` // configuration problem that prevents sending

	// TLSConfig overrides the TLS settings (tests inject RootCAs here).
	TLSConfig *tls.Config `json:"-"`
}

// Ready reports whether messages can be sent with this configuration.
func (c Config) Ready() bool {
	return c.Host != "" && c.FromEmail != "" && c.Problem == ""
}

// From returns the RFC 5322 From header value ("Name <email>").
func (c Config) From() string {
	if c.FromEmail == "" {
		return ""
	}
	a := mail.Address{Name: c.FromName, Address: c.FromEmail}
	return a.String()
}

// Display returns "Name <email>" without MIME encoding, for the UI.
func (c Config) Display() string {
	if c.FromName == "" {
		return c.FromEmail
	}
	return c.FromName + " <" + c.FromEmail + ">"
}

// NormalizeSecurity maps stored values ("tls" is the legacy name the settings
// page used for STARTTLS) to one of the Security* constants.
func NormalizeSecurity(sec string, port int) string {
	s := strings.ToLower(strings.TrimSpace(sec))
	switch s {
	case "none", "plain", "off":
		return SecurityNone
	case "ssl", "smtps", "implicit":
		return SecuritySSL
	case "auto":
		if port == 465 {
			return SecuritySSL
		}
		return SecurityAuto
	}
	if port == 465 {
		return SecuritySSL
	}
	return SecurityStartTLS
}

// IsGmail reports whether host is Google's SMTP service.
func IsGmail(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return strings.HasSuffix(h, "gmail.com") || strings.HasSuffix(h, "googlemail.com") || strings.HasSuffix(h, "google.com")
}

// CleanPassword strips the spaces Google shows inside App Passwords
// ("abcd efgh ijkl mnop") when the host is Gmail.
func CleanPassword(host, pw string) string {
	if IsGmail(host) {
		return strings.Join(strings.Fields(pw), "")
	}
	return pw
}

// ParseList splits a comma/semicolon/space/newline separated list of
// addresses, lower-cases, validates and de-duplicates it. Invalid entries are
// returned separately.
func ParseList(raw ...string) (valid, invalid []string) {
	seen := map[string]bool{}
	valid, invalid = []string{}, []string{}
	for _, r := range raw {
		for _, part := range strings.FieldsFunc(r, func(c rune) bool {
			return c == ',' || c == ';' || c == ' ' || c == '\n' || c == '\r' || c == '\t'
		}) {
			e := strings.ToLower(strings.TrimSpace(part))
			if e == "" || seen[e] {
				continue
			}
			seen[e] = true
			if !ValidAddress(e) {
				invalid = append(invalid, e)
				continue
			}
			valid = append(valid, e)
		}
	}
	return valid, invalid
}

// JoinList stores a list as a comma-separated string.
func JoinList(list []string) string { return strings.Join(list, ", ") }

// ValidAddress reports whether s is a single bare address like a@b.co.
func ValidAddress(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 254 || strings.ContainsAny(s, " \r\n,;<>\"") {
		return false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || !strings.EqualFold(a.Address, s) {
		return false
	}
	at := strings.LastIndex(s, "@")
	return at > 0 && strings.Contains(s[at+1:], ".") && !strings.HasSuffix(s, ".")
}

// FromEnv reads the SMTP_* environment variables (the legacy configuration):
// SMTP_HOST, SMTP_PORT, SMTP_USERNAME, SMTP_PASSWORD, SMTP_FROM (may be
// "Name <email>"), SMTP_FROM_NAME, SMTP_SECURITY, SMTP_REPLY_TO.
func FromEnv() Config {
	host := strings.TrimSpace(os.Getenv("SMTP_HOST"))
	if host == "" {
		return Config{}
	}
	port, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("SMTP_PORT")))
	if port <= 0 {
		port = 587
	}
	user := strings.TrimSpace(os.Getenv("SMTP_USERNAME"))
	from := strings.TrimSpace(os.Getenv("SMTP_FROM"))
	if from == "" {
		from = user
	}
	fromEmail, fromName := from, strings.TrimSpace(os.Getenv("SMTP_FROM_NAME"))
	if a, err := mail.ParseAddress(from); err == nil {
		fromEmail = a.Address
		if fromName == "" {
			fromName = a.Name
		}
	}
	if !strings.Contains(fromEmail, "@") {
		fromEmail = ""
	}
	return Config{
		Host: host, Port: port, Username: user,
		Password:  CleanPassword(host, os.Getenv("SMTP_PASSWORD")),
		Security:  NormalizeSecurity(envOr("SMTP_SECURITY", "auto"), port),
		FromEmail: strings.ToLower(fromEmail), FromName: fromName,
		ReplyTo: strings.TrimSpace(os.Getenv("SMTP_REPLY_TO")),
		CC:      []string{}, BCC: []string{}, Notify: []string{},
		Source: SourceEnvironment,
	}
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
