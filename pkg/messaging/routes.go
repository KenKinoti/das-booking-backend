package messaging

import (
	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/middleware"
)

func RegisterRoutes(r *gin.Engine, handler *Handler, cfg *config.Config) {
	// WebSocket endpoint
	r.GET("/ws/messaging", handler.HandleWebSocket)

	// API routes (protected by auth middleware)
	api := r.Group("/api/v1/messaging")
	api.Use(middleware.AuthRequired(cfg))

	// Thread management
	api.GET("/threads", handler.GetThreads)
	api.POST("/threads", handler.CreateThread)

	// Message operations
	api.GET("/threads/:threadId/messages", handler.GetMessages)
	api.POST("/threads/:threadId/messages", handler.SendMessage)

	// Settings and integrations
	api.GET("/settings", handler.GetMessageSettings)
	api.PUT("/settings", handler.UpdateMessageSettings)

	api.GET("/integrations", handler.GetMessageIntegrations)
	api.PUT("/integrations/:provider", handler.UpdateMessageIntegration)

	// Search
	api.GET("/search", handler.SearchMessages)

	// Unread count
	api.GET("/unread-count", handler.GetUnreadCount)

	// WhatsApp Integration
	whatsapp := api.Group("/whatsapp")
	whatsapp.POST("/send", handler.SendWhatsAppMessage)
	whatsapp.POST("/send-template", handler.SendWhatsAppTemplate)
	whatsapp.POST("/test-connection", handler.TestWhatsAppConnection)

	// WhatsApp webhook (public endpoint)
	r.GET("/webhook/whatsapp", handler.VerifyWhatsAppWebhook)
	r.POST("/webhook/whatsapp", handler.HandleWhatsAppWebhook)

	// File uploads (for later implementation)
	api.POST("/threads/:threadId/upload", func(c *gin.Context) {
		c.JSON(501, gin.H{"error": "File upload not implemented yet"})
	})

	// Message templates
	api.GET("/templates", func(c *gin.Context) {
		c.JSON(501, gin.H{"error": "Templates not implemented yet"})
	})
	api.POST("/templates", func(c *gin.Context) {
		c.JSON(501, gin.H{"error": "Templates not implemented yet"})
	})
}