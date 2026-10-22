package broadcast

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/occasions"
)

// LookaheadDays is how far ahead the occasions are read (at least the
// furthest nudge).
const LookaheadDays = 21

// Draft is a ready-to-edit announcement for an occasion.
type Draft struct {
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Kind        string   `json:"kind"`
	Priority    string   `json:"priority"`
	CTALabel    string   `json:"cta_label"`
	CTAURL      string   `json:"cta_url"`
	Audience    Audience `json:"audience"`
	Timezone    string   `json:"timezone"`
	LocalSendAt string   `json:"local_send_at"`
}

// Upcoming is one occasion on the Announcements page ("Coming up").
type Upcoming struct {
	occasions.Occasion
	DateText  string        `json:"date_text"` // "2026-10-20"
	DaysUntil int           `json:"days_until"`
	Skipped   bool          `json:"skipped"`
	Nudged    []int         `json:"nudged"` // lead days already nudged
	Planned   *Announcement `json:"planned"`
	Draft     Draft         `json:"draft"`
}

// calendar reads the occasions between two dates, one per key and day: the
// calendar may list a day twice (Christmas as a public holiday in KE/AU and
// as a worldwide observance); the public holiday wins — it carries the
// countries and the holiday template — and Important on either counts.
func (s *Service) calendar(from, to time.Time) []occasions.Occasion {
	out := []occasions.Occasion{}
	at := map[string]int{}
	for _, o := range s.occasionsBetween(from, to, "") {
		k := o.Key + "|" + dateOnly(o.Date).Format(DateLayout)
		i, seen := at[k]
		if !seen {
			at[k] = len(out)
			out = append(out, o)
			continue
		}
		important := out[i].Important || o.Important
		if o.Kind == occasions.KindPublicHoliday && out[i].Kind != occasions.KindPublicHoliday {
			out[i] = o
		}
		out[i].Important = important
	}
	return out
}

// today is the date in the team's time zone.
func (s *Service) today(st Settings) time.Time {
	loc, err := LoadZone(st.Timezone)
	if err != nil {
		loc = time.UTC
	}
	t := Now().In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// ComingUp lists the occasions of the next days (important ones, unless
// all), with what has been planned or nudged for each.
func (s *Service) ComingUp(days int, all bool) []Upcoming {
	st := s.GetSettings()
	if days <= 0 {
		days = LookaheadDays
	}
	if st.NudgeLeadDays[0] > days {
		days = st.NudgeLeadDays[0]
	}
	today := s.today(st)
	out := []Upcoming{}
	for _, o := range s.calendar(today, today.AddDate(0, 0, days)) {
		if !o.Important && !all {
			continue
		}
		u := Upcoming{Occasion: o, DateText: dateOnly(o.Date).Format(DateLayout), DaysUntil: DaysUntil(today, o.Date), Nudged: []int{}}
		var n int64
		fresh(s.DB).Model(&Skip{}).Where("occasion_key = ? AND year = ?", o.Key, o.Date.Year()).Count(&n)
		u.Skipped = n > 0
		fresh(s.DB).Model(&Nudge{}).Where("occasion_key = ? AND occasion_date = ?", o.Key, u.DateText).Order("lead_days DESC").Pluck("lead_days", &u.Nudged)
		u.Planned = s.plannedFor(o.Key, u.DateText)
		u.Draft = s.DraftFor(o, st)
		out = append(out, u)
	}
	return out
}

// plannedFor is the newest live announcement planned for an occasion.
func (s *Service) plannedFor(key, date string) *Announcement {
	var rows []Announcement
	fresh(s.DB).Where("occasion_key = ? AND occasion_date = ? AND status <> ?", key, date, StatusCancelled).Order("created_at DESC").Limit(1).Find(&rows)
	if len(rows) == 1 {
		return &rows[0]
	}
	return nil
}

// FindOccasion looks an occasion up by key and date in the calendar.
func (s *Service) FindOccasion(key, date string) (occasions.Occasion, bool) {
	d, err := time.Parse(DateLayout, date)
	if err != nil {
		return occasions.Occasion{}, false
	}
	for _, o := range s.calendar(d, d) {
		if o.Key == key {
			return o, true
		}
	}
	return occasions.Occasion{}, false
}

// SetSkip marks (or clears) "skip this year" for an occasion.
func (s *Service) SetSkip(key string, year int, skip bool, by, byName string) error {
	if !skip {
		return fresh(s.DB).Where("occasion_key = ? AND year = ?", key, year).Delete(&Skip{}).Error
	}
	return fresh(s.DB).Clauses(clause.OnConflict{DoNothing: true}).Create(&Skip{OccasionKey: key, Year: year, By: by, ByName: byName, CreatedAt: Now()}).Error
}

// ---------------------------------------------------------------- drafts

func countriesText(cs []string) string {
	names := map[string]string{"KE": "Kenya", "AU": "Australia", "TZ": "Tanzania", "UG": "Uganda", "RW": "Rwanda", "NZ": "New Zealand", "GB": "the UK", "US": "the US"}
	out := []string{}
	for _, c := range cs {
		if n, ok := names[strings.ToUpper(c)]; ok {
			out = append(out, n)
		} else {
			out = append(out, strings.ToUpper(c))
		}
	}
	return strings.Join(out, ", ")
}

// DraftFor is the template message for an occasion: a greeting and a
// practical reminder for public holidays, a customer idea for observances,
// an upsell for shopping seasons — scheduled for the day at the send hour.
func (s *Service) DraftFor(o occasions.Occasion, st Settings) Draft {
	day := dateOnly(o.Date)
	weekday := day.Weekday().String()
	name := o.Name
	d := Draft{Kind: KindUpdate, Priority: PriorityNormal, Timezone: st.Timezone,
		LocalSendAt: fmt.Sprintf("%sT%02d:00", day.Format(DateLayout), st.SendHour),
		Audience:    Audience{Countries: append([]string{}, o.Countries...), RoleLevel: LevelManagers, ExcludeSuperAdmins: true}}
	if len(d.Audience.Countries) == 1 {
		switch strings.ToUpper(d.Audience.Countries[0]) {
		case "AU":
			d.Timezone = "Australia/Sydney"
		case "KE":
			d.Timezone = "Africa/Nairobi"
		}
	}
	switch o.Kind {
	case occasions.KindPublicHoliday:
		d.Kind = KindReminder
		d.Title = "Happy " + name + " from DASYIN"
		greet := "<p>Happy " + escape(name) + "! From all of us at DASYIN, thank you for running your business with us.</p>"
		switch o.Key {
		case "mashujaa_day":
			greet = "<p>Happy Mashujaa Day! Today Kenya honours its heroes — and we'd like to honour the everyday heroes running businesses like yours. Thank you for building with DASYIN.</p>"
		case "jamhuri_day":
			greet = "<p>Happy Jamhuri Day! As Kenya celebrates its independence, thank you for growing your business with DASYIN.</p>"
		case "madaraka_day":
			greet = "<p>Happy Madaraka Day! Thank you for running your business with DASYIN — here's to self-rule and self-made businesses.</p>"
		case "australia_day":
			greet = "<p>Happy Australia Day! Thanks for running your business with DASYIN.</p>"
		case "anzac_day":
			d.Title = "Anzac Day — trading hours reminder"
			greet = "<p>On Anzac Day we pause to remember. Trading hours are restricted in many states until 1 pm, so please check your state's rules.</p>"
		case "christmas_day", "christmas":
			d.Title = "Merry Christmas from DASYIN"
			greet = "<p>Merry Christmas! Thank you for a great year with DASYIN — we wish you and your team a restful festive season.</p>"
		case "new_years_day", "new_year":
			d.Title = "Happy New Year from DASYIN"
			greet = "<p>Happy New Year! Thank you for running your business with DASYIN. Here's to a busy and profitable year ahead.</p>"
		}
		d.Body = greet + "<p>If you're open on " + weekday + ", a quick check:</p>" +
			"<ul><li>Update your opening hours and online booking times</li><li>Confirm who is on shift</li><li>Let your customers know — a short message goes a long way</li></ul>" +
			"<p>Enjoy the day.</p>"
		d.CTALabel, d.CTAURL = "Check opening hours", "/settings"
	case occasions.KindCommercial:
		d.Kind = KindPromotion
		d.Title = "Get ready for " + name
		d.Body = "<p>" + escape(name) + " is one of the busiest sales days of the year. Make the most of it with DASYIN:</p>" +
			"<ul><li><strong>Discounts and promotions</strong> at the till and online</li><li><strong>Low-stock alerts</strong> so best-sellers don't run out</li><li><strong>Sales reports</strong> by the hour, so you know what's working</li></ul>" +
			"<p>Need more modules or users for the rush? Upgrade your plan in a few clicks.</p>"
		d.CTALabel, d.CTAURL = "See plans", "/plan"
		d.Audience.RoleLevel = LevelOwners
		// Shopping seasons are worth a heads-up a few days before.
		d.LocalSendAt = fmt.Sprintf("%sT%02d:00", day.AddDate(0, 0, -5).Format(DateLayout), st.SendHour)
	case occasions.KindObservance:
		d.Kind = KindUpdate
		d.Title = name + " is coming up"
		d.Body = "<p>" + escape(name) + " is on " + weekday + " " + day.Format("2 January") + ". It's a good moment to reach out to your customers — a themed offer, a thank-you note or a special event.</p>" +
			"<ul><li>Set up a promotion at the till</li><li>Send your customers a short message</li><li>Plan your stock and staff for the day</li></ul>"
		d.CTALabel, d.CTAURL = "Open DASYIN", "/dashboard"
		d.Audience.RoleLevel = LevelOwners
		d.LocalSendAt = fmt.Sprintf("%sT%02d:00", day.AddDate(0, 0, -3).Format(DateLayout), st.SendHour)
	default:
		d.Title = name
		d.Body = "<p>" + escape(name) + " is on " + weekday + " " + day.Format("2 January") + ".</p><p>From all of us at DASYIN — thank you for running your business with us.</p>"
	}
	// A send time that has passed becomes "in an hour".
	if t, err := ParseLocal(d.LocalSendAt, d.Timezone); err == nil && t.Before(Now()) {
		d.LocalSendAt = FormatLocal(Now().Add(time.Hour).Truncate(15*time.Minute), d.Timezone)
	}
	return d
}

// ---------------------------------------------------------------- nudging

func whenText(days int) string {
	switch days {
	case 0:
		return "is today"
	case 1:
		return "is tomorrow"
	}
	return fmt.Sprintf("is in %d days", days)
}

// RunNudges nudges every super admin (in the app and by email) before the
// important occasions of the next weeks, once per occasion and lead time:
// with leads 14 and 3, an occasion 10 days away gets the 14-day nudge and,
// later, the 3-day one. Skipped occasions get none.
func (s *Service) RunNudges(ctx context.Context) int {
	st := s.GetSettings()
	if !st.NudgesOn {
		return 0
	}
	leads := st.NudgeLeadDays // descending
	today := s.today(st)
	ahead := LookaheadDays
	if leads[0] > ahead {
		ahead = leads[0]
	}
	sent := 0
	for _, o := range s.calendar(today, today.AddDate(0, 0, ahead)) {
		if ctx.Err() != nil {
			break
		}
		if !o.Important || strings.TrimSpace(o.Key) == "" {
			continue
		}
		days := DaysUntil(today, o.Date)
		if days < 0 {
			continue
		}
		lead := -1
		for i := len(leads) - 1; i >= 0; i-- { // smallest lead that covers it
			if days <= leads[i] {
				lead = leads[i]
				break
			}
		}
		if lead < 0 {
			continue
		}
		var skipped int64
		fresh(s.DB).Model(&Skip{}).Where("occasion_key = ? AND year = ?", o.Key, o.Date.Year()).Count(&skipped)
		if skipped > 0 {
			continue
		}
		date := dateOnly(o.Date).Format(DateLayout)
		n := Nudge{ID: uuid.NewString(), OccasionKey: o.Key, OccasionDate: date, LeadDays: lead, OccasionName: clip(o.Name, 160), SentAt: Now()}
		res := fresh(s.DB).Clauses(clause.OnConflict{DoNothing: true}).Create(&n)
		if res.Error != nil || res.RowsAffected == 0 {
			continue // already nudged for this lead time
		}
		inApp, emailed := s.nudge(ctx, o, days, st)
		fresh(s.DB).Model(&Nudge{}).Where("id = ?", n.ID).Updates(map[string]any{"recipients": inApp, "emailed": emailed})
		sent++
	}
	return sent
}

// NudgeText is the title and body of a nudge.
func (s *Service) NudgeText(o occasions.Occasion, days int, st Settings) (title, body, link string) {
	date := dateOnly(o.Date).Format(DateLayout)
	title = fmt.Sprintf("%s %s — plan a message", o.Name, whenText(days))
	link = "/super-admin/announcements?occasion=" + o.Key + "&date=" + date
	d := s.DraftFor(o, st)
	when := ""
	if t, err := ParseLocal(d.LocalSendAt, d.Timezone); err == nil {
		when = t.In(mustZone(d.Timezone)).Format("Mon 2 Jan, 15:04")
	}
	if p := s.plannedFor(o.Key, date); p != nil {
		switch p.Status {
		case StatusDraft:
			title = fmt.Sprintf("%s %s — finish your message", o.Name, whenText(days))
			body = fmt.Sprintf("Your draft “%s” is waiting. Review it and schedule it.", p.Title)
		case StatusScheduled, StatusSending:
			title = fmt.Sprintf("%s %s — your message is scheduled", o.Name, whenText(days))
			at := ""
			if p.NextSendAt != nil {
				at = " for " + p.NextSendAt.In(mustZone(p.Timezone)).Format("Mon 2 Jan, 15:04")
			}
			body = fmt.Sprintf("“%s” is scheduled%s. Review or update it.", p.Title, at)
		default:
			body = fmt.Sprintf("“%s” was sent. Open it to see how it did.", p.Title)
		}
		return
	}
	body = fmt.Sprintf("A ready draft is waiting: “%s”. Adjust it, or schedule it for %s in one click.", d.Title, when)
	if o.Note != "" {
		body += " Note: " + o.Note
	}
	return
}

func mustZone(tz string) *time.Location {
	if l, err := LoadZone(tz); err == nil {
		return l
	}
	return time.UTC
}

func (s *Service) nudge(ctx context.Context, o occasions.Occasion, days int, st Settings) (inApp, emailed int) {
	title, body, link := s.NudgeText(o, days, st)
	for _, sa := range s.SuperAdmins() {
		if sa.OrgID != "" {
			if n, err := notify.Send(s.DB, sa.OrgID, notify.Users(sa.UserID), notify.Message{Type: "broadcast.nudge", Severity: notify.Warning,
				Title: title, Body: body, Link: link, EntityType: "occasion", EntityID: o.Key + ":" + dateOnly(o.Date).Format(DateLayout), ActorName: "DASYIN"}); err == nil {
				inApp += n
			}
		}
		if !st.NudgeEmail || sa.Email == "" {
			continue
		}
		info := s.platformInfo()
		v := invoicing.EmailView{Kind: "reminder", Accent: "#4f46e5", DocLabel: "Coming up", Business: invoicing.EmailBusiness{Name: info.Name, Email: info.Email},
			LogoURL: info.LogoURL, Preheader: body,
			Message: "Hi " + firstName(sa.Name) + ",\n\n" + title + ".\n\n" + body + "\n\nPlan it in Platform admin → Announcements.",
			Actions: []invoicing.EmailAction{{Label: "Plan the message", URL: s.appURL() + link, Primary: true}},
			Note:    "You get these reminders because you are a DASYIN super admin. Mark an occasion “Skip this year” to stop them for it."}
		htmlOut, text := invoicing.RenderEmail(v)
		if err := s.sendMail(ctx, mail.Message{To: []string{sa.Email}, Subject: title, HTML: htmlOut, Text: text}); err != nil {
			log.Printf("broadcast: nudge email to %s: %v", sa.Email, err)
			continue
		}
		emailed++
	}
	return
}

func firstName(n string) string {
	if f := strings.Fields(n); len(f) > 0 && !strings.Contains(f[0], "@") {
		return f[0]
	}
	return "there"
}
