package invoicing

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// ---------------------------------------------------------------------------
// Undo send.
//
// An email sent from the send dialog, the payment dialog's receipt or the AI
// tools (the caller passes "hold": true) passes every pre-send check and is
// then held on the server for the organisation's undo window (Invoice
// settings → Email: off / 10 / 30 / 60 / 120 s, default 30) instead of going
// out at once. Nothing about the document changes while it is held — it is
// not marked Sent until the email actually goes.
//
//   - The hold is a row in invoice_email_holds (plus the email-log entry with
//     status "held"), so a restart neither loses nor repeats it.
//   - A timer fires at the exact send time; a sweep (and the start-up
//     recovery) picks up anything a timer missed — another server, a restart.
//   - Sending claims the row first (held → sending in one UPDATE): only the
//     claimer sends, so it goes out exactly once. Undo is the same UPDATE
//     held → cancelled: it either wins (nothing is sent) or is too late.
//   - A row left "sending" by a crash is never re-sent (it may already have
//     gone); it is marked interrupted and the sender is told to check.
//
// Scheduled sends (pkg/schedsend), subscription invoices and automatic
// receipts don't pass "hold" and go out straight away.
// ---------------------------------------------------------------------------

// Hold states.
const (
	HoldHeld        = "held"
	HoldSending     = "sending"
	HoldSent        = "sent"
	HoldCancelled   = "cancelled"
	HoldFailed      = "failed"
	HoldInterrupted = "interrupted"
)

// staleSending: a claim older than this belongs to a server that stopped.
const staleSending = 3 * time.Minute

// EmailHold is one held email (invoice_email_holds). Its ID is the id of the
// email-log entry it becomes.
type EmailHold struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string     `json:"organization_id" gorm:"type:varchar(255);index;not null"`
	InvoiceID      string     `json:"invoice_id" gorm:"type:varchar(64);index;not null"`
	Kind           string     `json:"kind" gorm:"type:varchar(20)"`
	PaymentID      string     `json:"payment_id,omitempty" gorm:"type:varchar(64)"`
	UserID         string     `json:"user_id" gorm:"type:varchar(255)"`
	Input          string     `json:"-" gorm:"type:text"` // heldInput as JSON
	Seconds        int        `json:"seconds"`
	SendAt         time.Time  `json:"send_at" gorm:"index"`
	State          string     `json:"state" gorm:"type:varchar(20);index"`
	ClaimedAt      *time.Time `json:"claimed_at"`
	DoneAt         *time.Time `json:"done_at"`
	CancelledBy    string     `json:"cancelled_by,omitempty" gorm:"type:varchar(255)"`
	SentNow        bool       `json:"sent_now"` // "Send now" skipped the rest of the wait
	Error          string     `json:"error,omitempty" gorm:"type:text"`
	CreatedAt      time.Time  `json:"created_at"`
}

func (EmailHold) TableName() string { return "invoice_email_holds" }

// heldInput is what the send will use: the request as checked, with the
// recipients resolved and the request's origins (links, open image, logo)
// kept for the background send.
type heldInput struct {
	To             string   `json:"to"`
	CC             []string `json:"cc"`
	BCC            []string `json:"bcc"`
	Subject        string   `json:"subject"`
	Message        string   `json:"message"`
	Kind           string   `json:"kind"`
	PaymentID      string   `json:"payment_id"`
	AppURL         string   `json:"app_url"`
	APIBase        string   `json:"api_base"`
	LogoURL        string   `json:"logo_url"`
	AcceptWarnings bool     `json:"accept_warnings"`
}

// HoldInfo is shown on a held email-log entry.
type HoldInfo struct {
	State       string    `json:"state"`
	Seconds     int       `json:"seconds"`
	SendAt      time.Time `json:"send_at"`
	SecondsLeft int       `json:"seconds_left"`
	SentNow     bool      `json:"sent_now,omitempty"`
	CancelledBy string    `json:"cancelled_by,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// ---------------------------------------------------------------- engine

// HoldEngine sends held emails on time: one precise timer per held email and
// a periodic sweep. One per process (StartHolds); without it holds are still
// created and sent by whichever server runs the engine.
type HoldEngine struct {
	H      *Handler // fully configured (Mail, PublicURL)
	Sweep  time.Duration
	mu     sync.Mutex
	timers map[string]*time.Timer
	stop   chan struct{}
	wg     sync.WaitGroup
}

var (
	holdsMu sync.RWMutex
	holds   *HoldEngine
)

func currentHolds() *HoldEngine {
	holdsMu.RLock()
	defer holdsMu.RUnlock()
	return holds
}

// StartHolds starts the process-wide hold engine: it recovers what a restart
// left (held emails get their timers back, overdue ones go now, interrupted
// sends are reported) and sweeps every sweep interval.
func StartHolds(h *Handler, sweep time.Duration) *HoldEngine {
	if sweep <= 0 {
		sweep = 15 * time.Second
	}
	e := &HoldEngine{H: h, Sweep: sweep, timers: map[string]*time.Timer{}, stop: make(chan struct{})}
	holdsMu.Lock()
	old := holds
	holds = e
	holdsMu.Unlock()
	if old != nil {
		old.Stop()
	}
	e.recover()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(e.Sweep)
		defer t.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-t.C:
				e.ReleaseDue()
			}
		}
	}()
	return e
}

// Stop stops the timers and the sweep (a restart in tests). Held emails stay
// held in the database.
func (e *HoldEngine) Stop() {
	if e == nil {
		return
	}
	e.mu.Lock()
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
	for id, t := range e.timers {
		if t.Stop() {
			e.wg.Done() // its function will never run
		}
		delete(e.timers, id)
	}
	e.mu.Unlock()
	e.wg.Wait()
	holdsMu.Lock()
	if holds == e {
		holds = nil
	}
	holdsMu.Unlock()
}

func (e *HoldEngine) stopped() bool {
	select {
	case <-e.stop:
		return true
	default:
		return false
	}
}

// schedule sets the exact timer for one held email.
func (e *HoldEngine) schedule(id string, at time.Time) {
	if e == nil || e.stopped() {
		return
	}
	d := time.Until(at)
	if d < 0 {
		d = 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.timers[id]; ok && t.Stop() {
		e.wg.Done()
	}
	e.wg.Add(1)
	e.timers[id] = time.AfterFunc(d, func() {
		defer e.wg.Done()
		e.mu.Lock()
		delete(e.timers, id)
		e.mu.Unlock()
		if e.stopped() {
			return
		}
		if _, err := e.H.releaseHold(id, false, ""); err != nil && !errors.Is(err, errNotHeld) {
			log.Printf("invoicing: held email %s: %v", id, err)
		}
	})
}

// forget drops the timer of an email that was undone or sent now.
func (e *HoldEngine) forget(id string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.timers[id]; ok {
		if t.Stop() {
			e.wg.Done() // the timer's function will never run
		}
		delete(e.timers, id)
	}
}

// recover reschedules every held email and reports sends a stopped server
// left half-done.
func (e *HoldEngine) recover() {
	db := e.H.DB
	if !db.Migrator().HasTable(&EmailHold{}) {
		return
	}
	cut := time.Now().Add(-staleSending)
	var stale []EmailHold
	db.Where("state = ? AND claimed_at < ?", HoldSending, cut).Find(&stale)
	for _, s := range stale {
		e.H.interrupted(s)
	}
	var held []EmailHold
	db.Select("id, send_at").Where("state = ?", HoldHeld).Find(&held)
	for _, x := range held {
		e.schedule(x.ID, x.SendAt)
	}
}

// ReleaseDue sends every held email whose time has come (the sweep; also
// reports stale claims). It returns how many it sent or tried to send.
func (e *HoldEngine) ReleaseDue() int {
	if e == nil {
		return 0
	}
	db := e.H.DB
	cut := time.Now().Add(-staleSending)
	var stale []EmailHold
	db.Where("state = ? AND claimed_at < ?", HoldSending, cut).Find(&stale)
	for _, s := range stale {
		e.H.interrupted(s)
	}
	var due []string
	db.Model(&EmailHold{}).Where("state = ? AND send_at <= ?", HoldHeld, time.Now()).Order("send_at").Limit(50).Pluck("id", &due)
	n := 0
	for _, id := range due {
		if e.stopped() {
			break
		}
		if _, err := e.H.releaseHold(id, false, ""); err == nil {
			n++
		}
	}
	return n
}

// scheduleHold gives a new held email its timer (when an engine runs here).
func scheduleHold(id string, at time.Time) {
	if e := currentHolds(); e != nil {
		e.schedule(id, at)
	}
}

// ---------------------------------------------------------------- hold / release

var errNotHeld = errors.New("this email is no longer waiting")

// HoldOverride replaces the organisation's undo window (tests only).
var HoldOverride func(org string) time.Duration

// holdWindow is the exact hold duration (sub-second in tests).
func (h *Handler) holdWindow(org string) time.Duration {
	if HoldOverride != nil {
		return HoldOverride(org)
	}
	return time.Duration(LoadSendOptions(h.DB, org).UndoSeconds) * time.Second
}

// newHold builds the hold row for an email that passed its checks.
func (h *Handler) newHold(c *gin.Context, inv *Invoice, b *builtEmail, in sendInput, entry *EmailLog, user string, wait time.Duration) *EmailHold {
	hi := heldInput{To: b.To, CC: b.CC, BCC: b.BCC, Subject: in.Subject, Message: in.Message, Kind: b.Kind, PaymentID: paymentID(b.Payment),
		AppURL: h.AppBase(c, in.AppURL), APIBase: publicAPIBase(c), LogoURL: b.LogoURL, AcceptWarnings: true}
	if hi.CC == nil {
		hi.CC = []string{}
	}
	if hi.BCC == nil {
		hi.BCC = []string{}
	}
	js, _ := json.Marshal(hi)
	now := h.now()
	return &EmailHold{ID: entry.ID, OrganizationID: inv.OrganizationID, InvoiceID: inv.ID, Kind: b.Kind, PaymentID: hi.PaymentID, UserID: user,
		Input: string(js), Seconds: int((wait + time.Second - 1) / time.Second), SendAt: now.Add(wait), State: HoldHeld, CreatedAt: now}
}

// releaseHold sends held email id now: when it is due, or at once when
// manual ("Send now" by user). Only the caller that claims the row sends.
func (h *Handler) releaseHold(id string, manual bool, user string) (*sendResult, error) {
	now := time.Now()
	q := h.DB.Model(&EmailHold{}).Where("id = ? AND state = ?", id, HoldHeld)
	if !manual {
		q = q.Where("send_at <= ?", now.Add(50*time.Millisecond))
	}
	upd := map[string]any{"state": HoldSending, "claimed_at": now}
	if manual {
		upd["sent_now"] = true
	}
	res := q.Updates(upd)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, errNotHeld
	}
	currentHolds().forget(id)
	var hd EmailHold
	if err := h.DB.Where("id = ?", id).Take(&hd).Error; err != nil {
		return nil, err
	}
	var hi heldInput
	_ = json.Unmarshal([]byte(hd.Input), &hi)
	cc, bcc := hi.CC, hi.BCC
	accept := true
	in := sendInput{To: hi.To, CC: &cc, BCC: &bcc, Subject: hi.Subject, Message: hi.Message, Kind: hi.Kind, PaymentID: hi.PaymentID,
		Mode: "email", AppURL: hi.AppURL, AcceptWarnings: &accept}
	in.logID, in.apiBase, in.logoURL, in.release = hd.ID, hi.APIBase, hi.LogoURL, true
	out, err := h.sendEmail(nil, hd.OrganizationID, hd.InvoiceID, in, hd.UserID)
	done := time.Now()
	if err != nil {
		h.DB.Model(&EmailHold{}).Where("id = ?", id).Updates(map[string]any{"state": HoldFailed, "done_at": done, "error": clipText(err.Error(), 1000)})
		// Failed before the send could write its own entry (e.g. email
		// sending was switched off meanwhile): say so on the held one.
		h.DB.Model(&EmailLog{}).Where("id = ? AND status = ?", id, "held").Updates(map[string]any{"status": "failed", "error_code": "NOT_SENT", "error": clipText(err.Error(), 1000)})
		h.tellSender(hd, "Email not sent", "The held "+kindWord(hd.Kind)+" email to "+hi.To+" wasn't sent: "+err.Error(), notify.Error)
		return nil, err
	}
	h.DB.Model(&EmailHold{}).Where("id = ?", id).Updates(map[string]any{"state": HoldSent, "done_at": done})
	if manual && user != "" {
		h.audit(nil, hd.OrganizationID, user, "invoice_email.send_now", "invoice", hd.InvoiceID, nil, map[string]any{"email_log_id": id, "to": hi.To, "kind": hd.Kind})
	}
	return out, nil
}

// interrupted marks a send a stopped server left half-done. It is never
// repeated automatically: some copies may already have gone.
func (h *Handler) interrupted(s EmailHold) {
	res := h.DB.Model(&EmailHold{}).Where("id = ? AND state = ?", s.ID, HoldSending).
		Updates(map[string]any{"state": HoldInterrupted, "done_at": time.Now(), "error": "The server stopped while this email was being sent."})
	if res.RowsAffected == 0 {
		return
	}
	var sent, total int64
	h.DB.Model(&EmailRecipient{}).Where("email_log_id = ?", s.ID).Count(&total)
	h.DB.Model(&EmailRecipient{}).Where("email_log_id = ? AND status = ?", s.ID, "sent").Count(&sent)
	msg := "The server restarted while it was sending — "
	switch {
	case total == 0:
		msg += "nothing was sent. Send it again from the document."
	default:
		msg += fmt.Sprintf("%d of %d copies were confirmed. Check the Emails tab before sending it again.", sent, total)
	}
	h.DB.Model(&EmailLog{}).Where("id = ? AND status = ?", s.ID, "held").Updates(map[string]any{"status": "failed", "error_code": "INTERRUPTED", "error": msg})
	h.tellSender(s, "Email may not have gone", msg, notify.Warning)
}

// cancelHold undoes held email id: nothing is sent and the document is left
// as it was. ok is false when it is no longer waiting.
func (h *Handler) cancelHold(org, id, user string) (*EmailHold, error) {
	now := h.now()
	res := h.DB.Model(&EmailHold{}).Where("id = ? AND organization_id = ? AND state = ?", id, org, HoldHeld).
		Updates(map[string]any{"state": HoldCancelled, "done_at": now, "cancelled_by": user})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, errNotHeld
	}
	currentHolds().forget(id)
	var hd EmailHold
	h.DB.Where("id = ?", id).Take(&hd)
	who := notify.UserName(h.DB, user)
	resp := "Undone — nothing was sent"
	if who != "" {
		resp = "Undone by " + who + " — nothing was sent"
	}
	h.DB.Model(&EmailLog{}).Where("id = ? AND status = ?", id, "held").Updates(map[string]any{"status": "cancelled", "response": resp})
	return &hd, nil
}

func (h *Handler) tellSender(hd EmailHold, title, body, sev string) {
	if hd.UserID == "" {
		return
	}
	var inv Invoice
	h.DB.Select("id, number, doc_type").Where("id = ?", hd.InvoiceID).Take(&inv)
	link := "/invoices/" + hd.InvoiceID
	if inv.DocType == TypeQuote {
		link = "/quotes/" + hd.InvoiceID
	}
	if inv.Number != "" {
		title += " — " + inv.Number
	}
	notify.Go(h.DB, hd.OrganizationID, notify.Users(hd.UserID), notify.Message{Type: "invoice.email_hold", Severity: sev, Title: title, Body: body,
		Link: link, EntityType: "invoice", EntityID: hd.InvoiceID})
}

func kindWord(k string) string {
	switch k {
	case KindQuote:
		return "quote"
	case KindReminder:
		return "reminder"
	case KindReceipt:
		return "receipt"
	case KindNotice:
		return "disregard notice"
	}
	return "invoice"
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// holdInfo describes the hold behind each of the given email-log ids.
func holdInfo(db *gorm.DB, ids []string) map[string]*HoldInfo {
	out := map[string]*HoldInfo{}
	if len(ids) == 0 || !db.Migrator().HasTable(&EmailHold{}) {
		return out
	}
	var rows []EmailHold
	db.Select("id, state, seconds, send_at, sent_now, cancelled_by, error").Where("id IN ?", ids).Find(&rows)
	now := time.Now()
	for _, r := range rows {
		left := int(r.SendAt.Sub(now).Seconds() + 0.999)
		if left < 0 || r.State != HoldHeld {
			left = 0
		}
		hi := &HoldInfo{State: r.State, Seconds: r.Seconds, SendAt: r.SendAt, SecondsLeft: left, SentNow: r.SentNow, Error: r.Error}
		if r.CancelledBy != "" {
			hi.CancelledBy = notify.UserName(db, r.CancelledBy)
		}
		out[r.ID] = hi
	}
	return out
}

// ---------------------------------------------------------------- endpoints

// loadLog finds email-log entry logId of invoice :id in the caller's org.
func (h *Handler) loadLog(c *gin.Context) (string, *Invoice, *EmailLog, bool) {
	org, okk := h.org(c)
	if !okk {
		return "", nil, nil, false
	}
	inv, err := h.load(h.DB, org, c.Param("id"), false)
	if err != nil {
		failErr(c, err)
		return "", nil, nil, false
	}
	var e EmailLog
	if err := h.DB.Where("id = ? AND organization_id = ? AND invoice_id = ?", c.Param("logId"), org, inv.ID).Take(&e).Error; err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Email not found")
		return "", nil, nil, false
	}
	return org, inv, &e, true
}

// UndoEmail serves POST /invoices/:id/emails/:logId/undo: a held email is
// cancelled — nothing is sent and the document's status is untouched.
func (h *Handler) UndoEmail(c *gin.Context) {
	org, inv, e, okk := h.loadLog(c)
	if !okk {
		return
	}
	user := ctxString(c, "user_id")
	hd, err := h.cancelHold(org, e.ID, user)
	if errors.Is(err, errNotHeld) {
		fail(c, http.StatusConflict, "TOO_LATE", tooLate(h.DB, e.ID))
		return
	}
	if err != nil {
		failErr(c, err)
		return
	}
	h.log(h.DB, inv, "email_undone", strings.TrimSpace(kindTitle(hd.Kind)+" email to "+e.To+" undone before it was sent"), user)
	h.audit(c, org, user, "invoice_email.undo", "invoice", inv.ID, nil, map[string]any{"email_log_id": e.ID, "to": e.To, "cc": e.CC, "kind": e.Kind, "number": inv.Number})
	out, _ := h.load(h.DB, org, inv.ID, true)
	ok(c, http.StatusOK, gin.H{"undone": true, "log_id": e.ID, "invoice": out})
}

// SendEmailNow serves POST /invoices/:id/emails/:logId/send-now: a held email
// goes at once.
func (h *Handler) SendEmailNow(c *gin.Context) {
	org, inv, e, okk := h.loadLog(c)
	if !okk {
		return
	}
	res, err := h.releaseHold(e.ID, true, ctxString(c, "user_id"))
	if errors.Is(err, errNotHeld) {
		fail(c, http.StatusConflict, "NOT_HELD", tooLate(h.DB, e.ID))
		return
	}
	if err != nil {
		failErr(c, err)
		return
	}
	_ = inv
	ok(c, http.StatusOK, gin.H{"emailed": res.Emailed, "log_id": e.ID, "invoice": res.Invoice, "to": res.Built.To, "cc": nonNilList(res.Built.CC)})
	_ = org
}

// tooLate says what happened to an email that is no longer held.
func tooLate(db *gorm.DB, id string) string {
	var hd EmailHold
	if db.Where("id = ?", id).Take(&hd).Error != nil {
		return "This email isn't waiting to be sent."
	}
	switch hd.State {
	case HoldCancelled:
		return "This email was already undone — nothing was sent."
	case HoldSending, HoldSent:
		return "Too late — this email has already gone. You can recall it instead: its links can be switched off and the recipients told to disregard it."
	case HoldFailed, HoldInterrupted:
		return "This email wasn't sent: " + hd.Error
	}
	return "This email isn't waiting to be sent."
}

func kindTitle(k string) string {
	w := kindWord(k)
	return strings.ToUpper(w[:1]) + w[1:]
}

func nonNilList(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}
