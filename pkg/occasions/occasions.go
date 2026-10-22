// Package occasions is the shared calendar of holidays, observances and
// commercial seasons (Mashujaa Day, Christmas, Halloween, Black Friday …).
//
// Two features read it:
//   - seasonal themes (agent "seasons", which owns this package and fills the
//     data with researched, sourced dates and adds custom occasions), and
//   - platform announcements (agent "broadcast"), which nudges the super admin
//     before important occasions so a message can be planned in time.
//
// The exported API below is the contract between them; keep it stable.
package occasions

import "time"

// Kinds of occasion.
const (
	KindPublicHoliday = "public_holiday" // official day off in a country
	KindObservance    = "observance"     // widely marked, not a day off (Halloween, Valentine's)
	KindCommercial    = "commercial"     // shopping season (Black Friday, back to school)
	KindCustom        = "custom"         // added by a super admin
)

// Occasion is one dated occurrence.
type Occasion struct {
	Key       string    `json:"key"`        // stable id of the occasion, e.g. "mashujaa_day", "halloween"
	Name      string    `json:"name"`       // "Mashujaa Day"
	Kind      string    `json:"kind"`       // see Kind* constants
	Date      time.Time `json:"date"`       // the day itself (date only, UTC midnight)
	Countries []string  `json:"countries"`  // ISO2 codes; empty = everywhere
	Theme     string    `json:"theme"`      // seasonal theme key ("" = none), e.g. "halloween", "kenya_national"
	ThemeFrom time.Time `json:"theme_from"` // when the theme starts showing (date only)
	ThemeTo   time.Time `json:"theme_to"`   // last day the theme shows (date only)
	Important bool      `json:"important"`  // worth a nudge to the super admin
	Note      string    `json:"note"`       // e.g. "date depends on the moon sighting — confirm"
	Source    string    `json:"source"`     // URL the date was checked against ("" for fixed dates)

	// Additions (seasons agent; additive — the fields above are the contract).

	// ExceptCountries: an "everywhere" occasion (Countries empty) that does
	// not apply in these countries because they have their own entry with
	// the same Key (e.g. Christmas Day is a public holiday in KE and AU and an
	// observance elsewhere).
	ExceptCountries []string `json:"except_countries,omitempty"`
	Greeting        string   `json:"greeting,omitempty"`  // one-line top-bar greeting, e.g. "Happy Mashujaa Day"
	Sources         []string `json:"sources,omitempty"`   // every URL checked (Source is the first)
	ID              string   `json:"id,omitempty"`        // custom occasions: their row id
	Palette         *Palette `json:"palette,omitempty"`   // custom occasions: accent overrides
	Tentative       bool     `json:"tentative,omitempty"` // date not final (moon sighting / lunar calendar)
}

// Palette overrides a theme's accent colour (custom occasions). Colours are
// #rrggbb; AccentDark is the dark-mode variant.
type Palette struct {
	Accent     string `json:"accent,omitempty"`
	AccentDark string `json:"accent_dark,omitempty"`
}

// Provider is implemented by the occasions service (registered at start-up).
type Provider interface {
	// Between returns occasions whose Date is in [from, to] (dates only),
	// for the given country (ISO2; "" = all countries), sorted by date.
	Between(from, to time.Time, country string) []Occasion
	// ThemeOn returns the occasion whose theme is showing on day for the
	// country (nil when none). When several overlap, the more specific
	// (country-specific, shorter window) wins.
	ThemeOn(day time.Time, country string) *Occasion
}

var current Provider

// Register sets the provider (called once from route registration).
func Register(p Provider) { current = p }

// Between is a convenience wrapper; returns nil until a provider is registered.
func Between(from, to time.Time, country string) []Occasion {
	if current == nil {
		return nil
	}
	return current.Between(from, to, country)
}

// ThemeOn is a convenience wrapper; returns nil until a provider is registered.
func ThemeOn(day time.Time, country string) *Occasion {
	if current == nil {
		return nil
	}
	return current.ThemeOn(day, country)
}
