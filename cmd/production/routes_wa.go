package main

import (
	"fmt"
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// WhatsApp Cloud API: delivery-status / inbound webhook (/public/whatsapp/webhook),
// sender status and template list for settings pages. Senders use pkg/wa.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, _ *config.Config) {
		(&wa.Handler{DB: db}).RegisterRoutes(auth, public)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := wa.Migrate(db); err != nil {
			log.Printf("whatsapp migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "whatsapp", err))
		}
	})
}
