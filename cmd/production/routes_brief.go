package main

import (
	"context"
	"errors"
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
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/projects2"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/subscriptions"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// jiraHandler is the Projects module's handler (set in routes_jira.go): the
// brief syncs Jira through it so a manual sync and the brief share its lock.
var jiraHandler *projects2.Handler

// Morning brief (pkg/brief): each person's daily note — bills & payables,
// money in, projects (Jira + local), opportunities and today — by email and
// WhatsApp at their time (default 08:00, their / the organisation's zone).
//
//	/api/v1/brief/**                    signed-in people (their own brief)
//	/api/v1/super-admin/brief/config    the WhatsApp template (super admins)
//
// Env: PUBLIC_APP_URL (links), BRIEF_SCHEDULER=off (no background sends),
// BRIEF_TICK (default 1m), BRIEF_TEST_CLOCK=1 (test clock endpoints; never in
// production).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		fx := &fxrates.Service{DB: db}
		if ps := plansService(db); ps != nil && ps.FX != nil {
			f := ps.FX
			fx.Live = f
			fx.LiveAsOf = func() *time.Time { _, t := f.Table(); return t }
		}
		svc := brief.New(db, fx)
		svc.AppURL = strings.TrimRight(os.Getenv("PUBLIC_APP_URL"), "/")
		ind := industryService(db)
		svc.Access = func(org, user, role string) (industry.Access, error) { return ind.AccessFor(org, user, role) }
		brand := branding.NewService(db)
		svc.LogoURL = func(org string) string {
			if u := brand.URLFor(nil, org); strings.HasPrefix(u, "http") {
				return u
			}
			return ""
		}
		svc.JiraSync = func(ctx context.Context, org string) error {
			if jiraHandler == nil {
				return errors.New("projects module not loaded")
			}
			_, err := jiraHandler.SyncOrg(ctx, org)
			return err
		}
		svc.TestClock = os.Getenv("BRIEF_TEST_CLOCK") == "1"
		if svc.TestClock {
			log.Printf("brief: TEST CLOCK enabled (BRIEF_TEST_CLOCK) — never in production")
		}
		(&brief.Handler{S: svc}).RegisterRoutes(auth)
		wa.OnStatus(svc.OnWhatsAppStatus)
		// No duplicate emails: people whose brief lists bills / client billing
		// are skipped by the separate automatic digests (unless they asked for both).
		recurbills.BriefCovers = func(org string) map[string]bool { return svc.Covered(org, "bills") }
		subscriptions.BriefCovers = func(org string) map[string]bool { return svc.Covered(org, "money") }
		if !strings.EqualFold(os.Getenv("BRIEF_SCHEDULER"), "off") {
			every := time.Minute
			if d, err := time.ParseDuration(os.Getenv("BRIEF_TICK")); err == nil && d >= 5*time.Second {
				every = d
			}
			svc.StartScheduler(context.Background(), every, 40*time.Second)
		}
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := brief.Migrate(db); err != nil {
			log.Printf("brief migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "brief", err))
		}
	})
}
