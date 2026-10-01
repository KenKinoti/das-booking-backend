package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TokenResponse is Zoho's OAuth token endpoint reply.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	APIDomain    string `json:"api_domain"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

// APIError is a non-success reply from Zoho.
type APIError struct {
	Status  int
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Zoho returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("Zoho: %s (HTTP %d, code %d)", e.Message, e.Status, e.Code)
}

// RateLimitError means Zoho kept refusing calls (429) after retries. Daily is
// true when the daily allowance is used up (resume later).
type RateLimitError struct {
	Daily      bool
	RetryAfter time.Duration
	Message    string
}

func (e *RateLimitError) Error() string {
	if e.Daily {
		return "Zoho's daily API limit for this organisation is used up: " + e.Message
	}
	return "Zoho is rate limiting requests: " + e.Message
}

// AuthError means the connection no longer works (token revoked/expired,
// wrong client secret) — the user must reconnect.
type AuthError struct{ Message string }

func (e *AuthError) Error() string { return "Zoho connection needs to be reconnected: " + e.Message }

// authURL builds the consent screen URL.
func authURL(accounts, clientID, redirect, state string) string {
	q := url.Values{}
	q.Set("scope", strings.Join(Scopes, ","))
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirect)
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	q.Set("state", state)
	return strings.TrimRight(accounts, "/") + "/oauth/v2/auth?" + q.Encode()
}

func postToken(ctx context.Context, hc *http.Client, accounts string, form url.Values) (*TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(accounts, "/")+"/oauth/v2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach Zoho accounts: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("unexpected reply from Zoho accounts (HTTP %d)", res.StatusCode)
	}
	if tr.Error != "" || tr.AccessToken == "" {
		msg := tr.Error
		switch msg {
		case "invalid_client":
			msg = "invalid_client — check the Client ID and Client Secret, and that the app was created in the same Zoho data centre"
		case "invalid_code":
			msg = "invalid_code — the authorisation code expired or was already used (try Connect again); for a refresh this means the refresh token was revoked"
		case "":
			msg = fmt.Sprintf("no access token (HTTP %d)", res.StatusCode)
		}
		return nil, &AuthError{Message: msg}
	}
	return &tr, nil
}

// ExchangeCode swaps an authorisation code for tokens.
func ExchangeCode(ctx context.Context, hc *http.Client, accounts, clientID, secret, redirect, code string) (*TokenResponse, error) {
	f := url.Values{}
	f.Set("grant_type", "authorization_code")
	f.Set("client_id", clientID)
	f.Set("client_secret", secret)
	f.Set("redirect_uri", redirect)
	f.Set("code", code)
	return postToken(ctx, hc, accounts, f)
}

// Revoke revokes a refresh token (best effort on disconnect).
func Revoke(ctx context.Context, hc *http.Client, accounts, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(accounts, "/")+"/oauth/v2/token/revoke?token="+url.QueryEscape(token), nil)
	if err != nil {
		return err
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// Client calls the Zoho Books API for one organisation, refreshing the access
// token as needed, spacing calls out (RequestsPerMinute) and retrying 429s.
type Client struct {
	HTTP              *http.Client
	Accounts          string // https://accounts.zoho.com.au
	APIDomain         string // https://www.zohoapis.com.au
	ClientID          string
	ClientSecret      string
	RefreshToken      string
	OrgID             string
	RequestsPerMinute int
	MaxRetries        int
	// Sleep waits (tests replace it to run instantly).
	Sleep func(ctx context.Context, d time.Duration) error

	calls  int64
	mu     sync.Mutex
	access string
	expiry time.Time
	last   time.Time
}

// Calls is the number of API requests made so far.
func (c *Client) Calls() int { return int(atomic.LoadInt64(&c.calls)) }

func (c *Client) hc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 90 * time.Second}
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) token(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.access != "" && time.Now().Before(c.expiry.Add(-2*time.Minute)) {
		return c.access, nil
	}
	f := url.Values{}
	f.Set("grant_type", "refresh_token")
	f.Set("client_id", c.ClientID)
	f.Set("client_secret", c.ClientSecret)
	f.Set("refresh_token", c.RefreshToken)
	tr, err := postToken(ctx, c.hc(), c.Accounts, f)
	if err != nil {
		return "", err
	}
	c.access = tr.AccessToken
	exp := tr.ExpiresIn
	if exp <= 0 {
		exp = 3600
	}
	c.expiry = time.Now().Add(time.Duration(exp) * time.Second)
	if tr.APIDomain != "" && trustedAPI(tr.APIDomain) {
		c.APIDomain = strings.TrimRight(tr.APIDomain, "/")
	}
	return c.access, nil
}

// throttle spaces requests to stay under the per-minute limit.
func (c *Client) throttle(ctx context.Context) error {
	rpm := c.RequestsPerMinute
	if rpm <= 0 {
		rpm = 90
	}
	gap := time.Minute / time.Duration(rpm)
	c.mu.Lock()
	wait := time.Until(c.last.Add(gap))
	if wait < 0 {
		wait = 0
	}
	c.last = time.Now().Add(wait)
	c.mu.Unlock()
	return c.sleep(ctx, wait)
}

// do performs a GET on /books/v3<path> and returns the body.
func (c *Client) do(ctx context.Context, path string, q url.Values, accept string) ([]byte, string, error) {
	if q == nil {
		q = url.Values{}
	}
	if c.OrgID != "" && q.Get("organization_id") == "" {
		q.Set("organization_id", c.OrgID)
	}
	if accept != "" {
		q.Set("accept", accept)
	}
	retries := c.MaxRetries
	if retries <= 0 {
		retries = 4
	}
	refreshed := false
	backoff := 20 * time.Second
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		tok, err := c.token(ctx, false)
		if err != nil {
			return nil, "", err
		}
		if err := c.throttle(ctx); err != nil {
			return nil, "", err
		}
		u := strings.TrimRight(c.APIDomain, "/") + "/books/v3" + path + "?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Authorization", "Zoho-oauthtoken "+tok)
		atomic.AddInt64(&c.calls, 1)
		res, err := c.hc().Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			if attempt < retries {
				if err := c.sleep(ctx, 5*time.Second); err != nil {
					return nil, "", err
				}
				continue
			}
			return nil, "", fmt.Errorf("could not reach Zoho: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 50<<20))
		res.Body.Close()
		ct := res.Header.Get("Content-Type")
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if accept == "" {
				var env struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				}
				if json.Unmarshal(body, &env) == nil && env.Code != 0 {
					return nil, ct, &APIError{Status: res.StatusCode, Code: env.Code, Message: env.Message}
				}
			}
			return body, ct, nil
		}
		var env struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &env)
		switch {
		case res.StatusCode == http.StatusUnauthorized && !refreshed:
			refreshed = true
			if _, err := c.token(ctx, true); err != nil {
				return nil, "", err
			}
			continue
		case res.StatusCode == http.StatusUnauthorized:
			return nil, "", &AuthError{Message: env.Message}
		case res.StatusCode == http.StatusTooManyRequests:
			msg := env.Message
			daily := strings.Contains(strings.ToLower(msg), "day") || strings.Contains(strings.ToLower(msg), "daily")
			wait := backoff
			if ra, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && ra > 0 {
				wait = time.Duration(ra) * time.Second
			}
			if daily || attempt >= retries {
				return nil, "", &RateLimitError{Daily: daily, RetryAfter: wait, Message: msg}
			}
			if err := c.sleep(ctx, wait); err != nil {
				return nil, "", err
			}
			backoff *= 2
			continue
		case res.StatusCode >= 500 && attempt < retries:
			if err := c.sleep(ctx, 10*time.Second); err != nil {
				return nil, "", err
			}
			continue
		}
		return nil, "", &APIError{Status: res.StatusCode, Code: env.Code, Message: env.Message}
	}
}

// Get decodes the JSON reply of GET /books/v3<path> into out.
func (c *Client) Get(ctx context.Context, path string, q url.Values, out interface{}) error {
	body, _, err := c.do(ctx, path, q, "")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("unexpected reply from Zoho for %s: %v", path, err)
	}
	return nil
}

// PDF downloads a document as PDF (GET <path>?accept=pdf).
func (c *Client) PDF(ctx context.Context, path string) ([]byte, error) {
	body, ct, err := c.do(ctx, path, nil, "pdf")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(body), "%PDF") && !strings.Contains(ct, "pdf") {
		return nil, errors.New("Zoho did not return a PDF")
	}
	return body, nil
}

// PageContext is Zoho's pagination block.
type PageContext struct {
	Page        int  `json:"page"`
	PerPage     int  `json:"per_page"`
	HasMorePage bool `json:"has_more_page"`
}

// Page fetches one page (200 rows) of a list endpoint; key is the array name.
func (c *Client) Page(ctx context.Context, path, key string, page int, extra url.Values) ([]json.RawMessage, bool, error) {
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", "200")
	var raw map[string]json.RawMessage
	if err := c.Get(ctx, path, q, &raw); err != nil {
		return nil, false, err
	}
	var rows []json.RawMessage
	if b, ok := raw[key]; ok {
		_ = json.Unmarshal(b, &rows)
	}
	var pc PageContext
	if b, ok := raw["page_context"]; ok {
		_ = json.Unmarshal(b, &pc)
	}
	return rows, pc.HasMorePage, nil
}

// ZohoOrg is an organisation in Zoho Books.
type ZohoOrg struct {
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
	CurrencyCode   string `json:"currency_code"`
	Country        string `json:"country"`
	IsDefault      bool   `json:"is_default_org"`
}

// Organizations lists the Zoho Books organisations of the connected user.
func (c *Client) Organizations(ctx context.Context) ([]ZohoOrg, error) {
	saved := c.OrgID
	c.OrgID = ""
	defer func() { c.OrgID = saved }()
	var out struct {
		Organizations []ZohoOrg `json:"organizations"`
	}
	if err := c.Get(ctx, "/organizations", nil, &out); err != nil {
		return nil, err
	}
	return out.Organizations, nil
}
