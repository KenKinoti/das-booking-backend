package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

func (h *Handler) GetCarePlans(c *gin.Context) {
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

	// Parse query parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	participantID := c.Query("participant_id")
	status := c.Query("status")
	createdBy := c.Query("created_by")
	search := c.Query("search")

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 10
	}

	offset := (page - 1) * limit

	// Build query with organization access control through participant
	query := h.DB.Joins("JOIN participants ON care_plans.participant_id = participants.id").
		Where("participants.organization_id = ?", orgID)

	if participantID != "" {
		query = query.Where("care_plans.participant_id = ?", participantID)
	}

	if status != "" {
		query = query.Where("care_plans.status = ?", status)
	}

	if createdBy != "" {
		query = query.Where("care_plans.created_by = ?", createdBy)
	}

	if search != "" {
		searchTerm := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(care_plans.title) LIKE ? OR LOWER(care_plans.description) LIKE ?", searchTerm, searchTerm)
	}

	// Get total count
	var total int64
	query.Model(&models.CarePlan{}).Count(&total)

	// Get care plans with related data
	var carePlans []models.CarePlan
	if err := query.Preload("Participant").Preload("Creator").Preload("Approver").
		Limit(limit).Offset(offset).Order("care_plans.created_at DESC").
		Find(&carePlans).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch care plans",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"care_plans": carePlans,
			"pagination": gin.H{
				"page":        page,
				"limit":       limit,
				"total":       total,
				"total_pages": (total + int64(limit) - 1) / int64(limit),
			},
		},
	})
}

func (h *Handler) GetCarePlan(c *gin.Context) {
	carePlanID := c.Param("id")
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

	// Find care plan with access control through participant
	var carePlan models.CarePlan
	if err := h.DB.Joins("JOIN participants ON care_plans.participant_id = participants.id").
		Where("care_plans.id = ? AND participants.organization_id = ?", carePlanID, orgID).
		Preload("Participant").Preload("Creator").Preload("Approver").
		First(&carePlan).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "CARE_PLAN_NOT_FOUND",
					"message": "Care plan not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch care plan",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    carePlan,
	})
}

type CreateCarePlanRequest struct {
	ParticipantID string    `json:"participant_id" binding:"required"`
	Title         string    `json:"title" binding:"required"`
	Description   string    `json:"description"`
	Goals         string    `json:"goals"`
	StartDate     time.Time `json:"start_date" binding:"required"`
	EndDate       *time.Time `json:"end_date,omitempty"`
}

func (h *Handler) CreateCarePlan(c *gin.Context) {
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

	userID, _ := c.Get("user_id")

	var req CreateCarePlanRequest
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
	if err := h.DB.Where("id = ? AND organization_id = ? AND is_active = ?", req.ParticipantID, orgID, true).First(&participant).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_PARTICIPANT",
				"message": "Participant not found or inactive",
			},
		})
		return
	}

	// Validate date range
	if req.EndDate != nil && req.EndDate.Before(req.StartDate) {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_DATE_RANGE",
				"message": "End date must be after start date",
			},
		})
		return
	}

	// Create care plan
	carePlan := models.CarePlan{
		ParticipantID: req.ParticipantID,
		Title:         req.Title,
		Description:   req.Description,
		Goals:         req.Goals,
		StartDate:     req.StartDate,
		EndDate:       req.EndDate,
		Status:        "active",
		CreatedBy:     userID.(string),
	}

	if err := h.DB.Create(&carePlan).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to create care plan",
			},
		})
		return
	}

	// Fetch care plan with related data
	h.DB.Preload("Participant").Preload("Creator").First(&carePlan, "id = ?", carePlan.ID)

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    carePlan,
		"message": "Care plan created successfully",
	})
}

type UpdateCarePlanRequest struct {
	Title       *string    `json:"title,omitempty"`
	Description *string    `json:"description,omitempty"`
	Goals       *string    `json:"goals,omitempty"`
	StartDate   *time.Time `json:"start_date,omitempty"`
	EndDate     *time.Time `json:"end_date,omitempty"`
	Status      *string    `json:"status,omitempty" binding:"omitempty,oneof=active completed cancelled"`
}

func (h *Handler) UpdateCarePlan(c *gin.Context) {
	carePlanID := c.Param("id")
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

	var req UpdateCarePlanRequest
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

	// Find care plan with access control
	var carePlan models.CarePlan
	if err := h.DB.Joins("JOIN participants ON care_plans.participant_id = participants.id").
		Where("care_plans.id = ? AND participants.organization_id = ?", carePlanID, orgID).
		First(&carePlan).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "CARE_PLAN_NOT_FOUND",
					"message": "Care plan not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch care plan",
			},
		})
		return
	}

	// Validate date range if being updated
	startDate := carePlan.StartDate
	var endDate *time.Time = carePlan.EndDate

	if req.StartDate != nil {
		startDate = *req.StartDate
	}
	if req.EndDate != nil {
		endDate = req.EndDate
	}

	if endDate != nil && endDate.Before(startDate) {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_DATE_RANGE",
				"message": "End date must be after start date",
			},
		})
		return
	}

	// Update fields
	updates := make(map[string]interface{})
	if req.Title != nil {
		updates["title"] = *req.Title
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Goals != nil {
		updates["goals"] = *req.Goals
	}
	if req.StartDate != nil {
		updates["start_date"] = *req.StartDate
	}
	if req.EndDate != nil {
		updates["end_date"] = *req.EndDate
	}
	if req.Status != nil {
		updates["status"] = *req.Status
	}

	if err := h.DB.Model(&carePlan).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update care plan",
			},
		})
		return
	}

	// Fetch updated care plan
	h.DB.Preload("Participant").Preload("Creator").Preload("Approver").First(&carePlan, "id = ?", carePlanID)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    carePlan,
		"message": "Care plan updated successfully",
	})
}

type ApproveCarePlanRequest struct {
	ApprovalAction string `json:"approval_action" binding:"required,oneof=approve reject"`
}

func (h *Handler) ApproveCarePlan(c *gin.Context) {
	carePlanID := c.Param("id")
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

	userID, _ := c.Get("user_id")
	userRole, _ := c.Get("role")

	// Only admin and manager can approve care plans
	if userRole != "admin" && userRole != "manager" {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INSUFFICIENT_PERMISSIONS",
				"message": "Only admin and manager can approve care plans",
			},
		})
		return
	}

	var req ApproveCarePlanRequest
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

	// Find care plan with access control
	var carePlan models.CarePlan
	if err := h.DB.Joins("JOIN participants ON care_plans.participant_id = participants.id").
		Where("care_plans.id = ? AND participants.organization_id = ?", carePlanID, orgID).
		First(&carePlan).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "CARE_PLAN_NOT_FOUND",
					"message": "Care plan not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch care plan",
			},
		})
		return
	}

	// Update approval fields
	now := time.Now()
	updates := map[string]interface{}{
		"approved_by": userID.(string),
		"approved_at": now,
	}

	if req.ApprovalAction == "reject" {
		updates["status"] = "cancelled"
	}

	if err := h.DB.Model(&carePlan).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update care plan approval",
			},
		})
		return
	}

	// Fetch updated care plan
	h.DB.Preload("Participant").Preload("Creator").Preload("Approver").First(&carePlan, "id = ?", carePlanID)

	message := "Care plan approved successfully"
	if req.ApprovalAction == "reject" {
		message = "Care plan rejected successfully"
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    carePlan,
		"message": message,
	})
}

func (h *Handler) DeleteCarePlan(c *gin.Context) {
	carePlanID := c.Param("id")
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

	// Find care plan with access control
	var carePlan models.CarePlan
	if err := h.DB.Joins("JOIN participants ON care_plans.participant_id = participants.id").
		Where("care_plans.id = ? AND participants.organization_id = ?", carePlanID, orgID).
		First(&carePlan).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "CARE_PLAN_NOT_FOUND",
					"message": "Care plan not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch care plan",
			},
		})
		return
	}

	// Only allow deletion of active or cancelled care plans, not completed ones
	if carePlan.Status == "completed" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_OPERATION",
				"message": "Completed care plans cannot be deleted",
			},
		})
		return
	}

	// Soft delete care plan
	if err := h.DB.Delete(&carePlan).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete care plan",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Care plan deleted successfully",
	})
}