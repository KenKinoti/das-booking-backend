package pricebook

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
)

// Handler serves /finance/supplier-prices/... (the Accounting module:
// /finance is covered by the plan and role checks). Supplier costs are for
// admins and managers only.
type Handler struct {
	S *Service
	// Pricing fetches Synergy's domain price list (nil = the Synergy API).
	Pricing SynergyPricing
}

// RegisterRoutes mounts the routes on the authenticated API group.
//
//	GET    /finance/supplier-prices                    suppliers, every price, KPIs and the page context
//	GET    /finance/supplier-prices/context            home currency, tax, payment fees, tiers, CSV template docs
//	PUT    /finance/supplier-prices/tiers              hosting tiers {tiers: [...]}
//	POST   /finance/supplier-prices/suppliers          create;  PUT/DELETE …/suppliers/:id
//	GET    /finance/supplier-prices/suppliers/:id/model  self-hosted cost model (?tier=&min_disk_gb=)
//	GET    /finance/supplier-prices/items              ?supplier_id=&kind=&type=&q=&stale=1&active=1|0
//	POST   /finance/supplier-prices/items              create;  PUT/DELETE …/items/:id;  GET …/items/:id/history
//	POST   /finance/supplier-prices/import             CSV (multipart "file" or JSON {csv}); ?dry_run=1 previews
//	GET    /finance/supplier-prices/export.csv         the price list in the template's format (?all=1 with inactive)
//	GET    /finance/supplier-prices/template.csv       the documented template
//	POST   /finance/supplier-prices/synergy/autofill   plans from the statements, domains from the Synergy API
//	POST   /finance/supplier-prices/compare            the combo comparison
//	POST   /finance/supplier-prices/retail             retail price of one option
//	POST   /finance/supplier-prices/services           save retail prices as catalog services
//	GET    /finance/supplier-prices/scenarios          saved comparisons;  POST;  PUT/DELETE …/scenarios/:id
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/finance/supplier-prices")
	g.Use(h.guard)
	g.GET("", h.overview)
	g.GET("/context", h.context)
	g.PUT("/tiers", h.saveTiers)
	g.GET("/suppliers", h.suppliers)
	g.POST("/suppliers", h.createSupplier)
	g.PUT("/suppliers/:id", h.updateSupplier)
	g.DELETE("/suppliers/:id", h.deleteSupplier)
	g.GET("/suppliers/:id/model", h.model)
	g.GET("/items", h.items)
	g.POST("/items", h.createItem)
	g.PUT("/items/:id", h.updateItem)
	g.DELETE("/items/:id", h.deleteItem)
	g.GET("/items/:id/history", h.history)
	g.POST("/import", h.importCSV)
	g.GET("/export.csv", h.exportCSV)
	g.GET("/template.csv", h.template)
	g.POST("/synergy/autofill", h.autofill)
	g.POST("/compare", h.compare)
	g.POST("/retail", h.retail)
	g.POST("/services", h.saveServices)
	g.GET("/scenarios", h.scenarios)
	g.POST("/scenarios", h.createScenario)
	g.PUT("/scenarios/:id", h.updateScenario)
	g.DELETE("/scenarios/:id", h.deleteScenario)
}

func ok(c *gin.Context, v any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": v}) }

func fail(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"success": false, "error": msg, "message": msg})
}

func failErr(c *gin.Context, err error) {
	var ve *ValidationError
	var pe *ParseError
	switch {
	case errors.As(err, &ve):
		fail(c, http.StatusBadRequest, ve.Msg)
	case errors.As(err, &pe):
		fail(c, http.StatusBadRequest, pe.Msg)
	case errors.Is(err, ErrNotFound):
		fail(c, http.StatusNotFound, "Not found")
	default:
		log.Printf("pricebook: %v", err)
		fail(c, http.StatusInternalServerError, "Something went wrong — try again")
	}
}

// CanUse reports whether a role may see supplier costs.
func CanUse(role string) bool {
	switch strings.ToLower(role) {
	case "super_admin", "admin", "owner", "org_admin", "manager":
		return true
	}
	return false
}

func orgOf(c *gin.Context) string {
	for _, k := range []string{"organization_id", "org_id"} {
		if v := strings.TrimSpace(c.GetString(k)); v != "" {
			return v
		}
	}
	return ""
}

// guard lets admins and managers of an organisation through.
func (h *Handler) guard(c *gin.Context) {
	if orgOf(c) == "" {
		fail(c, http.StatusUnauthorized, "No organisation is associated with this account")
		c.Abort()
		return
	}
	if !CanUse(c.GetString("user_role")) {
		fail(c, http.StatusForbidden, "Supplier prices are for admins and managers")
		c.Abort()
		return
	}
	c.Next()
}

func (h *Handler) overview(c *gin.Context) { ok(c, h.S.OverviewFor(orgOf(c), reqtz.Today(c))) }

func (h *Handler) context(c *gin.Context) {
	h.S.EnsureSeed(orgOf(c))
	ok(c, h.S.ContextFor(orgOf(c)))
}

func (h *Handler) saveTiers(c *gin.Context) {
	var in struct {
		Tiers []Tier `json:"tiers"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid tiers")
		return
	}
	t, err := h.S.SaveTiers(orgOf(c), in.Tiers)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, gin.H{"tiers": t})
}

func (h *Handler) suppliers(c *gin.Context) {
	h.S.EnsureSeed(orgOf(c))
	ok(c, gin.H{"suppliers": h.S.OverviewFor(orgOf(c), reqtz.Today(c)).Suppliers})
}

func (h *Handler) createSupplier(c *gin.Context) { h.saveSupplier(c, "") }

func (h *Handler) updateSupplier(c *gin.Context) { h.saveSupplier(c, c.Param("id")) }

func (h *Handler) saveSupplier(c *gin.Context, id string) {
	var in SupplierInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid supplier: check the numbers")
		return
	}
	sp, err := h.S.SaveSupplier(orgOf(c), id, in)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, gin.H{"supplier": sp})
}

func (h *Handler) deleteSupplier(c *gin.Context) {
	if err := h.S.DeleteSupplier(orgOf(c), c.Param("id")); err != nil {
		failErr(c, err)
		return
	}
	ok(c, gin.H{"deleted": true})
}

func queryNeed(c *gin.Context) CompareInput {
	in := CompareInput{Tier: c.Query("tier")}
	if v, blank, good := ParseAmount(c.Query("min_disk_gb")); good && !blank {
		in.MinDiskGB = v
	}
	if v, good := ParseCount(c.Query("min_sites")); good && v != nil {
		in.MinSites = *v
	}
	return in
}

func (h *Handler) model(c *gin.Context) {
	m, err := h.S.SelfHosted(orgOf(c), c.Param("id"), queryNeed(c), reqtz.Today(c))
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, m)
}

func (h *Handler) items(c *gin.Context) {
	h.S.EnsureSeed(orgOf(c))
	f := ItemFilter{SupplierID: c.Query("supplier_id"), Kind: c.Query("kind"), Type: c.Query("type"), Q: c.Query("q"), Stale: c.Query("stale") == "1" || c.Query("stale") == "true", Active: c.Query("active")}
	list := h.S.Items(orgOf(c), f, reqtz.Today(c))
	ok(c, gin.H{"items": list, "count": len(list)})
}

func (h *Handler) createItem(c *gin.Context) { h.saveItem(c, "") }

func (h *Handler) updateItem(c *gin.Context) { h.saveItem(c, c.Param("id")) }

func (h *Handler) saveItem(c *gin.Context, id string) {
	var in ItemInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid price: check the numbers")
		return
	}
	it, err := h.S.SaveItem(orgOf(c), c.GetString("user_id"), id, in, "manual", reqtz.Today(c))
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, gin.H{"item": it})
}

func (h *Handler) deleteItem(c *gin.Context) {
	if err := h.S.DeleteItem(orgOf(c), c.Param("id")); err != nil {
		failErr(c, err)
		return
	}
	ok(c, gin.H{"deleted": true})
}

func (h *Handler) history(c *gin.Context) {
	it, rows, err := h.S.History(orgOf(c), c.Param("id"), reqtz.Today(c))
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, gin.H{"item": it, "history": rows})
}

const maxCSV = 2 << 20

func (h *Handler) importCSV(c *gin.Context) {
	var data []byte
	dry := c.Query("dry_run") == "1" || c.Query("dry_run") == "true"
	if strings.HasPrefix(c.ContentType(), "multipart/") {
		fh, err := c.FormFile("file")
		if err != nil {
			fail(c, http.StatusBadRequest, "Choose a CSV file")
			return
		}
		if fh.Size > maxCSV {
			fail(c, http.StatusBadRequest, "The file is larger than 2 MB")
			return
		}
		f, err := fh.Open()
		if err != nil {
			fail(c, http.StatusBadRequest, "Could not read the file")
			return
		}
		defer f.Close()
		if data, err = io.ReadAll(io.LimitReader(f, maxCSV+1)); err != nil {
			fail(c, http.StatusBadRequest, "Could not read the file")
			return
		}
		if v := c.PostForm("dry_run"); v == "1" || v == "true" {
			dry = true
		}
	} else {
		var in struct {
			CSV    string `json:"csv"`
			DryRun bool   `json:"dry_run"`
		}
		if err := c.ShouldBindJSON(&in); err != nil {
			fail(c, http.StatusBadRequest, "Send the CSV as a file or as {\"csv\": \"…\"}")
			return
		}
		data, dry = []byte(in.CSV), dry || in.DryRun
	}
	if len(data) > maxCSV {
		fail(c, http.StatusBadRequest, "The file is larger than 2 MB")
		return
	}
	res, err := h.S.Import(orgOf(c), c.GetString("user_id"), data, dry, reqtz.Today(c))
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, res)
}

func (h *Handler) exportCSV(c *gin.Context) {
	f := ItemFilter{SupplierID: c.Query("supplier_id"), Active: "1"}
	if c.Query("all") == "1" {
		f.Active = ""
	}
	data := h.S.Export(orgOf(c), f, reqtz.Today(c))
	c.Header("Content-Disposition", `attachment; filename="supplier-prices-`+reqtz.DateString(c)+`.csv"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", data)
}

func (h *Handler) template(c *gin.Context) {
	c.Header("Content-Disposition", `attachment; filename="supplier-prices-template.csv"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", TemplateCSV())
}

func (h *Handler) autofill(c *gin.Context) {
	var in AutofillOptions
	_ = c.ShouldBindJSON(&in)
	res, err := h.S.AutofillSynergy(c.Request.Context(), orgOf(c), c.GetString("user_id"), in, h.Pricing, reqtz.Today(c))
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, res)
}

func (h *Handler) compare(c *gin.Context) {
	var in CompareInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid comparison: check the numbers")
		return
	}
	ok(c, h.S.Compare(orgOf(c), in, reqtz.Today(c)))
}

func (h *Handler) retail(c *gin.Context) {
	var in RetailRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid pricing request: check the numbers")
		return
	}
	res, _, err := h.S.Retail(orgOf(c), in, reqtz.Today(c))
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, res)
}

func (h *Handler) saveServices(c *gin.Context) {
	var in struct {
		Services []ServiceInput `json:"services"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid services")
		return
	}
	res, err := h.S.SaveServices(orgOf(c), in.Services)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, gin.H{"services": res})
}

func (h *Handler) scenarios(c *gin.Context) {
	ok(c, gin.H{"scenarios": h.S.Scenarios(orgOf(c))})
}

func (h *Handler) createScenario(c *gin.Context) { h.saveScenario(c, "") }

func (h *Handler) updateScenario(c *gin.Context) { h.saveScenario(c, c.Param("id")) }

func (h *Handler) saveScenario(c *gin.Context, id string) {
	var in ScenarioInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid comparison")
		return
	}
	sc, err := h.S.SaveScenario(orgOf(c), c.GetString("user_id"), id, in)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, gin.H{"scenario": sc})
}

func (h *Handler) deleteScenario(c *gin.Context) {
	if err := h.S.DeleteScenario(orgOf(c), c.Param("id")); err != nil {
		failErr(c, err)
		return
	}
	ok(c, gin.H{"deleted": true})
}
