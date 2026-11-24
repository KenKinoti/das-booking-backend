package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Error is an API error (status + code + plain message).
type Error struct {
	Status int
	Code   string
	Msg    string
	Field  string
}

func (e *Error) Error() string { return e.Msg }

func errf(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Msg: fmt.Sprintf(format, a...)}
}

var errForbidden = &Error{Status: 403, Code: "FORBIDDEN", Msg: "Only owners and admins can approve messages."}

// Actor is the reviewer doing something.
type Actor struct {
	Org, User, Role, Name string
}

func (s *Service) actorName(a Actor) string {
	if a.Name != "" {
		return a.Name
	}
	return firstNonEmpty(s.UserName(a.User), "Someone")
}

func (s *Service) load(org, id string) (*Item, error) {
	var it Item
	if err := s.db().Where("id = ? AND organization_id = ?", id, org).Limit(1).Find(&it).Error; err != nil {
		return nil, err
	}
	if it.ID == "" {
		return nil, &Error{Status: 404, Code: "NOT_FOUND", Msg: "This message wasn't found."}
	}
	return &it, nil
}

// eventTime keeps the history in order even when the clock stands still
// (test clock) or two steps share a microsecond.
func (s *Service) eventTime() time.Time {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	at := s.now().Truncate(time.Microsecond)
	if !at.After(s.lastEv) {
		at = s.lastEv.Add(time.Microsecond)
	}
	s.lastEv = at
	return at
}

func (s *Service) event(it *Item, action string, a *Actor, text string, ch Changes, note string) {
	ev := Event{ID: uuid.NewString(), OrganizationID: it.OrganizationID, ItemID: it.ID, At: s.eventTime(), Action: action, Text: clip(text, 590), Changes: ch, Note: clip(note, 590)}
	if a != nil {
		ev.UserID, ev.UserName = a.User, s.actorName(*a)
	}
	if err := s.db().Create(&ev).Error; err != nil {
		log.Printf("outbox: event: %v", err)
	}
}

func notPending(it *Item) error {
	switch it.Status {
	case StatusSent:
		return errf(409, "NOT_PENDING", "This message was already sent.")
	case StatusRejected:
		return errf(409, "NOT_PENDING", "This message was rejected, so it won't be sent.")
	case StatusSuperseded:
		return errf(409, "NOT_PENDING", "This message is no longer needed: %s", firstNonEmpty(it.SupersededReason, "the record changed"))
	case StatusSending:
		return errf(409, "BUSY", "This message is being sent right now.")
	}
	return nil
}

// ---------------------------------------------------------------- edits

// Edits are a reviewer's changes (nil = unchanged).
type Edits struct {
	To             *string   `json:"to"`
	CC             *[]string `json:"cc"`
	BCC            *[]string `json:"bcc"`
	Subject        *string   `json:"subject"`
	Message        *string   `json:"message"`
	WhatsAppText   *string   `json:"whatsapp_text"`
	WhatsAppParams *[]string `json:"whatsapp_params"`
}

func (e Edits) empty() bool {
	return e.To == nil && e.CC == nil && e.BCC == nil && e.Subject == nil && e.Message == nil && e.WhatsAppText == nil && e.WhatsAppParams == nil
}

func can(it *Item, field string) bool {
	for _, f := range it.Editable {
		if f == field {
			return true
		}
	}
	return false
}

func cleanList(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[strings.ToLower(v)] {
			continue
		}
		seen[strings.ToLower(v)] = true
		out = append(out, v)
	}
	return out
}

func validEmail(v string) bool {
	a, err := mail.ParseAddress(v)
	return err == nil && strings.Contains(a.Address, "@") && strings.Contains(a.Address[strings.LastIndex(a.Address, "@"):], ".")
}

func validPhone(v string) bool {
	d := 0
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			d++
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')':
		default:
			return false
		}
	}
	return d >= 7 && d <= 15
}

func field(code, f, msg string) *Error { return &Error{Status: 400, Code: code, Msg: msg, Field: f} }

// applyEdits validates e and changes it; it returns what changed.
func applyEdits(it *Item, e Edits) (Changes, error) {
	ch := Changes{}
	str := func(name string, cur *string, nv *string, check func(string) error) error {
		if nv == nil {
			return nil
		}
		if !can(it, name) {
			return field("NOT_EDITABLE", name, "This part of the message can't be changed here.")
		}
		v := *nv
		if name == FieldTo || name == FieldSubject {
			v = strings.TrimSpace(v)
		}
		if check != nil {
			if err := check(v); err != nil {
				return err
			}
		}
		if v != *cur {
			ch[name] = Change{From: *cur, To: v}
			*cur = v
		}
		return nil
	}
	list := func(name string, cur *[]string, nv *[]string, max int) error {
		if nv == nil {
			return nil
		}
		if !can(it, name) {
			return field("NOT_EDITABLE", name, "This part of the message can't be changed here.")
		}
		v := cleanList(*nv)
		if len(v) > max {
			return field("VALIDATION_ERROR", name, fmt.Sprintf("Too many addresses (at most %d)", max))
		}
		for _, a := range v {
			if !validEmail(a) {
				return field("VALIDATION_ERROR", name, "“"+a+"” is not a valid email address")
			}
		}
		if !reflect.DeepEqual(v, *cur) && !(len(v) == 0 && len(*cur) == 0) {
			ch[name] = Change{From: *cur, To: v}
			*cur = v
		}
		return nil
	}
	err := str(FieldTo, &it.To, e.To, func(v string) error {
		if v == "" {
			return field("VALIDATION_ERROR", FieldTo, "Who should it go to?")
		}
		if it.Channel == ChanEmail && !validEmail(v) {
			return field("VALIDATION_ERROR", FieldTo, "“"+v+"” is not a valid email address")
		}
		if it.Channel != ChanEmail && !validPhone(v) {
			return field("VALIDATION_ERROR", FieldTo, "“"+v+"” doesn't look like a phone number")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := list(FieldCC, &it.CC, e.CC, 50); err != nil {
		return nil, err
	}
	if err := list(FieldBCC, &it.BCC, e.BCC, 20); err != nil {
		return nil, err
	}
	if err := str(FieldSubject, &it.Subject, e.Subject, func(v string) error {
		if v == "" {
			return field("VALIDATION_ERROR", FieldSubject, "The subject can't be empty")
		}
		if len([]rune(v)) > 300 {
			return field("VALIDATION_ERROR", FieldSubject, "Keep the subject under 300 characters")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := str(FieldMessage, &it.Message, e.Message, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return field("VALIDATION_ERROR", FieldMessage, "The message can't be empty")
		}
		max := 20000
		if it.Channel == ChanSMS {
			max = 918
		}
		if len([]rune(v)) > max {
			return field("VALIDATION_ERROR", FieldMessage, fmt.Sprintf("Keep the message under %d characters", max))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := str(FieldWAText, &it.WAText, e.WhatsAppText, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return field("VALIDATION_ERROR", FieldWAText, "The WhatsApp text can't be empty")
		}
		if len([]rune(v)) > 4096 {
			return field("VALIDATION_ERROR", FieldWAText, "Keep the WhatsApp text under 4,096 characters")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if e.WhatsAppParams != nil {
		if !can(it, FieldWAParams) {
			return nil, field("NOT_EDITABLE", FieldWAParams, "This part of the message can't be changed here.")
		}
		v := *e.WhatsAppParams
		if it.WAParamCount > 0 && len(v) != it.WAParamCount {
			return nil, field("VALIDATION_ERROR", FieldWAParams, fmt.Sprintf("The template needs exactly %d values", it.WAParamCount))
		}
		for i, p := range v {
			p = strings.TrimSpace(p)
			if p == "" {
				return nil, field("VALIDATION_ERROR", FieldWAParams, fmt.Sprintf("Value %d of the template is empty", i+1))
			}
			if len([]rune(p)) > 1024 {
				return nil, field("VALIDATION_ERROR", FieldWAParams, fmt.Sprintf("Value %d is too long (at most 1,024 characters)", i+1))
			}
			v[i] = p
		}
		if !reflect.DeepEqual(v, it.WAParams) {
			ch[FieldWAParams] = Change{From: it.WAParams, To: v}
			it.WAParams = v
		}
	}
	if len(ch) == 0 {
		return nil, nil
	}
	return ch, nil
}

var fieldWords = map[string]string{FieldTo: "the recipient", FieldCC: "the CC", FieldBCC: "the BCC", FieldSubject: "the subject",
	FieldMessage: "the message", FieldWAText: "the WhatsApp text", FieldWAParams: "the template values"}

func changeText(name string, ch Changes) string {
	parts := []string{}
	for _, f := range []string{FieldTo, FieldCC, FieldBCC, FieldSubject, FieldMessage, FieldWAText, FieldWAParams} {
		if _, ok := ch[f]; ok {
			parts = append(parts, fieldWords[f])
		}
	}
	switch len(parts) {
	case 0:
		return name + " saved it"
	case 1:
		return name + " changed " + parts[0]
	}
	return name + " changed " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// Update saves a reviewer's edits.
func (s *Service) Update(a Actor, id string, e Edits) (*Item, error) {
	if !s.CanApprove(a.Org, a.User, a.Role) {
		return nil, errForbidden
	}
	it, err := s.load(a.Org, id)
	if err != nil {
		return nil, err
	}
	if err := notPending(it); err != nil {
		return nil, err
	}
	return s.saveEdits(a, it, e)
}

func (s *Service) saveEdits(a Actor, it *Item, e Edits) (*Item, error) {
	if e.empty() {
		return it, nil
	}
	prev := it.Status
	ch, err := applyEdits(it, e)
	if err != nil {
		return nil, err
	}
	if ch == nil {
		return it, nil
	}
	upd := map[string]any{"to": it.To, "cc": jsonList(it.CC), "bcc": jsonList(it.BCC), "subject": it.Subject, "message": it.Message, "wa_text": it.WAText,
		"wa_params": jsonList(it.WAParams), "edited": true, "updated_at": s.now()}
	if prev == StatusApproved {
		// Changed after it was approved: it needs approving again.
		upd["status"], upd["approved_by"], upd["approved_by_name"], upd["approved_at"], upd["approve_mode"] = StatusPending, "", "", nil, ""
	}
	res := s.db().Model(&Item{}).Where("id = ? AND status = ?", it.ID, prev).Updates(upd)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, errf(409, "BUSY", "This message changed while you were editing — open it again.")
	}
	it.Edited = true
	s.event(it, "edited", &a, changeText(s.actorName(a), ch), ch, "")
	return s.load(a.Org, it.ID)
}

// ---------------------------------------------------------------- decisions

// Outcome of an approval.
const (
	OutSent       = "sent"
	OutApproved   = "approved"
	OutSuperseded = "superseded"
	OutFailed     = "failed"
)

// Approve approves item id: now (send at once) or for its scheduled time.
func (s *Service) Approve(ctx context.Context, a Actor, id, when string, e *Edits, note string) (*Item, string, error) {
	if !s.CanApprove(a.Org, a.User, a.Role) {
		return nil, "", errForbidden
	}
	it, err := s.load(a.Org, id)
	if err != nil {
		return nil, "", err
	}
	if err := notPending(it); err != nil {
		return nil, "", err
	}
	if e != nil && !e.empty() {
		if it, err = s.saveEdits(a, it, *e); err != nil {
			return nil, "", err
		}
	}
	now := s.now()
	name := s.actorName(a)
	if when == "scheduled" {
		if it.SendAt == nil || !it.SendAt.After(now) {
			return nil, "", errf(409, "NO_SCHEDULED_TIME", "This message has no later send time — approve it to send now.")
		}
		res := s.db().Model(&Item{}).Where("id = ? AND status IN ?", it.ID, []string{StatusPending, StatusFailed, StatusApproved}).Updates(map[string]any{
			"status": StatusApproved, "approved_by": a.User, "approved_by_name": name, "approved_at": now, "approve_mode": "scheduled", "updated_at": now})
		if res.RowsAffected == 0 {
			return nil, "", errf(409, "BUSY", "This message is being sent right now.")
		}
		s.event(it, "approved", &a, name+" approved it for "+s.localTime(it.OrganizationID, *it.SendAt), nil, note)
		out, err := s.load(a.Org, it.ID)
		return out, OutApproved, err
	}
	s.db().Model(&Item{}).Where("id = ? AND status IN ?", it.ID, []string{StatusPending, StatusFailed, StatusApproved}).Updates(map[string]any{
		"approved_by": a.User, "approved_by_name": name, "approved_at": now, "approve_mode": "now", "updated_at": now})
	s.event(it, "approved", &a, name+" approved it to send now", nil, note)
	out, err := s.release(ctx, it.ID, []string{StatusPending, StatusFailed, StatusApproved}, &a)
	if out == nil {
		return nil, "", err
	}
	return out, outcomeOf(out), err
}

func outcomeOf(it *Item) string {
	switch it.Status {
	case StatusSent:
		return OutSent
	case StatusSuperseded:
		return OutSuperseded
	case StatusApproved:
		return OutApproved
	}
	return OutFailed
}

// Retry sends a failed item again.
func (s *Service) Retry(ctx context.Context, a Actor, id string) (*Item, string, error) {
	if !s.CanApprove(a.Org, a.User, a.Role) {
		return nil, "", errForbidden
	}
	it, err := s.load(a.Org, id)
	if err != nil {
		return nil, "", err
	}
	if it.Status != StatusFailed {
		if err := notPending(it); err != nil {
			return nil, "", err
		}
		return nil, "", errf(409, "NOT_FAILED", "Only a message that failed can be tried again — approve this one instead.")
	}
	s.event(it, "retried", &a, s.actorName(a)+" tried it again", nil, "")
	out, err := s.release(ctx, it.ID, []string{StatusFailed}, &a)
	if out == nil {
		return nil, "", err
	}
	return out, outcomeOf(out), err
}

// Reject decides an item won't go (the module records that it wasn't sent).
func (s *Service) Reject(ctx context.Context, a Actor, id, note string) (*Item, error) {
	if !s.CanApprove(a.Org, a.User, a.Role) {
		return nil, errForbidden
	}
	note = strings.TrimSpace(note)
	if len([]rune(note)) > 500 {
		return nil, field("VALIDATION_ERROR", "note", "Keep the note under 500 characters")
	}
	it, err := s.load(a.Org, id)
	if err != nil {
		return nil, err
	}
	if err := notPending(it); err != nil {
		return nil, err
	}
	now := s.now()
	name := s.actorName(a)
	res := s.db().Model(&Item{}).Where("id = ? AND status IN ?", it.ID, []string{StatusPending, StatusFailed, StatusApproved}).Updates(map[string]any{
		"status": StatusRejected, "decided_by": a.User, "decided_by_name": name, "decided_at": now, "decision_note": clip(note, 590), "updated_at": now})
	if res.RowsAffected == 0 {
		return nil, errf(409, "BUSY", "This message is being sent right now.")
	}
	text := name + " rejected it — it won't be sent"
	s.event(it, "rejected", &a, text, nil, note)
	out, _ := s.load(a.Org, it.ID)
	s.closed(ctx, out)
	return out, nil
}

// supersede marks a waiting / approved item as no longer needed.
func (s *Service) supersede(ctx context.Context, it *Item, reason string, from []string) bool {
	now := s.now()
	res := s.db().Model(&Item{}).Where("id = ? AND status IN ?", it.ID, from).Updates(map[string]any{
		"status": StatusSuperseded, "superseded_reason": clip(reason, 490), "decided_at": now, "updated_at": now, "claimed_at": nil})
	if res.RowsAffected == 0 {
		return false
	}
	s.event(it, "superseded", nil, "Not needed any more: "+reason, nil, "")
	out, err := s.load(it.OrganizationID, it.ID)
	if err == nil {
		s.closed(ctx, out)
	}
	return true
}

// closed tells the modules an item won't be sent.
func (s *Service) closed(ctx context.Context, it *Item) {
	if it == nil {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("outbox: %s closed hook: %v", it.Module, r)
			}
		}()
		if r := releaserOf(it.Module); r != nil {
			r.Closed(ctx, it)
		}
	}()
	s.origin(ctx, it)
}

func (s *Service) origin(ctx context.Context, it *Item) {
	if it == nil || it.Origin == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("outbox: %s origin hook: %v", it.Origin, r)
		}
	}()
	if h := originOf(it.Origin); h != nil {
		h(ctx, it)
	}
}

// release claims item id (from one of the given statuses), re-checks it
// with its module and sends it. The answer is the item as it ended up.
func (s *Service) release(ctx context.Context, id string, from []string, a *Actor) (*Item, error) {
	now := s.now()
	res := s.db().Model(&Item{}).Where("id = ? AND status IN ?", id, from).Updates(map[string]any{"status": StatusSending, "claimed_at": now, "attempts": gormExpr("attempts + 1"), "updated_at": now})
	if res.Error != nil {
		return nil, res.Error
	}
	var it Item
	if err := s.db().Where("id = ?", id).Take(&it).Error; err != nil {
		return nil, err
	}
	if res.RowsAffected == 0 {
		return &it, notPending(&it)
	}
	r := releaserOf(it.Module)
	if r == nil {
		s.fail(&it, errors.New("this kind of message can't be sent from this server right now"))
		out, _ := s.load(it.OrganizationID, it.ID)
		return out, errf(502, "SEND_FAILED", "This message couldn't be sent: the %s module isn't running here.", it.Module)
	}
	if why := safeRecheck(ctx, r, &it); why != "" {
		s.supersede(ctx, &it, why, []string{StatusSending})
		out, _ := s.load(it.OrganizationID, it.ID)
		return out, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	result, err := safeSend(sctx, r, &it)
	if err != nil {
		s.fail(&it, err)
		out, _ := s.load(it.OrganizationID, it.ID)
		s.origin(ctx, out)
		return out, &Error{Status: 502, Code: "SEND_FAILED", Msg: "This message couldn't be sent: " + err.Error()}
	}
	done := s.now()
	s.db().Model(&Item{}).Where("id = ?", it.ID).Updates(map[string]any{"status": StatusSent, "sent_at": done, "result": clip(firstNonEmpty(result.Text, "Sent to "+it.To), 590),
		"result_ref": clip(result.Ref, 250), "error": "", "claimed_at": nil, "updated_at": done})
	s.event(&it, "sent", nil, clip(firstNonEmpty(result.Text, "Sent to "+it.To), 590), nil, "")
	out, _ := s.load(it.OrganizationID, it.ID)
	s.origin(ctx, out)
	return out, nil
}

func (s *Service) fail(it *Item, err error) {
	now := s.now()
	s.db().Model(&Item{}).Where("id = ?", it.ID).Updates(map[string]any{"status": StatusFailed, "error": clip(err.Error(), 1900), "claimed_at": nil, "updated_at": now})
	s.event(it, "failed", nil, clip("Not sent: "+err.Error(), 590), nil, "")
}

func safeRecheck(ctx context.Context, r Releaser, it *Item) (why string) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("outbox: recheck %s: %v", it.ID, rec)
			why = ""
		}
	}()
	return r.Recheck(ctx, it)
}

func safeSend(ctx context.Context, r Releaser, it *Item) (res Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("the send stopped unexpectedly (%v)", rec)
		}
	}()
	return r.Send(ctx, it)
}

// ---------------------------------------------------------------- bulk

// BulkResult is one item's outcome in a bulk action.
type BulkResult struct {
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

// Bulk approves or rejects several items, one at a time, never stopping on a failure.
func (s *Service) Bulk(ctx context.Context, a Actor, action string, ids []string, when, note string) ([]BulkResult, error) {
	if !s.CanApprove(a.Org, a.User, a.Role) {
		return nil, errForbidden
	}
	if action != "approve" && action != "reject" {
		return nil, field("VALIDATION_ERROR", "action", "Choose approve or reject")
	}
	if len(ids) == 0 {
		return nil, field("VALIDATION_ERROR", "ids", "Choose at least one message")
	}
	if len(ids) > 100 {
		return nil, field("VALIDATION_ERROR", "ids", "At most 100 messages at a time")
	}
	out := []BulkResult{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] || id == "" {
			continue
		}
		seen[id] = true
		r := BulkResult{ID: id}
		var it *Item
		var err error
		switch action {
		case "reject":
			it, err = s.Reject(ctx, a, id, note)
			r.Outcome = "rejected"
		default:
			w := "now"
			if when == "scheduled" {
				if cur, e := s.load(a.Org, id); e == nil && cur.SendAt != nil && cur.SendAt.After(s.now()) {
					w = "scheduled"
				}
			}
			it, r.Outcome, err = s.Approve(ctx, a, id, w, nil, note)
		}
		if it != nil {
			r.Status = it.Status
		}
		if err != nil {
			r.Error = err.Error()
			if r.Outcome == "" || action == "reject" {
				r.Outcome = "failed"
			}
		} else {
			r.OK = true
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------- module notices

// localTime formats t in the organisation's time zone ("Tue 25 Nov 08:00").
func (s *Service) localTime(org string, t time.Time) string {
	return t.In(s.Loc(org)).Format("Mon 2 Jan 15:04")
}

// jsonList encodes a list column for a map update (serializer:json columns
// aren't serialised by map updates).
func jsonList(l []string) string {
	if l == nil {
		l = []string{}
	}
	b, _ := json.Marshal(l)
	return string(b)
}
