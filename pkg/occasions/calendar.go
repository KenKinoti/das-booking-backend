package occasions

// The built-in calendar: Kenyan and Australian public holidays plus widely
// marked observances and shopping seasons. Fixed and rule-based dates are
// computed for any year; moon- or lunar-calendar dates (Eid, Diwali) come
// from the researched table in data.go (2026–2028) and are left out for
// years the table does not cover.
//
// Theme windows: national days show their theme from 3 days before to the
// day; Halloween 24–31 Oct; Christmas 1–26 Dec; New Year's Eve 27–31 Dec and
// New Year 1–2 Jan; Black Friday Monday→Sunday of its week; Easter from Palm
// Sunday to Easter Monday; Eid and Diwali two days either side.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sources the rules were checked against (October 2026).
const (
	SrcKenyaLaw   = "https://new.kenyalaw.org/akn/ke/act/1912/21" // Public Holidays Act (Cap. 110), Schedule + s. 4 Sunday rule
	SrcFairWork   = "https://www.fairwork.gov.au/employment-conditions/public-holidays"
	SrcFairWork26 = "https://www.fairwork.gov.au/employment-conditions/public-holidays/2026-public-holidays"
	SrcFairWork27 = "https://www.fairwork.gov.au/employment-conditions/public-holidays/2027-public-holidays"
	SrcKenyaCal   = "https://www.timeanddate.com/holidays/kenya"                                                // Easter Sunday, Christmas Eve, New Year's Eve, Mother's Day 2026
	SrcKenyaCal2  = "https://calendarific.com/holidays/2026/KE"                                                 // Mother's Day 10 May 2026, Christmas Eve, New Year's Eve
	SrcBlackFri   = "https://www.mozo.com.au/online-shopping/black-friday"                                      // Black Friday 27 Nov 2026, Cyber Monday 30 Nov 2026
	SrcFathersAU  = "https://manofmany.com/lifestyle/fathers-day-australia"                                     // first Sunday of September (6 Sep 2026, 5 Sep 2027)
	SrcFathersKE  = "https://k24.digital/411/fathers-day-charlene-shares-special-message-to-celebrate-ruto/amp" // third Sunday of June (15 Jun 2025)
	SrcHalloween  = "https://publicholidays.com.au/halloween/"
	SrcValentines = "https://publicholidays.com/valentines-day/"
	SrcWomensDay  = "https://www.unwomen.org/en/news/events/2018/03/international-womens-day-2018"
)

func fairWork(y int) string {
	switch y {
	case 2026:
		return SrcFairWork26
	case 2027:
		return SrcFairWork27
	}
	return SrcFairWork
}

type rule struct {
	key, name, kind, theme, greeting string
	countries, except                []string
	important                        bool
	// on returns the day in year y (ok=false: none that year) and, for
	// table-driven dates, the table entry.
	on       func(y int) (time.Time, *varDate, bool)
	window   func(d time.Time) (time.Time, time.Time)
	sources  func(y int) []string
	note     string
	keSunday bool   // Kenya, Schedule Part I: a Sunday moves to the next free day (s. 4)
	auRule   string // Australian weekend substitute rule (newyear, australia, anzac, christmas, boxing)
	// elsewhere: a table-driven observance outside Kenya — the table's
	// Kenya-specific note does not apply, only "confirm" for tentative dates.
	elsewhere bool
}

var (
	ke   = []string{"KE"}
	au   = []string{"AU"}
	keAU = []string{"KE", "AU"}
)

func fixed(m time.Month, d int) func(int) (time.Time, *varDate, bool) {
	return func(y int) (time.Time, *varDate, bool) { return Day(y, m, d), nil, true }
}

func easterOffset(n int) func(int) (time.Time, *varDate, bool) {
	return func(y int) (time.Time, *varDate, bool) { return Easter(y).AddDate(0, 0, n), nil, true }
}

func nth(m time.Month, wd time.Weekday, n int) func(int) (time.Time, *varDate, bool) {
	return func(y int) (time.Time, *varDate, bool) { return NthWeekday(y, m, wd, n), nil, true }
}

func table(t map[int]varDate) func(int) (time.Time, *varDate, bool) {
	return func(y int) (time.Time, *varDate, bool) {
		v, ok := t[y]
		if !ok {
			return time.Time{}, nil, false
		}
		d, ok := ParseDay(v.Date)
		return d, &v, ok
	}
}

// around: the theme shows from `before` days before to `after` days after.
func around(before, after int) func(time.Time) (time.Time, time.Time) {
	return func(d time.Time) (time.Time, time.Time) { return d.AddDate(0, 0, -before), d.AddDate(0, 0, after) }
}

var national = around(3, 0) // national days: 3 days before → the day

func span(m1 time.Month, d1 int, m2 time.Month, d2 int) func(time.Time) (time.Time, time.Time) {
	return func(d time.Time) (time.Time, time.Time) { return Day(d.Year(), m1, d1), Day(d.Year(), m2, d2) }
}

func src(urls ...string) func(int) []string { return func(int) []string { return urls } }

func keAUSrc(y int) []string { return []string{SrcKenyaLaw, fairWork(y)} }

var rules = []rule{
	// ---- Shared Kenya + Australia public holidays (and the observance elsewhere)
	{key: "new_year", name: "New Year's Day", kind: KindPublicHoliday, countries: keAU, theme: "new_year", greeting: "Happy New Year %Y", important: true,
		on: fixed(time.January, 1), window: around(0, 1), sources: keAUSrc, keSunday: true, auRule: "newyear"},
	{key: "new_year", name: "New Year's Day", kind: KindObservance, except: keAU, theme: "new_year", greeting: "Happy New Year %Y", important: true,
		on: fixed(time.January, 1), window: around(0, 1), sources: src(SrcKenyaCal)},
	{key: "good_friday", name: "Good Friday", kind: KindPublicHoliday, countries: keAU, theme: "easter", greeting: "Wishing you a peaceful Easter", important: true,
		on: easterOffset(-2), window: around(0, 0), sources: keAUSrc},
	{key: "easter", name: "Easter Sunday", kind: KindObservance, theme: "easter", greeting: "Happy Easter", important: true,
		on: easterOffset(0), window: around(7, 1), sources: src(SrcKenyaCal)},
	{key: "easter_monday", name: "Easter Monday", kind: KindPublicHoliday, countries: keAU, theme: "easter", greeting: "Happy Easter Monday", important: true,
		on: easterOffset(1), window: around(0, 0), sources: keAUSrc},
	{key: "christmas_day", name: "Christmas Day", kind: KindPublicHoliday, countries: keAU, theme: "christmas", greeting: "Merry Christmas", important: true,
		on: fixed(time.December, 25), window: span(time.December, 1, time.December, 26), sources: keAUSrc, keSunday: true, auRule: "christmas"},
	{key: "christmas_day", name: "Christmas Day", kind: KindObservance, except: keAU, theme: "christmas", greeting: "Merry Christmas", important: true,
		on: fixed(time.December, 25), window: span(time.December, 1, time.December, 26), sources: src(SrcKenyaCal2)},
	{key: "boxing_day", name: "Boxing Day", kind: KindPublicHoliday, countries: keAU, theme: "christmas", greeting: "Happy Boxing Day", important: true,
		on: fixed(time.December, 26), window: around(0, 0), sources: keAUSrc, keSunday: true, auRule: "boxing"},

	// ---- Kenya (Public Holidays Act, Cap. 110)
	{key: "labour_day", name: "Labour Day", kind: KindPublicHoliday, countries: ke, theme: "labour_day", greeting: "Happy Labour Day", important: true,
		on: fixed(time.May, 1), window: national, sources: src(SrcKenyaLaw), keSunday: true},
	{key: "eid_al_fitr", name: "Idd-ul-Fitr", kind: KindPublicHoliday, countries: ke, theme: "eid", greeting: "Eid Mubarak", important: true,
		on: table(eidFitr), window: around(2, 2), sources: src(SrcKenyaLaw), keSunday: true},
	{key: "madaraka_day", name: "Madaraka Day", kind: KindPublicHoliday, countries: ke, theme: "kenya_national", greeting: "Happy Madaraka Day", important: true,
		on: fixed(time.June, 1), window: national, sources: src(SrcKenyaLaw), keSunday: true},
	{key: "eid_al_adha", name: "Idd-ul-Azha", kind: KindPublicHoliday, countries: ke, theme: "eid", greeting: "Eid al-Adha Mubarak", important: true,
		on: table(eidAdha), window: around(2, 2), sources: src(SrcKenyaLaw),
		note: "Schedule Part II of the Public Holidays Act (persons of the Islamic faith); the Interior CS has gazetted it as a public holiday for everyone in recent years (27 May 2026)."},
	{key: "mazingira_day", name: "Mazingira Day", kind: KindPublicHoliday, countries: ke, theme: "mazingira", greeting: "Happy Mazingira Day", important: true,
		on: fixed(time.October, 10), window: national, sources: src(SrcKenyaLaw), keSunday: true,
		note: "Environment day (formerly Moi Day / Huduma Day / Utamaduni Day) — in the Act's Schedule as amended in 2024."},
	{key: "mashujaa_day", name: "Mashujaa Day", kind: KindPublicHoliday, countries: ke, theme: "kenya_national", greeting: "Happy Mashujaa Day", important: true,
		on: fixed(time.October, 20), window: national, sources: src(SrcKenyaLaw), keSunday: true},
	{key: "jamhuri_day", name: "Jamhuri Day", kind: KindPublicHoliday, countries: ke, theme: "kenya_national", greeting: "Happy Jamhuri Day", important: true,
		on: fixed(time.December, 12), window: national, sources: src(SrcKenyaLaw), keSunday: true},

	{key: "eid_al_fitr", name: "Eid al-Fitr", kind: KindObservance, except: ke, theme: "eid", greeting: "Eid Mubarak", important: true,
		on: table(eidFitr), window: around(2, 2), elsewhere: true, note: "Can differ by a day between countries."},
	{key: "eid_al_adha", name: "Eid al-Adha", kind: KindObservance, except: ke, theme: "eid", greeting: "Eid al-Adha Mubarak", important: true,
		on: table(eidAdha), window: around(2, 2), elsewhere: true, note: "Can differ by a day between countries."},

	// ---- Australia (national public holidays)
	{key: "australia_day", name: "Australia Day", kind: KindPublicHoliday, countries: au, theme: "australia_day", greeting: "Happy Australia Day", important: true,
		on: fixed(time.January, 26), window: national, sources: func(y int) []string { return []string{fairWork(y)} }, auRule: "australia"},
	{key: "anzac_day", name: "Anzac Day", kind: KindPublicHoliday, countries: au, theme: "remembrance", greeting: "Anzac Day — Lest we forget", important: true,
		on: fixed(time.April, 25), window: national, sources: func(y int) []string { return []string{fairWork(y)} }, auRule: "anzac"},

	// ---- Observances and shopping seasons
	{key: "valentines_day", name: "Valentine's Day", kind: KindObservance, theme: "valentines", greeting: "Happy Valentine's Day", important: true,
		on: fixed(time.February, 14), window: around(4, 0), sources: src(SrcValentines)},
	{key: "womens_day", name: "International Women's Day", kind: KindObservance, theme: "womens_day", greeting: "Happy International Women's Day", important: true,
		on: fixed(time.March, 8), window: national, sources: src(SrcWomensDay)},
	{key: "mothers_day", name: "Mother's Day", kind: KindObservance, countries: keAU, theme: "mothers_day", greeting: "Happy Mother's Day", important: true,
		on: nth(time.May, time.Sunday, 2), window: national, sources: src(SrcKenyaCal2, SrcKenyaCal), note: "Second Sunday of May in Kenya and Australia."},
	{key: "fathers_day", name: "Father's Day", kind: KindObservance, countries: ke, theme: "fathers_day", greeting: "Happy Father's Day", important: true,
		on: nth(time.June, time.Sunday, 3), window: national, sources: src(SrcFathersKE), note: "Third Sunday of June in Kenya."},
	{key: "fathers_day", name: "Father's Day", kind: KindObservance, countries: au, theme: "fathers_day", greeting: "Happy Father's Day", important: true,
		on: nth(time.September, time.Sunday, 1), window: national, sources: src(SrcFathersAU), note: "First Sunday of September in Australia."},
	{key: "halloween", name: "Halloween", kind: KindObservance, theme: "halloween", greeting: "Happy Halloween", important: true,
		on: fixed(time.October, 31), window: span(time.October, 24, time.October, 31), sources: src(SrcHalloween)},
	{key: "diwali", name: "Diwali", kind: KindObservance, theme: "diwali", greeting: "Happy Diwali", important: true,
		on: table(diwali), window: around(2, 2), sources: src(SrcDiwali),
		note: "In Kenya a public holiday for persons of the Hindu faith (Public Holidays Act, Schedule Part III)."},
	{key: "black_friday", name: "Black Friday", kind: KindCommercial, theme: "black_friday", greeting: "It's Black Friday week", important: true,
		on: func(y int) (time.Time, *varDate, bool) { return BlackFriday(y), nil, true }, window: around(4, 2), sources: src(SrcBlackFri),
		note: "Day after US Thanksgiving (4th Thursday of November); the sales run all week."},
	{key: "cyber_monday", name: "Cyber Monday", kind: KindCommercial, theme: "black_friday", greeting: "Happy Cyber Monday",
		on: func(y int) (time.Time, *varDate, bool) { return CyberMonday(y), nil, true }, window: around(0, 0), sources: src(SrcBlackFri)},
	{key: "christmas_season", name: "Christmas shopping season", kind: KindCommercial,
		on: fixed(time.December, 1), sources: src(SrcKenyaCal2), note: "Festive trading from 1 December — the Christmas theme shows from today."},
	{key: "christmas_eve", name: "Christmas Eve", kind: KindObservance, theme: "christmas", greeting: "Merry Christmas Eve",
		on: fixed(time.December, 24), window: around(0, 0), sources: src(SrcKenyaCal2)},
	{key: "new_years_eve", name: "New Year's Eve", kind: KindObservance, theme: "new_year", greeting: "Countdown to %N", important: true,
		on: fixed(time.December, 31), window: span(time.December, 27, time.December, 31), sources: src(SrcKenyaCal2)},
}

var builtinCache sync.Map // year → []Occasion

// Builtin returns the built-in occasions of year y (sorted by date). The
// result is shared: callers must not modify it.
func Builtin(y int) []Occasion {
	if v, ok := builtinCache.Load(y); ok {
		return v.([]Occasion)
	}
	out := buildYear(y)
	builtinCache.Store(y, out)
	return out
}

func buildYear(y int) []Occasion {
	var out []Occasion
	var keHolidays = map[time.Time]bool{}
	type pending struct {
		i int
		r rule
	}
	var sundays []pending
	for _, r := range rules {
		d, v, ok := r.on(y)
		if !ok {
			continue
		}
		o := Occasion{Key: r.key, Name: r.name, Kind: r.kind, Date: d, Countries: append([]string(nil), r.countries...),
			ExceptCountries: append([]string(nil), r.except...), Theme: r.theme, Important: r.important}
		if o.Countries == nil {
			o.Countries = []string{}
		}
		if r.window != nil && r.theme != "" {
			o.ThemeFrom, o.ThemeTo = r.window(d)
		} else {
			o.ThemeFrom, o.ThemeTo = d, d
		}
		o.Greeting = strings.NewReplacer("%Y", fmt.Sprint(y), "%N", fmt.Sprint(y+1)).Replace(r.greeting)
		var notes []string
		if r.note != "" {
			notes = append(notes, r.note)
		}
		if r.sources != nil {
			o.Sources = append(o.Sources, r.sources(y)...)
		}
		if v != nil {
			o.Sources = append(append([]string(nil), v.Sources...), o.Sources...)
			o.Tentative = v.Tentative
			switch {
			case r.elsewhere && v.Tentative:
				notes = append([]string{moonNote}, notes...)
			case !r.elsewhere && v.Note != "":
				notes = append([]string{v.Note}, notes...)
			}
		}
		o.Sources = dedupe(o.Sources)
		if len(o.Sources) > 0 {
			o.Source = o.Sources[0]
		}
		if r.auRule != "" {
			if n := auWeekendNote(r.auRule, d); n != "" {
				notes = append(notes, n)
			}
		}
		o.Note = strings.Join(notes, " ")
		if r.kind == KindPublicHoliday && has(r.countries, "KE") {
			keHolidays[d] = true
		}
		out = append(out, o)
		if r.keSunday && d.Weekday() == time.Sunday && has(r.countries, "KE") {
			sundays = append(sundays, pending{len(out) - 1, r})
		}
	}
	// Kenya, Public Holidays Act s. 4: a Part I day on a Sunday moves to the
	// first following day that is not already a public holiday.
	for _, p := range sundays {
		o := &out[p.i]
		next := o.Date.AddDate(0, 0, 1)
		for keHolidays[next] {
			next = next.AddDate(0, 0, 1)
		}
		keHolidays[next] = true
		prefix := ""
		if len(o.Countries) > 1 {
			prefix = "Kenya: "
		}
		n := prefix + "falls on a Sunday — " + next.Format("Monday 2 January 2006") + " is the public holiday (Public Holidays Act, s. 4)."
		o.Note = strings.TrimSpace(n + " " + o.Note)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}

func auWeekendNote(kind string, d time.Time) string {
	wd := d.Weekday()
	if wd != time.Saturday && wd != time.Sunday {
		return ""
	}
	f := func(t time.Time) string { return t.Format("Monday 2 January") }
	sat := wd == time.Saturday
	switch kind {
	case "newyear":
		if sat {
			return "Australia: falls on a Saturday — most states add " + f(d.AddDate(0, 0, 2)) + "; check fairwork.gov.au."
		}
		return "Australia: falls on a Sunday — " + f(d.AddDate(0, 0, 1)) + " is the holiday in most states; check fairwork.gov.au."
	case "australia":
		n := 2
		if !sat {
			n = 1
		}
		return "Australia: falls on a weekend — " + f(d.AddDate(0, 0, n)) + " is the public holiday."
	case "anzac":
		if sat {
			return "Australia: falls on a Saturday — only some states add the Monday; check fairwork.gov.au."
		}
		return "Australia: falls on a Sunday — most states add " + f(d.AddDate(0, 0, 1)) + "; check fairwork.gov.au."
	case "christmas":
		if sat {
			return "Australia: additional public holiday " + f(d.AddDate(0, 0, 2)) + "."
		}
		return "Australia: additional public holiday " + f(d.AddDate(0, 0, 2)) + " (Boxing Day is the Monday)."
	case "boxing":
		if sat {
			return "Australia: additional public holiday " + f(d.AddDate(0, 0, 2)) + " (Tasmania moves Boxing Day to it)."
		}
		return "Australia: additional public holiday " + f(d.AddDate(0, 0, 2)) + "."
	}
	return ""
}

func has(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// matchesCountry reports whether o applies in country (ISO2; "" = any).
func (o Occasion) matchesCountry(country string) bool {
	if country == "" {
		return true
	}
	if len(o.Countries) > 0 && !has(o.Countries, country) {
		return false
	}
	return !has(o.ExceptCountries, country)
}
