package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orghooks"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/plans"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/subscriptions"
)

// Client subscriptions (pkg/subscriptions): customers on catalogue services
// billed monthly / quarterly / yearly, invoices created ahead and emailed
// with the pay link, dunning with admin alerts at least 3 days before a
// suspension, suspension and unsuspension at Synergy Wholesale through its
// API (or tasks for other providers), domain renewal reminders, the
// dashboard and the public "Your services" page.
//
//	/api/v1/subscriptions/…                 (plan module: invoicing; admins and managers)
//	/api/v1/public/your-services/:token     signed customer page
//
// Env: PUBLIC_APP_URL (links in emails), JWT_SECRET (in-process invoicing
// calls, signs the customer links), SYNERGY_API_URL (the Synergy SOAP
// endpoint, tests only), SUBSCRIPTIONS_SCHEDULER=off disables the
// background runs, SUBSCRIPTIONS_TEST_CLOCK=1 enables the admin-only test
// clock (test environments only).
func init() {
	// The subscriptions API belongs to the invoicing module (plan and role checks).
	plans.Rules = append(plans.Rules, plans.Rule{Prefix: "/subscriptions", Modules: []string{"invoicing"}})

	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		fx := &fxrates.Service{DB: db}
		if ps := plansService(db); ps != nil && ps.FX != nil {
			f := ps.FX
			fx.Live = f
			fx.LiveAsOf = func() *time.Time { _, t := f.Table(); return t }
		}
		brand := branding.NewService(db)
		exp := expenses.NewService(db, fx)
		svc := subscriptions.New(db, fx, exp)
		svc.AppURL = strings.TrimRight(os.Getenv("PUBLIC_APP_URL"), "/")
		svc.LogoURL = func(org string) string {
			if u := brand.URLFor(nil, org); strings.HasPrefix(u, "http") {
				return u
			}
			return ""
		}
		svc.Secret = []byte(cfg.JWTSecret)
		svc.Inv = &subscriptions.APIInvoicer{Engine: func() http.Handler { return aitools.Engine }, Secret: []byte(cfg.JWTSecret)}
		svc.Supplier = subscriptions.NewSynergyActions(exp)
		h := &subscriptions.Handler{S: svc}
		h.RegisterRoutes(auth)
		h.RegisterPublic(public)
		if err := svc.WatchPayments(context.Background()); err != nil {
			log.Printf("subscriptions: payment watcher: %v", err)
		}
		if !strings.EqualFold(os.Getenv("SUBSCRIPTIONS_SCHEDULER"), "off") {
			svc.StartScheduler(context.Background(), 10*time.Minute, 90*time.Second)
		}
		if svc.TestClock {
			log.Printf("subscriptions: TEST CLOCK enabled (SUBSCRIPTIONS_TEST_CLOCK) — never in production")
		}
		// Platform → Reset organisation: subscriptions are business records
		// (their invoices are removed too); the settings stay.
		orghooks.RegisterReset("subscriptions", subscriptions.ResetOrg)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := subscriptions.Migrate(db); err != nil {
			log.Printf("subscriptions migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "subscriptions", err))
		}
	})
}
