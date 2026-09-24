package projects2

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Crypto encrypts the stored Jira API token.
type Crypto interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}

// Handler serves the Projects API.
type Handler struct {
	DB     *gorm.DB
	Crypto Crypto
	// HTTP is used for Jira calls; nil means SafeHTTPClient().
	HTTP *http.Client
	// ValidateSite normalises and validates a Jira site URL; nil means NormalizeSiteURL.
	ValidateSite func(string) (string, error)
	Now          func() time.Time

	syncLocks sync.Map // org -> *sync.Mutex
}

// NewHandler builds a handler with production defaults.
func NewHandler(db *gorm.DB, crypto Crypto) *Handler {
	return &Handler{DB: db, Crypto: crypto, Now: time.Now}
}

// RegisterRoutes mounts the API under r (e.g. /api/v1/pm). The caller must
// attach auth middleware that sets "org_id". adminOnly guards credential
// changes (may be nil).
func (h *Handler) RegisterRoutes(r *gin.RouterGroup, adminOnly gin.HandlerFunc) {
	guard := []gin.HandlerFunc{}
	if adminOnly != nil {
		guard = append(guard, adminOnly)
	}
	r.GET("/stats", h.Stats)
	r.GET("/projects", h.ListProjects)
	r.POST("/projects", h.CreateProject)
	r.GET("/projects/:id", h.GetProject)
	r.PUT("/projects/:id", h.UpdateProject)
	r.DELETE("/projects/:id", h.DeleteProject)
	r.GET("/projects/:id/tasks", h.ListTasks)
	r.POST("/projects/:id/tasks", h.CreateTask)
	r.PUT("/tasks/:id", h.UpdateTask)
	r.DELETE("/tasks/:id", h.DeleteTask)

	r.GET("/jira/connection", h.GetConnection)
	r.PUT("/jira/connection", append(guard, h.SaveConnection)...)
	r.DELETE("/jira/connection", append(guard, h.DeleteConnection)...)
	r.POST("/jira/test", h.TestConnection)
	r.GET("/jira/projects", h.RemoteProjects)
	r.POST("/jira/sync", append(guard, h.Sync)...)
}

// ---------- helpers ----------

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func (h *Handler) org(c *gin.Context) (string, bool) {
	org := c.GetString("org_id")
	if org == "" {
		if v, exists := c.Get("org_id"); exists && v != nil {
			org = fmt.Sprint(v)
		}
	}
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
		return "", false
	}
	return org, true
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) today() time.Time {
	n := h.now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func (h *Handler) validateSite(s string) (string, error) {
	if h.ValidateSite != nil {
		return h.ValidateSite(s)
	}
	return NormalizeSiteURL(s)
}

func serverErr(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
}

// parseDateField: nil input = unchanged, "" = clear.
func parseDateField(s *string) (*time.Time, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil, true, nil
	}
	if len(v) >= 10 {
		if t, err := time.Parse("2006-01-02", v[:10]); err == nil {
			return &t, true, nil
		}
	}
	return nil, false, fmt.Errorf("invalid date %q (use YYYY-MM-DD)", v)
}

var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

func deriveKey(name string) string {
	var b strings.Builder
	for _, w := range strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		r := []rune(strings.ToUpper(w))
		if len(r) > 0 && r[0] < unicode.MaxASCII && unicode.IsLetter(r[0]) {
			b.WriteRune(r[0])
		}
	}
	k := b.String()
	if len(k) < 2 {
		clean := strings.Map(func(r rune) rune {
			r = unicode.ToUpper(r)
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				return r
			}
			return -1
		}, name)
		if len(clean) < 2 || clean[0] < 'A' || clean[0] > 'Z' {
			clean = "PRJ" + clean
		}
		k = clean
	}
	if len(k) > 5 {
		k = k[:5]
	}
	return k
}

var categoryLabel = map[string]string{CatTodo: "To do", CatInProgress: "In progress", CatDone: "Done"}

// ---------- stats ----------

type statRow struct {
	ProjectID      string
	StatusCategory string
	N              int
	Overdue        int
}

func (h *Handler) taskStats(org string, projectIDs []string) (map[string]*ProjectStats, error) {
	var rows []statRow
	q := h.DB.Model(&Task{}).
		Select("project_id, status_category, count(*) as n, sum(case when due_date < ? and status_category <> ? then 1 else 0 end) as overdue", h.today(), CatDone).
		Where("org_id = ?", org)
	if projectIDs != nil {
		if len(projectIDs) == 0 {
			return map[string]*ProjectStats{}, nil
		}
		q = q.Where("project_id IN ?", projectIDs)
	}
	if err := q.Group("project_id, status_category").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]*ProjectStats{}
	for _, r := range rows {
		s := out[r.ProjectID]
		if s == nil {
			s = &ProjectStats{}
			out[r.ProjectID] = s
		}
		s.Total += r.N
		s.Overdue += r.Overdue
		switch r.StatusCategory {
		case CatDone:
			s.Done += r.N
		case CatInProgress:
			s.InProgress += r.N
		default:
			s.Todo += r.N
		}
	}
	for _, s := range out {
		if s.Total > 0 {
			s.Progress = int(float64(s.Done) / float64(s.Total) * 100)
		}
	}
	return out, nil
}

func (h *Handler) attachStats(org string, projects []Project) error {
	ids := make([]string, len(projects))
	for i := range projects {
		ids[i] = projects[i].ID
	}
	st, err := h.taskStats(org, ids)
	if err != nil {
		return err
	}
	for i := range projects {
		if s, ok := st[projects[i].ID]; ok {
			projects[i].Stats = s
		} else {
			projects[i].Stats = &ProjectStats{}
		}
	}
	return nil
}

// Stats returns KPIs for the Projects page.
func (h *Handler) Stats(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var projects []Project
	if err := h.DB.Where("org_id = ?", org).Order("name").Find(&projects).Error; err != nil {
		serverErr(c, err)
		return
	}
	if err := h.attachStats(org, projects); err != nil {
		serverErr(c, err)
		return
	}
	byStatus := map[string]int{StatusPlanned: 0, StatusActive: 0, StatusOnHold: 0, StatusCompleted: 0}
	bySource := map[string]int{SourceLocal: 0, SourceJira: 0}
	tasks := map[string]int{"total": 0, CatTodo: 0, CatInProgress: 0, CatDone: 0, "open": 0, "overdue": 0}
	perProject := make([]gin.H, 0, len(projects))
	for _, p := range projects {
		byStatus[p.Status]++
		bySource[p.Source]++
		s := p.Stats
		tasks["total"] += s.Total
		tasks[CatTodo] += s.Todo
		tasks[CatInProgress] += s.InProgress
		tasks[CatDone] += s.Done
		tasks["overdue"] += s.Overdue
		perProject = append(perProject, gin.H{"project_id": p.ID, "key": p.Key, "name": p.Name, "source": p.Source, "status": p.Status,
			"total": s.Total, "done": s.Done, "in_progress": s.InProgress, "todo": s.Todo, "overdue": s.Overdue, "progress": s.Progress})
	}
	tasks["open"] = tasks[CatTodo] + tasks[CatInProgress]

	n := h.now()
	monthStart := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, n.Location())
	var completedThisMonth int64
	if err := h.DB.Model(&Task{}).Where("org_id = ? AND status_category = ? AND completed_at >= ?", org, CatDone, monthStart).Count(&completedThisMonth).Error; err != nil {
		serverErr(c, err)
		return
	}
	tasks["completed_this_month"] = int(completedThisMonth)

	jira := gin.H{"connected": false}
	var conn JiraConnection
	if err := h.DB.Where("org_id = ?", org).First(&conn).Error; err == nil {
		jira = gin.H{"connected": true, "site_url": conn.SiteURL, "last_sync_at": conn.LastSyncAt, "last_sync_status": conn.LastSyncStatus}
	}
	ok(c, http.StatusOK, gin.H{
		"projects":    gin.H{"total": len(projects), "by_status": byStatus, "by_source": bySource, "active": byStatus[StatusActive]},
		"tasks":       tasks,
		"per_project": perProject,
		"jira":        jira,
	})
}

// ---------- projects ----------

func (h *Handler) findProject(org, id string) (*Project, error) {
	var p Project
	if err := h.DB.Where("org_id = ? AND id = ?", org, id).First(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// ListProjects lists projects with progress. Filters: status, source, q.
func (h *Handler) ListProjects(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	q := h.DB.Where("org_id = ?", org)
	if s := c.Query("status"); s != "" && s != "all" {
		q = q.Where("status = ?", s)
	}
	if s := c.Query("source"); s != "" && s != "all" {
		q = q.Where("source = ?", s)
	}
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		like := "%" + strings.ToLower(s) + "%"
		q = q.Where("LOWER(name) LIKE ? OR LOWER(key) LIKE ? OR LOWER(description) LIKE ? OR LOWER(owner_name) LIKE ?", like, like, like, like)
	}
	var projects []Project
	if err := q.Order("CASE status WHEN 'active' THEN 0 WHEN 'planned' THEN 1 WHEN 'on_hold' THEN 2 ELSE 3 END, updated_at DESC").Find(&projects).Error; err != nil {
		serverErr(c, err)
		return
	}
	if err := h.attachStats(org, projects); err != nil {
		serverErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"projects": projects, "total": len(projects)})
}

// GetProject returns one project with progress.
func (h *Handler) GetProject(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	p, err := h.findProject(org, c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fail(c, http.StatusNotFound, "NOT_FOUND", "Project not found")
			return
		}
		serverErr(c, err)
		return
	}
	list := []Project{*p}
	if err := h.attachStats(org, list); err != nil {
		serverErr(c, err)
		return
	}
	ok(c, http.StatusOK, list[0])
}

type projectInput struct {
	Name        *string  `json:"name"`
	Key         *string  `json:"key"`
	Description *string  `json:"description"`
	Status      *string  `json:"status"`
	StartDate   *string  `json:"start_date"`
	DueDate     *string  `json:"due_date"`
	Budget      *float64 `json:"budget"`
	OwnerName   *string  `json:"owner_name"`
}

func (h *Handler) applyProject(p *Project, in projectInput, creating bool) error {
	local := p.Source != SourceJira
	if in.Name != nil && local {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if local && strings.TrimSpace(p.Name) == "" {
		return errors.New("Project name is required")
	}
	if len(p.Name) > 255 {
		return errors.New("Project name is too long")
	}
	if in.Key != nil && local && creating {
		p.Key = strings.ToUpper(strings.TrimSpace(*in.Key))
	}
	if in.Description != nil && local {
		p.Description = strings.TrimSpace(*in.Description)
	}
	if in.Status != nil {
		s := strings.TrimSpace(*in.Status)
		if !validProjectStatus[s] {
			return errors.New("Status must be planned, active, on_hold or completed")
		}
		p.Status = s
	}
	if d, set, err := parseDateField(in.StartDate); err != nil {
		return err
	} else if set {
		p.StartDate = d
	}
	if d, set, err := parseDateField(in.DueDate); err != nil {
		return err
	} else if set {
		p.DueDate = d
	}
	if p.StartDate != nil && p.DueDate != nil && p.DueDate.Before(*p.StartDate) {
		return errors.New("Due date must be on or after the start date")
	}
	if in.Budget != nil {
		if *in.Budget < 0 {
			return errors.New("Budget cannot be negative")
		}
		p.Budget = *in.Budget
	}
	if in.OwnerName != nil {
		p.OwnerName = strings.TrimSpace(*in.OwnerName)
	}
	return nil
}

func (h *Handler) keyTaken(org, key, exceptID string) (bool, error) {
	var n int64
	q := h.DB.Model(&Project{}).Where("org_id = ? AND key = ?", org, key)
	if exceptID != "" {
		q = q.Where("id <> ?", exceptID)
	}
	err := q.Count(&n).Error
	return n > 0, err
}

// CreateProject creates a local project.
func (h *Handler) CreateProject(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in projectInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_INPUT", "Invalid request body")
		return
	}
	p := Project{ID: uuid.NewString(), OrgID: org, Status: StatusActive, Source: SourceLocal, NextTaskNumber: 1}
	if err := h.applyProject(&p, in, true); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	if p.Key == "" {
		base := deriveKey(p.Name)
		p.Key = base
		for i := 2; i < 100; i++ {
			taken, err := h.keyTaken(org, p.Key, "")
			if err != nil {
				serverErr(c, err)
				return
			}
			if !taken {
				break
			}
			p.Key = fmt.Sprintf("%s%d", base, i)
		}
	} else if !keyRe.MatchString(p.Key) {
		fail(c, http.StatusBadRequest, "VALIDATION", "Key must be 2–10 letters or digits, starting with a letter (e.g. WEB)")
		return
	} else if taken, err := h.keyTaken(org, p.Key, ""); err != nil {
		serverErr(c, err)
		return
	} else if taken {
		fail(c, http.StatusConflict, "KEY_TAKEN", "Another project already uses the key "+p.Key)
		return
	}
	if err := h.DB.Create(&p).Error; err != nil {
		serverErr(c, err)
		return
	}
	p.Stats = &ProjectStats{}
	ok(c, http.StatusCreated, p)
}

// UpdateProject edits a project. Jira projects only accept local planning
// fields (status, dates, budget, owner); name/description come from Jira.
func (h *Handler) UpdateProject(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	p, err := h.findProject(org, c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fail(c, http.StatusNotFound, "NOT_FOUND", "Project not found")
			return
		}
		serverErr(c, err)
		return
	}
	var in projectInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_INPUT", "Invalid request body")
		return
	}
	in.Key = nil // keys are immutable (task keys depend on them)
	if err := h.applyProject(p, in, false); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	if err := h.DB.Save(p).Error; err != nil {
		serverErr(c, err)
		return
	}
	list := []Project{*p}
	_ = h.attachStats(org, list)
	ok(c, http.StatusOK, list[0])
}

// DeleteProject removes a project and its tasks. For a Jira project this only
// removes the local copy and stops syncing it.
func (h *Handler) DeleteProject(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	p, err := h.findProject(org, c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fail(c, http.StatusNotFound, "NOT_FOUND", "Project not found")
			return
		}
		serverErr(c, err)
		return
	}
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("org_id = ? AND project_id = ?", org, p.ID).Delete(&Task{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(p).Error; err != nil {
			return err
		}
		if p.Source == SourceJira {
			var conn JiraConnection
			if err := tx.Where("org_id = ?", org).First(&conn).Error; err == nil {
				keys := splitKeys(conn.ProjectKeys)
				kept := keys[:0]
				for _, k := range keys {
					if k != p.Key {
						kept = append(kept, k)
					}
				}
				return tx.Model(&conn).Update("project_keys", strings.Join(kept, ",")).Error
			}
		}
		return nil
	})
	if err != nil {
		serverErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

// ---------- tasks ----------

// ListTasks lists a project's tasks. Filters: status_category, assignee
// ("unassigned" for none), type, q.
func (h *Handler) ListTasks(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	p, err := h.findProject(org, c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fail(c, http.StatusNotFound, "NOT_FOUND", "Project not found")
			return
		}
		serverErr(c, err)
		return
	}
	var all []Task
	if err := h.DB.Where("org_id = ? AND project_id = ?", org, p.ID).Order("updated_at DESC").Find(&all).Error; err != nil {
		serverErr(c, err)
		return
	}
	assigneeSet := map[string]bool{}
	typeSet := map[string]bool{}
	for _, t := range all {
		if t.AssigneeName != "" {
			assigneeSet[t.AssigneeName] = true
		}
		if t.Type != "" {
			typeSet[t.Type] = true
		}
	}
	cat := c.Query("status_category")
	assignee := c.Query("assignee")
	typ := c.Query("type")
	search := strings.ToLower(strings.TrimSpace(c.Query("q")))
	tasks := make([]Task, 0, len(all))
	for _, t := range all {
		if cat != "" && cat != "all" && t.StatusCategory != cat {
			continue
		}
		if assignee == "unassigned" && t.AssigneeName != "" {
			continue
		}
		if assignee != "" && assignee != "all" && assignee != "unassigned" && t.AssigneeName != assignee {
			continue
		}
		if typ != "" && typ != "all" && t.Type != typ {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(t.Title+" "+t.Key+" "+t.AssigneeName), search) {
			continue
		}
		tasks = append(tasks, t)
	}
	ok(c, http.StatusOK, gin.H{"tasks": tasks, "total": len(tasks), "assignees": sortedKeys(assigneeSet), "types": sortedKeys(typeSet)})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type taskInput struct {
	Title          *string  `json:"title"`
	Description    *string  `json:"description"`
	StatusCategory *string  `json:"status_category"`
	Type           *string  `json:"type"`
	Priority       *string  `json:"priority"`
	AssigneeName   *string  `json:"assignee_name"`
	AssigneeEmail  *string  `json:"assignee_email"`
	DueDate        *string  `json:"due_date"`
	StoryPoints    *float64 `json:"story_points"`
	ClearPoints    bool     `json:"clear_story_points"`
}

var validPriority = map[string]string{"highest": "Highest", "high": "High", "medium": "Medium", "low": "Low", "lowest": "Lowest"}

func (h *Handler) applyTask(t *Task, in taskInput) error {
	if in.Title != nil {
		t.Title = strings.TrimSpace(*in.Title)
	}
	if t.Title == "" {
		return errors.New("Task title is required")
	}
	if len(t.Title) > 512 {
		return errors.New("Task title is too long")
	}
	if in.Description != nil {
		t.Description = strings.TrimSpace(*in.Description)
	}
	if in.StatusCategory != nil {
		cat := strings.TrimSpace(*in.StatusCategory)
		if !validCategory[cat] {
			return errors.New("Status must be todo, in_progress or done")
		}
		if cat == CatDone && t.StatusCategory != CatDone {
			n := h.now().UTC()
			t.CompletedAt = &n
		} else if cat != CatDone {
			t.CompletedAt = nil
		}
		t.StatusCategory = cat
	}
	if t.StatusCategory == "" {
		t.StatusCategory = CatTodo
	}
	t.Status = categoryLabel[t.StatusCategory]
	if in.Type != nil {
		t.Type = strings.TrimSpace(*in.Type)
	}
	if t.Type == "" {
		t.Type = "Task"
	}
	if in.Priority != nil {
		p := strings.ToLower(strings.TrimSpace(*in.Priority))
		if p == "" {
			t.Priority = ""
		} else if label, ok := validPriority[p]; ok {
			t.Priority = label
		} else {
			return errors.New("Priority must be highest, high, medium, low or lowest")
		}
	}
	if in.AssigneeName != nil {
		t.AssigneeName = strings.TrimSpace(*in.AssigneeName)
	}
	if in.AssigneeEmail != nil {
		e := strings.TrimSpace(*in.AssigneeEmail)
		if e != "" && !strings.Contains(e, "@") {
			return errors.New("Assignee email is not valid")
		}
		t.AssigneeEmail = e
	}
	if d, set, err := parseDateField(in.DueDate); err != nil {
		return err
	} else if set {
		t.DueDate = d
	}
	if in.ClearPoints {
		t.StoryPoints = nil
	} else if in.StoryPoints != nil {
		if *in.StoryPoints < 0 || *in.StoryPoints > 1000 {
			return errors.New("Story points must be between 0 and 1000")
		}
		v := *in.StoryPoints
		t.StoryPoints = &v
	}
	return nil
}

// CreateTask adds a task to a local project; its key is PROJECTKEY-n.
func (h *Handler) CreateTask(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in taskInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_INPUT", "Invalid request body")
		return
	}
	var created Task
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		var p Project
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("org_id = ? AND id = ?", org, c.Param("id")).First(&p).Error; err != nil {
			return err
		}
		if p.Source == SourceJira {
			return &httpErr{http.StatusConflict, "JIRA_READ_ONLY", "Tasks in Jira projects are synced from Jira — create the issue in Jira, then sync."}
		}
		t := Task{ID: uuid.NewString(), OrgID: org, ProjectID: p.ID, Source: SourceLocal}
		if err := h.applyTask(&t, in); err != nil {
			return &httpErr{http.StatusBadRequest, "VALIDATION", err.Error()}
		}
		n := p.NextTaskNumber
		if n < 1 {
			n = 1
		}
		t.Key = fmt.Sprintf("%s-%d", p.Key, n)
		if err := tx.Model(&p).Update("next_task_number", n+1).Error; err != nil {
			return err
		}
		created = t
		return tx.Create(&created).Error
	})
	if err != nil {
		h.txFail(c, err, "Project not found")
		return
	}
	ok(c, http.StatusCreated, created)
}

type httpErr struct {
	status int
	code   string
	msg    string
}

func (e *httpErr) Error() string { return e.msg }

func (h *Handler) txFail(c *gin.Context, err error, notFound string) {
	var he *httpErr
	if errors.As(err, &he) {
		fail(c, he.status, he.code, he.msg)
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", notFound)
		return
	}
	serverErr(c, err)
}

func (h *Handler) findLocalTask(org, id string) (*Task, error) {
	var t Task
	if err := h.DB.Where("org_id = ? AND id = ?", org, id).First(&t).Error; err != nil {
		return nil, err
	}
	if t.Source == SourceJira {
		return nil, &httpErr{http.StatusConflict, "JIRA_READ_ONLY", "This issue is synced from Jira — edit it in Jira."}
	}
	return &t, nil
}

// UpdateTask edits a local task (also used to move it between board columns).
func (h *Handler) UpdateTask(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	t, err := h.findLocalTask(org, c.Param("id"))
	if err != nil {
		h.txFail(c, err, "Task not found")
		return
	}
	var in taskInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_INPUT", "Invalid request body")
		return
	}
	if err := h.applyTask(t, in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	if err := h.DB.Save(t).Error; err != nil {
		serverErr(c, err)
		return
	}
	ok(c, http.StatusOK, t)
}

// DeleteTask deletes a local task.
func (h *Handler) DeleteTask(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	t, err := h.findLocalTask(org, c.Param("id"))
	if err != nil {
		h.txFail(c, err, "Task not found")
		return
	}
	if err := h.DB.Delete(t).Error; err != nil {
		serverErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}
