package expenses

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// HasSynergyData reports whether statements or usage reports were uploaded.
func (s *Service) HasSynergyData(org string) bool {
	var n int64
	s.DB.Model(&SynergyTxn{}).Where("organization_id = ?", org).Limit(1).Count(&n)
	if n > 0 {
		return true
	}
	s.DB.Model(&SynergyUsage{}).Where("organization_id = ?", org).Limit(1).Count(&n)
	return n > 0
}

func (s *Service) synClients(org string) []SynClient {
	var rows []struct{ ID, FirstName, LastName, Email, CompanyName, Website string }
	s.DB.Raw(`SELECT id, COALESCE(first_name,'') AS first_name, COALESCE(last_name,'') AS last_name, COALESCE(email,'') AS email,
		COALESCE(company_name,'') AS company_name, COALESCE(website,'') AS website FROM customers WHERE organization_id = ? AND deleted_at IS NULL`, org).Scan(&rows)
	out := make([]SynClient, 0, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(r.FirstName + " " + r.LastName)
		if name == "" {
			name = r.CompanyName
		}
		if name == "" {
			name = r.Email
		}
		out = append(out, SynClient{ID: r.ID, Name: name, Email: r.Email, Company: r.CompanyName, Website: r.Website})
	}
	return out
}

// synBilled loads invoice lines (issued in the last ~13 months, or all when
// all is set) that mention one of the domains.
func (s *Service) synBilled(org string, domains []string, since *time.Time) []SynBilledLine {
	out := []SynBilledLine{}
	if len(domains) == 0 {
		return out
	}
	home := s.Home(org)
	conds := []string{}
	args := []any{home, org}
	for _, d := range domains {
		conds = append(conds, "LOWER(ii.description) LIKE ?")
		args = append(args, "%"+strings.ToLower(d)+"%")
	}
	q := `SELECT i.id AS invoice_id, COALESCE(i.number,'') AS number, i.issue_date AS date, COALESCE(i.customer_id,'') AS customer_id,
		UPPER(COALESCE(NULLIF(i.currency,''), ?)) AS currency, COALESCE(i.total,0) AS total, COALESCE(i.total_base,0) AS total_base,
		COALESCE(ii.description,'') AS description, COALESCE(ii.quantity,1) AS quantity, COALESCE(ii.unit,'') AS unit,
		COALESCE(ii.line_subtotal,0) AS line_subtotal, COALESCE(ii.line_discount,0) AS line_discount
		FROM billing_invoice_items ii JOIN billing_invoices i ON i.id = ii.invoice_id
		WHERE i.organization_id = ? AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status NOT IN ('draft','void') AND (` + strings.Join(conds, " OR ") + `)`
	if since != nil {
		q += " AND i.issue_date >= ?"
		args = append(args, *since)
	}
	var rows []struct {
		InvoiceID, Number, CustomerID, Currency, Description, Unit string
		Date                                                       time.Time
		Total, TotalBase, Quantity, LineSubtotal, LineDiscount     float64
	}
	if err := s.DB.Raw(q, args...).Scan(&rows).Error; err != nil {
		return out
	}
	toHome := s.toHome(org)
	for _, r := range rows {
		net := r.LineSubtotal - r.LineDiscount
		var nh float64
		switch {
		case r.Currency == home:
			nh = net
		case r.TotalBase > 0 && r.Total > 0:
			nh = net * r.TotalBase / r.Total
		default:
			nh = toHome(net, r.Currency)
		}
		months := monthsCovered(r.Description, r.Unit, r.Quantity)
		out = append(out, SynBilledLine{InvoiceID: r.InvoiceID, Number: r.Number, Date: r.Date, CustomerID: r.CustomerID, Description: r.Description, Currency: r.Currency,
			Net: r2(net), NetHome: r2(nh), Months: months, MonthlyHome: r2(nh / months)})
	}
	return out
}

// SynergyReport builds the Synergy analytics for an organisation.
func (s *Service) SynergyReport(org string, today time.Time, basis string) *SynergyReport {
	in := SynInput{Today: today, Basis: basis, Home: s.Home(org), ToHome: s.toHome(org), LowBalance: 20}
	s.DB.Where("organization_id = ?", org).Order("date, seq").Find(&in.Txns)
	s.DB.Where("organization_id = ?", org).Order("snapshot_date, domain").Find(&in.Snaps)
	s.DB.Where("organization_id = ?", org).Find(&in.Sites)
	s.DB.Where("organization_id = ?", org).Find(&in.Plans)
	if c := s.connection(org, "synergy"); c != nil {
		in.Balance, in.BalanceAt, in.BalanceSource = c.Balance, c.BalanceAt, c.BalanceSource
		if c.Settings.LowBalance > 0 {
			in.LowBalance = c.Settings.LowBalance
		}
	}
	if len(in.Txns) > 0 || len(in.Snaps) > 0 {
		in.Clients = s.synClients(org)
		doms := map[string]bool{}
		for _, t := range in.Txns {
			if t.Domain != "" && isHostingKind(t.Kind) {
				doms[t.Domain] = true
			}
		}
		for _, u := range in.Snaps {
			doms[u.Domain] = true
		}
		list := []string{}
		for d := range doms {
			list = append(list, d)
		}
		since := Day(today).AddDate(0, -13, 0)
		in.Billed = s.synBilled(org, list, &since)
	}
	rep := BuildSynergyReport(in)
	var ups []SynergyUpload
	s.DB.Where("organization_id = ?", org).Order("created_at DESC").Limit(20).Find(&ups)
	rep.Uploads = ups
	return rep
}

// SynergySiteDetail is the drawer of one site.
type SynergySiteDetail struct {
	Site         *SynSite        `json:"site"`
	Transactions []SynergyTxn    `json:"transactions"`
	Snapshots    []SynergyUsage  `json:"snapshots"`
	Invoices     []SynBilledLine `json:"invoices"`
	Decisions    []SynDecision   `json:"decisions"`
	Currency     string          `json:"currency"`
	Home         string          `json:"home"`
}

// SynergySite returns one site with its history.
func (s *Service) SynergySiteDetail(org, domain string, today time.Time) (*SynergySiteDetail, error) {
	domain = cleanDomain(domain)
	rep := s.SynergyReport(org, today, "charge")
	out := &SynergySiteDetail{Transactions: []SynergyTxn{}, Snapshots: []SynergyUsage{}, Invoices: []SynBilledLine{}, Decisions: []SynDecision{}, Currency: "AUD", Home: rep.Home}
	for i := range rep.Sites {
		if rep.Sites[i].Domain == domain {
			out.Site = &rep.Sites[i]
		}
	}
	if out.Site == nil {
		return nil, ErrNotFound
	}
	for _, d := range rep.Decisions {
		if d.Domain == domain || containsStr(d.Domains, domain) {
			out.Decisions = append(out.Decisions, d)
		}
	}
	s.DB.Where("organization_id = ? AND domain = ?", org, domain).Order("date DESC, seq DESC").Find(&out.Transactions)
	s.DB.Where("organization_id = ? AND domain = ?", org, domain).Order("snapshot_date").Find(&out.Snapshots)
	out.Invoices = s.synBilled(org, []string{domain}, nil)
	sort.SliceStable(out.Invoices, func(i, j int) bool { return out.Invoices[i].Date.After(out.Invoices[j].Date) })
	return out, nil
}

// SynergySiteInput sets the client / price of a site.
type SynergySiteInput struct {
	CustomerID    string   `json:"customer_id"` // "" = automatic match
	NoClient      bool     `json:"no_client"`
	Price         *float64 `json:"price"` // null = from invoices
	PriceCurrency string   `json:"price_currency"`
	Notes         string   `json:"notes"`
}

// SaveSynergySite stores the user's choices for a site.
func (s *Service) SaveSynergySite(org, domain string, in SynergySiteInput) (*SynergySite, error) {
	domain = cleanDomain(domain)
	if domain == "" || !strings.Contains(domain, ".") {
		return nil, invalid("enter a domain")
	}
	if in.CustomerID != "" && !s.customerExists(org, in.CustomerID) {
		return nil, invalid("customer not found")
	}
	if in.Price != nil && (*in.Price < 0 || *in.Price > 1e7) {
		return nil, invalid("the monthly price must be zero or more")
	}
	in.PriceCurrency = strings.ToUpper(strings.TrimSpace(in.PriceCurrency))
	if in.Price != nil {
		if in.PriceCurrency == "" {
			in.PriceCurrency = s.Home(org)
		}
		if !currency.IsSupported(in.PriceCurrency) {
			return nil, invalid("choose a supported currency")
		}
		v := r2(*in.Price)
		in.Price = &v
	}
	if in.NoClient {
		in.CustomerID = ""
	}
	var st SynergySite
	if s.DB.Where("organization_id = ? AND domain = ?", org, domain).First(&st).Error != nil {
		st = SynergySite{ID: uuid.NewString(), OrganizationID: org, Domain: domain}
	}
	st.CustomerID, st.NoClient, st.Price, st.PriceCurrency, st.Notes, st.UpdatedAt = in.CustomerID, in.NoClient, in.Price, in.PriceCurrency, clip(strings.TrimSpace(in.Notes), 2000), s.now()
	if in.Price == nil {
		st.PriceCurrency = ""
	}
	return &st, s.DB.Save(&st).Error
}

// SynergyPlanInput sets a plan's price / limit (null = learned).
type SynergyPlanInput struct {
	Name         string   `json:"name"`
	MonthlyPrice *float64 `json:"monthly_price"`
	LimitMB      *float64 `json:"limit_mb"`
}

// SaveSynergyPlan stores a plan override.
func (s *Service) SaveSynergyPlan(org string, in SynergyPlanInput) (*SynergyPlan, error) {
	name := strings.Join(strings.Fields(in.Name), " ")
	if name == "" {
		return nil, invalid("enter the plan name")
	}
	if (in.MonthlyPrice != nil && (*in.MonthlyPrice < 0 || *in.MonthlyPrice > 100000)) || (in.LimitMB != nil && (*in.LimitMB <= 0 || *in.LimitMB > 1e8)) {
		return nil, invalid("price must be zero or more and the disk limit above zero")
	}
	k := planKey(name)
	var p SynergyPlan
	found := s.DB.Where("organization_id = ? AND key = ?", org, k).First(&p).Error == nil
	if in.MonthlyPrice == nil && in.LimitMB == nil {
		// Back to the learned values.
		if found {
			if err := s.DB.Delete(&SynergyPlan{}, "id = ?", p.ID).Error; err != nil {
				return nil, err
			}
		}
		return &SynergyPlan{Key: k, Name: name}, nil
	}
	if !found {
		p = SynergyPlan{ID: uuid.NewString(), OrganizationID: org, Key: k}
	}
	p.Name, p.MonthlyPrice, p.LimitMB, p.UpdatedAt = clip(name, 118), in.MonthlyPrice, in.LimitMB, s.now()
	return &p, s.DB.Save(&p).Error
}

// SynergySitesCSV exports the per-site table.
func (s *Service) SynergySitesCSV(org string, today time.Time) []byte {
	rep := s.SynergyReport(org, today, "charge")
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"Domain", "Status", "Client", "Plan", "Monthly cost (AUD)", "Domain renewals per month (AUD)", "Cost per month (" + rep.Home + ")",
		"Billed per month (" + rep.Home + ")", "Margin (" + rep.Home + ")", "Margin %", "Disk used (MB)", "Disk limit (MB)", "% used", "Growth MB/month", "Days to full", "Suggested plan", "Flags"})
	f := func(p *float64) string {
		if p == nil {
			return ""
		}
		return fmt.Sprintf("%.2f", *p)
	}
	for _, st := range rep.Sites {
		dtf := ""
		if st.DaysToFull != nil {
			dtf = fmt.Sprint(*st.DaysToFull)
		}
		_ = w.Write([]string{st.Domain, st.Status, st.CustomerName, st.Plan, fmt.Sprintf("%.2f", st.MonthlyCost), fmt.Sprintf("%.2f", st.DomainMonthly), fmt.Sprintf("%.2f", st.CostHome),
			f(st.Billed), f(st.Margin), f(st.MarginPct), f(st.UsageMB), f(st.LimitMB), f(st.PctUsed), f(st.GrowthMB), dtf, st.SuggestedPlan, strings.Join(st.Flags, " ")})
	}
	w.Flush()
	return b.Bytes()
}

// synergySummary is the compact block for the main dashboard card / AI.
func synergySummary(rep *SynergyReport, alerts int) map[string]any {
	k := rep.KPIs
	return map[string]any{"has_data": rep.HasData, "currency": "AUD", "run_rate": k.RunRate, "run_rate_home": k.RunRateHome, "active_sites": k.ActiveSites,
		"new_sites": k.NewSites, "cancelled_sites": k.CancelledSites, "balance": k.Balance, "balance_at": k.BalanceAt, "runway_days": k.RunwayDays, "runway_date": k.RunwayDate,
		"topup_needed": k.TopupNeeded, "over_quota": k.OverQuota, "near_full": k.NearFull, "unbilled": k.Unbilled, "unbilled_cost": k.UnbilledCost,
		"margin_monthly": k.MarginMonthly, "billed_monthly": k.BilledMonthly, "alerts": alerts, "decisions": len(rep.Decisions), "last_month": k.LastMonth, "this_month": k.ThisMonth,
		"latest_month": k.LatestMonth}
}
