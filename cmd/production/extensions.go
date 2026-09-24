package main

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
)

// RouteRegistrar lets feature modules add routes without editing main.go.
// auth is /api/v1 behind JWT auth (org_id, user_id, user_role in context);
// public is /api/v1/public (no auth).
type RouteRegistrar func(auth *gin.RouterGroup, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config)

var (
	// extraRoutes is appended to from init() in routes_*.go files.
	extraRoutes []RouteRegistrar
	// extraMigrations run after the core migrations (log, don't panic, on error).
	extraMigrations []func(db *gorm.DB)
)
