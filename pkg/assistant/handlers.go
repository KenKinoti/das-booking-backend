package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

// Rate limit: messages per user per window.
const (
	RateLimit  = 30
	RateWindow = 10 * time.Minute
)

type rateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

// allow records a hit for key and reports whether it is within the limit,
// plus the time until the next slot frees up when it is not.
func (r *rateLimiter) allow(key string, now time.Time, limit int, window time.Duration) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hits == nil {
		r.hits = map[string][]time.Time{}
	}
	cut := now.Add(-window)
	h := r.hits[key]
	i := 0
	for i < len(h) && !h[i].After(cut) {
		i++
	}
	h = h[i:]
	if len(h) >= limit {
		r.hits[key] = h
		return false, h[0].Sub(cut)
	}
	r.hits[key] = append(h, now)
	return true, 0
}

// Handler exposes the assistant over HTTP.
type Handler struct {
	Svc *Service
}

// RegisterRoutes mounts the endpoints on /api/v1/assistant.
func (h *Handler) RegisterRoutes(g *gin.RouterGroup) {
	if h.Svc.limiter == nil {
		h.Svc.limiter = &rateLimiter{}
	}
	g.GET("/status", h.status)
	g.GET("/settings", h.getSettings)
	g.PUT("/settings", h.putSettings)
	g.POST("/chat", h.chat)
	g.GET("/conversations", h.list)
	g.GET("/conversations/:id", h.get)
	g.PATCH("/conversations/:id", h.rename)
	g.DELETE("/conversations/:id", h.remove)
	g.POST("/conversations/:id/actions/:action_id", h.resolve)
}

func validTZ(tz string) string {
	tz = strings.TrimSpace(tz)
	if tz == "" || len(tz) > 64 {
		return ""
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return ""
	}
	return tz
}

// identity is the signed-in user. The time zone comes from the X-Timezone
// header (or the request body's "timezone", which avoids a CORS preflight
// header on cross-origin deployments).
func identity(c *gin.Context) aitools.Identity {
	return aitools.Identity{UserID: c.GetString("user_id"), OrgID: c.GetString("org_id"), Role: c.GetString("user_role"), Email: c.GetString("user_email"), TZ: validTZ(c.GetHeader("X-Timezone"))}
}

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func (h *Handler) usageSince(since time.Time, orgID string) gin.H {
	var row struct {
		In, Out, N int64
	}
	q := h.Svc.DB.Model(&Message{}).Select("COALESCE(SUM(input_tokens),0) AS \"in\", COALESCE(SUM(output_tokens),0) AS \"out\", COUNT(*) AS n").
		Where("role = ? AND created_at >= ?", "assistant", since)
	if orgID != "" {
		q = q.Where("org_id = ?", orgID)
	}
	q.Scan(&row)
	return gin.H{"input_tokens": row.In, "output_tokens": row.Out, "responses": row.N}
}

func (h *Handler) status(c *gin.Context) {
	who := identity(c)
	s := h.Svc
	out := gin.H{
		"enabled":     s.Enabled(),
		"model":       s.Cfg.Model,
		"tools_count": len(s.tools()),
		"rate_limit":  gin.H{"limit": RateLimit, "window_seconds": int(RateWindow.Seconds())},
		"super_admin": who.Role == "super_admin",
	}
	if !s.Enabled() {
		out["reason"] = "ANTHROPIC_API_KEY is not set on the backend."
		if who.Role == "super_admin" {
			out["setup"] = "Set the ANTHROPIC_API_KEY environment variable on the backend service (optionally ASSISTANT_MODEL) and restart it."
		} else {
			out["setup"] = "Ask your administrator to enable the AI assistant."
		}
	} else if len(s.tools()) == 0 {
		out["warning"] = "No AI tools are registered, so the assistant cannot read or change records yet."
	}
	var st UserSettings
	s.DB.Where("user_id = ?", who.UserID).First(&st)
	out["settings"] = gin.H{"auto_approve": st.AutoApprove}
	if who.Role == "super_admin" {
		y, m, d := time.Now().UTC().Date()
		day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		out["usage_today"] = gin.H{"platform": h.usageSince(day, ""), "organization": h.usageSince(day, who.OrgID), "since": day}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
}

func (h *Handler) getSettings(c *gin.Context) {
	who := identity(c)
	var st UserSettings
	h.Svc.DB.Where("user_id = ?", who.UserID).First(&st)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"auto_approve": st.AutoApprove}})
}

func (h *Handler) putSettings(c *gin.Context) {
	who := identity(c)
	var body struct {
		AutoApprove *bool `json:"auto_approve"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.AutoApprove == nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "auto_approve (boolean) is required")
		return
	}
	st := UserSettings{UserID: who.UserID, OrgID: who.OrgID, AutoApprove: *body.AutoApprove, UpdatedAt: time.Now()}
	if err := h.Svc.DB.Save(&st).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB", "Could not save settings")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"auto_approve": st.AutoApprove}})
}

// sse writes server-sent events.
type sse struct {
	mu     sync.Mutex
	c      *gin.Context
	closed bool
}

func startSSE(c *gin.Context) *sse {
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	w.Flush()
	return &sse{c: c}
}

func (s *sse) send(event string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	b, err := json.Marshal(data)
	if err != nil {
		b = []byte(`{}`)
	}
	if _, err := fmt.Fprintf(s.c.Writer, "event: %s\ndata: %s\n\n", event, b); err != nil {
		s.closed = true
		return
	}
	s.c.Writer.Flush()
}

func (s *sse) ping() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if _, err := fmt.Fprint(s.c.Writer, ": ping\n\n"); err != nil {
		s.closed = true
		return
	}
	s.c.Writer.Flush()
}

// stream runs fn while streaming its events, with keep-alive pings.
func stream(c *gin.Context, fn func(emit Emit) error) {
	out := startSSE(c)
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				out.ping()
			}
		}
	}()
	done := false
	emit := func(event string, data any) {
		if event == "done" {
			done = true
		}
		out.send(event, data)
	}
	err := fn(emit)
	close(stop)
	if err != nil {
		if c.Request.Context().Err() != nil {
			return
		}
		msg := err.Error()
		retry := true
		var ae *APIError
		switch {
		case errors.As(err, &ae):
			msg = ae.Friendly()
			retry = ae.Status != 401 && ae.Status != 403
		case errors.Is(err, ErrBusy), errors.Is(err, ErrNotFound):
			retry = false
		case strings.Contains(msg, "no longer waiting"), strings.Contains(msg, "nothing to retry"), strings.Contains(msg, "not configured"):
			retry = false
		case strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host"):
			msg = "Could not reach the AI service. Please try again."
		}
		out.send("error", gin.H{"message": msg, "retryable": retry})
		return
	}
	if !done {
		out.send("done", gin.H{})
	}
}

func (h *Handler) chat(c *gin.Context) {
	who := identity(c)
	if who.UserID == "" || who.OrgID == "" {
		fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Sign in again to use the assistant")
		return
	}
	var body struct {
		ConversationID string `json:"conversation_id"`
		Message        string `json:"message"`
		Retry          bool   `json:"retry"`
		Page           Page   `json:"page"`
		Timezone       string `json:"timezone"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	if tz := validTZ(body.Timezone); tz != "" {
		who.TZ = tz
	}
	body.Message = strings.TrimSpace(body.Message)
	if !body.Retry && body.Message == "" {
		fail(c, http.StatusBadRequest, "VALIDATION", "Type a message first")
		return
	}
	if len([]rune(body.Message)) > 8000 {
		fail(c, http.StatusBadRequest, "VALIDATION", "That message is too long (max 8,000 characters)")
		return
	}
	if !h.Svc.Enabled() {
		fail(c, http.StatusServiceUnavailable, "ASSISTANT_DISABLED", "The AI assistant is not configured. Set ANTHROPIC_API_KEY on the backend.")
		return
	}
	if body.ConversationID != "" {
		if _, err := h.Svc.getConversation(who, body.ConversationID); err != nil {
			fail(c, http.StatusNotFound, "NOT_FOUND", "Conversation not found")
			return
		}
	}
	if ok, wait := h.Svc.limiter.allow(who.UserID, time.Now(), RateLimit, RateWindow); !ok {
		c.Header("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		fail(c, http.StatusTooManyRequests, "RATE_LIMITED", fmt.Sprintf("You've sent a lot of messages. Please wait about %d minute(s) and try again.", int(wait.Minutes())+1))
		return
	}
	stream(c, func(emit Emit) error {
		return h.Svc.Chat(c.Request.Context(), who, ChatInput{ConversationID: body.ConversationID, Text: body.Message, Retry: body.Retry, Page: body.Page}, emit)
	})
}

func (h *Handler) resolve(c *gin.Context) {
	who := identity(c)
	var body struct {
		Approve  bool   `json:"approve"`
		Page     Page   `json:"page"`
		Timezone string `json:"timezone"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request body")
		return
	}
	if tz := validTZ(body.Timezone); tz != "" {
		who.TZ = tz
	}
	conv, err := h.Svc.getConversation(who, c.Param("id"))
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Conversation not found")
		return
	}
	if p := conv.pending(); p == nil || p.Queue[0].ID != c.Param("action_id") {
		fail(c, http.StatusConflict, "NOT_PENDING", "This action is no longer waiting for confirmation")
		return
	}
	stream(c, func(emit Emit) error {
		return h.Svc.Resolve(c.Request.Context(), who, conv.ID, c.Param("action_id"), body.Approve, body.Page, emit)
	})
}

func (h *Handler) list(c *gin.Context) {
	who := identity(c)
	var convs []Conversation
	q := h.Svc.DB.Where("org_id = ? AND user_id = ?", who.OrgID, who.UserID)
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		q = q.Where("LOWER(title) LIKE ?", "%"+strings.ToLower(s)+"%")
	}
	if err := q.Order("last_message_at DESC").Limit(100).Find(&convs).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB", "Could not load conversations")
		return
	}
	items := make([]gin.H, 0, len(convs))
	for _, cv := range convs {
		items = append(items, gin.H{"id": cv.ID, "title": cv.Title, "created_at": cv.CreatedAt, "updated_at": cv.UpdatedAt, "last_message_at": cv.LastMessageAt, "has_pending": cv.pending() != nil})
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items})
}

func (h *Handler) get(c *gin.Context) {
	who := identity(c)
	conv, err := h.Svc.getConversation(who, c.Param("id"))
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Conversation not found")
		return
	}
	view, err := h.Svc.View(conv)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DB", "Could not load conversation")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": view})
}

func (h *Handler) rename(c *gin.Context) {
	who := identity(c)
	conv, err := h.Svc.getConversation(who, c.Param("id"))
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Conversation not found")
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	c.ShouldBindJSON(&body)
	t := strings.TrimSpace(strings.Join(strings.Fields(body.Title), " "))
	if t == "" {
		fail(c, http.StatusBadRequest, "VALIDATION", "Title is required")
		return
	}
	t = truncate(t, 120)
	if err := h.Svc.DB.Model(&Conversation{}).Where("id = ?", conv.ID).Updates(map[string]any{"title": t, "updated_at": time.Now()}).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB", "Could not rename conversation")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"id": conv.ID, "title": t}})
}

func (h *Handler) remove(c *gin.Context) {
	who := identity(c)
	conv, err := h.Svc.getConversation(who, c.Param("id"))
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Conversation not found")
		return
	}
	if err := h.Svc.DB.Where("conversation_id = ?", conv.ID).Delete(&Message{}).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB", "Could not delete conversation")
		return
	}
	h.Svc.DB.Where("id = ?", conv.ID).Delete(&Conversation{})
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// View flattens a conversation into display items.
func (s *Service) View(conv *Conversation) (gin.H, error) {
	ms, err := s.messages(conv.ID)
	if err != nil {
		return nil, err
	}
	items := []gin.H{}
	toolIdx := map[string]int{}
	applyMeta := func(id string, m ToolMeta) {
		if i, ok := toolIdx[id]; ok {
			items[i]["status"] = m.Status
			items[i]["summary"] = m.Summary
			items[i]["link"] = m.Link
			items[i]["write"] = m.Write
			items[i]["changed"] = m.Changed
		}
	}
	for _, m := range ms {
		var blocks []Block
		json.Unmarshal([]byte(m.Content), &blocks)
		var meta map[string]ToolMeta
		if m.Meta != "" {
			json.Unmarshal([]byte(m.Meta), &meta)
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				items = append(items, gin.H{"kind": m.Role, "id": m.ID, "text": b.Text, "created_at": m.CreatedAt})
			case "tool_use":
				t, _ := s.tool(b.Name)
				toolIdx[b.ID] = len(items)
				items = append(items, gin.H{"kind": "tool", "id": b.ID, "name": b.Name, "title": toolTitle(t, b.Name), "status": "skipped", "summary": toolTitle(t, b.Name), "write": !t.ReadOnly})
			case "tool_result":
				if mm, ok := meta[b.ToolUseID]; ok {
					applyMeta(b.ToolUseID, mm)
				} else if i, ok := toolIdx[b.ToolUseID]; ok {
					if b.IsError {
						items[i]["status"] = "error"
					} else {
						items[i]["status"] = "done"
					}
				}
			}
		}
	}
	var pending any
	if p := conv.pending(); p != nil {
		for id, m := range p.Meta {
			applyMeta(id, m)
		}
		for _, q := range p.Queue {
			if i, ok := toolIdx[q.ID]; ok {
				items[i]["status"] = "awaiting"
			}
		}
		t, _ := s.tool(p.Queue[0].Name)
		pending = describeAction(conv.ID, t, p.Queue[0])
	}
	// Retry is possible when the last stored message is the user's.
	canRetry := len(ms) > 0 && ms[len(ms)-1].Role == "user" && pending == nil
	return gin.H{
		"conversation": gin.H{"id": conv.ID, "title": conv.Title, "created_at": conv.CreatedAt, "last_message_at": conv.LastMessageAt},
		"items":        items,
		"pending":      pending,
		"can_retry":    canRetry,
	}, nil
}
