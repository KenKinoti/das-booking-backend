package main

import (
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

// Exchange rates for documents in a customer's currency (/api/v1/fx/...):
// live market rates (shared with pkg/plans), the reference table, and each
// organisation's manual overrides. Invoicing locks the rate on each document.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := &fxrates.Service{DB: db}
		if ps := plansService(db); ps != nil && ps.FX != nil {
			fx := ps.FX
			svc.Live = fx
			svc.LiveAsOf = func() *time.Time { _, t := fx.Table(); return t }
		}
		(&fxrates.Handler{Svc: svc}).RegisterRoutes(auth)
		invoicing.FX = svc

		// Logo dimensions for the email header (width/height attributes keep
		// Outlook from stretching the image).
		bs := branding.NewService(db)
		invoicing.LogoSize = func(org string) (int, int) {
			if l, err := bs.Meta(org); err == nil && l != nil {
				return l.Width, l.Height
			}
			return 0, 0
		}
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := fxrates.Migrate(db); err != nil {
			log.Printf("fx rates migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "fxrates", err))
		}
		// Documents created before rates were locked: record their home
		// currency (and rate 1 when they are in it).
		if n := invoicing.BackfillBase(db); n > 0 {
			log.Printf("invoicing: recorded the home currency on %d document(s)", n)
		}
	})
}
