package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/seccall"
)

// Secure calls (pkg/seccall): end-to-end encrypted call links.
// Host: /api/v1/secure-calls…; super admin: /api/v1/secure-call-settings…;
// guests: /api/v1/public/calls/:id/… (Postgres-backed relay, works across
// Cloud Run instances).
//
// Env: PUBLIC_APP_URL (the link base), SECCALL_CLEANUP=off disables the
// cleanup loop on this instance, SECCALL_CLEANUP_EVERY (default 15s).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := seccall.New(db)
		svc.AppURL = strings.TrimRight(os.Getenv("PUBLIC_APP_URL"), "/")
		h := &seccall.Handler{S: svc}
		h.RegisterAuth(auth)
		h.RegisterPublic(public)
		ctx := context.Background()
		svc.StartListener(ctx)
		if !strings.EqualFold(os.Getenv("SECCALL_CLEANUP"), "off") {
			every := 15 * time.Second
			if d, err := time.ParseDuration(os.Getenv("SECCALL_CLEANUP_EVERY")); err == nil && d >= time.Second {
				every = d
			}
			svc.StartScheduler(ctx, every, 20*time.Second)
		}
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := seccall.Migrate(db); err != nil {
			log.Printf("secure calls migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("secure calls: %v", err))
		}
	})
}
