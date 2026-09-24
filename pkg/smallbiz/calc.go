package smallbiz

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// ---------- money ----------

func pow10(n int) int64 {
	p := int64(1)
	for i := 0; i < n; i++ {
		p *= 10
	}
	return p
}

// ParseMinor converts a decimal string ("1,234.50") to minor units for a
// currency with the given number of decimals, without going through floats.
func ParseMinor(s string, decimals int) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, ",", ""), " ", ""))
	if s == "" {
		return 0, errors.New("amount is required")
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" && (!hasDot || frac == "") {
		return 0, errors.New("amount is not a number")
	}
	if whole == "" {
		whole = "0"
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, errors.New("amount is not a number")
		}
	}
	frac = strings.TrimRight(frac, "0")
	if len(frac) > decimals {
		if decimals == 0 {
			return 0, errors.New("this currency has no decimal places")
		}
		return 0, fmt.Errorf("use at most %d decimal places", decimals)
	}
	if len(whole) > 15 {
		return 0, errors.New("amount is too large")
	}
	frac += strings.Repeat("0", decimals-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, errors.New("amount is not a number")
	}
	var f int64
	if decimals > 0 {
		f, _ = strconv.ParseInt(frac, 10, 64)
	}
	v := w*pow10(decimals) + f
	if neg {
		v = -v
	}
	return v, nil
}

// FormatMinor renders minor units as a plain decimal string ("1234.50").
func FormatMinor(v int64, decimals int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	p := pow10(decimals)
	s := strconv.FormatInt(v/p, 10)
	if decimals > 0 {
		s += "." + fmt.Sprintf("%0*d", decimals, v%p)
	}
	if neg {
		s = "-" + s
	}
	return s
}

// rateBP converts a percentage (e.g. 7.5) to basis points (750).
func rateBP(rate float64) int64 { return int64(math.Round(rate * 100)) }

// divRound divides with half-away-from-zero rounding.
func divRound(num, den int64) int64 {
	if den == 0 {
		return 0
	}
	if (num < 0) != (den < 0) {
		return -((abs(num)*2 + abs(den)) / (abs(den) * 2))
	}
	return (abs(num)*2 + abs(den)) / (abs(den) * 2)
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// TaxFromInclusive extracts the tax contained in a tax-inclusive amount:
// tax = amount × rate / (100 + rate), rounded to the nearest minor unit.
func TaxFromInclusive(amount int64, rate float64) int64 {
	bp := rateBP(rate)
	if bp <= 0 || amount == 0 {
		return 0
	}
	return divRound(amount*bp, 10000+bp)
}

// ValidRate reports whether a tax rate percentage is acceptable.
func ValidRate(rate float64) bool { return rate >= 0 && rate <= 100 && !math.IsNaN(rate) }

// ---------- dates & periods ----------

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// ParseDate accepts YYYY-MM-DD or RFC3339.
func ParseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date %q (use YYYY-MM-DD)", s)
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// FYStartDate returns the first day of the financial year containing t.
func FYStartDate(t time.Time, startMonth int) time.Time {
	if startMonth < 1 || startMonth > 12 {
		startMonth = 1
	}
	y := t.Year()
	if int(t.Month()) < startMonth {
		y--
	}
	return time.Date(y, time.Month(startMonth), 1, 0, 0, 0, 0, time.UTC)
}

// FYLabel names the financial year beginning at start, e.g. "FY 2026–27" or "2026".
func FYLabel(start time.Time) string {
	if start.Month() == time.January {
		return strconv.Itoa(start.Year())
	}
	return fmt.Sprintf("FY %d–%02d", start.Year(), (start.Year()+1)%100)
}

// Period is an inclusive date range.
type Period struct {
	Key   string    `json:"key"`
	Label string    `json:"label"`
	From  time.Time `json:"-"`
	To    time.Time `json:"-"` // inclusive
}

// Until returns the exclusive upper bound for queries.
func (p Period) Until() time.Time { return p.To.AddDate(0, 0, 1) }

func (p Period) Contains(d time.Time) bool {
	d = dateOnly(d)
	return !d.Before(p.From) && !d.After(p.To)
}

// ResolvePeriod turns a period key (this_month, last_month, this_quarter,
// last_quarter, this_fy, last_fy, ytd, all) or explicit from/to dates into a range.
func ResolvePeriod(key, from, to string, today time.Time, fyStart int) (Period, error) {
	today = dateOnly(today)
	if from != "" || to != "" {
		p := Period{Key: "custom", Label: "Custom range"}
		var err error
		if from != "" {
			if p.From, err = ParseDate(from); err != nil {
				return p, err
			}
		} else {
			p.From = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		if to != "" {
			if p.To, err = ParseDate(to); err != nil {
				return p, err
			}
		} else {
			p.To = time.Date(2999, 12, 31, 0, 0, 0, 0, time.UTC)
		}
		if p.To.Before(p.From) {
			return p, errors.New("end date is before start date")
		}
		return p, nil
	}
	ms := monthStart(today)
	qs := time.Date(today.Year(), time.Month(((int(today.Month())-1)/3)*3+1), 1, 0, 0, 0, 0, time.UTC)
	fy := FYStartDate(today, fyStart)
	mk := func(k, label string, a, b time.Time) Period { return Period{Key: k, Label: label, From: a, To: b} }
	switch key {
	case "", "this_month":
		return mk("this_month", ms.Format("January 2006"), ms, ms.AddDate(0, 1, -1)), nil
	case "last_month":
		lm := ms.AddDate(0, -1, 0)
		return mk("last_month", lm.Format("January 2006"), lm, ms.AddDate(0, 0, -1)), nil
	case "this_quarter":
		return mk("this_quarter", quarterLabel(qs), qs, qs.AddDate(0, 3, -1)), nil
	case "last_quarter":
		lq := qs.AddDate(0, -3, 0)
		return mk("last_quarter", quarterLabel(lq), lq, qs.AddDate(0, 0, -1)), nil
	case "this_fy":
		return mk("this_fy", FYLabel(fy), fy, fy.AddDate(1, 0, -1)), nil
	case "last_fy":
		lf := fy.AddDate(-1, 0, 0)
		return mk("last_fy", FYLabel(lf), lf, fy.AddDate(0, 0, -1)), nil
	case "ytd":
		return mk("ytd", FYLabel(fy)+" to date", fy, today), nil
	case "all":
		return mk("all", "All time", time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2999, 12, 31, 0, 0, 0, 0, time.UTC)), nil
	}
	return Period{}, fmt.Errorf("unknown period %q", key)
}

func quarterLabel(qs time.Time) string {
	end := qs.AddDate(0, 2, 0)
	return fmt.Sprintf("%s – %s", qs.Format("Jan"), end.Format("Jan 2006"))
}

// AddMonthsAnchored adds n months, keeping the anchor day where possible and
// clamping to the month's last day (31 Jan + 1 month = 28/29 Feb, then 31 Mar).
func AddMonthsAnchored(d time.Time, n, anchorDay int) time.Time {
	if anchorDay <= 0 {
		anchorDay = d.Day()
	}
	first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, n, 0)
	last := first.AddDate(0, 1, -1).Day()
	day := anchorDay
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
}

// RecurrenceMonths maps a recurrence to its interval in months (0 = none).
func RecurrenceMonths(r string) int {
	switch r {
	case "monthly":
		return 1
	case "quarterly":
		return 3
	case "yearly":
		return 12
	}
	return 0
}

// NextDue returns the next due date for a recurring reminder.
func NextDue(due time.Time, recurrence string, anchorDay int) (time.Time, bool) {
	m := RecurrenceMonths(recurrence)
	if m == 0 {
		return time.Time{}, false
	}
	return AddMonthsAnchored(due, m, anchorDay), true
}

// ---------- totals ----------

// Totals summarises a set of entries in one currency.
type Totals struct {
	Income       int64 `json:"income"`     // gross (tax inclusive)
	Expenses     int64 `json:"expenses"`   // gross
	IncomeNet    int64 `json:"income_net"` // excl. tax
	ExpensesNet  int64 `json:"expenses_net"`
	Profit       int64 `json:"profit"`   // IncomeNet - ExpensesNet
	NetCash      int64 `json:"net_cash"` // Income - Expenses
	TaxCollected int64 `json:"tax_collected"`
	TaxPaid      int64 `json:"tax_paid"`
	TaxPayable   int64 `json:"tax_payable"` // collected - paid (negative = refund)
	Count        int   `json:"count"`
}

func (t *Totals) Add(e Entry) {
	t.Count++
	if e.Type == TypeIncome {
		t.Income += e.Amount
		t.TaxCollected += e.Tax
		t.IncomeNet += e.Amount - e.Tax
	} else {
		t.Expenses += e.Amount
		t.TaxPaid += e.Tax
		t.ExpensesNet += e.Amount - e.Tax
	}
	t.Profit = t.IncomeNet - t.ExpensesNet
	t.NetCash = t.Income - t.Expenses
	t.TaxPayable = t.TaxCollected - t.TaxPaid
}

// Summarize totals entries of the given currency (all entries when cur == "").
func Summarize(entries []Entry, cur string) Totals {
	var t Totals
	for _, e := range entries {
		if cur == "" || e.Currency == cur {
			t.Add(e)
		}
	}
	return t
}

// SortChronological orders entries by date, then creation time, then id.
func SortChronological(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.Before(b.Date)
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

// RunningBalances returns the cash balance after each entry, per currency.
// opening applies to the base currency only.
func RunningBalances(entries []Entry, base string, opening int64) map[string]int64 {
	sorted := append([]Entry(nil), entries...)
	SortChronological(sorted)
	bal := map[string]int64{base: opening}
	out := make(map[string]int64, len(sorted))
	for _, e := range sorted {
		if e.Type == TypeIncome {
			bal[e.Currency] += e.Amount
		} else {
			bal[e.Currency] -= e.Amount
		}
		out[e.ID] = bal[e.Currency]
	}
	return out
}

// MonthRow is one month of a profit & loss report.
type MonthRow struct {
	Month string `json:"month"` // YYYY-MM
	Totals
}

// MonthlyTotals buckets entries into the months between from and to (inclusive).
func MonthlyTotals(entries []Entry, cur string, from, to time.Time) []MonthRow {
	rows := []MonthRow{}
	idx := map[string]int{}
	for m := monthStart(from); !m.After(to); m = m.AddDate(0, 1, 0) {
		idx[m.Format("2006-01")] = len(rows)
		rows = append(rows, MonthRow{Month: m.Format("2006-01")})
	}
	for _, e := range entries {
		if cur != "" && e.Currency != cur {
			continue
		}
		if i, ok := idx[e.Date.Format("2006-01")]; ok {
			rows[i].Add(e)
		}
	}
	return rows
}

// CategoryTotal is net (excl. tax) spend or income per category.
type CategoryTotal struct {
	CategoryID string `json:"category_id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Amount     int64  `json:"amount"` // excl. tax
	Gross      int64  `json:"gross"`
	Count      int    `json:"count"`
}

// ByCategory totals entries of one type per category, largest first.
func ByCategory(entries []Entry, cur, typ string, names map[string]string) []CategoryTotal {
	m := map[string]*CategoryTotal{}
	order := []string{}
	for _, e := range entries {
		if e.Type != typ || (cur != "" && e.Currency != cur) {
			continue
		}
		ct, ok := m[e.CategoryID]
		if !ok {
			name := names[e.CategoryID]
			if name == "" {
				name = "Uncategorised"
			}
			ct = &CategoryTotal{CategoryID: e.CategoryID, Name: name, Type: typ}
			m[e.CategoryID] = ct
			order = append(order, e.CategoryID)
		}
		ct.Amount += e.Amount - e.Tax
		ct.Gross += e.Amount
		ct.Count++
	}
	out := make([]CategoryTotal, 0, len(order))
	for _, k := range order {
		out = append(out, *m[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Amount != out[j].Amount {
			return out[i].Amount > out[j].Amount
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// RateTotal is the tax summary for one tax rate.
type RateTotal struct {
	Rate         float64 `json:"rate"`
	Sales        int64   `json:"sales"`     // gross income at this rate
	Purchases    int64   `json:"purchases"` // gross expenses at this rate
	TaxCollected int64   `json:"tax_collected"`
	TaxPaid      int64   `json:"tax_paid"`
}

// TaxByRate groups tax collected and paid by rate (lowest rate first).
func TaxByRate(entries []Entry, cur string) []RateTotal {
	m := map[int64]*RateTotal{}
	for _, e := range entries {
		if cur != "" && e.Currency != cur {
			continue
		}
		bp := rateBP(e.TaxRate)
		r, ok := m[bp]
		if !ok {
			r = &RateTotal{Rate: float64(bp) / 100}
			m[bp] = r
		}
		if e.Type == TypeIncome {
			r.Sales += e.Amount
			r.TaxCollected += e.Tax
		} else {
			r.Purchases += e.Amount
			r.TaxPaid += e.Tax
		}
	}
	out := make([]RateTotal, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rate < out[j].Rate })
	return out
}

// decimalsFor wraps currency.Decimals.
func decimalsFor(code string) int { return currency.Decimals(code) }
