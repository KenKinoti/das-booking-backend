package main

import (
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found: %v", err)
	}

	// Initialize SQLite database for testing
	db, err := gorm.Open(sqlite.Open("test_integrations.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Migrate only the required models for testing
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

	// Seed will be handled manually in createSampleData

	// Create sample data for testing
	if err := createSampleData(db); err != nil {
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
		v1.GET("/health", func(c *gin.Context) {
			c.JSON(200, gin.H{
				"status":      "ok",
				"service":     "DAS Booking Integrations Test Server",
				"version":     "1.0.0",
				"environment": "test",
				"integrations": []string{
					"Stripe Payments",
					"Twilio SMS & Voice",
					"WhatsApp Business API",
					"SMTP Email",
				},
			})
		})

		// Initialize simple integration handler for testing
		integrationHandler := handlers.NewSimpleIntegrationHandler(db)

		// Integration routes
		integrations := v1.Group("/integrations")
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

			// Templates
			integrations.GET("/templates/email", func(c *gin.Context) {
				c.JSON(200, gin.H{
					"templates": map[string]interface{}{
						"booking_confirmation": map[string]string{
							"subject": "Booking Confirmation - {{.BookingID}}",
							"body":    "Dear {{.CustomerName}}, your booking has been confirmed...",
						},
					},
				})
			})

			integrations.GET("/templates/sms", func(c *gin.Context) {
				c.JSON(200, gin.H{
					"templates": map[string]string{
						"booking_confirmation": "Booking Confirmed! ID: {{.BookingID}}",
					},
				})
			})
		}

		// System status
		v1.GET("/system/integrations/status", func(c *gin.Context) {
			c.JSON(200, gin.H{
				"status":               "operational",
				"supported_providers": []string{"stripe", "twilio", "whatsapp_business", "smtp"},
			})
		})

		v1.GET("/system/integrations/health", func(c *gin.Context) {
			c.JSON(200, gin.H{
				"status": "healthy",
				"services": map[string]string{
					"stripe":   "operational",
					"email":    "operational",
					"twilio":   "operational",
					"whatsapp": "operational",
				},
			})
		})
	}

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🚀 DAS Booking Integrations Test Server starting on port %s", port)
	log.Printf("📊 Environment: test")
	log.Printf("💾 Database: SQLite (test_integrations.db)")
	log.Printf("🔗 Available integrations: Stripe, Twilio, WhatsApp, Email")
	log.Printf("📱 Test API: http://localhost:%s/api/v1/health", port)

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
		Name:         "Sample Auto Service",
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
		CustomerType:   "individual",
		Status:         "active",
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
		BasePrice:      59.99,
		Duration:       30,
		Category:       "maintenance",
		IsActive:       true,
	}

	if err := db.Create(service).Error; err != nil {
		return err
	}

	log.Printf("✅ Sample data created successfully")
	log.Printf("🏢 Sample organization: %s", org.Name)
	log.Printf("👤 Sample customer: %s %s", customer.FirstName, customer.LastName)
	log.Printf("🔧 Sample service: %s", service.Name)

	return nil
}