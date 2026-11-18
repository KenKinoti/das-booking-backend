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
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/schedsend"
)

// Schedule send (pkg/schedsend, Communication → Scheduled): invoice / quote /
// receipt emails and payment reminders (through the invoicing send path),
// customer messages by email, SMS or WhatsApp to one customer, chosen
// customers or a group, recurring messages (statements of account, check-ins)
// and team messages — at a time in the creator's time zone or on a schedule.
// AI / MCP tools: scheduled_list, schedule_message.
//
//	/api/v1/schedsend/**                            signed in (roles checked per kind)
//	/api/v1/public/schedsend/unsubscribe|open/…     customers' links
//	/api/v1/public/sms/africastalking/delivery      Africa's Talking delivery reports
//	/api/v1/public/sms/twilio/status                Twilio status callbacks (signed)
//
// Env: PUBLIC_APP_URL (pay links), PUBLIC_API_URL (unsubscribe / open links,
// SMS callbacks), JWT_SECRET (in-process invoicing calls, signs links),
// SCHEDSEND_SCHEDULER=off (no background sends), SCHEDSEND_TICK (default 1m),
// SCHEDSEND_GAP (pause between bulk sends, default 150ms), SCHEDSEND_TEST_CLOCK=1
// (test clock + run endpoints; never in production), SMS_TWILIO_API_BASE /
// SMS_AFRICASTALKING_API_BASE (test fakes only).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := schedsend.New(db)
		svc.AppURL = strings.TrimRight(os.Getenv("PUBLIC_APP_URL"), "/")
		svc.APIURL = strings.TrimRight(branding.BaseURL(nil), "/")
		svc.Secret = []byte(cfg.JWTSecret)
		svc.Invoices = &schedsend.APIInvoices{Engine: func() http.Handler { return aitools.Engine }, Secret: []byte(cfg.JWTSecret)}
		brand := branding.NewService(db)
		svc.LogoURL = func(org string) string {
			if u := brand.URLFor(nil, org); strings.HasPrefix(u, "http") {
				return u
			}
			return ""
		}
		ind := industryService(db)
		svc.Can = func(org, user, role, module, act string) bool {
			a, err := ind.AccessFor(org, user, role)
			if err != nil {
				return barLegacyCan(role, act)
			}
			return a.Can(module, act)
		}
		if d, err := time.ParseDuration(os.Getenv("SCHEDSEND_GAP")); err == nil && d >= 0 {
			svc.Gap = d
		}
		testHooks := os.Getenv("SCHEDSEND_TEST_CLOCK") == "1"
		if testHooks {
			log.Printf("schedsend: TEST CLOCK enabled (SCHEDSEND_TEST_CLOCK) — never in production")
		}
		(&schedsend.Handler{S: svc, TestHooks: testHooks}).RegisterRoutes(auth, public)
		if !strings.EqualFold(os.Getenv("SCHEDSEND_SCHEDULER"), "off") {
			every := time.Minute
			if d, err := time.ParseDuration(os.Getenv("SCHEDSEND_TICK")); err == nil && d >= 5*time.Second {
				every = d
			}
			svc.Start(context.Background(), every, 40*time.Second)
		}
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := schedsend.Migrate(db); err != nil {
			log.Printf("schedsend migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "schedsend", err))
		}
	})
}
