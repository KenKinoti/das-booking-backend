package projects2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNormalizeSiteURL(t *testing.T) {
	good := map[string]string{
		"https://acme.atlassian.net":          "https://acme.atlassian.net",
		"acme.atlassian.net":                  "https://acme.atlassian.net",
		"https://ACME.atlassian.net/jira/for": "https://acme.atlassian.net",
		"https://jira.example.com:443":        "https://jira.example.com",
	}
	for in, want := range good {
		got, err := NormalizeSiteURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeSiteURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "http://acme.atlassian.net", "https://127.0.0.1", "https://[::1]", "https://localhost",
		"https://user:pw@acme.atlassian.net", "https://acme.atlassian.net:8080", "https://metadata.internal",
		"https://printer.local", "https://intranet", "ftp://acme.atlassian.net", "https://169.254.169.254"}
	for _, in := range bad {
		if got, err := NormalizeSiteURL(in); err == nil {
			t.Errorf("NormalizeSiteURL(%q) = %q, want error", in, got)
		}
	}
}

func TestSafeClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	c := &JiraClient{BaseURL: srv.URL, Email: "a@b.c", Token: "t", HTTP: SafeHTTPClient()}
	_, err := c.Myself(context.Background())
	if err == nil || !strings.Contains(err.Error(), "private or internal") {
		t.Fatalf("expected blocked address error, got %v", err)
	}
}

func TestMapStatusCategory(t *testing.T) {
	cases := map[string]string{"new": CatTodo, "indeterminate": CatInProgress, "done": CatDone, "undefined": CatTodo, "": CatTodo, "DONE": CatDone}
	for in, want := range cases {
		if got := MapStatusCategory(in); got != want {
			t.Errorf("MapStatusCategory(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapIssue(t *testing.T) {
	raw := `{"id":"10001","key":"ABC-7","fields":{
		"summary":"Fix login","status":{"name":"Code Review","statusCategory":{"key":"indeterminate"}},
		"issuetype":{"name":"Bug"},"priority":{"name":"High"},
		"assignee":{"displayName":"Ada Lovelace","emailAddress":"ada@example.com"},
		"duedate":"2026-10-03","updated":"2026-09-20T10:11:12.345+1000","customfield_10016":5}}`
	var ri RemoteIssue
	if err := json.Unmarshal([]byte(raw), &ri); err != nil {
		t.Fatal(err)
	}
	m := MapIssue(ri, "https://acme.atlassian.net/", []string{"customfield_10099", "customfield_10016"})
	if m.Key != "ABC-7" || m.ExternalID != "10001" || m.Title != "Fix login" || m.Status != "Code Review" ||
		m.StatusCategory != CatInProgress || m.Type != "Bug" || m.Priority != "High" || m.AssigneeName != "Ada Lovelace" ||
		m.AssigneeEmail != "ada@example.com" || m.Source != SourceJira {
		t.Fatalf("unexpected mapping: %+v", m)
	}
	if m.URL != "https://acme.atlassian.net/browse/ABC-7" {
		t.Errorf("url = %s", m.URL)
	}
	if m.DueDate == nil || m.DueDate.Format("2006-01-02") != "2026-10-03" {
		t.Errorf("due = %v", m.DueDate)
	}
	if m.RemoteUpdatedAt == nil || m.RemoteUpdatedAt.Format("2006-01-02T15:04") != "2026-09-20T00:11" {
		t.Errorf("updated = %v", m.RemoteUpdatedAt)
	}
	if m.StoryPoints == nil || *m.StoryPoints != 5 {
		t.Errorf("points = %v", m.StoryPoints)
	}
	if m.CompletedAt != nil {
		t.Errorf("in-progress issue should not be completed")
	}

	// done issue, unassigned, null fields
	raw = `{"id":"2","key":"ABC-8","fields":{"summary":"Ship","status":{"name":"Closed","statusCategory":{"key":"done"}},
		"assignee":null,"priority":null,"duedate":null,"updated":"2026-09-01T08:00:00.000+0000","resolutiondate":"2026-08-31T08:00:00.000+0000","customfield_10016":null}}`
	ri = RemoteIssue{}
	_ = json.Unmarshal([]byte(raw), &ri)
	m = MapIssue(ri, "https://acme.atlassian.net", []string{"customfield_10016"})
	if m.StatusCategory != CatDone || m.AssigneeName != "" || m.Priority != "" || m.DueDate != nil || m.StoryPoints != nil {
		t.Fatalf("unexpected mapping: %+v", m)
	}
	if m.CompletedAt == nil || m.CompletedAt.Format("2006-01-02") != "2026-08-31" {
		t.Errorf("completed = %v", m.CompletedAt)
	}
}

func TestBuildJQL(t *testing.T) {
	if got := BuildJQL("ABC", ""); got != `project = "ABC" ORDER BY updated DESC` {
		t.Error(got)
	}
	if got := BuildJQL("ABC", "assignee = currentUser() order by created"); got != `project = "ABC" AND (assignee = currentUser()) ORDER BY updated DESC` {
		t.Error(got)
	}
}

func issueJSON(n int) string {
	return fmt.Sprintf(`{"id":"%d","key":"ABC-%d","fields":{"summary":"Issue %d","status":{"name":"To Do","statusCategory":{"key":"new"}}}}`, 1000+n, n, n)
}

// fakeJira serves the endpoints the sync uses. issues is the issue count;
// legacyOnly makes /search/jql return 404 so the client must fall back.
func fakeJira(t *testing.T, issues int, legacyOnly bool, calls *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		user, pass, okAuth := r.BasicAuth()
		if !okAuth || user != "me@acme.com" || pass != "secret-token" {
			w.WriteHeader(401)
			w.Write([]byte(`{"errorMessages":["Unauthorized"]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch r.URL.Path {
		case "/rest/api/3/myself":
			w.Write([]byte(`{"accountId":"a1","displayName":"Me","emailAddress":"me@acme.com"}`))
		case "/rest/api/3/field":
			w.Write([]byte(`[{"id":"summary","name":"Summary","custom":false},{"id":"customfield_10016","name":"Story point estimate","custom":true}]`))
		case "/rest/api/3/project/search":
			start, _ := strconv.Atoi(q.Get("startAt"))
			all := []string{
				`{"id":"1","key":"ABC","name":"Alpha","description":"First","projectTypeKey":"software","lead":{"displayName":"Lead A"}}`,
				`{"id":"2","key":"XYZ","name":"Xylo","projectTypeKey":"business"}`,
				`{"id":"3","key":"OPS","name":"Ops","projectTypeKey":"service_desk"}`,
			}
			// two per page to exercise paging
			end := start + 2
			if end > len(all) {
				end = len(all)
			}
			fmt.Fprintf(w, `{"values":[%s],"startAt":%d,"maxResults":2,"total":%d,"isLast":%v}`, strings.Join(all[start:end], ","), start, len(all), end >= len(all))
		case "/rest/api/3/search/jql":
			if legacyOnly {
				w.WriteHeader(404)
				w.Write([]byte(`{"errorMessages":["Not found"]}`))
				return
			}
			if !strings.Contains(q.Get("jql"), `project = "ABC"`) {
				fmt.Fprint(w, `{"issues":[],"isLast":true}`)
				return
			}
			if !strings.Contains(q.Get("fields"), "customfield_10016") {
				t.Errorf("story points field not requested: %s", q.Get("fields"))
			}
			start := 0
			if tok := q.Get("nextPageToken"); tok != "" {
				start, _ = strconv.Atoi(strings.TrimPrefix(tok, "tok"))
			}
			end := start + 100
			if end > issues {
				end = issues
			}
			parts := []string{}
			for i := start; i < end; i++ {
				parts = append(parts, issueJSON(i+1))
			}
			next := ""
			if end < issues {
				next = fmt.Sprintf(`,"nextPageToken":"tok%d"`, end)
			}
			fmt.Fprintf(w, `{"issues":[%s]%s,"isLast":%v}`, strings.Join(parts, ","), next, end >= issues)
		case "/rest/api/3/search":
			start, _ := strconv.Atoi(q.Get("startAt"))
			n := issues
			if !strings.Contains(q.Get("jql"), `project = "ABC"`) {
				n = 0
			}
			end := start + 100
			if end > n {
				end = n
			}
			parts := []string{}
			for i := start; i < end; i++ {
				parts = append(parts, issueJSON(i+1))
			}
			fmt.Fprintf(w, `{"issues":[%s],"startAt":%d,"maxResults":100,"total":%d}`, strings.Join(parts, ","), start, n)
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestSearchIssuesPaginatesWithToken(t *testing.T) {
	srv := fakeJira(t, 250, false, nil)
	defer srv.Close()
	c := &JiraClient{BaseURL: srv.URL, Email: "me@acme.com", Token: "secret-token", HTTP: srv.Client()}
	sp := c.StoryPointFields(context.Background())
	if len(sp) != 1 || sp[0] != "customfield_10016" {
		t.Fatalf("story point fields = %v", sp)
	}
	issues, err := c.SearchIssues(context.Background(), BuildJQL("ABC", ""), sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 250 || issues[0].Key != "ABC-1" || issues[249].Key != "ABC-250" {
		t.Fatalf("got %d issues", len(issues))
	}
}

func TestSearchIssuesFallsBackToLegacy(t *testing.T) {
	srv := fakeJira(t, 230, true, nil)
	defer srv.Close()
	c := &JiraClient{BaseURL: srv.URL, Email: "me@acme.com", Token: "secret-token", HTTP: srv.Client()}
	issues, err := c.SearchIssues(context.Background(), BuildJQL("ABC", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 230 || issues[229].Key != "ABC-230" {
		t.Fatalf("got %d issues", len(issues))
	}
}

func TestListProjectsPaginates(t *testing.T) {
	srv := fakeJira(t, 0, false, nil)
	defer srv.Close()
	c := &JiraClient{BaseURL: srv.URL, Email: "me@acme.com", Token: "secret-token", HTTP: srv.Client()}
	ps, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 || ps[0].Lead.DisplayName != "Lead A" || ps[2].Key != "OPS" {
		t.Fatalf("projects = %+v", ps)
	}
}

func TestBadCredentialsMessage(t *testing.T) {
	srv := fakeJira(t, 0, false, nil)
	defer srv.Close()
	c := &JiraClient{BaseURL: srv.URL, Email: "me@acme.com", Token: "wrong", HTTP: srv.Client()}
	_, err := c.Myself(context.Background())
	je, isJE := err.(*JiraError)
	if !isJE || je.Status != 401 || !strings.Contains(je.Message, "email or API token") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeriveKey(t *testing.T) {
	cases := map[string]string{"Website Redesign": "WR", "Mobile app v2 launch plan extra words": "MAVLP", "x": "PRJX", "Payroll": "PAYRO"}
	for in, want := range cases {
		if got := deriveKey(in); got != want {
			t.Errorf("deriveKey(%q) = %q, want %q", in, got, want)
		}
	}
}
