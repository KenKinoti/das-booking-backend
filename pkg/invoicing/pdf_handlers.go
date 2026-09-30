package invoicing

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// OnlinePay reports whether an organisation takes online payments (set by
// pkg/payments when it is wired in). nil means online payments are off.
var OnlinePay func(db *gorm.DB, org string) bool

func (h *Handler) onlinePay(org string) bool {
	return OnlinePay != nil && OnlinePay(h.DB, org)
}

// cleanBase returns the scheme://host of an absolute http(s) URL, or "".
func cleanBase(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// AppBase is the web app origin used to build client links: PUBLIC_APP_URL,
// else the app URL the browser sent (hint), else the request's Origin /
// Referer. "" when unknown.
func (h *Handler) AppBase(c *gin.Context, hint string) string {
	if v := strings.TrimRight(strings.TrimSpace(h.PublicURL), "/"); v != "" {
		return v
	}
	if b := cleanBase(hint); b != "" {
		return b
	}
	if c != nil {
		if b := cleanBase(c.GetHeader("Origin")); b != "" {
			return b
		}
		if b := cleanBase(c.GetHeader("Referer")); b != "" {
			return b
		}
	}
	return ""
}

// ViewURL is the public link of a document.
func ViewURL(base, token string) string {
	if base == "" || token == "" {
		return ""
	}
	return base + "/i/" + token
}

// PayURL is the public pay page of an invoice.
func PayURL(base, token string) string {
	if base == "" || token == "" {
		return ""
	}
	return base + "/pay/" + token
}

// payable reports whether a client can pay inv online now.
func (h *Handler) payable(inv *Invoice) bool {
	if inv.DocType != TypeInvoice || inv.BalanceDue <= 0 {
		return false
	}
	switch inv.Status {
	case StatusSent, StatusPartial, StatusDraft:
	default:
		return false
	}
	return h.onlinePay(inv.OrganizationID)
}

// DocumentPDF renders inv with the organisation's business block and links.
func (h *Handler) DocumentPDF(db *gorm.DB, inv *Invoice, base string) ([]byte, error) {
	s, err := h.settingsFor(db, inv.OrganizationID, false)
	if err != nil {
		return nil, err
	}
	b := PDFBusinessFor(db, s, inv.OrganizationID)
	opt := PDFOptions{}
	if inv.Status != StatusDraft {
		opt.ViewURL = ViewURL(base, inv.PublicToken)
	}
	if h.payable(inv) {
		opt.PayURL = PayURL(base, inv.PublicToken)
	}
	return RenderPDF(inv, b, opt)
}

func sendPDF(c *gin.Context, name string, data []byte) {
	disp := "inline"
	if c.Query("download") == "1" {
		disp = "attachment"
	}
	c.Header("Content-Disposition", disp+`; filename="`+name+`"`)
	c.Header("Cache-Control", "private, no-store")
	c.Data(http.StatusOK, "application/pdf", data)
}

// PDF serves GET /invoices/:id/pdf.
func (h *Handler) PDF(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	inv, err := h.load(h.DB, org, c.Param("id"), false)
	if err != nil {
		failErr(c, err)
		return
	}
	data, err := h.DocumentPDF(h.DB, inv, h.AppBase(c, c.Query("app_url")))
	if err != nil {
		fail(c, http.StatusInternalServerError, "PDF_FAILED", "Could not create the PDF: "+err.Error())
		return
	}
	sendPDF(c, PDFFilename(inv), data)
}

// PublicPDF serves GET /public/invoices/:token/pdf.
func (h *Handler) PublicPDF(c *gin.Context) {
	inv, _, err := h.publicLoad(c.Param("token"))
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "This link is invalid or has expired")
		return
	}
	data, err := h.DocumentPDF(h.DB, inv, h.AppBase(c, c.Query("app_url")))
	if err != nil {
		fail(c, http.StatusInternalServerError, "PDF_FAILED", "Could not create the PDF")
		return
	}
	sendPDF(c, PDFFilename(inv), data)
}

func (h *Handler) receipt(c *gin.Context, inv *Invoice) {
	pid := c.Param("paymentId")
	for _, p := range inv.Payments {
		if p.ID == pid {
			s, err := h.settingsFor(h.DB, inv.OrganizationID, false)
			if err != nil {
				failErr(c, err)
				return
			}
			data, err := RenderReceiptPDF(inv, p, PDFBusinessFor(h.DB, s, inv.OrganizationID))
			if err != nil {
				fail(c, http.StatusInternalServerError, "PDF_FAILED", "Could not create the receipt")
				return
			}
			sendPDF(c, "Receipt-"+strings.TrimSuffix(PDFFilename(inv), ".pdf")+".pdf", data)
			return
		}
	}
	fail(c, http.StatusNotFound, "NOT_FOUND", "Payment not found")
}

// ReceiptPDF serves GET /invoices/:id/payments/:paymentId/receipt.
func (h *Handler) ReceiptPDF(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	inv, err := h.load(h.DB, org, c.Param("id"), false)
	if err != nil {
		failErr(c, err)
		return
	}
	h.receipt(c, inv)
}

// PublicReceiptPDF serves GET /public/invoices/:token/receipts/:paymentId.
func (h *Handler) PublicReceiptPDF(c *gin.Context) {
	inv, _, err := h.publicLoad(c.Param("token"))
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "This link is invalid or has expired")
		return
	}
	h.receipt(c, inv)
}
