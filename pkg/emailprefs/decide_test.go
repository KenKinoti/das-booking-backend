package emailprefs

import (
	"fmt"
	"testing"
)

func bp(b bool) *bool { return &b }

// Every mode × level × switch combination of the decision.
func TestDecideMatrix(t *testing.T) {
	people := map[string]Person{
		LevelOwner:   {UserID: "o", Role: "admin", Level: LevelOwner},
		LevelManager: {UserID: "m", Role: "manager", Level: LevelManager},
		LevelStaff:   {UserID: "s", Role: "staff", Level: LevelStaff},
	}
	bookings, _ := CategoryByKey(Bookings)
	want := map[string]map[string]bool{ // mode → level → on (no switches)
		ModeEveryone:       {LevelOwner: true, LevelManager: true, LevelStaff: true},
		ModeOwnersManagers: {LevelOwner: true, LevelManager: true, LevelStaff: false},
		ModeOwners:         {LevelOwner: true, LevelManager: false, LevelStaff: false},
	}
	for mode, levels := range want {
		for lvl, on := range levels {
			p := people[lvl]
			name := fmt.Sprintf("%s/%s", mode, lvl)
			d := decide("org", mode, p, bookings, nil)
			if d.On != on {
				t.Errorf("%s: got %v want %v (%s)", name, d.On, on, d.Reason)
			}
			if !on && d.Reason != ReasonMode {
				t.Errorf("%s: reason %s", name, d.Reason)
			}
			// An admin's "off" always wins, an admin's "on" beats the mode.
			if d := decide("org", mode, p, bookings, map[string]Pref{Bookings: {Admin: bp(false)}}); d.On || d.Reason != ReasonAdminOff {
				t.Errorf("%s admin off: %+v", name, d)
			}
			if d := decide("org", mode, p, bookings, map[string]Pref{Bookings: {Admin: bp(true)}}); !d.On || d.Reason != ReasonAdminOn {
				t.Errorf("%s admin on: %+v", name, d)
			}
			// All off (admin) beats everything, also an admin's "on" for one category.
			if d := decide("org", mode, p, bookings, map[string]Pref{AllKey: {Admin: bp(false)}, Bookings: {Admin: bp(true)}}); d.On || d.Reason != ReasonAllOff {
				t.Errorf("%s all off: %+v", name, d)
			}
			// The person's own "off" beats an admin's "on".
			if d := decide("org", mode, p, bookings, map[string]Pref{Bookings: {Admin: bp(true), SelfOff: true}}); d.On || d.Reason != ReasonSelfOff {
				t.Errorf("%s self off: %+v", name, d)
			}
			if d := decide("org", mode, p, bookings, map[string]Pref{AllKey: {SelfOff: true}}); d.On || d.Reason != ReasonSelfAllOff {
				t.Errorf("%s self all off: %+v", name, d)
			}
			// An admin's "off" is reported before the person's own.
			if d := decide("org", mode, p, bookings, map[string]Pref{Bookings: {Admin: bp(false), SelfOff: true}}); d.Reason != ReasonAdminOff {
				t.Errorf("%s admin+self off: %+v", name, d)
			}
		}
	}
}

func TestAudiences(t *testing.T) {
	Can = nil
	defer func() { Can = nil }()
	staff := Person{UserID: "s", Role: "staff", Level: LevelStaff}
	mgr := Person{UserID: "m", Role: "manager", Level: LevelManager}
	owner := Person{UserID: "o", Role: "owner", Level: LevelOwner}
	for _, c := range []struct {
		cat  string
		p    Person
		want bool
	}{
		{Bookings, staff, true}, {Agenda, staff, true},
		{BillReminders, staff, false}, {BillReminders, mgr, false}, {BillReminders, owner, true},
		{StrategyBrief, mgr, false}, {StrategyBrief, owner, true},
		{BarNightReport, staff, false}, {BarNightReport, mgr, true}, {BarNightReport, owner, true},
	} {
		cat, _ := CategoryByKey(c.cat)
		d := decide("org", ModeEveryone, c.p, cat, map[string]Pref{c.cat: {Admin: bp(true)}})
		if d.On != c.want {
			t.Errorf("%s for %s: %+v", c.cat, c.p.Role, d)
		}
		if !c.want && d.Reason != ReasonNotEligible {
			t.Errorf("%s for %s: reason %s", c.cat, c.p.Role, d.Reason)
		}
	}
	// The business-type permission decides the night report when wired.
	Can = func(org, user, legacy, module, act string) bool {
		return user == "s" && module == "inventory" && act == "approve"
	}
	cat, _ := CategoryByKey(BarNightReport)
	if !decide("org", ModeEveryone, staff, cat, nil).On || decide("org", ModeEveryone, mgr, cat, nil).On {
		t.Fatal("Can hook not used for the night report")
	}
}

func TestLevelOf(t *testing.T) {
	defer func() { RoleOf = nil }()
	RoleOf = nil
	for legacy, want := range map[string]string{"owner": LevelOwner, "admin": LevelOwner, "org_admin": LevelOwner, "super_admin": LevelOwner,
		"manager": LevelManager, "staff": LevelStaff, "care_worker": LevelStaff, "": LevelStaff} {
		if got, _ := LevelOf("o", "u", legacy); got != want {
			t.Errorf("legacy %q: %s want %s", legacy, got, want)
		}
	}
	// The business-type role template decides (Bar manager → manager, Bartender → staff) …
	RoleOf = func(org, user, legacy string) (string, string, bool) {
		switch user {
		case "bm":
			return "manager", "Bar manager", true
		case "bt":
			return "staff", "Bartender", true
		case "weird":
			return "staff", "Front desk", true
		}
		return "", "", false
	}
	if l, n := LevelOf("o", "bm", "manager"); l != LevelManager || n != "Bar manager" {
		t.Errorf("bar manager: %s %s", l, n)
	}
	if l, n := LevelOf("o", "bt", "staff"); l != LevelStaff || n != "Bartender" {
		t.Errorf("bartender: %s %s", l, n)
	}
	// … but an owner / admin by users.role stays an owner.
	if l, _ := LevelOf("o", "weird", "org_admin"); l != LevelOwner {
		t.Errorf("org_admin with a staff template: %s", l)
	}
	if l, n := LevelOf("o", "nobody", "manager"); l != LevelManager || n != "Manager" {
		t.Errorf("fallback: %s %s", l, n)
	}
}

func TestOneLineCategories(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Categories {
		if c.Key == "" || c.Label == "" || c.Description == "" || c.Audience == "" || seen[c.Key] || c.Key == AllKey {
			t.Errorf("bad category %+v", c)
		}
		seen[c.Key] = true
	}
	for _, k := range []string{Bookings, Agenda, BillReminders, BarNightReport, StrategyBrief} {
		if !seen[k] {
			t.Errorf("missing %s", k)
		}
	}
	if !ValidMode(ModeOwners) || ValidMode("nobody") || len(Modes) != 3 {
		t.Fatal("modes")
	}
}
