// Package fxrates quotes exchange rates between the platform's supported
// currencies for documents issued in a customer's currency.
//
// Order of precedence for a pair (from → to):
//
//  1. same currency                       source "same"     (rate 1)
//  2. an organisation's manual override   source "manual"   (Settings → Exchange rates)
//  3. live market rates (pkg/plans FX via pkg/market, USD-based)  source "live"
//  4. the reference table (plan_fx_rates / built-in defaults)      source "fallback"
//  5. nothing                                                       source "missing"
//
// A Quote's Rate is units of To per 1 From. Documents lock the quote they were
// created with (see pkg/invoicing); a rate is never silently re-applied.
package fxrates

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/plans"
)

// Sources.
const (
	SourceSame     = "same"
	SourceManual   = "manual"
	SourceLive     = "live"
	SourceFallback = "fallback"
	SourceMissing  = "missing"
)

// StaleAfter is how old a live rate may be before it is flagged.
const StaleAfter = 24 * time.Hour

// Live is a USD-based rate source: units of code per 1 USD. plans.FX
// satisfies it (source "live", "fallback" or "fixed").
type Live interface {
	Rate(code string) (rate float64, source string, ok bool)
}

// ManualRate is an organisation's own rate for a currency pair.
type ManualRate struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	FromCurrency   string    `json:"from_currency" gorm:"type:varchar(3);not null"`
	ToCurrency     string    `json:"to_currency" gorm:"type:varchar(3);not null"`
	Rate           float64   `json:"rate" gorm:"not null"` // units of To per 1 From
	Note           string    `json:"note" gorm:"type:varchar(255)"`
	UpdatedBy      string    `json:"updated_by" gorm:"type:varchar(255)"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName keeps the table name explicit.
func (ManualRate) TableName() string { return "fx_manual_rates" }

// Migrate creates the package's tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&ManualRate{}) }

// Quote is one conversion rate with where it came from.
type Quote struct {
	From     string     `json:"from"`
	To       string     `json:"to"`
	Rate     float64    `json:"rate"`    // units of To per 1 From
	Inverse  float64    `json:"inverse"` // units of From per 1 To
	Source   string     `json:"source"`  // same | manual | live | fallback | missing
	AsOf     *time.Time `json:"as_of"`   // when the rate was published / set (nil: reference table)
	Stale    bool       `json:"stale"`   // live rate older than 24h
	Provider string     `json:"provider"`
	ManualID string     `json:"manual_id,omitempty"`
	Note     string     `json:"note,omitempty"`
	Warning  string     `json:"warning,omitempty"`
}

// OK reports whether the quote has a usable rate.
func (q Quote) OK() bool { return q.Rate > 0 && q.Source != SourceMissing }

// Service quotes rates.
type Service struct {
	DB   *gorm.DB
	Live Live
	// LiveAsOf returns when live rates were last refreshed (nil: never).
	LiveAsOf func() *time.Time
	Now      func() time.Time
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// defaultPerUSD is the built-in reference table (plans.DefaultFXRates).
var defaultPerUSD = func() map[string]float64 {
	m := map[string]float64{}
	for _, r := range plans.DefaultFXRates() {
		m[strings.ToUpper(r.Currency)] = r.RatePerUSD
	}
	return m
}()

func norm(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }

// SigRound rounds a rate to 8 significant digits (removes float noise).
func SigRound(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	f, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 8, 64), 64)
	return f
}

// perUSD returns units of code per 1 USD and its source.
func (s *Service) perUSD(code string) (float64, string) {
	if code == "USD" {
		return 1, SourceLive
	}
	if s != nil && s.Live != nil {
		if r, src, ok := s.Live.Rate(code); ok && r > 0 {
			switch src {
			case "live", "fixed":
				return r, SourceLive
			default:
				return r, SourceFallback
			}
		}
	}
	if r, ok := defaultPerUSD[code]; ok && r > 0 {
		return r, SourceFallback
	}
	return 0, SourceMissing
}

// Manual returns the organisation's override for from→to (or the inverse of
// a to→from override).
func (s *Service) Manual(org, from, to string) (*ManualRate, float64) {
	if s == nil || s.DB == nil || org == "" || !s.DB.Migrator().HasTable(&ManualRate{}) {
		return nil, 0
	}
	var rows []ManualRate
	s.DB.Where("organization_id = ? AND ((from_currency = ? AND to_currency = ?) OR (from_currency = ? AND to_currency = ?))", org, from, to, to, from).
		Order("updated_at DESC").Limit(2).Find(&rows)
	for _, r := range rows {
		if r.Rate <= 0 {
			continue
		}
		if r.FromCurrency == from {
			rr := r
			return &rr, r.Rate
		}
	}
	for _, r := range rows {
		if r.Rate > 0 {
			rr := r
			return &rr, 1 / r.Rate
		}
	}
	return nil, 0
}

// Quote returns the rate to convert from → to for org.
func (s *Service) Quote(org, from, to string) Quote {
	from, to = norm(from), norm(to)
	q := Quote{From: from, To: to, Source: SourceMissing}
	if from == "" || to == "" {
		q.Warning = "Currency missing"
		return q
	}
	if from == to {
		q.Rate, q.Inverse, q.Source, q.Provider = 1, 1, SourceSame, "Same currency"
		t := s.now()
		q.AsOf = &t
		return q
	}
	if !currency.IsSupported(from) || !currency.IsSupported(to) {
		q.Warning = "Unsupported currency"
		return q
	}
	if m, r := s.Manual(org, from, to); m != nil && r > 0 {
		t := m.UpdatedAt
		q.Rate, q.Source, q.AsOf, q.ManualID, q.Note = SigRound(r), SourceManual, &t, m.ID, m.Note
		q.Inverse = SigRound(1 / r)
		q.Provider = "Your manual rate"
		q.Warning = "Manual rate set by your organisation"
		return q
	}
	fr, fsrc := s.perUSD(from)
	tr, tsrc := s.perUSD(to)
	if fr <= 0 || tr <= 0 {
		q.Warning = "No exchange rate available for " + from + " → " + to + ". Set a manual rate."
		return q
	}
	q.Rate = SigRound(tr / fr)
	q.Inverse = SigRound(fr / tr)
	if fsrc == SourceLive && tsrc == SourceLive {
		q.Source = SourceLive
		q.Provider = "Market rates (ECB / ExchangeRate-API)"
		if s != nil && s.LiveAsOf != nil {
			if t := s.LiveAsOf(); t != nil {
				tt := *t
				q.AsOf = &tt
			}
		}
		if q.AsOf == nil {
			t := s.now()
			q.AsOf = &t
		}
		if s.now().Sub(*q.AsOf) > StaleAfter {
			q.Stale = true
			q.Warning = "Live rate is more than 24 hours old"
		}
		return q
	}
	q.Source = SourceFallback
	q.Provider = "Reference table (live rates unavailable)"
	q.Warning = "Live rates are unavailable — this is an approximate reference rate. Check it or set a manual rate."
	return q
}

// Convert converts amount at rate and rounds to the target currency's minor units.
func Convert(amount, rate float64, to string) float64 {
	return Round(amount*rate, to)
}

// Round rounds v to the currency's minor units (JPY 0, KWD 3, else 2).
func Round(v float64, cur string) float64 {
	p := math.Pow10(currency.Decimals(cur))
	return math.Round(v*p) / p
}

// ---------------------------------------------------------------- manual rates CRUD

// ErrInvalid is returned for a bad manual rate.
var ErrInvalid = errors.New("invalid rate")

// SaveManual creates or replaces the org's rate for a pair.
func (s *Service) SaveManual(org, user, from, to string, rate float64, note string) (*ManualRate, error) {
	from, to = norm(from), norm(to)
	if !currency.IsSupported(from) || !currency.IsSupported(to) || from == to {
		return nil, errors.New("choose two different supported currencies")
	}
	if !(rate > 0) || rate > 1e9 {
		return nil, errors.New("the rate must be a positive number")
	}
	if len(note) > 255 {
		note = note[:255]
	}
	var row ManualRate
	err := s.DB.Where("organization_id = ? AND ((from_currency = ? AND to_currency = ?) OR (from_currency = ? AND to_currency = ?))", org, from, to, to, from).First(&row).Error
	now := s.now()
	if errors.Is(err, gorm.ErrRecordNotFound) {
		row = ManualRate{ID: uuid.NewString(), OrganizationID: org, CreatedAt: now}
	} else if err != nil {
		return nil, err
	}
	row.FromCurrency, row.ToCurrency, row.Rate, row.Note, row.UpdatedBy, row.UpdatedAt = from, to, SigRound(rate), strings.TrimSpace(note), user, now
	if err := s.DB.Save(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// ListManual lists the org's manual rates.
func (s *Service) ListManual(org string) []ManualRate {
	out := []ManualRate{}
	if s.DB.Migrator().HasTable(&ManualRate{}) {
		s.DB.Where("organization_id = ?", org).Order("from_currency, to_currency").Find(&out)
	}
	return out
}

// DeleteManual removes one of the org's manual rates.
func (s *Service) DeleteManual(org, id string) bool {
	res := s.DB.Where("organization_id = ? AND id = ?", org, id).Delete(&ManualRate{})
	return res.Error == nil && res.RowsAffected > 0
}
