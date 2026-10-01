package payments

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Service holds the payment logic (handlers are thin wrappers).
type Service struct {
	DB *gorm.DB
	// NewClient builds the Flutterwave client for a secret key (tests
	// replace it).
	NewClient func(secret string) *Client
	Now       func() time.Time
}

// NewService builds a service.
func NewService(db *gorm.DB) *Service {
	return &Service{DB: db, NewClient: NewClient, Now: time.Now}
}

func init() {
	// Invoices know whether their organisation takes online payments (email
	// "Pay now" button, PDF link, public page).
	invoicing.OnlinePay = func(db *gorm.DB, org string) bool {
		s, err := LoadSettings(db, org)
		return err == nil && s.Ready()
	}
}

// ---------------------------------------------------------------- settings

// DefaultSettings are the settings of an organisation that never saved any.
func DefaultSettings(org string) Settings {
	keys := []string{}
	for _, m := range Methods {
		keys = append(keys, m.Key)
	}
	return Settings{OrganizationID: org, Provider: "flutterwave", Mode: ModeTest, Methods: keys, FeeBearer: BearerBusiness, Fees: DefaultFees()}
}

// LoadSettings returns the organisation's settings (defaults when none).
func LoadSettings(db *gorm.DB, org string) (*Settings, error) {
	var s Settings
	err := db.Where("organization_id = ?", org).Take(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		d := DefaultSettings(org)
		return &d, nil
	}
	if err != nil {
		return nil, err
	}
	if len(s.Fees) == 0 {
		s.Fees = DefaultFees()
	}
	if s.Methods == nil {
		s.Methods = []string{}
	}
	if s.FeeBearer == "" {
		s.FeeBearer = BearerBusiness
	}
	return &s, nil
}

// Ready reports whether online payments can be taken.
func (s *Settings) Ready() bool {
	return s != nil && s.Enabled && s.SecretKey != "" && s.PublicKey != "" && len(s.Methods) > 0
}

// Secret decrypts a stored secret ("" when unset or unreadable).
func Secret(stored string) string {
	v, err := mail.DecryptSecret(stored)
	if err != nil {
		return ""
	}
	return v
}

// Mask describes a secret without revealing it.
func Mask(stored string) map[string]interface{} {
	v := Secret(stored)
	if v == "" {
		return map[string]interface{}{"set": stored != "", "hint": ""}
	}
	hint := "••••"
	if len(v) > 8 {
		hint += v[len(v)-4:]
	}
	return map[string]interface{}{"set": true, "hint": hint}
}

// KeyMode guesses test/live from a Flutterwave key prefix.
func KeyMode(key string) string {
	k := strings.ToUpper(key)
	switch {
	case strings.Contains(k, "_TEST"), strings.Contains(k, "TEST-"):
		return ModeTest
	case strings.HasPrefix(k, "FLWSECK-"), strings.HasPrefix(k, "FLWPUBK-"):
		return ModeLive
	}
	return ""
}

// ---------------------------------------------------------------- checkout

// Errors shown to payers.
var (
	ErrNotEnabled = errors.New("Online payments are not available for this invoice")
	ErrNotPayable = errors.New("This invoice can't be paid online")
)

// PayError is a user-facing validation error.
type PayError struct{ Msg string }

func (e *PayError) Error() string { return e.Msg }

// Invoice loads a payable invoice by its public token.
func (s *Service) Invoice(token string) (*invoicing.Invoice, error) {
	if len(token) < 20 {
		return nil, gorm.ErrRecordNotFound
	}
	var inv invoicing.Invoice
	err := s.DB.Where("public_token = ?", token).Preload("Payments").First(&inv).Error
	if err != nil {
		return nil, err
	}
	if inv.Status == invoicing.StatusDraft {
		return nil, gorm.ErrRecordNotFound
	}
	invoicing.Derive(&inv, s.Now())
	return &inv, nil
}

// PayContext is the routing context for an invoice.
func (s *Service) PayContext(inv *invoicing.Invoice, amount float64) Context {
	p := orgprefs.Get(s.DB, inv.OrganizationID)
	cc := ""
	if inv.CustomerID != nil && *inv.CustomerID != "" && s.DB.Migrator().HasTable("customers") {
		s.DB.Raw("SELECT COALESCE(country_code,'') FROM customers WHERE id = ? AND organization_id = ?", *inv.CustomerID, inv.OrganizationID).Scan(&cc)
	}
	return Context{Amount: amount, Currency: inv.Currency, CustomerCountry: strings.ToUpper(strings.TrimSpace(cc)), OrgCountry: p.CountryCode, OrgCurrency: p.Currency}
}

// CheckAmount validates what the payer wants to pay now.
func CheckAmount(st *Settings, inv *invoicing.Invoice, amount float64) (float64, error) {
	bal := inv.BalanceDue
	if amount <= 0 {
		return bal, nil
	}
	d := currency.Decimals(inv.Currency)
	p := math.Pow10(d)
	if math.Abs(math.Round(amount*p)-amount*p) > 1e-6 {
		if d == 0 {
			return 0, &PayError{inv.Currency + " amounts can't have decimals"}
		}
		return 0, &PayError{fmt.Sprintf("Use at most %d decimal places", d)}
	}
	if amount > bal+0.4/p {
		return 0, &PayError{fmt.Sprintf("That's more than the balance due (%s)", invoicing.FormatMoney(bal, inv.Currency))}
	}
	if math.Abs(amount-bal) < 0.4/p {
		return bal, nil
	}
	if !st.AllowPartial {
		return 0, &PayError{"Please pay the full balance of " + invoicing.FormatMoney(bal, inv.Currency)}
	}
	if st.MinPartial > 0 && amount < st.MinPartial && bal > st.MinPartial {
		return 0, &PayError{"The smallest part payment is " + invoicing.FormatMoney(st.MinPartial, inv.Currency)}
	}
	return amount, nil
}

// Checkout is the result of starting a payment.
type Checkout struct {
	Link  string `json:"link"`
	TxRef string `json:"tx_ref"`
	Tx    *Transaction
}

func nonce() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start creates a Flutterwave checkout for an invoice (public pay page).
func (s *Service) Start(ctx context.Context, token string, amount float64, method, appBase, logoURL string) (*Checkout, error) {
	inv, err := s.Invoice(token)
	if err != nil {
		return nil, err
	}
	st, err := LoadSettings(s.DB, inv.OrganizationID)
	if err != nil {
		return nil, err
	}
	if !st.Ready() {
		return nil, ErrNotEnabled
	}
	if inv.DocType != invoicing.TypeInvoice || inv.Status == invoicing.StatusVoid || inv.BalanceDue <= 0 {
		return nil, ErrNotPayable
	}
	amt, err := CheckAmount(st, inv, amount)
	if err != nil {
		return nil, err
	}
	opts := Options(st, s.PayContext(inv, amt))
	var opt *Option
	for i := range opts {
		if opts[i].Method == method || (method == "" && opts[i].Recommended) {
			opt = &opts[i]
			break
		}
	}
	if opt == nil {
		return nil, &PayError{"Choose a payment method"}
	}
	m, _ := MethodByKey(opt.Method)
	if appBase == "" {
		return nil, &PayError{"The payment page address is unknown"}
	}
	biz := ""
	if s.DB.Migrator().HasTable("billing_settings") {
		s.DB.Raw("SELECT COALESCE(business_name,'') FROM billing_settings WHERE organization_id = ?", inv.OrganizationID).Scan(&biz)
	}
	if biz == "" && s.DB.Migrator().HasTable("organizations") {
		s.DB.Raw("SELECT name FROM organizations WHERE id = ?", inv.OrganizationID).Scan(&biz)
	}
	email := strings.TrimSpace(inv.ClientEmail)
	if email == "" {
		return nil, &PayError{"This invoice has no email address for the payer — ask " + biz + " to add one"}
	}
	tx := &Transaction{
		ID: uuid.NewString(), OrganizationID: inv.OrganizationID, InvoiceID: inv.ID, InvoiceNumber: inv.Number, Provider: "flutterwave",
		Mode: st.Mode, TxRef: "dasyin-" + inv.ID + "-" + nonce(), Method: m.Key, MethodLabel: m.Label, Currency: inv.Currency,
		Amount: amt, Surcharge: opt.Surcharge, ChargeAmount: opt.Total, EstimatedFee: opt.Fee, Status: TxPending,
		CustomerEmail: email, CreatedAt: s.Now(), UpdatedAt: s.Now(),
	}
	if err := s.DB.Create(tx).Error; err != nil {
		return nil, err
	}
	desc := "Invoice " + inv.Number
	if amt < inv.BalanceDue {
		desc += " (part payment)"
	}
	req := PaymentRequest{
		TxRef: tx.TxRef, Amount: tx.ChargeAmount, Currency: inv.Currency, PaymentOptions: m.Key,
		RedirectURL: appBase + "/pay/" + token + "/return",
		Customer:    Customer{Email: email, Name: inv.ClientName, PhoneNumber: inv.ClientPhone},
		Customizations: Customizations{Title: biz, Description: desc, Logo: func() string {
			if strings.HasPrefix(logoURL, "https://") {
				return logoURL
			}
			return ""
		}()},
		Meta: map[string]string{"invoice_id": inv.ID, "invoice_number": inv.Number, "organization_id": inv.OrganizationID},
	}
	link, err := s.NewClient(Secret(st.SecretKey)).CreatePayment(ctx, req)
	if err != nil {
		s.DB.Model(tx).Updates(map[string]interface{}{"status": TxFailed, "error": truncate(err.Error(), 500), "updated_at": s.Now()})
		return nil, fmt.Errorf("could not start the payment: %w", err)
	}
	return &Checkout{Link: link, TxRef: tx.TxRef, Tx: tx}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------------------------------------------------------------- verify & record

// Outcome of verifying a transaction.
type Outcome struct {
	Tx       *Transaction
	Recorded bool // this call recorded the invoice payment
	Reason   string
}

// FindTx loads a transaction by tx_ref.
func (s *Service) FindTx(txRef string) (*Transaction, error) {
	var t Transaction
	if err := s.DB.Where("tx_ref = ?", txRef).Take(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// Check compares Flutterwave's verified transaction with what we asked for.
// "" means it is a successful payment of the expected amount.
func Check(t *Transaction, v *FlwTransaction) string {
	if v == nil {
		return "Flutterwave returned no transaction"
	}
	if v.TxRef != t.TxRef {
		return "The transaction reference does not match"
	}
	if !strings.EqualFold(v.Status, "successful") {
		st := strings.ToLower(v.Status)
		if st == "" {
			st = "unknown"
		}
		return "Payment status is " + st
	}
	if !strings.EqualFold(v.Currency, t.Currency) {
		return fmt.Sprintf("Paid in %s, expected %s", strings.ToUpper(v.Currency), t.Currency)
	}
	p := math.Pow10(currency.Decimals(t.Currency))
	if v.Amount+0.4/p < t.ChargeAmount {
		return fmt.Sprintf("Paid %s, expected %s", invoicing.FormatMoney(v.Amount, t.Currency), invoicing.FormatMoney(t.ChargeAmount, t.Currency))
	}
	return ""
}

// Complete verifies a transaction with Flutterwave (by its id when known,
// else by tx_ref) and records the invoice payment exactly once. Webhook
// bodies and redirect parameters are never trusted on their own.
func (s *Service) Complete(ctx context.Context, txRef, flwID string) (*Outcome, error) {
	t, err := s.FindTx(txRef)
	if err != nil {
		return nil, err
	}
	if t.Status == TxSuccessful {
		return &Outcome{Tx: t}, nil
	}
	st, err := LoadSettings(s.DB, t.OrganizationID)
	if err != nil {
		return nil, err
	}
	cl := s.NewClient(Secret(st.SecretKey))
	var v *FlwTransaction
	if flwID != "" {
		v, err = cl.Verify(ctx, flwID)
	} else {
		v, err = cl.VerifyByRef(ctx, t.TxRef)
	}
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != 401 && ae.Status != 429 {
			// Not found / not completed yet: nothing to record.
			return &Outcome{Tx: t, Reason: ae.Message}, nil
		}
		return nil, err
	}
	reason := Check(t, v)
	if v.TxRef != t.TxRef {
		// Someone else's transaction: leave this one untouched.
		return &Outcome{Tx: t, Reason: reason}, nil
	}
	out := &Outcome{Reason: reason}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var cur Transaction
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", t.ID).Take(&cur).Error; err != nil {
			return err
		}
		out.Tx = &cur
		if cur.Status == TxSuccessful {
			out.Reason = ""
			return nil // already recorded (webhook and redirect raced)
		}
		now := s.Now()
		upd := map[string]interface{}{"updated_at": now, "flw_transaction_id": string(v.ID), "flw_ref": v.FlwRef,
			"payment_type": v.PaymentType, "charged_amount": v.ChargedAmount}
		if reason != "" {
			status := TxFailed
			if strings.EqualFold(v.Status, "pending") {
				status = TxPending
			}
			if strings.Contains(strings.ToLower(v.Status), "cancel") {
				status = TxCancelled
			}
			upd["status"], upd["error"] = status, reason
			if err := tx.Model(&cur).Updates(upd).Error; err != nil {
				return err
			}
			if status == TxFailed {
				logActivity(tx, &cur, "online_payment_failed", fmt.Sprintf("Online payment of %s failed (%s): %s", invoicing.FormatMoney(cur.ChargeAmount, cur.Currency), cur.MethodLabel, reason))
			}
			return tx.Where("id = ?", cur.ID).Take(&cur).Error
		}
		// The same Flutterwave transaction can't pay twice.
		if id := string(v.ID); id != "" {
			var n int64
			tx.Model(&Transaction{}).Where("flw_transaction_id = ? AND status = ? AND id <> ?", id, TxSuccessful, cur.ID).Count(&n)
			if n > 0 {
				upd["status"], upd["error"] = TxFailed, "This Flutterwave transaction was already recorded"
				return tx.Model(&cur).Updates(upd).Error
			}
		}
		m, _ := MethodByKey(cur.Method)
		label := "Flutterwave · " + m.Short
		if m.Short == "" {
			label = "Flutterwave"
		}
		fee := v.AppFee
		act := fmt.Sprintf("Paid online via Flutterwave (%s) · ref %s", m.Label, firstNonEmpty(v.FlwRef, cur.TxRef))
		if fee > 0 {
			act += " · processing fee " + invoicing.FormatMoney(fee, cur.Currency)
		}
		if cur.Surcharge > 0 {
			act += " · surcharge paid by client " + invoicing.FormatMoney(cur.Surcharge, cur.Currency)
		}
		p, extra, err := invoicing.RecordOnlinePayment(tx, invoicing.OnlinePaymentInput{
			InvoiceID: cur.InvoiceID, Amount: cur.Amount, Date: now, Method: label,
			Reference: firstNonEmpty(v.FlwRef, string(v.ID)), Notes: "Flutterwave tx_ref " + cur.TxRef, Activity: act,
		})
		if errors.Is(err, invoicing.ErrNothingDue) {
			extra = cur.Amount
			err = nil
			logActivity(tx, &cur, "online_payment", fmt.Sprintf("Online payment of %s received after the invoice was settled — refund it from the Flutterwave dashboard (ref %s)", invoicing.FormatMoney(cur.Amount, cur.Currency), firstNonEmpty(v.FlwRef, cur.TxRef)))
		}
		if err != nil {
			return err
		}
		upd["status"], upd["error"], upd["app_fee"], upd["fee_charged"], upd["completed_at"], upd["unapplied"] = TxSuccessful, "", fee, fee > 0, now, extra
		if p != nil {
			upd["invoice_payment_id"] = p.ID
		}
		if err := tx.Model(&cur).Updates(upd).Error; err != nil {
			return err
		}
		out.Recorded = true
		return tx.Where("id = ?", cur.ID).Take(&cur).Error
	})
	if err != nil {
		return nil, err
	}
	if OnTransaction != nil && out.Tx != nil && (out.Recorded || (out.Tx.Status == TxFailed && t.Status != TxFailed)) {
		OnTransaction(s.DB, out.Tx, out.Recorded)
	}
	if AfterRecorded != nil && out.Recorded && out.Tx != nil && out.Tx.InvoicePaymentID != "" {
		AfterRecorded(s.DB, out.Tx)
	}
	return out, nil
}

// OnTransaction, when set, is called after Complete records an online
// payment (recorded=true) or marks an attempt failed (recorded=false). It is
// the extension point for notifications; it must not block for long.
var OnTransaction func(db *gorm.DB, t *Transaction, recorded bool)

// AfterRecorded, when set, is called once after an online payment has been
// recorded on its invoice (e.g. to email the client a receipt). It runs
// after the transaction is committed; it must not block for long.
var AfterRecorded func(db *gorm.DB, t *Transaction)

// AutoReceipt reports whether org wants a receipt emailed after each online
// payment (Settings → Online payments; on by default).
func AutoReceipt(db *gorm.DB, org string) bool {
	s, err := LoadSettings(db, org)
	return err == nil && !s.NoAutoReceipt
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func logActivity(tx *gorm.DB, t *Transaction, typ, msg string) {
	tx.Create(&invoicing.Activity{ID: uuid.NewString(), InvoiceID: t.InvoiceID, OrganizationID: t.OrganizationID, Type: typ, Message: msg, CreatedAt: time.Now()})
}

// Cancel marks a pending transaction cancelled (the payer came back
// without paying). A later webhook can still complete it.
func (s *Service) Cancel(txRef string) {
	s.DB.Model(&Transaction{}).Where("tx_ref = ? AND status = ?", txRef, TxPending).
		Updates(map[string]interface{}{"status": TxCancelled, "error": "Cancelled by the payer", "updated_at": s.Now()})
}

// ---------------------------------------------------------------- webhook

// WebhookEvent is the part of a Flutterwave webhook body we read.
type WebhookEvent struct {
	Event string `json:"event"`
	Data  struct {
		ID    flexString `json:"id"`
		TxRef string     `json:"tx_ref"`
	} `json:"data"`
	// Older payloads put the fields at the top level.
	TxRef string `json:"txRef"`
}

// Ref returns the tx_ref of the event.
func (e WebhookEvent) Ref() string { return firstNonEmpty(e.Data.TxRef, e.TxRef) }

// HashOK compares the verif-hash header with the organisation's secret hash
// in constant time.
func HashOK(st *Settings, header string) bool {
	want := Secret(st.WebhookHash)
	if want == "" || header == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(header)) == 1
}

// Webhook authenticates an event and returns the transaction it is about.
// The caller then completes it (re-verifying with the API).
func (s *Service) Webhook(ev WebhookEvent, hash string) (*Transaction, error) {
	ref := ev.Ref()
	if ref == "" {
		return nil, errors.New("no tx_ref")
	}
	t, err := s.FindTx(ref)
	if err != nil {
		return nil, err
	}
	st, err := LoadSettings(s.DB, t.OrganizationID)
	if err != nil {
		return nil, err
	}
	if !HashOK(st, hash) {
		return nil, ErrBadHash
	}
	return t, nil
}

// ErrBadHash is a webhook whose verif-hash doesn't match.
var ErrBadHash = errors.New("webhook secret hash does not match")

// ProcessWebhook completes the event's transaction, logging failures.
func (s *Service) ProcessWebhook(t *Transaction, flwID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := s.Complete(ctx, t.TxRef, flwID); err != nil {
		log.Printf("payments: webhook verify %s: %v", t.TxRef, err)
	}
}
