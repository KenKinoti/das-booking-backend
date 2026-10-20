package main

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/platform"
)

// Reset organisation for handover (platform admins only, pkg/platform/orgreset.go):
//
//	POST /api/v1/super-admin/platform/organizations/:id/reset   dry run / reset
//	GET  /api/v1/super-admin/platform/organizations/:id/export  backup ZIP
//
// (registered by platform.RegisterRoutes) and the same two under
// /api/v1/super-admin/organizations/:id/… . After a reset the cached
// business-type state of the organisation is dropped so its admin gets the
// setup wizard straight away.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		h := platform.NewHandler(db, cfg)
		g := auth.Group("/super-admin/organizations", platform.RequireSuperAdmin())
		g.POST("/:id/reset", h.ResetOrganization)
		g.GET("/:id/export", h.ExportOrganization)

		platform.OrgResetDone = append(platform.OrgResetDone, func(org string) {
			industryService(db).Invalidate(org)
		})
	})
}
