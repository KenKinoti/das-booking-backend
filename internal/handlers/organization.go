package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

func (h *Handler) GetOrganization(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Find organization
	var organization models.Organization
	if err := h.DB.Where("id = ?", orgID).First(&organization).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "ORGANIZATION_NOT_FOUND",
					"message": "Organization not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch organization",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    organization,
	})
}

type UpdateOrganizationRequest struct {
	Name    *string         `json:"name,omitempty"`
	ABN     *string         `json:"abn,omitempty"`
	Phone   *string         `json:"phone,omitempty"`
	Email   *string         `json:"email,omitempty" binding:"omitempty,email"`
	Website *string         `json:"website,omitempty"`
	Address *models.Address `json:"address,omitempty"`
	NDISReg *models.NDISReg `json:"ndis_registration,omitempty"`
}

func (h *Handler) UpdateOrganization(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	var req UpdateOrganizationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "VALIDATION_ERROR",
				"message": "Invalid request parameters",
				"details": err.Error(),
			},
		})
		return
	}

	// Find organization
	var organization models.Organization
	if err := h.DB.Where("id = ?", orgID).First(&organization).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "ORGANIZATION_NOT_FOUND",
					"message": "Organization not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch organization",
			},
		})
		return
	}

	// Check ABN uniqueness if being updated
	if req.ABN != nil && *req.ABN != organization.ABN {
		var existingOrg models.Organization
		if err := h.DB.Where("abn = ? AND id != ?", *req.ABN, orgID).First(&existingOrg).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "ABN_EXISTS",
					"message": "Another organization with this ABN already exists",
				},
			})
			return
		}
	}

	// Update fields
	updates := make(map[string]interface{})
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.ABN != nil {
		updates["abn"] = *req.ABN
	}
	if req.Phone != nil {
		updates["phone"] = *req.Phone
	}
	if req.Email != nil {
		updates["email"] = *req.Email
	}
	if req.Website != nil {
		updates["website"] = *req.Website
	}

	// Handle embedded structs
	if req.Address != nil {
		updates["address_street"] = req.Address.Street
		updates["address_suburb"] = req.Address.Suburb
		updates["address_state"] = req.Address.State
		updates["address_postcode"] = req.Address.Postcode
		updates["address_country"] = req.Address.Country
	}

	if req.NDISReg != nil {
		updates["ndis_registration_number"] = req.NDISReg.RegistrationNumber
		updates["ndis_registration_status"] = req.NDISReg.RegistrationStatus
		updates["ndis_expiry_date"] = req.NDISReg.ExpiryDate
	}

	if err := h.DB.Model(&organization).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update organization",
			},
		})
		return
	}

	// Fetch updated organization
	h.DB.First(&organization, "id = ?", orgID)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    organization,
		"message": "Organization updated successfully",
	})
}
