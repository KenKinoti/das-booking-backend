package occasions

import (
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// CustomOccasion is an occasion added by a super admin (occasion_custom).
type CustomOccasion struct {
	ID         string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	Name       string    `json:"name" gorm:"type:varchar(120);not null"`
	Date       time.Time `json:"date" gorm:"type:date;not null"`
	ThemeFrom  time.Time `json:"theme_from" gorm:"type:date;not null"`
	ThemeTo    time.Time `json:"theme_to" gorm:"type:date;not null"`
	Countries  string    `json:"-" gorm:"type:varchar(400)"` // comma-separated ISO2; "" = everywhere
	Theme      string    `json:"theme" gorm:"type:varchar(40);not null"`
	Accent     string    `json:"accent" gorm:"type:varchar(9)"`
	AccentDark string    `json:"accent_dark" gorm:"type:varchar(9)"`
	Greeting   string    `json:"greeting" gorm:"type:varchar(140)"`
	Note       string    `json:"note" gorm:"type:text"`
	Important  bool      `json:"important" gorm:"not null"`
	Enabled    bool      `json:"enabled" gorm:"not null"`
	Yearly     bool      `json:"yearly" gorm:"not null"` // repeats every year on the same day
	CreatedBy  string    `json:"created_by" gorm:"type:varchar(255)"`
	UpdatedBy  string    `json:"updated_by" gorm:"type:varchar(255)"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// TableName is fixed.
func (CustomOccasion) TableName() string { return "occasion_custom" }

// CountryList returns the countries as a slice.
func (c CustomOccasion) CountryList() []string {
	out := []string{}
	for _, s := range strings.Split(c.Countries, ",") {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Key is the custom occasion's stable key.
func (c CustomOccasion) Key() string {
	id := c.ID
	if len(id) > 8 {
		id = id[:8]
	}
	return "custom_" + id
}

// Toggle switches a built-in occasion's theme (or every theme, key "*") on
// or off platform-wide (occasion_toggles). No row = on.
type Toggle struct {
	Key       string    `json:"key" gorm:"type:varchar(80);primaryKey"`
	Enabled   bool      `json:"enabled" gorm:"not null"`
	UpdatedBy string    `json:"updated_by" gorm:"type:varchar(255)"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Toggle) TableName() string { return "occasion_toggles" }

// GlobalKey is the toggle row that switches every seasonal theme.
const GlobalKey = "*"

// Service is the occasions Provider: the built-in calendar plus custom
// occasions and theme switches from the database (cached briefly).
type Service struct {
	DB  *gorm.DB
	TTL time.Duration // cache lifetime (default 30s)

	mu       sync.RWMutex
	loadedAt time.Time
	customs  []CustomOccasion
	toggles  map[string]bool
}

// NewService builds the provider.
func NewService(db *gorm.DB) *Service { return &Service{DB: db, TTL: 30 * time.Second} }

// Invalidate drops the cache (after a write).
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.loadedAt = time.Time{}
	s.mu.Unlock()
}

func (s *Service) load() ([]CustomOccasion, map[string]bool) {
	s.mu.RLock()
	ttl := s.TTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	if !s.loadedAt.IsZero() && time.Since(s.loadedAt) < ttl {
		c, t := s.customs, s.toggles
		s.mu.RUnlock()
		return c, t
	}
	s.mu.RUnlock()
	var customs []CustomOccasion
	toggles := map[string]bool{}
	if s.DB != nil {
		if err := s.DB.Order("date").Find(&customs).Error; err != nil {
			log.Printf("occasions: load custom occasions: %v", err)
		}
		var rows []Toggle
		if err := s.DB.Find(&rows).Error; err != nil {
			log.Printf("occasions: load toggles: %v", err)
		}
		for _, r := range rows {
			toggles[r.Key] = r.Enabled
		}
	}
	s.mu.Lock()
	s.customs, s.toggles, s.loadedAt = customs, toggles, time.Now()
	s.mu.Unlock()
	return customs, toggles
}

// GlobalEnabled: seasonal themes are on platform-wide.
func (s *Service) GlobalEnabled() bool {
	_, t := s.load()
	on, ok := t[GlobalKey]
	return !ok || on
}

// OccasionEnabled: the occasion's theme is not switched off.
func (s *Service) OccasionEnabled(key string) bool {
	_, t := s.load()
	on, ok := t[key]
	return !ok || on
}

// customOccurrences expands the custom occasions for years y1..y2.
func customOccurrences(customs []CustomOccasion, y1, y2 int, enabledOnly bool) []Occasion {
	var out []Occasion
	for _, c := range customs {
		if enabledOnly && !c.Enabled {
			continue
		}
		base := customOccasion(c)
		if !c.Yearly {
			out = append(out, base)
			continue
		}
		for y := y1; y <= y2; y++ {
			shift := y - c.Date.Year()
			o := base
			o.Date = shiftYears(c.Date, shift)
			o.ThemeFrom = shiftYears(c.ThemeFrom, shift)
			o.ThemeTo = shiftYears(c.ThemeTo, shift)
			out = append(out, o)
		}
	}
	return out
}

func shiftYears(d time.Time, n int) time.Time {
	y := d.Year() + n
	day := d.Day()
	if d.Month() == time.February && day == 29 && !(y%4 == 0 && (y%100 != 0 || y%400 == 0)) {
		day = 28
	}
	return Day(y, d.Month(), day)
}

func customOccasion(c CustomOccasion) Occasion {
	o := Occasion{ID: c.ID, Key: c.Key(), Name: c.Name, Kind: KindCustom, Date: DateOnly(c.Date), Countries: c.CountryList(),
		Theme: c.Theme, ThemeFrom: DateOnly(c.ThemeFrom), ThemeTo: DateOnly(c.ThemeTo), Important: c.Important, Note: c.Note, Greeting: c.Greeting}
	if c.Accent != "" || c.AccentDark != "" {
		o.Palette = &Palette{Accent: c.Accent, AccentDark: c.AccentDark}
	}
	return o
}

// occurrences returns built-in + (enabled) custom occasions of years y1..y2.
func (s *Service) occurrences(y1, y2 int) []Occasion {
	customs, _ := s.load()
	var out []Occasion
	for y := y1; y <= y2; y++ {
		out = append(out, Builtin(y)...)
	}
	return append(out, customOccurrences(customs, y1, y2, true)...)
}

// Between implements Provider: occasions dated in [from, to] for country
// ("" = all countries), sorted by date. Theme switches do not hide
// occasions here — they only affect which theme shows.
func (s *Service) Between(from, to time.Time, country string) []Occasion {
	from, to = DateOnly(from), DateOnly(to)
	if to.Before(from) {
		return nil
	}
	country = strings.ToUpper(strings.TrimSpace(country))
	var out []Occasion
	for _, o := range s.occurrences(from.Year(), to.Year()) {
		if inRange(o.Date, from, to) && o.matchesCountry(country) {
			out = append(out, copyOccasion(o))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		if out[i].Important != out[j].Important {
			return out[i].Important
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ThemeOn implements Provider: the occasion whose theme shows on day for
// country ("" = any country), or nil. Honours the platform switches.
func (s *Service) ThemeOn(day time.Time, country string) *Occasion {
	return s.themeOn(day, strings.ToUpper(strings.TrimSpace(country)), false)
}

// ThemeOnGlobal is ThemeOn for an organisation whose country is unknown:
// only occasions marked everywhere count.
func (s *Service) ThemeOnGlobal(day time.Time) *Occasion { return s.themeOn(day, "", true) }

func (s *Service) themeOn(day time.Time, country string, globalOnly bool) *Occasion {
	if !s.GlobalEnabled() {
		return nil
	}
	day = DateOnly(day)
	var cands []Occasion
	for _, o := range s.occurrences(day.Year()-1, day.Year()+1) {
		if o.Theme == "" || !inRange(day, o.ThemeFrom, o.ThemeTo) {
			continue
		}
		if globalOnly && len(o.Countries) > 0 {
			continue
		}
		if !o.matchesCountry(country) || (o.Kind != KindCustom && !s.OccasionEnabled(o.Key)) {
			continue
		}
		cands = append(cands, o)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool { return wins(cands[i], cands[j], day) })
	o := copyOccasion(cands[0])
	return &o
}

// wins: precedence when theme windows overlap — a super admin's custom
// occasion, then the occasion that is today itself, then the more specific
// (country-specific, then the shorter window), then the important one.
func wins(a, b Occasion, day time.Time) bool {
	if (a.Kind == KindCustom) != (b.Kind == KindCustom) {
		return a.Kind == KindCustom
	}
	if ad, bd := a.Date.Equal(day), b.Date.Equal(day); ad != bd {
		return ad
	}
	if as, bs := len(a.Countries) > 0, len(b.Countries) > 0; as != bs {
		return as
	}
	if aw, bw := days(a.ThemeFrom, a.ThemeTo), days(b.ThemeFrom, b.ThemeTo); aw != bw {
		return aw < bw
	}
	if a.Important != b.Important {
		return a.Important
	}
	return a.Key < b.Key
}

func copyOccasion(o Occasion) Occasion {
	o.Countries = append([]string{}, o.Countries...)
	o.ExceptCountries = append([]string(nil), o.ExceptCountries...)
	o.Sources = append([]string(nil), o.Sources...)
	if o.Palette != nil {
		p := *o.Palette
		o.Palette = &p
	}
	return o
}

// Next returns the next occurrence (on or after day) of the occasion key —
// or, when key is a theme, of the first occasion using that theme — for
// country ("" = any), looking up to two years ahead. Used for previews.
func (s *Service) Next(key string, day time.Time, country string) *Occasion {
	day = DateOnly(day)
	customs, _ := s.load()
	all := append(s.occurrences(day.Year(), day.Year()+2), customOccurrences(customs, day.Year(), day.Year()+2, false)...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].Date.Before(all[j].Date) })
	for _, pass := range []func(o Occasion) bool{
		func(o Occasion) bool { return o.Key == key || o.ID == key },
		func(o Occasion) bool { return o.Theme == key && o.Important },
		func(o Occasion) bool { return o.Theme == key },
	} {
		var fallback *Occasion
		for _, o := range all {
			if !pass(o) || o.Theme == "" {
				continue
			}
			if !o.matchesCountry(country) && country != "" {
				if fallback == nil && !o.ThemeTo.Before(day) {
					c := copyOccasion(o)
					fallback = &c
				}
				continue
			}
			if !o.ThemeTo.Before(day) {
				c := copyOccasion(o)
				return &c
			}
		}
		if fallback != nil {
			return fallback
		}
	}
	return nil
}

// Today is the current calendar day in loc — or OCCASIONS_TODAY
// (YYYY-MM-DD), a test-only override of the occasions calendar's clock.
func Today(loc *time.Location) time.Time {
	if d, ok := ParseDay(strings.TrimSpace(os.Getenv("OCCASIONS_TODAY"))); ok {
		return d
	}
	if loc == nil {
		loc = time.UTC
	}
	return DateOnly(time.Now().In(loc))
}
