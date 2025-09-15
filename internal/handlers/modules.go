package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// GetOrganizationModules returns the module configuration for the organization
func (h *Handler) GetOrganizationModules(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var modules models.OrganizationModules
	if err := h.DB.Where("organization_id = ?", orgID).First(&modules).Error; err != nil {
		// If no modules config exists, create default one
		modules = models.OrganizationModules{
			OrganizationID:       orgID,
			InventoryEnabled:     false,
			SupplierEnabled:      false,
			PurchaseOrderEnabled: false,
			POSEnabled:          false,
			CRMEnabled:          false,
			ReportsEnabled:      false,
		}
		h.DB.Create(&modules)
	}

	c.JSON(http.StatusOK, gin.H{"modules": modules})
}

// UpdateOrganizationModules updates the module configuration for the organization
func (h *Handler) UpdateOrganizationModules(c *gin.Context) {
	orgID := h.getOrganizationID(c)
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID required"})
		return
	}

	var updateData models.OrganizationModules
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var modules models.OrganizationModules
	if err := h.DB.Where("organization_id = ?", orgID).First(&modules).Error; err != nil {
		// Create new modules config if it doesn't exist
		updateData.OrganizationID = orgID
		if err := h.DB.Create(&updateData).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create module configuration"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"modules": updateData})
		return
	}

	// Update existing configuration
	updateData.OrganizationID = orgID
	updateData.ID = modules.ID

	if err := h.DB.Model(&modules).Updates(updateData).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update module configuration"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"modules": modules})
}

// GetAllOrganizationModules returns module configurations for all organizations (super admin only)
func (h *Handler) GetAllOrganizationModules(c *gin.Context) {
	var modules []models.OrganizationModules
	if err := h.DB.Preload("Organization").Find(&modules).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch module configurations"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"modules": modules})
}

// UpdateOrganizationModulesById updates module configuration for a specific organization (super admin only)
func (h *Handler) UpdateOrganizationModulesById(c *gin.Context) {
	orgID := c.Param("org_id")

	var updateData models.OrganizationModules
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var modules models.OrganizationModules
	if err := h.DB.Where("organization_id = ?", orgID).First(&modules).Error; err != nil {
		// Create new modules config if it doesn't exist
		updateData.OrganizationID = orgID
		if err := h.DB.Create(&updateData).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create module configuration"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"modules": updateData})
		return
	}

	// Update existing configuration
	updateData.OrganizationID = orgID
	updateData.ID = modules.ID

	if err := h.DB.Model(&modules).Updates(updateData).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update module configuration"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"modules": modules})
}