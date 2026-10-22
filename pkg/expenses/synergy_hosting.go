package expenses

import (
	"context"
	"strings"
)

// Synergy Wholesale hosting service commands used by client subscriptions
// (pkg/subscriptions) to suspend and unsuspend a client's hosting when an
// invoice is not paid. Same SOAP client and endpoint as the other commands
// (see synergy.go); the request carries the hosting service's identifier —
// its hosting ID (hoid, from listHosting) or the hosting account's email —
// and the response a status ("OK") or errorMessage:
//
//	hostingSuspendService   {resellerID, apiKey, identifier, reason}  → status, errorMessage
//	hostingUnsuspendService {resellerID, apiKey, identifier}          → status, errorMessage
//	hostingGetService       {resellerID, apiKey, identifier}          → status, domain, plan, …
//
// Sources: Synergy Wholesale API documentation v3.16 (Hosting Services:
// "Suspend Hosting Service", "Unsuspend Hosting Service", "Hosting Service
// Query", "List Hosting Services"),
// https://synergywholesale.com/wp-content/uploads/2026/03/Synergy-Wholesale-API-Documentation-v3-16.pdf,
// and Synergy Wholesale's own WHMCS hosting module, which calls exactly these
// commands with 'identifier' => hoid (or the account email) and
// 'reason' => … for suspension and checks status == "OK"
// (https://github.com/SynergyWholesale/WHMCS-Hosting-Module, modules/servers/synergywholesale_hosting/synergywholesale_hosting.php:
// SuspendAccount → hostingSuspendService, UnsuspendAccount → hostingUnsuspendService).

// SuspendHosting suspends a hosting service (identifier: its hoid).
func (c *SynergyClient) SuspendHosting(ctx context.Context, identifier, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Suspended for non-payment"
	}
	if len(reason) > 200 {
		reason = reason[:200]
	}
	_, err := c.Call(ctx, "hostingSuspendService", [2]string{"identifier", identifier}, [2]string{"reason", reason})
	return err
}

// UnsuspendHosting unsuspends a hosting service (identifier: its hoid).
func (c *SynergyClient) UnsuspendHosting(ctx context.Context, identifier string) error {
	_, err := c.Call(ctx, "hostingUnsuspendService", [2]string{"identifier", identifier})
	return err
}

// SynergyAPI is the organisation's Synergy Wholesale API client when the
// API is connected and switched on (Expenses → Sources → Synergy Wholesale);
// nil with the reason in words otherwise.
func (s *Service) SynergyAPI(org string) (*SynergyClient, string) {
	c := s.connection(org, "synergy")
	if c == nil || c.Account == "" || c.SecretEnc == "" {
		return nil, "the Synergy Wholesale API is not connected (Expenses → Sources)"
	}
	if !c.Enabled {
		return nil, "the Synergy Wholesale connection is switched off (Expenses → Sources)"
	}
	key := dec(c.SecretEnc)
	if key == "" {
		return nil, "the stored Synergy Wholesale API key could not be read — enter it again in Expenses → Sources"
	}
	return NewSynergyClient(c.Account, key), ""
}
