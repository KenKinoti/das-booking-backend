package invoicing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// ---------------------------------------------------------------------------
// Recall.
//
// An email that reached the client's mail server can't be deleted from their
// inbox: Outlook's "Recall" only works inside one Microsoft 365 organisation,
// and an attached PDF stays wherever it landed. What DASYIN can do after
// sending is make the email useless:
//
//   - switch off that email's links: each person's View / Pay / quote links
//     carry their own token (emailtrack.go); a recalled token opens a neutral
//     "withdrawn" page (no amounts or lines), online payment and quote
//     acceptance through it are refused. Optionally every link to the
//     document is switched off too (a new public link token: links in other
//     emails, in the PDF and links shared by hand stop working; a later
//     send issues new links);
//   - tell the recipients to disregard it: a short branded notice, tracked
//     per person like the other emails, optionally linking the corrected
//     version;
//   - "Recall and correct": then open the document for editing, or void it
//     and start a corrected copy.
//
// Who opened what before the recall is kept and shown with it. Works for
// invoices, quotes, reminders and receipts.
// ---------------------------------------------------------------------------

// KindNotice is a "please disregard" email about a recalled one.
const KindNotice = "notice"

// EmailRecall is one person's copy of one email, recalled
// (invoice_email_recalls). RecipientID is "" for emails sent before
// per-recipient tracking (one row per address then).
type EmailRecall struct {
	ID             string `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	InvoiceID      string `json:"invoice_id" gorm:"type:varchar(64);index;not null"`
	EmailLogID     string `json:"email_log_id" gorm:"type:varchar(64);index"`
	RecipientID    string `json:"recipient_id" gorm:"type:varchar(64);index"`
	Email          string `json:"email" gorm:"type:varchar(320)"`
	Role           string `json:"role" gorm:"type:varchar(10)"`
	Kind           string `json:"kind" gorm:"type:varchar(20)"`
	// LinksOff: this copy had its own links and they are now off.
	LinksOff bool `json:"links_off"`
	// AllLinks: every link to the document was switched off with it;
	// RetiredToken is the document's public token that was retired.
	AllLinks     bool   `json:"all_links"`
	RetiredToken string `json:"-" gorm:"type:varchar(64);index"`
	// What had happened before the recall (open data itself is kept on the
	// recipient row).
	OpenedBefore     bool   `json:"opened_before"`
	AutoOpenedBefore bool   `json:"auto_opened_before"`
	ViewedBefore     bool   `json:"viewed_before"`
	HadAttachment    bool   `json:"had_attachment"`
	Attachment       string `json:"attachment,omitempty" gorm:"type:varchar(255)"`
	Reason           string `json:"reason,omitempty" gorm:"type:varchar(500)"`
	// The disregard notice sent to this person (when one was asked for).
	NoticeLogID       string     `json:"notice_log_id,omitempty" gorm:"type:varchar(64)"`
	NoticeRecipientID string     `json:"-" gorm:"type:varchar(64)"`
	NoticeError       string     `json:"notice_error,omitempty" gorm:"type:text"`
	RecalledBy        string     `json:"recalled_by" gorm:"type:varchar(255)"`
	RecalledByName    string     `json:"recalled_by_name" gorm:"type:varchar(255)"`
	RecalledAt        time.Time  `json:"recalled_at" gorm:"index"`
	NoticeSentAt      *time.Time `json:"notice_sent_at"`
}

func (EmailRecall) TableName() string { return "invoice_email_recalls" }

// ---------------------------------------------------------------- withdrawn links

// Withdrawal is a link that was switched off by a recall.
type Withdrawal struct {
	OrganizationID string
	InvoiceID      string
	DocType        string
	Number         string
	Kind           string
	At             time.Time
}

// Withdrawn reports whether a public link (the document's public token plus
// the recipient token ?r= from their email, when any) was switched off by a
// recall. nil means the link works as usual.
func Withdrawn(db *gorm.DB, publicToken, recipientToken string) *Withdrawal {
	if db == nil || !recallsReady(db) {
		return nil
	}
	var cur struct {
		ID, OrganizationID, DocType, Number string
	}
	if len(publicToken) >= 20 {
		db.Table("billing_invoices").Select("id, organization_id, doc_type, number").Where("public_token = ? AND deleted_at IS NULL", publicToken).Limit(1).Scan(&cur)
	}
	if validRecipientToken(recipientToken) {
		var rc EmailRecall
		err := db.Raw(`SELECT rc.* FROM invoice_email_recalls rc JOIN invoice_email_recipients er ON er.id = rc.recipient_id
			WHERE er.token = ? AND rc.links_off ORDER BY rc.recalled_at LIMIT 1`, recipientToken).Scan(&rc).Error
		// The recipient's token only counts for the document it belongs to
		// (its current link, or a link of it that was switched off).
		if err == nil && rc.ID != "" && (cur.ID == rc.InvoiceID || (cur.ID == "" && retiredFor(db, publicToken, rc.InvoiceID))) {
			return withdrawalOf(db, rc)
		}
	}
	if cur.ID == "" && len(publicToken) >= 20 {
		var rc EmailRecall
		if db.Where("retired_token = ?", publicToken).Order("recalled_at").Limit(1).Find(&rc); rc.ID != "" {
			return withdrawalOf(db, rc)
		}
	}
	return nil
}

// recallTable remembers that invoice_email_recalls exists (public links
// check it on every view).
var recallTable atomic.Bool

func recallsReady(db *gorm.DB) bool {
	if recallTable.Load() {
		return true
	}
	if db.Migrator().HasTable(&EmailRecall{}) {
		recallTable.Store(true)
		return true
	}
	return false
}

func retiredFor(db *gorm.DB, token, invoiceID string) bool {
	var n int64
	db.Model(&EmailRecall{}).Where("retired_token = ? AND invoice_id = ?", token, invoiceID).Count(&n)
	return n > 0
}

func withdrawalOf(db *gorm.DB, rc EmailRecall) *Withdrawal {
	w := &Withdrawal{OrganizationID: rc.OrganizationID, InvoiceID: rc.InvoiceID, Kind: rc.Kind, At: rc.RecalledAt}
	var inv struct{ DocType, Number string }
	db.Table("billing_invoices").Select("doc_type, number").Where("id = ?", rc.InvoiceID).Limit(1).Scan(&inv)
	w.DocType, w.Number = inv.DocType, inv.Number
	return w
}

// docWord is "invoice", "quote" or "receipt" for the withdrawn page.
func (w *Withdrawal) docWord() string {
	switch {
	case w.Kind == KindReceipt:
		return "receipt"
	case w.DocType == TypeQuote:
		return "quote"
	}
	return "invoice"
}

// payload is the withdrawn page: who withdrew what and when — never an
// amount or a line.
func (w *Withdrawal) payload(db *gorm.DB, r *http.Request) gin.H {
	h := &Handler{DB: db}
	s, err := h.settingsFor(db, w.OrganizationID, false)
	if err != nil || s == nil {
		s = &Settings{}
	}
	biz := PDFBusinessFor(db, s, w.OrganizationID)
	name := biz.Name
	if name == "" {
		name = "the sender"
	}
	logo := ""
	if LogoURL != nil {
		logo = LogoURL(r, w.OrganizationID)
	}
	on := w.At.In(orgLocation(db, w.OrganizationID)).Format("2 Jan 2006")
	word := w.docWord()
	msg := fmt.Sprintf("This %s was withdrawn by %s on %s. Please contact them for the current version.", word, name, on)
	return gin.H{"doc_type": w.DocType, "doc_word": word, "number": w.Number, "withdrawn_on": on, "withdrawn_at": w.At, "message": msg,
		"business": gin.H{"name": biz.Name, "email": biz.Email, "phone": biz.Phone, "website": biz.Website, "logo_url": logo, "accent_color": s.AccentColor}}
}

// write answers 410 Gone with the withdrawn page.
func (w *Withdrawal) write(c *gin.Context, h *Handler) {
	p := w.payload(h.DB, c.Request)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusGone, gin.H{"success": false, "error": gin.H{"code": "WITHDRAWN", "message": p["message"]}, "withdrawn": p})
}

// RetiredInvoiceID is the document whose public link token was switched off
// by a recall ("" when token was never retired).
func RetiredInvoiceID(db *gorm.DB, token string) string {
	if db == nil || len(token) < 20 || !recallsReady(db) {
		return ""
	}
	var rc EmailRecall
	db.Select("invoice_id").Where("retired_token = ?", token).Limit(1).Find(&rc)
	return rc.InvoiceID
}

// RefuseWithdrawn answers 410 when a public link (pay page, checkout) was
// switched off by a recall and reports whether it did (for pkg/payments).
func RefuseWithdrawn(c *gin.Context, db *gorm.DB, publicToken, recipientToken string) bool {
	w := Withdrawn(db, publicToken, recipientToken)
	if w == nil {
		return false
	}
	w.write(c, &Handler{DB: db})
	return true
}

// ---------------------------------------------------------------- routes

func (h *Handler) registerRecallRoutes(r *gin.RouterGroup) {
	r.GET("/email-options", h.GetEmailOptions)
	r.PUT("/email-options", h.PutEmailOptions)
	r.POST("/invoices/:id/emails/:logId/undo", h.UndoEmail)
	r.POST("/invoices/:id/emails/:logId/send-now", h.SendEmailNow)
	r.GET("/invoices/:id/emails/:logId/recall", h.RecallPreview)
	r.POST("/invoices/:id/emails/:logId/recall", h.RecallEmail)
}

// ---------------------------------------------------------------- targets

// recallTarget is one person's copy that can be recalled.
type recallTarget struct {
	Row     *EmailRecipient // nil for emails sent before per-recipient tracking
	Email   string
	Role    string
	Recall  *EmailRecall // already recalled
	Skipped string       // why it can't be recalled
}

func (t recallTarget) hasLinks() bool {
	return t.Row != nil && t.Row.Token != nil && *t.Row.Token != ""
}

// recallTargets lists everyone the email went to.
func (h *Handler) recallTargets(e *EmailLog) []recallTarget {
	var rows []EmailRecipient
	h.DB.Where("email_log_id = ?", e.ID).Order("position").Find(&rows)
	var recalls []EmailRecall
	h.DB.Where("email_log_id = ?", e.ID).Find(&recalls)
	byRow, byEmail := map[string]*EmailRecall{}, map[string]*EmailRecall{}
	for i := range recalls {
		if recalls[i].RecipientID != "" {
			byRow[recalls[i].RecipientID] = &recalls[i]
		} else {
			byEmail[strings.ToLower(recalls[i].Email)] = &recalls[i]
		}
	}
	var out []recallTarget
	if len(rows) == 0 {
		// Sent before per-recipient tracking: the addresses on the log.
		for i, a := range append([]string{e.To}, e.CC...) {
			if strings.TrimSpace(a) == "" {
				continue
			}
			role := "cc"
			if i == 0 {
				role = "to"
			}
			out = append(out, recallTarget{Email: a, Role: role, Recall: byEmail[strings.ToLower(a)]})
		}
		return out
	}
	sort.SliceStable(rows, func(i, j int) bool { return roleOrder(rows[i].Role) < roleOrder(rows[j].Role) })
	for i := range rows {
		r := &rows[i]
		t := recallTarget{Row: r, Email: r.Email, Role: r.Role, Recall: byRow[r.ID]}
		if r.Status == "failed" {
			t.Skipped = "Not delivered — nothing reached them"
		}
		out = append(out, t)
	}
	return out
}

// recallable says whether email e can be recalled ("" = yes).
func recallable(e *EmailLog) string {
	switch {
	case e.Mode == "test":
		return "Test emails only went to you."
	case e.Mode != "email":
		return "This email was sent from your own email app, so DASYIN has no links in it to switch off. Use “Reset client link” to switch off the document's link."
	case e.Kind == KindNotice:
		return "Disregard notices can't be recalled."
	case e.Status == "held":
		return "It hasn't gone yet — use Undo instead."
	case e.Status == "cancelled":
		return "It was undone — nothing was sent."
	case e.Status != "sent":
		return "Nothing was delivered, so there is nothing to recall."
	}
	return ""
}

// ---------------------------------------------------------------- preview

// RecallPreview serves GET /invoices/:id/emails/:logId/recall: who can be
// recalled, what can and can't be done, and the disregard notice's wording.
func (h *Handler) RecallPreview(c *gin.Context) {
	org, inv, e, okk := h.loadLog(c)
	if !okk {
		return
	}
	if why := recallable(e); why != "" {
		fail(c, http.StatusConflict, "NOT_RECALLABLE", why)
		return
	}
	people := []gin.H{}
	tracked := false
	for _, t := range h.recallTargets(e) {
		if t.hasLinks() {
			tracked = true
		}
		p := gin.H{"email": t.Email, "role": t.Role, "own_links": t.hasLinks(), "recalled": t.Recall != nil, "skipped": t.Skipped}
		if t.Row != nil {
			p["opened"] = t.Row.FirstHumanOpenAt != nil || t.Row.ViewCount > 0 || t.Row.AcceptedAt != nil
			p["auto_opened"] = t.Row.AutoOpenCount > 0
			p["viewed"] = t.Row.ViewCount > 0
		}
		people = append(people, p)
	}
	att := ""
	if len(e.Attachments) > 0 {
		att = e.Attachments[0]
	}
	subject, message := h.noticeText(inv, e, "", "", "")
	ok(c, http.StatusOK, gin.H{
		"log_id": e.ID, "kind": e.Kind, "number": inv.Number, "doc_type": inv.DocType, "status": inv.Status, "sent_at": e.CreatedAt,
		"recipients": people, "tracked": tracked, "all_links_required": !tracked,
		"attachment": att, "email_enabled": h.emailEnabled(org),
		"notice_subject": subject, "notice_message": message,
		"replacements": h.replacementChoices(inv),
		"can_edit":     inv.Status != StatusVoid && inv.Status != StatusConverted,
		"can_void":     inv.Status != StatusVoid && inv.Status != StatusConverted && inv.AmountPaid <= 0,
		"truth":        "Once an email reaches someone's mail server it can't be deleted from their inbox — Outlook's “Recall” only works inside one Microsoft 365 organisation, and an attached PDF stays wherever it landed. DASYIN can switch off the email's links and tell the recipients to disregard it.",
	})
}

// replacementChoices are documents a disregard notice can point to as the
// corrected version: this one (when it has a live link) and other sent
// documents of the same client.
func (h *Handler) replacementChoices(inv *Invoice) []gin.H {
	out := []gin.H{}
	var list []Invoice
	q := h.DB.Select("id, number, doc_type, status, total, currency, issue_date, client_name, created_at").
		Where("organization_id = ? AND doc_type IN ? AND status NOT IN ?", inv.OrganizationID, []string{TypeInvoice, TypeQuote}, []string{StatusDraft, StatusVoid})
	if inv.CustomerID != nil && *inv.CustomerID != "" {
		q = q.Where("(customer_id = ? OR id = ?)", *inv.CustomerID, inv.ID)
	} else {
		q = q.Where("(LOWER(client_name) = LOWER(?) OR id = ?)", inv.ClientName, inv.ID)
	}
	q.Order("created_at DESC").Limit(20).Find(&list)
	for _, d := range list {
		label := docLabel(d.DocType) + " " + d.Number
		if d.ID == inv.ID {
			label += " (this one, as it is now)"
		}
		out = append(out, gin.H{"id": d.ID, "number": d.Number, "doc_type": d.DocType, "status": d.Status, "total": d.Total, "currency": d.Currency,
			"label": label, "same": d.ID == inv.ID})
	}
	return out
}

// ---------------------------------------------------------------- recall

type recallInput struct {
	// Recipients: the addresses to recall (empty = everyone on the email).
	Recipients []string `json:"recipients"`
	// AllLinks also switches off every link to the document.
	AllLinks bool `json:"all_links"`
	// Notice emails the same people "Please disregard …".
	Notice bool   `json:"notice"`
	Reason string `json:"reason"`
	// ReplacementID: a sent document the notice links as the corrected one.
	ReplacementID string `json:"replacement_id"`
	// Correct: "" (just recall) | edit (open it for editing; a void or
	// converted document is duplicated instead) | replace (void it and
	// start a corrected copy).
	Correct string `json:"correct"`
	AppURL  string `json:"app_url"`
}

// RecallEmail serves POST /invoices/:id/emails/:logId/recall.
func (h *Handler) RecallEmail(c *gin.Context) {
	org, inv, e, okk := h.loadLog(c)
	if !okk {
		return
	}
	var in recallInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	user := ctxString(c, "user_id")
	out, err := h.recall(c, org, inv, e, in, user)
	if err != nil {
		failErr(c, err)
		return
	}
	ok(c, http.StatusOK, out)
}

// recall does the work of RecallEmail.
func (h *Handler) recall(c *gin.Context, org string, inv *Invoice, e *EmailLog, in recallInput, user string) (gin.H, error) {
	if why := recallable(e); why != "" {
		return nil, &apiError{http.StatusConflict, "NOT_RECALLABLE", why}
	}
	in.Reason = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(in.Reason))
	if len([]rune(in.Reason)) > 300 {
		return nil, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Keep the reason under 300 characters"}
	}
	if e.Kind == KindReceipt {
		in.Correct = "" // a receipt is corrected by editing its payment (the app opens it)
	}
	switch in.Correct {
	case "", "edit":
	case "replace":
		if inv.AmountPaid > 0 {
			return nil, &apiError{http.StatusConflict, "HAS_PAYMENTS", "This " + strings.ToLower(docLabel(inv.DocType)) + " has payments, so it can't be voided. Choose “Edit it” instead."}
		}
	default:
		return nil, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "Unknown correction"}
	}
	var repl *Invoice
	if in.ReplacementID != "" {
		r, err := h.load(h.DB, org, in.ReplacementID, false)
		if err != nil {
			return nil, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", "The corrected document wasn't found"}
		}
		if r.Status == StatusDraft || r.Status == StatusVoid {
			return nil, &apiError{http.StatusBadRequest, "VALIDATION_ERROR", r.Number + " has no live link yet — send it first, or leave the link out of the notice."}
		}
		repl = r
	}

	// Who.
	want := map[string]bool{}
	for _, a := range in.Recipients {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			want[a] = true
		}
	}
	all := h.recallTargets(e)
	var targets []recallTarget
	skipped := []gin.H{}
	for _, t := range all {
		if len(want) > 0 && !want[strings.ToLower(t.Email)] {
			continue
		}
		switch {
		case t.Recall != nil:
			skipped = append(skipped, gin.H{"email": t.Email, "reason": "Already recalled"})
			continue
		case t.Skipped != "":
			skipped = append(skipped, gin.H{"email": t.Email, "reason": t.Skipped})
			continue
		}
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		msg := "Nobody on this email can be recalled"
		if len(skipped) > 0 {
			msg += ": " + fmt.Sprint(skipped[0]["email"]) + " — " + fmt.Sprint(skipped[0]["reason"])
		}
		return nil, &apiError{http.StatusConflict, "NOTHING_TO_RECALL", msg}
	}
	// Copies without their own links (one message to everyone, or sent
	// before per-recipient tracking) can only be switched off with every
	// link to the document.
	forced := false
	for _, t := range targets {
		if !t.hasLinks() && !in.AllLinks {
			in.AllLinks, forced = true, true
		}
	}

	now := h.now()
	who := notify.UserName(h.DB, user)
	att := ""
	if len(e.Attachments) > 0 {
		att = e.Attachments[0]
	}
	var rows []EmailRecall
	var newToken string
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		retired := ""
		if in.AllLinks {
			var cur Invoice
			if err := tx.Select("id, public_token").Where("id = ?", inv.ID).Take(&cur).Error; err != nil {
				return err
			}
			retired, newToken = cur.PublicToken, NewPublicToken()
			if err := tx.Model(&Invoice{}).Where("id = ?", inv.ID).Update("public_token", newToken).Error; err != nil {
				return err
			}
		}
		for _, t := range targets {
			r := EmailRecall{ID: uuid.NewString(), OrganizationID: org, InvoiceID: inv.ID, EmailLogID: e.ID, Email: t.Email, Role: t.Role, Kind: e.Kind,
				LinksOff: t.hasLinks(), AllLinks: in.AllLinks, RetiredToken: retired, HadAttachment: att != "", Attachment: att, Reason: in.Reason,
				RecalledBy: user, RecalledByName: who, RecalledAt: now}
			if t.Row != nil {
				r.RecipientID = t.Row.ID
				r.OpenedBefore = t.Row.FirstHumanOpenAt != nil || t.Row.ViewCount > 0 || t.Row.AcceptedAt != nil
				r.AutoOpenedBefore = !r.OpenedBefore && t.Row.AutoOpenCount > 0
				r.ViewedBefore = t.Row.ViewCount > 0
			}
			if r.AllLinks {
				r.LinksOff = true
			}
			rows = append(rows, r)
		}
		if err := tx.Create(&rows).Error; err != nil {
			return err
		}
		names := make([]string, len(targets))
		for i, t := range targets {
			names[i] = t.Email
		}
		msg := kindTitle(e.Kind) + " email of " + pdfDate(e.CreatedAt) + " recalled for " + strings.Join(names, ", ") + " — its links are switched off"
		if in.AllLinks {
			msg += " · every link to " + inv.Number + " switched off (a new link is issued when it is sent again)"
		}
		if att != "" {
			msg += " · the attached " + strings.Fields(att)[0] + " can't be taken back"
		}
		if in.Reason != "" {
			msg += " (" + in.Reason + ")"
		}
		h.log(tx, inv, "recalled", msg, user)
		return nil
	})
	if err != nil {
		return nil, err
	}
	h.audit(c, org, user, "invoice_email.recall", "invoice", inv.ID, nil, map[string]any{"email_log_id": e.ID, "kind": e.Kind, "number": inv.Number,
		"recipients": recipientEmails(targets), "all_links": in.AllLinks, "notice": in.Notice, "reason": in.Reason, "correct": in.Correct})

	res := gin.H{"log_id": e.ID, "all_links": in.AllLinks, "all_links_forced": forced, "skipped": skipped}
	if att != "" {
		res["attachment_note"] = "The email had " + strings.Fields(att)[0] + " attached — that copy stays in their inbox and can't be taken back."
	}

	// The disregard notice.
	if in.Notice {
		res["notice"] = h.noticeFor(c, org, inv, e, rows, in, repl, user)
	}

	// Recall and correct.
	if in.Correct != "" {
		corr, err := h.correctAfterRecall(org, inv, in.Correct, user)
		if err != nil {
			res["correct_error"] = err.Error()
		} else {
			res["correct"] = corr
		}
	}
	recalled := []gin.H{}
	for _, r := range rows {
		recalled = append(recalled, gin.H{"email": r.Email, "role": r.Role, "links_off": r.LinksOff, "opened_before": r.OpenedBefore,
			"auto_opened_before": r.AutoOpenedBefore, "viewed_before": r.ViewedBefore})
	}
	res["recalled"] = recalled
	if fresh, err := h.load(h.DB, org, inv.ID, true); err == nil {
		res["invoice"] = fresh
	}
	return res, nil
}

func recipientEmails(ts []recallTarget) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Email
	}
	return out
}

// noticeFor sends the disregard notice to the recalled To / CC people and
// records it on their recall rows.
func (h *Handler) noticeFor(c *gin.Context, org string, inv *Invoice, e *EmailLog, rows []EmailRecall, in recallInput, repl *Invoice, user string) gin.H {
	var people []string
	for _, r := range rows {
		if r.Role != "bcc" && strings.TrimSpace(r.Email) != "" {
			people = append(people, r.Email)
		}
	}
	if len(people) == 0 {
		return gin.H{"sent": false, "error": "Only BCC copies were recalled — no notice is needed."}
	}
	entry, err := h.sendNotice(c, org, inv, e, people, in.Reason, repl, user, in.AppURL)
	if err != nil {
		h.DB.Model(&EmailRecall{}).Where("id IN ?", recallIDs(rows)).Update("notice_error", clipText(err.Error(), 1000))
		return gin.H{"sent": false, "error": err.Error()}
	}
	// Link each recall to the person's notice copy.
	var copies []EmailRecipient
	h.DB.Where("email_log_id = ?", entry.ID).Find(&copies)
	byEmail := map[string]EmailRecipient{}
	for _, cp := range copies {
		byEmail[strings.ToLower(cp.Email)] = cp
	}
	now := h.now()
	for _, r := range rows {
		upd := map[string]any{"notice_log_id": entry.ID}
		if cp, ok := byEmail[strings.ToLower(r.Email)]; ok {
			upd["notice_recipient_id"] = cp.ID
			if cp.Status == "failed" {
				upd["notice_error"] = clipText(cp.Error, 1000)
			} else {
				upd["notice_sent_at"] = now
			}
		}
		h.DB.Model(&EmailRecall{}).Where("id = ?", r.ID).Updates(upd)
	}
	h.audit(c, org, user, "invoice_email.notice", "invoice", inv.ID, nil, map[string]any{"email_log_id": entry.ID, "recalled_log_id": e.ID, "to": people, "status": entry.Status})
	return gin.H{"sent": entry.Status == "sent", "log_id": entry.ID, "to": entry.To, "cc": nonNilList(entry.CC), "subject": entry.Subject, "error": entry.Error}
}

func recallIDs(rows []EmailRecall) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// noticeText is the notice's subject and message.
func (h *Handler) noticeText(inv *Invoice, e *EmailLog, reason, replURL, replLabel string) (string, string) {
	loc := orgLocation(h.DB, inv.OrganizationID)
	sent := e.CreatedAt.In(loc)
	day := sent.Format("2 Jan")
	if sent.Year() != h.now().In(loc).Year() {
		day = sent.Format("2 Jan 2006")
	}
	what := strings.ToLower(docLabel(inv.DocType)) + " " + inv.Number
	switch e.Kind {
	case KindReminder:
		what = "the payment reminder for invoice " + inv.Number
	case KindReceipt:
		what = "receipt"
		if num := h.receiptNumberOf(inv, e.PaymentID); num != "" {
			what += " " + num
		}
		what += " for invoice " + inv.Number
	}
	subject := "Please disregard " + what + " sent on " + day
	s, _ := h.settingsFor(h.DB, inv.OrganizationID, false)
	if s == nil {
		s = &Settings{}
	}
	biz := PDFBusinessFor(h.DB, s, inv.OrganizationID).Name
	var b strings.Builder
	client := strings.TrimSpace(inv.ClientName)
	if client == "" {
		client = "there"
	}
	b.WriteString("Hi " + client + ",\n\n")
	b.WriteString("Please disregard " + what + " that we emailed on " + day + ".")
	if reason != "" {
		b.WriteString(" " + strings.TrimRight(reason, ".") + ".")
	}
	b.WriteString(" Its links no longer work.")
	if len(e.Attachments) > 0 {
		b.WriteString(" Please also delete the PDF that came with it.")
	}
	b.WriteString("\n\n")
	if replURL != "" {
		b.WriteString("The corrected version: " + replURL + "\n\n")
	} else {
		b.WriteString("We'll send you the correct version shortly.\n\n")
	}
	b.WriteString("Sorry for the confusion — just reply to this email if you have any questions.")
	if biz != "" {
		b.WriteString("\n\n" + biz)
	}
	_ = replLabel
	return subject, b.String()
}

func (h *Handler) receiptNumberOf(inv *Invoice, pid string) string {
	for _, p := range inv.Payments {
		if p.ID == pid {
			return p.ReceiptNumber
		}
	}
	if p, _ := h.cancelledReceipt(inv, pid); p != nil {
		return p.ReceiptNumber
	}
	return ""
}

// sendNotice emails "Please disregard …" to people, one tracked copy each
// (emailtrack.go), and logs it like any other invoice email.
func (h *Handler) sendNotice(c *gin.Context, org string, inv *Invoice, e *EmailLog, people []string, reason string, repl *Invoice, user, appURL string) (*EmailLog, error) {
	mcfg, viaMail := h.mailConfig(org)
	if !viaMail || !mcfg.Ready() {
		return nil, errors.New("email sending isn't set up — the links are switched off, but no notice was sent")
	}
	s, err := h.settingsFor(h.DB, org, false)
	if err != nil || s == nil {
		s = &Settings{}
	}
	base := h.AppBase(c, appURL)
	replURL, replLabel := "", ""
	if repl != nil {
		// Load it again: "every link" may have just given it a new token.
		if fresh, err := h.load(h.DB, org, repl.ID, false); err == nil {
			repl = fresh
		}
		replURL = ViewURL(base, repl.PublicToken)
		replLabel = "The corrected " + strings.ToLower(docLabel(repl.DocType))
	}
	subject, message := h.noticeText(inv, e, reason, replURL, replLabel)
	b := &builtEmail{Kind: KindNotice, Doc: inv, Subject: subject, Message: message, To: people[0], CC: people[1:], ViewURL: replURL}
	b.Business = PDFBusinessFor(h.DB, s, org)
	b.LogoURL = logoFor(c, org)
	b.From, b.Source, b.ReplyTo = mcfg.Display(), mcfg.Source, mcfg.ReplyTo
	if b.ReplyTo == "" {
		b.ReplyTo = mcfg.FromEmail
	}
	replyNote := ""
	if b.ReplyTo != "" {
		replyNote = "Questions? Just reply to this email — it goes to " + b.ReplyTo + "."
	}
	v := EmailView{Kind: KindNotice, Accent: s.AccentColor, DocLabel: "Please disregard", Number: inv.Number, Message: message, LogoURL: b.LogoURL, ReplyTo: replyNote,
		Business: EmailBusiness{Name: b.Business.Name, Address: b.Business.Address, Email: b.Business.Email, Phone: b.Business.Phone,
			Website: b.Business.Website, TaxNumberLabel: b.Business.TaxNumberLabel, TaxNumber: b.Business.TaxNumber},
		Preheader: subject}
	if LogoSize != nil && v.LogoURL != "" {
		v.LogoW, v.LogoH = LogoSize(org)
	}
	if replURL != "" {
		v.Actions = []EmailAction{{Label: replLabel, URL: replURL, Primary: true}}
	}
	b.View = v
	b.HTML, b.Text = RenderEmail(v)
	b.Amounts = map[string]string{}

	entry := &EmailLog{ID: uuid.NewString(), OrganizationID: org, InvoiceID: inv.ID, Kind: KindNotice, Mode: "email", UserID: user, UserEmail: h.userEmail(user),
		FromAddress: b.From, Source: b.Source, To: b.To, CC: nonNilList(b.CC), BCC: []string{}, Subject: subject, Checks: []Check{}, Amounts: b.Amounts,
		Attachments: []string{}, PaymentID: e.PaymentID, CreatedAt: h.now()}
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	start := time.Now()
	sendErr := h.deliver(ctx, c, org, mcfg, b, entry)
	entry.DurationMS = int(time.Since(start).Milliseconds())
	if sendErr != nil {
		entry.Status, entry.Error, entry.ErrorCode = "failed", sendErr.Error(), mail.Code(sendErr)
	} else {
		entry.Status = "sent"
		if entry.Response == "" {
			entry.Response = "250 accepted by " + mcfg.Host
		}
	}
	if err := h.DB.Create(entry).Error; err != nil {
		fmt.Printf("invoicing: notice log: %v\n", err)
	}
	if sendErr != nil {
		return entry, fmt.Errorf("the disregard notice could not be sent: %w", sendErr)
	}
	msg := "Disregard notice emailed to " + strings.Join(people, ", ")
	h.logRecipients(h.DB, inv, "notice_sent", msg, user, &Recipients{Kind: KindNotice, To: b.To, CC: b.CC, Emailed: true})
	return entry, nil
}

// correctAfterRecall opens the way to a corrected version: the same
// document for editing, or (voided when asked, or already void / converted)
// a new draft copy.
func (h *Handler) correctAfterRecall(org string, inv *Invoice, how, user string) (gin.H, error) {
	cur, err := h.load(h.DB, org, inv.ID, false)
	if err != nil {
		return nil, err
	}
	locked := cur.Status == StatusVoid || cur.Status == StatusConverted
	if how == "edit" && !locked {
		return gin.H{"action": "edit", "invoice_id": cur.ID, "number": cur.Number, "doc_type": cur.DocType}, nil
	}
	var out *Invoice
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		src, err := h.load(tx, org, cur.ID, false)
		if err != nil {
			return err
		}
		voided := false
		if how == "replace" && !locked {
			if src.AmountPaid > 0 {
				return &apiError{http.StatusConflict, "HAS_PAYMENTS", "It has payments, so it can't be voided"}
			}
			if err := tx.Model(&Invoice{}).Where("id = ?", src.ID).Updates(map[string]interface{}{"status": StatusVoid, "voided_at": h.now(), "is_recurring": false}).Error; err != nil {
				return err
			}
			h.log(tx, src, "voided", docLabel(src.DocType)+" voided to send a corrected copy (recall and correct)", user)
			voided = true
		}
		n, err := h.copyInvoice(tx, src, src.DocType, user, h.now(), true)
		if err != nil {
			return err
		}
		note := "Corrected copy of " + src.Number + " (recalled)"
		if voided {
			note += " — " + src.Number + " was voided"
		}
		h.log(tx, n, "created", note, user)
		h.log(tx, src, "corrected", "Corrected copy "+n.Number+" started", user)
		out = n
		return nil
	})
	if err != nil {
		return nil, err
	}
	action := "duplicate"
	if how == "replace" && !locked {
		action = "replace"
	}
	return gin.H{"action": action, "invoice_id": out.ID, "number": out.Number, "doc_type": out.DocType, "from": cur.Number}, nil
}

// ---------------------------------------------------------------- status

// RecallStatus is what the Emails tab shows for one recalled copy.
type RecallStatus struct {
	Email            string     `json:"email"`
	Role             string     `json:"role"`
	At               time.Time  `json:"at"`
	By               string     `json:"by"`
	LinksOff         bool       `json:"links_off"`
	AllLinks         bool       `json:"all_links"`
	OpenedBefore     bool       `json:"opened_before"`
	AutoOpenedBefore bool       `json:"auto_opened_before"`
	ViewedBefore     bool       `json:"viewed_before"`
	Attachment       string     `json:"attachment,omitempty"`
	Reason           string     `json:"reason,omitempty"`
	Notice           string     `json:"notice"` // "" | sent | failed
	NoticeSentAt     *time.Time `json:"notice_sent_at,omitempty"`
	NoticeOpenedAt   *time.Time `json:"notice_opened_at,omitempty"`
	NoticeAutoOpen   bool       `json:"notice_auto_open,omitempty"`
	NoticeViewedAt   *time.Time `json:"notice_viewed_at,omitempty"`
	NoticeError      string     `json:"notice_error,omitempty"`
	NoticeLogID      string     `json:"notice_log_id,omitempty"`
	// Text: "Recalled 18 Nov 10:42 by Ken — links off · opened before recall:
	// yes · disregard notice sent, opened 10:50 · the attached PDF can't be
	// taken back".
	Text string `json:"text"`
}

// recallsByLog loads the recalls of the given emails: per recipient row and
// per email (all of them, for emails without per-recipient rows).
func recallsByLog(db *gorm.DB, org string, logIDs []string, loc *time.Location) (map[string]*RecallStatus, map[string][]RecallStatus) {
	byRow, byLog := map[string]*RecallStatus{}, map[string][]RecallStatus{}
	if len(logIDs) == 0 || !db.Migrator().HasTable(&EmailRecall{}) {
		return byRow, byLog
	}
	var rows []EmailRecall
	db.Where("organization_id = ? AND email_log_id IN ?", org, logIDs).Order("recalled_at").Find(&rows)
	if len(rows) == 0 {
		return byRow, byLog
	}
	nids := []string{}
	for _, r := range rows {
		if r.NoticeRecipientID != "" {
			nids = append(nids, r.NoticeRecipientID)
		}
	}
	notices := map[string]EmailRecipient{}
	if len(nids) > 0 {
		var ns []EmailRecipient
		db.Where("id IN ?", nids).Find(&ns)
		for _, n := range ns {
			notices[n.ID] = n
		}
	}
	for _, r := range rows {
		st := recallStatusOf(r, notices, loc)
		if r.RecipientID != "" {
			s := st
			byRow[r.RecipientID] = &s
		}
		byLog[r.EmailLogID] = append(byLog[r.EmailLogID], st)
	}
	return byRow, byLog
}

func recallStatusOf(r EmailRecall, notices map[string]EmailRecipient, loc *time.Location) RecallStatus {
	if loc == nil {
		loc = time.UTC
	}
	s := RecallStatus{Email: r.Email, Role: r.Role, At: r.RecalledAt, By: r.RecalledByName, LinksOff: r.LinksOff, AllLinks: r.AllLinks,
		OpenedBefore: r.OpenedBefore, AutoOpenedBefore: r.AutoOpenedBefore, ViewedBefore: r.ViewedBefore, Attachment: r.Attachment, Reason: r.Reason,
		NoticeLogID: r.NoticeLogID, NoticeError: r.NoticeError, NoticeSentAt: r.NoticeSentAt}
	if r.HadAttachment && s.Attachment == "" {
		s.Attachment = "the PDF"
	}
	switch {
	case r.NoticeError != "":
		s.Notice = "failed"
	case r.NoticeRecipientID != "":
		s.Notice = "sent"
		if n, ok := notices[r.NoticeRecipientID]; ok {
			if n.Status == "failed" {
				s.Notice, s.NoticeError = "failed", n.Error
			}
			if n.FirstHumanOpenAt != nil {
				s.NoticeOpenedAt = n.FirstHumanOpenAt
			} else if n.FirstOpenedAt != nil {
				s.NoticeOpenedAt, s.NoticeAutoOpen = n.FirstOpenedAt, true
			}
			s.NoticeViewedAt = n.FirstViewedAt
		}
	}
	at := func(t time.Time) string { return t.In(loc).Format("2 Jan 15:04") }
	hm := func(t time.Time) string { return t.In(loc).Format("15:04") }
	parts := []string{"Recalled " + at(r.RecalledAt)}
	if s.By != "" {
		parts[0] += " by " + s.By
	}
	links := "links off"
	if r.AllLinks {
		links = "every link to the document off"
	}
	parts[0] += " — " + links
	opened := "no"
	switch {
	case r.ViewedBefore:
		opened = "yes (used their link)"
	case r.OpenedBefore:
		opened = "yes"
	case r.AutoOpenedBefore:
		opened = "only automatically (may not be a person)"
	}
	parts = append(parts, "opened before recall: "+opened)
	switch s.Notice {
	case "sent":
		n := "disregard notice sent"
		if s.NoticeOpenedAt != nil {
			n += ", opened " + hm(*s.NoticeOpenedAt)
			if s.NoticeAutoOpen {
				n += " (automatic)"
			}
		} else {
			n += ", not opened yet"
		}
		parts = append(parts, n)
	case "failed":
		parts = append(parts, "disregard notice not delivered")
	}
	if s.Attachment != "" {
		parts = append(parts, "the attached "+strings.Fields(s.Attachment)[0]+" can't be taken back")
	}
	s.Text = strings.Join(parts, " · ")
	return s
}
