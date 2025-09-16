package video

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// WebRTCSignalingServer handles WebRTC signaling for video calls
type WebRTCSignalingServer struct {
	DB             *gorm.DB
	connections    map[string]*WebSocketConnection
	rooms          map[string]*Room
	upgrader       websocket.Upgrader
	connectionsMux sync.RWMutex
	roomsMux       sync.RWMutex
}

// WebSocketConnection represents a WebSocket connection for signaling
type WebSocketConnection struct {
	ID             string
	UserID         uint
	OrganizationID uint
	Conn           *websocket.Conn
	Room           *Room
	IsHost         bool
	LastPing       time.Time
	Mutex          sync.Mutex
}

// Room represents a video call room
type Room struct {
	ID           string
	CallID       string
	HostID       uint
	Connections  map[string]*WebSocketConnection
	IsActive     bool
	CreatedAt    time.Time
	MaxPeers     int
	IsRecording  bool
	Mutex        sync.RWMutex
}

// SignalingMessage represents a WebRTC signaling message
type SignalingMessage struct {
	Type      string      `json:"type"`
	Data      interface{} `json:"data"`
	FromUser  uint        `json:"from_user"`
	ToUser    uint        `json:"to_user,omitempty"`
	RoomID    string      `json:"room_id,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
}

// WebRTC message types
const (
	MessageTypeOffer          = "offer"
	MessageTypeAnswer         = "answer"
	MessageTypeICECandidate   = "ice-candidate"
	MessageTypeJoinRoom       = "join-room"
	MessageTypeLeaveRoom      = "leave-room"
	MessageTypeUserJoined     = "user-joined"
	MessageTypeUserLeft       = "user-left"
	MessageTypeMuteAudio      = "mute-audio"
	MessageTypeMuteVideo      = "mute-video"
	MessageTypeScreenShare    = "screen-share"
	MessageTypeStopScreenShare = "stop-screen-share"
	MessageTypeStartRecording = "start-recording"
	MessageTypeStopRecording  = "stop-recording"
	MessageTypeCallEnd        = "call-end"
	MessageTypeError          = "error"
	MessageTypePing           = "ping"
	MessageTypePong           = "pong"
)

// NewWebRTCSignalingServer creates a new WebRTC signaling server
func NewWebRTCSignalingServer(db *gorm.DB) *WebRTCSignalingServer {
	server := &WebRTCSignalingServer{
		DB:          db,
		connections: make(map[string]*WebSocketConnection),
		rooms:       make(map[string]*Room),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins in development
			},
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
	}

	// Start cleanup goroutine for inactive connections
	go server.cleanupInactiveConnections()

	return server
}

// HandleWebRTCConnection handles WebSocket connections for WebRTC signaling
func (s *WebRTCSignalingServer) HandleWebRTCConnection(c *gin.Context) {
	// For WebSocket connections, extract token from query parameter
	token := c.Query("token")
	log.Printf("WebSocket connection attempt with token: %s", token)

	if token == "" {
		log.Printf("No token provided")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication token required"})
		return
	}

	// For development/testing, accept mock tokens
	var userID, organizationID uint
	if token == "mock-jwt-token-for-testing" {
		userID = 1
		organizationID = 1
		log.Printf("Accepted mock token, userID: %d, orgID: %d", userID, organizationID)
	} else {
		// TODO: Implement proper JWT token validation here
		// For now, use mock values for development
		userID = 1
		organizationID = 1
		log.Printf("Using default values for token: %s, userID: %d, orgID: %d", token, userID, organizationID)
	}

	if userID == 0 || organizationID == 0 {
		log.Printf("Invalid userID or organizationID")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid authentication token"})
		return
	}

	conn, err := s.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	connectionID := uuid.New().String()
	wsConn := &WebSocketConnection{
		ID:             connectionID,
		UserID:         userID,
		OrganizationID: organizationID,
		Conn:           conn,
		LastPing:       time.Now(),
	}

	s.connectionsMux.Lock()
	s.connections[connectionID] = wsConn
	s.connectionsMux.Unlock()

	log.Printf("WebRTC connection established: %s for user %d", connectionID, userID)

	// Handle incoming messages
	go s.handleConnection(wsConn)
}

// handleConnection handles messages from a WebSocket connection
func (s *WebRTCSignalingServer) handleConnection(conn *WebSocketConnection) {
	defer func() {
		s.removeConnection(conn)
		conn.Conn.Close()
	}()

	// Set connection timeouts
	conn.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.Conn.SetPongHandler(func(string) error {
		conn.LastPing = time.Now()
		conn.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		var message SignalingMessage
		err := conn.Conn.ReadJSON(&message)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		message.FromUser = conn.UserID
		message.Timestamp = time.Now()

		s.handleMessage(conn, &message)
	}
}

// handleMessage processes incoming signaling messages
func (s *WebRTCSignalingServer) handleMessage(conn *WebSocketConnection, message *SignalingMessage) {
	switch message.Type {
	case MessageTypeJoinRoom:
		s.handleJoinRoom(conn, message)
	case MessageTypeLeaveRoom:
		s.handleLeaveRoom(conn, message)
	case MessageTypeOffer, MessageTypeAnswer, MessageTypeICECandidate:
		s.handleSignalingMessage(conn, message)
	case MessageTypeMuteAudio, MessageTypeMuteVideo:
		s.handleMediaControl(conn, message)
	case MessageTypeScreenShare, MessageTypeStopScreenShare:
		s.handleScreenShare(conn, message)
	case MessageTypeStartRecording, MessageTypeStopRecording:
		s.handleRecording(conn, message)
	case MessageTypeCallEnd:
		s.handleCallEnd(conn, message)
	case MessageTypePing:
		s.handlePing(conn)
	default:
		log.Printf("Unknown message type: %s", message.Type)
	}
}

// handleJoinRoom handles room join requests
func (s *WebRTCSignalingServer) handleJoinRoom(conn *WebSocketConnection, message *SignalingMessage) {
	data, ok := message.Data.(map[string]interface{})
	if !ok {
		s.sendError(conn, "Invalid join room data")
		return
	}

	roomID, ok := data["room_id"].(string)
	if !ok {
		s.sendError(conn, "Room ID required")
		return
	}

	callID, _ := data["call_id"].(string)

	s.roomsMux.Lock()
	defer s.roomsMux.Unlock()

	room, exists := s.rooms[roomID]
	if !exists {
		// Create new room
		room = &Room{
			ID:          roomID,
			CallID:      callID,
			HostID:      conn.UserID,
			Connections: make(map[string]*WebSocketConnection),
			IsActive:    true,
			CreatedAt:   time.Now(),
			MaxPeers:    10, // Default max peers
		}
		s.rooms[roomID] = room
		conn.IsHost = true
	}

	// Check room capacity
	if len(room.Connections) >= room.MaxPeers {
		s.sendError(conn, "Room is full")
		return
	}

	// Add connection to room
	room.Mutex.Lock()
	room.Connections[conn.ID] = conn
	conn.Room = room
	room.Mutex.Unlock()

	// Notify existing participants about new user
	s.broadcastToRoom(room, &SignalingMessage{
		Type:     MessageTypeUserJoined,
		FromUser: conn.UserID,
		Data: map[string]interface{}{
			"user_id":   conn.UserID,
			"is_host":   conn.IsHost,
			"room_id":   roomID,
			"peer_count": len(room.Connections),
		},
	}, conn.ID)

	// Send room info to new participant
	s.sendMessage(conn, &SignalingMessage{
		Type: "room-joined",
		Data: map[string]interface{}{
			"room_id":    roomID,
			"is_host":    conn.IsHost,
			"peer_count": len(room.Connections),
		},
	})

	log.Printf("User %d joined room %s", conn.UserID, roomID)
}

// handleLeaveRoom handles room leave requests
func (s *WebRTCSignalingServer) handleLeaveRoom(conn *WebSocketConnection, message *SignalingMessage) {
	if conn.Room == nil {
		return
	}

	room := conn.Room
	room.Mutex.Lock()
	delete(room.Connections, conn.ID)
	connectionCount := len(room.Connections)
	room.Mutex.Unlock()

	// Notify other participants
	s.broadcastToRoom(room, &SignalingMessage{
		Type:     MessageTypeUserLeft,
		FromUser: conn.UserID,
		Data: map[string]interface{}{
			"user_id":    conn.UserID,
			"peer_count": connectionCount,
		},
	}, "")

	conn.Room = nil

	// Remove room if empty
	if connectionCount == 0 {
		s.roomsMux.Lock()
		delete(s.rooms, room.ID)
		s.roomsMux.Unlock()
		log.Printf("Room %s deleted (empty)", room.ID)
	}

	log.Printf("User %d left room %s", conn.UserID, room.ID)
}

// handleSignalingMessage handles WebRTC signaling messages (offer, answer, ICE candidates)
func (s *WebRTCSignalingServer) handleSignalingMessage(conn *WebSocketConnection, message *SignalingMessage) {
	if conn.Room == nil {
		s.sendError(conn, "Not in a room")
		return
	}

	// Store signaling data in database for reliability
	signal := models.WebRTCSignal{
		SessionID:  conn.Room.ID,
		FromUserID: conn.UserID,
		ToUserID:   message.ToUser,
		Type:       message.Type,
	}

	dataJSON, _ := json.Marshal(message.Data)
	signal.Data = string(dataJSON)

	if err := s.DB.Create(&signal).Error; err != nil {
		log.Printf("Failed to store signaling data: %v", err)
	}

	// Forward message to target user or broadcast to room
	if message.ToUser > 0 {
		s.sendToUser(conn.Room, message.ToUser, message)
	} else {
		s.broadcastToRoom(conn.Room, message, conn.ID)
	}
}

// handleMediaControl handles audio/video mute/unmute
func (s *WebRTCSignalingServer) handleMediaControl(conn *WebSocketConnection, message *SignalingMessage) {
	if conn.Room == nil {
		return
	}

	// Broadcast media control to all participants
	s.broadcastToRoom(conn.Room, message, "")
	log.Printf("User %d %s in room %s", conn.UserID, message.Type, conn.Room.ID)
}

// handleScreenShare handles screen sharing start/stop
func (s *WebRTCSignalingServer) handleScreenShare(conn *WebSocketConnection, message *SignalingMessage) {
	if conn.Room == nil {
		return
	}

	// Create or update screen share session
	if message.Type == MessageTypeScreenShare {
		screenShare := models.ScreenShare{
			SessionID:      uuid.New().String(),
			HostID:         conn.UserID,
			CallID:         nil, // Link to call if available
			OrganizationID: conn.OrganizationID,
			Status:         "active",
			Quality:        "hd",
			AudioEnabled:   false,
			IsRecorded:     false,
			StartedAt:      time.Now(),
		}

		if err := s.DB.Create(&screenShare).Error; err != nil {
			log.Printf("Failed to create screen share session: %v", err)
		}
	}

	// Broadcast screen share event to all participants
	s.broadcastToRoom(conn.Room, message, "")
	log.Printf("User %d %s in room %s", conn.UserID, message.Type, conn.Room.ID)
}

// handleRecording handles recording start/stop
func (s *WebRTCSignalingServer) handleRecording(conn *WebSocketConnection, message *SignalingMessage) {
	if conn.Room == nil || !conn.IsHost {
		s.sendError(conn, "Only host can control recording")
		return
	}

	room := conn.Room
	if message.Type == MessageTypeStartRecording {
		room.IsRecording = true
		log.Printf("Recording started in room %s", room.ID)
	} else {
		room.IsRecording = false
		log.Printf("Recording stopped in room %s", room.ID)
	}

	// Broadcast recording status to all participants
	s.broadcastToRoom(room, message, "")
}

// handleCallEnd handles call termination
func (s *WebRTCSignalingServer) handleCallEnd(conn *WebSocketConnection, message *SignalingMessage) {
	if conn.Room == nil {
		return
	}

	room := conn.Room

	// Only host can end the call for everyone
	if conn.IsHost {
		s.broadcastToRoom(room, message, "")
		s.closeRoom(room)
	} else {
		// Regular participant just leaves
		s.handleLeaveRoom(conn, message)
	}
}

// handlePing handles ping messages for connection keepalive
func (s *WebRTCSignalingServer) handlePing(conn *WebSocketConnection) {
	conn.LastPing = time.Now()
	s.sendMessage(conn, &SignalingMessage{
		Type: MessageTypePong,
		Data: map[string]interface{}{
			"timestamp": time.Now().Unix(),
		},
	})
}

// sendMessage sends a message to a specific connection
func (s *WebRTCSignalingServer) sendMessage(conn *WebSocketConnection, message *SignalingMessage) {
	conn.Mutex.Lock()
	defer conn.Mutex.Unlock()

	if err := conn.Conn.WriteJSON(message); err != nil {
		log.Printf("Failed to send message to connection %s: %v", conn.ID, err)
	}
}

// sendToUser sends a message to a specific user in a room
func (s *WebRTCSignalingServer) sendToUser(room *Room, userID uint, message *SignalingMessage) {
	room.Mutex.RLock()
	defer room.Mutex.RUnlock()

	for _, conn := range room.Connections {
		if conn.UserID == userID {
			s.sendMessage(conn, message)
			return
		}
	}
}

// broadcastToRoom broadcasts a message to all connections in a room
func (s *WebRTCSignalingServer) broadcastToRoom(room *Room, message *SignalingMessage, excludeConnectionID string) {
	room.Mutex.RLock()
	defer room.Mutex.RUnlock()

	for connID, conn := range room.Connections {
		if connID != excludeConnectionID {
			s.sendMessage(conn, message)
		}
	}
}

// sendError sends an error message to a connection
func (s *WebRTCSignalingServer) sendError(conn *WebSocketConnection, errorMessage string) {
	s.sendMessage(conn, &SignalingMessage{
		Type: MessageTypeError,
		Data: map[string]interface{}{
			"error": errorMessage,
		},
	})
}

// removeConnection removes a connection from the server
func (s *WebRTCSignalingServer) removeConnection(conn *WebSocketConnection) {
	s.connectionsMux.Lock()
	delete(s.connections, conn.ID)
	s.connectionsMux.Unlock()

	// Remove from room if in one
	if conn.Room != nil {
		s.handleLeaveRoom(conn, &SignalingMessage{Type: MessageTypeLeaveRoom})
	}

	log.Printf("Connection %s removed", conn.ID)
}

// closeRoom closes a room and removes all connections
func (s *WebRTCSignalingServer) closeRoom(room *Room) {
	room.Mutex.Lock()
	connections := make([]*WebSocketConnection, 0, len(room.Connections))
	for _, conn := range room.Connections {
		connections = append(connections, conn)
	}
	room.Mutex.Unlock()

	// Remove all connections from room
	for _, conn := range connections {
		conn.Room = nil
	}

	s.roomsMux.Lock()
	delete(s.rooms, room.ID)
	s.roomsMux.Unlock()

	log.Printf("Room %s closed", room.ID)
}

// cleanupInactiveConnections removes inactive connections periodically
func (s *WebRTCSignalingServer) cleanupInactiveConnections() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		s.connectionsMux.Lock()

		var toRemove []*WebSocketConnection
		for _, conn := range s.connections {
			if now.Sub(conn.LastPing) > 90*time.Second {
				toRemove = append(toRemove, conn)
			}
		}

		s.connectionsMux.Unlock()

		// Remove inactive connections
		for _, conn := range toRemove {
			log.Printf("Removing inactive connection: %s", conn.ID)
			s.removeConnection(conn)
			conn.Conn.Close()
		}
	}
}

// GetRoomInfo returns information about a room
func (s *WebRTCSignalingServer) GetRoomInfo(roomID string) map[string]interface{} {
	s.roomsMux.RLock()
	defer s.roomsMux.RUnlock()

	room, exists := s.rooms[roomID]
	if !exists {
		return nil
	}

	room.Mutex.RLock()
	defer room.Mutex.RUnlock()

	participants := make([]map[string]interface{}, 0, len(room.Connections))
	for _, conn := range room.Connections {
		participants = append(participants, map[string]interface{}{
			"user_id":     conn.UserID,
			"connection_id": conn.ID,
			"is_host":     conn.IsHost,
		})
	}

	return map[string]interface{}{
		"room_id":      room.ID,
		"call_id":      room.CallID,
		"host_id":      room.HostID,
		"is_active":    room.IsActive,
		"is_recording": room.IsRecording,
		"created_at":   room.CreatedAt,
		"participant_count": len(room.Connections),
		"participants": participants,
	}
}