package notify

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Handler serves the signed-in user's notifications.
type Handler struct{ DB *gorm.DB }

// NewHandler builds a handler.
func NewHandler(db *gorm.DB) *Handler { return &Handler{DB: db} }

// RegisterRoutes mounts /notifications under the authenticated group.
//
//	GET    /notifications?filter=all|unread&limit=50&before=RFC3339&since=RFC3339
//	GET    /notifications/unread-count
//	POST   /notifications/read-all
//	POST   /notifications/:id/read      (body {"read": false} marks unread)
//	DELETE /notifications/:id
//	DELETE /notifications?scope=read|all
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/notifications")
	g.GET("", h.List)
	g.GET("/unread-count", h.UnreadCount)
	g.POST("/read-all", h.ReadAll)
	g.POST("/:id/read", h.MarkRead)
	g.DELETE("/:id", h.Delete)
	g.DELETE("", h.Clear)
}

func ok(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func (h *Handler) who(c *gin.Context) (string, string, bool) {
	org, user := c.GetString("org_id"), c.GetString("user_id")
	if org == "" || user == "" {
		fail(c, http.StatusUnauthorized, "NO_SESSION", "Sign in to see notifications")
		return "", "", false
	}
	return org, user, true
}

func (h *Handler) mine(org, user string) *gorm.DB {
	return h.DB.Model(&Notification{}).Where("organization_id = ? AND user_id = ?", org, user)
}

func (h *Handler) unread(org, user string) int64 {
	var n int64
	h.mine(org, user).Where("read_at IS NULL").Count(&n)
	return n
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, f := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// List returns the newest notifications with the unread count.
func (h *Handler) List(c *gin.Context) {
	org, user, okk := h.who(c)
	if !okk {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := h.mine(org, user)
	if c.Query("filter") == "unread" {
		q = q.Where("read_at IS NULL")
	}
	if t, ok := parseTime(c.Query("before")); ok {
		q = q.Where("created_at < ?", t)
	}
	if t, ok := parseTime(c.Query("since")); ok {
		q = q.Where("created_at > ?", t)
	}
	if ty := c.Query("type"); ty != "" {
		q = q.Where("type LIKE ?", ty+"%")
	}
	var rows []Notification
	if err := q.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", "Could not load notifications")
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	var total int64
	h.mine(org, user).Count(&total)
	ok(c, gin.H{"notifications": rows, "unread": h.unread(org, user), "total": total, "has_more": more, "server_time": Now()})
}

// UnreadCount is the cheap badge poll.
func (h *Handler) UnreadCount(c *gin.Context) {
	org, user, okk := h.who(c)
	if !okk {
		return
	}
	var latest Notification
	h.mine(org, user).Order("created_at DESC").Limit(1).Find(&latest)
	var last *time.Time
	if latest.ID != "" {
		last = &latest.CreatedAt
	}
	ok(c, gin.H{"unread": h.unread(org, user), "latest_at": last})
}

// MarkRead marks one notification read (or unread with {"read": false}).
func (h *Handler) MarkRead(c *gin.Context) {
	org, user, okk := h.who(c)
	if !okk {
		return
	}
	var in struct {
		Read *bool `json:"read"`
	}
	_ = c.ShouldBindJSON(&in)
	var val interface{} = Now()
	if in.Read != nil && !*in.Read {
		val = nil
	}
	res := h.mine(org, user).Where("id = ?", c.Param("id")).Update("read_at", val)
	if res.Error != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", "Could not update the notification")
		return
	}
	if res.RowsAffected == 0 {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Notification not found")
		return
	}
	ok(c, gin.H{"id": c.Param("id"), "unread": h.unread(org, user)})
}

// ReadAll marks every unread notification read.
func (h *Handler) ReadAll(c *gin.Context) {
	org, user, okk := h.who(c)
	if !okk {
		return
	}
	res := h.mine(org, user).Where("read_at IS NULL").Update("read_at", Now())
	if res.Error != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", "Could not update notifications")
		return
	}
	ok(c, gin.H{"updated": res.RowsAffected, "unread": 0})
}

// Delete removes one notification.
func (h *Handler) Delete(c *gin.Context) {
	org, user, okk := h.who(c)
	if !okk {
		return
	}
	res := h.DB.Where("organization_id = ? AND user_id = ? AND id = ?", org, user, c.Param("id")).Delete(&Notification{})
	if res.Error != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", "Could not delete the notification")
		return
	}
	if res.RowsAffected == 0 {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Notification not found")
		return
	}
	ok(c, gin.H{"deleted": 1, "unread": h.unread(org, user)})
}

// Clear deletes read notifications (scope=read, default) or all of them.
func (h *Handler) Clear(c *gin.Context) {
	org, user, okk := h.who(c)
	if !okk {
		return
	}
	q := h.DB.Where("organization_id = ? AND user_id = ?", org, user)
	if c.DefaultQuery("scope", "read") != "all" {
		q = q.Where("read_at IS NOT NULL")
	}
	res := q.Delete(&Notification{})
	if res.Error != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", "Could not clear notifications")
		return
	}
	ok(c, gin.H{"deleted": res.RowsAffected, "unread": h.unread(org, user)})
}

// Prune deletes notifications older than the retention (read ones after
// `read`, everything after `all`).
func Prune(db *gorm.DB, read, all time.Duration) {
	now := Now()
	db.Where("read_at IS NOT NULL AND created_at < ?", now.Add(-read)).Delete(&Notification{})
	db.Where("created_at < ?", now.Add(-all)).Delete(&Notification{})
}
