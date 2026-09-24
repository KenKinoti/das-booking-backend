package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultModel matches the model used by the meetings summariser.
const DefaultModel = "claude-sonnet-4-5"

// Config is read from the environment.
type Config struct {
	APIKey  string
	Model   string
	BaseURL string
}

// ConfigFromEnv reads ANTHROPIC_API_KEY, ASSISTANT_MODEL / ANTHROPIC_MODEL and
// ANTHROPIC_BASE_URL.
func ConfigFromEnv() Config {
	c := Config{
		APIKey:  strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		Model:   strings.TrimSpace(os.Getenv("ASSISTANT_MODEL")),
		BaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")), "/"),
	}
	if c.Model == "" {
		c.Model = strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://api.anthropic.com"
	}
	return c
}

// apiTool is a tool definition as sent to the Messages API.
type apiTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

type apiMessage struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    []any        `json:"system,omitempty"`
	Messages  []apiMessage `json:"messages"`
	Tools     []apiTool    `json:"tools,omitempty"`
	Stream    bool         `json:"stream"`
}

// Usage is token usage of one API call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// apiResponse is the assembled result of one streamed call.
type apiResponse struct {
	Content    []Block
	StopReason string
	Usage      Usage
}

// APIError is an error from the Anthropic API.
type APIError struct {
	Status  int
	Type    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic %d %s: %s", e.Status, e.Type, e.Message)
}

// Friendly returns a message suitable for end users.
func (e *APIError) Friendly() string {
	switch {
	case e.Status == 401 || e.Type == "authentication_error":
		return "The AI service rejected the API key. Ask your administrator to check ANTHROPIC_API_KEY."
	case e.Status == 403 || e.Type == "permission_error":
		return "The AI service refused this request (permission denied)."
	case e.Status == 429 || e.Type == "rate_limit_error":
		return "The AI service is rate limiting requests right now. Please try again in a moment."
	case e.Status == 529 || e.Type == "overloaded_error":
		return "The AI service is overloaded right now. Please try again in a moment."
	case e.Status == 400 || e.Type == "invalid_request_error":
		return "The AI service could not process this conversation: " + e.Message
	default:
		return "The AI service returned an error. Please try again."
	}
}

// Client talks to the Messages API.
type Client struct {
	Cfg  Config
	HTTP *http.Client
}

func (cl *Client) httpClient() *http.Client {
	if cl.HTTP != nil {
		return cl.HTTP
	}
	return http.DefaultClient
}

// Stream sends req with stream:true, calling onText for each text delta, and
// returns the assembled response. Transient errors before any output are
// retried twice.
func (cl *Client) Stream(ctx context.Context, req apiRequest, onText func(string)) (*apiResponse, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 1200 * time.Millisecond):
			}
		}
		resp, err := cl.do(ctx, body)
		if err != nil {
			lastErr = err
			if ae, ok := err.(*APIError); ok && (ae.Status == 429 || ae.Status == 529 || ae.Status >= 500) {
				continue
			}
			return nil, err
		}
		return cl.read(resp, onText)
	}
	return nil, lastErr
}

func (cl *Client) do(ctx context.Context, body []byte) (*http.Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, cl.Cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("x-api-key", cl.Cfg.APIKey)
	hreq.Header.Set("anthropic-version", "2023-06-01")
	hreq.Header.Set("content-type", "application/json")
	hreq.Header.Set("accept", "text/event-stream")
	resp, err := cl.httpClient().Do(hreq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		ae := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			ae.Type, ae.Message = e.Error.Type, e.Error.Message
		}
		return nil, ae
	}
	return resp, nil
}

// read parses the SSE stream.
func (cl *Client) read(resp *http.Response, onText func(string)) (*apiResponse, error) {
	defer resp.Body.Close()
	// Non-streaming fallback: some proxies return plain JSON.
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		var m struct {
			Content    []Block `json:"content"`
			StopReason string  `json:"stop_reason"`
			Usage      Usage   `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			return nil, err
		}
		for _, b := range m.Content {
			if b.Type == "text" && onText != nil {
				onText(b.Text)
			}
		}
		return &apiResponse{Content: m.Content, StopReason: m.StopReason, Usage: m.Usage}, nil
	}

	out := &apiResponse{}
	type building struct {
		block Block
		json  strings.Builder
	}
	blocks := map[int]*building{}
	var order []int
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var data strings.Builder
	gotStop := false
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		raw := data.String()
		data.Reset()
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage Usage `json:"usage"`
			} `json:"message"`
			ContentBlock Block `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage *Usage `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return nil // ignore malformed lines
		}
		switch ev.Type {
		case "message_start":
			out.Usage.InputTokens = ev.Message.Usage.InputTokens
			out.Usage.OutputTokens = ev.Message.Usage.OutputTokens
		case "content_block_start":
			b := &building{block: ev.ContentBlock}
			if b.block.Type == "tool_use" {
				b.block.Input = nil
			}
			blocks[ev.Index] = b
			order = append(order, ev.Index)
			if b.block.Type == "text" && b.block.Text != "" && onText != nil {
				onText(b.block.Text)
			}
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				return nil
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.block.Text += ev.Delta.Text
				if onText != nil && ev.Delta.Text != "" {
					onText(ev.Delta.Text)
				}
			case "input_json_delta":
				b.json.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				out.StopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				out.Usage.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage != nil && ev.Usage.InputTokens > 0 {
				out.Usage.InputTokens = ev.Usage.InputTokens
			}
		case "message_stop":
			gotStop = true
		case "error":
			return &APIError{Status: 500, Type: ev.Error.Type, Message: ev.Error.Message}
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if !gotStop && out.StopReason == "" {
		return nil, fmt.Errorf("anthropic: stream ended unexpectedly")
	}
	for _, i := range order {
		b := blocks[i]
		switch b.block.Type {
		case "text":
			if b.block.Text != "" {
				out.Content = append(out.Content, Block{Type: "text", Text: b.block.Text})
			}
		case "tool_use":
			in := strings.TrimSpace(b.json.String())
			if in == "" || !json.Valid([]byte(in)) {
				in = "{}"
			}
			out.Content = append(out.Content, Block{Type: "tool_use", ID: b.block.ID, Name: b.block.Name, Input: json.RawMessage(in)})
		}
	}
	return out, nil
}
