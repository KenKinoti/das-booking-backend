package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/assistant"
)

// In-app "Ask DASYIN" assistant: /api/v1/assistant/...
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := assistant.NewService(db)
		if svc.Enabled() {
			log.Printf("assistant: enabled (model %s, base %s)", svc.Cfg.Model, svc.Cfg.BaseURL)
		} else {
			log.Printf("assistant: disabled (ANTHROPIC_API_KEY not set)")
		}
		(&assistant.Handler{Svc: svc}).RegisterRoutes(auth.Group("/assistant"))
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := assistant.Migrate(db); err != nil {
			log.Printf("assistant migration failed: %v", err)
		}
	})
}
