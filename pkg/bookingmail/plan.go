package bookingmail

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Events.
const (
	EvAssigned    = "assigned"    // new booking, or the booking was given to this person
	EvRescheduled = "rescheduled" // time changed, same person
	EvUnassigned  = "unassigned"  // the booking moved to someone else (or nobody)
	EvCancelled   = "cancelled"   // cancelled or deleted
	EvNoShow      = "no_show"     // customer did not show up
)

// Plan is one email to send.
type Plan struct {
	UserID string
	Event  string
	Method string // REQUEST | CANCEL | "" (no calendar data)
}

func staffOf(b *models.Booking) string {
	if b == nil || b.StaffID == nil {
		return ""
	}
	return strings.TrimSpace(*b.StaffID)
}

// live: the booking is still going to happen.
func live(status string) bool {
	switch status {
	case "cancelled", "no_show", "completed":
		return false
	}
	return true
}

// PlanEmails decides who is emailed about a change. before is nil for a new
// booking, after is nil for a deleted one.
func PlanEmails(before, after *models.Booking) []Plan {
	out := []Plan{}
	add := func(u, ev, m string) {
		if u == "" {
			return
		}
		for _, p := range out {
			if p.UserID == u {
				return
			}
		}
		out = append(out, Plan{UserID: u, Event: ev, Method: m})
	}
	switch {
	case before == nil && after == nil:
		return out
	case before == nil:
		if live(after.Status) {
			add(staffOf(after), EvAssigned, "REQUEST")
		}
		return out
	case after == nil:
		if live(before.Status) {
			add(staffOf(before), EvCancelled, "CANCEL")
		}
		return out
	}
	oldS, newS := staffOf(before), staffOf(after)
	switch {
	case after.Status == "cancelled" && before.Status != "cancelled":
		if live(before.Status) {
			add(newS, EvCancelled, "CANCEL")
			add(oldS, EvCancelled, "CANCEL")
		}
	case after.Status == "no_show" && before.Status != "no_show":
		add(newS, EvNoShow, "")
	case !live(after.Status):
		// completed (or still cancelled / no-show): nothing to tell.
	case !live(before.Status):
		// Re-opened (e.g. cancelled → scheduled): back in the calendar.
		add(newS, EvAssigned, "REQUEST")
	case oldS != newS:
		add(oldS, EvUnassigned, "CANCEL")
		add(newS, EvAssigned, "REQUEST")
	case !before.StartTime.Equal(after.StartTime) || !before.EndTime.Equal(after.EndTime):
		add(newS, EvRescheduled, "REQUEST")
	}
	return out
}

// ---------------------------------------------------------------- details

// ServiceNames lists a booking's services.
func ServiceNames(b *models.Booking) []string {
	out := []string{}
	for _, s := range b.Services {
		if n := strings.TrimSpace(s.Name); n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// CustomerName is "First Last" (or the company name).
func CustomerName(b *models.Booking) string {
	n := strings.TrimSpace(b.Customer.FirstName + " " + b.Customer.LastName)
	if n == "" {
		n = "Customer"
	}
	return n
}

// VehicleText describes the vehicle ("" when none).
func VehicleText(b *models.Booking) string {
	if b.Vehicle == nil {
		return ""
	}
	v := b.Vehicle
	t := strings.TrimSpace(strings.Join(strings.Fields(fmt.Sprintf("%s %s", v.Make, v.Model)), " "))
	if v.Year > 0 {
		t = fmt.Sprintf("%d %s", v.Year, t)
	}
	if v.LicensePlate != "" {
		t += " · " + v.LicensePlate
	}
	return strings.TrimSpace(t)
}

// Duration is "1 h 30 min".
func Duration(b *models.Booking) string {
	m := int(b.EndTime.Sub(b.StartTime).Minutes())
	switch {
	case m <= 0:
		return ""
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m%60 == 0:
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}
