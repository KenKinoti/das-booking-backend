package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
)

// GET /api/v1/super-admin/db-status — start-up migration problems in detail.
func init() {
	extraRoutes = append(extraRoutes, func(auth, _ *gin.RouterGroup, _ *gorm.DB, _ *config.Config) {
		auth.GET("/super-admin/db-status", func(c *gin.Context) {
			if c.GetString("user_role") != "super_admin" {
				c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "Super admin only"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"success": true, "data": dbstatus.Get(true)})
		})
	})
}
