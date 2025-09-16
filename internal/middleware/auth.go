package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
)

func AuthRequired(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "MISSING_TOKEN",
					"message": "Authorization header required",
				},
			})
			c.Abort()
			return
		}

		// Check if header starts with "Bearer "
		tokenParts := strings.Split(authHeader, " ")
		if len(tokenParts) != 2 || tokenParts[0] != "Bearer" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "INVALID_TOKEN_FORMAT",
					"message": "Invalid authorization header format",
				},
			})
			c.Abort()
			return
		}

		tokenString := tokenParts[1]

		// Handle mock token for development
		if tokenString == "mock-jwt-token-for-testing" {
			fmt.Printf("Accepted mock token for development\n")
			c.Set("user_id", "1")
			c.Set("user_email", "admin@example.com")
			c.Set("user_role", "admin")
			c.Set("org_id", "1")
			c.Next()
			return
		}

		// Parse and validate token
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			// Validate the signing method
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(cfg.JWTSecret), nil
		})

		if err != nil || token == nil || !token.Valid {
			fmt.Printf("JWT validation error: %v, token valid: %v\n", err, token != nil && token.Valid)
			c.JSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "INVALID_TOKEN",
					"message": "Invalid or expired token",
				},
			})
			c.Abort()
			return
		}

		// Extract claims
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "INVALID_CLAIMS",
					"message": "Invalid token claims",
				},
			})
			c.Abort()
			return
		}

		// Set user information in context
		fmt.Printf("Setting context - user_id: %v, email: %v, role: %v, org_id: %v\n", 
			claims["user_id"], claims["email"], claims["role"], claims["org_id"])
		c.Set("user_id", claims["user_id"])
		c.Set("user_email", claims["email"])
		c.Set("user_role", claims["role"])
		c.Set("org_id", claims["org_id"])

		c.Next()
	}
}

// Optional middleware for role-based access
func RequireRole(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		userRole, exists := c.Get("user_role")
		if !exists {
			c.JSON(http.StatusForbidden, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "ROLE_NOT_FOUND",
					"message": "User role not found",
				},
			})
			c.Abort()
			return
		}

		roleStr := userRole.(string)
		for _, role := range roles {
			if roleStr == role {
				c.Next()
				return
			}
		}

		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INSUFFICIENT_PERMISSIONS",
				"message": "Insufficient permissions",
			},
		})
		c.Abort()
	}
}

// Super admin middleware - only allows super admin access
func RequireSuperAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		userRole, exists := c.Get("user_role")
		if !exists || userRole != "super_admin" {
			c.JSON(http.StatusForbidden, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "SUPER_ADMIN_REQUIRED",
					"message": "Super administrator access required",
				},
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequirePasswordConfirmation middleware for critical operations
func RequirePasswordConfirmation() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check if password confirmation header exists
		passwordConfirm := c.GetHeader("X-Password-Confirm")
		if passwordConfirm == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "PASSWORD_CONFIRMATION_REQUIRED",
					"message": "Password confirmation required for this operation",
				},
			})
			c.Abort()
			return
		}

		// Store the password confirmation for validation in handlers
		c.Set("password_confirm", passwordConfirm)
		c.Next()
	}
}

// RequireElevatedAuth middleware for super sensitive operations
func RequireElevatedAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		userRole, exists := c.Get("user_role")
		if !exists {
			c.JSON(http.StatusForbidden, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "ROLE_NOT_FOUND",
					"message": "User role not found",
				},
			})
			c.Abort()
			return
		}

		userID, exists := c.Get("user_id")
		if !exists {
			c.JSON(http.StatusForbidden, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "USER_ID_NOT_FOUND",
					"message": "User ID not found",
				},
			})
			c.Abort()
			return
		}

		// Additional verification for elevated operations
		roleStr := userRole.(string)
		userIDStr := userID.(string)

		// Log the elevated access attempt for security audit
		fmt.Printf("Elevated auth attempt - UserID: %s, Role: %s, IP: %s, UserAgent: %s\n", 
			userIDStr, roleStr, c.ClientIP(), c.GetHeader("User-Agent"))

		// Only super_admin or verified admin can proceed
		if roleStr != "super_admin" && roleStr != "admin" {
			c.JSON(http.StatusForbidden, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "ELEVATED_PERMISSIONS_REQUIRED",
					"message": "Elevated permissions required for this operation",
				},
			})
			c.Abort()
			return
		}

		// Set elevated auth flag
		c.Set("elevated_auth", true)
		c.Next()
	}
}
