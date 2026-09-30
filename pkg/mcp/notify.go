package mcp

import (
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// notifyWrite tells the connection's owner that an external AI client changed
// data on their behalf (they are not watching the app when this happens).
func notifyWrite(db *gorm.DB, p Principal, t aitools.Tool) {
	client := p.ClientName
	if client == "" {
		client = "An AI client"
	}
	title := t.Title
	if title == "" {
		title = t.Name
	}
	sev := notify.Info
	if t.Destructive {
		sev = notify.Warning
	}
	notify.Go(db, p.OrgID, notify.Users(p.UserID), notify.Message{
		Type: "ai.mcp_write", Severity: sev, Title: client + " made a change",
		Body: title + " · via MCP", Link: "/settings/ai", EntityType: "mcp_tool", EntityID: t.Name,
	})
}
