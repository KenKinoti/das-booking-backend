package occasions

// Moon-sighting and lunar-calendar dates, researched October 2026. Add the
// next year here when its dates are announced (the calendar simply has no
// Eid / Diwali entry for a year missing from these tables).

// varDate is one table-driven date.
type varDate struct {
	Date      string   // YYYY-MM-DD (the date used in Kenya)
	Sources   []string // where it was checked
	Note      string
	Tentative bool // expected date; confirm closer to the date
}

const (
	SrcEidFitr26    = "https://citizen.digital/article/govt-declares-friday-a-public-holiday-to-celebrate-idd-ul-fitr-n379280"
	SrcEidFitrTAD   = "https://timeanddate.com/holidays/kenya/eid-al-fitr"
	SrcEidFitr27    = "https://calendar.center/gb/34/eid-al-fitr/"
	SrcEidAdha26    = "https://citizen.digital/article/govt-declares-may-27-public-holiday-to-mark-eid-ul-adha-n383374"
	SrcEidAdhaYears = "https://calendar-yearly.com/festivities/ke/eid-al-adha"
	SrcEidAdha27Alt = "https://www.calendarlabs.com/holidays/kenya/2027"
	SrcEid28Alt     = "https://www.calendarlabs.com/holidays/kenya/2028"
	SrcDiwali       = "https://publicholidays.com/diwali/"
	SrcDiwali26     = "https://www.smartpuja.com/blog/diwali-2026-date"
)

const moonNote = "Depends on the moon sighting — confirm closer to the date."

var eidFitr = map[int]varDate{
	2026: {Date: "2026-03-20", Sources: []string{SrcEidFitr26, SrcEidFitrTAD}, Note: "Declared a public holiday by the Interior CS (Friday 20 March 2026)."},
	2027: {Date: "2027-03-10", Sources: []string{SrcEidFitrTAD, SrcEidFitr27}, Tentative: true, Note: "Expected 9 or 10 March 2027. " + moonNote},
	2028: {Date: "2028-02-27", Sources: []string{SrcEidFitrTAD, SrcEid28Alt}, Tentative: true, Note: "Expected 26 or 27 February 2028. " + moonNote},
}

var eidAdha = map[int]varDate{
	2026: {Date: "2026-05-27", Sources: []string{SrcEidAdha26, SrcEidAdhaYears}, Note: "Declared a public holiday by the Interior CS (Wednesday 27 May 2026)."},
	2027: {Date: "2027-05-16", Sources: []string{SrcEidAdhaYears, SrcEidAdha27Alt}, Tentative: true, Note: "Expected 16 or 17 May 2027. " + moonNote},
	2028: {Date: "2028-05-05", Sources: []string{SrcEidAdhaYears, SrcEid28Alt}, Tentative: true, Note: "Expected 5 May 2028. " + moonNote},
}

var diwali = map[int]varDate{
	2026: {Date: "2026-11-08", Sources: []string{SrcDiwali, SrcDiwali26}},
	2027: {Date: "2027-10-29", Sources: []string{SrcDiwali}, Tentative: true, Note: "Lunar calendar (Kartik Amavasya) — confirm closer to the date."},
	2028: {Date: "2028-10-17", Sources: []string{SrcDiwali, SrcEid28Alt}, Tentative: true, Note: "Lunar calendar (Kartik Amavasya) — confirm closer to the date."},
}
