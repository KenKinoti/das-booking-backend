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
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/insights"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
)

// Insights 2.10: industry watch (/api/v1/insights/watch…), trade & exports
// (/api/v1/insights/trade…) and the strategy brief (/api/v1/insights/briefs…).
// See pkg/insights (watch_*.go, trade*.go, brief.go).
//
// Env: INDUSTRY_WATCH_BASE_URL (tests only: fetch every feed from a local fake),
// INSIGHTS_SCHEDULER=off (no daily feed refresh / monthly brief),
// WORLDBANK_BASE_URL, PUBLIC_APP_URL (link in the brief email).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		fx := &fxrates.Service{DB: db}
		if ps := plansService(db); ps != nil && ps.FX != nil {
			f := ps.FX
			fx.Live = f
			fx.LiveAsOf = func() *time.Time { _, t := f.Table(); return t }
		}
		exp := expenses.NewService(db, fx)
		bills := recurbills.New(db, fx, exp)
		svc := insights.New(db, fx, exp, bills, insights.NewWorldBankFromEnv(db))
		intel := insights.NewIntel(svc, insights.NewFetcherFromEnv())
		ind := industryService(db)
		intel.BusinessType = func(org string) (string, string) {
			st, err := ind.Settings(org)
			if err != nil || !st.Confirmed {
				return "", ""
			}
			t := ind.Type(st.TypeKey)
			return t.Key, t.Name
		}
		intel.Mail = mail.Shared(db)
		intel.AppURL = strings.TrimRight(os.Getenv("PUBLIC_APP_URL"), "/")
		brand := branding.NewService(db)
		intel.LogoURL = func(org string) string {
			if u := brand.URLFor(nil, org); strings.HasPrefix(u, "http") {
				return u
			}
			return ""
		}
		(&insights.IntelHandler{I: intel}).RegisterRoutes(auth)
		intel.StartScheduler(context.Background())
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := insights.MigrateIntel(db); err != nil {
			log.Printf("insights (watch / trade / briefs) migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "insights watch", err))
		}
	})
}
