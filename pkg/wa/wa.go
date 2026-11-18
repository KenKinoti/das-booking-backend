// Package wa sends WhatsApp messages through the WhatsApp Business Cloud API
// (Meta). It is the one place the ERP talks to WhatsApp for outbound
// messages: the morning brief, scheduled customer and team messages.
//
// Credentials live in the organisation's integration settings
// (System settings → Integrations → WhatsApp: access token, phone number ID,
// WhatsApp Business Account ID, API version). Resolve picks the
// organisation's own number; with allowPlatform it falls back to the
// platform's number (the super admins' organisation) — used for messages
// DASYIN itself sends to people (the morning brief), never for messages to an
// organisation's customers.
//
// Meta rules this package encodes:
//   - Messages the business starts must use an approved template; free text is
//     only allowed within 24 hours of the person's last message to the number
//     (error 131047 otherwise — Error.OutsideWindow).
//   - Template parameters may not contain new lines, tabs or more than four
//     consecutive spaces (error 132018); CleanParam makes text safe.
//   - Numbers are sent as digits with the country code and no "+".
//
// Tests point BaseURL at a fake (WHATSAPP_API_BASE, see
// /home/claude/tools/fake-meta-wa.mjs in the sandbox).
package wa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// BaseURL is the Graph API root (env WHATSAPP_API_BASE overrides, for tests).
func BaseURL() string {
	if b := strings.TrimRight(strings.TrimSpace(os.Getenv("WHATSAPP_API_BASE")), "/"); b != "" {
		return b
	}
	return "https://graph.facebook.com"
}

// DefaultAPIVersion is used when the settings leave the version empty.
const DefaultAPIVersion = "v21.0"

// Client is the HTTP client used for every call (short timeout).
var Client = &http.Client{Timeout: 20 * time.Second}

// ErrNotConfigured: neither the organisation nor (when allowed) the platform
// has WhatsApp switched on with a token and phone number ID.
var ErrNotConfigured = errors.New("WhatsApp is not set up: add the access token and phone number ID in System settings → Integrations → WhatsApp")

// Account is a usable WhatsApp sender.
type Account struct {
	OrgID             string // organisation whose settings these are
	Platform          bool   // true when the platform's number is used for another organisation
	PhoneNumberID     string
	BusinessAccountID string
	Token             string
	APIVersion        string
}

// Usable reports whether the settings can send.
func usable(s models.WhatsAppSettings) bool {
	return s.Enabled && strings.TrimSpace(s.AccessToken) != "" && strings.TrimSpace(s.PhoneNumberID) != ""
}

func fromSettings(org string, s models.WhatsAppSettings) *Account {
	tok := strings.TrimSpace(s.AccessToken)
	if mail.IsEncrypted(tok) {
		if p, err := mail.DecryptSecret(tok); err == nil {
			tok = p
		}
	}
	ver := strings.TrimSpace(s.APIVersion)
	if ver == "" {
		ver = DefaultAPIVersion
	}
	return &Account{OrgID: org, PhoneNumberID: strings.TrimSpace(s.PhoneNumberID), BusinessAccountID: strings.TrimSpace(s.BusinessAccountID), Token: tok, APIVersion: ver}
}

func load(db *gorm.DB, org string) (models.WhatsAppSettings, bool) {
	if db == nil || org == "" {
		return models.WhatsAppSettings{}, false
	}
	var row models.IntegrationSettings
	if err := db.Omit("Organization").Where("organization_id = ?", org).Limit(1).Find(&row).Error; err != nil || row.OrganizationID == "" {
		return models.WhatsAppSettings{}, false
	}
	return row.WhatsAppSettings, true
}

// Resolve returns the WhatsApp account to send from for org: its own number
// when set up, else (allowPlatform) the platform's number.
func Resolve(db *gorm.DB, org string, allowPlatform bool) (*Account, error) {
	if s, ok := load(db, org); ok && usable(s) {
		return fromSettings(org, s), nil
	}
	if allowPlatform {
		for _, p := range mail.Shared(db).PlatformOrgs() {
			if s, ok := load(db, p); ok && usable(s) {
				a := fromSettings(p, s)
				a.Platform = p != org
				return a, nil
			}
		}
	}
	return nil, ErrNotConfigured
}

// Status describes whether WhatsApp can be used for org (for settings pages).
type Status struct {
	Own      bool   `json:"own"`      // the organisation has its own number
	Platform bool   `json:"platform"` // the platform's number would be used (briefs only)
	Ready    bool   `json:"ready"`
	Note     string `json:"note"`
}

// StatusOf reports which number org would use.
func StatusOf(db *gorm.DB, org string, allowPlatform bool) Status {
	a, err := Resolve(db, org, allowPlatform)
	if err != nil {
		return Status{Note: ErrNotConfigured.Error()}
	}
	if a.Platform {
		return Status{Platform: true, Ready: true, Note: "Sent from DASYIN's WhatsApp number"}
	}
	return Status{Own: true, Ready: true, Note: "Sent from your WhatsApp Business number"}
}

// ---------------------------------------------------------------- numbers

var digitsOnly = regexp.MustCompile(`\D`)

// countryDial maps ISO country codes to dialling codes for the countries
// DASYIN serves; others need the number written with its country code.
var countryDial = map[string]string{"KE": "254", "AU": "61", "TZ": "255", "UG": "256", "RW": "250", "NZ": "64", "GB": "44", "US": "1", "ZA": "27", "NG": "234", "ET": "251"}

// NormalizeNumber turns a phone number as people type it ("0712 345 678",
// "+254712345678", "254-712-345678") into the digits WhatsApp wants
// ("254712345678"). A number starting with 0 gets defaultCountry's code.
func NormalizeNumber(raw, defaultCountry string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("no phone number")
	}
	plus := strings.HasPrefix(s, "+") || strings.HasPrefix(s, "00")
	d := digitsOnly.ReplaceAllString(s, "")
	if strings.HasPrefix(s, "00") {
		d = strings.TrimPrefix(d, "00")
	}
	if !plus {
		cc := countryDial[strings.ToUpper(strings.TrimSpace(defaultCountry))]
		switch {
		case strings.HasPrefix(d, "0") && cc != "":
			d = cc + strings.TrimLeft(d, "0")
		case cc != "" && strings.HasPrefix(d, cc):
			// already has the country code
		case cc != "" && len(d) <= 10:
			d = cc + d
		}
	}
	if len(d) < 8 || len(d) > 15 {
		return "", fmt.Errorf("%q is not a valid WhatsApp number (use the country code, e.g. +254 712 345 678)", raw)
	}
	return d, nil
}

// ---------------------------------------------------------------- templates

var multiSpace = regexp.MustCompile(` {4,}`)

// MaxParam is the longest template parameter text this package sends.
const MaxParam = 900

// CleanParam makes text safe as a template parameter: no new lines or tabs,
// at most three spaces in a row, trimmed and cut to MaxParam characters.
func CleanParam(s string) string {
	s = strings.NewReplacer("\r\n", " · ", "\n", " · ", "\r", " ", "\t", " ").Replace(s)
	s = multiSpace.ReplaceAllString(s, "   ")
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > MaxParam {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:MaxParam-1])) + "…"
	}
	if s == "" {
		s = "-"
	}
	return s
}

// Template is a template message.
type Template struct {
	Name       string   // approved template name, e.g. "dasyin_morning_brief"
	Language   string   // e.g. "en", "en_US", "sw"
	HeaderText []string // parameters of a TEXT header (usually none)
	BodyParams []string // {{1}}, {{2}}, … of the body (cleaned with CleanParam)
	// URLButton is the dynamic part of the first URL button (index 0), when
	// the template has a button like https://app.example/brief/{{1}}.
	URLButton string
}

// TemplateInfo describes a template from the business account.
type TemplateInfo struct {
	Name       string `json:"name"`
	Language   string `json:"language"`
	Status     string `json:"status"`   // APPROVED, PENDING, REJECTED, PAUSED, DISABLED
	Category   string `json:"category"` // UTILITY, MARKETING, AUTHENTICATION
	Body       string `json:"body"`     // body text with {{n}} placeholders
	BodyParams int    `json:"body_params"`
	URLButton  bool   `json:"url_button"` // has a dynamic URL button
}

// ---------------------------------------------------------------- errors

// Error is an error answered by the Cloud API.
type Error struct {
	HTTPStatus int
	Code       int
	Subcode    int
	Message    string
	Details    string
}

func (e *Error) Error() string {
	msg := e.Message
	if e.Details != "" && !strings.Contains(msg, e.Details) {
		msg += ": " + e.Details
	}
	return fmt.Sprintf("WhatsApp error %d: %s", e.Code, msg)
}

// OutsideWindow: free text can't be sent because the person hasn't messaged
// the number in the last 24 hours — use a template.
func (e *Error) OutsideWindow() bool { return e.Code == 131047 }

// Retryable: worth trying again later (rate limits, temporary errors).
func (e *Error) Retryable() bool {
	switch e.Code {
	case 4, 80007, 130429, 131000, 131016, 131048, 131056, 133004:
		return true
	}
	return e.HTTPStatus >= 500
}

// Friendly explains common errors in plain words.
func Friendly(err error) string {
	var e *Error
	if !errors.As(err, &e) {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	switch e.Code {
	case 131047:
		return "More than 24 hours since this person last messaged you — WhatsApp only allows an approved template now"
	case 132000, 132018:
		return "The template's parameters don't match the approved template (" + e.Message + ")"
	case 132001:
		return "That template doesn't exist in this language or isn't approved yet"
	case 131026:
		return "This number can't receive WhatsApp messages (not on WhatsApp, or hasn't accepted the latest terms)"
	case 190:
		return "The WhatsApp access token has expired or is invalid — create a permanent system-user token in Meta Business Manager"
	case 100:
		return "WhatsApp rejected the request: " + e.Message
	case 130429, 80007, 4:
		return "WhatsApp rate limit reached; it will be retried"
	}
	return e.Error()
}

// ---------------------------------------------------------------- calls

// Result of a sent message.
type Result struct {
	MessageID string `json:"message_id"` // wamid… — delivery statuses arrive on the webhook with this id
	WaID      string `json:"wa_id"`
}

func (a *Account) endpoint(parts ...string) string {
	esc := make([]string, len(parts))
	for i, p := range parts {
		esc[i] = url.PathEscape(p)
	}
	return BaseURL() + "/" + url.PathEscape(a.APIVersion) + "/" + strings.Join(esc, "/")
}

func (a *Account) do(ctx context.Context, method, u string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := Client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the WhatsApp Cloud API: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var eb struct {
			Error struct {
				Message      string `json:"message"`
				Code         int    `json:"code"`
				ErrorSubcode int    `json:"error_subcode"`
				ErrorData    struct {
					Details string `json:"details"`
				} `json:"error_data"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &eb)
		e := &Error{HTTPStatus: resp.StatusCode, Code: eb.Error.Code, Subcode: eb.Error.ErrorSubcode, Message: eb.Error.Message, Details: eb.Error.ErrorData.Details}
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
			if len(e.Message) > 300 {
				e.Message = e.Message[:300]
			}
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type sendResp struct {
	Contacts []struct {
		WaID string `json:"wa_id"`
	} `json:"contacts"`
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
}

func (a *Account) send(ctx context.Context, payload map[string]any) (*Result, error) {
	var r sendResp
	if err := a.do(ctx, http.MethodPost, a.endpoint(a.PhoneNumberID, "messages"), payload, &r); err != nil {
		return nil, err
	}
	res := &Result{}
	if len(r.Messages) > 0 {
		res.MessageID = r.Messages[0].ID
	}
	if len(r.Contacts) > 0 {
		res.WaID = r.Contacts[0].WaID
	}
	return res, nil
}

// SendText sends free text (only allowed inside the 24-hour window).
func (a *Account) SendText(ctx context.Context, to, body string, previewURL bool) (*Result, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("empty message")
	}
	if utf8.RuneCountInString(body) > 4096 {
		body = string([]rune(body)[:4095]) + "…"
	}
	return a.send(ctx, map[string]any{
		"messaging_product": "whatsapp", "recipient_type": "individual", "to": to, "type": "text",
		"text": map[string]any{"body": body, "preview_url": previewURL},
	})
}

// SendTemplate sends an approved template.
func (a *Account) SendTemplate(ctx context.Context, to string, t Template) (*Result, error) {
	if strings.TrimSpace(t.Name) == "" {
		return nil, errors.New("no WhatsApp template chosen")
	}
	lang := strings.TrimSpace(t.Language)
	if lang == "" {
		lang = "en"
	}
	var comps []map[string]any
	textParams := func(ps []string) []map[string]any {
		out := make([]map[string]any, 0, len(ps))
		for _, p := range ps {
			out = append(out, map[string]any{"type": "text", "text": CleanParam(p)})
		}
		return out
	}
	if len(t.HeaderText) > 0 {
		comps = append(comps, map[string]any{"type": "header", "parameters": textParams(t.HeaderText)})
	}
	if len(t.BodyParams) > 0 {
		comps = append(comps, map[string]any{"type": "body", "parameters": textParams(t.BodyParams)})
	}
	if t.URLButton != "" {
		comps = append(comps, map[string]any{"type": "button", "sub_type": "url", "index": "0",
			"parameters": []map[string]any{{"type": "text", "text": t.URLButton}}})
	}
	tpl := map[string]any{"name": t.Name, "language": map[string]any{"code": lang}}
	if len(comps) > 0 {
		tpl["components"] = comps
	}
	return a.send(ctx, map[string]any{"messaging_product": "whatsapp", "recipient_type": "individual", "to": to, "type": "template", "template": tpl})
}

var placeholder = regexp.MustCompile(`\{\{\s*\d+\s*\}\}`)

// Templates lists the business account's message templates (needs the
// WhatsApp Business Account ID in the settings).
func (a *Account) Templates(ctx context.Context) ([]TemplateInfo, error) {
	if a.BusinessAccountID == "" {
		return nil, errors.New("add the WhatsApp Business Account ID in System settings → Integrations → WhatsApp to list templates")
	}
	var out []TemplateInfo
	u := a.endpoint(a.BusinessAccountID, "message_templates") + "?fields=name,status,language,category,components&limit=100"
	for page := 0; page < 10 && u != ""; page++ {
		var r struct {
			Data []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Language   string `json:"language"`
				Category   string `json:"category"`
				Components []struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Buttons []struct {
						Type string `json:"type"`
						URL  string `json:"url"`
					} `json:"buttons"`
				} `json:"components"`
			} `json:"data"`
			Paging struct {
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := a.do(ctx, http.MethodGet, u, nil, &r); err != nil {
			return nil, err
		}
		for _, d := range r.Data {
			ti := TemplateInfo{Name: d.Name, Language: d.Language, Status: d.Status, Category: d.Category}
			for _, c := range d.Components {
				switch strings.ToUpper(c.Type) {
				case "BODY":
					ti.Body = c.Text
					ti.BodyParams = len(placeholder.FindAllString(c.Text, -1))
				case "BUTTONS":
					for _, b := range c.Buttons {
						if strings.EqualFold(b.Type, "URL") && placeholder.MatchString(b.URL) {
							ti.URLButton = true
						}
					}
				}
			}
			out = append(out, ti)
		}
		u = r.Paging.Next
		if u != "" && !strings.HasPrefix(u, BaseURL()) {
			u = "" // never follow a paging link to another host
		}
	}
	return out, nil
}

// Check verifies the token and phone number ID; it returns "<name> <number>".
func (a *Account) Check(ctx context.Context) (string, error) {
	var r struct {
		DisplayPhoneNumber string `json:"display_phone_number"`
		VerifiedName       string `json:"verified_name"`
	}
	if err := a.do(ctx, http.MethodGet, a.endpoint(a.PhoneNumberID)+"?fields=display_phone_number,verified_name", nil, &r); err != nil {
		return "", err
	}
	return strings.TrimSpace(r.VerifiedName + " " + r.DisplayPhoneNumber), nil
}
