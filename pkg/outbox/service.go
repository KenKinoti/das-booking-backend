package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Service is the review queue of every organisation.
type Service struct {
	DB   *gorm.DB
	Mail *mail.Service
	// AppURL is the web app origin (PUBLIC_APP_URL) for links in the update emails.
	AppURL string
	// LogoURL returns the organisation's absolute logo URL ("" for none).
	LogoURL func(org string) string
	// Now is the clock (tests, OUTBOX_TEST_CLOCK).
	Now func() time.Time
	// DefaultOff: organisations without a saved setting start with review
	// off (OUTBOX_DEFAULT=off; tests and other flows only).
	DefaultOff bool
	// TestClock enables the test endpoints (OUTBOX_TEST_CLOCK=1).
	TestClock bool
	runMu     sync.Mutex
	clockMu   sync.RWMutex
	fixed     *time.Time
	evMu      sync.Mutex
	lastEv    time.Time
}

var (
	stdMu sync.RWMutex
	std   *Service
)

// Install makes s the process-wide queue: from now on the gate is live
// (cmd/production and tests). Without it every module sends as before.
func Install(s *Service) *Service {
	if s.Mail == nil {
		s.Mail = mail.Shared(s.DB)
	}
	stdMu.Lock()
	std = s
	stdMu.Unlock()
	return s
}

// Uninstall switches the gate off again (tests).
func Uninstall() {
	stdMu.Lock()
	std = nil
	stdMu.Unlock()
}

// Default is the installed service (nil = review is not running here).
func Default() *Service {
	stdMu.RLock()
	defer stdMu.RUnlock()
	return std
}

// New builds a service from the environment.
func New(db *gorm.DB) *Service {
	return &Service{DB: db, Mail: mail.Shared(db), Now: time.Now,
		DefaultOff: strings.EqualFold(os.Getenv("OUTBOX_DEFAULT"), "off"),
		TestClock:  os.Getenv("OUTBOX_TEST_CLOCK") == "1"}
}

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }

func (s *Service) db() *gorm.DB { return s.DB.Session(&gorm.Session{NewDB: true}) }

// now is the service clock (the test clock when set).
func (s *Service) now() time.Time {
	s.clockMu.RLock()
	f := s.fixed
	s.clockMu.RUnlock()
	if f != nil {
		return *f
	}
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// SetClock fixes the clock (nil = real time again; tests).
func (s *Service) SetClock(t *time.Time) {
	s.clockMu.Lock()
	s.fixed = t
	s.clockMu.Unlock()
}

// Loc is the organisation's time zone.
func (s *Service) Loc(org string) *time.Location {
	if tz := orgprefs.Get(s.db(), org).Timezone; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	if l, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return l
	}
	return time.UTC
}

// ---------------------------------------------------------------- settings

// SettingsFor is the organisation's setting (defaults when none was saved).
func (s *Service) SettingsFor(org string) Settings {
	st := Settings{OrganizationID: org, Enabled: !s.DefaultOff, AdminsCanSendNow: true, StaleDays: DefaultStaleDays, SoonAfter: true, Daily: true, DailyTime: DefaultDailyTime}
	var row Settings
	if err := s.db().Where("organization_id = ?", org).Limit(1).Find(&row).Error; err == nil && row.OrganizationID != "" {
		row.saved = true
		if row.StaleDays <= 0 {
			row.StaleDays = DefaultStaleDays
		}
		if _, ok := parseHHMM(row.DailyTime); !ok {
			row.DailyTime = DefaultDailyTime
		}
		return row
	}
	return st
}

// Enabled reports whether org reviews client messages.
func (s *Service) Enabled(org string) bool { return s.SettingsFor(org).Enabled }

func parseHHMM(v string) (int, bool) {
	var h, m int
	if len(v) != 5 || v[2] != ':' {
		return 0, false
	}
	if _, err := fmt.Sscanf(v, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// ---------------------------------------------------------------- people

// RoleOf resolves a user's business-type role (pkg/industry): its base level
// (admin | manager | staff | read_only) and explicit permissions. Set by
// cmd/production; when nil or !ok, users.role decides.
var RoleOf func(org, user, legacy string) (base string, perms map[string][]string, ok bool)

// ownerRoles are the legacy roles that approve.
var ownerRoles = map[string]bool{"owner": true, "admin": true, "org_admin": true, "super_admin": true}

// Permission is the approve permission (module × action).
const (
	PermModule = "outbox"
	PermAction = "approve"
)

// CanApprove reports whether the user may edit, approve and reject (the
// outbox.approve permission: role base admin, legacy owner / admin, or an
// explicit "outbox": ["approve"] grant — the "*" wildcard doesn't count).
func (s *Service) CanApprove(org, user, legacy string) bool {
	legacy = strings.ToLower(strings.TrimSpace(legacy))
	if user != "" && legacy == "" {
		legacy = s.legacyRole(user)
	}
	if legacy == "super_admin" {
		return true
	}
	if user == "" {
		return false
	}
	if RoleOf != nil {
		if base, perms, ok := RoleOf(org, user, legacy); ok {
			if base == "admin" {
				return true
			}
			for _, a := range perms[PermModule] {
				if a == PermAction {
					return true
				}
			}
			return false
		}
	}
	return ownerRoles[legacy]
}

func (s *Service) legacyRole(user string) string {
	var role []string
	s.db().Raw(`SELECT COALESCE(role,'') FROM users WHERE id = ? LIMIT 1`, user).Scan(&role)
	if len(role) == 0 {
		return ""
	}
	return strings.ToLower(role[0])
}

// UserName is a user's display name ("" when unknown).
func (s *Service) UserName(user string) string {
	if user == "" {
		return ""
	}
	return notify.UserName(s.db(), user)
}

// Approvers are the organisation's active users who may approve.
func (s *Service) Approvers(org string) []string {
	var rows []struct{ ID, Role string }
	s.db().Raw(`SELECT id, COALESCE(role,'') AS role FROM users WHERE organization_id = ? AND deleted_at IS NULL AND (is_active IS NULL OR is_active) ORDER BY created_at`, org).Scan(&rows)
	out := []string{}
	for _, r := range rows {
		if s.CanApprove(org, r.ID, r.Role) {
			out = append(out, r.ID)
		}
	}
	return out
}

// ---------------------------------------------------------------- the gate

// Who is the person (or engine) a module sends as.
type Who struct {
	UserID string // "" for automatic sends without a person
	Role   string // users.role when known ("" = looked up)
}

// Decision is the gate's answer.
type Decision struct {
	// Wait: the message must become an item (review is on).
	Wait bool
	// SentNow: an approver ticked "Send now — I've reviewed it" — it goes.
	SentNow bool
	// Reviewing: review is on for the organisation.
	Reviewing bool
}

// Decide is the gate: does this client message wait? sendNow is the
// "Send now — I've reviewed it" tick (honoured for approvers only, while
// the setting allows it).
func (s *Service) Decide(org string, who Who, sendNow bool) Decision {
	if s == nil || org == "" {
		return Decision{}
	}
	st := s.SettingsFor(org)
	if !st.Enabled {
		return Decision{}
	}
	if sendNow && st.AdminsCanSendNow && s.CanApprove(org, who.UserID, who.Role) {
		return Decision{SentNow: true, Reviewing: true}
	}
	return Decision{Wait: true, Reviewing: true}
}

// Decide is the package-level gate (off when no service is installed).
func Decide(org string, who Who, sendNow bool) Decision {
	return Default().Decide(org, who, sendNow)
}

// Active reports whether the gate runs in this process.
func Active() bool { return Default() != nil }

// Reviewing reports whether org reviews client messages here (installed and on).
func Reviewing(org string) bool {
	s := Default()
	return s != nil && s.Enabled(org)
}

// LiveReply is the gate for a reply typed in an open conversation: an
// approver's goes at once (they review it as they write), others wait.
func LiveReply(org string, who Who) Decision { return Decide(org, who, true) }

// Ref is what a module tells its caller about a queued message.
type Ref struct {
	Queued  bool       `json:"queued"`
	ID      string     `json:"id,omitempty"`
	Status  string     `json:"status,omitempty"`
	Link    string     `json:"link,omitempty"`
	SendAt  *time.Time `json:"send_at"`
	SentNow bool       `json:"sent_now,omitempty"`
	Message string     `json:"message"`
}

// QueuedRef describes a waiting item.
func QueuedRef(it *Item) *Ref {
	if it == nil {
		return nil
	}
	return &Ref{Queued: true, ID: it.ID, Status: it.Status, Link: "/outbox?open=" + it.ID, SendAt: it.SendAt,
		Message: "Waiting for approval — it goes out once an admin approves it in Pending review."}
}

// SentNowRef says an approver sent it at once.
func SentNowRef() *Ref {
	return &Ref{SentNow: true, Message: "Sent now — you reviewed it."}
}

// ---------------------------------------------------------------- enqueue

// Enqueue stores a waiting item (in tx when given, so it commits with the
// module's own changes). An item with the same Key is returned as is.
func Enqueue(tx *gorm.DB, it *Item) (*Item, error) {
	s := Default()
	if s == nil {
		return nil, errors.New("pending review is not running")
	}
	return s.Enqueue(tx, it)
}

// Enqueue stores a waiting item.
func (s *Service) Enqueue(tx *gorm.DB, it *Item) (*Item, error) {
	if tx == nil {
		tx = s.db()
	}
	if it.OrganizationID == "" || it.Kind == "" || it.Module == "" || it.Channel == "" {
		return nil, errors.New("outbox: an item needs an organisation, kind, module and channel")
	}
	if it.Key != "" {
		var ex Item
		if err := tx.Where("organization_id = ? AND key = ?", it.OrganizationID, it.Key).Limit(1).Find(&ex).Error; err == nil && ex.ID != "" {
			return &ex, nil
		}
	}
	now := s.now()
	if it.ID == "" {
		it.ID = uuid.NewString()
	}
	it.Status = StatusPending
	if it.CC == nil {
		it.CC = []string{}
	}
	if it.BCC == nil {
		it.BCC = []string{}
	}
	if it.WAParams == nil {
		it.WAParams = []string{}
	}
	if it.Editable == nil {
		it.Editable = []string{}
	}
	if it.TriggerKind == "" {
		it.TriggerKind = TriggerManual
		if it.CreatedBy == "" {
			it.TriggerKind = TriggerAutomatic
		}
	}
	if it.CreatedBy != "" && it.CreatedByName == "" {
		it.CreatedByName = s.UserName(it.CreatedBy)
	}
	if it.Trigger == "" {
		switch {
		case it.TriggerKind == TriggerManual && it.CreatedByName != "":
			it.Trigger = "Sent by " + it.CreatedByName
		case it.TriggerKind == TriggerManual:
			it.Trigger = "Sent by hand"
		default:
			it.Trigger = "Automatic"
		}
	}
	it.Subject = clip(it.Subject, 490)
	it.Trigger = clip(it.Trigger, 290)
	it.Flag = clip(it.Flag, 290)
	it.CreatedAt, it.UpdatedAt = now, now
	if err := tx.Create(it).Error; err != nil {
		return nil, err
	}
	ev := &Event{ID: uuid.NewString(), OrganizationID: it.OrganizationID, ItemID: it.ID, At: s.eventTime(), Action: "queued", Text: clip("Queued — "+it.Trigger, 590)}
	if err := tx.Create(ev).Error; err != nil {
		return nil, err
	}
	s.tellApprovers(it)
	return it, nil
}

// tellApprovers adds the in-app notification (deduped while unread).
func (s *Service) tellApprovers(it *Item) {
	org := it.OrganizationID
	ids := s.Approvers(org)
	if len(ids) == 0 {
		return
	}
	notify.Go(s.db(), org, notify.Users(ids...), notify.Message{Type: "outbox.waiting", Severity: notify.Info,
		Title: "Messages waiting for your approval", Body: clip(KindLabel(it.Kind)+" to "+firstNonEmpty(it.ToName, it.To)+" is waiting in Pending review.", 400),
		Link: "/outbox", EntityType: "outbox", EntityID: org, Dedupe: true})
}

// KindLabel is a kind's readable name.
func KindLabel(k string) string {
	if l, ok := KindLabels[k]; ok {
		return l
	}
	return "Message"
}

// ---------------------------------------------------------------- registry

// Result is what a module's send produced.
type Result struct {
	Text string // "Sent to a@x (+1 CC)"
	Ref  string // email-log id, WhatsApp message id, SMS id …
}

// Preview is the client's-eye view of an item.
type Preview struct {
	Subject     string       `json:"subject"`
	From        string       `json:"from"`
	ReplyTo     string       `json:"reply_to"`
	To          string       `json:"to"`
	CC          []string     `json:"cc"`
	BCC         []string     `json:"bcc"`
	HTML        string       `json:"html"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
	SMS         *SMSInfo     `json:"sms"`
	Error       string       `json:"error"`
	// WindowOpen: WhatsApp free text is possible now (24 h window).
	WindowOpen *bool `json:"-"`
}

// Attachment is a file the message carries, made again at release.
type Attachment struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // pdf | ics
	Note string `json:"note"`
	URL  string `json:"url"` // authenticated API path
}

// SMSInfo is the length of an SMS.
type SMSInfo struct {
	Chars    int `json:"chars"`
	Segments int `json:"segments"`
}

// Releaser sends the items a module owns.
type Releaser interface {
	// Recheck says why the item is no longer needed ("" = still valid).
	Recheck(ctx context.Context, it *Item) string
	// Send sends the item through the module's normal path.
	Send(ctx context.Context, it *Item) (Result, error)
	// Preview renders it exactly as the client will get it.
	Preview(ctx context.Context, it *Item) Preview
	// Closed tells the module it won't be sent (rejected / superseded).
	Closed(ctx context.Context, it *Item)
}

// OriginHook hears the final outcome (sent / rejected / superseded /
// failed) of items a module started through another module's send path.
type OriginHook func(ctx context.Context, it *Item)

var (
	regMu     sync.RWMutex
	releasers = map[string]Releaser{}
	origins   = map[string]OriginHook{}
)

// Register sets the releaser of a module's items (a later one replaces it).
func Register(module string, r Releaser) {
	regMu.Lock()
	releasers[module] = r
	regMu.Unlock()
}

// RegisterOrigin sets the outcome hook of a starting module.
func RegisterOrigin(origin string, h OriginHook) {
	regMu.Lock()
	origins[origin] = h
	regMu.Unlock()
}

func releaserOf(module string) Releaser {
	regMu.RLock()
	defer regMu.RUnlock()
	return releasers[module]
}

func originOf(origin string) OriginHook {
	regMu.RLock()
	defer regMu.RUnlock()
	return origins[origin]
}

// ---------------------------------------------------------------- helpers

// UpdateFlags sets an item's time-sensitive flag (a module learns a deadline later).
func UpdateFlags(org, id string, deadline *time.Time, flag, blocks string) {
	s := Default()
	if s == nil || id == "" {
		return
	}
	s.db().Model(&Item{}).Where("id = ? AND organization_id = ?", id, org).Updates(map[string]any{"deadline": deadline, "flag": clip(flag, 290), "blocks": blocks, "updated_at": s.now()})
}

// Get loads one item (any organisation; modules use it for their own items).
func Get(id string) *Item {
	s := Default()
	if s == nil || id == "" {
		return nil
	}
	var it Item
	if s.db().Where("id = ?", id).Limit(1).Find(&it).Error != nil || it.ID == "" {
		return nil
	}
	return &it
}

// PayloadOf decodes an item's payload into v.
func PayloadOf(it *Item, v any) error {
	if it == nil || it.Payload == "" {
		return nil
	}
	return json.Unmarshal([]byte(it.Payload), v)
}

// Payload encodes v for Item.Payload.
func Payload(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("outbox: payload: %v", err)
		return "{}"
	}
	return string(b)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// Clip trims text to n bytes on a character boundary (modules' snippets).
func Clip(s string, n int) string { return clip(s, n) }
