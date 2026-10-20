package onboarding

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Handler serves /api/v1/onboarding/*.
//
//	GET  /onboarding/me              steps, progress and who it is for
//	POST /onboarding/me/welcomed     the welcome sheet was seen
//	POST /onboarding/me/steps/:key   {done: true|false} tick or untick a step
//	POST /onboarding/me/dismiss      hide it ({dismissed: false} shows it again)
//	POST /onboarding/me/restart      start over (welcome again, ticks cleared)
//
// A super admin viewing an organisation (token claim impersonating=true)
// gets the owner's steps as a read-only preview: writes answer 200 with
// preview=true and save nothing. On their own platform session they get
// hidden=true and no steps.
type Handler struct {
	Svc *Service
	// JWTSecret verifies the token again to read the impersonating claim
	// (the auth middleware does not put it in the context).
	JWTSecret string
}

// RegisterRoutes mounts the routes on the authenticated /api/v1 group.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/onboarding/me")
	g.GET("", h.me)
	g.POST("/welcomed", h.welcomed)
	g.POST("/steps/:key", h.step)
	g.POST("/dismiss", h.dismiss)
	g.POST("/restart", h.restart)
}

func fail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

// impersonating reads the impersonating claim of the request's token.
func (h *Handler) impersonating(c *gin.Context) bool {
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) != 2 || parts[0] != "Bearer" {
		return false
	}
	claims := jwt.MapClaims{}
	var err error
	if h.JWTSecret != "" {
		_, err = jwt.ParseWithClaims(parts[1], claims, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(h.JWTSecret), nil
		})
	} else {
		_, _, err = jwt.NewParser().ParseUnverified(parts[1], claims)
	}
	if err != nil {
		return false
	}
	v, _ := claims["impersonating"].(bool)
	return v
}

func (h *Handler) who(c *gin.Context) (Who, bool) {
	w := Who{Org: c.GetString("org_id"), Role: c.GetString("user_role")}
	if u, ok := c.Get("user_id"); ok && u != nil {
		w.User = fmt.Sprint(u)
	}
	if w.Org == "" || w.User == "" {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Organisation not found in token")
		return w, false
	}
	w.Preview = w.Role == "super_admin" && h.impersonating(c)
	return w, true
}

func (h *Handler) reply(c *gin.Context, v View, err error) {
	switch {
	case errors.Is(err, ErrUnknownStep):
		fail(c, http.StatusNotFound, "STEP_NOT_FOUND", "That step is not one of your getting-started steps")
	case err != nil:
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not load your getting-started steps")
	default:
		c.JSON(http.StatusOK, gin.H{"success": true, "data": v, "preview": v.Preview})
	}
}

func (h *Handler) me(c *gin.Context) {
	if w, ok := h.who(c); ok {
		v, err := h.Svc.View(w)
		h.reply(c, v, err)
	}
}

func (h *Handler) welcomed(c *gin.Context) {
	if w, ok := h.who(c); ok {
		v, err := h.Svc.Welcomed(w)
		h.reply(c, v, err)
	}
}

func (h *Handler) step(c *gin.Context) {
	w, ok := h.who(c)
	if !ok {
		return
	}
	var in struct {
		Done *bool `json:"done"`
	}
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&in); err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Send {\"done\": true} or {\"done\": false}")
			return
		}
	}
	done := in.Done == nil || *in.Done
	v, err := h.Svc.SetStep(w, strings.TrimSpace(c.Param("key")), done)
	h.reply(c, v, err)
}

func (h *Handler) dismiss(c *gin.Context) {
	w, ok := h.who(c)
	if !ok {
		return
	}
	var in struct {
		Dismissed *bool `json:"dismissed"`
	}
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&in); err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Send {\"dismissed\": true} or {\"dismissed\": false}")
			return
		}
	}
	v, err := h.Svc.Dismiss(w, in.Dismissed == nil || *in.Dismissed)
	h.reply(c, v, err)
}

func (h *Handler) restart(c *gin.Context) {
	if w, ok := h.who(c); ok {
		v, err := h.Svc.Restart(w)
		h.reply(c, v, err)
	}
}
