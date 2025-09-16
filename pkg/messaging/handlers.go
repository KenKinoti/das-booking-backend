package messaging

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

type Handler struct {
	DB       *gorm.DB
	upgrader websocket.Upgrader
	clients  map[string]*websocket.Conn
	hub      *WebSocketHub
}

type WebSocketHub struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan []byte
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
}

type WebSocketMessage struct {
	Type     string      `json:"type"`
	ThreadID uint        `json:"thread_id,omitempty"`
	UserID   uint        `json:"user_id,omitempty"`
	Data     interface{} `json:"data"`
}

func NewHandler(db *gorm.DB) *Handler {
	hub := &WebSocketHub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
	}

	go hub.run()

	return &Handler{
		DB: db,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for development
			},
		},
		clients: make(map[string]*websocket.Conn),
		hub:     hub,
	}
}

func (h *WebSocketHub) run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Println("Client connected")

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.Close()
				log.Println("Client disconnected")
			}

		case message := <-h.broadcast:
			for client := range h.clients {
				select {
				case err := <-func() chan error {
					errCh := make(chan error, 1)
					go func() {
						defer close(errCh)
						errCh <- client.WriteMessage(websocket.TextMessage, message)
					}()
					return errCh
				}():
					if err != nil {
						log.Printf("Error writing message: %v", err)
						client.Close()
						delete(h.clients, client)
					}
				default:
					close := make(chan struct{})
					go func() {
						defer func() { close <- struct{}{} }()
					}()
					client.Close()
					delete(h.clients, client)
				}
			}
		}
	}
}

// WebSocket connection handler
func (h *Handler) HandleWebSocket(c *gin.Context) {
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection: %v", err)
		return
	}

	h.hub.register <- conn

	defer func() {
		h.hub.unregister <- conn
	}()

	for {
		var msg WebSocketMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			log.Printf("Error reading JSON: %v", err)
			break
		}

		// Handle different message types
		switch msg.Type {
		case "join_thread":
			// Handle joining a thread
		case "leave_thread":
			// Handle leaving a thread
		case "typing_start":
			h.handleTypingIndicator(msg.ThreadID, msg.UserID, true)
		case "typing_stop":
			h.handleTypingIndicator(msg.ThreadID, msg.UserID, false)
		}
	}
}

// Get all threads for a user
func (h *Handler) GetThreads(c *gin.Context) {
	userID := c.GetUint("user_id") // Assumes user ID is set by auth middleware
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var threads []models.MessageThread
	err := h.DB.Joins("JOIN message_participants ON message_participants.thread_id = message_threads.id").
		Where("message_participants.user_id = ? AND message_participants.is_active = ?", userID, true).
		Preload("Participants.User").
		Preload("Messages", func(db *gorm.DB) *gorm.DB {
			return db.Order("created_at DESC").Limit(1)
		}).
		Order("message_threads.last_message_at DESC").
		Find(&threads).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch threads"})
		return
	}

	// Add unread count for each thread
	for i := range threads {
		var unreadCount int64
		h.DB.Model(&models.Message{}).
			Joins("LEFT JOIN message_read_status ON message_read_status.message_id = messages.id AND message_read_status.user_id = ?", userID).
			Where("messages.thread_id = ? AND message_read_status.id IS NULL AND messages.sender_id != ?", threads[i].ID, userID).
			Count(&unreadCount)

		// Add unread count to thread data (you might want to add this field to the model)
		threads[i].Messages = append(threads[i].Messages, models.Message{
			Content: fmt.Sprintf("%d", unreadCount), // Temporary way to pass unread count
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    threads,
	})
}

// Create a new thread
func (h *Handler) CreateThread(c *gin.Context) {
	userID := c.GetUint("user_id")
	orgID := c.GetUint("organization_id")

	var request struct {
		Name         string `json:"name"`
		Type         string `json:"type" binding:"required"`
		ParticipantIDs []uint `json:"participant_ids" binding:"required"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Create thread
	thread := models.MessageThread{
		Name:           request.Name,
		Type:           request.Type,
		CreatedBy:      userID,
		OrganizationID: orgID,
		IsActive:       true,
		LastMessageAt:  nil,
	}

	if err := h.DB.Create(&thread).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create thread"})
		return
	}

	// Add participants
	participants := []models.MessageParticipant{}

	// Add creator as admin
	participants = append(participants, models.MessageParticipant{
		ThreadID:  thread.ID,
		UserID:    userID,
		Role:      "admin",
		IsActive:  true,
		JoinedAt:  time.Now(),
	})

	// Add other participants
	for _, participantID := range request.ParticipantIDs {
		if participantID != userID {
			participants = append(participants, models.MessageParticipant{
				ThreadID:  thread.ID,
				UserID:    participantID,
				Role:      "member",
				IsActive:  true,
				JoinedAt:  time.Now(),
			})
		}
	}

	if err := h.DB.Create(&participants).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add participants"})
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
	threadID, _ := strconv.ParseUint(c.Param("threadId"), 10, 32)
	userID := c.GetUint("user_id")

	// Verify user is participant in thread
	var participant models.MessageParticipant
	if err := h.DB.Where("thread_id = ? AND user_id = ? AND is_active = ?", threadID, userID, true).
		First(&participant).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return
	}

	// Get pagination parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset := (page - 1) * limit

	var messages []models.Message
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch messages"})
		return
	}

	// Mark messages as read
	go h.markMessagesAsRead(uint(threadID), userID)

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
	threadID, _ := strconv.ParseUint(c.Param("threadId"), 10, 32)
	userID := c.GetUint("user_id")

	var request struct {
		Content     string `json:"content" binding:"required"`
		MessageType string `json:"message_type"`
		ReplyToID   *uint  `json:"reply_to_id"`
		Metadata    string `json:"metadata"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify user is participant in thread
	var participant models.MessageParticipant
	if err := h.DB.Where("thread_id = ? AND user_id = ? AND is_active = ?", threadID, userID, true).
		First(&participant).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return
	}

	// Create message
	message := models.Message{
		ThreadID:    uint(threadID),
		SenderID:    userID,
		Content:     request.Content,
		MessageType: request.MessageType,
		ReplyToID:   request.ReplyToID,
		Metadata:    request.Metadata,
		IsForwarded: false,
		IsEdited:    false,
		IsDeleted:   false,
	}

	if request.MessageType == "" {
		message.MessageType = "text"
	}

	if err := h.DB.Create(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send message"})
		return
	}

	// Update thread last message time
	h.DB.Model(&models.MessageThread{}).Where("id = ?", threadID).
		Updates(map[string]interface{}{
			"last_message_at": time.Now(),
		})

	// Load message with sender info
	h.DB.Preload("Sender").Preload("ReplyTo.Sender").First(&message, message.ID)

	// Broadcast message via WebSocket
	wsMessage := WebSocketMessage{
		Type:     "new_message",
		ThreadID: uint(threadID),
		Data:     message,
	}
	messageJSON, _ := json.Marshal(wsMessage)
	h.hub.broadcast <- messageJSON

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    message,
	})
}

// Mark messages as read
func (h *Handler) markMessagesAsRead(threadID, userID uint) {
	// Get unread messages for this user in this thread
	var messageIDs []uint
	h.DB.Model(&models.Message{}).
		Joins("LEFT JOIN message_read_status ON message_read_status.message_id = messages.id AND message_read_status.user_id = ?", userID).
		Where("messages.thread_id = ? AND message_read_status.id IS NULL AND messages.sender_id != ?", threadID, userID).
		Pluck("messages.id", &messageIDs)

	// Create read status records
	var readStatuses []models.MessageReadStatus
	for _, messageID := range messageIDs {
		readStatuses = append(readStatuses, models.MessageReadStatus{
			MessageID: messageID,
			UserID:    userID,
			ReadAt:    time.Now(),
		})
	}

	if len(readStatuses) > 0 {
		h.DB.Create(&readStatuses)

		// Reset unread count for participant
		h.DB.Model(&models.MessageParticipant{}).
			Where("thread_id = ? AND user_id = ?", threadID, userID).
			Update("unread_count", 0)
	}
}

// Handle typing indicator
func (h *Handler) handleTypingIndicator(threadID, userID uint, isTyping bool) {
	var indicator models.TypingIndicator
	result := h.DB.Where("thread_id = ? AND user_id = ?", threadID, userID).First(&indicator)

	if result.Error != nil {
		// Create new indicator
		indicator = models.TypingIndicator{
			ThreadID: threadID,
			UserID:   userID,
			IsTyping: isTyping,
		}
		if isTyping {
			now := time.Now()
			indicator.StartedAt = &now
		}
		h.DB.Create(&indicator)
	} else {
		// Update existing indicator
		updates := map[string]interface{}{
			"is_typing": isTyping,
		}
		if isTyping {
			now := time.Now()
			updates["started_at"] = &now
		}
		h.DB.Model(&indicator).Updates(updates)
	}

	// Broadcast typing indicator
	wsMessage := WebSocketMessage{
		Type:     "typing_indicator",
		ThreadID: threadID,
		UserID:   userID,
		Data: gin.H{
			"is_typing": isTyping,
			"user_id":   userID,
		},
	}
	messageJSON, _ := json.Marshal(wsMessage)
	h.hub.broadcast <- messageJSON
}

// Get message settings for organization
func (h *Handler) GetMessageSettings(c *gin.Context) {
	orgID := c.GetUint("organization_id")

	var settings models.MessageSettings
	result := h.DB.Where("organization_id = ?", orgID).First(&settings)

	if result.Error != nil {
		// Create default settings
		settings = models.MessageSettings{
			OrganizationID:         orgID,
			EnableInAppMessaging:   true,
			EnableGroupChats:       true,
			EnableFileSharing:      true,
			MaxFileSize:           10485760, // 10MB
			AllowedFileTypes:      `["jpg", "jpeg", "png", "gif", "pdf", "doc", "docx", "txt"]`,
			MessageRetentionDays:   365,
			EnableMessageReactions: true,
			EnableTypingIndicator:  true,
			EnableReadReceipts:     true,
			EnableVoiceMessages:    true,
			EnableVideoMessages:    true,
			ModerationEnabled:      false,
			ProfanityFilterEnabled: false,
		}
		h.DB.Create(&settings)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    settings,
	})
}

// Update message settings
func (h *Handler) UpdateMessageSettings(c *gin.Context) {
	orgID := c.GetUint("organization_id")

	var request models.MessageSettings
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var settings models.MessageSettings
	result := h.DB.Where("organization_id = ?", orgID).First(&settings)

	if result.Error != nil {
		// Create new settings
		request.OrganizationID = orgID
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

// Get message integrations
func (h *Handler) GetMessageIntegrations(c *gin.Context) {
	orgID := c.GetUint("organization_id")

	var integrations []models.MessageIntegration
	err := h.DB.Where("organization_id = ?", orgID).Find(&integrations).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch integrations"})
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
	orgID := c.GetUint("organization_id")
	provider := c.Param("provider")

	var request models.MessageIntegration
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var integration models.MessageIntegration
	result := h.DB.Where("organization_id = ? AND provider = ?", orgID, provider).First(&integration)

	if result.Error != nil {
		// Create new integration
		request.OrganizationID = orgID
		request.Provider = provider
		if err := h.DB.Create(&request).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create integration"})
			return
		}
		integration = request
	} else {
		// Update existing integration
		if err := h.DB.Model(&integration).Updates(&request).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update integration"})
			return
		}
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
	userID := c.GetUint("user_id")
	query := c.Query("q")
	threadID := c.Query("thread_id")

	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Search query is required"})
		return
	}

	db := h.DB.Model(&models.Message{}).
		Joins("JOIN message_participants ON message_participants.thread_id = messages.thread_id").
		Where("message_participants.user_id = ? AND message_participants.is_active = ? AND messages.is_deleted = ?",
			userID, true, false).
		Where("messages.content ILIKE ?", "%"+query+"%").
		Preload("Sender").
		Preload("Thread")

	if threadID != "" {
		db = db.Where("messages.thread_id = ?", threadID)
	}

	var messages []models.Message
	err := db.Order("messages.created_at DESC").Limit(100).Find(&messages).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Search failed"})
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
	orgID := c.GetUint("organization_id")

	var request struct {
		To      string `json:"to" binding:"required"`
		Message string `json:"message" binding:"required"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get WhatsApp settings
	var settings models.MessageSettings
	if err := h.DB.Where("organization_id = ?", orgID).First(&settings).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "WhatsApp settings not found"})
		return
	}

	if settings.WhatsappAPIKey == "" || settings.WhatsappPhoneNumber == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "WhatsApp not configured"})
		return
	}

	// Initialize WhatsApp service
	whatsapp := NewWhatsAppService(&settings)

	// Send message
	response, err := whatsapp.SendMessage(request.To, request.Message)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    response,
	})
}

// Send WhatsApp template message
func (h *Handler) SendWhatsAppTemplate(c *gin.Context) {
	orgID := c.GetUint("organization_id")

	var request struct {
		To         string        `json:"to" binding:"required"`
		Template   string        `json:"template" binding:"required"`
		Language   string        `json:"language"`
		Components []interface{} `json:"components"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if request.Language == "" {
		request.Language = "en_US"
	}

	// Get WhatsApp settings
	var settings models.MessageSettings
	if err := h.DB.Where("organization_id = ?", orgID).First(&settings).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "WhatsApp settings not found"})
		return
	}

	if settings.WhatsappAPIKey == "" || settings.WhatsappPhoneNumber == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "WhatsApp not configured"})
		return
	}

	// Initialize WhatsApp service
	whatsapp := NewWhatsAppService(&settings)

	// Send template
	response, err := whatsapp.SendTemplate(request.To, request.Template, request.Language, request.Components)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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

	// Get WhatsApp settings for verification token
	var settings models.MessageSettings
	if err := h.DB.First(&settings).Error; err != nil {
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

	// Get WhatsApp settings
	var settings models.MessageSettings
	if err := h.DB.First(&settings).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load settings"})
		return
	}

	whatsapp := NewWhatsAppService(&settings)
	messages, err := whatsapp.ProcessWebhookEvent(&event)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Process each incoming message
	for _, msg := range messages {
		// Find or create thread based on external phone number from metadata
		var metadata map[string]string
		if err := json.Unmarshal([]byte(msg.Metadata), &metadata); err != nil {
			log.Printf("Failed to parse message metadata: %v", err)
			continue
		}

		phoneNumber := metadata["phone"]
		var thread models.MessageThread
		result := h.DB.Where("name = ? AND type = ?", phoneNumber, "whatsapp").First(&thread)

		if result.Error != nil {
			// Create new thread for WhatsApp conversation
			thread = models.MessageThread{
				Name:           phoneNumber,
				Type:           "whatsapp",
				OrganizationID: settings.OrganizationID,
			}
			h.DB.Create(&thread)
		}

		// Set thread ID and save message
		msg.ThreadID = thread.ID
		if err := h.DB.Create(msg).Error; err != nil {
			log.Printf("Failed to save WhatsApp message: %v", err)
			continue
		}

		// Broadcast message via WebSocket
		wsMessage := WebSocketMessage{
			Type:     "message",
			ThreadID: thread.ID,
			Data:     msg,
		}
		messageJSON, _ := json.Marshal(wsMessage)
		h.hub.broadcast <- messageJSON
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// Test WhatsApp connection
func (h *Handler) TestWhatsAppConnection(c *gin.Context) {
	orgID := c.GetUint("organization_id")

	// Get WhatsApp settings
	var settings models.MessageSettings
	if err := h.DB.Where("organization_id = ?", orgID).First(&settings).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "WhatsApp settings not found"})
		return
	}

	if settings.WhatsappAPIKey == "" || settings.WhatsappPhoneNumber == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "WhatsApp not configured"})
		return
	}

	// Initialize and test WhatsApp service
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

// Get unread message count for user
func (h *Handler) GetUnreadCount(c *gin.Context) {
	userID := c.GetUint("user_id")

	var count int64
	err := h.DB.Model(&models.MessageReadStatus{}).
		Joins("JOIN messages ON messages.id = message_read_statuses.message_id").
		Joins("JOIN message_participants ON message_participants.thread_id = messages.thread_id").
		Where("message_participants.user_id = ? AND message_participants.is_active = ? AND message_read_statuses.user_id = ? AND message_read_statuses.read_at IS NULL",
			userID, true, userID).
		Count(&count).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get unread count"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"count": count,
		},
	})
}