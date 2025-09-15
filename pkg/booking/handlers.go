package booking

import (
	"net/http"
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

// Booking handlers using existing models from internal/models
func (h *Handler) GetBookings(c *gin.Context) {
	var bookings []models.Booking
	err := h.db.Preload("Customer").Preload("Service").Preload("Staff").Find(&bookings).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": bookings})
}

func (h *Handler) CreateBooking(c *gin.Context) {
	var booking models.Booking
	if err := c.ShouldBindJSON(&booking); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.db.Create(&booking).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": booking})
}

func (h *Handler) GetBooking(c *gin.Context) {
	id := c.Param("id")
	var booking models.Booking
	err := h.db.Preload("Customer").Preload("Service").Preload("Staff").First(&booking, "id = ?", id).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": booking})
}

func (h *Handler) UpdateBooking(c *gin.Context) {
	id := c.Param("id")
	var booking models.Booking
	if err := h.db.First(&booking, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
		return
	}

	if err := c.ShouldBindJSON(&booking); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.db.Save(&booking).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": booking})
}

func (h *Handler) DeleteBooking(c *gin.Context) {
	id := c.Param("id")
	if err := h.db.Delete(&models.Booking{}, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Booking deleted"})
}

func (h *Handler) UpdateBookingStatus(c *gin.Context) {
	id := c.Param("id")
	var request struct {
		Status string `json:"status"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var booking models.Booking
	if err := h.db.First(&booking, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Booking not found"})
		return
	}

	booking.Status = request.Status
	if err := h.db.Save(&booking).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": booking})
}

func (h *Handler) GetAvailableSlots(c *gin.Context) {
	// Simple available slots response
	slots := []string{"09:00", "10:00", "11:00", "14:00", "15:00", "16:00"}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": slots})
}

func (h *Handler) GetDashboard(c *gin.Context) {
	var totalBookings, todaysBookings, upcomingBookings, completedBookings int64
	var totalRevenue float64

	h.db.Model(&models.Booking{}).Count(&totalBookings)
	h.db.Model(&models.Booking{}).Where("DATE(booking_date) = ?", time.Now().Format("2006-01-02")).Count(&todaysBookings)
	h.db.Model(&models.Booking{}).Where("booking_date > ? AND status = ?", time.Now(), "confirmed").Count(&upcomingBookings)
	h.db.Model(&models.Booking{}).Where("status = ?", "completed").Count(&completedBookings)

	dashboard := gin.H{
		"total_bookings":     totalBookings,
		"todays_bookings":    todaysBookings,
		"upcoming_bookings":  upcomingBookings,
		"completed_bookings": completedBookings,
		"total_revenue":      totalRevenue,
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": dashboard})
}

// Customer handlers
func (h *Handler) GetCustomers(c *gin.Context) {
	var customers []models.Customer
	err := h.db.Find(&customers).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": customers})
}

func (h *Handler) CreateCustomer(c *gin.Context) {
	var customer models.Customer
	if err := c.ShouldBindJSON(&customer); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.db.Create(&customer).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": customer})
}

func (h *Handler) GetCustomer(c *gin.Context) {
	id := c.Param("id")
	var customer models.Customer
	err := h.db.Preload("Vehicles").First(&customer, "id = ?", id).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Customer not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": customer})
}

func (h *Handler) UpdateCustomer(c *gin.Context) {
	id := c.Param("id")
	var customer models.Customer
	if err := h.db.First(&customer, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Customer not found"})
		return
	}

	if err := c.ShouldBindJSON(&customer); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.db.Save(&customer).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": customer})
}

// Service handlers
func (h *Handler) GetServices(c *gin.Context) {
	var services []models.Service
	err := h.db.Find(&services).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": services})
}

func (h *Handler) CreateService(c *gin.Context) {
	var service models.Service
	if err := c.ShouldBindJSON(&service); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.db.Create(&service).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": service})
}

func (h *Handler) GetService(c *gin.Context) {
	id := c.Param("id")
	var service models.Service
	err := h.db.First(&service, "id = ?", id).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Service not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": service})
}

func (h *Handler) UpdateService(c *gin.Context) {
	id := c.Param("id")
	var service models.Service
	if err := h.db.First(&service, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Service not found"})
		return
	}

	if err := c.ShouldBindJSON(&service); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.db.Save(&service).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": service})
}

// Vehicle handlers
func (h *Handler) GetVehicles(c *gin.Context) {
	var vehicles []models.Vehicle
	err := h.db.Preload("Customer").Find(&vehicles).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": vehicles})
}

func (h *Handler) CreateVehicle(c *gin.Context) {
	var vehicle models.Vehicle
	if err := c.ShouldBindJSON(&vehicle); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.db.Create(&vehicle).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": vehicle})
}

func (h *Handler) GetCustomerVehicles(c *gin.Context) {
	customerID := c.Param("customer_id")
	var vehicles []models.Vehicle
	err := h.db.Where("customer_id = ?", customerID).Find(&vehicles).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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