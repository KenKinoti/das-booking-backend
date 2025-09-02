package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func (h *Handler) GetCurrentUser(c *gin.Context) {
	userID, exists := c.Get("user_id")
	fmt.Printf("GetCurrentUser - userID: %v, exists: %v\n", userID, exists)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "User not authenticated",
			},
		})
		return
	}

	var user models.User
	if err := h.DB.Where("id = ?", userID).First(&user).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "USER_NOT_FOUND",
				"message": "User not found",
			},
		})
		return
	}

	// Convert to response format (without password hash)
	userResponse := UserResponse{
		ID:             user.ID,
		Email:          user.Email,
		FirstName:      user.FirstName,
		LastName:       user.LastName,
		Phone:          user.Phone,
		Role:           user.Role,
		OrganizationID: user.OrganizationID,
		IsActive:       user.IsActive,
		LastLoginAt:    user.LastLoginAt,
		CreatedAt:      user.CreatedAt,
		UpdatedAt:      user.UpdatedAt,
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    userResponse,
	})
}

func (h *Handler) GetUsers(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Get user role for permission check
	userRole, roleExists := c.Get("user_role")
	if !roleExists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "User role not found in context",
			},
		})
		return
	}

	// Parse query parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	role := c.Query("role")
	isActive := c.Query("is_active")
	search := c.Query("search")
	createdAfter := c.Query("created_after")

	if page < 1 {
		page = 1
	}
	
	// Allow higher limits for super admin and admin users
	maxLimit := 100
	if userRole == "super_admin" || userRole == "admin" {
		maxLimit = 1000
	}
	
	if limit < 1 || limit > maxLimit {
		limit = 10
	}

	offset := (page - 1) * limit

	// Build query - super admins can see all users
	var query *gorm.DB
	if userRole == "super_admin" {
		query = h.DB.Model(&models.User{})
	} else {
		query = h.DB.Where("organization_id = ?", orgID)
	}

	if role != "" {
		query = query.Where("role = ?", role)
	}

	if isActive != "" {
		activeFilter, _ := strconv.ParseBool(isActive)
		query = query.Where("is_active = ?", activeFilter)
	}

	if search != "" {
		searchTerm := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(first_name) LIKE ? OR LOWER(last_name) LIKE ? OR LOWER(email) LIKE ?", searchTerm, searchTerm, searchTerm)
	}

	if createdAfter != "" {
		if createdAfterTime, err := time.Parse(time.RFC3339, createdAfter); err == nil {
			query = query.Where("created_at >= ?", createdAfterTime)
		}
	}

	// Get total count
	var total int64
	query.Model(&models.User{}).Count(&total)

	// Get users
	var users []models.User
	if err := query.Limit(limit).Offset(offset).Order("created_at DESC").Find(&users).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch users",
			},
		})
		return
	}

	// Convert to response format
	userResponses := make([]UserResponse, len(users))
	for i, user := range users {
		userResponses[i] = UserResponse{
			ID:             user.ID,
			Email:          user.Email,
			FirstName:      user.FirstName,
			LastName:       user.LastName,
			Phone:          user.Phone,
			Role:           user.Role,
			OrganizationID: user.OrganizationID,
			IsActive:       user.IsActive,
			LastLoginAt:    user.LastLoginAt,
			CreatedAt:      user.CreatedAt,
			UpdatedAt:      user.UpdatedAt,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"users":      userResponses,
			"pagination": gin.H{
				"page":        page,
				"limit":       limit,
				"total":       total,
				"total_pages": (total + int64(limit) - 1) / int64(limit),
			},
		},
	})
}

type CreateUserRequest struct {
	Email     string `json:"email" binding:"required,email"`
	Password  string `json:"password" binding:"required,min=8"`
	FirstName string `json:"first_name" binding:"required"`
	LastName  string `json:"last_name" binding:"required"`
	Phone     string `json:"phone"`
	Role      string `json:"role" binding:"required,oneof=admin manager care_worker support_coordinator"`
}

func (h *Handler) CreateUser(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	var req CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "VALIDATION_ERROR",
				"message": "Invalid request parameters",
				"details": err.Error(),
			},
		})
		return
	}

	// Check if user with email already exists
	var existingUser models.User
	if err := h.DB.Where("email = ?", req.Email).First(&existingUser).Error; err == nil {
		c.JSON(http.StatusConflict, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "USER_EXISTS",
				"message": "User with this email already exists",
			},
		})
		return
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "PASSWORD_HASH_ERROR",
				"message": "Failed to hash password",
			},
		})
		return
	}

	// Create user
	user := models.User{
		Email:          req.Email,
		PasswordHash:   string(hashedPassword),
		FirstName:      req.FirstName,
		LastName:       req.LastName,
		Phone:          req.Phone,
		Role:           req.Role,
		OrganizationID: orgID.(string),
		IsActive:       true,
	}

	if err := h.DB.Create(&user).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to create user",
			},
		})
		return
	}

	// Convert to response format
	userResponse := UserResponse{
		ID:             user.ID,
		Email:          user.Email,
		FirstName:      user.FirstName,
		LastName:       user.LastName,
		Phone:          user.Phone,
		Role:           user.Role,
		OrganizationID: user.OrganizationID,
		IsActive:       user.IsActive,
		LastLoginAt:    user.LastLoginAt,
		CreatedAt:      user.CreatedAt,
		UpdatedAt:      user.UpdatedAt,
	}

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    userResponse,
		"message": "User created successfully",
	})
}

type UpdateUserRequest struct {
	FirstName *string `json:"first_name,omitempty"`
	LastName  *string `json:"last_name,omitempty"`
	Phone     *string `json:"phone,omitempty"`
	Role      *string `json:"role,omitempty" binding:"omitempty,oneof=admin manager care_worker support_coordinator"`
	IsActive  *bool   `json:"is_active,omitempty"`
}

func (h *Handler) UpdateUser(c *gin.Context) {
	userID := c.Param("id")
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Get current user ID to prevent self-deactivation if admin
	currentUserID, _ := c.Get("user_id")
	currentUserRole, _ := c.Get("user_role")

	var req UpdateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "VALIDATION_ERROR",
				"message": "Invalid request parameters",
				"details": err.Error(),
			},
		})
		return
	}

	// Find user - super admins can access users from any organization
	var user models.User
	var query *gorm.DB
	if currentUserRole == "super_admin" {
		query = h.DB.Where("id = ?", userID)
	} else {
		query = h.DB.Where("id = ? AND organization_id = ?", userID, orgID)
	}
	
	if err := query.First(&user).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "USER_NOT_FOUND",
					"message": "User not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch user",
			},
		})
		return
	}

	// Prevent admin from deactivating themselves
	if currentUserID == userID && req.IsActive != nil && !*req.IsActive && currentUserRole == "admin" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_OPERATION",
				"message": "Admin users cannot deactivate themselves",
			},
		})
		return
	}

	// Prevent support workers from changing their own active status
	if currentUserID == userID && req.IsActive != nil && (currentUserRole == "care_worker" || currentUserRole == "support_coordinator") {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_OPERATION",
				"message": "Support workers cannot change their own active/inactive status",
			},
		})
		return
	}

	// Update fields
	updates := make(map[string]interface{})
	if req.FirstName != nil {
		updates["first_name"] = *req.FirstName
	}
	if req.LastName != nil {
		updates["last_name"] = *req.LastName
	}
	if req.Phone != nil {
		updates["phone"] = *req.Phone
	}
	if req.Role != nil {
		updates["role"] = *req.Role
	}
	if req.IsActive != nil {
		updates["is_active"] = *req.IsActive
	}

	if err := h.DB.Model(&user).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update user",
			},
		})
		return
	}

	// Fetch updated user
	h.DB.First(&user, "id = ?", userID)

	// Convert to response format
	userResponse := UserResponse{
		ID:             user.ID,
		Email:          user.Email,
		FirstName:      user.FirstName,
		LastName:       user.LastName,
		Phone:          user.Phone,
		Role:           user.Role,
		OrganizationID: user.OrganizationID,
		IsActive:       user.IsActive,
		LastLoginAt:    user.LastLoginAt,
		CreatedAt:      user.CreatedAt,
		UpdatedAt:      user.UpdatedAt,
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    userResponse,
		"message": "User updated successfully",
	})
}

func (h *Handler) DeleteUser(c *gin.Context) {
	userID := c.Param("id")
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Get current user info
	currentUserID, _ := c.Get("user_id")
	currentUserRole, _ := c.Get("user_role")
	
	// Prevent self-deletion
	if currentUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_OPERATION",
				"message": "Users cannot delete themselves",
			},
		})
		return
	}
	
	// Protect system admin user from deletion
	var checkUser models.User
	if err := h.DB.Where("id = ?", userID).First(&checkUser).Error; err == nil {
		if checkUser.Email == "kennedy@dasyin.com.au" {
			c.JSON(http.StatusForbidden, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "PROTECTED_USER",
					"message": "This system administrator user cannot be deleted",
				},
			})
			return
		}
	}

	// Find user - super admins can access users from any organization
	var user models.User
	var query *gorm.DB
	if currentUserRole == "super_admin" {
		query = h.DB.Where("id = ?", userID)
	} else {
		query = h.DB.Where("id = ? AND organization_id = ?", userID, orgID)
	}
	
	if err := query.First(&user).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "USER_NOT_FOUND",
					"message": "User not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch user",
			},
		})
		return
	}

	// Soft delete user
	if err := h.DB.Delete(&user).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete user",
			},
		})
		return
	}

	// Revoke all refresh tokens for this user
	h.DB.Model(&models.RefreshToken{}).Where("user_id = ?", userID).Update("is_revoked", true)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "User deleted successfully",
	})
}
