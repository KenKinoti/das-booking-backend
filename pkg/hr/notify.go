package hr

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// notifyLeave: submitted → managers; approved/rejected → the requester and
// the employee's own user account.
func (h *Handler) notifyLeave(c *gin.Context, l *LeaveRequest, event string) {
	actor := ctxString(c, "user_id")
	who := l.EmployeeName
	if who == "" {
		who = "An employee"
	}
	days := strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", l.Days), "0"), ".")
	unit := "days"
	if l.Days == 1 {
		unit = "day"
	}
	span := fmt.Sprintf("%s leave · %s → %s (%s %s)", strings.ReplaceAll(l.Type, "_", " "), l.StartDate, l.EndDate, days, unit)
	m := notify.Message{Link: "/hcm?tab=leave", EntityType: "leave_request", EntityID: l.ID, ActorID: actor, ActorName: notify.UserName(h.DB, actor)}
	var to notify.Target
	switch event {
	case "submitted":
		m.Type, m.Severity = "leave.submitted", notify.Info
		m.Title = who + " requested leave"
		m.Body = strings.ToUpper(span[:1]) + span[1:]
		if l.Reason != "" {
			m.Body += " — " + l.Reason
		}
		to = notify.Roles(managerRoles...).Except(actor)
	case "approved", "rejected":
		m.Type = "leave." + event
		m.Severity = notify.Success
		if event == "rejected" {
			m.Severity = notify.Error
		}
		m.Title = fmt.Sprintf("Leave %s for %s", event, who)
		m.Body = strings.ToUpper(span[:1]) + span[1:]
		if m.ActorName != "" {
			m.Body += " · by " + m.ActorName
		}
		ids := []string{l.RequestedBy}
		var uid *string
		h.DB.Model(&Employee{}).Where("id = ? AND org_id = ?", l.EmployeeID, l.OrgID).Select("user_id").Scan(&uid)
		if uid != nil {
			ids = append(ids, *uid)
		}
		to = notify.Users(ids...).Except(actor)
	default:
		return
	}
	notify.Go(h.DB, l.OrgID, to, m)
}

var managerRoles = []string{"super_admin", "admin", "owner", "org_admin", "manager", "hr", "hr_manager"}
