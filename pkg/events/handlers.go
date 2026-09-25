// Package events implements organisation-scoped event management: events,
// ticket types, registrations/check-in, plus a small public registration API
// and a shareable landing page.
package events

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Handler serves the events API.
type Handler struct {
	db *gorm.DB
}

// NewHandler creates an events handler.
func NewHandler(db *gorm.DB) *Handler { return &Handler{db: db} }

var (
	validTypes       = map[string]bool{"in_person": true, "online": true, "hybrid": true}
	validStatuses    = map[string]bool{"draft": true, "published": true, "cancelled": true, "completed": true}
	validRegStatuses = map[string]bool{"pending": true, "confirmed": true, "cancelled": true, "waitlisted": true}
	validPayStatuses = map[string]bool{"pending": true, "paid": true, "free": true, "refunded": true}

	// DefaultCategories are offered in the UI alongside any the org already uses.
	DefaultCategories = []string{"Conference", "Workshop", "Seminar", "Networking", "Training", "Meetup", "Webinar", "Social", "Fundraiser", "Community", "Sports", "Other"}
)

// ---------------------------------------------------------------------------
// helpers

type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func newErr(status int, code, msg string) *apiError { return &apiError{status, code, msg} }

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}, "message": msg})
}

func failErr(c *gin.Context, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		fail(c, ae.status, ae.code, ae.msg)
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
}

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func ctxString(c *gin.Context, keys ...string) string {
	for _, k := range keys {
		v, exists := c.Get(k)
		if !exists || v == nil {
			continue
		}
		s := strings.TrimSpace(fmt.Sprint(v))
		if s != "" && s != "<nil>" {
			return s
		}
	}
	return ""
}

func (h *Handler) org(c *gin.Context) (string, bool) {
	org := ctxString(c, "org_id", "organization_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
		return "", false
	}
	return org, true
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5*sign(v))) / 100
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date %q", s)
}

func validEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

// ---------------------------------------------------------------------------
// views

type eventStats struct {
	Registrations int     `json:"registrations"` // active registrations (bookings)
	Tickets       int     `json:"tickets"`       // active tickets (sum of quantities)
	CheckedIn     int     `json:"checked_in"`    // tickets checked in
	Waitlisted    int     `json:"waitlisted"`
	Cancelled     int     `json:"cancelled"`
	Revenue       float64 `json:"revenue"`  // gross from active registrations
	Paid          float64 `json:"paid"`     // collected
	Capacity      int     `json:"capacity"` // 0 = unlimited
	Remaining     int     `json:"remaining"`
	FillRate      float64 `json:"fill_rate"` // percent, 0 when unlimited
}

type eventView struct {
	models.Event
	DisplayStatus string     `json:"display_status"`
	Stats         eventStats `json:"stats"`
}

type regAgg struct {
	EventID      string
	TicketTypeID string
	Status       string
	Regs         int
	Tickets      int
	CheckedIn    int
	Revenue      float64
	Paid         float64
}

func displayStatus(e *models.Event, now time.Time) string {
	switch e.Status {
	case "draft", "cancelled":
		return e.Status
	}
	if e.Status == "completed" || e.EndDate.Before(now) {
		return "completed"
	}
	if !e.StartDate.After(now) {
		return "live"
	}
	return "upcoming"
}

// eventCapacity is the effective capacity: the event cap if set, otherwise the
// sum of ticket quantities when every active ticket type is limited.
func eventCapacity(e *models.Event) int {
	if e.MaxCapacity > 0 {
		return e.MaxCapacity
	}
	total := 0
	active := 0
	for _, t := range e.TicketTypes {
		if !t.IsActive {
			continue
		}
		active++
		if t.Quantity <= 0 {
			return 0
		}
		total += t.Quantity
	}
	if active == 0 {
		return 0
	}
	return total
}

func (h *Handler) aggregates(db *gorm.DB, eventIDs []string) ([]regAgg, error) {
	var rows []regAgg
	if len(eventIDs) == 0 {
		return rows, nil
	}
	err := db.Model(&models.EventRegistration{}).
		Select(`event_id, ticket_type_id, status, COUNT(*) AS regs, COALESCE(SUM(quantity),0) AS tickets,
			COALESCE(SUM(CASE WHEN checked_in_at IS NOT NULL THEN quantity ELSE 0 END),0) AS checked_in,
			COALESCE(SUM(CASE WHEN payment_status <> 'refunded' THEN total_amount ELSE 0 END),0) AS revenue,
			COALESCE(SUM(CASE WHEN payment_status = 'paid' THEN total_amount ELSE 0 END),0) AS paid`).
		Where("event_id IN ?", eventIDs).
		Group("event_id, ticket_type_id, status").
		Scan(&rows).Error
	return rows, err
}

func isActiveReg(status string) bool { return status == "confirmed" || status == "pending" }

// buildViews decorates events with stats and per-ticket sold counts.
func (h *Handler) buildViews(events []models.Event) ([]eventView, error) {
	ids := make([]string, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	aggs, err := h.aggregates(h.db, ids)
	if err != nil {
		return nil, err
	}
	stats := map[string]*eventStats{}
	sold := map[string]int{}
	for _, a := range aggs {
		s := stats[a.EventID]
		if s == nil {
			s = &eventStats{}
			stats[a.EventID] = s
		}
		switch {
		case isActiveReg(a.Status):
			s.Registrations += a.Regs
			s.Tickets += a.Tickets
			s.CheckedIn += a.CheckedIn
			s.Revenue += a.Revenue
			s.Paid += a.Paid
			sold[a.TicketTypeID] += a.Tickets
		case a.Status == "waitlisted":
			s.Waitlisted += a.Tickets
		case a.Status == "cancelled":
			s.Cancelled += a.Regs
		}
	}
	now := time.Now()
	out := make([]eventView, 0, len(events))
	for _, e := range events {
		for i := range e.TicketTypes {
			e.TicketTypes[i].SoldQuantity = sold[e.TicketTypes[i].ID]
		}
		v := eventView{Event: e, DisplayStatus: displayStatus(&e, now)}
		if s := stats[e.ID]; s != nil {
			v.Stats = *s
		}
		v.Stats.Revenue = round2(v.Stats.Revenue)
		v.Stats.Paid = round2(v.Stats.Paid)
		v.Stats.Capacity = eventCapacity(&e)
		if v.Stats.Capacity > 0 {
			v.Stats.Remaining = v.Stats.Capacity - v.Stats.Tickets
			if v.Stats.Remaining < 0 {
				v.Stats.Remaining = 0
			}
			v.Stats.FillRate = round2(float64(v.Stats.Tickets) * 100 / float64(v.Stats.Capacity))
		}
		v.AvailableTickets = v.Stats.Remaining
		out = append(out, v)
	}
	return out, nil
}

func ticketOrder(db *gorm.DB) *gorm.DB { return db.Order("sort_order ASC, created_at ASC") }

func (h *Handler) loadEvent(db *gorm.DB, org, id string) (*models.Event, error) {
	var e models.Event
	err := db.Preload("TicketTypes", ticketOrder).
		Where("id = ? AND organization_id = ?", id, org).First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, newErr(http.StatusNotFound, "NOT_FOUND", "Event not found")
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (h *Handler) view(e *models.Event) (*eventView, error) {
	vs, err := h.buildViews([]models.Event{*e})
	if err != nil {
		return nil, err
	}
	return &vs[0], nil
}

// ---------------------------------------------------------------------------
// events

// ListEvents GET /events?scope=upcoming|past|draft|cancelled|all&category=&type=&search=&page=&per_page=
func (h *Handler) ListEvents(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	per, _ := strconv.Atoi(c.DefaultQuery("per_page", c.DefaultQuery("limit", "24")))
	if page < 1 {
		page = 1
	}
	if per < 1 || per > 200 {
		per = 24
	}
	now := time.Now()
	q := h.db.Model(&models.Event{}).Where("organization_id = ?", org)
	scope := c.DefaultQuery("scope", "all")
	order := "start_date DESC"
	switch scope {
	case "upcoming":
		q = q.Where("status = ? AND end_date >= ?", "published", now)
		order = "start_date ASC"
	case "past":
		q = q.Where("status IN ? AND (end_date < ? OR status = ?)", []string{"published", "completed"}, now, "completed")
	case "draft", "cancelled":
		q = q.Where("status = ?", scope)
	}
	if v := strings.TrimSpace(c.Query("category")); v != "" {
		q = q.Where("LOWER(category) = LOWER(?)", v)
	}
	if v := c.Query("type"); v != "" {
		q = q.Where("type = ?", v)
	}
	if v := strings.TrimSpace(c.Query("search")); v != "" {
		like := "%" + strings.ToLower(v) + "%"
		q = q.Where("(LOWER(title) LIKE ? OR LOWER(description) LIKE ? OR LOWER(venue_name) LIKE ? OR LOWER(category) LIKE ? OR LOWER(tags) LIKE ?)", like, like, like, like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		failErr(c, err)
		return
	}
	var events []models.Event
	if err := q.Preload("TicketTypes", ticketOrder).Order(order).Offset((page - 1) * per).Limit(per).Find(&events).Error; err != nil {
		failErr(c, err)
		return
	}
	views, err := h.buildViews(events)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"events": views, "total": total, "page": page, "per_page": per})
}

// Stats GET /events/stats — KPIs for the events overview.
func (h *Handler) Stats(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	now := time.Now()
	var events []models.Event
	if err := h.db.Preload("TicketTypes").Where("organization_id = ?", org).Find(&events).Error; err != nil {
		failErr(c, err)
		return
	}
	views, err := h.buildViews(events)
	if err != nil {
		failErr(c, err)
		return
	}
	var upcoming, drafts, live, past, regs, tickets, checkedIn, upTickets, upCapacity int
	var revenue, paid float64
	currencies := map[string]int{}
	for _, v := range views {
		currencies[strings.ToUpper(v.Currency)]++
	}
	currency, best := orgprefs.Currency(h.db, org), 0
	for k, n := range currencies {
		if k != "" && (n > best || (n == best && k < currency)) {
			currency, best = k, n
		}
	}
	// Money is summed in one currency only; other currencies are reported apart.
	otherRevenue := map[string]float64{}
	for _, v := range views {
		regs += v.Stats.Registrations
		tickets += v.Stats.Tickets
		checkedIn += v.Stats.CheckedIn
		if vc := strings.ToUpper(v.Currency); vc == "" || vc == currency {
			revenue += v.Stats.Revenue
			paid += v.Stats.Paid
		} else if v.Stats.Revenue != 0 {
			otherRevenue[vc] = round2(otherRevenue[vc] + v.Stats.Revenue)
		}
		switch v.DisplayStatus {
		case "draft":
			drafts++
		case "completed":
			past++
		case "upcoming", "live":
			if v.DisplayStatus == "live" {
				live++
			}
			upcoming++
			if v.Stats.Capacity > 0 {
				upTickets += v.Stats.Tickets
				upCapacity += v.Stats.Capacity
			}
		}
	}
	fill := 0.0
	if upCapacity > 0 {
		fill = round2(float64(upTickets) * 100 / float64(upCapacity))
	}
	var next *eventView
	for i := range views {
		v := &views[i]
		if v.DisplayStatus == "upcoming" && (next == nil || v.StartDate.Before(next.StartDate)) {
			next = v
		}
	}
	data := gin.H{
		"total_events": len(views), "upcoming": upcoming, "live": live, "drafts": drafts, "past": past,
		"registrations": regs, "tickets": tickets, "checked_in": checkedIn,
		"revenue": round2(revenue), "paid": round2(paid), "currency": currency, "other_currency_revenue": otherRevenue,
		"capacity_filled": fill, "upcoming_tickets": upTickets, "upcoming_capacity": upCapacity,
		"generated_at": now,
	}
	if next != nil {
		data["next_event"] = gin.H{"id": next.ID, "title": next.Title, "start_date": next.StartDate}
	}
	ok(c, http.StatusOK, data)
}

// Categories GET /events/categories — defaults plus categories the org uses.
func (h *Handler) Categories(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	var used []string
	h.db.Model(&models.Event{}).Where("organization_id = ? AND category <> ''", org).Distinct().Pluck("category", &used)
	seen := map[string]bool{}
	var out []string
	for _, s := range append(used, DefaultCategories...) {
		k := strings.ToLower(strings.TrimSpace(s))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, strings.TrimSpace(s))
	}
	sort.Strings(out)
	ok(c, http.StatusOK, gin.H{"categories": out, "used": used})
}

// GetEvent GET /events/:id
func (h *Handler) GetEvent(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	e, err := h.loadEvent(h.db, org, c.Param("id"))
	if err != nil {
		failErr(c, err)
		return
	}
	v, err := h.view(e)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, v)
}

type ticketInput struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
	Quantity    int     `json:"quantity"`
	MaxPerOrder int     `json:"max_per_order"`
	IsActive    *bool   `json:"is_active"`
	SortOrder   *int    `json:"sort_order"`
}

type eventInput struct {
	Title            string          `json:"title"`
	Description      string          `json:"description"`
	ShortDescription string          `json:"short_description"`
	Category         string          `json:"category"`
	Type             string          `json:"type"`
	Status           string          `json:"status"`
	StartDate        string          `json:"start_date"`
	EndDate          string          `json:"end_date"`
	Timezone         string          `json:"timezone"`
	VenueName        string          `json:"venue_name"`
	Address          *models.Address `json:"address"`
	OnlineURL        string          `json:"online_url"`
	OnlinePlatform   string          `json:"online_platform"`
	MaxCapacity      int             `json:"max_capacity"`
	Currency         string          `json:"currency"`
	CoverImageURL    string          `json:"cover_image_url"`
	IsPublic         *bool           `json:"is_public"`
	RequiresApproval *bool           `json:"requires_approval"`
	AllowWaitlist    *bool           `json:"allow_waitlist"`
	Tags             string          `json:"tags"`
	ContactName      string          `json:"contact_name"`
	ContactEmail     string          `json:"contact_email"`
	TicketTypes      *[]ticketInput  `json:"ticket_types"`
}

func (in *eventInput) apply(e *models.Event) error {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return newErr(http.StatusBadRequest, "VALIDATION", "Title is required")
	}
	if len(in.Title) > 255 {
		return newErr(http.StatusBadRequest, "VALIDATION", "Title is too long (max 255 characters)")
	}
	if in.Type == "" {
		in.Type = "in_person"
	}
	if !validTypes[in.Type] {
		return newErr(http.StatusBadRequest, "VALIDATION", "Type must be in_person, online or hybrid")
	}
	if in.Status == "" {
		in.Status = "draft"
	}
	if !validStatuses[in.Status] {
		return newErr(http.StatusBadRequest, "VALIDATION", "Invalid status")
	}
	if in.StartDate == "" || in.EndDate == "" {
		return newErr(http.StatusBadRequest, "VALIDATION", "Start and end date/time are required")
	}
	start, err := parseTime(in.StartDate)
	if err != nil {
		return newErr(http.StatusBadRequest, "VALIDATION", "Invalid start date")
	}
	end, err := parseTime(in.EndDate)
	if err != nil {
		return newErr(http.StatusBadRequest, "VALIDATION", "Invalid end date")
	}
	if !end.After(start) {
		return newErr(http.StatusBadRequest, "VALIDATION", "End must be after the start")
	}
	if in.MaxCapacity < 0 {
		return newErr(http.StatusBadRequest, "VALIDATION", "Capacity cannot be negative")
	}
	if in.ContactEmail != "" && !validEmail(strings.TrimSpace(in.ContactEmail)) {
		return newErr(http.StatusBadRequest, "VALIDATION", "Contact email is not valid")
	}
	if in.Status == "published" {
		if in.Type != "online" && strings.TrimSpace(in.VenueName) == "" && (in.Address == nil || strings.TrimSpace(in.Address.Street) == "") {
			return newErr(http.StatusBadRequest, "VALIDATION", "Add a venue before publishing an in-person event")
		}
		if in.Type != "in_person" && strings.TrimSpace(in.OnlineURL) == "" {
			return newErr(http.StatusBadRequest, "VALIDATION", "Add an online URL before publishing an online event")
		}
	}
	if in.TicketTypes != nil {
		for i, t := range *in.TicketTypes {
			if strings.TrimSpace(t.Name) == "" {
				return newErr(http.StatusBadRequest, "VALIDATION", fmt.Sprintf("Ticket %d needs a name", i+1))
			}
			if t.Price < 0 || t.Quantity < 0 {
				return newErr(http.StatusBadRequest, "VALIDATION", fmt.Sprintf("Ticket %q has a negative price or quantity", t.Name))
			}
		}
	}
	e.Title = in.Title
	e.Description = in.Description
	e.ShortDescription = strings.TrimSpace(in.ShortDescription)
	if len(e.ShortDescription) > 500 {
		e.ShortDescription = e.ShortDescription[:500]
	}
	e.Category = strings.TrimSpace(in.Category)
	e.Type = in.Type
	e.Status = in.Status
	e.StartDate = start.UTC()
	e.EndDate = end.UTC()
	e.Duration = int(end.Sub(start).Minutes())
	if in.Timezone != "" {
		e.Timezone = in.Timezone
	} else if e.Timezone == "" {
		e.Timezone = "UTC"
	}
	e.VenueName = strings.TrimSpace(in.VenueName)
	if in.Address != nil {
		e.Address = *in.Address
	}
	if e.Address.Country == "" {
		e.Address.Country = "Australia"
	}
	e.OnlineURL = strings.TrimSpace(in.OnlineURL)
	e.OnlinePlatform = strings.TrimSpace(in.OnlinePlatform)
	if e.Type == "online" {
		e.VenueName = ""
		e.Address = models.Address{Country: e.Address.Country}
	}
	if e.Type == "in_person" {
		e.OnlineURL = ""
		e.OnlinePlatform = ""
	}
	e.MaxCapacity = in.MaxCapacity
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if len(cur) == 3 {
		e.Currency = cur
	} else if e.Currency == "" {
		e.Currency = "AUD"
	}
	e.CoverImageURL = strings.TrimSpace(in.CoverImageURL)
	if in.IsPublic != nil {
		e.IsPublic = *in.IsPublic
	}
	if in.RequiresApproval != nil {
		e.RequiresApproval = *in.RequiresApproval
	}
	if in.AllowWaitlist != nil {
		e.AllowWaitlist = *in.AllowWaitlist
	}
	e.Tags = strings.TrimSpace(in.Tags)
	e.ContactName = strings.TrimSpace(in.ContactName)
	e.ContactEmail = strings.TrimSpace(in.ContactEmail)
	return nil
}

func applyPricing(e *models.Event, tickets []models.TicketType) {
	minPrice := -1.0
	for _, t := range tickets {
		if !t.IsActive {
			continue
		}
		if minPrice < 0 || t.Price < minPrice {
			minPrice = t.Price
		}
	}
	if minPrice < 0 {
		minPrice = 0
	}
	e.BasePrice = round2(minPrice)
	free := true
	for _, t := range tickets {
		if t.IsActive && t.Price > 0 {
			free = false
		}
	}
	e.IsFree = free
}

// syncTickets makes the event's ticket types match the input list.
func (h *Handler) syncTickets(tx *gorm.DB, e *models.Event, in []ticketInput) ([]models.TicketType, error) {
	var existing []models.TicketType
	if err := tx.Where("event_id = ?", e.ID).Find(&existing).Error; err != nil {
		return nil, err
	}
	byID := map[string]*models.TicketType{}
	for i := range existing {
		byID[existing[i].ID] = &existing[i]
	}
	keep := map[string]bool{}
	var result []models.TicketType
	for i, t := range in {
		var tt models.TicketType
		if cur, found := byID[t.ID]; t.ID != "" && found {
			tt = *cur
		} else {
			tt = models.TicketType{EventID: e.ID, MinPerOrder: 1, IsActive: true}
		}
		tt.Name = strings.TrimSpace(t.Name)
		tt.Description = strings.TrimSpace(t.Description)
		tt.Price = round2(t.Price)
		tt.Quantity = t.Quantity
		tt.MaxPerOrder = t.MaxPerOrder
		if tt.MaxPerOrder <= 0 {
			tt.MaxPerOrder = 10
		}
		if t.IsActive != nil {
			tt.IsActive = *t.IsActive
		}
		tt.SortOrder = i
		if t.SortOrder != nil {
			tt.SortOrder = *t.SortOrder
		}
		if err := tx.Save(&tt).Error; err != nil {
			return nil, err
		}
		keep[tt.ID] = true
		result = append(result, tt)
	}
	for _, old := range existing {
		if keep[old.ID] {
			continue
		}
		var n int64
		tx.Model(&models.EventRegistration{}).Where("ticket_type_id = ?", old.ID).Count(&n)
		if n > 0 {
			// Has registrations: keep for history but take it off sale.
			old.IsActive = false
			if err := tx.Save(&old).Error; err != nil {
				return nil, err
			}
			result = append(result, old)
			continue
		}
		if err := tx.Delete(&old).Error; err != nil {
			return nil, err
		}
	}
	return result, nil
}

// CreateEvent POST /events
func (h *Handler) CreateEvent(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	var in eventInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body: "+err.Error())
		return
	}
	// New events default to the organisation's home currency and country.
	prefs := orgprefs.Get(h.db, org)
	e := models.Event{OrganizationID: org, CreatedBy: ctxString(c, "user_id"), IsPublic: true, Currency: prefs.Currency}
	if err := in.apply(&e); err != nil {
		failErr(c, err)
		return
	}
	if (in.Address == nil || strings.TrimSpace(in.Address.Country) == "") && prefs.CountryName != "" {
		e.Address.Country = prefs.CountryName
	}
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&e).Error; err != nil {
			return err
		}
		tickets := []ticketInput{}
		if in.TicketTypes != nil {
			tickets = *in.TicketTypes
		}
		if len(tickets) == 0 {
			tickets = []ticketInput{{Name: "General admission", Quantity: 0}}
		}
		tts, err := h.syncTickets(tx, &e, tickets)
		if err != nil {
			return err
		}
		applyPricing(&e, tts)
		return tx.Model(&e).Select("base_price", "is_free").Updates(&e).Error
	})
	if err != nil {
		failErr(c, err)
		return
	}
	h.respondEvent(c, org, e.ID, http.StatusCreated)
}

func (h *Handler) respondEvent(c *gin.Context, org, id string, status int) {
	e, err := h.loadEvent(h.db, org, id)
	if err != nil {
		failErr(c, err)
		return
	}
	v, err := h.view(e)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, status, v)
}

// UpdateEvent PUT /events/:id
func (h *Handler) UpdateEvent(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	var in eventInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body: "+err.Error())
		return
	}
	id := c.Param("id")
	err := h.db.Transaction(func(tx *gorm.DB) error {
		e, err := h.loadEvent(tx, org, id)
		if err != nil {
			return err
		}
		if err := in.apply(e); err != nil {
			return err
		}
		tickets := e.TicketTypes
		if in.TicketTypes != nil {
			if tickets, err = h.syncTickets(tx, e, *in.TicketTypes); err != nil {
				return err
			}
		}
		applyPricing(e, tickets)
		e.TicketTypes = nil
		return tx.Omit(clause.Associations).Save(e).Error
	})
	if err != nil {
		failErr(c, err)
		return
	}
	h.respondEvent(c, org, id, http.StatusOK)
}

// SetStatus POST /events/:id/status {status}
func (h *Handler) SetStatus(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || !validStatuses[body.Status] {
		fail(c, http.StatusBadRequest, "VALIDATION", "Status must be draft, published, cancelled or completed")
		return
	}
	e, err := h.loadEvent(h.db, org, c.Param("id"))
	if err != nil {
		failErr(c, err)
		return
	}
	if body.Status == "published" {
		if e.Type != "online" && e.VenueName == "" && e.Address.Street == "" {
			fail(c, http.StatusBadRequest, "VALIDATION", "Add a venue before publishing an in-person event")
			return
		}
		if e.Type != "in_person" && e.OnlineURL == "" {
			fail(c, http.StatusBadRequest, "VALIDATION", "Add an online URL before publishing an online event")
			return
		}
	}
	if err := h.db.Model(&models.Event{}).Where("id = ? AND organization_id = ?", e.ID, org).Update("status", body.Status).Error; err != nil {
		failErr(c, err)
		return
	}
	h.respondEvent(c, org, e.ID, http.StatusOK)
}

// DuplicateEvent POST /events/:id/duplicate — copies the event as a draft.
func (h *Handler) DuplicateEvent(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	src, err := h.loadEvent(h.db, org, c.Param("id"))
	if err != nil {
		failErr(c, err)
		return
	}
	cp := *src
	cp.ID = ""
	cp.Title = strings.TrimSpace(src.Title + " (copy)")
	if len(cp.Title) > 255 {
		cp.Title = cp.Title[:255]
	}
	cp.Status = "draft"
	cp.CreatedBy = ctxString(c, "user_id")
	cp.CreatedAt, cp.UpdatedAt = time.Time{}, time.Time{}
	cp.TicketTypes, cp.Registrations, cp.Sessions, cp.Reviews, cp.GalleryImages = nil, nil, nil, nil, nil
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&cp).Error; err != nil {
			return err
		}
		for _, t := range src.TicketTypes {
			nt := t
			nt.ID = ""
			nt.EventID = cp.ID
			nt.SoldQuantity = 0
			nt.CreatedAt, nt.UpdatedAt = time.Time{}, time.Time{}
			if err := tx.Create(&nt).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		failErr(c, err)
		return
	}
	h.respondEvent(c, org, cp.ID, http.StatusCreated)
}

// DeleteEvent DELETE /events/:id — soft-deletes the event, its tickets and registrations.
func (h *Handler) DeleteEvent(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	e, err := h.loadEvent(h.db, org, c.Param("id"))
	if err != nil {
		failErr(c, err)
		return
	}
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("event_id = ?", e.ID).Delete(&models.EventRegistration{}).Error; err != nil {
			return err
		}
		if err := tx.Where("event_id = ?", e.ID).Delete(&models.TicketType{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.Event{}, "id = ? AND organization_id = ?", e.ID, org).Error
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"id": e.ID, "deleted": true})
}

// ---------------------------------------------------------------------------
// registrations

type regInput struct {
	RegistrantName      *string `json:"registrant_name"`
	RegistrantEmail     *string `json:"registrant_email"`
	RegistrantPhone     *string `json:"registrant_phone"`
	Company             *string `json:"company"`
	TicketTypeID        *string `json:"ticket_type_id"`
	Quantity            *int    `json:"quantity"`
	Status              *string `json:"status"`
	PaymentStatus       *string `json:"payment_status"`
	PaymentMethod       *string `json:"payment_method"`
	Notes               *string `json:"notes"`
	DietaryRequirements *string `json:"dietary_requirements"`
	SpecialRequests     *string `json:"special_requests"`
	CheckedIn           *bool   `json:"checked_in"`
}

func strv(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// checkCapacity verifies the event/ticket can take qty more tickets.
func (h *Handler) checkCapacity(tx *gorm.DB, e *models.Event, tt *models.TicketType, qty int, excludeRegID string) error {
	type row struct {
		TicketTypeID string
		Tickets      int
	}
	var rows []row
	q := tx.Model(&models.EventRegistration{}).
		Select("ticket_type_id, COALESCE(SUM(quantity),0) AS tickets").
		Where("event_id = ? AND status IN ?", e.ID, []string{"confirmed", "pending"})
	if excludeRegID != "" {
		q = q.Where("id <> ?", excludeRegID)
	}
	if err := q.Group("ticket_type_id").Scan(&rows).Error; err != nil {
		return err
	}
	total, forTicket := 0, 0
	for _, r := range rows {
		total += r.Tickets
		if tt != nil && r.TicketTypeID == tt.ID {
			forTicket += r.Tickets
		}
	}
	if tt != nil && tt.Quantity > 0 && forTicket+qty > tt.Quantity {
		left := tt.Quantity - forTicket
		if left < 0 {
			left = 0
		}
		return newErr(http.StatusConflict, "SOLD_OUT", fmt.Sprintf("Only %d %q ticket(s) left", left, tt.Name))
	}
	if capy := eventCapacity(e); capy > 0 && total+qty > capy {
		left := capy - total
		if left < 0 {
			left = 0
		}
		return newErr(http.StatusConflict, "SOLD_OUT", fmt.Sprintf("Event capacity reached — %d place(s) left", left))
	}
	return nil
}

func findTicket(e *models.Event, id string) *models.TicketType {
	for i := range e.TicketTypes {
		if e.TicketTypes[i].ID == id {
			return &e.TicketTypes[i]
		}
	}
	return nil
}

func lockEvent(tx *gorm.DB, org, id string) error {
	var e models.Event
	q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("id = ?", id)
	if org != "" {
		q = q.Where("organization_id = ?", org)
	}
	err := q.First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return newErr(http.StatusNotFound, "NOT_FOUND", "Event not found")
	}
	return err
}

// ListRegistrations GET /events/:id/registrations?search=&status=&checked_in=yes|no&ticket_type_id=
func (h *Handler) ListRegistrations(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	e, err := h.loadEvent(h.db, org, c.Param("id"))
	if err != nil {
		failErr(c, err)
		return
	}
	regs, err := h.queryRegistrations(c, e.ID)
	if err != nil {
		failErr(c, err)
		return
	}
	v, err := h.view(e)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"registrations": regs, "total": len(regs), "summary": v.Stats})
}

func (h *Handler) queryRegistrations(c *gin.Context, eventID string) ([]models.EventRegistration, error) {
	q := h.db.Where("event_id = ?", eventID)
	if s := c.Query("status"); s != "" && s != "all" {
		q = q.Where("status = ?", s)
	}
	switch c.Query("checked_in") {
	case "yes", "true":
		q = q.Where("checked_in_at IS NOT NULL")
	case "no", "false":
		q = q.Where("checked_in_at IS NULL")
	}
	if t := c.Query("ticket_type_id"); t != "" {
		q = q.Where("ticket_type_id = ?", t)
	}
	if s := strings.TrimSpace(c.Query("search")); s != "" {
		like := "%" + strings.ToLower(s) + "%"
		q = q.Where("(LOWER(registrant_name) LIKE ? OR LOWER(registrant_email) LIKE ? OR LOWER(registrant_phone) LIKE ? OR LOWER(company) LIKE ? OR LOWER(qr_code) LIKE ?)", like, like, like, like, like)
	}
	var regs []models.EventRegistration
	err := q.Order("created_at DESC").Limit(5000).Find(&regs).Error
	return regs, err
}

// CreateRegistration POST /events/:id/registrations — add an attendee manually.
func (h *Handler) CreateRegistration(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	var in regInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body")
		return
	}
	var reg models.EventRegistration
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := lockEvent(tx, org, c.Param("id")); err != nil {
			return err
		}
		e, err := h.loadEvent(tx, org, c.Param("id"))
		if err != nil {
			return err
		}
		reg = models.EventRegistration{OrganizationID: org, EventID: e.ID, Source: "manual", Quantity: 1, Status: "confirmed"}
		if err := h.applyReg(tx, e, &reg, &in, true); err != nil {
			return err
		}
		return tx.Create(&reg).Error
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusCreated, reg)
}

// applyReg validates input and applies it to reg (create or update).
func (h *Handler) applyReg(tx *gorm.DB, e *models.Event, reg *models.EventRegistration, in *regInput, creating bool) error {
	if in.RegistrantName != nil || creating {
		reg.RegistrantName = strv(in.RegistrantName)
		if reg.RegistrantName == "" {
			return newErr(http.StatusBadRequest, "VALIDATION", "Attendee name is required")
		}
	}
	if in.RegistrantEmail != nil || creating {
		reg.RegistrantEmail = strings.ToLower(strv(in.RegistrantEmail))
		if !validEmail(reg.RegistrantEmail) {
			return newErr(http.StatusBadRequest, "VALIDATION", "A valid email is required")
		}
	}
	if in.RegistrantPhone != nil {
		reg.RegistrantPhone = strv(in.RegistrantPhone)
	}
	if in.Company != nil {
		reg.Company = strv(in.Company)
	}
	if in.Notes != nil {
		reg.Notes = strv(in.Notes)
	}
	if in.DietaryRequirements != nil {
		reg.DietaryRequirements = strv(in.DietaryRequirements)
	}
	if in.SpecialRequests != nil {
		reg.SpecialRequests = strv(in.SpecialRequests)
	}
	if in.PaymentMethod != nil {
		reg.PaymentMethod = strv(in.PaymentMethod)
	}
	if in.Status != nil {
		if !validRegStatuses[*in.Status] {
			return newErr(http.StatusBadRequest, "VALIDATION", "Invalid registration status")
		}
		reg.Status = *in.Status
	}
	if in.Quantity != nil {
		if *in.Quantity < 1 || *in.Quantity > 100 {
			return newErr(http.StatusBadRequest, "VALIDATION", "Quantity must be between 1 and 100")
		}
		reg.Quantity = *in.Quantity
	}
	ticketChanged := false
	if in.TicketTypeID != nil && *in.TicketTypeID != reg.TicketTypeID {
		reg.TicketTypeID = *in.TicketTypeID
		ticketChanged = true
	}
	if reg.TicketTypeID == "" {
		// default to the first active ticket
		for _, t := range e.TicketTypes {
			if t.IsActive {
				reg.TicketTypeID = t.ID
				ticketChanged = true
				break
			}
		}
	}
	tt := findTicket(e, reg.TicketTypeID)
	if reg.TicketTypeID != "" && tt == nil {
		return newErr(http.StatusBadRequest, "VALIDATION", "Unknown ticket type")
	}
	if tt != nil {
		reg.TicketTypeName = tt.Name
		if ticketChanged || creating || in.Quantity != nil {
			reg.UnitPrice = tt.Price
		}
	}
	reg.TotalAmount = round2(reg.UnitPrice * float64(reg.Quantity))
	if in.PaymentStatus != nil {
		if !validPayStatuses[*in.PaymentStatus] {
			return newErr(http.StatusBadRequest, "VALIDATION", "Invalid payment status")
		}
		reg.PaymentStatus = *in.PaymentStatus
	}
	if reg.PaymentStatus == "" || (reg.TotalAmount == 0 && reg.PaymentStatus == "pending") {
		if reg.TotalAmount == 0 {
			reg.PaymentStatus = "free"
		} else {
			reg.PaymentStatus = "pending"
		}
	}
	if reg.TotalAmount > 0 && reg.PaymentStatus == "free" {
		reg.PaymentStatus = "pending"
	}
	if isActiveReg(reg.Status) {
		if err := h.checkCapacity(tx, e, tt, reg.Quantity, reg.ID); err != nil {
			return err
		}
	}
	if in.CheckedIn != nil {
		setCheckIn(reg, *in.CheckedIn, "")
	}
	return nil
}

func setCheckIn(reg *models.EventRegistration, in bool, by string) {
	if in {
		if reg.CheckedInAt == nil {
			now := time.Now()
			reg.CheckedInAt = &now
			reg.CheckedInBy = by
		}
	} else {
		reg.CheckedInAt = nil
		reg.CheckedInBy = ""
	}
}

func (h *Handler) loadReg(tx *gorm.DB, org, eventID, regID string) (*models.EventRegistration, error) {
	var reg models.EventRegistration
	err := tx.Where("id = ? AND event_id = ?", regID, eventID).First(&reg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, newErr(http.StatusNotFound, "NOT_FOUND", "Registration not found")
	}
	return &reg, err
}

// UpdateRegistration PUT /events/:id/registrations/:regId
func (h *Handler) UpdateRegistration(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	var in regInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body")
		return
	}
	var reg *models.EventRegistration
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := lockEvent(tx, org, c.Param("id")); err != nil {
			return err
		}
		e, err := h.loadEvent(tx, org, c.Param("id"))
		if err != nil {
			return err
		}
		if reg, err = h.loadReg(tx, org, e.ID, c.Param("regId")); err != nil {
			return err
		}
		if in.CheckedIn != nil && *in.CheckedIn {
			setCheckIn(reg, true, ctxString(c, "user_id"))
		}
		if err := h.applyReg(tx, e, reg, &in, false); err != nil {
			return err
		}
		return tx.Save(reg).Error
	})
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, reg)
}

// CheckIn POST /events/:id/registrations/:regId/check-in {checked_in: bool}
func (h *Handler) CheckIn(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	body := struct {
		CheckedIn *bool `json:"checked_in"`
	}{}
	_ = c.ShouldBindJSON(&body)
	want := true
	if body.CheckedIn != nil {
		want = *body.CheckedIn
	}
	e, err := h.loadEvent(h.db, org, c.Param("id"))
	if err != nil {
		failErr(c, err)
		return
	}
	reg, err := h.loadReg(h.db, org, e.ID, c.Param("regId"))
	if err != nil {
		failErr(c, err)
		return
	}
	if want && !isActiveReg(reg.Status) {
		fail(c, http.StatusConflict, "NOT_CHECKABLE", "Only confirmed or pending registrations can be checked in")
		return
	}
	setCheckIn(reg, want, ctxString(c, "user_id"))
	if want && reg.Status == "pending" {
		reg.Status = "confirmed"
	}
	if err := h.db.Model(reg).Select("checked_in_at", "checked_in_by", "status").Updates(reg).Error; err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, reg)
}

// DeleteRegistration DELETE /events/:id/registrations/:regId
func (h *Handler) DeleteRegistration(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	e, err := h.loadEvent(h.db, org, c.Param("id"))
	if err != nil {
		failErr(c, err)
		return
	}
	reg, err := h.loadReg(h.db, org, e.ID, c.Param("regId"))
	if err != nil {
		failErr(c, err)
		return
	}
	if err := h.db.Delete(reg).Error; err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"id": reg.ID, "deleted": true})
}

// ExportRegistrations GET /events/:id/registrations/export — CSV download.
func (h *Handler) ExportRegistrations(c *gin.Context) {
	org, okOrg := h.org(c)
	if !okOrg {
		return
	}
	e, err := h.loadEvent(h.db, org, c.Param("id"))
	if err != nil {
		failErr(c, err)
		return
	}
	regs, err := h.queryRegistrations(c, e.ID)
	if err != nil {
		failErr(c, err)
		return
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"Reference", "Name", "Email", "Phone", "Company", "Ticket", "Quantity", "Unit price", "Total", "Currency", "Status", "Payment", "Checked in at", "Registered at", "Source", "Dietary requirements", "Notes"})
	for _, r := range regs {
		checked := ""
		if r.CheckedInAt != nil {
			checked = r.CheckedInAt.Format("2006-01-02 15:04")
		}
		_ = w.Write([]string{
			r.QRCode, csvSafe(r.RegistrantName), csvSafe(r.RegistrantEmail), csvSafe(r.RegistrantPhone), csvSafe(r.Company), csvSafe(r.TicketTypeName),
			strconv.Itoa(r.Quantity), fmt.Sprintf("%.2f", r.UnitPrice), fmt.Sprintf("%.2f", r.TotalAmount), e.Currency,
			r.Status, r.PaymentStatus, checked, r.RegistrationDate.Format("2006-01-02 15:04"), r.Source, csvSafe(r.DietaryRequirements), csvSafe(r.Notes),
		})
	}
	w.Flush()
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, e.Title)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-registrations.csv"`, name))
	c.Data(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
}

// csvSafe neutralises spreadsheet formula injection.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		return "'" + s
	}
	return s
}
