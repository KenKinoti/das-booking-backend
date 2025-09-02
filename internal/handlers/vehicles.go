package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

// GetVehicles retrieves all vehicles for the organization
func (h *Handler) GetVehicles(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var vehicles []models.Vehicle
	query := h.DB.Where("organization_id = ?", orgID).Preload("Customer")

	// Apply search filter if provided
	search := c.Query("search")
	if search != "" {
		query = query.Where("make ILIKE ? OR model ILIKE ? OR license_plate ILIKE ? OR vin ILIKE ?", 
			"%"+search+"%", "%"+search+"%", "%"+search+"%", "%"+search+"%")
	}

	// Apply customer filter if provided
	customerID := c.Query("customer_id")
	if customerID != "" {
		query = query.Where("customer_id = ?", customerID)
	}

	// Apply status filter if provided
	status := c.Query("status")
	if status != "" {
		isActive := status == "active"
		query = query.Where("is_active = ?", isActive)
	}

	if err := query.Order("created_at DESC").Find(&vehicles).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch vehicles"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"vehicles": vehicles})
}

// GetVehicle retrieves a specific vehicle by ID
func (h *Handler) GetVehicle(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var vehicle models.Vehicle
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).
		Preload("Customer").Preload("Bookings").
		First(&vehicle).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Vehicle not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch vehicle"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"vehicle": vehicle})
}

// GetCustomerVehicles retrieves all vehicles for a specific customer
func (h *Handler) GetCustomerVehicles(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	customerID := c.Param("customer_id")

	// Verify customer belongs to organization
	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", customerID, orgID).First(&customer).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Customer not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch customer"})
		return
	}

	var vehicles []models.Vehicle
	if err := h.DB.Where("customer_id = ? AND organization_id = ?", customerID, orgID).
		Order("created_at DESC").Find(&vehicles).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch vehicles"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"vehicles": vehicles})
}

// CreateVehicle creates a new vehicle
func (h *Handler) CreateVehicle(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var vehicle models.Vehicle
	if err := c.ShouldBindJSON(&vehicle); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	vehicle.OrganizationID = orgID
	vehicle.IsActive = true

	// Verify customer exists and belongs to organization
	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", vehicle.CustomerID, orgID).First(&customer).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid customer ID"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to validate customer"})
		return
	}

	// Check if VIN already exists (if provided)
	if vehicle.VIN != "" {
		var existingVehicle models.Vehicle
		if err := h.DB.Where("vin = ? AND organization_id = ?", vehicle.VIN, orgID).First(&existingVehicle).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Vehicle with this VIN already exists"})
			return
		}
	}

	// Check if license plate already exists (if provided)
	if vehicle.LicensePlate != "" {
		var existingVehicle models.Vehicle
		if err := h.DB.Where("license_plate = ? AND organization_id = ?", vehicle.LicensePlate, orgID).First(&existingVehicle).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Vehicle with this license plate already exists"})
			return
		}
	}

	if err := h.DB.Create(&vehicle).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create vehicle"})
		return
	}

	// Reload with customer info
	h.DB.Where("id = ?", vehicle.ID).Preload("Customer").First(&vehicle)

	c.JSON(http.StatusCreated, gin.H{"vehicle": vehicle})
}

// UpdateVehicle updates an existing vehicle
func (h *Handler) UpdateVehicle(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var vehicle models.Vehicle
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&vehicle).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Vehicle not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch vehicle"})
		return
	}

	var updateData models.Vehicle
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// If customer is being changed, verify the new customer
	if updateData.CustomerID != "" && updateData.CustomerID != vehicle.CustomerID {
		var customer models.Customer
		if err := h.DB.Where("id = ? AND organization_id = ?", updateData.CustomerID, orgID).First(&customer).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid customer ID"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to validate customer"})
			return
		}
	}

	// Check if VIN is being changed and if it conflicts with another vehicle
	if updateData.VIN != "" && updateData.VIN != vehicle.VIN {
		var existingVehicle models.Vehicle
		if err := h.DB.Where("vin = ? AND organization_id = ? AND id != ?", updateData.VIN, orgID, id).First(&existingVehicle).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Vehicle with this VIN already exists"})
			return
		}
	}

	// Check if license plate is being changed and if it conflicts with another vehicle
	if updateData.LicensePlate != "" && updateData.LicensePlate != vehicle.LicensePlate {
		var existingVehicle models.Vehicle
		if err := h.DB.Where("license_plate = ? AND organization_id = ? AND id != ?", updateData.LicensePlate, orgID, id).First(&existingVehicle).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Vehicle with this license plate already exists"})
			return
		}
	}

	// Update only provided fields
	if err := h.DB.Model(&vehicle).Updates(updateData).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update vehicle"})
		return
	}

	// Reload with customer info
	h.DB.Where("id = ?", vehicle.ID).Preload("Customer").First(&vehicle)

	c.JSON(http.StatusOK, gin.H{"vehicle": vehicle})
}

// DeleteVehicle soft deletes a vehicle
func (h *Handler) DeleteVehicle(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var vehicle models.Vehicle
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&vehicle).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Vehicle not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch vehicle"})
		return
	}

	// Check if vehicle has active bookings
	var activeBookingsCount int64
	h.DB.Model(&models.Booking{}).Where("vehicle_id = ? AND status NOT IN ?", 
		id, []string{"completed", "cancelled", "no_show"}).Count(&activeBookingsCount)
	
	if activeBookingsCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Cannot delete vehicle with active bookings"})
		return
	}

	if err := h.DB.Delete(&vehicle).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete vehicle"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Vehicle deleted successfully"})
}

// ToggleVehicleStatus toggles the active status of a vehicle
func (h *Handler) ToggleVehicleStatus(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var vehicle models.Vehicle
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&vehicle).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Vehicle not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch vehicle"})
		return
	}

	newStatus := !vehicle.IsActive
	if err := h.DB.Model(&vehicle).Update("is_active", newStatus).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update vehicle status"})
		return
	}

	vehicle.IsActive = newStatus
	c.JSON(http.StatusOK, gin.H{"vehicle": vehicle})
}

// UpdateVehicleMileage updates the mileage of a vehicle
func (h *Handler) UpdateVehicleMileage(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var request struct {
		Mileage int    `json:"mileage" binding:"required,min=0"`
		Notes   string `json:"notes"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var vehicle models.Vehicle
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&vehicle).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Vehicle not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch vehicle"})
		return
	}

	updates := map[string]interface{}{
		"mileage": request.Mileage,
	}

	if request.Notes != "" {
		updates["notes"] = request.Notes
	}

	if err := h.DB.Model(&vehicle).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update vehicle mileage"})
		return
	}

	// Reload with customer info
	h.DB.Where("id = ?", vehicle.ID).Preload("Customer").First(&vehicle)

	c.JSON(http.StatusOK, gin.H{"vehicle": vehicle})
}

// GetVehicleStats returns statistics about vehicles
func (h *Handler) GetVehicleStats(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var stats struct {
		TotalVehicles   int64 `json:"total_vehicles"`
		ActiveVehicles  int64 `json:"active_vehicles"`
		InactiveVehicles int64 `json:"inactive_vehicles"`
		NewThisMonth    int64 `json:"new_this_month"`
	}

	// Total vehicles
	h.DB.Model(&models.Vehicle{}).Where("organization_id = ?", orgID).Count(&stats.TotalVehicles)

	// Active vehicles
	h.DB.Model(&models.Vehicle{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&stats.ActiveVehicles)

	// Inactive vehicles
	h.DB.Model(&models.Vehicle{}).Where("organization_id = ? AND is_active = ?", orgID, false).Count(&stats.InactiveVehicles)

	// New this month
	h.DB.Model(&models.Vehicle{}).Where("organization_id = ? AND DATE_TRUNC('month', created_at) = DATE_TRUNC('month', CURRENT_DATE)", orgID).Count(&stats.NewThisMonth)

	c.JSON(http.StatusOK, gin.H{"stats": stats})
}