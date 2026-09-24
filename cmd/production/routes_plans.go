package main

import (
	"context"
	"log"
	"sync"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/plans"
)

// Subscription plans: tiers, industry bundles, multi-currency quotes and
// module entitlements (see pkg/plans).

var (
	plansOnce sync.Once
	plansSvc  *plans.Service
)

func plansService(db *gorm.DB) *plans.Service {
	plansOnce.Do(func() {
		fx := plans.NewFX()
		fx.Start(context.Background())
		plansSvc = plans.NewService(db, fx)
	})
	return plansSvc
}

// plansEnforcer blocks API calls to modules outside the organisation's plan.
// It is installed on the engine before any route is registered so it covers
// the core API (internal/handlers), the module groups and messaging/video.
func plansEnforcer(db *gorm.DB, cfg *config.Config) gin.HandlerFunc {
	svc := plansService(db)
	e := &plans.Enforcer{
		Lookup:   svc.AccessFor,
		Identify: plans.TokenIdentity(cfg.JWTSecret),
		Users:    svc.UserCount,
	}
	return e.Middleware()
}

func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		h := &plans.Handler{Svc: plansService(db)}
		h.RegisterPublic(public)
		h.RegisterAuth(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := plans.Migrate(db); err != nil {
			log.Printf("plans migrate: %v", err)
		}
	})
}
