// Package mpos is "Fast sale on any device": the pieces that let a phone,
// tablet or desktop till sell as fast as possible and keep owners in the
// picture about who sold what on which device.
//
//	GET    /pos/quick-picks                 the person's favourites at this bar (last 30 days), then the bar's and the shop's top sellers
//	GET    /pos/people-summary              managers: sales, value, voids and discounts per person and per device
//	POST   /pos/barcode-links               managers: save a scanned barcode on a product ("Link this barcode")
//	POST   /pos/transactions/:id/photos     attach a JPEG to a sale (M-Pesa confirmation, a damaged bottle)
//	GET    /pos/transactions/:id/photos     the photos of a sale (managers; staff see the ones they added)
//	GET    /pos/sale-photos/:photo_id       one photo (image/jpeg)
//	DELETE /pos/sale-photos/:photo_id       managers
//
// Sales carry device_id / device_label / location_id / client_ref (columns
// on pos_transactions, set by internal/handlers CreatePOSTransaction with
// the Clean* helpers here). Everything is scoped to the caller's
// organisation. The plan / business type / role guard (pkg/plans,
// pkg/industry) applies to /pos first; "managers" here are role base admin
// or manager (owners, bar managers) and super admins.
package mpos

import (
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Handler serves the fast-sale endpoints.
type Handler struct {
	DB *gorm.DB
	// Base returns the role base (admin | manager | staff | read_only) of a
	// user; nil or "" falls back to the legacy users.role string.
	Base func(org, user, legacyRole string) string
}

// Register adds the routes to the /api/v1 group (behind JWT).
func (h *Handler) Register(auth *gin.RouterGroup) {
	auth.GET("/pos/quick-picks", h.quickPicks)
	auth.GET("/pos/people-summary", h.peopleSummary)
	auth.POST("/pos/barcode-links", h.linkBarcode)
	auth.POST("/pos/transactions/:id/photos", h.addPhoto)
	auth.GET("/pos/transactions/:id/photos", h.listPhotos)
	auth.GET("/pos/sale-photos/:photo_id", h.getPhoto)
	auth.DELETE("/pos/sale-photos/:photo_id", h.deletePhoto)
}

type caller struct {
	Org, User, Role string
}

func (h *Handler) who(c *gin.Context) (caller, bool) {
	org := c.GetString("org_id")
	if org == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return caller{}, false
	}
	return caller{Org: org, User: c.GetString("user_id"), Role: c.GetString("user_role")}, true
}

// manager: owners, admins, managers (role base) and super admins.
func (h *Handler) manager(w caller) bool {
	switch strings.ToLower(w.Role) {
	case "super_admin", "admin", "owner":
		return true
	}
	base := ""
	if h.Base != nil {
		base = h.Base(w.Org, w.User, w.Role)
	}
	if base == "" {
		base = strings.ToLower(w.Role)
	}
	return base == "admin" || base == "manager"
}

func forbidden(c *gin.Context, msg string) {
	c.JSON(http.StatusForbidden, gin.H{"error": msg, "code": "MANAGER_ONLY"})
}

// ------------------------------------------------------------------ sale tags

var deviceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// CleanDeviceID keeps a device id the till generated (8–64 of A–Z a–z 0–9 _ -), else "".
func CleanDeviceID(s string) string {
	s = strings.TrimSpace(s)
	if !deviceIDRe.MatchString(s) {
		return ""
	}
	return s
}

// CleanDeviceLabel trims a device name ("Brian's phone") to one line of at most 60 characters.
func CleanDeviceLabel(s string) string { return cleanLine(s, 60) }

// cleanLine: one line of text, spaces collapsed, no control characters or angle brackets, at most max runes.
func cleanLine(s string, max int) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsSpace(r) {
			if space {
				continue
			}
			space = true
			r = ' '
		} else if unicode.IsControl(r) || r == '<' || r == '>' {
			continue
		} else {
			space = false
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// CleanClientRef keeps the till's idempotency key (same rules as a device id), else "".
func CleanClientRef(s string) string { return CleanDeviceID(s) }
