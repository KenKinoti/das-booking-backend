package events

import (
	"net/http"
	"strconv"
	"time"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

type Handler struct {
	db *gorm.DB
}

func NewHandler(db *gorm.DB) *Handler {
	return &Handler{db: db}
}

// Event CRUD operations

// GetEvents returns a list of events with pagination and filtering
func (h *Handler) GetEvents(c *gin.Context) {
	var events []models.Event
	var total int64

	// Pagination parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset := (page - 1) * limit

	// Filter parameters
	category := c.Query("category")
	eventType := c.Query("type")
	status := c.Query("status")
	organizationID := c.Query("organization_id")
	search := c.Query("search")
	upcoming := c.Query("upcoming") == "true"

	query := h.db.Model(&models.Event{})

	// Apply filters
	if category != "" {
		query = query.Where("category = ?", category)
	}
	if eventType != "" {
		query = query.Where("type = ?", eventType)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if organizationID != "" {
		query = query.Where("organization_id = ?", organizationID)
	}
	if search != "" {
		query = query.Where("title ILIKE ? OR description ILIKE ?", "%"+search+"%", "%"+search+"%")
	}
	if upcoming {
		query = query.Where("start_date > ?", time.Now())
	}

	// Get total count
	query.Count(&total)

	// Get events with preloaded relationships
	err := query.Preload("Organization").
		Preload("Creator").
		Preload("TicketTypes").
		Preload("Registrations").
		Order("start_date ASC").
		Offset(offset).
		Limit(limit).
		Find(&events).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Calculate pagination info
	totalPages := (int(total) + limit - 1) / limit

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"events": events,
			"pagination": gin.H{
				"page":        page,
				"limit":       limit,
				"total":       total,
				"total_pages": totalPages,
			},
		},
	})
}

// GetEvent returns a single event by ID
func (h *Handler) GetEvent(c *gin.Context) {
	id := c.Param("id")
	var event models.Event

	err := h.db.Preload("Organization").
		Preload("Creator").
		Preload("TicketTypes").
		Preload("Registrations").
		Preload("Sessions").
		Preload("GalleryImages").
		Preload("Reviews").
		First(&event, "id = ?", id).Error

	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": event})
}

// CreateEvent creates a new event
func (h *Handler) CreateEvent(c *gin.Context) {
	var event models.Event

	if err := c.ShouldBindJSON(&event); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Set creation metadata
	event.CreatedAt = time.Now()
	event.UpdatedAt = time.Now()

	if err := h.db.Create(&event).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": event})
}

// UpdateEvent updates an existing event
func (h *Handler) UpdateEvent(c *gin.Context) {
	id := c.Param("id")
	var event models.Event

	// Check if event exists
	if err := h.db.First(&event, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	// Bind updated data
	if err := c.ShouldBindJSON(&event); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	event.UpdatedAt = time.Now()

	if err := h.db.Save(&event).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": event})
}

// DeleteEvent deletes an event
func (h *Handler) DeleteEvent(c *gin.Context) {
	id := c.Param("id")
	var event models.Event

	if err := h.db.First(&event, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	if err := h.db.Delete(&event).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Event deleted successfully"})
}

// Ticket Type operations

// CreateTicketType creates a new ticket type for an event
func (h *Handler) CreateTicketType(c *gin.Context) {
	eventID := c.Param("event_id")
	var ticketType models.TicketType

	if err := c.ShouldBindJSON(&ticketType); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ticketType.EventID = eventID
	ticketType.CreatedAt = time.Now()
	ticketType.UpdatedAt = time.Now()

	if err := h.db.Create(&ticketType).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": ticketType})
}

// GetTicketTypes returns ticket types for an event
func (h *Handler) GetTicketTypes(c *gin.Context) {
	eventID := c.Param("event_id")
	var ticketTypes []models.TicketType

	err := h.db.Where("event_id = ?", eventID).
		Order("sort_order ASC").
		Find(&ticketTypes).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": ticketTypes})
}

// Registration operations

// RegisterForEvent creates a new event registration
func (h *Handler) RegisterForEvent(c *gin.Context) {
	eventID := c.Param("event_id")
	var registration models.EventRegistration

	if err := c.ShouldBindJSON(&registration); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify event exists and is available for registration
	var event models.Event
	if err := h.db.Preload("TicketTypes").First(&event, "id = ?", eventID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}

	// Verify ticket type exists and has availability
	var ticketType models.TicketType
	if err := h.db.First(&ticketType, "id = ? AND event_id = ?", registration.TicketTypeID, eventID).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ticket type"})
		return
	}

	// Check availability
	if ticketType.AvailableQuantity() < registration.Quantity {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Not enough tickets available"})
		return
	}

	registration.EventID = eventID
	registration.RegistrationDate = time.Now()
	registration.CreatedAt = time.Now()
	registration.UpdatedAt = time.Now()

	// Start transaction
	tx := h.db.Begin()

	// Create registration
	if err := tx.Create(&registration).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Update sold quantity
	if err := tx.Model(&ticketType).Update("sold_quantity", ticketType.SoldQuantity+registration.Quantity).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update ticket availability"})
		return
	}

	tx.Commit()

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": registration})
}

// GetRegistrations returns registrations for an event
func (h *Handler) GetRegistrations(c *gin.Context) {
	eventID := c.Param("event_id")
	var registrations []models.EventRegistration

	err := h.db.Where("event_id = ?", eventID).
		Preload("TicketType").
		Preload("User").
		Preload("Customer").
		Order("registration_date DESC").
		Find(&registrations).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": registrations})
}

// CheckInRegistration checks in a registration using QR code
func (h *Handler) CheckInRegistration(c *gin.Context) {
	var request struct {
		QRCode      string `json:"qr_code" binding:"required"`
		CheckedInBy string `json:"checked_in_by" binding:"required"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var registration models.EventRegistration
	if err := h.db.Where("qr_code = ?", request.QRCode).First(&registration).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registration not found"})
		return
	}

	if !registration.CanCheckIn() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Registration cannot be checked in"})
		return
	}

	now := time.Now()
	registration.CheckedInAt = &now
	registration.CheckedInBy = request.CheckedInBy
	registration.Status = "attended"

	if err := h.db.Save(&registration).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": registration})
}

// Event Categories

// GetEventCategories returns all event categories
func (h *Handler) GetEventCategories(c *gin.Context) {
	var categories []models.EventCategory

	err := h.db.Where("is_active = ?", true).
		Order("sort_order ASC, name ASC").
		Find(&categories).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": categories})
}

// Event Analytics

// GetEventAnalytics returns analytics data for an event
func (h *Handler) GetEventAnalytics(c *gin.Context) {
	eventID := c.Param("event_id")

	var totalRegistrations int64
	var totalRevenue float64
	var confirmedAttendees int64
	var checkedInCount int64

	// Total registrations
	h.db.Model(&models.EventRegistration{}).Where("event_id = ?", eventID).Count(&totalRegistrations)

	// Total revenue
	h.db.Model(&models.EventRegistration{}).
		Where("event_id = ? AND payment_status = ?", eventID, "paid").
		Select("COALESCE(SUM(total_amount), 0)").Scan(&totalRevenue)

	// Confirmed attendees
	h.db.Model(&models.EventRegistration{}).
		Where("event_id = ? AND status = ?", eventID, "confirmed").Count(&confirmedAttendees)

	// Checked in count
	h.db.Model(&models.EventRegistration{}).
		Where("event_id = ? AND checked_in_at IS NOT NULL", eventID).Count(&checkedInCount)

	analytics := gin.H{
		"total_registrations":  totalRegistrations,
		"total_revenue":        totalRevenue,
		"confirmed_attendees":  confirmedAttendees,
		"checked_in_count":     checkedInCount,
		"check_in_rate":        float64(checkedInCount) / float64(confirmedAttendees) * 100,
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": analytics})
}

// Search Events
func (h *Handler) SearchEvents(c *gin.Context) {
	query := c.Query("q")
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Query parameter 'q' is required"})
		return
	}

	var events []models.Event
	err := h.db.Where("title ILIKE ? OR description ILIKE ? OR tags ILIKE ?",
		"%"+query+"%", "%"+query+"%", "%"+query+"%").
		Where("status = ? AND is_public = ?", "published", true).
		Preload("Organization").
		Preload("TicketTypes").
		Order("start_date ASC").
		Limit(50).
		Find(&events).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": events})
}