package main

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/pnl"
)

// Profit & loss (/api/v1/reports/profit-loss): the one P&L calculation used by
// the dashboard, the report page, Finance, the Business hub, Analytics and the
// AI tools. No tables of its own; the link columns it relies on
// (bills.purchase_order_id, smallbiz_entries.link_type) are migrated by the
// finance and smallbiz modules.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		pnl.NewHandler(db).RegisterRoutes(auth)
	})
}
