package schedsend

import (
	"strings"
	"testing"
	"time"
)

func occurrences(t *testing.T, s Schedule, n int) []string {
	t.Helper()
	out := []string{}
	for _, x := range s.Upcoming(n) {
		out = append(out, FormatLocal(x, s.TZ))
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func norm(t *testing.T, r Recurrence) Recurrence {
	t.Helper()
	if err := r.Normalize(); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMonthlyMonthEndsDoNotDrift(t *testing.T) {
	s := Schedule{Start: "2027-01-31T08:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqMonthly})}
	eq(t, occurrences(t, s, 5), []string{"2027-01-31T08:00", "2027-02-28T08:00", "2027-03-31T08:00", "2027-04-30T08:00", "2027-05-31T08:00"})
	// Leap year: the 29th of February.
	s.Start = "2028-01-31T08:00"
	eq(t, occurrences(t, s, 2), []string{"2028-01-31T08:00", "2028-02-29T08:00"})
	// Day 30 every 2 months from November.
	s = Schedule{Start: "2026-11-30T09:15", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqMonthly, Interval: 2, MonthDay: 30})}
	eq(t, occurrences(t, s, 4), []string{"2026-11-30T09:15", "2027-01-30T09:15", "2027-03-30T09:15", "2027-05-30T09:15"})
}

func TestMonthlyLastDayAndFirst(t *testing.T) {
	s := Schedule{Start: "2027-01-15T08:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqMonthly, MonthDay: -1})}
	eq(t, occurrences(t, s, 4), []string{"2027-01-31T08:00", "2027-02-28T08:00", "2027-03-31T08:00", "2027-04-30T08:00"})
	// "On the 1st" starting mid-month: the next 1st.
	s = Schedule{Start: "2026-11-18T08:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqMonthly, MonthDay: 1})}
	eq(t, occurrences(t, s, 3), []string{"2026-12-01T08:00", "2027-01-01T08:00", "2027-02-01T08:00"})
	// Day 31 in a 30-day month is its last day.
	s = Schedule{Start: "2027-04-01T07:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqMonthly, MonthDay: 31})}
	eq(t, occurrences(t, s, 3), []string{"2027-04-30T07:00", "2027-05-31T07:00", "2027-06-30T07:00"})
}

func TestWeeklyOnDays(t *testing.T) {
	// Wednesday 18 Nov 2026; Mondays and Thursdays.
	s := Schedule{Start: "2026-11-18T08:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqWeekly, Weekdays: []int{4, 1}})}
	eq(t, occurrences(t, s, 5), []string{"2026-11-19T08:00", "2026-11-23T08:00", "2026-11-26T08:00", "2026-11-30T08:00", "2026-12-03T08:00"})
	// Every 2 weeks on Monday (start week counts).
	s.Repeat = norm(t, Recurrence{Freq: FreqWeekly, Interval: 2, Weekdays: []int{1}})
	eq(t, occurrences(t, s, 3), []string{"2026-11-30T08:00", "2026-12-14T08:00", "2026-12-28T08:00"})
	// No days: the start day.
	s.Repeat = norm(t, Recurrence{Freq: FreqWeekly})
	eq(t, occurrences(t, s, 3), []string{"2026-11-18T08:00", "2026-11-25T08:00", "2026-12-02T08:00"})
}

func TestDailyCountUntilYearly(t *testing.T) {
	s := Schedule{Start: "2026-11-18T08:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqDaily, Interval: 7, Count: 3})}
	eq(t, occurrences(t, s, 10), []string{"2026-11-18T08:00", "2026-11-25T08:00", "2026-12-02T08:00"})
	s.Repeat = norm(t, Recurrence{Freq: FreqDaily, Until: "2026-11-20"})
	eq(t, occurrences(t, s, 10), []string{"2026-11-18T08:00", "2026-11-19T08:00", "2026-11-20T08:00"})
	s = Schedule{Start: "2028-02-29T10:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqYearly})}
	eq(t, occurrences(t, s, 3), []string{"2028-02-29T10:00", "2029-02-28T10:00", "2030-02-28T10:00"})
	// One-off.
	s = Schedule{Start: "2026-11-19T08:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{})}
	eq(t, occurrences(t, s, 3), []string{"2026-11-19T08:00"})
	if _, ok := s.After(s.Upcoming(1)[0], 1); ok {
		t.Fatal("a one-off has no next occurrence")
	}
	// The end date before the start: never sends.
	s = Schedule{Start: "2026-11-19T08:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqDaily, Until: "2026-11-01"})}
	if _, err := s.First(); err == nil {
		t.Fatal("an end date before the first send should fail")
	}
}

func TestAfterRespectsCountAlreadyDone(t *testing.T) {
	s := Schedule{Start: "2026-11-18T08:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqDaily, Count: 2})}
	first, _ := s.First()
	if _, ok := s.After(first, 1); !ok {
		t.Fatal("second occurrence expected")
	}
	if _, ok := s.After(first, 2); ok {
		t.Fatal("count of 2 reached")
	}
}

// Nairobi has no daylight saving: 08:00 is always 05:00 UTC.
func TestDSTFreeZone(t *testing.T) {
	s := Schedule{Start: "2027-03-01T08:00", TZ: "Africa/Nairobi", Repeat: norm(t, Recurrence{Freq: FreqMonthly})}
	for _, x := range s.Upcoming(12) {
		if x.UTC().Hour() != 5 || x.UTC().Minute() != 0 {
			t.Fatalf("Nairobi 08:00 should be 05:00Z, got %s", x.UTC())
		}
	}
}

// Sydney moves to daylight saving on the first Sunday of October: the local
// time stays 08:00 while the UTC time moves an hour.
func TestDSTZoneKeepsWallClock(t *testing.T) {
	s := Schedule{Start: "2027-09-27T08:00", TZ: "Australia/Sydney", Repeat: norm(t, Recurrence{Freq: FreqWeekly})}
	got := s.Upcoming(3)
	loc, _ := time.LoadLocation("Australia/Sydney")
	for _, x := range got {
		if l := x.In(loc); l.Hour() != 8 || l.Minute() != 0 {
			t.Fatalf("expected 08:00 local, got %s", l)
		}
	}
	if got[0].UTC().Hour() != 22 || got[1].UTC().Hour() != 21 {
		t.Fatalf("UTC hour should move from 22 (AEST) to 21 (AEDT): %s %s", got[0].UTC(), got[1].UTC())
	}
	// New York: daily across the March change.
	s = Schedule{Start: "2027-03-13T09:00", TZ: "America/New_York", Repeat: norm(t, Recurrence{Freq: FreqDaily})}
	got = s.Upcoming(2)
	if got[0].UTC().Hour() != 14 || got[1].UTC().Hour() != 13 {
		t.Fatalf("New York 09:00: %s %s", got[0].UTC(), got[1].UTC())
	}
	// A wall-clock time inside the spring-forward gap moves forward by the gap.
	at, err := ParseLocal("2027-03-14T02:30", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	if l := at.In(time.FixedZone("EDT", -4*3600)); l.Hour() != 3 || l.Minute() != 30 {
		t.Fatalf("02:30 in the gap should become 03:30 EDT, got %s", l)
	}
}

func TestQuickPicks(t *testing.T) {
	// Wednesday 18 Nov 2026, 23:18 in Nairobi (20:18 UTC).
	now := time.Date(2026, 11, 18, 20, 18, 30, 0, time.UTC)
	q := QuickPicks(now, "Africa/Nairobi")
	want := map[string]string{"tomorrow_8": "2026-11-19T08:00", "monday_8": "2026-11-23T08:00", "in_1h": "2026-11-19T00:20"}
	for _, p := range q {
		if want[p.Key] != p.Local {
			t.Fatalf("%s: got %s want %s", p.Key, p.Local, want[p.Key])
		}
	}
	if q[0].At != "2026-11-19T05:00:00Z" {
		t.Fatalf("tomorrow 8:00 in UTC: %s", q[0].At)
	}
	// On a Monday, "next Monday" is a week ahead; Sydney's zone is used.
	mon := time.Date(2026, 11, 22, 23, 0, 0, 0, time.UTC) // Mon 23 Nov 10:00 in Sydney
	for _, p := range QuickPicks(mon, "Australia/Sydney") {
		if p.Key == "monday_8" && p.Local != "2026-11-30T08:00" {
			t.Fatalf("next Monday from a Monday: %s", p.Local)
		}
		if p.Key == "tomorrow_8" && p.At != "2026-11-23T21:00:00Z" {
			t.Fatalf("Sydney tomorrow 08:00 (AEDT) = 21:00Z the day before, got %s", p.At)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	for _, r := range []Recurrence{{Freq: "hourly"}, {Freq: FreqWeekly, Weekdays: []int{7}}, {Freq: FreqMonthly, MonthDay: 32}, {Freq: FreqDaily, Until: "31/12/2027"}, {Freq: FreqDaily, Interval: 400}} {
		r := r
		if err := r.Normalize(); err == nil {
			t.Fatalf("%+v should be rejected", r)
		}
	}
	r := Recurrence{Freq: FreqNone, Interval: 5, Count: 3}
	if err := r.Normalize(); err != nil || r.Count != 0 || r.Interval != 1 {
		t.Fatalf("none clears the rest: %+v %v", r, err)
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]Recurrence{
		"Once":                              {Freq: FreqNone},
		"Every month on the last day":       {Freq: FreqMonthly, MonthDay: -1},
		"Every 7 days until paid (6 times)": {Freq: FreqDaily, Interval: 7, Count: 6, StopWhenPaid: true},
		"Every week on Mon, Thu":            {Freq: FreqWeekly, Weekdays: []int{1, 4}},
	}
	for want, r := range cases {
		if got := r.Describe(""); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}
