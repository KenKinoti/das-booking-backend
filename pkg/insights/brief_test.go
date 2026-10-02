package insights

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

func TestBriefPeriod(t *testing.T) {
	today := d("2026-10-02")
	from, to, label, partial, err := BriefPeriod("", today)
	if err != nil || from.Format(dayFmt) != "2026-09-01" || to.Format(dayFmt) != "2026-09-30" || label != "September 2026" || partial {
		t.Fatalf("default period: %v %v %s %v %v", from, to, label, partial, err)
	}
	_, to, _, partial, _ = BriefPeriod("2026-10", today)
	if !partial || to.Format(dayFmt) != "2026-10-02" {
		t.Fatalf("current month: %v %v", to, partial)
	}
	for _, bad := range []string{"2026-11", "2026-13", "sep", "2010-01"} {
		if _, _, _, _, err := BriefPeriod(bad, today); err == nil {
			t.Errorf("BriefPeriod(%q) accepted", bad)
		}
	}
}

func briefFixtures(t *testing.T) *Intel {
	i, db := tradeFixtures(t)
	db.Exec(`INSERT INTO users (id, organization_id, email, role, is_active, created_at) VALUES
		('u1', ?, 'owner@example.com', 'admin', true, now()), ('u2', ?, 'staff@example.com', 'staff', true, now()), ('u3', ?, 'gone@example.com', 'admin', false, now())`, org, org, org)
	// Industry watch items of the month (stored, as a refresh would).
	c := i.Context(org)
	for k, it := range []struct{ title, sum, day string }{
		{"Hosting and domain prices rise in Kenya", "Registrars say .co.ke renewals cost more.", "2026-09-12"},
		{"Australia tightens data protection rules for small business", "The Privacy Act changes affect websites.", "2026-09-20"},
		{"A festival opens", "Nothing relevant.", "2026-09-21"},
		{"Old news about hosting", "From last year.", "2025-09-21"},
	} {
		topics, countries, score, reasons := ScoreItem(c, it.title, it.sum, nil, "")
		pub := d(it.day)
		db.Create(&WatchItem{ID: uuid.NewString(), OrganizationID: org, Hash: it.day, FeedID: "f1", Title: it.title, Link: "https://news.example.org/" + it.day, PublishedAt: &pub, Summary: it.sum,
			Source: "Example News", Topics: topics, Countries: countries, Score: score, Reasons: reasons, FirstSeenAt: pub, Dismissed: k == 99})
	}
	return i
}

func TestBriefGenerationIsDeterministic(t *testing.T) {
	i := briefFixtures(t)
	srv := fakeTradeWB(t)
	defer srv.Close()
	i.WB = &WorldBank{BaseURL: srv.URL, Client: srv.Client(), TTL: time.Hour, Now: i.Now}
	ctx := context.Background()
	today := d("2026-09-30")

	a, err := i.BuildBrief(ctx, org, "2026-09", today, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := i.BuildBrief(ctx, org, "2026-09", today, time.UTC)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("two builds on the same data differ:\n%s\n%s", ja, jb)
	}

	// 1. Snapshot: September = AUD 500 @ 80 + AUD 200 @ 100 = 60,000; August = 50,000 (+20%).
	sn := a.Snapshot
	if a.Currency != "KES" || a.PeriodLabel != "September 2026" || a.Partial || a.PrevLabel != "August 2026" {
		t.Fatalf("header %+v", a)
	}
	// The P&L lists KES documents only (none in September): the AUD invoices are added at their locked rates.
	if sn.PnLRevenue != 0 || !near(sn.ForeignRevenue, 60000) || !near(sn.NetProfit, 60000) || !near(sn.PrevNetProfit, 50000) || !strings.Contains(strings.Join(a.Notes, " "), "include KES 60,000 of invoices in other currencies") {
		t.Fatalf("foreign revenue in the snapshot %+v\n%v", sn, a.Notes)
	}
	if !near(sn.Revenue, 60000) || !near(sn.PrevRevenue, 50000) || sn.RevenueDelta == nil || *sn.RevenueDelta != 20 || !near(sn.Invoiced, 60000) || sn.Invoices != 2 || sn.Clients != 2 || sn.NewClients != 1 {
		t.Fatalf("snapshot %+v", sn)
	}
	// Cash: 50 AUD at the locked rate of that invoice (100) = 5,000. Receivables today: 120,000 KES + 63,000 (AUD, locked) + 13,000 (USD, today's rate).
	if !near(sn.Cash, 5000) || !near(sn.Outstanding, 196000) || !near(sn.Overdue, 196000) || sn.OverdueCount != 7 {
		t.Fatalf("cash / receivables %+v", sn)
	}
	// 2. Changes.
	if len(a.Changes) < 4 || a.Changes[0].Key != "revenue" || a.Changes[0].Tone != "good" || !strings.Contains(a.Changes[0].Text, "Revenue KES 60,000, up 20% on August 2026 (KES 50,000).") {
		t.Fatalf("changes %+v", a.Changes)
	}
	txt := ""
	for _, ch := range a.Changes {
		txt += ch.Text + "\n"
	}
	if !strings.Contains(txt, "1 new client invoiced for the first time") || !strings.Contains(txt, "Company K1: KES 0.00 invoiced vs KES 50,000 before (down KES 50,000).") {
		t.Fatalf("changes text:\n%s", txt)
	}
	// 3. Export & FX: the trade view's own figures.
	tr := a.Trade
	if tr.ExportPct == nil || *tr.ExportPct != 48.9 || !near(tr.Export, 153000) || !near(tr.PeriodExport, 60000) || len(tr.Markets) != 2 || tr.Markets[0].Country != "AU" ||
		!near(tr.Unrealised, 4500) || !near(tr.ForeignOpen, 63000) || !near(tr.ForeignToday, 67500) || len(tr.Hedge) != 3 {
		t.Fatalf("trade %+v", tr)
	}
	if !strings.Contains(strings.Join(tr.Lines, "\n"), "KES 4,500 more (unrealised)") || !strings.Contains(tr.Lines[0], "Exports are 48.9% of revenue over the last 12 months") {
		t.Fatalf("trade lines %v", tr.Lines)
	}
	// 4. Signals: no default-indicator data on the fake → none from indicators, but never an error.
	_ = a.Signals
	// 5. Headlines: the month's items by relevance, the old one left out.
	// "Australia … data protection … small business": regulation 18 + clients' sectors 15 + a client country 10 = 43;
	// "Hosting and domain prices rise in Kenya": hosting 21 + home country 15 = 36.
	if len(a.Headlines) != 3 || a.Headlines[0].Title != "Australia tightens data protection rules for small business" || a.Headlines[0].Score != 43 || a.Headlines[0].Link == "" ||
		a.Headlines[0].Date != "2026-09-20" || a.Headlines[0].Why != "Regulation & tax · Clients' sectors · Australia" {
		t.Fatalf("headlines %+v", a.Headlines)
	}
	if a.Headlines[1].Title != "Hosting and domain prices rise in Kenya" || a.Headlines[1].Score != 36 || a.Headlines[2].Score != 0 {
		t.Fatalf("headline order %+v", a.Headlines)
	}
	// 6. Risks: sorted by severity; overdue is 100% of receivables → high.
	keys := map[string]BriefRisk{}
	for k, r := range a.Risks {
		keys[r.Key] = r
		if k > 0 && (map[string]int{"high": 0, "medium": 1, "low": 2})[a.Risks[k-1].Severity] > (map[string]int{"high": 0, "medium": 1, "low": 2})[r.Severity] {
			t.Fatalf("risks not sorted: %+v", a.Risks)
		}
	}
	if r, ok := keys["overdue"]; !ok || r.Severity != "high" || !strings.Contains(r.Text, "KES 196,000 is overdue across 7 invoices — 100% of what clients owe you.") {
		t.Fatalf("overdue risk %+v", r)
	}
	if _, ok := keys["fx_aud"]; !ok {
		t.Fatalf("no AUD exposure risk: %+v", a.Risks)
	}
	if r, ok := keys["export_single_market"]; !ok || !strings.Contains(r.Text, "91.5% of export revenue comes from Australia") {
		t.Fatalf("single market risk %+v", r)
	}
	// 7. Actions: ranked 1..n, from the opportunity rules plus the FX and market actions.
	if len(a.Actions) < 3 {
		t.Fatalf("actions %+v", a.Actions)
	}
	act := map[string]BriefAction{}
	for k, x := range a.Actions {
		act[x.Key] = x
		if x.Rank != k+1 || x.Title == "" || x.Rationale == "" || x.Basis == "" {
			t.Fatalf("action %d incomplete: %+v", k, x)
		}
	}
	if c, ok := act["collection"]; !ok || !near(c.Value, 196000) || c.ValueKind != "one_off" {
		t.Fatalf("collection action %+v", c)
	}
	if e, ok := act["fx_buffer_eur"]; !ok || !near(e.Value, 375) || e.ValueKind != "at_risk" || !strings.Contains(e.Basis, "1 EUR = 150 KES") {
		t.Fatalf("EUR buffer action %+v", e)
	}
	if m, ok := act["market_au"]; !ok || m.ValueKind != "none" || m.Value != 0 || !strings.HasPrefix(m.Title, "Grow Australia") || !strings.Contains(m.Rationale, "rank 1 of") {
		t.Fatalf("market action %+v", m)
	}
	sum := 0.0
	for _, x := range a.Actions {
		if x.ValueKind == "per_year" || x.ValueKind == "one_off" {
			sum += x.Value
		}
	}
	if !near(a.ActionTotal, sum) || a.ActionTotal < 196000 {
		t.Fatalf("action total %v vs %v", a.ActionTotal, sum)
	}
	if h := a.Headline(); !strings.HasPrefix(h, "September 2026: revenue KES 60,000 (+20% on August 2026); net profit KES ") || !strings.Contains(h, "exports 48.9% of 12-month revenue") {
		t.Fatalf("headline %q", h)
	}

	// A month in progress is compared with the same days of the month before.
	p, err := i.BuildBrief(ctx, org, "2026-09", d("2026-09-10"), time.UTC)
	if err != nil || !p.Partial || p.To != "2026-09-10" || p.PrevLabel != "the first 10 days of August" || !near(p.Snapshot.PrevRevenue, 50000) || len(p.Notes) < 3 {
		t.Fatalf("partial month: %+v %v", p, err)
	}
	if _, err := i.BuildBrief(ctx, org, "2027-01", today, time.UTC); err == nil {
		t.Fatal("future month accepted")
	}
}

func TestBriefStoreAIAndEmail(t *testing.T) {
	i := briefFixtures(t)
	ctx := context.Background()
	today := d("2026-09-30")
	b1, err := i.GenerateBrief(ctx, org, "u1", "2026-09", "manual", today, time.UTC)
	if err != nil || b1.GeneratedBy != "rule" || b1.Title != "Strategy brief — September 2026" || b1.Content == nil || b1.Headline == "" {
		t.Fatalf("generate %+v %v", b1, err)
	}
	// Generating the same month again replaces the stored rule-based brief.
	b2, _ := i.GenerateBrief(ctx, org, "u1", "2026-09", "manual", today, time.UTC)
	if b2.ID != b1.ID || len(i.Briefs(org)) != 1 {
		t.Fatalf("regenerate created a second brief: %s vs %s", b1.ID, b2.ID)
	}
	if got := i.BriefByID(org, b1.ID); got == nil || got.Content == nil || got.Content.Snapshot.Revenue != b1.Content.Snapshot.Revenue || len(got.Content.Actions) != len(b1.Content.Actions) {
		t.Fatal("stored content does not round-trip")
	}
	if i.BriefByID("other-org", b1.ID) != nil {
		t.Fatal("brief visible to another organisation")
	}

	// AI version: same data in the prompt, instructed to use only those numbers; stored next to the rule-based one.
	if _, err := i.WriteBriefAI(ctx, org, "u1", b1.ID); err == nil {
		t.Fatal("AI brief without a key")
	}
	var prompt string
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &req)
		prompt = req.Messages[0].Content[0].Text
		if r.Header.Get("x-api-key") != "sk-test" || r.URL.Path != "/v1/messages" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"Summary\nRevenue was KES 60,000.\n\nWhat to do next\n- Collect KES 196,000."}]}`))
	}))
	defer ai.Close()
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_BASE_URL", ai.URL)
	ab, err := i.WriteBriefAI(ctx, org, "u1", b1.ID)
	if err != nil || ab.GeneratedBy != "ai" || ab.ID == b1.ID || !strings.Contains(ab.AIText, "Revenue was KES 60,000.") || ab.AIModel == "" || ab.Content == nil {
		t.Fatalf("AI brief %+v %v", ab, err)
	}
	if !strings.Contains(prompt, "Use ONLY the facts and numbers in the JSON below") || !strings.Contains(prompt, `"revenue":60000`) || !strings.Contains(prompt, `"export_pct":48.9`) {
		t.Fatalf("prompt:\n%s", prompt)
	}
	list := i.Briefs(org)
	if len(list) != 2 || list[0].Content != nil || list[0].AIText != "" {
		t.Fatalf("history %+v", list)
	}
	by := map[string]bool{list[0].GeneratedBy: true, list[1].GeneratedBy: true}
	if !by["rule"] || !by["ai"] {
		t.Fatalf("history should keep both versions: %+v", by)
	}
	// Writing with AI again replaces the AI version only.
	ab2, _ := i.WriteBriefAI(ctx, org, "u1", ab.ID)
	if ab2 == nil || ab2.ID != ab.ID || len(i.Briefs(org)) != 2 {
		t.Fatal("second AI run")
	}

	// Email: active admins only, rendered with the shared template.
	to := i.BriefRecipients(org)
	if len(to) != 1 || to[0] != "owner@example.com" {
		t.Fatalf("recipients %v", to)
	}
	i.AppURL = "https://app.example.com"
	msg := i.BriefMessage(org, b1, to)
	if !strings.HasPrefix(msg.Subject, "Strategy brief — September 2026: revenue KES 60,000") || len(msg.To) != 1 {
		t.Fatalf("subject %q", msg.Subject)
	}
	for _, want := range []string{"WHAT CHANGED", "Revenue KES 60,000, up 20% on August 2026", "EXPORT & FX", "Overdue invoices to collect", "https://app.example.com/insights?tab=brief&brief=" + b1.ID, "Hosting and domain prices rise in Kenya"} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("email text lacks %q", want)
		}
	}
	if !strings.Contains(msg.HTML, "Strategy brief") || !strings.Contains(msg.HTML, "Open the brief") {
		t.Error("email HTML")
	}
	if am := i.BriefMessage(org, ab, to); !strings.Contains(am.Text, "AI-written from your data") || !strings.Contains(am.Text, "Revenue was KES 60,000.") {
		t.Error("AI email")
	}
	// Sending goes through pkg/mail and stamps the brief.
	if _, err := i.EmailBrief(ctx, org, b1.ID); err == nil {
		t.Fatal("email without a mail service")
	}
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_FROM", "erp@example.com")
	var sent []mail.Message
	ms := mail.NewService(i.DB)
	ms.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error { sent = append(sent, m); return nil }
	i.Mail = ms
	got, err := i.EmailBrief(ctx, org, b1.ID)
	if err != nil || len(got) != 1 || len(sent) != 1 || sent[0].To[0] != "owner@example.com" {
		t.Fatalf("email: %v %v sent=%d", got, err, len(sent))
	}
	if bb := i.BriefByID(org, b1.ID); bb.EmailedAt == nil || bb.EmailedTo != "owner@example.com" {
		t.Fatalf("emailed stamp %+v", bb)
	}

	// Scheduler: generates last month's brief once, and emails it when switched on.
	i.DeleteBrief(org, b1.ID)
	i.Now = func() time.Time { return d("2026-10-01").Add(6 * time.Hour) }
	i.Settings(org)
	i.DB.Model(&IntelSettings{}).Where("organization_id = ?", org).Update("brief_email", true)
	sent = nil
	if _, n := i.RunScheduled(ctx); n != 1 {
		t.Fatalf("scheduled briefs = %d, want 1", n)
	}
	rb := i.ruleBrief(org, "2026-09")
	if rb == nil || rb.Trigger != "scheduled" || rb.EmailedAt == nil || len(sent) != 1 {
		t.Fatalf("scheduled brief %+v sent=%d", rb, len(sent))
	}
	if _, n := i.RunScheduled(ctx); n != 0 || len(sent) != 1 {
		t.Fatal("the month's brief was generated twice")
	}
	i.DeleteBrief(org, rb.ID)
	i.DB.Model(&IntelSettings{}).Where("organization_id = ?", org).Update("brief_auto", false)
	if _, n := i.RunScheduled(ctx); n != 0 {
		t.Fatal("brief generated with auto-generation off")
	}
}
