package assistant

// One resolver for the Anthropic configuration used by every AI feature in
// the app: Ask DASYIN (chat + status), meeting summaries (pkg/meetings),
// expense extraction (pkg/expenses) and market "Explain with AI"
// (pkg/insights).
//
// Order (see Resolve):
//  1. the platform setting a super admin saved in System settings → AI
//     assistant (one platform-wide row, API key encrypted at rest),
//  2. the environment: ANTHROPIC_API_KEY, ASSISTANT_MODEL / ANTHROPIC_MODEL,
//  3. nothing — the AI features are off.
//
// The API key and the model resolve independently, so a super admin can pick a
// model in the app while the key still comes from the environment.
// ANTHROPIC_BASE_URL is only ever read from the environment (tests, gateways).
//
// Saved settings are cached for a few seconds and the cache is dropped on
// every save, so a new key works immediately — no restart.

import (
	"os"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// DefaultModel matches the model used by the meetings summariser.
const DefaultModel = "claude-sonnet-4-5"

// DefaultBaseURL is the Anthropic API.
const DefaultBaseURL = "https://api.anthropic.com"

// Where a resolved value came from.
const (
	SourceSettings    = "settings"    // saved in the app by a super admin
	SourceEnvironment = "environment" // ANTHROPIC_API_KEY etc. on the backend service
	SourceNone        = "none"        // not set anywhere
	SourceDefault     = "default"     // (model only) the built-in default
)

// Config is a resolved Anthropic configuration.
type Config struct {
	APIKey  string
	Model   string
	BaseURL string

	// Source says where the API key came from: settings | environment | none.
	Source string
	// ModelSource says where the model came from: settings | environment | default.
	ModelSource string
	// Problem is set when a saved key exists but cannot be used (for example
	// the server's encryption key changed).
	Problem string
}

// Enabled reports whether an API key is available.
func (c Config) Enabled() bool { return c.APIKey != "" }

// ConfigFromEnv reads ANTHROPIC_API_KEY, ASSISTANT_MODEL / ANTHROPIC_MODEL and
// ANTHROPIC_BASE_URL — the environment layer of Resolve.
func ConfigFromEnv() Config {
	c := Config{
		APIKey:      strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		Model:       strings.TrimSpace(os.Getenv("ASSISTANT_MODEL")),
		BaseURL:     strings.TrimRight(strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")), "/"),
		Source:      SourceNone,
		ModelSource: SourceEnvironment,
	}
	if c.APIKey != "" {
		c.Source = SourceEnvironment
	}
	if c.Model == "" {
		c.Model = strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	}
	if c.Model == "" {
		c.Model, c.ModelSource = DefaultModel, SourceDefault
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	return c
}

// PlatformSettingsID is the id of the single platform-wide settings row.
const PlatformSettingsID = "platform"

// PlatformSettings is the platform-wide AI configuration saved by a super
// admin. There is one row (id "platform"). APIKey holds the encrypted key
// (mail.EncryptSecret, the helper used for SMTP passwords); the plain key is
// never stored, returned by the API or logged.
type PlatformSettings struct {
	ID           string     `json:"-" gorm:"type:varchar(32);primaryKey"`
	APIKey       string     `json:"-" gorm:"column:api_key;type:text"`
	KeyHint      string     `json:"-" gorm:"type:varchar(40)"`
	KeyUpdatedAt *time.Time `json:"-"`
	KeyUpdatedBy string     `json:"-" gorm:"type:varchar(255)"`
	Model        string     `json:"-" gorm:"type:varchar(120)"`

	LastTestAt      *time.Time `json:"-"`
	LastTestOK      bool       `json:"-"`
	LastTestCode    string     `json:"-" gorm:"type:varchar(40)"`
	LastTestMessage string     `json:"-" gorm:"type:text"`
	LastTestModel   string     `json:"-" gorm:"type:varchar(120)"`

	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

func (PlatformSettings) TableName() string { return "assistant_platform_settings" }

// MaskKey returns a display hint for a key ("sk-ant-…a1B2"): enough to
// recognise which key is saved, never enough to use it.
func MaskKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) < 16 {
		return "…" + k[len(k)-2:]
	}
	if strings.HasPrefix(k, "sk-ant-") {
		return "sk-ant-…" + k[len(k)-4:]
	}
	return k[:3] + "…" + k[len(k)-4:]
}

// ---------------------------------------------------------------- cache

// CacheTTL is how long saved settings are cached. Saves invalidate the cache
// in this process; other instances pick a change up within the TTL.
var CacheTTL = 15 * time.Second

type saved struct {
	key, model, problem string
	at                  time.Time
}

var (
	cfgMu     sync.Mutex
	cfgCache  = map[*gorm.DB]saved{}
	defaultDB *gorm.DB
)

// Use sets the database used by Current (called once at start-up). Packages
// whose AI entry points have no database handle (meeting summaries, expense
// extraction, market explanations) resolve through it.
func Use(db *gorm.DB) {
	cfgMu.Lock()
	defaultDB = db
	cfgMu.Unlock()
}

// Current resolves the configuration with the database given to Use. Without
// one (unit tests of other packages) only the environment is consulted.
func Current() Config {
	cfgMu.Lock()
	db := defaultDB
	cfgMu.Unlock()
	return Resolve(db)
}

// Invalidate drops the cached saved settings (every database).
func Invalidate() {
	cfgMu.Lock()
	cfgCache = map[*gorm.DB]saved{}
	cfgMu.Unlock()
}

// loadPlatformSettings reads the single settings row; ok is false when there
// is none (or the table does not exist yet).
func loadPlatformSettings(db *gorm.DB) (PlatformSettings, bool) {
	var row PlatformSettings
	if db == nil {
		return row, false
	}
	// Quiet: before the first migration the table is missing, which is fine.
	q := db.Session(&gorm.Session{NewDB: true, Logger: logger.Default.LogMode(logger.Silent)})
	res := q.Where("id = ?", PlatformSettingsID).Limit(1).Find(&row)
	if res.Error != nil || res.RowsAffected == 0 {
		return PlatformSettings{}, false
	}
	return row, true
}

func loadSaved(db *gorm.DB) saved {
	if db == nil {
		return saved{}
	}
	cfgMu.Lock()
	if s, ok := cfgCache[db]; ok && time.Since(s.at) < CacheTTL {
		cfgMu.Unlock()
		return s
	}
	cfgMu.Unlock()

	s := saved{at: time.Now()}
	if row, ok := loadPlatformSettings(db); ok {
		s.model = strings.TrimSpace(row.Model)
		if row.APIKey != "" {
			key, err := mail.DecryptSecret(row.APIKey)
			if err != nil || strings.TrimSpace(key) == "" {
				s.problem = "The saved Anthropic API key can't be read (the server's encryption key changed). Paste the key again in System settings → AI assistant."
			} else {
				s.key = strings.TrimSpace(key)
			}
		}
	}
	cfgMu.Lock()
	cfgCache[db] = s
	cfgMu.Unlock()
	return s
}

// Resolve returns the Anthropic configuration in effect right now:
// saved platform settings → environment → none. The environment is read on
// every call; saved settings are cached for CacheTTL.
func Resolve(db *gorm.DB) Config {
	cfg := ConfigFromEnv()
	s := loadSaved(db)
	if s.key != "" {
		cfg.APIKey, cfg.Source = s.key, SourceSettings
	}
	if s.model != "" {
		cfg.Model, cfg.ModelSource = s.model, SourceSettings
	}
	cfg.Problem = s.problem
	return cfg
}
