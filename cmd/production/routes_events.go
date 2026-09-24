package main

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/events"
)

// Events module: organisation-scoped management API under /api/v1/events and a
// public landing page / registration API under /api/v1/public/events.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		h := events.NewHandler(db)
		events.RegisterRoutes(auth, h)
		events.RegisterPublicRoutes(public, h)
	})
	extraMigrations = append(extraMigrations, events.Migrate)
}
