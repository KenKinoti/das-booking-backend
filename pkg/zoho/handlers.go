package zoho

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Handler serves /api/v1/imports/... and the public OAuth callback.
type Handler struct {
	DB        *gorm.DB
	Engine    *Engine
	PublicApp string // PUBLIC_APP_URL (where to send the browser after OAuth)
}

// NewHandler creates the handler with its engine.
func NewHandler(db *gorm.DB) *Handler {
	return &Handler{DB: db, Engine: NewEngine(db)}
}

const maxUpload = 25 << 20

// RegisterRoutes mounts the authenticated API.
func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	g := r.Group("/imports")
	g.GET("/zoho/connection", h.GetConnection)
	g.PUT("/zoho/connection", h.admin, h.SaveConnection)
	g.DELETE("/zoho/connection", h.admin, h.Disconnect)
	g.POST("/zoho/connect", h.admin, h.Connect)
	g.GET("/zoho/organizations", h.admin, h.Organizations)
	g.PUT("/zoho/organization", h.admin, h.ChooseOrganization)
	g.POST("/zoho/preview", h.admin, h.PreviewAPI)

	g.GET("/fields", h.Fields)
	g.POST("/files", h.admin, h.UploadFiles)
	g.GET("/jobs", h.ListJobs)
	g.GET("/jobs/:id", h.GetJob)
	g.DELETE("/jobs/:id", h.admin, h.DeleteJob)
	g.PUT("/jobs/:id/options", h.admin, h.SaveOptions)
	g.POST("/jobs/:id/preview", h.admin, h.PreviewJob)
	g.POST("/jobs/:id/start", h.admin, h.StartJob)
	g.POST("/jobs/:id/cancel", h.admin, h.CancelJob)
	g.POST("/jobs/:id/resume", h.admin, h.ResumeJob)
	g.POST("/jobs/:id/undo", h.admin, h.UndoJob)
	g.GET("/jobs/:id/errors", h.Errors)
	g.GET("/jobs/:id/errors.csv", h.ErrorsCSV)
	g.GET("/jobs/:id/files", h.ListFiles)
	g.POST("/jobs/:id/files", h.admin, h.AddFiles)
	g.PUT("/jobs/:id/files/:fileId", h.admin, h.UpdateFile)
	g.DELETE("/jobs/:id/files/:fileId", h.admin, h.DeleteFile)
}

// RegisterPublic mounts the OAuth callback (no auth: Zoho redirects the browser).
func (h *Handler) RegisterPublic(r *gin.RouterGroup) {
	r.GET("/zoho/callback", h.Callback)
}

// ---------- helpers ----------

func ctxStr(c *gin.Context, k string) string {
	if v, ok := c.Get(k); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func fail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func ok(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func (h *Handler) org(c *gin.Context) string {
	org := ctxStr(c, "org_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organisation not found in token")
	}
	return org
}

func (h *Handler) admin(c *gin.Context) {
	switch ctxStr(c, "user_role") {
	case "super_admin", "admin", "owner", "org_admin":
		c.Next()
		return
	}
	fail(c, http.StatusForbidden, "FORBIDDEN", "Only administrators can import data")
}

func (h *Handler) job(c *gin.Context) (*Job, bool) {
	org := h.org(c)
	if org == "" {
		return nil, false
	}
	var j Job
	if err := h.DB.Where("id = ? AND organization_id = ?", c.Param("id"), org).First(&j).Error; err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Import not found")
		return nil, false
	}
	return &j, true
}

// RedirectURI is what the user registers in the Zoho API console.
func RedirectURI(r *http.Request) string {
	return branding.BaseURL(r) + "/api/v1/public/zoho/callback"
}

func newState() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- connection ----------

type connectionView struct {
	Connected    bool         `json:"connected"`
	Connection   *Connection  `json:"connection"`
	HasSecret    bool         `json:"has_secret"`
	RedirectURI  string       `json:"redirect_uri"`
	Scopes       []string     `json:"scopes"`
	DataCenters  []DataCenter `json:"data_centers"`
	DefaultDC    string       `json:"default_dc"`
	HomeCurrency string       `json:"home_currency"`
	Running      *Job         `json:"running,omitempty"`
}

func (h *Handler) GetConnection(c *gin.Context) {
	org := h.org(c)
	if org == "" {
		return
	}
	v := connectionView{RedirectURI: RedirectURI(c.Request), Scopes: Scopes, DataCenters: DataCenters, DefaultDC: DefaultDC,
		HomeCurrency: orgprefs.Currency(h.DB, org)}
	var conn Connection
	if err := h.DB.Where("organization_id = ?", org).First(&conn).Error; err == nil {
		v.Connection = &conn
		v.HasSecret = conn.ClientSecret != ""
		v.Connected = conn.RefreshToken != ""
	}
	ok(c, v)
}

type saveConnInput struct {
	DC           string `json:"dc"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func (h *Handler) SaveConnection(c *gin.Context) {
	org := h.org(c)
	if org == "" {
		return
	}
	var in saveConnInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	in.ClientID = strings.TrimSpace(in.ClientID)
	in.ClientSecret = strings.TrimSpace(in.ClientSecret)
	if in.ClientID == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Enter the Client ID from the Zoho API console")
		return
	}
	dc := FindDC(in.DC)
	var conn Connection
	err := h.DB.Where("organization_id = ?", org).First(&conn).Error
	isNew := errors.Is(err, gorm.ErrRecordNotFound)
	if err != nil && !isNew {
		fail(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if isNew {
		conn = Connection{OrganizationID: org}
	}
	changed := conn.ClientID != in.ClientID || conn.DC != dc.Code
	conn.DC, conn.ClientID = dc.Code, in.ClientID
	if in.ClientSecret != "" {
		enc, err := mail.EncryptSecret(in.ClientSecret)
		if err != nil {
			fail(c, http.StatusInternalServerError, "ENCRYPT_FAILED", "Could not store the secret")
			return
		}
		conn.ClientSecret = enc
		changed = true
	}
	if conn.ClientSecret == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Enter the Client Secret from the Zoho API console")
		return
	}
	if changed && !isNew {
		// New app credentials: the old tokens belong to the old app.
		conn.RefreshToken, conn.APIDomain, conn.AccountsServer, conn.ConnectedAt = "", "", "", nil
	}
	if err := h.DB.Save(&conn).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	h.GetConnection(c)
}

type connectInput struct {
	ReturnTo string `json:"return_to"`
}

// Connect starts OAuth: returns the Zoho consent URL.
func (h *Handler) Connect(c *gin.Context) {
	org := h.org(c)
	if org == "" {
		return
	}
	var in connectInput
	_ = c.ShouldBindJSON(&in)
	var conn Connection
	if err := h.DB.Where("organization_id = ?", org).First(&conn).Error; err != nil || conn.ClientID == "" || conn.ClientSecret == "" {
		fail(c, http.StatusBadRequest, "NOT_CONFIGURED", "Save the Client ID and Client Secret first")
		return
	}
	ret := strings.TrimSpace(in.ReturnTo)
	if u, err := url.Parse(ret); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		ret = ""
	}
	if h.PublicApp != "" && ret != "" && !strings.HasPrefix(ret, strings.TrimRight(h.PublicApp, "/")+"/") {
		ret = strings.TrimRight(h.PublicApp, "/") + "/imports/zoho"
	}
	if ret == "" && h.PublicApp != "" {
		ret = strings.TrimRight(h.PublicApp, "/") + "/imports/zoho"
	}
	st := OAuthState{State: newState(), OrganizationID: org, UserID: ctxStr(c, "user_id"), ReturnTo: ret, DC: conn.DC,
		RedirectURI: RedirectURI(c.Request), ExpiresAt: time.Now().Add(15 * time.Minute), CreatedAt: time.Now()}
	h.DB.Where("expires_at < ?", time.Now()).Delete(&OAuthState{})
	if err := h.DB.Create(&st).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	dc := FindDC(conn.DC)
	ok(c, gin.H{"url": authURL(dc.Accounts, conn.ClientID, st.RedirectURI, st.State), "redirect_uri": st.RedirectURI})
}

// Callback finishes OAuth (Zoho redirects the browser here).
func (h *Handler) Callback(c *gin.Context) {
	stateID := c.Query("state")
	back := func(ret string, params url.Values) {
		if ret == "" {
			ret = "/imports/zoho"
		}
		sep := "?"
		if strings.Contains(ret, "?") {
			sep = "&"
		}
		c.Redirect(http.StatusFound, ret+sep+params.Encode())
	}
	var st OAuthState
	if stateID == "" || h.DB.Where("state = ?", stateID).First(&st).Error != nil {
		c.Data(http.StatusBadRequest, "text/html; charset=utf-8", []byte("<p>This Zoho sign-in link is invalid or was already used. Go back to DASYIN → Import data → Zoho Books and click Connect again.</p>"))
		return
	}
	h.DB.Delete(&OAuthState{}, "state = ?", st.State) // single use
	if time.Now().After(st.ExpiresAt) {
		back(st.ReturnTo, url.Values{"zoho": {"error"}, "message": {"The sign-in took too long — try Connect again."}})
		return
	}
	if e := c.Query("error"); e != "" {
		msg := "Zoho did not grant access (" + e + ")."
		if e == "access_denied" {
			msg = "Access was declined in Zoho."
		}
		back(st.ReturnTo, url.Values{"zoho": {"error"}, "message": {msg}})
		return
	}
	code := c.Query("code")
	var conn Connection
	if code == "" || h.DB.Where("organization_id = ?", st.OrganizationID).First(&conn).Error != nil {
		back(st.ReturnTo, url.Values{"zoho": {"error"}, "message": {"Zoho did not return an authorisation code."}})
		return
	}
	dc := FindDC(conn.DC)
	accounts := dc.Accounts
	if as := c.Query("accounts-server"); as != "" {
		if !trustedAccounts(as) {
			back(st.ReturnTo, url.Values{"zoho": {"error"}, "message": {"Zoho named an unexpected accounts server; connection refused."}})
			return
		}
		accounts = strings.TrimRight(as, "/")
	}
	secret, err := mail.DecryptSecret(conn.ClientSecret)
	if err != nil {
		back(st.ReturnTo, url.Values{"zoho": {"error"}, "message": {"Could not read the saved client secret — save it again."}})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	tr, err := ExchangeCode(ctx, h.Engine.hc(), accounts, conn.ClientID, secret, st.RedirectURI, code)
	if err != nil {
		back(st.ReturnTo, url.Values{"zoho": {"error"}, "message": {err.Error()}})
		return
	}
	if tr.RefreshToken == "" {
		back(st.ReturnTo, url.Values{"zoho": {"error"}, "message": {"Zoho did not return a refresh token. Remove DASYIN from Zoho's connected apps (accounts → Sessions → Connected apps) and connect again."}})
		return
	}
	rt, err := mail.EncryptSecret(tr.RefreshToken)
	if err != nil {
		back(st.ReturnTo, url.Values{"zoho": {"error"}, "message": {"Could not store the token."}})
		return
	}
	api := dc.API
	if tr.APIDomain != "" && trustedAPI(tr.APIDomain) {
		api = strings.TrimRight(tr.APIDomain, "/")
	}
	now := time.Now()
	conn.RefreshToken, conn.AccountsServer, conn.APIDomain, conn.ConnectedAt, conn.ConnectedBy, conn.LastError = rt, accounts, api, &now, st.UserID, ""
	conn.Scopes = strings.Join(Scopes, ",")
	// Pick the organisation automatically when there is only one.
	cl, err := h.Engine.ClientFor(&conn)
	if err == nil {
		if orgs, err := cl.Organizations(ctx); err == nil {
			if len(orgs) == 1 || (conn.ZohoOrgID != "" && !hasOrg(orgs, conn.ZohoOrgID)) {
				if len(orgs) == 1 {
					conn.ZohoOrgID, conn.ZohoOrgName, conn.ZohoCurrency = orgs[0].OrganizationID, orgs[0].Name, strings.ToUpper(orgs[0].CurrencyCode)
				} else {
					conn.ZohoOrgID, conn.ZohoOrgName, conn.ZohoCurrency = "", "", ""
				}
			}
		}
	}
	if err := h.DB.Save(&conn).Error; err != nil {
		back(st.ReturnTo, url.Values{"zoho": {"error"}, "message": {"Could not save the connection."}})
		return
	}
	back(st.ReturnTo, url.Values{"zoho": {"connected"}})
}

func hasOrg(orgs []ZohoOrg, id string) bool {
	for _, o := range orgs {
		if o.OrganizationID == id {
			return true
		}
	}
	return false
}

func (h *Handler) connection(c *gin.Context) (*Connection, bool) {
	org := h.org(c)
	if org == "" {
		return nil, false
	}
	var conn Connection
	if err := h.DB.Where("organization_id = ?", org).First(&conn).Error; err != nil || conn.RefreshToken == "" {
		fail(c, http.StatusBadRequest, "NOT_CONNECTED", "Connect Zoho Books first")
		return nil, false
	}
	return &conn, true
}

// Organizations lists the Zoho organisations the connected user can import from.
func (h *Handler) Organizations(c *gin.Context) {
	conn, okk := h.connection(c)
	if !okk {
		return
	}
	cl, err := h.Engine.ClientFor(conn)
	if err != nil {
		fail(c, http.StatusBadRequest, "NOT_CONNECTED", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	orgs, err := cl.Organizations(ctx)
	if err != nil {
		h.DB.Model(&Connection{}).Where("organization_id = ?", conn.OrganizationID).Update("last_error", err.Error())
		fail(c, http.StatusBadGateway, "ZOHO_ERROR", err.Error())
		return
	}
	if orgs == nil {
		orgs = []ZohoOrg{}
	}
	ok(c, orgs)
}

func (h *Handler) ChooseOrganization(c *gin.Context) {
	conn, okk := h.connection(c)
	if !okk {
		return
	}
	var in struct {
		OrganizationID string `json:"organization_id"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.OrganizationID == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose an organisation")
		return
	}
	cl, err := h.Engine.ClientFor(conn)
	if err != nil {
		fail(c, http.StatusBadRequest, "NOT_CONNECTED", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	orgs, err := cl.Organizations(ctx)
	if err != nil {
		fail(c, http.StatusBadGateway, "ZOHO_ERROR", err.Error())
		return
	}
	for _, o := range orgs {
		if o.OrganizationID == in.OrganizationID {
			h.DB.Model(&Connection{}).Where("organization_id = ?", conn.OrganizationID).Updates(map[string]interface{}{
				"zoho_org_id": o.OrganizationID, "zoho_org_name": o.Name, "zoho_currency": strings.ToUpper(o.CurrencyCode)})
			h.GetConnection(c)
			return
		}
	}
	fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "That organisation is not available to the connected Zoho user")
}

// Disconnect revokes the token and forgets it (imported data stays).
func (h *Handler) Disconnect(c *gin.Context) {
	org := h.org(c)
	if org == "" {
		return
	}
	var conn Connection
	if err := h.DB.Where("organization_id = ?", org).First(&conn).Error; err == nil && conn.RefreshToken != "" {
		if rt, err := mail.DecryptSecret(conn.RefreshToken); err == nil && rt != "" {
			acc := FindDC(conn.DC).Accounts
			if trustedAccounts(conn.AccountsServer) {
				acc = conn.AccountsServer
			}
			ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
			_ = Revoke(ctx, h.Engine.hc(), acc, rt)
			cancel()
		}
	}
	var running []Job
	h.DB.Where("organization_id = ? AND source = ? AND status IN ?", org, SourceAPI, []string{JobRunning, JobPreviewing, JobQueued, JobPaused}).Find(&running)
	for _, j := range running {
		h.Engine.Cancel(j.ID)
		h.DB.Model(&Job{}).Where("id = ?", j.ID).Updates(map[string]interface{}{"status": JobCancelled, "message": "Zoho was disconnected", "resume_at": nil})
	}
	h.DB.Where("organization_id = ?", org).Delete(&Connection{})
	ok(c, gin.H{"disconnected": true})
}

// ---------- jobs ----------

func defaultOptions() Options {
	t := true
	return Options{OnCustomerMatch: "merge", OnDocMatch: "skip", ItemsToCatalog: true, ContinueNumbering: &t,
		Entities: map[string]bool{EntItems: true, EntContacts: true, EntQuotes: true, EntInvoices: true, EntPayments: true, EntCredits: true,
			EntPDFs: true, EntHistory: true, EntRecurring: false}}
}

func cleanOptions(o Options) Options {
	d := defaultOptions()
	if o.Entities == nil {
		o.Entities = d.Entities
	}
	switch o.OnCustomerMatch {
	case "merge", "skip", "create":
	default:
		o.OnCustomerMatch = "merge"
	}
	switch o.OnDocMatch {
	case "skip", "overwrite":
	default:
		o.OnDocMatch = "skip"
	}
	if o.ContinueNumbering == nil {
		o.ContinueNumbering = d.ContinueNumbering
	}
	for k, v := range o.Decisions {
		switch v {
		case "merge", "skip", "create", "overwrite":
		default:
			delete(o.Decisions, k)
		}
	}
	if o.RequestsPerMinute < 0 || o.RequestsPerMinute > 100 {
		o.RequestsPerMinute = 0
	}
	return o
}

func (h *Handler) busy(org string) *Job {
	var j Job
	if h.DB.Where("organization_id = ? AND status IN ?", org, []string{JobRunning, JobQueued, JobPreviewing}).Order("created_at DESC").First(&j).Error == nil {
		return &j
	}
	return nil
}

// PreviewAPI creates a Zoho API job and builds its preview.
func (h *Handler) PreviewAPI(c *gin.Context) {
	conn, okk := h.connection(c)
	if !okk {
		return
	}
	if conn.ZohoOrgID == "" {
		fail(c, http.StatusBadRequest, "NO_ORGANIZATION", "Choose which Zoho Books organisation to import from")
		return
	}
	var in struct {
		Options *Options `json:"options"`
	}
	_ = c.ShouldBindJSON(&in)
	if b := h.busy(conn.OrganizationID); b != nil {
		fail(c, http.StatusConflict, "BUSY", "Another import is running — wait for it to finish or stop it first")
		return
	}
	opts := defaultOptions()
	if in.Options != nil {
		opts = cleanOptions(*in.Options)
	}
	j := Job{ID: uuid.NewString(), OrganizationID: conn.OrganizationID, Source: SourceAPI, Status: JobDraft, Options: opts, CreatedBy: ctxStr(c, "user_id"),
		Counts: map[string]*Count{}, Message: "Reading your Zoho Books data…"}
	if err := h.DB.Create(&j).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if err := h.Engine.StartPreview(j.ID); err != nil {
		fail(c, http.StatusConflict, "BUSY", err.Error())
		return
	}
	h.respondJob(c, &j)
}

func (h *Handler) respondJob(c *gin.Context, j *Job) {
	var fresh Job
	if h.DB.Where("id = ?", j.ID).First(&fresh).Error == nil {
		j = &fresh
	}
	if s := h.Engine.Snapshot(j.ID); s != nil {
		s.Preview, s.Summary, s.Options = j.Preview, j.Summary, j.Options
		if s.Status == "" {
			s.Status = j.Status
		}
		j = s
	}
	ok(c, h.jobView(j))
}

type jobView struct {
	*Job
	Running    bool              `json:"running"`
	CanUndo    bool              `json:"can_undo"`
	CanResume  bool              `json:"can_resume"`
	Steps      []string          `json:"steps"`
	Labels     map[string]string `json:"labels"`
	Connection string            `json:"connection,omitempty"`
}

func (h *Handler) jobView(j *Job) jobView {
	v := jobView{Job: j, Running: h.Engine.IsRunning(j.ID), Labels: EntityLabels}
	v.CanUndo = (j.Status == JobCompleted || j.Status == JobFailed || j.Status == JobCancelled || j.Status == JobPaused) && !v.Running
	v.CanResume = (j.Status == JobFailed || j.Status == JobCancelled || j.Status == JobPaused) && !v.Running && j.StartedAt != nil
	if j.Source == SourceAPI {
		v.Steps = apiSteps
	} else {
		v.Steps = fileSteps
	}
	if j.Counts == nil {
		j.Counts = map[string]*Count{}
	}
	return v
}

func (h *Handler) ListJobs(c *gin.Context) {
	org := h.org(c)
	if org == "" {
		return
	}
	var jobs []Job
	h.DB.Select("id, organization_id, source, status, step, message, counts, error_count, error, api_calls, started_at, finished_at, created_by, created_at, updated_at, resume_at, heartbeat, summary").
		Where("organization_id = ?", org).Order("created_at DESC").Limit(30).Find(&jobs)
	out := make([]jobView, 0, len(jobs))
	for i := range jobs {
		if s := h.Engine.Snapshot(jobs[i].ID); s != nil {
			jobs[i].Counts, jobs[i].Step, jobs[i].Message = s.Counts, s.Step, s.Message
		}
		out = append(out, h.jobView(&jobs[i]))
	}
	ok(c, out)
}

func (h *Handler) GetJob(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	h.Engine.Kick(j)
	h.respondJob(c, j)
}

func (h *Handler) DeleteJob(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	if h.Engine.IsRunning(j.ID) || j.Status == JobRunning || j.Status == JobPreviewing {
		fail(c, http.StatusConflict, "RUNNING", "Stop the import before removing it")
		return
	}
	h.DB.Where("job_id = ?", j.ID).Delete(&Upload{})
	h.DB.Delete(j)
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) SaveOptions(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	var in Options
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid options")
		return
	}
	if j.Status != JobPreview && j.Status != JobDraft && j.Status != JobCompleted && j.Status != JobFailed && j.Status != JobCancelled {
		fail(c, http.StatusConflict, "LOCKED", "Options can't be changed while the import runs")
		return
	}
	j.Options = cleanOptions(in)
	h.DB.Model(&Job{}).Where("id = ?", j.ID).Update("options", mustJSON(j.Options))
	h.respondJob(c, j)
}

func (h *Handler) PreviewJob(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	var in struct {
		Options *Options `json:"options"`
	}
	_ = c.ShouldBindJSON(&in)
	if in.Options != nil {
		j.Options = cleanOptions(*in.Options)
		h.DB.Model(&Job{}).Where("id = ?", j.ID).Update("options", mustJSON(j.Options))
	}
	if j.Source == SourceCSV {
		var n int64
		h.DB.Model(&Upload{}).Where("job_id = ? AND kind <> '' AND kind <> ?", j.ID, FileIgnore).Count(&n)
		if n == 0 {
			fail(c, http.StatusBadRequest, "NO_FILES", "Choose what each file contains first")
			return
		}
	}
	if err := h.Engine.StartPreview(j.ID); err != nil {
		fail(c, http.StatusConflict, "BUSY", err.Error())
		return
	}
	h.respondJob(c, j)
}

func (h *Handler) StartJob(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	var in struct {
		Options *Options `json:"options"`
	}
	_ = c.ShouldBindJSON(&in)
	if j.Status != JobPreview {
		fail(c, http.StatusConflict, "NOT_READY", "Build the preview first")
		return
	}
	if b := h.busy(j.OrganizationID); b != nil && b.ID != j.ID {
		fail(c, http.StatusConflict, "BUSY", "Another import is running — wait for it to finish or stop it first")
		return
	}
	if in.Options != nil {
		j.Options = cleanOptions(*in.Options)
	}
	j.Counts = map[string]*Count{}
	h.DB.Model(&Job{}).Where("id = ?", j.ID).Updates(map[string]interface{}{"options": mustJSON(j.Options), "counts": "{}", "step_index": 0, "cursor": 1,
		"error_count": 0, "summary": nil, "message": "Starting…"})
	h.DB.Where("job_id = ?", j.ID).Delete(&JobError{})
	if err := h.Engine.Start(j.ID, false); err != nil {
		fail(c, http.StatusConflict, "BUSY", err.Error())
		return
	}
	h.respondJob(c, j)
}

func (h *Handler) CancelJob(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	if !h.Engine.Cancel(j.ID) {
		if j.Status == JobRunning || j.Status == JobPaused || j.Status == JobQueued || j.Status == JobPreviewing {
			h.DB.Model(&Job{}).Where("id = ?", j.ID).Updates(map[string]interface{}{"status": JobCancelled, "message": "Stopped", "resume_at": nil})
		}
	}
	h.Engine.Wait(j.ID, 10*time.Second)
	h.respondJob(c, j)
}

func (h *Handler) ResumeJob(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	if b := h.busy(j.OrganizationID); b != nil && b.ID != j.ID {
		fail(c, http.StatusConflict, "BUSY", "Another import is running")
		return
	}
	if err := h.Engine.Start(j.ID, true); err != nil {
		fail(c, http.StatusConflict, "NOT_RESUMABLE", err.Error())
		return
	}
	h.respondJob(c, j)
}

func (h *Handler) UndoJob(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	_ = c.ShouldBindJSON(&in)
	if !in.Confirm {
		fail(c, http.StatusBadRequest, "CONFIRM", "Confirm that everything imported by this job should be deleted")
		return
	}
	if h.Engine.IsRunning(j.ID) || j.Status == JobRunning || j.Status == JobPreviewing {
		fail(c, http.StatusConflict, "RUNNING", "Stop the import before undoing it")
		return
	}
	if j.Status == JobUndone {
		fail(c, http.StatusConflict, "ALREADY_UNDONE", "This import was already undone")
		return
	}
	res, err := Undo(h.DB, j, time.Now())
	if err != nil {
		fail(c, http.StatusInternalServerError, "UNDO_FAILED", err.Error())
		return
	}
	if j.Summary == nil {
		j.Summary = &Summary{}
	}
	j.Summary.Undo = res
	h.DB.Model(&Job{}).Where("id = ?", j.ID).Updates(map[string]interface{}{"status": JobUndone, "summary": mustJSON(j.Summary), "message": "Undone", "resume_at": nil})
	h.respondJob(c, j)
}

func (h *Handler) Errors(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	var list []JobError
	q := h.DB.Where("job_id = ?", j.ID)
	if s := c.Query("severity"); s != "" {
		q = q.Where("severity = ?", s)
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	if limit < 1 || limit > 2000 {
		limit = 200
	}
	q.Order("id ASC").Limit(limit).Find(&list)
	if list == nil {
		list = []JobError{}
	}
	ok(c, list)
}

func (h *Handler) ErrorsCSV(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	var list []JobError
	h.DB.Where("job_id = ?", j.ID).Order("id ASC").Find(&list)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="import-%s-issues.csv"`, j.ID[:8]))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write([]byte{0xEF, 0xBB, 0xBF})
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"Severity", "What", "Record", "Source ID", "Row", "Message"})
	for _, e := range list {
		row := ""
		if e.Row > 0 {
			row = strconv.Itoa(e.Row)
		}
		_ = w.Write([]string{e.Severity, firstNonEmpty(EntityLabels[e.Entity], e.Entity), csvSafe(e.Label), e.ExternalID, row, csvSafe(e.Message)})
	}
	w.Flush()
}

// csvSafe stops spreadsheet formula injection.
func csvSafe(s string) string {
	if s != "" && strings.ContainsAny(s[:1], "=+-@") {
		return "'" + s
	}
	return s
}

// ---------- files ----------

// Fields lists the file kinds and their fields (for the mapping screen).
func (h *Handler) Fields(c *gin.Context) {
	ok(c, FileKinds)
}

type fileView struct {
	Upload
	Sample [][]string `json:"sample"`
	Error  string     `json:"error,omitempty"`
}

func (h *Handler) fileViews(jobID string) []fileView {
	var ups []Upload
	h.DB.Where("job_id = ?", jobID).Order("created_at ASC").Find(&ups)
	out := make([]fileView, 0, len(ups))
	for _, u := range ups {
		fv := fileView{Upload: u, Sample: [][]string{}}
		if _, rows, err := ReadTable(u.Name, u.Data); err == nil {
			for i := 0; i < len(rows) && i < 3; i++ {
				fv.Sample = append(fv.Sample, rows[i])
			}
		} else {
			fv.Error = err.Error()
		}
		fv.Data = nil
		out = append(out, fv)
	}
	return out
}

func (h *Handler) saveFiles(c *gin.Context, j *Job) ([]string, bool) {
	if err := c.Request.ParseMultipartForm(maxUpload); err != nil {
		fail(c, http.StatusBadRequest, "UPLOAD_ERROR", "Could not read the upload (25 MB max per request)")
		return nil, false
	}
	files := c.Request.MultipartForm.File["files"]
	if len(files) == 0 {
		files = c.Request.MultipartForm.File["file"]
	}
	if len(files) == 0 {
		fail(c, http.StatusBadRequest, "NO_FILES", "Choose one or more CSV or XLSX files")
		return nil, false
	}
	var problems []string
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			problems = append(problems, fh.Filename+": could not be read")
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
		f.Close()
		if err != nil || len(data) > maxUpload {
			problems = append(problems, fh.Filename+": too large (25 MB max)")
			continue
		}
		headers, rows, err := ReadTable(fh.Filename, data)
		if err != nil {
			problems = append(problems, fh.Filename+": "+err.Error())
			continue
		}
		kind := DetectKind(fh.Filename, headers)
		u := Upload{ID: uuid.NewString(), JobID: j.ID, OrgID: j.OrganizationID, Name: cut(fh.Filename, 250), Kind: kind, Detected: kind, Headers: headers,
			Mapping: AutoMap(kind, headers), Rows: len(rows), Data: data, CreatedAt: time.Now()}
		if err := h.DB.Create(&u).Error; err != nil {
			problems = append(problems, fh.Filename+": "+err.Error())
		}
	}
	return problems, true
}

// UploadFiles starts a file import job with the uploaded files.
func (h *Handler) UploadFiles(c *gin.Context) {
	org := h.org(c)
	if org == "" {
		return
	}
	j := Job{ID: uuid.NewString(), OrganizationID: org, Source: SourceCSV, Status: JobDraft, Options: defaultOptions(), CreatedBy: ctxStr(c, "user_id"),
		Counts: map[string]*Count{}, Message: "Check what each file contains"}
	if err := h.DB.Create(&j).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	problems, okk := h.saveFiles(c, &j)
	if !okk {
		h.DB.Delete(&j)
		return
	}
	ok(c, gin.H{"job": h.jobView(&j), "files": h.fileViews(j.ID), "problems": problems})
}

func (h *Handler) AddFiles(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	if j.Source != SourceCSV || (j.Status != JobDraft && j.Status != JobPreview && j.Status != JobFailed) {
		fail(c, http.StatusConflict, "LOCKED", "Files can only be added before importing")
		return
	}
	problems, okk := h.saveFiles(c, j)
	if !okk {
		return
	}
	h.DB.Model(&Job{}).Where("id = ?", j.ID).Updates(map[string]interface{}{"status": JobDraft, "preview": nil})
	ok(c, gin.H{"files": h.fileViews(j.ID), "problems": problems})
}

func (h *Handler) ListFiles(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	ok(c, h.fileViews(j.ID))
}

func (h *Handler) UpdateFile(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	if j.Status != JobDraft && j.Status != JobPreview && j.Status != JobFailed {
		fail(c, http.StatusConflict, "LOCKED", "Files can't be changed after importing")
		return
	}
	var in struct {
		Kind    *string           `json:"kind"`
		Mapping map[string]string `json:"mapping"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request")
		return
	}
	var u Upload
	if h.DB.Where("id = ? AND job_id = ?", c.Param("fileId"), j.ID).First(&u).Error != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "File not found")
		return
	}
	upd := map[string]interface{}{}
	if in.Kind != nil {
		k := *in.Kind
		if k != FileIgnore && k != "" && kindByName(k) == nil {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Unknown file type")
			return
		}
		if k != u.Kind {
			upd["kind"] = k
			upd["mapping"] = mustJSON(AutoMap(k, u.Headers))
			u.Kind = k
		}
	}
	if in.Mapping != nil {
		hs := map[string]bool{}
		for _, x := range u.Headers {
			hs[x] = true
		}
		m := map[string]string{}
		fk := kindByName(u.Kind)
		for f, hd := range in.Mapping {
			if hd == "" || !hs[hd] || fk == nil {
				continue
			}
			for _, fd := range fk.Fields {
				if fd.Key == f {
					m[f] = hd
				}
			}
		}
		upd["mapping"] = mustJSON(m)
	}
	if len(upd) > 0 {
		h.DB.Model(&Upload{}).Where("id = ?", u.ID).Updates(upd)
		h.DB.Model(&Job{}).Where("id = ?", j.ID).Updates(map[string]interface{}{"status": JobDraft, "preview": nil})
	}
	ok(c, h.fileViews(j.ID))
}

func (h *Handler) DeleteFile(c *gin.Context) {
	j, okk := h.job(c)
	if !okk {
		return
	}
	if j.Status != JobDraft && j.Status != JobPreview && j.Status != JobFailed {
		fail(c, http.StatusConflict, "LOCKED", "Files can't be removed after importing")
		return
	}
	h.DB.Where("id = ? AND job_id = ?", c.Param("fileId"), j.ID).Delete(&Upload{})
	h.DB.Model(&Job{}).Where("id = ?", j.ID).Updates(map[string]interface{}{"status": JobDraft, "preview": nil})
	ok(c, h.fileViews(j.ID))
}
