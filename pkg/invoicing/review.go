package invoicing

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
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
)

// ---------------------------------------------------------------------------
// Pending review (pkg/outbox).
//
// While the organisation reviews client messages (the default), an invoice,
// quote, reminder or receipt email that passed its pre-send checks is not
// sent: it becomes a Pending-review item whose id is the id of its email-log
// entry (status "awaiting_approval"), and the document is not marked sent.
// An approver ("Send now — I've reviewed it", send_now: true) sends at once.
// Approved, it goes through sendEmail again (release) — the checks run once
// more, the PDF is made again with any later edits, and the same entry
// becomes "sent" or "failed" with the usual tracking. Rejected or no longer
// needed (paid, voided …), the entry says so and the timeline too.
// ---------------------------------------------------------------------------

// ReviewMeta is what an in-process caller (schedule send, subscriptions)
// tells Pending review about a message it asks to send (body "review").
type ReviewMeta struct {
	Trigger     string               `json:"trigger"`
	TriggerKind string               `json:"trigger_kind"` // manual | automatic | scheduled
	Origin      string               `json:"origin"`       // schedsend | subscriptions
	OriginRef   string               `json:"origin_ref"`   // delivery id / event id
	Dated       bool                 `json:"dated"`
	Deadline    *time.Time           `json:"deadline"`
	Flag        string               `json:"flag"`
	Blocks      string               `json:"blocks"`
	SendAt      *time.Time           `json:"send_at"`
	Key         string               `json:"key"`
	Context     []outbox.ContextLine `json:"context"`
	// Force: queue even if the setting was just switched off (a schedule
	// prepared ahead must never go early).
	Force bool `json:"force"`
}

// reviewPayload is what a queued email needs besides the editable fields.
type reviewPayload struct {
	InvoiceID      string `json:"invoice_id"`
	Kind           string `json:"kind"`
	PaymentID      string `json:"payment_id"`
	AppURL         string `json:"app_url"`
	APIBase        string `json:"api_base"`
	LogoURL        string `json:"logo_url"`
	AcceptWarnings bool   `json:"accept_warnings"`
}

// reviewDecision runs the gate for one send.
func (h *Handler) reviewDecision(c *gin.Context, org, user string, in sendInput) outbox.Decision {
	if in.release || !outbox.Active() {
		return outbox.Decision{}
	}
	role := ""
	if c != nil {
		role = ctxString(c, "user_role")
	}
	d := outbox.Decide(org, outbox.Who{UserID: user, Role: role}, in.SendNow)
	if !d.Wait && !d.SentNow && in.Review != nil && in.Review.Force {
		d.Wait = true
	}
	return d
}

func outboxKind(kind string) string {
	switch kind {
	case KindQuote:
		return outbox.KindQuoteEmail
	case KindReminder:
		return outbox.KindReminderEmail
	case KindReceipt:
		return outbox.KindReceiptEmail
	case KindNotice:
		return outbox.KindNoticeEmail
	}
	return outbox.KindInvoiceEmail
}

func docLink(inv *Invoice) string {
	if inv.DocType == TypeQuote {
		return "/quotes/" + inv.ID
	}
	return "/invoices/" + inv.ID
}

// reviewItem builds the waiting item of a checked email.
func (h *Handler) reviewItem(c *gin.Context, inv *Invoice, b *builtEmail, in sendInput, entry *EmailLog, user string) *outbox.Item {
	p := reviewPayload{InvoiceID: inv.ID, Kind: b.Kind, PaymentID: paymentID(b.Payment), AppURL: h.AppBase(c, in.AppURL), APIBase: publicAPIBase(c),
		LogoURL: b.LogoURL, AcceptWarnings: true}
	it := &outbox.Item{ID: entry.ID, OrganizationID: inv.OrganizationID, Kind: outboxKind(b.Kind), Module: "invoicing", Channel: outbox.ChanEmail,
		To: b.To, ToName: firstNonEmpty(inv.ClientContact, inv.ClientName), CC: nonNilList(b.CC), BCC: nonNilList(b.BCC), Subject: b.Subject, Message: b.Message,
		Editable: []string{outbox.FieldTo, outbox.FieldCC, outbox.FieldBCC, outbox.FieldSubject, outbox.FieldMessage}, Payload: outbox.Payload(p),
		RecordType: inv.DocType, RecordID: inv.ID, RecordNumber: inv.Number, RecordLabel: strings.TrimSpace(docLabel(inv.DocType) + " " + inv.Number),
		RecordLink: docLink(inv), CustomerName: inv.ClientName, Currency: inv.Currency, CreatedBy: user}
	if inv.CustomerID != nil {
		it.CustomerID = *inv.CustomerID
	}
	amt := inv.Total
	switch b.Kind {
	case KindReceipt:
		if b.Payment != nil {
			amt = b.Payment.Amount
		}
	case KindReminder:
		amt = inv.BalanceDue
	}
	it.Amount = &amt
	ctxLines := []outbox.ContextLine{}
	due := ""
	if !inv.DueDate.IsZero() {
		w := " · due "
		if inv.DocType == TypeQuote {
			w = " · valid until "
		}
		due = w + inv.DueDate.Format("2 Jan 2006")
	}
	ctxLines = append(ctxLines, outbox.ContextLine{Label: docLabel(inv.DocType), Value: strings.TrimSpace(inv.Number + " · " + FormatMoney(inv.Total, inv.Currency) + due)})
	if b.Kind == KindReminder {
		ctxLines = append(ctxLines, outbox.ContextLine{Label: "Still to pay", Value: FormatMoney(inv.BalanceDue, inv.Currency)})
	}
	if b.Kind == KindReceipt && b.Payment != nil {
		ctxLines = append(ctxLines, outbox.ContextLine{Label: "Payment", Value: strings.TrimSpace(b.Payment.ReceiptNumber + " · " + FormatMoney(b.Payment.Amount, inv.Currency))})
	}
	if inv.ClientName != "" {
		ctxLines = append(ctxLines, outbox.ContextLine{Label: "Client", Value: inv.ClientName})
	}
	// Who / why.
	switch {
	case in.Review != nil && in.Review.Trigger != "":
		it.Trigger, it.TriggerKind = in.Review.Trigger, in.Review.TriggerKind
	case c == nil && user == "" && b.Kind == KindReceipt:
		it.Trigger, it.TriggerKind = "Automatic: online payment received", outbox.TriggerAutomatic
	case c == nil && user == "":
		it.Trigger, it.TriggerKind = "Automatic", outbox.TriggerAutomatic
	}
	it.Dated = b.Kind == KindReminder
	if r := in.Review; r != nil {
		it.Origin, it.OriginRef, it.Key = r.Origin, r.OriginRef, r.Key
		it.Dated = it.Dated || r.Dated
		it.Deadline, it.Flag, it.Blocks, it.SendAt = r.Deadline, r.Flag, r.Blocks, r.SendAt
		ctxLines = append(ctxLines, r.Context...)
	}
	if it.Deadline == nil && inv.DocType == TypeQuote && b.Kind == KindQuote && !inv.DueDate.IsZero() {
		exp := dateOnly(inv.DueDate).Add(24*time.Hour - time.Minute)
		if exp.Sub(h.now()) <= 72*time.Hour {
			it.Deadline = &exp
			it.Flag = "Quote expires " + exp.Format("Mon 2 Jan")
		}
	}
	it.Context = ctxLines
	return it
}

// queueEmail stores the item and the "waiting" timeline entry (in tx).
func (h *Handler) queueEmail(tx *gorm.DB, c *gin.Context, inv *Invoice, b *builtEmail, in sendInput, entry *EmailLog, user string) (*outbox.Item, error) {
	it, err := outbox.Enqueue(tx, h.reviewItem(c, inv, b, in, entry, user))
	if err != nil {
		return nil, err
	}
	entry.Status = "awaiting_approval"
	entry.Response = "Waiting for approval in Pending review"
	msg := kindTitle(b.Kind) + " email to " + b.To
	if len(b.CC) > 0 {
		msg += " (+" + fmt.Sprint(len(b.CC)) + " CC)"
	}
	msg += " waiting for approval in Pending review"
	h.log(tx, inv, "awaiting_approval", msg, user)
	return it, nil
}

// reviewJSON is the "review" object of a send answer.
func reviewJSON(res *sendResult) (bool, any) {
	switch {
	case res == nil:
		return false, nil
	case res.Queued != nil:
		return true, outbox.QueuedRef(res.Queued)
	case res.SentNow:
		return false, outbox.SentNowRef()
	}
	return false, nil
}

// ---------------------------------------------------------------- releaser

type reviewReleaser struct{ h *Handler }

func (h *Handler) registerReview() {
	outbox.Register("invoicing", &reviewReleaser{h: h})
}

func (r *reviewReleaser) payload(it *outbox.Item) reviewPayload {
	var p reviewPayload
	_ = outbox.PayloadOf(it, &p)
	if p.InvoiceID == "" {
		p.InvoiceID = it.RecordID
	}
	return p
}

// Recheck: is the email still right to send?
func (r *reviewReleaser) Recheck(ctx context.Context, it *outbox.Item) string {
	if it.Kind == outbox.KindNoticeEmail {
		return ""
	}
	p := r.payload(it)
	var inv Invoice
	if err := r.h.DB.Where("id = ? AND organization_id = ?", p.InvoiceID, it.OrganizationID).Take(&inv).Error; err != nil {
		return "The " + strings.ToLower(docLabel(it.RecordType)) + " was deleted"
	}
	Derive(&inv, r.h.now())
	word := strings.ToLower(docLabel(inv.DocType))
	paid := inv.DocType == TypeInvoice && (inv.Status == StatusPaid || (inv.Total > 0 && inv.BalanceDue <= 0.005 && inv.Status != StatusDraft))
	switch {
	case inv.Status == StatusVoid && p.Kind != KindReceipt:
		return "The " + word + " was voided"
	case inv.Status == StatusConverted:
		return "The quote was converted to an invoice"
	}
	switch p.Kind {
	case KindReminder:
		if paid {
			return "Paid in full — the reminder isn't needed"
		}
		if inv.Status != StatusSent && inv.Status != StatusPartial {
			return "The invoice isn't waiting for payment any more"
		}
	case KindInvoice:
		if paid && inv.SentAt == nil {
			return ""
		}
		if paid {
			return "Already paid in full"
		}
	case KindQuote:
		switch inv.Status {
		case StatusAccepted:
			return "The quote was already accepted"
		case StatusDeclined:
			return "The quote was declined"
		}
	case KindReceipt:
		var n int64
		r.h.DB.Model(&Payment{}).Where("id = ? AND invoice_id = ?", p.PaymentID, inv.ID).Count(&n)
		if n == 0 {
			return "The payment was removed"
		}
	}
	return ""
}

// Send goes through sendEmail again (release): checks, PDF, tracking.
func (r *reviewReleaser) Send(ctx context.Context, it *outbox.Item) (outbox.Result, error) {
	if it.Kind == outbox.KindNoticeEmail {
		return r.h.releaseNotice(ctx, it)
	}
	p := r.payload(it)
	cc, bcc := append([]string{}, it.CC...), append([]string{}, it.BCC...)
	accept := true
	in := sendInput{To: it.To, CC: &cc, BCC: &bcc, Subject: it.Subject, Message: it.Message, Kind: p.Kind, PaymentID: p.PaymentID, Mode: "email",
		AppURL: p.AppURL, AcceptWarnings: &accept}
	in.logID, in.apiBase, in.logoURL, in.release = it.ID, p.APIBase, p.LogoURL, true
	in.approvedBy = it.ApprovedByName
	out, err := r.h.sendEmail(nil, it.OrganizationID, p.InvoiceID, in, it.CreatedBy)
	if err != nil {
		var ce *checkError
		if errors.As(err, &ce) {
			return outbox.Result{}, errors.New(ce.msg)
		}
		return outbox.Result{}, err
	}
	if !out.Emailed {
		return outbox.Result{}, errors.New("email sending isn't set up for this organisation")
	}
	text := "Sent to " + out.Built.To
	if n := len(out.Built.CC); n > 0 {
		text += fmt.Sprintf(" (+%d CC)", n)
	}
	return outbox.Result{Text: text, Ref: it.ID}, nil
}

// Preview builds the email exactly as it would go now.
func (r *reviewReleaser) Preview(ctx context.Context, it *outbox.Item) outbox.Preview {
	if it.Kind == outbox.KindNoticeEmail {
		return r.h.previewNotice(it)
	}
	p := r.payload(it)
	pv := outbox.Preview{Subject: it.Subject, To: it.To, CC: it.CC, BCC: it.BCC, Text: it.Message}
	inv, err := r.h.load(r.h.DB, it.OrganizationID, p.InvoiceID, false)
	if err != nil {
		pv.Error = "The " + strings.ToLower(docLabel(it.RecordType)) + " was deleted, so there is nothing to preview."
		return pv
	}
	mcfg, viaMail := r.h.mailConfig(it.OrganizationID)
	cc, bcc := append([]string{}, it.CC...), append([]string{}, it.BCC...)
	in := sendInput{To: it.To, CC: &cc, BCC: &bcc, Subject: it.Subject, Message: it.Message, Kind: p.Kind, PaymentID: p.PaymentID, Mode: "email", AppURL: p.AppURL}
	in.apiBase, in.logoURL = p.APIBase, p.LogoURL
	b, err := r.h.buildEmail(r.h.DB, nil, inv, in, mcfg, viaMail)
	if err != nil {
		pv.Error = "The preview couldn't be made: " + err.Error()
		return pv
	}
	if viaMail {
		b.previewAsRecipient(LoadTrackingPrefs(r.h.DB, it.OrganizationID))
	}
	pv.Subject, pv.From, pv.ReplyTo, pv.To, pv.CC, pv.BCC, pv.HTML, pv.Text = b.Subject, b.From, b.ReplyTo, b.To, nonNilList(b.CC), nonNilList(b.BCC), b.HTML, b.Text
	pv.Attachments = []outbox.Attachment{}
	note := "Made again when it is sent, so later edits are included"
	switch {
	case p.Kind == KindReceipt && b.Payment != nil:
		pv.Attachments = append(pv.Attachments, outbox.Attachment{Name: b.Attachment.Filename, Kind: "pdf", Note: note,
			URL: "/invoicing/invoices/" + inv.ID + "/payments/" + b.Payment.ID + "/receipt"})
	case b.NoPDF:
		pv.Attachments = append(pv.Attachments, outbox.Attachment{Name: PDFFilename(b.Doc), Kind: "pdf", Note: "Not attached — the email links to it (Invoice settings → Email)",
			URL: "/invoicing/invoices/" + inv.ID + "/pdf"})
	case b.Attachment.Filename != "":
		pv.Attachments = append(pv.Attachments, outbox.Attachment{Name: b.Attachment.Filename, Kind: "pdf", Note: note, URL: "/invoicing/invoices/" + inv.ID + "/pdf"})
	}
	if b.PDFErr != nil {
		pv.Error = "The PDF couldn't be made: " + b.PDFErr.Error()
	}
	return pv
}

// Closed: rejected or no longer needed — the email log and timeline say so.
func (r *reviewReleaser) Closed(ctx context.Context, it *outbox.Item) {
	h := r.h
	resp, typ, line := "", "", ""
	switch it.Status {
	case outbox.StatusRejected:
		who := firstNonEmpty(it.DecidedByName, "an admin")
		resp = "Not sent — rejected in Pending review by " + who
		if it.DecisionNote != "" {
			resp += ": " + it.DecisionNote
		}
		typ, line = "review_rejected", resp
	case outbox.StatusSuperseded:
		resp = "Not sent — " + it.SupersededReason
		typ, line = "review_superseded", resp
	default:
		return
	}
	status := map[string]string{outbox.StatusRejected: "rejected", outbox.StatusSuperseded: "superseded"}[it.Status]
	h.DB.Model(&EmailLog{}).Where("id = ? AND status IN ?", it.ID, []string{"awaiting_approval", "failed", "blocked"}).Updates(map[string]any{"status": status, "response": clipText(resp, 900)})
	if it.Kind == outbox.KindNoticeEmail {
		line = "Disregard notice not sent" + strings.TrimPrefix(resp, "Not sent")
	} else {
		line = kindTitle(r.payload(it).Kind) + " email to " + it.To + " not sent" + strings.TrimPrefix(resp, "Not sent")
	}
	var inv Invoice
	if h.DB.Where("id = ? AND organization_id = ?", firstNonEmpty(r.payload(it).InvoiceID, it.RecordID), it.OrganizationID).Take(&inv).Error == nil {
		h.log(h.DB, &inv, typ, clipText(line, 900), it.DecidedBy)
	}
}

// ---------------------------------------------------------------- disregard notices

type noticePayload struct {
	InvoiceID     string   `json:"invoice_id"`
	EmailLogID    string   `json:"email_log_id"`
	People        []string `json:"people"`
	Reason        string   `json:"reason"`
	ReplacementID string   `json:"replacement_id"`
	AppURL        string   `json:"app_url"`
}

// queueNotice puts a disregard notice in Pending review.
func (h *Handler) queueNotice(c *gin.Context, org string, inv *Invoice, e *EmailLog, people []string, reason string, repl *Invoice, user, appURL string) (*outbox.Item, error) {
	base := h.AppBase(c, appURL)
	replURL, replLabel := "", ""
	p := noticePayload{InvoiceID: inv.ID, EmailLogID: e.ID, People: people, Reason: reason, AppURL: base}
	if repl != nil {
		p.ReplacementID = repl.ID
		replURL, replLabel = ViewURL(base, repl.PublicToken), "The corrected "+strings.ToLower(docLabel(repl.DocType))
	}
	subject, message := h.noticeText(inv, e, reason, replURL, replLabel)
	it := &outbox.Item{ID: uuid.NewString(), OrganizationID: org, Kind: outbox.KindNoticeEmail, Module: "invoicing", Channel: outbox.ChanEmail,
		To: people[0], CC: append([]string{}, people[1:]...), BCC: noticeBCC(e, people), Subject: subject, Message: message, ToName: firstNonEmpty(inv.ClientContact, inv.ClientName),
		Editable: []string{outbox.FieldSubject, outbox.FieldMessage}, Payload: outbox.Payload(p), RecordType: inv.DocType, RecordID: inv.ID,
		RecordNumber: inv.Number, RecordLabel: strings.TrimSpace(docLabel(inv.DocType) + " " + inv.Number), RecordLink: docLink(inv),
		CustomerName: inv.ClientName, CreatedBy: user,
		Context: []outbox.ContextLine{{Label: "Recalled email", Value: kindTitle(e.Kind) + " email of " + pdfDate(e.CreatedAt) + " — its links are already switched off"}}}
	if inv.CustomerID != nil {
		it.CustomerID = *inv.CustomerID
	}
	q, err := outbox.Enqueue(nil, it)
	if err != nil {
		return nil, err
	}
	h.log(h.DB, inv, "awaiting_approval", "Disregard notice to "+strings.Join(people, ", ")+" waiting for approval in Pending review", user)
	return q, nil
}

func (h *Handler) noticeArgs(it *outbox.Item) (*Invoice, *EmailLog, noticePayload, *Invoice, error) {
	var p noticePayload
	_ = outbox.PayloadOf(it, &p)
	inv, err := h.load(h.DB, it.OrganizationID, p.InvoiceID, false)
	if err != nil {
		return nil, nil, p, nil, errors.New("the document was deleted")
	}
	var e EmailLog
	if err := h.DB.Where("id = ? AND invoice_id = ?", p.EmailLogID, inv.ID).Take(&e).Error; err != nil {
		return nil, nil, p, nil, errors.New("the recalled email wasn't found")
	}
	var repl *Invoice
	if p.ReplacementID != "" {
		if r, err := h.load(h.DB, it.OrganizationID, p.ReplacementID, false); err == nil {
			repl = r
		}
	}
	return inv, &e, p, repl, nil
}

func (h *Handler) releaseNotice(ctx context.Context, it *outbox.Item) (outbox.Result, error) {
	inv, e, p, repl, err := h.noticeArgs(it)
	if err != nil {
		return outbox.Result{}, err
	}
	people := append([]string{it.To}, it.CC...)
	entry, err := h.sendNoticeAs(nil, it.OrganizationID, inv, e, people, p.Reason, repl, it.CreatedBy, p.AppURL, it.Subject, it.Message)
	if err != nil {
		return outbox.Result{}, err
	}
	var rows []EmailRecall
	h.DB.Where("email_log_id = ? AND LOWER(email) IN ? AND (notice_log_id IS NULL OR notice_log_id = '')", e.ID, lowerAll(people)).Find(&rows)
	h.linkNotice(rows, entry)
	return outbox.Result{Text: "Disregard notice sent to " + strings.Join(people, ", "), Ref: entry.ID}, nil
}

func (h *Handler) previewNotice(it *outbox.Item) outbox.Preview {
	pv := outbox.Preview{Subject: it.Subject, To: it.To, CC: it.CC, Text: it.Message}
	inv, e, p, repl, err := h.noticeArgs(it)
	if err != nil {
		pv.Error = "The preview couldn't be made: " + err.Error()
		return pv
	}
	b := h.noticeEmail(nil, it.OrganizationID, inv, e, append([]string{it.To}, it.CC...), p.Reason, repl, p.AppURL, it.Subject, it.Message)
	pv.From, pv.ReplyTo, pv.HTML, pv.Text = b.From, b.ReplyTo, b.HTML, b.Text
	return pv
}

// ---------------------------------------------------------------- answers

// reviewAnswer adds queued / review to a JSON answer.
func reviewAnswer(resp gin.H, res *sendResult) {
	q, rv := reviewJSON(res)
	resp["queued"] = q
	if rv != nil {
		resp["review"] = rv
	}
}

// statusForWaiting answers a send that waits (HTTP 200 like a send).
var _ = http.StatusOK

// ensure json import is used for payload helpers in tests.
var _ = json.Marshal

func lowerAll(l []string) []string {
	out := make([]string, len(l))
	for i, v := range l {
		out[i] = strings.ToLower(strings.TrimSpace(v))
	}
	return out
}

// reviewedIDs: which email-log entries went through Pending review.
func reviewedIDs(db *gorm.DB, ids []string) map[string]bool {
	out := map[string]bool{}
	if len(ids) == 0 || !db.Migrator().HasTable("outbox_items") {
		return out
	}
	var found []string
	db.Table("outbox_items").Where("id IN ?", ids).Pluck("id", &found)
	for _, id := range found {
		out[id] = true
	}
	return out
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}
