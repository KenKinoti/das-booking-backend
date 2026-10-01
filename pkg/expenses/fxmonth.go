package expenses

// Exchange rates per month.
//
// An expense is converted to the home currency at the rate of the month it
// is dated in, and that rate is stored on the expense (fx_rate, fx_source,
// fx_date, fx_basis, fx_note). The rate of a month is the latest rate seen
// while the month was current (recorded on every conversion and by the daily
// sync), so once the month is over it is effectively its month-end rate and
// never changes on its own. A month that ended before any rate was recorded
// gets the rate of the day it was first needed, clearly labelled
// ("backfilled"); the user can set a month's rate by hand (Expenses →
// Synergy → rates), which re-converts the Synergy expenses of that month.

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
)

// Rate bases.
const (
	FxBasisSame       = "same"
	FxBasisMonth      = "month"      // the rate recorded during that month
	FxBasisBackfilled = "backfilled" // no rate was recorded during the month: the rate on the day it was first needed
	FxBasisManual     = "manual"     // set by the user for that month
	FxBasisToday      = "today"      // today's rate (current-view conversions)
	FxBasisMissing    = "missing"
)

// FxMonth is the rate used for one month (expense_fx_months).
type FxMonth struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_efx_key,priority:1"`
	FromCurrency   string     `json:"from" gorm:"type:varchar(3);not null;uniqueIndex:idx_efx_key,priority:2"`
	ToCurrency     string     `json:"to" gorm:"type:varchar(3);not null;uniqueIndex:idx_efx_key,priority:3"`
	Month          string     `json:"month" gorm:"type:varchar(7);not null;uniqueIndex:idx_efx_key,priority:4"`
	Rate           float64    `json:"rate"`
	Source         string     `json:"source" gorm:"type:varchar(20)"` // live | fallback | manual (fxrates)
	Provider       string     `json:"provider" gorm:"type:varchar(160)"`
	AsOf           *time.Time `json:"as_of"`
	Manual         bool       `json:"manual"`
	Backfilled     bool       `json:"backfilled"`
	UpdatedBy      string     `json:"updated_by" gorm:"type:varchar(255)"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (FxMonth) TableName() string { return "expense_fx_months" }

// RateInfo is a conversion rate with where it came from.
type RateInfo struct {
	From     string     `json:"from"`
	To       string     `json:"to"`
	Month    string     `json:"month,omitempty"`
	Rate     float64    `json:"rate"` // units of To per 1 From
	Source   string     `json:"source"`
	Provider string     `json:"provider,omitempty"`
	AsOf     *time.Time `json:"as_of"`
	Basis    string     `json:"basis"` // same | month | backfilled | manual | today | missing
	Label    string     `json:"label"`
	Stale    bool       `json:"stale,omitempty"`
}

func trimRate(v float64) string {
	switch {
	case v >= 100:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
	case v >= 1:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
	}
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
}

// rateLabel: "1 AUD = 85.4 KES · live · Sep 2026 rate (30 Sep)".
func rateLabel(r RateInfo) string {
	if r.From == "" || r.To == "" || r.From == r.To {
		return ""
	}
	if r.Rate <= 0 {
		return "no rate for " + r.From + " → " + r.To
	}
	l := fmt.Sprintf("1 %s = %s %s", r.From, trimRate(r.Rate), r.To)
	src := map[string]string{fxrates.SourceLive: "live", fxrates.SourceFallback: "reference", fxrates.SourceManual: "your rate"}[r.Source]
	if r.Basis == FxBasisManual {
		src = "your rate"
	}
	if src != "" {
		l += " · " + src
	}
	mon := ""
	if t, err := time.Parse("2006-01", r.Month); err == nil {
		mon = t.Format("Jan 2006")
	}
	day := ""
	if r.AsOf != nil {
		day = r.AsOf.Format("2 Jan 2006")
	}
	switch r.Basis {
	case FxBasisMonth:
		l += " · " + mon + " rate"
		if day != "" {
			l += " (" + day + ")"
		}
	case FxBasisBackfilled:
		l += " · rate of " + day + " (none was recorded in " + mon + ")"
	case FxBasisManual:
		l += " · set for " + mon
	case FxBasisToday:
		l += " · today"
		if day != "" {
			l += " (" + day + ")"
		}
	}
	return l
}

func (s *Service) fxRow(org, from, to, month string) *FxMonth {
	var r FxMonth
	if s.DB.Where("organization_id = ? AND from_currency = ? AND to_currency = ? AND month = ?", org, from, to, month).First(&r).Error != nil {
		return nil
	}
	return &r
}

func infoOf(r *FxMonth) RateInfo {
	b := FxBasisMonth
	switch {
	case r.Manual:
		b = FxBasisManual
	case r.Backfilled:
		b = FxBasisBackfilled
	}
	ri := RateInfo{From: r.FromCurrency, To: r.ToCurrency, Month: r.Month, Rate: r.Rate, Source: r.Source, Provider: r.Provider, AsOf: r.AsOf, Basis: b}
	ri.Label = rateLabel(ri)
	return ri
}

// RateFor is the rate to convert an amount dated day (see the package notes).
func (s *Service) RateFor(org, from, to string, day time.Time) RateInfo {
	from, to = strings.ToUpper(strings.TrimSpace(from)), strings.ToUpper(strings.TrimSpace(to))
	now := s.now()
	if from == to || from == "" || to == "" {
		t := now
		return RateInfo{From: from, To: to, Rate: 1, Source: fxrates.SourceSame, Basis: FxBasisSame, AsOf: &t}
	}
	if day.IsZero() {
		day = now
	}
	m, cur := monthOf(day), monthOf(now)
	row := s.fxRow(org, from, to, m)
	if row != nil && row.Rate > 0 && (row.Manual || m < cur) {
		return infoOf(row)
	}
	q := s.FX.Quote(org, from, to)
	if !q.OK() {
		if row != nil && row.Rate > 0 {
			return infoOf(row)
		}
		return RateInfo{From: from, To: to, Month: m, Source: fxrates.SourceMissing, Basis: FxBasisMissing, Label: "no rate for " + from + " → " + to + " — set one in Invoice settings → Exchange rates"}
	}
	asOf := q.AsOf
	if asOf == nil {
		t := now
		asOf = &t
	}
	if m > cur {
		ri := RateInfo{From: from, To: to, Month: m, Rate: q.Rate, Source: q.Source, Provider: q.Provider, AsOf: asOf, Basis: FxBasisToday, Stale: q.Stale}
		ri.Label = rateLabel(ri)
		return ri
	}
	if row == nil {
		row = &FxMonth{ID: uuid.NewString(), OrganizationID: org, FromCurrency: from, ToCurrency: to, Month: m, CreatedAt: now, Backfilled: m < cur}
	}
	row.Rate, row.Source, row.Provider, row.AsOf, row.UpdatedAt = q.Rate, q.Source, clip(q.Provider, 158), asOf, now
	s.DB.Save(row)
	ri := infoOf(row)
	ri.Stale = q.Stale
	return ri
}

// TodayRate is the current rate (current-view conversions such as run-rates).
func (s *Service) TodayRate(org, from, to string) RateInfo {
	from, to = strings.ToUpper(from), strings.ToUpper(to)
	now := s.now()
	if from == to {
		return RateInfo{From: from, To: to, Rate: 1, Source: fxrates.SourceSame, Basis: FxBasisSame, AsOf: &now}
	}
	ri := s.RateFor(org, from, to, now) // records this month's rate
	if ri.Basis == FxBasisMonth {
		ri.Basis = FxBasisToday
		ri.Label = rateLabel(ri)
	}
	return ri
}

// RecordMonthRates records this month's rate for the currencies expenses use.
func (s *Service) RecordMonthRates(org string) {
	home := s.Home(org)
	for _, c := range []string{"AUD", "EUR", "USD", "GBP"} {
		if c != home {
			s.RateFor(org, c, home, s.now())
		}
	}
}

// MonthRates lists the recorded rates of a pair, newest first.
func (s *Service) MonthRates(org, from, to string) []RateInfo {
	var rows []FxMonth
	s.DB.Where("organization_id = ? AND from_currency = ? AND to_currency = ?", org, from, to).Order("month DESC").Limit(24).Find(&rows)
	out := []RateInfo{}
	cur := monthOf(s.now())
	for i := range rows {
		ri := infoOf(&rows[i])
		if rows[i].Month == cur && !rows[i].Manual {
			ri.Label += " · until the month ends"
		}
		out = append(out, ri)
	}
	return out
}

// MonthRateInput sets (rate > 0) or clears (rate 0) a month's rate by hand.
type MonthRateInput struct {
	From  string  `json:"from"`
	To    string  `json:"to"`
	Month string  `json:"month"`
	Rate  float64 `json:"rate"`
}

// SetMonthRate stores a manual month rate and re-converts the Synergy expenses.
func (s *Service) SetMonthRate(org, user string, in MonthRateInput) (*SynergyApplyResult, error) {
	in.From, in.To = strings.ToUpper(strings.TrimSpace(in.From)), strings.ToUpper(strings.TrimSpace(in.To))
	if in.To == "" {
		in.To = s.Home(org)
	}
	if !currency.IsSupported(in.From) || !currency.IsSupported(in.To) || in.From == in.To {
		return nil, invalid("choose two different supported currencies")
	}
	if _, err := time.Parse("2006-01", in.Month); err != nil {
		return nil, invalid("month must look like 2026-09")
	}
	if in.Rate < 0 || in.Rate > 1e7 {
		return nil, invalid("enter a rate above zero")
	}
	row := s.fxRow(org, in.From, in.To, in.Month)
	now := s.now()
	if in.Rate == 0 {
		if row != nil && row.Manual {
			s.DB.Delete(&FxMonth{}, "id = ?", row.ID)
		}
	} else {
		if row == nil {
			row = &FxMonth{ID: uuid.NewString(), OrganizationID: org, FromCurrency: in.From, ToCurrency: in.To, Month: in.Month, CreatedAt: now}
		}
		row.Rate, row.Source, row.Provider, row.Manual, row.Backfilled, row.AsOf, row.UpdatedBy, row.UpdatedAt = fxrates.SigRound(in.Rate), fxrates.SourceManual, "Set by you", true, false, &now, user, now
		if err := s.DB.Save(row).Error; err != nil {
			return nil, err
		}
	}
	return s.ApplySynergyAccounting(org, user)
}
