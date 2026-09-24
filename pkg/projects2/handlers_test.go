package projects2

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Integration tests run against Postgres when TEST_DATABASE_URL is set, e.g.
// TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/das_test?sslmode=disable
func setup(t *testing.T, jiraURL string) (*gin.Engine, *gorm.DB, string) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	org := "test-" + uuid.NewString()
	t.Cleanup(func() {
		db.Where("org_id = ?", org).Delete(&Task{})
		db.Where("org_id = ?", org).Delete(&Project{})
		db.Where("org_id = ?", org).Delete(&JiraConnection{})
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(db, nil)
	h.HTTP = http.DefaultClient
	// The fake Jira is plain http on loopback; accept exactly it in tests.
	h.ValidateSite = func(s string) (string, error) {
		if s == jiraURL {
			return s, nil
		}
		return NormalizeSiteURL(s)
	}
	h.Now = func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) }
	g := r.Group("/pm", func(c *gin.Context) { c.Set("org_id", org); c.Next() })
	h.RegisterRoutes(g, nil)
	return r, db, org
}

func do(t *testing.T, r *gin.Engine, method, path string, body interface{}) (int, map[string]interface{}) {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func data(m map[string]interface{}) map[string]interface{} {
	d, _ := m["data"].(map[string]interface{})
	return d
}

func TestLocalProjectAndTaskCRUD(t *testing.T) {
	r, _, _ := setup(t, "")
	code, res := do(t, r, "POST", "/pm/projects", gin.H{"name": "Website Redesign", "due_date": "2026-12-01", "budget": 5000})
	if code != 201 {
		t.Fatalf("create project %d %v", code, res)
	}
	p := data(res)
	pid := p["id"].(string)
	if p["key"] != "WR" || p["source"] != "local" || p["status"] != "active" {
		t.Fatalf("project = %v", p)
	}
	if code, _ := do(t, r, "POST", "/pm/projects", gin.H{"name": "Other", "key": "WR"}); code != 409 {
		t.Fatalf("duplicate key accepted: %d", code)
	}
	code, res = do(t, r, "POST", "/pm/projects/"+pid+"/tasks", gin.H{"title": "Wireframes", "due_date": "2026-09-01", "priority": "high"})
	if code != 201 || data(res)["key"] != "WR-1" || data(res)["priority"] != "High" {
		t.Fatalf("create task %d %v", code, res)
	}
	tid := data(res)["id"].(string)
	do(t, r, "POST", "/pm/projects/"+pid+"/tasks", gin.H{"title": "Copy", "status_category": "done"})
	code, res = do(t, r, "PUT", "/pm/tasks/"+tid, gin.H{"status_category": "in_progress"})
	if code != 200 || data(res)["status"] != "In progress" {
		t.Fatalf("update task %d %v", code, res)
	}
	code, res = do(t, r, "GET", "/pm/projects/"+pid, nil)
	stats := data(res)["stats"].(map[string]interface{})
	if stats["total"].(float64) != 2 || stats["done"].(float64) != 1 || stats["progress"].(float64) != 50 || stats["overdue"].(float64) != 1 {
		t.Fatalf("stats = %v", stats)
	}
	code, res = do(t, r, "GET", "/pm/stats", nil)
	tasks := data(res)["tasks"].(map[string]interface{})
	if code != 200 || tasks["open"].(float64) != 1 || tasks["completed_this_month"].(float64) != 1 || tasks["overdue"].(float64) != 1 {
		t.Fatalf("page stats = %v", res)
	}
	if code, _ := do(t, r, "DELETE", "/pm/tasks/"+tid, nil); code != 200 {
		t.Fatalf("delete task %d", code)
	}
	if code, _ := do(t, r, "DELETE", "/pm/projects/"+pid, nil); code != 200 {
		t.Fatalf("delete project %d", code)
	}
	if code, _ := do(t, r, "GET", "/pm/projects/"+pid, nil); code != 404 {
		t.Fatalf("deleted project still there")
	}
}

func TestJiraConnectAndSync(t *testing.T) {
	srv := fakeJira(t, 150, false, nil)
	defer srv.Close()
	r, db, org := setup(t, srv.URL)

	code, res := do(t, r, "PUT", "/pm/jira/connection", gin.H{"site_url": srv.URL, "email": "me@acme.com", "api_token": "secret-token"})
	if code != 200 {
		t.Fatalf("save connection %d %v", code, res)
	}
	view := data(res)
	if view["api_token_masked"] != "••••••••oken" || view["api_token"] != nil {
		t.Fatalf("token leaked or not masked: %v", view)
	}
	raw, _ := json.Marshal(res)
	if bytes.Contains(raw, []byte("secret-token")) {
		t.Fatal("token returned by API")
	}
	// changing email without re-entering token is refused
	if code, _ := do(t, r, "PUT", "/pm/jira/connection", gin.H{"email": "other@acme.com"}); code != 400 {
		t.Fatalf("email change without token accepted: %d", code)
	}
	if code, res := do(t, r, "POST", "/pm/jira/test", nil); code != 200 || data(res)["account"].(map[string]interface{})["display_name"] != "Me" {
		t.Fatalf("test %d %v", code, res)
	}
	if code, res := do(t, r, "POST", "/pm/jira/test", gin.H{"site_url": srv.URL, "email": "me@acme.com", "api_token": "bad"}); code != 200 || data(res)["ok"] != false {
		t.Fatalf("bad token test %d %v", code, res)
	}
	code, res = do(t, r, "GET", "/pm/jira/projects", nil)
	if code != 200 || len(data(res)["projects"].([]interface{})) != 3 {
		t.Fatalf("remote projects %d %v", code, res)
	}
	code, res = do(t, r, "POST", "/pm/jira/sync", gin.H{"project_keys": []string{"ABC", "XYZ", "NOPE"}})
	if code != 200 {
		t.Fatalf("sync %d %v", code, res)
	}
	d := data(res)
	result := d["result"].(map[string]interface{})
	if d["status"] != "partial" || result["created"].(float64) != 150 || result["errors"].(float64) != 1 {
		t.Fatalf("sync result %v", d)
	}
	var abc Project
	if err := db.Where("org_id = ? AND key = ? AND source = ?", org, "ABC", SourceJira).First(&abc).Error; err != nil {
		t.Fatal(err)
	}
	if abc.OwnerName != "Lead A" || abc.ExternalURL != srv.URL+"/browse/ABC" {
		t.Fatalf("project %+v", abc)
	}
	// Jira tasks are read-only
	var one Task
	db.Where("project_id = ?", abc.ID).First(&one)
	if code, _ := do(t, r, "PUT", "/pm/tasks/"+one.ID, gin.H{"title": "x"}); code != 409 {
		t.Fatalf("jira task edit allowed: %d", code)
	}
	// second sync: nothing changes
	code, res = do(t, r, "POST", "/pm/jira/sync", gin.H{"project_keys": []string{"ABC"}})
	result = data(res)["result"].(map[string]interface{})
	if code != 200 || result["created"].(float64) != 0 || result["updated"].(float64) != 0 || data(res)["status"] != "success" {
		t.Fatalf("resync %d %v", code, res)
	}
	// connection shows last sync
	_, res = do(t, r, "GET", "/pm/jira/connection", nil)
	if data(res)["last_sync_status"] != "success" {
		t.Fatalf("connection %v", res)
	}
	// disconnect removes synced data
	if code, _ := do(t, r, "DELETE", "/pm/jira/connection", nil); code != 200 {
		t.Fatal("disconnect failed")
	}
	var n int64
	db.Model(&Task{}).Where("org_id = ?", org).Count(&n)
	if n != 0 {
		t.Fatalf("%d tasks left after disconnect", n)
	}
}
