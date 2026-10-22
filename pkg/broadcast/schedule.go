package broadcast

import (
	"errors"
	"strings"
	"sync/atomic"
	"time"
	_ "time/tzdata" // time zones on hosts without zoneinfo
)

// LocalLayout is the wall-clock format of LocalSendAt / LocalExpiresAt.
const LocalLayout = "2006-01-02T15:04"

// DateLayout is a plain date.
const DateLayout = "2006-01-02"

// ---------------------------------------------------------------- clock

var (
	clockOffset atomic.Int64 // nanoseconds added to the wall clock (tests)
	baseNow     = time.Now
)

// Now is the package clock: the wall clock plus the test offset.
func Now() time.Time { return baseNow().UTC().Add(time.Duration(clockOffset.Load())) }

// ClockOffset is the current test offset.
func ClockOffset() time.Duration { return time.Duration(clockOffset.Load()) }

// SetClockOffset moves the package clock (BROADCAST_CLOCK_OFFSET, the test
// hook and unit tests).
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

// ParseLocal reads a wall-clock time ("2026-10-20T08:00", seconds optional)
// in a time zone and returns it in UTC.
func ParseLocal(s, tz string) (time.Time, error) {
	loc, err := LoadZone(tz)
	if err != nil {
		return time.Time{}, err
	}
	s = strings.TrimSpace(s)
	for _, f := range []string{LocalLayout, "2006-01-02T15:04:05", "2006-01-02 15:04", DateLayout} {
		if t, err := time.ParseInLocation(f, s, loc); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("use a date and time like 2026-10-20T08:00")
}

// FormatLocal is t as wall-clock text in tz.
func FormatLocal(t time.Time, tz string) string {
	loc, err := LoadZone(tz)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format(LocalLayout)
}

func daysIn(y int, m time.Month, loc *time.Location) int {
	return time.Date(y, m+1, 0, 12, 0, 0, 0, loc).Day()
}

// Occurrence is the k-th send time (k = 0 is the first) of a schedule that
// starts at anchor (wall clock in tz) and repeats weekly or monthly at the
// same local time. Monthly on the 29th–31st falls on the last day of shorter
// months without drifting (31 Jan → 28 Feb → 31 Mar).
func Occurrence(anchor, tz, repeat string, k int) (time.Time, error) {
	loc, err := LoadZone(tz)
	if err != nil {
		return time.Time{}, err
	}
	a, err := time.ParseInLocation(LocalLayout, strings.TrimSpace(anchor), loc)
	if err != nil {
		return time.Time{}, errors.New("use a date and time like 2026-10-20T08:00")
	}
	switch repeat {
	case RepeatWeekly:
		return time.Date(a.Year(), a.Month(), a.Day()+7*k, a.Hour(), a.Minute(), 0, 0, loc).UTC(), nil
	case RepeatMonthly:
		y, m := a.Year(), a.Month()+time.Month(k)
		first := time.Date(y, m, 1, 12, 0, 0, 0, loc)
		d := a.Day()
		if n := daysIn(first.Year(), first.Month(), loc); d > n {
			d = n
		}
		return time.Date(first.Year(), first.Month(), d, a.Hour(), a.Minute(), 0, 0, loc).UTC(), nil
	}
	if k > 0 {
		return time.Time{}, errors.New("does not repeat")
	}
	return a.UTC(), nil
}

// withinUntil: the occurrence's local date is on or before until (a date in
// tz; "" = no end).
func withinUntil(t time.Time, tz, until string) bool {
	if strings.TrimSpace(until) == "" {
		return true
	}
	loc, err := LoadZone(tz)
	if err != nil {
		return false
	}
	return t.In(loc).Format(DateLayout) <= strings.TrimSpace(until)
}

// NextSend is the first occurrence after the ones already sent that is
// later than now (missed occurrences are skipped, never sent in a burst);
// nil when the schedule has ended.
func NextSend(a *Announcement, now time.Time) *time.Time {
	if a.Recurrence != RepeatWeekly && a.Recurrence != RepeatMonthly {
		return nil
	}
	for k := a.Occurrences; k < a.Occurrences+2000; k++ {
		t, err := Occurrence(a.LocalSendAt, a.Timezone, a.Recurrence, k)
		if err != nil {
			return nil
		}
		if !withinUntil(t, a.Timezone, a.RepeatUntil) {
			return nil
		}
		if a.ExpiresAt != nil && !t.Before(*a.ExpiresAt) {
			return nil
		}
		if t.After(now) {
			return &t
		}
	}
	return nil
}

// UpcomingSends lists the next n send times (for the composer's preview).
func UpcomingSends(anchor, tz, repeat, until string, n int) []time.Time {
	out := []time.Time{}
	for k := 0; len(out) < n && k < 400; k++ {
		t, err := Occurrence(anchor, tz, repeat, k)
		if err != nil || !withinUntil(t, tz, until) {
			break
		}
		out = append(out, t)
		if repeat != RepeatWeekly && repeat != RepeatMonthly {
			break
		}
	}
	return out
}

// DaysUntil is the number of calendar days from today to day (both dates;
// negative when day has passed).
func DaysUntil(today, day time.Time) int {
	a := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}
