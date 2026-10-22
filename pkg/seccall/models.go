// Package seccall is the "Secure call" feature: shareable call links whose
// conversation is end-to-end encrypted between the participants' browsers.
//
// What the server does — and, as importantly, what it cannot do:
//
//   - The link is <PUBLIC_APP_URL>/c/<room id>#<key>. The room id (128 random
//     bits) comes from here; the key (256 random bits) is generated in the
//     host's browser and lives only in the URL fragment, which browsers never
//     send to a server. DASYIN never sees it.
//   - Browsers derive a "link check" from the key (HKDF, its own label) and
//     send it when they create or join a room. The server stores only its
//     SHA-256, so someone who only knows the room id (e.g. from a request log)
//     cannot join, and a wrong or cut-off link is refused with a clear message.
//     The check is a one-way value: it does not reveal the key.
//   - Signalling (session descriptions, ICE candidates, control messages) is
//     encrypted and authenticated in the browser (AES-GCM, key from the
//     fragment via HKDF, random IV per message, room id + sender + recipient
//     as associated data). This package relays those messages as opaque
//     strings: it checks their shape (and refuses anything that looks like
//     plain text) but never decrypts, parses or changes them. Because it
//     cannot read or alter the descriptions, it cannot swap the DTLS
//     fingerprints and sit in the middle of the media.
//   - Media flows browser to browser (WebRTC mesh, DTLS-SRTP). A TURN relay,
//     when configured, only forwards packets that are already encrypted.
//     Nothing is recorded; no media passes through this service.
//
// The relay is Postgres-backed so it works when Cloud Run runs several
// instances: messages are rows with a per-room sequence number, long polls
// wake on LISTEN/NOTIFY (with a short re-check as a fallback), and a cleanup
// loop (one instance at a time, Postgres advisory lock) removes messages after
// two minutes and ends rooms when they expire.
package seccall

import "time"

// Participant states.
const (
	StateLobby   = "lobby"   // waiting for the host to let them in
	StateActive  = "active"  // in the call
	StateLeft    = "left"    // left (or went quiet: LeftReason "stale")
	StateRemoved = "removed" // removed by the host
	StateDenied  = "denied"  // the host said no in the lobby
)

// Room is one secure call. Only metadata is stored — never the key.
type Room struct {
	ID               string     `json:"id" gorm:"primaryKey;type:varchar(32)"`
	OrganizationID   string     `json:"-" gorm:"type:varchar(64);index;not null"`
	HostUserID       string     `json:"-" gorm:"type:varchar(64);index;not null"`
	HostName         string     `json:"host_name" gorm:"type:varchar(120)"`
	Title            string     `json:"title" gorm:"type:varchar(120)"`
	CheckHash        string     `json:"-" gorm:"type:varchar(64);not null"` // SHA-256 (hex) of the link check
	Lobby            bool       `json:"lobby" gorm:"not null;default:false"`
	Locked           bool       `json:"locked" gorm:"not null;default:false"`
	MaxParticipants  int        `json:"max_participants" gorm:"not null;default:6"`
	LastSeq          int64      `json:"-" gorm:"not null;default:0"`
	PeakParticipants int        `json:"peak_participants" gorm:"not null;default:0"`
	JoinCount        int        `json:"join_count" gorm:"not null;default:0"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        time.Time  `json:"expires_at" gorm:"index"`
	EndedAt          *time.Time `json:"ended_at" gorm:"index"`
	EndReason        string     `json:"end_reason" gorm:"type:varchar(20)"`
}

// TableName keeps the table name explicit.
func (Room) TableName() string { return "seccall_rooms" }

// Participant is one person in (or waiting for) a room. The participant token
// is a bearer secret handed to the browser once; only its SHA-256 is stored.
type Participant struct {
	ID         string     `json:"id" gorm:"primaryKey;type:varchar(32)"`
	RoomID     string     `json:"-" gorm:"type:varchar(32);index;not null"`
	Name       string     `json:"name" gorm:"type:varchar(80)"`
	IsHost     bool       `json:"is_host" gorm:"not null;default:false"`
	UserID     string     `json:"-" gorm:"type:varchar(64)"`
	TokenHash  string     `json:"-" gorm:"type:varchar(64);index"`
	State      string     `json:"state" gorm:"type:varchar(12);index"`
	LeftReason string     `json:"left_reason,omitempty" gorm:"type:varchar(12)"`
	CreatedAt  time.Time  `json:"joined_at"`
	AdmittedAt *time.Time `json:"admitted_at,omitempty"`
	LastSeen   time.Time  `json:"last_seen" gorm:"index"`
	LeftAt     *time.Time `json:"left_at,omitempty"`
}

// TableName keeps the table name explicit.
func (Participant) TableName() string { return "seccall_participants" }

// Message is one relayed item. Kind "sig" is a browser's ciphertext (opaque
// here); kind "evt" is a server event (join, leave, lobby, lock, end …).
// ToPID "" is everyone in the room except the sender; "@hosts" is the host.
type Message struct {
	ID        int64     `gorm:"primaryKey;autoIncrement"`
	RoomID    string    `gorm:"type:varchar(32);not null;uniqueIndex:idx_seccall_msg_room_seq,priority:1"`
	Seq       int64     `gorm:"not null;uniqueIndex:idx_seccall_msg_room_seq,priority:2"`
	FromPID   string    `gorm:"column:from_pid;type:varchar(32);not null;default:''"`
	ToPID     string    `gorm:"column:to_pid;type:varchar(32);not null;default:''"`
	Kind      string    `gorm:"type:varchar(8);not null"`
	Body      string    `gorm:"type:text;not null"`
	CreatedAt time.Time `gorm:"index"`
}

// TableName keeps the table name explicit.
func (Message) TableName() string { return "seccall_messages" }

// ICE modes.
const (
	ModeSTUN   = "stun"   // STUN only (no relay)
	ModeStatic = "static" // TURN with a fixed username and password
	ModeCoturn = "coturn" // TURN with time-limited credentials (coturn use-auth-secret)
)

// Settings is the platform-wide ICE (STUN/TURN) configuration, one row
// (ID "platform"), edited by a super admin in System settings → Calls.
// Credential and SharedSecret are encrypted at rest (pkg/mail EncryptSecret).
type Settings struct {
	ID           string    `json:"-" gorm:"primaryKey;type:varchar(20)"`
	Mode         string    `json:"mode" gorm:"type:varchar(10);not null;default:'stun'"`
	STUNURLs     string    `json:"-" gorm:"column:stun_urls;type:text"`
	TURNURLs     string    `json:"-" gorm:"column:turn_urls;type:text"`
	Username     string    `json:"username" gorm:"type:varchar(200)"`
	Credential   string    `json:"-" gorm:"type:text"`
	SharedSecret string    `json:"-" gorm:"type:text"`
	UpdatedAt    time.Time `json:"updated_at"`
	UpdatedBy    string    `json:"updated_by" gorm:"type:varchar(200)"`
}

// TableName keeps the table name explicit.
func (Settings) TableName() string { return "seccall_settings" }

// Models lists every table of the package (for migrations and tests).
func Models() []interface{} {
	return []interface{}{&Room{}, &Participant{}, &Message{}, &Settings{}}
}
