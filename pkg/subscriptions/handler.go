package subscriptions

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Handler serves the API.
type Handler struct{ S *Service }

// RegisterRoutes mounts the authenticated routes (plan module: invoicing;
// admins and managers; settings and runs: admins).
//
//	GET    /subscriptions/dashboard                 MRR / ARR, active, past due, suspended, churned this month, renewals (30 days), upcoming suspensions, domains
//	GET    /subscriptions                           list (?status=&customer_id=&period=&supplier=&q=)
//	POST   /subscriptions                           create
//	GET    /subscriptions/:id                       drawer: lines, invoices, timeline, tasks, events, domains
//	PUT    /subscriptions/:id                       update;  DELETE (only without invoices)
//	POST   /subscriptions/:id/extend                {days, note}            give more time (moves the suspension; unsuspends a suspended one)
//	POST   /subscriptions/:id/mark-paid             {amount?, method, reference, date, email_receipt}
//	POST   /subscriptions/:id/suspend               {note}                  suspend now
//	POST   /subscriptions/:id/skip-suspension       {note, undo}            don't suspend this time (for the unpaid invoice)
//	POST   /subscriptions/:id/unsuspend             {days?, note}           unsuspend by hand
//	POST   /subscriptions/:id/cancel                {when: now|end_of_term, note}
//	POST   /subscriptions/:id/remind                email the client a reminder now
//	GET    /subscriptions/catalog                   catalogue services (?currency=&period= adds each one's price for it)
//	GET    /subscriptions/synergy-sites             hosted sites with client, subscription and suggested price
//	POST   /subscriptions/from-synergy              {domain, customer_id?, service_id?, price?, period?, start_date?, save_service?}
//	POST   /subscriptions/from-synergy/bulk         {domains?, dry_run?}    every matched, unbilled site
//	GET    /subscriptions/tasks                     provider tasks (?status=open|done|all)
//	PUT    /subscriptions/tasks/:id                 {status: done|open|dismissed, note}
//	GET    /subscriptions/events                    activity (?subscription_id=)
//	GET    /subscriptions/settings  PUT             billing, dunning and suspension settings (admins edit)
//	POST   /subscriptions/run                       run billing + dunning now (admins)
//	GET    /subscriptions/digest  POST              preview / email the daily digest now
//	POST   /subscriptions/import                    CSV (multipart "file" or {csv}); ?dry_run=1, create_customers=1
//	GET    /subscriptions/import/template.csv
//	GET    /subscriptions/customers/:id/portal-link the customer's signed "Your services" link
//	GET    /subscriptions/test-clock  POST          {date|days|reset, run}  (SUBSCRIPTIONS_TEST_CLOCK only, admins)
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/subscriptions")
	g.Use(h.guard)
	g.GET("", h.list)
	g.POST("", h.create)
	g.GET("/dashboard", h.dashboard)
	g.GET("/catalog", h.catalog)
	g.GET("/synergy-sites", h.synergySites)
	g.POST("/from-synergy", h.fromSynergy)
	g.POST("/from-synergy/bulk", h.bulkFromSynergy)
	g.GET("/tasks", h.tasks)
	g.PUT("/tasks/:id", h.updateTask)
	g.GET("/events", h.events)
	g.GET("/settings", h.settings)
	g.PUT("/settings", h.admin(h.saveSettings))
	g.POST("/run", h.admin(h.run))
	g.GET("/digest", h.digest)
	g.POST("/digest", h.admin(h.sendDigest))
	g.POST("/import", h.importCSV)
	g.GET("/import/template.csv", h.template)
	g.GET("/customers/:id/portal-link", h.portalLink)
	g.GET("/test-clock", h.admin(h.testClock))
	g.POST("/test-clock", h.admin(h.setTestClock))
	g.GET("/:id", h.get)
	g.PUT("/:id", h.update)
	g.DELETE("/:id", h.delete)
	g.POST("/:id/extend", h.extend)
	g.POST("/:id/mark-paid", h.markPaid)
	g.POST("/:id/suspend", h.suspendNow)
	g.POST("/:id/skip-suspension", h.skip)
	g.POST("/:id/unsuspend", h.unsuspendNow)
	g.POST("/:id/cancel", h.cancel)
	g.POST("/:id/remind", h.remind)
}

// RegisterPublic mounts GET /public/your-services/:token.
func (h *Handler) RegisterPublic(public *gin.RouterGroup) {
	public.GET("/your-services/:token", h.portal)
}

func ok(c *gin.Context, v any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": v}) }

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func failErr(c *gin.Context, err error) {
	var ve *ValidationError
	var ae *APIError
	switch {
	case errors.As(err, &ve):
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", ve.Msg)
	case errors.Is(err, ErrNotFound):
		fail(c, http.StatusNotFound, "NOT_FOUND", "Not found")
	case errors.As(err, &ae):
		st := ae.Status
		if st < 400 || st >= 500 {
			st = http.StatusBadRequest
		}
		fail(c, st, firstNonEmpty(ae.Code, "INVOICING_ERROR"), ae.Message)
	default:
		log.Printf("subscriptions: %v", err)
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", "Something went wrong — try again")
	}
}

func orgOf(c *gin.Context) string {
	for _, k := range []string{"organization_id", "org_id"} {
		if v, ok := c.Get(k); ok && v != nil {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func roleOf(c *gin.Context) string { return strings.ToLower(c.GetString("user_role")) }

// CanManage: admins and managers create and change subscriptions.
func CanManage(role string) bool {
	switch role {
	case "super_admin", "admin", "owner", "org_admin", "manager":
		return true
	}
	return false
}

func (h *Handler) guard(c *gin.Context) {
	if orgOf(c) == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "No organisation is associated with this account")
		c.Abort()
		return
	}
	if !CanManage(roleOf(c)) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Subscriptions are for admins and managers")
		c.Abort()
		return
	}
	c.Next()
}

func (h *Handler) admin(fn gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isAdminRole(roleOf(c)) {
			fail(c, http.StatusForbidden, "FORBIDDEN", "Only administrators can do this")
			return
		}
		fn(c)
	}
}

// locked runs fn under the organisation's lock (so it never races a run).
func (h *Handler) locked(c *gin.Context, fn func()) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	if !h.S.Locked(ctx, orgOf(c), fn) {
		fail(c, http.StatusServiceUnavailable, "BUSY", "Billing is running — try again in a minute")
	}
}

func bind(c *gin.Context, v any) bool {
	if err := c.ShouldBindJSON(v); err != nil && !errors.Is(err, io.EOF) {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return false
	}
	return true
}

// ---------------------------------------------------------------- subscriptions

func (h *Handler) list(c *gin.Context) {
	f := Filter{Status: c.Query("status"), CustomerID: c.Query("customer_id"), Period: c.Query("period"), Supplier: c.Query("supplier"), Q: c.Query("q")}
	org := orgOf(c)
	list := h.S.List(org, f)
	counts := map[string]int{}
	var rows []struct {
		Status string
		N      int
	}
	q := h.S.DB.Table("client_subscriptions").Select("status, COUNT(*) AS n").Where("organization_id = ?", org)
	if f.CustomerID != "" {
		q = q.Where("customer_id = ?", f.CustomerID)
	}
	q.Group("status").Scan(&rows)
	for _, r := range rows {
		counts[r.Status] = r.N
	}
	ok(c, gin.H{"subscriptions": list, "counts": counts, "home": h.S.home(org), "today": DayStr(h.S.Today(org))})
}

func (h *Handler) get(c *gin.Context) {
	d, err := h.S.GetDetail(orgOf(c), c.Param("id"))
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, d)
}

func (h *Handler) create(c *gin.Context) {
	var in SubInput
	if !bind(c, &in) {
		return
	}
	sub, err := h.S.Create(orgOf(c), c.GetString("user_id"), in)
	if err != nil {
		failErr(c, err)
		return
	}
	forgetSites(orgOf(c))
	h.kick(orgOf(c))
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": sub})
}

// kick runs billing for the organisation soon after a change (the first
// invoice of a subscription that is already due goes out straight away).
func (h *Handler) kick(org string) {
	go func() {
		defer func() { recover() }()
		time.Sleep(300 * time.Millisecond)
		h.S.RunOrgLocked(context.Background(), org, RunOptions{})
	}()
}

func (h *Handler) update(c *gin.Context) {
	var in SubInput
	if !bind(c, &in) {
		return
	}
	h.locked(c, func() {
		sub, err := h.S.Update(orgOf(c), c.Param("id"), in)
		if err != nil {
			failErr(c, err)
			return
		}
		forgetSites(orgOf(c))
		ok(c, sub)
	})
	h.kick(orgOf(c))
}

func (h *Handler) delete(c *gin.Context) {
	h.locked(c, func() {
		if err := h.S.Delete(orgOf(c), c.Param("id")); err != nil {
			failErr(c, err)
			return
		}
		forgetSites(orgOf(c))
		ok(c, gin.H{"deleted": true})
	})
}

type noteIn struct {
	Days int    `json:"days"`
	Note string `json:"note"`
	Undo bool   `json:"undo"`
}

func (h *Handler) extend(c *gin.Context) {
	var in noteIn
	if !bind(c, &in) {
		return
	}
	h.locked(c, func() {
		r, err := h.S.Extend(c.Request.Context(), orgOf(c), c.Param("id"), c.GetString("user_id"), in.Days, in.Note)
		if err != nil {
			failErr(c, err)
			return
		}
		ok(c, r)
	})
}

func (h *Handler) markPaid(c *gin.Context) {
	var in MarkPaidInput
	if !bind(c, &in) {
		return
	}
	var out *Subscription
	var err error
	h.locked(c, func() {
		out, err = h.S.MarkPaid(c.Request.Context(), orgOf(c), c.Param("id"), c.GetString("user_id"), roleOf(c), in)
	})
	if c.Writer.Written() {
		return
	}
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, out)
}

func (h *Handler) suspendNow(c *gin.Context) {
	var in noteIn
	if !bind(c, &in) {
		return
	}
	h.locked(c, func() {
		sub, err := h.S.SuspendNow(c.Request.Context(), orgOf(c), c.Param("id"), c.GetString("user_id"), in.Note)
		if err != nil {
			failErr(c, err)
			return
		}
		ok(c, sub)
	})
}

func (h *Handler) skip(c *gin.Context) {
	var in noteIn
	if !bind(c, &in) {
		return
	}
	h.locked(c, func() {
		sub, err := h.S.SkipSuspension(orgOf(c), c.Param("id"), c.GetString("user_id"), in.Note, in.Undo)
		if err != nil {
			failErr(c, err)
			return
		}
		ok(c, sub)
	})
}

func (h *Handler) unsuspendNow(c *gin.Context) {
	var in noteIn
	if !bind(c, &in) {
		return
	}
	h.locked(c, func() {
		sub, err := h.S.Unsuspend(c.Request.Context(), orgOf(c), c.Param("id"), c.GetString("user_id"), in.Days, in.Note)
		if err != nil {
			failErr(c, err)
			return
		}
		ok(c, sub)
	})
}

func (h *Handler) cancel(c *gin.Context) {
	var in CancelInput
	if !bind(c, &in) {
		return
	}
	h.locked(c, func() {
		sub, err := h.S.Cancel(c.Request.Context(), orgOf(c), c.Param("id"), c.GetString("user_id"), in)
		if err != nil {
			failErr(c, err)
			return
		}
		forgetSites(orgOf(c))
		ok(c, sub)
	})
}

func (h *Handler) remind(c *gin.Context) {
	h.locked(c, func() {
		to, err := h.S.SendReminder(c.Request.Context(), orgOf(c), c.Param("id"), c.GetString("user_id"), roleOf(c))
		if err != nil {
			failErr(c, err)
			return
		}
		ok(c, gin.H{"sent_to": to})
	})
}

// ---------------------------------------------------------------- catalogue, Synergy sites

// CatalogQuote is a catalogue service with its price for a subscription.
type CatalogQuote struct {
	CatalogService
	Quote     *float64 `json:"quote"` // per period in the currency asked, as invoices are written
	QuoteNote string   `json:"quote_note,omitempty"`
}

func (h *Handler) catalog(c *gin.Context) {
	org := orgOf(c)
	cur := strings.ToUpper(strings.TrimSpace(c.Query("currency")))
	period := c.Query("period")
	tax := h.S.taxDefaults(org)
	out := []CatalogQuote{}
	for _, svc := range h.S.Catalog(org) {
		q := CatalogQuote{CatalogService: svc}
		if cur != "" && PeriodMonths(period) > 0 {
			p := PerPeriod(svc.Price, svc.Period, period)
			switch {
			case svc.PriceIncludesTax && !tax.PricesIncludeTax:
				p = p / (1 + tax.Rate/100)
			case !svc.PriceIncludesTax && tax.PricesIncludeTax:
				p = p * (1 + tax.Rate/100)
			}
			if v, okk := h.S.convert(org, p, svc.Currency, cur); okk {
				v = RoundCur(v, cur)
				q.Quote = &v
				notes := []string{}
				if svc.Period != "" && svc.Period != period {
					notes = append(notes, money(svc.Price, svc.Currency)+" "+strings.ToLower(PeriodLabel(svc.Period))+" → per "+periodNoun(period))
				}
				if svc.Currency != cur {
					notes = append(notes, "converted from "+svc.Currency+" at today's rate")
				}
				q.QuoteNote = strings.Join(notes, ", ")
			} else {
				q.QuoteNote = "no exchange rate " + svc.Currency + " → " + cur
			}
		}
		out = append(out, q)
	}
	ok(c, gin.H{"services": out, "tax": tax, "home": h.S.home(org), "periods": Periods, "kinds": Kinds})
}

func (h *Handler) synergySites(c *gin.Context) {
	org := orgOf(c)
	if c.Query("refresh") == "1" {
		forgetSites(org)
	}
	rows := h.S.SynergySites(org)
	ready := 0
	for _, r := range rows {
		if r.Ready {
			ready++
		}
	}
	ok(c, gin.H{"sites": rows, "ready": ready, "home": h.S.home(org), "target_margin": h.S.SettingsFor(org).TargetMargin})
}

func (h *Handler) fromSynergy(c *gin.Context) {
	var in FromSynergyInput
	if !bind(c, &in) {
		return
	}
	sub, err := h.S.FromSynergy(orgOf(c), c.GetString("user_id"), in)
	if err != nil {
		failErr(c, err)
		return
	}
	h.kick(orgOf(c))
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": sub})
}

func (h *Handler) bulkFromSynergy(c *gin.Context) {
	var in struct {
		Domains []string `json:"domains"`
		DryRun  bool     `json:"dry_run"`
	}
	if !bind(c, &in) {
		return
	}
	res, err := h.S.BulkFromSynergy(orgOf(c), c.GetString("user_id"), in.Domains, in.DryRun)
	if err != nil {
		failErr(c, err)
		return
	}
	created := 0
	for _, r := range res {
		if r.OK && r.SubscriptionID != "" {
			created++
		}
	}
	if created > 0 {
		h.kick(orgOf(c))
	}
	ok(c, gin.H{"results": res, "created": created, "dry_run": in.DryRun})
}

// ---------------------------------------------------------------- tasks, events

func (h *Handler) tasks(c *gin.Context) {
	ok(c, gin.H{"tasks": h.S.Tasks(orgOf(c), TaskFilter{Status: c.DefaultQuery("status", "all"), SubscriptionID: c.Query("subscription_id")})})
}

func (h *Handler) updateTask(c *gin.Context) {
	var in struct{ Status, Note string }
	if !bind(c, &in) {
		return
	}
	t, err := h.S.UpdateTask(orgOf(c), c.Param("id"), c.GetString("user_id"), in.Status, in.Note)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, t)
}

func (h *Handler) events(c *gin.Context) {
	out := []Event{}
	q := h.S.DB.Where("organization_id = ?", orgOf(c))
	if id := c.Query("subscription_id"); id != "" {
		q = q.Where("subscription_id = ?", id)
	}
	q.Order("created_at DESC").Limit(300).Find(&out)
	type row struct {
		Event
		Title string `json:"title"`
	}
	rows := make([]row, 0, len(out))
	for _, e := range out {
		rows = append(rows, row{Event: e, Title: StageTitle(e)})
	}
	ok(c, gin.H{"events": rows})
}

// ---------------------------------------------------------------- settings, runs

func (h *Handler) settings(c *gin.Context) {
	org := orgOf(c)
	st := h.S.SettingsFor(org)
	pol := PolicyOf(st)
	// An example timeline for an invoice due in 14 days.
	today := h.S.Today(org)
	ex := []map[string]any{}
	d := pol.DatesFor(today.AddDate(0, 0, 14), st.GraceDays, 0, st.AutoSuspend, nil, today)
	for _, s := range pol.Steps(d) {
		ex = append(ex, map[string]any{"date": DayStr(s.Date), "key": s.Key, "kind": s.Kind, "audience": s.Audience, "label": StepLabel(s), "offset": s.Offset})
	}
	api := false
	why := ""
	if h.S.Exp != nil {
		cl, w := h.S.Exp.SynergyAPI(org)
		api, why = cl != nil, w
	}
	ok(c, gin.H{"settings": st, "example": ex, "today": DayStr(today), "test_clock": h.S.TestClock, "recipients": h.S.AdminRecipients(org, st),
		"can_edit": isAdminRole(roleOf(c)), "min_notice_days": MinNoticeDays, "synergy_api": api, "synergy_api_note": why})
}

func (h *Handler) saveSettings(c *gin.Context) {
	var in SettingsInput
	if !bind(c, &in) {
		return
	}
	st, err := h.S.SaveSettings(orgOf(c), in)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, gin.H{"settings": st})
}

func (h *Handler) run(c *gin.Context) {
	r, okk := h.S.RunOrgLocked(c.Request.Context(), orgOf(c), RunOptions{Force: true})
	if !okk {
		fail(c, http.StatusConflict, "BUSY", "Billing is already running — try again in a minute")
		return
	}
	ok(c, r)
}

func (h *Handler) digest(c *gin.Context) { ok(c, h.S.Digest(orgOf(c))) }

func (h *Handler) sendDigest(c *gin.Context) {
	status, err := h.S.SendDigest(c.Request.Context(), orgOf(c), "", true)
	if err != nil {
		fail(c, http.StatusBadGateway, "EMAIL_FAILED", "The digest could not be emailed: "+err.Error())
		return
	}
	ok(c, gin.H{"status": status})
}

func (h *Handler) testClock(c *gin.Context) {
	if !h.S.TestClock {
		fail(c, http.StatusNotFound, "NOT_FOUND", "The test clock is off (SUBSCRIPTIONS_TEST_CLOCK)")
		return
	}
	org := orgOf(c)
	ok(c, gin.H{"today": DayStr(h.S.Today(org)), "offset_days": h.S.SettingsFor(org).TestOffsetDays})
}

func (h *Handler) setTestClock(c *gin.Context) {
	if !h.S.TestClock {
		fail(c, http.StatusNotFound, "NOT_FOUND", "The test clock is off (SUBSCRIPTIONS_TEST_CLOCK)")
		return
	}
	var in struct {
		Date  string `json:"date"`
		Days  *int   `json:"days"`
		Reset bool   `json:"reset"`
		Run   bool   `json:"run"`
	}
	if !bind(c, &in) {
		return
	}
	org := orgOf(c)
	today, off, err := h.S.SetTestClock(org, in.Date, in.Days, in.Reset)
	if err != nil {
		failErr(c, err)
		return
	}
	out := gin.H{"today": DayStr(today), "offset_days": off}
	if in.Run {
		r, okk := h.S.RunOrgLocked(c.Request.Context(), org, RunOptions{Force: true})
		if !okk {
			fail(c, http.StatusConflict, "BUSY", "Billing is already running — try again")
			return
		}
		out["run"] = r
	}
	ok(c, out)
}

// ---------------------------------------------------------------- import

func (h *Handler) importCSV(c *gin.Context) {
	var rdr io.Reader
	dry := c.Query("dry_run") == "1" || c.PostForm("dry_run") == "1"
	create := c.Query("create_customers") == "1" || c.PostForm("create_customers") == "1"
	if fh, err := c.FormFile("file"); err == nil {
		f, err := fh.Open()
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Could not read the file")
			return
		}
		defer f.Close()
		rdr = io.LimitReader(f, 5<<20)
	} else {
		var in struct {
			CSV             string `json:"csv"`
			DryRun          bool   `json:"dry_run"`
			CreateCustomers bool   `json:"create_customers"`
		}
		if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.CSV) == "" {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Upload a CSV file")
			return
		}
		rdr, dry, create = strings.NewReader(in.CSV), dry || in.DryRun, create || in.CreateCustomers
	}
	res, err := h.S.Import(orgOf(c), c.GetString("user_id"), rdr, dry, create)
	if err != nil {
		failErr(c, err)
		return
	}
	if !dry && res.Created > 0 {
		forgetSites(orgOf(c))
		h.kick(orgOf(c))
	}
	ok(c, res)
}

func (h *Handler) template(c *gin.Context) {
	c.Header("Content-Disposition", `attachment; filename="subscriptions-template.csv"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", []byte(CSVTemplate))
}

func (h *Handler) portalLink(c *gin.Context) {
	org := orgOf(c)
	if _, okk := h.S.customer(org, c.Param("id")); !okk {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Customer not found")
		return
	}
	tok := h.S.PortalToken(org, c.Param("id"))
	ok(c, gin.H{"token": tok, "url": h.S.PortalURL(org, c.Param("id")), "path": "/your-services/" + tok})
}

// ---------------------------------------------------------------- public

func (h *Handler) portal(c *gin.Context) {
	v, err := h.S.Portal(c.Param("token"))
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "This link is not valid — ask for a new one")
		return
	}
	c.Header("Cache-Control", "no-store")
	ok(c, v)
}

func (h *Handler) dashboard(c *gin.Context) { ok(c, h.S.Dashboard(orgOf(c))) }
