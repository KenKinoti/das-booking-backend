package barstock

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// The night report: an optional email after the day cutoff to the managers
// (users with approve on inventory): sales by category, pour cost, wastage,
// comps, the variance of the last count, low stock at bars and pending
// transfers / approvals. Off by default per organisation; the scheduler runs
// every 10 minutes under a Postgres advisory lock per organisation, so with
// several instances exactly one sends. BAR_SCHEDULER=off disables it.

// Recipients are the emails of active users who may approve on inventory,
// as far as Who gets emails (pkg/emailprefs) lets them have it.
func (s *Service) Recipients(db *gorm.DB, org string) []string {
	type u struct{ ID, Email, Role string }
	var users []u
	db.Table("users").Select("id, email, role").Where("organization_id = ? AND deleted_at IS NULL AND is_active", org).Order("email").Scan(&users)
	ids := []string{}
	for _, x := range users {
		if strings.TrimSpace(x.Email) == "" {
			continue
		}
		ok := legacyCan(x.Role, "approve")
		if s.Can != nil {
			ok = s.Can(org, x.ID, x.Role, "inventory", "approve")
		}
		if ok {
			ids = append(ids, x.ID)
		}
	}
	return emailprefs.UserEmails(db, org, emailprefs.BarNightReport, ids)
}

type orgInfo struct{ Name, Email, Phone, Address, Accent, Logo string }

func (s *Service) orgInfo(db *gorm.DB, org string) orgInfo {
	var o struct {
		Name, Email, Phone, AddressStreet, AddressSuburb, AddressState, AddressPostcode string
	}
	db.Raw(`SELECT name, COALESCE(email,'') AS email, COALESCE(phone,'') AS phone, COALESCE(address_street,'') AS address_street,
		COALESCE(address_suburb,'') AS address_suburb, COALESCE(address_state,'') AS address_state, COALESCE(address_postcode,'') AS address_postcode
		FROM organizations WHERE id = ?`, org).Scan(&o)
	parts := []string{}
	for _, p := range []string{o.AddressStreet, strings.TrimSpace(o.AddressSuburb + " " + o.AddressState + " " + o.AddressPostcode)} {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, strings.TrimSpace(p))
		}
	}
	info := orgInfo{Name: o.Name, Email: o.Email, Phone: o.Phone, Address: strings.Join(parts, ", ")}
	if info.Name == "" {
		info.Name = "DASYIN ERP"
	}
	var ok bool
	if db.Raw("SELECT to_regclass('billing_settings') IS NOT NULL").Row().Scan(&ok); ok {
		var accent []string
		db.Raw(`SELECT accent_color FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&accent)
		if len(accent) > 0 {
			info.Accent = accent[0]
		}
	}
	if s.LogoURL != nil {
		info.Logo = s.LogoURL(org)
	}
	return info
}

func (s *Service) appLink(path string) string {
	base := strings.TrimRight(strings.TrimSpace(s.AppURL), "/")
	if base == "" {
		return ""
	}
	return base + path
}

func pctText(p *float64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", *p)
}

// NightMessage renders the night report of a business date.
func (s *Service) NightMessage(db *gorm.DB, org, day string, to []string) (mail.Message, gin.H, error) {
	ov, err := s.Overview(db, org, day, nil)
	if err != nil {
		return mail.Message{}, nil, err
	}
	cur := ov["currency"].(string)
	money := func(v any) string {
		f, _ := v.(float64)
		return invoicing.FormatMoney(f, cur)
	}
	info := s.orgInfo(db, org)
	d, _ := time.Parse("2006-01-02", day)
	dayLabel := d.Format("Mon 2 Jan 2006")
	subject := fmt.Sprintf("Night report — %s — %s", info.Name, dayLabel)
	sales := ov["sales"].(gin.H)
	pour, _ := ov["pour_cost_pct"].(*float64)
	wastage := ov["wastage"].(gin.H)
	comps := ov["comps"].(gin.H)
	low := ov["low_stock_at_bars"].([]gin.H)
	pt := ov["pending_transfers"].(gin.H)
	pa := ov["pending_approvals"].(gin.H)

	facts := []invoicing.EmailRow{
		{Label: "Pour cost", Value: pctText(pour), Strong: true},
		{Label: "Cost of sales", Value: money(ov["cost_of_sales"])},
		{Label: "All POS takings", Value: money(ov["pos_sales_total"])},
		{Label: "Wastage", Value: fmt.Sprintf("%v entries · %s", wastage["entries"], money(wastage["value"]))},
		{Label: "Comps & staff drinks", Value: fmt.Sprintf("%v · %v drinks · %s", comps["entries"], comps["servings"], money(comps["value"]))},
		{Label: "Low stock at bars", Value: fmt.Sprint(len(low))},
		{Label: "Pending transfers", Value: fmt.Sprint(pt["total"])},
		{Label: "Waiting for approval", Value: fmt.Sprintf("%v wastage · %v counts", pa["wastage"], pa["counts"])},
	}
	if lc, ok := ov["last_count_variance"].(gin.H); ok {
		facts = append(facts, invoicing.EmailRow{Label: "Last count", Value: fmt.Sprintf("%v %v (%v): %s, %v flagged", lc["number"], lc["location"], lc["business_date"],
			money(lc["variance_value"]), lc["flagged_count"])})
	}
	lines := []invoicing.EmailLine{}
	for _, c := range sales["categories"].([]SalesCategory) {
		detail := fmt.Sprintf("%d sold · pour cost %s", c.Qty, pctText(c.PourCost))
		if c.Target != nil {
			detail += fmt.Sprintf(" (target %.1f%%)", *c.Target)
		}
		lines = append(lines, invoicing.EmailLine{Description: nz(c.Label, c.Category), Detail: detail, Amount: invoicing.FormatMoney(c.Revenue, cur)})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Here is how the night of %s went.\n", dayLabel)
	if len(low) > 0 {
		b.WriteString("\nLow stock at the bars:\n")
		for i, l := range low {
			if i == 10 {
				fmt.Fprintf(&b, "• … and %d more\n", len(low)-10)
				break
			}
			fmt.Fprintf(&b, "• %s — %s at %s\n", l["name"], l["on_hand_text"], l["location"])
		}
	}
	tone, status := "ok", "Pour cost "+pctText(pour)
	if f, ok := pa["total"].(int64); ok && f > 0 {
		tone, status = "warn", fmt.Sprintf("%d waiting for your approval", f)
	}
	actions := []invoicing.EmailAction{}
	if u := s.appLink("/bar-stock?tab=overview"); u != "" {
		actions = append(actions, invoicing.EmailAction{Label: "Open bar stock", URL: u, Primary: true})
	}
	v := invoicing.EmailView{Kind: "reminder", Business: invoicing.EmailBusiness{Name: info.Name, Address: info.Address, Email: info.Email, Phone: info.Phone},
		LogoURL: info.Logo, Accent: info.Accent, Preheader: subject, DocLabel: "Night report", Number: dayLabel, Message: b.String(),
		AmountLabel: "Bar sales (ex VAT)", Amount: money(sales["revenue"]), StatusLine: status, StatusTone: tone, Facts: facts, Lines: lines,
		Actions: actions, Note: "Sent after the day cutoff to the bar managers of " + info.Name + ". Switch it off in Bar stock → Settings."}
	htmlOut, text := invoicing.RenderEmail(v)
	return mail.Message{To: to, Subject: subject, HTML: htmlOut, Text: text}, ov, nil
}

func nz(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// SendNight sends the night report of day (to the recipients, or to).
func (s *Service) SendNight(ctx context.Context, org, day string, to []string) ([]string, string, error) {
	if s.Mail == nil {
		return nil, "", errf(http.StatusServiceUnavailable, "Email is not configured")
	}
	if len(to) == 0 {
		to = s.Recipients(s.DB, org)
	}
	if len(to) == 0 {
		return nil, "", badReq("No one can approve bar stock in this organisation, so there is no one to send the night report to")
	}
	msg, _, err := s.NightMessage(s.DB, org, day, to)
	if err != nil {
		return nil, "", err
	}
	if _, err := s.Mail.SendAs(ctx, org, msg); err != nil {
		return to, msg.Subject, errf(http.StatusBadGateway, "Could not send the night report: %s", err.Error())
	}
	return to, msg.Subject, nil
}

// dueNightDay is the business date whose report is due now ("" = none).
func (s *Service) dueNightDay(db *gorm.DB, org string, st Settings) string {
	loc := s.Location(db, org)
	now := s.now().In(loc)
	sh, sm, ok := ParseHM(st.NightReportSendAt)
	if !ok {
		sh, sm = 7, 0
	}
	ch, cm, _ := ParseHM(st.DayCutoff)
	mins := now.Hour()*60 + now.Minute()
	if mins < sh*60+sm || mins < ch*60+cm {
		return ""
	}
	return AddDays(BusinessDate(now, loc, st.DayCutoff), -1)
}

// RunNightReports sends every due night report once; it returns how many were sent.
func (s *Service) RunNightReports(ctx context.Context) int {
	if !tablesReady(s.DB) {
		return 0
	}
	var orgs []string
	s.DB.Model(&Settings{}).Where("night_report_enabled").Pluck("organization_id", &orgs)
	n := 0
	for _, org := range orgs {
		if ctx.Err() != nil {
			return n
		}
		st := s.Settings(s.DB, org)
		day := s.dueNightDay(s.DB, org, st)
		if day == "" {
			continue
		}
		var done int64
		s.DB.Model(&NightReport{}).Where("organization_id = ? AND business_date = ?", org, day).Count(&done)
		if done > 0 {
			continue
		}
		ok := s.withLock(ctx, "barstock:night:"+org, func() {
			var again int64
			s.DB.Model(&NightReport{}).Where("organization_id = ? AND business_date = ?", org, day).Count(&again)
			if again > 0 {
				return
			}
			to, _, err := s.SendNight(ctx, org, day, nil)
			row := NightReport{OrganizationID: org, BusinessDate: day, SentAt: s.now(), Recipients: strings.Join(to, ", ")}
			if err != nil {
				row.Error = err.Error()
				log.Printf("barstock: night report for %s (%s): %v", org, day, err)
			} else {
				n++
			}
			s.DB.Create(&row)
		})
		if !ok {
			log.Printf("barstock: night report for %s is running on another instance", org)
		}
	}
	return n
}

func (s *Service) withLock(ctx context.Context, key string, fn func()) bool {
	got := false
	err := s.DB.WithContext(ctx).Connection(func(tx *gorm.DB) error {
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
	if err != nil {
		log.Printf("barstock: lock %s: %v", key, err)
		return false
	}
	return got
}

// StartScheduler checks for due night reports every interval.
func (s *Service) StartScheduler(ctx context.Context, every, firstDelay time.Duration) {
	if every <= 0 {
		every = 10 * time.Minute
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("barstock: scheduler stopped: %v", r)
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(firstDelay):
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			s.RunNightReports(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (h *Handler) nightPreview(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Approve, "see the night report"); err != nil {
		h.fail(c, err)
		return
	}
	day := c.Query("date")
	if !ValidDay(day) {
		day = AddDays(h.S.Today(h.S.DB, a.Org), -1)
	}
	msg, ov, err := h.S.NightMessage(h.S.DB, a.Org, day, h.S.Recipients(h.S.DB, a.Org))
	if err != nil {
		h.fail(c, err)
		return
	}
	out := gin.H{"business_date": day, "subject": msg.Subject, "recipients": msg.To, "data": ov}
	if qBool(c, "html") {
		out["html"], out["text"] = msg.HTML, msg.Text
	}
	h.ok(c, http.StatusOK, a, out)
}

func (h *Handler) nightSend(c *gin.Context) {
	a := h.access(c)
	if err := a.need(a.Approve, "send the night report"); err != nil {
		h.fail(c, err)
		return
	}
	var in struct {
		Date string   `json:"date"`
		To   []string `json:"to"`
	}
	if c.Request.ContentLength > 0 {
		if err := bind(c, &in); err != nil {
			h.fail(c, err)
			return
		}
	}
	day := in.Date
	if !ValidDay(day) {
		day = AddDays(h.S.Today(h.S.DB, a.Org), -1)
	}
	to := []string{}
	if len(in.To) > 0 {
		var emails []string
		h.S.DB.Table("users").Where("organization_id = ? AND deleted_at IS NULL", a.Org).Pluck("LOWER(email)", &emails)
		known := map[string]bool{}
		for _, e := range emails {
			known[e] = true
		}
		for _, e := range in.To {
			e = strings.ToLower(strings.TrimSpace(e))
			if !known[e] {
				h.fail(c, badReq("%s is not a user of this organisation", e))
				return
			}
			to = append(to, e)
		}
	}
	sent, subject, err := h.S.SendNight(c.Request.Context(), a.Org, day, to)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.ok(c, http.StatusOK, a, gin.H{"business_date": day, "subject": subject, "sent_to": sent})
}
