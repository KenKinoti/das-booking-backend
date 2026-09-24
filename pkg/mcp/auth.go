package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

// Token formats.
const (
	PATPrefix     = "dsy_"  // personal access token
	AccessPrefix  = "dsyo_" // OAuth access token
	RefreshPrefix = "dsyr_" // OAuth refresh token

	ScopeRead      = "read"
	ScopeReadWrite = "read_write"

	OAuthScopeRead  = "erp.read"
	OAuthScopeWrite = "erp.write"

	AccessTTL  = time.Hour
	RefreshTTL = 30 * 24 * time.Hour
	CodeTTL    = 5 * time.Minute
	RequestTTL = 15 * time.Minute
)

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomString returns n cryptographically random base62 characters.
func randomString(n int) string {
	var sb strings.Builder
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < n; i++ {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		sb.WriteByte(alphabet[k.Int64()])
	}
	return sb.String()
}

// NewPAT returns a new personal access token value.
func NewPAT() string { return PATPrefix + randomString(40) }

// HashToken is the stored form of any token or code.
func HashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

// Principal is the authenticated caller of /mcp.
type Principal struct {
	aitools.Identity
	Scope      string // read | read_write
	Kind       string // token | oauth | test
	TokenID    string // PAT id or OAuth grant id
	ClientName string
}

// CanWrite reports whether write tools are allowed.
func (p Principal) CanWrite() bool { return p.Scope == ScopeReadWrite }

func (p Principal) key() string { return p.Kind + ":" + p.TokenID }

var errUnauthorized = errors.New("unauthorized")

// userIdentity loads an active user as a tool identity.
func (s *Server) userIdentity(userID string) (aitools.Identity, error) {
	var u struct {
		ID             string
		Email          string
		Role           string
		OrganizationID string
		IsActive       bool
	}
	err := s.DB.Raw("SELECT id, email, role, organization_id, is_active FROM users WHERE id = ? AND deleted_at IS NULL", userID).Scan(&u).Error
	if err != nil || u.ID == "" || !u.IsActive {
		return aitools.Identity{}, errUnauthorized
	}
	id := aitools.Identity{UserID: u.ID, OrgID: u.OrganizationID, Role: u.Role, Email: u.Email}
	var tz string
	s.DB.Raw("SELECT hours_timezone FROM organizations WHERE id = ?", u.OrganizationID).Row().Scan(&tz)
	if tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			id.TZ = tz
		}
	}
	return id, nil
}

// authenticate resolves a bearer token to a principal.
func (s *Server) authenticate(raw string) (Principal, error) {
	now := s.now()
	h := HashToken(raw)
	switch {
	case strings.HasPrefix(raw, PATPrefix):
		var t Token
		if err := s.DB.Where("hash = ?", h).First(&t).Error; err != nil {
			return Principal{}, errUnauthorized
		}
		if t.RevokedAt != nil || (t.ExpiresAt != nil && now.After(*t.ExpiresAt)) {
			return Principal{}, errUnauthorized
		}
		id, err := s.userIdentity(t.UserID)
		if err != nil || id.OrgID != t.OrgID {
			return Principal{}, errUnauthorized
		}
		if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) > time.Minute {
			s.DB.Model(&Token{}).Where("id = ?", t.ID).Update("last_used_at", now)
		}
		return Principal{Identity: id, Scope: t.Scope, Kind: "token", TokenID: t.ID, ClientName: t.Name}, nil
	case strings.HasPrefix(raw, AccessPrefix):
		var t OAuthToken
		if err := s.DB.Where("hash = ? AND kind = ?", h, "access").First(&t).Error; err != nil {
			return Principal{}, errUnauthorized
		}
		if t.RevokedAt != nil || now.After(t.ExpiresAt) {
			return Principal{}, errUnauthorized
		}
		var g OAuthGrant
		if err := s.DB.Where("id = ?", t.GrantID).First(&g).Error; err != nil || g.RevokedAt != nil {
			return Principal{}, errUnauthorized
		}
		id, err := s.userIdentity(g.UserID)
		if err != nil || id.OrgID != g.OrgID {
			return Principal{}, errUnauthorized
		}
		var cl OAuthClient
		s.DB.Where("id = ?", g.ClientID).First(&cl)
		if g.LastUsedAt == nil || now.Sub(*g.LastUsedAt) > time.Minute {
			s.DB.Model(&OAuthGrant{}).Where("id = ?", g.ID).Update("last_used_at", now)
		}
		return Principal{Identity: id, Scope: g.Scope, Kind: "oauth", TokenID: g.ID, ClientName: cl.Name}, nil
	}
	return Principal{}, errUnauthorized
}

// ---------------------------------------------------------------------------
// URLs

// BaseURL is the public URL of this API server (no trailing slash):
// PUBLIC_API_URL, else derived from the request (X-Forwarded-Proto/Host).
func BaseURL(r *http.Request) string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_API_URL")), "/"); v != "" {
		return v
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host
}

// ResourceURL is the MCP endpoint (the OAuth protected resource).
func ResourceURL(r *http.Request) string { return BaseURL(r) + "/mcp" }

// ResourceMetadataURL is the RFC 9728 metadata location for ResourceURL.
func ResourceMetadataURL(r *http.Request) string {
	return BaseURL(r) + "/.well-known/oauth-protected-resource/mcp"
}

func sameResource(a, b string) bool {
	ua, e1 := url.Parse(strings.TrimRight(a, "/"))
	ub, e2 := url.Parse(strings.TrimRight(b, "/"))
	if e1 != nil || e2 != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) && ua.Path == ub.Path
}

// ---------------------------------------------------------------------------
// Origin validation (DNS-rebinding protection)

var localOrigin = regexp.MustCompile(`^https?://(localhost|127\.0\.0\.1|\[::1\])(:\d+)?$`)

// originAllowed: requests without Origin (server-side clients) are allowed;
// browser origins must be the web app, Claude, localhost or MCP_ALLOWED_ORIGINS.
func originAllowed(origin string) bool {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return true
	}
	if localOrigin.MatchString(origin) {
		return true
	}
	allowed := []string{"https://claude.ai", "https://claude.com", "https://www.claude.ai"}
	if app := aitools.AppURL(); app != "" {
		if u, err := url.Parse(app); err == nil {
			allowed = append(allowed, u.Scheme+"://"+u.Host)
		}
	}
	for _, o := range strings.Split(os.Getenv("MCP_ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			if o == "*" {
				return true
			}
			allowed = append(allowed, o)
		}
	}
	for _, a := range allowed {
		if strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Rate limiting (token bucket per key)

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	perMin  float64
	buckets map[string]*bucket
}

func newLimiter(perMin int) *limiter {
	return &limiter{perMin: float64(perMin), buckets: map[string]*bucket{}}
}

// take consumes n units; it returns false and the wait when exhausted.
func (l *limiter) take(key string, n int, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.perMin, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Minutes() * l.perMin
	if b.tokens > l.perMin {
		b.tokens = l.perMin
	}
	b.last = now
	if b.tokens < float64(n) {
		need := float64(n) - b.tokens
		return false, time.Duration(need/l.perMin*float64(time.Minute)) + time.Second
	}
	b.tokens -= float64(n)
	if len(l.buckets) > 10000 { // drop idle buckets
		for k, v := range l.buckets {
			if now.Sub(v.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	return true, 0
}

// ---------------------------------------------------------------------------
// Audit

var secretKey = regexp.MustCompile(`(?i)(pass|secret|token|card|cvv|cvc|api_?key|pin|iban|account_number)`)

// SummarizeArgs returns a short JSON summary of tool arguments with secrets
// redacted and long strings truncated.
func SummarizeArgs(args map[string]any) string {
	var clean func(v any, depth int) any
	clean = func(v any, depth int) any {
		switch x := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, val := range x {
				if secretKey.MatchString(k) {
					out[k] = "[redacted]"
					continue
				}
				if depth > 3 {
					out[k] = "…"
					continue
				}
				out[k] = clean(val, depth+1)
			}
			return out
		case []any:
			out := []any{}
			for i, val := range x {
				if i >= 10 {
					out = append(out, "…")
					break
				}
				out = append(out, clean(val, depth+1))
			}
			return out
		case string:
			if len(x) > 200 {
				return x[:200] + "…"
			}
			return x
		}
		return v
	}
	b, _ := json.Marshal(clean(args, 0))
	s := string(b)
	if len(s) > 2000 {
		s = s[:2000] + "…"
	}
	return s
}

// LogActivity records one AI tool call in ai_activity. It is safe to call
// from other packages (e.g. the in-app assistant with Source "assistant").
func LogActivity(db *gorm.DB, a Activity) {
	if db == nil {
		return
	}
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if len(a.Error) > 1000 {
		a.Error = a.Error[:1000]
	}
	_ = db.Create(&a).Error
}

// bearer extracts a bearer token from the Authorization header.
func bearer(c *gin.Context) string {
	h := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}
