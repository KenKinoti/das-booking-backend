package expenses

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Gmail (read-only) for billing emails. OAuth 2.0 web flow with the
// gmail.readonly scope; the refresh token is stored encrypted.
//
// Endpoints can be overridden for tests: GOOGLE_OAUTH_AUTH_URL,
// GOOGLE_OAUTH_TOKEN_URL, GMAIL_API_BASE.

// GmailScope is the only scope requested.
const GmailScope = "https://www.googleapis.com/auth/gmail.readonly"

func envOr(k, def string) string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv(k)), "/"); v != "" {
		return v
	}
	return def
}

func googleAuthURL() string {
	return envOr("GOOGLE_OAUTH_AUTH_URL", "https://accounts.google.com/o/oauth2/v2/auth")
}
func googleTokenURL() string {
	return envOr("GOOGLE_OAUTH_TOKEN_URL", "https://oauth2.googleapis.com/token")
}
func gmailBase() string { return envOr("GMAIL_API_BASE", "https://gmail.googleapis.com") }

// GoogleClientFromEnv returns the platform OAuth client (GOOGLE_CLIENT_ID /
// GOOGLE_CLIENT_SECRET), used when an organisation has not entered its own.
func GoogleClientFromEnv() (string, string) {
	return strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID")), strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET"))
}

// AuthCodeURL builds the consent URL.
func AuthCodeURL(clientID, redirect, state, loginHint string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("scope", GmailScope)
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	q.Set("include_granted_scopes", "true")
	q.Set("state", state)
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	return googleAuthURL() + "?" + q.Encode()
}

// Token is an OAuth token response.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func postToken(ctx context.Context, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach Google: %w", err)
	}
	defer resp.Body.Close()
	var t Token
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(raw, &t)
	if resp.StatusCode != http.StatusOK || t.AccessToken == "" {
		msg := t.ErrorDesc
		if msg == "" {
			msg = t.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		if t.Error == "invalid_grant" {
			msg = "Google access was revoked or expired — reconnect Gmail (" + msg + ")"
		}
		return nil, errors.New("Google sign-in failed: " + msg)
	}
	return &t, nil
}

// ExchangeCode swaps an authorisation code for tokens.
func ExchangeCode(ctx context.Context, clientID, secret, redirect, code string) (*Token, error) {
	return postToken(ctx, url.Values{"code": {code}, "client_id": {clientID}, "client_secret": {secret}, "redirect_uri": {redirect}, "grant_type": {"authorization_code"}})
}

// Refresh gets a new access token.
func Refresh(ctx context.Context, clientID, secret, refresh string) (*Token, error) {
	return postToken(ctx, url.Values{"refresh_token": {refresh}, "client_id": {clientID}, "client_secret": {secret}, "grant_type": {"refresh_token"}})
}

// Gmail is an authorised API client.
type Gmail struct {
	Token string
	HTTP  *http.Client
}

func (g *Gmail) get(ctx context.Context, path string, q url.Values, out any) error {
	u := gmailBase() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 45 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach Gmail: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 40<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			msg += " — reconnect Gmail and allow read-only access"
		}
		return errors.New("Gmail: " + msg)
	}
	return json.Unmarshal(raw, out)
}

// Profile returns the mailbox address.
func (g *Gmail) Profile(ctx context.Context) (string, error) {
	var p struct {
		EmailAddress string `json:"emailAddress"`
	}
	err := g.get(ctx, "/gmail/v1/users/me/profile", nil, &p)
	return p.EmailAddress, err
}

// List returns message ids matching q (newest first), up to max.
func (g *Gmail) List(ctx context.Context, q string, max int) ([]string, error) {
	ids := []string{}
	token := ""
	for len(ids) < max {
		v := url.Values{"q": {q}, "maxResults": {"100"}}
		if token != "" {
			v.Set("pageToken", token)
		}
		var r struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := g.get(ctx, "/gmail/v1/users/me/messages", v, &r); err != nil {
			return ids, err
		}
		for _, m := range r.Messages {
			ids = append(ids, m.ID)
		}
		if r.NextPageToken == "" || len(r.Messages) == 0 {
			break
		}
		token = r.NextPageToken
	}
	if len(ids) > max {
		ids = ids[:max]
	}
	return ids, nil
}

// MessagePart is a MIME part from messages.get (format=full).
type MessagePart struct {
	PartID   string `json:"partId"`
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Body struct {
		AttachmentID string `json:"attachmentId"`
		Size         int    `json:"size"`
		Data         string `json:"data"`
	} `json:"body"`
	Parts []MessagePart `json:"parts"`
}

// Message is a fetched email.
type Message struct {
	ID           string      `json:"id"`
	ThreadID     string      `json:"threadId"`
	InternalDate string      `json:"internalDate"`
	Snippet      string      `json:"snippet"`
	Payload      MessagePart `json:"payload"`
}

// Header returns a header value.
func (m *Message) Header(name string) string {
	for _, h := range m.Payload.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// Time is when Gmail received it.
func (m *Message) Time() *time.Time {
	var ms int64
	if _, err := fmt.Sscan(m.InternalDate, &ms); err == nil && ms > 0 {
		t := time.UnixMilli(ms).UTC()
		return &t
	}
	return nil
}

// Get fetches a message (format=full).
func (g *Gmail) Get(ctx context.Context, id string) (*Message, error) {
	var m Message
	err := g.get(ctx, "/gmail/v1/users/me/messages/"+url.PathEscape(id), url.Values{"format": {"full"}}, &m)
	return &m, err
}

// Attachment downloads an attachment's bytes.
func (g *Gmail) Attachment(ctx context.Context, msgID, attID string) ([]byte, error) {
	var a struct {
		Data string `json:"data"`
	}
	if err := g.get(ctx, "/gmail/v1/users/me/messages/"+url.PathEscape(msgID)+"/attachments/"+url.PathEscape(attID), nil, &a); err != nil {
		return nil, err
	}
	return decodeB64URL(a.Data)
}

func decodeB64URL(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}

// Bodies walks the MIME tree: plain text, HTML, and attachments.
func (m *Message) Bodies() (plain, htmlBody string, atts []MessagePart) {
	var walk func(p MessagePart)
	walk = func(p MessagePart) {
		mt := strings.ToLower(p.MimeType)
		switch {
		case p.Filename != "" && (p.Body.AttachmentID != "" || p.Body.Data != ""):
			atts = append(atts, p)
		case mt == "text/plain" && p.Body.Data != "":
			if b, err := decodeB64URL(p.Body.Data); err == nil && plain == "" {
				plain = string(b)
			}
		case mt == "text/html" && p.Body.Data != "":
			if b, err := decodeB64URL(p.Body.Data); err == nil && htmlBody == "" {
				htmlBody = string(b)
			}
		}
		for _, c := range p.Parts {
			walk(c)
		}
	}
	walk(m.Payload)
	return
}

// ---------- search presets ----------

// Preset is a saved Gmail search for one vendor's billing emails.
type Preset struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	VendorKey string `json:"vendor_key"`
	Query     string `json:"query"`
}

// Presets are the built-in searches. Verified against real billing emails
// (2026): Google Workspace sends from payments-noreply@google.com with
// subjects "Google Workspace: Your invoice is available for <domain>" (PDF
// attached, amount only in the PDF), "Google Workspace: We've received your
// payment for <domain>" and "Google Workspace: Payment was declined for
// <domain>". Synergy Wholesale sends from noreply@synergywholesale.com:
// "Payment receipt for invoice <n>" (invoice PDF attached), "Your monthly
// transaction statement for <Month>" (CSV attached) and low-balance notices.
var Presets = []Preset{
	{Key: "google_workspace", Label: "Google Workspace invoices & receipts", VendorKey: "google_workspace",
		Query: `from:payments-noreply@google.com subject:("Google Workspace")`},
	{Key: "synergy", Label: "Synergy Wholesale receipts, invoices & statements", VendorKey: "synergy",
		Query: `from:(synergywholesale.com) (subject:invoice OR subject:receipt OR subject:statement OR subject:"low balance")`},
}

// SenderQuery builds a query for user-defined senders ("billing@x.com", "@vendor.com").
func SenderQuery(senders []string) string {
	parts := []string{}
	for _, s := range senders {
		s = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(s), "@"))
		if s == "" || strings.ContainsAny(s, " ()\"") {
			continue
		}
		parts = append(parts, "from:"+s)
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, " OR ") + `) (invoice OR receipt OR payment OR bill OR renewal)`
}
