package integrations

import (
	"fmt"
	"log"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/encryption"
	"github.com/twilio/twilio-go"
	twilioApi "github.com/twilio/twilio-go/rest/api/v2010"
	"gorm.io/gorm"
)

// TwilioService handles Twilio SMS and voice integration
type TwilioService struct {
	db      *gorm.DB
	encrypt *encryption.EncryptionService
}

// NewTwilioService creates a new Twilio service
func NewTwilioService(db *gorm.DB) *TwilioService {
	return &TwilioService{
		db:      db,
		encrypt: encryption.GlobalEncryption,
	}
}

// TestConnection tests the Twilio API connection
func (t *TwilioService) TestConnection(settings *models.TwilioSettings) error {
	// Decrypt auth token
	authToken, err := t.encrypt.Decrypt(settings.AuthToken)
	if err != nil {
		return fmt.Errorf("failed to decrypt auth token: %w", err)
	}

	// Create Twilio client
	client := twilio.NewRestClientWithParams(twilio.ClientParams{
		Username: settings.AccountSID,
		Password: authToken,
	})

	// Test by fetching account information
	params := &twilioApi.FetchAccountParams{}
	_, err = client.Api.FetchAccount(settings.AccountSID, params)
	if err != nil {
		return fmt.Errorf("twilio connection test failed: %w", err)
	}

	// Update connection status
	settings.ConnectionVerified = true
	now := time.Now()
	settings.LastTestedAt = &now

	return nil
}

// SendSMS sends an SMS message
func (t *TwilioService) SendSMS(organizationID, customerID, bookingID, toNumber, message string) error {
	// Get integration settings
	var settings models.IntegrationSettings
	if err := t.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		return fmt.Errorf("failed to get integration settings: %w", err)
	}

	if !settings.TwilioSettings.Enabled || !settings.TwilioSettings.EnableSMS {
		return fmt.Errorf("Twilio SMS is not enabled")
	}

	// Decrypt auth token
	authToken, err := t.encrypt.Decrypt(settings.TwilioSettings.AuthToken)
	if err != nil {
		return fmt.Errorf("failed to decrypt auth token: %w", err)
	}

	// Create Twilio client
	client := twilio.NewRestClientWithParams(twilio.ClientParams{
		Username: settings.TwilioSettings.AccountSID,
		Password: authToken,
	})

	// Prepare message parameters
	params := &twilioApi.CreateMessageParams{}
	params.SetTo(toNumber)
	params.SetBody(message)

	// Use messaging service if available, otherwise use phone number
	if settings.TwilioSettings.MessagingServiceSID != "" {
		params.SetMessagingServiceSid(settings.TwilioSettings.MessagingServiceSID)
	} else {
		params.SetFrom(settings.TwilioSettings.FromPhoneNumber)
	}

	// Send message
	resp, err := client.Api.CreateMessage(params)
	if err != nil {
		t.LogCommunicationEvent(organizationID, customerID, bookingID, "sms", "twilio", "outbound",
			settings.TwilioSettings.FromPhoneNumber, toNumber, "", "", "", message, "failed", "", err.Error())
		return fmt.Errorf("failed to send SMS: %w", err)
	}

	// Log successful SMS
	t.LogCommunicationEvent(organizationID, customerID, bookingID, "sms", "twilio", "outbound",
		settings.TwilioSettings.FromPhoneNumber, toNumber, "", "", "", message, "sent", *resp.Sid, "")

	return nil
}

// SendWhatsAppMessage sends a WhatsApp message via Twilio
func (t *TwilioService) SendWhatsAppMessage(organizationID, customerID, bookingID, toNumber, message string) error {
	// Get integration settings
	var settings models.IntegrationSettings
	if err := t.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		return fmt.Errorf("failed to get integration settings: %w", err)
	}

	if !settings.TwilioSettings.Enabled || !settings.TwilioSettings.EnableWhatsApp {
		return fmt.Errorf("Twilio WhatsApp is not enabled")
	}

	// Decrypt auth token
	authToken, err := t.encrypt.Decrypt(settings.TwilioSettings.AuthToken)
	if err != nil {
		return fmt.Errorf("failed to decrypt auth token: %w", err)
	}

	// Create Twilio client
	client := twilio.NewRestClientWithParams(twilio.ClientParams{
		Username: settings.TwilioSettings.AccountSID,
		Password: authToken,
	})

	// Format WhatsApp numbers
	fromNumber := fmt.Sprintf("whatsapp:%s", settings.TwilioSettings.WhatsAppFromNumber)
	toWhatsApp := fmt.Sprintf("whatsapp:%s", toNumber)

	// Prepare message parameters
	params := &twilioApi.CreateMessageParams{}
	params.SetTo(toWhatsApp)
	params.SetFrom(fromNumber)
	params.SetBody(message)

	// Send message
	resp, err := client.Api.CreateMessage(params)
	if err != nil {
		t.LogCommunicationEvent(organizationID, customerID, bookingID, "whatsapp", "twilio", "outbound",
			fromNumber, toWhatsApp, "", "", "", message, "failed", "", err.Error())
		return fmt.Errorf("failed to send WhatsApp message: %w", err)
	}

	// Log successful message
	t.LogCommunicationEvent(organizationID, customerID, bookingID, "whatsapp", "twilio", "outbound",
		fromNumber, toWhatsApp, "", "", "", message, "sent", *resp.Sid, "")

	return nil
}

// SendBookingConfirmationSMS sends booking confirmation via SMS
func (t *TwilioService) SendBookingConfirmationSMS(organizationID string, booking *models.Booking, customer *models.Customer) error {
	message := fmt.Sprintf("Booking Confirmed!\nID: %s\nDate: %s\nTime: %s\nThank you for choosing our services!",
		booking.ID,
		booking.StartTime.Format("Jan 2, 2006"),
		booking.StartTime.Format("3:04 PM"),
	)

	return t.SendSMS(organizationID, customer.ID, booking.ID, customer.Phone, message)
}

// SendBookingReminderSMS sends booking reminder via SMS
func (t *TwilioService) SendBookingReminderSMS(organizationID string, booking *models.Booking, customer *models.Customer, hoursBeforeBooking int) error {
	message := fmt.Sprintf("Booking Reminder!\nID: %s\nDate: %s\nTime: %s\nYour appointment is in %d hours. Please arrive on time.",
		booking.ID,
		booking.StartTime.Format("Jan 2, 2006"),
		booking.StartTime.Format("3:04 PM"),
		hoursBeforeBooking,
	)

	return t.SendSMS(organizationID, customer.ID, booking.ID, customer.Phone, message)
}

// SendBookingConfirmationWhatsApp sends booking confirmation via WhatsApp
func (t *TwilioService) SendBookingConfirmationWhatsApp(organizationID string, booking *models.Booking, customer *models.Customer) error {
	message := fmt.Sprintf(`🎉 *Booking Confirmed!*

📋 *Booking Details:*
• ID: %s
• Date: %s
• Time: %s

Thank you for choosing our services! We look forward to serving you.

Reply STOP to opt out.`,
		booking.ID,
		booking.StartTime.Format("January 2, 2006"),
		booking.StartTime.Format("3:04 PM"),
	)

	return t.SendWhatsAppMessage(organizationID, customer.ID, booking.ID, customer.Phone, message)
}

// SendBookingReminderWhatsApp sends booking reminder via WhatsApp
func (t *TwilioService) SendBookingReminderWhatsApp(organizationID string, booking *models.Booking, customer *models.Customer, hoursBeforeBooking int) error {
	message := fmt.Sprintf(`⏰ *Booking Reminder*

📋 *Booking Details:*
• ID: %s
• Date: %s
• Time: %s

⏳ Your appointment is in *%d hours*. Please arrive on time.

Reply STOP to opt out.`,
		booking.ID,
		booking.StartTime.Format("January 2, 2006"),
		booking.StartTime.Format("3:04 PM"),
		hoursBeforeBooking,
	)

	return t.SendWhatsAppMessage(organizationID, customer.ID, booking.ID, customer.Phone, message)
}

// MakeVoiceCall makes a voice call
func (t *TwilioService) MakeVoiceCall(organizationID, customerID, bookingID, toNumber, message string) error {
	// Get integration settings
	var settings models.IntegrationSettings
	if err := t.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		return fmt.Errorf("failed to get integration settings: %w", err)
	}

	if !settings.TwilioSettings.Enabled || !settings.TwilioSettings.EnableVoice {
		return fmt.Errorf("Twilio Voice is not enabled")
	}

	// Decrypt auth token
	authToken, err := t.encrypt.Decrypt(settings.TwilioSettings.AuthToken)
	if err != nil {
		return fmt.Errorf("failed to decrypt auth token: %w", err)
	}

	// Create Twilio client
	client := twilio.NewRestClientWithParams(twilio.ClientParams{
		Username: settings.TwilioSettings.AccountSID,
		Password: authToken,
	})

	// Create TwiML URL for the message
	twimlURL := fmt.Sprintf("http://twimlets.com/message?Message=%s", message)

	// Prepare call parameters
	params := &twilioApi.CreateCallParams{}
	params.SetTo(toNumber)
	params.SetFrom(settings.TwilioSettings.FromPhoneNumber)
	params.SetUrl(twimlURL)

	// Make call
	resp, err := client.Api.CreateCall(params)
	if err != nil {
		t.LogCommunicationEvent(organizationID, customerID, bookingID, "voice", "twilio", "outbound",
			settings.TwilioSettings.FromPhoneNumber, toNumber, "", "", "", message, "failed", "", err.Error())
		return fmt.Errorf("failed to make voice call: %w", err)
	}

	// Log successful call
	t.LogCommunicationEvent(organizationID, customerID, bookingID, "voice", "twilio", "outbound",
		settings.TwilioSettings.FromPhoneNumber, toNumber, "", "", "", message, "initiated", *resp.Sid, "")

	return nil
}

// GetMessageStatus gets the status of a sent message
func (t *TwilioService) GetMessageStatus(organizationID, messageSID string) (string, error) {
	// Get integration settings
	var settings models.IntegrationSettings
	if err := t.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		return "", fmt.Errorf("failed to get integration settings: %w", err)
	}

	// Decrypt auth token
	authToken, err := t.encrypt.Decrypt(settings.TwilioSettings.AuthToken)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt auth token: %w", err)
	}

	// Create Twilio client
	client := twilio.NewRestClientWithParams(twilio.ClientParams{
		Username: settings.TwilioSettings.AccountSID,
		Password: authToken,
	})

	// Fetch message
	params := &twilioApi.FetchMessageParams{}
	resp, err := client.Api.FetchMessage(messageSID, params)
	if err != nil {
		return "", fmt.Errorf("failed to fetch message status: %w", err)
	}

	return string(*resp.Status), nil
}

// HandleWebhook handles Twilio webhooks for message status updates
func (t *TwilioService) HandleWebhook(organizationID string, webhookData map[string]string) error {
	messageSID := webhookData["MessageSid"]
	status := webhookData["MessageStatus"]
	errorCode := webhookData["ErrorCode"]

	// Update communication log
	updates := map[string]interface{}{
		"status": status,
	}

	if status == "delivered" {
		now := time.Now()
		updates["delivered_at"] = now
	}

	if errorCode != "" {
		updates["error_message"] = fmt.Sprintf("Error code: %s", errorCode)
	}

	if err := t.db.Model(&models.CommunicationLog{}).
		Where("provider_msg_id = ? AND organization_id = ?", messageSID, organizationID).
		Updates(updates).Error; err != nil {
		return fmt.Errorf("failed to update communication log: %w", err)
	}

	// Log integration event
	t.LogIntegrationEvent(organizationID, "webhook_message_status", "success",
		fmt.Sprintf("Message status updated to %s for SID %s", status, messageSID), errorCode, "")

	return nil
}

// LogCommunicationEvent logs communication events
func (t *TwilioService) LogCommunicationEvent(organizationID, customerID, bookingID, commType, provider, direction,
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

	if err := t.db.Create(logEntry).Error; err != nil {
		log.Printf("Failed to log communication event: %v", err)
	}
}

// LogIntegrationEvent logs integration events
func (t *TwilioService) LogIntegrationEvent(organizationID, event, status, message, errorCode, userID string) {
	logEntry := &models.IntegrationLog{
		OrganizationID: organizationID,
		Integration:    "twilio",
		Event:          event,
		Status:         status,
		Message:        message,
		ErrorCode:      errorCode,
		UserID:         userID,
	}

	if err := t.db.Create(logEntry).Error; err != nil {
		log.Printf("Failed to log integration event: %v", err)
	}
}

// GetSMSTemplates returns common SMS templates
func (t *TwilioService) GetSMSTemplates() map[string]string {
	return map[string]string{
		"booking_confirmation": "Booking Confirmed! ID: {{.BookingID}}, Date: {{.Date}}, Time: {{.Time}}. Thank you!",
		"booking_reminder":     "Reminder: Your appointment ({{.BookingID}}) is in {{.HoursUntil}} hours on {{.Date}} at {{.Time}}. Please arrive on time.",
		"booking_cancelled":    "Your booking {{.BookingID}} has been cancelled. Contact us if you have questions.",
		"payment_received":     "Payment received for booking {{.BookingID}}. Amount: ${{.Amount}}. Thank you!",
		"service_completed":    "Service completed for booking {{.BookingID}}. Thank you for choosing us! Please rate your experience.",
	}
}

// GetWhatsAppTemplates returns common WhatsApp templates
func (t *TwilioService) GetWhatsAppTemplates() map[string]string {
	return map[string]string{
		"booking_confirmation": `🎉 *Booking Confirmed!*

📋 *Details:*
• ID: {{.BookingID}}
• Date: {{.Date}}
• Time: {{.Time}}

Thank you for choosing our services!
Reply STOP to opt out.`,
		"booking_reminder": `⏰ *Booking Reminder*

📋 *Details:*
• ID: {{.BookingID}}
• Date: {{.Date}}
• Time: {{.Time}}

⏳ Your appointment is in *{{.HoursUntil}} hours*.
Reply STOP to opt out.`,
		"payment_received": `💳 *Payment Received*

✅ Payment confirmed for booking {{.BookingID}}
💰 Amount: ${{.Amount}}

Thank you for your payment!
Reply STOP to opt out.`,
	}
}