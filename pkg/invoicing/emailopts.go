package invoicing

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Sending options (Invoice settings → Email): the "Undo send" window and
// whether invoice, quote and reminder emails carry the PDF.
//
// Stored so that a missing row means the defaults: a 2-minute undo window
// and the PDF attached (today's behaviour).
// ---------------------------------------------------------------------------

// UndoChoices are the undo windows an organisation can pick (seconds; 0 = off).
var UndoChoices = []int{0, 10, 30, 60, 120}

// DefaultUndoSeconds is the undo window when nothing was chosen.
const DefaultUndoSeconds = 120

// EmailOptions is an organisation's sending choice (invoice_email_options).
type EmailOptions struct {
	OrganizationID string `gorm:"type:varchar(255);primaryKey"`
	// UndoSeconds: how long a sent email is held so it can be undone (nil =
	// the default, 0 = off).
	UndoSeconds *int
	// NoPDF: "Attach the PDF to invoice emails" off (invoice, quote and
	// reminder emails go out link-only; receipts keep their receipt PDF).
	NoPDF     bool
	UpdatedBy string    `gorm:"type:varchar(255)"`
	UpdatedAt time.Time ``
}

func (EmailOptions) TableName() string { return "invoice_email_options" }

// SendOptions is the effective setting (positive names, for the API).
type SendOptions struct {
	UndoSeconds int        `json:"undo_seconds"`
	AttachPDF   bool       `json:"attach_pdf"`
	Choices     []int      `json:"undo_choices"`
	UpdatedAt   *time.Time `json:"updated_at"`
}

// LoadSendOptions returns org's sending options (the defaults when unset).
func LoadSendOptions(db *gorm.DB, org string) SendOptions {
	o := SendOptions{UndoSeconds: DefaultUndoSeconds, AttachPDF: true, Choices: UndoChoices}
	if org == "" || db == nil || !db.Migrator().HasTable(&EmailOptions{}) {
		return o
	}
	var s EmailOptions
	if db.Where("organization_id = ?", org).Limit(1).Find(&s).Error != nil || s.OrganizationID == "" {
		return o
	}
	if s.UndoSeconds != nil {
		o.UndoSeconds = *s.UndoSeconds
	}
	o.AttachPDF = !s.NoPDF
	t := s.UpdatedAt
	o.UpdatedAt = &t
	return o
}

func validUndo(n int) bool {
	for _, c := range UndoChoices {
		if c == n {
			return true
		}
	}
	return false
}

// GetEmailOptions serves GET /email-options.
func (h *Handler) GetEmailOptions(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	ok(c, http.StatusOK, LoadSendOptions(h.DB, org))
}

// PutEmailOptions serves PUT /email-options {undo_seconds, attach_pdf}; fields
// left out keep their value.
func (h *Handler) PutEmailOptions(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in struct {
		UndoSeconds *int  `json:"undo_seconds"`
		AttachPDF   *bool `json:"attach_pdf"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	if in.UndoSeconds != nil && !validUndo(*in.UndoSeconds) {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose Off, 10, 30, 60 or 120 seconds")
		return
	}
	before := LoadSendOptions(h.DB, org)
	var s EmailOptions
	h.DB.Where("organization_id = ?", org).Limit(1).Find(&s)
	s.OrganizationID = org
	if in.UndoSeconds != nil {
		v := *in.UndoSeconds
		s.UndoSeconds = &v
	}
	if in.AttachPDF != nil {
		s.NoPDF = !*in.AttachPDF
	}
	user := ctxString(c, "user_id")
	s.UpdatedBy, s.UpdatedAt = user, h.now()
	if err := h.DB.Save(&s).Error; err != nil {
		failErr(c, err)
		return
	}
	after := LoadSendOptions(h.DB, org)
	h.audit(c, org, user, "invoice_email.options", "invoice_settings", org,
		map[string]any{"undo_seconds": before.UndoSeconds, "attach_pdf": before.AttachPDF},
		map[string]any{"undo_seconds": after.UndoSeconds, "attach_pdf": after.AttachPDF})
	ok(c, http.StatusOK, after)
}

// ---------------------------------------------------------------- audit

// audit writes one audit_logs row when the table exists (never fails the
// request). c may be nil (background work).
func (h *Handler) audit(c *gin.Context, org, user, action, entityType, entityID string, old, values any) {
	db := h.DB.Session(&gorm.Session{NewDB: true})
	if !db.Migrator().HasTable("audit_logs") {
		return
	}
	enc := func(v any) string {
		if v == nil {
			return "{}"
		}
		b, err := json.Marshal(v)
		if err != nil || string(b) == "null" {
			return "{}"
		}
		return string(b)
	}
	var uid *string
	if user != "" {
		uid = &user
	}
	ip, ua := "", ""
	if c != nil && c.Request != nil {
		ip, ua = c.ClientIP(), c.Request.UserAgent()
		if len(ua) > 500 {
			ua = ua[:500]
		}
	}
	if len(entityID) > 36 {
		entityID = entityID[:36]
	}
	row := map[string]any{"id": uuid.NewString(), "organization_id": org, "user_id": uid, "action": action, "entity_type": entityType,
		"entity_id": entityID, "old_values": enc(old), "new_values": enc(values), "ip_address": ip, "user_agent": ua, "created_at": h.now()}
	if err := db.Table("audit_logs").Create(row).Error; err != nil {
		// Older databases: organisation ids longer than the column, etc. —
		// the audit entry is best effort.
		_ = err
	}
}
