package expenses

import (
	"strings"
	"testing"
	"time"
)

func d(s string) time.Time { t, _ := time.Parse(dayFmt, s); return t }

func monthly(vendor, service, cur string, start string, n int, amt func(i int) float64) []Charge {
	out := []Charge{}
	for i := 0; i < n; i++ {
		v := amt(i)
		out = append(out, Charge{ID: vendor + string(rune('a'+i)), VendorID: "v-" + vendor, Vendor: vendor, Service: service, Category: CatEmail, Currency: cur,
			Date: d(start).AddDate(0, i, 0), Net: v, Total: v, OrigTotal: v, Paid: true})
	}
	return out
}

func TestDetectSubscriptionsAndPriceIncrease(t *testing.T) {
	today := d("2026-10-01")
	cs := monthly("Google Workspace", "Google Workspace (example.com.au)", "EUR", "2026-04-01", 6, func(i int) float64 {
		if i == 5 {
			return 43.2 // 36 → 43.20 (+20%)
		}
		return 36
	})
	// Yearly domain renewal.
	cs = append(cs, Charge{ID: "y1", VendorID: "v-syn", Vendor: "Synergy Wholesale", Service: "example.com renewal", Currency: "AUD", Date: d("2024-11-03"), Net: 15, OrigTotal: 16.5, Paid: true},
		Charge{ID: "y2", VendorID: "v-syn", Vendor: "Synergy Wholesale", Service: "example.com renewal", Currency: "AUD", Date: d("2025-11-01"), Net: 15, OrigTotal: 16.5, Paid: true})
	// Irregular one-offs are not subscriptions.
	cs = append(cs, Charge{ID: "o1", VendorID: "v-x", Vendor: "Fiverr", Service: "Logo", Currency: "USD", Date: d("2026-02-10"), Net: 80, OrigTotal: 80, Paid: true},
		Charge{ID: "o2", VendorID: "v-x", Vendor: "Fiverr", Service: "Logo", Currency: "USD", Date: d("2026-07-29"), Net: 50, OrigTotal: 50, Paid: true})
	subs := DetectSubscriptions(cs, today)
	if len(subs) != 2 {
		t.Fatalf("subs %+v", subs)
	}
	var g, y Subscription
	for _, s := range subs {
		if s.Interval == "monthly" {
			g = s
		} else {
			y = s
		}
	}
	if g.NextDate != "2026-10-01" || !near(g.LastAmount, 43.2) || !near(g.Annualised, 518.4) || g.ChangePct == nil || !near(*g.ChangePct, 20) || g.Status != "active" {
		t.Fatalf("google %+v", g)
	}
	if y.Interval != "yearly" || y.NextDate != "2026-11-01" || !near(y.Annualised, 15) {
		t.Fatalf("yearly %+v", y)
	}
	alerts := Alerts(AlertInput{Today: today, Home: "AUD", Charges: cs, Subs: subs})
	var price *Alert
	for i := range alerts {
		if alerts[i].Type == "price_increase" {
			price = &alerts[i]
		}
	}
	if price == nil || !strings.Contains(price.Title, "20.0%") {
		t.Fatalf("price alert %+v", alerts)
	}
	// A 4% rise is under the 5% threshold.
	cs2 := monthly("Slack", "Slack Pro", "USD", "2026-06-05", 4, func(i int) float64 {
		if i == 3 {
			return 10.4
		}
		return 10
	})
	for _, a := range Alerts(AlertInput{Today: today, Charges: cs2, Subs: DetectSubscriptions(cs2, today)}) {
		if a.Type == "price_increase" {
			t.Fatalf("4%% should not alert: %+v", a)
		}
	}
}

func TestAlertsDuplicatesNewVendorRenewalsBalance(t *testing.T) {
	today := d("2026-10-01")
	cs := []Charge{
		{ID: "a", VendorID: "v1", Vendor: "Hostinger", Currency: "USD", Date: d("2026-09-20"), Net: 10, Total: 15, OrigTotal: 9.99, Paid: true, InvoiceNumber: "H-1"},
		{ID: "b", VendorID: "v1", Vendor: "Hostinger", Currency: "USD", Date: d("2026-09-21"), Net: 10, Total: 15, OrigTotal: 9.99, Paid: true, InvoiceNumber: "H-2"},
		{ID: "c", VendorID: "v2", Vendor: "Old Co", Currency: "AUD", Date: d("2025-01-01"), Net: 5, Total: 5, OrigTotal: 5, Paid: false},
	}
	exp := d("2026-10-11")
	past := d("2026-09-20")
	cost := 11.95
	rens := []Renewal{
		{ID: "r1", Name: "example.com.au", Kind: "domain", ExpiresOn: &exp, Active: true, Cost: &cost, Currency: "AUD"},
		{ID: "r2", Name: "gone.com", Kind: "domain", ExpiresOn: &past, Active: true, Status: "expired"},
	}
	bal := -58.83
	dec := Item{ID: "x", VendorName: "Google Workspace", InvoiceDate: &past, Kind: KindDeclined}
	al := Alerts(AlertInput{Today: today, Home: "AUD", Charges: cs, Renewals: rens, Balance: &bal, Declined: []Item{dec}})
	types := map[string]int{}
	for _, a := range al {
		types[a.Type]++
	}
	want := map[string]int{"duplicate": 1, "new_vendor": 1, "unpaid": 1, "renewal_due": 1, "renewal_expired": 1, "low_balance": 1, "payment_failed": 1}
	for k, v := range want {
		if types[k] != v {
			t.Fatalf("%s: got %d (all %v)", k, types[k], types)
		}
	}
	if al[0].Severity != "danger" {
		t.Fatalf("danger first: %+v", al[0])
	}
}

func TestForecast(t *testing.T) {
	today := d("2026-10-20")
	cs := monthly("Google Workspace", "GW", "AUD", "2026-05-05", 6, func(int) float64 { return 30 })
	cs = append(cs, Charge{ID: "one", VendorID: "z", Vendor: "Canva", Service: "Canva", Date: d("2026-08-10"), Net: 90, OrigTotal: 90, Paid: true})
	subs := DetectSubscriptions(cs, today)
	exp := d("2026-12-01")
	cost := 20.0
	rens := []Renewal{{ID: "r", Name: "x.com", Kind: "domain", Active: true, ExpiresOn: &exp, Cost: &cost, Currency: "USD"}}
	fc := Forecast(cs, subs, rens, today, 3, func(v float64, cur string) float64 {
		if cur == "USD" {
			return v * 1.5
		}
		return v
	})
	if len(fc) != 3 || fc[0].Month != "2026-11" {
		t.Fatalf("%+v", fc)
	}
	// Nov: GW 30 + other (90/3 = 30 over Jul–Sep) ; Dec: GW 30 + renewal 30 + 30.
	if !near(fc[0].Subscriptions, 30) || !near(fc[0].Other, 30) || !near(fc[0].Total, 60) || !near(fc[1].Renewals, 30) || !near(fc[1].Total, 90) {
		t.Fatalf("%+v", fc)
	}
}
