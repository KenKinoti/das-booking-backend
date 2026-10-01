package main

import (
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/platform"
)

// Platform admin (super admin) API under /api/v1/super-admin/platform:
// cross-organisation stats, organisation & user management, audit feed,
// database health and platform integration settings. Super admins only.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		platform.NewHandler(db, cfg).RegisterRoutes(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := platform.Migrate(db); err != nil {
			log.Printf("platform migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "platform", err))
		}
	})
}
