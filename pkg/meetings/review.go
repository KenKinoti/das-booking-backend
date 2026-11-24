package meetings

// Pending review (pkg/outbox): while the organisation reviews client
// messages, an invitation (or cancellation) to a guest who is not a user of
// the organisation waits for an approver; users of the organisation are
// invited at once. The participant shows invite_status "waiting" until it
// goes; the invitation is built again at release (current time, sequence
// and .ics), so later edits to the meeting are included.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
)

// reviewOpts is who sends the invitations and whether an approver ticked
// "Send now — I've reviewed it".
type reviewOpts struct {
	User, Role string
	SendNow    bool
}

func optsOf(c *gin.Context, sendNow bool) reviewOpts {
	o := reviewOpts{SendNow: sendNow}
	if c != nil {
		o.User, o.Role = c.GetString("user_id"), c.GetString("user_role")
		if o.User == "" {
			if v, ok := c.Get("user_id"); ok && v != nil {
				o.User = fmt.Sprint(v)
			}
		}
		if o.Role == "" {
			if v, ok := c.Get("user_role"); ok && v != nil {
				o.Role = fmt.Sprint(v)
			}
		}
	}
	return o
}

// isMember: the address belongs to a user of the organisation (exempt).
func (h *Handler) isMember(org string, p Participant) bool {
	if p.UserID != "" {
		return true
	}
	var n int64
	h.DB.Table("users").Where("organization_id = ? AND LOWER(email) = ? AND deleted_at IS NULL", org, strings.ToLower(strings.TrimSpace(p.Email))).Count(&n)
	return n > 0
}

type invitePayload struct {
	MeetingID     string `json:"meeting_id"`
	ParticipantID string `json:"participant_id"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	Method        string `json:"method"`
	// Removed: a cancellation to someone taken off the meeting (they are no
	// longer a participant, so their details are kept here).
	Removed bool `json:"removed"`
}

// queueInvite puts one invitation in Pending review.
func (h *Handler) queueInvite(m *Meeting, p Participant, method string, removed bool, o reviewOpts) (*outbox.Item, error) {
	msg := h.buildInvite(m, p, method, nil)
	kind := outbox.KindMeetingInvite
	if method == MethodCancel {
		kind = outbox.KindMeetingCancel
	}
	it := &outbox.Item{ID: uuid.NewString(), OrganizationID: m.OrgID, Kind: kind, Module: "meetings", Channel: outbox.ChanEmail, To: p.Email, ToName: p.Name,
		Subject: msg.Subject, Message: msg.Text, Editable: []string{outbox.FieldSubject},
		Payload:    outbox.Payload(invitePayload{MeetingID: m.ID, ParticipantID: p.ID, Email: p.Email, Name: p.Name, Method: method, Removed: removed}),
		RecordType: "meeting", RecordID: m.ID, RecordLabel: "Meeting: " + m.Title, RecordLink: "/meetings/" + m.ID, CustomerID: p.CustomerID,
		CustomerName: p.Name, CreatedBy: o.User,
		Context: []outbox.ContextLine{{Label: "Meeting", Value: m.Title + " · " + h.fmtWhen(m)}}}
	if method != MethodCancel && m.StartAt.Sub(h.now()) <= 48*time.Hour && m.StartAt.After(h.now()) {
		st := m.StartAt
		it.Deadline = &st
		loc := time.UTC
		if l, err := time.LoadLocation(m.Timezone); err == nil {
			loc = l
		}
		day := "on " + st.In(loc).Format("Mon 2 Jan")
		if st.In(loc).Format("2006-01-02") == h.now().In(loc).Format("2006-01-02") {
			day = "today"
		} else if st.In(loc).Format("2006-01-02") == h.now().In(loc).AddDate(0, 0, 1).Format("2006-01-02") {
			day = "tomorrow"
		}
		it.Flag = "Meeting " + day + " " + st.In(loc).Format("15:04")
	}
	q, err := outbox.Enqueue(nil, it)
	if err != nil {
		return nil, err
	}
	if p.ID != "" && !removed {
		h.DB.Model(&Participant{}).Where("id = ?", p.ID).Updates(map[string]any{"review_id": q.ID, "invite_status": "waiting"})
	}
	return q, nil
}

// ---------------------------------------------------------------- releaser

type reviewReleaser struct{ h *Handler }

func (h *Handler) registerReview() { outbox.Register("meetings", &reviewReleaser{h: h}) }

func (r *reviewReleaser) load(it *outbox.Item) (*Meeting, Participant, invitePayload, error) {
	var p invitePayload
	_ = outbox.PayloadOf(it, &p)
	var m Meeting
	if err := r.h.DB.Preload("Participants").Where("id = ? AND org_id = ?", p.MeetingID, it.OrganizationID).Take(&m).Error; err != nil {
		return nil, Participant{}, p, errors.New("the meeting was deleted")
	}
	part := Participant{ID: p.ParticipantID, MeetingID: m.ID, OrgID: m.OrgID, Email: p.Email, Name: p.Name}
	found := false
	for _, x := range m.Participants {
		if x.ID == p.ParticipantID {
			part, found = x, true
		}
	}
	if !found && !p.Removed && p.Method != MethodCancel {
		return &m, part, p, errors.New("they were taken off the meeting")
	}
	return &m, part, p, nil
}

// Recheck: is the invitation still right?
func (r *reviewReleaser) Recheck(ctx context.Context, it *outbox.Item) string {
	m, _, p, err := r.load(it)
	if err != nil {
		msg := err.Error()
		return strings.ToUpper(msg[:1]) + msg[1:]
	}
	if p.Method != MethodCancel {
		if m.Status == StatusCancelled {
			return "The meeting was cancelled"
		}
		if m.EndAt.Before(r.h.now()) {
			return "The meeting is over"
		}
	}
	return ""
}

// Send builds the invitation again and emails it.
func (r *reviewReleaser) Send(ctx context.Context, it *outbox.Item) (outbox.Result, error) {
	h := r.h
	m, part, p, err := r.load(it)
	if err != nil {
		return outbox.Result{}, err
	}
	mailer := h.mailerFor(m.OrgID)
	if mailer == nil || !mailer.Configured() {
		return outbox.Result{}, errors.New("email (SMTP) is not set up for this organisation")
	}
	var attendees []Participant
	if p.Method == MethodCancel && p.Removed {
		attendees = []Participant{part}
	}
	msg := h.buildInvite(m, part, p.Method, attendees)
	msg.To = it.To
	if strings.TrimSpace(it.Subject) != "" {
		msg.Subject = it.Subject
	}
	if err := mailer.SendInvite(msg); err != nil {
		return outbox.Result{}, err
	}
	now := h.now()
	if p.Method != MethodCancel && part.ID != "" {
		h.DB.Model(&Participant{}).Where("id = ?", part.ID).Updates(map[string]any{"invited_at": now, "invite_status": "sent"})
		h.DB.Model(&Meeting{}).Where("id = ?", m.ID).Update("invites_sent_at", now)
	} else if part.ID != "" {
		h.DB.Model(&Participant{}).Where("id = ?", part.ID).Update("invite_status", "sent")
	}
	return outbox.Result{Text: "Sent to " + it.To}, nil
}

// Preview renders the invitation as the guest gets it.
func (r *reviewReleaser) Preview(ctx context.Context, it *outbox.Item) outbox.Preview {
	pv := outbox.Preview{Subject: it.Subject, To: it.To, Text: it.Message}
	m, part, p, err := r.load(it)
	if err != nil {
		pv.Error = "The preview couldn't be made: " + err.Error()
		return pv
	}
	msg := r.h.buildInvite(m, part, p.Method, nil)
	pv.HTML, pv.Text = msg.HTML, msg.Text
	if strings.TrimSpace(it.Subject) != "" {
		pv.Subject = it.Subject
	}
	pv.Attachments = []outbox.Attachment{{Name: "invite.ics", Kind: "ics", Note: "The calendar invitation is made again when it is sent", URL: "/meetings/" + m.ID + "/ics"}}
	return pv
}

// Closed: the participant is no longer "waiting".
func (r *reviewReleaser) Closed(ctx context.Context, it *outbox.Item) {
	var p invitePayload
	_ = outbox.PayloadOf(it, &p)
	if p.ParticipantID == "" {
		return
	}
	st := "rejected"
	if it.Status == outbox.StatusSuperseded {
		st = ""
	}
	r.h.DB.Model(&Participant{}).Where("id = ? AND review_id = ?", p.ParticipantID, it.ID).Update("invite_status", st)
}
