package insights

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
)

// Hosting unit economics from the Synergy Wholesale analytics (pkg/expenses):
// cost per site (AUD → home), what the client is billed, margin, disk use.

// SiteRow is one hosted site.
type SiteRow struct {
	Domain       string   `json:"domain"`
	Status       string   `json:"status"`
	StatusDate   string   `json:"status_date,omitempty"`
	Plan         string   `json:"plan"`
	CustomerID   string   `json:"customer_id,omitempty"`
	Client       string   `json:"client"`
	CostAUD      float64  `json:"cost_aud"`  // per month
	CostHome     float64  `json:"cost_home"` // per month
	Billed       *float64 `json:"billed"`    // per month, home
	BilledSource string   `json:"billed_source,omitempty"`
	Margin       *float64 `json:"margin"`
	MarginPct    *float64 `json:"margin_pct"`
	UsageMB      *float64 `json:"usage_mb"`
	LimitMB      *float64 `json:"limit_mb"`
	PctUsed      *float64 `json:"pct_used"`
	DaysToFull   *int     `json:"days_to_full"`
	CostPerGB    *float64 `json:"cost_per_gb"`
	RevenuePerGB *float64 `json:"revenue_per_gb"`
	Suggested    string   `json:"suggested_plan,omitempty"`
	SuggestedAUD *float64 `json:"suggested_delta_aud,omitempty"`
	Flags        []string `json:"flags"`
}

// PlanRow aggregates sites per plan.
type PlanRow struct {
	Plan     string  `json:"plan"`
	Sites    int     `json:"sites"`
	CostHome float64 `json:"cost_home"`
	Billed   float64 `json:"billed"`
	Margin   float64 `json:"margin"`
	AvgPrice float64 `json:"avg_price"` // billed per site per month, billed sites only
	Billable int     `json:"billed_sites"`
	AvgUsage float64 `json:"avg_pct_used"`
}

// Hosting is the hosting unit-economics view.
type Hosting struct {
	HasData       bool      `json:"has_data"`
	Home          string    `json:"home"`
	Active        int       `json:"active_sites"`
	Cancelled     int       `json:"cancelled_sites"`
	Billed        int       `json:"billed_sites"`
	Unbilled      int       `json:"unbilled_sites"`
	NearFull      int       `json:"near_full"`
	RevenueMonth  float64   `json:"revenue_month"` // billed per month, home
	CostMonth     float64   `json:"cost_month"`    // home
	MarginMonth   float64   `json:"margin_month"`
	MarginPct     *float64  `json:"margin_pct"`
	ARPS          float64   `json:"arps"`          // average revenue per billed site per month
	CostPerSite   float64   `json:"cost_per_site"` // per active site per month
	ProfitablePct float64   `json:"profitable_pct"`
	RunRateAUD    float64   `json:"run_rate_aud"`
	RunRateHome   float64   `json:"run_rate_home"`
	Balance       *float64  `json:"balance_aud"`
	RunwayDays    *int      `json:"runway_days"`
	RunwayDate    string    `json:"runway_date,omitempty"`
	Alerts        int       `json:"alerts"`
	Sites         []SiteRow `json:"sites"`
	Plans         []PlanRow `json:"plans"`
	Note          string    `json:"note"`
}

func (s *Service) synergy(org string) *expenses.SynergyReport {
	if s.Exp == nil {
		return nil
	}
	rep := s.Exp.SynergyReport(org, s.now(), "")
	if rep == nil || !rep.HasData {
		return nil
	}
	s.adjustBilling(org, rep)
	return rep
}

var (
	reMonthsN  = regexp.MustCompile(`(\d+)\s*(?:months|month|mo)\b`)
	reYearsN   = regexp.MustCompile(`(\d+)\s*(?:years|year|yrs|yr)\b`)
	reMonthly  = regexp.MustCompile(`\b(monthly|per month|/month|/mo)\b`)
	reQuarter  = regexp.MustCompile(`\b(quarter|quarterly)\b`)
	reAnnually = regexp.MustCompile(`\b(annual|annually|yearly|per year|/year|/yr)\b`)
)

// MonthsCovered reads the service period of a hosting / domain line (default 12).
func MonthsCovered(desc string) float64 {
	d := strings.ToLower(desc)
	if m := reMonthsN.FindStringSubmatch(d); m != nil {
		if n, _ := strconv.Atoi(m[1]); n > 0 && n <= 120 {
			return float64(n)
		}
	}
	if m := reYearsN.FindStringSubmatch(d); m != nil {
		if n, _ := strconv.Atoi(m[1]); n > 0 && n <= 10 {
			return float64(n * 12)
		}
	}
	switch {
	case reMonthly.MatchString(d):
		return 1
	case reQuarter.MatchString(d):
		return 3
	case reAnnually.MatchString(d):
		return 12
	}
	return 12
}

// adjustBilling recomputes what each site is billed from hosting / domain
// invoice lines only (the Expenses matcher also counts other lines that name
// the domain, e.g. a website build), unless a manual monthly price is set;
// then margin and the unbilled / below-cost flags follow.
func (s *Service) adjustBilling(org string, rep *expenses.SynergyReport) {
	c := s.conv(org, "")
	var rows []struct {
		ID, Number, Descr, CK string
		D                     time.Time
		Amt                   float64
	}
	s.scan(&rows, "WITH "+linesCTE(c, s.Rules(org), Opts{})+`
SELECT id, number, descr, ck, d, amt FROM fl
WHERE doc_type = 'invoice' AND line IN ('hosting','domains') AND d > (@today)::date - interval '13 months' ORDER BY d DESC, number DESC`,
		map[string]interface{}{"org": org, "today": day(s.now()).Format(dayFmt)})
	for i := range rep.Sites {
		st := &rep.Sites[i]
		if st.BilledSource == "manual" {
			continue
		}
		dom := strings.ToLower(st.Domain)
		inv, num, when := "", "", ""
		var monthly float64
		for _, r := range rows {
			if !strings.Contains(strings.ToLower(r.Descr), dom) {
				continue
			}
			if inv == "" {
				inv, num, when = r.ID, r.Number, r.D.Format(dayFmt)
			}
			if r.ID == inv {
				monthly += r.Amt / MonthsCovered(r.Descr)
			}
		}
		flags := []string{}
		for _, f := range st.Flags {
			if f != "unbilled" && f != "below_cost" {
				flags = append(flags, f)
			}
		}
		st.Billed, st.BilledSource, st.BilledInvoice, st.BilledDate, st.Margin, st.MarginPct = nil, "", "", "", nil, nil
		if inv != "" && monthly > 0 {
			v := r2(monthly)
			st.Billed, st.BilledSource, st.BilledInvoice, st.BilledDate = &v, "invoice", num, when
		}
		if st.Status != "cancelled" && !st.NoClient && st.Billed == nil {
			flags = append(flags, "unbilled")
		}
		if st.Billed != nil && st.Status != "cancelled" {
			m := r2(*st.Billed - st.CostHome)
			st.Margin = &m
			p := r1(m / *st.Billed * 100)
			st.MarginPct = &p
			if m < 0 {
				flags = append(flags, "below_cost")
			}
		}
		st.Flags = flags
	}
}

func siteRow(st expenses.SynSite) SiteRow {
	r := SiteRow{Domain: st.Domain, Status: st.Status, StatusDate: st.StatusDate, Plan: st.Plan, CustomerID: st.CustomerID, Client: st.CustomerName,
		CostAUD: r2(st.TotalMonthly), CostHome: r2(st.CostHome), Billed: st.Billed, BilledSource: st.BilledSource, Margin: st.Margin, MarginPct: st.MarginPct,
		UsageMB: st.UsageMB, LimitMB: st.LimitMB, PctUsed: st.PctUsed, DaysToFull: st.DaysToFull, Suggested: st.SuggestedPlan, SuggestedAUD: st.SuggestedDelta, Flags: st.Flags}
	if r.Flags == nil {
		r.Flags = []string{}
	}
	if st.UsageMB != nil && *st.UsageMB > 0 {
		gb := *st.UsageMB / 1024
		if gb >= 0.05 {
			r.CostPerGB = fptr(r2(st.CostHome / gb))
			if st.Billed != nil {
				r.RevenuePerGB = fptr(r2(*st.Billed / gb))
			}
		}
	}
	return r
}

func hasFlag(fl []string, f string) bool {
	for _, x := range fl {
		if x == f {
			return true
		}
	}
	return false
}

// HostingUnits builds per-site and per-plan unit economics.
func (s *Service) HostingUnits(org string) *Hosting {
	out := &Hosting{Home: s.Home(org), Sites: []SiteRow{}, Plans: []PlanRow{},
		Note: "Costs are Synergy Wholesale charges per month (hosting plan + domain renewals averaged over 12 months), converted from AUD. Billed = what the client pays per month for the site: the manual price set in Expenses → Synergy, else the latest invoice's hosting / domain lines naming the domain, spread over the months they cover (12 unless the line says otherwise)."}
	rep := s.synergy(org)
	if rep == nil {
		return out
	}
	out.HasData = true
	k := rep.KPIs
	out.RunRateAUD, out.RunRateHome, out.Balance, out.RunwayDays, out.RunwayDate = r2(k.RunRate), r2(k.RunRateHome), k.Balance, k.RunwayDays, k.RunwayDate
	out.Alerts = len(rep.Alerts)
	plans := map[string]*PlanRow{}
	profitable := 0
	usage := map[string][]float64{}
	for _, st := range rep.Sites {
		row := siteRow(st)
		out.Sites = append(out.Sites, row)
		if st.Status == "cancelled" {
			out.Cancelled++
			continue
		}
		out.Active++
		out.CostMonth += st.CostHome
		p := plans[st.Plan]
		if p == nil {
			p = &PlanRow{Plan: st.Plan}
			plans[st.Plan] = p
		}
		p.Sites++
		p.CostHome += st.CostHome
		if st.PctUsed != nil {
			usage[st.Plan] = append(usage[st.Plan], *st.PctUsed)
		}
		if hasFlag(st.Flags, "near_full") || hasFlag(st.Flags, "over_quota") {
			out.NearFull++
		}
		if st.Billed != nil && *st.Billed > 0 {
			out.Billed++
			out.RevenueMonth += *st.Billed
			p.Billed += *st.Billed
			p.Billable++
			if st.Margin != nil && *st.Margin > 0 {
				profitable++
			}
		} else if !st.NoClient {
			out.Unbilled++
		}
	}
	out.RevenueMonth, out.CostMonth = r2(out.RevenueMonth), r2(out.CostMonth)
	out.MarginMonth = r2(out.RevenueMonth - out.CostMonth)
	if out.RevenueMonth > 0 {
		out.MarginPct = fptr(r1(out.MarginMonth / out.RevenueMonth * 100))
	}
	if out.Billed > 0 {
		out.ARPS = r2(out.RevenueMonth / float64(out.Billed))
		out.ProfitablePct = r1(float64(profitable) / float64(out.Billed) * 100)
	}
	if out.Active > 0 {
		out.CostPerSite = r2(out.CostMonth / float64(out.Active))
	}
	for _, p := range plans {
		p.CostHome, p.Billed = r2(p.CostHome), r2(p.Billed)
		p.Margin = r2(p.Billed - p.CostHome)
		if p.Billable > 0 {
			p.AvgPrice = r2(p.Billed / float64(p.Billable))
		}
		if u := usage[p.Plan]; len(u) > 0 {
			p.AvgUsage = r1(mean(u))
		}
		out.Plans = append(out.Plans, *p)
	}
	sort.SliceStable(out.Plans, func(i, j int) bool { return out.Plans[i].Sites > out.Plans[j].Sites })
	sort.SliceStable(out.Sites, func(i, j int) bool {
		a, b := out.Sites[i], out.Sites[j]
		if (a.Status == "cancelled") != (b.Status == "cancelled") {
			return b.Status == "cancelled"
		}
		am, bm := math.Inf(1), math.Inf(1)
		if a.Margin != nil {
			am = *a.Margin
		}
		if b.Margin != nil {
			bm = *b.Margin
		}
		return am < bm
	})
	return out
}
