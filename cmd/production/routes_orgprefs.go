package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Organisation regional preferences (country, home currency, time zone):
// GET/PUT /api/v1/org/prefs. pkg/orgprefs is the single source every module
// reads its default currency from.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		(&orgprefs.Handler{DB: db}).RegisterRoutes(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := orgprefs.Migrate(db); err != nil {
			log.Printf("orgprefs migrate: %v", err)
			return
		}
		// Records that gained a currency column keep the currency they were
		// made in: the organisation's currency at the time of this upgrade.
		for _, t := range []string{"pos_transactions", "bills"} {
			if !db.Migrator().HasColumn(t, "currency") {
				continue
			}
			res := db.Exec(`UPDATE ` + t + ` x SET currency = o.currency FROM organizations o
				WHERE o.id = x.organization_id AND COALESCE(x.currency,'') = '' AND COALESCE(o.currency,'') <> ''`)
			if res.Error != nil {
				log.Printf("orgprefs: backfill %s.currency: %v", t, res.Error)
			} else if res.RowsAffected > 0 {
				log.Printf("orgprefs: set currency on %d existing %s row(s)", res.RowsAffected, t)
			}
		}
	})
}
