package schedsend

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// ---------------------------------------------------------------- invoices

// InvoiceResult is what the invoicing send answered.
type InvoiceResult struct {
	Emailed bool
	LogID   string
	To      string
	CC      []string
	BCC     []string
	Subject string
}

// APIError is a non-2xx answer from the invoicing API.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

// InvoiceSender sends one invoice / quote / reminder / receipt email.
type InvoiceSender interface {
	Send(ctx context.Context, who Actor, invoiceID string, body map[string]any) (InvoiceResult, error)
}

// APIInvoices calls POST /api/v1/invoicing/invoices/:id/send in-process, as
// the person who scheduled the email — the exact path of the Send button
// (pre-send checks, branded email + PDF, email log, per-recipient tracking,
// activity history). The role checks run again at send time.
type APIInvoices struct {
	Engine func() http.Handler
	Secret []byte
}

// Send implements InvoiceSender.
func (a *APIInvoices) Send(ctx context.Context, who Actor, invoiceID string, body map[string]any) (InvoiceResult, error) {
	var out InvoiceResult
	if a.Engine == nil || a.Engine() == nil {
		return out, &APIError{Status: 503, Code: "NOT_READY", Message: "the server is still starting"}
	}
	now := time.Now()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"user_id": who.User, "org_id": who.Org, "role": who.Role, "email": who.Email,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "src": "schedsend"}).SignedString(a.Secret)
	if err != nil {
		return out, err
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/invoicing/invoices/"+invoiceID+"/send", bytes.NewReader(buf))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-By", "dasyin-schedsend")
	rec := httptest.NewRecorder()
	a.Engine().ServeHTTP(rec, req)
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code >= 300 {
		e := &APIError{Status: rec.Code, Message: http.StatusText(rec.Code)}
		if m, ok := res["error"].(map[string]any); ok {
			e.Code, _ = m["code"].(string)
			if s, ok := m["message"].(string); ok && s != "" {
				e.Message = s
			}
			if checks, ok := m["checks"].([]any); ok {
				for _, c := range checks {
					if cm, ok := c.(map[string]any); ok && cm["severity"] == "error" {
						if t, _ := cm["title"].(string); t != "" {
							e.Message += " — " + t
							break
						}
					}
				}
			}
		} else if s, ok := res["error"].(string); ok && s != "" {
			e.Message = s
		}
		return out, e
	}
	d, _ := res["data"].(map[string]any)
	out.Emailed, _ = d["emailed"].(bool)
	out.LogID, _ = d["log_id"].(string)
	out.To, _ = d["to"].(string)
	out.Subject, _ = d["subject"].(string)
	for _, k := range []string{"cc", "bcc"} {
		if l, ok := d[k].([]any); ok {
			for _, v := range l {
				if s, ok := v.(string); ok {
					if k == "cc" {
						out.CC = append(out.CC, s)
					} else {
						out.BCC = append(out.BCC, s)
					}
				}
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- WhatsApp

// WAAccount sends WhatsApp messages (*wa.Account).
type WAAccount interface {
	SendText(ctx context.Context, to, body string, previewURL bool) (*wa.Result, error)
	SendTemplate(ctx context.Context, to string, t wa.Template) (*wa.Result, error)
}

// WhatsApp resolves the organisation's own number and the 24-hour window.
type WhatsApp interface {
	Resolve(org string) (WAAccount, error)
	WindowOpen(org, phone string, now time.Time) bool
}

type waGateway struct{ db *gorm.DB }

func (g waGateway) Resolve(org string) (WAAccount, error) {
	// Customers (and staff) only ever hear from the organisation's own number.
	a, err := wa.Resolve(fresh(g.db), org, false)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (g waGateway) WindowOpen(org, phone string, now time.Time) bool {
	return wa.WindowOpen(fresh(g.db), org, phone, now)
}

// windowNow is the time the 24-hour WhatsApp window is checked at: always the
// real clock (the window is Meta's rule and its timestamps are real), even
// when the test clock is moved.
func windowNow() time.Time { return time.Now() }

func (s *Service) wa() WhatsApp {
	if s.WA != nil {
		return s.WA
	}
	return waGateway{s.DB}
}

// ---------------------------------------------------------------- email

func (s *Service) sendMail(ctx context.Context, org string, msg mail.Message) (string, error) {
	if s.Mail != nil {
		return s.Mail(ctx, org, msg)
	}
	svc := mail.Shared(fresh(s.DB))
	cfg, _ := svc.Resolve(org)
	if !cfg.Ready() {
		if cfg.Problem != "" {
			return "", &mail.Error{Code: "NOT_CONFIGURED", Msg: cfg.Problem}
		}
		return "", mail.ErrNotConfigured
	}
	if msg.MessageID == "" {
		msg.MessageID = mail.NewMessageID(cfg.FromEmail)
	}
	err := svc.SendWith(ctx, org, cfg, msg)
	return msg.MessageID, err
}

// EmailReady reports whether the organisation can send email.
func (s *Service) EmailReady(org string) (bool, string, string) {
	cfg, _ := mail.Shared(fresh(s.DB)).Resolve(org)
	return cfg.Ready(), cfg.Display(), cfg.Problem
}

type orgInfo struct {
	Name, Email, Phone, Address, Accent, Logo string
}

func (s *Service) info(org string) orgInfo {
	var o struct{ Name, Email, Phone string }
	fresh(s.DB).Raw(`SELECT COALESCE(name,'') AS name, COALESCE(email,'') AS email, COALESCE(phone,'') AS phone FROM organizations WHERE id = ?`, org).Scan(&o)
	info := orgInfo{Name: s.orgName(org), Email: o.Email, Phone: o.Phone}
	if hasTable(s.DB, "billing_settings") {
		fresh(s.DB).Raw(`SELECT COALESCE(accent_color,'') FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&info.Accent)
	}
	if s.LogoURL != nil {
		info.Logo = s.LogoURL(org)
	}
	return info
}

// emailFor renders a customer, statement or team email in the shared branded
// template (pkg/invoicing RenderEmail).
func (s *Service) emailFor(org string, m *Message, subject, body string, open []OpenInvoice, unsubURL, pixelURL string, team bool) (string, string) {
	info := s.info(org)
	v := invoicing.EmailView{Kind: "reminder", Business: invoicing.EmailBusiness{Name: info.Name, Email: info.Email, Phone: info.Phone},
		LogoURL: info.Logo, Accent: info.Accent, Preheader: subject, DocLabel: "Message from " + info.Name, Message: body, TrackingPixelURL: pixelURL}
	if team {
		v.DocLabel = "Team message"
	}
	if m.Kind == KindStatement {
		v.DocLabel = "Statement of account"
		home := s.homeCurrency(org)
		v.AmountLabel, v.Amount = "Balance to pay", balanceText(open, home)
		if len(open) == 0 {
			v.StatusLine, v.StatusTone = "Nothing outstanding — thank you!", "ok"
		} else {
			v.StatusLine, v.StatusTone = fmt.Sprintf("%d unpaid invoice(s)", len(open)), "warn"
		}
		for i, o := range open {
			if i >= 12 {
				v.MoreLines = len(open) - 12
				break
			}
			v.Lines = append(v.Lines, invoicing.EmailLine{Description: o.Number, Detail: "Due " + longDate(o.DueDate), Amount: invoicing.FormatMoney(o.BalanceDue, o.Currency)})
		}
		if len(open) > 0 {
			if u := s.payLink(org, open[0].PublicToken); u != "" {
				v.Actions = []invoicing.EmailAction{{Label: "View & pay " + open[0].Number, URL: u, Primary: true}}
			}
		}
	}
	if unsubURL != "" {
		v.FooterNotice = "You get offers from " + info.Name + ". Don't want them? Unsubscribe: " + unsubURL
	}
	if info.Email != "" && !team {
		v.ReplyTo = "Questions? Just reply to this email."
	}
	return invoicing.RenderEmail(v)
}

// ---------------------------------------------------------------- signed links

func (s *Service) sign(parts ...string) string {
	m := hmac.New(sha256.New, append([]byte("schedsend:"), s.Secret...))
	m.Write([]byte(strings.Join(parts, "|")))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

// token is "<a>.<b>.<sig>" (a, b are ids).
func (s *Service) token(kind, a, b string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(a)) + "." + base64.RawURLEncoding.EncodeToString([]byte(b)) + "." + s.sign(kind, a, b)
}

func (s *Service) readToken(kind, tok string) (string, string, bool) {
	p := strings.Split(strings.TrimSuffix(tok, ".gif"), ".")
	if len(p) != 3 {
		return "", "", false
	}
	a, err1 := base64.RawURLEncoding.DecodeString(p[0])
	b, err2 := base64.RawURLEncoding.DecodeString(p[1])
	if err1 != nil || err2 != nil || len(s.Secret) == 0 {
		return "", "", false
	}
	if !hmac.Equal([]byte(s.sign(kind, string(a), string(b))), []byte(p[2])) {
		return "", "", false
	}
	return string(a), string(b), true
}

func (s *Service) apiBase() string { return strings.TrimRight(s.APIURL, "/") }

// UnsubscribeURL is the one-click unsubscribe link of a customer.
func (s *Service) UnsubscribeURL(org, customerID string) string {
	if s.apiBase() == "" || len(s.Secret) == 0 {
		return ""
	}
	return s.apiBase() + "/api/v1/public/schedsend/unsubscribe?t=" + s.token("unsub", org, customerID)
}

func (s *Service) pixelURL(org, deliveryID string) string {
	if s.apiBase() == "" || len(s.Secret) == 0 {
		return ""
	}
	if !invoicing.LoadTrackingPrefs(fresh(s.DB), org).TrackOpens {
		return ""
	}
	return s.apiBase() + "/api/v1/public/schedsend/open/" + s.token("open", org, deliveryID) + ".gif"
}

// ---------------------------------------------------------------- errors

// temporary reports whether err is worth retrying and gives a plain reason.
func temporary(channel string, err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	var we *wa.Error
	if errors.As(err, &we) {
		return we.Retryable(), wa.Friendly(err)
	}
	var se *SMSError
	if errors.As(err, &se) {
		return se.Retryable, se.Error()
	}
	var ae *APIError
	if errors.As(err, &ae) {
		retry := ae.Status >= 500 && ae.Code != "SMTP_NOT_CONFIGURED"
		return retry, ae.Message
	}
	var me *mail.Error
	if errors.As(err, &me) {
		switch me.Code {
		case "TIMEOUT", "CONNECTION_REFUSED", "DNS", "SMTP":
			return true, me.Error()
		}
		return false, me.Error()
	}
	if errors.Is(err, wa.ErrNotConfigured) {
		return false, err.Error()
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	if strings.Contains(low, "timeout") || strings.Contains(low, "connection refused") || strings.Contains(low, "could not reach") {
		return true, msg
	}
	return false, msg
}
