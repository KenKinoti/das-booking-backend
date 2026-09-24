package main

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/analytics"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/market"
)

// Dashboard analytics (/api/v1/analytics/overview) and live market data
// (/api/v1/market/overview). No tables of their own: analytics aggregates the
// existing invoicing, POS, booking, customer and product tables.
func init() {
	marketSvc := market.NewServiceFromEnv()
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		analytics.NewHandler(db).RegisterRoutes(auth.Group("/analytics"))
		mh := &market.Handler{Svc: marketSvc, DefaultBase: func(org string) string { return analytics.Currency(db, org) }}
		mh.RegisterRoutes(auth.Group("/market"))
	})
}
