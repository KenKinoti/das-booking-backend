// Package reqtz resolves the caller's time zone so "today", "this week" and
// "this month" figures use the user's calendar rather than the server's (the
// server usually runs in UTC while users are elsewhere).
//
// The browser sends its IANA zone as the `tz` query parameter (or the
// X-Timezone header). Unknown or missing zones fall back to the server zone.
package reqtz

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Location returns the request's time zone.
func Location(c *gin.Context) *time.Location {
	if c != nil {
		for _, v := range []string{c.Query("tz"), c.GetHeader("X-Timezone")} {
			if v = strings.TrimSpace(v); v != "" {
				if l, err := time.LoadLocation(v); err == nil {
					return l
				}
			}
		}
	}
	return time.Local
}

// Now returns the current time in the request's time zone.
func Now(c *gin.Context) time.Time { return time.Now().In(Location(c)) }

// Today returns midnight today in the request's time zone.
func Today(c *gin.Context) time.Time {
	n := Now(c)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())
}

// MonthStart returns midnight on the 1st of the current month in the request's zone.
func MonthStart(c *gin.Context) time.Time {
	n := Now(c)
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, n.Location())
}

// WeekStart returns midnight on Monday of the current week in the request's zone.
func WeekStart(c *gin.Context) time.Time {
	t := Today(c)
	return t.AddDate(0, 0, -int((t.Weekday()+6)%7))
}

// DateString is today's calendar date (YYYY-MM-DD) in the request's zone.
func DateString(c *gin.Context) string { return Now(c).Format("2006-01-02") }
