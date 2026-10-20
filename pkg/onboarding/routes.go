package onboarding

import (
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
)

// RouteRule mirrors how the web app guards a page (src/navigation.js nav
// items + the router guard): the module it belongs to, the permission and
// action the role needs, a business-type feature, a minimum base level or
// admins only. A step is only shown when the person can open its page.
type RouteRule struct {
	Prefix  string
	Module  string // entitlement module ("core" = always in the plan)
	Perm    string // permission module when it differs from Module (e.g. "team")
	Act     string // action needed (default view)
	Feature string // business-type feature (e.g. "bar_stock")
	MinBase string // minimum base level (manager, admin)
	Admin   bool   // organisation admins only (the setup wizard)
}

// RouteRules are the pages steps may link to. A step whose route is not
// listed here is never shown (and the tests fail), so every new step's page
// is checked against the role's permissions.
var RouteRules = []RouteRule{
	{Prefix: "/setup", Admin: true},
	{Prefix: "/dashboard", Module: "core"},
	{Prefix: "/insights", Module: "core", MinBase: industry.BaseManager},
	{Prefix: "/reports-analytics", Module: "core"},
	{Prefix: "/reports", Module: "core"},
	{Prefix: "/reports/profit-loss", Module: "core"},
	{Prefix: "/expenses", Module: "core"},
	{Prefix: "/assistant", Module: "core"},
	{Prefix: "/staff", Module: "core", Perm: industry.PermTeam},
	{Prefix: "/business", Module: "smallbiz"},
	{Prefix: "/billing", Module: "invoicing"},
	{Prefix: "/invoices", Module: "invoicing"},
	{Prefix: "/invoices/settings", Module: "invoicing"},
	{Prefix: "/quotes", Module: "invoicing"},
	{Prefix: "/pos", Module: "pos"},
	{Prefix: "/pos-transactions", Module: "pos"},
	{Prefix: "/ecommerce", Module: "ecommerce"},
	{Prefix: "/customers", Module: "crm"},
	{Prefix: "/crm", Module: "crm"},
	{Prefix: "/bookings", Module: "bookings"},
	{Prefix: "/vehicles", Module: "bookings", Act: industry.ActCreate, Feature: "vehicles"},
	{Prefix: "/scheduling", Module: "bookings", Act: industry.ActCreate},
	{Prefix: "/services", Module: "bookings", Act: industry.ActCreate},
	{Prefix: "/events", Module: "events"},
	{Prefix: "/inventory", Module: "inventory"},
	{Prefix: "/bar-stock", Module: "inventory", Feature: "bar_stock"},
	{Prefix: "/suppliers", Module: "inventory", Act: industry.ActCreate},
	{Prefix: "/production", Module: "manufacturing"},
	{Prefix: "/finance", Module: "accounting"},
	{Prefix: "/bills", Module: "accounting"},
	{Prefix: "/banking", Module: "accounting"},
	{Prefix: "/supplier-prices", Module: "accounting", MinBase: industry.BaseManager},
	{Prefix: "/hcm", Module: "hr"},
	{Prefix: "/projects", Module: "projects"},
	{Prefix: "/documents", Module: "documents"},
	{Prefix: "/meetings", Module: "communication"},
	{Prefix: "/messages", Module: "communication"},
	{Prefix: "/communication", Module: "communication"},
	{Prefix: "/settings"},
	{Prefix: "/plan", MinBase: industry.BaseAdmin},
	{Prefix: "/profile"},
	{Prefix: "/faq"},
}

// RoutePath is a step route without its query string and hash.
func RoutePath(to string) string {
	if i := strings.IndexAny(to, "?#"); i >= 0 {
		to = to[:i]
	}
	if len(to) > 1 {
		to = strings.TrimSuffix(to, "/")
	}
	return to
}

// RuleFor returns the most specific rule for a route.
func RuleFor(to string) (RouteRule, bool) {
	p := RoutePath(to)
	best, found := RouteRule{}, false
	for _, r := range RouteRules {
		if p == r.Prefix || strings.HasPrefix(p, r.Prefix+"/") {
			if !found || len(r.Prefix) > len(best.Prefix) {
				best, found = r, true
			}
		}
	}
	return best, found
}

var baseRank = map[string]int{industry.BaseReadOnly: 0, industry.BaseStaff: 1, industry.BaseManager: 2, industry.BaseAdmin: 3}

// Reachable reports whether a person with access a (role base level base)
// in an organisation of type t can open the page at to — the same checks
// as the web app's router guard and navigation: plan ∩ business type, the
// role's permission, pages the type hides, type features and base level.
func Reachable(a industry.Access, t industry.BusinessType, confirmed bool, base, to string) bool {
	r, ok := RuleFor(to)
	if !ok {
		return false
	}
	if a.SuperAdmin {
		return true
	}
	if r.Admin && base != industry.BaseAdmin {
		return false
	}
	if r.Module != "" && !a.ModuleOn(r.Module) {
		return false
	}
	perm, act := r.Perm, r.Act
	if perm == "" {
		perm = r.Module
	}
	if act == "" {
		act = industry.ActView
	}
	if perm != "" && !industry.RoleCan(a.Role, perm, act) {
		return false
	}
	p := RoutePath(to)
	for _, h := range a.HiddenPath {
		if h == r.Prefix || p == h || strings.HasPrefix(p, h+"/") {
			return false
		}
	}
	if r.Feature != "" {
		has := false
		for _, f := range t.Features {
			has = has || f == r.Feature
		}
		if !confirmed || !has {
			return false
		}
	}
	if r.MinBase != "" && baseRank[base] < baseRank[r.MinBase] {
		return false
	}
	return true
}
