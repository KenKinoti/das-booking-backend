package events

import (
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// RegisterRoutes mounts the organisation-scoped management API on an
// authenticated group (JWT middleware must set org_id / user_id).
func RegisterRoutes(auth *gin.RouterGroup, h *Handler) {
	g := auth.Group("/events")
	g.GET("", h.ListEvents)
	g.POST("", h.CreateEvent)
	g.GET("/stats", h.Stats)
	g.GET("/categories", h.Categories)
	g.GET("/:id", h.GetEvent)
	g.PUT("/:id", h.UpdateEvent)
	g.DELETE("/:id", h.DeleteEvent)
	g.POST("/:id/status", h.SetStatus)
	g.POST("/:id/duplicate", h.DuplicateEvent)
	g.GET("/:id/registrations", h.ListRegistrations)
	g.POST("/:id/registrations", h.CreateRegistration)
	g.GET("/:id/registrations/export", h.ExportRegistrations)
	g.PUT("/:id/registrations/:regId", h.UpdateRegistration)
	g.DELETE("/:id/registrations/:regId", h.DeleteRegistration)
	g.POST("/:id/registrations/:regId/check-in", h.CheckIn)
}

// RegisterPublicRoutes mounts the unauthenticated landing page + registration API.
func RegisterPublicRoutes(public *gin.RouterGroup, h *Handler) {
	g := public.Group("/events")
	g.GET("/:id", h.PublicGetEvent)
	g.GET("/:id/page", h.PublicPage)
	g.POST("/:id/register", h.PublicRegister)
}

// Migrate creates/updates the event tables and removes legacy foreign keys
// (to users/customers/organizations) that older model versions created.
func Migrate(db *gorm.DB) {
	for _, m := range []interface{}{&models.Event{}, &models.TicketType{}, &models.EventRegistration{}} {
		if err := db.AutoMigrate(m); err != nil {
			log.Printf("⚠️  events migration %T: %v", m, err)
		}
	}
	stmts := []string{
		`ALTER TABLE events DROP CONSTRAINT IF EXISTS fk_events_organization`,
		`ALTER TABLE events DROP CONSTRAINT IF EXISTS fk_events_creator`,
		`ALTER TABLE event_registrations DROP CONSTRAINT IF EXISTS fk_event_registrations_ticket_type`,
		`ALTER TABLE event_registrations DROP CONSTRAINT IF EXISTS fk_ticket_types_registrations`,
		`ALTER TABLE event_registrations DROP CONSTRAINT IF EXISTS fk_event_registrations_user`,
		`ALTER TABLE event_registrations DROP CONSTRAINT IF EXISTS fk_event_registrations_customer`,
		`ALTER TABLE event_registrations DROP CONSTRAINT IF EXISTS fk_event_registrations_event`,
		`ALTER TABLE ticket_types DROP CONSTRAINT IF EXISTS fk_ticket_types_event`,
		`ALTER TABLE event_reviews DROP CONSTRAINT IF EXISTS fk_event_reviews_user`,
		`ALTER TABLE event_reviews DROP CONSTRAINT IF EXISTS fk_event_reviews_registration`,
		`ALTER TABLE event_registrations ALTER COLUMN ticket_type_id DROP NOT NULL`,
		`ALTER TABLE events ALTER COLUMN created_by DROP NOT NULL`,
		`ALTER TABLE events ALTER COLUMN category DROP NOT NULL`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			log.Printf("⚠️  events migration: %v", err)
		}
	}
}
