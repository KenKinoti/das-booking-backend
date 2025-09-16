package messaging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

type WhatsAppService struct {
	BaseURL     string
	AccessToken string
	PhoneID     string
	Client      *http.Client
}

type WhatsAppMessage struct {
	To   string `json:"to"`
	Type string `json:"type"`
	Text struct {
		Body string `json:"body"`
	} `json:"text,omitempty"`
	Template struct {
		Name     string `json:"name"`
		Language struct {
			Code string `json:"code"`
		} `json:"language"`
		Components []interface{} `json:"components,omitempty"`
	} `json:"template,omitempty"`
}

type WhatsAppResponse struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	Error struct {
		Code    int    `json:"code"`
		Title   string `json:"title"`
		Message string `json:"message"`
	} `json:"error"`
}

type WhatsAppWebhookEvent struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"`
		Changes []struct {
			Value struct {
				MessagingProduct string `json:"messaging_product"`
				Metadata         struct {
					DisplayPhoneNumber string `json:"display_phone_number"`
					PhoneNumberID      string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
					WaID string `json:"wa_id"`
				} `json:"contacts"`
				Messages []struct {
					From      string `json:"from"`
					ID        string `json:"id"`
					Timestamp string `json:"timestamp"`
					Text      struct {
						Body string `json:"body"`
					} `json:"text"`
					Type string `json:"type"`
				} `json:"messages"`
				Statuses []struct {
					ID           string `json:"id"`
					Status       string `json:"status"`
					Timestamp    string `json:"timestamp"`
					RecipientID  string `json:"recipient_id"`
					Conversation struct {
						ID     string `json:"id"`
						Origin struct {
							Type string `json:"type"`
						} `json:"origin"`
					} `json:"conversation"`
					Pricing struct {
						Billable     bool   `json:"billable"`
						PricingModel string `json:"pricing_model"`
						Category     string `json:"category"`
					} `json:"pricing"`
				} `json:"statuses"`
			} `json:"value"`
			Field string `json:"field"`
		} `json:"changes"`
	} `json:"entry"`
}

func NewWhatsAppService(settings *models.MessageSettings) *WhatsAppService {
	return &WhatsAppService{
		BaseURL:     "https://graph.facebook.com/v17.0",
		AccessToken: settings.WhatsappAPIKey,
		PhoneID:     settings.WhatsappPhoneNumber,
		Client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (w *WhatsAppService) SendMessage(to, message string) (*WhatsAppResponse, error) {
	// Clean phone number (remove + and ensure it starts with country code)
	to = strings.ReplaceAll(to, "+", "")
	if !strings.HasPrefix(to, "1") && len(to) == 10 {
		to = "1" + to // Add US country code if missing
	}

	whatsappMsg := WhatsAppMessage{
		To:   to,
		Type: "text",
	}
	whatsappMsg.Text.Body = message

	jsonData, err := json.Marshal(whatsappMsg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal message: %v", err)
	}

	url := fmt.Sprintf("%s/%s/messages", w.BaseURL, w.PhoneID)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.AccessToken)

	resp, err := w.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	var whatsappResp WhatsAppResponse
	if err := json.NewDecoder(resp.Body).Decode(&whatsappResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return &whatsappResp, fmt.Errorf("WhatsApp API error: %s", whatsappResp.Error.Message)
	}

	return &whatsappResp, nil
}

func (w *WhatsAppService) SendTemplate(to, templateName, language string, components []interface{}) (*WhatsAppResponse, error) {
	// Clean phone number
	to = strings.ReplaceAll(to, "+", "")
	if !strings.HasPrefix(to, "1") && len(to) == 10 {
		to = "1" + to
	}

	whatsappMsg := WhatsAppMessage{
		To:   to,
		Type: "template",
	}
	whatsappMsg.Template.Name = templateName
	whatsappMsg.Template.Language.Code = language
	whatsappMsg.Template.Components = components

	jsonData, err := json.Marshal(whatsappMsg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal template message: %v", err)
	}

	url := fmt.Sprintf("%s/%s/messages", w.BaseURL, w.PhoneID)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.AccessToken)

	resp, err := w.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	var whatsappResp WhatsAppResponse
	if err := json.NewDecoder(resp.Body).Decode(&whatsappResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return &whatsappResp, fmt.Errorf("WhatsApp API error: %s", whatsappResp.Error.Message)
	}

	return &whatsappResp, nil
}

func (w *WhatsAppService) VerifyWebhook(mode, token, challenge string) (string, error) {
	if mode == "subscribe" && token == w.AccessToken {
		return challenge, nil
	}
	return "", fmt.Errorf("invalid webhook verification")
}

func (w *WhatsAppService) ProcessWebhookEvent(event *WhatsAppWebhookEvent) ([]*models.Message, error) {
	var messages []*models.Message

	for _, entry := range event.Entry {
		for _, change := range entry.Changes {
			if change.Field == "messages" {
				for _, msg := range change.Value.Messages {
					// Create internal message from WhatsApp message
					message := &models.Message{
						Content:     msg.Text.Body,
						MessageType: "text",
						SenderID:    0, // Will be set by handler when thread is found/created
						Metadata:    fmt.Sprintf(`{"external_id": "%s", "phone": "%s", "integration": "whatsapp"}`, msg.ID, msg.From),
					}

					messages = append(messages, message)
				}
			}
		}
	}

	return messages, nil
}

func (w *WhatsAppService) TestConnection() error {
	url := fmt.Sprintf("%s/%s", w.BaseURL, w.PhoneID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create test request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+w.AccessToken)

	resp, err := w.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send test request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("WhatsApp API test failed with status: %d", resp.StatusCode)
	}

	return nil
}