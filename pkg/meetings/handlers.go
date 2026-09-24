package meetings

import (
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Handler serves the /meetings API.
type Handler struct {
	DB        *gorm.DB
	Mailer    Mailer
	PublicURL string // base URL of the web app, used for in-app join links
	Now       func() time.Time
}

// NewHandler builds a handler with SMTP and PUBLIC_APP_URL from the environment.
func NewHandler(db *gorm.DB) *Handler {
	return &Handler{DB: db, Mailer: SMTPMailerFromEnv(), PublicURL: os.Getenv("PUBLIC_APP_URL"), Now: time.Now}
}

// Migrate creates/updates the meeting tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&Meeting{}, &Participant{}, &Transcript{})
}

// RegisterRoutes mounts the API on an authenticated group.
func (h *Handler) RegisterRoutes(g *gin.RouterGroup) {
	g.GET("", h.list)
	g.POST("", h.create)
	g.GET("/:id", h.get)
	g.PUT("/:id", h.update)
	g.DELETE("/:id", h.remove)
	g.POST("/:id/cancel", h.cancel)
	g.POST("/:id/start", h.start)
	g.POST("/:id/invite", h.invite)
	g.GET("/:id/ics", h.ics)
	g.POST("/:id/transcript", h.saveTranscript)
	g.GET("/:id/transcript", h.getTranscript)
	g.GET("/:id/transcript.txt", h.transcriptTxt)
}

// ---------------------------------------------------------------------------
// helpers

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func ctxString(c *gin.Context, key string) string {
	v, exists := c.Get(key)
	if !exists || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) org(c *gin.Context) (string, bool) {
	org := ctxString(c, "org_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
		return "", false
	}
	return org, true
}

func (h *Handler) baseURL(c *gin.Context) string {
	if h.PublicURL != "" {
		return strings.TrimRight(h.PublicURL, "/")
	}
	if o := c.GetHeader("Origin"); strings.HasPrefix(o, "http") {
		return strings.TrimRight(o, "/")
	}
	if r := c.GetHeader("Referer"); r != "" {
		if u, err := url.Parse(r); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	return "http://localhost:5173"
}

func (h *Handler) load(c *gin.Context, org string) (*Meeting, bool) {
	var m Meeting
	err := h.DB.Preload("Participants", func(db *gorm.DB) *gorm.DB { return db.Order("created_at, name") }).
		Where("id = ? AND org_id = ?", c.Param("id"), org).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Meeting not found")
		return nil, false
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return nil, false
	}
	var count int64
	h.DB.Model(&Transcript{}).Where("meeting_id = ?", m.ID).Count(&count)
	m.HasTranscript = count > 0
	return &m, true
}

type userRow struct {
	FirstName, LastName, Email string
}

func (h *Handler) currentUser(c *gin.Context) (id, name, email string) {
	id = ctxString(c, "user_id")
	email = ctxString(c, "user_email")
	var u userRow
	if id != "" && h.DB.Table("users").Select("first_name, last_name, email").Where("id = ?", id).Limit(1).Scan(&u).Error == nil {
		name = strings.TrimSpace(u.FirstName + " " + u.LastName)
		if u.Email != "" {
			email = u.Email
		}
	}
	if name == "" {
		name = email
	}
	return
}

// ---------------------------------------------------------------------------
// input

type participantInput struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	CustomerID string `json:"customer_id"`
	UserID     string `json:"user_id"`
}

type meetingInput struct {
	Title           *string             `json:"title"`
	Description     *string             `json:"description"`
	StartAt         *string             `json:"start_at"` // RFC3339; or date + start_time in timezone
	EndAt           *string             `json:"end_at"`
	Date            string              `json:"date"`       // YYYY-MM-DD
	StartTime       string              `json:"start_time"` // HH:MM
	DurationMinutes int                 `json:"duration_minutes"`
	Timezone        *string             `json:"timezone"`
	LocationType    *string             `json:"location_type"`
	Location        *string             `json:"location"`
	JoinURL         *string             `json:"join_url"`
	Recurrence      *string             `json:"recurrence"`
	RecurrenceCount *int                `json:"recurrence_count"`
	ReminderMinutes *int                `json:"reminder_minutes"`
	Status          *string             `json:"status"`
	Participants    *[]participantInput `json:"participants"`
	SendInvites     *bool               `json:"send_invites"`
}

type apiErr struct{ msg string }

func (e apiErr) Error() string { return e.msg }

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// apply copies input onto m. Returns whether invite-relevant fields changed.
func (h *Handler) apply(c *gin.Context, m *Meeting, in *meetingInput, creating bool) (bool, error) {
	changed := false
	if in.Title != nil {
		t := str(in.Title)
		if t == "" {
			return false, apiErr{"Title is required"}
		}
		if len(t) > 255 {
			return false, apiErr{"Title is too long"}
		}
		changed = changed || t != m.Title
		m.Title = t
	}
	if creating && m.Title == "" {
		return false, apiErr{"Title is required"}
	}
	if in.Description != nil {
		d := strings.TrimSpace(*in.Description)
		changed = changed || d != m.Description
		m.Description = d
	}
	if in.Timezone != nil {
		tz := str(in.Timezone)
		if tz == "" {
			tz = DefaultTimezone
		}
		if _, err := time.LoadLocation(tz); err != nil {
			return false, apiErr{"Unknown timezone: " + tz}
		}
		changed = changed || tz != m.Timezone
		m.Timezone = tz
	}
	if m.Timezone == "" {
		m.Timezone = DefaultTimezone
	}
	loc, _ := time.LoadLocation(m.Timezone)

	start, end := m.StartAt, m.EndAt
	oldDur := end.Sub(start)
	switch {
	case in.Date != "" && in.StartTime != "":
		t, err := time.ParseInLocation("2006-01-02 15:04", in.Date+" "+in.StartTime, loc)
		if err != nil {
			return false, apiErr{"Invalid date or start time"}
		}
		start = t
	case in.StartAt != nil && str(in.StartAt) != "":
		t, err := time.Parse(time.RFC3339, str(in.StartAt))
		if err != nil {
			return false, apiErr{"start_at must be RFC3339 (e.g. 2026-10-01T10:00:00+10:00)"}
		}
		start = t
	}
	if in.DurationMinutes > 0 {
		if in.DurationMinutes > 24*60 {
			return false, apiErr{"Duration cannot exceed 24 hours"}
		}
		end = start.Add(time.Duration(in.DurationMinutes) * time.Minute)
	} else if in.EndAt != nil && str(in.EndAt) != "" {
		t, err := time.Parse(time.RFC3339, str(in.EndAt))
		if err != nil {
			return false, apiErr{"end_at must be RFC3339"}
		}
		end = t
	} else if !start.Equal(m.StartAt) && oldDur > 0 {
		end = start.Add(oldDur)
	}
	if start.IsZero() {
		return false, apiErr{"Start date and time are required"}
	}
	if end.IsZero() {
		end = start.Add(30 * time.Minute)
	}
	if !end.After(start) {
		return false, apiErr{"End time must be after start time"}
	}
	start, end = start.UTC().Truncate(time.Second), end.UTC().Truncate(time.Second)
	if !start.Equal(m.StartAt) || !end.Equal(m.EndAt) {
		changed = true
	}
	m.StartAt, m.EndAt = start, end

	if in.LocationType != nil {
		lt := str(in.LocationType)
		switch lt {
		case LocationInApp, LocationExternal, LocationPhysical:
		case "":
			lt = LocationInApp
		default:
			return false, apiErr{"location_type must be in_app, external or physical"}
		}
		changed = changed || lt != m.LocationType
		m.LocationType = lt
	}
	if m.LocationType == "" {
		m.LocationType = LocationInApp
	}
	if in.Location != nil {
		l := str(in.Location)
		changed = changed || l != m.Location
		m.Location = l
	}
	switch m.LocationType {
	case LocationInApp:
		j := h.baseURL(c) + "/meetings/" + m.ID
		if m.JoinURL == "" || !strings.HasSuffix(m.JoinURL, "/meetings/"+m.ID) {
			m.JoinURL = j
		}
	default:
		if in.JoinURL != nil {
			j := str(in.JoinURL)
			if j != "" {
				u, err := url.Parse(j)
				if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
					return false, apiErr{"Meeting link must be a valid http(s) URL"}
				}
			}
			changed = changed || j != m.JoinURL
			m.JoinURL = j
		} else if strings.HasSuffix(m.JoinURL, "/meetings/"+m.ID) {
			m.JoinURL = ""
		}
		if m.LocationType == LocationExternal && m.JoinURL == "" {
			return false, apiErr{"Add the external meeting link (Zoom, Teams, Google Meet…)"}
		}
		if m.LocationType == LocationPhysical && m.Location == "" {
			return false, apiErr{"Add the meeting address"}
		}
	}

	if in.Recurrence != nil {
		r := str(in.Recurrence)
		if r == "" {
			r = "none"
		}
		if r != "none" && r != "daily" && r != "weekly" {
			return false, apiErr{"recurrence must be none, daily or weekly"}
		}
		changed = changed || r != m.Recurrence
		m.Recurrence = r
	}
	if m.Recurrence == "" {
		m.Recurrence = "none"
	}
	if in.RecurrenceCount != nil {
		n := *in.RecurrenceCount
		if n < 0 || n > 365 {
			return false, apiErr{"recurrence_count must be between 0 and 365"}
		}
		changed = changed || n != m.RecurrenceCount
		m.RecurrenceCount = n
	}
	if in.ReminderMinutes != nil {
		n := *in.ReminderMinutes
		if n < 0 || n > 7*24*60 {
			return false, apiErr{"Reminder must be between 0 minutes and 7 days"}
		}
		m.ReminderMinutes = n
	}
	if in.Status != nil && !creating {
		s := str(in.Status)
		switch s {
		case StatusScheduled, StatusInProgress, StatusCompleted, StatusCancelled:
			m.Status = s
		default:
			return false, apiErr{"Invalid status"}
		}
	}
	return changed, nil
}

func normalizeParticipants(in []participantInput) ([]participantInput, error) {
	seen := map[string]bool{}
	var out []participantInput
	for _, p := range in {
		email := strings.ToLower(strings.TrimSpace(p.Email))
		name := strings.TrimSpace(p.Name)
		if email == "" {
			if name == "" {
				continue
			}
			return nil, apiErr{"Participant " + name + " has no email address"}
		}
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Address != email {
			return nil, apiErr{"Invalid email address: " + p.Email}
		}
		if seen[email] {
			continue
		}
		seen[email] = true
		if name == "" {
			name = strings.Split(email, "@")[0]
		}
		out = append(out, participantInput{Name: name, Email: email, CustomerID: strings.TrimSpace(p.CustomerID), UserID: strings.TrimSpace(p.UserID)})
	}
	if len(out) > 200 {
		return nil, apiErr{"A meeting can have at most 200 participants"}
	}
	return out, nil
}

// syncParticipants replaces the participant set, keeping response status for
// people who stay. It returns the removed participants.
func (h *Handler) syncParticipants(tx *gorm.DB, m *Meeting, in []participantInput) ([]Participant, bool, error) {
	existing := map[string]Participant{}
	for _, p := range m.Participants {
		existing[strings.ToLower(p.Email)] = p
	}
	var next []Participant
	changed := false
	keep := map[string]bool{}
	for _, p := range in {
		if old, found := existing[p.Email]; found {
			old.Name, old.CustomerID, old.UserID = p.Name, p.CustomerID, p.UserID
			if err := tx.Save(&old).Error; err != nil {
				return nil, false, err
			}
			next = append(next, old)
			keep[p.Email] = true
			continue
		}
		np := Participant{ID: uuid.NewString(), MeetingID: m.ID, OrgID: m.OrgID, Name: p.Name, Email: p.Email, CustomerID: p.CustomerID, UserID: p.UserID, Response: "needs_action"}
		if err := tx.Create(&np).Error; err != nil {
			return nil, false, err
		}
		next = append(next, np)
		changed = true
	}
	var removed []Participant
	for email, p := range existing {
		if !keep[email] {
			removed = append(removed, p)
			if err := tx.Delete(&Participant{}, "id = ?", p.ID).Error; err != nil {
				return nil, false, err
			}
			changed = true
		}
	}
	m.Participants = next
	return removed, changed, nil
}

// ---------------------------------------------------------------------------
// invitations

// InviteResult reports the outcome of sending invitations.
type InviteResult struct {
	Emailed        bool          `json:"emailed"`
	SMTPConfigured bool          `json:"smtp_configured"`
	Method         string        `json:"method"`
	Sent           int           `json:"sent"`
	Recipients     []string      `json:"recipients"`
	Failed         []InviteError `json:"failed"`
	Message        string        `json:"message"`
}

// InviteError is a per-recipient failure.
type InviteError struct {
	Email string `json:"email"`
	Error string `json:"error"`
}

func (h *Handler) fmtWhen(m *Meeting) string {
	loc, err := time.LoadLocation(m.Timezone)
	if err != nil {
		loc = time.UTC
	}
	s, e := m.StartAt.In(loc), m.EndAt.In(loc)
	end := e.Format("3:04 PM")
	if s.YearDay() != e.YearDay() || s.Year() != e.Year() {
		end = e.Format("Mon 2 Jan 2006, 3:04 PM")
	}
	return fmt.Sprintf("%s – %s (%s)", s.Format("Monday 2 January 2006, 3:04 PM"), end, m.Timezone)
}

func (h *Handler) buildInvite(m *Meeting, p Participant, method string, attendees []Participant) InviteMessage {
	when := h.fmtWhen(m)
	subject := "Invitation: " + m.Title + " @ " + when
	verb := "You have been invited to"
	if method == MethodCancel {
		subject = "Cancelled: " + m.Title + " @ " + when
		verb = "This meeting has been cancelled:"
	} else if m.Sequence > 0 {
		subject = "Updated invitation: " + m.Title + " @ " + when
		verb = "This meeting has been updated:"
	}
	var t strings.Builder
	fmt.Fprintf(&t, "Hi %s,\n\n%s %s\n\nWhen: %s\n", p.Name, verb, m.Title, when)
	if m.JoinURL != "" && method != MethodCancel {
		fmt.Fprintf(&t, "Join: %s\n", m.JoinURL)
	}
	if m.Location != "" {
		fmt.Fprintf(&t, "Where: %s\n", m.Location)
	}
	if m.OrganizerName != "" {
		fmt.Fprintf(&t, "Organizer: %s <%s>\n", m.OrganizerName, m.OrganizerEmail)
	}
	if m.Description != "" {
		fmt.Fprintf(&t, "\nAgenda:\n%s\n", m.Description)
	}
	if method != MethodCancel {
		t.WriteString("\nUse the calendar invitation attached to accept or decline.\n")
	}

	e := html.EscapeString
	var hb strings.Builder
	fmt.Fprintf(&hb, `<div style="font-family:Arial,sans-serif;font-size:14px;line-height:1.5"><p>Hi %s,</p><p>%s <strong>%s</strong></p><p><strong>When:</strong> %s<br>`, e(p.Name), e(verb), e(m.Title), e(when))
	if m.Location != "" {
		fmt.Fprintf(&hb, `<strong>Where:</strong> %s<br>`, e(m.Location))
	}
	if m.OrganizerName != "" {
		fmt.Fprintf(&hb, `<strong>Organizer:</strong> %s &lt;%s&gt;`, e(m.OrganizerName), e(m.OrganizerEmail))
	}
	hb.WriteString("</p>")
	if m.JoinURL != "" && method != MethodCancel {
		fmt.Fprintf(&hb, `<p><a href="%s" style="display:inline-block;padding:10px 18px;background:#2563eb;color:#fff;border-radius:6px;text-decoration:none">Join meeting</a></p>`, e(m.JoinURL))
	}
	if m.Description != "" {
		fmt.Fprintf(&hb, `<p><strong>Agenda</strong><br>%s</p>`, strings.ReplaceAll(e(m.Description), "\n", "<br>"))
	}
	hb.WriteString("</div>")

	return InviteMessage{
		To: p.Email, ToName: p.Name, Subject: subject, Text: t.String(), HTML: hb.String(),
		ICS:      BuildICS(m, ICSOptions{Method: method, Attendees: attendees, Now: h.now()}),
		Method:   method,
		FromName: m.OrganizerName,
	}
}

// sendInvites emails the calendar to recipients (all participants when nil).
func (h *Handler) sendInvites(m *Meeting, recipients []Participant, method string, icsAttendees []Participant) InviteResult {
	if recipients == nil {
		recipients = m.Participants
	}
	res := InviteResult{Method: method, Recipients: []string{}, Failed: []InviteError{}}
	for _, p := range recipients {
		res.Recipients = append(res.Recipients, p.Email)
	}
	res.SMTPConfigured = h.Mailer != nil && h.Mailer.Configured()
	if len(recipients) == 0 {
		res.Message = "No participants to invite"
		return res
	}
	if !res.SMTPConfigured {
		res.Message = "Email (SMTP) is not configured — download the .ics file and send it from your email app"
		return res
	}
	now := h.now()
	for _, p := range recipients {
		if err := h.Mailer.SendInvite(h.buildInvite(m, p, method, icsAttendees)); err != nil {
			log.Printf("meetings: invite to %s failed: %v", p.Email, err)
			res.Failed = append(res.Failed, InviteError{Email: p.Email, Error: err.Error()})
			continue
		}
		res.Sent++
		if method != MethodCancel && p.ID != "" {
			h.DB.Model(&Participant{}).Where("id = ?", p.ID).Update("invited_at", now)
		}
	}
	res.Emailed = res.Sent > 0
	if res.Sent > 0 && method != MethodCancel {
		m.InvitesSentAt = &now
		h.DB.Model(&Meeting{}).Where("id = ?", m.ID).Update("invites_sent_at", now)
	}
	switch {
	case res.Sent == len(recipients):
		res.Message = fmt.Sprintf("Emailed %d %s", res.Sent, plural(res.Sent, "person", "people"))
	case res.Sent > 0:
		res.Message = fmt.Sprintf("Emailed %d of %d; %d failed", res.Sent, len(recipients), len(res.Failed))
	default:
		res.Message = "Sending failed: " + res.Failed[0].Error
	}
	return res
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---------------------------------------------------------------------------
// handlers

func (h *Handler) list(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	q := h.DB.Model(&Meeting{}).Where("org_id = ?", org)
	now := h.now().UTC()
	switch c.Query("scope") {
	case "upcoming":
		q = q.Where("status IN ? AND end_at >= ?", []string{StatusScheduled, StatusInProgress}, now)
	case "past":
		q = q.Where("status <> ? AND (end_at < ? OR status = ?)", StatusCancelled, now, StatusCompleted)
	case "cancelled":
		q = q.Where("status = ?", StatusCancelled)
	}
	if s := c.Query("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	if f := c.Query("from"); f != "" {
		if t, err := time.Parse(time.RFC3339, f); err == nil {
			q = q.Where("end_at >= ?", t)
		}
	}
	if t := c.Query("to"); t != "" {
		if tt, err := time.Parse(time.RFC3339, t); err == nil {
			q = q.Where("start_at <= ?", tt)
		}
	}
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		like := "%" + strings.ToLower(s) + "%"
		q = q.Where("LOWER(title) LIKE ? OR LOWER(description) LIKE ? OR id IN (?)", like, like,
			h.DB.Model(&Participant{}).Select("meeting_id").Where("org_id = ? AND (LOWER(name) LIKE ? OR LOWER(email) LIKE ?)", org, like, like))
	}
	var items []Meeting
	if err := q.Preload("Participants", func(db *gorm.DB) *gorm.DB { return db.Order("created_at, name") }).
		Order("start_at ASC").Limit(1000).Find(&items).Error; err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	if items == nil {
		items = []Meeting{}
	}
	ids := make([]string, 0, len(items))
	for _, m := range items {
		ids = append(ids, m.ID)
	}
	if len(ids) > 0 {
		var withT []string
		h.DB.Model(&Transcript{}).Where("meeting_id IN ?", ids).Pluck("meeting_id", &withT)
		has := map[string]bool{}
		for _, id := range withT {
			has[id] = true
		}
		for i := range items {
			items[i].HasTranscript = has[items[i].ID]
		}
	}
	ok(c, http.StatusOK, gin.H{"meetings": items, "smtp_configured": h.Mailer != nil && h.Mailer.Configured()})
}

func (h *Handler) get(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	m, found := h.load(c, org)
	if !found {
		return
	}
	ok(c, http.StatusOK, gin.H{"meeting": m, "smtp_configured": h.Mailer != nil && h.Mailer.Configured()})
}

func (h *Handler) create(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var in meetingInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_INPUT", "Invalid request body")
		return
	}
	uid, uname, uemail := h.currentUser(c)
	m := &Meeting{ID: uuid.NewString(), OrgID: org, Status: StatusScheduled, OrganizerID: uid, OrganizerName: uname, OrganizerEmail: uemail, CreatedBy: uid, ReminderMinutes: 15}
	if in.Timezone == nil {
		tz := DefaultTimezone
		in.Timezone = &tz
	}
	if _, err := h.apply(c, m, &in, true); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	var parts []participantInput
	if in.Participants != nil {
		var err error
		if parts, err = normalizeParticipants(*in.Participants); err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Participants", "Transcript").Create(m).Error; err != nil {
			return err
		}
		_, _, err := h.syncParticipants(tx, m, parts)
		return err
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	resp := gin.H{"meeting": m}
	if in.SendInvites == nil || *in.SendInvites {
		resp["invite"] = h.sendInvites(m, nil, MethodRequest, nil)
	}
	ok(c, http.StatusCreated, resp)
}

func (h *Handler) update(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	m, found := h.load(c, org)
	if !found {
		return
	}
	if m.Status == StatusCancelled {
		fail(c, http.StatusConflict, "CANCELLED", "Cancelled meetings cannot be edited")
		return
	}
	var in meetingInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_INPUT", "Invalid request body")
		return
	}
	changed, err := h.apply(c, m, &in, false)
	if err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	var parts []participantInput
	if in.Participants != nil {
		if parts, err = normalizeParticipants(*in.Participants); err != nil {
			fail(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
	}
	var removed []Participant
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if in.Participants != nil {
			var pc bool
			var e error
			removed, pc, e = h.syncParticipants(tx, m, parts)
			if e != nil {
				return e
			}
			changed = changed || pc
		}
		if changed {
			m.Sequence++
		}
		return tx.Omit("Participants", "Transcript").Save(m).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	resp := gin.H{"meeting": m, "changed": changed}
	send := in.SendInvites == nil || *in.SendInvites
	if send && changed && m.Status != StatusCompleted {
		resp["invite"] = h.sendInvites(m, nil, MethodRequest, nil)
		if len(removed) > 0 {
			for _, r := range removed {
				h.sendInvites(m, []Participant{r}, MethodCancel, []Participant{r})
			}
		}
	}
	ok(c, http.StatusOK, resp)
}

func (h *Handler) remove(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	m, found := h.load(c, org)
	if !found {
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("meeting_id = ?", m.ID).Delete(&Transcript{}).Error; err != nil {
			return err
		}
		if err := tx.Where("meeting_id = ?", m.ID).Delete(&Participant{}).Error; err != nil {
			return err
		}
		return tx.Delete(&Meeting{}, "id = ? AND org_id = ?", m.ID, org).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) cancel(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	m, found := h.load(c, org)
	if !found {
		return
	}
	if m.Status == StatusCancelled {
		fail(c, http.StatusConflict, "ALREADY_CANCELLED", "Meeting is already cancelled")
		return
	}
	var body struct {
		Notify *bool `json:"notify"`
	}
	_ = c.ShouldBindJSON(&body)
	now := h.now()
	m.Status = StatusCancelled
	m.CancelledAt = &now
	m.Sequence++
	if err := h.DB.Omit("Participants", "Transcript").Save(m).Error; err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	resp := gin.H{"meeting": m}
	if body.Notify == nil || *body.Notify {
		resp["invite"] = h.sendInvites(m, nil, MethodCancel, nil)
	}
	ok(c, http.StatusOK, resp)
}

func (h *Handler) start(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	m, found := h.load(c, org)
	if !found {
		return
	}
	if m.Status == StatusScheduled {
		m.Status = StatusInProgress
		h.DB.Model(&Meeting{}).Where("id = ?", m.ID).Update("status", StatusInProgress)
	}
	ok(c, http.StatusOK, gin.H{"meeting": m})
}

func (h *Handler) invite(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	m, found := h.load(c, org)
	if !found {
		return
	}
	if m.Status == StatusCancelled {
		fail(c, http.StatusConflict, "CANCELLED", "Meeting is cancelled")
		return
	}
	var body struct {
		Emails []string `json:"emails"`
	}
	_ = c.ShouldBindJSON(&body)
	var recipients []Participant
	if len(body.Emails) > 0 {
		want := map[string]bool{}
		for _, e := range body.Emails {
			want[strings.ToLower(strings.TrimSpace(e))] = true
		}
		recipients = []Participant{}
		for _, p := range m.Participants {
			if want[strings.ToLower(p.Email)] {
				recipients = append(recipients, p)
			}
		}
	}
	ok(c, http.StatusOK, h.sendInvites(m, recipients, MethodRequest, nil))
}

func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		out = "meeting"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

func (h *Handler) ics(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	m, found := h.load(c, org)
	if !found {
		return
	}
	method := MethodRequest
	if m.Status == StatusCancelled {
		method = MethodCancel
	}
	if strings.EqualFold(c.Query("method"), "publish") {
		method = MethodPublish
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.ics"`, safeFilename(m.Title)))
	c.Data(http.StatusOK, "text/calendar; charset=utf-8; method="+method, []byte(BuildICS(m, ICSOptions{Method: method, Now: h.now()})))
}

func (h *Handler) saveTranscript(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	m, found := h.load(c, org)
	if !found {
		return
	}
	var body struct {
		Text     string `json:"text"`
		Notes    string `json:"notes"`
		Language string `json:"language"`
		Complete *bool  `json:"complete"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_INPUT", "Invalid request body")
		return
	}
	text := strings.TrimSpace(body.Text)
	notes := strings.TrimSpace(body.Notes)
	if text == "" && notes == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Transcript or notes are required")
		return
	}
	if len(text)+len(notes) > 2<<20 {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Transcript is too large (2 MB max)")
		return
	}
	source := text
	if notes != "" {
		if source != "" {
			source += "\n"
		}
		source += notes
	}
	sum := Summarize(c.Request.Context(), m.Title, source)

	uid, uname, _ := h.currentUser(c)
	var t Transcript
	err := h.DB.Where("meeting_id = ?", m.ID).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		t = Transcript{ID: uuid.NewString(), MeetingID: m.ID, OrgID: org, CreatedBy: uid, CreatedByName: uname}
	} else if err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	t.Text, t.Notes, t.Language = text, notes, strings.TrimSpace(body.Language)
	t.Summary, t.KeyDecisions, t.ActionItems, t.SummarySource = sum.Summary, sum.KeyDecisions, sum.ActionItems, sum.Source
	if err := h.DB.Save(&t).Error; err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	if (body.Complete == nil || *body.Complete) && m.Status != StatusCancelled {
		m.Status = StatusCompleted
		h.DB.Model(&Meeting{}).Where("id = ?", m.ID).Update("status", StatusCompleted)
	}
	ok(c, http.StatusOK, gin.H{"transcript": t, "meeting": m})
}

func (h *Handler) getTranscript(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	var t Transcript
	err := h.DB.Where("meeting_id = ? AND org_id = ?", c.Param("id"), org).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "No transcript saved for this meeting yet")
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"transcript": t})
}

// TranscriptText renders the plain-text transcript download.
func TranscriptText(m *Meeting, t *Transcript) string {
	var b strings.Builder
	rule := strings.Repeat("=", 72)
	loc, err := time.LoadLocation(m.Timezone)
	if err != nil {
		loc = time.UTC
	}
	b.WriteString(rule + "\n")
	b.WriteString("MEETING TRANSCRIPT\n")
	b.WriteString(rule + "\n")
	fmt.Fprintf(&b, "Title:        %s\n", m.Title)
	fmt.Fprintf(&b, "Date:         %s\n", m.StartAt.In(loc).Format("Monday 2 January 2006"))
	fmt.Fprintf(&b, "Time:         %s – %s (%s)\n", m.StartAt.In(loc).Format("3:04 PM"), m.EndAt.In(loc).Format("3:04 PM"), m.Timezone)
	if m.OrganizerName != "" || m.OrganizerEmail != "" {
		fmt.Fprintf(&b, "Organizer:    %s <%s>\n", m.OrganizerName, m.OrganizerEmail)
	}
	if m.JoinURL != "" {
		fmt.Fprintf(&b, "Link:         %s\n", m.JoinURL)
	}
	if m.Location != "" {
		fmt.Fprintf(&b, "Location:     %s\n", m.Location)
	}
	b.WriteString("Participants:")
	if len(m.Participants) == 0 {
		b.WriteString(" (none)\n")
	} else {
		b.WriteString("\n")
		ps := append([]Participant(nil), m.Participants...)
		sort.SliceStable(ps, func(i, j int) bool { return strings.ToLower(ps[i].Name) < strings.ToLower(ps[j].Name) })
		for _, p := range ps {
			fmt.Fprintf(&b, "  - %s <%s>\n", p.Name, p.Email)
		}
	}
	if t.CreatedByName != "" {
		fmt.Fprintf(&b, "Recorded by:  %s\n", t.CreatedByName)
	}
	fmt.Fprintf(&b, "Saved:        %s\n", t.UpdatedAt.In(loc).Format("2 Jan 2006 3:04 PM MST"))

	section := func(name string) {
		b.WriteString("\n" + rule + "\n" + name + "\n" + rule + "\n")
	}
	section("TRANSCRIPT")
	if t.Text != "" {
		b.WriteString(strings.TrimRight(t.Text, "\n") + "\n")
	} else {
		b.WriteString("(no transcript recorded)\n")
	}
	if t.Notes != "" {
		section("NOTES")
		b.WriteString(strings.TrimRight(t.Notes, "\n") + "\n")
	}
	section("SUMMARY")
	b.WriteString(strings.TrimSpace(t.Summary) + "\n")
	section("KEY DECISIONS")
	if len(t.KeyDecisions) == 0 {
		b.WriteString("None recorded.\n")
	}
	for i, d := range t.KeyDecisions {
		fmt.Fprintf(&b, "%d. %s\n", i+1, d)
	}
	section("ACTION ITEMS")
	if len(t.ActionItems) == 0 {
		b.WriteString("None recorded.\n")
	}
	for i, a := range t.ActionItems {
		line := fmt.Sprintf("%d. [ ] %s", i+1, a.Text)
		if a.Owner != "" {
			line += " — Owner: " + a.Owner
		}
		if a.Due != "" {
			line += " — Due: " + a.Due
		}
		b.WriteString(line + "\n")
	}
	src := "local extractive summariser"
	if t.SummarySource == "anthropic" {
		src = "Claude (Anthropic)"
	}
	b.WriteString("\n-- Summary generated by " + src + " --\n")
	return strings.ReplaceAll(b.String(), "\n", "\r\n")
}

func (h *Handler) transcriptTxt(c *gin.Context) {
	org, okk := h.org(c)
	if !okk {
		return
	}
	m, found := h.load(c, org)
	if !found {
		return
	}
	var t Transcript
	err := h.DB.Where("meeting_id = ? AND org_id = ?", m.ID, org).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "No transcript saved for this meeting yet")
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", err.Error())
		return
	}
	name := fmt.Sprintf("%s-%s-transcript.txt", safeFilename(m.Title), m.StartAt.Format("2006-01-02"))
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(TranscriptText(m, &t)))
}
