package bookingmail

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// ---------------------------------------------------------------------------
// Calendar subscription feeds: subscribe once in Apple Calendar, Google
// Calendar or Outlook and the bookings stay in sync. A feed address holds a
// long random token (booking_calendar_feeds); regenerating revokes the old
// one. A user feed lists the bookings assigned to that person, the
// organisation feed (admins / managers) lists everybody's.
// ---------------------------------------------------------------------------

// Feed window and refresh hint.
const (
	FeedPastDays     = 30 // bookings that started up to this many days ago
	FeedAheadMonths  = 12 // … and up to this many months ahead
	FeedCancelDays   = 30 // cancelled bookings stay (as CANCELLED) this long
	FeedRefresh      = 30 * time.Minute
	feedMaxEvents    = 5000
	feedTouchMinimum = 5 * time.Minute
)

// Errors of FeedByToken.
var (
	ErrFeedNotFound = errors.New("calendar feed not found")
	ErrFeedRevoked  = errors.New("calendar feed link was replaced")
)

func newFeedSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func feedHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func feedOwner(scope, user string) string {
	if scope == FeedOrg {
		return ""
	}
	return user
}

// ActiveFeed returns the current feed of an owner (nil when there is none).
func (n *Notifier) ActiveFeed(org, scope, user string) *FeedToken {
	var ft FeedToken
	if n.DB.Where("organization_id = ? AND scope = ? AND user_id = ? AND revoked_at IS NULL", org, scope, feedOwner(scope, user)).
		Order("created_at DESC").First(&ft).Error != nil {
		return nil
	}
	return &ft
}

// HadFeed reports whether the owner ever had a feed (so a revoked feed is
// not silently re-created).
func (n *Notifier) HadFeed(org, scope, user string) bool {
	var c int64
	n.DB.Model(&FeedToken{}).Where("organization_id = ? AND scope = ? AND user_id = ?", org, scope, feedOwner(scope, user)).Count(&c)
	return c > 0
}

// RevokeFeed turns the owner's feed links off; it returns how many were live.
func (n *Notifier) RevokeFeed(org, scope, user string) int64 {
	now := n.now()
	return n.DB.Model(&FeedToken{}).Where("organization_id = ? AND scope = ? AND user_id = ? AND revoked_at IS NULL", org, scope, feedOwner(scope, user)).
		Update("revoked_at", now).RowsAffected
}

// NewFeed replaces the owner's feed with a fresh secret address: the old
// link stops working at once.
func (n *Notifier) NewFeed(org, scope, user, by string) (*FeedToken, error) {
	if scope != FeedUser && scope != FeedOrg {
		return nil, errors.New("unknown feed scope")
	}
	if org == "" || (scope == FeedUser && user == "") {
		return nil, errors.New("feed owner missing")
	}
	tok, err := newFeedSecret()
	if err != nil {
		return nil, err
	}
	ft := FeedToken{ID: uuid.NewString(), OrganizationID: org, Scope: scope, UserID: feedOwner(scope, user), Token: tok, TokenHash: feedHash(tok), CreatedBy: by, CreatedAt: n.now()}
	err = n.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&FeedToken{}).Where("organization_id = ? AND scope = ? AND user_id = ? AND revoked_at IS NULL", org, scope, ft.UserID).
			Update("revoked_at", ft.CreatedAt).Error; err != nil {
			return err
		}
		return tx.Create(&ft).Error
	})
	if err != nil {
		return nil, err
	}
	return &ft, nil
}

// FeedByToken finds the feed of a secret address. ErrFeedRevoked: the link
// was regenerated or turned off (or its owner left); ErrFeedNotFound: there
// never was such a link.
func (n *Notifier) FeedByToken(token string) (*FeedToken, error) {
	token = strings.TrimSpace(token)
	if len(token) < 32 || len(token) > 128 {
		return nil, ErrFeedNotFound
	}
	var ft FeedToken
	if n.DB.Where("token_hash = ?", feedHash(token)).First(&ft).Error != nil {
		return nil, ErrFeedNotFound
	}
	if subtle.ConstantTimeCompare([]byte(ft.Token), []byte(token)) != 1 {
		return nil, ErrFeedNotFound
	}
	if ft.RevokedAt != nil {
		return &ft, ErrFeedRevoked
	}
	if ft.Scope == FeedUser {
		u := n.user(ft.UserID)
		if u == nil || u.OrganizationID != ft.OrganizationID || (u.IsActive != nil && !*u.IsActive) {
			return &ft, ErrFeedRevoked
		}
	}
	return &ft, nil
}

// FeedPath is the API path of a feed.
func FeedPath(token string) string { return "/api/v1/public/calendar/" + token + ".ics" }

// FeedName is the calendar name shown in calendar apps, e.g.
// "DASYIN bookings — Amina Otieno".
func (n *Notifier) FeedName(ft *FeedToken) string {
	what := "DASYIN " + strings.ToLower(n.term(ft.OrganizationID, "bookings", "Bookings"))
	who := ""
	if ft.Scope == FeedOrg {
		o, _ := n.org(ft.OrganizationID)
		who = strings.TrimSpace(o.Name)
	} else if u := n.user(ft.UserID); u != nil {
		who = strings.TrimSpace(u.FirstName + " " + u.LastName)
	}
	if who == "" {
		return what
	}
	return what + " — " + who
}

// FeedICS renders a feed: the owner's bookings from FeedPastDays ago to
// FeedAheadMonths ahead. Cancelled and deleted bookings stay as
// STATUS:CANCELLED for FeedCancelDays, then drop out. The output only changes
// when a booking does (DTSTAMP is the booking's last change), so its ETag is
// stable between changes.
func (n *Notifier) FeedICS(ft *FeedToken) string {
	org := ft.OrganizationID
	now := n.now()
	from, to := now.AddDate(0, 0, -FeedPastDays), now.AddDate(0, FeedAheadMonths, 0)
	q := n.DB.Unscoped().Preload("Customer").Preload("Vehicle").Preload("Services").
		Where("organization_id = ? AND start_time >= ? AND start_time < ?", org, from, to)
	if ft.Scope == FeedUser {
		q = q.Where("staff_id = ?", ft.UserID)
	}
	var bookings []models.Booking
	q.Order("start_time ASC, id ASC").Limit(feedMaxEvents).Find(&bookings)

	names := map[string]string{}
	if ft.Scope == FeedOrg {
		var users []staffUser
		n.DB.Session(&gorm.Session{NewDB: true}).Table("users").Select("id, first_name, last_name").Where("organization_id = ?", org).Find(&users)
		for _, u := range users {
			names[u.ID] = strings.TrimSpace(u.FirstName + " " + u.LastName)
		}
	}
	ids := make([]string, 0, len(bookings))
	for _, b := range bookings {
		ids = append(ids, b.ID)
	}
	seqs := n.sequences(ids)
	oc := n.orgCtx(org)
	cutoff := now.AddDate(0, 0, -FeedCancelDays)
	events := make([]ICSInput, 0, len(bookings))
	for i := range bookings {
		b := &bookings[i]
		changed := b.UpdatedAt
		if b.DeletedAt.Valid && b.DeletedAt.Time.After(changed) {
			changed = b.DeletedAt.Time
		}
		cancelled := gone(b)
		if cancelled && changed.Before(cutoff) {
			continue
		}
		ev := n.Event(b, seqs[b.ID], oc)
		ev.Cancelled, ev.Stamp = cancelled, changed
		if ev.Stamp.IsZero() {
			ev.Stamp = b.CreatedAt
		}
		if b.UpdatedAt.Before(changed) {
			// LAST-MODIFIED follows a deletion too.
			cp := *b
			cp.UpdatedAt = changed
			ev.Booking = &cp
		}
		switch {
		case cancelled:
			ev.Summary = "Cancelled: " + ev.Summary
		case b.Status == "no_show":
			ev.Summary = "No-show: " + ev.Summary
		}
		if ft.Scope == FeedOrg {
			who := "Unassigned"
			if id := staffOf(b); id != "" && names[id] != "" {
				who = names[id]
				ev.Description = "Staff: " + who + "\n" + ev.Description
			}
			ev.Summary = who + ": " + ev.Summary
		}
		// Reminders only make sense for bookings still to come.
		if !live(b.Status) || !b.StartTime.After(now) {
			ev.Reminder = 0
		}
		events = append(events, ev)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Booking.StartTime.Before(events[j].Booking.StartTime) })
	words := n.term(org, "bookings", "Bookings")
	desc := words + " assigned to you in DASYIN ERP. Read-only — change them in the app."
	if ft.Scope == FeedOrg {
		desc = words + " of all staff in DASYIN ERP. Read-only — change them in the app."
	}
	return BuildFeed(FeedInput{Name: n.FeedName(ft), Description: desc, Events: events, Refresh: FeedRefresh})
}

// FeedETag is the strong validator of a feed body.
func FeedETag(body string) string {
	h := sha256.Sum256([]byte(body))
	return `"` + hex.EncodeToString(h[:16]) + `"`
}

// MarkServed records what a feed returned: when its content last changed
// (the Last-Modified time) and, at most every few minutes, that a calendar
// app fetched it.
func (n *Notifier) MarkServed(ft *FeedToken, etag string) time.Time {
	now := n.now().UTC().Truncate(time.Second)
	upd := map[string]any{}
	changed := now
	if ft.ETag == etag && ft.ChangedAt != nil {
		changed = ft.ChangedAt.UTC().Truncate(time.Second)
	} else {
		upd["etag"], upd["changed_at"] = etag, now
		ft.ETag, ft.ChangedAt = etag, &now
	}
	if ft.LastUsedAt == nil || now.Sub(*ft.LastUsedAt) >= feedTouchMinimum {
		upd["last_used_at"] = now
		ft.LastUsedAt = &now
	}
	if len(upd) > 0 {
		n.DB.Model(&FeedToken{}).Where("id = ?", ft.ID).Updates(upd)
	}
	return changed
}
