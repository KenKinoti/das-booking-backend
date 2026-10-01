package main

import (
	"fmt"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/insights"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
)

// Insights (/api/v1/insights/...): dashboard business snapshot, internal
// opportunities, World Bank market indicators (WORLDBANK_BASE_URL overrides
// https://api.worldbank.org), uploaded datasets and CSV fact-table exports;
// plus the analyst views under /api/v1/analytics/... (revenue, clients,
// payments, seasonality, service-lines, hosting, service-map).
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
		(&insights.Handler{S: svc}).RegisterRoutes(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := insights.Migrate(db); err != nil {
			log.Printf("insights migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "insights", err))
		}
	})
}
