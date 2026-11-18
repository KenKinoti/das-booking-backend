package scout

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// Scheduled scans run in the organisation's time zone: a light scan every
// day at DailyAt (default 06:30, so the results are in before an 8 am brief)
// and, on WeeklyDay (default Monday), the deep scan instead. A day's slot is
// claimed with a conditional update (last_slot), so with several instances
// exactly one runs it; a server that was down at 06:30 catches up later the
// same day. SCOUT_SCHEDULER=off disables the background loop.

// ReminderHour is when (local time) reminders are sent on their day.
const ReminderHour = 8

func atToday(local time.Time, hhmm string) time.Time {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		t, _ = time.Parse("15:04", "06:30")
	}
	return time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, local.Location())
}

// DueKind says which scan is due at now (organisation time zone loc) and the
// local date of that slot; "" when none.
func DueKind(st Settings, now time.Time, loc *time.Location) (kind, slot string) {
	if !st.Enabled {
		return "", ""
	}
	local := now.In(loc)
	day := local.Format("2006-01-02")
	if st.LastSlot == day || local.Before(atToday(local, st.DailyAt)) {
		return "", ""
	}
	switch {
	case st.WeeklyEnabled && int(local.Weekday()) == st.WeeklyDay:
		return KindDeep, day
	case st.DailyEnabled:
		return KindLight, day
	}
	return "", ""
}

// NextRuns lists the next daily and weekly scans (a slot that is overdue
// today runs at the next check, shown as now).
func NextRuns(st Settings, now time.Time, loc *time.Location) []NextRun {
	out := []NextRun{}
	local := now.In(loc)
	gotLight, gotDeep := !st.DailyEnabled, !st.WeeklyEnabled
	for i := 0; i < 8 && !(gotLight && gotDeep); i++ {
		d := local.AddDate(0, 0, i)
		if st.LastSlot == d.Format("2006-01-02") {
			continue
		}
		at := atToday(d, st.DailyAt)
		if i == 0 && at.Before(local) {
			at = local
		}
		switch {
		case st.WeeklyEnabled && int(d.Weekday()) == st.WeeklyDay:
			if !gotDeep {
				out = append(out, NextRun{Kind: KindDeep, At: at})
				gotDeep = true
			}
		case st.DailyEnabled:
			if !gotLight {
				out = append(out, NextRun{Kind: KindLight, At: at})
				gotLight = true
			}
		}
	}
	return out
}

// Due is one organisation's scheduled work done by RunScheduled.
type Due struct {
	Org    string `json:"org"`
	Kind   string `json:"kind"`
	Slot   string `json:"slot"`
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// TickResult is what one scheduler pass did.
type TickResult struct {
	Scans     []Due `json:"scans"`
	Reminders int   `json:"reminders"`
	Expired   int   `json:"expired"`
}

// RunScheduled does the due work once: scans, reminders, expiry. wait makes
// it wait for the scans it started (tests).
func (s *Service) RunScheduled(ctx context.Context, wait bool) TickResult {
	res := TickResult{Scans: []Due{}}
	var rows []Settings
	s.DB.Where("enabled").Find(&rows)
	for _, st := range rows {
		if ctx.Err() != nil {
			return res
		}
		org := st.OrganizationID
		loc := s.Loc(org)
		kind, slot := DueKind(st, s.now(), loc)
		if kind == "" {
			continue
		}
		// Claim the slot (exactly one instance runs it).
		claim := s.DB.Model(&Settings{}).Where("organization_id = ? AND enabled AND COALESCE(last_slot,'') <> ?", org, slot).Update("last_slot", slot)
		if claim.Error != nil || claim.RowsAffected == 0 {
			continue
		}
		d := Due{Org: org, Kind: kind, Slot: slot}
		run, err := s.Start(org, StartOpts{Kind: kind, Trigger: "schedule", ByName: "Schedule"})
		if err != nil {
			d.Error = err.Error()
			log.Printf("scout: scheduled %s scan for %s: %v", kind, org, err)
		} else {
			d.RunID, d.Status = run.ID, run.Status
		}
		res.Scans = append(res.Scans, d)
	}
	if wait {
		s.Wait()
		for i := range res.Scans {
			if res.Scans[i].RunID != "" {
				if r, err := s.GetRun(res.Scans[i].Org, res.Scans[i].RunID); err == nil {
					res.Scans[i].Status = r.Status
				}
			}
		}
	}
	res.Reminders = s.sendReminders()
	res.Expired = s.expireAll()
	return res
}

// sendReminders notifies the people who asked to be reminded, at 08:00 on
// the day in the organisation's time zone.
func (s *Service) sendReminders() int {
	var due []Opportunity
	// remind_on is a local date: anything up to tomorrow (UTC) may be due somewhere.
	s.DB.Where("remind_on IS NOT NULL AND reminded_at IS NULL AND remind_on <= ?", s.now().UTC().AddDate(0, 0, 1).Format("2006-01-02")).Limit(500).Find(&due)
	n := 0
	for _, o := range due {
		loc := s.Loc(o.OrganizationID)
		local := s.now().In(loc)
		if localDay(local, loc).Before(*o.RemindOn) || local.Hour() < ReminderHour {
			continue
		}
		res := s.DB.Model(&Opportunity{}).Where("id = ? AND reminded_at IS NULL", o.ID).Update("reminded_at", s.now())
		if res.RowsAffected == 0 || o.RemindUserID == "" {
			continue
		}
		body := TypeLabel(o.Type)
		if o.Deadline != nil {
			days := int(o.Deadline.Sub(localDay(local, loc)).Hours() / 24)
			body += fmt.Sprintf(" · deadline %s (%s)", o.Deadline.Format("2 Jan"), daysText(days))
		}
		if o.NextStep != "" {
			body += " · Next step: " + clip(o.NextStep, 160)
		}
		notify.Go(s.DB, o.OrganizationID, notify.Users(o.RemindUserID), notify.Message{Type: "scout.reminder", Severity: notify.Info,
			Title: "Reminder: " + clip(o.Title, 200), Body: body, Link: CardPath(o.ID), EntityType: "scout_opportunity", EntityID: o.ID})
		n++
	}
	return n
}

func daysText(d int) string {
	switch {
	case d < 0:
		return "passed"
	case d == 0:
		return "today"
	case d == 1:
		return "tomorrow"
	}
	return fmt.Sprintf("in %d days", d)
}

func (s *Service) expireAll() int {
	var orgs []string
	s.DB.Model(&Opportunity{}).Where("status IN ? AND deadline IS NOT NULL AND deadline < ?", []string{StatusNew, StatusSaved}, s.now().UTC().AddDate(0, 0, 1).Format("2006-01-02")).
		Distinct("organization_id").Pluck("organization_id", &orgs)
	for _, org := range orgs {
		s.expire(org)
	}
	return len(orgs)
}

// StartScheduler checks every 5 minutes for due work.
func (s *Service) StartScheduler(ctx context.Context) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SCOUT_SCHEDULER")), "off") {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(75 * time.Second):
		}
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("scout: scheduled run failed: %v", r)
					}
				}()
				s.RunScheduled(ctx, false)
			}()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}
