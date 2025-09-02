package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

// GetServices retrieves all services for the organization
func (h *Handler) GetServices(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var services []models.Service
	query := h.DB.Where("organization_id = ?", orgID)

	// Apply search filter if provided
	search := c.Query("search")
	if search != "" {
		query = query.Where("name ILIKE ? OR description ILIKE ? OR category ILIKE ?", 
			"%"+search+"%", "%"+search+"%", "%"+search+"%")
	}

	// Apply category filter if provided
	category := c.Query("category")
	if category != "" {
		query = query.Where("category = ?", category)
	}

	// Apply status filter if provided
	status := c.Query("status")
	if status != "" {
		isActive := status == "active"
		query = query.Where("is_active = ?", isActive)
	}

	// Apply vehicle requirement filter if provided
	requiresVehicle := c.Query("requires_vehicle")
	if requiresVehicle != "" {
		requires := requiresVehicle == "true"
		query = query.Where("requires_vehicle = ?", requires)
	}

	if err := query.Order("category ASC, name ASC").Find(&services).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch services"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"services": services})
}

// GetService retrieves a specific service by ID
func (h *Handler) GetService(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var service models.Service
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&service).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Service not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch service"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"service": service})
}

// CreateService creates a new service
func (h *Handler) CreateService(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var service models.Service
	if err := c.ShouldBindJSON(&service); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	service.OrganizationID = orgID
	service.IsActive = true

	// Validate duration is positive
	if service.Duration <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Duration must be greater than 0"})
		return
	}

	// Validate price is positive
	if service.Price < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Price must be greater than or equal to 0"})
		return
	}

	// Check if service with same name already exists in the category
	var existingService models.Service
	if err := h.DB.Where("name = ? AND category = ? AND organization_id = ?", 
		service.Name, service.Category, orgID).First(&existingService).Error; err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Service with this name already exists in this category"})
		return
	}

	if err := h.DB.Create(&service).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create service"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"service": service})
}

// UpdateService updates an existing service
func (h *Handler) UpdateService(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var service models.Service
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&service).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Service not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch service"})
		return
	}

	var updateData models.Service
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate duration if being updated
	if updateData.Duration > 0 && updateData.Duration <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Duration must be greater than 0"})
		return
	}

	// Validate price if being updated
	if updateData.Price >= 0 && updateData.Price < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Price must be greater than or equal to 0"})
		return
	}

	// Check if name/category combination is being changed and if it conflicts
	if (updateData.Name != "" && updateData.Name != service.Name) || 
	   (updateData.Category != "" && updateData.Category != service.Category) {
		
		checkName := service.Name
		checkCategory := service.Category
		
		if updateData.Name != "" {
			checkName = updateData.Name
		}
		if updateData.Category != "" {
			checkCategory = updateData.Category
		}

		var existingService models.Service
		if err := h.DB.Where("name = ? AND category = ? AND organization_id = ? AND id != ?", 
			checkName, checkCategory, orgID, id).First(&existingService).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Service with this name already exists in this category"})
			return
		}
	}

	// Update only provided fields
	if err := h.DB.Model(&service).Updates(updateData).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update service"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"service": service})
}

// DeleteService soft deletes a service
func (h *Handler) DeleteService(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var service models.Service
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&service).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Service not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch service"})
		return
	}

	// Check if service is being used in any active bookings
	var activeBookingsCount int64
	h.DB.Table("booking_services").
		Joins("JOIN bookings ON booking_services.booking_id = bookings.id").
		Where("booking_services.service_id = ? AND bookings.status NOT IN ?", 
			id, []string{"completed", "cancelled", "no_show"}).
		Count(&activeBookingsCount)
	
	if activeBookingsCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Cannot delete service that is used in active bookings"})
		return
	}

	if err := h.DB.Delete(&service).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete service"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Service deleted successfully"})
}

// ToggleServiceStatus toggles the active status of a service
func (h *Handler) ToggleServiceStatus(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var service models.Service
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&service).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Service not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch service"})
		return
	}

	newStatus := !service.IsActive
	if err := h.DB.Model(&service).Update("is_active", newStatus).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update service status"})
		return
	}

	service.IsActive = newStatus
	c.JSON(http.StatusOK, gin.H{"service": service})
}

// GetServiceCategories returns all distinct service categories
func (h *Handler) GetServiceCategories(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var categories []string
	if err := h.DB.Model(&models.Service{}).
		Where("organization_id = ? AND is_active = ?", orgID, true).
		Distinct("category").
		Order("category ASC").
		Pluck("category", &categories).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch service categories"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"categories": categories})
}

// GetServiceStats returns statistics about services
func (h *Handler) GetServiceStats(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var stats struct {
		TotalServices     int64   `json:"total_services"`
		ActiveServices    int64   `json:"active_services"`
		InactiveServices  int64   `json:"inactive_services"`
		VehicleServices   int64   `json:"vehicle_services"`
		Categories        int64   `json:"total_categories"`
		AveragePrice      float64 `json:"average_price"`
		AverageDuration   float64 `json:"average_duration"`
	}

	// Total services
	h.DB.Model(&models.Service{}).Where("organization_id = ?", orgID).Count(&stats.TotalServices)

	// Active services
	h.DB.Model(&models.Service{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&stats.ActiveServices)

	// Inactive services
	h.DB.Model(&models.Service{}).Where("organization_id = ? AND is_active = ?", orgID, false).Count(&stats.InactiveServices)

	// Vehicle services
	h.DB.Model(&models.Service{}).Where("organization_id = ? AND requires_vehicle = ?", orgID, true).Count(&stats.VehicleServices)

	// Categories
	h.DB.Model(&models.Service{}).Where("organization_id = ?", orgID).Distinct("category").Count(&stats.Categories)

	// Average price and duration
	var avgData struct {
		AvgPrice    *float64 `json:"avg_price"`
		AvgDuration *float64 `json:"avg_duration"`
	}

	h.DB.Model(&models.Service{}).
		Where("organization_id = ? AND is_active = ?", orgID, true).
		Select("AVG(price) as avg_price, AVG(duration) as avg_duration").
		Scan(&avgData)

	if avgData.AvgPrice != nil {
		stats.AveragePrice = *avgData.AvgPrice
	}
	if avgData.AvgDuration != nil {
		stats.AverageDuration = *avgData.AvgDuration
	}

	c.JSON(http.StatusOK, gin.H{"stats": stats})
}

// DuplicateService creates a copy of an existing service
func (h *Handler) DuplicateService(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	id := c.Param("id")
	var originalService models.Service
	if err := h.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&originalService).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Service not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch service"})
		return
	}

	// Create a copy
	newService := models.Service{
		OrganizationID:  originalService.OrganizationID,
		Name:           originalService.Name + " (Copy)",
		Description:    originalService.Description,
		Category:       originalService.Category,
		Duration:       originalService.Duration,
		Price:          originalService.Price,
		IsActive:       originalService.IsActive,
		RequiresVehicle: originalService.RequiresVehicle,
	}

	// Ensure name doesn't conflict
	counter := 1
	baseName := newService.Name
	for {
		var existingService models.Service
		if err := h.DB.Where("name = ? AND category = ? AND organization_id = ?", 
			newService.Name, newService.Category, orgID).First(&existingService).Error; err != nil {
			break // Name is available
		}
		counter++
		newService.Name = baseName + " (" + string(rune('0'+counter)) + ")"
	}

	if err := h.DB.Create(&newService).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to duplicate service"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"service": newService})
}