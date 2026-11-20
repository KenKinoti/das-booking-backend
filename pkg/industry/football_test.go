package industry

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Football analysis (pkg/football) is an opt-in module: the Nightclub / bar /
// lounge type and the platform organisation have it; "*" types and
// unconfirmed organisations don't; owners and managers by default, staff only
// with an explicit grant.

var withFootball = append(append([]string{}, everyModule...), "football")

func footballActs(a Access) string {
	out := []string{}
	for _, act := range AllActions {
		if a.Can("football", act) {
			out = append(out, act)
		}
	}
	return strings.Join(out, ",")
}

func TestFootballOptInModule(t *testing.T) {
	if !contains(typeByKey(t, "nightclub").Modules, "football") || !contains(ModuleKeys(), "football") {
		t.Fatal("football is a plans module and part of the nightclub type")
	}
	all := "view,create,edit,delete,approve"
	cases := []struct {
		name string
		a    Access
		want string
	}{
		{"club owner", accessOf(t, "nightclub", true, withFootball, nil, roleOf(t, "nightclub", "owner")), all},
		{"club bar manager", accessOf(t, "nightclub", true, withFootball, nil, roleOf(t, "nightclub", "bar_manager")), all},
		{"club bartender", accessOf(t, "nightclub", true, withFootball, nil, roleOf(t, "nightclub", "bartender")), ""},
		{"club host", accessOf(t, "nightclub", true, withFootball, nil, roleOf(t, "nightclub", "host")), ""},
		{"club without football in the plan", accessOf(t, "nightclub", true, everyModule, nil, roleOf(t, "nightclub", "owner")), ""},
		{"salon owner", accessOf(t, "salon", true, withFootball, nil, roleOf(t, "salon", "owner")), ""},
		{"salon owner who enabled it", accessOf(t, "salon", true, withFootball, []string{"football"}, roleOf(t, "salon", "owner")), all},
		{"general type owner", accessOf(t, "general", true, withFootball, nil, roleOf(t, "general", "owner")), ""},
		{"unconfirmed restaurant manager", accessOf(t, "restaurant", false, withFootball, nil, roleOf(t, "restaurant", "restaurant_manager")), ""},
	}
	for _, c := range cases {
		if got := footballActs(c.a); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// The platform organisation (has a super admin): owners and managers yes,
	// staff and read-only only when granted.
	plat := func(role *OrgRole) Access {
		a := accessOf(t, "general", false, withFootball, nil, role)
		a.Platform = true
		return a
	}
	if footballActs(plat(roleOf(t, "general", "owner"))) != all || footballActs(plat(roleOf(t, "general", "manager"))) != all {
		t.Fatal("platform owner / manager")
	}
	if footballActs(plat(roleOf(t, "general", "staff"))) != "" || footballActs(plat(roleOf(t, "general", "viewer"))) != "" {
		t.Fatal("platform staff must not get football through \"*\"")
	}
	granted := *roleOf(t, "general", "staff")
	granted.Perms = Perms{AllModules: work, "football": []string{ActView, ActCreate}}
	if footballActs(plat(&granted)) != "view,create" {
		t.Fatal("explicit grant")
	}
	ro := *roleOf(t, "general", "viewer")
	ro.Perms = Perms{"football": full}
	if footballActs(plat(&ro)) != "view" {
		t.Fatal("read-only stays read-only")
	}
	// An explicit empty grant takes a manager's default away.
	m := *roleOf(t, "nightclub", "bar_manager")
	m.Perms = Perms{"inventory": full, "football": nopes}
	if footballActs(accessOf(t, "nightclub", true, withFootball, nil, &m)) != "" {
		t.Fatal("explicit no for a manager")
	}
	// Other modules keep the "*" behaviour.
	if !plat(roleOf(t, "general", "staff")).Can("invoicing", ActCreate) {
		t.Fatal("staff still work on other modules")
	}
}

// Requests to /api/v1/football answer per role (plan rule /football).
func TestFootballDecide(t *testing.T) {
	owner := accessOf(t, "nightclub", true, withFootball, nil, roleOf(t, "nightclub", "owner"))
	bartender := accessOf(t, "nightclub", true, withFootball, nil, roleOf(t, "nightclub", "bartender"))
	salon := accessOf(t, "salon", true, withFootball, nil, roleOf(t, "salon", "owner"))
	for _, c := range []struct {
		name   string
		a      Access
		method string
		path   string
		code   string
	}{
		{"owner reads", owner, "GET", "/api/v1/football/fixtures", ""},
		{"owner records a bet", owner, "POST", "/api/v1/football/bets", ""},
		{"owner settles", owner, "POST", "/api/v1/football/bets/x/settle", ""},
		{"bartender", bartender, "GET", "/api/v1/football/fixtures", "ROLE_FORBIDDEN"},
		{"salon", salon, "GET", "/api/v1/football/fixtures", "MODULE_HIDDEN_FOR_TYPE"},
	} {
		d := Decide(c.a, c.method, c.path, nil)
		if (c.code == "") != d.Allowed || d.Code != c.code {
			t.Errorf("%s: %+v", c.name, d)
		}
		if !d.Allowed && d.Status != http.StatusForbidden {
			t.Errorf("%s: status %d", c.name, d.Status)
		}
	}
}

// Existing databases: the nightclub type gets football once; a super
// admin's later removal sticks. The platform organisation is detected.
func TestFootballAdditionAndPlatform(t *testing.T) {
	db := testDB(t)
	var bt BusinessType
	db.Where("key = 'nightclub'").First(&bt)
	if !contains(bt.Modules, "football") {
		t.Fatal("fresh install")
	}
	// Simulate a database seeded before v2.16.
	db.Model(&BusinessType{}).Where("key = 'nightclub'").Select("modules").Updates(&BusinessType{Modules: []string{"pos", "inventory"}})
	db.Where("key = ?", "added:nightclub:football").Delete(&Meta{})
	if err := Seed(db); err != nil {
		t.Fatal(err)
	}
	db.Where("key = 'nightclub'").First(&bt)
	if strings.Join(bt.Modules, ",") != "pos,inventory,football" {
		t.Fatalf("upgrade %v", bt.Modules)
	}
	// Removed by a super admin → stays removed.
	db.Model(&BusinessType{}).Where("key = 'nightclub'").Select("modules").Updates(&BusinessType{Modules: []string{"pos", "inventory"}})
	Seed(db)
	db.Where("key = 'nightclub'").First(&bt)
	if contains(bt.Modules, "football") {
		t.Fatal("re-added after removal")
	}
	// Platform organisation flag.
	svc := NewService(db)
	f := seedOrg(t, db, "general")
	a, _ := svc.AccessFor(f.org, f.admin, "admin")
	if a.Platform || a.TypeShows("football") {
		t.Fatal("ordinary organisation")
	}
	db.Exec(`INSERT INTO users (id, email, role, organization_id) VALUES (?, ?, 'super_admin', ?)`, uuid.NewString(), "boss@t.test", f.org)
	svc.Invalidate(f.org)
	a, _ = svc.AccessFor(f.org, f.admin, "admin")
	if !a.Platform || !a.TypeShows("football") {
		t.Fatal("platform organisation")
	}
}
