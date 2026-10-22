package subscriptions

import (
	"testing"
	"time"
)

func d(s string) time.Time {
	t, ok := ParseDay(s)
	if !ok {
		panic("bad date " + s)
	}
	return t
}

func TestAddMonthsOnMonthEnds(t *testing.T) {
	cases := []struct {
		from string
		n    int
		day  int
		want string
	}{
		{"2027-01-31", 1, 31, "2027-02-28"},
		{"2028-01-31", 1, 31, "2028-02-29"}, // leap year
		{"2027-01-31", 2, 31, "2027-03-31"}, // never drifts to the 28th
		{"2027-03-31", 1, 31, "2027-04-30"},
		{"2027-12-15", 1, 15, "2028-01-15"}, // year boundary
		{"2027-01-30", -1, 30, "2026-12-30"},
		{"2027-03-30", -1, 30, "2027-02-28"},
		{"2028-02-29", 12, 29, "2029-02-28"},
		{"2028-02-29", 48, 29, "2032-02-29"},
	}
	for _, c := range cases {
		if got := DayStr(AddMonthsOn(d(c.from), c.n, c.day)); got != c.want {
			t.Errorf("AddMonthsOn(%s, %d, %d) = %s, want %s", c.from, c.n, c.day, got, c.want)
		}
	}
	if DaysIn(2028, time.February) != 29 || DaysIn(2027, time.February) != 28 || DaysIn(2100, time.February) != 28 || DaysIn(2000, time.February) != 29 {
		t.Fatal("leap years")
	}
}

func TestScheduleMonthEndBillingDay(t *testing.T) {
	sc, err := NewSchedule(d("2027-01-31"), 31, false, PeriodMonthly)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2027-01-31", "2027-02-28", "2027-03-31", "2027-04-30", "2027-05-31", "2027-06-30"}
	p := sc.First()
	for i, w := range want {
		if DayStr(p.Start) != w {
			t.Fatalf("period %d starts %s, want %s", i, DayStr(p.Start), w)
		}
		if i+1 < len(want) && DayStr(p.End) != want[i+1] {
			t.Fatalf("period %d ends %s, want %s", i, DayStr(p.End), want[i+1])
		}
		if p.Partial || p.Fraction != 1 {
			t.Fatalf("full periods only: %+v", p)
		}
		q, ok := sc.StartingOn(p.Start)
		if !ok || !q.Start.Equal(p.Start) {
			t.Fatalf("StartingOn(%s) failed", w)
		}
		p = sc.Next(p)
	}
	if _, ok := sc.StartingOn(d("2027-02-27")); ok {
		t.Fatal("27 Feb is not a period start")
	}
	if c := sc.Covering(d("2027-03-15")); DayStr(c.Start) != "2027-02-28" || DayStr(c.End) != "2027-03-31" {
		t.Fatalf("covering: %+v", c)
	}
	// Starting on the last day of a 30-day month with billing day 31 keeps the 31st.
	sc2, _ := NewSchedule(d("2027-04-30"), 31, false, PeriodMonthly)
	if DayStr(sc2.At(1).Start) != "2027-05-31" {
		t.Fatalf("billing day 31 from 30 Apr: %s", DayStr(sc2.At(1).Start))
	}
	// Without proration the billing day is the start day.
	sc3, _ := NewSchedule(d("2027-04-10"), 1, false, PeriodMonthly)
	if sc3.BillingDay != 10 || sc3.Partial || DayStr(sc3.At(1).Start) != "2027-05-10" {
		t.Fatalf("no proration: %+v", sc3)
	}
}

func TestScheduleLeapYearYearly(t *testing.T) {
	sc, _ := NewSchedule(d("2028-02-29"), 0, false, PeriodYearly)
	got := []string{}
	for k := 0; k < 5; k++ {
		got = append(got, DayStr(sc.At(k).Start))
	}
	want := []string{"2028-02-29", "2029-02-28", "2030-02-28", "2031-02-28", "2032-02-29"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("yearly from 29 Feb: %v", got)
		}
	}
	// A leap year has 366 days, the next one 365.
	// 29 Feb 2028 → 28 Feb 2029 is 365 days; 28 Feb 2031 → 29 Feb 2032 is 366.
	if sc.At(0).Days() != 365 || sc.At(3).Days() != 366 {
		t.Fatalf("days per year: %d %d", sc.At(0).Days(), sc.At(3).Days())
	}
	// Yearly from 31 Dec crosses no month end trouble; 1 Mar after a leap day.
	sc2, _ := NewSchedule(d("2027-03-01"), 0, false, PeriodYearly)
	if DayStr(sc2.At(1).Start) != "2028-03-01" || sc2.At(0).Days() != 366 {
		t.Fatalf("yearly over 29 Feb 2028: %s %d", DayStr(sc2.At(1).Start), sc2.At(0).Days())
	}
	// Quarterly.
	sc3, _ := NewSchedule(d("2026-11-30"), 0, false, PeriodQuarterly)
	if DayStr(sc3.At(1).Start) != "2027-02-28" || DayStr(sc3.At(2).Start) != "2027-05-30" {
		t.Fatalf("quarterly: %s %s", DayStr(sc3.At(1).Start), DayStr(sc3.At(2).Start))
	}
}

func TestProration(t *testing.T) {
	// 20 Jan → billing day 1: 12 of January's 31 days.
	sc, _ := NewSchedule(d("2027-01-20"), 1, true, PeriodMonthly)
	f := sc.First()
	if !f.Partial || DayStr(f.Start) != "2027-01-20" || DayStr(f.End) != "2027-02-01" || f.Days() != 12 {
		t.Fatalf("partial: %+v", f)
	}
	if f.Fraction != 0.3871 { // 12/31
		t.Fatalf("fraction %v", f.Fraction)
	}
	if got := Prorate(1500, f.Fraction, "KES"); got != 580.65 {
		t.Fatalf("prorated KES %v", got)
	}
	if got := Prorate(1500, 12.0/31.0, "JPY"); got != 581 { // no minor units
		t.Fatalf("prorated JPY %v", got)
	}
	if n := sc.Next(f); DayStr(n.Start) != "2027-02-01" || DayStr(n.End) != "2027-03-01" || n.Partial {
		t.Fatalf("after partial: %+v", n)
	}
	// Leap February: 10 Feb 2028 → 1 Mar: 20 of 29 days.
	sc2, _ := NewSchedule(d("2028-02-10"), 1, true, PeriodMonthly)
	if f := sc2.First(); f.Days() != 20 || f.Fraction != 0.6897 {
		t.Fatalf("leap partial: %+v", f)
	}
	// Billing day later in the same month: 5 Mar → 25 Mar of [25 Feb, 25 Mar) = 20/28.
	sc3, _ := NewSchedule(d("2027-03-05"), 25, true, PeriodMonthly)
	if f := sc3.First(); DayStr(f.End) != "2027-03-25" || f.Days() != 20 || f.Fraction != 0.7143 {
		t.Fatalf("same-month partial: %+v", f)
	}
	// Yearly with proration to the 1st: 11 days of the 365-day year before.
	sc4, _ := NewSchedule(d("2026-10-21"), 1, true, PeriodYearly)
	if f := sc4.First(); DayStr(f.End) != "2026-11-01" || f.Days() != 11 || f.Fraction != 0.0301 {
		t.Fatalf("yearly partial: %+v", f)
	}
	// Starting on the billing day: no partial period.
	sc5, _ := NewSchedule(d("2027-03-01"), 1, true, PeriodMonthly)
	if sc5.Partial {
		t.Fatal("aligned start has no partial period")
	}
	// 31 Jan → billing day 1 (1 Feb): one day of January.
	sc6, _ := NewSchedule(d("2027-01-31"), 1, true, PeriodMonthly)
	if f := sc6.First(); DayStr(f.Start) != "2027-01-31" || DayStr(f.End) != "2027-02-01" || f.Days() != 1 {
		t.Fatalf("one-day partial: %+v", f)
	}
}

func TestLeadTimes(t *testing.T) {
	st := DefaultSettings("o")
	if Lead(nil, st, PeriodMonthly) != 7 || Lead(nil, st, PeriodQuarterly) != 7 || Lead(nil, st, PeriodYearly) != 14 {
		t.Fatal("defaults: 7 days for monthly and quarterly, 14 for yearly")
	}
	ten := 10
	if Lead(&ten, st, PeriodYearly) != 10 {
		t.Fatal("own lead time wins")
	}
	st.LeadMonthly = 40
	if Lead(nil, st, PeriodMonthly) != 27 {
		t.Fatal("a monthly invoice is never created a whole period early")
	}
	sp := Span{Start: d("2026-11-04")}
	if DayStr(IssueDate(sp, 14)) != "2026-10-21" || DayStr(IssueDate(Span{Start: d("2027-03-01")}, 7)) != "2027-02-22" || DayStr(IssueDate(Span{Start: d("2028-03-01")}, 7)) != "2028-02-23" {
		t.Fatal("issue dates: N days before the period starts (leap February)")
	}
	if DayStr(DueDate(Span{Start: d("2027-01-31")}, 7)) != "2027-02-07" {
		t.Fatal("payment terms")
	}
}

func TestTimeZones(t *testing.T) {
	nairobi, _ := time.LoadLocation("Africa/Nairobi")
	sydney, _ := time.LoadLocation("Australia/Sydney")
	at := time.Date(2026, 10, 20, 22, 30, 0, 0, time.UTC)
	if DayStr(LocalDay(at, nairobi)) != "2026-10-21" { // 01:30 in Nairobi
		t.Fatal("Nairobi day")
	}
	if DayStr(LocalDay(at, sydney)) != "2026-10-21" { // 09:30 AEDT
		t.Fatal("Sydney day")
	}
	if DayStr(LocalDay(time.Date(2026, 10, 20, 13, 59, 0, 0, time.UTC), sydney)) != "2026-10-21" {
		t.Fatal("Sydney is past midnight at 14:00 UTC")
	}
	if DayStr(LocalDay(time.Date(2026, 10, 20, 20, 59, 0, 0, time.UTC), nairobi)) != "2026-10-20" {
		t.Fatal("Nairobi still the 20th at 23:59")
	}
	// Day keeps the calendar date of the value it is given (no conversion).
	if DayStr(Day(time.Date(2026, 1, 31, 23, 30, 0, 0, nairobi))) != "2026-01-31" {
		t.Fatal("Day")
	}
	// DST change in Sydney (5 Apr 2026): still whole days.
	if DaysBetween(LocalDay(time.Date(2026, 4, 4, 1, 0, 0, 0, time.UTC), sydney), LocalDay(time.Date(2026, 4, 6, 1, 0, 0, 0, time.UTC), sydney)) != 2 {
		t.Fatal("DST")
	}
}

func TestPerPeriodAndCatalogue(t *testing.T) {
	if PerPeriod(1000, PeriodMonthly, PeriodYearly) != 12000 || PerPeriod(12000, PeriodYearly, PeriodMonthly) != 1000 || PerPeriod(900, PeriodQuarterly, PeriodMonthly) != 300 {
		t.Fatal("per period")
	}
	if PerPeriod(1500, "", PeriodYearly) != 1500 {
		t.Fatal("an unknown catalogue period is the billing period")
	}
	if Monthly(15000, PeriodYearly) != 1250 || Monthly(4500, PeriodQuarterly) != 1500 {
		t.Fatal("monthly equivalents")
	}
	cases := map[string]string{
		"Starter hosting + .co.ke domain — yearly": PeriodYearly, "Starter hosting — monthly": PeriodMonthly, "Business hosting (per month)": PeriodMonthly,
		"Domain renewal /yr": PeriodYearly, "Annual maintenance": PeriodYearly, "Email — quarterly": PeriodQuarterly, "Starter hosting": "", "12 months of hosting": PeriodYearly,
	}
	for name, want := range cases {
		if got := InferPeriod(name, ""); got != want {
			t.Errorf("InferPeriod(%q) = %q, want %q", name, got, want)
		}
	}
	if InferPeriod("Starter hosting", "Billed per year") != PeriodYearly {
		t.Fatal("the description counts")
	}
	if ServiceLabel("Starter hosting + .co.ke domain — yearly") != "Starter hosting + .co.ke domain" || ServiceLabel("Business hosting (monthly)") != "Business hosting" || ServiceLabel("Hosting") != "Hosting" {
		t.Fatalf("labels: %q", ServiceLabel("Business hosting (monthly)"))
	}
	if InferKind(".co.ke domain", "") != KindDomain || InferKind("Micro Env", "Hosting & domains") != KindHosting || InferKind("Starter hosting + .co.ke domain", "") != KindHosting ||
		InferKind("5 mailboxes email", "") != KindEmail || InferKind("Website care plan", "") != KindMaintenance || InferKind("Logo design", "Design") != KindOther {
		t.Fatal("kinds")
	}
}
