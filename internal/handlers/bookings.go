package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

var validBookingStatuses = map[string]bool{
	"scheduled":   true,
	"confirmed":   true,
	"in_progress": true,
	"completed":   true,
	"cancelled":   true,
	"no_show":     true,
}

// defaultBookingMinutes is used for the end time when no services are selected.
const defaultBookingMinutes = 60

// bookingParseTimeParam accepts an RFC3339 instant or a YYYY-MM-DD date
// (interpreted in the server's local time zone). isDate reports the latter.
func bookingParseTimeParam(v string) (t time.Time, isDate bool, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, false, true
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t, true, true
	}
	return time.Time{}, false, false
}

// bookingNilIfEmpty turns "" (or whitespace) into nil so optional FKs can be cleared.
func bookingNilIfEmpty(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	return &v
}

func (h *Handler) bookingPreload(db *gorm.DB) *gorm.DB {
	return db.Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services")
}

// bookingLoadServices loads and validates services for an organisation.
func (h *Handler) bookingLoadServices(db *gorm.DB, orgID string, ids []string) ([]models.Service, error) {
	services := []models.Service{}
	clean := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			clean = append(clean, id)
		}
	}
	if len(clean) == 0 {
		return services, nil
	}
	if err := db.Where("id IN ? AND organization_id = ?", clean, orgID).Find(&services).Error; err != nil {
		return nil, err
	}
	if len(services) != len(clean) {
		return nil, errors.New("one or more selected services are invalid")
	}
	return services, nil
}

func bookingServiceTotals(services []models.Service) (price float64, minutes int) {
	for _, s := range services {
		price += s.Price
		minutes += s.Duration
	}
	return
}

// bookingHasConflict checks for overlapping active bookings for the same staff
// member or vehicle. Bookings without staff or vehicle never conflict.
func (h *Handler) bookingHasConflict(orgID string, start, end time.Time, staffID, vehicleID *string, excludeID string) bool {
	if staffID == nil && vehicleID == nil {
		return false
	}
	q := h.DB.Model(&models.Booking{}).Where(
		"organization_id = ? AND status NOT IN ? AND start_time < ? AND end_time > ?",
		orgID, []string{"cancelled", "no_show", "completed"}, end, start,
	)
	if excludeID != "" {
		q = q.Where("id <> ?", excludeID)
	}
	switch {
	case staffID != nil && vehicleID != nil:
		q = q.Where("(staff_id = ? OR vehicle_id = ?)", *staffID, *vehicleID)
	case staffID != nil:
		q = q.Where("staff_id = ?", *staffID)
	default:
		q = q.Where("vehicle_id = ?", *vehicleID)
	}
	var n int64
	q.Count(&n)
	return n > 0
}

func (h *Handler) bookingValidateRefs(orgID, customerID string, vehicleID, staffID *string) (int, string) {
	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", customerID, orgID).First(&customer).Error; err != nil {
		return http.StatusBadRequest, "Please choose a valid customer"
	}
	if vehicleID != nil {
		var vehicle models.Vehicle
		if err := h.DB.Where("id = ? AND customer_id = ? AND organization_id = ?", *vehicleID, customerID, orgID).First(&vehicle).Error; err != nil {
			return http.StatusBadRequest, "The selected vehicle does not belong to this customer"
		}
	}
	if staffID != nil {
		var staff models.User
		if err := h.DB.Where("id = ? AND organization_id = ?", *staffID, orgID).First(&staff).Error; err != nil {
			return http.StatusBadRequest, "Please choose a valid staff member"
		}
	}
	return 0, ""
}

// GetBookings retrieves all bookings for the organization with optional filters.
// date_from / date_to accept YYYY-MM-DD (inclusive days) or RFC3339 instants
// (date_to exclusive).
func (h *Handler) GetBookings(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	query := h.bookingPreload(h.DB.Where("organization_id = ?", orgID))

	if status := c.Query("status"); status != "" {
		query = query.Where("status IN ?", strings.Split(status, ","))
	}
	if v := c.Query("customer_id"); v != "" {
		query = query.Where("customer_id = ?", v)
	}
	if v := c.Query("vehicle_id"); v != "" {
		query = query.Where("vehicle_id = ?", v)
	}
	if v := c.Query("staff_id"); v != "" {
		query = query.Where("staff_id = ?", v)
	}
	if t, _, ok := bookingParseTimeParam(c.Query("date_from")); ok {
		query = query.Where("start_time >= ?", t)
	}
	if t, isDate, ok := bookingParseTimeParam(c.Query("date_to")); ok {
		if isDate {
			t = t.AddDate(0, 0, 1)
		}
		query = query.Where("start_time < ?", t)
	}

	bookings := []models.Booking{}
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

	var booking models.Booking
	if err := h.bookingPreload(h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), orgID)).
		First(&booking).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch booking"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"booking": booking})
}

// CreateBooking creates a new booking. end_time and total_price are derived
// from the selected services when not supplied.
func (h *Handler) CreateBooking(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var request struct {
		CustomerID    string     `json:"customer_id"`
		VehicleID     *string    `json:"vehicle_id"`
		StaffID       *string    `json:"staff_id"`
		StartTime     *time.Time `json:"start_time"`
		EndTime       *time.Time `json:"end_time"`
		Status        string     `json:"status"`
		ServiceIDs    []string   `json:"service_ids"`
		TotalPrice    *float64   `json:"total_price"`
		Notes         string     `json:"notes"`
		InternalNotes string     `json:"internal_notes"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	request.CustomerID = strings.TrimSpace(request.CustomerID)
	if request.CustomerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Customer is required"})
		return
	}
	if request.StartTime == nil || request.StartTime.IsZero() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Start time is required"})
		return
	}
	status := strings.TrimSpace(request.Status)
	if status == "" {
		status = "scheduled"
	}
	if !validBookingStatuses[status] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid status"})
		return
	}
	vehicleID := bookingNilIfEmpty(request.VehicleID)
	staffID := bookingNilIfEmpty(request.StaffID)

	if code, msg := h.bookingValidateRefs(orgID, request.CustomerID, vehicleID, staffID); code != 0 {
		c.JSON(code, gin.H{"error": msg})
		return
	}

	services, err := h.bookingLoadServices(h.DB, orgID, request.ServiceIDs)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Some selected services are invalid"})
		return
	}
	price, minutes := bookingServiceTotals(services)
	if minutes <= 0 {
		minutes = defaultBookingMinutes
	}

	start := *request.StartTime
	end := start.Add(time.Duration(minutes) * time.Minute)
	if request.EndTime != nil && !request.EndTime.IsZero() {
		end = *request.EndTime
	}
	if !end.After(start) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "End time must be after the start time"})
		return
	}
	if request.TotalPrice != nil {
		if *request.TotalPrice < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Price cannot be negative"})
			return
		}
		price = *request.TotalPrice
	}

	if h.bookingHasConflict(orgID, start, end, staffID, vehicleID, "") {
		c.JSON(http.StatusConflict, gin.H{"error": "This time overlaps another booking for the same staff member or vehicle"})
		return
	}

	booking := models.Booking{
		CustomerID:     request.CustomerID,
		OrganizationID: orgID,
		VehicleID:      vehicleID,
		StaffID:        staffID,
		StartTime:      start,
		EndTime:        end,
		Status:         status,
		Notes:          strings.TrimSpace(request.Notes),
		InternalNotes:  strings.TrimSpace(request.InternalNotes),
	}

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Customer", "Organization", "Vehicle", "Staff", "Services").Create(&booking).Error; err != nil {
			return err
		}
		if len(services) > 0 {
			if err := tx.Model(&booking).Association("Services").Append(&services); err != nil {
				return err
			}
		}
		// The model's BeforeCreate hook recomputes TotalPrice from (empty)
		// in-memory services, so persist the real total explicitly.
		return tx.Model(&booking).UpdateColumn("total_price", price).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create booking"})
		return
	}

	h.bookingPreload(h.DB.Where("id = ?", booking.ID)).First(&booking)
	h.notifyBooking(c, &booking, "created", "")
	c.JSON(http.StatusCreated, gin.H{"booking": booking})
}

// UpdateBooking updates an existing booking. Only fields present in the body
// are changed; "" clears vehicle_id/staff_id; service_ids replaces services.
func (h *Handler) UpdateBooking(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var booking models.Booking
	if err := h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), orgID).First(&booking).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch booking"})
		return
	}

	var request struct {
		CustomerID    *string    `json:"customer_id"`
		VehicleID     *string    `json:"vehicle_id"`
		StaffID       *string    `json:"staff_id"`
		StartTime     *time.Time `json:"start_time"`
		EndTime       *time.Time `json:"end_time"`
		Status        *string    `json:"status"`
		ServiceIDs    *[]string  `json:"service_ids"`
		TotalPrice    *float64   `json:"total_price"`
		Notes         *string    `json:"notes"`
		InternalNotes *string    `json:"internal_notes"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	updates := map[string]interface{}{}

	customerID := booking.CustomerID
	if request.CustomerID != nil && strings.TrimSpace(*request.CustomerID) != "" {
		customerID = strings.TrimSpace(*request.CustomerID)
		updates["customer_id"] = customerID
	}
	vehicleID := booking.VehicleID
	if request.VehicleID != nil {
		vehicleID = bookingNilIfEmpty(request.VehicleID)
		updates["vehicle_id"] = vehicleID
	} else if customerID != booking.CustomerID {
		vehicleID = nil // vehicle belonged to the previous customer
		updates["vehicle_id"] = nil
	}
	staffID := booking.StaffID
	if request.StaffID != nil {
		staffID = bookingNilIfEmpty(request.StaffID)
		updates["staff_id"] = staffID
	}

	if code, msg := h.bookingValidateRefs(orgID, customerID, vehicleID, staffID); code != 0 {
		c.JSON(code, gin.H{"error": msg})
		return
	}

	if request.Status != nil {
		s := strings.TrimSpace(*request.Status)
		if !validBookingStatuses[s] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid status"})
			return
		}
		updates["status"] = s
	}
	if request.Notes != nil {
		updates["notes"] = strings.TrimSpace(*request.Notes)
	}
	if request.InternalNotes != nil {
		updates["internal_notes"] = strings.TrimSpace(*request.InternalNotes)
	}

	var services []models.Service
	if request.ServiceIDs != nil {
		var err error
		services, err = h.bookingLoadServices(h.DB, orgID, *request.ServiceIDs)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Some selected services are invalid"})
			return
		}
		if request.TotalPrice == nil {
			price, _ := bookingServiceTotals(services)
			updates["total_price"] = price
		}
	}
	if request.TotalPrice != nil {
		if *request.TotalPrice < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Price cannot be negative"})
			return
		}
		updates["total_price"] = *request.TotalPrice
	}

	start, end := booking.StartTime, booking.EndTime
	if request.StartTime != nil && !request.StartTime.IsZero() {
		// Moving the start keeps the duration unless an end is supplied too.
		dur := end.Sub(start)
		start = *request.StartTime
		end = start.Add(dur)
	}
	if request.EndTime != nil && !request.EndTime.IsZero() {
		end = *request.EndTime
	} else if request.ServiceIDs != nil && len(services) > 0 && request.StartTime != nil {
		_, minutes := bookingServiceTotals(services)
		end = start.Add(time.Duration(minutes) * time.Minute)
	}
	if !end.After(start) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "End time must be after the start time"})
		return
	}
	if !start.Equal(booking.StartTime) {
		updates["start_time"] = start
	}
	if !end.Equal(booking.EndTime) {
		updates["end_time"] = end
	}

	prevStatus := booking.Status
	finalStatus := booking.Status
	if s, ok := updates["status"].(string); ok {
		finalStatus = s
	}
	if finalStatus != "cancelled" && finalStatus != "no_show" && finalStatus != "completed" &&
		h.bookingHasConflict(orgID, start, end, staffID, vehicleID, booking.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "This time overlaps another booking for the same staff member or vehicle"})
		return
	}

	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if len(updates) > 0 {
			if err := tx.Model(&booking).Updates(updates).Error; err != nil {
				return err
			}
		}
		if request.ServiceIDs != nil {
			assoc := tx.Model(&booking).Association("Services")
			if len(services) == 0 {
				return assoc.Clear()
			}
			if err := assoc.Replace(services); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update booking"})
		return
	}

	var fresh models.Booking
	h.bookingPreload(h.DB.Where("id = ?", booking.ID)).First(&fresh)
	h.notifyBooking(c, &fresh, "status", prevStatus)
	c.JSON(http.StatusOK, gin.H{"booking": fresh})
}

// DeleteBooking soft deletes a booking
func (h *Handler) DeleteBooking(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var booking models.Booking
	if err := h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), orgID).First(&booking).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
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

// GetAvailableTimeSlots returns available booking start times (HH:MM) for a
// given date, based on the organisation's business hours.
func (h *Handler) GetAvailableTimeSlots(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	date := c.Query("date")
	staffID := c.Query("staff_id")
	duration, _ := strconv.Atoi(c.DefaultQuery("duration", "60"))
	if duration <= 0 {
		duration = 60
	}

	if date == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Date parameter is required"})
		return
	}
	parsedDate, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid date format. Use YYYY-MM-DD"})
		return
	}

	var org models.Organization
	if err := h.DB.Where("id = ?", orgID).First(&org).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch organization"})
		return
	}

	var openTime, closeTime string
	switch parsedDate.Weekday() {
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
	startClock, err1 := time.Parse("15:04", openTime)
	endClock, err2 := time.Parse("15:04", closeTime)
	if err1 != nil || err2 != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid business hours format"})
		return
	}

	dayStart := time.Date(parsedDate.Year(), parsedDate.Month(), parsedDate.Day(), startClock.Hour(), startClock.Minute(), 0, 0, time.Local)
	dayEnd := time.Date(parsedDate.Year(), parsedDate.Month(), parsedDate.Day(), endClock.Hour(), endClock.Minute(), 0, 0, time.Local)

	var bookings []models.Booking
	q := h.DB.Where("organization_id = ? AND start_time < ? AND end_time > ? AND status NOT IN ?",
		orgID, dayEnd, dayStart, []string{"cancelled", "no_show"})
	if staffID != "" {
		q = q.Where("staff_id = ?", staffID)
	}
	q.Find(&bookings)

	slot := time.Duration(duration) * time.Minute
	buffer := time.Duration(org.BookingSettings.BufferTime) * time.Minute
	available := []string{}
	for cur := dayStart; !cur.Add(slot).After(dayEnd); cur = cur.Add(slot + buffer) {
		slotEnd := cur.Add(slot)
		conflict := false
		for _, b := range bookings {
			if cur.Before(b.EndTime) && slotEnd.After(b.StartTime) {
				conflict = true
				break
			}
		}
		if !conflict {
			available = append(available, cur.Format("15:04"))
		}
	}

	c.JSON(http.StatusOK, gin.H{"available_slots": available})
}

// UpdateBookingStatus updates just the status of a booking
func (h *Handler) UpdateBookingStatus(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var request struct {
		Status string `json:"status" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Status is required"})
		return
	}
	if !validBookingStatuses[request.Status] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid status"})
		return
	}

	var booking models.Booking
	if err := h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), orgID).First(&booking).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch booking"})
		return
	}

	prevStatus := booking.Status
	if err := h.DB.Model(&booking).Update("status", request.Status).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update booking status"})
		return
	}

	var fresh models.Booking
	h.bookingPreload(h.DB.Where("id = ?", booking.ID)).First(&fresh)
	h.notifyBooking(c, &fresh, "status", prevStatus)
	c.JSON(http.StatusOK, gin.H{"booking": fresh})
}
