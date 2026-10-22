package subscriptions

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/pricebook"
)

// Synergy Wholesale hosted sites → subscriptions. Each hosted site (Expenses
// → Synergy: statements, usage reports and the API) with the customer it is
// matched to becomes a subscription in one click — or all matched sites at
// once — priced from:
//
//  1. the catalogue service this Synergy plan is sold as (remembered from the
//     last site on that plan, or a service whose name mentions the plan),
//  2. what the client is billed for the site today (invoices / the price set
//     on the site), or
//  3. the retail helper: the site's Synergy cost with the target margin,
//     tax and friendly rounding (pkg/pricebook).

// SiteSub is the subscription that bills a site.
type SiteSub struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	Status string `json:"status"`
}

// Suggestion is how a site would be billed.
type Suggestion struct {
	ServiceID   string  `json:"service_id,omitempty"`
	ServiceName string  `json:"service_name,omitempty"`
	Description string  `json:"description"`
	Period      string  `json:"period"`
	Price       float64 `json:"price"` // per period, as invoices are written (incl. tax when prices include tax)
	Currency    string  `json:"currency"`
	Source      string  `json:"source"` // mapped | catalogue | billed | retail
	Note        string  `json:"note,omitempty"`
}

// SiteRow is one hosted site.
type SiteRow struct {
	Domain       string      `json:"domain"`
	Plan         string      `json:"plan"`
	Status       string      `json:"status"`
	CostHome     float64     `json:"cost_home"` // per month, home currency
	Home         string      `json:"home"`
	CustomerID   string      `json:"customer_id"`
	CustomerName string      `json:"customer_name"`
	ClientMatch  string      `json:"client_match"`
	NoClient     bool        `json:"no_client"`
	Subscription *SiteSub    `json:"subscription"`
	Suggestion   *Suggestion `json:"suggestion"`
	Start        string      `json:"start"`
	Ready        bool        `json:"ready"`
	Reason       string      `json:"reason,omitempty"`
}

func planKey(p string) string { return strings.ToLower(strings.Join(strings.Fields(p), " ")) }

// covered maps domains to the live subscription that bills them.
func (s *Service) covered(org string) map[string]SiteSub {
	out := map[string]SiteSub{}
	var subs []Subscription
	s.DB.Select("id, number, status, site_domain, domain, domains").Where("organization_id = ? AND status <> ?", org, StatusCancelled).Find(&subs)
	for _, x := range subs {
		for _, d := range append([]string{x.SiteDomain}, x.Domains...) {
			if d != "" {
				out[strings.ToLower(d)] = SiteSub{ID: x.ID, Number: x.Number, Status: x.Status}
			}
		}
	}
	return out
}

// nextRenewals are the hosting services' next renewal dates at Synergy.
func (s *Service) nextRenewals(org string) map[string]time.Time {
	out := map[string]time.Time{}
	if !hasTable(s.DB, "expense_renewals") {
		return out
	}
	var rows []struct {
		Name      string
		ExpiresOn *time.Time
	}
	s.DB.Raw(`SELECT name, expires_on FROM expense_renewals WHERE organization_id = ? AND source = ? AND kind = 'hosting' AND COALESCE(active, true)`, org, expenses.SourceSynergy).Scan(&rows)
	for _, r := range rows {
		if r.ExpiresOn != nil {
			out[strings.ToLower(r.Name)] = Day(*r.ExpiresOn)
		}
	}
	return out
}

func (s *Service) planMaps(org string) map[string]PlanMap {
	out := map[string]PlanMap{}
	var rows []PlanMap
	s.DB.Where("organization_id = ?", org).Find(&rows)
	for _, r := range rows {
		out[r.PlanKey] = r
	}
	return out
}

// suggest prices a site for a customer currency (and period; "" = the
// service's own or monthly).
func (s *Service) suggest(org string, plan string, costHome float64, billedHome *float64, manual *float64, manualCur string, cur, period string,
	cat map[string]CatalogService, maps map[string]PlanMap, st Settings, tax TaxDefaults, home string) *Suggestion {
	var svc *CatalogService
	src := ""
	if m, ok := maps[planKey(plan)]; ok && plan != "" {
		if c, ok := cat[m.ServiceID]; ok {
			svc, src = &c, "mapped"
			if period == "" {
				period = m.Period
			}
		}
	}
	if svc == nil && plan != "" {
		pk := planKey(plan)
		ids := []string{}
		for id, c := range cat {
			if strings.Contains(planKey(c.Name), pk) || strings.Contains(planKey(c.Description), pk) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			// Prefer one whose period matches.
			for _, id := range ids {
				if c := cat[id]; period == "" || c.Period == period || c.Period == "" {
					svc, src = &c, "catalogue"
					break
				}
			}
		}
	}
	if period == "" {
		period = PeriodMonthly
		if svc != nil && svc.Period != "" {
			period = svc.Period
		}
	}
	months := float64(PeriodMonths(period))
	sg := &Suggestion{Period: period, Currency: cur, Description: hostingLabel(plan)}
	if svc != nil {
		p := PerPeriod(svc.Price, svc.Period, period)
		switch {
		case svc.PriceIncludesTax && !tax.PricesIncludeTax:
			p = p / (1 + tax.Rate/100)
		case !svc.PriceIncludesTax && tax.PricesIncludeTax:
			p = p * (1 + tax.Rate/100)
		}
		if v, ok := s.convert(org, p, svc.Currency, cur); ok {
			sg.ServiceID, sg.ServiceName, sg.Description, sg.Price, sg.Source = svc.ID, svc.Name, svc.Label, RoundCur(v, cur), src
			sg.Note = "From the catalogue: " + svc.Name + " (" + money(svc.Price, svc.Currency) + ")"
			return sg
		}
	}
	if manual != nil && *manual > 0 {
		if v, ok := s.convert(org, *manual*months, firstNonEmpty(manualCur, home), cur); ok {
			sg.Price, sg.Source, sg.Note = RoundCur(v, cur), "billed", "The price set on the site in Expenses → Synergy ("+money(*manual, firstNonEmpty(manualCur, home))+"/month)"
			return sg
		}
	}
	if billedHome != nil && *billedHome > 0 {
		if v, ok := s.convert(org, *billedHome*months, home, cur); ok {
			sg.Price, sg.Source, sg.Note = RoundCur(v, cur), "billed", "What the client is billed for it today ("+money(*billedHome, home)+"/month on invoices)"
			return sg
		}
	}
	if costHome > 0 {
		c, ok := s.convert(org, costHome*months, home, cur)
		if ok {
			pp, err := pricebook.PriceFor(c, "margin", st.TargetMargin, pricebook.RetailMarket{Currency: cur, TaxRate: tax.Rate, PricesIncludeTax: tax.PricesIncludeTax, Rounding: "friendly"})
			if err == nil && pp.Price > 0 {
				sg.Price, sg.Source = pp.Price, "retail"
				sg.Note = fmt.Sprintf("Retail helper: %.0f%% margin on Synergy's cost (%s per %s)", st.TargetMargin, money(RoundCur(c, cur), cur), periodNoun(period))
				return sg
			}
		}
	}
	return sg
}

func periodNoun(p string) string {
	return map[string]string{PeriodMonthly: "month", PeriodQuarterly: "quarter", PeriodYearly: "year"}[p]
}

func hostingLabel(plan string) string {
	if strings.TrimSpace(plan) == "" {
		return "Web hosting"
	}
	return "Web hosting (" + strings.TrimSpace(plan) + ")"
}

// SynergySites lists the hosted sites with their client, subscription and
// suggested price.
func (s *Service) SynergySites(org string) []SiteRow {
	out := []SiteRow{}
	sites := s.synergySites(org)
	if len(sites) == 0 {
		return out
	}
	home := s.home(org)
	st := s.SettingsFor(org)
	tax := s.taxDefaults(org)
	cat := s.catalog(org, nil)
	maps := s.planMaps(org)
	cov := s.covered(org)
	renew := s.nextRenewals(org)
	today := s.Today(org)
	ids := []string{}
	for _, x := range sites {
		if x.CustomerID != "" {
			ids = append(ids, x.CustomerID)
		}
	}
	cust := s.customers(org, ids)
	for _, x := range sites {
		r := SiteRow{Domain: strings.ToLower(x.Domain), Plan: x.Plan, Status: x.Status, CostHome: RoundCur(x.CostHome, home), Home: home, CustomerID: x.CustomerID,
			CustomerName: x.CustomerName, ClientMatch: x.ClientMatch, NoClient: x.NoClient}
		if c, ok := cov[r.Domain]; ok {
			cc := c
			r.Subscription = &cc
		}
		cur := home
		if c, ok := cust[x.CustomerID]; ok {
			cur, _ = currency.ForCustomer(c.Currency, c.CountryCode, c.Country, home)
			r.CustomerName = c.Name // the business, as on its invoices and alerts
		} else {
			r.CustomerID = ""
		}
		r.Suggestion = s.suggest(org, x.Plan, x.CostHome, x.Billed, x.Price, x.PriceCurrency, cur, "", cat, maps, st, tax, home)
		start := StartOfMonth(today).AddDate(0, 1, 0)
		if d, ok := renew[r.Domain]; ok && !d.Before(today) {
			start = d
		}
		r.Start = DayStr(start)
		switch {
		case r.Subscription != nil:
			r.Reason = "Billed by " + r.Subscription.Number
		case x.Status == "cancelled":
			r.Reason = "Cancelled at Synergy"
		case x.NoClient:
			r.Reason = "Marked as not a client site"
		case r.CustomerID == "":
			r.Reason = "No client matched — choose one"
		case r.Suggestion == nil || r.Suggestion.Price <= 0:
			r.Reason = "No price — choose a catalogue service or type one"
		default:
			r.Ready = true
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Subscription == nil) != (out[j].Subscription == nil) {
			return out[i].Subscription == nil
		}
		return out[i].Domain < out[j].Domain
	})
	return out
}

// FromSynergyInput creates a subscription for a hosted site.
type FromSynergyInput struct {
	Domain      string   `json:"domain"`
	CustomerID  string   `json:"customer_id"`
	ServiceID   string   `json:"service_id"` // "" = the suggested one, "none" = no catalogue service
	Description string   `json:"description"`
	Price       *float64 `json:"price"`
	Period      string   `json:"period"`
	StartDate   string   `json:"start_date"`
	AutoSend    *bool    `json:"auto_send"`
	// SaveService adds the price to the catalogue as a service for this
	// Synergy plan (when no service was chosen) and remembers it.
	SaveService bool `json:"save_service"`
}

// FromSynergy creates the subscription for one hosted site.
func (s *Service) FromSynergy(org, user string, in FromSynergyInput) (*Subscription, error) {
	dom := cleanDomain(in.Domain)
	var row *SiteRow
	for _, r := range s.SynergySites(org) {
		if r.Domain == dom {
			rr := r
			row = &rr
			break
		}
	}
	if row == nil {
		return nil, invalid("%s is not one of your Synergy Wholesale sites (Expenses → Synergy)", firstNonEmpty(dom, "the site"))
	}
	if row.Subscription != nil {
		return nil, invalid("%s is already billed by %s", dom, row.Subscription.Number)
	}
	cid := firstNonEmpty(in.CustomerID, row.CustomerID)
	cu, ok := s.customer(org, cid)
	if !ok {
		return nil, invalid("choose the client %s belongs to", dom)
	}
	home := s.home(org)
	cur, _ := currency.ForCustomer(cu.Currency, cu.CountryCode, cu.Country, home)
	st := s.SettingsFor(org)
	tax := s.taxDefaults(org)
	cat := s.catalog(org, nil)
	maps := s.planMaps(org)
	period := strings.TrimSpace(in.Period)
	if period != "" && PeriodMonths(period) == 0 {
		return nil, invalid("choose a billing period: monthly, quarterly or yearly")
	}
	var site struct {
		billed, manual *float64
		manualCur      string
	}
	for _, x := range s.synergySites(org) {
		if strings.EqualFold(x.Domain, dom) {
			site.billed, site.manual, site.manualCur = x.Billed, x.Price, x.PriceCurrency
		}
	}
	sg := s.suggest(org, row.Plan, row.CostHome, site.billed, site.manual, site.manualCur, cur, period, cat, maps, st, tax, home)
	if period == "" {
		period = sg.Period
	}
	svcID := strings.TrimSpace(in.ServiceID)
	switch {
	case svcID == "none": // typed item, no catalogue service
		svcID = ""
	case svcID == "" && sg.ServiceID != "":
		svcID = sg.ServiceID
	}
	price := sg.Price
	desc := firstNonEmpty(in.Description, sg.Description)
	if svcID != "" {
		svc, ok := cat[svcID]
		if !ok {
			return nil, invalid("the catalogue service was not found")
		}
		if svcID != sg.ServiceID && in.Period == "" && svc.Period != "" {
			period = svc.Period // sold per the chosen service's period
		}
		if svcID != sg.ServiceID {
			p := PerPeriod(svc.Price, svc.Period, period)
			if v, ok := s.convert(org, p, svc.Currency, cur); ok {
				price = RoundCur(v, cur)
			}
		}
		desc = firstNonEmpty(in.Description, svc.Label)
	}
	if in.Price != nil {
		price = RoundCur(*in.Price, cur)
	}
	if price <= 0 {
		return nil, invalid("enter the price for %s", dom)
	}
	if svcID == "" && in.SaveService {
		name := fmt.Sprintf("%s — %s", desc, strings.ToLower(PeriodLabel(period)))
		id, err := s.saveCatalogService(org, name, "Synergy Wholesale "+row.Plan+" hosting, sold "+strings.ToLower(PeriodLabel(period)), price, cur, tax.PricesIncludeTax)
		if err != nil {
			return nil, err
		}
		svcID = id
	}
	if svcID != "" && row.Plan != "" {
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&PlanMap{OrganizationID: org, PlanKey: planKey(row.Plan), Plan: row.Plan, ServiceID: svcID, Period: period, UpdatedAt: s.now()})
	}
	start := firstNonEmpty(in.StartDate, row.Start)
	no := false
	sub, err := s.Create(org, user, SubInput{CustomerID: cu.ID, Period: period, StartDate: start, AutoSend: in.AutoSend, BillPastPeriods: &no,
		Lines:      []LineInput{{ServiceID: svcID, Kind: KindHosting, Description: desc, UnitPrice: &price}},
		SiteDomain: dom, Supplier: SupplierSynergy, SupplierPlan: row.Plan, Source: "synergy"})
	if err != nil {
		return nil, err
	}
	forgetSites(org)
	return sub, nil
}

// saveCatalogService adds (or updates, by name) a catalogue service.
func (s *Service) saveCatalogService(org, name, desc string, price float64, cur string, incTax bool) (string, error) {
	if !hasTable(s.DB, "services") {
		return "", invalid("the services catalogue is not available")
	}
	name = clip(name, 250)
	cat := "Hosting & domains"
	var row struct{ ID string }
	s.DB.Raw(`SELECT id FROM services WHERE organization_id = ? AND LOWER(name) = LOWER(?) AND deleted_at IS NULL LIMIT 1`, org, name).Scan(&row)
	now := s.now()
	if row.ID != "" {
		return row.ID, s.DB.Exec(`UPDATE services SET price = ?, currency = ?, price_includes_tax = ?, is_active = true, updated_at = ? WHERE id = ? AND organization_id = ?`,
			price, cur, incTax, now, row.ID, org).Error
	}
	id := uuid.NewString()
	// Duration 0: a product, not a timed appointment (as for imported catalogue items).
	err := s.DB.Exec(`INSERT INTO services (id, organization_id, name, description, category, duration, price, currency, price_includes_tax, is_active, requires_vehicle, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, true, false, ?, ?)`, id, org, name, clip(desc, 900), cat, price, cur, incTax, now, now).Error
	return id, err
}

// BulkResult is one site of a bulk creation.
type BulkResult struct {
	Domain         string  `json:"domain"`
	OK             bool    `json:"ok"`
	Error          string  `json:"error,omitempty"`
	SubscriptionID string  `json:"subscription_id,omitempty"`
	Number         string  `json:"number,omitempty"`
	Customer       string  `json:"customer,omitempty"`
	Price          float64 `json:"price,omitempty"`
	Currency       string  `json:"currency,omitempty"`
	Period         string  `json:"period,omitempty"`
	Start          string  `json:"start,omitempty"`
}

// BulkFromSynergy creates subscriptions for every matched, unbilled site
// (or only `domains`), each with its suggested service, price and start.
// dryRun lists what would be created.
func (s *Service) BulkFromSynergy(org, user string, domains []string, dryRun bool) ([]BulkResult, error) {
	want := map[string]bool{}
	for _, d := range domains {
		if d = cleanDomain(d); d != "" {
			want[d] = true
		}
	}
	out := []BulkResult{}
	for _, r := range s.SynergySites(org) {
		if len(want) > 0 && !want[r.Domain] {
			continue
		}
		if r.Subscription != nil || r.Status == "cancelled" || r.NoClient {
			continue
		}
		b := BulkResult{Domain: r.Domain, Customer: r.CustomerName, Start: r.Start}
		if r.Suggestion != nil {
			b.Price, b.Currency, b.Period = r.Suggestion.Price, r.Suggestion.Currency, r.Suggestion.Period
		}
		if !r.Ready {
			b.Error = r.Reason
			out = append(out, b)
			continue
		}
		if dryRun {
			b.OK = true
			out = append(out, b)
			continue
		}
		p := r.Suggestion.Price
		sub, err := s.FromSynergy(org, user, FromSynergyInput{Domain: r.Domain, CustomerID: r.CustomerID, ServiceID: r.Suggestion.ServiceID, Price: &p, Period: r.Suggestion.Period,
			StartDate: r.Start, Description: r.Suggestion.Description})
		if err != nil {
			b.Error = err.Error()
		} else {
			b.OK, b.SubscriptionID, b.Number = true, sub.ID, sub.Number
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return out, invalid("there are no Synergy sites to bill: every hosted site already has a subscription, or none is matched to a client")
	}
	return out, nil
}
