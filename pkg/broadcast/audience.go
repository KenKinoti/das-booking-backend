package broadcast

import (
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
)

// Recipient is one person an announcement goes to.
type Recipient struct {
	UserID  string `json:"user_id"`
	OrgID   string `json:"organization_id"`
	OrgName string `json:"organization_name"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Level   string `json:"level"` // owner | manager | staff (pkg/emailprefs)
}

// OrgInfo is what the audience filters look at for an organisation.
type OrgInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	BusinessType string `json:"business_type"` // business-type key (pkg/industry)
	Country      string `json:"country"`       // ISO2, "" when unknown
	Tier         string `json:"tier"`          // plan tier, "" when none
	Suspended    bool   `json:"suspended"`
}

func fresh(db *gorm.DB) *gorm.DB { return db.Session(&gorm.Session{NewDB: true}) }

func hasTable(db *gorm.DB, name string) bool {
	var ok bool
	if err := fresh(db).Raw("SELECT to_regclass(?) IS NOT NULL", name).Row().Scan(&ok); err != nil {
		return false
	}
	return ok
}

func lowerSet(list []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range list {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out[s] = true
		}
	}
	return out
}

// Normalize cleans an audience (trimmed, de-duplicated, known role level).
func (a Audience) Normalize() Audience {
	clean := func(list []string, f func(string) string) []string {
		seen := map[string]bool{}
		out := []string{}
		for _, s := range list {
			s = f(strings.TrimSpace(s))
			if s != "" && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		return out
	}
	same := func(s string) string { return s }
	a.OrganizationIDs = clean(a.OrganizationIDs, same)
	a.BusinessTypes = clean(a.BusinessTypes, strings.ToLower)
	a.PlanTiers = clean(a.PlanTiers, strings.ToLower)
	a.Countries = clean(a.Countries, strings.ToUpper)
	switch a.RoleLevel {
	case LevelOwners, LevelManagers, LevelEveryone:
	default:
		a.RoleLevel = LevelEveryone
	}
	if a.ActiveWithinDays < 0 {
		a.ActiveWithinDays = 0
	}
	if a.ActiveWithinDays > 3650 {
		a.ActiveWithinDays = 3650
	}
	return a
}

// Orgs loads every organisation with the facts the filters use.
func (s *Service) Orgs() []OrgInfo {
	db := s.DB
	var rows []struct {
		ID, Name, BusinessType, CountryCode string
	}
	fresh(db).Raw(`SELECT id, COALESCE(name,'') AS name, COALESCE(business_type,'') AS business_type, COALESCE(country_code,'') AS country_code
		FROM organizations WHERE deleted_at IS NULL ORDER BY name`).Scan(&rows)
	types := map[string]string{}
	if hasTable(db, "industry_org_settings") {
		var ts []struct{ OrganizationID, TypeKey string }
		fresh(db).Raw(`SELECT organization_id, COALESCE(type_key,'') AS type_key FROM industry_org_settings`).Scan(&ts)
		for _, t := range ts {
			if k := strings.TrimSpace(t.TypeKey); k != "" {
				types[t.OrganizationID] = k
			}
		}
	}
	tiers := map[string]string{}
	if hasTable(db, "plan_subscriptions") {
		var ps []struct{ OrganizationID, Tier string }
		fresh(db).Raw(`SELECT organization_id, COALESCE(tier,'') AS tier FROM plan_subscriptions`).Scan(&ps)
		for _, p := range ps {
			tiers[p.OrganizationID] = p.Tier
		}
	}
	suspended := map[string]bool{}
	if hasTable(db, "platform_org_status") {
		var ss []string
		fresh(db).Raw(`SELECT organization_id FROM platform_org_status WHERE status = 'suspended'`).Scan(&ss)
		for _, id := range ss {
			suspended[id] = true
		}
	}
	out := make([]OrgInfo, 0, len(rows))
	for _, r := range rows {
		o := OrgInfo{ID: r.ID, Name: r.Name, Suspended: suspended[r.ID]}
		if k, ok := types[r.ID]; ok {
			o.BusinessType = k
		} else {
			o.BusinessType = industry.TypeKeyFor(r.BusinessType)
		}
		if cc := strings.ToUpper(strings.TrimSpace(r.CountryCode)); currency.IsCountry(cc) {
			o.Country = cc
		}
		if t, ok := tiers[r.ID]; ok {
			o.Tier = t
		} else if s.TierOf != nil {
			o.Tier = s.TierOf(r.ID)
		}
		out = append(out, o)
	}
	return out
}

func (a Audience) orgMatches(o OrgInfo, ids, types, tiers, countries map[string]bool) bool {
	if o.Suspended {
		return false
	}
	if len(ids) > 0 && !ids[strings.ToLower(o.ID)] {
		return false
	}
	if len(types) > 0 && !types[strings.ToLower(o.BusinessType)] {
		return false
	}
	if len(tiers) > 0 && !tiers[strings.ToLower(o.Tier)] {
		return false
	}
	if len(countries) > 0 && !countries[strings.ToLower(o.Country)] {
		return false
	}
	return true
}

// levelAllows: the person's level is inside the audience's role level.
func levelAllows(want, level string) bool {
	switch want {
	case LevelOwners:
		return level == emailprefs.LevelOwner
	case LevelManagers:
		return level == emailprefs.LevelOwner || level == emailprefs.LevelManager
	}
	return true
}

// Resolve lists the people an audience reaches (active members of active
// organisations), sorted by organisation then name. Offers leave out people
// who unsubscribed from them.
func (s *Service) Resolve(a Audience, kind string) []Recipient {
	a = a.Normalize()
	ids, types, tiers, countries := lowerSet(a.OrganizationIDs), lowerSet(a.BusinessTypes), lowerSet(a.PlanTiers), lowerSet(a.Countries)
	orgs := map[string]OrgInfo{}
	orgIDs := []string{}
	for _, o := range s.Orgs() {
		if a.orgMatches(o, ids, types, tiers, countries) {
			orgs[o.ID] = o
			orgIDs = append(orgIDs, o.ID)
		}
	}
	out := []Recipient{}
	if len(orgIDs) == 0 {
		return out
	}
	unsub := map[string]bool{}
	if kind == KindPromotion {
		var us []string
		fresh(s.DB).Model(&Unsubscribe{}).Pluck("user_id", &us)
		for _, u := range us {
			unsub[u] = true
		}
	}
	var cutoff *time.Time
	if a.ActiveWithinDays > 0 {
		t := Now().AddDate(0, 0, -a.ActiveWithinDays)
		cutoff = &t
	}
	for i := 0; i < len(orgIDs); i += 400 {
		j := i + 400
		if j > len(orgIDs) {
			j = len(orgIDs)
		}
		q := fresh(s.DB).Table("users").Select(`id, COALESCE(email,'') AS email, COALESCE(first_name,'') AS first_name, COALESCE(last_name,'') AS last_name,
			COALESCE(role,'') AS role, organization_id`).
			Where("organization_id IN ? AND deleted_at IS NULL AND (is_active IS NULL OR is_active)", orgIDs[i:j])
		if cutoff != nil {
			q = q.Where("last_login_at >= ?", *cutoff)
		}
		if a.ExcludeSuperAdmins {
			q = q.Where("COALESCE(role,'') <> 'super_admin'")
		}
		var users []struct {
			ID, Email, FirstName, LastName, Role, OrganizationID string
		}
		q.Scan(&users)
		for _, u := range users {
			if unsub[u.ID] {
				continue
			}
			level, _ := emailprefs.LevelOf(u.OrganizationID, u.ID, u.Role)
			if !levelAllows(a.RoleLevel, level) {
				continue
			}
			name := strings.TrimSpace(u.FirstName + " " + u.LastName)
			if name == "" {
				name = u.Email
			}
			out = append(out, Recipient{UserID: u.ID, OrgID: u.OrganizationID, OrgName: orgs[u.OrganizationID].Name,
				Email: strings.ToLower(strings.TrimSpace(u.Email)), Name: name, Role: u.Role, Level: level})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OrgName != out[j].OrgName {
			return strings.ToLower(out[i].OrgName) < strings.ToLower(out[j].OrgName)
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Preview summarises who an audience reaches.
type Preview struct {
	Count         int              `json:"count"`
	Organizations int              `json:"organizations"`
	WithEmail     int              `json:"with_email"`
	Unsubscribed  int              `json:"unsubscribed"` // offers: left out because they unsubscribed
	ByLevel       map[string]int   `json:"by_level"`
	TopOrgs       []map[string]any `json:"top_organizations"`
	Sample        []Recipient      `json:"sample"`
}

// PreviewAudience counts the recipients (and shows a few).
func (s *Service) PreviewAudience(a Audience, kind string) Preview {
	list := s.Resolve(a, kind)
	p := Preview{Count: len(list), ByLevel: map[string]int{"owner": 0, "manager": 0, "staff": 0}, TopOrgs: []map[string]any{}, Sample: []Recipient{}}
	perOrg := map[string]int{}
	names := map[string]string{}
	for _, r := range list {
		perOrg[r.OrgID]++
		names[r.OrgID] = r.OrgName
		p.ByLevel[r.Level]++
		if r.Email != "" {
			p.WithEmail++
		}
	}
	p.Organizations = len(perOrg)
	type kv struct {
		id string
		n  int
	}
	var ks []kv
	for id, n := range perOrg {
		ks = append(ks, kv{id, n})
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].n != ks[j].n {
			return ks[i].n > ks[j].n
		}
		return names[ks[i].id] < names[ks[j].id]
	})
	for i, k := range ks {
		if i == 6 {
			break
		}
		p.TopOrgs = append(p.TopOrgs, map[string]any{"id": k.id, "name": names[k.id], "count": k.n})
	}
	for i, r := range list {
		if i == 8 {
			break
		}
		p.Sample = append(p.Sample, r)
	}
	if kind == KindPromotion {
		all := s.Resolve(a, KindUpdate)
		p.Unsubscribed = len(all) - len(list)
	}
	return p
}
