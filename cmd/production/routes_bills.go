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
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
)

// Recurring bills + bill reminders (/api/v1/finance/recurring…,
// /api/v1/finance/bill-reminders…), staff booking emails
// (/api/v1/booking-emails…) and booking calendar files / feeds
// (/api/v1/bookings/:id/calendar.ics, /api/v1/public/bookings/:id/calendar.ics,
// /api/v1/public/calendar/<token>.ics).
//
// Env: PUBLIC_APP_URL (links in emails), PUBLIC_API_URL (logo URL and the
// public calendar links in emails), JWT_SECRET (signs the calendar links),
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
		// Calendar links: signed with the server secret, served from this API.
		n.APIURL, n.Secret = branding.BaseURL(nil), []byte(cfg.JWTSecret)
		ind := industryService(db)
		n.Terms = func(org string) map[string]string {
			st, err := ind.Settings(org)
			if err != nil || !st.Confirmed {
				return industry.DefaultTerms()
			}
			return industry.Terms(ind.Type(st.TypeKey))
		}
		bookingmail.Default = n
		bh := &bookingmail.Handler{N: n}
		bh.RegisterRoutes(auth)
		bh.RegisterCalendarRoutes(auth, public)
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
