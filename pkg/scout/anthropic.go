package scout

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

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/assistant"
)

// The Messages API with the server-side web search tool.
//
// Docs: https://platform.claude.com/docs/en/agents-and-tools/tool-use/web-search-tool
//
//   - Tool: {"type": "web_search_20250305", "name": "web_search", "max_uses": N,
//     "allowed_domains" | "blocked_domains" (one or the other, never both — a
//     request with both is a 400; entries are bare domains with an optional
//     path, no scheme), "user_location": {"type": "approximate", "city",
//     "region", "country" (ISO 3166-1 alpha-2; unsupported codes are a 400),
//     "timezone" (IANA)}}. web_search_20250305 is the basic version: the
//     newer web_search_20260209 / _20260318 default allowed_callers to code
//     execution ("dynamic filtering"), which would enable a second tool — the
//     scout deliberately enables only web search.
//   - The answer's content mixes "text", "server_tool_use" ({id, name:
//     "web_search", input: {query}}) and "web_search_tool_result" ({tool_use_id,
//     content: [{type: "web_search_result", url, title, encrypted_content,
//     page_age}]} — or, on an error, content is ONE object {type:
//     "web_search_tool_result_error", error_code: too_many_requests |
//     invalid_tool_input | max_uses_exceeded | query_too_long |
//     request_too_large | unavailable}, still HTTP 200).
//   - Text blocks carry citations [{type: "web_search_result_location", url,
//     title, encrypted_index, cited_text}].
//   - usage.server_tool_use.web_search_requests counts the searches billed.
//   - stop_reason "pause_turn": a long turn was paused; send the assistant
//     content back unchanged to continue (searches already run count against
//     max_uses).
//   - If web search is disabled for the Anthropic organisation, the request
//     fails with a 400 invalid_request_error saying so.

// WebSearchToolType is the tool version the scout uses.
const WebSearchToolType = "web_search_20250305"

// AnthropicVersion is the API version header.
const AnthropicVersion = "2023-06-01"

// MaxContinuations is how many times a paused turn is resumed.
const MaxContinuations = 2

type userLocation struct {
	Type     string `json:"type"`
	City     string `json:"city,omitempty"`
	Region   string `json:"region,omitempty"`
	Country  string `json:"country,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

type webSearchTool struct {
	Type           string        `json:"type"`
	Name           string        `json:"name"`
	MaxUses        int           `json:"max_uses,omitempty"`
	AllowedDomains []string      `json:"allowed_domains,omitempty"`
	BlockedDomains []string      `json:"blocked_domains,omitempty"`
	UserLocation   *userLocation `json:"user_location,omitempty"`
}

type apiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type apiRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system"`
	Messages  []apiMessage    `json:"messages"`
	Tools     []webSearchTool `json:"tools"`
}

type apiUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	ServerToolUse            struct {
		WebSearchRequests int `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

type apiResponse struct {
	ID         string          `json:"id"`
	Content    json.RawMessage `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      apiUsage        `json:"usage"`
}

// block is one content block of an answer (only the fields the scout reads).
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Citations []citation      `json:"citations,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type citation struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	CitedText string `json:"cited_text"`
}

type searchResult struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	PageAge string `json:"page_age"`
}

// APIError is an error answer of the Messages API.
type APIError struct {
	Status  int
	Type    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic %d %s: %s", e.Status, e.Type, e.Message)
}

// Friendly is a message for the owner.
func (e *APIError) Friendly() string {
	m := strings.ToLower(e.Message)
	switch {
	case e.Status == 401 || e.Type == "authentication_error":
		return "Anthropic rejected the API key. Check the key in System settings → AI assistant."
	case e.Status == 402 || e.Type == "billing_error" || strings.Contains(m, "credit balance") || strings.Contains(m, "purchase credits"):
		return "The Anthropic account has no credit left. Add credit at console.anthropic.com → Billing."
	case strings.Contains(m, "web search") && (strings.Contains(m, "not enabled") || strings.Contains(m, "disabled")):
		return "Web search is switched off for your Anthropic organisation. An Anthropic Console admin can switch it on (Settings → Privacy / capabilities)."
	case e.Status == 404 || e.Type == "not_found_error":
		return "The AI model in System settings → AI assistant was not found. Choose another model there."
	case e.Status == 429 || e.Type == "rate_limit_error":
		return "Anthropic is rate limiting requests right now. Try again in a few minutes."
	case e.Status == 529 || e.Type == "overloaded_error":
		return "Anthropic is overloaded right now. Try again in a few minutes."
	case e.Status == 400 || e.Type == "invalid_request_error":
		return "Anthropic could not process the scan: " + e.Message
	}
	return "Anthropic returned an error. Try again later."
}

// Friendly turns any scan error into a message for the owner.
func Friendly(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Friendly()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "The scan took too long and was stopped. Try again, or lower the number of searches."
	}
	return "The scan failed: " + err.Error()
}

// client calls the Messages API (non-streaming).
type client struct {
	cfg  assistant.Config
	http *http.Client
}

func (cl *client) post(ctx context.Context, req apiRequest) (*apiResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		res, err := cl.once(ctx, body)
		if err == nil {
			return res, nil
		}
		last = err
		var ae *APIError
		if errors.As(err, &ae) && (ae.Status == 429 || ae.Status == 529 || ae.Status >= 500) {
			continue
		}
		return nil, err
	}
	return nil, last
}

func (cl *client) once(ctx context.Context, body []byte) (*apiResponse, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cl.cfg.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("x-api-key", cl.cfg.APIKey)
	hreq.Header.Set("anthropic-version", AnthropicVersion)
	hreq.Header.Set("content-type", "application/json")
	hc := cl.http
	if hc == nil {
		hc = &http.Client{Timeout: 6 * time.Minute}
	}
	resp, err := hc.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
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
		if len(ae.Message) > 400 {
			ae.Message = ae.Message[:400]
		}
		return nil, ae
	}
	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unreadable answer from the AI service: %w", err)
	}
	return &out, nil
}

// Exchange is everything one scan received.
type Exchange struct {
	Usage       Usage
	Requests    int
	StopReason  string
	Queries     []string
	Results     map[string]SourceRef // normalised URL → the result as the API returned it
	Cited       map[string]bool      // normalised URLs cited in text blocks
	Text        string               // the final answer (text after the last search result)
	AllText     string               // every text block
	SearchErrs  []string
	NoLocation  bool // retried without user_location after a 400
	Interrupted string
}

// converse runs one scan turn: the request, then up to MaxContinuations
// resumes after pause_turn. budgetLeft (US$) is checked before each resume.
func (cl *client) converse(ctx context.Context, req apiRequest, model string, budgetLeft float64) (*Exchange, error) {
	ex := &Exchange{Results: map[string]SourceRef{}, Cited: map[string]bool{}}
	userMsg := req.Messages
	var assistantContent []json.RawMessage
	for i := 0; i <= MaxContinuations; i++ {
		r := req
		if len(assistantContent) > 0 {
			joined, _ := json.Marshal(assistantContent)
			r.Messages = append(append([]apiMessage{}, userMsg...), apiMessage{Role: "assistant", Content: joined})
		}
		res, err := cl.post(ctx, r)
		if err != nil {
			var ae *APIError
			// Unsupported user_location (e.g. a country code the API does not
			// know) is a 400: retry once without it.
			if errors.As(err, &ae) && ae.Status == 400 && strings.Contains(strings.ToLower(ae.Message), "user_location") && len(req.Tools) > 0 && req.Tools[0].UserLocation != nil && !ex.NoLocation {
				ex.NoLocation = true
				req.Tools = append([]webSearchTool{}, req.Tools...)
				req.Tools[0].UserLocation = nil
				i--
				continue
			}
			if ex.Requests > 0 {
				ex.Interrupted = Friendly(err)
				break
			}
			return ex, err
		}
		ex.Requests++
		ex.StopReason = res.StopReason
		ex.Usage.Input += res.Usage.InputTokens
		ex.Usage.Output += res.Usage.OutputTokens
		ex.Usage.CacheWrite += res.Usage.CacheCreationInputTokens
		ex.Usage.CacheRead += res.Usage.CacheReadInputTokens
		ex.Usage.Searches += res.Usage.ServerToolUse.WebSearchRequests
		var raws []json.RawMessage
		if len(res.Content) > 0 {
			if err := json.Unmarshal(res.Content, &raws); err != nil {
				return ex, fmt.Errorf("unreadable answer content: %w", err)
			}
		}
		assistantContent = append(assistantContent, raws...)
		if res.StopReason != "pause_turn" {
			break
		}
		if i == MaxContinuations {
			ex.Interrupted = "The AI service paused the scan more often than allowed; the answer may be incomplete."
			break
		}
		// Resume only if the worst case of another round still fits: the
		// whole conversation is read again, plus the searches not used yet.
		_, _, spent := Cost(model, ex.Usage)
		left := req.Tools[0].MaxUses - ex.Usage.Searches
		if left < 0 {
			left = 0
		}
		_, _, next := Cost(model, Usage{Input: res.Usage.InputTokens + res.Usage.OutputTokens + left*TokensPerSearchWorst, Output: req.MaxTokens, Searches: left})
		if spent+next > budgetLeft {
			ex.Interrupted = "The scan was paused by the AI service and not resumed, to stay within the monthly budget."
			break
		}
	}
	ex.read(assistantContent)
	return ex, nil
}

// read collects queries, results, citations and the answer text.
func (ex *Exchange) read(content []json.RawMessage) {
	var all, after strings.Builder
	for _, raw := range content {
		var b block
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "server_tool_use":
			var in struct {
				Query string `json:"query"`
			}
			if json.Unmarshal(b.Input, &in) == nil && strings.TrimSpace(in.Query) != "" {
				ex.Queries = append(ex.Queries, clip(in.Query, 300))
			}
		case "web_search_tool_result":
			after.Reset()
			var list []searchResult
			if json.Unmarshal(b.Content, &list) == nil {
				for _, r := range list {
					if r.Type != "" && r.Type != "web_search_result" {
						continue
					}
					if n := NormURL(r.URL); n != "" {
						if _, seen := ex.Results[n]; !seen {
							ex.Results[n] = SourceRef{Title: clip(cleanText(r.Title), 200), URL: r.URL, Domain: domainOf(r.URL), PageAge: clip(r.PageAge, 40)}
						}
					}
				}
				continue
			}
			var e struct {
				Type      string `json:"type"`
				ErrorCode string `json:"error_code"`
			}
			if json.Unmarshal(b.Content, &e) == nil && e.ErrorCode != "" {
				ex.SearchErrs = append(ex.SearchErrs, e.ErrorCode)
			}
		case "text":
			all.WriteString(b.Text)
			after.WriteString(b.Text)
			for _, c := range b.Citations {
				if n := NormURL(c.URL); n != "" {
					ex.Cited[n] = true
				}
			}
		}
	}
	ex.AllText = all.String()
	ex.Text = after.String()
}
