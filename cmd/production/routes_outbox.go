package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/brief"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
)

// Pending review (pkg/outbox, Communication → Pending review): while an
// organisation reviews client messages (the default), no email, SMS or
// WhatsApp to a client leaves DASYIN without an owner's or admin's approval.
// Invoicing, schedule send, subscriptions, meetings and messaging ask the gate
// and register how their waiting messages are sent, previewed and re-checked;
// approvers edit, approve (now / for the scheduled time), reject or do it in
// bulk; the engine releases approved items, supersedes ones no longer needed,
// marks stale ones and emails the reviewers (≈15 minutes after messages start
// waiting, at most hourly, and a daily summary at 07:30 organisation time).
//
//	/api/v1/outbox/items[/:id[/approve|reject|retry]]   list, detail, edit, decide
//	/api/v1/outbox/bulk | /counts | /settings            bulk decide, badge, settings
//
// Env: PUBLIC_APP_URL (links in the update emails), OUTBOX_ENGINE=off (no
// background job here), OUTBOX_TICK (default 1m), OUTBOX_TEST_CLOCK=1 (test
// clock + run endpoints; never in production), OUTBOX_DEFAULT=off
// (organisations without a saved setting start with review off — tests and
// other agents' flows only; never in production).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := outbox.New(db)
		svc.AppURL = strings.TrimRight(os.Getenv("PUBLIC_APP_URL"), "/")
		brand := branding.NewService(db)
		svc.LogoURL = func(org string) string {
			if u := brand.URLFor(nil, org); strings.HasPrefix(u, "http") {
				return u
			}
			return ""
		}
		ind := industryService(db)
		// outbox.approve: the business-type role (base admin, or an explicit grant).
		outbox.RoleOf = func(org, user, legacy string) (string, map[string][]string, bool) {
			r, err := ind.RoleFor(org, user, legacy)
			if err != nil || r == nil {
				return "", nil, false
			}
			return r.Base, r.Perms, true
		}
		// Update emails follow "Who gets emails" (category pending_review).
		outbox.EmailsFor = func(db *gorm.DB, org, category string, ids []string) []string {
			return emailprefs.UserEmails(db, org, category, ids)
		}
		if svc.TestClock {
			log.Printf("outbox: TEST CLOCK enabled (OUTBOX_TEST_CLOCK) — never in production")
		}
		if svc.DefaultOff {
			log.Printf("outbox: OUTBOX_DEFAULT=off — organisations without a saved setting don't review client messages (tests only)")
		}
		outbox.Install(svc)
		(&outbox.Handler{S: svc}).RegisterRoutes(auth)
		if !strings.EqualFold(os.Getenv("OUTBOX_ENGINE"), "off") {
			every := time.Minute
			if d, err := time.ParseDuration(os.Getenv("OUTBOX_TICK")); err == nil && d >= 5*time.Second {
				every = d
			}
			go func() {
				// Start once the tables exist (migrations run in the background).
				for i := 0; i < 300; i++ {
					if db.Migrator().HasTable("outbox_items") {
						svc.Start(context.Background(), every, 30*time.Second)
						return
					}
					time.Sleep(2 * time.Second)
				}
				log.Printf("outbox: outbox_items is still missing — the Pending review job starts with the next server start")
			}()
		}
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := outbox.Migrate(db); err != nil {
			log.Printf("outbox migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "outbox", err))
		}
	})

	// "Waiting for your approval" in the morning brief (approvers only).
	brief.Register("outbox", "Waiting for your approval", 5, func(ctx context.Context, in *brief.Input) (*brief.Section, error) {
		svc := outbox.Default()
		if svc == nil || in.Viewer == nil {
			return nil, nil
		}
		a := outbox.Actor{Org: in.Org, User: in.Viewer.UserID, Role: in.Viewer.Role}
		if !svc.CanApprove(a.Org, a.User, a.Role) {
			return nil, nil
		}
		rows, total, _ := svc.List(a, outbox.Query{Tab: "waiting", Sort: "oldest", PerPage: 5})
		if total == 0 {
			return nil, nil
		}
		sec := &brief.Section{Key: "outbox", Title: "Waiting for your approval", Link: "/outbox"}
		items := make([]brief.Item, 0, len(rows))
		for _, r := range rows {
			title := r.KindLabel
			if r.Record != nil && r.Record.Number != "" {
				title += " " + r.Record.Number
			}
			to := r.ToName
			if r.Customer != nil && r.Customer.Name != "" {
				to = r.Customer.Name
			}
			title += " to " + firstNonBlank(to, r.To)
			ch := map[string]string{"email": "Email", "sms": "SMS", "whatsapp": "WhatsApp"}[r.Channel]
			detail := ch + " · waiting " + waitedFor(in.Now.Sub(r.CreatedAt)) + " · " + r.Trigger
			it := brief.Item{Key: r.ID, Title: title, Detail: detail, Link: r.Link, Tone: "info"}
			if r.Amount != nil {
				it.Amount, it.Currency = *r.Amount, r.Currency
				it.Value = in.Money(*r.Amount, r.Currency)
			}
			switch {
			case r.Blocks == "suspension" || r.TimeSensitive:
				it.Tag, it.Tone = "Time-sensitive", "danger"
			case r.Stale:
				it.Tag, it.Tone = "Stale", "warn"
			case r.Status == "failed":
				it.Tag, it.Tone = "Failed", "danger"
			}
			items = append(items, it)
		}
		sec.Add("waiting", "Client messages waiting for approval", items, int(total), "/outbox")
		if total == 1 {
			sec.Say(5, "1 message waiting for your approval")
		} else {
			sec.Say(5, "%d messages waiting for your approval", total)
		}
		return sec, nil
	}, brief.Icon("fa-solid fa-envelope-circle-check"),
		brief.Describe("Client emails, SMS and WhatsApp messages waiting in Pending review, oldest first, with stale and time-sensitive ones flagged."),
		brief.Requires(func(v *brief.Viewer) bool {
			svc := outbox.Default()
			return svc != nil && svc.CanApprove(v.OrgID, v.UserID, v.Role)
		}, "For owners and admins (who approve client messages)"))
}

func firstNonBlank(v ...string) string {
	for _, x := range v {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

func waitedFor(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}
