package outbox

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Audit writes an audit_logs row (best effort: older databases may lack it).
func Audit(db *gorm.DB, org, user, action, entityType, entityID string, old, values any, now time.Time) {
	db = db.Session(&gorm.Session{NewDB: true})
	if !db.Migrator().HasTable("audit_logs") {
		return
	}
	enc := func(v any) string {
		if v == nil {
			return "{}"
		}
		b, err := json.Marshal(v)
		if err != nil || string(b) == "null" {
			return "{}"
		}
		return string(b)
	}
	var uid *string
	if user != "" {
		uid = &user
	}
	if len(entityID) > 36 {
		entityID = entityID[:36]
	}
	row := map[string]any{"id": uuid.NewString(), "organization_id": org, "user_id": uid, "action": action, "entity_type": entityType,
		"entity_id": entityID, "old_values": enc(old), "new_values": enc(values), "ip_address": "", "user_agent": "", "created_at": now}
	_ = db.Table("audit_logs").Create(row).Error
}

// SentNow records that an approver sent a client message at once ("Send
// now — I've reviewed it"): it never became an item, so it is audited here.
func SentNow(org, user, entityType, entityID, what, to string) {
	s := Default()
	if s == nil {
		return
	}
	Audit(s.db(), org, user, "outbox.send_now", entityType, entityID, nil, map[string]any{"what": what, "to": to, "by": s.UserName(user)}, s.now())
}
