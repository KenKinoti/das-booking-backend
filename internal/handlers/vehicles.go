package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
		query = query.Where("(make ILIKE ? OR model ILIKE ? OR license_plate ILIKE ? OR vin ILIKE ?)",
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

	customerID := c.Param("id")

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

// vehicleInput is the writable part of a vehicle.
type vehicleInput struct {
	CustomerID   *string `json:"customer_id"`
	Make         *string `json:"make"`
	Model        *string `json:"model"`
	Year         *int    `json:"year"`
	LicensePlate *string `json:"license_plate"`
	VIN          *string `json:"vin"`
	Color        *string `json:"color"`
	Mileage      *int    `json:"mileage"`
	Notes        *string `json:"notes"`
	IsActive     *bool   `json:"is_active"`
}

func (in vehicleInput) validate(requireAll bool) string {
	if requireAll || in.Make != nil {
		if trimPtr(in.Make) == "" {
			return "Make is required"
		}
	}
	if requireAll || in.Model != nil {
		if trimPtr(in.Model) == "" {
			return "Model is required"
		}
	}
	if requireAll || in.Year != nil {
		if in.Year == nil || *in.Year < 1900 || *in.Year > time.Now().Year()+1 {
			return "Enter a valid year"
		}
	}
	if in.Mileage != nil && *in.Mileage < 0 {
		return "Mileage cannot be negative"
	}
	return ""
}

// vehicleConflict reports a duplicate plate or VIN within the organisation.
func (h *Handler) vehicleConflict(orgID, excludeID, plate, vin string) string {
	if plate != "" {
		var n int64
		h.DB.Model(&models.Vehicle{}).Where("organization_id = ? AND UPPER(license_plate) = ? AND id <> ?", orgID, strings.ToUpper(plate), excludeID).Count(&n)
		if n > 0 {
			return "A vehicle with this licence plate already exists"
		}
	}
	if vin != "" {
		var n int64
		// VIN is globally unique in the schema, so check across all organisations.
		h.DB.Unscoped().Model(&models.Vehicle{}).Where("UPPER(vin) = ? AND id <> ?", strings.ToUpper(vin), excludeID).Count(&n)
		if n > 0 {
			return "A vehicle with this VIN is already registered"
		}
	}
	return ""
}

// CreateVehicle creates a new vehicle for a customer.
func (h *Handler) CreateVehicle(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var in vehicleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid vehicle data: " + err.Error()})
		return
	}
	if msg := in.validate(true); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}

	var customer models.Customer
	if err := h.DB.Where("id = ? AND organization_id = ?", trimPtr(in.CustomerID), orgID).First(&customer).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Choose a valid customer"})
		return
	}

	vehicle := models.Vehicle{
		ID:             uuid.NewString(),
		CustomerID:     customer.ID,
		OrganizationID: orgID,
		Make:           trimPtr(in.Make),
		Model:          trimPtr(in.Model),
		Year:           *in.Year,
		LicensePlate:   strings.ToUpper(trimPtr(in.LicensePlate)),
		VIN:            strings.ToUpper(trimPtr(in.VIN)),
		Color:          trimPtr(in.Color),
		Notes:          trimPtr(in.Notes),
		IsActive:       true,
	}
	if in.Mileage != nil {
		vehicle.Mileage = *in.Mileage
	}
	if msg := h.vehicleConflict(orgID, "", vehicle.LicensePlate, vehicle.VIN); msg != "" {
		c.JSON(http.StatusConflict, gin.H{"error": msg})
		return
	}

	// An empty VIN is stored as NULL so the unique constraint allows many.
	omit := []string{"Customer", "Organization", "Bookings"}
	if vehicle.VIN == "" {
		omit = append(omit, "VIN")
	}
	if err := h.DB.Omit(omit...).Create(&vehicle).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create vehicle"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"vehicle": vehicle})
}

// UpdateVehicle updates the fields that were sent.
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

	var in vehicleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid vehicle data: " + err.Error()})
		return
	}
	if msg := in.validate(false); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}

	updates := map[string]interface{}{}
	if in.CustomerID != nil && trimPtr(in.CustomerID) != vehicle.CustomerID {
		var customer models.Customer
		if err := h.DB.Where("id = ? AND organization_id = ?", trimPtr(in.CustomerID), orgID).First(&customer).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Choose a valid customer"})
			return
		}
		updates["customer_id"] = customer.ID
	}
	plate, vin := "", ""
	if in.LicensePlate != nil {
		plate = strings.ToUpper(trimPtr(in.LicensePlate))
		updates["license_plate"] = plate
	}
	if in.VIN != nil {
		vin = strings.ToUpper(trimPtr(in.VIN))
		if vin == "" {
			updates["vin"] = nil
		} else {
			updates["vin"] = vin
		}
	}
	if msg := h.vehicleConflict(orgID, id, plate, vin); msg != "" {
		c.JSON(http.StatusConflict, gin.H{"error": msg})
		return
	}
	if in.Make != nil {
		updates["make"] = trimPtr(in.Make)
	}
	if in.Model != nil {
		updates["model"] = trimPtr(in.Model)
	}
	if in.Year != nil {
		updates["year"] = *in.Year
	}
	if in.Color != nil {
		updates["color"] = trimPtr(in.Color)
	}
	if in.Mileage != nil {
		updates["mileage"] = *in.Mileage
	}
	if in.Notes != nil {
		updates["notes"] = *in.Notes
	}
	if in.IsActive != nil {
		updates["is_active"] = *in.IsActive
	}
	if len(updates) > 0 {
		if err := h.DB.Model(&models.Vehicle{}).Where("id = ? AND organization_id = ?", id, orgID).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update vehicle"})
			return
		}
	}
	h.DB.Where("id = ?", id).First(&vehicle)
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
		TotalVehicles    int64 `json:"total_vehicles"`
		ActiveVehicles   int64 `json:"active_vehicles"`
		InactiveVehicles int64 `json:"inactive_vehicles"`
		NewThisMonth     int64 `json:"new_this_month"`
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
