package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/database"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/middleware"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

// ============= MODELS =============

// ERP Models - Consolidated for optimal performance
type ChartOfAccount struct {
	ID             string  `json:"id" gorm:"primaryKey"`
	OrganizationID string  `json:"organization_id"`
	Code           string  `json:"code"`
	Name           string  `json:"name"`
	AccountType    string  `json:"account_type"`
	SubType        string  `json:"sub_type"`
	DebitBalance   float64 `json:"debit_balance"`
	CreditBalance  float64 `json:"credit_balance"`
	IsActive       bool    `json:"is_active"`
}

type CRMLead struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id"`
	FirstName      string    `json:"first_name"`
	LastName       string    `json:"last_name"`
	Email          string    `json:"email"`
	Phone          string    `json:"phone"`
	CompanyName    string    `json:"company_name"`
	Status         string    `json:"status"`
	Source         string    `json:"source"`
	EstimatedValue float64   `json:"estimated_value"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type CRMCustomer struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id"`
	CustomerNumber string    `json:"customer_number"`
	Type           string    `json:"type"`
	Status         string    `json:"status"`
	FirstName      string    `json:"first_name"`
	LastName       string    `json:"last_name"`
	CompanyName    string    `json:"company_name"`
	DisplayName    string    `json:"display_name"`
	Email          string    `json:"email"`
	Phone          string    `json:"phone"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type InventoryProduct struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id"`
	SKU            string    `json:"sku"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Type           string    `json:"type"`
	UnitOfMeasure  string    `json:"unit_of_measure"`
	CostPrice      float64   `json:"cost_price"`
	SellingPrice   float64   `json:"selling_price"`
	CurrentStock   float64   `json:"current_stock"`
	ReorderPoint   float64   `json:"reorder_point"`
	IsActive       bool      `json:"is_active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Opportunity struct {
	ID             string    `json:"id" gorm:"primaryKey"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	CustomerID     string    `json:"customer_id"`
	Stage          string    `json:"stage"`
	Amount         float64   `json:"amount"`
	Probability    int       `json:"probability"`
	CloseDate      time.Time `json:"close_date"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Table name functions
func (ChartOfAccount) TableName() string   { return "chart_of_accounts" }
func (CRMLead) TableName() string          { return "crm_leads" }
func (CRMCustomer) TableName() string      { return "crm_customers" }
func (InventoryProduct) TableName() string { return "inventory_products" }

// ============= UTILITY FUNCTIONS =============

func getOrganizationID(db *gorm.DB) string {
	var orgID string
	err := db.Raw("SELECT id FROM organizations LIMIT 1").Scan(&orgID).Error
	if err != nil {
		return "1"
	}
	return orgID
}

func generateID() string {
	return "id-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func successResponse(data interface{}, message ...string) gin.H {
	response := gin.H{"success": true, "data": data}
	if len(message) > 0 {
		response["message"] = message[0]
	}
	if slice, ok := data.([]interface{}); ok {
		response["count"] = len(slice)
	}
	return response
}

func errorResponse(err string, details ...string) gin.H {
	response := gin.H{"success": false, "error": err}
	if len(details) > 0 {
		response["details"] = details[0]
	}
	return response
}

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

	// Initialize Gin router
	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.Default()

	// Setup middleware
	router.Use(middleware.CORS())
	router.Use(middleware.Logger())
	router.Use(middleware.Recovery())

	// Setup API routes
	api := router.Group("/api/v1")

	// Health check endpoint
	api.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "ok",
			"service": "DASYIN Unified ERP & Booking System",
			"version": "2.0.0",
			"modules": []string{"Booking", "Finance", "CRM", "Inventory"},
		})
	})

	// Setup module routes
	setupBookingRoutes(api, db)
	setupFinanceRoutes(api, db)
	setupCRMRoutes(api, db)
	setupInventoryRoutes(api, db)
	setupAdminRoutes(api, db)

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🚀 DASYIN Unified System starting on port %s", port)
	log.Printf("📊 Modules: Booking, Finance, CRM, Inventory")
	log.Printf("🔗 Health: http://localhost:%s/api/v1/health", port)
	log.Fatal(router.Run(":" + port))
}

// ============= BOOKING MODULE =============

func setupBookingRoutes(api *gin.RouterGroup, db *gorm.DB) {
	booking := api.Group("/booking")
	{
		// Core booking operations
		booking.GET("/bookings", getBookings(db))
		booking.GET("/bookings/:id", getBooking(db))
		booking.POST("/bookings", createBooking(db))
		booking.PUT("/bookings/:id", updateBooking(db))
		booking.DELETE("/bookings/:id", deleteBooking(db))
		booking.PUT("/bookings/:id/status", updateBookingStatus(db))
		booking.GET("/available-slots", getAvailableTimeSlots(db))
		booking.GET("/dashboard", getBookingDashboard(db))

		// Customer management
		booking.GET("/customers", getBookingCustomers(db))
		booking.POST("/customers", createBookingCustomer(db))
		booking.GET("/customers/:id", getBookingCustomer(db))
		booking.PUT("/customers/:id", updateBookingCustomer(db))

		// Service management
		booking.GET("/services", getBookingServices(db))
		booking.POST("/services", createBookingService(db))
		booking.GET("/services/:id", getBookingService(db))
		booking.PUT("/services/:id", updateBookingService(db))

		// Vehicle management
		booking.GET("/vehicles", getBookingVehicles(db))
		booking.POST("/vehicles", createBookingVehicle(db))
		booking.GET("/customer/:customer_id/vehicles", getCustomerVehicles(db))
	}
}

func getBookings(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var bookings []models.Booking

		query := db.Where("organization_id = ?", orgID).Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services")

		if status := c.Query("status"); status != "" {
			query = query.Where("status = ?", status)
		}
		if customerID := c.Query("customer_id"); customerID != "" {
			query = query.Where("customer_id = ?", customerID)
		}
		if dateFrom := c.Query("date_from"); dateFrom != "" {
			if parsedDate, err := time.Parse("2006-01-02", dateFrom); err == nil {
				query = query.Where("start_time >= ?", parsedDate)
			}
		}
		if dateTo := c.Query("date_to"); dateTo != "" {
			if parsedDate, err := time.Parse("2006-01-02", dateTo); err == nil {
				query = query.Where("start_time <= ?", parsedDate.Add(24*time.Hour))
			}
		}

		if err := query.Order("start_time ASC").Find(&bookings).Error; err != nil {
			c.JSON(http.StatusInternalServerError, errorResponse("Failed to fetch bookings", err.Error()))
			return
		}

		c.JSON(200, successResponse(bookings))
	}
}

func getBooking(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")

		var booking models.Booking
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).
			Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services").
			First(&booking).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Booking not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch booking"))
			return
		}

		c.JSON(200, successResponse(booking))
	}
}

func createBooking(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)

		var request struct {
			CustomerID    string    `json:"customer_id" binding:"required"`
			VehicleID     *string   `json:"vehicle_id"`
			StaffID       *string   `json:"staff_id"`
			StartTime     time.Time `json:"start_time" binding:"required"`
			EndTime       time.Time `json:"end_time" binding:"required"`
			ServiceIDs    []string  `json:"service_ids" binding:"required"`
			Notes         string    `json:"notes"`
			InternalNotes string    `json:"internal_notes"`
		}

		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}

		// Validate customer exists
		var customer models.Customer
		if err := db.Where("id = ? AND organization_id = ?", request.CustomerID, orgID).First(&customer).Error; err != nil {
			c.JSON(400, errorResponse("Invalid customer ID"))
			return
		}

		// Validate services exist
		var services []models.Service
		if err := db.Where("id IN ? AND organization_id = ?", request.ServiceIDs, orgID).Find(&services).Error; err != nil {
			c.JSON(400, errorResponse("Invalid service IDs"))
			return
		}

		if len(services) != len(request.ServiceIDs) {
			c.JSON(400, errorResponse("Some service IDs are invalid"))
			return
		}

		// Calculate total price
		var totalPrice float64
		for _, service := range services {
			totalPrice += service.Price
		}

		// Create booking
		booking := models.Booking{
			CustomerID:     request.CustomerID,
			OrganizationID: orgID,
			VehicleID:      request.VehicleID,
			StaffID:        request.StaffID,
			StartTime:      request.StartTime,
			EndTime:        request.EndTime,
			Status:         "scheduled",
			TotalPrice:     totalPrice,
			Notes:          request.Notes,
			InternalNotes:  request.InternalNotes,
		}

		tx := db.Begin()
		defer func() {
			if r := recover(); r != nil {
				tx.Rollback()
			}
		}()

		if err := tx.Create(&booking).Error; err != nil {
			tx.Rollback()
			c.JSON(500, errorResponse("Failed to create booking", err.Error()))
			return
		}

		if err := tx.Model(&booking).Association("Services").Append(&services); err != nil {
			tx.Rollback()
			c.JSON(500, errorResponse("Failed to associate services"))
			return
		}

		tx.Commit()

		db.Where("id = ?", booking.ID).Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services").First(&booking)
		c.JSON(201, successResponse(booking, "Booking created successfully"))
	}
}

func updateBooking(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")

		var booking models.Booking
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&booking).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Booking not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch booking"))
			return
		}

		var request struct {
			VehicleID     *string   `json:"vehicle_id"`
			StaffID       *string   `json:"staff_id"`
			StartTime     time.Time `json:"start_time"`
			EndTime       time.Time `json:"end_time"`
			Status        string    `json:"status"`
			ServiceIDs    []string  `json:"service_ids"`
			Notes         string    `json:"notes"`
			InternalNotes string    `json:"internal_notes"`
		}

		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}

		updates := make(map[string]interface{})
		if request.VehicleID != nil {
			updates["vehicle_id"] = *request.VehicleID
		}
		if request.StaffID != nil {
			updates["staff_id"] = *request.StaffID
		}
		if !request.StartTime.IsZero() {
			updates["start_time"] = request.StartTime
		}
		if !request.EndTime.IsZero() {
			updates["end_time"] = request.EndTime
		}
		if request.Status != "" {
			updates["status"] = request.Status
		}
		updates["notes"] = request.Notes
		updates["internal_notes"] = request.InternalNotes

		if err := db.Model(&booking).Updates(updates).Error; err != nil {
			c.JSON(500, errorResponse("Failed to update booking"))
			return
		}

		db.Where("id = ?", booking.ID).Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services").First(&booking)
		c.JSON(200, successResponse(booking, "Booking updated successfully"))
	}
}

func deleteBooking(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")

		var booking models.Booking
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&booking).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Booking not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch booking"))
			return
		}

		if err := db.Delete(&booking).Error; err != nil {
			c.JSON(500, errorResponse("Failed to delete booking"))
			return
		}

		c.JSON(200, successResponse(nil, "Booking deleted successfully"))
	}
}

func updateBookingStatus(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")

		var request struct {
			Status string `json:"status" binding:"required"`
		}

		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}

		validStatuses := []string{"scheduled", "confirmed", "in_progress", "completed", "cancelled", "no_show"}
		valid := false
		for _, status := range validStatuses {
			if request.Status == status {
				valid = true
				break
			}
		}

		if !valid {
			c.JSON(400, errorResponse("Invalid status"))
			return
		}

		var booking models.Booking
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&booking).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Booking not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch booking"))
			return
		}

		if err := db.Model(&booking).Update("status", request.Status).Error; err != nil {
			c.JSON(500, errorResponse("Failed to update status"))
			return
		}

		db.Where("id = ?", booking.ID).Preload("Customer").Preload("Vehicle").Preload("Staff").Preload("Services").First(&booking)
		c.JSON(200, successResponse(booking, "Status updated successfully"))
	}
}

func getAvailableTimeSlots(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		date := c.Query("date")
		if date == "" {
			c.JSON(400, errorResponse("Date parameter is required"))
			return
		}

		_, err := time.Parse("2006-01-02", date)
		if err != nil {
			c.JSON(400, errorResponse("Invalid date format. Use YYYY-MM-DD"))
			return
		}

		// Mock available slots - in real implementation, check business hours and existing bookings
		availableSlots := []string{"09:00", "10:00", "11:00", "14:00", "15:00", "16:00"}

		c.JSON(200, gin.H{
			"success":         true,
			"date":           date,
			"available_slots": availableSlots,
		})
	}
}

func getBookingDashboard(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)

		var totalBookings, todaysBookings, upcomingBookings, completedBookings int64
		var totalRevenue float64

		db.Model(&models.Booking{}).Where("organization_id = ?", orgID).Count(&totalBookings)
		db.Model(&models.Booking{}).Where("organization_id = ? AND DATE(start_time) = CURRENT_DATE", orgID).Count(&todaysBookings)
		db.Model(&models.Booking{}).Where("organization_id = ? AND start_time > NOW() AND status IN ?", orgID, []string{"scheduled", "confirmed"}).Count(&upcomingBookings)
		db.Model(&models.Booking{}).Where("organization_id = ? AND status = ?", orgID, "completed").Count(&completedBookings)
		db.Model(&models.Booking{}).Where("organization_id = ? AND status = ?", orgID, "completed").Select("COALESCE(SUM(total_price), 0)").Scan(&totalRevenue)

		c.JSON(200, successResponse(gin.H{
			"total_bookings":    totalBookings,
			"todays_bookings":   todaysBookings,
			"upcoming_bookings": upcomingBookings,
			"completed_bookings": completedBookings,
			"total_revenue":     totalRevenue,
		}))
	}
}

// Booking customer handlers
func getBookingCustomers(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var customers []models.Customer
		if err := db.Where("organization_id = ? AND is_active = ?", orgID, true).Find(&customers).Error; err != nil {
			c.JSON(500, errorResponse("Failed to fetch customers"))
			return
		}
		c.JSON(200, successResponse(customers))
	}
}

func createBookingCustomer(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var customer models.Customer
		if err := c.ShouldBindJSON(&customer); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		customer.OrganizationID = orgID
		customer.IsActive = true
		if err := db.Create(&customer).Error; err != nil {
			c.JSON(500, errorResponse("Failed to create customer"))
			return
		}
		c.JSON(201, successResponse(customer, "Customer created successfully"))
	}
}

func getBookingCustomer(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")
		var customer models.Customer
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).Preload("Vehicles").First(&customer).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Customer not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch customer"))
			return
		}
		c.JSON(200, successResponse(customer))
	}
}

func updateBookingCustomer(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")
		var customer models.Customer
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&customer).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Customer not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch customer"))
			return
		}
		var updates models.Customer
		if err := c.ShouldBindJSON(&updates); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		if err := db.Model(&customer).Updates(updates).Error; err != nil {
			c.JSON(500, errorResponse("Failed to update customer"))
			return
		}
		c.JSON(200, successResponse(customer, "Customer updated successfully"))
	}
}

// Booking service handlers
func getBookingServices(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var services []models.Service
		if err := db.Where("organization_id = ? AND is_active = ?", orgID, true).Find(&services).Error; err != nil {
			c.JSON(500, errorResponse("Failed to fetch services"))
			return
		}
		c.JSON(200, successResponse(services))
	}
}

func createBookingService(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var service models.Service
		if err := c.ShouldBindJSON(&service); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		service.OrganizationID = orgID
		service.IsActive = true
		if err := db.Create(&service).Error; err != nil {
			c.JSON(500, errorResponse("Failed to create service"))
			return
		}
		c.JSON(201, successResponse(service, "Service created successfully"))
	}
}

func getBookingService(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")
		var service models.Service
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&service).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Service not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch service"))
			return
		}
		c.JSON(200, successResponse(service))
	}
}

func updateBookingService(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")
		var service models.Service
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&service).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Service not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch service"))
			return
		}
		var updates models.Service
		if err := c.ShouldBindJSON(&updates); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		if err := db.Model(&service).Updates(updates).Error; err != nil {
			c.JSON(500, errorResponse("Failed to update service"))
			return
		}
		c.JSON(200, successResponse(service, "Service updated successfully"))
	}
}

// Vehicle handlers
func getBookingVehicles(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var vehicles []models.Vehicle
		if err := db.Where("organization_id = ? AND is_active = ?", orgID, true).Preload("Customer").Find(&vehicles).Error; err != nil {
			c.JSON(500, errorResponse("Failed to fetch vehicles"))
			return
		}
		c.JSON(200, successResponse(vehicles))
	}
}

func createBookingVehicle(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var vehicle models.Vehicle
		if err := c.ShouldBindJSON(&vehicle); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		vehicle.OrganizationID = orgID
		vehicle.IsActive = true
		if err := db.Create(&vehicle).Error; err != nil {
			c.JSON(500, errorResponse("Failed to create vehicle"))
			return
		}
		c.JSON(201, successResponse(vehicle, "Vehicle created successfully"))
	}
}

func getCustomerVehicles(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		customerID := c.Param("customer_id")
		var vehicles []models.Vehicle
		if err := db.Where("customer_id = ? AND organization_id = ? AND is_active = ?", customerID, orgID, true).Find(&vehicles).Error; err != nil {
			c.JSON(500, errorResponse("Failed to fetch vehicles"))
			return
		}
		c.JSON(200, successResponse(vehicles))
	}
}

// ============= FINANCE MODULE =============

func setupFinanceRoutes(api *gin.RouterGroup, db *gorm.DB) {
	finance := api.Group("/finance")
	{
		finance.GET("/chart-of-accounts", getChartOfAccounts(db))
		finance.POST("/chart-of-accounts", createChartOfAccount(db))
		finance.GET("/dashboard", getFinanceDashboard(db))
	}
}

func getChartOfAccounts(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var accounts []ChartOfAccount
		if err := db.Where("is_active = ?", true).Find(&accounts).Error; err != nil {
			c.JSON(500, errorResponse("Failed to fetch accounts", err.Error()))
			return
		}
		c.JSON(200, successResponse(accounts))
	}
}

func createChartOfAccount(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var account ChartOfAccount
		if err := c.ShouldBindJSON(&account); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		account.OrganizationID = orgID
		account.IsActive = true
		if account.ID == "" {
			account.ID = generateID()
		}
		if err := db.Create(&account).Error; err != nil {
			c.JSON(500, errorResponse("Failed to create account", err.Error()))
			return
		}
		c.JSON(201, successResponse(account, "Account created successfully"))
	}
}

func getFinanceDashboard(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var accountCount int64
		var totalAssets, totalLiabilities, totalRevenue, totalExpenses float64

		db.Model(&ChartOfAccount{}).Where("is_active = ?", true).Count(&accountCount)
		db.Model(&ChartOfAccount{}).Where("account_type = ? AND is_active = ?", "asset", true).Select("COALESCE(SUM(debit_balance - credit_balance), 0)").Scan(&totalAssets)
		db.Model(&ChartOfAccount{}).Where("account_type = ? AND is_active = ?", "liability", true).Select("COALESCE(SUM(credit_balance - debit_balance), 0)").Scan(&totalLiabilities)
		db.Model(&ChartOfAccount{}).Where("account_type = ? AND is_active = ?", "revenue", true).Select("COALESCE(SUM(credit_balance - debit_balance), 0)").Scan(&totalRevenue)
		db.Model(&ChartOfAccount{}).Where("account_type = ? AND is_active = ?", "expense", true).Select("COALESCE(SUM(debit_balance - credit_balance), 0)").Scan(&totalExpenses)

		c.JSON(200, successResponse(gin.H{
			"accounts_count":   accountCount,
			"total_assets":     totalAssets,
			"total_liabilities": totalLiabilities,
			"total_revenue":    totalRevenue,
			"total_expenses":   totalExpenses,
			"net_income":      totalRevenue - totalExpenses,
		}))
	}
}

// ============= CRM MODULE =============

func setupCRMRoutes(api *gin.RouterGroup, db *gorm.DB) {
	crm := api.Group("/crm")
	{
		crm.GET("/leads", getCRMLeads(db))
		crm.POST("/leads", createCRMLead(db))
		crm.GET("/leads/:id", getCRMLead(db))
		crm.PUT("/leads/:id", updateCRMLead(db))
		crm.DELETE("/leads/:id", deleteCRMLead(db))

		crm.GET("/customers", getCRMCustomers(db))
		crm.POST("/customers", createCRMCustomer(db))

		crm.GET("/opportunities", getOpportunities(db))
		crm.POST("/opportunities", createOpportunity(db))

		crm.GET("/dashboard", getCRMDashboard(db))
	}
}

func getCRMLeads(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var leads []CRMLead
		if err := db.Find(&leads).Error; err != nil {
			c.JSON(500, errorResponse("Failed to fetch leads", err.Error()))
			return
		}
		c.JSON(200, successResponse(leads))
	}
}

func createCRMLead(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var lead CRMLead
		if err := c.ShouldBindJSON(&lead); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		if lead.ID == "" {
			lead.ID = generateID()
		}
		if lead.Status == "" {
			lead.Status = "new"
		}
		lead.OrganizationID = orgID
		lead.CreatedAt = time.Now()
		lead.UpdatedAt = time.Now()
		if err := db.Create(&lead).Error; err != nil {
			c.JSON(500, errorResponse("Failed to create lead", err.Error()))
			return
		}
		c.JSON(201, successResponse(lead, "Lead created successfully"))
	}
}

func getCRMLead(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")
		var lead CRMLead
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&lead).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Lead not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch lead"))
			return
		}
		c.JSON(200, successResponse(lead))
	}
}

func updateCRMLead(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")
		var lead CRMLead
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&lead).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Lead not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch lead"))
			return
		}
		var updates CRMLead
		if err := c.ShouldBindJSON(&updates); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		updates.UpdatedAt = time.Now()
		if err := db.Model(&lead).Updates(updates).Error; err != nil {
			c.JSON(500, errorResponse("Failed to update lead"))
			return
		}
		c.JSON(200, successResponse(lead, "Lead updated successfully"))
	}
}

func deleteCRMLead(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")
		var lead CRMLead
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&lead).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Lead not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch lead"))
			return
		}
		if err := db.Delete(&lead).Error; err != nil {
			c.JSON(500, errorResponse("Failed to delete lead"))
			return
		}
		c.JSON(200, successResponse(nil, "Lead deleted successfully"))
	}
}

func getCRMCustomers(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var customers []CRMCustomer
		if err := db.Where("status = ?", "active").Find(&customers).Error; err != nil {
			c.JSON(500, errorResponse("Failed to fetch customers", err.Error()))
			return
		}
		c.JSON(200, successResponse(customers))
	}
}

func createCRMCustomer(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var customer CRMCustomer
		if err := c.ShouldBindJSON(&customer); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		if customer.ID == "" {
			customer.ID = generateID()
		}
		if customer.Status == "" {
			customer.Status = "active"
		}
		if customer.Type == "" {
			customer.Type = "individual"
		}
		customer.OrganizationID = orgID
		customer.CreatedAt = time.Now()
		customer.UpdatedAt = time.Now()
		var count int64
		db.Model(&CRMCustomer{}).Where("organization_id = ?", orgID).Count(&count)
		customer.CustomerNumber = "CUST-" + strconv.FormatInt(count+1, 10)
		if err := db.Create(&customer).Error; err != nil {
			c.JSON(500, errorResponse("Failed to create customer", err.Error()))
			return
		}
		c.JSON(201, successResponse(customer, "Customer created successfully"))
	}
}

func getOpportunities(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var opportunities []Opportunity
		if err := db.Find(&opportunities).Error; err != nil {
			c.JSON(500, errorResponse("Failed to fetch opportunities", err.Error()))
			return
		}
		c.JSON(200, successResponse(opportunities))
	}
}

func createOpportunity(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var opportunity Opportunity
		if err := c.ShouldBindJSON(&opportunity); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		if opportunity.ID == "" {
			opportunity.ID = generateID()
		}
		if opportunity.Stage == "" {
			opportunity.Stage = "prospecting"
		}
		if opportunity.Probability == 0 {
			opportunity.Probability = 10
		}
		opportunity.OrganizationID = orgID
		opportunity.CreatedAt = time.Now()
		opportunity.UpdatedAt = time.Now()
		if err := db.Create(&opportunity).Error; err != nil {
			c.JSON(500, errorResponse("Failed to create opportunity", err.Error()))
			return
		}
		c.JSON(201, successResponse(opportunity, "Opportunity created successfully"))
	}
}

func getCRMDashboard(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var leadCount, customerCount, oppCount int64
		var totalOppValue float64

		db.Model(&CRMLead{}).Count(&leadCount)
		db.Model(&CRMCustomer{}).Where("status = ?", "active").Count(&customerCount)
		db.Model(&Opportunity{}).Count(&oppCount)
		db.Model(&Opportunity{}).Select("COALESCE(SUM(amount), 0)").Scan(&totalOppValue)

		c.JSON(200, successResponse(gin.H{
			"leads_count":           leadCount,
			"customers_count":       customerCount,
			"opportunities_count":   oppCount,
			"total_pipeline_value":  totalOppValue,
		}))
	}
}

// ============= INVENTORY MODULE =============

func setupInventoryRoutes(api *gin.RouterGroup, db *gorm.DB) {
	inventory := api.Group("/inventory")
	{
		inventory.GET("/products", getInventoryProducts(db))
		inventory.POST("/products", createInventoryProduct(db))
		inventory.GET("/products/:id", getInventoryProduct(db))
		inventory.PUT("/products/:id", updateInventoryProduct(db))
		inventory.DELETE("/products/:id", deleteInventoryProduct(db))
		inventory.GET("/dashboard", getInventoryDashboard(db))
	}
}

func getInventoryProducts(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var products []InventoryProduct
		if err := db.Where("is_active = ?", true).Find(&products).Error; err != nil {
			c.JSON(500, errorResponse("Failed to fetch products", err.Error()))
			return
		}
		c.JSON(200, successResponse(products))
	}
}

func createInventoryProduct(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		var product InventoryProduct
		if err := c.ShouldBindJSON(&product); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		if product.ID == "" {
			product.ID = generateID()
		}
		if product.Type == "" {
			product.Type = "inventory"
		}
		if product.UnitOfMeasure == "" {
			product.UnitOfMeasure = "each"
		}
		product.OrganizationID = orgID
		product.IsActive = true
		product.CreatedAt = time.Now()
		product.UpdatedAt = time.Now()
		if err := db.Create(&product).Error; err != nil {
			c.JSON(500, errorResponse("Failed to create product", err.Error()))
			return
		}
		c.JSON(201, successResponse(product, "Product created successfully"))
	}
}

func getInventoryProduct(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")
		var product InventoryProduct
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&product).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Product not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch product"))
			return
		}
		c.JSON(200, successResponse(product))
	}
}

func updateInventoryProduct(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")
		var product InventoryProduct
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&product).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Product not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch product"))
			return
		}
		var updates InventoryProduct
		if err := c.ShouldBindJSON(&updates); err != nil {
			c.JSON(400, errorResponse("Invalid request", err.Error()))
			return
		}
		updates.UpdatedAt = time.Now()
		if err := db.Model(&product).Updates(updates).Error; err != nil {
			c.JSON(500, errorResponse("Failed to update product"))
			return
		}
		c.JSON(200, successResponse(product, "Product updated successfully"))
	}
}

func deleteInventoryProduct(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)
		id := c.Param("id")
		var product InventoryProduct
		if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&product).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(404, errorResponse("Product not found"))
				return
			}
			c.JSON(500, errorResponse("Failed to fetch product"))
			return
		}
		if err := db.Model(&product).Update("is_active", false).Error; err != nil {
			c.JSON(500, errorResponse("Failed to delete product"))
			return
		}
		c.JSON(200, successResponse(nil, "Product deleted successfully"))
	}
}

func getInventoryDashboard(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var productCount, lowStockCount int64
		var totalInventoryValue float64

		db.Model(&InventoryProduct{}).Where("is_active = ?", true).Count(&productCount)
		db.Model(&InventoryProduct{}).Where("is_active = ? AND current_stock <= reorder_point", true).Count(&lowStockCount)
		db.Model(&InventoryProduct{}).Where("is_active = ?", true).Select("COALESCE(SUM(current_stock * cost_price), 0)").Scan(&totalInventoryValue)

		c.JSON(200, successResponse(gin.H{
			"products_count":        productCount,
			"low_stock_count":       lowStockCount,
			"total_inventory_value": totalInventoryValue,
		}))
	}
}

// ============= ADMIN MODULE =============

func setupAdminRoutes(api *gin.RouterGroup, db *gorm.DB) {
	admin := api.Group("/admin")
	{
		admin.GET("/overview", getAdminOverview(db))
		admin.GET("/modules", getModuleStatus(db))
	}
}

func getAdminOverview(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := getOrganizationID(db)

		var stats struct {
			Bookings      int64
			Customers     int64
			Services      int64
			Vehicles      int64
			Accounts      int64
			Leads         int64
			CRMCustomers  int64
			Products      int64
			Opportunities int64
		}

		db.Model(&models.Booking{}).Where("organization_id = ?", orgID).Count(&stats.Bookings)
		db.Model(&models.Customer{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&stats.Customers)
		db.Model(&models.Service{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&stats.Services)
		db.Model(&models.Vehicle{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&stats.Vehicles)
		db.Model(&ChartOfAccount{}).Where("is_active = ?", true).Count(&stats.Accounts)
		db.Model(&CRMLead{}).Count(&stats.Leads)
		db.Model(&CRMCustomer{}).Where("status = ?", "active").Count(&stats.CRMCustomers)
		db.Model(&InventoryProduct{}).Where("is_active = ?", true).Count(&stats.Products)
		db.Model(&Opportunity{}).Count(&stats.Opportunities)

		c.JSON(200, successResponse(gin.H{
			"system_stats": stats,
			"modules": gin.H{
				"booking": gin.H{
					"name":        "Booking & Appointment Management",
					"description": "Manage appointments, customers, services, and vehicles",
					"status":      "active",
				},
				"finance": gin.H{
					"name":        "Financial Management",
					"description": "Chart of accounts, journal entries, invoicing",
					"status":      "active",
				},
				"crm": gin.H{
					"name":        "Customer Relationship Management",
					"description": "Leads, customers, opportunities, sales pipeline",
					"status":      "active",
				},
				"inventory": gin.H{
					"name":        "Inventory Management",
					"description": "Products, stock levels, warehouses",
					"status":      "active",
				},
			},
		}))
	}
}

func getModuleStatus(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(200, successResponse(gin.H{
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
		}))
	}
}