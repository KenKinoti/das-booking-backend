package main

import (
	"fmt"
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mpos"
)

// Fast sale on any device (pkg/mpos): quick picks learned from each person's
// sales, sales by person and device for managers, "Link this barcode" and
// photos on sales. All under /pos, so the plan rule for the POS module and
// the business-type / role guard apply first.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		ind := industryService(db)
		h := &mpos.Handler{DB: db, Base: func(org, user, legacy string) string {
			r, err := ind.RoleFor(org, user, legacy)
			if err != nil || r == nil {
				return ""
			}
			return r.Base
		}}
		h.Register(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := mpos.Migrate(db); err != nil {
			log.Printf("mpos migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "mpos", err))
		}
	})
}
