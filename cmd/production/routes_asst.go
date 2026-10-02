package main

import (
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/assistant"
)

// In-app "Ask DASYIN" assistant: /api/v1/assistant/... and the super-admin
// AI settings: /api/v1/super-admin/ai/settings.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		// Every AI feature (Ask DASYIN, meeting summaries, expense extraction,
		// market explanations) resolves the Anthropic key through this
		// database: saved in System settings → AI assistant, else environment.
		assistant.Use(db)
		svc := assistant.NewService(db)
		if cfg := svc.Config(); cfg.Enabled() {
			log.Printf("assistant: enabled (key from %s, model %s, base %s)", cfg.Source, cfg.Model, cfg.BaseURL)
		} else {
			log.Printf("assistant: no Anthropic API key yet (add one in System settings → AI assistant, or set ANTHROPIC_API_KEY)")
		}
		h := &assistant.Handler{Svc: svc}
		h.RegisterRoutes(auth.Group("/assistant"))
		// Super admin: the platform's Anthropic key and model.
		h.RegisterAdminRoutes(auth.Group("/super-admin/ai"))
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := assistant.Migrate(db); err != nil {
			log.Printf("assistant migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "assistant", err))
		}
	})
}
