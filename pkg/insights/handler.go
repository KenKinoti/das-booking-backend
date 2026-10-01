package insights

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
)

// Migrate creates the tables and the indexes the analytics queries use.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&ServiceRule{}, &WBCache{}, &CustomIndicator{}, &Dataset{}); err != nil {
		return err
	}
	for _, q := range []string{
		`CREATE INDEX IF NOT EXISTS idx_ins_inv_org_type_issue ON billing_invoices (organization_id, doc_type, issue_date) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_ins_inv_org_customer ON billing_invoices (organization_id, customer_id) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_ins_pay_org_date ON billing_payments (organization_id, date)`,
		`CREATE INDEX IF NOT EXISTS idx_ins_pay_invoice_date ON billing_payments (invoice_id, date)`,
		`CREATE INDEX IF NOT EXISTS idx_ins_exp_org_date ON expense_items (organization_id, invoice_date)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			log.Printf("insights: index: %v", err)
		}
	}
	return nil
}

// Handler serves /insights/... and the advanced /analytics/... endpoints.
type Handler struct{ S *Service }

// RegisterRoutes mounts the routes on the authenticated API group.
//
//	GET  /insights/dashboard                 business snapshot, imported history, hosting, 90-day cash forecast
//	GET  /insights/opportunities             internal signals with estimated value and actions
//	GET  /insights/market                    World Bank indicators (?refresh=1), FX, datasets
//	POST /insights/market/explain            optional AI explanation
//	POST /insights/market/indicators         follow a custom indicator {code, country, label}
//	DELETE /insights/market/indicators/:id
//	GET/POST /insights/datasets, DELETE /insights/datasets/:id   uploaded CSV series
//	GET  /insights/export                    dataset catalogue; /insights/export/:dataset (CSV, ?from&to)
//	GET  /analytics/revenue|clients|payments|seasonality|service-lines|hosting
//	GET/PUT/DELETE /analytics/service-map    keyword → service line mapping
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/insights")
	g.GET("/dashboard", h.dashboard)
	g.GET("/opportunities", h.opportunities)
	g.GET("/market", h.market)
	g.POST("/market/explain", h.explain)
	g.POST("/market/indicators", h.addIndicator)
	g.DELETE("/market/indicators/:id", h.deleteIndicator)
	g.GET("/datasets", h.datasets)
	g.POST("/datasets", h.uploadDataset)
	g.DELETE("/datasets/:id", h.deleteDataset)
	g.GET("/export", h.exportList)
	g.GET("/export/:dataset", h.export)

	a := auth.Group("/analytics")
	a.GET("/revenue", h.revenue)
	a.GET("/clients", h.clients)
	a.GET("/payments", h.payments)
	a.GET("/seasonality", h.seasonality)
	a.GET("/service-lines", h.serviceLines)
	a.GET("/hosting", h.hosting)
	a.GET("/service-map", h.getMap)
	a.PUT("/service-map", h.putMap)
	a.DELETE("/service-map", h.resetMap)
}

func ok(c *gin.Context, v interface{}) { c.JSON(http.StatusOK, gin.H{"success": true, "data": v}) }

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func orgOf(c *gin.Context) (string, bool) {
	for _, k := range []string{"organization_id", "org_id"} {
		if v := strings.TrimSpace(c.GetString(k)); v != "" {
			return v, true
		}
	}
	fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
	return "", false
}

func canEdit(c *gin.Context) bool {
	switch strings.ToLower(c.GetString("user_role")) {
	case "super_admin", "admin", "owner", "manager", "org_admin":
		return true
	}
	return false
}

func (h *Handler) opts(c *gin.Context, defMonths int) (Opts, bool) {
	r, err := ParseRange(reqtz.Now(c), c.Query("from"), c.Query("to"), defMonths)
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_RANGE", err.Error())
		return Opts{}, false
	}
	o := Opts{Range: r, Currency: c.Query("currency"), Client: strings.TrimSpace(c.Query("client")), Line: strings.TrimSpace(c.Query("line")),
		Month: strings.TrimSpace(c.Query("month")), Grain: c.Query("grain"), Today: day(reqtz.Now(c))}
	if o.Line != "" {
		if _, ok := LineLabels[o.Line]; !ok {
			fail(c, http.StatusBadRequest, "INVALID_LINE", "Unknown service line")
			return o, false
		}
	}
	if o.Month != "" {
		if _, err := time.Parse("2006-01", o.Month); err != nil {
			fail(c, http.StatusBadRequest, "INVALID_MONTH", "month must be YYYY-MM")
			return o, false
		}
	}
	return o, true
}

func (h *Handler) dashboard(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	ok(c, h.S.DashboardFor(org, reqtz.Now(c), reqtz.Location(c)))
}

func (h *Handler) opportunities(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	ok(c, h.S.OpportunitiesFor(org, reqtz.Now(c)))
}

func (h *Handler) revenue(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	o, good := h.opts(c, 24)
	if !good {
		return
	}
	o.Range = h.S.clampAll(org, o.Range)
	ok(c, h.S.RevenueBreakdown(org, o))
}

func (h *Handler) clients(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	o, good := h.opts(c, 36)
	if !good {
		return
	}
	o.Range = h.S.clampAll(org, o.Range)
	ok(c, h.S.ClientAnalytics(org, o))
}

func (h *Handler) payments(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	o, good := h.opts(c, 24)
	if !good {
		return
	}
	o.Range = h.S.clampAll(org, o.Range)
	if o.Range.Months() > 120 {
		o.Range.From = monthStart(o.Range.To).AddDate(0, -119, 0)
	}
	ok(c, h.S.PaymentAnalytics(org, o))
}

func (h *Handler) seasonality(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	o, good := h.opts(c, 36)
	if !good {
		return
	}
	o.Range = h.S.clampAll(org, o.Range)
	ok(c, h.S.SeasonalityAnalytics(org, o))
}

func (h *Handler) serviceLines(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	o, good := h.opts(c, 12)
	if !good {
		return
	}
	o.Range = h.S.clampAll(org, o.Range)
	ok(c, h.S.ServiceLines(org, o))
}

func (h *Handler) hosting(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	ok(c, h.S.HostingUnits(org))
}

func (h *Handler) getMap(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var n int64
	h.S.DB.Model(&ServiceRule{}).Where("organization_id = ?", org).Count(&n)
	ok(c, gin.H{"rules": h.S.Rules(org), "defaults": n == 0, "labels": LineLabels, "order": LineOrder})
}

func (h *Handler) putMap(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !canEdit(c) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only admins can change the service-line mapping")
		return
	}
	var in struct {
		Rules []ServiceRule `json:"rules"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	rs, err := h.S.SaveRules(org, in.Rules)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	ok(c, gin.H{"rules": rs, "defaults": false, "labels": LineLabels, "order": LineOrder})
}

func (h *Handler) resetMap(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !canEdit(c) {
		fail(c, http.StatusForbidden, "FORBIDDEN", "Only admins can change the service-line mapping")
		return
	}
	ok(c, gin.H{"rules": h.S.ResetRules(org), "defaults": true, "labels": LineLabels, "order": LineOrder})
}

func (h *Handler) market(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	ok(c, h.S.MarketFor(c.Request.Context(), org, c.Query("refresh") == "1"))
}

func (h *Handler) explain(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	_ = c.ShouldBindJSON(&in)
	if !AIEnabled() {
		fail(c, http.StatusServiceUnavailable, "AI_DISABLED", "AI is not configured on the backend (ANTHROPIC_API_KEY).")
		return
	}
	m := h.S.MarketFor(c.Request.Context(), org, false)
	txt, err := h.S.Explain(c.Request.Context(), m, strings.TrimSpace(in.Code))
	if err != nil {
		fail(c, http.StatusBadGateway, "AI_FAILED", err.Error())
		return
	}
	ok(c, gin.H{"text": txt, "note": "Generated by AI from the figures shown — check before acting."})
}

func (h *Handler) addIndicator(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var in struct {
		Code, Country, Label string
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	ci, err := h.S.AddCustom(c.Request.Context(), org, in.Code, in.Country, in.Label)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	ok(c, ci)
}

func (h *Handler) deleteIndicator(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	res := h.S.DB.Where("organization_id = ? AND id = ?", org, c.Param("id")).Delete(&CustomIndicator{})
	if res.RowsAffected == 0 {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Indicator not found")
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) datasets(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	out := []Dataset{}
	h.S.DB.Where("organization_id = ?", org).Order("name").Find(&out)
	ok(c, gin.H{"datasets": out})
}

// uploadDataset accepts multipart (file + fields) or JSON {name, unit, source, source_url, country, notes, csv}.
func (h *Handler) uploadDataset(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var in struct {
		Name, Unit, Source, Country, Notes, CSV string
		SourceURL                               string `json:"source_url" form:"source_url"`
	}
	var body string
	if strings.HasPrefix(c.ContentType(), "multipart/") {
		in.Name, in.Unit, in.Source, in.Country, in.Notes, in.SourceURL = c.PostForm("name"), c.PostForm("unit"), c.PostForm("source"), c.PostForm("country"), c.PostForm("notes"), c.PostForm("source_url")
		fh, err := c.FormFile("file")
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", "Choose a CSV file")
			return
		}
		if fh.Size > 2<<20 {
			fail(c, http.StatusBadRequest, "VALIDATION", "The file is too large (max 2 MB)")
			return
		}
		f, err := fh.Open()
		if err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", "Could not read the file")
			return
		}
		defer f.Close()
		buf := new(strings.Builder)
		if _, err := ioCopy(buf, f); err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", "Could not read the file")
			return
		}
		body = buf.String()
	} else {
		if err := c.ShouldBindJSON(&in); err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
			return
		}
		body = in.CSV
	}
	in.Name, in.Source = strings.TrimSpace(in.Name), strings.TrimSpace(in.Source)
	if in.Name == "" {
		fail(c, http.StatusBadRequest, "VALIDATION", "Give the series a name")
		return
	}
	if in.Source == "" {
		fail(c, http.StatusBadRequest, "VALIDATION", "Say where the data comes from (source)")
		return
	}
	if len(in.Name) > 160 || len(in.Source) > 255 || len(in.SourceURL) > 500 || len(in.Unit) > 60 {
		fail(c, http.StatusBadRequest, "VALIDATION", "A field is too long")
		return
	}
	if u := strings.TrimSpace(in.SourceURL); u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		fail(c, http.StatusBadRequest, "VALIDATION", "The source link must start with http:// or https://")
		return
	}
	pts, warn, err := ParseSeriesCSV(strings.NewReader(body))
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	country := strings.ToUpper(strings.TrimSpace(in.Country))
	if country != "" && !ValidCountry(country) {
		fail(c, http.StatusBadRequest, "VALIDATION", "Country must be a 2–3 letter code such as KEN")
		return
	}
	now := h.S.now()
	d := &Dataset{ID: newID(), OrganizationID: org, Name: in.Name, Unit: strings.TrimSpace(in.Unit), Country: country, Source: in.Source, SourceURL: strings.TrimSpace(in.SourceURL),
		Notes: strings.TrimSpace(in.Notes), Points: pts, CreatedBy: c.GetString("user_id"), CreatedAt: now, UpdatedAt: now}
	if err := h.S.DB.Create(d).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DB", "Could not save the dataset")
		return
	}
	ok(c, gin.H{"dataset": d, "warnings": warn})
}

func (h *Handler) deleteDataset(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	res := h.S.DB.Where("organization_id = ? AND id = ?", org, c.Param("id")).Delete(&Dataset{})
	if res.RowsAffected == 0 {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Dataset not found")
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) exportList(c *gin.Context) {
	if _, okk := orgOf(c); !okk {
		return
	}
	ok(c, gin.H{"datasets": Datasets, "note": "CSV, UTF-8, comma separated, one header row. Amounts *_home are in the home currency; see the conversion rules on the Analytics page."})
}

func (h *Handler) export(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	key := strings.TrimSuffix(c.Param("dataset"), ".csv")
	if DatasetByKey(key) == nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Unknown dataset")
		return
	}
	r, err := ParseRange(reqtz.Now(c), c.Query("from"), c.Query("to"), 12)
	if err != nil {
		fail(c, http.StatusBadRequest, "INVALID_RANGE", err.Error())
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s_%s_%s.csv"`, key, r.From.Format(dayFmt), r.To.Format(dayFmt)))
	c.Status(http.StatusOK)
	if _, err := h.S.WriteDataset(c.Writer, org, key, r); err != nil {
		log.Printf("insights export %s: %v", key, err)
	}
}
