package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

func (h *Handler) GetParticipants(c *gin.Context) {
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

	// Debug output
	fmt.Printf("GetParticipants - userRole: %v (type: %T)\n", userRole, userRole)

	// Parse query parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	isActive := c.Query("is_active")
	search := c.Query("search")
	ndisNumber := c.Query("ndis_number")

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

	// Build query - super admins can see all participants
	var query *gorm.DB
	if userRole == "super_admin" {
		query = h.DB.Model(&models.Participant{})
	} else {
		query = h.DB.Where("organization_id = ?", orgID)
	}

	if isActive != "" {
		activeFilter, _ := strconv.ParseBool(isActive)
		query = query.Where("is_active = ?", activeFilter)
	}

	if ndisNumber != "" {
		query = query.Where("ndis_number = ?", ndisNumber)
	}

	if search != "" {
		searchTerm := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(first_name) LIKE ? OR LOWER(last_name) LIKE ? OR LOWER(email) LIKE ?", searchTerm, searchTerm, searchTerm)
	}

	// Get total count
	var total int64
	query.Model(&models.Participant{}).Count(&total)

	// Get participants with emergency contacts
	var participants []models.Participant
	if err := query.Preload("EmergencyContacts").Limit(limit).Offset(offset).Order("created_at DESC").Find(&participants).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch participants",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"participants": participants,
			"pagination": gin.H{
				"page":        page,
				"limit":       limit,
				"total":       total,
				"total_pages": (total + int64(limit) - 1) / int64(limit),
			},
		},
	})
}

func (h *Handler) GetParticipant(c *gin.Context) {
	participantID := c.Param("id")
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

	// Find participant with all related data
	var participant models.Participant
	if err := h.DB.Where("id = ? AND organization_id = ?", participantID, orgID).
		Preload("EmergencyContacts").
		Preload("Shifts").
		Preload("Documents").
		Preload("CarePlans").
		First(&participant).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "PARTICIPANT_NOT_FOUND",
					"message": "Participant not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch participant",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    participant,
	})
}

type CreateParticipantRequest struct {
	FirstName      string                    `json:"first_name" binding:"required"`
	LastName       string                    `json:"last_name" binding:"required"`
	DateOfBirth    time.Time                 `json:"date_of_birth" binding:"required"`
	NDISNumber     string                    `json:"ndis_number"`
	Email          string                    `json:"email" binding:"omitempty,email"`
	Phone          string                    `json:"phone"`
	Address        models.Address            `json:"address"`
	MedicalInfo    models.MedicalInformation `json:"medical_information"`
	Funding        models.FundingInformation `json:"funding"`
	EmergencyContacts []CreateParticipantEmergencyContactRequest `json:"emergency_contacts,omitempty"`
}

type CreateParticipantEmergencyContactRequest struct {
	Name         string `json:"name" binding:"required"`
	Relationship string `json:"relationship" binding:"required"`
	Phone        string `json:"phone" binding:"required"`
	Email        string `json:"email" binding:"omitempty,email"`
	IsPrimary    bool   `json:"is_primary"`
}

func (h *Handler) CreateParticipant(c *gin.Context) {
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

	var req CreateParticipantRequest
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

	// Check if participant with NDIS number already exists (if provided)
	if req.NDISNumber != "" {
		var existingParticipant models.Participant
		if err := h.DB.Where("ndis_number = ?", req.NDISNumber).First(&existingParticipant).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "NDIS_NUMBER_EXISTS",
					"message": "Participant with this NDIS number already exists",
				},
			})
			return
		}
	}

	// Start transaction
	tx := h.DB.Begin()

	// Create participant
	participant := models.Participant{
		FirstName:      req.FirstName,
		LastName:       req.LastName,
		DateOfBirth:    req.DateOfBirth,
		NDISNumber:     req.NDISNumber,
		Email:          req.Email,
		Phone:          req.Phone,
		Address:        req.Address,
		MedicalInfo:    req.MedicalInfo,
		Funding:        req.Funding,
		OrganizationID: orgID.(string),
		IsActive:       true,
	}

	if err := tx.Create(&participant).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to create participant",
			},
		})
		return
	}

	// Create emergency contacts
	for _, contactReq := range req.EmergencyContacts {
		contact := models.EmergencyContact{
			ParticipantID: participant.ID,
			Name:          contactReq.Name,
			Relationship:  contactReq.Relationship,
			Phone:         contactReq.Phone,
			Email:         contactReq.Email,
			IsPrimary:     contactReq.IsPrimary,
			IsActive:      true,
		}
		if err := tx.Create(&contact).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "DATABASE_ERROR",
					"message": "Failed to create emergency contact",
				},
			})
			return
		}
	}

	tx.Commit()

	// Fetch participant with emergency contacts
	h.DB.Preload("EmergencyContacts").First(&participant, "id = ?", participant.ID)

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    participant,
		"message": "Participant created successfully",
	})
}

type UpdateParticipantRequest struct {
	FirstName   *string                    `json:"first_name,omitempty"`
	LastName    *string                    `json:"last_name,omitempty"`
	DateOfBirth *time.Time                 `json:"date_of_birth,omitempty"`
	NDISNumber  *string                    `json:"ndis_number,omitempty"`
	Email       *string                    `json:"email,omitempty" binding:"omitempty,email"`
	Phone       *string                    `json:"phone,omitempty"`
	Address     *models.Address            `json:"address,omitempty"`
	MedicalInfo *models.MedicalInformation `json:"medical_information,omitempty"`
	Funding     *models.FundingInformation `json:"funding,omitempty"`
	IsActive    *bool                      `json:"is_active,omitempty"`
}

func (h *Handler) UpdateParticipant(c *gin.Context) {
	participantID := c.Param("id")
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

	var req UpdateParticipantRequest
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

	// Find participant
	var participant models.Participant
	if err := h.DB.Where("id = ? AND organization_id = ?", participantID, orgID).First(&participant).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "PARTICIPANT_NOT_FOUND",
					"message": "Participant not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch participant",
			},
		})
		return
	}

	// Check NDIS number uniqueness if being updated
	if req.NDISNumber != nil && *req.NDISNumber != participant.NDISNumber {
		var existingParticipant models.Participant
		if err := h.DB.Where("ndis_number = ? AND id != ?", *req.NDISNumber, participantID).First(&existingParticipant).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "NDIS_NUMBER_EXISTS",
					"message": "Another participant with this NDIS number already exists",
				},
			})
			return
		}
	}

	// Update fields
	updates := make(map[string]interface{})
	if req.FirstName != nil {
		updates["first_name"] = *req.FirstName
	}
	if req.LastName != nil {
		updates["last_name"] = *req.LastName
	}
	if req.DateOfBirth != nil {
		updates["date_of_birth"] = *req.DateOfBirth
	}
	if req.NDISNumber != nil {
		updates["ndis_number"] = *req.NDISNumber
	}
	if req.Email != nil {
		updates["email"] = *req.Email
	}
	if req.Phone != nil {
		updates["phone"] = *req.Phone
	}
	if req.IsActive != nil {
		updates["is_active"] = *req.IsActive
	}

	// Handle embedded structs
	if req.Address != nil {
		updates["address_street"] = req.Address.Street
		updates["address_suburb"] = req.Address.Suburb
		updates["address_state"] = req.Address.State
		updates["address_postcode"] = req.Address.Postcode
		updates["address_country"] = req.Address.Country
	}

	if req.MedicalInfo != nil {
		updates["medical_conditions"] = req.MedicalInfo.Conditions
		updates["medical_medications"] = req.MedicalInfo.Medications
		updates["medical_allergies"] = req.MedicalInfo.Allergies
		updates["medical_doctor_name"] = req.MedicalInfo.DoctorName
		updates["medical_doctor_phone"] = req.MedicalInfo.DoctorPhone
		updates["medical_notes"] = req.MedicalInfo.Notes
	}

	if req.Funding != nil {
		updates["funding_total_budget"] = req.Funding.TotalBudget
		updates["funding_used_budget"] = req.Funding.UsedBudget
		updates["funding_remaining_budget"] = req.Funding.RemainingBudget
		updates["funding_budget_year"] = req.Funding.BudgetYear
		updates["funding_plan_start_date"] = req.Funding.PlanStartDate
		updates["funding_plan_end_date"] = req.Funding.PlanEndDate
	}

	if err := h.DB.Model(&participant).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update participant",
			},
		})
		return
	}

	// Fetch updated participant
	h.DB.Preload("EmergencyContacts").First(&participant, "id = ?", participantID)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    participant,
		"message": "Participant updated successfully",
	})
}

func (h *Handler) DeleteParticipant(c *gin.Context) {
	participantID := c.Param("id")
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

	// Find participant
	var participant models.Participant
	if err := h.DB.Where("id = ? AND organization_id = ?", participantID, orgID).First(&participant).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "PARTICIPANT_NOT_FOUND",
					"message": "Participant not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch participant",
			},
		})
		return
	}

	// Soft delete participant (this will cascade to related records due to GORM soft delete)
	if err := h.DB.Delete(&participant).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete participant",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Participant deleted successfully",
	})
}
