package seccall

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type apiT struct {
	t   *testing.T
	eng *gin.Engine
}

func (a apiT) do(method, path, token string, body any, headers ...string) (int, map[string]any) {
	a.t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	a.eng.ServeHTTP(w, req)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

// newAPI mounts the handler with a stand-in for the JWT middleware: the
// X-Test-Role / X-Test-User / X-Test-Org headers play the token's claims.
func newAPI(t *testing.T, s *Service) apiT {
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	h := &Handler{S: s}
	auth := eng.Group("/api/v1", func(c *gin.Context) {
		c.Set("org_id", c.GetHeader("X-Test-Org"))
		c.Set("user_id", c.GetHeader("X-Test-User"))
		c.Set("user_role", c.GetHeader("X-Test-Role"))
	})
	h.RegisterAuth(auth)
	h.RegisterPublic(eng.Group("/api/v1/public"))
	return apiT{t: t, eng: eng}
}

func TestHTTPNoPlaintextAccepted(t *testing.T) {
	db := testDB(t)
	s := svcAt(db, newClock())
	s.AppURL = "https://app.example.com"
	db.Exec(`INSERT INTO users (id, email, first_name, last_name, role, organization_id) VALUES ('mgr-1', 'm@x.test', 'Wanjiru', 'Kamau', 'manager', 'org-a')`)
	a := newAPI(t, s)
	l := newLink(t)
	as := func(role, user string) []string {
		return []string{"X-Test-Org", "org-a", "X-Test-User", user, "X-Test-Role", role}
	}

	st, body := a.do("POST", "/api/v1/secure-calls", "", map[string]any{"link_check": l.check}, as("staff", "staff-1")...)
	if st != 403 || errCode(body) != "FORBIDDEN" {
		t.Fatalf("staff create: %d %v", st, body)
	}
	st, body = a.do("POST", "/api/v1/secure-calls", "", map[string]any{"link_check": l.check, "title": "Board", "expires": "1h"}, as("manager", "mgr-1")...)
	if st != 200 {
		t.Fatalf("create: %d %v", st, body)
	}
	data := body["data"].(map[string]any)
	call := data["call"].(map[string]any)
	id := call["id"].(string)
	if data["app_url"] != "https://app.example.com" || call["host_name"] != "Wanjiru Kamau" || call["path"] != "/c/"+id {
		t.Fatalf("create body %v", data)
	}
	if _, leaked := call["check_hash"]; leaked {
		t.Fatal("check hash returned")
	}
	st, _ = a.do("GET", "/api/v1/secure-call-settings", "", nil, as("manager", "mgr-1")...)
	if st != 403 {
		t.Fatalf("manager read call settings: %d", st)
	}
	st, _ = a.do("GET", "/api/v1/secure-call-settings", "", nil, as("super_admin", "sa")...)
	if st != 200 {
		t.Fatalf("super admin settings: %d", st)
	}

	// public: status needs nothing; preview needs the link check
	st, body = a.do("GET", "/api/v1/public/calls/"+id, "", nil)
	if st != 200 || body["data"].(map[string]any)["status"] != "active" {
		t.Fatalf("status %d %v", st, body)
	}
	if _, has := body["data"].(map[string]any)["title"]; has {
		t.Fatal("status reveals the title without the link check")
	}
	st, body = a.do("POST", "/api/v1/public/calls/"+id+"/preview", "", map[string]any{"link_check": newLink(t).check})
	if st != 403 || errCode(body) != "WRONG_LINK" {
		t.Fatalf("preview wrong %d %v", st, body)
	}
	st, body = a.do("POST", "/api/v1/public/calls/"+NewRoomID()+"/join", "", map[string]any{"name": "x", "link_check": l.check})
	if st != 404 {
		t.Fatalf("unknown room %d", st)
	}

	st, body = a.do("POST", "/api/v1/public/calls/"+id+"/join", "", map[string]any{"name": "Amina", "link_check": l.check})
	if st != 200 {
		t.Fatalf("join %d %v", st, body)
	}
	g1 := body["data"].(map[string]any)
	tok1 := g1["token"].(string)
	_, body = a.do("POST", "/api/v1/public/calls/"+id+"/join", "", map[string]any{"name": "Brian", "link_check": l.check})
	g2 := body["data"].(map[string]any)
	pid2 := g2["participant_id"].(string)

	count := func() int64 {
		var n int64
		db.Model(&Message{}).Where("room_id = ? AND kind = 'sig'", id).Count(&n)
		return n
	}
	sdp := "v=0\r\no=- 46117314 2 IN IP4 127.0.0.1\r\ns=-\r\na=fingerprint:sha-256 AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89\r\n"
	iv := base64.RawURLEncoding.EncodeToString(make([]byte, 12))
	plain := []any{
		map[string]any{"to": pid2, "sdp": sdp},
		map[string]any{"to": pid2, "candidate": "candidate:1 1 udp 2122260223 192.168.1.20 54400 typ host"},
		map[string]any{"to": pid2, "ciphertext": sdp},
		map[string]any{"to": pid2, "ciphertext": `{"type":"offer","sdp":"v=0"}`},
		map[string]any{"to": pid2, "ciphertext": iv + "." + base64.RawURLEncoding.EncodeToString([]byte(sdp))},
		map[string]any{"to": pid2, "ciphertext": base64.StdEncoding.EncodeToString([]byte(sdp))},
		map[string]any{"messages": []any{map[string]any{"to": pid2, "ciphertext": l.seal("fine and encrypted, long enough", "x")}, map[string]any{"to": pid2, "ciphertext": sdp}}},
		`{"to":"` + pid2 + `","data":{"type":"offer","sdp":"v=0"}}`,
	}
	for i, p := range plain {
		st, body = a.do("POST", "/api/v1/public/calls/"+id+"/signal", tok1, p)
		if st != 400 || errCode(body) != "CIPHERTEXT_REQUIRED" {
			t.Fatalf("plain #%d accepted: %d %v", i, st, body)
		}
	}
	if n := count(); n != 0 {
		t.Fatalf("%d plaintext messages stored", n)
	}
	// without a participant token nothing is relayed
	ct := l.seal(`{"n":1,"t":"offer","sdp":"v=0 ..."}`, "dasyin-seccall/v1|"+id+"|"+g1["participant_id"].(string)+"|"+pid2)
	st, _ = a.do("POST", "/api/v1/public/calls/"+id+"/signal", "", map[string]any{"to": pid2, "ciphertext": ct})
	if st != 401 {
		t.Fatalf("no token: %d", st)
	}
	st, _ = a.do("POST", "/api/v1/public/calls/"+id+"/signal", "sct_made-up-token-made-up-token", map[string]any{"to": pid2, "ciphertext": ct})
	if st != 401 {
		t.Fatalf("made-up token: %d", st)
	}
	// real ciphertext is relayed byte for byte
	st, body = a.do("POST", "/api/v1/public/calls/"+id+"/signal", tok1, map[string]any{"to": pid2, "ciphertext": ct})
	if st != 200 {
		t.Fatalf("signal %d %v", st, body)
	}
	var stored Message
	db.Where("room_id = ? AND kind = 'sig'", id).First(&stored)
	if stored.Body != ct {
		t.Fatal("the relay changed the ciphertext")
	}
	since := int64(g2["since"].(float64))
	st, body = a.do("GET", "/api/v1/public/calls/"+id+"/poll?wait=0&since="+jsonNum(since), g2["token"].(string), nil)
	if st != 200 {
		t.Fatalf("poll %d %v", st, body)
	}
	msgs := body["data"].(map[string]any)["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["ciphertext"] != ct {
		t.Fatalf("poll msgs %v", msgs)
	}
	// ICE only for participants
	st, _ = a.do("GET", "/api/v1/public/calls/"+id+"/ice", "", nil)
	if st != 401 {
		t.Fatalf("ice without token %d", st)
	}
	st, _ = a.do("GET", "/api/v1/public/calls/"+id+"/ice", tok1, nil)
	if st != 200 {
		t.Fatalf("ice %d", st)
	}
	// sendBeacon-style leave (text/plain body with the token)
	req := httptest.NewRequest("POST", "/api/v1/public/calls/"+id+"/leave", strings.NewReader(`{"token":"`+tok1+`"}`))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	w := httptest.NewRecorder()
	a.eng.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("beacon leave %d %s", w.Code, w.Body.String())
	}
	st, _ = a.do("GET", "/api/v1/public/calls/"+id+"/poll?wait=0", tok1, nil)
	if st != 401 {
		t.Fatalf("poll after leave %d", st)
	}

	// host actions through HTTP, other organisation refused
	st, _ = a.do("POST", "/api/v1/secure-calls/"+id+"/lock", "", nil, "X-Test-Org", "org-b", "X-Test-User", "mgr-1", "X-Test-Role", "manager")
	if st != 404 {
		t.Fatalf("other org lock %d", st)
	}
	st, _ = a.do("POST", "/api/v1/secure-calls/"+id+"/lock", "", nil, as("manager", "mgr-1")...)
	if st != 200 {
		t.Fatalf("lock %d", st)
	}
	st, body = a.do("POST", "/api/v1/public/calls/"+id+"/join", "", map[string]any{"name": "Late", "link_check": l.check})
	if st != 423 || errCode(body) != "LOCKED" {
		t.Fatalf("join locked %d %v", st, body)
	}
	st, _ = a.do("POST", "/api/v1/secure-calls/"+id+"/end", "", nil, as("manager", "mgr-1")...)
	if st != 200 {
		t.Fatalf("end %d", st)
	}
	st, body = a.do("POST", "/api/v1/public/calls/"+id+"/preview", "", map[string]any{"link_check": l.check})
	if st != 410 || errCode(body) != "CALL_ENDED" {
		t.Fatalf("preview ended %d %v", st, body)
	}
	st, body = a.do("GET", "/api/v1/secure-calls?status=ended", "", nil, as("manager", "mgr-1")...)
	if st != 200 || len(body["data"].(map[string]any)["calls"].([]any)) != 1 {
		t.Fatalf("ended list %d %v", st, body)
	}
}

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.RemoteAddr = "10.0.0.1:1234"
	c.Request.Header.Set("X-Forwarded-For", "1.2.3.4, 198.51.100.77")
	if ip := ClientIP(c); ip != "198.51.100.77" {
		t.Fatalf("got %s", ip)
	}
	c.Request.Header.Del("X-Forwarded-For")
	if ip := ClientIP(c); ip != "10.0.0.1" {
		t.Fatalf("got %s", ip)
	}
	_ = http.StatusOK
}
