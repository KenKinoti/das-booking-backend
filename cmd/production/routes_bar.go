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
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/barstock"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Club / bar stock (/api/v1/bar/**, pkg/barstock): stock items on top of
// products, stores and bars (inventory_locations) with transfers, deliveries
// with weighted average cost, wastage, stock takes and variance, reorders into
// purchase orders, reports and the optional night report email.
//
// The plans rule for /bar (module inventory, pos read-also) lives in
// pkg/plans/middleware.go, so the plan, business type and role guard apply.
// POS sales / voids and purchase order receipts call bar stock through the
// hook variables in internal/handlers (pos.go, suppliers.go).
// BAR_SCHEDULER=off disables the night report scheduler.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := barstock.New(db)
		ind := industryService(db)
		svc.Can = func(org, user, role, module, act string) bool {
			a, err := ind.AccessFor(org, user, role)
			if err != nil {
				return barLegacyCan(role, act) // fail open like the role guard, but by legacy role
			}
			return a.Can(module, act)
		}
		svc.NextPONumber = handlers.NextPONumber
		svc.Mail = mail.Shared(db)
		svc.AppURL = strings.TrimRight(os.Getenv("PUBLIC_APP_URL"), "/")
		brand := branding.NewService(db)
		svc.LogoURL = func(org string) string {
			if u := brand.URLFor(nil, org); strings.HasPrefix(u, "http") {
				return u
			}
			return ""
		}
		(&barstock.Handler{S: svc}).RegisterRoutes(auth)

		// POS and purchase order hooks.
		handlers.BarManagedProducts = svc.ManagedProducts
		handlers.BarSaleHook = func(tx *gorm.DB, s handlers.BarSale) ([]string, error) {
			res, err := svc.OnSale(tx, s.OrgID, s.UserID, s.LocationID, s.Txn, s.Items)
			return res.Touched, err
		}
		handlers.BarVoidHook = func(tx *gorm.DB, org, user string, t *models.POSTransaction, reason string) (map[string]bool, error) {
			res, err := svc.OnVoid(tx, org, user, t, reason)
			return res.Handled, err
		}
		handlers.BarPOReceiveHook = svc.OnPOReceive

		if !strings.EqualFold(os.Getenv("BAR_SCHEDULER"), "off") {
			svc.StartScheduler(context.Background(), 10*time.Minute, 90*time.Second)
		}
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := barstock.Migrate(db); err != nil {
			log.Printf("bar stock migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "barstock", err))
		}
	})
}

// barLegacyCan is the fallback when business-type access can't be loaded.
func barLegacyCan(role, act string) bool {
	switch strings.ToLower(role) {
	case "super_admin", "admin", "owner", "manager":
		return true
	}
	return act == "view" || act == "create"
}
