package emailprefs_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/barstock"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/bookingmail"
	ep "github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/insights"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
)

func bp(b bool) *bool { return &b }

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
	boot.Exec(`DROP SCHEMA IF EXISTS emailprefs_test CASCADE`)
	boot.Exec(`CREATE SCHEMA emailprefs_test`)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path=emailprefs_test"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) primary key, name text, business_type varchar(100) default 'general', email text, phone text, address_street text, address_suburb text,
			address_state text, address_postcode text, country_code text, currency text, timezone text, region_source text, address_country text, created_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE users (id varchar(255) primary key, email text, role text, first_name text, last_name text, organization_id varchar(255), is_active boolean default true,
			created_at timestamptz default now(), deleted_at timestamptz)`,
		`CREATE TABLE audit_logs (id varchar(36) primary key, organization_id varchar(36), user_id varchar(36), action varchar(100), entity_type varchar(50), entity_id varchar(36),
			old_values jsonb, new_values jsonb, ip_address varchar(45), user_agent varchar(500), created_at timestamptz)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(append(bookingmail.Models(), &notify.Notification{})...); err != nil {
		t.Fatal(err)
	}
	if err := industry.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := ep.Migrate(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ep.RoleOf, ep.Can = nil, nil })
	return db
}

// club is a nightclub with an owner, a bar manager, a bartender and a store
// keeper (business-type roles), plus a super admin of another organisation.
type club struct {
	org, owner, manager, bartender, keeper, outsider string
	ind                                              *industry.Service
}

func newOrg(t *testing.T, db *gorm.DB) (string, *industry.Service) {
	t.Helper()
	org := uuid.NewString()
	db.Exec(`INSERT INTO organizations (id, name, business_type, country_code, currency, timezone, region_source, created_at) VALUES (?, 'Club Habari', 'general', 'KE', 'KES', 'Africa/Nairobi', 'user', now())`, org)
	ind := industry.NewService(db)
	// Wire the hooks the way cmd/production does.
	ep.RoleOf = func(o, u, legacy string) (string, string, bool) {
		r, err := ind.RoleFor(o, u, legacy)
		if err != nil || r == nil {
			return "", "", false
		}
		return r.Base, r.Name, true
	}
	ep.Can = func(o, u, legacy, module, act string) bool {
		a, err := ind.AccessFor(o, u, legacy)
		return err == nil && a.Can(module, act)
	}
	return org, ind
}

func user(t *testing.T, db *gorm.DB, org, email, role, first string) string {
	t.Helper()
	id := uuid.NewString()
	if err := db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id) VALUES (?, ?, ?, ?, 'Test', ?)`, id, email, role, first, org).Error; err != nil {
		t.Fatal(err)
	}
	return id
}

func newClub(t *testing.T, db *gorm.DB) club {
	t.Helper()
	org, ind := newOrg(t, db)
	c := club{org: org, ind: ind}
	c.owner = user(t, db, org, "peter@club.test", "admin", "Peter")
	if err := ind.OnOrgCreated(org, "nightclub", "test"); err != nil {
		t.Fatal(err)
	}
	c.manager = user(t, db, org, "mary.manager@club.test", "staff", "Mary")
	c.bartender = user(t, db, org, "brian.bar@club.test", "staff", "Brian")
	c.keeper = user(t, db, org, "kevin.store@club.test", "staff", "Kevin")
	for id, role := range map[string]string{c.manager: "bar_manager", c.bartender: "bartender", c.keeper: "store_keeper"} {
		if _, _, err := ind.Assign(org, id, role, "test"); err != nil {
			t.Fatal(err)
		}
	}
	other, _ := newOrg(t, db)
	c.outsider = user(t, db, other, "root@platform.test", "super_admin", "Root")
	return c
}

func emails(db *gorm.DB, org, cat string, ids ...string) string {
	return strings.Join(ep.UserEmails(db, org, cat, ids), ",")
}

func TestNightclubDefaultLevelsAndOverrides(t *testing.T) {
	db := testDB(t)
	c := newClub(t, db)
	// Nightclub → "only owners", decided at first read and stored.
	pol := ep.PolicyFor(db, c.org)
	if pol.Mode != ep.ModeOwners || pol.Source != "default" {
		t.Fatalf("nightclub default %+v", pol)
	}
	// A general organisation starts with everyone; the stored mode survives a later change of type.
	gen, gind := newOrg(t, db)
	if m := ep.PolicyFor(db, gen).Mode; m != ep.ModeEveryone {
		t.Fatalf("general default %s", m)
	}
	if _, err := gind.SetType(gen, "nightclub", "test"); err != nil {
		t.Fatal(err)
	}
	if m := ep.PolicyFor(db, gen).Mode; m != ep.ModeEveryone {
		t.Fatalf("stored mode changed by itself: %s", m)
	}
	var n int64
	db.Model(&ep.Policy{}).Where("organization_id IN ?", []string{c.org, gen}).Count(&n)
	if n != 2 {
		t.Fatalf("policy rows %d", n)
	}

	// Levels from the role templates.
	people, _ := ep.People(db, c.org)
	got := map[string]string{}
	for _, p := range people {
		got[p.Email] = p.Level + "/" + p.RoleLabel
	}
	for e, want := range map[string]string{"peter@club.test": "owner/Owner", "mary.manager@club.test": "manager/Bar manager", "brian.bar@club.test": "staff/Bartender",
		"kevin.store@club.test": "staff/Store keeper"} {
		if got[e] != want {
			t.Errorf("%s: %s want %s", e, got[e], want)
		}
	}
	if len(people) != 4 || people[0].Level != ep.LevelOwner {
		t.Fatalf("people %d, owners first", len(people))
	}

	all := []string{c.owner, c.manager, c.bartender, c.keeper, c.outsider}
	// owners: only Peter.
	if e := emails(db, c.org, ep.Bookings, all...); e != "peter@club.test" {
		t.Fatalf("owners mode bookings: %s", e)
	}
	// owners and managers: + the bar manager.
	if _, err := ep.SetMode(db, c.org, ep.ModeOwnersManagers, c.owner); err != nil {
		t.Fatal(err)
	}
	if e := emails(db, c.org, ep.Bookings, all...); e != "peter@club.test,mary.manager@club.test" {
		t.Fatalf("owners+managers bookings: %s", e)
	}
	// everyone: every member — never the super admin of another organisation.
	ep.SetMode(db, c.org, ep.ModeEveryone, c.owner)
	if e := emails(db, c.org, ep.Bookings, all...); e != "peter@club.test,mary.manager@club.test,brian.bar@club.test,kevin.store@club.test" {
		t.Fatalf("everyone bookings: %s", e)
	}
	if ok, why := ep.Check(db, c.org, c.outsider, ep.Bookings); ok || why != "not a member of the organisation" {
		t.Fatalf("outsider %v %s", ok, why)
	}
	// Admin switches: Brian's bookings off; Kevin all off.
	if err := ep.SetAdmin(db, c.org, c.bartender, nil, map[string]*bool{ep.Bookings: bp(false)}, c.owner); err != nil {
		t.Fatal(err)
	}
	ep.SetAdmin(db, c.org, c.keeper, bp(true), nil, c.owner)
	if e := emails(db, c.org, ep.Bookings, all...); e != "peter@club.test,mary.manager@club.test" {
		t.Fatalf("after admin switches: %s", e)
	}
	if ok, why := ep.Check(db, c.org, c.bartender, ep.Bookings); ok || why != "turned off by an administrator" {
		t.Fatalf("bartender %v %s", ok, why)
	}
	if !ep.Allowed(db, c.org, c.bartender, ep.Agenda) || ep.Allowed(db, c.org, c.keeper, ep.Agenda) {
		t.Fatal("agenda: one category off must not touch the others; all off must")
	}
	// owners mode + an admin's exception: the bar manager gets the night report.
	ep.SetMode(db, c.org, ep.ModeOwners, c.owner)
	ep.SetAdmin(db, c.org, c.manager, nil, map[string]*bool{ep.BarNightReport: bp(true)}, c.owner)
	if !ep.Allowed(db, c.org, c.manager, ep.BarNightReport) || ep.Allowed(db, c.org, c.manager, ep.Bookings) {
		t.Fatal("exception for the bar manager")
	}
	// Reset to the mode (null).
	ep.SetAdmin(db, c.org, c.manager, nil, map[string]*bool{ep.BarNightReport: nil}, c.owner)
	if ep.Allowed(db, c.org, c.manager, ep.BarNightReport) {
		t.Fatal("reset to the mode")
	}
	pv, ok := ep.PersonOf(db, c.org, c.keeper)
	if !ok || !pv.AllOff || !pv.EmailsOff || pv.Reasons[ep.Bookings] != ep.ReasonAllOff || pv.Applies[ep.BillReminders] {
		t.Fatalf("keeper view %+v", pv)
	}
	if _, ok := ep.PersonOf(db, c.org, c.outsider); ok {
		t.Fatal("outsider is not a member")
	}
}

func TestFilterAddresses(t *testing.T) {
	db := testDB(t)
	c := newClub(t, db)
	list := []string{"Brian.Bar@Club.test", "accounts@outside.test", "peter@club.test", "Mary Manager <mary.manager@club.test>", "root@platform.test"}
	got := func() string { return strings.Join(ep.FilterAddresses(db, c.org, ep.BillReminders, list), ",") }
	// owners: members below owner level go (case-insensitive, also "Name <addr>"); other addresses stay.
	if g := got(); g != "accounts@outside.test,peter@club.test,root@platform.test" {
		t.Fatalf("owners: %s", g)
	}
	// An admin's exception keeps that address.
	ep.SetAdmin(db, c.org, c.manager, nil, map[string]*bool{ep.BillReminders: bp(true)}, c.owner)
	if g := ep.FilterAddresses(db, c.org, ep.BillReminders, list); len(g) != 4 || g[2] != "Mary Manager <mary.manager@club.test>" {
		t.Fatalf("exception: %v", g)
	}
	ep.SetAdmin(db, c.org, c.manager, nil, map[string]*bool{ep.BillReminders: nil}, c.owner)
	// owners and managers: the staff member goes, the manager stays.
	ep.SetMode(db, c.org, ep.ModeOwnersManagers, c.owner)
	if g := got(); g != "accounts@outside.test,peter@club.test,Mary Manager <mary.manager@club.test>,root@platform.test" {
		t.Fatalf("owners+managers: %s", g)
	}
	// everyone: the list as typed …
	ep.SetMode(db, c.org, ep.ModeEveryone, c.owner)
	if g := got(); g != strings.Join(list, ",") {
		t.Fatalf("everyone: %s", g)
	}
	// … but a member's own switches count in every mode: all off (admin), this email off (admin), their own opt-out.
	ep.SetAdmin(db, c.org, c.bartender, bp(true), nil, c.owner)
	ep.SetAdmin(db, c.org, c.manager, nil, map[string]*bool{ep.BillReminders: bp(false)}, c.owner)
	if err := ep.SetSelf(db, c.org, c.owner, nil, map[string]bool{ep.BillReminders: false}); err != nil {
		t.Fatal(err)
	}
	if g := got(); g != "accounts@outside.test,root@platform.test" {
		t.Fatalf("switches: %s", g)
	}
	// Another email the switches don't touch keeps the addresses (the bartender's "all off" still counts).
	if g := strings.Join(ep.FilterAddresses(db, c.org, ep.StrategyBrief, list), ","); g != "accounts@outside.test,peter@club.test,Mary Manager <mary.manager@club.test>,root@platform.test" {
		t.Fatalf("other category: %s", g)
	}
	// Their own "pause all" too.
	ep.SetSelf(db, c.org, c.owner, nil, map[string]bool{ep.BillReminders: true})
	ep.SetSelf(db, c.org, c.owner, bp(true), nil)
	if g := strings.Join(ep.FilterAddresses(db, c.org, ep.StrategyBrief, list), ","); g != "accounts@outside.test,Mary Manager <mary.manager@club.test>,root@platform.test" {
		t.Fatalf("self all off: %s", g)
	}
}

func TestSendersHonourTheResolver(t *testing.T) {
	db := testDB(t)
	c := newClub(t, db)
	t.Setenv("SMTP_HOST", "smtp.test.invalid")
	t.Setenv("SMTP_FROM", "Club <bookings@club.test>")

	// ---- bookings: the bartender's booking email
	var mu sync.Mutex
	var sent []mail.Message
	ms := mail.NewService(db)
	ms.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, m)
		return nil
	}
	n := &bookingmail.Notifier{DB: db, Mail: ms, Async: false, Now: func() time.Time { return time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC) }}
	staff := c.bartender
	start := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	bk := func(id string) *models.Booking {
		return &models.Booking{ID: id, OrganizationID: c.org, Status: "scheduled", StartTime: start, EndTime: start.Add(4 * time.Hour), StaffID: &staff,
			Customer: models.Customer{FirstName: "Grace", LastName: "Guest"}}
	}
	lastLog := func(id string) bookingmail.Log {
		var l bookingmail.Log
		db.Where("booking_id = ?", id).Order("created_at DESC").First(&l)
		return l
	}
	n.Handle(nil, bk("b-owners"), c.owner) // owners mode (nightclub default)
	if len(sent) != 0 || lastLog("b-owners").Status != "skipped" || lastLog("b-owners").Error != "the organisation only emails owners" {
		t.Fatalf("owners mode: %d sent, log %+v", len(sent), lastLog("b-owners"))
	}
	ep.SetMode(db, c.org, ep.ModeEveryone, c.owner)
	n.Handle(nil, bk("b-everyone"), c.owner)
	if len(sent) != 1 || !strings.Contains(sent[0].To[0], "brian.bar@club.test") {
		t.Fatalf("everyone: %d sent", len(sent))
	}
	ep.SetAdmin(db, c.org, c.bartender, nil, map[string]*bool{ep.Bookings: bp(false)}, c.owner)
	n.Handle(nil, bk("b-off"), c.owner)
	if len(sent) != 1 || lastLog("b-off").Error != "turned off by an administrator" {
		t.Fatalf("overridden: %d sent, %+v", len(sent), lastLog("b-off"))
	}
	ep.SetAdmin(db, c.org, c.bartender, nil, map[string]*bool{ep.Bookings: nil}, c.owner)

	// ---- bill reminders: admins + lists; staff addresses go in owners mode
	ep.SetMode(db, c.org, ep.ModeOwners, c.owner)
	bills := &recurbills.Service{DB: db}
	st := recurbills.DefaultSettings(c.org)
	st.Recipients = []string{"brian.bar@club.test", "accounts@outside.test"}
	sched := &recurbills.RecurringBill{ExtraRecipients: []string{"KEVIN.STORE@club.test", "ops@outside.test"}}
	if got := strings.Join(bills.Recipients(c.org, st, sched), ","); got != "peter@club.test,accounts@outside.test,ops@outside.test" {
		t.Fatalf("bill reminders, owners: %s", got)
	}
	ep.SetMode(db, c.org, ep.ModeEveryone, c.owner)
	if got := strings.Join(bills.Recipients(c.org, st, sched), ","); got != "peter@club.test,brian.bar@club.test,accounts@outside.test,kevin.store@club.test,ops@outside.test" {
		t.Fatalf("bill reminders, everyone: %s", got)
	}
	ep.SetAdmin(db, c.org, c.owner, nil, map[string]*bool{ep.BillReminders: bp(false)}, c.owner)
	if got := strings.Join(bills.Recipients(c.org, st, nil), ","); got != "brian.bar@club.test,accounts@outside.test" {
		t.Fatalf("owner switched off: %s", got)
	}
	ep.SetAdmin(db, c.org, c.owner, nil, map[string]*bool{ep.BillReminders: nil}, c.owner)
	// A member typed into a list follows their switches in every mode: the bartender pauses all their emails.
	ep.SetSelf(db, c.org, c.bartender, bp(true), nil)
	if got := strings.Join(bills.Recipients(c.org, st, sched), ","); got != "peter@club.test,accounts@outside.test,kevin.store@club.test,ops@outside.test" {
		t.Fatalf("bartender paused: %s", got)
	}
	ep.SetSelf(db, c.org, c.bartender, bp(false), nil)

	// ---- bar night report: approvers (owner, bar manager) through the mode
	bar := barstock.New(db)
	bar.Can = ep.Can
	night := func() string { return strings.Join(bar.Recipients(db, c.org), ",") }
	if got := night(); got != "mary.manager@club.test,peter@club.test" {
		t.Fatalf("night report, everyone: %s", got)
	}
	ep.SetMode(db, c.org, ep.ModeOwners, c.owner)
	if got := night(); got != "peter@club.test" {
		t.Fatalf("night report, owners: %s", got)
	}
	ep.SetAdmin(db, c.org, c.manager, nil, map[string]*bool{ep.BarNightReport: bp(true)}, c.owner)
	// … an admin's "on" for a bartender cannot leak the report (it is not for their role).
	ep.SetAdmin(db, c.org, c.bartender, nil, map[string]*bool{ep.BarNightReport: bp(true)}, c.owner)
	if got := night(); got != "mary.manager@club.test,peter@club.test" {
		t.Fatalf("night report, exception: %s", got)
	}

	// ---- strategy brief: admins through the resolver
	intel := insights.NewIntel(&insights.Service{DB: db}, nil)
	if got := strings.Join(intel.BriefRecipients(c.org), ","); got != "peter@club.test" {
		t.Fatalf("brief: %s", got)
	}
	ep.SetAdmin(db, c.org, c.owner, bp(true), nil, c.owner)
	if got := intel.BriefRecipients(c.org); len(got) != 0 {
		t.Fatalf("brief with the owner all off: %v", got)
	}
	// A super admin who is a member is an owner; one of another organisation never gets it.
	sa := user(t, db, c.org, "sa.member@club.test", "super_admin", "Sam")
	if got := strings.Join(intel.BriefRecipients(c.org), ","); got != "sa.member@club.test" || !ep.Allowed(db, c.org, sa, ep.StrategyBrief) {
		t.Fatalf("brief, member super admin: %s", got)
	}
}

func TestLegacyBookingPrefsMigration(t *testing.T) {
	db := testDB(t)
	org, _ := newOrg(t, db)
	off := user(t, db, org, "off@shop.test", "staff", "Off")
	on := user(t, db, org, "on@shop.test", "staff", "On")
	// Recorded without an organisation id: the user's organisation is used.
	db.Create(&bookingmail.UserPref{UserID: off, BookingEmails: false})
	db.Create(&bookingmail.UserPref{UserID: on, OrganizationID: org, BookingEmails: true})
	db.Where("key = ?", "legacy_booking_prefs").Delete(&ep.Meta{}) // Migrate already ran on the empty table
	moved, err := ep.MigrateLegacyBookingPrefs(db)
	if err != nil || moved != 1 {
		t.Fatalf("moved %d %v", moved, err)
	}
	if ep.Allowed(db, org, off, ep.Bookings) || ep.Allowed(db, org, off, ep.Agenda) {
		t.Fatal("legacy off not applied to bookings + agenda")
	}
	if ok, why := ep.Check(db, org, off, ep.Bookings); ok || why != "turned off in the staff member's profile" {
		t.Fatalf("reason %s", why)
	}
	if !ep.Allowed(db, org, on, ep.Bookings) {
		t.Fatal("legacy on")
	}
	// The person turns booking emails back on; running the migration again changes nothing.
	if err := ep.SetSelfCategories(db, org, off, []string{ep.Bookings, ep.Agenda}, true); err != nil {
		t.Fatal(err)
	}
	if moved, err := ep.MigrateLegacyBookingPrefs(db); err != nil || moved != 0 || !ep.Allowed(db, org, off, ep.Bookings) {
		t.Fatalf("second run moved %d (%v), allowed %v", moved, err, ep.Allowed(db, org, off, ep.Bookings))
	}
	var rows int64
	db.Model(&ep.Pref{}).Where("user_id = ?", off).Count(&rows)
	if rows != 2 {
		t.Fatalf("pref rows %d", rows)
	}
}

// ---------------------------------------------------------------- HTTP

type call func(method, path, body, user, role string) (int, map[string]any)

func server(t *testing.T, db *gorm.DB, org string) call {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1", func(c *gin.Context) {
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("user_role", c.GetHeader("X-Role"))
		c.Set("org_id", org)
		c.Next()
	})
	(&ep.Handler{DB: db, Previews: map[string]ep.PreviewFunc{ep.BillReminders: func(o string) ep.Preview {
		st := recurbills.DefaultSettings(o)
		st.Recipients = []string{"Brian.Bar@club.test", "accounts@outside.test"}
		return ep.Preview{Recipients: (&recurbills.Service{DB: db}).Recipients(o, st, nil), Enabled: true}
	}}}).RegisterRoutes(g)
	n := &bookingmail.Notifier{DB: db}
	(&bookingmail.Handler{N: n}).RegisterRoutes(g)
	return func(method, path, body, user, role string) (int, map[string]any) {
		req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-User", user)
		req.Header.Set("X-Role", role)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
}

func data(m map[string]any) map[string]any { d, _ := m["data"].(map[string]any); return d }

func TestHTTP(t *testing.T) {
	db := testDB(t)
	c := newClub(t, db)
	api := server(t, db, c.org)

	// Admins only.
	if code, _ := api("GET", "/email-recipients", "", c.bartender, "staff"); code != http.StatusForbidden {
		t.Fatalf("staff read the list: %d", code)
	}
	if code, _ := api("GET", "/email-recipients", "", c.manager, "manager"); code != http.StatusForbidden {
		t.Fatalf("manager read the list: %d", code)
	}
	code, out := api("GET", "/email-recipients", "", c.owner, "admin")
	d := data(out)
	if code != 200 || d["mode"] != ep.ModeOwners || len(d["people"].([]any)) != 4 || len(d["categories"].([]any)) != len(ep.Categories) || len(d["modes"].([]any)) != 3 {
		t.Fatalf("list %d %v", code, out)
	}
	p0 := d["people"].([]any)[0].(map[string]any)
	for _, k := range []string{"user_id", "name", "email", "role", "role_label", "level", "effective", "overrides", "all_off", "reason_per_category", "emails_off"} {
		if _, ok := p0[k]; !ok {
			t.Fatalf("person lacks %s: %v", k, p0)
		}
	}
	// Super admin acting in the organisation.
	if code, _ := api("GET", "/email-recipients", "", c.outsider, "super_admin"); code != 200 {
		t.Fatalf("super admin: %d", code)
	}

	// Policy.
	if code, _ := api("PUT", "/email-recipients/policy", `{"mode":"nobody"}`, c.owner, "admin"); code != 400 {
		t.Fatalf("bad mode %d", code)
	}
	if code, out := api("PUT", "/email-recipients/policy", `{"mode":"everyone"}`, c.owner, "admin"); code != 200 || data(out)["mode"] != "everyone" || data(out)["mode_source"] != "admin" {
		t.Fatalf("set mode %d %v", code, out)
	}
	// Preview through the sender's resolver: the owner and the typed-in list …
	preview := func() string {
		code, out := api("GET", "/email-recipients/preview?category=bill_reminders", "", c.owner, "admin")
		if code != 200 {
			t.Fatalf("preview %d %v", code, out)
		}
		r := []string{}
		for _, x := range data(out)["recipients"].([]any) {
			r = append(r, x.(string))
		}
		return strings.Join(r, ",")
	}
	if p := preview(); p != "peter@club.test,brian.bar@club.test,accounts@outside.test" {
		t.Fatalf("preview %s", p)
	}
	// … without a member whose emails are all off.
	api("PUT", "/email-recipients/users/"+c.bartender, `{"all_off":true}`, c.owner, "admin")
	if p := preview(); p != "peter@club.test,accounts@outside.test" {
		t.Fatalf("preview after all off %s", p)
	}
	api("PUT", "/email-recipients/users/"+c.bartender, `{"all_off":false}`, c.owner, "admin")
	if code, out := api("GET", "/email-recipients/preview?category=bookings", "", c.owner, "admin"); code != 200 || len(data(out)["recipients"].([]any)) != 4 {
		t.Fatalf("default preview %d %v", code, out)
	}

	// One person.
	if code, _ := api("PUT", "/email-recipients/users/"+c.outsider, `{"all_off":true}`, c.owner, "admin"); code != 404 {
		t.Fatalf("non-member %d", code)
	}
	if code, _ := api("PUT", "/email-recipients/users/"+c.bartender, `{"overrides":{"nope":false}}`, c.owner, "admin"); code != 400 {
		t.Fatalf("unknown category %d", code)
	}
	code, out = api("PUT", "/email-recipients/users/"+c.bartender, `{"overrides":{"bookings":false}}`, c.owner, "admin")
	pp := data(out)["person"].(map[string]any)
	if code != 200 || pp["effective"].(map[string]any)["bookings"] != false || pp["reason_per_category"].(map[string]any)["bookings"] != "admin_off" {
		t.Fatalf("override %d %v", code, out)
	}
	var audits int64
	db.Table("audit_logs").Where("organization_id = ? AND action LIKE 'email_recipients.%'", c.org).Count(&audits)
	if audits != 4 {
		t.Fatalf("audit rows %d", audits)
	}

	// The bartender's own page shows the lock and cannot switch it back on.
	code, out = api("GET", "/me/email-preferences", "", c.bartender, "staff")
	cats := map[string]map[string]any{}
	for _, x := range data(out)["categories"].([]any) {
		m := x.(map[string]any)
		cats[m["key"].(string)] = m
	}
	if code != 200 || len(cats) != 3 || cats[ep.PlatformUpdates] == nil || cats["bookings"]["locked"] != true || cats["bookings"]["reason_text"] != "Turned off by your administrator" || cats["agenda"]["locked"] != false {
		t.Fatalf("mine %d %v", code, out)
	}
	if code, out := api("PUT", "/me/email-preferences", `{"categories":{"bookings":true}}`, c.bartender, "staff"); code != http.StatusForbidden || out["error"].(map[string]any)["code"] != "LOCKED_BY_ADMIN" {
		t.Fatalf("re-enabled an admin lock: %d %v", code, out)
	}
	if code, _ := api("PUT", "/me/email-preferences", `{"categories":{"bill_reminders":false}}`, c.bartender, "staff"); code != 400 {
		t.Fatalf("category that is not for them %d", code)
	}
	if ep.Allowed(db, c.org, c.bartender, ep.Bookings) {
		t.Fatal("lock broken")
	}
	// In "only owners" mode the lock is the mode.
	api("PUT", "/email-recipients/policy", `{"mode":"owners"}`, c.owner, "admin")
	code, out = api("GET", "/me/email-preferences", "", c.keeper, "staff")
	for _, x := range data(out)["categories"].([]any) {
		m := x.(map[string]any)
		if m["locked"] != true || m["reason_text"] != "Turned off by your administrator — only owners get emails" {
			t.Fatalf("mode lock %v", m)
		}
	}
	// The owner turns their own daily agenda off (self opt-out), and back on.
	if code, out := api("PUT", "/me/email-preferences", `{"categories":{"agenda":false}}`, c.owner, "admin"); code != 200 || ep.Allowed(db, c.org, c.owner, ep.Agenda) {
		t.Fatalf("self off %d %v", code, out)
	}
	code, out = api("GET", "/email-recipients", "", c.owner, "admin")
	for _, x := range data(out)["people"].([]any) {
		m := x.(map[string]any)
		if m["user_id"] == c.owner && (m["reason_per_category"].(map[string]any)["agenda"] != "self_off" || m["self_off"].(map[string]any)["agenda"] != true) {
			t.Fatalf("admin sees 'turned off by them': %v", m)
		}
	}
	if code, _ := api("PUT", "/me/email-preferences", `{"categories":{"agenda":true}}`, c.owner, "admin"); code != 200 || !ep.Allowed(db, c.org, c.owner, ep.Agenda) {
		t.Fatalf("self on %d", code)
	}
	// Pause everything, then the old endpoint switches only booking emails back on.
	api("PUT", "/me/email-preferences", `{"all_off":true}`, c.owner, "admin")
	if ep.Allowed(db, c.org, c.owner, ep.BillReminders) {
		t.Fatal("self all off")
	}
	if code, out := api("PUT", "/booking-emails/me", `{"booking_emails":true}`, c.owner, "admin"); code != 200 || data(out)["booking_emails"] != true {
		t.Fatalf("old endpoint %d %v", code, out)
	}
	if !ep.Allowed(db, c.org, c.owner, ep.Bookings) || !ep.Allowed(db, c.org, c.owner, ep.Agenda) || ep.Allowed(db, c.org, c.owner, ep.BillReminders) {
		t.Fatal("old endpoint must only re-enable booking emails")
	}
	if code, out := api("PUT", "/booking-emails/me", `{"booking_emails":false}`, c.owner, "admin"); code != 200 || data(out)["booking_emails"] != false || ep.Allowed(db, c.org, c.owner, ep.Bookings) {
		t.Fatalf("old endpoint off %d %v", code, out)
	}
	if code, out := api("GET", "/booking-emails/me", "", c.bartender, "staff"); code != 200 || data(out)["booking_emails"] != true || data(out)["locked_by_admin"] != true {
		t.Fatalf("old endpoint read %d %v", code, out)
	}
	// A super admin acting in another organisation is not emailed by it.
	if code, out := api("GET", "/me/email-preferences", "", c.outsider, "super_admin"); code != 200 || data(out)["member"] != false {
		t.Fatalf("outsider mine %d %v", code, out)
	}
	if code, _ := api("PUT", "/me/email-preferences", `{"all_off":true}`, c.outsider, "super_admin"); code != http.StatusForbidden {
		t.Fatalf("outsider save %d", code)
	}
	// Owners first, then managers and staff; names sorted.
	people, _ := ep.People(db, c.org)
	lv := []string{}
	for _, p := range people {
		lv = append(lv, p.Level)
	}
	if !sort.SliceIsSorted(lv, func(i, j int) bool {
		return map[string]int{"owner": 0, "manager": 1, "staff": 2}[lv[i]] < map[string]int{"owner": 0, "manager": 1, "staff": 2}[lv[j]]
	}) {
		t.Fatalf("order %v", lv)
	}
}
