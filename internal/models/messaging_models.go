package models

import (
	"time"
	"gorm.io/gorm"
)

// MessageThread represents a conversation between users
type MessageThread struct {
	ID             uint           `json:"id" gorm:"primaryKey"`
	Name           string         `json:"name" gorm:"size:255"` // Optional thread name for group chats
	Type           string         `json:"type" gorm:"size:50;default:'direct'"` // direct, group, broadcast
	CreatedBy      uint           `json:"created_by"`
	CreatedByUser  *User          `json:"created_by_user,omitempty" gorm:"foreignKey:CreatedBy"`
	OrganizationID uint           `json:"organization_id"`
	Organization   *Organization  `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	IsActive       bool           `json:"is_active" gorm:"default:true"`
	LastMessageAt  *time.Time     `json:"last_message_at"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`

	// Relationships
	Participants []MessageParticipant `json:"participants,omitempty" gorm:"foreignKey:ThreadID"`
	Messages     []Message            `json:"messages,omitempty" gorm:"foreignKey:ThreadID"`
}

// MessageParticipant represents users in a message thread
type MessageParticipant struct {
	ID           uint           `json:"id" gorm:"primaryKey"`
	ThreadID     uint           `json:"thread_id"`
	Thread       *MessageThread `json:"thread,omitempty" gorm:"foreignKey:ThreadID"`
	UserID       uint           `json:"user_id"`
	User         *User          `json:"user,omitempty" gorm:"foreignKey:UserID"`
	Role         string         `json:"role" gorm:"size:50;default:'member'"` // admin, member, guest
	JoinedAt     time.Time      `json:"joined_at" gorm:"default:CURRENT_TIMESTAMP"`
	LeftAt       *time.Time     `json:"left_at"`
	IsActive     bool           `json:"is_active" gorm:"default:true"`
	IsMuted      bool           `json:"is_muted" gorm:"default:false"`
	LastSeenAt   *time.Time     `json:"last_seen_at"`
	UnreadCount  int            `json:"unread_count" gorm:"default:0"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

// Message represents individual messages in threads
type Message struct {
	ID              uint                `json:"id" gorm:"primaryKey"`
	ThreadID        uint                `json:"thread_id"`
	Thread          *MessageThread      `json:"thread,omitempty" gorm:"foreignKey:ThreadID"`
	SenderID        uint                `json:"sender_id"`
	Sender          *User               `json:"sender,omitempty" gorm:"foreignKey:SenderID"`
	Content         string              `json:"content" gorm:"type:text"`
	MessageType     string              `json:"message_type" gorm:"size:50;default:'text'"` // text, image, file, audio, video, location, contact
	ReplyToID       *uint               `json:"reply_to_id"`
	ReplyTo         *Message            `json:"reply_to,omitempty" gorm:"foreignKey:ReplyToID"`
	IsForwarded     bool                `json:"is_forwarded" gorm:"default:false"`
	ForwardedFromID *uint               `json:"forwarded_from_id"`
	IsEdited        bool                `json:"is_edited" gorm:"default:false"`
	EditedAt        *time.Time          `json:"edited_at"`
	IsDeleted       bool                `json:"is_deleted" gorm:"default:false"`
	DeletedAt       *time.Time          `json:"deleted_at_msg"`
	DeliveredAt     *time.Time          `json:"delivered_at"`
	ReadAt          *time.Time          `json:"read_at"`
	Metadata        string              `json:"metadata" gorm:"type:json"` // For file paths, locations, etc.
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`

	// Relationships
	Attachments []MessageAttachment `json:"attachments,omitempty" gorm:"foreignKey:MessageID"`
	Reactions   []MessageReaction   `json:"reactions,omitempty" gorm:"foreignKey:MessageID"`
	ReadStatus  []MessageReadStatus `json:"read_status,omitempty" gorm:"foreignKey:MessageID"`
}

// MessageAttachment represents file attachments
type MessageAttachment struct {
	ID          uint           `json:"id" gorm:"primaryKey"`
	MessageID   uint           `json:"message_id"`
	Message     *Message       `json:"message,omitempty" gorm:"foreignKey:MessageID"`
	FileName    string         `json:"file_name" gorm:"size:255"`
	FileType    string         `json:"file_type" gorm:"size:100"`
	FileSize    int64          `json:"file_size"`
	FilePath    string         `json:"file_path" gorm:"size:500"`
	ThumbnailPath *string      `json:"thumbnail_path" gorm:"size:500"`
	MimeType    string         `json:"mime_type" gorm:"size:100"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

// MessageReaction represents emoji reactions to messages
type MessageReaction struct {
	ID        uint           `json:"id" gorm:"primaryKey"`
	MessageID uint           `json:"message_id"`
	Message   *Message       `json:"message,omitempty" gorm:"foreignKey:MessageID"`
	UserID    uint           `json:"user_id"`
	User      *User          `json:"user,omitempty" gorm:"foreignKey:UserID"`
	Emoji     string         `json:"emoji" gorm:"size:10"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

// MessageReadStatus tracks read status per user per message
type MessageReadStatus struct {
	ID        uint           `json:"id" gorm:"primaryKey"`
	MessageID uint           `json:"message_id"`
	Message   *Message       `json:"message,omitempty" gorm:"foreignKey:MessageID"`
	UserID    uint           `json:"user_id"`
	User      *User          `json:"user,omitempty" gorm:"foreignKey:UserID"`
	ReadAt    time.Time      `json:"read_at" gorm:"default:CURRENT_TIMESTAMP"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

// MessageIntegration represents external messaging integrations
type MessageIntegration struct {
	ID             uint           `json:"id" gorm:"primaryKey"`
	OrganizationID uint           `json:"organization_id"`
	Organization   *Organization  `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Provider       string         `json:"provider" gorm:"size:100"` // whatsapp, telegram, slack, discord
	IsEnabled      bool           `json:"is_enabled" gorm:"default:false"`
	Configuration  string         `json:"configuration" gorm:"type:json"` // API keys, webhook URLs, etc.
	WebhookURL     string         `json:"webhook_url" gorm:"size:500"`
	APIKey         string         `json:"api_key" gorm:"size:500"`
	APISecret      string         `json:"api_secret" gorm:"size:500"`
	PhoneNumber    string         `json:"phone_number" gorm:"size:20"` // For WhatsApp
	IsVerified     bool           `json:"is_verified" gorm:"default:false"`
	LastSyncAt     *time.Time     `json:"last_sync_at"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

// MessageSettings represents organization-wide messaging settings
type MessageSettings struct {
	ID                    uint           `json:"id" gorm:"primaryKey"`
	OrganizationID        uint           `json:"organization_id"`
	Organization          *Organization  `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	EnableInAppMessaging  bool           `json:"enable_in_app_messaging" gorm:"default:true"`
	EnableGroupChats      bool           `json:"enable_group_chats" gorm:"default:true"`
	EnableFileSharing     bool           `json:"enable_file_sharing" gorm:"default:true"`
	MaxFileSize           int64          `json:"max_file_size" gorm:"default:10485760"` // 10MB default
	AllowedFileTypes      string         `json:"allowed_file_types" gorm:"type:json"`   // JSON array of file extensions
	MessageRetentionDays  int            `json:"message_retention_days" gorm:"default:365"`
	EnableMessageReactions bool          `json:"enable_message_reactions" gorm:"default:true"`
	EnableTypingIndicator bool           `json:"enable_typing_indicator" gorm:"default:true"`
	EnableReadReceipts    bool           `json:"enable_read_receipts" gorm:"default:true"`
	EnableVoiceMessages   bool           `json:"enable_voice_messages" gorm:"default:true"`
	EnableVideoMessages   bool           `json:"enable_video_messages" gorm:"default:true"`
	ModerationEnabled     bool           `json:"moderation_enabled" gorm:"default:false"`
	ProfanityFilterEnabled bool          `json:"profanity_filter_enabled" gorm:"default:false"`
	WhatsappAPIKey        string         `json:"whatsapp_api_key" gorm:"size:500"`
	WhatsappPhoneNumber   string         `json:"whatsapp_phone_number" gorm:"size:20"`
	CreatedAt             time.Time      `json:"created_at"`
	UpdatedAt             time.Time      `json:"updated_at"`
	DeletedAt             gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

// WebSocketConnection represents active WebSocket connections
type WebSocketConnection struct {
	ID             uint           `json:"id" gorm:"primaryKey"`
	UserID         uint           `json:"user_id"`
	User           *User          `json:"user,omitempty" gorm:"foreignKey:UserID"`
	OrganizationID uint           `json:"organization_id"`
	Organization   *Organization  `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	ConnectionID   string         `json:"connection_id" gorm:"size:255;uniqueIndex"`
	IsActive       bool           `json:"is_active" gorm:"default:true"`
	LastPingAt     *time.Time     `json:"last_ping_at"`
	UserAgent      string         `json:"user_agent" gorm:"size:500"`
	IPAddress      string         `json:"ip_address" gorm:"size:50"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

// TypingIndicator represents typing status
type TypingIndicator struct {
	ID        uint           `json:"id" gorm:"primaryKey"`
	ThreadID  uint           `json:"thread_id"`
	Thread    *MessageThread `json:"thread,omitempty" gorm:"foreignKey:ThreadID"`
	UserID    uint           `json:"user_id"`
	User      *User          `json:"user,omitempty" gorm:"foreignKey:UserID"`
	IsTyping  bool           `json:"is_typing" gorm:"default:false"`
	StartedAt *time.Time     `json:"started_at"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// MessageTemplate represents reusable message templates
type MessageTemplate struct {
	ID             uint           `json:"id" gorm:"primaryKey"`
	OrganizationID uint           `json:"organization_id"`
	Organization   *Organization  `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	CreatedBy      uint           `json:"created_by"`
	CreatedByUser  *User          `json:"created_by_user,omitempty" gorm:"foreignKey:CreatedBy"`
	Name           string         `json:"name" gorm:"size:255"`
	Content        string         `json:"content" gorm:"type:text"`
	Category       string         `json:"category" gorm:"size:100"` // greeting, followup, support, etc.
	Variables      string         `json:"variables" gorm:"type:json"` // Placeholders like {customer_name}
	IsActive       bool           `json:"is_active" gorm:"default:true"`
	UsageCount     int            `json:"usage_count" gorm:"default:0"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}