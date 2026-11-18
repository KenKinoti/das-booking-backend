package scout

// DB tests: TEST_DATABASE_URL (a Postgres database; each test gets its own
// schema with the tables the scout reads and writes).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/assistant"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/crm"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/insights"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

const (
	orgA  = "org-a"
	orgB  = "org-b"
	owner = "user-owner"
)

func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	schema := "scout_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	admin.Exec("CREATE SCHEMA " + schema)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := db.DB(); err == nil {
			d.Close()
		}
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if d, err := admin.DB(); err == nil {
			d.Close()
		}
	})
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) PRIMARY KEY, name text, country_code varchar(2), currency varchar(3), timezone text, region_source text, address_country text)`,
		`CREATE TABLE users (id varchar(255) PRIMARY KEY, organization_id varchar(255), email text, first_name text, last_name text, role text, is_active boolean DEFAULT true, created_at timestamptz DEFAULT now(), deleted_at timestamptz)`,
		`CREATE TABLE customers (id varchar(255) PRIMARY KEY, organization_id varchar(255), first_name text, last_name text, company_name text, email text, country_code text, currency text, created_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE services (id varchar(255) PRIMARY KEY, organization_id varchar(255), name text, price numeric, currency varchar(3), is_active boolean DEFAULT true, deleted_at timestamptz)`,
		`CREATE TABLE products (id varchar(255) PRIMARY KEY, organization_id varchar(255), name text, selling_price numeric, is_active boolean DEFAULT true, deleted_at timestamptz)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(&invoicing.Invoice{}, &invoicing.InvoiceItem{}, &crm.Lead{}, &crm.Opportunity{}, &insights.ServiceRule{}, &insights.IntelSettings{}); err != nil {
		t.Fatal(err)
	}
	if err := notify.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES (?, 'Example Web Co', 'KE', 'KES', 'Africa/Nairobi', 'user'), (?, 'Other Org', 'AU', 'AUD', 'Australia/Sydney', 'user')`, orgA, orgB)
	db.Exec(`INSERT INTO users (id, organization_id, email, first_name, last_name, role) VALUES (?, ?, 'owner@example.test', 'Olive', 'Owner', 'super_admin'), ('user-admin', ?, 'admin@example.test', 'Ada', 'Admin', 'admin')`, owner, orgA, orgA)
	return db
}

// fakeAPI answers like the Messages API with the web search tool: the
// results below, and an answer built by `answer` from the prompt's date.
type fakeAPI struct {
	mu      sync.Mutex
	srv     *httptest.Server
	calls   int
	prompts []string
	tools   []map[string]any
	answer  func(n int, today time.Time) map[string]any
	usage   [2]int // input, output
}

var fakeResults = []map[string]any{
	{"type": "web_search_result", "url": "https://tenders.go.ke/tender/kajiado-0417", "title": "County website and hosting — Kajiado", "encrypted_content": "e1", "page_age": "3 days ago"},
	{"type": "web_search_result", "url": "https://www2.fundsforngos.org/digital-skills-grant/", "title": "Digital skills grant for CBOs", "encrypted_content": "e2", "page_age": "5 days ago"},
	{"type": "web_search_result", "url": "https://www.ungm.org/Public/Notice/261044", "title": "Hosting services (Nairobi)", "encrypted_content": "e3"},
	{"type": "web_search_result", "url": "https://www.tenders.gov.au/Atm/Show/abc", "title": "Accessibility uplift — council", "encrypted_content": "e4"},
}

var reToday = regexp.MustCompile(`Today is (\d{4}-\d{2}-\d{2})`)

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{usage: [2]int{20000, 2000}}
	f.answer = func(n int, today time.Time) map[string]any {
		day := func(k int) string { return today.AddDate(0, 0, k).Format("2006-01-02") }
		src := func(u string) []map[string]any { return []map[string]any{{"title": "Source", "url": u}} }
		if n == 1 {
			return map[string]any{"opportunities": []map[string]any{
				{"title": "County website and hosting tender — Kajiado County", "type": "tender", "buyer": "County Government of Kajiado", "summary": "Design, host and maintain the county website.",
					"why_it_fits": "Web development and hosting in Kenya.", "est_value": 2500000, "currency": "KES", "deadline": day(20), "country": "KE", "effort": "medium", "confidence": "high",
					"next_step": "Download the tender document.", "sources": src("https://tenders.go.ke/tender/kajiado-0417")},
				{"title": "Digital skills grant for community-based organisations", "type": "grant", "summary": "Up to US$15,000 for CBO websites.", "why_it_fits": "Your NGO clients.",
					"est_value": "US$15,000", "deadline": day(35), "country": "KE", "effort": "low", "confidence": "medium", "next_step": "Tell CBO clients.", "sources": src("https://www2.fundsforngos.org/digital-skills-grant/")},
				{"title": "Managed hosting for a Nairobi UN office", "type": "lead", "buyer": "UN agency", "summary": "RFQ for hosting.", "est_value": 18000, "currency": "USD", "deadline": day(12),
					"country": "Kenya", "sources": src("https://www.ungm.org/Public/Notice/261044")},
				{"title": "Invented framework tender in Narok", "type": "tender", "deadline": day(25), "country": "KE", "sources": src("https://narok-tenders.example.org/notice/77")},
				{"title": "County website & hosting tender (Kajiado)", "type": "tender", "sources": src("https://tenders.go.ke/tender/kajiado-0417")},
				{"title": "Website redesign for a water services board", "type": "tender", "deadline": day(-5), "country": "KE", "sources": src("https://www.ungm.org/Public/Notice/261044")},
				{"title": "Road construction works in Kajiado", "type": "tender", "est_value": 90000000, "currency": "KES", "deadline": day(30), "country": "KE", "sources": src("https://tenders.go.ke/tender/kajiado-0417")},
			}, "market_signals": []map[string]any{
				{"market": "Kenya — counties", "country": "KE", "sector": "Government", "signal": "Counties moved procurement to e-GP.", "so_what": "Register on e-GP.", "sources": src("https://tenders.go.ke/tender/kajiado-0417")},
			}}
		}
		return map[string]any{"opportunities": []map[string]any{
			{"title": "County website and hosting tender — Kajiado County", "type": "tender", "deadline": day(20), "country": "KE", "sources": src("https://tenders.go.ke/tender/kajiado-0417")},
			{"title": "Website accessibility uplift for a regional council", "type": "tender", "est_value": "A$85,000", "deadline": day(14), "country": "AU", "sources": src("https://www.tenders.gov.au/Atm/Show/abc")},
		}, "market_signals": []map[string]any{}}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			Tools []map[string]any `json:"tools"`
		}
		if err := json.Unmarshal(b, &req); err != nil || len(req.Messages) == 0 {
			t.Errorf("bad request %s", b)
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.calls++
		n := f.calls
		f.prompts = append(f.prompts, req.Messages[0].Content)
		f.tools = append(f.tools, req.Tools...)
		usage := f.usage
		f.mu.Unlock()
		m := reToday.FindStringSubmatch(req.Messages[0].Content)
		today, _ := time.Parse("2006-01-02", m[1])
		js, _ := json.Marshal(f.answer(n, today))
		content := []map[string]any{
			{"type": "text", "text": "Searching."},
			{"type": "server_tool_use", "id": "srvtoolu_a", "name": "web_search", "input": map[string]any{"query": "kenya county website tender"}},
			{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_a", "content": fakeResults},
			{"type": "text", "text": string(js)},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apiAnswer(content, "end_turn", usage[0], usage[1], 2))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newSvc(t *testing.T, db *gorm.DB, f *fakeAPI, now *time.Time) *Service {
	s := &Service{DB: db, AppURL: "https://app.example.test"}
	s.Now = func() time.Time { return *now }
	if f != nil {
		s.AI = func() assistant.Config {
			return assistant.Config{APIKey: "sk-ant-test", Model: "claude-sonnet-4-5", BaseURL: f.srv.URL, Source: assistant.SourceSettings}
		}
	} else {
		s.AI = func() assistant.Config {
			return assistant.Config{Model: "claude-sonnet-4-5", Source: assistant.SourceNone}
		}
	}
	notify.Disabled = false
	return s
}

func seedBusiness(t *testing.T, db *gorm.DB) {
	cust := func(id, company, first, cc, cur string) {
		if err := db.Exec(`INSERT INTO customers (id, organization_id, first_name, last_name, company_name, country_code, currency) VALUES (?, ?, ?, '', ?, ?, ?)`, id, orgA, first, company, cc, cur).Error; err != nil {
			t.Fatal(err)
		}
	}
	cust("c1", "Example Women Group CBO", "", "KE", "")
	cust("c2", "Sample Training College", "", "KE", "")
	cust("c3", "Sydney Example Foundation", "", "AU", "AUD")
	cust("c4", "", "Jane Example", "", "KES") // no country: currency → Kenya
	cust("c5", "Hilltop Lodge & Safaris", "", "", "")
	db.Exec(`INSERT INTO customers (id, organization_id, first_name, last_name, company_name, country_code) VALUES ('cx', ?, 'Other', 'Org', 'Other Org Customer', 'AU')`, orgB)
	inv := func(cid, cur string, days int, lines ...any) {
		id := uuid.NewString()
		c := cid
		issue := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -days)
		if err := db.Create(&invoicing.Invoice{ID: id, OrganizationID: orgA, DocType: "invoice", Number: "INV-" + id[:6], Status: "sent", CustomerID: &c, ClientName: "Client",
			IssueDate: issue, DueDate: issue.AddDate(0, 0, 14), Currency: cur, PublicToken: uuid.NewString()}).Error; err != nil {
			t.Fatal(err)
		}
		for k := 0; k+1 < len(lines); k += 2 {
			db.Create(&invoicing.InvoiceItem{ID: uuid.NewString(), InvoiceID: id, Position: k, Description: lines[k].(string), Quantity: 1, UnitPrice: lines[k+1].(float64), LineTotal: lines[k+1].(float64)})
		}
	}
	inv("c1", "KES", 20, "Web hosting — examplecbo.org (12 months)", 12000.0, "Website design & development — deposit", 60000.0)
	inv("c2", "KES", 60, "Web hosting — college.example (12 months)", 14000.0, "Google Workspace Business Starter — 3 users", 25000.0)
	inv("c3", "AUD", 90, "Website maintenance & support — quarter", 450.0)
	inv("c4", "KES", 900, "Web hosting — ancient (12 months)", 9000.0) // older than 24 months: ignored
	db.Exec(`INSERT INTO services (id, organization_id, name, price, currency) VALUES ('s1', ?, 'WordPress website package', 85000, 'KES'), ('s2', ?, 'DASYIN ERP licence (annual)', 120000, 'KES')`, orgA, orgA)
	db.Create(&insights.IntelSettings{OrganizationID: orgA, TargetMarkets: []string{"UG", "TZ"}})
}

func TestProfileBuildAndDefaults(t *testing.T) {
	db := testDB(t)
	seedBusiness(t, db)
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	s := newSvc(t, db, nil, &now)
	s.BusinessType = func(string) string { return "Web agency" }
	st, err := s.Settings(orgA)
	if err != nil {
		t.Fatal(err)
	}
	p := st.Profile
	if p.OrgName != "Example Web Co" || p.HomeCountry != "KE" || p.Currency != "KES" || p.Timezone != "Africa/Nairobi" || p.BusinessType != "Web agency" {
		t.Fatalf("profile basics %+v", p)
	}
	names := []string{}
	for _, o := range p.Offerings {
		names = append(names, fmt.Sprintf("%s/%s/%v/%s/%d", o.Name, o.Source, o.Price, o.Currency, o.Sold))
	}
	all := strings.Join(names, "; ")
	for _, want := range []string{"Hosting/invoices/13000/KES/2", "Web development/invoices/60000/KES/1", "Google Workspace & email/invoices/25000/KES/1",
		"Website maintenance & support/invoices/450/AUD/1", "WordPress website package/services/85000/KES/0", "DASYIN ERP licence (annual)/services/120000/KES/0"} {
		if !strings.Contains(all, want) {
			t.Errorf("offerings lack %s: %s", want, all)
		}
	}
	if strings.Contains(all, "examplecbo") || strings.Contains(all, "college.example") {
		t.Fatalf("client details leaked into the profile: %s", all)
	}
	if len(p.Countries) != 2 || p.Countries[0].Key != "KE" || p.Countries[0].Customers != 4 || p.Countries[1].Key != "AU" || p.Countries[1].Customers != 1 {
		t.Fatalf("countries %+v", p.Countries)
	}
	sec := map[string]int{}
	for _, x := range p.Sectors {
		sec[x.Key] = x.Customers
	}
	if sec["ngo"] != 2 || sec["education"] != 1 || sec["hospitality"] != 1 || sec["other"] != 1 {
		t.Fatalf("sectors %+v", p.Sectors)
	}
	if strings.Join(p.TargetMarkets, ",") != "UG,TZ" {
		t.Fatalf("target markets %v", p.TargetMarkets)
	}
	f := st.Focus
	if strings.Join(f.Countries, ",") != "KE,AU,UG,TZ" || len(f.Types) != 6 || f.MinValueCurrency != "KES" || st.Enabled || st.MonthlyBudgetUSD != 10 || st.DailySearches != 5 || st.WeeklySearches != 20 {
		t.Fatalf("defaults %+v / %+v", f, st)
	}
	dom := []string{}
	for _, x := range f.Sources {
		dom = append(dom, x.Domain)
	}
	if !strings.Contains(strings.Join(dom, ","), "tenders.go.ke") || !strings.Contains(strings.Join(dom, ","), "tenders.gov.au") || !strings.Contains(strings.Join(dom, ","), "ungm.org") {
		t.Fatalf("sources %v", dom)
	}
	// Edits are cleaned; the description survives a profile refresh.
	desc := "We build websites and host them for NGOs."
	p.Description = desc
	foc := Focus{Countries: []string{"Kenya", "au", "Narnia"}, Types: []string{"tenders", "grant", "bogus"}, Sources: []Source{{Domain: "https://www.UNGM.org/", Label: "UN"}, {Domain: "not a domain"}},
		BlockedDomains: []string{"spam.example.com"}, MinValue: 50000, MinValueCurrency: "usd", Keywords: []string{"hosting", "Hosting", " "}}
	if _, err := s.SaveSettings(orgA, owner, false, SettingsPatch{Profile: &p, Focus: &foc}); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Settings(orgA)
	if strings.Join(st.Focus.Countries, ",") != "KE,AU" || strings.Join(st.Focus.Types, ",") != "tender,grant" || len(st.Focus.Sources) != 1 || st.Focus.Sources[0].Domain != "ungm.org" ||
		st.Focus.MinValueCurrency != "USD" || len(st.Focus.Keywords) != 1 {
		t.Fatalf("cleaned focus %+v", st.Focus)
	}
	budget := 25.0
	if _, err := s.SaveSettings(orgA, "user-admin", false, SettingsPatch{MonthlyBudgetUSD: &budget}); err != ErrForbidden {
		t.Fatalf("budget needs a super admin: %v", err)
	}
	st, _ = s.RefreshProfile(orgA, owner)
	if st.Profile.Description != desc || len(st.Profile.Offerings) < 6 {
		t.Fatalf("refresh %+v", st.Profile)
	}
	// Turning it on needs an API key.
	if _, err := s.Enable(orgA, owner, 10, 6); err == nil || !strings.Contains(err.Error(), "System settings → AI assistant") {
		t.Fatalf("enable without key: %v", err)
	}
}

// enableA switches the scout on for org A with the fake API.
func enableA(t *testing.T, s *Service, budget float64) {
	if _, err := s.Settings(orgA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enable(orgA, owner, budget, 6); err != nil {
		t.Fatal(err)
	}
}

func scanNow(t *testing.T, s *Service, kind string) *Run {
	r, err := s.Start(orgA, StartOpts{Kind: kind, Trigger: "manual", By: owner, ByName: "Olive Owner"})
	if err != nil {
		t.Fatal(err)
	}
	s.Wait()
	got, err := s.GetRun(orgA, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestScanValidateDedupeDismissFeedback(t *testing.T) {
	db := testDB(t)
	seedBusiness(t, db)
	f := newFakeAPI(t)
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	s := newSvc(t, db, f, &now)
	// Off: no scan.
	if _, err := s.Start(orgA, StartOpts{Kind: KindLight}); err == nil || !strings.Contains(err.Error(), "switched off") {
		t.Fatalf("off: %v", err)
	}
	st, _ := s.Settings(orgA)
	st.Focus.Exclusions = []string{"construction"}
	if _, err := s.SaveSettings(orgA, owner, true, SettingsPatch{Focus: &st.Focus}); err != nil {
		t.Fatal(err)
	}
	enableA(t, s, 10)
	r := scanNow(t, s, KindLight)
	if r.Status != RunOK || r.Found != 7 || r.Kept != 3 || r.Unverified != 1 || r.Duplicates != 1 || r.Expired != 1 || r.Filtered != 1 || r.SignalsKept != 1 {
		t.Fatalf("run %+v", r)
	}
	if r.Searches != 2 || r.InputTokens != 20000 || r.OutputTokens != 2000 || r.CostUSD != 0.11 || r.SearchCostUSD != 0.02 || r.MaxSearches != 5 || len(r.Queries) != 1 || r.ResultURLs != 4 {
		t.Fatalf("usage %+v", r)
	}
	if !strings.Contains(r.Message, "3 new") || !strings.Contains(r.Prompt, "Example Web Co") || !strings.Contains(r.Prompt, "Exclude anything about: construction") {
		t.Fatalf("message / prompt %q", r.Message)
	}
	if len(f.tools) != 1 || f.tools[0]["type"] != "web_search_20250305" || f.tools[0]["max_uses"].(float64) != 5 || f.tools[0]["user_location"].(map[string]any)["country"] != "KE" {
		t.Fatalf("tool %+v", f.tools)
	}
	if _, blocked := f.tools[0]["allowed_domains"]; blocked {
		t.Fatal("allowed_domains only with 'only these sources'")
	}
	feed, err := s.List(orgA, Filter{})
	if err != nil || feed.Total != 3 {
		t.Fatalf("feed %v %+v", err, feed)
	}
	top := feed.Items[0]
	if top.Title != "County website and hosting tender — Kajiado County" || top.Fit < 60 || len(top.Sources) != 1 || top.Sources[0].URL != "https://tenders.go.ke/tender/kajiado-0417" {
		t.Fatalf("top %+v", top)
	}
	for _, it := range feed.Items {
		if strings.Contains(it.Title, "Invented") || strings.Contains(it.Title, "water services") || strings.Contains(it.Title, "construction") {
			t.Fatalf("dropped item stored: %s", it.Title)
		}
		if it.Fit < feed.Items[len(feed.Items)-1].Fit {
			t.Fatal("not ranked by fit")
		}
	}
	var lead Opportunity
	db.Where("organization_id = ? AND type = ?", orgA, TypeLead).First(&lead)
	if lead.Country != "KE" || lead.Currency != "USD" || lead.ValueHome != nil {
		t.Fatalf("lead %+v", lead) // no FX service in the test: no home value
	}
	// Facets / filters.
	if len(feed.Types) != 3 || len(feed.Countries) != 1 || feed.Countries[0].Key != "KE" || feed.Countries[0].Count != 3 || feed.Statuses[StatusNew] != 3 {
		t.Fatalf("facets %+v %+v %+v", feed.Types, feed.Countries, feed.Statuses)
	}
	if f2, _ := s.List(orgA, Filter{Type: TypeGrant, Sort: "deadline"}); f2.Total != 1 || len(f2.Types) != 3 {
		t.Fatalf("facets ignore the type filter: %+v", f2.Types)
	}
	if f2, _ := s.List(orgA, Filter{Type: TypeGrant}); f2.Total != 1 {
		t.Fatalf("type filter %d", f2.Total)
	}
	if f2, _ := s.List(orgA, Filter{Deadline: "7"}); f2.Total != 0 {
		t.Fatalf("deadline 7 %d", f2.Total)
	}
	if f2, _ := s.List(orgA, Filter{Deadline: "30"}); f2.Total != 2 {
		t.Fatalf("deadline 30 %d", f2.Total)
	}
	if f2, _ := s.List(orgB, Filter{Status: "all"}); f2.Total != 0 {
		t.Fatal("other organisation sees nothing")
	}
	// Second scan: the tender again → refreshed, not duplicated.
	now = now.Add(24 * time.Hour)
	r2 := scanNow(t, s, KindLight)
	if r2.Kept != 1 || r2.Updated != 1 {
		t.Fatalf("second run %+v", r2)
	}
	var tender Opportunity
	db.Where("organization_id = ? AND title LIKE 'County website%'", orgA).First(&tender)
	if tender.SeenCount != 2 {
		t.Fatalf("seen %d", tender.SeenCount)
	}
	if !strings.Contains(f.prompts[1], "## Already in the feed") || !strings.Contains(f.prompts[1], "County website and hosting tender — Kajiado County") {
		t.Fatalf("second prompt must list the feed:\n%s", f.prompts[1])
	}
	// Dismiss with a reason → the next prompt says "not interested".
	var au Opportunity
	db.Where("organization_id = ? AND country = 'AU'", orgA).First(&au)
	if _, err := s.Dismiss(orgA, au.ID, owner, "nope", ""); err == nil {
		t.Fatal("unknown reason")
	}
	if _, err := s.Dismiss(orgA, au.ID, owner, "wrong_country", "Not bidding in Australia this year"); err != nil {
		t.Fatal(err)
	}
	r3 := scanNow(t, s, KindLight)
	if r3.Status != RunOK {
		t.Fatalf("third %+v", r3)
	}
	p := f.prompts[2]
	if !strings.Contains(p, "## Not interested") || !strings.Contains(p, `"Website accessibility uplift for a regional council" — Wrong country or region: Not bidding in Australia this year`) {
		t.Fatalf("dismissed item missing from the prompt:\n%s", p)
	}
	if strings.Contains(p[strings.Index(p, "## Already in the feed"):], "accessibility uplift") {
		t.Fatal("a dismissed card is not 'already in the feed'")
	}
	// The dismissed card stays dismissed when it shows up again.
	got, _ := s.Get(orgA, au.ID)
	if got.Status != StatusDismissed || got.SeenCount != 2 {
		t.Fatalf("dismissed again %+v", got)
	}
	if fd, _ := s.List(orgA, Filter{Status: StatusDismissed}); fd.Total != 1 {
		t.Fatal("dismissed filter")
	}
	// Restore.
	if o, _ := s.Update(orgA, au.ID, ItemPatch{Restore: true}); o.Status != StatusNew {
		t.Fatalf("restore %+v", o)
	}
	// Runs list and month totals.
	rl, _ := s.Runs(orgA, 10)
	if len(rl.Runs) != 3 || rl.Totals.Runs != 3 || rl.Totals.Searches != 6 || rl.Spent != 0.33 || rl.Runs[0].Prompt != "" {
		t.Fatalf("runs %+v", rl.Totals)
	}
	// Markets.
	mk, _ := s.Markets(orgA, 90)
	if len(mk) != 1 || mk[0].Country != "KE" || len(mk[0].Signals) != 1 {
		t.Fatalf("markets %+v", mk)
	}
	// One scan at a time.
	db.Create(&Run{ID: uuid.NewString(), OrganizationID: orgA, Kind: KindLight, Status: RunRunning, StartedAt: now})
	if _, err := s.Start(orgA, StartOpts{Kind: KindLight}); err != ErrBusy {
		t.Fatalf("busy: %v", err)
	}
}

func TestBudgetCapStopsRun(t *testing.T) {
	db := testDB(t)
	f := newFakeAPI(t)
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	s := newSvc(t, db, f, &now)
	enableA(t, s, 1)
	// Spent this month: US$0.95 of US$1.00 — even one search's worst case does not fit.
	db.Create(&Run{ID: uuid.NewString(), OrganizationID: orgA, Kind: KindLight, Status: RunOK, StartedAt: now.Add(-time.Hour), CostUSD: 0.95})
	// Last month's spend does not count.
	db.Create(&Run{ID: uuid.NewString(), OrganizationID: orgA, Kind: KindDeep, Status: RunOK, StartedAt: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), CostUSD: 5})
	r, err := s.Start(orgA, StartOpts{Kind: KindLight, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != RunBudget || !strings.Contains(r.Message, "Monthly budget reached: US$0.95 of US$1.00") || !strings.Contains(r.Message, "1 December") {
		t.Fatalf("budget run %+v", r)
	}
	if f.calls != 0 {
		t.Fatal("the API must not be called once the budget is reached")
	}
	time.Sleep(300 * time.Millisecond) // notify.Go is asynchronous
	var notes []notify.Notification
	db.Where("organization_id = ? AND type = ?", orgA, "scout.budget").Find(&notes)
	if len(notes) != 2 { // the super admin (enabled it) and the admin
		t.Fatalf("budget notifications %d", len(notes))
	}
	// Once a month only.
	s.Start(orgA, StartOpts{Kind: KindLight})
	time.Sleep(200 * time.Millisecond)
	var n int64
	db.Model(&notify.Notification{}).Where("type = ?", "scout.budget").Count(&n)
	if n != 2 {
		t.Fatalf("notified again: %d", n)
	}
	st, _ := s.StatusFor(orgA)
	if !st.BudgetHit || st.Spent != 0.95 || st.Left != 0.05 {
		t.Fatalf("status %+v", st)
	}
	// With US$0.30 left a 5-search scan is limited to what fits.
	db.Model(&Run{}).Where("organization_id = ? AND cost_usd = 0.95", orgA).Update("cost_usd", 0.70)
	r, err = s.Start(orgA, StartOpts{Kind: KindLight})
	if err != nil {
		t.Fatal(err)
	}
	s.Wait()
	r, _ = s.GetRun(orgA, r.ID)
	if r.Status != RunOK || r.MaxSearches >= 5 || r.MaxSearches < 1 || !strings.Contains(r.Message, "to stay within the monthly budget") || r.EstimateUSD > 0.30 {
		t.Fatalf("limited run %+v", r)
	}
	// A run that takes the month over the budget notifies (new month → new notice).
	now = time.Date(2026, 12, 2, 9, 0, 0, 0, time.UTC)
	db.Create(&Run{ID: uuid.NewString(), OrganizationID: orgA, Kind: KindLight, Status: RunOK, StartedAt: now.Add(-time.Hour), CostUSD: 0.60})
	f.usage = [2]int{120000, 4000} // ≈ US$0.44 + searches
	r = scanNow(t, s, KindLight)
	if r.Status != RunOK || r.CostUSD < 0.4 {
		t.Fatalf("december run %+v", r)
	}
	time.Sleep(300 * time.Millisecond)
	db.Model(&notify.Notification{}).Where("type = ?", "scout.budget").Count(&n)
	if n != 4 {
		t.Fatalf("december notice: %d", n)
	}
	// The worst case learns from that heavy scan: (120,000 − 3,500) / 2 searches × 1.25.
	if per := s.PerSearchWorst(orgA); per != 72812 {
		t.Fatalf("per-search worst case %d", per)
	}
	if WorstCasePer("claude-sonnet-4-5", 5, LightMaxTokens, s.PerSearchWorst(orgA)) <= WorstCase("claude-sonnet-4-5", 5, LightMaxTokens) {
		t.Fatal("the calibrated worst case must be higher")
	}
}

func TestScheduledScansInOrgTimezone(t *testing.T) {
	db := testDB(t)
	f := newFakeAPI(t)
	// Sunday 22 Nov 2026 10:00 Nairobi: enabled today → first scheduled scan is tomorrow.
	now := time.Date(2026, 11, 22, 7, 0, 0, 0, time.UTC)
	s := newSvc(t, db, f, &now)
	enableA(t, s, 10)
	ctx := context.Background()
	if res := s.RunScheduled(ctx, true); len(res.Scans) != 0 {
		t.Fatalf("enabled today: %+v", res)
	}
	now = time.Date(2026, 11, 23, 3, 20, 0, 0, time.UTC) // Monday 06:20 Nairobi
	if res := s.RunScheduled(ctx, true); len(res.Scans) != 0 {
		t.Fatalf("before 06:30: %+v", res)
	}
	now = time.Date(2026, 11, 23, 3, 31, 0, 0, time.UTC) // Monday 06:31 Nairobi → weekly deep scan
	res := s.RunScheduled(ctx, true)
	if len(res.Scans) != 1 || res.Scans[0].Kind != KindDeep || res.Scans[0].Status != RunOK || res.Scans[0].Slot != "2026-11-23" {
		t.Fatalf("monday: %+v", res)
	}
	r, _ := s.GetRun(orgA, res.Scans[0].RunID)
	if r.MaxSearches != 20 || r.MaxTokens != DeepMaxTokens || r.Trigger != "schedule" || !strings.Contains(f.prompts[0], "weekly deep scan: use at most 20 web searches") {
		t.Fatalf("deep run %+v", r)
	}
	now = now.Add(2 * time.Hour)
	if res := s.RunScheduled(ctx, true); len(res.Scans) != 0 {
		t.Fatal("one scan per day")
	}
	now = time.Date(2026, 11, 24, 3, 45, 0, 0, time.UTC) // Tuesday 06:45 → light
	res = s.RunScheduled(ctx, true)
	if len(res.Scans) != 1 || res.Scans[0].Kind != KindLight {
		t.Fatalf("tuesday %+v", res)
	}
	// A new scan from the schedule notifies the owner.
	time.Sleep(300 * time.Millisecond)
	var n int64
	db.Model(&notify.Notification{}).Where("type = ? AND user_id = ?", "scout.found", owner).Count(&n)
	if n < 1 {
		t.Fatal("found notification")
	}
	st, _ := s.StatusFor(orgA)
	if len(st.Next) != 2 || st.Next[0].Kind != KindLight || st.Next[0].At.Format("2006-01-02 15:04") != "2026-11-25 06:30" || st.Next[1].At.Format("2006-01-02") != "2026-11-30" {
		t.Fatalf("next %+v", st.Next)
	}
}

func TestPursueRemindExpireAndBrief(t *testing.T) {
	db := testDB(t)
	f := newFakeAPI(t)
	now := time.Date(2026, 11, 18, 5, 0, 0, 0, time.UTC) // 08:00 Nairobi
	s := newSvc(t, db, f, &now)
	if _, ok := BriefSection(context.Background(), db, orgA, owner, now.Add(-24*time.Hour), 5); ok {
		t.Fatal("never enabled: no section")
	}
	st, _ := s.Settings(orgA)
	st.Focus.Exclusions = []string{"construction"}
	if _, err := s.SaveSettings(orgA, owner, true, SettingsPatch{Focus: &st.Focus}); err != nil {
		t.Fatal(err)
	}
	enableA(t, s, 10)
	if _, ok := BriefSection(context.Background(), db, orgA, owner, now.Add(-24*time.Hour), 5); ok {
		t.Fatal("nothing found yet: no section")
	}
	scanNow(t, s, KindLight)
	var tender, lead Opportunity
	db.Where("organization_id = ? AND title LIKE 'County website%'", orgA).First(&tender)
	db.Where("organization_id = ? AND type = ?", orgA, TypeLead).First(&lead)
	s.Update(orgA, tender.ID, ItemPatch{Notes: strPtr("Call the county ICT officer first.")})

	// Pursue → a CRM deal with the details and the source links.
	res, err := s.Pursue(orgA, tender.ID, owner, "super_admin", "")
	if err != nil {
		t.Fatal(err)
	}
	var deal crm.Opportunity
	if err := db.Where("organization_id = ? AND id = ?", orgA, res.ID).First(&deal).Error; err != nil || res.Kind != "deal" {
		t.Fatalf("deal %v %+v", err, res)
	}
	if deal.Name != tender.Title || deal.Value != 2500000 || deal.Stage != "prospecting" || deal.Company != "County Government of Kajiado" || deal.ExpectedCloseDate == nil ||
		!strings.Contains(deal.Notes, "https://tenders.go.ke/tender/kajiado-0417") || !strings.Contains(deal.Notes, "Call the county ICT officer first.") ||
		!strings.Contains(deal.Notes, "https://app.example.test/insights?tab=scout&opp="+tender.ID) || !strings.Contains(deal.Notes, "fit ") {
		t.Fatalf("deal %+v", deal)
	}
	again, _ := s.Pursue(orgA, tender.ID, owner, "super_admin", "")
	var nd int64
	db.Model(&crm.Opportunity{}).Where("organization_id = ?", orgA).Count(&nd)
	if again.ID != res.ID || nd != 1 {
		t.Fatal("pursuing twice must not create a second deal")
	}
	if o, _ := s.Get(orgA, tender.ID); o.Status != StatusPursued || o.CRMKind != "deal" {
		t.Fatalf("status %+v", o)
	}
	// A lead card becomes a CRM lead; CRM not allowed → a clear refusal.
	s.CRMAllowed = func(org, user, role string) (bool, string) { return false, "CRM is not in your plan." }
	if _, err := s.Pursue(orgA, lead.ID, owner, "admin", ""); err == nil || !strings.Contains(err.Error(), "CRM is not in your plan.") {
		t.Fatalf("crm refused: %v", err)
	}
	s.CRMAllowed = nil
	lr, err := s.Pursue(orgA, lead.ID, owner, "admin", "")
	var l crm.Lead
	if err != nil || lr.Kind != "lead" || db.Where("id = ?", lr.ID).First(&l).Error != nil || l.Source != "market_scout" || l.Company != "UN agency" || !strings.Contains(l.Notes, "ungm.org") {
		t.Fatalf("lead %v %+v %+v", err, lr, l)
	}

	// Remind me: the grant, today → notification at 08:00 Nairobi, and the brief lists it first.
	var grant Opportunity
	db.Where("organization_id = ? AND type = ?", orgA, TypeGrant).First(&grant)
	yesterday := now.AddDate(0, 0, -1)
	if _, err := s.Remind(orgA, grant.ID, owner, &yesterday); err == nil {
		t.Fatal("past date")
	}
	today := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	if o, err := s.Remind(orgA, grant.ID, owner, &today); err != nil || o.Status != StatusSaved || o.RemindOn == nil {
		t.Fatalf("remind %v %+v", err, o)
	}
	now = time.Date(2026, 11, 18, 4, 30, 0, 0, time.UTC) // 07:30 Nairobi: not yet
	if n := s.sendReminders(); n != 0 {
		t.Fatal("before 08:00")
	}
	now = time.Date(2026, 11, 18, 5, 5, 0, 0, time.UTC)
	if n := s.sendReminders(); n != 1 {
		t.Fatalf("reminders %d", n)
	}
	if n := s.sendReminders(); n != 0 {
		t.Fatal("once")
	}
	time.Sleep(300 * time.Millisecond)
	var rem notify.Notification
	if db.Where("type = ? AND user_id = ?", "scout.reminder", owner).First(&rem).Error != nil || !strings.HasPrefix(rem.Title, "Reminder: ") || rem.Link != CardPath(grant.ID) {
		t.Fatalf("reminder notification %+v", rem)
	}

	os.Setenv("PUBLIC_APP_URL", "https://app.example.test")
	defer os.Unsetenv("PUBLIC_APP_URL")
	testOffset.Store(int64(time.Until(now))) // BriefSection uses the package clock
	defer testOffset.Store(0)
	b, ok := BriefSection(context.Background(), db, orgA, owner, now.Add(-12*time.Hour), 5)
	if !ok || b.Key != "market_scout" || b.Title != "Market scout" || b.Link != "https://app.example.test/insights?tab=scout" {
		t.Fatalf("brief %+v", b)
	}
	if len(b.Items) != 1 || b.Items[0].ID != grant.ID || b.Items[0].Why != "reminder" || b.Items[0].Value != "USD 15,000" || b.Items[0].DaysLeft == nil || *b.Items[0].DaysLeft != 35 ||
		b.Items[0].URL != "https://app.example.test/insights?tab=scout&opp="+grant.ID || b.Items[0].Fit <= 0 || b.Items[0].SourceURL == "" || b.Reminders != 1 || b.New != 1 {
		t.Fatalf("brief items %+v", b.Items)
	}
	if !strings.Contains(b.Headline, "1 new opportunity") || !strings.Contains(b.Headline, "1 reminder for today") || !strings.Contains(b.Headline, "best fit") {
		t.Fatalf("headline %q", b.Headline)
	}
	// Someone else: no reminder, the grant is listed as new.
	b2, ok := BriefSection(context.Background(), db, orgA, "user-admin", now.Add(-12*time.Hour), 5)
	if !ok || b2.Items[0].Why != "new" || b2.Reminders != 0 {
		t.Fatalf("brief for another user %+v", b2)
	}
	// Nothing new since now → only closing-soon cards (none within 7 days here) → no section.
	if _, ok := BriefSection(context.Background(), db, orgA, "user-admin", now.Add(time.Minute), 5); ok {
		t.Fatal("nothing to say")
	}
	// Deadline within 7 days → closing soon.
	soon := today.AddDate(0, 0, 3)
	db.Model(&Opportunity{}).Where("id = ?", grant.ID).Update("deadline", soon)
	b3, ok := BriefSection(context.Background(), db, orgA, "user-admin", now.Add(time.Minute), 5)
	if !ok || len(b3.Items) != 1 || b3.Items[0].Why != "closing_soon" || b3.Closing != 1 || b3.Items[0].When != "closes in 3 days" {
		t.Fatalf("closing soon %+v", b3)
	}
	// Expiry: past the deadline → expired, out of the feed and the brief.
	now = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	testOffset.Store(int64(time.Until(now)))
	feed, _ := s.List(orgA, Filter{})
	for _, it := range feed.Items {
		if it.ID == grant.ID {
			t.Fatal("expired card still in the feed")
		}
	}
	if o, _ := s.Get(orgA, grant.ID); o.Status != StatusExpired {
		t.Fatalf("expired %+v", o)
	}
	if _, ok := BriefSection(context.Background(), db, orgA, "user-admin", now.Add(time.Minute), 5); ok {
		t.Fatal("expired card in the brief")
	}
}

func strPtr(s string) *string { return &s }
