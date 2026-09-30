package assistant

import (
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// notifyWrite tells the organisation's other admins that Ask DASYIN changed
// data for a user (the user sees it in the chat already).
func (s *Service) notifyWrite(who aitools.Identity, t aitools.Tool, meta ToolMeta) {
	if meta.Status == "error" || s.DB == nil {
		return
	}
	name := notify.UserName(s.DB, who.UserID)
	if name == "" {
		name = "A team member"
	}
	body := meta.Summary
	if body == "" {
		body = toolTitle(t, t.Name)
	}
	sev := notify.Info
	if t.Destructive {
		sev = notify.Warning
	}
	link := meta.Link
	if link == "" {
		link = "/assistant"
	}
	notify.Go(s.DB, who.OrgID, notify.Admins().Except(who.UserID), notify.Message{
		Type: "ai.assistant_write", Severity: sev, Title: "Ask DASYIN · " + toolTitle(t, t.Name),
		Body: body + " (for " + name + ")", Link: link, EntityType: meta.Changed, EntityID: "", ActorID: who.UserID, ActorName: name,
	})
}
