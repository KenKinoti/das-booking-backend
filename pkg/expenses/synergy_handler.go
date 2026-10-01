package expenses

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
)

// Synergy routes (under /expenses):
//
//	GET    /synergy                          analytics (?basis=charge|service)
//	GET    /synergy/sites/:domain            one site: charges, plan changes, usage, invoices
//	PUT    /synergy/sites/:domain            client link / monthly price / notes
//	PUT    /synergy/plans                    plan price / disk limit override
//	GET    /synergy/sites.csv                per-site table export
//	GET    /synergy/statement-expenses       preview expenses from statements (?basis=&months=2026-08,2026-09)
//	POST   /synergy/statement-expenses       create / update them
//
// Files are uploaded with POST /expenses/uploads (statements and usage reports
// are detected by their header; "dates" = {"file name": "YYYY-MM-DD"} sets the
// snapshot date of a usage report).
func (h *Handler) registerSynergy(g *gin.RouterGroup) {
	g.GET("/synergy", h.synergy)
	g.GET("/synergy/sites.csv", h.synergyCSV)
	g.GET("/synergy/sites/:domain", h.synergySite)
	g.PUT("/synergy/sites/:domain", h.saveSynergySite)
	g.PUT("/synergy/plans", h.saveSynergyPlan)
	g.GET("/synergy/statement-expenses", h.statementPreview)
	g.POST("/synergy/statement-expenses", h.statementCreate)
}

func (h *Handler) synergy(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	ok(c, h.S.SynergyReport(o, reqtz.Now(c), c.Query("basis")))
}

func (h *Handler) synergyCSV(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	c.Header("Content-Disposition", `attachment; filename="synergy-sites-`+time.Now().Format("2006-01-02")+`.csv"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", h.S.SynergySitesCSV(o, reqtz.Now(c)))
}

func (h *Handler) synergySite(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	d, err := h.S.SynergySiteDetail(o, c.Param("domain"), reqtz.Now(c))
	if err != nil {
		if err == ErrNotFound {
			fail(c, http.StatusNotFound, "NOT_FOUND", "Site not found in the Synergy statements or usage reports")
			return
		}
		errOut(c, err)
		return
	}
	ok(c, d)
}

func (h *Handler) saveSynergySite(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in SynergySiteInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	st, err := h.S.SaveSynergySite(o, c.Param("domain"), in)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, st)
}

func (h *Handler) saveSynergyPlan(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in SynergyPlanInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	p, err := h.S.SaveSynergyPlan(o, in)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, p)
}

func splitMonths(s string) []string {
	out := []string{}
	for _, m := range strings.Split(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func (h *Handler) statementPreview(c *gin.Context) {
	o, okk := h.org(c)
	if !okk {
		return
	}
	p, err := h.S.StatementExpenses(o, StatementExpenseInput{Basis: c.Query("basis"), Months: splitMonths(c.Query("months"))})
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, p)
}

func (h *Handler) statementCreate(c *gin.Context) {
	o, okk := h.admin(c)
	if !okk {
		return
	}
	var in StatementExpenseInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION", "Invalid request")
		return
	}
	p, err := h.S.CreateStatementExpenses(o, c.GetString("user_id"), in)
	if err != nil {
		errOut(c, err)
		return
	}
	ok(c, p)
}
