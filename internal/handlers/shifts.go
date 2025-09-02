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

// parseTimeFromString accepts multiple time formats and returns a time.Time
func parseTimeFromString(timeStr string) (time.Time, error) {
	timeStr = strings.TrimSpace(timeStr)
	
	// List of supported time formats
	timeFormats := []string{
		time.RFC3339,                    // "2006-01-02T15:04:05Z07:00"
		time.RFC3339Nano,               // "2006-01-02T15:04:05.999999999Z07:00"
		"2006-01-02T15:04:05",         // Local time without timezone
		"2006-01-02 15:04:05",         // Space separated local time
		"2006-01-02T15:04",            // Short format without seconds
		"2006-01-02 15:04",            // Short format with space
	}
	
	// Try each format
	for _, format := range timeFormats {
		if t, err := time.Parse(format, timeStr); err == nil {
			// If no timezone info, treat as local time
			if t.Location() == time.UTC && !strings.Contains(timeStr, "Z") && !strings.Contains(timeStr, "+") && !strings.Contains(timeStr, "-") {
				// Convert to local timezone if no timezone was specified
				local := time.Local
				t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), local)
			}
			return t, nil
		}
	}
	
	return time.Time{}, &time.ParseError{Value: timeStr, Layout: "", ValueElem: ""}
}

func (h *Handler) GetShifts(c *gin.Context) {
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
	participantID := c.Query("participant_id")
	staffID := c.Query("staff_id")
	status := c.Query("status")
	serviceType := c.Query("service_type")
	startDate := c.Query("start_date")
	endDate := c.Query("end_date")

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

	// Build query - super admins can see all shifts
	var query *gorm.DB
	if userRole == "super_admin" {
		query = h.DB.Model(&models.Shift{}).Preload("Participant").Preload("Staff")
	} else {
		query = h.DB.Joins("JOIN participants ON shifts.participant_id = participants.id").
			Where("participants.organization_id = ?", orgID)
	}

	if participantID != "" {
		query = query.Where("shifts.participant_id = ?", participantID)
	}

	if staffID != "" {
		query = query.Where("shifts.staff_id = ?", staffID)
	}

	if status != "" {
		query = query.Where("shifts.status = ?", status)
	}

	if serviceType != "" {
		query = query.Where("shifts.service_type = ?", serviceType)
	}

	if startDate != "" {
		if parsedDate, err := time.Parse("2006-01-02", startDate); err == nil {
			query = query.Where("shifts.start_time >= ?", parsedDate)
		}
	}

	if endDate != "" {
		if parsedDate, err := time.Parse("2006-01-02", endDate); err == nil {
			// Add 24 hours to include the entire end date
			endOfDay := parsedDate.Add(24 * time.Hour)
			query = query.Where("shifts.start_time < ?", endOfDay)
		}
	}

	// Get total count
	var total int64
	query.Model(&models.Shift{}).Count(&total)

	// Get shifts with related data
	var shifts []models.Shift
	if err := query.Preload("Participant").Preload("Staff").
		Limit(limit).Offset(offset).Order("shifts.start_time DESC").
		Find(&shifts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch shifts",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"shifts": shifts,
			"pagination": gin.H{
				"page":        page,
				"limit":       limit,
				"total":       total,
				"total_pages": (total + int64(limit) - 1) / int64(limit),
			},
		},
	})
}

func (h *Handler) GetShift(c *gin.Context) {
	shiftID := c.Param("id")
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

	// Find shift with access control through participant
	var shift models.Shift
	if err := h.DB.Joins("JOIN participants ON shifts.participant_id = participants.id").
		Where("shifts.id = ? AND participants.organization_id = ?", shiftID, orgID).
		Preload("Participant").Preload("Staff").
		First(&shift).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "SHIFT_NOT_FOUND",
					"message": "Shift not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch shift",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    shift,
	})
}

type CreateShiftRequest struct {
	ParticipantID string `json:"participant_id" binding:"required"`
	StaffID       string `json:"staff_id" binding:"required"`
	StartTime     string `json:"start_time" binding:"required"` // Accept ISO string or local datetime
	EndTime       string `json:"end_time" binding:"required"`   // Accept ISO string or local datetime
	ServiceType   string `json:"service_type" binding:"required"`
	Location      string `json:"location" binding:"required"`
	HourlyRate    float64 `json:"hourly_rate" binding:"required,gt=0"`
	Notes         string `json:"notes"`
}

func (h *Handler) CreateShift(c *gin.Context) {
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

	var req CreateShiftRequest
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

	// Parse start and end times - accept multiple formats
	startTime, err := parseTimeFromString(req.StartTime)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_START_TIME",
				"message": "Invalid start time format. Use ISO format or local datetime.",
				"details": err.Error(),
			},
		})
		return
	}

	endTime, err := parseTimeFromString(req.EndTime)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_END_TIME",
				"message": "Invalid end time format. Use ISO format or local datetime.",
				"details": err.Error(),
			},
		})
		return
	}

	// Validate time range
	if endTime.Before(startTime) || endTime.Equal(startTime) {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_TIME_RANGE",
				"message": "End time must be after start time",
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

	// Verify staff belongs to organization
	var staff models.User
	if err := h.DB.Where("id = ? AND organization_id = ? AND is_active = ?", req.StaffID, orgID, true).First(&staff).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_STAFF",
				"message": "Staff member not found or inactive",
			},
		})
		return
	}

	// Check for overlapping shifts for the staff member
	var overlappingShifts int64
	h.DB.Model(&models.Shift{}).
		Where("staff_id = ? AND status NOT IN (?, ?) AND ((start_time <= ? AND end_time > ?) OR (start_time < ? AND end_time >= ?))",
			req.StaffID, "cancelled", "completed", startTime, startTime, endTime, endTime).
		Count(&overlappingShifts)

	if overlappingShifts > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "SCHEDULE_CONFLICT",
				"message": "Staff member already has a shift scheduled during this time",
			},
		})
		return
	}

	// Create shift
	shift := models.Shift{
		ParticipantID: req.ParticipantID,
		StaffID:       req.StaffID,
		StartTime:     startTime,
		EndTime:       endTime,
		ServiceType:   req.ServiceType,
		Location:      req.Location,
		Status:        "scheduled",
		HourlyRate:    req.HourlyRate,
		Notes:         req.Notes,
	}

	if err := h.DB.Create(&shift).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to create shift",
			},
		})
		return
	}

	// Fetch shift with related data
	h.DB.Preload("Participant").Preload("Staff").First(&shift, "id = ?", shift.ID)

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    shift,
		"message": "Shift created successfully",
	})
}

type UpdateShiftRequest struct {
	StartTime       *string  `json:"start_time,omitempty"`        // Accept string for easier frontend integration
	EndTime         *string  `json:"end_time,omitempty"`          // Accept string for easier frontend integration
	ActualStartTime *string  `json:"actual_start_time,omitempty"` // Accept string for easier frontend integration
	ActualEndTime   *string  `json:"actual_end_time,omitempty"`   // Accept string for easier frontend integration
	ServiceType     *string  `json:"service_type,omitempty"`
	Location        *string  `json:"location,omitempty"`
	HourlyRate      *float64 `json:"hourly_rate,omitempty" binding:"omitempty,gt=0"`
	Notes           *string  `json:"notes,omitempty"`
	CompletionNotes *string  `json:"completion_notes,omitempty"`
}

func (h *Handler) UpdateShift(c *gin.Context) {
	shiftID := c.Param("id")
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

	var req UpdateShiftRequest
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

	// Find shift with access control - same logic as GetShifts
	var shift models.Shift
	var query *gorm.DB
	if userRole == "super_admin" {
		query = h.DB.Model(&models.Shift{}).Where("id = ?", shiftID)
	} else {
		query = h.DB.Joins("JOIN participants ON shifts.participant_id = participants.id").
			Where("shifts.id = ? AND participants.organization_id = ?", shiftID, orgID)
	}
	
	if err := query.First(&shift).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "SHIFT_NOT_FOUND",
					"message": "Shift not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch shift",
			},
		})
		return
	}

	// Parse and validate time ranges if being updated
	startTime := shift.StartTime
	endTime := shift.EndTime
	
	if req.StartTime != nil {
		if parsedStart, err := parseTimeFromString(*req.StartTime); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "INVALID_START_TIME",
					"message": "Invalid start time format",
					"details": err.Error(),
				},
			})
			return
		} else {
			startTime = parsedStart
		}
	}
	
	if req.EndTime != nil {
		if parsedEnd, err := parseTimeFromString(*req.EndTime); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "INVALID_END_TIME",
					"message": "Invalid end time format",
					"details": err.Error(),
				},
			})
			return
		} else {
			endTime = parsedEnd
		}
	}
	
	if endTime.Before(startTime) || endTime.Equal(startTime) {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_TIME_RANGE",
				"message": "End time must be after start time",
			},
		})
		return
	}

	// Check for overlapping shifts if time is being changed
	if (req.StartTime != nil || req.EndTime != nil) && shift.Status != "cancelled" && shift.Status != "completed" {
		var overlappingShifts int64
		h.DB.Model(&models.Shift{}).
			Where("staff_id = ? AND id != ? AND status NOT IN (?, ?) AND ((start_time <= ? AND end_time > ?) OR (start_time < ? AND end_time >= ?))",
				shift.StaffID, shiftID, "cancelled", "completed", startTime, startTime, endTime, endTime).
			Count(&overlappingShifts)

		if overlappingShifts > 0 {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "SCHEDULE_CONFLICT",
					"message": "Staff member already has a shift scheduled during this time",
				},
			})
			return
		}
	}

	// Update fields
	updates := make(map[string]interface{})
	hourlyRate := shift.HourlyRate // Default to current rate
	timeChanged := false
	
	if req.StartTime != nil {
		updates["start_time"] = startTime
		timeChanged = true
	}
	if req.EndTime != nil {
		updates["end_time"] = endTime
		timeChanged = true
	}
	if req.ActualStartTime != nil {
		if actualStart, err := parseTimeFromString(*req.ActualStartTime); err == nil {
			updates["actual_start_time"] = actualStart
		}
	}
	if req.ActualEndTime != nil {
		if actualEnd, err := parseTimeFromString(*req.ActualEndTime); err == nil {
			updates["actual_end_time"] = actualEnd
		}
	}
	if req.ServiceType != nil {
		updates["service_type"] = *req.ServiceType
	}
	if req.Location != nil {
		updates["location"] = *req.Location
	}
	if req.HourlyRate != nil {
		updates["hourly_rate"] = *req.HourlyRate
		hourlyRate = *req.HourlyRate
		timeChanged = true // Rate change also affects total cost
	}
	if req.Notes != nil {
		updates["notes"] = *req.Notes
	}
	if req.CompletionNotes != nil {
		updates["completion_notes"] = *req.CompletionNotes
	}
	
	// CRITICAL FIX: Recalculate total cost when time or rate changes
	if timeChanged && endTime.After(startTime) {
		duration := endTime.Sub(startTime).Hours()
		totalCost := duration * hourlyRate
		updates["total_cost"] = totalCost
	}

	if err := h.DB.Model(&shift).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update shift",
			},
		})
		return
	}

	// Fetch updated shift
	h.DB.Preload("Participant").Preload("Staff").First(&shift, "id = ?", shiftID)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    shift,
		"message": "Shift updated successfully",
	})
}

type UpdateShiftStatusRequest struct {
	Status          string  `json:"status" binding:"required,oneof=scheduled in_progress completed cancelled no_show"`
	CompletionNotes *string `json:"completion_notes,omitempty"`
	ActualStartTime *string `json:"actual_start_time,omitempty"` // Accept string for easier frontend integration
	ActualEndTime   *string `json:"actual_end_time,omitempty"`   // Accept string for easier frontend integration
}

func (h *Handler) UpdateShiftStatus(c *gin.Context) {
	shiftID := c.Param("id")
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

	userID, userExists := c.Get("user_id")
	if !userExists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "User ID not found in context",
			},
		})
		return
	}

	var req UpdateShiftStatusRequest
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

	// Find shift with access control
	var shift models.Shift
	if err := h.DB.Joins("JOIN participants ON shifts.participant_id = participants.id").
		Where("shifts.id = ? AND participants.organization_id = ?", shiftID, orgID).
		First(&shift).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "SHIFT_NOT_FOUND",
					"message": "Shift not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch shift",
			},
		})
		return
	}

	// Role-based permissions check
	role := fmt.Sprintf("%v", userRole)
	currentUserID := fmt.Sprintf("%v", userID)
	
	// Only admin and manager can edit shifts, staff can only start/complete their own shifts
	canEdit := role == "admin" || role == "manager"
	isOwnShift := shift.StaffID == currentUserID
	
	// For status changes, staff can only modify their own shifts and only for start/complete actions
	if !canEdit && !isOwnShift {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INSUFFICIENT_PERMISSIONS",
				"message": "You can only modify your own assigned shifts",
			},
		})
		return
	}
	
	// Staff can only start/complete shifts, not cancel or reschedule
	if !canEdit && (req.Status == "cancelled" || req.Status == "no_show") {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INSUFFICIENT_PERMISSIONS", 
				"message": "Only managers and admins can cancel or mark shifts as no-show",
			},
		})
		return
	}

	// Validate status transitions
	validTransitions := map[string][]string{
		"scheduled":   {"in_progress", "cancelled", "no_show"},
		"in_progress": {"completed", "cancelled"},
		"completed":   {}, // Final state
		"cancelled":   {"scheduled"}, // Can be rescheduled
		"no_show":     {"scheduled"}, // Can be rescheduled
	}

	allowedTransitions, exists := validTransitions[shift.Status]
	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_STATUS",
				"message": "Current shift status is invalid",
			},
		})
		return
	}

	// Check if transition is allowed
	isValidTransition := false
	for _, allowedStatus := range allowedTransitions {
		if req.Status == allowedStatus {
			isValidTransition = true
			break
		}
	}

	if !isValidTransition && req.Status != shift.Status {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_TRANSITION",
				"message": "Invalid status transition from " + shift.Status + " to " + req.Status,
			},
		})
		return
	}

	// Special validation for starting shifts (30-minute rule)
	if req.Status == "in_progress" && shift.Status == "scheduled" {
		now := time.Now()
		shiftStart := shift.StartTime
		
		// Check if trying to start more than 30 minutes before scheduled time
		if now.Before(shiftStart.Add(-30 * time.Minute)) {
			minutesEarly := int(shiftStart.Sub(now).Minutes())
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "TOO_EARLY_TO_START",
					"message": fmt.Sprintf("Cannot start shift more than 30 minutes early. Shift starts in %d minutes.", minutesEarly),
				},
			})
			return
		}
	}

	// Update fields
	updates := map[string]interface{}{
		"status": req.Status,
	}

	if req.CompletionNotes != nil {
		updates["completion_notes"] = *req.CompletionNotes
	}
	if req.ActualStartTime != nil {
		if actualStart, err := parseTimeFromString(*req.ActualStartTime); err == nil {
			updates["actual_start_time"] = actualStart
		}
	}
	if req.ActualEndTime != nil {
		if actualEnd, err := parseTimeFromString(*req.ActualEndTime); err == nil {
			updates["actual_end_time"] = actualEnd
		}
	}

	// Auto-set actual times for certain transitions
	now := time.Now()
	if req.Status == "in_progress" && shift.ActualStartTime == nil {
		updates["actual_start_time"] = now
	}
	if req.Status == "completed" && shift.ActualEndTime == nil {
		updates["actual_end_time"] = now
	}

	if err := h.DB.Model(&shift).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update shift status",
			},
		})
		return
	}

	// Fetch updated shift
	h.DB.Preload("Participant").Preload("Staff").First(&shift, "id = ?", shiftID)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    shift,
		"message": "Shift status updated successfully",
	})
}

func (h *Handler) DeleteShift(c *gin.Context) {
	shiftID := c.Param("id")
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

	// Find shift with access control
	var shift models.Shift
	if err := h.DB.Joins("JOIN participants ON shifts.participant_id = participants.id").
		Where("shifts.id = ? AND participants.organization_id = ?", shiftID, orgID).
		First(&shift).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "SHIFT_NOT_FOUND",
					"message": "Shift not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch shift",
			},
		})
		return
	}

	// Only allow deletion of scheduled or cancelled shifts
	if shift.Status != "scheduled" && shift.Status != "cancelled" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_OPERATION",
				"message": "Only scheduled or cancelled shifts can be deleted",
			},
		})
		return
	}

	// Soft delete shift
	if err := h.DB.Delete(&shift).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete shift",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Shift deleted successfully",
	})
}
