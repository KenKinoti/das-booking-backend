package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Engine runs import jobs in background goroutines. Progress is kept in
// memory while a job runs here and checkpointed to the database after every
// page (API) or batch of rows (files), so a job interrupted by a restart or a
// rate limit continues where it stopped.
type Engine struct {
	DB    *gorm.DB
	HTTP  *http.Client
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error // tests
	// ClientHook lets tests adjust API clients (e.g. RequestsPerMinute).
	ClientHook func(*Client)

	mu      sync.Mutex
	running map[string]*run
}

type run struct {
	cancel    context.CancelFunc
	cancelled bool
	mu        sync.Mutex
	job       Job
}

// NewEngine creates an engine.
func NewEngine(db *gorm.DB) *Engine {
	return &Engine{DB: db, running: map[string]*run{}}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) hc() *http.Client {
	if e.HTTP != nil {
		return e.HTTP
	}
	return &http.Client{Timeout: 90 * time.Second}
}

// Snapshot returns the live state of a job running here (nil otherwise).
func (e *Engine) Snapshot(id string) *Job {
	e.mu.Lock()
	r := e.running[id]
	e.mu.Unlock()
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.job
	j.Counts = map[string]*Count{}
	for k, v := range r.job.Counts {
		c := *v
		j.Counts[k] = &c
	}
	return &j
}

// IsRunning reports whether job id runs in this process.
func (e *Engine) IsRunning(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[id] != nil
}

// Cancel stops a running job (it can be resumed later).
func (e *Engine) Cancel(id string) bool {
	e.mu.Lock()
	r := e.running[id]
	e.mu.Unlock()
	if r == nil {
		return false
	}
	r.mu.Lock()
	r.cancelled = true
	r.mu.Unlock()
	r.cancel()
	return true
}

// Wait blocks until job id is no longer running here (tests).
func (e *Engine) Wait(id string, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if !e.IsRunning(id) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// claim takes ownership of a job in the database (one runner at a time,
// across server instances): queued/paused/failed/cancelled jobs, or running
// ones whose runner stopped sending heartbeats.
func (e *Engine) claim(id string, from []string, status string) bool {
	now := e.now()
	stale := now.Add(-2 * time.Minute)
	res := e.DB.Model(&Job{}).Where("id = ? AND (status IN ? OR (status IN ? AND (heartbeat IS NULL OR heartbeat < ?)))", id, from, []string{JobRunning, JobPreviewing}, stale).
		Updates(map[string]interface{}{"status": status, "heartbeat": now, "resume_at": nil, "error": ""})
	return res.Error == nil && res.RowsAffected == 1
}

// Recover is called at start-up: jobs that were running when the server
// stopped resume shortly; a watcher resumes paused jobs when their time comes.
func (e *Engine) Recover(ctx context.Context) {
	t := e.now().Add(20 * time.Second)
	e.DB.Model(&Job{}).Where("status IN ?", []string{JobRunning, JobQueued}).
		Updates(map[string]interface{}{"status": JobPaused, "resume_at": t, "message": "The server restarted — continuing shortly"})
	e.DB.Model(&Job{}).Where("status = ?", JobPreviewing).Updates(map[string]interface{}{"status": JobFailed, "error": "The server restarted while building the preview — build it again"})
	go func() {
		tk := time.NewTicker(30 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				e.resumeDue()
			}
		}
	}()
}

func (e *Engine) resumeDue() {
	var jobs []Job
	e.DB.Where("status = ? AND resume_at IS NOT NULL AND resume_at <= ?", JobPaused, e.now()).Limit(5).Find(&jobs)
	for _, j := range jobs {
		if !e.IsRunning(j.ID) {
			_ = e.Start(j.ID, true)
		}
	}
}

// Kick resumes a job whose runner died (polling the job calls this).
func (e *Engine) Kick(j *Job) {
	if e.IsRunning(j.ID) {
		return
	}
	stale := j.Heartbeat == nil || j.Heartbeat.Before(e.now().Add(-2*time.Minute))
	due := j.Status == JobPaused && j.ResumeAt != nil && !j.ResumeAt.After(e.now())
	if (j.Status == JobRunning && stale) || due {
		_ = e.Start(j.ID, true)
	}
}

// Start runs (or resumes) the import of job id in the background.
func (e *Engine) Start(id string, resume bool) error {
	from := []string{JobPreview, JobQueued, JobPaused}
	if resume {
		from = append(from, JobFailed, JobCancelled)
	}
	if !e.claim(id, from, JobRunning) {
		return errors.New("the import is already running or can't be started from its current state")
	}
	var j Job
	if err := e.DB.Where("id = ?", id).First(&j).Error; err != nil {
		return err
	}
	if j.StartedAt == nil {
		t := e.now()
		j.StartedAt = &t
		e.DB.Model(&Job{}).Where("id = ?", id).Update("started_at", t)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{cancel: cancel, job: j}
	e.mu.Lock()
	e.running[id] = r
	e.mu.Unlock()
	go e.execute(ctx, r)
	return nil
}

// StartPreview builds the preview of job id in the background.
func (e *Engine) StartPreview(id string) error {
	if !e.claim(id, []string{JobDraft, JobPreview, JobFailed, JobCancelled, JobQueued}, JobPreviewing) {
		return errors.New("the preview is already being built")
	}
	var j Job
	if err := e.DB.Where("id = ?", id).First(&j).Error; err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{cancel: cancel, job: j}
	e.mu.Lock()
	e.running[id] = r
	e.mu.Unlock()
	go e.preview(ctx, r)
	return nil
}

func (e *Engine) done(id string) {
	e.mu.Lock()
	delete(e.running, id)
	e.mu.Unlock()
}

// ---------- shared plumbing ----------

func (e *Engine) importer(r *run) *Importer {
	j := &r.job
	src := "zoho"
	if j.Source == SourceCSV && !j.zohoFiles(e.DB) {
		src = "csv"
	}
	im := &Importer{DB: e.DB, Org: j.OrganizationID, User: j.CreatedBy, JobID: j.ID, RefSource: src, Opts: j.Options, Now: e.Now, DayFirst: true}
	im.Home = strings.ToUpper(orgprefs.Currency(e.DB, j.OrganizationID))
	if j.Counts == nil {
		j.Counts = map[string]*Count{}
	}
	im.Counts = j.Counts
	im.OnError = func(je JobError) {
		e.DB.Create(&je)
		r.job.ErrorCount++
	}
	return im
}

func (j *Job) zohoFiles(db *gorm.DB) bool {
	var kinds []string
	db.Model(&Upload{}).Where("job_id = ?", j.ID).Pluck("kind", &kinds)
	for _, k := range kinds {
		if strings.HasPrefix(k, "zoho_") {
			return true
		}
	}
	return false
}

// setStep records the current step (in memory; persisted at checkpoints).
func (r *run) setStep(idx int, name, msg string) {
	r.mu.Lock()
	r.job.StepIndex, r.job.Step, r.job.Message = idx, name, msg
	r.mu.Unlock()
}

// checkpoint persists progress; cursor is where to continue.
func (e *Engine) checkpoint(r *run, stepIdx, cursor int, calls int) {
	r.mu.Lock()
	r.job.StepIndex, r.job.Cursor = stepIdx, cursor
	if calls > 0 {
		r.job.APICalls = calls
	}
	t := e.now()
	r.job.Heartbeat = &t
	j := r.job
	r.mu.Unlock()
	e.DB.Model(&Job{}).Where("id = ?", j.ID).Updates(map[string]interface{}{
		"step": j.Step, "step_index": j.StepIndex, "cursor": j.Cursor, "message": j.Message, "counts": mustJSON(j.Counts),
		"api_calls": j.APICalls, "error_count": j.ErrorCount, "heartbeat": t,
	})
}

func (e *Engine) heartbeat(r *run, calls int) {
	r.mu.Lock()
	if calls > 0 {
		r.job.APICalls = calls
	}
	t := e.now()
	last := r.job.Heartbeat
	r.job.Heartbeat = &t
	r.mu.Unlock()
	if last == nil || t.Sub(*last) > 10*time.Second {
		e.DB.Model(&Job{}).Where("id = ?", r.job.ID).Updates(map[string]interface{}{"heartbeat": t, "message": r.job.Message})
	}
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// finish records the end state of a run.
func (e *Engine) finish(r *run, status, msg string, err error, resumeAt *time.Time) {
	r.mu.Lock()
	r.job.Status = status
	if msg != "" {
		r.job.Message = msg
	}
	if err != nil {
		r.job.Error = err.Error()
	}
	t := e.now()
	r.job.Heartbeat = &t
	if status == JobCompleted || status == JobFailed || status == JobCancelled {
		r.job.FinishedAt = &t
	}
	j := r.job
	r.mu.Unlock()
	upd := map[string]interface{}{"status": j.Status, "message": j.Message, "error": j.Error, "counts": mustJSON(j.Counts), "heartbeat": t,
		"finished_at": j.FinishedAt, "resume_at": resumeAt, "api_calls": j.APICalls, "error_count": j.ErrorCount, "step": j.Step,
		"step_index": j.StepIndex, "cursor": j.Cursor}
	if j.Summary != nil {
		upd["summary"] = mustJSON(j.Summary)
	}
	if j.Preview != nil {
		upd["preview"] = mustJSON(j.Preview)
	}
	if err := e.DB.Model(&Job{}).Where("id = ?", j.ID).Updates(upd).Error; err != nil {
		log.Printf("zoho import: could not save job %s: %v", j.ID, err)
	}
}

// stopFor turns an error into a job state.
func (e *Engine) stopFor(r *run, ctx context.Context, err error) {
	r.mu.Lock()
	cancelled := r.cancelled
	r.mu.Unlock()
	var rl *RateLimitError
	var ae *AuthError
	switch {
	case cancelled:
		e.finish(r, JobCancelled, "Stopped — you can continue it later", nil, nil)
	case errors.As(err, &rl):
		wait := rl.RetryAfter
		msg := "Zoho asked us to slow down — continuing automatically"
		if rl.Daily {
			wait = time.Hour
			msg = "Zoho's daily API limit for your plan is used up — the import continues automatically when calls are allowed again (checked hourly)"
		}
		if wait < time.Minute {
			wait = 2 * time.Minute
		}
		t := e.now().Add(wait)
		e.finish(r, JobPaused, msg, err, &t)
	case errors.As(err, &ae):
		e.DB.Model(&Connection{}).Where("organization_id = ?", r.job.OrganizationID).Update("last_error", err.Error())
		e.finish(r, JobFailed, "The Zoho connection stopped working — reconnect, then continue the import", err, nil)
	case ctx.Err() != nil:
		t := e.now().Add(20 * time.Second)
		e.finish(r, JobPaused, "Interrupted — continuing shortly", nil, &t)
	default:
		e.finish(r, JobFailed, "The import stopped with an error — you can continue it", err, nil)
	}
}

// ---------- API client ----------

// ClientFor builds an API client from an organisation's connection.
func (e *Engine) ClientFor(conn *Connection) (*Client, error) {
	if conn == nil || conn.RefreshToken == "" {
		return nil, errors.New("Zoho Books is not connected")
	}
	secret, err := mail.DecryptSecret(conn.ClientSecret)
	if err != nil {
		return nil, errors.New("could not read the saved client secret — enter it again")
	}
	rt, err := mail.DecryptSecret(conn.RefreshToken)
	if err != nil {
		return nil, errors.New("could not read the saved Zoho token — reconnect")
	}
	dc := FindDC(conn.DC)
	acc := dc.Accounts
	if conn.AccountsServer != "" && trustedAccounts(conn.AccountsServer) {
		acc = conn.AccountsServer
	}
	api := dc.API
	if conn.APIDomain != "" && trustedAPI(conn.APIDomain) {
		api = conn.APIDomain
	}
	c := &Client{HTTP: e.hc(), Accounts: acc, APIDomain: api, ClientID: conn.ClientID, ClientSecret: secret, RefreshToken: rt,
		OrgID: conn.ZohoOrgID, Sleep: e.Sleep}
	if e.ClientHook != nil {
		e.ClientHook(c)
	}
	return c, nil
}

func (e *Engine) clientForJob(j *Job) (*Client, *Connection, error) {
	var conn Connection
	if err := e.DB.Where("organization_id = ?", j.OrganizationID).First(&conn).Error; err != nil {
		return nil, nil, errors.New("Zoho Books is not connected")
	}
	if conn.ZohoOrgID == "" {
		return nil, nil, errors.New("choose which Zoho Books organisation to import from")
	}
	c, err := e.ClientFor(&conn)
	if err != nil {
		return nil, nil, err
	}
	if j.Options.RequestsPerMinute > 0 {
		c.RequestsPerMinute = j.Options.RequestsPerMinute
	}
	return c, &conn, nil
}

// apiList describes a Zoho list endpoint.
type apiList struct {
	ent, path, key, detailKey, kind string
	extra                           url.Values
}

var apiLists = map[string]apiList{
	EntItems:     {EntItems, "/items", "items", "item", "", url.Values{"filter_by": {"Status.All"}}},
	EntContacts:  {EntContacts, "/contacts", "contacts", "contact", "", url.Values{"filter_by": {"Status.All"}}},
	EntQuotes:    {EntQuotes, "/estimates", "estimates", "estimate", KindQuote, url.Values{"filter_by": {"Status.All"}}},
	EntInvoices:  {EntInvoices, "/invoices", "invoices", "invoice", KindInvoice, url.Values{"filter_by": {"Status.All"}}},
	EntCredits:   {EntCredits, "/creditnotes", "creditnotes", "creditnote", KindCredit, url.Values{"filter_by": {"Status.All"}}},
	EntRecurring: {EntRecurring, "/recurringinvoices", "recurring_invoices", "recurring_invoice", KindRecurring, url.Values{"filter_by": {"Status.All"}}},
	EntPayments:  {EntPayments, "/customerpayments", "customerpayments", "payment", "", nil},
}

// Steps of an API import, in order.
var apiSteps = []string{EntSettings, EntItems, EntContacts, EntQuotes, EntInvoices, EntCredits, EntPayments, EntRecurring, "finish"}

// Steps of a file import, in order.
var fileSteps = []string{EntItems, EntContacts, EntQuotes, EntInvoices, EntCredits, "finish"}

// ---------- run ----------

func (e *Engine) execute(ctx context.Context, r *run) {
	defer e.done(r.job.ID)
	defer func() {
		if p := recover(); p != nil {
			log.Printf("zoho import %s panicked: %v", r.job.ID, p)
			e.finish(r, JobFailed, "The import stopped with an internal error", fmt.Errorf("%v", p), nil)
		}
	}()
	// Resume from the last checkpoint: counts as they were then.
	r.mu.Lock()
	if r.job.Counts == nil {
		r.job.Counts = map[string]*Count{}
	}
	r.job.Status = JobRunning
	r.mu.Unlock()
	var err error
	if r.job.Source == SourceAPI {
		err = e.runAPI(ctx, r)
	} else {
		err = e.runFiles(ctx, r)
	}
	if err != nil {
		e.stopFor(r, ctx, err)
		return
	}
	e.finish(r, JobCompleted, "Import finished", nil, nil)
}

func (e *Engine) seedTotals(r *run) {
	if r.job.Preview == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ep := range r.job.Preview.Entities {
		c := r.job.Counts[ep.Entity]
		if c == nil {
			c = &Count{}
			r.job.Counts[ep.Entity] = c
		}
		if c.Total < ep.Count {
			c.Total = ep.Count
		}
	}
}

func (e *Engine) runAPI(ctx context.Context, r *run) error {
	c, conn, err := e.clientForJob(&r.job)
	if err != nil {
		return err
	}
	im := e.importer(r)
	im.SrcBase = strings.ToUpper(conn.ZohoCurrency)
	e.seedTotals(r)
	opts := r.job.Options
	start, cursor := r.job.StepIndex, r.job.Cursor
	for si := start; si < len(apiSteps); si++ {
		step := apiSteps[si]
		page := 1
		if si == start && cursor > 1 {
			page = cursor
		}
		if step == "finish" {
			r.setStep(si, step, "Finishing: numbering and reconciliation")
			e.finishImport(r, im, conn)
			continue
		}
		if !opts.want(step) && step != EntSettings {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if step == EntSettings {
			r.setStep(si, step, "Reading organisation settings")
			if err := e.apiSettings(ctx, c, im, conn); err != nil {
				return err
			}
			e.checkpoint(r, si+1, 1, c.Calls())
			continue
		}
		// Contacts must be known before documents even when not re-imported:
		// handled by refs. Each list step walks pages of 200.
		l := apiLists[step]
		for {
			r.setStep(si, step, fmt.Sprintf("%s — page %d", EntityLabels[step], page))
			rows, more, err := c.Page(ctx, l.path, l.key, page, l.extra)
			if err != nil {
				return err
			}
			for _, raw := range rows {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := e.apiRow(ctx, c, im, r, l, raw); err != nil {
					return err
				}
				e.heartbeat(r, c.Calls())
			}
			if !more {
				break
			}
			page++
			e.checkpoint(r, si, page, c.Calls())
		}
		e.checkpoint(r, si+1, 1, c.Calls())
	}
	return nil
}

// fatal errors stop the job; others are recorded against the row.
func fatal(err error) bool {
	var rl *RateLimitError
	var ae *AuthError
	return errors.As(err, &rl) || errors.As(err, &ae) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (e *Engine) apiSettings(ctx context.Context, c *Client, im *Importer, conn *Connection) error {
	orgs, err := c.Organizations(ctx)
	if err != nil {
		return err
	}
	for _, o := range orgs {
		if o.OrganizationID == conn.ZohoOrgID && o.CurrencyCode != "" {
			im.SrcBase = strings.ToUpper(o.CurrencyCode)
			e.DB.Model(&Connection{}).Where("organization_id = ?", conn.OrganizationID).Updates(map[string]interface{}{"zoho_currency": im.SrcBase, "zoho_org_name": o.Name})
		}
	}
	var tx struct {
		Taxes []struct {
			TaxName       flexStr `json:"tax_name"`
			TaxPercentage flexNum `json:"tax_percentage"`
		} `json:"taxes"`
	}
	if err := c.Get(ctx, "/settings/taxes", nil, &tx); err != nil {
		if fatal(err) {
			return err
		}
		im.warn(EntSettings, "", "Taxes", "Could not read tax rates: "+err.Error())
		return nil
	}
	var taxes []TaxRate
	for _, t := range tx.Taxes {
		taxes = append(taxes, TaxRate{Name: string(t.TaxName), Percent: float64(t.TaxPercentage)})
	}
	im.ImportTaxes(taxes)
	return nil
}

func (e *Engine) apiRow(ctx context.Context, c *Client, im *Importer, r *run, l apiList, raw json.RawMessage) error {
	opts := r.job.Options
	switch l.ent {
	case EntItems:
		var z zItem
		if err := json.Unmarshal(raw, &z); err != nil {
			im.fail(EntItems, "", "item", 0, err)
			return nil
		}
		im.UpsertItem(z.norm())
	case EntContacts:
		var z zContact
		if err := json.Unmarshal(raw, &z); err != nil {
			im.fail(EntContacts, "", "contact", 0, err)
			return nil
		}
		if strings.EqualFold(string(z.ContactType), "vendor") {
			return nil
		}
		ext := string(z.ContactID)
		if ref := im.ref(entContactRef, ext); ref != nil && ref.Modified != "" && ref.Modified == string(z.LastModifiedTime) && !opts.Overwrite && im.exists("customers", ref.LocalID) {
			im.load()
			im.contactLocal[ext] = ref.LocalID
			im.tally(EntContacts, outUnchanged)
			return nil
		}
		var det struct {
			Contact zContact `json:"contact"`
		}
		if err := c.Get(ctx, "/contacts/"+url.PathEscape(ext), nil, &det); err != nil {
			if fatal(err) {
				return err
			}
			im.fail(EntContacts, ext, string(z.ContactName), 0, err)
			return nil
		}
		ct := det.Contact.norm()
		if ct.Modified == "" {
			ct.Modified = string(z.LastModifiedTime)
		}
		im.UpsertContact(ct)
	case EntPayments:
		var z struct {
			PaymentID     flexStr `json:"payment_id"`
			PaymentNumber flexStr `json:"payment_number"`
			CustomerName  flexStr `json:"customer_name"`
			UnusedAmount  flexNum `json:"unused_amount"`
			CurrencyCode  flexStr `json:"currency_code"`
		}
		if json.Unmarshal(raw, &z) == nil && float64(z.UnusedAmount) > 0.004 {
			im.warn(EntPayments, string(z.PaymentID), "Payment "+string(z.PaymentNumber), fmt.Sprintf("%s %.2f of this payment from %s is not applied to any invoice in Zoho — it was not imported (record it as a deposit or credit if needed).",
				z.CurrencyCode, float64(z.UnusedAmount), z.CustomerName))
		}
	default: // documents
		var z zDoc
		if err := json.Unmarshal(raw, &z); err != nil {
			im.fail(l.ent, "", l.ent, 0, err)
			return nil
		}
		listDoc := z.norm(l.kind)
		listDoc.ListOnly = true
		ext := listDoc.ExtID
		refEnt := entDocRef + ":" + l.kind
		if ref := im.ref(refEnt, ext); ref != nil && ref.Modified != "" && ref.Modified == listDoc.Modified && !opts.Overwrite && im.exists("billing_invoices", ref.LocalID) {
			im.Unchanged(listDoc, ref.LocalID)
			return e.apiExtras(ctx, c, im, l, listDoc, ref.LocalID, false)
		}
		var det map[string]json.RawMessage
		if err := c.Get(ctx, l.path+"/"+url.PathEscape(ext), nil, &det); err != nil {
			if fatal(err) {
				return err
			}
			im.fail(l.ent, ext, listDoc.Number, 0, err)
			return nil
		}
		var zd zDoc
		if err := json.Unmarshal(det[l.detailKey], &zd); err != nil {
			im.fail(l.ent, ext, listDoc.Number, 0, fmt.Errorf("unexpected reply from Zoho: %v", err))
			return nil
		}
		d := zd.norm(l.kind)
		if d.Modified == "" {
			d.Modified = listDoc.Modified
		}
		if d.ExtID == "" {
			d.ExtID = ext
		}
		res := im.UpsertDoc(d)
		if res.LocalID != "" {
			return e.apiExtras(ctx, c, im, l, d, res.LocalID, res.Changed)
		}
	}
	return nil
}

// apiExtras fetches the history and original PDF of a document when they are
// new or the document changed.
func (e *Engine) apiExtras(ctx context.Context, c *Client, im *Importer, l apiList, d Doc, localID string, changed bool) error {
	if l.kind == KindRecurring {
		return nil
	}
	opts := im.Opts
	if opts.want(EntHistory) && (changed || !im.HasRef("history", d.Kind+":"+d.ExtID)) {
		var cm struct {
			Comments []zComment `json:"comments"`
		}
		if err := c.Get(ctx, l.path+"/"+url.PathEscape(d.ExtID)+"/comments", nil, &cm); err != nil {
			if fatal(err) {
				return err
			}
			im.fail(EntHistory, d.ExtID, d.Number, 0, err)
		} else {
			var list []Comment
			for _, z := range cm.Comments {
				list = append(list, z.norm())
			}
			im.WriteHistory(d, localID, list)
		}
	}
	if opts.want(EntPDFs) && (changed || !im.HasRef(entPDFRef, d.Kind+":"+d.ExtID)) {
		pdf, err := c.PDF(ctx, l.path+"/"+url.PathEscape(d.ExtID))
		if err != nil {
			if fatal(err) {
				return err
			}
			im.fail(EntPDFs, d.ExtID, d.Number, 0, err)
		} else {
			im.SavePDF(d, localID, pdf)
		}
	}
	return nil
}

func (e *Engine) finishImport(r *run, im *Importer, conn *Connection) {
	sum := &Summary{}
	sum.Numbering = im.ContinueNumbering()
	sum.Reconciliation = im.Reconcile()
	sum.Flags = im.Flags()
	if r.job.Preview != nil {
		sum.Warnings = append(sum.Warnings, r.job.Preview.Warnings...)
	}
	r.mu.Lock()
	r.job.Summary = sum
	r.mu.Unlock()
	if conn != nil {
		t := e.now()
		e.DB.Model(&Connection{}).Where("organization_id = ?", conn.OrganizationID).Updates(map[string]interface{}{"last_synced_at": t, "last_error": ""})
	}
}

// ---------- files ----------

func (e *Engine) uploads(jobID string) ([]Upload, error) {
	var ups []Upload
	err := e.DB.Where("job_id = ?", jobID).Order("created_at ASC").Find(&ups).Error
	return ups, err
}

func (e *Engine) dataset(j *Job) (*Dataset, error) {
	ups, err := e.uploads(j.ID)
	if err != nil {
		return nil, err
	}
	if len(ups) == 0 {
		return nil, errors.New("no files uploaded")
	}
	ds, err := BuildDataset(ups, true)
	if err != nil {
		return nil, err
	}
	return ds, nil
}

func (e *Engine) runFiles(ctx context.Context, r *run) error {
	ds, err := e.dataset(&r.job)
	if err != nil {
		return err
	}
	im := e.importer(r)
	im.DayFirst = GuessDayFirst(ds, true)
	im.SrcBase = im.Home
	e.seedTotals(r)
	if r.job.StepIndex == 0 && r.job.Cursor <= 1 {
		for _, je := range ds.Errors {
			im.report(je)
			im.tally(fileEntity(je.Entity), "failed")
		}
		for _, u := range ds.Unapplied {
			im.warn(EntPayments, "", "Payments", u)
		}
	}
	opts := r.job.Options
	start, cursor := r.job.StepIndex, r.job.Cursor
	for si := start; si < len(fileSteps); si++ {
		step := fileSteps[si]
		from := 0
		if si == start && cursor > 1 {
			from = cursor - 1
		}
		if step == "finish" {
			r.setStep(si, step, "Finishing: numbering and reconciliation")
			e.finishImport(r, im, nil)
			continue
		}
		if !opts.want(step) {
			continue
		}
		var n int
		switch step {
		case EntItems:
			n = len(ds.Items)
		case EntContacts:
			n = len(ds.Contacts)
		default:
			n = len(ds.Docs)
		}
		r.setStep(si, step, EntityLabels[step])
		for i := from; i < n; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			switch step {
			case EntItems:
				im.UpsertItem(ds.Items[i])
			case EntContacts:
				im.UpsertContact(ds.Contacts[i])
			default:
				d := ds.Docs[i]
				if entForKind(d.Kind) != step {
					continue
				}
				im.UpsertDoc(d)
			}
			if (i+1)%50 == 0 {
				e.checkpoint(r, si, i+2, 0)
			} else {
				e.heartbeat(r, 0)
			}
		}
		e.checkpoint(r, si+1, 1, 0)
	}
	return nil
}

func fileEntity(kind string) string {
	switch kind {
	case FileContacts, FileGenCust:
		return EntContacts
	case FilePersons:
		return EntPersons
	case FileItems:
		return EntItems
	case FileQuotes:
		return EntQuotes
	case FileCredits:
		return EntCredits
	case FilePayments:
		return EntPayments
	}
	return EntInvoices
}

// ---------- preview ----------

func (e *Engine) preview(ctx context.Context, r *run) {
	defer e.done(r.job.ID)
	defer func() {
		if p := recover(); p != nil {
			log.Printf("zoho preview %s panicked: %v", r.job.ID, p)
			e.finish(r, JobFailed, "Could not build the preview", fmt.Errorf("%v", p), nil)
		}
	}()
	var pv *Preview
	var err error
	if r.job.Source == SourceAPI {
		pv, err = e.previewAPI(ctx, r)
	} else {
		pv, err = e.previewFiles(r)
	}
	if err != nil {
		var rl *RateLimitError
		if errors.As(err, &rl) {
			e.finish(r, JobFailed, "Zoho is limiting requests right now — try the preview again in a few minutes", err, nil)
			return
		}
		e.stopFor(r, ctx, err)
		if r.job.Status == JobPaused { // previews don't resume by themselves
			e.DB.Model(&Job{}).Where("id = ?", r.job.ID).Updates(map[string]interface{}{"status": JobFailed, "resume_at": nil})
		}
		return
	}
	r.mu.Lock()
	r.job.Preview = pv
	r.job.Counts = map[string]*Count{}
	r.job.StepIndex, r.job.Cursor = 0, 1
	r.mu.Unlock()
	e.finish(r, JobPreview, "Preview ready", nil, nil)
}

func (e *Engine) previewAPI(ctx context.Context, r *run) (*Preview, error) {
	c, conn, err := e.clientForJob(&r.job)
	if err != nil {
		return nil, err
	}
	im := e.importer(r)
	im.SrcBase = strings.ToUpper(conn.ZohoCurrency)
	if orgs, err := c.Organizations(ctx); err == nil {
		for _, o := range orgs {
			if o.OrganizationID == conn.ZohoOrgID {
				im.SrcBase = strings.ToUpper(o.CurrencyCode)
				conn.ZohoOrgName = o.Name
				e.DB.Model(&Connection{}).Where("organization_id = ?", conn.OrganizationID).Updates(map[string]interface{}{"zoho_currency": im.SrcBase, "zoho_org_name": o.Name})
			}
		}
	} else if fatal(err) {
		return nil, err
	}
	var items []Item
	var contacts []Contact
	var docs []Doc
	var warnings []string
	for _, ent := range []string{EntItems, EntContacts, EntQuotes, EntInvoices, EntCredits, EntPayments, EntRecurring} {
		if !r.job.Options.want(ent) && ent != EntPayments {
			continue
		}
		l := apiLists[ent]
		for page := 1; ; page++ {
			r.setStep(0, "preview", fmt.Sprintf("Reading %s from Zoho — page %d", strings.ToLower(EntityLabels[ent]), page))
			e.heartbeat(r, c.Calls())
			rows, more, err := c.Page(ctx, l.path, l.key, page, l.extra)
			if err != nil {
				if fatal(err) {
					return nil, err
				}
				warnings = append(warnings, fmt.Sprintf("Could not read %s: %v", strings.ToLower(EntityLabels[ent]), err))
				break
			}
			for _, raw := range rows {
				switch ent {
				case EntItems:
					var z zItem
					if json.Unmarshal(raw, &z) == nil {
						items = append(items, z.norm())
					}
				case EntContacts:
					var z zContact
					if json.Unmarshal(raw, &z) == nil && !strings.EqualFold(string(z.ContactType), "vendor") {
						ct := z.norm()
						ct.ListOnly = true
						contacts = append(contacts, ct)
					}
				case EntPayments:
					var z struct {
						PaymentNumber flexStr `json:"payment_number"`
						CustomerName  flexStr `json:"customer_name"`
						UnusedAmount  flexNum `json:"unused_amount"`
						CurrencyCode  flexStr `json:"currency_code"`
					}
					if json.Unmarshal(raw, &z) == nil && float64(z.UnusedAmount) > 0.004 {
						warnings = append(warnings, fmt.Sprintf("Payment %s from %s has %s %.2f not applied to an invoice — it will not be imported.", z.PaymentNumber, z.CustomerName, z.CurrencyCode, float64(z.UnusedAmount)))
					}
				default:
					var z zDoc
					if json.Unmarshal(raw, &z) == nil {
						d := z.norm(l.kind)
						d.ListOnly = true
						docs = append(docs, d)
					}
				}
			}
			if !more {
				break
			}
		}
	}
	pv := e.buildPreview(im, items, contacts, docs, warnings)
	pv.OrgName = conn.ZohoOrgName
	// Estimate API calls (details, history, PDFs) against Zoho's daily caps.
	calls := len(contacts)
	for _, d := range docs {
		calls++
		if d.Kind != KindRecurring {
			if r.job.Options.want(EntHistory) {
				calls++
			}
			if r.job.Options.want(EntPDFs) {
				calls++
			}
		}
	}
	if calls > 900 {
		pv.Warnings = append(pv.Warnings, fmt.Sprintf("About %d Zoho API calls are needed. Zoho allows about 100 a minute and a daily number that depends on your plan (Free 1,000 · Standard 2,000 · Professional 5,000 · Premium and above 10,000). The import pauses when the daily limit is reached and continues by itself — or untick Original PDFs / Email & history to need fewer calls.", calls))
	}
	return pv, nil
}

func (e *Engine) previewFiles(r *run) (*Preview, error) {
	ds, err := e.dataset(&r.job)
	if err != nil {
		return nil, err
	}
	im := e.importer(r)
	im.SrcBase = im.Home
	var warnings []string
	warnings = append(warnings, ds.Unapplied...)
	if len(ds.Errors) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d row(s) can't be imported (see the error list after importing): e.g. row %d of %s — %s", len(ds.Errors), ds.Errors[0].Row, ds.Errors[0].Label, ds.Errors[0].Message))
	}
	pv := e.buildPreview(im, ds.Items, ds.Contacts, ds.Docs, warnings)
	// Persons count.
	np := 0
	for _, c := range ds.Contacts {
		np += len(c.Persons)
	}
	if np > 0 {
		pv.Entities = append(pv.Entities, EntityPreview{Entity: EntPersons, Label: EntityLabels[EntPersons], Count: np, New: np})
	}
	return pv, nil
}

// buildPreview counts, samples and matches records against existing data.
func (e *Engine) buildPreview(im *Importer, items []Item, contacts []Contact, docs []Doc, warnings []string) *Preview {
	im.load()
	pv := &Preview{BaseCurrency: firstNonEmpty(im.SrcBase, im.Home), HomeCurrency: im.Home, Warnings: []string{}, Duplicates: []Duplicate{}, BuiltAt: e.now().UTC().Format(time.RFC3339)}
	if im.SrcBase != "" && !strings.EqualFold(im.SrcBase, im.Home) {
		pv.Warnings = append(pv.Warnings, fmt.Sprintf("Your Zoho base currency is %s but your home currency here is %s. Documents in %s get rate 1; documents in other currencies need an exchange rate to %s set after importing (Settings → Organisation can change the home currency).", im.SrcBase, im.Home, im.Home, im.Home))
	}
	if len(items) > 0 {
		ep := EntityPreview{Entity: EntItems, Label: EntityLabels[EntItems], Count: len(items), Columns: []string{"Name", "Rate", "Tax"}}
		for _, it := range items {
			if ref := im.ref(entItemRef, firstNonEmpty(it.ExtID, "name:"+lowerTrim(it.Name))); ref != nil {
				ep.Linked++
			} else {
				ep.New++
			}
			if len(ep.Sample) < 5 {
				ep.Sample = append(ep.Sample, map[string]string{"Name": it.Name, "Rate": fmt.Sprintf("%.2f", it.Rate), "Tax": strings.TrimSpace(it.TaxName)})
			}
		}
		pv.Entities = append(pv.Entities, ep)
	}
	noEmail := 0
	if len(contacts) > 0 {
		ep := EntityPreview{Entity: EntContacts, Label: EntityLabels[EntContacts], Count: len(contacts), Columns: []string{"Name", "Email", "Currency", "Status"}}
		def := firstNonEmpty(im.Opts.OnCustomerMatch, "merge")
		for i := range contacts {
			c := &contacts[i]
			if c.primaryEmail() == "" {
				noEmail++
			}
			if ref := im.ref(entContactRef, c.ext()); ref != nil && im.exists("customers", ref.LocalID) {
				ep.Linked++
			} else if id, by := im.matchCustomer(c); id != "" {
				ep.Matches++
				pv.Duplicates = append(pv.Duplicates, Duplicate{Key: "contact:" + c.ext(), Entity: EntContacts, Label: c.displayName() + emailSuffix(c.primaryEmail()),
					MatchID: id, MatchLabel: im.custLabel[id], MatchBy: by, Default: def})
			} else {
				ep.New++
			}
			if len(ep.Sample) < 5 {
				st := "Active"
				if !c.Active {
					st = "Inactive"
				}
				ep.Sample = append(ep.Sample, map[string]string{"Name": c.displayName(), "Email": c.primaryEmail(), "Currency": firstNonEmpty(c.Currency, im.SrcBase), "Status": st})
			}
		}
		pv.Entities = append(pv.Entities, ep)
		if noEmail > 0 {
			pv.Warnings = append(pv.Warnings, fmt.Sprintf("%d customer(s) have no email address — they are imported, but invoices can't be emailed to them until one is added.", noEmail))
		}
	}
	byKind := map[string][]Doc{}
	for _, d := range docs {
		byKind[d.Kind] = append(byKind[d.Kind], d)
	}
	curr := map[string]*CurrencySum{}
	unsupported := map[string]int{}
	payments := 0
	for _, kind := range []string{KindQuote, KindInvoice, KindCredit, KindRecurring} {
		list := byKind[kind]
		if len(list) == 0 {
			continue
		}
		ent := entForKind(kind)
		ep := EntityPreview{Entity: ent, Label: EntityLabels[ent], Count: len(list), Columns: []string{"Number", "Customer", "Date", "Status", "Total"}}
		def := firstNonEmpty(im.Opts.OnDocMatch, "skip")
		var numbers []string
		for i := range list {
			d := &list[i]
			ext := firstNonEmpty(d.ExtID, "number:"+lowerTrim(d.Number))
			payments += len(d.Payments)
			if ref := im.ref(entDocRef+":"+kind, ext); ref != nil && im.exists("billing_invoices", ref.LocalID) {
				ep.Linked++
			} else if m := im.matchDoc(d); m != "" {
				ep.Matches++
				pv.Duplicates = append(pv.Duplicates, Duplicate{Key: "doc:" + kind + ":" + ext, Entity: ent, Label: d.Number + " · " + d.CustomerName,
					MatchID: m, MatchLabel: "Existing " + strings.ReplaceAll(docType(kind), "_", " ") + " " + d.Number, MatchBy: "number", Default: def})
			} else {
				ep.New++
			}
			cur := strings.ToUpper(firstNonEmpty(d.Currency, im.SrcBase, im.Home))
			if !currency.IsSupported(cur) {
				unsupported[cur]++
			}
			if kind == KindInvoice {
				st := mapStatus(kind, d.Status)
				cs := curr[cur]
				if cs == nil {
					cs = &CurrencySum{Currency: cur, Supported: currency.IsSupported(cur)}
					curr[cur] = cs
				}
				cs.Invoices++
				if st != "draft" && st != "void" {
					cs.Total += d.Total
					if !d.BalanceUnknown {
						cs.Outstanding += d.Balance
					}
				}
			}
			if kind == KindInvoice || kind == KindQuote {
				numbers = append(numbers, d.Number)
			}
			if len(ep.Sample) < 5 {
				total := fmt.Sprintf("%s %.2f", cur, d.Total)
				if d.Total == 0 && len(d.Lines) > 0 {
					total = cur + " (from lines)"
				}
				ep.Sample = append(ep.Sample, map[string]string{"Number": firstNonEmpty(d.Number, d.RecurName), "Customer": d.CustomerName, "Date": d.Date, "Status": d.Status, "Total": total})
			}
		}
		pv.Entities = append(pv.Entities, ep)
		if len(numbers) > 0 && im.Opts.continueNumbering() {
			best, bestN := "", -1
			prefCount := map[string]int{}
			for _, n := range numbers {
				if p, _, _, ok := splitNum(n); ok {
					prefCount[p]++
				}
			}
			pref, pc := "", -1
			for p, c := range prefCount {
				if c > pc || (c == pc && p < pref) {
					pref, pc = p, c
				}
			}
			width := 0
			for _, n := range numbers {
				if p, v, w, ok := splitNum(n); ok && p == pref && v > bestN {
					best, bestN, width = n, v, w
				}
			}
			// Numbers already used here with that prefix push it further (as on import).
			var used []string
			im.DB.Table("billing_invoices").Where("organization_id = ? AND doc_type = ? AND deleted_at IS NULL AND number LIKE ?", im.Org, docType(kind),
				strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(pref)+"%").Pluck("number", &used)
			for _, n := range used {
				if p, v, w, ok := splitNum(n); ok && p == pref && v > bestN {
					bestN, width = v, w
				}
			}
			if bestN >= 0 {
				pv.Numbering = append(pv.Numbering, NumberPreview{DocType: docType(kind), Highest: best, Next: fmt.Sprintf("%s%0*d", pref, width, bestN+1)})
			}
		}
	}
	if payments > 0 {
		pv.Entities = append(pv.Entities, EntityPreview{Entity: EntPayments, Label: EntityLabels[EntPayments], Count: payments, New: payments})
	}
	for cur, n := range unsupported {
		pv.Warnings = append(pv.Warnings, fmt.Sprintf("%d document(s) are in %s, which is not supported here — they will be listed as errors.", n, cur))
	}
	for _, cs := range curr {
		cs.Total = round2(cs.Total)
		cs.Outstanding = round2(cs.Outstanding)
		pv.Currencies = append(pv.Currencies, *cs)
	}
	sort.Slice(pv.Currencies, func(i, j int) bool { return pv.Currencies[i].Invoices > pv.Currencies[j].Invoices })
	pv.Warnings = append(pv.Warnings, warnings...)
	if pv.Currencies == nil {
		pv.Currencies = []CurrencySum{}
	}
	return pv
}

func emailSuffix(e string) string {
	if e == "" {
		return ""
	}
	return " <" + e + ">"
}

func round2(v float64) float64 { return float64(int64(v*100+sign(v)*0.5)) / 100 }

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
