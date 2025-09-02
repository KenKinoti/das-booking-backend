package main

import (
	"log"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/database"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/middleware"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found")
	}

	// Initialize configuration
	cfg := config.Load()

	// Initialize database
	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Run migrations
	if err := models.MigrateExtendedDB(db); err != nil {
		log.Fatal("Failed to migrate database:", err)
	}

	// Create indexes
	if err := models.CreateIndexes(db); err != nil {
		log.Fatal("Failed to create indexes:", err)
	}

	// Seed database (optional, only for development)
	if cfg.Environment == "development" {
		if err := models.SeedDatabase(db); err != nil {
			log.Println("Warning: Failed to seed database:", err)
		}
	}

	// Initialize Gin router
	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.Default()

	// Setup middleware
	router.Use(middleware.CORS())
	router.Use(middleware.Logger())
	router.Use(middleware.Recovery())

	// Initialize handlers
	h := handlers.NewHandler(db, cfg)

	// Setup routes
	h.SetupRoutes(router)

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server starting on port %s", port)
	log.Fatal(router.Run(":" + port))
}
