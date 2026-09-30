package fxrates

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Handler serves /api/v1/fx/...
//
//	GET    /fx/rate?from=KES&to=USD      one quote (manual → live → fallback)
//	GET    /fx/rates?base=KES            every supported currency vs base
//	GET    /fx/manual-rates              the organisation's manual rates
//	PUT    /fx/manual-rates              {from, to, rate, note} create / replace (admins)
//	DELETE /fx/manual-rates/:id          remove (admins)
type Handler struct{ Svc *Service }

// RegisterRoutes mounts the routes on the authenticated group.
func (h *Handler) RegisterRoutes(g *gin.RouterGroup) {
	g.GET("/fx/rate", h.rate)
	g.GET("/fx/rates", h.rates)
	g.GET("/fx/manual-rates", h.listManual)
	g.PUT("/fx/manual-rates", h.saveManual)
	g.POST("/fx/manual-rates", h.saveManual)
	g.DELETE("/fx/manual-rates/:id", h.deleteManual)
}

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func org(c *gin.Context) (string, bool) {
	o := c.GetString("org_id")
	if o == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
		return "", false
	}
	return o, true
}

func (h *Handler) rate(c *gin.Context) {
	o, ok := org(c)
	if !ok {
		return
	}
	from, to := norm(c.Query("from")), norm(c.Query("to"))
	base := orgprefs.Currency(h.Svc.DB, o)
	if from == "" {
		from = base
	}
	if to == "" {
		to = base
	}
	if !currency.IsSupported(from) || !currency.IsSupported(to) {
		fail(c, http.StatusBadRequest, "UNSUPPORTED_CURRENCY", "Choose supported currencies")
		return
	}
	q := h.Svc.Quote(o, from, to)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"quote": q, "base_currency": base, "can_edit": orgprefs.CanEdit(c.GetString("user_role"))}})
}

func (h *Handler) rates(c *gin.Context) {
	o, ok := org(c)
	if !ok {
		return
	}
	base := norm(c.Query("base"))
	if base == "" || !currency.IsSupported(base) {
		base = orgprefs.Currency(h.Svc.DB, o)
	}
	out := []Quote{}
	for _, cur := range currency.Supported {
		if cur == base {
			continue
		}
		out = append(out, h.Svc.Quote(o, base, cur))
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"base_currency": base, "rates": out, "manual": h.Svc.ListManual(o), "can_edit": orgprefs.CanEdit(c.GetString("user_role"))}})
}

func (h *Handler) listManual(c *gin.Context) {
	o, ok := org(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"manual": h.Svc.ListManual(o), "base_currency": orgprefs.Currency(h.Svc.DB, o), "can_edit": orgprefs.CanEdit(c.GetString("user_role"))}})
}

func (h *Handler) saveManual(c *gin.Context) {
	o, ok := org(c)
	if !ok {
		return
	}
	if !orgprefs.CanEdit(c.GetString("user_role")) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only admins and managers can set exchange rates")
		return
	}
	var in struct {
		From string  `json:"from"`
		To   string  `json:"to"`
		Rate float64 `json:"rate"`
		Note string  `json:"note"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	row, err := h.Svc.SaveManual(o, c.GetString("user_id"), in.From, in.To, in.Rate, in.Note)
	if err != nil {
		msg := err.Error()
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", strings.ToUpper(msg[:1])+msg[1:])
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"manual": row, "quote": h.Svc.Quote(o, row.FromCurrency, row.ToCurrency)}})
}

func (h *Handler) deleteManual(c *gin.Context) {
	o, ok := org(c)
	if !ok {
		return
	}
	if !orgprefs.CanEdit(c.GetString("user_role")) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only admins and managers can change exchange rates")
		return
	}
	if !h.Svc.DeleteManual(o, c.Param("id")) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Rate not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"deleted": true}})
}
