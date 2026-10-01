package main

import (
	"fmt"
	"log"
	"sync"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/platform"
)

// Business types (mechanic, salon, clinic, …): modules shown per type,
// terminology, role templates with module × action permissions and data
// scope, and the first-run setup wizard. See pkg/industry.
//
//	GET  /api/v1/industry/me            type, terms, effective modules, role, permissions
//	GET  /api/v1/industry/types         registry (super admins edit with PUT/POST)
//	PUT  /api/v1/industry/org/type      change business type (admins)
//	PUT  /api/v1/industry/org/extras    "show extra modules" (admins)
//	PUT  /api/v1/industry/org/setup     wizard progress (admins)
//	/api/v1/industry/roles, /assignments  roles & who has them (admins)
//
// Enforcement runs inside the plans enforcer (routes_plans.go) right after
// the plan check: 403 MODULE_HIDDEN_FOR_TYPE / ROLE_FORBIDDEN.

var (
	industryOnce sync.Once
	industrySvc  *industry.Service
)

func industryService(db *gorm.DB) *industry.Service {
	industryOnce.Do(func() {
		industrySvc = industry.NewService(db)
		plansSvc := plansService(db)
		industrySvc.PlanModules = func(org string) (map[string]bool, error) {
			a, err := plansSvc.AccessFor(org)
			if err != nil {
				return nil, err
			}
			return a.Modules, nil
		}
	})
	return industrySvc
}

func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		svc := industryService(db)
		(&industry.Handler{Svc: svc}).RegisterRoutes(auth)

		handlers.RoleTemplateLegacy = func(org, ref string) (string, bool) {
			roles, err := svc.Roles(org)
			if err != nil {
				return "", false
			}
			for _, r := range roles {
				if r.ID == ref || r.Key == ref {
					return industry.LegacyRoleForBase(r.Base, ""), true
				}
			}
			return "", false
		}
		handlers.AssignRoleTemplate = func(org, user, ref, by string) error {
			_, _, err := svc.Assign(org, user, ref, by)
			return err
		}
		handlers.ClearRoleTemplate = svc.Unassign
		platform.OrgCreated = func(org, businessType, by string) {
			if err := svc.OnOrgCreated(org, businessType, by); err != nil {
				log.Printf("industry: new organisation %s: %v", org, err)
			}
		}
		platform.OrgTypeChanged = func(org, businessType, by string) {
			key := industry.TypeKeyFor(businessType)
			if t := svc.Type(key); t.Key != key {
				key = "general"
			}
			if _, err := svc.SetType(org, key, by); err != nil {
				log.Printf("industry: change type of %s: %v", org, err)
			}
		}
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := industry.Migrate(db); err != nil {
			log.Printf("industry migrate: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "industry", err))
		}
		industryService(db).Invalidate("")
	})
}
