package seccall

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Limits are the abuse limits of the relay. Per-IP and per-participant
// counters live in each instance's memory (a client that is spread over N
// instances gets at most N times the limit); per-room limits are counted in
// Postgres, so they hold across instances.
type Limits struct {
	MaxParticipants     int           // hard cap per room (mesh size)
	JoinPerIPPerMin     int           // join + preview attempts per IP per minute
	JoinsPerRoomPer10m  int           // join attempts per room per 10 minutes (DB)
	SignalPerPartPerMin int           // relayed messages per participant per minute
	RoomMsgsPerMin      int           // relayed messages per room per minute (DB)
	PollPerIPPerMin     int           // polls per IP per minute
	CreatePerUserPerHr  int           // new rooms per user per hour
	BatchMax            int           // messages per signal request
	MaxCiphertext       int           // bytes of one decoded ciphertext
	MessageTTL          time.Duration // relayed messages are deleted after this
	StaleAfter          time.Duration // no poll for this long → participant left
	PollWait            time.Duration // longest long-poll
	KeepEnded           time.Duration // ended rooms stay in "My calls" this long
}

// DefaultLimits are used by New.
func DefaultLimits() Limits {
	return Limits{
		MaxParticipants:     6,
		JoinPerIPPerMin:     20,
		JoinsPerRoomPer10m:  40,
		SignalPerPartPerMin: 900,
		RoomMsgsPerMin:      4000,
		PollPerIPPerMin:     600, // several people behind one office NAT, each polling fast while connecting
		CreatePerUserPerHr:  30,
		BatchMax:            25,
		MaxCiphertext:       64 * 1024,
		MessageTTL:          2 * time.Minute,
		StaleAfter:          45 * time.Second,
		PollWait:            25 * time.Second,
		KeepEnded:           7 * 24 * time.Hour,
	}
}

// limiter is a fixed-window counter per key.
type limiter struct {
	mu    sync.Mutex
	m     map[string]*window
	swept time.Time
}

type window struct {
	start time.Time
	per   time.Duration
	n     int
}

func newLimiter() *limiter { return &limiter{m: map[string]*window{}} }

// allow counts one event for key and reports whether it is within limit
// events per period.
func (l *limiter) allow(key string, limit int, per time.Duration, now time.Time) bool {
	if limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > time.Minute || len(l.m) > 50000 {
		for k, w := range l.m {
			if now.Sub(w.start) >= w.per {
				delete(l.m, k)
			}
		}
		l.swept = now
	}
	w := l.m[key]
	if w == nil || now.Sub(w.start) >= per {
		w = &window{start: now, per: per}
		l.m[key] = w
	}
	if w.n >= limit {
		return false
	}
	w.n++
	return true
}

// ClientIP is the address used for rate limits. Behind Cloud Run's front end
// the last X-Forwarded-For entry is the one Google added (the client's own
// entries come first and can be made up), so it is preferred over the first.
func ClientIP(c *gin.Context) string {
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil {
				return ip.String()
			}
		}
	}
	return c.RemoteIP()
}

// ---------------------------------------------------------------- wake-ups

// hub wakes long polls of a room in this instance. Other instances are
// reached through Postgres NOTIFY (see listen.go), which calls wake too.
type hub struct {
	mu    sync.Mutex
	rooms map[string]map[chan struct{}]struct{}
}

func newHub() *hub { return &hub{rooms: map[string]map[chan struct{}]struct{}{}} }

func (h *hub) subscribe(room string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	set := h.rooms[room]
	if set == nil {
		set = map[chan struct{}]struct{}{}
		h.rooms[room] = set
	}
	set[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if set := h.rooms[room]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.rooms, room)
			}
		}
		h.mu.Unlock()
	}
}

func (h *hub) wake(room string) {
	h.mu.Lock()
	for ch := range h.rooms[room] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *hub) waiting() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.rooms {
		n += len(s)
	}
	return n
}
