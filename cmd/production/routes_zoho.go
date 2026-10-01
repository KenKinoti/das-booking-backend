package main

import (
	"context"
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"log"
	"os"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/zoho"
)

// Data import: Zoho Books (live OAuth connection or export files) and generic
// CSV/XLSX files from other tools.
//
//	GET/PUT/DELETE /api/v1/imports/zoho/connection    app credentials, disconnect
//	POST   /api/v1/imports/zoho/connect                → Zoho consent URL
//	GET    /api/v1/public/zoho/callback                OAuth redirect URI
//	GET    /api/v1/imports/zoho/organizations          PUT /imports/zoho/organization
//	POST   /api/v1/imports/zoho/preview                new API import job → preview
//	POST   /api/v1/imports/files                       new file import job (multipart "files")
//	GET    /api/v1/imports/jobs[/:id]                  progress / report
//	POST   /api/v1/imports/jobs/:id/{preview,start,cancel,resume,undo}
//	GET    /api/v1/imports/jobs/:id/errors[.csv]
//	GET    /api/v1/invoicing/invoices/:id/attachments[/:attachmentId]  (original PDFs)
//
// Env: PUBLIC_API_URL (redirect URI shown to the user), PUBLIC_APP_URL (where
// the browser returns after connecting); tests: ZOHO_ACCOUNTS_URL, ZOHO_API_URL.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		h := zoho.NewHandler(db)
		h.PublicApp = os.Getenv("PUBLIC_APP_URL")
		h.RegisterRoutes(auth)
		h.RegisterPublic(public)
		invoicing.RegisterAttachmentRoutes(auth.Group("/invoicing"), db)
		h.Engine.Recover(context.Background())
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := zoho.Migrate(db); err != nil {
			log.Printf("⚠️  import (zoho) migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "zoho", err))
		}
	})
}
