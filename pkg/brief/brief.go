// Package brief is the morning brief: a short daily note for each person who
// switches it on (My profile → Morning brief) with what needs attention
// today — bills to pay, money in, project progress (Jira and local), the
// opportunities worth going for and today's bookings, meetings and reminders.
//
// Every section shows only what the person may see (plan, business type and
// role through pkg/industry; money sections need an owner / admin / manager
// role) and empty sections are left out. The brief starts with a one-line
// headline ("3 bills due this week (KES 41,200), 2 Jira issues overdue, 1 new
// opportunity worth ~KES 60,000").
//
// Delivery: a scheduler (Postgres advisory-lock leader, every minute) sends
// each person's brief once per local date at their time — by email (pkg/mail,
// the shared branded template) and / or WhatsApp (pkg/wa: an approved
// template, or the fuller text while the person's 24-hour window is open).
// After downtime it catches up for 3 hours, after that the day is recorded as
// missed. Every brief is kept (brief_snapshots) for the history at
// /brief/:date with its delivery status per channel.
//
// Sections are pluggable: other packages add theirs with Register (key,
// title, order, provider) from an init function.
package brief

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Item is one line of a section.
type Item struct {
	Key      string  `json:"key,omitempty"` // stable id: "new since the last brief"
	Title    string  `json:"title"`
	Detail   string  `json:"detail,omitempty"`
	Amount   float64 `json:"amount,omitempty"`
	Currency string  `json:"currency,omitempty"`
	Value    string  `json:"value,omitempty"`    // the figure on the right ("KES 12,000", "60%")
	Link     string  `json:"link,omitempty"`     // app path, e.g. /bills?bill=…
	External string  `json:"external,omitempty"` // outside link (a Jira issue)
	Tag      string  `json:"tag,omitempty"`      // "Overdue", "Due today", "New", "Blocked"
	Tone     string  `json:"tone,omitempty"`     // danger | warn | ok | info
	Progress *int    `json:"progress,omitempty"` // projects: % done
}

// Group is a titled list inside a section ("Due this week", "Overdue").
type Group struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Items []Item `json:"items"`
	More  int    `json:"more,omitempty"` // further items not listed
	Link  string `json:"link,omitempty"` // "see all"
}

// Fragment is a section's contribution to the headline; lower Priority
// comes first.
type Fragment struct {
	Text     string `json:"text"`
	Priority int    `json:"priority"`
}

// Section is one part of the brief.
type Section struct {
	Key      string     `json:"key"`
	Title    string     `json:"title"`
	Icon     string     `json:"icon,omitempty"`
	Order    int        `json:"order"`
	Summary  string     `json:"summary,omitempty"`
	Groups   []Group    `json:"groups"`
	Notes    []string   `json:"notes,omitempty"`
	Link     string     `json:"link,omitempty"`
	Headline []Fragment `json:"headline,omitempty"`
}

// Empty reports whether the section has nothing to show.
func (s *Section) Empty() bool {
	if s == nil {
		return true
	}
	for _, g := range s.Groups {
		if len(g.Items) > 0 {
			return false
		}
	}
	return true
}

// Add appends a group when it has items (n is the total, so "and N more").
func (s *Section) Add(key, title string, items []Item, total int, link string) {
	if len(items) == 0 {
		return
	}
	g := Group{Key: key, Title: title, Items: items, Link: link}
	if total > len(items) {
		g.More = total - len(items)
	}
	s.Groups = append(s.Groups, g)
}

// Say adds a headline fragment.
func (s *Section) Say(priority int, format string, a ...any) {
	s.Headline = append(s.Headline, Fragment{Text: fmt.Sprintf(format, a...), Priority: priority})
}

// Hidden is a section that was not included, and why (previews only).
type Hidden struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Reason string `json:"reason"` // off | not_allowed | empty | error
	Note   string `json:"note,omitempty"`
}

// Brief is one person's brief for one local date.
type Brief struct {
	Date         string     `json:"date"`       // YYYY-MM-DD in the person's time zone
	DateLabel    string     `json:"date_label"` // "Wed 18 Nov"
	Timezone     string     `json:"timezone"`
	FirstName    string     `json:"first_name"`
	Greeting     string     `json:"greeting"` // "Good morning Ken"
	Headline     string     `json:"headline"`
	Organization string     `json:"organization"`
	Currency     string     `json:"currency"`
	Since        *time.Time `json:"since,omitempty"` // "new" means after this
	GeneratedAt  time.Time  `json:"generated_at"`
	Sections     []Section  `json:"sections"`
	Hidden       []Hidden   `json:"hidden,omitempty"`
}

// Section finds a section by key.
func (b *Brief) Section(key string) *Section {
	if b == nil {
		return nil
	}
	for i := range b.Sections {
		if b.Sections[i].Key == key {
			return &b.Sections[i]
		}
	}
	return nil
}

// keys lists the item keys of a section (for "new since the last brief").
func (b *Brief) keys(section string) map[string]bool {
	out := map[string]bool{}
	if s := b.Section(section); s != nil {
		for _, g := range s.Groups {
			for _, it := range g.Items {
				if it.Key != "" {
					out[it.Key] = true
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- viewer

// Viewer is the person a brief is for and what they may see.
type Viewer struct {
	UserID    string `json:"user_id"`
	OrgID     string `json:"organization_id"`
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Role      string `json:"role"`  // users.role
	Phone     string `json:"phone"` // users.phone as typed
	// Managing: owner / admin / manager (business-type role base, else
	// users.role) — money sections are only for them.
	Managing bool   `json:"managing"`
	Scope    string `json:"scope"` // all | own (bookings assigned to them)
	can      func(module, act string) bool
}

// Can reports whether the person may view module (plan, business type and role).
func (v *Viewer) Can(module string) bool {
	if v == nil {
		return false
	}
	if v.can == nil {
		return true
	}
	return v.can(module, "view")
}

// WithAccess returns a copy of v using can for module checks (tests, other packages).
func (v Viewer) WithAccess(can func(module, act string) bool) Viewer {
	v.can = can
	return v
}

// ---------------------------------------------------------------- registry

// Input is what a section provider gets.
type Input struct {
	DB       *gorm.DB
	Org      string
	OrgName  string
	Viewer   *Viewer
	Now      time.Time      // the instant the brief is built
	Loc      *time.Location // the person's time zone
	Today    time.Time      // the local date (midnight UTC of that date)
	Since    time.Time      // the last brief (or 24 hours ago): "new" means after this
	Currency string         // the organisation's home currency
	Previous *Brief         // the last brief sent to this person (nil when none)
	AppURL   string

	svc      *Service
	jiraNote string
}

// Money formats an amount for the brief ("KES 41,200").
func (in *Input) Money(v float64, cur string) string {
	if cur == "" {
		cur = in.Currency
	}
	return Money(v, cur)
}

// IsNew reports whether key was not in the same section of the previous brief.
func (in *Input) IsNew(section, key string) bool {
	if in.Previous == nil {
		return true
	}
	return !in.Previous.keys(section)[key]
}

// Provider builds a section; nil or an empty section is left out.
type Provider func(ctx context.Context, in *Input) (*Section, error)

type registration struct {
	Key, Title  string
	Order       int
	Icon        string
	Description string
	Provider    Provider
	Allowed     func(v *Viewer) bool
	Reason      string // shown when not allowed
}

// Option changes a registration.
type Option func(*registration)

// Requires limits a section to the people allowed (why: shown to the rest).
func Requires(allowed func(v *Viewer) bool, why string) Option {
	return func(r *registration) { r.Allowed, r.Reason = allowed, why }
}

// Icon sets the section's Font Awesome icon (e.g. "fa-solid fa-file-invoice").
func Icon(icon string) Option { return func(r *registration) { r.Icon = icon } }

// Describe sets the one-line description shown in My profile → Morning brief.
func Describe(text string) Option { return func(r *registration) { r.Description = text } }

var (
	regMu    sync.RWMutex
	registry = map[string]*registration{}
)

// Register adds a section (call from init). A later registration with the
// same key replaces the earlier one.
func Register(key, title string, order int, provider Provider, opts ...Option) {
	r := &registration{Key: key, Title: title, Order: order, Provider: provider}
	for _, o := range opts {
		o(r)
	}
	regMu.Lock()
	registry[key] = r
	regMu.Unlock()
}

// Unregister removes a section (tests).
func Unregister(key string) {
	regMu.Lock()
	delete(registry, key)
	regMu.Unlock()
}

func sections() []*registration {
	regMu.RLock()
	out := make([]*registration, 0, len(registry))
	for _, r := range registry {
		out = append(out, r)
	}
	regMu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// SectionInfo describes a registered section for the settings page.
type SectionInfo struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Icon        string `json:"icon,omitempty"`
	Description string `json:"description,omitempty"`
	Allowed     bool   `json:"allowed"`
	Reason      string `json:"reason,omitempty"`
	On          bool   `json:"on"`
}

// ---------------------------------------------------------------- headline

// MaxFragments is how many items the headline names.
const MaxFragments = 4

// Headline joins the sections' fragments, most important first. It goes for
// breadth: each section's most important fragment first (so bills, money,
// projects and opportunities all get a word), then further fragments while
// there is room.
func Headline(secs []Section) string {
	per := [][]Fragment{}
	for _, s := range secs {
		fs := []Fragment{}
		for _, f := range s.Headline {
			if strings.TrimSpace(f.Text) != "" {
				fs = append(fs, f)
			}
		}
		sort.SliceStable(fs, func(i, j int) bool { return fs[i].Priority < fs[j].Priority })
		if len(fs) > 0 {
			per = append(per, fs)
		}
	}
	if len(per) == 0 {
		return "Nothing needs your attention this morning — have a good day"
	}
	var picked []Fragment
	for round := 0; len(picked) < MaxFragments; round++ {
		var cands []Fragment
		for _, fs := range per {
			if round < len(fs) {
				cands = append(cands, fs[round])
			}
		}
		if len(cands) == 0 {
			break
		}
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].Priority < cands[j].Priority })
		for _, f := range cands {
			if len(picked) < MaxFragments {
				picked = append(picked, f)
			}
		}
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].Priority < picked[j].Priority })
	parts := make([]string, len(picked))
	for i, f := range picked {
		parts[i] = f.Text
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------- money

// Money formats an amount the brief's way: the currency code, thousands
// separators, no decimals from 1,000 up ("KES 41,200", "AUD 120.50").
func Money(v float64, cur string) string {
	cur = strings.ToUpper(strings.TrimSpace(cur))
	d := currency.Decimals(cur)
	if math.Abs(v) >= 1000 || d == 0 {
		d = 0
	}
	neg := v < 0
	s := fmt.Sprintf("%.*f", d, math.Abs(v))
	if strings.HasSuffix(s, ".00") {
		s = strings.TrimSuffix(s, ".00")
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := strings.TrimSpace(cur + " " + b.String() + frac)
	if neg {
		out = "-" + out
	}
	return out
}

// Approx formats an estimate ("~KES 60,000").
func Approx(v float64, cur string) string { return "~" + Money(roundNice(v), cur) }

// roundNice rounds an estimate to 2–3 significant figures.
func roundNice(v float64) float64 {
	a := math.Abs(v)
	switch {
	case a >= 100000:
		return math.Round(v/1000) * 1000
	case a >= 10000:
		return math.Round(v/100) * 100
	case a >= 1000:
		return math.Round(v/10) * 10
	}
	return math.Round(v)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func dayStr(t time.Time) string { return t.Format("2006-01-02") }

// localDay is the local calendar date of t in loc, as midnight UTC.
func localDay(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}
