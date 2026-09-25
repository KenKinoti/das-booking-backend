package orgprefs

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Handler serves GET/PUT /api/v1/org/prefs for the signed-in organisation.
type Handler struct{ DB *gorm.DB }

// RegisterRoutes mounts the routes on the authenticated group.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	auth.GET("/org/prefs", h.get)
	auth.PUT("/org/prefs", h.put)
}

// CanEdit reports whether a role may change organisation-wide settings.
func CanEdit(role string) bool {
	switch role {
	case "super_admin", "admin", "owner", "manager":
		return true
	}
	return false
}

func (h *Handler) payload(c *gin.Context, p Prefs) gin.H {
	return gin.H{
		"country_code": p.CountryCode, "country_name": p.CountryName, "currency": p.Currency,
		"currency_decimals": p.Decimals, "timezone": p.Timezone, "source": p.Source,
		"needs_setup": p.NeedsSetup, "can_edit": CanEdit(c.GetString("user_role")),
	}
}

func (h *Handler) get(c *gin.Context) {
	org := c.GetString("org_id")
	if org == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "Organisation not found in token"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.payload(c, Get(h.DB, org))})
}

func (h *Handler) put(c *gin.Context) {
	org := c.GetString("org_id")
	if org == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "Organisation not found in token"})
		return
	}
	if !CanEdit(c.GetString("user_role")) {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "Only admins and managers can change where the business is based", "message": "Only admins and managers can change where the business is based"})
		return
	}
	var in Input
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Invalid request body"})
		return
	}
	p, err := Set(h.DB, org, in, SourceUser)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error(), "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.payload(c, p), "message": "Saved"})
}
