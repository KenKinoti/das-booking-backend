package invoicing

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// EmailStatus summarises the emails sent about one document (from
// invoice_email_log and, per person, invoice_email_recipients): shown as a
// chip on the invoice and an icon in lists. Test sends are left out.
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

	// Per person, for the last delivered invoice / quote / reminder email
	// (receipts only when nothing else went out; the last failed email when
	// none was delivered) — SummaryKind says which kind that is:
	// Tracking is how it went out (per_recipient | single | "" = before
	// tracking); People the To + CC addresses; Opened how many of them opened
	// it (the open image loaded by a person, or one of their links used);
	// AutoOpened those whose only opens looked automatic; Viewed those who
	// used their View / Pay link; PaidVia who paid through their link.
	SummaryKind  string            `json:"summary_kind,omitempty"`
	Tracking     string            `json:"tracking"`
	People       int               `json:"people"`
	Opened       int               `json:"opened"`
	AutoOpened   int               `json:"auto_opened"`
	Viewed       int               `json:"viewed"`
	PaidVia      string            `json:"paid_via,omitempty"`
	OpenTracking bool              `json:"open_tracking"`
	Recipients   []RecipientStatus `json:"recipients"`
	// Summary is a one-line description ("Opened by 2 of 3 · viewed by 1").
	Summary string `json:"summary"`
}

// RecipientStatus is one person's copy of one email and what happened to it.
type RecipientStatus struct {
	Email        string `json:"email"`
	Role         string `json:"role"` // to | cc | bcc
	Tracked      bool   `json:"tracked"`
	OpenTracking bool   `json:"open_tracking"`
	Status       string `json:"status"` // sending | sent | failed
	Error        string `json:"error,omitempty"`
	// State is the furthest step reached: failed | sending | sent |
	// opened_auto (only automatic opens) | opened | viewed | paid — or
	// untracked (BCC / one-message sends).
	State      string     `json:"state"`
	SentAt     *time.Time `json:"sent_at"`
	Opens      int        `json:"opens"`      // opens that looked like a person
	AutoOpens  int        `json:"auto_opens"` // opens that looked automatic
	OpenedAt   *time.Time `json:"opened_at"`  // first open by a person (else first automatic one)
	LastOpenAt *time.Time `json:"last_open_at"`
	// OpenedAutomatic: every open so far looked automatic — "opened
	// (automatic, may not be a person)".
	OpenedAutomatic bool       `json:"opened_automatic"`
	OpenVia         string     `json:"open_via,omitempty"` // gmail | yahoo | apple_proxy | apple_mail | scanner | prefetch
	Views           int        `json:"views"`
	ViewedAt        *time.Time `json:"viewed_at"`
	LastViewedAt    *time.Time `json:"last_viewed_at"`
	PayPageAt       *time.Time `json:"pay_page_at"`
	AcceptedAt      *time.Time `json:"accepted_at"`
	PaidAt          *time.Time `json:"paid_at"`
	PaidAmount      float64    `json:"paid_amount,omitempty"`
	PaidCurrency    string     `json:"paid_currency,omitempty"`
	// Timeline: "Sent 10:40 → Opened 10:42 (3×) → Viewed invoice 10:44" in
	// the organisation's time zone.
	Timeline string `json:"timeline"`
}

// paidTx is a successful online payment started from a recipient's link.
type paidTx struct {
	At       *time.Time
	Amount   float64
	Currency string
}

// paidByRef looks up successful online payments (pkg/payments) by checkout
// reference.
func paidByRef(db *gorm.DB, rows []EmailRecipient) map[string]paidTx {
	refs := []string{}
	for _, r := range rows {
		refs = append(refs, r.CheckoutRefs...)
	}
	out := map[string]paidTx{}
	if len(refs) == 0 || !db.Migrator().HasTable("pay_transactions") {
		return out
	}
	var txs []struct {
		TxRef       string
		Amount      float64
		Currency    string
		CompletedAt *time.Time
		UpdatedAt   time.Time
	}
	db.Table("pay_transactions").Select("tx_ref, amount, currency, completed_at, updated_at").Where("tx_ref IN ? AND status = ?", refs, "successful").Scan(&txs)
	for _, t := range txs {
		at := t.CompletedAt
		if at == nil {
			u := t.UpdatedAt
			at = &u
		}
		out[t.TxRef] = paidTx{At: at, Amount: t.Amount, Currency: t.Currency}
	}
	return out
}

// recipientStatus describes one copy.
func recipientStatus(r EmailRecipient, paid map[string]paidTx, loc *time.Location) RecipientStatus {
	s := RecipientStatus{Email: r.Email, Role: r.Role, Tracked: r.Tracked, OpenTracking: r.OpenTracking, Status: r.Status, Error: r.Error,
		SentAt: r.SentAt, Opens: r.OpenCount - r.AutoOpenCount, AutoOpens: r.AutoOpenCount, LastOpenAt: r.LastOpenedAt, OpenVia: r.OpenVia,
		Views: r.ViewCount, ViewedAt: r.FirstViewedAt, LastViewedAt: r.LastViewedAt, PayPageAt: r.PayPageAt, AcceptedAt: r.AcceptedAt}
	if s.Opens < 0 {
		s.Opens = 0
	}
	s.OpenedAt = r.FirstHumanOpenAt
	if s.OpenedAt == nil {
		s.OpenedAt = r.FirstOpenedAt
	}
	s.OpenedAutomatic = r.OpenCount > 0 && s.Opens == 0
	for _, ref := range r.CheckoutRefs {
		if p, ok := paid[ref]; ok && (s.PaidAt == nil || p.At.Before(*s.PaidAt)) {
			s.PaidAt, s.PaidAmount, s.PaidCurrency = p.At, p.Amount, p.Currency
		}
	}
	switch {
	case r.Status == "failed":
		s.State = "failed"
	case !r.Tracked:
		s.State = "untracked"
	case s.PaidAt != nil:
		s.State = "paid"
	case s.Views > 0 || s.AcceptedAt != nil:
		s.State = "viewed"
	case s.Opens > 0:
		s.State = "opened"
	case s.AutoOpens > 0:
		s.State = "opened_auto"
	case r.Status == "sending":
		s.State = "sending"
	default:
		s.State = "sent"
	}
	s.Timeline = recipientTimeline(s, r.Kind, loc)
	return s
}

func (s RecipientStatus) openedByPerson() bool {
	return s.Opens > 0 || s.Views > 0 || s.AcceptedAt != nil || s.PaidAt != nil
}

func recipientTimeline(s RecipientStatus, kind string, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	at := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.In(loc).Format("2 Jan 15:04")
	}
	switch s.State {
	case "failed":
		return "Not delivered: " + s.Error
	case "sending":
		return "Sending…"
	}
	steps := []string{strings.TrimSpace("Sent " + at(s.SentAt))}
	if !s.Tracked {
		if s.Role == "bcc" {
			return steps[0] + " · BCC copy, not tracked"
		}
		return steps[0] + " · one message to everyone, not tracked per person"
	}
	switch {
	case s.Opens > 0:
		o := "Opened " + at(s.OpenedAt)
		if s.Opens > 1 {
			o += fmt.Sprintf(" (%d×)", s.Opens)
		}
		if s.AutoOpens > 0 {
			o += fmt.Sprintf(" + %d automatic", s.AutoOpens)
		}
		steps = append(steps, o)
	case s.AutoOpens > 0:
		steps = append(steps, "Opened "+at(s.OpenedAt)+" (automatic, may not be a person)")
	}
	if s.ViewedAt != nil {
		what := "Viewed invoice"
		switch kind {
		case KindQuote:
			what = "Viewed quote"
		}
		if s.PayPageAt != nil && !s.PayPageAt.After(*s.ViewedAt) {
			what = "Opened the pay page"
		}
		v := what + " " + at(s.ViewedAt)
		if s.Views > 1 {
			v += fmt.Sprintf(" (%d×)", s.Views)
		}
		steps = append(steps, v)
	}
	if s.AcceptedAt != nil {
		steps = append(steps, "Accepted "+at(s.AcceptedAt))
	}
	if s.PaidAt != nil {
		steps = append(steps, "Paid via link "+at(s.PaidAt))
	}
	return strings.Join(steps, " → ")
}

// recipientsByLog loads the copies of the given sends, in send order (To,
// CCs, BCCs).
func recipientsByLog(db *gorm.DB, org string, logIDs []string, loc *time.Location) map[string][]RecipientStatus {
	out := map[string][]RecipientStatus{}
	if len(logIDs) == 0 || !db.Migrator().HasTable(&EmailRecipient{}) {
		return out
	}
	var rows []EmailRecipient
	db.Where("organization_id = ? AND email_log_id IN ?", org, logIDs).Order("created_at, position").Find(&rows)
	sort.SliceStable(rows, func(i, j int) bool { return roleOrder(rows[i].Role) < roleOrder(rows[j].Role) })
	paid := paidByRef(db, rows)
	for _, r := range rows {
		out[r.EmailLogID] = append(out[r.EmailLogID], recipientStatus(r, paid, loc))
	}
	return out
}

func roleOrder(r string) int {
	switch r {
	case "to":
		return 0
	case "cc":
		return 1
	}
	return 2
}

func orgLocation(db *gorm.DB, org string) *time.Location {
	if tz := orgprefs.Get(db, org).Timezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.UTC
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
		out[inv.ID] = &EmailStatus{State: "none", ByKind: map[string]int{}, ViewedAt: inv.ViewedAt, Recipients: []RecipientStatus{}}
	}
	var rows []EmailLog
	db.Select("id, invoice_id, kind, mode, status, \"to\", cc, error, tracking, created_at").
		Where("organization_id = ? AND invoice_id IN ? AND mode <> ?", org, ids, "test").Order("created_at").Find(&rows)
	// The email the per-person summary describes, per document: the last
	// delivered non-receipt email, else the last delivered one, else the
	// last failed one.
	type pick struct {
		id, kind, tracking string
		people             int
		rank               int
	}
	summaryOf := map[string]pick{}
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
		rank := 0
		if r.Status == "sent" {
			rank = 1
			if r.Kind != KindReceipt {
				rank = 2
			}
		}
		if cur, ok := summaryOf[r.InvoiceID]; !ok || rank >= cur.rank {
			summaryOf[r.InvoiceID] = pick{id: r.ID, kind: r.Kind, tracking: r.Tracking, people: 1 + len(r.CC), rank: rank}
		}
	}
	lastLog := map[string]string{}
	logIDs := []string{}
	for invID, p := range summaryOf {
		s := out[invID]
		s.SummaryKind, s.Tracking, s.People = p.kind, p.tracking, p.people
		lastLog[invID] = p.id
		logIDs = append(logIDs, p.id)
	}
	byLog := map[string][]RecipientStatus{}
	if len(logIDs) > 0 {
		byLog = recipientsByLog(db, org, logIDs, orgLocation(db, org))
	}
	for invID, s := range out {
		if s.ViewedAt != nil && s.LastSentAt != nil && !s.ViewedAt.Before(*s.LastSentAt) {
			s.ViewedAfter = true
		}
		if rs := byLog[lastLog[invID]]; len(rs) > 0 {
			s.Recipients = rs
			summariseRecipients(s)
		}
	}
	return out
}

// summariseRecipients fills the per-person counts and the summary line.
func summariseRecipients(s *EmailStatus) {
	s.People, s.Opened, s.AutoOpened, s.Viewed, s.PaidVia = 0, 0, 0, 0, ""
	for _, r := range s.Recipients {
		if r.Role == "bcc" {
			continue
		}
		s.People++
		if r.OpenTracking {
			s.OpenTracking = true
		}
		if !r.Tracked {
			continue
		}
		switch {
		case r.openedByPerson():
			s.Opened++
		case r.AutoOpens > 0:
			s.AutoOpened++
		}
		if r.Views > 0 || r.AcceptedAt != nil {
			s.Viewed++
		}
		if r.PaidAt != nil && s.PaidVia == "" {
			s.PaidVia = r.Email
		}
	}
	if s.Tracking != TrackPerRecipient {
		return
	}
	parts := []string{fmt.Sprintf("Opened by %d of %d", s.Opened, s.People)}
	if s.Viewed > 0 {
		parts = append(parts, fmt.Sprintf("viewed by %d", s.Viewed))
	}
	if s.AutoOpened > 0 {
		parts = append(parts, fmt.Sprintf("%d automatic open%s", s.AutoOpened, map[bool]string{true: "", false: "s"}[s.AutoOpened == 1]))
	}
	if s.PaidVia != "" {
		parts = append(parts, "paid via "+s.PaidVia+"’s link")
	}
	s.Summary = strings.Join(parts, " · ")
}

func (h *Handler) withEmailStatus(org string, list []Invoice) {
	m := EmailStatuses(h.DB, org, list)
	for i := range list {
		list[i].EmailStatus = m[list[i].ID]
	}
}
