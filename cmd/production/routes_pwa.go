package main

import (
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify/hooks"
)

// In-app notification centre (the bell in the top bar):
//
//	GET    /api/v1/notifications            list + unread count
//	GET    /api/v1/notifications/unread-count
//	POST   /api/v1/notifications/read-all
//	POST   /api/v1/notifications/:id/read
//	DELETE /api/v1/notifications/:id
//	DELETE /api/v1/notifications?scope=read|all
//
// Modules call notify.Send/notify.Go; invoicing and online payments are
// observed through hooks (gorm callback on billing_activities and
// payments.OnTransaction) so those packages stay untouched.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		notify.NewHandler(db).RegisterRoutes(auth)
		if err := hooks.RegisterBilling(db); err != nil {
			log.Printf("⚠️  notification hooks: %v", err)
		}
		go func() {
			// Housekeeping: read notifications go after 60 days, all after 180.
			// Migrations run in the background after the routes are registered,
			// so wait for the table instead of logging "relation does not exist".
			time.Sleep(time.Minute)
			for {
				if db.Migrator().HasTable(&notify.Notification{}) {
					notify.Prune(db, 60*24*time.Hour, 180*24*time.Hour)
				}
				time.Sleep(12 * time.Hour)
			}
		}()
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := notify.Migrate(db); err != nil {
			log.Printf("⚠️  notifications migration failed: %v", err)
		}
	})
}
