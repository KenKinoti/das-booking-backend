package main

import (
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/middleware"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/encryption"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/projects2"
)

// Projects module with Jira Cloud sync, mounted at /api/v1/pm (the legacy
// /api/v1/projects/* stubs in modules.go are left untouched).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		h := projects2.NewHandler(db, encryption.NewEncryptionService())
		applyJiraTestSite(h, cfg)
		h.RegisterRoutes(auth.Group("/pm"), middleware.RequireRole("admin", "owner", "manager"))
	})
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		if err := projects2.Migrate(db); err != nil {
			log.Printf("projects2 migration failed: %v", err)
			dbstatus.AddMigrationError(fmt.Sprintf("%s: %v", "projects2", err))
		}
	})
}

// applyJiraTestSite lets local/E2E environments point the Jira integration at
// a fake Jira (e.g. PM_JIRA_TEST_SITE=http://127.0.0.1:9911). Only that exact
// URL bypasses the https/private-address checks; it is ignored in production.
func applyJiraTestSite(h *projects2.Handler, cfg *config.Config) {
	site := strings.TrimRight(os.Getenv("PM_JIRA_TEST_SITE"), "/")
	if site == "" {
		return
	}
	if strings.EqualFold(os.Getenv("ENVIRONMENT"), "production") && os.Getenv("PM_JIRA_TEST_SITE_FORCE") != "1" {
		log.Printf("PM_JIRA_TEST_SITE ignored in production")
		return
	}
	u, err := url.Parse(site)
	if err != nil || u.Host == "" {
		return
	}
	log.Printf("⚠️  Jira test site enabled: %s", site)
	h.ValidateSite = func(s string) (string, error) {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(s), "/"), site) {
			return site, nil
		}
		return projects2.NormalizeSiteURL(s)
	}
	safe := projects2.SafeHTTPClient()
	plain := &http.Transport{}
	h.HTTP = &http.Client{
		Timeout:       45 * time.Second,
		CheckRedirect: safe.CheckRedirect,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Scheme == u.Scheme && r.URL.Host == u.Host {
				return plain.RoundTrip(r)
			}
			return safe.Transport.RoundTrip(r)
		}),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
