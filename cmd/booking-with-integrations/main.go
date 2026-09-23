package main

import (
	"log"
	"os"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/database"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/middleware"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/routes"
)

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found: %v", err)
	}

	// Get database URL from environment
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "sqlite://das_booking_with_integrations.db"
		log.Printf("Using default SQLite database: %s", databaseURL)
	}

	// Initialize database
	db, err := database.Initialize(databaseURL)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Run migrations
	if err := database.MigrateAll(db); err != nil {
		log.Fatal("Migration failed:", err)
	}

	// Seed default integration settings for existing organizations
	if err := database.SeedIntegrationDefaults(db); err != nil {
		log.Printf("Warning: Failed to seed integration defaults: %v", err)
	}

	// Initialize configuration
	cfg := &config.Config{
		JWTSecret:    os.Getenv("JWT_SECRET"),
		DatabaseURL:  databaseURL,
		Environment:  os.Getenv("ENVIRONMENT"),
	}

	if cfg.JWTSecret == "" {
		cfg.JWTSecret = "development-booking-platform-secret-key"
		log.Printf("Using default JWT secret for development")
	}

	if cfg.Environment == "" {
		cfg.Environment = "development"
	}

	// Initialize sample data
	if err := initializeSampleData(db); err != nil {
		log.Printf("Warning: Failed to initialize sample data: %v", err)
	}

	// Setup Gin router
	gin.SetMode(gin.ReleaseMode)
	if cfg.Environment == "development" {
		gin.SetMode(gin.DebugMode)
	}

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
				"service":     "DAS Booking Platform with Integrations",
				"version":     "1.0.0",
				"environment": cfg.Environment,
				"integrations": []string{
					"Stripe Payments",
					"PayPal Payments",
					"Twilio SMS & Voice",
					"WhatsApp Business API",
					"SMTP Email",
					"SendGrid Email",
					"Zoho Suite",
				},
				"features": []string{
					"Customer Management",
					"Booking Management",
					"Staff Management",
					"Service Catalog",
					"Payment Processing",
					"Communication Automation",
					"Email Notifications",
					"SMS Notifications",
					"WhatsApp Messaging",
					"Integration Logging",
					"Security & Authentication",
				},
			})
		})

		// Initialize core handlers
		authHandler := handlers.NewAuthHandler(db, cfg.JWTSecret)
		userHandler := handlers.NewUserHandler(db)
		orgHandler := handlers.NewOrganizationHandler(db)
		customerHandler := handlers.NewCustomerHandler(db)
		bookingHandler := handlers.NewBookingHandler(db)
		serviceHandler := handlers.NewServiceHandler(db)

		// Authentication routes (no auth required)
		auth := v1.Group("/auth")
		{
			auth.POST("/login", authHandler.Login)
			auth.POST("/register", authHandler.Register)
			auth.POST("/refresh", authHandler.RefreshToken)
		}

		// Protected routes
		protected := v1.Group("/")
		protected.Use(middleware.AuthMiddleware(cfg.JWTSecret))
		{
			// User management
			users := protected.Group("/users")
			{
				users.GET("/profile", userHandler.GetProfile)
				users.PUT("/profile", userHandler.UpdateProfile)
				users.POST("/change-password", userHandler.ChangePassword)
			}

			// Organization management
			organizations := protected.Group("/organizations")
			{
				organizations.GET("/", orgHandler.GetOrganizations)
				organizations.POST("/", orgHandler.CreateOrganization)
				organizations.GET("/:id", orgHandler.GetOrganization)
				organizations.PUT("/:id", orgHandler.UpdateOrganization)
				organizations.DELETE("/:id", orgHandler.DeleteOrganization)
			}

			// Customer management
			customers := protected.Group("/customers")
			{
				customers.GET("/", customerHandler.GetCustomers)
				customers.POST("/", customerHandler.CreateCustomer)
				customers.GET("/:id", customerHandler.GetCustomer)
				customers.PUT("/:id", customerHandler.UpdateCustomer)
				customers.DELETE("/:id", customerHandler.DeleteCustomer)
			}

			// Booking management
			bookings := protected.Group("/bookings")
			{
				bookings.GET("/", bookingHandler.GetBookings)
				bookings.POST("/", bookingHandler.CreateBooking)
				bookings.GET("/:id", bookingHandler.GetBooking)
				bookings.PUT("/:id", bookingHandler.UpdateBooking)
				bookings.DELETE("/:id", bookingHandler.DeleteBooking)
				bookings.POST("/:id/confirm", bookingHandler.ConfirmBooking)
				bookings.POST("/:id/cancel", bookingHandler.CancelBooking)
				bookings.POST("/:id/reschedule", bookingHandler.RescheduleBooking)
			}

			// Service management
			services := protected.Group("/services")
			{
				services.GET("/", serviceHandler.GetServices)
				services.POST("/", serviceHandler.CreateService)
				services.GET("/:id", serviceHandler.GetService)
				services.PUT("/:id", serviceHandler.UpdateService)
				services.DELETE("/:id", serviceHandler.DeleteService)
			}

			// Integration routes - this is the new functionality
			routes.SetupIntegrationRoutes(protected, db)
			routes.SetupSystemRoutes(protected, db)
		}

		// Public webhook endpoints (no auth required)
		routes.SetupIntegrationRoutes(v1, db) // This will also add webhook routes
	}

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🚀 DAS Booking Platform with Integrations starting on port %s", port)
	log.Printf("📊 Environment: %s", cfg.Environment)
	log.Printf("💾 Database: %s", databaseURL)
	log.Printf("🔗 Available integrations: Stripe, PayPal, Twilio, WhatsApp, Email, Zoho")
	log.Printf("📱 API Documentation: http://localhost:%s/api/v1/health", port)

	if err := r.Run(":" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}

// initializeSampleData creates sample data for testing
func initializeSampleData(db *gorm.DB) error {
	// Check if sample data already exists
	var count int64
	db.Model(&models.Organization{}).Count(&count)
	if count > 0 {
		log.Printf("Sample data already exists, skipping initialization")
		return nil
	}

	log.Printf("Initializing sample data...")

	// Create sample organization
	org := &models.Organization{
		ID:           "org_sample_123",
		Name:         "Sample Auto Service",
		BusinessType: "automotive",
		ABN:          "12345678901",
		Phone:        "+1-555-0123",
		Email:        "info@sampleauto.com",
		Website:      "https://sampleauto.com",
		Description:  "Full-service automotive repair and maintenance",
		Address: models.Address{
			Street:     "123 Main Street",
			City:       "Sample City",
			State:      "CA",
			PostalCode: "90210",
			Country:    "USA",
		},
		BusinessHours: models.BusinessHours{
			Monday:    "08:00-18:00",
			Tuesday:   "08:00-18:00",
			Wednesday: "08:00-18:00",
			Thursday:  "08:00-18:00",
			Friday:    "08:00-18:00",
			Saturday:  "09:00-15:00",
			Sunday:    "closed",
		},
		BookingSettings: models.BookingSettings{
			RequireApproval:      false,
			AllowOnlineBooking:   true,
			BookingBuffer:        30,
			MaxAdvanceBookingDays: 90,
		},
	}

	if err := db.Create(org).Error; err != nil {
		return err
	}

	// Create sample admin user
	adminUser := &models.User{
		ID:             "user_admin_123",
		Email:          "admin@sampleauto.com",
		FirstName:      "Admin",
		LastName:       "User",
		Phone:          "+1-555-0124",
		Role:           "admin",
		OrganizationID: org.ID,
		IsActive:       true,
	}

	// Hash password (simplified for demo)
	adminUser.PasswordHash = "$2a$10$sample.hashed.password.for.demo.purposes.only"

	if err := db.Create(adminUser).Error; err != nil {
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
		Address: models.Address{
			Street:     "456 Oak Avenue",
			City:       "Sample City",
			State:      "CA",
			PostalCode: "90211",
			Country:    "USA",
		},
		CustomerType:   "individual",
		Status:         "active",
	}

	if err := db.Create(customer).Error; err != nil {
		return err
	}

	// Create sample services
	services := []models.Service{
		{
			ID:             "service_oil_change",
			OrganizationID: org.ID,
			Name:           "Oil Change",
			Description:    "Complete oil change with filter replacement",
			Price:          59.99,
			Duration:       30,
			Category:       "maintenance",
			IsActive:       true,
		},
		{
			ID:             "service_brake_repair",
			OrganizationID: org.ID,
			Name:           "Brake Repair",
			Description:    "Brake pad and rotor replacement",
			Price:          299.99,
			Duration:       120,
			Category:       "repair",
			IsActive:       true,
		},
	}

	for _, service := range services {
		if err := db.Create(&service).Error; err != nil {
			return err
		}
	}

	// Create sample booking
	booking := &models.Booking{
		ID:              "booking_sample_123",
		OrganizationID:  org.ID,
		CustomerID:      customer.ID,
		ServiceID:       services[0].ID,
		Status:          "confirmed",
		BookingType:     "appointment",
		AppointmentDate: models.JSONTime{Time: models.ParseTime("2024-12-20T10:00:00Z")},
		Duration:        30,
		TotalAmount:     59.99,
		Notes:           "Sample booking for oil change",
	}

	if err := db.Create(booking).Error; err != nil {
		return err
	}

	log.Printf("✅ Sample data initialized successfully")
	log.Printf("📧 Admin login: admin@sampleauto.com (password: demo123)")
	log.Printf("🏢 Sample organization: %s", org.Name)
	log.Printf("👤 Sample customer: %s %s", customer.FirstName, customer.LastName)
	log.Printf("📅 Sample booking: %s", booking.ID)

	return nil
}