package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// Organization Branding Endpoints

func (h *Handler) GetOrganizationBranding(c *gin.Context) {
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

	var branding models.OrganizationBranding
	if err := h.DB.Where("organization_id = ?", orgID).First(&branding).Error; err != nil {
		// Create default branding if none exists
		branding = models.OrganizationBranding{
			OrganizationID: fmt.Sprintf("%v", orgID),
			PrimaryColor:   "#667eea",
			SecondaryColor: "#764ba2",
			AccentColor:    "#10b981",
			ThemeName:      "professional",
		}
		h.DB.Create(&branding)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    branding,
	})
}

type UpdateBrandingRequest struct {
	LogoURL        *string `json:"logo_url,omitempty"`
	PrimaryColor   *string `json:"primary_color,omitempty"`
	SecondaryColor *string `json:"secondary_color,omitempty"`
	AccentColor    *string `json:"accent_color,omitempty"`
	ThemeName      *string `json:"theme_name,omitempty"`
	CustomCSS      *string `json:"custom_css,omitempty"`
	FaviconURL     *string `json:"favicon_url,omitempty"`
	CompanySlogan  *string `json:"company_slogan,omitempty"`
	FooterText     *string `json:"footer_text,omitempty"`
}

func (h *Handler) UpdateOrganizationBranding(c *gin.Context) {
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

	// Check if user has permission to update branding
	userRole, _ := c.Get("user_role")
	if userRole != "admin" {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INSUFFICIENT_PERMISSIONS",
				"message": "Only administrators can update organization branding",
			},
		})
		return
	}

	var req UpdateBrandingRequest
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

	var branding models.OrganizationBranding
	if err := h.DB.Where("organization_id = ?", orgID).First(&branding).Error; err != nil {
		// Create if doesn't exist
		branding = models.OrganizationBranding{
			OrganizationID: fmt.Sprintf("%v", orgID),
		}
		h.DB.Create(&branding)
	}

	// Update fields
	updates := make(map[string]interface{})
	if req.LogoURL != nil {
		updates["logo_url"] = *req.LogoURL
	}
	if req.PrimaryColor != nil {
		updates["primary_color"] = *req.PrimaryColor
	}
	if req.SecondaryColor != nil {
		updates["secondary_color"] = *req.SecondaryColor
	}
	if req.AccentColor != nil {
		updates["accent_color"] = *req.AccentColor
	}
	if req.ThemeName != nil {
		updates["theme_name"] = *req.ThemeName
	}
	if req.CustomCSS != nil {
		updates["custom_css"] = *req.CustomCSS
	}
	if req.FaviconURL != nil {
		updates["favicon_url"] = *req.FaviconURL
	}
	if req.CompanySlogan != nil {
		updates["company_slogan"] = *req.CompanySlogan
	}
	if req.FooterText != nil {
		updates["footer_text"] = *req.FooterText
	}

	if err := h.DB.Model(&branding).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update branding",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    branding,
		"message": "Branding updated successfully",
	})
}

// Organization Settings Endpoints

func (h *Handler) GetOrganizationSettings(c *gin.Context) {
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

	var settings models.OrganizationSettings
	if err := h.DB.Where("organization_id = ?", orgID).First(&settings).Error; err != nil {
		// Create default settings if none exist
		settings = models.OrganizationSettings{
			OrganizationID:           fmt.Sprintf("%v", orgID),
			Timezone:                 "Australia/Adelaide",
			DateFormat:               "DD/MM/YYYY",
			TimeFormat:               "24h",
			Currency:                 "AUD",
			Language:                 "en-AU",
			DefaultShiftDuration:     120,
			MaxShiftDuration:         720,
			MinShiftNotice:           30,
			EnableSMSNotifications:   true,
			EnableEmailNotifications: true,
		}
		h.DB.Create(&settings)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    settings,
	})
}

type UpdateSettingsRequest struct {
	Timezone                 *string `json:"timezone,omitempty"`
	DateFormat               *string `json:"date_format,omitempty"`
	TimeFormat               *string `json:"time_format,omitempty"`
	Currency                 *string `json:"currency,omitempty"`
	Language                 *string `json:"language,omitempty"`
	DefaultShiftDuration     *int    `json:"default_shift_duration,omitempty"`
	MaxShiftDuration         *int    `json:"max_shift_duration,omitempty"`
	MinShiftNotice           *int    `json:"min_shift_notice,omitempty"`
	RequireShiftNotes        *bool   `json:"require_shift_notes,omitempty"`
	RequirePhotoEvidence     *bool   `json:"require_photo_evidence,omitempty"`
	AutoAssignShifts         *bool   `json:"auto_assign_shifts,omitempty"`
	EnableSMSNotifications   *bool   `json:"enable_sms_notifications,omitempty"`
	EnableEmailNotifications *bool   `json:"enable_email_notifications,omitempty"`
}

func (h *Handler) UpdateOrganizationSettings(c *gin.Context) {
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

	// Check permissions
	userRole, _ := c.Get("user_role")
	if userRole != "admin" && userRole != "manager" {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INSUFFICIENT_PERMISSIONS",
				"message": "Only administrators and managers can update organization settings",
			},
		})
		return
	}

	var req UpdateSettingsRequest
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

	var settings models.OrganizationSettings
	if err := h.DB.Where("organization_id = ?", orgID).First(&settings).Error; err != nil {
		settings = models.OrganizationSettings{OrganizationID: fmt.Sprintf("%v", orgID)}
		h.DB.Create(&settings)
	}

	// Update fields
	updates := make(map[string]interface{})
	if req.Timezone != nil {
		updates["timezone"] = *req.Timezone
	}
	if req.DateFormat != nil {
		updates["date_format"] = *req.DateFormat
	}
	if req.TimeFormat != nil {
		updates["time_format"] = *req.TimeFormat
	}
	if req.Currency != nil {
		updates["currency"] = *req.Currency
	}
	if req.Language != nil {
		updates["language"] = *req.Language
	}
	if req.DefaultShiftDuration != nil {
		updates["default_shift_duration"] = *req.DefaultShiftDuration
	}
	if req.MaxShiftDuration != nil {
		updates["max_shift_duration"] = *req.MaxShiftDuration
	}
	if req.MinShiftNotice != nil {
		updates["min_shift_notice"] = *req.MinShiftNotice
	}
	if req.RequireShiftNotes != nil {
		updates["require_shift_notes"] = *req.RequireShiftNotes
	}
	if req.RequirePhotoEvidence != nil {
		updates["require_photo_evidence"] = *req.RequirePhotoEvidence
	}
	if req.AutoAssignShifts != nil {
		updates["auto_assign_shifts"] = *req.AutoAssignShifts
	}
	if req.EnableSMSNotifications != nil {
		updates["enable_sms_notifications"] = *req.EnableSMSNotifications
	}
	if req.EnableEmailNotifications != nil {
		updates["enable_email_notifications"] = *req.EnableEmailNotifications
	}

	if err := h.DB.Model(&settings).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update settings",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    settings,
		"message": "Settings updated successfully",
	})
}

// Organization Management Endpoints

type CreateOrganizationRequest struct {
	Name          string            `json:"name" binding:"required"`
	ABN           string            `json:"abn"`
	Phone         string            `json:"phone"`
	Email         string            `json:"email" binding:"required,email"`
	Website       string            `json:"website"`
	Address       models.Address    `json:"address"`
	NDISReg       models.NDISReg    `json:"ndis_registration"`
	AdminUser     CreateAdminUser   `json:"admin_user" binding:"required"`
	Subscription  SubscriptionPlan  `json:"subscription" binding:"required"`
}

type CreateAdminUser struct {
	FirstName string `json:"first_name" binding:"required"`
	LastName  string `json:"last_name" binding:"required"`
	Email     string `json:"email" binding:"required,email"`
	Phone     string `json:"phone"`
	Password  string `json:"password" binding:"required,min=6"`
}

type SubscriptionPlan struct {
	PlanName        string `json:"plan_name" binding:"required"`
	BillingEmail    string `json:"billing_email" binding:"required,email"`
	BillingCycle    string `json:"billing_cycle" binding:"required,oneof=monthly yearly"`
	MaxUsers        int    `json:"max_users" binding:"required,min=1"`
	MaxParticipants int    `json:"max_participants" binding:"required,min=1"`
	MaxStorageGB    int    `json:"max_storage_gb" binding:"required,min=1"`
}

// This endpoint would typically be used by a system admin or during organization onboarding
func (h *Handler) CreateOrganization(c *gin.Context) {
	var req CreateOrganizationRequest
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

	// Start transaction
	tx := h.DB.Begin()

	// Create organization
	org := models.Organization{
		Name:    req.Name,
		ABN:     req.ABN,
		Phone:   req.Phone,
		Email:   req.Email,
		Website: req.Website,
		Address: req.Address,
		NDISReg: req.NDISReg,
	}

	if err := tx.Create(&org).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to create organization",
			},
		})
		return
	}

	// Hash admin password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.AdminUser.Password), bcrypt.DefaultCost)
	if err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "PASSWORD_HASH_ERROR",
				"message": "Failed to hash password",
			},
		})
		return
	}

	// Create admin user
	adminUser := models.User{
		Email:          req.AdminUser.Email,
		PasswordHash:   string(hashedPassword),
		FirstName:      req.AdminUser.FirstName,
		LastName:       req.AdminUser.LastName,
		Phone:          req.AdminUser.Phone,
		Role:           "admin",
		OrganizationID: org.ID,
		IsActive:       true,
	}

	if err := tx.Create(&adminUser).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to create admin user",
			},
		})
		return
	}

	// Calculate subscription pricing
	planRates := map[string]float64{
		"starter":      49.00,  // AUD per month
		"professional": 99.00,
		"enterprise":   199.00,
	}

	monthlyRate := planRates[req.Subscription.PlanName]
	if monthlyRate == 0 {
		monthlyRate = 49.00 // Default to starter
	}

	// Create subscription
	subscription := models.OrganizationSubscription{
		OrganizationID:      org.ID,
		PlanName:           req.Subscription.PlanName,
		Status:             "active",
		BillingEmail:       req.Subscription.BillingEmail,
		MonthlyRate:        monthlyRate,
		MaxUsers:           req.Subscription.MaxUsers,
		MaxParticipants:    req.Subscription.MaxParticipants,
		MaxStorageGB:       req.Subscription.MaxStorageGB,
		HasCustomBranding:  req.Subscription.PlanName != "starter",
		HasAPIAccess:       req.Subscription.PlanName == "enterprise",
		HasAdvancedReports: req.Subscription.PlanName != "starter",
		BillingCycle:       req.Subscription.BillingCycle,
		NextBillingDate:    time.Now().AddDate(0, 1, 0), // Next month
	}

	if err := tx.Create(&subscription).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to create subscription",
			},
		})
		return
	}

	// Setup organization defaults
	if err := models.SetupOrganizationDefaults(tx, org.ID); err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "SETUP_ERROR",
				"message": "Failed to setup organization defaults",
			},
		})
		return
	}

	// Commit transaction
	tx.Commit()

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data": gin.H{
			"organization": org,
			"admin_user":   adminUser,
			"subscription": subscription,
		},
		"message": "Organization created successfully",
	})
}

// Get organization subscription info
func (h *Handler) GetOrganizationSubscription(c *gin.Context) {
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

	var subscription models.OrganizationSubscription
	if err := h.DB.Where("organization_id = ?", orgID).First(&subscription).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "SUBSCRIPTION_NOT_FOUND",
				"message": "Organization subscription not found",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    subscription,
	})
}

// Usage tracking for subscription limits
func (h *Handler) GetOrganizationUsage(c *gin.Context) {
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

	// Get current usage
	var userCount, participantCount int64
	h.DB.Model(&models.User{}).Where("organization_id = ? AND deleted_at IS NULL", orgID).Count(&userCount)
	h.DB.Model(&models.Participant{}).Where("organization_id = ? AND deleted_at IS NULL", orgID).Count(&participantCount)

	// Get subscription limits
	var subscription models.OrganizationSubscription
	if err := h.DB.Where("organization_id = ?", orgID).First(&subscription).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "SUBSCRIPTION_NOT_FOUND",
				"message": "Organization subscription not found",
			},
		})
		return
	}

	usage := gin.H{
		"users": gin.H{
			"current": userCount,
			"limit":   subscription.MaxUsers,
			"percentage": float64(userCount) / float64(subscription.MaxUsers) * 100,
		},
		"participants": gin.H{
			"current": participantCount,
			"limit":   subscription.MaxParticipants,
			"percentage": float64(participantCount) / float64(subscription.MaxParticipants) * 100,
		},
		"storage": gin.H{
			"current_gb": 0, // TODO: Calculate actual storage usage
			"limit_gb":   subscription.MaxStorageGB,
			"percentage": 0,
		},
		"plan_name": subscription.PlanName,
		"status":    subscription.Status,
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    usage,
	})
}

type UpdateSubscriptionRequest struct {
	PlanName       *string `json:"plan_name,omitempty"`
	MonthlyRate    *float64 `json:"monthly_rate,omitempty"`
	MaxUsers       *int    `json:"max_users,omitempty"`
	MaxParticipants *int   `json:"max_participants,omitempty"`
	MaxStorageGB   *int    `json:"max_storage_gb,omitempty"`
	BillingCycle   *string `json:"billing_cycle,omitempty"`
}

func (h *Handler) UpdateOrganizationSubscription(c *gin.Context) {
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

	var req UpdateSubscriptionRequest
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

	// Find existing subscription
	var subscription models.OrganizationSubscription
	if err := h.DB.Where("organization_id = ?", orgID).First(&subscription).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "SUBSCRIPTION_NOT_FOUND",
				"message": "Organization subscription not found",
			},
		})
		return
	}

	// Update subscription fields
	updates := make(map[string]interface{})
	if req.PlanName != nil {
		updates["plan_name"] = *req.PlanName
	}
	if req.MonthlyRate != nil {
		updates["monthly_rate"] = *req.MonthlyRate
	}
	if req.MaxUsers != nil {
		updates["max_users"] = *req.MaxUsers
	}
	if req.MaxParticipants != nil {
		updates["max_participants"] = *req.MaxParticipants
	}
	if req.MaxStorageGB != nil {
		updates["max_storage_gb"] = *req.MaxStorageGB
	}
	if req.BillingCycle != nil {
		updates["billing_cycle"] = *req.BillingCycle
	}

	if err := h.DB.Model(&subscription).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update subscription",
			},
		})
		return
	}

	// Fetch updated subscription
	h.DB.First(&subscription, "organization_id = ?", orgID)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    subscription,
		"message": "Organization subscription updated successfully",
	})
}

// Super Admin endpoints for managing multiple organizations

func (h *Handler) GetAllOrganizations(c *gin.Context) {
	// Parse query parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	search := c.Query("search")
	status := c.Query("status")

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}

	offset := (page - 1) * limit

	// Build query
	query := h.DB.Preload("Users", "role = ? AND is_active = ?", "admin", true).
		Preload("Participants")

	if search != "" {
		query = query.Where("LOWER(name) LIKE ? OR LOWER(email) LIKE ? OR abn LIKE ?", 
			"%"+search+"%", "%"+search+"%", "%"+search+"%")
	}

	if status != "" {
		// Join with subscription to filter by status
		query = query.Joins("LEFT JOIN organization_subscriptions ON organizations.id = organization_subscriptions.organization_id").
			Where("organization_subscriptions.status = ?", status)
	}

	// Get total count
	var total int64
	query.Model(&models.Organization{}).Count(&total)

	// Get organizations
	var organizations []models.Organization
	if err := query.Limit(limit).Offset(offset).Order("created_at DESC").Find(&organizations).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch organizations",
			},
		})
		return
	}

	// Create response structure with subscription details
	type OrganizationResponse struct {
		models.Organization
		Subscription *models.OrganizationSubscription `json:"subscription,omitempty"`
	}

	var organizationResponses []OrganizationResponse
	for _, org := range organizations {
		var subscription models.OrganizationSubscription
		response := OrganizationResponse{Organization: org}
		
		if err := h.DB.Where("organization_id = ?", org.ID).First(&subscription).Error; err == nil {
			response.Subscription = &subscription
		}
		
		organizationResponses = append(organizationResponses, response)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"organizations": organizationResponses,
			"pagination": gin.H{
				"page":        page,
				"limit":       limit,
				"total":       total,
				"total_pages": (total + int64(limit) - 1) / int64(limit),
			},
		},
	})
}

func (h *Handler) GetOrganizationById(c *gin.Context) {
	orgID := c.Param("id")

	var org models.Organization
	if err := h.DB.Preload("Users").Preload("Participants").First(&org, "id = ?", orgID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "ORGANIZATION_NOT_FOUND",
					"message": "Organization not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch organization",
			},
		})
		return
	}

	// Get subscription details
	var subscription models.OrganizationSubscription
	h.DB.Where("organization_id = ?", orgID).First(&subscription)

	// Get branding details
	var branding models.OrganizationBranding
	h.DB.Where("organization_id = ?", orgID).First(&branding)

	// Get settings details
	var settings models.OrganizationSettings
	h.DB.Where("organization_id = ?", orgID).First(&settings)

	response := gin.H{
		"organization": org,
		"subscription": subscription,
		"branding":     branding,
		"settings":     settings,
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    response,
	})
}

type UpdateOrganizationStatusRequest struct {
	Status string `json:"status" binding:"required,oneof=active suspended cancelled"`
	Reason string `json:"reason"`
}

func (h *Handler) UpdateOrganizationStatus(c *gin.Context) {
	orgID := c.Param("id")

	var req UpdateOrganizationStatusRequest
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

	// Update subscription status
	if err := h.DB.Model(&models.OrganizationSubscription{}).
		Where("organization_id = ?", orgID).
		Update("status", req.Status).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update organization status",
			},
		})
		return
	}

	// If suspending or cancelling, deactivate all users in the organization
	if req.Status == "suspended" || req.Status == "cancelled" {
		h.DB.Model(&models.User{}).
			Where("organization_id = ?", orgID).
			Update("is_active", false)
	} else if req.Status == "active" {
		// If reactivating, activate admin users
		h.DB.Model(&models.User{}).
			Where("organization_id = ? AND role = ?", orgID, "admin").
			Update("is_active", true)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Organization status updated successfully",
		"data": gin.H{
			"organization_id": orgID,
			"status":          req.Status,
			"reason":          req.Reason,
		},
	})
}

func (h *Handler) DeleteOrganization(c *gin.Context) {
	orgID := c.Param("id")

	// Start transaction
	tx := h.DB.Begin()
	if tx.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to start transaction",
			},
		})
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// Check if organization exists
	var org models.Organization
	if err := tx.First(&org, "id = ?", orgID).Error; err != nil {
		tx.Rollback()
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "ORGANIZATION_NOT_FOUND",
					"message": "Organization not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch organization",
			},
		})
		return
	}

	// Soft delete organization (this will cascade to related records due to GORM relationships)
	if err := tx.Delete(&org).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete organization",
			},
		})
		return
	}

	// Commit transaction
	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to commit transaction",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Organization deleted successfully",
	})
}