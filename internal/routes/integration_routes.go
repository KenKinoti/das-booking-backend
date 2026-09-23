package routes

import (
	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"gorm.io/gorm"
)

// SetupIntegrationRoutes sets up all integration-related routes
func SetupIntegrationRoutes(router *gin.RouterGroup, db *gorm.DB) {
	integrationHandler := handlers.NewIntegrationHandler(db)

	// Integration settings routes
	integrations := router.Group("/integrations")
	{
		// Organization-specific integration settings
		orgGroup := integrations.Group("/organizations/:organizationId")
		{
			// Settings management
			orgGroup.GET("/settings", integrationHandler.GetIntegrationSettings)
			orgGroup.PUT("/settings", integrationHandler.UpdateIntegrationSettings)

			// Connection testing
			orgGroup.POST("/test/stripe", integrationHandler.TestStripeConnection)
			orgGroup.POST("/test/email", integrationHandler.TestEmailConnection)
			orgGroup.POST("/test/twilio", integrationHandler.TestTwilioConnection)
			orgGroup.POST("/test/whatsapp", integrationHandler.TestWhatsAppConnection)

			// Sending test messages
			orgGroup.POST("/send/test-email", integrationHandler.SendTestEmail)
			orgGroup.POST("/send/test-sms", integrationHandler.SendTestSMS)
			orgGroup.POST("/send/test-whatsapp", integrationHandler.SendTestWhatsApp)

			// Logs and monitoring
			orgGroup.GET("/logs/integrations", integrationHandler.GetIntegrationLogs)
			orgGroup.GET("/logs/communications", integrationHandler.GetCommunicationLogs)
			orgGroup.GET("/transactions/payments", integrationHandler.GetPaymentTransactions)
		}

		// Webhook endpoints (these don't require organization ID in URL)
		webhooks := integrations.Group("/webhooks")
		{
			// Stripe webhooks
			webhooks.POST("/stripe/:organizationId", func(c *gin.Context) {
				// Handle Stripe webhook
				c.JSON(200, gin.H{"message": "Stripe webhook received"})
			})

			// Twilio webhooks
			webhooks.POST("/twilio/:organizationId", func(c *gin.Context) {
				// Handle Twilio webhook
				c.JSON(200, gin.H{"message": "Twilio webhook received"})
			})

			// WhatsApp webhooks
			webhooks.GET("/whatsapp/:organizationId", func(c *gin.Context) {
				// WhatsApp webhook verification
				mode := c.Query("hub.mode")
				token := c.Query("hub.verify_token")
				challenge := c.Query("hub.challenge")

				// For now, just return the challenge
				if mode == "subscribe" {
					c.String(200, challenge)
				} else {
					c.JSON(403, gin.H{"error": "Forbidden"})
				}
			})

			webhooks.POST("/whatsapp/:organizationId", func(c *gin.Context) {
				// Handle WhatsApp webhook
				c.JSON(200, gin.H{"message": "WhatsApp webhook received"})
			})
		}

		// General integration utilities
		integrations.GET("/templates/email", func(c *gin.Context) {
			// Return email templates
			c.JSON(200, gin.H{
				"templates": map[string]interface{}{
					"booking_confirmation": map[string]string{
						"subject": "Booking Confirmation - {{.BookingID}}",
						"body":    "Dear {{.CustomerName}}, your booking has been confirmed...",
					},
					"booking_reminder": map[string]string{
						"subject": "Booking Reminder - {{.BookingID}}",
						"body":    "Dear {{.CustomerName}}, this is a reminder about your upcoming booking...",
					},
				},
			})
		})

		integrations.GET("/templates/sms", func(c *gin.Context) {
			// Return SMS templates
			c.JSON(200, gin.H{
				"templates": map[string]string{
					"booking_confirmation": "Booking Confirmed! ID: {{.BookingID}}, Date: {{.Date}}, Time: {{.Time}}",
					"booking_reminder":     "Reminder: Your appointment ({{.BookingID}}) is tomorrow at {{.Time}}",
				},
			})
		})

		integrations.GET("/templates/whatsapp", func(c *gin.Context) {
			// Return WhatsApp templates
			c.JSON(200, gin.H{
				"templates": map[string]string{
					"booking_confirmation": "🎉 *Booking Confirmed!*\n\n📋 Details:\n• ID: {{.BookingID}}\n• Date: {{.Date}}",
					"booking_reminder":     "⏰ *Booking Reminder*\n\nYour appointment is tomorrow at {{.Time}}",
				},
			})
		})
	}
}

// SetupSystemRoutes sets up system-wide integration routes
func SetupSystemRoutes(router *gin.RouterGroup, db *gorm.DB) {
	// System-wide integration status
	router.GET("/system/integrations/status", func(c *gin.Context) {
		// Get overall integration status across all organizations
		var activeIntegrations []map[string]interface{}

		// Query active integrations
		result := db.Raw(`
			SELECT
				organization_id,
				COUNT(*) as total_integrations,
				SUM(CASE WHEN stripe_enabled = true THEN 1 ELSE 0 END) as stripe_active,
				SUM(CASE WHEN email_enabled = true THEN 1 ELSE 0 END) as email_active,
				SUM(CASE WHEN whatsapp_enabled = true THEN 1 ELSE 0 END) as whatsapp_active,
				SUM(CASE WHEN twilio_enabled = true THEN 1 ELSE 0 END) as twilio_active
			FROM integration_settings
			GROUP BY organization_id
		`).Scan(&activeIntegrations)

		if result.Error != nil {
			c.JSON(500, gin.H{"error": "Failed to fetch integration status"})
			return
		}

		c.JSON(200, gin.H{
			"active_integrations": activeIntegrations,
			"supported_providers": []string{
				"stripe", "paypal", "twilio", "whatsapp_business",
				"zoho", "smtp", "sendgrid", "mailgun",
			},
		})
	})

	// Health check for integrations
	router.GET("/system/integrations/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "healthy",
			"services": map[string]string{
				"stripe":    "operational",
				"email":     "operational",
				"twilio":    "operational",
				"whatsapp":  "operational",
				"zoho":      "operational",
			},
			"timestamp": "2024-01-01T00:00:00Z",
		})
	})
}