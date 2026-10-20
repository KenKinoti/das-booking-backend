package main

import (
	"fmt"
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/onboarding"
)

// "Getting started": basic steps on how to use DASYIN from each person's
// point of view — their role and their business type (the steps live with
// the role templates in pkg/industry/defaults.go). See pkg/onboarding.
//
//	GET  /api/v1/onboarding/me             {role_name, type_name, steps:[{key,title,description,to,done,auto}], welcomed, dismissed, percent, …}
//	POST /api/v1/onboarding/me/welcomed    the welcome sheet was seen
//	POST /api/v1/onboarding/me/steps/:key  {done} tick / untick a step
//	POST /api/v1/onboarding/me/dismiss     hide ({dismissed:false} shows it again)
//	POST /api/v1/onboarding/me/restart     start over
//
// A super admin viewing an organisation gets its owner's steps read-only
// (preview: true). An organisation reset for handover clears everyone's
// progress (orghooks reset hook "onboarding").

func init() {
	onboarding.RegisterResetHook()
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := onboarding.NewService(db, industryService(db))
		(&onboarding.Handler{Svc: svc, JWTSecret: cfg.JWTSecret}).RegisterRoutes(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := onboarding.Migrate(db); err != nil {
			log.Printf("onboarding migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "onboarding", err))
		}
	})
}
