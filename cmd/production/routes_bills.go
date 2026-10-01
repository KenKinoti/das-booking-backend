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
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/bookingmail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
)

// Recurring bills + bill reminders (/api/v1/finance/recurring…,
// /api/v1/finance/bill-reminders…) and staff booking emails
// (/api/v1/booking-emails…).
//
// Env: PUBLIC_APP_URL (links in emails), PUBLIC_API_URL (logo URL),
// RECURRING_BILLS_SCHEDULER=off and BOOKING_AGENDA_SCHEDULER=off disable the
// background runs.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		fx := &fxrates.Service{DB: db}
		if ps := plansService(db); ps != nil && ps.FX != nil {
			f := ps.FX
			fx.Live = f
			fx.LiveAsOf = func() *time.Time { _, t := f.Table(); return t }
		}
		brand := branding.NewService(db)
		logo := func(org string) string {
			u := brand.URLFor(nil, org)
			if strings.HasPrefix(u, "http") {
				return u
			}
			return ""
		}
		appURL := strings.TrimRight(os.Getenv("PUBLIC_APP_URL"), "/")

		svc := recurbills.New(db, fx, expenses.NewService(db, fx))
		svc.AppURL, svc.LogoURL = appURL, logo
		(&recurbills.Handler{S: svc}).RegisterRoutes(auth)
		if !strings.EqualFold(os.Getenv("RECURRING_BILLS_SCHEDULER"), "off") {
			svc.StartScheduler(context.Background(), 10*time.Minute, 60*time.Second)
		}

		n := bookingmail.New(db)
		n.AppURL, n.LogoURL = appURL, logo
		bookingmail.Default = n
		(&bookingmail.Handler{N: n}).RegisterRoutes(auth)
		if !strings.EqualFold(os.Getenv("BOOKING_AGENDA_SCHEDULER"), "off") {
			n.StartScheduler(context.Background())
		}
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		for name, m := range map[string]func(*gorm.DB) error{"recurring bills": recurbills.Migrate, "booking emails": bookingmail.Migrate} {
			if err := m(db); err != nil {
				log.Printf("%s migration failed: %v", name, err)
				dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", name, err))
			}
		}
	})
}
