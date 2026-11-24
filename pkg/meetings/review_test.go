package meetings

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/outbox"
)

type capMailer struct {
	mu   sync.Mutex
	sent []InviteMessage
}

func (m *capMailer) Configured() bool { return true }
func (m *capMailer) SendInvite(msg InviteMessage) error {
	m.mu.Lock()
	m.sent = append(m.sent, msg)
	m.mu.Unlock()
	return nil
}
func (m *capMailer) to() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{}
	for _, s := range m.sent {
		out = append(out, s.To)
	}
	return out
}

func reviewDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`DROP SCHEMA IF EXISTS meetings_review_test CASCADE`)
	boot.Exec(`CREATE SCHEMA meetings_review_test`)
	if sq, err := boot.DB(); err == nil {
		sq.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=meetings_review_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if sq, err := db.DB(); err == nil {
		t.Cleanup(func() { sq.Close() })
	}
	db.Exec(`CREATE TABLE users (id varchar(255) primary key, email text, role text, first_name text, last_name text, organization_id varchar(255), is_active boolean default true,
		created_at timestamptz default now(), deleted_at timestamptz)`)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := outbox.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&notify.Notification{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// Guests' invitations wait in Pending review; users of the organisation are
// invited at once; approved, the guest gets the invitation built again;
// an approver's "Send now" invites guests at once; a cancelled meeting's
// waiting invite is no longer needed.
func TestReviewMeetingInvites(t *testing.T) {
	db := reviewDB(t)
	notify.Disabled = true
	t.Cleanup(func() { notify.Disabled = false })
	ob := outbox.Install(&outbox.Service{DB: db, Now: time.Now})
	t.Cleanup(outbox.Uninstall)
	org, owner, staff := uuid.NewString(), uuid.NewString(), uuid.NewString()
	db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id) VALUES (?, 'neema@studio.test', 'owner', 'Neema', 'Owner', ?)`, owner, org)
	db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id) VALUES (?, 'otieno@studio.test', 'staff', 'Otieno', 'Staff', ?)`, staff, org)
	mailer := &capMailer{}
	h := &Handler{DB: db, Mailer: mailer, Now: time.Now}
	h.registerReview()
	start := time.Now().Add(26 * time.Hour)
	m := &Meeting{ID: uuid.NewString(), OrgID: org, Title: "Website review", StartAt: start, EndAt: start.Add(time.Hour), Timezone: "Africa/Nairobi", Status: StatusScheduled}
	db.Omit("Participants", "Transcript").Create(m)
	guest := Participant{ID: uuid.NewString(), MeetingID: m.ID, OrgID: org, Name: "Wanjiru Kamau", Email: "wanjiru@kamau.test"}
	member := Participant{ID: uuid.NewString(), MeetingID: m.ID, OrgID: org, Name: "Otieno", Email: "otieno@studio.test"}
	db.Create(&guest)
	db.Create(&member)
	db.Preload("Participants").Where("id = ?", m.ID).Take(m)

	res := h.sendInvites(m, nil, MethodRequest, nil, reviewOpts{User: staff, Role: "staff", SendNow: true})
	if res.Sent != 1 || res.Waiting != 1 || len(res.ReviewIDs) != 1 || !res.Queued || !strings.Contains(res.Message, "1 waiting for approval") {
		t.Fatalf("invite result: %+v", res)
	}
	if got := mailer.to(); len(got) != 1 || !strings.Contains(got[0], "otieno@studio.test") {
		t.Fatalf("only the member at once: %v", got)
	}
	var p Participant
	db.Where("id = ?", guest.ID).Take(&p)
	if p.InviteStatus != "waiting" || p.ReviewID != res.ReviewIDs[0] || p.InvitedAt != nil {
		t.Fatalf("guest waiting: %+v", p)
	}
	it := outbox.Get(res.ReviewIDs[0])
	if it.Kind != outbox.KindMeetingInvite || !strings.HasPrefix(it.Flag, "Meeting tomorrow") || it.Deadline == nil {
		t.Fatalf("item: %+v", it)
	}
	if pv := ob.DetailOf(context.Background(), it, true).Preview; len(pv.Attachments) != 1 || pv.Attachments[0].Name != "invite.ics" || pv.HTML == "" {
		t.Fatalf("preview: %+v", pv)
	}
	approver := outbox.Actor{Org: org, User: owner, Role: "owner"}
	if _, outcome, err := ob.Approve(context.Background(), approver, it.ID, "now", nil, ""); err != nil || outcome != outbox.OutSent {
		t.Fatalf("approve: %v %s", err, outcome)
	}
	if got := mailer.to(); len(got) != 2 || !strings.Contains(got[1], "wanjiru@kamau.test") || !strings.Contains(mailer.sent[1].ICS, "METHOD:REQUEST") {
		t.Fatalf("guest invited after approval: %v", got)
	}
	db.Where("id = ?", guest.ID).Take(&p)
	if p.InviteStatus != "sent" || p.InvitedAt == nil {
		t.Fatalf("guest sent: %+v", p)
	}
	// An approver's Send now: the guest at once.
	res = h.sendInvites(m, nil, MethodRequest, nil, reviewOpts{User: owner, Role: "owner", SendNow: true})
	if res.Sent != 2 || res.Waiting != 0 {
		t.Fatalf("send now: %+v", res)
	}
	// Waiting, then the meeting is cancelled → no longer needed.
	res = h.sendInvites(m, []Participant{guest}, MethodRequest, nil, reviewOpts{User: staff, Role: "staff"})
	db.Model(&Meeting{}).Where("id = ?", m.ID).Update("status", StatusCancelled)
	if r, _ := ob.RunOnce(context.Background(), time.Second); r.Superseded != 1 {
		t.Fatalf("superseded: %+v", r)
	}
	if it := outbox.Get(res.ReviewIDs[0]); it.Status != outbox.StatusSuperseded || it.SupersededReason != "The meeting was cancelled" {
		t.Fatalf("cancelled meeting: %+v", it)
	}
}
