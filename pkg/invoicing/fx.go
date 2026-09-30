package invoicing

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// FX quotes exchange rates (set by cmd/production; nil falls back to the
// built-in reference table plus the organisation's manual rates).
var FX *fxrates.Service

// LogoSize returns the organisation logo's pixel size (0, 0 when unknown).
var LogoSize func(org string) (int, int)

func (h *Handler) fx() *fxrates.Service {
	if FX != nil {
		return FX
	}
	return &fxrates.Service{DB: h.DB, Now: h.now}
}

// Currency conversion rules
//
//   - Catalog prices (services, products) are in the organisation's home
//     currency (pkg/orgprefs). A document in another currency converts them at
//     the document's exchange rate: unit price = round(original × rate) to the
//     document currency's minor units; the original price and currency are kept
//     on the line.
//   - The rate (units of document currency per 1 home currency) is LOCKED when
//     the document is created. It changes only when the user asks: "Update
//     rate" on a draft (re-fetched, lines with an original price re-converted)
//     or a rate typed in by hand. Sent documents keep their rate.
//   - Documents in the home currency have rate 1 (source "same").
//   - TotalBase = round(Total ÷ rate) in the home currency, for reporting.
//   - A foreign-currency document without a rate can't be emailed (pre-send
//     check); the user locks one first.

// rateInput is what the editor sends about the exchange rate.
type rateInput struct {
	ExchangeRate *float64 `json:"exchange_rate"`
	RateSource   string   `json:"rate_source"`
	RateDate     string   `json:"rate_date"`
	UpdateRate   bool     `json:"update_rate"`
}

func validRateSource(s string) bool {
	switch s {
	case fxrates.SourceLive, fxrates.SourceFallback, fxrates.SourceManual:
		return true
	}
	return false
}

// setQuote stores q on inv.
func setQuote(inv *Invoice, q fxrates.Quote, now time.Time) {
	if !q.OK() {
		inv.ExchangeRate, inv.RateSource, inv.RateDate, inv.RateProvider = 0, fxrates.SourceMissing, nil, ""
		return
	}
	inv.ExchangeRate = q.Rate
	inv.RateSource = q.Source
	t := now
	if q.AsOf != nil {
		t = *q.AsOf
	}
	t = t.UTC()
	inv.RateDate = &t
	inv.RateProvider = q.Provider
}

// lockRate decides the exchange rate of inv after an edit. prevCurrency /
// prevBase are the stored values ("" for a new document). It reports whether
// the rate changed (so converted lines must be re-priced).
func (h *Handler) lockRate(tx *gorm.DB, inv *Invoice, in rateInput, prevCurrency, prevBase string) (bool, error) {
	base := inv.BaseCurrency
	if base == "" || prevCurrency == "" {
		base = orgprefs.Currency(tx, inv.OrganizationID)
	}
	inv.BaseCurrency = strings.ToUpper(base)
	now := h.now()
	if inv.Currency == inv.BaseCurrency {
		changed := inv.ExchangeRate != 1
		inv.ExchangeRate, inv.RateSource, inv.RateProvider = 1, fxrates.SourceSame, "Same currency"
		if inv.RateDate == nil || changed {
			t := now.UTC()
			inv.RateDate = &t
		}
		return changed, nil
	}
	currencyChanged := prevCurrency != "" && (prevCurrency != inv.Currency || prevBase != inv.BaseCurrency)
	isNew := prevCurrency == ""
	locked := inv.ExchangeRate > 0 && !currencyChanged && !isNew
	if in.UpdateRate {
		if !isNew && inv.Status != StatusDraft && locked {
			return false, &apiError{http.StatusConflict, "RATE_LOCKED", "The exchange rate is locked once a document has been sent"}
		}
		q := h.fx().Quote(inv.OrganizationID, inv.BaseCurrency, inv.Currency)
		old := inv.ExchangeRate
		setQuote(inv, q, now)
		return inv.ExchangeRate != old, nil
	}
	if in.ExchangeRate != nil && *in.ExchangeRate > 0 {
		r := fxrates.SigRound(*in.ExchangeRate)
		if r > 1e9 {
			return false, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "The exchange rate is not valid"}
		}
		// A sent document keeps its rate (the client sends it back unchanged).
		if locked && inv.Status != StatusDraft && math.Abs(r-inv.ExchangeRate) > 1e-12 {
			return false, nil
		}
		if locked && math.Abs(r-inv.ExchangeRate) <= 1e-12*math.Max(1, r) {
			return false, nil
		}
		src := strings.ToLower(strings.TrimSpace(in.RateSource))
		if !validRateSource(src) {
			src = fxrates.SourceManual
		}
		old := inv.ExchangeRate
		inv.ExchangeRate, inv.RateSource = r, src
		t := now.UTC()
		if d, err := time.Parse(time.RFC3339, strings.TrimSpace(in.RateDate)); err == nil && src != fxrates.SourceManual {
			t = d.UTC()
		}
		inv.RateDate = &t
		switch src {
		case fxrates.SourceManual:
			inv.RateProvider = "Entered by hand"
		case fxrates.SourceLive:
			inv.RateProvider = "Market rates (ECB / ExchangeRate-API)"
		default:
			inv.RateProvider = "Reference table (live rates unavailable)"
		}
		return old != inv.ExchangeRate, nil
	}
	if locked {
		return false, nil
	}
	setQuote(inv, h.fx().Quote(inv.OrganizationID, inv.BaseCurrency, inv.Currency), now)
	return true, nil
}

// reconvertLines re-prices lines that came from the catalog at inv's rate.
// Lines whose original currency is the document currency get the original
// price back ("Keep KES").
func reconvertLines(inv *Invoice) {
	for i := range inv.Items {
		it := &inv.Items[i]
		if it.OriginalUnitPrice == nil {
			continue
		}
		oc := strings.ToUpper(it.OriginalCurrency)
		switch {
		case oc == inv.Currency || oc == "":
			it.UnitPrice = *it.OriginalUnitPrice
			it.OriginalCurrency = inv.Currency
		case oc == inv.BaseCurrency && inv.ExchangeRate > 0:
			it.UnitPrice = fxrates.Convert(*it.OriginalUnitPrice, inv.ExchangeRate, inv.Currency)
		}
	}
}

// setBaseTotal fills TotalBase from Total and the locked rate.
func setBaseTotal(inv *Invoice) {
	switch {
	case inv.BaseCurrency == "" || inv.Currency == inv.BaseCurrency:
		inv.TotalBase = inv.Total
	case inv.ExchangeRate > 0:
		inv.TotalBase = roundCur(inv.Total/inv.ExchangeRate, inv.BaseCurrency)
	default:
		inv.TotalBase = 0
	}
}

// ToBase converts an amount in inv's currency to the home currency at the
// locked rate (ok false when there is no rate).
func ToBase(inv *Invoice, v float64) (float64, bool) {
	if inv.BaseCurrency == "" || inv.Currency == inv.BaseCurrency {
		return v, true
	}
	if inv.ExchangeRate <= 0 {
		return 0, false
	}
	return roundCur(v/inv.ExchangeRate, inv.BaseCurrency), true
}

// BackfillBase records the home currency on documents created before rates
// were locked; documents in it get rate 1. Foreign-currency ones keep rate 0
// ("missing") until the user locks one. Returns the number of rows updated.
func BackfillBase(db *gorm.DB) int {
	if !db.Migrator().HasTable(&Invoice{}) {
		return 0
	}
	var orgs []string
	db.Model(&Invoice{}).Where("base_currency IS NULL OR base_currency = ''").Distinct().Pluck("organization_id", &orgs)
	n := 0
	for _, org := range orgs {
		base := orgprefs.Currency(db, org)
		res := db.Exec(`UPDATE billing_invoices SET base_currency = ?, exchange_rate = 1, rate_source = 'same', total_base = total, rate_date = created_at
			WHERE organization_id = ? AND (base_currency IS NULL OR base_currency = '') AND UPPER(COALESCE(currency,'')) IN (?, '')`, base, org, base)
		n += int(res.RowsAffected)
		res = db.Exec(`UPDATE billing_invoices SET base_currency = ?, rate_source = 'missing'
			WHERE organization_id = ? AND (base_currency IS NULL OR base_currency = '')`, base, org)
		n += int(res.RowsAffected)
	}
	return n
}

// LockRate serves POST /invoices/:id/rate — "Update rate" on a draft, or lock
// a rate on a document that has none. Body: {} (fetch the current rate) or
// {"exchange_rate": 0.0077} (manual). Converted lines are re-priced.
func (h *Handler) LockRate(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in struct {
		ExchangeRate *float64 `json:"exchange_rate"`
	}
	_ = c.ShouldBindJSON(&in)
	user := ctxString(c, "user_id")
	var out *Invoice
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		inv, err := h.load(tx, org, c.Param("id"), false)
		if err != nil {
			return err
		}
		if inv.Status == StatusVoid || inv.Status == StatusConverted {
			return &apiError{http.StatusConflict, "LOCKED", "This document can no longer be edited"}
		}
		if inv.Status != StatusDraft && inv.ExchangeRate > 0 && inv.Currency != inv.BaseCurrency {
			return &apiError{http.StatusConflict, "RATE_LOCKED", "The exchange rate is locked once a document has been sent"}
		}
		if inv.BaseCurrency == "" {
			inv.BaseCurrency = orgprefs.Currency(tx, org)
		}
		old := inv.ExchangeRate
		if inv.Currency == inv.BaseCurrency {
			if _, err := h.lockRate(tx, inv, rateInput{}, inv.Currency, inv.BaseCurrency); err != nil {
				return err
			}
		} else if in.ExchangeRate != nil {
			r := fxrates.SigRound(*in.ExchangeRate)
			if !(r > 0) || r > 1e9 {
				return &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Enter a rate greater than zero"}
			}
			t := h.now().UTC()
			inv.ExchangeRate, inv.RateSource, inv.RateDate, inv.RateProvider = r, fxrates.SourceManual, &t, "Entered by hand"
		} else {
			setQuote(inv, h.fx().Quote(org, inv.BaseCurrency, inv.Currency), h.now())
		}
		if inv.Currency != inv.BaseCurrency && inv.ExchangeRate <= 0 {
			return &apiError{http.StatusServiceUnavailable, "NO_RATE", "No exchange rate is available for " + inv.BaseCurrency + " → " + inv.Currency + ". Enter one by hand."}
		}
		if inv.Status == StatusDraft {
			reconvertLines(inv)
		}
		Recalculate(inv)
		ApplyPayments(inv, h.now())
		if inv.DocType == TypeInvoice && inv.Total+0.4/math.Pow10(currency.Decimals(inv.Currency)) < inv.AmountPaid {
			return &apiError{http.StatusConflict, "TOTAL_BELOW_PAID", "The new total would be less than the amount already paid"}
		}
		setBaseTotal(inv)
		for i := range inv.Items {
			it := inv.Items[i]
			if err := tx.Model(&InvoiceItem{}).Where("id = ?", it.ID).Updates(map[string]interface{}{
				"unit_price": it.UnitPrice, "line_subtotal": it.LineSubtotal, "line_discount": it.LineDiscount, "line_tax": it.LineTax, "line_total": it.LineTotal,
				"original_currency": it.OriginalCurrency}).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Updates(map[string]interface{}{
			"base_currency": inv.BaseCurrency, "exchange_rate": inv.ExchangeRate, "rate_source": inv.RateSource, "rate_date": inv.RateDate,
			"rate_provider": inv.RateProvider, "subtotal": inv.Subtotal, "discount_total": inv.DiscountTotal, "tax_total": inv.TaxTotal,
			"total": inv.Total, "balance_due": inv.BalanceDue, "total_base": inv.TotalBase, "status": inv.Status}).Error; err != nil {
			return err
		}
		msg := "Exchange rate set to " + RateText(inv)
		if old > 0 && old != inv.ExchangeRate {
			msg = "Exchange rate updated to " + RateText(inv) + " (was " + rateNum(old) + ")"
		}
		h.log(tx, inv, "rate", msg, user)
		out, err = h.load(tx, org, inv.ID, true)
		return err
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, out)
}

// rateNum prints a rate with up to 6 significant digits.
func rateNum(r float64) string {
	if r <= 0 {
		return "—"
	}
	s := strings.TrimRight(strings.TrimRight(formatSig(r, 6), "0"), ".")
	return s
}

func formatSig(v float64, sig int) string {
	if v == 0 {
		return "0"
	}
	mag := int(math.Floor(math.Log10(math.Abs(v))))
	dec := sig - 1 - mag
	if dec < 0 {
		dec = 0
	}
	if dec > 10 {
		dec = 10
	}
	return strconv.FormatFloat(v, 'f', dec, 64)
}

// RateText describes inv's rate: "1 KES = 0.0077 USD".
func RateText(inv *Invoice) string {
	if inv.BaseCurrency == "" || inv.Currency == inv.BaseCurrency {
		return "same currency"
	}
	if inv.ExchangeRate <= 0 {
		return "no rate"
	}
	if inv.ExchangeRate < 1 {
		return "1 " + inv.Currency + " = " + rateNum(1/inv.ExchangeRate) + " " + inv.BaseCurrency
	}
	return "1 " + inv.BaseCurrency + " = " + rateNum(inv.ExchangeRate) + " " + inv.Currency
}

// finishAmounts locks the exchange rate after an edit, re-prices converted
// catalog lines at it and recalculates every total (incl. TotalBase).
func (h *Handler) finishAmounts(tx *gorm.DB, inv *Invoice, in rateInput, prevCurrency, prevBase string) error {
	if _, err := h.lockRate(tx, inv, in, prevCurrency, prevBase); err != nil {
		return err
	}
	reconvertLines(inv)
	Recalculate(inv)
	ApplyPayments(inv, h.now())
	setBaseTotal(inv)
	return nil
}
