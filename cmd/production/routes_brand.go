package main

import (
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/events"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
)

// Company logo: one logo per organisation (org_logos), used on invoices,
// quotes, receipts, emails, public pages and the sidebar.
//
//	PUT/POST /api/v1/organization/logo   upload (admins)
//	DELETE   /api/v1/organization/logo   remove (admins)
//	GET      /api/v1/organization/branding  (core handler → branding.Service.Info)
//	GET      /api/v1/public/org-logo/:orgId  the image (ETag, cacheable)
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		h := branding.NewHandler(db)
		h.RegisterRoutes(auth)
		h.RegisterPublic(public)

		// Every business block (invoice settings, public invoice, emails)
		// uses the organisation logo.
		invoicing.LogoURL = func(r *http.Request, org string) string { return h.Svc.URLFor(r, org) }
		events.OrgLogoURL = func(r *http.Request, org string) string { return h.Svc.URLFor(r, org) }
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := branding.Migrate(db); err != nil {
			log.Printf("⚠️  branding migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "branding", err))
			return
		}
		if n, err := branding.Backfill(db); err != nil {
			log.Printf("⚠️  logo backfill failed: %v", err)
		} else if n > 0 {
			log.Printf("🖼️  moved %d invoicing logo(s) into company logos", n)
		}
	})
}
