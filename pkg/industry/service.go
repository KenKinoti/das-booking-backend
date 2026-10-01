package industry

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Service owns the registry, per-organisation settings and roles.
type Service struct {
	DB *gorm.DB
	// PlanModules returns the organisation's plan entitlements (optional).
	PlanModules func(org string) (map[string]bool, error)
	Now         func() time.Time
	TTL         time.Duration

	mu     sync.Mutex
	types  []BusinessType
	typeAt time.Time
	orgs   map[string]orgEntry
}

type orgEntry struct {
	settings OrgSettings
	roles    []OrgRole
	assign   map[string]string // user → role id
	at       time.Time
}

// NewService builds a service.
func NewService(db *gorm.DB) *Service {
	return &Service{DB: db, Now: time.Now, TTL: 20 * time.Second, orgs: map[string]orgEntry{}}
}

// Migrate creates the tables, seeds the registry and maps existing
// organisations and users onto business types and role templates. It is
// idempotent: it only adds what is missing.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&BusinessType{}, &OrgSettings{}, &OrgRole{}, &UserRole{}, &Meta{}); err != nil {
		return err
	}
	if err := Seed(db); err != nil {
		return err
	}
	_, err := MigrateLegacy(db)
	return err
}

// Seed inserts default types that do not exist yet (never overwrites edits).
func Seed(db *gorm.DB) error {
	for _, t := range DefaultTypes() {
		t := t
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&t).Error; err != nil {
			return err
		}
	}
	m := Meta{Key: "installed_at", Value: time.Now().UTC().Format(time.RFC3339)}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&m).Error
}

// LegacyReport summarises MigrateLegacy.
type LegacyReport struct {
	Orgs, Roles, Users int
}

// MigrateLegacy gives every organisation a settings row (type from its
// business_type, unconfirmed → no module changes), its type's template roles
// and every user without a role assignment the template their legacy role
// maps to (admin → Owner/Admin, manager → the type's manager, others → the
// type's default staff role). Safe to run any number of times.
func MigrateLegacy(db *gorm.DB) (LegacyReport, error) {
	var rep LegacyReport
	svc := NewService(db)
	var orgs []struct {
		ID           string
		BusinessType string
	}
	if err := db.Table("organizations").Select("id, business_type").Where("deleted_at IS NULL").Scan(&orgs).Error; err != nil {
		return rep, err
	}
	for _, o := range orgs {
		created, err := svc.ensureSettings(o.ID, o.BusinessType)
		if err != nil {
			return rep, err
		}
		if created {
			rep.Orgs++
		}
		st, _ := svc.loadSettings(o.ID)
		n, err := svc.ensureRoles(o.ID, st.TypeKey)
		if err != nil {
			return rep, err
		}
		rep.Roles += n
		var users []struct {
			ID   string
			Role string
		}
		db.Table("users").Select("id, role").Where("organization_id = ? AND deleted_at IS NULL AND role <> ?", o.ID, "super_admin").Scan(&users)
		roles, _ := svc.loadRoles(o.ID)
		for _, u := range users {
			r := legacyRole(roles, u.Role)
			if r == nil {
				continue
			}
			res := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&UserRole{UserID: u.ID, OrganizationID: o.ID, RoleID: r.ID, AssignedBy: "migration", UpdatedAt: time.Now()})
			if res.Error != nil {
				return rep, res.Error
			}
			rep.Users += int(res.RowsAffected)
		}
	}
	if rep.Orgs+rep.Roles+rep.Users > 0 {
		log.Printf("industry: legacy mapping — %d organisation(s), %d role(s), %d user assignment(s)", rep.Orgs, rep.Roles, rep.Users)
	}
	return rep, nil
}

// ---------------------------------------------------------------- registry

// Types returns the (briefly cached) registry, active and inactive.
func (s *Service) Types() ([]BusinessType, error) {
	s.mu.Lock()
	if s.types != nil && s.Now().Sub(s.typeAt) < time.Minute {
		t := s.types
		s.mu.Unlock()
		return t, nil
	}
	s.mu.Unlock()
	var out []BusinessType
	if err := s.DB.Order("sort_order, key").Find(&out).Error; err != nil {
		return nil, err
	}
	if len(out) == 0 {
		out = DefaultTypes()
	}
	s.mu.Lock()
	s.types, s.typeAt = out, s.Now()
	s.mu.Unlock()
	return out, nil
}

// Type returns the registry entry for key (general when unknown).
func (s *Service) Type(key string) BusinessType {
	types, _ := s.Types()
	var general BusinessType
	for _, t := range types {
		if t.Key == key {
			return t
		}
		if t.Key == "general" {
			general = t
		}
	}
	if general.Key == "" {
		for _, t := range DefaultTypes() {
			if t.Key == "general" {
				return t
			}
		}
	}
	return general
}

func (s *Service) typeExists(key string) bool {
	types, _ := s.Types()
	for _, t := range types {
		if t.Key == key {
			return true
		}
	}
	return false
}

// Terms merges the defaults with a type's words.
func Terms(t BusinessType) map[string]string {
	out := DefaultTerms()
	for k, v := range t.Terms {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	return out
}

// Invalidate drops cached state ("" = everything).
func (s *Service) Invalidate(org string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if org == "" {
		s.types = nil
		s.orgs = map[string]orgEntry{}
		return
	}
	delete(s.orgs, org)
}

// ---------------------------------------------------------------- org state

func (s *Service) loadSettings(org string) (OrgSettings, error) {
	var st OrgSettings
	err := s.DB.Where("organization_id = ?", org).First(&st).Error
	return st, err
}

func (s *Service) loadRoles(org string) ([]OrgRole, error) {
	var roles []OrgRole
	err := s.DB.Where("organization_id = ?", org).Order("sort_order, name").Find(&roles).Error
	return roles, err
}

// ensureSettings creates the settings row of an organisation that has none
// (type from business_type, unconfirmed, legacy).
func (s *Service) ensureSettings(org, businessType string) (bool, error) {
	key := TypeKeyFor(businessType)
	if !s.typeExists(key) {
		key = "general"
	}
	st := OrgSettings{OrganizationID: org, TypeKey: key, SetupStatus: SetupLegacy, ExtraModules: []string{}, UpdatedBy: "migration"}
	res := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&st)
	return res.RowsAffected > 0, res.Error
}

// templateRole turns a template into an organisation role.
func templateRole(org, typeKey string, t RoleTemplate, order int) OrgRole {
	scope := t.Scope
	if scope == "" {
		scope = ScopeAll
	}
	p := Perms{}
	for k, v := range t.Perms {
		p[k] = append([]string{}, v...)
	}
	return OrgRole{ID: uuid.NewString(), OrganizationID: org, Key: t.Key, TypeKey: typeKey, Name: t.Name, Description: t.Description,
		Base: t.Base, Scope: scope, Perms: p, Legacy: t.Legacy, Home: t.Home, SortOrder: order}
}

// ensureRoles adds the type's template roles an organisation does not have.
func (s *Service) ensureRoles(org, typeKey string) (int, error) {
	t := s.Type(typeKey)
	n := 0
	for i, rt := range t.Roles {
		r := templateRole(org, t.Key, rt, i)
		res := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&r)
		if res.Error != nil {
			return n, res.Error
		}
		n += int(res.RowsAffected)
	}
	return n, nil
}

// org returns the cached organisation state, creating defaults when missing.
func (s *Service) org(org string) (orgEntry, error) {
	s.mu.Lock()
	if e, ok := s.orgs[org]; ok && s.Now().Sub(e.at) < s.TTL {
		s.mu.Unlock()
		return e, nil
	}
	s.mu.Unlock()
	st, err := s.loadSettings(org)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var bt string
		s.DB.Table("organizations").Select("business_type").Where("id = ?", org).Scan(&bt)
		if _, err := s.ensureSettings(org, bt); err != nil {
			return orgEntry{}, err
		}
		st, err = s.loadSettings(org)
	}
	if err != nil {
		return orgEntry{}, err
	}
	roles, err := s.loadRoles(org)
	if err != nil {
		return orgEntry{}, err
	}
	if len(roles) == 0 {
		if _, err := s.ensureRoles(org, st.TypeKey); err != nil {
			return orgEntry{}, err
		}
		roles, _ = s.loadRoles(org)
	}
	var ur []UserRole
	s.DB.Where("organization_id = ?", org).Find(&ur)
	e := orgEntry{settings: st, roles: roles, assign: map[string]string{}, at: s.Now()}
	for _, a := range ur {
		e.assign[a.UserID] = a.RoleID
	}
	s.mu.Lock()
	s.orgs[org] = e
	s.mu.Unlock()
	return e, nil
}

// Settings returns an organisation's business type settings.
func (s *Service) Settings(org string) (OrgSettings, error) {
	e, err := s.org(org)
	return e.settings, err
}

// Roles returns an organisation's roles.
func (s *Service) Roles(org string) ([]OrgRole, error) {
	e, err := s.org(org)
	return e.roles, err
}

// Assignments returns user → role id for an organisation.
func (s *Service) Assignments(org string) (map[string]string, error) {
	e, err := s.org(org)
	return e.assign, err
}

// legacyRole picks the role a legacy users.role maps to.
func legacyRole(roles []OrgRole, legacy string) *OrgRole {
	want := "staff"
	switch legacy {
	case "admin", "owner", "super_admin":
		want = "admin"
	case "manager":
		want = "manager"
	}
	var byBase *OrgRole
	for i := range roles {
		r := &roles[i]
		if r.Legacy == want && !r.Custom {
			return r
		}
		if byBase == nil && r.Base == want {
			byBase = r
		}
	}
	if byBase != nil {
		return byBase
	}
	if want == "admin" {
		for i := range roles {
			if roles[i].Key == "owner" {
				return &roles[i]
			}
		}
	}
	return nil
}

// consistent reports whether a stored role assignment still matches the
// user's legacy role string (a demoted admin must not keep Owner access).
func consistent(r *OrgRole, legacy string) bool {
	switch r.Base {
	case BaseAdmin:
		return legacy == "admin" || legacy == "owner"
	case BaseManager:
		return legacy == "manager"
	default:
		return legacy != "admin" && legacy != "owner" && legacy != "manager"
	}
}

// RoleFor resolves a user's role: their assignment when it still matches
// their legacy role, otherwise the template their legacy role maps to.
func (s *Service) RoleFor(org, user, legacy string) (*OrgRole, error) {
	e, err := s.org(org)
	if err != nil {
		return nil, err
	}
	if id, ok := e.assign[user]; ok {
		for i := range e.roles {
			if e.roles[i].ID == id && consistent(&e.roles[i], legacy) {
				r := e.roles[i]
				return &r, nil
			}
		}
	}
	if r := legacyRole(e.roles, legacy); r != nil {
		c := *r
		return &c, nil
	}
	return nil, nil
}

// AccessFor computes a user's effective access.
func (s *Service) AccessFor(org, user, legacy string) (Access, error) {
	a := Access{SuperAdmin: legacy == "super_admin", UserID: user}
	e, err := s.org(org)
	if err != nil {
		return a, err
	}
	t := s.Type(e.settings.TypeKey)
	a.TypeKey = t.Key
	a.HiddenPath = t.HiddenPaths
	a.TypeMods = map[string]bool{}
	all := false
	for _, m := range t.Modules {
		if m == AllModules {
			all = true
		}
		a.TypeMods[m] = true
	}
	for _, m := range e.settings.ExtraModules {
		a.TypeMods[m] = true
	}
	a.Restricted = e.settings.Confirmed && !all
	if !e.settings.Confirmed {
		a.HiddenPath = nil
	}
	if s.PlanModules != nil {
		if pm, err := s.PlanModules(org); err == nil {
			a.Plan = pm
		}
	}
	if !a.SuperAdmin {
		a.Role, _ = s.RoleFor(org, user, legacy)
	}
	return a, nil
}

// ---------------------------------------------------------------- changes

// OnOrgCreated records the business type a new organisation was created
// with. An explicit (non-general) type is confirmed straight away and its
// admin gets the setup wizard on first sign-in; without one the
// organisation behaves like a legacy "general" one (all plan modules, a
// banner inviting the admin to choose a type).
func (s *Service) OnOrgCreated(org, businessType, by string) error {
	key := TypeKeyFor(businessType)
	if !s.typeExists(key) {
		key = "general"
	}
	st := OrgSettings{OrganizationID: org, TypeKey: key, SetupStatus: SetupLegacy, ExtraModules: []string{}, UpdatedBy: by}
	if key != "general" {
		st.Confirmed, st.SetupStatus = true, SetupPending
	}
	if err := s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&st).Error; err != nil {
		return err
	}
	s.DB.Table("organizations").Where("id = ?", org).Update("business_type", key)
	if _, err := s.ensureRoles(org, key); err != nil {
		return err
	}
	// The first admin gets the Owner role.
	roles, _ := s.loadRoles(org)
	var admins []string
	s.DB.Table("users").Where("organization_id = ? AND role = ? AND deleted_at IS NULL", org, "admin").Pluck("id", &admins)
	if r := legacyRole(roles, "admin"); r != nil {
		for _, u := range admins {
			s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&UserRole{UserID: u, OrganizationID: org, RoleID: r.ID, AssignedBy: by, UpdatedAt: time.Now()})
		}
	}
	s.Invalidate(org)
	return nil
}

// SetType changes an organisation's business type: the template roles of
// the new type replace the old ones (custom roles are kept) and users are
// moved to the matching new role (same key, else by their base level).
func (s *Service) SetType(org, key, by string) (OrgSettings, error) {
	key = normKey(key)
	if !s.typeExists(key) {
		return OrgSettings{}, fmt.Errorf("unknown business type %q", key)
	}
	e, err := s.org(org)
	if err != nil {
		return OrgSettings{}, err
	}
	t := s.Type(key)
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		st := e.settings
		st.TypeKey, st.Confirmed, st.UpdatedBy = key, true, by
		if st.SetupStatus == SetupLegacy {
			st.SetupStatus = SetupDone
		}
		if st.ExtraModules == nil {
			st.ExtraModules = []string{}
		}
		if err := tx.Save(&st).Error; err != nil {
			return err
		}
		if err := tx.Table("organizations").Where("id = ?", org).Update("business_type", key).Error; err != nil {
			return err
		}
		// Template roles of the new type: update in place by key, add missing.
		keep := map[string]bool{}
		for i, rt := range t.Roles {
			keep[rt.Key] = true
			nr := templateRole(org, key, rt, i)
			var cur OrgRole
			if tx.Where("organization_id = ? AND key = ?", org, rt.Key).First(&cur).Error == nil {
				if cur.Custom {
					continue
				}
				if err := tx.Model(&OrgRole{}).Where("id = ?", cur.ID).Updates(map[string]interface{}{
					"type_key": key, "name": nr.Name, "description": nr.Description, "base": nr.Base, "scope": nr.Scope,
					"perms": mustJSON(nr.Perms), "legacy": nr.Legacy, "home": nr.Home, "sort_order": nr.SortOrder, "updated_at": time.Now(),
				}).Error; err != nil {
					return err
				}
			} else if err := tx.Create(&nr).Error; err != nil {
				return err
			}
		}
		var roles []OrgRole
		if err := tx.Where("organization_id = ?", org).Find(&roles).Error; err != nil {
			return err
		}
		newRoles := []OrgRole{}
		stale := map[string]OrgRole{}
		for _, r := range roles {
			if r.Custom || keep[r.Key] {
				newRoles = append(newRoles, r)
			} else {
				stale[r.ID] = r
			}
		}
		// Move users off roles the new type does not have.
		var assigned []UserRole
		tx.Where("organization_id = ?", org).Find(&assigned)
		for _, a := range assigned {
			old, isStale := stale[a.RoleID]
			if !isStale {
				continue
			}
			legacy := map[string]string{BaseAdmin: "admin", BaseManager: "manager"}[old.Base]
			if r := legacyRole(newRoles, legacy); r != nil {
				if err := tx.Model(&UserRole{}).Where("user_id = ?", a.UserID).Updates(map[string]interface{}{"role_id": r.ID, "assigned_by": by, "updated_at": time.Now()}).Error; err != nil {
					return err
				}
			}
		}
		for id := range stale {
			if err := tx.Where("id = ?", id).Delete(&OrgRole{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	s.Invalidate(org)
	if err != nil {
		return OrgSettings{}, err
	}
	return s.Settings(org)
}

// SetExtras stores the modules an admin enabled outside the business type.
func (s *Service) SetExtras(org string, mods []string, by string) (OrgSettings, error) {
	if _, err := s.org(org); err != nil {
		return OrgSettings{}, err
	}
	clean := []string{}
	seen := map[string]bool{}
	for _, m := range mods {
		m = normKey(m)
		if m == "" || m == "core" || seen[m] {
			continue
		}
		seen[m] = true
		clean = append(clean, m)
	}
	sort.Strings(clean)
	err := s.DB.Model(&OrgSettings{}).Where("organization_id = ?", org).Updates(map[string]interface{}{"extra_modules": serializeList(clean), "updated_by": by, "updated_at": time.Now()}).Error
	s.Invalidate(org)
	if err != nil {
		return OrgSettings{}, err
	}
	return s.Settings(org)
}

func serializeList(l []string) string { return mustJSON(l) }

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// SetSetup records wizard progress ("pending" with a step, or "done").
func (s *Service) SetSetup(org, status, step, by string) (OrgSettings, error) {
	if _, err := s.org(org); err != nil {
		return OrgSettings{}, err
	}
	if status != SetupPending && status != SetupDone {
		return OrgSettings{}, errors.New("status must be pending or done")
	}
	err := s.DB.Model(&OrgSettings{}).Where("organization_id = ?", org).Updates(map[string]interface{}{"setup_status": status, "setup_step": step, "updated_by": by, "updated_at": time.Now()}).Error
	s.Invalidate(org)
	if err != nil {
		return OrgSettings{}, err
	}
	return s.Settings(org)
}

// LegacyRoleForBase is the users.role string stored for a base level.
func LegacyRoleForBase(base, current string) string {
	switch base {
	case BaseAdmin:
		return "admin"
	case BaseManager:
		return "manager"
	}
	if current == "care_worker" || current == "support_coordinator" || current == "staff" {
		return current
	}
	return "staff"
}

// Assign gives a user one of the organisation's roles and keeps users.role
// in step with the role's base level. It returns the legacy role stored.
func (s *Service) Assign(org, user, roleRef, by string) (*OrgRole, string, error) {
	roles, err := s.Roles(org)
	if err != nil {
		return nil, "", err
	}
	var r *OrgRole
	for i := range roles {
		if roles[i].ID == roleRef || roles[i].Key == roleRef {
			r = &roles[i]
			break
		}
	}
	if r == nil {
		return nil, "", errors.New("choose one of your organisation's roles")
	}
	var cur struct {
		ID   string
		Role string
	}
	if err := s.DB.Table("users").Select("id, role").Where("id = ? AND organization_id = ? AND deleted_at IS NULL", user, org).Scan(&cur).Error; err != nil || cur.ID == "" {
		return nil, "", errors.New("user not found")
	}
	if cur.Role == "super_admin" {
		return nil, "", errors.New("super admin accounts cannot be given an organisation role")
	}
	legacy := LegacyRoleForBase(r.Base, cur.Role)
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Table("users").Where("id = ?", user).Update("role", legacy).Error; err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&UserRole{UserID: user, OrganizationID: org, RoleID: r.ID, AssignedBy: by, UpdatedAt: time.Now()}).Error
	})
	s.Invalidate(org)
	if err != nil {
		return nil, "", err
	}
	c := *r
	return &c, legacy, nil
}

// Unassign forgets a user's role (they fall back to their legacy mapping).
func (s *Service) Unassign(org, user string) {
	s.DB.Where("user_id = ? AND organization_id = ?", user, org).Delete(&UserRole{})
	s.Invalidate(org)
}
