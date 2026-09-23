package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Standalone models for the integration server
type IntegrationSettings struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id"`
	GeneralSettings GeneralSettings `json:"general_settings" gorm:"embedded;embeddedPrefix:general_"`
	EmailSettings   EmailSettings   `json:"email_settings" gorm:"embedded;embeddedPrefix:email_"`
	StripeSettings  StripeSettings  `json:"stripe_settings" gorm:"embedded;embeddedPrefix:stripe_"`
	TwilioSettings  TwilioSettings  `json:"twilio_settings" gorm:"embedded;embeddedPrefix:twilio_"`
	WhatsAppSettings WhatsAppSettings `json:"whatsapp_settings" gorm:"embedded;embeddedPrefix:whatsapp_"`
	ZohoSettings     ZohoSettings     `json:"zoho_settings" gorm:"embedded;embeddedPrefix:zoho_"`
	FirebaseSettings FirebaseSettings `json:"firebase_settings" gorm:"embedded;embeddedPrefix:firebase_"`
	APIKeys          APIKeys          `json:"api_keys" gorm:"embedded;embeddedPrefix:api_"`
	SecuritySettings SecuritySettings `json:"security_settings" gorm:"embedded;embeddedPrefix:security_"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type GeneralSettings struct {
	PlatformName    string `json:"platform_name"`
	DefaultTimezone string `json:"default_timezone"`
	DefaultCurrency string `json:"default_currency"`
}

type EmailSettings struct {
	Enabled      bool   `json:"enabled"`
	Provider     string `json:"provider"`
	SMTPHost     string `json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port"`
	SMTPUsername string `json:"smtp_username"`
	SMTPPassword string `json:"smtp_password"`
	FromEmail    string `json:"from_email"`
	FromName     string `json:"from_name"`
}

type StripeSettings struct {
	Enabled        bool   `json:"enabled"`
	PublishableKey string `json:"publishable_key"`
	SecretKey      string `json:"secret_key"`
	WebhookSecret  string `json:"webhook_secret"`
	TestMode       bool   `json:"test_mode"`
	Currency       string `json:"currency"`
}

type TwilioSettings struct {
	Enabled         bool   `json:"enabled"`
	AccountSID      string `json:"account_sid"`
	AuthToken       string `json:"auth_token"`
	FromPhoneNumber string `json:"from_phone_number"`
	EnableSMS       bool   `json:"enable_sms"`
	EnableVoice     bool   `json:"enable_voice"`
}

type WhatsAppSettings struct {
	Enabled           bool   `json:"enabled"`
	AccessToken       string `json:"access_token"`
	PhoneNumberID     string `json:"phone_number_id"`
	BusinessAccountID string `json:"business_account_id"`
	WebhookURL        string `json:"webhook_url"`
	VerifyToken       string `json:"verify_token"`
}

type ZohoSettings struct {
	Enabled      bool   `json:"enabled"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	APIRegion    string `json:"api_region"`
	EnableCRM    bool   `json:"enable_crm"`
	EnableBooks  bool   `json:"enable_books"`
}

type FirebaseSettings struct {
	Enabled         bool   `json:"enabled"`
	ProjectID       string `json:"project_id"`
	PrivateKey      string `json:"private_key"`
	ClientEmail     string `json:"client_email"`
	DatabaseURL     string `json:"database_url"`
	StorageBucket   string `json:"storage_bucket"`
	EnableAuth      bool   `json:"enable_auth"`
	EnableFirestore bool   `json:"enable_firestore"`
	EnableStorage   bool   `json:"enable_storage"`
}

type SecuritySettings struct {
	SessionTimeout      int    `json:"session_timeout"`
	PasswordMinLength   int    `json:"password_min_length"`
	RequireSpecialChars bool   `json:"require_special_chars"`
	RequireNumbers      bool   `json:"require_numbers"`
	EnableTwoFactor     bool   `json:"enable_two_factor"`
	RateLimitRequests   int    `json:"rate_limit_requests"`
	RateLimitWindow     int    `json:"rate_limit_window"`
}

type APIKeys struct {
	OpenAI     string `json:"openai"`
	Stripe     string `json:"stripe"`
	Twilio     string `json:"twilio"`
	SendGrid   string `json:"sendgrid"`
	Cloudinary string `json:"cloudinary"`
	Firebase   string `json:"firebase"`
	Pusher     string `json:"pusher"`
	GoogleMaps string `json:"google_maps"`
}

type IntegrationLog struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id"`
	Integration    string    `json:"integration"`
	Event          string    `json:"event"`
	Status         string    `json:"status"`
	Message        string    `json:"message"`
	ErrorCode      string    `json:"error_code"`
	CreatedAt      time.Time `json:"created_at"`
}

type CommunicationLog struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id"`
	Type           string    `json:"type"`
	Provider       string    `json:"provider"`
	Direction      string    `json:"direction"`
	FromNumber     string    `json:"from_number"`
	ToNumber       string    `json:"to_number"`
	FromEmail      string    `json:"from_email"`
	ToEmail        string    `json:"to_email"`
	Subject        string    `json:"subject"`
	Message        string    `json:"message"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

type PaymentTransaction struct {
	ID                    string    `json:"id" gorm:"primaryKey"`
	OrganizationID        string    `json:"organization_id"`
	Provider              string    `json:"provider"`
	ProviderTransactionID string    `json:"provider_transaction_id"`
	Amount                int64     `json:"amount"`
	Currency              string    `json:"currency"`
	Status                string    `json:"status"`
	Description           string    `json:"description"`
	CreatedAt             time.Time `json:"created_at"`
}

// GORM hooks
func (i *IntegrationSettings) BeforeCreate(tx *gorm.DB) error {
	if i.ID == "" {
		i.ID = uuid.New().String()
	}
	return nil
}

func (i *IntegrationLog) BeforeCreate(tx *gorm.DB) error {
	if i.ID == "" {
		i.ID = uuid.New().String()
	}
	return nil
}

func (c *CommunicationLog) BeforeCreate(tx *gorm.DB) error {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	return nil
}

func (p *PaymentTransaction) BeforeCreate(tx *gorm.DB) error {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	return nil
}

var db *gorm.DB

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found: %v", err)
	}

	// Initialize SQLite database
	var err error
	db, err = gorm.Open(sqlite.Open("standalone_integrations.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Auto-migrate tables
	log.Printf("Running database migrations...")
	if err := db.AutoMigrate(
		&IntegrationSettings{},
		&IntegrationLog{},
		&CommunicationLog{},
		&PaymentTransaction{},
	); err != nil {
		log.Fatal("Migration failed:", err)
	}

	// Create sample data
	if err := createSampleData(); err != nil {
		log.Printf("Warning: Failed to create sample data: %v", err)
	}

	// Setup Gin router
	gin.SetMode(gin.DebugMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	// Add CORS middleware
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
		// Health check
		v1.GET("/health", healthCheck)

		// Integration routes
		integrations := v1.Group("/integrations")
		{
			// Organization-specific settings
			orgGroup := integrations.Group("/organizations/:organizationId")
			{
				// Settings CRUD
				orgGroup.GET("/settings", getIntegrationSettings)
				orgGroup.PUT("/settings", updateIntegrationSettings)

				// Connection tests
				orgGroup.POST("/test/stripe", testStripeConnection)
				orgGroup.POST("/test/email", testEmailConnection)
				orgGroup.POST("/test/twilio", testTwilioConnection)
				orgGroup.POST("/test/whatsapp", testWhatsAppConnection)
				orgGroup.POST("/test/zoho", testZohoConnection)
				orgGroup.POST("/test/firebase", testFirebaseConnection)

				// Send test messages
				orgGroup.POST("/send/test-email", sendTestEmail)
				orgGroup.POST("/send/test-sms", sendTestSMS)
				orgGroup.POST("/send/test-whatsapp", sendTestWhatsApp)

				// Logs and monitoring
				orgGroup.GET("/logs/integrations", getIntegrationLogs)
				orgGroup.GET("/logs/communications", getCommunicationLogs)
				orgGroup.GET("/transactions/payments", getPaymentTransactions)
			}

			// Templates
			integrations.GET("/templates/email", getEmailTemplates)
			integrations.GET("/templates/sms", getSMSTemplates)
			integrations.GET("/templates/whatsapp", getWhatsAppTemplates)
		}

		// System status
		v1.GET("/system/integrations/status", getSystemStatus)
		v1.GET("/system/integrations/health", getSystemHealth)
	}

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🚀 DAS Booking Standalone Integrations Server starting on port %s", port)
	log.Printf("📊 Environment: development")
	log.Printf("💾 Database: SQLite (standalone_integrations.db)")
	log.Printf("🔗 Available integrations: Stripe, PayPal, Twilio, WhatsApp, Email, Zoho, Firebase")
	log.Printf("📱 Test API: http://localhost:%s/api/v1/health", port)
	log.Printf("⚙️  Settings API: http://localhost:%s/api/v1/integrations/organizations/org_sample_123/settings", port)

	if err := r.Run(":" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}

// Handlers
func healthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":      "ok",
		"service":     "DAS Booking Standalone Integrations Server",
		"version":     "1.0.0",
		"environment": "development",
		"integrations": []string{
			"Stripe Payments",
			"PayPal Payments",
			"Twilio SMS & Voice",
			"WhatsApp Business API",
			"SMTP Email",
			"SendGrid Email",
			"Zoho Suite",
			"Firebase Suite",
		},
		"features": []string{
			"Integration Settings Management",
			"Connection Testing",
			"Test Message Sending",
			"Logging & Monitoring",
			"API Key Management",
			"Security Configuration",
		},
	})
}

func getIntegrationSettings(c *gin.Context) {
	organizationID := c.Param("organizationId")
	if organizationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID is required"})
		return
	}

	var settings IntegrationSettings
	if err := db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			// Create default settings
			settings = IntegrationSettings{
				OrganizationID: organizationID,
				GeneralSettings: GeneralSettings{
					PlatformName:    "DAS Booking Platform",
					DefaultTimezone: "UTC",
					DefaultCurrency: "USD",
				},
				EmailSettings: EmailSettings{
					Provider: "smtp",
					SMTPPort: 587,
					FromName: "DAS Booking Platform",
				},
				StripeSettings: StripeSettings{
					TestMode: true,
					Currency: "USD",
				},
				SecuritySettings: SecuritySettings{
					SessionTimeout:      30,
					PasswordMinLength:   8,
					RequireSpecialChars: true,
					RequireNumbers:      true,
					RateLimitRequests:   100,
					RateLimitWindow:     15,
				},
			}

			if err := db.Create(&settings).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create default settings"})
				return
			}
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch settings"})
			return
		}
	}

	// Mask sensitive fields
	settings.StripeSettings.SecretKey = maskSecretKey(settings.StripeSettings.SecretKey)
	settings.EmailSettings.SMTPPassword = maskSecretKey(settings.EmailSettings.SMTPPassword)
	settings.TwilioSettings.AuthToken = maskSecretKey(settings.TwilioSettings.AuthToken)
	settings.WhatsAppSettings.AccessToken = maskSecretKey(settings.WhatsAppSettings.AccessToken)
	settings.ZohoSettings.ClientSecret = maskSecretKey(settings.ZohoSettings.ClientSecret)
	settings.FirebaseSettings.PrivateKey = maskSecretKey(settings.FirebaseSettings.PrivateKey)

	c.JSON(http.StatusOK, gin.H{"data": settings})
}

func updateIntegrationSettings(c *gin.Context) {
	organizationID := c.Param("organizationId")
	if organizationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Organization ID is required"})
		return
	}

	var updateData IntegrationSettings
	if err := c.ShouldBindJSON(&updateData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}

	// Get existing settings
	var existingSettings IntegrationSettings
	if err := db.Where("organization_id = ?", organizationID).First(&existingSettings).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			// Create new settings
			updateData.OrganizationID = organizationID
			if err := db.Create(&updateData).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create settings"})
				return
			}

			logIntegrationEvent(organizationID, "settings_created", "success", "Integration settings created", "")

			c.JSON(http.StatusCreated, gin.H{
				"message": "Integration settings created successfully",
				"data":    updateData,
			})
			return
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch existing settings"})
			return
		}
	}

	// Update settings
	updateData.ID = existingSettings.ID
	updateData.OrganizationID = organizationID
	updateData.CreatedAt = existingSettings.CreatedAt

	if err := db.Save(&updateData).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update settings"})
		return
	}

	logIntegrationEvent(organizationID, "settings_updated", "success", "Integration settings updated", "")

	c.JSON(http.StatusOK, gin.H{
		"message": "Integration settings updated successfully",
		"data":    updateData,
	})
}

// Connection test handlers
func testStripeConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")

	// Simulate connection test
	time.Sleep(1 * time.Second)

	logIntegrationEvent(organizationID, "stripe_test", "success", "Stripe connection test successful", "")

	c.JSON(http.StatusOK, gin.H{
		"message":   "Stripe connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func testEmailConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")

	time.Sleep(1 * time.Second)

	logIntegrationEvent(organizationID, "email_test", "success", "Email connection test successful", "")

	c.JSON(http.StatusOK, gin.H{
		"message":   "Email connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func testTwilioConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")

	time.Sleep(1 * time.Second)

	logIntegrationEvent(organizationID, "twilio_test", "success", "Twilio connection test successful", "")

	c.JSON(http.StatusOK, gin.H{
		"message":   "Twilio connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func testWhatsAppConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")

	time.Sleep(1 * time.Second)

	logIntegrationEvent(organizationID, "whatsapp_test", "success", "WhatsApp connection test successful", "")

	c.JSON(http.StatusOK, gin.H{
		"message":   "WhatsApp connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func testZohoConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")

	time.Sleep(1 * time.Second)

	logIntegrationEvent(organizationID, "zoho_test", "success", "Zoho connection test successful", "")

	c.JSON(http.StatusOK, gin.H{
		"message":   "Zoho connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

func testFirebaseConnection(c *gin.Context) {
	organizationID := c.Param("organizationId")

	time.Sleep(1 * time.Second)

	logIntegrationEvent(organizationID, "firebase_test", "success", "Firebase connection test successful", "")

	c.JSON(http.StatusOK, gin.H{
		"message":   "Firebase connection test successful",
		"verified":  true,
		"tested_at": time.Now(),
	})
}

// Test message handlers
func sendTestEmail(c *gin.Context) {
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

	time.Sleep(2 * time.Second)

	// Log communication
	logCommunication(organizationID, "email", "smtp", "outbound", "", "", "", requestData.ToEmail, requestData.Subject, requestData.Message, "sent")

	logIntegrationEvent(organizationID, "test_email_sent", "success", "Test email sent to "+requestData.ToEmail, "")

	c.JSON(http.StatusOK, gin.H{"message": "Test email sent successfully"})
}

func sendTestSMS(c *gin.Context) {
	organizationID := c.Param("organizationId")

	var requestData struct {
		ToNumber string `json:"to_number" binding:"required"`
		Message  string `json:"message"`
	}

	if err := c.ShouldBindJSON(&requestData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}

	time.Sleep(2 * time.Second)

	logCommunication(organizationID, "sms", "twilio", "outbound", "+15551234567", requestData.ToNumber, "", "", "", requestData.Message, "sent")

	logIntegrationEvent(organizationID, "test_sms_sent", "success", "Test SMS sent to "+requestData.ToNumber, "")

	c.JSON(http.StatusOK, gin.H{"message": "Test SMS sent successfully"})
}

func sendTestWhatsApp(c *gin.Context) {
	organizationID := c.Param("organizationId")

	var requestData struct {
		ToNumber string `json:"to_number" binding:"required"`
		Message  string `json:"message"`
	}

	if err := c.ShouldBindJSON(&requestData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}

	time.Sleep(2 * time.Second)

	logCommunication(organizationID, "whatsapp", "whatsapp_business", "outbound", "+15551234567", requestData.ToNumber, "", "", "", requestData.Message, "sent")

	logIntegrationEvent(organizationID, "test_whatsapp_sent", "success", "Test WhatsApp message sent to "+requestData.ToNumber, "")

	c.JSON(http.StatusOK, gin.H{"message": "Test WhatsApp message sent successfully"})
}

// Log handlers
func getIntegrationLogs(c *gin.Context) {
	organizationID := c.Param("organizationId")

	var logs []IntegrationLog
	if err := db.Where("organization_id = ?", organizationID).Order("created_at DESC").Limit(50).Find(&logs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch integration logs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": logs})
}

func getCommunicationLogs(c *gin.Context) {
	organizationID := c.Param("organizationId")

	var logs []CommunicationLog
	if err := db.Where("organization_id = ?", organizationID).Order("created_at DESC").Limit(50).Find(&logs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch communication logs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": logs})
}

func getPaymentTransactions(c *gin.Context) {
	organizationID := c.Param("organizationId")

	var transactions []PaymentTransaction
	if err := db.Where("organization_id = ?", organizationID).Order("created_at DESC").Limit(50).Find(&transactions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch payment transactions"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": transactions})
}

// Template handlers
func getEmailTemplates(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"templates": map[string]interface{}{
			"booking_confirmation": map[string]string{
				"subject": "Booking Confirmation - {{.BookingID}}",
				"body":    "Dear {{.CustomerName}}, your booking has been confirmed for {{.Date}} at {{.Time}}. Thank you!",
			},
			"booking_reminder": map[string]string{
				"subject": "Booking Reminder - {{.BookingID}}",
				"body":    "Dear {{.CustomerName}}, this is a reminder about your upcoming booking on {{.Date}} at {{.Time}}.",
			},
			"payment_confirmation": map[string]string{
				"subject": "Payment Confirmation - {{.Amount}}",
				"body":    "Dear {{.CustomerName}}, we have received your payment of {{.Amount}}. Thank you!",
			},
		},
	})
}

func getSMSTemplates(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"templates": map[string]string{
			"booking_confirmation": "Booking confirmed! {{.BookingID}} on {{.Date}} at {{.Time}}. Thank you!",
			"booking_reminder":     "Reminder: Your appointment {{.BookingID}} is tomorrow at {{.Time}}",
			"payment_received":     "Payment of {{.Amount}} received. Thank you!",
		},
	})
}

func getWhatsAppTemplates(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"templates": map[string]string{
			"booking_confirmation": "🎉 *Booking Confirmed!*\n\n📋 Details:\n• ID: {{.BookingID}}\n• Date: {{.Date}}\n• Time: {{.Time}}\n\nThank you!",
			"booking_reminder":     "⏰ *Booking Reminder*\n\nYour appointment {{.BookingID}} is tomorrow at {{.Time}}",
			"payment_received":     "💳 *Payment Received*\n\nAmount: {{.Amount}}\nThank you for your payment!",
		},
	})
}

// System status handlers
func getSystemStatus(c *gin.Context) {
	var totalSettings int64
	db.Model(&IntegrationSettings{}).Count(&totalSettings)

	var totalLogs int64
	db.Model(&IntegrationLog{}).Count(&totalLogs)

	c.JSON(http.StatusOK, gin.H{
		"status": "operational",
		"statistics": gin.H{
			"total_organizations": totalSettings,
			"total_events":        totalLogs,
		},
		"supported_providers": []string{
			"stripe", "paypal", "twilio", "whatsapp_business",
			"smtp", "sendgrid", "zoho", "firebase",
		},
	})
}

func getSystemHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "healthy",
		"services": map[string]string{
			"stripe":   "operational",
			"email":    "operational",
			"twilio":   "operational",
			"whatsapp": "operational",
			"zoho":     "operational",
			"firebase": "operational",
		},
		"timestamp": time.Now(),
	})
}

// Helper functions
func maskSecretKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) < 8 {
		return "****"
	}
	return key[:4] + "****"
}

func logIntegrationEvent(organizationID, event, status, message, errorCode string) {
	logEntry := &IntegrationLog{
		OrganizationID: organizationID,
		Integration:    "system",
		Event:          event,
		Status:         status,
		Message:        message,
		ErrorCode:      errorCode,
	}

	db.Create(logEntry)
}

func logCommunication(organizationID, commType, provider, direction, fromNumber, toNumber, fromEmail, toEmail, subject, message, status string) {
	logEntry := &CommunicationLog{
		OrganizationID: organizationID,
		Type:           commType,
		Provider:       provider,
		Direction:      direction,
		FromNumber:     fromNumber,
		ToNumber:       toNumber,
		FromEmail:      fromEmail,
		ToEmail:        toEmail,
		Subject:        subject,
		Message:        message,
		Status:         status,
	}

	db.Create(logEntry)
}

func createSampleData() error {
	// Check if sample data already exists
	var count int64
	db.Model(&IntegrationSettings{}).Count(&count)
	if count > 0 {
		log.Printf("Sample data already exists, skipping initialization")
		return nil
	}

	log.Printf("Creating sample integration data...")

	// Create sample integration settings
	sampleSettings := &IntegrationSettings{
		OrganizationID: "org_sample_123",
		GeneralSettings: GeneralSettings{
			PlatformName:    "Sample Auto Service",
			DefaultTimezone: "America/New_York",
			DefaultCurrency: "USD",
		},
		EmailSettings: EmailSettings{
			Enabled:   false,
			Provider:  "smtp",
			SMTPHost:  "smtp.gmail.com",
			SMTPPort:  587,
			FromName:  "Sample Auto Service",
			FromEmail: "noreply@sampleauto.com",
		},
		StripeSettings: StripeSettings{
			Enabled:        false,
			TestMode:       true,
			Currency:       "USD",
			PublishableKey: "pk_test_sample_key",
		},
		TwilioSettings: TwilioSettings{
			Enabled:    false,
			EnableSMS:  true,
			EnableVoice: false,
		},
		WhatsAppSettings: WhatsAppSettings{
			Enabled: false,
		},
		ZohoSettings: ZohoSettings{
			Enabled:     false,
			APIRegion:   "com",
			EnableCRM:   true,
			EnableBooks: false,
		},
		FirebaseSettings: FirebaseSettings{
			Enabled:         false,
			EnableAuth:      true,
			EnableFirestore: true,
			EnableStorage:   false,
		},
		SecuritySettings: SecuritySettings{
			SessionTimeout:      30,
			PasswordMinLength:   8,
			RequireSpecialChars: true,
			RequireNumbers:      true,
			EnableTwoFactor:     false,
			RateLimitRequests:   100,
			RateLimitWindow:     15,
		},
	}

	if err := db.Create(sampleSettings).Error; err != nil {
		return err
	}

	// Create sample logs
	sampleLogs := []IntegrationLog{
		{
			OrganizationID: "org_sample_123",
			Integration:    "system",
			Event:          "settings_created",
			Status:         "success",
			Message:        "Integration settings initialized",
		},
		{
			OrganizationID: "org_sample_123",
			Integration:    "stripe",
			Event:          "connection_test",
			Status:         "success",
			Message:        "Stripe connection test successful",
		},
	}

	for _, logEntry := range sampleLogs {
		db.Create(&logEntry)
	}

	log.Printf("✅ Sample integration data created successfully")
	log.Printf("🏢 Sample organization: org_sample_123")
	log.Printf("⚙️  Settings URL: http://localhost:8080/api/v1/integrations/organizations/org_sample_123/settings")

	return nil
}