package plans

import (
	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// notifyPlan tells the organisation's admins that the plan changed.
func (h *Handler) notifyPlan(c *gin.Context, org, tierName string, tierChanged, byPlatform bool) {
	actor := ctxString(c, "user_id")
	name := notify.UserName(h.Svc.DB, actor)
	m := notify.Message{Type: "plan.updated", Severity: notify.Info, Link: "/plan", EntityType: "plan", EntityID: org, ActorID: actor, ActorName: name}
	if tierChanged {
		m.Type, m.Severity = "plan.changed", notify.Success
		m.Title = "Plan changed to " + tierName
	} else {
		m.Title = tierName + " plan updated"
	}
	switch {
	case byPlatform:
		m.Body = "Updated by the DASYIN platform team. Modules and limits may have changed."
	case name != "":
		m.Body = "Changed by " + name + ". Modules and billing now follow the new plan."
	default:
		m.Body = "Modules and billing now follow the new plan."
	}
	notify.Go(h.Svc.DB, org, notify.Admins().Except(actor), m)
}
