package projects2

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Errors of SyncOrg.
var (
	ErrNotConnected = errors.New("Jira is not connected")
	ErrSyncRunning  = errors.New("a Jira sync is already running")
	ErrNoProjects   = errors.New("no Jira projects are chosen to sync")
)

// SyncOrg runs the Jira sync of an organisation outside a request — what
// POST /pm/jira/sync does with the saved project selection — so other
// modules (the morning brief) can keep Jira figures fresh. It records the
// outcome on the connection like a manual sync.
func (h *Handler) SyncOrg(ctx context.Context, org string) (SyncResult, error) {
	conn, err := h.loadConnection(org)
	if err != nil {
		return SyncResult{}, err
	}
	if conn == nil {
		return SyncResult{}, ErrNotConnected
	}
	keys := splitKeys(conn.ProjectKeys)
	if len(keys) == 0 {
		return SyncResult{}, ErrNoProjects
	}
	site, err := h.validateSite(conn.SiteURL)
	if err != nil {
		return SyncResult{}, err
	}
	token, err := h.decryptToken(conn)
	if err != nil {
		return SyncResult{}, errors.New("the saved Jira API token could not be read — enter it again in Projects → Jira")
	}
	lock := h.orgLock(org)
	if !lock.TryLock() {
		return SyncResult{}, ErrSyncRunning
	}
	defer lock.Unlock()
	result, fatal := h.runSync(ctx, org, h.client(site, conn.Email, token), conn.DefaultJQL, keys)
	status, msg := "success", ""
	switch {
	case fatal != nil:
		status, msg = "error", fatal.Error()
	case result.Errors > 0 && result.Errors == len(result.Projects):
		status, msg = "error", firstError(result)
	case result.Errors > 0:
		status, msg = "partial", firstError(result)
	}
	statsJSON, _ := json.Marshal(result)
	h.DB.Model(&JiraConnection{}).Where("org_id = ?", org).Updates(map[string]interface{}{
		"last_sync_at": h.now().UTC(), "last_sync_status": status, "last_sync_message": msg, "last_sync_stats": string(statsJSON),
	})
	if fatal != nil {
		return result, fatal
	}
	if status == "error" {
		return result, errors.New(strings.TrimSpace("Jira sync failed: " + msg))
	}
	return result, nil
}
