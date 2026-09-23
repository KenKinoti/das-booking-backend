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
	db, err := gorm.Open(sqlite.Open("simple_integrations.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Migrate only the required models for testing (without complex service dependencies)
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
				"service":     "DAS Booking Simple Integration Test Server",
				"version":     "1.0.0",
				"environment": "test",
				"integrations": []string{
					"Stripe Payments (simulated)",
					"Twilio SMS & Voice (simulated)",
					"WhatsApp Business API (simulated)",
					"SMTP Email (simulated)",
					"Firebase Auth/Firestore/Storage (simulated)",
					"PayPal Payments (simulated)",
					"Zoho Suite (simulated)",
				},
			})
		})

		// Initialize simple integration handler for testing (without problematic service dependencies)
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

				// Connection testing (all simulated for demo)
				orgGroup.POST("/test/stripe", integrationHandler.TestStripeConnection)
				orgGroup.POST("/test/email", integrationHandler.TestEmailConnection)
				orgGroup.POST("/test/twilio", integrationHandler.TestTwilioConnection)
				orgGroup.POST("/test/whatsapp", integrationHandler.TestWhatsAppConnection)

				// Additional integrations for testing
				orgGroup.POST("/test/firebase", func(c *gin.Context) {
					organizationID := c.Param("organizationId")
					c.JSON(http.StatusOK, gin.H{
						"message":   "Firebase connection test successful (simulated)",
						"verified":  true,
						"tested_at": "2023-01-01T12:00:00Z",
						"services":  []string{"Auth", "Firestore", "Storage"},
						"org_id":    organizationID,
					})
				})

				orgGroup.POST("/test/paypal", func(c *gin.Context) {
					organizationID := c.Param("organizationId")
					c.JSON(http.StatusOK, gin.H{
						"message":   "PayPal connection test successful (simulated)",
						"verified":  true,
						"tested_at": "2023-01-01T12:00:00Z",
						"mode":      "sandbox",
						"org_id":    organizationID,
					})
				})

				orgGroup.POST("/test/zoho", func(c *gin.Context) {
					organizationID := c.Param("organizationId")
					c.JSON(http.StatusOK, gin.H{
						"message":   "Zoho connection test successful (simulated)",
						"verified":  true,
						"tested_at": "2023-01-01T12:00:00Z",
						"services":  []string{"CRM", "Books", "Mail"},
						"org_id":    organizationID,
					})
				})

				// Sending test messages
				orgGroup.POST("/send/test-email", integrationHandler.SendTestEmail)
				orgGroup.POST("/send/test-sms", integrationHandler.SendTestSMS)
				orgGroup.POST("/send/test-whatsapp", integrationHandler.SendTestWhatsApp)

				// Logs and monitoring
				orgGroup.GET("/logs/integrations", integrationHandler.GetIntegrationLogs)
				orgGroup.GET("/logs/communications", integrationHandler.GetCommunicationLogs)
				orgGroup.GET("/transactions/payments", integrationHandler.GetPaymentTransactions)
			}

			// Integration status endpoints
			integrations.GET("/status", func(c *gin.Context) {
				c.JSON(200, gin.H{
					"status": "operational",
					"integrations": gin.H{
						"stripe":   gin.H{"status": "operational", "type": "payment"},
						"paypal":   gin.H{"status": "operational", "type": "payment"},
						"twilio":   gin.H{"status": "operational", "type": "communication"},
						"whatsapp": gin.H{"status": "operational", "type": "communication"},
						"email":    gin.H{"status": "operational", "type": "communication"},
						"firebase": gin.H{"status": "operational", "type": "backend_service"},
						"zoho":     gin.H{"status": "operational", "type": "crm_suite"},
					},
				})
			})
		}
	}

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	log.Printf("🚀 DAS Booking Simple Integration Test Server starting on port %s", port)
	log.Printf("📊 Environment: test")
	log.Printf("💾 Database: SQLite (simple_integrations.db)")
	log.Printf("🔗 Available integrations: Stripe, PayPal, Twilio, WhatsApp, Email, Firebase, Zoho")
	log.Printf("📱 Test API: http://localhost:%s/api/v1/health", port)
	log.Printf("⚙️  Integration Settings: http://localhost:%s/api/v1/integrations/organizations/org_sample_123/settings", port)

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
	log.Printf("🏢 Sample organization: %s (%s)", org.Name, org.ID)
	log.Printf("👤 Sample customer: %s %s (%s)", customer.FirstName, customer.LastName, customer.ID)
	log.Printf("🔧 Sample service: %s (%s)", service.Name, service.ID)

	return nil
}