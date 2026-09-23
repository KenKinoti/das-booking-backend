package database

import (
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

// MigrateTestModels migrates only the models needed for integration testing
func MigrateTestModels(db *gorm.DB) error {
	return db.AutoMigrate(
		&models.Organization{},
		&models.User{},
		&models.Customer{},
		&models.Service{},
		&models.Booking{},
		&models.IntegrationSettings{},
		&models.IntegrationLog{},
		&models.PaymentTransaction{},
		&models.CommunicationLog{},
	)
}

// SeedTestIntegrationDefaults seeds default integration settings for test organizations
func SeedTestIntegrationDefaults(db *gorm.DB) error {
	// Get all organizations that don't have integration settings
	var organizations []models.Organization
	if err := db.Raw(`
		SELECT o.* FROM organizations o
		LEFT JOIN integration_settings is ON o.id = is.organization_id
		WHERE is.id IS NULL
	`).Scan(&organizations).Error; err != nil {
		return err
	}

	// Create default integration settings for each organization
	for _, org := range organizations {
		defaultSettings := &models.IntegrationSettings{
			OrganizationID: org.ID,
			GeneralSettings: models.GeneralSettings{
				PlatformName:    "DAS Booking Platform",
				DefaultTimezone: "UTC",
				DefaultCurrency: "USD",
				DefaultLanguage: "en",
			},
			EmailSettings: models.EmailSettings{
				Enabled:  false,
				Provider: "smtp",
				SMTPPort: 587,
				SMTPSecurity: "tls",
				FromName: "DAS Booking Platform",
			},
			SecuritySettings: models.SecuritySettings{
				SessionTimeout:      30,
				PasswordMinLength:   8,
				RequireSpecialChars: true,
				RequireNumbers:      true,
				EnableTwoFactor:     false,
				CORSOrigins:         "http://localhost:5173,https://localhost:5173",
				RateLimitRequests:   100,
				RateLimitWindow:     15,
			},
			StripeSettings: models.StripeSettings{
				Enabled:       false,
				TestMode:      true,
				Currency:      "USD",
				CaptureMethod: "automatic",
				PaymentMethods: `["card"]`,
			},
			PayPalSettings: models.PayPalSettings{
				Enabled:     false,
				SandboxMode: true,
				Currency:    "USD",
			},
			TwilioSettings: models.TwilioSettings{
				Enabled:     false,
				EnableSMS:   true,
				EnableVoice: false,
				EnableWhatsApp: false,
			},
			WhatsAppSettings: models.WhatsAppSettings{
				Enabled:    false,
				APIVersion: "v18.0",
			},
			ZohoSettings: models.ZohoSettings{
				Enabled:   false,
				APIRegion: "com",
			},
		}

		if err := db.Create(defaultSettings).Error; err != nil {
			return err
		}
	}

	return nil
}