package main

import (
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/meetings"
)

func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		meetings.NewHandler(db).RegisterRoutes(auth.Group("/meetings"))
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := meetings.Migrate(db); err != nil {
			log.Printf("meetings migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "meetings", err))
		}
	})
}
