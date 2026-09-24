package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/middleware"
)

// Extra routes for customers/services/staff/scheduling/documents/CRM pages.
// (Core routes live in internal/handlers/handler.go; CRM routes in pkg/crm.)
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		h := handlers.NewHandler(db, cfg)
		auth.POST("/users/:id/reset-password", middleware.RequireRole("admin"), h.ResetUserPassword)
		auth.GET("/documents-stats", h.GetDocumentStats)
	})

	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		// Document bytes live in the database (Cloud Run disks are ephemeral).
		if err := db.AutoMigrate(&handlers.DocumentBlob{}); err != nil {
			log.Printf("ui1 migrate document_blobs: %v", err)
		}
		// vehicles.vin is globally unique; empty strings would collide, so
		// "no VIN" is stored as NULL.
		if err := db.Exec("UPDATE vehicles SET vin = NULL WHERE vin = ''").Error; err != nil {
			log.Printf("ui1 vin cleanup: %v", err)
		}
		// Opportunities now link to the core customers table.
		db.Exec("ALTER TABLE opportunities DROP CONSTRAINT IF EXISTS fk_opportunities_customer")
	})
}
