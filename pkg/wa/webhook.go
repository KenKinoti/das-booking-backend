package wa

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// MessageLog is one outbound WhatsApp message and its delivery status
// (whatsapp_message_log). Features record what they send with Record; the
// webhook moves Status along accepted → sent → delivered → read, or failed.
type MessageLog struct {
	ID             uint       `json:"-" gorm:"primaryKey"`
	OrganizationID string     `json:"-" gorm:"type:varchar(255);index;not null"` // organisation the message is for
	SenderOrgID    string     `json:"-" gorm:"type:varchar(255)"`                // organisation whose number sent it
	MessageID      string     `json:"message_id" gorm:"type:varchar(200);uniqueIndex"`
	To             string     `json:"to" gorm:"type:varchar(32);index"`
	Kind           string     `json:"kind" gorm:"type:varchar(16)"` // text | template
	Template       string     `json:"template,omitempty" gorm:"type:varchar(120)"`
	Source         string     `json:"source" gorm:"type:varchar(40);index"` // brief | schedule | …
	SourceRef      string     `json:"source_ref" gorm:"type:varchar(120);index"`
	Status         string     `json:"status" gorm:"type:varchar(16)"` // accepted | sent | delivered | read | failed
	Error          string     `json:"error,omitempty" gorm:"type:text"`
	CreatedAt      time.Time  `json:"created_at"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
	ReadAt         *time.Time `json:"read_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// TableName is fixed.
func (MessageLog) TableName() string { return "whatsapp_message_log" }

// Window remembers when a number last messaged an organisation's WhatsApp
// number (whatsapp_windows): free text is allowed for 24 hours after that.
type Window struct {
	OrganizationID string    `json:"-" gorm:"type:varchar(255);primaryKey"`
	Phone          string    `json:"phone" gorm:"type:varchar(32);primaryKey"`
	Name           string    `json:"name" gorm:"type:varchar(200)"`
	LastInboundAt  time.Time `json:"last_inbound_at"`
	LastText       string    `json:"last_text" gorm:"type:text"`
}

// TableName is fixed.
func (Window) TableName() string { return "whatsapp_windows" }

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&MessageLog{}, &Window{}) }

// Record stores a sent message for delivery tracking.
func Record(db *gorm.DB, org string, a *Account, to string, res *Result, kind, template, source, ref string) {
	if db == nil || res == nil || res.MessageID == "" {
		return
	}
	sender := org
	if a != nil {
		sender = a.OrgID
	}
	row := MessageLog{OrganizationID: org, SenderOrgID: sender, MessageID: res.MessageID, To: to, Kind: kind, Template: template, Source: source, SourceRef: ref, Status: "accepted"}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		log.Printf("wa: record message: %v", err)
	}
}

// WindowOpen reports whether free text may be sent to phone from org's number.
func WindowOpen(db *gorm.DB, org, phone string, now time.Time) bool {
	var w Window
	if db == nil || db.Where("organization_id = ? AND phone = ?", org, phone).Limit(1).Find(&w).Error != nil || w.Phone == "" {
		return false
	}
	return now.Sub(w.LastInboundAt) < 24*time.Hour-5*time.Minute
}

// StatusEvent is a delivery status from the webhook.
type StatusEvent struct {
	OrgID     string
	MessageID string
	To        string
	Status    string // sent | delivered | read | failed
	Error     string
	At        time.Time
}

// InboundEvent is a message someone sent to the number.
type InboundEvent struct {
	OrgID string
	From  string
	Name  string
	Text  string
	At    time.Time
}

var (
	hookMu        sync.RWMutex
	statusHooks   []func(StatusEvent)
	inboundHooks  []func(InboundEvent)
	webhookSecret = func() string { return strings.TrimSpace(os.Getenv("WHATSAPP_APP_SECRET")) }
)

// OnStatus registers a delivery status listener (called after the log is updated).
func OnStatus(fn func(StatusEvent)) {
	hookMu.Lock()
	statusHooks = append(statusHooks, fn)
	hookMu.Unlock()
}

// OnInbound registers an inbound message listener.
func OnInbound(fn func(InboundEvent)) {
	hookMu.Lock()
	inboundHooks = append(inboundHooks, fn)
	hookMu.Unlock()
}

// Handler serves the webhook and the settings helpers.
type Handler struct{ DB *gorm.DB }

// RegisterRoutes: public GET/POST /whatsapp/webhook (set this URL as the
// callback in Meta's app dashboard, with the verify token from the settings);
// signed-in GET /whatsapp/status and GET /whatsapp/templates.
func (h *Handler) RegisterRoutes(auth, public *gin.RouterGroup) {
	public.GET("/whatsapp/webhook", h.verify)
	public.POST("/whatsapp/webhook", h.receive)
	auth.GET("/whatsapp/status", h.status)
	auth.GET("/whatsapp/templates", h.templates)
}

func secretValue(s string) string {
	s = strings.TrimSpace(s)
	if mail.IsEncrypted(s) {
		if p, err := mail.DecryptSecret(s); err == nil {
			return p
		}
	}
	return s
}

func (h *Handler) verify(c *gin.Context) {
	if c.Query("hub.mode") != "subscribe" || c.Query("hub.verify_token") == "" {
		c.String(http.StatusForbidden, "verification failed")
		return
	}
	var rows []models.IntegrationSettings
	h.DB.Omit("Organization").Where("whatsapp_verify_token <> ''").Find(&rows)
	for _, r := range rows {
		if hmac.Equal([]byte(secretValue(r.WhatsAppSettings.VerifyToken)), []byte(c.Query("hub.verify_token"))) {
			c.String(http.StatusOK, c.Query("hub.challenge"))
			return
		}
	}
	c.String(http.StatusForbidden, "verification failed")
}

type webhookBody struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []struct {
					From      string `json:"from"`
					Timestamp string `json:"timestamp"`
					Type      string `json:"type"`
					Text      struct {
						Body string `json:"body"`
					} `json:"text"`
					Button struct {
						Text string `json:"text"`
					} `json:"button"`
				} `json:"messages"`
				Statuses []struct {
					ID          string `json:"id"`
					Status      string `json:"status"`
					Timestamp   string `json:"timestamp"`
					RecipientID string `json:"recipient_id"`
					Errors      []struct {
						Code    int    `json:"code"`
						Title   string `json:"title"`
						Message string `json:"message"`
					} `json:"errors"`
				} `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

func unixTime(s string) time.Time {
	var n int64
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return time.Now()
		}
		n = n*10 + int64(ch-'0')
	}
	if n == 0 {
		return time.Now()
	}
	return time.Unix(n, 0)
}

func (h *Handler) receive(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 2<<20))
	if err != nil {
		c.Status(http.StatusBadRequest)
		return
	}
	if sec := webhookSecret(); sec != "" {
		sig := strings.TrimPrefix(c.GetHeader("X-Hub-Signature-256"), "sha256=")
		m := hmac.New(sha256.New, []byte(sec))
		m.Write(raw)
		if !hmac.Equal([]byte(hex.EncodeToString(m.Sum(nil))), []byte(sig)) {
			c.Status(http.StatusUnauthorized)
			return
		}
	}
	var b webhookBody
	if json.Unmarshal(raw, &b) != nil {
		c.Status(http.StatusOK) // never make Meta retry a body we can't read
		return
	}
	for _, e := range b.Entry {
		for _, ch := range e.Changes {
			v := ch.Value
			org := h.orgForPhoneNumberID(v.Metadata.PhoneNumberID)
			for _, s := range v.Statuses {
				h.applyStatus(org, s.ID, s.RecipientID, s.Status, s.Timestamp, s.Errors)
			}
			names := map[string]string{}
			for _, ct := range v.Contacts {
				names[ct.WaID] = ct.Profile.Name
			}
			for _, m := range v.Messages {
				if org == "" || m.From == "" {
					continue
				}
				text := m.Text.Body
				if text == "" {
					text = m.Button.Text
				}
				at := unixTime(m.Timestamp)
				w := Window{OrganizationID: org, Phone: m.From, Name: names[m.From], LastInboundAt: at, LastText: text}
				h.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "organization_id"}, {Name: "phone"}},
					DoUpdates: clause.AssignmentColumns([]string{"name", "last_inbound_at", "last_text"})}).Create(&w)
				ev := InboundEvent{OrgID: org, From: m.From, Name: names[m.From], Text: text, At: at}
				hookMu.RLock()
				for _, fn := range inboundHooks {
					fn(ev)
				}
				hookMu.RUnlock()
			}
		}
	}
	c.Status(http.StatusOK)
}

func (h *Handler) orgForPhoneNumberID(id string) string {
	if id == "" {
		return ""
	}
	var row models.IntegrationSettings
	h.DB.Omit("Organization").Where("whatsapp_phone_number_id = ?", id).Order("created_at").Limit(1).Find(&row)
	return row.OrganizationID
}

var statusRank = map[string]int{"accepted": 0, "sent": 1, "delivered": 2, "read": 3, "failed": 4}

func (h *Handler) applyStatus(senderOrg, id, to, status, ts string, errs []struct {
	Code    int    `json:"code"`
	Title   string `json:"title"`
	Message string `json:"message"`
}) {
	if id == "" {
		return
	}
	var row MessageLog
	if h.DB.Where("message_id = ?", id).Limit(1).Find(&row).Error != nil || row.ID == 0 {
		return
	}
	at := unixTime(ts)
	upd := map[string]any{"updated_at": time.Now()}
	if statusRank[status] >= statusRank[row.Status] {
		upd["status"] = status
	}
	switch status {
	case "delivered":
		upd["delivered_at"] = at
	case "read":
		upd["read_at"] = at
		if row.DeliveredAt == nil {
			upd["delivered_at"] = at
		}
	case "failed":
		msg := "failed"
		if len(errs) > 0 {
			msg = strings.TrimSpace(errs[0].Title + ": " + errs[0].Message)
			if fr := Friendly(&Error{Code: errs[0].Code, Message: errs[0].Message}); fr != "" && errs[0].Code != 0 {
				msg = fr
			}
		}
		upd["error"] = msg
	}
	h.DB.Model(&MessageLog{}).Where("id = ?", row.ID).Updates(upd)
	ev := StatusEvent{OrgID: row.OrganizationID, MessageID: id, To: to, Status: status, At: at}
	if e, ok := upd["error"].(string); ok {
		ev.Error = e
	}
	hookMu.RLock()
	for _, fn := range statusHooks {
		fn(ev)
	}
	hookMu.RUnlock()
}

func (h *Handler) status(c *gin.Context) {
	org := c.GetString("org_id")
	own := StatusOf(h.DB, org, false)
	brief := StatusOf(h.DB, org, true)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"customers": own, "notifications": brief}})
}

func (h *Handler) templates(c *gin.Context) {
	org := c.GetString("org_id")
	platform := c.Query("for") == "notifications"
	a, err := Resolve(h.DB, org, platform)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"templates": []TemplateInfo{}, "note": err.Error()}})
		return
	}
	list, err := a.Templates(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"templates": []TemplateInfo{}, "note": Friendly(err)}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"templates": list, "platform": a.Platform}})
}
