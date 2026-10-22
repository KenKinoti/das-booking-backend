package occasions

import (
	"strings"
	"testing"
	"time"
)

func d(s string) time.Time {
	t, ok := ParseDay(s)
	if !ok {
		panic(s)
	}
	return t
}

func ymd(t time.Time) string { return t.Format("2006-01-02") }

// find returns the built-in occasion key in year y for country (exact
// Countries match when country != "").
func find(t *testing.T, y int, key, country string) Occasion {
	t.Helper()
	for _, o := range Builtin(y) {
		if o.Key != key {
			continue
		}
		if country == "" && len(o.Countries) == 0 || country != "" && has(o.Countries, country) {
			return o
		}
	}
	t.Fatalf("%d: no %s for %q", y, key, country)
	return Occasion{}
}

func TestEaster(t *testing.T) {
	for y, want := range map[int]string{2024: "2024-03-31", 2025: "2025-04-20", 2026: "2026-04-05", 2027: "2027-03-28", 2028: "2028-04-16", 2038: "2038-04-25"} {
		if got := ymd(Easter(y)); got != want {
			t.Errorf("Easter %d = %s, want %s", y, got, want)
		}
	}
	// Good Friday / Easter Monday from the computus (Fair Work 2026/2027).
	for y, want := range map[int][2]string{2026: {"2026-04-03", "2026-04-06"}, 2027: {"2027-03-26", "2027-03-29"}, 2028: {"2028-04-14", "2028-04-17"}} {
		gf, em := find(t, y, "good_friday", "KE"), find(t, y, "easter_monday", "AU")
		if ymd(gf.Date) != want[0] || ymd(em.Date) != want[1] {
			t.Errorf("%d: Good Friday %s Easter Monday %s", y, ymd(gf.Date), ymd(em.Date))
		}
		if !has(gf.Countries, "AU") || !has(em.Countries, "KE") || gf.Kind != KindPublicHoliday {
			t.Errorf("%d: Easter holidays are public holidays in KE and AU: %+v", y, gf)
		}
	}
	e := find(t, 2026, "easter", "")
	if ymd(e.ThemeFrom) != "2026-03-29" || ymd(e.ThemeTo) != "2026-04-06" || e.Theme != "easter" {
		t.Errorf("Easter window %s→%s", ymd(e.ThemeFrom), ymd(e.ThemeTo))
	}
}

func TestBlackFridayAndRules(t *testing.T) {
	for y, want := range map[int][2]string{2025: {"2025-11-28", "2025-12-01"}, 2026: {"2026-11-27", "2026-11-30"}, 2027: {"2027-11-26", "2027-11-29"}, 2028: {"2028-11-24", "2028-11-27"}} {
		if ymd(BlackFriday(y)) != want[0] || ymd(CyberMonday(y)) != want[1] {
			t.Errorf("%d: Black Friday %s Cyber Monday %s", y, ymd(BlackFriday(y)), ymd(CyberMonday(y)))
		}
	}
	bf := find(t, 2026, "black_friday", "")
	if ymd(bf.ThemeFrom) != "2026-11-23" || ymd(bf.ThemeTo) != "2026-11-29" || !bf.Important || bf.Kind != KindCommercial || bf.Source == "" {
		t.Errorf("black friday %+v", bf)
	}
	// "n-th weekday" rules.
	checks := map[string]string{
		ymd(NthWeekday(2026, time.May, time.Sunday, 2)):         "2026-05-10", // Mother's Day 2026
		ymd(NthWeekday(2027, time.May, time.Sunday, 2)):         "2027-05-09",
		ymd(NthWeekday(2026, time.June, time.Sunday, 3)):        "2026-06-21", // Father's Day KE
		ymd(NthWeekday(2025, time.June, time.Sunday, 3)):        "2025-06-15",
		ymd(NthWeekday(2026, time.September, time.Sunday, 1)):   "2026-09-06", // Father's Day AU
		ymd(NthWeekday(2027, time.September, time.Sunday, 1)):   "2027-09-05",
		ymd(NthWeekday(2026, time.November, time.Thursday, -1)): "2026-11-26",
	}
	for got, want := range checks {
		if got != want {
			t.Errorf("nth weekday %s want %s", got, want)
		}
	}
	if o := find(t, 2026, "mothers_day", "KE"); ymd(o.Date) != "2026-05-10" || !has(o.Countries, "AU") {
		t.Errorf("mother's day %+v", o)
	}
	if ymd(find(t, 2026, "fathers_day", "KE").Date) != "2026-06-21" || ymd(find(t, 2026, "fathers_day", "AU").Date) != "2026-09-06" {
		t.Error("father's day KE third Sunday of June, AU first Sunday of September")
	}
}

func TestKenyanDays(t *testing.T) {
	want := map[string]string{
		"new_year": "2026-01-01", "eid_al_fitr": "2026-03-20", "good_friday": "2026-04-03", "easter_monday": "2026-04-06", "labour_day": "2026-05-01",
		"eid_al_adha": "2026-05-27", "madaraka_day": "2026-06-01", "mazingira_day": "2026-10-10", "mashujaa_day": "2026-10-20",
		"jamhuri_day": "2026-12-12", "christmas_day": "2026-12-25", "boxing_day": "2026-12-26",
	}
	got := map[string]string{}
	for _, o := range Builtin(2026) {
		if o.Kind == KindPublicHoliday && has(o.Countries, "KE") {
			got[o.Key] = ymd(o.Date)
			if o.Source == "" || !strings.Contains(strings.Join(o.Sources, " "), "kenyalaw.org") {
				t.Errorf("%s: no Kenya Law source %v", o.Key, o.Sources)
			}
			if !o.Important {
				t.Errorf("%s: public holidays are important", o.Key)
			}
		}
	}
	if len(got) != len(want) {
		t.Errorf("Kenyan public holidays 2026: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %s, want %s", k, got[k], v)
		}
	}
	m := find(t, 2026, "mashujaa_day", "KE")
	if m.Name != "Mashujaa Day" || m.Theme != "kenya_national" || ymd(m.ThemeFrom) != "2026-10-17" || ymd(m.ThemeTo) != "2026-10-20" || m.Greeting != "Happy Mashujaa Day" {
		t.Errorf("mashujaa %+v", m)
	}
	if o := find(t, 2026, "mazingira_day", "KE"); o.Theme != "mazingira" || !strings.Contains(o.Note, "Environment") {
		t.Errorf("mazingira %+v", o)
	}
	// Boxing Day keeps its gazetted name.
	if o := find(t, 2026, "boxing_day", "KE"); o.Name != "Boxing Day" {
		t.Error(o.Name)
	}
	// 2027: Sunday rule (Public Holidays Act s. 4).
	for key, next := range map[string]string{"mazingira_day": "Monday 11 October 2027", "jamhuri_day": "Monday 13 December 2027", "boxing_day": "Monday 27 December 2027"} {
		if o := find(t, 2027, key, "KE"); !strings.Contains(o.Note, next) {
			t.Errorf("2027 %s note %q, want %s", key, o.Note, next)
		}
	}
	// 2028: Idd-ul-Fitr expected Sunday 27 Feb → Monday 28 Feb; tentative + confirm.
	f := find(t, 2028, "eid_al_fitr", "KE")
	if ymd(f.Date) != "2028-02-27" || !f.Tentative || !strings.Contains(f.Note, "confirm closer to the date") || !strings.Contains(f.Note, "Monday 28 February 2028") {
		t.Errorf("eid 2028 %+v", f)
	}
	// Moon dates: every table year has a source; 2026 was declared.
	for y := 2026; y <= 2028; y++ {
		for _, k := range []string{"eid_al_fitr", "eid_al_adha"} {
			o := find(t, y, k, "KE")
			if o.Source == "" || o.Theme != "eid" {
				t.Errorf("%d %s %+v", y, k, o)
			}
			if (y > 2026) != o.Tentative {
				t.Errorf("%d %s tentative=%v", y, k, o.Tentative)
			}
		}
		if o := find(t, y, "diwali", ""); o.Source == "" || o.Theme != "diwali" {
			t.Errorf("%d diwali %+v", y, o)
		}
	}
	for y, want := range map[int]string{2026: "2026-11-08", 2027: "2027-10-29", 2028: "2028-10-17"} {
		if got := ymd(find(t, y, "diwali", "").Date); got != want {
			t.Errorf("diwali %d = %s", y, got)
		}
	}
	for y, want := range map[int][2]string{2027: {"2027-03-10", "2027-05-16"}, 2028: {"2028-02-27", "2028-05-05"}} {
		if ymd(find(t, y, "eid_al_fitr", "KE").Date) != want[0] || ymd(find(t, y, "eid_al_adha", "KE").Date) != want[1] {
			t.Errorf("eid %d", y)
		}
	}
	// No table entry → no Eid that year (rather than a guess).
	for _, o := range Builtin(2031) {
		if o.Key == "eid_al_fitr" || o.Key == "diwali" {
			t.Errorf("2031 has %s", o.Key)
		}
	}
}

func TestAustralianDays(t *testing.T) {
	want := map[string]string{"new_year": "2026-01-01", "australia_day": "2026-01-26", "good_friday": "2026-04-03", "easter_monday": "2026-04-06",
		"anzac_day": "2026-04-25", "christmas_day": "2026-12-25", "boxing_day": "2026-12-26"}
	got := map[string]string{}
	for _, o := range Builtin(2026) {
		if o.Kind == KindPublicHoliday && has(o.Countries, "AU") {
			got[o.Key] = ymd(o.Date)
			if !strings.Contains(strings.Join(o.Sources, " "), "fairwork.gov.au") {
				t.Errorf("%s: no Fair Work source", o.Key)
			}
		}
	}
	if len(got) != len(want) {
		t.Errorf("AU public holidays %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %s want %s", k, got[k], v)
		}
	}
	if o := find(t, 2026, "boxing_day", "AU"); !strings.Contains(o.Note, "Monday 28 December") {
		t.Errorf("2026 Boxing Day substitute: %q", o.Note)
	}
	if o := find(t, 2027, "christmas_day", "AU"); !strings.Contains(o.Note, "Monday 27 December") {
		t.Errorf("2027 Christmas: %q", o.Note)
	}
	if o := find(t, 2027, "boxing_day", "AU"); !strings.Contains(o.Note, "Tuesday 28 December") || !strings.Contains(o.Note, "Kenya: falls on a Sunday") {
		t.Errorf("2027 Boxing Day: %q", o.Note)
	}
	if o := find(t, 2027, "anzac_day", "AU"); !strings.Contains(o.Note, "Monday 26 April") || o.Theme != "remembrance" {
		t.Errorf("2027 Anzac %+v", o)
	}
	if o := find(t, 2027, "australia_day", "AU"); o.Theme != "australia_day" || ymd(o.ThemeFrom) != "2027-01-23" {
		t.Errorf("australia day %+v", o)
	}
}

func TestWindows(t *testing.T) {
	cases := []struct{ key, country, from, to string }{
		{"halloween", "", "2026-10-24", "2026-10-31"},
		{"christmas_day", "KE", "2026-12-01", "2026-12-26"},
		{"christmas_day", "", "2026-12-01", "2026-12-26"},
		{"new_years_eve", "", "2026-12-27", "2026-12-31"},
		{"new_year", "AU", "2026-01-01", "2026-01-02"},
		{"valentines_day", "", "2026-02-10", "2026-02-14"},
		{"jamhuri_day", "KE", "2026-12-09", "2026-12-12"},
		{"madaraka_day", "KE", "2026-05-29", "2026-06-01"},
		{"womens_day", "", "2026-03-05", "2026-03-08"},
		{"eid_al_fitr", "KE", "2026-03-18", "2026-03-22"},
		{"diwali", "", "2026-11-06", "2026-11-10"},
	}
	for _, c := range cases {
		o := find(t, 2026, c.key, c.country)
		if ymd(o.ThemeFrom) != c.from || ymd(o.ThemeTo) != c.to {
			t.Errorf("%s/%s window %s→%s want %s→%s", c.key, c.country, ymd(o.ThemeFrom), ymd(o.ThemeTo), c.from, c.to)
		}
		if _, ok := ThemeByKey(o.Theme); !ok {
			t.Errorf("%s: theme %q unknown", c.key, o.Theme)
		}
	}
	// Every built-in theme key is drawn by the web app; every theme has a greeting.
	for y := 2026; y <= 2028; y++ {
		for _, o := range Builtin(y) {
			if o.Theme == "" {
				continue
			}
			if _, ok := ThemeByKey(o.Theme); !ok || o.Greeting == "" || o.ThemeFrom.After(o.Date) || o.ThemeTo.Before(o.Date) {
				t.Errorf("%d %s: theme %q greeting %q window %s→%s", y, o.Key, o.Theme, o.Greeting, ymd(o.ThemeFrom), ymd(o.ThemeTo))
			}
		}
	}
	if g := find(t, 2026, "new_years_eve", "").Greeting; g != "Countdown to 2027" {
		t.Error(g)
	}
	if g := find(t, 2027, "new_year", "KE").Greeting; g != "Happy New Year 2027" {
		t.Error(g)
	}
}

func TestCountryFilter(t *testing.T) {
	s := &Service{}
	keys := func(country string) map[string]int {
		m := map[string]int{}
		for _, o := range s.Between(d("2026-12-01"), d("2026-12-31"), country) {
			m[o.Key+"/"+o.Kind]++
		}
		return m
	}
	keM, auM, usM, all := keys("KE"), keys("au"), keys("US"), keys("")
	if keM["jamhuri_day/public_holiday"] != 1 || auM["jamhuri_day/public_holiday"] != 0 || usM["jamhuri_day/public_holiday"] != 0 {
		t.Errorf("Jamhuri Day is Kenyan: %v %v", keM, auM)
	}
	// Christmas: a public holiday in KE/AU, an observance elsewhere — never both.
	if keM["christmas_day/public_holiday"] != 1 || keM["christmas_day/observance"] != 0 || usM["christmas_day/observance"] != 1 || usM["christmas_day/public_holiday"] != 0 {
		t.Errorf("christmas KE %v US %v", keM, usM)
	}
	if all["christmas_day/public_holiday"] != 1 || all["christmas_day/observance"] != 1 || all["jamhuri_day/public_holiday"] != 1 {
		t.Errorf("all countries %v", all)
	}
	list := s.Between(d("2026-10-01"), d("2026-11-30"), "KE")
	for i := 1; i < len(list); i++ {
		if list[i].Date.Before(list[i-1].Date) {
			t.Fatal("not sorted")
		}
	}
	if len(s.Between(d("2026-10-02"), d("2026-10-01"), "")) != 0 {
		t.Error("empty range")
	}
	// Between keeps the contract's shape: dates are UTC midnight.
	for _, o := range list {
		if o.Date.Location() != time.UTC || o.Date.Hour() != 0 {
			t.Errorf("%s date %v", o.Key, o.Date)
		}
	}
}

// fake loads a service with custom occasions and toggles, no database.
func fake(customs []CustomOccasion, toggles map[string]bool) *Service {
	if toggles == nil {
		toggles = map[string]bool{}
	}
	return &Service{TTL: time.Hour, loadedAt: time.Now(), customs: customs, toggles: toggles}
}

func TestThemeOnPrecedence(t *testing.T) {
	s := fake(nil, nil)
	on := func(day, country string) string {
		o := s.ThemeOn(d(day), country)
		if o == nil {
			return ""
		}
		return o.Key + ":" + o.Theme
	}
	cases := map[[2]string]string{
		{"2026-10-30", "KE"}: "halloween:halloween",
		{"2026-10-30", ""}:   "halloween:halloween",
		{"2026-10-24", "AU"}: "halloween:halloween",
		{"2026-10-23", "KE"}: "",
		{"2026-10-20", "KE"}: "mashujaa_day:kenya_national",
		{"2026-10-17", "KE"}: "mashujaa_day:kenya_national",
		{"2026-10-20", "AU"}: "",
		{"2026-10-08", "KE"}: "mazingira_day:mazingira",
		// Jamhuri (KE, 4 days) beats the 26-day Christmas window.
		{"2026-12-10", "KE"}: "jamhuri_day:kenya_national",
		{"2026-12-10", "AU"}: "christmas_day:christmas",
		{"2026-12-24", "KE"}: "christmas_eve:christmas", // the day itself
		{"2026-12-25", "US"}: "christmas_day:christmas",
		{"2026-12-26", "AU"}: "boxing_day:christmas",
		{"2026-12-29", "KE"}: "new_years_eve:new_year",
		{"2027-01-02", "AU"}: "new_year:new_year",
		{"2027-01-25", "AU"}: "australia_day:australia_day",
		{"2027-01-25", "KE"}: "",
		{"2026-04-03", "KE"}: "good_friday:easter", // public holiday, 1-day window beats Easter's
		{"2026-04-03", "US"}: "easter:easter",
		{"2026-11-25", "KE"}: "black_friday:black_friday",
		{"2026-11-30", "AU"}: "cyber_monday:black_friday",
		{"2026-11-08", "KE"}: "diwali:diwali",
		{"2026-03-20", "KE"}: "eid_al_fitr:eid",
		{"2026-03-20", "GB"}: "eid_al_fitr:eid",
		// 2027: Diwali (29 Oct) overlaps Halloween — Halloween night is Halloween.
		{"2027-10-28", ""}: "diwali:diwali",
		{"2027-10-31", ""}: "halloween:halloween",
		// 2027: International Women's Day on the day beats Eid's window.
		{"2027-03-08", "KE"}: "womens_day:womens_day",
		{"2027-03-09", "KE"}: "eid_al_fitr:eid",
	}
	for in, want := range cases {
		if got := on(in[0], in[1]); got != want {
			t.Errorf("ThemeOn(%s, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
	// Unknown country: only "everywhere" occasions.
	if o := s.ThemeOnGlobal(d("2026-10-20")); o != nil {
		t.Errorf("global-only on Mashujaa Day: %+v", o)
	}
	if o := s.ThemeOnGlobal(d("2026-10-29")); o == nil || o.Key != "halloween" {
		t.Errorf("global-only Halloween %+v", o)
	}
	// Switches: an occasion's theme off → the next candidate; everything off → nil.
	s = fake(nil, map[string]bool{"jamhuri_day": false})
	if o := s.ThemeOn(d("2026-12-10"), "KE"); o == nil || o.Key != "christmas_day" {
		t.Errorf("jamhuri off: %+v", o)
	}
	if !s.GlobalEnabled() || s.OccasionEnabled("jamhuri_day") || !s.OccasionEnabled("halloween") {
		t.Error("toggle lookups")
	}
	s = fake(nil, map[string]bool{GlobalKey: false})
	if s.ThemeOn(d("2026-10-30"), "KE") != nil || s.GlobalEnabled() {
		t.Error("global off")
	}
	// Between ignores theme switches (the calendar still lists Halloween).
	if len(s.Between(d("2026-10-31"), d("2026-10-31"), "KE")) != 1 {
		t.Error("between with themes off")
	}
	// The package-level contract wrappers use the registered provider.
	Register(nil)
	if Between(d("2026-10-31"), d("2026-10-31"), "KE") != nil || ThemeOn(d("2026-10-31"), "KE") != nil {
		t.Error("no provider yet")
	}
	Register(fake(nil, nil))
	defer Register(nil)
	if o := ThemeOn(d("2026-10-20"), "KE"); o == nil || o.Key != "mashujaa_day" || len(Between(d("2026-10-20"), d("2026-10-20"), "KE")) != 1 {
		t.Errorf("wrappers %+v", o)
	}
}

func TestCustomOccasions(t *testing.T) {
	anniv := CustomOccasion{ID: "11111111-aaaa-bbbb-cccc-000000000001", Name: "DASYIN birthday", Date: d("2026-10-29"), ThemeFrom: d("2026-10-28"), ThemeTo: d("2026-10-29"),
		Countries: "KE", Theme: "new_year", Accent: "#0b5394", AccentDark: "#8ab4f8", Greeting: "Happy birthday, DASYIN", Enabled: true, Important: true}
	sale := CustomOccasion{ID: "22222222-aaaa-bbbb-cccc-000000000002", Name: "Spring sale", Date: d("2025-09-15"), ThemeFrom: d("2025-09-10"), ThemeTo: d("2025-09-15"),
		Theme: "easter", Enabled: true, Yearly: true}
	off := CustomOccasion{ID: "33333333-aaaa-bbbb-cccc-000000000003", Name: "Hidden", Date: d("2026-10-30"), ThemeFrom: d("2026-10-30"), ThemeTo: d("2026-10-30"), Theme: "eid", Enabled: false}
	s := fake([]CustomOccasion{anniv, sale, off}, map[string]bool{"halloween": false})
	// Custom beats a built-in (even when the built-in is on).
	s.toggles = map[string]bool{}
	o := s.ThemeOn(d("2026-10-29"), "KE")
	if o == nil || o.Kind != KindCustom || o.Key != "custom_11111111" || o.Palette == nil || o.Palette.Accent != "#0b5394" || o.Greeting != "Happy birthday, DASYIN" || o.ID != anniv.ID {
		t.Fatalf("custom wins: %+v", o)
	}
	// Country filter applies to customs; a disabled custom never shows.
	if o := s.ThemeOn(d("2026-10-29"), "AU"); o == nil || o.Key != "halloween" {
		t.Errorf("AU on 29 Oct: %+v", o)
	}
	if o := s.ThemeOn(d("2026-10-30"), "KE"); o == nil || o.Key != "halloween" {
		t.Errorf("disabled custom: %+v", o)
	}
	// Yearly repeats move with the year.
	if o := s.ThemeOn(d("2027-09-12"), "AU"); o == nil || o.Key != "custom_22222222" || ymd(o.Date) != "2027-09-15" {
		t.Errorf("yearly: %+v", o)
	}
	b := s.Between(d("2026-10-01"), d("2026-10-31"), "KE")
	n := 0
	for _, x := range b {
		if x.Kind == KindCustom {
			n++
		}
	}
	if n != 1 {
		t.Errorf("Between customs: %d", n)
	}
	// Previews: by occasion key, theme key or custom id.
	if p := s.Next("mashujaa_day", d("2026-10-21"), "KE"); p == nil || ymd(p.Date) != "2027-10-20" {
		t.Errorf("next mashujaa %+v", p)
	}
	if p := s.Next("christmas", d("2026-10-21"), "KE"); p == nil || p.Key != "christmas_day" {
		t.Errorf("preview christmas theme %+v", p)
	}
	if p := s.Next("australia_day", d("2026-10-21"), "KE"); p == nil || p.Key != "australia_day" || ymd(p.Date) != "2027-01-26" {
		t.Errorf("preview another country's theme %+v", p)
	}
	// Another country's theme previews its next occurrence, not a past one.
	if p := s.Next("kenya_national", d("2026-10-30"), "AU"); p == nil || p.Key != "jamhuri_day" || ymd(p.Date) != "2026-12-12" {
		t.Errorf("preview kenya_national from AU %+v", p)
	}
	if p := s.Next(off.ID, d("2026-10-21"), "KE"); p == nil || p.Theme != "eid" {
		t.Errorf("preview disabled custom %+v", p)
	}
	if shiftYears(d("2028-02-29"), 1) != d("2029-02-28") {
		t.Error("leap day")
	}
}

func TestResolveSettings(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		global, org bool
		user        *bool
		on          bool
		reason      string
	}{
		{true, true, nil, true, ReasonOn},
		{true, false, nil, false, ReasonOffOrg},
		{true, true, &no, false, ReasonOffUser},
		{true, false, &yes, true, ReasonOn}, // a person may keep them on for their own screen
		{false, true, &yes, false, ReasonOffGlobal},
		{false, true, nil, false, ReasonOffGlobal},
	}
	for _, c := range cases {
		on, reason := Resolve(c.global, c.org, c.user)
		if on != c.on || reason != c.reason {
			t.Errorf("Resolve(%v,%v,%v) = %v %s", c.global, c.org, c.user, on, reason)
		}
	}
}

func TestPaletteAndThemes(t *testing.T) {
	p, err := NormalisePalette(&Palette{Accent: "#0B5394"})
	if err != nil || p.Accent != "#0b5394" || p.AccentDark == "" || Contrast(p.AccentDark, darkSurface) < 4.5 || Contrast(p.AccentDark, darkBtnText) < 4.5 {
		t.Fatalf("derive dark: %+v %v", p, err)
	}
	if _, err := NormalisePalette(&Palette{Accent: "#ffd54f"}); err == nil || !strings.Contains(err.Error(), "darker") {
		t.Errorf("light gold must be refused for white text: %v", err)
	}
	if _, err := NormalisePalette(&Palette{Accent: "#0b5394", AccentDark: "#202040"}); err == nil {
		t.Error("dark accent too dark")
	}
	if _, err := NormalisePalette(&Palette{Accent: "blue"}); err == nil {
		t.Error("not a colour")
	}
	if p, err := NormalisePalette(&Palette{}); p != nil || err != nil {
		t.Error("empty palette")
	}
	// Built-in theme accents meet AA as button backgrounds (white text in
	// light mode, the dark theme's near-black text in dark mode) except
	// Black Friday's light accent, which is near-black with white text.
	seen := map[string]bool{}
	for _, th := range Themes {
		if seen[th.Key] || th.Label == "" || !strings.HasPrefix(th.Icon, "fa-solid fa-") {
			t.Errorf("theme %+v", th)
		}
		seen[th.Key] = true
		if c := Contrast(th.Accent, "#ffffff"); c < 4.5 {
			t.Errorf("%s light accent %s contrast %.2f", th.Key, th.Accent, c)
		}
		if c := Contrast(th.AccentDark, darkBtnText); c < 4.5 {
			t.Errorf("%s dark accent %s contrast %.2f", th.Key, th.AccentDark, c)
		}
	}
	if Contrast("#000000", "#ffffff") < 20.9 {
		t.Error("contrast formula")
	}
}

func TestCustomValidation(t *testing.T) {
	c := CustomOccasion{}
	good := CustomInput{Name: "Founders' day", Date: "2026-11-15", ThemeFrom: "2026-11-13", ThemeTo: "2026-11-15", Countries: []string{"ke", "AU", "KE"}, Theme: "new_year", Accent: "#0b5394"}
	if err := ApplyCustom(&c, good); err != nil || c.Countries != "KE,AU" || !c.Enabled || c.AccentDark == "" {
		t.Fatalf("good: %+v %v", c, err)
	}
	bad := []CustomInput{
		{Date: "2026-11-15", Theme: "new_year"},
		{Name: "x", Date: "15/11/2026", Theme: "new_year"},
		{Name: "x", Date: "2026-11-15", Theme: "nope"},
		{Name: "x", Date: "2026-11-15", ThemeFrom: "2026-11-16", Theme: "eid"},
		{Name: "x", Date: "2026-11-15", ThemeFrom: "2026-11-01", ThemeTo: "2027-03-01", Theme: "eid"},
		{Name: "x", Date: "2026-11-15", Countries: []string{"Kenya"}, Theme: "eid"},
		{Name: "x", Date: "2026-11-15", Theme: "eid", Accent: "#ffff00"},
	}
	for i, in := range bad {
		if err := ApplyCustom(&CustomOccasion{}, in); err == nil {
			t.Errorf("bad %d accepted", i)
		}
	}
}
