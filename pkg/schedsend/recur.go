package schedsend

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	_ "time/tzdata" // time zones on hosts without zoneinfo
)

// LocalLayout is the wall-clock format of LocalSendAt.
const LocalLayout = "2006-01-02T15:04"

// DateLayout is a plain date.
const DateLayout = "2006-01-02"

// ---------------------------------------------------------------- clock

var (
	clockOffset atomic.Int64 // nanoseconds added to the wall clock (test clock)
	baseNow     = time.Now
)

// Now is the package clock: the wall clock plus the test offset
// (SCHEDSEND_TEST_CLOCK=1 only).
func Now() time.Time { return baseNow().UTC().Add(time.Duration(clockOffset.Load())) }

// ClockOffset is the current test offset.
func ClockOffset() time.Duration { return time.Duration(clockOffset.Load()) }

// SetClockOffset moves the package clock (tests).
func SetClockOffset(d time.Duration) { clockOffset.Store(int64(d)) }

// SetNow replaces the wall clock (unit tests); nil restores time.Now.
func SetNow(f func() time.Time) {
	if f == nil {
		f = time.Now
	}
	baseNow = f
}

// ---------------------------------------------------------------- zones

// LoadZone loads an IANA time zone ("" = Africa/Nairobi).
func LoadZone(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		tz = DefaultTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, errors.New("unknown time zone " + tz)
	}
	return loc, nil
}

// ParseLocal reads a wall-clock time ("2026-11-19T08:00") in tz and returns
// it in UTC. A time that does not exist on that day (a daylight-saving gap)
// moves forward by the gap, as clocks do.
func ParseLocal(s, tz string) (time.Time, error) {
	loc, err := LoadZone(tz)
	if err != nil {
		return time.Time{}, err
	}
	s = strings.TrimSpace(s)
	for _, f := range []string{LocalLayout, "2006-01-02T15:04:05", "2006-01-02 15:04", DateLayout} {
		if t, err := time.Parse(f, s); err == nil {
			return wall(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), loc), nil
		}
	}
	return time.Time{}, errors.New("use a date and time like 2026-11-19T08:00")
}

// FormatLocal is t as wall-clock text in tz.
func FormatLocal(t time.Time, tz string) string {
	loc, err := LoadZone(tz)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format(LocalLayout)
}

func daysIn(y int, m time.Month) int { return time.Date(y, m+1, 0, 12, 0, 0, 0, time.UTC).Day() }

// civil is a calendar date (no time zone).
type civil struct {
	Y int
	M time.Month
	D int
}

func civilOf(t time.Time) civil { return civil{t.Year(), t.Month(), t.Day()} }

func (c civil) t() time.Time { return time.Date(c.Y, c.M, c.D, 12, 0, 0, 0, time.UTC) }

func (c civil) addDays(n int) civil { return civilOf(c.t().AddDate(0, 0, n)) }

func (c civil) weekday() time.Weekday { return c.t().Weekday() }

func (c civil) before(o civil) bool { return c.t().Before(o.t()) }

func (c civil) String() string { return fmt.Sprintf("%04d-%02d-%02d", c.Y, c.M, c.D) }

func daysBetween(a, b civil) int { return int(b.t().Sub(a.t()).Hours() / 24) }

// monday of the week containing c (weeks start on Monday).
func monday(c civil) civil { return c.addDays(-((int(c.weekday()) + 6) % 7)) }

// ---------------------------------------------------------------- recurrence

// Normalize fills defaults and checks a recurrence.
func (r *Recurrence) Normalize() error {
	r.Freq = strings.ToLower(strings.TrimSpace(r.Freq))
	if r.Freq == "" {
		r.Freq = FreqNone
	}
	switch r.Freq {
	case FreqNone, FreqDaily, FreqWeekly, FreqMonthly, FreqYearly:
	default:
		return fmt.Errorf("repeat must be none, daily, weekly, monthly or yearly")
	}
	if r.Interval <= 0 {
		r.Interval = 1
	}
	if r.Interval > 365 {
		return fmt.Errorf("repeat every %d is too long", r.Interval)
	}
	seen := map[int]bool{}
	days := []int{}
	for _, d := range r.Weekdays {
		if d < 0 || d > 6 {
			return fmt.Errorf("weekdays are 0 (Sunday) to 6 (Saturday)")
		}
		if !seen[d] {
			seen[d] = true
			days = append(days, d)
		}
	}
	sort.Ints(days)
	r.Weekdays = days
	if r.MonthDay < -1 || r.MonthDay > 31 {
		return fmt.Errorf("day of the month must be 1–31 or the last day")
	}
	r.Until = strings.TrimSpace(r.Until)
	if r.Until != "" {
		if _, err := time.Parse(DateLayout, r.Until); err != nil {
			return fmt.Errorf("the end date must look like 2027-01-31")
		}
	}
	if r.Count < 0 || r.Count > 5000 {
		return fmt.Errorf("repeat count must be between 1 and 5000")
	}
	if r.Freq == FreqNone {
		r.Interval, r.Weekdays, r.MonthDay, r.Until, r.Count = 1, nil, 0, "", 0
	}
	return nil
}

// Repeats reports whether there is more than one occurrence.
func (r Recurrence) Repeats() bool { return r.Freq != "" && r.Freq != FreqNone }

// monthTarget is the day a monthly rule sends in year y, month m.
func (r Recurrence) monthTarget(anchor civil, y int, m time.Month) int {
	n := daysIn(y, m)
	d := r.MonthDay
	switch {
	case d == -1:
		return n
	case d == 0:
		d = anchor.D
	}
	if d > n {
		d = n
	}
	return d
}

// matches reports whether date c is an occurrence date of r started at anchor.
func (r Recurrence) matches(anchor, c civil) bool {
	if c.before(anchor) {
		return false
	}
	iv := r.Interval
	if iv <= 0 {
		iv = 1
	}
	switch r.Freq {
	case FreqDaily:
		return daysBetween(anchor, c)%iv == 0
	case FreqWeekly:
		days := r.Weekdays
		if len(days) == 0 {
			days = []int{int(anchor.weekday())}
		}
		ok := false
		for _, d := range days {
			if int(c.weekday()) == d {
				ok = true
			}
		}
		return ok && (daysBetween(monday(anchor), monday(c))/7)%iv == 0
	case FreqMonthly:
		months := (c.Y-anchor.Y)*12 + int(c.M) - int(anchor.M)
		return months%iv == 0 && c.D == r.monthTarget(anchor, c.Y, c.M)
	case FreqYearly:
		years := c.Y - anchor.Y
		if years%iv != 0 || c.M != anchor.M {
			return false
		}
		d := anchor.D
		if n := daysIn(c.Y, c.M); d > n {
			d = n
		}
		return c.D == d
	}
	return c == anchor
}

// Schedule is a start (wall clock in a zone) and a recurrence.
type Schedule struct {
	Start  string // LocalLayout
	TZ     string
	Repeat Recurrence
}

func (s Schedule) anchor() (civil, int, int, *time.Location, error) {
	loc, err := LoadZone(s.TZ)
	if err != nil {
		return civil{}, 0, 0, nil, err
	}
	// Read the wall clock as written (no zone): a start inside a
	// daylight-saving gap keeps its hour; wall() places it.
	a, err := time.Parse(LocalLayout, strings.TrimSpace(s.Start))
	if err != nil {
		return civil{}, 0, 0, nil, errors.New("use a date and time like 2026-11-19T08:00")
	}
	return civil{a.Year(), a.Month(), a.Day()}, a.Hour(), a.Minute(), loc, nil
}

// at is the instant of date c at hh:mm local.
func at(c civil, hh, mm int, loc *time.Location) time.Time { return wall(c.Y, c.M, c.D, hh, mm, loc) }

// wall is the instant of a wall-clock time in loc (UTC). A time that does
// not exist that day (the hour skipped when daylight saving starts) moves
// forward by the gap — 02:30 becomes 03:30 — as clocks do; Go alone would
// pick the earlier offset and send an hour early.
func wall(y int, m time.Month, d, hh, mm int, loc *time.Location) time.Time {
	t := time.Date(y, m, d, hh, mm, 0, 0, loc)
	if l := t.In(loc); l.Hour() == hh && l.Minute() == mm {
		return t.UTC()
	}
	_, before := time.Date(y, m, d-1, 12, 0, 0, 0, loc).Zone()
	return time.Date(y, m, d, hh, mm, 0, 0, time.FixedZone("", before)).UTC()
}

// stepFrom is the next date ≥ from that matches (searching a bounded range).
func (s Schedule) nextDate(a, from civil) (civil, bool) {
	r := s.Repeat
	if !r.Repeats() {
		if from.before(a) || from == a {
			return a, true
		}
		return civil{}, false
	}
	switch r.Freq {
	case FreqMonthly:
		// Walk month by month (at most ~ 100 years).
		y, m := from.Y, from.M
		for i := 0; i < 1300; i++ {
			c := civil{y, m, r.monthTarget(a, y, m)}
			if !c.before(from) && r.matches(a, c) {
				return c, true
			}
			m++
			if m > 12 {
				m, y = 1, y+1
			}
		}
		return civil{}, false
	case FreqYearly:
		for y := from.Y; y < from.Y+1000; y++ {
			d := a.D
			if n := daysIn(y, a.M); d > n {
				d = n
			}
			c := civil{y, a.M, d}
			if !c.before(from) && r.matches(a, c) {
				return c, true
			}
		}
		return civil{}, false
	}
	limit := 7*r.Interval + 8
	if r.Freq == FreqDaily {
		limit = r.Interval + 1
	}
	c := from
	if c.before(a) {
		c = a
	}
	for i := 0; i <= limit; i++ {
		if r.matches(a, c) {
			return c, true
		}
		c = c.addDays(1)
	}
	return civil{}, false
}

// First is the first send time.
func (s Schedule) First() (time.Time, error) {
	a, hh, mm, loc, err := s.anchor()
	if err != nil {
		return time.Time{}, err
	}
	c, ok := s.nextDate(a, a)
	if !ok {
		return time.Time{}, errors.New("this schedule never sends")
	}
	t := at(c, hh, mm, loc)
	if !s.within(c, 1) {
		return time.Time{}, errors.New("the end date is before the first send")
	}
	return t, nil
}

// within: occurrence number n (1-based) on date c is inside Until / Count.
func (s Schedule) within(c civil, n int) bool {
	r := s.Repeat
	if r.Count > 0 && n > r.Count {
		return false
	}
	if r.Until != "" && c.String() > r.Until {
		return false
	}
	return true
}

// After is the occurrence that follows the one at prev (a send time of this
// schedule); done is the number of occurrences already handled (for Count).
// ok is false when the schedule has ended.
func (s Schedule) After(prev time.Time, done int) (time.Time, bool) {
	if !s.Repeat.Repeats() {
		return time.Time{}, false
	}
	a, hh, mm, loc, err := s.anchor()
	if err != nil {
		return time.Time{}, false
	}
	pc := civilOf(prev.In(loc))
	c, ok := s.nextDate(a, pc.addDays(1))
	if !ok || !s.within(c, done+1) {
		return time.Time{}, false
	}
	return at(c, hh, mm, loc), true
}

// Upcoming lists the next n send times from the start (composer preview).
func (s Schedule) Upcoming(n int) []time.Time {
	out := []time.Time{}
	t, err := s.First()
	if err != nil {
		return out
	}
	out = append(out, t)
	for len(out) < n {
		nx, ok := s.After(t, len(out))
		if !ok {
			break
		}
		out = append(out, nx)
		t = nx
	}
	return out
}

// Describe is the recurrence in words ("Every month on the last day").
func (r Recurrence) Describe(start string) string {
	if !r.Repeats() {
		return "Once"
	}
	every := func(one, many string) string {
		if r.Interval <= 1 {
			return "Every " + one
		}
		return fmt.Sprintf("Every %d %s", r.Interval, many)
	}
	var s string
	switch r.Freq {
	case FreqDaily:
		s = every("day", "days")
	case FreqWeekly:
		names := []string{}
		for _, d := range r.Weekdays {
			names = append(names, time.Weekday(d).String()[:3])
		}
		s = every("week", "weeks")
		if len(names) > 0 {
			s += " on " + strings.Join(names, ", ")
		}
	case FreqMonthly:
		s = every("month", "months")
		switch {
		case r.MonthDay == -1:
			s += " on the last day"
		case r.MonthDay > 0:
			s += " on day " + fmt.Sprint(r.MonthDay)
		}
	case FreqYearly:
		s = every("year", "years")
	}
	if r.StopWhenPaid {
		s += " until paid"
	}
	if r.Until != "" {
		s += " until " + r.Until
	}
	if r.Count > 0 {
		s += fmt.Sprintf(" (%d times)", r.Count)
	}
	return s
}

// ---------------------------------------------------------------- quick picks

// QuickPick is a ready-made send time.
type QuickPick struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Local string `json:"local"` // LocalLayout in the zone
	At    string `json:"at"`    // RFC 3339 (UTC)
}

// QuickPicks: tomorrow 8:00, next Monday 8:00 and in 1 hour, in tz at now.
func QuickPicks(now time.Time, tz string) []QuickPick {
	loc, err := LoadZone(tz)
	if err != nil {
		loc, _ = LoadZone("")
	}
	n := now.In(loc)
	today := civilOf(n)
	tom := today.addDays(1)
	// Next Monday: the Monday after today (a week ahead when today is Monday).
	days := (8 - int(today.weekday())) % 7
	if days == 0 {
		days = 7
	}
	mon := today.addDays(days)
	hour := n.Add(time.Hour)
	// Round "in 1 hour" up to the next 5 minutes.
	if r := hour.Minute() % 5; r != 0 || hour.Second() > 0 {
		hour = hour.Add(time.Duration(5-r)*time.Minute - time.Duration(hour.Second())*time.Second - time.Duration(hour.Nanosecond()))
	}
	mk := func(key, label string, t time.Time) QuickPick {
		return QuickPick{Key: key, Label: label, Local: t.In(loc).Format(LocalLayout), At: t.UTC().Format(time.RFC3339)}
	}
	return []QuickPick{
		mk("tomorrow_8", "Tomorrow 8:00", at(tom, 8, 0, loc)),
		mk("monday_8", "Next Monday 8:00", at(mon, 8, 0, loc)),
		mk("in_1h", "In 1 hour", hour),
	}
}
