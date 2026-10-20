package onboarding

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
)

// accessFor builds the access a role of type t has in a confirmed
// organisation whose plan includes everything (the plan is checked at run
// time; here the business type and the role must allow each step).
func accessFor(t industry.BusinessType, r industry.RoleTemplate) industry.Access {
	a := industry.Access{TypeKey: t.Key, TypeMods: map[string]bool{}, HiddenPath: t.HiddenPaths}
	all := false
	for _, m := range t.Modules {
		a.TypeMods[m] = true
		all = all || m == industry.AllModules
	}
	a.Restricted = !all
	a.Role = &industry.OrgRole{Key: r.Key, Name: r.Name, Base: r.Base, Scope: r.Scope, Perms: r.Perms, TypeKey: t.Key}
	return a
}

// routerPaths reads the app's route paths from src/router/index.js.
func routerPaths(t *testing.T) map[string]bool {
	t.Helper()
	cands := []string{os.Getenv("ROUTER_JS"), "../../../das-booking-frontend/src/router/index.js", "/home/claude/w/das-booking-frontend/src/router/index.js"}
	for _, c := range cands {
		if c == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(c))
		if err != nil {
			continue
		}
		out := map[string]bool{}
		for _, m := range regexp.MustCompile(`path:\s*'([^']+)'`).FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
		if len(out) < 20 {
			t.Fatalf("only %d routes parsed from %s", len(out), c)
		}
		return out
	}
	t.Skip("src/router/index.js not found (set ROUTER_JS)")
	return nil
}

var leftover = regexp.MustCompile(`\{[A-Za-z_]+\}`)

func checkSteps(t *testing.T, label string, steps []industry.Step, terms []map[string]string, routes map[string]bool) {
	t.Helper()
	if len(steps) < 3 || len(steps) > 6 {
		t.Errorf("%s: %d steps, want 3–6", label, len(steps))
	}
	keys := map[string]bool{}
	for _, s := range steps {
		where := label + " / " + s.Key
		if s.Key == "" || keys[s.Key] {
			t.Errorf("%s: missing or duplicate key", where)
		}
		keys[s.Key] = true
		if strings.TrimSpace(s.Title) == "" || strings.TrimSpace(s.Description) == "" || !strings.HasPrefix(s.To, "/") {
			t.Errorf("%s: title, description and a route are required: %+v", where, s)
		}
		for _, tm := range terms {
			title, desc := FillTerms(s.Title, tm), FillTerms(s.Description, tm)
			if n := len([]rune(desc)); n > 140 {
				t.Errorf("%s: description is %d characters (max 140): %q", where, n, desc)
			}
			if n := len([]rune(title)); n > 60 {
				t.Errorf("%s: title is %d characters (max 60): %q", where, n, title)
			}
			if leftover.MatchString(title + desc) {
				t.Errorf("%s: placeholder left in %q / %q", where, title, desc)
			}
		}
		if s.DoneWhen != "" && !Known(s.DoneWhen) {
			t.Errorf("%s: done_when %q has no detector", where, s.DoneWhen)
		}
		if _, ok := RuleFor(s.To); !ok {
			t.Errorf("%s: route %s has no RouteRule", where, s.To)
		}
		if routes != nil && !routes[RoutePath(s.To)] {
			t.Errorf("%s: route %s is not in src/router/index.js", where, s.To)
		}
	}
}

// Every role of every business type has its own 3–6 steps, in plain words,
// linking to pages that exist and that the role can open.
func TestEveryTypeAndRoleHasSteps(t *testing.T) {
	routes := routerPaths(t)
	types := industry.DefaultTypes()
	if len(types) < 16 {
		t.Fatalf("only %d types", len(types))
	}
	roles := 0
	for _, bt := range types {
		terms := []map[string]string{industry.DefaultTerms(), industry.Terms(bt)}
		for _, r := range bt.Roles {
			roles++
			label := bt.Key + "/" + r.Key
			if len(r.Steps) == 0 {
				t.Errorf("%s: the role template has no steps of its own", label)
				continue
			}
			steps := industry.StepsFor(bt, r.Key, r.Base)
			checkSteps(t, label, steps, terms, routes)
			a := accessFor(bt, r)
			for _, s := range steps {
				if !Reachable(a, bt, true, r.Base, s.To) {
					t.Errorf("%s / %s: %s is not reachable for this role (module, permission, hidden page or feature)", label, s.Key, s.To)
				}
			}
		}
	}
	if roles < 70 {
		t.Fatalf("only %d roles checked", roles)
	}
}

// The examples from the brief, in the person's own words.
func TestNightclubSteps(t *testing.T) {
	bt := industry.DefaultType("nightclub")
	keys := func(role string) string {
		r, _ := industry.RoleTemplateByKey(bt, role)
		out := []string{}
		for _, s := range industry.StepsFor(bt, role, r.Base) {
			out = append(out, s.Key)
		}
		return strings.Join(out, ",")
	}
	for role, want := range map[string]string{
		"owner":        "setup,prices,team,delivery,count,variance",
		"bartender":    "bar,sale,request,count,wastage",
		"store_keeper": "delivery,issue,count,levels",
	} {
		if got := keys(role); got != want {
			t.Errorf("%s steps %s, want %s", role, got, want)
		}
	}
	bar := industry.StepsFor(bt, "bartender", industry.BaseStaff)
	titles := []string{}
	for _, s := range bar {
		titles = append(titles, s.Title)
	}
	want := "Pick your bar on the till|Ring up a tot, a bottle and a cocktail|Ask the store for stock|Count your bar at closing|Log a breakage or a comp"
	if strings.Join(titles, "|") != want {
		t.Fatalf("bartender titles %v", titles)
	}
	if bar[1].DoneWhen != "my_pos_sale" || bar[1].To != "/pos" || bar[2].DoneWhen != "my_transfer_request" {
		t.Fatalf("bartender sale/request %+v %+v", bar[1], bar[2])
	}
	// Bar stock pages are hidden as /inventory for a club; steps never link there.
	for _, r := range bt.Roles {
		for _, s := range industry.StepsFor(bt, r.Key, r.Base) {
			if RoutePath(s.To) == "/inventory" || RoutePath(s.To) == "/crm" || strings.HasPrefix(s.To, "/invoices") {
				t.Errorf("%s/%s links to %s (hidden or not a club module)", r.Key, s.Key, s.To)
			}
		}
	}
}

func TestGeneralOwnerAndStaffSteps(t *testing.T) {
	bt := industry.DefaultType("general")
	var got []string
	for _, s := range industry.StepsFor(bt, "owner", industry.BaseAdmin) {
		got = append(got, s.Key+":"+s.DoneWhen)
	}
	if strings.Join(got, ",") != "type:type_chosen,details:business_details,logo:logo,customer:customer,invoice:invoice,team:staff" {
		t.Fatalf("general owner %v", got)
	}
	staff := industry.StepsFor(bt, "staff", industry.BaseStaff)
	if len(staff) != len(industry.BaseSteps(industry.BaseStaff)) || staff[0].Key != "phone" {
		t.Fatalf("general staff gets the generic staff steps: %+v", staff)
	}
}

// Roles without steps (custom roles, types added by a super admin, registry
// rows seeded before steps existed) still get sensible steps.
func TestStepsFallbacks(t *testing.T) {
	club := industry.DefaultType("nightclub")
	key := func(s []industry.Step) string {
		out := []string{}
		for _, x := range s {
			out = append(out, x.Key)
		}
		return strings.Join(out, ",")
	}
	owner := key(industry.StepsFor(club, "owner", industry.BaseAdmin))
	if got := key(industry.StepsFor(club, "custom-1234", industry.BaseAdmin)); got != owner {
		t.Fatalf("custom admin role in a club gets the owner's steps: %s", got)
	}
	mgr := key(industry.StepsFor(club, "bar_manager", industry.BaseManager))
	if got := key(industry.StepsFor(club, "custom-1234", industry.BaseManager)); got != mgr {
		t.Fatalf("custom manager role gets the bar manager's steps: %s", got)
	}
	if got := key(industry.StepsFor(club, "custom-1234", industry.BaseStaff)); got != key(industry.BaseSteps(industry.BaseStaff)) {
		t.Fatalf("custom staff role gets the generic staff steps: %s", got)
	}
	// A registry row saved without steps falls back to the built-in ones.
	stripped := club
	stripped.Roles = append([]industry.RoleTemplate{}, club.Roles...)
	for i := range stripped.Roles {
		stripped.Roles[i].Steps = nil
	}
	if got := key(industry.StepsFor(stripped, "bartender", industry.BaseStaff)); got != "bar,sale,request,count,wastage" {
		t.Fatalf("registry row without steps: %s", got)
	}
	// A new type with no steps anywhere: base defaults.
	newType := industry.BusinessType{Key: "laundromat", Modules: []string{industry.AllModules}, Roles: []industry.RoleTemplate{{Key: "owner", Base: industry.BaseAdmin}}}
	if got := key(industry.StepsFor(newType, "owner", industry.BaseAdmin)); got != key(industry.BaseSteps(industry.BaseAdmin)) {
		t.Fatalf("new type owner: %s", got)
	}
	// Edited registry steps win.
	edited := stripped
	edited.Roles[3].Steps = []industry.Step{{Key: "x", Title: "X", Description: "x", To: "/pos"}}
	if got := key(industry.StepsFor(edited, edited.Roles[3].Key, industry.BaseStaff)); got != "x" {
		t.Fatalf("edited steps: %s", got)
	}
}

func TestBaseStepsValid(t *testing.T) {
	routes := routerPaths(t)
	general := industry.DefaultType("general")
	for _, base := range []string{industry.BaseAdmin, industry.BaseManager, industry.BaseStaff, industry.BaseReadOnly} {
		steps := industry.BaseSteps(base)
		var terms []map[string]string
		for _, bt := range industry.DefaultTypes() {
			terms = append(terms, industry.Terms(bt))
		}
		checkSteps(t, "base/"+base, steps, terms, routes)
		// In a general business every module is on: the generic role of each
		// level can open every step.
		var r industry.RoleTemplate
		for _, x := range general.Roles {
			if x.Base == base {
				r = x
			}
		}
		a := accessFor(general, r)
		for _, s := range steps {
			if !Reachable(a, general, true, base, s.To) {
				t.Errorf("base %s step %s (%s) is not reachable for %s", base, s.Key, s.To, r.Key)
			}
		}
	}
}

func TestFillTerms(t *testing.T) {
	club := industry.Terms(industry.DefaultType("nightclub"))
	ndis := industry.Terms(industry.DefaultType("ndis"))
	for in, want := range map[string]string{
		"Add your first {customer}":        "Add your first guest",
		"Open {Bookings}":                  "Open Table reservations",
		"Find {a_customer} or {a_booking}": "Find a guest or a reservation",
		"Keep {unknown} as is":             "Keep {unknown} as is",
	} {
		if got := FillTerms(in, club); got != want {
			t.Errorf("club %q → %q, want %q", in, got, want)
		}
	}
	if got := FillTerms("Add {a_customer}", industry.Terms(industry.DefaultType("restaurant"))); got != "Add a guest" {
		t.Errorf("restaurant: %q", got)
	}
	if got := FillTerms("Add {a_customer}", industry.Terms(industry.DefaultType("education"))); got != "Add a student" {
		t.Errorf("education: %q", got)
	}
	if got := FillTerms("Ask {a_staff_member}", ndis); got != "Ask a support worker" {
		t.Errorf("ndis: %q", got)
	}
	if got := FillTerms("Book {a_booking}", industry.Terms(industry.DefaultType("salon"))); got != "Book an appointment" {
		t.Errorf("salon article: %q", got)
	}
}

func TestReachableRules(t *testing.T) {
	club := industry.DefaultType("nightclub")
	bartender, _ := industry.RoleTemplateByKey(club, "bartender")
	a := accessFor(club, bartender)
	for to, want := range map[string]bool{
		"/pos": true, "/bar-stock?tab=counts": true, "/dashboard": false, "/inventory": false,
		"/staff": false, "/bookings": false, "/profile": true, "/setup": false, "/nowhere": false,
	} {
		if got := Reachable(a, club, true, industry.BaseStaff, to); got != want {
			t.Errorf("bartender %s: %v, want %v", to, got, want)
		}
	}
	// Bar stock needs a confirmed club (the feature is off otherwise).
	if Reachable(a, club, false, industry.BaseStaff, "/bar-stock") {
		t.Error("bar stock in an unconfirmed organisation")
	}
	own, _ := industry.RoleTemplateByKey(club, "owner")
	oa := accessFor(club, own)
	if !Reachable(oa, club, true, industry.BaseAdmin, "/setup") || Reachable(oa, club, true, industry.BaseAdmin, "/invoices/new") {
		t.Error("owner: setup yes, invoices (not a club module) no")
	}
	// The plan is checked too.
	oa.Plan = map[string]bool{"inventory": true}
	if Reachable(oa, club, true, industry.BaseAdmin, "/pos") || !Reachable(oa, club, true, industry.BaseAdmin, "/bar-stock") {
		t.Error("plan without POS")
	}
	if p := RoutePath("/settings#business-type"); p != "/settings" {
		t.Errorf("RoutePath %q", p)
	}
}
