package main

import (
	"fmt"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/pricebook"
)

// Supplier prices (/api/v1/finance/supplier-prices/...): price lists of
// wholesale suppliers, a self-hosted server and competitors; CSV import /
// export; the Synergy auto-fill (plan prices learned from the statements in
// pkg/expenses, domain prices from the Synergy API); the self-hosted cost
// model; the domain + hosting combo comparison in the supplier's and the
// home currency (pkg/fxrates) and the retail pricing helper that saves
// catalog services. Under /finance, so the Accounting module's plan and role
// checks apply; admins and managers only.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		fx := &fxrates.Service{DB: db}
		if ps := plansService(db); ps != nil && ps.FX != nil {
			f := ps.FX
			fx.Live = f
			fx.LiveAsOf = func() *time.Time { _, t := f.Table(); return t }
		}
		(&pricebook.Handler{S: pricebook.New(db, fx)}).RegisterRoutes(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := pricebook.Migrate(db); err != nil {
			log.Printf("pricebook migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "pricebook", err))
		}
	})
}
