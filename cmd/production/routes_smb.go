package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/smallbiz"
)

// Small business "Business hub": cash book, reminders, profile and reports.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		smallbiz.NewHandler(db).RegisterRoutes(auth.Group("/smallbiz"))
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := smallbiz.Migrate(db); err != nil {
			log.Printf("⚠️  smallbiz migration failed: %v", err)
		}
	})
}
