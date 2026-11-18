package brief

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// jsonText is the stored form of a serializer:json column in a map update.
func jsonText(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

// MaxAttempts is how often a channel is tried before it is reported failed.
const MaxAttempts = 3

// RecommendedTemplate is the WhatsApp template to submit to Meta (category
// Utility): body "Good morning {{1}}, your DASYIN brief for {{2}}: {{3}}"
// and a URL button "Open brief" → <PUBLIC_APP_URL>/brief/{{1}}.
var RecommendedTemplate = struct {
	Name, Language, Category, Body, ButtonText, ButtonURL string
}{"dasyin_morning_brief", "en", "UTILITY", "Good morning {{1}}, your DASYIN brief for {{2}}: {{3}}", "Open brief", "/brief/{{1}}"}

// ---------------------------------------------------------------- config

// Config returns the platform's brief settings.
func (s *Service) Config() Config {
	var c Config
	s.DB.Where("id = 1").Limit(1).Find(&c)
	c.ID = 1
	if strings.TrimSpace(c.WALanguage) == "" {
		c.WALanguage = RecommendedTemplate.Language
	}
	return c
}

// SaveConfig stores the WhatsApp template choice.
func (s *Service) SaveConfig(name, lang, by string) (Config, error) {
	name, lang = strings.TrimSpace(name), strings.TrimSpace(lang)
	if name != "" && !templateName.MatchString(name) {
		return s.Config(), invalid("A template name has lower-case letters, numbers and underscores, e.g. dasyin_morning_brief")
	}
	if lang == "" {
		lang = RecommendedTemplate.Language
	}
	c := Config{ID: 1, WATemplate: name, WALanguage: lang, UpdatedBy: by, UpdatedAt: time.Now()}
	if err := s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&c).Error; err != nil {
		return s.Config(), err
	}
	return s.Config(), nil
}

// ---------------------------------------------------------------- whatsapp plan

// WAPlan is how a brief would go out on WhatsApp.
type WAPlan struct {
	Ready    bool     `json:"ready"`
	Problem  string   `json:"problem,omitempty"`
	To       string   `json:"to,omitempty"`   // digits
	Mode     string   `json:"mode,omitempty"` // template | text
	Platform bool     `json:"platform"`       // from DASYIN's number
	Template string   `json:"template,omitempty"`
	Language string   `json:"language,omitempty"`
	Params   []string `json:"params,omitempty"` // {{1}} {{2}} {{3}}
	Button   string   `json:"button,omitempty"` // the URL button's {{1}}
	Text     string   `json:"text,omitempty"`   // the text message (window open), or the template rendered
	account  *wa.Account
}

// TemplateParams are the template's body parameters, cleaned for Meta (no
// new lines, tabs or more than three spaces in a row).
func TemplateParams(b *Brief) []string {
	return []string{wa.CleanParam(nz(b.FirstName, "there")), wa.CleanParam(b.DateLabel), wa.CleanParam(b.Headline)}
}

// realNow is the wall clock (WhatsApp windows are real times).
func (s *Service) realNow() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// planWhatsApp works out the number, sender and message for v.
func (s *Service) planWhatsApp(v *Viewer, b *Brief) WAPlan {
	p := WAPlan{}
	if strings.TrimSpace(v.Phone) == "" {
		p.Problem = "No WhatsApp number — add yours in My profile → Morning brief"
		return p
	}
	to, err := wa.NormalizeNumber(v.Phone, orgprefs.Country(s.DB, v.OrgID))
	if err != nil {
		p.Problem = err.Error()
		return p
	}
	p.To = to
	acc, err := wa.Resolve(s.DB, v.OrgID, true)
	if err != nil {
		p.Problem = "WhatsApp is not set up yet: a super admin adds DASYIN's WhatsApp Business number in System settings → WhatsApp"
		return p
	}
	p.account, p.Platform = acc, acc.Platform
	if wa.WindowOpen(s.DB, acc.OrgID, to, s.realNow()) {
		p.Mode, p.Text, p.Ready = "text", s.Text(b, true), true
		return p
	}
	cfg := s.Config()
	p.Mode, p.Template, p.Language = "template", cfg.WATemplate, cfg.WALanguage
	p.Params = TemplateParams(b)
	p.Button = b.Date
	p.Text = fmt.Sprintf("Good morning %s, your DASYIN brief for %s: %s", p.Params[0], p.Params[1], p.Params[2])
	if cfg.WATemplate == "" {
		p.Problem = "No WhatsApp template chosen yet: a super admin picks the approved brief template in System settings → Notifications"
		return p
	}
	p.Ready = true
	return p
}

// ---------------------------------------------------------------- delivery

// channelResult is the outcome of one channel.
type channelResult struct {
	status string // ChSent | ChAccepted | ChFailed | ChRetry
	err    string
}

func mailRetryable(err error) bool {
	switch mail.Code(err) {
	case "TIMEOUT", "CONNECTION_REFUSED", "DNS", "SMTP", "":
		return true
	}
	return false
}

func (s *Service) sendEmail(ctx context.Context, v *Viewer, snap *Snapshot) channelResult {
	if strings.TrimSpace(v.Email) == "" {
		return channelResult{status: ChFailed, err: "Your account has no email address"}
	}
	if ok, why := s.emailAllowed(v); !ok {
		return channelResult{status: ChFailed, err: why}
	}
	msg := s.Email(v, snap.Data, snap.Kind == KindTest)
	err := s.sendMail(ctx, v.OrgID, msg)
	if err == nil {
		return channelResult{status: ChSent}
	}
	st := ChFailed
	if mailRetryable(err) {
		st = ChRetry
	}
	return channelResult{status: st, err: err.Error()}
}

// EmailCategory is the brief's key in pkg/emailprefs (who gets emails): the
// person's "Pause all my emails", an administrator turning their emails off
// and the organisation's mode apply to the brief's email like to any other.
const EmailCategory = "morning_brief"

// emailAllowed asks pkg/emailprefs whether v may be emailed the brief.
func (s *Service) emailAllowed(v *Viewer) (bool, string) {
	if !hasTable(s.DB, "email_recipient_prefs") {
		return true, ""
	}
	ok, why := emailprefs.Check(s.DB, v.OrgID, v.UserID, EmailCategory)
	if ok {
		return true, ""
	}
	return false, "Not emailed — " + why + " (My profile → Email notifications, or Settings → Who gets emails)"
}

func (s *Service) sendWhatsApp(ctx context.Context, v *Viewer, snap *Snapshot) (channelResult, WAPlan, string) {
	p := s.planWhatsApp(v, snap.Data)
	if !p.Ready {
		return channelResult{status: ChFailed, err: p.Problem}, p, ""
	}
	var res *wa.Result
	var err error
	if p.Mode == "text" {
		res, err = p.account.SendText(ctx, p.To, p.Text, true)
		var we *wa.Error
		if err != nil && errors.As(err, &we) && we.OutsideWindow() {
			// The window closed in between: fall back to the template.
			cfg := s.Config()
			p.Mode, p.Template, p.Language, p.Params, p.Button = "template", cfg.WATemplate, cfg.WALanguage, TemplateParams(snap.Data), snap.Data.Date
			if cfg.WATemplate == "" {
				return channelResult{status: ChFailed, err: wa.Friendly(err)}, p, ""
			}
			res, err = nil, nil
		}
	}
	if p.Mode == "template" && res == nil && err == nil {
		res, err = p.account.SendTemplate(ctx, p.To, wa.Template{Name: p.Template, Language: p.Language, BodyParams: p.Params, URLButton: p.Button})
	}
	if err != nil {
		st := ChFailed
		var we *wa.Error
		if errors.As(err, &we) && we.Retryable() {
			st = ChRetry
		} else if !errors.As(err, &we) {
			st = ChRetry // network trouble reaching Meta
		}
		return channelResult{status: st, err: wa.Friendly(err)}, p, ""
	}
	tpl := ""
	if p.Mode == "template" {
		tpl = p.Template
	}
	wa.Record(s.DB, v.OrgID, p.account, p.To, res, p.Mode, tpl, "brief", snap.ID)
	return channelResult{status: ChAccepted}, p, res.MessageID
}

// send delivers a snapshot over the person's channels (only the channels
// still to do when retrying) and stores the outcome.
func (s *Service) send(ctx context.Context, v *Viewer, st Settings, snap *Snapshot, retry bool) {
	now := s.now()
	snap.Attempts++
	wantEmail := st.Email && (!retry || snap.EmailStatus == ChRetry)
	wantWA := st.WhatsApp && (!retry || snap.WAStatus == ChRetry)
	if wantEmail {
		r := s.sendEmail(ctx, v, snap)
		snap.EmailStatus, snap.EmailError, snap.EmailTo, snap.EmailAt = r.status, r.err, v.Email, &now
	} else if !retry {
		snap.EmailStatus = ChOff
	}
	if wantWA {
		r, p, id := s.sendWhatsApp(ctx, v, snap)
		snap.WAStatus, snap.WAError, snap.WATo, snap.WAKind, snap.WAPlatform, snap.WAAt = r.status, r.err, p.To, p.Mode, p.Platform, &now
		if id != "" {
			snap.WAMessageID = id
		}
	} else if !retry {
		snap.WAStatus = ChOff
	}
	s.settle(snap, now)
	if err := s.DB.Model(&Snapshot{}).Where("id = ?", snap.ID).Updates(map[string]any{
		"status": snap.Status, "headline": snap.Headline, "data": jsonText(snap.Data), "attempts": snap.Attempts, "next_retry_at": snap.NextRetryAt,
		"email_status": snap.EmailStatus, "email_to": snap.EmailTo, "email_error": snap.EmailError, "email_at": snap.EmailAt,
		"wa_status": snap.WAStatus, "wa_to": snap.WATo, "wa_kind": snap.WAKind, "wa_platform": snap.WAPlatform, "wa_message_id": snap.WAMessageID,
		"wa_error": snap.WAError, "wa_at": snap.WAAt, "updated_at": time.Now(),
	}).Error; err != nil {
		log.Printf("brief: save snapshot %s: %v", snap.ID, err)
	}
}

func okStatus(st string) bool {
	return st == ChSent || st == ChAccepted || st == ChDelivered || st == ChRead
}

// settle works out the overall status (and the next retry).
func (s *Service) settle(snap *Snapshot, now time.Time) {
	chans := []*string{}
	if snap.EmailStatus != ChOff {
		chans = append(chans, &snap.EmailStatus)
	}
	if snap.WAStatus != ChOff {
		chans = append(chans, &snap.WAStatus)
	}
	retrying := false
	for _, c := range chans {
		if *c == ChRetry {
			if snap.Attempts >= MaxAttempts {
				*c = ChFailed
			} else {
				retrying = true
			}
		}
	}
	ok, failed := 0, 0
	for _, c := range chans {
		switch {
		case okStatus(*c):
			ok++
		case *c == ChFailed:
			failed++
		}
	}
	snap.NextRetryAt = nil
	switch {
	case retrying:
		snap.Status = StatusRetry
		next := now.Add(time.Duration(5*snap.Attempts) * time.Minute)
		snap.NextRetryAt = &next
	case len(chans) == 0:
		snap.Status = StatusFailed
	case failed == 0:
		snap.Status = StatusSent
	case ok > 0:
		snap.Status = StatusPartial
	default:
		snap.Status = StatusFailed
	}
}

// ---------------------------------------------------------------- one brief

// SendNow builds and sends a person's brief: kind KindTest for "Send me a
// test now" (replaces today's earlier test), KindScheduled from the
// scheduler (claimed once per local date).
func (s *Service) SendNow(ctx context.Context, v *Viewer, st Settings, kind string) (*Snapshot, error) {
	if !st.Email && !st.WhatsApp {
		return nil, invalid("Choose email, WhatsApp or both first")
	}
	tz, loc := s.zoneOf(st)
	now := s.now()
	date := dayStr(localDay(now, loc))
	snap := &Snapshot{ID: uuid.NewString(), OrganizationID: v.OrgID, UserID: v.UserID, Date: date, Kind: kind, Timezone: tz, Status: StatusSending,
		CreatedAt: now, UpdatedAt: time.Now()}
	if kind == KindTest {
		s.DB.Where("user_id = ? AND date = ? AND kind = ?", v.UserID, date, KindTest).Delete(&Snapshot{})
	}
	if err := s.DB.Create(snap).Error; err != nil {
		return nil, err
	}
	b := s.Build(ctx, v, st, BuildOptions{SyncJira: true, JiraTimeout: 30 * time.Second})
	snap.Data, snap.Headline = b, b.Headline
	s.send(ctx, v, st, snap, false)
	return snap, nil
}

// deliverScheduled sends a claimed scheduled snapshot.
func (s *Service) deliverScheduled(ctx context.Context, snap *Snapshot) {
	v, err := s.Viewer(snap.UserID)
	if err != nil {
		s.DB.Model(&Snapshot{}).Where("id = ?", snap.ID).Updates(map[string]any{"status": StatusFailed, "email_error": "The person's account is no longer active", "updated_at": time.Now()})
		return
	}
	st := s.SettingsFor(v.UserID, v.OrgID)
	b := s.Build(ctx, v, st, BuildOptions{SyncJira: true, Date: snap.Date})
	snap.Data, snap.Headline = b, b.Headline
	s.send(ctx, v, st, snap, false)
}

// retry resends the channels of a snapshot that failed with a temporary error.
func (s *Service) retry(ctx context.Context, snap *Snapshot) {
	v, err := s.Viewer(snap.UserID)
	if err != nil || snap.Data == nil {
		s.DB.Model(&Snapshot{}).Where("id = ?", snap.ID).Updates(map[string]any{"status": StatusFailed, "next_retry_at": nil, "updated_at": time.Now()})
		return
	}
	st := s.SettingsFor(v.UserID, v.OrgID)
	s.send(ctx, v, st, snap, true)
}

// ---------------------------------------------------------------- statuses

var waRank = map[string]int{ChAccepted: 1, ChSent: 1, ChDelivered: 2, ChRead: 3, ChFailed: 4}

// OnWhatsAppStatus moves a brief's WhatsApp status along (wa.OnStatus).
func (s *Service) OnWhatsAppStatus(ev wa.StatusEvent) {
	if ev.MessageID == "" {
		return
	}
	var snap Snapshot
	s.DB.Where("wa_message_id = ?", ev.MessageID).Limit(1).Find(&snap)
	if snap.ID == "" {
		return
	}
	st := ev.Status
	if st == "sent" {
		st = ChAccepted
	}
	if waRank[st] < waRank[snap.WAStatus] {
		return
	}
	upd := map[string]any{"wa_status": st, "updated_at": time.Now()}
	if st == ChFailed {
		upd["wa_error"] = nz(ev.Error, "WhatsApp could not deliver the message")
		if snap.EmailStatus != ChOff && okStatus(snap.EmailStatus) {
			upd["status"] = StatusPartial
		} else {
			upd["status"] = StatusFailed
		}
	}
	s.DB.Model(&Snapshot{}).Where("id = ?", snap.ID).Updates(upd)
}
