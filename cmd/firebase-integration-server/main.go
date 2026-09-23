package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// IntegrationHandler handles integration endpoints without external service dependencies
type IntegrationHandler struct {
	db *gorm.DB
}

// NewIntegrationHandler creates a new integration handler
func NewIntegrationHandler(db *gorm.DB) *IntegrationHandler {
	return &IntegrationHandler{
		db: db,
	}
}

// GetIntegrationSettings gets integration settings for an organization
func (h *IntegrationHandler) GetIntegrationSettings(c *gin.Context) {
	organizationID := c.Param("organizationId")
	if organizationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID is required"})
		return
	}

	var settings models.IntegrationSettings
	if err := h.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			// Create default settings including Firebase
			settings = models.IntegrationSettings{
				OrganizationID: organizationID,
				GeneralSettings: models.GeneralSettings{
					PlatformName:    "DAS Booking Platform",
					DefaultTimezone: "UTC",
					DefaultCurrency: "USD",
					DefaultLanguage: "en",
				},
				EmailSettings: models.EmailSettings{
					Provider: "smtp",
					SMTPPort: 587,
					SMTPSecurity: "tls",
					FromName: "DAS Booking Platform",
				},
				SecuritySettings: models.SecuritySettings{
					SessionTimeout:      30,
					PasswordMinLength:   8,
					RequireSpecialChars: true,
					RequireNumbers:      true,
					RateLimitRequests:   100,
					RateLimitWindow:     15,
				},
				StripeSettings: models.StripeSettings{
					Enabled:       false,
					TestMode:      true,
					Currency:      "USD",
					CaptureMethod: "automatic",
					PaymentMethods: `["card"]`,
				},
				PayPalSettings: models.PayPalSettings{
					Enabled:     false,
					SandboxMode: true,
					Currency:    "USD",
				},
				TwilioSettings: models.TwilioSettings{
					Enabled:        false,
					EnableSMS:      true,
					EnableVoice:    false,
					EnableWhatsApp: false,
				},
				WhatsAppSettings: models.WhatsAppSettings{
					Enabled:    false,
					APIVersion: "v18.0",
				},
				FirebaseSettings: models.FirebaseSettings{
					Enabled:           false,
					ProjectID:         "",
					EnableAuth:        true,
					EnableFirestore:   true,
					EnableStorage:     true,
					EnableFunctions:   false,
					EnableMessaging:   false,
					EnableAnalytics:   true,
				},
				ZohoSettings: models.ZohoSettings{
					Enabled:   false,
					APIRegion: "com",
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
func (h *IntegrationHandler) UpdateIntegrationSettings(c *gin.Context) {
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

// Connection test endpoints (all simulated)
func (h *IntegrationHandler) TestStripeConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")
	time.Sleep(1 * time.Second) // Simulate API call
	h.logIntegrationEvent(organizationID, "stripe_test_success", "success", "Stripe connection test successful (simulated)", "", "")
	c.JSON(http.StatusOK, gin.H{
		"message":   "Stripe connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func (h *IntegrationHandler) TestPayPalConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")
	time.Sleep(1 * time.Second)
	h.logIntegrationEvent(organizationID, "paypal_test_success", "success", "PayPal connection test successful (simulated)", "", "")
	c.JSON(http.StatusOK, gin.H{
		"message":   "PayPal connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func (h *IntegrationHandler) TestFirebaseConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")
	time.Sleep(2 * time.Second) // Simulate longer Firebase setup validation
	h.logIntegrationEvent(organizationID, "firebase_test_success", "success", "Firebase connection test successful (simulated)", "", "")
	c.JSON(http.StatusOK, gin.H{
		"message":   "Firebase connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
		"services": gin.H{
			"auth":      "connected",
			"firestore": "connected",
			"storage":   "connected",
			"functions": "not_configured",
		},
	})
}

func (h *IntegrationHandler) TestZohoConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")
	time.Sleep(1 * time.Second)
	h.logIntegrationEvent(organizationID, "zoho_test_success", "success", "Zoho connection test successful (simulated)", "", "")
	c.JSON(http.StatusOK, gin.H{
		"message":   "Zoho connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
		"services":  []string{"CRM", "Books", "Mail"},
	})
}

func (h *IntegrationHandler) TestEmailConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")
	time.Sleep(1 * time.Second)
	h.logIntegrationEvent(organizationID, "email_test_success", "success", "Email connection test successful (simulated)", "", "")
	c.JSON(http.StatusOK, gin.H{
		"message":   "Email connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func (h *IntegrationHandler) TestTwilioConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")
	time.Sleep(1 * time.Second)
	h.logIntegrationEvent(organizationID, "twilio_test_success", "success", "Twilio connection test successful (simulated)", "", "")
	c.JSON(http.StatusOK, gin.H{
		"message":   "Twilio connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func (h *IntegrationHandler) TestWhatsAppConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")
	time.Sleep(1 * time.Second)
	h.logIntegrationEvent(organizationID, "whatsapp_test_success", "success", "WhatsApp connection test successful (simulated)", "", "")
	c.JSON(http.StatusOK, gin.H{
		"message":   "WhatsApp connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

// Test message endpoints
func (h *IntegrationHandler) SendTestEmail(c *gin.Context) {
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

	time.Sleep(2 * time.Second) // Simulate email sending

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

// Get logs endpoints
func (h *IntegrationHandler) GetIntegrationLogs(c *gin.Context) {
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

func (h *IntegrationHandler) GetCommunicationLogs(c *gin.Context) {
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

// Helper methods
func (h *IntegrationHandler) maskSensitiveFields(settings models.IntegrationSettings) models.IntegrationSettings {
	masked := settings
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
	if masked.FirebaseSettings.PrivateKey != "" {
		masked.FirebaseSettings.PrivateKey = "****"
	}
	if masked.PayPalSettings.ClientSecret != "" {
		masked.PayPalSettings.ClientSecret = "****"
	}
	return masked
}

func (h *IntegrationHandler) logIntegrationEvent(organizationID, event, status, message, errorCode, userID string) {
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
		log.Printf("Failed to log integration event: %v\n", err)
	}
}

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found: %v", err)
	}

	// Initialize SQLite database
	db, err := gorm.Open(sqlite.Open("firebase_integrations.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Run migrations
	log.Printf("Running database migrations...")
	if err := db.AutoMigrate(
		&models.Organization{},
		&models.User{},
		&models.Customer{},
		&models.Service{},
		&models.Booking{},
		&models.IntegrationSettings{},
		&models.IntegrationLog{},
		&models.PaymentTransaction{},
		&models.CommunicationLog{},
	); err != nil {
		log.Fatal("Migration failed:", err)
	}

	// Create sample data
	if err := createSampleData(db); err != nil {
		log.Printf("Warning: Failed to create sample data: %v", err)
	}

	// Setup Gin router
	gin.SetMode(gin.DebugMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	// CORS middleware
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS,PATCH")
		c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Requested-With")
		c.Header("Access-Control-Expose-Headers", "Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// API routes
	v1 := r.Group("/api/v1")
	{
		v1.GET("/health", func(c *gin.Context) {
			c.JSON(200, gin.H{
				"status":      "ok",
				"service":     "DAS Booking Integration Server with Firebase",
				"version":     "1.0.0",
				"environment": "test",
				"integrations": []string{
					"Firebase (Auth, Firestore, Storage)",
					"Stripe Payments",
					"PayPal Payments",
					"Twilio SMS & Voice",
					"WhatsApp Business API",
					"SMTP Email",
					"Zoho Suite (CRM, Books, Mail)",
				},
			})
		})

		integrationHandler := NewIntegrationHandler(db)

		// Integration routes
		integrations := v1.Group("/integrations")
		{
			orgGroup := integrations.Group("/organizations/:organizationId")
			{
				// Settings management
				orgGroup.GET("/settings", integrationHandler.GetIntegrationSettings)
				orgGroup.PUT("/settings", integrationHandler.UpdateIntegrationSettings)

				// Connection testing
				orgGroup.POST("/test/stripe", integrationHandler.TestStripeConnection)
				orgGroup.POST("/test/paypal", integrationHandler.TestPayPalConnection)
				orgGroup.POST("/test/firebase", integrationHandler.TestFirebaseConnection)
				orgGroup.POST("/test/zoho", integrationHandler.TestZohoConnection)
				orgGroup.POST("/test/email", integrationHandler.TestEmailConnection)
				orgGroup.POST("/test/twilio", integrationHandler.TestTwilioConnection)
				orgGroup.POST("/test/whatsapp", integrationHandler.TestWhatsAppConnection)

				// Test messaging
				orgGroup.POST("/send/test-email", integrationHandler.SendTestEmail)

				// Logs and monitoring
				orgGroup.GET("/logs/integrations", integrationHandler.GetIntegrationLogs)
				orgGroup.GET("/logs/communications", integrationHandler.GetCommunicationLogs)
			}
		}
	}

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	log.Printf("🚀 DAS Booking Integration Server with Firebase starting on port %s", port)
	log.Printf("📊 Environment: test")
	log.Printf("💾 Database: SQLite (firebase_integrations.db)")
	log.Printf("🔗 Available integrations:")
	log.Printf("   📱 Firebase (Auth, Firestore, Storage)")
	log.Printf("   💳 Stripe & PayPal Payments")
	log.Printf("   📞 Twilio & WhatsApp Communications")
	log.Printf("   ✉️  SMTP Email")
	log.Printf("   🏢 Zoho Suite (CRM, Books, Mail)")
	log.Printf("🌐 Health Check: http://localhost:%s/api/v1/health", port)
	log.Printf("⚙️  Settings API: http://localhost:%s/api/v1/integrations/organizations/org_sample_123/settings", port)

	if err := r.Run(":" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}

func createSampleData(db *gorm.DB) error {
	// Check if sample data already exists
	var count int64
	db.Model(&models.Organization{}).Count(&count)
	if count > 0 {
		log.Printf("Sample data already exists, skipping initialization")
		return nil
	}

	log.Printf("Creating sample data...")

	// Create sample organization
	org := &models.Organization{
		ID:           "org_sample_123",
		Name:         "Sample Auto Service with Firebase",
		BusinessType: "automotive",
		Phone:        "+1-555-0123",
		Email:        "info@sampleauto.com",
	}

	if err := db.Create(org).Error; err != nil {
		return err
	}

	// Create sample customer
	customer := &models.Customer{
		ID:             "customer_john_123",
		OrganizationID: org.ID,
		FirstName:      "John",
		LastName:       "Doe",
		Email:          "john.doe@example.com",
		Phone:          "+1-555-0125",
		IsActive:       true,
	}

	if err := db.Create(customer).Error; err != nil {
		return err
	}

	// Create sample service
	service := &models.Service{
		ID:             "service_oil_change",
		OrganizationID: org.ID,
		Name:           "Oil Change",
		Description:    "Complete oil change with filter replacement",
		Price:          59.99,
		Duration:       30,
		Category:       "maintenance",
		IsActive:       true,
	}

	if err := db.Create(service).Error; err != nil {
		return err
	}

	log.Printf("✅ Sample data created successfully")
	log.Printf("🏢 Sample organization: %s (%s)", org.Name, org.ID)
	log.Printf("👤 Sample customer: %s %s (%s)", customer.FirstName, customer.LastName, customer.ID)
	log.Printf("🔧 Sample service: %s (%s)", service.Name, service.ID)

	return nil
}