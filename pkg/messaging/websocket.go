package messaging

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
)

const wsWriteTimeout = 10 * time.Second

// wsClient is one WebSocket connection. Writes are serialised with mu because
// gorilla/websocket allows only one concurrent writer per connection.
type wsClient struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	userID string
	orgID  string
}

func (cl *wsClient) send(payload []byte) error {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	_ = cl.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return cl.conn.WriteMessage(websocket.TextMessage, payload)
}

func (cl *wsClient) sendJSON(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return cl.send(b)
}

func (cl *wsClient) identity() (string, string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.userID, cl.orgID
}

func (cl *wsClient) setIdentity(uid, oid string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.userID, cl.orgID = uid, oid
}

// wsEnvelope is a broadcast addressed to a set of user IDs. Only
// authenticated clients whose user ID is in the set receive it.
type wsEnvelope struct {
	payload []byte
	userIDs map[string]bool
}

type WebSocketHub struct {
	mu      sync.RWMutex
	clients map[*wsClient]bool
}

func newHub() *WebSocketHub {
	return &WebSocketHub{clients: make(map[*wsClient]bool)}
}

func (h *WebSocketHub) add(cl *wsClient) {
	h.mu.Lock()
	h.clients[cl] = true
	h.mu.Unlock()
	log.Println("Client connected")
}

func (h *WebSocketHub) remove(cl *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[cl]; ok {
		delete(h.clients, cl)
		cl.conn.Close()
		log.Println("Client disconnected")
	}
	h.mu.Unlock()
}

// deliver sends the envelope to matching clients. It never blocks the caller
// for longer than the per-connection write timeout.
func (h *WebSocketHub) deliver(env wsEnvelope) {
	if len(env.userIDs) == 0 {
		return
	}
	h.mu.RLock()
	targets := make([]*wsClient, 0)
	for cl := range h.clients {
		uid, _ := cl.identity()
		if uid != "" && env.userIDs[uid] {
			targets = append(targets, cl)
		}
	}
	h.mu.RUnlock()

	for _, cl := range targets {
		if err := cl.send(env.payload); err != nil {
			log.Printf("Error writing message: %v", err)
			h.remove(cl)
		}
	}
}

// broadcastToUsers marshals msg and delivers it asynchronously to the given
// users' connections.
func (h *Handler) broadcastToUsers(msg WebSocketMessage, userIDs []string) {
	if len(userIDs) == 0 {
		return
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal websocket message: %v", err)
		return
	}
	set := make(map[string]bool, len(userIDs))
	for _, id := range userIDs {
		if id != "" {
			set[id] = true
		}
	}
	go h.hub.deliver(wsEnvelope{payload: payload, userIDs: set})
}

// threadParticipantIDs returns the active participants' user IDs for a thread.
func (h *Handler) threadParticipantIDs(threadID uint) []string {
	var ids []string
	h.DB.Table("message_participants").
		Where("thread_id = ? AND is_active = ? AND deleted_at IS NULL", threadID, true).
		Pluck("user_id", &ids)
	return ids
}

var errInvalidToken = errors.New("invalid token")

// parseToken validates an access token and returns the user and org IDs.
func (h *Handler) parseToken(tokenString string) (string, string, error) {
	tokenString = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tokenString), "Bearer "))
	if tokenString == "" || h.jwtSecret == "" {
		return "", "", errInvalidToken
	}
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(h.jwtSecret), nil
	})
	if err != nil || token == nil || !token.Valid {
		return "", "", errInvalidToken
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", "", errInvalidToken
	}
	if t, _ := claims["type"].(string); t == "refresh" {
		return "", "", errInvalidToken
	}
	uid := claimString(claims["user_id"])
	oid := claimString(claims["org_id"])
	if oid == "" {
		oid = claimString(claims["organization_id"])
	}
	if uid == "" {
		return "", "", errInvalidToken
	}
	return uid, oid, nil
}

func claimString(v interface{}) string {
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if ok {
		return strings.TrimSpace(s)
	}
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

// tokenFromRequest reads a token from the Authorization header or the
// ?token= query parameter (browsers cannot set headers on WebSocket upgrades).
func tokenFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		return auth
	}
	return r.URL.Query().Get("token")
}

// WebSocket connection handler. The client authenticates either during the
// upgrade (Authorization header or ?token=) or with a first message
// {"type":"auth","token":"<jwt>"}. Client-supplied user IDs are never trusted.
func (h *Handler) HandleWebSocket(c *gin.Context) {
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection: %v", err)
		return
	}

	cl := &wsClient{conn: conn}
	if tok := tokenFromRequest(c.Request); tok != "" {
		if uid, oid, err := h.parseToken(tok); err == nil {
			cl.setIdentity(uid, oid)
		}
	}

	h.hub.add(cl)
	defer h.hub.remove(cl)

	if uid, _ := cl.identity(); uid != "" {
		_ = cl.sendJSON(WebSocketMessage{Type: "auth_success", UserID: uid, Data: gin.H{"authenticated": true}})
	}

	for {
		var msg struct {
			Type     string `json:"type"`
			ThreadID uint   `json:"thread_id"`
			Token    string `json:"token"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("Error reading JSON: %v", err)
			}
			break
		}

		if msg.Type == "auth" {
			uid, oid, err := h.parseToken(msg.Token)
			if err != nil {
				_ = cl.sendJSON(WebSocketMessage{Type: "auth_error", Data: gin.H{"error": "Invalid or missing token"}})
				continue
			}
			cl.setIdentity(uid, oid)
			_ = cl.sendJSON(WebSocketMessage{Type: "auth_success", UserID: uid, Data: gin.H{"authenticated": true}})
			continue
		}

		uid, _ := cl.identity()
		if uid == "" {
			_ = cl.sendJSON(WebSocketMessage{Type: "auth_error", Data: gin.H{"error": "Unauthorized"}})
			continue
		}

		switch msg.Type {
		case "ping":
			_ = cl.sendJSON(WebSocketMessage{Type: "pong"})
		case "join_thread", "leave_thread":
			// Delivery is addressed by participant membership; nothing to do.
		case "typing_start":
			h.handleTypingIndicator(msg.ThreadID, uid, true)
		case "typing_stop":
			h.handleTypingIndicator(msg.ThreadID, uid, false)
		}
	}
}
