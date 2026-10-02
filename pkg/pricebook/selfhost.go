package pricebook

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// Self-hosted cost model: a server's monthly costs are fixed, so the cost
// per site falls as sites are added.
//
//	line (fixed)        = amount per month
//	line (your time)    = hours per month × rate per hour
//	monthly total       = Σ lines, each converted to the supplier's currency
//	cost per site       = monthly total ÷ number of sites
//	break-even vs plan  = the smallest number of sites at which the cost per
//	                      site is at or below the plan's monthly price
//	                    = ceil(monthly total ÷ plan price per month)

// DefaultCostLines is the checklist a new self-hosted supplier starts with
// (all zero until filled in).
func DefaultCostLines() []CostLine {
	return []CostLine{
		{Key: "server", Label: "Server / VPS", Kind: "fixed"},
		{Key: "panel", Label: "Control-panel licence", Kind: "fixed"},
		{Key: "backups", Label: "Backups", Kind: "fixed"},
		{Key: "ip", Label: "IP addresses", Kind: "fixed"},
		{Key: "monitoring", Label: "Monitoring", Kind: "fixed"},
		{Key: "time", Label: "Your time", Kind: "time"},
		{Key: "power", Label: "Power (on-premises)", Kind: "fixed"},
		{Key: "internet", Label: "Internet (on-premises)", Kind: "fixed"},
	}
}

// LineMonthly is a cost line's amount per month in its own currency.
func LineMonthly(l CostLine) float64 {
	if l.Kind == "time" {
		return l.Hours * l.Rate
	}
	return l.Amount
}

// CostRow is a cost line with its monthly amounts worked out.
type CostRow struct {
	CostLine
	Monthly      float64 `json:"monthly"`       // own currency
	MonthlyLocal float64 `json:"monthly_local"` // the supplier's currency
	MonthlyHome  float64 `json:"monthly_home"`
	NoRate       bool    `json:"no_rate,omitempty"`
}

// BreakEven compares the server with one wholesale plan.
type BreakEven struct {
	SupplierID   string  `json:"supplier_id"`
	SupplierName string  `json:"supplier_name"`
	ItemID       string  `json:"item_id"`
	Plan         string  `json:"plan"`
	Currency     string  `json:"currency"`
	PlanMonthly  float64 `json:"plan_monthly"`      // the plan's regular price per month, its currency
	PlanHome     float64 `json:"plan_monthly_home"` // … in the home currency
	Sites        int     `json:"sites"`             // sites needed to break even (0 = never)
	Reachable    bool    `json:"reachable"`         // within the server's capacity
	// Saving per month vs paying the plan for every site (home currency;
	// negative = the server costs more).
	SavingCurrent  float64 `json:"saving_current"`
	SavingCapacity float64 `json:"saving_capacity"`
	Text           string  `json:"text"`
}

// ChartSeries is a line of the cost-per-site chart.
type ChartSeries struct {
	Key    string    `json:"key"`
	Name   string    `json:"name"`
	Values []float64 `json:"values"`
	Kind   string    `json:"kind"` // self | plan
}

// SelfHostModel is the worked-out model of one self-hosted supplier.
type SelfHostModel struct {
	SupplierID   string    `json:"supplier_id"`
	Name         string    `json:"name"`
	Currency     string    `json:"currency"`
	Home         string    `json:"home"`
	Rows         []CostRow `json:"rows"`
	MonthlyTotal float64   `json:"monthly_total"` // supplier currency
	MonthlyHome  float64   `json:"monthly_total_home"`
	YearlyHome   float64   `json:"yearly_total_home"`
	Capacity     int       `json:"capacity"`
	CurrentSites int       `json:"current_sites"`
	DiskGB       float64   `json:"disk_gb"`
	DiskPerSite  float64   `json:"disk_per_site_gb"`
	// Cost per site per month (nil when it cannot be worked out yet).
	PerSiteCurrent      *float64      `json:"per_site_current"`
	PerSiteCapacity     *float64      `json:"per_site_capacity"`
	PerSiteCurrentHome  *float64      `json:"per_site_current_home"`
	PerSiteCapacityHome *float64      `json:"per_site_capacity_home"`
	Split               []CurShare    `json:"split"`
	BreakEven           []BreakEven   `json:"break_even"`
	ChartX              []int         `json:"chart_x"`
	Chart               []ChartSeries `json:"chart"`
	Warnings            []string      `json:"warnings"`
	// NoRate lists the currencies of cost lines that could not be converted:
	// the total is incomplete, so the server is not compared until a rate is set.
	NoRate []string `json:"no_rate"`
}

// SelfHost totals a self-hosted supplier's monthly costs (no break-even:
// see SelfHostFull).
func SelfHost(s Supplier, r Rates) *SelfHostModel {
	m := &SelfHostModel{SupplierID: s.ID, Name: s.Name, Currency: s.Currency, Home: r.Home, Capacity: s.Capacity, CurrentSites: s.CurrentSites, DiskGB: s.DiskGB,
		Rows: []CostRow{}, BreakEven: []BreakEven{}, Chart: []ChartSeries{}, ChartX: []int{}, Warnings: []string{}, Split: []CurShare{}, NoRate: []string{}}
	byCur := map[string]float64{}
	total := 0.0
	for _, l := range s.Costs {
		cur := strings.ToUpper(strings.TrimSpace(l.Currency))
		if cur == "" {
			cur = s.Currency
		}
		l.Currency = cur
		row := CostRow{CostLine: l, Monthly: RoundCur(LineMonthly(l), cur)}
		local := r.Convert(row.Monthly, cur, s.Currency)
		if row.Monthly > 0 && cur != s.Currency && (r.Rate(cur) <= 0 || r.Rate(s.Currency) <= 0) {
			row.NoRate = true
			m.NoRate = append(m.NoRate, cur)
			m.Warnings = append(m.Warnings, "No exchange rate for "+cur+" — "+l.Label+" is left out of the total.")
			local = 0
		}
		row.MonthlyLocal = RoundCur(local, s.Currency)
		row.MonthlyHome = r.ToHome(row.Monthly, cur)
		total += local
		byCur[cur] += local
		m.Rows = append(m.Rows, row)
	}
	m.MonthlyTotal = RoundCur(total, s.Currency)
	m.MonthlyHome = r.ToHome(m.MonthlyTotal, s.Currency)
	m.YearlyHome = RoundCur(m.MonthlyHome*12, r.Home)
	if total > 0 {
		curs := []string{}
		for c := range byCur {
			curs = append(curs, c)
		}
		sort.Strings(curs)
		for _, c := range curs {
			if byCur[c] > 0 {
				m.Split = append(m.Split, CurShare{Currency: c, Share: byCur[c] / total})
			}
		}
	}
	if len(m.Split) == 0 {
		m.Split = []CurShare{{Currency: s.Currency, Share: 1}}
	}
	per := func(n int) (*float64, *float64) {
		if n < 1 || m.MonthlyTotal <= 0 {
			return nil, nil
		}
		a := RoundCur(m.MonthlyTotal/float64(n), s.Currency)
		b := RoundCur(m.MonthlyHome/float64(n), r.Home)
		return &a, &b
	}
	m.PerSiteCurrent, m.PerSiteCurrentHome = per(s.CurrentSites)
	m.PerSiteCapacity, m.PerSiteCapacityHome = per(s.Capacity)
	if s.DiskGB > 0 && s.Capacity > 0 {
		m.DiskPerSite = math.Round(s.DiskGB/float64(s.Capacity)*100) / 100
	}
	return m
}

// BreakEvenSites is the smallest number of sites at which the cost per site
// (total ÷ sites) is at or below the plan's monthly price. 0 means never
// (a free plan or no costs).
func BreakEvenSites(monthlyTotal, planMonthly float64) int {
	if monthlyTotal <= 0 || planMonthly <= 0 {
		return 0
	}
	return int(math.Ceil(monthlyTotal/planMonthly - 1e-9))
}

// SelfHostFull adds the break-even against each wholesale supplier's
// cheapest plan that covers the need, and the chart of cost per site against
// the number of sites.
func SelfHostFull(s Supplier, d Data, in CompareInput) *SelfHostModel {
	m := SelfHost(s, d.Rates)
	if m.MonthlyTotal <= 0 {
		m.Warnings = append(m.Warnings, "Enter the monthly costs of the server to see the cost per site.")
	}
	if s.Capacity < 1 {
		m.Warnings = append(m.Warnings, "Enter how many sites one server holds.")
	}
	bySupplier := map[string][]Item{}
	for _, it := range d.Items {
		if it.Active {
			bySupplier[it.SupplierID] = append(bySupplier[it.SupplierID], it)
		}
	}
	// One client per plan here: a plan's own price is the fair comparison.
	one := in
	one.ShareSites = 1
	for _, w := range d.Suppliers {
		if w.Type != TypeWholesale {
			continue
		}
		var best *Part
		for _, it := range bySupplier[w.ID] {
			if it.Kind != KindHosting || PeriodMonths(it.Period) <= 0 {
				continue
			}
			if ok, shared, _ := planFit(it, one); ok {
				p := d.hostingPart(w, it, one, shared)
				if p.Rate > 0 && (best == nil || p.OngoingH < best.OngoingH) {
					best = p
				}
			}
		}
		if best == nil {
			continue
		}
		planHome := RoundCur(best.OngoingH/12, d.Home)
		be := BreakEven{SupplierID: w.ID, SupplierName: w.Name, ItemID: best.ItemID, Plan: best.Name, Currency: best.Currency,
			PlanMonthly: RoundCur(best.Ongoing/12, best.Currency), PlanHome: planHome, Sites: BreakEvenSites(m.MonthlyHome, planHome)}
		be.Reachable = be.Sites > 0 && (s.Capacity < 1 || be.Sites <= s.Capacity)
		if s.CurrentSites > 0 {
			be.SavingCurrent = RoundCur(planHome*float64(s.CurrentSites)-m.MonthlyHome, d.Home)
		}
		if s.Capacity > 0 {
			be.SavingCapacity = RoundCur(planHome*float64(s.Capacity)-m.MonthlyHome, d.Home)
		}
		switch {
		case m.MonthlyHome <= 0:
			be.Text = "Enter the server's costs to see the break-even."
		case be.Sites == 0:
			be.Text = "No break-even: " + best.Name + " has no monthly price."
		case !be.Reachable:
			be.Text = "Needs " + itoa(be.Sites) + " sites to match " + best.Name + " — more than the server holds (" + itoa(s.Capacity) + ")."
		case s.CurrentSites >= be.Sites:
			be.Text = "Already cheaper than " + best.Name + ": break-even was " + itoa(be.Sites) + " site" + plural(be.Sites) + ", you have " + itoa(s.CurrentSites) + "."
		default:
			be.Text = "Cheaper than " + best.Name + " from " + itoa(be.Sites) + " site" + plural(be.Sites) + " — " + itoa(be.Sites-s.CurrentSites) + " more than now."
		}
		m.BreakEven = append(m.BreakEven, be)
	}
	sort.SliceStable(m.BreakEven, func(i, j int) bool { return m.BreakEven[i].PlanHome < m.BreakEven[j].PlanHome })

	// Chart: cost per site (home currency) for 1 … max sites.
	if m.MonthlyHome > 0 {
		max := s.Capacity
		for _, be := range m.BreakEven {
			if be.Sites > max && be.Sites <= 400 {
				max = be.Sites
			}
		}
		if s.CurrentSites > max {
			max = s.CurrentSites
		}
		if max < 10 {
			max = 10
		}
		if max > 400 {
			max = 400
		}
		step := 1
		if max > 120 {
			step = int(math.Ceil(float64(max) / 120))
		}
		for n := 1; n <= max; n += step {
			m.ChartX = append(m.ChartX, n)
		}
		if last := m.ChartX[len(m.ChartX)-1]; last != max {
			m.ChartX = append(m.ChartX, max)
		}
		self := ChartSeries{Key: "self", Name: s.Name, Kind: "self"}
		for _, n := range m.ChartX {
			self.Values = append(self.Values, RoundCur(m.MonthlyHome/float64(n), d.Home))
		}
		m.Chart = append(m.Chart, self)
		for _, be := range m.BreakEven {
			if be.PlanHome <= 0 {
				continue
			}
			ps := ChartSeries{Key: be.SupplierID, Name: be.SupplierName + " · " + be.Plan, Kind: "plan"}
			for range m.ChartX {
				ps.Values = append(ps.Values, be.PlanHome)
			}
			m.Chart = append(m.Chart, ps)
		}
	}
	return m
}

func itoa(n int) string { return strconv.Itoa(n) }
