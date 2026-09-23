package integrations

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/encryption"
	"github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"
	"gopkg.in/gomail.v2"
	"gorm.io/gorm"
)

// EmailService handles email integration
type EmailService struct {
	db      *gorm.DB
	encrypt *encryption.EncryptionService
}

// EmailMessage represents an email message
type EmailMessage struct {
	To          []string
	CC          []string
	BCC         []string
	Subject     string
	PlainBody   string
	HTMLBody    string
	Attachments []EmailAttachment
	Priority    string // high, normal, low
}

// EmailAttachment represents an email attachment
type EmailAttachment struct {
	Filename    string
	Content     []byte
	ContentType string
}

// NewEmailService creates a new email service
func NewEmailService(db *gorm.DB) *EmailService {
	return &EmailService{
		db:      db,
		encrypt: encryption.GlobalEncryption,
	}
}

// TestConnection tests the email configuration
func (e *EmailService) TestConnection(settings *models.EmailSettings) error {
	switch strings.ToLower(settings.Provider) {
	case "smtp", "gmail", "outlook":
		return e.testSMTPConnection(settings)
	case "sendgrid":
		return e.testSendGridConnection(settings)
	default:
		return fmt.Errorf("unsupported email provider: %s", settings.Provider)
	}
}

// testSMTPConnection tests SMTP connection
func (e *EmailService) testSMTPConnection(settings *models.EmailSettings) error {
	// Decrypt password
	password, err := e.encrypt.Decrypt(settings.SMTPPassword)
	if err != nil {
		return fmt.Errorf("failed to decrypt SMTP password: %w", err)
	}

	// Create SMTP auth
	auth := smtp.PlainAuth("", settings.SMTPUsername, password, settings.SMTPHost)

	// Test connection
	addr := fmt.Sprintf("%s:%d", settings.SMTPHost, settings.SMTPPort)

	// For TLS/SSL connections
	if settings.SMTPSecurity == "ssl" || settings.SMTPPort == 465 {
		// SSL connection
		conn, err := tls.Dial("tcp", addr, &tls.Config{
			ServerName: settings.SMTPHost,
		})
		if err != nil {
			return fmt.Errorf("SSL connection failed: %w", err)
		}
		defer conn.Close()

		client, err := smtp.NewClient(conn, settings.SMTPHost)
		if err != nil {
			return fmt.Errorf("SMTP client creation failed: %w", err)
		}
		defer client.Close()

		// Test authentication
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP authentication failed: %w", err)
		}
	} else {
		// Plain or STARTTLS connection
		client, err := smtp.Dial(addr)
		if err != nil {
			return fmt.Errorf("SMTP connection failed: %w", err)
		}
		defer client.Close()

		// Start TLS if required
		if settings.SMTPSecurity == "tls" {
			if err := client.StartTLS(&tls.Config{
				ServerName: settings.SMTPHost,
			}); err != nil {
				return fmt.Errorf("STARTTLS failed: %w", err)
			}
		}

		// Test authentication
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP authentication failed: %w", err)
		}
	}

	// Update connection status
	settings.TestEmailSent = true
	now := time.Now()
	settings.LastTestedAt = &now

	return nil
}

// testSendGridConnection tests SendGrid API connection
func (e *EmailService) testSendGridConnection(settings *models.EmailSettings) error {
	// Get SendGrid API key from settings
	// For SendGrid, we store the API key in SMTPPassword field
	apiKey, err := e.encrypt.Decrypt(settings.SMTPPassword)
	if err != nil {
		return fmt.Errorf("failed to decrypt SendGrid API key: %w", err)
	}

	// Create SendGrid client
	client := sendgrid.NewSendClient(apiKey)

	// Create a test email
	from := mail.NewEmail(settings.FromName, settings.FromEmail)
	to := mail.NewEmail("Test", settings.FromEmail)
	subject := "SendGrid Connection Test"
	plainTextContent := "This is a test email to verify SendGrid connection."
	htmlContent := "<p>This is a test email to verify SendGrid connection.</p>"

	message := mail.NewSingleEmail(from, subject, to, plainTextContent, htmlContent)

	// Send test email
	response, err := client.Send(message)
	if err != nil {
		return fmt.Errorf("SendGrid connection test failed: %w", err)
	}

	if response.StatusCode >= 400 {
		return fmt.Errorf("SendGrid API error: status %d, body: %s", response.StatusCode, response.Body)
	}

	// Update connection status
	settings.TestEmailSent = true
	now := time.Now()
	settings.LastTestedAt = &now

	return nil
}

// SendEmail sends an email using the configured provider
func (e *EmailService) SendEmail(organizationID string, message *EmailMessage) error {
	// Get integration settings
	var settings models.IntegrationSettings
	if err := e.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		return fmt.Errorf("failed to get integration settings: %w", err)
	}

	if !settings.EmailSettings.Enabled {
		return fmt.Errorf("email integration is not enabled")
	}

	switch strings.ToLower(settings.EmailSettings.Provider) {
	case "smtp", "gmail", "outlook":
		return e.sendSMTPEmail(&settings.EmailSettings, message, organizationID)
	case "sendgrid":
		return e.sendSendGridEmail(&settings.EmailSettings, message, organizationID)
	default:
		return fmt.Errorf("unsupported email provider: %s", settings.EmailSettings.Provider)
	}
}

// sendSMTPEmail sends email via SMTP
func (e *EmailService) sendSMTPEmail(settings *models.EmailSettings, message *EmailMessage, organizationID string) error {
	// Decrypt password
	password, err := e.encrypt.Decrypt(settings.SMTPPassword)
	if err != nil {
		return fmt.Errorf("failed to decrypt SMTP password: %w", err)
	}

	// Create gomail message
	m := gomail.NewMessage()

	// Set headers
	m.SetHeader("From", fmt.Sprintf("%s <%s>", settings.FromName, settings.FromEmail))
	m.SetHeader("To", message.To...)

	if len(message.CC) > 0 {
		m.SetHeader("Cc", message.CC...)
	}
	if len(message.BCC) > 0 {
		m.SetHeader("Bcc", message.BCC...)
	}

	m.SetHeader("Subject", message.Subject)

	if settings.ReplyToEmail != "" {
		m.SetHeader("Reply-To", settings.ReplyToEmail)
	}

	// Set priority
	switch strings.ToLower(message.Priority) {
	case "high":
		m.SetHeader("X-Priority", "1")
		m.SetHeader("X-MSMail-Priority", "High")
	case "low":
		m.SetHeader("X-Priority", "5")
		m.SetHeader("X-MSMail-Priority", "Low")
	}

	// Set body
	if message.HTMLBody != "" {
		m.SetBody("text/html", message.HTMLBody)
		if message.PlainBody != "" {
			m.AddAlternative("text/plain", message.PlainBody)
		}
	} else {
		m.SetBody("text/plain", message.PlainBody)
	}

	// Add attachments (simplified version)
	for _, attachment := range message.Attachments {
		// For simplicity, skip attachments in this implementation
		_ = attachment
	}

	// Create dialer
	d := gomail.NewDialer(settings.SMTPHost, settings.SMTPPort, settings.SMTPUsername, password)

	// Set TLS config
	if settings.SMTPSecurity == "ssl" {
		d.SSL = true
	} else if settings.SMTPSecurity == "tls" {
		d.TLSConfig = &tls.Config{
			ServerName: settings.SMTPHost,
		}
	}

	// Send email
	if err := d.DialAndSend(m); err != nil {
		e.LogCommunicationEvent(organizationID, "", "", "email", settings.Provider, "outbound",
			"", "", settings.FromEmail, strings.Join(message.To, ","), message.Subject, message.PlainBody,
			"failed", "", err.Error())
		return fmt.Errorf("failed to send email: %w", err)
	}

	// Log successful email
	e.LogCommunicationEvent(organizationID, "", "", "email", settings.Provider, "outbound",
		"", "", settings.FromEmail, strings.Join(message.To, ","), message.Subject, message.PlainBody,
		"sent", "", "")

	return nil
}

// sendSendGridEmail sends email via SendGrid
func (e *EmailService) sendSendGridEmail(settings *models.EmailSettings, message *EmailMessage, organizationID string) error {
	// Get SendGrid API key
	apiKey, err := e.encrypt.Decrypt(settings.SMTPPassword)
	if err != nil {
		return fmt.Errorf("failed to decrypt SendGrid API key: %w", err)
	}

	// Create SendGrid client
	client := sendgrid.NewSendClient(apiKey)

	// Create email
	from := mail.NewEmail(settings.FromName, settings.FromEmail)
	subject := message.Subject

	// Create personalization for each recipient
	m := mail.NewV3Mail()
	m.SetFrom(from)
	m.Subject = subject

	personalization := mail.NewPersonalization()

	// Add recipients
	for _, to := range message.To {
		personalization.AddTos(mail.NewEmail("", to))
	}
	for _, cc := range message.CC {
		personalization.AddCCs(mail.NewEmail("", cc))
	}
	for _, bcc := range message.BCC {
		personalization.AddBCCs(mail.NewEmail("", bcc))
	}

	m.AddPersonalizations(personalization)

	// Add content
	if message.PlainBody != "" {
		m.AddContent(mail.NewContent("text/plain", message.PlainBody))
	}
	if message.HTMLBody != "" {
		m.AddContent(mail.NewContent("text/html", message.HTMLBody))
	}

	// Add reply-to
	if settings.ReplyToEmail != "" {
		m.SetReplyTo(mail.NewEmail("", settings.ReplyToEmail))
	}

	// Add attachments
	for _, attachment := range message.Attachments {
		a := mail.NewAttachment()
		a.SetFilename(attachment.Filename)
		a.SetContent(string(attachment.Content))
		a.SetType(attachment.ContentType)
		a.SetDisposition("attachment")
		m.AddAttachment(a)
	}

	// Send email
	response, err := client.Send(m)
	if err != nil {
		e.LogCommunicationEvent(organizationID, "", "", "email", "sendgrid", "outbound",
			"", "", settings.FromEmail, strings.Join(message.To, ","), message.Subject, message.PlainBody,
			"failed", "", err.Error())
		return fmt.Errorf("failed to send email via SendGrid: %w", err)
	}

	if response.StatusCode >= 400 {
		errorMsg := fmt.Sprintf("SendGrid API error: status %d", response.StatusCode)
		e.LogCommunicationEvent(organizationID, "", "", "email", "sendgrid", "outbound",
			"", "", settings.FromEmail, strings.Join(message.To, ","), message.Subject, message.PlainBody,
			"failed", "", errorMsg)
		return fmt.Errorf(errorMsg)
	}

	// Log successful email
	e.LogCommunicationEvent(organizationID, "", "", "email", "sendgrid", "outbound",
		"", "", settings.FromEmail, strings.Join(message.To, ","), message.Subject, message.PlainBody,
		"sent", "", "")

	return nil
}

// SendBookingConfirmation sends a booking confirmation email
func (e *EmailService) SendBookingConfirmation(organizationID string, booking *models.Booking, customer *models.Customer) error {
	subject := fmt.Sprintf("Booking Confirmation - %s", booking.ID)

	plainBody := fmt.Sprintf(`
Dear %s,

Your booking has been confirmed!

Booking Details:
- Booking ID: %s
- Date: %s
- Time: %s
- Status: %s

Thank you for choosing our services.

Best regards,
The Team
`, customer.FirstName, booking.ID, booking.StartTime.Format("2006-01-02"),
   booking.StartTime.Format("15:04"), booking.Status)

	htmlBody := fmt.Sprintf(`
<h2>Booking Confirmation</h2>
<p>Dear %s,</p>
<p>Your booking has been confirmed!</p>

<h3>Booking Details:</h3>
<ul>
<li><strong>Booking ID:</strong> %s</li>
<li><strong>Date:</strong> %s</li>
<li><strong>Time:</strong> %s</li>
<li><strong>Status:</strong> %s</li>
</ul>

<p>Thank you for choosing our services.</p>
<p>Best regards,<br>The Team</p>
`, customer.FirstName, booking.ID, booking.StartTime.Format("January 2, 2006"),
   booking.StartTime.Format("3:04 PM"), booking.Status)

	message := &EmailMessage{
		To:        []string{customer.Email},
		Subject:   subject,
		PlainBody: plainBody,
		HTMLBody:  htmlBody,
		Priority:  "normal",
	}

	return e.SendEmail(organizationID, message)
}

// SendBookingReminder sends a booking reminder email
func (e *EmailService) SendBookingReminder(organizationID string, booking *models.Booking, customer *models.Customer, hoursBeforeBooking int) error {
	subject := fmt.Sprintf("Booking Reminder - %s", booking.ID)

	plainBody := fmt.Sprintf(`
Dear %s,

This is a reminder about your upcoming booking.

Booking Details:
- Booking ID: %s
- Date: %s
- Time: %s
- Status: %s

Your appointment is in %d hours. Please arrive on time.

Best regards,
The Team
`, customer.FirstName, booking.ID, booking.StartTime.Format("2006-01-02"),
   booking.StartTime.Format("15:04"), booking.Status, hoursBeforeBooking)

	htmlBody := fmt.Sprintf(`
<h2>Booking Reminder</h2>
<p>Dear %s,</p>
<p>This is a reminder about your upcoming booking.</p>

<h3>Booking Details:</h3>
<ul>
<li><strong>Booking ID:</strong> %s</li>
<li><strong>Date:</strong> %s</li>
<li><strong>Time:</strong> %s</li>
<li><strong>Status:</strong> %s</li>
</ul>

<p>Your appointment is in <strong>%d hours</strong>. Please arrive on time.</p>
<p>Best regards,<br>The Team</p>
`, customer.FirstName, booking.ID, booking.StartTime.Format("January 2, 2006"),
   booking.StartTime.Format("3:04 PM"), booking.Status, hoursBeforeBooking)

	message := &EmailMessage{
		To:        []string{customer.Email},
		Subject:   subject,
		PlainBody: plainBody,
		HTMLBody:  htmlBody,
		Priority:  "normal",
	}

	return e.SendEmail(organizationID, message)
}

// LogCommunicationEvent logs communication events
func (e *EmailService) LogCommunicationEvent(organizationID, customerID, bookingID, commType, provider, direction,
	fromNumber, toNumber, fromEmail, toEmail, subject, message, status, providerMsgID, errorMessage string) {

	logEntry := &models.CommunicationLog{
		OrganizationID: organizationID,
		CustomerID:     customerID,
		BookingID:      bookingID,
		Type:           commType,
		Provider:       provider,
		Direction:      direction,
		FromNumber:     fromNumber,
		ToNumber:       toNumber,
		FromEmail:      fromEmail,
		ToEmail:        toEmail,
		Subject:        subject,
		Message:        message,
		Status:         status,
		ProviderMsgID:  providerMsgID,
		ErrorMessage:   errorMessage,
		MessageType:    "text",
	}

	if err := e.db.Create(logEntry).Error; err != nil {
		log.Printf("Failed to log communication event: %v", err)
	}
}

// GetEmailTemplates returns common email templates
func (e *EmailService) GetEmailTemplates() map[string]map[string]string {
	return map[string]map[string]string{
		"booking_confirmation": {
			"subject": "Booking Confirmation - {{.BookingID}}",
			"plain": `Dear {{.CustomerName}},

Your booking has been confirmed!

Booking Details:
- Booking ID: {{.BookingID}}
- Date: {{.Date}}
- Time: {{.Time}}
- Status: {{.Status}}

Thank you for choosing our services.

Best regards,
{{.CompanyName}}`,
			"html": `<h2>Booking Confirmation</h2>
<p>Dear {{.CustomerName}},</p>
<p>Your booking has been confirmed!</p>
<h3>Booking Details:</h3>
<ul>
<li><strong>Booking ID:</strong> {{.BookingID}}</li>
<li><strong>Date:</strong> {{.Date}}</li>
<li><strong>Time:</strong> {{.Time}}</li>
<li><strong>Status:</strong> {{.Status}}</li>
</ul>
<p>Thank you for choosing our services.</p>
<p>Best regards,<br>{{.CompanyName}}</p>`,
		},
		"booking_reminder": {
			"subject": "Booking Reminder - {{.BookingID}}",
			"plain": `Dear {{.CustomerName}},

This is a reminder about your upcoming booking.

Booking Details:
- Booking ID: {{.BookingID}}
- Date: {{.Date}}
- Time: {{.Time}}

Your appointment is in {{.HoursUntil}} hours. Please arrive on time.

Best regards,
{{.CompanyName}}`,
			"html": `<h2>Booking Reminder</h2>
<p>Dear {{.CustomerName}},</p>
<p>This is a reminder about your upcoming booking.</p>
<h3>Booking Details:</h3>
<ul>
<li><strong>Booking ID:</strong> {{.BookingID}}</li>
<li><strong>Date:</strong> {{.Date}}</li>
<li><strong>Time:</strong> {{.Time}}</li>
</ul>
<p>Your appointment is in <strong>{{.HoursUntil}} hours</strong>. Please arrive on time.</p>
<p>Best regards,<br>{{.CompanyName}}</p>`,
		},
	}
}