package subscriptions

import (
	"strings"
	"testing"
	"time"
)

func defaultPolicy() Policy { return PolicyOf(DefaultSettings("o")) }

func keys(steps []Step) string {
	out := []string{}
	for _, s := range steps {
		out = append(out, s.Key+"@"+DayStr(s.Date))
	}
	return strings.Join(out, " ")
}

func tp(s string) *time.Time { t := d(s); return &t }

// The default timeline: reminders 3 days before, on and 3 days after the
// due date; admin alerts 3 days and 1 day before suspension; the final
// notice 2 days before; suspended 7 days after the due date.
func TestDunningTimelineDefaults(t *testing.T) {
	p := defaultPolicy()
	dt := p.DatesFor(d("2030-01-19"), 7, 0, true, nil, d("2030-01-01"))
	if dt.Suspend == nil || DayStr(*dt.Suspend) != "2030-01-26" || dt.Deferred || dt.FinalNotice == nil || DayStr(*dt.FinalNotice) != "2030-01-24" {
		t.Fatalf("dates: %+v", dt)
	}
	got := keys(p.Steps(dt))
	want := "client_-3@2030-01-16 client_0@2030-01-19 client_3@2030-01-22 admin_3@2030-01-23 final_notice@2030-01-24 admin_1@2030-01-25 suspend@2030-01-26"
	if got != want {
		t.Fatalf("steps:\n got %s\nwant %s", got, want)
	}
	// The admins always hear at least 3 days ahead.
	for _, s := range p.Steps(dt) {
		if s.Kind == KindAlert && s.Before >= MinNoticeDays && DaysBetween(s.Date, *dt.Suspend) < MinNoticeDays {
			t.Fatal("first alert too late")
		}
	}
}

func TestDunningDueByDay(t *testing.T) {
	p := defaultPolicy()
	due := d("2030-01-19")
	plan := func(day string) DuePlan {
		return p.Due(p.DatesFor(due, 7, 0, true, tp("2030-01-23"), d(day)), d(day), false)
	}
	if x := plan("2030-01-15"); x.Client != nil || x.Admin != nil || x.Suspend != nil {
		t.Fatal("nothing 4 days before due")
	}
	if x := plan("2030-01-16"); x.Client == nil || x.Client.Key != "client_-3" {
		t.Fatal("reminder 3 days before")
	}
	if x := plan("2030-01-22"); x.Client == nil || x.Client.Key != "client_3" || len(x.Stale) != 2 {
		t.Fatalf("D+3: latest reminder only, the others stale: %+v", x)
	}
	if x := plan("2030-01-23"); x.Admin == nil || x.Admin.Key != "admin_3" {
		t.Fatal("admin alert 3 days before suspension")
	}
	if x := plan("2030-01-24"); x.FinalNotice == nil || x.Client != nil {
		t.Fatal("final notice supersedes the plain reminders")
	}
	if x := plan("2030-01-25"); x.Admin == nil || x.Admin.Key != "admin_1" {
		t.Fatal("admin alert the day before")
	}
	if x := plan("2030-01-26"); x.Suspend == nil || x.Client != nil || x.Admin != nil || x.FinalNotice != nil {
		t.Fatalf("suspension day: only the state change: %+v", x)
	}
	if x := p.Due(p.DatesFor(due, 7, 0, true, tp("2030-01-23"), d("2030-01-27")), d("2030-01-27"), true); x.Suspend != nil || x.Client != nil {
		t.Fatal("already suspended: nothing more")
	}
}

// The suspension never comes sooner than 3 days after the first admin alert.
func TestDunningMinimumNotice(t *testing.T) {
	p := defaultPolicy()
	due := d("2030-01-19")
	// Grace of 1 day: the 3-day alert lands before the due date and the suspension stays on D+1.
	dt := p.DatesFor(due, 1, 0, true, nil, d("2030-01-10"))
	if DayStr(*dt.Suspend) != "2030-01-20" || dt.Deferred || !strings.Contains(keys(p.Steps(dt)), "admin_3@2030-01-17") {
		t.Fatalf("grace 1: %s %s", DayStr(*dt.Suspend), keys(p.Steps(dt)))
	}
	// Nothing ran until the day before the planned suspension (an outage): the
	// alert goes out today and the suspension waits 3 days for it.
	dt = p.DatesFor(due, 7, 0, true, nil, d("2030-01-25"))
	if DayStr(*dt.Suspend) != "2030-01-28" || !dt.Deferred || DayStr(*dt.Planned) != "2030-01-26" {
		t.Fatalf("deferred: %+v", dt)
	}
	x := p.Due(dt, d("2030-01-25"), false)
	if x.Admin == nil || x.Admin.Key != "admin_3" || x.Suspend != nil {
		t.Fatalf("the late alert is the 3-day one: %+v", x)
	}
	// Once that alert went out on the 25th, the suspension stays on the 28th.
	if dt2 := p.DatesFor(due, 7, 0, true, tp("2030-01-25"), d("2030-01-27")); DayStr(*dt2.Suspend) != "2030-01-28" {
		t.Fatalf("after the late alert: %s", DayStr(*dt2.Suspend))
	}
	// Alerted on time: no deferral.
	if dt3 := p.DatesFor(due, 7, 0, true, tp("2030-01-23"), d("2030-01-26")); DayStr(*dt3.Suspend) != "2030-01-26" || dt3.Deferred {
		t.Fatal("alerted on time")
	}
}

// Giving more time moves the suspension, the final notice and the admin
// alerts; the reminders before the due date stay where they were.
func TestDunningGiveMoreTime(t *testing.T) {
	p := defaultPolicy()
	due := d("2030-01-19")
	dt := p.DatesFor(due, 7, 7, true, tp("2030-01-23"), d("2030-01-24"))
	want := "client_-3@2030-01-16 client_0@2030-01-19 client_3@2030-01-22 admin_3@2030-01-30 final_notice@2030-01-31 admin_1@2030-02-01 suspend@2030-02-02"
	if got := keys(p.Steps(dt)); got != want {
		t.Fatalf("+7 days:\n got %s\nwant %s", got, want)
	}
	// The alert and notice keys carry the suspension date, so they go out again for the new date.
	a := Step{Key: "admin_3", Kind: KindAlert}
	before := a.EventKey("per", p.DatesFor(due, 7, 0, true, nil, d("2030-01-01")))
	after := a.EventKey("per", dt)
	if before == after || !strings.HasSuffix(after, "2030-02-02") {
		t.Fatalf("event keys %s / %s", before, after)
	}
	if (Step{Key: "client_3", Kind: KindReminder}).EventKey("per", dt) != "per|client_3" {
		t.Fatal("reminder keys don't move")
	}
}

func TestDunningNoSuspension(t *testing.T) {
	p := defaultPolicy()
	dt := p.DatesFor(d("2030-01-19"), 7, 0, false, nil, d("2030-01-30"))
	if dt.Suspend != nil || dt.FinalNotice != nil {
		t.Fatal("auto-suspend off / don't suspend: no suspension, no final notice")
	}
	if got := keys(p.Steps(dt)); got != "client_-3@2030-01-16 client_0@2030-01-19 client_3@2030-01-22" {
		t.Fatalf("only reminders: %s", got)
	}
	if x := p.Due(dt, d("2030-01-30"), false); x.Suspend != nil || x.Admin != nil || x.Client == nil {
		t.Fatal("still reminded, never suspended")
	}
}

func TestDunningCustomSchedule(t *testing.T) {
	st := DefaultSettings("o")
	st.ClientDays, st.AdminDays, st.FinalNoticeDays = []int{-7, -1, 1}, []int{5, 2}, 3
	p := PolicyOf(st)
	dt := p.DatesFor(d("2030-01-19"), 10, 0, true, nil, d("2030-01-01"))
	want := "client_-7@2030-01-12 client_-1@2030-01-18 client_1@2030-01-20 admin_5@2030-01-24 final_notice@2030-01-26 admin_2@2030-01-27 suspend@2030-01-29"
	if got := keys(p.Steps(dt)); got != want {
		t.Fatalf("custom:\n got %s\nwant %s", got, want)
	}
}

func TestSettingsRules(t *testing.T) {
	if _, err := cleanDays([]int{3, 3, 1}, 1, 30, 4, "admin alerts"); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanDays([]int{40}, 1, 30, 4, "admin alerts"); err == nil {
		t.Fatal("range")
	}
}

func TestPortalToken(t *testing.T) {
	s := &Service{Secret: []byte("secret-1")}
	tok := s.PortalToken("org-1", "cust-9")
	cid, sig, ok := splitPortalToken(tok)
	if !ok || cid != "cust-9" || sig != s.portalSig("org-1", "cust-9") || strings.ToLower(tok) != tok {
		t.Fatalf("token %q", tok)
	}
	if (&Service{Secret: []byte("secret-2")}).portalSig("org-1", "cust-9") == sig || s.portalSig("org-2", "cust-9") == sig || s.portalSig("org-1", "cust-8") == sig {
		t.Fatal("the signature depends on the secret, organisation and customer")
	}
	for _, bad := range []string{"", "abc", "abc.", ".abc"} {
		if _, _, ok := splitPortalToken(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	if (&Service{}).PortalToken("o", "c") != "" {
		t.Fatal("no secret, no link")
	}
}

func TestTaskStepsAndTitles(t *testing.T) {
	tg := Target{Supplier: SupplierSynergy, Plan: "Micro Env", Domain: "wanjiku.co.ke", Kind: KindHosting, Reason: "unpaid invoice INV-1001"}
	s := strings.Join(TaskSteps(TaskSuspend, tg), "\n")
	for _, want := range []string{"Synergy Wholesale", "wanjiku.co.ke (Micro Env)", "Suspend", "INV-1001", "mark this task done"} {
		if !strings.Contains(s, want) {
			t.Fatalf("suspend steps miss %q:\n%s", want, s)
		}
	}
	if s := strings.Join(TaskSteps(TaskRestore, tg), "\n"); !strings.Contains(s, "Unsuspend") {
		t.Fatalf("restore steps:\n%s", s)
	}
	h := Target{Supplier: SupplierHostingCom, Domain: "x.com.au", Kind: KindHosting}
	if TaskTitle(TaskSuspend, h) != "Suspend x.com.au manually at hosting.com" || TaskTitle(TaskRestore, h) != "Unsuspend x.com.au manually at hosting.com" {
		t.Fatalf("titles: %s / %s", TaskTitle(TaskSuspend, h), TaskTitle(TaskRestore, h))
	}
	if n := strings.Join(TaskSteps(TaskSuspend, Target{Domain: "y.com", Kind: KindHosting}), "\n"); !strings.Contains(n, "none is set") {
		t.Fatalf("no supplier:\n%s", n)
	}
	dom := Target{Supplier: SupplierSynergy, Domain: "z.co.ke", Kind: KindDomain}
	if !strings.Contains(TaskTitle(TaskSuspend, dom), "Hold the renewal of z.co.ke") || !strings.Contains(strings.Join(TaskSteps(TaskSuspend, dom), " "), "auto-renew") {
		t.Fatal("a domain-only subscription holds the renewal")
	}
	if domainThreshold([]int{30, 14, 7}, 31) != 0 || domainThreshold([]int{30, 14, 7}, 30) != 30 || domainThreshold([]int{30, 14, 7}, 13) != 14 ||
		domainThreshold([]int{30, 14, 7}, 3) != 7 || domainThreshold([]int{30, 14, 7}, -1) != -1 {
		t.Fatal("domain thresholds")
	}
}
