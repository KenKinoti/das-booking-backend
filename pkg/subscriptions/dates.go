package subscriptions

import (
	"fmt"
	"math"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Date maths. Every date is a calendar date held as UTC midnight; "today"
// is the organisation's local date (see Service.Today), so time zones only
// matter when deciding which day it is.

const dayFmt = "2006-01-02"

// Day truncates t to its calendar date (UTC midnight) without converting
// time zones: 23:30 on 31 Jan in Nairobi stays 31 Jan.
func Day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// LocalDay is the calendar date of instant t in loc.
func LocalDay(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	return Day(t.In(loc))
}

// ParseDay parses YYYY-MM-DD (longer strings use their first 10 characters).
func ParseDay(s string) (time.Time, bool) {
	if len(s) >= 10 {
		if t, err := time.Parse(dayFmt, s[:10]); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// DayStr formats a date.
func DayStr(t time.Time) string { return t.Format(dayFmt) }

// DaysBetween is b − a in whole days.
func DaysBetween(a, b time.Time) int {
	return int(math.Round(Day(b).Sub(Day(a)).Hours() / 24))
}

// DaysIn is the number of days in a month (leap years included).
func DaysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// DateOn is day `day` of the month, clamped to its last day (31 → 30 Apr,
// 28/29 Feb).
func DateOn(year int, month time.Month, day int) time.Time {
	// Normalise the month first (month 13 = January of the next year).
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	if last := DaysIn(first.Year(), first.Month()); day > last {
		day = last
	}
	if day < 1 {
		day = 1
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
}

// AddMonthsOn moves n months from the month of `from` and lands on billing
// day `day` (clamped). It never drifts: 31 Jan + 1 → 28/29 Feb, + 2 → 31 Mar.
func AddMonthsOn(from time.Time, n, day int) time.Time {
	return DateOn(from.Year(), from.Month()+time.Month(n), day)
}

// Schedule is when a subscription's periods start.
type Schedule struct {
	Start      time.Time // first day of service
	Anchor     time.Time // first day of the first full period
	BillingDay int       // day of the month periods start on (1–31, clamped)
	Months     int       // period length
	Partial    bool      // a prorated first period runs from Start to Anchor
}

// NewSchedule works out the periods. Without proration the billing day is
// the start day. With proration and a different billing day, the first
// period runs from the start to the next billing day and is charged for its
// share of a full period.
func NewSchedule(start time.Time, billingDay int, prorate bool, period string) (Schedule, error) {
	m := PeriodMonths(period)
	if m == 0 {
		return Schedule{}, fmt.Errorf("choose a billing period")
	}
	if start.IsZero() {
		return Schedule{}, fmt.Errorf("choose a start date")
	}
	s := Day(start)
	if billingDay < 0 || billingDay > 31 {
		return Schedule{}, fmt.Errorf("the billing day is 1 to 31")
	}
	if !prorate || billingDay == 0 || DateOn(s.Year(), s.Month(), billingDay).Equal(s) {
		bd := s.Day()
		if billingDay > s.Day() && DateOn(s.Year(), s.Month(), billingDay).Equal(s) {
			bd = billingDay // e.g. day 31 starting on 30 April
		}
		return Schedule{Start: s, Anchor: s, BillingDay: bd, Months: m}, nil
	}
	a := DateOn(s.Year(), s.Month(), billingDay)
	if !a.After(s) {
		a = AddMonthsOn(s, 1, billingDay)
	}
	return Schedule{Start: s, Anchor: a, BillingDay: billingDay, Months: m, Partial: true}, nil
}

// Span is one billing period [Start, End).
type Span struct {
	Index    int // −1 for the prorated first period
	Start    time.Time
	End      time.Time
	Partial  bool
	Fraction float64 // share of a full period (1 for full periods)
}

// Days is the number of days in the period.
func (p Span) Days() int { return DaysBetween(p.Start, p.End) }

// At is the k-th full period (k ≥ 0).
func (s Schedule) At(k int) Span {
	return Span{Index: k, Start: AddMonthsOn(s.Anchor, k*s.Months, s.BillingDay), End: AddMonthsOn(s.Anchor, (k+1)*s.Months, s.BillingDay), Fraction: 1}
}

// First is the first period (the prorated one when there is one).
func (s Schedule) First() Span {
	if !s.Partial {
		return s.At(0)
	}
	prev := AddMonthsOn(s.Anchor, -s.Months, s.BillingDay)
	full := DaysBetween(prev, s.Anchor)
	part := DaysBetween(s.Start, s.Anchor)
	f := 1.0
	if full > 0 {
		f = float64(part) / float64(full)
	}
	if f > 1 {
		f = 1
	}
	return Span{Index: -1, Start: s.Start, End: s.Anchor, Partial: true, Fraction: math.Round(f*10000) / 10000}
}

// StartingOn finds the period that starts on d (ok false when no period does).
func (s Schedule) StartingOn(d time.Time) (Span, bool) {
	d = Day(d)
	if f := s.First(); f.Start.Equal(d) {
		return f, true
	}
	if d.Before(s.Anchor) {
		return Span{}, false
	}
	months := (d.Year()-s.Anchor.Year())*12 + int(d.Month()) - int(s.Anchor.Month())
	k := months / s.Months
	for _, kk := range []int{k - 1, k, k + 1} {
		if kk < 0 {
			continue
		}
		if p := s.At(kk); p.Start.Equal(d) {
			return p, true
		}
	}
	return Span{}, false
}

// Covering is the period that contains d (the first period when d is before it).
func (s Schedule) Covering(d time.Time) Span {
	d = Day(d)
	f := s.First()
	if d.Before(f.End) {
		return f
	}
	months := (d.Year()-s.Anchor.Year())*12 + int(d.Month()) - int(s.Anchor.Month())
	k := months/s.Months - 1
	if k < 0 {
		k = 0
	}
	for i := 0; i < 4; i++ {
		p := s.At(k + i)
		if !d.Before(p.Start) && d.Before(p.End) {
			return p
		}
	}
	return s.At(k)
}

// Next is the period after p.
func (s Schedule) Next(p Span) Span {
	if p.Index < 0 {
		return s.At(0)
	}
	return s.At(p.Index + 1)
}

// ---------------------------------------------------------------- invoices

// LeadFor is the organisation's default for a period: how many days before
// the period starts its invoice is created.
func LeadFor(st Settings, period string) int {
	switch period {
	case PeriodYearly:
		return st.LeadYearly
	case PeriodQuarterly:
		return st.LeadQuarterly
	}
	return st.LeadMonthly
}

// Lead is the subscription's own lead time, else the organisation's for its
// period, kept shorter than the period so two invoices are never open for
// one period.
func Lead(own *int, st Settings, period string) int {
	l := LeadFor(st, period)
	if own != nil {
		l = *own
	}
	if l < 0 {
		l = 0
	}
	if max := PeriodMonths(period)*28 - 1; max > 0 && l > max {
		l = max
	}
	return l
}

// DueDate is when a period's invoice is due: its start + payment terms.
func DueDate(p Span, terms int) time.Time { return Day(p.Start).AddDate(0, 0, terms) }

// IssueDate is when a period's invoice is created: lead days before the
// period starts.
func IssueDate(p Span, lead int) time.Time { return Day(p.Start).AddDate(0, 0, -lead) }

// Prorate is price × fraction rounded to the currency's minor units.
func Prorate(price, fraction float64, cur string) float64 {
	return RoundCur(price*fraction, cur)
}

// RoundCur rounds to the currency's minor units.
func RoundCur(v float64, cur string) float64 {
	p := math.Pow10(currency.Decimals(cur))
	return math.Round(v*p) / p
}

// Monthly is a per-period amount per month.
func Monthly(amount float64, period string) float64 {
	m := PeriodMonths(period)
	if m == 0 {
		return 0
	}
	return amount / float64(m)
}

// PerPeriod converts a price for one period (from, e.g. a monthly catalogue
// price) to another (to, e.g. yearly). An unknown `from` is taken as `to`.
func PerPeriod(price float64, from, to string) float64 {
	fm, tm := PeriodMonths(from), PeriodMonths(to)
	if fm == 0 || tm == 0 || fm == tm {
		return price
	}
	return price / float64(fm) * float64(tm)
}

// StartOfMonth is the first day of d's month.
func StartOfMonth(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC)
}
