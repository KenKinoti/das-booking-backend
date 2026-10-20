package handlers

import (
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
)

// DefaultAppVersion is the release reported by /health when APP_VERSION is not set.
const DefaultAppVersion = "2.13.0"

// AppVersion returns the running release (env APP_VERSION, default DefaultAppVersion).
func AppVersion() string {
	if v := os.Getenv("APP_VERSION"); v != "" {
		return v
	}
	return DefaultAppVersion
}

func (h *Handler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":   "ok",
		"message":  "DASYIN ERP API is running",
		"service":  "DASYIN ERP Backend",
		"version":  AppVersion(),
		"database": dbstatus.Get(false),
	})
}
