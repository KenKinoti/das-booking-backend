package messaging

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

// contextString reads a value set by the auth middleware and renders it as a
// string. User and organisation IDs are string UUIDs across the system.
func contextString(c *gin.Context, keys ...string) string {
	for _, key := range keys {
		v, ok := c.Get(key)
		if !ok || v == nil {
			continue
		}
		s := strings.TrimSpace(fmt.Sprint(v))
		if s != "" && s != "<nil>" {
			return s
		}
	}
	return ""
}

// userID returns the authenticated user's ID, or "" when unauthenticated.
func userID(c *gin.Context) string {
	return contextString(c, "user_id")
}

// orgID returns the authenticated user's organisation ID, or "" if missing.
func orgID(c *gin.Context) string {
	return contextString(c, "org_id", "organization_id")
}

// requireUser returns the user ID or writes a 401 and returns ok=false.
func requireUser(c *gin.Context) (string, bool) {
	uid := userID(c)
	if uid == "" {
		c.JSON(401, gin.H{"success": false, "error": "Unauthorized"})
		return "", false
	}
	return uid, true
}

// requireOrg returns the organisation ID or writes a 401 and returns ok=false.
func requireOrg(c *gin.Context) (string, bool) {
	oid := orgID(c)
	if oid == "" {
		c.JSON(401, gin.H{"success": false, "error": "Unauthorized: organization not found in token"})
		return "", false
	}
	return oid, true
}

// requireUserAndOrg returns both IDs or writes a 401 and returns ok=false.
func requireUserAndOrg(c *gin.Context) (string, string, bool) {
	uid, ok := requireUser(c)
	if !ok {
		return "", "", false
	}
	oid, ok := requireOrg(c)
	if !ok {
		return "", "", false
	}
	return uid, oid, true
}
