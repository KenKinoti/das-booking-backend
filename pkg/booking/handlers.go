// Package booking serves the legacy /api/v1/booking/* endpoints. The app uses
// the core /customers, /services, /bookings and /vehicles routes; these are
// kept for older API clients and are strictly scoped to the caller's
// organisation (they used to return every organisation's records).
package booking

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

type Handler struct {
	db *gorm.DB
}

func NewHandler(db *gorm.DB) *Handler {
	return &Handler{db: db}
}

// orgOf returns the caller's organisation or aborts with 401.
func orgOf(c *gin.Context) (string, bool) {
	for _, k := range []string{"org_id", "organization_id"} {
		if v, ok := c.Get(k); ok && v != nil {
			if s := fmt.Sprint(v); s != "" {
				return s, true
			}
		}
	}
	c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "Organization not found in token"})
	return "", false
}

func notFound(c *gin.Context, what string) {
	c.JSON(http.StatusNotFound, gin.H{"success": false, "error": what + " not found"})
}

func serverError(c *gin.Context, err error) {
	c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
}

// ---- bookings ----

func (h *Handler) GetBookings(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	bookings := []models.Booking{}
	if err := h.db.Preload("Customer").Preload("Staff").Preload("Services").
		Where("organization_id = ?", org).Order("start_time DESC").Find(&bookings).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": bookings})
}

func (h *Handler) CreateBooking(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var booking models.Booking
	if err := c.ShouldBindJSON(&booking); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	var n int64
	h.db.Model(&models.Customer{}).Where("id = ? AND organization_id = ?", booking.CustomerID, org).Count(&n)
	if n == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Customer not found"})
		return
	}
	if booking.ID == "" {
		booking.ID = uuid.NewString()
	}
	booking.OrganizationID = org
	if err := h.db.Omit("Organization", "Customer", "Staff", "Vehicle", "Services").Create(&booking).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": booking})
}

func (h *Handler) GetBooking(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var booking models.Booking
	if err := h.db.Preload("Customer").Preload("Staff").Preload("Services").
		First(&booking, "id = ? AND organization_id = ?", c.Param("id"), org).Error; err != nil {
		notFound(c, "Booking")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": booking})
}

func (h *Handler) UpdateBooking(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var booking models.Booking
	if err := h.db.First(&booking, "id = ? AND organization_id = ?", c.Param("id"), org).Error; err != nil {
		notFound(c, "Booking")
		return
	}
	id := booking.ID
	if err := c.ShouldBindJSON(&booking); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	booking.ID, booking.OrganizationID = id, org
	if err := h.db.Omit("Organization", "Customer", "Staff", "Vehicle", "Services").Save(&booking).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": booking})
}

func (h *Handler) DeleteBooking(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	res := h.db.Where("id = ? AND organization_id = ?", c.Param("id"), org).Delete(&models.Booking{})
	if res.Error != nil {
		serverError(c, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		notFound(c, "Booking")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Booking deleted"})
}

func (h *Handler) UpdateBookingStatus(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var request struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.Status == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "status is required"})
		return
	}
	res := h.db.Model(&models.Booking{}).Where("id = ? AND organization_id = ?", c.Param("id"), org).Update("status", request.Status)
	if res.Error != nil {
		serverError(c, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		notFound(c, "Booking")
		return
	}
	var booking models.Booking
	h.db.First(&booking, "id = ?", c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"success": true, "data": booking})
}

// GetAvailableSlots used to return a fixed list of times. Real availability is
// served by GET /api/v1/bookings/available-slots.
func (h *Handler) GetAvailableSlots(c *gin.Context) {
	c.JSON(http.StatusGone, gin.H{"success": false, "error": "Use GET /api/v1/bookings/available-slots"})
}

// GetDashboard returns booking counts for the caller's organisation.
func (h *Handler) GetDashboard(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var total, today, upcoming, completed int64
	var revenue float64
	base := func() *gorm.DB { return h.db.Model(&models.Booking{}).Where("organization_id = ?", org) }
	base().Count(&total)
	base().Where("start_time >= ? AND start_time < ? AND status NOT IN ?", dayStart, dayStart.AddDate(0, 0, 1), []string{"cancelled", "no_show"}).Count(&today)
	base().Where("start_time > ? AND status IN ?", now, []string{"scheduled", "confirmed", "in_progress"}).Count(&upcoming)
	base().Where("status = ?", "completed").Count(&completed)
	base().Where("status = ?", "completed").Select("COALESCE(SUM(total_price),0)").Row().Scan(&revenue)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"total_bookings":     total,
		"todays_bookings":    today,
		"upcoming_bookings":  upcoming,
		"completed_bookings": completed,
		"total_revenue":      revenue, // completed bookings' value
	}})
}

// ---- customers ----

func (h *Handler) GetCustomers(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	customers := []models.Customer{}
	if err := h.db.Where("organization_id = ?", org).Order("created_at DESC").Find(&customers).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": customers})
}

func (h *Handler) CreateCustomer(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var customer models.Customer
	if err := c.ShouldBindJSON(&customer); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	if customer.ID == "" {
		customer.ID = uuid.NewString()
	}
	customer.OrganizationID = org
	customer.IsActive = true
	if err := h.db.Omit("Organization", "Vehicles", "Bookings").Create(&customer).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": customer})
}

func (h *Handler) GetCustomer(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var customer models.Customer
	if err := h.db.Preload("Vehicles").First(&customer, "id = ? AND organization_id = ?", c.Param("id"), org).Error; err != nil {
		notFound(c, "Customer")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": customer})
}

func (h *Handler) UpdateCustomer(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var customer models.Customer
	if err := h.db.First(&customer, "id = ? AND organization_id = ?", c.Param("id"), org).Error; err != nil {
		notFound(c, "Customer")
		return
	}
	id := customer.ID
	if err := c.ShouldBindJSON(&customer); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	customer.ID, customer.OrganizationID = id, org
	if err := h.db.Omit("Organization", "Vehicles", "Bookings").Save(&customer).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": customer})
}

// ---- services ----

func (h *Handler) GetServices(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	services := []models.Service{}
	if err := h.db.Where("organization_id = ?", org).Order("name ASC").Find(&services).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": services})
}

func (h *Handler) CreateService(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var service models.Service
	if err := c.ShouldBindJSON(&service); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	if service.ID == "" {
		service.ID = uuid.NewString()
	}
	service.OrganizationID = org
	if err := h.db.Omit("Organization").Create(&service).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": service})
}

func (h *Handler) GetService(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var service models.Service
	if err := h.db.First(&service, "id = ? AND organization_id = ?", c.Param("id"), org).Error; err != nil {
		notFound(c, "Service")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": service})
}

func (h *Handler) UpdateService(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var service models.Service
	if err := h.db.First(&service, "id = ? AND organization_id = ?", c.Param("id"), org).Error; err != nil {
		notFound(c, "Service")
		return
	}
	id := service.ID
	if err := c.ShouldBindJSON(&service); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	service.ID, service.OrganizationID = id, org
	if err := h.db.Omit("Organization").Save(&service).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": service})
}

// ---- vehicles ----

func (h *Handler) GetVehicles(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	vehicles := []models.Vehicle{}
	if err := h.db.Preload("Customer").Where("organization_id = ?", org).Find(&vehicles).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": vehicles})
}

func (h *Handler) CreateVehicle(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	var vehicle models.Vehicle
	if err := c.ShouldBindJSON(&vehicle); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	var n int64
	h.db.Model(&models.Customer{}).Where("id = ? AND organization_id = ?", vehicle.CustomerID, org).Count(&n)
	if n == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Customer not found"})
		return
	}
	if vehicle.ID == "" {
		vehicle.ID = uuid.NewString()
	}
	vehicle.OrganizationID = org
	if err := h.db.Omit("Organization", "Customer").Create(&vehicle).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": vehicle})
}

func (h *Handler) GetCustomerVehicles(c *gin.Context) {
	org, ok := orgOf(c)
	if !ok {
		return
	}
	vehicles := []models.Vehicle{}
	if err := h.db.Where("customer_id = ? AND organization_id = ?", c.Param("customer_id"), org).Find(&vehicles).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": vehicles})
}

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	// Booking routes
	r.GET("/bookings", h.GetBookings)
	r.POST("/bookings", h.CreateBooking)
	r.GET("/bookings/:id", h.GetBooking)
	r.PUT("/bookings/:id", h.UpdateBooking)
	r.DELETE("/bookings/:id", h.DeleteBooking)
	r.PUT("/bookings/:id/status", h.UpdateBookingStatus)
	r.GET("/available-slots", h.GetAvailableSlots)
	r.GET("/dashboard", h.GetDashboard)

	// Customer routes
	r.GET("/customers", h.GetCustomers)
	r.POST("/customers", h.CreateCustomer)
	r.GET("/customers/:id", h.GetCustomer)
	r.PUT("/customers/:id", h.UpdateCustomer)

	// Service routes
	r.GET("/services", h.GetServices)
	r.POST("/services", h.CreateService)
	r.GET("/services/:id", h.GetService)
	r.PUT("/services/:id", h.UpdateService)

	// Vehicle routes
	r.GET("/vehicles", h.GetVehicles)
	r.POST("/vehicles", h.CreateVehicle)
	r.GET("/customer/:customer_id/vehicles", h.GetCustomerVehicles)
}
