package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
)

// SynergyActions suspends and unsuspends hosting at Synergy Wholesale
// through its reseller API (SOAP, pkg/expenses' client):
//
//	listHosting              finds the hosting service of a site (hoid by domain)
//	hostingSuspendService    {identifier: hoid, reason}  suspends it
//	hostingUnsuspendService  {identifier: hoid}          unsuspends it
//
// (Synergy Wholesale API documentation v3.16, Hosting Services; the same
// calls as Synergy's own WHMCS hosting module — see
// pkg/expenses/synergy_hosting.go for the sources.)
//
// It handles a hosted site when the organisation has the API connected
// (Expenses → Sources), the subscription's provider is Synergy Wholesale (or
// not set) and the site is one of the account's hosting services; anything
// else is a task for an administrator.
type SynergyActions struct {
	// Client returns the organisation's API client (nil + why when not connected).
	Client func(org string) (*expenses.SynergyClient, string)

	mu    sync.Mutex
	cache map[string]hostingCache
}

type hostingCache struct {
	at     time.Time
	byName map[string]expenses.SynergyHosting
	err    error
}

// NewSynergyActions uses the expenses service's stored connection.
func NewSynergyActions(exp *expenses.Service) *SynergyActions {
	return &SynergyActions{Client: func(org string) (*expenses.SynergyClient, string) {
		if exp == nil {
			return nil, "the Synergy Wholesale API is not available"
		}
		return exp.SynergyAPI(org)
	}}
}

func (a *SynergyActions) hosting(ctx context.Context, org string, cl *expenses.SynergyClient) (map[string]expenses.SynergyHosting, error) {
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[string]hostingCache{}
	}
	c, ok := a.cache[org]
	a.mu.Unlock()
	if ok && time.Since(c.at) < 5*time.Minute {
		return c.byName, c.err
	}
	list, err := cl.Hosting(ctx)
	m := map[string]expenses.SynergyHosting{}
	for _, h := range list {
		if d := strings.ToLower(strings.TrimSpace(h.Domain)); d != "" {
			if ex, dup := m[d]; dup && isGone(h.Status) && !isGone(ex.Status) {
				continue // keep the live service over a cancelled one on the same domain
			}
			m[d] = h
		}
	}
	a.mu.Lock()
	a.cache[org] = hostingCache{at: time.Now(), byName: m, err: err}
	a.mu.Unlock()
	return m, err
}

// Forget drops the cached hosting list of an organisation.
func (a *SynergyActions) Forget(org string) {
	a.mu.Lock()
	delete(a.cache, org)
	a.mu.Unlock()
}

func isGone(status string) bool {
	st := strings.ToLower(status)
	return strings.Contains(st, "cancel") || strings.Contains(st, "terminat") || strings.Contains(st, "deleted") || strings.Contains(st, "expired")
}

// Handles finds the site's hosting service at Synergy Wholesale.
func (a *SynergyActions) Handles(ctx context.Context, org string, t Target) (string, bool, string) {
	if t.Kind != KindHosting || t.Domain == "" {
		return "", false, ""
	}
	if t.Supplier != "" && t.Supplier != SupplierSynergy {
		return "", false, "" // another provider: suspended by hand
	}
	if a.Client == nil {
		return "", false, ""
	}
	cl, why := a.Client(org)
	if cl == nil {
		if t.Supplier == SupplierSynergy {
			return "", false, "Suspend it by hand: " + why + "."
		}
		return "", false, ""
	}
	if t.Ref != "" {
		return t.Ref, true, ""
	}
	m, err := a.hosting(ctx, org, cl)
	if err != nil {
		if t.Supplier == SupplierSynergy {
			return "", false, "Couldn't reach the Synergy Wholesale API (" + err.Error() + ") — do it by hand."
		}
		return "", false, ""
	}
	h, ok := m[strings.ToLower(t.Domain)]
	if !ok {
		if t.Supplier == SupplierSynergy {
			return "", false, t.Domain + " is not one of the hosting services the Synergy Wholesale API lists for your account — check where it is hosted."
		}
		return "", false, ""
	}
	if isGone(h.Status) {
		return "", false, fmt.Sprintf("Synergy Wholesale lists %s as %s.", t.Domain, h.Status)
	}
	if h.ID == "" || strings.Contains(h.ID, "|") {
		return "", false, "Synergy Wholesale returned no hosting ID for " + t.Domain + " — do it by hand."
	}
	return h.ID, true, ""
}

func (a *SynergyActions) client(org string) (*expenses.SynergyClient, error) {
	if a.Client == nil {
		return nil, errors.New("the Synergy Wholesale API is not available")
	}
	cl, why := a.Client(org)
	if cl == nil {
		return nil, errors.New(why)
	}
	return cl, nil
}

// Suspend calls hostingSuspendService.
func (a *SynergyActions) Suspend(ctx context.Context, org string, t Target) (string, error) {
	cl, err := a.client(org)
	if err != nil {
		return "", err
	}
	ref, ok, why := a.Handles(ctx, org, t)
	if !ok {
		return "", errors.New(firstNonEmpty(why, t.Domain+" is not hosted at Synergy Wholesale"))
	}
	reason := firstNonEmpty(t.Reason, "non-payment")
	if t.Number != "" {
		reason += " (" + t.Number + ")"
	}
	if err := cl.SuspendHosting(ctx, ref, "DASYIN: "+reason); err != nil {
		return "", err
	}
	a.Forget(org)
	return fmt.Sprintf("Suspended %s at Synergy Wholesale through the API (hostingSuspendService, hosting ID %s).", t.Domain, ref), nil
}

// Unsuspend calls hostingUnsuspendService.
func (a *SynergyActions) Unsuspend(ctx context.Context, org string, t Target) (string, error) {
	cl, err := a.client(org)
	if err != nil {
		return "", err
	}
	ref := t.Ref
	if ref == "" {
		r, ok, why := a.Handles(ctx, org, t)
		if !ok {
			return "", errors.New(firstNonEmpty(why, t.Domain+" is not hosted at Synergy Wholesale"))
		}
		ref = r
	}
	if err := cl.UnsuspendHosting(ctx, ref); err != nil {
		return "", err
	}
	a.Forget(org)
	return fmt.Sprintf("Unsuspended %s at Synergy Wholesale through the API (hostingUnsuspendService, hosting ID %s).", t.Domain, ref), nil
}
