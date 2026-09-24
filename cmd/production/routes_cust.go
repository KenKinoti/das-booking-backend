package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Customer contacts (CC recipients for invoices, quotes, reminders and
// bookings) under /api/v1/customers/:id/contacts, the country list, and the
// customer country_code backfill.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		handlers.NewHandler(db, cfg).RegisterCustomerContactRoutes(auth)
		auth.GET("/reference/countries", func(c *gin.Context) {
			type row struct {
				currency.Country
				Currency string `json:"currency,omitempty"`
			}
			out := make([]row, 0, len(currency.Countries))
			for _, ct := range currency.Countries {
				cur, _ := currency.CurrencyForCountry(ct.Code)
				out = append(out, row{ct, cur})
			}
			c.JSON(200, gin.H{"countries": out})
		})
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := db.AutoMigrate(&models.CustomerContact{}); err != nil {
			log.Printf("customer contacts migrate: %v", err)
			return
		}
		if n, err := handlers.BackfillCustomerCountries(db); err != nil {
			log.Printf("customer country backfill: %v", err)
		} else if n > 0 {
			log.Printf("customer country backfill: %d customers", n)
		}
	})
}
