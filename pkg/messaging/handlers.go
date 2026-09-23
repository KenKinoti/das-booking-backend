package messaging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

type Handler struct {
	DB        *gorm.DB
	upgrader  websocket.Upgrader
	hub       *WebSocketHub
	jwtSecret string
}

type WebSocketMessage struct {
	Type     string      `json:"type"`
	ThreadID uint        `json:"thread_id,omitempty"`
	UserID   string      `json:"user_id,omitempty"`
	Data     interface{} `json:"data"`
}

func NewHandler(db *gorm.DB) *Handler {
	return &Handler{
		DB: db,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for development
			},
		},
		hub: newHub(),
	}
}

// parseThreadID reads the numeric :threadId URL parameter, writing a 400 on
// failure.
func parseThreadID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("threadId"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Invalid thread ID"})
		return 0, false
	}
	return uint(id), true
}

// isParticipant reports whether the user is an active participant of the
// thread and the thread belongs to the organisation.
func (h *Handler) isParticipant(threadID uint, uid, oid string) bool {
	var count int64
	h.DB.Model(&models.MessageParticipant{}).
		Joins("JOIN message_threads ON message_threads.id = message_participants.thread_id AND message_threads.deleted_at IS NULL").
		Where("message_participants.thread_id = ? AND message_participants.user_id = ? AND message_participants.is_active = ?", threadID, uid, true).
		Where("message_threads.organization_id = ?", oid).
		Count(&count)
	return count > 0
}

// unreadCountForThread counts messages in a thread not sent by and not yet
// read by the user.
func (h *Handler) unreadCountForThread(threadID uint, uid string) int64 {
	var n int64
	h.DB.Model(&models.Message{}).
		Joins("LEFT JOIN message_read_statuses ON message_read_statuses.message_id = messages.id AND message_read_statuses.user_id = ? AND message_read_statuses.deleted_at IS NULL", uid).
		Where("messages.thread_id = ? AND messages.is_deleted = ? AND message_read_statuses.id IS NULL AND messages.sender_id <> ?", threadID, false, uid).
		Count(&n)
	return n
}

// Get all threads for a user
func (h *Handler) GetThreads(c *gin.Context) {
	uid, oid, ok := requireUserAndOrg(c)
	if !ok {
		return
	}

	threads := []models.MessageThread{}
	err := h.DB.
		Where("message_threads.organization_id = ?", oid).
		Where("message_threads.id IN (?)", h.DB.Model(&models.MessageParticipant{}).
			Select("thread_id").
			Where("user_id = ? AND is_active = ?", uid, true)).
		Preload("Participants", "is_active = ?", true).
		Preload("Participants.User").
		Order("message_threads.last_message_at DESC NULLS LAST, message_threads.created_at DESC").
		Find(&threads).Error

	if err != nil {
		log.Printf("messaging: failed to fetch threads: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to fetch threads"})
		return
	}

	for i := range threads {
		// Latest message first (clients read messages[0] as the preview).
		msgs := []models.Message{}
		h.DB.Where("thread_id = ? AND is_deleted = ?", threads[i].ID, false).
			Preload("Sender").
			Order("created_at DESC").
			Limit(1).
			Find(&msgs)

		// Unread count is passed as a trailing pseudo-message (legacy shape).
		unread := h.unreadCountForThread(threads[i].ID, uid)
		threads[i].Messages = append(msgs, models.Message{
			Content: fmt.Sprintf("%d", unread),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    threads,
	})
}

// Create a new thread
func (h *Handler) CreateThread(c *gin.Context) {
	uid, oid, ok := requireUserAndOrg(c)
	if !ok {
		return
	}

	var request struct {
		Name           string   `json:"name"`
		Type           string   `json:"type" binding:"required"`
		ParticipantIDs []string `json:"participant_ids" binding:"required"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}

	// De-duplicate participants (excluding the creator, who is added as admin).
	seen := map[string]bool{uid: true}
	others := []string{}
	for _, id := range request.ParticipantIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		others = append(others, id)
	}

	// All participants must be users in the same organisation.
	if len(others) > 0 {
		var found int64
		h.DB.Model(&models.User{}).Where("id IN ? AND organization_id = ?", others, oid).Count(&found)
		if int(found) != len(others) {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "One or more participants were not found in your organization"})
			return
		}
	}

	thread := models.MessageThread{
		Name:           request.Name,
		Type:           request.Type,
		CreatedBy:      uid,
		OrganizationID: oid,
		IsActive:       true,
		LastMessageAt:  nil,
	}

	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&thread).Error; err != nil {
			return err
		}
		now := time.Now()
		participants := []models.MessageParticipant{{
			ThreadID: thread.ID,
			UserID:   uid,
			Role:     "admin",
			IsActive: true,
			JoinedAt: now,
		}}
		for _, id := range others {
			participants = append(participants, models.MessageParticipant{
				ThreadID: thread.ID,
				UserID:   id,
				Role:     "member",
				IsActive: true,
				JoinedAt: now,
			})
		}
		return tx.Create(&participants).Error
	})
	if err != nil {
		log.Printf("messaging: failed to create thread: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to create thread"})
		return
	}

	// Load thread with participants
	h.DB.Preload("Participants.User").First(&thread, thread.ID)

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    thread,
	})
}

// Get messages for a thread
func (h *Handler) GetMessages(c *gin.Context) {
	uid, oid, ok := requireUserAndOrg(c)
	if !ok {
		return
	}
	threadID, ok := parseThreadID(c)
	if !ok {
		return
	}

	if !h.isParticipant(threadID, uid, oid) {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "Access denied"})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	offset := (page - 1) * limit

	messages := []models.Message{}
	err := h.DB.Where("thread_id = ? AND is_deleted = ?", threadID, false).
		Preload("Sender").
		Preload("Attachments").
		Preload("Reactions").
		Preload("ReplyTo.Sender").
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&messages).Error

	if err != nil {
		log.Printf("messaging: failed to fetch messages: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to fetch messages"})
		return
	}

	// Mark messages as read
	go h.markMessagesAsRead(threadID, uid)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    messages,
		"pagination": gin.H{
			"page":  page,
			"limit": limit,
			"total": len(messages),
		},
	})
}

// Send a message
func (h *Handler) SendMessage(c *gin.Context) {
	uid, oid, ok := requireUserAndOrg(c)
	if !ok {
		return
	}
	threadID, ok := parseThreadID(c)
	if !ok {
		return
	}

	var request struct {
		Content     string `json:"content" binding:"required"`
		MessageType string `json:"message_type"`
		ReplyToID   *uint  `json:"reply_to_id"`
		Metadata    string `json:"metadata"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}

	if !h.isParticipant(threadID, uid, oid) {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "Access denied"})
		return
	}

	// A reply must reference a message in the same thread.
	if request.ReplyToID != nil {
		var n int64
		h.DB.Model(&models.Message{}).Where("id = ? AND thread_id = ?", *request.ReplyToID, threadID).Count(&n)
		if n == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "reply_to_id does not reference a message in this thread"})
			return
		}
	}

	// metadata is stored in a json column; it must be valid JSON.
	metadata := strings.TrimSpace(request.Metadata)
	if metadata == "" {
		metadata = "{}"
	} else if !json.Valid([]byte(metadata)) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "metadata must be valid JSON"})
		return
	}

	message := models.Message{
		ThreadID:    threadID,
		SenderID:    uid,
		Content:     request.Content,
		MessageType: request.MessageType,
		ReplyToID:   request.ReplyToID,
		Metadata:    metadata,
		IsForwarded: false,
		IsEdited:    false,
		IsDeleted:   false,
	}

	if request.MessageType == "" {
		message.MessageType = "text"
	}

	if err := h.DB.Create(&message).Error; err != nil {
		log.Printf("messaging: failed to send message: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to send message"})
		return
	}

	// Update thread last message time and other participants' unread counters.
	h.DB.Model(&models.MessageThread{}).Where("id = ?", threadID).
		Update("last_message_at", time.Now())
	h.DB.Model(&models.MessageParticipant{}).
		Where("thread_id = ? AND user_id <> ? AND is_active = ?", threadID, uid, true).
		UpdateColumn("unread_count", gorm.Expr("unread_count + 1"))

	// Load message with sender info
	h.DB.Preload("Sender").Preload("ReplyTo.Sender").First(&message, message.ID)

	// Broadcast to thread participants via WebSocket
	h.broadcastToUsers(WebSocketMessage{
		Type:     "new_message",
		ThreadID: threadID,
		Data:     message,
	}, h.threadParticipantIDs(threadID))

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    message,
	})
}

// Mark messages as read
func (h *Handler) markMessagesAsRead(threadID uint, uid string) {
	var messageIDs []uint
	h.DB.Model(&models.Message{}).
		Joins("LEFT JOIN message_read_statuses ON message_read_statuses.message_id = messages.id AND message_read_statuses.user_id = ? AND message_read_statuses.deleted_at IS NULL", uid).
		Where("messages.thread_id = ? AND message_read_statuses.id IS NULL AND messages.sender_id <> ?", threadID, uid).
		Pluck("messages.id", &messageIDs)

	now := time.Now()
	readStatuses := make([]models.MessageReadStatus, 0, len(messageIDs))
	for _, messageID := range messageIDs {
		readStatuses = append(readStatuses, models.MessageReadStatus{
			MessageID: messageID,
			UserID:    uid,
			ReadAt:    now,
		})
	}

	if len(readStatuses) > 0 {
		if err := h.DB.Create(&readStatuses).Error; err != nil {
			log.Printf("messaging: failed to mark messages read: %v", err)
			return
		}
	}

	// Reset unread count for participant
	h.DB.Model(&models.MessageParticipant{}).
		Where("thread_id = ? AND user_id = ?", threadID, uid).
		Updates(map[string]interface{}{"unread_count": 0, "last_seen_at": now})
}

// Handle typing indicator
func (h *Handler) handleTypingIndicator(threadID uint, uid string, isTyping bool) {
	if threadID == 0 || uid == "" {
		return
	}
	var n int64
	h.DB.Model(&models.MessageParticipant{}).
		Where("thread_id = ? AND user_id = ? AND is_active = ?", threadID, uid, true).
		Count(&n)
	if n == 0 {
		return
	}

	var indicator models.TypingIndicator
	result := h.DB.Where("thread_id = ? AND user_id = ?", threadID, uid).First(&indicator)

	if result.Error != nil {
		indicator = models.TypingIndicator{
			ThreadID: threadID,
			UserID:   uid,
			IsTyping: isTyping,
		}
		if isTyping {
			now := time.Now()
			indicator.StartedAt = &now
		}
		h.DB.Create(&indicator)
	} else {
		updates := map[string]interface{}{
			"is_typing": isTyping,
		}
		if isTyping {
			updates["started_at"] = time.Now()
		}
		h.DB.Model(&indicator).Updates(updates)
	}

	recipients := []string{}
	for _, id := range h.threadParticipantIDs(threadID) {
		if id != uid {
			recipients = append(recipients, id)
		}
	}
	h.broadcastToUsers(WebSocketMessage{
		Type:     "typing_indicator",
		ThreadID: threadID,
		UserID:   uid,
		Data: gin.H{
			"is_typing": isTyping,
			"user_id":   uid,
		},
	}, recipients)
}

const defaultAllowedFileTypes = `["jpg", "jpeg", "png", "gif", "pdf", "doc", "docx", "txt"]`

// Get message settings for organization
func (h *Handler) GetMessageSettings(c *gin.Context) {
	oid, ok := requireOrg(c)
	if !ok {
		return
	}

	var settings models.MessageSettings
	result := h.DB.Where("organization_id = ?", oid).First(&settings)

	if result.Error != nil {
		if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
			log.Printf("messaging: failed to load settings: %v", result.Error)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to load settings"})
			return
		}
		settings = models.MessageSettings{
			OrganizationID:         oid,
			EnableInAppMessaging:   true,
			EnableGroupChats:       true,
			EnableFileSharing:      true,
			MaxFileSize:            10485760, // 10MB
			AllowedFileTypes:       defaultAllowedFileTypes,
			MessageRetentionDays:   365,
			EnableMessageReactions: true,
			EnableTypingIndicator:  true,
			EnableReadReceipts:     true,
			EnableVoiceMessages:    true,
			EnableVideoMessages:    true,
			ModerationEnabled:      false,
			ProfanityFilterEnabled: false,
		}
		if err := h.DB.Create(&settings).Error; err != nil {
			log.Printf("messaging: failed to create default settings: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to create settings"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    settings,
	})
}

// Update message settings
func (h *Handler) UpdateMessageSettings(c *gin.Context) {
	oid, ok := requireOrg(c)
	if !ok {
		return
	}

	var request models.MessageSettings
	cols, err := bindWithColumns(c, h.DB, &request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	// Never let the client move settings to another organisation or row.
	request.ID = 0
	request.OrganizationID = oid
	request.Organization = nil
	if request.AllowedFileTypes != "" && !json.Valid([]byte(request.AllowedFileTypes)) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "allowed_file_types must be a JSON array"})
		return
	}

	var settings models.MessageSettings
	result := h.DB.Where("organization_id = ?", oid).First(&settings)

	if result.Error != nil {
		if request.AllowedFileTypes == "" {
			request.AllowedFileTypes = defaultAllowedFileTypes
		}
		if err := h.DB.Create(&request).Error; err != nil {
			log.Printf("messaging: failed to create settings: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to create settings"})
			return
		}
		// Columns with DB defaults skip zero values on insert; re-apply the
		// fields the client actually sent (e.g. explicit false).
		if len(cols) > 0 {
			h.DB.Model(&request).Select(cols).Updates(&request)
		}
		settings = request
	} else {
		if len(cols) == 0 {
			// Nothing updatable was sent; return the current record.
		} else if err := h.DB.Model(&settings).Select(cols).Updates(&request).Error; err != nil {
			log.Printf("messaging: failed to update settings: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to update settings"})
			return
		}
		h.DB.First(&settings, settings.ID)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    settings,
	})
}

// Get message integrations
func (h *Handler) GetMessageIntegrations(c *gin.Context) {
	oid, ok := requireOrg(c)
	if !ok {
		return
	}

	integrations := []models.MessageIntegration{}
	if err := h.DB.Where("organization_id = ?", oid).Find(&integrations).Error; err != nil {
		log.Printf("messaging: failed to fetch integrations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to fetch integrations"})
		return
	}

	// Remove sensitive data from response
	for i := range integrations {
		integrations[i].APIKey = ""
		integrations[i].APISecret = ""
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    integrations,
	})
}

// Create or update message integration
func (h *Handler) UpdateMessageIntegration(c *gin.Context) {
	oid, ok := requireOrg(c)
	if !ok {
		return
	}
	provider := c.Param("provider")

	var request models.MessageIntegration
	cols, err := bindWithColumns(c, h.DB, &request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	request.ID = 0
	request.OrganizationID = oid
	request.Organization = nil
	request.Provider = provider
	if request.Configuration != "" && !json.Valid([]byte(request.Configuration)) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "configuration must be valid JSON"})
		return
	}

	var integration models.MessageIntegration
	result := h.DB.Where("organization_id = ? AND provider = ?", oid, provider).First(&integration)

	if result.Error != nil {
		if request.Configuration == "" {
			request.Configuration = "{}"
		}
		if err := h.DB.Create(&request).Error; err != nil {
			log.Printf("messaging: failed to create integration: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to create integration"})
			return
		}
		if len(cols) > 0 {
			h.DB.Model(&request).Select(cols).Updates(&request)
		}
		integration = request
	} else {
		if len(cols) == 0 {
			// Nothing updatable was sent; return the current record.
		} else if err := h.DB.Model(&integration).Select(cols).Updates(&request).Error; err != nil {
			log.Printf("messaging: failed to update integration: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to update integration"})
			return
		}
		h.DB.First(&integration, integration.ID)
	}

	// Remove sensitive data from response
	integration.APIKey = ""
	integration.APISecret = ""

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    integration,
	})
}

// Search messages
func (h *Handler) SearchMessages(c *gin.Context) {
	uid, oid, ok := requireUserAndOrg(c)
	if !ok {
		return
	}
	query := strings.TrimSpace(c.Query("q"))
	threadID := c.Query("thread_id")

	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Search query is required"})
		return
	}

	db := h.DB.Model(&models.Message{}).
		Joins("JOIN message_participants ON message_participants.thread_id = messages.thread_id AND message_participants.deleted_at IS NULL").
		Joins("JOIN message_threads ON message_threads.id = messages.thread_id AND message_threads.deleted_at IS NULL").
		Where("message_participants.user_id = ? AND message_participants.is_active = ? AND messages.is_deleted = ?",
			uid, true, false).
		Where("message_threads.organization_id = ?", oid).
		Where("messages.content ILIKE ?", "%"+query+"%").
		Preload("Sender").
		Preload("Thread")

	if threadID != "" {
		tid, err := strconv.ParseUint(threadID, 10, 32)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Invalid thread_id"})
			return
		}
		db = db.Where("messages.thread_id = ?", tid)
	}

	messages := []models.Message{}
	if err := db.Order("messages.created_at DESC").Limit(100).Find(&messages).Error; err != nil {
		log.Printf("messaging: search failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Search failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    messages,
	})
}

// WhatsApp Integration Endpoints

// Send WhatsApp message
func (h *Handler) SendWhatsAppMessage(c *gin.Context) {
	oid, ok := requireOrg(c)
	if !ok {
		return
	}

	var request struct {
		To      string `json:"to" binding:"required"`
		Message string `json:"message" binding:"required"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}

	var settings models.MessageSettings
	if err := h.DB.Where("organization_id = ?", oid).First(&settings).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "WhatsApp settings not found"})
		return
	}

	if settings.WhatsappAPIKey == "" || settings.WhatsappPhoneNumber == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "WhatsApp not configured"})
		return
	}

	whatsapp := NewWhatsAppService(&settings)

	response, err := whatsapp.SendMessage(request.To, request.Message)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    response,
	})
}

// Send WhatsApp template message
func (h *Handler) SendWhatsAppTemplate(c *gin.Context) {
	oid, ok := requireOrg(c)
	if !ok {
		return
	}

	var request struct {
		To         string        `json:"to" binding:"required"`
		Template   string        `json:"template" binding:"required"`
		Language   string        `json:"language"`
		Components []interface{} `json:"components"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}

	if request.Language == "" {
		request.Language = "en_US"
	}

	var settings models.MessageSettings
	if err := h.DB.Where("organization_id = ?", oid).First(&settings).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "WhatsApp settings not found"})
		return
	}

	if settings.WhatsappAPIKey == "" || settings.WhatsappPhoneNumber == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "WhatsApp not configured"})
		return
	}

	whatsapp := NewWhatsAppService(&settings)

	response, err := whatsapp.SendTemplate(request.To, request.Template, request.Language, request.Components)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    response,
	})
}

// WhatsApp webhook verification
func (h *Handler) VerifyWhatsAppWebhook(c *gin.Context) {
	mode := c.Query("hub.mode")
	token := c.Query("hub.verify_token")
	challenge := c.Query("hub.challenge")

	var settings models.MessageSettings
	if err := h.DB.Where("whatsapp_api_key <> ''").First(&settings).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Webhook verification failed"})
		return
	}

	whatsapp := NewWhatsAppService(&settings)
	challengeResponse, err := whatsapp.VerifyWebhook(mode, token, challenge)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Webhook verification failed"})
		return
	}

	c.String(http.StatusOK, challengeResponse)
}

// Handle WhatsApp webhook events
func (h *Handler) HandleWhatsAppWebhook(c *gin.Context) {
	var event WhatsAppWebhookEvent
	if err := c.ShouldBindJSON(&event); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid webhook payload"})
		return
	}

	var settings models.MessageSettings
	if err := h.DB.Where("whatsapp_api_key <> ''").First(&settings).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load settings"})
		return
	}

	whatsapp := NewWhatsAppService(&settings)
	messages, err := whatsapp.ProcessWebhookEvent(&event)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	for _, msg := range messages {
		var metadata map[string]string
		if err := json.Unmarshal([]byte(msg.Metadata), &metadata); err != nil {
			log.Printf("Failed to parse message metadata: %v", err)
			continue
		}

		phoneNumber := metadata["phone"]
		var thread models.MessageThread
		result := h.DB.Where("name = ? AND type = ? AND organization_id = ?", phoneNumber, "whatsapp", settings.OrganizationID).First(&thread)

		if result.Error != nil {
			thread = models.MessageThread{
				Name:           phoneNumber,
				Type:           "whatsapp",
				OrganizationID: settings.OrganizationID,
				IsActive:       true,
			}
			if err := h.DB.Create(&thread).Error; err != nil {
				log.Printf("Failed to create WhatsApp thread: %v", err)
				continue
			}
		}

		msg.ThreadID = thread.ID
		if err := h.DB.Create(msg).Error; err != nil {
			log.Printf("Failed to save WhatsApp message: %v", err)
			continue
		}
		h.DB.Model(&models.MessageThread{}).Where("id = ?", thread.ID).Update("last_message_at", time.Now())

		h.broadcastToUsers(WebSocketMessage{
			Type:     "message",
			ThreadID: thread.ID,
			Data:     msg,
		}, h.threadParticipantIDs(thread.ID))
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// Test WhatsApp connection
func (h *Handler) TestWhatsAppConnection(c *gin.Context) {
	oid, ok := requireOrg(c)
	if !ok {
		return
	}

	var settings models.MessageSettings
	if err := h.DB.Where("organization_id = ?", oid).First(&settings).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "WhatsApp settings not found"})
		return
	}

	if settings.WhatsappAPIKey == "" || settings.WhatsappPhoneNumber == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "WhatsApp not configured"})
		return
	}

	whatsapp := NewWhatsAppService(&settings)
	if err := whatsapp.TestConnection(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "WhatsApp connection successful",
	})
}

// Get unread message count for user: messages in the user's active threads,
// not sent by the user and without a read receipt from the user.
func (h *Handler) GetUnreadCount(c *gin.Context) {
	uid, oid, ok := requireUserAndOrg(c)
	if !ok {
		return
	}

	var count int64
	err := h.DB.Model(&models.Message{}).
		Joins("JOIN message_participants ON message_participants.thread_id = messages.thread_id AND message_participants.deleted_at IS NULL").
		Joins("JOIN message_threads ON message_threads.id = messages.thread_id AND message_threads.deleted_at IS NULL").
		Joins("LEFT JOIN message_read_statuses ON message_read_statuses.message_id = messages.id AND message_read_statuses.user_id = ? AND message_read_statuses.deleted_at IS NULL", uid).
		Where("message_participants.user_id = ? AND message_participants.is_active = ?", uid, true).
		Where("message_threads.organization_id = ?", oid).
		Where("messages.is_deleted = ? AND messages.sender_id <> ? AND message_read_statuses.id IS NULL", false, uid).
		Count(&count).Error

	if err != nil {
		log.Printf("messaging: failed to get unread count: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to get unread count"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"count": count,
		},
	})
}

// protectedColumns can never be changed through an update request.
var protectedColumns = map[string]bool{
	"id": true, "organization_id": true, "provider": true,
	"created_at": true, "updated_at": true, "deleted_at": true,
}

// bindWithColumns binds the JSON body into dst and returns the DB column names
// for the fields present in the body, so explicit false/zero values are
// persisted by Select(cols).Updates(dst).
func bindWithColumns(c *gin.Context, db *gorm.DB, dst interface{}) ([]string, error) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("request body is required")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(dst); err != nil {
		return nil, err
	}
	cols := []string{}
	for _, f := range stmt.Schema.Fields {
		if f.DBName == "" || protectedColumns[f.DBName] {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" {
			name = f.DBName
		}
		if _, ok := raw[name]; ok {
			cols = append(cols, f.DBName)
		}
	}
	return cols, nil
}
