package main

import (
	"context"
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
)

// Expenses (/api/v1/expenses/...): Synergy Wholesale (SOAP API), Gmail
// billing emails (Google Workspace, Synergy, custom senders), uploads; an
// inbox to review and approve, which creates paid supplier bills counted once
// by the P&L; subscriptions, renewals, alerts, forecast and margin per client.
// Public: GET /api/v1/public/google/callback (Gmail OAuth redirect).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		fx := &fxrates.Service{DB: db}
		if ps := plansService(db); ps != nil && ps.FX != nil {
			f := ps.FX
			fx.Live = f
			fx.LiveAsOf = func() *time.Time { _, t := f.Table(); return t }
		}
		svc := expenses.NewService(db, fx)
		(&expenses.Handler{S: svc}).RegisterRoutes(auth, public)
		svc.StartScheduler(context.Background())
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := expenses.Migrate(db); err != nil {
			log.Printf("expenses migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "expenses", err))
		}
	})
}
