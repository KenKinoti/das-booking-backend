package schedsend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/wa"
)

// Handler serves the API.
type Handler struct {
	S *Service
	// TestHooks (SCHEDSEND_TEST_CLOCK=1) adds the test clock and run endpoints.
	TestHooks bool
}

// RegisterRoutes mounts
//
//	GET    /schedsend/meta                     time zone, quick picks, channels, merge fields, permissions
//	GET    /schedsend/messages                 ?tab=upcoming|recurring|sent|failed&kind=&channel=&q=&invoice_id=
//	POST   /schedsend/messages                 schedule (audit schedsend.create)
//	POST   /schedsend/preview                  per-recipient preview of an unsaved message (?pick=<id>)
//	GET    /schedsend/messages/:id             detail with runs and per-recipient results (?run=)
//	PUT    /schedsend/messages/:id             edit
//	POST   /schedsend/messages/:id/pause|resume|cancel|send-now|duplicate
//	GET    /schedsend/badges?ids=a,b           scheduled sends per invoice
//	GET    /schedsend/customers/:id/marketing  PUT {opted_out}
//	GET    /schedsend/sms-settings             PUT (admins), POST /schedsend/sms-settings/test {to}
//	public GET|POST /schedsend/unsubscribe?t=, GET /schedsend/open/:token, POST /sms/africastalking/delivery, POST /sms/twilio/status
func (h *Handler) RegisterRoutes(auth, public *gin.RouterGroup) {
	g := auth.Group("/schedsend")
	g.GET("/meta", h.meta)
	g.GET("/messages", h.list)
	g.POST("/messages", h.create)
	g.POST("/preview", h.preview)
	g.GET("/messages/:id", h.get)
	g.PUT("/messages/:id", h.update)
	g.POST("/messages/:id/:action", h.action)
	g.GET("/badges", h.badges)
	g.GET("/staff", h.staff)
	g.GET("/customers/:id/marketing", h.getOptOut)
	g.PUT("/customers/:id/marketing", h.putOptOut)
	g.GET("/sms-settings", h.getSMS)
	g.PUT("/sms-settings", h.putSMS)
	g.POST("/sms-settings/test", h.testSMS)
	if h.TestHooks {
		g.POST("/test-clock", h.testClock)
		g.POST("/test-run", h.testRun)
	}
	public.GET("/schedsend/unsubscribe", h.unsubscribePage)
	public.POST("/schedsend/unsubscribe", h.unsubscribe)
	public.GET("/schedsend/open/:token", h.open)
	public.POST("/sms/africastalking/delivery", h.atDelivery)
	public.POST("/sms/twilio/status", h.twilioStatus)
}

func str(c *gin.Context, key string) string {
	v, ok := c.Get(key)
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func (h *Handler) actor(c *gin.Context) (Actor, bool) {
	a := Actor{Org: str(c, "org_id"), User: str(c, "user_id"), Role: str(c, "user_role"), Email: str(c, "user_email")}
	if a.Org == "" || a.User == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": gin.H{"code": "UNAUTHORIZED", "message": "Sign in again"}})
		return a, false
	}
	var u struct{ FirstName, LastName string }
	fresh(h.S.DB).Raw(`SELECT COALESCE(first_name,'') AS first_name, COALESCE(last_name,'') AS last_name FROM users WHERE id = ?`, a.User).Scan(&u)
	a.Name = firstNonEmpty(strings.TrimSpace(u.FirstName+" "+u.LastName), a.Email)
	return a, true
}

func ok(c *gin.Context, data any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": data}) }

func fail(c *gin.Context, err error) {
	var e *Error
	if errors.As(err, &e) {
		c.JSON(e.Status, gin.H{"success": false, "error": gin.H{"code": e.Code, "message": e.Msg}})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"code": "SERVER_ERROR", "message": err.Error()}})
}

// audit writes one audit_logs row (never fails the request).
func (h *Handler) audit(c *gin.Context, a Actor, action, id string, values any) {
	if !hasTable(h.S.DB, "audit_logs") {
		return
	}
	b, err := json.Marshal(values)
	if err != nil || string(b) == "null" {
		b = []byte("{}")
	}
	uid := a.User
	ua := c.Request.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}
	row := models.AuditLog{ID: uuid.NewString(), OrganizationID: a.Org, UserID: &uid, Action: action, EntityType: "scheduled_message", EntityID: id,
		OldValues: "{}", NewValues: string(b), IPAddress: c.ClientIP(), UserAgent: ua, CreatedAt: time.Now()}
	fresh(h.S.DB).Omit("Organization", "User").Create(&row)
}

func auditValues(m *Message) gin.H {
	return gin.H{"kind": m.Kind, "title": m.Title, "channels": m.Channels, "next_run_at": m.NextRunAt, "timezone": m.Timezone,
		"repeat": m.Repeat.Describe(m.LocalSendAt), "invoice": m.DocNumber}
}

// ---------------------------------------------------------------- meta

// Option is a key / label pair.
type Option struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count,omitempty"`
}

func (h *Handler) meta(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	s := h.S
	tz := strings.TrimSpace(c.Query("tz"))
	if _, err := time.LoadLocation(tz); tz == "" || err != nil {
		tz = s.orgTZ(a.Org)
	}
	now := Now()
	emailOK, from, problem := s.EmailReady(a.Org)
	countries := []Option{}
	if hasTable(s.DB, "customers") {
		var rows []struct {
			Code string
			N    int
		}
		fresh(s.DB).Raw(`SELECT UPPER(country_code) AS code, COUNT(*) AS n FROM customers WHERE organization_id = ? AND deleted_at IS NULL AND COALESCE(country_code,'') <> ''
			GROUP BY UPPER(country_code) ORDER BY n DESC`, a.Org).Scan(&rows)
		for _, r := range rows {
			countries = append(countries, Option{Key: r.Code, Label: r.Code, Count: r.N})
		}
	}
	groups := []Option{
		{GroupAllEmail, "Everyone with an email address", 0}, {GroupAllPhone, "Everyone with a phone number", 0},
		{GroupOverdue, "Customers with overdue invoices", 0}, {GroupOpenBalance, "Customers with unpaid invoices", 0},
		{GroupSubscriptions, "Customers with active subscriptions", 0}, {GroupType, "A customer type (business / individual)", 0},
		{GroupCountry, "Customers in a country", 0},
	}
	ok(c, gin.H{
		"timezone": tz, "org_timezone": s.orgTZ(a.Org), "now": now, "now_local": FormatLocal(now, tz), "quick_picks": QuickPicks(now, tz),
		"merge_fields": MergeFields, "groups": groups, "countries": countries, "country": s.orgCountry(a.Org),
		"channels": gin.H{
			"email":    gin.H{"ready": emailOK, "from": from, "note": problem},
			"whatsapp": wa.StatusOf(fresh(s.DB), a.Org, false),
			"sms":      StatusOfSMS(s.DB, a.Org),
		},
		"permissions": gin.H{"invoices": s.canInvoices(a), "bulk": a.managerUp() && !a.readOnly(), "team": a.managerUp(), "level": a.Level(),
			"single": !a.readOnly(), "admin": a.Level() == "owner"},
		"catch_up_hours": int(CatchUpWindow.Hours()), "test_clock": h.TestHooks, "clock_offset_seconds": int(ClockOffset().Seconds()),
	})
}

// ---------------------------------------------------------------- CRUD

func (h *Handler) list(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	f := ListFilter{Tab: c.DefaultQuery("tab", "upcoming"), Kind: c.Query("kind"), Channel: c.Query("channel"), Search: c.Query("q"), InvoiceID: c.Query("invoice_id"),
		Mine: c.Query("mine") == "1"}
	list, counts, err := h.S.List(a, f)
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]gin.H, 0, len(list))
	for i := range list {
		m := &list[i]
		out = append(out, gin.H{"message": m, "repeat_text": m.Repeat.Describe(m.LocalSendAt), "recipients_text": h.recipientsText(m)})
	}
	ok(c, gin.H{"items": out, "counts": counts})
}

// recipientsText is "Wanjiru Kamau", "12 customers", "Everyone with an email address" …
func (h *Handler) recipientsText(m *Message) string {
	switch m.Kind {
	case KindInvoice, KindReceipt:
		return firstNonEmpty(m.InvoiceSend.To, m.CustomerName)
	case KindReminder:
		return m.CustomerName
	case KindTeam:
		switch {
		case len(m.Audience.UserIDs) == 1:
			return "1 person"
		case len(m.Audience.UserIDs) > 1:
			return fmt.Sprintf("%d people", len(m.Audience.UserIDs))
		case len(m.Audience.Levels) > 0:
			return strings.Join(m.Audience.Levels, ", ")
		}
		return "Everyone in the team"
	}
	switch m.Audience.Mode {
	case AudOne:
		if len(m.Audience.CustomerIDs) == 1 {
			if r := h.S.Customer(m.OrganizationID, m.Audience.CustomerIDs[0]); r != nil {
				return r.Name()
			}
		}
		return "1 customer"
	case AudChosen:
		return fmt.Sprintf("%d customers", len(m.Audience.CustomerIDs))
	case AudGroup:
		switch m.Audience.Group {
		case GroupAllEmail:
			return "Everyone with an email address"
		case GroupAllPhone:
			return "Everyone with a phone number"
		case GroupOverdue:
			return "Customers with overdue invoices"
		case GroupOpenBalance:
			return "Customers with unpaid invoices"
		case GroupSubscriptions:
			return "Customers with active subscriptions"
		case GroupType:
			return "Customer type: " + m.Audience.GroupValue
		case GroupCountry:
			return "Customers in " + strings.ToUpper(m.Audience.GroupValue)
		}
	}
	return ""
}

func bindInput(c *gin.Context) (Input, bool) {
	var in Input
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, bad("invalid request: %s", err.Error()))
		return in, false
	}
	return in, true
}

func (h *Handler) create(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	in, okk := bindInput(c)
	if !okk {
		return
	}
	m, err := h.S.Create(a, in)
	if err != nil {
		fail(c, err)
		return
	}
	h.audit(c, a, "schedsend.create", m.ID, auditValues(m))
	ok(c, gin.H{"message": m, "repeat_text": m.Repeat.Describe(m.LocalSendAt)})
}

func (h *Handler) get(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	d, err := h.S.GetDetail(a, c.Param("id"), c.Query("run"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"detail": d, "recipients_text": h.recipientsText(d.Message)})
}

func (h *Handler) update(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	in, okk := bindInput(c)
	if !okk {
		return
	}
	m, err := h.S.Update(a, c.Param("id"), in)
	if err != nil {
		fail(c, err)
		return
	}
	h.audit(c, a, "schedsend.update", m.ID, auditValues(m))
	ok(c, gin.H{"message": m})
}

func (h *Handler) action(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	id := c.Param("id")
	var m *Message
	var err error
	switch act := c.Param("action"); act {
	case "pause", "resume", "cancel":
		m, err = h.S.SetStatus(a, id, act)
		if err == nil && act == "cancel" {
			h.audit(c, a, "schedsend.cancel", id, auditValues(m))
		}
	case "send-now":
		m, err = h.S.SendNow(a, id)
		if err == nil {
			h.audit(c, a, "schedsend.send_now", id, auditValues(m))
		}
	case "duplicate":
		m, err = h.S.Duplicate(a, id)
		if err == nil {
			h.audit(c, a, "schedsend.create", m.ID, gin.H{"duplicate_of": id, "kind": m.Kind, "title": m.Title})
		}
	default:
		fail(c, &Error{http.StatusNotFound, "NOT_FOUND", "Unknown action"})
		return
	}
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"message": m})
}

func (h *Handler) preview(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	in, okk := bindInput(c)
	if !okk {
		return
	}
	if strings.TrimSpace(in.LocalSendAt) == "" {
		in.LocalSendAt = FormatLocal(Now().Add(24*time.Hour), firstNonEmpty(in.Timezone, h.S.orgTZ(a.Org)))
	}
	m := &Message{ID: "preview", OrganizationID: a.Org, CreatedBy: a.User}
	if err := h.S.Apply(a, m, in, Now()); err != nil {
		fail(c, err)
		return
	}
	if err := h.S.Permitted(a, m); err != nil {
		fail(c, err)
		return
	}
	p, err := h.S.Preview(m, c.Query("pick"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, p)
}

func (h *Handler) badges(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	ids := []string{}
	for _, id := range strings.Split(c.Query("ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) > 500 {
		ids = ids[:500]
	}
	ok(c, gin.H{"badges": h.S.Badges(a.Org, ids)})
}

// staff lists the active users for team messages (managers and above).
func (h *Handler) staff(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	if !a.managerUp() {
		fail(c, forbidden("Team messages can be scheduled by managers and above."))
		return
	}
	people, err := h.S.Staff(a.Org, Audience{Mode: AudStaff})
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]gin.H, 0, len(people))
	for _, p := range people {
		out = append(out, gin.H{"id": p.ID, "name": p.Name(), "email": p.Email, "level": p.Level, "has_phone": strings.TrimSpace(p.Phone) != ""})
	}
	ok(c, gin.H{"people": out})
}

// ---------------------------------------------------------------- opt-out

func (h *Handler) getOptOut(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	var o OptOut
	fresh(h.S.DB).Where("organization_id = ? AND customer_id = ?", a.Org, c.Param("id")).Limit(1).Find(&o)
	ok(c, gin.H{"opted_out": o.CustomerID != "", "since": o.CreatedAt, "source": o.Source})
}

func (h *Handler) putOptOut(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	if a.readOnly() {
		fail(c, forbidden("Your role can't change customers."))
		return
	}
	var in struct {
		OptedOut bool `json:"opted_out"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, bad("invalid request"))
		return
	}
	id := c.Param("id")
	if h.S.Customer(a.Org, id) == nil {
		fail(c, &Error{http.StatusNotFound, "NOT_FOUND", "Customer not found"})
		return
	}
	if in.OptedOut {
		fresh(h.S.DB).Clauses(clause.OnConflict{DoNothing: true}).Create(&OptOut{OrganizationID: a.Org, CustomerID: id, Source: "staff", By: a.User, CreatedAt: Now()})
	} else {
		fresh(h.S.DB).Where("organization_id = ? AND customer_id = ?", a.Org, id).Delete(&OptOut{})
	}
	ok(c, gin.H{"opted_out": in.OptedOut})
}

// ---------------------------------------------------------------- SMS settings

func (h *Handler) smsView(org string) gin.H {
	st := LoadSMSSettings(h.S.DB, org)
	return gin.H{"provider": st.Provider, "at_username": st.ATUsername, "at_sender_id": st.ATSenderID, "at_key_set": st.ATAPIKey != "",
		"last_test_at": st.LastTestAt, "last_test_ok": st.LastTestOK, "last_test_note": st.LastTestNote, "updated_at": st.UpdatedAt,
		"status": StatusOfSMS(h.S.DB, org), "delivery_report_url": h.S.apiBase() + "/api/v1/public/sms/africastalking/delivery"}
}

func (h *Handler) getSMS(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	v := h.smsView(a.Org)
	v["status"] = StatusOfSMS(h.S.DB, a.Org)
	st := v["status"].(SMSStatus)
	v["at_key_hint"] = st.ATKeyHint
	v["can_edit"] = a.Level() == "owner"
	ok(c, v)
}

func (h *Handler) putSMS(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	if a.Level() != "owner" {
		fail(c, forbidden("Only owners and administrators can change SMS settings."))
		return
	}
	var in struct {
		Provider   string  `json:"provider"`
		ATUsername string  `json:"at_username"`
		ATAPIKey   *string `json:"at_api_key"` // nil / "" keeps the saved key
		ATSenderID string  `json:"at_sender_id"`
		ClearKey   bool    `json:"clear_at_api_key"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, bad("invalid request"))
		return
	}
	in.Provider = strings.ToLower(strings.TrimSpace(in.Provider))
	if in.Provider != "" && in.Provider != ProviderAT && in.Provider != ProviderTwilio {
		fail(c, bad("provider must be africastalking or twilio"))
		return
	}
	sender := strings.TrimSpace(in.ATSenderID)
	if len(sender) > 11 && strings.Trim(sender, "+0123456789") != "" {
		fail(c, bad("an alphanumeric sender ID has at most 11 characters"))
		return
	}
	st := LoadSMSSettings(h.S.DB, a.Org)
	st.Provider, st.ATUsername, st.ATSenderID = in.Provider, strings.TrimSpace(in.ATUsername), sender
	if in.ClearKey {
		st.ATAPIKey = ""
	} else if in.ATAPIKey != nil && strings.TrimSpace(*in.ATAPIKey) != "" {
		enc, err := mail.EncryptSecret(strings.TrimSpace(*in.ATAPIKey))
		if err != nil {
			fail(c, err)
			return
		}
		st.ATAPIKey = enc
	}
	st.UpdatedBy, st.UpdatedAt = a.User, Now()
	if err := fresh(h.S.DB).Save(&st).Error; err != nil {
		fail(c, err)
		return
	}
	h.audit(c, a, "settings.sms.update", a.Org, gin.H{"provider": st.Provider, "at_username": st.ATUsername, "at_sender_id": st.ATSenderID, "at_key_changed": in.ATAPIKey != nil && *in.ATAPIKey != ""})
	ok(c, h.smsView(a.Org))
}

func (h *Handler) testSMS(c *gin.Context) {
	a, okk := h.actor(c)
	if !okk {
		return
	}
	if a.Level() != "owner" {
		fail(c, forbidden("Only owners and administrators can send a test SMS."))
		return
	}
	var in struct {
		To string `json:"to"`
	}
	_ = c.ShouldBindJSON(&in)
	to, err := E164(in.To, firstNonEmpty(h.S.orgCountry(a.Org), "KE"))
	if err != nil {
		fail(c, bad("%s", err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	res, err := h.S.sendSMS(ctx, a.Org, to, "Test message from "+h.S.orgName(a.Org)+" — SMS is working.", "test")
	now := Now()
	st := LoadSMSSettings(h.S.DB, a.Org)
	st.LastTestAt = &now
	st.LastTestOK = err == nil
	if err != nil {
		st.LastTestNote = clip(err.Error(), 490)
	} else {
		st.LastTestNote = clip(fmt.Sprintf("Sent to %s with %s (id %s)", to, res.Provider, res.ID), 490)
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = now
	}
	fresh(h.S.DB).Save(&st)
	ok(c, gin.H{"success": err == nil, "message": st.LastTestNote})
}

// ---------------------------------------------------------------- test hooks

// testAdmin: the test hooks are for owners / admins only.
func (h *Handler) testAdmin(c *gin.Context) bool {
	a, okk := h.actor(c)
	if !okk {
		return false
	}
	if a.Level() != "owner" {
		fail(c, forbidden("Owners and administrators only"))
		return false
	}
	return true
}

func (h *Handler) testClock(c *gin.Context) {
	if !h.testAdmin(c) {
		return
	}
	var in struct {
		Advance string `json:"advance"` // e.g. "24h"
		Offset  string `json:"offset"`  // absolute offset
		At      string `json:"at"`      // RFC 3339: move the clock to this instant
		Reset   bool   `json:"reset"`
	}
	_ = c.ShouldBindJSON(&in)
	switch {
	case in.Reset:
		SetClockOffset(0)
	case in.At != "":
		t, err := time.Parse(time.RFC3339, in.At)
		if err != nil {
			fail(c, bad("at must be RFC 3339"))
			return
		}
		SetClockOffset(ClockOffset() + t.Sub(Now()))
	case in.Offset != "":
		d, err := time.ParseDuration(in.Offset)
		if err != nil {
			fail(c, bad("offset like 26h"))
			return
		}
		SetClockOffset(d)
	case in.Advance != "":
		d, err := time.ParseDuration(in.Advance)
		if err != nil {
			fail(c, bad("advance like 24h"))
			return
		}
		SetClockOffset(ClockOffset() + d)
	}
	ok(c, gin.H{"now": Now(), "offset_seconds": int(ClockOffset().Seconds())})
}

func (h *Handler) testRun(c *gin.Context) {
	if !h.testAdmin(c) {
		return
	}
	r, err := h.S.RunOnce(c.Request.Context(), 30*time.Second)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"result": r, "now": Now()})
}
