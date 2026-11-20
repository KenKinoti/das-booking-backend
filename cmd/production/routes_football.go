package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/football"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Football match analysis & bet tracker (/api/v1/football/…, pkg/football):
// fixtures, results, standings, players, injuries, line-ups and odds from
// API-Football v3 and The Odds API v4 (platform keys in System settings →
// Football data, super admins: /api/v1/super-admin/football/…), Dixon–Coles
// and Elo predictions frozen before kickoff and graded after full time, and
// a personal bet tracker with limits, breaks, bankroll and CLV. DASYIN never
// takes bets or money.
//
// The plans rule for /football (module "football") lives in
// pkg/plans/middleware.go; the module is opt-in (pkg/industry
// OptInModules): the Nightclub / bar / lounge type and the platform
// organisation have it, owners and managers by default.
//
// Env: FOOTBALL_APIFOOTBALL_BASE, FOOTBALL_ODDSAPI_BASE (tests: the fakes),
// FOOTBALL_TEST_CLOCK=1 (test clock endpoint), FOOTBALL_SCHEDULER=off (no
// background fetching; manual refresh still works).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := football.New(db)
		svc.OrgCurrency = func(org string) string { return orgprefs.Currency(db, org) }
		svc.OrgZone = func(org string) *time.Location {
			tz := strings.TrimSpace(orgprefs.Get(db, org).Timezone)
			if tz == "" {
				tz = "Africa/Nairobi"
			}
			l, err := time.LoadLocation(tz)
			if err != nil {
				return nil
			}
			return l
		}
		ind := industryService(db)
		svc.Can = func(org, user, role, act string) bool {
			a, err := ind.AccessFor(org, user, role)
			if err != nil {
				return role == "super_admin" || role == "admin" || role == "manager"
			}
			return a.Can("football", act)
		}
		(&football.Handler{S: svc}).RegisterRoutes(auth)
		svc.StartScheduler(context.Background())
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := football.Migrate(db); err != nil {
			log.Printf("football migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "football", err))
		}
	})
}
