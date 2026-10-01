package invoicing

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// ---------------------------------------------------------------- origin

// Payment origins.
const (
	OriginManual  = "manual"
	OriginDeposit = "deposit"
	OriginOnline  = "online" // paid online (Flutterwave) — mirrors the processor's record
	OriginImport  = "import" // imported from Zoho Books / a CSV file
)

// PaymentOrigin says where a payment came from.
func PaymentOrigin(p Payment) string {
	switch {
	case p.Source == "zoho" || p.Source == "csv":
		return OriginImport
	case strings.HasPrefix(p.Method, "Flutterwave") || strings.HasPrefix(p.Notes, "Flutterwave tx_ref"):
		return OriginOnline
	case p.IsDeposit:
		return OriginDeposit
	}
	return OriginManual
}

// paymentLockReason explains why the amount and date of p can't be edited
// ("" when they can).
func paymentLockReason(p Payment) string {
	switch PaymentOrigin(p) {
	case OriginOnline:
		return "This payment was made online through Flutterwave, so its amount, date and method mirror the processor's record and can't be changed here. You can still edit the reference and notes. To correct it, refund or adjust it in Flutterwave and remove the payment."
	case OriginImport:
		from := "Zoho Books"
		if p.Source == "csv" {
			from = "a CSV import"
		}
		return "This payment was imported from " + from + ", so its amount, date and method mirror that record and can't be changed here. You can still edit the reference and notes. Correct it at the source and re-import."
	}
	return ""
}

// AfterFind fills the derived origin fields.
func (p *Payment) AfterFind(tx *gorm.DB) error {
	p.Origin = PaymentOrigin(*p)
	p.LockReason = paymentLockReason(*p)
	return nil
}

// BeforeCreate gives every new payment the organisation's next receipt number.
func (p *Payment) BeforeCreate(tx *gorm.DB) error {
	if p.ReceiptNumber != "" || p.OrganizationID == "" {
		return nil
	}
	n, err := nextReceiptNumber(tx, p.OrganizationID)
	if err != nil {
		return err
	}
	p.ReceiptNumber = n
	return nil
}

// AfterCreate fills the derived origin fields of a new payment.
func (p *Payment) AfterCreate(tx *gorm.DB) error {
	p.Origin = PaymentOrigin(*p)
	p.LockReason = paymentLockReason(*p)
	return nil
}

// ---------------------------------------------------------------- numbers

// FormatReceiptNumber builds a receipt number, e.g. ("RCT-", 1, 4) → RCT-0001.
func FormatReceiptNumber(prefix string, n, pad int) string {
	if pad < 1 || pad > 10 {
		pad = 4
	}
	if n < 1 {
		n = 1
	}
	return fmt.Sprintf("%s%0*d", prefix, pad, n)
}

// nextReceiptNumber reserves the organisation's next receipt number inside
// db's transaction (the settings row is locked by the UPDATE). Numbers that
// are already used (e.g. after "next number" was lowered) are skipped.
func nextReceiptNumber(db *gorm.DB, org string) (string, error) {
	db = db.Session(&gorm.Session{NewDB: true, SkipHooks: true})
	var n int64
	db.Model(&Settings{}).Where("organization_id = ?", org).Count(&n)
	if n == 0 {
		s := defaultSettings(org)
		s.Currency = orgprefs.Currency(db, org)
		// Tax defaults follow the organisation's country (VAT 16% in Kenya …).
		s.TaxLabel, s.DefaultTaxRate, s.TaxNumberLabel = orgprefs.DefaultTax(orgprefs.Country(db, org))
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&s).Error; err != nil {
			return "", err
		}
	}
	for attempt := 0; attempt < 200; attempt++ {
		var row struct {
			Prefix string
			Num    int
			Pad    int
		}
		err := db.Raw(`UPDATE billing_settings SET next_receipt_number = GREATEST(COALESCE(next_receipt_number, 1), 1) + 1
			WHERE organization_id = ?
			RETURNING COALESCE(receipt_prefix, 'RCT-') AS prefix, next_receipt_number - 1 AS num, COALESCE(number_padding, 4) AS pad`, org).Scan(&row).Error
		if err != nil {
			return "", err
		}
		num := FormatReceiptNumber(row.Prefix, row.Num, row.Pad)
		var used int64
		db.Model(&Payment{}).Where("organization_id = ? AND receipt_number = ?", org, num).Count(&used)
		if used == 0 && db.Migrator().HasTable(&CancelledReceipt{}) {
			db.Model(&CancelledReceipt{}).Where("organization_id = ? AND receipt_number = ?", org, num).Count(&used)
		}
		if used == 0 {
			return num, nil
		}
	}
	return "", errors.New("could not find a free receipt number")
}

// BackfillReceiptNumbers numbers every payment that has no receipt number
// yet, per organisation in payment-date order. Safe to run repeatedly.
func BackfillReceiptNumbers(db *gorm.DB) int {
	var orgs []string
	db.Model(&Payment{}).Where("COALESCE(receipt_number, '') = '' AND COALESCE(organization_id, '') <> ''").Distinct().Pluck("organization_id", &orgs)
	done := 0
	for _, org := range orgs {
		_ = db.Transaction(func(tx *gorm.DB) error {
			var ids []string
			tx.Model(&Payment{}).Where("organization_id = ? AND COALESCE(receipt_number, '') = ''", org).
				Order("date ASC, created_at ASC, id ASC").Pluck("id", &ids)
			for _, id := range ids {
				num, err := nextReceiptNumber(tx, org)
				if err != nil {
					return err
				}
				if err := tx.Model(&Payment{}).Where("id = ?", id).UpdateColumn("receipt_number", num).Error; err != nil {
					return err
				}
				done++
			}
			return nil
		})
	}
	return done
}

// ReceiptFilename is the attachment / download name, e.g. "RCT-0001.pdf".
func ReceiptFilename(inv *Invoice, p Payment) string {
	n := strings.Trim(safeFileRe.ReplaceAllString(p.ReceiptNumber, "-"), "-.")
	if n == "" {
		return "Receipt-" + strings.TrimSuffix(PDFFilename(inv), ".pdf") + ".pdf"
	}
	return n + ".pdf"
}

// ---------------------------------------------------------------- figures

// receiptFigures are the invoice totals as at one payment: everything paid
// up to and including it (in date order), and what was left to pay.
type receiptFigures struct {
	Total, PaidToDate, Balance float64
}

func figuresAt(inv *Invoice, p Payment) receiptFigures {
	paid := p.Amount
	for _, x := range inv.Payments {
		if x.ID == p.ID {
			continue
		}
		xd, pd := dateOnly(x.Date), dateOnly(p.Date)
		if xd.Before(pd) || (xd.Equal(pd) && x.CreatedAt.Before(p.CreatedAt)) {
			paid += x.Amount
		}
	}
	f := receiptFigures{Total: inv.Total, PaidToDate: roundCur(paid, inv.Currency)}
	f.Balance = roundCur(inv.Total-f.PaidToDate-inv.CreditsApplied-inv.WriteOffAmount, inv.Currency)
	if math.Abs(f.Balance) < 0.4/math.Pow10(currency.Decimals(inv.Currency)) {
		f.Balance = 0
	}
	return f
}

// ---------------------------------------------------------------- cancelled

// CancelledReceipt keeps the receipt of a payment that was removed, so an
// old receipt link shows "Cancelled" instead of failing.
type CancelledReceipt struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"` // the payment id
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	InvoiceID      string    `json:"invoice_id" gorm:"type:varchar(64);index;not null"`
	ReceiptNumber  string    `json:"receipt_number" gorm:"type:varchar(40);index"`
	Snapshot       string    `json:"-" gorm:"type:text"` // the payment as JSON
	Reason         string    `json:"reason" gorm:"type:text"`
	CancelledBy    string    `json:"cancelled_by" gorm:"type:varchar(255)"`
	CancelledAt    time.Time `json:"cancelled_at"`
}

func (CancelledReceipt) TableName() string { return "billing_cancelled_receipts" }

func (h *Handler) cancelReceipt(tx *gorm.DB, p Payment, user, reason string) {
	snap, _ := json.Marshal(p)
	tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&CancelledReceipt{ID: p.ID, OrganizationID: p.OrganizationID, InvoiceID: p.InvoiceID,
		ReceiptNumber: p.ReceiptNumber, Snapshot: string(snap), Reason: reason, CancelledBy: user, CancelledAt: h.now()})
}

func (h *Handler) cancelledReceipt(inv *Invoice, pid string) (*Payment, *CancelledReceipt) {
	if !h.DB.Migrator().HasTable(&CancelledReceipt{}) {
		return nil, nil
	}
	var cr CancelledReceipt
	if err := h.DB.Where("id = ? AND invoice_id = ?", pid, inv.ID).Take(&cr).Error; err != nil {
		return nil, nil
	}
	var p Payment
	if err := json.Unmarshal([]byte(cr.Snapshot), &p); err != nil {
		return nil, nil
	}
	return &p, &cr
}

// ---------------------------------------------------------------- edit

// paymentEditInput is PUT /invoices/:id/payments/:paymentId. Fields left out
// keep their value.
type paymentEditInput struct {
	Amount    *float64 `json:"amount"`
	Date      *string  `json:"date"`
	Method    *string  `json:"method"`
	Reference *string  `json:"reference"`
	Notes     *string  `json:"notes"`
	Reason    string   `json:"reason"`
	// EmailReceipt emails the revised receipt to the client after saving.
	EmailReceipt bool   `json:"email_receipt"`
	AppURL       string `json:"app_url"`
}

// normalizeMethod maps loose spellings to a stored method.
func normalizeMethod(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	m = strings.NewReplacer(" ", "_", "-", "_").Replace(m)
	switch m {
	case "mpesa", "m_pesa", "mobile", "mobilemoney":
		return "mobile_money"
	case "bank", "transfer", "eft", "wire", "direct_debit", "bank_deposit":
		return "bank_transfer"
	case "credit_card", "debit_card", "eftpos":
		return "card"
	case "check":
		return "cheque"
	}
	return m
}

func decimalsError(cur string) error {
	d := currency.Decimals(cur)
	msg := fmt.Sprintf("%s amounts can have at most %d decimal places", cur, d)
	if d == 0 {
		msg = cur + " amounts can't have decimals"
	}
	return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", msg}
}

// editPayment applies in to payment pid of inv (loaded with its payments),
// recalculates paid / balance / status and logs what changed. It returns the
// updated payment and whether anything changed.
func (h *Handler) editPayment(tx *gorm.DB, inv *Invoice, pid string, in paymentEditInput, user string) (*Payment, bool, error) {
	idx := -1
	for i := range inv.Payments {
		if inv.Payments[i].ID == pid {
			idx = i
		}
	}
	if idx < 0 {
		return nil, false, &apiError{http.StatusNotFound, "NOT_FOUND", "Payment not found"}
	}
	if inv.Status == StatusVoid {
		return nil, false, &apiError{http.StatusConflict, "LOCKED", "Payments on a void invoice can't be changed"}
	}
	old := inv.Payments[idx]
	p := old
	what := "Payment"
	if old.IsDeposit {
		what = "Deposit"
	}
	lock := paymentLockReason(old)
	cur := inv.Currency
	var changes []string

	if in.Amount != nil {
		amt := roundCur(*in.Amount, cur)
		if amt <= 0 {
			return nil, false, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", what + " amount must be greater than zero"}
		}
		if math.Abs(amt-*in.Amount) > 1e-9 {
			return nil, false, decimalsError(cur)
		}
		if math.Abs(amt-old.Amount) > 1e-9 {
			if lock != "" {
				return nil, false, &apiError{http.StatusConflict, "PAYMENT_LOCKED", lock}
			}
			others := roundCur(inv.AmountPaid-old.Amount, cur)
			max := roundCur(inv.Total-others-inv.CreditsApplied-inv.WriteOffAmount, cur)
			if amt > max+0.4/math.Pow10(currency.Decimals(cur)) {
				return nil, false, &apiError{http.StatusBadRequest, "OVERPAYMENT", fmt.Sprintf(
					"Payments can't add up to more than the invoice total (%s). With the other payments (%s), the most this payment can be is %s.",
					FormatMoney(inv.Total, cur), FormatMoney(others, cur), FormatMoney(math.Max(max, 0), cur))}
			}
			changes = append(changes, FormatMoney(old.Amount, cur)+" → "+FormatMoney(amt, cur))
			p.Amount = amt
		}
	}
	if in.Date != nil {
		d, err := parseDate(*in.Date)
		if err != nil || d.IsZero() {
			return nil, false, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Enter a valid " + strings.ToLower(what) + " date"}
		}
		if !dateOnly(d).Equal(dateOnly(old.Date)) {
			if lock != "" {
				return nil, false, &apiError{http.StatusConflict, "PAYMENT_LOCKED", lock}
			}
			if d.After(dateOnly(h.now()).AddDate(0, 0, 1)) {
				return nil, false, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", what + " date can't be in the future"}
			}
			changes = append(changes, "date "+pdfDate(old.Date)+" → "+pdfDate(d))
			p.Date = d
		}
	}
	if in.Method != nil && strings.TrimSpace(*in.Method) != "" {
		m := normalizeMethod(*in.Method)
		if m != old.Method {
			if lock != "" {
				return nil, false, &apiError{http.StatusConflict, "PAYMENT_LOCKED", lock}
			}
			if !validMethod(m) {
				return nil, false, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Choose a payment method: " + strings.ReplaceAll(strings.Join(PaymentMethods, ", "), "_", " ")}
			}
			changes = append(changes, "method "+PaymentMethodLabel(old.Method)+" → "+PaymentMethodLabel(m))
			p.Method = m
		}
	}
	if in.Reference != nil {
		r := strings.TrimSpace(*in.Reference)
		if len(r) > 255 {
			return nil, false, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Reference is too long (255 characters max)"}
		}
		if r != old.Reference {
			switch {
			case old.Reference == "":
				changes = append(changes, "reference "+r+" added")
			case r == "":
				changes = append(changes, "reference "+old.Reference+" removed")
			default:
				changes = append(changes, "reference "+old.Reference+" → "+r)
			}
			p.Reference = r
		}
	}
	if in.Notes != nil && strings.TrimSpace(*in.Notes) != strings.TrimSpace(old.Notes) {
		if len(*in.Notes) > 4000 {
			return nil, false, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Notes are too long (4,000 characters max)"}
		}
		p.Notes = strings.TrimSpace(*in.Notes)
		changes = append(changes, "notes updated")
	}
	reason := strings.TrimSpace(in.Reason)
	if len(reason) > 500 {
		return nil, false, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "The reason is too long (500 characters max)"}
	}
	if len(changes) == 0 {
		return &old, false, nil
	}
	now := h.now()
	p.RevisedAt, p.RevisedBy, p.UpdatedAt = &now, user, &now
	if err := tx.Model(&Payment{}).Where("id = ? AND invoice_id = ?", p.ID, inv.ID).Updates(map[string]interface{}{
		"amount": p.Amount, "date": p.Date, "method": p.Method, "reference": p.Reference, "notes": p.Notes,
		"revised_at": now, "revised_by": user, "updated_at": now,
	}).Error; err != nil {
		return nil, false, err
	}
	inv.Payments[idx] = p
	ApplyPayments(inv, now)
	if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Updates(map[string]interface{}{"amount_paid": inv.AmountPaid, "balance_due": inv.BalanceDue, "status": inv.Status, "paid_at": inv.PaidAt}).Error; err != nil {
		return nil, false, err
	}
	msg := what + " edited"
	if p.ReceiptNumber != "" {
		msg += " (" + p.ReceiptNumber + ")"
	}
	msg += ": " + strings.Join(changes, "; ")
	if reason != "" {
		msg += " (" + reason + ")"
	}
	h.log(tx, inv, "payment_edited", msg, user)
	p.Origin, p.LockReason = PaymentOrigin(p), paymentLockReason(p)
	return &p, true, nil
}

// UpdatePayment serves PUT /invoices/:id/payments/:paymentId.
func (h *Handler) UpdatePayment(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in paymentEditInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	user := ctxString(c, "user_id")
	pid := c.Param("paymentId")
	var out *Invoice
	var changed bool
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		inv, err := h.lockedLoad(tx, org, c.Param("id"))
		if err != nil {
			return err
		}
		if _, changed, err = h.editPayment(tx, inv, pid, in, user); err != nil {
			return err
		}
		out, err = h.load(tx, org, inv.ID, true)
		return err
	})
	if err != nil {
		failErr(c, err)
		return
	}
	resp := gin.H{"invoice": out, "changed": changed}
	if in.EmailReceipt {
		resp["receipt_email"] = h.emailReceipt(c, org, out.ID, pid, user, in.AppURL)
		if o, err := h.load(h.DB, org, out.ID, true); err == nil {
			resp["invoice"] = o
		}
	}
	ok(c, http.StatusOK, resp)
}

// lockedLoad loads an invoice with its payments, locking the invoice row so
// concurrent payment changes are serialised.
func (h *Handler) lockedLoad(tx *gorm.DB, org, id string) (*Invoice, error) {
	var row Invoice
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("id = ? AND organization_id = ?", id, org).Take(&row).Error; err != nil {
		return nil, err
	}
	return h.load(tx, org, id, false)
}

// ---------------------------------------------------------------- email

// ReceiptEmailResult says what happened to an automatic receipt email.
type ReceiptEmailResult struct {
	Emailed bool     `json:"emailed"`
	To      string   `json:"to,omitempty"`
	CC      []string `json:"cc,omitempty"`
	BCC     []string `json:"bcc,omitempty"`
	Subject string   `json:"subject,omitempty"`
	File    string   `json:"attachment,omitempty"`
	Reason  string   `json:"reason,omitempty"` // why it wasn't sent
	Checks  []Check  `json:"checks,omitempty"`
	LogID   string   `json:"log_id,omitempty"`
}

// emailReceipt emails the receipt of payment pid to the client (recipients,
// subject and message as the send dialog would suggest). Errors found by the
// pre-send checks stop it; warnings don't. c may be nil (background sends).
func (h *Handler) emailReceipt(c *gin.Context, org, invID, pid, user, appURL string) ReceiptEmailResult {
	res, err := h.sendEmail(c, org, invID, sendInput{Kind: KindReceipt, PaymentID: pid, Mode: "email", AppURL: appURL}, user)
	if err != nil {
		out := ReceiptEmailResult{Reason: err.Error()}
		var ce *checkError
		if errors.As(err, &ce) {
			out.Checks = ce.checks
		}
		return out
	}
	return ReceiptEmailResult{Emailed: res.Emailed, To: res.Built.To, CC: res.Built.CC, BCC: res.Built.BCC, Subject: res.Built.Subject, File: res.Built.Attachment.Filename, LogID: res.Log.ID}
}

// AutoReceipt emails the receipt for an online payment (called after a
// Flutterwave payment is verified). Failures are logged on the invoice.
func (h *Handler) AutoReceipt(org, invoiceID, paymentID string) ReceiptEmailResult {
	r := h.emailReceipt(nil, org, invoiceID, paymentID, "", "")
	if !r.Emailed && r.Reason != "" {
		var inv Invoice
		if h.DB.Where("id = ? AND organization_id = ?", invoiceID, org).Take(&inv).Error == nil {
			h.log(h.DB, &inv, "receipt_failed", "Automatic receipt not emailed: "+r.Reason, "")
		}
	}
	return r
}

// receiptTemplateVars has every placeholder name (values don't matter): used
// to reject unknown placeholders when the templates are saved.
func receiptTemplateVars() map[string]string {
	v := map[string]string{"payment_reference": "x", "title": "x", "discount": "x", "shipping": "x"}
	for _, p := range Placeholders {
		v[p.Name] = "x"
	}
	return v
}

var safeFileRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
