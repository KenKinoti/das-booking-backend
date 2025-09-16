package video

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Handler handles video call and streaming operations
type Handler struct {
	DB             *gorm.DB
	SignalingServer *WebRTCSignalingServer
}

// NewHandler creates a new video handler
func NewHandler(db *gorm.DB) *Handler {
	return &Handler{
		DB:             db,
		SignalingServer: NewWebRTCSignalingServer(db),
	}
}

// InitiateCall initiates a new video call
func (h *Handler) InitiateCall(c *gin.Context) {
	userID := c.GetUint("user_id")
	organizationID := c.GetUint("organization_id")

	var request struct {
		CalleeID     uint   `json:"callee_id" binding:"required"`
		CallType     string `json:"call_type" binding:"required"` // video, audio, screen_share
		ThreadID     *uint  `json:"thread_id"`
		IsEmergency  bool   `json:"is_emergency"`
		Quality      string `json:"quality"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate call type
	if request.CallType != "video" && request.CallType != "audio" && request.CallType != "screen_share" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid call type"})
		return
	}

	// Check if callee exists and is in the same organization
	var callee models.User
	if err := h.DB.Where("id = ? AND organization_id = ?", request.CalleeID, organizationID).First(&callee).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Callee not found"})
		return
	}

	// Create video call record
	call := models.VideoCall{
		CallID:         uuid.New().String(),
		CallerID:       userID,
		CalleeID:       request.CalleeID,
		ThreadID:       request.ThreadID,
		OrganizationID: organizationID,
		CallType:       request.CallType,
		Status:         "pending",
		Quality:        request.Quality,
		IsEmergency:    request.IsEmergency,
	}

	if call.Quality == "" {
		call.Quality = "hd"
	}

	if err := h.DB.Create(&call).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create call"})
		return
	}

	// TODO: Send real-time notification to callee via WebSocket
	// This would integrate with the messaging system to notify the callee

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    call,
		"room_id": call.CallID,
	})
}

// AcceptCall accepts an incoming video call
func (h *Handler) AcceptCall(c *gin.Context) {
	userID := c.GetUint("user_id")
	callID := c.Param("callId")

	var call models.VideoCall
	if err := h.DB.Where("call_id = ? AND (caller_id = ? OR callee_id = ?)",
		callID, userID, userID).First(&call).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Call not found"})
		return
	}

	if call.Status != "pending" && call.Status != "ringing" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Call cannot be accepted"})
		return
	}

	// Update call status
	call.Status = "active"
	startTime := time.Now()
	call.StartedAt = &startTime

	if err := h.DB.Save(&call).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update call"})
		return
	}

	// Create participant records
	participants := []models.CallParticipant{
		{
			CallID:       call.ID,
			UserID:       call.CallerID,
			Role:         "host",
			Status:       "connecting",
			AudioEnabled: true,
			VideoEnabled: call.CallType == "video",
			JoinedAt:     &startTime,
		},
		{
			CallID:       call.ID,
			UserID:       call.CalleeID,
			Role:         "participant",
			Status:       "connecting",
			AudioEnabled: true,
			VideoEnabled: call.CallType == "video",
			JoinedAt:     &startTime,
		},
	}

	for _, participant := range participants {
		h.DB.Create(&participant)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    call,
		"room_id": call.CallID,
	})
}

// RejectCall rejects an incoming video call
func (h *Handler) RejectCall(c *gin.Context) {
	userID := c.GetUint("user_id")
	callID := c.Param("callId")

	var call models.VideoCall
	if err := h.DB.Where("call_id = ? AND callee_id = ?", callID, userID).First(&call).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Call not found"})
		return
	}

	if call.Status != "pending" && call.Status != "ringing" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Call cannot be rejected"})
		return
	}

	// Update call status
	call.Status = "rejected"
	endTime := time.Now()
	call.EndedAt = &endTime

	if err := h.DB.Save(&call).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update call"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Call rejected",
	})
}

// EndCall ends an active video call
func (h *Handler) EndCall(c *gin.Context) {
	userID := c.GetUint("user_id")
	callID := c.Param("callId")

	var call models.VideoCall
	if err := h.DB.Where("call_id = ? AND (caller_id = ? OR callee_id = ?)",
		callID, userID, userID).First(&call).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Call not found"})
		return
	}

	if call.Status != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Call is not active"})
		return
	}

	// Update call status
	call.Status = "ended"
	endTime := time.Now()
	call.EndedAt = &endTime

	// Calculate duration
	if call.StartedAt != nil {
		call.Duration = int(endTime.Sub(*call.StartedAt).Seconds())
	}

	if err := h.DB.Save(&call).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update call"})
		return
	}

	// Update participant records
	h.DB.Model(&models.CallParticipant{}).
		Where("call_id = ? AND left_at IS NULL", call.ID).
		Update("left_at", endTime)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Call ended",
		"duration": call.Duration,
	})
}

// GetCallHistory retrieves call history for a user
func (h *Handler) GetCallHistory(c *gin.Context) {
	userID := c.GetUint("user_id")
	organizationID := c.GetUint("organization_id")

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset := (page - 1) * limit

	var calls []models.VideoCall
	query := h.DB.Where("organization_id = ? AND (caller_id = ? OR callee_id = ?)",
		organizationID, userID, userID).
		Preload("Caller").
		Preload("Callee").
		Preload("Participants").
		Order("created_at DESC").
		Limit(limit).
		Offset(offset)

	if err := query.Find(&calls).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch call history"})
		return
	}

	var total int64
	h.DB.Model(&models.VideoCall{}).
		Where("organization_id = ? AND (caller_id = ? OR callee_id = ?)",
			organizationID, userID, userID).
		Count(&total)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"calls":        calls,
			"current_page": page,
			"per_page":     limit,
			"total":        total,
			"last_page":    (total + int64(limit) - 1) / int64(limit),
		},
	})
}

// StartRecording starts recording a video call
func (h *Handler) StartRecording(c *gin.Context) {
	userID := c.GetUint("user_id")
	callID := c.Param("callId")

	var call models.VideoCall
	if err := h.DB.Where("call_id = ? AND caller_id = ?", callID, userID).First(&call).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Call not found or unauthorized"})
		return
	}

	if call.Status != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Call is not active"})
		return
	}

	if call.IsRecorded {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Recording already in progress"})
		return
	}

	// Create recording record
	recording := models.CallRecording{
		CallID:      call.ID,
		RecordingID: uuid.New().String(),
		FileName:    fmt.Sprintf("call_%s_%d.mp4", call.CallID, time.Now().Unix()),
		FilePath:    fmt.Sprintf("/recordings/calls/%s/", call.CallID),
		Format:      "mp4",
		Quality:     call.Quality,
		Status:      "processing",
		StartedAt:   time.Now(),
	}

	if err := h.DB.Create(&recording).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start recording"})
		return
	}

	// Update call record
	call.IsRecorded = true
	call.RecordingID = &recording.RecordingID
	h.DB.Save(&call)

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"recording_id": recording.RecordingID,
		"message":      "Recording started",
	})
}

// StopRecording stops recording a video call
func (h *Handler) StopRecording(c *gin.Context) {
	userID := c.GetUint("user_id")
	callID := c.Param("callId")

	var call models.VideoCall
	if err := h.DB.Where("call_id = ? AND caller_id = ?", callID, userID).First(&call).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Call not found or unauthorized"})
		return
	}

	if !call.IsRecorded || call.RecordingID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No active recording"})
		return
	}

	// Update recording record
	var recording models.CallRecording
	if err := h.DB.Where("recording_id = ?", *call.RecordingID).First(&recording).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Recording not found"})
		return
	}

	completedAt := time.Now()
	recording.CompletedAt = &completedAt
	recording.Duration = int(completedAt.Sub(recording.StartedAt).Seconds())
	recording.Status = "ready"
	recording.DownloadURL = fmt.Sprintf("/api/v1/video/recordings/%s/download", recording.RecordingID)

	if err := h.DB.Save(&recording).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update recording"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"message":  "Recording stopped",
		"duration": recording.Duration,
		"download_url": recording.DownloadURL,
	})
}

// CreateLiveStream creates a new live stream
func (h *Handler) CreateLiveStream(c *gin.Context) {
	userID := c.GetUint("user_id")
	organizationID := c.GetUint("organization_id")

	var request struct {
		Title       string     `json:"title" binding:"required"`
		Description string     `json:"description"`
		Privacy     string     `json:"privacy" binding:"required"` // public, private, organization
		Quality     string     `json:"quality"`
		ScheduledAt *time.Time `json:"scheduled_at"`
		ThreadID    *uint      `json:"thread_id"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Create live stream record
	stream := models.LiveStream{
		StreamID:       uuid.New().String(),
		HostID:         userID,
		OrganizationID: organizationID,
		ThreadID:       request.ThreadID,
		Title:          request.Title,
		Description:    request.Description,
		Status:         "scheduled",
		Privacy:        request.Privacy,
		Quality:        request.Quality,
		ScheduledAt:    request.ScheduledAt,
		StreamURL:      fmt.Sprintf("rtmp://localhost:1935/live/%s", uuid.New().String()),
		WatchURL:       fmt.Sprintf("/live/%s", uuid.New().String()),
	}

	if stream.Quality == "" {
		stream.Quality = "hd"
	}

	if stream.ScheduledAt == nil {
		now := time.Now()
		stream.ScheduledAt = &now
	}

	if err := h.DB.Create(&stream).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create live stream"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    stream,
	})
}

// StartLiveStream starts a live stream
func (h *Handler) StartLiveStream(c *gin.Context) {
	userID := c.GetUint("user_id")
	streamID := c.Param("streamId")

	var stream models.LiveStream
	if err := h.DB.Where("stream_id = ? AND host_id = ?", streamID, userID).First(&stream).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Stream not found or unauthorized"})
		return
	}

	if stream.Status == "live" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Stream is already live"})
		return
	}

	// Update stream status
	stream.Status = "live"
	startTime := time.Now()
	stream.StartedAt = &startTime

	if err := h.DB.Save(&stream).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start stream"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"message":    "Live stream started",
		"stream_url": stream.StreamURL,
		"watch_url":  stream.WatchURL,
	})
}

// EndLiveStream ends a live stream
func (h *Handler) EndLiveStream(c *gin.Context) {
	userID := c.GetUint("user_id")
	streamID := c.Param("streamId")

	var stream models.LiveStream
	if err := h.DB.Where("stream_id = ? AND host_id = ?", streamID, userID).First(&stream).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Stream not found or unauthorized"})
		return
	}

	if stream.Status != "live" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Stream is not live"})
		return
	}

	// Update stream status
	stream.Status = "ended"
	endTime := time.Now()
	stream.EndedAt = &endTime

	// Calculate duration
	if stream.StartedAt != nil {
		stream.Duration = int(endTime.Sub(*stream.StartedAt).Seconds())
	}

	if err := h.DB.Save(&stream).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to end stream"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"message":  "Live stream ended",
		"duration": stream.Duration,
	})
}

// GetVideoSettings retrieves video settings for an organization
func (h *Handler) GetVideoSettings(c *gin.Context) {
	organizationID := c.GetUint("organization_id")

	var settings models.VideoSettings
	err := h.DB.Where("organization_id = ?", organizationID).First(&settings).Error

	if err != nil {
		// Create default settings if not found
		settings = models.VideoSettings{
			OrganizationID:         organizationID,
			EnableVideoCalls:       true,
			EnableScreenSharing:    true,
			EnableRecording:        true,
			EnableLiveStreaming:    false,
			MaxCallDuration:        3600,
			MaxParticipants:        10,
			DefaultQuality:         "hd",
			AutoRecord:             false,
			RecordingRetentionDays: 30,
			AllowedCallTypes:       `["video", "audio", "screen_share"]`,
			STUNServers:            `["stun:stun.l.google.com:19302"]`,
		}
		h.DB.Create(&settings)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    settings,
	})
}

// UpdateVideoSettings updates video settings for an organization
func (h *Handler) UpdateVideoSettings(c *gin.Context) {
	organizationID := c.GetUint("organization_id")

	var request models.VideoSettings
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var settings models.VideoSettings
	err := h.DB.Where("organization_id = ?", organizationID).First(&settings).Error

	if err != nil {
		// Create new settings
		request.OrganizationID = organizationID
		if err := h.DB.Create(&request).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create settings"})
			return
		}
		settings = request
	} else {
		// Update existing settings
		if err := h.DB.Model(&settings).Updates(&request).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update settings"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    settings,
	})
}

// GetRoomInfo gets information about an active room
func (h *Handler) GetRoomInfo(c *gin.Context) {
	roomID := c.Param("roomId")

	roomInfo := h.SignalingServer.GetRoomInfo(roomID)
	if roomInfo == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    roomInfo,
	})
}

// HandleWebRTCSignaling handles WebRTC signaling WebSocket connections
func (h *Handler) HandleWebRTCSignaling(c *gin.Context) {
	h.SignalingServer.HandleWebRTCConnection(c)
}