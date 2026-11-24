package outbox

import (
	"context"
	"fmt"
	"html"
	"log"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Category is the "Who gets emails" key of the update emails.
const Category = "pending_review"

// digests sends the soon-after and daily update emails that are due.
func (s *Service) digests(ctx context.Context, r *RunResult) {
	var orgs []string
	s.db().Model(&Item{}).Distinct("organization_id").Where("status IN ?", []string{StatusPending, StatusFailed}).Pluck("organization_id", &orgs)
	for _, org := range orgs {
		if ctx.Err() != nil {
			return
		}
		st := s.SettingsFor(org)
		if st.Daily {
			if ok, err := s.dailyDue(ctx, org, st); ok {
				r.DailyEmails++
			} else if err != nil {
				r.Notes = append(r.Notes, org+": daily: "+err.Error())
			}
		}
		if st.Enabled && st.SoonAfter {
			if ok, err := s.soonDue(ctx, org, st); ok {
				r.SoonEmails++
			} else if err != nil {
				r.Notes = append(r.Notes, org+": soon after: "+err.Error())
			}
		}
	}
}

func (s *Service) lastDigest(org, kind string) *time.Time {
	var at []time.Time
	s.db().Model(&Digest{}).Where("organization_id = ? AND kind = ?", org, kind).Order("sent_at DESC").Limit(1).Pluck("sent_at", &at)
	if len(at) == 0 {
		return nil
	}
	return &at[0]
}

// soonDue: ~15 minutes after the first new item, at most one an hour.
func (s *Service) soonDue(ctx context.Context, org string, st Settings) (bool, error) {
	now := s.now()
	var first []time.Time
	s.db().Model(&Item{}).Where("organization_id = ? AND status IN ? AND notified_at IS NULL", org, []string{StatusPending, StatusFailed}).
		Order("created_at").Limit(1).Pluck("created_at", &first)
	if len(first) == 0 || now.Before(first[0].Add(SoonAfterDelay)) {
		return false, nil
	}
	if last := s.lastDigest(org, "soon"); last != nil && now.Sub(*last) < SoonAfterMinGap {
		return false, nil
	}
	var fresh []Item
	s.db().Where("organization_id = ? AND status IN ? AND notified_at IS NULL", org, []string{StatusPending, StatusFailed}).Order("created_at").Find(&fresh)
	if len(fresh) == 0 {
		return false, nil
	}
	var waiting int64
	s.db().Model(&Item{}).Where("organization_id = ? AND status IN ?", org, []string{StatusPending, StatusFailed}).Count(&waiting)
	key := fmt.Sprint(now.Unix() / 60)
	return s.sendDigest(ctx, org, st, "soon", key, fresh, int(waiting))
}

// dailyDue: at the daily time (organisation time zone) when anything waits.
func (s *Service) dailyDue(ctx context.Context, org string, st Settings) (bool, error) {
	now := s.now()
	loc := s.Loc(org)
	local := now.In(loc)
	at, _ := parseHHMM(st.DailyTime)
	if local.Hour()*60+local.Minute() < at {
		return false, nil
	}
	key := local.Format("2006-01-02")
	var n int64
	s.db().Model(&Digest{}).Where("organization_id = ? AND kind = ? AND key = ?", org, "daily", key).Count(&n)
	if n > 0 {
		return false, nil
	}
	var all []Item
	s.db().Where("organization_id = ? AND status IN ?", org, []string{StatusPending, StatusFailed}).Order("created_at").Find(&all)
	if len(all) == 0 {
		return false, nil
	}
	return s.sendDigest(ctx, org, st, "daily", key, all, len(all))
}

// EmailsFor filters users through "Who gets emails" (pkg/emailprefs
// UserEmails; set by cmd/production — pkg/emailprefs can't be imported here
// without an import cycle). nil = every user's own address.
var EmailsFor func(db *gorm.DB, org, category string, userIDs []string) []string

// DigestRecipients are the addresses that get the update emails.
func (s *Service) DigestRecipients(org string) []string {
	ids := s.Approvers(org)
	var raw []string
	if EmailsFor != nil {
		raw = EmailsFor(s.db(), org, Category, ids)
	} else if len(ids) > 0 {
		s.db().Raw(`SELECT LOWER(email) FROM users WHERE id IN ? AND COALESCE(email,'') <> ''`, ids).Scan(&raw)
	}
	v, _ := mail.ParseList(strings.Join(raw, ","))
	return v
}

func (s *Service) sendDigest(ctx context.Context, org string, st Settings, kind, key string, items []Item, waiting int) (bool, error) {
	now := s.now()
	d := Digest{ID: uuid.NewString(), OrganizationID: org, Kind: kind, Key: key, SentAt: now, Items: len(items), Waiting: waiting}
	// Claim the slot first: two servers never send the same digest.
	if res := s.db().Clauses(clause.OnConflict{DoNothing: true}).Create(&d); res.Error != nil || res.RowsAffected == 0 {
		return false, res.Error
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	s.db().Model(&Item{}).Where("id IN ? AND notified_at IS NULL", ids).Update("notified_at", now)
	to := s.DigestRecipients(org)
	if len(to) == 0 {
		msg := "Nobody gets Pending review emails — check Settings → Who gets emails"
		s.db().Model(&Digest{}).Where("id = ?", d.ID).Update("error", msg)
		return false, nil
	}
	msg := s.DigestMessage(org, st, kind, items, waiting)
	msg.To = to
	_, err := s.Mail.SendAs(ctx, org, msg)
	upd := map[string]any{"recipients": strings.Join(to, ", ")}
	if err != nil {
		upd["error"] = clip(err.Error(), 1000)
		log.Printf("outbox: %s update email for %s: %v", kind, org, err)
	}
	s.db().Model(&Digest{}).Where("id = ?", d.ID).Updates(upd)
	return err == nil, err
}

// ---------------------------------------------------------------- the email

type orgInfo struct {
	Name, Logo, Accent string
}

func (s *Service) org(org string) orgInfo {
	var name []string
	s.db().Raw(`SELECT COALESCE(name,'') FROM organizations WHERE id = ? LIMIT 1`, org).Scan(&name)
	o := orgInfo{Name: "your organisation", Accent: "#2563eb"}
	if len(name) > 0 && name[0] != "" {
		o.Name = name[0]
	}
	if s.LogoURL != nil {
		o.Logo = s.LogoURL(org)
	}
	return o
}

// ago is "3 hours" / "2 days".
func ago(d time.Duration) string {
	switch {
	case d < time.Hour:
		m := int(math.Max(1, d.Minutes()))
		return plural(m, "minute", "minutes")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour", "hours")
	}
	return plural(int(d.Hours()/24), "day", "days")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func money(v float64, cur string) string {
	neg := v < 0
	v = math.Abs(v)
	whole := int64(v)
	cents := int64(math.Round((v - float64(whole)) * 100))
	if cents == 100 {
		whole++
		cents = 0
	}
	s := fmt.Sprint(whole)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := fmt.Sprintf("%s.%02d", b.String(), cents)
	if cur != "" {
		out = cur + " " + out
	}
	if neg {
		out = "-" + out
	}
	return out
}

func (s *Service) link(path string) string {
	if s.AppURL == "" {
		return ""
	}
	return strings.TrimRight(s.AppURL, "/") + path
}

func channelWord(c string) string {
	switch c {
	case ChanSMS:
		return "SMS"
	case ChanWhatsApp:
		return "WhatsApp"
	}
	return "Email"
}

// DigestMessage renders an update email (soon | daily).
func (s *Service) DigestMessage(org string, st Settings, kind string, items []Item, waiting int) mail.Message {
	info := s.org(org)
	now := s.now()
	stale, blocking, sensitive := 0, 0, 0
	for i := range items {
		r := s.RowOf(&items[i], st)
		if r.Stale {
			stale++
		}
		if r.Blocks == "suspension" {
			blocking++
		} else if r.TimeSensitive {
			sensitive++
		}
	}
	var subject, lead string
	if kind == "daily" {
		flags := []string{}
		if stale > 0 {
			flags = append(flags, fmt.Sprintf("%d stale", stale))
		}
		if blocking > 0 {
			flags = append(flags, plural(blocking, "blocking a suspension", "blocking suspensions"))
		}
		if sensitive > 0 {
			flags = append(flags, fmt.Sprintf("%d time-sensitive", sensitive))
		}
		subject = fmt.Sprintf("Pending review: %s waiting", plural(waiting, "message", "messages"))
		if len(flags) > 0 {
			subject += " (" + strings.Join(flags, ", ") + ")"
		}
		lead = fmt.Sprintf("Good morning. %s for %s %s waiting for your approval — nothing goes to a client until someone approves it. Oldest first:",
			plural(waiting, "client message", "client messages"), info.Name, map[bool]string{true: "is", false: "are"}[waiting == 1])
	} else {
		if len(items) == 1 {
			it := items[0]
			what := KindLabel(it.Kind)
			if it.RecordNumber != "" {
				what += " " + it.RecordNumber
			}
			subject = "A message is waiting for your approval: " + what + " to " + firstNonEmpty(it.CustomerName, it.ToName, it.To)
		} else {
			subject = fmt.Sprintf("%d messages are waiting for your approval", len(items))
		}
		lead = fmt.Sprintf("%s started waiting in Pending review", plural(len(items), "new client message", "new client messages"))
		if waiting > len(items) {
			lead += fmt.Sprintf(" (%d waiting in all)", waiting)
		}
		lead += ". Nothing goes out until you approve it — you can edit each one first."
	}
	listed := items
	if len(listed) > 50 {
		listed = listed[:50]
	}
	var h, t strings.Builder
	e := html.EscapeString
	h.WriteString(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"></head>`)
	h.WriteString(`<body style="margin:0;padding:0;background:#f4f5f7;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#1f2937">`)
	h.WriteString(`<div style="display:none;max-height:0;overflow:hidden">` + e(subject) + `</div>`)
	h.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f4f5f7"><tr><td align="center" style="padding:24px 12px">`)
	h.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:640px;background:#ffffff;border-radius:12px;overflow:hidden">`)
	h.WriteString(`<tr><td style="padding:20px 24px;border-bottom:3px solid ` + info.Accent + `">`)
	if info.Logo != "" {
		h.WriteString(`<img src="` + e(info.Logo) + `" alt="` + e(info.Name) + `" style="max-height:40px;max-width:200px;display:block">`)
	} else {
		h.WriteString(`<div style="font-size:18px;font-weight:700">` + e(info.Name) + `</div>`)
	}
	h.WriteString(`<div style="font-size:13px;color:#6b7280;margin-top:4px">Pending review</div></td></tr>`)
	h.WriteString(`<tr><td style="padding:20px 24px"><p style="margin:0 0 16px;font-size:15px;line-height:1.5">` + e(lead) + `</p>`)
	t.WriteString(subject + "\n\n" + lead + "\n\n")
	for i := range listed {
		it := &listed[i]
		r := s.RowOf(it, st)
		title := KindLabel(it.Kind)
		if it.RecordNumber != "" {
			title += " " + it.RecordNumber
		}
		to := firstNonEmpty(it.ToName, it.To)
		if it.ToName != "" && it.To != "" && it.ToName != it.To {
			to = it.ToName + " (" + it.To + ")"
		}
		meta := []string{channelWord(it.Channel) + " to " + to}
		if it.Amount != nil {
			meta = append(meta, money(*it.Amount, it.Currency))
		}
		meta = append(meta, "waiting "+ago(now.Sub(it.CreatedAt)))
		why := it.Trigger
		flags := []string{}
		if r.Stale {
			flags = append(flags, "Stale — waiting more than "+plural(st.StaleDays, "day", "days"))
		}
		if r.Flag != "" {
			flags = append(flags, r.Flag)
		} else if r.TimeSensitive && it.Deadline != nil {
			flags = append(flags, "Time-sensitive: "+s.localTime(org, *it.Deadline))
		}
		if it.Status == StatusFailed {
			flags = append(flags, "Failed — try it again: "+clip(it.Error, 160))
		}
		border := "#e5e7eb"
		if len(flags) > 0 {
			border = "#f59e0b"
		}
		h.WriteString(`<div style="border:1px solid ` + border + `;border-radius:10px;padding:12px 14px;margin:0 0 10px">`)
		h.WriteString(`<div style="font-weight:600;font-size:15px">` + e(title) + `</div>`)
		if it.Subject != "" {
			h.WriteString(`<div style="font-size:14px;margin-top:2px">` + e(it.Subject) + `</div>`)
		}
		h.WriteString(`<div style="font-size:13px;color:#4b5563;margin-top:4px">` + e(strings.Join(meta, " · ")) + `</div>`)
		h.WriteString(`<div style="font-size:13px;color:#6b7280;margin-top:2px">` + e(why) + `</div>`)
		for _, f := range flags {
			h.WriteString(`<div style="font-size:13px;color:#b45309;font-weight:600;margin-top:4px">⚑ ` + e(f) + `</div>`)
		}
		if u := s.link("/outbox?open=" + it.ID); u != "" {
			h.WriteString(`<div style="margin-top:8px"><a href="` + e(u) + `" style="color:` + info.Accent + `;font-size:14px;font-weight:600;text-decoration:none">Review →</a></div>`)
		}
		h.WriteString(`</div>`)
		t.WriteString("• " + title)
		if it.Subject != "" {
			t.WriteString(" — " + it.Subject)
		}
		t.WriteString("\n  " + strings.Join(meta, " · ") + "\n  " + why + "\n")
		for _, f := range flags {
			t.WriteString("  ! " + f + "\n")
		}
		if u := s.link("/outbox?open=" + it.ID); u != "" {
			t.WriteString("  Review: " + u + "\n")
		}
		t.WriteString("\n")
	}
	if len(items) > len(listed) {
		more := fmt.Sprintf("…and %d more.", len(items)-len(listed))
		h.WriteString(`<p style="font-size:14px;color:#4b5563">` + e(more) + `</p>`)
		t.WriteString(more + "\n\n")
	}
	if u := s.link("/outbox"); u != "" {
		h.WriteString(`<p style="margin:18px 0 6px"><a href="` + e(u) + `" style="display:inline-block;background:` + info.Accent + `;color:#ffffff;padding:12px 22px;border-radius:8px;font-weight:600;font-size:15px;text-decoration:none">Review all</a></p>`)
		t.WriteString("Review all: " + u + "\n\n")
	}
	note := "You're getting this because you approve client messages for " + info.Name + ". Choose who gets these emails in Settings → Who gets emails → Messages waiting for approval."
	h.WriteString(`<p style="font-size:12px;color:#9ca3af;margin:18px 0 0;line-height:1.5">` + e(note) + `</p></td></tr></table></td></tr></table></body></html>`)
	t.WriteString(note + "\n")
	return mail.Message{Subject: subject, HTML: h.String(), Text: t.String()}
}
