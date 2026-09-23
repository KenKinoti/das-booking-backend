package main

import (
	"log"
	"os"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	// Internal packages
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
)

func main() {
	// Initialize database
	db, err := gorm.Open(sqlite.Open("org_test.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Auto-migrate models
	if err := db.AutoMigrate(
		&models.Organization{},
		&models.User{},
		&models.Customer{},
		&models.Vehicle{},
		&models.Service{},
		&models.Booking{},
		&models.Participant{},
		&models.Shift{},
		&models.CarePlan{},
		&models.Document{},
		&models.EmergencyContact{},
	); err != nil {
		log.Fatal("Migration failed:", err)
	}

	// Initialize config
	cfg := &config.Config{
		JWTSecret: "development-secret-key",
	}

	// Initialize Gin router
	r := gin.Default()

	// Add CORS middleware
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// Health check endpoint
	r.GET("/api/v1/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "OK",
			"message": "Organization Test Server is running",
			"version": "1.0.0",
		})
	})

	// Register organization management routes manually
	internalHandler := handlers.NewHandler(db, cfg)

	// API v1 routes
	v1 := r.Group("/api/v1")

	// Super Admin routes for organization management
	superAdmin := v1.Group("/super-admin")
	{
		// Organization management
		organizations := superAdmin.Group("/organizations")
		{
			organizations.GET("", internalHandler.GetAllOrganizations)
			organizations.GET("/:id", internalHandler.GetOrganizationById)
			organizations.POST("", internalHandler.CreateOrganization)
			organizations.PATCH("/:id/status", internalHandler.UpdateOrganizationStatus)
			organizations.PATCH("/:id/modules", internalHandler.UpdateOrganizationModules)
			organizations.DELETE("/:id", internalHandler.DeleteOrganization)
		}
	}

	// Get port from environment or default
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Println("🚀 Organization Test Server starting on port", port)
	log.Println("🔗 Health: http://localhost:" + port + "/api/v1/health")
	log.Println("📊 Organization Management: http://localhost:" + port + "/api/v1/super-admin/organizations")

	// Start server
	if err := r.Run(":" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}