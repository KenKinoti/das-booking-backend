package recurbills

import (
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := time.Parse(dayFmt, s)
	if err != nil {
		panic(err)
	}
	return t
}

func dates(l []Occ) []string {
	out := []string{}
	for _, o := range l {
		out = append(out, o.Date.Format(dayFmt))
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestMonthEndClamping(t *testing.T) {
	r := Rule{Frequency: FreqMonthly, Start: d("2027-01-31")}
	eq(t, dates(r.Between(d("2027-01-01"), d("2027-06-30"), 0)), []string{"2027-01-31", "2027-02-28", "2027-03-31", "2027-04-30", "2027-05-31", "2027-06-30"})
	// Leap year: 31 Jan → 29 Feb, and never drifts to the 29th afterwards.
	r = Rule{Frequency: FreqMonthly, Start: d("2028-01-31")}
	eq(t, dates(r.Between(d("2028-01-01"), d("2028-04-30"), 0)), []string{"2028-01-31", "2028-02-29", "2028-03-31", "2028-04-30"})
	// 30th: February clamps, March is back on the 30th.
	r = Rule{Frequency: FreqMonthly, Start: d("2026-11-30")}
	eq(t, dates(r.Between(d("2026-11-01"), d("2027-03-31"), 0)), []string{"2026-11-30", "2026-12-30", "2027-01-30", "2027-02-28", "2027-03-30"})
	// Yearly on 29 Feb → 28 Feb in common years, 29 Feb in the next leap year.
	r = Rule{Frequency: FreqYearly, Start: d("2028-02-29")}
	eq(t, dates(r.Between(d("2028-01-01"), d("2032-12-31"), 0)), []string{"2028-02-29", "2029-02-28", "2030-02-28", "2031-02-28", "2032-02-29"})
	if got := AddMonthsClamped(d("2026-12-31"), -10); got.Format(dayFmt) != "2026-02-28" {
		t.Fatalf("negative months: %s", got.Format(dayFmt))
	}
}

func TestFrequencies(t *testing.T) {
	eq(t, dates(Rule{Frequency: FreqWeekly, Start: d("2026-10-01")}.Between(d("2026-10-01"), d("2026-10-29"), 0)),
		[]string{"2026-10-01", "2026-10-08", "2026-10-15", "2026-10-22", "2026-10-29"})
	eq(t, dates(Rule{Frequency: FreqQuarterly, Start: d("2026-08-31")}.Between(d("2026-01-01"), d("2027-09-01"), 0)),
		[]string{"2026-08-31", "2026-11-30", "2027-02-28", "2027-05-31", "2027-08-31"})
	eq(t, dates(Rule{Frequency: FreqCustom, Interval: 2, Start: d("2026-10-25")}.Between(d("2026-10-26"), d("2027-04-30"), 0)),
		[]string{"2026-12-25", "2027-02-25", "2027-04-25"})
	// Starting the window well after the start skips ahead correctly.
	eq(t, dates(Rule{Frequency: FreqMonthly, Start: d("2020-01-15")}.Between(d("2026-10-01"), d("2026-11-30"), 0)), []string{"2026-10-15", "2026-11-15"})
	eq(t, dates(Rule{Frequency: FreqWeekly, Start: d("2020-01-02")}.Between(d("2026-10-01"), d("2026-10-10"), 0)), []string{"2026-10-01", "2026-10-08"})
}

func TestEndConditions(t *testing.T) {
	r := Rule{Frequency: FreqMonthly, Start: d("2026-10-25"), EndMode: EndAfter, EndAfter: 3}
	eq(t, dates(r.Between(d("2026-01-01"), d("2030-01-01"), 0)), []string{"2026-10-25", "2026-11-25", "2026-12-25"})
	if _, ok := r.Next(d("2026-12-26")); ok {
		t.Fatal("ended schedule has a next date")
	}
	end := d("2027-01-24")
	r = Rule{Frequency: FreqMonthly, Start: d("2026-10-25"), EndMode: EndOn, EndDate: &end}
	eq(t, dates(r.Between(d("2026-01-01"), d("2030-01-01"), 0)), []string{"2026-10-25", "2026-11-25", "2026-12-25"})
	if err := (Rule{Frequency: FreqCustom, Interval: 0, Start: d("2026-10-01")}).Validate(); err == nil {
		t.Fatal("custom with interval 0 accepted")
	}
	bad := d("2026-09-01")
	if err := (Rule{Frequency: FreqMonthly, Start: d("2026-10-01"), EndMode: EndOn, EndDate: &bad}).Validate(); err == nil {
		t.Fatal("end before start accepted")
	}
	if s := (Rule{Frequency: FreqMonthly, Start: d("2026-10-25"), EndMode: EndAfter, EndAfter: 12}).Describe(); s != "Monthly on the 25th, 12 times" {
		t.Fatalf("describe: %q", s)
	}
}

func TestDueCatchUpAndAdvance(t *testing.T) {
	r := Rule{Frequency: FreqMonthly, Start: d("2026-06-25")}
	// Bill created 7 days ahead; leads 7/1 before a due date 0 days after the bill date.
	got := Due(r, d("2026-10-01"), 0, 7, []int{7, 1}, nil, 0)
	eq(t, dates(got), []string{"2026-06-25", "2026-07-25", "2026-08-25", "2026-09-25"})
	// On 18 Oct the October bill (7 days ahead) is due for creation.
	got = Due(r, d("2026-10-18"), 0, 7, []int{7, 1}, nil, 0)
	if dates(got)[len(got)-1] != "2026-10-25" {
		t.Fatalf("advance: %v", dates(got))
	}
	// A 14-day lead makes the bill exist 14 days before it is due.
	got = Due(r, d("2026-10-11"), 0, 3, []int{14}, nil, 0)
	if dates(got)[len(got)-1] != "2026-10-25" {
		t.Fatalf("lead horizon: %v", dates(got))
	}
	// notBefore (resume / creation boundary) and the catch-up cap.
	nb := d("2026-08-01")
	eq(t, dates(Due(r, d("2026-10-01"), 0, 7, nil, &nb, 0)), []string{"2026-08-25", "2026-09-25"})
	eq(t, dates(Due(r, d("2026-10-01"), 0, 7, nil, nil, 2)), []string{"2026-08-25", "2026-09-25"})
	if g := GenerateDate(d("2026-10-25"), 10, 7, []int{30}); g.Format(dayFmt) != "2026-10-05" {
		t.Fatalf("generate date %s", g.Format(dayFmt))
	}
}

func TestReminderStages(t *testing.T) {
	p := ReminderPolicy{Leads: []int{1, 7, 7}, OnDue: true, OverdueEvery: 3, OverdueMax: 3}
	due := d("2026-10-25")
	keys := []string{}
	for _, s := range p.Stages(due) {
		keys = append(keys, s.Key+"@"+s.Date.Format("01-02"))
	}
	eq(t, keys, []string{"before_7@10-18", "before_1@10-24", "due@10-25", "overdue_1@10-28", "overdue_2@10-31", "overdue_3@11-03"})
	cases := []struct {
		today, key string
		days       int
		ok         bool
	}{
		{"2026-10-10", "", 0, false},
		{"2026-10-18", "before_7", 7, true},
		{"2026-10-20", "before_7", 5, true}, // still the latest reached stage (created late)
		{"2026-10-24", "before_1", 1, true},
		{"2026-10-25", "due", 0, true},
		{"2026-10-27", "due", -2, true},
		{"2026-10-28", "overdue_1", -3, true},
		{"2026-11-01", "overdue_2", -7, true}, // missed runs never fire overdue_1 late
		{"2026-11-20", "overdue_3", -26, true},
	}
	for _, c := range cases {
		s, ok := p.Current(due, d(c.today))
		if ok != c.ok || (ok && (s.Key != c.key || s.Days != c.days)) {
			t.Fatalf("%s: got %v %+v, want %s %d", c.today, ok, s, c.key, c.days)
		}
	}
	// No overdue reminders configured.
	p2 := ReminderPolicy{Leads: []int{7}}
	if s, ok := p2.Current(due, d("2026-11-20")); !ok || s.Key != "before_7" {
		t.Fatalf("no-overdue policy: %+v %v", s, ok)
	}
}
