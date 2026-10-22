package main

import (
	"fmt"
	"log"
	"os"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/occasions"
)

// Seasonal themes: the occasions calendar (Kenyan and Australian public
// holidays, observances and shopping seasons, plus custom occasions), the
// provider other modules read (occasions.Between / occasions.ThemeOn), and
// the theme API: GET /occasions/theme, organisation and personal switches,
// and the platform admin's /super-admin/occasions/**.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := occasions.NewService(db)
		occasions.Register(svc)
		if d := os.Getenv("OCCASIONS_TODAY"); d != "" {
			log.Printf("⚠️  occasions: OCCASIONS_TODAY=%s — the occasions calendar's clock is pinned (test-only)", d)
		}
		(&occasions.Handler{DB: db, Svc: svc}).RegisterRoutes(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := occasions.Migrate(db); err != nil {
			log.Printf("occasions migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "occasions", err))
		}
	})
}
