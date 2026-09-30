package mail

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/encryption"
)

// ---------------------------------------------------------------- secrets

const secretPrefix = "enc:v1:"

// EncryptSecret encrypts a password for storage (AES-GCM keyed by
// ENCRYPTION_KEY, falling back to JWT_SECRET). Already-encrypted and empty
// values are returned unchanged.
func EncryptSecret(plain string) (string, error) {
	if plain == "" || strings.HasPrefix(plain, secretPrefix) {
		return plain, nil
	}
	enc, err := encryption.NewEncryptionService().Encrypt(plain)
	if err != nil {
		return "", err
	}
	return secretPrefix + enc, nil
}

// DecryptSecret reverses EncryptSecret. Values saved before encryption was
// introduced (no prefix) are returned as they are.
func DecryptSecret(stored string) (string, error) {
	if !strings.HasPrefix(stored, secretPrefix) {
		return stored, nil
	}
	return encryption.NewEncryptionService().Decrypt(strings.TrimPrefix(stored, secretPrefix))
}

// IsEncrypted reports whether a stored secret is encrypted.
func IsEncrypted(stored string) bool { return strings.HasPrefix(stored, secretPrefix) }

// ---------------------------------------------------------------- last result

// SendStatus remembers the last send result per organisation.
type SendStatus struct {
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	LastSuccessAt  *time.Time `json:"last_success_at"`
	LastSuccessTo  string     `json:"last_success_to" gorm:"type:text"`
	LastErrorAt    *time.Time `json:"last_error_at"`
	LastError      string     `json:"last_error" gorm:"type:text"`
	LastSource     string     `json:"last_source" gorm:"type:varchar(20)"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// TableName keeps the table name explicit.
func (SendStatus) TableName() string { return "mail_send_status" }

// Migrate creates the package's tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&SendStatus{}) }

// ---------------------------------------------------------------- service

// Service resolves and caches outgoing-mail configuration per organisation.
type Service struct {
	DB  *gorm.DB
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]cached
	// Send is the transport (replaceable in tests).
	Transport func(ctx context.Context, cfg Config, msg Message) error
}

type cached struct {
	cfg Config
	at  time.Time
}

var (
	sharedMu sync.Mutex
	shared   = map[*gorm.DB]*Service{}
)

// Shared returns the process-wide service for db, so every module shares one
// cache (and one invalidation).
func Shared(db *gorm.DB) *Service {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if s, ok := shared[db]; ok {
		return s
	}
	s := NewService(db)
	shared[db] = s
	return s
}

// NewService builds a service with a 30-second cache.
func NewService(db *gorm.DB) *Service {
	return &Service{DB: db, TTL: 30 * time.Second, cache: map[string]cached{}, Transport: Send}
}

// InvalidateAll drops every cached configuration. Settings saves call it:
// a platform-default change affects every organisation.
func InvalidateAll() {
	sharedMu.Lock()
	list := make([]*Service, 0, len(shared))
	for _, s := range shared {
		list = append(list, s)
	}
	sharedMu.Unlock()
	for _, s := range list {
		s.Invalidate()
	}
}

// Invalidate clears the cache.
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.cache = map[string]cached{}
	s.mu.Unlock()
}

// Resolve returns the configuration used for orgID and its source
// (organisation | platform | environment | "" when nothing is set up).
func Resolve(db *gorm.DB, orgID string) (Config, string) {
	return Shared(db).Resolve(orgID)
}

// Resolve — see the package documentation for the order.
func (s *Service) Resolve(orgID string) (Config, string) {
	s.mu.Lock()
	if c, ok := s.cache[orgID]; ok && time.Since(c.at) < s.TTL {
		s.mu.Unlock()
		return c.cfg, c.cfg.Source
	}
	s.mu.Unlock()
	cfg := s.resolve(orgID)
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]cached{}
	}
	s.cache[orgID] = cached{cfg: cfg, at: time.Now()}
	s.mu.Unlock()
	return cfg, cfg.Source
}

// FromSettings converts saved settings to a Config (password decrypted).
// usable is false when the settings are switched off or have no host.
func FromSettings(e models.EmailSettings) (cfg Config, usable bool) {
	port := e.SMTPPort
	if port <= 0 {
		port = 587
	}
	host := strings.TrimSpace(e.SMTPHost)
	from := strings.ToLower(strings.TrimSpace(e.FromEmail))
	user := strings.TrimSpace(e.SMTPUsername)
	if from == "" && strings.Contains(user, "@") {
		from = strings.ToLower(user)
	}
	cc, _ := ParseList(e.AlwaysCC)
	bcc, _ := ParseList(e.AlwaysBCC)
	notify, _ := ParseList(e.NotificationRecipients)
	cfg = Config{
		Host: host, Port: port, Username: user,
		Security:  NormalizeSecurity(e.SMTPSecurity, port),
		FromEmail: from, FromName: strings.TrimSpace(e.FromName),
		ReplyTo: strings.TrimSpace(e.ReplyToEmail),
		CC:      cc, BCC: bcc, Notify: notify,
	}
	pw, err := DecryptSecret(e.SMTPPassword)
	if err != nil {
		cfg.Problem = "The saved SMTP password can't be decrypted (the server's encryption key changed). Re-enter the password and save."
	}
	cfg.Password = CleanPassword(host, pw)
	if user != "" && cfg.Password == "" && cfg.Problem == "" {
		cfg.Problem = "An SMTP username is saved without a password. Enter the password (for Gmail, an App Password) and save."
	}
	if host != "" && from == "" && cfg.Problem == "" {
		cfg.Problem = "Enter the From email address."
	}
	return cfg, e.Enabled && host != ""
}

func (s *Service) loadSettings(orgID string) (*models.IntegrationSettings, bool) {
	if s.DB == nil || orgID == "" {
		return nil, false
	}
	var row models.IntegrationSettings
	err := s.DB.Omit("Organization").Where("organization_id = ?", orgID).Order("created_at").First(&row).Error
	if err != nil {
		return nil, false
	}
	return &row, true
}

// PlatformOrgs lists the organisations whose saved settings act as the
// platform default: those of super administrators, oldest account first.
func (s *Service) PlatformOrgs() []string {
	if s.DB == nil {
		return nil
	}
	var ids []string
	s.DB.Raw(`SELECT organization_id FROM users WHERE role = 'super_admin' AND deleted_at IS NULL AND organization_id IS NOT NULL AND organization_id <> ''
		GROUP BY organization_id ORDER BY MIN(created_at)`).Scan(&ids)
	return ids
}

// IsPlatformOrg reports whether orgID is a platform (super admin) organisation.
func (s *Service) IsPlatformOrg(orgID string) bool {
	for _, id := range s.PlatformOrgs() {
		if id == orgID {
			return true
		}
	}
	return false
}

func (s *Service) resolve(orgID string) Config {
	note := ""
	platform := s.PlatformOrgs()
	isPlatform := false
	for _, id := range platform {
		if id == orgID {
			isPlatform = true
		}
	}
	if row, ok := s.loadSettings(orgID); ok {
		cfg, usable := FromSettings(row.EmailSettings)
		if usable {
			cfg.Source, cfg.OrgID = SourceOrganisation, orgID
			if isPlatform {
				cfg.Source = SourcePlatform
			}
			return cfg
		}
		if strings.TrimSpace(row.EmailSettings.SMTPHost) != "" && !row.EmailSettings.Enabled {
			note = "This organisation's own email settings are saved but switched off."
		}
	}
	for _, id := range platform {
		if id == orgID {
			continue
		}
		if row, ok := s.loadSettings(id); ok {
			if cfg, usable := FromSettings(row.EmailSettings); usable {
				cfg.Source, cfg.OrgID, cfg.Note = SourcePlatform, id, note
				return cfg
			}
		}
	}
	if cfg := FromEnv(); cfg.Host != "" {
		cfg.Note = note
		if cfg.FromEmail == "" {
			cfg.Problem = "SMTP_HOST is set but SMTP_FROM (or an email-style SMTP_USERNAME) is missing."
		}
		return cfg
	}
	return Config{Source: SourceNone, Note: note, CC: []string{}, BCC: []string{}, Notify: []string{}}
}

// ---------------------------------------------------------------- sending

// SendAs resolves orgID's configuration and sends msg, recording the result.
// It returns the configuration that was used.
func (s *Service) SendAs(ctx context.Context, orgID string, msg Message) (Config, error) {
	cfg, _ := s.Resolve(orgID)
	if !cfg.Ready() {
		if cfg.Problem != "" {
			return cfg, &Error{Code: "NOT_CONFIGURED", Msg: cfg.Problem}
		}
		return cfg, ErrNotConfigured
	}
	err := s.SendWith(ctx, orgID, cfg, msg)
	return cfg, err
}

// SendWith sends with an explicit configuration and records the result for
// orgID.
func (s *Service) SendWith(ctx context.Context, orgID string, cfg Config, msg Message) error {
	tr := s.Transport
	if tr == nil {
		tr = Send
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err := tr(ctx, cfg, msg)
	s.Record(orgID, cfg.Source, strings.Join(append(append(append([]string{}, msg.To...), msg.CC...), msg.BCC...), ", "), err)
	return err
}

// Record stores the outcome of a send for the status card.
func (s *Service) Record(orgID, source, to string, sendErr error) {
	if s.DB == nil || orgID == "" {
		return
	}
	now := time.Now()
	var row SendStatus
	if err := s.DB.Where("organization_id = ?", orgID).First(&row).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return
		}
		row = SendStatus{OrganizationID: orgID}
	}
	row.LastSource, row.UpdatedAt = source, now
	if sendErr == nil {
		row.LastSuccessAt, row.LastSuccessTo = &now, truncate(to, 500)
	} else {
		row.LastErrorAt, row.LastError = &now, truncate(sendErr.Error(), 1000)
	}
	s.DB.Save(&row)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Status is the effective configuration for the UI (no secrets).
type Status struct {
	Ready         bool       `json:"ready"`
	Source        string     `json:"source"`
	SourceLabel   string     `json:"source_label"`
	FromEmail     string     `json:"from_email"`
	FromName      string     `json:"from_name"`
	From          string     `json:"from"`
	ReplyTo       string     `json:"reply_to"`
	Host          string     `json:"host"`
	Port          int        `json:"port"`
	Security      string     `json:"security"`
	Auth          bool       `json:"auth"`
	AlwaysCC      []string   `json:"always_cc"`
	AlwaysBCC     []string   `json:"always_bcc"`
	Notify        []string   `json:"notification_recipients"`
	Note          string     `json:"note,omitempty"`
	Problem       string     `json:"problem,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastSuccessTo string     `json:"last_success_to,omitempty"`
	LastErrorAt   *time.Time `json:"last_error_at"`
	LastError     string     `json:"last_error,omitempty"`
}

// SourceLabel is the human name of a source.
func SourceLabel(src string) string {
	switch src {
	case SourceOrganisation:
		return "Organisation settings"
	case SourcePlatform:
		return "Platform default"
	case SourceEnvironment:
		return "Server environment (SMTP_*)"
	}
	return "Not set up"
}

// Status reports the effective configuration for orgID.
func (s *Service) Status(orgID string) Status {
	cfg, src := s.Resolve(orgID)
	st := StatusOf(cfg)
	st.Source, st.SourceLabel = src, SourceLabel(src)
	if s.DB != nil && orgID != "" {
		var row SendStatus
		if err := s.DB.Where("organization_id = ?", orgID).First(&row).Error; err == nil {
			st.LastSuccessAt, st.LastSuccessTo = row.LastSuccessAt, row.LastSuccessTo
			st.LastErrorAt, st.LastError = row.LastErrorAt, row.LastError
		}
	}
	return st
}

// StatusOf describes cfg without the send history.
func StatusOf(cfg Config) Status {
	nz := func(l []string) []string {
		if l == nil {
			return []string{}
		}
		return l
	}
	return Status{
		Ready: cfg.Ready(), Source: cfg.Source, SourceLabel: SourceLabel(cfg.Source),
		FromEmail: cfg.FromEmail, FromName: cfg.FromName, From: cfg.Display(), ReplyTo: cfg.ReplyTo,
		Host: cfg.Host, Port: cfg.Port, Security: cfg.Security, Auth: cfg.Username != "",
		AlwaysCC: nz(cfg.CC), AlwaysBCC: nz(cfg.BCC), Notify: nz(cfg.Notify),
		Note: cfg.Note, Problem: cfg.Problem,
	}
}
