package hr

import (
	"fmt"
	"math"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// PeriodsPerYear returns how many pay periods of the given frequency fit in a year.
func PeriodsPerYear(freq string) int {
	switch freq {
	case FreqWeekly:
		return 52
	case FreqFortnightly:
		return 26
	case FreqMonthly:
		return 12
	}
	return 0
}

// RoundTo rounds v to the number of minor units of the currency (half away from zero).
func RoundTo(v float64, code string) float64 {
	p := math.Pow(10, float64(currency.Decimals(code)))
	return math.Round(v*p) / p
}

// LineInput holds what's needed to compute one employee's pay.
type LineInput struct {
	PayType       string  // hourly | salary
	Rate          float64 // hourly rate, or annual salary
	Hours         float64 // approved hours in the period (hourly staff)
	Frequency     string  // weekly | fortnightly | monthly
	DeductionRate float64 // percent withheld (tax, super, etc.), 0-100
	Currency      string
}

// LineResult is the computed pay for one employee.
type LineResult struct {
	Gross, Deductions, Net float64
}

// CalcLine computes gross, deductions and net pay:
//
//	hourly: gross = hours × rate
//	salary: gross = annual salary ÷ periods per year
//	deductions = gross × rate%, net = gross − deductions
//
// Each figure is rounded to the currency's minor unit, and net is derived from
// the rounded figures so gross = deductions + net always holds exactly.
func CalcLine(in LineInput) (LineResult, error) {
	if in.Rate < 0 || in.Hours < 0 {
		return LineResult{}, fmt.Errorf("rate and hours can't be negative")
	}
	if in.DeductionRate < 0 || in.DeductionRate > 100 {
		return LineResult{}, fmt.Errorf("deduction rate must be between 0 and 100")
	}
	var gross float64
	switch in.PayType {
	case PayHourly:
		gross = in.Hours * in.Rate
	case PaySalary:
		n := PeriodsPerYear(in.Frequency)
		if n == 0 {
			return LineResult{}, fmt.Errorf("unknown pay frequency %q", in.Frequency)
		}
		gross = in.Rate / float64(n)
	default:
		return LineResult{}, fmt.Errorf("unknown pay type %q", in.PayType)
	}
	gross = RoundTo(gross, in.Currency)
	ded := RoundTo(gross*in.DeductionRate/100, in.Currency)
	net := RoundTo(gross-ded, in.Currency)
	return LineResult{Gross: gross, Deductions: ded, Net: net}, nil
}

// PeriodEnd returns the inclusive last day of a period starting on start.
func PeriodEnd(start time.Time, freq string) time.Time {
	switch freq {
	case FreqWeekly:
		return start.AddDate(0, 0, 6)
	case FreqFortnightly:
		return start.AddDate(0, 0, 13)
	case FreqMonthly:
		return start.AddDate(0, 1, -1)
	}
	return start
}

// LeaveDays counts weekdays (Mon-Fri) between two dates inclusive.
func LeaveDays(start, end time.Time) float64 {
	if end.Before(start) {
		return 0
	}
	n := 0
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n++
		}
	}
	return float64(n)
}

// Totals sums pay-run lines.
func Totals(lines []PayRunLine, code string) (gross, ded, net float64) {
	for _, l := range lines {
		gross += l.Gross
		ded += l.Deductions
		net += l.Net
	}
	return RoundTo(gross, code), RoundTo(ded, code), RoundTo(net, code)
}
