package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/scout"
)

// Market scout (Insights → Market scout, /api/v1/insights/scout/…): web
// searches with the platform's Anthropic key (System settings → AI
// assistant) for tenders, grants, partnerships, leads, new markets and
// products to add, ranked by fit, with a monthly US$ budget. See pkg/scout.
//
// Env: ANTHROPIC_BASE_URL (tests: a fake API), SCOUT_TEST_CLOCK=1 (test clock
// and run-due endpoints), SCOUT_SCHEDULER=off (no daily / weekly scans),
// PUBLIC_APP_URL (links in CRM notes and the morning brief).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		fx := &fxrates.Service{DB: db}
		if ps := plansService(db); ps != nil && ps.FX != nil {
			f := ps.FX
			fx.Live = f
			fx.LiveAsOf = func() *time.Time { _, t := f.Table(); return t }
		}
		svc := scout.New(db, fx)
		ind := industryService(db)
		svc.BusinessType = func(org string) string {
			st, err := ind.Settings(org)
			if err != nil || !st.Confirmed {
				return ""
			}
			return ind.Type(st.TypeKey).Name
		}
		// Pursue creates CRM records: the plan must include CRM and the
		// person's role must be allowed to create there.
		svc.CRMAllowed = func(org, user, role string) (bool, string) {
			if ps := plansService(db); ps != nil {
				if a, err := ps.AccessFor(org); err == nil && !a.Has("crm") {
					return false, "CRM is not in your plan."
				}
			}
			if a, err := ind.AccessFor(org, user, role); err == nil && !a.Can("crm", "create") {
				return false, "Your role can't add CRM deals or leads."
			}
			return true, ""
		}
		(&scout.Handler{S: svc}).RegisterRoutes(auth)
		svc.StartScheduler(context.Background())
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := scout.Migrate(db); err != nil {
			log.Printf("market scout migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "market scout", err))
		}
	})
}
