package integrations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/encryption"
	"gorm.io/gorm"
)

// WhatsAppService handles WhatsApp Business API integration
type WhatsAppService struct {
	db      *gorm.DB
	encrypt *encryption.EncryptionService
}

// WhatsAppMessage represents a WhatsApp message structure
type WhatsAppMessage struct {
	MessagingProduct string                 `json:"messaging_product"`
	To               string                 `json:"to"`
	Type             string                 `json:"type"`
	Text             *WhatsAppText          `json:"text,omitempty"`
	Template         *WhatsAppTemplate      `json:"template,omitempty"`
	Image            *WhatsAppMedia         `json:"image,omitempty"`
	Document         *WhatsAppMedia         `json:"document,omitempty"`
}

// WhatsAppText represents text message content
type WhatsAppText struct {
	Body string `json:"body"`
}

// WhatsAppTemplate represents template message content
type WhatsAppTemplate struct {
	Name     string                       `json:"name"`
	Language WhatsAppTemplateLanguage     `json:"language"`
	Components []WhatsAppTemplateComponent `json:"components,omitempty"`
}

// WhatsAppTemplateLanguage represents template language
type WhatsAppTemplateLanguage struct {
	Code string `json:"code"`
}

// WhatsAppTemplateComponent represents template components
type WhatsAppTemplateComponent struct {
	Type       string                         `json:"type"`
	Parameters []WhatsAppTemplateParameter    `json:"parameters,omitempty"`
}

// WhatsAppTemplateParameter represents template parameters
type WhatsAppTemplateParameter struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// WhatsAppMedia represents media content
type WhatsAppMedia struct {
	ID       string `json:"id,omitempty"`
	Link     string `json:"link,omitempty"`
	Caption  string `json:"caption,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// WhatsAppResponse represents API response
type WhatsAppResponse struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	Error struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// WhatsAppWebhookEntry represents webhook entry
type WhatsAppWebhookEntry struct {
	ID      string `json:"id"`
	Changes []struct {
		Value struct {
			Messages []struct {
				ID        string `json:"id"`
				From      string `json:"from"`
				Timestamp string `json:"timestamp"`
				Type      string `json:"type"`
				Text      struct {
					Body string `json:"body"`
				} `json:"text"`
			} `json:"messages"`
			Statuses []struct {
				ID           string `json:"id"`
				Status       string `json:"status"`
				Timestamp    string `json:"timestamp"`
				RecipientID  string `json:"recipient_id"`
			} `json:"statuses"`
		} `json:"value"`
		Field string `json:"field"`
	} `json:"changes"`
}

// NewWhatsAppService creates a new WhatsApp service
func NewWhatsAppService(db *gorm.DB) *WhatsAppService {
	return &WhatsAppService{
		db:      db,
		encrypt: encryption.GlobalEncryption,
	}
}

// TestConnection tests the WhatsApp Business API connection
func (w *WhatsAppService) TestConnection(settings *models.WhatsAppSettings) error {
	// Decrypt access token
	accessToken, err := w.encrypt.Decrypt(settings.AccessToken)
	if err != nil {
		return fmt.Errorf("failed to decrypt access token: %w", err)
	}

	// Test by fetching phone number info
	url := fmt.Sprintf("https://graph.facebook.com/%s/%s",
		settings.APIVersion, settings.PhoneNumberID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("WhatsApp API connection test failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WhatsApp API test failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Update connection status
	settings.ConnectionVerified = true
	now := time.Now()
	settings.LastTestedAt = &now

	return nil
}

// SendTextMessage sends a text message via WhatsApp Business API
func (w *WhatsAppService) SendTextMessage(organizationID, customerID, bookingID, toNumber, message string) error {
	// Get integration settings
	var settings models.IntegrationSettings
	if err := w.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		return fmt.Errorf("failed to get integration settings: %w", err)
	}

	if !settings.WhatsAppSettings.Enabled {
		return fmt.Errorf("WhatsApp integration is not enabled")
	}

	// Decrypt access token
	accessToken, err := w.encrypt.Decrypt(settings.WhatsAppSettings.AccessToken)
	if err != nil {
		return fmt.Errorf("failed to decrypt access token: %w", err)
	}

	// Prepare message
	whatsappMsg := WhatsAppMessage{
		MessagingProduct: "whatsapp",
		To:               toNumber,
		Type:             "text",
		Text: &WhatsAppText{
			Body: message,
		},
	}

	// Convert to JSON
	jsonData, err := json.Marshal(whatsappMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Send request
	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/messages",
		settings.WhatsAppSettings.APIVersion, settings.WhatsAppSettings.PhoneNumberID)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		w.LogCommunicationEvent(organizationID, customerID, bookingID, "whatsapp", "whatsapp_business", "outbound",
			"", toNumber, "", "", "", message, "failed", "", err.Error())
		return fmt.Errorf("failed to send WhatsApp message: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var whatsappResp WhatsAppResponse
	if err := json.Unmarshal(body, &whatsappResp); err != nil {
		return fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if resp.StatusCode != http.StatusOK || whatsappResp.Error.Code != 0 {
		errorMsg := fmt.Sprintf("API error: %s (code: %d)", whatsappResp.Error.Message, whatsappResp.Error.Code)
		w.LogCommunicationEvent(organizationID, customerID, bookingID, "whatsapp", "whatsapp_business", "outbound",
			"", toNumber, "", "", "", message, "failed", "", errorMsg)
		return fmt.Errorf("WhatsApp API error: %s", errorMsg)
	}

	// Log successful message
	messageID := ""
	if len(whatsappResp.Messages) > 0 {
		messageID = whatsappResp.Messages[0].ID
	}

	w.LogCommunicationEvent(organizationID, customerID, bookingID, "whatsapp", "whatsapp_business", "outbound",
		"", toNumber, "", "", "", message, "sent", messageID, "")

	return nil
}

// SendTemplateMessage sends a template message via WhatsApp Business API
func (w *WhatsAppService) SendTemplateMessage(organizationID, customerID, bookingID, toNumber, templateName string, parameters []string) error {
	// Get integration settings
	var settings models.IntegrationSettings
	if err := w.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		return fmt.Errorf("failed to get integration settings: %w", err)
	}

	if !settings.WhatsAppSettings.Enabled {
		return fmt.Errorf("WhatsApp integration is not enabled")
	}

	// Decrypt access token
	accessToken, err := w.encrypt.Decrypt(settings.WhatsAppSettings.AccessToken)
	if err != nil {
		return fmt.Errorf("failed to decrypt access token: %w", err)
	}

	// Prepare template components
	var components []WhatsAppTemplateComponent
	if len(parameters) > 0 {
		var templateParams []WhatsAppTemplateParameter
		for _, param := range parameters {
			templateParams = append(templateParams, WhatsAppTemplateParameter{
				Type: "text",
				Text: param,
			})
		}

		components = append(components, WhatsAppTemplateComponent{
			Type:       "body",
			Parameters: templateParams,
		})
	}

	// Prepare message
	whatsappMsg := WhatsAppMessage{
		MessagingProduct: "whatsapp",
		To:               toNumber,
		Type:             "template",
		Template: &WhatsAppTemplate{
			Name: templateName,
			Language: WhatsAppTemplateLanguage{
				Code: "en",
			},
			Components: components,
		},
	}

	// Convert to JSON
	jsonData, err := json.Marshal(whatsappMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal template message: %w", err)
	}

	// Send request
	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/messages",
		settings.WhatsAppSettings.APIVersion, settings.WhatsAppSettings.PhoneNumberID)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		w.LogCommunicationEvent(organizationID, customerID, bookingID, "whatsapp", "whatsapp_business", "outbound",
			"", toNumber, "", "", "", fmt.Sprintf("Template: %s", templateName), "failed", "", err.Error())
		return fmt.Errorf("failed to send WhatsApp template: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var whatsappResp WhatsAppResponse
	if err := json.Unmarshal(body, &whatsappResp); err != nil {
		return fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if resp.StatusCode != http.StatusOK || whatsappResp.Error.Code != 0 {
		errorMsg := fmt.Sprintf("API error: %s (code: %d)", whatsappResp.Error.Message, whatsappResp.Error.Code)
		w.LogCommunicationEvent(organizationID, customerID, bookingID, "whatsapp", "whatsapp_business", "outbound",
			"", toNumber, "", "", "", fmt.Sprintf("Template: %s", templateName), "failed", "", errorMsg)
		return fmt.Errorf("WhatsApp API error: %s", errorMsg)
	}

	// Log successful template message
	messageID := ""
	if len(whatsappResp.Messages) > 0 {
		messageID = whatsappResp.Messages[0].ID
	}

	w.LogCommunicationEvent(organizationID, customerID, bookingID, "whatsapp", "whatsapp_business", "outbound",
		"", toNumber, "", "", "", fmt.Sprintf("Template: %s", templateName), "sent", messageID, "")

	return nil
}

// SendBookingConfirmation sends booking confirmation via WhatsApp
func (w *WhatsAppService) SendBookingConfirmation(organizationID string, booking *models.Booking, customer *models.Customer) error {
	message := fmt.Sprintf(`🎉 *Booking Confirmed!*

📋 *Booking Details:*
• ID: %s
• Date: %s
• Time: %s
• Status: %s

Thank you for choosing our services! We look forward to serving you.

For any questions, please reply to this message.`,
		booking.ID,
		booking.StartTime.Format("January 2, 2006"),
		booking.StartTime.Format("3:04 PM"),
		booking.Status,
	)

	return w.SendTextMessage(organizationID, customer.ID, booking.ID, customer.Phone, message)
}

// SendBookingReminder sends booking reminder via WhatsApp
func (w *WhatsAppService) SendBookingReminder(organizationID string, booking *models.Booking, customer *models.Customer, hoursBeforeBooking int) error {
	message := fmt.Sprintf(`⏰ *Booking Reminder*

📋 *Booking Details:*
• ID: %s
• Date: %s
• Time: %s

⏳ Your appointment is in *%d hours*. Please arrive on time.

For any questions, please reply to this message.`,
		booking.ID,
		booking.StartTime.Format("January 2, 2006"),
		booking.StartTime.Format("3:04 PM"),
		hoursBeforeBooking,
	)

	return w.SendTextMessage(organizationID, customer.ID, booking.ID, customer.Phone, message)
}

// HandleWebhook handles WhatsApp webhook events
func (w *WhatsAppService) HandleWebhook(organizationID string, webhookData []byte) error {
	var webhook struct {
		Entry []WhatsAppWebhookEntry `json:"entry"`
	}

	if err := json.Unmarshal(webhookData, &webhook); err != nil {
		return fmt.Errorf("failed to unmarshal webhook data: %w", err)
	}

	for _, entry := range webhook.Entry {
		for _, change := range entry.Changes {
			// Handle incoming messages
			for _, message := range change.Value.Messages {
				if err := w.handleIncomingMessage(organizationID, message); err != nil {
					log.Printf("Error handling incoming WhatsApp message: %v", err)
				}
			}

			// Handle message status updates
			for _, status := range change.Value.Statuses {
				if err := w.handleMessageStatus(organizationID, status.ID, status.Status); err != nil {
					log.Printf("Error handling WhatsApp message status: %v", err)
				}
			}
		}
	}

	return nil
}

func (w *WhatsAppService) handleIncomingMessage(organizationID string, message struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Text      struct {
		Body string `json:"body"`
	} `json:"text"`
}) error {
	// Log incoming message
	w.LogCommunicationEvent(organizationID, "", "", "whatsapp", "whatsapp_business", "inbound",
		"", message.From, "", "", "", message.Text.Body, "received", message.ID, "")

	// Log integration event
	w.LogIntegrationEvent(organizationID, "webhook_message_received", "success",
		fmt.Sprintf("Received WhatsApp message from %s", message.From), "", "")

	return nil
}

func (w *WhatsAppService) handleMessageStatus(organizationID, messageID, status string) error {
	// Update communication log
	updates := map[string]interface{}{
		"status": status,
	}

	if status == "delivered" {
		now := time.Now()
		updates["delivered_at"] = now
	} else if status == "read" {
		now := time.Now()
		updates["read_at"] = now
	}

	if err := w.db.Model(&models.CommunicationLog{}).
		Where("provider_msg_id = ? AND organization_id = ?", messageID, organizationID).
		Updates(updates).Error; err != nil {
		return fmt.Errorf("failed to update communication log: %w", err)
	}

	// Log integration event
	w.LogIntegrationEvent(organizationID, "webhook_message_status", "success",
		fmt.Sprintf("WhatsApp message status updated to %s for ID %s", status, messageID), "", "")

	return nil
}

// VerifyWebhook verifies WhatsApp webhook signature
func (w *WhatsAppService) VerifyWebhook(settings *models.WhatsAppSettings, mode, token string) (string, error) {
	// Decrypt verify token
	verifyToken, err := w.encrypt.Decrypt(settings.VerifyToken)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt verify token: %w", err)
	}

	if mode == "subscribe" && token == verifyToken {
		// Return the challenge to verify the webhook
		return token, nil
	}

	return "", fmt.Errorf("webhook verification failed")
}

// LogCommunicationEvent logs communication events
func (w *WhatsAppService) LogCommunicationEvent(organizationID, customerID, bookingID, commType, provider, direction,
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

	if err := w.db.Create(logEntry).Error; err != nil {
		log.Printf("Failed to log communication event: %v", err)
	}
}

// LogIntegrationEvent logs integration events
func (w *WhatsAppService) LogIntegrationEvent(organizationID, event, status, message, errorCode, userID string) {
	logEntry := &models.IntegrationLog{
		OrganizationID: organizationID,
		Integration:    "whatsapp",
		Event:          event,
		Status:         status,
		Message:        message,
		ErrorCode:      errorCode,
		UserID:         userID,
	}

	if err := w.db.Create(logEntry).Error; err != nil {
		log.Printf("Failed to log integration event: %v", err)
	}
}

// GetMessageTemplates returns common WhatsApp message templates
func (w *WhatsAppService) GetMessageTemplates() map[string]string {
	return map[string]string{
		"booking_confirmation": `🎉 *Booking Confirmed!*

📋 *Details:*
• ID: {{.BookingID}}
• Date: {{.Date}}
• Time: {{.Time}}

Thank you for choosing our services!`,
		"booking_reminder": `⏰ *Booking Reminder*

📋 *Details:*
• ID: {{.BookingID}}
• Date: {{.Date}}
• Time: {{.Time}}

⏳ Your appointment is in *{{.HoursUntil}} hours*.`,
		"payment_received": `💳 *Payment Received*

✅ Payment confirmed for booking {{.BookingID}}
💰 Amount: ${{.Amount}}

Thank you for your payment!`,
		"service_completed": `✅ *Service Completed*

Thank you for choosing our services!

📋 Booking: {{.BookingID}}
⭐ Please rate your experience by replying with 1-5 stars.`,
	}
}