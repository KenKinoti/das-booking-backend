package brief

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// Service builds and delivers briefs.
type Service struct {
	DB *gorm.DB
	FX *fxrates.Service
	// AppURL is the web app origin (PUBLIC_APP_URL) for links.
	AppURL string
	// Access returns a person's access (pkg/industry: plan → type → role).
	// nil: every module is visible and the legacy role decides "managing".
	Access func(org, user, role string) (industry.Access, error)
	// SendMail sends an email as the organisation (default mail.Shared(db).SendAs).
	SendMail func(ctx context.Context, org string, msg mail.Message) error
	// JiraSync pulls the organisation's Jira projects now (pkg/projects2);
	// nil: Jira figures are used as they are.
	JiraSync func(ctx context.Context, org string) error
	// LogoURL returns the organisation's absolute logo URL ("" for none).
	LogoURL func(org string) string
	// Now is the real clock (tests replace it); the test clock adds an offset.
	Now func() time.Time
	// TestClock allows moving the clock (BRIEF_TEST_CLOCK=1, never in production).
	TestClock bool
	// CatchUp is how late a brief may still go out after downtime (default 3 h).
	CatchUp time.Duration

	mu        sync.Mutex
	offset    time.Duration
	jiraFail  map[string]time.Time
	lastPrune time.Time

	srcOnce sync.Once
	src     *sources
}

// New builds a service.
func New(db *gorm.DB, fx *fxrates.Service) *Service {
	if fx == nil {
		fx = &fxrates.Service{DB: db}
	}
	return &Service{DB: db, FX: fx, Now: time.Now, CatchUp: 3 * time.Hour, jiraFail: map[string]time.Time{}}
}

// now is the service clock (the test clock's offset applied).
func (s *Service) now() time.Time {
	n := time.Now()
	if s.Now != nil {
		n = s.Now()
	}
	s.mu.Lock()
	off := s.offset
	s.mu.Unlock()
	return n.Add(off)
}

// Clock returns the service clock (for other packages' services it builds).
func (s *Service) Clock() time.Time { return s.now() }

// SetClock moves the clock to at (zero time: back to the real clock).
func (s *Service) SetClock(at time.Time) {
	real := time.Now()
	if s.Now != nil {
		real = s.Now()
	}
	s.mu.Lock()
	if at.IsZero() {
		s.offset = 0
	} else {
		s.offset = at.Sub(real)
	}
	s.mu.Unlock()
}

func (s *Service) sendMail(ctx context.Context, org string, msg mail.Message) error {
	if s.SendMail != nil {
		return s.SendMail(ctx, org, msg)
	}
	_, err := mail.Shared(s.DB).SendAs(ctx, org, msg)
	return err
}

// ---------------------------------------------------------------- zones

// OrgZone is the time zone a person's brief uses by default: the
// organisation's (orgprefs), unless it doesn't belong to the organisation's
// country (e.g. a Kenyan business saved as Australia/Adelaide) — then the
// country's zone.
func OrgZone(db *gorm.DB, org string) string {
	p := orgprefs.Get(db, org)
	tz := strings.TrimSpace(p.Timezone)
	if d, ok := orgprefs.DefaultsFor(p.CountryCode); ok && p.CountryCode != "" {
		match := false
		for _, z := range d.Timezones {
			if z == tz {
				match = true
			}
		}
		if !match && d.Timezone != "" {
			tz = d.Timezone
		}
	}
	if _, err := time.LoadLocation(tz); tz == "" || err != nil {
		tz = orgprefs.DefaultTimezone
	}
	return tz
}

// zoneOf is the effective zone of a person's settings.
func (s *Service) zoneOf(st Settings) (string, *time.Location) {
	tz := strings.TrimSpace(st.Timezone)
	if tz == "" {
		tz = OrgZone(s.DB, st.OrganizationID)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		tz = OrgZone(s.DB, st.OrganizationID)
		loc, _ = time.LoadLocation(tz)
	}
	if loc == nil {
		tz, loc = "UTC", time.UTC
	}
	return tz, loc
}

// ---------------------------------------------------------------- people

var managingRoles = map[string]bool{"super_admin": true, "owner": true, "admin": true, "org_admin": true, "manager": true}

// ErrNoUser: the person is not an active user.
var ErrNoUser = errors.New("user not found")

// Viewer loads a person and what they may see.
func (s *Service) Viewer(userID string) (*Viewer, error) {
	var u struct {
		ID, Email, FirstName, LastName, Role, Phone, OrganizationID string
	}
	s.DB.Raw(`SELECT id, COALESCE(email,'') AS email, COALESCE(first_name,'') AS first_name, COALESCE(last_name,'') AS last_name,
		COALESCE(role,'') AS role, COALESCE(phone,'') AS phone, COALESCE(organization_id,'') AS organization_id
		FROM users WHERE id = ? AND deleted_at IS NULL AND (is_active IS NULL OR is_active) LIMIT 1`, userID).Scan(&u)
	if u.ID == "" || u.OrganizationID == "" {
		return nil, ErrNoUser
	}
	v := &Viewer{UserID: u.ID, OrgID: u.OrganizationID, Email: strings.TrimSpace(u.Email), FirstName: strings.TrimSpace(u.FirstName),
		LastName: strings.TrimSpace(u.LastName), Role: u.Role, Phone: strings.TrimSpace(u.Phone), Scope: industry.ScopeAll}
	v.Managing = managingRoles[strings.ToLower(u.Role)]
	if s.Access != nil {
		if a, err := s.Access(u.OrganizationID, u.ID, u.Role); err == nil {
			v.can = a.Can
			v.Scope = a.Scope()
			switch {
			case a.SuperAdmin:
				v.Managing = true
			case a.Role != nil:
				v.Managing = a.Role.Base == industry.BaseAdmin || a.Role.Base == industry.BaseManager
			}
		} else {
			log.Printf("brief: access for %s: %v", u.ID, err)
		}
	}
	return v, nil
}

// ---------------------------------------------------------------- settings

// SettingsFor returns a person's settings (the defaults when none are saved).
func (s *Service) SettingsFor(userID, org string) Settings {
	var st Settings
	s.DB.Where("user_id = ?", userID).Limit(1).Find(&st)
	if st.UserID == "" {
		st = Settings{UserID: userID, OrganizationID: org, Time: DefaultTime, Days: DaysEveryDay, Email: true}
	}
	if st.OrganizationID == "" {
		st.OrganizationID = org
	}
	if st.Time == "" {
		st.Time = DefaultTime
	}
	if st.Days == "" {
		st.Days = DaysEveryDay
	}
	if st.SectionsOff == nil {
		st.SectionsOff = []string{}
	}
	return st
}

// SectionOn reports whether the person has the section switched on.
func (st Settings) SectionOn(key string) bool {
	for _, k := range st.SectionsOff {
		if k == key {
			return false
		}
	}
	return true
}

// Input is a change to a person's settings (nil = keep).
type SettingsInput struct {
	Enabled     *bool           `json:"enabled"`
	Time        *string         `json:"time"`
	Days        *string         `json:"days"`
	Timezone    *string         `json:"timezone"` // "" = the organisation's
	Email       *bool           `json:"email"`
	WhatsApp    *bool           `json:"whatsapp"`
	Phone       *string         `json:"phone"` // updates users.phone
	Sections    map[string]bool `json:"sections"`
	AlsoDigests *bool           `json:"also_digests"`
}

// ValidationError is a user-facing input error.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{Msg: fmt.Sprintf(format, a...)} }

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

// NormalTime turns "8:00", "08:00", "0800" into "08:00" (ok false when invalid).
func NormalTime(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) == 4 && !strings.Contains(s, ":") {
		s = s[:2] + ":" + s[2:]
	}
	if len(s) == 4 && s[1] == ':' {
		s = "0" + s
	}
	if !hhmm.MatchString(s) {
		return "", false
	}
	return s, true
}

// SaveSettings validates and stores a person's settings. A WhatsApp number
// given here is normalised with the organisation's country and saved as the
// person's phone (users.phone).
func (s *Service) SaveSettings(v *Viewer, in SettingsInput) (Settings, error) {
	cur := s.SettingsFor(v.UserID, v.OrgID)
	next := cur
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	if in.Time != nil {
		t, ok := NormalTime(*in.Time)
		if !ok {
			return cur, invalid("Choose a time like 08:00")
		}
		next.Time = t
	}
	if in.Days != nil {
		switch strings.TrimSpace(*in.Days) {
		case DaysEveryDay, DaysWeekdays:
			next.Days = strings.TrimSpace(*in.Days)
		default:
			return cur, invalid("Choose every day or weekdays")
		}
	}
	if in.Timezone != nil {
		tz := strings.TrimSpace(*in.Timezone)
		if tz != "" {
			if _, err := time.LoadLocation(tz); err != nil {
				return cur, invalid("Unknown time zone %s", tz)
			}
		}
		next.Timezone = tz
	}
	if in.Email != nil {
		next.Email = *in.Email
	}
	if in.WhatsApp != nil {
		next.WhatsApp = *in.WhatsApp
	}
	if in.AlsoDigests != nil {
		next.AlsoDigests = *in.AlsoDigests
	}
	if in.Sections != nil {
		off := map[string]bool{}
		for _, k := range cur.SectionsOff {
			off[k] = true
		}
		for k, on := range in.Sections {
			off[k] = !on
		}
		next.SectionsOff = []string{}
		for _, r := range sections() {
			if off[r.Key] {
				next.SectionsOff = append(next.SectionsOff, r.Key)
			}
		}
		for k, isOff := range off { // sections registered later (other packages) keep their choice
			if isOff && !contains(next.SectionsOff, k) {
				next.SectionsOff = append(next.SectionsOff, k)
			}
		}
	}
	phone := v.Phone
	if in.Phone != nil {
		raw := strings.TrimSpace(*in.Phone)
		if raw == "" {
			phone = ""
		} else {
			d, err := wa.NormalizeNumber(raw, orgprefs.Country(s.DB, v.OrgID))
			if err != nil {
				return cur, invalid("%s", strings.ToUpper(err.Error()[:1])+err.Error()[1:])
			}
			phone = "+" + d
		}
	}
	if next.Enabled && !next.Email && !next.WhatsApp {
		return cur, invalid("Choose email, WhatsApp or both")
	}
	if next.WhatsApp && strings.TrimSpace(phone) == "" {
		return cur, invalid("Add your WhatsApp number (with the country code, e.g. +254 712 345 678)")
	}
	if next.Email && next.Enabled && v.Email == "" {
		return cur, invalid("Your account has no email address — use WhatsApp")
	}
	now := s.now()
	if next.Enabled != cur.Enabled || next.Time != cur.Time || next.Days != cur.Days || next.Timezone != cur.Timezone || cur.ScheduleFrom.IsZero() {
		next.ScheduleFrom = now
	}
	next.UserID, next.OrganizationID, next.UpdatedAt = v.UserID, v.OrgID, now
	if next.CreatedAt.IsZero() {
		next.CreatedAt = now
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if in.Phone != nil && phone != v.Phone {
			if err := tx.Exec(`UPDATE users SET phone = ?, updated_at = ? WHERE id = ?`, phone, time.Now(), v.UserID).Error; err != nil {
				return err
			}
		}
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&next).Error
	})
	if err != nil {
		return cur, err
	}
	v.Phone = phone
	return s.SettingsFor(v.UserID, v.OrgID), nil
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// Sections lists the registered sections for a person (allowed, on).
func (s *Service) Sections(v *Viewer, st Settings) []SectionInfo {
	out := []SectionInfo{}
	for _, r := range sections() {
		si := SectionInfo{Key: r.Key, Title: r.Title, Icon: r.Icon, Description: r.Description, Allowed: true, On: st.SectionOn(r.Key)}
		if r.Allowed != nil && !r.Allowed(v) {
			si.Allowed, si.Reason = false, r.Reason
		}
		out = append(out, si)
	}
	return out
}

// ---------------------------------------------------------------- build

// BuildOptions change how a brief is built.
type BuildOptions struct {
	// SyncJira refreshes Jira first when the last sync is over an hour old.
	SyncJira bool
	// JiraTimeout bounds that sync (default 60 s).
	JiraTimeout time.Duration
	// Date builds for this local date (default: today in the person's zone).
	Date string
}

// Build makes a person's brief now.
func (s *Service) Build(ctx context.Context, v *Viewer, st Settings, opt BuildOptions) *Brief {
	now := s.now()
	tz, loc := s.zoneOf(st)
	today := localDay(now, loc)
	if d, err := time.Parse("2006-01-02", opt.Date); err == nil {
		today = d
	}
	var org struct{ Name string }
	s.DB.Raw(`SELECT COALESCE(name,'') AS name FROM organizations WHERE id = ?`, v.OrgID).Scan(&org)
	in := &Input{DB: s.DB, Org: v.OrgID, OrgName: org.Name, Viewer: v, Now: now, Loc: loc, Today: today,
		Currency: orgprefs.Currency(s.DB, v.OrgID), AppURL: strings.TrimRight(s.AppURL, "/"), svc: s}
	in.Since = now.Add(-24 * time.Hour)
	if prev := s.previous(v.UserID, today); prev != nil {
		in.Previous = prev.Data
		if prev.CreatedAt.Before(now) && prev.CreatedAt.After(now.Add(-14*24*time.Hour)) {
			in.Since = prev.CreatedAt
		}
	}
	b := &Brief{Date: dayStr(today), DateLabel: today.Format("Mon 2 Jan"), Timezone: tz, FirstName: v.FirstName,
		Organization: org.Name, Currency: in.Currency, GeneratedAt: now, Sections: []Section{}}
	since := in.Since
	b.Since = &since
	b.Greeting = greeting(now.In(loc), v.FirstName)

	regs := sections()
	if opt.SyncJira {
		for _, r := range regs {
			if r.Key == "projects" && st.SectionOn(r.Key) && (r.Allowed == nil || r.Allowed(v)) {
				in.jiraNote = s.ensureJira(ctx, v.OrgID, now, loc, opt.JiraTimeout)
			}
		}
	} else {
		in.jiraNote = s.jiraStale(v.OrgID, now, loc)
	}
	for _, r := range regs {
		if !st.SectionOn(r.Key) {
			b.Hidden = append(b.Hidden, Hidden{Key: r.Key, Title: r.Title, Reason: "off"})
			continue
		}
		if r.Allowed != nil && !r.Allowed(v) {
			b.Hidden = append(b.Hidden, Hidden{Key: r.Key, Title: r.Title, Reason: "not_allowed", Note: r.Reason})
			continue
		}
		sec, err := s.run(ctx, r, in)
		if err != nil {
			log.Printf("brief: section %s for %s: %v", r.Key, v.UserID, err)
			b.Hidden = append(b.Hidden, Hidden{Key: r.Key, Title: r.Title, Reason: "error", Note: "This part could not be prepared just now."})
			continue
		}
		if sec.Empty() {
			b.Hidden = append(b.Hidden, Hidden{Key: r.Key, Title: r.Title, Reason: "empty"})
			continue
		}
		sec.Key, sec.Order = r.Key, r.Order
		if sec.Title == "" {
			sec.Title = r.Title
		}
		if sec.Icon == "" {
			sec.Icon = r.Icon
		}
		b.Sections = append(b.Sections, *sec)
	}
	b.Headline = Headline(b.Sections)
	return b
}

// run calls a provider, turning a panic into an error.
func (s *Service) run(ctx context.Context, r *registration, in *Input) (sec *Section, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return r.Provider(ctx, in)
}

func greeting(local time.Time, first string) string {
	g := "Good morning"
	switch h := local.Hour(); {
	case h >= 17:
		g = "Good evening"
	case h >= 12:
		g = "Good afternoon"
	}
	if first = strings.TrimSpace(first); first != "" {
		return g + " " + first
	}
	return g
}

// previous is the last scheduled brief sent before date.
func (s *Service) previous(user string, date time.Time) *Snapshot {
	var snap Snapshot
	s.DB.Where("user_id = ? AND kind = ? AND date < ? AND status IN ?", user, KindScheduled, dayStr(date),
		[]string{StatusSent, StatusPartial, StatusRetry}).Order("date DESC").Limit(1).Find(&snap)
	if snap.ID == "" {
		return nil
	}
	return &snap
}

// ---------------------------------------------------------------- jira

type jiraConn struct {
	LastSyncAt     *time.Time
	LastSyncStatus string
	ProjectKeys    string
}

func (s *Service) jiraConn(org string) (jiraConn, bool) {
	var rows []jiraConn
	if !hasTable(s.DB, "pm_jira_connections") {
		return jiraConn{}, false
	}
	s.DB.Raw(`SELECT last_sync_at, COALESCE(last_sync_status,'') AS last_sync_status, COALESCE(project_keys,'') AS project_keys
		FROM pm_jira_connections WHERE org_id = ? LIMIT 1`, org).Scan(&rows)
	if len(rows) == 0 || strings.TrimSpace(rows[0].ProjectKeys) == "" {
		return jiraConn{}, false
	}
	return rows[0], true
}

// jiraNote says how old the Jira figures are when Jira could not be reached:
// the last time the synced projects were refreshed.
func (s *Service) jiraNote(org string, loc *time.Location) string {
	var at []*time.Time
	s.DB.Raw(`SELECT MAX(last_synced_at) FROM pm_projects WHERE org_id = ? AND source = 'jira'`, org).Scan(&at)
	if len(at) == 0 || at[0] == nil {
		return "Jira could not be reached; no figures from Jira yet"
	}
	return "Jira could not be reached; figures from " + at[0].In(loc).Format("Mon 2 Jan 15:04")
}

// ensureJira syncs Jira when the organisation has a connection and the last
// sync is over an hour old. It never fails the brief: when Jira can't be
// reached the note says how old the figures are.
func (s *Service) ensureJira(ctx context.Context, org string, now time.Time, loc *time.Location, timeout time.Duration) string {
	c, ok := s.jiraConn(org)
	if !ok {
		return ""
	}
	if c.LastSyncAt != nil && now.Sub(*c.LastSyncAt) < time.Hour && c.LastSyncStatus != "error" {
		return ""
	}
	if s.JiraSync == nil {
		return ""
	}
	s.mu.Lock()
	failed, recent := s.jiraFail[org]
	s.mu.Unlock()
	if recent && time.Since(failed) < 10*time.Minute {
		return s.jiraNote(org, loc)
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := s.JiraSync(cctx, org); err != nil {
		log.Printf("brief: Jira sync for %s: %v", org, err)
		s.mu.Lock()
		s.jiraFail[org] = time.Now()
		s.mu.Unlock()
		return s.jiraNote(org, loc)
	}
	s.mu.Lock()
	delete(s.jiraFail, org)
	s.mu.Unlock()
	return ""
}

// jiraStale is the note for a brief built without syncing (page views):
// only when the last sync failed.
func (s *Service) jiraStale(org string, now time.Time, loc *time.Location) string {
	c, ok := s.jiraConn(org)
	if !ok || c.LastSyncStatus != "error" {
		return ""
	}
	return s.jiraNote(org, loc)
}

func hasTable(db *gorm.DB, name string) bool {
	var ok bool
	if err := db.Raw("SELECT to_regclass(?) IS NOT NULL", name).Row().Scan(&ok); err != nil {
		return false
	}
	return ok
}
