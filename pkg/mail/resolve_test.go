package mail

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail/mailtest"
)

func TestSecretsRoundTrip(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "k1")
	enc, err := EncryptSecret("app password")
	if err != nil || !IsEncrypted(enc) || strings.Contains(enc, "app password") {
		t.Fatalf("encrypt: %q %v", enc, err)
	}
	if again, _ := EncryptSecret(enc); again != enc {
		t.Error("double encryption")
	}
	if pw, err := DecryptSecret(enc); err != nil || pw != "app password" {
		t.Errorf("decrypt = %q %v", pw, err)
	}
	if pw, _ := DecryptSecret("legacy-plain"); pw != "legacy-plain" {
		t.Errorf("legacy = %q", pw)
	}
	t.Setenv("ENCRYPTION_KEY", "k2")
	cfg, usable := FromSettings(models.EmailSettings{Enabled: true, SMTPHost: "smtp.x.test", SMTPUsername: "u@x.test", SMTPPassword: enc})
	if !usable || cfg.Ready() || !strings.Contains(cfg.Problem, "decrypted") {
		t.Errorf("wrong key: usable=%v ready=%v problem=%q", usable, cfg.Ready(), cfg.Problem)
	}
}

func TestFromSettings(t *testing.T) {
	enc, _ := EncryptSecret("abcd efgh ijkl mnop")
	cfg, usable := FromSettings(models.EmailSettings{Enabled: true, SMTPHost: "smtp.gmail.com", SMTPPort: 587, SMTPSecurity: "tls",
		SMTPUsername: "me@gmail.com", SMTPPassword: enc, FromName: "Shop", AlwaysCC: "a@x.test, A@x.test", AlwaysBCC: "records@x.test", NotificationRecipients: "ops@x.test;bad"})
	if !usable || !cfg.Ready() {
		t.Fatalf("not ready: %+v", cfg)
	}
	if cfg.Password != "abcdefghijklmnop" || cfg.FromEmail != "me@gmail.com" || cfg.Security != SecurityStartTLS {
		t.Errorf("cfg = %+v", cfg)
	}
	if len(cfg.CC) != 1 || cfg.BCC[0] != "records@x.test" || len(cfg.Notify) != 1 {
		t.Errorf("lists: %v %v %v", cfg.CC, cfg.BCC, cfg.Notify)
	}
	if _, usable := FromSettings(models.EmailSettings{Enabled: false, SMTPHost: "h"}); usable {
		t.Error("disabled settings must not be usable")
	}
	if c, _ := FromSettings(models.EmailSettings{Enabled: true, SMTPHost: "h", SMTPUsername: "u"}); c.Ready() {
		t.Error("username without password must not be ready")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("SMTP_HOST", "smtp.env.test")
	t.Setenv("SMTP_PORT", "2525")
	t.Setenv("SMTP_FROM", "Env Sender <env@x.test>")
	t.Setenv("SMTP_USERNAME", "")
	cfg := FromEnv()
	if !cfg.Ready() || cfg.FromEmail != "env@x.test" || cfg.FromName != "Env Sender" || cfg.Port != 2525 || cfg.Source != SourceEnvironment || cfg.Security != SecurityAuto {
		t.Errorf("env cfg = %+v", cfg)
	}
	t.Setenv("SMTP_HOST", "")
	if FromEnv().Host != "" {
		t.Error("expected empty")
	}
}

// Precedence tests run against Postgres when TEST_DATABASE_URL is set.
func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	// Own schema, so real users/settings in the database don't interfere.
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`CREATE SCHEMA IF NOT EXISTS mailpkg_test`)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=mailpkg_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.IntegrationSettings{}, &SendStatus{}); err != nil {
		t.Fatal(err)
	}
	// Test organisations are not real rows.
	db.Exec(`ALTER TABLE integration_settings DROP CONSTRAINT IF EXISTS fk_integration_settings_organization`)
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS users (id varchar(255) primary key, email text, password_hash text, first_name text, last_name text, role text, organization_id varchar(255), created_at timestamptz, deleted_at timestamptz)`).Error; err != nil {
		t.Fatal(err)
	}
	// A table left by an older run of this test lacks the columns the inserts below use.
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash text, ADD COLUMN IF NOT EXISTS first_name text, ADD COLUMN IF NOT EXISTS last_name text`)
	return db
}

func saveEmail(t *testing.T, db *gorm.DB, org string, e models.EmailSettings) {
	t.Helper()
	db.Unscoped().Where("organization_id = ?", org).Delete(&models.IntegrationSettings{})
	row := models.IntegrationSettings{ID: uuid.NewString(), OrganizationID: org, EmailSettings: e}
	if err := db.Omit("Organization").Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

func TestResolvePrecedence(t *testing.T) {
	db := testDB(t)
	t.Setenv("SMTP_HOST", "")
	platform, org, other := "plat-"+uuid.NewString(), "org-"+uuid.NewString(), "other-"+uuid.NewString()
	// Make `platform` the oldest super admin's organisation.
	// (password_hash/first/last name are NOT NULL on a fully migrated database)
	if err := db.Exec(`INSERT INTO users (id, email, password_hash, first_name, last_name, role, organization_id, created_at) VALUES (?, ?, 'x', 'Super', 'Admin', 'super_admin', ?, ?)`, uuid.NewString(), "sa-"+uuid.NewString()[:8]+"@x.test", platform, time.Unix(1, 0)).Error; err != nil {
		t.Fatalf("insert super admin: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM users WHERE organization_id = ?`, platform)
		db.Unscoped().Where("organization_id IN ?", []string{platform, org, other}).Delete(&models.IntegrationSettings{})
	})
	svc := NewService(db)

	// Nothing saved, no env → none.
	if cfg, src := svc.Resolve(other); src != SourceNone || cfg.Ready() {
		// Another test DB may have platform settings from other runs; only
		// assert when no platform default exists.
		if src != SourcePlatform {
			t.Fatalf("expected none, got %q", src)
		}
	}

	// Env fallback.
	svc.Invalidate()
	t.Setenv("SMTP_HOST", "smtp.env.test")
	t.Setenv("SMTP_FROM", "env@x.test")
	if _, src := svc.Resolve(org); src != SourceEnvironment && src != SourcePlatform {
		t.Fatalf("expected environment, got %q", src)
	}

	// Platform default beats env.
	saveEmail(t, db, platform, models.EmailSettings{Enabled: true, SMTPHost: "smtp.platform.test", FromEmail: "platform@x.test", FromName: "Platform"})
	svc.Invalidate()
	cfg, src := svc.Resolve(org)
	if src != SourcePlatform || cfg.FromEmail != "platform@x.test" || cfg.OrgID != platform {
		t.Fatalf("expected platform, got %q %+v", src, cfg)
	}
	// The platform organisation itself reports "platform".
	if _, src := svc.Resolve(platform); src != SourcePlatform {
		t.Errorf("platform org source = %q", src)
	}

	// Organisation's own settings, switched off → still platform, with a note.
	saveEmail(t, db, org, models.EmailSettings{Enabled: false, SMTPHost: "smtp.org.test", FromEmail: "org@x.test"})
	svc.Invalidate()
	cfg, src = svc.Resolve(org)
	if src != SourcePlatform || cfg.Note == "" {
		t.Fatalf("disabled org settings: %q %+v", src, cfg)
	}

	// Enabled → organisation wins.
	saveEmail(t, db, org, models.EmailSettings{Enabled: true, SMTPHost: "smtp.org.test", FromEmail: "org@x.test", FromName: "Org"})
	// Cached until invalidated.
	if cfg, _ := svc.Resolve(org); cfg.FromEmail != "platform@x.test" {
		t.Errorf("expected cached platform config, got %s", cfg.FromEmail)
	}
	svc.Invalidate()
	cfg, src = svc.Resolve(org)
	if src != SourceOrganisation || cfg.Display() != "Org <org@x.test>" {
		t.Fatalf("expected organisation, got %q %+v", src, cfg)
	}
	// Another organisation still gets the platform default.
	if _, src := svc.Resolve(other); src != SourcePlatform {
		t.Errorf("other org source = %q", src)
	}
}

func TestSendAsRecordsStatus(t *testing.T) {
	db := testDB(t)
	t.Setenv("SMTP_HOST", "")
	org := "org-" + uuid.NewString()
	t.Cleanup(func() {
		db.Unscoped().Where("organization_id = ?", org).Delete(&models.IntegrationSettings{})
		db.Where("organization_id = ?", org).Delete(&SendStatus{})
	})
	s := mailtest.Start(t, mailtest.StartTLS, "u@x.test", "secret")
	enc, _ := EncryptSecret("secret")
	saveEmail(t, db, org, models.EmailSettings{Enabled: true, SMTPHost: s.Host, SMTPPort: s.Port, SMTPSecurity: "tls", SMTPUsername: "u@x.test", SMTPPassword: enc,
		FromEmail: "shop@x.test", FromName: "Shop", AlwaysBCC: "records@x.test"})
	svc := NewService(db)
	tlsCfg := s.ClientTLS()
	svc.Transport = func(ctx context.Context, cfg Config, msg Message) error {
		cfg.TLSConfig = tlsCfg
		return Send(ctx, cfg, msg)
	}
	cfg, err := svc.SendAs(context.Background(), org, Message{To: []string{"c@x.test"}, BCC: []string{"records@x.test"}, Subject: "Hi", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source != SourceOrganisation {
		t.Errorf("source = %q", cfg.Source)
	}
	st := svc.Status(org)
	if !st.Ready || st.LastSuccessAt == nil || st.From != "Shop <shop@x.test>" || len(st.AlwaysBCC) != 1 {
		t.Errorf("status = %+v", st)
	}
	if got := s.Messages(); len(got) != 1 || len(got[0].Rcpt) != 2 {
		t.Errorf("messages = %+v", got)
	}

	// Wrong password → recorded error.
	bad, _ := EncryptSecret("nope")
	db.Model(&models.IntegrationSettings{}).Where("organization_id = ?", org).Update("email_smtp_password", bad)
	svc.Invalidate()
	if _, err := svc.SendAs(context.Background(), org, Message{To: []string{"c@x.test"}, Text: "x"}); Code(err) != "AUTH_FAILED" {
		t.Fatalf("expected auth failure, got %v", err)
	}
	if st := svc.Status(org); st.LastErrorAt == nil || !strings.Contains(st.LastError, "rejected") {
		t.Errorf("status after failure = %+v", st)
	}
}
