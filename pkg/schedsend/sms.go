package schedsend

// SMS through Twilio or Africa's Talking.
//
// Twilio (existing settings: System settings → Integrations → SMS (Twilio):
// account SID, auth token, From number or messaging service SID):
//   POST https://api.twilio.com/2010-04-01/Accounts/{AccountSid}/Messages.json
//   form To, Body, From | MessagingServiceSid, StatusCallback; HTTP basic auth
//   SID:token. 201 → {"sid":"SM…","status":"queued"}; errors
//   {"code":21211,"message":…,"status":400}. Status callbacks POST MessageSid,
//   MessageStatus (queued|sent|delivered|undelivered|failed), ErrorCode, signed
//   with X-Twilio-Signature (HMAC-SHA1 of the URL + sorted form fields).
//   Docs: https://www.twilio.com/docs/messaging/api/message-resource#create-a-message-resource
//         https://www.twilio.com/docs/usage/webhooks/webhooks-security
//
// Africa's Talking (System settings → Integrations → SMS: username, API key,
// sender ID), as their official SDK sends it (africastalking-python SMS.send):
//   POST https://api.africastalking.com/version1/messaging
//   (username "sandbox" → https://api.sandbox.africastalking.com/version1/messaging)
//   headers apiKey: <key>, Accept: application/json; form
//   username, to (+E.164, comma separated), message, from (sender ID / short
//   code, optional), bulkSMSMode=1.
//   201 → {"SMSMessageData":{"Message":"Sent to 1/1 Total Cost: KES 0.8000",
//   "Recipients":[{"statusCode":101,"number":"+2547…","status":"Success",
//   "cost":"KES 0.8000","messageId":"ATXid_…"}]}}. Recipient statusCode 100
//   Processed, 101 Sent, 102 Queued; 401 RiskHold, 402 InvalidSenderId,
//   403 InvalidPhoneNumber, 404 UnsupportedNumberType, 405 InsufficientBalance,
//   406 UserInBlacklist, 407 CouldNotRoute, 409 DoNotDisturbRejection,
//   500 InternalServerError, 501 GatewayError, 502 RejectedByGateway.
//   Delivery reports POST (form) id, status (Success|Failed|Rejected|Buffered|
//   Sent|Submitted), phoneNumber, networkCode, failureReason, retryCount to the
//   callback URL set in the Africa's Talking dashboard (SMS → Callback URLs →
//   Delivery reports).
//   Docs: https://developers.africastalking.com/docs/sms/sending/bulk
//         https://developers.africastalking.com/docs/sms/notifications
//         https://github.com/AfricasTalkingLtd/africastalking-python (SMS.py, Service.py)
//
// Tests point the providers at fakes with SMS_TWILIO_API_BASE and
// SMS_AFRICASTALKING_API_BASE (/home/claude/tools/fake-sms.mjs).

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// SMS providers.
const (
	ProviderAT     = "africastalking"
	ProviderTwilio = "twilio"
)

// SMSClient is the HTTP client for both providers.
var SMSClient = &http.Client{Timeout: 20 * time.Second}

// ErrSMSNotConfigured: no provider is set up.
var ErrSMSNotConfigured = errors.New("SMS is not set up: add Africa's Talking or Twilio in System settings → Integrations → SMS")

// SMSResult is an accepted text.
type SMSResult struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Status   string `json:"status"`
	Cost     string `json:"cost,omitempty"`
}

// SMSError is a provider error.
type SMSError struct {
	Provider   string
	HTTPStatus int
	Code       int
	Msg        string
	Retryable  bool
}

func (e *SMSError) Error() string { return e.Msg }

// SMSProvider is a resolved sender.
type SMSProvider struct {
	Kind string // africastalking | twilio
	// Africa's Talking
	ATUsername, ATKey, ATSender string
	// Twilio
	TwSID, TwToken, TwFrom, TwService string
	// StatusURL receives delivery statuses (Twilio StatusCallback).
	StatusURL string
}

func secret(s string) string {
	s = strings.TrimSpace(s)
	if mail.IsEncrypted(s) {
		if p, err := mail.DecryptSecret(s); err == nil {
			return p
		}
	}
	return s
}

// LoadSMSSettings returns the organisation's SMS settings row (zero when none).
func LoadSMSSettings(db *gorm.DB, org string) SMSSettings {
	var st SMSSettings
	fresh(db).Where("organization_id = ?", org).Limit(1).Find(&st)
	st.OrganizationID = org
	return st
}

func twilioSettings(db *gorm.DB, org string) (models.TwilioSettings, bool) {
	var row models.IntegrationSettings
	if !hasTable(db, "integration_settings") {
		return models.TwilioSettings{}, false
	}
	if err := fresh(db).Omit("Organization").Where("organization_id = ?", org).Limit(1).Find(&row).Error; err != nil || row.OrganizationID == "" {
		return models.TwilioSettings{}, false
	}
	t := row.TwilioSettings
	ok := t.Enabled && t.EnableSMS && strings.TrimSpace(t.AccountSID) != "" && strings.TrimSpace(t.AuthToken) != "" &&
		(strings.TrimSpace(t.FromPhoneNumber) != "" || strings.TrimSpace(t.MessagingServiceSID) != "")
	return t, ok
}

// ResolveSMS picks the organisation's provider: the chosen one, else
// Africa's Talking when set up, else Twilio.
func ResolveSMS(db *gorm.DB, org string) (*SMSProvider, error) {
	st := LoadSMSSettings(db, org)
	atOK := strings.TrimSpace(st.ATUsername) != "" && strings.TrimSpace(st.ATAPIKey) != ""
	tw, twOK := twilioSettings(db, org)
	useAT := func() *SMSProvider {
		return &SMSProvider{Kind: ProviderAT, ATUsername: strings.TrimSpace(st.ATUsername), ATKey: secret(st.ATAPIKey), ATSender: strings.TrimSpace(st.ATSenderID)}
	}
	useTw := func() *SMSProvider {
		return &SMSProvider{Kind: ProviderTwilio, TwSID: strings.TrimSpace(tw.AccountSID), TwToken: secret(tw.AuthToken), TwFrom: strings.TrimSpace(tw.FromPhoneNumber),
			TwService: strings.TrimSpace(tw.MessagingServiceSID)}
	}
	switch st.Provider {
	case ProviderAT:
		if !atOK {
			return nil, errors.New("Africa's Talking is chosen for SMS but its username and API key aren't saved")
		}
		return useAT(), nil
	case ProviderTwilio:
		if !twOK {
			return nil, errors.New("Twilio is chosen for SMS but it isn't set up (enable it and add the account SID, auth token and From number)")
		}
		return useTw(), nil
	}
	if atOK {
		return useAT(), nil
	}
	if twOK {
		return useTw(), nil
	}
	return nil, ErrSMSNotConfigured
}

// SMSStatus describes SMS for the composer and settings.
type SMSStatus struct {
	Ready      bool   `json:"ready"`
	Provider   string `json:"provider"`
	Note       string `json:"note"`
	ATSet      bool   `json:"at_set"`
	TwilioSet  bool   `json:"twilio_set"`
	ATKeyHint  string `json:"at_key_hint"`
	TwilioFrom string `json:"twilio_from"`
}

// StatusOfSMS reports which provider org would use.
func StatusOfSMS(db *gorm.DB, org string) SMSStatus {
	st := LoadSMSSettings(db, org)
	tw, twOK := twilioSettings(db, org)
	out := SMSStatus{ATSet: st.ATUsername != "" && st.ATAPIKey != "", TwilioSet: twOK, TwilioFrom: firstNonEmpty(tw.FromPhoneNumber, tw.MessagingServiceSID)}
	if k := secret(st.ATAPIKey); len(k) > 4 {
		out.ATKeyHint = "••••" + k[len(k)-4:]
	} else if k != "" {
		out.ATKeyHint = "••••"
	}
	p, err := ResolveSMS(db, org)
	if err != nil {
		out.Note = err.Error()
		return out
	}
	out.Ready, out.Provider = true, p.Kind
	if p.Kind == ProviderAT {
		out.Note = "Sent with Africa's Talking"
		if p.ATSender != "" {
			out.Note += " as " + p.ATSender
		}
	} else {
		out.Note = "Sent with Twilio from " + firstNonEmpty(p.TwFrom, p.TwService)
	}
	return out
}

func envBase(key, def string) string {
	if b := strings.TrimRight(strings.TrimSpace(os.Getenv(key)), "/"); b != "" {
		return b
	}
	return def
}

// E164 turns a phone number into +<country code><number>.
func E164(raw, country string) (string, error) {
	d, err := wa.NormalizeNumber(raw, country)
	if err != nil {
		return "", err
	}
	return "+" + d, nil
}

// Send sends one text to an E.164 number.
func (p *SMSProvider) Send(ctx context.Context, to, text string) (SMSResult, error) {
	if strings.TrimSpace(text) == "" {
		return SMSResult{}, &SMSError{Provider: p.Kind, Msg: "the text is empty"}
	}
	if p.Kind == ProviderAT {
		return p.sendAT(ctx, to, text)
	}
	return p.sendTwilio(ctx, to, text)
}

func (p *SMSProvider) sendAT(ctx context.Context, to, text string) (SMSResult, error) {
	host := "https://api.africastalking.com"
	if p.ATUsername == "sandbox" {
		host = "https://api.sandbox.africastalking.com"
	}
	u := envBase("SMS_AFRICASTALKING_API_BASE", host) + "/version1/messaging"
	form := url.Values{"username": {p.ATUsername}, "to": {to}, "message": {text}, "bulkSMSMode": {"1"}}
	if p.ATSender != "" {
		form.Set("from", p.ATSender)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return SMSResult{}, err
	}
	req.Header.Set("apiKey", p.ATKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := SMSClient.Do(req)
	if err != nil {
		return SMSResult{}, &SMSError{Provider: ProviderAT, Msg: "Could not reach Africa's Talking: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		e := &SMSError{Provider: ProviderAT, HTTPStatus: resp.StatusCode, Retryable: resp.StatusCode >= 500 || resp.StatusCode == 429}
		switch {
		case resp.StatusCode == 401:
			e.Msg = "Africa's Talking rejected the username or API key"
		case msg != "":
			e.Msg = "Africa's Talking: " + msg
		default:
			e.Msg = fmt.Sprintf("Africa's Talking answered HTTP %d", resp.StatusCode)
		}
		return SMSResult{}, e
	}
	var r struct {
		SMSMessageData struct {
			Message    string `json:"Message"`
			Recipients []struct {
				StatusCode int    `json:"statusCode"`
				Number     string `json:"number"`
				Status     string `json:"status"`
				Cost       string `json:"cost"`
				MessageID  string `json:"messageId"`
			} `json:"Recipients"`
		} `json:"SMSMessageData"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return SMSResult{}, &SMSError{Provider: ProviderAT, Msg: "Africa's Talking sent an answer that couldn't be read"}
	}
	if len(r.SMSMessageData.Recipients) == 0 {
		m := r.SMSMessageData.Message
		e := &SMSError{Provider: ProviderAT, Msg: "Africa's Talking didn't send it: " + firstNonEmpty(m, "no recipients accepted")}
		if strings.Contains(strings.ToLower(m), "invalidsenderid") {
			e.Msg = "Africa's Talking rejected the sender ID — use one approved on your account, or leave it empty"
		}
		return SMSResult{}, e
	}
	rc := r.SMSMessageData.Recipients[0]
	switch rc.StatusCode {
	case 100, 101, 102:
		return SMSResult{Provider: ProviderAT, ID: rc.MessageID, Status: strings.ToLower(rc.Status), Cost: rc.Cost}, nil
	}
	return SMSResult{}, &SMSError{Provider: ProviderAT, Code: rc.StatusCode, Msg: atReason(rc.StatusCode, rc.Status), Retryable: rc.StatusCode == 500 || rc.StatusCode == 501}
}

func atReason(code int, status string) string {
	switch code {
	case 401:
		return "Africa's Talking is holding this message for a risk review"
	case 402:
		return "The sender ID isn't approved on your Africa's Talking account"
	case 403:
		return "This isn't a valid phone number"
	case 404:
		return "This kind of number can't receive SMS"
	case 405:
		return "Your Africa's Talking balance is too low — top up the account"
	case 406:
		return "This person has blocked promotional messages (blacklist)"
	case 407:
		return "Africa's Talking couldn't route a message to this number"
	case 409:
		return "The network rejected it (Do Not Disturb is on for this number)"
	case 500, 501:
		return "Africa's Talking had a temporary problem; it will be retried"
	case 502:
		return "The mobile network rejected the message"
	}
	return fmt.Sprintf("Africa's Talking didn't send it (%d %s)", code, status)
}

func (p *SMSProvider) sendTwilio(ctx context.Context, to, text string) (SMSResult, error) {
	u := envBase("SMS_TWILIO_API_BASE", "https://api.twilio.com") + "/2010-04-01/Accounts/" + url.PathEscape(p.TwSID) + "/Messages.json"
	form := url.Values{"To": {to}, "Body": {text}}
	if p.TwService != "" {
		form.Set("MessagingServiceSid", p.TwService)
	} else {
		form.Set("From", p.TwFrom)
	}
	if p.StatusURL != "" {
		form.Set("StatusCallback", p.StatusURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return SMSResult{}, err
	}
	req.SetBasicAuth(p.TwSID, p.TwToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := SMSClient.Do(req)
	if err != nil {
		return SMSResult{}, &SMSError{Provider: ProviderTwilio, Msg: "Could not reach Twilio: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		SID          string `json:"sid"`
		Status       string `json:"status"`
		Code         int    `json:"code"`
		Message      string `json:"message"`
		ErrorCode    *int   `json:"error_code"`
		ErrorMessage string `json:"error_message"`
	}
	_ = json.Unmarshal(raw, &r)
	if resp.StatusCode >= 300 {
		return SMSResult{}, &SMSError{Provider: ProviderTwilio, HTTPStatus: resp.StatusCode, Code: r.Code, Msg: twilioReason(r.Code, r.Message, resp.StatusCode),
			Retryable: resp.StatusCode >= 500 || resp.StatusCode == 429 || r.Code == 20429}
	}
	if r.SID == "" {
		return SMSResult{}, &SMSError{Provider: ProviderTwilio, Msg: "Twilio sent an answer that couldn't be read"}
	}
	return SMSResult{Provider: ProviderTwilio, ID: r.SID, Status: r.Status}, nil
}

func twilioReason(code int, msg string, status int) string {
	switch code {
	case 20003:
		return "Twilio rejected the account SID or auth token"
	case 21211, 21614:
		return "This isn't a valid mobile number"
	case 21408:
		return "Your Twilio account isn't allowed to text this country (Messaging → Settings → Geo permissions)"
	case 21608:
		return "Twilio trial accounts can only text verified numbers"
	case 21610:
		return "This person replied STOP — Twilio won't deliver until they reply START"
	case 21606, 21659, 21212:
		return "The From number can't send SMS — check it in Twilio"
	case 20429:
		return "Twilio rate limit reached; it will be retried"
	}
	if msg != "" {
		return "Twilio: " + msg
	}
	return fmt.Sprintf("Twilio answered HTTP %d", status)
}

// twilioErrorText explains a Twilio delivery error code from a status callback.
func twilioErrorText(code string) string {
	switch code {
	case "30003":
		return "The phone is off or out of coverage"
	case "30004":
		return "The person blocked messages from this number"
	case "30005":
		return "The number doesn't exist"
	case "30006":
		return "This number can't receive SMS (landline or unreachable carrier)"
	case "30007":
		return "The carrier filtered the message as spam"
	case "30008":
		return "Unknown delivery error"
	}
	if code == "" {
		return "Not delivered"
	}
	return "Not delivered (Twilio error " + code + ")"
}

// TwilioSignature is X-Twilio-Signature for a POST to fullURL with form.
func TwilioSignature(token, fullURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(fullURL)
	for _, k := range keys {
		for _, v := range form[k] {
			b.WriteString(k)
			b.WriteString(v)
		}
	}
	m := hmac.New(sha1.New, []byte(token))
	m.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// sendSMS sends through the organisation's provider (or the test hook).
func (s *Service) sendSMS(ctx context.Context, org, to, text, ref string) (SMSResult, error) {
	if s.SMS != nil {
		return s.SMS(ctx, org, to, text, ref)
	}
	p, err := ResolveSMS(s.DB, org)
	if err != nil {
		return SMSResult{}, &SMSError{Msg: err.Error()}
	}
	if p.Kind == ProviderTwilio && s.apiBase() != "" {
		p.StatusURL = s.apiBase() + "/api/v1/public/sms/twilio/status"
	}
	return p.Send(ctx, to, text)
}
