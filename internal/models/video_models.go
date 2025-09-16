package models

import (
	"time"

	"gorm.io/gorm"
)

// VideoCall represents a video call session
type VideoCall struct {
	ID             uint              `json:"id" gorm:"primaryKey"`
	CallID         string            `json:"call_id" gorm:"unique;not null;index"`
	CallerID       uint              `json:"caller_id" gorm:"not null;index"`
	CalleeID       uint              `json:"callee_id" gorm:"not null;index"`
	ThreadID       *uint             `json:"thread_id" gorm:"index"` // Link to message thread
	OrganizationID uint              `json:"organization_id" gorm:"not null;index"`
	CallType       string            `json:"call_type" gorm:"not null"` // video, audio, screen_share
	Status         string            `json:"status" gorm:"not null"`   // pending, ringing, active, ended, rejected, missed
	Quality        string            `json:"quality" gorm:"default:'hd'"` // sd, hd, fhd, 4k
	StartedAt      *time.Time        `json:"started_at"`
	EndedAt        *time.Time        `json:"ended_at"`
	Duration       int               `json:"duration"` // in seconds
	RecordingID    *string           `json:"recording_id"`
	IsRecorded     bool              `json:"is_recorded" gorm:"default:false"`
	IsEmergency    bool              `json:"is_emergency" gorm:"default:false"`
	Participants   []CallParticipant `json:"participants" gorm:"foreignKey:CallID;references:ID"`
	Recordings     []CallRecording   `json:"recordings" gorm:"foreignKey:CallID;references:ID"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	DeletedAt      gorm.DeletedAt    `json:"-" gorm:"index"`

	// Relationships
	Caller       User            `json:"caller" gorm:"foreignKey:CallerID"`
	Callee       User            `json:"callee" gorm:"foreignKey:CalleeID"`
	Thread       *MessageThread  `json:"thread,omitempty" gorm:"foreignKey:ThreadID"`
	Organization Organization    `json:"organization" gorm:"foreignKey:OrganizationID"`
}

// CallParticipant represents a participant in a video call
type CallParticipant struct {
	ID                uint           `json:"id" gorm:"primaryKey"`
	CallID            uint           `json:"call_id" gorm:"not null;index"`
	UserID            uint           `json:"user_id" gorm:"not null;index"`
	JoinedAt          *time.Time     `json:"joined_at"`
	LeftAt            *time.Time     `json:"left_at"`
	Role              string         `json:"role" gorm:"not null"` // host, participant, observer
	Status            string         `json:"status" gorm:"not null"` // connecting, connected, disconnected
	AudioEnabled      bool           `json:"audio_enabled" gorm:"default:true"`
	VideoEnabled      bool           `json:"video_enabled" gorm:"default:true"`
	ScreenShareActive bool           `json:"screen_share_active" gorm:"default:false"`
	ConnectionQuality string         `json:"connection_quality"` // excellent, good, fair, poor
	PeerID            string         `json:"peer_id"` // WebRTC peer ID
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	DeletedAt         gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Call VideoCall `json:"call" gorm:"foreignKey:CallID"`
	User User      `json:"user" gorm:"foreignKey:UserID"`
}

// CallRecording represents a recorded video call
type CallRecording struct {
	ID           uint           `json:"id" gorm:"primaryKey"`
	CallID       uint           `json:"call_id" gorm:"not null;index"`
	RecordingID  string         `json:"recording_id" gorm:"unique;not null"`
	FileName     string         `json:"file_name" gorm:"not null"`
	FilePath     string         `json:"file_path" gorm:"not null"`
	FileSize     int64          `json:"file_size"` // in bytes
	Duration     int            `json:"duration"`  // in seconds
	Format       string         `json:"format" gorm:"not null"` // mp4, webm, mkv
	Quality      string         `json:"quality"` // sd, hd, fhd
	Status       string         `json:"status" gorm:"not null"` // processing, ready, failed
	StartedAt    time.Time      `json:"started_at"`
	CompletedAt  *time.Time     `json:"completed_at"`
	DownloadURL  string         `json:"download_url"`
	ThumbnailURL string         `json:"thumbnail_url"`
	IsPrivate    bool           `json:"is_private" gorm:"default:true"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Call VideoCall `json:"call" gorm:"foreignKey:CallID"`
}

// LiveStream represents a live streaming session
type LiveStream struct {
	ID             uint               `json:"id" gorm:"primaryKey"`
	StreamID       string             `json:"stream_id" gorm:"unique;not null;index"`
	HostID         uint               `json:"host_id" gorm:"not null;index"`
	OrganizationID uint               `json:"organization_id" gorm:"not null;index"`
	ThreadID       *uint              `json:"thread_id" gorm:"index"`
	Title          string             `json:"title" gorm:"not null"`
	Description    string             `json:"description"`
	Status         string             `json:"status" gorm:"not null"` // scheduled, live, ended, paused
	Privacy        string             `json:"privacy" gorm:"not null"` // public, private, organization
	StreamURL      string             `json:"stream_url"`
	WatchURL       string             `json:"watch_url"`
	ThumbnailURL   string             `json:"thumbnail_url"`
	ViewerCount    int                `json:"viewer_count" gorm:"default:0"`
	MaxViewers     int                `json:"max_viewers" gorm:"default:0"`
	Quality        string             `json:"quality" gorm:"default:'hd'"` // sd, hd, fhd, 4k
	StartedAt      *time.Time         `json:"started_at"`
	ScheduledAt    *time.Time         `json:"scheduled_at"`
	EndedAt        *time.Time         `json:"ended_at"`
	Duration       int                `json:"duration"` // in seconds
	IsRecorded     bool               `json:"is_recorded" gorm:"default:true"`
	RecordingID    *string            `json:"recording_id"`
	Viewers        []StreamViewer     `json:"viewers" gorm:"foreignKey:StreamID;references:ID"`
	Recordings     []StreamRecording  `json:"recordings" gorm:"foreignKey:StreamID;references:ID"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
	DeletedAt      gorm.DeletedAt     `json:"-" gorm:"index"`

	// Relationships
	Host         User            `json:"host" gorm:"foreignKey:HostID"`
	Organization Organization    `json:"organization" gorm:"foreignKey:OrganizationID"`
	Thread       *MessageThread  `json:"thread,omitempty" gorm:"foreignKey:ThreadID"`
}

// StreamViewer represents a viewer of a live stream
type StreamViewer struct {
	ID           uint           `json:"id" gorm:"primaryKey"`
	StreamID     uint           `json:"stream_id" gorm:"not null;index"`
	UserID       *uint          `json:"user_id" gorm:"index"` // null for anonymous viewers
	ViewerID     string         `json:"viewer_id" gorm:"not null"` // session ID for anonymous
	JoinedAt     time.Time      `json:"joined_at"`
	LeftAt       *time.Time     `json:"left_at"`
	WatchTime    int            `json:"watch_time"` // in seconds
	IsActive     bool           `json:"is_active" gorm:"default:true"`
	IPAddress    string         `json:"ip_address"`
	UserAgent    string         `json:"user_agent"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Stream LiveStream `json:"stream" gorm:"foreignKey:StreamID"`
	User   *User      `json:"user,omitempty" gorm:"foreignKey:UserID"`
}

// StreamRecording represents a recorded live stream
type StreamRecording struct {
	ID           uint           `json:"id" gorm:"primaryKey"`
	StreamID     uint           `json:"stream_id" gorm:"not null;index"`
	RecordingID  string         `json:"recording_id" gorm:"unique;not null"`
	FileName     string         `json:"file_name" gorm:"not null"`
	FilePath     string         `json:"file_path" gorm:"not null"`
	FileSize     int64          `json:"file_size"` // in bytes
	Duration     int            `json:"duration"`  // in seconds
	Format       string         `json:"format" gorm:"not null"` // mp4, webm, mkv
	Quality      string         `json:"quality"` // sd, hd, fhd
	Status       string         `json:"status" gorm:"not null"` // processing, ready, failed
	StartedAt    time.Time      `json:"started_at"`
	CompletedAt  *time.Time     `json:"completed_at"`
	DownloadURL  string         `json:"download_url"`
	ThumbnailURL string         `json:"thumbnail_url"`
	ViewCount    int            `json:"view_count" gorm:"default:0"`
	IsPublic     bool           `json:"is_public" gorm:"default:false"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Stream LiveStream `json:"stream" gorm:"foreignKey:StreamID"`
}

// ScreenShare represents a screen sharing session
type ScreenShare struct {
	ID             uint           `json:"id" gorm:"primaryKey"`
	SessionID      string         `json:"session_id" gorm:"unique;not null;index"`
	HostID         uint           `json:"host_id" gorm:"not null;index"`
	CallID         *uint          `json:"call_id" gorm:"index"` // Optional: linked to video call
	ThreadID       *uint          `json:"thread_id" gorm:"index"`
	OrganizationID uint           `json:"organization_id" gorm:"not null;index"`
	Status         string         `json:"status" gorm:"not null"` // active, paused, ended
	Quality        string         `json:"quality" gorm:"default:'hd'"` // sd, hd, fhd
	AudioEnabled   bool           `json:"audio_enabled" gorm:"default:false"`
	IsRecorded     bool           `json:"is_recorded" gorm:"default:false"`
	StartedAt      time.Time      `json:"started_at"`
	EndedAt        *time.Time     `json:"ended_at"`
	Duration       int            `json:"duration"` // in seconds
	RecordingID    *string        `json:"recording_id"`
	ViewerCount    int            `json:"viewer_count" gorm:"default:0"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Host         User            `json:"host" gorm:"foreignKey:HostID"`
	Call         *VideoCall      `json:"call,omitempty" gorm:"foreignKey:CallID"`
	Thread       *MessageThread  `json:"thread,omitempty" gorm:"foreignKey:ThreadID"`
	Organization Organization    `json:"organization" gorm:"foreignKey:OrganizationID"`
}

// VideoSettings represents video call and streaming settings for an organization
type VideoSettings struct {
	ID                     uint           `json:"id" gorm:"primaryKey"`
	OrganizationID         uint           `json:"organization_id" gorm:"unique;not null;index"`
	EnableVideoCalls       bool           `json:"enable_video_calls" gorm:"default:true"`
	EnableScreenSharing    bool           `json:"enable_screen_sharing" gorm:"default:true"`
	EnableRecording        bool           `json:"enable_recording" gorm:"default:true"`
	EnableLiveStreaming    bool           `json:"enable_live_streaming" gorm:"default:false"`
	MaxCallDuration        int            `json:"max_call_duration" gorm:"default:3600"` // seconds
	MaxParticipants        int            `json:"max_participants" gorm:"default:10"`
	DefaultQuality         string         `json:"default_quality" gorm:"default:'hd'"` // sd, hd, fhd, 4k
	AutoRecord             bool           `json:"auto_record" gorm:"default:false"`
	RecordingRetentionDays int            `json:"recording_retention_days" gorm:"default:30"`
	AllowedCallTypes       string         `json:"allowed_call_types" gorm:"default:'video,audio,screen_share'"` // JSON array as string
	STUNServers            string         `json:"stun_servers"` // JSON array of STUN servers
	TURNServers            string         `json:"turn_servers"` // JSON array of TURN servers
	ICEServers             string         `json:"ice_servers"`  // JSON array of ICE servers
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
	DeletedAt              gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization Organization `json:"organization" gorm:"foreignKey:OrganizationID"`
}

// WebRTCSignal represents WebRTC signaling data for peer connections
type WebRTCSignal struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	SessionID  string         `json:"session_id" gorm:"not null;index"`
	FromUserID uint           `json:"from_user_id" gorm:"not null;index"`
	ToUserID   uint           `json:"to_user_id" gorm:"not null;index"`
	Type       string         `json:"type" gorm:"not null"` // offer, answer, ice-candidate
	Data       string         `json:"data" gorm:"type:text"` // JSON data
	Processed  bool           `json:"processed" gorm:"default:false"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	FromUser User `json:"from_user" gorm:"foreignKey:FromUserID"`
	ToUser   User `json:"to_user" gorm:"foreignKey:ToUserID"`
}