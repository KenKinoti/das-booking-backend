package invoicing

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// ---------------------------------------------------------------------------
// Per-recipient email tracking.
//
// Every customer email (invoice, quote, reminder, receipt — everything
// sendEmail sends through the organisation's mail account) goes to each To/CC
// person as their own copy (To: that person, "Also sent to: …" in the body),
// so each copy carries its own unguessable token:
//
//   - opened: a 1×1 image /public/email-open/<token>.gif (only when the
//     organisation keeps open tracking on). Opens that look automatic (Apple
//     Mail Privacy Protection, security scanners, prefetch within ~10 s of
//     sending) are counted separately — "opened" is never certain, because
//     many mail apps don't load images until asked to;
//   - viewed: the View / Pay links carry ?r=<token>; the public invoice and
//     pay pages report which copy's link was used — reliable;
//   - paid via link: the pay page reports the checkout reference started from
//     that link; a successful online payment with that reference is credited
//     to the recipient;
//   - delivery failures: per copy, from the SMTP server.
//
// BCC addresses get their own untracked copy (their link use isn't counted as
// the client's). Links without a token (older emails, "Copy client link")
// keep working exactly as before. "Track each recipient separately" off sends
// one message to everyone, as before.
// ---------------------------------------------------------------------------

const (
	TrackPerRecipient = "per_recipient"
	TrackSingle       = "single"

	// maxTrackedRecipients: above this many To/CC addresses the email goes out
	// as one message (one SMTP session per copy would take too long).
	maxTrackedRecipients = 20
	// automaticWindow: an open this soon after sending, from a proxy or an
	// app that isn't a normal browser, is a prefetch rather than a person.
	automaticWindow = 10 * time.Second
	// openDedupe: the same client fetching the image twice in a row (common)
	// counts once.
	openDedupe      = 3 * time.Second
	maxCheckoutRefs = 10

	// OpenNoticeText is the optional footer line (Settings → Email tracking).
	OpenNoticeText = "This email may record when it's opened."
)

// EmailRecipient is one person's copy of one email about a document and
// everything recorded about it. Stores no full IP address: only the /24
// (IPv4) or /48 (IPv6) network and the user agent of the last open.
type EmailRecipient struct {
	ID             string `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	InvoiceID      string `json:"invoice_id" gorm:"type:varchar(64);index;not null"`
	EmailLogID     string `json:"email_log_id" gorm:"type:varchar(64);index"`
	Kind           string `json:"kind" gorm:"type:varchar(20)"`
	Email          string `json:"email" gorm:"type:varchar(320)"`
	Role           string `json:"role" gorm:"type:varchar(10)"` // to | cc | bcc
	Position       int    `json:"position"`                     // order within the send (To, CCs, BCCs)
	// Tracked: the copy has its own links (false for BCC copies and for
	// emails sent as one message). OpenTracking: it carried the open image.
	Tracked      bool `json:"tracked"`
	OpenTracking bool `json:"open_tracking"`
	// Token identifies the copy in its links and open image (nil when the
	// copy has none). 24 random bytes, URL-safe base64.
	Token     *string    `json:"-" gorm:"type:varchar(64);uniqueIndex"`
	Status    string     `json:"status" gorm:"type:varchar(20)"` // sending | sent | failed
	ErrorCode string     `json:"error_code,omitempty" gorm:"type:varchar(40)"`
	Error     string     `json:"error,omitempty" gorm:"type:text"`
	MessageID string     `json:"message_id" gorm:"type:varchar(255)"`
	SentAt    *time.Time `json:"sent_at"`

	OpenCount        int        `json:"open_count"`
	AutoOpenCount    int        `json:"auto_open_count"`
	FirstOpenedAt    *time.Time `json:"first_opened_at"`
	LastOpenedAt     *time.Time `json:"last_opened_at"`
	FirstHumanOpenAt *time.Time `json:"first_human_open_at"`
	LastHumanOpenAt  *time.Time `json:"last_human_open_at"`
	OpenVia          string     `json:"open_via" gorm:"type:varchar(20)"`
	LastUserAgent    string     `json:"-" gorm:"type:varchar(300)"`
	LastIPClass      string     `json:"-" gorm:"type:varchar(64)"`

	ViewCount     int        `json:"view_count"`
	FirstViewedAt *time.Time `json:"first_viewed_at"`
	LastViewedAt  *time.Time `json:"last_viewed_at"`
	PayPageAt     *time.Time `json:"pay_page_at"`
	AcceptedAt    *time.Time `json:"accepted_at"`
	CheckoutRefs  []string   `json:"-" gorm:"type:text;serializer:json"`
	CreatedAt     time.Time  `json:"created_at"`
}

func (EmailRecipient) TableName() string { return "invoice_email_recipients" }

// EmailTrackingSettings is an organisation's email tracking choice. Stored
// inverted so a missing row (and every new column) means the default:
// per-recipient copies on, open tracking on, footer notice off.
type EmailTrackingSettings struct {
	OrganizationID string    `gorm:"type:varchar(255);primaryKey"`
	SingleMessage  bool      // "Track each recipient separately" off
	NoOpenTracking bool      // the open image off (links are still tracked)
	OpenNotice     bool      // "This email may record when it's opened" in the footer
	UpdatedBy      string    `gorm:"type:varchar(255)"`
	UpdatedAt      time.Time ``
}

func (EmailTrackingSettings) TableName() string { return "invoice_email_tracking" }

// TrackingPrefs is the effective setting (positive names, for the API).
type TrackingPrefs struct {
	TrackRecipients bool       `json:"track_recipients"`
	TrackOpens      bool       `json:"track_opens"`
	OpenNotice      bool       `json:"open_notice"`
	NoticeText      string     `json:"notice_text"`
	UpdatedAt       *time.Time `json:"updated_at"`
}

// LoadTrackingPrefs returns org's tracking setting (the defaults when unset).
func LoadTrackingPrefs(db *gorm.DB, org string) TrackingPrefs {
	p := TrackingPrefs{TrackRecipients: true, TrackOpens: true, NoticeText: OpenNoticeText}
	var s EmailTrackingSettings
	if org == "" || db.Where("organization_id = ?", org).Limit(1).Find(&s).Error != nil || s.OrganizationID == "" {
		return p
	}
	p.TrackRecipients, p.TrackOpens, p.OpenNotice = !s.SingleMessage, !s.NoOpenTracking, s.OpenNotice
	t := s.UpdatedAt
	p.UpdatedAt = &t
	return p
}

// GetEmailTracking serves GET /email-tracking.
func (h *Handler) GetEmailTracking(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	ok(c, http.StatusOK, LoadTrackingPrefs(h.DB, org))
}

// PutEmailTracking serves PUT /email-tracking {track_recipients, track_opens,
// open_notice}; fields left out keep their value.
func (h *Handler) PutEmailTracking(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in struct {
		TrackRecipients *bool `json:"track_recipients"`
		TrackOpens      *bool `json:"track_opens"`
		OpenNotice      *bool `json:"open_notice"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	var s EmailTrackingSettings
	h.DB.Where("organization_id = ?", org).Limit(1).Find(&s)
	s.OrganizationID = org
	if in.TrackRecipients != nil {
		s.SingleMessage = !*in.TrackRecipients
	}
	if in.TrackOpens != nil {
		s.NoOpenTracking = !*in.TrackOpens
	}
	if in.OpenNotice != nil {
		s.OpenNotice = *in.OpenNotice
	}
	s.UpdatedBy, s.UpdatedAt = ctxString(c, "user_id"), h.now()
	if err := h.DB.Save(&s).Error; err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, LoadTrackingPrefs(h.DB, org))
}

// ---------------------------------------------------------------- tokens & links

func newRecipientToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// validRecipientToken: exactly the shape newRecipientToken makes.
func validRecipientToken(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isWordByte(s[i]) && s[i] != '-' {
			return false
		}
	}
	return true
}

// withRecipientToken adds ?r=<tok> (or &r=) to a link.
func withRecipientToken(u, tok string) string {
	if u == "" || tok == "" {
		return u
	}
	if strings.Contains(u, "?") {
		return u + "&r=" + tok
	}
	return u + "?r=" + tok
}

// tokeniseLinks adds the copy's token to every occurrence of urls in a plain
// text message (e.g. a {{link}} placeholder), keeping any query string.
func tokeniseLinks(s string, urls []string, tok string) string {
	for _, u := range urls {
		if u == "" || !strings.Contains(s, u) {
			continue
		}
		var out strings.Builder
		rest := s
		for {
			i := strings.Index(rest, u)
			if i < 0 {
				out.WriteString(rest)
				break
			}
			j := i + len(u)
			out.WriteString(rest[:j])
			switch {
			case j < len(rest) && (isWordByte(rest[j]) || rest[j] == '/' || rest[j] == '-'):
				// part of a longer URL — leave it
			case j+1 < len(rest) && rest[j] == '?' && !strings.ContainsRune(" \t\r\n", rune(rest[j+1])):
				out.WriteString("?r=" + tok + "&")
				j++
			default:
				out.WriteString("?r=" + tok)
			}
			rest = rest[j:]
		}
		s = out.String()
	}
	return s
}

// publicAPIBase is this API's public origin, for the open image: the
// PUBLIC_API_URL environment variable, else the request's host. "" when
// unknown (background sends without PUBLIC_API_URL: no open image).
func publicAPIBase(c *gin.Context) string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_API_URL")), "/"); v != "" {
		return strings.TrimSuffix(v, "/api/v1")
	}
	if c == nil || c.Request == nil {
		return ""
	}
	r := c.Request
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	if host == "" || (scheme != "http" && scheme != "https") {
		return ""
	}
	return scheme + "://" + host
}

// PixelURL is a copy's open-tracking image.
func PixelURL(base, tok string) string {
	if base == "" || tok == "" {
		return ""
	}
	return base + "/api/v1/public/email-open/" + tok + ".gif"
}

// personalView is the email as one recipient's copy shows it.
func (b *builtEmail) personalView(tok string, alsoSent, pixel, notice string) EmailView {
	v := b.View
	v.Actions = make([]EmailAction, len(b.View.Actions))
	for i, a := range b.View.Actions {
		if tok != "" && (a.URL == b.ViewURL || a.URL == b.PayURL) {
			a.URL = withRecipientToken(a.URL, tok)
		}
		v.Actions[i] = a
	}
	if tok != "" {
		v.Message = tokeniseLinks(v.Message, []string{b.ViewURL, b.PayURL}, tok)
	}
	v.AlsoSentTo, v.TrackingPixelURL, v.FooterNotice = alsoSent, pixel, notice
	return v
}

func alsoSentLine(everyone []string, me string) string {
	others := []string{}
	for _, e := range everyone {
		if !strings.EqualFold(e, me) {
			others = append(others, e)
		}
	}
	if len(others) == 0 {
		return ""
	}
	return "Also sent to: " + strings.Join(others, ", ")
}

// tracksSeparately reports whether an email to b's recipients goes out as
// one copy per person (setting on, not too many people).
func tracksSeparately(p TrackingPrefs, b *builtEmail) bool {
	return p.TrackRecipients && b.To != "" && 1+len(b.CC) <= maxTrackedRecipients
}

// previewAsRecipient re-renders b as the To person's copy (the "Also sent
// to" line, no token or open image) for the send dialog preview.
func (b *builtEmail) previewAsRecipient(p TrackingPrefs) {
	if !tracksSeparately(p, b) {
		return
	}
	notice := ""
	if p.TrackOpens && p.OpenNotice {
		notice = OpenNoticeText
	}
	b.HTML, b.Text = RenderEmail(b.personalView("", alsoSentLine(append([]string{b.To}, b.CC...), b.To), "", notice))
}

// ---------------------------------------------------------------- delivery

type emailCopy struct {
	row *EmailRecipient
	msg mail.Message
}

// deliver sends b through the organisation's mail account: one copy per
// To/CC person (plus an untracked copy per BCC address) when per-recipient
// tracking is on, else one message to everyone. Each copy is recorded in
// invoice_email_recipients before it goes out (so an immediate open finds
// it). It returns an error only when no To/CC copy was delivered; partial
// failures are noted on entry and on each copy.
func (h *Handler) deliver(ctx context.Context, c *gin.Context, org string, mcfg mail.Config, b *builtEmail, entry *EmailLog) error {
	prefs := LoadTrackingPrefs(h.DB, org)
	people := append([]string{b.To}, b.CC...)
	atts := []mail.Attachment{b.Attachment}
	now := h.now()
	pos := 0
	base := func(email, role string) *EmailRecipient {
		t := now
		pos++
		return &EmailRecipient{ID: uuid.NewString(), OrganizationID: org, InvoiceID: entry.InvoiceID, EmailLogID: entry.ID, Kind: b.Kind,
			Email: email, Role: role, Position: pos, Status: "sending", SentAt: &t, CreatedAt: now}
	}

	if !tracksSeparately(prefs, b) {
		// One message to everyone, as before per-recipient tracking.
		msg := mail.Message{To: []string{b.To}, CC: b.CC, BCC: b.BCC, Subject: b.Subject, Text: b.Text, HTML: b.HTML,
			Attachments: atts, MessageID: mail.NewMessageID(mcfg.FromEmail), ReplyTo: mcfg.ReplyTo}
		entry.MessageID, entry.Tracking, entry.Copies = msg.MessageID, TrackSingle, 1
		rows := []*EmailRecipient{}
		for i, p := range people {
			role := "cc"
			if i == 0 {
				role = "to"
			}
			rows = append(rows, base(p, role))
		}
		for _, p := range b.BCC {
			rows = append(rows, base(p, "bcc"))
		}
		for _, r := range rows {
			r.MessageID = msg.MessageID
		}
		h.saveCopies(rows)
		err := h.Mail.SendWith(ctx, org, mcfg, msg)
		upd := map[string]interface{}{"status": "sent"}
		if err != nil {
			entry.CopyFails = 1
			upd = map[string]interface{}{"status": "failed", "error": err.Error(), "error_code": mail.Code(err)}
		}
		h.DB.Model(&EmailRecipient{}).Where("email_log_id = ?", entry.ID).Updates(upd)
		return err
	}

	entry.Tracking = TrackPerRecipient
	apiBase := publicAPIBase(c)
	pixelOn := prefs.TrackOpens && apiBase != ""
	notice := ""
	if pixelOn && prefs.OpenNotice {
		notice = OpenNoticeText
	}
	var copies []emailCopy
	for i, p := range people {
		role := "cc"
		if i == 0 {
			role = "to"
		}
		tok := newRecipientToken()
		pixel := ""
		if pixelOn {
			pixel = PixelURL(apiBase, tok)
		}
		htmlOut, text := RenderEmail(b.personalView(tok, alsoSentLine(people, p), pixel, notice))
		row := base(p, role)
		row.Token, row.Tracked, row.OpenTracking = &tok, true, pixelOn
		row.MessageID = mail.NewMessageID(mcfg.FromEmail)
		copies = append(copies, emailCopy{row, mail.Message{To: []string{p}, Subject: b.Subject, Text: text, HTML: htmlOut,
			Attachments: atts, MessageID: row.MessageID, ReplyTo: mcfg.ReplyTo}})
	}
	for _, p := range b.BCC {
		// Untracked: its own token only so that using its links isn't counted
		// as the client's view.
		tok := newRecipientToken()
		htmlOut, text := RenderEmail(b.personalView(tok, "Copy of the email sent to "+strings.Join(people, ", ")+" (BCC — this copy isn't tracked).", "", ""))
		row := base(p, "bcc")
		row.Token = &tok
		row.MessageID = mail.NewMessageID(mcfg.FromEmail)
		copies = append(copies, emailCopy{row, mail.Message{BCC: []string{p}, Subject: b.Subject, Text: text, HTML: htmlOut,
			Attachments: atts, MessageID: row.MessageID, ReplyTo: mcfg.ReplyTo}})
	}
	rows := make([]*EmailRecipient, len(copies))
	for i := range copies {
		rows[i] = copies[i].row
	}
	h.saveCopies(rows)
	entry.MessageID = copies[0].msg.MessageID

	var firstErr error
	var failed []string
	delivered := 0
	for _, cp := range copies {
		err := h.Mail.SendWith(ctx, org, mcfg, cp.msg)
		entry.Copies++
		if err != nil {
			entry.CopyFails++
			h.DB.Model(&EmailRecipient{}).Where("id = ?", cp.row.ID).Updates(map[string]interface{}{"status": "failed", "error": err.Error(), "error_code": mail.Code(err)})
			if cp.row.Role != "bcc" {
				failed = append(failed, cp.row.Email+": "+err.Error())
				if firstErr == nil {
					firstErr = err
				}
			}
			continue
		}
		h.DB.Model(&EmailRecipient{}).Where("id = ?", cp.row.ID).Updates(map[string]interface{}{"status": "sent", "sent_at": h.now()})
		if cp.row.Role != "bcc" {
			delivered++
		}
	}
	if delivered == 0 {
		return firstErr
	}
	entry.Response = fmt.Sprintf("250 accepted — %d of %d copies", entry.Copies-entry.CopyFails, entry.Copies)
	if mcfg.Host != "" {
		entry.Response += " by " + mcfg.Host
	}
	if len(failed) > 0 {
		entry.ErrorCode = "PARTIAL"
		entry.Error = fmt.Sprintf("%d of %d people didn't get their copy: %s", len(failed), len(people), strings.Join(failed, "; "))
	}
	return nil
}

func (h *Handler) saveCopies(rows []*EmailRecipient) {
	if len(rows) == 0 {
		return
	}
	if err := h.DB.Create(&rows).Error; err != nil {
		fmt.Printf("invoicing: email recipients: %v\n", err)
	}
}

// ---------------------------------------------------------------- opens

// pixelGIF is a transparent 1×1 GIF (43 bytes).
var pixelGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x21, 0xf9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00, 0x00, 0x00,
	0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44, 0x01, 0x00, 0x3b,
}

// EmailOpen serves GET /public/email-open/:file (<token>.gif): always the
// same tiny image, never cached; the open is recorded after the image is
// written. Unknown tokens get the image too (nothing to learn from it).
func (h *Handler) EmailOpen(c *gin.Context) {
	tok := strings.TrimSuffix(c.Param("file"), ".gif")
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, private, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "image/gif", pixelGIF)
	c.Writer.Flush()
	if validRecipientToken(tok) {
		h.recordOpen(tok, c.Request.UserAgent(), c.ClientIP())
	}
}

// recordOpen counts one open of a copy. Returns whether it was counted and
// whether it looked automatic.
func (h *Handler) recordOpen(tok, ua, ip string) (counted, automatic bool) {
	var r EmailRecipient
	if err := h.DB.Select("id, organization_id, tracked, open_tracking, sent_at, last_opened_at, last_user_agent").
		Where("token = ?", tok).Take(&r).Error; err != nil || !r.Tracked || !r.OpenTracking {
		return false, false
	}
	// Turned off since the email went out: stop recording opens.
	if !LoadTrackingPrefs(h.DB, r.OrganizationID).TrackOpens {
		return false, false
	}
	now := h.now()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if r.LastOpenedAt != nil && now.Sub(*r.LastOpenedAt) >= 0 && now.Sub(*r.LastOpenedAt) < openDedupe && r.LastUserAgent == ua {
		return false, false
	}
	cls := ipClass(ip)
	auto, via := classifyOpen(ua, cls, r.SentAt, now)
	upd := map[string]interface{}{
		"open_count":      gorm.Expr("open_count + 1"),
		"first_opened_at": gorm.Expr("COALESCE(first_opened_at, ?)", now),
		"last_opened_at":  now, "last_user_agent": ua, "last_ip_class": cls, "open_via": via,
	}
	if auto {
		upd["auto_open_count"] = gorm.Expr("auto_open_count + 1")
	} else {
		upd["first_human_open_at"] = gorm.Expr("COALESCE(first_human_open_at, ?)", now)
		upd["last_human_open_at"] = now
	}
	h.DB.Model(&EmailRecipient{}).Where("id = ?", r.ID).Updates(upd)
	return true, auto
}

// ipClass keeps only the network of an address: a.b.c.0/24 for IPv4, the
// /48 for IPv6 — enough to recognise a mail proxy, not to locate a person.
func ipClass(ip string) string {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil {
		return ""
	}
	if v4 := p.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.0/24", v4[0], v4[1], v4[2])
	}
	n := net.IPNet{IP: p.Mask(net.CIDRMask(48, 128)), Mask: net.CIDRMask(48, 128)}
	return n.String()
}

var scannerHints = []string{"bot", "crawler", "spider", "scanner", "curl/", "wget", "python", "go-http-client", "java/", "okhttp",
	"headless", "phantomjs", "barracuda", "mimecast", "proofpoint", "symantec", "messagelabs", "forcepoint", "trendmicro",
	"trend micro", "sophos", "ironport", "fortinet", "zscaler", "safelinks", "linkpreview", "facebookexternalhit",
	"slack", "whatsapp", "telegram", "discord", "preview", "monitor"}

// classifyOpen decides whether an open looks automatic (not a person) and
// names the client/proxy when known:
//   - Gmail and Yahoo fetch images through their own proxy when the person
//     opens the email: a real open ("gmail" / "yahoo");
//   - Apple Mail Privacy Protection preloads every image through Apple's
//     proxy (bare "Mozilla/5.0" user agent, Apple's 17.0.0.0/8 network):
//     automatic ("apple_proxy");
//   - security scanners, link previewers, scripts and an empty user agent:
//     automatic ("scanner");
//   - anything else that isn't a regular browser within ~10 s of sending:
//     automatic ("prefetch").
func classifyOpen(ua, ipCls string, sentAt *time.Time, now time.Time) (automatic bool, via string) {
	l := strings.ToLower(strings.TrimSpace(ua))
	switch {
	case strings.Contains(l, "googleimageproxy"):
		return false, "gmail"
	case strings.Contains(l, "yahoomailproxy"):
		return false, "yahoo"
	case l == "":
		return true, "scanner"
	case l == "mozilla/5.0" || strings.HasPrefix(ipCls, "17."):
		return true, "apple_proxy"
	}
	for _, s := range scannerHints {
		if strings.Contains(l, s) {
			return true, "scanner"
		}
	}
	appleMail := strings.Contains(l, "applewebkit") && !strings.Contains(l, "safari") && !strings.Contains(l, "chrome")
	browser := false
	for _, s := range []string{"chrome/", "firefox/", "safari/", "edg/", "outlook", "thunderbird", "microsoft office"} {
		if strings.Contains(l, s) {
			browser = true
			break
		}
	}
	if sentAt != nil && now.Sub(*sentAt) < automaticWindow && (appleMail || !browser) {
		return true, "prefetch"
	}
	if appleMail {
		return false, "apple_mail"
	}
	return false, ""
}

// ---------------------------------------------------------------- links

const (
	evView     = "view"
	evPayPage  = "pay_page"
	evCheckout = "checkout"
	evAccept   = "accept"
)

// linkHit records a person using their own link to inv. It returns the copy
// (nil for links without a valid token for this document) and whether this
// was that copy's first view.
func (h *Handler) linkHit(inv *Invoice, tok, event, txRef string) (*EmailRecipient, bool) {
	if !validRecipientToken(tok) || inv == nil {
		return nil, false
	}
	var r EmailRecipient
	if err := h.DB.Where("token = ? AND invoice_id = ?", tok, inv.ID).Take(&r).Error; err != nil {
		return nil, false
	}
	if !r.Tracked {
		return &r, false
	}
	now := h.now()
	upd := map[string]interface{}{}
	first := false
	switch event {
	case evView, evPayPage:
		first = r.FirstViewedAt == nil
		upd["view_count"] = gorm.Expr("view_count + 1")
		upd["first_viewed_at"] = gorm.Expr("COALESCE(first_viewed_at, ?)", now)
		upd["last_viewed_at"] = now
		if event == evPayPage {
			upd["pay_page_at"] = gorm.Expr("COALESCE(pay_page_at, ?)", now)
		}
	case evCheckout:
		ref := strings.TrimSpace(txRef)
		if ref == "" || len(ref) > 100 {
			return &r, false
		}
		refs := append([]string{}, r.CheckoutRefs...)
		for _, x := range refs {
			if x == ref {
				return &r, false
			}
		}
		refs = append(refs, ref)
		if len(refs) > maxCheckoutRefs {
			refs = refs[len(refs)-maxCheckoutRefs:]
		}
		js, _ := json.Marshal(refs)
		upd["checkout_refs"] = string(js)
	case evAccept:
		upd["accepted_at"] = gorm.Expr("COALESCE(accepted_at, ?)", now)
		upd["first_viewed_at"] = gorm.Expr("COALESCE(first_viewed_at, ?)", now)
	default:
		return &r, false
	}
	h.DB.Model(&EmailRecipient{}).Where("id = ?", r.ID).Updates(upd)
	return &r, first
}

// noteClientView records a view of inv's public page (or pay page): the
// document's first view (viewed_at, as before) and each person's first view
// through their own link. Views through a BCC copy aren't the client's.
func (h *Handler) noteClientView(inv *Invoice, tok, event string) {
	rec, first := h.linkHit(inv, tok, event, "")
	if rec != nil && !rec.Tracked {
		return
	}
	who := "client"
	if rec != nil {
		who = rec.Email
	}
	where := ""
	if event == evPayPage {
		where = " (pay page)"
	}
	if inv.ViewedAt == nil {
		t := h.now()
		if h.DB.Model(&Invoice{}).Where("id = ? AND viewed_at IS NULL", inv.ID).Update("viewed_at", t).RowsAffected > 0 {
			inv.ViewedAt = &t
			h.log(h.DB, inv, "viewed", "Viewed by "+who+where, "")
			return
		}
	}
	if first && rec != nil {
		h.log(h.DB, inv, "viewed", "Viewed by "+who+where, "")
	}
}

// PublicTrack serves POST /public/invoices/:token/track {r, event, tx_ref}:
// the pay page reports that it was opened from a recipient's link
// ("pay_page") and the checkout it started ("checkout"). Always 204.
func (h *Handler) PublicTrack(c *gin.Context) {
	raw, _ := io.ReadAll(io.LimitReader(c.Request.Body, 4096))
	var in struct {
		R     string `json:"r"`
		Event string `json:"event"`
		TxRef string `json:"tx_ref"`
	}
	_ = json.Unmarshal(raw, &in) // sendBeacon posts text/plain
	if in.R == "" {
		in.R = c.Query("r")
	}
	if inv, _, err := h.publicLoad(c.Param("token")); err == nil && validRecipientToken(in.R) {
		switch in.Event {
		case evPayPage:
			h.noteClientView(inv, in.R, evPayPage)
		case evCheckout:
			h.linkHit(inv, in.R, evCheckout, in.TxRef)
		}
	}
	c.Status(http.StatusNoContent)
}
