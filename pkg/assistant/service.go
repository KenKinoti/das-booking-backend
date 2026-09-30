package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

// Emit sends one server-sent event to the browser.
type Emit func(event string, data any)

// Page is what the user is looking at when they send a message.
type Page struct {
	Path   string            `json:"path"`
	Name   string            `json:"name"`
	Title  string            `json:"title"`
	Params map[string]string `json:"params"`
	Query  map[string]string `json:"query"`
}

// Service runs conversations.
type Service struct {
	DB        *gorm.DB
	Cfg       Config
	Client    *Client
	ListTools func() []aitools.Tool
	GetTool   func(string) (aitools.Tool, bool)
	Now       func() time.Time
	MaxSteps  int
	ToolTTL   time.Duration

	locks   sync.Map // conversation id -> *sync.Mutex
	limiter *rateLimiter
}

// NewService builds a service from the environment.
func NewService(db *gorm.DB) *Service {
	cfg := ConfigFromEnv()
	s := &Service{DB: db, Cfg: cfg}
	if cfg.APIKey != "" {
		s.Client = &Client{Cfg: cfg}
	}
	return s
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) tools() []aitools.Tool {
	if s.ListTools != nil {
		return s.ListTools()
	}
	return aitools.All()
}

func (s *Service) tool(name string) (aitools.Tool, bool) {
	if s.GetTool != nil {
		return s.GetTool(name)
	}
	return aitools.Get(name)
}

func (s *Service) maxSteps() int {
	if s.MaxSteps > 0 {
		return s.MaxSteps
	}
	return 8
}

// Enabled reports whether an API key is configured.
func (s *Service) Enabled() bool { return s.Client != nil && s.Client.Cfg.APIKey != "" }

// ErrBusy is returned when the conversation is already running.
var ErrBusy = errors.New("this conversation is already working on a reply")

// ErrNotFound is returned for unknown or foreign conversations.
var ErrNotFound = errors.New("conversation not found")

func (s *Service) lock(id string) (func(), bool) {
	v, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	m := v.(*sync.Mutex)
	if !m.TryLock() {
		return nil, false
	}
	return m.Unlock, true
}

// getConversation loads a conversation owned by the user.
func (s *Service) getConversation(who aitools.Identity, id string) (*Conversation, error) {
	var c Conversation
	err := s.DB.Where("id = ? AND org_id = ? AND user_id = ?", id, who.OrgID, who.UserID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Service) messages(convID string) ([]Message, error) {
	var ms []Message
	err := s.DB.Where("conversation_id = ?", convID).Order("seq asc, created_at asc").Find(&ms).Error
	return ms, err
}

func (s *Service) nextSeq(convID string) int {
	var n int
	s.DB.Model(&Message{}).Where("conversation_id = ?", convID).Select("COALESCE(MAX(seq),0)").Scan(&n)
	return n + 1
}

func (s *Service) addMessage(conv *Conversation, role string, blocks []Block, meta map[string]ToolMeta, usage Usage, model string) (*Message, error) {
	content, _ := json.Marshal(blocks)
	m := &Message{
		ID: uuid.NewString(), ConversationID: conv.ID, OrgID: conv.OrgID, UserID: conv.UserID,
		Seq: s.nextSeq(conv.ID), Role: role, Content: string(content), Model: model,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CreatedAt: s.now(),
	}
	if len(meta) > 0 {
		b, _ := json.Marshal(meta)
		m.Meta = string(b)
	}
	if err := s.DB.Create(m).Error; err != nil {
		return nil, err
	}
	conv.LastMessageAt = m.CreatedAt
	s.DB.Model(&Conversation{}).Where("id = ?", conv.ID).Updates(map[string]any{"last_message_at": m.CreatedAt, "updated_at": s.now()})
	if usage.InputTokens+usage.OutputTokens > 0 {
		log.Printf("assistant usage: org=%s user=%s conv=%s model=%s input_tokens=%d output_tokens=%d", conv.OrgID, conv.UserID, conv.ID, model, usage.InputTokens, usage.OutputTokens)
	}
	return m, nil
}

func (s *Service) setPending(conv *Conversation, p *pendingState) error {
	val := ""
	if p != nil {
		b, _ := json.Marshal(p)
		val = string(b)
	}
	conv.Pending = val
	return s.DB.Model(&Conversation{}).Where("id = ?", conv.ID).Update("pending", val).Error
}

func (conv *Conversation) pending() *pendingState {
	if strings.TrimSpace(conv.Pending) == "" {
		return nil
	}
	var p pendingState
	if json.Unmarshal([]byte(conv.Pending), &p) != nil || len(p.Queue) == 0 {
		return nil
	}
	return &p
}

// autoTitle makes a short title from the first user message.
func autoTitle(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(t) <= 60 {
		return t
	}
	r := []rune(t)[:60]
	s := string(r)
	if i := strings.LastIndex(s, " "); i > 30 {
		s = s[:i]
	}
	return strings.TrimRight(s, " ,.;:-") + "…"
}

// ChatInput is one user turn.
type ChatInput struct {
	ConversationID string
	Text           string
	Retry          bool
	Page           Page
}

// Chat appends the user's message (or retries the last one) and runs the loop.
func (s *Service) Chat(ctx context.Context, who aitools.Identity, in ChatInput, emit Emit) error {
	var conv *Conversation
	if in.ConversationID != "" {
		c, err := s.getConversation(who, in.ConversationID)
		if err != nil {
			return err
		}
		conv = c
	} else {
		if in.Retry {
			return fmt.Errorf("nothing to retry")
		}
		now := s.now()
		conv = &Conversation{ID: uuid.NewString(), OrgID: who.OrgID, UserID: who.UserID, Title: autoTitle(in.Text), CreatedAt: now, UpdatedAt: now, LastMessageAt: now}
		if conv.Title == "" {
			conv.Title = "New conversation"
		}
		if err := s.DB.Create(conv).Error; err != nil {
			return err
		}
	}
	unlock, ok := s.lock(conv.ID)
	if !ok {
		return ErrBusy
	}
	defer unlock()
	emit("conversation", map[string]any{"id": conv.ID, "title": conv.Title})

	if in.Retry {
		ms, err := s.messages(conv.ID)
		if err != nil {
			return err
		}
		if len(ms) == 0 || ms[len(ms)-1].Role != "user" || conv.pending() != nil {
			return fmt.Errorf("nothing to retry")
		}
	} else {
		text := strings.TrimSpace(in.Text)
		if text == "" {
			return fmt.Errorf("message is empty")
		}
		var blocks []Block
		var meta map[string]ToolMeta
		// A new message while an action is awaiting confirmation declines it.
		if p := conv.pending(); p != nil {
			meta = p.Meta
			if meta == nil {
				meta = map[string]ToolMeta{}
			}
			results := p.Results
			for _, q := range p.Queue {
				results = append(results, Block{Type: "tool_result", ToolUseID: q.ID, Content: "Not performed: the user sent a new message instead of confirming this action.", IsError: true})
				t, _ := s.tool(q.Name)
				meta[q.ID] = ToolMeta{Name: q.Name, Title: toolTitle(t, q.Name), Status: "declined", Summary: "Skipped " + lowerFirst(toolTitle(t, q.Name))}
				emit("tool_result", toolEvent(q.ID, meta[q.ID]))
			}
			blocks = append(blocks, s.orderResults(p.MessageID, results)...)
			if err := s.setPending(conv, nil); err != nil {
				return err
			}
		}
		blocks = append(blocks, Block{Type: "text", Text: text})
		if _, err := s.addMessage(conv, "user", blocks, meta, Usage{}, ""); err != nil {
			return err
		}
		if conv.Title == "" || conv.Title == "New conversation" {
			conv.Title = autoTitle(text)
			s.DB.Model(&Conversation{}).Where("id = ?", conv.ID).Update("title", conv.Title)
		}
	}
	return s.loop(ctx, conv, who, in.Page, emit)
}

// Resolve confirms or cancels the pending action and resumes the loop.
func (s *Service) Resolve(ctx context.Context, who aitools.Identity, convID, actionID string, approve bool, page Page, emit Emit) error {
	conv, err := s.getConversation(who, convID)
	if err != nil {
		return err
	}
	unlock, ok := s.lock(conv.ID)
	if !ok {
		return ErrBusy
	}
	defer unlock()
	// Re-read under the lock.
	if conv, err = s.getConversation(who, convID); err != nil {
		return err
	}
	p := conv.pending()
	if p == nil || p.Queue[0].ID != actionID {
		return fmt.Errorf("this action is no longer waiting for confirmation")
	}
	emit("conversation", map[string]any{"id": conv.ID, "title": conv.Title})
	if p.Meta == nil {
		p.Meta = map[string]ToolMeta{}
	}
	q := p.Queue[0]
	p.Queue = p.Queue[1:]
	t, known := s.tool(q.Name)
	if approve && known {
		res, meta := s.execute(ctx, who, t, q, true, emit)
		p.Results = append(p.Results, res)
		p.Meta[q.ID] = meta
	} else {
		p.Results = append(p.Results, Block{Type: "tool_result", ToolUseID: q.ID, Content: "The user declined this action. It was NOT performed. Acknowledge briefly and ask if they want something different.", IsError: true})
		m := ToolMeta{Name: q.Name, Title: toolTitle(t, q.Name), Status: "declined", Summary: "Cancelled: " + lowerFirst(toolTitle(t, q.Name))}
		p.Meta[q.ID] = m
		emit("tool_result", toolEvent(q.ID, m))
	}
	if len(p.Queue) > 0 {
		if err := s.setPending(conv, p); err != nil {
			return err
		}
		s.emitPending(conv, p, emit)
		emit("done", map[string]any{"paused": true})
		return nil
	}
	if _, err := s.addMessage(conv, "user", s.orderResults(p.MessageID, p.Results), p.Meta, Usage{}, ""); err != nil {
		return err
	}
	if err := s.setPending(conv, nil); err != nil {
		return err
	}
	return s.loop(ctx, conv, who, page, emit)
}

// orderResults sorts tool_result blocks into the order of the tool_use
// blocks in the assistant message.
func (s *Service) orderResults(msgID string, results []Block) []Block {
	var m Message
	if s.DB.Where("id = ?", msgID).First(&m).Error != nil {
		return results
	}
	var blocks []Block
	json.Unmarshal([]byte(m.Content), &blocks)
	pos := map[string]int{}
	for i, b := range blocks {
		pos[b.ID] = i
	}
	out := append([]Block(nil), results...)
	sort.SliceStable(out, func(i, j int) bool { return pos[out[i].ToolUseID] < pos[out[j].ToolUseID] })
	return out
}

func (s *Service) autoApprove(who aitools.Identity) bool {
	var st UserSettings
	if s.DB.Where("user_id = ?", who.UserID).First(&st).Error != nil {
		return false
	}
	return st.AutoApprove
}

// needsConfirm decides whether a tool call must wait for the user.
func needsConfirm(t aitools.Tool, auto bool) bool {
	if t.ReadOnly {
		return false
	}
	if t.Destructive {
		return true
	}
	return !auto
}

// loop runs model → tools → model until the model stops, a confirmation is
// needed, or the step budget is used up.
func (s *Service) loop(ctx context.Context, conv *Conversation, who aitools.Identity, page Page, emit Emit) error {
	if !s.Enabled() {
		return fmt.Errorf("the assistant is not configured (ANTHROPIC_API_KEY is not set)")
	}
	auto := s.autoApprove(who)
	sys := s.systemPrompt(who, page)
	tools := s.apiTools()
	var total Usage
	for step := 0; step < s.maxSteps(); step++ {
		ms, err := s.messages(conv.ID)
		if err != nil {
			return err
		}
		req := apiRequest{Model: s.Cfg.Model, MaxTokens: 2048, System: sys, Messages: buildHistory(ms), Tools: tools}
		var partial strings.Builder
		emit("step", map[string]any{"n": step + 1})
		resp, err := s.Client.Stream(ctx, req, func(d string) {
			partial.WriteString(d)
			emit("text", map[string]any{"delta": d})
		})
		if err != nil {
			if ctx.Err() != nil {
				// Stopped by the user: keep what was already shown.
				if t := strings.TrimSpace(partial.String()); t != "" {
					s.addMessage(conv, "assistant", []Block{{Type: "text", Text: t + " …"}}, nil, Usage{}, s.Cfg.Model)
				}
				return ctx.Err()
			}
			return err
		}
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		content := resp.Content
		if len(content) == 0 {
			content = []Block{{Type: "text", Text: "(No response.)"}}
		}
		am, err := s.addMessage(conv, "assistant", content, nil, resp.Usage, s.Cfg.Model)
		if err != nil {
			return err
		}
		var uses []Block
		for _, b := range content {
			if b.Type == "tool_use" {
				uses = append(uses, b)
			}
		}
		if len(uses) == 0 {
			emit("done", map[string]any{"usage": total, "stop_reason": resp.StopReason})
			return nil
		}
		var results []Block
		meta := map[string]ToolMeta{}
		var queue []Block
		for _, u := range uses {
			t, ok := s.tool(u.Name)
			if !ok {
				results = append(results, Block{Type: "tool_result", ToolUseID: u.ID, Content: "Unknown tool: " + u.Name, IsError: true})
				meta[u.ID] = ToolMeta{Name: u.Name, Title: u.Name, Status: "error", Summary: "Unknown tool " + u.Name}
				emit("tool_result", toolEvent(u.ID, meta[u.ID]))
				continue
			}
			if needsConfirm(t, auto) {
				queue = append(queue, u)
				continue
			}
			res, m := s.execute(ctx, who, t, u, false, emit)
			results = append(results, res)
			meta[u.ID] = m
		}
		if len(queue) > 0 {
			p := &pendingState{MessageID: am.ID, Results: results, Meta: meta, Queue: queue}
			if err := s.setPending(conv, p); err != nil {
				return err
			}
			s.emitPending(conv, p, emit)
			emit("done", map[string]any{"usage": total, "paused": true})
			return nil
		}
		if _, err := s.addMessage(conv, "user", results, meta, Usage{}, ""); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	msg := fmt.Sprintf("I stopped after %d steps to keep things quick. Say \"continue\" if you'd like me to keep going.", s.maxSteps())
	emit("text", map[string]any{"delta": msg})
	s.addMessage(conv, "assistant", []Block{{Type: "text", Text: msg}}, nil, Usage{}, s.Cfg.Model)
	emit("done", map[string]any{"usage": total, "stop_reason": "max_steps"})
	return nil
}

func (s *Service) apiTools() []apiTool {
	all := s.tools()
	out := make([]apiTool, 0, len(all))
	for _, t := range all {
		desc := t.Description
		if !t.ReadOnly {
			if t.Destructive {
				desc += " [Destructive: the user will be asked to confirm before it runs.]"
			} else {
				desc += " [Changes data: the user may be asked to confirm before it runs.]"
			}
		}
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, apiTool{Name: t.Name, Description: desc, InputSchema: schema})
	}
	if len(out) > 0 {
		out[len(out)-1].CacheControl = map[string]any{"type": "ephemeral"}
	}
	return out
}

func (s *Service) emitPending(conv *Conversation, p *pendingState, emit Emit) {
	q := p.Queue[0]
	t, _ := s.tool(q.Name)
	emit("pending_action", describeAction(conv.ID, t, q))
}

// execute runs one tool. Writes run detached from the request context so a
// closed browser tab cannot leave a half-finished change unrecorded.
func (s *Service) execute(ctx context.Context, who aitools.Identity, t aitools.Tool, u Block, confirmed bool, emit Emit) (res Block, meta ToolMeta) {
	emit("tool_start", map[string]any{"id": u.ID, "name": t.Name, "title": toolTitle(t, t.Name), "label": runningLabel(t), "write": !t.ReadOnly})
	var args map[string]any
	if len(u.Input) > 0 {
		json.Unmarshal(u.Input, &args)
	}
	if args == nil {
		args = map[string]any{}
	}
	ttl := s.ToolTTL
	if ttl == 0 {
		ttl = 60 * time.Second
	}
	base := ctx
	if !t.ReadOnly {
		base = context.WithoutCancel(ctx)
	}
	tctx, cancel := context.WithTimeout(base, ttl)
	defer cancel()
	out, err := func() (out any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("tool failed unexpectedly: %v", r)
			}
		}()
		return t.Handler(&aitools.Ctx{Context: tctx, Who: who}, args)
	}()
	if err != nil {
		msg := err.Error()
		var ae *aitools.APIError
		if errors.As(err, &ae) {
			msg = fmt.Sprintf("%s (HTTP %d)", ae.Message, ae.Status)
		}
		meta = ToolMeta{Name: t.Name, Title: toolTitle(t, t.Name), Status: "error", Summary: toolTitle(t, t.Name) + " failed: " + truncate(msg, 160), Write: !t.ReadOnly}
		emit("tool_result", toolEvent(u.ID, meta))
		return Block{Type: "tool_result", ToolUseID: u.ID, Content: "Error: " + msg, IsError: true}, meta
	}
	b, _ := json.Marshal(out)
	txt := string(b)
	if len(txt) > 24000 {
		txt = txt[:24000] + "…(truncated — narrow the query for more)"
	}
	// Normalise named map types (gin.H, structs) for the summary helpers.
	var norm any
	if json.Unmarshal(b, &norm) != nil {
		norm = out
	}
	meta = resultMeta(t, args, norm)
	if !t.ReadOnly {
		s.notifyWrite(who, t, meta)
	}
	emit("tool_result", toolEvent(u.ID, meta))
	return Block{Type: "tool_result", ToolUseID: u.ID, Content: txt}, meta
}

func toolEvent(id string, m ToolMeta) map[string]any {
	return map[string]any{"id": id, "name": m.Name, "title": m.Title, "status": m.Status, "summary": m.Summary, "link": m.Link, "changed": m.Changed, "write": m.Write}
}

// buildHistory converts stored messages into a valid Messages API history:
// same-role messages are merged, every tool_use gets a tool_result, orphan
// tool_results are dropped and long histories are trimmed at a clean user turn.
func buildHistory(ms []Message) []apiMessage {
	var out []apiMessage
	for _, m := range ms {
		var blocks []Block
		if json.Unmarshal([]byte(m.Content), &blocks) != nil || len(blocks) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}
		out = append(out, apiMessage{Role: m.Role, Content: blocks})
	}
	// Trim to the last ~40 messages, starting at a user message with text.
	const keep = 40
	if len(out) > keep {
		start := len(out) - keep
		for start < len(out) && !(out[start].Role == "user" && !hasToolResult(out[start])) {
			start++
		}
		if start < len(out) {
			out = out[start:]
		}
	}
	for len(out) > 0 && out[0].Role != "user" {
		out = out[1:]
	}
	// Repair tool_use / tool_result pairing.
	for i := 0; i < len(out); i++ {
		if out[i].Role == "user" {
			var prev map[string]bool
			if i > 0 {
				prev = toolUseIDs(out[i-1])
			}
			var kept []Block
			for _, b := range out[i].Content {
				if b.Type == "tool_result" && !prev[b.ToolUseID] {
					continue
				}
				kept = append(kept, b)
			}
			out[i].Content = kept
			continue
		}
		ids := toolUseIDs(out[i])
		if len(ids) == 0 {
			continue
		}
		have := map[string]bool{}
		if i+1 < len(out) {
			for _, b := range out[i+1].Content {
				if b.Type == "tool_result" {
					have[b.ToolUseID] = true
				}
			}
		}
		var missing []Block
		for _, b := range out[i].Content {
			if b.Type == "tool_use" && !have[b.ID] {
				missing = append(missing, Block{Type: "tool_result", ToolUseID: b.ID, Content: "Not performed.", IsError: true})
			}
		}
		if len(missing) == 0 {
			continue
		}
		if i+1 < len(out) {
			out[i+1].Content = append(missing, out[i+1].Content...)
		} else {
			out = append(out, apiMessage{Role: "user", Content: missing})
		}
	}
	// Tool results must come first within a user message.
	var clean []apiMessage
	for _, m := range out {
		if m.Role == "user" {
			sort.SliceStable(m.Content, func(a, b int) bool {
				return m.Content[a].Type == "tool_result" && m.Content[b].Type != "tool_result"
			})
		}
		if len(m.Content) == 0 {
			continue
		}
		if n := len(clean); n > 0 && clean[n-1].Role == m.Role {
			clean[n-1].Content = append(clean[n-1].Content, m.Content...)
			continue
		}
		clean = append(clean, m)
	}
	return clean
}

func hasToolResult(m apiMessage) bool {
	for _, b := range m.Content {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

func toolUseIDs(m apiMessage) map[string]bool {
	ids := map[string]bool{}
	if m.Role != "assistant" {
		return ids
	}
	for _, b := range m.Content {
		if b.Type == "tool_use" {
			ids[b.ID] = true
		}
	}
	return ids
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
