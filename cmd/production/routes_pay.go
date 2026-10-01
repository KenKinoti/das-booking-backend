package main

import (
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"log"
	"os"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/payments"
)

// Online invoice payments through Flutterwave (hosted checkout).
//
//	GET/PUT  /api/v1/payments/settings            organisation settings (admins edit)
//	POST     /api/v1/payments/settings/test       test the secret key (GET /v3/balances)
//	POST     /api/v1/payments/estimate            least-cost options for an amount
//	GET      /api/v1/invoicing/invoices/:id/online-payments
//	GET      /api/v1/super-admin/payments/status
//	GET      /api/v1/public/pay/:token            pay page data
//	POST     /api/v1/public/pay/:token/checkout   start a checkout → Flutterwave link
//	POST     /api/v1/public/pay/:token/verify     after the redirect back
//	POST     /api/v1/public/payments/flutterwave/webhook
//
// Env: FLUTTERWAVE_BASE_URL (default https://api.flutterwave.com), PUBLIC_APP_URL.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		h := payments.NewHandler(db)
		h.PublicURL = os.Getenv("PUBLIC_APP_URL")
		h.RegisterRoutes(auth)
		h.RegisterPublic(public)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := payments.Migrate(db); err != nil {
			log.Printf("⚠️  payments migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "payments", err))
		}
	})
}
