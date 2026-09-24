package main

import (
	"context"
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mcp"
)

// MCP server for AI clients (Claude and others): POST /mcp (Streamable HTTP),
// OAuth 2.1 (/oauth/*, /.well-known/*) and the settings API under
// /api/v1/mcp and /api/v1/oauth. Tools come from pkg/aitools.
var mcpServer *mcp.Server

func mcpFor(db *gorm.DB) *mcp.Server {
	if mcpServer == nil {
		mcpServer = mcp.New(db)
		mcpServer.StartCleanup(context.Background())
	}
	return mcpServer
}

func init() {
	extraRootRoutes = append(extraRootRoutes, func(r *gin.Engine, db *gorm.DB, cfg *config.Config) {
		mcpFor(db).RegisterRoot(r)
	})
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		mcpFor(db).RegisterAPI(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := mcp.Migrate(db); err != nil {
			log.Printf("mcp migrate: %v", err)
		}
	})
}
