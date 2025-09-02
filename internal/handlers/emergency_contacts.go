package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

func (h *Handler) GetEmergencyContacts(c *gin.Context) {
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

	participantID := c.Query("participant_id")
	if participantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "VALIDATION_ERROR",
				"message": "participant_id is required",
			},
		})
		return
	}

	// Parse query parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	isPrimary := c.Query("is_primary")
	isActive := c.Query("is_active")
	search := c.Query("search")

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 10
	}

	offset := (page - 1) * limit

	// Verify participant belongs to organization
	var participant models.Participant
	if err := h.DB.Where("id = ? AND organization_id = ?", participantID, orgID).First(&participant).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_PARTICIPANT",
				"message": "Participant not found",
			},
		})
		return
	}

	// Build query
	query := h.DB.Where("participant_id = ?", participantID)

	if isPrimary != "" {
		primaryFilter, _ := strconv.ParseBool(isPrimary)
		query = query.Where("is_primary = ?", primaryFilter)
	}

	if isActive != "" {
		activeFilter, _ := strconv.ParseBool(isActive)
		query = query.Where("is_active = ?", activeFilter)
	}

	if search != "" {
		searchTerm := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(name) LIKE ? OR LOWER(relationship) LIKE ? OR LOWER(phone) LIKE ? OR LOWER(email) LIKE ?", searchTerm, searchTerm, searchTerm, searchTerm)
	}

	// Get total count
	var total int64
	query.Model(&models.EmergencyContact{}).Count(&total)

	// Get emergency contacts
	var contacts []models.EmergencyContact
	if err := query.Preload("Participant").Limit(limit).Offset(offset).Order("is_primary DESC, created_at DESC").Find(&contacts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch emergency contacts",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"emergency_contacts": contacts,
			"pagination": gin.H{
				"page":        page,
				"limit":       limit,
				"total":       total,
				"total_pages": (total + int64(limit) - 1) / int64(limit),
			},
		},
	})
}

func (h *Handler) GetEmergencyContact(c *gin.Context) {
	contactID := c.Param("id")
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

	// Find emergency contact with access control through participant
	var contact models.EmergencyContact
	if err := h.DB.Joins("JOIN participants ON emergency_contacts.participant_id = participants.id").
		Where("emergency_contacts.id = ? AND participants.organization_id = ?", contactID, orgID).
		Preload("Participant").
		First(&contact).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "CONTACT_NOT_FOUND",
					"message": "Emergency contact not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch emergency contact",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    contact,
	})
}

type CreateEmergencyContactRequest struct {
	ParticipantID string `json:"participant_id" binding:"required"`
	Name          string `json:"name" binding:"required"`
	Relationship  string `json:"relationship" binding:"required"`
	Phone         string `json:"phone" binding:"required"`
	Email         string `json:"email" binding:"omitempty,email"`
	IsPrimary     bool   `json:"is_primary"`
}

func (h *Handler) CreateEmergencyContact(c *gin.Context) {
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

	var req CreateEmergencyContactRequest
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

	// Verify participant belongs to organization
	var participant models.Participant
	if err := h.DB.Where("id = ? AND organization_id = ?", req.ParticipantID, orgID).First(&participant).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_PARTICIPANT",
				"message": "Participant not found",
			},
		})
		return
	}

	// If setting as primary, unset other primary contacts for this participant
	if req.IsPrimary {
		h.DB.Model(&models.EmergencyContact{}).
			Where("participant_id = ? AND is_primary = ?", req.ParticipantID, true).
			Update("is_primary", false)
	}

	// Create emergency contact
	contact := models.EmergencyContact{
		ParticipantID: req.ParticipantID,
		Name:          req.Name,
		Relationship:  req.Relationship,
		Phone:         req.Phone,
		Email:         req.Email,
		IsPrimary:     req.IsPrimary,
		IsActive:      true,
	}

	if err := h.DB.Create(&contact).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to create emergency contact",
			},
		})
		return
	}

	// Fetch contact with participant data
	h.DB.Preload("Participant").First(&contact, "id = ?", contact.ID)

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    contact,
		"message": "Emergency contact created successfully",
	})
}

type UpdateEmergencyContactRequest struct {
	Name         *string `json:"name,omitempty"`
	Relationship *string `json:"relationship,omitempty"`
	Phone        *string `json:"phone,omitempty"`
	Email        *string `json:"email,omitempty" binding:"omitempty,email"`
	IsPrimary    *bool   `json:"is_primary,omitempty"`
	IsActive     *bool   `json:"is_active,omitempty"`
}

func (h *Handler) UpdateEmergencyContact(c *gin.Context) {
	contactID := c.Param("id")
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

	var req UpdateEmergencyContactRequest
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

	// Find emergency contact with access control
	var contact models.EmergencyContact
	if err := h.DB.Joins("JOIN participants ON emergency_contacts.participant_id = participants.id").
		Where("emergency_contacts.id = ? AND participants.organization_id = ?", contactID, orgID).
		First(&contact).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "CONTACT_NOT_FOUND",
					"message": "Emergency contact not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch emergency contact",
			},
		})
		return
	}

	// If setting as primary, unset other primary contacts for this participant
	if req.IsPrimary != nil && *req.IsPrimary && !contact.IsPrimary {
		h.DB.Model(&models.EmergencyContact{}).
			Where("participant_id = ? AND is_primary = ?", contact.ParticipantID, true).
			Update("is_primary", false)
	}

	// Update fields
	updates := make(map[string]interface{})
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Relationship != nil {
		updates["relationship"] = *req.Relationship
	}
	if req.Phone != nil {
		updates["phone"] = *req.Phone
	}
	if req.Email != nil {
		updates["email"] = *req.Email
	}
	if req.IsPrimary != nil {
		updates["is_primary"] = *req.IsPrimary
	}
	if req.IsActive != nil {
		updates["is_active"] = *req.IsActive
	}

	if err := h.DB.Model(&contact).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update emergency contact",
			},
		})
		return
	}

	// Fetch updated contact
	h.DB.Preload("Participant").First(&contact, "id = ?", contactID)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    contact,
		"message": "Emergency contact updated successfully",
	})
}

func (h *Handler) DeleteEmergencyContact(c *gin.Context) {
	contactID := c.Param("id")
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

	// Find emergency contact with access control
	var contact models.EmergencyContact
	if err := h.DB.Joins("JOIN participants ON emergency_contacts.participant_id = participants.id").
		Where("emergency_contacts.id = ? AND participants.organization_id = ?", contactID, orgID).
		First(&contact).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "CONTACT_NOT_FOUND",
					"message": "Emergency contact not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch emergency contact",
			},
		})
		return
	}

	// Delete emergency contact (hard delete since it's a simple entity)
	if err := h.DB.Unscoped().Delete(&contact).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete emergency contact",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Emergency contact deleted successfully",
	})
}