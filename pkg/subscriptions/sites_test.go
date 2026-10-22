package subscriptions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
)

func fp(v float64) *float64 { return &v }

// Synergy hosted sites → subscriptions: one click per site, or every
// matched site at once, priced from the catalogue, the current billing or
// the retail helper.
func TestSynergySitesOneClickAndBulk(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	otieno := f.addCustomer(f.org, "Otieno", "Studio", "otieno@studio.example", "", "", "")
	bondi := f.addCustomer(f.org, "Bondi", "Cafe", "hi@bondi.example", "", "", "AU")
	if _, err := f.s.FX.SaveManual(f.org, f.admin, "AUD", "KES", 85, "test"); err != nil {
		t.Fatal(err)
	}
	f.s.SitesFn = func(org string) []expenses.SynSite {
		if org != f.org {
			return nil
		}
		return []expenses.SynSite{
			{Domain: "wanjiku.example", Plan: "Micro Env", Status: "active", CostHome: 650, CustomerID: f.customer, CustomerName: "Wanjiku Traders", ClientMatch: "website"},
			{Domain: "otieno.example", Plan: "Mega Env", Status: "active", CostHome: 2000, CustomerID: otieno, CustomerName: "Otieno Studio", ClientMatch: "invoice", Billed: fp(3000)},
			{Domain: "bondi.example", Plan: "Large Env", Status: "active", CostHome: 1000, CustomerID: bondi, CustomerName: "Bondi Cafe", ClientMatch: "email"},
			{Domain: "nomatch.example", Plan: "Basic Env", Status: "active", CostHome: 800},
			{Domain: "mine.example", Plan: "Basic Env", Status: "active", CostHome: 800, NoClient: true},
			{Domain: "gone.example", Plan: "Basic Env", Status: "cancelled", CustomerID: f.customer},
		}
	}
	micro := f.service("Micro Env hosting — monthly", 1200, "KES")
	rows := f.s.SynergySites(f.org)
	by := map[string]SiteRow{}
	for _, r := range rows {
		by[r.Domain] = r
	}
	if r := by["wanjiku.example"]; !r.Ready || r.Suggestion.Source != "catalogue" || r.Suggestion.ServiceID != micro || r.Suggestion.Price != 1200 || r.Suggestion.Period != PeriodMonthly {
		t.Fatalf("catalogue price: %+v %+v", r, r.Suggestion)
	}
	if r := by["otieno.example"]; !r.Ready || r.Suggestion.Source != "billed" || r.Suggestion.Price != 3000 {
		t.Fatalf("billed today: %+v", r.Suggestion)
	}
	// Retail helper: 50% margin on 1000 KES/month, in the Australian client's AUD, rounded up to a friendly price.
	if r := by["bondi.example"]; !r.Ready || r.Suggestion.Source != "retail" || r.Suggestion.Currency != "AUD" || r.Suggestion.Price < 1000.0/85*2 {
		t.Fatalf("retail: %+v", r.Suggestion)
	}
	if by["nomatch.example"].Ready || !strings.Contains(by["nomatch.example"].Reason, "No client") || by["mine.example"].Ready || by["gone.example"].Ready {
		t.Fatal("not ready: no client / not a client site / cancelled")
	}
	if !strings.HasPrefix(by["wanjiku.example"].Start, "2030-02-01") {
		t.Fatalf("starts at the next month by default: %s", by["wanjiku.example"].Start)
	}
	// One click: Wanjiku's site.
	sub, err := f.s.FromSynergy(f.org, f.admin, FromSynergyInput{Domain: "wanjiku.example"})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Supplier != SupplierSynergy || sub.SiteDomain != "wanjiku.example" || sub.SupplierPlan != "Micro Env" || sub.Source != "synergy" || len(sub.Lines) != 1 ||
		sub.Lines[0].ServiceID != micro || sub.Amount != 1200 || DayStr(sub.StartDate) != "2030-02-01" || sub.CustomerID != f.customer {
		t.Fatalf("one click: %+v %+v", sub, sub.Lines)
	}
	if _, err := f.s.FromSynergy(f.org, f.admin, FromSynergyInput{Domain: "wanjiku.example"}); err == nil || !strings.Contains(err.Error(), sub.Number) {
		t.Fatalf("billed twice: %v", err)
	}
	// Choosing a service for a plan saves it for the next site on that plan.
	big := f.service("Business hosting — yearly", 36000, "KES")
	s2, err := f.s.FromSynergy(f.org, f.admin, FromSynergyInput{Domain: "otieno.example", ServiceID: big})
	if err != nil || s2.Period != PeriodYearly || s2.Amount != 36000 {
		t.Fatalf("chosen service: %v %+v", err, s2)
	}
	var pm PlanMap
	if f.db.Where("organization_id = ? AND plan_key = ?", f.org, "mega env").First(&pm).Error != nil || pm.ServiceID != big || pm.Period != PeriodYearly {
		t.Fatalf("plan map: %+v", pm)
	}
	// Bulk: a dry run lists the rest; then every matched, unbilled site.
	res, err := f.s.BulkFromSynergy(f.org, f.admin, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	okN, bad := 0, 0
	for _, r := range res {
		if r.OK {
			okN++
		} else {
			bad++
		}
	}
	if okN != 1 || bad != 1 || len(res) != 2 {
		t.Fatalf("dry run: %+v", res)
	}
	var n int64
	f.db.Model(&Subscription{}).Where("organization_id = ?", f.org).Count(&n)
	if n != 2 {
		t.Fatal("a dry run creates nothing")
	}
	res, err = f.s.BulkFromSynergy(f.org, f.admin, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var created *BulkResult
	for i := range res {
		if res[i].OK {
			created = &res[i]
		}
	}
	if created == nil || created.Domain != "bondi.example" || created.Number == "" {
		t.Fatalf("bulk: %+v", res)
	}
	g, _ := f.s.Get(f.org, created.SubscriptionID)
	if g.Currency != "AUD" || g.Supplier != SupplierSynergy {
		t.Fatalf("bulk subscription: %s %s", g.Currency, g.Supplier)
	}
	// Everything billed now: the dashboard knows.
	dash := f.s.Dashboard(f.org)
	if dash.Unbilled != 1 || dash.ReadySites != 0 || !dash.HasSynergy {
		t.Fatalf("dashboard sites: unbilled %d ready %d", dash.Unbilled, dash.ReadySites)
	}
	// Through the API.
	tok := f.token(f.org, f.admin, "admin")
	if code, out := f.api("GET", "/subscriptions/synergy-sites", tok, nil); code != 200 || len(out["data"].(map[string]any)["sites"].([]any)) != 6 {
		t.Fatalf("API sites: %d", code)
	}
	if code, out := f.api("POST", "/subscriptions/from-synergy/bulk", tok, map[string]any{}); code != 200 || out["data"].(map[string]any)["created"].(float64) != 0 {
		t.Fatalf("nothing left to bill: %d %v", code, out)
	}
	if code, out := f.api("GET", "/subscriptions/catalog?currency=KES&period=yearly", tok, nil); code != 200 {
		t.Fatalf("catalog: %d %v", code, out)
	} else {
		for _, s := range out["data"].(map[string]any)["services"].([]any) {
			m := s.(map[string]any)
			if m["id"] == micro && m["quote"].(float64) != 14400 {
				t.Fatalf("monthly service quoted yearly: %v", m)
			}
		}
	}
}

func TestAITools(t *testing.T) {
	f := setup(t, "Africa/Nairobi", "KES", "KE")
	prevE, prevS := aitools.Engine, aitools.JWTSecret
	aitools.Engine, aitools.JWTSecret = f.engine, testSecret
	t.Cleanup(func() { aitools.Engine, aitools.JWTSecret = prevE, prevS })
	due := "2030-01-19"
	sub := f.subscribe(hosting(due, 1500, "wanjiku.example", SupplierHostingCom))
	for _, off := range []int{-7, 4} {
		f.at(f.day(off, due))
		f.run()
	}
	ctx := &aitools.Ctx{Context: context.Background(), Who: aitools.Identity{UserID: f.admin, OrgID: f.org, Role: "admin", Email: f.adminEmail(1)}}
	call := func(name string, readOnly bool, args map[string]any) map[string]any {
		t.Helper()
		tool, ok := aitools.Get(name)
		if !ok || tool.ReadOnly != readOnly || tool.Module != "invoicing" {
			t.Fatalf("%s: registered invoicing tool (read-only %v)", name, readOnly)
		}
		v, err := tool.Handler(ctx, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	o := call("subscriptions_overview", true, map[string]any{})
	up, _ := o["upcoming_suspensions"].([]any)
	if o["mrr"].(float64) != 1500 || o["arr"].(float64) != 18000 || o["currency"] != "KES" || len(up) != 1 || up[0].(map[string]any)["suspend_on"] != f.day(7, due) ||
		up[0].(map[string]any)["days_to_suspend"].(float64) != 3 {
		t.Fatalf("subscriptions_overview: %v", o)
	}
	l := call("subscriptions_list", true, map[string]any{"search": "wanjiku"})
	if rows, _ := l["subscriptions"].([]any); len(rows) != 1 || rows[0].(map[string]any)["number"] != sub.Number || rows[0].(map[string]any)["status"] != StatusPastDue {
		t.Fatalf("subscriptions_list: %v", l)
	}
	// A write tool (the assistant shows it for confirmation before it runs).
	x := call("subscriptions_extend", false, map[string]any{"search": "wanjiku.example", "days": 14, "note": "Paying on the 15th"})
	if x["suspends_on"] != f.day(21, due) || !strings.Contains(x["summary"].(string), "14 more day") {
		t.Fatalf("subscriptions_extend: %v", x)
	}
	if f.periods(sub.ID)[0].ExtraDays != 14 {
		t.Fatal("extra days stored")
	}
	tool, _ := aitools.Get("subscriptions_extend")
	if _, err := tool.Handler(ctx, map[string]any{"search": "nobody"}); err == nil {
		t.Fatal("no match")
	}
}
