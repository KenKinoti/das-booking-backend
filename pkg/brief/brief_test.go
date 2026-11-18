package brief

import (
	"strings"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func TestDecideAcrossTimeZones(t *testing.T) {
	nairobi, adelaide := mustLoc(t, "Africa/Nairobi"), mustLoc(t, "Australia/Adelaide")
	st := Settings{Enabled: true, Time: "08:00", Days: DaysEveryDay, ScheduleFrom: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	d := day(2026, 11, 19)
	// 08:00 in Nairobi is 05:00 UTC.
	cases := []struct {
		now  time.Time
		want Decision
	}{
		{time.Date(2026, 11, 19, 4, 59, 0, 0, time.UTC), Wait},
		{time.Date(2026, 11, 19, 5, 0, 0, 0, time.UTC), Send},
		{time.Date(2026, 11, 19, 7, 59, 0, 0, time.UTC), Send}, // 2 h 59 late: still within the catch-up window
		{time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC), Send},  // exactly 3 h
		{time.Date(2026, 11, 19, 8, 1, 0, 0, time.UTC), Missed},
	}
	for _, c := range cases {
		got, at := Decide(st, nairobi, d, c.now, 3*time.Hour)
		if got != c.want {
			t.Errorf("Nairobi at %s: %s, want %s", c.now.Format(time.RFC3339), got, c.want)
		}
		if !at.Equal(time.Date(2026, 11, 19, 5, 0, 0, 0, time.UTC)) {
			t.Errorf("due at %s", at)
		}
	}
	// The same 08:00 in Adelaide (UTC+10:30 in November) is 21:30 UTC the day before.
	got, at := Decide(st, adelaide, d, time.Date(2026, 11, 18, 21, 30, 0, 0, time.UTC), 3*time.Hour)
	if got != Send || !at.Equal(time.Date(2026, 11, 18, 21, 30, 0, 0, time.UTC)) {
		t.Fatalf("Adelaide: %s at %s", got, at)
	}
}

func TestDecideWeekdaysAndScheduleChanges(t *testing.T) {
	loc := mustLoc(t, "Africa/Nairobi")
	st := Settings{Enabled: true, Time: "07:30", Days: DaysWeekdays}
	sat, mon := day(2026, 11, 21), day(2026, 11, 23)
	if d, _ := Decide(st, loc, sat, time.Date(2026, 11, 21, 5, 0, 0, 0, time.UTC), 3*time.Hour); d != Skip {
		t.Fatalf("Saturday with weekdays: %s", d)
	}
	if d, _ := Decide(st, loc, mon, time.Date(2026, 11, 23, 4, 31, 0, 0, time.UTC), 3*time.Hour); d != Send {
		t.Fatalf("Monday: %s", d)
	}
	st.Days = DaysEveryDay
	if d, _ := Decide(st, loc, sat, time.Date(2026, 11, 21, 5, 0, 0, 0, time.UTC), 3*time.Hour); d != Send {
		t.Fatalf("Saturday every day: %s", d)
	}
	// Switched on at 10:00 local: today's 07:30 is neither sent late nor missed.
	st.ScheduleFrom = time.Date(2026, 11, 23, 7, 0, 0, 0, time.UTC)
	if d, _ := Decide(st, loc, mon, time.Date(2026, 11, 23, 7, 1, 0, 0, time.UTC), 3*time.Hour); d != Skip {
		t.Fatalf("schedule changed after the due time: %s", d)
	}
	if d, _ := Decide(st, loc, mon.AddDate(0, 0, 1), time.Date(2026, 11, 24, 4, 31, 0, 0, time.UTC), 3*time.Hour); d != Send {
		t.Fatalf("next day: %s", d)
	}
	st.Enabled = false
	if d, _ := Decide(st, loc, mon, time.Date(2026, 11, 23, 4, 31, 0, 0, time.UTC), 3*time.Hour); d != Skip {
		t.Fatalf("off: %s", d)
	}
}

func TestDueAtAcrossDaylightSaving(t *testing.T) {
	syd := mustLoc(t, "Australia/Sydney")
	// Daylight saving starts on Sunday 4 October 2026 in Sydney (UTC+10 → +11).
	before := DueAt("08:00", syd, day(2026, 10, 3))
	after := DueAt("08:00", syd, day(2026, 10, 5))
	if before.UTC().Hour() != 22 || after.UTC().Hour() != 21 {
		t.Fatalf("DST: %s / %s", before.UTC(), after.UTC())
	}
	if DueAt("bad", syd, day(2026, 10, 5)).In(syd).Hour() != 8 {
		t.Fatal("invalid time should fall back to 08:00")
	}
}

func TestNormalTime(t *testing.T) {
	for in, want := range map[string]string{"8:00": "08:00", "08:00": "08:00", "0730": "07:30", " 23:59 ": "23:59"} {
		if got, ok := NormalTime(in); !ok || got != want {
			t.Errorf("NormalTime(%q) = %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"24:00", "8", "ab:cd", "12:60", ""} {
		if _, ok := NormalTime(bad); ok {
			t.Errorf("NormalTime(%q) accepted", bad)
		}
	}
}

func TestHeadlineOrderAndLimit(t *testing.T) {
	secs := []Section{
		{Key: "opportunities", Headline: []Fragment{{"1 new opportunity worth ~KES 60,000", 40}}},
		{Key: "bills", Headline: []Fragment{{"3 bills due this week (KES 41,200)", 20}}},
		{Key: "projects", Headline: []Fragment{{"2 Jira issues overdue", 30}}},
	}
	if h := Headline(secs); h != "3 bills due this week (KES 41,200), 2 Jira issues overdue, 1 new opportunity worth ~KES 60,000" {
		t.Fatalf("headline %q", h)
	}
	secs = append(secs, Section{Headline: []Fragment{{"1 bill overdue (KES 5,000)", 10}, {"2 meetings today", 48}}})
	h := Headline(secs)
	if !strings.HasPrefix(h, "1 bill overdue (KES 5,000), 3 bills due") || strings.Contains(h, "meetings") || strings.Count(h, ", ") != MaxFragments-1 {
		t.Fatalf("headline %q", h)
	}
	// Breadth first: a section with several fragments doesn't crowd out the others.
	h = Headline([]Section{
		{Headline: []Fragment{{"a1", 10}, {"a2", 11}, {"a3", 12}}},
		{Headline: []Fragment{{"b1", 30}}},
		{Headline: []Fragment{{"c1", 60}}},
	})
	if h != "a1, a2, b1, c1" {
		t.Fatalf("breadth %q", h)
	}
	if h := Headline(nil); !strings.HasPrefix(h, "Nothing needs your attention") {
		t.Fatalf("empty headline %q", h)
	}
}

func TestMoney(t *testing.T) {
	cases := [][2]string{
		{Money(41200, "KES"), "KES 41,200"},
		{Money(41200.4, "KES"), "KES 41,200"},
		{Money(20000+21200, "KES"), "KES 41,200"},
		{Money(120.5, "AUD"), "AUD 120.50"},
		{Money(120, "AUD"), "AUD 120"},
		{Money(-1500, "kes"), "-KES 1,500"},
		{Money(1234567, "USD"), "USD 1,234,567"},
		{Money(1000, "JPY"), "JPY 1,000"},
		{Money(999.999, "KES"), "KES 1,000"},
		{Money(0.5, "KES"), "KES 0.50"},
		{Money(41200, ""), "41,200"},
		{Approx(59870, "KES"), "~KES 59,900"},
		{Approx(123456, "KES"), "~KES 123,000"},
		{Approx(1234.5, "KES"), "~KES 1,230"},
		{Approx(60000, "KES"), "~KES 60,000"},
	}
	for _, c := range cases {
		if c[0] != c[1] {
			t.Errorf("got %q want %q", c[0], c[1])
		}
	}
}

func TestTemplateParamsAreClean(t *testing.T) {
	b := &Brief{FirstName: "", DateLabel: "Thu 19 Nov", Headline: "2 bills due\nthis week\t(KES 41,200),      1 overdue"}
	p := TemplateParams(b)
	if len(p) != 3 || p[0] != "there" || p[1] != "Thu 19 Nov" {
		t.Fatalf("params %q", p)
	}
	for _, x := range p {
		if strings.ContainsAny(x, "\n\t") || strings.Contains(x, "    ") {
			t.Fatalf("param not clean: %q", x)
		}
	}
}

func TestSectionAddAndEmpty(t *testing.T) {
	s := &Section{}
	if !s.Empty() {
		t.Fatal("new section should be empty")
	}
	s.Add("x", "X", nil, 0, "")
	if !s.Empty() || len(s.Groups) != 0 {
		t.Fatal("empty group added")
	}
	s.Add("x", "X", []Item{{Title: "a"}, {Title: "b"}}, 5, "/x")
	if s.Empty() || s.Groups[0].More != 3 {
		t.Fatalf("group %+v", s.Groups)
	}
	var nilSec *Section
	if !nilSec.Empty() {
		t.Fatal("nil section should be empty")
	}
}

func TestRegisterOrderAndOptions(t *testing.T) {
	Register("zz_test", "Test section", 45, nil, Icon("fa-solid fa-star"), Describe("d"), Requires(func(v *Viewer) bool { return v.Managing }, "managers"))
	defer Unregister("zz_test")
	keys := []string{}
	for _, r := range sections() {
		keys = append(keys, r.Key)
	}
	got := strings.Join(keys, ",")
	if got != "bills,money,projects,opportunities,zz_test,today" {
		t.Fatalf("order %s", got)
	}
	s := &Service{}
	info := s.Sections(&Viewer{Managing: false}, Settings{SectionsOff: []string{"today"}})
	for _, si := range info {
		switch si.Key {
		case "zz_test":
			if si.Allowed || si.Reason != "managers" || si.Icon != "fa-solid fa-star" {
				t.Fatalf("zz_test %+v", si)
			}
		case "today":
			if si.On || !si.Allowed {
				t.Fatalf("today %+v", si)
			}
		case "bills", "money", "opportunities":
			if si.Allowed {
				t.Fatalf("%s allowed for staff", si.Key)
			}
		}
	}
}

func TestTextAndEmailRendering(t *testing.T) {
	s := &Service{AppURL: "https://app.example"}
	pct := 60
	b := &Brief{Date: "2026-11-19", DateLabel: "Thu 19 Nov", FirstName: "Ken", Greeting: "Good morning Ken", Headline: "2 bills due this week (KES 41,200)",
		Sections: []Section{{Key: "bills", Title: "Bills & payables", Summary: "KES 41,200 due this week", Link: "/bills?view=upcoming",
			Groups: []Group{{Title: "Due later this week", Items: []Item{{Title: "Hosting <Plan>", Detail: "due Fri 21 Nov", Value: "KES 20,000", Link: "/bills?bill=b1", Tag: "New"}}, More: 2, Link: "/bills"}}},
			{Key: "projects", Title: "Projects", Notes: []string{"Jira could not be reached; figures from Thu 19 Nov 06:00"},
				Groups: []Group{{Title: "Active projects", Items: []Item{{Title: "DA — App", Value: "60%", Progress: &pct, External: "https://x.atlassian.net/browse/DA"}}}}}}}
	txt := s.Text(b, false)
	for _, want := range []string{"Good morning Ken,", "2 bills due this week (KES 41,200)", "BILLS & PAYABLES", "• Hosting <Plan> — KES 20,000 (due Fri 21 Nov)",
		"https://app.example/bills?bill=b1", "…and 2 more", "Jira could not be reached", "Open the full brief: https://app.example/brief/2026-11-19"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text misses %q:\n%s", want, txt)
		}
	}
	w := s.Text(b, true)
	if !strings.Contains(w, "*Good morning Ken* — your brief for Thu 19 Nov") || !strings.Contains(w, "*Bills & payables*") || strings.Contains(w, "https://app.example/bills?bill=b1") {
		t.Errorf("whatsapp text:\n%s", w)
	}
	html := s.sectionsHTML(b, "#4f46e5")
	if !strings.Contains(html, "Hosting &lt;Plan&gt;") || !strings.Contains(html, `href="https://app.example/bills?bill=b1"`) || !strings.Contains(html, "Open in Jira") || !strings.Contains(html, "and 2 more") {
		t.Errorf("html: %s", html)
	}
	if sub := Subject(b, true); sub != "[Test] Morning brief · Thu 19 Nov — 2 bills due this week (KES 41,200)" {
		t.Errorf("subject %q", sub)
	}
}

func TestGreeting(t *testing.T) {
	loc := mustLoc(t, "Africa/Nairobi")
	for h, want := range map[int]string{7: "Good morning Ken", 13: "Good afternoon Ken", 19: "Good evening Ken"} {
		if g := greeting(time.Date(2026, 11, 19, h, 0, 0, 0, loc), "Ken"); g != want {
			t.Errorf("%d: %q", h, g)
		}
	}
	if g := greeting(time.Date(2026, 11, 19, 8, 0, 0, 0, loc), ""); g != "Good morning" {
		t.Errorf("no name: %q", g)
	}
}
