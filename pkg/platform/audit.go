package platform

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type auditEntry struct {
	ID               string    `json:"id"`
	Source           string    `json:"source"`
	OrganizationID   string    `json:"organization_id"`
	OrganizationName string    `json:"organization_name"`
	UserID           string    `json:"user_id"`
	UserName         string    `json:"user_name"`
	UserEmail        string    `json:"user_email"`
	Action           string    `json:"action"`
	EntityType       string    `json:"entity_type"`
	EntityID         string    `json:"entity_id"`
	Summary          string    `json:"summary"`
	OldValues        string    `json:"old_values"`
	NewValues        string    `json:"new_values"`
	IPAddress        string    `json:"ip_address"`
	CreatedAt        time.Time `json:"created_at"`
}

// AuditFeed merges the platform audit log, the accounting audit trail and the
// invoicing activity history into one filterable, paginated feed.
func (h *Handler) AuditFeed(c *gin.Context) {
	page, limit, offset := pageParams(c, 50, 200)
	parts := []string{}
	source := c.Query("source")
	want := func(s string) bool { return source == "" || source == s }

	if want("platform") && hasTable(h.DB, "audit_logs") {
		parts = append(parts, `SELECT a.id::text AS id, 'platform' AS source, a.organization_id::text AS organization_id,
			COALESCE(a.user_id,'') AS user_id, a.action, a.entity_type, a.entity_id,
			'' AS summary, COALESCE(a.old_values::text,'') AS old_values, COALESCE(a.new_values::text,'') AS new_values,
			COALESCE(a.ip_address,'') AS ip_address, a.created_at FROM audit_logs a`)
	}
	if want("accounting") && hasTable(h.DB, "audit_trails") {
		parts = append(parts, `SELECT t.id::text AS id, 'accounting' AS source, t.organization_id::text AS organization_id, COALESCE(t.user_id,'') AS user_id,
			'ledger.' || LOWER(t.action) AS action, t.table_name AS entity_type, t.record_id AS entity_id,
			COALESCE(t.field_name,'') AS summary, COALESCE(t.old_value,'') AS old_values, COALESCE(t.new_value,'') AS new_values,
			COALESCE(t.ip_address,'') AS ip_address, t.timestamp AS created_at FROM audit_trails t`)
	}
	if want("invoicing") && hasTable(h.DB, "billing_activities") {
		parts = append(parts, `SELECT b.id::text AS id, 'invoicing' AS source, COALESCE(b.organization_id,'') AS organization_id, COALESCE(b.user_id,'') AS user_id,
			'invoice.' || COALESCE(NULLIF(b.type,''),'activity') AS action, 'invoice' AS entity_type, b.invoice_id AS entity_id,
			COALESCE(b.message,'') AS summary, '' AS old_values, '' AS new_values, '' AS ip_address, b.created_at AS created_at FROM billing_activities b`)
	}
	if len(parts) == 0 {
		ok(c, http.StatusOK, gin.H{"entries": []auditEntry{}, "total": 0, "page": page, "limit": limit})
		return
	}
	union := "(" + strings.Join(parts, " UNION ALL ") + ") e"

	where := []string{"1=1"}
	args := []interface{}{}
	if org := c.Query("organization_id"); org != "" {
		where = append(where, "e.organization_id = ?")
		args = append(args, org)
	}
	if act := strings.TrimSpace(c.Query("action")); act != "" {
		where = append(where, "e.action LIKE ?")
		args = append(args, strings.ToLower(act)+"%")
	}
	if from := c.Query("from"); from != "" {
		if t, err := time.Parse("2006-01-02", from); err == nil {
			where = append(where, "e.created_at >= ?")
			args = append(args, t)
		}
	}
	if to := c.Query("to"); to != "" {
		if t, err := time.Parse("2006-01-02", to); err == nil {
			where = append(where, "e.created_at < ?")
			args = append(args, t.AddDate(0, 0, 1))
		}
	}
	if s := strings.TrimSpace(c.Query("search")); s != "" {
		p := like(s)
		where = append(where, `(LOWER(e.action) LIKE ? OR LOWER(e.entity_type) LIKE ? OR LOWER(e.summary) LIKE ? OR e.entity_id = ?
			OR LOWER(COALESCE(o.name,'')) LIKE ? OR LOWER(COALESCE(u.email,'')) LIKE ? OR LOWER(COALESCE(u.first_name || ' ' || u.last_name,'')) LIKE ?)`)
		args = append(args, p, p, p, strings.TrimSpace(s), p, p, p)
	}
	from := union + ` LEFT JOIN organizations o ON o.id = e.organization_id LEFT JOIN users u ON u.id = e.user_id`
	cond := strings.Join(where, " AND ")

	var total int64
	if err := h.DB.Raw("SELECT COUNT(*) FROM "+from+" WHERE "+cond, args...).Scan(&total).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load audit log")
		return
	}
	rows := []auditEntry{}
	q := `SELECT e.*, COALESCE(o.name,'') AS organization_name, COALESCE(TRIM(u.first_name || ' ' || u.last_name),'') AS user_name,
		COALESCE(u.email,'') AS user_email FROM ` + from + ` WHERE ` + cond + ` ORDER BY e.created_at DESC LIMIT ? OFFSET ?`
	if err := h.DB.Raw(q, append(args, limit, offset)...).Scan(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load audit log")
		return
	}

	counts := gin.H{}
	for _, s := range []struct{ key, table string }{{"platform", "audit_logs"}, {"accounting", "audit_trails"}, {"invoicing", "billing_activities"}} {
		var n int64
		if hasTable(h.DB, s.table) {
			h.DB.Table(s.table).Count(&n)
		}
		counts[s.key] = n
	}
	ok(c, http.StatusOK, gin.H{"entries": rows, "total": total, "page": page, "limit": limit, "source_counts": counts})
}
