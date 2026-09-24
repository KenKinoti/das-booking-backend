package main

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
)

// Finance routes are registered by finance.Handler.RegisterRoutes in main.go;
// this file only adds the schema clean-up for older finance tables.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) { finance.MigrateLegacy(db) })
}
