package industry

import (
	"net/http"
	"sort"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/plans"
)

// Access is what one user may do in their organisation, after applying the
// plan, the business type and their role — in that order.
type Access struct {
	SuperAdmin bool
	TypeKey    string
	// Restricted is true when the business type limits modules (a confirmed,
	// non-general type). Unconfirmed / general organisations keep every
	// module of their plan.
	Restricted bool
	TypeMods   map[string]bool // modules of the type ∪ admin-enabled extras
	Plan       map[string]bool // plan entitlements (nil = unknown → fail open)
	HiddenPath []string
	Role       *OrgRole // nil = unknown → fail open
	UserID     string
}

func normKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// InPlan reports whether the plan includes module m (core always).
func (a Access) InPlan(m string) bool {
	if m == "" || m == plans.CoreModule || m == PermCore || m == PermTeam || a.Plan == nil {
		return true
	}
	return a.Plan[m]
}

// TypeShows reports whether the business type shows module m.
func (a Access) TypeShows(m string) bool {
	if m == "" || m == plans.CoreModule || m == PermCore || m == PermTeam || !a.Restricted {
		return true
	}
	return a.TypeMods[m]
}

// ModuleOn: in the plan and shown for the business type.
func (a Access) ModuleOn(m string) bool { return a.InPlan(m) && a.TypeShows(m) }

// actionsFor returns the actions a permission set grants on module m. An
// explicit entry wins over the "*" wildcard.
func actionsFor(p Perms, m string) []string {
	if acts, ok := p[m]; ok {
		return acts
	}
	return p[AllModules]
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// RoleCan reports whether role r may perform act on module m (ignoring the
// plan and the business type).
func RoleCan(r *OrgRole, m, act string) bool {
	if r == nil {
		return true
	}
	switch r.Base {
	case BaseAdmin:
		return true
	case BaseReadOnly:
		if act != ActView {
			return false
		}
	}
	return contains(actionsFor(r.Perms, m), act)
}

// Can is the full check: super admin bypass → plan → type → role.
func (a Access) Can(m, act string) bool {
	if a.SuperAdmin {
		return true
	}
	if !a.ModuleOn(m) {
		return false
	}
	return RoleCan(a.Role, m, act)
}

// Scope is the role's data scope ("all" or "own").
func (a Access) Scope() string {
	if a.SuperAdmin || a.Role == nil || a.Role.Base == BaseAdmin || a.Role.Scope == "" {
		return ScopeAll
	}
	return a.Role.Scope
}

// EffectiveModules lists the modules that are on (plan ∩ type), sorted.
func (a Access) EffectiveModules(all []string) []string {
	out := []string{}
	for _, m := range all {
		if a.ModuleOn(m) {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// Permissions expands the role into module → actions for the modules given
// (plus the core and team pseudo modules), already intersected with the
// plan and the business type.
func (a Access) Permissions(mods []string) map[string][]string {
	out := map[string][]string{}
	for _, m := range append(append([]string{}, mods...), PermCore, PermTeam) {
		acts := []string{}
		for _, act := range AllActions {
			if a.Can(m, act) {
				acts = append(acts, act)
			}
		}
		out[m] = acts
	}
	return out
}

// ActionFor maps an HTTP request to a permission action.
func ActionFor(method, path string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ActView
	case http.MethodDelete:
		return ActDelete
	case http.MethodPut, http.MethodPatch:
		return ActEdit
	}
	p := strings.TrimSuffix(path, "/")
	last := p[strings.LastIndex(p, "/")+1:]
	switch last {
	case "approve", "decide", "void", "reject", "approve-all":
		return ActApprove
	case "status", "toggle-status", "receive", "close", "mileage", "cancel", "complete", "check-in", "checkin":
		return ActEdit
	}
	return ActCreate
}

// Decision is the outcome of Decide.
type Decision struct {
	Allowed bool
	Status  int
	Code    string // MODULE_HIDDEN_FOR_TYPE | ROLE_FORBIDDEN
	Module  string
	Action  string
	Message string
	// OwnScope is set when the request touches records limited to the
	// user's own (bookings / job cards assigned to them).
	OwnScope bool
}

// ScopedModules are the modules where the "own" data scope applies.
var ScopedModules = map[string]bool{"bookings": true}

// Decide checks a request against the business type and the role. The plan
// check (402 MODULE_NOT_IN_PLAN) runs before this in plans.Enforcer.
func Decide(a Access, method, path string, label func(string) string) Decision {
	if a.SuperAdmin {
		return Decision{Allowed: true}
	}
	r, ok := plans.MatchRule(path)
	if !ok {
		return Decision{Allowed: true}
	}
	read := method == http.MethodGet || method == http.MethodHead
	cands := append([]string{}, r.Modules...)
	if read {
		cands = append(cands, r.ReadAlso...)
	}
	act := ActionFor(method, strings.TrimPrefix(path, "/api/v1"))
	if label == nil {
		label = func(s string) string { return s }
	}
	shown := []string{}
	for _, m := range cands {
		if a.ModuleOn(m) {
			shown = append(shown, m)
		}
	}
	if len(shown) == 0 && !(read && r.ReadAny) {
		return Decision{Status: http.StatusForbidden, Code: "MODULE_HIDDEN_FOR_TYPE", Module: r.Modules[0], Action: act,
			Message: label(r.Modules[0]) + " is not part of your business type. An admin can enable it in Settings → Business type."}
	}
	allowed := read && r.ReadAny
	via := ""
	for _, m := range shown {
		if RoleCan(a.Role, m, act) {
			allowed, via = true, m
			break
		}
	}
	if !allowed {
		roleName := "your role"
		if a.Role != nil && a.Role.Name != "" {
			roleName = "the " + a.Role.Name + " role"
		}
		return Decision{Status: http.StatusForbidden, Code: "ROLE_FORBIDDEN", Module: r.Modules[0], Action: act,
			Message: "You don't have permission to " + act + " " + strings.ToLower(label(r.Modules[0])) + " (" + roleName + ")."}
	}
	d := Decision{Allowed: true, Module: via, Action: act}
	if a.Scope() == ScopeOwn && ScopedModules[r.Modules[0]] && (via == "" || ScopedModules[via]) {
		d.OwnScope = true
	}
	return d
}
