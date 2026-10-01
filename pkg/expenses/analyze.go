package expenses

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Analysis works on approved items only (the expenses the P&L counts), in
// the home currency, excluding tax (same as the P&L), dated by invoice date.

// Charge is one approved expense as the analysis sees it.
type Charge struct {
	ID, VendorID, Vendor, Service, Category, Currency string
	Date                                              time.Time
	Net, Total                                        float64 // home currency (ex tax, incl tax)
	OrigTotal                                         float64 // original currency, incl tax
	Paid                                              bool
	InvoiceNumber                                     string
}

// ChargeOf converts an item.
func ChargeOf(it Item) Charge {
	d := time.Time{}
	if it.InvoiceDate != nil {
		d = Day(*it.InvoiceDate)
	}
	return Charge{ID: it.ID, VendorID: it.VendorID, Vendor: it.VendorName, Service: it.Service, Category: it.Category, Currency: it.Currency,
		Date: d, Net: it.HomeNet, Total: it.HomeTotal, OrigTotal: it.Total, Paid: it.Paid, InvoiceNumber: it.InvoiceNumber}
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Subscription is a detected recurring charge.
type Subscription struct {
	Key        string   `json:"key"`
	VendorID   string   `json:"vendor_id"`
	Vendor     string   `json:"vendor"`
	Service    string   `json:"service"`
	Category   string   `json:"category"`
	Interval   string   `json:"interval"` // monthly | quarterly | half-yearly | yearly
	Months     int      `json:"months"`
	Count      int      `json:"count"`
	LastDate   string   `json:"last_date"`
	LastAmount float64  `json:"last_amount"`   // home, ex tax
	LastOrig   float64  `json:"last_original"` // original currency, incl tax
	Currency   string   `json:"currency"`      // original currency
	PrevOrig   *float64 `json:"previous_original"`
	ChangePct  *float64 `json:"change_pct"`
	NextDate   string   `json:"next_date"`
	NextAmount float64  `json:"next_amount"` // home, ex tax
	Annualised float64  `json:"annualised"`  // home, ex tax
	Status     string   `json:"status"`      // active | overdue | lapsed
	// RecurringBillID is the recurring bill (Finance → Bills → Recurring)
	// this subscription was linked to, if any.
	RecurringBillID string `json:"recurring_bill_id,omitempty"`
	next            time.Time
}

func serviceKey(c Charge) string {
	s := strings.ToLower(strings.TrimSpace(c.Service))
	if s == "" {
		s = strings.ToLower(c.Category)
	}
	return c.VendorID + "|" + c.Vendor + "|" + s
}

// DetectSubscriptions finds charges that repeat about monthly, quarterly,
// half-yearly or yearly for the same vendor and service.
func DetectSubscriptions(charges []Charge, today time.Time) []Subscription {
	groups := map[string][]Charge{}
	for _, c := range charges {
		if c.Date.IsZero() || c.Net <= 0 {
			continue
		}
		groups[serviceKey(c)] = append(groups[serviceKey(c)], c)
	}
	out := []Subscription{}
	for key, cs := range groups {
		if len(cs) < 2 {
			continue
		}
		sort.Slice(cs, func(i, j int) bool { return cs[i].Date.Before(cs[j].Date) })
		// Charges on the same day (e.g. a seat change billed twice) count as one billing.
		dates := []time.Time{cs[0].Date}
		for _, c := range cs[1:] {
			if c.Date.Sub(dates[len(dates)-1]) > 5*24*time.Hour {
				dates = append(dates, c.Date)
			}
		}
		if len(dates) < 2 {
			continue
		}
		ints := []float64{}
		for i := 1; i < len(dates); i++ {
			ints = append(ints, dates[i].Sub(dates[i-1]).Hours()/24)
		}
		med := median(ints)
		months, label := 0, ""
		switch {
		case med >= 25 && med <= 36:
			months, label = 1, "monthly"
		case med >= 80 && med <= 100:
			months, label = 3, "quarterly"
		case med >= 170 && med <= 195:
			months, label = 6, "half-yearly"
		case med >= 345 && med <= 385:
			months, label = 12, "yearly"
		default:
			continue
		}
		// Most intervals must agree.
		ok := 0
		for _, v := range ints {
			if math.Abs(v-med) <= med*0.35 {
				ok++
			}
		}
		if float64(ok) < float64(len(ints))*0.6 {
			continue
		}
		last := cs[len(cs)-1]
		// Amount of the last billing (sum of same-day charges).
		var lastNet, lastOrig, prevOrig float64
		lastDay := dates[len(dates)-1]
		prevDay := dates[len(dates)-2]
		var hasPrev bool
		samecur := true
		for _, c := range cs {
			if !c.Date.Before(lastDay) {
				lastNet += c.Net
				lastOrig += c.OrigTotal
			} else if !c.Date.Before(prevDay) && c.Date.Sub(prevDay) <= 5*24*time.Hour {
				prevOrig += c.OrigTotal
				hasPrev = true
			}
			if c.Currency != last.Currency {
				samecur = false
			}
		}
		next := lastDay.AddDate(0, months, 0)
		s := Subscription{Key: key, VendorID: last.VendorID, Vendor: last.Vendor, Service: last.Service, Category: last.Category, Interval: label, Months: months,
			Count: len(dates), LastDate: lastDay.Format(dayFmt), LastAmount: r2(lastNet), LastOrig: r2(lastOrig), Currency: last.Currency,
			NextDate: next.Format(dayFmt), NextAmount: r2(lastNet), Annualised: r2(lastNet * 12 / float64(months)), Status: "active", next: next}
		if s.Service == "" {
			s.Service = CategoryLabels[s.Category]
		}
		if hasPrev && samecur && prevOrig > 0 {
			p := r2(prevOrig)
			s.PrevOrig = &p
			ch := math.Round((lastOrig-prevOrig)/prevOrig*1000) / 10
			s.ChangePct = &ch
		}
		late := today.Sub(next).Hours() / 24
		switch {
		case late > float64(months)*30*1.5:
			s.Status = "lapsed"
		case late > 7:
			s.Status = "overdue"
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Annualised != out[j].Annualised {
			return out[i].Annualised > out[j].Annualised
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64{}, v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// Alert is something to act on.
type Alert struct {
	Key      string  `json:"key"`
	Type     string  `json:"type"`     // price_increase | new_vendor | duplicate | payment_failed | unpaid | renewal_due | renewal_expired | low_balance | subscription_overdue
	Severity string  `json:"severity"` // info | warning | danger
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	Link     string  `json:"link,omitempty"`
	Amount   float64 `json:"amount,omitempty"`
	Currency string  `json:"currency,omitempty"`
	Date     string  `json:"date,omitempty"`
}

// AlertInput is what the alert rules look at.
type AlertInput struct {
	Today         time.Time
	Home          string
	Charges       []Charge // approved
	Subs          []Subscription
	Declined      []Item // payment-declined notices
	Renewals      []Renewal
	Balance       *float64
	LowBalance    float64
	PriceIncrease float64 // fraction, default 0.05
	Fmt           func(v float64, cur string) string
}

// Alerts evaluates every rule.
func Alerts(in AlertInput) []Alert {
	if in.PriceIncrease <= 0 {
		in.PriceIncrease = 0.05
	}
	f := in.Fmt
	if f == nil {
		f = func(v float64, cur string) string { return fmt.Sprintf("%s %.2f", cur, v) }
	}
	out := []Alert{}
	today := Day(in.Today)

	// Price increases on subscriptions.
	for _, s := range in.Subs {
		if s.ChangePct != nil && *s.ChangePct > in.PriceIncrease*100 && s.PrevOrig != nil {
			out = append(out, Alert{Key: "price:" + s.Key + ":" + s.LastDate, Type: "price_increase", Severity: "warning",
				Title: fmt.Sprintf("%s went up %.1f%%", s.Vendor, *s.ChangePct),
				Body:  fmt.Sprintf("%s: %s now, was %s (%s).", nonEmpty(s.Service, s.Vendor), f(s.LastOrig, s.Currency), f(*s.PrevOrig, s.Currency), s.Interval),
				Link:  "/expenses?tab=subscriptions", Amount: s.LastOrig, Currency: s.Currency, Date: s.LastDate})
		}
		if s.Status == "overdue" {
			out = append(out, Alert{Key: "subdue:" + s.Key + ":" + s.NextDate, Type: "subscription_overdue", Severity: "info",
				Title: s.Vendor + " charge not seen yet",
				Body:  fmt.Sprintf("%s was expected on %s (%s). Sync or check the inbox.", nonEmpty(s.Service, s.Vendor), s.NextDate, s.Interval),
				Link:  "/expenses?tab=inbox", Date: s.NextDate})
		}
	}

	// New vendors (first approved charge in the last 45 days).
	first := map[string]Charge{}
	for _, c := range in.Charges {
		k := c.VendorID + "|" + c.Vendor
		if p, ok := first[k]; !ok || c.Date.Before(p.Date) {
			first[k] = c
		}
	}
	for k, c := range first {
		if !c.Date.IsZero() && today.Sub(c.Date).Hours()/24 <= 45 && !c.Date.After(today) {
			out = append(out, Alert{Key: "newvendor:" + k, Type: "new_vendor", Severity: "info", Title: "New vendor: " + c.Vendor,
				Body: fmt.Sprintf("First charge %s on %s.", f(c.Total, in.Home), c.Date.Format(dayFmt)), Link: "/expenses?vendor=" + c.VendorID, Date: c.Date.Format(dayFmt)})
		}
	}

	// Duplicate charges: same vendor, same original amount, within 3 days.
	cs := append([]Charge{}, in.Charges...)
	sort.Slice(cs, func(i, j int) bool { return cs[i].Date.Before(cs[j].Date) })
	for i := range cs {
		for j := i + 1; j < len(cs) && cs[j].Date.Sub(cs[i].Date) <= 3*24*time.Hour; j++ {
			a, b := cs[i], cs[j]
			if a.Vendor == b.Vendor && a.Currency == b.Currency && math.Abs(a.OrigTotal-b.OrigTotal) < 0.005 && a.OrigTotal > 0 &&
				(a.InvoiceNumber == "" || b.InvoiceNumber == "" || a.InvoiceNumber != b.InvoiceNumber) {
				ids := []string{a.ID, b.ID}
				sort.Strings(ids)
				out = append(out, Alert{Key: "dup:" + strings.Join(ids, ":"), Type: "duplicate", Severity: "warning",
					Title: "Possible duplicate charge: " + a.Vendor,
					Body:  fmt.Sprintf("Two charges of %s on %s and %s. Check you were not billed twice.", f(a.OrigTotal, a.Currency), a.Date.Format(dayFmt), b.Date.Format(dayFmt)),
					Link:  "/expenses?tab=history&vendor=" + a.VendorID, Amount: a.OrigTotal, Currency: a.Currency, Date: b.Date.Format(dayFmt)})
			}
		}
	}

	// Failed payments (declined notices in the last 30 days without a later charge).
	for _, d := range in.Declined {
		if d.InvoiceDate == nil {
			continue
		}
		dd := Day(*d.InvoiceDate)
		if today.Sub(dd).Hours()/24 > 30 {
			continue
		}
		resolved := false
		for _, c := range in.Charges {
			if c.Vendor == d.VendorName && !c.Date.Before(dd) && c.Paid {
				resolved = true
				break
			}
		}
		if !resolved {
			out = append(out, Alert{Key: "declined:" + d.ID, Type: "payment_failed", Severity: "danger", Title: "Payment declined: " + nonEmpty(d.VendorName, "vendor"),
				Body: fmt.Sprintf("%s on %s. Update the card or pay the balance to avoid an interruption.", nonEmpty(d.Service, d.Subject), dd.Format(dayFmt)),
				Link: "/expenses?tab=alerts", Date: dd.Format(dayFmt)})
		}
	}

	// Unpaid approved bills.
	for _, c := range in.Charges {
		if !c.Paid {
			out = append(out, Alert{Key: "unpaid:" + c.ID, Type: "unpaid", Severity: "warning", Title: "Unpaid: " + c.Vendor,
				Body: fmt.Sprintf("%s %s from %s is not marked as paid.", nonEmpty(c.InvoiceNumber, "Invoice"), f(c.Total, in.Home), c.Date.Format(dayFmt)),
				Link: "/bills", Amount: c.Total, Currency: in.Home, Date: c.Date.Format(dayFmt)})
		}
	}

	// Renewals.
	for _, r := range in.Renewals {
		if !r.Active || r.ExpiresOn == nil {
			continue
		}
		days := int(math.Round(Day(*r.ExpiresOn).Sub(today).Hours() / 24))
		cost := ""
		if r.Cost != nil {
			cost = " Renewal about " + f(*r.Cost, r.Currency) + "."
		}
		switch {
		case days < 0 && days >= -45 && !strings.Contains(strings.ToLower(r.Status), "renew"):
			out = append(out, Alert{Key: "expired:" + r.ID + ":" + r.ExpiresOn.Format(dayFmt), Type: "renewal_expired", Severity: "danger",
				Title: r.Name + " has expired", Body: fmt.Sprintf("%s expired on %s.%s", kindLabel(r.Kind), r.ExpiresOn.Format(dayFmt), cost),
				Link: "/expenses?tab=renewals", Date: r.ExpiresOn.Format(dayFmt)})
		case days >= 0 && days <= 30:
			sev := "warning"
			note := " Auto-renew is off."
			if r.AutoRenew {
				sev, note = "info", " Auto-renews — keep enough balance."
			}
			out = append(out, Alert{Key: "renew:" + r.ID + ":" + r.ExpiresOn.Format(dayFmt), Type: "renewal_due", Severity: sev,
				Title: fmt.Sprintf("%s renews in %d day%s", r.Name, days, plural(days)),
				Body:  fmt.Sprintf("%s due %s.%s%s", kindLabel(r.Kind), r.ExpiresOn.Format(dayFmt), cost, note),
				Link:  "/expenses?tab=renewals", Date: r.ExpiresOn.Format(dayFmt)})
		}
	}

	// Low Synergy balance.
	if in.Balance != nil {
		lim := in.LowBalance
		if lim <= 0 {
			lim = 20
		}
		if *in.Balance < lim {
			out = append(out, Alert{Key: fmt.Sprintf("lowbal:%s", today.Format("2006-01-02")), Type: "low_balance", Severity: map[bool]string{true: "danger", false: "warning"}[*in.Balance < 0],
				Title: "Synergy Wholesale balance is low",
				Body:  fmt.Sprintf("Balance %s (alert below %s). Renewals and registrations fail when the balance runs out.", f(*in.Balance, "AUD"), f(lim, "AUD")),
				Link:  "/expenses?tab=sources", Amount: *in.Balance, Currency: "AUD"})
		}
	}

	sev := map[string]int{"danger": 0, "warning": 1, "info": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if sev[out[i].Severity] != sev[out[j].Severity] {
			return sev[out[i].Severity] < sev[out[j].Severity]
		}
		return out[i].Date > out[j].Date
	})
	return out
}

func kindLabel(k string) string {
	switch k {
	case "domain":
		return "Domain"
	case "hosting":
		return "Hosting"
	case "ssl":
		return "SSL certificate"
	}
	return "Service"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// ForecastMonth is the expected spend in one future month (home, ex tax).
type ForecastMonth struct {
	Month         string  `json:"month"`
	Subscriptions float64 `json:"subscriptions"`
	Renewals      float64 `json:"renewals"`
	Other         float64 `json:"other"`
	Total         float64 `json:"total"`
	Items         []struct {
		Label  string  `json:"label"`
		Date   string  `json:"date"`
		Amount float64 `json:"amount"`
		Kind   string  `json:"kind"`
	} `json:"items"`
}

// Forecast projects the next n months: detected subscriptions on their
// expected dates, renewals with a known cost (converted with toHome), and
// the average of other (non-recurring) spend over the last 3 full months.
func Forecast(charges []Charge, subs []Subscription, renewals []Renewal, today time.Time, n int, toHome func(v float64, cur string) float64) []ForecastMonth {
	out := make([]ForecastMonth, n)
	start := monthStart(today).AddDate(0, 1, 0)
	if today.Day() < 15 {
		start = monthStart(today) // still most of this month ahead
	}
	for i := range out {
		out[i].Month = start.AddDate(0, i, 0).Format("2006-01")
	}
	idx := func(t time.Time) int {
		for i := range out {
			m := start.AddDate(0, i, 0)
			if !t.Before(m) && t.Before(m.AddDate(0, 1, 0)) {
				return i
			}
		}
		return -1
	}
	add := func(i int, kind, label string, d time.Time, v float64) {
		if i < 0 {
			return
		}
		switch kind {
		case "subscription":
			out[i].Subscriptions += v
		case "renewal":
			out[i].Renewals += v
		}
		out[i].Items = append(out[i].Items, struct {
			Label  string  `json:"label"`
			Date   string  `json:"date"`
			Amount float64 `json:"amount"`
			Kind   string  `json:"kind"`
		}{label, d.Format(dayFmt), r2(v), kind})
	}
	end := start.AddDate(0, n, 0)
	for _, s := range subs {
		if s.Status == "lapsed" {
			continue
		}
		d := s.next
		for d.Before(today) {
			d = d.AddDate(0, s.Months, 0)
		}
		for ; d.Before(end); d = d.AddDate(0, s.Months, 0) {
			add(idx(d), "subscription", nonEmpty(s.Service, s.Vendor), d, s.NextAmount)
		}
	}
	for _, r := range renewals {
		if !r.Active || r.ExpiresOn == nil || r.Cost == nil || *r.Cost <= 0 {
			continue
		}
		d := Day(*r.ExpiresOn)
		if d.Before(today) || !d.Before(end) {
			continue
		}
		add(idx(d), "renewal", r.Name+" ("+kindLabel(r.Kind)+")", d, toHome(*r.Cost, r.Currency))
	}
	// Other spend: charges not part of a detected subscription in the last 3 full months.
	subKeys := map[string]bool{}
	for _, s := range subs {
		subKeys[s.Key] = true
	}
	m0 := monthStart(today).AddDate(0, -3, 0)
	m1 := monthStart(today)
	var other float64
	for _, c := range charges {
		if !c.Date.Before(m0) && c.Date.Before(m1) && !subKeys[serviceKey(c)] {
			other += c.Net
		}
	}
	avg := r2(other / 3)
	for i := range out {
		out[i].Other = avg
		out[i].Subscriptions = r2(out[i].Subscriptions)
		out[i].Renewals = r2(out[i].Renewals)
		out[i].Total = r2(out[i].Subscriptions + out[i].Renewals + out[i].Other)
		sort.Slice(out[i].Items, func(a, b int) bool { return out[i].Items[a].Date < out[i].Items[b].Date })
	}
	return out
}

const dayFmt = "2006-01-02"
