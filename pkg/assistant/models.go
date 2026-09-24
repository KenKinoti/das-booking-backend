// Package assistant is the in-app "Ask DASYIN" chat assistant. It runs an
// agentic loop against the Anthropic Messages API using the shared AI tool
// registry (pkg/aitools). Read-only tools run automatically; any tool with side
// effects pauses the loop until the user confirms it in the UI.
package assistant

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// Conversation is one chat thread, private to its user.
type Conversation struct {
	ID            string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID         string    `json:"-" gorm:"type:varchar(255);index;not null"`
	UserID        string    `json:"-" gorm:"type:varchar(255);index;not null"`
	Title         string    `json:"title" gorm:"type:varchar(200)"`
	Pending       string    `json:"-" gorm:"type:text"` // JSON pendingState, "" when none
	LastMessageAt time.Time `json:"last_message_at" gorm:"index"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (Conversation) TableName() string { return "assistant_conversations" }

// Message is one Anthropic-format message. Content holds the content blocks
// exactly as sent to / received from the API; Meta holds display data keyed
// by tool_use id (never sent to the model).
type Message struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	ConversationID string    `json:"conversation_id" gorm:"type:varchar(64);index;not null"`
	OrgID          string    `json:"-" gorm:"type:varchar(255);index"`
	UserID         string    `json:"-" gorm:"type:varchar(255);index"`
	Seq            int       `json:"seq" gorm:"not null;default:0"`
	Role           string    `json:"role" gorm:"type:varchar(16);not null"`
	Content        string    `json:"-" gorm:"type:text"`
	Meta           string    `json:"-" gorm:"type:text"`
	Model          string    `json:"model,omitempty" gorm:"type:varchar(100)"`
	InputTokens    int       `json:"input_tokens"`
	OutputTokens   int       `json:"output_tokens"`
	CreatedAt      time.Time `json:"created_at" gorm:"index"`
}

func (Message) TableName() string { return "assistant_messages" }

// UserSettings holds per-user assistant preferences.
type UserSettings struct {
	UserID      string    `json:"-" gorm:"type:varchar(255);primaryKey"`
	OrgID       string    `json:"-" gorm:"type:varchar(255);index"`
	AutoApprove bool      `json:"auto_approve" gorm:"not null;default:false"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (UserSettings) TableName() string { return "assistant_user_settings" }

// Migrate creates the assistant tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&Conversation{}, &Message{}, &UserSettings{})
}

// Block is an Anthropic content block (the subset we use).
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// MarshalJSON emits only the fields the Messages API accepts for each block
func (b Block) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": b.Type}
	switch b.Type {
	case "text":
		m["text"] = b.Text
	case "tool_use":
		m["id"] = b.ID
		m["name"] = b.Name
		in := b.Input
		if len(in) == 0 {
			in = json.RawMessage(`{}`)
		}
		m["input"] = in
	case "tool_result":
		m["tool_use_id"] = b.ToolUseID
		m["content"] = b.Content
		if b.IsError {
			m["is_error"] = true
		}
	default:
		if b.Text != "" {
			m["text"] = b.Text
		}
	}
	return json.Marshal(m)
}

// ToolMeta is display information about one tool call.
type ToolMeta struct {
	Name    string         `json:"name"`
	Title   string         `json:"title"`
	Status  string         `json:"status"` // done | error | declined
	Summary string         `json:"summary"`
	Link    string         `json:"link,omitempty"`
	Changed string         `json:"changed,omitempty"` // entity type changed by a write
	Write   bool           `json:"write,omitempty"`
	Extra   map[string]any `json:"extra,omitempty"`
}

// pendingState is stored on a conversation while the loop waits for the user.
type pendingState struct {
	MessageID string              `json:"message_id"` // assistant message holding the tool_use blocks
	Results   []Block             `json:"results"`    // tool_result blocks already produced
	Meta      map[string]ToolMeta `json:"meta"`
	Queue     []Block             `json:"queue"` // tool_use blocks awaiting confirmation (first is shown)
}
