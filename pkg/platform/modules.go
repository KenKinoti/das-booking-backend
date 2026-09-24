package platform

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/plans"
)

// ModuleMatrix reports, for every organisation, which modules its plan
// actually unlocks (the same entitlement rules the API enforces). Plans are
// edited on the Plans & pricing page; this view is read-only.
func (h *Handler) ModuleMatrix(c *gin.Context) {
	svc := plans.NewService(h.DB, nil)
	cat, err := svc.Catalog()
	if err != nil {
		fail(c, http.StatusServiceUnavailable, "PLANS_UNAVAILABLE", "The plan catalogue is not available yet")
		return
	}
	rows := []OrgRow{}
	if err := h.orgQuery().Select(h.orgSelect()).Order("LOWER(o.name)").Limit(1000).Scan(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load organisations")
		return
	}
	mods := make([]gin.H, 0, len(cat.Modules))
	for _, m := range cat.Modules {
		mods = append(mods, gin.H{"key": m.Key, "name": m.Name, "description": m.Description, "icon": m.Icon, "core": m.Core, "active": m.Active})
	}
	orgs := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		row := gin.H{"id": r.ID, "name": r.Name, "status": r.Status, "users": r.Users, "business_type": r.BusinessType}
		sub, err := svc.Subscription(r.ID)
		if err != nil {
			row["error"] = "Plan could not be loaded"
			orgs = append(orgs, row)
			continue
		}
		a := plans.AccessOf(cat, sub)
		keys := make([]string, 0, len(a.Modules))
		for k, on := range a.Modules {
			if on {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		row["modules"] = keys
		row["plan"] = gin.H{"tier": sub.Tier, "industry": sub.Industry, "status": sub.Status, "cycle": sub.Cycle, "max_users": a.MaxUsers}
		orgs = append(orgs, row)
	}
	tiers := make([]gin.H, 0, len(cat.Tiers))
	for _, t := range cat.Tiers {
		tiers = append(tiers, gin.H{"key": t.Key, "name": t.Name})
	}
	ok(c, http.StatusOK, gin.H{"modules": mods, "organizations": orgs, "tiers": tiers})
}
