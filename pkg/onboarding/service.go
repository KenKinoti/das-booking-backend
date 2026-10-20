package onboarding

import (
	"errors"
	"log"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orghooks"
)

// ErrUnknownStep is returned when a step key is not one of the person's steps.
var ErrUnknownStep = errors.New("unknown step")

// Service computes and stores "Getting started" progress.
type Service struct {
	DB       *gorm.DB
	Industry *industry.Service
	Now      func() time.Time
}

// NewService builds a service.
func NewService(db *gorm.DB, ind *industry.Service) *Service {
	return &Service{DB: db, Industry: ind, Now: time.Now}
}

// Who is the caller: organisation, user, legacy users.role string and
// whether a super admin is viewing the organisation (token claim
// impersonating=true).
type Who struct {
	Org, User, Role string
	Preview         bool
}

// Migrate creates onboarding_progress. The first time it is created, people
// who have already signed in are marked as welcomed (no welcome sheet for
// existing users; the "Getting started" pill still offers their steps).
// Organisations created later, people who never signed in and every
// organisation reset for handover start fresh.
func Migrate(db *gorm.DB) error {
	fresh := !db.Migrator().HasTable(&Progress{})
	if err := db.AutoMigrate(&Progress{}); err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	return db.Exec(`INSERT INTO onboarding_progress (organization_id, user_id, welcomed_at, done_steps, updated_at)
		SELECT organization_id, id, now(), '{}', now() FROM users
		WHERE deleted_at IS NULL AND role <> 'super_admin' AND last_login_at IS NOT NULL
		  AND organization_id IS NOT NULL AND organization_id <> ''
		ON CONFLICT DO NOTHING`).Error
}

// ResetOrg is the orghooks reset hook: everyone in the organisation starts
// "Getting started" afresh after a handover reset. Only org's rows go.
func ResetOrg(tx *gorm.DB, org string, _ orghooks.ResetOptions) error {
	if org == "" || !tx.Migrator().HasTable(&Progress{}) {
		return nil
	}
	return tx.Where("organization_id = ?", org).Delete(&Progress{}).Error
}

// RegisterResetHook registers ResetOrg with pkg/orghooks.
func RegisterResetHook() { orghooks.RegisterReset("onboarding", ResetOrg) }

// person resolves whose steps are shown: the caller, or for a super admin
// viewing an organisation its owner (the first admin), read-only.
func (s *Service) person(w Who) (user, legacy string) {
	if !w.Preview {
		return w.User, w.Role
	}
	var owner struct{ ID string }
	s.DB.Table("users").Select("id").
		Where("organization_id = ? AND role IN ('admin', 'owner') AND deleted_at IS NULL", w.Org).
		Order("created_at").Limit(1).Scan(&owner)
	return owner.ID, "admin"
}

func (s *Service) load(org, user string) (Progress, bool) {
	var p Progress
	if user == "" {
		return p, false
	}
	err := s.DB.Where("organization_id = ? AND user_id = ?", org, user).Limit(1).Find(&p).Error
	if err != nil || p.UserID == "" {
		return Progress{OrganizationID: org, UserID: user, DoneSteps: map[string]string{}}, false
	}
	if p.DoneSteps == nil {
		p.DoneSteps = map[string]string{}
	}
	return p, true
}

func (s *Service) save(p *Progress) error {
	p.UpdatedAt = s.Now()
	if p.DoneSteps == nil {
		p.DoneSteps = map[string]string{}
	}
	return s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(p).Error
}

// roleOf returns the role key, name and base level shown for access a.
func roleOf(a industry.Access, legacy string) (key, name, base string) {
	if a.Role != nil {
		base = a.Role.Base
		if base == "" {
			base = industry.BaseStaff
		}
		return a.Role.Key, a.Role.Name, base
	}
	switch legacy {
	case "admin", "owner":
		return "owner", "Owner", industry.BaseAdmin
	case "manager":
		return "manager", "Manager", industry.BaseManager
	}
	return "staff", "Staff member", industry.BaseStaff
}

// View computes the caller's steps and progress.
func (s *Service) View(w Who) (View, error) {
	v := View{Steps: []StepView{}, Preview: w.Preview}
	if w.Role == "super_admin" && !w.Preview {
		v.Hidden = true
		return v, nil
	}
	if w.Org == "" {
		return v, errors.New("organisation required")
	}
	user, legacy := s.person(w)
	a, err := s.Industry.AccessFor(w.Org, user, legacy)
	if err != nil {
		return v, err
	}
	a.SuperAdmin = false
	st, err := s.Industry.Settings(w.Org)
	if err != nil {
		return v, err
	}
	t := s.Industry.Type(st.TypeKey)
	terms := industry.DefaultTerms()
	if st.Confirmed {
		terms = industry.Terms(t)
	}
	v.TypeKey, v.TypeName = t.Key, t.Name
	var base string
	v.RoleKey, v.RoleName, base = roleOf(a, legacy)
	v.RoleBase = base

	var names struct{ FirstName, OrgName string }
	if user != "" {
		s.DB.Table("users").Select("first_name").Where("id = ?", user).Scan(&names.FirstName)
	}
	s.DB.Table("organizations").Select("name").Where("id = ?", w.Org).Scan(&names.OrgName)
	v.FirstName, v.OrgName = names.FirstName, names.OrgName

	p, _ := s.load(w.Org, user)
	v.Welcomed, v.Dismissed = p.WelcomedAt != nil, p.DismissedAt != nil
	seen := map[string]bool{}
	for _, step := range industry.StepsFor(t, v.RoleKey, base) {
		if step.Key == "" || seen[step.Key] || !Reachable(a, t, st.Confirmed, base, step.To) {
			continue
		}
		seen[step.Key] = true
		auto := step.DoneWhen != "" && Known(step.DoneWhen)
		done := p.DoneSteps[step.Key] != ""
		if !done && auto {
			done = Detect(s.DB, step.DoneWhen, w.Org, user)
		}
		v.Steps = append(v.Steps, StepView{
			Key: step.Key, Title: FillTerms(step.Title, terms), Description: FillTerms(step.Description, terms),
			To: step.To, Done: done, Auto: auto,
		})
		if done {
			v.DoneCount++
		}
	}
	v.Total = len(v.Steps)
	if v.Total > 0 {
		v.Percent = v.DoneCount * 100 / v.Total
		v.AllDone = v.DoneCount == v.Total
	}
	return v, nil
}

// change loads the caller's row, applies fn and saves it (not in preview).
func (s *Service) change(w Who, fn func(p *Progress) error) (View, error) {
	if w.Preview || w.Role == "super_admin" {
		return s.View(w) // read-only: nothing is saved
	}
	p, _ := s.load(w.Org, w.User)
	if err := fn(&p); err != nil {
		return View{}, err
	}
	if err := s.save(&p); err != nil {
		log.Printf("onboarding: save %s/%s: %v", w.Org, w.User, err)
		return View{}, err
	}
	return s.View(w)
}

// Welcomed records that the person saw the welcome sheet.
func (s *Service) Welcomed(w Who) (View, error) {
	return s.change(w, func(p *Progress) error {
		if p.WelcomedAt == nil {
			now := s.Now()
			p.WelcomedAt = &now
		}
		return nil
	})
}

// SetStep ticks (or unticks) one of the person's steps.
func (s *Service) SetStep(w Who, key string, done bool) (View, error) {
	cur, err := s.View(w)
	if err != nil {
		return cur, err
	}
	found := false
	for _, st := range cur.Steps {
		found = found || st.Key == key
	}
	if !found {
		return cur, ErrUnknownStep
	}
	return s.change(w, func(p *Progress) error {
		if done {
			p.DoneSteps[key] = s.Now().UTC().Format(time.RFC3339)
		} else {
			delete(p.DoneSteps, key)
		}
		return nil
	})
}

// Dismiss hides "Getting started" (dismissed=false shows it again).
func (s *Service) Dismiss(w Who, dismissed bool) (View, error) {
	return s.change(w, func(p *Progress) error {
		now := s.Now()
		if dismissed {
			p.DismissedAt = &now
		} else {
			p.DismissedAt = nil
		}
		if p.WelcomedAt == nil {
			p.WelcomedAt = &now
		}
		return nil
	})
}

// Restart clears the person's progress: the welcome sheet shows again and
// ticked steps are cleared (steps that tick themselves stay done).
func (s *Service) Restart(w Who) (View, error) {
	if w.Preview || w.Role == "super_admin" {
		return s.View(w)
	}
	if err := s.DB.Where("organization_id = ? AND user_id = ?", w.Org, w.User).Delete(&Progress{}).Error; err != nil {
		return View{}, err
	}
	return s.View(w)
}
