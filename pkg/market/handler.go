package market

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handler serves /market endpoints.
type Handler struct {
	Svc *Service
	// DefaultBase returns the base currency for an organisation (e.g. its invoicing currency).
	DefaultBase func(org string) string
}

// RegisterRoutes mounts GET /overview on the group.
func (h *Handler) RegisterRoutes(g *gin.RouterGroup) {
	g.GET("/overview", h.Overview)
}

// Overview returns market data. Query: base (3-letter currency, defaults to
// the organisation's invoicing currency, else AUD). Always 200.
func (h *Handler) Overview(c *gin.Context) {
	org := ""
	if v, ok := c.Get("org_id"); ok && v != nil {
		org = fmt.Sprint(v)
	}
	def := "AUD"
	if h.DefaultBase != nil && org != "" {
		def = NormalizeCurrency(h.DefaultBase(org), "AUD")
	}
	base := NormalizeCurrency(c.Query("base"), def)
	c.Header("Cache-Control", "private, max-age=60")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.Svc.Overview(c.Request.Context(), base)})
}
