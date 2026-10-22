package sso

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"
)

// Status checklist for System settings → Sign-in providers: everything that
// decides whether the "Sign in with …" button shows on the sign-in page and
// whether signing in works, worked out on the server (the same Load and
// Usable the sign-in page uses), so the admin page can't disagree with it.

// Check is one line of the checklist.
type Check struct {
	ID     string     `json:"id"`
	State  string     `json:"state"` // ok | warn | error | info
	Label  string     `json:"label"`
	Detail string     `json:"detail"`
	Value  string     `json:"value,omitempty"` // e.g. the redirect URI to copy
	At     *time.Time `json:"at,omitempty"`
}

// Status is a provider's checklist.
type Status struct {
	ShowsOnLogin bool      `json:"shows_on_login"`
	Reason       string    `json:"reason"` // why it does / doesn't show
	Ready        bool      `json:"ready"`  // shows, and no check is red
	Errors       int       `json:"errors"`
	Warnings     int       `json:"warnings"`
	Checks       []Check   `json:"checks"`
	CheckedAt    time.Time `json:"checked_at"`
}

// checkTTL: how long a discovery check result is reused ("Check now" skips it).
const checkTTL = 5 * time.Minute

type discCheck struct {
	OK     bool
	Err    string
	Issuer string
	Keys   int
	Took   time.Duration
	At     time.Time
}

var guidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const googleIDSuffix = ".apps.googleusercontent.com"

// ClientIDProblem returns "" when the client ID looks right; otherwise a
// state (error = surely wrong, warn = unusual) and why.
func ClientIDProblem(provider, id string) (state, msg string) {
	switch provider {
	case Google:
		if strings.HasPrefix(id, "GOCSPX-") {
			return "error", "This is the client secret, not the client ID — the client ID ends with .apps.googleusercontent.com."
		}
		if !strings.HasSuffix(strings.ToLower(id), googleIDSuffix) {
			return "warn", "Google client IDs end with .apps.googleusercontent.com — copy the whole Client ID from Google Cloud → Credentials."
		}
	case Microsoft:
		if !guidRe.MatchString(id) {
			return "warn", "A Microsoft Application (client) ID looks like 1b2c3d4e-5f60-7182-93a4-b5c6d7e8f901 — copy it from the app registration's Overview."
		}
	}
	return "", ""
}

// SecretProblem is ClientIDProblem for the client secret.
func SecretProblem(provider, secret, id string) (state, msg string) {
	if secret == id {
		return "error", "The client secret is the same as the client ID."
	}
	switch provider {
	case Google:
		if strings.HasSuffix(strings.ToLower(secret), googleIDSuffix) {
			return "error", "This is the client ID, not the client secret."
		}
	case Microsoft:
		if guidRe.MatchString(secret) {
			return "error", "This looks like the secret's ID. Copy the secret's Value instead (Certificates & secrets → Value — shown only right after it's created)."
		}
	}
	return "", ""
}

func setUp(s Settings) bool {
	return s.Enabled || s.ClientID != "" || s.ClientSecret != "" || s.SecretUnreadable
}

// showsReason explains, in one sentence, whether the button shows.
func showsReason(s Settings) string {
	if s.Usable() {
		return "Yes — it's switched on and has a client ID and secret."
	}
	var missing []string
	if s.ClientID == "" {
		missing = append(missing, "client ID")
	}
	if s.ClientSecret == "" {
		if s.SecretUnreadable {
			missing = append(missing, "client secret (the saved one can't be decrypted)")
		} else {
			missing = append(missing, "client secret")
		}
	}
	need := strings.Join(missing, " and ")
	verb := "is"
	if len(missing) > 1 {
		verb = "are"
	}
	switch {
	case s.Enabled:
		return "No — it's switched on, but the " + need + " " + verb + " missing."
	case !s.Configured() && s.Switched == "off":
		return "No — it's switched off, and the " + need + " " + verb + " missing."
	case !s.Configured():
		return "No — add the " + need + "."
	case s.Switched == "off":
		return "No — an administrator switched it off. Turn it on to show the button."
	default:
		return "No — the credentials are saved, but it was never switched on. Turn it on to show the button."
	}
}

func consoleWhere(p string) string {
	if p == Microsoft {
		return "Microsoft Entra admin centre → App registrations → your app → Authentication → Web → Redirect URIs"
	}
	return "Google Cloud console → APIs & Services → Credentials → your OAuth client → Authorised redirect URIs"
}

func isLocalHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".localhost")
}

func originOf(v string) string {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// Status computes the checklist (refresh re-runs the network check).
func (h *Handler) Status(c *gin.Context, s Settings, refresh bool) Status {
	p, name := s.Provider, s.Name
	st := Status{CheckedAt: h.now(), ShowsOnLogin: s.Usable(), Reason: showsReason(s)}
	add := func(ch Check) { st.Checks = append(st.Checks, ch) }
	missing := "info" // a provider nobody set up is not an error
	if setUp(s) {
		missing = "error"
	}

	// 1. The switch.
	sw := Check{ID: "enabled", Label: "Switched on", At: s.SwitchedAt}
	switch {
	case s.Enabled && !s.Saved:
		sw.State, sw.Detail = "ok", "On — nothing is saved here, so it follows the server environment, which has a client ID and secret."
	case s.Enabled && s.Switched == "auto":
		sw.State, sw.Detail = "ok", "On — turned on automatically when the client ID and secret were saved."
	case s.Enabled && s.Switched == "on":
		sw.State, sw.Detail = "ok", "On — switched on by an administrator."
	case s.Enabled:
		sw.State, sw.Detail = "ok", "On."
	case s.Switched == "off":
		sw.State, sw.Detail = "error", "Off — switched off by an administrator. Saving credentials keeps it off; use Turn on."
	case s.Configured():
		sw.State, sw.Detail = "error", "Off — the credentials are saved, but it was never switched on."
	default:
		sw.State, sw.Detail = "info", "Off — not set up yet. Saving a client ID and secret turns it on."
	}
	add(sw)

	// 2. Client ID.
	idc := Check{ID: "client_id", Label: "Client ID", Value: s.ClientID}
	if s.ClientID == "" {
		idc.State, idc.Detail = missing, "Missing — paste the client ID from the "+name+" console."
	} else {
		from := "Saved in System settings."
		if s.ClientIDFrom == "env" {
			from = "From the server environment (" + s.ClientIDEnv + ")."
		}
		if state, msg := ClientIDProblem(p, s.ClientID); state != "" {
			idc.State, idc.Detail = state, from+" "+msg
		} else {
			idc.State, idc.Detail = "ok", from+" Looks like a "+name+" client ID."
		}
	}
	add(idc)

	// 3. Client secret.
	sc := Check{ID: "client_secret", Label: "Client secret"}
	switch {
	case s.ClientSecret == "" && s.SecretUnreadable:
		sc.State, sc.Detail = "error", "A secret is saved but can't be decrypted — the server's ENCRYPTION_KEY (or JWT_SECRET) changed since it was saved. Paste the client secret again."
	case s.ClientSecret == "":
		sc.State, sc.Detail = missing, "Missing — paste the client secret."
	default:
		sc.State, sc.Detail = "ok", "Saved encrypted in System settings."
		if s.ClientSecretFrom == "env" {
			sc.Detail = "From the server environment (" + s.ClientSecretEnv + ")."
			if s.SecretUnreadable {
				sc.State, sc.Detail = "warn", "The secret saved here can't be decrypted (the server's encryption key changed), so the environment's ("+s.ClientSecretEnv+") is used. Paste the secret again or remove it."
			}
		}
		if state, msg := SecretProblem(p, s.ClientSecret, s.ClientID); state != "" {
			sc.State, sc.Detail = state, sc.Detail+" "+msg
		} else if s.ClientID != "" && s.ClientIDFrom != s.ClientSecretFrom && sc.State == "ok" {
			where := map[string]string{"env": "the server environment", "settings": "System settings"}
			sc.State, sc.Detail = "warn", sc.Detail+" The client ID comes from "+where[s.ClientIDFrom]+" and the secret from "+where[s.ClientSecretFrom]+" — make sure they belong to the same OAuth client."
		}
	}
	add(sc)

	// 4. Redirect URI.
	ru := h.RedirectURI(c, p)
	rc := Check{ID: "redirect_uri", Label: "Redirect URI", Value: ru, State: "ok"}
	base, fixed := cleanBase(h.PublicAPI)
	if base == "" {
		rc.State = "warn"
		rc.Detail = "PUBLIC_API_URL isn't set on the server, so this URI is worked out from the address this page used to reach the server (" + requestBase(c) + "). It must be the backend address " + name + " calls back — the API server, not the web app. Set PUBLIC_API_URL to that address so the URI never changes, and add exactly this URI in " + consoleWhere(p) + "."
	} else {
		rc.Detail = "Built from PUBLIC_API_URL (" + base + "). Register exactly this URI in " + consoleWhere(p) + "."
		for _, f := range fixed {
			rc.State = "warn"
			if f == "path" {
				rc.Detail = "PUBLIC_API_URL should be just the server's address — the /api/v1 at its end is ignored. " + rc.Detail
			} else {
				rc.Detail = "PUBLIC_API_URL has no https:// — https is assumed. " + rc.Detail
			}
		}
		if !strings.EqualFold(hostOf(base), requestHost(c)) {
			rc.Detail += " (This page reached the server at " + requestHost(c) + "; sign-ins started there move to " + hostOf(base) + " first.)"
		}
	}
	if u, err := url.Parse(ru); err == nil && u.Scheme == "http" && !isLocalHost(u.Hostname()) {
		rc.State = "error"
		if base == "" {
			rc.Detail = name + " only accepts https:// redirect URIs (except localhost), and this server sees plain http. Set PUBLIC_API_URL to its https:// address."
		} else {
			rc.Detail = name + " only accepts https:// redirect URIs (except localhost). Change PUBLIC_API_URL to the https:// address."
		}
	}
	add(rc)

	// 5. Where the browser returns after signing in.
	appURL := strings.TrimRight(strings.TrimSpace(h.PublicApp), "/")
	origin := originOf(c.GetHeader("Origin"))
	ac := Check{ID: "app_url", Label: "Back to the app"}
	switch {
	case appURL == "" && origin != "" && origin != strings.ToLower(requestBase(c)):
		ac.State, ac.Detail = "error", "PUBLIC_APP_URL isn't set, so after signing in the browser is sent to this server ("+requestBase(c)+CompletePath+") instead of the app. Set PUBLIC_APP_URL="+origin+" on the server."
	case appURL == "":
		ac.State, ac.Detail = "warn", "PUBLIC_APP_URL isn't set — after signing in the browser returns to "+requestBase(c)+CompletePath+". Set it to the web app's address."
	case origin != "" && originOf(appURL) != origin:
		ac.State, ac.Detail = "warn", "After signing in people land on "+appURL+" (PUBLIC_APP_URL), but you're using the app at "+origin+". Sessions don't carry over between addresses — set PUBLIC_APP_URL to the address people use."
	default:
		ac.State, ac.Detail = "ok", "After signing in, people return to "+appURL+CompletePath+" (PUBLIC_APP_URL)."
	}
	add(ac)

	// 6. The provider, seen from the server.
	dc := Check{ID: "discovery", Label: name + " reachable from the server", Value: s.DiscoveryURL}
	if !setUp(s) {
		dc.State, dc.Detail = "info", "Not checked — set up the client ID and secret first."
	} else {
		d := h.discovery(s.DiscoveryURL, refresh)
		at := d.At
		dc.At = &at
		if d.OK {
			dc.State = "ok"
			dc.Detail = fmt.Sprintf("Fetched the discovery document and %d signing key(s) in %d ms (issuer %s).", d.Keys, d.Took.Milliseconds(), d.Issuer)
			if s.TestIssuer {
				dc.Detail += " Test issuer (SSO_TEST_ISSUERS=1)."
			}
		} else {
			dc.State = "error"
			dc.Detail = "Couldn't fetch it: " + d.Err + ". Signing in fails until the server can reach " + name + " (outbound network, firewall, DNS)."
		}
	}
	add(dc)

	// 7. The latest real sign-in attempt.
	var hl ProviderHealth
	if h.DB != nil && h.DB.Where("provider = ?", p).Take(&hl).Error == nil {
		switch {
		case hl.LastErrorAt != nil && (hl.LastSuccessAt == nil || hl.LastErrorAt.After(*hl.LastSuccessAt)):
			lc := Check{ID: "last_attempt", Label: "Last sign-in attempt", State: "warn", At: hl.LastErrorAt}
			if healthCritical[hl.LastErrorCode] && hl.LastErrorSource == "server" {
				lc.State = "error"
			}
			lc.Detail = "Failed: " + explainError(p, hl.LastErrorCode)
			if hl.LastErrorDetail != "" {
				lc.Detail += " " + name + " / the server said: " + hl.LastErrorDetail
			}
			if s.Saved && s.UpdatedAt.After(*hl.LastErrorAt) {
				lc.State = "info"
				lc.Detail = "Before the settings were last changed — " + lc.Detail + " Try signing in again to check the new settings."
			}
			add(lc)
		case hl.LastSuccessAt != nil:
			add(Check{ID: "last_attempt", Label: "Last sign-in attempt", State: "ok", At: hl.LastSuccessAt, Detail: "Went through — " + name + " sent a valid sign-in back."})
		}
	}

	for _, ch := range st.Checks {
		switch ch.State {
		case "error":
			st.Errors++
		case "warn":
			st.Warnings++
		}
	}
	st.Ready = st.ShowsOnLogin && st.Errors == 0
	return st
}

func hostOf(base string) string {
	if u, err := url.Parse(base); err == nil {
		return u.Host
	}
	return ""
}

// healthCritical: sign-in errors that mean the setup is wrong (not a person
// cancelling or using an unknown account) — red when this server saw them.
var healthCritical = map[string]bool{"invalid_client": true, "redirect_uri_mismatch": true, "provider_unavailable": true, "invalid_token": true}

// explainError says what an error code means for the administrator.
func explainError(p, code string) string {
	name := providerNames[p]
	switch code {
	case "invalid_client":
		return name + " rejected the client ID or secret (invalid_client) — copy a fresh client secret from the " + name + " console and save it."
	case "redirect_uri_mismatch":
		return "the redirect URI isn't registered for this client (redirect_uri_mismatch) — add the redirect URI above in " + consoleWhere(p) + "."
	case "provider_unavailable":
		return "the server couldn't reach " + name + "."
	case "invalid_token":
		return "the sign-in " + name + " sent back couldn't be verified."
	case "browser_mismatch":
		return "the browser cookie set when sign-in started didn't come back with the callback — usually sign-in started on another address than the redirect URI."
	case "cancelled":
		return "the person cancelled, or " + name + " denied access (access_denied). If the app is in Testing mode, add them as a test user or publish the consent screen."
	case "access_blocked":
		return name + " blocked the sign-in (the app is limited to one organisation, or an admin policy blocks it)."
	case "expired":
		return "the sign-in code had expired or was already used (invalid_grant)."
	default:
		return name + " returned an error (" + code + ")."
	}
}

// warmChecks runs the discovery checks of the set-up providers in parallel.
func (h *Handler) warmChecks(c *gin.Context, refresh bool, providers ...string) {
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, p := range providers {
		s := Load(h.DB, p)
		if !setUp(s) || seen[s.DiscoveryURL] {
			continue
		}
		seen[s.DiscoveryURL] = true
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			h.discovery(u, refresh)
		}(s.DiscoveryURL)
	}
	wg.Wait()
}

// discovery fetches the discovery document and its key set with a short
// timeout (cached for checkTTL unless refresh).
func (h *Handler) discovery(u string, refresh bool) discCheck {
	h.checkMu.Lock()
	if h.checks == nil {
		h.checks = map[string]discCheck{}
	}
	if r, ok := h.checks[u]; ok && !refresh && h.now().Sub(r.At) < checkTTL && !r.At.After(h.now()) {
		h.checkMu.Unlock()
		return r
	}
	h.checkMu.Unlock()
	timeout := h.CheckTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	o := h.OIDC
	if o == nil {
		o = NewOIDC()
	}
	r := discCheck{At: h.now()}
	t0 := time.Now()
	var d Discovery
	if err := o.getJSON(ctx, u, &d); err != nil {
		r.Err = netErrText(err, timeout)
	} else if d.Issuer == "" || d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		r.Err = "the discovery document is incomplete (issuer, endpoints or jwks_uri missing)"
	} else {
		r.Issuer = d.Issuer
		var set struct {
			Keys []jwk `json:"keys"`
		}
		if err := o.getJSON(ctx, d.JWKSURI, &set); err != nil {
			r.Err = "signing keys: " + netErrText(err, timeout)
		} else {
			for _, k := range set.Keys {
				if k.Kty == "RSA" {
					r.Keys++
				}
			}
			if r.Keys == 0 {
				r.Err = "no RSA signing keys at " + d.JWKSURI
			} else {
				r.OK = true
			}
		}
	}
	r.Took = time.Since(t0)
	h.checkMu.Lock()
	h.checks[u] = r
	h.checkMu.Unlock()
	return r
}

func netErrText(err error, timeout time.Duration) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Sprintf("no answer within %s", timeout)
	}
	return cut(err.Error(), 300)
}

// recordSuccess remembers that the provider sent a valid sign-in back.
func (h *Handler) recordSuccess(p string) {
	now := h.now()
	h.upsertHealth(ProviderHealth{Provider: p, LastSuccessAt: &now, UpdatedAt: now}, "last_success_at")
}

// recordFailure remembers a failed sign-in; server: seen by this server
// (certain) rather than reported through the browser (could be faked).
func (h *Handler) recordFailure(p, code, detail string, server bool) {
	now := h.now()
	src := "browser"
	if server {
		src = "server"
	}
	h.upsertHealth(ProviderHealth{Provider: p, LastErrorAt: &now, LastErrorCode: cut(code, 40), LastErrorDetail: cut(strings.TrimSpace(detail), 500),
		LastErrorSource: src, UpdatedAt: now}, "last_error_at", "last_error_code", "last_error_detail", "last_error_source")
}

// upsertHealth writes the given columns (best effort).
func (h *Handler) upsertHealth(row ProviderHealth, cols ...string) {
	if h.DB == nil {
		return
	}
	cols = append(cols, "updated_at")
	h.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "provider"}}, DoUpdates: clause.AssignmentColumns(cols)}).Create(&row)
}

// ProviderErrorCode maps an ?error= the provider sent to the callback (plus
// its error_description) to the code the sign-in error page explains.
func ProviderErrorCode(e, desc string) string {
	d := strings.ToLower(desc)
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "access_denied":
		return "cancelled"
	case "redirect_uri_mismatch":
		return "redirect_uri_mismatch"
	case "invalid_client", "unauthorized_client":
		return "invalid_client"
	case "org_internal", "admin_policy_enforced", "disallowed_useragent":
		return "access_blocked"
	}
	switch {
	case strings.Contains(d, "aadsts50011") || strings.Contains(d, "aadsts500113") || strings.Contains(d, "redirect_uri_mismatch"):
		return "redirect_uri_mismatch"
	case strings.Contains(d, "aadsts700016") || strings.Contains(d, "aadsts7000215"):
		return "invalid_client"
	}
	return "provider_error"
}

var detailRe = regexp.MustCompile(`[^a-zA-Z0-9_.\-]`)

// providerError answers a callback carrying ?error= (the provider refused,
// or the person cancelled). record: the callback belongs to a real sign-in
// attempt (state + browser cookie checked), so the status page shows it.
func (h *Handler) providerError(c *gin.Context, p, perr string, record bool) {
	desc := c.Query("error_description")
	code := ProviderErrorCode(perr, desc)
	if record {
		h.recordFailure(p, code, cut(strings.TrimSuffix(perr+": "+desc, ": "), 300), false)
	}
	extra := url.Values{}
	switch code {
	case "redirect_uri_mismatch":
		extra.Set("redirect_uri", h.RedirectURI(c, p))
	case "provider_error":
		extra.Set("detail", cut(detailRe.ReplaceAllString(perr, ""), 60))
	}
	h.toAppErrorV(c, code, p, "", extra)
}

// classifyVerifyError maps a failed code exchange / token check to the code
// the error page explains, plus the detail kept for the status page.
func classifyVerifyError(err error) (code, detail string) {
	detail = cut(err.Error(), 500)
	var te *TokenError
	if errors.As(err, &te) {
		d := strings.ToLower(te.Description)
		switch {
		case te.Code == "invalid_client" || te.Code == "unauthorized_client" || strings.Contains(d, "aadsts7000215") || strings.Contains(d, "aadsts700016"):
			return "invalid_client", detail
		case te.Code == "redirect_uri_mismatch" || strings.Contains(d, "redirect_uri") || strings.Contains(d, "redirect uri") || strings.Contains(d, "aadsts50011") || strings.Contains(d, "aadsts500113"):
			return "redirect_uri_mismatch", detail
		case te.Code == "invalid_grant":
			return "expired", detail
		}
		return "provider_error", detail
	}
	var ue *url.Error
	if errors.As(err, &ue) || strings.HasPrefix(err.Error(), "discovery:") || strings.HasPrefix(err.Error(), "jwks:") {
		return "provider_unavailable", detail
	}
	return "invalid_token", detail
}
