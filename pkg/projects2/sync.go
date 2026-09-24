package projects2

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func splitKeys(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range strings.Split(s, ",") {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func maskToken(last4 string) string {
	if last4 == "" {
		return ""
	}
	return "••••••••" + last4
}

// SyncResult summarises one sync run.
type SyncResult struct {
	Projects []ProjectSyncResult `json:"projects"`
	Issues   int                 `json:"issues"`
	Created  int                 `json:"created"`
	Updated  int                 `json:"updated"`
	Removed  int                 `json:"removed"`
	Errors   int                 `json:"errors"`
	Duration float64             `json:"duration_seconds"`
}

// ProjectSyncResult is the outcome for one Jira project.
type ProjectSyncResult struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	ProjectID string `json:"project_id,omitempty"`
	Issues    int    `json:"issues"`
	Created   int    `json:"created"`
	Updated   int    `json:"updated"`
	Removed   int    `json:"removed"`
	Error     string `json:"error,omitempty"`
}

func (h *Handler) connectionView(conn *JiraConnection) gin.H {
	if conn == nil {
		return gin.H{"connected": false, "project_keys": []string{}}
	}
	var stats interface{}
	if conn.LastSyncStats != "" {
		var r SyncResult
		if json.Unmarshal([]byte(conn.LastSyncStats), &r) == nil {
			stats = r
		}
	}
	keys := splitKeys(conn.ProjectKeys)
	if keys == nil {
		keys = []string{}
	}
	return gin.H{
		"connected":         true,
		"site_url":          conn.SiteURL,
		"email":             conn.Email,
		"api_token_masked":  maskToken(conn.TokenLast4),
		"has_token":         conn.TokenEnc != "",
		"default_jql":       conn.DefaultJQL,
		"project_keys":      keys,
		"account_name":      conn.AccountName,
		"last_sync_at":      conn.LastSyncAt,
		"last_sync_status":  conn.LastSyncStatus,
		"last_sync_message": conn.LastSyncMessage,
		"last_sync_stats":   stats,
		"updated_at":        conn.UpdatedAt,
	}
}

func (h *Handler) loadConnection(org string) (*JiraConnection, error) {
	var conn JiraConnection
	err := h.DB.Where("org_id = ?", org).First(&conn).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &conn, nil
}

func (h *Handler) decryptToken(conn *JiraConnection) (string, error) {
	if conn == nil || conn.TokenEnc == "" {
		return "", errors.New("no API token saved")
	}
	if h.Crypto == nil {
		return conn.TokenEnc, nil
	}
	return h.Crypto.Decrypt(conn.TokenEnc)
}

func (h *Handler) client(site, email, token string) *JiraClient {
	return &JiraClient{BaseURL: site, Email: email, Token: token, HTTP: h.HTTP}
}

// GetConnection returns the org's Jira connection (token masked).
func (h *Handler) GetConnection(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	conn, err := h.loadConnection(org)
	if err != nil {
		serverErr(c, err)
		return
	}
	ok(c, http.StatusOK, h.connectionView(conn))
}

type connectionInput struct {
	SiteURL     *string   `json:"site_url"`
	Email       *string   `json:"email"`
	APIToken    string    `json:"api_token"`
	DefaultJQL  *string   `json:"default_jql"`
	ProjectKeys *[]string `json:"project_keys"`
}

// resolveCredentials merges input with the stored connection. When the site
// or email changes, a fresh token is required so a saved token can never be
// redirected to a different host or account.
func (h *Handler) resolveCredentials(conn *JiraConnection, in connectionInput) (site, email, token string, err error) {
	if conn != nil {
		site, email = conn.SiteURL, conn.Email
	}
	if in.SiteURL != nil {
		if site, err = h.validateSite(*in.SiteURL); err != nil {
			return
		}
	} else if site == "" {
		err = errors.New("Site URL is required, e.g. https://your-team.atlassian.net")
		return
	}
	if in.Email != nil {
		email = strings.TrimSpace(*in.Email)
	}
	if _, perr := mail.ParseAddress(email); perr != nil || email == "" {
		err = errors.New("Enter the email address of your Atlassian account")
		return
	}
	token = strings.TrimSpace(in.APIToken)
	if token == "" {
		if conn == nil || conn.TokenEnc == "" {
			err = errors.New("API token is required")
			return
		}
		if !strings.EqualFold(site, conn.SiteURL) || !strings.EqualFold(email, conn.Email) {
			err = errors.New("Re-enter the API token when changing the site or email")
			return
		}
		if token, err = h.decryptToken(conn); err != nil {
			err = errors.New("The saved API token could not be read — please enter it again")
			return
		}
	}
	if len(token) > 1024 {
		err = errors.New("API token is too long")
	}
	return
}

// SaveConnection creates or updates the Jira connection.
func (h *Handler) SaveConnection(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in connectionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_INPUT", "Invalid request body")
		return
	}
	conn, err := h.loadConnection(org)
	if err != nil {
		serverErr(c, err)
		return
	}
	site, email, token, err := h.resolveCredentials(conn, in)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	if conn == nil {
		conn = &JiraConnection{OrgID: org}
	}
	if !strings.EqualFold(conn.SiteURL, site) || !strings.EqualFold(conn.Email, email) {
		conn.AccountName = ""
	}
	conn.SiteURL, conn.Email = site, email
	if strings.TrimSpace(in.APIToken) != "" {
		enc := token
		if h.Crypto != nil {
			if enc, err = h.Crypto.Encrypt(token); err != nil {
				serverErr(c, err)
				return
			}
		}
		conn.TokenEnc = enc
		if len(token) >= 4 {
			conn.TokenLast4 = token[len(token)-4:]
		} else {
			conn.TokenLast4 = ""
		}
	}
	if in.DefaultJQL != nil {
		conn.DefaultJQL = strings.TrimSpace(*in.DefaultJQL)
	}
	if in.ProjectKeys != nil {
		conn.ProjectKeys = strings.Join(splitKeys(strings.Join(*in.ProjectKeys, ",")), ",")
	}
	if err := h.DB.Save(conn).Error; err != nil {
		serverErr(c, err)
		return
	}
	ok(c, http.StatusOK, h.connectionView(conn))
}

// DeleteConnection disconnects Jira. Synced Jira projects and issues are
// removed too unless ?keep_data=1. Nothing is changed in Jira.
func (h *Handler) DeleteConnection(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	keep := c.Query("keep_data") == "1" || c.Query("keep_data") == "true"
	removed := int64(0)
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("org_id = ?", org).Delete(&JiraConnection{}).Error; err != nil {
			return err
		}
		if keep {
			return nil
		}
		if err := tx.Where("org_id = ? AND source = ?", org, SourceJira).Delete(&Task{}).Error; err != nil {
			return err
		}
		res := tx.Where("org_id = ? AND source = ?", org, SourceJira).Delete(&Project{})
		removed = res.RowsAffected
		return res.Error
	})
	if err != nil {
		serverErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"disconnected": true, "removed_projects": removed})
}

func jiraFail(c *gin.Context, err error) {
	// Never surface Jira's 401 as ours (the client would log the user out).
	fail(c, http.StatusBadGateway, "JIRA_ERROR", err.Error())
}

// TestConnection checks credentials against GET /rest/api/3/myself. The body
// may carry unsaved values; missing ones fall back to the stored connection.
func (h *Handler) TestConnection(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in connectionInput
	_ = c.ShouldBindJSON(&in)
	conn, err := h.loadConnection(org)
	if err != nil {
		serverErr(c, err)
		return
	}
	site, email, token, err := h.resolveCredentials(conn, in)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	me, err := h.client(site, email, token).Myself(ctx)
	if err != nil {
		// A failed test is a result, not a server error.
		ok(c, http.StatusOK, gin.H{"ok": false, "site_url": site, "message": err.Error()})
		return
	}
	if conn != nil && strings.EqualFold(conn.SiteURL, site) && strings.EqualFold(conn.Email, email) {
		h.DB.Model(conn).Update("account_name", me.DisplayName)
	}
	ok(c, http.StatusOK, gin.H{"ok": true, "site_url": site, "account": gin.H{"account_id": me.AccountID, "display_name": me.DisplayName, "email": me.EmailAddress}})
}

func (h *Handler) savedClient(c *gin.Context, org string) (*JiraConnection, *JiraClient, bool) {
	conn, err := h.loadConnection(org)
	if err != nil {
		serverErr(c, err)
		return nil, nil, false
	}
	if conn == nil {
		fail(c, http.StatusBadRequest, "NOT_CONNECTED", "Connect Jira first")
		return nil, nil, false
	}
	// Re-validate in case the rules tightened since it was saved.
	site, err := h.validateSite(conn.SiteURL)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return nil, nil, false
	}
	token, err := h.decryptToken(conn)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "The saved API token could not be read — please enter it again")
		return nil, nil, false
	}
	return conn, h.client(site, conn.Email, token), true
}

// RemoteProjects lists the Jira projects visible to the connected account.
func (h *Handler) RemoteProjects(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	conn, client, okk := h.savedClient(c, org)
	if !okk {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	remote, err := client.ListProjects(ctx)
	if err != nil {
		jiraFail(c, err)
		return
	}
	selected := map[string]bool{}
	for _, k := range splitKeys(conn.ProjectKeys) {
		selected[k] = true
	}
	var local []Project
	h.DB.Where("org_id = ? AND source = ?", org, SourceJira).Find(&local)
	synced := map[string]*Project{}
	for i := range local {
		synced[local[i].ExternalID] = &local[i]
	}
	out := make([]gin.H, 0, len(remote))
	for _, rp := range remote {
		row := gin.H{"id": rp.ID, "key": rp.Key, "name": rp.Name, "description": rp.Description, "type": rp.ProjectTypeKey,
			"lead": rp.Lead.DisplayName, "selected": selected[strings.ToUpper(rp.Key)], "synced": false,
			"url": strings.TrimRight(client.BaseURL, "/") + "/browse/" + rp.Key}
		if lp, found := synced[rp.ID]; found {
			row["synced"] = true
			row["project_id"] = lp.ID
			row["last_synced_at"] = lp.LastSyncedAt
		}
		out = append(out, row)
	}
	ok(c, http.StatusOK, gin.H{"projects": out, "total": len(out)})
}

func (h *Handler) orgLock(org string) *sync.Mutex {
	m, _ := h.syncLocks.LoadOrStore(org, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// Sync pulls the selected Jira projects and their issues (read-only).
// Body: {"project_keys": ["ABC"]} (optional; saved as the new selection).
func (h *Handler) Sync(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in struct {
		ProjectKeys *[]string `json:"project_keys"`
	}
	_ = c.ShouldBindJSON(&in)
	conn, client, okk := h.savedClient(c, org)
	if !okk {
		return
	}
	if in.ProjectKeys != nil {
		conn.ProjectKeys = strings.Join(splitKeys(strings.Join(*in.ProjectKeys, ",")), ",")
		if err := h.DB.Model(conn).Update("project_keys", conn.ProjectKeys).Error; err != nil {
			serverErr(c, err)
			return
		}
	}
	keys := splitKeys(conn.ProjectKeys)
	if len(keys) == 0 {
		fail(c, http.StatusBadRequest, "NO_PROJECTS", "Choose at least one Jira project to sync")
		return
	}
	lock := h.orgLock(org)
	if !lock.TryLock() {
		fail(c, http.StatusConflict, "SYNC_RUNNING", "A sync is already running — try again shortly")
		return
	}
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 4*time.Minute)
	defer cancel()
	result, fatal := h.runSync(ctx, org, client, conn.DefaultJQL, keys)

	now := h.now().UTC()
	status, msg := "success", ""
	switch {
	case fatal != nil:
		status, msg = "error", fatal.Error()
	case result.Errors > 0 && result.Errors == len(result.Projects):
		status, msg = "error", firstError(result)
	case result.Errors > 0:
		status, msg = "partial", firstError(result)
	}
	statsJSON, _ := json.Marshal(result)
	h.DB.Model(&JiraConnection{}).Where("org_id = ?", org).Updates(map[string]interface{}{
		"last_sync_at": now, "last_sync_status": status, "last_sync_message": msg, "last_sync_stats": string(statsJSON),
	})
	if fatal != nil {
		jiraFail(c, fatal)
		return
	}
	ok(c, http.StatusOK, gin.H{"status": status, "message": msg, "result": result, "last_sync_at": now})
}

func firstError(r SyncResult) string {
	for _, p := range r.Projects {
		if p.Error != "" {
			return p.Key + ": " + p.Error
		}
	}
	return ""
}

func (h *Handler) runSync(ctx context.Context, org string, client *JiraClient, filter string, keys []string) (SyncResult, error) {
	start := time.Now()
	result := SyncResult{Projects: []ProjectSyncResult{}}
	remote, err := client.ListProjects(ctx)
	if err != nil {
		return result, err
	}
	byKey := map[string]RemoteProject{}
	for _, rp := range remote {
		byKey[strings.ToUpper(rp.Key)] = rp
	}
	spFields := client.StoryPointFields(ctx)
	for _, key := range keys {
		pr := ProjectSyncResult{Key: key}
		rp, found := byKey[key]
		if !found {
			pr.Error = "project not found in Jira or no access"
		} else {
			pr.Name = rp.Name
			if err := h.syncProject(ctx, org, client, rp, filter, spFields, &pr); err != nil {
				pr.Error = err.Error()
			}
		}
		if pr.Error != "" {
			result.Errors++
		}
		result.Issues += pr.Issues
		result.Created += pr.Created
		result.Updated += pr.Updated
		result.Removed += pr.Removed
		result.Projects = append(result.Projects, pr)
	}
	result.Duration = time.Since(start).Round(10 * time.Millisecond).Seconds()
	return result, nil
}

func sameTask(a, b *Task) bool {
	eqTime := func(x, y *time.Time) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return x.Equal(*y)
	}
	eqF := func(x, y *float64) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return *x == *y
	}
	return a.Key == b.Key && a.Title == b.Title && a.Status == b.Status && a.StatusCategory == b.StatusCategory &&
		a.Type == b.Type && a.Priority == b.Priority && a.AssigneeName == b.AssigneeName && a.AssigneeEmail == b.AssigneeEmail &&
		a.URL == b.URL && eqTime(a.DueDate, b.DueDate) && eqTime(a.RemoteUpdatedAt, b.RemoteUpdatedAt) &&
		eqTime(a.CompletedAt, b.CompletedAt) && eqF(a.StoryPoints, b.StoryPoints)
}

func (h *Handler) syncProject(ctx context.Context, org string, client *JiraClient, rp RemoteProject, filter string, spFields []string, pr *ProjectSyncResult) error {
	issues, err := client.SearchIssues(ctx, BuildJQL(rp.Key, filter), spFields)
	if err != nil {
		return err
	}
	pr.Issues = len(issues)
	site := strings.TrimRight(client.BaseURL, "/")
	now := h.now().UTC()
	return h.DB.Transaction(func(tx *gorm.DB) error {
		var p Project
		err := tx.Where("org_id = ? AND source = ? AND external_id = ?", org, SourceJira, rp.ID).First(&p).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			p = Project{ID: uuid.NewString(), OrgID: org, Source: SourceJira, ExternalID: rp.ID, Status: StatusActive, NextTaskNumber: 1}
		} else if err != nil {
			return err
		}
		p.Key = rp.Key
		p.Name = rp.Name
		if p.Name == "" {
			p.Name = rp.Key
		}
		p.Description = strings.TrimSpace(rp.Description)
		p.ExternalURL = site + "/browse/" + rp.Key
		if p.OwnerName == "" {
			p.OwnerName = rp.Lead.DisplayName
		}
		p.LastSyncedAt = &now
		if err := tx.Save(&p).Error; err != nil {
			return err
		}
		pr.ProjectID = p.ID

		var existing []Task
		if err := tx.Where("org_id = ? AND project_id = ? AND source = ?", org, p.ID, SourceJira).Find(&existing).Error; err != nil {
			return err
		}
		byExt := make(map[string]*Task, len(existing))
		for i := range existing {
			byExt[existing[i].ExternalID] = &existing[i]
		}
		seen := make(map[string]bool, len(issues))
		var toCreate []Task
		for _, ri := range issues {
			m := MapIssue(ri, site, spFields)
			if seen[m.ExternalID] {
				continue
			}
			seen[m.ExternalID] = true
			if old, found := byExt[m.ExternalID]; found {
				if sameTask(old, &m) {
					continue
				}
				m.ID, m.OrgID, m.ProjectID, m.CreatedAt, m.Description = old.ID, org, p.ID, old.CreatedAt, old.Description
				if err := tx.Save(&m).Error; err != nil {
					return err
				}
				pr.Updated++
				continue
			}
			m.ID, m.OrgID, m.ProjectID = uuid.NewString(), org, p.ID
			toCreate = append(toCreate, m)
		}
		if len(toCreate) > 0 {
			if err := tx.CreateInBatches(toCreate, 200).Error; err != nil {
				return err
			}
			pr.Created = len(toCreate)
		}
		var stale []string
		for ext, t := range byExt {
			if !seen[ext] {
				stale = append(stale, t.ID)
			}
		}
		if len(stale) > 0 {
			sort.Strings(stale)
			if err := tx.Where("org_id = ? AND id IN ?", org, stale).Delete(&Task{}).Error; err != nil {
				return err
			}
			pr.Removed = len(stale)
		}
		return nil
	})
}
