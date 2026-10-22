package broadcast

import (
	"bytes"
	"encoding/csv"
	"math"
	"time"
)

// Stats of an announcement (all occurrences, test sends left out).
type Stats struct {
	Recipients   int64   `json:"recipients"`
	Delivered    int64   `json:"delivered"`
	Read         int64   `json:"read"`
	ReadPct      float64 `json:"read_pct"`
	Clicked      int64   `json:"clicked"`
	ClickPct     float64 `json:"click_pct"`
	Dismissed    int64   `json:"dismissed"`
	Emailed      int64   `json:"emailed"`
	EmailFailed  int64   `json:"email_failed"`
	EmailSkipped int64   `json:"email_skipped"`
	EmailPending int64   `json:"email_pending"`
	Unsubscribed int64   `json:"unsubscribed"`
}

func pct(n, of int64) float64 {
	if of == 0 {
		return 0
	}
	return math.Round(float64(n)*1000/float64(of)) / 10
}

// StatsFor returns the stats of several announcements.
func (s *Service) StatsFor(ids []string) map[string]Stats {
	out := map[string]Stats{}
	if len(ids) == 0 {
		return out
	}
	s.syncReads(readScope{announcements: ids})
	var rows []struct {
		AnnouncementID                                                                                    string
		Recipients, Delivered, Read, Clicked, Dismissed, Emailed, EmailFailed, EmailSkipped, EmailPending int64
		Unsubscribed                                                                                      int64
	}
	fresh(s.DB).Raw(`SELECT announcement_id, COUNT(*) AS recipients,
		COUNT(*) FILTER (WHERE delivered_at IS NOT NULL) AS delivered,
		COUNT(*) FILTER (WHERE read_at IS NOT NULL) AS read,
		COUNT(*) FILTER (WHERE clicked_at IS NOT NULL) AS clicked,
		COUNT(*) FILTER (WHERE dismissed_at IS NOT NULL) AS dismissed,
		COUNT(*) FILTER (WHERE email_status = 'sent') AS emailed,
		COUNT(*) FILTER (WHERE email_status = 'failed') AS email_failed,
		COUNT(*) FILTER (WHERE email_status = 'skipped') AS email_skipped,
		COUNT(*) FILTER (WHERE email_status IN ('pending','sending')) AS email_pending,
		COUNT(*) FILTER (WHERE unsubscribed) AS unsubscribed
		FROM broadcast_deliveries WHERE announcement_id IN ? AND NOT is_test GROUP BY announcement_id`, ids).Scan(&rows)
	for _, r := range rows {
		out[r.AnnouncementID] = Stats{Recipients: r.Recipients, Delivered: r.Delivered, Read: r.Read, ReadPct: pct(r.Read, r.Delivered),
			Clicked: r.Clicked, ClickPct: pct(r.Clicked, r.Delivered), Dismissed: r.Dismissed, Emailed: r.Emailed, EmailFailed: r.EmailFailed,
			EmailSkipped: r.EmailSkipped, EmailPending: r.EmailPending, Unsubscribed: r.Unsubscribed}
	}
	return out
}

func fmtTime(t *time.Time, tz string) string {
	if t == nil {
		return ""
	}
	return t.In(mustZone(tz)).Format("2006-01-02 15:04")
}

// ExportCSV is one row per recipient (and occurrence).
func (s *Service) ExportCSV(a *Announcement) ([]byte, error) {
	s.syncReads(readScope{announcements: []string{a.ID}})
	var rows []Delivery
	if err := fresh(s.DB).Where("announcement_id = ? AND NOT is_test", a.ID).Order("occurrence_at, org_name, name").Find(&rows).Error; err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // Excel reads UTF-8 with a BOM
	w := csv.NewWriter(&buf)
	tz := a.Timezone
	_ = w.Write([]string{"Sent (" + tz + ")", "Name", "Email", "Organisation", "Delivered", "Read", "Clicked", "Dismissed", "Email", "Emailed at", "Email note", "Unsubscribed"})
	for _, d := range rows {
		occ := d.OccurrenceAt
		unsub := ""
		if d.Unsubscribed {
			unsub = "yes"
		}
		_ = w.Write([]string{fmtTime(&occ, tz), csvSafe(d.Name), csvSafe(d.Email), csvSafe(d.OrgName), fmtTime(d.DeliveredAt, tz), fmtTime(d.ReadAt, tz), fmtTime(d.ClickedAt, tz),
			fmtTime(d.DismissedAt, tz), d.EmailStatus, fmtTime(d.EmailedAt, tz), csvSafe(d.EmailNote), unsub})
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// csvSafe keeps spreadsheet apps from running a cell as a formula.
func csvSafe(s string) string {
	if s != "" && (s[0] == '=' || s[0] == '+' || s[0] == '-' || s[0] == '@' || s[0] == '\t' || s[0] == '\r') {
		return "'" + s
	}
	return s
}
