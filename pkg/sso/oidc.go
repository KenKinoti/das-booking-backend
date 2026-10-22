package sso

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file is a small OpenID Connect relying party built on the standard
// library: discovery documents and JSON Web Key Sets are fetched and cached,
// and ID tokens (RS256 only) are verified — signature, issuer, audience,
// expiry and nonce.

// Discovery is the subset of /.well-known/openid-configuration we use.
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// jwk is one RSA public key of a JWKS.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksCache struct {
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

type discCache struct {
	doc     *Discovery
	fetched time.Time
}

// OIDC fetches and caches discovery documents and key sets.
type OIDC struct {
	HTTP *http.Client
	Now  func() time.Time
	// DiscoveryTTL / JWKSTTL: how long documents are reused (default 1h / 6h).
	DiscoveryTTL time.Duration
	JWKSTTL      time.Duration
	// MinRefetch limits key-set refetches when a token names an unknown key
	// (key rotation) — default 30s.
	MinRefetch time.Duration

	mu   sync.Mutex
	disc map[string]*discCache
	jwks map[string]*jwksCache
}

// NewOIDC returns a client with sensible defaults.
func NewOIDC() *OIDC {
	return &OIDC{HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (o *OIDC) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *OIDC) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (o *OIDC) getJSON(ctx context.Context, u string, v interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", u, res.StatusCode)
	}
	return json.Unmarshal(body, v)
}

// Discover returns the provider's discovery document (cached).
func (o *OIDC) Discover(ctx context.Context, discoveryURL string) (*Discovery, error) {
	ttl := o.DiscoveryTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	o.mu.Lock()
	if o.disc == nil {
		o.disc = map[string]*discCache{}
	}
	if c := o.disc[discoveryURL]; c != nil && o.now().Sub(c.fetched) < ttl {
		o.mu.Unlock()
		return c.doc, nil
	}
	o.mu.Unlock()
	var d Discovery
	if err := o.getJSON(ctx, discoveryURL, &d); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if d.Issuer == "" || d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return nil, errors.New("discovery: incomplete document")
	}
	o.mu.Lock()
	o.disc[discoveryURL] = &discCache{doc: &d, fetched: o.now()}
	o.mu.Unlock()
	return &d, nil
}

func parseRSAKey(k jwk) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.N, "="))
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.E, "="))
	if err != nil {
		return nil, err
	}
	if len(nb) < 256 { // at least 2048-bit
		return nil, errors.New("RSA key too small")
	}
	e := 0
	for _, b := range eb {
		e = e<<8 | int(b)
	}
	if e < 3 {
		return nil, errors.New("bad RSA exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}, nil
}

// keys returns the key set of jwksURI; force refetches it (unless it was
// fetched less than MinRefetch ago).
func (o *OIDC) keys(ctx context.Context, jwksURI string, force bool) (map[string]*rsa.PublicKey, error) {
	ttl := o.JWKSTTL
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	minRe := o.MinRefetch
	if minRe <= 0 {
		minRe = 30 * time.Second
	}
	o.mu.Lock()
	if o.jwks == nil {
		o.jwks = map[string]*jwksCache{}
	}
	c := o.jwks[jwksURI]
	if c != nil {
		age := o.now().Sub(c.fetched)
		if (!force && age < ttl) || (force && age < minRe) {
			o.mu.Unlock()
			return c.keys, nil
		}
	}
	o.mu.Unlock()
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := o.getJSON(ctx, jwksURI, &set); err != nil {
		if c != nil {
			return c.keys, nil // keep using the last good set
		}
		return nil, fmt.Errorf("jwks: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		pk, err := parseRSAKey(k)
		if err != nil {
			continue
		}
		keys[k.Kid] = pk
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks: no usable RSA signing keys")
	}
	o.mu.Lock()
	o.jwks[jwksURI] = &jwksCache{keys: keys, fetched: o.now()}
	o.mu.Unlock()
	return keys, nil
}

// Claims are the ID token claims we read.
type Claims struct {
	Issuer            string      `json:"iss"`
	Subject           string      `json:"sub"`
	Audience          audience    `json:"aud"`
	AuthorizedParty   string      `json:"azp"`
	Expiry            numericDate `json:"exp"`
	IssuedAt          numericDate `json:"iat"`
	NotBefore         numericDate `json:"nbf"`
	Nonce             string      `json:"nonce"`
	Email             string      `json:"email"`
	EmailVerified     flexBool    `json:"email_verified"`
	PreferredUsername string      `json:"preferred_username"`
	Name              string      `json:"name"`
	GivenName         string      `json:"given_name"`
	FamilyName        string      `json:"family_name"`
	TenantID          string      `json:"tid"`
	ObjectID          string      `json:"oid"`
	EmailDomainOwner  flexBool    `json:"xms_edov"` // Microsoft: email domain owner verified
	HostedDomain      string      `json:"hd"`       // Google Workspace domain
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = audience{s}
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	*a = l
	return nil
}

func (a audience) has(v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}

type numericDate int64

func (n *numericDate) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		var s string
		if err2 := json.Unmarshal(b, &s); err2 != nil {
			return err
		}
		if _, err3 := fmt.Sscanf(s, "%f", &f); err3 != nil {
			return err
		}
	}
	*n = numericDate(int64(f))
	return nil
}

// flexBool accepts true, "true", 1 and "1" (Microsoft sends some booleans as strings).
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.ToLower(strings.TrimSpace(string(b))), `"`)
	*f = flexBool(s == "true" || s == "1")
	return nil
}

// Verification errors (tests match on them).
var (
	ErrMalformed = errors.New("malformed ID token")
	ErrAlg       = errors.New("unsupported ID token algorithm")
	ErrSignature = errors.New("ID token signature is invalid")
	ErrIssuer    = errors.New("ID token issuer is wrong")
	ErrAudience  = errors.New("ID token audience is wrong")
	ErrExpired   = errors.New("ID token has expired")
	ErrNotYet    = errors.New("ID token is not valid yet")
	ErrNonce     = errors.New("ID token nonce does not match")
)

// VerifyOptions are what an ID token must match.
type VerifyOptions struct {
	JWKSURI  string
	ClientID string
	Nonce    string
	// IssuerOK reports whether iss is acceptable (given the other claims).
	IssuerOK func(iss string, c *Claims) bool
	// Leeway for clock skew (default 2 minutes).
	Leeway time.Duration
}

// Verify checks an ID token and returns its claims.
func (o *OIDC) Verify(ctx context.Context, raw string, opt VerifyOptions) (*Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hb, &hdr) != nil {
		return nil, ErrMalformed
	}
	if hdr.Alg != "RS256" {
		return nil, ErrAlg
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}
	keys, err := o.keys(ctx, opt.JWKSURI, false)
	if err != nil {
		return nil, err
	}
	pick := func(keys map[string]*rsa.PublicKey) *rsa.PublicKey {
		if k := keys[hdr.Kid]; k != nil {
			return k
		}
		if hdr.Kid == "" && len(keys) == 1 {
			for _, k := range keys {
				return k
			}
		}
		return nil
	}
	key := pick(keys)
	if key == nil { // keys rotated: refetch once
		if keys, err = o.keys(ctx, opt.JWKSURI, true); err != nil {
			return nil, err
		}
		if key = pick(keys); key == nil {
			return nil, ErrSignature
		}
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return nil, ErrSignature
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return nil, ErrMalformed
	}
	if opt.IssuerOK == nil || !opt.IssuerOK(c.Issuer, &c) {
		return nil, ErrIssuer
	}
	if opt.ClientID == "" || !c.Audience.has(opt.ClientID) {
		return nil, ErrAudience
	}
	if len(c.Audience) > 1 && c.AuthorizedParty != opt.ClientID {
		return nil, ErrAudience
	}
	leeway := opt.Leeway
	if leeway <= 0 {
		leeway = 2 * time.Minute
	}
	now := o.now()
	if c.Expiry == 0 || now.After(time.Unix(int64(c.Expiry), 0).Add(leeway)) {
		return nil, ErrExpired
	}
	if c.NotBefore != 0 && now.Add(leeway).Before(time.Unix(int64(c.NotBefore), 0)) {
		return nil, ErrNotYet
	}
	if c.IssuedAt != 0 && now.Add(leeway).Before(time.Unix(int64(c.IssuedAt), 0)) {
		return nil, ErrNotYet
	}
	if opt.Nonce == "" || c.Nonce != opt.Nonce {
		return nil, ErrNonce
	}
	if c.Subject == "" {
		return nil, ErrMalformed
	}
	return &c, nil
}

// TokenResponse is the token endpoint's answer.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// TokenError is an error answer of the token endpoint (RFC 6749 §5.2), e.g.
// invalid_client when the client secret is wrong.
type TokenError struct {
	Status      int
	Code        string
	Description string
}

func (e *TokenError) Error() string {
	msg := e.Code
	if e.Description != "" {
		msg += ": " + e.Description
	}
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", e.Status)
	}
	return "token endpoint: " + msg
}

// Exchange swaps an authorization code (with its PKCE verifier) for tokens.
func (o *OIDC) Exchange(ctx context.Context, tokenURL, clientID, clientSecret, code, redirectURI, verifier string) (*TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var tr TokenResponse
	_ = json.Unmarshal(body, &tr)
	if res.StatusCode != http.StatusOK || tr.Error != "" {
		return nil, &TokenError{Status: res.StatusCode, Code: tr.Error, Description: tr.ErrorDesc}
	}
	if tr.IDToken == "" {
		return nil, errors.New("token endpoint: no id_token in the response")
	}
	return &tr, nil
}

// PKCEChallenge is the S256 code challenge of verifier.
func PKCEChallenge(verifier string) string {
	s := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(s[:])
}
