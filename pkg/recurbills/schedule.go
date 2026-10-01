package recurbills

import (
	"fmt"
	"sort"
	"time"
)

// Frequencies.
const (
	FreqWeekly    = "weekly"
	FreqMonthly   = "monthly"
	FreqQuarterly = "quarterly"
	FreqYearly    = "yearly"
	FreqCustom    = "custom" // every Interval months
)

// End conditions.
const (
	EndNever = "never"
	EndAfter = "after" // after EndAfter occurrences
	EndOn    = "on"    // last occurrence on or before EndDate
)

const dayFmt = "2006-01-02"

// Day truncates t to a UTC calendar date.
func Day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// ParseDay parses YYYY-MM-DD.
func ParseDay(s string) (time.Time, bool) {
	if len(s) >= 10 {
		if t, err := time.Parse(dayFmt, s[:10]); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Rule is the part of a recurring bill that decides its dates.
type Rule struct {
	Frequency string
	Interval  int // months for "custom" (≥1); ignored otherwise
	Start     time.Time
	EndMode   string
	EndAfter  int
	EndDate   *time.Time
}

// RuleOf extracts the rule of a recurring bill.
func RuleOf(r *RecurringBill) Rule {
	return Rule{Frequency: r.Frequency, Interval: r.Interval, Start: Day(r.StartDate), EndMode: r.EndMode, EndAfter: r.EndAfter, EndDate: r.EndDate}
}

// Months is the step in months (0 for weekly).
func (r Rule) Months() int {
	switch r.Frequency {
	case FreqWeekly:
		return 0
	case FreqQuarterly:
		return 3
	case FreqYearly:
		return 12
	case FreqCustom:
		if r.Interval < 1 {
			return 1
		}
		return r.Interval
	}
	return 1
}

// Validate checks the rule.
func (r Rule) Validate() error {
	switch r.Frequency {
	case FreqWeekly, FreqMonthly, FreqQuarterly, FreqYearly:
	case FreqCustom:
		if r.Interval < 1 || r.Interval > 60 {
			return fmt.Errorf("repeat every 1 to 60 months")
		}
	default:
		return fmt.Errorf("choose how often the bill repeats")
	}
	if r.Start.IsZero() {
		return fmt.Errorf("choose a start date")
	}
	switch r.EndMode {
	case "", EndNever:
	case EndAfter:
		if r.EndAfter < 1 || r.EndAfter > 1000 {
			return fmt.Errorf("end after 1 to 1000 bills")
		}
	case EndOn:
		if r.EndDate == nil || Day(*r.EndDate).Before(r.Start) {
			return fmt.Errorf("the end date must be on or after the start date")
		}
	default:
		return fmt.Errorf("choose when the schedule ends")
	}
	return nil
}

// AddMonthsClamped adds n months keeping the anchor day, clamped to the end
// of shorter months: 31 Jan + 1 → 28/29 Feb, + 2 → 31 Mar (never drifts).
func AddMonthsClamped(anchor time.Time, n int) time.Time {
	y, m := anchor.Year(), int(anchor.Month())-1+n
	y += m / 12
	m %= 12
	if m < 0 {
		m += 12
		y--
	}
	first := time.Date(y, time.Month(m+1), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	d := anchor.Day()
	if d > last {
		d = last
	}
	return time.Date(y, time.Month(m+1), d, 0, 0, 0, 0, time.UTC)
}

// At returns the k-th (0-based) occurrence date, ignoring the end condition.
func (r Rule) At(k int) time.Time {
	s := Day(r.Start)
	if r.Frequency == FreqWeekly {
		return s.AddDate(0, 0, 7*k)
	}
	return AddMonthsClamped(s, k*r.Months())
}

// Ended reports whether the k-th occurrence (date d) is past the end.
func (r Rule) Ended(k int, d time.Time) bool {
	switch r.EndMode {
	case EndAfter:
		return k >= r.EndAfter
	case EndOn:
		return r.EndDate != nil && d.After(Day(*r.EndDate))
	}
	return false
}

// Occ is one computed occurrence.
type Occ struct {
	Index int
	Date  time.Time
}

// Between lists occurrences with from ≤ date ≤ to (at most max; 0 = 500).
func (r Rule) Between(from, to time.Time, max int) []Occ {
	if max <= 0 {
		max = 500
	}
	from, to = Day(from), Day(to)
	out := []Occ{}
	// Skip ahead roughly to "from" without walking every period.
	k := 0
	if from.After(r.Start) {
		if r.Frequency == FreqWeekly {
			k = int(from.Sub(r.Start).Hours()/24/7) - 1
		} else {
			months := (from.Year()-r.Start.Year())*12 + int(from.Month()) - int(r.Start.Month())
			k = months/r.Months() - 1
		}
		if k < 0 {
			k = 0
		}
	}
	for ; len(out) < max; k++ {
		d := r.At(k)
		if r.Ended(k, d) || d.After(to) {
			break
		}
		if !d.Before(from) {
			out = append(out, Occ{Index: k, Date: d})
		}
	}
	return out
}

// Next returns the first occurrence on or after from (ok false when ended).
func (r Rule) Next(from time.Time) (Occ, bool) {
	l := r.Between(from, Day(from).AddDate(30, 0, 0), 1)
	if len(l) == 0 {
		return Occ{}, false
	}
	return l[0], true
}

// Describe is a human summary, e.g. "Monthly on the 25th" or "Every 2 months".
func (r Rule) Describe() string {
	s := ""
	switch r.Frequency {
	case FreqWeekly:
		s = "Weekly on " + r.Start.Weekday().String()
	case FreqMonthly:
		s = "Monthly on the " + ordinal(r.Start.Day())
	case FreqQuarterly:
		s = "Every 3 months on the " + ordinal(r.Start.Day())
	case FreqYearly:
		s = "Yearly on " + r.Start.Format("2 January")
	case FreqCustom:
		if r.Months() == 1 {
			s = "Monthly on the " + ordinal(r.Start.Day())
		} else {
			s = fmt.Sprintf("Every %d months on the %s", r.Months(), ordinal(r.Start.Day()))
		}
	}
	switch r.EndMode {
	case EndAfter:
		s += fmt.Sprintf(", %d times", r.EndAfter)
	case EndOn:
		if r.EndDate != nil {
			s += ", until " + r.EndDate.Format("2 Jan 2006")
		}
	}
	return s
}

func ordinal(d int) string {
	suf := "th"
	if d%100 < 11 || d%100 > 13 {
		switch d % 10 {
		case 1:
			suf = "st"
		case 2:
			suf = "nd"
		case 3:
			suf = "rd"
		}
	}
	return fmt.Sprintf("%d%s", d, suf)
}

// ---------------------------------------------------------------- generation plan

// GenerateDate is the day the bill for an occurrence is created: AdvanceDays
// before the bill date, and early enough that the first reminder (largest
// lead time before the due date) has a bill to point at.
func GenerateDate(billDate time.Time, dueDays, advanceDays int, leads []int) time.Time {
	g := Day(billDate).AddDate(0, 0, -advanceDays)
	due := Day(billDate).AddDate(0, 0, dueDays)
	for _, l := range leads {
		if d := due.AddDate(0, 0, -l); d.Before(g) {
			g = d
		}
	}
	return g
}

// Due lists the occurrences that should exist as bills by today: generation
// date ≤ today, not before notBefore (pause / creation boundaries), limited to
// max (catch-up is capped so a long outage can't create hundreds of bills).
func Due(r Rule, today time.Time, dueDays, advanceDays int, leads []int, notBefore *time.Time, max int) []Occ {
	today = Day(today)
	// The furthest bill date that can be due for generation today.
	horizon := today.AddDate(0, 0, advanceDays)
	for _, l := range leads {
		if h := today.AddDate(0, 0, l-dueDays); h.After(horizon) {
			horizon = h
		}
	}
	from := r.Start
	if notBefore != nil && Day(*notBefore).After(from) {
		from = Day(*notBefore)
	}
	out := []Occ{}
	for _, o := range r.Between(from, horizon, 0) {
		if GenerateDate(o.Date, dueDays, advanceDays, leads).After(today) {
			continue
		}
		out = append(out, o)
	}
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// ---------------------------------------------------------------- reminder stages

// ReminderPolicy is when reminders go out for one bill.
type ReminderPolicy struct {
	Leads        []int // days before the due date (e.g. 7, 1)
	OnDue        bool  // on the due date
	OverdueEvery int   // then every N days while unpaid (0 = never)
	OverdueMax   int   // at most this many overdue reminders
}

// Stage is one reminder step.
type Stage struct {
	Key  string    // before_7 | before_1 | due | overdue_1 …
	Date time.Time // the day it becomes due
	Days int       // days until the due date (negative when overdue)
	Kind string    // upcoming | due | overdue
}

// Stages lists every stage for a due date, in date order.
func (p ReminderPolicy) Stages(due time.Time) []Stage {
	due = Day(due)
	seen := map[int]bool{}
	leads := []int{}
	for _, l := range p.Leads {
		if l > 0 && l <= 90 && !seen[l] {
			seen[l] = true
			leads = append(leads, l)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(leads)))
	out := []Stage{}
	for _, l := range leads {
		out = append(out, Stage{Key: fmt.Sprintf("before_%d", l), Date: due.AddDate(0, 0, -l), Days: l, Kind: "upcoming"})
	}
	if p.OnDue {
		out = append(out, Stage{Key: "due", Date: due, Days: 0, Kind: "due"})
	}
	if p.OverdueEvery > 0 {
		for i := 1; i <= p.OverdueMax; i++ {
			out = append(out, Stage{Key: fmt.Sprintf("overdue_%d", i), Date: due.AddDate(0, 0, i*p.OverdueEvery), Days: -i * p.OverdueEvery, Kind: "overdue"})
		}
	}
	return out
}

// Current returns the latest stage reached by today (ok false when none has
// been reached yet or every stage is over). Only the latest stage is ever
// sent, so a server that was down never fires a burst of stale reminders.
// A stage stays current until the next one is reached; after the last
// stage, nothing more is sent.
func (p ReminderPolicy) Current(due, today time.Time) (Stage, bool) {
	today = Day(today)
	st := p.Stages(due)
	var cur Stage
	found := false
	for i, s := range st {
		if s.Date.After(today) {
			break
		}
		// Expired: a later stage was already reached.
		if i+1 < len(st) && !st[i+1].Date.After(today) {
			continue
		}
		cur, found = s, true
	}
	if found {
		// Wording uses the real distance to the due date.
		cur.Days = int(Day(due).Sub(today).Hours() / 24)
	}
	return cur, found
}
