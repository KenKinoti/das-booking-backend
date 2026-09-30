package meetings

import (
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// notifyMeeting tells invited team members (participants linked to a user)
// that a meeting was scheduled or cancelled. The organiser is skipped.
func (h *Handler) notifyMeeting(m *Meeting, event string) {
	if m == nil {
		return
	}
	var ids []string
	h.DB.Model(&Participant{}).Where("meeting_id = ? AND user_id <> ''", m.ID).Pluck("user_id", &ids)
	if len(ids) == 0 {
		return
	}
	when := m.StartAt
	if loc, err := time.LoadLocation(m.Timezone); err == nil {
		when = when.In(loc)
	}
	body := when.Format("Mon 2 Jan, 15:04 MST")
	if m.OrganizerName != "" {
		body += " · organised by " + m.OrganizerName
	}
	msg := notify.Message{Link: "/meetings/" + m.ID, EntityType: "meeting", EntityID: m.ID, ActorID: m.OrganizerID, ActorName: m.OrganizerName, Body: body}
	switch event {
	case "scheduled":
		msg.Type, msg.Severity, msg.Title = "meeting.scheduled", notify.Info, "Invited: "+m.Title
	case "cancelled":
		msg.Type, msg.Severity, msg.Title = "meeting.cancelled", notify.Warning, "Cancelled: "+m.Title
	default:
		return
	}
	notify.Go(h.DB, m.OrgID, notify.Users(ids...).Except(m.OrganizerID), msg)
}
