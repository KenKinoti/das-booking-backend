package handlers

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/middleware"
)

type Handler struct {
	DB     *gorm.DB
	Config *config.Config
}

func NewHandler(db *gorm.DB, cfg *config.Config) *Handler {
	return &Handler{
		DB:     db,
		Config: cfg,
	}
}

// Helper method to get organization ID from context
func (h *Handler) getOrganizationID(c *gin.Context) string {
	orgID, exists := c.Get("org_id")
	if !exists {
		return ""
	}
	
	orgIDStr, ok := orgID.(string)
	if !ok {
		return ""
	}
	
	return orgIDStr
}

func (h *Handler) SetupRoutes(router *gin.Engine) {
	// Health check
	router.GET("/health", h.HealthCheck)

	// API v1 routes
	v1 := router.Group("/api/v1")
	{
		// Public auth routes
		auth := v1.Group("/auth")
		{
			auth.POST("/login", h.Login)
			auth.POST("/refresh", h.RefreshToken)
			auth.GET("/test-accounts", h.GetTestAccounts)
		}

		// Protected routes (require authentication)
		protected := v1.Group("/")
		protected.Use(middleware.AuthRequired(h.Config))
		{
			// Auth routes that require authentication
			auth := protected.Group("/auth")
			{
				auth.POST("/logout", h.Logout)
			}

			// User routes
			users := protected.Group("/users")
			{
				users.GET("/me", h.GetCurrentUser)
				users.GET("", h.GetUsers)
				users.POST("", middleware.RequireRole("admin"), h.CreateUser)
				users.PUT("/:id", h.UpdateUser)
				users.DELETE("/:id", middleware.RequireRole("admin"), h.DeleteUser)
			}

			// Participant routes
			participants := protected.Group("/participants")
			{
				participants.GET("", h.GetParticipants)
				participants.GET("/:id", h.GetParticipant)
				participants.POST("", h.CreateParticipant)
				participants.PUT("/:id", h.UpdateParticipant)
				participants.DELETE("/:id", h.DeleteParticipant)
			}

			// Shift routes
			shifts := protected.Group("/shifts")
			{
				shifts.GET("", h.GetShifts)
				shifts.GET("/:id", h.GetShift)
				shifts.POST("", h.CreateShift)
				shifts.PUT("/:id", h.UpdateShift)
				shifts.PATCH("/:id/status", h.UpdateShiftStatus)
				shifts.DELETE("/:id", h.DeleteShift)
			}

			// Document routes
			documents := protected.Group("/documents")
			{
				documents.GET("", h.GetDocuments)
				documents.GET("/:id", h.GetDocument)
				documents.POST("", h.UploadDocument)
				documents.PUT("/:id", h.UpdateDocument)
				documents.DELETE("/:id", h.DeleteDocument)
				documents.GET("/:id/download", h.DownloadDocument)
			}

			// Organization routes
			organization := protected.Group("/organization")
			{
				organization.GET("", h.GetOrganization)
				organization.PUT("", middleware.RequireRole("admin", "manager"), h.UpdateOrganization)
				
				// Organization branding routes
				organization.GET("/branding", h.GetOrganizationBranding)
				organization.PUT("/branding", middleware.RequireRole("admin"), h.UpdateOrganizationBranding)
				
				// Organization settings routes
				organization.GET("/settings", h.GetOrganizationSettings)
				organization.PUT("/settings", middleware.RequireRole("admin"), h.UpdateOrganizationSettings)
				
				// Organization subscription routes
				organization.GET("/subscription", middleware.RequireRole("admin"), h.GetOrganizationSubscription)
				organization.PUT("/subscription", middleware.RequireRole("admin"), h.UpdateOrganizationSubscription)
			}

			// Super Admin routes (require super_admin role)
			superAdmin := protected.Group("/super-admin")
			superAdmin.Use(middleware.RequireSuperAdmin())
			{
				// Organization management
				organizations := superAdmin.Group("/organizations")
				{
					organizations.GET("", h.GetAllOrganizations)
					organizations.GET("/:id", h.GetOrganizationById)
					organizations.POST("", h.CreateOrganization)
					organizations.PATCH("/:id/status", h.UpdateOrganizationStatus)
					organizations.DELETE("/:id", h.DeleteOrganization)
				}
			}

			// Admin routes (require admin role)
			admin := protected.Group("/admin")
			admin.Use(middleware.RequireRole("admin", "super_admin"))
			{
				admin.POST("/seed", h.SeedDatabase)
				admin.POST("/seed-organizations", h.SeedOrganizations)
				admin.POST("/seed-advanced", h.SeedAdvanced)
				admin.DELETE("/clear-test-data", middleware.RequireElevatedAuth(), h.ClearTestData)
				admin.DELETE("/truncate", middleware.RequireElevatedAuth(), middleware.RequirePasswordConfirmation(), h.TruncateDatabase)
				
				// Database management routes (admin can view stats)
				admin.GET("/stats", h.GetSystemStats)
				admin.GET("/tables", h.GetTableStats)
				admin.POST("/backup", middleware.RequireSuperAdmin(), middleware.RequireElevatedAuth(), h.DatabaseBackup)
				admin.POST("/restore", middleware.RequireSuperAdmin(), middleware.RequireElevatedAuth(), h.DatabaseRestore)
				admin.POST("/maintenance", middleware.RequireSuperAdmin(), middleware.RequireElevatedAuth(), h.DatabaseMaintenance)
				admin.POST("/cleanup", middleware.RequireSuperAdmin(), middleware.RequireElevatedAuth(), middleware.RequirePasswordConfirmation(), h.DatabaseCleanup)
				admin.GET("/tables/:table", middleware.RequireSuperAdmin(), h.GetTableData)
			}

			// Emergency Contact routes
			emergencyContacts := protected.Group("/emergency-contacts")
			{
				emergencyContacts.GET("", h.GetEmergencyContacts)
				emergencyContacts.GET("/:id", h.GetEmergencyContact)
				emergencyContacts.POST("", h.CreateEmergencyContact)
				emergencyContacts.PUT("/:id", h.UpdateEmergencyContact)
				emergencyContacts.DELETE("/:id", h.DeleteEmergencyContact)
			}

			// Care Plan routes
			carePlans := protected.Group("/care-plans")
			{
				carePlans.GET("", h.GetCarePlans)
				carePlans.GET("/:id", h.GetCarePlan)
				carePlans.POST("", h.CreateCarePlan)
				carePlans.PUT("/:id", h.UpdateCarePlan)
				carePlans.PATCH("/:id/approve", middleware.RequireRole("admin,manager"), h.ApproveCarePlan)
				carePlans.DELETE("/:id", h.DeleteCarePlan)
			}

			// Billing routes
			billing := protected.Group("/billing")
			{
				billing.GET("", h.GetBilling)
				billing.GET("/:id", h.GetBillingRecord)
				billing.POST("/generate", h.GenerateInvoice)
				billing.POST("/:id/payment", h.MarkAsPaid)
				billing.GET("/:id/download", h.DownloadInvoice)
			}

			// Reports routes
			reports := protected.Group("/reports")
			{
				reports.GET("/dashboard", h.GetDashboardStats)
				reports.GET("/revenue", h.GetRevenueReport)
				reports.GET("/shifts", h.GetShiftsReport)
				reports.GET("/service-hours", h.GetServiceHoursReport)
				reports.GET("/participants", h.GetParticipantReport)
				reports.GET("/staff-performance", h.GetStaffPerformance)
				reports.GET("/:type/export", h.ExportReport)
				reports.GET("/templates", h.GetReportTemplates)
			}

			// Customer routes
			customers := protected.Group("/customers")
			{
				customers.GET("", h.GetCustomers)
				customers.GET("/stats", h.GetCustomerStats)
				customers.GET("/:id", h.GetCustomer)
				customers.POST("", h.CreateCustomer)
				customers.PUT("/:id", h.UpdateCustomer)
				customers.PATCH("/:id/toggle-status", h.ToggleCustomerStatus)
				customers.DELETE("/:id", h.DeleteCustomer)
			}

			// Vehicle routes
			vehicles := protected.Group("/vehicles")
			{
				vehicles.GET("", h.GetVehicles)
				vehicles.GET("/stats", h.GetVehicleStats)
				vehicles.GET("/:id", h.GetVehicle)
				vehicles.POST("", h.CreateVehicle)
				vehicles.PUT("/:id", h.UpdateVehicle)
				vehicles.PATCH("/:id/toggle-status", h.ToggleVehicleStatus)
				vehicles.PATCH("/:id/mileage", h.UpdateVehicleMileage)
				vehicles.DELETE("/:id", h.DeleteVehicle)
			}

			// Customer vehicle routes
			customerVehicles := protected.Group("/customers/:customer_id/vehicles")
			{
				customerVehicles.GET("", h.GetCustomerVehicles)
			}

			// Service routes
			services := protected.Group("/services")
			{
				services.GET("", h.GetServices)
				services.GET("/categories", h.GetServiceCategories)
				services.GET("/stats", h.GetServiceStats)
				services.GET("/:id", h.GetService)
				services.POST("", h.CreateService)
				services.POST("/:id/duplicate", h.DuplicateService)
				services.PUT("/:id", h.UpdateService)
				services.PATCH("/:id/toggle-status", h.ToggleServiceStatus)
				services.DELETE("/:id", h.DeleteService)
			}

			// Booking routes
			bookings := protected.Group("/bookings")
			{
				bookings.GET("", h.GetBookings)
				bookings.GET("/available-slots", h.GetAvailableTimeSlots)
				bookings.GET("/:id", h.GetBooking)
				bookings.POST("", h.CreateBooking)
				bookings.PUT("/:id", h.UpdateBooking)
				bookings.PATCH("/:id/status", h.UpdateBookingStatus)
				bookings.DELETE("/:id", h.DeleteBooking)
			}
		}
	}
}
