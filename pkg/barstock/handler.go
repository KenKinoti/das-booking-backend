package barstock

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Handler serves /api/v1/bar/**.
type Handler struct{ S *Service }

// RegisterRoutes mounts the bar stock API on the authenticated group. The
// plans rule for /bar (module inventory, pos read-also) and the business
// type / role guard run before these handlers; the handlers add the finer
// checks (approve for money and settings, location assignments).
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/bar")
	g.GET("/settings", h.getSettings)
	g.PUT("/settings", h.putSettings)
	g.GET("/setup/catalog", h.catalog)
	g.POST("/setup", h.setup)

	g.GET("/locations", h.listLocations)
	g.POST("/locations", h.createLocation)
	g.PUT("/locations/:id", h.updateLocation)
	g.DELETE("/locations/:id", h.deleteLocation)
	g.GET("/terminals", h.getTerminals)
	g.PUT("/terminals", h.putTerminals)

	g.GET("/items", h.listItems)
	g.POST("/items", h.createItem)
	g.GET("/items/:product_id", h.getItem)
	g.PUT("/items/:product_id", h.updateItem)
	g.DELETE("/items/:product_id", h.deleteItem)
	g.POST("/items/:product_id/menu-items", h.makeMenuItems)
	g.PUT("/par", h.putPar)

	g.GET("/recipes", h.listRecipes)
	g.POST("/recipes", h.createRecipe)
	g.GET("/recipes/:pos_product_id", h.getRecipe)
	g.PUT("/recipes/:pos_product_id", h.updateRecipe)
	g.DELETE("/recipes/:pos_product_id", h.deleteRecipe)
	g.GET("/pos-availability", h.posAvailability)

	g.GET("/stock", h.listStock)
	g.GET("/movements", h.listMovements)

	g.GET("/transfers", h.listTransfers)
	g.GET("/transfers/suggest", h.suggestTransfer)
	g.POST("/transfers", h.createTransfer)
	g.GET("/transfers/:id", h.getTransfer)
	g.POST("/transfers/:id/issue", h.issueTransfer)
	g.POST("/transfers/:id/receive", h.receiveTransfer)
	g.POST("/transfers/:id/cancel", h.cancelTransfer)

	g.GET("/wastage", h.listWastage)
	g.POST("/wastage", h.createWastage)
	g.POST("/wastage/:id/approve", h.approveWastage)
	g.POST("/wastage/:id/reject", h.rejectWastage)

	g.GET("/counts", h.listCounts)
	g.POST("/counts", h.createCount)
	g.GET("/counts/:id", h.getCount)
	g.PUT("/counts/:id/lines", h.saveCountLines)
	g.POST("/counts/:id/submit", h.submitCount)
	g.POST("/counts/:id/approve", h.approveCount)
	g.POST("/counts/:id/recount", h.recountCount)
	g.DELETE("/counts/:id", h.deleteCount)

	g.GET("/reorder", h.reorder)
	g.POST("/reorder/create-pos", h.createPOs)
	g.GET("/purchase-orders", h.openPOs)
	g.GET("/deliveries", h.listDeliveries)
	g.POST("/deliveries", h.createDelivery)

	g.GET("/overview", h.overview)
	g.GET("/variance", h.variance)
	g.GET("/reports/pour-cost", h.pourCostReport)
	g.GET("/reports/valuation", h.valuationReport)
	g.GET("/reports/slow-movers", h.slowMovers)
	g.GET("/reports/night", h.nightPreview)
	g.POST("/reports/night/send", h.nightSend)
	g.GET("/reports/export/:file", h.export)
}

// ---------------------------------------------------------------- access

// access is what the caller may do (module inventory).
type access struct {
	Org, User, Role                     string
	View, Create, Edit, Delete, Approve bool
	// Locs limits view/create to assigned locations (nil = every location).
	Locs map[string]bool
}

func legacyCan(role, act string) bool {
	switch strings.ToLower(role) {
	case "super_admin", "admin", "owner", "manager":
		return true
	case "staff", "employee", "care_worker", "support_coordinator", "":
		return act == "view" || act == "create"
	}
	return act == "view"
}

func (h *Handler) access(c *gin.Context) access {
	a := access{Org: c.GetString("org_id"), User: c.GetString("user_id"), Role: c.GetString("user_role")}
	can := func(act string) bool {
		if h.S.Can != nil {
			return h.S.Can(a.Org, a.User, a.Role, "inventory", act)
		}
		return legacyCan(a.Role, act)
	}
	a.View, a.Create, a.Edit, a.Delete, a.Approve = can("view"), can("create"), can("edit"), can("delete"), can("approve")
	if !a.Approve && a.User != "" {
		var locs []string
		h.S.DB.Table("bar_location_staff").Where("organization_id = ? AND user_id = ?", a.Org, a.User).Pluck("location_id", &locs)
		if len(locs) > 0 {
			a.Locs = map[string]bool{}
			for _, l := range locs {
				a.Locs[l] = true
			}
		}
	}
	return a
}

// canLoc reports whether a limited user may use location id.
func (a access) canLoc(id string) bool { return a.Locs == nil || a.Locs[id] }

func (a access) need(ok bool, what string) error {
	if !ok {
		return forbidden("You don't have permission to %s", what)
	}
	return nil
}

func (a access) needLoc(id string) error {
	if !a.canLoc(id) {
		return forbidden("You are not assigned to that location")
	}
	return nil
}

// ---------------------------------------------------------------- responses

// moneyKeys are removed from responses for users without approve.
var moneyKeys = map[string]bool{
	"avg_cost": true, "value": true, "last_cost": true, "best_cost": true, "best_supplier": true, "best_supplier_id": true,
	"unit_cost": true, "cost": true, "total_cost": true, "pour_cost_pct": true, "margin": true, "margin_pct": true,
	"variance_value": true, "cost_of_sales": true, "stock_value": true, "stock_value_by_location": true, "price_history": true,
	"loss_value": true, "net_variance_value": true, "wastage_value": true, "comp_value": true, "line_total": true,
	"suggested_tot_price": true, "suggested_bottle_price": true, "suggested_price": true, "cost_per_serving": true,
	"pour_cost_target": true, "gross_profit": true, "cost_per_unit": true, "loss_value_share": true, "lowest_cost": true,
	"avg_cost_per_unit": true, "expected_value": true, "counted_value": true, "variance_amount": true, "value_total": true,
	"discrepancy_value": true,
}

func strip(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if moneyKeys[k] {
				delete(t, k)
				continue
			}
			t[k] = strip(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = strip(t[i])
		}
		return t
	}
	return v
}

// ok writes {success:true, data}. Money fields are stripped unless the
// caller has approve on inventory.
func (h *Handler) ok(c *gin.Context, status int, a access, data any) {
	if a.Approve {
		c.JSON(status, gin.H{"success": true, "data": data})
		return
	}
	raw, err := json.Marshal(data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "encode response"})
		return
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "encode response"})
		return
	}
	c.JSON(status, gin.H{"success": true, "data": strip(generic)})
}

func (h *Handler) fail(c *gin.Context, err error) {
	st, msg := statusOf(err)
	c.JSON(st, gin.H{"success": false, "error": msg, "message": msg})
}

var jsonUnmarshal = json.Unmarshal

func bind(c *gin.Context, v any) error {
	if err := c.ShouldBindJSON(v); err != nil {
		return badReq("Invalid request: %s", err.Error())
	}
	return nil
}

func qInt(c *gin.Context, k string, def int) int {
	if v, err := strconv.Atoi(c.Query(k)); err == nil {
		return v
	}
	return def
}

func qBool(c *gin.Context, k string) bool {
	v := strings.ToLower(strings.TrimSpace(c.Query(k)))
	return v == "1" || v == "true" || v == "yes"
}
