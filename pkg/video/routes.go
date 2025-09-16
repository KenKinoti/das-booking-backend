package video

import (
	"github.com/gin-gonic/gin"
)

// RegisterRoutes registers video call and streaming routes
func RegisterRoutes(r *gin.Engine, handler *Handler) {
	// WebRTC signaling WebSocket endpoint
	r.GET("/ws/webrtc", handler.HandleWebRTCSignaling)

	// API routes (protected by auth middleware)
	api := r.Group("/api/v1/video")

	// Video call endpoints
	calls := api.Group("/calls")
	{
		calls.POST("/initiate", handler.InitiateCall)
		calls.POST("/:callId/accept", handler.AcceptCall)
		calls.POST("/:callId/reject", handler.RejectCall)
		calls.POST("/:callId/end", handler.EndCall)
		calls.GET("/history", handler.GetCallHistory)
		calls.POST("/:callId/recording/start", handler.StartRecording)
		calls.POST("/:callId/recording/stop", handler.StopRecording)
	}

	// Live streaming endpoints
	streams := api.Group("/streams")
	{
		streams.POST("/create", handler.CreateLiveStream)
		streams.POST("/:streamId/start", handler.StartLiveStream)
		streams.POST("/:streamId/end", handler.EndLiveStream)
	}

	// Room management
	rooms := api.Group("/rooms")
	{
		rooms.GET("/:roomId/info", handler.GetRoomInfo)
	}

	// Settings
	settings := api.Group("/settings")
	{
		settings.GET("/", handler.GetVideoSettings)
		settings.PUT("/", handler.UpdateVideoSettings)
	}

	// Recording downloads (these might need additional file serving logic)
	api.GET("/recordings/:recordingId/download", func(c *gin.Context) {
		c.JSON(501, gin.H{"error": "Recording download not implemented yet"})
	})

	// Live stream viewing endpoints
	api.GET("/live/:streamId", func(c *gin.Context) {
		c.JSON(501, gin.H{"error": "Live stream viewing not implemented yet"})
	})
}