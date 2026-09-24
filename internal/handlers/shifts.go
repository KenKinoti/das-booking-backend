package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

// Shifts are staff roster entries. They belong to the organisation of the
// staff member working them; a participant (care recipient) is optional so
// the roster works for any kind of business.

var shiftStatuses = map[string]bool{
	"scheduled": true, "confirmed": true, "in_progress": true, "completed": true, "cancelled": true, "no_show": true,
}

// parseTimeFromString accepts RFC3339 or local "YYYY-MM-DDTHH:MM[:SS]" values.
func parseTimeFromString(timeStr string) (time.Time, error) {
	timeStr = strings.TrimSpace(timeStr)
	for _, f := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(f, timeStr); err == nil {
			return t, nil
		}
	}
	for _, f := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(f, timeStr, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &time.ParseError{Value: timeStr}
}

func shiftErr(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func canManageShifts(role string) bool {
	return role == "admin" || role == "manager" || role == "super_admin"
}

// orgShifts scopes a shift query to the caller's organisation via the staff member.
func (h *Handler) orgShifts(orgID string) *gorm.DB {
	return h.DB.Model(&models.Shift{}).
		Where("shifts.staff_id IN (SELECT id FROM users WHERE organization_id = ?)", orgID)
}

func (h *Handler) loadShift(c *gin.Context) (*models.Shift, bool) {
	var shift models.Shift
	if err := h.orgShifts(c.GetString("org_id")).Where("shifts.id = ?", c.Param("id")).First(&shift).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			shiftErr(c, http.StatusNotFound, "SHIFT_NOT_FOUND", "Shift not found")
		} else {
			shiftErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to fetch shift")
		}
		return nil, false
	}
	return &shift, true
}

func (h *Handler) reloadShift(id string) models.Shift {
	var shift models.Shift
	h.DB.Preload("Staff").Preload("Participant").First(&shift, "id = ?", id)
	return shift
}

// overlap reports whether the staff member already has an open shift in the range.
func (h *Handler) shiftOverlaps(staffID, excludeID string, start, end time.Time) bool {
	var n int64
	h.DB.Model(&models.Shift{}).
		Where("staff_id = ? AND id <> ? AND status NOT IN ? AND start_time < ? AND end_time > ?",
			staffID, excludeID, []string{"cancelled", "no_show"}, end, start).
		Count(&n)
	return n > 0
}

// GetShifts lists shifts. Query: start_date, end_date (YYYY-MM-DD, inclusive),
// staff_id, participant_id, status, service_type, page, limit.
func (h *Handler) GetShifts(c *gin.Context) {
	orgID := c.GetString("org_id")
	if orgID == "" {
		shiftErr(c, http.StatusUnauthorized, "UNAUTHORIZED", "Organization not found in context")
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}

	query := h.orgShifts(orgID)
	if v := c.Query("staff_id"); v != "" {
		query = query.Where("shifts.staff_id = ?", v)
	}
	if v := c.Query("participant_id"); v != "" {
		query = query.Where("shifts.participant_id = ?", v)
	}
	if v := c.Query("status"); v != "" {
		query = query.Where("shifts.status IN ?", strings.Split(v, ","))
	}
	if v := c.Query("service_type"); v != "" {
		query = query.Where("shifts.service_type = ?", v)
	}
	if v := c.Query("start_date"); v != "" {
		if d, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
			query = query.Where("shifts.end_time > ?", d)
		} else if t, err := parseTimeFromString(v); err == nil {
			query = query.Where("shifts.end_time > ?", t)
		}
	}
	if v := c.Query("end_date"); v != "" {
		if d, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
			query = query.Where("shifts.start_time < ?", d.Add(24*time.Hour))
		} else if t, err := parseTimeFromString(v); err == nil {
			query = query.Where("shifts.start_time < ?", t)
		}
	}

	var total int64
	query.Session(&gorm.Session{}).Count(&total)

	order := "shifts.start_time DESC"
	if c.Query("order") == "asc" {
		order = "shifts.start_time ASC"
	}
	var shifts []models.Shift
	if err := query.Preload("Participant").Preload("Staff").
		Limit(limit).Offset((page - 1) * limit).Order(order).
		Find(&shifts).Error; err != nil {
		shiftErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to fetch shifts")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"shifts": shifts,
			"pagination": gin.H{
				"page": page, "limit": limit, "total": total,
				"total_pages": (total + int64(limit) - 1) / int64(limit),
			},
		},
	})
}

func (h *Handler) GetShift(c *gin.Context) {
	shift, ok := h.loadShift(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.reloadShift(shift.ID)})
}

type CreateShiftRequest struct {
	ParticipantID string   `json:"participant_id"`
	StaffID       string   `json:"staff_id" binding:"required"`
	StartTime     string   `json:"start_time" binding:"required"`
	EndTime       string   `json:"end_time" binding:"required"`
	ServiceType   string   `json:"service_type"`
	Location      string   `json:"location"`
	HourlyRate    *float64 `json:"hourly_rate"`
	Notes         string   `json:"notes"`
	Status        string   `json:"status"`
}

func (h *Handler) CreateShift(c *gin.Context) {
	orgID := c.GetString("org_id")
	if orgID == "" {
		shiftErr(c, http.StatusUnauthorized, "UNAUTHORIZED", "Organization not found in context")
		return
	}
	if !canManageShifts(c.GetString("user_role")) {
		shiftErr(c, http.StatusForbidden, "FORBIDDEN", "Only admins and managers can create shifts")
		return
	}

	var req CreateShiftRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		shiftErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose a staff member, start and end time")
		return
	}
	start, err := parseTimeFromString(req.StartTime)
	if err != nil {
		shiftErr(c, http.StatusBadRequest, "INVALID_START_TIME", "Invalid start time")
		return
	}
	end, err := parseTimeFromString(req.EndTime)
	if err != nil {
		shiftErr(c, http.StatusBadRequest, "INVALID_END_TIME", "Invalid end time")
		return
	}
	if !end.After(start) {
		shiftErr(c, http.StatusBadRequest, "INVALID_TIME_RANGE", "End time must be after start time")
		return
	}
	if end.Sub(start) > 24*time.Hour {
		shiftErr(c, http.StatusBadRequest, "INVALID_TIME_RANGE", "A shift cannot be longer than 24 hours")
		return
	}
	rate := 0.0
	if req.HourlyRate != nil {
		if *req.HourlyRate < 0 {
			shiftErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Hourly rate cannot be negative")
			return
		}
		rate = *req.HourlyRate
	}
	status := "scheduled"
	if req.Status != "" {
		if !shiftStatuses[req.Status] {
			shiftErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid status")
			return
		}
		status = req.Status
	}

	var staff models.User
	if err := h.DB.Where("id = ? AND organization_id = ?", req.StaffID, orgID).First(&staff).Error; err != nil {
		shiftErr(c, http.StatusBadRequest, "INVALID_STAFF", "Choose a staff member from your organisation")
		return
	}
	if !staff.IsActive {
		shiftErr(c, http.StatusBadRequest, "INVALID_STAFF", staff.FirstName+" is deactivated")
		return
	}
	if req.ParticipantID != "" {
		var n int64
		h.DB.Model(&models.Participant{}).Where("id = ? AND organization_id = ?", req.ParticipantID, orgID).Count(&n)
		if n == 0 {
			shiftErr(c, http.StatusBadRequest, "INVALID_PARTICIPANT", "Participant not found")
			return
		}
	}
	if status != "cancelled" && h.shiftOverlaps(staff.ID, "", start, end) {
		shiftErr(c, http.StatusConflict, "SHIFT_OVERLAP", staff.FirstName+" already has a shift at this time")
		return
	}

	serviceType := strings.TrimSpace(req.ServiceType)
	if serviceType == "" {
		serviceType = "General"
	}
	shift := models.Shift{
		ID:            uuid.NewString(),
		ParticipantID: req.ParticipantID,
		StaffID:       staff.ID,
		StartTime:     start,
		EndTime:       end,
		ServiceType:   serviceType,
		Location:      strings.TrimSpace(req.Location),
		Status:        status,
		HourlyRate:    rate,
		TotalCost:     end.Sub(start).Hours() * rate,
		Notes:         strings.TrimSpace(req.Notes),
	}
	if err := h.DB.Omit("Participant", "Staff").Create(&shift).Error; err != nil {
		shiftErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to create shift")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": h.reloadShift(shift.ID)})
}

type UpdateShiftRequest struct {
	StaffID         *string  `json:"staff_id,omitempty"`
	ParticipantID   *string  `json:"participant_id,omitempty"`
	StartTime       *string  `json:"start_time,omitempty"`
	EndTime         *string  `json:"end_time,omitempty"`
	ActualStartTime *string  `json:"actual_start_time,omitempty"`
	ActualEndTime   *string  `json:"actual_end_time,omitempty"`
	ServiceType     *string  `json:"service_type,omitempty"`
	Location        *string  `json:"location,omitempty"`
	HourlyRate      *float64 `json:"hourly_rate,omitempty"`
	Notes           *string  `json:"notes,omitempty"`
	CompletionNotes *string  `json:"completion_notes,omitempty"`
	Status          *string  `json:"status,omitempty"`
}

func (h *Handler) UpdateShift(c *gin.Context) {
	orgID := c.GetString("org_id")
	if !canManageShifts(c.GetString("user_role")) {
		shiftErr(c, http.StatusForbidden, "FORBIDDEN", "Only admins and managers can edit shifts")
		return
	}
	var req UpdateShiftRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		shiftErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	shift, ok := h.loadShift(c)
	if !ok {
		return
	}

	updates := map[string]interface{}{}
	start, end := shift.StartTime, shift.EndTime
	staffID := shift.StaffID
	rate := shift.HourlyRate
	status := shift.Status

	if req.StartTime != nil {
		t, err := parseTimeFromString(*req.StartTime)
		if err != nil {
			shiftErr(c, http.StatusBadRequest, "INVALID_START_TIME", "Invalid start time")
			return
		}
		start = t
		updates["start_time"] = t
	}
	if req.EndTime != nil {
		t, err := parseTimeFromString(*req.EndTime)
		if err != nil {
			shiftErr(c, http.StatusBadRequest, "INVALID_END_TIME", "Invalid end time")
			return
		}
		end = t
		updates["end_time"] = t
	}
	if !end.After(start) {
		shiftErr(c, http.StatusBadRequest, "INVALID_TIME_RANGE", "End time must be after start time")
		return
	}
	if end.Sub(start) > 24*time.Hour {
		shiftErr(c, http.StatusBadRequest, "INVALID_TIME_RANGE", "A shift cannot be longer than 24 hours")
		return
	}
	if req.StaffID != nil && *req.StaffID != shift.StaffID {
		var staff models.User
		if err := h.DB.Where("id = ? AND organization_id = ? AND is_active = ?", *req.StaffID, orgID, true).First(&staff).Error; err != nil {
			shiftErr(c, http.StatusBadRequest, "INVALID_STAFF", "Choose an active staff member from your organisation")
			return
		}
		staffID = staff.ID
		updates["staff_id"] = staff.ID
	}
	if req.ParticipantID != nil {
		if *req.ParticipantID != "" {
			var n int64
			h.DB.Model(&models.Participant{}).Where("id = ? AND organization_id = ?", *req.ParticipantID, orgID).Count(&n)
			if n == 0 {
				shiftErr(c, http.StatusBadRequest, "INVALID_PARTICIPANT", "Participant not found")
				return
			}
		}
		updates["participant_id"] = *req.ParticipantID
	}
	if req.Status != nil {
		if !shiftStatuses[*req.Status] {
			shiftErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid status")
			return
		}
		status = *req.Status
		updates["status"] = status
	}
	if req.HourlyRate != nil {
		if *req.HourlyRate < 0 {
			shiftErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Hourly rate cannot be negative")
			return
		}
		rate = *req.HourlyRate
		updates["hourly_rate"] = rate
	}
	if req.ServiceType != nil {
		v := strings.TrimSpace(*req.ServiceType)
		if v == "" {
			v = "General"
		}
		updates["service_type"] = v
	}
	if req.Location != nil {
		updates["location"] = strings.TrimSpace(*req.Location)
	}
	if req.Notes != nil {
		updates["notes"] = *req.Notes
	}
	if req.CompletionNotes != nil {
		updates["completion_notes"] = *req.CompletionNotes
	}
	if req.ActualStartTime != nil {
		if t, err := parseTimeFromString(*req.ActualStartTime); err == nil {
			updates["actual_start_time"] = t
		}
	}
	if req.ActualEndTime != nil {
		if t, err := parseTimeFromString(*req.ActualEndTime); err == nil {
			updates["actual_end_time"] = t
		}
	}

	if status != "cancelled" && status != "no_show" && h.shiftOverlaps(staffID, shift.ID, start, end) {
		shiftErr(c, http.StatusConflict, "SHIFT_OVERLAP", "This staff member already has a shift at this time")
		return
	}
	updates["total_cost"] = end.Sub(start).Hours() * rate

	if err := h.DB.Model(&models.Shift{}).Where("id = ?", shift.ID).Updates(updates).Error; err != nil {
		shiftErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to update shift")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.reloadShift(shift.ID)})
}

type UpdateShiftStatusRequest struct {
	Status          string  `json:"status" binding:"required"`
	CompletionNotes *string `json:"completion_notes,omitempty"`
	ActualStartTime *string `json:"actual_start_time,omitempty"`
	ActualEndTime   *string `json:"actual_end_time,omitempty"`
}

// UpdateShiftStatus changes a shift's status. Managers can set any status;
// staff can only start and complete their own shifts.
func (h *Handler) UpdateShiftStatus(c *gin.Context) {
	var req UpdateShiftStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil || !shiftStatuses[req.Status] {
		shiftErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid status")
		return
	}
	shift, ok := h.loadShift(c)
	if !ok {
		return
	}

	role := c.GetString("user_role")
	if !canManageShifts(role) {
		if shift.StaffID != c.GetString("user_id") {
			shiftErr(c, http.StatusForbidden, "FORBIDDEN", "You can only update your own shifts")
			return
		}
		allowed := (shift.Status == "scheduled" || shift.Status == "confirmed") && req.Status == "in_progress" ||
			shift.Status == "in_progress" && req.Status == "completed"
		if !allowed {
			shiftErr(c, http.StatusForbidden, "FORBIDDEN", "You can only start or complete your shift")
			return
		}
		if req.Status == "in_progress" && time.Now().Before(shift.StartTime.Add(-30*time.Minute)) {
			shiftErr(c, http.StatusBadRequest, "TOO_EARLY", "Shifts can be started at most 30 minutes early")
			return
		}
	}

	updates := map[string]interface{}{"status": req.Status}
	if req.CompletionNotes != nil {
		updates["completion_notes"] = *req.CompletionNotes
	}
	if req.ActualStartTime != nil {
		if t, err := parseTimeFromString(*req.ActualStartTime); err == nil {
			updates["actual_start_time"] = t
		}
	}
	if req.ActualEndTime != nil {
		if t, err := parseTimeFromString(*req.ActualEndTime); err == nil {
			updates["actual_end_time"] = t
		}
	}
	now := time.Now()
	if req.Status == "in_progress" && shift.ActualStartTime == nil {
		updates["actual_start_time"] = now
	}
	if req.Status == "completed" && shift.ActualEndTime == nil {
		updates["actual_end_time"] = now
	}
	if (req.Status == "scheduled" || req.Status == "confirmed") && h.shiftOverlaps(shift.StaffID, shift.ID, shift.StartTime, shift.EndTime) {
		shiftErr(c, http.StatusConflict, "SHIFT_OVERLAP", "This staff member already has a shift at this time")
		return
	}

	if err := h.DB.Model(&models.Shift{}).Where("id = ?", shift.ID).Updates(updates).Error; err != nil {
		shiftErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to update shift status")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.reloadShift(shift.ID)})
}

func (h *Handler) DeleteShift(c *gin.Context) {
	if !canManageShifts(c.GetString("user_role")) {
		shiftErr(c, http.StatusForbidden, "FORBIDDEN", "Only admins and managers can delete shifts")
		return
	}
	shift, ok := h.loadShift(c)
	if !ok {
		return
	}
	if shift.Status == "in_progress" || shift.Status == "completed" {
		shiftErr(c, http.StatusBadRequest, "INVALID_OPERATION", "Shifts that have started or finished cannot be deleted; cancel them instead")
		return
	}
	if err := h.DB.Delete(&models.Shift{}, "id = ?", shift.ID).Error; err != nil {
		shiftErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to delete shift")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Shift deleted"})
}
