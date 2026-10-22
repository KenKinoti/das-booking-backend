package broadcast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AIRequest asks for an AI draft.
type AIRequest struct {
	OccasionKey  string   `json:"occasion_key"`
	OccasionDate string   `json:"occasion_date"`
	OccasionName string   `json:"occasion_name"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	Notes        string   `json:"notes"` // what the message should say
	Swahili      bool     `json:"swahili"`
	Audience     Audience `json:"audience"`
}

// AIResult is an AI draft (always labelled "AI draft" in the composer).
type AIResult struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Swahili bool   `json:"swahili"`
	Model   string `json:"model"`
}

// ErrAIOff: no Anthropic key is configured (System settings → AI assistant).
var ErrAIOff = errors.New("AI drafting needs an Anthropic API key — add one in System settings → AI assistant")

// AIEnabled reports whether "Write with AI" can be offered.
func (s *Service) AIEnabled() bool { return s.aiConfig().APIKey != "" }

func (s *Service) aiPrompt(in AIRequest) string {
	var b strings.Builder
	b.WriteString("You write short announcements that the DASYIN team sends to the people who use DASYIN ERP — a business management system ")
	b.WriteString("(bookings, point of sale, invoicing, stock, staff) used by small and medium businesses, mostly in Kenya and Australia.\n")
	b.WriteString("Voice: warm, plain and practical; no hype, no exclamation-mark spam, no emojis; British/Kenyan English spelling.\n\n")
	if in.OccasionName != "" {
		b.WriteString("Occasion: " + in.OccasionName)
		if in.OccasionDate != "" {
			if d, err := time.Parse(DateLayout, in.OccasionDate); err == nil {
				b.WriteString(" on " + d.Format("Monday 2 January 2006"))
			}
		}
		b.WriteString(".\n")
	}
	b.WriteString("Kind of message: " + KindLabel(in.Kind) + ".")
	switch in.Kind {
	case KindPromotion:
		b.WriteString(" It is an offer: one clear benefit and a gentle invitation to upgrade or try a module; no pressure.")
	case KindReminder:
		b.WriteString(" It is a reminder: greet them, then the practical things to check.")
	case KindUrgent:
		b.WriteString(" It is an urgent service notice: say what happens, when, and what they need to do.")
	}
	b.WriteString("\n")
	if w := countriesText(in.Audience.Countries); w != "" {
		b.WriteString("Readers: businesses in " + w + ".\n")
	}
	switch in.Audience.Normalize().RoleLevel {
	case LevelOwners:
		b.WriteString("Readers are business owners and admins.\n")
	case LevelManagers:
		b.WriteString("Readers are owners and managers.\n")
	}
	if t := strings.TrimSpace(in.Title); t != "" {
		b.WriteString("Working title: " + clip(t, 140) + "\n")
	}
	if n := strings.TrimSpace(in.Notes); n != "" {
		b.WriteString("What it should say: " + clip(n, 1200) + "\n")
	}
	b.WriteString("\nAt most 110 words: one or two short paragraphs, and at most one bullet list of up to 3 items. ")
	b.WriteString("Use only <p>, <strong>, <ul> and <li> tags.\n")
	b.WriteString(`Return ONLY a JSON object: {"title": "at most 70 characters", "body_html": "<p>…</p>"`)
	if in.Swahili {
		b.WriteString(`, "body_sw_html": "the same message in natural Kiswahili, same tags"`)
	}
	b.WriteString("}.")
	return b.String()
}

// WriteWithAI drafts a message with the in-app Anthropic key.
func (s *Service) WriteWithAI(ctx context.Context, in AIRequest) (AIResult, error) {
	cfg := s.aiConfig()
	if cfg.APIKey == "" {
		return AIResult{}, ErrAIOff
	}
	body, _ := json.Marshal(map[string]any{
		"model": cfg.Model, "max_tokens": 1200,
		"messages": []map[string]string{{"role": "user", "content": s.aiPrompt(in)}},
	})
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return AIResult{}, err
	}
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return AIResult{}, fmt.Errorf("could not reach the AI service: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return AIResult{}, fmt.Errorf("the AI service answered %d — try again in a moment", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return AIResult{}, errors.New("the AI service sent an answer we could not read")
	}
	var txt strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			txt.WriteString(c.Text)
		}
	}
	return parseAIDraft(txt.String(), in, cfg.Model)
}

// parseAIDraft reads the model's JSON (or, failing that, its text).
func parseAIDraft(text string, in AIRequest, model string) (AIResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return AIResult{}, errors.New("the AI service returned an empty draft")
	}
	res := AIResult{Model: model}
	var j struct {
		Title    string `json:"title"`
		BodyHTML string `json:"body_html"`
		BodySW   string `json:"body_sw_html"`
	}
	if a, b := strings.Index(text, "{"), strings.LastIndex(text, "}"); a >= 0 && b > a && json.Unmarshal([]byte(text[a:b+1]), &j) == nil && strings.TrimSpace(j.BodyHTML) != "" {
		res.Title = clip(strings.TrimSpace(j.Title), 140)
		body := j.BodyHTML
		if in.Swahili && strings.TrimSpace(j.BodySW) != "" {
			body += "<p><strong>Kwa Kiswahili</strong></p>" + j.BodySW
			res.Swahili = true
		}
		res.Body, _ = Sanitize(body)
	} else {
		res.Body, _ = Sanitize(text)
	}
	if res.Title == "" {
		res.Title = strings.TrimSpace(in.Title)
		if res.Title == "" {
			res.Title = in.OccasionName
		}
		if res.Title == "" {
			res.Title = "A message from DASYIN"
		}
	}
	if strings.TrimSpace(PlainText(res.Body, false, "")) == "" {
		return AIResult{}, errors.New("the AI draft was empty — try again")
	}
	return res, nil
}
