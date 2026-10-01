package main

import (
	"fmt"
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/sso"
)

// Single sign-on: "Sign in with Google / Microsoft" (pkg/sso).
//
//	/api/v1/public/sso/providers | /:provider/start | /:provider/callback | POST /exchange
//	/api/v1/sso/identities, /sso/:provider/link, /sso/domains, /sso/admin/providers
//
// Env: GOOGLE_OAUTH_CLIENT_ID/SECRET (fallback GOOGLE_CLIENT_ID/SECRET),
// MICROSOFT_CLIENT_ID/SECRET/TENANT, PUBLIC_API_URL (redirect URIs),
// PUBLIC_APP_URL (where the browser returns). Tests only: SSO_TEST_ISSUERS=1
// with SSO_GOOGLE_ISSUER / SSO_MICROSOFT_ISSUER.
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		core := handlers.NewHandler(db, cfg)
		h := sso.NewHandler(db)
		h.Issue = func(c *gin.Context, u *models.User) (interface{}, error) { return core.IssueSession(c, u) }
		h.UserLimit = func(org string) string {
			ps := plansService(db)
			if ps == nil {
				return ""
			}
			a, err := ps.AccessFor(org)
			if err != nil || a.MaxUsers <= 0 {
				return ""
			}
			if ps.UserCount(org) >= int64(a.MaxUsers) {
				return fmt.Sprintf("The plan allows up to %d users", a.MaxUsers)
			}
			return ""
		}
		h.RegisterPublic(public)
		h.RegisterAuth(auth)
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := sso.Migrate(db); err != nil {
			log.Printf("⚠️  sso migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "sso", err))
		}
	})
}
