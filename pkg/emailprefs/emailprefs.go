// Package emailprefs decides who receives an organisation's internal emails
// (not customer emails such as invoices, receipts and quotes): booking
// emails and the daily agenda to staff, bill reminders, the bar night report
// and the monthly strategy brief. Every sender asks this package before it
// emails a person (Allowed, UserEmails) and passes the addresses an admin
// typed into a list through FilterAddresses (a member's address there
// follows that member's switches and the mode too).
//
// Three things decide, in this order (see decide):
//
//  1. the category's audience — who the email is for at all (the person a
//     booking is about, the admins, the people who may approve bar stock);
//  2. per-person switches — an admin can turn every email or one category off
//     for anyone, or switch one on as an exception to the mode; each person
//     can turn their own emails off in My profile (but not back on what an
//     admin turned off);
//  3. the organisation's mode — everyone involved (the default, exactly the
//     behaviour before this package), owners and managers only, or owners
//     only. Nightclubs start in "owners only" (decided the first time the
//     policy is read, then stored).
//
// Levels come from the user's business-type role (pkg/industry, through the
// RoleOf hook: "Bar manager" is a manager, "Bartender" is staff) with a
// fallback on users.role. Super admins who are not members of the
// organisation never receive its emails through this package.
package emailprefs

import (
	"log"
	netmail "net/mail"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
)

// Category keys.
const (
	Bookings       = "bookings"
	Agenda         = "agenda"
	BillReminders  = "bill_reminders"
	BarNightReport = "bar_night_report"
	StrategyBrief  = "strategy_brief"
)

// Audiences: who a category's emails are for before the mode and the
// per-person switches.
const (
	Involved = "involved" // the person the email is about (any member)
	Admins   = "admins"   // users.role owner / admin / org_admin / super_admin
	// ApproversPrefix + module: users who may approve on that module.
	ApproversPrefix = "approvers:"
)

// Category is one kind of internal email.
type Category struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Audience    string `json:"audience"`
}

// Categories in display order. A new category is one line here; its sender
// then asks Allowed / UserEmails / FilterAddresses with the key.
var Categories = []Category{
	{Bookings, "Booking emails", "A booking is assigned to them, moved, reassigned or cancelled — with a calendar invite.", Involved},
	{Agenda, "Daily agenda", "Each morning, their bookings for the day (when the daily agenda is switched on).", Involved},
	{BillReminders, "Bill reminders", "Recurring and unpaid bills coming due or overdue, and the weekly bills digest.", Admins},
	{BarNightReport, "Bar night report", "After each night: sales, pour cost, wastage, the last count's variance and low stock.", ApproversPrefix + "inventory"},
	{StrategyBrief, "Monthly strategy brief", "What changed in the business this month and the actions worth taking.", Admins},
}

// CategoryByKey finds a category.
func CategoryByKey(key string) (Category, bool) {
	for _, c := range Categories {
		if c.Key == key {
			return c, true
		}
	}
	return Category{}, false
}

func category(key string) Category {
	if c, ok := CategoryByKey(key); ok {
		return c
	}
	// An unknown key still follows the mode and the person's switches.
	return Category{Key: key, Label: key, Audience: Involved}
}

// Modes.
const (
	ModeEveryone       = "everyone"
	ModeOwnersManagers = "owners_managers"
	ModeOwners         = "owners"
)

// Mode is one organisation-wide choice.
type Mode struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Modes in display order.
var Modes = []Mode{
	{ModeEveryone, "Everyone involved (default)", "Each email goes to the people it concerns — staff get emails about their own bookings."},
	{ModeOwnersManagers, "Owners and managers", "Owners, admins and managers get emails. Other staff get none."},
	{ModeOwners, "Only owners — staff get no emails", "Only owners and admins get emails. Switch someone on below to make an exception."},
}

// ValidMode reports whether m is a mode key.
func ValidMode(m string) bool {
	for _, x := range Modes {
		if x.Key == m {
			return true
		}
	}
	return false
}

// Levels.
const (
	LevelOwner   = "owner"
	LevelManager = "manager"
	LevelStaff   = "staff"
)

// AllKey is the Pref category that stands for every email ("all off").
const AllKey = "*"

// ---------------------------------------------------------------- hooks

// RoleOf resolves a user's business-type role (pkg/industry): its base level
// (admin | manager | staff | read_only) and name. Set by cmd/production;
// when nil or !ok, users.role decides.
var RoleOf func(org, userID, legacy string) (base, name string, ok bool)

// Can reports whether a user may perform act on module (pkg/industry access).
// When nil, users.role decides (owners, admins and managers may approve).
var Can func(org, userID, legacy, module, act string) bool

// Now is the clock (tests replace it).
var Now = time.Now

// ---------------------------------------------------------------- tables

// Policy is an organisation's mode (one row per organisation).
type Policy struct {
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	Mode           string `json:"mode" gorm:"type:varchar(20);not null;default:'everyone'"`
	// Source: "default" = decided from the business type at first read,
	// "admin" = chosen in Settings.
	Source    string    `json:"source" gorm:"type:varchar(10);not null;default:'default'"`
	UpdatedBy string    `json:"updated_by" gorm:"type:varchar(255)"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Policy) TableName() string { return "email_recipient_policies" }

// Pref is one person's switch for one category (or AllKey).
type Pref struct {
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	UserID         string `json:"user_id" gorm:"type:varchar(255);primaryKey;index"`
	Category       string `json:"category" gorm:"type:varchar(40);primaryKey"`
	// Admin is an admin's choice: nil = follow the mode, false = off, true =
	// on as an exception to the mode (for AllKey only false is used).
	Admin *bool `json:"admin"`
	// SelfOff: the person turned it off in My profile.
	SelfOff   bool      `json:"self_off" gorm:"not null;default:false"`
	UpdatedBy string    `json:"updated_by" gorm:"type:varchar(255)"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName is fixed.
func (Pref) TableName() string { return "email_recipient_prefs" }

// Meta records one-off migrations.
type Meta struct {
	Key       string `gorm:"type:varchar(80);primaryKey"`
	Value     string `gorm:"type:text"`
	UpdatedAt time.Time
}

// TableName is fixed.
func (Meta) TableName() string { return "email_recipient_meta" }

// Models lists the tables.
func Models() []interface{} { return []interface{}{&Policy{}, &Pref{}, &Meta{}} }

func fresh(db *gorm.DB) *gorm.DB { return db.Session(&gorm.Session{NewDB: true}) }

// ---------------------------------------------------------------- policy

// BusinessType is the organisation's business-type key ("general" when unknown).
func BusinessType(db *gorm.DB, org string) string {
	var key []string
	fresh(db).Raw(`SELECT type_key FROM industry_org_settings WHERE organization_id = ?`, org).Scan(&key)
	if len(key) > 0 && strings.TrimSpace(key[0]) != "" {
		return strings.TrimSpace(key[0])
	}
	var bt []string
	fresh(db).Raw(`SELECT COALESCE(business_type, '') FROM organizations WHERE id = ?`, org).Scan(&bt)
	if len(bt) > 0 {
		return industry.TypeKeyFor(bt[0])
	}
	return "general"
}

// DefaultMode is the mode an organisation starts with: owners only for
// nightclubs / bars / lounges (staff there are many and change often),
// everyone involved otherwise.
func DefaultMode(db *gorm.DB, org string) string {
	if BusinessType(db, org) == "nightclub" {
		return ModeOwners
	}
	return ModeEveryone
}

// PolicyFor returns an organisation's policy. The first read decides the
// default from the business type and stores it, so it no longer changes by
// itself (a later change of business type keeps the stored mode).
func PolicyFor(db *gorm.DB, org string) Policy {
	var rows []Policy
	if err := fresh(db).Where("organization_id = ?", org).Limit(1).Find(&rows).Error; err == nil && len(rows) == 1 {
		if !ValidMode(rows[0].Mode) {
			rows[0].Mode = ModeEveryone
		}
		return rows[0]
	}
	now := Now()
	p := Policy{OrganizationID: org, Mode: DefaultMode(db, org), Source: "default", UpdatedBy: "default", CreatedAt: now, UpdatedAt: now}
	if org == "" {
		p.Mode = ModeEveryone
		return p
	}
	if err := fresh(db).Clauses(clause.OnConflict{DoNothing: true}).Create(&p).Error; err == nil {
		rows = nil
		fresh(db).Where("organization_id = ?", org).Limit(1).Find(&rows)
		if len(rows) == 1 {
			return rows[0]
		}
	}
	return p
}

// SetMode stores an admin's choice.
func SetMode(db *gorm.DB, org, mode, by string) (Policy, error) {
	now := Now()
	p := PolicyFor(db, org)
	p.OrganizationID, p.Mode, p.Source, p.UpdatedBy, p.UpdatedAt = org, mode, "admin", by, now
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	err := fresh(db).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "organization_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"mode", "source", "updated_by", "updated_at"})}).Create(&p).Error
	return p, err
}

// ---------------------------------------------------------------- people

// Person is a member of an organisation as far as email is concerned.
type Person struct {
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Role      string `json:"role"`       // users.role
	RoleLabel string `json:"role_label"` // the business-type role ("Bar manager")
	Level     string `json:"level"`      // owner | manager | staff
	Active    bool   `json:"active"`
}

type userRow struct {
	ID, Email, FirstName, LastName, Role, OrganizationID string
	IsActive                                             *bool
}

// userCols, from the full users table down to the columns every schema has
// (some packages' test schemas carry fewer columns).
var userCols = []string{
	"id, COALESCE(email,'') AS email, COALESCE(first_name,'') AS first_name, COALESCE(last_name,'') AS last_name, COALESCE(role,'') AS role, COALESCE(organization_id,'') AS organization_id, is_active",
	"id, COALESCE(email,'') AS email, COALESCE(role,'') AS role, COALESCE(organization_id,'') AS organization_id, is_active",
	"id, COALESCE(email,'') AS email, COALESCE(role,'') AS role, COALESCE(organization_id,'') AS organization_id",
}

// scanUsers runs a users query with the richest column set the table has.
func scanUsers(db *gorm.DB, where func(*gorm.DB) *gorm.DB) []userRow {
	for _, cols := range userCols {
		var rows []userRow
		if err := where(fresh(db).Table("users").Select(cols)).Scan(&rows).Error; err == nil {
			return rows
		}
	}
	return nil
}

func usersByID(db *gorm.DB, ids []string) map[string]userRow {
	out := map[string]userRow{}
	for i := 0; i < len(ids); i += 500 {
		j := i + 500
		if j > len(ids) {
			j = len(ids)
		}
		part := ids[i:j]
		rows := scanUsers(db, func(q *gorm.DB) *gorm.DB { return q.Where("id IN ? AND deleted_at IS NULL", part) })
		for _, r := range rows {
			out[r.ID] = r
		}
	}
	return out
}

// members are the organisation's users (active only unless all).
func members(db *gorm.DB, org string, all bool) []userRow {
	rows := scanUsers(db, func(q *gorm.DB) *gorm.DB {
		q = q.Where("organization_id = ? AND deleted_at IS NULL", org)
		if !all {
			q = q.Where("is_active IS NULL OR is_active")
		}
		return q.Order("created_at")
	})
	if rows == nil {
		rows = []userRow{}
	}
	return rows
}

var legacyLabels = map[string]string{"super_admin": "Super admin", "owner": "Owner", "admin": "Admin", "org_admin": "Admin", "manager": "Manager",
	"staff": "Staff", "care_worker": "Staff", "support_coordinator": "Coordinator", "read_only": "Read only"}

func ownerRole(r string) bool {
	switch r {
	case "owner", "admin", "org_admin", "super_admin":
		return true
	}
	return false
}

// LevelOf is a user's level and role name: the business-type role's base
// (through RoleOf) with a fallback on users.role. Owners and admins by
// users.role stay owners whatever their template says.
func LevelOf(org, userID, legacy string) (level, label string) {
	legacy = strings.ToLower(strings.TrimSpace(legacy))
	switch {
	case ownerRole(legacy):
		level = LevelOwner
	case legacy == "manager":
		level = LevelManager
	default:
		level = LevelStaff
	}
	label = legacyLabels[legacy]
	if label == "" {
		label = "Staff"
		if w := strings.TrimSpace(strings.ReplaceAll(legacy, "_", " ")); w != "" {
			label = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	if RoleOf != nil && legacy != "super_admin" {
		if base, name, ok := RoleOf(org, userID, legacy); ok {
			if strings.TrimSpace(name) != "" {
				label = name
			}
			if level != LevelOwner {
				switch base {
				case industry.BaseAdmin:
					level = LevelOwner
				case industry.BaseManager:
					level = LevelManager
				case industry.BaseStaff, industry.BaseReadOnly:
					level = LevelStaff
				}
			}
		}
	}
	return level, label
}

func toPerson(org string, u userRow) Person {
	lvl, label := LevelOf(org, u.ID, u.Role)
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		name = u.Email
	}
	return Person{UserID: u.ID, Name: name, Email: strings.ToLower(strings.TrimSpace(u.Email)), Role: u.Role, RoleLabel: label, Level: lvl,
		Active: u.IsActive == nil || *u.IsActive}
}

// eligible: the category's audience includes the person.
func eligible(org string, p Person, c Category) bool {
	switch {
	case c.Audience == Admins:
		return ownerRole(strings.ToLower(p.Role))
	case strings.HasPrefix(c.Audience, ApproversPrefix):
		mod := strings.TrimPrefix(c.Audience, ApproversPrefix)
		if Can != nil {
			return Can(org, p.UserID, p.Role, mod, industry.ActApprove)
		}
		r := strings.ToLower(p.Role)
		return ownerRole(r) || r == "manager"
	}
	return true
}

// ModeAllows reports whether a level gets emails under a mode.
func ModeAllows(mode, level string) bool {
	switch mode {
	case ModeOwners:
		return level == LevelOwner
	case ModeOwnersManagers:
		return level == LevelOwner || level == LevelManager
	}
	return true
}

// ---------------------------------------------------------------- deciding

// Reasons a person does or doesn't get a category.
const (
	ReasonDefault     = "default"      // on: the mode includes them
	ReasonAdminOn     = "admin_on"     // on: an admin switched it on (exception to the mode)
	ReasonMode        = "mode"         // off: the mode leaves their level out
	ReasonAdminOff    = "admin_off"    // off: an admin turned it off
	ReasonAllOff      = "all_off"      // off: an admin turned all their emails off
	ReasonSelfOff     = "self_off"     // off: they turned it off themselves
	ReasonSelfAllOff  = "self_all_off" // off: they turned all their emails off
	ReasonNotEligible = "not_eligible" // this email is not for their role
	ReasonNotMember   = "not_member"   // not a member of the organisation
)

// ReasonText explains a reason in a log line or the UI.
func ReasonText(reason, mode string) string {
	switch reason {
	case ReasonDefault:
		return "on (default)"
	case ReasonAdminOn:
		return "switched on by an administrator"
	case ReasonMode:
		if mode == ModeOwnersManagers {
			return "the organisation only emails owners and managers"
		}
		return "the organisation only emails owners"
	case ReasonAdminOff:
		return "turned off by an administrator"
	case ReasonAllOff:
		return "all emails turned off by an administrator"
	case ReasonSelfOff, ReasonSelfAllOff:
		return "turned off in the staff member's profile"
	case ReasonNotEligible:
		return "this email is not sent to their role"
	case ReasonNotMember:
		return "not a member of the organisation"
	}
	return reason
}

// Decision is the outcome for one person and category.
type Decision struct {
	On     bool   `json:"on"`
	Reason string `json:"reason"`
}

// prefsOf loads the switches of some users: user → category → pref.
func prefsOf(db *gorm.DB, org string, ids []string) map[string]map[string]Pref {
	out := map[string]map[string]Pref{}
	if org == "" || len(ids) == 0 {
		return out
	}
	for i := 0; i < len(ids); i += 500 {
		j := i + 500
		if j > len(ids) {
			j = len(ids)
		}
		var rows []Pref
		fresh(db).Where("organization_id = ? AND user_id IN ?", org, ids[i:j]).Find(&rows)
		for _, r := range rows {
			if out[r.UserID] == nil {
				out[r.UserID] = map[string]Pref{}
			}
			out[r.UserID][r.Category] = r
		}
	}
	return out
}

func decide(org, mode string, p Person, c Category, prefs map[string]Pref) Decision {
	if !eligible(org, p, c) {
		return Decision{false, ReasonNotEligible}
	}
	all, one := prefs[AllKey], prefs[c.Key]
	switch {
	case all.Admin != nil && !*all.Admin:
		return Decision{false, ReasonAllOff}
	case one.Admin != nil && !*one.Admin:
		return Decision{false, ReasonAdminOff}
	case all.SelfOff:
		return Decision{false, ReasonSelfAllOff}
	case one.SelfOff:
		return Decision{false, ReasonSelfOff}
	case one.Admin != nil && *one.Admin:
		return Decision{true, ReasonAdminOn}
	case !ModeAllows(mode, p.Level):
		return Decision{false, ReasonMode}
	}
	return Decision{true, ReasonDefault}
}

// Check decides whether userID gets an email of category from org, with the
// reason as text for logs.
func Check(db *gorm.DB, org, userID, cat string) (bool, string) {
	d, mode := checkOne(db, org, userID, cat)
	return d.On, ReasonText(d.Reason, mode)
}

func checkOne(db *gorm.DB, org, userID, cat string) (Decision, string) {
	u, ok := usersByID(db, []string{userID})[userID]
	if !ok || org == "" || u.OrganizationID != org {
		return Decision{false, ReasonNotMember}, ""
	}
	mode := PolicyFor(db, org).Mode
	return decide(org, mode, toPerson(org, u), category(cat), prefsOf(db, org, []string{userID})[userID]), mode
}

// Allowed reports whether userID gets emails of category from org.
func Allowed(db *gorm.DB, org, userID, cat string) bool {
	d, _ := checkOne(db, org, userID, cat)
	return d.On
}

// FilterUsers keeps the users (ids, in order) who get emails of category.
func FilterUsers(db *gorm.DB, org, cat string, ids []string) []string {
	out := []string{}
	for _, u := range allowedUsers(db, org, cat, ids) {
		out = append(out, u.ID)
	}
	return out
}

// UserEmails is the email addresses (lower case, in order, without blanks)
// of the users who get emails of category.
func UserEmails(db *gorm.DB, org, cat string, ids []string) []string {
	out := []string{}
	for _, u := range allowedUsers(db, org, cat, ids) {
		if e := strings.ToLower(strings.TrimSpace(u.Email)); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func allowedUsers(db *gorm.DB, org, cat string, ids []string) []userRow {
	out := []userRow{}
	if org == "" || len(ids) == 0 {
		return out
	}
	users := usersByID(db, ids)
	mode := PolicyFor(db, org).Mode
	prefs := prefsOf(db, org, ids)
	c := category(cat)
	seen := map[string]bool{}
	for _, id := range ids {
		u, ok := users[id]
		if !ok || seen[id] || u.OrganizationID != org {
			continue
		}
		seen[id] = true
		if decide(org, mode, toPerson(org, u), c, prefs[id]).On {
			out = append(out, u)
		}
	}
	return out
}

func bareAddress(a string) string {
	a = strings.TrimSpace(a)
	if p, err := netmail.ParseAddress(a); err == nil {
		a = p.Address
	}
	return strings.ToLower(strings.TrimSpace(a))
}

// FilterAddresses passes the addresses an admin typed into a list
// (notification recipients, reminder recipients, a bill's extra
// recipients). An address that belongs to no member of the organisation
// stays. An address that belongs to a member (users.email, any case)
// follows that member's switches for the category and the organisation's
// mode — everything but the category's audience, since an admin chose to
// add it: it is dropped when all their emails or this one are off (by an
// admin or by themselves), or when the mode leaves their level out unless
// an admin switched them on. So "Emails off" and "Turned off by your
// administrator" hold for the lists too.
func FilterAddresses(db *gorm.DB, org, cat string, addrs []string) []string {
	if org == "" || len(addrs) == 0 {
		return addrs
	}
	listed := map[string]bool{}
	for _, a := range addrs {
		if b := bareAddress(a); b != "" {
			listed[b] = true
		}
	}
	var mine []userRow
	for _, m := range members(db, org, true) {
		if listed[strings.ToLower(strings.TrimSpace(m.Email))] {
			mine = append(mine, m)
		}
	}
	if len(mine) == 0 {
		return addrs
	}
	ids := make([]string, 0, len(mine))
	for _, m := range mine {
		ids = append(ids, m.ID)
	}
	mode := PolicyFor(db, org).Mode
	prefs := prefsOf(db, org, ids)
	c := category(cat)
	c.Audience = Involved // the admin chose the address: who the email is for doesn't apply
	keep, drop := map[string]bool{}, map[string]bool{}
	for _, m := range mine {
		e := strings.ToLower(strings.TrimSpace(m.Email))
		if decide(org, mode, toPerson(org, m), c, prefs[m.ID]).On {
			keep[e] = true
		} else {
			drop[e] = true
		}
	}
	out := []string{}
	for _, a := range addrs {
		if b := bareAddress(a); drop[b] && !keep[b] {
			continue
		}
		out = append(out, a)
	}
	return out
}

// ---------------------------------------------------------------- views

// PersonView is one row of Settings → Who gets emails.
type PersonView struct {
	Person
	// Effective: whether they get each category now.
	Effective map[string]bool `json:"effective"`
	// Overrides are the admin's switches (null = follow the mode).
	Overrides map[string]*bool `json:"overrides"`
	// SelfOff: categories they turned off themselves.
	SelfOff    map[string]bool `json:"self_off"`
	AllOff     bool            `json:"all_off"`      // an admin turned all their emails off
	SelfAllOff bool            `json:"self_all_off"` // they turned all their emails off
	// Applies: the category's audience includes them.
	Applies map[string]bool `json:"applies"`
	// ByMode: what the mode alone gives them (for the switches).
	ByMode  map[string]bool   `json:"by_mode"`
	Reasons map[string]string `json:"reason_per_category"`
	// EmailsOff: no category reaches them (all off, or the mode leaves them out).
	EmailsOff bool `json:"emails_off"`
}

var levelRank = map[string]int{LevelOwner: 0, LevelManager: 1, LevelStaff: 2}

func view(org, mode string, p Person, prefs map[string]Pref) PersonView {
	v := PersonView{Person: p, Effective: map[string]bool{}, Overrides: map[string]*bool{}, SelfOff: map[string]bool{}, Applies: map[string]bool{},
		ByMode: map[string]bool{}, Reasons: map[string]string{}}
	if a := prefs[AllKey].Admin; a != nil && !*a {
		v.AllOff = true
	}
	v.SelfAllOff = prefs[AllKey].SelfOff
	some := false
	for _, c := range Categories {
		d := decide(org, mode, p, c, prefs)
		v.Effective[c.Key], v.Reasons[c.Key] = d.On, d.Reason
		v.Applies[c.Key] = d.Reason != ReasonNotEligible
		v.ByMode[c.Key] = v.Applies[c.Key] && ModeAllows(mode, p.Level)
		v.Overrides[c.Key] = prefs[c.Key].Admin
		v.SelfOff[c.Key] = prefs[c.Key].SelfOff
		some = some || d.On
	}
	v.EmailsOff = !some
	return v
}

// People lists the organisation's active members with their switches,
// owners first.
func People(db *gorm.DB, org string) ([]PersonView, Policy) {
	pol := PolicyFor(db, org)
	ms := members(db, org, false)
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	prefs := prefsOf(db, org, ids)
	out := make([]PersonView, 0, len(ms))
	for _, m := range ms {
		out = append(out, view(org, pol.Mode, toPerson(org, m), prefs[m.ID]))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if levelRank[out[i].Level] != levelRank[out[j].Level] {
			return levelRank[out[i].Level] < levelRank[out[j].Level]
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, pol
}

// PersonOf is one member's view (ok false when not a member).
func PersonOf(db *gorm.DB, org, userID string) (PersonView, bool) {
	u, ok := usersByID(db, []string{userID})[userID]
	if !ok || org == "" || u.OrganizationID != org {
		return PersonView{}, false
	}
	pol := PolicyFor(db, org)
	return view(org, pol.Mode, toPerson(org, u), prefsOf(db, org, []string{userID})[userID]), true
}

// ---------------------------------------------------------------- writing

func getPref(db *gorm.DB, org, user, cat string) Pref {
	var rows []Pref
	fresh(db).Where("organization_id = ? AND user_id = ? AND category = ?", org, user, cat).Limit(1).Find(&rows)
	if len(rows) == 1 {
		return rows[0]
	}
	return Pref{OrganizationID: org, UserID: user, Category: cat}
}

func putPref(db *gorm.DB, p Pref) error {
	p.UpdatedAt = Now()
	return fresh(db).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "organization_id"}, {Name: "user_id"}, {Name: "category"}},
		DoUpdates: clause.AssignmentColumns([]string{"admin", "self_off", "updated_by", "updated_at"})}).Create(&p).Error
}

// SetAdmin stores an admin's switches for one person: allOff (nil = leave)
// and per-category overrides (a nil value = back to the mode). Unknown
// categories are ignored by the caller's validation.
func SetAdmin(db *gorm.DB, org, user string, allOff *bool, overrides map[string]*bool, by string) error {
	if allOff != nil {
		p := getPref(db, org, user, AllKey)
		if *allOff {
			f := false
			p.Admin = &f
		} else {
			p.Admin = nil
		}
		p.UpdatedBy = by
		if err := putPref(db, p); err != nil {
			return err
		}
	}
	for cat, v := range overrides {
		p := getPref(db, org, user, cat)
		if v == nil {
			p.Admin = nil
		} else {
			b := *v
			p.Admin = &b
		}
		p.UpdatedBy = by
		if err := putPref(db, p); err != nil {
			return err
		}
	}
	return nil
}

// SetSelf stores a person's own choices: allOff (nil = leave) and
// per-category on/off. Turning something on only clears their own "off";
// it never overrides an administrator.
func SetSelf(db *gorm.DB, org, user string, allOff *bool, on map[string]bool) error {
	if allOff != nil {
		p := getPref(db, org, user, AllKey)
		p.SelfOff, p.UpdatedBy = *allOff, user
		if err := putPref(db, p); err != nil {
			return err
		}
	}
	for cat, v := range on {
		p := getPref(db, org, user, cat)
		p.SelfOff, p.UpdatedBy = !v, user
		if err := putPref(db, p); err != nil {
			return err
		}
	}
	return nil
}

// SetSelfCategories turns some categories on or off as the person's own
// choice. Turning them on while all their emails are paused un-pauses only
// these: the other categories stay off.
func SetSelfCategories(db *gorm.DB, org, user string, cats []string, on bool) error {
	prefs := prefsOf(db, org, []string{user})[user]
	if on && prefs[AllKey].SelfOff {
		f := false
		rest := map[string]bool{}
		for _, c := range Categories {
			rest[c.Key] = false
		}
		for _, c := range cats {
			delete(rest, c)
		}
		if err := SetSelf(db, org, user, &f, rest); err != nil {
			return err
		}
	}
	m := map[string]bool{}
	for _, c := range cats {
		m[c] = on
	}
	return SetSelf(db, org, user, nil, m)
}

// SelfOn reports a person's own choice for a category (true unless they
// turned it, or all their emails, off).
func SelfOn(db *gorm.DB, org, user, cat string) bool {
	prefs := prefsOf(db, org, []string{user})[user]
	return !prefs[AllKey].SelfOff && !prefs[cat].SelfOff
}

// ---------------------------------------------------------------- migration

// Migrate creates the tables and moves the old "Email me about my
// bookings" choices (pkg/bookingmail booking_email_user_prefs) into the new
// store once.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(Models()...); err != nil {
		return err
	}
	_, err := MigrateLegacyBookingPrefs(db)
	return err
}

const legacyKey = "legacy_booking_prefs"

// MigrateLegacyBookingPrefs copies every booking_email_user_prefs row with
// booking_emails = false into the new store as the person's own "off" for
// booking emails and the daily agenda (the old switch covered both). It runs
// once (recorded in email_recipient_meta) and never overwrites a choice
// already made in the new store, so running it again changes nothing. It
// returns how many people were moved.
func MigrateLegacyBookingPrefs(db *gorm.DB) (int, error) {
	var done []Meta
	fresh(db).Where("key = ?", legacyKey).Limit(1).Find(&done)
	if len(done) == 1 {
		return 0, nil
	}
	n := 0
	if fresh(db).Migrator().HasTable("booking_email_user_prefs") {
		var rows []struct{ UserID, OrganizationID string }
		if err := fresh(db).Raw(`SELECT p.user_id, COALESCE(NULLIF(u.organization_id, ''), p.organization_id, '') AS organization_id
			FROM booking_email_user_prefs p LEFT JOIN users u ON u.id = p.user_id WHERE NOT p.booking_emails`).Scan(&rows).Error; err != nil {
			return 0, err
		}
		now := Now()
		for _, r := range rows {
			if r.OrganizationID == "" || r.UserID == "" {
				continue
			}
			moved := false
			for _, cat := range []string{Bookings, Agenda} {
				res := fresh(db).Clauses(clause.OnConflict{DoNothing: true}).Create(&Pref{OrganizationID: r.OrganizationID, UserID: r.UserID, Category: cat,
					SelfOff: true, UpdatedBy: "migration", UpdatedAt: now})
				if res.Error != nil {
					return n, res.Error
				}
				moved = moved || res.RowsAffected > 0
			}
			if moved {
				n++
			}
		}
	}
	if err := fresh(db).Clauses(clause.OnConflict{DoNothing: true}).Create(&Meta{Key: legacyKey, Value: "done", UpdatedAt: Now()}).Error; err != nil {
		return n, err
	}
	if n > 0 {
		log.Printf("emailprefs: moved %d \"Email me about my bookings\" choice(s) into Who gets emails", n)
	}
	return n, nil
}
