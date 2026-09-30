package events

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// notifyRegistration tells admins about a registration from the public page.
func notifyRegistration(db *gorm.DB, ev *models.Event, reg *models.EventRegistration) {
	if ev == nil || reg == nil {
		return
	}
	m := notify.Message{Link: "/events/" + ev.ID + "/registrations", EntityType: "event_registration", EntityID: reg.ID}
	tickets := "1 ticket"
	if reg.Quantity != 1 {
		tickets = fmt.Sprintf("%d tickets", reg.Quantity)
	}
	m.Body = fmt.Sprintf("%s · %s · %s", reg.RegistrantName, tickets, ev.Title)
	switch reg.Status {
	case "waitlisted":
		m.Type, m.Severity, m.Title = "event.waitlisted", notify.Warning, "Waitlist sign-up: "+ev.Title
	case "pending":
		m.Type, m.Severity, m.Title = "event.registration_pending", notify.Info, "Registration awaiting approval"
	default:
		m.Type, m.Severity, m.Title = "event.registered", notify.Success, "New registration: "+ev.Title
	}
	notify.Go(db, ev.OrganizationID, notify.Admins(), m)
}
