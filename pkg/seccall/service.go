package seccall

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Service implements secure calls.
type Service struct {
	DB     *gorm.DB
	Now    func() time.Time
	Limits Limits
	// AppURL is PUBLIC_APP_URL; links are AppURL + "/c/" + room id + "#" + key.
	AppURL string

	hub       *hub
	lim       *limiter
	listening atomic.Bool
}

// New builds the service with the default limits.
func New(db *gorm.DB) *Service {
	return &Service{DB: db, Limits: DefaultLimits(), hub: newHub(), lim: newLimiter()}
}

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(Models()...) }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// ---------------------------------------------------------------- errors

// Error is a refusal with an HTTP status, a stable code and a message for people.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func refuse(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

var (
	errNotFound  = refuse(http.StatusNotFound, "NOT_FOUND", "This call link isn't valid. Check that you have the whole link.")
	errEnded     = refuse(http.StatusGone, "CALL_ENDED", "This call has ended.")
	errWrongLink = refuse(http.StatusForbidden, "WRONG_LINK", "This link is incomplete or has been changed. Ask the person who sent it for the full link.")
	errLocked    = refuse(http.StatusLocked, "LOCKED", "The host has locked this call, so nobody new can join. Ask them to unlock it.")
	errToken     = refuse(http.StatusUnauthorized, "NOT_IN_CALL", "You're no longer in this call. Join again with the link.")
	errRemoved   = refuse(http.StatusForbidden, "REMOVED", "The host removed you from this call.")
	errDenied    = refuse(http.StatusForbidden, "DENIED", "The host didn't let you in to this call.")
	errWaiting   = refuse(http.StatusForbidden, "NOT_ADMITTED", "Wait for the host to let you in.")
	errLimited   = refuse(http.StatusTooManyRequests, "RATE_LIMITED", "Too many attempts. Wait a minute and try again.")
	errNoCreate  = refuse(http.StatusForbidden, "FORBIDDEN", "Only owners, admins and managers can start secure calls.")
	errNotHost   = refuse(http.StatusNotFound, "NOT_FOUND", "Call not found.")
	errResume    = refuse(http.StatusConflict, "RESUME", "Reconnecting…")
)

func errFull(n int) *Error {
	return refuse(http.StatusConflict, "FULL", fmt.Sprintf("This call is full (%d people).", n))
}

func invalid(msg string) *Error { return refuse(http.StatusBadRequest, "INVALID", msg) }

// ---------------------------------------------------------------- random values & hashes

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("seccall: no randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewRoomID is 128 random bits (22 base64url characters).
func NewRoomID() string { return randomID(16) }

func newParticipantID() string { return randomID(12) }

// tokenPrefix marks participant tokens (they travel as "Authorization: Bearer").
const tokenPrefix = "sct_"

func newToken() string { return tokenPrefix + randomID(32) }

// HashToken is the stored form of a participant token.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// HashCheck validates a link check (16 bytes, base64url) and returns its
// stored form, the hex SHA-256 of the 16 bytes.
func HashCheck(check string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(check))
	if err != nil || len(b) != 16 {
		return "", false
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), true
}

func checkMatches(room *Room, check string) bool {
	h, ok := HashCheck(check)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h), []byte(room.CheckHash)) == 1
}

// ValidRoomID reports whether id has the shape of a room id.
func ValidRoomID(id string) bool {
	if len(id) != 22 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(id)
	return err == nil && len(b) == 16
}

// ---------------------------------------------------------------- ciphertext

// ValidateCiphertext checks the shape of a relayed message: "<iv>.<ct>", both
// base64url without padding, a 12-byte IV and an AES-GCM ciphertext (at least
// one byte plus the 16-byte tag). The server never decrypts it. As a sanity
// check it refuses payloads that are obviously not encrypted (mostly
// printable text, or one byte value dominating) — encrypted data looks
// random. Plain SDP, JSON or candidate strings never get through.
func ValidateCiphertext(s string, max int) error {
	iv, ct, ok := strings.Cut(s, ".")
	if !ok || iv == "" || ct == "" || strings.Contains(ct, ".") {
		return errors.New("messages must be end-to-end encrypted: expected \"<iv>.<ciphertext>\" in base64url")
	}
	ivb, err := base64.RawURLEncoding.DecodeString(iv)
	if err != nil || len(ivb) != 12 {
		return errors.New("messages must be end-to-end encrypted: the IV must be 12 bytes of base64url")
	}
	if max > 0 && base64.RawURLEncoding.DecodedLen(len(ct)) > max {
		return fmt.Errorf("message too large (max %d bytes)", max)
	}
	ctb, err := base64.RawURLEncoding.DecodeString(ct)
	if err != nil || len(ctb) < 17 {
		return errors.New("messages must be end-to-end encrypted: the ciphertext must be base64url AES-GCM output")
	}
	if looksPlain(ctb) {
		return errors.New("messages must be end-to-end encrypted: this looks like plain text")
	}
	return nil
}

// looksPlain is true for byte strings that are clearly not ciphertext.
func looksPlain(b []byte) bool {
	if len(b) < 32 {
		return false
	}
	printable := 0
	var freq [256]int
	top := 0
	for _, c := range b {
		if c == '\t' || c == '\n' || c == '\r' || (c >= 0x20 && c < 0x7f) {
			printable++
		}
		freq[c]++
		if freq[c] > top {
			top = freq[c]
		}
	}
	if printable*100 >= len(b)*85 {
		return true
	}
	return len(b) >= 64 && top*100 > len(b)*20
}

// ---------------------------------------------------------------- views

// Peer is a participant as other participants see them.
type Peer struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	IsHost bool      `json:"is_host"`
	Since  time.Time `json:"since"`
}

// PublicRoom is what a participant (or someone holding the full link) sees.
type PublicRoom struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	HostName        string    `json:"host_name"`
	Status          string    `json:"status"` // active | ended
	Lobby           bool      `json:"lobby"`
	Locked          bool      `json:"locked"`
	MaxParticipants int       `json:"max_participants"`
	Participants    int       `json:"participants"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// RoomView is a room in the host's list.
type RoomView struct {
	Room
	Status       string       `json:"status"` // active | ended
	Participants int          `json:"participants"`
	Waiting      int          `json:"waiting"`
	Path         string       `json:"path"`
	People       []PersonView `json:"people,omitempty"`
}

// PersonView is a participant in the host's detail view.
type PersonView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	IsHost     bool       `json:"is_host"`
	State      string     `json:"state"`
	LeftReason string     `json:"left_reason,omitempty"`
	JoinedAt   time.Time  `json:"joined_at"`
	LastSeen   time.Time  `json:"last_seen"`
	LeftAt     *time.Time `json:"left_at,omitempty"`
}

func (s *Service) status(r *Room) string {
	if r.EndedAt != nil || !s.now().Before(r.ExpiresAt) {
		return "ended"
	}
	return "active"
}

func (s *Service) publicRoom(r *Room, active int) PublicRoom {
	return PublicRoom{ID: r.ID, Title: r.Title, HostName: r.HostName, Status: s.status(r), Lobby: r.Lobby, Locked: r.Locked,
		MaxParticipants: r.MaxParticipants, Participants: active, ExpiresAt: r.ExpiresAt}
}

// ---------------------------------------------------------------- create & host views

// CreateInput is the body of POST /secure-calls.
type CreateInput struct {
	Title           string `json:"title"`
	Expires         string `json:"expires"` // 1h | 24h | 7d
	Lobby           bool   `json:"lobby"`
	MaxParticipants int    `json:"max_participants"`
	// LinkCheck is HKDF(key, "join-check"), 16 bytes base64url, computed in
	// the host's browser. Only its SHA-256 is stored.
	LinkCheck string `json:"link_check"`
}

// Expiry options.
var expiryOptions = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

// CanCreate reports whether a role may start secure calls (manager level or above).
func CanCreate(role string) bool {
	switch strings.ToLower(role) {
	case "super_admin", "admin", "owner", "org_admin", "manager":
		return true
	}
	return false
}

func cleanName(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		// control characters, zero-width and bidi override / isolate marks (a name must not reorder the page)
		if unicode.IsControl(r) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
	}
	return strings.TrimSpace(s)
}

// Create starts a room for the host.
func (s *Service) Create(org, user, role, hostName string, in CreateInput) (*RoomView, error) {
	if org == "" || user == "" {
		return nil, refuse(http.StatusUnauthorized, "NO_ORGANIZATION", "Sign in to start a secure call.")
	}
	if !CanCreate(role) {
		return nil, errNoCreate
	}
	if !s.lim.allow("create:"+user, s.Limits.CreatePerUserPerHr, time.Hour, s.now()) {
		return nil, errLimited
	}
	check, ok := HashCheck(in.LinkCheck)
	if !ok {
		return nil, invalid("link_check is missing or malformed (16 bytes, base64url) — it is computed from the call key in your browser.")
	}
	exp := in.Expires
	if exp == "" {
		exp = "24h"
	}
	d, ok := expiryOptions[exp]
	if !ok {
		return nil, invalid("expires must be 1h, 24h or 7d")
	}
	max := in.MaxParticipants
	if max == 0 {
		max = s.Limits.MaxParticipants
	}
	if max < 2 || max > s.Limits.MaxParticipants {
		return nil, invalid(fmt.Sprintf("max_participants must be between 2 and %d", s.Limits.MaxParticipants))
	}
	title := cleanName(in.Title, 80)
	now := s.now()
	r := Room{ID: NewRoomID(), OrganizationID: org, HostUserID: user, HostName: cleanName(hostName, 80), Title: title, CheckHash: check,
		Lobby: in.Lobby, MaxParticipants: max, CreatedAt: now, ExpiresAt: now.Add(d)}
	if err := s.DB.Create(&r).Error; err != nil {
		return nil, err
	}
	return s.view(&r, 0, 0), nil
}

func (s *Service) view(r *Room, active, waiting int) *RoomView {
	return &RoomView{Room: *r, Status: s.status(r), Participants: active, Waiting: waiting, Path: "/c/" + r.ID}
}

type countRow struct {
	RoomID string
	State  string
	N      int
}

func (s *Service) counts(db *gorm.DB, ids []string) map[string]map[string]int {
	out := map[string]map[string]int{}
	if len(ids) == 0 {
		return out
	}
	var rows []countRow
	db.Raw(`SELECT room_id, state, count(*) AS n FROM seccall_participants WHERE room_id IN ? AND state IN ? GROUP BY room_id, state`,
		ids, []string{StateActive, StateLobby}).Scan(&rows)
	for _, r := range rows {
		if out[r.RoomID] == nil {
			out[r.RoomID] = map[string]int{}
		}
		out[r.RoomID][r.State] = r.N
	}
	return out
}

// List returns the host's calls: status "active", "ended" or "" (both).
func (s *Service) List(org, user, status string) ([]RoomView, error) {
	now := s.now()
	q := s.DB.Model(&Room{}).Where("organization_id = ? AND host_user_id = ?", org, user)
	switch status {
	case "active":
		q = q.Where("ended_at IS NULL AND expires_at > ?", now)
	case "ended":
		q = q.Where("ended_at IS NOT NULL OR expires_at <= ?", now)
	}
	var rooms []Room
	if err := q.Order("created_at DESC").Limit(200).Find(&rooms).Error; err != nil {
		return nil, err
	}
	ids := make([]string, len(rooms))
	for i := range rooms {
		ids[i] = rooms[i].ID
	}
	c := s.counts(s.DB, ids)
	out := make([]RoomView, 0, len(rooms))
	for i := range rooms {
		v := s.view(&rooms[i], c[rooms[i].ID][StateActive], c[rooms[i].ID][StateLobby])
		if v.Status == "ended" {
			v.Participants, v.Waiting = 0, 0
		}
		out = append(out, *v)
	}
	return out, nil
}

// hostRoom loads a room of the host (org + creator); others get "not found".
func (s *Service) hostRoom(db *gorm.DB, org, user, id string, lock bool) (*Room, error) {
	if !ValidRoomID(id) {
		return nil, errNotHost
	}
	q := db.Where("id = ? AND organization_id = ? AND host_user_id = ?", id, org, user)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var r Room
	res := q.Limit(1).Find(&r)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, errNotHost
	}
	return &r, nil
}

// Get returns one of the host's calls with everyone who joined.
func (s *Service) Get(org, user, id string) (*RoomView, error) {
	r, err := s.hostRoom(s.DB, org, user, id, false)
	if err != nil {
		return nil, err
	}
	var ps []Participant
	s.DB.Where("room_id = ?", r.ID).Order("created_at").Limit(500).Find(&ps)
	active, waiting := 0, 0
	people := make([]PersonView, 0, len(ps))
	for _, p := range ps {
		switch p.State {
		case StateActive:
			active++
		case StateLobby:
			waiting++
		}
		people = append(people, PersonView{ID: p.ID, Name: p.Name, IsHost: p.IsHost, State: p.State, LeftReason: p.LeftReason, JoinedAt: p.CreatedAt, LastSeen: p.LastSeen, LeftAt: p.LeftAt})
	}
	v := s.view(r, active, waiting)
	v.People = people
	return v, nil
}

// ---------------------------------------------------------------- relay core

type event map[string]any

func evt(to, from string, e event) Message {
	b, _ := json.Marshal(e)
	return Message{ToPID: to, FromPID: from, Kind: "evt", Body: string(b)}
}

const toHosts = "@hosts"

// emit appends messages to a room's relay (in tx, which must run under the
// room's row lock or take it here through the UPDATE) and announces them.
func (s *Service) emit(tx *gorm.DB, roomID string, msgs ...Message) (int64, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	var last int64
	if err := tx.Raw(`UPDATE seccall_rooms SET last_seq = last_seq + ? WHERE id = ? RETURNING last_seq`, len(msgs), roomID).Scan(&last).Error; err != nil {
		return 0, err
	}
	first := last - int64(len(msgs)) + 1
	now := s.now()
	for i := range msgs {
		msgs[i].RoomID, msgs[i].Seq, msgs[i].CreatedAt = roomID, first+int64(i), now
	}
	if err := tx.Create(&msgs).Error; err != nil {
		return 0, err
	}
	if err := tx.Exec(`SELECT pg_notify(?, ?)`, notifyChannel, roomID).Error; err != nil {
		return 0, err
	}
	return last, nil
}

func lockRoom(tx *gorm.DB, id string) (*Room, error) {
	var r Room
	res := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Limit(1).Find(&r)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, errNotFound
	}
	return &r, nil
}

func (s *Service) peers(db *gorm.DB, roomID, state string) []Peer {
	var ps []Participant
	db.Where("room_id = ? AND state = ?", roomID, state).Order("created_at").Limit(50).Find(&ps)
	out := make([]Peer, 0, len(ps))
	for _, p := range ps {
		since := p.CreatedAt
		if p.AdmittedAt != nil {
			since = *p.AdmittedAt
		}
		out = append(out, Peer{ID: p.ID, Name: p.Name, IsHost: p.IsHost, Since: since})
	}
	return out
}

func (s *Service) countStates(db *gorm.DB, roomID string, states ...string) int {
	var n int64
	db.Model(&Participant{}).Where("room_id = ? AND state IN ?", roomID, states).Count(&n)
	return int(n)
}

// ---------------------------------------------------------------- preview & join

// Preview returns the room for someone holding the full link (the check
// proves it); used by the pre-join screen.
func (s *Service) Preview(roomID, check, ip string) (*PublicRoom, error) {
	if !s.lim.allow("join:"+ip, s.Limits.JoinPerIPPerMin, time.Minute, s.now()) {
		return nil, errLimited
	}
	r, err := s.room(roomID)
	if err != nil {
		return nil, err
	}
	if s.status(r) == "ended" {
		return nil, errEnded
	}
	if !checkMatches(r, check) {
		return nil, errWrongLink
	}
	pr := s.publicRoom(r, s.countStates(s.DB, r.ID, StateActive))
	return &pr, nil
}

// Status says whether a room exists and is still open (no secret needed).
func (s *Service) Status(roomID string) (string, error) {
	r, err := s.room(roomID)
	if err != nil {
		return "", err
	}
	return s.status(r), nil
}

func (s *Service) room(id string) (*Room, error) {
	if !ValidRoomID(id) {
		return nil, errNotFound
	}
	var r Room
	res := s.DB.Where("id = ?", id).Limit(1).Find(&r)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, errNotFound
	}
	return &r, nil
}

// JoinInput is the body of a join.
type JoinInput struct {
	Name      string `json:"name"`
	LinkCheck string `json:"link_check"`
}

// JoinResult is handed to the browser once: the token is not stored.
type JoinResult struct {
	ParticipantID string     `json:"participant_id"`
	Token         string     `json:"token"`
	State         string     `json:"state"`
	IsHost        bool       `json:"is_host"`
	Since         int64      `json:"since"`
	Room          PublicRoom `json:"room"`
	Peers         []Peer     `json:"peers"`
	Lobby         []Peer     `json:"lobby,omitempty"`
}

// JoinGuest adds someone with the link (no account needed).
func (s *Service) JoinGuest(roomID string, in JoinInput, ip string) (*JoinResult, error) {
	now := s.now()
	if !s.lim.allow("join:"+ip, s.Limits.JoinPerIPPerMin, time.Minute, now) {
		return nil, errLimited
	}
	return s.join(roomID, in, false, "", "")
}

// JoinHost adds the signed-in creator: no lobby, and a locked room still
// lets them in.
func (s *Service) JoinHost(org, user, roomID string, in JoinInput) (*JoinResult, error) {
	if _, err := s.hostRoom(s.DB, org, user, roomID, false); err != nil {
		return nil, err
	}
	return s.join(roomID, in, true, user, org)
}

func (s *Service) join(roomID string, in JoinInput, host bool, user, org string) (*JoinResult, error) {
	name := cleanName(in.Name, 40)
	if name == "" {
		return nil, invalid("Enter your name so the others know who you are.")
	}
	if !ValidRoomID(roomID) {
		return nil, errNotFound
	}
	var out *JoinResult
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		r, err := lockRoom(tx, roomID)
		if err != nil {
			return err
		}
		now := s.now()
		if s.status(r) == "ended" {
			return errEnded
		}
		if !checkMatches(r, in.LinkCheck) {
			return errWrongLink
		}
		if host && (r.OrganizationID != org || r.HostUserID != user) {
			return errNotHost
		}
		if !host {
			if r.Locked {
				return errLocked
			}
			var recent int64
			tx.Model(&Participant{}).Where("room_id = ? AND created_at > ?", r.ID, now.Add(-10*time.Minute)).Count(&recent)
			if int(recent) >= s.Limits.JoinsPerRoomPer10m {
				return errLimited
			}
		}
		active := s.countStates(tx, r.ID, StateActive)
		waiting := s.countStates(tx, r.ID, StateLobby)
		if active+waiting >= r.MaxParticipants {
			return errFull(r.MaxParticipants)
		}
		tok := newToken()
		p := Participant{ID: newParticipantID(), RoomID: r.ID, Name: name, IsHost: host, UserID: user, TokenHash: HashToken(tok),
			State: StateActive, CreatedAt: now, LastSeen: now}
		if r.Lobby && !host {
			p.State = StateLobby
		} else {
			p.AdmittedAt = &now
		}
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		upd := map[string]any{"join_count": gorm.Expr("join_count + 1")}
		if p.State == StateActive && active+1 > r.PeakParticipants {
			upd["peak_participants"] = active + 1
		}
		if err := tx.Model(&Room{}).Where("id = ?", r.ID).Updates(upd).Error; err != nil {
			return err
		}
		var msgs []Message
		if p.State == StateActive {
			msgs = append(msgs, evt("", p.ID, event{"type": "join", "pid": p.ID, "name": p.Name, "host": p.IsHost}))
		} else {
			msgs = append(msgs, evt(toHosts, p.ID, event{"type": "knock", "pid": p.ID, "name": p.Name}))
		}
		last, err := s.emit(tx, r.ID, msgs...)
		if err != nil {
			return err
		}
		res := &JoinResult{ParticipantID: p.ID, Token: tok, State: p.State, IsHost: host, Since: last}
		if p.State == StateActive {
			active++
			res.Peers = without(s.peers(tx, r.ID, StateActive), p.ID)
		} else {
			res.Peers = []Peer{}
		}
		res.Room = s.publicRoom(r, active)
		if host {
			res.Lobby = s.peers(tx, r.ID, StateLobby)
		}
		out = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.hub.wake(roomID)
	return out, nil
}

func without(ps []Peer, id string) []Peer {
	out := ps[:0]
	for _, p := range ps {
		if p.ID != id {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------- participant auth

// participant finds the participant for a token in a room.
func (s *Service) participant(db *gorm.DB, roomID, token string) (*Participant, error) {
	if !strings.HasPrefix(token, tokenPrefix) || len(token) < 20 || !ValidRoomID(roomID) {
		return nil, errToken
	}
	var p Participant
	res := db.Where("room_id = ? AND token_hash = ?", roomID, HashToken(token)).Limit(1).Find(&p)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, errToken
	}
	return &p, nil
}

func stateErr(p *Participant) error {
	switch p.State {
	case StateRemoved:
		return errRemoved
	case StateDenied:
		return errDenied
	case StateLeft:
		if p.LeftReason == "ended" {
			return errEnded
		}
		if !strings.HasPrefix(p.LeftReason, "stale") {
			return errToken
		}
	}
	return nil
}

// ---------------------------------------------------------------- signal

// SignalIn is one message from a browser: Ciphertext is "<iv>.<ct>" and is
// never decrypted here. To is a participant id or "*" (everyone).
type SignalIn struct {
	To         string `json:"to"`
	Ciphertext string `json:"ciphertext"`
}

// SignalResult says how many messages were queued and how many were dropped
// because their recipient has just left.
type SignalResult struct {
	Queued  int   `json:"queued"`
	Dropped int   `json:"dropped"`
	Seq     int64 `json:"seq"`
}

// Signal relays encrypted messages from a participant.
func (s *Service) Signal(roomID, token string, in []SignalIn) (*SignalResult, error) {
	if len(in) == 0 {
		return nil, invalid("No messages.")
	}
	if len(in) > s.Limits.BatchMax {
		return nil, invalid(fmt.Sprintf("At most %d messages per request.", s.Limits.BatchMax))
	}
	for i := range in {
		in[i].To = strings.TrimSpace(in[i].To)
		if in[i].To == "" {
			return nil, invalid("Each message needs a recipient (a participant id or \"*\").")
		}
		if err := ValidateCiphertext(in[i].Ciphertext, s.Limits.MaxCiphertext); err != nil {
			return nil, refuse(http.StatusBadRequest, "CIPHERTEXT_REQUIRED", err.Error())
		}
	}
	p, err := s.participant(s.DB, roomID, token)
	if err != nil {
		return nil, err
	}
	if err := stateErr(p); err != nil {
		return nil, err
	}
	if p.State != StateActive {
		return nil, errWaiting
	}
	now := s.now()
	if !s.lim.allow("sig:"+p.ID, s.Limits.SignalPerPartPerMin, time.Minute, now) {
		return nil, errLimited
	}
	out := &SignalResult{}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		r, err := lockRoom(tx, roomID)
		if err != nil {
			return err
		}
		if s.status(r) == "ended" {
			return errEnded
		}
		var recent int64
		tx.Model(&Message{}).Where("room_id = ? AND created_at > ?", r.ID, now.Add(-time.Minute)).Count(&recent)
		if int(recent)+len(in) > s.Limits.RoomMsgsPerMin {
			return errLimited
		}
		var ids []string
		tx.Model(&Participant{}).Where("room_id = ? AND state = ?", r.ID, StateActive).Pluck("id", &ids)
		live := map[string]bool{}
		for _, id := range ids {
			live[id] = true
		}
		var msgs []Message
		for _, m := range in {
			to := m.To
			if to == "*" {
				to = ""
			} else if to == p.ID || !live[to] {
				out.Dropped++
				continue
			}
			msgs = append(msgs, Message{FromPID: p.ID, ToPID: to, Kind: "sig", Body: m.Ciphertext})
		}
		if len(msgs) == 0 {
			return nil
		}
		last, err := s.emit(tx, r.ID, msgs...)
		out.Queued, out.Seq = len(msgs), last
		return err
	})
	if err != nil {
		return nil, err
	}
	if out.Queued > 0 {
		s.hub.wake(roomID)
	}
	return out, nil
}

// ---------------------------------------------------------------- poll

// OutMessage is a relayed message as delivered. For kind "sig", Ciphertext is
// exactly what the sender posted and To is the recipient id or "*".
type OutMessage struct {
	Seq        int64           `json:"seq"`
	From       string          `json:"from"`
	To         string          `json:"to"`
	Kind       string          `json:"kind"`
	Ciphertext string          `json:"ciphertext,omitempty"`
	Event      json.RawMessage `json:"event,omitempty"`
}

// You is the polling participant.
type You struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	IsHost bool   `json:"is_host"`
}

// PollResult is one long-poll answer.
type PollResult struct {
	Messages []OutMessage `json:"messages"`
	Next     int64        `json:"next"`
	You      You          `json:"you"`
	Peers    []Peer       `json:"peers"`
	Lobby    []Peer       `json:"lobby,omitempty"`
	Room     PublicRoom   `json:"room"`
	Relay    string       `json:"relay"` // notify | polling
}

// Poll waits (up to wait, at most Limits.PollWait) for messages after since.
// It doubles as the heartbeat. A participant who went quiet ("stale") and
// polls again is put back in the call.
func (s *Service) Poll(ctx context.Context, roomID, token string, since int64, wait time.Duration, ip string) (*PollResult, error) {
	if !s.lim.allow("poll:"+ip, s.Limits.PollPerIPPerMin, time.Minute, s.now()) {
		return nil, errLimited
	}
	if wait <= 0 || wait > s.Limits.PollWait {
		wait = s.Limits.PollWait
	}
	if since < 0 {
		since = 0
	}
	p, err := s.participant(s.DB, roomID, token)
	if err != nil {
		return nil, err
	}
	if err := stateErr(p); err != nil {
		return nil, err
	}
	if p.State == StateLeft {
		if p, err = s.resume(roomID, p.ID); err != nil {
			return nil, err
		}
	}
	s.touch(p)
	deadline := time.Now().Add(wait)
	for {
		ch, cancel := s.hub.subscribe(roomID)
		res, done, err := s.collect(roomID, p.ID, since)
		if errors.Is(err, errResume) {
			// went quiet while waiting (cleanup ran): put back and go on
			cancel()
			if p, err = s.resume(roomID, p.ID); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil || done || len(res.Messages) > 0 || !time.Now().Before(deadline) {
			cancel()
			if res != nil {
				s.touch(&Participant{ID: p.ID})
			}
			return res, err
		}
		since = res.Next
		recheck := 3 * time.Second
		if !s.listenerUp() {
			recheck = time.Second
		}
		if left := time.Until(deadline); left < recheck {
			recheck = left
		}
		t := time.NewTimer(recheck)
		select {
		case <-ctx.Done():
			t.Stop()
			cancel()
			return nil, ctx.Err()
		case <-ch:
		case <-t.C:
		}
		t.Stop()
		cancel()
	}
}

func (s *Service) touch(p *Participant) {
	now := s.now()
	s.DB.Model(&Participant{}).Where("id = ? AND state IN ?", p.ID, []string{StateActive, StateLobby}).Update("last_seen", now)
}

// collect reads the participant's pending messages. done is true when the
// poll must answer now (state changed).
func (s *Service) collect(roomID, pid string, since int64) (*PollResult, bool, error) {
	var r Room
	if res := s.DB.Where("id = ?", roomID).Limit(1).Find(&r); res.Error != nil || res.RowsAffected == 0 {
		if res.Error != nil {
			return nil, true, res.Error
		}
		return nil, true, errNotFound
	}
	if s.status(&r) == "ended" {
		return nil, true, errEnded
	}
	var p Participant
	if res := s.DB.Where("id = ?", pid).Limit(1).Find(&p); res.Error != nil || res.RowsAffected == 0 {
		return nil, true, errToken
	}
	if err := stateErr(&p); err != nil {
		return nil, true, err
	}
	if p.State == StateLeft {
		return nil, true, errResume
	}
	var rows []Message
	q := s.DB.Where("room_id = ? AND seq > ? AND seq <= ?", roomID, since, r.LastSeq)
	if p.State == StateLobby {
		q = q.Where("to_pid = ?", pid)
	} else if p.IsHost {
		q = q.Where("to_pid = ? OR to_pid = ? OR (to_pid = '' AND from_pid <> ?)", pid, toHosts, pid)
	} else {
		q = q.Where("to_pid = ? OR (to_pid = '' AND from_pid <> ?)", pid, pid)
	}
	if err := q.Order("seq").Limit(300).Find(&rows).Error; err != nil {
		return nil, true, err
	}
	next := r.LastSeq
	if len(rows) == 300 {
		next = rows[len(rows)-1].Seq
	}
	if next < since {
		next = since
	}
	res := &PollResult{Messages: make([]OutMessage, 0, len(rows)), Next: next,
		You: You{ID: p.ID, Name: p.Name, State: p.State, IsHost: p.IsHost}, Relay: "notify"}
	if !s.listenerUp() {
		res.Relay = "polling"
	}
	for _, m := range rows {
		o := OutMessage{Seq: m.Seq, From: m.FromPID, To: m.ToPID, Kind: m.Kind}
		if m.Kind == "sig" {
			o.Ciphertext = m.Body
			if o.To == "" {
				o.To = "*"
			}
		} else {
			o.Event = json.RawMessage(m.Body)
			if o.To == "" {
				o.To = "*"
			}
		}
		res.Messages = append(res.Messages, o)
	}
	if p.State == StateActive {
		res.Peers = without(s.peers(s.DB, roomID, StateActive), p.ID)
	} else {
		res.Peers = []Peer{}
	}
	if p.IsHost {
		res.Lobby = s.peers(s.DB, roomID, StateLobby)
	}
	res.Room = s.publicRoom(&r, s.countStates(s.DB, roomID, StateActive))
	return res, false, nil
}

// resume puts a participant who went quiet back where they were.
func (s *Service) resume(roomID, pid string) (*Participant, error) {
	var out Participant
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		r, err := lockRoom(tx, roomID)
		if err != nil {
			return err
		}
		if s.status(r) == "ended" {
			return errEnded
		}
		var p Participant
		if res := tx.Where("id = ?", pid).Limit(1).Find(&p); res.Error != nil || res.RowsAffected == 0 {
			return errToken
		}
		if p.State != StateLeft {
			out = p
			return nil
		}
		if !strings.HasPrefix(p.LeftReason, "stale") {
			return errToken
		}
		state := StateActive
		if p.LeftReason == "stale_lobby" {
			state = StateLobby
		}
		if s.countStates(tx, r.ID, StateActive, StateLobby) >= r.MaxParticipants {
			return errFull(r.MaxParticipants)
		}
		now := s.now()
		if err := tx.Model(&Participant{}).Where("id = ?", p.ID).Updates(map[string]any{"state": state, "left_reason": "", "left_at": nil, "last_seen": now}).Error; err != nil {
			return err
		}
		p.State, p.LeftReason, p.LeftAt, p.LastSeen = state, "", nil, now
		if state == StateActive {
			_, err = s.emit(tx, r.ID, evt("", p.ID, event{"type": "join", "pid": p.ID, "name": p.Name, "host": p.IsHost, "resumed": true}))
		} else {
			_, err = s.emit(tx, r.ID, evt(toHosts, p.ID, event{"type": "knock", "pid": p.ID, "name": p.Name}))
		}
		out = p
		return err
	})
	if err != nil {
		return nil, err
	}
	s.hub.wake(roomID)
	return &out, nil
}

// ---------------------------------------------------------------- leave

// Leave takes a participant out of the call (their token stops working).
func (s *Service) Leave(roomID, token string) error {
	p, err := s.participant(s.DB, roomID, token)
	if err != nil {
		return err
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		r, err := lockRoom(tx, roomID)
		if err != nil {
			return err
		}
		now := s.now()
		was := p.State
		if err := tx.Model(&Participant{}).Where("id = ?", p.ID).Updates(map[string]any{"state": StateLeft, "left_reason": "left", "left_at": now, "token_hash": ""}).Error; err != nil {
			return err
		}
		if s.status(r) == "ended" {
			return nil
		}
		switch was {
		case StateActive:
			_, err = s.emit(tx, r.ID, evt("", p.ID, event{"type": "leave", "pid": p.ID, "reason": "left"}))
		case StateLobby:
			_, err = s.emit(tx, r.ID, evt(toHosts, p.ID, event{"type": "lobby_left", "pid": p.ID}))
		}
		return err
	})
	if err == nil {
		s.hub.wake(roomID)
	}
	return err
}

// ---------------------------------------------------------------- host actions

// hostAct runs fn on one of the host's open rooms under its row lock.
func (s *Service) hostAct(org, user, id string, fn func(tx *gorm.DB, r *Room) error) error {
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		r, err := s.hostRoom(tx, org, user, id, true)
		if err != nil {
			return err
		}
		if s.status(r) == "ended" {
			return errEnded
		}
		return fn(tx, r)
	})
	if err == nil {
		s.hub.wake(id)
	}
	return err
}

// End ends the call for everyone.
func (s *Service) End(org, user, id string) error {
	return s.hostAct(org, user, id, func(tx *gorm.DB, r *Room) error {
		return s.endRoom(tx, r, "host")
	})
}

func (s *Service) endRoom(tx *gorm.DB, r *Room, reason string) error {
	now := s.now()
	at := now
	if reason == "expired" && r.ExpiresAt.Before(now) {
		at = r.ExpiresAt
	}
	if err := tx.Model(&Room{}).Where("id = ?", r.ID).Updates(map[string]any{"ended_at": at, "end_reason": reason}).Error; err != nil {
		return err
	}
	if err := tx.Model(&Participant{}).Where("room_id = ? AND state IN ?", r.ID, []string{StateActive, StateLobby}).
		Updates(map[string]any{"state": StateLeft, "left_reason": "ended", "left_at": now}).Error; err != nil {
		return err
	}
	if err := tx.Where("room_id = ?", r.ID).Delete(&Message{}).Error; err != nil {
		return err
	}
	// wake every long poll of the room (they answer "This call has ended")
	return tx.Exec(`SELECT pg_notify(?, ?)`, notifyChannel, r.ID).Error
}

// SetLocked locks (nobody new can join, not even into the lobby) or unlocks.
func (s *Service) SetLocked(org, user, id string, locked bool) error {
	return s.hostAct(org, user, id, func(tx *gorm.DB, r *Room) error {
		if err := tx.Model(&Room{}).Where("id = ?", r.ID).Update("locked", locked).Error; err != nil {
			return err
		}
		_, err := s.emit(tx, r.ID, evt("", "", event{"type": "lock", "locked": locked}))
		return err
	})
}

// SetLobby turns "Ask me before people join" on or off. Turning it off lets
// everyone waiting in (as far as there is room).
func (s *Service) SetLobby(org, user, id string, on bool) error {
	return s.hostAct(org, user, id, func(tx *gorm.DB, r *Room) error {
		if err := tx.Model(&Room{}).Where("id = ?", r.ID).Update("lobby", on).Error; err != nil {
			return err
		}
		msgs := []Message{evt("", "", event{"type": "lobby", "enabled": on})}
		if !on {
			var waiting []Participant
			tx.Where("room_id = ? AND state = ?", r.ID, StateLobby).Order("created_at").Find(&waiting)
			active := s.countStates(tx, r.ID, StateActive)
			for _, w := range waiting {
				if active >= r.MaxParticipants {
					break
				}
				am, err := s.admit(tx, r, &w)
				if err != nil {
					return err
				}
				msgs = append(msgs, am...)
				active++
			}
		}
		_, err := s.emit(tx, r.ID, msgs...)
		return err
	})
}

func (s *Service) lobbyOne(tx *gorm.DB, r *Room, pid string) (*Participant, error) {
	var p Participant
	res := tx.Where("id = ? AND room_id = ?", pid, r.ID).Limit(1).Find(&p)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, refuse(http.StatusNotFound, "NOT_FOUND", "That person isn't in this call any more.")
	}
	return &p, nil
}

func (s *Service) admit(tx *gorm.DB, r *Room, p *Participant) ([]Message, error) {
	now := s.now()
	if err := tx.Model(&Participant{}).Where("id = ?", p.ID).Updates(map[string]any{"state": StateActive, "admitted_at": now, "last_seen": now}).Error; err != nil {
		return nil, err
	}
	active := s.countStates(tx, r.ID, StateActive)
	if active > r.PeakParticipants {
		tx.Model(&Room{}).Where("id = ?", r.ID).Update("peak_participants", active)
	}
	return []Message{
		evt(p.ID, "", event{"type": "admitted"}),
		evt("", p.ID, event{"type": "join", "pid": p.ID, "name": p.Name, "host": false}),
	}, nil
}

// Admit lets someone in from the lobby.
func (s *Service) Admit(org, user, id, pid string) error {
	return s.hostAct(org, user, id, func(tx *gorm.DB, r *Room) error {
		p, err := s.lobbyOne(tx, r, pid)
		if err != nil {
			return err
		}
		if p.State != StateLobby {
			return refuse(http.StatusConflict, "NOT_WAITING", "That person isn't waiting any more.")
		}
		if s.countStates(tx, r.ID, StateActive) >= r.MaxParticipants {
			return errFull(r.MaxParticipants)
		}
		msgs, err := s.admit(tx, r, p)
		if err != nil {
			return err
		}
		msgs = append(msgs, evt(toHosts, "", event{"type": "lobby_update"}))
		_, err = s.emit(tx, r.ID, msgs...)
		return err
	})
}

// Deny refuses someone waiting in the lobby.
func (s *Service) Deny(org, user, id, pid string) error {
	return s.hostAct(org, user, id, func(tx *gorm.DB, r *Room) error {
		p, err := s.lobbyOne(tx, r, pid)
		if err != nil {
			return err
		}
		if p.State != StateLobby {
			return refuse(http.StatusConflict, "NOT_WAITING", "That person isn't waiting any more.")
		}
		now := s.now()
		if err := tx.Model(&Participant{}).Where("id = ?", p.ID).Updates(map[string]any{"state": StateDenied, "left_at": now}).Error; err != nil {
			return err
		}
		_, err = s.emit(tx, r.ID, evt(p.ID, "", event{"type": "denied"}), evt(toHosts, "", event{"type": "lobby_update"}))
		return err
	})
}

// Remove takes someone out of the call; their token stops working and they
// cannot rejoin with it (a new join with the link is a new participant —
// lock the room to stop that).
func (s *Service) Remove(org, user, id, pid string) error {
	return s.hostAct(org, user, id, func(tx *gorm.DB, r *Room) error {
		p, err := s.lobbyOne(tx, r, pid)
		if err != nil {
			return err
		}
		if p.State != StateActive && p.State != StateLobby {
			return refuse(http.StatusConflict, "NOT_IN_CALL", "That person has already left.")
		}
		if p.IsHost && p.UserID == user {
			return invalid("You can't remove yourself — use Leave or End for all.")
		}
		now := s.now()
		was := p.State
		if err := tx.Model(&Participant{}).Where("id = ?", p.ID).Updates(map[string]any{"state": StateRemoved, "left_at": now}).Error; err != nil {
			return err
		}
		msgs := []Message{evt(p.ID, "", event{"type": "removed"})}
		if was == StateActive {
			msgs = append(msgs, evt("", p.ID, event{"type": "leave", "pid": p.ID, "reason": "removed", "name": p.Name}))
		} else {
			msgs = append(msgs, evt(toHosts, "", event{"type": "lobby_update"}))
		}
		_, err = s.emit(tx, r.ID, msgs...)
		return err
	})
}

// UpdateInput is the body of PATCH /secure-calls/:id.
type UpdateInput struct {
	Lobby  *bool   `json:"lobby"`
	Locked *bool   `json:"locked"`
	Title  *string `json:"title"`
}

// Update changes the title and/or the lobby and lock switches.
func (s *Service) Update(org, user, id string, in UpdateInput) error {
	if in.Title != nil {
		t := cleanName(*in.Title, 80)
		if err := s.hostAct(org, user, id, func(tx *gorm.DB, r *Room) error {
			return tx.Model(&Room{}).Where("id = ?", r.ID).Update("title", t).Error
		}); err != nil {
			return err
		}
	}
	if in.Lobby != nil {
		if err := s.SetLobby(org, user, id, *in.Lobby); err != nil {
			return err
		}
	}
	if in.Locked != nil {
		if err := s.SetLocked(org, user, id, *in.Locked); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- cleanup

// CleanupResult counts what one cleanup pass did.
type CleanupResult struct {
	Expired  int   `json:"expired"`
	Stale    int   `json:"stale"`
	Messages int64 `json:"messages"`
	Purged   int64 `json:"purged"`
}

// Cleanup ends expired rooms, marks quiet participants as gone, deletes
// relayed messages older than MessageTTL and forgets rooms that ended more
// than KeepEnded ago.
func (s *Service) Cleanup(ctx context.Context) CleanupResult {
	var out CleanupResult
	now := s.now()
	db := s.DB.WithContext(ctx)

	var expired []Room
	db.Where("ended_at IS NULL AND expires_at <= ?", now).Limit(500).Find(&expired)
	for i := range expired {
		r := expired[i]
		if err := db.Transaction(func(tx *gorm.DB) error {
			lr, err := lockRoom(tx, r.ID)
			if err != nil || lr.EndedAt != nil {
				return err
			}
			return s.endRoom(tx, lr, "expired")
		}); err == nil {
			out.Expired++
			s.hub.wake(r.ID)
		}
	}

	type staleRow struct {
		ID     string
		RoomID string
		State  string
		Name   string
	}
	var stale []staleRow
	db.Raw(`SELECT p.id, p.room_id, p.state, p.name FROM seccall_participants p JOIN seccall_rooms r ON r.id = p.room_id
		WHERE p.state IN ? AND p.last_seen < ? AND r.ended_at IS NULL LIMIT 1000`, []string{StateActive, StateLobby}, now.Add(-s.Limits.StaleAfter)).Scan(&stale)
	for _, st := range stale {
		st := st
		if err := db.Transaction(func(tx *gorm.DB) error {
			if _, err := lockRoom(tx, st.RoomID); err != nil {
				return err
			}
			reason := "stale"
			if st.State == StateLobby {
				reason = "stale_lobby"
			}
			res := tx.Model(&Participant{}).Where("id = ? AND state = ? AND last_seen < ?", st.ID, st.State, now.Add(-s.Limits.StaleAfter)).
				Updates(map[string]any{"state": StateLeft, "left_reason": reason, "left_at": now})
			if res.Error != nil || res.RowsAffected == 0 {
				return res.Error
			}
			var m Message
			if st.State == StateActive {
				m = evt("", st.ID, event{"type": "leave", "pid": st.ID, "reason": "stale", "name": st.Name})
			} else {
				m = evt(toHosts, st.ID, event{"type": "lobby_left", "pid": st.ID})
			}
			_, err := s.emit(tx, st.RoomID, m)
			return err
		}); err == nil {
			out.Stale++
			s.hub.wake(st.RoomID)
		}
	}

	res := db.Where("created_at < ?", now.Add(-s.Limits.MessageTTL)).Delete(&Message{})
	out.Messages = res.RowsAffected

	var old []string
	db.Model(&Room{}).Where("(ended_at IS NOT NULL AND ended_at < ?) OR (ended_at IS NULL AND expires_at < ?)", now.Add(-s.Limits.KeepEnded), now.Add(-s.Limits.KeepEnded)).Limit(1000).Pluck("id", &old)
	if len(old) > 0 {
		db.Where("room_id IN ?", old).Delete(&Message{})
		db.Where("room_id IN ?", old).Delete(&Participant{})
		out.Purged = db.Where("id IN ?", old).Delete(&Room{}).RowsAffected
	}
	return out
}

// CleanupLocked runs Cleanup on one instance at a time (Postgres advisory
// lock). ok is false when another instance holds the lock.
func (s *Service) CleanupLocked(ctx context.Context) (res CleanupResult, ok bool) {
	err := s.DB.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", "seccall:cleanup").Scan(&ok).Error; err != nil {
			return err
		}
		if !ok {
			return nil
		}
		defer tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", "seccall:cleanup")
		res = s.Cleanup(ctx)
		return nil
	})
	if err != nil {
		return res, false
	}
	return res, ok
}

// StartScheduler runs CleanupLocked every `every` (after firstDelay).
func (s *Service) StartScheduler(ctx context.Context, every, firstDelay time.Duration) {
	if every <= 0 {
		every = 15 * time.Second
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("seccall: cleanup stopped: %v", r)
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(firstDelay):
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			if r, ok := s.CleanupLocked(ctx); ok && (r.Expired+r.Stale > 0 || r.Purged > 0) {
				log.Printf("seccall: cleanup — %d rooms expired, %d quiet participants, %d messages, %d old rooms removed", r.Expired, r.Stale, r.Messages, r.Purged)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}
