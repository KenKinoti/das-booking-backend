package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
		query = query.Where("(name ILIKE ? OR description ILIKE ? OR category ILIKE ?)",
			"%"+search+"%", "%"+search+"%", "%"+search+"%")
	}

	// Apply category filter if provided
	category := c.Query("category")
	if category != "" {
		query = query.Where("LOWER(category) = LOWER(?)", category)
	}

	// category_match=hair,barber matches any category containing a keyword.
	if match := c.Query("category_match"); match != "" {
		var conds []string
		var args []interface{}
		for _, kw := range strings.Split(match, ",") {
			if kw = strings.TrimSpace(kw); kw != "" {
				conds = append(conds, "category ILIKE ?", "name ILIKE ?")
				args = append(args, "%"+kw+"%", "%"+kw+"%")
			}
		}
		if len(conds) > 0 {
			query = query.Where("("+strings.Join(conds, " OR ")+")", args...)
		}
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

// serviceInput is the writable part of a service.
type serviceInput struct {
	Name            *string  `json:"name"`
	Description     *string  `json:"description"`
	Category        *string  `json:"category"`
	Duration        *int     `json:"duration"`
	Price           *float64 `json:"price"`
	IsActive        *bool    `json:"is_active"`
	RequiresVehicle *bool    `json:"requires_vehicle"`
}

func (in serviceInput) validate(requireAll bool) string {
	if (requireAll || in.Name != nil) && trimPtr(in.Name) == "" {
		return "Service name is required"
	}
	if (requireAll || in.Category != nil) && trimPtr(in.Category) == "" {
		return "Category is required"
	}
	if requireAll || in.Duration != nil {
		if in.Duration == nil || *in.Duration <= 0 || *in.Duration > 24*60 {
			return "Duration must be between 1 and 1440 minutes"
		}
	}
	if requireAll || in.Price != nil {
		if in.Price == nil || *in.Price < 0 {
			return "Price must be zero or more"
		}
	}
	return ""
}

func (h *Handler) serviceNameTaken(orgID, name, category, excludeID string) bool {
	var n int64
	h.DB.Model(&models.Service{}).Where("organization_id = ? AND LOWER(name) = LOWER(?) AND LOWER(category) = LOWER(?) AND id <> ?", orgID, name, category, excludeID).Count(&n)
	return n > 0
}

// CreateService creates a new service
func (h *Handler) CreateService(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var in serviceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid service data: " + err.Error()})
		return
	}
	if msg := in.validate(true); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	service := models.Service{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		Name:           trimPtr(in.Name),
		Description:    trimPtr(in.Description),
		Category:       trimPtr(in.Category),
		Duration:       *in.Duration,
		Price:          *in.Price,
		IsActive:       true,
	}
	if in.RequiresVehicle != nil {
		service.RequiresVehicle = *in.RequiresVehicle
	}
	if h.serviceNameTaken(orgID, service.Name, service.Category, "") {
		c.JSON(http.StatusConflict, gin.H{"error": "A service with this name already exists in this category"})
		return
	}
	if err := h.DB.Omit("Organization", "Bookings").Create(&service).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create service"})
		return
	}
	if in.IsActive != nil && !*in.IsActive {
		h.DB.Model(&models.Service{}).Where("id = ?", service.ID).Update("is_active", false)
		service.IsActive = false
	}
	c.JSON(http.StatusCreated, gin.H{"service": service})
}

// UpdateService updates the fields that were sent.
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

	var in serviceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid service data: " + err.Error()})
		return
	}
	if msg := in.validate(false); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}

	name, category := service.Name, service.Category
	updates := map[string]interface{}{}
	if in.Name != nil {
		name = trimPtr(in.Name)
		updates["name"] = name
	}
	if in.Category != nil {
		category = trimPtr(in.Category)
		updates["category"] = category
	}
	if h.serviceNameTaken(orgID, name, category, id) {
		c.JSON(http.StatusConflict, gin.H{"error": "A service with this name already exists in this category"})
		return
	}
	if in.Description != nil {
		updates["description"] = *in.Description
	}
	if in.Duration != nil {
		updates["duration"] = *in.Duration
	}
	if in.Price != nil {
		updates["price"] = *in.Price
	}
	if in.IsActive != nil {
		updates["is_active"] = *in.IsActive
	}
	if in.RequiresVehicle != nil {
		updates["requires_vehicle"] = *in.RequiresVehicle
	}
	if len(updates) > 0 {
		if err := h.DB.Model(&models.Service{}).Where("id = ? AND organization_id = ?", id, orgID).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update service"})
			return
		}
	}
	h.DB.Where("id = ?", id).First(&service)
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

// GetServiceCategories returns the organisation's categories with counts.
// "categories" stays a list of names for older callers; "details" has figures.
func (h *Handler) GetServiceCategories(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	type catRow struct {
		Name            string  `json:"name"`
		ServiceCount    int64   `json:"service_count"`
		ActiveCount     int64   `json:"active_count"`
		AveragePrice    float64 `json:"average_price"`
		MinPrice        float64 `json:"min_price"`
		MaxPrice        float64 `json:"max_price"`
		AverageDuration float64 `json:"average_duration"`
	}
	var rows []catRow
	if err := h.DB.Model(&models.Service{}).
		Select(`category AS name, COUNT(*) AS service_count,
			SUM(CASE WHEN is_active THEN 1 ELSE 0 END) AS active_count,
			COALESCE(AVG(price), 0) AS average_price, COALESCE(MIN(price), 0) AS min_price,
			COALESCE(MAX(price), 0) AS max_price, COALESCE(AVG(duration), 0) AS average_duration`).
		Where("organization_id = ?", orgID).Group("category").Order("category ASC").Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch service categories"})
		return
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if rows == nil {
		rows = []catRow{}
	}
	c.JSON(http.StatusOK, gin.H{"categories": names, "details": rows})
}

// GetServiceStats returns statistics about services
func (h *Handler) GetServiceStats(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Organization not found"})
		return
	}

	var stats struct {
		TotalServices    int64   `json:"total_services"`
		ActiveServices   int64   `json:"active_services"`
		InactiveServices int64   `json:"inactive_services"`
		VehicleServices  int64   `json:"vehicle_services"`
		Categories       int64   `json:"total_categories"`
		AveragePrice     float64 `json:"average_price"`
		AverageDuration  float64 `json:"average_duration"`
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
		Name:            originalService.Name + " (Copy)",
		Description:     originalService.Description,
		Category:        originalService.Category,
		Duration:        originalService.Duration,
		Price:           originalService.Price,
		IsActive:        true,
		RequiresVehicle: originalService.RequiresVehicle,
	}

	// Ensure name doesn't conflict
	baseName := newService.Name
	for counter := 2; h.serviceNameTaken(orgID, newService.Name, newService.Category, "") && counter < 1000; counter++ {
		newService.Name = fmt.Sprintf("%s (%d)", baseName, counter)
	}
	newService.ID = uuid.NewString()

	if err := h.DB.Omit("Organization", "Bookings").Create(&newService).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to duplicate service"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"service": newService})
}
