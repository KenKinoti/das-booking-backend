package assistant

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
)

// ---- fake Anthropic --------------------------------------------------------

type fakeReply struct {
	status int
	text   string
	uses   []Block
}

type fakeAPI struct {
	mu      sync.Mutex
	replies []fakeReply
	reqs    []apiRequest
	keys    []string // x-api-key of every request
	srv     *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) push(r ...fakeReply) {
	f.mu.Lock()
	f.replies = append(f.replies, r...)
	f.mu.Unlock()
}

func (f *fakeAPI) requests() []apiRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiRequest(nil), f.reqs...)
}

func (f *fakeAPI) lastKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keys) == 0 {
		return ""
	}
	return f.keys[len(f.keys)-1]
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	var req apiRequest
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.keys = append(f.keys, r.Header.Get("x-api-key"))
	var rep fakeReply
	if len(f.replies) > 0 {
		rep = f.replies[0]
		f.replies = f.replies[1:]
	} else {
		rep = fakeReply{text: "ok"}
	}
	f.mu.Unlock()
	if err := validateHistory(req.Messages); err != nil {
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, err.Error())
		return
	}
	if rep.status != 0 {
		w.WriteHeader(rep.status)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"overloaded_error","message":"fake %d"}}`, rep.status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(name string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	}
	ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": 100, "output_tokens": 1}}})
	i := 0
	if rep.text != "" {
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
		for _, w := range strings.SplitAfter(rep.text, " ") {
			ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": w}})
		}
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		i++
	}
	for _, u := range rep.uses {
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": u.ID, "name": u.Name, "input": map[string]any{}}})
		js := string(u.Input)
		for len(js) > 0 {
			n := 5
			if n > len(js) {
				n = len(js)
			}
			ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": js[:n]}})
			js = js[n:]
		}
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		i++
	}
	stop := "end_turn"
	if len(rep.uses) > 0 {
		stop = "tool_use"
	}
	ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 20}})
	ev("message_stop", map[string]any{"type": "message_stop"})
}

// validateHistory enforces the Messages API invariants the real API checks.
func validateHistory(ms []apiMessage) error {
	if len(ms) == 0 || ms[0].Role != "user" {
		return fmt.Errorf("first message must be user")
	}
	for i := range ms {
		if i > 0 && ms[i].Role == ms[i-1].Role {
			return fmt.Errorf("consecutive %s messages", ms[i].Role)
		}
		if ms[i].Role == "assistant" {
			for _, b := range ms[i].Content {
				if b.Type != "tool_use" {
					continue
				}
				found := false
				if i+1 < len(ms) {
					for _, r := range ms[i+1].Content {
						if r.Type == "tool_result" && r.ToolUseID == b.ID {
							found = true
						}
					}
				}
				if !found {
					return fmt.Errorf("tool_use %s without result", b.ID)
				}
			}
		}
	}
	return nil
}

func use(id, name, input string) Block {
	return Block{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}
}

// ---- test tools --------------------------------------------------------------

type calls struct {
	mu sync.Mutex
	n  map[string]int
	a  map[string]map[string]any
	w  map[string]aitools.Identity
}

func (c *calls) record(name string, who aitools.Identity, args map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[name]++
	c.a[name] = args
	c.w[name] = who
}

func (c *calls) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[name]
}

func testTools(c *calls) map[string]aitools.Tool {
	obj := map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}
	return map[string]aitools.Tool{
		"things_search": {Name: "things_search", Title: "Search things", ReadOnly: true, InputSchema: obj, Handler: func(x *aitools.Ctx, a map[string]any) (any, error) {
			c.record("things_search", x.Who, a)
			return map[string]any{"items": []any{map[string]any{"id": "t1", "name": "Sam"}, map[string]any{"id": "t2", "name": "Sam"}}}, nil
		}},
		"things_create": {Name: "things_create", Title: "Create thing", InputSchema: obj, Handler: func(x *aitools.Ctx, a map[string]any) (any, error) {
			c.record("things_create", x.Who, a)
			return map[string]any{"data": map[string]any{"id": "t9", "name": a["name"], "url": "/things/t9"}}, nil
		}},
		"things_delete": {Name: "things_delete", Title: "Delete thing", Destructive: true, InputSchema: obj, Handler: func(x *aitools.Ctx, a map[string]any) (any, error) {
			c.record("things_delete", x.Who, a)
			return map[string]any{"deleted": true}, nil
		}},
		"things_fail": {Name: "things_fail", Title: "Fail thing", ReadOnly: true, Handler: func(x *aitools.Ctx, a map[string]any) (any, error) {
			return nil, &aitools.APIError{Status: 402, Message: "Module not in plan"}
		}},
	}
}

type env struct {
	svc   *Service
	api   *fakeAPI
	calls *calls
	db    *gorm.DB
}

func setup(t *testing.T) *env {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "a.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI(t)
	c := &calls{n: map[string]int{}, a: map[string]map[string]any{}, w: map[string]aitools.Identity{}}
	tools := testTools(c)
	cfg := Config{APIKey: "test", Model: "fake-model", BaseURL: api.srv.URL}
	svc := &Service{DB: db, Client: &Client{Cfg: cfg}, limiter: &rateLimiter{},
		ListTools: func() []aitools.Tool {
			var out []aitools.Tool
			for _, n := range []string{"things_create", "things_delete", "things_fail", "things_search"} {
				out = append(out, tools[n])
			}
			return out
		},
		GetTool: func(n string) (aitools.Tool, bool) { t, ok := tools[n]; return t, ok },
	}
	return &env{svc: svc, api: api, calls: c, db: db}
}

type event struct {
	Name string
	Data map[string]any
}

type recorder struct {
	mu sync.Mutex
	ev []event
}

func (r *recorder) emit(name string, data any) {
	b, _ := json.Marshal(data)
	var m map[string]any
	json.Unmarshal(b, &m)
	r.mu.Lock()
	r.ev = append(r.ev, event{name, m})
	r.mu.Unlock()
}

func (r *recorder) names() []string {
	var out []string
	for _, e := range r.ev {
		if e.Name != "text" && e.Name != "step" {
			out = append(out, e.Name)
		}
	}
	return out
}

func (r *recorder) find(name string) *event {
	for i := range r.ev {
		if r.ev[i].Name == name {
			return &r.ev[i]
		}
	}
	return nil
}

func (r *recorder) text() string {
	var b strings.Builder
	for _, e := range r.ev {
		if e.Name == "text" {
			b.WriteString(e.Data["delta"].(string))
		}
	}
	return b.String()
}

var alice = aitools.Identity{UserID: "u-alice", OrgID: "org-1", Role: "admin", Email: "alice@example.com", TZ: "Australia/Adelaide"}
var bob = aitools.Identity{UserID: "u-bob", OrgID: "org-1", Role: "admin", Email: "bob@example.com"}
var eve = aitools.Identity{UserID: "u-alice", OrgID: "org-2", Role: "admin"}

func convID(r *recorder) string {
	e := r.find("conversation")
	if e == nil {
		return ""
	}
	return e.Data["id"].(string)
}

// ---- tests -------------------------------------------------------------------

func TestReadOnlyToolRunsAutomatically(t *testing.T) {
	e := setup(t)
	e.api.push(
		fakeReply{text: "Looking.", uses: []Block{use("tu1", "things_search", `{"name":"Sam"}`)}},
		fakeReply{text: "There are two Sams."},
	)
	rec := &recorder{}
	err := e.svc.Chat(context.Background(), alice, ChatInput{Text: "find Sam", Page: Page{Path: "/customers/42", Name: "customer-detail", Params: map[string]string{"id": "42"}}}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.names(), ","); got != "conversation,tool_start,tool_result,done" {
		t.Fatalf("events = %s", got)
	}
	if rec.text() != "Looking.There are two Sams." {
		t.Fatalf("text = %q", rec.text())
	}
	if e.calls.count("things_search") != 1 || e.calls.a["things_search"]["name"] != "Sam" {
		t.Fatalf("search not called with args: %+v", e.calls.a)
	}
	if w := e.calls.w["things_search"]; w.UserID != alice.UserID || w.OrgID != alice.OrgID || w.TZ != alice.TZ {
		t.Fatalf("tool ran as %+v", w)
	}
	tr := rec.find("tool_result").Data
	if tr["summary"] != "Searched things · 2 results" || tr["status"] != "done" {
		t.Fatalf("tool_result = %+v", tr)
	}
	reqs := e.api.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	// System prompt carries identity, date and page context.
	sys, _ := json.Marshal(reqs[0].System)
	for _, want := range []string{"alice@example.com", "Australia/Adelaide", "/customers/42", "id=42", "Default currency: AUD", "Never guess"} {
		if !strings.Contains(string(sys), want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if len(reqs[0].Tools) != 4 || reqs[0].Tools[3].CacheControl == nil {
		t.Errorf("tools not sent correctly: %+v", reqs[0].Tools)
	}
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	if last.Role != "user" || last.Content[0].Type != "tool_result" || !strings.Contains(last.Content[0].Content, "t2") {
		t.Fatalf("tool_result not sent back: %+v", last)
	}
	// Persisted: user, assistant(tool_use), user(tool_result), assistant(text).
	var ms []Message
	e.db.Order("seq").Find(&ms)
	if len(ms) != 4 {
		t.Fatalf("stored %d messages", len(ms))
	}
	if ms[1].InputTokens != 100 || ms[1].OutputTokens != 20 {
		t.Fatalf("usage not stored: %+v", ms[1])
	}
	var conv Conversation
	e.db.First(&conv)
	if conv.Title != "find Sam" || conv.UserID != alice.UserID || conv.OrgID != alice.OrgID {
		t.Fatalf("conversation = %+v", conv)
	}
}

func TestWriteToolPausesUntilConfirmed(t *testing.T) {
	e := setup(t)
	e.api.push(fakeReply{text: "Creating.", uses: []Block{use("tu1", "things_create", `{"name":"Ann"}`)}})
	rec := &recorder{}
	if err := e.svc.Chat(context.Background(), alice, ChatInput{Text: "create Ann"}, rec.emit); err != nil {
		t.Fatal(err)
	}
	if e.calls.count("things_create") != 0 {
		t.Fatal("write tool ran without confirmation")
	}
	pa := rec.find("pending_action")
	if pa == nil {
		t.Fatalf("no pending_action: %v", rec.names())
	}
	if pa.Data["summary"] != "Create thing: Ann" || pa.Data["destructive"] != false || pa.Data["id"] != "tu1" {
		t.Fatalf("pending = %+v", pa.Data)
	}
	if d := rec.find("done"); d == nil || d.Data["paused"] != true {
		t.Fatal("expected paused done")
	}
	id := convID(rec)
	// The view shows the pending card after reload.
	conv, _ := e.svc.getConversation(alice, id)
	view, _ := e.svc.View(conv)
	if view["pending"] == nil {
		t.Fatal("view lost pending action")
	}
	// Someone else cannot confirm it.
	if err := e.svc.Resolve(context.Background(), bob, id, "tu1", true, Page{}, (&recorder{}).emit); err != ErrNotFound {
		t.Fatalf("bob resolve err = %v", err)
	}
	// Wrong action id.
	if err := e.svc.Resolve(context.Background(), alice, id, "nope", true, Page{}, (&recorder{}).emit); err == nil {
		t.Fatal("expected error for wrong action id")
	}
	e.api.push(fakeReply{text: "Created Ann."})
	rec2 := &recorder{}
	if err := e.svc.Resolve(context.Background(), alice, id, "tu1", true, Page{}, rec2.emit); err != nil {
		t.Fatal(err)
	}
	if e.calls.count("things_create") != 1 || e.calls.a["things_create"]["name"] != "Ann" {
		t.Fatal("create not executed once with args")
	}
	tr := rec2.find("tool_result").Data
	if tr["changed"] != "things" || tr["link"] != "/things/t9" || tr["summary"] != "Created thing Ann" {
		t.Fatalf("tool_result = %+v", tr)
	}
	if rec2.text() != "Created Ann." {
		t.Fatalf("resume text = %q", rec2.text())
	}
	// Confirming twice fails.
	if err := e.svc.Resolve(context.Background(), alice, id, "tu1", true, Page{}, (&recorder{}).emit); err == nil {
		t.Fatal("double confirm should fail")
	}
	if e.calls.count("things_create") != 1 {
		t.Fatal("create ran twice")
	}
}

func TestCancelSendsDeclined(t *testing.T) {
	e := setup(t)
	e.api.push(fakeReply{uses: []Block{use("tu1", "things_delete", `{"name":"Old"}`)}}, fakeReply{text: "OK, not deleted."})
	rec := &recorder{}
	e.svc.Chat(context.Background(), alice, ChatInput{Text: "delete Old"}, rec.emit)
	pa := rec.find("pending_action")
	if pa == nil || pa.Data["destructive"] != true {
		t.Fatal("destructive tool must be pending and flagged")
	}
	rec2 := &recorder{}
	if err := e.svc.Resolve(context.Background(), alice, convID(rec), "tu1", false, Page{}, rec2.emit); err != nil {
		t.Fatal(err)
	}
	if e.calls.count("things_delete") != 0 {
		t.Fatal("declined tool ran")
	}
	reqs := e.api.requests()
	last := reqs[len(reqs)-1].Messages
	res := last[len(last)-1].Content[0]
	if res.Type != "tool_result" || !res.IsError || !strings.Contains(res.Content, "declined") {
		t.Fatalf("declined result = %+v", res)
	}
	if rec2.find("tool_result").Data["status"] != "declined" {
		t.Fatal("expected declined chip")
	}
}

func TestAutoApprove(t *testing.T) {
	e := setup(t)
	e.db.Create(&UserSettings{UserID: alice.UserID, OrgID: alice.OrgID, AutoApprove: true})
	e.api.push(
		fakeReply{uses: []Block{use("a", "things_create", `{"name":"X"}`), use("b", "things_delete", `{"name":"Y"}`), use("c", "things_search", `{}`)}},
	)
	rec := &recorder{}
	e.svc.Chat(context.Background(), alice, ChatInput{Text: "do it"}, rec.emit)
	if e.calls.count("things_create") != 1 || e.calls.count("things_search") != 1 {
		t.Fatal("non-destructive write and read should run with auto-approve")
	}
	if e.calls.count("things_delete") != 0 || rec.find("pending_action").Data["id"] != "b" {
		t.Fatal("destructive tool must still wait")
	}
	// Approving the delete sends all three results in tool_use order.
	e.api.push(fakeReply{text: "done"})
	e.svc.Resolve(context.Background(), alice, convID(rec), "b", true, Page{}, (&recorder{}).emit)
	reqs := e.api.requests()
	msgs := reqs[len(reqs)-1].Messages
	var ids []string
	for _, b := range msgs[len(msgs)-1].Content {
		ids = append(ids, b.ToolUseID)
	}
	if strings.Join(ids, "") != "abc" {
		t.Fatalf("result order = %v", ids)
	}
}

func TestMultiplePendingAreConfirmedOneByOne(t *testing.T) {
	e := setup(t)
	e.api.push(fakeReply{uses: []Block{use("a", "things_create", `{"name":"X"}`), use("b", "things_create", `{"name":"Y"}`)}})
	rec := &recorder{}
	e.svc.Chat(context.Background(), alice, ChatInput{Text: "two"}, rec.emit)
	id := convID(rec)
	r2 := &recorder{}
	e.svc.Resolve(context.Background(), alice, id, "a", true, Page{}, r2.emit)
	if pa := r2.find("pending_action"); pa == nil || pa.Data["id"] != "b" {
		t.Fatal("second action should be pending next")
	}
	if len(e.api.requests()) != 1 {
		t.Fatal("model must not be called until all actions resolved")
	}
	e.api.push(fakeReply{text: "both"})
	r3 := &recorder{}
	e.svc.Resolve(context.Background(), alice, id, "b", false, Page{}, r3.emit)
	if r3.text() != "both" || e.calls.count("things_create") != 1 {
		t.Fatalf("text=%q creates=%d", r3.text(), e.calls.count("things_create"))
	}
}

func TestNewMessageDeclinesPending(t *testing.T) {
	e := setup(t)
	e.api.push(fakeReply{uses: []Block{use("a", "things_create", `{"name":"X"}`)}}, fakeReply{text: "sure"})
	rec := &recorder{}
	e.svc.Chat(context.Background(), alice, ChatInput{Text: "make X"}, rec.emit)
	rec2 := &recorder{}
	if err := e.svc.Chat(context.Background(), alice, ChatInput{ConversationID: convID(rec), Text: "actually never mind"}, rec2.emit); err != nil {
		t.Fatal(err)
	}
	if e.calls.count("things_create") != 0 {
		t.Fatal("pending action ran")
	}
	reqs := e.api.requests()
	last := reqs[len(reqs)-1].Messages
	u := last[len(last)-1]
	if u.Content[0].Type != "tool_result" || u.Content[1].Text != "actually never mind" {
		t.Fatalf("user msg = %+v", u.Content)
	}
}

func TestToolErrorIsReported(t *testing.T) {
	e := setup(t)
	e.api.push(fakeReply{uses: []Block{use("a", "things_fail", `{}`)}}, fakeReply{text: "not in plan"})
	rec := &recorder{}
	e.svc.Chat(context.Background(), alice, ChatInput{Text: "x"}, rec.emit)
	tr := rec.find("tool_result").Data
	if tr["status"] != "error" || !strings.Contains(tr["summary"].(string), "Module not in plan") {
		t.Fatalf("tool_result = %+v", tr)
	}
	reqs := e.api.requests()
	m := reqs[1].Messages[len(reqs[1].Messages)-1].Content[0]
	if !m.IsError || !strings.Contains(m.Content, "402") {
		t.Fatalf("error result = %+v", m)
	}
}

func TestMaxSteps(t *testing.T) {
	e := setup(t)
	e.svc.MaxSteps = 3
	for i := 0; i < 5; i++ {
		e.api.push(fakeReply{uses: []Block{use(fmt.Sprintf("s%d", i), "things_search", `{}`)}})
	}
	rec := &recorder{}
	if err := e.svc.Chat(context.Background(), alice, ChatInput{Text: "loop"}, rec.emit); err != nil {
		t.Fatal(err)
	}
	if len(e.api.requests()) != 3 || !strings.Contains(rec.text(), "stopped after 3 steps") {
		t.Fatalf("reqs=%d text=%q", len(e.api.requests()), rec.text())
	}
	// History stays valid for the next turn.
	e.api.push(fakeReply{text: "fine"})
	if err := e.svc.Chat(context.Background(), alice, ChatInput{ConversationID: convID(rec), Text: "continue"}, (&recorder{}).emit); err != nil {
		t.Fatal(err)
	}
}

func TestAPIErrorsAndRetry(t *testing.T) {
	e := setup(t)
	// Transient 529 is retried transparently.
	e.api.push(fakeReply{status: 529}, fakeReply{text: "recovered"})
	rec := &recorder{}
	if err := e.svc.Chat(context.Background(), alice, ChatInput{Text: "hi"}, rec.emit); err != nil || rec.text() != "recovered" {
		t.Fatalf("err=%v text=%q", err, rec.text())
	}
	// Persistent failure surfaces an APIError; the user message is kept for retry.
	e.api.push(fakeReply{status: 529}, fakeReply{status: 529}, fakeReply{status: 529})
	rec2 := &recorder{}
	err := e.svc.Chat(context.Background(), alice, ChatInput{ConversationID: convID(rec), Text: "again"}, rec2.emit)
	ae, ok := err.(*APIError)
	if !ok || ae.Status != 529 || !strings.Contains(ae.Friendly(), "overloaded") {
		t.Fatalf("err = %v", err)
	}
	conv, _ := e.svc.getConversation(alice, convID(rec))
	view, _ := e.svc.View(conv)
	if view["can_retry"] != true {
		t.Fatal("expected can_retry")
	}
	e.api.push(fakeReply{text: "second time lucky"})
	rec3 := &recorder{}
	if err := e.svc.Chat(context.Background(), alice, ChatInput{ConversationID: convID(rec), Retry: true}, rec3.emit); err != nil || rec3.text() != "second time lucky" {
		t.Fatalf("retry err=%v text=%q", err, rec3.text())
	}
	// Nothing to retry now.
	if err := e.svc.Chat(context.Background(), alice, ChatInput{ConversationID: convID(rec), Retry: true}, (&recorder{}).emit); err == nil {
		t.Fatal("expected nothing to retry")
	}
}

func TestBuildHistoryRepairs(t *testing.T) {
	mk := func(role string, blocks ...Block) Message {
		b, _ := json.Marshal(blocks)
		return Message{Role: role, Content: string(b)}
	}
	ms := []Message{
		mk("user", Block{Type: "text", Text: "a"}),
		mk("assistant", use("x", "t", `{}`)), // no result stored
		mk("user", Block{Type: "text", Text: "b"}),
		mk("user", Block{Type: "text", Text: "c"}), // consecutive user
		mk("assistant", Block{Type: "text", Text: "d"}),
		mk("user", Block{Type: "tool_result", ToolUseID: "zzz", Content: "orphan"}, Block{Type: "text", Text: "e"}),
	}
	h := buildHistory(ms)
	if err := validateHistory(h); err != nil {
		t.Fatal(err)
	}
	if len(h) != 5 || h[2].Content[0].Type != "tool_result" || len(h[2].Content) != 3 {
		b, _ := json.Marshal(h)
		t.Fatalf("history = %s", b)
	}
	for _, b := range h[4].Content {
		if b.ToolUseID == "zzz" {
			t.Fatal("orphan result kept")
		}
	}
}

func TestRateLimiter(t *testing.T) {
	r := &rateLimiter{}
	now := time.Now()
	for i := 0; i < 3; i++ {
		if ok, _ := r.allow("u", now, 3, time.Minute); !ok {
			t.Fatal("should allow")
		}
	}
	if ok, wait := r.allow("u", now, 3, time.Minute); ok || wait <= 0 {
		t.Fatal("should block")
	}
	if ok, _ := r.allow("other", now, 3, time.Minute); !ok {
		t.Fatal("per-user")
	}
	if ok, _ := r.allow("u", now.Add(61*time.Second), 3, time.Minute); !ok {
		t.Fatal("window should slide")
	}
}

// ---- HTTP: scoping and SSE -----------------------------------------------------

func router(e *env) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1/assistant", func(c *gin.Context) {
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_role", c.GetHeader("X-Role"))
		c.Set("user_email", c.GetHeader("X-User")+"@example.com")
		c.Next()
	})
	(&Handler{Svc: e.svc}).RegisterRoutes(g)
	return r
}

func do(r http.Handler, who aitools.Identity, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User", who.UserID)
	req.Header.Set("X-Org", who.OrgID)
	req.Header.Set("X-Role", who.Role)
	req.Header.Set("X-Timezone", "Europe/London")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func parseSSE(t *testing.T, body string) []event {
	var out []event
	sc := bufio.NewScanner(strings.NewReader(body))
	var name string
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "event: "):
			name = strings.TrimPrefix(l, "event: ")
		case strings.HasPrefix(l, "data: "):
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &m); err != nil {
				t.Fatalf("bad data %q", l)
			}
			out = append(out, event{name, m})
		}
	}
	return out
}

func TestHTTPChatScopingAndCRUD(t *testing.T) {
	e := setup(t)
	r := router(e)
	e.api.push(fakeReply{text: "Hello Alice", uses: []Block{use("w1", "things_create", `{"name":"Z"}`)}})
	w := do(r, alice, "POST", "/api/v1/assistant/chat", `{"message":"make Z please","page":{"path":"/invoices"}}`)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("chat %d %s", w.Code, w.Body.String())
	}
	evs := parseSSE(t, w.Body.String())
	var id string
	var sawPending bool
	for _, ev := range evs {
		if ev.Name == "conversation" {
			id = ev.Data["id"].(string)
		}
		if ev.Name == "pending_action" {
			sawPending = true
		}
	}
	if id == "" || !sawPending {
		t.Fatalf("events %+v", evs)
	}
	if calls := e.calls.w["things_create"]; calls.UserID != "" {
		t.Fatal("ran before confirm")
	}
	// Bob (same org) and Eve (same user id, other org) cannot see or touch it.
	for _, other := range []aitools.Identity{bob, eve} {
		if w := do(r, other, "GET", "/api/v1/assistant/conversations/"+id, ""); w.Code != 404 {
			t.Fatalf("%s get = %d", other.UserID, w.Code)
		}
		if w := do(r, other, "POST", "/api/v1/assistant/conversations/"+id+"/actions/w1", `{"approve":true}`); w.Code != 404 {
			t.Fatalf("%s confirm = %d", other.UserID, w.Code)
		}
		if w := do(r, other, "DELETE", "/api/v1/assistant/conversations/"+id, ""); w.Code != 404 {
			t.Fatalf("%s delete = %d", other.UserID, w.Code)
		}
		if w := do(r, other, "POST", "/api/v1/assistant/chat", `{"conversation_id":"`+id+`","message":"hi"}`); w.Code != 404 {
			t.Fatalf("%s chat = %d", other.UserID, w.Code)
		}
		w := do(r, other, "GET", "/api/v1/assistant/conversations", "")
		if strings.Contains(w.Body.String(), id) {
			t.Fatalf("%s lists alice's conversation", other.UserID)
		}
	}
	// Alice confirms over HTTP; the tool runs with her identity and TZ header.
	e.api.push(fakeReply{text: "Made Z."})
	w = do(r, alice, "POST", "/api/v1/assistant/conversations/"+id+"/actions/w1", `{"approve":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: done") {
		t.Fatalf("confirm %d %s", w.Code, w.Body.String())
	}
	if got := e.calls.w["things_create"]; got.UserID != alice.UserID || got.OrgID != alice.OrgID || got.TZ != "Europe/London" || got.Email != "u-alice@example.com" {
		t.Fatalf("ran as %+v", got)
	}
	// List, get, rename, delete.
	w = do(r, alice, "GET", "/api/v1/assistant/conversations", "")
	if !strings.Contains(w.Body.String(), "make Z please") {
		t.Fatalf("list = %s", w.Body.String())
	}
	w = do(r, alice, "GET", "/api/v1/assistant/conversations/"+id, "")
	var got struct {
		Data struct {
			Items   []map[string]any `json:"items"`
			Pending any              `json:"pending"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	kinds := []string{}
	for _, it := range got.Data.Items {
		kinds = append(kinds, it["kind"].(string))
	}
	if strings.Join(kinds, ",") != "user,assistant,tool,assistant" || got.Data.Pending != nil {
		t.Fatalf("items = %v pending=%v", kinds, got.Data.Pending)
	}
	if got.Data.Items[2]["status"] != "done" || got.Data.Items[2]["link"] != "/things/t9" {
		t.Fatalf("tool item = %+v", got.Data.Items[2])
	}
	if w := do(r, alice, "PATCH", "/api/v1/assistant/conversations/"+id, `{"title":"  Renamed  "}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"Renamed"`) {
		t.Fatalf("rename %d %s", w.Code, w.Body.String())
	}
	if w := do(r, alice, "DELETE", "/api/v1/assistant/conversations/"+id, ""); w.Code != 200 {
		t.Fatalf("delete %d", w.Code)
	}
	var n int64
	e.db.Model(&Message{}).Where("conversation_id = ?", id).Count(&n)
	if n != 0 {
		t.Fatal("messages not deleted")
	}
}

func TestHTTPStatusSettingsAndRateLimit(t *testing.T) {
	e := setup(t)
	r := router(e)
	sa := aitools.Identity{UserID: "u-sa", OrgID: "org-1", Role: "super_admin"}
	w := do(r, sa, "GET", "/api/v1/assistant/status", "")
	if !strings.Contains(w.Body.String(), `"enabled":true`) || !strings.Contains(w.Body.String(), "usage_today") {
		t.Fatalf("status = %s", w.Body.String())
	}
	w = do(r, alice, "GET", "/api/v1/assistant/status", "")
	if strings.Contains(w.Body.String(), "usage_today") {
		t.Fatal("usage shown to non-super-admin")
	}
	if w := do(r, alice, "PUT", "/api/v1/assistant/settings", `{"auto_approve":true}`); w.Code != 200 {
		t.Fatalf("settings %d", w.Code)
	}
	if !e.svc.autoApprove(alice) || e.svc.autoApprove(bob) {
		t.Fatal("setting not per-user")
	}
	// Disabled service: no pinned client, nothing saved, nothing in the environment.
	t.Setenv("ANTHROPIC_API_KEY", "")
	off := setup(t)
	off.svc.Client = nil
	ro := router(off)
	w = do(ro, alice, "GET", "/api/v1/assistant/status", "")
	if !strings.Contains(w.Body.String(), `"enabled":false`) || !strings.Contains(w.Body.String(), "Ask your administrator") || strings.Contains(w.Body.String(), "setup_path") {
		t.Fatalf("disabled status = %s", w.Body.String())
	}
	w = do(ro, sa, "GET", "/api/v1/assistant/status", "")
	body := w.Body.String()
	if !strings.Contains(body, `"source":"none"`) || !strings.Contains(body, "System settings → AI assistant") || !strings.Contains(body, `"setup_path":"/system-settings?tab=ai"`) {
		t.Fatalf("super admin disabled status = %s", body)
	}
	// The old wording sent people to the server; a restart is never needed now.
	if strings.Contains(strings.ToLower(body), "restart") || strings.Contains(body, "ANTHROPIC_API_KEY") {
		t.Fatalf("status still mentions a manual server step: %s", body)
	}
	if w := do(ro, alice, "POST", "/api/v1/assistant/chat", `{"message":"hi"}`); w.Code != 503 {
		t.Fatalf("disabled chat = %d", w.Code)
	}
	// Rate limit.
	for i := 0; i < RateLimit; i++ {
		e.api.push(fakeReply{text: "ok"})
		if w := do(r, bob, "POST", "/api/v1/assistant/chat", `{"message":"hi"}`); w.Code != 200 {
			t.Fatalf("msg %d = %d", i, w.Code)
		}
	}
	if w := do(r, bob, "POST", "/api/v1/assistant/chat", `{"message":"hi"}`); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

func TestHTTPErrorEvent(t *testing.T) {
	e := setup(t)
	e.svc.Client.Cfg.APIKey = "x"
	r := router(e)
	e.api.push(fakeReply{status: 401})
	w := do(r, alice, "POST", "/api/v1/assistant/chat", `{"message":"hi"}`)
	evs := parseSSE(t, w.Body.String())
	last := evs[len(evs)-1]
	if last.Name != "error" || last.Data["retryable"] != false || !strings.Contains(last.Data["message"].(string), "API key") {
		t.Fatalf("events = %+v", evs)
	}
}

func TestDescribeHelpers(t *testing.T) {
	tl := aitools.Tool{Name: "customers_search", ReadOnly: true}
	if toolTitle(tl, "") != "Search customers" || runningLabel(tl) != "Searching customers…" {
		t.Fatalf("title=%q label=%q", toolTitle(tl, ""), runningLabel(tl))
	}
	inv := aitools.Tool{Name: "invoices_create", Title: "Create invoice"}
	m := resultMeta(inv, map[string]any{}, map[string]any{"data": map[string]any{"invoice_number": "INV-0042", "url": "/invoicing/invoices/9"}})
	if m.Summary != "Created invoice INV-0042" || m.Link != "/invoicing/invoices/9" || m.Changed != "invoices" {
		t.Fatalf("meta = %+v", m)
	}
	if safeLink("javascript:alert(1)") != "" || safeLink("//evil.com") != "" {
		t.Fatal("unsafe link allowed")
	}
	if autoTitle(strings.Repeat("word ", 30)) == "" || len([]rune(autoTitle(strings.Repeat("word ", 30)))) > 61 {
		t.Fatal("autoTitle")
	}
}
