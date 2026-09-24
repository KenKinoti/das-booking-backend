package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/account"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/ecom"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/hr"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mrp"
)

// HR & payroll, manufacturing (MRP), online store connections and the
// signed-in user's own account.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		hr.NewHandler(db).RegisterRoutes(auth.Group("/hr"))
		mrp.NewHandler(db).RegisterRoutes(auth.Group("/mrp"))
		ecom.NewHandler(db).RegisterRoutes(auth.Group("/ecom"))
		account.NewHandler(db).RegisterRoutes(auth.Group("/account"))
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		for name, m := range map[string]func(*gorm.DB) error{"hr": hr.Migrate, "mrp": mrp.Migrate, "ecom": ecom.Migrate} {
			if err := m(db); err != nil {
				log.Printf("⚠️  %s migration failed: %v", name, err)
			}
		}
	})
}
