// Package notify stores in-app notifications (the bell in the top bar).
//
// Any package can call notify.Send to tell one or more users about an event:
//
//	notify.Send(db, org, notify.Admins().Except(actor), notify.Message{
//	    Type: "booking.created", Severity: notify.Info,
//	    Title: "New booking", Body: "Jane Doe · Tue 14:00",
//	    Link: "/bookings?booking=" + id, EntityType: "booking", EntityID: id,
//	})
//
// Send never fails the caller's request: errors are logged and returned for
// callers that care. Recipients are resolved from the users table of the
// organisation (active users only).
package notify

import (
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Severity levels (drive the icon/colour in the UI).
const (
	Info    = "info"
	Success = "success"
	Warning = "warning"
	Error   = "error"
)

// Notification is one message for one user.
type Notification struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);not null;index:idx_notif_user,priority:1"`
	UserID         string     `json:"user_id" gorm:"type:varchar(255);not null;index:idx_notif_user,priority:2"`
	Type           string     `json:"type" gorm:"type:varchar(64);not null;index"`
	Severity       string     `json:"severity" gorm:"type:varchar(16);not null;default:info"`
	Title          string     `json:"title" gorm:"type:varchar(255);not null"`
	Body           string     `json:"body" gorm:"type:text"`
	Link           string     `json:"link" gorm:"type:varchar(512)"`
	EntityType     string     `json:"entity_type" gorm:"type:varchar(64);index:idx_notif_entity,priority:1"`
	EntityID       string     `json:"entity_id" gorm:"type:varchar(255);index:idx_notif_entity,priority:2"`
	ActorID        string     `json:"actor_id,omitempty" gorm:"type:varchar(255)"`
	ActorName      string     `json:"actor_name,omitempty" gorm:"type:varchar(255)"`
	ReadAt         *time.Time `json:"read_at" gorm:"index"`
	CreatedAt      time.Time  `json:"created_at" gorm:"index:idx_notif_user,priority:3"`
}

// TableName keeps the table name stable.
func (Notification) TableName() string { return "notifications" }

// Migrate creates the notifications table.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&Notification{}) }

// Message is what callers send; it is copied to every recipient.
type Message struct {
	Type       string // dotted event type, e.g. "booking.created"
	Severity   string // info | success | warning | error
	Title      string
	Body       string
	Link       string // in-app route, e.g. "/bookings"
	EntityType string
	EntityID   string
	ActorID    string // user who caused it (for display)
	ActorName  string
	// Dedupe: when set, skip recipients who already have an UNREAD
	// notification of the same Type for the same entity (e.g. low stock).
	Dedupe bool
}

// Target selects recipients inside an organisation.
type Target struct {
	UserIDs  []string // explicit users
	Roles    []string // everyone with one of these roles
	All      bool     // every active user in the organisation
	Excluded []string // never these users (usually the actor)
}

// AdminRoles are the roles that manage an organisation.
var AdminRoles = []string{"owner", "admin", "org_admin", "super_admin", "manager"}

// Users targets specific users.
func Users(ids ...string) Target { return Target{UserIDs: ids} }

// Roles targets users with any of the roles.
func Roles(roles ...string) Target { return Target{Roles: roles} }

// Admins targets the organisation's admins and managers.
func Admins() Target { return Target{Roles: AdminRoles} }

// Everyone targets every active user of the organisation.
func Everyone() Target { return Target{All: true} }

// Plus adds explicit users to a target.
func (t Target) Plus(ids ...string) Target {
	t.UserIDs = append(append([]string{}, t.UserIDs...), ids...)
	return t
}

// Except removes users (empty ids are ignored).
func (t Target) Except(ids ...string) Target {
	for _, id := range ids {
		if id != "" {
			t.Excluded = append(append([]string{}, t.Excluded...), id)
		}
	}
	return t
}

// Now is swappable in tests.
var Now = func() time.Time { return time.Now().UTC() }

// Disabled turns Send into a no-op (tests).
var Disabled bool

// Recipients resolves a target to user ids (active users of the org).
func Recipients(db *gorm.DB, org string, t Target) ([]string, error) {
	if org == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, id := range t.Excluded {
		seen[id] = true
	}
	var out []string
	add := func(ids []string) {
		for _, id := range ids {
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	q := func() *gorm.DB {
		return db.Session(&gorm.Session{NewDB: true}).Table("users").
			Where("organization_id = ? AND deleted_at IS NULL AND (is_active IS NULL OR is_active = ?)", org, true)
	}
	if len(t.UserIDs) > 0 {
		var ids []string
		if err := q().Where("id IN ?", t.UserIDs).Pluck("id", &ids).Error; err != nil {
			return nil, err
		}
		add(ids)
	}
	if t.All {
		var ids []string
		if err := q().Order("created_at").Limit(500).Pluck("id", &ids).Error; err != nil {
			return nil, err
		}
		add(ids)
	} else if len(t.Roles) > 0 {
		var ids []string
		if err := q().Where("role IN ?", t.Roles).Order("created_at").Limit(500).Pluck("id", &ids).Error; err != nil {
			return nil, err
		}
		add(ids)
	}
	return out, nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func validSeverity(s string) string {
	switch s {
	case Info, Success, Warning, Error:
		return s
	case "danger":
		return Error
	}
	return Info
}

// Send stores the message for every recipient of the target. It returns the
// number of notifications written.
func Send(db *gorm.DB, org string, to Target, m Message) (int, error) {
	if Disabled || db == nil || org == "" || strings.TrimSpace(m.Title) == "" {
		return 0, nil
	}
	db = db.Session(&gorm.Session{NewDB: true})
	users, err := Recipients(db, org, to)
	if err != nil {
		log.Printf("notify: recipients for %s: %v", m.Type, err)
		return 0, err
	}
	if len(users) == 0 {
		return 0, nil
	}
	if m.Dedupe && m.EntityID != "" {
		var have []string
		db.Model(&Notification{}).Where("organization_id = ? AND type = ? AND entity_id = ? AND read_at IS NULL AND user_id IN ?", org, m.Type, m.EntityID, users).Pluck("user_id", &have)
		if len(have) > 0 {
			skip := map[string]bool{}
			for _, u := range have {
				skip[u] = true
			}
			kept := users[:0]
			for _, u := range users {
				if !skip[u] {
					kept = append(kept, u)
				}
			}
			users = kept
			if len(users) == 0 {
				return 0, nil
			}
		}
	}
	now := Now()
	rows := make([]Notification, 0, len(users))
	for _, u := range users {
		rows = append(rows, Notification{
			ID: uuid.NewString(), OrganizationID: org, UserID: u, Type: clip(m.Type, 64), Severity: validSeverity(m.Severity),
			Title: clip(m.Title, 250), Body: clip(m.Body, 2000), Link: clip(m.Link, 500),
			EntityType: clip(m.EntityType, 64), EntityID: clip(m.EntityID, 250), ActorID: m.ActorID, ActorName: clip(m.ActorName, 250),
			CreatedAt: now,
		})
	}
	if err := db.Create(&rows).Error; err != nil {
		log.Printf("notify: save %s: %v", m.Type, err)
		return 0, err
	}
	return len(rows), nil
}

// Go sends in the background (never blocks or fails a request).
func Go(db *gorm.DB, org string, to Target, m Message) {
	if Disabled || db == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("notify: panic sending %s: %v", m.Type, r)
			}
		}()
		_, _ = Send(db, org, to, m)
	}()
}

// UserName returns "First Last" for a user id ("" when unknown).
func UserName(db *gorm.DB, id string) string {
	if id == "" || db == nil {
		return ""
	}
	var r struct{ FirstName, LastName, Email string }
	if err := db.Session(&gorm.Session{NewDB: true}).Table("users").Select("first_name, last_name, email").Where("id = ?", id).Take(&r).Error; err != nil {
		return ""
	}
	n := strings.TrimSpace(r.FirstName + " " + r.LastName)
	if n == "" {
		n = r.Email
	}
	return n
}
