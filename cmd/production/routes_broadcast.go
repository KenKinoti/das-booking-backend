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
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/broadcast"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Announcements (Platform admin → Announcements, pkg/broadcast): super admins
// message the people who use DASYIN — updates, reminders, offers and urgent
// notices — in the app (notification centre + an urgent banner on the
// dashboard) and by email to whoever hasn't read it after 1 h (urgent) / 3 h,
// through the platform email account. Offers have a one-click unsubscribe.
// Super admins are nudged before important occasions (pkg/occasions).
//
//	/api/v1/super-admin/announcements/**   super admins (audit-logged)
//	/api/v1/announcements/me/**            everyone signed in
//	/api/v1/public/announcements/open|unsubscribe|resubscribe
//
// Env: PUBLIC_APP_URL / PUBLIC_API_URL (email links), JWT_SECRET (signs
// them), BROADCAST_SCHEDULER=off (no background runs), BROADCAST_TICK
// (default 1m), BROADCAST_TEST_HOOKS=1 (test clock + fake occasions; never in
// production; adds POST …/test-clock and …/test-occasions), BROADCAST_CLOCK_OFFSET
// (e.g. 61m: the starting test clock; only with the test hooks).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := broadcast.New(db)
		svc.AppURL = strings.TrimRight(os.Getenv("PUBLIC_APP_URL"), "/")
		svc.APIURL = strings.TrimRight(branding.BaseURL(nil), "/")
		svc.Secret = []byte(cfg.JWTSecret)
		svc.TierOf = func(org string) string {
			sub, err := plansService(db).Subscription(org)
			if err != nil || sub == nil {
				return ""
			}
			return sub.Tier
		}
		brand := branding.NewService(db)
		svc.Platform = func() broadcast.PlatformInfo {
			info := broadcast.PlatformInfo{Name: "DASYIN"}
			orgs := mail.Shared(db).PlatformOrgs()
			if len(orgs) == 0 {
				return info
			}
			var o struct{ Name, Email, Phone, Website string }
			db.Raw(`SELECT COALESCE(name,'') AS name, COALESCE(email,'') AS email, COALESCE(phone,'') AS phone, COALESCE(website,'') AS website FROM organizations WHERE id = ?`, orgs[0]).Scan(&o)
			if strings.TrimSpace(o.Name) != "" {
				info.Name = o.Name
			}
			info.Email, info.Phone, info.Website = o.Email, o.Phone, o.Website
			if u := brand.URLFor(nil, orgs[0]); strings.HasPrefix(u, "http") {
				info.LogoURL = u
			}
			return info
		}
		testHooks := os.Getenv("BROADCAST_TEST_HOOKS") == "1"
		if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv("BROADCAST_CLOCK_OFFSET"))); err == nil && testHooks {
			broadcast.SetClockOffset(d)
		}
		ind := industryService(db)
		h := &broadcast.Handler{S: svc, TestHooks: testHooks}
		h.BusinessTypes = func() []broadcast.Option {
			out := []broadcast.Option{}
			types, err := ind.Types()
			if err != nil {
				return out
			}
			for _, t := range types {
				out = append(out, broadcast.Option{Key: t.Key, Label: t.Name})
			}
			return out
		}
		h.RegisterRoutes(auth, public)
		if !strings.EqualFold(os.Getenv("BROADCAST_SCHEDULER"), "off") {
			every := time.Minute
			if d, err := time.ParseDuration(os.Getenv("BROADCAST_TICK")); err == nil && d >= 5*time.Second {
				every = d
			}
			svc.StartScheduler(context.Background(), every, 45*time.Second)
		}
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := broadcast.Migrate(db); err != nil {
			log.Printf("broadcast migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "broadcast", err))
		}
	})
}
