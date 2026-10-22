package occasions

import "time"

// Date helpers. Every occasion date is a calendar day: UTC midnight.

// Day returns the calendar day y-m-d as UTC midnight.
func Day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// DateOnly truncates t to its calendar day (in t's own location) as UTC midnight.
func DateOnly(t time.Time) time.Time { return Day(t.Year(), t.Month(), t.Day()) }

// ParseDay parses "2006-01-02".
func ParseDay(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Easter returns Easter Sunday (Gregorian calendar) using the anonymous
// Gregorian computus (Meeus/Jones/Butcher).
func Easter(y int) time.Time {
	a := y % 19
	b := y / 100
	c := y % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := ((h + l - 7*m + 114) % 31) + 1
	return Day(y, time.Month(month), day)
}

// NthWeekday returns the n-th (1-based) weekday of a month, e.g. the second
// Sunday of May. n = -1 is the last one.
func NthWeekday(y int, m time.Month, wd time.Weekday, n int) time.Time {
	if n < 0 {
		last := Day(y, m+1, 1).AddDate(0, 0, -1)
		back := (int(last.Weekday()) - int(wd) + 7) % 7
		return last.AddDate(0, 0, -back)
	}
	first := Day(y, m, 1)
	ahead := (int(wd) - int(first.Weekday()) + 7) % 7
	return first.AddDate(0, 0, ahead+7*(n-1))
}

// BlackFriday is the day after the fourth Thursday of November (US
// Thanksgiving), now the global start of the holiday sales.
func BlackFriday(y int) time.Time {
	return NthWeekday(y, time.November, time.Thursday, 4).AddDate(0, 0, 1)
}

// CyberMonday is the Monday after Black Friday.
func CyberMonday(y int) time.Time { return BlackFriday(y).AddDate(0, 0, 3) }

func inRange(d, from, to time.Time) bool { return !d.Before(from) && !d.After(to) }

func days(from, to time.Time) int { return int(to.Sub(from).Hours()/24 + 0.5) }
