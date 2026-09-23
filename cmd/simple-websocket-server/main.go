package main

import (
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for testing
	},
}

func handleWebSocket(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	log.Printf("✅ New WebSocket connection established from %s", c.ClientIP())

	// Send welcome message
	welcomeMsg := map[string]interface{}{
		"type": "welcome",
		"data": map[string]interface{}{
			"message": "Connected to DAS Communication Server",
			"status":  "connected",
		},
	}

	if err := conn.WriteJSON(welcomeMsg); err != nil {
		log.Printf("Error sending welcome message: %v", err)
		return
	}

	// Handle incoming messages
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		log.Printf("📨 Received message: %+v", msg)

		// Echo the message back with confirmation
		response := map[string]interface{}{
			"type": "message_received",
			"data": map[string]interface{}{
				"original_message": msg,
				"status":          "processed",
				"timestamp":       "2025-09-16T15:58:00Z",
			},
		}

		if err := conn.WriteJSON(response); err != nil {
			log.Printf("Error sending response: %v", err)
			break
		}
	}

	log.Printf("🔌 WebSocket connection closed")
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8091"
	}

	r := gin.Default()

	// CORS middleware
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "ok",
			"service": "Simple WebSocket Communication Server",
			"version": "1.0.0",
		})
	})

	// WebSocket endpoints
	r.GET("/ws/communication", handleWebSocket)
	r.GET("/ws/messaging", handleWebSocket)

	log.Printf("🚀 Simple WebSocket Server starting on port %s", port)
	log.Printf("📡 WebSocket endpoints available:")
	log.Printf("   - ws://localhost:%s/ws/communication", port)
	log.Printf("   - ws://localhost:%s/ws/messaging", port)
	log.Printf("🌐 Health check: http://localhost:%s/health", port)

	if err := r.Run(":" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}