package bookingmail

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// ---------------------------------------------------------------- daily agenda

// AgendaResult reports one agenda run.
type AgendaResult struct {
	Sent    int      `json:"sent"`
	Skipped int      `json:"skipped"`
	Failed  int      `json:"failed"`
	To      []string `json:"to"`
}

// SendAgenda emails each staff member today's bookings (once per person per
// day unless force).
func (n *Notifier) SendAgenda(ctx context.Context, org string, force bool) AgendaResult {
	res := AgendaResult{To: []string{}}
	loc := n.Loc(org)
	local := n.now().In(loc)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)
	var bookings []models.Booking
	n.DB.Preload("Customer").Preload("Vehicle").Preload("Services").
		Where("organization_id = ? AND staff_id IS NOT NULL AND staff_id <> '' AND start_time >= ? AND start_time < ? AND status NOT IN ?",
			org, dayStart, dayEnd, []string{"cancelled", "no_show"}).Order("start_time").Find(&bookings)
	by := map[string][]models.Booking{}
	order := []string{}
	for _, b := range bookings {
		id := *b.StaffID
		if _, ok := by[id]; !ok {
			order = append(order, id)
		}
		by[id] = append(by[id], b)
	}
	date := dayStart.Format("2006-01-02")
	for _, uid := range order {
		u := n.user(uid)
		if u == nil || u.OrganizationID != org || !n.WantsEmails(uid) || !mail.ValidAddress(strings.ToLower(u.Email)) {
			res.Skipped++
			continue
		}
		key := "agenda|" + uid + "|" + date
		if force {
			key += "|" + n.now().UTC().Format(time.RFC3339Nano)
		}
		var have int64
		n.DB.Model(&Log{}).Where("organization_id = ? AND key = ?", org, key).Count(&have)
		if have > 0 {
			continue
		}
		msg := n.agendaMessage(org, u, by[uid], dayStart)
		row := n.logRow(Log{OrganizationID: org, Key: key, UserID: uid, Email: strings.ToLower(u.Email), Event: "agenda", Subject: clip(msg.Subject, 480), Status: "queued", Attempts: 1})
		_, err := n.Mail.SendAs(ctx, org, msg)
		upd := map[string]any{"status": "sent", "updated_at": n.now()}
		if err != nil {
			upd["status"], upd["error"] = "failed", clip(err.Error(), 1000)
			res.Failed++
		} else {
			res.Sent++
			res.To = append(res.To, strings.ToLower(u.Email))
		}
		n.DB.Model(&Log{}).Where("id = ?", row.ID).Updates(upd)
	}
	return res
}

func (n *Notifier) agendaMessage(org string, u *staffUser, list []models.Booking, day time.Time) mail.Message {
	loc := n.Loc(org)
	o, addr := n.org(org)
	var b strings.Builder
	first := nzs(strings.TrimSpace(u.FirstName), "there")
	b.WriteString(fmt.Sprintf("Good morning %s,\n\nYou have %d booking%s today:\n\n", first, len(list), map[bool]string{true: "", false: "s"}[len(list) == 1]))
	for _, bk := range list {
		bk := bk
		line := fmt.Sprintf("• %s–%s  %s", bk.StartTime.In(loc).Format("15:04"), bk.EndTime.In(loc).Format("15:04"), CustomerName(&bk))
		if s := strings.Join(ServiceNames(&bk), ", "); s != "" {
			line += " — " + s
		}
		if ph := strings.TrimSpace(bk.Customer.Phone); ph != "" {
			line += " · " + ph
		}
		if v := VehicleText(&bk); v != "" {
			line += " · " + v
		}
		if l := n.link("/bookings?booking=" + bk.ID); l != "" {
			line += "\n  " + l
		}
		b.WriteString(line + "\n")
	}
	subject := fmt.Sprintf("Your bookings today (%s): %d", day.Format("Mon 2 Jan"), len(list))
	logo := ""
	if n.LogoURL != nil {
		logo = n.LogoURL(org)
	}
	actions := []invoicing.EmailAction{}
	if l := n.link("/bookings"); l != "" {
		actions = append(actions, invoicing.EmailAction{Label: "Open bookings", URL: l, Primary: true})
	}
	v := invoicing.EmailView{Kind: "booking", Business: invoicing.EmailBusiness{Name: nzs(o.Name, "DASYIN ERP"), Address: addr, Email: o.Email, Phone: o.Phone},
		LogoURL: logo, Preheader: subject, DocLabel: "Today's agenda", Number: day.Format("Mon 2 Jan"), Message: b.String(), Actions: actions,
		Note: "Daily agenda — turn booking emails off in My profile → Booking emails."}
	htmlOut, text := invoicing.RenderEmail(v)
	return mail.Message{To: []string{u.Email}, Subject: subject, HTML: htmlOut, Text: text}
}

// RunAgendas sends due daily agendas (07:00 org time by default) once a day.
func (n *Notifier) RunAgendas(ctx context.Context) {
	var list []Settings
	n.DB.Where("daily_agenda AND staff_emails").Find(&list)
	for _, st := range list {
		loc := n.Loc(st.OrganizationID)
		local := n.now().In(loc)
		today := local.Format("2006-01-02")
		if local.Hour() < st.AgendaHour || st.LastAgendaDate == today {
			continue
		}
		n.withLock(ctx, "bookingmail-agenda:"+st.OrganizationID, func() {
			var fresh Settings
			if n.DB.Where("organization_id = ?", st.OrganizationID).First(&fresh).Error != nil || fresh.LastAgendaDate == today {
				return
			}
			r := n.SendAgenda(ctx, st.OrganizationID, false)
			n.DB.Model(&Settings{}).Where("organization_id = ?", st.OrganizationID).Update("last_agenda_date", today)
			if r.Sent+r.Failed > 0 {
				log.Printf("bookingmail: agenda %s — %d sent, %d failed", st.OrganizationID, r.Sent, r.Failed)
			}
		})
	}
}

func (n *Notifier) withLock(ctx context.Context, key string, fn func()) bool {
	got := false
	err := n.DB.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", key).Scan(&got).Error; err != nil {
			return err
		}
		if !got {
			return nil
		}
		defer tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", key)
		fn()
		return nil
	})
	return err == nil && got
}

// StartScheduler checks for due agendas every 10 minutes.
func (n *Notifier) StartScheduler(ctx context.Context) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("bookingmail: scheduler stopped: %v", r)
			}
		}()
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
		}
		for {
			n.RunAgendas(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// ---------------------------------------------------------------- HTTP

// Handler serves /api/v1/booking-emails/….
type Handler struct{ N *Notifier }

// RegisterRoutes mounts the routes.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/booking-emails")
	g.GET("/settings", h.getSettings)
	g.PUT("/settings", h.saveSettings)
	g.GET("/me", h.getMe)
	g.PUT("/me", h.saveMe)
	g.GET("/log", h.log)
	g.POST("/agenda/send", h.agendaNow)
}

func orgOf(c *gin.Context) string {
	if v := c.GetString("organization_id"); v != "" {
		return v
	}
	if v, ok := c.Get("org_id"); ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func isAdmin(role string) bool {
	switch role {
	case "owner", "admin", "org_admin", "super_admin", "manager":
		return true
	}
	return false
}

func okJSON(c *gin.Context, data any) { c.JSON(http.StatusOK, gin.H{"success": true, "data": data}) }

func bad(c *gin.Context, code int, msg string) {
	c.JSON(code, gin.H{"success": false, "error": msg})
}

func (h *Handler) getSettings(c *gin.Context) {
	org := orgOf(c)
	if org == "" {
		bad(c, http.StatusForbidden, "No organisation")
		return
	}
	okJSON(c, gin.H{"settings": h.N.SettingsFor(org), "timezone": h.N.Loc(org).String()})
}

func (h *Handler) saveSettings(c *gin.Context) {
	org := orgOf(c)
	if org == "" || !isAdmin(c.GetString("user_role")) {
		bad(c, http.StatusForbidden, "Only admins and managers can change booking email settings")
		return
	}
	var in struct {
		StaffEmails *bool `json:"staff_emails"`
		DailyAgenda *bool `json:"daily_agenda"`
		AgendaHour  *int  `json:"agenda_hour"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		bad(c, http.StatusBadRequest, "Invalid request")
		return
	}
	st := h.N.SettingsFor(org)
	if in.StaffEmails != nil {
		st.StaffEmails = *in.StaffEmails
	}
	if in.DailyAgenda != nil {
		st.DailyAgenda = *in.DailyAgenda
	}
	if in.AgendaHour != nil {
		if *in.AgendaHour < 0 || *in.AgendaHour > 23 {
			bad(c, http.StatusBadRequest, "Choose an hour between 0 and 23")
			return
		}
		st.AgendaHour = *in.AgendaHour
	}
	st.OrganizationID, st.UpdatedAt = org, time.Now()
	if err := h.N.DB.Save(&st).Error; err != nil {
		bad(c, http.StatusInternalServerError, "Could not save")
		return
	}
	okJSON(c, gin.H{"settings": st})
}

func (h *Handler) getMe(c *gin.Context) {
	uid := c.GetString("user_id")
	if uid == "" {
		bad(c, http.StatusUnauthorized, "Not signed in")
		return
	}
	okJSON(c, gin.H{"booking_emails": h.N.WantsEmails(uid), "org_staff_emails": h.N.SettingsFor(orgOf(c)).StaffEmails})
}

func (h *Handler) saveMe(c *gin.Context) {
	uid := c.GetString("user_id")
	if uid == "" {
		bad(c, http.StatusUnauthorized, "Not signed in")
		return
	}
	var in struct {
		BookingEmails *bool `json:"booking_emails"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.BookingEmails == nil {
		bad(c, http.StatusBadRequest, "Choose on or off")
		return
	}
	p := UserPref{UserID: uid, OrganizationID: orgOf(c), BookingEmails: *in.BookingEmails, UpdatedAt: time.Now()}
	if err := h.N.DB.Save(&p).Error; err != nil {
		bad(c, http.StatusInternalServerError, "Could not save")
		return
	}
	okJSON(c, gin.H{"booking_emails": p.BookingEmails})
}

func (h *Handler) log(c *gin.Context) {
	org := orgOf(c)
	if org == "" {
		bad(c, http.StatusForbidden, "No organisation")
		return
	}
	q := h.N.DB.Where("organization_id = ?", org)
	if b := c.Query("booking_id"); b != "" {
		q = q.Where("booking_id = ?", b)
	} else if !isAdmin(c.GetString("user_role")) {
		q = q.Where("user_id = ?", c.GetString("user_id"))
	}
	out := []Log{}
	q.Order("created_at DESC").Limit(200).Find(&out)
	okJSON(c, out)
}

func (h *Handler) agendaNow(c *gin.Context) {
	org := orgOf(c)
	if org == "" || !isAdmin(c.GetString("user_role")) {
		bad(c, http.StatusForbidden, "Only admins and managers can send the agenda")
		return
	}
	okJSON(c, h.N.SendAgenda(c.Request.Context(), org, true))
}
