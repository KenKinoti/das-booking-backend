package payments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Handler serves the payments API.
type Handler struct {
	Svc       *Service
	PublicURL string // web app origin (PUBLIC_APP_URL)
	// Async processes webhooks after answering 200 (tests turn it off).
	Async bool
	// LogoURL resolves the organisation logo (absolute) for the checkout.
	LogoURL func(r *http.Request, org string) string
}

// NewHandler builds a handler.
func NewHandler(db *gorm.DB) *Handler {
	return &Handler{Svc: NewService(db), Async: true}
}

// RegisterRoutes mounts the authenticated API.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	auth.GET("/payments/settings", h.GetSettings)
	auth.PUT("/payments/settings", h.UpdateSettings)
	auth.POST("/payments/settings/test", h.Test)
	auth.GET("/payments/fee-defaults", h.FeeDefaults)
	auth.POST("/payments/estimate", h.Estimate)
	auth.GET("/payments/transactions", h.Transactions)
	auth.GET("/invoicing/invoices/:id/online-payments", h.InvoiceTransactions)
	auth.GET("/super-admin/payments/status", h.PlatformStatus)
}

// RegisterPublic mounts the pay page API and the webhook.
func (h *Handler) RegisterPublic(public *gin.RouterGroup) {
	public.GET("/pay/:token", h.PayPage)
	public.POST("/pay/:token/checkout", h.Checkout)
	public.POST("/pay/:token/verify", h.Verify)
	public.POST("/payments/flutterwave/webhook", h.Webhook)
}

func ok(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func canEdit(role string) bool {
	switch role {
	case "super_admin", "admin", "owner", "org_admin":
		return true
	}
	return false
}

func (h *Handler) org(c *gin.Context) (string, bool) {
	org := c.GetString("org_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "No organisation in the session")
		return "", false
	}
	return org, true
}

// webhookURL is the address to paste into the Flutterwave dashboard.
func webhookURL(r *http.Request) string {
	return branding.BaseURL(r) + "/api/v1/public/payments/flutterwave/webhook"
}

func (h *Handler) view(c *gin.Context, s *Settings) gin.H {
	methods := []gin.H{}
	for _, m := range Methods {
		methods = append(methods, gin.H{"key": m.Key, "label": m.Label, "icon": m.Icon, "currencies": m.Currencies})
	}
	fees := s.Fees
	if len(fees) == 0 {
		fees = DefaultFees()
	}
	return gin.H{
		"enabled": s.Enabled, "provider": "flutterwave", "mode": s.Mode, "public_key": s.PublicKey,
		"secret_key": Mask(s.SecretKey), "encryption_key": Mask(s.EncryptionKey), "webhook_hash": Mask(s.WebhookHash),
		"methods": s.Methods, "fee_bearer": s.FeeBearer, "allow_partial": s.AllowPartial, "min_partial": s.MinPartial,
		"fees": fees, "fee_tax_percent": s.FeeTaxPercent, "ready": s.Ready(),
		"last_test_at": s.LastTestAt, "last_test_ok": s.LastTestOK, "last_test_message": s.LastTestMessage,
		"updated_at":  s.UpdatedAt,
		"webhook_url": webhookURL(c.Request), "api_base_url": BaseURL(),
		"available_methods": methods, "pricing_source": PricingSource, "pricing_checked_on": PricingCheckedOn,
		"can_edit": canEdit(c.GetString("user_role")),
	}
}

// GetSettings serves GET /payments/settings.
func (h *Handler) GetSettings(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	s, err := LoadSettings(h.Svc.DB, org)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	ok(c, h.view(c, s))
}

type settingsInput struct {
	Enabled       *bool      `json:"enabled"`
	Mode          *string    `json:"mode"`
	PublicKey     *string    `json:"public_key"`
	SecretKey     *string    `json:"secret_key"` // "" keeps, "-" clears
	EncryptionKey *string    `json:"encryption_key"`
	WebhookHash   *string    `json:"webhook_hash"`
	Methods       *[]string  `json:"methods"`
	FeeBearer     *string    `json:"fee_bearer"`
	AllowPartial  *bool      `json:"allow_partial"`
	MinPartial    *float64   `json:"min_partial"`
	Fees          *[]FeeRule `json:"fees"`
	FeeTaxPercent *float64   `json:"fee_tax_percent"`
	ResetFees     bool       `json:"reset_fees"`
}

func setSecret(dst *string, in *string) error {
	if in == nil {
		return nil
	}
	v := strings.TrimSpace(*in)
	switch v {
	case "":
		return nil
	case "-":
		*dst = ""
		return nil
	}
	enc, err := mail.EncryptSecret(v)
	if err != nil {
		return err
	}
	*dst = enc
	return nil
}

// apply validates in and applies it to s; returns a field → message map.
func (in settingsInput) apply(s *Settings) (map[string]string, error) {
	errs := map[string]string{}
	if in.Mode != nil {
		m := strings.ToLower(strings.TrimSpace(*in.Mode))
		if m != ModeTest && m != ModeLive {
			errs["mode"] = "Choose test or live mode"
		} else {
			s.Mode = m
		}
	}
	if in.PublicKey != nil {
		s.PublicKey = strings.TrimSpace(*in.PublicKey)
	}
	for _, x := range []struct {
		dst *string
		in  *string
	}{{&s.SecretKey, in.SecretKey}, {&s.EncryptionKey, in.EncryptionKey}, {&s.WebhookHash, in.WebhookHash}} {
		if err := setSecret(x.dst, x.in); err != nil {
			return nil, err
		}
	}
	if in.Methods != nil {
		keep := []string{}
		seen := map[string]bool{}
		for _, k := range *in.Methods {
			if _, ok := MethodByKey(k); ok && !seen[k] {
				keep = append(keep, k)
				seen[k] = true
			}
		}
		s.Methods = keep
	}
	if in.FeeBearer != nil {
		switch *in.FeeBearer {
		case BearerBusiness, BearerCustomer:
			s.FeeBearer = *in.FeeBearer
		default:
			errs["fee_bearer"] = "Choose who pays the processing fee"
		}
	}
	if in.AllowPartial != nil {
		s.AllowPartial = *in.AllowPartial
	}
	if in.MinPartial != nil {
		if *in.MinPartial < 0 {
			errs["min_partial"] = "The minimum part payment can't be negative"
		} else {
			s.MinPartial = *in.MinPartial
		}
	}
	if in.FeeTaxPercent != nil {
		if *in.FeeTaxPercent < 0 || *in.FeeTaxPercent > 50 {
			errs["fee_tax_percent"] = "Tax on fees must be between 0 and 50%"
		} else {
			s.FeeTaxPercent = *in.FeeTaxPercent
		}
	}
	if in.ResetFees {
		s.Fees = DefaultFees()
	} else if in.Fees != nil {
		rows := []FeeRule{}
		for i, r := range *in.Fees {
			r.Method = strings.TrimSpace(r.Method)
			r.Currency = strings.ToUpper(strings.TrimSpace(r.Currency))
			if _, ok := MethodByKey(r.Method); !ok {
				errs["fees"] = "Row " + itoa(i+1) + ": unknown payment method"
				continue
			}
			if r.Currency != "*" && len(r.Currency) != 3 {
				errs["fees"] = "Row " + itoa(i+1) + ": use a 3-letter currency code or *"
				continue
			}
			if r.Scope != ScopeAny && r.Scope != ScopeLocal && r.Scope != ScopeInternational {
				r.Scope = ScopeAny
			}
			if r.Method != "card" {
				r.Scope = ScopeAny
			}
			if r.Percent < 0 || r.Percent > 20 || r.Fixed < 0 || r.Cap < 0 {
				errs["fees"] = "Row " + itoa(i+1) + ": the percentage must be 0–20 and amounts can't be negative"
				continue
			}
			rows = append(rows, r)
		}
		if len(rows) > 200 {
			errs["fees"] = "Too many fee rows (200 max)"
		}
		s.Fees = rows
	}
	if in.Enabled != nil {
		s.Enabled = *in.Enabled
	}
	if s.Enabled {
		switch {
		case s.PublicKey == "":
			errs["public_key"] = "Enter the Flutterwave public key"
		case s.SecretKey == "":
			errs["secret_key"] = "Enter the Flutterwave secret key"
		case len(s.Methods) == 0:
			errs["methods"] = "Allow at least one payment method"
		}
		if s.WebhookHash == "" {
			errs["webhook_hash"] = "Enter the webhook secret hash (the same value as in the Flutterwave dashboard)"
		}
		if km := KeyMode(Secret(s.SecretKey)); km != "" && km != s.Mode {
			errs["mode"] = "The secret key is a " + km + " key but the mode is " + s.Mode
		}
	}
	return errs, nil
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func firstErr(errs map[string]string) string {
	keys := []string{}
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return errs[keys[0]]
}

// UpdateSettings serves PUT /payments/settings (administrators).
func (h *Handler) UpdateSettings(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	if !canEdit(c.GetString("user_role")) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only administrators can change the payment settings")
		return
	}
	var in settingsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	s, err := LoadSettings(h.Svc.DB, org)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	errs, err := in.apply(s)
	if err != nil {
		fail(c, http.StatusInternalServerError, "ENCRYPTION_FAILED", "Could not store the keys securely: "+err.Error())
		return
	}
	if len(errs) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"code": "VALIDATION_ERROR", "message": firstErr(errs), "fields": errs}})
		return
	}
	s.OrganizationID = org
	s.UpdatedAt = time.Now()
	s.UpdatedBy = c.GetString("user_id")
	if err := h.Svc.DB.Save(s).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	ok(c, h.view(c, s))
}

// Test serves POST /payments/settings/test: an authenticated, harmless
// Flutterwave call (GET /v3/balances) with the saved or typed secret key.
func (h *Handler) Test(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	if !canEdit(c.GetString("user_role")) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only administrators can test the payment settings")
		return
	}
	var in struct {
		SecretKey string `json:"secret_key"`
	}
	_ = c.ShouldBindJSON(&in)
	s, err := LoadSettings(h.Svc.DB, org)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	key := strings.TrimSpace(in.SecretKey)
	if key == "" {
		key = Secret(s.SecretKey)
	}
	if key == "" {
		fail(c, http.StatusBadRequest, "NO_KEY", "Enter the Flutterwave secret key first")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	bal, err := h.Svc.NewClient(key).Balances(ctx)
	now := time.Now()
	msg := ""
	if err != nil {
		msg = err.Error()
	} else {
		cur := []string{}
		for _, b := range bal {
			cur = append(cur, b.Currency)
		}
		msg = "Connected to Flutterwave"
		if len(cur) > 0 {
			msg += " · wallets: " + strings.Join(cur, ", ")
		}
	}
	if strings.TrimSpace(in.SecretKey) == "" {
		h.Svc.DB.Model(&Settings{}).Where("organization_id = ?", org).Updates(map[string]interface{}{"last_test_at": now, "last_test_ok": err == nil, "last_test_message": msg})
	}
	if err != nil {
		fail(c, http.StatusBadGateway, "FLUTTERWAVE_ERROR", msg)
		return
	}
	ok(c, gin.H{"ok": true, "message": msg, "mode": KeyMode(key), "ms": time.Since(start).Milliseconds(), "tested_at": now})
}

// FeeDefaults serves GET /payments/fee-defaults.
func (h *Handler) FeeDefaults(c *gin.Context) {
	ok(c, gin.H{"fees": DefaultFees(), "source": PricingSource, "checked_on": PricingCheckedOn})
}

// Estimate serves POST /payments/estimate: least-cost options for an amount.
func (h *Handler) Estimate(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in struct {
		Amount          float64    `json:"amount"`
		Currency        string     `json:"currency"`
		CustomerCountry string     `json:"customer_country"`
		Draft           *Settings  `json:"draft"`
		Fees            *[]FeeRule `json:"fees"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	s, err := LoadSettings(h.Svc.DB, org)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if in.Draft != nil {
		s.Methods, s.FeeBearer, s.FeeTaxPercent = in.Draft.Methods, in.Draft.FeeBearer, in.Draft.FeeTaxPercent
		if len(in.Draft.Fees) > 0 {
			s.Fees = in.Draft.Fees
		}
	}
	cur := strings.ToUpper(in.Currency)
	if !currency.IsSupported(cur) {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Unsupported currency")
		return
	}
	ctx := Context{Amount: in.Amount, Currency: cur, CustomerCountry: strings.ToUpper(in.CustomerCountry)}
	p := h.Svc.PayContext(&invoicing.Invoice{OrganizationID: org, Currency: cur}, in.Amount)
	ctx.OrgCountry, ctx.OrgCurrency = p.OrgCountry, p.OrgCurrency
	ok(c, gin.H{"options": Options(s, ctx), "card_scope": CardScope(cur, ctx.CustomerCountry, ctx.OrgCountry, ctx.OrgCurrency)})
}

// Transactions serves GET /payments/transactions (latest first).
func (h *Handler) Transactions(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	q := h.Svc.DB.Where("organization_id = ?", org)
	if id := c.Query("invoice_id"); id != "" {
		q = q.Where("invoice_id = ?", id)
	}
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	list := []Transaction{}
	q.Order("created_at DESC").Limit(200).Find(&list)
	ok(c, gin.H{"transactions": list})
}

// InvoiceTransactions serves GET /invoicing/invoices/:id/online-payments.
func (h *Handler) InvoiceTransactions(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var n int64
	h.Svc.DB.Model(&invoicing.Invoice{}).Where("id = ? AND organization_id = ?", c.Param("id"), org).Count(&n)
	if n == 0 {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Invoice not found")
		return
	}
	list := []Transaction{}
	h.Svc.DB.Where("organization_id = ? AND invoice_id = ?", org, c.Param("id")).Order("created_at DESC").Limit(100).Find(&list)
	s, _ := LoadSettings(h.Svc.DB, org)
	ok(c, gin.H{"transactions": list, "enabled": s != nil && s.Ready(), "mode": s.Mode, "fee_bearer": s.FeeBearer})
}

// PlatformStatus serves GET /super-admin/payments/status: which
// organisations take online payments.
func (h *Handler) PlatformStatus(c *gin.Context) {
	if c.GetString("user_role") != "super_admin" {
		fail(c, http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Only a platform administrator can see this")
		return
	}
	var rows []Settings
	h.Svc.DB.Order("updated_at DESC").Limit(500).Find(&rows)
	type row struct {
		OrganizationID string     `json:"organization_id"`
		Name           string     `json:"name"`
		Enabled        bool       `json:"enabled"`
		Ready          bool       `json:"ready"`
		Mode           string     `json:"mode"`
		Methods        int        `json:"methods"`
		LastTestAt     *time.Time `json:"last_test_at"`
		LastTestOK     bool       `json:"last_test_ok"`
		Successful     int64      `json:"successful_payments"`
		UpdatedAt      time.Time  `json:"updated_at"`
	}
	out := []row{}
	for _, s := range rows {
		r := row{OrganizationID: s.OrganizationID, Enabled: s.Enabled, Ready: s.Ready(), Mode: s.Mode, Methods: len(s.Methods), LastTestAt: s.LastTestAt, LastTestOK: s.LastTestOK, UpdatedAt: s.UpdatedAt}
		if h.Svc.DB.Migrator().HasTable("organizations") {
			h.Svc.DB.Raw("SELECT name FROM organizations WHERE id = ?", s.OrganizationID).Scan(&r.Name)
		}
		h.Svc.DB.Model(&Transaction{}).Where("organization_id = ? AND status = ?", s.OrganizationID, TxSuccessful).Count(&r.Successful)
		out = append(out, r)
	}
	ok(c, gin.H{"organizations": out, "api_base_url": BaseURL(), "webhook_url": webhookURL(c.Request)})
}

// ---------------------------------------------------------------- public

func (h *Handler) appBase(c *gin.Context, hint string) string {
	ih := &invoicing.Handler{PublicURL: h.PublicURL}
	return ih.AppBase(c, hint)
}

func (h *Handler) logo(c *gin.Context, org string) string {
	if h.LogoURL != nil {
		return h.LogoURL(c.Request, org)
	}
	if invoicing.LogoURL != nil {
		return invoicing.LogoURL(c.Request, org)
	}
	return ""
}

func (h *Handler) business(org string) string {
	db := h.Svc.DB
	name := ""
	if db.Migrator().HasTable("billing_settings") {
		db.Raw("SELECT COALESCE(business_name,'') FROM billing_settings WHERE organization_id = ?", org).Scan(&name)
	}
	if name == "" && db.Migrator().HasTable("organizations") {
		db.Raw("SELECT name FROM organizations WHERE id = ?", org).Scan(&name)
	}
	return name
}

func notFound(c *gin.Context) {
	fail(c, http.StatusNotFound, "NOT_FOUND", "This payment link is invalid or has been replaced. Ask the sender for a new one.")
}

// PayPage serves GET /public/pay/:token: the invoice summary and the ways to
// pay (cheapest first). ?amount= recomputes the fees for a part payment.
func (h *Handler) PayPage(c *gin.Context) {
	inv, err := h.Svc.Invoice(c.Param("token"))
	if err != nil {
		notFound(c)
		return
	}
	s, err := LoadSettings(h.Svc.DB, inv.OrganizationID)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", "Could not load the payment page")
		return
	}
	payable := s.Ready() && inv.DocType == invoicing.TypeInvoice && inv.Status != invoicing.StatusVoid && inv.BalanceDue > 0
	amount := inv.BalanceDue
	amtErr := ""
	if v := c.Query("amount"); v != "" && payable {
		var f float64
		if err := json.Unmarshal([]byte(v), &f); err == nil {
			if a, err := CheckAmount(s, inv, f); err != nil {
				amtErr = err.Error()
			} else {
				amount = a
			}
		}
	}
	options := []Option{}
	if payable {
		options = Options(s, h.Svc.PayContext(inv, amount))
	}
	reason := ""
	switch {
	case inv.DocType != invoicing.TypeInvoice:
		reason = "Quotes can't be paid online."
	case inv.Status == invoicing.StatusVoid:
		reason = "This invoice was cancelled."
	case inv.BalanceDue <= 0:
		reason = "This invoice is fully paid. Thank you!"
	case !s.Ready():
		reason = "Online payment isn't available for this invoice. Please use the payment details on the invoice."
	}
	pays := []gin.H{}
	for _, p := range inv.Payments {
		pays = append(pays, gin.H{"id": p.ID, "amount": p.Amount, "date": p.Date, "method": p.Method})
	}
	ok(c, gin.H{
		"invoice": gin.H{"number": inv.Number, "client_name": inv.ClientName, "currency": inv.Currency, "total": inv.Total,
			"amount_paid": inv.AmountPaid, "balance_due": inv.BalanceDue, "due_date": inv.DueDate, "issue_date": inv.IssueDate,
			"status": inv.Status, "display_status": inv.DisplayStatus, "title": inv.Title, "payments": pays},
		"business":          gin.H{"name": h.business(inv.OrganizationID), "logo_url": h.logo(c, inv.OrganizationID)},
		"payable":           payable,
		"reason":            reason,
		"amount":            amount,
		"amount_error":      amtErr,
		"options":           options,
		"settings":          gin.H{"allow_partial": s.AllowPartial, "min_partial": s.MinPartial, "fee_bearer": s.FeeBearer, "mode": s.Mode, "provider": "flutterwave"},
		"currency_decimals": currency.Decimals(inv.Currency),
	})
}

// Checkout serves POST /public/pay/:token/checkout {amount, method, app_url}.
func (h *Handler) Checkout(c *gin.Context) {
	var in struct {
		Amount float64 `json:"amount"`
		Method string  `json:"method"`
		AppURL string  `json:"app_url"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	inv, err := h.Svc.Invoice(c.Param("token"))
	if err != nil {
		notFound(c)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	co, err := h.Svc.Start(ctx, c.Param("token"), in.Amount, in.Method, h.appBase(c, in.AppURL), h.logo(c, inv.OrganizationID))
	if err != nil {
		var pe *PayError
		switch {
		case errors.As(err, &pe):
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", pe.Msg)
		case errors.Is(err, ErrNotEnabled), errors.Is(err, ErrNotPayable):
			fail(c, http.StatusConflict, "NOT_PAYABLE", err.Error())
		case errors.Is(err, gorm.ErrRecordNotFound):
			notFound(c)
		default:
			fail(c, http.StatusBadGateway, "CHECKOUT_FAILED", "Could not start the payment. Please try again in a moment.")
		}
		return
	}
	ok(c, gin.H{"link": co.Link, "tx_ref": co.TxRef, "amount": co.Tx.ChargeAmount, "method": co.Tx.Method})
}

// Verify serves POST /public/pay/:token/verify {tx_ref, transaction_id,
// status} after Flutterwave redirects the payer back. The payment is
// verified with Flutterwave; the redirect parameters are only hints.
func (h *Handler) Verify(c *gin.Context) {
	var in struct {
		TxRef         string `json:"tx_ref"`
		TransactionID string `json:"transaction_id"`
		Status        string `json:"status"`
	}
	_ = c.ShouldBindJSON(&in)
	inv, err := h.Svc.Invoice(c.Param("token"))
	if err != nil {
		notFound(c)
		return
	}
	t, err := h.Svc.FindTx(in.TxRef)
	if err != nil || t.InvoiceID != inv.ID {
		fail(c, http.StatusNotFound, "NOT_FOUND", "We couldn't find this payment")
		return
	}
	status := strings.ToLower(in.Status)
	if (status == "cancelled" || status == "canceled") && in.TransactionID == "" {
		h.Svc.Cancel(t.TxRef)
		t, _ = h.Svc.FindTx(t.TxRef)
	} else {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
		defer cancel()
		out, err := h.Svc.Complete(ctx, t.TxRef, in.TransactionID)
		if err != nil {
			fail(c, http.StatusBadGateway, "VERIFY_FAILED", "We couldn't confirm the payment with Flutterwave yet. If money left your account it will be recorded automatically — refresh in a minute.")
			return
		}
		t = out.Tx
	}
	inv, _ = h.Svc.Invoice(c.Param("token"))
	receipt := ""
	if t.InvoicePaymentID != "" {
		receipt = "/api/v1/public/invoices/" + c.Param("token") + "/receipts/" + t.InvoicePaymentID
	}
	ok(c, gin.H{
		"status": t.Status, "reason": t.Error, "amount": t.ChargeAmount, "applied": t.Amount, "surcharge": t.Surcharge,
		"currency": t.Currency, "method": t.MethodLabel, "reference": firstNonEmpty(t.FlwRef, t.TxRef), "tx_ref": t.TxRef,
		"receipt_path": receipt, "invoice_number": inv.Number, "balance_due": inv.BalanceDue, "invoice_status": inv.Status,
		"mode": t.Mode,
	})
}

// Webhook serves POST /public/payments/flutterwave/webhook. It checks the
// verif-hash header against the organisation's secret hash, answers 200
// straight away and re-verifies the transaction with the API before
// recording anything (the body is never trusted).
func (h *Handler) Webhook(c *gin.Context) {
	raw, _ := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	var ev WebhookEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		fail(c, http.StatusBadRequest, "BAD_PAYLOAD", "Invalid JSON")
		return
	}
	t, err := h.Svc.Webhook(ev, c.GetHeader("verif-hash"))
	switch {
	case errors.Is(err, ErrBadHash):
		fail(c, http.StatusUnauthorized, "BAD_SIGNATURE", "verif-hash does not match")
		return
	case err != nil:
		// Not one of ours (another integration on the same account): ack.
		c.JSON(http.StatusOK, gin.H{"success": true, "ignored": true})
		return
	}
	id := string(ev.Data.ID)
	if h.Async {
		go h.Svc.ProcessWebhook(t, id)
	} else {
		h.Svc.ProcessWebhook(t, id)
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}
