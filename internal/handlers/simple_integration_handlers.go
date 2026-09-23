package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

// SimpleIntegrationHandler handles basic integration endpoints for testing
type SimpleIntegrationHandler struct {
	db *gorm.DB
}

// NewSimpleIntegrationHandler creates a new simple integration handler
func NewSimpleIntegrationHandler(db *gorm.DB) *SimpleIntegrationHandler {
	return &SimpleIntegrationHandler{
		db: db,
	}
}

// GetIntegrationSettings gets integration settings for an organization
func (h *SimpleIntegrationHandler) GetIntegrationSettings(c *gin.Context) {
	organizationID := c.Param("organizationId")
	if organizationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID is required"})
		return
	}

	var settings models.IntegrationSettings
	if err := h.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			// Create default settings
			settings = models.IntegrationSettings{
				OrganizationID: organizationID,
				GeneralSettings: models.GeneralSettings{
					PlatformName:    "DAS Booking Platform",
					DefaultTimezone: "UTC",
					DefaultCurrency: "USD",
				},
				EmailSettings: models.EmailSettings{
					Provider: "smtp",
					SMTPPort: 587,
					SMTPSecurity: "tls",
				},
				SecuritySettings: models.SecuritySettings{
					SessionTimeout:      30,
					PasswordMinLength:   8,
					RequireSpecialChars: true,
					RequireNumbers:      true,
					RateLimitRequests:   100,
					RateLimitWindow:     15,
				},
			}

			if err := h.db.Create(&settings).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create default settings"})
				return
			}
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch settings"})
			return
		}
	}

	// Mask sensitive fields for response
	settingsResponse := h.maskSensitiveFields(settings)

	c.JSON(http.StatusOK, gin.H{"data": settingsResponse})
}

// UpdateIntegrationSettings updates integration settings
func (h *SimpleIntegrationHandler) UpdateIntegrationSettings(c *gin.Context) {
	organizationID := c.Param("organizationId")
	if organizationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID is required"})
		return
	}

	var updateData models.IntegrationSettings
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}

	// Get existing settings
	var existingSettings models.IntegrationSettings
	if err := h.db.Where("organization_id = ?", organizationID).First(&existingSettings).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			// Create new settings
			updateData.OrganizationID = organizationID
			if err := h.encryptSensitiveFields(&updateData); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to encrypt sensitive data"})
				return
			}

			if err := h.db.Create(&updateData).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create settings"})
				return
			}

			c.JSON(http.StatusCreated, gin.H{
				"message": "Integration settings created successfully",
				"data":    h.maskSensitiveFields(updateData),
			})
			return
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch existing settings"})
			return
		}
	}

	// Merge updates with existing settings
	updateData.ID = existingSettings.ID
	updateData.OrganizationID = organizationID
	updateData.CreatedAt = existingSettings.CreatedAt

	// Encrypt sensitive fields
	if err := h.encryptSensitiveFields(&updateData); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to encrypt sensitive data"})
		return
	}

	// Update settings
	if err := h.db.Save(&updateData).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update settings"})
		return
	}

	// Log integration event
	h.logIntegrationEvent(organizationID, "settings_updated", "success", "Integration settings updated", "", "")

	c.JSON(http.StatusOK, gin.H{
		"message": "Integration settings updated successfully",
		"data":    h.maskSensitiveFields(updateData),
	})
}

// Simple connection tests (for demo purposes)
func (h *SimpleIntegrationHandler) TestStripeConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")

	// Simulate Stripe connection test
	time.Sleep(1 * time.Second)

	// For demo, always return success
	h.logIntegrationEvent(organizationID, "stripe_test_success", "success", "Stripe connection test successful (simulated)", "", "")

	c.JSON(http.StatusOK, gin.H{
		"message":   "Stripe connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func (h *SimpleIntegrationHandler) TestEmailConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")

	// Simulate email connection test
	time.Sleep(1 * time.Second)

	h.logIntegrationEvent(organizationID, "email_test_success", "success", "Email connection test successful (simulated)", "", "")

	c.JSON(http.StatusOK, gin.H{
		"message":   "Email connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func (h *SimpleIntegrationHandler) TestTwilioConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")

	// Simulate Twilio connection test
	time.Sleep(1 * time.Second)

	h.logIntegrationEvent(organizationID, "twilio_test_success", "success", "Twilio connection test successful (simulated)", "", "")

	c.JSON(http.StatusOK, gin.H{
		"message":   "Twilio connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func (h *SimpleIntegrationHandler) TestWhatsAppConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")

	// Simulate WhatsApp connection test
	time.Sleep(1 * time.Second)

	h.logIntegrationEvent(organizationID, "whatsapp_test_success", "success", "WhatsApp connection test successful (simulated)", "", "")

	c.JSON(http.StatusOK, gin.H{
		"message":   "WhatsApp connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

// Send test messages (simulated)
func (h *SimpleIntegrationHandler) SendTestEmail(c *gin.Context) {
	organizationID := c.Param("organizationId")

	var requestData struct {
		ToEmail string `json:"to_email" binding:"required,email"`
		Subject string `json:"subject"`
		Message string `json:"message"`
	}

	if err := c.ShouldBindJSON(&requestData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}

	// Simulate sending email
	time.Sleep(2 * time.Second)

	// Log the communication
	logEntry := &models.CommunicationLog{
		OrganizationID: organizationID,
		Type:           "email",
		Provider:       "smtp",
		Direction:      "outbound",
		ToEmail:        requestData.ToEmail,
		Subject:        requestData.Subject,
		Message:        requestData.Message,
		Status:         "sent",
		MessageType:    "text",
	}

	if err := h.db.Create(logEntry).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to log communication"})
		return
	}

	h.logIntegrationEvent(organizationID, "test_email_sent", "success", fmt.Sprintf("Test email sent to %s", requestData.ToEmail), "", "")

	c.JSON(http.StatusOK, gin.H{"message": "Test email sent successfully (simulated)"})
}

func (h *SimpleIntegrationHandler) SendTestSMS(c *gin.Context) {
	organizationID := c.Param("organizationId")

	var requestData struct {
		ToNumber string `json:"to_number" binding:"required"`
		Message  string `json:"message"`
	}

	if err := c.ShouldBindJSON(&requestData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}

	// Simulate sending SMS
	time.Sleep(2 * time.Second)

	// Log the communication
	logEntry := &models.CommunicationLog{
		OrganizationID: organizationID,
		Type:           "sms",
		Provider:       "twilio",
		Direction:      "outbound",
		ToNumber:       requestData.ToNumber,
		Message:        requestData.Message,
		Status:         "sent",
		MessageType:    "text",
	}

	if err := h.db.Create(logEntry).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to log communication"})
		return
	}

	h.logIntegrationEvent(organizationID, "test_sms_sent", "success", fmt.Sprintf("Test SMS sent to %s", requestData.ToNumber), "", "")

	c.JSON(http.StatusOK, gin.H{"message": "Test SMS sent successfully (simulated)"})
}

func (h *SimpleIntegrationHandler) SendTestWhatsApp(c *gin.Context) {
	organizationID := c.Param("organizationId")

	var requestData struct {
		ToNumber string `json:"to_number" binding:"required"`
		Message  string `json:"message"`
	}

	if err := c.ShouldBindJSON(&requestData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}

	// Simulate sending WhatsApp message
	time.Sleep(2 * time.Second)

	// Log the communication
	logEntry := &models.CommunicationLog{
		OrganizationID: organizationID,
		Type:           "whatsapp",
		Provider:       "whatsapp_business",
		Direction:      "outbound",
		ToNumber:       requestData.ToNumber,
		Message:        requestData.Message,
		Status:         "sent",
		MessageType:    "text",
	}

	if err := h.db.Create(logEntry).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to log communication"})
		return
	}

	h.logIntegrationEvent(organizationID, "test_whatsapp_sent", "success", fmt.Sprintf("Test WhatsApp message sent to %s", requestData.ToNumber), "", "")

	c.JSON(http.StatusOK, gin.H{"message": "Test WhatsApp message sent successfully (simulated)"})
}

// Get logs
func (h *SimpleIntegrationHandler) GetIntegrationLogs(c *gin.Context) {
	organizationID := c.Param("organizationId")
	page := c.DefaultQuery("page", "1")
	limit := c.DefaultQuery("limit", "50")

	// Parse pagination
	var offset, limitInt int
	if _, err := fmt.Sscanf(page, "%d", &offset); err != nil {
		offset = 1
	}
	if _, err := fmt.Sscanf(limit, "%d", &limitInt); err != nil {
		limitInt = 50
	}
	offset = (offset - 1) * limitInt

	// Get logs
	var logs []models.IntegrationLog
	var totalCount int64

	query := h.db.Where("organization_id = ?", organizationID)
	query.Model(&models.IntegrationLog{}).Count(&totalCount)

	if err := query.Order("created_at DESC").Offset(offset).Limit(limitInt).Find(&logs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch integration logs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": logs,
		"pagination": gin.H{
			"total":    totalCount,
			"page":     page,
			"limit":    limitInt,
			"has_more": offset+limitInt < int(totalCount),
		},
	})
}

func (h *SimpleIntegrationHandler) GetCommunicationLogs(c *gin.Context) {
	organizationID := c.Param("organizationId")
	page := c.DefaultQuery("page", "1")
	limit := c.DefaultQuery("limit", "50")

	// Parse pagination
	var offset, limitInt int
	if _, err := fmt.Sscanf(page, "%d", &offset); err != nil {
		offset = 1
	}
	if _, err := fmt.Sscanf(limit, "%d", &limitInt); err != nil {
		limitInt = 50
	}
	offset = (offset - 1) * limitInt

	// Get logs
	var logs []models.CommunicationLog
	var totalCount int64

	query := h.db.Where("organization_id = ?", organizationID)
	query.Model(&models.CommunicationLog{}).Count(&totalCount)

	if err := query.Order("created_at DESC").Offset(offset).Limit(limitInt).Find(&logs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch communication logs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": logs,
		"pagination": gin.H{
			"total":    totalCount,
			"page":     page,
			"limit":    limitInt,
			"has_more": offset+limitInt < int(totalCount),
		},
	})
}

func (h *SimpleIntegrationHandler) GetPaymentTransactions(c *gin.Context) {
	organizationID := c.Param("organizationId")

	// Get payment transactions
	var transactions []models.PaymentTransaction
	if err := h.db.Where("organization_id = ?", organizationID).Order("created_at DESC").Find(&transactions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch payment transactions"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": transactions})
}

// Helper methods
func (h *SimpleIntegrationHandler) maskSensitiveFields(settings models.IntegrationSettings) models.IntegrationSettings {
	// Create a copy to avoid modifying the original
	masked := settings

	// Mask sensitive fields
	if masked.StripeSettings.SecretKey != "" {
		masked.StripeSettings.SecretKey = "sk_****"
	}
	if masked.EmailSettings.SMTPPassword != "" {
		masked.EmailSettings.SMTPPassword = "****"
	}
	if masked.TwilioSettings.AuthToken != "" {
		masked.TwilioSettings.AuthToken = "****"
	}
	if masked.WhatsAppSettings.AccessToken != "" {
		masked.WhatsAppSettings.AccessToken = "EAA****"
	}

	return masked
}

func (h *SimpleIntegrationHandler) encryptSensitiveFields(settings *models.IntegrationSettings) error {
	// For demo purposes, just return nil (no actual encryption needed for testing)
	return nil
}

func (h *SimpleIntegrationHandler) logIntegrationEvent(organizationID, event, status, message, errorCode, userID string) {
	logEntry := &models.IntegrationLog{
		OrganizationID: organizationID,
		Integration:    "system",
		Event:          event,
		Status:         status,
		Message:        message,
		ErrorCode:      errorCode,
		UserID:         userID,
	}

	if err := h.db.Create(logEntry).Error; err != nil {
		// Don't return error, just log it
		fmt.Printf("Failed to log integration event: %v\n", err)
	}
}