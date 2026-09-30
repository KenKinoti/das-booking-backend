package handlers

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// In-app notifications for core module events (bookings, POS, stock).

func (h *Handler) actor(c *gin.Context) (string, string) {
	id := c.GetString("user_id")
	return id, notify.UserName(h.DB, id)
}

func bookingWho(b *models.Booking) string {
	n := strings.TrimSpace(b.Customer.FirstName + " " + b.Customer.LastName)
	if n == "" {
		n = "A customer"
	}
	return n
}

// orgTime renders a time in the organisation's time zone.
func (h *Handler) orgTime(org string, t time.Time, layout string) string {
	if tz := orgprefs.Get(h.DB, org).Timezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			t = t.In(loc)
		}
	}
	return t.Format(layout)
}

var bookingStatusText = map[string]string{
	"scheduled":   "scheduled",
	"confirmed":   "confirmed",
	"in_progress": "started",
	"completed":   "completed",
	"cancelled":   "cancelled",
	"no_show":     "marked as a no-show",
}

// notifyBooking tells admins (and the assigned staff member) about a booking
// being created or changing status.
func (h *Handler) notifyBooking(c *gin.Context, b *models.Booking, event, prevStatus string) {
	if b == nil || b.ID == "" {
		return
	}
	actorID, actorName := h.actor(c)
	to := notify.Admins()
	if b.StaffID != nil && *b.StaffID != "" {
		to = to.Plus(*b.StaffID)
	}
	to = to.Except(actorID)
	m := notify.Message{
		Link: "/bookings?booking=" + b.ID, EntityType: "booking", EntityID: b.ID, ActorID: actorID, ActorName: actorName,
	}
	body := fmt.Sprintf("%s · %s", bookingWho(b), h.orgTime(b.OrganizationID, b.StartTime, "Mon 2 Jan, 3:04 PM"))
	if len(b.Services) > 0 {
		names := make([]string, 0, len(b.Services))
		for _, s := range b.Services {
			names = append(names, s.Name)
		}
		body += " · " + strings.Join(names, ", ")
	}
	if actorName != "" {
		body += " · by " + actorName
	}
	m.Body = body
	switch event {
	case "created":
		m.Type, m.Severity, m.Title = "booking.created", notify.Info, "New booking"
	case "status":
		if b.Status == prevStatus {
			return
		}
		verb := bookingStatusText[b.Status]
		if verb == "" {
			verb = strings.ReplaceAll(b.Status, "_", " ")
		}
		m.Type = "booking." + b.Status
		m.Title = "Booking " + verb
		switch b.Status {
		case "cancelled", "no_show":
			m.Severity = notify.Warning
		case "completed", "confirmed":
			m.Severity = notify.Success
		default:
			m.Severity = notify.Info
		}
	case "deleted":
		m.Type, m.Severity, m.Title, m.Link = "booking.deleted", notify.Warning, "Booking deleted", "/bookings"
	default:
		return
	}
	notify.Go(h.DB, b.OrganizationID, to, m)
}

// notifyLowStock warns admins when a sale leaves products at or below their
// reorder level (one unread notice per product at a time).
func (h *Handler) notifyLowStock(orgID string, productIDs []string) {
	if len(productIDs) == 0 {
		return
	}
	var rows []struct {
		ID           string
		Name         string
		SKU          string
		CurrentStock float64
		MinStock     float64
	}
	if err := h.DB.Table("products").Select("id, name, sku, current_stock, "+invThresholdSQL+" AS min_stock").
		Where("organization_id = ? AND id IN ? AND deleted_at IS NULL", orgID, productIDs).Scan(&rows).Error; err != nil {
		return
	}
	for _, p := range rows {
		if p.CurrentStock > 0 && (p.MinStock <= 0 || p.CurrentStock > p.MinStock) {
			continue
		}
		m := notify.Message{Link: "/inventory?stock=low_stock", EntityType: "product", EntityID: p.ID, Dedupe: true}
		if p.CurrentStock <= 0 {
			m.Type, m.Severity, m.Link = "stock.out", notify.Error, "/inventory?stock=out_of_stock"
			m.Title = p.Name + " is out of stock"
			m.Body = "Reorder soon — it can’t be sold until stock arrives."
		} else {
			m.Type, m.Severity = "stock.low", notify.Warning
			m.Title = p.Name + " is running low"
			m.Body = fmt.Sprintf("%s left (reorder level %s).", trimNum(p.CurrentStock), trimNum(p.MinStock))
		}
		if p.SKU != "" {
			m.Body += " SKU " + p.SKU + "."
		}
		notify.Go(h.DB, orgID, notify.Admins(), m)
	}
}

func trimNum(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// notifyPOSVoid tells admins a sale was voided/refunded (stock restored).
func (h *Handler) notifyPOSVoid(c *gin.Context, t *models.POSTransaction, reason string) {
	if t == nil || t.ID == "" {
		return
	}
	actorID, actorName := h.actor(c)
	body := fmt.Sprintf("%s %s refunded", t.Currency, trimMoney(t.TotalAmount))
	if reason != "" {
		body += " — " + reason
	}
	if actorName != "" {
		body += " · by " + actorName
	}
	notify.Go(h.DB, t.OrganizationID, notify.Admins().Except(actorID), notify.Message{
		Type: "pos.voided", Severity: notify.Warning, Title: "Sale " + t.TransactionNumber + " voided",
		Body: strings.TrimSpace(body), Link: "/pos-transactions?range=all&q=" + url.QueryEscape(t.TransactionNumber), EntityType: "pos_transaction", EntityID: t.ID,
		ActorID: actorID, ActorName: actorName,
	})
}

func trimMoney(v float64) string { return fmt.Sprintf("%.2f", v) }
