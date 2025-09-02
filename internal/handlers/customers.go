package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

// GetCustomers retrieves all customers for the organization
func (h *Handler) GetCustomers(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var customers []models.Customer
	query := h.DB.Where("organization_id = ?", orgID).Preload("Vehicles")

	// Apply search filter if provided
	search := c.Query("search")
	if search != "" {
		query = query.Where("first_name ILIKE ? OR last_name ILIKE ? OR email ILIKE ? OR phone ILIKE ?", 
			"%"+search+"%", "%"+search+"%", "%"+search+"%", "%"+search+"%")
	}

	// Apply status filter if provided
	status := c.Query("status")
	if status != "" {
		isActive := status == "active"
		query = query.Where("is_active = ?", isActive)
	}

	if err := query.Order("created_at DESC").Find(&customers).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch customers"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"customers": customers})
}

// GetCustomer retrieves a specific customer by ID
func (h *Handler) GetCustomer(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).
		Preload("Vehicles").Preload("Bookings").
		First(&customer).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Customer not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch customer"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"customer": customer})
}

// CreateCustomer creates a new customer
func (h *Handler) CreateCustomer(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var customer models.Customer
	if err := c.ShouldBindJSON(&customer); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	customer.OrganizationID = orgID
	customer.IsActive = true

	// Check if customer with same email already exists
	if customer.Email != "" {
		var existingCustomer models.Customer
		if err := h.DB.Where("email = ? AND organization_id = ?", customer.Email, orgID).First(&existingCustomer).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Customer with this email already exists"})
			return
		}
	}

	if err := h.DB.Create(&customer).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create customer"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"customer": customer})
}

// UpdateCustomer updates an existing customer
func (h *Handler) UpdateCustomer(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&customer).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Customer not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch customer"})
		return
	}

	var updateData models.Customer
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check if email is being changed and if it conflicts with another customer
	if updateData.Email != "" && updateData.Email != customer.Email {
		var existingCustomer models.Customer
		if err := h.DB.Where("email = ? AND organization_id = ? AND id != ?", updateData.Email, orgID, id).First(&existingCustomer).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Customer with this email already exists"})
			return
		}
	}

	// Update only provided fields
	if err := h.DB.Model(&customer).Updates(updateData).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update customer"})
		return
	}

	// Reload with relationships
	h.DB.Where("id = ?", customer.ID).Preload("Vehicles").First(&customer)

	c.JSON(http.StatusOK, gin.H{"customer": customer})
}

// DeleteCustomer soft deletes a customer
func (h *Handler) DeleteCustomer(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&customer).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Customer not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch customer"})
		return
	}

	// Check if customer has active bookings
	var activeBookingsCount int64
	h.DB.Model(&models.Booking{}).Where("customer_id = ? AND status NOT IN ?", 
		id, []string{"completed", "cancelled", "no_show"}).Count(&activeBookingsCount)
	
	if activeBookingsCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Cannot delete customer with active bookings"})
		return
	}

	if err := h.DB.Delete(&customer).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete customer"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Customer deleted successfully"})
}

// ToggleCustomerStatus toggles the active status of a customer
func (h *Handler) ToggleCustomerStatus(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&customer).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Customer not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch customer"})
		return
	}

	newStatus := !customer.IsActive
	if err := h.DB.Model(&customer).Update("is_active", newStatus).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update customer status"})
		return
	}

	customer.IsActive = newStatus
	c.JSON(http.StatusOK, gin.H{"customer": customer})
}

// GetCustomerStats returns statistics about customers
func (h *Handler) GetCustomerStats(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var stats struct {
		TotalCustomers   int64 `json:"total_customers"`
		ActiveCustomers  int64 `json:"active_customers"`
		InactiveCustomers int64 `json:"inactive_customers"`
		NewThisMonth     int64 `json:"new_this_month"`
	}

	// Total customers
	h.DB.Model(&models.Customer{}).Where("organization_id = ?", orgID).Count(&stats.TotalCustomers)

	// Active customers
	h.DB.Model(&models.Customer{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&stats.ActiveCustomers)

	// Inactive customers
	h.DB.Model(&models.Customer{}).Where("organization_id = ? AND is_active = ?", orgID, false).Count(&stats.InactiveCustomers)

	// New this month
	h.DB.Model(&models.Customer{}).Where("organization_id = ? AND DATE_TRUNC('month', created_at) = DATE_TRUNC('month', CURRENT_DATE)", orgID).Count(&stats.NewThisMonth)

	c.JSON(http.StatusOK, gin.H{"stats": stats})
}