// Package analytics serves organisation-scoped business analytics for the
// dashboard and the reports pages. Everything is computed with a handful of
// SQL aggregates over the existing tables (invoicing, payments, POS,
// bookings, customers, products, services). Missing data yields zeros and
// empty arrays, never errors.
package analytics

import (
	"fmt"
	"strings"
	"time"
)

const dayFmt = "2006-01-02"

// Period is an inclusive date range [From, To] in the caller's time zone.
type Period struct {
	From time.Time // midnight, UTC-labelled calendar date
	To   time.Time // midnight, UTC-labelled calendar date (inclusive)
}

// Days returns the number of calendar days in the period.
func (p Period) Days() int { return int(p.To.Sub(p.From).Hours()/24) + 1 }

// Contains reports whether the calendar date d falls inside the period.
func (p Period) Contains(d time.Time) bool {
	d = dateOnly(d)
	return !d.Before(p.From) && !d.After(p.To)
}

// Bounds returns [start, end) instants for timestamp columns in loc.
func (p Period) Bounds(loc *time.Location) (time.Time, time.Time) {
	s := time.Date(p.From.Year(), p.From.Month(), p.From.Day(), 0, 0, 0, 0, loc)
	e := time.Date(p.To.Year(), p.To.Month(), p.To.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	return s, e
}

func dateOnly(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func parseDay(s string) (time.Time, bool) {
	t, err := time.Parse(dayFmt, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// resolvePeriods works out the current and comparison periods.
// range: 7d, 30d, 90d, 12m, mtd, qtd, ytd (ignored when from/to are given).
// compare: previous (default), year, none.
func resolvePeriods(today time.Time, from, to, rng, compare string) (Period, *Period, error) {
	var cur Period
	f, okF := parseDay(from)
	t, okT := parseDay(to)
	switch {
	case okF && okT:
		cur = Period{From: f, To: t}
	case okF:
		cur = Period{From: f, To: today}
	default:
		cur.To = today
		switch strings.ToLower(rng) {
		case "7d":
			cur.From = today.AddDate(0, 0, -6)
		case "90d":
			cur.From = today.AddDate(0, 0, -89)
		case "12m", "1y":
			cur.From = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -11, 0)
		case "mtd":
			cur.From = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
		case "qtd":
			q := (int(today.Month()) - 1) / 3
			cur.From = time.Date(today.Year(), time.Month(q*3+1), 1, 0, 0, 0, 0, time.UTC)
		case "ytd":
			cur.From = time.Date(today.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		default:
			cur.From = today.AddDate(0, 0, -29)
		}
	}
	if cur.To.Before(cur.From) {
		return cur, nil, fmt.Errorf("'from' must be on or before 'to'")
	}
	if cur.Days() > 3*366 {
		return cur, nil, fmt.Errorf("date range too large (max 3 years)")
	}
	switch strings.ToLower(compare) {
	case "none", "off", "false", "0":
		return cur, nil, nil
	case "year", "yoy":
		p := Period{From: cur.From.AddDate(-1, 0, 0), To: cur.To.AddDate(-1, 0, 0)}
		return cur, &p, nil
	default:
		days := cur.Days()
		pTo := cur.From.AddDate(0, 0, -1)
		p := Period{From: pTo.AddDate(0, 0, -(days - 1)), To: pTo}
		return cur, &p, nil
	}
}

// pickInterval chooses a bucket size for the series.
func pickInterval(req string, days int) string {
	switch req {
	case "day", "week", "month":
		return req
	}
	switch {
	case days <= 62:
		return "day"
	case days <= 200:
		return "week"
	default:
		return "month"
	}
}

// bucketStart maps a calendar date to the start of its bucket.
func bucketStart(d time.Time, interval string) time.Time {
	d = dateOnly(d)
	switch interval {
	case "week":
		wd := (int(d.Weekday()) + 6) % 7 // Monday = 0
		return d.AddDate(0, 0, -wd)
	case "month":
		return time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return d
}

func nextBucket(d time.Time, interval string) time.Time {
	switch interval {
	case "week":
		return d.AddDate(0, 0, 7)
	case "month":
		return d.AddDate(0, 1, 0)
	}
	return d.AddDate(0, 0, 1)
}

// buckets lists the bucket start dates covering the period.
func buckets(p Period, interval string) []time.Time {
	out := []time.Time{}
	for b := bucketStart(p.From, interval); !b.After(p.To); b = nextBucket(b, interval) {
		out = append(out, b)
		if len(out) > 1200 {
			break
		}
	}
	return out
}
