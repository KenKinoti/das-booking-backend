package events

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SetupRoutes sets up all event-related routes
func SetupRoutes(router *gin.RouterGroup, db *gorm.DB) {
	handler := NewHandler(db)

	// Public routes - no authentication required
	public := router.Group("/events")
	{
		// Event discovery and search
		public.GET("", handler.GetEvents)              // GET /api/events?category=&type=&search=&upcoming=true
		public.GET("/search", handler.SearchEvents)    // GET /api/events/search?q=conference
		public.GET("/categories", handler.GetEventCategories) // GET /api/events/categories
		public.GET("/:id", handler.GetEvent)           // GET /api/events/:id
		public.GET("/:event_id/tickets", handler.GetTicketTypes) // GET /api/events/:event_id/tickets
	}

	// Protected routes - require authentication
	protected := router.Group("/events")
	// TODO: Add authentication middleware here
	// protected.Use(middleware.AuthRequired())
	{
		// Event management (organizers)
		protected.POST("", handler.CreateEvent)          // POST /api/events
		protected.PUT("/:id", handler.UpdateEvent)       // PUT /api/events/:id
		protected.DELETE("/:id", handler.DeleteEvent)    // DELETE /api/events/:id

		// Ticket type management
		protected.POST("/:event_id/tickets", handler.CreateTicketType) // POST /api/events/:event_id/tickets

		// Registration management
		protected.POST("/:event_id/register", handler.RegisterForEvent) // POST /api/events/:event_id/register
		protected.GET("/:event_id/registrations", handler.GetRegistrations) // GET /api/events/:event_id/registrations

		// Check-in functionality
		protected.POST("/checkin", handler.CheckInRegistration) // POST /api/events/checkin

		// Analytics
		protected.GET("/:event_id/analytics", handler.GetEventAnalytics) // GET /api/events/:event_id/analytics
	}

	// Organizer-specific routes
	organizer := router.Group("/organizer/events")
	// TODO: Add organizer authentication middleware
	// organizer.Use(middleware.OrganizerRequired())
	{
		// These routes will be filtered by the organizer's organization
		organizer.GET("", handler.GetEvents)              // GET /api/organizer/events (filtered by org)
		organizer.POST("", handler.CreateEvent)           // POST /api/organizer/events
		organizer.GET("/:id", handler.GetEvent)           // GET /api/organizer/events/:id
		organizer.PUT("/:id", handler.UpdateEvent)        // PUT /api/organizer/events/:id
		organizer.DELETE("/:id", handler.DeleteEvent)     // DELETE /api/organizer/events/:id
		organizer.GET("/:event_id/analytics", handler.GetEventAnalytics) // GET /api/organizer/events/:event_id/analytics
	}
}