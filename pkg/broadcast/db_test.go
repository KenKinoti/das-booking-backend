package broadcast_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	bc "github.com/kenkinoti/gofiber-das-crm-backend/pkg/broadcast"
	ep "github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/occasions"
)

// ---------------------------------------------------------------- fixture

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`DROP SCHEMA IF EXISTS broadcast_test CASCADE`)
	boot.Exec(`CREATE SCHEMA broadcast_test`)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=broadcast_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) primary key, name text, business_type varchar(100) default 'general', email text, phone text, website text,
			country_code text, currency text, timezone text, region_source text, address_country text, created_at timestamptz default now(), deleted_at timestamptz)`,
		`CREATE TABLE users (id varchar(255) primary key, email text, role text, first_name text, last_name text, organization_id varchar(255), is_active boolean default true,
			last_login_at timestamptz, created_at timestamptz default now(), deleted_at timestamptz)`,
		`CREATE TABLE audit_logs (id varchar(36) primary key, organization_id varchar(36), user_id varchar(36), action varchar(100), entity_type varchar(50), entity_id varchar(64),
			old_values jsonb, new_values jsonb, ip_address varchar(45), user_agent varchar(500), created_at timestamptz)`,
		`CREATE TABLE plan_subscriptions (organization_id varchar(255) primary key, tier varchar(20))`,
		`CREATE TABLE platform_org_status (organization_id varchar(255) primary key, status varchar(20))`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := notify.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := industry.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := ep.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := bc.Migrate(db); err != nil {
		t.Fatal(err)
	}
	ind := industry.NewService(db)
	ep.RoleOf = func(o, u, legacy string) (string, string, bool) {
		r, err := ind.RoleFor(o, u, legacy)
		if err != nil || r == nil {
			return "", "", false
		}
		return r.Base, r.Name, true
	}
	t.Cleanup(func() {
		ep.RoleOf, ep.Can = nil, nil
		bc.SetClockOffset(0)
		bc.SetNow(nil)
	})
	return db
}

type world struct {
	db  *gorm.DB
	ind *industry.Service
	// organisations
	clubKE, clubKE2, salonKE, clubAU, platformOrg, suspended string
	// people
	kOwner, kManager, kBartender, k2Owner, k2Admin, salonOwner, salonStaff, auOwner, superAdmin, suspendedOwner, staleOwner string
}

func org(t *testing.T, w *world, name, bt, country, tier string) string {
	t.Helper()
	id := uuid.NewString()
	if err := w.db.Exec(`INSERT INTO organizations (id, name, business_type, country_code, currency, timezone) VALUES (?, ?, ?, ?, 'KES', 'Africa/Nairobi')`, id, name, bt, country).Error; err != nil {
		t.Fatal(err)
	}
	if tier != "" {
		w.db.Exec(`INSERT INTO plan_subscriptions (organization_id, tier) VALUES (?, ?)`, id, tier)
	}
	return id
}

func person(t *testing.T, w *world, orgID, email, role, first string, lastLogin *time.Time) string {
	t.Helper()
	id := uuid.NewString()
	if err := w.db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id, last_login_at) VALUES (?, ?, ?, ?, 'Test', ?, ?)`,
		id, email, role, first, orgID, lastLogin).Error; err != nil {
		t.Fatal(err)
	}
	return id
}

func newWorld(t *testing.T) *world {
	db := testDB(t)
	w := &world{db: db, ind: industry.NewService(db)}
	recent := time.Now().AddDate(0, 0, -2)
	old := time.Now().AddDate(0, 0, -60)
	// Two nightclubs in Kenya (business type through pkg/industry), a salon in Kenya, a club in Australia.
	w.clubKE = org(t, w, "Club Habari", "general", "KE", "sme")
	w.kOwner = person(t, w, w.clubKE, "peter@habari.test", "admin", "Peter", &recent)
	if err := w.ind.OnOrgCreated(w.clubKE, "nightclub", "test"); err != nil {
		t.Fatal(err)
	}
	w.kManager = person(t, w, w.clubKE, "mary@habari.test", "staff", "Mary", &recent)
	w.kBartender = person(t, w, w.clubKE, "brian@habari.test", "staff", "Brian", &recent)
	for id, role := range map[string]string{w.kManager: "bar_manager", w.kBartender: "bartender"} {
		if _, _, err := w.ind.Assign(w.clubKE, id, role, "test"); err != nil {
			t.Fatal(err)
		}
	}
	w.clubKE2 = org(t, w, "Kilele Lounge", "lounge", "KE", "large") // legacy business type "lounge" → nightclub
	w.k2Owner = person(t, w, w.clubKE2, "olive@kilele.test", "owner", "Olive", &recent)
	w.k2Admin = person(t, w, w.clubKE2, "adam@kilele.test", "org_admin", "Adam", &old)
	w.staleOwner = person(t, w, w.clubKE2, "", "admin", "Nomail", nil)
	w.salonKE = org(t, w, "Nywele Salon", "salon", "KE", "small")
	w.salonOwner = person(t, w, w.salonKE, "sally@nywele.test", "admin", "Sally", &recent)
	w.salonStaff = person(t, w, w.salonKE, "sam@nywele.test", "staff", "Sam", &recent)
	w.clubAU = org(t, w, "Bondi Bar", "bar", "AU", "sme")
	w.auOwner = person(t, w, w.clubAU, "bruce@bondi.test", "admin", "Bruce", &recent)
	w.platformOrg = org(t, w, "DASYIN", "general", "KE", "large")
	w.superAdmin = person(t, w, w.platformOrg, "kennedy@dasyin.test", "super_admin", "Kennedy", &recent)
	w.suspended = org(t, w, "Closed Club", "nightclub", "KE", "sme")
	w.suspendedOwner = person(t, w, w.suspended, "gone@closed.test", "admin", "Gone", &recent)
	db.Exec(`INSERT INTO platform_org_status (organization_id, status) VALUES (?, 'suspended')`, w.suspended)
	// An inactive owner and a deleted one never receive anything.
	db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id, is_active) VALUES (?, 'off@habari.test', 'admin', 'Off', 'X', ?, false)`, uuid.NewString(), w.clubKE)
	db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id, deleted_at) VALUES (?, 'del@habari.test', 'admin', 'Del', 'X', ?, now())`, uuid.NewString(), w.clubKE)
	return w
}

type outbox struct {
	mu   sync.Mutex
	msgs []mail.Message
	fail map[string]bool
}

func (o *outbox) send(_ context.Context, m mail.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, to := range m.To {
		if o.fail[to] {
			return &mail.Error{Code: "RECIPIENT_REJECTED", Msg: "rejected " + to}
		}
	}
	o.msgs = append(o.msgs, m)
	return nil
}

func (o *outbox) to(addr string) []mail.Message {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := []mail.Message{}
	for _, m := range o.msgs {
		for _, t := range m.To {
			if t == addr {
				out = append(out, m)
			}
		}
	}
	return out
}

func (o *outbox) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.msgs)
}

func service(w *world) (*bc.Service, *outbox) {
	box := &outbox{fail: map[string]bool{}}
	s := bc.New(w.db)
	s.Secret, s.AppURL, s.APIURL = []byte("test-secret"), "https://app.test", "https://api.test"
	s.SendMail = box.send
	s.Occasions = func(from, to time.Time, country string) []occasions.Occasion { return nil }
	return s, box
}

func emails(rs []bc.Recipient) map[string]bool {
	out := map[string]bool{}
	for _, r := range rs {
		out[r.Email] = true
	}
	return out
}

func newAnnouncement(t *testing.T, s *bc.Service, in bc.Input) *bc.Announcement {
	t.Helper()
	a := &bc.Announcement{ID: uuid.NewString(), Status: bc.StatusDraft, CreatedAt: bc.Now(), UpdatedAt: bc.Now()}
	if err := bc.Apply(a, in); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(a).Error; err != nil {
		t.Fatal(err)
	}
	return a
}

func reload(t *testing.T, db *gorm.DB, id string) *bc.Announcement {
	t.Helper()
	var a bc.Announcement
	if err := db.First(&a, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	return &a
}

func deliveryOf(t *testing.T, db *gorm.DB, ann, user string) bc.Delivery {
	t.Helper()
	var d bc.Delivery
	if err := db.Where("announcement_id = ? AND user_id = ? AND NOT is_test", ann, user).Order("occurrence_at DESC").First(&d).Error; err != nil {
		t.Fatalf("no delivery for %s: %v", user, err)
	}
	return d
}

// ---------------------------------------------------------------- audience

func TestAudienceFilters(t *testing.T) {
	w := newWorld(t)
	s, _ := service(w)
	got := func(a bc.Audience, kind string) map[string]bool { return emails(s.Resolve(a, kind)) }

	// Everyone, super admins left out: not the suspended club, inactive or deleted people.
	all := got(bc.Audience{ExcludeSuperAdmins: true}, bc.KindUpdate)
	if len(all) != 9 || all["kennedy@dasyin.test"] || all["gone@closed.test"] || all["off@habari.test"] || all["del@habari.test"] {
		t.Fatalf("all %v", all)
	}
	if !got(bc.Audience{}, bc.KindUpdate)["kennedy@dasyin.test"] {
		t.Fatal("super admins are included unless excluded")
	}
	// Owners of nightclubs in Kenya: business-type registry and legacy "lounge" both count; the Bondi Bar is in Australia.
	club := got(bc.Audience{BusinessTypes: []string{"nightclub"}, Countries: []string{"ke"}, RoleLevel: bc.LevelOwners, ExcludeSuperAdmins: true}, bc.KindUrgent)
	want := []string{"peter@habari.test", "olive@kilele.test", "adam@kilele.test", ""}
	if len(club) != len(want) {
		t.Fatalf("nightclub owners in KE %v", club)
	}
	for _, e := range want {
		if !club[e] {
			t.Fatalf("missing %q in %v", e, club)
		}
	}
	// Managers and above: the bar manager (business-type role) joins; the bartender does not.
	mgr := got(bc.Audience{OrganizationIDs: []string{w.clubKE}, RoleLevel: bc.LevelManagers}, bc.KindUpdate)
	if len(mgr) != 2 || !mgr["mary@habari.test"] || mgr["brian@habari.test"] {
		t.Fatalf("managers %v", mgr)
	}
	// Plan tiers and countries.
	small := got(bc.Audience{PlanTiers: []string{"SMALL"}}, bc.KindUpdate)
	if len(small) != 2 || !small["sally@nywele.test"] {
		t.Fatalf("small tier %v", small)
	}
	au := got(bc.Audience{Countries: []string{"AU"}}, bc.KindUpdate)
	if len(au) != 1 || !au["bruce@bondi.test"] {
		t.Fatalf("australia %v", au)
	}
	// Active in the last 7 days: Adam (60 days) and the never-signed-in owner drop out.
	active := got(bc.Audience{OrganizationIDs: []string{w.clubKE2}, ActiveWithinDays: 7}, bc.KindUpdate)
	if len(active) != 1 || !active["olive@kilele.test"] {
		t.Fatalf("active %v", active)
	}
	// Preview counts match.
	p := s.PreviewAudience(bc.Audience{BusinessTypes: []string{"nightclub"}, Countries: []string{"KE"}, RoleLevel: bc.LevelOwners, ExcludeSuperAdmins: true}, bc.KindUrgent)
	if p.Count != 4 || p.Organizations != 2 || p.WithEmail != 3 || p.ByLevel["owner"] != 4 {
		t.Fatalf("preview %+v", p)
	}
	// Unsubscribed people are left out of offers only.
	if err := s.Unsubscribe(w.salonOwner, "", "app"); err != nil {
		t.Fatal(err)
	}
	if got(bc.Audience{OrganizationIDs: []string{w.salonKE}}, bc.KindPromotion)["sally@nywele.test"] {
		t.Fatal("offer to an unsubscribed person")
	}
	if !got(bc.Audience{OrganizationIDs: []string{w.salonKE}}, bc.KindUpdate)["sally@nywele.test"] {
		t.Fatal("unsubscribing from offers stopped updates")
	}
	if p := s.PreviewAudience(bc.Audience{OrganizationIDs: []string{w.salonKE}}, bc.KindPromotion); p.Unsubscribed != 1 || p.Count != 1 {
		t.Fatalf("offer preview %+v", p)
	}
}

// ---------------------------------------------------------------- sending + escalation

func TestUrgentEscalationOneHour(t *testing.T) {
	w := newWorld(t)
	s, box := service(w)
	ctx := context.Background()
	a := newAnnouncement(t, s, bc.Input{Title: "Card payments down", Body: "<p>M-Pesa works; cards are down until 14:00.</p>", Kind: bc.KindUrgent,
		Audience: bc.Audience{BusinessTypes: []string{"nightclub"}, Countries: []string{"KE"}, RoleLevel: bc.LevelOwners, ExcludeSuperAdmins: true}})
	n, err := s.SendNow(ctx, a, "sa")
	if err != nil || n != 4 || a.Status != bc.StatusSent {
		t.Fatalf("send now %d %v %s", n, err, a.Status)
	}
	// In-app: a notification per person, linked to its delivery.
	var notes []notify.Notification
	w.db.Where("type = ?", "announcement.urgent").Find(&notes)
	if len(notes) != 4 || notes[0].EntityType != "announcement" || !strings.HasPrefix(notes[0].Link, "/dashboard?announcement=") || notes[0].Severity != notify.Error {
		t.Fatalf("notifications %d %+v", len(notes), notes)
	}
	// Banner for the owners.
	items := s.Mine(w.kOwner, 10)
	if len(items) != 1 || !items[0].Banner || items[0].Title != "Card payments down" {
		t.Fatalf("mine %+v", items)
	}
	// Peter reads it in the app after 30 minutes; Olive reads it in the bell (notification centre).
	peter, olive := deliveryOf(t, w.db, a.ID, w.kOwner), deliveryOf(t, w.db, a.ID, w.k2Owner)
	bc.SetClockOffset(30 * time.Minute)
	if _, err := s.Mark(w.kOwner, peter.ID, "read"); err != nil {
		t.Fatal(err)
	}
	w.db.Model(&notify.Notification{}).Where("entity_id = ?", olive.ID).Update("read_at", time.Now())
	// Before the hour: nothing.
	bc.SetClockOffset(59 * time.Minute)
	if sent, _, _ := s.Escalate(ctx, 100); sent != 0 || box.count() != 0 {
		t.Fatalf("emailed before the hour: %d", sent)
	}
	// After the hour: only Adam (unread, has an email); the owner without email is skipped.
	bc.SetClockOffset(61 * time.Minute)
	sent, failed, skipped := s.Escalate(ctx, 100)
	if sent != 1 || failed != 0 || skipped != 1 || len(box.to("adam@kilele.test")) != 1 || len(box.to("peter@habari.test")) != 0 || len(box.to("olive@kilele.test")) != 0 {
		t.Fatalf("escalation sent %d failed %d skipped %d, box %d", sent, failed, skipped, box.count())
	}
	m := box.to("adam@kilele.test")[0]
	if m.Subject != "Urgent: Card payments down" || !strings.Contains(m.HTML, "M-Pesa works") {
		t.Fatalf("email %s", m.Subject)
	}
	// Idempotent: another pass sends nothing more.
	if sent, _, _ := s.Escalate(ctx, 100); sent != 0 || box.count() != 1 {
		t.Fatal("emailed twice")
	}
	if d := deliveryOf(t, w.db, a.ID, w.kOwner); d.EmailStatus != bc.EmailNotNeeded {
		t.Fatalf("peter %s", d.EmailStatus)
	}
	if d := deliveryOf(t, w.db, a.ID, w.k2Owner); d.EmailStatus != bc.EmailNotNeeded || d.ReadAt == nil {
		t.Fatalf("olive (read in the bell) %s", d.EmailStatus)
	}
	if d := deliveryOf(t, w.db, a.ID, w.staleOwner); d.EmailStatus != bc.EmailSkipped || d.EmailNote != "No email address" {
		t.Fatalf("no email %s %s", d.EmailStatus, d.EmailNote)
	}
	// The email link marks read + clicked.
	adam := deliveryOf(t, w.db, a.ID, w.k2Admin)
	id, target, err := s.ParseToken(s.Token(bc.PurposeOpen, adam.ID, "view"), bc.PurposeOpen)
	if err != nil || id != adam.ID || target != "view" {
		t.Fatal(err)
	}
	if _, err := s.Mark(w.k2Admin, adam.ID, "click"); err != nil {
		t.Fatal(err)
	}
	st := s.StatsFor([]string{a.ID})[a.ID]
	if st.Recipients != 4 || st.Read != 3 || st.Clicked != 1 || st.Emailed != 1 || st.EmailSkipped != 1 || st.ReadPct != 75 {
		t.Fatalf("stats %+v", st)
	}
	csv, err := s.ExportCSV(a)
	if err != nil || strings.Count(string(csv), "\n") != 5 || !strings.Contains(string(csv), "adam@kilele.test") {
		t.Fatalf("csv %v\n%s", err, csv)
	}
}

func TestNormalEscalationThreeHoursAndPrefs(t *testing.T) {
	w := newWorld(t)
	s, box := service(w)
	ctx := context.Background()
	// The salon's staff member turned "Updates from DASYIN" off for themselves.
	if err := ep.SetSelf(w.db, w.salonKE, w.salonStaff, nil, map[string]bool{ep.PlatformUpdates: false}); err != nil {
		t.Fatal(err)
	}
	a := newAnnouncement(t, s, bc.Input{Title: "New: stock counts on your phone", Body: "Count stock from your phone.", Kind: bc.KindUpdate,
		Audience: bc.Audience{OrganizationIDs: []string{w.salonKE}}})
	if n, err := s.SendNow(ctx, a, "sa"); err != nil || n != 2 {
		t.Fatalf("send %d %v", n, err)
	}
	bc.SetClockOffset(2*time.Hour + 59*time.Minute)
	if sent, _, _ := s.Escalate(ctx, 100); sent != 0 {
		t.Fatal("emailed before 3 hours")
	}
	bc.SetClockOffset(3*time.Hour + time.Minute)
	sent, _, skipped := s.Escalate(ctx, 100)
	if sent != 1 || skipped != 1 || len(box.to("sally@nywele.test")) != 1 || len(box.to("sam@nywele.test")) != 0 {
		t.Fatalf("3h: sent %d skipped %d", sent, skipped)
	}
	if d := deliveryOf(t, w.db, a.ID, w.salonStaff); d.EmailStatus != bc.EmailSkipped || !strings.Contains(d.EmailNote, "Updates from DASYIN") {
		t.Fatalf("prefs skip %s %s", d.EmailStatus, d.EmailNote)
	}
	if m := box.to("sally@nywele.test")[0]; !strings.Contains(m.HTML, "My profile") {
		t.Fatal("update email should say how to turn them off")
	}
	// Urgent service notices go even to people who turned the emails off.
	bc.SetClockOffset(0)
	u := newAnnouncement(t, s, bc.Input{Title: "Maintenance tonight", Body: "Down 23:00–23:30.", Kind: bc.KindUrgent, Audience: bc.Audience{OrganizationIDs: []string{w.salonKE}}})
	if _, err := s.SendNow(ctx, u, "sa"); err != nil {
		t.Fatal(err)
	}
	bc.SetClockOffset(61 * time.Minute)
	s.Escalate(ctx, 100)
	if len(box.to("sam@nywele.test")) != 1 {
		t.Fatal("urgent notice did not reach someone with update emails off")
	}
	// "Never email" and 0-hour delays.
	bc.SetClockOffset(0)
	never, zero := bc.NeverEmail, 0
	nv := newAnnouncement(t, s, bc.Input{Title: "In-app only", Body: "x", EmailDelayMinutes: &never, Audience: bc.Audience{OrganizationIDs: []string{w.clubAU}}})
	s.SendNow(ctx, nv, "sa")
	if d := deliveryOf(t, w.db, nv.ID, w.auOwner); d.EmailStatus != bc.EmailNever {
		t.Fatalf("never %s", d.EmailStatus)
	}
	zr := newAnnouncement(t, s, bc.Input{Title: "Right away", Body: "x", EmailDelayMinutes: &zero, Audience: bc.Audience{OrganizationIDs: []string{w.clubAU}}})
	s.SendNow(ctx, zr, "sa")
	if len(box.to("bruce@bondi.test")) != 1 {
		t.Fatal("0-hour delay did not email at once")
	}
	// A failed send is recorded and can be retried.
	box.fail["bruce@bondi.test"] = true
	f := newAnnouncement(t, s, bc.Input{Title: "Will fail", Body: "x", EmailDelayMinutes: &zero, Audience: bc.Audience{OrganizationIDs: []string{w.clubAU}}})
	s.SendNow(ctx, f, "sa")
	if d := deliveryOf(t, w.db, f.ID, w.auOwner); d.EmailStatus != bc.EmailFailed || !strings.Contains(d.EmailNote, "rejected") {
		t.Fatalf("failed %s %s", d.EmailStatus, d.EmailNote)
	}
	delete(box.fail, "bruce@bondi.test")
	if n := s.RetryFailed(f); n != 1 {
		t.Fatalf("retry %d", n)
	}
	s.Escalate(ctx, 100)
	if d := deliveryOf(t, w.db, f.ID, w.auOwner); d.EmailStatus != bc.EmailSent {
		t.Fatalf("after retry %s", d.EmailStatus)
	}
}

func TestPromotionUnsubscribe(t *testing.T) {
	w := newWorld(t)
	s, box := service(w)
	ctx := context.Background()
	zero := 0
	in := bc.Input{Title: "20% off the Large plan", Body: "Upgrade before Friday.", Kind: bc.KindPromotion, CTALabel: "See plans", CTAURL: "/plan",
		EmailDelayMinutes: &zero, Audience: bc.Audience{OrganizationIDs: []string{w.clubAU}}}
	a := newAnnouncement(t, s, in)
	if _, err := s.SendNow(ctx, a, "sa"); err != nil {
		t.Fatal(err)
	}
	msgs := box.to("bruce@bondi.test")
	if len(msgs) != 1 || !strings.Contains(msgs[0].HTML, "Unsubscribe from DASYIN offers") {
		var ds []bc.Delivery
		w.db.Find(&ds)
		t.Fatalf("offer email %d %+v", len(msgs), ds)
	}
	m := regexp.MustCompile(`https://app\.test/unsubscribe\?t=([^"&\s]+)`).FindStringSubmatch(msgs[0].HTML)
	if m == nil {
		t.Fatal("no unsubscribe link")
	}
	// The public endpoints: info, then one-click unsubscribe.
	api := server(t, s, w)
	tok := strings.ReplaceAll(m[1], "%3D", "=")
	code, out := api("GET", "/api/v1/public/announcements/unsubscribe?t="+m[1], "", "", "")
	if code != 200 || data(out)["unsubscribed"] != false || data(out)["email"] != "br••••@bondi.test" {
		t.Fatalf("info %d %v", code, out)
	}
	code, out = api("POST", "/api/v1/public/announcements/unsubscribe", `{"t":"`+tok+`"}`, "", "")
	if code != 200 || data(out)["unsubscribed"] != true {
		t.Fatalf("unsubscribe %d %v", code, out)
	}
	if code, _ := api("POST", "/api/v1/public/announcements/unsubscribe", `{"t":"`+tok+`x"}`, "", ""); code != 400 {
		t.Fatalf("tampered token %d", code)
	}
	// No further offers: not in the app, not by email; existing offers hidden.
	if items := s.Mine(w.auOwner, 10); len(items) != 0 {
		t.Fatalf("offers still showing %+v", items)
	}
	b := newAnnouncement(t, s, in)
	if n, _ := s.SendNow(ctx, b, "sa"); n != 0 {
		t.Fatalf("offer sent to an unsubscribed person: %d", n)
	}
	if len(box.to("bruce@bondi.test")) != 1 {
		t.Fatal("offer emailed after unsubscribing")
	}
	// A queued offer email is dropped too.
	bc.SetClockOffset(0)
	three := 180
	in2 := in
	in2.EmailDelayMinutes = &three
	w.db.Where("user_id = ?", w.auOwner).Delete(&bc.Unsubscribe{})
	c := newAnnouncement(t, s, in2)
	s.SendNow(ctx, c, "sa")
	s.Unsubscribe(w.auOwner, "", "app")
	bc.SetClockOffset(4 * time.Hour)
	s.Escalate(ctx, 100)
	if d := deliveryOf(t, w.db, c.ID, w.auOwner); d.EmailStatus != bc.EmailSkipped {
		t.Fatalf("queued offer %s", d.EmailStatus)
	}
	// Updates still arrive.
	bc.SetClockOffset(0)
	u := newAnnouncement(t, s, bc.Input{Title: "Update", Body: "x", Audience: bc.Audience{OrganizationIDs: []string{w.clubAU}}})
	if n, _ := s.SendNow(ctx, u, "sa"); n != 1 {
		t.Fatal("update blocked by the offers unsubscribe")
	}
	st := s.StatsFor([]string{a.ID})[a.ID]
	if st.Unsubscribed != 1 {
		t.Fatalf("stats unsubscribed %+v", st)
	}
	// Resubscribe through the link.
	if code, out := api("POST", "/api/v1/public/announcements/resubscribe", `{"t":"`+tok+`"}`, "", ""); code != 200 || data(out)["unsubscribed"] != false {
		t.Fatalf("resubscribe %d %v", code, out)
	}
	if s.Unsubscribed(w.auOwner) {
		t.Fatal("still unsubscribed")
	}
}

// ---------------------------------------------------------------- scheduling

func TestSchedulingRecurrenceAndIdempotentScheduler(t *testing.T) {
	w := newWorld(t)
	s, _ := service(w)
	ctx := context.Background()
	// Fix the wall clock: Monday 4 January 2027, 07:00 in Nairobi.
	base, _ := bc.ParseLocal("2027-01-04T07:00", "Africa/Nairobi")
	bc.SetNow(func() time.Time { return base })
	a := newAnnouncement(t, s, bc.Input{Title: "Weekly tip", Body: "Tip of the week.", Kind: bc.KindUpdate, Timezone: "Africa/Nairobi",
		LocalSendAt: "2027-01-04T08:00", Recurrence: bc.RepeatWeekly, RepeatUntil: "2027-01-18", Audience: bc.Audience{OrganizationIDs: []string{w.clubAU}}})
	if err := s.Schedule(a, "sa"); err != nil {
		t.Fatal(err)
	}
	if a.NextSendAt.Format(time.RFC3339) != "2027-01-04T05:00:00Z" {
		t.Fatalf("next %s", a.NextSendAt)
	}
	// Not yet due.
	if r, err := s.RunOnce(ctx, 5*time.Second); err != nil || r.People != 0 {
		t.Fatalf("early %+v %v", r, err)
	}
	// 08:01: sent once, however many passes run — even concurrently.
	bc.SetClockOffset(61 * time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.RunOnce(ctx, 5*time.Second) }()
	}
	wg.Wait()
	s.RunOnce(ctx, 5*time.Second)
	var n int64
	w.db.Model(&bc.Delivery{}).Where("announcement_id = ?", a.ID).Count(&n)
	var notes int64
	w.db.Model(&notify.Notification{}).Where("type = 'announcement.update'").Count(&notes)
	if n != 1 || notes != 1 {
		t.Fatalf("deliveries %d notifications %d", n, notes)
	}
	a = reload(t, w.db, a.ID)
	if a.Status != bc.StatusScheduled || a.Occurrences != 1 || bc.FormatLocal(*a.NextSendAt, "Africa/Nairobi") != "2027-01-11T08:00" {
		t.Fatalf("after first %s %d %v", a.Status, a.Occurrences, a.NextSendAt)
	}
	// The server was down for a week: the missed 11 Jan send is skipped, not sent in a burst.
	bc.SetClockOffset(14*24*time.Hour + 2*time.Hour) // 18 Jan 09:00
	w.db.Model(&bc.Announcement{}).Where("id = ?", a.ID).Update("next_send_at", a.NextSendAt)
	s.RunOnce(ctx, 5*time.Second)
	a = reload(t, w.db, a.ID)
	if a.Occurrences != 2 || a.Status != bc.StatusSent || a.NextSendAt != nil {
		t.Fatalf("end of series %s %d %v", a.Status, a.Occurrences, a.NextSendAt)
	}
	// The person sees the newest occurrence only.
	if items := s.Mine(w.auOwner, 10); len(items) != 1 {
		t.Fatalf("mine %d", len(items))
	}
	// Monthly in Sydney at 09:00 local.
	bc.SetClockOffset(0)
	m := newAnnouncement(t, s, bc.Input{Title: "Month end", Body: "Close the month.", Timezone: "Australia/Sydney", LocalSendAt: "2027-01-31T09:00", Recurrence: bc.RepeatMonthly,
		Audience: bc.Audience{Countries: []string{"AU"}}})
	if err := s.Schedule(m, "sa"); err != nil {
		t.Fatal(err)
	}
	if m.NextSendAt.Format(time.RFC3339) != "2027-01-30T22:00:00Z" {
		t.Fatalf("sydney %s", m.NextSendAt)
	}
	// Scheduling in the past is refused; cancelling stops it.
	p := newAnnouncement(t, s, bc.Input{Title: "Past", Body: "x", LocalSendAt: "2027-01-01T08:00"})
	if err := s.Schedule(p, "sa"); err == nil {
		t.Fatal("scheduled in the past")
	}
	if err := s.Cancel(m, "sa"); err != nil || m.Status != bc.StatusCancelled {
		t.Fatal("cancel")
	}
	bc.SetClockOffset(30 * 24 * time.Hour)
	if r, _ := s.RunOnce(ctx, 5*time.Second); r.Announcements != 0 {
		t.Fatal("a cancelled announcement was sent")
	}
	// Expiry hides it in the app and stops the emails.
	bc.SetClockOffset(0)
	e := newAnnouncement(t, s, bc.Input{Title: "Short lived", Body: "x", LocalExpiresAt: "2027-01-04T09:00", Audience: bc.Audience{OrganizationIDs: []string{w.salonKE}}})
	if _, err := s.SendNow(ctx, e, "sa"); err != nil {
		t.Fatal(err)
	}
	if len(s.Mine(w.salonOwner, 10)) != 1 {
		t.Fatal("not showing")
	}
	bc.SetClockOffset(3*time.Hour + time.Minute)
	if len(s.Mine(w.salonOwner, 10)) != 0 {
		t.Fatal("expired one still showing")
	}
	s.Escalate(ctx, 100)
	if d := deliveryOf(t, w.db, e.ID, w.salonOwner); d.EmailStatus != bc.EmailSkipped || !strings.Contains(d.EmailNote, "Expired") {
		t.Fatalf("expired email %s %s", d.EmailStatus, d.EmailNote)
	}
}

// ---------------------------------------------------------------- nudges

func TestNudgesBeforeOccasions(t *testing.T) {
	w := newWorld(t)
	s, box := service(w)
	ctx := context.Background()
	base := time.Date(2027, 10, 6, 6, 0, 0, 0, time.UTC) // 6 Oct, 09:00 in Nairobi
	bc.SetNow(func() time.Time { return base })
	mashujaa := occasions.Occasion{Key: "mashujaa_day", Name: "Mashujaa Day", Kind: occasions.KindPublicHoliday, Date: time.Date(2027, 10, 20, 0, 0, 0, 0, time.UTC), Countries: []string{"KE"}, Important: true}
	halloween := occasions.Occasion{Key: "halloween", Name: "Halloween", Kind: occasions.KindObservance, Date: time.Date(2027, 10, 31, 0, 0, 0, 0, time.UTC)}
	diwali := occasions.Occasion{Key: "diwali", Name: "Diwali", Kind: occasions.KindObservance, Date: time.Date(2027, 10, 29, 0, 0, 0, 0, time.UTC), Important: true}
	var asked []string
	s.Occasions = func(from, to time.Time, country string) []occasions.Occasion {
		asked = append(asked, from.Format(bc.DateLayout)+".."+to.Format(bc.DateLayout)+"|"+country)
		out := []occasions.Occasion{}
		for _, o := range []occasions.Occasion{mashujaa, diwali, halloween} {
			if !o.Date.Before(from) && !o.Date.After(to) {
				out = append(out, o)
			}
		}
		return out
	}
	// 14 days before Mashujaa Day: one nudge to the super admin, in the app and by email. Diwali (23 days) is out of range.
	if n := s.RunNudges(ctx); n != 1 {
		t.Fatalf("nudges %d", n)
	}
	if asked[0] != "2027-10-06..2027-10-27|" {
		t.Fatalf("looked ahead %v", asked)
	}
	var notes []notify.Notification
	w.db.Where("user_id = ? AND type = 'broadcast.nudge'", w.superAdmin).Find(&notes)
	if len(notes) != 1 || notes[0].Title != "Mashujaa Day is in 14 days — plan a message" || !strings.Contains(notes[0].Link, "occasion=mashujaa_day&date=2027-10-20") {
		t.Fatalf("nudge %+v", notes)
	}
	if !strings.Contains(notes[0].Body, "Wed 20 Oct, 08:00") {
		t.Fatalf("nudge body %q", notes[0].Body)
	}
	if mm := box.to("kennedy@dasyin.test"); len(mm) != 1 || mm[0].Subject != notes[0].Title {
		t.Fatalf("nudge email %d", len(mm))
	}
	// Idempotent.
	if n := s.RunNudges(ctx); n != 0 {
		t.Fatal("nudged twice")
	}
	// The ready draft: "Schedule for 20 Oct 08:00" in one click.
	up := s.ComingUp(0, false)
	if len(up) != 1 || up[0].Key != "mashujaa_day" || up[0].DaysUntil != 14 || up[0].Draft.LocalSendAt != "2027-10-20T08:00" || len(up[0].Nudged) != 1 {
		t.Fatalf("coming up %+v", up)
	}
	api := server(t, s, w)
	code, out := api("POST", "/api/v1/super-admin/announcements/occasions/plan", `{"key":"mashujaa_day","date":"2027-10-20","schedule":true}`, w.superAdmin, "super_admin")
	if code != 201 {
		t.Fatalf("plan %d %v", code, out)
	}
	ann := data(out)["announcement"].(map[string]any)
	if ann["status"] != "scheduled" || ann["local_send_at"] != "2027-10-20T08:00" || ann["occasion_key"] != "mashujaa_day" {
		t.Fatalf("planned %v", ann)
	}
	// Skip this year: Diwali gets no nudge.
	if err := s.SetSkip("diwali", 2027, true, w.superAdmin, "Kennedy"); err != nil {
		t.Fatal(err)
	}
	// 3 days before: the nudge says it is scheduled (Diwali, 12 days away, is skipped).
	bc.SetClockOffset(11 * 24 * time.Hour)
	if n := s.RunNudges(ctx); n != 1 {
		t.Fatal("3-day nudge")
	}
	w.db.Where("user_id = ? AND type = 'broadcast.nudge'", w.superAdmin).Order("created_at DESC").Find(&notes)
	if len(notes) != 2 || !strings.Contains(notes[0].Title+notes[1].Title, "is in 3 days — your message is scheduled") {
		t.Fatalf("3-day nudge %v / %v", notes[0].Title, notes[1].Title)
	}
	if n := s.RunNudges(ctx); n != 0 {
		t.Fatal("skipped occasion nudged")
	}
	if up := s.ComingUp(0, false); len(up) != 2 || !up[1].Skipped || up[0].Planned == nil {
		t.Fatalf("coming up after skip %+v", up)
	}
	s.SetSkip("diwali", 2027, false, w.superAdmin, "Kennedy")
	if n := s.RunNudges(ctx); n != 1 {
		t.Fatal("un-skipped occasion not nudged")
	}
	// Configurable lead days and switching nudges off.
	if _, err := s.SaveSettings(bc.Settings{NudgeLeadDays: []int{7}, NudgeEmail: false, NudgesOn: false, SendHour: 9, Timezone: "Africa/Nairobi"}, w.superAdmin); err != nil {
		t.Fatal(err)
	}
	if st := s.GetSettings(); len(st.NudgeLeadDays) != 1 || st.NudgeLeadDays[0] != 7 || st.SendHour != 9 || st.NudgesOn {
		t.Fatalf("settings %+v", st)
	}
}

// ---------------------------------------------------------------- HTTP

func server(t *testing.T, s *bc.Service, w *world) func(method, path, body, user, role string) (int, map[string]any) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	auth := r.Group("/api/v1", func(c *gin.Context) {
		if u := c.GetHeader("X-User"); u != "" {
			var orgID string
			w.db.Raw(`SELECT organization_id FROM users WHERE id = ?`, u).Scan(&orgID)
			c.Set("user_id", u)
			c.Set("org_id", orgID)
			c.Set("user_role", c.GetHeader("X-Role"))
		}
		c.Next()
	})
	pub := r.Group("/api/v1/public")
	(&bc.Handler{S: s}).RegisterRoutes(auth, pub)
	return func(method, path, body, user, role string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if user != "" {
			req.Header.Set("X-User", user)
			req.Header.Set("X-Role", role)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
}

func data(out map[string]any) map[string]any {
	d, _ := out["data"].(map[string]any)
	return d
}

func TestHTTPSuperAdminOnlyAndAudited(t *testing.T) {
	w := newWorld(t)
	s, box := service(w)
	api := server(t, s, w)
	// Owners, admins and staff are refused every admin route.
	for _, role := range []string{"admin", "owner", "staff", ""} {
		for _, rt := range [][2]string{{"GET", "/api/v1/super-admin/announcements"}, {"POST", "/api/v1/super-admin/announcements"}, {"POST", "/api/v1/super-admin/announcements/audience"},
			{"POST", "/api/v1/super-admin/announcements/run-due"}, {"GET", "/api/v1/super-admin/announcements/occasions"}} {
			if code, _ := api(rt[0], rt[1], `{}`, w.kOwner, role); code != http.StatusForbidden {
				t.Fatalf("%s %s as %q: %d", rt[0], rt[1], role, code)
			}
		}
	}
	sa := func(method, path, body string) (int, map[string]any) {
		return api(method, path, body, w.superAdmin, "super_admin")
	}
	code, out := sa("POST", "/api/v1/super-admin/announcements/audience", `{"audience":{"business_types":["nightclub"],"countries":["KE"],"role_level":"owners","exclude_super_admins":true},"kind":"urgent"}`)
	if code != 200 || data(out)["count"].(float64) != 4 {
		t.Fatalf("audience %d %v", code, out)
	}
	code, out = sa("POST", "/api/v1/super-admin/announcements", `{"title":"<b>Hi</b>","body":"<p>Hello <script>x</script></p>","kind":"weird"}`)
	if code != 400 || out["error"].(map[string]any)["field"] != "kind" {
		t.Fatalf("validation %d %v", code, out)
	}
	code, out = sa("POST", "/api/v1/super-admin/announcements", `{"title":"Hello owners","body":"<p>Hello <script>x</script><b>all</b></p>","kind":"update","audience":{"organization_ids":["`+w.clubAU+`"]}}`)
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	ann := data(out)["announcement"].(map[string]any)
	id := ann["id"].(string)
	if ann["body"] != "<p>Hello <strong>all</strong></p>" || ann["status"] != "draft" || ann["email_delay_minutes"].(float64) != 180 {
		t.Fatalf("created %v", ann)
	}
	// Test send to myself: the in-app card and an email, not counted in the stats.
	code, out = sa("POST", "/api/v1/super-admin/announcements/"+id+"/test", ``)
	if code != 200 || data(out)["email_status"] != "sent" || len(box.to("kennedy@dasyin.test")) != 1 || !strings.HasPrefix(box.to("kennedy@dasyin.test")[0].Subject, "[Test]") {
		t.Fatalf("test send %d %v", code, out)
	}
	code, out = sa("POST", "/api/v1/super-admin/announcements/"+id+"/send-now", ``)
	if code != 200 || data(out)["sent_to"].(float64) != 1 {
		t.Fatalf("send now %d %v", code, out)
	}
	if st := data(out)["stats"].(map[string]any); st["recipients"].(float64) != 1 {
		t.Fatalf("stats include the test send: %v", st)
	}
	if code, _ := sa("PUT", "/api/v1/super-admin/announcements/"+id, `{"title":"x","body":"y"}`); code != http.StatusConflict {
		t.Fatalf("edited after sending: %d", code)
	}
	if code, _ := sa("DELETE", "/api/v1/super-admin/announcements/"+id, ``); code != http.StatusConflict {
		t.Fatalf("deleted after sending: %d", code)
	}
	code, out = sa("GET", "/api/v1/super-admin/announcements", ``)
	if code != 200 || data(out)["total"].(float64) != 1 {
		t.Fatalf("list %d %v", code, out)
	}
	// The owner's side: list, read, dismiss — and not someone else's.
	code, out = api("GET", "/api/v1/announcements/me", ``, w.auOwner, "admin")
	items := data(out)["items"].([]any)
	if code != 200 || len(items) != 1 || data(out)["unread"].(float64) != 1 {
		t.Fatalf("me %d %v", code, out)
	}
	did := items[0].(map[string]any)["id"].(string)
	if code, _ := api("POST", "/api/v1/announcements/me/"+did+"/read", ``, w.salonOwner, "admin"); code != 404 {
		t.Fatalf("someone else's delivery %d", code)
	}
	if code, _ := api("POST", "/api/v1/announcements/me/"+did+"/dismiss", ``, w.auOwner, "admin"); code != 200 {
		t.Fatalf("dismiss %d", code)
	}
	var note notify.Notification
	w.db.Where("entity_id = ?", did).First(&note)
	if note.ReadAt == nil {
		t.Fatal("reading in the app did not mark the notification read")
	}
	if code, out := api("PUT", "/api/v1/announcements/me/preferences", `{"promotions":false}`, w.auOwner, "admin"); code != 200 || data(out)["promotions"] != false || !s.Unsubscribed(w.auOwner) {
		t.Fatalf("prefs %d %v", code, out)
	}
	// The open link: marks clicked and redirects into the app.
	req := httptest.NewRequest("GET", "/api/v1/public/announcements/open?t="+s.Token(bc.PurposeOpen, did, "view"), nil)
	rec := httptest.NewRecorder()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	(&bc.Handler{S: s}).RegisterRoutes(r.Group("/api/v1"), r.Group("/api/v1/public"))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://app.test/dashboard?announcement="+did {
		t.Fatalf("open %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if d := deliveryOf(t, w.db, id, w.auOwner); d.ClickedAt == nil {
		t.Fatal("open link did not mark clicked")
	}
	// Every admin action is in the audit log.
	var actions []string
	w.db.Raw(`SELECT action FROM audit_logs WHERE action LIKE 'announcement.%' ORDER BY created_at`).Scan(&actions)
	joined := strings.Join(actions, ",")
	for _, a := range []string{"announcement.create", "announcement.test_send", "announcement.send_now", "announcement.promotions_off"} {
		if !strings.Contains(joined, a) {
			t.Fatalf("audit lacks %s: %s", a, joined)
		}
	}
	// Export.
	req = httptest.NewRequest("GET", "/api/v1/super-admin/announcements/"+id+"/export", nil)
	req.Header.Set("X-User", w.superAdmin)
	req.Header.Set("X-Role", "super_admin")
	rec = httptest.NewRecorder()
	r2 := gin.New()
	r2.Use(func(c *gin.Context) { c.Set("user_id", w.superAdmin); c.Set("user_role", "super_admin"); c.Next() })
	(&bc.Handler{S: s}).RegisterRoutes(r2.Group("/api/v1"), r2.Group("/api/v1/public"))
	r2.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/csv") || !strings.Contains(rec.Body.String(), "bruce@bondi.test") {
		t.Fatalf("export %d %s", rec.Code, rec.Body.String())
	}
}
