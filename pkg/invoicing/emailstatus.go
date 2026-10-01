package invoicing

import (
	"time"

	"gorm.io/gorm"
)

// EmailStatus summarises the emails sent about one document (from
// invoice_email_log): shown as a chip on the invoice and an icon in lists.
// Test sends are left out.
type EmailStatus struct {
	State       string         `json:"state"` // sent | failed | none
	Sent        int            `json:"sent"`  // emails delivered to the mail server (all kinds)
	Failed      int            `json:"failed"`
	Recorded    int            `json:"recorded"` // opened in the user's own mail app (mailto)
	LastAt      *time.Time     `json:"last_at"`  // last attempt (sent or failed)
	LastSentAt  *time.Time     `json:"last_sent_at"`
	LastKind    string         `json:"last_kind,omitempty"`
	LastTo      string         `json:"last_to,omitempty"`
	LastCC      int            `json:"last_cc"`
	LastError   string         `json:"last_error,omitempty"`
	ViewedAt    *time.Time     `json:"viewed_at"`
	ViewedAfter bool           `json:"viewed_after_last_send"` // the client opened the link after the last send
	ByKind      map[string]int `json:"by_kind"`
}

// EmailStatuses loads the summaries of several documents.
func EmailStatuses(db *gorm.DB, org string, invs []Invoice) map[string]*EmailStatus {
	out := map[string]*EmailStatus{}
	if len(invs) == 0 || !db.Migrator().HasTable(&EmailLog{}) {
		return out
	}
	ids := make([]string, 0, len(invs))
	for _, inv := range invs {
		ids = append(ids, inv.ID)
		out[inv.ID] = &EmailStatus{State: "none", ByKind: map[string]int{}, ViewedAt: inv.ViewedAt}
	}
	var rows []EmailLog
	db.Select("id, invoice_id, kind, mode, status, \"to\", cc, error, created_at").
		Where("organization_id = ? AND invoice_id IN ? AND mode <> ?", org, ids, "test").Order("created_at").Find(&rows)
	for _, r := range rows {
		s := out[r.InvoiceID]
		if s == nil {
			continue
		}
		t := r.CreatedAt
		switch {
		case r.Mode == "mailto" || r.Status == "recorded":
			s.Recorded++
			continue
		case r.Status == "sent":
			s.Sent++
			s.ByKind[r.Kind]++
			s.LastSentAt = &t
			s.LastError = ""
		case r.Status == "failed":
			s.Failed++
			s.LastError = r.Error
		default:
			continue
		}
		s.LastAt, s.LastKind, s.LastTo, s.LastCC = &t, r.Kind, r.To, len(r.CC)
		s.State = map[bool]string{true: "sent", false: "failed"}[r.Status == "sent"]
	}
	for _, s := range out {
		if s.State == "failed" && s.Sent > 0 {
			// The last attempt failed; earlier ones went out.
			s.State = "failed"
		}
		if s.ViewedAt != nil && s.LastSentAt != nil && !s.ViewedAt.Before(*s.LastSentAt) {
			s.ViewedAfter = true
		}
	}
	return out
}

func (h *Handler) withEmailStatus(org string, list []Invoice) {
	m := EmailStatuses(h.DB, org, list)
	for i := range list {
		list[i].EmailStatus = m[list[i].ID]
	}
}
