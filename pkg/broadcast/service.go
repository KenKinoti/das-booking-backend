package broadcast

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/assistant"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/occasions"
)

// Service sends announcements, emails the unread and nudges super admins.
type Service struct {
	DB     *gorm.DB
	AppURL string // PUBLIC_APP_URL (links in emails)
	APIURL string // PUBLIC_API_URL (tracked email links)
	Secret []byte // signs the email links

	// SendMail delivers an email; nil = the platform account (pkg/mail).
	SendMail func(ctx context.Context, msg mail.Message) error
	// Occasions is the calendar; nil = occasions.Between.
	Occasions func(from, to time.Time, country string) []occasions.Occasion
	// AI resolves the Anthropic configuration; nil = assistant.Resolve.
	AI func() assistant.Config
	// TierOf gives the plan tier of an organisation without a plan row yet.
	TierOf func(org string) string
	// Platform is the sender block of the emails.
	Platform func() PlatformInfo

	runMu sync.Mutex // one run at a time in this process (the lock covers instances)
}

// New builds a service.
func New(db *gorm.DB) *Service { return &Service{DB: db} }

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }

func (s *Service) occasionsBetween(from, to time.Time, country string) []occasions.Occasion {
	if s.Occasions != nil {
		return s.Occasions(from, to, country)
	}
	return occasions.Between(from, to, country)
}

func (s *Service) aiConfig() assistant.Config {
	if s.AI != nil {
		return s.AI()
	}
	return assistant.Resolve(s.DB)
}

// ---------------------------------------------------------------- settings

// SettingsID is the id of the settings row.
const SettingsID = "platform"

// DefaultLeadDays: nudge 14 and 3 days before.
var DefaultLeadDays = []int{14, 3}

// GetSettings returns the settings (defaults when none are saved).
func (s *Service) GetSettings() Settings {
	var rows []Settings
	fresh(s.DB).Where("id = ?", SettingsID).Limit(1).Find(&rows)
	st := Settings{ID: SettingsID, NudgeLeadDays: DefaultLeadDays, NudgeEmail: true, NudgesOn: true, SendHour: 8, Timezone: DefaultTimezone}
	if len(rows) == 1 {
		st = rows[0]
	}
	st.NudgeLeadDays = cleanLeads(st.NudgeLeadDays)
	if st.SendHour < 0 || st.SendHour > 23 {
		st.SendHour = 8
	}
	if _, err := LoadZone(st.Timezone); err != nil || st.Timezone == "" {
		st.Timezone = DefaultTimezone
	}
	return st
}

func cleanLeads(in []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, d := range in {
		if d >= 0 && d <= 90 && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return append([]int{}, DefaultLeadDays...)
	}
	// descending: the furthest nudge first
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] > out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// SaveSettings stores the settings.
func (s *Service) SaveSettings(st Settings, by string) (Settings, error) {
	st.ID, st.UpdatedBy, st.UpdatedAt = SettingsID, by, Now()
	st.NudgeLeadDays = cleanLeads(st.NudgeLeadDays)
	if st.SendHour < 0 || st.SendHour > 23 {
		return st, errors.New("the send hour must be between 0 and 23")
	}
	if _, err := LoadZone(st.Timezone); err != nil {
		return st, err
	}
	err := fresh(s.DB).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, UpdateAll: true}).Create(&st).Error
	return s.GetSettings(), err
}

// ---------------------------------------------------------------- validating

// Input is what the composer sends.
type Input struct {
	Title             string   `json:"title"`
	Body              string   `json:"body"`
	Kind              string   `json:"kind"`
	Priority          string   `json:"priority"`
	CTALabel          string   `json:"cta_label"`
	CTAURL            string   `json:"cta_url"`
	ImageURL          string   `json:"image_url"`
	EmailDelayMinutes *int     `json:"email_delay_minutes"`
	Audience          Audience `json:"audience"`
	Timezone          string   `json:"timezone"`
	LocalSendAt       string   `json:"local_send_at"` // "" = send now
	Recurrence        string   `json:"recurrence"`
	RepeatUntil       string   `json:"repeat_until"`
	LocalExpiresAt    string   `json:"local_expires_at"`
	OccasionKey       string   `json:"occasion_key"`
	OccasionDate      string   `json:"occasion_date"`
	OccasionName      string   `json:"occasion_name"`
	AIDraft           bool     `json:"ai_draft"`
}

// FieldError is a validation problem with the field it belongs to.
type FieldError struct {
	Field, Msg string
}

func (e *FieldError) Error() string { return e.Msg }

func ferr(field, msg string) error { return &FieldError{Field: field, Msg: msg} }

// DefaultDelay: urgent → 1 hour, everything else → 3 hours.
func DefaultDelay(priority string) int {
	if priority == PriorityUrgent {
		return 60
	}
	return 180
}

func validDelay(d int) bool {
	for _, x := range DelayChoices {
		if d == x {
			return true
		}
	}
	return false
}

// Apply validates in and copies it onto a (a draft or a scheduled one).
func Apply(a *Announcement, in Input) error {
	a.Title = strings.TrimSpace(in.Title)
	if a.Title == "" {
		return ferr("title", "Give the announcement a title")
	}
	if len([]rune(a.Title)) > 140 {
		return ferr("title", "Keep the title under 140 characters")
	}
	a.Body, a.BodyText = Sanitize(in.Body)
	if strings.TrimSpace(a.BodyText) == "" {
		return ferr("body", "Write the message")
	}
	if len([]rune(a.BodyText)) > 5000 {
		return ferr("body", "Keep the message under 5,000 characters")
	}
	switch in.Kind {
	case KindUpdate, KindReminder, KindPromotion, KindUrgent:
		a.Kind = in.Kind
	case "":
		a.Kind = KindUpdate
	default:
		return ferr("kind", "Choose update, reminder, offer or urgent")
	}
	switch in.Priority {
	case PriorityUrgent, PriorityNormal:
		a.Priority = in.Priority
	default:
		a.Priority = PriorityNormal
	}
	if a.Kind == KindUrgent {
		a.Priority = PriorityUrgent
	}
	a.CTALabel, a.CTAURL = strings.TrimSpace(in.CTALabel), strings.TrimSpace(in.CTAURL)
	if a.CTALabel != "" || a.CTAURL != "" {
		if a.CTALabel == "" {
			return ferr("cta_label", "Give the button a label")
		}
		if len([]rune(a.CTALabel)) > 40 {
			return ferr("cta_label", "Keep the button label under 40 characters")
		}
		u, ok := SafeCTA(a.CTAURL)
		if !ok {
			return ferr("cta_url", "The button needs an in-app path like /plan or an https:// link")
		}
		a.CTAURL = u
	}
	a.ImageURL = strings.TrimSpace(in.ImageURL)
	if a.ImageURL != "" {
		u, ok := SafeImage(a.ImageURL)
		if !ok {
			return ferr("image_url", "Images must be https:// links")
		}
		a.ImageURL = u
	}
	if in.EmailDelayMinutes != nil {
		if !validDelay(*in.EmailDelayMinutes) {
			return ferr("email_delay_minutes", "Choose 0, 1, 3, 6 or 24 hours, or never")
		}
		a.EmailDelayMinutes = *in.EmailDelayMinutes
	} else {
		a.EmailDelayMinutes = DefaultDelay(a.Priority)
	}
	a.Audience = in.Audience.Normalize()
	a.Timezone = strings.TrimSpace(in.Timezone)
	if a.Timezone == "" {
		a.Timezone = DefaultTimezone
	}
	if _, err := LoadZone(a.Timezone); err != nil {
		return ferr("timezone", "Choose a time zone from the list")
	}
	a.LocalSendAt, a.SendAt = "", nil
	if t := strings.TrimSpace(in.LocalSendAt); t != "" {
		at, err := ParseLocal(t, a.Timezone)
		if err != nil {
			return ferr("local_send_at", err.Error())
		}
		a.LocalSendAt, a.SendAt = FormatLocal(at, a.Timezone), &at
	}
	switch in.Recurrence {
	case RepeatWeekly, RepeatMonthly:
		a.Recurrence = in.Recurrence
	default:
		a.Recurrence = RepeatNone
	}
	a.RepeatUntil = strings.TrimSpace(in.RepeatUntil)
	if a.Recurrence == RepeatNone {
		a.RepeatUntil = ""
	} else if a.RepeatUntil != "" {
		if _, err := time.Parse(DateLayout, a.RepeatUntil); err != nil {
			return ferr("repeat_until", "Pick the last day it repeats")
		}
		if a.SendAt != nil && FormatLocal(*a.SendAt, a.Timezone)[:10] > a.RepeatUntil {
			return ferr("repeat_until", "The end date is before the first send")
		}
	}
	a.LocalExpiresAt, a.ExpiresAt = "", nil
	if t := strings.TrimSpace(in.LocalExpiresAt); t != "" {
		at, err := ParseLocal(t, a.Timezone)
		if err != nil {
			return ferr("local_expires_at", err.Error())
		}
		start := Now()
		if a.SendAt != nil && a.SendAt.After(start) {
			start = *a.SendAt
		}
		if !at.After(start) {
			return ferr("local_expires_at", "It would expire before it is sent")
		}
		a.LocalExpiresAt, a.ExpiresAt = FormatLocal(at, a.Timezone), &at
	}
	a.OccasionKey, a.OccasionDate, a.OccasionName = clip(in.OccasionKey, 80), clip(in.OccasionDate, 10), clip(in.OccasionName, 160)
	a.AIDraft = in.AIDraft
	return nil
}

// ---------------------------------------------------------------- state changes

// ErrNotEditable: sent or cancelled announcements can't change.
var ErrNotEditable = errors.New("this announcement was already sent or cancelled — duplicate it to send again")

// Schedule moves a draft to scheduled (it is sent at SendAt, or at once
// when it has none — the scheduler or SendNow does that).
func (s *Service) Schedule(a *Announcement, by string) error {
	if a.Status != StatusDraft && a.Status != StatusScheduled {
		return ErrNotEditable
	}
	now := Now()
	if a.SendAt == nil {
		return ferr("local_send_at", "Pick when to send it, or use Send now")
	}
	if a.SendAt.Before(now.Add(-time.Minute)) {
		return ferr("local_send_at", "That time has passed — pick a later one or use Send now")
	}
	t := *a.SendAt
	a.Status, a.NextSendAt, a.UpdatedBy, a.UpdatedAt, a.LastError = StatusScheduled, &t, by, now, ""
	return fresh(s.DB).Save(a).Error
}

// Unschedule moves a scheduled announcement back to draft.
func (s *Service) Unschedule(a *Announcement, by string) error {
	if a.Status != StatusScheduled || a.Occurrences > 0 {
		return ErrNotEditable
	}
	res := fresh(s.DB).Model(&Announcement{}).Where("id = ? AND status = ?", a.ID, StatusScheduled).
		Updates(map[string]any{"status": StatusDraft, "next_send_at": nil, "updated_by": by, "updated_at": Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotEditable
	}
	a.Status, a.NextSendAt = StatusDraft, nil
	return nil
}

// Cancel stops an announcement: nothing more is sent or emailed and it no
// longer shows in the app.
func (s *Service) Cancel(a *Announcement, by string) error {
	if a.Status == StatusCancelled {
		return nil
	}
	now := Now()
	if err := fresh(s.DB).Model(&Announcement{}).Where("id = ?", a.ID).
		Updates(map[string]any{"status": StatusCancelled, "next_send_at": nil, "cancelled_at": now, "updated_by": by, "updated_at": now}).Error; err != nil {
		return err
	}
	fresh(s.DB).Model(&Delivery{}).Where("announcement_id = ? AND email_status IN ?", a.ID, []string{EmailPending, EmailSending}).
		Updates(map[string]any{"email_status": EmailSkipped, "email_note": "Announcement cancelled"})
	// Take the unread notifications away again.
	fresh(s.DB).Exec(`DELETE FROM notifications WHERE entity_type = 'announcement' AND read_at IS NULL AND entity_id IN (SELECT id FROM broadcast_deliveries WHERE announcement_id = ?)`, a.ID)
	a.Status, a.NextSendAt, a.CancelledAt = StatusCancelled, nil, &now
	return nil
}

// SendNow sends a draft or scheduled announcement at once.
func (s *Service) SendNow(ctx context.Context, a *Announcement, by string) (int, error) {
	if a.Status != StatusDraft && a.Status != StatusScheduled {
		return 0, ErrNotEditable
	}
	now := Now()
	if a.SendAt == nil || a.SendAt.After(now) || a.Occurrences == 0 {
		// The schedule starts now (a repeating one repeats from now).
		a.LocalSendAt = FormatLocal(now, a.Timezone)
		t, _ := ParseLocal(a.LocalSendAt, a.Timezone)
		a.SendAt = &t
	}
	if a.ExpiresAt != nil && !a.ExpiresAt.After(now) {
		return 0, ferr("local_expires_at", "It has already expired — change the expiry first")
	}
	a.Status, a.NextSendAt, a.UpdatedBy, a.UpdatedAt = StatusScheduled, &now, by, now
	if err := fresh(s.DB).Save(a).Error; err != nil {
		return 0, err
	}
	n, err := s.sendOne(ctx, a.ID)
	if err != nil {
		return n, err
	}
	// Emails due straight away (a 0-hour delay).
	s.Escalate(ctx, 200)
	var b Announcement
	if fresh(s.DB).First(&b, "id = ?", a.ID).Error == nil {
		*a = b
	}
	return n, nil
}

// ---------------------------------------------------------------- delivering

func severityOf(a *Announcement) string {
	switch {
	case a.Kind == KindUrgent || a.Priority == PriorityUrgent:
		return notify.Error
	case a.Kind == KindReminder:
		return notify.Warning
	case a.Kind == KindPromotion:
		return notify.Success
	}
	return notify.Info
}

// NotificationType is the in-app notification type of a kind.
func NotificationType(kind string) string { return "announcement." + kind }

// deliver writes the deliveries (and notifications) of one occurrence for
// the recipients who don't have one yet; it returns how many were written.
func (s *Service) deliver(a *Announcement, occAt time.Time, recips []Recipient, test bool) ([]Delivery, error) {
	have := map[string]bool{}
	var ids []string
	fresh(s.DB).Model(&Delivery{}).Where("announcement_id = ? AND occurrence_at = ?", a.ID, occAt).Pluck("user_id", &ids)
	for _, id := range ids {
		have[id] = true
	}
	now := Now()
	out := []Delivery{}
	var todo []Recipient
	for _, r := range recips {
		if !have[r.UserID] {
			have[r.UserID] = true
			todo = append(todo, r)
		}
	}
	for i := 0; i < len(todo); i += 200 {
		j := i + 200
		if j > len(todo) {
			j = len(todo)
		}
		batch := make([]Delivery, 0, j-i)
		notes := make([]notify.Notification, 0, j-i)
		for _, r := range todo[i:j] {
			d := Delivery{ID: uuid.NewString(), AnnouncementID: a.ID, OccurrenceAt: occAt, UserID: r.UserID, OrganizationID: r.OrgID,
				Email: r.Email, Name: clip(r.Name, 250), OrgName: clip(r.OrgName, 250), Kind: a.Kind, IsTest: test,
				DeliveredAt: &now, EmailStatus: EmailPending, CreatedAt: now}
			switch {
			case test:
				d.EmailStatus = EmailSending
			case a.EmailDelayMinutes == NeverEmail:
				d.EmailStatus, d.EmailNote = EmailNever, "In-app only"
			default:
				due := now.Add(time.Duration(a.EmailDelayMinutes) * time.Minute)
				d.EmailDueAt = &due
			}
			batch = append(batch, d)
			title := a.Title
			if test {
				title = "[Test] " + title
			}
			notes = append(notes, notify.Notification{ID: uuid.NewString(), OrganizationID: r.OrgID, UserID: r.UserID,
				Type: NotificationType(a.Kind), Severity: severityOf(a), Title: clip(title, 250), Body: clip(a.BodyText, 400),
				Link: ViewPath(d.ID), EntityType: "announcement", EntityID: d.ID, ActorName: "DASYIN", CreatedAt: notify.Now()})
		}
		err := fresh(s.DB).Transaction(func(tx *gorm.DB) error {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&batch).Error; err != nil {
				return err
			}
			if notify.Disabled {
				return nil
			}
			return tx.Create(&notes).Error
		})
		if err != nil {
			return out, err
		}
		out = append(out, batch...)
	}
	return out, nil
}

// sendOne sends the due occurrence of one announcement, if it can claim it.
func (s *Service) sendOne(ctx context.Context, id string) (int, error) {
	now := Now()
	var a Announcement
	if err := fresh(s.DB).First(&a, "id = ?", id).Error; err != nil {
		return 0, err
	}
	if a.NextSendAt == nil || a.NextSendAt.After(now) {
		return 0, nil
	}
	// Claim it: scheduled → sending (or take over a send that stalled).
	res := fresh(s.DB).Model(&Announcement{}).
		Where("id = ? AND next_send_at = ? AND (status = ? OR (status = ? AND updated_at < ?))", a.ID, *a.NextSendAt, StatusScheduled, StatusSending, now.Add(-10*time.Minute)).
		Updates(map[string]any{"status": StatusSending, "updated_at": now})
	if res.Error != nil || res.RowsAffected == 0 {
		return 0, res.Error
	}
	occAt := *a.NextSendAt
	if a.ExpiresAt != nil && !a.ExpiresAt.After(now) {
		fresh(s.DB).Model(&Announcement{}).Where("id = ?", a.ID).Updates(map[string]any{"status": StatusSent, "next_send_at": nil,
			"last_error": "Expired before it was sent", "updated_at": now})
		return 0, nil
	}
	recips := s.Resolve(a.Audience, a.Kind)
	rows, err := s.deliver(&a, occAt, recips, false)
	if err != nil {
		fresh(s.DB).Model(&Announcement{}).Where("id = ?", a.ID).Updates(map[string]any{"status": StatusScheduled, "last_error": clip(err.Error(), 500), "updated_at": Now()})
		return len(rows), err
	}
	a.Occurrences++
	upd := map[string]any{"occurrences": a.Occurrences, "last_sent_at": now, "last_error": "", "updated_at": Now()}
	if next := NextSend(&a, now); next != nil {
		upd["status"], upd["next_send_at"] = StatusScheduled, *next
	} else {
		upd["status"], upd["next_send_at"] = StatusSent, nil
	}
	if err := fresh(s.DB).Model(&Announcement{}).Where("id = ?", a.ID).Updates(upd).Error; err != nil {
		return len(rows), err
	}
	log.Printf("broadcast: %q sent to %d people (occurrence %d)", a.Title, len(rows), a.Occurrences)
	return len(rows), nil
}

// SendDue sends every announcement whose time has come.
func (s *Service) SendDue(ctx context.Context) (announcements, people int) {
	var ids []string
	now := Now()
	fresh(s.DB).Model(&Announcement{}).
		Where("next_send_at <= ? AND (status = ? OR (status = ? AND updated_at < ?))", now, StatusScheduled, StatusSending, now.Add(-10*time.Minute)).
		Order("next_send_at").Limit(50).Pluck("id", &ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		n, err := s.sendOne(ctx, id)
		if err != nil {
			log.Printf("broadcast: send %s: %v", id, err)
			continue
		}
		if n > 0 {
			announcements++
			people += n
		}
	}
	return
}

// TestSend delivers the announcement to one person (the super admin) at once:
// the in-app card and the email, whatever the delay and their switches.
func (s *Service) TestSend(ctx context.Context, a *Announcement, r Recipient) (*Delivery, error) {
	rows, err := s.deliver(a, Now(), []Recipient{r}, true)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("nothing was sent")
	}
	d := rows[0]
	now := Now()
	upd := map[string]any{"emailed_at": now, "email_status": EmailSent, "email_note": "Test send"}
	if d.Email == "" {
		upd = map[string]any{"email_status": EmailSkipped, "email_note": "You have no email address"}
	} else if err := s.sendMail(ctx, s.EmailFor(a, &d, true)); err != nil {
		upd = map[string]any{"email_status": EmailFailed, "email_note": clip(err.Error(), 480)}
	}
	fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(upd)
	return s.reloadDelivery(d.ID)
}

// ---------------------------------------------------------------- reading

// readScope limits a read sync to some deliveries (zero value: all).
type readScope struct {
	userID        string
	announcements []string
	pendingOnly   bool // only deliveries still waiting for their email
}

// syncReads copies "read" from the notification centre (the bell's own read
// switch, "Mark all read") onto the deliveries in scope and settles their
// emails.
func (s *Service) syncReads(sc readScope) {
	cond, args := "", []any{}
	if sc.userID != "" {
		cond += " AND d.user_id = ?"
		args = append(args, sc.userID)
	}
	if len(sc.announcements) > 0 {
		cond += " AND d.announcement_id IN ?"
		args = append(args, sc.announcements)
	}
	if sc.pendingOnly {
		cond += " AND d.email_status = 'pending'"
	}
	if hasTable(s.DB, "notifications") {
		fresh(s.DB).Exec(`UPDATE broadcast_deliveries d SET read_at = n.read_at FROM notifications n
			WHERE n.entity_type = 'announcement' AND n.entity_id = d.id AND d.read_at IS NULL AND n.read_at IS NOT NULL`+cond, args...)
	}
	fresh(s.DB).Exec(`UPDATE broadcast_deliveries d SET email_status = ?, email_note = ?
		WHERE d.email_status = 'pending' AND (d.read_at IS NOT NULL OR d.dismissed_at IS NOT NULL)`+cond,
		append([]any{EmailNotNeeded, "Read in the app before the email was due"}, args...)...)
}

// Mark records read / dismiss / click on a person's delivery.
func (s *Service) Mark(userID, deliveryID, what string) (*Delivery, error) {
	var d Delivery
	if err := fresh(s.DB).Where("id = ? AND user_id = ?", deliveryID, userID).First(&d).Error; err != nil {
		return nil, err
	}
	now := Now()
	upd := map[string]any{}
	if d.ReadAt == nil {
		upd["read_at"] = now
	}
	switch what {
	case "dismiss":
		if d.DismissedAt == nil {
			upd["dismissed_at"] = now
		}
	case "click":
		if d.ClickedAt == nil {
			upd["clicked_at"] = now
		}
	}
	if d.EmailStatus == EmailPending {
		upd["email_status"], upd["email_note"] = EmailNotNeeded, "Read in the app before the email was due"
	}
	if len(upd) > 0 {
		if err := fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(upd).Error; err != nil {
			return nil, err
		}
	}
	if hasTable(s.DB, "notifications") {
		fresh(s.DB).Model(&notify.Notification{}).Where("entity_type = 'announcement' AND entity_id = ? AND user_id = ? AND read_at IS NULL", d.ID, userID).Update("read_at", notify.Now())
	}
	return s.reloadDelivery(d.ID)
}

// reloadDelivery reads a delivery into a fresh value (gorm keeps a pointer
// field's old value when scanning NULL into a used struct).
func (s *Service) reloadDelivery(id string) (*Delivery, error) {
	var d Delivery
	if err := fresh(s.DB).First(&d, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &d, nil
}

// ---------------------------------------------------------------- emailing

// Unsubscribed reports whether the person unsubscribed from offers.
func (s *Service) Unsubscribed(userID string) bool {
	var n int64
	fresh(s.DB).Model(&Unsubscribe{}).Where("user_id = ?", userID).Count(&n)
	return n > 0
}

type userState struct {
	Email          string
	Active         bool
	OrganizationID string
}

func (s *Service) userNow(id string) (userState, bool) {
	var rows []struct {
		Email          string
		IsActive       *bool
		OrganizationID string
	}
	fresh(s.DB).Raw(`SELECT COALESCE(email,'') AS email, is_active, COALESCE(organization_id,'') AS organization_id FROM users WHERE id = ? AND deleted_at IS NULL`, id).Scan(&rows)
	if len(rows) == 0 {
		return userState{}, false
	}
	return userState{Email: strings.ToLower(strings.TrimSpace(rows[0].Email)), Active: rows[0].IsActive == nil || *rows[0].IsActive, OrganizationID: rows[0].OrganizationID}, true
}

// emailDecision: whether a due delivery is emailed (and why not).
func (s *Service) emailDecision(a *Announcement, d *Delivery, now time.Time) (bool, string) {
	if a.Status == StatusCancelled {
		return false, "Announcement cancelled"
	}
	if a.ExpiresAt != nil && !a.ExpiresAt.After(now) {
		return false, "Expired before the email was due"
	}
	u, ok := s.userNow(d.UserID)
	if !ok || !u.Active {
		return false, "The account is no longer active"
	}
	if u.Email == "" {
		return false, "No email address"
	}
	d.Email = u.Email
	if a.Kind == KindPromotion && s.Unsubscribed(d.UserID) {
		return false, "Unsubscribed from DASYIN offers"
	}
	if ServiceNotice(a) {
		return true, ""
	}
	if on, why := emailprefs.Check(s.DB, u.OrganizationID, d.UserID, emailprefs.PlatformUpdates); !on {
		return false, "“Updates from DASYIN” emails are off: " + why
	}
	return true, ""
}

// Escalate emails the people who haven't read an announcement once its delay
// has passed (at most limit emails per call).
func (s *Service) Escalate(ctx context.Context, limit int) (sent, failed, skipped int) {
	s.syncReads(readScope{pendingOnly: true})
	now := Now()
	var due []Delivery
	fresh(s.DB).Where("email_status = ? AND email_due_at <= ? AND read_at IS NULL AND dismissed_at IS NULL AND NOT is_test", EmailPending, now).
		Order("email_due_at").Limit(limit).Find(&due)
	cache := map[string]*Announcement{}
	for i := range due {
		if ctx.Err() != nil {
			return
		}
		d := due[i]
		res := fresh(s.DB).Model(&Delivery{}).Where("id = ? AND email_status = ? AND read_at IS NULL AND dismissed_at IS NULL", d.ID, EmailPending).
			Update("email_status", EmailSending)
		if res.Error != nil || res.RowsAffected == 0 {
			continue // read in the meantime, or another run took it
		}
		a := cache[d.AnnouncementID]
		if a == nil {
			a = &Announcement{}
			if err := fresh(s.DB).First(a, "id = ?", d.AnnouncementID).Error; err != nil {
				fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(map[string]any{"email_status": EmailSkipped, "email_note": "Announcement deleted"})
				skipped++
				continue
			}
			cache[d.AnnouncementID] = a
		}
		ok, why := s.emailDecision(a, &d, now)
		if !ok {
			fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(map[string]any{"email_status": EmailSkipped, "email_note": clip(why, 480)})
			skipped++
			continue
		}
		if err := s.sendMail(ctx, s.EmailFor(a, &d, false)); err != nil {
			fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(map[string]any{"email_status": EmailFailed, "email_note": clip(err.Error(), 480)})
			failed++
			continue
		}
		fresh(s.DB).Model(&Delivery{}).Where("id = ?", d.ID).Updates(map[string]any{"email_status": EmailSent, "emailed_at": Now(), "email_note": "", "email": d.Email})
		sent++
	}
	return
}

// RetryFailed puts an announcement's failed emails back in the queue.
func (s *Service) RetryFailed(a *Announcement) int64 {
	now := Now()
	res := fresh(s.DB).Model(&Delivery{}).Where("announcement_id = ? AND email_status = ? AND NOT is_test", a.ID, EmailFailed).
		Updates(map[string]any{"email_status": EmailPending, "email_due_at": now, "email_note": ""})
	return res.RowsAffected
}

// ---------------------------------------------------------------- scheduler

// RunResult is what one scheduler pass did.
type RunResult struct {
	Announcements int  `json:"announcements"`
	People        int  `json:"people"`
	Emailed       int  `json:"emailed"`
	EmailFailed   int  `json:"email_failed"`
	EmailSkipped  int  `json:"email_skipped"`
	Nudges        int  `json:"nudges"`
	Ran           bool `json:"ran"`
}

const lockKey = "broadcast:scheduler"

// withLock runs fn while holding the platform-wide advisory lock; wait > 0
// keeps trying that long (manual runs), 0 gives up at once (the ticker).
func (s *Service) withLock(ctx context.Context, wait time.Duration, fn func()) (bool, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	got := false
	err := fresh(s.DB).WithContext(ctx).Connection(func(tx *gorm.DB) error {
		end := time.Now().Add(wait)
		for {
			if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", lockKey).Scan(&got).Error; err != nil {
				return err
			}
			if got || time.Now().After(end) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		if !got {
			return nil
		}
		defer tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", lockKey)
		fn()
		return nil
	})
	return got, err
}

// RunOnce sends due announcements, then due emails, then nudges — under the
// lock, so with several instances exactly one does each piece of work.
func (s *Service) RunOnce(ctx context.Context, wait time.Duration) (RunResult, error) {
	var r RunResult
	ok, err := s.withLock(ctx, wait, func() {
		r.Ran = true
		r.Announcements, r.People = s.SendDue(ctx)
		r.Emailed, r.EmailFailed, r.EmailSkipped = s.Escalate(ctx, 500)
		r.Nudges = s.RunNudges(ctx)
	})
	if err == nil && !ok {
		err = errors.New("another server is running announcements right now — try again in a moment")
	}
	return r, err
}

// StartScheduler runs every interval (BROADCAST_SCHEDULER=off disables).
func (s *Service) StartScheduler(ctx context.Context, every, firstDelay time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(firstDelay):
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("broadcast: scheduled run failed: %v", r)
					}
				}()
				r, err := s.RunOnce(ctx, 0)
				if err == nil && (r.People+r.Emailed+r.EmailFailed+r.Nudges) > 0 {
					log.Printf("broadcast: %d announcement(s) to %d people, %d emailed, %d failed, %d skipped, %d nudge(s)",
						r.Announcements, r.People, r.Emailed, r.EmailFailed, r.EmailSkipped, r.Nudges)
				}
			}()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// ---------------------------------------------------------------- helpers

// SuperAdmins lists the active super admins.
func (s *Service) SuperAdmins() []Recipient {
	var rows []struct {
		ID, Email, FirstName, LastName, OrganizationID string
	}
	fresh(s.DB).Raw(`SELECT id, COALESCE(email,'') AS email, COALESCE(first_name,'') AS first_name, COALESCE(last_name,'') AS last_name, COALESCE(organization_id,'') AS organization_id
		FROM users WHERE role = 'super_admin' AND deleted_at IS NULL AND (is_active IS NULL OR is_active) ORDER BY created_at`).Scan(&rows)
	out := []Recipient{}
	for _, r := range rows {
		n := strings.TrimSpace(r.FirstName + " " + r.LastName)
		if n == "" {
			n = r.Email
		}
		out = append(out, Recipient{UserID: r.ID, OrgID: r.OrganizationID, Email: strings.ToLower(strings.TrimSpace(r.Email)), Name: n, Role: "super_admin", Level: emailprefs.LevelOwner})
	}
	return out
}

// Person is one user as a recipient (for test sends).
func (s *Service) Person(userID string) (Recipient, bool) {
	var rows []struct {
		ID, Email, FirstName, LastName, Role, OrganizationID, OrgName string
	}
	fresh(s.DB).Raw(`SELECT u.id, COALESCE(u.email,'') AS email, COALESCE(u.first_name,'') AS first_name, COALESCE(u.last_name,'') AS last_name,
		COALESCE(u.role,'') AS role, COALESCE(u.organization_id,'') AS organization_id, COALESCE(o.name,'') AS org_name
		FROM users u LEFT JOIN organizations o ON o.id = u.organization_id WHERE u.id = ? AND u.deleted_at IS NULL`, userID).Scan(&rows)
	if len(rows) == 0 {
		return Recipient{}, false
	}
	r := rows[0]
	n := strings.TrimSpace(r.FirstName + " " + r.LastName)
	if n == "" {
		n = r.Email
	}
	lvl, _ := emailprefs.LevelOf(r.OrganizationID, r.ID, r.Role)
	return Recipient{UserID: r.ID, OrgID: r.OrganizationID, OrgName: r.OrgName, Email: strings.ToLower(strings.TrimSpace(r.Email)), Name: n, Role: r.Role, Level: lvl}, true
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
