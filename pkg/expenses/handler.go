package expenses

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/branding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
)

// Handler serves /api/v1/expenses/... and the public Google OAuth callback.
type Handler struct{ S *Service }

// RegisterRoutes mounts the routes.
func (h *Handler) RegisterRoutes(auth, public *gin.RouterGroup) {
	g := auth.Group("/expenses")
	g.GET("/dashboard", h.dashboard)
	g.GET("/summary", h.summary)
	g.GET("/items", h.list)
	g.POST("/items", h.create)
	g.GET("/items/:id", h.get)
	g.PATCH("/items/:id", h.update)
	g.DELETE("/items/:id", h.del)
	g.POST("/items/:id/approve", h.approve)
	g.POST("/items/:id/unapprove", h.unapprove)
	g.POST("/items/:id/ignore", h.ignore)
	g.POST("/items/:id/restore", h.restore)
	g.PUT("/items/:id/allocations", h.allocations)
	g.POST("/bulk-approve", h.bulkApprove)
	g.POST("/uploads", h.upload)
	g.GET("/documents/:id", h.document)
	g.GET("/vendors", h.vendors)
	g.POST("/vendors", h.saveVendor)
	g.PATCH("/vendors/:id", h.saveVendor)
	g.GET("/vendors/:id/detail", h.vendorDetail)
	g.GET("/renewals", h.renewals)
	g.POST("/renewals", h.saveRenewal)
	g.PATCH("/renewals/:id", h.saveRenewal)
	g.GET("/clients", h.clients)
	g.GET("/alerts", h.alerts)
	g.POST("/alerts/dismiss", h.dismiss)
	g.GET("/export.csv", h.export)
	g.GET("/sources", h.sources)
	g.PUT("/sources/synergy", h.saveSynergy)
	g.POST("/sources/synergy/test", h.testSynergy)
	g.DELETE("/sources/synergy", h.deleteSynergy)
	g.PUT("/sources/gmail", h.saveGmail)
	g.POST("/sources/gmail/connect", h.connectGmail)
	g.DELETE("/sources/gmail", h.deleteGmail)
	g.PUT("/sources/schedule", h.saveSchedule)
	g.POST("/sync", h.sync)
	h.registerSynergy(g)
	public.GET("/google/callback", h.googleCallback)
}

func orgOf(c *gin.Context) string {
	for _, k := range []string{"org_id", "organization_id"} {
		if v := c.GetString(k); v != "" {
			return v
		}
	}
	return ""
}

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func ok(c *gin.Context, data any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": data}) }

func (h *Handler) org(c *gin.Context) (string, bool) {
	o := orgOf(c)
	if o == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
		return "", false
	}
	return o, true
}

// admin: changing sources, approving and editing need a managing role.
func (h *Handler) admin(c *gin.Context) (string, bool) {
	o, okk := h.org(c)
	if !okk {
		return "", false
	}
	if !orgprefs.CanEdit(c.GetString("user_role")) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only owners, admins and managers can change expenses")
		return "", false
	}
	return o, true
}

func errOut(c *gin.Context, err error) {
	var ve *ValidationError
	var fe *finance.ValidationError
	var se *SynergyError
	switch {
	case errors.Is(err, ErrNotFound) || errors.Is(err, finance.ErrNotFound):
		fail(c, http.StatusNotFound, "NOT_FOUND", "Expense not found")
	case errors.As(err, &ve):
		fail(c, http.StatusBadRequest, "VALIDATION", ve.Msg)
	case errors.As(err, &fe):
		fail(c, http.StatusBadRequest, "VALIDATION", fe.Error())
	case errors.As(err, &se):
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": se.Error(), "error": gin.H{"code": "SYNERGY_" + se.Status, "message": se.Message, "hint": se.Hint, "ip_blocked": se.IPBlocked}})
	default:
		fail(c, http.StatusInternalServerError, "ERROR", err.Error())
	}
}

func (h *Handler) dashboard(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	h.S.EnsureSynergyBackfill(o)
	ok(c, h.S.DashboardFor(o, reqtz.Now(c), c.Query("from"), c.Query("to")))
}

// summary is a compact view for the main dashboard card and AI tools.
func (h *Handler) summary(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	d := h.S.DashboardFor(o, reqtz.Now(c), c.Query("from"), c.Query("to"))
	vendor := strings.ToLower(strings.TrimSpace(c.Query("vendor")))
	subs := d.Subscriptions
	byV := d.ByVendor
	if vendor != "" {
		subs = []Subscription{}
		for _, s := range d.Subscriptions {
			if strings.Contains(strings.ToLower(s.Vendor+" "+s.Service), vendor) {
				subs = append(subs, s)
			}
		}
		byV = []map[string]any{}
		for _, v := range d.ByVendor {
			if strings.Contains(strings.ToLower(fmt.Sprint(v["vendor"])), vendor) {
				byV = append(byV, v)
			}
		}
	}
	months := d.Months
	if len(months) > 6 {
		months = months[len(months)-6:]
	}
	alerts := d.Alerts
	if len(alerts) > 8 {
		alerts = alerts[:8]
	}
	top := byV
	if len(top) > 8 {
		top = top[:8]
	}
	ok(c, gin.H{"currency": d.Currency, "today": d.Today, "kpis": d.KPIs, "by_vendor": top, "by_category": d.ByCategory, "subscriptions": subs,
		"alerts": alerts, "forecast": d.Forecast, "months": months, "balance": d.Balance, "range": d.Range, "sources": d.Sources, "synergy": d.Synergy,
		"note": "Amounts are approved expenses in the home currency excluding GST/VAT, dated by invoice date (the same figures the P&L counts)."})
}

func (h *Handler) list(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	lim, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	items, total := h.S.List(o, ListFilter{Status: c.Query("status"), VendorID: c.Query("vendor_id"), Category: c.Query("category"), Q: c.Query("q"),
		From: validDay(c.Query("from")), To: validDay(c.Query("to")), Source: c.Query("source"), CustomerID: c.Query("customer_id"), Limit: lim})
	ok(c, gin.H{"items": items, "total": total})
}

func validDay(s string) string {
	if _, err := time.Parse(dayFmt, s); err == nil {
		return s
	}
	return ""
}

func (h *Handler) get(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	it, err := h.S.Get(o, c.Param("id"))
	if err != nil {
		errOut(c, err)
		return
	}
	var dup *Item
	if it.DuplicateOf != "" {
		dup, _ = h.S.Get(o, it.DuplicateOf)
	}
	var dups []Item
	h.S.DB.Where("organization_id = ? AND duplicate_of = ?", o, it.ID).Order("created_at").Find(&dups)
	ok(c, gin.H{"item": it, "duplicate_of": dup, "duplicates": dups, "ai_enabled": AIEnabled()})
}

func (h *Handler) create(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in ItemInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	it, err := h.S.CreateManual(o, in)
	if err != nil {
		errOut(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": it})
}

func (h *Handler) update(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in ItemInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	it, err := h.S.Update(o, c.Param("id"), in)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, it)
}

func (h *Handler) del(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	if err := h.S.Delete(o, c.Param("id")); err != nil {
		errOut(c, err)
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) approve(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in ApproveInput
	_ = c.ShouldBindJSON(&in)
	it, err := h.S.Approve(o, c.GetString("user_id"), c.Param("id"), in)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, it)
}

func (h *Handler) bulkApprove(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in struct {
		IDs []string `json:"ids"`
		ApproveInput
	}
	if err := c.ShouldBindJSON(&in); err != nil || len(in.IDs) == 0 {
		fail(c, http.StatusBadRequest, "VALIDATION", "Choose expenses to approve")
		return
	}
	approved, failed := 0, []gin.H{}
	for _, id := range in.IDs {
		if _, err := h.S.Approve(o, c.GetString("user_id"), id, in.ApproveInput); err != nil {
			msg := err.Error()
			failed = append(failed, gin.H{"id": id, "message": msg})
			continue
		}
		approved++
	}
	ok(c, gin.H{"approved": approved, "failed": failed})
}

func (h *Handler) unapprove(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	it, err := h.S.Unapprove(o, c.GetString("user_id"), c.Param("id"))
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, it)
}

func (h *Handler) ignore(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&in)
	it, err := h.S.SetStatus(o, c.Param("id"), StatusIgnored, nonEmpty(in.Reason, "Ignored"))
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, it)
}

func (h *Handler) restore(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	it, err := h.S.SetStatus(o, c.Param("id"), StatusPending, "")
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, it)
}

func (h *Handler) allocations(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in struct {
		Allocations []Allocation `json:"allocations"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	it, err := h.S.SetAllocations(o, c.Param("id"), in.Allocations)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, it)
}

func (h *Handler) upload(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	form, err := c.MultipartForm()
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Upload files as multipart form data")
		return
	}
	files := form.File["files"]
	files = append(files, form.File["file"]...)
	if len(files) == 0 {
		fail(c, http.StatusBadRequest, "VALIDATION", "Choose at least one file")
		return
	}
	if len(files) > 25 {
		fail(c, http.StatusBadRequest, "VALIDATION", "Upload at most 25 files at a time")
		return
	}
	vendorID, category := c.PostForm("vendor_id"), c.PostForm("category")
	// dates: {"hosting-account-usage.csv": "2026-10-01"} — snapshot dates chosen for usage reports.
	dates := map[string]string{}
	if v := strings.TrimSpace(c.PostForm("dates")); v != "" {
		if err := json.Unmarshal([]byte(v), &dates); err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", "dates must be a JSON object of file name → YYYY-MM-DD")
			return
		}
	}
	results := []*UploadResult{}
	errs := []gin.H{}
	for _, fh := range files {
		if fh.Size > 15<<20 {
			errs = append(errs, gin.H{"file": fh.Filename, "message": "larger than 15 MB"})
			continue
		}
		f, err := fh.Open()
		if err != nil {
			errs = append(errs, gin.H{"file": fh.Filename, "message": err.Error()})
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(f, 16<<20))
		f.Close()
		var snap *time.Time
		if d, ok := dates[fh.Filename]; ok {
			t, perr := time.Parse(dayFmt, strings.TrimSpace(d))
			if perr != nil {
				errs = append(errs, gin.H{"file": fh.Filename, "message": "the snapshot date must look like 2026-10-01"})
				continue
			}
			snap = &t
		}
		r, err := h.S.UploadAs(c.Request.Context(), o, c.GetString("user_id"), fh.Filename, fh.Header.Get("Content-Type"), data, vendorID, category, snap)
		if err != nil {
			errs = append(errs, gin.H{"file": fh.Filename, "message": err.Error()})
			continue
		}
		results = append(results, r)
	}
	ok(c, gin.H{"results": results, "errors": errs, "ai_enabled": AIEnabled()})
}

func (h *Handler) document(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	var d Document
	if h.S.DB.Where("organization_id = ? AND id = ?", o, c.Param("id")).First(&d).Error != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Document not found")
		return
	}
	disp := "inline"
	if c.Query("download") == "1" {
		disp = "attachment"
	}
	c.Header("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disp, strings.ReplaceAll(d.Filename, `"`, "")))
	c.Header("Cache-Control", "private, max-age=300")
	c.Data(http.StatusOK, nonEmpty(d.MimeType, "application/octet-stream"), d.Data)
}

func (h *Handler) vendors(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	cats := []gin.H{}
	for _, k := range Categories {
		cats = append(cats, gin.H{"key": k, "label": CategoryLabels[k]})
	}
	ok(c, gin.H{"vendors": h.S.Vendors(o), "categories": cats})
}

func (h *Handler) saveVendor(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in VendorInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	v, err := h.S.SaveVendor(o, c.Param("id"), in)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, v)
}

func (h *Handler) vendorDetail(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	id := c.Param("id")
	v := h.S.vendorByID(o, id)
	if v == nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Vendor not found")
		return
	}
	today := Day(reqtz.Now(c))
	charges, _ := h.S.approvedCharges(o, id)
	items, _ := h.S.List(o, ListFilter{VendorID: id, Limit: 500})
	subs := DetectSubscriptions(charges, today)
	ms := monthStart(today)
	months := []gin.H{}
	for m := ms.AddDate(0, -23, 0); !m.After(ms); m = m.AddDate(0, 1, 0) {
		val, n := sumNet(charges, m, m.AddDate(0, 1, -1))
		months = append(months, gin.H{"month": m.Format("2006-01"), "amount": val, "count": n})
	}
	ytd, _ := sumNet(charges, time.Date(today.Year(), 1, 1, 0, 0, 0, 0, time.UTC), today)
	all, n := sumNet(charges, time.Time{}, today.AddDate(5, 0, 0))
	var rens []Renewal
	for _, r := range h.S.Renewals(o, 0) {
		if r.VendorID == id {
			rens = append(rens, r)
		}
	}
	docs := []gin.H{}
	seen := map[string]bool{}
	for _, it := range items {
		if it.DocumentID != "" && !seen[it.DocumentID] {
			seen[it.DocumentID] = true
			d := ""
			if it.InvoiceDate != nil {
				d = it.InvoiceDate.Format(dayFmt)
			}
			docs = append(docs, gin.H{"id": it.DocumentID, "name": it.DocumentName, "item_id": it.ID, "date": d, "invoice_number": it.InvoiceNumber})
		}
	}
	ok(c, gin.H{"vendor": v, "items": items, "subscriptions": subs, "months": months, "ytd": ytd, "total": all, "count": n, "renewals": rens,
		"documents": docs, "currency": h.S.Home(o)})
}

func (h *Handler) renewals(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	within, _ := strconv.Atoi(c.Query("within"))
	ok(c, gin.H{"renewals": h.S.Renewals(o, within)})
}

func (h *Handler) saveRenewal(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in RenewalInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	r, err := h.S.SaveRenewal(o, c.Param("id"), in)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, r)
}

func (h *Handler) clients(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	today := Day(reqtz.Now(c))
	from, to := today.AddDate(-1, 0, 1), today
	if t, err := time.Parse(dayFmt, c.Query("from")); err == nil {
		from = t
	}
	if t, err := time.Parse(dayFmt, c.Query("to")); err == nil {
		to = t
	}
	ok(c, gin.H{"clients": h.S.ClientMargins(o, from, to), "currency": h.S.Home(o), "from": from.Format(dayFmt), "to": to.Format(dayFmt)})
}

func (h *Handler) alerts(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	d := h.S.DashboardFor(o, reqtz.Now(c), "", "")
	ok(c, gin.H{"alerts": d.Alerts})
}

func (h *Handler) dismiss(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in struct {
		Key string `json:"key"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Key == "" {
		fail(c, http.StatusBadRequest, "VALIDATION", "Missing alert key")
		return
	}
	if err := h.S.Dismiss(o, in.Key); err != nil {
		errOut(c, err)
		return
	}
	ok(c, gin.H{"dismissed": true})
}

func (h *Handler) export(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	st := c.DefaultQuery("status", StatusApproved)
	items, _ := h.S.List(o, ListFilter{Status: st, VendorID: c.Query("vendor_id"), From: validDay(c.Query("from")), To: validDay(c.Query("to")), Limit: 1000})
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"Invoice date", "Vendor", "Service", "Category", "Invoice number", "Currency", "Total", "Tax", "Net", "Home currency", "Exchange rate", "Rate source",
		"Home total", "Home tax", "Home net", "Paid", "Status", "Source", "Client", "Bill", "Notes"})
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	for _, it := range items {
		d := ""
		if it.InvoiceDate != nil {
			d = it.InvoiceDate.Format(dayFmt)
		}
		client := it.CustomerName
		for _, a := range it.Allocations {
			if client != "" {
				client += "; "
			}
			client += fmt.Sprintf("%s %.0f%%", a.CustomerName, a.Percent)
		}
		_ = w.Write([]string{d, it.VendorName, it.Service, CategoryLabels[it.Category], it.InvoiceNumber, it.Currency, f(it.Total), f(it.Tax), f(it.Net), it.HomeCurrency,
			trimFloat(it.FxRate), it.FxSource, f(it.HomeTotal), f(it.HomeTax), f(it.HomeNet), map[bool]string{true: "yes", false: "no"}[it.Paid], it.Status, it.Source,
			client, it.BillNumber, it.Notes})
	}
	w.Flush()
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="expenses-%s.csv"`, time.Now().Format("2006-01-02")))
	c.Data(http.StatusOK, "text/csv; charset=utf-8", b.Bytes())
}

// RedirectURI is the OAuth redirect: ${PUBLIC_API_URL}/api/v1/public/google/callback.
func RedirectURI(r *http.Request) string {
	return branding.BaseURL(r) + "/api/v1/public/google/callback"
}

func (h *Handler) sources(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	out := gin.H{}
	for _, k := range []string{"synergy", "gmail", "schedule"} {
		cn := h.S.connection(o, k)
		if cn == nil {
			out[k] = nil
			continue
		}
		var last any
		if cn.LastResult != "" {
			last = jsonRaw(cn.LastResult)
		}
		row := gin.H{"enabled": cn.Enabled, "account": cn.Account, "connected": cn.SecretEnc != "", "settings": cn.Settings, "last_sync_at": cn.LastSyncAt,
			"last_status": cn.LastStatus, "last_error": cn.LastError, "last_result": last, "balance": cn.Balance, "balance_at": cn.BalanceAt,
			"client_id": cn.ClientID, "has_client_secret": cn.ClientSecret != ""}
		out[k] = row
	}
	envID, envSecret := GoogleClientFromEnv()
	presets := Presets
	if v := h.S.vendorByKey(o, "synergy"); v != nil {
		out["synergy_count_from"] = nonEmpty(v.CountFrom, "invoices")
	}
	out["presets"] = presets
	out["redirect_uri"] = RedirectURI(c.Request)
	out["google_env_client"] = envID != "" && envSecret != ""
	out["egress_ip"] = h.S.CachedEgressIP()
	out["ai_enabled"] = AIEnabled()
	out["can_edit"] = orgprefs.CanEdit(c.GetString("user_role"))
	out["synergy_endpoint"] = NewSynergyClient("", "").URL
	ok(c, out)
}

func (h *Handler) saveSynergy(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in SynergyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	if _, err := h.S.SaveSynergy(o, in); err != nil {
		errOut(c, err)
		return
	}
	h.sources(c)
}

func (h *Handler) testSynergy(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	bal, err := h.S.TestSynergy(c.Request.Context(), o)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, gin.H{"balance": bal, "currency": "AUD", "message": "Connected — account balance " + fmtMoney(bal, "AUD")})
}

func (h *Handler) deleteSynergy(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	if err := h.S.DisconnectSynergy(o); err != nil {
		errOut(c, err)
		return
	}
	ok(c, gin.H{"disconnected": true})
}

func (h *Handler) saveGmail(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in GmailInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	if _, err := h.S.SaveGmail(o, in); err != nil {
		errOut(c, err)
		return
	}
	h.sources(c)
}

func (h *Handler) connectGmail(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	u, err := h.S.StartGmail(o, c.GetString("user_id"), RedirectURI(c.Request), appOrigin(c))
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, gin.H{"url": u})
}

func (h *Handler) deleteGmail(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	if err := h.S.DisconnectGmail(o); err != nil {
		errOut(c, err)
		return
	}
	ok(c, gin.H{"disconnected": true})
}

func (h *Handler) saveSchedule(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in ScheduleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	if _, err := h.S.SaveSchedule(o, in); err != nil {
		errOut(c, err)
		return
	}
	h.sources(c)
}

func (h *Handler) sync(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in struct {
		Source string `json:"source"`
	}
	_ = c.ShouldBindJSON(&in)
	ctx := c.Request.Context()
	var res []*SyncResult
	locked := h.S.withLock(ctx, "expenses-sync:"+o, func(_ *gorm.DB) {
		switch in.Source {
		case "synergy":
			res = []*SyncResult{h.S.SyncSynergy(ctx, o)}
			h.S.NotifyAlerts(o)
		case "gmail":
			res = []*SyncResult{h.S.SyncGmail(ctx, o)}
			h.S.NotifyAlerts(o)
		default:
			res = h.S.SyncAll(ctx, o)
		}
	})
	if !locked {
		fail(c, http.StatusConflict, "SYNC_RUNNING", "A sync is already running — try again in a minute")
		return
	}
	if len(res) == 0 {
		fail(c, http.StatusBadRequest, "NO_SOURCES", "Connect Synergy Wholesale or Gmail first")
		return
	}
	ok(c, gin.H{"results": res})
}

func (h *Handler) googleCallback(c *gin.Context) {
	app := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_APP_URL")), "/")
	if st := c.Query("state"); st != "" {
		var cn Connection
		if h.S.DB.Where("kind = ? AND state = ?", "gmail", st).First(&cn).Error == nil && cn.ReturnURL != "" {
			app = cn.ReturnURL
		}
	}
	back := func(q url.Values) {
		target := app + "/expenses?tab=sources&" + q.Encode()
		c.Redirect(http.StatusFound, target)
	}
	if e := c.Query("error"); e != "" {
		msg := e
		if e == "access_denied" {
			msg = "Access was not granted"
		}
		// Clear the pending state.
		if st := c.Query("state"); st != "" {
			h.S.DB.Model(&Connection{}).Where("kind = ? AND state = ?", "gmail", st).Updates(map[string]any{"state": "", "last_error": msg})
		}
		back(url.Values{"gmail": {"error"}, "message": {msg}})
		return
	}
	_, err := h.S.FinishGmail(c.Request.Context(), c.Query("state"), c.Query("code"), RedirectURI(c.Request))
	if err != nil {
		back(url.Values{"gmail": {"error"}, "message": {err.Error()}})
		return
	}
	back(url.Values{"gmail": {"connected"}})
}

// appOrigin is where the browser app runs (to come back after Google sign-in):
// PUBLIC_APP_URL, else the request's Origin / Referer.
func appOrigin(c *gin.Context) string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_APP_URL")), "/"); v != "" {
		return v
	}
	for _, h := range []string{c.GetHeader("Origin"), c.GetHeader("Referer")} {
		if u, err := url.Parse(h); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	return ""
}

func jsonRaw(s string) any {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return nil
	}
	return v
}
