package hr

import (
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func fail(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"success": false, "error": msg, "message": msg})
}

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func ctxString(c *gin.Context, key string) string {
	v, exists := c.Get(key)
	if !exists || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func orgOf(c *gin.Context) (string, bool) {
	org := ctxString(c, "org_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "Organisation not found in token")
		return "", false
	}
	return org, true
}

// canManage reports whether the caller may manage staff, approve and run payroll.
func canManage(c *gin.Context) bool {
	switch strings.ToLower(ctxString(c, "user_role")) {
	case "super_admin", "admin", "owner", "manager", "hr", "hr_manager":
		return true
	}
	return false
}

func requireManager(c *gin.Context) bool {
	if !canManage(c) {
		fail(c, http.StatusForbidden, "Only managers and admins can do this")
		return false
	}
	return true
}

const dateLayout = "2006-01-02"

func parseDate(s string) (time.Time, error) {
	return time.Parse(dateLayout, strings.TrimSpace(s))
}

func validDate(s string) bool {
	_, err := parseDate(s)
	return err == nil
}

func validEmail(s string) bool {
	if s == "" {
		return true
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

func trimMax(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > n {
		return string([]rune(s)[:n])
	}
	return s
}

func oneOf(v string, opts ...string) bool {
	for _, o := range opts {
		if v == o {
			return true
		}
	}
	return false
}

func fullName(first, last string) string {
	return strings.TrimSpace(first + " " + last)
}
