package expenses

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Synergy analytics: per-site costs, usage, client billing and margin, plan
// prices learned from the statements, balance runway and recommendations.
// BuildSynergyReport is pure (no database) so it is unit-tested directly.

// SynClient is a customer as the matcher sees it.
type SynClient struct {
	ID, Name, Email, Company, Website string
}

// SynBilledLine is an invoice line that mentions a hosted domain.
type SynBilledLine struct {
	InvoiceID   string    `json:"invoice_id"`
	Number      string    `json:"number"`
	Date        time.Time `json:"date"`
	CustomerID  string    `json:"customer_id"`
	Description string    `json:"description"`
	Currency    string    `json:"currency"`
	Net         float64   `json:"net"`      // excl. tax, invoice currency
	NetHome     float64   `json:"net_home"` // excl. tax, home currency
	Months      float64   `json:"months"`   // service months the line covers
	MonthlyHome float64   `json:"monthly_home"`
}

// SynInput is everything the report needs.
type SynInput struct {
	Today         time.Time
	Basis         string // charge | service
	Home          string
	Txns          []SynergyTxn
	Snaps         []SynergyUsage
	Sites         []SynergySite
	Plans         []SynergyPlan
	Clients       []SynClient
	Billed        []SynBilledLine
	ToHome        func(v float64, cur string) float64
	Balance       *float64
	BalanceAt     *time.Time
	BalanceSource string
	LowBalance    float64
}

// SynPlan is a hosting plan with its learned / set price and disk limit.
type SynPlan struct {
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	MonthlyPrice *float64 `json:"monthly_price"`
	PriceSource  string   `json:"price_source"` // charges | plan change | manual
	LimitMB      *float64 `json:"limit_mb"`
	LimitSource  string   `json:"limit_source"` // usage report | manual
	Sites        int      `json:"sites"`        // active sites on it now
	Evidence     string   `json:"evidence"`
	Manual       bool     `json:"manual"`
}

// SynUsagePoint is one snapshot of one site.
type SynUsagePoint struct {
	Date    string  `json:"date"`
	UsageMB float64 `json:"usage_mb"`
	LimitMB float64 `json:"limit_mb"`
	PctUsed float64 `json:"pct_used"`
	Plan    string  `json:"plan"`
}

// SynSite is one hosted domain.
type SynSite struct {
	Domain         string          `json:"domain"`
	Status         string          `json:"status"` // active | new | cancelled
	StatusDate     string          `json:"status_date,omitempty"`
	Plan           string          `json:"plan"`
	PlanPrice      *float64        `json:"plan_price"`
	MonthlyCost    float64         `json:"monthly_cost"`   // AUD: hosting plan (0 once cancelled)
	DomainMonthly  float64         `json:"domain_monthly"` // AUD: domain renewals of the last 12 months / 12
	TotalMonthly   float64         `json:"total_monthly"`  // AUD
	CostHome       float64         `json:"cost_home"`      // TotalMonthly in the home currency
	LastCharge     string          `json:"last_charge,omitempty"`
	Spend12m       float64         `json:"spend_12m"` // AUD, all charges − credits
	UsageMB        *float64        `json:"usage_mb"`
	LimitMB        *float64        `json:"limit_mb"`
	PctUsed        *float64        `json:"pct_used"`
	RemainingMB    *float64        `json:"remaining_mb"`
	SnapshotDate   string          `json:"snapshot_date,omitempty"`
	GrowthMB       *float64        `json:"growth_mb_month"` // MB per month between the last two snapshots
	Trend          string          `json:"trend"`           // up | down | flat | ""
	DaysToFull     *int            `json:"days_to_full"`
	SuggestedPlan  string          `json:"suggested_plan,omitempty"`
	SuggestedDelta *float64        `json:"suggested_delta,omitempty"` // AUD per month (+ costs more)
	Flags          []string        `json:"flags"`
	CustomerID     string          `json:"customer_id"`
	CustomerName   string          `json:"customer_name"`
	ClientMatch    string          `json:"client_match"` // manual | invoice | website | email | name | none | ""
	NoClient       bool            `json:"no_client"`
	Billed         *float64        `json:"billed"` // per month, home currency
	BilledSource   string          `json:"billed_source"`
	BilledInvoice  string          `json:"billed_invoice,omitempty"`
	BilledDate     string          `json:"billed_date,omitempty"`
	Margin         *float64        `json:"margin"`
	MarginPct      *float64        `json:"margin_pct"`
	Price          *float64        `json:"price"` // manual monthly price
	PriceCurrency  string          `json:"price_currency,omitempty"`
	Notes          string          `json:"notes,omitempty"`
	History        []SynUsagePoint `json:"history"`
	FirstSeen      string          `json:"first_seen"`
	PlanChanges    int             `json:"plan_changes"`
}

// SynDecision is one recommendation with the numbers behind it.
type SynDecision struct {
	Key      string         `json:"key"`
	Type     string         `json:"type"`
	Severity string         `json:"severity"` // danger | warning | info | success
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	Domain   string         `json:"domain,omitempty"`
	Domains  []string       `json:"domains,omitempty"`
	Impact   float64        `json:"impact"` // AUD per month (+ = costs more / at risk, − = saving)
	Action   string         `json:"action,omitempty"`
	Numbers  map[string]any `json:"numbers,omitempty"`
}

// SynMonth is one month of spend by category (AUD, basis-dependent).
type SynMonth struct {
	Month    string  `json:"month"`
	Hosting  float64 `json:"hosting"`
	Domains  float64 `json:"domains"`
	Support  float64 `json:"support"`
	Other    float64 `json:"other"`
	Total    float64 `json:"total"`
	Credits  float64 `json:"credits"` // service credits (refunds), included (negatively) in the totals
	Topups   float64 `json:"topups"`  // money added — not an expense
	Count    int     `json:"count"`
	TotalHom float64 `json:"total_home"`
}

// SynSpell is a period with a negative balance.
type SynSpell struct {
	From   string  `json:"from"`
	To     string  `json:"to"` // "" while still negative
	Lowest float64 `json:"lowest"`
}

// SynComparison explains what changed between two statement months.
type SynComparison struct {
	From        string           `json:"from"`
	To          string           `json:"to"`
	CostFrom    float64          `json:"cost_from"`
	CostTo      float64          `json:"cost_to"`
	Delta       float64          `json:"delta"`
	DeltaPct    *float64         `json:"delta_pct"`
	New         []string         `json:"new"`
	Cancelled   []string         `json:"cancelled"`
	PlanChanges []map[string]any `json:"plan_changes"`
	Lines       []string         `json:"lines"` // plain English
	ByCategory  []map[string]any `json:"by_category"`
}

// SynKPIs are the headline numbers.
type SynKPIs struct {
	ThisMonth        float64  `json:"this_month"`
	LastMonth        float64  `json:"last_month"`
	YTD              float64  `json:"ytd"`
	ThisMonthHome    float64  `json:"this_month_home"`
	LastMonthHome    float64  `json:"last_month_home"`
	YTDHome          float64  `json:"ytd_home"`
	MonthChangePct   *float64 `json:"month_change_pct"`
	ActiveSites      int      `json:"active_sites"`
	NewSites         int      `json:"new_sites"`
	CancelledSites   int      `json:"cancelled_sites"`
	RunRate          float64  `json:"run_rate"` // AUD per month, active hosting plans
	RunRateHome      float64  `json:"run_rate_home"`
	DomainsMonthly   float64  `json:"domains_monthly"`
	Balance          *float64 `json:"balance"`
	BalanceAt        string   `json:"balance_at,omitempty"`
	BalanceSource    string   `json:"balance_source,omitempty"`
	RunwayDays       *int     `json:"runway_days"`
	RunwayDate       string   `json:"runway_date,omitempty"`
	TopupNeeded      float64  `json:"topup_needed"` // to cover the next charge run
	NextChargeDate   string   `json:"next_charge_date,omitempty"`
	TopupsTotal      float64  `json:"topups_total"`
	TopupsYTD        float64  `json:"topups_ytd"`
	TopupsMonthlyAvg float64  `json:"topups_monthly_avg"`
	PlanChangesMonth float64  `json:"plan_changes_month"` // net cost of plan changes in the latest statement month
	BilledMonthly    float64  `json:"billed_monthly"`     // home: what clients are billed per month for hosting
	MarginMonthly    *float64 `json:"margin_monthly"`     // home
	Unbilled         int      `json:"unbilled"`
	UnbilledCost     float64  `json:"unbilled_cost"` // AUD per month
	OverQuota        int      `json:"over_quota"`
	NearFull         int      `json:"near_full"`
	LatestMonth      string   `json:"latest_month"`
}

// SynergyReport is the Synergy analytics view.
type SynergyReport struct {
	Currency       string          `json:"currency"` // AUD
	Home           string          `json:"home"`
	Basis          string          `json:"basis"`
	Today          string          `json:"today"`
	HasData        bool            `json:"has_data"`
	HasStatements  bool            `json:"has_statements"`
	HasUsage       bool            `json:"has_usage"`
	KPIs           SynKPIs         `json:"kpis"`
	Months         []SynMonth      `json:"months"`
	Sites          []SynSite       `json:"sites"`
	Decisions      []SynDecision   `json:"decisions"`
	Comparison     *SynComparison  `json:"comparison"`
	Plans          []SynPlan       `json:"plans"`
	BalancePoints  []SynBalancePt  `json:"balance_points"`
	NegativeSpells []SynSpell      `json:"negative_spells"`
	Gaps           []BalanceGap    `json:"gaps"`
	Snapshots      []string        `json:"snapshots"`
	StatementFrom  string          `json:"statement_from,omitempty"`
	StatementTo    string          `json:"statement_to,omitempty"`
	StatementMons  []string        `json:"statement_months"`
	Uploads        []SynergyUpload `json:"uploads"`
	Alerts         []Alert         `json:"alerts"`
	Kinds          map[string]int  `json:"kinds"`
}

// SynBalancePt is the account balance at the end of a day.
type SynBalancePt struct {
	Date    string  `json:"date"`
	Balance float64 `json:"balance"`
}

var genericMail = map[string]bool{"gmail.com": true, "googlemail.com": true, "yahoo.com": true, "yahoo.co.uk": true, "ymail.com": true, "outlook.com": true,
	"hotmail.com": true, "live.com": true, "msn.com": true, "icloud.com": true, "me.com": true, "aol.com": true, "proton.me": true, "protonmail.com": true,
	"bigpond.com": true, "bigpond.net.au": true, "optusnet.com.au": true, "gmx.com": true, "zoho.com": true, "mail.com": true, "yandex.com": true}

var reNonAlnum = regexp.MustCompile(`[^a-z0-9]`)

func alnum(s string) string { return reNonAlnum.ReplaceAllString(strings.ToLower(s), "") }

func roundTo(v, step float64) float64 { return math.Round(v/step) * step }

func fptr(v float64) *float64 { return &v }

func iptr(v int) *int { return &v }

// planEvent is a known plan for a domain at a time.
type planEvent struct {
	at       time.Time
	plan     string // the plan from this moment
	from     string // plan changes: the plan before
	isChange bool
}

type planTimeline map[string][]planEvent

func buildPlanTimeline(txns []SynergyTxn, snaps []SynergyUsage) planTimeline {
	tl := planTimeline{}
	for _, t := range txns {
		if (t.Kind == SynPlanUpgrade || t.Kind == SynPlanDowngrade) && t.Domain != "" && t.PlanTo != "" {
			tl[t.Domain] = append(tl[t.Domain], planEvent{at: t.Date, plan: t.PlanTo, from: t.PlanFrom, isChange: true})
		}
	}
	for _, u := range snaps {
		if u.Plan != "" {
			tl[u.Domain] = append(tl[u.Domain], planEvent{at: u.SnapshotDate, plan: u.Plan})
		}
	}
	for d := range tl {
		ev := tl[d]
		sort.SliceStable(ev, func(i, j int) bool { return ev[i].at.Before(ev[j].at) })
	}
	return tl
}

// at returns the plan of domain at time t ("" when unknown).
func (tl planTimeline) at(domain string, t time.Time) string {
	ev := tl[domain]
	var best *planEvent
	for i := range ev {
		if !ev[i].at.After(t) {
			best = &ev[i]
		}
	}
	if best != nil {
		return best.plan
	}
	if len(ev) > 0 {
		if ev[0].isChange {
			return ev[0].from
		}
		return ev[0].plan
	}
	return ""
}

func planKey(p string) string { return strings.ToLower(strings.Join(strings.Fields(p), " ")) }

type priceObs struct {
	at     time.Time
	amount float64
	domain string
}

// LearnPlans works out each plan's monthly price (from monthly hosting
// charges, then from plan-change pro-ratas) and disk limit (usage reports).
func LearnPlans(txns []SynergyTxn, snaps []SynergyUsage, manual []SynergyPlan) (map[string]*SynPlan, []map[string]any) {
	tl := buildPlanTimeline(txns, snaps)
	plans := map[string]*SynPlan{}
	get := func(name string) *SynPlan {
		k := planKey(name)
		if k == "" {
			return nil
		}
		p := plans[k]
		if p == nil {
			p = &SynPlan{Key: k, Name: strings.TrimSpace(name)}
			plans[k] = p
		}
		return p
	}
	obs := map[string][]priceObs{}
	for _, t := range txns {
		if t.Kind != SynHostingMonthly || t.Debit <= 0 || t.Domain == "" {
			continue
		}
		pl := tl.at(t.Domain, t.Date)
		if pl == "" {
			continue
		}
		get(pl)
		obs[planKey(pl)] = append(obs[planKey(pl)], priceObs{t.Date, t.Debit, t.Domain})
	}
	priceChanges := []map[string]any{}
	for k, os := range obs {
		sort.SliceStable(os, func(i, j int) bool { return os[i].at.Before(os[j].at) })
		last := os[len(os)-1]
		p := plans[k]
		p.MonthlyPrice, p.PriceSource = fptr(last.amount), "charges"
		doms := map[string]bool{}
		for _, o := range os {
			doms[o.domain] = true
		}
		p.Evidence = fmt.Sprintf("%d monthly charge%s on %d site%s", len(os), plural(len(os)), len(doms), plural(len(doms)))
		// Same plan charged differently over time (per site, consecutive charges).
		bySite := map[string][]priceObs{}
		for _, o := range os {
			bySite[o.domain] = append(bySite[o.domain], o)
		}
		for d, so := range bySite {
			for i := 1; i < len(so); i++ {
				if math.Abs(so[i].amount-so[i-1].amount) >= 0.005 {
					priceChanges = append(priceChanges, map[string]any{"plan": p.Name, "domain": d, "from": so[i-1].amount, "to": so[i].amount, "date": so[i].at.Format(dayFmt)})
				}
			}
		}
	}
	// Plan changes: the pro-rata amount × days in the month ÷ days left ≈ the price difference.
	type change struct {
		from, to string
		diff     float64
	}
	changes := []change{}
	for _, t := range txns {
		if (t.Kind != SynPlanUpgrade && t.Kind != SynPlanDowngrade) || t.PlanFrom == "" || t.PlanTo == "" {
			continue
		}
		get(t.PlanFrom)
		get(t.PlanTo)
		day := Day(t.Date)
		paidThrough := time.Date(day.Year(), day.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		for _, c := range txns {
			if c.Domain == t.Domain && (c.Kind == SynHostingMonthly || c.Kind == SynHostingProrata) && !c.Date.After(t.Date) && c.PeriodTo != nil && c.PeriodTo.After(paidThrough) {
				paidThrough = *c.PeriodTo
			}
		}
		left := paidThrough.Sub(day).Hours() / 24
		monthDays := paidThrough.Sub(paidThrough.AddDate(0, -1, 0)).Hours() / 24
		if left < 1 {
			continue
		}
		amt := t.Debit - t.Credit
		diff := roundTo(amt*monthDays/left, 0.25)
		changes = append(changes, change{planKey(t.PlanFrom), planKey(t.PlanTo), diff})
	}
	for pass := 0; pass < 6; pass++ {
		progress := false
		for _, c := range changes {
			f, to := plans[c.from], plans[c.to]
			if f == nil || to == nil {
				continue
			}
			if f.MonthlyPrice != nil && to.MonthlyPrice == nil && *f.MonthlyPrice+c.diff > 0 {
				to.MonthlyPrice, to.PriceSource = fptr(r2(*f.MonthlyPrice+c.diff)), "plan change"
				to.Evidence = fmt.Sprintf("≈ %s %s %s a month (from a plan-change pro-rata)", f.Name, map[bool]string{true: "+", false: "−"}[c.diff >= 0], audFmt(math.Abs(c.diff)))
				progress = true
			} else if to.MonthlyPrice != nil && f.MonthlyPrice == nil && *to.MonthlyPrice-c.diff > 0 {
				f.MonthlyPrice, f.PriceSource = fptr(r2(*to.MonthlyPrice-c.diff)), "plan change"
				f.Evidence = fmt.Sprintf("≈ %s %s %s a month (from a plan-change pro-rata)", to.Name, map[bool]string{true: "−", false: "+"}[c.diff >= 0], audFmt(math.Abs(c.diff)))
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	// Disk limits from the usage reports (latest).
	sn := append([]SynergyUsage(nil), snaps...)
	sort.SliceStable(sn, func(i, j int) bool { return sn[i].SnapshotDate.Before(sn[j].SnapshotDate) })
	for _, u := range sn {
		if p := get(u.Plan); p != nil && u.LimitMB > 0 {
			p.LimitMB, p.LimitSource = fptr(u.LimitMB), "usage report"
		}
	}
	for _, m := range manual {
		p := get(nonEmpty(m.Name, m.Key))
		if p == nil {
			continue
		}
		if m.MonthlyPrice != nil {
			p.MonthlyPrice, p.PriceSource, p.Manual = fptr(*m.MonthlyPrice), "manual", true
		}
		if m.LimitMB != nil {
			p.LimitMB, p.LimitSource, p.Manual = fptr(*m.LimitMB), "manual", true
		}
	}
	return plans, priceChanges
}

// monthsCovered reads how many months an invoice line pays for.
var (
	reMonthsN = regexp.MustCompile(`(?i)\b(\d{1,2})\s*(?:months?|mths?|mos?)\b`)
	reYearsN  = regexp.MustCompile(`(?i)\b(\d)\s*(?:years?|yrs?)\b`)
	reYearly  = regexp.MustCompile(`(?i)\b(annual|annually|yearly|per\s+year|p\.?a\.?|/\s*yr|/\s*year|12\s*months)\b`)
	reQuarter = regexp.MustCompile(`(?i)\b(quarter|quarterly)\b`)
	reHalfYr  = regexp.MustCompile(`(?i)\b(half[- ]year|semi[- ]annual|6\s*months)\b`)
)

func monthsCovered(desc, unit string, qty float64) float64 {
	u := strings.ToLower(unit)
	if qty <= 0 {
		qty = 1
	}
	switch {
	case strings.HasPrefix(u, "month") || u == "mo" || u == "mth":
		return qty
	case strings.HasPrefix(u, "year") || u == "yr" || u == "annum":
		return 12 * qty
	}
	switch {
	case reHalfYr.MatchString(desc):
		return 6
	case reYearly.MatchString(desc):
		return 12
	case reQuarter.MatchString(desc):
		return 3
	}
	if m := reYearsN.FindStringSubmatch(desc); m != nil {
		var n float64
		fmt.Sscanf(m[1], "%f", &n)
		if n > 0 {
			return 12 * n
		}
	}
	if m := reMonthsN.FindStringSubmatch(desc); m != nil {
		var n float64
		fmt.Sscanf(m[1], "%f", &n)
		if n > 0 {
			return n
		}
	}
	return 1
}

// MatchClient finds the customer a hosted domain belongs to.
func MatchClient(domain string, clients []SynClient) (string, string) {
	d := cleanDomain(domain)
	label := alnum(strings.SplitN(d, ".", 2)[0])
	for _, c := range clients {
		if c.Website != "" && cleanDomain(c.Website) == d {
			return c.ID, "website"
		}
	}
	for _, c := range clients {
		if i := strings.LastIndex(c.Email, "@"); i >= 0 {
			ed := strings.ToLower(strings.TrimSpace(c.Email[i+1:]))
			if !genericMail[ed] && (ed == d || strings.TrimPrefix(ed, "mail.") == d) {
				return c.ID, "email"
			}
		}
	}
	if len(label) >= 4 {
		for _, c := range clients {
			for _, n := range []string{c.Company, c.Name} {
				cn := alnum(n)
				if len(cn) < 4 {
					continue
				}
				if cn == label || (len(label) >= 5 && strings.HasPrefix(cn, label)) || (len(cn) >= 5 && strings.HasPrefix(label, cn)) {
					return c.ID, "name"
				}
			}
		}
	}
	return "", ""
}

func isHostingKind(k string) bool {
	return k == SynHostingMonthly || k == SynHostingProrata || k == SynPlanUpgrade || k == SynPlanDowngrade || k == SynCancelCredit
}

// BuildSynergyReport computes the analytics.
func BuildSynergyReport(in SynInput) *SynergyReport {
	today := Day(in.Today)
	if in.Basis != "service" {
		in.Basis = "charge"
	}
	toHome := in.ToHome
	if toHome == nil {
		toHome = func(v float64, cur string) float64 { return v }
	}
	home := nonEmpty(in.Home, "AUD")
	txns := append([]SynergyTxn(nil), in.Txns...)
	sort.SliceStable(txns, func(i, j int) bool {
		if !txns[i].Date.Equal(txns[j].Date) {
			return txns[i].Date.Before(txns[j].Date)
		}
		return txns[i].Seq < txns[j].Seq
	})
	snaps := append([]SynergyUsage(nil), in.Snaps...)
	sort.SliceStable(snaps, func(i, j int) bool {
		if !snaps[i].SnapshotDate.Equal(snaps[j].SnapshotDate) {
			return snaps[i].SnapshotDate.Before(snaps[j].SnapshotDate)
		}
		return snaps[i].Domain < snaps[j].Domain
	})
	rep := &SynergyReport{Currency: "AUD", Home: home, Basis: in.Basis, Today: today.Format(dayFmt), Months: []SynMonth{}, Sites: []SynSite{}, Decisions: []SynDecision{},
		Plans: []SynPlan{}, BalancePoints: []SynBalancePt{}, NegativeSpells: []SynSpell{}, Gaps: []BalanceGap{}, Snapshots: []string{}, StatementMons: []string{},
		Uploads: []SynergyUpload{}, Alerts: []Alert{}, Kinds: map[string]int{}}
	rep.HasStatements, rep.HasUsage = len(txns) > 0, len(snaps) > 0
	rep.HasData = rep.HasStatements || rep.HasUsage
	if !rep.HasData {
		return rep
	}

	// ---------- statement months, spend, balance ----------
	rows := make([]SynRow, len(txns))
	for i, t := range txns {
		rows[i] = t.Row()
		rep.Kinds[t.Kind]++
	}
	rep.Gaps = ReconcileSynergy(rows)
	monthSet := map[string]bool{}
	byMonth := map[string]*SynMonth{}
	mget := func(m string) *SynMonth {
		x := byMonth[m]
		if x == nil {
			x = &SynMonth{Month: m}
			byMonth[m] = x
		}
		return x
	}
	var topupsTotal, topupsYTD float64
	for _, t := range txns {
		monthSet[t.Month] = true
		if t.Kind == SynTopup {
			mget(t.Month).Topups += t.Credit - t.Debit
			topupsTotal += t.Credit - t.Debit
			if t.Date.Year() == today.Year() {
				topupsYTD += t.Credit - t.Debit
			}
			continue
		}
		if math.Abs(t.Amount) < 0.005 {
			continue
		}
		m := mget(groupMonth(t, in.Basis))
		switch nonEmpty(t.Category, CatOther) {
		case CatHosting:
			m.Hosting += t.Amount
		case CatDomains:
			m.Domains += t.Amount
		case CatSupport:
			m.Support += t.Amount
		default:
			m.Other += t.Amount
		}
		if t.Amount < 0 {
			m.Credits -= t.Amount
		}
		m.Total += t.Amount
		m.Count++
	}
	for m := range monthSet {
		rep.StatementMons = append(rep.StatementMons, m)
	}
	sort.Strings(rep.StatementMons)
	if len(txns) > 0 {
		rep.StatementFrom, rep.StatementTo = txns[0].Date.Format(dayFmt), txns[len(txns)-1].Date.Format(dayFmt)
	}
	// Month series: from the first statement month to the current month (max 24).
	if len(txns) > 0 {
		start := monthStart(txns[0].Date)
		end := monthStart(today)
		for k := range byMonth {
			if t, err := time.Parse("2006-01", k); err == nil && t.After(end) {
				end = t
			}
		}
		if end.Sub(start).Hours()/24 > 731 {
			start = end.AddDate(-2, 1, 0)
		}
		for m := start; !m.After(end); m = m.AddDate(0, 1, 0) {
			x := mget(m.Format("2006-01"))
			x.Hosting, x.Domains, x.Support, x.Other, x.Total = r2(x.Hosting), r2(x.Domains), r2(x.Support), r2(x.Other), r2(x.Total)
			x.Credits, x.Topups = r2(x.Credits), r2(x.Topups)
			x.TotalHom = r2(toHome(x.Total, "AUD"))
			rep.Months = append(rep.Months, *x)
		}
	}
	thisM, lastM := today.Format("2006-01"), today.AddDate(0, -1, 0).Format("2006-01")
	if byMonth[thisM] != nil {
		rep.KPIs.ThisMonth = r2(byMonth[thisM].Total)
	}
	if byMonth[lastM] != nil {
		rep.KPIs.LastMonth = r2(byMonth[lastM].Total)
	}
	for k, x := range byMonth {
		if strings.HasPrefix(k, fmt.Sprint(today.Year())+"-") && k <= thisM {
			rep.KPIs.YTD += x.Total
		}
	}
	rep.KPIs.YTD = r2(rep.KPIs.YTD)
	rep.KPIs.ThisMonthHome, rep.KPIs.LastMonthHome, rep.KPIs.YTDHome = r2(toHome(rep.KPIs.ThisMonth, "AUD")), r2(toHome(rep.KPIs.LastMonth, "AUD")), r2(toHome(rep.KPIs.YTD, "AUD"))
	rep.KPIs.MonthChangePct = pctChange(rep.KPIs.ThisMonth, rep.KPIs.LastMonth)
	rep.KPIs.TopupsTotal, rep.KPIs.TopupsYTD = r2(topupsTotal), r2(topupsYTD)
	if n := len(rep.StatementMons); n > 0 {
		rep.KPIs.TopupsMonthlyAvg = r2(topupsTotal / float64(n))
		rep.KPIs.LatestMonth = rep.StatementMons[n-1]
	}
	// Balance points (end of day) and negative spells.
	var spell *SynSpell
	for i, t := range txns {
		if !t.HasBalance {
			continue
		}
		d := t.Date.Format(dayFmt)
		if n := len(rep.BalancePoints); n > 0 && rep.BalancePoints[n-1].Date == d {
			rep.BalancePoints[n-1].Balance = t.Balance
		} else {
			rep.BalancePoints = append(rep.BalancePoints, SynBalancePt{Date: d, Balance: t.Balance})
		}
		if t.Balance < -0.004 {
			if spell == nil {
				spell = &SynSpell{From: d, Lowest: t.Balance}
			}
			if t.Balance < spell.Lowest {
				spell.Lowest = t.Balance
			}
		} else if spell != nil {
			spell.To = d
			rep.NegativeSpells = append(rep.NegativeSpells, *spell)
			spell = nil
		}
		_ = i
	}
	if spell != nil {
		rep.NegativeSpells = append(rep.NegativeSpells, *spell)
	}

	// ---------- plans ----------
	plans, priceChanges := LearnPlans(txns, snaps, in.Plans)
	tl := buildPlanTimeline(txns, snaps)

	// ---------- sites ----------
	snapDates := []time.Time{}
	seenDate := map[string]bool{}
	for _, u := range snaps {
		k := u.SnapshotDate.Format(dayFmt)
		if !seenDate[k] {
			seenDate[k] = true
			snapDates = append(snapDates, u.SnapshotDate)
			rep.Snapshots = append(rep.Snapshots, k)
		}
	}
	var latestSnap, prevSnap time.Time
	if n := len(snapDates); n > 0 {
		latestSnap = snapDates[n-1]
		if n > 1 {
			prevSnap = snapDates[n-2]
		}
	}
	siteSnaps := map[string][]SynergyUsage{}
	for _, u := range snaps {
		siteSnaps[u.Domain] = append(siteSnaps[u.Domain], u)
	}
	siteTx := map[string][]SynergyTxn{}
	domains := map[string]bool{}
	for _, t := range txns {
		if t.Domain == "" {
			continue
		}
		siteTx[t.Domain] = append(siteTx[t.Domain], t)
		if isHostingKind(t.Kind) {
			domains[t.Domain] = true
		}
	}
	for d := range siteSnaps {
		domains[d] = true
	}
	overrides := map[string]SynergySite{}
	for _, s := range in.Sites {
		overrides[s.Domain] = s
	}
	clientName := map[string]string{}
	for _, c := range in.Clients {
		clientName[c.ID] = nonEmpty(strings.TrimSpace(c.Name), c.Company)
	}
	billedBy := map[string][]SynBilledLine{}
	for _, b := range in.Billed {
		for d := range domains {
			if strings.Contains(strings.ToLower(b.Description), d) {
				billedBy[d] = append(billedBy[d], b)
			}
		}
	}
	yearAgo := today.AddDate(-1, 0, 0)
	names := []string{}
	for d := range domains {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		st := SynSite{Domain: d, Status: "active", Flags: []string{}, History: []SynUsagePoint{}}
		ss := siteSnaps[d]
		for _, u := range ss {
			st.History = append(st.History, SynUsagePoint{Date: u.SnapshotDate.Format(dayFmt), UsageMB: u.UsageMB, LimitMB: u.LimitMB, PctUsed: u.PctUsed, Plan: u.Plan})
		}
		var first time.Time
		var lastCharge, lastCancel, lastProrata time.Time
		var lastMonthly *SynergyTxn
		for i, t := range siteTx[d] {
			if first.IsZero() || t.Date.Before(first) {
				first = t.Date
			}
			if t.Kind != SynTopup && !t.Date.Before(yearAgo) {
				st.Spend12m += t.Amount
			}
			switch t.Kind {
			case SynHostingMonthly:
				lastCharge = t.Date
				lastMonthly = &siteTx[d][i]
			case SynHostingProrata:
				lastCharge, lastProrata = t.Date, t.Date
			case SynCancelCredit:
				lastCancel = t.Date
			case SynPlanUpgrade, SynPlanDowngrade:
				st.PlanChanges++
			case SynDomainRenewal:
				if !t.Date.Before(yearAgo) {
					st.DomainMonthly += t.Amount / 12
				}
			}
		}
		if len(ss) > 0 && (first.IsZero() || ss[0].SnapshotDate.Before(first)) {
			first = ss[0].SnapshotDate
		}
		st.FirstSeen = first.Format(dayFmt)
		st.Spend12m, st.DomainMonthly = r2(st.Spend12m), r2(st.DomainMonthly)
		if !lastCharge.IsZero() {
			st.LastCharge = lastCharge.Format(dayFmt)
		}
		inLatest := len(ss) > 0 && ss[len(ss)-1].SnapshotDate.Equal(latestSnap)
		inPrev := false
		for _, u := range ss {
			if !prevSnap.IsZero() && u.SnapshotDate.Equal(prevSnap) {
				inPrev = true
			}
		}
		switch {
		case !lastCancel.IsZero() && !lastCancel.Before(lastCharge):
			st.Status, st.StatusDate = "cancelled", lastCancel.Format(dayFmt)
		case len(ss) > 0 && !inLatest && !latestSnap.IsZero() && (lastCharge.IsZero() || latestSnap.After(lastCharge.AddDate(0, 0, 10))):
			st.Status, st.StatusDate = "cancelled", latestSnap.Format(dayFmt)
		case !lastProrata.IsZero() && today.Sub(Day(lastProrata)).Hours()/24 <= 62:
			st.Status, st.StatusDate = "new", lastProrata.Format(dayFmt)
		case inLatest && !prevSnap.IsZero() && !inPrev && len(siteTx[d]) == 0:
			st.Status, st.StatusDate = "new", latestSnap.Format(dayFmt)
		case inLatest && !prevSnap.IsZero() && !inPrev && !first.Before(prevSnap):
			st.Status, st.StatusDate = "new", first.Format(dayFmt)
		}
		// Plan now: the last plan change after the latest snapshot, else the snapshot.
		st.Plan = tl.at(d, today.Add(24*time.Hour))
		if st.Plan == "" && len(ss) > 0 {
			st.Plan = ss[len(ss)-1].Plan
		}
		if p := plans[planKey(st.Plan)]; p != nil && p.MonthlyPrice != nil {
			st.PlanPrice = fptr(*p.MonthlyPrice)
		}
		cost := 0.0
		switch {
		case st.PlanPrice != nil:
			cost = *st.PlanPrice
		case lastMonthly != nil:
			cost = lastMonthly.Debit
		}
		if st.Status == "cancelled" {
			cost = 0
			st.DomainMonthly = 0
		}
		st.MonthlyCost = r2(cost)
		st.TotalMonthly = r2(st.MonthlyCost + st.DomainMonthly)
		st.CostHome = r2(toHome(st.TotalMonthly, "AUD"))
		// Usage.
		if n := len(ss); n > 0 {
			u := ss[n-1]
			st.UsageMB, st.LimitMB, st.PctUsed, st.RemainingMB = fptr(u.UsageMB), fptr(u.LimitMB), fptr(u.PctUsed), fptr(u.RemainingMB)
			st.SnapshotDate = u.SnapshotDate.Format(dayFmt)
			if n > 1 {
				p := ss[n-2]
				days := u.SnapshotDate.Sub(p.SnapshotDate).Hours() / 24
				if days > 0 {
					g := math.Round((u.UsageMB-p.UsageMB)/days*30.44*10) / 10
					st.GrowthMB = &g
					thr := math.Max(20, u.LimitMB*0.01)
					switch {
					case g > thr:
						st.Trend = "up"
					case g < -thr:
						st.Trend = "down"
					default:
						st.Trend = "flat"
					}
				}
			}
			if st.Status != "cancelled" {
				switch {
				case u.RemainingMB <= 0 || u.PctUsed >= 100:
					st.DaysToFull = iptr(0)
				case st.GrowthMB != nil && *st.GrowthMB > 0:
					dd := int(math.Ceil(u.RemainingMB / (*st.GrowthMB / 30.44)))
					if dd > 3650 {
						dd = 3650
					}
					st.DaysToFull = iptr(dd)
				}
				if u.RemainingMB < 0 || u.PctUsed >= 100 {
					st.Flags = append(st.Flags, "over_quota")
				} else if u.PctUsed >= 75 {
					st.Flags = append(st.Flags, "near_full")
				}
				if st.GrowthMB != nil && *st.GrowthMB >= math.Max(1024, u.LimitMB*0.25) {
					st.Flags = append(st.Flags, "fast_growth")
				}
				// Cheapest plan that fits usage (+ a month's growth) with 25% headroom.
				need := u.UsageMB
				if st.GrowthMB != nil && *st.GrowthMB > 0 {
					need += *st.GrowthMB
				}
				need *= 1.25
				var best *SynPlan
				for _, p := range plans {
					if p.MonthlyPrice == nil || p.LimitMB == nil || *p.LimitMB < need {
						continue
					}
					if best == nil || *p.MonthlyPrice < *best.MonthlyPrice || (*p.MonthlyPrice == *best.MonthlyPrice && *p.LimitMB > *best.LimitMB) {
						best = p
					}
				}
				if best != nil && best.Key != planKey(st.Plan) && st.PlanPrice != nil {
					delta := r2(*best.MonthlyPrice - *st.PlanPrice)
					fits := u.LimitMB >= need
					// Upgrades when it does not fit; downgrades only for a real saving.
					if (!fits && delta >= 0) || (fits && delta <= -0.5 && u.PctUsed < 40 && st.Trend != "up") {
						st.SuggestedPlan, st.SuggestedDelta = best.Name, &delta
					}
				}
			}
		}
		// Client and billing.
		ov, hasOv := overrides[d]
		if hasOv {
			st.Notes, st.NoClient = ov.Notes, ov.NoClient
			if ov.Price != nil {
				st.Price, st.PriceCurrency = fptr(*ov.Price), nonEmpty(ov.PriceCurrency, home)
			}
		}
		bl := billedBy[d]
		sort.SliceStable(bl, func(i, j int) bool { return bl[i].Date.After(bl[j].Date) })
		switch {
		case hasOv && ov.CustomerID != "":
			st.CustomerID, st.ClientMatch = ov.CustomerID, "manual"
		case st.NoClient:
			st.ClientMatch = "none"
		case len(bl) > 0 && bl[0].CustomerID != "":
			st.CustomerID, st.ClientMatch = bl[0].CustomerID, "invoice"
		default:
			st.CustomerID, st.ClientMatch = MatchClient(d, in.Clients)
		}
		if st.CustomerID != "" {
			st.CustomerName = nonEmpty(clientName[st.CustomerID], "(deleted customer)")
		}
		switch {
		case st.Price != nil:
			v := r2(toHome(*st.Price, st.PriceCurrency))
			st.Billed, st.BilledSource = &v, "manual"
		case len(bl) > 0:
			var v float64
			for _, b := range bl {
				if b.InvoiceID == bl[0].InvoiceID {
					v += b.MonthlyHome
				}
			}
			v = r2(v)
			st.Billed, st.BilledSource, st.BilledInvoice, st.BilledDate = &v, "invoice", bl[0].Number, bl[0].Date.Format(dayFmt)
		}
		if st.Status != "cancelled" && !st.NoClient {
			if st.CustomerID == "" {
				st.Flags = append(st.Flags, "no_client")
			}
			if st.Billed == nil || *st.Billed <= 0 {
				st.Flags = append(st.Flags, "unbilled")
			}
		}
		if st.Billed != nil && st.Status != "cancelled" {
			m := r2(*st.Billed - st.CostHome)
			st.Margin = &m
			if *st.Billed > 0 {
				p := math.Round(m / *st.Billed * 1000) / 10
				st.MarginPct = &p
			}
			if m < 0 {
				st.Flags = append(st.Flags, "below_cost")
			}
		}
		for _, pc := range priceChanges {
			if pc["domain"] == d {
				st.Flags = append(st.Flags, "price_change")
				break
			}
		}
		rep.Sites = append(rep.Sites, st)
	}
	// Plans list.
	active := map[string]int{}
	for _, st := range rep.Sites {
		if st.Status != "cancelled" {
			active[planKey(st.Plan)]++
		}
	}
	for _, p := range plans {
		p.Sites = active[p.Key]
		rep.Plans = append(rep.Plans, *p)
	}
	sort.SliceStable(rep.Plans, func(i, j int) bool {
		a, b := rep.Plans[i], rep.Plans[j]
		if (a.MonthlyPrice == nil) != (b.MonthlyPrice == nil) {
			return a.MonthlyPrice != nil
		}
		if a.MonthlyPrice != nil && *a.MonthlyPrice != *b.MonthlyPrice {
			return *a.MonthlyPrice < *b.MonthlyPrice
		}
		return a.Name < b.Name
	})

	// ---------- KPIs ----------
	k := &rep.KPIs
	var billed, marginSum float64
	hasMargin := false
	for _, st := range rep.Sites {
		switch st.Status {
		case "cancelled":
			k.CancelledSites++
			continue
		case "new":
			k.NewSites++
		}
		k.ActiveSites++
		k.RunRate += st.MonthlyCost
		k.DomainsMonthly += st.DomainMonthly
		if st.Billed != nil {
			billed += *st.Billed
		}
		if st.Margin != nil {
			marginSum += *st.Margin
			hasMargin = true
		}
		if containsStr(st.Flags, "unbilled") {
			k.Unbilled++
			k.UnbilledCost += st.TotalMonthly
		}
		if containsStr(st.Flags, "over_quota") {
			k.OverQuota++
		}
		if containsStr(st.Flags, "near_full") {
			k.NearFull++
		}
	}
	k.RunRate, k.DomainsMonthly, k.UnbilledCost = r2(k.RunRate), r2(k.DomainsMonthly), r2(k.UnbilledCost)
	k.RunRateHome = r2(toHome(k.RunRate, "AUD"))
	k.BilledMonthly = r2(billed)
	if hasMargin {
		k.MarginMonthly = fptr(r2(marginSum))
	}
	if lm := k.LatestMonth; lm != "" {
		var pc float64
		for _, t := range txns {
			if t.Month == lm && (t.Kind == SynPlanUpgrade || t.Kind == SynPlanDowngrade) {
				pc += t.Amount
			}
		}
		k.PlanChangesMonth = r2(pc)
	}
	// Balance and runway.
	bal, balAt, balSrc := in.Balance, in.BalanceAt, in.BalanceSource
	if len(txns) > 0 {
		last := txns[len(txns)-1]
		if last.HasBalance && (bal == nil || balAt == nil || !balAt.After(last.Date)) {
			bal, balAt, balSrc = fptr(last.Balance), &last.Date, "statement"
		}
	}
	if bal != nil {
		k.Balance = fptr(r2(*bal))
		k.BalanceSource = balSrc
		if balAt != nil {
			k.BalanceAt = balAt.Format(dayFmt)
		}
		// The monthly hosting run is charged around the same day each month.
		days := []int{}
		for _, t := range txns {
			if t.Kind == SynHostingMonthly {
				days = append(days, t.Date.Day())
			}
		}
		chargeDay := 25
		if len(days) > 0 {
			sort.Ints(days)
			chargeDay = days[len(days)/2]
		}
		next := time.Date(today.Year(), today.Month(), chargeDay, 0, 0, 0, 0, time.UTC)
		if next.Month() != today.Month() || !next.After(today) {
			next = time.Date(today.Year(), today.Month()+1, min(chargeDay, 28), 0, 0, 0, 0, time.UTC)
		}
		k.NextChargeDate = next.Format(dayFmt)
		b := *bal
		switch {
		case b < 0:
			k.RunwayDays, k.RunwayDate = iptr(0), today.Format(dayFmt)
			k.TopupNeeded = r2(-b + k.RunRate)
		case k.RunRate > 0:
			cycles := int(math.Floor(b / k.RunRate)) // charge runs fully covered
			if cycles > 24 {
				cycles = 24
			}
			out := next.AddDate(0, cycles, 0)
			k.RunwayDays, k.RunwayDate = iptr(int(out.Sub(today).Hours()/24)), out.Format(dayFmt)
			if cycles == 0 {
				k.TopupNeeded = r2(k.RunRate - b)
			}
		}
	}

	// ---------- comparison of the last two statement months ----------
	if n := len(rep.StatementMons); n >= 2 {
		rep.Comparison = compareMonths(txns, rep.StatementMons[n-2], rep.StatementMons[n-1], snaps, prevSnap, latestSnap)
	}

	// ---------- decisions ----------
	rep.Decisions = synDecisions(rep, txns, plans, priceChanges, today)
	rep.Alerts = synAlerts(rep, in.LowBalance)
	return rep
}

func monthName(m string) string {
	t, err := time.Parse("2006-01", m)
	if err != nil {
		return m
	}
	return t.Format("January")
}

func compareMonths(txns []SynergyTxn, a, b string, snaps []SynergyUsage, prevSnap, latestSnap time.Time) *SynComparison {
	c := &SynComparison{From: a, To: b, New: []string{}, Cancelled: []string{}, PlanChanges: []map[string]any{}, Lines: []string{}, ByCategory: []map[string]any{}}
	type agg struct{ cost, topup, support, renew, cancel, plan float64 }
	var A, B agg
	nSupportA, nSupportB := 0, 0
	cats := map[string][2]float64{}
	chains := map[string][]string{}
	chainAmt := map[string]float64{}
	seenBefore := map[string]bool{}
	for _, t := range txns {
		if t.Month < b && t.Domain != "" && isHostingKind(t.Kind) {
			seenBefore[t.Domain] = true
		}
	}
	for _, t := range txns {
		var g *agg
		var idx int
		switch t.Month {
		case a:
			g, idx = &A, 0
		case b:
			g, idx = &B, 1
		default:
			continue
		}
		if t.Kind == SynTopup {
			g.topup += t.Credit - t.Debit
			continue
		}
		g.cost += t.Amount
		cv := cats[nonEmpty(t.Category, CatOther)]
		cv[idx] += t.Amount
		cats[nonEmpty(t.Category, CatOther)] = cv
		switch t.Kind {
		case SynSupportFee:
			g.support += t.Amount
			if idx == 0 {
				nSupportA++
			} else {
				nSupportB++
			}
		case SynDomainRenewal:
			g.renew += t.Amount
		case SynCancelCredit:
			g.cancel -= t.Amount
			if idx == 1 && !containsStr(c.Cancelled, t.Domain) {
				c.Cancelled = append(c.Cancelled, t.Domain)
			}
		case SynPlanUpgrade, SynPlanDowngrade:
			g.plan += t.Amount
			if idx == 1 {
				ch := chains[t.Domain]
				if len(ch) == 0 {
					ch = append(ch, t.PlanFrom)
				}
				chains[t.Domain] = append(ch, t.PlanTo)
				chainAmt[t.Domain] += t.Amount
			}
		case SynHostingProrata:
			if idx == 1 && !seenBefore[t.Domain] && !containsStr(c.New, t.Domain) {
				c.New = append(c.New, t.Domain)
			}
		}
	}
	c.CostFrom, c.CostTo = r2(A.cost), r2(B.cost)
	c.Delta = r2(B.cost - A.cost)
	c.DeltaPct = pctChange(B.cost, A.cost)
	for _, k := range Categories {
		if v, ok := cats[k]; ok {
			c.ByCategory = append(c.ByCategory, map[string]any{"key": k, "label": CategoryLabels[k], "from": r2(v[0]), "to": r2(v[1]), "delta": r2(v[1] - v[0])})
		}
	}
	doms := []string{}
	for d := range chains {
		doms = append(doms, d)
	}
	sort.Strings(doms)
	for _, d := range doms {
		c.PlanChanges = append(c.PlanChanges, map[string]any{"domain": d, "chain": chains[d], "amount": r2(chainAmt[d])})
	}
	an, bn := monthName(a), monthName(b)
	dir := "up"
	if c.Delta < 0 {
		dir = "down"
	}
	line := fmt.Sprintf("Synergy cost %s from %s in %s to %s in %s (%s", dir, audFmt(c.CostFrom), an, audFmt(c.CostTo), bn, audSigned(c.Delta))
	if c.DeltaPct != nil {
		line += fmt.Sprintf(", %+.1f%%", *c.DeltaPct)
	}
	c.Lines = append(c.Lines, line+").")
	if len(c.New) > 0 {
		c.Lines = append(c.Lines, fmt.Sprintf("New in %s: %s.", bn, strings.Join(c.New, ", ")))
	}
	if len(c.Cancelled) > 0 {
		c.Lines = append(c.Lines, fmt.Sprintf("Cancelled in %s: %d site%s (%s) — %s credited back.", bn, len(c.Cancelled), plural(len(c.Cancelled)), strings.Join(c.Cancelled, ", "), audFmt(B.cancel)))
	}
	if len(c.PlanChanges) > 0 {
		parts := []string{}
		for _, pc := range c.PlanChanges {
			parts = append(parts, fmt.Sprintf("%s %s (%s)", pc["domain"], strings.Join(pc["chain"].([]string), " → "), audSigned(pc["amount"].(float64))))
		}
		c.Lines = append(c.Lines, fmt.Sprintf("Plan changes in %s: %s — net %s.", bn, strings.Join(parts, "; "), audSigned(r2(B.plan))))
	} else if math.Abs(A.plan) > 0.004 {
		c.Lines = append(c.Lines, fmt.Sprintf("No plan changes in %s (%s in %s).", bn, audSigned(r2(A.plan)), an))
	}
	if nSupportA+nSupportB > 0 {
		c.Lines = append(c.Lines, fmt.Sprintf("Priority support: %d ticket%s in %s (%s) vs %d in %s.", nSupportB, plural(nSupportB), bn, audFmt(B.support), nSupportA, an))
	}
	if A.renew+B.renew > 0 {
		c.Lines = append(c.Lines, fmt.Sprintf("Domain renewals: %s in %s, %s in %s.", audFmt(A.renew), an, audFmt(B.renew), bn))
	}
	c.Lines = append(c.Lines, fmt.Sprintf("Top-ups (money added, not a cost): %s in %s vs %s in %s.", audFmt(B.topup), bn, audFmt(A.topup), an))
	if !prevSnap.IsZero() && !latestSnap.IsZero() {
		var pu, lu float64
		growth := map[string]float64{}
		for _, u := range snaps {
			switch {
			case u.SnapshotDate.Equal(prevSnap):
				pu += u.UsageMB
				growth[u.Domain] -= u.UsageMB
			case u.SnapshotDate.Equal(latestSnap):
				lu += u.UsageMB
				growth[u.Domain] += u.UsageMB
			}
		}
		top, topV := "", 0.0
		have := map[string]bool{}
		for _, u := range snaps {
			if u.SnapshotDate.Equal(prevSnap) {
				have[u.Domain] = true
			}
		}
		for d, g := range growth {
			if have[d] && g > topV {
				top, topV = d, g
			}
		}
		l := fmt.Sprintf("Disk use across all sites: %s on %s → %s on %s.", mbText(pu), prevSnap.Format("2 Jan"), mbText(lu), latestSnap.Format("2 Jan"))
		if top != "" {
			l += fmt.Sprintf(" Biggest grower: %s (+%s).", top, mbText(topV))
		}
		c.Lines = append(c.Lines, l)
	}
	return c
}

func synDecisions(rep *SynergyReport, txns []SynergyTxn, plans map[string]*SynPlan, priceChanges []map[string]any, today time.Time) []SynDecision {
	out := []SynDecision{}
	add := func(d SynDecision) { out = append(out, d) }
	byPlan := func(name string) *SynPlan { return plans[planKey(name)] }
	_ = byPlan
	for _, st := range rep.Sites {
		if st.Status == "cancelled" || st.PctUsed == nil {
			continue
		}
		pct := *st.PctUsed
		switch {
		case containsStr(st.Flags, "over_quota"):
			body := fmt.Sprintf("%s used of %s (%.0f%%, %s over).", mbText(*st.UsageMB), mbText(*st.LimitMB), pct, mbText(-math.Min(0, *st.RemainingMB)))
			title := fmt.Sprintf("%s is over quota (%.0f%%) on %s", st.Domain, pct, st.Plan)
			action := "Clean up old backups / mail, or upgrade the plan"
			impact := 0.0
			if st.SuggestedPlan != "" && st.SuggestedDelta != nil {
				title += fmt.Sprintf(" — upgrade to %s (%s/mo) or clean up", st.SuggestedPlan, audSigned(*st.SuggestedDelta))
				action = fmt.Sprintf("Upgrade to %s, or clean up", st.SuggestedPlan)
				impact = *st.SuggestedDelta
			}
			if st.Billed != nil && st.SuggestedDelta != nil && *st.SuggestedDelta > 0 {
				body += fmt.Sprintf(" If you upgrade, consider passing the %s/mo on to %s.", audFmt(*st.SuggestedDelta), nonEmpty(st.CustomerName, "the client"))
			}
			add(SynDecision{Key: "over_quota:" + st.Domain, Type: "over_quota", Severity: "danger", Title: title, Body: body, Domain: st.Domain, Impact: impact, Action: action,
				Numbers: map[string]any{"usage_mb": *st.UsageMB, "limit_mb": *st.LimitMB, "pct": pct, "plan": st.Plan, "suggested_plan": st.SuggestedPlan, "delta": st.SuggestedDelta}})
		case containsStr(st.Flags, "near_full"):
			title := fmt.Sprintf("%s is at %.0f%% of %s", st.Domain, pct, st.Plan)
			body := fmt.Sprintf("%s of %s used, %s left.", mbText(*st.UsageMB), mbText(*st.LimitMB), mbText(*st.RemainingMB))
			if st.PlanChanges > 0 {
				for i := len(txns) - 1; i >= 0; i-- {
					t := txns[i]
					if t.Domain == st.Domain && t.Kind == SynPlanDowngrade {
						title = fmt.Sprintf("%s downgraded to %s and is at %.0f%%", st.Domain, t.PlanTo, pct)
						break
					}
				}
			}
			if st.DaysToFull != nil {
				title += fmt.Sprintf(" — ~%d days to full", *st.DaysToFull)
				body += fmt.Sprintf(" At the current growth (%s/month) it is full around %s.", mbText(*st.GrowthMB), today.AddDate(0, 0, *st.DaysToFull).Format("2 Jan 2006"))
			} else if st.GrowthMB != nil && *st.GrowthMB < 0 {
				body += fmt.Sprintf(" Usage fell %s since the previous report, so it is not growing now.", mbText(-*st.GrowthMB))
			}
			if st.SuggestedPlan != "" && st.SuggestedDelta != nil {
				body += fmt.Sprintf(" %s (%s/mo) gives 25%% headroom.", st.SuggestedPlan, audSigned(*st.SuggestedDelta))
			}
			add(SynDecision{Key: "near_full:" + st.Domain, Type: "near_full", Severity: "warning", Title: title, Body: body, Domain: st.Domain, Action: "Watch it, clean up or upgrade",
				Numbers: map[string]any{"pct": pct, "days_to_full": st.DaysToFull, "growth_mb_month": st.GrowthMB}})
		}
		if containsStr(st.Flags, "fast_growth") && st.GrowthMB != nil {
			title := fmt.Sprintf("%s grew %s in a month", st.Domain, mbText(*st.GrowthMB))
			body := fmt.Sprintf("Now %s of %s on %s (%.0f%%).", mbText(*st.UsageMB), mbText(*st.LimitMB), st.Plan, pct)
			if st.DaysToFull != nil && *st.DaysToFull > 0 {
				body += fmt.Sprintf(" At this rate it is full in ~%d days (%s).", *st.DaysToFull, today.AddDate(0, 0, *st.DaysToFull).Format("2 Jan"))
			}
			impact := 0.0
			if st.SuggestedPlan != "" && st.SuggestedDelta != nil && *st.SuggestedDelta > 0 {
				body += fmt.Sprintf(" Next step up that fits another month's growth: %s (%s/mo).", st.SuggestedPlan, audSigned(*st.SuggestedDelta))
				impact = *st.SuggestedDelta
			}
			body += " Check what is growing (backups, uploads, mail)."
			add(SynDecision{Key: "fast_growth:" + st.Domain, Type: "fast_growth", Severity: "warning", Title: title, Body: body, Domain: st.Domain, Impact: impact, Action: "Find out what is growing",
				Numbers: map[string]any{"growth_mb_month": *st.GrowthMB, "days_to_full": st.DaysToFull}})
		}
		if st.SuggestedDelta != nil && *st.SuggestedDelta < 0 && !containsStr(st.Flags, "over_quota") {
			add(SynDecision{Key: "downsize:" + st.Domain, Type: "downsize", Severity: "info",
				Title:  fmt.Sprintf("%s uses %.0f%% of %s — %s would save %s/mo", st.Domain, pct, st.Plan, st.SuggestedPlan, audFmt(-*st.SuggestedDelta)),
				Body:   fmt.Sprintf("%s used of %s; %s still leaves 25%% headroom. Only if disk is the reason for the bigger plan (check CPU / email limits first).", mbText(*st.UsageMB), mbText(*st.LimitMB), st.SuggestedPlan),
				Domain: st.Domain, Impact: *st.SuggestedDelta, Action: "Consider a smaller plan", Numbers: map[string]any{"pct": pct, "suggested_plan": st.SuggestedPlan, "saving": -*st.SuggestedDelta}})
		}
	}
	k := rep.KPIs
	// Plan changes this (latest) month.
	if lm := k.LatestMonth; lm != "" {
		var up, down float64
		n := 0
		doms := []string{}
		for _, t := range txns {
			if t.Month != lm || (t.Kind != SynPlanUpgrade && t.Kind != SynPlanDowngrade) {
				continue
			}
			n++
			if t.Amount > 0 {
				up += t.Amount
			} else {
				down -= t.Amount
			}
			if !containsStr(doms, t.Domain) {
				doms = append(doms, t.Domain)
			}
		}
		if n > 0 {
			add(SynDecision{Key: "plan_changes:" + lm, Type: "plan_changes", Severity: "info",
				Title:   planChangeTitle(r2(up-down), monthName(lm)),
				Body:    fmt.Sprintf("%d change%s on %s: %s in upgrades, %s back from downgrades. The new plans apply from the next monthly charge.", n, plural(n), strings.Join(doms, ", "), audFmt(r2(up)), audFmt(r2(down))),
				Domains: doms, Impact: r2(up - down), Numbers: map[string]any{"upgrades": r2(up), "credits": r2(down), "count": n}})
		}
	}
	// Unbilled / no client.
	var noClient, unbilled []string
	var unbilledCost float64
	for _, st := range rep.Sites {
		if st.Status == "cancelled" {
			continue
		}
		if containsStr(st.Flags, "no_client") {
			noClient = append(noClient, st.Domain)
		}
		if containsStr(st.Flags, "unbilled") {
			unbilled = append(unbilled, st.Domain)
			unbilledCost += st.TotalMonthly
		}
	}
	if len(unbilled) > 0 {
		t := fmt.Sprintf("%d site%s are not billed to a client — you're paying %s/mo for them", len(unbilled), plural(len(unbilled)), audFmt(r2(unbilledCost)))
		if len(unbilled) == 1 {
			t = fmt.Sprintf("1 site is not billed to a client — you're paying %s/mo for it", audFmt(r2(unbilledCost)))
		}
		body := fmt.Sprintf("No invoice line mentions %s in the last 13 months and no monthly price is set.", strings.Join(unbilled, ", "))
		if len(noClient) > 0 {
			body += fmt.Sprintf(" %d of them %s no matching client (by website, email domain or name) — link one, or mark it as your own site.", len(noClient), map[bool]string{true: "has", false: "have"}[len(noClient) == 1])
		}
		add(SynDecision{Key: "unbilled", Type: "unbilled", Severity: "warning", Title: t, Body: body, Domains: unbilled, Impact: r2(unbilledCost), Action: "Link clients and set a monthly price",
			Numbers: map[string]any{"count": len(unbilled), "no_client": len(noClient), "monthly_cost": r2(unbilledCost)}})
	}
	// Billed below cost.
	var below []string
	var belowGap float64
	for _, st := range rep.Sites {
		if containsStr(st.Flags, "below_cost") && st.Margin != nil {
			below = append(below, fmt.Sprintf("%s (%s %s vs cost %s)", st.Domain, rep.Home, trimMoney(*st.Billed), trimMoney(st.CostHome)))
			belowGap -= *st.Margin
		}
	}
	if len(below) > 0 {
		add(SynDecision{Key: "below_cost", Type: "below_cost", Severity: "danger", Title: fmt.Sprintf("%d site%s billed below cost", len(below), plural(len(below))),
			Body: "Billed per month vs what Synergy charges you: " + strings.Join(below, "; ") + ". Raise the price or move them to a cheaper plan.", Impact: r2(belowGap), Action: "Review prices",
			Numbers: map[string]any{"count": len(below), "gap_home": r2(belowGap)}})
	}
	// Balance.
	if len(rep.NegativeSpells) > 0 || (k.Balance != nil && k.RunRate > 0 && *k.Balance < k.RunRate) {
		days := []string{}
		lowest := 0.0
		for _, sp := range rep.NegativeSpells {
			t, _ := time.Parse(dayFmt, sp.From)
			days = append(days, t.Format("2 Jan"))
			if sp.Lowest < lowest {
				lowest = sp.Lowest
			}
		}
		title, body := "", ""
		sev := "warning"
		if len(days) > 0 {
			title = fmt.Sprintf("Balance went negative on %s", joinAnd(days))
			body = fmt.Sprintf("Lowest %s. ", audFmt(lowest))
		} else {
			title = "Balance won't cover the next hosting run"
		}
		if k.Balance != nil {
			body += fmt.Sprintf("Balance now %s; the monthly hosting run is about %s (charged around %s).", audFmt(*k.Balance), audFmt(k.RunRate), dayOrdinal(k.NextChargeDate))
			if k.TopupNeeded > 0 && k.NextChargeDate != "" {
				nd, _ := time.Parse(dayFmt, k.NextChargeDate)
				title += fmt.Sprintf(" — top up %s by %s to stay ahead", audFmt(k.TopupNeeded), nd.AddDate(0, 0, -1).Format("2 Jan"))
				if *k.Balance < 0 {
					sev = "danger"
				}
			}
		}
		if k.TopupsMonthlyAvg > 0 {
			body += fmt.Sprintf(" You top up %s a month on average.", audFmt(k.TopupsMonthlyAvg))
		}
		add(SynDecision{Key: "balance", Type: "balance", Severity: sev, Title: title, Body: body, Impact: k.TopupNeeded, Action: "Top up the Synergy account",
			Numbers: map[string]any{"balance": k.Balance, "run_rate": k.RunRate, "topup_needed": k.TopupNeeded, "next_charge": k.NextChargeDate, "negative_days": days, "lowest": lowest}})
	}
	// Support tickets this quarter.
	qStart := time.Date(today.Year(), ((today.Month()-1)/3)*3+1, 1, 0, 0, 0, 0, time.UTC)
	lookFrom := qStart
	if today.Sub(qStart).Hours() < 24*20 { // early in a quarter: look at the last 90 days
		lookFrom = today.AddDate(0, 0, -90)
	}
	var supN int
	var supAmt, supPrice float64
	for _, t := range txns {
		if t.Kind == SynSupportFee && !t.Date.Before(lookFrom) {
			supN++
			supAmt += t.Amount
			supPrice = t.Amount
		}
	}
	if supN > 0 {
		period := "this quarter"
		if !lookFrom.Equal(qStart) {
			period = "in the last 90 days"
		}
		add(SynDecision{Key: "support_fees", Type: "support_fees", Severity: "info", Title: fmt.Sprintf("Priority tickets cost %s — %d %s", audFmt(supPrice), supN, period),
			Body:   fmt.Sprintf("%s in priority / after-hours support fees %s. Non-urgent tickets during business hours (9–5 AEST) are free.", audFmt(r2(supAmt)), period),
			Impact: r2(supAmt), Numbers: map[string]any{"count": supN, "total": r2(supAmt), "price": supPrice}})
	}
	// Cancellations → savings.
	var cancelled []string
	var saving, credits float64
	recent := today.AddDate(0, 0, -62)
	for _, st := range rep.Sites {
		if st.Status != "cancelled" || st.StatusDate == "" {
			continue
		}
		sd, _ := time.Parse(dayFmt, st.StatusDate)
		if sd.Before(recent) {
			continue
		}
		cancelled = append(cancelled, st.Domain)
		if p := plans[planKey(st.Plan)]; p != nil && p.MonthlyPrice != nil {
			saving += *p.MonthlyPrice
		}
		for _, t := range txns {
			if t.Domain == st.Domain && t.Kind == SynCancelCredit {
				credits -= t.Amount
			}
		}
	}
	if len(cancelled) > 0 {
		add(SynDecision{Key: "cancellations", Type: "cancellations", Severity: "success",
			Title:   fmt.Sprintf("Cancelled %d service%s → saving %s/mo from the next cycle", len(cancelled), plural(len(cancelled)), audFmt(r2(saving))),
			Body:    fmt.Sprintf("%s. %s was credited back pro rata. Stop billing those clients and archive their backups.", strings.Join(cancelled, ", "), audFmt(r2(credits))),
			Domains: cancelled, Impact: -r2(saving), Numbers: map[string]any{"count": len(cancelled), "saving": r2(saving), "credits": r2(credits)}})
	}
	// New sites.
	for _, st := range rep.Sites {
		if st.Status == "new" {
			sd, _ := time.Parse(dayFmt, st.StatusDate)
			body := fmt.Sprintf("New since %s, now on %s.", sd.Format("2 Jan 2006"), nonEmpty(st.Plan, "an unknown plan"))
			if st.CustomerID == "" || st.Billed == nil {
				body += " Link it to a client and set what you bill so it shows a margin."
			}
			add(SynDecision{Key: "new:" + st.Domain, Type: "new_site", Severity: "info", Title: fmt.Sprintf("%s is new (%s, %s/mo)", st.Domain, nonEmpty(st.Plan, "plan unknown"), audFmt(st.MonthlyCost)),
				Body: body, Domain: st.Domain, Impact: st.MonthlyCost})
		}
	}
	// Price changes.
	for _, pc := range priceChanges {
		add(SynDecision{Key: fmt.Sprintf("price_change:%s:%s", pc["domain"], pc["date"]), Type: "price_change", Severity: "warning",
			Title:  fmt.Sprintf("Price change detected for %s: %s → %s/mo (%s)", pc["domain"], audFmt(pc["from"].(float64)), audFmt(pc["to"].(float64)), pc["plan"]),
			Body:   fmt.Sprintf("Charged on %s. Same plan, different monthly charge — check Synergy's price list and your client's price.", pc["date"]),
			Domain: fmt.Sprint(pc["domain"]), Impact: r2(pc["to"].(float64) - pc["from"].(float64)), Numbers: pc})
	}
	// Statement gaps.
	if len(rep.Gaps) > 0 {
		add(SynDecision{Key: "gaps", Type: "statement_gap", Severity: "warning", Title: fmt.Sprintf("%d gap%s in the statement balance", len(rep.Gaps), plural(len(rep.Gaps))),
			Body: rep.Gaps[0].Message, Action: "Upload the missing statement"})
	}
	rank := map[string]int{"danger": 0, "warning": 1, "info": 2, "success": 3}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	return out
}

func trimMoney(v float64) string { return fmt.Sprintf("%.2f", v) }

func joinAnd(l []string) string {
	switch len(l) {
	case 0:
		return ""
	case 1:
		return l[0]
	}
	return strings.Join(l[:len(l)-1], ", ") + " and " + l[len(l)-1]
}

func dayOrdinal(d string) string {
	t, err := time.Parse(dayFmt, d)
	if err != nil {
		return d
	}
	n := t.Day()
	suf := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suf = "st"
		case 2:
			suf = "nd"
		case 3:
			suf = "rd"
		}
	}
	return fmt.Sprintf("the %d%s", n, suf)
}

// synAlerts feeds the Expenses alerts / notifications.
func synAlerts(rep *SynergyReport, low float64) []Alert {
	out := []Alert{}
	link := "/expenses?tab=synergy"
	for _, st := range rep.Sites {
		if st.Status == "cancelled" || st.PctUsed == nil {
			continue
		}
		switch {
		case containsStr(st.Flags, "over_quota"):
			out = append(out, Alert{Key: "synergy_over_quota:" + st.Domain + ":" + st.SnapshotDate, Type: "synergy_over_quota", Severity: "danger",
				Title: fmt.Sprintf("%s is over its disk quota (%.0f%%)", st.Domain, *st.PctUsed),
				Body:  fmt.Sprintf("%s used of %s on %s (usage report %s).", mbText(*st.UsageMB), mbText(*st.LimitMB), st.Plan, st.SnapshotDate), Link: link, Date: st.SnapshotDate})
		case containsStr(st.Flags, "near_full"):
			b := fmt.Sprintf("%s used of %s on %s.", mbText(*st.UsageMB), mbText(*st.LimitMB), st.Plan)
			if st.DaysToFull != nil {
				b += fmt.Sprintf(" About %d days to full.", *st.DaysToFull)
			}
			out = append(out, Alert{Key: "synergy_quota75:" + st.Domain + ":" + st.SnapshotDate, Type: "synergy_quota", Severity: "warning",
				Title: fmt.Sprintf("%s is at %.0f%% of its disk quota", st.Domain, *st.PctUsed), Body: b, Link: link, Date: st.SnapshotDate})
		}
	}
	k := rep.KPIs
	if k.Balance != nil && (*k.Balance >= low || low <= 0) && k.RunwayDays != nil && *k.RunwayDays <= 14 && k.TopupNeeded > 0 {
		out = append(out, Alert{Key: "synergy_runway:" + k.NextChargeDate, Type: "synergy_runway", Severity: map[bool]string{true: "danger", false: "warning"}[*k.Balance < 0],
			Title: fmt.Sprintf("Synergy balance runs out around %s", k.RunwayDate), Body: fmt.Sprintf("Balance %s vs about %s of monthly hosting — top up %s.", audFmt(*k.Balance), audFmt(k.RunRate), audFmt(k.TopupNeeded)),
			Link: link, Amount: k.TopupNeeded, Currency: "AUD"})
	}
	if k.Unbilled > 0 {
		out = append(out, Alert{Key: fmt.Sprintf("synergy_unbilled:%s:%d", k.LatestMonth, k.Unbilled), Type: "synergy_unbilled", Severity: "warning",
			Title: fmt.Sprintf("%d hosted site%s not billed to a client", k.Unbilled, plural(k.Unbilled)),
			Body:  fmt.Sprintf("You pay Synergy %s/mo for them. Link a client and set a monthly price in Expenses → Synergy.", audFmt(k.UnbilledCost)), Link: link, Amount: k.UnbilledCost, Currency: "AUD"})
	}
	return out
}

func planChangeTitle(net float64, month string) string {
	if net < 0 {
		return fmt.Sprintf("Plan changes saved %s in %s", audFmt(-net), month)
	}
	return fmt.Sprintf("Plan changes cost %s in %s", audFmt(net), month)
}
