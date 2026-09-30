package invoicing

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PayLink serves GET /invoices/:id/pay-link: the client's online payment
// page (when online payments are on and the invoice has a balance).
func (h *Handler) PayLink(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	inv, err := h.load(h.DB, org, c.Param("id"), false)
	if err != nil {
		failErr(c, err)
		return
	}
	base := h.AppBase(c, c.Query("app_url"))
	out := gin.H{"enabled": h.onlinePay(org), "payable": false, "url": "", "view_url": "", "reason": ""}
	if inv.Status != StatusDraft {
		out["view_url"] = ViewURL(base, inv.PublicToken)
	}
	switch {
	case inv.DocType != TypeInvoice:
		out["reason"] = "Quotes can't be paid online — convert the quote to an invoice first."
	case !h.onlinePay(org):
		out["reason"] = "Online payments are off. An administrator can turn on Flutterwave under Settings → Online payments."
	case inv.BalanceDue <= 0:
		out["reason"] = "This invoice has no balance to pay."
	case inv.Status == StatusVoid:
		out["reason"] = "Void invoices can't be paid."
	case inv.Status == StatusDraft:
		out["reason"] = "Send or mark the invoice as sent to activate its pay link."
	case base == "":
		out["reason"] = "The app address is unknown (set PUBLIC_APP_URL)."
	default:
		out["payable"] = true
		out["url"] = PayURL(base, inv.PublicToken)
	}
	out["number"] = inv.Number
	out["balance_due"] = inv.BalanceDue
	out["currency"] = inv.Currency
	ok(c, http.StatusOK, out)
}

// OnlinePaymentInput is a payment received through an online processor.
type OnlinePaymentInput struct {
	InvoiceID string
	Amount    float64
	Date      time.Time
	Method    string // shown as is, e.g. "Flutterwave · M-Pesa" (max 30 characters)
	Reference string
	Notes     string
	Activity  string // extra activity log line (optional)
}

// ErrNothingDue is returned when an online payment arrives for an invoice
// that no longer has a balance (the money must be refunded by hand).
var ErrNothingDue = errors.New("the invoice has no balance due")

// RecordOnlinePayment records a verified online payment on an invoice inside
// tx (the invoice row is locked). The amount is capped at the balance due;
// the recorded payment and the unapplied remainder are returned.
func RecordOnlinePayment(tx *gorm.DB, in OnlinePaymentInput) (*Payment, float64, error) {
	var inv Invoice
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", in.InvoiceID).First(&inv).Error; err != nil {
		return nil, in.Amount, err
	}
	if err := tx.Where("invoice_id = ?", inv.ID).Order("date ASC, created_at ASC").Find(&inv.Payments).Error; err != nil {
		return nil, in.Amount, err
	}
	h := &Handler{DB: tx, Now: time.Now}
	ApplyPayments(&inv, h.now())
	amt := roundCur(in.Amount, inv.Currency)
	if inv.BalanceDue <= 0 || inv.Status == StatusVoid {
		return nil, amt, ErrNothingDue
	}
	extra := 0.0
	if amt > inv.BalanceDue {
		extra = roundCur(amt-inv.BalanceDue, inv.Currency)
		amt = inv.BalanceDue
	}
	if inv.Status == StatusDraft {
		// Paid before it was sent: it is live now.
		inv.Status = StatusSent
		if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Update("status", StatusSent).Error; err != nil {
			return nil, in.Amount, err
		}
	}
	method := in.Method
	if len(method) > 30 {
		method = method[:30]
	}
	d := in.Date
	if d.IsZero() {
		d = time.Now()
	}
	if err := h.recordPayment(tx, &inv, paymentInput{Amount: amt, Date: d.Format("2006-01-02"), Method: method, Reference: in.Reference, Notes: in.Notes, keepMethod: true}, "", false); err != nil {
		return nil, in.Amount, err
	}
	p := inv.Payments[len(inv.Payments)-1]
	if strings.TrimSpace(in.Activity) != "" {
		msg := in.Activity
		if extra > 0 {
			msg += fmt.Sprintf(" · %s %s more than the balance was paid — refund it from the Flutterwave dashboard", inv.Currency, fmtAmount(extra, inv.Currency))
		}
		h.log(tx, &inv, "online_payment", msg, "")
	}
	return &p, extra, nil
}
