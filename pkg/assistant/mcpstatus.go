package assistant

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// MCPStatus says whether Claude (or another MCP client) is connected to an
// organisation: an approved one-click connector (OAuth grant) or an active
// personal access token. It is read straight from the pkg/mcp tables,
// read-only, so people can see that "Claude ↔ DASYIN" already works even while
// the in-app assistant has no API key.
type MCPStatus struct {
	Connected  bool       `json:"connected"`    // anyone in the organisation
	Mine       bool       `json:"mine"`         // the signed-in user
	Apps       int64      `json:"apps"`         // approved connectors in the organisation
	Tokens     int64      `json:"tokens"`       // active access tokens in the organisation
	LastUsedAt *time.Time `json:"last_used_at"` // most recent use of any of them
}

// MCPStatusFor counts the organisation's active MCP connections.
func MCPStatusFor(db *gorm.DB, orgID, userID string, now time.Time) MCPStatus {
	var st MCPStatus
	if db == nil || orgID == "" {
		return st
	}
	// Quiet: the MCP tables may not exist (tests, a database that is still
	// being migrated) — that simply means "not connected".
	q := func() *gorm.DB {
		return db.Session(&gorm.Session{NewDB: true, Logger: logger.Default.LogMode(logger.Silent)})
	}
	type agg struct {
		N    int64
		Mine int64
	}
	var tok, app agg
	q().Table("mcp_tokens").
		Select("COUNT(*) AS n, COALESCE(SUM(CASE WHEN user_id = ? THEN 1 ELSE 0 END), 0) AS mine", userID).
		Where("org_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)", orgID, now).
		Scan(&tok)
	q().Table("mcp_oauth_grants").
		Select("COUNT(*) AS n, COALESCE(SUM(CASE WHEN user_id = ? THEN 1 ELSE 0 END), 0) AS mine", userID).
		Where("org_id = ? AND revoked_at IS NULL", orgID).
		Scan(&app)
	st.Tokens, st.Apps = tok.N, app.N
	st.Connected = tok.N+app.N > 0
	st.Mine = tok.Mine+app.Mine > 0
	if st.Connected {
		var a, b []time.Time
		q().Table("mcp_tokens").Where("org_id = ? AND revoked_at IS NULL AND last_used_at IS NOT NULL", orgID).Order("last_used_at DESC").Limit(1).Pluck("last_used_at", &a)
		q().Table("mcp_oauth_grants").Where("org_id = ? AND revoked_at IS NULL AND last_used_at IS NOT NULL", orgID).Order("last_used_at DESC").Limit(1).Pluck("last_used_at", &b)
		for _, t := range append(a, b...) {
			t := t
			if st.LastUsedAt == nil || t.After(*st.LastUsedAt) {
				st.LastUsedAt = &t
			}
		}
	}
	return st
}
