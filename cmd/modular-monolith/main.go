package main

import (
	"log"
	"os"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	// Internal packages - existing booking models
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"

	// Modular ERP packages with clear boundaries
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/inventory"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/crm"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/booking"
)

func main() {
	// Initialize database
	db, err := gorm.Open(sqlite.Open("das_booking.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Auto-migrate all models
	if err := autoMigrate(db); err != nil {
		log.Fatal("Migration failed:", err)
	}

	// Initialize services with clear module boundaries
	financeService := finance.NewService(db)
	inventoryService := inventory.NewService(db)
	crmService := crm.NewService(db)

	// Initialize handlers for each module
	financeHandler := finance.NewHandler(financeService)
	inventoryHandler := inventory.NewHandler(inventoryService)
	crmHandler := crm.NewHandler(crmService)
	bookingHandler := booking.NewHandler(db)

	// Initialize sample data
	initializeSampleData(db, financeService, inventoryService, crmService)

	// Setup Gin router
	r := gin.Default()

	// Add CORS middleware
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// API routes with clear module boundaries
	v1 := r.Group("/api/v1")
	{
		// Health check
		v1.GET("/health", func(c *gin.Context) {
			c.JSON(200, gin.H{
				"status":  "ok",
				"service": "DASYIN Modular Monolith ERP",
				"version": "2.0.0",
				"modules": []string{"Booking", "Finance", "CRM", "Inventory"},
			})
		})

		// Module route registration with clear boundaries
		financeHandler.RegisterRoutes(v1.Group("/finance"))
		inventoryHandler.RegisterRoutes(v1.Group("/inventory"))
		crmHandler.RegisterRoutes(v1.Group("/crm"))
		bookingHandler.RegisterRoutes(v1.Group("/booking"))

		// Admin routes
		v1.GET("/admin/overview", getAdminOverview)
		v1.GET("/admin/modules", getModuleStatus)
	}

	// Get port from environment or default
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🚀 DASYIN Modular Monolith starting on port %s", port)
	log.Printf("📊 Modules: Booking, Finance, CRM, Inventory")
	log.Printf("🔗 Health: http://localhost:%s/api/v1/health", port)

	if err := r.Run(":" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}

func autoMigrate(db *gorm.DB) error {
	// Migrate existing booking models
	if err := db.AutoMigrate(
		&models.Organization{},
		&models.User{},
		&models.Customer{},
		&models.Vehicle{},
		&models.Service{},
		&models.Booking{},
	); err != nil {
		return err
	}

	// Migrate ERP models
	if err := db.AutoMigrate(
		&finance.ChartOfAccount{},
		&finance.JournalEntry{},
		&finance.JournalEntryLine{},
		&inventory.Product{},
		&inventory.StockMovement{},
		&crm.Lead{},
		&crm.CRMCustomer{},
		&crm.Opportunity{},
	); err != nil {
		return err
	}

	return nil
}

func initializeSampleData(db *gorm.DB, financeService *finance.Service, inventoryService *inventory.Service, crmService *crm.Service) {
	orgID := "default-org"

	// Check if data already exists
	var count int64
	db.Model(&finance.ChartOfAccount{}).Where("organization_id = ?", orgID).Count(&count)
	if count == 0 {
		financeService.InitializeDefaultAccounts(orgID)
		log.Println("✅ Finance: Default chart of accounts initialized")
	}

	db.Model(&inventory.Product{}).Where("organization_id = ?", orgID).Count(&count)
	if count == 0 {
		inventoryService.InitializeSampleProducts(orgID)
		log.Println("✅ Inventory: Sample products initialized")
	}

	db.Model(&crm.Lead{}).Where("organization_id = ?", orgID).Count(&count)
	if count == 0 {
		crmService.InitializeSampleData(orgID)
		log.Println("✅ CRM: Sample leads initialized")
	}
}

func getAdminOverview(c *gin.Context) {
	c.JSON(200, gin.H{
		"success": true,
		"data": gin.H{
			"system_name":    "DASYIN Modular Monolith ERP",
			"version":        "2.0.0",
			"architecture":   "Modular Monolith",
			"modules_active": 4,
			"modules": []string{
				"Booking Management",
				"Finance Management",
				"CRM",
				"Inventory Management",
			},
		},
	})
}

func getModuleStatus(c *gin.Context) {
	c.JSON(200, gin.H{
		"success": true,
		"data": gin.H{
			"booking": gin.H{
				"name":     "Booking Management",
				"status":   "active",
				"features": []string{"appointment_scheduling", "customer_management", "service_management", "vehicle_tracking"},
			},
			"finance": gin.H{
				"name":     "Finance Management",
				"status":   "active",
				"features": []string{"chart_of_accounts", "double_entry_bookkeeping", "financial_reporting"},
			},
			"crm": gin.H{
				"name":     "CRM",
				"status":   "active",
				"features": []string{"lead_management", "customer_360", "opportunity_tracking", "sales_pipeline"},
			},
			"inventory": gin.H{
				"name":     "Inventory Management",
				"status":   "active",
				"features": []string{"product_catalog", "stock_tracking", "reorder_alerts"},
			},
		},
	})
}