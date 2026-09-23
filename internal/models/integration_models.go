package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// IntegrationSettings stores all third-party integration configurations per organization
type IntegrationSettings struct {
	ID             string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`

	// Core Settings
	GeneralSettings    GeneralSettings    `json:"general_settings" gorm:"embedded;embeddedPrefix:general_"`
	EmailSettings      EmailSettings      `json:"email_settings" gorm:"embedded;embeddedPrefix:email_"`
	SecuritySettings   SecuritySettings   `json:"security_settings" gorm:"embedded;embeddedPrefix:security_"`

	// Third-Party Integrations
	StripeSettings     StripeSettings     `json:"stripe_settings" gorm:"embedded;embeddedPrefix:stripe_"`
	PayPalSettings     PayPalSettings     `json:"paypal_settings" gorm:"embedded;embeddedPrefix:paypal_"`
	TwilioSettings     TwilioSettings     `json:"twilio_settings" gorm:"embedded;embeddedPrefix:twilio_"`
	WhatsAppSettings   WhatsAppSettings   `json:"whatsapp_settings" gorm:"embedded;embeddedPrefix:whatsapp_"`
	FirebaseSettings   FirebaseSettings   `json:"firebase_settings" gorm:"embedded;embeddedPrefix:firebase_"`
	ZohoSettings       ZohoSettings       `json:"zoho_settings" gorm:"embedded;embeddedPrefix:zoho_"`

	// API Keys (encrypted)
	APIKeys            APIKeys            `json:"api_keys" gorm:"embedded;embeddedPrefix:api_"`

	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
	DeletedAt          gorm.DeletedAt     `json:"-" gorm:"index"`

	// Relationships
	Organization       Organization       `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
}

// GeneralSettings for platform configuration
type GeneralSettings struct {
	PlatformName     string `json:"platform_name" gorm:"type:varchar(255);default:'DAS Booking Platform'"`
	DefaultTimezone  string `json:"default_timezone" gorm:"type:varchar(100);default:'UTC'"`
	DefaultCurrency  string `json:"default_currency" gorm:"type:varchar(3);default:'USD'"`
	DefaultLanguage  string `json:"default_language" gorm:"type:varchar(10);default:'en'"`
}

// EmailSettings for SMTP and email service configuration
type EmailSettings struct {
	Enabled         bool   `json:"enabled" gorm:"default:false"`
	Provider        string `json:"provider" gorm:"type:varchar(50);default:'smtp'"` // smtp, gmail, outlook, sendgrid, mailgun
	SMTPHost        string `json:"smtp_host" gorm:"type:varchar(255)"`
	SMTPPort        int    `json:"smtp_port" gorm:"default:587"`
	SMTPUsername    string `json:"smtp_username" gorm:"type:varchar(255)"`
	SMTPPassword    string `json:"smtp_password" gorm:"type:varchar(255)"` // encrypted
	SMTPSecurity    string `json:"smtp_security" gorm:"type:varchar(10);default:'tls'"` // tls, ssl, none
	FromName        string `json:"from_name" gorm:"type:varchar(255);default:'DAS Booking Platform'"`
	FromEmail       string `json:"from_email" gorm:"type:varchar(255)"`
	ReplyToEmail    string `json:"reply_to_email" gorm:"type:varchar(255)"`
	TestEmailSent   bool   `json:"test_email_sent" gorm:"default:false"`
	LastTestedAt    *time.Time `json:"last_tested_at,omitempty"`
}

// SecuritySettings for authentication and access control
type SecuritySettings struct {
	SessionTimeout       int    `json:"session_timeout" gorm:"default:30"`        // minutes
	PasswordMinLength    int    `json:"password_min_length" gorm:"default:8"`
	RequireSpecialChars  bool   `json:"require_special_chars" gorm:"default:true"`
	RequireNumbers       bool   `json:"require_numbers" gorm:"default:true"`
	EnableTwoFactor      bool   `json:"enable_two_factor" gorm:"default:false"`
	AllowedIPAddresses   string `json:"allowed_ip_addresses" gorm:"type:text"` // comma-separated
	CORSOrigins          string `json:"cors_origins" gorm:"type:text"`
	RateLimitRequests    int    `json:"rate_limit_requests" gorm:"default:100"`
	RateLimitWindow      int    `json:"rate_limit_window" gorm:"default:15"` // minutes
}

// StripeSettings for Stripe payment processing
type StripeSettings struct {
	Enabled              bool       `json:"enabled" gorm:"default:false"`
	PublishableKey       string     `json:"publishable_key" gorm:"type:varchar(255)"`
	SecretKey            string     `json:"secret_key" gorm:"type:varchar(255)"` // encrypted
	WebhookSecret        string     `json:"webhook_secret" gorm:"type:varchar(255)"` // encrypted
	TestMode             bool       `json:"test_mode" gorm:"default:true"`
	Currency             string     `json:"currency" gorm:"type:varchar(3);default:'USD'"`
	CaptureMethod        string     `json:"capture_method" gorm:"type:varchar(20);default:'automatic'"` // automatic, manual
	PaymentMethods       string     `json:"payment_methods" gorm:"type:text;default:'card'"` // JSON array: card, bank_transfer, etc.
	ConnectionVerified   bool       `json:"connection_verified" gorm:"default:false"`
	LastTestedAt         *time.Time `json:"last_tested_at,omitempty"`
}

// PayPalSettings for PayPal payment processing
type PayPalSettings struct {
	Enabled              bool       `json:"enabled" gorm:"default:false"`
	ClientID             string     `json:"client_id" gorm:"type:varchar(255)"`
	ClientSecret         string     `json:"client_secret" gorm:"type:varchar(255)"` // encrypted
	WebhookID            string     `json:"webhook_id" gorm:"type:varchar(255)"`
	SandboxMode          bool       `json:"sandbox_mode" gorm:"default:true"`
	Currency             string     `json:"currency" gorm:"type:varchar(3);default:'USD'"`
	ConnectionVerified   bool       `json:"connection_verified" gorm:"default:false"`
	LastTestedAt         *time.Time `json:"last_tested_at,omitempty"`
}

// TwilioSettings for SMS and voice services
type TwilioSettings struct {
	Enabled              bool       `json:"enabled" gorm:"default:false"`
	AccountSID           string     `json:"account_sid" gorm:"type:varchar(255)"`
	AuthToken            string     `json:"auth_token" gorm:"type:varchar(255)"` // encrypted
	FromPhoneNumber      string     `json:"from_phone_number" gorm:"type:varchar(20)"`
	MessagingServiceSID  string     `json:"messaging_service_sid" gorm:"type:varchar(255)"`
	EnableSMS            bool       `json:"enable_sms" gorm:"default:true"`
	EnableVoice          bool       `json:"enable_voice" gorm:"default:false"`
	EnableWhatsApp       bool       `json:"enable_whatsapp" gorm:"default:false"`
	WhatsAppFromNumber   string     `json:"whatsapp_from_number" gorm:"type:varchar(20)"`
	ConnectionVerified   bool       `json:"connection_verified" gorm:"default:false"`
	LastTestedAt         *time.Time `json:"last_tested_at,omitempty"`
}

// WhatsAppSettings for WhatsApp Business API
type WhatsAppSettings struct {
	Enabled              bool       `json:"enabled" gorm:"default:false"`
	AccessToken          string     `json:"access_token" gorm:"type:varchar(500)"` // encrypted
	PhoneNumberID        string     `json:"phone_number_id" gorm:"type:varchar(255)"`
	BusinessAccountID    string     `json:"business_account_id" gorm:"type:varchar(255)"`
	WebhookURL           string     `json:"webhook_url" gorm:"type:varchar(500)"`
	VerifyToken          string     `json:"verify_token" gorm:"type:varchar(255)"` // encrypted
	APIVersion           string     `json:"api_version" gorm:"type:varchar(10);default:'v18.0'"`
	ConnectionVerified   bool       `json:"connection_verified" gorm:"default:false"`
	LastTestedAt         *time.Time `json:"last_tested_at,omitempty"`
}

// ZohoSettings for Zoho suite integration
type ZohoSettings struct {
	Enabled              bool       `json:"enabled" gorm:"default:false"`
	ClientID             string     `json:"client_id" gorm:"type:varchar(255)"`
	ClientSecret         string     `json:"client_secret" gorm:"type:varchar(255)"` // encrypted
	RefreshToken         string     `json:"refresh_token" gorm:"type:varchar(500)"` // encrypted
	AccessToken          string     `json:"access_token" gorm:"type:varchar(500)"` // encrypted, auto-refreshed
	APIRegion            string     `json:"api_region" gorm:"type:varchar(50);default:'com'"` // com, eu, in, com.au
	TokenExpiresAt       *time.Time `json:"token_expires_at,omitempty"`

	// Zoho Module Enablement
	EnableCRM            bool       `json:"enable_crm" gorm:"default:false"`
	EnableBooks          bool       `json:"enable_books" gorm:"default:false"`
	EnableDesk           bool       `json:"enable_desk" gorm:"default:false"`
	EnableMail           bool       `json:"enable_mail" gorm:"default:false"`
	EnableCampaigns      bool       `json:"enable_campaigns" gorm:"default:false"`

	ConnectionVerified   bool       `json:"connection_verified" gorm:"default:false"`
	LastTestedAt         *time.Time `json:"last_tested_at,omitempty"`
}

// FirebaseSettings for Firebase integration (Auth, Firestore, Storage, etc.)
type FirebaseSettings struct {
	Enabled             bool       `json:"enabled" gorm:"default:false"`
	ProjectID           string     `json:"project_id" gorm:"type:varchar(255)"`
	PrivateKey          string     `json:"private_key" gorm:"type:text"` // encrypted
	ClientEmail         string     `json:"client_email" gorm:"type:varchar(255)"`
	ClientID            string     `json:"client_id" gorm:"type:varchar(255)"`
	ClientCertURL       string     `json:"client_cert_url" gorm:"type:varchar(500)"`
	DatabaseURL         string     `json:"database_url" gorm:"type:varchar(500)"`
	StorageBucket       string     `json:"storage_bucket" gorm:"type:varchar(255)"`

	// Service Enablement
	EnableAuth          bool       `json:"enable_auth" gorm:"default:true"`
	EnableFirestore     bool       `json:"enable_firestore" gorm:"default:true"`
	EnableStorage       bool       `json:"enable_storage" gorm:"default:true"`
	EnableFunctions     bool       `json:"enable_functions" gorm:"default:false"`
	EnableMessaging     bool       `json:"enable_messaging" gorm:"default:false"`
	EnableAnalytics     bool       `json:"enable_analytics" gorm:"default:true"`

	ConnectionVerified  bool       `json:"connection_verified" gorm:"default:false"`
	LastTestedAt        *time.Time `json:"last_tested_at,omitempty"`
}

// APIKeys for various third-party services (all encrypted)
type APIKeys struct {
	OpenAI        string `json:"openai" gorm:"type:varchar(500)"` // encrypted
	Stripe        string `json:"stripe" gorm:"type:varchar(500)"` // encrypted
	Twilio        string `json:"twilio" gorm:"type:varchar(500)"` // encrypted
	SendGrid      string `json:"sendgrid" gorm:"type:varchar(500)"` // encrypted
	Cloudinary    string `json:"cloudinary" gorm:"type:varchar(500)"` // encrypted
	Firebase      string `json:"firebase" gorm:"type:varchar(500)"` // encrypted
	Pusher        string `json:"pusher" gorm:"type:varchar(500)"` // encrypted
	Mailgun       string `json:"mailgun" gorm:"type:varchar(500)"` // encrypted
	GoogleMaps    string `json:"google_maps" gorm:"type:varchar(500)"` // encrypted
	AWSSecretKey  string `json:"aws_secret_key" gorm:"type:varchar(500)"` // encrypted
}

// IntegrationLog tracks integration events and errors
type IntegrationLog struct {
	ID               string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID   string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	Integration      string         `json:"integration" gorm:"type:varchar(50);not null;index"` // stripe, paypal, twilio, etc.
	Event            string         `json:"event" gorm:"type:varchar(100);not null;index"` // connection_test, payment_created, sms_sent, etc.
	Status           string         `json:"status" gorm:"type:varchar(20);not null;index"` // success, error, warning
	Message          string         `json:"message" gorm:"type:text"`
	ErrorCode        string         `json:"error_code" gorm:"type:varchar(100);index"`
	RequestData      string         `json:"request_data" gorm:"type:text"` // JSON
	ResponseData     string         `json:"response_data" gorm:"type:text"` // JSON
	Duration         int            `json:"duration"` // milliseconds
	UserID           string         `json:"user_id" gorm:"type:varchar(255);index"`
	IPAddress        string         `json:"ip_address" gorm:"type:varchar(45)"`
	CreatedAt        time.Time      `json:"created_at"`

	// Relationships
	Organization     Organization   `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	User             *User          `json:"user,omitempty" gorm:"foreignKey:UserID"`
}

// PaymentTransaction tracks all payment transactions across providers
type PaymentTransaction struct {
	ID                  string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID      string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	CustomerID          string         `json:"customer_id" gorm:"type:varchar(255);index"`
	BookingID           string         `json:"booking_id" gorm:"type:varchar(255);index"`

	// Payment Details
	Provider            string         `json:"provider" gorm:"type:varchar(20);not null;index"` // stripe, paypal
	ProviderTransactionID string       `json:"provider_transaction_id" gorm:"type:varchar(255);index"`
	Amount              int64          `json:"amount"` // Amount in cents
	Currency            string         `json:"currency" gorm:"type:varchar(3);not null"`
	Status              string         `json:"status" gorm:"type:varchar(20);not null;index"` // pending, completed, failed, refunded
	PaymentMethod       string         `json:"payment_method" gorm:"type:varchar(50)"` // card, bank_transfer, paypal

	// Transaction Metadata
	Description         string         `json:"description" gorm:"type:varchar(500)"`
	Metadata            string         `json:"metadata" gorm:"type:text"` // JSON
	FailureReason       string         `json:"failure_reason" gorm:"type:varchar(500)"`
	RefundAmount        int64          `json:"refund_amount" gorm:"default:0"` // Amount refunded in cents
	RefundReason        string         `json:"refund_reason" gorm:"type:varchar(500)"`

	// Timestamps
	ProcessedAt         *time.Time     `json:"processed_at,omitempty"`
	RefundedAt          *time.Time     `json:"refunded_at,omitempty"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	DeletedAt           gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization        Organization   `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Customer            *Customer      `json:"customer,omitempty" gorm:"foreignKey:CustomerID"`
	Booking             *Booking       `json:"booking,omitempty" gorm:"foreignKey:BookingID"`
}

// CommunicationLog tracks all communications (SMS, WhatsApp, Email)
type CommunicationLog struct {
	ID              string         `json:"id" gorm:"type:varchar(255);primaryKey"`
	OrganizationID  string         `json:"organization_id" gorm:"type:varchar(255);not null;index"`
	CustomerID      string         `json:"customer_id" gorm:"type:varchar(255);index"`
	BookingID       string         `json:"booking_id" gorm:"type:varchar(255);index"`

	// Communication Details
	Type            string         `json:"type" gorm:"type:varchar(20);not null;index"` // sms, whatsapp, email, voice
	Provider        string         `json:"provider" gorm:"type:varchar(20);not null;index"` // twilio, whatsapp_business, smtp
	Direction       string         `json:"direction" gorm:"type:varchar(10);not null;index"` // inbound, outbound

	// Contact Information
	FromNumber      string         `json:"from_number" gorm:"type:varchar(50)"`
	ToNumber        string         `json:"to_number" gorm:"type:varchar(50)"`
	FromEmail       string         `json:"from_email" gorm:"type:varchar(255)"`
	ToEmail         string         `json:"to_email" gorm:"type:varchar(255)"`

	// Message Content
	Subject         string         `json:"subject" gorm:"type:varchar(500)"`
	Message         string         `json:"message" gorm:"type:text"`
	MessageType     string         `json:"message_type" gorm:"type:varchar(20)"` // text, template, media
	TemplateID      string         `json:"template_id" gorm:"type:varchar(255)"`
	MediaURLs       string         `json:"media_urls" gorm:"type:text"` // JSON array

	// Status and Tracking
	Status          string         `json:"status" gorm:"type:varchar(20);not null;index"` // sent, delivered, failed, read
	ProviderMsgID   string         `json:"provider_msg_id" gorm:"type:varchar(255);index"`
	ErrorMessage    string         `json:"error_message" gorm:"type:text"`
	DeliveredAt     *time.Time     `json:"delivered_at,omitempty"`
	ReadAt          *time.Time     `json:"read_at,omitempty"`

	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`

	// Relationships
	Organization    Organization   `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	Customer        *Customer      `json:"customer,omitempty" gorm:"foreignKey:CustomerID"`
	Booking         *Booking       `json:"booking,omitempty" gorm:"foreignKey:BookingID"`
}

// BeforeCreate generates UUIDs for new records
func (i *IntegrationSettings) BeforeCreate(tx *gorm.DB) error {
	if i.ID == "" {
		i.ID = uuid.New().String()
	}
	return nil
}

func (i *IntegrationLog) BeforeCreate(tx *gorm.DB) error {
	if i.ID == "" {
		i.ID = uuid.New().String()
	}
	return nil
}

func (p *PaymentTransaction) BeforeCreate(tx *gorm.DB) error {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	return nil
}

func (c *CommunicationLog) BeforeCreate(tx *gorm.DB) error {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	return nil
}

// Helper methods for JSON serialization
func (a *APIKeys) ToJSON() string {
	data, _ := json.Marshal(a)
	return string(data)
}

func (a *APIKeys) FromJSON(data string) error {
	return json.Unmarshal([]byte(data), a)
}

// Integration status helper methods
func (i *IntegrationSettings) GetEnabledIntegrations() []string {
	var enabled []string

	if i.StripeSettings.Enabled {
		enabled = append(enabled, "stripe")
	}
	if i.PayPalSettings.Enabled {
		enabled = append(enabled, "paypal")
	}
	if i.TwilioSettings.Enabled {
		enabled = append(enabled, "twilio")
	}
	if i.WhatsAppSettings.Enabled {
		enabled = append(enabled, "whatsapp")
	}
	if i.ZohoSettings.Enabled {
		enabled = append(enabled, "zoho")
	}
	if i.FirebaseSettings.Enabled {
		enabled = append(enabled, "firebase")
	}
	if i.EmailSettings.Enabled {
		enabled = append(enabled, "email")
	}

	return enabled
}

func (i *IntegrationSettings) IsPaymentEnabled() bool {
	return i.StripeSettings.Enabled || i.PayPalSettings.Enabled
}

func (i *IntegrationSettings) IsCommunicationEnabled() bool {
	return i.TwilioSettings.Enabled || i.WhatsAppSettings.Enabled || i.EmailSettings.Enabled
}