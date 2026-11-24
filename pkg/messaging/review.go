package messaging

// Pending review (pkg/outbox): a WhatsApp message to a client number from
// /messaging/whatsapp/send (a reply typed in a conversation: approvers send
// at once, others wait) or /send-template (waits unless an approver ticks
// "Send now — I've reviewed it").

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
)

type waPayload struct {
	Template   string          `json:"template"`
	Language   string          `json:"language"`
	Components json.RawMessage `json:"components"`
}

// queueWhatsApp answers 202 with the waiting item.
func (h *Handler) queueWhatsApp(c *gin.Context, org, to, text, template, language string, components []interface{}) {
	user := contextString(c, "user_id", "userID")
	it := &outbox.Item{OrganizationID: org, Channel: outbox.ChanWhatsApp, Module: "messaging", To: to, CreatedBy: user}
	if template == "" {
		it.Kind, it.WAMode, it.WAText, it.Message = outbox.KindWhatsAppReply, "text", text, text
		it.Editable = []string{outbox.FieldWAText}
		it.Trigger = ""
	} else {
		raw, _ := json.Marshal(components)
		it.Kind, it.WAMode, it.WATemplate, it.WALanguage = outbox.KindWhatsAppTemplate, "template", template, language
		it.WAParams = templateParams(components)
		it.WAParamCount = len(it.WAParams)
		it.Payload = outbox.Payload(waPayload{Template: template, Language: language, Components: raw})
		it.Editable = []string{}
	}
	q, err := outbox.Enqueue(nil, it)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Couldn't put the message in Pending review: " + err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": gin.H{"queued": true, "review": outbox.QueuedRef(q)}})
}

// templateParams reads the body parameters' text (for the review drawer).
func templateParams(components []interface{}) []string {
	out := []string{}
	for _, c := range components {
		m, ok := c.(map[string]interface{})
		if !ok || !strings.EqualFold(stringOf(m["type"]), "body") {
			continue
		}
		ps, _ := m["parameters"].([]interface{})
		for _, p := range ps {
			if pm, ok := p.(map[string]interface{}); ok {
				out = append(out, stringOf(pm["text"]))
			}
		}
	}
	return out
}

func stringOf(v interface{}) string {
	s, _ := v.(string)
	return s
}

// withParams puts edited body parameters back into the components.
func withParams(components []interface{}, params []string) []interface{} {
	i := 0
	for _, c := range components {
		m, ok := c.(map[string]interface{})
		if !ok || !strings.EqualFold(stringOf(m["type"]), "body") {
			continue
		}
		ps, _ := m["parameters"].([]interface{})
		for _, p := range ps {
			if pm, ok := p.(map[string]interface{}); ok && i < len(params) {
				pm["text"] = params[i]
				i++
			}
		}
	}
	return components
}

type reviewReleaser struct{ h *Handler }

func (h *Handler) registerReview() { outbox.Register("messaging", &reviewReleaser{h: h}) }

func (r *reviewReleaser) service(org string) (*WhatsAppService, error) {
	var settings models.MessageSettings
	if err := r.h.DB.Where("organization_id = ?", org).First(&settings).Error; err != nil {
		return nil, errors.New("WhatsApp isn't set up (Messaging settings)")
	}
	if settings.WhatsappAPIKey == "" || settings.WhatsappPhoneNumber == "" {
		return nil, errors.New("WhatsApp isn't set up (Messaging settings)")
	}
	return NewWhatsAppService(&settings), nil
}

func (r *reviewReleaser) Recheck(ctx context.Context, it *outbox.Item) string { return "" }

func (r *reviewReleaser) Send(ctx context.Context, it *outbox.Item) (outbox.Result, error) {
	svc, err := r.service(it.OrganizationID)
	if err != nil {
		return outbox.Result{}, err
	}
	if it.Kind == outbox.KindWhatsAppReply {
		res, err := svc.SendMessage(it.To, it.WAText)
		if err != nil {
			return outbox.Result{}, err
		}
		return outbox.Result{Text: "Sent to " + it.To + " on WhatsApp", Ref: messageID(res)}, nil
	}
	var p waPayload
	_ = outbox.PayloadOf(it, &p)
	var comps []interface{}
	_ = json.Unmarshal(p.Components, &comps)
	comps = withParams(comps, it.WAParams)
	res, err := svc.SendTemplate(it.To, p.Template, p.Language, comps)
	if err != nil {
		return outbox.Result{}, err
	}
	return outbox.Result{Text: "Sent to " + it.To + " (WhatsApp template " + p.Template + ")", Ref: messageID(res)}, nil
}

func messageID(res *WhatsAppResponse) string {
	if res == nil {
		return ""
	}
	b, _ := json.Marshal(res)
	var m struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(b, &m)
	if len(m.Messages) > 0 {
		return m.Messages[0].ID
	}
	return ""
}

func (r *reviewReleaser) Preview(ctx context.Context, it *outbox.Item) outbox.Preview {
	pv := outbox.Preview{To: it.To, Text: it.WAText}
	if it.Kind == outbox.KindWhatsAppTemplate {
		pv.Text = "Template " + it.WATemplate + ": " + strings.Join(it.WAParams, " · ")
	}
	return pv
}

func (r *reviewReleaser) Closed(ctx context.Context, it *outbox.Item) {}
