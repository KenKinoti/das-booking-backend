package broadcast

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// ---------------------------------------------------------------- tokens

// Token purposes.
const (
	PurposeOpen  = "open"  // the email's buttons: mark read + clicked, then go on
	PurposeUnsub = "unsub" // the offer's unsubscribe link
)

func (s *Service) sign(payload string) string {
	m := hmac.New(sha256.New, append([]byte("broadcast:"), s.Secret...))
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

// Token signs purpose + delivery id (+ an optional target for open links).
func (s *Service) Token(purpose, deliveryID, target string) string {
	p := purpose + "|" + deliveryID + "|" + target
	return base64.RawURLEncoding.EncodeToString([]byte(p)) + "." + s.sign(p)
}

// ErrBadToken is returned for a link that was changed or is not ours.
var ErrBadToken = errors.New("this link is not valid — it may have been cut short")

// ParseToken checks a token and returns the delivery id and target.
func (s *Service) ParseToken(tok, purpose string) (deliveryID, target string, err error) {
	parts := strings.SplitN(strings.TrimSpace(tok), ".", 2)
	if len(parts) != 2 || len(s.Secret) == 0 {
		return "", "", ErrBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", ErrBadToken
	}
	if !hmac.Equal([]byte(s.sign(string(raw))), []byte(parts[1])) {
		return "", "", ErrBadToken
	}
	f := strings.SplitN(string(raw), "|", 3)
	if len(f) != 3 || f[0] != purpose || f[1] == "" {
		return "", "", ErrBadToken
	}
	return f[1], f[2], nil
}

// ---------------------------------------------------------------- links

func (s *Service) appURL() string {
	u := strings.TrimRight(strings.TrimSpace(s.AppURL), "/")
	if u == "" {
		u = "http://localhost:5173"
	}
	return u
}

func (s *Service) apiURL() string {
	u := strings.TrimRight(strings.TrimSpace(s.APIURL), "/")
	if u == "" {
		u = s.appURL()
	}
	return u
}

// OpenURL is the tracked link of an email button: it marks the delivery read
// and clicked, then goes to the target ("cta" or "view").
func (s *Service) OpenURL(deliveryID, target string) string {
	return s.apiURL() + "/api/v1/public/announcements/open?t=" + url.QueryEscape(s.Token(PurposeOpen, deliveryID, target))
}

// UnsubscribeURL is the offer's one-click unsubscribe page.
func (s *Service) UnsubscribeURL(deliveryID string) string {
	return s.appURL() + "/unsubscribe?t=" + url.QueryEscape(s.Token(PurposeUnsub, deliveryID, ""))
}

// ViewPath is where the in-app notification and the email's "Read" button go.
func ViewPath(deliveryID string) string {
	return "/dashboard?announcement=" + url.QueryEscape(deliveryID)
}

// ResolveTarget is where an open link lands in the app.
func (s *Service) ResolveTarget(a *Announcement, d *Delivery, target string) string {
	if target == "cta" && a.CTAURL != "" {
		if strings.HasPrefix(a.CTAURL, "/") {
			return s.appURL() + a.CTAURL
		}
		return a.CTAURL
	}
	return s.appURL() + ViewPath(d.ID)
}

// ---------------------------------------------------------------- email

// KindLabel is the label of a kind ("Update", "Offer" …).
func KindLabel(kind string) string {
	switch kind {
	case KindReminder:
		return "Reminder"
	case KindPromotion:
		return "Offer"
	case KindUrgent:
		return "Urgent notice"
	}
	return "Update"
}

// ServiceNotice: urgent messages about the service are emailed even to people
// who turned off "Updates from DASYIN" (never offers).
func ServiceNotice(a *Announcement) bool {
	if a.Kind == KindPromotion {
		return false
	}
	return a.Kind == KindUrgent || a.Priority == PriorityUrgent
}

const bodyMarker = "@@DASYIN_ANNOUNCEMENT_BODY@@"

// PlatformInfo is the sender block of the emails (the platform organisation).
type PlatformInfo struct {
	Name, Email, Phone, Address, Website, LogoURL string
}

func (s *Service) platformInfo() PlatformInfo {
	info := PlatformInfo{Name: "DASYIN"}
	if s.Platform != nil {
		info = s.Platform()
		if strings.TrimSpace(info.Name) == "" {
			info.Name = "DASYIN"
		}
	}
	return info
}

// EmailFor renders the email of a delivery (test marks a test send).
func (s *Service) EmailFor(a *Announcement, d *Delivery, test bool) mail.Message {
	const accent = "#4f46e5"
	info := s.platformInfo()
	subject := a.Title
	if a.Kind == KindUrgent || a.Priority == PriorityUrgent {
		subject = "Urgent: " + subject
	}
	if test {
		subject = "[Test] " + subject
	}
	label := KindLabel(a.Kind) + " from " + info.Name
	actions := []invoicing.EmailAction{}
	if a.CTALabel != "" && a.CTAURL != "" {
		actions = append(actions, invoicing.EmailAction{Label: a.CTALabel, URL: s.OpenURL(d.ID, "cta"), Primary: true})
		actions = append(actions, invoicing.EmailAction{Label: "Open in DASYIN", URL: s.OpenURL(d.ID, "view")})
	} else {
		actions = append(actions, invoicing.EmailAction{Label: "Read in DASYIN", URL: s.OpenURL(d.ID, "view"), Primary: true})
	}
	v := invoicing.EmailView{
		Kind: "reminder", Accent: accent, DocLabel: label,
		Business:  invoicing.EmailBusiness{Name: info.Name, Email: info.Email, Phone: info.Phone, Address: info.Address, Website: info.Website},
		LogoURL:   info.LogoURL,
		Preheader: clip(PlainText(a.Body, false, ""), 140),
		Actions:   actions,
	}
	switch {
	case a.Kind == KindPromotion:
		v.Note = "You get DASYIN offers because you use DASYIN ERP. Unsubscribe with one click (link above) — offers stop straight away, by email and in the app."
		v.Groups = []invoicing.EmailActionGroup{{Links: []invoicing.EmailAction{{Label: "Unsubscribe from DASYIN offers", URL: s.UnsubscribeURL(d.ID)}}}}
	case ServiceNotice(a):
		v.Note = "This is an urgent notice about the DASYIN service, so it is sent even if you turned off update emails."
	default:
		v.Note = "You can turn off these emails in DASYIN → My profile → Email notifications (“Updates from DASYIN”)."
	}
	if a.EmailDelayMinutes > 0 {
		v.Note += " You got this email because you hadn't read it in the app yet."
	} else {
		v.Note += " It is also in your DASYIN notifications."
	}
	if test {
		v.Note = "Test send — only you received this. " + v.Note
	}
	// The text part: the plain body with link addresses.
	tv := v
	tv.Message = a.Title + "\n\n" + PlainText(a.Body, true, s.appURL())
	if a.ImageURL != "" {
		tv.Message += "\n\nImage: " + a.ImageURL
	}
	_, text := invoicing.RenderEmail(tv)
	// The HTML part: title, image and the rich body in place of the message.
	v.Message = bodyMarker
	htmlOut, _ := invoicing.RenderEmail(v)
	const font = `-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif`
	var rich strings.Builder
	rich.WriteString(`<div class="tx" style="font:700 21px/1.35 ` + font + `;color:#1a1d2e;margin:0 0 14px">` + escape(a.Title) + `</div>`)
	if a.ImageURL != "" {
		rich.WriteString(`<img src="` + escape(a.ImageURL) + `" alt="" width="536" style="display:block;width:100%;max-width:536px;height:auto;border:0;border-radius:10px;margin:0 0 16px">`)
	}
	rich.WriteString(EmailHTML(a.Body, accent, s.appURL()))
	htmlOut = strings.Replace(htmlOut, bodyMarker, rich.String(), 1)
	return mail.Message{To: []string{d.Email}, Subject: subject, HTML: htmlOut, Text: text}
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// sendMail sends through the platform account (System settings → Email, the
// super admins' organisation, then SMTP_* — pkg/mail).
func (s *Service) sendMail(ctx context.Context, msg mail.Message) error {
	if s.SendMail != nil {
		return s.SendMail(ctx, msg)
	}
	svc := mail.Shared(s.DB)
	org := ""
	if orgs := svc.PlatformOrgs(); len(orgs) > 0 {
		org = orgs[0]
	}
	_, err := svc.SendAs(ctx, org, msg)
	return err
}
