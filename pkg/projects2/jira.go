package projects2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------- site URL validation (SSRF guard) ----------

var errBlockedAddress = errors.New("the Jira site resolves to a private or internal address")

// NormalizeSiteURL validates a user-supplied Jira site and returns it as
// "https://host[:port]". Only https is accepted, credentials/IP literals and
// internal-looking hostnames are rejected. Private addresses reached through
// DNS are blocked again at dial time (see SafeHTTPClient).
func NormalizeSiteURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("Site URL is required, e.g. https://your-team.atlassian.net")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", errors.New("Site URL is not a valid URL")
	}
	if strings.ToLower(u.Scheme) != "https" {
		return "", errors.New("Site URL must use https://")
	}
	if u.User != nil {
		return "", errors.New("Site URL must not contain credentials")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" && port != "443" {
		return "", errors.New("Site URL must use the standard https port")
	}
	if net.ParseIP(host) != nil {
		return "", errors.New("Use your Jira site's hostname, not an IP address")
	}
	if !strings.Contains(host, ".") || host == "localhost" {
		return "", errors.New("Site URL must be a public hostname")
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home", ".corp", ".intranet"} {
		if strings.HasSuffix(host, suffix) {
			return "", errors.New("Site URL must be a public hostname")
		}
	}
	if !regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`).MatchString(host) {
		return "", errors.New("Site URL has an invalid hostname")
	}
	return "https://" + host, nil
}

func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		// carrier-grade NAT 100.64.0.0/10
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xC0 == 64)
}

// SafeHTTPClient refuses to connect to private, loopback or link-local
// addresses (checked on the resolved IP at dial time, so DNS tricks do not
// bypass it), does not follow redirects and has conservative timeouts.
func SafeHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || isBlockedIP(ip) {
				return errBlockedAddress
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   45 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ---------- client ----------

// JiraClient talks to the Jira Cloud REST API v3 with basic auth (email:token).
type JiraClient struct {
	BaseURL string
	Email   string
	Token   string
	HTTP    *http.Client
}

// JiraError is a failed Jira call.
type JiraError struct {
	Status  int
	Message string
}

func (e *JiraError) Error() string { return e.Message }

func (c *JiraClient) get(ctx context.Context, path string, q url.Values, out interface{}) error {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Email, c.Token)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = SafeHTTPClient()
	}
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(err, errBlockedAddress) {
			return &JiraError{Status: 0, Message: errBlockedAddress.Error()}
		}
		return &JiraError{Status: 0, Message: "Could not reach Jira: " + friendlyNetErr(err)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &JiraError{Status: resp.StatusCode, Message: jiraErrorMessage(resp.StatusCode, body)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &JiraError{Status: resp.StatusCode, Message: "Unexpected response from Jira (is this a Jira Cloud site?)"}
	}
	return nil
}

func friendlyNetErr(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the site name could not be resolved"
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Timeout") {
		return "the request timed out"
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return msg
}

func jiraErrorMessage(status int, body []byte) string {
	var parsed struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
		Message       string            `json:"message"`
	}
	_ = json.Unmarshal(body, &parsed)
	detail := strings.Join(parsed.ErrorMessages, " ")
	for _, v := range parsed.Errors {
		detail = strings.TrimSpace(detail + " " + v)
	}
	if detail == "" {
		detail = parsed.Message
	}
	switch status {
	case http.StatusUnauthorized:
		return "Jira rejected the email or API token (401). Check both and try again."
	case http.StatusForbidden:
		if detail == "" {
			detail = "the account does not have permission"
		}
		return "Jira denied access (403): " + detail
	case http.StatusNotFound:
		if detail == "" {
			return "Not found (404) — check the site URL is your Jira Cloud site."
		}
	case http.StatusTooManyRequests:
		return "Jira rate limit reached (429). Try again in a minute."
	}
	if status >= 300 && status < 400 {
		return fmt.Sprintf("Jira redirected the request (%d) — check the site URL.", status)
	}
	if detail == "" {
		detail = http.StatusText(status)
	}
	return fmt.Sprintf("Jira error %d: %s", status, detail)
}

// JiraUser is the account behind the credentials.
type JiraUser struct {
	AccountID    string `json:"accountId"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

// Myself verifies credentials.
func (c *JiraClient) Myself(ctx context.Context) (*JiraUser, error) {
	var u JiraUser
	if err := c.get(ctx, "/rest/api/3/myself", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// RemoteProject is a Jira project as listed by the API.
type RemoteProject struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	ProjectTypeKey string `json:"projectTypeKey"`
	Style          string `json:"style"`
	Lead           struct {
		DisplayName string `json:"displayName"`
	} `json:"lead"`
	AvatarURLs map[string]string `json:"avatarUrls"`
}

// ListProjects pages through /rest/api/3/project/search (max 2000 projects).
func (c *JiraClient) ListProjects(ctx context.Context) ([]RemoteProject, error) {
	var all []RemoteProject
	startAt := 0
	for page := 0; page < 40; page++ {
		var resp struct {
			Values     []RemoteProject `json:"values"`
			IsLast     bool            `json:"isLast"`
			Total      int             `json:"total"`
			MaxResults int             `json:"maxResults"`
		}
		q := url.Values{"startAt": {strconv.Itoa(startAt)}, "maxResults": {"50"}, "orderBy": {"name"}, "expand": {"description,lead"}}
		if err := c.get(ctx, "/rest/api/3/project/search", q, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Values...)
		startAt += len(resp.Values)
		if resp.IsLast || len(resp.Values) == 0 || (resp.Total > 0 && startAt >= resp.Total) {
			break
		}
	}
	return all, nil
}

// StoryPointFields finds the custom field ids used for story points.
func (c *JiraClient) StoryPointFields(ctx context.Context) []string {
	var fields []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Custom bool   `json:"custom"`
	}
	if err := c.get(ctx, "/rest/api/3/field", nil, &fields); err != nil {
		return nil
	}
	var ids []string
	for _, f := range fields {
		n := strings.ToLower(strings.TrimSpace(f.Name))
		if f.Custom && (n == "story points" || n == "story point estimate") {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// RemoteIssue is the subset of a Jira issue we store.
type RemoteIssue struct {
	ID     string                     `json:"id"`
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
}

// baseIssueFields are always requested from Jira.
var baseIssueFields = []string{"summary", "status", "issuetype", "priority", "assignee", "duedate", "updated", "resolutiondate"}

const maxIssuesPerProject = 10000

// SearchIssues runs a JQL query via /rest/api/3/search/jql (token paging),
// falling back to the legacy /rest/api/3/search (startAt paging) when the new
// endpoint is unavailable.
func (c *JiraClient) SearchIssues(ctx context.Context, jql string, extraFields []string) ([]RemoteIssue, error) {
	fields := strings.Join(append(append([]string{}, baseIssueFields...), extraFields...), ",")
	issues, err := c.searchJQL(ctx, jql, fields)
	var je *JiraError
	if err != nil && errors.As(err, &je) && (je.Status == http.StatusNotFound || je.Status == http.StatusGone || je.Status == http.StatusMethodNotAllowed) {
		return c.searchLegacy(ctx, jql, fields)
	}
	return issues, err
}

func (c *JiraClient) searchJQL(ctx context.Context, jql, fields string) ([]RemoteIssue, error) {
	var all []RemoteIssue
	token := ""
	for len(all) < maxIssuesPerProject {
		q := url.Values{"jql": {jql}, "fields": {fields}, "maxResults": {"100"}}
		if token != "" {
			q.Set("nextPageToken", token)
		}
		var resp struct {
			Issues        []RemoteIssue `json:"issues"`
			NextPageToken string        `json:"nextPageToken"`
			IsLast        *bool         `json:"isLast"`
		}
		if err := c.get(ctx, "/rest/api/3/search/jql", q, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Issues...)
		if resp.NextPageToken == "" || resp.NextPageToken == token || len(resp.Issues) == 0 || (resp.IsLast != nil && *resp.IsLast) {
			break
		}
		token = resp.NextPageToken
	}
	return all, nil
}

func (c *JiraClient) searchLegacy(ctx context.Context, jql, fields string) ([]RemoteIssue, error) {
	var all []RemoteIssue
	startAt := 0
	for len(all) < maxIssuesPerProject {
		q := url.Values{"jql": {jql}, "fields": {fields}, "maxResults": {"100"}, "startAt": {strconv.Itoa(startAt)}}
		var resp struct {
			Issues     []RemoteIssue `json:"issues"`
			StartAt    int           `json:"startAt"`
			MaxResults int           `json:"maxResults"`
			Total      int           `json:"total"`
		}
		if err := c.get(ctx, "/rest/api/3/search", q, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Issues...)
		startAt = resp.StartAt + len(resp.Issues)
		if len(resp.Issues) == 0 || startAt >= resp.Total {
			break
		}
	}
	return all, nil
}

// ---------- mapping ----------

// MapStatusCategory converts Jira's statusCategory.key to ours.
func MapStatusCategory(key string) string {
	switch strings.ToLower(key) {
	case "done":
		return CatDone
	case "indeterminate":
		return CatInProgress
	default: // "new", "undefined"
		return CatTodo
	}
}

func parseJiraTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

func parseJiraDate(s string) *time.Time {
	s = strings.TrimSpace(s)
	if len(s) < 10 {
		return nil
	}
	t, err := time.Parse("2006-01-02", s[:10])
	if err != nil {
		return nil
	}
	return &t
}

// MapIssue converts a Jira issue into a Task (without ids/org/project set).
func MapIssue(ri RemoteIssue, siteURL string, storyPointFields []string) Task {
	t := Task{
		Key:        ri.Key,
		ExternalID: ri.ID,
		Source:     SourceJira,
		URL:        strings.TrimRight(siteURL, "/") + "/browse/" + url.PathEscape(ri.Key),
	}
	str := func(name string) string {
		var s string
		if raw, ok := ri.Fields[name]; ok {
			_ = json.Unmarshal(raw, &s)
		}
		return s
	}
	t.Title = str("summary")
	if t.Title == "" {
		t.Title = ri.Key
	}
	var status struct {
		Name           string `json:"name"`
		StatusCategory struct {
			Key string `json:"key"`
		} `json:"statusCategory"`
	}
	if raw, ok := ri.Fields["status"]; ok {
		_ = json.Unmarshal(raw, &status)
	}
	t.Status = status.Name
	t.StatusCategory = MapStatusCategory(status.StatusCategory.Key)

	var named struct {
		Name string `json:"name"`
	}
	if raw, ok := ri.Fields["issuetype"]; ok && json.Unmarshal(raw, &named) == nil {
		t.Type = named.Name
	}
	named.Name = ""
	if raw, ok := ri.Fields["priority"]; ok && json.Unmarshal(raw, &named) == nil {
		t.Priority = named.Name
	}
	var assignee struct {
		DisplayName  string `json:"displayName"`
		EmailAddress string `json:"emailAddress"`
	}
	if raw, ok := ri.Fields["assignee"]; ok && json.Unmarshal(raw, &assignee) == nil {
		t.AssigneeName = assignee.DisplayName
		t.AssigneeEmail = assignee.EmailAddress
	}
	t.DueDate = parseJiraDate(str("duedate"))
	t.RemoteUpdatedAt = parseJiraTime(str("updated"))
	if t.StatusCategory == CatDone {
		t.CompletedAt = parseJiraTime(str("resolutiondate"))
		if t.CompletedAt == nil {
			t.CompletedAt = t.RemoteUpdatedAt
		}
	}
	for _, f := range storyPointFields {
		raw, ok := ri.Fields[f]
		if !ok {
			continue
		}
		var v *float64
		if json.Unmarshal(raw, &v) == nil && v != nil {
			t.StoryPoints = v
			break
		}
	}
	return t
}

// BuildJQL scopes an optional user filter to one project.
func BuildJQL(projectKey, filter string) string {
	jql := fmt.Sprintf(`project = "%s"`, strings.ReplaceAll(projectKey, `"`, ``))
	f := strings.TrimSpace(filter)
	if f != "" {
		if i := strings.Index(strings.ToLower(f), "order by"); i >= 0 {
			f = strings.TrimSpace(f[:i])
		}
		if f != "" {
			jql += " AND (" + f + ")"
		}
	}
	return jql + " ORDER BY updated DESC"
}
