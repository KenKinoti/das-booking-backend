package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// Outgoing email settings (organisation / platform default) and status.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		mail.NewHandler(db).RegisterRoutes(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := mail.Migrate(db); err != nil {
			log.Printf("mail migration failed: %v", err)
		}
	})
}
