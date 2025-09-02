package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

// GetBookings retrieves all bookings for the organization with optional filters
func (h *Handler) GetBookings(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	// Parse query parameters for filtering
	status := c.Query("status")
	customerID := c.Query("customer_id")
	vehicleID := c.Query("vehicle_id")
	staffID := c.Query("staff_id")
	dateFrom := c.Query("date_from")
	dateTo := c.Query("date_to")

	query := h.DB.Where("organization_id = ?", orgID).Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services")

	// Apply filters
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if customerID != "" {
		query = query.Where("customer_id = ?", customerID)
	}
	if vehicleID != "" {
		query = query.Where("vehicle_id = ?", vehicleID)
	}
	if staffID != "" {
		query = query.Where("staff_id = ?", staffID)
	}
	if dateFrom != "" {
		if parsedDate, err := time.Parse("2006-01-02", dateFrom); err == nil {
			query = query.Where("start_time >= ?", parsedDate)
		}
	}
	if dateTo != "" {
		if parsedDate, err := time.Parse("2006-01-02", dateTo); err == nil {
			query = query.Where("start_time <= ?", parsedDate.Add(24*time.Hour))
		}
	}

	var bookings []models.Booking
	if err := query.Order("start_time ASC").Find(&bookings).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch bookings"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"bookings": bookings})
}

// GetBooking retrieves a specific booking by ID
func (h *Handler) GetBooking(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var booking models.Booking
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).
		Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services").
		First(&booking).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch booking"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"booking": booking})
}

// CreateBooking creates a new booking
func (h *Handler) CreateBooking(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var request struct {
		CustomerID    string    `json:"customer_id" binding:"required"`
		VehicleID     *string   `json:"vehicle_id"`
		StaffID       *string   `json:"staff_id"`
		StartTime     time.Time `json:"start_time" binding:"required"`
		EndTime       time.Time `json:"end_time" binding:"required"`
		ServiceIDs    []string  `json:"service_ids" binding:"required"`
		Notes         string    `json:"notes"`
		InternalNotes string    `json:"internal_notes"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate customer exists and belongs to organization
	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", request.CustomerID, orgID).First(&customer).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid customer ID"})
		return
	}

	// Validate vehicle if provided
	if request.VehicleID != nil {
		var vehicle models.Vehicle
		if err := h.DB.Where("id = ? AND customer_id = ? AND organization_id = ?", *request.VehicleID, request.CustomerID, orgID).First(&vehicle).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid vehicle ID"})
			return
		}
	}

	// Validate staff if provided
	if request.StaffID != nil {
		var staff models.User
		if err := h.DB.Where("id = ? AND organization_id = ?", *request.StaffID, orgID).First(&staff).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid staff ID"})
			return
		}
	}

	// Validate services exist and belong to organization
	var services []models.Service
	if err := h.DB.Where("id IN ? AND organization_id = ?", request.ServiceIDs, orgID).Find(&services).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid service IDs"})
		return
	}

	if len(services) != len(request.ServiceIDs) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Some service IDs are invalid"})
		return
	}

	// Check for booking conflicts
	var conflictCount int64
	conflictQuery := h.DB.Model(&models.Booking{}).Where(
		"organization_id = ? AND status NOT IN ? AND ((start_time <= ? AND end_time > ?) OR (start_time < ? AND end_time >= ?))",
		orgID, []string{"cancelled", "no_show"}, request.StartTime, request.StartTime, request.EndTime, request.EndTime,
	)

	if request.StaffID != nil {
		conflictQuery = conflictQuery.Where("staff_id = ?", *request.StaffID)
	}

	if request.VehicleID != nil {
		conflictQuery = conflictQuery.Where("vehicle_id = ?", *request.VehicleID)
	}

	conflictQuery.Count(&conflictCount)
	if conflictCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Booking time conflicts with existing appointment"})
		return
	}

	// Calculate total price
	var totalPrice float64
	for _, service := range services {
		totalPrice += service.Price
	}

	// Create booking
	booking := models.Booking{
		CustomerID:     request.CustomerID,
		OrganizationID: orgID,
		VehicleID:      request.VehicleID,
		StaffID:        request.StaffID,
		StartTime:      request.StartTime,
		EndTime:        request.EndTime,
		Status:         "scheduled",
		TotalPrice:     totalPrice,
		Notes:          request.Notes,
		InternalNotes:  request.InternalNotes,
	}

	// Start transaction
	tx := h.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if err := tx.Create(&booking).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create booking"})
		return
	}

	// Associate services with booking
	if err := tx.Model(&booking).Association("Services").Append(&services); err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to associate services"})
		return
	}

	tx.Commit()

	// Reload booking with relationships
	h.DB.Where("id = ?", booking.ID).
		Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services").
		First(&booking)

	c.JSON(http.StatusCreated, gin.H{"booking": booking})
}

// UpdateBooking updates an existing booking
func (h *Handler) UpdateBooking(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var booking models.Booking
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&booking).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch booking"})
		return
	}

	var request struct {
		VehicleID     *string   `json:"vehicle_id"`
		StaffID       *string   `json:"staff_id"`
		StartTime     time.Time `json:"start_time"`
		EndTime       time.Time `json:"end_time"`
		Status        string    `json:"status"`
		ServiceIDs    []string  `json:"service_ids"`
		Notes         string    `json:"notes"`
		InternalNotes string    `json:"internal_notes"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate vehicle if provided
	if request.VehicleID != nil {
		var vehicle models.Vehicle
		if err := h.DB.Where("id = ? AND customer_id = ? AND organization_id = ?", *request.VehicleID, booking.CustomerID, orgID).First(&vehicle).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid vehicle ID"})
			return
		}
	}

	// Validate staff if provided
	if request.StaffID != nil {
		var staff models.User
		if err := h.DB.Where("id = ? AND organization_id = ?", *request.StaffID, orgID).First(&staff).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid staff ID"})
			return
		}
	}

	// Update booking
	updates := make(map[string]interface{})
	
	if request.VehicleID != nil {
		updates["vehicle_id"] = *request.VehicleID
	}
	if request.StaffID != nil {
		updates["staff_id"] = *request.StaffID
	}
	if !request.StartTime.IsZero() {
		updates["start_time"] = request.StartTime
	}
	if !request.EndTime.IsZero() {
		updates["end_time"] = request.EndTime
	}
	if request.Status != "" {
		updates["status"] = request.Status
	}
	if request.Notes != "" {
		updates["notes"] = request.Notes
	}
	if request.InternalNotes != "" {
		updates["internal_notes"] = request.InternalNotes
	}

	// Start transaction
	tx := h.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if err := tx.Model(&booking).Updates(updates).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update booking"})
		return
	}

	// Update services if provided
	if len(request.ServiceIDs) > 0 {
		var services []models.Service
		if err := tx.Where("id IN ? AND organization_id = ?", request.ServiceIDs, orgID).Find(&services).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid service IDs"})
			return
		}

		if len(services) != len(request.ServiceIDs) {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": "Some service IDs are invalid"})
			return
		}

		// Calculate new total price
		var totalPrice float64
		for _, service := range services {
			totalPrice += service.Price
		}

		// Update services association
		if err := tx.Model(&booking).Association("Services").Replace(&services); err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update services"})
			return
		}

		// Update total price
		if err := tx.Model(&booking).Update("total_price", totalPrice).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update total price"})
			return
		}
	}

	tx.Commit()

	// Reload booking with relationships
	h.DB.Where("id = ?", booking.ID).
		Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services").
		First(&booking)

	c.JSON(http.StatusOK, gin.H{"booking": booking})
}

// DeleteBooking soft deletes a booking
func (h *Handler) DeleteBooking(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var booking models.Booking
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&booking).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch booking"})
		return
	}

	if err := h.DB.Delete(&booking).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete booking"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Booking deleted successfully"})
}

// GetAvailableTimeSlots returns available booking time slots for a given date
func (h *Handler) GetAvailableTimeSlots(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	date := c.Query("date")
	staffID := c.Query("staff_id")
	duration, _ := strconv.Atoi(c.DefaultQuery("duration", "60")) // Default 60 minutes

	if date == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Date parameter is required"})
		return
	}

	parsedDate, err := time.Parse("2006-01-02", date)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid date format. Use YYYY-MM-DD"})
		return
	}

	// Get organization to check business hours
	var org models.Organization
	if err := h.DB.Where("id = ?", orgID).First(&org).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch organization"})
		return
	}

	// Get business hours for the day
	dayOfWeek := parsedDate.Weekday()
	var openTime, closeTime string

	switch dayOfWeek {
	case time.Monday:
		openTime, closeTime = org.BusinessHours.MondayOpen, org.BusinessHours.MondayClose
	case time.Tuesday:
		openTime, closeTime = org.BusinessHours.TuesdayOpen, org.BusinessHours.TuesdayClose
	case time.Wednesday:
		openTime, closeTime = org.BusinessHours.WednesdayOpen, org.BusinessHours.WednesdayClose
	case time.Thursday:
		openTime, closeTime = org.BusinessHours.ThursdayOpen, org.BusinessHours.ThursdayClose
	case time.Friday:
		openTime, closeTime = org.BusinessHours.FridayOpen, org.BusinessHours.FridayClose
	case time.Saturday:
		openTime, closeTime = org.BusinessHours.SaturdayOpen, org.BusinessHours.SaturdayClose
	case time.Sunday:
		openTime, closeTime = org.BusinessHours.SundayOpen, org.BusinessHours.SundayClose
	}

	if openTime == "" || closeTime == "" {
		c.JSON(http.StatusOK, gin.H{"available_slots": []string{}})
		return
	}

	// Parse business hours
	startTime, err := time.Parse("15:04", openTime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid business hours format"})
		return
	}

	endTime, err := time.Parse("15:04", closeTime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid business hours format"})
		return
	}

	// Convert to full datetime
	startDateTime := time.Date(parsedDate.Year(), parsedDate.Month(), parsedDate.Day(), startTime.Hour(), startTime.Minute(), 0, 0, parsedDate.Location())
	endDateTime := time.Date(parsedDate.Year(), parsedDate.Month(), parsedDate.Day(), endTime.Hour(), endTime.Minute(), 0, 0, parsedDate.Location())

	// Get existing bookings for the day
	var bookings []models.Booking
	query := h.DB.Where("organization_id = ? AND DATE(start_time) = ? AND status NOT IN ?", 
		orgID, parsedDate.Format("2006-01-02"), []string{"cancelled", "no_show"})
	
	if staffID != "" {
		query = query.Where("staff_id = ?", staffID)
	}
	
	query.Find(&bookings)

	// Generate time slots
	slotDuration := time.Duration(duration) * time.Minute
	bufferTime := time.Duration(org.BookingSettings.BufferTime) * time.Minute
	var availableSlots []string

	for current := startDateTime; current.Add(slotDuration).Before(endDateTime) || current.Add(slotDuration).Equal(endDateTime); current = current.Add(slotDuration + bufferTime) {
		slotEnd := current.Add(slotDuration)
		
		// Check if this slot conflicts with any existing booking
		conflict := false
		for _, booking := range bookings {
			if (current.Before(booking.EndTime) && slotEnd.After(booking.StartTime)) {
				conflict = true
				break
			}
		}

		if !conflict {
			availableSlots = append(availableSlots, current.Format("15:04"))
		}
	}

	c.JSON(http.StatusOK, gin.H{"available_slots": availableSlots})
}

// UpdateBookingStatus updates just the status of a booking
func (h *Handler) UpdateBookingStatus(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var request struct {
		Status string `json:"status" binding:"required"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	validStatuses := []string{"scheduled", "confirmed", "in_progress", "completed", "cancelled", "no_show"}
	valid := false
	for _, status := range validStatuses {
		if request.Status == status {
			valid = true
			break
		}
	}

	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid status"})
		return
	}

	var booking models.Booking
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&booking).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch booking"})
		return
	}

	if err := h.DB.Model(&booking).Update("status", request.Status).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update booking status"})
		return
	}

	// Reload booking with relationships
	h.DB.Where("id = ?", booking.ID).
		Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services").
		First(&booking)

	c.JSON(http.StatusOK, gin.H{"booking": booking})
}