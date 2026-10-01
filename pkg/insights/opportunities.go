package insights

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Internal business opportunities: rules over invoices, quotes, payments,
// Synergy hosting and renewals. Each opportunity has an estimated annual value
// in the home currency (with the basis spelled out) and a one-click action.

// QuoteDraft pre-fills a draft quote (created by the web app through the
// invoicing API).
type QuoteDraft struct {
	CustomerID  string  `json:"customer_id"`
	ClientName  string  `json:"client_name"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	Currency    string  `json:"currency"`
}

// OppItem is one client / site / document behind an opportunity.
type OppItem struct {
	Name       string      `json:"name"`
	CustomerID string      `json:"customer_id,omitempty"`
	Domain     string      `json:"domain,omitempty"`
	Detail     string      `json:"detail"`
	Value      float64     `json:"value"`
	Link       string      `json:"link,omitempty"`
	Quote      *QuoteDraft `json:"quote,omitempty"`
	LinkSite   bool        `json:"link_site,omitempty"` // site without a client: offer "link to client"
}

// Opportunity is one actionable signal.
type Opportunity struct {
	Key        string    `json:"key"`
	Type       string    `json:"type"` // upsell | winback | pricing | unbilled | collection | pipeline | renewal | risk
	Title      string    `json:"title"`
	Summary    string    `json:"summary"`
	Severity   string    `json:"severity"` // high | medium | low
	Value      float64   `json:"value"`    // estimated, per year unless ValueKind says otherwise
	ValueKind  string    `json:"value_kind"`
	ValueBasis string    `json:"value_basis"`
	Count      int       `json:"count"`
	Items      []OppItem `json:"items"`
	ActionText string    `json:"action_text"`
	Link       string    `json:"link"`
}

// Opportunities is GET /insights/opportunities.
type Opportunities struct {
	Currency string         `json:"currency"`
	Today    string         `json:"today"`
	Total    float64        `json:"total_value"`
	ByType   map[string]int `json:"by_type"`
	Items    []Opportunity  `json:"opportunities"`
	Pipeline *Pipeline      `json:"pipeline"`
	Calendar []RenewalRow   `json:"renewal_calendar"`
	Notes    []string       `json:"notes"`
}

// Pipeline summarises quotes.
type Pipeline struct {
	OpenCount      int      `json:"open_count"`
	OpenValue      float64  `json:"open_value"`
	ExpiredCount   int      `json:"expired_count"` // sent, past validity, within 90 days
	ExpiredValue   float64  `json:"expired_value"`
	Won12m         int      `json:"won_12m"`
	Lost12m        int      `json:"lost_12m"`
	ConversionPct  *float64 `json:"conversion_pct"`
	ExpectedValue  float64  `json:"expected_value"`
	AvgDaysToClose *float64 `json:"avg_days_to_close"`
}

// RenewalRow is one upcoming renewal (domains, hosting, SSL, recurring invoices).
type RenewalRow struct {
	Date       string  `json:"date"`
	Kind       string  `json:"kind"` // domain | hosting | ssl | other | recurring_invoice
	Name       string  `json:"name"`
	Client     string  `json:"client"`
	CustomerID string  `json:"customer_id,omitempty"`
	Cost       float64 `json:"cost"`  // home currency, what it costs you
	Price      float64 `json:"price"` // home currency, what the client pays (0 = not set)
	DaysLeft   int     `json:"days_left"`
	Link       string  `json:"link"`
}

func sev(v, hi, med float64) string {
	switch {
	case v >= hi:
		return "high"
	case v >= med:
		return "medium"
	}
	return "low"
}

// OpportunitiesFor runs every rule.
func (s *Service) OpportunitiesFor(org string, today time.Time) *Opportunities {
	today = day(today)
	c := s.conv(org, "")
	home := c.Home
	out := &Opportunities{Currency: home, Today: today.Format(dayFmt), ByType: map[string]int{}, Items: []Opportunity{}, Calendar: []RenewalRow{}, Notes: []string{}}
	names := s.clientNames(org)
	// Thresholds scale with the business: 15% / 5% of trailing-12-month revenue.
	var rev12 float64
	s.DB.Raw("WITH "+c.docsCTE()+` SELECT COALESCE(SUM(sgn * f * (total - tax_total)), 0) FROM docs WHERE f IS NOT NULL AND d > (@today)::date - interval '12 months' AND d <= (@today)::date`,
		map[string]interface{}{"org": org, "today": today.Format(dayFmt)}).Row().Scan(&rev12)
	hi, med := math.Max(rev12*0.15, 1), math.Max(rev12*0.05, 0.5)

	audHome := 0.0
	if q := s.FX.Quote(org, "AUD", home); q.OK() {
		audHome = q.Rate
	}
	usdHome := 0.0
	if q := s.FX.Quote(org, "USD", home); q.OK() {
		usdHome = q.Rate
	}
	rep := s.synergy(org)

	// Service lines bought per client (12 / 24 / 36 months).
	type mix struct{ r12, r24, r36 map[string]float64 }
	mixes := map[string]*mix{}
	rules := s.Rules(org)
	var lr []struct {
		CK, Line      string
		R12, R24, R36 float64
	}
	s.scan(&lr, "WITH "+linesCTE(c, rules, Opts{})+`
SELECT ck, line,
	COALESCE(SUM(amt) FILTER (WHERE d > (@today)::date - interval '12 months'), 0) AS r12,
	COALESCE(SUM(amt) FILTER (WHERE d > (@today)::date - interval '24 months'), 0) AS r24,
	COALESCE(SUM(amt) FILTER (WHERE d > (@today)::date - interval '36 months'), 0) AS r36
FROM fl WHERE d <= (@today)::date GROUP BY ck, line`, map[string]interface{}{"org": org, "today": today.Format(dayFmt)})
	for _, r := range lr {
		m := mixes[r.CK]
		if m == nil {
			m = &mix{map[string]float64{}, map[string]float64{}, map[string]float64{}}
			mixes[r.CK] = m
		}
		m.r12[r.Line] += r.R12
		m.r24[r.Line] += r.R24
		m.r36[r.Line] += r.R36
	}
	medianOf := func(line string) float64 {
		v := []float64{}
		for _, m := range mixes {
			if m.r12[line] > 0 {
				v = append(v, m.r12[line])
			}
		}
		return median(v)
	}

	add := func(o Opportunity) {
		if o.Count == 0 {
			return
		}
		o.Value = r2(o.Value)
		if o.Severity == "" {
			o.Severity = sev(o.Value, hi, med)
		}
		sort.SliceStable(o.Items, func(i, j int) bool { return o.Items[i].Value > o.Items[j].Value })
		if len(o.Items) > 25 {
			o.Items = o.Items[:25]
		}
		for i := range o.Items {
			o.Items[i].Value = r2(o.Items[i].Value)
		}
		out.Items = append(out.Items, o)
		out.ByType[o.Type]++
		if o.ValueKind != "at_risk" {
			out.Total += o.Value
		}
	}
	draft := func(cid, name, title, desc string, annual float64) *QuoteDraft {
		if cid == "" {
			return nil
		}
		return &QuoteDraft{CustomerID: cid, ClientName: name, Title: title, Description: desc, Quantity: 1, UnitPrice: r2(annual), Currency: home}
	}

	// Hosting markup actually achieved (billed ÷ cost on billed sites).
	markup := 2.0
	if rep != nil {
		var b, cst float64
		for _, st := range rep.Sites {
			if st.Status != "cancelled" && st.Billed != nil && *st.Billed > 0 && st.CostHome > 0 {
				b += *st.Billed
				cst += st.CostHome
			}
		}
		if cst > 0 && b > 0 {
			markup = math.Max(1, b/cst)
		}
	}
	siteClients := map[string][]string{}
	if rep != nil {
		up := Opportunity{Key: "upsell_disk", Type: "upsell", Title: "Hosting plan upgrades", ValueKind: "per_year",
			ValueBasis: fmt.Sprintf("Price difference to the cheapest plan that fits (+25%% headroom), at your current hosting markup (×%s), for a year.", trimFloat(r2(markup))),
			ActionText: "Create upgrade quote", Link: "/expenses?tab=synergy"}
		unb := Opportunity{Key: "unbilled_sites", Type: "unbilled", Title: "Hosting you pay for but don't bill", ValueKind: "per_year",
			ValueBasis: fmt.Sprintf("Synergy cost per month × your hosting markup (×%s) × 12.", trimFloat(r2(markup))), ActionText: "Bill the client / link the site", Link: "/expenses?tab=synergy"}
		below := Opportunity{Key: "below_cost", Type: "pricing", Title: "Sites billed below cost", ValueKind: "per_year",
			ValueBasis: "Raising the price to cost + 50% (a 33% margin), per year.", ActionText: "Create price-review quote", Link: "/expenses?tab=synergy"}
		lost := Opportunity{Key: "cancelled_sites", Type: "winback", Title: "Cancelled hosting to win back", ValueKind: "per_year",
			ValueBasis: "What the client was billed per month (else cost × markup) × 12.", ActionText: "Open client", Link: "/expenses?tab=synergy"}
		// Plans priced below your typical rate (median billed per plan, ≥3 billed sites).
		perPlan := map[string][]float64{}
		for _, st := range rep.Sites {
			if st.Status != "cancelled" && st.Billed != nil && *st.Billed > 0 {
				perPlan[st.Plan] = append(perPlan[st.Plan], *st.Billed)
			}
		}
		cheap := Opportunity{Key: "plan_price", Type: "pricing", Title: "Sites priced below your usual rate for the plan", ValueKind: "per_year",
			ValueBasis: "Difference to the median monthly price you charge on the same plan (plans with 3+ billed sites), × 12.", ActionText: "Create price-review quote", Link: "/analytics?view=hosting"}
		for _, st := range rep.Sites {
			name := st.CustomerName
			if st.CustomerID != "" {
				if n := names[st.CustomerID]; n != "" {
					name = n
				}
				siteClients[st.CustomerID] = append(siteClients[st.CustomerID], st.Domain)
			}
			siteLink := "/expenses?tab=synergy"
			if st.Status == "cancelled" {
				if st.CustomerID == "" {
					continue
				}
				v := st.CostHome * markup * 12
				if st.Billed != nil && *st.Billed > 0 {
					v = *st.Billed * 12
				}
				if v <= 0 && st.TotalMonthly > 0 {
					v = st.TotalMonthly * audHome * markup * 12
				}
				lost.Count++
				lost.Value += v
				lost.Items = append(lost.Items, OppItem{Name: name, CustomerID: st.CustomerID, Domain: st.Domain, Detail: "Cancelled " + st.StatusDate + " · " + st.Plan, Value: v,
					Link: "/customers?q=" + urlq(name)})
				continue
			}
			if hasFlag(st.Flags, "near_full") || hasFlag(st.Flags, "over_quota") {
				deltaHome := 0.0
				if st.SuggestedDelta != nil && *st.SuggestedDelta > 0 {
					rate := audHome
					if st.TotalMonthly > 0 && st.CostHome > 0 {
						rate = st.CostHome / st.TotalMonthly
					}
					deltaHome = *st.SuggestedDelta * rate
				}
				m := markup
				if st.Billed != nil && *st.Billed > 0 && st.CostHome > 0 {
					m = math.Max(1, *st.Billed/st.CostHome)
				}
				v := deltaHome * m * 12
				pct := ""
				if st.PctUsed != nil {
					pct = fmt.Sprintf("%s%% of disk used", trimFloat(r1(*st.PctUsed)))
				}
				det := strings.TrimSpace(fmt.Sprintf("%s on %s · suggest %s", pct, st.Plan, nonEmpty(st.SuggestedPlan, "a bigger plan")))
				up.Count++
				up.Value += v
				up.Items = append(up.Items, OppItem{Name: name, CustomerID: st.CustomerID, Domain: st.Domain, Detail: det, Value: v, Link: siteLink, LinkSite: st.CustomerID == "",
					Quote: draft(st.CustomerID, name, "Hosting plan upgrade — "+st.Domain,
						fmt.Sprintf("Hosting plan upgrade for %s: %s → %s (12 months)", st.Domain, st.Plan, nonEmpty(st.SuggestedPlan, "larger plan")), v)})
			}
			if hasFlag(st.Flags, "unbilled") {
				v := st.CostHome * markup * 12
				unb.Count++
				unb.Value += v
				det := "No invoice names this domain in the last 13 months"
				if st.CustomerID == "" {
					det = "Not linked to a client"
				}
				unb.Items = append(unb.Items, OppItem{Name: nonEmpty(name, st.Domain), CustomerID: st.CustomerID, Domain: st.Domain, Detail: det + " · costs " + home + " " + amt(st.CostHome) + "/mo",
					Value: v, Link: siteLink, LinkSite: st.CustomerID == "",
					Quote: draft(st.CustomerID, name, "Web hosting — "+st.Domain, fmt.Sprintf("Web hosting for %s (%s), 12 months", st.Domain, st.Plan), v)})
			}
			if hasFlag(st.Flags, "below_cost") && st.Billed != nil {
				v := (st.CostHome*1.5 - *st.Billed) * 12
				below.Count++
				below.Value += v
				below.Items = append(below.Items, OppItem{Name: name, CustomerID: st.CustomerID, Domain: st.Domain,
					Detail: fmt.Sprintf("Billed %s %s/mo, costs %s %s/mo", home, amt(*st.Billed), home, amt(st.CostHome)), Value: v, Link: siteLink,
					Quote: draft(st.CustomerID, name, "Hosting price review — "+st.Domain, fmt.Sprintf("Web hosting for %s (%s), 12 months — revised price", st.Domain, st.Plan), st.CostHome*1.5*12)})
			}
			if p := perPlan[st.Plan]; len(p) >= 3 && st.Billed != nil && *st.Billed > 0 && !hasFlag(st.Flags, "below_cost") {
				md := median(p)
				if *st.Billed < md*0.7 {
					v := (md - *st.Billed) * 12
					cheap.Count++
					cheap.Value += v
					cheap.Items = append(cheap.Items, OppItem{Name: name, CustomerID: st.CustomerID, Domain: st.Domain,
						Detail: fmt.Sprintf("%s %s/mo vs your usual %s %s/mo on %s", home, amt(*st.Billed), home, amt(md), st.Plan), Value: v, Link: siteLink,
						Quote: draft(st.CustomerID, name, "Hosting price review — "+st.Domain, fmt.Sprintf("Web hosting for %s (%s), 12 months", st.Domain, st.Plan), md*12)})
				}
			}
		}
		add(up)
		add(unb)
		add(below)
		add(cheap)
		add(lost)
	}

	// Cross-sell: hosting clients without Workspace / email.
	wsAnnual := medianOf(LineWorkspace)
	wsBasis := "Median yearly Workspace / email spend of your clients who buy it."
	if wsAnnual <= 0 && usdHome > 0 {
		wsAnnual = 3 * 7 * 12 * usdHome
		wsBasis = "No Workspace sales yet: 3 users × USD 7 per user per month (Google Workspace Business Starter list price) for a year."
	}
	ws := Opportunity{Key: "hosting_no_workspace", Type: "upsell", Title: "Hosting clients without Google Workspace", ValueKind: "per_year", ValueBasis: wsBasis, ActionText: "Create Workspace quote", Link: "/analytics?view=services"}
	cids := make([]string, 0, len(siteClients))
	for cid := range siteClients {
		cids = append(cids, cid)
	}
	sort.Strings(cids)
	for _, cid := range cids {
		if m := mixes[cid]; m != nil && m.r24[LineWorkspace] > 0 {
			continue
		}
		name := nameOf(names, cid)
		ws.Count++
		ws.Value += wsAnnual
		ws.Items = append(ws.Items, OppItem{Name: name, CustomerID: cid, Domain: strings.Join(siteClients[cid], ", "), Detail: "Hosts " + strings.Join(siteClients[cid], ", ") + " with you; no Workspace / email billed in 24 months",
			Value: wsAnnual, Link: "/customers?q=" + urlq(name), Quote: draft(cid, name, "Google Workspace", "Google Workspace Business Starter — 3 users, 12 months (incl. setup and migration)", wsAnnual)})
	}
	add(ws)

	// Cross-sell: website clients without a maintenance plan.
	mAnnual := medianOf(LineMaintenance)
	mt := Opportunity{Key: "web_no_maintenance", Type: "upsell", Title: "Websites without a maintenance plan", ValueKind: "per_year", ActionText: "Create maintenance quote", Link: "/analytics?view=services"}
	if mAnnual > 0 {
		mt.ValueBasis = "Median yearly maintenance spend of your clients who have a plan."
	} else {
		mt.ValueBasis = "No maintenance sales yet: 15% of the client's web-development spend (3 years) per year — a common care-plan rule of thumb."
	}
	keys := make([]string, 0, len(mixes))
	for k := range mixes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, ck := range keys {
		m := mixes[ck]
		if m.r36[LineWebDev] <= 0 || m.r12[LineMaintenance] > 0 {
			continue
		}
		v := mAnnual
		if v <= 0 {
			v = m.r36[LineWebDev] * 0.15
		}
		name := nameOf(names, ck)
		mt.Count++
		mt.Value += v
		mt.Items = append(mt.Items, OppItem{Name: name, CustomerID: customerID(ck), Detail: fmt.Sprintf("Web development %s %s in 3 years; no maintenance billed in 12 months", home, amt(m.r36[LineWebDev])),
			Value: v, Link: "/customers?q=" + urlq(name), Quote: draft(customerID(ck), name, "Website care plan", "Website maintenance & support plan — updates, backups, security monitoring (12 months)", v)})
	}
	add(mt)

	// Win-back and retention from the churn analysis.
	ch := s.churn(org, c, Opts{Range: Range{From: today.AddDate(-3, 0, 0), To: today}, Today: today})
	wb := Opportunity{Key: "winback", Type: "winback", Title: "Lapsed clients to win back", ValueKind: "per_year", ValueBasis: "The client's average yearly revenue while they were buying.", ActionText: "Open client", Link: "/analytics?view=clients"}
	ar := Opportunity{Key: "at_risk", Type: "winback", Title: "Clients going quiet (6–12 months)", ValueKind: "per_year", ValueBasis: "The client's average yearly revenue.", ActionText: "Open client", Link: "/analytics?view=clients"}
	for _, cl := range ch.Clients {
		if cl.Invoices < 2 && cl.Status == "lapsed" {
			continue // one-off jobs are not churn
		}
		it := OppItem{Name: cl.Name, CustomerID: cl.CustomerID, Detail: fmt.Sprintf("Last invoice %s (%d months ago) · %d invoices · bought %s", cl.Last, cl.MonthsSince, cl.Invoices, labelList(cl.Lines)),
			Value: cl.AnnualValue, Link: "/customers?q=" + urlq(cl.Name)}
		if cl.Status == "lapsed" {
			wb.Count++
			wb.Value += cl.AnnualValue
			wb.Items = append(wb.Items, it)
		} else {
			ar.Count++
			ar.Value += cl.AnnualValue
			ar.Items = append(ar.Items, it)
		}
	}
	add(ar)
	add(wb)

	// Collection risk: overdue by client.
	pa := s.PaymentAnalytics(org, Opts{Range: Range{From: today.AddDate(-2, 0, 0), To: today}, Today: today})
	col := Opportunity{Key: "collection", Type: "collection", Title: "Overdue invoices to collect", ValueKind: "one_off", ValueBasis: "Overdue balances today.", ActionText: "Open overdue invoices", Link: "/invoices?status=overdue"}
	for _, pc := range pa.Clients {
		if pc.Overdue <= 0 {
			continue
		}
		det := fmt.Sprintf("Oldest %d days overdue", pc.OverdueDays)
		if pc.Paid > 0 {
			det += fmt.Sprintf(" · usually pays in %s days (%s%% on time)", trimFloat(pc.AvgDays), trimFloat(pc.OnTimePct))
		}
		col.Count++
		col.Value += pc.Overdue
		col.Items = append(col.Items, OppItem{Name: pc.Name, CustomerID: pc.CustomerID, Detail: det, Value: pc.Overdue, Link: "/invoices?status=overdue&q=" + urlq(pc.Name)})
	}
	if col.Count > 0 {
		col.Severity = sev(col.Value, hi, med)
	}
	add(col)

	// Pipeline: open quotes and conversion.
	out.Pipeline = s.pipeline(org, c, today)
	if p := out.Pipeline; p.OpenCount+p.ExpiredCount > 0 {
		pl := Opportunity{Key: "pipeline", Type: "pipeline", Title: "Open quotes to follow up", ValueKind: "one_off", Count: p.OpenCount + p.ExpiredCount, Link: "/quotes",
			ActionText: "Open quotes", Value: p.ExpectedValue}
		if p.ConversionPct != nil {
			pl.ValueBasis = fmt.Sprintf("Open quotes %s %s (+ half of recently expired ones) × your 12-month win rate %s%%.", home, amt(p.OpenValue), trimFloat(*p.ConversionPct))
		} else {
			pl.ValueBasis = "Open quote value (no decided quotes in 12 months to estimate a win rate)."
		}
		var qs []struct {
			ID, Number, CK, Due string
			Amt                 float64
			Expired             bool
		}
		s.scan(&qs, `SELECT i.id, i.number, COALESCE(NULLIF(i.customer_id,''), 'n:' || LOWER(TRIM(i.client_name))) AS ck, to_char(i.due_date, 'YYYY-MM-DD') AS due,
			(i.total - i.tax_total) * `+c.Factor("i")+` AS amt, i.due_date::date < (@today)::date AS expired
		FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type = 'quote' AND i.status = 'sent'
			AND i.due_date::date >= (@today)::date - 90 AND `+c.Factor("i")+` IS NOT NULL ORDER BY amt DESC LIMIT 25`, map[string]interface{}{"org": org, "today": today.Format(dayFmt)})
		for _, q := range qs {
			det := q.Number + " · valid until " + q.Due
			if q.Expired {
				det = q.Number + " · expired " + q.Due + " — follow up or re-issue"
			}
			pl.Items = append(pl.Items, OppItem{Name: nameOf(names, q.CK), CustomerID: customerID(q.CK), Detail: det, Value: q.Amt, Link: "/quotes/" + q.ID})
		}
		pl.Severity = sev(pl.Value, hi, med)
		add(pl)
	}

	// Renewal calendar: domains / hosting / SSL (Expenses) and recurring invoices.
	out.Calendar = s.renewalCalendar(org, c, today, 90)
	rn := Opportunity{Key: "renewals", Type: "renewal", Title: "Renewals to bill in the next 60 days", ValueKind: "one_off",
		ValueBasis: "Your price for the renewal where set, else its cost × 1.3.", ActionText: "Create renewal quote", Link: "/expenses?tab=renewals"}
	for _, r := range out.Calendar {
		if r.Kind == "recurring_invoice" || r.DaysLeft > 60 || r.CustomerID == "" {
			continue
		}
		v := r.Price
		if v <= 0 {
			v = r.Cost * 1.3
		}
		if v <= 0 {
			continue
		}
		rn.Count++
		rn.Value += v
		rn.Items = append(rn.Items, OppItem{Name: r.Client, CustomerID: r.CustomerID, Domain: r.Name, Detail: fmt.Sprintf("%s renews %s (in %d days)", strings.Title(r.Kind), r.Date, r.DaysLeft), Value: v, Link: r.Link,
			Quote: draft(r.CustomerID, r.Client, strings.Title(r.Kind)+" renewal — "+r.Name, fmt.Sprintf("%s renewal: %s (12 months)", strings.Title(r.Kind), r.Name), v)})
	}
	add(rn)

	// Concentration risk.
	con := s.concentration(org, c, Opts{Range: Range{From: today.AddDate(-1, 0, 1), To: today}, Today: today})
	if con.Clients >= 2 && (con.Top1Pct >= 25 || con.HHI >= 1500) && len(con.Top) > 0 {
		t := con.Top[0]
		r := Opportunity{Key: "concentration", Type: "risk", Title: "Revenue depends on few clients", ValueKind: "at_risk", Count: 1, Value: t.Revenue, Link: "/analytics?view=clients", ActionText: "See concentration",
			ValueBasis: "Revenue from your largest client over the last 12 months (at risk if they leave).",
			Summary:    fmt.Sprintf("Top client %s%% of 12-month revenue, top 5 %s%%, HHI %s (%s). Diversify: target new clients in the same sector, add recurring services to the long tail.", trimFloat(con.Top1Pct), trimFloat(con.Top5Pct), trimFloat(con.HHI), con.Level),
			Severity:   map[string]string{"high": "high", "moderate": "medium", "low": "low"}[con.Level]}
		if con.Top1Pct >= 40 {
			r.Severity = "high"
		}
		for _, x := range con.Top {
			if len(r.Items) >= 5 {
				break
			}
			r.Items = append(r.Items, OppItem{Name: x.Name, CustomerID: x.CustomerID, Detail: fmt.Sprintf("%s%% of revenue", trimFloat(x.SharePct)), Value: x.Revenue, Link: "/customers?q=" + urlq(x.Name)})
		}
		add(r)
	}

	for i := range out.Items {
		o := &out.Items[i]
		if o.Summary == "" {
			o.Summary = fmt.Sprintf("%d %s · about %s %s%s", o.Count, map[bool]string{true: "item", false: "items"}[o.Count == 1], home, fmtThousands(o.Value),
				map[string]string{"per_year": " a year", "one_off": "", "at_risk": " at risk"}[o.ValueKind])
		}
	}
	rank := map[string]int{"high": 0, "medium": 1, "low": 2}
	sort.SliceStable(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if rank[a.Severity] != rank[b.Severity] {
			return rank[a.Severity] < rank[b.Severity]
		}
		return a.Value > b.Value
	})
	out.Total = r2(out.Total)
	out.Notes = append(out.Notes, c.Note, "Values are estimates to help you prioritise; each card states how it was calculated.")
	return out
}

// amt formats an amount for a sentence: thousands separators, cents only for small amounts.
func amt(v float64) string {
	if math.Abs(v) >= 100 {
		return fmtThousands(v)
	}
	return fmt.Sprintf("%.2f", v)
}

func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func urlq(s string) string {
	r := strings.NewReplacer("%", "%25", "&", "%26", "#", "%23", "?", "%3F", "+", "%2B", " ", "%20")
	return r.Replace(s)
}

func fmtThousands(v float64) string {
	neg := v < 0
	n := int64(math.Round(math.Abs(v)))
	s := fmt.Sprint(n)
	b := &strings.Builder{}
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func (s *Service) pipeline(org string, c *Conv, today time.Time) *Pipeline {
	p := &Pipeline{}
	a := map[string]interface{}{"org": org, "today": today.Format(dayFmt)}
	var r struct {
		OpenN, ExpN, Won, Lost int
		OpenV, ExpV            float64
		Days                   *float64
	}
	s.scan(&r, `SELECT
		COUNT(*) FILTER (WHERE status = 'sent' AND due_date::date >= (@today)::date) AS open_n,
		COALESCE(SUM((total - tax_total) * f) FILTER (WHERE status = 'sent' AND due_date::date >= (@today)::date), 0) AS open_v,
		COUNT(*) FILTER (WHERE status = 'sent' AND due_date::date < (@today)::date AND due_date::date >= (@today)::date - 90) AS exp_n,
		COALESCE(SUM((total - tax_total) * f) FILTER (WHERE status = 'sent' AND due_date::date < (@today)::date AND due_date::date >= (@today)::date - 90), 0) AS exp_v,
		COUNT(*) FILTER (WHERE status IN ('accepted','converted') AND issue_date > (@today)::date - 365) AS won,
		COUNT(*) FILTER (WHERE (status = 'declined' OR (status = 'sent' AND due_date::date < (@today)::date)) AND issue_date > (@today)::date - 365) AS lost,
		AVG(EXTRACT(EPOCH FROM (updated_at - issue_date::timestamp)) / 86400) FILTER (WHERE status IN ('accepted','converted') AND issue_date > (@today)::date - 365) AS days
	FROM (SELECT i.*, `+c.Factor("i")+` AS f FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type = 'quote') q
	WHERE f IS NOT NULL`, a)
	p.OpenCount, p.OpenValue, p.ExpiredCount, p.ExpiredValue, p.Won12m, p.Lost12m = r.OpenN, r2(r.OpenV), r.ExpN, r2(r.ExpV), r.Won, r.Lost
	if r.Won+r.Lost > 0 {
		p.ConversionPct = fptr(r1(float64(r.Won) / float64(r.Won+r.Lost) * 100))
		p.ExpectedValue = r2((r.OpenV + r.ExpV*0.5) * *p.ConversionPct / 100)
	} else {
		p.ExpectedValue = r2(r.OpenV)
	}
	if r.Days != nil && *r.Days >= 0 {
		p.AvgDaysToClose = fptr(r1(*r.Days))
	}
	return p
}

func (s *Service) renewalCalendar(org string, c *Conv, today time.Time, days int) []RenewalRow {
	out := []RenewalRow{}
	names := s.clientNames(org)
	if s.Exp != nil {
		for _, r := range s.Exp.Renewals(org, days) {
			if r.ExpiresOn == nil || day(*r.ExpiresOn).Before(today.AddDate(0, 0, -7)) {
				continue
			}
			toHome := func(v *float64, cur string) float64 {
				if v == nil {
					return 0
				}
				cur = strings.ToUpper(nonEmpty(cur, c.Home))
				if cur == c.Home {
					return *v
				}
				if q := s.FX.Quote(org, cur, c.Home); q.OK() {
					return *v * q.Rate
				}
				return 0
			}
			client := r.CustomerName
			if n := names[r.CustomerID]; n != "" {
				client = n
			}
			out = append(out, RenewalRow{Date: r.ExpiresOn.Format(dayFmt), Kind: r.Kind, Name: r.Name, Client: client, CustomerID: r.CustomerID,
				Cost: r2(toHome(r.Cost, r.Currency)), Price: r2(toHome(r.Price, r.Currency)), DaysLeft: int(day(*r.ExpiresOn).Sub(today).Hours() / 24), Link: "/expenses?tab=renewals"})
		}
	}
	var recs []struct {
		ID, Number, CK string
		Next           time.Time
		Amt            float64
	}
	s.scan(&recs, `SELECT i.id, i.number, COALESCE(NULLIF(i.customer_id,''), 'n:' || LOWER(TRIM(i.client_name))) AS ck, i.next_recurrence_date AS next,
		(i.total - i.tax_total) * `+c.Factor("i")+` AS amt
	FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.is_recurring AND i.status <> 'void'
		AND i.next_recurrence_date BETWEEN (@today)::date AND (@end)::date AND (i.recurrence_end_date IS NULL OR i.recurrence_end_date >= i.next_recurrence_date)
		AND `+c.Factor("i")+` IS NOT NULL`, map[string]interface{}{"org": org, "today": today.Format(dayFmt), "end": today.AddDate(0, 0, days).Format(dayFmt)})
	for _, r := range recs {
		out = append(out, RenewalRow{Date: r.Next.Format(dayFmt), Kind: "recurring_invoice", Name: r.Number, Client: nameOf(names, r.CK), CustomerID: customerID(r.CK),
			Price: r2(r.Amt), DaysLeft: int(day(r.Next).Sub(today).Hours() / 24), Link: "/invoices/" + r.ID})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}
