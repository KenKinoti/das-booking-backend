package schedsend

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"
)

// ---------------------------------------------------------------- unsubscribe

const pageCSS = `body{margin:0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;background:#f3f4f8;color:#1a1d2e}
main{max-width:460px;margin:12vh auto;padding:28px 24px;background:#fff;border:1px solid #e4e7ee;border-radius:14px}
h1{font-size:20px;margin:0 0 8px}p{color:#5b6275;line-height:1.5}button{font:inherit;font-weight:600;padding:10px 16px;border-radius:9px;border:0;background:#4f46e5;color:#fff;cursor:pointer}
@media (prefers-color-scheme:dark){body{background:#0f1117;color:#e7e9ef}main{background:#171a23;border-color:#2a2f3c}p{color:#a6adbd}}`

func page(c *gin.Context, status int, title, body string) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Robots-Tag", "noindex")
	c.String(status, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<title>`+html.EscapeString(title)+`</title><style>`+pageCSS+`</style></head><body><main><h1>`+html.EscapeString(title)+`</h1>`+body+`</main></body></html>`)
}

// unsubscribePage asks to confirm (mail scanners open GET links; the
// unsubscribe itself is the POST — RFC 8058 one-click clients POST too).
func (h *Handler) unsubscribePage(c *gin.Context) {
	org, cust, valid := h.S.readToken("unsub", c.Query("t"))
	if !valid {
		page(c, http.StatusBadRequest, "This link isn't valid", `<p>The unsubscribe link is incomplete or has been changed. Reply to the email and ask to be removed instead.</p>`)
		return
	}
	name := html.EscapeString(h.S.orgName(org))
	var o OptOut
	fresh(h.S.DB).Where("organization_id = ? AND customer_id = ?", org, cust).Limit(1).Find(&o)
	if o.CustomerID != "" {
		page(c, http.StatusOK, "You're unsubscribed", `<p>You won't get offers or marketing messages from `+name+`. You'll still get invoices, receipts and other messages about your account.</p>`)
		return
	}
	page(c, http.StatusOK, "Unsubscribe from "+h.S.orgName(org)+"?", `<p>Stop getting offers and marketing messages from `+name+
		`? You'll still get invoices, receipts and other messages about your account.</p><form method="post"><input type="hidden" name="t" value="`+
		html.EscapeString(c.Query("t"))+`"><button type="submit">Unsubscribe</button></form>`)
}

func (h *Handler) unsubscribe(c *gin.Context) {
	tok := firstNonEmpty(c.PostForm("t"), c.Query("t"))
	org, cust, valid := h.S.readToken("unsub", tok)
	if !valid {
		page(c, http.StatusBadRequest, "This link isn't valid", `<p>The unsubscribe link is incomplete or has been changed.</p>`)
		return
	}
	if h.S.Customer(org, cust) != nil {
		fresh(h.S.DB).Clauses(clause.OnConflict{DoNothing: true}).Create(&OptOut{OrganizationID: org, CustomerID: cust, Source: "link", CreatedAt: Now()})
	}
	page(c, http.StatusOK, "You're unsubscribed", `<p>You won't get offers or marketing messages from `+html.EscapeString(h.S.orgName(org))+
		` any more. You'll still get invoices, receipts and other messages about your account.</p>`)
}

// ---------------------------------------------------------------- opens

var pixel = []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0x21, 0xf9, 0x04, 0x01, 0x00,
	0x00, 0x00, 0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44, 0x01, 0x00, 0x3b}

func (h *Handler) open(c *gin.Context) {
	if org, id, valid := h.S.readToken("open", c.Param("token")); valid {
		var d Delivery
		if fresh(h.S.DB).Where("id = ? AND organization_id = ? AND channel = ?", id, org, ChanEmail).Limit(1).Find(&d).Error == nil && d.ID != "" && d.ReadAt == nil {
			// Opens within a minute of sending are usually a mail scanner.
			if d.SentAt == nil || Now().Sub(*d.SentAt) > 20*time.Second {
				h.S.applyStatus(ChanEmail, d.ProviderID, "opened", "", Now())
			}
		}
	}
	c.Header("Cache-Control", "no-store, max-age=0")
	c.Data(http.StatusOK, "image/gif", pixel)
}

// ---------------------------------------------------------------- SMS statuses

// atDelivery receives Africa's Talking delivery reports (form: id, status,
// phoneNumber, failureReason …). Ids are unguessable; only known ids change.
func (h *Handler) atDelivery(c *gin.Context) {
	id := strings.TrimSpace(c.PostForm("id"))
	switch strings.ToLower(strings.TrimSpace(c.PostForm("status"))) {
	case "success":
		h.S.applyStatus(ChanSMS, id, "delivered", "", Now())
	case "failed", "rejected":
		reason := strings.TrimSpace(c.PostForm("failureReason"))
		h.S.applyStatus(ChanSMS, id, "failed", atFailure(reason), Now())
	}
	c.Status(http.StatusOK)
}

func atFailure(r string) string {
	switch r {
	case "InsufficientCredit":
		return "Not delivered: the person's line has no credit for premium messages"
	case "InvalidLinkId", "UserIsInactive", "UserInBlackList", "UserInBlacklist":
		return "Not delivered: the person has blocked or can't receive these messages"
	case "UserAccountSuspended":
		return "Not delivered: the person's mobile account is suspended"
	case "NotNetworkSubscriber", "UserNotSubscribedToProduct":
		return "Not delivered: the number isn't on a supported network"
	case "AbsentSubscriber", "DeliveryFailure":
		return "Not delivered: the phone is off or unreachable"
	case "":
		return "Not delivered"
	}
	return "Not delivered (" + r + ")"
}

// twilioStatus receives Twilio StatusCallback posts, checking the signature
// with the auth token of the organisation that sent the message.
func (h *Handler) twilioStatus(c *gin.Context) {
	if err := c.Request.ParseForm(); err != nil {
		c.Status(http.StatusBadRequest)
		return
	}
	sid := c.PostForm("MessageSid")
	var d Delivery
	if sid == "" || fresh(h.S.DB).Where("provider_id = ? AND channel = ?", sid, ChanSMS).Limit(1).Find(&d).Error != nil || d.ID == "" {
		c.Status(http.StatusOK)
		return
	}
	tw, _ := twilioSettings(h.S.DB, d.OrganizationID)
	token := secret(tw.AuthToken)
	full := h.S.apiBase() + "/api/v1/public/sms/twilio/status"
	if token == "" || TwilioSignature(token, full, url.Values(c.Request.PostForm)) != c.GetHeader("X-Twilio-Signature") {
		c.Status(http.StatusForbidden)
		return
	}
	switch c.PostForm("MessageStatus") {
	case "delivered":
		h.S.applyStatus(ChanSMS, sid, "delivered", "", Now())
	case "read":
		h.S.applyStatus(ChanSMS, sid, "read", "", Now())
	case "undelivered", "failed":
		h.S.applyStatus(ChanSMS, sid, "failed", twilioErrorText(c.PostForm("ErrorCode")), Now())
	}
	c.Status(http.StatusOK)
}
